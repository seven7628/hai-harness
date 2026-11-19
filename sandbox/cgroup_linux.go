//go:build linux

package sandbox

// cgroup v2 兜底（2026-11-18「资源有界化」第 6 步；调研文档的 S9）。
//
// 问题：**进程组杀不到逃逸者**（见 orphan.go 的说明）。Linux 上有一个内核级的
// 答案：cgroup v2 的 `cgroup.kill` —— **内核**杀掉 cgroup 里的所有进程，
// **连 setsid 换组的逃逸者也一起杀**（进程的 cgroup 归属在 fork 时继承，
// 换进程组不影响它）。
//
// 为什么是「一个 cgroup 装全部执行」而不是「每次执行一个 cgroup」：
//   - 每条命令建+删一个 cgroup = 每条命令 4 次内核操作，且**删不掉**：cgroup
//     里有进程时不能 rmdir（要等进程退出 ⇒ 与退出有竞态 ⇒ 会漏一堆空目录）；
//   - 一个 cgroup 装全部 ⇒ **零 per-execution 成本**，应用退出时一次
//     `cgroup.kill` 把「所有我们起的、还活着的」一次收干净（含逃逸者）；
//   - per-execution 的逃逸者由**执行标记认领**（orphan.go）周期回收 ✓。
//
// 可用性：cgroup v2 需要能写 /sys/fs/cgroup ⇒ 桌面 Linux 上通常只有 systemd
// 委托的 user slice 可写 ⇒ **写不了就当没有**（降级、只记一行日志、绝不报错）。
// 这与调研文档 §3.2 的结论一致：Linux 专属增强，**不影响跨平台同行为**
// （可移植核心的行为完全一致，这层只是「多一道保险」）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	// cgroupRootCandidates cgroup v2 的**候选挂载点/委托点**（按顺序试）。
	//
	// ① /sys/fs/cgroup：systemd 统一层级下的根（root 或容器里可写）；
	// ② /sys/fs/cgroup/user.slice/user-<uid>.slice：systemd 给登录用户委托的
	//    那一份（桌面 Linux 上的常见可写点）；
	// ③ $XDG_RUNTIME_DIR/… 不存在（cgroup 不在那儿）⇒ 略。
	cgroupRootName = "hai-exec"
	// cgroupNameLen 名字里带 pid ⇒ 多个 bridge 实例不撞名。
	cgroupNameLen = 16
)

var (
	cgroupOnce sync.Once
	cgroupPath string // "" = 不可用（降级）
	cgroupNote string // 降级原因（只报一次）
)

// initCgroup 惰性建我们自己的 cgroup（第一次执行时）。失败 ⇒ 降级（返回 ""）。
//
// 为什么惰性：建目录是一次内核操作；没跑过命令的进程不需要它（且可能没权限）。
func initCgroup() string {
	cgroupOnce.Do(func() {
		cgroupPath, cgroupNote = createCgroup()
		if cgroupPath == "" && cgroupNote != "" {
			fmt.Fprintf(os.Stderr, "sandbox: cgroup 兜底不可用（%s）⇒ 只靠进程组杀\n", cgroupNote)
		}
	})
	return cgroupPath
}

// createCgroup 试着在候选挂载点下建一个我们专属的 cgroup 目录。
//
// 返回 (路径, "") 或 ("", 原因)。**任何失败都只是降级**，绝不 panic、绝不阻塞。
func createCgroup() (string, string) {
	uid := os.Getuid()
	candidates := []string{
		"/sys/fs/cgroup",
		filepath.Join("/sys/fs/cgroup/user.slice", "user-"+strconv.Itoa(uid)+".slice"),
	}
	var lastErr error
	for _, root := range candidates {
		// 根不存在 ⇒ 试下一个。
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			continue
		}
		// 必须是 cgroup v2（cgroup.controllers 存在 ⇒ v2；v1 没有这个文件）。
		if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
			lastErr = fmt.Errorf("%s 不是 cgroup v2", root)
			continue
		}
		name := cgroupRootName + "-" + strconv.Itoa(os.Getpid()) + "-" +
			strconv.FormatInt(time.Now().UnixNano()%1000000, 36)
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			lastErr = fmt.Errorf("建 %s 失败: %w", dir, err)
			continue
		}
		return dir, ""
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("找不到可写的 cgroup v2 挂载点")
	}
	return "", lastErr.Error()
}

// cgroupAttach 把刚起的子进程放进我们的 cgroup（它的后代**继承**这个 cgroup ⇒
// 连 setsid 逃逸者也在里面 ⇒ 退出时一次 cgroup.kill 全收）。
//
// 失败一律忽略（降级）：进程组杀仍覆盖绝大多数残留。
func cgroupAttach(pid int) {
	dir := initCgroup()
	if dir == "" || pid <= 0 {
		return
	}
	// 写 pid 到 cgroup.procs ⇒ 内核把该进程移进这个 cgroup。
	// 用 O_WRONLY|O_TRUNC（cgroupfs 的惯例写法），失败即放弃。
	f, err := os.OpenFile(filepath.Join(dir, "cgroup.procs"), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(strconv.Itoa(pid))
}

// cgroupKillAll 用内核的 cgroup.kill 收掉我们 cgroup 里的所有进程（含逃逸者）。
//
// 顺序：先 cgroup.kill（内核收进程）⇒ 再删目录（进程死光后目录才可删 ⇒
// 删不掉就留下，下次启动时按名字前缀清理）。
func cgroupKillAll() {
	cgroupOnce.Do(func() {}) // 确保 initCgroup 跑过（没跑过就是没有 cgroup）
	dir := cgroupPath
	if dir == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "cgroup.kill"), os.O_WRONLY|os.O_TRUNC, 0)
	if err == nil {
		_, _ = f.WriteString("1")
		_ = f.Close()
	}
	// 进程可能还在退出 ⇒ 删不掉就算了（下次启动清理）。
	_ = os.Remove(dir)
}

// cgroupAvailable 当前有没有可用的 cgroup 兜底（诊断/回归网用）。
func cgroupAvailable() bool { return initCgroup() != "" }
