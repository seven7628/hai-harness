package runtime

import (
	"context"
	"testing"
)

// BenchmarkEnsureReady 测**已就绪**运行时的 Ensure 快路径成本。
//
// 为什么单独测：Ensure 是 run_python / 三个预览器**每次调用**都会走的入口。
// 「快路径」本该是「不碰磁盘、不起进程」的纯查表，但现状里 Status 仍会
// 起一次 `venv/bin/python -c "import sys"`（确认解释器还能跑）⇒ 每次调用
// 白付一次进程启动 + venv site 初始化。
//
// 前提：运行时**已 bootstrap**（否则 Ensure 会触发真实安装，bench 不可复现）。
// 不满足就 skip，并说清原因。
func BenchmarkEnsureReady(b *testing.B) {
	rt := New(Config{})
	ctx := context.Background()
	st, err := rt.Status(ctx)
	if err != nil {
		b.Skipf("Status 失败: %v", err)
	}
	if !st.Ready {
		b.Skipf("受管运行时未就绪（%s / stale=%v）：先跑 go test ./runtime/ -run TestRealEnsureInstallsRuntime", st.Reason, st.Stale)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rt.Ensure(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
