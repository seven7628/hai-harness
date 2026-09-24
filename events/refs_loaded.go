package events

import "time"

// RefsLoadedType 用户在消息中用 @ 引用的文件/目录已由 AgentHarness 层展开（或未找到/受保护）。
// 在每次 LLM 请求前批量发出一次（同一消息的多个引用合并成一个事件），
// 客户端据此在对话页用户消息块下方展示「已加载 …」状态。
const RefsLoadedType EventType = "refs_loaded"

// RefsLoaded 一次 @ 引用展开结果的批量事件。
// Items 为同一请求中全部引用的判定结果（保持输入顺序，去重）。
type RefsLoaded struct {
	// SessionId 会话 id（供事件日志恢复时定位会话）。
	SessionId string `json:"session_id,omitempty"`
	// RunId 触发本次展开的 agent 运行 id（关联事件流）。
	RunId string `json:"run_id,omitempty"`
	// Items 引用结果列表（批量；空 = 本次无引用展开，可不下发）。
	Items []RefLoadedItem `json:"items"`
	// Timestamp 事件时间。
	Timestamp time.Time `json:"timestamp"`
	EventType EventType `json:"event_type"`
}

// RefStatus 单个 @ 引用的展开状态。
type RefStatus string

const (
	// RefStatusLoaded 展开成功（文件内容 / 目录清单已注入请求）。
	RefStatusLoaded RefStatus = "loaded"
	// RefStatusMissing 目标不存在（文件/目录未找到）——保留原文。
	RefStatusMissing RefStatus = "missing"
	// RefStatusBlocked 受保护（敏感路径 / 工作区外 / 无工作区无法解析）——保留原文。
	RefStatusBlocked RefStatus = "blocked"
)

// RefLoadedItem 单个 @ 引用的展开结果。
type RefLoadedItem struct {
	// Path 用户输入的引用原文（含 @，如 @./docs/text.txt）。
	Path string `json:"path"`
	// Status 展开状态（loaded / missing / blocked）。
	Status RefStatus `json:"status"`
	// Resolved 解析后的绝对路径（loaded 时可用；missing/blocked 为空）。
	Resolved string `json:"resolved,omitempty"`
	// Kind 引用目标类型：file / dir（仅 loaded）。
	Kind string `json:"kind,omitempty"`
}

// Type 实现 events.Event。
func (r *RefsLoaded) Type() EventType { return RefsLoadedType }
