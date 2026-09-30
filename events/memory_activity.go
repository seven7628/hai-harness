package events

import "time"

// MemoryActivity Project Memory 的用户可见活动（官方 §2.7，2026-09-30 补）。
//
// 官方原文：
//
//	"When you see messages like **'Saved 2 memories'** or **'Recalled 2
//	 memories'** in the Claude Code interface, Claude is actively updating or
//	 reading from `~/.claude/projects/<project>/memory/`."
//
// 这条事件就是那两行提示的数据源。它报的是**事实**（刚写了/读了哪个记忆文件），
// 不是文案：`Count` 与 `File` 由宿主如实填，"Saved 2 memories" 这种合并展示是
// 前端的事——把聚合放在 UI 而不是在宿主，是因为宿主没有"一次运行结束"的可靠
// 观测点，而在 UI 侧聚合同样能得到那句话，且不必让事件系统承担跨运行的聚合态。
//
// 为什么独立事件而不是挂 AgentEnd：AgentEnd 的字段语义是「本次运行的结算」，
// 且它已在重放/归档链路里被多处消费，加字段会牵动它们的假设（同 RunSilentEnd
// 的取舍理由）。
type MemoryActivity struct {
	// RunId 本次运行；与 agent_start / agent_end 同 RunId 关联。
	RunId string `json:"run_id"`
	// Scope 谁在动这份记忆：主会话还是子 agent。
	//
	// 子 agent 有自己独立的记忆目录（官方：enabled with the subagent `memory`
	// field, is a separate directory），所以两条 Scope 的活动必须能分开统计。
	Scope MemoryScope `json:"scope"`
	// Action 写（saved）还是读（recalled）。
	Action MemoryAction `json:"action"`
	// File 记忆文件名（如 relay-backoff.md）。不含目录。
	File string `json:"file"`
	// Count 本次事件的记忆条数。逐文件事件恒为 1；前端合并成 N 时自己累加。
	Count int `json:"count"`
	// Timestamp 发生时刻。
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

// MemoryScope 记忆活动的归属。
type MemoryScope string

const (
	// MemoryScopeMain 主会话的 auto memory。
	MemoryScopeMain MemoryScope = "main"
	// MemoryScopeSubagent 子 agent 自己的独立 auto memory。
	MemoryScopeSubagent MemoryScope = "subagent"
)

// MemoryAction 记忆活动是读还是写。
type MemoryAction string

const (
	// MemoryActionSaved 模型写/改了一条记忆（官方 "Saved N memories"）。
	MemoryActionSaved MemoryAction = "saved"
	// MemoryActionRecalled 模型读了一条记忆（官方 "Recalled N memories"）。
	MemoryActionRecalled MemoryAction = "recalled"
)

func (e *MemoryActivity) Type() EventType { return e.EventType }
