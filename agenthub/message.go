package agenthub

import (
	"fmt"
	"time"
)

// SenderKind 是消息发送者的类型。
type SenderKind string

const (
	SenderHuman  SenderKind = "human"
	SenderAgent  SenderKind = "agent"
	SenderSystem SenderKind = "system"
)

// Sender 是消息作者。agent 消息必须有 AgentID，human/system 没有。
type Sender struct {
	Kind    SenderKind `json:"kind"`
	AgentID AgentID    `json:"agent_id,omitempty"`
}

// Intent 是消息意图；用于 UI 与护栏，不替代正文。
type Intent string

const (
	IntentChat    Intent = "chat"
	IntentAssign  Intent = "assign"
	IntentReport  Intent = "report"
	IntentAsk     Intent = "ask"
	IntentHandoff Intent = "handoff"
)

// MessageKind 是消息的展示/语义类型。activity 不在主日志里，故不在此列。
type MessageKind string

const (
	KindText         MessageKind = "text"
	KindTaskResult   MessageKind = "task_result"
	KindArtifact     MessageKind = "artifact"
	KindHandoff      MessageKind = "handoff"
	KindApprovalStub MessageKind = "approval_stub"
	KindRouting      MessageKind = "routing_receipt"
	KindTaskUpdate   MessageKind = "task_update"
	KindRoundSummary MessageKind = "round_summary"
	KindRoutineCard  MessageKind = "routine_card"
	KindSystem       MessageKind = "system"
)

// Visibility 控制消息是否只对人类/群主可见。
type Visibility string

const (
	VisibilityGroup Visibility = "group"
	VisibilityOwner Visibility = "owner"
)

// AgentRef 是给 UI 用的反规范化智能体信息（历史消息在改名/删除后仍可渲染）。
type AgentRef struct {
	AgentID AgentID `json:"agent_id"`
	Name    string  `json:"name"`
	Avatar  Avatar  `json:"avatar"`
	Accent  string  `json:"accent"`
}

// TurnContext 说明“这条回复是基于谁的消息产生的”，驱动 TurnContextLine。
// 它不是作者署名：作者永远只有一个。
type TurnContext struct {
	TriggerSeqs []int64    `json:"trigger_seqs"`     // 触发本轮的消息 seq
	Senders     []AgentRef `json:"senders"`          // 去重后的触发者
	Reason      string     `json:"reason,omitempty"` // multi_agent | paginated | interleaved
}

// Message 是群/DM 的一条持久化消息。
//
// Seq 只在群内有意义；DM 的索引与未读由 bridge 的 dm 索引维护。
// Context 只出现在回复消息上；流式草稿（hub_message_delta）不落成 Message。
type Message struct {
	Seq     int64        `json:"seq"`
	ID      string       `json:"id"`
	GroupID GroupID      `json:"group_id,omitempty"` // DM 为空
	RoundID string       `json:"round_id,omitempty"`
	Context *TurnContext `json:"context,omitempty"`
	Sender  Sender       `json:"sender"`
	Kind    MessageKind  `json:"kind"`
	Text    string       `json:"text,omitempty"`
	// Attachments 是这条消息的图片/文件引用。图片以 data URL 持久化 —— 前端刷新
	// 后仍能直接渲染，不用回读会话 jsonl；文件只是路径引用（后端不读内容）。
	Attachments []Attachment   `json:"attachments,omitempty"`
	Mentions    []AgentID      `json:"mentions,omitempty"`
	MentionAll  bool           `json:"mention_all,omitempty"`
	ReplyToSeq  int64          `json:"reply_to_seq,omitempty"`
	Intent      Intent         `json:"intent,omitempty"`
	TaskID      TaskID         `json:"task_id,omitempty"`
	Turn        int            `json:"turn,omitempty"`
	Visibility  Visibility     `json:"visibility"`
	Refs        map[string]any `json:"refs,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

// OwnerOnlyPlaceholder 是 owner-only 消息进入其他智能体上下文时的替身，绝不泄露正文。
const OwnerOnlyPlaceholder = "[仅群主可见的一条记录]"

// EffectiveVisibility 把空值归一到 group（§5.9：缺失 = 默认）。
func (v Visibility) EffectiveVisibility() Visibility {
	if v == "" {
		return VisibilityGroup
	}
	return v
}

// Validate 校验可见性枚举；空值按默认 group 处理。
func (v Visibility) Validate() error {
	switch v.EffectiveVisibility() {
	case VisibilityGroup, VisibilityOwner:
		return nil
	default:
		return fmt.Errorf("%w: visibility must be group or owner", ErrInvalid)
	}
}

// AgentContextKind 判断消息类型是否允许进入智能体上下文/工具视图。
// 回执、轮次摘要与审批占位只是人类 UI 的投影，正文可能含敏感信息，统一排除。
func (m Message) AgentContextKind() bool {
	switch m.Kind {
	case KindRouting, KindRoundSummary, KindApprovalStub:
		return false
	default:
		return true
	}
}

// VisibleTo 判断某条消息能否进入读方（人/智能体）的视图。
// 智能体走白名单：只有空值/group 可见；owner 与任何非法值都 fail-closed。
func (m Message) VisibleTo(isHuman bool) bool {
	if isHuman {
		return true
	}
	return m.Visibility.EffectiveVisibility() == VisibilityGroup
}

// ContextText 返回该消息进入上下文时的正文：不可见时只给占位。
func (m Message) ContextText(isHuman bool) string {
	return m.Redacted(isHuman).Text
}

// Redacted 返回进入某读方上下文的副本：不可见时替换正文并清空 refs/mentions/context，
// 防止 P1-09 直接 json.Marshal(Message) 绕过 ContextText 泄露 owner-only 元数据。
func (m Message) Redacted(isHuman bool) Message {
	if m.VisibleTo(isHuman) {
		return m
	}
	m.Text = OwnerOnlyPlaceholder
	m.Attachments = nil // owner-only 附件（图片 data URL/文件名）同样不能泄给其他读方
	m.Refs = nil
	m.Mentions = nil
	m.MentionAll = false
	m.Context = nil
	m.TaskID = ""
	m.ReplyToSeq = 0
	m.Intent = ""
	return m
}

// AgentToolView 返回消息进入模型**工具结果**的副本：可见性走 Redacted，图片附件只留
// 元数据（kind/name/mime/bytes），不带 data URL。
//
// 工具结果是文本：一张 2.5MiB 的图带上 base64 就是 ~3.3MiB（约 80 万 token）塞进
// 下一轮请求，既看不见图，也可能被 provider 直接拒绝。图片真正进上下文的唯一路径
// 是 wake 的 image 内容块，不是 group_history 的 JSON。
func (m Message) AgentToolView() Message {
	m = m.Redacted(false)
	if len(m.Attachments) == 0 {
		return m
	}
	atts := make([]Attachment, len(m.Attachments))
	for i, a := range m.Attachments {
		a.DataURL = ""
		atts[i] = a
	}
	m.Attachments = atts
	return m
}
