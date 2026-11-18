package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/tools"
)

func mkPNG(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mkNoisePNG 伪随机噪声图（PNG 压不动）：用于触发体积闸门的降采样路径。
func mkNoisePNG(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(12345)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = seed*1664525 + 1013904223
			img.Set(x, y, color.RGBA{R: uint8(seed >> 24), G: uint8(seed >> 16), B: uint8(seed >> 8), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReadFileImageEmbeds(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(p, mkPNG(t, 40, 30), 0o644); err != nil {
		t.Fatal(err)
	}
	f := NewFileTools(dir)
	tool := newReadFileTool(f)
	sink := &tools.ImageSink{}
	out, err := tool.Call(tools.WithImageSink(context.Background(), sink), "read_file", `{"path":"shot.png"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	t.Logf("text: %s", out)
	imgs := sink.Images()
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
	if !strings.HasPrefix(imgs[0].Content, "data:image/png;base64,") {
		t.Fatalf("bad data URL: %.40s", imgs[0].Content)
	}
	if imgs[0].MimeType != "image/png" {
		t.Fatalf("mime = %s", imgs[0].MimeType)
	}
}

func TestReadFileImageDownscalesOverLimit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.png")
	// 3000x2000 噪声图：PNG 压不动 → 必然超 3MiB 闸门 → 走降采样
	big := mkNoisePNG(t, 3000, 2000)
	if err := os.WriteFile(p, big, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("original png = %.1fMB", float64(len(big))/(1<<20))
	tool := newReadFileTool(NewFileTools(dir))
	tool.maxImageBytes = 512 * 1024 // 强制触发降采样
	sink := &tools.ImageSink{}
	out, err := tool.Call(tools.WithImageSink(context.Background(), sink), "read_file", `{"path":"big.png"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	t.Logf("text: %s", out)
	imgs := sink.Images()
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
	if !strings.HasPrefix(imgs[0].Content, "data:image/jpeg;base64,") {
		t.Fatalf("expected jpeg re-encode: %.40s", imgs[0].Content)
	}
	if !strings.Contains(out, "downscaled") {
		t.Fatalf("text should mention downscale: %s", out)
	}
	// 验证确实是合法、可解码的图（不是损坏数据）
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(imgs[0].Content, "data:image/jpeg;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("re-encoded jpeg not decodable: %v", err)
	}
	t.Logf("re-encoded %dx%d, %.0fKB", cfg.Width, cfg.Height, float64(len(raw))/1024)
	if cfg.Width > maxImageDim || cfg.Height > maxImageDim {
		t.Fatalf("long edge %d exceeds maxImageDim %d", cfg.Width, maxImageDim)
	}
}

func TestReadFileRejectsBinary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("bin\x00ary\x01\x02\x03"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newReadFileTool(NewFileTools(dir))
	sink := &tools.ImageSink{}
	_, err := tool.Call(tools.WithImageSink(context.Background(), sink), "read_file", `{"path":"a.bin"}`)
	if err == nil {
		t.Fatal("binary must be rejected")
	}
	t.Logf("err: %v", err)
	if len(sink.Images()) != 0 {
		t.Fatal("no images for binary")
	}
}

func TestReadFileImageRejectsLineRange(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s.png"), mkPNG(t, 10, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newReadFileTool(NewFileTools(dir))
	_, err := tool.Call(context.Background(), "read_file", `{"path":"s.png","start_line":1,"end_line":5}`)
	if err == nil || !strings.Contains(err.Error(), "line ranges do not apply") {
		t.Fatalf("want range rejection, got %v", err)
	}
	t.Logf("err: %v", err)
}
