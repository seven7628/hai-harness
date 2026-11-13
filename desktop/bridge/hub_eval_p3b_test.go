package main

// P3b-08：评测 8–9（方案 §10.3）+ 停止护栏的验收级组合。
//
//  评测 8：A 在**自己的 run 里**调生产 assign_task 把事派给 B；B 的 run 返回最终答复 →
//    账本恰好一条 agent 目标任务 delivered、群里恰好一张 task_result 卡（执行者 B）、
//    只有派发者 A 被唤醒（拿到 task_result，并能继续跑）；C 零 provider 调用/零唤醒；
//    卡不带 mentions/不路由；A↔B 不会继续接龙（没有第二次 assign / 对话唤醒）；
//    重放同一条 AgentEnd 不复制卡、不重复回推（§6.5.6/§6.5.7/§6.7.9）。
//
//  评测 9：结果卡落地后，人 @C —— C 的下一次唤醒请求（真实 provider 请求里的群快照）
//    包含那张卡的正文（失败时是错误原因）且 seq 对得上；C 回复不唤醒 B。
//    失败分支：B 失败 → failed 卡 + A 带错误被唤醒 + C 下次被 @ 仍能看到。
//
//  停止护栏：A 的 run 里 group_say @B 已入队，人点 hub_conversation_stop → B 从不启动、
//    对话 stopped、恰好一条系统消息、之后也没有补唤醒（§6.5.6 规则 5）。
//
// 全部确定性 fake provider（不碰网络）：按轮脚本、逐请求记录，等待用轮询。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
)

// p3bEvalRound 一轮模型响应：正文 / tool call / provider 错误（三选一），
// gate 非空时这一轮先阻塞（停止护栏要在「run 中途」插人）。
type p3bEvalRound struct {
	content string
	tool    *core.ToolCall
	err     error
	gate    <-chan struct{}
}

// p3bEvalProvider 按轮返回的确定性 provider；reqs 保留每次请求（评测 9 断言快照内容）。
type p3bEvalProvider struct {
	mu     sync.Mutex
	rounds []p3bEvalRound
	reqs   []provider.StreamRequest
}

func newP3bEvalProvider(rounds ...p3bEvalRound) *p3bEvalProvider {
	return &p3bEvalProvider{rounds: rounds}
}

func (p *p3bEvalProvider) Stream(ctx context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	idx := len(p.reqs)
	if req != nil {
		p.reqs = append(p.reqs, *req)
	}
	round := p3bEvalRound{}
	if len(p.rounds) > 0 {
		if idx < len(p.rounds) {
			round = p.rounds[idx]
		} else {
			round = p.rounds[len(p.rounds)-1]
		}
	}
	p.mu.Unlock()
	if round.gate != nil {
		select {
		case <-round.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if round.err != nil {
		return round.err
	}
	model := ""
	if req != nil {
		model = req.Model
	}
	ev := provider.LLMEndEvent{
		FinishReason: core.FinishReasonStop, Content: round.content, Model: model,
		Usage: core.Usage{Input: 20, Output: 10},
	}
	if round.tool != nil {
		ev.ToolCalls = []core.ToolCall{*round.tool}
		ev.FinishReason = core.FinishReasonToolCall
	}
	return h(ev)
}

func (p *p3bEvalProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func (p *p3bEvalProvider) requestAt(i int) *provider.StreamRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.reqs) {
		return nil
	}
	cp := p.reqs[i]
	return &cp
}

func (p *p3bEvalProvider) lastRequest() *provider.StreamRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reqs) == 0 {
		return nil
	}
	cp := p.reqs[len(p.reqs)-1]
	return &cp
}

// p3bEvalRequestText 把一次 provider 请求里的全部正文拼起来（含 hidden 群快照块）。
func p3bEvalRequestText(req *provider.StreamRequest) string {
	if req == nil {
		return ""
	}
	var b strings.Builder
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			b.WriteString(c.Content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// p3bEvalEventCount 事件流里满足条件的条数。
func p3bEvalEventCount(buf *syncBuf, eventType string, match func(map[string]any) bool) int {
	n := 0
	for _, ev := range readAllLines(buf) {
		if ev["event_type"] != eventType {
			continue
		}
		if match != nil && !match(ev) {
			continue
		}
		n++
	}
	return n
}

// p3bEvalAgentTasks 账本里 target=agent 的任务（评测要「恰好一条」）。
func p3bEvalAgentTasks(t *testing.T, m *manager) []*agenthub.Task {
	t.Helper()
	all, err := mustHubStore(t, m).ListTasks(context.Background(), agenthub.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]*agenthub.Task, 0, 1)
	for _, task := range all {
		if task != nil && task.Target.Kind == agenthub.TargetAgent {
			out = append(out, task)
		}
	}
	return out
}

func p3bEvalLoadTask(t *testing.T, m *manager, id agenthub.TaskID) *agenthub.Task {
	t.Helper()
	got, err := mustHubStore(t, m).LoadTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// p3bEvalGroupTexts 群日志里某发送者的普通正文（结果卡不算气泡）。
func p3bEvalGroupTexts(msgs []agenthub.Message, senderKind agenthub.SenderKind, agentID agenthub.AgentID) []agenthub.Message {
	out := make([]agenthub.Message, 0, 2)
	for _, msg := range msgs {
		if msg.Kind != agenthub.KindText || msg.Sender.Kind != senderKind {
			continue
		}
		if agentID != "" && msg.Sender.AgentID != agentID {
			continue
		}
		out = append(out, msg)
	}
	return out
}

// p3bEvalScenario 评测 8/9 共用的三人群 + 生产链路状态。
type p3bEvalScenario struct {
	m             *manager
	buf           *syncBuf
	g             *agenthub.Group
	a, b, c       agenthub.AgentProfile
	fa, fb, fc    *p3bEvalProvider
	bsA, bsB, bsC *bridgeSession
	task          *agenthub.Task
	card          agenthub.Message
}

// p3bEvalScenarioRun 走生产链路：人 @A → A 的 production assign_task → B 的群 run →
// AgentEnd 结算 → 结果卡 + 派发者回推。fail 非空时 B 的 provider 以该错误失败。
func p3bEvalScenarioRun(t *testing.T, prefix string, fail error) *p3bEvalScenario {
	t.Helper()
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createDispatchAgent(t, m, buf, 1, prefix+"A", true)
	b := createDispatchAgent(t, m, buf, 2, prefix+"B", false)
	c := createDispatchAgent(t, m, buf, 3, prefix+"C", false)
	g := assignGroup(t, m, buf, 4, prefix+"群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member", c.ID: "member",
	})
	assignArgs := fmt.Sprintf(
		`{"to_agent":%q,"instruction":"把三家价格表整理成一页","title":"价格表一页"}`,
		string(b.ID),
	)
	fa := newP3bEvalProvider(
		// 第 1 轮：A 在生产 loop 里调 assign_task（不是测试直接调工具）。
		p3bEvalRound{tool: &core.ToolCall{Id: "call_assign", Index: 0, Name: "assign_task", Arguments: assignArgs}},
		// 第 2 轮：A 收尾；不带 @，否则会再叫醒别人。
		p3bEvalRound{content: "已派给 " + prefix + "B，等结果。"},
		// 第 3 轮：被 task_result 唤醒后的汇报（结果唤醒不是群轮次，不写群气泡）。
		p3bEvalRound{content: "收到结果，已经向群里汇报。"},
	)
	// B 的结算必须晚于 A 的第一轮：派发者空闲时收到结果是本评测的前提
	// （「A 还在跑时结果排队」的并发语义由 P3b-05/06 的单测覆盖，这里要的是
	// 确定性的验收链路，不能靠两个 run 赛跑）。
	releaseB := make(chan struct{})
	bRound := p3bEvalRound{content: "价格表整理好了：A $49 / B $39 / C 未公开。", gate: releaseB}
	if fail != nil {
		bRound = p3bEvalRound{err: fail, gate: releaseB}
	}
	fb := newP3bEvalProvider(bRound)
	fc := newP3bEvalProvider(p3bEvalRound{content: "我不该被叫醒"})

	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	setGroupFakeLoopIters(t, m, bsA, g, string(a.ID), fa, 2)
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	setGroupFakeLoopIters(t, m, bsB, g, string(b.ID), fb, 1)
	bsC := mustGroupSession(t, m, string(c.ID), string(g.ID))
	setGroupFakeLoopIters(t, m, bsC, g, string(c.ID), fc, 1)

	if ok, errMsg, _ := hubCmd(t, m, buf, 10, "hub_group_send", map[string]any{
		"group_id": string(g.ID),
		"text":     "@" + prefix + "A 把价格表的事派给 " + prefix + "B",
	}); !ok {
		t.Fatalf("人类 @A 失败: %s", errMsg)
	}

	// 产线里工具调用是异步的：等账本出现那条 agent 任务；等 A 的第一轮（工具轮 + 收尾）
	// 真的收尾，再放 B 结算 —— 派发者必须是在空闲状态下被结果唤醒的。
	convWait(t, "A 在生产 run 里用 assign_task 建出任务", func() bool {
		tasks := p3bEvalAgentTasks(t, m)
		return len(tasks) == 1
	})
	convWait(t, "A 的第一轮收尾（工具轮 + 收尾正文）", func() bool {
		return fa.callCount() >= 2 && !bsA.running.Load() && !bsA.hubGroupBusy.Load()
	})
	close(releaseB)
	task := p3bEvalAgentTasks(t, m)[0]
	convWait(t, "任务结算为 delivered", func() bool {
		got := p3bEvalLoadTask(t, m, task.ID)
		return got != nil && got.Status == agenthub.TaskDelivered
	})
	task = p3bEvalLoadTask(t, m, task.ID)

	msgs := p3bMessages(t, m, g.ID)
	cards := p3bTaskResultCards(msgs, task.ID)
	if len(cards) != 1 {
		t.Fatalf("结果卡 = %d 张, want 1（msgs=%+v）", len(cards), msgs)
	}
	return &p3bEvalScenario{
		m: m, buf: buf, g: g, a: a, b: b, c: c,
		fa: fa, fb: fb, fc: fc, bsA: bsA, bsB: bsB, bsC: bsC,
		task: task, card: cards[0],
	}
}

// TestEval8AgentAssignCompletesAndWakesDispatcher：评测 8（方案 §10.3）。
func TestEval8AgentAssignCompletesAndWakesDispatcher(t *testing.T) {
	s := p3bEvalScenarioRun(t, "Eval8", nil)
	m, buf, g := s.m, s.buf, s.g

	// ① 账本：恰好一条 agent 目标任务，A 派、B 执行、终态 delivered（完成且已回推）。
	tasks := p3bEvalAgentTasks(t, m)
	if len(tasks) != 1 {
		t.Fatalf("agent 目标任务 = %d 条, want 1", len(tasks))
	}
	task := tasks[0]
	if task.Target.AgentID != s.b.ID || task.Target.SessionID != hubSessionID(string(s.b.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}) {
		t.Fatalf("任务目标 = %+v, want B 的群会话", task.Target)
	}
	if task.Origin.Kind != agenthub.OriginGroup || task.Origin.AgentID != s.a.ID || task.Origin.GroupID != g.ID {
		t.Fatalf("任务来源 = %+v, want A@群", task.Origin)
	}
	if task.Status != agenthub.TaskDelivered {
		t.Fatalf("任务状态 = %s, want delivered", task.Status)
	}
	if task.Result == nil || !strings.Contains(task.Result.Summary, "价格表整理好了") {
		t.Fatalf("任务结果 = %+v", task.Result)
	}
	completedUpdates := p3bEvalEventCount(buf, "hub_task_update", func(ev map[string]any) bool {
		return ev["task_id"] == string(task.ID) && ev["status"] == string(agenthub.TaskCompleted)
	})
	if completedUpdates != 1 {
		t.Fatalf("completed 的 hub_task_update = %d 条, want 1", completedUpdates)
	}

	// ② 群日志：恰好一张卡，执行者=B，task_id/status/summary 齐；没有 mentions/路由。
	msgs := p3bMessages(t, m, g.ID)
	cards := p3bTaskResultCards(msgs, task.ID)
	if len(cards) != 1 {
		t.Fatalf("结果卡 = %d 张, want 1", len(cards))
	}
	card := cards[0]
	if card.Sender.Kind != agenthub.SenderAgent || card.Sender.AgentID != s.b.ID {
		t.Fatalf("卡执行者 = %+v, want B", card.Sender)
	}
	if p3bRefString(t, card.Refs, "task_id") != string(task.ID) ||
		p3bRefString(t, card.Refs, "status") != string(agenthub.TaskCompleted) ||
		p3bRefString(t, card.Refs, "agent_id") != string(s.b.ID) ||
		p3bRefString(t, card.Refs, "origin_agent_id") != string(s.a.ID) {
		t.Fatalf("卡 refs = %+v", card.Refs)
	}
	if !strings.Contains(card.Text, "价格表整理好了") {
		t.Fatalf("卡正文 = %q", card.Text)
	}
	if len(card.Mentions) != 0 || card.MentionAll || card.RoundID != "" || card.ReplyToSeq != 0 {
		t.Fatalf("结果卡不能带 mentions/路由: %+v", card)
	}
	if n := p3bEvalEventCount(buf, "hub_message", func(ev map[string]any) bool {
		msg, _ := ev["message"].(map[string]any)
		return msg != nil && msg["kind"] == string(agenthub.KindTaskResult) && msg["task_id"] == string(task.ID)
	}); n != 1 {
		t.Fatalf("task_result hub_message 事件 = %d 条, want 1", n)
	}

	// ③ B 的最终答复只进结果卡：不重复写普通群气泡（§6.7.9 第 6 步）。
	if got := p3bEvalGroupTexts(msgs, agenthub.SenderAgent, s.b.ID); len(got) != 0 {
		t.Fatalf("B 写了 %d 条普通群消息, want 0（结果由卡承载）", len(got))
	}

	// ④ 派发者 A 被唤醒：HasTaskResult + 恰好一次 delivered 事件 + 真的又跑了一轮，
	//    且这次唤醒的请求里带着 B 的结果正文。
	if !s.bsA.s.HasTaskResult(string(task.ID)) {
		t.Fatal("派发者 A 没收到 task_result")
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("task_result_delivered = %d 次, want 1", n)
	}
	convWait(t, "A 被结果唤醒并开跑（第 3 次 provider 调用）", func() bool { return s.fa.callCount() >= 3 })
	texts := p3bTaskResultTexts(s.bsA, task.ID)
	if len(texts) != 1 || !strings.Contains(texts[0], "价格表整理好了") {
		t.Fatalf("A 的 task_result 正文 = %+v", texts)
	}
	if wakeReq := s.fa.requestAt(2); !strings.Contains(p3bEvalRequestText(wakeReq), "价格表整理好了") {
		t.Fatalf("A 的唤醒请求里没有结果正文：%s", p3bEvalRequestText(wakeReq))
	}

	// ⑤ C（本轮没被点名的成员）零 provider 调用、零唤醒、零输入积压。
	// 负向断言先留一个窗口：run 是异步起的，「没等到」不等于「没发生」。
	time.Sleep(200 * time.Millisecond)
	if n := s.fc.callCount(); n != 0 {
		t.Fatalf("C 被调用了 %d 次 provider, want 0", n)
	}
	if s.bsC.s.HasTaskResult(string(task.ID)) {
		t.Fatal("C 收到了 task_result：只应回推派发者")
	}
	if n := s.bsC.s.PendingCount(); n != 0 {
		t.Fatalf("C 有 %d 条积压输入, want 0", n)
	}
	cSession := hubSessionID(string(s.c.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	if n := p3bEvalEventCount(buf, "agent_start", func(ev map[string]any) bool {
		return ev["session_id"] == cSession
	}); n != 0 {
		t.Fatalf("C 有 %d 个 agent_start, want 0", n)
	}

	// ⑥ 防循环：没有第二次 assign，也没有第二段对话唤醒。
	if n := p3bEvalEventCount(buf, "hub_round", func(ev map[string]any) bool {
		round, _ := ev["round"].(map[string]any)
		if round == nil {
			return false
		}
		raw, _ := round["targets"].([]any)
		for _, x := range raw {
			if x == string(s.b.ID) {
				return true
			}
		}
		return false
	}); n != 1 {
		t.Fatalf("唤醒 B 的轮次 = %d, want 1（只该有 assign 那一次）", n)
	}
	assignMsgs := 0
	for _, msg := range msgs {
		if msg.Intent == agenthub.IntentAssign {
			assignMsgs++
		}
	}
	if assignMsgs != 1 {
		t.Fatalf("intent=assign 的群消息 = %d 条, want 1", assignMsgs)
	}
	// A 的群正文 = 派任务那条 + 收尾那句；被结果唤醒后不再写气泡（否则就是接龙）。
	if got := p3bEvalGroupTexts(msgs, agenthub.SenderAgent, s.a.ID); len(got) != 2 {
		t.Fatalf("A 的群消息 = %d 条, want 2（assign + 收尾）: %+v", len(got), got)
	}
	// 对话控制器：只用了人类那 1 轮，队列空、不是终态（结果唤醒不计入对话轮次）。
	if v := convView(m, g.ID); v.TurnsUsed != 1 || v.QueueLen != 0 || v.Status.Terminal() {
		t.Fatalf("对话视图 = %+v, want turns=1 queue=0 非终态", v)
	}
	// A 只被唤醒一次（1 次 human @ + 1 次 task_result；没有互相 @ 的第二跳）。
	if n := s.fa.callCount(); n != 3 {
		t.Fatalf("A 的 provider 调用 = %d 次, want 3（工具轮 + 收尾 + 结果唤醒）", n)
	}
	if n := s.fb.callCount(); n != 1 {
		t.Fatalf("B 的 provider 调用 = %d 次, want 1", n)
	}

	// ⑦ 重放同一条 AgentEnd：不复制卡、不重复回推、状态不回退。
	runID := ""
	for _, ev := range readAllLines(buf) {
		if ev["event_type"] != "agent_start" || ev["session_id"] != hubSessionID(string(s.b.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}) {
			continue
		}
		runID, _ = ev["run_id"].(string)
	}
	if runID == "" {
		t.Fatal("找不到 B 的 run_id（agent_start 缺 run_id）")
	}
	replay := &bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}}
	m.finishHubTask(replay, &events.AgentEnd{
		RunId: runID, Content: "重放的旧正文", FinishReason: core.FinishReasonStop,
	})
	if got := p3bEvalLoadTask(t, m, task.ID); got.Status != agenthub.TaskDelivered || got.Result == nil ||
		!strings.Contains(got.Result.Summary, "价格表整理好了") {
		t.Fatalf("重放改写了任务终态: %+v", got)
	}
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)); n != 1 {
		t.Fatalf("重放后结果卡 = %d 张, want 1", n)
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("重放后 task_result_delivered = %d 次, want 1", n)
	}
}

// p3bEvalCardLine 群快照里这张卡的渲染行（renderGroupSnapshot 的格式）。
// p3bEvalCardLine 是快照里这张卡应当渲染出的行（P6-11 起发送者带落盘角色）。
func p3bEvalCardLine(card agenthub.Message) string {
	who := string(card.Sender.AgentID)
	if card.Sender.Kind == agenthub.SenderAgent {
		who += "(member)" // 场景里 B 是 member
	}
	return fmt.Sprintf("#%d %s: %s", card.Seq, who, strings.TrimSpace(card.Text))
}

// TestEval9OtherMemberSeesResultOnNextMention：评测 9（方案 §10.3 / §6.5.7）。
func TestEval9OtherMemberSeesResultOnNextMention(t *testing.T) {
	s := p3bEvalScenarioRun(t, "Eval9", nil)
	m, buf, g := s.m, s.buf, s.g
	task := s.task

	// 结果卡落地时 C 没有被叫醒（结果卡不路由）。
	if n := s.fc.callCount(); n != 0 {
		t.Fatalf("结果卡落地就吵醒了 C（%d 次 provider 调用）", n)
	}
	// 人 @C：C 的下一次唤醒请求必须带那张卡（seq + 执行者 + 正文）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 20, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@Eval9C 你看下 B 的结果，附一句评价",
	}); !ok {
		t.Fatalf("人类 @C 失败: %s", errMsg)
	}
	convWait(t, "C 被 @ 后开跑", func() bool { return s.fc.callCount() >= 1 })
	reqText := p3bEvalRequestText(s.fc.lastRequest())
	if !strings.Contains(reqText, p3bEvalCardLine(s.card)) {
		t.Fatalf("C 的快照里没有结果卡行 %q：\n%s", p3bEvalCardLine(s.card), reqText)
	}
	if !strings.Contains(reqText, "价格表整理好了") {
		t.Fatalf("C 的快照里没有结果正文：\n%s", reqText)
	}
	if n := strings.Count(reqText, p3bEvalCardLine(s.card)); n != 1 {
		t.Fatalf("快照里同一张卡出现 %d 次, want 1", n)
	}
	// C 的回复（没有 @）不产生第二个唤醒：B 不再跑，也没有新的轮次。
	convWait(t, "C 的回复落账", func() bool {
		return len(p3bEvalGroupTexts(p3bMessages(t, m, g.ID), agenthub.SenderAgent, s.c.ID)) == 1
	})
	msgs := p3bMessages(t, m, g.ID)
	if n := s.fb.callCount(); n != 1 {
		t.Fatalf("C 回复后 B 又被调用 %d 次, want 1（只有它自己那轮）", n)
	}
	if n := p3bEvalEventCount(buf, "hub_round", func(ev map[string]any) bool {
		round, _ := ev["round"].(map[string]any)
		if round == nil || round["trigger_seq"] != float64(s.card.Seq) {
			return false
		}
		return true
	}); n != 0 {
		t.Fatalf("结果卡触发了 %d 个路由轮次, want 0", n)
	}
	if n := p3bTaskResultCards(msgs, task.ID); len(n) != 1 {
		t.Fatalf("C 说话后结果卡 = %d 张, want 1", len(n))
	}

	t.Run("failed", func(t *testing.T) {
		const reason = "上游 529 过载：简报接口持续失败"
		f := p3bEvalScenarioRun(t, "Eval9F", provider.MarkPermanent(fmt.Errorf("%s", reason)))
		fm, fbuf, fg := f.m, f.buf, f.g
		// ① 失败卡 + 错误原因；账本 failed 但已回推（delivered）。
		if f.task.Status != agenthub.TaskDelivered {
			t.Fatalf("失败任务状态 = %s, want delivered（failed 已回推）", f.task.Status)
		}
		if f.card.Kind != agenthub.KindTaskResult ||
			p3bRefString(t, f.card.Refs, "status") != string(agenthub.TaskFailed) ||
			p3bRefString(t, f.card.Refs, "error") != reason {
			t.Fatalf("失败卡 = %+v refs=%+v", f.card, f.card.Refs)
		}
		if !strings.Contains(f.card.Text, reason) {
			t.Fatalf("失败卡正文 = %q, want 含错误原因", f.card.Text)
		}
		// ② 派发者 A 被唤醒且看到错误。
		if !f.bsA.s.HasTaskResult(string(f.task.ID)) {
			t.Fatal("失败结果没回推给 A")
		}
		convWait(t, "A 被失败结果唤醒", func() bool { return f.fa.callCount() >= 3 })
		texts := p3bTaskResultTexts(f.bsA, f.task.ID)
		if len(texts) != 1 || !strings.Contains(texts[0], reason) {
			t.Fatalf("A 收到的失败正文 = %+v", texts)
		}
		// ③ C 下次被 @ 仍能在快照里看到 failed 卡（正文就是错误原因）。
		if ok, errMsg, _ := hubCmd(t, fm, fbuf, 20, "hub_group_send", map[string]any{
			"group_id": string(fg.ID), "text": "@Eval9FC 你看下失败原因",
		}); !ok {
			t.Fatalf("失败分支：人类 @C 失败: %s", errMsg)
		}
		convWait(t, "失败分支 C 被 @ 后开跑", func() bool { return f.fc.callCount() >= 1 })
		reqText := p3bEvalRequestText(f.fc.lastRequest())
		if !strings.Contains(reqText, p3bEvalCardLine(f.card)) || !strings.Contains(reqText, reason) {
			t.Fatalf("C 的快照里没有失败卡行 %q：\n%s", p3bEvalCardLine(f.card), reqText)
		}
		if n := f.fb.callCount(); n != 1 {
			t.Fatalf("失败分支 C 回复后 B 又被调用 %d 次, want 1", n)
		}
	})
}

// TestEval8HumanStopStopsChain：人类停止发生在「A 已 @B、B 还排在队列里」的窗口：
// B 从不启动，对话进 stopped（恰好一条系统消息），之后也没有补唤醒。
func TestEval8HumanStopStopsChain(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createDispatchAgent(t, m, buf, 1, "EvalStopA", true)
	b := createDispatchAgent(t, m, buf, 2, "EvalStopB", false)
	g := assignGroup(t, m, buf, 3, "停止评测群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	gate := make(chan struct{})
	fa := newP3bEvalProvider(
		// A 的 run 里用生产 group_say @B：消息落账 + 唤醒入队（A 自己还在跑，泵不放行）。
		p3bEvalRound{tool: &core.ToolCall{
			Id: "call_say", Index: 0, Name: "group_say",
			Arguments: fmt.Sprintf(`{"group_id":%q,"text":"@EvalStopB 你来写结论。"}`, string(g.ID)),
		}},
		// 第二轮卡住：停止要打在 run 中途，B 的那条唤醒还留在队列里。
		p3bEvalRound{content: "我先起了个头。", gate: gate},
	)
	fb := newP3bEvalProvider(p3bEvalRound{content: "B 不该跑起来"})
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	setGroupFakeLoopIters(t, m, bsA, g, string(a.ID), fa, 2)
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	setGroupFakeLoopIters(t, m, bsB, g, string(b.ID), fb, 1)
	defer close(gate) // 收尾：别把 goroutine 留在 TempDir 清理之后

	if ok, errMsg, _ := hubCmd(t, m, buf, 10, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@EvalStopA 你先起个头",
	}); !ok {
		t.Fatalf("人类 @A 失败: %s", errMsg)
	}
	// A 的 group_say 已入队给 B；A 还在第二轮（gate 上）→ 队列里恰好一条。
	convWait(t, "A @B 的唤醒进入队列", func() bool {
		v := convView(m, g.ID)
		return v.QueueLen == 1 && v.TurnsUsed == 1
	})
	if n := fb.callCount(); n != 0 {
		t.Fatalf("B 在停止前就被启动了（%d 次 provider 调用）", n)
	}

	ok, errMsg, data := hubCmd(t, m, buf, 11, "hub_conversation_stop", map[string]any{
		"group_id": string(g.ID),
	})
	if !ok {
		t.Fatalf("hub_conversation_stop: %s", errMsg)
	}
	conv, _ := data["conversation"].(map[string]any)
	if conv == nil || conv["status"] != string(agenthub.ConversationStopped) {
		t.Fatalf("停止回执 = %+v", data)
	}
	if n, _ := data["interrupted"].(float64); n < 1 {
		t.Fatalf("interrupted = %v, want >= 1", data["interrupted"])
	}
	// 被中断的 run 收尾（AgentEnd abort）之后，队列不能把 B 补投出去。
	convWait(t, "A 的 run 收尾", func() bool {
		return !bsA.hubGroupBusy.Load() && !bsA.running.Load()
	})
	time.Sleep(250 * time.Millisecond) // 给可能迟到的一次泵留窗口（没有就该一直为 0）

	if n := fb.callCount(); n != 0 {
		t.Fatalf("停止之后 B 还是跑了（%d 次 provider 调用）", n)
	}
	bSession := hubSessionID(string(b.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	if n := p3bEvalEventCount(buf, "agent_start", func(ev map[string]any) bool {
		return ev["session_id"] == bSession
	}); n != 0 {
		t.Fatalf("B 有 %d 个 agent_start, want 0", n)
	}
	if v := convView(m, g.ID); v.Status != agenthub.ConversationStopped || v.QueueLen != 0 || v.TurnsUsed != 1 {
		t.Fatalf("停止后对话视图 = %+v, want stopped/queue=0/turns=1", v)
	}
	msgs := p3bMessages(t, m, g.ID)
	systems := convSystemMessages(msgs)
	if len(systems) != 1 || !strings.Contains(systems[0].Text, "停止了自动对话") {
		t.Fatalf("停止系统消息 = %+v, want 恰好一条", systems)
	}
	if got := p3bEvalGroupTexts(msgs, agenthub.SenderAgent, b.ID); len(got) != 0 {
		t.Fatalf("B 在群里发言 %d 条, want 0", len(got))
	}
	// 只有一个 stopped 事件（幂等；泵到终态也只写一条系统消息）。
	if n := p3bEvalEventCount(buf, "hub_conversation", func(ev map[string]any) bool {
		conv, _ := ev["conversation"].(map[string]any)
		return conv != nil && conv["status"] == string(agenthub.ConversationStopped)
	}); n != 1 {
		t.Fatalf("stopped 的 hub_conversation 事件 = %d 条, want 1", n)
	}
	if n := len(p3bTaskResultCards(msgs, "")); n != 0 {
		t.Fatalf("停止链路上冒出 %d 张任务卡, want 0", n)
	}
}
