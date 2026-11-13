package main

// P1-04：DM 会话生命周期（懒创建/唯一）+ hub_open/send/history + DM 索引与未读。

import (
	"context"
	"encoding/json"
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

// createHubAgent 直接经命令建一个可用智能体，返回 profile。
func createHubAgent(t *testing.T, m *manager, buf *syncBuf, id int, name string) agenthub.AgentProfile {
	t.Helper()
	ok, errMsg, data := hubCmd(t, m, buf, id, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"name": name, "provider": "deepseek", "model": "deepseek-v4-flash", "role": "测试角色",
		},
	})
	if !ok {
		t.Fatalf("create agent failed: %s", errMsg)
	}
	return agentFromData(t, data)
}

// DM 懒创建：hub_open 不建会话；hubDMSession 建且只有一个；再次 open 仍是同一个 sid。
func TestHubDMLazySingleSession(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Solo")
	ws := m.hubRuntime(string(p.ID))

	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_open", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": string(p.ID)},
	})
	if !ok {
		t.Fatalf("open failed: %s", errMsg)
	}
	wantSID := "hub." + string(p.ID) + ".dm"
	if data["session_id"] != wantSID {
		t.Fatalf("session_id = %v, want %s", data["session_id"], wantSID)
	}
	if data["exists"] == true {
		t.Fatal("hub_open must not create the session (懒创建)")
	}
	if ws.sessions[wantSID] != nil {
		t.Fatal("hub_open created a session")
	}

	bs1, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatalf("hubDMSession: %v", err)
	}
	if bs1.id != wantSID {
		t.Fatalf("session id = %q, want %q", bs1.id, wantSID)
	}
	bs2, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bs1 != bs2 {
		t.Fatal("hubDMSession must reuse the single DM session")
	}
	// profile 的 provider/model 必须落到会话上（不是全局 active）。
	if bs1.provider != "deepseek" || bs1.model != "deepseek-v4-flash" {
		t.Fatalf("session provider/model = %q/%q", bs1.provider, bs1.model)
	}
	if bs1.persona != "hub" {
		t.Fatalf("session persona = %q, want hub", bs1.persona)
	}

	ok, _, data = hubCmd(t, m, buf, 3, "hub_open", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": string(p.ID)},
	})
	if !ok {
		t.Fatal("second open failed")
	}
	if data["exists"] != true {
		t.Fatal("hub_open should report the existing session")
	}
}

// 发一条 DM：索引落账 → 会话跑一轮 → 最终回复进索引 → hub_open 能看到两条并推进已读。
func TestHubDMSendAndReplyIndexed(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Chatty")
	agentID := string(p.ID)

	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	// 用 mock provider 跑一轮（不碰网络）：一轮直接 stop 并给最终回复。
	mock := &bridgeMockProvider{rounds: []func(*provider.StreamRequest, func(provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "answer from agent"})
		},
	}}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(mock)))

	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_send", map[string]any{
		"target":        map[string]any{"kind": "dm", "agent_id": agentID},
		"text":          "hello",
		"client_msg_id": "c-1",
	})
	if !ok {
		t.Fatalf("send failed: %s", errMsg)
	}
	if data["seq"] != float64(1) {
		t.Fatalf("seq = %v, want 1", data["seq"])
	}

	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	// 等 AgentEnd → 回复进索引（异步 run）。
	deadline := time.Now().Add(5 * time.Second)
	var msgs []agenthub.Message
	for time.Now().Before(deadline) {
		msgs, err = st.ReadDirectMessages(context.Background(), p.ID, 0, 10)
		if err == nil && len(msgs) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(msgs) < 2 {
		t.Fatalf("dm index has %d messages, want 2 (human + agent reply): %+v", len(msgs), msgs)
	}
	if msgs[0].Sender.Kind != agenthub.SenderHuman || msgs[0].Text != "hello" {
		t.Fatalf("first message wrong: %+v", msgs[0])
	}
	if msgs[1].Sender.Kind != agenthub.SenderAgent || msgs[1].Text != "answer from agent" {
		t.Fatalf("agent reply not indexed: %+v", msgs[1])
	}
	if msgs[1].Sender.AgentID != p.ID {
		t.Fatalf("reply agent_id = %q, want %q", msgs[1].Sender.AgentID, p.ID)
	}
	// hub_message 事件也要发（前端回显/替换乐观气泡）。
	if len(hubEvents(t, buf, "hub_message")) < 2 {
		t.Fatalf("want hub_message events for both messages, got %d", len(hubEvents(t, buf, "hub_message")))
	}
	// open 后已读推进到最后一条。
	ok, _, data = hubCmd(t, m, buf, 3, "hub_open", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": agentID},
	})
	if !ok {
		t.Fatal("open failed")
	}
	cur, _ := data["cursor"].(map[string]any)
	if cur == nil || cur["human_read_seq"] != float64(2) {
		t.Fatalf("cursor not advanced: %v", data["cursor"])
	}
}

// client_msg_id 幂等：重发同一条不重复入上下文、不重复落索引。
func TestHubDMSendDedupesClientMsgID(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Dedup")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	mock := &bridgeMockProvider{rounds: []func(*provider.StreamRequest, func(provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
		},
	}}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(mock)))

	payload := map[string]any{
		"target":        map[string]any{"kind": "dm", "agent_id": agentID},
		"text":          "once",
		"client_msg_id": "same-id",
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_send", payload); !ok {
		t.Fatalf("first send failed: %s", errMsg)
	}
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_send", payload)
	if !ok {
		t.Fatalf("second send failed: %s", errMsg)
	}
	if data["deduplicated"] != true {
		t.Fatalf("duplicate client_msg_id must be deduplicated, got %v", data)
	}
	st, _ := m.hubStore()
	msgs, err := st.ReadDirectMessages(context.Background(), p.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	humans := 0
	for _, msg := range msgs {
		if msg.Sender.Kind == agenthub.SenderHuman {
			humans++
		}
	}
	if humans != 1 {
		t.Fatalf("human messages = %d, want 1 (dedupe failed)", humans)
	}
}

// 翻页：seq 游标（不用 offset），has_more 由多读一条判断。
func TestHubDMHistoryPaging(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Pager")
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		msg := &agenthub.Message{Kind: agenthub.KindText, Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: "m"}
		if _, err := st.AppendDirectMessage(context.Background(), p.ID, msg); err != nil {
			t.Fatal(err)
		}
	}
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_history", map[string]any{
		"agent_id": string(p.ID), "limit": 2,
	})
	if !ok {
		t.Fatalf("history failed: %s", errMsg)
	}
	raw, _ := json.Marshal(data["messages"])
	var msgs []agenthub.Message
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Seq != 4 || msgs[1].Seq != 5 {
		t.Fatalf("latest page = %+v, want seq 4,5", msgs)
	}
	if data["has_more"] != true {
		t.Fatalf("has_more = %v, want true", data["has_more"])
	}
	ok, _, data = hubCmd(t, m, buf, 3, "hub_history", map[string]any{
		"agent_id": string(p.ID), "before_seq": 4, "limit": 2,
	})
	if !ok {
		t.Fatal("history page 2 failed")
	}
	raw, _ = json.Marshal(data["messages"])
	msgs = nil
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Seq != 2 || msgs[1].Seq != 3 {
		t.Fatalf("older page = %+v, want seq 2,3", msgs)
	}
	// 翻历史不推进已读。
	st2, _ := m.hubStore()
	cur, _ := st2.LoadDMCursor(context.Background(), p.ID)
	if cur.HumanReadSeq != 0 {
		t.Fatalf("history must not advance read cursor, got %d", cur.HumanReadSeq)
	}
	if !strings.Contains(buf.String(), "\"hub_history\"") && !strings.Contains(buf.String(), "command_response") {
		t.Fatal("no response recorded")
	}
}

// 断线重连：会话从内存里消失后，同一个 agent 再打开/发送仍用同一个 sid，历史不丢。
func TestHubDMReconnectKeepsSessionIDAndHistory(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Reconnect")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	sid := bs.id
	st, _ := m.hubStore()
	msg := &agenthub.Message{Kind: agenthub.KindText, Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: "before reconnect"}
	if _, err := st.AppendDirectMessage(context.Background(), p.ID, msg); err != nil {
		t.Fatal(err)
	}
	// 模拟进程重启：丢掉内存里的会话实例。
	ws := m.hubRuntime(agentID)
	delete(ws.sessions, sid)

	bs2, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatalf("reconnect failed: %v", err)
	}
	if bs2.id != sid {
		t.Fatalf("reconnect sid = %q, want %q", bs2.id, sid)
	}
	ok, _, data := hubCmd(t, m, buf, 2, "hub_open", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": agentID},
	})
	if !ok {
		t.Fatal("open after reconnect failed")
	}
	raw, _ := json.Marshal(data["messages"])
	var msgs []agenthub.Message
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "before reconnect" {
		t.Fatalf("history lost across reconnect: %+v", msgs)
	}
}

// DM 没有「新会话/归档重开」，只有「清空上下文」（§6.4）：/clear 之后
// 会话仍是同一个（sid 不变），DM 索引与记忆保留 —— 否则左栏历史会凭空消失。
func TestHubDMClearKeepsSessionAndIndex(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Clearable")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	sid := bs.id
	st, _ := m.hubStore()
	msg := &agenthub.Message{Kind: agenthub.KindText, Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: "keep me"}
	if _, err := st.AppendDirectMessage(context.Background(), p.ID, msg); err != nil {
		t.Fatal(err)
	}

	buf.Reset()
	m.dispatch(command{Id: 9, Type: "command", Payload: map[string]any{
		"workspace": bs.workspace, "session_id": sid, "line": "/clear",
	}})
	var clearResp map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) == nil && msg["event_type"] == "command_response" && msg["id"] == float64(9) {
			clearResp = msg
		}
	}
	if clearResp == nil {
		t.Fatal("/clear 没有 command_response")
	}
	if clearResp["ok"] != true {
		t.Fatalf("/clear failed: %v", clearResp["error"])
	}

	again, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatalf("session lost after /clear: %v", err)
	}
	if again.id != sid {
		t.Fatalf("sid changed after /clear: %q -> %q", sid, again.id)
	}
	msgs, err := st.ReadDirectMessages(context.Background(), p.ID, 0, 10)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "keep me" {
		t.Fatalf("DM index lost after /clear: %+v, %v", msgs, err)
	}
}

// 左栏预览/未读：list 里的 dm 摘要来自 DM 索引，不读 transcript。
func TestHubAgentListCarriesDMPreviewAndUnread(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Preview")
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("长文", 60) // 120 个字符 → 预览必须按字符截断
	for _, text := range []string{"第一条", long} {
		msg := &agenthub.Message{Kind: agenthub.KindText, Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: text}
		if _, err := st.AppendDirectMessage(context.Background(), p.ID, msg); err != nil {
			t.Fatal(err)
		}
	}
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_agent_list", map[string]any{})
	if !ok {
		t.Fatalf("list failed: %s", errMsg)
	}
	dm, _ := data["dm"].(map[string]any)
	entry, _ := dm[string(p.ID)].(map[string]any)
	if entry == nil {
		t.Fatalf("list response missing dm preview: %v", data)
	}
	if entry["unread"] != float64(2) {
		t.Fatalf("unread = %v, want 2", entry["unread"])
	}
	if entry["last_seq"] != float64(2) {
		t.Fatalf("last_seq = %v, want 2", entry["last_seq"])
	}
	preview, _ := entry["last_text"].(string)
	if !strings.HasPrefix(preview, "长文") || strings.HasSuffix(preview, "文长") {
		t.Fatalf("preview cut wrong: %q", preview)
	}
	if n := len([]rune(preview)); n > 81 { // 80 + 省略号
		t.Fatalf("preview too long: %d runes", n)
	}
}

// 子 agent 的 AgentEnd 不能进 DM 索引（P1-04 复核 blocking 1）。
func TestHubDMSubagentAgentEndNotIndexed(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Parent")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	h := bs.handler()
	h(context.Background(), &events.AgentEnd{Content: "sub internal answer", ParentRunId: "run-child", Depth: 1})
	st, _ := m.hubStore()
	msgs, err := st.ReadDirectMessages(context.Background(), p.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("subagent AgentEnd leaked into DM index: %+v", msgs)
	}
	// 根 run 的收尾照常落索引（别把过滤写成「全都不落」）。
	h(context.Background(), &events.AgentEnd{Content: "root answer"})
	msgs, _ = st.ReadDirectMessages(context.Background(), p.ID, 0, 10)
	if len(msgs) != 1 || msgs[0].Text != "root answer" {
		t.Fatalf("root AgentEnd must be indexed: %+v", msgs)
	}
}

// 并发装配只能产出**一个** DM 会话（P1-04 复核 blocking 2）。
func TestHubDMConcurrentAssemblySingleSession(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Concurrent")
	agentID := string(p.ID)

	const n = 8
	results := make([]*bridgeSession, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = m.hubDMSession(agentID)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("hubDMSession #%d: %v", i, err)
		}
	}
	first := results[0]
	if first == nil {
		t.Fatal("nil session")
	}
	for i, bs := range results {
		if bs != first {
			t.Fatalf("session #%d differs: assembled %d instances (want 1)", i, distinctSessions(results))
		}
	}
	ws := m.hubRuntime(agentID)
	ws.mu.Lock()
	count := len(ws.sessions)
	ws.mu.Unlock()
	if count != 1 {
		t.Fatalf("ws.sessions entries = %d, want 1", count)
	}
}

func distinctSessions(list []*bridgeSession) int {
	seen := map[*bridgeSession]struct{}{}
	for _, bs := range list {
		if bs != nil {
			seen[bs] = struct{}{}
		}
	}
	return len(seen)
}

// client_msg_id 幂等不受「最近 N 条」窗口限制（P1-04 复核 non-blocking 1）。
func TestHubDMDedupeOutsideWindow(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Window")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	mock := &bridgeMockProvider{rounds: []func(*provider.StreamRequest, func(provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
		},
	}}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(mock)))

	payload := map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": agentID}, "text": "old", "client_msg_id": "c-old",
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_send", payload); !ok {
		t.Fatalf("first send failed: %s", errMsg)
	}
	st, _ := m.hubStore()
	for i := 0; i < 150; i++ {
		msg := &agenthub.Message{Kind: agenthub.KindText, Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: "filler"}
		if _, err := st.AppendDirectMessage(context.Background(), p.ID, msg); err != nil {
			t.Fatal(err)
		}
	}
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_send", payload)
	if !ok {
		t.Fatalf("second send failed: %s", errMsg)
	}
	if data["deduplicated"] != true {
		t.Fatalf("dedupe must not be window-limited, got %v", data)
	}
}

// hub home 不是工作区：interrupt 兜底 / delete_session 都不能造项目 runtime 或删 DM。
func TestHubHomeNotTreatedAsWorkspace(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Home")
	agentID := string(p.ID)
	home := m.hubRuntime(agentID).path
	if !isHubHomePath(home) {
		t.Fatalf("isHubHomePath(%q) = false", home)
	}
	if n := m.interruptAllIn(home); n != 0 {
		t.Fatalf("interruptAllIn = %d, want 0", n)
	}
	m.mu.Lock()
	_, phantom := m.workspaces[home]
	m.mu.Unlock()
	if phantom {
		t.Fatal("interruptAllIn created a project runtime for hub home")
	}
	buf.Reset()
	m.dispatch(command{Id: 9, Type: CmdDeleteSession, Payload: map[string]any{
		"workspace": home, "session_id": "hub." + agentID + ".dm",
	}})
	if line := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "command_response" && mm["id"] == float64(9)
	}); line == nil || line["ok"] == true {
		t.Fatalf("delete_session on hub home must fail, got %v", line)
	}
}

// 未装配 hub runtime 时，按 workspace+session_id 寻址的命令（blocks_page/compact）不能
// 因为 m.get 落到项目分支而造出幻影 runtime —— hub home 从来不是工作区（§9.1）。
func TestHubHomeCommandDoesNotCreateProjectRuntime(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 2, "Phantom")
	agentID := string(p.ID)
	home := hubAgentHome(agentID)
	if m.hubRuntimeIfExists(agentID) != nil {
		t.Fatal("runtime assembled before the command: test needs an unassembled home")
	}
	buf.Reset()
	m.dispatch(command{Id: 11, Type: CmdBlocksPage, Payload: map[string]any{
		"workspace": home, "session_id": "hub." + agentID + ".dm",
	}})
	line := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "command_response" && mm["id"] == float64(11)
	})
	if line == nil {
		t.Fatalf("no command_response for blocks_page: %s", buf.String())
	}
	if line["ok"] == true {
		t.Fatalf("blocks_page on an unassembled hub home must fail, got %v", line)
	}
	m.mu.Lock()
	_, phantom := m.workspaces[home]
	m.mu.Unlock()
	if phantom {
		t.Fatal("blocks_page created a project runtime for the hub home")
	}
	if m.hubRuntimeIfExists(agentID) != nil {
		t.Fatal("blocks_page must not assemble a hub runtime either")
	}
}

// 场景 1-2：DM 与群的消息内容不互串 ——
//  1. DM 里发的正文/附件不进群日志，也不进群唤醒快照（模型在群里读不到）；
//  2. 反向群消息不进 DM 索引，也不进 DM 会话的模型上下文。
//
// 两组都是「正对照 + 负对照」：先证明各自那条消息确实进了自己的上下文，
// 否则「对方没出现」也可能只是「什么都没读到」。
func TestHubDMAndGroupContentStaysInItsOwnSession(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "LeakA")
	g := groupWithMembers(t, m, buf, 2, "不串话群", memberRef{a.ID, "owner"})
	agentID := string(a.ID)

	const dmOnly = "DM-ONLY-私聊专属正文-x9"
	const groupOnly = "GROUP-ONLY-群专属正文-y7"
	imgURL := testImageDataURL("dm-only-image")

	// —— DM 发一条「独特正文 + 独特图片附件」 ——
	dm, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	dmProv := &evalProvider{reply: "DM 第一条回执"}
	setDMFakeLoop(t, m, dm, agentID, dmProv)
	if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": agentID},
		"text":   dmOnly,
		"attachments": []any{map[string]any{
			"kind": "image", "name": "dm-only.png", "mime_type": "image/png",
			"bytes": float64(9), "data_url": imgURL,
		}},
	}); !ok {
		t.Fatalf("dm send: %s", errMsg)
	}
	waitHubIndex(t, m, a.ID, 2)
	// 正对照：DM 自己的模型上下文里确实有这条正文与附件。
	if prompt := isolationPrompt(dmProv.lastRequest()); !strings.Contains(prompt, dmOnly) || !strings.Contains(prompt, imgURL) {
		t.Fatalf("DM 会话没拿到自己的正文/附件（正对照失败）:\n%s", prompt)
	}

	// —— 群里 @ 同一智能体 ——
	grp, err := m.hubGroupSession(agentID, string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	grpProv := &evalProvider{reply: "群回执"}
	setGroupFakeLoop(t, m, grp, g, agentID, grpProv)
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": groupOnly, "mentions": []string{agentID},
	}); !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, msg := range msgs {
			if msg.Sender.Kind == agenthub.SenderAgent && strings.Contains(msg.Text, "群回执") {
				return true
			}
		}
		return false
	})
	grpPrompt := isolationPrompt(grpProv.lastRequest())
	if !strings.Contains(grpPrompt, groupOnly) {
		t.Fatalf("群会话没拿到本轮触发消息（正对照失败）:\n%s", grpPrompt)
	}
	// 负对照：DM 的正文/附件一个字节都不该出现在群的模型上下文里（含 hidden 快照块）。
	if strings.Contains(grpPrompt, dmOnly) {
		t.Fatalf("DM 正文漏进了群会话上下文:\n%s", grpPrompt)
	}
	if strings.Contains(grpPrompt, imgURL) || strings.Contains(grpPrompt, "dm-only.png") {
		t.Fatalf("DM 附件漏进了群会话上下文:\n%s", grpPrompt)
	}

	// 群快照本身（wake 的 hidden 块来源）也只能由群日志构造。
	fst, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	grpMsgs, err := fst.ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range grpMsgs {
		if strings.Contains(msg.Text, dmOnly) {
			t.Fatalf("DM 正文落进了群日志: %+v", msg)
		}
	}
	snap := agenthub.BuildGroupSnapshot(g, a.ID, grpMsgs, 0,
		agenthub.Round{GroupID: g.ID, ID: "rnd_isolation"}, agenthub.SnapshotOptions{})
	if !strings.Contains(snap.Text, groupOnly) {
		t.Fatalf("群快照缺本轮群消息（正对照失败）:\n%s", snap.Text)
	}
	if strings.Contains(snap.Text, dmOnly) {
		t.Fatalf("DM 正文出现在群快照里:\n%s", snap.Text)
	}

	// —— 反向：群消息不进 DM 索引 / DM 上下文 ——
	dmProv2 := &evalProvider{reply: "DM 第二条回执"}
	setDMFakeLoop(t, m, dm, agentID, dmProv2)
	if ok, errMsg, _ := hubCmd(t, m, buf, 5, "hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": agentID},
		"text":   "DM-ONLY-第二次提问",
	}); !ok {
		t.Fatalf("dm send #2: %s", errMsg)
	}
	waitHubIndex(t, m, a.ID, 4)
	dmPrompt := isolationPrompt(dmProv2.lastRequest())
	if !strings.Contains(dmPrompt, "DM-ONLY-第二次提问") {
		t.Fatalf("DM 第二轮没拿到自己的新消息（正对照失败）:\n%s", dmPrompt)
	}
	if strings.Contains(dmPrompt, groupOnly) {
		t.Fatalf("群正文漏进了 DM 会话上下文:\n%s", dmPrompt)
	}
	dmMsgs, err := fst.ReadDirectMessages(context.Background(), a.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(dmMsgs) != 4 {
		t.Fatalf("DM 索引条数 = %d, want 4（人/回执 × 2）", len(dmMsgs))
	}
	for _, msg := range dmMsgs {
		if strings.Contains(msg.Text, groupOnly) {
			t.Fatalf("群正文出现在 DM 索引（agenthub DM 索引串话）: %+v", msg)
		}
	}
}
