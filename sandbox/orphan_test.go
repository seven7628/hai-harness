package sandbox

// 逃逸者兜底的回归网（2026-11-18「资源有界化」第 5 步）。
//
// 钉住：
//  1. 每次执行都带**执行标记**（后代继承 ⇒ 逃逸者也带）；
//  2. 清扫器只认「**父进程已死** + 带标记」的进程（正在跑的不碰）；
//  3. 端到端：一个「父进程已死」的残留进程 ⇒ 被认领 ⇒ 被收掉。
//
// 端到端用例的构造（为什么用**测试二进制自己**当被测进程，而不是 sleep）：
// macOS 的 kern.procargs2 **对 Apple 平台二进制（/bin/*、/usr/bin/*）不返回
// 环境段**（实测：/bin/sleep 的 procargs2 只有 argv ⇒ 读不到标记），
// 而非平台二进制（我们的 Go 二进制、Chrome、node、python）**能读到** ✓
// ⇒ 用测试二进制当被测进程，两平台都能真实验证这条链路。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// orphanRoleEnv 子进程的角色标记（测试用）。
const orphanRoleEnv = "HAI_TEST_ORPHAN_ROLE"

// orphanPIDFileEnv 子进程写 pid 的文件路径（测试用）。
const orphanPIDFileEnv = "HAI_TEST_ORPHAN_PIDFILE"

// TestOrphanHelper 重入的辅助用例：按角色行事。
//
//   - 角色 "child"：起一个 grandchild（带我们的执行标记），写下它的 pid，然后**立刻退出**
//     ⇒ grandchild 的父进程死了 ⇒ 它被 init 收养（PPID == 1）。
//   - 角色 "grandchild"：睡很久，等着被认领/被收。
//
// 角色为空 ⇒ 直接返回（普通那次运行）。
func TestOrphanHelper(t *testing.T) {
	switch os.Getenv(orphanRoleEnv) {
	case "child":
		g := exec.Command(os.Args[0], "-test.run", "TestOrphanHelper")
		g.Env = append(isolatedEnv(), orphanRoleEnv+"=grandchild")
		if err := g.Start(); err != nil {
			t.Fatalf("起 grandchild 失败: %v", err)
		}
		// 写 pid（grandchild 自己不写：它不知道路径 ⇒ 由 child 代写）。
		if f := os.Getenv(orphanPIDFileEnv); f != "" {
			_ = os.WriteFile(f, []byte(strconv.Itoa(g.Process.Pid)), 0o600)
		}
		return // 退出 ⇒ grandchild 变孤儿
	case "grandchild":
		time.Sleep(60 * time.Second)
		return
	}
}

// TestIsolatedEnvCarriesExecMarker：每次执行都带标记，且**每次不同**。
func TestIsolatedEnvCarriesExecMarker(t *testing.T) {
	env := isolatedEnv()
	var marker string
	for _, e := range env {
		if strings.HasPrefix(e, execIDEnvVar+"=") {
			marker = e
		}
	}
	if marker == "" {
		t.Fatalf("环境里没有执行标记 %s", execIDEnvVar)
	}
	if marker == execIDEnvVar+"=" {
		t.Fatal("执行标记没有值")
	}
	// 两次执行 ⇒ 两个不同的标记（否则认不出「哪一次执行」的残留）。
	env2 := isolatedEnv()
	same := false
	for _, e := range env2 {
		if e == marker {
			same = true
		}
	}
	if same {
		t.Fatalf("两次执行的标记相同（%s）⇒ 认不出是哪一次执行的残留", marker)
	}
}

// TestHasExecMarker：标记识别（含「只有前缀没有等号」不算命中）。
func TestHasExecMarker(t *testing.T) {
	if !hasExecMarker([]byte("PATH=/bin\n" + execIDEnvVar + "=123-4\n")) {
		t.Fatal("应识别出标记")
	}
	if hasExecMarker([]byte("PATH=/bin\n")) {
		t.Fatal("没有标记却命中了")
	}
	if hasExecMarker([]byte(execIDEnvVar)) { // 只有名字、没有 "=" ⇒ 不是标记
		t.Fatal("只有变量名不算命中")
	}
}

// TestSweepOrphansReclaimsEscapedProcess 端到端：父进程已死的残留进程被收掉。
func TestSweepOrphansReclaimsEscapedProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")

	// ① 起「child」角色：它起 grandchild 后立刻退出 ⇒ grandchild 成孤儿。
	child := exec.Command(os.Args[0], "-test.run", "TestOrphanHelper")
	child.Env = append(isolatedEnv(), orphanRoleEnv+"=child", orphanPIDFileEnv+"="+pidFile)
	if err := child.Run(); err != nil {
		t.Fatalf("child 进程失败: %v", err)
	}

	// ② 读 grandchild 的 pid。
	pid := 0
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				pid, _ = strconv.Atoi(s)
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid <= 0 {
		t.Fatal("拿不到 grandchild 的 pid")
	}
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()

	// ③ 等它被 init 收养（PPID == 1）。
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ppid, _, ok := platformProcParent(pid); ok && ppid == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	ppid, _, ok := platformProcParent(pid)
	if !ok {
		t.Skipf("读不到 %d 的父进程（平台不支持 / 进程已退）", pid)
	}
	if ppid != 1 {
		t.Skipf("环境没有把孤儿收养到 init（ppid=%d）⇒ 跳过", ppid)
	}

	// ④ 清扫器应当认领它（父进程已死 + 带标记）。
	cands := scanOrphanCandidates()
	found := false
	for _, c := range cands {
		if c.pid == pid {
			found = true
		}
	}
	if !found {
		t.Fatalf("清扫器没认领逃逸者 %d（候选: %+v）", pid, cands)
	}

	// ⑤ 收掉它。
	sweepOrphans()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // 已消失 ✓
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("逃逸者 %d 没收掉", pid)
}

// TestSweepOrphansIgnoresLiveProcesses：正在跑的执行（父进程还活着）不被认领。
//
// 这是**误杀防线**：只按标记杀会把用户正在跑的命令杀掉 ⇒ 必须两个条件同时成立。
func TestSweepOrphansIgnoresLiveProcesses(t *testing.T) {
	// 起一个带标记的长跑进程（父进程 = 测试进程，活着）。
	cmd := exec.Command(os.Args[0], "-test.run", "TestOrphanHelper")
	cmd.Env = append(isolatedEnv(), orphanRoleEnv+"=grandchild")
	if err := cmd.Start(); err != nil {
		t.Fatalf("起进程失败: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	time.Sleep(300 * time.Millisecond)

	for _, c := range scanOrphanCandidates() {
		if c.pid == cmd.Process.Pid {
			t.Fatalf("正在跑的进程 %d 被误认成逃逸者", cmd.Process.Pid)
		}
	}
}
