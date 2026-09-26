package events

import "time"

// MemoryProbeType memory 命中后重复探查的读数事件（Phase A 收尾，纯可观测性）。
//
// 与 RunSilentEnd 同理：独立事件而非给 AgentEnd 加字段 —— 要的是**可独立聚合的
// 计数**，而 AgentEnd 的字段语义是「本次运行的结算」，且它已在重放/归档链路里被
// 多处消费。独立事件只需在此登记，未知类型在重放侧安全跳过。
//
// 用途：量化「模型在 memory_view 命中后是否仍重复探查同一件事」。真实评估中曾
// 观察到命中后仍连续 bash/read_file，但缺少读数就无法判断提示词改动是否真的
// 降低了重复劳动。
//
// 口径（读数定义，实现见 agents/memory_probe.go）：
//   - Views = 本 Run 成功的 memory_view 次数；为 0 时事件不发出（零噪声）；
//   - PostHitProbeCalls 只统计**先于本批已成功命中**的探查调用
//     （bash/read_file/grep/glob/run_python）。同一批内与 memory_view 一同
//     发起的调用不计入 —— 模型在看到记忆内容之前就发起了它们，计入会把并发
//     误判成不信任；
//   - memory_write 不算探查：它是记忆维护动作。
//
// 语义边界：这是观测，不是失败。不阻断、不改写、不注入提醒，也不影响 IsError、
// FinishReason 或任何 AgentEnd 字段。
type MemoryProbe struct {
	RunId       string `json:"run_id"`
	ParentRunId string `json:"parent_run_id,omitempty"`

	// Views 本 Run 成功的 memory_view 次数（发出条件：> 0）。
	Views int `json:"views"`
	// PostHitProbeCalls 命中**之后**仍发生的探查调用总数（跨轮口径，见类型注释）。
	PostHitProbeCalls int `json:"post_hit_probe_calls"`
	// PostHitTools 按工具名拆分 PostHitProbeCalls，便于定位是 bash 还是 read_file 在重复。
	PostHitTools map[string]int `json:"post_hit_tools,omitempty"`

	Timestamp time.Time `json:"timestamp"`
	EventType EventType `json:"event_type"`
}

func (e *MemoryProbe) Type() EventType { return e.EventType }
