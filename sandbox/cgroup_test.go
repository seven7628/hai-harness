package sandbox

// cgroup v2 兜底的**降级纪律**测试（2026-11-18「资源有界化」第 6 步）。
//
// 钉住：**没有 cgroup 权限时必须静默降级**（不报错、不崩、不影响命令执行）。
// 桌面 Linux 上 /sys/fs/cgroup 通常只有 systemd 委托的那一份可写 ⇒ 降级是
// **常态路径**，不是异常路径 ⇒ 必须被测试覆盖（CI 的 ubuntu runner 大概率
// 就是这条路径）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// testCtx 测试用 ctx。
func testCtx() context.Context { return context.Background() }

func TestCgroupDegradesGracefully(t *testing.T) {
	// initCgroup 不许 panic；不可用 ⇒ 返回 ""（降级）。
	dir := initCgroup()
	if dir == "" {
		// 降级路径 ✓：没有 cgroup 也能正常跑命令（进程组杀 + 标记认领兜底）。
		if _, err := (NoSandbox{}).Run(testCtx(), ExecSpec{Command: "echo ok"}); err != nil {
			t.Fatalf("没有 cgroup 时命令仍应能跑: %v", err)
		}
		return
	}
	// 可用 ⇒ 目录里应有 cgroup.procs / cgroup.kill（v2 的两个关键文件）。
	defer os.Remove(dir) // 空 cgroup 可删；删不掉（EBUSY）也无所谓
	for _, f := range []string{"cgroup.procs", "cgroup.kill"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("cgroup 目录里应有 %s: %v", f, err)
		}
	}
	// 挂着进程进 cgroup 也不许报错（失败即放弃 ⇒ 降级）。
	cgroupAttach(os.Getpid())
}
