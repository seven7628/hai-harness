//go:build !linux

package sandbox

// cgroup v2 兜底 · 非 Linux：**不存在**（cgroup 是 Linux 专属内核能力）。
//
// ⇒ 平台行为差异**只在收益形态**：Linux 上多一道「连 setsid 逃逸者也收」的
// 保险；其它平台上逃逸者由**执行标记认领**（orphan.go）兜底 ⇒ 用户可见的
// 行为一致（都是「不留残余」），只是 Linux 更彻底。

// initCgroup 非 Linux：没有 cgroup ⇒ 降级（返回 ""）。
func initCgroup() string { return "" }

// cgroupAttach 非 Linux：什么都不做（进程组杀 + 标记认领已覆盖）。
func cgroupAttach(pid int) {}

// cgroupKillAll 非 Linux：什么都不做。
func cgroupKillAll() {}

// cgroupAvailable 非 Linux：恒 false。
func cgroupAvailable() bool { return false }
