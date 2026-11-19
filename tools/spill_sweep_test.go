package tools

// 引擎落盘目录的**回收**测试（2026-11-17，第 5 步）。
//
// 问题：spillText 每次截断写一个文件，但**没有任何清理器** ⇒ 目录只增不减
// （引擎注释自陈「实测本机历史里 680 条结果被截断、累计丢掉 1.17 GB」）。
//
// 现在：低频异步清扫 —— ① 超过 TTL 的文件删掉；② 总量超上限就按最旧先删。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSpillFile 造一个落盘文件（可指定 mtime）。
func writeSpillFile(t *testing.T, dir, name string, size int, mod time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSweepSpillDirRemovesExpired 过期文件被删、新的留下。
func TestSweepSpillDirRemovesExpired(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := writeSpillFile(t, dir, "spill-old.txt", 1024, now.Add(-48*time.Hour))
	fresh := writeSpillFile(t, dir, "spill-new.txt", 1024, now.Add(-time.Hour))

	sweepSpillDir(dir, now, spillMaxTotalBytes)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("过期文件应被删除（还在: %v）", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("未过期文件应保留: %v", err)
	}
}

// TestSweepSpillDirEnforcesTotalCap 总量超上限 ⇒ 按「最旧先删」回收。
func TestSweepSpillDirEnforcesTotalCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	const size = 1024
	oldest := writeSpillFile(t, dir, "spill-a.txt", size, now.Add(-3*time.Hour))
	middle := writeSpillFile(t, dir, "spill-b.txt", size, now.Add(-2*time.Hour))
	newest := writeSpillFile(t, dir, "spill-c.txt", size, now.Add(-time.Hour))

	// 上限 1 KB：三个 1 KB 文件共 3 KB ⇒ 超限 ⇒ 按最旧先删（删到 ≤ 1 KB 为止）
	// ⇒ 最旧的两个被删，只剩最新的。
	sweepSpillDir(dir, now, size)

	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("最旧的文件应被优先删除（还在: %v）", err)
	}
	if _, err := os.Stat(middle); !os.IsNotExist(err) {
		t.Errorf("次旧的文件应被删除（还在: %v）", err)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Errorf("最新的文件应保留: %v", err)
	}
}

// TestSweepSpillDirKeepsDirectoryEntries 子目录不参与回收（只处理文件）。
func TestSweepSpillDirKeepsDirectoryEntries(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	// 子目录里放一个过期文件
	writeSpillFile(t, sub, "spill-old.txt", 10, now.Add(-72*time.Hour))
	sweepSpillDir(dir, now, spillMaxTotalBytes)
	if _, err := os.Stat(sub); err != nil {
		t.Errorf("子目录不该被删: %v", err)
	}
}

// TestSweepSpillRootRemovesExpiredSessionDirs 会话目录按 TTL 回收。
//
// 钉住「**会话级**」的语义（2026-11-18 资源有界化第 2 步）：根目录下的每个
// 会话目录都被扫一遍 —— 新鲜文件留着（模型读得到）、过期文件删、空目录删。
//
// 为什么值得单独测：改造前清扫的触发点挂在 **Engine** 上（只在「又写了一个新
// 文件」时才扫）⇒ 会话一死，最后一次清扫永远不发生 ⇒ 会话目录永久留下
// （实测 72 个 / 15MB）。现在触发点挂在**会话**上（宿主显式调 SweepSpillRoot）。
func TestSweepSpillRootRemovesExpiredSessionDirs(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	// ① 有新鲜文件的会话目录 ⇒ 保留（模型可能还要读那个路径）。
	keep := filepath.Join(root, "sess-fresh")
	if err := os.MkdirAll(keep, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSpillFile(t, keep, "spill-a.txt", 1024, now.Add(-time.Hour))

	// ② 全是过期文件的会话目录 ⇒ 删。
	expired := filepath.Join(root, "sess-expired")
	if err := os.MkdirAll(expired, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSpillFile(t, expired, "spill-b.txt", 1024, now.Add(-48*time.Hour))

	// ③ 空目录 ⇒ 删（空壳）。
	empty := filepath.Join(root, "sess-empty")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}

	SweepSpillRoot(root)

	// 清扫是异步的 ⇒ 轮询等它落地。
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, keepErr := os.Stat(keep)
		_, expiredErr := os.Stat(expired)
		_, emptyErr := os.Stat(empty)
		if keepErr == nil && os.IsNotExist(expiredErr) && os.IsNotExist(emptyErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("清扫未生效：keep=%v expired=%v empty=%v", keepErr, expiredErr, emptyErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(keep, "spill-a.txt")); err != nil {
		t.Fatalf("TTL 内的文件必须留着（模型读得到）: %v", err)
	}
}
