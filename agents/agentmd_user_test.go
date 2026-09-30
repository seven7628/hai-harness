package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeUserMDFile 写测试指令文件（自带 helper：agents 包的 writeTestFile 在
// agentmd_test.go 里，而 .gitignore 的 *_test.go 规则使那个文件不在版本控制内 ——
// 本文件必须自包含，不能依赖仓库外的测试助手）。
func writeUserMDFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 用户级层：~/.agents/AGENTS.md 的发现规则。
//
// 这一组钉的是**形状差异**（单文件 vs 工作区递归）—— 用户级目录同时是 skills /
// subagents 的家，递归会把子目录里各技能自带的 AGENTS.md 误当个人全局指令。
func TestDiscoverUserAgentMD(t *testing.T) {
	dir := t.TempDir()
	writeUserMDFile(t, filepath.Join(dir, "AGENTS.md"), "个人全局：一律用中文回答")
	files := discoverUserAgentMD(dir)
	if len(files) != 1 {
		t.Fatalf("应只发现 1 个用户级文件，实际 %d 个：%+v", len(files), files)
	}
	if files[0].Name != "AGENTS.md" || files[0].Content != "个人全局：一律用中文回答" {
		t.Fatalf("内容/文件名不符：%+v", files[0])
	}

	// 同目录并存 CLAUDE.md → AGENTS.md 优先（与工作区同级共存规则同向）
	writeUserMDFile(t, filepath.Join(dir, "CLAUDE.md"), "个人全局：claude 变体")
	files = discoverUserAgentMD(dir)
	if len(files) != 1 || files[0].Name != "AGENTS.md" {
		t.Fatalf("AGENTS.md 未优先于 CLAUDE.md：%+v", files)
	}

	// 非递归：子目录里的 AGENTS.md 一律不认
	sub := filepath.Join(dir, "skills", "pdf")
	writeUserMDFile(t, filepath.Join(sub, "AGENTS.md"), "某个 skill 自带的说明")
	if got := discoverUserAgentMD(dir); len(got) != 1 || got[0].Name != "AGENTS.md" {
		t.Fatalf("用户级发现不应递归子目录：%+v", got)
	}
}

// 用户级层缺文件时的降级：只有 CLAUDE.md 仍能兜底；目录/断链/特殊文件不算指令文件。
func TestDiscoverUserAgentMDFallback(t *testing.T) {
	dir := t.TempDir()
	if got := discoverUserAgentMD(dir); got != nil {
		t.Fatalf("空目录应返回 nil，实际 %+v", got)
	}
	if got := discoverUserAgentMD(""); got != nil {
		t.Fatalf("空路径应返回 nil，实际 %+v", got)
	}
	if got := discoverUserAgentMD(filepath.Join(dir, "nope")); got != nil {
		t.Fatalf("不存在的目录应返回 nil，实际 %+v", got)
	}

	// 目录名恰好叫 AGENTS.md → 不是指令文件，跳过后仍应兜底到 CLAUDE.md
	if err := os.MkdirAll(filepath.Join(dir, "bait", "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeUserMDFile(t, filepath.Join(dir, "bait", "CLAUDE.md"), "只有 claude")
	got := discoverUserAgentMD(filepath.Join(dir, "bait"))
	if len(got) != 1 || got[0].Name != "CLAUDE.md" {
		t.Fatalf("目录形态的 AGENTS.md 应被跳过并兜底 CLAUDE.md：%+v", got)
	}

	// 符号链接跟随（~/.agents/AGENTS.md → dotfiles 仓库是常见形态）
	real := filepath.Join(t.TempDir(), "real-AGENTS.md")
	writeUserMDFile(t, real, "经链接的个人全局")
	linkDir := t.TempDir()
	if err := os.Symlink(real, filepath.Join(linkDir, "AGENTS.md")); err != nil {
		t.Skipf("本机不支持符号链接: %v", err)
	}
	if got := discoverUserAgentMD(linkDir); len(got) != 1 || got[0].Content != "经链接的个人全局" {
		t.Fatalf("未跟随符号链接：%+v", got)
	}
}

// 路径展示：home 前缀缩成 ~（注入 system 的是个人 home 路径，不外泄用户名）。
func TestDisplayAgentMDPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("无 home 目录")
	}
	if got, want := displayAgentMDPath(filepath.Join(home, ".agents", "AGENTS.md")), filepath.Join("~", ".agents", "AGENTS.md"); got != want {
		t.Fatalf("home 子树应缩成 ~：got %q, want %q", got, want)
	}
	outside := filepath.Join(t.TempDir(), "AGENTS.md")
	if got := displayAgentMDPath(outside); got != outside {
		t.Fatalf("非 home 子树应原样返回：got %q, want %q", got, outside)
	}
	// home 本身（rel == "."）也缩成 "~"，不缩成 "./x"
	if got := displayAgentMDPath(filepath.Join(home, "AGENTS.md")); got != filepath.Join("~", "AGENTS.md") {
		t.Fatalf("home 根下的文件：got %q", got)
	}
}

// 分层顺序：用户级在最前（优先级最低），其后显式文件、最后工作区递归。
//
// 刻意钉住「**拼接而非覆盖**」：用户级装个人全局偏好、工作区装项目约定，
// 工作区根文件不得把个人偏好顶掉（否则用户配了「一律用中文回答」，
// 进了带 AGENTS.md 的仓库就静默失效）。对齐 Codex / Claude Code 的
// global-then-project 拼接语义。
func TestUserAgentMDLayeredBeforeWorkspace(t *testing.T) {
	userDir := t.TempDir()
	ws := t.TempDir()
	writeUserMDFile(t, filepath.Join(userDir, "AGENTS.md"), "USER_PREF")
	writeUserMDFile(t, filepath.Join(ws, "AGENTS.md"), "WS_ROOT")
	writeUserMDFile(t, filepath.Join(ws, "sub", "AGENTS.md"), "WS_SUB")
	explicit := filepath.Join(t.TempDir(), "EXPLICIT.md")
	writeUserMDFile(t, explicit, "EXPLICIT")

	sp := NewAgentLoop(
		WithUserAgentMDDir(userDir),
		WithAgentMDFiles(explicit),
		WithAgentMDDir(ws),
	).composeSystemPrompt()

	order := []struct {
		marker string
		want   int
	}{
		{"USER_PREF", strings.Index(sp, "USER_PREF")},
		{"EXPLICIT", strings.Index(sp, "EXPLICIT")},
		{"WS_ROOT", strings.Index(sp, "WS_ROOT")},
		{"WS_SUB", strings.Index(sp, "WS_SUB")},
	}
	prev := -1
	for _, o := range order {
		if o.want < 0 {
			t.Fatalf("%s 未注入 system", o.marker)
		}
		if o.want <= prev {
			t.Fatalf("%s 位置 %d 未严格晚于前一项（%d）", o.marker, o.want, prev)
		}
		prev = o.want
	}
	// 用户级与工作区根**同名共存**（两份都在），这正是「不覆盖」的判据
	if strings.Count(sp, "# AGENTS.md") != 3 { // 用户级 + ws 根 + ws/sub
		t.Fatalf("同名文件应各自成块而非互相覆盖，实际块数 %d", strings.Count(sp, "# AGENTS.md"))
	}
}

// 不传 WithUserAgentMDDir 时**逐字节**回到本选项存在之前的行为（不得凭空多一层）。
func TestUserAgentMDAbsentIsNoop(t *testing.T) {
	ws := t.TempDir()
	writeUserMDFile(t, filepath.Join(ws, "AGENTS.md"), "WS_ROOT")
	base := NewAgentLoop(WithAgentMDDir(ws)).composeSystemPrompt()
	empty := NewAgentLoop(WithAgentMDDir(ws), WithUserAgentMDDir("")).composeSystemPrompt()
	if base != empty {
		t.Fatal("未注入用户级目录时不应改变 system")
	}
	// 目录存在但无指令文件 → 同样无变化（不得注入空块或占位文本）
	userDir := t.TempDir()
	if got := NewAgentLoop(WithAgentMDDir(ws), WithUserAgentMDDir(userDir)).composeSystemPrompt(); got != base {
		t.Fatalf("用户级目录无指令文件时不应改变 system：\n%s", got)
	}
}
