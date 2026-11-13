package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 预览产物缓存子树（~/.go-code/previews，R08a）的放行口径：**读开、写关**。
//
// 为什么会有一个新的放行子树：office 预览面板要把转换产物 HTML 读回前端
// （store.loadOfficePreview → file_preview），而 file_preview 与所有文件工具共用
// denyHarnessConfigPath。不加这一档，表现是「转换成功但读不回」。
//
// 为什么还必须**只读**：缓存条目 = 「这份文档的预览」。能写 = 模型可以把 index.html
// 换成任意 HTML，用户看到的是**看起来属于这份文档**的伪造内容。缓存键是内容哈希、
// 模型算得出来 —— 所以「难猜」不是防线，只读才是。
//
// 本文件钉四件事：读得动（功能成立）、写被拒（安全）、软链接与硬链接都逃不出去
// （同 memory 子树的两条链）、同目录其它 .go-code 内容照旧保护。
func previewsFixture(t *testing.T) (cfgDir, entry, htmlPath string) {
	t.Helper()
	home := setupCfgHome(t)
	cfgDir = filepath.Join(home, ".go-code")
	entry = filepath.Join(cfgDir, "previews", "abcdef")
	htmlPath = filepath.Join(entry, "index.html")
	if err := os.MkdirAll(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(htmlPath, []byte("<html>ok</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgDir, entry, htmlPath
}

// 读放行：file_preview（以及 read_file）必须读得动产物 —— 否则功能不成立。
func TestPreviewsEntryIsReadable(t *testing.T) {
	_, _, htmlPath := previewsFixture(t)
	if err := DenyHarnessConfigPath(htmlPath); err != nil {
		t.Fatalf("预览产物被拦住了，前端会读不回: %v", err)
	}
	ft := NewFileTools(t.TempDir())
	out, err := toolByName(t, ft, "read_file").Call(context.Background(), "read_file",
		`{"path":`+jsonStr(htmlPath)+`}`)
	if err != nil {
		t.Fatalf("read_file 读不回预览产物: %v", err)
	}
	if !strings.Contains(out, "<html>ok</html>") {
		t.Fatalf("读回的内容不对: %.80s", out)
	}
}

// 写拒绝：write_file / edit_file 都不能改缓存条目（改了就是伪造预览）。
func TestPreviewsEntryIsNotWritable(t *testing.T) {
	_, _, htmlPath := previewsFixture(t)
	if err := DenyHarnessConfigPathWrite(htmlPath); err == nil {
		t.Fatal("写门面放行了 previews（缓存可被改写）")
	}
	ft := NewFileTools(t.TempDir())
	if _, err := toolByName(t, ft, "write_file").Call(context.Background(), "write_file",
		`{"path":`+jsonStr(htmlPath)+`,"content":"PWNED"}`); err == nil {
		t.Fatal("write_file 改写了预览缓存")
	}
	if _, err := toolByName(t, ft, "edit_file").Call(context.Background(), "edit_file",
		`{"path":`+jsonStr(htmlPath)+`,"old_string":"ok","new_string":"pwned"}`); err == nil {
		t.Fatal("edit_file 改写了预览缓存")
	}
	b, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "<html>ok</html>" {
		t.Fatalf("缓存内容被改写: %q", b)
	}
}

// 软链接逃逸：previews 里指向 settings.json 的链接必须仍被拒（读与写两条都拒）。
func TestPreviewsSymlinkCannotEscape(t *testing.T) {
	cfgDir, entry, _ := previewsFixture(t)
	secret := filepath.Join(cfgDir, "settings.json")
	if err := os.WriteFile(secret, []byte(`{"api_key":"SECRET"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	steal := filepath.Join(entry, "steal.html")
	if err := os.Symlink(secret, steal); err != nil {
		t.Skipf("本机不支持软链接: %v", err)
	}
	if err := DenyHarnessConfigPath(steal); err == nil {
		t.Fatal("经 previews 软链接读到了 settings.json（放行子树成了逃逸桥）")
	}
}

// 硬链接逃逸：`ln`（不带 -s）与 `ln -s` 一样常用，而 EvalSymlinks 对硬链完全无感
// —— 它自己就是真实名字。唯一的判据是 inode 比对（isHardlinkToProtected）。
//
// 为什么这条必须**在 previews 里单独钉一遍**（评审判定）：previews 是新开的放行子树，
// 它的硬链防护在此之前只能靠「memory 那边有测试」间接传递 —— 那是别人的断言，
// 不是这棵子树的断言。硬链可写意味着改的就是 settings.json 本身。
func TestPreviewsHardlinkCannotEscapeToConfig(t *testing.T) {
	cfgDir, entry, _ := previewsFixture(t)
	secret := filepath.Join(cfgDir, "settings.json")
	if err := os.WriteFile(secret, []byte(`{"api_key":"SECRET-VALUE-123"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(entry, "note.html")
	if err := os.Link(secret, link); err != nil {
		t.Skipf("本机不支持创建硬链接: %v", err)
	}
	ft := NewFileTools(t.TempDir())
	// 读侧
	out, err := toolByName(t, ft, "read_file").Call(context.Background(), "read_file", `{"path":`+jsonStr(link)+`}`)
	if err == nil {
		t.Fatalf("❌ 硬链接逃逸未被拦截，读到了: %.120s", out)
	}
	if strings.Contains(out, "SECRET-VALUE-123") {
		t.Fatalf("❌ 密钥泄漏: %s", out)
	}
	// 写侧（经硬链写 = 改 settings.json 本身）：两条独立断言 ——
	// 「写被拒」与「源文件没被改动」是两件事，后者的内容检查不能藏在 if 里。
	if _, err := toolByName(t, ft, "write_file").Call(context.Background(), "write_file",
		`{"path":`+jsonStr(link)+`,"content":"PWNED"}`); err == nil {
		t.Fatal("write_file 放行了硬链")
	}
	if b, err := os.ReadFile(secret); err != nil || string(b) != `{"api_key":"SECRET-VALUE-123"}` {
		t.Fatalf("❌ settings.json 被经硬链改写: %q err=%v", b, err)
	}
}

// 反向：同目录下别的 .go-code 内容照旧保护（放行只限这一个子树）。
func TestOtherConfigPathsStayProtected(t *testing.T) {
	cfgDir, _, _ := previewsFixture(t)
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
