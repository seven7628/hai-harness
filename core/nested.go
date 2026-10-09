package core

import (
	"sync"
	"time"
)

// 嵌套调用（编排型工具内部再次调用工具）的有界记录与用量累加。
//
// 归属本包而非 tools：events 需要引用本类型，而 tools 已 import events，
// 放tools 会成环（events ← tools）。core 是两者的共同依赖，无环。
//
// 对齐 pi 的 NESTED_CALL_LIMITS（pi-src/codemode/tool.ts:220-226）。

// 嵌套记录限额。语义与 pi 一致：**限额只约束「记录」，不约束「执行」** ——
// 超限后工具调用照常执行（副作用真实发生、usage 照常计费），只是不再进记录快照。
const (
	// NestedMaxCalls 单次外层调用内最多保留的嵌套记录条数。
	NestedMaxCalls = 256
	// NestedMaxArgsPerCall 单条记录里 args/result 各字段的字符上限。
	NestedMaxArgsPerCall = 8 << 10 // 8 KiB
	// NestedMaxArgsTotal 单次外层调用内所有记录的 args 字符总量上限。
	NestedMaxArgsTotal = 32 << 10 // 32 KiB
	// NestedMaxErrorChars 单条记录里错误文本的字符上限。
	NestedMaxErrorChars = 500
	// NestedMaxDepth 编排嵌套深度上限。codemode 不自嵌套 ——
	// 对齐 pi 给 codemode 工具自身标 exposure:"model-only"（tool.ts:424）
	// 从而硬封死递归，即「深度恒为 1」。
	//
	// 注意与 subagent 的 maxSpawnDepth（subagent/agent_tools.go:27 = 2）是
	// **独立的另一道闸**：那道管「运行」间的派生（主→子→孙），
	// 本常量管「单次运行内工具调用的嵌套」。两者都要接线才无逃逸路径。
	NestedMaxDepth = 1
)

// NestedCallRecord 一次嵌套工具调用的摘要。字段刻意保持扁平且有界 ——
// 它会挂到外层 ToolResult 上并进入 UI/导出，不能带任意大的载荷。
type NestedCallRecord struct {
	Id   string `json:"id"`
	Name string `json:"name"`

	// Args / Result 已按 NestedMaxArgsPerCall 截断（标注截断）。
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`

	IsError bool   `json:"is_error,omitempty"`
	Error   string `json:"error,omitempty"` // 已按 NestedMaxErrorChars 截断

	Duration time.Duration `json:"duration,omitempty"`

	// Usage 该次调用报告的用量；nil = 未报告（非模型调用类工具）。
	Usage *Usage `json:"usage,omitempty"`
}

// NestedRecorder 一次外层调用内的嵌套记录器 + 用量累加器。
//
// 并发安全：脚本内可用 Promise.all 并发发起多个嵌套调用。
type NestedRecorder struct {
	mu       sync.Mutex
	calls    []NestedCallRecord
	argBytes int
	dropped  int       // 因超限被丢弃的记录数（可观测：非零说明脚本调用量超过限额）
	usageBox *usageBox // 共享：嵌套派生出的子记录器与其父共用同一个用量累加器
}

// usageBox 共享用量累加器。单独成结构是为了让 Clone 出来的子记录器
// 与父记录器 AddUsage 到同一处 —— 外层 ToolResult.Usage 必须包含所有内层用量。
type usageBox struct {
	mu sync.Mutex
	u  Usage
}

func (b *usageBox) add(u Usage) {
	if b == nil || u.IsZero() {
		return
	}
	b.mu.Lock()
	b.u = b.u.Add(u)
	b.mu.Unlock()
}

func (b *usageBox) get() Usage {
	if b == nil {
		return Usage{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.u
}

// NewNestedRecorder 创建一个记录器。
func NewNestedRecorder() *NestedRecorder { return &NestedRecorder{usageBox: &usageBox{}} }

// box 取共享用量盒，必要时在**锁内**惰性初始化。
//
// 惰性初始化必须在 r.mu 内：零值 NestedRecorder{}（未经 NewNestedRecorder）的
// usageBox 为 nil，并发首次 AddUsage 若无锁写入 r.usageBox 即为数据竞态
// —— 由 TestZeroValueRecorderConcurrentAddUsage 以 -race 捕获。
func (r *NestedRecorder) box() *usageBox {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.usageBox == nil {
		r.usageBox = &usageBox{}
	}
	return r.usageBox
}

// Add 记录一次嵌套调用；argsBytes 为该次调用的参数字节数（用于总量限额）。
//
// 返回是否已记录。**超限时调用不记录但仍计费** —— 调用者必须继续执行
// AddUsage，否则用量会漏，session 成本就会低于真实开销。
func (r *NestedRecorder) Add(rec NestedCallRecord, argsBytes int) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// 总量限额与条数限额：任一超限即丢弃记录（但计费照旧，由 AddUsage 负责）。
	overCount := len(r.calls) >= NestedMaxCalls
	overBytes := argsBytes > 0 && r.argBytes+argsBytes > NestedMaxArgsTotal
	if overCount || overBytes {
		r.dropped++
		return false
	}
	r.calls = append(r.calls, rec)
	r.argBytes += argsBytes
	return true
}

// AddUsage 累加嵌套调用用量。与 Add **分开**是刻意的：记录可能被限额丢弃，
// 但用量必须无条件累加（计费不因记录截断而漏）。
func (r *NestedRecorder) AddUsage(u Usage) {
	if r == nil || u.IsZero() {
		return
	}
	r.box().add(u)
}

// Usage 返回当前累加的用量（不取出、不清零）。
func (r *NestedRecorder) Usage() Usage {
	if r == nil {
		return Usage{}
	}
	return r.box().get()
}

// TakeUsage 返回并**清空**累加的用量（引擎在最外层调用收尾时并入 ToolResult）。
//
// 为什么必须与 TakeRecord 分开取：记录会被限额丢弃、用量不会（见 Add），
// 两者的取出时机也可能不同（外层的 usage 要在 finish 时才合并）。
func (r *NestedRecorder) TakeUsage() Usage {
	if r == nil {
		return Usage{}
	}
	b := r.box()
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.u
	b.u = Usage{}
	return u
}

// TakeRecord 返回当前记录并**清空**（挂到外层 ToolResult 上后调用一次）。
func (r *NestedRecorder) TakeRecord() []NestedCallRecord {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil
	}
	out := r.calls
	r.calls = nil
	r.argBytes = 0
	return out
}

// Stats 返回被丢弃的记录数（可观测性：非零说明脚本调用量超过限额）。
func (r *NestedRecorder) Stats() (dropped int) {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// Clone 派生一个子记录器：**用量累加共享**（子层的用量必须计入外层），
// 记录独立（外层只关心自己这层的调用序列）。
//
// 用于嵌套作用域继承：一个编排工具内部再调另一个编排工具时，
// 内层的记录挂内层的 ToolResult，内层的用量汇入外层。
func (r *NestedRecorder) Clone() *NestedRecorder {
	if r == nil {
		return nil
	}
	// 共享用量累加器：把 usage 指向同一个 *usageBox。
	return &NestedRecorder{usageBox: r.box()} // 共享同一个 box
}
