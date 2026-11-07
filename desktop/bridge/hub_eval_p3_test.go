package main

// P3a-10：评测 7（方案 §10.3）——群组 @、历史、私有上下文、流式一致性。
//
//  1. @all 唤醒除发送者外的全部成员，每个被唤醒的成员各写一条 final；
//  2. @A 只唤醒 A：B/C 在有界窗口内没有 agent 消息，游标也不动；
//  3. 无 @：只写日志，不写路由回执、不发 hub_round、不唤醒；
//  4. 群历史能翻到早期消息（命令分页 + group_history 工具的 before_seq/search）；
//  5. owner-only 与 approval_stub 不进非 owner 的快照，也不进真实唤醒请求；
//  6. 两个成员同时流式：草稿各自一个 message_id，final 与 delta 同 ID、正文一致，
//     每条 final 只发一次事件。P3a-09 的 80ms 合并/顺序/迟到 delta 由
//     hub_events_test.go 与 scripts/test-hub-stream.mjs 覆盖，这里只补评测级断言。
//
// 全部走确定性 fake provider（不碰网络）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// eval7GroupFixture 建 n 个智能体（第一个 owner，其余 member）和一个群。
func eval7GroupFixture(t *testing.T, m *manager, buf *syncBuf, cmdID int, groupName string, names ...string) ([]agenthub.AgentProfile, *agenthub.Group) {
	t.Helper()
	agents := make([]agenthub.AgentProfile, 0, len(names))
	for i, name := range names {
		agents = append(agents, createHubAgent(t, m, buf, cmdID+i, name))
	}
	members := make([]struct {
		id   agenthub.AgentID
		role string
	}, 0, len(agents))
	for i, a := range agents {
		role := "member"
		if i == 0 {
			role = "owner"
		}
		members = append(members, struct {
			id   agenthub.AgentID
			role string
		}{a.ID, role})
	}
	return agents, groupWithMembers(t, m, buf, cmdID+len(agents), groupName, members...)
}

// eval7WireGroup 预装配成员的群会话并换成确定性 fake loop。
// 不预装配的话唤醒路径会建真实 provider loop，评测就会碰网络。
func eval7WireGroup(t *testing.T, m *manager, g *agenthub.Group, agentID agenthub.AgentID, fake provider.Provider) *bridgeSession {
	t.Helper()
	bs, err := m.hubGroupSession(string(agentID), string(g.ID))
	if err != nil {
		t.Fatalf("group session for %s: %v", agentID, err)
	}
	setGroupFakeLoop(t, m, bs, g, string(agentID), fake)
	return bs
}

func eval7AgentMessages(msgs []agenthub.Message, id agenthub.AgentID) []agenthub.Message {
	var out []agenthub.Message
	for _, msg := range msgs {
		if msg.Sender.Kind == agenthub.SenderAgent && msg.Sender.AgentID == id {
			out = append(out, msg)
		}
	}
	return out
}

func eval7ReadGroup(t *testing.T, m *manager, gid agenthub.GroupID) []agenthub.Message {
	t.Helper()
	msgs, err := mustHubStore(t, m).ReadMessages(context.Background(), gid, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func eval7RoundOf(t *testing.T, data map[string]any) (id string, mentionAll bool, targets []agenthub.AgentID) {
	t.Helper()
	raw, err := json.Marshal(data["round"])
	if err != nil {
		t.Fatalf("round 字段不可序列化: %v", err)
	}
	var round struct {
		ID         string             `json:"id"`
		MentionAll bool               `json:"mention_all"`
		Targets    []agenthub.AgentID `json:"targets"`
	}
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("round payload = %s: %v", raw, err)
	}
	return round.ID, round.MentionAll, round.Targets
}

func eval7HistoryPage(t *testing.T, data map[string]any) ([]agenthub.Message, bool) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("history payload 不可序列化: %v", err)
	}
	var page struct {
		Messages []agenthub.Message `json:"messages"`
		HasMore  bool               `json:"has_more"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("history payload = %s: %v", raw, err)
	}
	return page.Messages, page.HasMore
}

func eval7HasText(msgs []agenthub.Message, text string) bool {
	for _, msg := range msgs {
		if strings.Contains(msg.Text, text) {
			return true
		}
	}
	return false
}

// 评测 7-1：@all 唤醒全部其他成员；每个成员一条 final；人只有触发消息。
func TestEval7MentionAllWakesEveryOtherMember(t *testing.T) {
	m, buf := testManager(t)
	agents, g := eval7GroupFixture(t, m, buf, 1, "评测7全群", "Eval7AllA", "Eval7AllB", "Eval7AllC")
	replies := []string{"A 收到", "B 收到", "C 收到"}
	for i, a := range agents {
		eval7WireGroup(t, m, g, a.ID, &evalProvider{reply: replies[i]})
	}

	ok, errMsg, data := hubCmd(t, m, buf, 40, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@all 每人回一句",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	roundID, mentionAll, targets := eval7RoundOf(t, data)
	if roundID == "" || !mentionAll {
		t.Fatalf("round = %+v，want @all round", data["round"])
	}
	triggerSeq := numOr(data, "seq", 0)
	if len(targets) != len(agents) {
		t.Fatalf("@all targets = %v，want 全部 %d 名成员", targets, len(agents))
	}
	hit := map[agenthub.AgentID]bool{}
	for _, id := range targets {
		hit[id] = true
	}
	for _, a := range agents {
		if !hit[a.ID] {
			t.Fatalf("@all targets = %v，缺少 %s", targets, a.ID)
		}
	}

	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for i, a := range agents {
			if !eval7HasText(eval7AgentMessages(msgs, a.ID), replies[i]) {
				return false
			}
		}
		return true
	})
	for i, a := range agents {
		own := eval7AgentMessages(msgs, a.ID)
		if len(own) != 1 {
			t.Fatalf("%s final 数 = %d，want 1（@all 每人只回一条）", a.ID, len(own))
		}
		if own[0].Text != replies[i] || own[0].RoundID != roundID {
			t.Fatalf("%s final = %+v，want text %q round %q", a.ID, own[0], replies[i], roundID)
		}
	}
	// 触发者是人：日志里只有一条 human 消息（触发本身），没有以 human 身份冒出的 agent 回复。
	var human int
	for _, msg := range msgs {
		if msg.Sender.Kind == agenthub.SenderHuman {
			human++
		}
	}
	if human != 1 {
		t.Fatalf("human 消息数 = %d，want 1（发送者不该被当成 agent 回复）", human)
	}
	// 人触发 + 路由回执 + 3 条 final：多出来的就是重复 final 或幽灵发送者。
	if len(msgs) != 1+1+len(agents) {
		t.Fatalf("群日志 %d 条，want %d: %+v", len(msgs), 2+len(agents), msgs)
	}
	// 每个成员的游标都只推进到本轮触发消息：@all 的唤醒 header 不能串到别人身上。
	// 消息先落、游标后写（不在一个事务里），所以等游标而不是立刻读。
	st := mustHubStore(t, m)
	if triggerSeq == 0 {
		t.Fatal("send response 缺 seq")
	}
	for _, a := range agents {
		var cur *agenthub.Cursor
		var err error
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cur, err = st.LoadCursor(context.Background(), g.ID, a.ID); err == nil && cur.AgentContextSeq == triggerSeq {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err != nil || cur.AgentContextSeq != triggerSeq {
			t.Fatalf("%s 游标 = %+v (%v)，want trigger %d（唤醒归属串台）", a.ID, cur, err, triggerSeq)
		}
	}
}

// 评测 7-2：@A 只唤醒 A；其余成员有界窗口内没有 agent 消息、游标不动。
func TestEval7MentionOneWakesOnlyThatMember(t *testing.T) {
	m, buf := testManager(t)
	agents, g := eval7GroupFixture(t, m, buf, 1, "评测7单点", "Eval7OneA", "Eval7OneB", "Eval7OneC")
	// 三个都装 fake loop：任何误唤醒都会留下可断言的 agent 消息，而不是静默失败。
	sessions := make([]*bridgeSession, len(agents))
	for i, a := range agents {
		sessions[i] = eval7WireGroup(t, m, g, a.ID, &evalProvider{reply: fmt.Sprintf("冒泡-%d", i)})
	}

	ok, errMsg, data := hubCmd(t, m, buf, 40, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@Eval7OneB 只回你",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	roundID, mentionAll, targets := eval7RoundOf(t, data)
	if mentionAll || len(targets) != 1 || targets[0] != agents[1].ID {
		t.Fatalf("targets = %v (mention_all=%v)，want 只有 %s", targets, mentionAll, agents[1].ID)
	}

	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return eval7HasText(eval7AgentMessages(msgs, agents[1].ID), "冒泡-1")
	})
	// 有界静默窗口：误唤醒的 run 用的是立即返回的 fake provider，300ms 足够走完一轮。
	time.Sleep(300 * time.Millisecond)
	msgs = eval7ReadGroup(t, m, g.ID)
	for _, i := range []int{0, 2} {
		a := agents[i]
		if got := eval7AgentMessages(msgs, a.ID); len(got) != 0 {
			t.Fatalf("%s 未被 @，却写了 agent 消息: %+v", a.ID, got)
		}
		cur, err := mustHubStore(t, m).LoadCursor(context.Background(), g.ID, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.AgentContextSeq != 0 {
			t.Fatalf("%s 未被 @，游标却推进到 %d（说明被唤醒过）", a.ID, cur.AgentContextSeq)
		}
		if sessions[i].hubGroupBusy.Load() {
			t.Fatalf("%s 未被 @，群会话却处于 busy", a.ID)
		}
	}
	own := eval7AgentMessages(msgs, agents[1].ID)
	if len(own) != 1 || own[0].Text != "冒泡-1" || own[0].RoundID != roundID {
		t.Fatalf("target final = %+v，want 1 条 text %q round %q", own, "冒泡-1", roundID)
	}
	if sessions[1].hubGroupBusy.Load() {
		t.Fatal("target 已 final，群会话仍 busy")
	}
	// 回执只写被唤醒的 A：其他成员不能被写成 target。
	acts, err := mustHubStore(t, m).ReadActivity(context.Background(), g.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || !strings.Contains(acts[0].Summary, string(agents[1].ID)) {
		t.Fatalf("routing 回执 = %+v，want 只含 %s", acts, agents[1].ID)
	}
	if strings.Contains(acts[0].Summary, string(agents[0].ID)) || strings.Contains(acts[0].Summary, string(agents[2].ID)) {
		t.Fatalf("routing 回执把未 @ 的成员写进了 targets: %s", acts[0].Summary)
	}
}

// 评测 7-3：无 @ 只写日志，不产生回执/hub_round/唤醒，也没有 agent 消息。
func TestEval7NoMentionDoesNotWake(t *testing.T) {
	m, buf := testManager(t)
	agents, g := eval7GroupFixture(t, m, buf, 1, "评测7无at", "Eval7NoAtA", "Eval7NoAtB")
	sessions := make([]*bridgeSession, len(agents))
	for i, a := range agents {
		sessions[i] = eval7WireGroup(t, m, g, a.ID, &evalProvider{reply: "不该出现"})
	}

	ok, errMsg, data := hubCmd(t, m, buf, 40, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "只记录，不点名",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	// 响应为了兼容前端仍带 round 字段（前端 roundHasAction 会过滤空 targets），
	// 但本轮不该落回执、不该发 hub_round 事件、不该唤醒。
	roundID, mentionAll, targets := eval7RoundOf(t, data)
	if mentionAll || len(targets) != 0 {
		t.Fatalf("无 @ 竟然路由出了 targets=%v (round %q, mention_all=%v)", targets, roundID, mentionAll)
	}
	if _, hasWakeErrors := data["wake_errors"]; hasWakeErrors {
		t.Fatalf("无 @ 不该尝试唤醒，response 却带 wake_errors: %+v", data["wake_errors"])
	}
	if evs := hubEvents(t, buf, "hub_round"); len(evs) != 0 {
		t.Fatalf("无 @ 产生了 hub_round 事件: %+v", evs)
	}
	time.Sleep(300 * time.Millisecond)
	msgs := eval7ReadGroup(t, m, g.ID)
	if len(msgs) != 1 || msgs[0].Sender.Kind != agenthub.SenderHuman || msgs[0].Text != "只记录，不点名" {
		t.Fatalf("群日志 = %+v，want 只有人的那一条（无路由回执）", msgs)
	}
	acts, err := mustHubStore(t, m).ReadActivity(context.Background(), g.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 0 {
		t.Fatalf("无 @ 写了 routing activity: %+v", acts)
	}
	for i, a := range agents {
		if got := eval7AgentMessages(msgs, a.ID); len(got) != 0 {
			t.Fatalf("%s 无 @ 却发言: %+v", a.ID, got)
		}
		cur, err := mustHubStore(t, m).LoadCursor(context.Background(), g.ID, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.AgentContextSeq != 0 {
			t.Fatalf("%s 无 @ 游标却动了: %d", a.ID, cur.AgentContextSeq)
		}
		if sessions[i].hubGroupBusy.Load() {
			t.Fatalf("%s 无 @ 却处于 busy", a.ID)
		}
	}
}

// 评测 7-4：群历史可检索早期消息，不是只拿得到最新一页。
func TestEval7HistoryReachesEarlyMessages(t *testing.T) {
	m, buf := testManager(t)
	agents, g := eval7GroupFixture(t, m, buf, 1, "评测7历史", "Eval7HistA")
	ctx := context.Background()
	const early = "EARLY-MARKER-项目起点"
	for i := 1; i <= 40; i++ {
		text := fmt.Sprintf("历史消息 %03d", i)
		if i == 1 {
			text = early
		}
		if _, err := agenthub.AppendGroupMessage(ctx, mustHubStore(t, m), &agenthub.Message{
			GroupID: g.ID, Kind: agenthub.KindText,
			Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: text,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// ① 人的分页：最新页看不到第 1 条，靠 before_seq 一页页翻回去。
	ok, errMsg, data := hubCmd(t, m, buf, 10, "hub_group_history", map[string]any{
		"group_id": string(g.ID), "limit": 10,
	})
	if !ok {
		t.Fatalf("history: %s", errMsg)
	}
	page, hasMore := eval7HistoryPage(t, data)
	if len(page) != 10 || !hasMore {
		t.Fatalf("latest page = %d 条 (has_more=%v)，want 10 条且有更早", len(page), hasMore)
	}
	if eval7HasText(page, early) {
		t.Fatal("最新一页就包含第 1 条：断言退化，翻页没被验证")
	}
	found := false
	for i := 0; i < 8 && !found; i++ {
		before := page[0].Seq
		ok, errMsg, data = hubCmd(t, m, buf, 11+i, "hub_group_history", map[string]any{
			"group_id": string(g.ID), "limit": 10, "before_seq": before,
		})
		if !ok {
			t.Fatalf("history page %d: %s", i, errMsg)
		}
		page, _ = eval7HistoryPage(t, data)
		if len(page) == 0 {
			break
		}
		if page[0].Seq >= before {
			t.Fatalf("before_seq=%d 没往前翻（第一页 seq=%d）", before, page[0].Seq)
		}
		found = eval7HasText(page, early)
	}
	if !found {
		t.Fatal("before_seq 翻页始终到不了第 1 条群消息")
	}

	// ② 模型面：group_history 工具同样支持 before_seq 与 query 检索全量历史。
	bs, err := m.hubDMSession(string(agents[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	call := func(args string) string {
		t.Helper()
		tool, err := bs.hubEngine.GetTool(context.Background(), "group_history")
		if err != nil {
			t.Fatalf("group_history tool: %v", err)
		}
		out, err := tool.Call(context.Background(), "group_history", args)
		if err != nil {
			t.Fatalf("group_history(%s): %v", args, err)
		}
		return out
	}
	out := call(`{"group_id":"` + string(g.ID) + `","before_seq":11,"limit":10}`)
	if !strings.Contains(out, early) {
		t.Fatalf("group_history before_seq 没翻到早期消息: %s", out)
	}
	out = call(`{"group_id":"` + string(g.ID) + `","query":"early-marker"}`)
	if !strings.Contains(out, early) || !strings.Contains(out, `"count":1`) {
		t.Fatalf("group_history query 没检索到早期消息: %s", out)
	}
}

// 评测 7-5：owner-only 与 approval_stub 不进非 owner 的群上下文（快照 + 真实唤醒请求）。
func TestEval7PrivateContentStaysOutOfGroupContext(t *testing.T) {
	m, buf := testManager(t)
	agents, g := eval7GroupFixture(t, m, buf, 1, "评测7隐私", "Eval7PrivA", "Eval7PrivB")
	member := agents[1]
	ctx := context.Background()
	const ownerSecret = "OWNER-ONLY-SECRET-8842 授权口令 alpha"
	const approvalSecret = "APPROVAL-SECRET-9931 rm -rf /srv/prod"
	const publicText = "公开消息：周会改到周四"
	st := mustHubStore(t, m)
	if _, err := agenthub.AppendGroupMessage(ctx, st, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText, Sender: agenthub.Sender{Kind: agenthub.SenderHuman},
		Text: ownerSecret, Visibility: agenthub.VisibilityOwner,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := agenthub.AppendGroupMessage(ctx, st, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindApprovalStub, Sender: agenthub.Sender{Kind: agenthub.SenderSystem},
		Text: approvalSecret, Refs: map[string]any{"need_id": "need_priv"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := agenthub.AppendGroupMessage(ctx, st, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText, Sender: agenthub.Sender{Kind: agenthub.SenderHuman},
		Text: publicText,
	}); err != nil {
		t.Fatal(err)
	}

	fake := &evalProvider{reply: "隐私检查完成"}
	eval7WireGroup(t, m, g, member.ID, fake)
	ok, errMsg, data := hubCmd(t, m, buf, 10, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@Eval7PrivB 看公开消息",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	triggerSeq := numOr(data, "seq", 0)
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return eval7HasText(eval7AgentMessages(msgs, member.ID), "隐私检查完成")
	})

	// ① 快照正文：两条私有消息整条排除（owner-only 不是 redacted 占位），公开消息必须在
	// —— 否则「没泄漏」可能只是因为快照是空的。
	msgs := eval7ReadGroup(t, m, g.ID)
	snap := agenthub.BuildGroupSnapshot(g, member.ID, msgs, 0, agenthub.Round{
		ID: "rnd_eval7_priv", GroupID: g.ID, TriggerSeq: triggerSeq,
		Targets: []agenthub.AgentID{member.ID},
	}, agenthub.SnapshotOptions{})
	if strings.Contains(snap.Text, "OWNER-ONLY-SECRET") || strings.Contains(snap.Text, "APPROVAL-SECRET") {
		t.Fatalf("私有正文进了非 owner 快照:\n%s", snap.Text)
	}
	if strings.Contains(snap.Text, agenthub.OwnerOnlyPlaceholder) {
		t.Fatalf("owner-only 应当整条排除，快照里却出现占位符:\n%s", snap.Text)
	}
	if !strings.Contains(snap.Text, publicText) {
		t.Fatalf("快照没带公开消息（断言退化）:\n%s", snap.Text)
	}

	// ② 真实唤醒请求：模型看到的每一块（system / hidden wake / text）都不能有密文。
	req := fake.lastRequest()
	if req == nil {
		t.Fatal("provider 从未被调用：唤醒链路没走通")
	}
	var all strings.Builder
	var wakeSnapshot string
	wakes := 0
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			all.WriteString(c.Content)
			all.WriteByte('\n')
			if c.Type == core.ContentTypeHubGroupWake {
				wakes++
				if _, snapText, err := agenthub.ParseGroupWake(c.Content); err != nil {
					t.Fatalf("解析 hub_group_wake: %v", err)
				} else {
					wakeSnapshot = snapText
				}
			}
		}
	}
	if wakes != 1 {
		t.Fatalf("唤醒请求里的 hub_group_wake 块 = %d，want 1（否则隐私断言没覆盖真实上下文）", wakes)
	}
	text := all.String()
	if strings.Contains(text, "OWNER-ONLY-SECRET") || strings.Contains(text, "APPROVAL-SECRET") {
		t.Fatalf("私有正文进了真实唤醒请求:\n%s", text)
	}
	if !strings.Contains(wakeSnapshot, publicText) {
		t.Fatalf("唤醒的 hidden 快照没带公开消息（断言退化）:\n%s", wakeSnapshot)
	}
}

// eval7Barrier 让两个并发 provider 在开流前互等：如果发送路径是「跑完 A 再跑 B」，
// 「并行草稿」断言就退化成两条互不相干的单流；超时兜底避免唤醒 bug 变成挂死。
type eval7Barrier struct {
	mu      sync.Mutex
	waiting int
	ch      chan struct{}
}

func newEval7Barrier(n int) *eval7Barrier {
	return &eval7Barrier{waiting: n, ch: make(chan struct{})}
}

func (b *eval7Barrier) wait() {
	b.mu.Lock()
	b.waiting--
	last := b.waiting == 0
	if last {
		close(b.ch)
	}
	b.mu.Unlock()
	select {
	case <-b.ch:
	case <-time.After(5 * time.Second):
	}
}

// gatedStreamProvider 在屏障放行后分块流式，让两条流真正交错（并为 80ms 合并留出窗口）。
type gatedStreamProvider struct {
	chunks  []string
	reply   string
	barrier *eval7Barrier
}

func (p *gatedStreamProvider) Stream(_ context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.barrier.wait()
	for i, c := range p.chunks {
		if err := h(provider.LLMContentDeltaEvent{Delta: c, Index: int64(i)}); err != nil {
			return err
		}
		time.Sleep(30 * time.Millisecond)
	}
	return h(provider.LLMEndEvent{
		FinishReason: core.FinishReasonStop,
		Content:      p.reply,
		Model:        req.Model,
		Usage:        core.Usage{Input: 3, Output: 2},
	})
}

// 评测 7-6：两个成员并行流式，草稿不串台；final 与 delta 同 ID、正文一致、事件不重复。
func TestEval7ConcurrentStreamsKeepDraftAndFinalConsistent(t *testing.T) {
	m, buf := testManager(t)
	agents, g := eval7GroupFixture(t, m, buf, 1, "评测7流式", "Eval7StreamA", "Eval7StreamB")
	barrier := newEval7Barrier(len(agents))
	replies := []string{"A 正在回复", "B 也在回复"}
	chunks := [][]string{{"A 正在", "回复"}, {"B 也", "在回复"}}
	sessions := make([]*bridgeSession, len(agents))
	for i, a := range agents {
		sessions[i] = eval7WireGroup(t, m, g, a.ID, &gatedStreamProvider{
			chunks: chunks[i], reply: replies[i], barrier: barrier,
		})
	}

	ok, errMsg, data := hubCmd(t, m, buf, 40, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@Eval7StreamA @Eval7StreamB 同时说一句",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	roundID, _, targets := eval7RoundOf(t, data)
	if len(targets) != 2 {
		t.Fatalf("targets = %v，want 两个并行成员", targets)
	}
	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, want := range replies {
			found := false
			for _, msg := range msgs {
				if msg.Sender.Kind == agenthub.SenderAgent && msg.Text == want {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	})
	deadline := time.Now().Add(5 * time.Second)
	for (sessions[0].hubGroupBusy.Load() || sessions[1].hubGroupBusy.Load()) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	msgs = eval7ReadGroup(t, m, g.ID)

	// final 不重复：每个成员恰好一条，且都挂在本轮。
	finals := map[agenthub.AgentID]*agenthub.Message{}
	for i := range msgs {
		msg := &msgs[i]
		if msg.Sender.Kind != agenthub.SenderAgent {
			continue
		}
		if _, dup := finals[msg.Sender.AgentID]; dup {
			t.Fatalf("%s 写了多条 final: %+v", msg.Sender.AgentID, eval7AgentMessages(msgs, msg.Sender.AgentID))
		}
		finals[msg.Sender.AgentID] = msg
	}
	if len(finals) != len(agents) {
		t.Fatalf("final 成员数 = %d，want %d", len(finals), len(agents))
	}
	for _, a := range agents {
		final := finals[a.ID]
		if final == nil || final.RoundID != roundID {
			t.Fatalf("%s final = %+v，want round %q", a.ID, final, roundID)
		}
	}

	// 草稿：两个不同的 message_id（并行草稿不合并/不串），每个 id 的 delta 拼起来等于对应 final。
	deltas := map[string][]map[string]any{}
	for _, ev := range hubEvents(t, buf, "hub_message_delta") {
		id, _ := ev["message_id"].(string)
		if id == "" {
			t.Fatalf("delta 缺 message_id: %+v", ev)
		}
		deltas[id] = append(deltas[id], ev)
	}
	if len(deltas) != len(agents) {
		t.Fatalf("草稿流 = %d，want %d（并行 agent 各自一条草稿）: %v", len(deltas), len(agents), deltas)
	}
	hubMessages := hubEvents(t, buf, "hub_message")
	for _, a := range agents {
		final := finals[a.ID]
		evs := deltas[final.ID]
		if len(evs) == 0 {
			t.Fatalf("%s final id %s 没有对应 delta（草稿 ID 集合 = %v）", a.ID, final.ID, deltas)
		}
		var text strings.Builder
		for i, ev := range evs {
			if got := int(ev["index"].(float64)); got != i {
				t.Fatalf("%s delta index[%d] = %d，want 单调从 0 开始", a.ID, i, got)
			}
			if ev["agent_id"] != string(a.ID) {
				t.Fatalf("delta agent_id = %v，want %s（草稿串台）", ev["agent_id"], a.ID)
			}
			if ev["group_id"] != string(g.ID) {
				t.Fatalf("delta group_id = %v，want %s", ev["group_id"], g.ID)
			}
			text.WriteString(ev["delta"].(string))
		}
		if text.String() != final.Text {
			t.Fatalf("%s delta 拼接 = %q，final = %q", a.ID, text.String(), final.Text)
		}
		// final 事件恰好一次：前端按 message_id 替换草稿，重复事件会插两条。
		var events int
		for _, ev := range hubMessages {
			msg, _ := ev["message"].(map[string]any)
			if msg != nil && msg["id"] == final.ID {
				events++
			}
		}
		if events != 1 {
			t.Fatalf("%s final %s 的 hub_message 事件 = %d，want 1", a.ID, final.ID, events)
		}
	}
}
