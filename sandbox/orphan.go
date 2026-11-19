package sandbox

// 逃逸者兜底（2026-11-18「资源有界化」第 5 步；调研文档的 S7/S8）。
//
// 问题：**进程组杀不到逃逸者**。`kill(-pgid)` 只能收「还在组里」的进程 ⇒
// 一旦后代 `setsid()`（或双重 fork）换了组，它就**不在任何我们的组里** ⇒
// 组杀（runExec 的正常退出收组、超时收组、升级收组）全都够不着它 ⇒
// 它照样是个孤儿（PPID=1、继续烧 CPU）。
//
// 解法：**给执行打标记，靠标记认领**。
//   - 每次执行注入 `HAI_EXEC_ID=<pid>-<序号>` 到子进程环境 ⇒ **后代继承**它
//     （setsid 不清环境 ⇒ 逃逸者也带着标记）；
//   - 清扫器扫全机进程：找「**父进程已死**（PPID == 1）+ **环境里有我们的标记**」
//     ⇒ 那就是从我们这儿逃出去的孤儿 ⇒ 杀掉。
//
// 为什么「父进程已死」是必要条件：还活着的执行（正在跑）也带标记 ⇒ 只按标记
// 杀会把**正在跑的命令**杀掉 ⇒ 必须两个条件同时成立。
//
// 平台分叉（两平台没有共同原语，见调研文档 §3.2）：
//   - macOS：没有 /proc ⇒ sysctl（kern.proc.all 拿 pid/ppid/pgid，
//     kern.procargs2 拿环境）；
//   - Linux：/proc/<pid>/stat（ppid/pgid）+ /proc/<pid>/environ（环境）。
//
// 纪律（比「多杀」重要得多）：**读不到 / 解析不了 ⇒ 不列**（宁漏勿误杀）。
// 误杀的代价是「用户的进程被我们杀了」，漏杀的代价只是「再多一个孤儿」。

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// execIDEnvVar 执行标记的环境变量名（逃逸者兜底的认领依据）。
//
// 为什么不叫 HAI_XXX_ID：它必须**明确是本仓的**、且模型/用户不会自己设置成
// 同样的名字（否则我们会误杀别人的进程）。
const execIDEnvVar = "HAI_EXEC_ID"

// execSeq 执行序号（进程内自增）：标记 = pid + 序号 ⇒ 进程内唯一、跨进程唯一
// （pid 不同）。不用随机数：省一次 crypto/rand（每条命令都要生成一次）。
var execSeq atomic.Uint64

// newExecID 生成本次执行的标记值。
func newExecID() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), execSeq.Add(1))
}

// orphanCandidate 一个疑似逃逸者（父进程已死 + 带我们的标记）。
type orphanCandidate struct {
	pid  int
	pgid int // 进程组（杀组时用；0 = 未知 ⇒ 只杀 pid）
}

// scanOrphanCandidates 平台实现（orphan_darwin.go / orphan_linux.go /
// orphan_other.go 三选一编译进来）：列出「父进程已死 + 带我们的执行标记」的进程。
//
// 契约：**只列确定的**。任何一步读不到 / 解析不对 ⇒ 不列（宁漏勿误杀）。

// orphanSweepInterval 逃逸者兜底清扫的周期（60 秒）。
//
// 为什么不是更短：一次 macOS 扫描 = 一次 kern.proc.all（几百个进程 × 几百字节
// ≈ 数百 KB 的拷贝）+ 只给「父进程已死」的那几个读环境 ⇒ 单次成本不大，
// 但没必要更快：逃逸者是**长尾**（正常的后台进程已被 S1 的组杀收干净）⇒
// 一分钟级的延迟不影响任何指标。
const orphanSweepInterval = 60 * time.Second

// sweepOrphans 收掉逃逸者：扫描 + 杀（SIGTERM → 宽限 → SIGKILL）。
//
// 杀法：**只杀它自己那个组**（如果它是组长）⇒ 连它 fork 的后代一起收；
// 它不是组长（pgid 属于别人）⇒ 只杀 pid，**绝不**杀那个别人的组（误杀）。
func sweepOrphans() int {
	cands := scanOrphanCandidates()
	for _, c := range cands {
		_ = syscall.Kill(c.pid, syscall.SIGTERM)
		if c.pgid == c.pid && c.pgid > 1 {
			// 它是自己组的组长 ⇒ 整组收（它 fork 的后代也跑不掉）。
			_ = syscall.Kill(-c.pgid, syscall.SIGTERM)
		}
		go func(c orphanCandidate) {
			time.Sleep(reapGrace)
			_ = syscall.Kill(c.pid, syscall.SIGKILL)
			if c.pgid == c.pid && c.pgid > 1 {
				_ = syscall.Kill(-c.pgid, syscall.SIGKILL)
			}
		}(c)
	}
	return len(cands)
}

// OrphanSweeper 逃逸者兜底清扫器（周期跑；应用关闭时停）。
type OrphanSweeper struct {
	stopOnce sync.Once
	stopCh   chan struct{}
}

// StartOrphanSweeper 起周期清扫协程。
//
// 什么时候起：宿主启动时（bridge）⇒ 清扫器与宿主同生命周期 ⇒ 宿主退出它也退出。
func StartOrphanSweeper() *OrphanSweeper {
	s := &OrphanSweeper{stopCh: make(chan struct{})}
	go func() {
		t := time.NewTicker(orphanSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-t.C:
				sweepOrphans()
			}
		}
	}()
	return s
}

// Stop 停清扫协程（幂等）。
func (s *OrphanSweeper) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
}

// hasExecMarker 判断一段环境里有没有我们的执行标记。
func hasExecMarker(env []byte) bool {
	return len(env) > 0 && containsBytes(env, []byte(execIDEnvVar+"="))
}

// containsBytes 字节包含（避免为一个判断引入 bytes 包的依赖差异）。
func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
