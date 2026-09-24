//go:build !windows

package hooks

import (
	"context"
	"os/exec"
)

// commandFor 用用户的 shell 执行命令，使 $GOCODE_PROJECT_DIR、管道、&& 等按预期工作
// （与 Claude Code / Codex 的 "shell command" 语义一致）。
//
// 必须传 ctx：ctx 到期时 CommandContext 会 kill 子进程，这是"事件级超时"真正生效的前提。
func commandFor(ctx context.Context, cmd string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
}
