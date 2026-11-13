package main

// hub_run_timeout.go：单轮 run 超时（P6-08）+ 按 agent 停止的共用动作。
//
// run_timeout_ms 是全局设置（settings.json 的 hub.conversation），P6-04 之前只回显
// 到对话视图里、没人真的执行。这里把它变成执行点：
//
//   - 投递成功（CommitDelivery）之后挂一个定时器，键 = (group, agent)（同一目标不会
//     并行跑两个群 run，忙闩保证）；
//   - run 真正收尾（hubOnRunEnd → finishHubGroupDelivery）或按 agent 停止时撤掉；
//   - 到点先复核账本：这条 wake 已经不是 running 就什么都不做 —— 收尾后的迟到定时器
//     绝不能把下一轮误杀；
//   - 否则只中断这个成员在本群的 run、账本记 failed(run_timeout)、写一条系统说明，
//     再补一次泵把队列继续下去（§1.3「超时中断并继续队列」）。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// hubRunTimer 是一个在跑群 run 的超时定时器。
type hubRunTimer struct {
	gid     agenthub.GroupID
	agent   agenthub.AgentID
	wakeID  string
	timeout time.Duration
	timer   *time.Timer
}

func hubRunTimerKey(gid agenthub.GroupID, agent agenthub.AgentID) string {
	return string(gid) + "\x00" + string(agent)
}

// armHubRunTimeout 给刚投递成功的一轮挂上 run 超时。重复投递先撤旧的再挂新的
// （同一个目标不会有两个在跑 run，旧定时器留着只会误杀新一轮）。
func (m *manager) armHubRunTimeout(gid agenthub.GroupID, agent agenthub.AgentID, wakeID string) {
	if m == nil || gid == "" || agent == "" || wakeID == "" {
		return
	}
	timeout := m.hubConversation().Limits().RunTimeout
	if timeout <= 0 {
		return
	}
	rt := &hubRunTimer{gid: gid, agent: agent, wakeID: wakeID, timeout: timeout}
	rt.timer = time.AfterFunc(timeout, func() { m.hubRunTimeoutFired(rt) })
	key := hubRunTimerKey(gid, agent)
	m.hubRunTimerMu.Lock()
	if m.hubRunTimers == nil {
		m.hubRunTimers = map[string]*hubRunTimer{}
	}
	old := m.hubRunTimers[key]
	m.hubRunTimers[key] = rt
	m.hubRunTimerMu.Unlock()
	if old != nil && old.timer != nil {
		old.timer.Stop()
	}
}

// clearHubRunTimeout 撤掉某目标的超时定时器（run 收尾 / 停止 / 关停）。
func (m *manager) clearHubRunTimeout(gid agenthub.GroupID, agent agenthub.AgentID) {
	if m == nil || gid == "" || agent == "" {
		return
	}
	key := hubRunTimerKey(gid, agent)
	m.hubRunTimerMu.Lock()
	rt := m.hubRunTimers[key]
	delete(m.hubRunTimers, key)
	m.hubRunTimerMu.Unlock()
	if rt != nil && rt.timer != nil {
		rt.timer.Stop()
	}
}

// hubRunTimeoutFired 到点：只杀**还在 running** 的那一轮，然后让队列继续。
func (m *manager) hubRunTimeoutFired(rt *hubRunTimer) {
	if m == nil || rt == nil || m.hubShutdown.Load() {
		return
	}
	ctrl := m.hubConversation()
	d, ok := ctrl.Delivery(rt.gid, rt.wakeID)
	if !ok || d.Status != agenthub.DeliveryRunning {
		// 已经收尾/被停止/被换代收口：定时器是迟到的，什么都不做。
		m.clearHubRunTimeout(rt.gid, rt.agent)
		return
	}
	// 与停止/投递同一个闸门：中断要落在「这一轮确实还在跑」的确定性窗口里。
	unlock := m.hubConvDispatch.lock(rt.gid)
	interrupted := m.interruptGroupAgent(rt.gid, rt.agent)
	ctrl.FinishAgentDeliveries(rt.gid, rt.agent, agenthub.DeliveryFailed, agenthub.DeliveryReasonTimeout, time.Time{})
	unlock()
	m.clearHubRunTimeout(rt.gid, rt.agent)
	view := ctrl.View(rt.gid)
	if interrupted {
		m.notifyHubRunTimeout(rt.gid, rt.agent, rt.timeout)
		view = ctrl.View(rt.gid)
	}
	m.emitHubConversation(rt.gid, view)
	if interrupted {
		// 队列继续由这一轮 run 真正收尾后的 hubOnRunEnd 驱动（与停止同一条路径）。
		// **不能**在这里补泵：中断是异步的，泵会赶在被中断的 run 收尾之前把下一条
		// wake 投出去，两个 run 共用一个「每目标最多一条 running」的账本记录。
		return
	}
	// 没有可中断的会话（run 已经在闸门前收尾）：账本已经收口，泵一次让队列继续。
	m.scheduleHubConversationPump(rt.gid)
}

// notifyHubRunTimeout 写一条系统说明：谁、超时多久、队列继续（不静默中断）。
func (m *manager) notifyHubRunTimeout(gid agenthub.GroupID, agent agenthub.AgentID, timeout time.Duration) {
	st, err := m.hubStore()
	if err != nil {
		return
	}
	name := agentDisplayName(context.Background(), st, agent)
	text := fmt.Sprintf("「%s」这一轮跑了 %s 还没结束，已按单轮上限中断；队列继续",
		name, hubRunTimeoutText(timeout))
	m.appendHubSystemMessage(gid, text)
}

// hubRunTimeoutText 把超时上限说成人话（测试里常注入 100ms 级的上限）。
func hubRunTimeoutText(d time.Duration) string {
	if d <= 0 {
		return "上限时间"
	}
	if d < time.Second {
		return "不到 1 秒"
	}
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()+0.5))
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes()+0.5))
}

// appendHubSystemMessage 往群日志写一条系统消息并广播（停止/超时/等待人类共用）。
// 失败只告警：这些是说明性记录，不能反过来让停止/超时动作失败。
func (m *manager) appendHubSystemMessage(gid agenthub.GroupID, text string) {
	text = strings.TrimSpace(text)
	if m == nil || gid == "" || text == "" {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		m.hubAlertf("group_conversation_notice_failed", "群 %s 系统说明写入失败：%v", gid, err)
		return
	}
	msg := &agenthub.Message{
		GroupID: gid, Kind: agenthub.KindSystem,
		Sender: agenthub.Sender{Kind: agenthub.SenderSystem}, Text: text,
	}
	seq, err := agenthub.AppendGroupMessage(context.Background(), st, msg)
	if err != nil {
		m.hubAlertf("group_conversation_notice_failed", "群 %s 系统说明写入失败：%v", gid, err)
		return
	}
	m.emitHubGroupMessage(gid, seq, msg)
}

// hubGroupSessionIfLoaded 取**已装配**的 (group, agent) 群会话，不为它凭空装配：
// 停止/超时只关心正在跑的那一个，给空闲成员装配会话是白花钱。
func (m *manager) hubGroupSessionIfLoaded(gid agenthub.GroupID, agent agenthub.AgentID) *bridgeSession {
	if m == nil || gid == "" || agent == "" {
		return nil
	}
	for _, bs := range m.groupSessions(gid) {
		if bs == nil || bs.hubOnRunEnd == nil {
			continue // 只认群会话（DM 没有 hubOnRunEnd）
		}
		if bs.hubDeltaAgentID == string(agent) {
			return bs
		}
	}
	return nil
}

// interruptGroupAgent 只中断某成员在该群的群 run，返回是否真的打断了什么。
// 调用方必须持有该群的 hubConvDispatch 闸门（与停止/投递互斥）。
func (m *manager) interruptGroupAgent(gid agenthub.GroupID, agent agenthub.AgentID) bool {
	bs := m.hubGroupSessionIfLoaded(gid, agent)
	if bs == nil || !groupSessionActive(bs) {
		return false
	}
	if bs.hubInterrupt != nil {
		bs.hubInterrupt()
	} else {
		bs.s.Interrupt()
	}
	return true
}
