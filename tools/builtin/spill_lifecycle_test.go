package builtin

// 工具级落盘资源的回收（2026-11-18「资源有界化」第 2 步）。
//
// 钉住三件事：
//  1. **没发生溢出的调用不留目录**（改造前每次调用都建一个 0700 目录 ⇒
//     实测 2158 个目录、87% 是空壳）；
//  2. **TTL 内的文件必须留着**（路径是交给模型的承诺：模型要能读）；
//  3. **过期的文件与空壳目录被回收**（按年龄删，不是随手删）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSpillStoreDoesNotCreateDirUntilWrite：reserve 只给路径，write 才建目录。
func TestSpillStoreDoesNotCreateDirUntilWrite(t *testing.T) {
	s := newSpillStore("hai-harness-test-spill-")
	path := s.reserve(context.Background())
	if path == "" {
		t.Fatal("reserve 返回空路径")
	}
	dir := filepath.Dir(path)
	defer os.RemoveAll(dir)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("reserve 不该建目录（实测存在：%v）", err)
	}
	// 真落盘 ⇒ 目录才出现（0700）。
	written := s.write(context.Background(), "hello")
	if written == "" {
		t.Fatal("write 返回空路径")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("write 后目录应存在: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("落盘目录权限 = %o，期望 700", perm)
	}
}

// TestSweepToolSpillDirKeepsFreshRemovesExpired：TTL 内的留着，过期的删，空壳删。
func TestSweepToolSpillDirKeepsFreshRemovesExpired(t *testing.T) {
	now := time.Now()
	root := t.TempDir()

	// ① 有新鲜文件的目录 ⇒ 保留（模型还要读）。
	keep := filepath.Join(root, "hai-harness-bash-spill-keep")
	mkdir(t, keep)
	writeFile(t, filepath.Join(keep, "spill-a.txt"), "fresh", now.Add(-time.Hour))

	// ② 全是过期文件的目录 ⇒ 文件删 + 目录删。
	expired := filepath.Join(root, "hai-harness-bash-spill-expired")
	mkdir(t, expired)
	writeFile(t, filepath.Join(expired, "spill-b.txt"), "old", now.Add(-48*time.Hour))

	// ③ 空目录（改造前每次调用都建 ⇒ 87% 是这种）⇒ 删。
	empty := filepath.Join(root, "hai-harness-python-spill-empty")
	mkdir(t, empty)

	// ④ 不是落盘目录（别误删）⇒ 不动。
	other := filepath.Join(root, "something-else")
	mkdir(t, other)
	writeFile(t, filepath.Join(other, "x.txt"), "keep me", now.Add(-72*time.Hour))

	for _, d := range []string{keep, expired, empty, other} {
		sweepToolSpillDir(d, now)
	}

	if _, err := os.Stat(filepath.Join(keep, "spill-a.txt")); err != nil {
		t.Fatalf("TTL 内的文件必须留着（模型读得到）: %v", err)
	}
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("过期的目录应被回收（实测仍存在）")
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatalf("空壳目录应被回收（实测仍存在）")
	}
	if _, err := os.Stat(filepath.Join(other, "x.txt")); err != nil {
		t.Fatalf("非落盘目录不该被动: %v", err)
	}
}

// TestSweepToolSpillRootEnforcesTotalCap：总量超上限 ⇒ 按**最旧先删**。
//
// 为什么需要总量上限：TTL 只按年龄删 ⇒ 一天内跑几百次大输出命令就能堆到
// GB 级（实测清扫后仍剩 1.0GB / 363 个文件，全是当天写的）。
func TestSweepToolSpillRootEnforcesTotalCap(t *testing.T) {
	now := time.Now()
	root := t.TempDir()

	// 三个 200KB 的文件（上限按测试尺度给：300KB）。
	const testCap = 300 << 10

	dir := filepath.Join(root, "hai-harness-bash-spill-cap")
	mkdir(t, dir)
	blob := make([]byte, 200<<10)
	for i, age := range []time.Duration{3 * time.Hour, 2 * time.Hour, 1 * time.Hour} {
		p := filepath.Join(dir, "spill-"+string(rune('a'+i))+".txt")
		writeFile(t, p, string(blob), now.Add(-age))
	}

	sweepToolSpillRoot(root, now, testCap)

	// 最旧的被删，最新的留着。
	if _, err := os.Stat(filepath.Join(dir, "spill-a.txt")); !os.IsNotExist(err) {
		t.Fatalf("最旧的文件应被删（总量超上限）")
	}
	if _, err := os.Stat(filepath.Join(dir, "spill-c.txt")); err != nil {
		t.Fatalf("最新的文件必须留着: %v", err)
	}
}

// TestIsToolSpillDirMatchesConvention：目录识别按命名约定（新增一族自动覆盖）。
func TestIsToolSpillDirMatchesConvention(t *testing.T) {
	for _, name := range []string{
		"hai-harness-bash-spill-123",
		"hai-harness-python-spill-456",
		"hai-harness-codemode-spill-789",
	} {
		if !isToolSpillDir(name) {
			t.Fatalf("%s 应被认成工具级落盘目录", name)
		}
	}
	for _, name := range []string{
		"hai-harness-toolspill", // 引擎级根目录：由 tools 包自己扫
		"my-build-dir",
		"hai-harness-sessions",
	} {
		if isToolSpillDir(name) {
			t.Fatalf("%s 不该被认成工具级落盘目录", name)
		}
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}
