package events

import "time"

type ContentChunk struct {
	RunId     string `json:"run_id,omitempty"`     // 所属运行（事件自包含，多并发 agent 区分归属）
	RequestId string `json:"request_id,omitempty"` // 所属请求（多轮运行时区分轮次）
	// Index 该请求内 content 块序号（0 起自增，provider 层生成）：
	// 流式渲染顺序、丢块检测、首字耗时定位（首个块时间戳 = TTFT）。
	Index   int64  `json:"index"`
	Content string `json:"content"`

	EventType EventType `json:"event_type"`

	Timestamp time.Time `json:"timestamp"`
}

func (c *ContentChunk) Type() EventType {
	return c.EventType
}

type ReasoningChunk struct {
	RunId     string `json:"run_id,omitempty"`     // 所属运行（事件自包含，多并发 agent 区分归属）
	RequestId string `json:"request_id,omitempty"` // 所属请求（多轮运行时区分轮次）
	// Index 该请求内 reasoning 块序号（0 起自增，与 content 独立计数）。
	Index   int64  `json:"index"`
	Content string `json:"content"`

	EventType EventType `json:"event_type"`
	Timestamp time.Time `json:"timestamp"`
}

func (r *ReasoningChunk) Type() EventType {
	return r.EventType
}
