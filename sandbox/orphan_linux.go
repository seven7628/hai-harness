//go:build linux

package sandbox

// 逃逸者兜底 · Linux 实现（/proc）。
//
// 原语：
//   - `/proc/<pid>/stat` ⇒ ppid（第 4 字段）与 pgid（第 5 字段）；
//   - `/proc/<pid>/environ` ⇒ 进程的环境块（NUL 分隔）。
//
// 纪律：读不到 / 解析不出 ⇒ 不列（宁漏勿误杀）。

import (
	"os"
	"strconv"
	"strings"
)

// scanOrphanCandidates Linux：找出「PPID == 1 且环境里有我们的执行标记」的进程。
func scanOrphanCandidates() []orphanCandidate {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil // 读不到 ⇒ 不列
	}
	var out []orphanCandidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		ppid, pgid, ok := linuxProcParent(pid)
		if !ok || ppid != 1 {
			continue // 不是「父进程已死」的 ⇒ 跳过（正在跑的不碰）
		}
		env, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil || len(env) == 0 {
			continue // 读不到环境（权限 / 进程已退）⇒ 不列
		}
		if !hasExecMarker(env) {
			continue
		}
		out = append(out, orphanCandidate{pid: pid, pgid: pgid})
	}
	return out
}

// platformProcParent 取进程的 ppid / pgid（/proc）。
func platformProcParent(pid int) (ppid, pgid int, ok bool) {
	return linuxProcParent(pid)
}

// linuxProcParent 取进程的 ppid / pgid。
//
// /proc/<pid>/stat 的第 2 个字段是 comm（进程名，**可能含空格与括号**）⇒
// 不能按空格直接切 ⇒ 从**最后一个 ')' 之后**切（内核保证 comm 里的 ')' 会被
// 括号包住，所以最后一个 ')' 之后一定是字段 3 起的部分）。
func linuxProcParent(pid int) (ppid, pgid int, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return 0, 0, false
	}
	fields := strings.Fields(s[i+2:])
	// fields[0] = state（字段 3）⇒ fields[1] = ppid（字段 4）、fields[2] = pgid（字段 5）。
	if len(fields) < 3 {
		return 0, 0, false
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	pgid, err = strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	return ppid, pgid, true
}
