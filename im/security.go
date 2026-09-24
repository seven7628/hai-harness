package im

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Security 安全模型（手机远程 = 高危面，docs/IM_INTEGRATION.md §6）。
//
// 六道防线：
//  1. 主开关（Config.Enabled，默认关）
//  2. 授权名单（AllowUsers per-gateway，空 = 全拒绝）
//  3. 绑定确认（RequireBindConfirm：陌生 Chat 首次发消息需桌面端确认）
//  4. 能力收口（IM 只能 Ask/Approve/Interrupt/状态查询；控制面只在桌面端——宿主层强制）
//  5. 审计（谁在哪个 Chat 驱动了什么、审批决策 → im-audit.jsonl）
//  6. 消息校验（平台侧签名/加密，见各 Gateway 实现）
type Security struct {
	mu    sync.Mutex
	cfg   Config // 完整配置（授权名单在 Gateway 级）
	audit *auditLogger
	// 已确认绑定的 Chat（运行期内存缓存：确认后本进程内不再重复弹窗）
	confirmed map[string]bool
}

// NewSecurity 创建安全校验器。
func NewSecurity(cfg Config, auditPath string) *Security {
	return &Security{
		cfg:       cfg,
		audit:     newAuditLogger(auditPath),
		confirmed: make(map[string]bool),
	}
}

// Update 热更新安全配置。
func (s *Security) Update(cfg Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// UserAllowed 授权名单校验：该 Gateway 下用户是否允许。
// 名单为空 = 全拒绝（安全默认）。
func (s *Security) UserAllowed(gateway, userID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.UserAllowed(gateway, userID)
}

// ChatConfirmed 该 Chat 是否已确认绑定（本进程内）。
func (s *Security) ChatConfirmed(chat Chat) bool {
	c, _ := chat.Normalize()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.confirmed[c.String()]
}

// ConfirmChat 标记 Chat 已确认绑定（绑定确认弹窗用户点"确认"后调用）。
func (s *Security) ConfirmChat(chat Chat) {
	c, _ := chat.Normalize()
	s.mu.Lock()
	s.confirmed[c.String()] = true
	s.mu.Unlock()
}

// ShouldRequireConfirm 该 Chat 是否需要绑定确认（主开关开启时）。
// 已确认的 Chat 返回 false。
func (s *Security) ShouldRequireConfirm(chat Chat) bool {
	s.mu.Lock()
	need := s.cfg.Security.RequireBindConfirm
	s.mu.Unlock()
	if !need {
		return false
	}
	return !s.ChatConfirmed(chat)
}

// Audit 记录审计事件（驱动/审批/拒绝等）。失败静默（审计不阻断功能）。
func (s *Security) Audit(entry AuditEntry) {
	s.audit.log(entry)
}

// AuditEntry 审计记录。
type AuditEntry struct {
	Time      time.Time `json:"time"`
	Gateway   string    `json:"gateway"`
	Chat      string    `json:"chat,omitempty"`
	UserID    string    `json:"user_id,omitempty"`
	Action    string    `json:"action"` // message_received | route_miss | bind_confirmed | approval_decided | message_sent
	Detail    string    `json:"detail,omitempty"`
	Allowed   bool      `json:"allowed,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
}

// auditLogger 追加写 JSONL 审计日志（~/.go-code/im-audit.jsonl）。
type auditLogger struct {
	mu   sync.Mutex
	path string
}

func newAuditLogger(path string) *auditLogger {
	return &auditLogger{path: path}
}

func (l *auditLogger) log(entry AuditEntry) {
	if l == nil || l.path == "" {
		return
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now()
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

// ValidateUserID 简单校验用户标识非空（防注入空值绕过名单）。
func ValidateUserID(userID string) bool {
	return strings.TrimSpace(userID) != ""
}

// String 审计记录摘要（日志打印）。
func (e AuditEntry) String() string {
	return fmt.Sprintf("%s %s %s@%s %s", e.Time.Format(time.RFC3339), e.Action, e.UserID, e.Gateway, e.Detail)
}
