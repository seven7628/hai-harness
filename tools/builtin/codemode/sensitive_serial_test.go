package codemode

// sensitive_test.go：宿主读盘路径策略（F1）的单测 —— 不需要 node，也不依赖 bridge。
// 门的单测在 serial_test.go。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDenyHostReadHarnessConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	allowed := []string{
		filepath.Join(home, ".go-code", "plugins", "chrome", "pic.png"),
		filepath.Join(home, ".go-code", "runtime", "python", "pic.png"),
		filepath.Join(home, "Pictures", "pic.png"),
		filepath.Join(home, ".go-codeevil", "pic.png"), // 前缀陷阱：不是受保护目录
		"/tmp/pic.png",
		filepath.Join(home, "notes", ".go-code"),
	}
	for _, p := range allowed {
		if reason := denyHostRead(p); reason != "" {
			t.Errorf("应当放行 %q，却被拒：%s", p, reason)
		}
	}
	denied := []string{
		filepath.Join(home, ".go-code", "settings-screenshot.png"),
		filepath.Join(home, ".go-code", "keys.json"),
		filepath.Join(home, ".go-code"),                        // 目录本身也拒（不猜它是不是目录）
		filepath.Join(home, ".go-code", "runtime2", "pic.png"), // 放行子树的前缀陷阱
	}
	for _, p := range denied {
		reason := denyHostRead(p)
		if reason == "" {
			t.Errorf("必须拒绝 %q（沙箱与文件工具都拒绝该目录）", p)
			continue
		}
		if !strings.Contains(reason, "harness config directory") {
			t.Errorf("拒绝原因要能让人看懂，实际：%s", reason)
		}
		// 文案不许泄漏「文件多大」（F1 的第二半：存在性/尺寸 oracle）。
		if strings.Contains(reason, "bytes") || strings.Contains(reason, "size") {
			t.Errorf("拒绝原因里不该带尺寸信息（会变成 oracle）：%s", reason)
		}
	}
}

// 防止「路径策略只写在文档里」：确认真实文件被拒（不是纯字符串比对过关）。
func TestDenyHostReadOnRealFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "screenshot.png")
	if err := os.WriteFile(target, []byte("\x89PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	if reason := denyHostRead(target); reason == "" {
		t.Fatal("真实存在的受保护文件必须被拒")
	}
}
