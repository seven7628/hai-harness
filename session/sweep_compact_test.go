package session

// sidecar 死字节回收的行为测试（2026-11-19，爬 3）。
//
// 三条不变量：
//
//  1. **回收确实发生**：sidecar 变小（死字节被扔掉）。
//  2. **record 仍可读、块内容不变**：回收后用**真实读路径**（LoadRecordForView）
//     重读 ⇒ 读路径内部会校验 count 与 `blockset1:` sum
//     ⇒ 这条测试同时验证了「回收没破坏被引用的块」。
//  3. **幂等**：再跑一遍不再变小（needsCompact 变 false ⇒ 直接跳过）。

import (
	"context"
	"os"
	"testing"
	"time"
)

// bigBlock 造一个指定大小的块 JSON。
//
// 内容必须**不可压缩**：块是 zlib 压缩后存进 sidecar 的 ⇒ "AAAA..." 这种重复内容
// 会压成几 KB ⇒ sidecar 里根本没有死字节，测不出回收。
// ⇒ 用固定种子的伪随机串（确定性 + 不可压缩）。
func bigBlock(id int, tag string, size int) string {
	filler := randText(tag, size)
	return `{"id":` + itoa(id) + `,"kind":"assistant","status":"done","ts":100,"text":"` + filler + `"}`
}

// randText 生成确定性的伪随机文本（不可压缩）。
func randText(tag string, size int) string {
	// xorshift32：确定性、不依赖 math/rand 的全局状态。
	var x uint32 = 0x9e3779b9
	for _, c := range tag {
		x = x*31 + uint32(c)
	}
	if x == 0 {
		x = 0x12345678
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, 0, size)
	for len(out) < size {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		out = append(out, alphabet[x%uint32(len(alphabet))])
	}
	return string(out)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// TestSweepBlockStoreDeadBytesReclaimsAndKeepsRecord 可读。
func TestSweepBlockStoreDeadBytesReclaimsAndKeepsRecord(t *testing.T) {
	// 门槛调低（compactMinBytes 是 block store 自己的阈值，needsCompactAt 用它）
	oldMin := compactMinBytes.Load()
	compactMinBytes.Store(1 << 20) // 1MB
	defer compactMinBytes.Store(oldMin)

	root := t.TempDir()
	s, err := NewFileStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// ① 第一版：块 1 是 2MB 的内容
	v1 := sidecarTestView(t, `[`+bigBlock(1, "A", 2<<20)+`]`)
	if err := s.CommitRecord(ctx, sidecarRecord(t, 1, v1)); err != nil {
		t.Fatal(err)
	}
	// ② 第二版：**同一个 id** 内容变了 ⇒ sidecar 追加新版本，旧版本变死字节
	blockV2 := bigBlock(1, "B", 2<<20)
	v2 := sidecarTestView(t, `[`+blockV2+`]`)
	if err := s.CommitRecord(ctx, sidecarRecord(t, 2, v2)); err != nil {
		t.Fatal(err)
	}

	path, err := s.RecordPath("ws-a", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	sidecar := blockStorePath(path)
	sizeBefore, err := os.Stat(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("回收前 sidecar: %d 字节", sizeBefore.Size())

	// ③ 回收
	res, err := SweepBlockStoreDeadBytes(root, 1<<20, 0, nil)
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if res.VerifyErr != 0 {
		t.Fatalf("回收后重读校验失败 %d 个", res.VerifyErr)
	}
	sizeAfter, err := os.Stat(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("回收后 sidecar: %d 字节（回收 %d 个文件，省 %d 字节，用时 %s）",
		sizeAfter.Size(), res.Files, res.Saved(), res.Elapsed.Round(time.Millisecond))
	if sizeAfter.Size() >= sizeBefore.Size() {
		t.Fatalf("回收后没有变小: %d → %d", sizeBefore.Size(), sizeAfter.Size())
	}

	// ④ 真实读路径重读（读路径内部校验 count + blockset1 sum）
	rec, err := s.LoadRecordForView(ctx, "ws-a", "sess-1")
	if err != nil {
		t.Fatalf("回收后 record 读不出来: %v", err)
	}
	defer rec.ReleaseView()
	if len(rec.LoadedBlockSpans) != 1 {
		t.Fatalf("应有 1 个块，得 %d", len(rec.LoadedBlockSpans))
	}
	var got string
	if err := rec.LoadedBlockSpans[0].LogicalTransient(func(raw []byte) error {
		got = string(raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got != blockV2 {
		t.Fatalf("块内容不是第二版（回收搬错了版本）: %.80s", got)
	}

	// ⑤ 幂等：再跑一遍不再变小
	res2, err := SweepBlockStoreDeadBytes(root, 1<<20, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	sizeAgain, _ := os.Stat(sidecar)
	if res2.Files != 0 {
		t.Fatalf("第二次回收又动了文件（不幂等）: %d", res2.Files)
	}
	if sizeAgain.Size() != sizeAfter.Size() {
		t.Fatalf("第二次回收后体积变了: %d → %d", sizeAfter.Size(), sizeAgain.Size())
	}
	t.Log("✅ 回收后 record 可读、块内容正确、且幂等")
}

// TestSweepBlockStoreDeadBytesSkipsSmallFiles：小文件不碰（打开可能要全量扫索引）。
func TestSweepBlockStoreDeadBytesSkipsSmallFiles(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v := sidecarTestView(t, `[`+bigBlock(1, "A", 1024)+`]`)
	if err := s.CommitRecord(ctx, sidecarRecord(t, 1, v)); err != nil {
		t.Fatal(err)
	}
	res, err := SweepBlockStoreDeadBytes(root, 32<<20, 0, nil) // 门槛 32MB
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 0 {
		t.Fatalf("小文件不该被回收: %d", res.Files)
	}
}

// TestSweepBlockStoreDeadBytesRemovesOrphanTmp：老的孤儿 .compact.tmp 被清掉，
// 新的（正在跑的）不动。
func TestSweepBlockStoreDeadBytesRemovesOrphanTmp(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v := sidecarTestView(t, `[`+bigBlock(1, "A", 1024)+`]`)
	if err := s.CommitRecord(ctx, sidecarRecord(t, 1, v)); err != nil {
		t.Fatal(err)
	}
	path, _ := s.RecordPath("ws-a", "sess-1")
	sidecar := blockStorePath(path)

	old := sidecar + ".compact.tmp"
	if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	fresh := sidecar + ".fresh.compact.tmp"
	if err := os.WriteFile(fresh, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := SweepBlockStoreDeadBytes(root, 32<<20, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.OrphanTmp != 1 {
		t.Fatalf("应清掉 1 个孤儿 tmp，得 %d", res.OrphanTmp)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("孤儿 tmp 还在")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("新 tmp 被误删了（正在跑的回收会被打断）: %v", err)
	}
}
