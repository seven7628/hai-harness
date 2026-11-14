package main

// P7-04（review nit #6）：run 因 429 收尾时，退回队列**之前**要显式撤掉这一轮的
// run 超时定时器。
//
// 修复前的现状：投递成功时按 (group, agent) 挂了 run 超时，run 因 429 结束只把投递
// 退回队列（queued + 500ms 退避），定时器还留在 map 里 —— 退避窗口内这个 agent
// 根本没有在跑的 run。run_timeout 比退避短时，迟到的定时器会在窗口内白跑一次复核
// （hubRunTimeoutFired 复核后什么都不做，但那是白跑，且 map 里挂着一条「僵尸」）。
//
// 反证价值：去掉 429 分支的 clearHubRunTimeout → 本用例在「queued(rate_limit) 却还
// 挂着定时器」处直接红。

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

// conv429GateProvider：第一次开流先卡在门闩上（把「投递成功、run 超时已挂」做成可
// 观测窗口），放行后返回 429；之后每次正常收尾（429 退避重投成功）。
type conv429GateProvider struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	gate    chan struct{}
	reply   string
}

func newConv429GateProvider(reply string) *conv429GateProvider {
	return &conv429GateProvider{
		started: make(chan struct{}, 1),
		gate:    make(chan struct{}),
		reply:   reply,
	}
}

func (p *conv429GateProvider) Stream(ctx context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n == 1 {
		select {
		case p.started <- struct{}{}:
		default:
		}
		select {
		case <-p.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
		return &provider.RateLimitError{StatusCode: 429, Err: errors.New("注入：上游 429")}
	}
	return h(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: p.reply})
}

func (p *conv429GateProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// waitStarted 等第一次开流（门闩式 fake 的确定性锚点）。
func (p *conv429GateProvider) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider 没有被调用：唤醒链路没走通")
	}
}

// TestHubConversation429ClearsRunTimeout：429 退避窗口内该 agent 不该还挂着 run 超时；
// 退避到期重投成功后再按新一轮重新挂上（不重复、不漏）。
func TestHubConversation429ClearsRunTimeout(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "限流A")
	b := createHubAgent(t, m, buf, 2, "限流B")
	g := convGroupWith(t, m, buf, 10, "限流群", map[string]any{"max_turns_per_human": 100}, a, b)
	fb := newConv429GateProvider("重投成功")
	release := releaseOnce(fb.gate)
	defer release()
	setGroupFakeLoopNoRetry(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)

	// 人类 @B：B 的 run 第一轮就 429（run 中途限流，不是投递窗口假 429）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@限流B 你先接一下",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	fb.waitStarted(t)

	// 前提：投递成功那一刻确实挂了 run 超时（否则「429 之后没有定时器」毫无意义）。
	if rt := convRunTimer(m, g.ID, b.ID); rt == nil {
		t.Fatal("投递成功后没有挂 run 超时定时器（用例前提不成立）")
	}

	// 放行 → 429 → 退回队列（500ms 退避）。
	release()
	convWait(t, "B 的投递退回队列（429 退避）", func() bool {
		ds := convDeliveriesFor(m, g.ID, b.ID)
		if len(ds) == 0 {
			return false
		}
		last := ds[len(ds)-1]
		return last.Status == agenthub.DeliveryQueued && last.Reason == agenthub.DeliveryReasonRateLimit
	})
	// 退避窗口内（这条投递是 queued，没有任何在跑的 run）：定时器必须已经撤掉。
	if rt := convRunTimer(m, g.ID, b.ID); rt != nil {
		t.Fatalf("429 退避窗口内还挂着 run 超时定时器: %+v（这个 agent 现在没有在跑的 run）", rt)
	}

	// 退避到期重投：仍然只投一次、最终 done，且重投期间正常挂上新的定时器（随后收尾撤掉）。
	convWait(t, "429 退避后重投成功", func() bool {
		ds := convDeliveriesFor(m, g.ID, b.ID)
		if len(ds) == 0 {
			return false
		}
		last := ds[len(ds)-1]
		return last.Status == agenthub.DeliveryDone && last.Attempts >= 2
	})
	if n := fb.callCount(); n != 2 {
		t.Fatalf("provider 开流 %d 次, want 2（429 一次 + 重投一次）", n)
	}
	convWait(t, "重投收尾后定时器撤掉", func() bool { return convRunTimer(m, g.ID, b.ID) == nil })
}
