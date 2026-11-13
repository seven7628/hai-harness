package main

// 场景6：Hub ↔ Project 的端到端集成。真 manager / FileStore / queue / 会话装配，
// 只把模型换成 fake provider：
//
//	A. session_send 批次（command + 正文 + files + images）的账本/附件归一；Project
//	   会话真消费正文（`@路径` + image 块）并把结果回推给群内发起者；同一智能体的
//	   DM 与群上下文/落盘文件互相隔离。
//	B. Project 侧的内联审批 / Ask / ToDo 只走 need 与事件通道，绝不写进 Hub 群上下文。
//	C. 每个 LLM 轮次一条气泡（段正文 = 该轮 LLMEnd.Content），纯 reasoning 轮不造
//	   气泡、中间段不路由。
//
// 已知缺口（这里不用假断言盖住）：command=compact 与正文**同批**投递时，强制压缩
// 会在本轮结算段 Stop（agents/agent_loop.go 的 forced 分支），正文不会进入模型
// 上下文，任务以空摘要结算。因此 A 的「命令先于正文」用两次 session_send 证明：
// 先只发 compact（command_only，不建账、不产生任务），再发正文批次 —— 压缩的
// compress_end 早于正文那轮 agent_start。同批 compact+正文的正文消费需要生产侧修，
// 不在本测试的改动范围。
//
// 全部确定性：不碰网络、不读真实 settings（testManager 已隔离 HOME）；等待一律是
// 轮询锚点（convWait / p404aWaitGroupIdle / waitResultNotifyDrained / waitEventOffsets），
// 不用固定 sleep 猜时序。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
)

// ---------- 本文件的装配/断言辅助 ----------

// sc6SessionSend 调用会话（群或 DM）工具引擎里的 session_send，返回解码后的结果。
func sc6SessionSend(t *testing.T, bs *bridgeSession, payload map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := bs.hubEngine.GetTool(context.Background(), "session_send")
	if err != nil {
		t.Fatalf("取 session_send 工具: %v", err)
	}
	out, err := tool.Call(context.Background(), "session_send", string(raw))
	if err != nil {
		t.Fatalf("session_send %s: %v", raw, err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("session_send 返回不是 JSON 对象: %s (%v)", out, err)
	}
	return res
}

// sc6Receipt 取出 session_send 的 command 回执（没有回执说明这一批没带命令）。
func sc6Receipt(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	cmd, _ := res["command"].(map[string]any)
	if cmd == nil {
		t.Fatalf("session_send 没有 command 回执: %v", res)
	}
	return cmd
}

// sc6AssertAttachment 断言账本里某个 kind 的附件是**归一化后的真实路径**（macOS 上
// /tmp → /private/tmp 这类软链必须被折叠），名字/字节数同源。
func sc6AssertAttachment(t *testing.T, task *agenthub.Task, kind, path string) {
	t.Helper()
	if task == nil {
		t.Fatal("task = nil")
	}
	var got []agenthub.TaskAttachment
	for _, att := range task.Attachments {
		if att.Kind == kind {
			got = append(got, att)
		}
	}
	if len(got) != 1 {
		t.Fatalf("账本 %s 附件 = %d, want 1（%+v）", kind, len(got), task.Attachments)
	}
	want := hubCanonicalPath(path)
	fi, err := os.Stat(want)
	if err != nil {
		t.Fatalf("读附件 %q: %v", want, err)
	}
	att := got[0]
	if att.Path != want {
		t.Fatalf("%s 附件路径 = %q, want 归一化真实路径 %q", kind, att.Path, want)
	}
	if att.Name != filepath.Base(want) {
		t.Fatalf("%s 附件名 = %q, want %q", kind, att.Name, filepath.Base(want))
	}
	if att.Bytes != fi.Size() {
		t.Fatalf("%s 附件字节数 = %d, want %d", kind, att.Bytes, fi.Size())
	}
}

// sc6PNGDataURL 是目标会话请求里该 PNG 的期望 data URL：小图（8x8，远低于内联闸门）
// 会被 loadImageForModel 原样内嵌，base64 就是文件字节本身。
func sc6PNGDataURL(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
}

// sc6WaitManualCompressEnd 等目标会话真的执行了 compact（reason=manual 的 compress_end）。
func sc6WaitManualCompressEnd(t *testing.T, buf *syncBuf, sid string) {
	t.Helper()
	convWait(t, "目标会话执行 compact（manual 压缩事件）", func() bool {
		for _, ev := range hubEvents(t, buf, "compress_end") {
			if ev["session_id"] == sid && ev["reason"] == "manual" {
				return true
			}
		}
		return false
	})
}

// sc6WakeBlocks 返回一次模型请求里的全部 hub_group_wake 块（快照只该有一份）。
func sc6WakeBlocks(req *provider.StreamRequest) []core.Content {
	var out []core.Content
	if req == nil {
		return out
	}
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			if c.Type == core.ContentTypeHubGroupWake {
				out = append(out, c)
			}
		}
	}
	return out
}

// TestHubScenario6BatchIsolationAndProjectResult 覆盖场景1/3/4：批次账本 + 隔离 +
// Project 真消费 + 回推总结。两个子用例各自 new manager（互不污染）。
func TestHubScenario6BatchIsolationAndProjectResult(t *testing.T) {
	t.Run("batch-ledger", sc6BatchLedger)
	t.Run("project-result-and-isolation", sc6ProjectResultAndIsolation)
}

// sc6BatchLedger：一批 session_send（compact + 正文 + 文件 + 图片）真的建了账：
// 命令进 Task、附件只存归一化路径引用、命令在目标会话里执行。
func sc6BatchLedger(t *testing.T) {
	m, buf := testManager(t)
	root := t.TempDir()
	a := createWorkspaceAgent(t, m, buf, 1, "批次A", []string{root})
	b := createWorkspaceAgent(t, m, buf, 2, "批次B", []string{root})
	g := assignGroup(t, m, buf, 3, "批次群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), newConvProvider("批次结果收到"))
	// B 永远不该被叫醒；也装 fake，避免任何路径退回真实 provider。
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	setGroupFakeLoop(t, m, bsB, g, string(b.ID), newConvProvider("批次B不该说话"))

	ws := filepath.Join(root, "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(ws); err != nil {
		t.Fatal(err)
	}
	target, err := m.create(ws, "s-batch", "")
	if err != nil {
		t.Fatal(err)
	}
	// compact 需要真压缩器（否则命令在注入段按「未配置」失败，compress_* 不会出现）。
	target.s.SetLoop(agents.NewAgentLoop(
		agents.WithProvider(&fixedEvalProvider{reply: "批次目标正文"}),
		agents.WithModel("sc6-batch-fake"),
		agents.WithCompressor(agents.NewLLMCompressor(
			&fixedEvalProvider{reply: "## 摘要\n- 批次压缩完成"}, "sc6-batch-fake")),
	))

	mdPath := filepath.Join(ws, "a.md")
	if err := os.WriteFile(mdPath, []byte("# notes\n三家对比表要点"), 0o644); err != nil {
		t.Fatal(err)
	}
	pngPath := filepath.Join(ws, "a.png")
	writeTestPNG(t, pngPath, 8, 8)

	res := sc6SessionSend(t, bsA, map[string]any{
		"workspace": ws, "session_id": "s-batch",
		"command": "compact", "text": "做一份三家对比表",
		"files": []string{mdPath}, "images": []string{pngPath},
	})
	if res["accepted"] != true || res["queued"] != false {
		t.Fatalf("批次没被接收: %v", res)
	}
	taskID := str(res, "task_id")
	if taskID == "" {
		t.Fatalf("带正文的批次必须建账拿 task_id: %v", res)
	}
	receipt := sc6Receipt(t, res)
	if receipt["name"] != "compact" || receipt["delivered"] != true {
		t.Fatalf("命令回执 = %v, want compact/delivered", receipt)
	}

	task := p3bLoadTask(t, m, agenthub.TaskID(taskID))
	if task.Command != "compact" {
		t.Fatalf("账本 command = %q, want compact", task.Command)
	}
	sc6AssertAttachment(t, task, agenthub.AttachmentKindFile, mdPath)
	sc6AssertAttachment(t, task, agenthub.AttachmentKindImage, pngPath)
	// 附件只存引用：账本/任务事件里不能出现图片本体。
	if raw, err := json.Marshal(task); err == nil && strings.Contains(string(raw), "data:image") {
		t.Fatalf("账本里出现了图片 data URL: %s", raw)
	}

	// 命令真的在目标会话里执行了（manual 压缩），随后会话回到空闲。
	sc6WaitManualCompressEnd(t, buf, "s-batch")
	convWait(t, "目标会话执行完命令后回到空闲", func() bool { return !target.s.IsRunning() })
}

// sc6ProjectResultAndIsolation：先 compact 再派正文 —— Project 会话真消费正文
// （`@绝对路径` + image 块）、把哨兵摘要回推给发起者；DM 与群互不串上下文/文件。
func sc6ProjectResultAndIsolation(t *testing.T) {
	const (
		summary   = "SC6-PROJECT-SUMMARY：三家对比表已完成"
		aReply    = "SC6-A-BUBBLE：收到项目结果，我来总结"
		dmOnly    = "SC6-DM-ONLY-私聊哨兵"
		groupOnly = "SC6-GROUP-ONLY-群哨兵"
	)
	m, buf := testManager(t)
	root := t.TempDir()
	a := createWorkspaceAgent(t, m, buf, 1, "派发A", []string{root})
	b := createWorkspaceAgent(t, m, buf, 2, "旁观B", []string{root})
	g := assignGroup(t, m, buf, 3, "项目群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fa := newConvProvider(aReply)
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), fa)
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	fb := newConvProvider("旁观者不该被叫醒")
	setGroupFakeLoop(t, m, bsB, g, string(b.ID), fb)

	ws := filepath.Join(root, "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(ws); err != nil {
		t.Fatal(err)
	}
	target, err := m.create(ws, "s-proj", "")
	if err != nil {
		t.Fatal(err)
	}
	tp := newConvProvider(summary)
	target.s.SetLoop(agents.NewAgentLoop(
		agents.WithProvider(tp),
		agents.WithModel("sc6-target-fake"),
		// 压缩器用自己的 fake：压缩轮不得消耗正文那轮的脚本回复。
		agents.WithCompressor(agents.NewLLMCompressor(
			&fixedEvalProvider{reply: "## 摘要\n- 压缩完成"}, "sc6-target-fake")),
	))

	mdPath := filepath.Join(ws, "a.md")
	if err := os.WriteFile(mdPath, []byte("# notes\n三家对比表要点"), 0o644); err != nil {
		t.Fatal(err)
	}
	pngPath := filepath.Join(ws, "a.png")
	writeTestPNG(t, pngPath, 8, 8)
	wantImage := sc6PNGDataURL(t, pngPath)

	// 1) 命令单独一批：compact 是独立操作（压缩后停在当前状态），不建账。
	compact := sc6SessionSend(t, bsA, map[string]any{
		"workspace": ws, "session_id": "s-proj", "command": "compact",
	})
	if compact["accepted"] != true || str(compact, "status") != agenthub.StatusCommandOnly {
		t.Fatalf("command_only 批次 = %v", compact)
	}
	if str(compact, "task_id") != "" {
		t.Fatalf("只有命令不该建账: %v", compact)
	}
	if receipt := sc6Receipt(t, compact); receipt["delivered"] != true {
		t.Fatalf("命令回执 = %v, want delivered", receipt)
	}
	sc6WaitManualCompressEnd(t, buf, "s-proj")
	convWait(t, "压缩后目标会话空闲", func() bool { return !target.s.IsRunning() })

	// 2) 正文批次：文件引用 + 图片 + 文本。正文那轮必须晚于压缩。
	sent := sc6SessionSend(t, bsA, map[string]any{
		"workspace": ws, "session_id": "s-proj", "text": "做一份三家对比表",
		"files": []string{mdPath}, "images": []string{pngPath},
	})
	if sent["accepted"] != true {
		t.Fatalf("正文批次没被接收: %v", sent)
	}
	id := agenthub.TaskID(str(sent, "task_id"))
	if id == "" {
		t.Fatalf("正文批次没有 task_id: %v", sent)
	}

	// 3) 目标请求：@归一化路径 + 图片 data URL + 正文都在模型上下文里。
	convWait(t, "Project 会话被派发并开跑", func() bool { return tp.callCount() >= 1 })
	req := tp.lastRequest()
	if !requestHasTextContaining(req, "@"+hubCanonicalPath(mdPath)) {
		t.Fatalf("目标请求缺 `@%s` 文件引用", hubCanonicalPath(mdPath))
	}
	if !requestHasTextContaining(req, "做一份三家对比表") {
		t.Fatal("目标请求缺派发正文")
	}
	if !requestHasImageBlock(req, wantImage) {
		t.Fatalf("目标请求缺图片块（data URL %s…）", wantImage[:40])
	}

	// 4) 顺序：命令的 compress_end 早于正文那轮 agent_start（最后一条 s-proj 的
	// agent_start 就是正文那轮：压缩批次不产生正文轮）。
	compressAt, bodyAt := waitEventOffsets(t, buf,
		func(ev map[string]any) bool {
			return ev["event_type"] == "compress_end" && ev["session_id"] == "s-proj" && ev["reason"] == "manual"
		},
		func(ev map[string]any) bool {
			return ev["event_type"] == "agent_start" && ev["session_id"] == "s-proj"
		},
	)
	if compressAt >= bodyAt {
		t.Fatalf("顺序反了：compress_end 在 %d，正文那轮 agent_start 在 %d（命令必须先于正文）", compressAt, bodyAt)
	}

	// 5) 结算 + 回推：任务 delivered、通知排空、目标空闲。
	convWait(t, "Project 任务结算并回推完成", func() bool {
		return p3bLoadTask(t, m, id).Status == agenthub.TaskDelivered
	})
	waitResultNotifyDrained(t, m)
	convWait(t, "目标会话运行结束", func() bool { return !target.s.IsRunning() })

	// 6) 群日志：恰好一张结果卡 + 发起者一条总结气泡，卡先于气泡。
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return len(p3bTaskResultCards(msgs, id)) == 1 && len(agentTextMessages(msgs, a.ID)) == 1
	})
	p404aWaitGroupIdle(t, bsA)
	msgs := p3bMessages(t, m, g.ID)
	cards := p3bTaskResultCards(msgs, id)
	bubbles := agentTextMessages(msgs, a.ID)
	if len(cards) != 1 {
		t.Fatalf("结果卡 = %d, want 1（msgs=%+v）", len(cards), msgs)
	}
	if len(bubbles) != 1 {
		t.Fatalf("A 的总结气泡 = %d, want 1（msgs=%+v）", len(bubbles), msgs)
	}
	card, bubble := cards[0], bubbles[0]
	if card.Seq >= bubble.Seq {
		t.Fatalf("顺序反了：结果卡 seq=%d，气泡 seq=%d（卡必须先落账）", card.Seq, bubble.Seq)
	}
	if card.Sender.AgentID != a.ID || !strings.Contains(card.Text, summary) {
		t.Fatalf("结果卡 = %+v（sender/text 要指向发起者与项目摘要）", card)
	}
	if !strings.Contains(card.Text, ws) || !strings.Contains(card.Text, "s-proj") {
		t.Fatalf("结果卡看不出项目归属: %q", card.Text)
	}
	if bubble.Text != aReply {
		t.Fatalf("发起者气泡 = %q, want %q", bubble.Text, aReply)
	}
	// 回推触发的气泡不是 @ 路由：无 mentions/round/reply_to/context。
	if len(bubble.Mentions) != 0 || bubble.MentionAll || bubble.RoundID != "" ||
		bubble.ReplyToSeq != 0 || bubble.Context != nil {
		t.Fatalf("回推气泡不能带 mentions/round/reply_to/context: %+v", bubble)
	}
	if len(card.Mentions) != 0 || card.MentionAll || card.RoundID != "" || card.ReplyToSeq != 0 {
		t.Fatalf("结果卡不能带 mentions/路由: %+v", card)
	}
	if !bsA.s.HasTaskResult(string(id)) {
		t.Fatal("发起者的群会话没有收到 task_result")
	}
	if texts := p3bTaskResultTexts(bsA, id); len(texts) != 1 || !strings.Contains(texts[0], summary) {
		t.Fatalf("发起者上下文里的结果正文 = %q, want 恰一份含项目摘要", texts)
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), id); n != 1 {
		t.Fatalf("task_result_delivered = %d, want 1（只推一次）", n)
	}

	// 7) 旁观者不被叫醒，回推不产生 @ 路由。
	if n := fb.callCount(); n != 0 {
		t.Fatalf("旁观者被叫醒 %d 次（结果卡不能路由 @）", n)
	}
	if bsB.s.HasTaskResult(string(id)) {
		t.Fatal("旁观者收到 task_result：只应唤醒发起者")
	}
	if n := gwGroupRounds(buf, g.ID); n != 0 {
		t.Fatalf("结果卡产生了 %d 条 @ 路由（hub_round）, want 0", n)
	}

	// 8) 隔离：同一智能体的 DM 与群各看各的（哨兵 + 文件双证据）。
	dm, err := m.hubDMSession(string(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	fdm := newConvProvider("SC6-DM-回复")
	setDMFakeLoop(t, m, dm, string(a.ID), fdm)
	if ok, errMsg, _ := hubCmd(t, m, buf, 10, "hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": string(a.ID)}, "text": dmOnly,
	}); !ok {
		t.Fatalf("dm send: %s", errMsg)
	}
	convWait(t, "DM 会话开跑", func() bool { return fdm.callCount() >= 1 })
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@" + a.Name + " " + groupOnly,
	}); !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	convWait(t, "群会话第二轮开跑", func() bool { return fa.callCount() >= 2 })
	dmPrompt := isolationPrompt(fdm.lastRequest())
	groupPrompt := isolationPrompt(fa.lastRequest())
	if !strings.Contains(dmPrompt, dmOnly) {
		t.Fatal("DM prompt 缺本轮 DM 正文")
	}
	if strings.Contains(dmPrompt, groupOnly) || strings.Contains(dmPrompt, summary) {
		t.Fatalf("DM prompt 串进了群内容（GROUP-ONLY / 项目摘要）:\n%s", dmPrompt)
	}
	if !strings.Contains(groupPrompt, groupOnly) {
		t.Fatal("群 prompt 缺本轮群正文")
	}
	if strings.Contains(groupPrompt, dmOnly) {
		t.Fatalf("群 prompt 串进了 DM 正文:\n%s", groupPrompt)
	}

	// 落盘也分家：record 与 session jsonl 按确定性 sid 各一个文件。
	dmSID := hubSessionID(string(a.ID), agenthub.Scope{Kind: agenthub.ScopeDM})
	grpSID := hubSessionID(string(a.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	wsRT := m.hubRuntime(string(a.ID))
	wsRT.mu.Lock()
	dmBS, grpBS := wsRT.sessions[dmSID], wsRT.sessions[grpSID]
	wsRT.mu.Unlock()
	if dmBS == nil || grpBS == nil {
		t.Fatalf("precondition: DM/群会话都该登记在 ws.sessions（%v/%v）", dmBS != nil, grpBS != nil)
	}
	stopSession(dmBS) // 停会话让 checkpoint 把 record 落完
	stopSession(grpBS)
	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	wsKey := "hub-" + string(a.ID)
	dmRecord, err := store.RecordPath(wsKey, dmSID)
	if err != nil {
		t.Fatal(err)
	}
	grpRecord, err := store.RecordPath(wsKey, grpSID)
	if err != nil {
		t.Fatal(err)
	}
	if dmRecord == grpRecord {
		t.Fatalf("DM 与群共用同一个 record 文件: %q", dmRecord)
	}
	home := hubAgentHome(string(a.ID))
	dmJSONL := filepath.Join(home, "sessions", dmSID+".jsonl")
	grpJSONL := filepath.Join(home, "sessions", grpSID+".jsonl")
	if dmJSONL == grpJSONL {
		t.Fatal("DM 与群共用同一个 session jsonl")
	}
	for _, path := range []string{dmRecord, grpRecord, dmJSONL, grpJSONL} {
		fi, err := os.Stat(path)
		if err != nil || fi.Size() == 0 {
			t.Fatalf("落盘文件缺失/为空: %q (%v)", path, err)
		}
	}
}

// TestHubScenario6InlineCardsStayOutOfHubContext 覆盖场景2：审批/Ask/ToDo 是内联展示
// 与事件，不是群消息 —— 群快照、群唤醒块、模型 prompt 里都不能出现它们的正文。
func TestHubScenario6InlineCardsStayOutOfHubContext(t *testing.T) {
	const (
		needSentinel = "SC6-NEED-ONLY-审批参数"
		askSentinel  = "SC6-ASK-ONLY-提问题干"
		todoSentinel = "SC6-TODO-ONLY-待办标题"
		trigger      = "SC6-TRIGGER-本轮正文"
	)
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "内联A")
	g := groupWithMembers(t, m, buf, 2, "内联群", memberRef{a.ID, "owner"})
	site := buildProjectNeedSite(t, m, buf, agenthub.TaskOrigin{
		Kind: agenthub.OriginGroup, GroupID: g.ID, AgentID: a.ID, AgentName: a.Name,
	})
	ctx := context.Background()

	// Project 侧：一条审批 need、一条 Ask need（正文带唯一哨兵）。
	// 审批要真注册，hub_need_resolve 才能 Approve 到锚点。
	if _, err := site.bs.s.RequestApproval(ctx, core.ToolCall{Id: "call-need-inline", Name: "bash"}); err != nil {
		t.Fatalf("register approval: %v", err)
	}
	site.bs.emit(&events.ToolApprovalRequested{
		RunId: "run-need-inline", Id: "call-need-inline", Name: "bash",
		Arguments: `{"command":"echo ` + needSentinel + `"}`,
		Timestamp: time.Now(), EventType: events.ToolApprovalType,
	})
	site.bs.emit(&events.AskUserQuestion{
		RunId: "run-ask-inline", Id: "batch-ask-inline",
		Questions: []events.QuestionItem{{Id: "q1", Question: askSentinel}},
		Timestamp: time.Now(), EventType: events.AskUserQuestionType,
	})
	// Project 会话的待办：事件带 todos 快照（前端内联渲染），但不写群消息。
	site.bs.emit(&events.ToolResponse{
		RunId: "run-todo-inline", Id: "call-todo-inline", Name: "todo_add",
		Result:    "已新增待办",
		Todos:     []events.TodoSnapshot{{Id: "todo_1", Title: todoSentinel}},
		Timestamp: time.Now(), EventType: events.ToolRunEndType,
	})
	var todoEvent map[string]any
	for _, ev := range hubEvents(t, buf, "tool_run_end") {
		if ev["name"] == "todo_add" {
			todoEvent = ev
		}
	}
	if todoEvent == nil {
		t.Fatal("Project 会话没有发出 todo_add 的 tool_run_end")
	}
	if raw, err := json.Marshal(todoEvent["todos"]); err != nil || !strings.Contains(string(raw), todoSentinel) {
		t.Fatalf("tool_run_end 的 todos 快照 = %s (%v), want 含 %q", raw, err, todoSentinel)
	}

	// Hub 侧：origin A 的群会话跑一轮真实唤醒，记录模型请求。
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fake := newConvProvider("SC6-内联回复")
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), fake)
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@" + a.Name + " " + trigger,
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	seq, _ := data["seq"].(float64)
	if seq <= 0 {
		t.Fatalf("hub_group_send 没返回 trigger seq: %v", data)
	}
	convWait(t, "群会话被唤醒", func() bool { return fake.callCount() >= 1 })
	p404aWaitGroupIdle(t, bsA)
	msgs := p3bMessages(t, m, g.ID)
	for _, msg := range msgs {
		for _, s := range []string{needSentinel, askSentinel, todoSentinel} {
			if strings.Contains(msg.Text, s) {
				t.Fatalf("群消息泄漏了 Project 内联正文 %q: %+v", s, msg)
			}
		}
	}

	// 1) 群快照（BuildGroupSnapshot）只含本轮触发正文，不含 need/Ask/todo 正文。
	round := agenthub.Round{GroupID: g.ID, ID: "rnd_sc6", TriggerSeq: int64(seq)}
	snap := agenthub.BuildGroupSnapshot(g, a.ID, msgs, 0, round, agenthub.SnapshotOptions{})
	if !strings.Contains(snap.Text, trigger) {
		t.Fatalf("群快照缺本轮触发正文:\n%s", snap.Text)
	}
	for _, s := range []string{needSentinel, askSentinel, todoSentinel} {
		if strings.Contains(snap.Text, s) {
			t.Fatalf("群快照泄漏了内联正文 %q:\n%s", s, snap.Text)
		}
	}

	// 2) 模型 prompt：整包（含 hidden 快照块）都不含哨兵；wake 块恰好一个，
	//    ParseGroupWake 出来的快照同样干净。
	req := fake.lastRequest()
	prompt := isolationPrompt(req)
	for _, s := range []string{needSentinel, askSentinel, todoSentinel} {
		if strings.Contains(prompt, s) {
			t.Fatalf("模型 prompt 泄漏了内联正文 %q:\n%s", s, prompt)
		}
	}
	if !strings.Contains(prompt, trigger) {
		t.Fatal("模型 prompt 缺本轮触发正文")
	}
	wakes := sc6WakeBlocks(req)
	if len(wakes) != 1 {
		t.Fatalf("hub_group_wake 块 = %d, want 1", len(wakes))
	}
	header, wakeSnap, err := agenthub.ParseGroupWake(wakes[0].Content)
	if err != nil {
		t.Fatalf("ParseGroupWake: %v", err)
	}
	if header.GroupID != g.ID || header.AgentID != a.ID {
		t.Fatalf("wake 归属 = %s/%s, want %s/%s", header.GroupID, header.AgentID, g.ID, a.ID)
	}
	if !strings.Contains(wakeSnap, trigger) {
		t.Fatalf("wake 快照缺触发正文:\n%s", wakeSnap)
	}
	for _, s := range []string{needSentinel, askSentinel, todoSentinel} {
		if strings.Contains(wakeSnap, s) {
			t.Fatalf("wake 快照泄漏了内联正文 %q:\n%s", s, wakeSnap)
		}
	}

	// 3) hub_message 事件（前端群消息投影）里也没有内联正文。
	for _, ev := range hubEvents(t, buf, "hub_message") {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{needSentinel, askSentinel, todoSentinel} {
			if strings.Contains(string(raw), s) {
				t.Fatalf("hub_message 泄漏了内联正文 %q: %s", s, raw)
			}
		}
	}

	// 4) hub_need_list 按群查得到该 need，且 origin_agent_name 已按 profile 补齐。
	ok, errMsg, nd := hubCmd(t, m, buf, 4, "hub_need_list", map[string]any{
		"group_id": string(g.ID),
	})
	if !ok {
		t.Fatalf("hub_need_list: %s", errMsg)
	}
	raw, err := json.Marshal(nd)
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Needs []*agenthub.Need `json:"needs"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	var approval, question *agenthub.Need
	for _, n := range listed.Needs {
		switch n.Kind {
		case "approval":
			approval = n
		case "question":
			question = n
		}
	}
	if approval == nil || question == nil {
		t.Fatalf("hub_need_list 缺审批/提问 need: %s", raw)
	}
	if approval.OriginAgentID != a.ID || approval.OriginAgentName != a.Name {
		t.Fatalf("审批 need origin = %q/%q, want %q/%q",
			approval.OriginAgentID, approval.OriginAgentName, a.ID, a.Name)
	}
	if !strings.Contains(approval.Body, needSentinel) || !strings.Contains(question.Body, askSentinel) {
		t.Fatalf("need 正文丢了哨兵（审批=%q 提问=%q）", approval.Body, question.Body)
	}
	if approval.GroupID != g.ID || approval.TaskID != site.task.ID {
		t.Fatalf("审批 need 归属 = group %q task %q", approval.GroupID, approval.TaskID)
	}

	// 5) resolve need 不写群消息：群日志条数前后一致。
	before := len(p3bMessages(t, m, g.ID))
	if ok, errMsg, _ := hubCmd(t, m, buf, 5, "hub_need_resolve", map[string]any{
		"need_id": string(approval.ID), "resolution": "approve",
	}); !ok {
		t.Fatalf("hub_need_resolve: %s", errMsg)
	}
	after := len(p3bMessages(t, m, g.ID))
	if after != before {
		t.Fatalf("need resolve 前后群消息条数 %d → %d（resolve 不能往群里写消息）", before, after)
	}
	if resolved, err := site.st.LoadNeed(ctx, approval.ID); err != nil || resolved.Status != "resolved" {
		t.Fatalf("审批 need 未 resolve: %+v (%v)", resolved, err)
	}
}

// ---------- 场景2b：本地分段 provider（不改现有 segmentProvider） ----------

// sc6Round 一轮模型响应：reasoning 增量 + 正文增量 + LLMEnd 的权威正文 + 可选工具调用。
type sc6Round struct {
	reasoning []string
	chunks    []string
	content   string
	tool      *core.ToolCall
}

// sc6Provider 按调用次序回放 sc6Round，与真实 provider 同形：先 LLMStart（分段键），
// 再 reasoning/正文增量，最后 LLMEnd。
type sc6Provider struct {
	mu     sync.Mutex
	rounds []sc6Round
	calls  int
}

func (p *sc6Provider) Stream(_ context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	idx := p.calls
	p.calls++
	round := sc6Round{}
	if len(p.rounds) > 0 {
		if idx < len(p.rounds) {
			round = p.rounds[idx]
		} else {
			round = p.rounds[len(p.rounds)-1]
		}
	}
	p.mu.Unlock()
	rid := fmt.Sprintf("req_sc6_%d", idx)
	if err := h(provider.LLMStartEvent{RequestId: rid, Model: req.Model, Timestamp: time.Now()}); err != nil {
		return err
	}
	for i, c := range round.reasoning {
		if err := h(provider.LLMReasoningDeltaEvent{RequestId: rid, Delta: c, Index: int64(i)}); err != nil {
			return err
		}
	}
	for i, c := range round.chunks {
		if err := h(provider.LLMContentDeltaEvent{RequestId: rid, Delta: c, Index: int64(i)}); err != nil {
			return err
		}
	}
	ev := provider.LLMEndEvent{
		RequestId: rid, FinishReason: core.FinishReasonStop, Content: round.content,
		Model: req.Model, Usage: core.Usage{Input: 10, Output: 5},
	}
	if round.tool != nil {
		ev.ToolCalls = []core.ToolCall{*round.tool}
		ev.FinishReason = core.FinishReasonToolCall
	}
	return h(ev)
}

func (p *sc6Provider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// TestHubScenario6SegmentedBubbles 覆盖场景2b：一段一条气泡、纯 reasoning 轮零气泡、
// 每段草稿 index 从 0 连续且文本 == 该轮 final、final 晚于 delta、中间段不路由。
func TestHubScenario6SegmentedBubbles(t *testing.T) {
	const (
		seg1 = "@分段A 段一-A，段一-B"
		seg2 = "段二"
	)
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "分段A")
	b := createHubAgent(t, m, buf, 2, "分段B")
	g := groupWithMembers(t, m, buf, 3, "分段群", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	bs, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	// 默认 80ms 定时器会把 pending 提前发出去，段的先后就不确定了（同 P3a-09 的理由）。
	bs.hubDelta.interval = time.Hour
	prov := &sc6Provider{rounds: []sc6Round{
		{
			// 轮1：reasoning + 正文 + 工具调用（工具轮后继续下一轮）。
			reasoning: []string{"第一轮思考"},
			chunks:    []string{"@分段A 段一-A", "，段一-B"},
			content:   seg1,
			tool:      &core.ToolCall{Id: "call_sc6_1", Index: 0, Name: "todo_add", Arguments: `{"title":"分段一"}`},
		},
		{
			// 轮2：正文 + 工具调用（再进一轮，制造「纯 reasoning 收尾轮」）。
			chunks:  []string{"段二"},
			content: seg2,
			tool:    &core.ToolCall{Id: "call_sc6_2", Index: 0, Name: "todo_add", Arguments: `{"title":"分段二"}`},
		},
		{
			// 轮3：只有 reasoning，没有正文 —— 不得落空气泡/思考气泡。
			reasoning: []string{"纯思考绝不落成气泡"},
		},
	}}
	setGroupStreamLoop(t, m, bs, g, string(b.ID), prov, 3)
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@" + b.Name + " 说两段",
	}); !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return len(agentTextMessages(msgs, b.ID)) >= 2
	})
	p404aWaitGroupIdle(t, bs)
	// 三轮都真的跑了：没有第三轮，「纯 reasoning 不造气泡」就是空断言。
	if n := prov.callCount(); n != 3 {
		t.Fatalf("LLM 轮数 = %d, want 3（reasoning-only 收尾轮必须真发生）", n)
	}

	msgs := p3bMessages(t, m, g.ID)
	bubbles := agentTextMessages(msgs, b.ID)
	if len(bubbles) != 2 {
		t.Fatalf("B 的 text 气泡 = %d, want 2（reasoning-only 轮不得造气泡）: %+v", len(bubbles), bubbles)
	}
	if bubbles[0].Text != seg1 || bubbles[1].Text != seg2 {
		t.Fatalf("气泡正文 = %q / %q, want %q / %q", bubbles[0].Text, bubbles[1].Text, seg1, seg2)
	}
	if bubbles[0].ID == bubbles[1].ID {
		t.Fatalf("两段共用了同一个 message_id %q（前端会把它们并成一条）", bubbles[0].ID)
	}
	for _, msg := range msgs {
		if strings.Contains(msg.Text, "纯思考绝不落成气泡") || strings.Contains(msg.Text, "第一轮思考") {
			t.Fatalf("reasoning 文本漏进了群消息: %+v", msg)
		}
	}

	// 每段一条草稿：message_id 与 final 相同、index 从 0 连续、文本 == 该轮 final。
	groups := groupDeltas(t, buf)
	if len(groups) != 2 {
		t.Fatalf("草稿段数 = %d, want 2: %+v", len(groups), groups)
	}
	for i, grp := range groups {
		if grp.messageID != bubbles[i].ID {
			t.Fatalf("第 %d 段草稿 message_id = %q, final = %q", i, grp.messageID, bubbles[i].ID)
		}
		if grp.text != bubbles[i].Text {
			t.Fatalf("第 %d 段草稿文本 = %q, final = %q", i, grp.text, bubbles[i].Text)
		}
		for j, idx := range grp.indexes {
			if idx != j {
				t.Fatalf("第 %d 段 index[%d] = %d，want 从 0 连续递增: %v", i, j, idx, grp.indexes)
			}
		}
	}

	// final 必须晚于对应 delta（前端按 message_id 用 final 替换草稿）。
	for i, msg := range bubbles {
		id := msg.ID
		deltaAt, finalAt := waitEventOffsets(t, buf,
			func(ev map[string]any) bool {
				return ev["event_type"] == "hub_message_delta" && ev["message_id"] == id
			},
			func(ev map[string]any) bool {
				if ev["event_type"] != "hub_message" {
					return false
				}
				body, _ := ev["message"].(map[string]any)
				return body != nil && body["id"] == id
			},
		)
		if deltaAt > finalAt {
			t.Fatalf("第 %d 段顺序反了：final(%d) 早于 delta(%d)", i, finalAt, deltaAt)
		}
	}

	// 中间段不路由：段一正文里的 @分段A 不能落 mentions，也不能产生本轮 round。
	if len(bubbles[0].Mentions) != 0 || bubbles[0].MentionAll {
		t.Fatalf("中间段被路由了：mentions=%v all=%v", bubbles[0].Mentions, bubbles[0].MentionAll)
	}
	for _, ev := range hubEvents(t, buf, "hub_round") {
		round, _ := ev["round"].(map[string]any)
		if round == nil {
			continue
		}
		if trigger, ok := round["trigger_seq"].(float64); ok && int64(trigger) == bubbles[0].Seq {
			t.Fatalf("中间段 %d 产生了路由 round: %v", bubbles[0].Seq, ev)
		}
	}
}
