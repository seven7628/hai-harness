package provider

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// mkPNGDataURL 生成 w×h 的 PNG data URL（纯色 → 体积极小，几 MB 的边也压得动）。
func mkPNGDataURL(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 64, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func imageCount(msgs []core.Message) int {
	n := 0
	for _, m := range msgs {
		for _, c := range m.Content {
			if c.Type == core.ContentTypeImage {
				n++
			}
		}
	}
	return n
}

// TestSanitizeOversizeImages 单边超 core.MaxImageSidePx 的图片降级为文本说明：
// 边界与实测一致（390x8192 通过、390x8193 拒收），且必须覆盖 tool 消息（read_file
// 的图就在 tool 回执里）。
func TestSanitizeOversizeImages(t *testing.T) {
	okURL := mkPNGDataURL(t, 390, 200)
	hugeURL := mkPNGDataURL(t, 390, 8193)
	toolMsg := core.NewToolMessageWithImages("c1", "[read_file: tall.png]", []core.Content{
		{Type: core.ContentTypeImage, Content: hugeURL, MimeType: "image/png"},
	})
	msgs := []core.Message{
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "看这两张"}),
		core.NewUserMessage(core.Content{Type: core.ContentTypeImage, Content: okURL, MimeType: "image/png"}),
		toolMsg,
	}
	out := SanitizeOversizeImages(msgs)
	if got := imageCount(out); got != 1 {
		t.Fatalf("want 1 remaining image (the 390x200 one), got %d", got)
	}
	// tool 消息里那张被换成文本，且文本说清尺寸与原因（模型能据此重新取图）
	var note string
	for _, c := range out[2].Content {
		if c.Type == core.ContentTypeText && strings.Contains(c.Content, "image omitted") {
			note = c.Content
		}
	}
	if !strings.Contains(note, "390x8193") || !strings.Contains(note, fmt.Sprint(core.MaxImageSidePx)) {
		t.Fatalf("placeholder should state the real size and limit: %q", note)
	}
	if !strings.Contains(note, "Re-capture") {
		t.Fatalf("placeholder should tell the model what to do next: %q", note)
	}
	// 未超限的图原样保留
	if out[1].Content[0].Content != okURL {
		t.Fatal("合规图片不该被动")
	}
}

// TestSanitizeOversizeImagesNoop 无超限图时原样返回（不分配 → 缓存前缀稳定）。
func TestSanitizeOversizeImagesNoop(t *testing.T) {
	msgs := []core.Message{
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "hi"}),
		core.NewUserMessage(core.Content{Type: core.ContentTypeImage, Content: mkPNGDataURL(t, 10, 10)}),
		{ // 远程 URL：看不到尺寸，不能凭空判死（留给上游取图）
			Role:    core.User,
			Content: []core.Content{{Type: core.ContentTypeImage, Content: "https://example.com/a.png"}},
		},
		{ // 损坏的 data URL：不是本函数的判据，原样放行
			Role:    core.User,
			Content: []core.Content{{Type: core.ContentTypeImage, Content: "data:image/png;base64,AAAA"}},
		},
	}
	out := SanitizeOversizeImages(msgs)
	if &out[0] != &msgs[0] {
		t.Fatal("合规上下文不该重新分配")
	}
}

// TestRepairMessagesCombinesBoth 两道修复在同一次请求组装里都生效。
func TestRepairMessagesCombinesBoth(t *testing.T) {
	msgs := []core.Message{
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "go"}),
		core.NewAssistantMessage([]core.Content{{Type: core.ContentTypeText, Content: ""}}, "",
			[]core.ToolCall{{Id: "c1", Name: "read_file"}, {Id: "c2", Name: "read_file"}}),
		core.NewToolMessageWithImages("c1", "r1", []core.Content{
			{Type: core.ContentTypeImage, Content: mkPNGDataURL(t, 390, 8193)},
		}),
		core.NewAssistantMessage([]core.Content{{Type: core.ContentTypeText, Content: "intruder"}}, "", nil),
		core.NewToolMessage("c2", "r2"),
	}
	out := RepairMessages(msgs)
	if imageCount(out) != 0 {
		t.Fatal("超限图片应已降级")
	}
	if !core.ToolBatchesIntact(out) {
		t.Fatalf("工具批次应已修复: %d messages", len(out))
	}
}

// TestImageDataURLSizeHead 尺寸只读头部：超长 data URL 也能在截断前缀下读出尺寸
// （不把整图搬进内存），非 data URL / 非 base64 一律 ok=false。
func TestImageDataURLSizeHead(t *testing.T) {
	u := mkPNGDataURL(t, 640, 480)
	w, h, ok := imageDataURLSize(u)
	if !ok || w != 640 || h != 480 {
		t.Fatalf("got %dx%d ok=%v", w, h, ok)
	}
	if _, _, ok := imageDataURLSize("https://example.com/a.png"); ok {
		t.Fatal("远程 URL 不该判出尺寸")
	}
	if _, _, ok := imageDataURLSize("data:image/png,%89PNG"); ok {
		t.Fatal("非 base64 data URL 不该判出尺寸")
	}
}
