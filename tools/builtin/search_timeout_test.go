package builtin

// 搜索工具的可中断性与工具级时限（search_timeout.go）的测试。
//
// 改造前 grep/glob 的 Call 丢弃 ctx —— 引擎的 5 分钟工具超时对它们无效，
// 用户中断传不进来，会话 abort 后 goroutine 还在后台把工作区读完。
// 这里钉住「现在会响应取消」这条契约。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/tools"
)

// TestSearchToolsDeclareTimeout 两个工具都声明工具级时限。
func TestSearchToolsDeclareTimeout(t *testing.T) {
	ft := NewFileTools(t.TempDir())
	for _, tool := range []tools.Tool{newGrepTool(ft), newGlobTool(ft)} {
		tp, ok := tool.(tools.ToolTimeoutProvider)
		if !ok {
			t.Fatalf("%s 未实现 ToolTimeoutProvider", tool.Name())
		}
		d := tp.ToolTimeout()
		if d <= 0 || d > 5*time.Minute {
			t.Errorf("%s 的工具级时限不合理: %v", tool.Name(), d)
		}
	}
}

// TestSearchToolsAlreadyCancelled 调用前就取消 ⇒ 立刻返回错误，不读任何文件。
func TestSearchToolsAlreadyCancelled(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name string
		tool tools.Tool
		args string
	}{
		{"grep", newGrepTool(ft), `{"pattern":"needle"}`},
		{"glob", newGlobTool(ft), `{"pattern":"**/*.go"}`},
	} {
		start := time.Now()
		_, err := tc.tool.Call(ctx, "", tc.args)
		if err == nil {
			t.Errorf("%s: 已取消的 ctx 应当报错", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "search_cancelled") {
			t.Errorf("%s: 错误码不对: %v", tc.name, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: 取消后返回太慢: %v", tc.name, d)
		}
	}
}

// TestSearchToolsTimeout 过了 deadline ⇒ 报 search_timeout（并给出收窄建议）。
func TestSearchToolsTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(dir)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	for _, tc := range []struct {
		name string
		tool tools.Tool
		args string
	}{
		{"grep", newGrepTool(ft), `{"pattern":"needle"}`},
		{"glob", newGlobTool(ft), `{"pattern":"**/*.go"}`},
	} {
		_, err := tc.tool.Call(ctx, "", tc.args)
		if err == nil {
			t.Errorf("%s: 过了 deadline 应当报错", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "search_timeout") {
			t.Errorf("%s: 错误码不对: %v", tc.name, err)
		}
		if !strings.Contains(err.Error(), "narrow") {
			t.Errorf("%s: 超时错误应给出「收窄搜索」的下一步动作: %v", tc.name, err)
		}
	}
}

// TestSearchToolsCancelDuringScan 扫描**进行中**取消 ⇒ 及时返回（不是等扫完）。
func TestSearchToolsCancelDuringScan(t *testing.T) {
	dir := t.TempDir()
	// 造一个够大的树，让扫描持续足够久
	body := strings.Repeat("some line of text here\n", 5000)
	for i := 0; i < 300; i++ {
		p := filepath.Join(dir, "d", string(rune('a'+i%26)), string(rune('a'+i/26)), "f.txt")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ft := NewFileTools(dir)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// 10ms：这棵树（37.5 MB）在并行扫描下约 84ms ⇒ 取消稳稳落在扫描**进行中**。
		// （改造前是单线程、要几秒，所以是 30ms；并行化后余量必须收窄，
		//  否则扫描可能在取消生效前就跑完了 ⇒ 测试偶发失败。）
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := newGrepTool(ft).Call(ctx, "", `{"pattern":"zzz_nomatch"}`)
	d := time.Since(start)
	if err == nil {
		t.Fatalf("扫描中被取消应当报错")
	}
	if !strings.Contains(err.Error(), "search_cancelled") {
		t.Errorf("错误码不对: %v", err)
	}
	// 扫完整个树要远大于此（300 个 200KB 文件）；取消后必须很快返回
	if d > 3*time.Second {
		t.Errorf("取消后返回太慢: %v", d)
	}
}
