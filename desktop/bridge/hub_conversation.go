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
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// errHubGroupTargetBusy 目标群会话正忙（上一轮没结束）。与真正的错误区分开：
// 对话队列遇到它只「留队下次再试」，不当故障告警。
var errHubGroupTargetBusy = errors.New("群会话忙：上一轮尚未结束")

// hubConversation 懒开对话控制器。限额可被测试用 hubConvLimits 先注入（生产恒 nil）。
func (m *manager) hubConversation() *agenthub.ConversationController {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hubConv == nil {
		opts := agenthub.ConversationOptions{}
		if m.hubConvLimits != nil {
			opts.Limits = *m.hubConvLimits
		}
		m.hubConv = agenthub.NewConversationController(opts)
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
func (m *manager) offerGroupWakes(g *agenthub.Group, round agenthub.Round, trigger *agenthub.Message, sender agenthub.AgentID, depth int) ([]agenthub.Wake, error) {
	if m == nil || g == nil || trigger == nil || len(round.Targets) == 0 {
		return nil, nil
	}
	ctrl := m.hubConversation()
	wakes, err := ctrl.Offer(agenthub.ConversationOffer{
		GroupID: g.ID, Sender: sender, Targets: round.Targets,
		Text: trigger.Text, Depth: depth, TriggerSeq: trigger.Seq, RoundID: round.ID,
	})
	if err != nil {
		if errors.Is(err, agenthub.ErrConversationExhausted) {
			m.notifyHubConversation(g.ID, ctrl.View(g.ID))
		}
		return nil, err
	}
	return wakes, nil
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

// pumpHubConversation 推动某个群的自动对话队列：**最多**投一条 wake 出去。
//
// 调用点：
//   - group_say 入队之后（同 goroutine，首条立即投递）；
//   - 群 run 真正收尾（emit 末尾的 hubOnRunEnd，running 与群轮闩都已放开）；
//   - 冷却定时器（scheduleHubConversationTimer）。
//
// 投递失败（目标忙）时**不动队列**：wake 还在队首，下一次群 run 结束再取。
// 串行语义：一次调用最多投一条 —— 下一条必须等这一条的 run 结束。
//
// 两段式（NextDue → 投递 → BeginTurn）之间可能被人类消息/停止插队：BeginTurn 会
// 返回 false，那一轮就不记账（会话里多收到一份快照是良性的，最多多一次空转）。
func (m *manager) pumpHubConversation(gid agenthub.GroupID) {
	if m == nil || m.hubShutdown.Load() {
		return
	}
	ctrl := m.hubConversation()
	view := ctrl.View(gid)
	if view.Terminal() {
		m.notifyHubConversation(gid, view)
		return
	}
	if view.Status != agenthub.ConversationRunning {
		return
	}
	wake, ok := ctrl.NextDue(gid, time.Time{})
	if !ok {
		// NextDue 也可能刚把状态推到 exhausted（队列里还剩预算外的 wake）。
		if v := ctrl.View(gid); v.Terminal() {
			m.notifyHubConversation(gid, v)
			return
		}
		if head, has := ctrl.Peek(gid); has {
			if d := time.Until(head.ReadyAt); d > 0 {
				m.scheduleHubConversationTimer(gid, d)
			}
		}
		return
	}
	if err := m.dispatchConversationWake(gid, wake); err != nil {
		if errors.Is(err, errHubGroupTargetBusy) {
			return // 留队：下一次群 run 结束再试，不告警（这是闸门，不是故障）
		}
		m.hubAlertf("group_conversation_wake_failed", "群 %s 自动对话唤醒 %s 失败：%v", gid, wake.Target, err)
		if errors.Is(err, agenthub.ErrNotFound) {
			// 群/成员已不存在：重试永远是同一个结果，收口成 stopped。
			m.notifyHubConversation(gid, ctrl.Stop(gid, "群或成员已不存在，自动对话已停止", time.Time{}))
		}
		return
	}
	if _, begun := ctrl.BeginTurn(gid, time.Time{}); !begun {
		// 投递与记账之间被人类消息/停止插队：这一轮已不属于当前对话，不再记账。
		return
	}
	m.emitHubConversation(gid, ctrl.View(gid))
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
	return m.wakeGroupAgent(ctx, st, g, wake)
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
			text = "本轮自动对话已达上限，等待人类继续"
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

// groupActiveSessions 该群里「已装配且仍在跑」的群会话（停止按钮用）。
// 只查不建：点一次停止不能为全体成员装配会话。
func (m *manager) groupActiveSessions(g *agenthub.Group) []*bridgeSession {
	if m == nil || g == nil {
		return nil
	}
	var out []*bridgeSession
	for _, mem := range g.Members {
		ws := m.hubRuntimeIfExists(string(mem.AgentID))
		if ws == nil {
			continue
		}
		sid := hubSessionID(string(mem.AgentID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
		ws.mu.Lock()
		bs := ws.sessions[sid]
		ws.mu.Unlock()
		if bs == nil {
			continue
		}
		if bs.hubGroupBusy.Load() || bs.hubDispatchPending.Load() || bs.running.Load() || bs.s.PendingCount() > 0 {
			out = append(out, bs)
		}
	}
	return out
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
		if _, err := q.Pump(ctx, target); err != nil && ctx.Err() == nil {
			m.hubAlertf("task_pump_failed", "群 %s 智能体 %s 的任务泵失败：%v", gid, agentID, err)
		}
	})
}

// hubConversationStop 人类「停止自动对话」：中断当前群 run、清队列、stopped + 一条系统消息。
// 幂等：重复点击只返回当前视图（不再写消息/事件）。
func (m *manager) hubConversationStop(c command) {
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
	ctrl := m.hubConversation()
	view := ctrl.Stop(g.ID, "人类停止了自动对话，等你继续", time.Time{})
	m.bumpHubConversationGen(g.ID) // 冷却定时器作废：停止之后不许再自己接龙
	interrupted := 0
	for _, bs := range m.groupActiveSessions(g) {
		if bs.hubInterrupt != nil {
			bs.hubInterrupt()
		} else {
			bs.s.Interrupt()
		}
		interrupted++
	}
	m.notifyHubConversation(g.ID, view)
	m.resp(c, true, "", map[string]any{"conversation": view, "interrupted": interrupted})
}
