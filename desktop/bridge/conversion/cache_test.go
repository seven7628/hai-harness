package conversion

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeEntry 造一个缓存条目（index.html 大小用 Truncate 造稀疏文件，不真占磁盘）。
func makeEntry(t *testing.T, dir, key string, size int64, mod time.Time) string {
	t.Helper()
	d := filepath.Join(dir, key)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(d, indexName))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Skipf("本机不支持稀疏文件: %v", err)
	}
	f.Close()
	if err := os.Chtimes(d, mod, mod); err != nil {
		t.Fatal(err)
	}
	return d
}

// ⑦清理：25h 前的目录按年龄删掉；总量超 512 MiB 时按 LRU 删到预算内。
func TestSweepExpiresOldAndTrimsToLRUBudget(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	const mib = int64(1 << 20)
	expired := makeEntry(t, dir, "expired", 1<<10, now.Add(-25*time.Hour))
	oldest := makeEntry(t, dir, "oldest", 300*mib, now.Add(-3*time.Hour))
	middle := makeEntry(t, dir, "middle", 300*mib, now.Add(-2*time.Hour))
	newest := makeEntry(t, dir, "newest", 300*mib, now.Add(-1*time.Hour))

	c := NewCache(dir, DefaultMaxCacheBytes, DefaultMaxCacheAge)
	st, err := c.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	for _, p := range []string{expired, oldest, middle} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("该删的没删: %s（err=%v）", filepath.Base(p), err)
		}
	}
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("最新条目被误删: %v", err)
	}
	if st.Removed != 3 {
		t.Fatalf("Removed = %d，期望 3（1 过期 + 2 LRU）", st.Removed)
	}
	if st.Entries != 1 || st.Bytes != 300*mib {
		t.Fatalf("现存 %d 条 / %d 字节，期望 1 条 / %d 字节", st.Entries, st.Bytes, 300*mib)
	}
	if st.Freed < 600*mib {
		t.Fatalf("Freed = %d，期望 ≥ %d", st.Freed, 600*mib)
	}
}

// 缓存目录还不存在时清扫不算错误（首次启动的常态）。
func TestSweepMissingDirIsFine(t *testing.T) {
	c := NewCache(filepath.Join(t.TempDir(), "nope"), DefaultMaxCacheBytes, DefaultMaxCacheAge)
	st, err := c.Sweep()
	if err != nil || st != (Stats{}) {
		t.Fatalf("Sweep(不存在) = %+v, %v", st, err)
	}
}

// 半成品条目不算命中：index.html 是写入的最后一步，它不在 = 上次转换没跑完。
func TestLookupRejectsPartialEntry(t *testing.T) {
	dir := t.TempDir()
	key := "abc"
	d := filepath.Join(dir, key)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	// 只剩下 atomicfile 的临时文件（进程在 rename 之前被杀）
	if err := os.WriteFile(filepath.Join(d, ".atomic-123"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCache(dir, DefaultMaxCacheBytes, DefaultMaxCacheAge)
	if p, ok := c.Lookup(key); ok {
		t.Fatalf("半成品条目被当成命中: %s", p)
	}
}

// 键的三个维度：内容、文件名、kind 任一变化都必须换键。
func TestCacheKeyDimensions(t *testing.T) {
	dir := t.TempDir()
	a := makeSrc(t, dir, "a.xlsx", "same-bytes")
	b := makeSrc(t, dir, "b.xlsx", "same-bytes")
	c := makeSrc(t, t.TempDir(), "a.xlsx", "other-bytes") // 同名不同内容（同目录会覆盖 a）

	k1, err := CacheKey("xlsx", "a.xlsx", nil, a)
	if err != nil {
		t.Fatal(err)
	}
	k1again, _ := CacheKey("xlsx", "a.xlsx", nil, a)
	if k1 != k1again {
		t.Fatal("同一输入两次算键不一致")
	}
	if k2, _ := CacheKey("xlsx", "b.xlsx", nil, b); k2 == k1 {
		t.Fatal("文件名未进键（产物 <title> 会写成旧名字）")
	}
	if k3, _ := CacheKey("pptx", "a.xlsx", nil, a); k3 == k1 {
		t.Fatal("kind 未进键（不同格式共用产物）")
	}
	if k4, _ := CacheKey("xlsx", "a.xlsx", nil, c); k4 == k1 {
		t.Fatal("内容未进键")
	}
}

func makeSrc(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
