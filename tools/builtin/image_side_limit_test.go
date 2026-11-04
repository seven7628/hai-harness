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

// 单边 8192px 闸门的回归测试。
//
// 为什么要单独一个文件：本仓大量 *_test.go 被 .git/info/exclude 排除（见项目记忆
// clean-checkout-test-breakage），写进那些文件的用例不会随提交进库、干净检出也跑不到。
// 本文件的 helper 全部自带（不复用被排除文件里的 mkPNG 等），保证别处 checkout 能独立编译。

func mkSideGatePNG(t *testing.T, w, h int) []byte {
	t.Helper()
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

// TestReadFileImageSlicesTallImage 窄高全页截图（单边 > maxImageSidePx）必须**切片**
// 而不是原样内嵌，也不是等比缩成一条细线。2026-11-04 实证：390x9697 的合法 PNG
// （837KB，体积闸门放行）被上游以「unsupported image」400 拒收 —— 真判据是单边
// >8192（实测 390x8192 → 200 / 390x8193 → 400），与文件大小、格式无关。
func TestReadFileImageSlicesTallImage(t *testing.T) {
	dir := t.TempDir()
	// 100x8300：单边 8300 > 8192，PNG 体积远小于 3MiB 闸门 → 走切片路径。
	// 用窄宽而不是 390 是为了让用例快（像素数少一个数量级）。
	raw := mkSideGatePNG(t, 100, 8300)
	if encodedLen(len(raw)) > maxInlineImageBytes {
		t.Fatalf("test fixture too big: %d", len(raw))
	}
	p := filepath.Join(dir, "tall.png")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newReadFileTool(NewFileTools(dir))
	sink := &tools.ImageSink{}
	out, err := tool.Call(tools.WithImageSink(context.Background(), sink), "read_file", `{"path":"tall.png"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	t.Logf("text: %s", out)
	imgs := sink.Images()
	if len(imgs) != 2 {
		t.Fatalf("want 2 tiles, got %d", len(imgs))
	}
	if !strings.Contains(out, "split into 2 tiles") {
		t.Fatalf("text should explain the split: %s", out)
	}
	if !strings.Contains(out, "100x8300") {
		t.Fatalf("text should keep the original size: %s", out)
	}
	totalH := 0
	for i, im := range imgs {
		body := strings.TrimPrefix(im.Content, "data:"+im.MimeType+";base64,")
		b, derr := base64.StdEncoding.DecodeString(body)
		if derr != nil {
			t.Fatalf("tile %d not base64: %v", i, derr)
		}
		cfg, _, derr := image.DecodeConfig(bytes.NewReader(b))
		if derr != nil {
			t.Fatalf("tile %d not decodable: %v", i, derr)
		}
		if cfg.Width > maxImageSidePx || cfg.Height > maxImageSidePx {
			t.Fatalf("tile %d %dx%d exceeds maxImageSidePx %d", i, cfg.Width, cfg.Height, maxImageSidePx)
		}
		if cfg.Width != 100 {
			t.Fatalf("tile %d width = %d, want native 100 (must not shrink horizontally)", i, cfg.Width)
		}
		if im.MimeType != "image/png" {
			t.Fatalf("tile %d mime = %s, want lossless png", i, im.MimeType)
		}
		totalH += cfg.Height
	}
	if totalH != 8300 {
		t.Fatalf("tiles cover %d rows, want all 8300 (no content dropped)", totalH)
	}
}

// TestReadFileImageKeepsNormalTallScreenshot 单边未超 8192 的常规长截图保持原样内嵌
// （1440x4151 / 2560x5644 这类上游本来就收，不要为了 maxImageDim 把它们砍糊）。
func TestReadFileImageKeepsNormalTallScreenshot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.png"), mkSideGatePNG(t, 200, 3000), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newReadFileTool(NewFileTools(dir))
	sink := &tools.ImageSink{}
	out, err := tool.Call(tools.WithImageSink(context.Background(), sink), "read_file", `{"path":"page.png"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := len(sink.Images()); got != 1 {
		t.Fatalf("want 1 image, got %d", got)
	}
	if !strings.Contains(out, "embedded as-is") || !strings.Contains(out, "200x3000") {
		t.Fatalf("normal tall screenshot should stay as-is: %s", out)
	}
}

// TestReadFileImageUndecodableFormatStaysInline 标准库没有解码器的格式（webp/bmp/heic）
// 读不出尺寸，但体积达标时仍应原样内嵌（webp 本就在上游支持列表里）—— 单边闸门
// 不能因为「量不到边长」就把它们误杀。
func TestReadFileImageUndecodableFormatStaysInline(t *testing.T) {
	dir := t.TempDir()
	// 最小 RIFF/WEBP 头（detectImageFormat 只认魔数；内容不需要真能解码）
	webp := []byte("RIFF\x1a\x00\x00\x00WEBPVP8 \x0e\x00\x00\x00\x2f\x00\x00\x00\x10\x07\x10\x11\x11\x88\x88\xfe\x07\x00")
	if err := os.WriteFile(filepath.Join(dir, "a.webp"), webp, 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newReadFileTool(NewFileTools(dir))
	sink := &tools.ImageSink{}
	out, err := tool.Call(tools.WithImageSink(context.Background(), sink), "read_file", `{"path":"a.webp"}`)
	if err != nil {
		t.Fatalf("webp 体积达标应原样内嵌，不该报错: %v", err)
	}
	imgs := sink.Images()
	if len(imgs) != 1 || imgs[0].MimeType != "image/webp" {
		t.Fatalf("want 1 webp image, got %+v", imgs)
	}
	if !strings.Contains(out, "embedded as-is") {
		t.Fatalf("text: %s", out)
	}
}

// TestReadFileImageCorruptPNGFailsLoudly 头坏掉的 PNG（可解码格式但读不出尺寸）不原样
// 发出去 —— 以前「体积达标就内嵌」会把坏图送到上游换一个 400，现在直接报解码错误。
func TestReadFileImageCorruptPNGFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	bad := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00") // 截断的 IHDR
	if err := os.WriteFile(filepath.Join(dir, "bad.png"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newReadFileTool(NewFileTools(dir))
	sink := &tools.ImageSink{}
	if _, err := tool.Call(tools.WithImageSink(context.Background(), sink), "read_file", `{"path":"bad.png"}`); err == nil {
		t.Fatal("截断 PNG 应报解码错误而不是原样内嵌")
	}
	if len(sink.Images()) != 0 {
		t.Fatal("坏图不该进内容块")
	}
}

// TestTileRects 切片几何：边界先按 maxSide 求片数，超 maxTiles 才封顶。
func TestTileRects(t *testing.T) {
	cases := []struct {
		w, h  int
		want  int
		label string
	}{
		{390, 8192, 1, "边界内不切"},
		{390, 8193, 2, "刚超 1px 就切"},
		{390, 9697, 2, "真实事故尺寸"},
		{9000, 390, 2, "横向超限也切"},
		{50, 100000, maxImageTiles, "片数封顶"},
	}
	for _, c := range cases {
		got := tileRects(c.w, c.h, maxImageSidePx, maxImageTiles)
		if len(got) != c.want {
			t.Fatalf("%s: %dx%d → %d tiles, want %d", c.label, c.w, c.h, len(got), c.want)
		}
		// 覆盖完整、无重叠
		if c.h > c.w {
			if got[0].Min.Y != 0 || got[len(got)-1].Max.Y != c.h {
				t.Fatalf("%s: vertical coverage broken: %v", c.label, got)
			}
			for i := 1; i < len(got); i++ {
				if got[i].Min.Y != got[i-1].Max.Y {
					t.Fatalf("%s: gap/overlap between tiles: %v", c.label, got)
				}
			}
			continue
		}
		if got[0].Min.X != 0 || got[len(got)-1].Max.X != c.w {
			t.Fatalf("%s: horizontal coverage broken: %v", c.label, got)
		}
	}
}
