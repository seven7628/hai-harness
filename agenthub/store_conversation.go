package agenthub

// store_conversation.go：每群对话状态的持久化（P6-09，§1.6）。
//
// 布局：conversations/<gid>.json（tmp+rename 原子写，与其它实体一致）。
//
// 为什么单独一个文件而不是塞进 group.json：对话状态按**代**变化（人类每句话换代），
// 写频率比群元数据高一个量级；混在一起会让「改名/改策略」这类低频写和对话写互相
// 覆盖，也会把 group.json 撑大。
//
// 兼容旧数据：文件不存在 = 空对话（ErrNotFound 由调用方当成「没有」）；
// 坏文件不覆盖、不猜测 —— 直接报 ErrCorrupt，由上层告警并跳过这一群。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ConversationStateSchema 是对话状态文件的版本号。改动字段语义时必须 +1，
// 旧版本读不出来就按坏文件处理（宁可告警也不猜）。
const ConversationStateSchema = 1

// ConversationQueueEntry 是队列里一条待投递/已投递未完成的 wake。
// 只存协议字段：正文/附件在群日志里（按 TriggerSeq 取回），避免把图片 data URL
// 复制进对话状态文件。
type ConversationQueueEntry struct {
	WakeID     string     `json:"wake_id"`
	Target     AgentID    `json:"target"`
	Sender     AgentID    `json:"sender,omitempty"`
	Human      bool       `json:"human,omitempty"`
	Depth      int        `json:"depth,omitempty"`
	Turn       int        `json:"turn,omitempty"`
	TriggerSeq int64      `json:"trigger_seq,omitempty"`
	RoundID    string     `json:"round_id,omitempty"`
	EnqueuedAt time.Time  `json:"enqueued_at,omitempty"`
	ReadyAt    *time.Time `json:"ready_at,omitempty"`
	// DeliveryStatus 是投递账本口径（P6-01）：queued = 还没投；running = 已交给目标
	// 会话但没跑完（重启后一律标 interrupted，绝不重放）。
	DeliveryStatus DeliveryStatus `json:"delivery_status"`
	Reason         string         `json:"reason,omitempty"`
	Attempts       int            `json:"attempts,omitempty"`
}

// ConversationPairState 是一对（无序）agent 的成功投递时间戳，供滚动窗口过期。
type ConversationPairState struct {
	// Key 是 ConversationPairKey 的口径：`a↔b`（无序）。
	Key string      `json:"key"`
	At  []time.Time `json:"at,omitempty"`
}

// ConversationPrintState 是一对 agent 见过的归一化正文指纹（有上限）。
type ConversationPrintState struct {
	Key    string   `json:"key"`
	Prints []string `json:"prints,omitempty"`
}

// ConversationState 是一代对话的持久化快照。字段与 conversation 一一对应，
// 只多一个 SchemaVersion 与 SavedAt（排障用）。
type ConversationState struct {
	SchemaVersion  int                      `json:"schema_version"`
	GroupID        GroupID                  `json:"group_id"`
	ConversationID string                   `json:"conversation_id"`
	HumanSeq       int64                    `json:"human_seq"`
	Status         ConversationStatus       `json:"status"`
	StopReason     string                   `json:"stop_reason,omitempty"`
	Turns          int                      `json:"turns"`
	Depth          int                      `json:"depth"`
	StartedAt      time.Time                `json:"started_at,omitempty"`
	UpdatedAt      time.Time                `json:"updated_at,omitempty"`
	LastWakeAt     time.Time                `json:"last_wake_at,omitempty"`
	Queue          []ConversationQueueEntry `json:"queue,omitempty"`
	Pairs          []ConversationPairState  `json:"pairs,omitempty"`
	Prints         []ConversationPrintState `json:"prints,omitempty"`
	SavedAt        time.Time                `json:"saved_at,omitempty"`
}

// ConversationPersister 是对话状态的落盘点。FileStore 实现；测试可注入内存实现。
// nil = 不持久化（单元测试/评测）。
type ConversationPersister interface {
	SaveConversationState(ctx context.Context, st *ConversationState) error
	LoadConversationState(ctx context.Context, gid GroupID) (*ConversationState, error)
	// ListConversationGroups 返回有状态文件的群（启动恢复用）。
	ListConversationGroups(ctx context.Context) ([]GroupID, error)
	DeleteConversationState(ctx context.Context, gid GroupID) error
}

var _ ConversationPersister = (*FileStore)(nil)

func (s *FileStore) conversationPath(gid GroupID) string {
	return filepath.Join(s.root, "conversations", string(gid)+".json")
}

// SaveConversationState 原子写一份对话状态。GroupID 必须合法（防路径穿越）。
func (s *FileStore) SaveConversationState(ctx context.Context, st *ConversationState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if st == nil || !validID(string(st.GroupID)) {
		return fmt.Errorf("%w: conversation state", ErrInvalid)
	}
	cp := *st
	cp.SchemaVersion = ConversationStateSchema
	cp.SavedAt = time.Now().UTC()
	return writeJSONAtomic(s.conversationPath(cp.GroupID), &cp)
}

// LoadConversationState 读一份对话状态。文件不存在返回 ErrNotFound（= 空对话）；
// JSON 坏了返回 ErrCorrupt（调用方不许覆盖）。
func (s *FileStore) LoadConversationState(ctx context.Context, gid GroupID) (*ConversationState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(gid)) {
		return nil, fmt.Errorf("%w: conversation group", ErrInvalid)
	}
	var st ConversationState
	if err := readJSON(s.conversationPath(gid), &st); err != nil {
		return nil, err
	}
	if st.GroupID == "" {
		st.GroupID = gid
	}
	if st.GroupID != gid {
		return nil, fmt.Errorf("%w: conversation %s 里写的是 %s", ErrCorrupt, gid, st.GroupID)
	}
	if st.SchemaVersion > ConversationStateSchema {
		return nil, fmt.Errorf("%w: conversation schema %d > %d", ErrUnsupportedSchema, st.SchemaVersion, ConversationStateSchema)
	}
	return &st, nil
}

// ListConversationGroups 列出有状态文件的群（忽略临时文件/非 json）。
func (s *FileStore) ListConversationGroups(ctx context.Context) ([]GroupID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, "conversations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // 还没写过任何对话状态：空
		}
		return nil, err
	}
	out := make([]GroupID, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		gid := GroupID(strings.TrimSuffix(name, ".json"))
		if !validID(string(gid)) {
			continue
		}
		out = append(out, gid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// DeleteConversationState 删掉一份对话状态（群已删除时清理）。文件不存在不算错。
func (s *FileStore) DeleteConversationState(ctx context.Context, gid GroupID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID(string(gid)) {
		return fmt.Errorf("%w: conversation group", ErrInvalid)
	}
	if err := os.Remove(s.conversationPath(gid)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
