package builtin

// bash 的 **cwd 记忆分区**测试（2026-11-17，第 11 步）。
//
// 问题：cwd（cd 跨调用记忆）存在**工具实例字段**上，而工具实例在注册表里共享
// —— 主 agent 与后台子 agent 共用同一个 ToolEngine ⇒ 子 agent 的 `cd` 会改掉
// 主 agent 后续命令的工作目录（两个 agent 互相踩）。
//
// 现在：cwd 按**调用方分区**（键 = 后台任务 id；主 agent 直调 = ""）。
// 主 agent 的跨轮记忆照旧 ✓（键跨轮稳定）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/events"
)

// bashCtx 造一个带 ToolContext 的 ctx（taskId 区分调用方）。
func bashCtx(runID, taskID string) context.Context {
	return events.WithToolContextForRunTask(context.Background(), runID, runID, taskID, 0, nil, nil, nil)
}

// TestBashCwdIsolatedPerCaller 子 agent 的 cd 不影响主 agent。
func TestBashCwdIsolatedPerCaller(t *testing.T) {
	root := t.TempDir()
	// 2026-11-19：cd 目标必须在**工作区根内**（cd 越界现在会重置回根，
	// 见 bash 的 projectRoot/cwdResetNotice）。改造前这里用 t.TempDir() 造的是
	// 根的**兄弟目录**，新语义下会被重置 ⇒ 断言自然失败（不是本测试要测的东西）。
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := NewBashTool(root, 0, nil, nil)

	mainCtx := bashCtx("run-main", "")       // 主 agent 直调
	subCtx := bashCtx("run-sub", "task-123") // 后台子 agent

	// 子 agent cd 到别处
	if _, err := tool.Call(subCtx, "", `{"command":"cd `+sub+`"}`); err != nil {
		t.Fatal(err)
	}
	// 子 agent 自己的 cwd 变了
	if got := tool.currentCwd(subCtx); got != sub {
		t.Errorf("子 agent 的 cwd = %q，want %q", got, sub)
	}
	// 主 agent 的 cwd **不受影响**
	if got := tool.currentCwd(mainCtx); got != root {
		t.Errorf("子 agent 的 cd 影响了主 agent 的 cwd：%q，want %q", got, root)
	}
}

// TestBashCwdPersistsForSameCaller 同一调用方的 cwd 跨调用记忆照旧。
func TestBashCwdPersistsForSameCaller(t *testing.T) {
	root := t.TempDir()
	// 同上：cd 目标必须在工作区根内（越界会重置）。
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := NewBashTool(root, 0, nil, nil)
	ctx := bashCtx("run-main", "")

	if _, err := tool.Call(ctx, "", `{"command":"cd `+sub+`"}`); err != nil {
		t.Fatal(err)
	}
	if got := tool.currentCwd(ctx); got != sub {
		t.Errorf("cd 记忆丢了：%q，want %q", got, sub)
	}
	// 目录确实存在（cd 的前提）
	if _, err := os.Stat(sub); err != nil {
		t.Fatal(err)
	}
	_ = filepath.Clean(sub)
}
