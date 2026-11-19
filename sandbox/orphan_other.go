//go:build !darwin && !linux

package sandbox

// 逃逸者兜底 · 其它平台（Windows 等）：**不支持** ⇒ 不列任何候选。
//
// 纪律（与其它平台一致）：读不到 ⇒ 不列（宁漏勿误杀）。
// 兜底仍有一层：进程组杀（runExec 的正常退出/超时/升级）覆盖了绝大多数残留。

// scanOrphanCandidates 不支持的平台：返回空（不列 ⇒ 不杀）。
func scanOrphanCandidates() []orphanCandidate { return nil }

// platformProcParent 不支持的平台：读不到（ok=false）。
func platformProcParent(pid int) (ppid, pgid int, ok bool) { return 0, 0, false }
