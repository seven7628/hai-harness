package sandbox

import "testing"

// BenchmarkIsolatedEnv 测「每次命令都重建一遍最小环境白名单」的成本（第 4 轮爬山第 9 步）。
//
// 为什么单独测：isolatedEnv 每次执行命令都调一次（8 个固定项 + 6 个代理变量），
// 而「命令执行」的固定成本是**进程 spawn**（~5ms）⇒ 只有当 isolatedEnv 相对它
// 不可忽略时，才值得缓存（缓存会**冻结**环境变量 ⇒ 语义变更）。
func BenchmarkIsolatedEnv(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = isolatedEnv()
	}
}
