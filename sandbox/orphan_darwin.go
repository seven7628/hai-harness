//go:build darwin

package sandbox

// 逃逸者兜底 · macOS 实现（sysctl；macOS 没有 /proc）。
//
// 原语：
//   - `kern.proc.all` ⇒ 全机进程的 kinfo_proc 数组（含 pid / ppid / pgid）；
//   - `kern.procargs2 <pid>` ⇒ 该进程的 argv + envp（内核拼成一块 buffer）。
//
// ⚠️ **实测坑（写下来防以后有人"优化"掉）**：macOS 的 kern.procargs2
// **对 Apple 平台二进制（/bin/*、/usr/bin/* 里的系统自带头）不返回环境段**
// —— 实测 /bin/sleep、/usr/bin/env 的 procargs2 只有 argv（29~34 字节），
// 而**非平台二进制**（我们的 Go 二进制、Chrome、node、python…）**能读到** ✓。
// ⇒ 兜底对「平台二进制逃逸者」**认不出**（漏，不是误杀）；
//   而现实里的逃逸者（浏览器 / node / python）都在覆盖范围内 ✓。
//
// 纪律：
//   - **只读「父进程已死」的进程的环境**（PPID == 1）：其它进程带标记是正常的
//     （正在跑的命令）⇒ 不能碰。
//   - 读不到 / 解析不出 ⇒ 不列（宁漏勿误杀）。

import (
	"golang.org/x/sys/unix"
)

// platformProcParent 取进程的 ppid / pgid（sysctl kern.proc.pid）。
//
// 读不到（进程已退 / 权限）⇒ ok=false（调用方据此跳过，不猜）。
func platformProcParent(pid int) (ppid, pgid int, ok bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, 0, false
	}
	if int(kp.Proc.P_pid) != pid {
		return 0, 0, false // 自检：pid 字段不自洽 ⇒ 布局/实现变了 ⇒ 不猜
	}
	return int(kp.Eproc.Ppid), int(kp.Eproc.Pgid), true
}

// scanOrphanCandidates macOS：找出「PPID == 1 且环境里有我们的执行标记」的进程。
func scanOrphanCandidates() []orphanCandidate {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil // 读不到 ⇒ 不列（宁漏勿误杀）
	}
	var out []orphanCandidate
	for _, kp := range kps {
		pid := int(kp.Proc.P_pid)
		ppid := int(kp.Eproc.Ppid)
		if pid <= 1 || ppid != 1 {
			continue // 不是「父进程已死」的 ⇒ 跳过（正在跑的不碰）
		}
		env, err := unix.SysctlRaw("kern.procargs2", pid)
		if err != nil || len(env) == 0 {
			continue // 读不到环境（权限 / 进程已退）⇒ 不列
		}
		if !hasExecMarker(env) {
			continue
		}
		out = append(out, orphanCandidate{pid: pid, pgid: int(kp.Eproc.Pgid)})
	}
	return out
}
