package main

// hub_conversation_persist.go：对话状态的落盘接线与重启恢复（P6-09）。
//
// 状态文件由 agenthub.FileStore 原子写（conversations/<gid>.json），写时机在控制器
// 里（状态变化才写，不按 token）。这里只负责三件事：
//   - 给控制器一个 persister（懒取 hubStore：控制器第一次被用到时存储可能还没开）；
//   - 启动时把磁盘状态装回控制器：未投递的恢复排队、已投递未完成的标 interrupted
//     并写系统说明、群已删除的清掉状态文件、坏文件告警且**不覆盖**；
//   - 群删除时清掉它的状态文件。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// hubConversationPersister 把对话状态落到 Hub 存储。store 每次懒取：控制器在第一次
// 用到对话时构造，那时 hubStore 可能还没打开。
type hubConversationPersister struct{ m *manager }

func (p hubConversationPersister) store() (agenthub.ConversationPersister, error) {
	if p.m == nil {
		return nil, fmt.Errorf("%w: manager 不可用", agenthub.ErrInvalid)
	}
	st, err := p.m.hubStore()
	if err != nil {
		return nil, err
	}
	return st, nil
}

func (p hubConversationPersister) SaveConversationState(ctx context.Context, st *agenthub.ConversationState) error {
	s, err := p.store()
	if err != nil {
		return err
	}
	return s.SaveConversationState(ctx, st)
}

func (p hubConversationPersister) LoadConversationState(ctx context.Context, gid agenthub.GroupID) (*agenthub.ConversationState, error) {
	s, err := p.store()
	if err != nil {
		return nil, err
	}
	return s.LoadConversationState(ctx, gid)
}

func (p hubConversationPersister) ListConversationGroups(ctx context.Context) ([]agenthub.GroupID, error) {
	s, err := p.store()
	if err != nil {
		return nil, err
	}
	return s.ListConversationGroups(ctx)
}

func (p hubConversationPersister) DeleteConversationState(ctx context.Context, gid agenthub.GroupID) error {
	s, err := p.store()
	if err != nil {
		return err
	}
	return s.DeleteConversationState(ctx, gid)
}

// restoreHubConversations 把磁盘上的群对话状态装回控制器（P6-09 重启恢复）。
// 幂等：一个 manager 只跑一次。失败只告警 —— 恢复不了也不能让 bridge 起不来。
func (m *manager) restoreHubConversations() {
	if m == nil || m.hubShutdown.Load() {
		return
	}
	m.hubRestoreOnce.Do(func() {
		st, err := m.hubStore()
		if err != nil {
			m.hubAlertf("hub_conversation_restore_failed", "读取群对话状态失败：%v", err)
			return
		}
		gids, err := st.ListConversationGroups(context.Background())
		if err != nil {
			m.hubAlertf("hub_conversation_restore_failed", "列出群对话状态失败：%v", err)
			return
		}
		ctrl := m.hubConversation()
		for _, gid := range gids {
			m.restoreHubConversation(ctrl, st, gid)
		}
	})
}

// restoreHubConversation 恢复一个群的对话状态。语义见 §1.6：
//   - 未投递 wake → 恢复排队（状态还是 running 就接着投）；
//   - 已投递未完成 → interrupted + 一条系统说明，**不自动重试副作用**；
//   - 已完成 → 不在状态文件里（终态记录不落盘），自然不重放；
//   - 群已删除 → 清理状态文件；
//   - 坏文件/过新版本 → 告警 + 该群不再写盘（不覆盖）。
func (m *manager) restoreHubConversation(ctrl *agenthub.ConversationController, st *agenthub.FileStore, gid agenthub.GroupID) {
	ctx := context.Background()
	state, err := st.LoadConversationState(ctx, gid)
	switch {
	case errors.Is(err, agenthub.ErrNotFound):
		return // 兼容旧数据：没有文件 = 空对话
	case errors.Is(err, agenthub.ErrCorrupt), errors.Is(err, agenthub.ErrUnsupportedSchema):
		// 坏文件不覆盖：标记该群不可写，之后的状态变化只告警。人工修好/删掉文件
		// 再重启即可恢复持久化。
		ctrl.BlockPersist(gid)
		m.hubAlertf("hub_conversation_corrupt",
			"群 %s 的对话状态文件损坏或不支持（%v）：已跳过恢复且不再写这个文件，请人工检查 conversations/%s.json",
			gid, err, gid)
		return
	case err != nil:
		m.hubAlertf("hub_conversation_restore_failed", "群 %s 的对话状态读取失败：%v", gid, err)
		return
	}
	g, gerr := st.LoadGroup(ctx, gid)
	if gerr != nil || g == nil || g.Deleted {
		// 群没了：状态文件没有意义，清掉（§1.6「群已删除 → 清理」）。
		if derr := ctrl.DeleteState(ctx, gid); derr != nil {
			m.hubAlertf("hub_conversation_restore_failed", "群 %s 的对话状态清理失败：%v", gid, derr)
		}
		return
	}
	// 正文/附件不在状态文件里：按 TriggerSeq 从群日志补回（一次读一页，建索引）。
	var bySeq map[int64]agenthub.Message
	load := func(e agenthub.ConversationQueueEntry) (string, []agenthub.Attachment) {
		if e.TriggerSeq <= 0 {
			return "", nil
		}
		if bySeq == nil {
			bySeq = map[int64]agenthub.Message{}
			if msgs, rerr := st.ReadMessages(ctx, gid, 0, 500); rerr == nil {
				for _, msg := range msgs {
					bySeq[msg.Seq] = msg
				}
			}
		}
		msg, ok := bySeq[e.TriggerSeq]
		if !ok {
			return "", nil
		}
		return msg.Text, msg.Attachments
	}
	rep := ctrl.RestoreWith(state, time.Time{}, load)
	if rep.Interrupted > 0 {
		names := make([]string, 0, len(rep.InterruptedTargets))
		for _, id := range rep.InterruptedTargets {
			names = append(names, agentDisplayName(ctx, st, id))
		}
		m.appendHubSystemMessage(gid, fmt.Sprintf(
			"Hub 重启时这一轮还没跑完：%s 的运行已标记中断（不会自动重试），队列继续",
			strings.Join(names, "、")))
	}
	if rep.Restored > 0 || rep.Interrupted > 0 {
		m.emitHubConversation(gid, ctrl.View(gid))
	}
	// 未投递的唤醒接着投（重启不该把队列悄悄丢掉）。B1：waiting_human 且队列非空
	// 同样要泵（软状态不是终态）。
	if rep.Restored > 0 && ctrl.View(gid).Schedulable() {
		m.scheduleHubConversationPump(gid)
	}
}

// deleteHubConversationState 群被删除时清掉它的对话状态（不阻断删除流程）。
func (m *manager) deleteHubConversationState(gid agenthub.GroupID) {
	if m == nil || gid == "" {
		return
	}
	ctrl := m.hubConversation()
	if err := ctrl.DeleteState(context.Background(), gid); err != nil && !errors.Is(err, agenthub.ErrNotFound) {
		m.hubAlertf("hub_conversation_delete_failed", "群 %s 的对话状态清理失败：%v", gid, err)
	}
}
