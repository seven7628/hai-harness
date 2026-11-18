package tools

// 单次工具调用的**变更 diff** 收集槽（每次调用独立，放 ctx；与 ImageSink /
// ExecSink / StructuredSink 同范式）。
//
// 为什么不用「工具实例上的暂存字段 + ToolDiffProvider」（改造前的做法）：
// 工具实例在注册表里是**共享**的 —— 主 agent 与后台子 agent 共用同一个
// ToolEngine（desktop/bridge），同一工具实例会被**并发**调用。
// 实例字段在这种并发下会互相清空/串味：
//
//	A 调用刚写完 t.diff，B 调用进来把 t.diff 置 nil ⇒ A 的产出卡丢失；
//	或 A 读到 B 的 diff ⇒ 宿主的「+/- 变更」挂到错误的文件上。
//
// 引擎自己的注释已经写明了这条（「实例字段会互相清空/串味（评审实测 3/300 复现）」），
// 而 write_file / edit_file / run_python 正是漏网的三个（它们的 CanParallel=false
// 只覆盖「单批内」，覆盖不了「主 agent × 子 agent × codemode 嵌套」三条并发路径）。
//
// 把槽放进 ctx（每次调用新建）⇒ 按构造即无竞态。

import (
	"context"
	"sync"

	"github.com/seven7628/hai-harness/core"
)

// DiffSink 单次调用的变更 diff 槽。
type DiffSink struct {
	mu   sync.Mutex
	diff *core.FileDiff
	set  bool
}

// SetDiff 由工具在 Call 内调用（本次调用对文件的变更）。
func (s *DiffSink) SetDiff(d *core.FileDiff) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.diff, s.set = d, true
	s.mu.Unlock()
}

// Diff 读取（engine 在 Call 返回后读取）。ok=false = 本次调用没上报。
func (s *DiffSink) Diff() (*core.FileDiff, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.diff, s.set
}

type diffSinkKey struct{}

// WithDiffSink 把变更 diff 槽挂进 ctx（每次调用一个）。
func WithDiffSink(ctx context.Context, s *DiffSink) context.Context {
	return context.WithValue(ctx, diffSinkKey{}, s)
}

// DiffSinkFrom 取出本次调用的 diff 槽；未注入（直接调用/单测）返回 nil（nil 安全）。
func DiffSinkFrom(ctx context.Context) *DiffSink {
	s, _ := ctx.Value(diffSinkKey{}).(*DiffSink)
	return s
}

// SetFileDiff 便捷函数：工具经它把 diff 写进本次调用的槽。
// 未注入槽时（直调/单测）静默丢弃 —— 与 ImageSink 的 nil 安全同语义。
func SetFileDiff(ctx context.Context, d *core.FileDiff) {
	DiffSinkFrom(ctx).SetDiff(d)
}
