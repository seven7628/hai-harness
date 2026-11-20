package agents

// 同步子运行的 cwd 分区键（R6）回归测试。
//
// 来源：docs/AGENT_CWD_DESIGN_2026-11-19.md §14.6（R6）+ §14.11 T4 + §16.3 #4。
//
// 问题：subagent 的**同步**子运行不设 opts.TaskId ⇒ ToolContext.TaskId 为空
// ⇒ 与主 agent 共用 workdir.MainKey 分区 ⇒ 子 agent 的 `cd` 会把主 agent 的
// 目录改掉（两个 agent 在同一份工作树里互相踩）。
// 现在：taskID 为空且 Depth > 0 时合成 sync:<runid>:<depth>。

import (
	"strings"
	"testing"
)

// TestSyncSubrunPartitionKeyDiffers T4：同步子运行的分区键 ≠ 主 agent 的分区键。
func TestSyncSubrunPartitionKeyDiffers(t *testing.T) {
	mainKey := cwdPartitionKey("", "run-main", 0)
	if mainKey != "" {
		t.Fatalf("主运行的分区键应留空（由 workdir.KeyFrom 回落到 MainKey），得到 %q", mainKey)
	}

	sub1 := cwdPartitionKey("", "run-1", 1)
	if sub1 == "" {
		t.Fatal("同步子运行的分区键不能为空（会与主 agent 同分区）")
	}
	if !strings.HasPrefix(sub1, "sync:") {
		t.Fatalf("同步子运行的分区键应带 sync: 前缀，得到 %q", sub1)
	}

	// 两次不同的同步子运行必须落在不同分区。
	sub2 := cwdPartitionKey("", "run-2", 1)
	if sub1 == sub2 {
		t.Fatalf("不同同步子运行的分区键撞了：%q", sub1)
	}

	// 同一子运行内**稳定**（一条命令里多次取到同一个键）。
	if again := cwdPartitionKey("", "run-1", 1); again != sub1 {
		t.Fatalf("同一子运行的分区键不稳定：%q vs %q", sub1, again)
	}

	// 嵌套层区分（孙运行不与父运行同分区）。
	if cwdPartitionKey("", "run-1", 2) == sub1 {
		t.Fatal("不同 Depth 的嵌套子运行不该同分区")
	}

	// 后台子运行（有 TaskId）保持原值 —— 它是跨 Run 稳定的身份键。
	if got := cwdPartitionKey("task-abc", "run-x", 3); got != "task-abc" {
		t.Fatalf("后台子运行的分区键应原样用 TaskId，得到 %q", got)
	}
}
