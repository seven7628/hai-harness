package agenthub

// handoff.go：群内交接 + ack + 护栏（P3a-06，§6.6.5/§6.9）。
//
// 护栏（与 GroupPolicy 默认值一致）：
//   - 深度：ChainDepth >= group.policy.max_handoff_depth（默认 3）拒绝；
//   - 同一对 (from→to) 10 分钟内最多 2 次；第 3 次拒绝并进 Needs you；
//   - 交接必须 ack：pending_ack → accepted/declined；同一 need 只能处理一次（幂等）。
//
// 角色只剩 owner/member（P6-12）：不再有「observer 不能交接」的特判 —— 交接双方
// 只要是群成员即可；「只读」由群策略 readonly 管，不由角色管。
//
// 本文件做纯判定 + 状态迁移；落盘/交接卡/ack 工具在 bridge（hub_handoff.go）。

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrHandoffTooDeep     = errors.New("hub handoff: depth limit reached")
	ErrHandoffRateLimited = errors.New("hub handoff: pair rate limited")
	ErrHandoffNotPending  = errors.New("hub handoff: not pending ack")
)

// HandoffStatus 交接状态。
type HandoffStatus string

const (
	HandoffPendingAck HandoffStatus = "pending_ack"
	HandoffAccepted   HandoffStatus = "accepted"
	HandoffDeclined   HandoffStatus = "declined"
	HandoffCompleted  HandoffStatus = "completed"
	HandoffExpired    HandoffStatus = "expired"
)

// Handoff 一次交接（群内 A→B）。
type Handoff struct {
	ID          string        `json:"id"`
	GroupID     GroupID       `json:"group_id"`
	FromAgentID AgentID       `json:"from_agent_id"`
	ToAgentID   AgentID       `json:"to_agent_id"`
	TaskID      TaskID        `json:"task_id,omitempty"`
	Instruction string        `json:"instruction"`
	ArtifactIDs []string      `json:"artifact_ids,omitempty"`
	Status      HandoffStatus `json:"status"`
	ChainDepth  int           `json:"chain_depth"`
	Reason      string        `json:"reason,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	AckedAt     *time.Time    `json:"ack_at,omitempty"`
}

const (
	DefaultHandoffPairWindow = 10 * time.Minute
	DefaultHandoffPairLimit  = 2
)

// CheckHandoff 判定是否允许一次 A→B 交接。角色从 g.Members 解析，调用方不能自报；
// 非成员/已删群 fail-closed。history 可传全量，内部只数同群、同无序对且在窗口内的。
func CheckHandoff(g *Group, from, to AgentID, depth int, history []Handoff, now time.Time) error {
	if g == nil {
		return fmt.Errorf("%w: group is nil", ErrInvalid)
	}
	if g.Deleted {
		return fmt.Errorf("%w: group %s 已删除", ErrNotFound, g.ID)
	}
	if from == "" || to == "" || from == to {
		return fmt.Errorf("%w: handoff 需要两个不同的智能体", ErrInvalid)
	}
	if _, ok := g.RoleOf(from); !ok {
		return fmt.Errorf("%w: %s 不是群成员", ErrInvalid, from)
	}
	if _, ok := g.RoleOf(to); !ok {
		return fmt.Errorf("%w: %s 不是群成员", ErrInvalid, to)
	}
	if depth < 0 {
		return fmt.Errorf("%w: depth %d", ErrInvalid, depth)
	}
	maxDepth := g.Policy.MaxHandoffDepth
	if maxDepth <= 0 {
		maxDepth = DefaultGroupMaxHandoffDepth
	}
	if depth >= maxDepth {
		return fmt.Errorf("%w: %d >= %d", ErrHandoffTooDeep, depth, maxDepth)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	window := DefaultHandoffPairWindow
	count := 0
	for _, h := range history {
		if h.GroupID != g.ID {
			continue // 空 GroupID 也不计入，避免跨群污染
		}
		samePair := (h.FromAgentID == from && h.ToAgentID == to) ||
			(h.FromAgentID == to && h.ToAgentID == from)
		if !samePair {
			continue
		}
		if h.CreatedAt.Before(now.Add(-window)) || h.CreatedAt.After(now) {
			continue // 窗口外/未来时间都不占额
		}
		count++
	}
	if count >= DefaultHandoffPairLimit {
		return fmt.Errorf("%w: %s ↔ %s 在 %s 内已有 %d 次", ErrHandoffRateLimited, from, to, window, count)
	}
	return nil
}

// NextHandoffDepth 从历史推导本次交接的链深度：0 = 首跳，A→B→C→D 后 depth=3。
func NextHandoffDepth(history []Handoff, from AgentID) int {
	depth := 0
	for _, h := range history {
		if h.ToAgentID != from || h.Status == HandoffDeclined || h.Status == HandoffExpired {
			continue
		}
		if h.ChainDepth+1 > depth {
			depth = h.ChainDepth + 1
		}
	}
	return depth
}

// CanTransitionHandoff pending_ack → accepted/declined；其它迁移非法（重复决策幂等返回原状）。
func (h *Handoff) CanTransitionHandoff(to HandoffStatus) bool {
	if h == nil {
		return false
	}
	if h.Status == to {
		return true
	}
	return h.Status == HandoffPendingAck && (to == HandoffAccepted || to == HandoffDeclined)
}

// Ack 处理交接决策（幂等：同状态重复调用不报错）。
func (h *Handoff) Ack(accept bool, at time.Time) error {
	if h == nil {
		return fmt.Errorf("%w: handoff is nil", ErrInvalid)
	}
	to := HandoffDeclined
	if accept {
		to = HandoffAccepted
	}
	if h.Status == to {
		return nil
	}
	// 零值反序列化/构造函数遗漏按 pending_ack 处理，避免「已存在但永远不能 ack」。
	if h.Status == "" && (to == HandoffAccepted || to == HandoffDeclined) {
		h.Status = HandoffPendingAck
	}
	if !h.CanTransitionHandoff(to) {
		return fmt.Errorf("%w: %s -> %s", ErrHandoffNotPending, h.Status, to)
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	h.Status = to
	h.AckedAt = &at
	return nil
}
