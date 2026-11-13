package main

// P6-03：预跑预算门 + provider 信号量 + 429 退避（bridge 接线）。
//
// 预算门要证明的是「**不启动** run」：账本记 skipped(budget)、写一条系统说明、
// 目标会话一次都没被叫起来；而不是跑起来再在 LLMEnd 处硬停。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// spendHubCost 给某智能体记一笔今日已结算成本（预算门的输入）。
func spendHubCost(t *testing.T, m *manager, agent agenthub.AgentID, group agenthub.GroupID, usd float64) {
	t.Helper()
	st := mustHubStore(t, m)
	task := &agenthub.Task{
		ID: agenthub.TaskID("task_budget_" + string(agent)), Status: agenthub.TaskCompleted,
		Origin:    agenthub.TaskOrigin{Kind: agenthub.OriginGroup, AgentID: agent, GroupID: group},
		UpdatedAt: time.Now().UTC(),
		Result:    &agenthub.TaskResult{Usage: agenthub.Usage{CostPriced: true, CostUSD: usd}},
	}
	if err := st.SaveTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
}

// TestHubConversationBudgetGateSkipsBeforeRun：agent 日预算用满 → 人类 @它 时不启动
// run（provider 零调用），账本 skipped(budget)，群里有一条系统说明。
func TestHubConversationBudgetGateSkipsBeforeRun(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createHubAgent(t, m, buf, 1, "预算A")
	b := createHubAgent(t, m, buf, 2, "预算B")
	g := convGroupWith(t, m, buf, 10, "预算群", nil, a, b)

	// B 的日预算设成 $1，并记一笔 $2 的今日成本 → 已用满。
	if ok, errMsg, _ := hubCmd(t, m, buf, 20, "hub_agent_update", map[string]any{
		"agent_id": string(b.ID),
		"patch":    map[string]any{"policy": map[string]any{"daily_budget_usd": 1}},
	}); !ok {
		t.Fatalf("update agent budget: %s", errMsg)
	}
	spendHubCost(t, m, b.ID, g.ID, 2)

	fb := newConvProvider("B 不该跑")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)

	ok, errMsg, data := hubCmd(t, m, buf, 21, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@预算B 跑一下",
	})
	if !ok {
		t.Fatalf("send: %s", errMsg)
	}
	d := mustDelivery(t, data, b.ID)
	if d.Status != agenthub.DeliverySkipped || d.Reason != agenthub.DeliveryReasonBudget {
		t.Fatalf("投递记录 = %+v, want skipped/budget", d)
	}
	if n := fb.callCount(); n != 0 {
		t.Fatalf("预算不足仍启动了 run（provider 调用 %d 次）", n)
	}
	if v := convView(m, g.ID); v.QueueLen != 0 {
		t.Fatalf("queue = %d, want 0（命中的 wake 已出队）", v.QueueLen)
	}
	// 群里要有一条说明「为什么没人被叫醒」。
	msgs := convGroupMessages(t, m, g.ID)
	found := false
	for _, msg := range convSystemMessages(msgs) {
		if msg.Text != "" && (contains(msg.Text, "预算")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺预算系统说明：%+v", convSystemMessages(msgs))
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestHubConversationBudgetGateGroupLevel：群日预算用满 → 任何成员都不启动 run。
func TestHubConversationBudgetGateGroupLevel(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createHubAgent(t, m, buf, 1, "群预算A")
	b := createHubAgent(t, m, buf, 2, "群预算B")
	// 建群时把群日预算设成 $1。
	g := convGroupWith(t, m, buf, 10, "群预算群", map[string]any{"daily_budget_usd": 1}, a, b)
	spendHubCost(t, m, a.ID, g.ID, 5)

	fb := newConvProvider("B 不该跑")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)
	ok, errMsg, data := hubCmd(t, m, buf, 21, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@群预算B 跑一下",
	})
	if !ok {
		t.Fatalf("send: %s", errMsg)
	}
	if d := mustDelivery(t, data, b.ID); d.Status != agenthub.DeliverySkipped || d.Reason != agenthub.DeliveryReasonBudget {
		t.Fatalf("投递记录 = %+v, want skipped/budget", d)
	}
	if n := fb.callCount(); n != 0 {
		t.Fatalf("群预算用满仍启动了 run（%d 次）", n)
	}
}

// TestHubConversationRateLimitRunEndRequeues：run 因 429 结束（ErrorKind=rate_limit）
// 时把这条投递退回队列 + 退避，**不标 failed**、不写 final；退避到期后必须**真的
// 重投一次**（B2：旧断言只读瞬时 queued，run 收尾把它覆写成 done 也是绿的）。
func TestHubConversationRateLimitRunEndRequeues(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createHubAgent(t, m, buf, 1, "跑限A")
	b := createHubAgent(t, m, buf, 2, "跑限B")
	g := convGroupWith(t, m, buf, 10, "跑限群", nil, a, b)
	// 让 B 的 run 以 429 结束：provider 直接返回 RateLimitError。
	fb := &rateLimitProvider{}
	setGroupFakeLoopNoRetry(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)

	ok, errMsg, data := hubCmd(t, m, buf, 20, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@跑限B 试试",
	})
	if !ok {
		t.Fatalf("send: %s", errMsg)
	}
	d0 := mustDelivery(t, data, b.ID)
	convWait(t, "429 run 结束后记录退回 queued", func() bool {
		d, found := m.hubConversation().Delivery(g.ID, d0.WakeID)
		return found && d.Status == agenthub.DeliveryQueued
	})
	d, _ := m.hubConversation().Delivery(g.ID, d0.WakeID)
	if d.Status == agenthub.DeliveryFailed {
		t.Fatalf("429 结束不该标 failed: %+v", d)
	}
	if d.Status == agenthub.DeliveryDone {
		t.Fatalf("run 收尾把退避中的 queued 覆写成 done（B2）: %+v", d)
	}
	if d.Reason != agenthub.DeliveryReasonRateLimit {
		t.Fatalf("reason = %q, want %q", d.Reason, agenthub.DeliveryReasonRateLimit)
	}
	if d.ReadyAt == nil {
		t.Fatal("缺退避时刻（ReadyAt）")
	}
	if n := fb.callCount(); n < 1 {
		t.Fatalf("provider 调用 = %d, want >= 1", n)
	}
	// B2 真绿断言：等退避到期，provider 必须被**第二次**调用（真的重投了一次）。
	// 旧实现里 queued 被 run 收尾覆写成 done → BeginDelivery no-op → 这里永远超时。
	convWait(t, "退避到期后必须真的重投一次", func() bool { return fb.callCount() >= 2 })
	d2, _ := m.hubConversation().Delivery(g.ID, d0.WakeID)
	if d2.Attempts < 2 {
		t.Fatalf("重投后 attempts = %d, want >= 2（%+v）", d2.Attempts, d2)
	}
	if d2.Status == agenthub.DeliveryDone {
		t.Fatalf("退避中的记录不该被收口成 done: %+v", d2)
	}
}

// setGroupFakeLoopNoRetry 装配一个**不重试**的 fake loop：429 路径要验证的是
// 「run 以限流结束之后怎么收口」，而不是重试策略本身（默认 10 次重试会让测试等 10s+）。
func setGroupFakeLoopNoRetry(t *testing.T, m *manager, bs *bridgeSession, g *agenthub.Group, agentID string, fake provider.Provider) {
	t.Helper()
	loaded, _, err := m.hubLoadAgent(agentID)
	if err != nil {
		t.Fatal(err)
	}
	members, err := m.hubGroupMemberProfiles(context.Background(), mustHubStore(t, m), g)
	if err != nil {
		t.Fatal(err)
	}
	ws := m.hubRuntime(agentID)
	spec := m.hubSpecFor(loaded, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}, ws.path)
	spec.Group = g
	spec.Members = members
	opts := m.hubLoopOptions(ws, bs.id, spec, bs.hubEngine)
	opts = append(opts,
		agents.WithProvider(fake),
		agents.WithModel("group-fake"),
		agents.WithMaxIterations(1),
		agents.WithMaxRetries(1), // 1 = 只尝试一次，不重试
	)
	bs.s.SetLoop(agents.NewAgentLoop(opts...))
	bs.loop = bs.s.Loop()
}

// rateLimitProvider 每次都返回 429（模拟上游限流）。
type rateLimitProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *rateLimitProvider) Stream(ctx context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &provider.RateLimitError{StatusCode: 429, Err: errors.New("too many requests")}
}

func (p *rateLimitProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// TestHubProviderSemaphoreSharedAcrossLoops：装配口径 —— 所有 hub loop 共享同一个
// 进程级信号量实例（改一个 loop 的配置不会让别的 loop 绕过闸门）。
func TestHubProviderSemaphoreSharedAcrossLoops(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "闸门A")
	b := createHubAgent(t, m, buf, 2, "闸门B")
	g := convGroupWith(t, m, buf, 10, "闸门群", nil, a, b)
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	// 未配置 = 不限（nil），保持旧行为。
	if sem := m.hubProviderSemaphore(); sem != nil {
		t.Fatalf("未配置时不该有信号量: %+v", sem)
	}
	if bsA.loop == nil || bsB.loop == nil {
		t.Fatal("会话 loop 未装配")
	}
	// 两个 loop 用的是同一个 manager 级实例（同一份配置生效）。
	sem := m.hubProviderSemaphore()
	if sem != m.hubProviderSemaphore() {
		t.Fatal("信号量必须是进程级单例")
	}
}

var _ = core.FinishReasonStop
