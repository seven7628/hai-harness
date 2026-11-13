package main

// P1-10：评测 1–3（方案 §10.3）。
//
//  1. DM 单会话：多轮不新建会话、到阈值触发压缩、压缩后仍记得记忆里的关键事实；
//  2. 独立记忆：A 记住的事实 B 看不到；
//  3. 独立 provider：两个智能体各用自己的 provider/model，事件与请求归属正确。
//
// 用确定性 fake provider（不碰网络）：请求按顺序记录、压缩请求回摘要、普通轮回固定正文。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

type evalProvider struct {
	mu    sync.Mutex
	reply string
	calls []provider.StreamRequest
}

func (p *evalProvider) Stream(_ context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	cp := *req
	p.mu.Lock()
	p.calls = append(p.calls, cp)
	p.mu.Unlock()
	system := strings.ToLower(systemTextOf(req))
	if strings.Contains(system, "summar") || strings.Contains(system, "压缩") {
		return h(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "SUMMARY: 前文要点", Model: req.Model})
	}
	return h(provider.LLMEndEvent{
		FinishReason: core.FinishReasonStop,
		Content:      p.reply,
		Model:        req.Model,
		Usage:        core.Usage{Input: 12, Output: 8},
	})
}

func (p *evalProvider) lastRequest() *provider.StreamRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		return nil
	}
	cp := p.calls[len(p.calls)-1]
	return &cp
}

func (p *evalProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// waitHubIndex 等 DM 索引条数到 want（异步 run 收尾）。
func waitHubIndex(t *testing.T, m *manager, id agenthub.AgentID, want int) []agenthub.Message {
	t.Helper()
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	var msgs []agenthub.Message
	for time.Now().Before(deadline) {
		msgs, err = st.ReadDirectMessages(context.Background(), id, 0, 500)
		if err == nil && len(msgs) >= want {
			return msgs
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("dm index has %d messages, want %d", len(msgs), want)
	return nil
}

// 评测 1：连续多轮只有一个会话；到线触发自动压缩；压缩后系统提示词里仍有记忆事实。
func TestEval1DMSingleSessionCompressKeepsMemory(t *testing.T) {
	m, buf := testManager(t)
	// 阈值必须来自 Profile（生产装配路径）：测试**不覆盖** WithCompressThreshold，
	// 否则把 hub_session.go 的阈值改成 0.99 评测仍会 PASS（P1-10 评审 B1）。
	okCreate, errCreate, dataCreate := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"name": "EvalOne", "provider": "deepseek", "model": "deepseek-v4-flash",
			"role": "测试角色", "compress_threshold": 0.5,
		},
	})
	if !okCreate {
		t.Fatalf("create agent failed: %s", errCreate)
	}
	p := agentFromData(t, dataCreate)
	agentID := string(p.ID)
	root := filepath.Join(appDataDir(), "hub")
	const fact = "关键事实：本月预算 20 万"
	if _, err := agenthub.WriteMemoryFile(root, p.ID, "MEMORY.md", "- ["+fact+"](fact.md) — "+fact+"\n"); err != nil {
		t.Fatal(err)
	}
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	loaded, _, err := m.hubLoadAgent(agentID)
	if err != nil {
		t.Fatal(err)
	}
	mock := &evalProvider{reply: strings.Repeat("这是一段用于堆上下文的回复。", 12)} // ~144 字
	opts := m.hubLoopOptions(m.hubRuntime(agentID), bs.id, m.hubSpecFor(loaded, agenthub.Scope{Kind: agenthub.ScopeDM}, m.hubRuntime(agentID).path), bs.hubEngine)
	opts = append(opts,
		agents.WithProvider(mock),
		agents.WithModel("eval-model"),
		agents.WithContextWindow(3000),
		agents.WithCompressor(agents.NewLLMCompressor(mock, "eval-model")),
		agents.WithMaxIterations(1),
	)
	bs.s.SetLoop(agents.NewAgentLoop(opts...))
	if got := bs.loop.CompressThreshold(); got != 0.5 {
		t.Fatalf("loop compress threshold = %v, want profile value 0.5（生产装配没读 Profile 阈值）", got)
	}

	const turns = 50
	sidSeen := map[string]bool{}
	for i := 1; i <= turns; i++ {
		ok, errMsg, data := hubCmd(t, m, buf, 100+i, "hub_send", map[string]any{
			"target":        map[string]any{"kind": "dm", "agent_id": agentID},
			"text":          "turn-" + strconv.Itoa(i) + "-marker-" + strconv.Itoa(i*7919),
			"client_msg_id": "eval-1-" + strconv.Itoa(i),
		})
		if !ok {
			t.Fatalf("turn %d send failed: %s", i, errMsg)
		}
		sidSeen[string(str(data, "session_id"))] = true
		waitHubIndex(t, m, p.ID, 2*i)
	}
	// ① 只有一个会话：确定性 sid，且 ws.sessions 里只有它。
	if len(sidSeen) != 1 || !sidSeen["hub."+agentID+".dm"] {
		t.Fatalf("session ids seen = %v, want only hub.<id>.dm", sidSeen)
	}
	ws := m.hubRuntime(agentID)
	ws.mu.Lock()
	sessionCount := len(ws.sessions)
	ws.mu.Unlock()
	if sessionCount != 1 {
		t.Fatalf("ws.sessions = %d, want 1（DM 不该新建会话）", sessionCount)
	}
	again, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if again != bs {
		t.Fatal("hubDMSession 返回了另一个会话对象：DM 单例语义不成立")
	}
	// ② 触发过自动压缩（事件 + fake provider 收到过摘要请求）。
	if got := len(hubEvents(t, buf, "compress_start")); got == 0 {
		t.Fatalf("no compress_start events in %d turns (threshold 0.5 × 3000)", turns)
	}
	var sawSummaryRequest bool
	for _, req := range mock.calls {
		if strings.Contains(strings.ToLower(systemTextOf(&req)), "summar") {
			sawSummaryRequest = true
			break
		}
	}
	if !sawSummaryRequest {
		t.Fatal("compressor never called the model: 自动压缩没有发生")
	}
	// ③ 压缩后仍记得记忆里的关键事实：最后一次请求的 system 里必须有它。
	last := mock.lastRequest()
	if last == nil {
		t.Fatal("no request captured")
	}
	if !strings.Contains(systemTextOf(last), fact) {
		t.Fatalf("compressed context lost memory fact %q", fact)
	}
	// ④ 压缩确实缩短了上下文：最后一个请求的消息里能看到摘要，且不再包含第一轮原文。
	var joined strings.Builder
	for _, msg := range last.Messages {
		for _, c := range msg.Content {
			joined.WriteString(c.Content)
		}
	}
	text := joined.String()
	if !strings.Contains(text, "SUMMARY") {
		t.Fatalf("compressed request carries no summary: %q", text[:min(200, len(text))])
	}
	// 首条真实用户输入是**任务锚点**，压缩会逐字保留（S1-A），所以不能拿它当「旧轮次」判据；
	// 中间轮次应被摘要替换掉，且上下文条数明显少于 2×轮数。
	if strings.Contains(text, "turn-5-marker-39595") {
		t.Fatal("middle turn still in context after compression（只留了锚点+近窗才对）")
	}
	if len(last.Messages) >= 2*turns {
		t.Fatalf("last request carries %d messages (>= 2×%d): 压缩没有生效", len(last.Messages), turns)
	}
}

// 评测 2：独立记忆 —— A 写下的事实 B 看不到（物理目录也不同）。
func TestEval2IndependentMemory(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "EvalA")
	b := createHubAgent(t, m, buf, 2, "EvalB")
	root := filepath.Join(appDataDir(), "hub")
	const factA = "关键事实：预算 20 万"
	const factB = "关键事实：预算 50 万"
	if _, err := agenthub.WriteMemoryFile(root, a.ID, "MEMORY.md", "- ["+factA+"](fact-a.md) — "+factA+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := agenthub.WriteMemoryFile(root, b.ID, "MEMORY.md", "- ["+factB+"](fact-b.md) — "+factB+"\n"); err != nil {
		t.Fatal(err)
	}
	aLoop, _, err := m.hubLoadAgent(string(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	bLoop, _, err := m.hubLoadAgent(string(b.ID))
	if err != nil {
		t.Fatal(err)
	}
	aReq := hubLoopFor(t, m, m.hubRuntime(string(a.ID)), m.hubSpecFor(aLoop, agenthub.Scope{Kind: agenthub.ScopeDM}, m.hubRuntime(string(a.ID)).path))
	bReq := hubLoopFor(t, m, m.hubRuntime(string(b.ID)), m.hubSpecFor(bLoop, agenthub.Scope{Kind: agenthub.ScopeDM}, m.hubRuntime(string(b.ID)).path))
	aSys, bSys := systemTextOf(aReq), systemTextOf(bReq)
	// 双向断言：A 只见 A 的事实、B 只见 B 的事实。只测「B 没看到 A」会把
	// 「B 的记忆功能整体坏掉」误判成隔离成功（P1-10 评审 B2）。
	if !strings.Contains(aSys, factA) {
		t.Fatal("A's system prompt missing its own memory fact")
	}
	if strings.Contains(aSys, factB) {
		t.Fatal("A saw B's memory fact (记忆串台)")
	}
	if !strings.Contains(bSys, factB) {
		t.Fatal("B's system prompt missing its own memory fact（B 的记忆没有生效，不是隔离）")
	}
	if strings.Contains(bSys, factA) {
		t.Fatal("B saw A's memory fact (记忆串台)")
	}
	// 物理目录分离 + 各自目录里确实有文件（不是「两边都空」）。
	aDir, _ := agenthub.MemoryAgentDir(root, a.ID)
	bDir, _ := agenthub.MemoryAgentDir(root, b.ID)
	if aDir == bDir {
		t.Fatalf("agents share memory dir %q", aDir)
	}
	for _, pair := range []struct {
		name string
		id   agenthub.AgentID
	}{{"A", a.ID}, {"B", b.ID}} {
		ents, err := agenthub.ListMemory(root, pair.id)
		if err != nil || len(ents) == 0 {
			t.Fatalf("%s memory entries = %v, err = %v; want its own MEMORY.md", pair.name, ents, err)
		}
	}
}

// 评测 3：独立 provider/model —— 两个智能体各自装配，请求 Model 与事件归属都不串。
func TestEval3IndependentProviders(t *testing.T) {
	m, buf := testManager(t)
	// bs.metrics 在会话创建时从 m.metrics 快照，必须先于 hubDMSession 设置。
	m.metrics = NewFileMetricsStore(filepath.Join(appDataDir(), "metrics.jsonl"))
	a := createHubAgent(t, m, buf, 1, "EvalDeepSeek")
	// B 用另一个 provider/model（配置里存在即可；请求由 fake provider 承接，不联网）。
	ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"name": "EvalOpenAI", "provider": "openai", "model": "gpt-5", "role": "审查",
		},
	})
	if !ok {
		t.Fatalf("create B failed: %s", errMsg)
	}
	_, _, data := hubCmd(t, m, buf, 3, "hub_agent_list", map[string]any{})
	agentsList, _ := data["agents"].([]any)
	if len(agentsList) != 2 {
		t.Fatalf("agents = %d, want 2", len(agentsList))
	}
	bsA, err := m.hubDMSession(string(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	var bID string
	for _, raw := range agentsList {
		row, _ := raw.(map[string]any)
		if row["name"] == "EvalOpenAI" {
			bID = str(row, "id")
		}
	}
	if bID == "" {
		t.Fatal("B not found in list")
	}
	bsB, err := m.hubDMSession(bID)
	if err != nil {
		t.Fatal(err)
	}
	if bsA.loop.Model() == bsB.loop.Model() {
		t.Fatalf("both sessions use model %q; provider/model 没有独立装配", bsA.loop.Model())
	}
	mockA := &evalProvider{reply: "A 的回答"}
	mockB := &evalProvider{reply: "B 的回答"}
	loadedA, _, err := m.hubLoadAgent(string(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	optsA := m.hubLoopOptions(m.hubRuntime(string(a.ID)), bsA.id, m.hubSpecFor(loadedA, agenthub.Scope{Kind: agenthub.ScopeDM}, m.hubRuntime(string(a.ID)).path), bsA.hubEngine)
	optsA = append(optsA, agents.WithProvider(mockA), agents.WithMaxIterations(1))
	bsA.s.SetLoop(agents.NewAgentLoop(optsA...))
	loadedB, _, err := m.hubLoadAgent(bID)
	if err != nil {
		t.Fatal(err)
	}
	optsB := m.hubLoopOptions(m.hubRuntime(bID), bsB.id, m.hubSpecFor(loadedB, agenthub.Scope{Kind: agenthub.ScopeDM}, m.hubRuntime(bID).path), bsB.hubEngine)
	optsB = append(optsB, agents.WithProvider(mockB), agents.WithMaxIterations(1))
	bsB.s.SetLoop(agents.NewAgentLoop(optsB...))

	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": string(a.ID)}, "text": "hi A", "client_msg_id": "e3-a",
	}); !ok {
		t.Fatalf("send A failed: %s", errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 5, "hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": bID}, "text": "hi B", "client_msg_id": "e3-b",
	}); !ok {
		t.Fatalf("send B failed: %s", errMsg)
	}
	waitHubIndex(t, m, a.ID, 2)
	waitHubIndex(t, m, agenthub.AgentID(bID), 2)

	if req := mockA.lastRequest(); req == nil || req.Model != a.Model || req.Provider != "deepseek" {
		t.Fatalf("A request = %+v, want model %q provider deepseek", req, a.Model)
	}
	if req := mockB.lastRequest(); req == nil || req.Model != "gpt-5" || req.Provider != "openai" {
		t.Fatalf("B request = %+v, want model gpt-5 provider openai", req)
	}
	// 事件归属：llm_end 条数与正/反向归属都要断言（事件注入 session_id 是恒真的，
	// 只看「sid→model」会把「两边都跑同一个 provider」漏掉 —— P1-10 评审 B3）。
	ends := hubEvents(t, buf, "llm_end")
	if len(ends) != 2 {
		t.Fatalf("llm_end count = %d, want 2（events: %v）", len(ends), ends)
	}
	sidFor := map[string]string{}
	for _, raw := range ends {
		sidFor[str(raw, "session_id")] = str(raw, "model")
	}
	if got := sidFor["hub."+string(a.ID)+".dm"]; got != a.Model {
		t.Fatalf("A llm_end model = %q, want %q (events: %v)", got, a.Model, sidFor)
	}
	if got := sidFor["hub."+bID+".dm"]; got != "gpt-5" {
		t.Fatalf("B llm_end model = %q, want gpt-5 (events: %v)", got, sidFor)
	}
	if sidFor["hub."+string(a.ID)+".dm"] == sidFor["hub."+bID+".dm"] {
		t.Fatalf("both sessions reported the same model: %v", sidFor)
	}
	// metrics：两次调用各一条，provider/model/usage 都归属正确（成本按 provider 记账）。
	raw, err := os.ReadFile(filepath.Join(appDataDir(), "metrics.jsonl"))
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	bySID := map[string]MetricsEntry{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var e MetricsEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		bySID[e.SessionID] = e
	}
	aEntry, okA := bySID["hub."+string(a.ID)+".dm"]
	bEntry, okB := bySID["hub."+bID+".dm"]
	if !okA || !okB {
		t.Fatalf("metrics missing session entries: %+v", bySID)
	}
	if aEntry.Provider != "deepseek" || aEntry.Model != a.Model || aEntry.Usage.Input != 12 || aEntry.Usage.Output != 8 {
		t.Fatalf("A metrics = %+v", aEntry)
	}
	if bEntry.Provider != "openai" || bEntry.Model != "gpt-5" || bEntry.Usage.Input != 12 || bEntry.Usage.Output != 8 {
		t.Fatalf("B metrics = %+v", bEntry)
	}
}
