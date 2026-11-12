package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReplaceCreatesParentsAndWrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b", "f.txt")
	if err := Replace(p, "hello"); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q", got)
	}
	// 不留临时文件
	ents, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".atomic-") {
			t.Fatalf("残留临时文件 %s", e.Name())
		}
	}
	// 文件 0644、目录 0755（两个常量都不许漂）
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("file mode = %v", st.Mode().Perm())
	}
	dst, _ := os.Stat(filepath.Dir(p))
	if dst.Mode().Perm() != 0o755 {
		t.Fatalf("dir mode = %v", dst.Mode().Perm())
	}
	if ErrLocked.Error() != "file is locked by another writer" {
		t.Fatalf("ErrLocked 文案漂了：%q", ErrLocked.Error())
	}
}

func TestReplaceOverwritesAtomically(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := Replace(p, strings.Repeat("x", 1024)); err != nil {
		t.Fatal(err)
	}
	if err := Replace(p, "short"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "short" {
		t.Fatalf("覆盖后 content = %q", got)
	}
}

func TestAppendCreatesParentsAndAppends(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x", "y", "log.txt")
	if err := Append(p, "a"); err != nil {
		t.Fatal(err)
	}
	if err := Append(p, "b"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "ab" {
		t.Fatalf("content = %q", got)
	}
}

func TestLockPathMissingFileReturnsNil(t *testing.T) {
	if lock, err := LockPath(filepath.Join(t.TempDir(), "nope")); err != nil || lock != nil {
		t.Fatalf("missing file: lock=%v err=%v", lock, err)
	}
}

func TestLockIsExclusiveAcrossHandles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := LockPath(p)
	if err != nil || first == nil {
		t.Fatalf("first lock: %v", err)
	}
	defer first.Close()
	// 同一进程的另一个 fd 也拿不到（flock 按 open file description 计）。
	// 放 goroutine + 超时：若有人把 LOCK_NB 去掉改成阻塞锁，这里 2s 内报错而不是挂到 go test 超时。
	done := make(chan error, 1)
	go func() {
		second, err := LockPath(p)
		if second != nil {
			second.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("第二个写者不应拿到锁")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("第二个写者阻塞超过 2s：锁变成了阻塞锁（应为 LOCK_NB）")
	}
}

func TestReplaceLockedWithoutLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := ReplaceLocked(nil, p, "nolock"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "nolock" {
		t.Fatalf("content = %q", got)
	}
}

func TestConcurrentAppendsAreNotInterleaved(t *testing.T) {
	// 每个写者用**不同长度、不同内容**的整行 payload：
	//   - 若 Append 丢掉 O_APPEND 改成截断写，最终只剩一条 → 行数 != 成功数，红；
	//   - 若无锁导致两条交错，会出现半行 → 逐行校验失败，红；
	//   - 非阻塞锁语义下允许部分调用返回 ErrLocked，只统计**成功数**并核对行数。
	const n = 24
	p := filepath.Join(t.TempDir(), "log.txt")
	payloads := make([]string, n)
	valid := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		payloads[i] = fmt.Sprintf("%02d-%s\n", i, strings.Repeat(string(rune('a'+i%26)), 40+i)) // 长度各不相同
		valid[payloads[i]] = true
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	okN := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Append(p, payloads[i]); err == nil {
				mu.Lock()
				okN++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if okN == 0 {
		t.Fatal("一次都没写成功：锁或 O_APPEND 实现有问题")
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != okN {
		t.Fatalf("成功 %d 次但文件有 %d 行（丢写或截断式写入）\n%q", okN, len(lines), got)
	}
	for _, l := range lines {
		if !valid[l+"\n"] {
			t.Fatalf("出现被截断/交错的半行：%q", l)
		}
	}
}
