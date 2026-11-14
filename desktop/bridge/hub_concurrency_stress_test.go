package main

// P7-03：12 人群 @all 的并发 / 429 压力回归（确定性 fake provider，不碰网络、不要 API key）。
//
// 契约澄清（P7-03 文档由主 Session 更新）：`agenthub/router.go` 的 @all = 「除发送者外的
// 全部活跃成员」。人类发送时 sender 不在成员表里，所以 12 个智能体的群（1 owner + 11
// member，正好是 MaxGroupMembers 上限）人类 @all 得到 **12** 个目标、并发峰值 12；只有
// 群内智能体（这里是 owner）自己 @all 时才排除自己 = 11。产品口径是「并发跟随本轮目标
// 数」，所以两个口径各钉一个子测试，不是「永远 11」。
//
// 反证价值：
//   - 调度退化成串行 → 峰值 1 被抓；
//   - 把 owner 也算成「发送者」/ 按 11 硬编码上限 → 人类那轮峰值 11 被抓；
//   - 429 标 failed、退避后不重投、重投重复投递 → 账本 attempts + provider 开流次数被抓。
//
// 并发时序全部靠门闩（所有 provider 调用先卡在同一个 hold 上）+ 条件轮询，不用 sleep 猜。

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

// hubStressRecorder 记录「同时在飞的 provider 调用数」（并发峰值的唯一来源）与每个
// target 的开流次数（429 重投 → 2）。
type hubStressRecorder struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	calls    map[agenthub.AgentID]int
	hold     chan struct{}
	once     sync.Once
}

func newHubStressRecorder() *hubStressRecorder {
	return &hubStressRecorder{calls: map[agenthub.AgentID]int{}, hold: make(chan struct{})}
}

// open 一次性放行所有卡在门闩上的 provider 调用（重复调用安全）。
func (r *hubStressRecorder) open() {
	r.once.Do(func() { close(r.hold) })
}

// enter 记一次开流：并发 +1、峰值取 max、该 target 调用次数 +1，返回这是第几次。
func (r *hubStressRecorder) enter(a agenthub.AgentID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight++
	if r.inFlight > r.peak {
		r.peak = r.inFlight
	}
	r.calls[a]++
	return r.calls[a]
}

func (r *hubStressRecorder) leave() {
	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
}

// peakNow 当前并发峰值。门闩没放行时，它等于「同时在飞的 run 数」。
func (r *hubStressRecorder) peakNow() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak
}

// callsOf 某 target 的开流次数（429 退避重投 = 2，正常 = 1）。
func (r *hubStressRecorder) callsOf(a agenthub.AgentID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[a]
}

// hubStressProvider 是每个 target 独立的确定性 fake：
//   - 每次开流先记账，再卡在门闩上（把「同时在飞」变成可观测量）；
//   - 前 failures 次开流返回 429 —— 这是 **run 中途**限流：provider.Stream 出错后
//     agents loop 以 AgentEnd{FinishReasonError, ErrorKindRateLimit} 收尾，走
//     requeueHubWakeOnRateLimit（不是投递窗口的假 429）；
//   - 之后正常收尾。
type hubStressProvider struct {
	rec      *hubStressRecorder
	agent    agenthub.AgentID
	failures int
	reply    string
}

func (p *hubStressProvider) Stream(ctx context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	n := p.rec.enter(p.agent)
	defer p.rec.leave()
	select {
	case <-p.rec.hold:
	case <-ctx.Done():
		return ctx.Err()
	}
	if n <= p.failures {
		return &provider.RateLimitError{StatusCode: 429, Err: errors.New("stress: 上游 429（注入）")}
	}
	return h(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: p.reply})
}

// hubStressRig 是「12 人群 + 每个成员一个确定性 fake loop」的测试夹具。
type hubStressRig struct {
	m       *manager
	buf     *syncBuf
	g       *agenthub.Group
	owner   agenthub.AgentProfile
	members []agenthub.AgentProfile // 11 个 member（不含 owner）
	rec     *hubStressRecorder
	provs   map[agenthub.AgentID]*hubStressProvider
}

// agents 返回 owner + 11 个 member（12 人群的全体智能体）。
func (r *hubStressRig) agents() []agenthub.AgentProfile {
	out := make([]agenthub.AgentProfile, 0, len(r.members)+1)
	out = append(out, r.owner)
	return append(out, r.members...)
}

// newHubStressRig 建 12 个智能体（1 owner + 11 member，正好 MaxGroupMembers 上限）的群，
// 并给每个成员装「不重试」的确定性 fake loop。failIdx 是首轮开流就 429 的智能体下标
// （0 = owner，1..11 = member）。
func newHubStressRig(t *testing.T, failIdx ...int) *hubStressRig {
	t.Helper()
	fails := make(map[int]bool, len(failIdx))
	for _, i := range failIdx {
		fails[i] = true
	}
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	rec := newHubStressRecorder()
	// 断言失败提前 return 时也要放行：否则 run goroutine 会一直卡在门闩上。
	t.Cleanup(rec.open)
	agents := make([]agenthub.AgentProfile, 0, agenthub.MaxGroupMembers)
	for i := 0; i < agenthub.MaxGroupMembers; i++ {
		agents = append(agents, createHubAgent(t, m, buf, i+1, "压力成员"+string(rune('A'+i))))
	}
	g := convGroupWith(t, m, buf, 30, "十二人压力群", nil, agents...)
	rig := &hubStressRig{
		m: m, buf: buf, g: g, owner: agents[0], members: agents[1:],
		rec: rec, provs: make(map[agenthub.AgentID]*hubStressProvider, len(agents)),
	}
	for i, ag := range agents {
		p := &hubStressProvider{rec: rec, agent: ag.ID, reply: "收到，" + ag.Name + " 就位"}
		if fails[i] {
			p.failures = 1
		}
		rig.provs[ag.ID] = p
		bs := mustGroupSession(t, m, string(ag.ID), string(g.ID))
		setGroupFakeLoopNoRetry(t, m, bs, g, string(ag.ID), p)
	}
	return rig
}

// stressWait 轮询直到 pred 为真。超时给足（12 个群会话装配 + 429 退避 500ms），但判据
// 本身是确定性的条件（不是 sleep 猜时序）。
func stressWait(t *testing.T, timeout time.Duration, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）：%s", timeout, what)
}

// stressWaitDelivered 等这一轮的全部投递收口成 done（429 退避重投也算），返回账本快照。
func stressWaitDelivered(t *testing.T, rig *hubStressRig, want int) []agenthub.Delivery {
	t.Helper()
	var ds []agenthub.Delivery
	stressWait(t, 60*time.Second, "全部投递收口成 done（含 429 退避重投）", func() bool {
		ds = rig.m.hubConversation().Deliveries(rig.g.ID)
		if len(ds) != want {
			return false
		}
		for _, d := range ds {
			if d.Status != agenthub.DeliveryDone {
				return false
			}
		}
		return true
	})
	return ds
}

// stressCheckLedger 校验账本：一个 target 一条、全部 done、attempts 与 provider 开流
// 次数一致（429 的目标 = 2，其余 = 1），没有 failed/skipped，也没有重复 target。
func stressCheckLedger(t *testing.T, rig *hubStressRig, ds []agenthub.Delivery, want int, human bool) {
	t.Helper()
	if len(ds) != want {
		t.Fatalf("投递记录 = %d, want %d（一个 target 一条）", len(ds), want)
	}
	byTarget := make(map[agenthub.AgentID]agenthub.Delivery, len(ds))
	for _, d := range ds {
		if prev, dup := byTarget[d.Target]; dup {
			t.Fatalf("同一 target 两条投递记录（重复 wake）：%+v / %+v", prev, d)
		}
		byTarget[d.Target] = d
		switch d.Status {
		case agenthub.DeliveryDone:
		case agenthub.DeliveryFailed, agenthub.DeliverySkipped:
			t.Fatalf("投递 %s 落到 %s(%s)：429 不该标 failed/skipped", d.Target, d.Status, d.Reason)
		default:
			t.Fatalf("投递 %s 还停在 %s（队列没排空）", d.Target, d.Status)
		}
		if d.Reason != "" {
			t.Fatalf("done 记录不该带原因码: %+v", d)
		}
		if d.Human != human {
			t.Fatalf("投递 %s 的 human 标记 = %v, want %v（轮次归属）", d.Target, d.Human, human)
		}
	}
	if len(byTarget) != want {
		t.Fatalf("去重后 target = %d, want %d", len(byTarget), want)
	}
	for _, ag := range rig.agents() {
		d, ok := byTarget[ag.ID]
		if !ok {
			// owner 在 owner-@all 那轮不是目标，跳过。
			if !human && ag.ID == rig.owner.ID {
				continue
			}
			t.Fatalf("target %s 没有投递记录（丢 wake）", ag.ID)
		}
		wantAttempts := 1
		if rig.provs[ag.ID].failures > 0 {
			wantAttempts = 2 // 429 必须退避后重投，且只重投一次
		}
		if d.Attempts != wantAttempts {
			t.Fatalf("target %s attempts = %d, want %d（429 退避重投 / 无重复投递）", ag.ID, d.Attempts, wantAttempts)
		}
		if got := rig.rec.callsOf(ag.ID); got != wantAttempts {
			t.Fatalf("target %s provider 开流 %d 次, want %d（重投必须真的再跑一轮；重复投递会多跑）", ag.ID, got, wantAttempts)
		}
	}
}

// stressCheckAgentEnds 校验每个 target 的 agent_end：429 的目标必须先是
// error/rate_limit（run 中途限流，不是投递窗口假 429），再是 stop（重投成功）；
// 其余目标只有一个 stop。owner 在 owner-@all 那轮不该有任何 agent_end。
func stressCheckAgentEnds(t *testing.T, rig *hubStressRig, human bool) {
	t.Helper()
	for _, ag := range rig.agents() {
		sid := hubSessionID(string(ag.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: rig.g.ID})
		ends := hubStressAgentEnds(t, rig.buf, sid)
		failed := rig.provs[ag.ID].failures > 0
		want := 1
		if failed {
			want = 2
		}
		if !human && ag.ID == rig.owner.ID {
			want = 0
		}
		if len(ends) != want {
			t.Fatalf("target %s 的 agent_end = %d 个, want %d", ag.ID, len(ends), want)
		}
		if failed {
			if ends[0]["finish_reason"] != string(core.FinishReasonError) ||
				ends[0]["error_kind"] != provider.ErrorKindRateLimit {
				t.Fatalf("target %s 首轮不是「run 中途 429」: %+v", ag.ID, ends[0])
			}
		}
		if want == 0 {
			continue
		}
		if last := ends[len(ends)-1]; last["finish_reason"] != string(core.FinishReasonStop) {
			t.Fatalf("target %s 没有以正常收尾结束: %+v", ag.ID, last)
		}
	}
}

// hubStressAgentEnds 取某个群会话的 agent_end 事件（按注入的 session_id 归并）。
func hubStressAgentEnds(t *testing.T, buf *syncBuf, sid string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range hubEvents(t, buf, "agent_end") {
		if s, _ := e["session_id"].(string); s == sid {
			out = append(out, e)
		}
	}
	return out
}

// hubStressPendingTimer 该群是否还挂着冷却/429 退避定时器（泄漏检查）。
func hubStressPendingTimer(m *manager, gid agenthub.GroupID) bool {
	m.hubConvMu.Lock()
	defer m.hubConvMu.Unlock()
	return m.hubConvTimer[gid] != nil
}

// hubStressPendingRunTimeouts 还挂着的 run 超时定时器数量（泄漏检查）。
func hubStressPendingRunTimeouts(m *manager) int {
	m.hubRunTimerMu.Lock()
	defer m.hubRunTimerMu.Unlock()
	return len(m.hubRunTimers)
}

// stressCheckQuiescent 收口后的静态检查：队列 0、非终态、退避定时器已摘、没有还在跑的
// 群会话、run 超时定时器已撤（剩下的交给 testManager 的统一清理路径）。
func stressCheckQuiescent(t *testing.T, rig *hubStressRig) {
	t.Helper()
	v := convView(rig.m, rig.g.ID)
	if v.QueueLen != 0 {
		t.Fatalf("队列 = %d, want 0（全部目标都投出去了）", v.QueueLen)
	}
	if v.Terminal() {
		t.Fatalf("群对话落到终态 %s：%s", v.Status, v.StopReason)
	}
	// 队列排空后的合法状态：running（还在等下一句）或 waiting_human（等你继续）。
	if v.Status != agenthub.ConversationRunning && v.Status != agenthub.ConversationWaitingHuman {
		t.Fatalf("收口后状态 = %s, want running/waiting_human", v.Status)
	}
	stressWait(t, 10*time.Second, "429 退避定时器摘掉", func() bool {
		return !hubStressPendingTimer(rig.m, rig.g.ID)
	})
	for _, ag := range rig.agents() {
		bs, err := rig.m.hubGroupSession(string(ag.ID), string(rig.g.ID))
		if err != nil {
			t.Fatalf("取群会话 %s: %v", ag.ID, err)
		}
		if groupSessionActive(bs) {
			t.Fatalf("target %s 的群会话收口后仍活跃（run 泄漏）", ag.ID)
		}
	}
	if n := hubStressPendingRunTimeouts(rig.m); n != 0 {
		t.Fatalf("还挂着 %d 个 run 超时定时器（泄漏）", n)
	}
}

// TestHubAllMembersConcurrencyAnd429 是 P7-03 的主验收：12 人群 @all 的并发 = 本轮目标
// 数，且 3 条 run 中途 429 会退避重投成功（不 failed、不丢、不重复）。
func TestHubAllMembersConcurrencyAnd429(t *testing.T) {
	// 子测试 1：人类 @all —— 人类不在成员表里，12 个智能体全是目标，峰值 12；
	// 其中 3 条（member，非 owner）首轮 run 中途 429，退避后重投成功，其余 9 条正常收尾。
	t.Run("human_mention_all_12", func(t *testing.T) {
		// 3 个 member 首轮 429（下标 0 = owner，不注入限流：owner 要证明不受影响）。
		failIdx := []int{3, 7, 11}
		rig := newHubStressRig(t, failIdx...)
		ok, errMsg, data := hubCmd(t, rig.m, rig.buf, 40, "hub_group_send", map[string]any{
			"group_id":    string(rig.g.ID),
			"text":        "全体报到：一句话说明你在做什么",
			"mention_all": true,
		})
		if !ok {
			t.Fatalf("人类 @all 发送失败: %s", errMsg)
		}
		targets := toStringIDs(data["targets"])
		if len(targets) != agenthub.MaxGroupMembers {
			t.Fatalf("@all 目标 = %d, want %d（12 人群 = owner + 11 member，人类不占发送者名额）",
				len(targets), agenthub.MaxGroupMembers)
		}
		seen := make(map[agenthub.AgentID]bool, len(targets))
		for _, id := range targets {
			if seen[id] {
				t.Fatalf("@all 目标重复: %s（会跑两条 run、投两次）", id)
			}
			seen[id] = true
		}
		for _, ag := range rig.agents() {
			if !seen[ag.ID] {
				t.Fatalf("@all 漏了成员 %s（丢 wake）", ag.ID)
			}
			// 投递窗口就该是 running：目标全空闲，没人该留在 queued。
			if d := mustDelivery(t, data, ag.ID); d.Status != agenthub.DeliveryRunning {
				t.Fatalf("target %s 投递后状态 = %s(%s), want running", ag.ID, d.Status, d.Reason)
			}
		}
		// 12 条 run 全卡在门闩上 → 峰值就是真实并发。
		stressWait(t, 30*time.Second, "12 条 run 同时在飞", func() bool {
			return rig.rec.peakNow() >= len(targets)
		})
		if peak := rig.rec.peakNow(); peak != agenthub.MaxGroupMembers {
			t.Fatalf("并发峰值 = %d, want %d（并发 = 本轮目标数；1 = 串行，11 = 把人类算成发送者）",
				peak, agenthub.MaxGroupMembers)
		}
		for _, ag := range rig.agents() {
			if n := rig.rec.callsOf(ag.ID); n != 1 {
				t.Fatalf("target %s 首轮开流 %d 次, want 1（每个目标都真的启动了）", ag.ID, n)
			}
		}
		// 放行：3 条 429（run 中途），9 条正常收尾。
		rig.rec.open()
		ds := stressWaitDelivered(t, rig, len(targets))
		stressCheckLedger(t, rig, ds, len(targets), true)
		stressCheckAgentEnds(t, rig, true)
		stressCheckQuiescent(t, rig)
		t.Logf("人类 @all：目标 %d、并发峰值 %d、429 重投 %d 条、其余 %d 条一次成功",
			len(targets), rig.rec.peakNow(), len(failIdx), len(targets)-len(failIdx))
	})

	// 子测试 2：owner 智能体自己 @all —— 排除发送者自己 = 11 个目标，峰值 11
	// （不是 12：owner 不给自己投）。这条钉住「并发 = 本轮目标数」的另一侧。
	t.Run("owner_mention_all_11", func(t *testing.T) {
		rig := newHubStressRig(t)
		res, err := p3bGroupSay(t, rig.m, string(rig.owner.ID), map[string]any{
			"group_id": string(rig.g.ID), "text": "各模块报一下进度", "mention_all": true,
		})
		if err != nil {
			t.Fatalf("owner @all: %v", err)
		}
		targets := toStringIDs(res["targets"])
		if len(targets) != len(rig.members) {
			t.Fatalf("owner @all 目标 = %d, want %d（12 人群排除发送者自己）", len(targets), len(rig.members))
		}
		seen := make(map[agenthub.AgentID]bool, len(targets))
		for _, id := range targets {
			if seen[id] {
				t.Fatalf("@all 目标重复: %s", id)
			}
			seen[id] = true
		}
		if seen[rig.owner.ID] {
			t.Fatalf("owner 给自己投了 wake: %v", targets)
		}
		for _, ag := range rig.members {
			if !seen[ag.ID] {
				t.Fatalf("@all 漏了成员 %s（丢 wake）", ag.ID)
			}
		}
		// 投递窗口：11 条都该是 running（owner 不在里面）。
		ds0 := rig.m.hubConversation().Deliveries(rig.g.ID)
		if len(ds0) != len(targets) {
			t.Fatalf("投递记录 = %d, want %d", len(ds0), len(targets))
		}
		for _, d := range ds0 {
			if d.Status != agenthub.DeliveryRunning {
				t.Fatalf("target %s 投递后状态 = %s(%s), want running", d.Target, d.Status, d.Reason)
			}
		}
		stressWait(t, 30*time.Second, "11 条 run 同时在飞", func() bool {
			return rig.rec.peakNow() >= len(targets)
		})
		if peak := rig.rec.peakNow(); peak != len(rig.members) {
			t.Fatalf("并发峰值 = %d, want %d（不是 12：owner 是发送者，不给自己投）", peak, len(rig.members))
		}
		if n := rig.rec.callsOf(rig.owner.ID); n != 0 {
			t.Fatalf("owner 不该被唤醒，provider 却开流 %d 次", n)
		}
		rig.rec.open()
		ds := stressWaitDelivered(t, rig, len(targets))
		stressCheckLedger(t, rig, ds, len(targets), false)
		stressCheckAgentEnds(t, rig, false)
		stressCheckQuiescent(t, rig)
		t.Logf("owner @all：目标 %d、并发峰值 %d、全部一次成功", len(targets), rig.rec.peakNow())
	})
}
