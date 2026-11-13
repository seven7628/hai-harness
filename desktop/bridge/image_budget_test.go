package main

import (
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// TestImageBudgetExceeded 只累计图片块；文本不参与（由 maxCommandLineBytes 兜底）。
func TestImageBudgetExceeded(t *testing.T) {
	img := func(n int) core.Content {
		return core.Content{Type: core.ContentTypeImage, Content: strings.Repeat("A", n)}
	}
	txt := core.Content{Type: core.ContentTypeText, Content: strings.Repeat("B", 1000)}

	cases := []struct {
		name   string
		blocks []core.Content
		limit  int
		want   bool
		total  int
	}{
		{"no image", []core.Content{txt}, 100, false, 0},
		{"under", []core.Content{txt, img(50)}, 100, false, 50},
		{"exact", []core.Content{img(100)}, 100, false, 100},
		{"over", []core.Content{img(101)}, 100, true, 101},
		{"multi image sums", []core.Content{img(60), img(60)}, 100, true, 120},
		{"text does not count", []core.Content{txt, img(90)}, 100, false, 90},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			over, total := imageBudgetExceeded(c.blocks, c.limit)
			if over != c.want || total != c.total {
				t.Fatalf("got (%v,%d), want (%v,%d)", over, total, c.want, c.total)
			}
		})
	}
}

// TestImageBudgetWithinStdinLineLimit 传输层闸门必须真的留在 stdin 单行上限内：
// 图片总量 ≤ maxImageBlockBytes 时，加文本/转义也不该越过 maxCommandLineBytes。
func TestImageBudgetWithinStdinLineLimit(t *testing.T) {
	if maxImageBlockBytes >= maxCommandLineBytes {
		t.Fatalf("image gate %d must be strictly below stdin line limit %d",
			maxImageBlockBytes, maxCommandLineBytes)
	}
	// 余量至少容纳 1MiB 文本 + JSON 转义与命令封装开销
	if slack := maxCommandLineBytes - maxImageBlockBytes; slack < 1<<20 {
		t.Fatalf("slack = %d bytes, want ≥1MiB for text/escaping overhead", slack)
	}
}

// TestAskBatchRejectsOversizedImage 超限图片 → 明确 code（image_too_large），
// 而不是落到「command line too long」（后者用户完全无法行动）。
func TestAskBatchRejectsOversizedImage(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	sid := newSessionID(t, m, buf, ws)

	big := strings.Repeat("A", maxImageBlockBytes+1)
	m.dispatch(cmd(2, "ask_batch", map[string]any{
		"workspace":  ws,
		"session_id": sid,
		"messages": []any{map[string]any{"content": []any{
			map[string]any{"type": "image", "content": "data:image/png;base64," + big, "mime_type": "image/png"},
		}}},
	}))
	lines := readLines(buf)
	if len(lines) == 0 {
		t.Fatal("no response")
	}
	last := lines[len(lines)-1]
	if last["ok"] != false {
		t.Fatalf("oversized image must fail: %+v", last)
	}
	if last["code"] != "image_too_large" {
		t.Fatalf("want code=image_too_large, got %+v", last["code"])
	}
	errMsg, _ := last["error"].(string)
	if !strings.Contains(errMsg, "message 1") {
		t.Fatalf("error should name the offending message: %q", errMsg)
	}
	if !strings.Contains(errMsg, "3.0MB") {
		t.Fatalf("error should state the limit: %q", errMsg)
	}
}

// TestAskBatchAcceptsImageWithinLimit 达标图片正常入队（不误伤）。
func TestAskBatchAcceptsImageWithinLimit(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	sid := newSessionID(t, m, buf, ws)

	ok := strings.Repeat("A", 1024)
	m.dispatch(cmd(2, "ask_batch", map[string]any{
		"workspace":  ws,
		"session_id": sid,
		"messages": []any{map[string]any{"content": []any{
			map[string]any{"type": "text", "content": "look"},
			map[string]any{"type": "image", "content": "data:image/png;base64," + ok, "mime_type": "image/png"},
		}}},
	}))
	last := readLines(buf)
	if len(last) == 0 || last[len(last)-1]["ok"] != true {
		t.Fatalf("within-limit image must be accepted: %+v", last)
	}
}

// TestAskBatchReportsOffendingMessageIndex 多条消息时错误必须指向越界的那一条
// （用户能直接定位并删除/压缩该条，而不是在整批里猜）。
func TestAskBatchReportsOffendingMessageIndex(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	sid := newSessionID(t, m, buf, ws)

	small := "data:image/png;base64," + strings.Repeat("A", 512)
	big := "data:image/png;base64," + strings.Repeat("A", maxImageBlockBytes+1)
	m.dispatch(cmd(2, "ask_batch", map[string]any{
		"workspace":  ws,
		"session_id": sid,
		"messages": []any{
			map[string]any{"content": []any{map[string]any{"type": "image", "content": small}}},
			map[string]any{"content": []any{map[string]any{"type": "text", "content": "ok text"}}},
			map[string]any{"content": []any{map[string]any{"type": "image", "content": big}}},
		},
	}))
	lines := readLines(buf)
	last := lines[len(lines)-1]
	if last["ok"] != false || last["code"] != "image_too_large" {
		t.Fatalf("want image_too_large failure: %+v", last)
	}
	if errMsg, _ := last["error"].(string); !strings.Contains(errMsg, "message 3") {
		t.Fatalf("error must point at message 3: %q", errMsg)
	}
	if d, ok := last["data"].(map[string]any); !ok || d["message_index"] != float64(2) {
		t.Fatalf("data.message_index should be 0-based 2: %+v", last["data"])
	}
}
