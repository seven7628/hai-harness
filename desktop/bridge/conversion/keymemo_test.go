package conversion

// 两级内容键的回归网（第 4 轮爬山：命中缓存也要把整个文件读一遍算 sha256）。
//
// 契约：
//  1. 备忘返回的键与 CacheKey **逐字相同**（缓存目录结构与命中语义不变）；
//  2. 命中备忘时**不读文件内容**（文件不可读时仍能拿到键）；
//  3. 内容变了 ⇒ 键必须变（哪怕 mtime 被还原 —— 靠 ctime 兜住）。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFile 写一个临时文件并返回路径。
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestKeyMemoSameKeyAsCacheKey：备忘的键必须与 CacheKey 逐字相同。
func TestKeyMemoSameKeyAsCacheKey(t *testing.T) {
	p := writeFile(t, "a.xlsx", "hello")
	km := newKeyMemo(0)

	got, err := km.ContentKey("xlsx", "a.xlsx", nil, p)
	if err != nil {
		t.Fatal(err)
	}
	want, err := CacheKey("xlsx", "a.xlsx", nil, p)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("备忘键 = %s，CacheKey = %s，必须逐字相同", got, want)
	}
}

// TestKeyMemoHitSkipsContentHash：命中备忘时**不读文件内容**（不真算 sha256）。
//
// 判据用钩子（keyMemo.hook）而不是「把文件 chmod 成不可读」—— 后者行不通：
// chmod 本身会改 ctime ⇒ L1 变 ⇒ 备忘自然失配（这恰恰是下面另一条用例要断言的
// 正确行为）。钩子直接证明「走的是备忘分支，没有调用 CacheKey」。
func TestKeyMemoHitSkipsContentHash(t *testing.T) {
	p := writeFile(t, "b.xlsx", "hello")
	km := newKeyMemo(0)
	var hits, misses int
	km.hook = func(hit bool) {
		if hit {
			hits++
		} else {
			misses++
		}
	}
	if _, err := km.ContentKey("xlsx", "b.xlsx", nil, p); err != nil {
		t.Fatal(err)
	}
	if _, err := km.ContentKey("xlsx", "b.xlsx", nil, p); err != nil {
		t.Fatal(err)
	}
	if hits != 1 || misses != 1 {
		t.Fatalf("命中 %d 次 / 未命中 %d 次，期望 1/1（第二次必须走备忘）", hits, misses)
	}
}

// TestKeyMemoInvalidatesOnMetadataChange：元数据变了（chmod 也会改 ctime）⇒
// 备忘必须失配重算。宁可多算一次，也不拿旧键去命中旧产物。
func TestKeyMemoInvalidatesOnMetadataChange(t *testing.T) {
	p := writeFile(t, "h.xlsx", "hello")
	km := newKeyMemo(0)
	k1, err := km.ContentKey("xlsx", "h.xlsx", nil, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	var hits, misses int
	km.hook = func(hit bool) {
		if hit {
			hits++
		} else {
			misses++
		}
	}
	k2, err := km.ContentKey("xlsx", "h.xlsx", nil, p)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatal("内容没变而键变了：内容键只该由内容决定（chmod 不该影响键）")
	}
	// ctime 变了 ⇒ L1 变了 ⇒ 备忘必须失配重算（miss），但键仍由内容决定（不变）。
	if misses != 1 || hits != 0 {
		t.Fatalf("chmod 后应失配重算（miss 1 次），却 miss %d / hit %d", misses, hits)
	}
	// 重算后备忘被刷新 ⇒ 下一次走快路径。
	if _, err := km.ContentKey("xlsx", "h.xlsx", nil, p); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("重算后应命中备忘，却 hit %d 次", hits)
	}
}

// TestKeyMemoInvalidatesOnContentChange：内容变了 ⇒ 键必须变。
func TestKeyMemoInvalidatesOnContentChange(t *testing.T) {
	p := writeFile(t, "c.xlsx", "v1")
	km := newKeyMemo(0)
	k1, err := km.ContentKey("xlsx", "c.xlsx", nil, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("v2-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	k2, err := km.ContentKey("xlsx", "c.xlsx", nil, p)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatal("内容变了但键没变 ⇒ 会命中旧产物（给用户看错内容）")
	}
}

// TestKeyMemoInvalidatesWhenMtimePreserved：mtime 被**还原**（cp -p / 解压的常见情形）
// 时，键仍必须变 —— 靠的是 ctime（用户改不了）。
func TestKeyMemoInvalidatesWhenMtimePreserved(t *testing.T) {
	p := writeFile(t, "d.xlsx", "v1")
	km := newKeyMemo(0)
	k1, err := km.ContentKey("xlsx", "d.xlsx", nil, p)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	mt := fi.ModTime()
	if err := os.WriteFile(p, []byte("v2-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 把 mtime 还原回去（模拟 cp -p / 解压保留时间戳）
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	k2, err := km.ContentKey("xlsx", "d.xlsx", nil, p)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatal("mtime 被还原时键仍没变 ⇒ ctime 没起作用")
	}
}

// TestKeyMemoDifferentParamsDifferentKeys：kind / 文件名 / 参数任一不同 ⇒ 键不同。
func TestKeyMemoDifferentParamsDifferentKeys(t *testing.T) {
	p := writeFile(t, "e.xlsx", "x")
	km := newKeyMemo(0)
	a, err := km.ContentKey("xlsx", "e.xlsx", nil, p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := km.ContentKey("docx", "e.xlsx", nil, p)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("kind 不同却算出同一个键")
	}
	c, err := km.ContentKey("xlsx", "e.xlsx", map[string]string{"dpi": "144"}, p)
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Fatal("参数不同却算出同一个键")
	}
}

// TestKeyMemoEvictsOldest：容量有界（超了淘汰最旧的）。
func TestKeyMemoEvictsOldest(t *testing.T) {
	km := newKeyMemo(2)
	p1 := writeFile(t, "f1.xlsx", "1")
	p2 := writeFile(t, "f2.xlsx", "2")
	p3 := writeFile(t, "f3.xlsx", "3")
	for _, p := range []string{p1, p2, p3} {
		if _, err := km.ContentKey("xlsx", filepath.Base(p), nil, p); err != nil {
			t.Fatal(err)
		}
	}
	if km.max != 2 {
		t.Fatalf("容量 = %d，期望 2", km.max)
	}
	km.mu.Lock()
	n := len(km.m)
	km.mu.Unlock()
	if n > 2 {
		t.Fatalf("备忘条目 = %d，超过容量 %d", n, km.max)
	}
}

// TestStatKeyChangesWithContent：StatKey 本身要能分辨内容变化。
func TestStatKeyChangesWithContent(t *testing.T) {
	p := writeFile(t, "g.xlsx", "v1")
	s1, err := StatKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("v2-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 至少等一纳秒级的 mtime 变化不可靠 ⇒ 显式错开 1ms
	time.Sleep(2 * time.Millisecond)
	s2, err := StatKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if s1 == s2 {
		t.Fatal("内容变了但 StatKey 没变")
	}
}

// BenchmarkCacheKeyBigFile vs BenchmarkContentKeyMemoHit：命中缓存时省掉的那次
// 全文件读盘 + 哈希（50 MiB 文件；真实场景里 100 MiB 的 pptx/xlsx 也不罕见）。
func BenchmarkCacheKeyBigFile(b *testing.B) {
	p := filepath.Join(b.TempDir(), "big.xlsx")
	data := make([]byte, 50<<20)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := CacheKey("xlsx", "big.xlsx", nil, p); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkContentKeyMemoHit(b *testing.B) {
	p := filepath.Join(b.TempDir(), "big.xlsx")
	data := make([]byte, 50<<20)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		b.Fatal(err)
	}
	km := newKeyMemo(0)
	if _, err := km.ContentKey("xlsx", "big.xlsx", nil, p); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := km.ContentKey("xlsx", "big.xlsx", nil, p); err != nil {
			b.Fatal(err)
		}
	}
}
