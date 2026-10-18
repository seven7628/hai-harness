package subagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// ---------- 会话级恢复：状态持久化 + 原地续跑 ----------
//
// 这一组测试钉住 2026-10 的语义变更：后台子任务与主会话同档持久化（每轮 checkpoint
// 落有效上下文 + 用量），agent_resume 据此**原地复用同一 task id**续跑，而不是新建一条。
// 参照 session 侧的 TestAgentResumeEndToEnd（端到端）——这里聚焦 SDK 层的存储与恢复契约。

// recordingStateStore 记录每次 Save 的调用与内容，供检查「每轮 checkpoint」。
type recordingStateStore struct {
	mu    sync.Mutex
	saves [][]core.Message // 每轮 checkpoint 的 Messages
	last  *RunState
}

func (r *recordingStateStore) snapshot() [][]core.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]core.Message, len(r.saves))
	for i, m := range r.saves {
		out[i] = append([]core.Message{}, m...)
	}
	return out
}

// recordingStore 构造端口：Save 记录，Load 返回最后一条（同进程恢复语义）。
func recordingStore() (StateStore, *recordingStateStore) {
	rec := &recordingStateStore{}
	mem := NewMemoryStateStore()
	return StateStore{
		Load: mem.Load,
		Save: func(ctx context.Context, taskID string, st *RunState) error {
			if st != nil {
				rec.mu.Lock()
				rec.saves = append(rec.saves, append([]core.Message{}, st.Messages...))
				cp := *st
				rec.last = &cp
				rec.mu.Unlock()
			}
			return mem.Save(ctx, taskID, st)
		},
		AppendHistory: mem.AppendHistory,
	}, rec
}

// TestResumeInPlaceSameTaskID 会话级恢复的核心契约：注入状态存储后，agent_resume
// **原地复用同一 task id**，且续跑输入 = 上一轮落盘上下文（含原任务原文）+ 本次增量指令。
func TestResumeInPlaceSameTaskID(t *testing.T) {
	dir := t.TempDir()
	store, rec := recordingStore()

	// 原任务：两轮（工具 + 收尾），每轮都会触发 OnTurn → checkpoint。
	origLoop := rzTwoRoundLoop(nil)
	spawn, reg := rzJournalSpawnTool(t, dir, origLoop)
	spawn.spec.Store = store
	resume := &agentResumeTool{spawn: spawn, reg: reg}

	const origTask = "研究 X 方案并把结论写到 report.md；只读源码，不要改任何文件。"
	out, err := spawn.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"name":"research X","task":`+jsonString(origTask)+`}`)
	if err != nil {
		t.Fatal(err)
	}
	old := rzParseSpawn(t, out)
	rzWaitSnapshot(t, reg, old.TaskID, TaskCompleted)

	// 每轮 checkpoint：两轮运行至少落两次 state（初始 + 每轮）。
	if got := len(rec.snapshot()); got < 2 {
		t.Fatalf("应每轮落一次 checkpoint，实际 %d 次", got)
	}

	// 续跑：换 recording loop 以便检查续跑输入。
	recLoop, cap := resumeInputCapturingLoop()
	resume.spawn.spec.Loop = recLoop

	rout, err := resume.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"task_id":"`+old.TaskID+`","message":"接着把结论写完整"}`)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	got := rzParseSpawn(t, rout)
	if got.TaskID != old.TaskID {
		t.Fatalf("原地续跑必须复用同一 task id：old=%s got=%s", old.TaskID, got.TaskID)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(rout), &raw); err != nil {
		t.Fatalf("resume 返回非 JSON: %v\n%s", err, rout)
	}
	if raw["resumed_from"] != old.TaskID {
		t.Fatalf("原地续跑的 resumed_from 应为自身 %s，got %v", old.TaskID, raw["resumed_from"])
	}

	// 同一 task id 被重新驱动 → 最终再次 completed（不是两条任务）。
	rzWaitSnapshot(t, reg, old.TaskID, TaskCompleted)
	if infos := reg.List(); len(infos) != 1 {
		t.Fatalf("注册表应恒为同一任务（1 条），实际 %d 条: %+v", len(infos), infos)
	}

	if cap.count() == 0 {
		t.Fatal("续跑任务未收到任何 user 输入")
	}
	// 续跑输入 = 落盘上下文（含原任务原文）+ 增量指令。capture 只取首条 user 消息，
	// 故取「最后一次运行的首条 user 消息」（= 落盘上下文的首条 = 原任务原文）。
	last := cap.input(cap.count() - 1)
	if !strings.Contains(last, origTask) {
		t.Fatalf("续跑输入应含原任务原文（落盘上下文）:\n%s", last)
	}
	// 增量指令以追加的 user 消息下发：直接查 load 回来的最终上下文。
	st, lerr := store.Load(context.Background(), old.TaskID)
	if lerr != nil || st == nil {
		t.Fatalf("续跑后应能读回状态: %v", lerr)
	}
	if !messagesContain(st.Messages, "接着把结论写完整") {
		t.Fatalf("续跑上下文应含本次增量指令: %+v", st.Messages)
	}
	if !messagesContain(st.Messages, origTask) {
		t.Fatalf("续跑上下文应含原任务原文: %+v", st.Messages)
	}
}

// messagesContain 任一消息的文本块含子串。
func messagesContain(msgs []core.Message, sub string) bool {
	for _, m := range msgs {
		for _, c := range m.Content {
			if strings.Contains(c.Content, sub) {
				return true
			}
		}
	}
	return false
}

// TestResumeAcrossProcessWithState 跨进程恢复（本功能最实的价值）：新 Registry + 新
// Tool（模拟进程重启），仅凭**磁盘上的状态**把上一进程跑过的任务原地续起来。
// 这里用文件型 store 而非内存，才能真实跨「进程」边界。
func TestResumeAcrossProcessWithState(t *testing.T) {
	dir := t.TempDir()
	journalPath := func(taskID string) (string, error) {
		return filepath.Join(dir, taskID+".jsonl"), nil
	}
	store := fileStateStore(t, dir)

	// ---- 进程 A：派任务并跑完（状态落盘） ----
	regA := NewRegistry()
	regA.SetSessionCtx(context.Background())
	t.Cleanup(func() { _ = regA.Close(context.Background()) })
	spawnA := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: rzTwoRoundLoop(nil), Background: true,
		JournalPath: journalPath, Store: store,
	}, func() context.Context { return context.Background() }, regA)
	const origTask = "调研并写出长文报告"
	out, err := spawnA.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"name":"long research","task":`+jsonString(origTask)+`}`)
	if err != nil {
		t.Fatal(err)
	}
	old := rzParseSpawn(t, out)
	rzWaitSnapshot(t, regA, old.TaskID, TaskCompleted)

	// ---- 进程 B：全新 Registry（模拟重启后任务对象已不存在） ----
	regB := NewRegistry()
	regB.SetSessionCtx(context.Background())
	t.Cleanup(func() { _ = regB.Close(context.Background()) })
	recLoop, cap := resumeInputCapturingLoop()
	spawnB := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: recLoop, Background: true,
		JournalPath: journalPath, Store: store,
	}, func() context.Context { return context.Background() }, regB)
	resumeB := &agentResumeTool{spawn: spawnB, reg: regB}

	rout, err := resumeB.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"task_id":"`+old.TaskID+`","message":"继续"}`)
	if err != nil {
		t.Fatalf("跨进程恢复: %v", err)
	}
	got := rzParseSpawn(t, rout)
	if got.TaskID != old.TaskID {
		t.Fatalf("跨进程恢复也应原地复用 task id：old=%s got=%s", old.TaskID, got.TaskID)
	}
	rzWaitSnapshot(t, regB, old.TaskID, TaskCompleted)

	// 身份经 journal + store 重建，续跑输入含原任务原文（落盘上下文，不是有损简报）。
	if cap.count() == 0 {
		t.Fatal("跨进程续跑未收到输入")
	}
	if joined := cap.input(0); !strings.Contains(joined, origTask) {
		t.Fatalf("跨进程续跑输入应含原任务原文:\n%s", joined)
	}
}

// TestCheckpointMessagesAreEffectiveContext checkpoint 落的是**有效上下文**
// （ac.Messages，含助手回复与工具回执），不是仅输入 —— 否则恢复会丢工具结果。
func TestCheckpointMessagesAreEffectiveContext(t *testing.T) {
	dir := t.TempDir()
	store, rec := recordingStore()
	spawn, reg := rzJournalSpawnTool(t, dir, rzTwoRoundLoop(nil))
	spawn.spec.Store = store

	out, err := spawn.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"name":"n","task":"跑两轮"}`)
	if err != nil {
		t.Fatal(err)
	}
	id := rzParseSpawn(t, out).TaskID
	rzWaitSnapshot(t, reg, id, TaskCompleted)

	saves := rec.snapshot()
	// 最后一次 checkpoint 应含工具回执（rzTwoRoundLoop 第一轮调 echo 工具）→
	// 有效上下文里必然出现 tool 角色消息。
	last := saves[len(saves)-1]
	roles := map[core.MessageRole]int{}
	for _, m := range last {
		roles[m.Role]++
	}
	if roles[core.Tool] == 0 {
		t.Fatalf("最后一次 checkpoint 应含工具回执（有效上下文），roles=%v", roles)
	}
	if roles[core.User] == 0 {
		t.Fatalf("checkpoint 应含原始用户指令，roles=%v", roles)
	}
}

// TestResumeAccumulatesUsageAcrossRuns 原地续跑的用量记账：续跑后任务总用量 = 上一轮
// 累计 + 本轮（**不重置基数、不双计**）。rzTwoRoundLoop 每次运行产生 Input=30 的用量
// （10+20）；续跑换一个全新 loop 实例（脚本 provider 有状态，复用同一实例会只跑剩余轮），
// 两轮运行后应为 60 —— 这是「同一任务多轮运行」与「主会话累计用量」同构的判据。
func TestResumeAccumulatesUsageAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	store, _ := recordingStore()
	spawn, reg := rzJournalSpawnTool(t, dir, rzTwoRoundLoop(nil))
	spawn.spec.Store = store
	resume := &agentResumeTool{spawn: spawn, reg: reg}

	out, err := spawn.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"name":"n","task":"跑两轮"}`)
	if err != nil {
		t.Fatal(err)
	}
	id := rzParseSpawn(t, out).TaskID
	rzWaitSnapshot(t, reg, id, TaskCompleted)

	first := reg.TotalUsage().Input // 注册表只有这一个任务 → 合计即本任务
	if first != 30 {
		t.Fatalf("第一轮用量应为 30（10+20），实际 %d", first)
	}

	// 续跑：换全新 loop 实例（同一人设、新的运行），确保这一轮同样跑满两轮。
	resume.spawn.spec.Loop = rzTwoRoundLoop(nil)
	if _, err := resume.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"task_id":"`+id+`"}`); err != nil {
		t.Fatalf("resume: %v", err)
	}
	rzWaitSnapshot(t, reg, id, TaskCompleted)

	if got := reg.TotalUsage().Input; got != 60 {
		t.Fatalf("续跑后总用量应为两轮之和（60），实际 %d —— 基数为 0（%d）或双计（%d）",
			got, got, got)
	}
}

// TestResumeInPlaceRejectsRunningTask 运行中的任务不可原地续（restart 的状态闸门）——
// 与既有 TestResumeRejectsRunningTask（入口闸门）互补，这里钉住 Registry 层的二次防线。
func TestResumeInPlaceRejectsRunningTask(t *testing.T) {
	dir := t.TempDir()
	gate := make(chan struct{})
	store := NewMemoryStateStore()
	spawn, reg := rzJournalSpawnTool(t, dir, rzTwoRoundLoop(gate))
	spawn.spec.Store = store
	resume := &agentResumeTool{spawn: spawn, reg: reg}

	out, err := spawn.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"name":"n","task":"阻塞任务"}`)
	if err != nil {
		t.Fatal(err)
	}
	id := rzParseSpawn(t, out).TaskID
	rzWaitSnapshot(t, reg, id, TaskRunning)

	_, err = resume.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"task_id":"`+id+`"}`)
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("运行中任务必须拒绝续跑: %v", err)
	}
	close(gate) // 放行，避免泄漏
	rzWaitSnapshot(t, reg, id, TaskCompleted)
}

// TestStateStoreNilSafeDegrades 未注入存储（零值端口）时任务照跑，只是不可恢复 ——
// 落盘是旁路，绝不阻断派发。
func TestStateStoreNilSafeDegrades(t *testing.T) {
	dir := t.TempDir()
	spawn, reg := rzJournalSpawnTool(t, dir, rzTwoRoundLoop(nil))
	spawn.spec.Store = StateStore{} // 显式零值

	out, err := spawn.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"name":"n","task":"无存储"}`)
	if err != nil {
		t.Fatal(err)
	}
	id := rzParseSpawn(t, out).TaskID
	rzWaitSnapshot(t, reg, id, TaskCompleted)
}

// TestAgentToolsWiresStateStore NewAgentTools 的变参把存储透传到 spawn 工具
// （装配断言：宿主注入的端口必须真的接上，否则恢复静默退化为内存态）。
func TestAgentToolsWiresStateStore(t *testing.T) {
	store := NewMemoryStateStore()
	list := NewAgentTools(nil, nil, NewRegistry(), nil, nil, nil, WithAgentStateStore(store))
	var spawn *Tool
	for _, tl := range list {
		if s, ok := tl.(*Tool); ok {
			spawn = s
		}
	}
	if spawn == nil {
		t.Fatal("NewAgentTools 未返回 agent_spawn")
	}
	if !spawn.spec.Store.Enabled() {
		t.Fatal("WithAgentStateStore 未把端口接到 agent_spawn.spec.Store")
	}
}

// fileStateStore 文件型状态存储（跨进程测试用）：每次 Save 写一份 <dir>/<taskID>.state.json，
// Load 读回 —— 语义等价于宿主的 session.FileStore 适配器，但只依赖本包。
func fileStateStore(t *testing.T, dir string) StateStore {
	t.Helper()
	type onDisk struct {
		Messages []core.Message `json:"messages"`
		Usage    core.Usage     `json:"usage"`
	}
	return StateStore{
		Load: func(_ context.Context, taskID string) (*RunState, error) {
			b, err := os.ReadFile(filepath.Join(dir, taskID+".state.json"))
			if err != nil {
				return nil, nil // 无 checkpoint → 按「无历史可续」处理
			}
			var d onDisk
			if json.Unmarshal(b, &d) != nil {
				return nil, nil
			}
			return &RunState{Messages: d.Messages, Usage: d.Usage}, nil
		},
		Save: func(_ context.Context, taskID string, st *RunState) error {
			if st == nil {
				return nil
			}
			b, err := json.Marshal(onDisk{Messages: st.Messages, Usage: st.Usage})
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, taskID+".state.json"), b, 0o600)
		},
		AppendHistory: func(context.Context, string, []core.Message) error { return nil },
	}
}
