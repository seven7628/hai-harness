package tools

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// 工具调用不得泄漏 Reparentable 的 watcher goroutine。
//
// 2026-11-09 实测事故：bridge 连续跑 18h，goroutine 创建计数到 65 万，
// dump 里大量 [select] 的停泊 watcher；goroutine 堆积又把 subagent 注册表的
// r.mu 拖成长临界区 → 桌面端「所有工作区会话数都是 0、无法切会话」。
//
// 根因：core.NewReparentable 每次创建都会起一个 watcher goroutine，它只在
// 「父取消 / Cancel() / Detach()」三者之一发生时才退出。工具**正常跑完**时
// 没人调 Cancel/Detach（Detach 只在 promote 路径被调）→ watcher 永久停泊
// 在 select 上 → **每次工具调用漏一个 goroutine**。
//
// 本测试用「父 ctx 永不取消」的场景（N 次调用后 goroutine 数不涨）来锁住这个行为。
func TestToolCallDoesNotLeakReparentableWatcher(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &mockTool{name: "a"})

	// 基线（含引擎自身的后台 goroutine）。
	base := runtime.NumGoroutine()

	const rounds = 3
	const perRound = 40
	for r := 0; r < rounds; r++ {
		calls := make([]core.ToolCall, 0, perRound)
		for i := 0; i < perRound; i++ {
			calls = append(calls, core.ToolCall{
				Id:        fmt.Sprintf("c%d-%d", r, i),
				Name:      "a",
				Arguments: "{}",
			})
		}
		// 父 ctx 永不取消（Background）——最贴近真实：会话活着时父 ctx 一直不取消。
		if _, err := e.Sequence(context.Background(), calls, nil); err != nil {
			t.Fatalf("Sequence 失败: %v", err)
		}
	}

	// watcher 退出是异步的，给它一点时间。
	deadline := time.Now().Add(3 * time.Second)
	after := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		after = runtime.NumGoroutine()
		if after <= base+5 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after > base+5 {
		t.Fatalf("工具调用泄漏 goroutine：基线 %d → %d（%d 次调用）；watcher 没被释放",
			base, after, rounds*perRound)
	}
}
