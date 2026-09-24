package im

import (
	"fmt"
	"sync"
	"time"
)

// ApprovalStatus 审批状态。
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
	ApprovalExpired  ApprovalStatus = "expired" // 超时自动拒绝
	ApprovalHandled  ApprovalStatus = "handled" // 已在别处处理（双通道）
)

// Approval 单个审批请求的 IM 侧状态。
type Approval struct {
	ID        string // 决策锚（= ToolCall.Id）
	SessionID string // 归属会话（回调解路由）
	Workspace string // 归属 workspace（回调解路由）
	Chat      Chat   // 发起 Chat
	MessageID string // 卡片消息 id（更新状态用）
	Name      string // 工具名
	Status    ApprovalStatus
	CreatedAt time.Time
	DecidedAt time.Time
	UserID    string // 决策者
	// 文本指令降级：卡片无按钮时，把"待决策"转成文本，用户回复数字
	TextMode bool
}

// ApprovalManager 审批状态机（IM 无关）：多待批堆叠、幂等、超时、双通道去重。
//
// 线程安全：内部互斥。
type ApprovalManager struct {
	mu        sync.Mutex
	pending   map[string]*Approval // id → 审批（id = ToolCall.Id 决策锚）
	timeout   time.Duration        // 超时（0 = 不超时，由 SDK 侧控制）
	onDecided func(a *Approval)    // 决策回调（宿主注入：调用 Session.Approve / 更新卡片）
}

// NewApprovalManager 创建审批管理器。timeout <= 0 表示不主动超时（SDK 侧已有超时拒绝）。
func NewApprovalManager(timeout time.Duration, onDecided func(a *Approval)) *ApprovalManager {
	return &ApprovalManager{
		pending:   make(map[string]*Approval),
		timeout:   timeout,
		onDecided: onDecided,
	}
}

// Register 注册一个审批请求（收到 ToolApprovalRequested 时调用）。
// 返回审批记录；若已存在（重复事件）返回已有记录。
func (m *ApprovalManager) Register(id, sessionID, workspace string, chat Chat, messageID, name string, textMode bool) *Approval {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.pending[id]; ok {
		return a
	}
	a := &Approval{
		ID:        id,
		SessionID: sessionID,
		Workspace: workspace,
		Chat:      chat,
		MessageID: messageID,
		Name:      name,
		Status:    ApprovalPending,
		CreatedAt: time.Now(),
		TextMode:  textMode,
	}
	m.pending[id] = a
	return a
}

// Get 查审批（幂等决策用）。
func (m *ApprovalManager) Get(id string) (*Approval, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.pending[id]
	return a, ok
}

// Decide 决策一个审批（approve=true 批准）。幂等：已决策返回 false（不重复回调）。
// 返回 (是否本次生效, 当前状态)。
func (m *ApprovalManager) Decide(id string, approve bool, userID string) (bool, ApprovalStatus) {
	m.mu.Lock()
	a, ok := m.pending[id]
	if !ok {
		m.mu.Unlock()
		return false, ApprovalHandled
	}
	if a.Status != ApprovalPending {
		st := a.Status
		m.mu.Unlock()
		return false, st // 已在别处处理（双通道）
	}
	if approve {
		a.Status = ApprovalApproved
	} else {
		a.Status = ApprovalRejected
	}
	a.DecidedAt = time.Now()
	a.UserID = userID
	cb := m.onDecided
	m.mu.Unlock()
	if cb != nil {
		cb(a)
	}
	return true, a.Status
}

// Expire 超时处理（返回是否本次生效）。
func (m *ApprovalManager) Expire(id string) bool {
	m.mu.Lock()
	a, ok := m.pending[id]
	if !ok || a.Status != ApprovalPending {
		m.mu.Unlock()
		return false
	}
	a.Status = ApprovalExpired
	a.DecidedAt = time.Now()
	cb := m.onDecided
	m.mu.Unlock()
	if cb != nil {
		cb(a)
	}
	return true
}

// PendingCount 当前待批数量（UI 徽标）。
func (m *ApprovalManager) PendingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, a := range m.pending {
		if a.Status == ApprovalPending {
			n++
		}
	}
	return n
}

// Pending 当前全部待批（排序：按创建时间）。
func (m *ApprovalManager) Pending() []*Approval {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Approval
	for _, a := range m.pending {
		if a.Status == ApprovalPending {
			out = append(out, a)
		}
	}
	// 稳定排序：先创建的在前
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.Before(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Cleanup 清理已终结的审批（防泄漏，可定期调用）。
func (m *ApprovalManager) Cleanup(maxAge time.Duration) {
	cut := time.Now().Add(-maxAge)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, a := range m.pending {
		if a.Status != ApprovalPending && a.DecidedAt.Before(cut) {
			delete(m.pending, id)
		}
	}
}

// String 审批摘要（日志/审计）。
func (a *Approval) String() string {
	return fmt.Sprintf("approval[%s] %s %s @%s (%s)", a.ID, a.Name, a.Status, a.Chat.String(), a.SessionID)
}
