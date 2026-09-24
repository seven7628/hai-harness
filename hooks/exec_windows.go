//go:build windows

package hooks

import (
	"context"
	"os/exec"
)

// commandFor Windows 上走 cmd.exe；需要 PowerShell 语法时请显式写 `powershell -Command "…"`。
// 必须传 ctx：ctx 到期时 CommandContext 会 kill 子进程，这是"事件级超时"真正生效的前提。
func commandFor(ctx context.Context, cmd string) *exec.Cmd {
	return exec.CommandContext(ctx, "cmd.exe", "/C", cmd)
}
