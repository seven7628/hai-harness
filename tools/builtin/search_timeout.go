package builtin

// 搜索工具（grep / glob）的**可中断性**与工具级时限。
//
// 改造前 grepTool.Call / globTool.Call 的第一个参数是 `_ context.Context`
// —— **ctx 被整个丢弃**。后果（2026-11-17 实测）：
//   - 引擎的 5 分钟工具超时（tools/engine.go:23 defaultToolTimeout）对它们
//     **完全无效** —— 超时是靠取消 taskCtx 实现的，而工具不看 ctx；
//   - 用户中断 / 会话 abort 同样传不进来；
//   - 会话 abort 后引擎走 toolStuckTimeout 返回（engine.go:938），但
//     **grep 的 goroutine 还在后台继续把整个工作区读完**（engine.go:944
//     的注释也承认这一点）。
//
// 现在：ctx 被真正使用 —— 遍历回调与文件扫描循环都检查它；
// 并通过 ToolTimeoutProvider 声明工具级时限（引擎把它落在可摘离的
// liftableTimeout 上：任务 promote 成后台时自动解除，见 tools/timeout.go）。

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// searchToolTimeout grep / glob 的工具级时限。
	//
	// 取 60s 的理由：加了忽略列表后，一次全仓 grep 实测 ~90ms（合成树）
	// / 341ms（真实仓的 6 个源码目录），60s 是三个数量级的余量；
	// 而默认的 5 分钟在「模型搜了一棵巨型 monorepo」时会白等 5 分钟
	// 且期间不可中断。
	searchToolTimeout = 60 * time.Second

	// searchCtxCheckEvery 扫描循环里检查 ctx 的间隔（行数）。
	// ctx.Err() 不是免费的（引擎的 liftableTimeout.Err() 要加锁），
	// 所以按行数抽样检查而不是每行都查。
	searchCtxCheckEvery = 4096
)

// searchCancelledError 把 ctx 的取消/超时翻译成模型可恢复的错误。
//
// 区分两件事（模型的下一步动作不同）：
//   - DeadlineExceeded = 搜太宽了 → 让模型**收窄**；
//   - Canceled         = 被用户/会话中断 → 让模型**换个更小的范围重来**。
func searchCancelledError(ctx context.Context, tool string) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return recoveryError("search_timeout", "narrow_search",
			fmt.Sprintf("%s exceeded the %s time limit", tool, searchToolTimeout),
			"Narrow the search: set a specific path, add an include filter, or search a subdirectory. Do not repeat the same search with the same arguments.")
	case errors.Is(ctx.Err(), context.Canceled):
		return recoveryError("search_cancelled", "retry_narrower",
			fmt.Sprintf("%s was cancelled before finishing", tool),
			"The search was interrupted. Retry with a narrower path or include filter so it finishes quickly.")
	default:
		return ctx.Err()
	}
}
