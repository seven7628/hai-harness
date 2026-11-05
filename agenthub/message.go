package agenthub

import "time"

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
	Seq        int64          `json:"seq"`
	ID         string         `json:"id"`
	GroupID    GroupID        `json:"group_id,omitempty"` // DM 为空
	RoundID    string         `json:"round_id,omitempty"`
	Context    *TurnContext   `json:"context,omitempty"`
	Sender     Sender         `json:"sender"`
	Kind       MessageKind    `json:"kind"`
	Text       string         `json:"text,omitempty"`
	Mentions   []AgentID      `json:"mentions,omitempty"`
	MentionAll bool           `json:"mention_all,omitempty"`
	ReplyToSeq int64          `json:"reply_to_seq,omitempty"`
	Intent     Intent         `json:"intent,omitempty"`
	TaskID     TaskID         `json:"task_id,omitempty"`
	Turn       int            `json:"turn,omitempty"`
	Visibility Visibility     `json:"visibility"`
	Refs       map[string]any `json:"refs,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}
