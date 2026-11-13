package main

// hub_conversation.go：群内自动对话的 bridge 侧接线（P3b-04，方案 §6.5.6）。
//
// 纯状态机在 agenthub.ConversationController（轮次预算/深度/踢皮球/冷却/同内容）。
// 这里负责「什么时候唤醒谁」：
//   - 人类消息：BeginHuman 开新对话（清队列/计数），首轮目标**并行**唤醒（不进队列），
//     所以人一句话就能打断自动接龙；
//   - 智能体之间的 @（group_say / final 回复）：Offer 入队，然后**串行**放行 ——
//     一次只投一条，下一条等上一个 run 的 AgentEnd（finishHubReply 的 defer，在
//     hubGroupBusy 放开之后），并且两次智能体唤醒之间至少间隔 Cooldown；
//   - 目标忙：wake 留在队里，下一次群 run 结束再取（不丢唤醒、也不硬塞第二份快照）；
//   - 终态（stopped/exhausted）：只写**一条**系统消息 + 一个 hub_conversation 事件。
//
// 定时器只用于冷却窗口（群 run 结束 → 下一条还没到 ReadyAt）。人类消息会换代
// （hubConvGen），旧定时器一律作废 —— 断线/连点也不会把上一条对话的队列带回来。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/provider"
)

// errHubGroupTargetBusy 目标群会话正忙（上一轮没结束）。与真正的错误区分开：
// 对话队列遇到它只「留队下次再试」，不当故障告警。
var errHubGroupTargetBusy = errors.New("群会话忙：上一轮尚未结束")

// hubConversation 懒开对话控制器。限额可被测试用 hubConvLimits 先注入（生产恒 nil）。
// 生产不设自动接龙轮次上限：深度/同对/冷却/同内容指纹照样拦，唯独不再按「第 8 轮」停。
func (m *manager) hubConversation() *agenthub.ConversationController {
	m.mu.Lock()
	defer m.mu.Unlock()
	// P6-04：限额来自 settings.json 顶层 hub.conversation（全局，不是每群策略）。
	// I1：按文件指纹缓存（只 stat，不每次读盘）；文件变了就热更新，无需重启。
	limits := loadHubConversationLimitsCached()
	if m.hubConvLimits != nil {
		limits = *m.hubConvLimits
	}
	if m.hubConv == nil {
		opts := agenthub.ConversationOptions{Limits: limits}
		// P6-09：对话状态落盘（conversations/<gid>.json）。落盘失败只告警：
		// 磁盘问题不能让群聊停下。
		opts.Persister = hubConversationPersister{m: m}
		opts.OnPersistError = func(gid agenthub.GroupID, err error) {
			m.hubAlertf("hub_conversation_persist_failed", "群 %s 对话状态写入失败：%v", gid, err)
		}
		m.hubConv = agenthub.NewConversationController(opts)
		return m.hubConv
	}
	// I1：设置保存后立即生效（旧对话状态原样保留）。只在真的变了时写一次。
	if m.hubConv.Limits() != limits {
		m.hubConv.SetLimits(limits)
	}
	return m.hubConv
}

// hubConversationView 当前对话快照（从未有过对话时 Status=idle）。
func (m *manager) hubConversationView(gid agenthub.GroupID) agenthub.ConversationView {
	return m.hubConversation().View(gid)
}

// beginHubConversation 人类消息开启新对话：清队列/计数、换代、占 1 个首轮预算。
// 返回视图与首轮 turn（0 = 没有目标，不占预算）。
func (m *manager) beginHubConversation(g *agenthub.Group, humanSeq int64, targets int) (agenthub.ConversationView, int) {
	if g == nil {
		return agenthub.ConversationView{}, 0
	}
	// 与 pump 的「取/投/记账」互斥：人类开新对话时不能让一条旧 wake 正在投递。
	unlock := m.hubConvDispatch.lock(g.ID)
	defer unlock()
	return m.beginHubConversationWithinGate(g, humanSeq, targets)
}

// beginHubConversationWithinGate 同 beginHubConversation，但**调用方已持有**
// 该群的 dispatch 闸门。首轮唤醒要和换代放在同一个临界区里，否则停止可以插在
// 「对话已换代」与「首轮 wake 投递」之间（B2）。
func (m *manager) beginHubConversationWithinGate(g *agenthub.Group, humanSeq int64, targets int) (agenthub.ConversationView, int) {
	if g == nil {
		return agenthub.ConversationView{}, 0
	}
	ctrl := m.hubConversation()
	ctrl.BeginHuman(g.ID, humanSeq, time.Time{})
	m.bumpHubConversationGen(g.ID) // 上一条对话的冷却计时器作废
	turn := ctrl.StartHumanRound(g.ID, targets, time.Time{})
	view := ctrl.View(g.ID)
	m.emitHubConversation(g.ID, view)
	return view, turn
}

// offerGroupWakes 把一条智能体消息上的 @ 目标交给对话控制器（串行队列）。
// 命中护栏时控制器已把状态推到 exhausted，这里顺手写系统消息/事件（去重）。
//
// conversationID 是**发起这条 @ 的那一轮 run** 所属的对话 id（人类首轮/无 wake 的
// run 为空 = 不校验）。调用方必须显式传进来，不能现读会话镜像：final 收尾时
// finishHubReply 已经清掉 hubGroupWake，现读只会拿到空串，旧 generation 的 @ 就会
// 被当成新对话的 @ 路由出去（P6-07 §1.5）。
func (m *manager) offerGroupWakes(g *agenthub.Group, round agenthub.Round, trigger *agenthub.Message, sender agenthub.AgentID, depth int, conversationID string) ([]agenthub.Wake, error) {
	if m == nil || g == nil || trigger == nil || len(round.Targets) == 0 {
		return nil, nil
	}
	ctrl := m.hubConversation()
	wakes, err := ctrl.Offer(agenthub.ConversationOffer{
		GroupID: g.ID, Sender: sender, Targets: round.Targets,
		Text: trigger.Text, Depth: depth, TriggerSeq: trigger.Seq, RoundID: round.ID,
		// 发起者那一轮所属的对话 id：人类插话换代之后，旧 run 的 @ 不得进入新对话
		//（§1.1/§1.5）。空 = 未知（人类首轮/测试），不校验。
		ConversationID: strings.TrimSpace(conversationID),
	})
	if err != nil {
		if errors.Is(err, agenthub.ErrConversationExhausted) {
			m.notifyHubConversation(g.ID, ctrl.View(g.ID))
		}
		return nil, err
	}
	// 全部目标都被护栏跳过 → waiting_human（不是 exhausted）：写一条说明，人类一句话
	// 就能继续（P6-04 §1.1 到限行为）。
	m.notifyHubConversationWaiting(g.ID, ctrl.View(g.ID))
	return wakes, nil
}

// hubRunConversationID 取某智能体**当前在跑的 run** 所属对话 id（空 = 未知）。
// group_say 用它做换代路由校验：旧 generation 的 @ 不能进入人类新开的对话。
func (m *manager) hubRunConversationID(agentID string, gid agenthub.GroupID) string {
	if m == nil || agentID == "" || gid == "" {
		return ""
	}
	ws := m.hubRuntimeIfExists(agentID)
	if ws == nil {
		return ""
	}
	sid := hubSessionID(agentID, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: gid})
	ws.mu.Lock()
	bs := ws.sessions[sid]
	ws.mu.Unlock()
	if bs == nil {
		return ""
	}
	return bs.hubWakeConversationID()
}

// hubGroupNextDepth 下一跳 @ 的深度：当前 run 绑定的唤醒深度 + 1（人类=0 → 首轮=1）。
// 读的是 emit 写进原子字段的镜像，工具 goroutine 可安全读。
func (m *manager) hubGroupNextDepth(agentID string, g *agenthub.Group) int {
	if m == nil || g == nil {
		return 1
	}
	ws := m.hubRuntimeIfExists(agentID)
	if ws == nil {
		return 1
	}
	sid := hubSessionID(agentID, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	ws.mu.Lock()
	bs := ws.sessions[sid]
	ws.mu.Unlock()
	if bs == nil {
		return 1
	}
	return int(bs.hubWakeDepth.Load()) + 1
}

// hubConvDispatchHook 仅测试用：把「取到 wake、尚未投递」的窗口停住（生产恒 nil），
// 用来证明停止/人类插话确实被群闸门挡在这个窗口之外（P3b-08 B2）。
var hubConvDispatchHook func(phase string, gid agenthub.GroupID)

// hubConvDispatchBarrier 读钩子时取一次快照（同其它 barrier 惯例）。
func hubConvDispatchBarrier(phase string, gid agenthub.GroupID) {
	if hook := hubConvDispatchHook; hook != nil {
		hook(phase, gid)
	}
}

// hubConvDispatchGate 按群串行化「取 wake → 投递 → 记账」与「停止 / 人类开新对话」。
//
// 两段式（NextDue 之后再投递）以前没有任何闸门：人类在两步之间点停止，wake 仍会被
// 投进目标会话（真跑一轮，却不经过 BeginTurn 记账，等于绕过预算/深度护栏），
// 而 BeginTurn=false 被当成良性竞态静默吞掉。现在停止/人类消息持同一把群锁，
// 要么排在投递之后（再中断刚起来的 run），要么排在之前（投递方看到 stopped 直接退）。
//
// 按群分锁：一个群的停止不该把别的群的接龙堵住。锁条目按群缓存（群数量有界）。
type hubConvDispatchGate struct {
	mu  sync.Mutex
	per map[agenthub.GroupID]*sync.Mutex
}

// lock 返回该群的解锁函数。不可重入：调用方不得在持锁时再次调用同类路径
// （wake 投递是异步唤醒 run，不会同步回到 pump）。
func (g *hubConvDispatchGate) lock(gid agenthub.GroupID) func() {
	g.mu.Lock()
	if g.per == nil {
		g.per = map[agenthub.GroupID]*sync.Mutex{}
	}
	mu := g.per[gid]
	if mu == nil {
		mu = &sync.Mutex{}
		g.per[gid] = mu
	}
	g.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// pumpHubConversation 是**统一群调度器**的一次 pass（P6-02）：人类与 agent 的 wake
// 走同一个入口，一次把「现在能跑的目标」全部并行放行。
//
// 并发 = 本轮目标数（D1/§1.3）：12 人群 @all = 11 条同时投；显式 @3 = 3 条。
// 每个 target 至多一条在飞（群会话忙闩 + DueWakes 的去重），所以「并发 = 目标数」
// 是自然上限，不需要（也不再允许）一个 MaxParallelRuns 旋钮。
//
// 调用点：
//   - group_say / 人类消息入队之后（同 goroutine，首条立即投递）；
//   - 群 run 真正收尾（emit 末尾的 hubOnRunEnd，running 与群轮闩都已放开）；
//   - 冷却/退避定时器（scheduleHubConversationTimer）。
//
// 目标忙的 wake **不动队列**：账本退回 queued，下一次 pass 再取（不丢、不标 failed）。
// 投递出错（可重试的 429/上游抖动）同样退回 queued + 退避，只有真错误才 failed。
func (m *manager) pumpHubConversation(gid agenthub.GroupID) {
	if m == nil || m.hubShutdown.Load() {
		return
	}
	// 取/投/记账是一个临界区：停止与人类插话不能插在 DueWakes 与投递之间
	// （否则 wake 会在停止之后真跑一轮，见 hubConvDispatchGate）。
	unlock := m.hubConvDispatch.lock(gid)
	defer unlock()
	ctrl := m.hubConversation()
	view := ctrl.View(gid)
	if view.Terminal() {
		m.notifyHubConversation(gid, view)
		return
	}
	// B1：waiting_human 且队列非空时**也要泵** —— 与控制器 schedulableLocked 同一
	// 判据（View.Schedulable）。只认 running 会让「到限 → waiting_human → 另一条
	// 在飞 run 回来 @D」的 D 永久留在队里（Offer 成功、DueWakes 却返回空）。
	// stopped/exhausted 仍不放。
	if !view.Schedulable() {
		return
	}
	// 串行 fallback（保留）：hubConvSerialPump 打开时一次只放一条。生产恒关；
	// 弱网/单 provider 槽位需要降级时可以用它，测试也用它锁住 fallback 不腐烂。
	if hubConvSerialPump {
		wake, ok := ctrl.NextDue(gid, time.Time{})
		if !ok {
			m.armHubConversationTimer(gid)
			return
		}
		m.runHubConversationPass(gid, []agenthub.Wake{wake})
		return
	}
	wakes := ctrl.DueWakes(gid, time.Time{})
	if len(wakes) == 0 {
		// DueWakes 也可能刚把状态推到 exhausted（队列里还剩预算外的 wake）。
		if v := ctrl.View(gid); v.Terminal() {
			m.notifyHubConversation(gid, v)
			return
		}
		m.armHubConversationTimer(gid)
		return
	}
	m.runHubConversationPass(gid, wakes)
}

// hubConvSerialPump 是串行 fallback 开关（默认关）。P6-02 之后正常路径必须并行
// 放行（并发 = 本轮目标数）；保留它是因为「一次一个」在单 provider 槽位/弱网
// 场景仍是合理的降级策略。测试用它锁住 fallback 不腐烂，生产恒为 false。
var hubConvSerialPump = false

// armHubConversationTimer 队列里还有东西但都不可跑（冷却/429 退避）时，排一个定时器
// 到最早可跑时刻。没有可跑时刻就什么都不做（等 run 收尾/人类消息再来）。
func (m *manager) armHubConversationTimer(gid agenthub.GroupID) {
	ctrl := m.hubConversation()
	next, ok := ctrl.NextReadyAt(gid)
	if !ok {
		return
	}
	if d := time.Until(next); d > 0 {
		m.scheduleHubConversationTimer(gid, d)
	}
}

// runHubConversationPass 并行投递一批 wake（并发 = 批大小 = 本轮目标数）。
//
// 先过一次「停止/换代」复查（Stop P1：停止是**先标记 stopped、再抢闸门**，所以它
// 可能刚好落在 DueWakes 之后），再逐条投递；任何一条失败都不影响其他目标。
func (m *manager) runHubConversationPass(gid agenthub.GroupID, wakes []agenthub.Wake) {
	ctrl := m.hubConversation()
	hubConvDispatchBarrier("due", gid)
	cur := ctrl.View(gid)
	if cur.Status == agenthub.ConversationStopped {
		return
	}
	// 预跑预算门（P6-03）：一次 pass 只读一次群/成本索引，逐 target 判三级日预算。
	// 预算不足的目标**不启动 run**（账本记 skipped(budget)），其他目标照跑。
	g := m.loadHubGroup(gid)
	deliverable := make([]agenthub.Wake, 0, len(wakes))
	for _, w := range wakes {
		if w.ConversationID != "" && w.ConversationID != cur.ID {
			// 旧 generation 的 wake：丢弃（账本里会记 skipped(stale)）。
			m.hubAlertf("group_conversation_stale_wake", "群 %s 唤醒 %s 属于已结束的对话，已丢弃", gid, w.Target)
			continue
		}
		if ok, reason := m.hubConversationBudget(g, w.Target); !ok {
			if ctrl.SkipWake(gid, w.ID, agenthub.DeliveryReasonBudget, time.Time{}) {
				m.notifyHubBudgetBlocked(gid, reason)
			}
			continue
		}
		deliverable = append(deliverable, w)
	}
	if len(deliverable) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, w := range deliverable {
		wg.Add(1)
		go func(w agenthub.Wake) {
			defer wg.Done()
			m.deliverScheduledWake(gid, w)
		}(w)
	}
	wg.Wait()
	hubConvDispatchBarrier("delivered", gid)
	// 记账（TurnsUsed/Depth/冷却起点）已经在 wakeGroupAgent 的 CommitDelivery 里完成：
	// 那条路径是幂等的唯一入口，这里不能再记一次（会把轮次算成两轮）。
	m.emitHubConversation(gid, ctrl.View(gid))
	// 还有没投出去的（目标忙/冷却/退避）→ 排定时器兜底；真正放行时闸门会再判一次。
	if v := ctrl.View(gid); v.QueueLen > 0 {
		m.armHubConversationTimer(gid)
	}
}

// deliverScheduledWake 投递一条调度器放行的 wake，并把失败分成「可重试」与「真错误」：
//   - 目标忙（errHubGroupTargetBusy）：账本已退回 queued，不告警；
//   - 429/上游抖动：退回 queued + 指数退避（**不标 failed**，§1.3）；
//   - 其它错误：账本 failed + 告警；群/成员不存在则收口成 stopped。
func (m *manager) deliverScheduledWake(gid agenthub.GroupID, wake agenthub.Wake) {
	ctrl := m.hubConversation()
	err := m.dispatchConversationWake(gid, wake)
	if err == nil {
		return
	}
	// 账本是权威：还留在 queued = 可重试（目标忙 / 429 退避），不是失败。
	// wakeGroupAgent 已经把记录退回队列并写好 ReadyAt，这里只负责排下一次泵。
	if d, ok := ctrl.Delivery(gid, wake.ID); ok && d.Status == agenthub.DeliveryQueued {
		m.emitHubConversation(gid, ctrl.View(gid))
		if d.ReadyAt != nil {
			if delay := time.Until(*d.ReadyAt); delay > 0 {
				m.scheduleHubConversationTimer(gid, delay)
			}
		}
		return
	}
	m.hubAlertf("group_conversation_wake_failed", "群 %s 自动对话唤醒 %s 失败：%v", gid, wake.Target, err)
	if errors.Is(err, agenthub.ErrNotFound) {
		// 群/成员已不存在：重试永远是同一个结果，收口成 stopped。
		m.notifyHubConversation(gid, ctrl.Stop(gid, "群或成员已不存在，自动对话已停止", time.Time{}))
	}
}

// hubWakeRetryBase/Max 是 429 退避的档位（P6-03 会把 provider 信号量接到同一档）。
const (
	hubWakeRetryBase = 500 * time.Millisecond
	hubWakeRetryMax  = 30 * time.Second
)

// hubWakeRetryDelay 第 n 次尝试的退避时长（500ms 起，翻倍，封顶 30s）。
func hubWakeRetryDelay(ctrl *agenthub.ConversationController, gid agenthub.GroupID, wake agenthub.Wake) time.Duration {
	attempt := 1
	if ctrl != nil {
		if d, ok := ctrl.Delivery(gid, wake.ID); ok && d.Attempts > 0 {
			attempt = d.Attempts
		}
	}
	d := hubWakeRetryBase
	for i := 1; i < attempt; i++ {
		if d >= hubWakeRetryMax/2 {
			return hubWakeRetryMax
		}
		d *= 2
	}
	if d > hubWakeRetryMax {
		return hubWakeRetryMax
	}
	return d
}

// loadHubGroup 读群元数据（预算门用；读不到就返回 nil，由调用方 fail-open 放行）。
func (m *manager) loadHubGroup(gid agenthub.GroupID) *agenthub.Group {
	if m == nil || gid == "" {
		return nil
	}
	st, err := m.hubStore()
	if err != nil {
		return nil
	}
	g, err := st.LoadGroup(context.Background(), gid)
	if err != nil || g == nil || g.Deleted {
		return nil
	}
	return g
}

// hubConversationBudget 是**预跑预算门**（P6-03）：run 启动之前判三级日预算
// （agent / group / global）。预算不足 → 不启动 run，账本记 skipped(budget)。
//
// 为什么要在启动前判：hubBudgetCheck 是 LLMEnd 处的**硬停**，它只能阻止继续烧，
// 挡不住「已经开跑一轮」。11 个成员同时被 @ 时，跑完再停已经花掉了 11 轮钱。
//
// 读不到档案/索引时 fail-open（放行）：预算门不能因为一次读失败把群聊卡死；
// 真正的兜底仍是 LLMEnd 硬停与 Guard.Check。
func (m *manager) hubConversationBudget(g *agenthub.Group, agentID agenthub.AgentID) (bool, string) {
	if m == nil || g == nil || agentID == "" {
		return true, ""
	}
	st, err := m.hubStore()
	if err != nil || st == nil {
		return true, ""
	}
	p, _, err := m.hubLoadAgent(string(agentID))
	if err != nil || p == nil {
		return true, ""
	}
	limits := agenthub.GuardLimits{
		DailyBudgetUSD:       p.Policy.DailyBudgetUSD,
		GroupDailyBudgetUSD:  g.Policy.DailyBudgetUSD,
		GlobalDailyBudgetUSD: agenthub.DefaultGlobalDailyBudgetUSD,
	}
	if limits.DailyBudgetUSD <= 0 && limits.GroupDailyBudgetUSD <= 0 && limits.GlobalDailyBudgetUSD <= 0 {
		return true, "" // 三级都不限额：不必读成本索引
	}
	agentDay, groupDay, globalDay := st.DailyCost(agentID, g.ID, time.Now().UTC())
	if over, msg := limits.OverDailyBudget(agentDay, groupDay, globalDay); over {
		return false, msg
	}
	return true, ""
}

// notifyHubBudgetBlocked 预算门命中时写一条系统消息（按原因去重）+ 告警事件。
// 人需要知道「为什么没人被叫醒」，而不是只看到队列没动。
func (m *manager) notifyHubBudgetBlocked(gid agenthub.GroupID, reason string) {
	if m == nil {
		return
	}
	key := "budget|" + reason
	m.hubConvMu.Lock()
	if m.hubConvNotified == nil {
		m.hubConvNotified = map[agenthub.GroupID]string{}
	}
	if m.hubConvNotified[gid] == key {
		m.hubConvMu.Unlock()
		return
	}
	m.hubConvNotified[gid] = key
	m.hubConvMu.Unlock()

	text := "预算不足，本轮没有启动：" + reason
	st, err := m.hubStore()
	if err != nil {
		return
	}
	msg := &agenthub.Message{
		GroupID: gid, Kind: agenthub.KindSystem,
		Sender: agenthub.Sender{Kind: agenthub.SenderSystem}, Text: text,
	}
	seq, err := agenthub.AppendGroupMessage(context.Background(), st, msg)
	if err != nil {
		m.hubAlertf("group_conversation_notice_failed", "群 %s 预算系统消息写入失败：%v", gid, err)
		return
	}
	m.emitHubGroupMessage(gid, seq, msg)
	m.hubAlertf("group_conversation_budget", "群 %s 预算不足，本轮未启动：%s", gid, reason)
}

// hubRetryableWakeError 判定「可重试」的投递错误：provider 限流（429）与上游瞬时
// 不可用都不该把目标标 failed —— 排回队列、退避后重试即可（§1.3）。
func hubRetryableWakeError(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var rl *provider.RateLimitError
	if errors.As(err, &rl) {
		return agenthub.DeliveryReasonRateLimit, true
	}
	var up *provider.UpstreamUnavailableError
	if errors.As(err, &up) {
		return agenthub.DeliveryReasonRateLimit, true
	}
	return "", false
}

// dispatchConversationWake 把一条 wake 投给目标智能体的群会话（hidden 快照 + 正文）。
func (m *manager) dispatchConversationWake(gid agenthub.GroupID, wake agenthub.Wake) error {
	st, err := m.hubStore()
	if err != nil {
		return err
	}
	ctx := context.Background()
	g, err := st.LoadGroup(ctx, gid)
	if err != nil {
		return err
	}
	if g == nil || g.Deleted {
		return fmt.Errorf("%w: 群组 %s 不存在", agenthub.ErrNotFound, gid)
	}
	_, err = m.wakeGroupAgent(ctx, st, g, wake)
	return err
}

// finishHubGroupDelivery 群 run 收尾时把**这一轮**的投递记录推进到 done。
//
// 必须按 wake id 收口，不能「把这个目标所有 running 都标 done」：忙闩在
// finishHubReply 的 defer 里就放开了，而收尾在 emit 末尾 —— 中间这一小段里泵可能
// 已经把下一条 wake 投给同一目标（BeginDelivery 记成 running）。按目标收口会把
// 新那一轮误标 done，它的 CommitDelivery 之后就只能 no-op（轮次不涨、pair 不记）。
// wakeID 为空（任务结果回推等无 wake 的 run）才退回按目标收口。
func (m *manager) finishHubGroupDelivery(gid agenthub.GroupID, agentID agenthub.AgentID, wakeID string) {
	if m == nil || gid == "" || agentID == "" {
		return
	}
	// P6-08：这一轮已经收尾，超时定时器必须撤掉（迟到的定时器会误杀下一轮）。
	m.clearHubRunTimeout(gid, agentID)
	ctrl := m.hubConversation()
	finished := false
	if wakeID != "" {
		// B2：只收口**这一轮真的还在跑**的记录。429 退回队列（queued + ReadyAt）的
		// 那一条绝不能被 AgentEnd 覆写成 done —— 否则退避到期后 BeginDelivery
		// no-op，该 wake 永久占住队首。FinishDelivery 内部也只允许 running → 终态，
		// 这里是同一判据的显式前置检查（两处都做更稳）。
		if d, ok := ctrl.Delivery(gid, wakeID); !ok || d.Status != agenthub.DeliveryRunning {
			return
		}
		finished = ctrl.FinishDelivery(gid, wakeID, agenthub.DeliveryDone, "", time.Time{})
	} else {
		finished = ctrl.FinishAgentDeliveries(gid, agentID, agenthub.DeliveryDone, "", time.Time{}) > 0
	}
	if finished {
		// 终态视图由 notifyHubConversation 统一去重上报（否则「run 收尾 + 耗尽」
		// 会各发一个 exhausted 事件）。
		if v := ctrl.View(gid); !v.Terminal() {
			m.emitHubConversation(gid, v)
		}
	}
}

// scheduleHubConversationTimer 在冷却窗口结束后重试一次泵。同一群只保留一个定时器，
// 人类消息（bumpHubConversationGen）会让已排队的定时器作废。
func (m *manager) scheduleHubConversationTimer(gid agenthub.GroupID, d time.Duration) {
	if m == nil {
		return
	}
	if d <= 0 {
		d = time.Millisecond
	}
	m.hubConvMu.Lock()
	gen := m.hubConvGen[gid]
	if t := m.hubConvTimer[gid]; t != nil {
		t.Stop()
	}
	if m.hubConvTimer == nil {
		m.hubConvTimer = map[agenthub.GroupID]*time.Timer{}
	}
	m.hubConvTimer[gid] = time.AfterFunc(d, func() {
		m.hubConvMu.Lock()
		if m.hubConvGen[gid] != gen { // 人类消息已换代：这一代作废
			m.hubConvMu.Unlock()
			return
		}
		delete(m.hubConvTimer, gid)
		m.hubConvMu.Unlock()
		m.scheduleHubConversationPump(gid)
	})
	m.hubConvMu.Unlock()
}

// scheduleHubConversationPump 异步泵一次（关停后 spawnHubBackground 直接拒绝）。
// emit 路径（持有 dispatchMu）**必须**走这里：同步泵会在 Ask→persist 失败→emit
// 的路径上重入同一把锁（与 P2-03 hubPumpQueue 同一教训）。
func (m *manager) scheduleHubConversationPump(gid agenthub.GroupID) {
	if m == nil || m.hubShutdown.Load() {
		return
	}
	m.spawnHubBackground(func(context.Context) {
		m.pumpHubConversation(gid)
	})
}

// bumpHubConversationGen 换代并停掉该群挂着的冷却定时器。
func (m *manager) bumpHubConversationGen(gid agenthub.GroupID) {
	m.hubConvMu.Lock()
	defer m.hubConvMu.Unlock()
	if m.hubConvGen == nil {
		m.hubConvGen = map[agenthub.GroupID]uint64{}
	}
	m.hubConvGen[gid]++
	if t := m.hubConvTimer[gid]; t != nil {
		t.Stop()
		delete(m.hubConvTimer, gid)
	}
}

// emitHubConversation 广播对话视图（begin/turn 变化/停止/耗尽）。
func (m *manager) emitHubConversation(gid agenthub.GroupID, view agenthub.ConversationView) {
	if m == nil || m.out == nil {
		return
	}
	m.out.writeLine(map[string]any{
		"event_type": "hub_conversation", "group_id": string(gid), "conversation": view,
	})
}

// notifyHubConversation 终态收口：写**一条**系统群消息 + 一个 hub_conversation 事件。
// 幂等按「对话 id + 状态」去重：重复停止/重复泵不会刷屏，人类开新对话后可以再报一次。
func (m *manager) notifyHubConversation(gid agenthub.GroupID, view agenthub.ConversationView) {
	if m == nil || !view.Terminal() {
		return
	}
	key := view.ID + "|" + string(view.Status)
	m.hubConvMu.Lock()
	if m.hubConvNotified == nil {
		m.hubConvNotified = map[agenthub.GroupID]string{}
	}
	if m.hubConvNotified[gid] == key {
		m.hubConvMu.Unlock()
		return
	}
	m.hubConvNotified[gid] = key
	m.hubConvMu.Unlock()

	text := strings.TrimSpace(view.StopReason)
	if text == "" {
		if view.Status == agenthub.ConversationStopped {
			text = "人类停止了自动对话，等你继续"
		} else {
			text = "自动对话已停止，等你继续"
		}
	}
	st, err := m.hubStore()
	if err != nil {
		m.hubAlertf("group_conversation_notice_failed", "群 %s 自动对话系统消息写入失败：%v", gid, err)
		return
	}
	msg := &agenthub.Message{
		GroupID: gid, Kind: agenthub.KindSystem,
		Sender: agenthub.Sender{Kind: agenthub.SenderSystem}, Text: text,
	}
	seq, err := agenthub.AppendGroupMessage(context.Background(), st, msg)
	if err != nil {
		// 群已删除等永久错误：保留去重标记，避免每次泵都刷一条 hub_alert。
		m.hubAlertf("group_conversation_notice_failed", "群 %s 自动对话系统消息写入失败：%v", gid, err)
		return
	}
	m.emitHubGroupMessage(gid, seq, msg)
	m.emitHubConversation(gid, view)
}

// groupSessions 返回该群**已装配**的群会话（只查不建）。
//
// 不读群文件：群会话的 sid 是 hub.<agent>.g.<group> 的确定形态，遍历 hub 运行时逐个
// 拼出来即可 —— O(智能体数) 且不会因为群被删/存储抖动而失败，也不给全体成员装配会话。
func (m *manager) groupSessions(gid agenthub.GroupID) []*bridgeSession {
	if m == nil || gid == "" {
		return nil
	}
	m.mu.Lock()
	runtimes := make([]*workspaceRuntime, 0, len(m.hubRuntimes))
	for _, ws := range m.hubRuntimes {
		if ws != nil {
			runtimes = append(runtimes, ws)
		}
	}
	m.mu.Unlock()
	out := make([]*bridgeSession, 0, len(runtimes))
	for _, ws := range runtimes {
		sid := hubSessionID(ws.agentID, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: gid})
		ws.mu.Lock()
		bs := ws.sessions[sid]
		ws.mu.Unlock()
		if bs != nil {
			out = append(out, bs)
		}
	}
	return out
}

// hubAgentID 该会话所属的 Hub 智能体（非 hub 运行时 / 未装配返回空）。
func (bs *bridgeSession) hubAgentID() agenthub.AgentID {
	if bs == nil || bs.runtime == nil || bs.runtime.kind != runtimeHubAgent {
		return ""
	}
	return agenthub.AgentID(bs.runtime.agentID)
}

// groupSessionActive 该群会话是否「还在跑」：与 hubSessionBusy 同口径
// （running / 群轮闩 / 已投未开跑 / 有积压输入）。
func groupSessionActive(bs *bridgeSession) bool {
	if bs == nil {
		return false
	}
	return bs.running.Load() || bs.hubGroupBusy.Load() || bs.hubDispatchPending.Load() ||
		bs.s.PendingCount() > 0
}

// groupRunInFlight 该群是否还有群会话在跑；except 非空时跳过该智能体
// （刚放行的那一条不该把自己算成「还在跑」）。
// 串行泵与停止按钮共用这一个判据。
func (m *manager) groupRunInFlight(gid agenthub.GroupID, except agenthub.AgentID) bool {
	if m == nil {
		return false
	}
	for _, bs := range m.groupSessions(gid) {
		if except != "" && bs.hubAgentID() == except {
			continue
		}
		if groupSessionActive(bs) {
			return true
		}
	}
	return false
}

// groupActiveSessions 该群里仍在跑的群会话（停止按钮用）。
func (m *manager) groupActiveSessions(g *agenthub.Group) []*bridgeSession {
	if m == nil || g == nil {
		return nil
	}
	sessions := m.groupSessions(g.ID)
	out := make([]*bridgeSession, 0, len(sessions))
	for _, bs := range sessions {
		if groupSessionActive(bs) {
			out = append(out, bs)
		}
	}
	return out
}

// pumpAgentTask 泵一次智能体任务队列，并把泵出来的**终态**如实投影出去（P3b-05 B-1）。
//
// agenthub.Queue.Pump 在「目标不可派发（成员被移出/档案失效）」「目标侧去重命中」
// 「已投递但落账失败」这几条路径上会把任务落成 canceled/failed 并返回该终态（可能
// 同时带 error）。调用方以前只把 error 变成告警、把 task 丢掉 —— 结果卡与派发者
// 回推永远不来，任务板也停在旧状态。这里统一补上：
//   - 任何终态都发 hub_task_update（UI 才看得到 canceled/failed）；
//   - target=agent 时走 notifyAgentTaskResult（幂等卡 + 只回推派发者，canceled/
//     failed 也走同一条通知路径）。
//
// 非终态（dispatched/queued/busy/nil）原样返回；error 也原样返回，告警由调用方保留。
func (m *manager) pumpAgentTask(ctx context.Context, target agenthub.TaskTarget) (*agenthub.Task, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: manager 不可用", agenthub.ErrInvalid)
	}
	if target.Kind != agenthub.TargetAgent || target.GroupID == "" || target.AgentID == "" {
		return nil, fmt.Errorf("%w: pumpAgentTask 只接受带 group/agent 的 agent 目标", agenthub.ErrInvalid)
	}
	q, err := m.hubQueueFor()
	if err != nil {
		return nil, err
	}
	task, perr := q.Pump(ctx, target)
	if task == nil || !task.Status.Terminal() {
		return task, perr
	}
	m.emitHubTaskUpdate(task)
	m.notifyAgentTaskResult(task)
	return task, perr
}

// scheduleAgentTaskPump 群会话根 run 结束后推进该智能体的任务队列（D-agent-task-pump）。
//
// assign_task 撞上「目标忙」时任务留在账本 queued（AgentDispatch 返回 Accepted=false）；
// 没有这个泵，那条任务要等下一个人 @ 才可能被派发，甚至永远卡住。这里在
// **hubGroupBusy 放开之后**泵一次（只一次：Pump 自身最多派一个），异步执行 ——
// 调用点持有 dispatchMu，同步泵会在 Ask→persist 失败→emit 上重入死锁。
// 关停后 spawnHubBackground 直接拒绝（不再动账本）。
func (m *manager) scheduleAgentTaskPump(gid agenthub.GroupID, agentID agenthub.AgentID) {
	if m == nil || m.hubShutdown.Load() || gid == "" || agentID == "" {
		return
	}
	m.spawnHubBackground(func(ctx context.Context) {
		q, err := m.hubQueueFor()
		if err != nil {
			m.hubAlertf("task_pump_failed", "任务泵不可用（群 %s / 智能体 %s）：%v", gid, agentID, err)
			return
		}
		target := agenthub.TaskTarget{Kind: agenthub.TargetAgent, GroupID: gid, AgentID: agentID}
		if q.QueuedCountFor(target) == 0 {
			return // 没有排队的任务：不碰存储、不阻塞事件
		}
		// 终态（canceled/failed）由 pumpAgentTask 投影成任务卡 + 结果卡 + 回推，
		// 不再静默丢掉（P3b-05 B-1）。非终态（dispatched/running）以前被直接丢弃：
		// 忙路径的任务板就永远停在 queued，直到完成才有下一条事件（P3b-07a N-02）。
		pumped, perr := m.pumpAgentTask(ctx, target)
		if perr != nil && ctx.Err() == nil {
			m.hubAlertf("task_pump_failed", "群 %s 智能体 %s 的任务泵失败：%v", gid, agentID, perr)
		}
		if pumped == nil || pumped.Status.Terminal() {
			return
		}
		// 幂等：Pump 只有把队首 queued→dispatched 才算派发，同一个任务只会从这里
		// 成功返回一次；重复的 run-end 看到队列已空直接返回，不会重发。
		latest := pumped
		if st, serr := m.hubStore(); serr == nil {
			if cur, rerr := st.LoadTask(ctx, pumped.ID); rerr == nil && cur != nil {
				latest = cur // 投递到发事件的窗口里可能已经 AgentStart：如实报 running
			}
		}
		if latest.Status.Terminal() {
			return // 结算路径（finishHubTask）已经/即将投影终态，别抢发旧快照
		}
		m.emitHubTaskUpdate(latest)
	})
}

// notifyHubConversationWaiting 全部目标都被护栏跳过时，写一条系统说明（P6-04）：
// 状态是 waiting_human 而不是 exhausted —— 人类一句话就能继续，UI 也不该显示「已达上限」。
// 按「对话 id + 原因」去重：同一代里重复 @ 不会刷屏。
func (m *manager) notifyHubConversationWaiting(gid agenthub.GroupID, view agenthub.ConversationView) {
	if m == nil || view.Status != agenthub.ConversationWaitingHuman {
		return
	}
	text := strings.TrimSpace(view.StopReason)
	if text == "" {
		text = "本轮没有可唤醒的目标，等待人类继续"
	}
	key := "waiting|" + view.ID + "|" + text
	m.hubConvMu.Lock()
	if m.hubConvNotified == nil {
		m.hubConvNotified = map[agenthub.GroupID]string{}
	}
	if m.hubConvNotified[gid] == key {
		m.hubConvMu.Unlock()
		return
	}
	m.hubConvNotified[gid] = key
	m.hubConvMu.Unlock()

	st, err := m.hubStore()
	if err != nil {
		return
	}
	msg := &agenthub.Message{
		GroupID: gid, Kind: agenthub.KindSystem,
		Sender: agenthub.Sender{Kind: agenthub.SenderSystem}, Text: text,
	}
	seq, err := agenthub.AppendGroupMessage(context.Background(), st, msg)
	if err != nil {
		m.hubAlertf("group_conversation_notice_failed", "群 %s 对话说明写入失败：%v", gid, err)
		return
	}
	m.emitHubGroupMessage(gid, seq, msg)
	m.emitHubConversation(gid, view)
}

// hubConversationReset 人类「重置本群限制」（P6-04 ④）：立即清零 pair/深度/指纹/
// 队列并解除 waiting_human，**不自动唤醒任何人**（下一次 @ 立即生效）。
//
// 与 stop 的区别：stop 是「别再说了」（中断在飞 run、抑制 final、写系统消息）；
// reset 是「把额度还给我」（不动在飞 run，只清计数）。在飞 run 收尾时它的 final
// 照常落账 —— 它属于旧 generation 的产出，人已经看到了。
func (m *manager) hubConversationReset(c command) {
	if m == nil {
		return
	}
	if m.hubShutdown.Load() {
		m.resp(c, false, "Hub 正在关停，已忽略重置对话限制的请求", nil)
		return
	}
	gid := agenthub.GroupID(strings.TrimSpace(str(c.Payload, "group_id")))
	if gid == "" {
		m.resp(c, false, "group_id 不能为空", nil)
		return
	}
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	g, err := st.LoadGroup(ctx, gid)
	if err != nil {
		m.resp(c, false, "读取群组失败: "+err.Error(), nil)
		return
	}
	if g == nil || g.Deleted {
		m.resp(c, false, "群组不存在", nil)
		return
	}
	ctrl := m.hubConversation()
	// 与调度器同一个闸门：重置不能插在「取 wake → 投递」之间（否则刚清掉的
	// 队列会被这一 pass 再投一条出去）。
	unlock := m.hubConvDispatch.lock(g.ID)
	view := ctrl.Reset(g.ID, time.Time{})
	unlock()
	m.bumpHubConversationGen(g.ID) // 冷却/退避定时器作废
	m.hubConvMu.Lock()
	if m.hubConvNotified != nil {
		delete(m.hubConvNotified, g.ID) // 下一次护栏命中要能再报一次
	}
	m.hubConvMu.Unlock()
	m.emitHubConversation(g.ID, view)
	m.resp(c, true, "", map[string]any{"conversation": view})
}

// hubConversationStop 人类「停止自动对话」：中断当前群 run、清队列、stopped + 一条系统消息。
// 幂等：重复点击只返回当前视图（不再写消息/事件）。
func (m *manager) hubConversationStop(c command) {
	if m == nil {
		return
	}
	// 关停之后不再写群日志/系统消息：迟到的 UI 点击不该在正在拆除的存储上留痕（N6）。
	if m.hubShutdown.Load() {
		m.resp(c, false, "Hub 正在关停，已忽略停止自动对话的请求", nil)
		return
	}
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	gid := agenthub.GroupID(strings.TrimSpace(str(c.Payload, "group_id")))
	if gid == "" {
		m.resp(c, false, "group_id 不能为空", nil)
		return
	}
	g, err := st.LoadGroup(ctx, gid)
	if err != nil {
		m.resp(c, false, "读取群组失败: "+err.Error(), nil)
		return
	}
	if g == nil || g.Deleted {
		m.resp(c, false, "群组不存在", nil)
		return
	}
	// P6-08：带 agent_id = 只停这个成员（其他成员继续）。不带 = 群级停止。
	if agentID := agenthub.AgentID(strings.TrimSpace(str(c.Payload, "agent_id"))); agentID != "" {
		m.hubConversationStopAgent(c, g, agentID)
		return
	}
	ctrl := m.hubConversation()
	// Stop P1：**先**标记 stopped / 清队列（不持闸门），再去抢闸门做中断。
	// 这样停止窗口里收尾的 fast run 在 finishHubRound 里读到的一定是 stopped，
	// final 会被抑制；而闸门保证「正在投递的 wake」先落地/记账，不会被中途撕开。
	view := ctrl.Stop(g.ID, "人类停止了自动对话，等你继续", time.Time{})
	m.bumpHubConversationGen(g.ID) // 冷却定时器作废：停止之后不许再自己接龙
	// P6-01：账本跟着停止走 —— 排队的记 skipped(stopped)，在跑的记 interrupted
	//（它们的 run 马上被中断，不会再正常收尾成 done）。
	ctrl.SkipQueuedDeliveries(g.ID, agenthub.DeliveryReasonStopped, time.Time{})
	unlock := m.hubConvDispatch.lock(g.ID)
	interrupted := 0
	for _, bs := range m.groupActiveSessions(g) {
		// P6-08：被停掉的 run 不用再等超时定时器（迟到的那次会复核账本 no-op）。
		m.clearHubRunTimeout(g.ID, agenthub.AgentID(bs.hubDeltaAgentID))
		if bs.hubInterrupt != nil {
			bs.hubInterrupt()
		} else {
			bs.s.Interrupt()
		}
		interrupted++
	}
	unlock()
	if interrupted > 0 {
		ctrl.InterruptRunningDeliveries(g.ID, agenthub.DeliveryReasonInterrupted, time.Time{})
	}
	view = ctrl.View(g.ID)
	// 系统消息在放闸门之后写：任何已经在收尾的 final 要么排在它前面（闸门先到，
	// 状态还是 running），要么已经被抑制 —— 不会出现「停止」之后又冒一条 final。
	m.notifyHubConversation(g.ID, view)
	m.resp(c, true, "", map[string]any{"conversation": view, "interrupted": interrupted})
}

// hubConversationStopAgent 只停一个成员的群运行（P6-08）：
//   - 中断它在本群的 run（其他成员的 run 一条都不碰）；
//   - 丢掉它排队的「下一个输入」（skipped(stopped)），不再冒出来跑；
//   - 账本把它的 running 收口成 failed(interrupted)；
//   - 对话状态保持 running（其他成员还要继续接龙），只写一条点名说明。
//
// 幂等：没有可停的东西（没 run、没排队）时只回当前视图，不写说明、不告警。
func (m *manager) hubConversationStopAgent(c command, g *agenthub.Group, agentID agenthub.AgentID) {
	if m == nil || g == nil || agentID == "" {
		return
	}
	if !groupHasMember(g, agentID) {
		m.resp(c, false, "该智能体不是这个群的成员", nil)
		return
	}
	ctrl := m.hubConversation()
	// 与投递/停止同一个闸门：先摘队列，再中断，避免「刚摘掉的 wake 又被这一 pass 投出去」。
	unlock := m.hubConvDispatch.lock(g.ID)
	skipped := ctrl.SkipAgentQueuedDeliveries(g.ID, agentID, agenthub.DeliveryReasonStopped, time.Time{})
	interrupted := 0
	if m.interruptGroupAgent(g.ID, agentID) {
		interrupted = 1
	}
	if interrupted > 0 {
		ctrl.FinishAgentDeliveries(g.ID, agentID, agenthub.DeliveryFailed, agenthub.DeliveryReasonInterrupted, time.Time{})
	}
	unlock()
	m.clearHubRunTimeout(g.ID, agentID)
	if interrupted > 0 || skipped > 0 {
		st, err := m.hubStore()
		name := string(agentID)
		if err == nil {
			name = agentDisplayName(context.Background(), st, agentID)
		}
		text := fmt.Sprintf("已停止「%s」在本群的运行（中断 %d 个 run，跳过 %d 条排队唤醒），其他成员继续",
			name, interrupted, skipped)
		m.appendHubSystemMessage(g.ID, text)
	}
	view := ctrl.View(g.ID)
	m.emitHubConversation(g.ID, view)
	m.resp(c, true, "", map[string]any{
		"conversation": view, "interrupted": interrupted, "skipped": skipped, "agent_id": string(agentID),
	})
}
