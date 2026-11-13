package session

// 清扫的幂等性：压不动的 sidecar 不该被反复重写。
//
// ## 回归（2026-11-13）
//
// 老判据是「有未压缩块 ⇒ 需要重写」。实测存量里剩下的未压缩块 **100% 压不动**
// （内容本身是随机/二进制，zlib 压了反而更大）⇒ 判据永远为真 ⇒
// **每次启动**都把整份 sidecar 解压一遍、压一遍、重写一遍
// （实测 10.5s + 96MB 重写），而文件内容一个字节都没变。
//
// 后果有两层：白烧 CPU/IO；重写改了 mtime ⇒ 清扫索引的指纹失效 ⇒
// 下次启动照样重扫 —— **索引本来能把这轮整个跳过的**。
//
// 现在判据是「有未压缩**且压得动**的块」。

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// randomBlock 造一块「压不动」的内容（随机字节）。
func randomBlock(t *testing.T, n int) []byte {
	t.Helper()
	r := rand.New(rand.NewSource(int64(n)))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	return b
}

// writeSidecar 写一个只有指定块的 sidecar，返回路径。
func writeSidecar(t *testing.T, dir string, blocks map[string][]byte) string {
	t.Helper()
	path := filepath.Join(dir, "s-x.record.json")
	bs, err := openBlockStore(blockStorePath(path))
	if err != nil {
		t.Fatal(err)
	}
	for id, payload := range blocks {
		if err := bs.put(id, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if err := bs.Close(); err != nil {
		t.Fatal(err)
	}
	return blockStorePath(path)
}

// TestSweepDoesNotRewriteIncompressible 压不动的 sidecar：清扫**不许**动它。
func TestSweepDoesNotRewriteIncompressible(t *testing.T) {
	root := t.TempDir()
	sidecar := writeSidecar(t, root, map[string][]byte{
		"1": randomBlock(t, 4096),
		"2": randomBlock(t, 8192),
	})
	before, err := os.Stat(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	// 确保 mtime 有分辨率（有些文件系统 1s 精度）。
	time.Sleep(1100 * time.Millisecond)

	res, err := SweepBlockStores(root, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rewritten != 0 {
		t.Fatalf("压不动的 sidecar 不该被重写，却重写了 %d 个", res.Rewritten)
	}
	after, err := os.Stat(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("文件大小变了：%d → %d", before.Size(), after.Size())
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("mtime 变了（%v → %v）—— 清扫索引的指纹会失效 ⇒ 下次启动照样重扫",
			before.ModTime(), after.ModTime())
	}
}

// TestSweepCompressesCompressible 能压的块必须被压，且**只数新压上的**。
func TestSweepCompressesCompressible(t *testing.T) {
	root := t.TempDir()
	// 写入时**关掉压缩** ⇒ sidecar 里留下未压缩块（存量形态）。
	// （新写入的块在 put 时就压好了 ⇒ 没有「待压」的东西，清扫本就该什么都不做。）
	old := blockCompressEnabled
	blockCompressEnabled = false
	sidecar := writeSidecar(t, root, map[string][]byte{
		// 要 >512B 才会尝试压缩（compressMinBytes）。
		"1": []byte(strings.Repeat("可压缩的内容", 100)),
		"2": randomBlock(t, 4096),
	})
	blockCompressEnabled = old
	defer func() { blockCompressEnabled = old }()

	res, err := SweepBlockStores(root, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rewritten != 1 {
		t.Fatalf("应重写 1 个文件，得 %d", res.Rewritten)
	}
	// 计数：新压上的只有 1 个（那个重复内容的块），不是全部块数。
	if res.Blocks != 1 {
		t.Fatalf("新压上的块数=%d want 1（老口径会把已压过的块也算进去）", res.Blocks)
	}
	// 压完后再扫一次：不该再重写（内容已经压不动了）。
	res2, err := SweepBlockStores(root, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Rewritten != 0 {
		t.Fatalf("第二次清扫不该重写，却重写了 %d 个", res2.Rewritten)
	}
	_ = sidecar
}
