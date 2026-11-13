package builtin

import (
	"os"
	"path/filepath"
	"testing"
)

// 预览产物缓存子树（~/.go-code/previews，R08a）的放行与防护。
//
// 为什么会有一个**新**的放行子树：office 预览面板要把转换产物 HTML 读回前端
// （store.loadOfficePreview → file_preview），而 bridge 的 file_preview 与所有文件工具
// 共用 denyHarnessConfigPath。不加这一档，表现是「转换成功但读不回」——产物明明在、
// 面板却报错，最难排查的一类不一致。
//
// 加了放行就必须同时钉住三件事：
//  1. 普通产物文件可读（否则功能不成立）；
//  2. 软链接逃逸仍被拦（previews 与 memory 同性质：模型可写 → 可造桥）；
//  3. 同一目录下的其他 .go-code 内容（sessions/events/settings.json）仍被保护。
func TestPreviewsSubtreeIsReadableButCannotEscape(t *testing.T) {
	home := setupCfgHome(t)
	cfgDir := filepath.Join(home, ".go-code")
	previews := filepath.Join(cfgDir, "previews", "abcdef")

	// 1) 正常产物可读（这是前端读回的唯一路径）
	htmlPath := filepath.Join(previews, "index.html")
	if err := os.MkdirAll(previews, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(htmlPath, []byte("<html>ok</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DenyHarnessConfigPath(htmlPath); err != nil {
		t.Fatalf("预览产物被拦住了，前端会读不回: %v", err)
	}

	// 2) 软链接逃逸：previews 里指向 settings.json 的链接必须仍被拒
	secret := filepath.Join(cfgDir, "settings.json")
	if err := os.WriteFile(secret, []byte(`{"api_key":"SECRET"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	steal := filepath.Join(previews, "steal.html")
	if err := os.Symlink(secret, steal); err != nil {
		t.Skipf("本机不支持软链接: %v", err)
	}
	if err := DenyHarnessConfigPath(steal); err == nil {
		t.Fatal("经 previews 软链接读到了 settings.json（放行子树成了逃逸桥）")
	}

	// 3) 反向：同目录下别的 .go-code 内容照旧保护（放行只限这一个子树）
	for _, p := range []string{
		filepath.Join(cfgDir, "events", "x.jsonl"),
		filepath.Join(cfgDir, "sessions", "ws", "s.json"),
		filepath.Join(cfgDir, "previews-other", "index.html"),
	} {
		if err := DenyHarnessConfigPath(p); err == nil {
			t.Fatalf("受保护路径被误放行: %s", p)
		}
	}
}
