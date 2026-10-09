package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeUserMDFile 写测试指令文件。
//
// 自带而不复用别处的助手：本仓 .gitignore 有 *_test.go，除本文件外 agents 包的
// 测试文件都未纳入版本控制，复用它们的 helper 会让本文件在干净 clone 上编译不过。
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

	// 空/纯空白 AGENTS.md **不得**遮蔽 CLAUDE.md（否则用户在全局层配的偏好静默失效）
	for _, blank := range []string{"", "   \n\t  "} {
		b := t.TempDir()
		writeUserMDFile(t, filepath.Join(b, "AGENTS.md"), blank)
		writeUserMDFile(t, filepath.Join(b, "CLAUDE.md"), "IMPORTANT PREF")
		got := discoverUserAgentMD(b)
		if len(got) != 1 || got[0].Name != "CLAUDE.md" || !strings.Contains(got[0].Content, "PREF") {
			t.Fatalf("空白 AGENTS.md 遮蔽了有效 CLAUDE.md（blank=%q）：%+v", blank, got)
		}
		// 端到端：偏好必须真的进了 system
		sp := NewAgentLoop(WithUserAgentMDDir(b)).composeSystemPrompt()
		if !strings.Contains(sp, "IMPORTANT PREF") {
			t.Fatalf("空白 AGENTS.md 导致用户偏好未注入（blank=%q）", blank)
		}
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

// 注入 system 的路径必须是**工具能直接用的绝对路径**。
//
// 这一条曾是真实缺陷：早先渲染成 "~/.agents/AGENTS.md"，而文件工具的 resolve
// 不展开 "~"，write_file 会把它锚到工作区、静默建出一个字面量 "~" 目录并报成功
// （见 tools/builtin 的 resolve + 本仓 write_file 复现）。故此处钉住"绝对 + 非 ~ 前缀"。
func TestUserAgentMDRelPathIsActionableAbs(t *testing.T) {
	dir := t.TempDir()
	writeUserMDFile(t, filepath.Join(dir, "AGENTS.md"), "PREF")
	files := discoverUserAgentMD(dir)
	if len(files) != 1 {
		t.Fatalf("应发现 1 个文件，实际 %d", len(files))
	}
	rel := files[0].RelPath
	if !filepath.IsAbs(rel) {
		t.Fatalf("RelPath 必须是绝对路径（模型要照着它操作），实际 %q", rel)
	}
	if strings.HasPrefix(rel, "~") {
		t.Fatalf("RelPath 不得以 ~ 开头：文件工具不展开 ~，会锚到工作区（实际 %q）", rel)
	}
	// 渲染出的路径必须能被 write_file 原样解析（工具层不锚定 workspace 的分支）
	if got := filepath.Clean(rel); got != rel {
		t.Fatalf("RelPath 应是 Clean 过的：%q vs %q", rel, got)
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

// 用户把 ~/.agents 本身当工作区打开（dotfiles / skills 仓库很常见）时，同一个文件
// 不得被注入两份。
//
// 这条不是洁癖：用户级渲染绝对路径、工作区层渲染相对路径，两块路径标注不同但
// 内容逐字相同，模型会看到"两份互相矛盾（其实相同）的规则"。
func TestUserAgentMDDedupWhenWorkspaceIsUserDir(t *testing.T) {
	userDir := t.TempDir()
	writeUserMDFile(t, filepath.Join(userDir, "AGENTS.md"), "PREF")
	sp := NewAgentLoop(WithUserAgentMDDir(userDir), WithAgentMDDir(userDir)).composeSystemPrompt()
	if n := strings.Count(sp, "PREF"); n != 1 {
		t.Fatalf("同一物理文件被注入 %d 次，应为 1 次:\n%s", n, sp)
	}
	// 去重不得误伤**不同**的文件：子目录那份必须仍在
	writeUserMDFile(t, filepath.Join(userDir, "sub", "AGENTS.md"), "SUBPREF")
	sp = NewAgentLoop(WithUserAgentMDDir(userDir), WithAgentMDDir(userDir)).composeSystemPrompt()
	if !strings.Contains(sp, "SUBPREF") {
		t.Fatalf("去重误伤了工作区里另一个文件:\n%s", sp)
	}
	if strings.Count(sp, "PREF")-strings.Count(sp, "SUBPREF") != 1 {
		t.Fatalf("根文件应仍只一份:\n%s", sp)
	}
}

// 超大体量的用户级文件：不得整份注入（全局文件一次手滑 = 每个工作区每轮都付费），
// 也**不得静默截断**（模型会按"规则只到一半"行事），必须明确告知。
func TestUserAgentMDSizeCap(t *testing.T) {
	dir := t.TempDir()
	writeUserMDFile(t, filepath.Join(dir, "AGENTS.md"), strings.Repeat("X", userAgentMDMaxBytes+1))
	got := discoverUserAgentMD(dir)
	if len(got) != 1 {
		t.Fatalf("应产出 1 条说明，实际 %d", len(got))
	}
	if strings.Contains(got[0].Content, "XXX") {
		t.Fatal("超限文件被注入了（不得静默截断或整份注入）")
	}
	if !strings.Contains(got[0].Content, "上限") {
		t.Fatalf("超限应明确告知而非静默：%q", got[0].Content)
	}
	// 恰好在上限内 → 正常注入
	dir2 := t.TempDir()
	writeUserMDFile(t, filepath.Join(dir2, "AGENTS.md"), strings.Repeat("X", userAgentMDMaxBytes))
	if got := discoverUserAgentMD(dir2); len(got) != 1 || !strings.Contains(got[0].Content, "XXX") {
		t.Fatal("上限内应正常注入")
	}
}
