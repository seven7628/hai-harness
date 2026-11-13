package main

// P6-02：统一群调度器 —— 并发 = 本轮目标数（12 人群 @all = 11 条同时跑）、
// 人类与 agent wake 同一调度器、目标忙留队、429 不标 failed、串行 fallback 仍在。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// concurrencyProvider 记录**同时在飞**的 provider 调用数（P6-02 的并行证据）。
// 所有调用卡在同一个 release 上，直到测试放行 —— 这样 maxInFlight 就是真实的
// 并发峰值，不靠 sleep 猜时序。
type concurrencyProvider struct {
	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	started     chan struct{}
	release     <-chan struct{}
}

func newConcurrencyProvider() *concurrencyProvider {
	return &concurrencyProvider{started: make(chan struct{}, 64)}
}

func (p *concurrencyProvider) gated() chan struct{} {
	ch := make(chan struct{})
	p.release = ch
	return ch
}

func (p *concurrencyProvider) Stream(ctx context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.maxInFlight {
		p.maxInFlight = p.inFlight
	}
	release := p.release
	p.mu.Unlock()
	select {
	case p.started <- struct{}{}:
	default:
	}
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return h(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "收到"})
}

func (p *concurrencyProvider) peak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxInFlight
}

func (p *concurrencyProvider) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider 没有被调用")
	}
}

// TestHubGroupAllMentionsRunsAllTargetsInParallel 是 P6-02 的主验收：12 人群里
// 某个成员 @all 必须**真的**同时放行 11 条 run（并发 = 本轮目标数 = 除发送者外的
// 全部活跃成员），不是只改了个字段。
func TestHubGroupAllMentionsRunsAllTargetsInParallel(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})

	agents := make([]agenthub.AgentProfile, 0, 12)
	for i := 0; i < 12; i++ {
		agents = append(agents, createHubAgent(t, m, buf, i+1, "并发成员"+string(rune('A'+i))))
	}
	g := convGroupWith(t, m, buf, 30, "十二人群", nil, agents...)
	prov := newConcurrencyProvider()
	release := prov.gated()
	defer close(release)
	sender := agents[0]
	// A（发送者）用不卡门闩的 fake：它要能跑完一轮；其余 11 个成员共用计数 provider。
	p3bWireFake(t, m, g, string(sender.ID), "点名了")
	for _, ag := range agents[1:] {
		bs := mustGroupSession(t, m, string(ag.ID), string(g.ID))
		setGroupFakeLoop(t, m, bs, g, string(ag.ID), prov)
	}

	// 人类 @A：A 先跑一轮（让 A 成为「发送者」）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 40, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@并发成员A 你来点名",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	convWait(t, "A 的 run 收尾", func() bool {
		bs, err := m.hubGroupSession(string(sender.ID), string(g.ID))
		return err == nil && !bs.hubGroupBusy.Load()
	})

	// A 的 group_say @all：12 人群 = 除发送者外的 11 条，必须同时跑。
	res, err := p3bGroupSay(t, m, string(sender.ID), map[string]any{
		"group_id": string(g.ID), "text": "@所有人 报个到", "mention_all": true,
	})
	if err != nil {
		t.Fatalf("A 的 group_say: %v", err)
	}
	if got := len(toStringIDs(res["targets"])); got != 11 {
		t.Fatalf("@all 目标 = %d, want 11（12 人群除发送者）", got)
	}
	convWait(t, "11 条 run 同时跑", func() bool { return prov.peak() >= 11 })
	if got := prov.peak(); got != 11 {
		t.Fatalf("并发峰值 = %d, want 11（并发必须 = 本轮目标数）", got)
	}
	// 账本里 11 条都是 running（没有一个被串行闸门挡住）。
	running := 0
	for _, d := range m.hubConversation().Deliveries(g.ID) {
		if d.Status == agenthub.DeliveryRunning {
			running++
		}
	}
	if running != 11 {
		t.Fatalf("running 记录 = %d, want 11", running)
	}
	if v := convView(m, g.ID); v.QueueLen != 0 {
		t.Fatalf("queue = %d, want 0（全部已放行）", v.QueueLen)
	}
}

// TestHubGroupSerialPumpFallback：串行 fallback 仍然可用（一次只放一条），
// 保证降级路径不腐烂。
func TestHubGroupSerialPumpFallback(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createHubAgent(t, m, buf, 1, "降级A")
	b := createHubAgent(t, m, buf, 2, "降级B")
	c := createHubAgent(t, m, buf, 3, "降级C")
	g := convGroupWith(t, m, buf, 10, "降级群", nil, a, b, c)
	prov := newConcurrencyProvider()
	release := prov.gated()
	defer close(release)
	for _, ag := range []agenthub.AgentProfile{a, b, c} {
		setGroupFakeLoop(t, m, mustGroupSession(t, m, string(ag.ID), string(g.ID)), g, string(ag.ID), prov)
	}
	hubConvSerialPump = true
	defer func() { hubConvSerialPump = false }()

	if ok, errMsg, _ := hubCmd(t, m, buf, 20, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@降级A @降级B @降级C 排队来",
	}); !ok {
		t.Fatalf("send: %s", errMsg)
	}
	prov.waitStarted(t)
	// 串行：只放行一条，另外两条留在队列里。
	convWait(t, "只放行一条", func() bool { return convView(m, g.ID).QueueLen == 2 })
	if got := prov.peak(); got != 1 {
		t.Fatalf("串行 fallback 并发峰值 = %d, want 1", got)
	}
}

// rateLimitOnceProvider 前 n 次开流返回 429（run **中途**限流的确定性注入），
// 之后转给内层 provider。旧版本用 hubWakeAskHook（投递窗口）注入 429，那条路径
// 根本不会产生 AgentEnd，因此从没测到「run 收尾覆写 requeue」这个 bug（B2）。
type rateLimitOnceProvider struct {
	inner *convProvider
	mu    sync.Mutex
	fails int
}

func (p *rateLimitOnceProvider) Stream(ctx context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	fail := p.fails > 0
	if fail {
		p.fails--
	}
	p.mu.Unlock()
	if fail {
		return &provider.RateLimitError{StatusCode: 429, Err: errors.New("too many requests")}
	}
	return p.inner.Stream(ctx, req, h)
}

// TestHubGroupRateLimitRequeuesNotFails：run 中途 provider 429 → 退回队列 + 退避，
// **不标 failed**，且退避到期后必须**真的重投一次**（第二次 provider 调用、
// attempts 增加）。目标不是坏掉，只是暂时被限流。
func TestHubGroupRateLimitRequeuesNotFails(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createHubAgent(t, m, buf, 1, "限流A")
	b := createHubAgent(t, m, buf, 2, "限流B")
	g := convGroupWith(t, m, buf, 10, "限流群", nil, a, b)
	fb := newConvProvider("限流之后成功")
	prov := &rateLimitOnceProvider{inner: fb, fails: 1}
	// 不重试：429 要作为「这一轮结束的原因」冒到 AgentEnd（默认 10 次重试会让测试
	// 等十几秒，而且测不到 run 收尾的收口路径）。
	setGroupFakeLoopNoRetry(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), prov)

	ok, errMsg, data := hubCmd(t, m, buf, 20, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@限流B 试一下",
	})
	if !ok {
		t.Fatalf("send: %s", errMsg)
	}
	d0 := mustDelivery(t, data, b.ID)
	convWait(t, "429 之后记录退回 queued", func() bool {
		d, found := m.hubConversation().Delivery(g.ID, d0.WakeID)
		return found && d.Status == agenthub.DeliveryQueued && d.ReadyAt != nil
	})
	d, _ := m.hubConversation().Delivery(g.ID, d0.WakeID)
	if d.Status == agenthub.DeliveryFailed {
		t.Fatalf("429 不该标 failed: %+v", d)
	}
	if d.Status == agenthub.DeliveryDone {
		t.Fatalf("run 收尾把退避中的 queued 覆写成 done（B2）: %+v", d)
	}
	if d.Reason != agenthub.DeliveryReasonRateLimit {
		t.Fatalf("原因 = %q, want %q", d.Reason, agenthub.DeliveryReasonRateLimit)
	}
	if !d.ReadyAt.After(time.Now().Add(-time.Second)) {
		t.Fatalf("缺退避时刻: %+v", d.ReadyAt)
	}
	// 退避到期后自动重试：这一条必须真的再跑一次（第二次 provider 调用）。
	convWait(t, "退避后必须真的重投一次", func() bool { return fb.callCount() >= 1 })
	d2, _ := m.hubConversation().Delivery(g.ID, d0.WakeID)
	if d2.Attempts < 2 {
		t.Fatalf("重投后 attempts = %d, want >= 2（%+v）", d2.Attempts, d2)
	}
	if d2.Status != agenthub.DeliveryRunning && d2.Status != agenthub.DeliveryDone {
		t.Fatalf("重投后状态 = %s, want running/done", d2.Status)
	}
}

// TestHubGroupPolicyIgnoresMaxParallelRuns：MaxParallelRuns 已删除（D1）——
// 旧 JSON 里带着它仍然能读，group update 传它被忽略（并发永远是目标数）。
func TestHubGroupPolicyIgnoresMaxParallelRuns(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "旧字段A")
	b := createHubAgent(t, m, buf, 2, "旧字段B")
	ok, errMsg, data := hubCmd(t, m, buf, 10, "hub_group_create", map[string]any{
		"name": "旧字段群",
		"members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
		// 旧客户端还会传这个键：必须被忽略而不是报错。
		"policy": map[string]any{"mode": "ask", "max_parallel_runs": 7},
	})
	if !ok {
		t.Fatalf("create with legacy field: %s", errMsg)
	}
	g := groupFromData(t, data)
	if g.Policy.Mode != agenthub.GroupModeAsk {
		t.Fatalf("mode = %q", g.Policy.Mode)
	}
	// update 传 max_parallel_runs：忽略，不报错，也不影响其它字段。
	ok, errMsg, data = hubCmd(t, m, buf, 12, "hub_group_update", map[string]any{
		"group_id": string(g.ID),
		"patch":    map[string]any{"policy": map[string]any{"max_parallel_runs": 3, "max_mentions_per_message": 5}},
	})
	if !ok {
		t.Fatalf("update with legacy field: %s", errMsg)
	}
	if got := groupFromData(t, data); got.Policy.MaxMentions != 5 {
		t.Fatalf("同批合法字段被丢弃: %+v", got.Policy)
	}
}
