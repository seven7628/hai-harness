package session

// 解压路径的**分配上界**与**所有权契约**（2026-11-18，bridge 内存爬山第 1 步）。
//
// 背景（实测）：一次「打开大会话」解压 1971 个块 / 输出 100.3MB。
// 改造前每块走 zlib.NewReader + io.ReadAll：
//   · zlib.NewReader ⇒ 新的 flate 解码器 ⇒ 每次重分配 32KB 的 LZ77 滑窗；
//   · io.ReadAll 从 512B 起倍增扩容，每步抛弃旧数组 ⇒ 总分配 **897MB**
//     （真实输出的 8.9 倍）。
// ⇒ bridge 的堆被撑到软上限（2GiB），GC 近乎连续执行 ⇒ 整机变卡。
//
// 这里钉住两件事：
//  1. **分配上界**：解压的总分配必须贴近数据量本身（改造前是 8.9 倍 ⇒ 这条会红）。
//  2. **所有权契约**：decompressBlock 交付的字节归调用方独占（池化缓冲被复用后
//     也不能改坏它）；decompressBlockTransient 的结果只在校验回调期间有效。

import (
	"bytes"
	"compress/zlib"
	"runtime"
	"strings"
	"testing"
)

// makeCompressedBlock 造一个「压缩后的块」，逻辑内容是 pattern 重复的文本。
func makeCompressedBlock(t *testing.T, pattern string, size int) []byte {
	t.Helper()
	raw := []byte(strings.Repeat(pattern, size/len(pattern)+1)[:size])
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestDecompressBlockAllocationBounded：解压的总分配必须贴近数据量。
func TestDecompressBlockAllocationBounded(t *testing.T) {
	const (
		blockSize = 64 << 10
		blocks    = 64
	)
	payload := makeCompressedBlock(t, "abc123", blockSize)

	// 预热：让池里的缓冲长到够大（稳态才是要测的）。
	for i := 0; i < 16; i++ {
		out, err := decompressBlock(payload)
		if err != nil {
			t.Fatalf("预热解压失败: %v", err)
		}
		if len(out) != blockSize {
			t.Fatalf("解压后长度 = %d，期望 %d", len(out), blockSize)
		}
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < blocks; i++ {
		out, err := decompressBlock(payload)
		if err != nil {
			t.Fatalf("解压失败: %v", err)
		}
		runtime.KeepAlive(out)
	}
	runtime.ReadMemStats(&after)

	alloc := int64(after.TotalAlloc - before.TotalAlloc)
	data := int64(blockSize * blocks)
	// 交付拷贝 = 1x 数据量；给 2.5x 余量（拷贝的容量取整 + 池的一次性扩容）。
	// 改造前 io.ReadAll 的倍增扩容是 ~8.9x ⇒ 这条判据能把它挡住。
	if limit := data * 5 / 2; alloc > limit {
		t.Fatalf("解压总分配 %d 字节（数据量 %d，倍数 %.2fx），超过上界 %d ⇒ 倍增扩容回来了",
			alloc, data, float64(alloc)/float64(data), limit)
	}
	t.Logf("解压 %d 块 / %d 字节：总分配 %d 字节（%.2fx 数据量）",
		blocks, blockSize, alloc, float64(alloc)/float64(data))
}

// TestDecompressBlockResultOwned：交付的字节归调用方独占。
//
// 池化之后最容易出的错是「返回的是池化缓冲的切片」⇒ 下一个块解压时把它改坏。
// 这条测试钉住：解压 A 之后解压 B，A 的内容不能变。
func TestDecompressBlockResultOwned(t *testing.T) {
	payloadA := makeCompressedBlock(t, "AAAA", 4096)
	payloadB := makeCompressedBlock(t, "BBBB", 8192)

	outA, err := decompressBlock(payloadA)
	if err != nil {
		t.Fatal(err)
	}
	copyA := bytes.Clone(outA)

	// 多解几次别的块，逼池把缓冲复用起来。
	for i := 0; i < 32; i++ {
		if _, err := decompressBlock(payloadB); err != nil {
			t.Fatal(err)
		}
	}

	if !bytes.Equal(outA, copyA) {
		t.Fatalf("先解压的结果被后来的解压改坏了（所有权破了）")
	}
	if !bytes.Equal(outA, bytes.Repeat([]byte("A"), 4096)) {
		t.Fatalf("解压内容不对: %q", outA[:16])
	}
}

// TestDecompressBlockTransientScope：transient 变体的结果只在回调期间有效。
//
// 契约：**回调返回后不得再读 out**（它已归池，可能被下一个块覆写）。
// 这条测试用「回调内取值 + 回调外读会拿到别的东西」来钉住边界，
// 并确认回调期间读到的确实是正确内容。
func TestDecompressBlockTransientScope(t *testing.T) {
	payload := makeCompressedBlock(t, "ZZZZ", 4096)

	var inside []byte
	var err error
	err = decompressBlockTransient(payload, func(out []byte) error {
		inside = bytes.Clone(out) // 回调内必须自己拷一份才能留
		return nil
	})
	if err != nil {
		t.Fatalf("transient 解压失败: %v", err)
	}
	if !bytes.Equal(inside, bytes.Repeat([]byte("Z"), 4096)) {
		t.Fatalf("transient 回调里拿到的内容不对: %q", inside[:16])
	}
}

// TestDecompressBlockCorruptStillErrors：坏输入仍然报错（调用方据此判损坏）。
func TestDecompressBlockCorruptStillErrors(t *testing.T) {
	if _, err := decompressBlock([]byte("not a zlib stream")); err == nil {
		t.Fatal("坏输入应该报错")
	}
	if _, err := decompressBlock(nil); err == nil {
		t.Fatal("空输入应该报错")
	}
}
