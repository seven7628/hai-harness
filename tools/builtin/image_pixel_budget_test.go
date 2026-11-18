package builtin

// 图片路径的**有界化**测试（2026-11-17，第 4 步）。
//
// 三个契约：
//  1. **像素预算**（在解码之前判）：头部声明 30000×30000 的 PNG ⇒ 直接拒绝，
//     而不是整图解码成 3.6 GB 的 RGBA。
//  2. **原始文件体积上限**：读之前判（编码体积闸在读完之后才判 ⇒ 读时无保护）。
//  3. **降采样可中断 + 逐级**：降采样是最长的 CPU 段，ctx 取消要能打断。

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pngChunk 拼一个 PNG chunk（长度 + 类型 + 数据 + CRC）。
func pngChunk(typ string, data []byte) []byte {
	out := make([]byte, 0, 12+len(data))
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(data)))
	out = append(out, l[:]...)
	out = append(out, typ...)
	out = append(out, data...)
	crc := crc32.ChecksumIEEE(append([]byte(typ), data...))
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], crc)
	return append(out, c[:]...)
}

// fakePNGHeader 造一个**头部声明** w×h 的 PNG（数据是假的 —— 我们期望在解码前就被拒）。
func fakePNGHeader(w, h uint32) []byte {
	out := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	var ihdr []byte
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], w)
	ihdr = append(ihdr, b[:]...)
	binary.BigEndian.PutUint32(b[:], h)
	ihdr = append(ihdr, b[:]...)
	ihdr = append(ihdr, 8, 2, 0, 0, 0) // 8bit RGB
	out = append(out, pngChunk("IHDR", ihdr)...)
	out = append(out, pngChunk("IDAT", []byte{0x78, 0x9C, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01})...)
	out = append(out, pngChunk("IEND", nil)...)
	return out
}

// TestReadFileRejectsHugePixelCount 头部声明 30000×30000 的 PNG ⇒ **在解码前**拒绝。
//
// 改造前：只判「编码体积 ≤ 3 MiB」与「单边 ≤ 8192」—— 纯色 PNG 编码后可能只有
// 几 MB ⇒ 两道闸都过 ⇒ image.Decode 按原分辨率整图展开 = **3.6 GB** RGBA。
func TestReadFileRejectsHugePixelCount(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bomb.png")
	if err := os.WriteFile(p, fakePNGHeader(30000, 30000), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newReadFileTool(NewFileTools(dir))
	_, err := tool.Call(context.Background(), "read_file", fmt.Sprintf(`{"path":%q}`, p))
	if err == nil {
		t.Fatalf("超大像素图应当被拒绝")
	}
	if !strings.Contains(err.Error(), "too large to decode") {
		t.Errorf("错误应说明「太大不能解码」: %v", err)
	}
	// 错误文案要指路（模型的下一步动作）
	if !strings.Contains(err.Error(), "sips") {
		t.Errorf("错误应指路先缩小: %v", err)
	}
}

// TestReadFileRejectsHugeRawImage 原始文件超过上限 ⇒ 读之前就拒绝。
func TestReadFileRejectsHugeRawImage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "huge.png")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(fakePNGHeader(100, 100)); err != nil {
		t.Fatal(err)
	}
	// 稀疏放大到超过原始体积上限（内容无意义，我们期望在读之前就被拒）
	if err := f.Truncate(maxImageRawBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()

	tool := newReadFileTool(NewFileTools(dir))
	_, err = tool.Call(context.Background(), "read_file", fmt.Sprintf(`{"path":%q}`, p))
	if err == nil {
		t.Fatalf("超大原始文件应当被拒绝")
	}
	if !strings.Contains(err.Error(), "too large to read inline") {
		t.Errorf("错误应说明「太大读不进」: %v", err)
	}
}

// TestReadFileImageCancelled 已取消的 ctx ⇒ 图片路径立刻报错（不解码）。
func TestReadFileImageCancelled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.png")
	// 用真 PNG（1×1）—— 取消发生在解码之前。
	if err := os.WriteFile(p, mkPNG(t, 1, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := newReadFileTool(NewFileTools(dir))
	if _, err := tool.Call(ctx, "read_file", fmt.Sprintf(`{"path":%q}`, p)); err == nil {
		t.Errorf("已取消的 ctx 应当报错")
	}
}
