package agenthub

// handoff.go：群内交接 + ack + 护栏（P3a-06，§6.6.5/§6.9）。
//
// 护栏（与 GroupPolicy 默认值一致）：
//   - 深度：ChainDepth >= group.policy.max_handoff_depth（默认 3）拒绝；
//   - 同一对 (from→to) 10 分钟内最多 2 次；第 3 次拒绝并进 Needs you；
//   - observer 不能发起/接收交接（只读旁听）；
//   - 交接必须 ack：pending_ack → accepted/declined；同一 need 只能处理一次（幂等）。
//
// 本文件只做纯判定 + 状态迁移，落盘/事件由 bridge 负责。

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrHandoffTooDeep     = errors.New("hub handoff: depth limit reached")
	ErrHandoffRateLimited = errors.New("hub handoff: pair rate limited")
	ErrHandoffObserver    = errors.New("hub handoff: observer cannot hand off")
	ErrHandoffNotPending  = errors.New("hub handoff: not pending ack")
)

// HandoffStatus 交接状态。
type HandoffStatus string

const (
	HandoffPendingAck HandoffStatus = "pending_ack"
	HandoffAccepted   HandoffStatus = "accepted"
	HandoffDeclined   HandoffStatus = "declined"
)

// Handoff 一次交接（群内 A→B）。
type Handoff struct {
	ID          string        `json:"id"`
	GroupID     GroupID       `json:"group_id"`
	FromAgentID AgentID       `json:"from_agent_id"`
	ToAgentID   AgentID       `json:"to_agent_id"`
	TaskID      TaskID        `json:"task_id,omitempty"`
	Instruction string        `json:"instruction"`
	Status      HandoffStatus `json:"status"`
	ChainDepth  int           `json:"chain_depth"`
	CreatedAt   time.Time     `json:"created_at"`
	AckedAt     *time.Time    `json:"acked_at,omitempty"`
}

// HandoffGuardLimits 覆盖群默认值（零值用 GroupPolicy 的默认/显式值）。
type HandoffGuardLimits struct {
	MaxDepth   int
	PairWindow time.Duration
	PairLimit  int
}

const (
	DefaultHandoffPairWindow = 10 * time.Minute
	DefaultHandoffPairLimit  = 2
)

// CheckHandoff 判定是否允许一次 A→B 交接。history 可传全量，内部只数同对且在窗口内的。
func CheckHandoff(g *Group, from, to AgentID, fromRole, toRole MemberRole, depth int, history []Handoff, now time.Time) error {
	if g == nil {
		return fmt.Errorf("%w: group is nil", ErrInvalid)
	}
	if fromRole == RoleObserver || toRole == RoleObserver {
		return fmt.Errorf("%w: observer（%s → %s）", ErrHandoffObserver, fromRole, toRole)
	}
	if from == "" || to == "" || from == to {
		return fmt.Errorf("%w: handoff 需要两个不同的智能体", ErrInvalid)
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
		if h.GroupID != "" && h.GroupID != g.ID {
			continue
		}
		if h.FromAgentID != from || h.ToAgentID != to {
			continue
		}
		if h.CreatedAt.Before(now.Add(-window)) {
			continue
		}
		count++
	}
	if count >= DefaultHandoffPairLimit {
		return fmt.Errorf("%w: %s → %s 在 %s 内已有 %d 次", ErrHandoffRateLimited, from, to, window, count)
	}
	return nil
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
