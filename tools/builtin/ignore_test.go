package builtin

// 忽略规则（ignore.go）的语义与接线测试。
//
// 覆盖三层：
//  ① gitignore 语义解析（反选 / 锚定 / 只匹配目录 / basename / ** / 后匹配者胜）
//  ② 匹配器行为（内置黑名单 / 逐目录增量读 / 子树剪枝 / 显式路径豁免）
//  ③ grep / glob 两个工具的接线（默认跳过 + no_ignore 逃生口）

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// ---------- ① 语义解析 ----------

func TestParseIgnorePattern(t *testing.T) {
	cases := []struct {
		line    string
		negate  bool
		dirOnly bool
		segs    []string
		ok      bool
	}{
		{line: "", ok: false},
		{line: "   ", ok: false},
		{line: "# comment", ok: false},
		{line: "node_modules", ok: true, segs: []string{"**", "node_modules"}},
		{line: "/build", ok: true, segs: []string{"build"}},
		{line: "build/", ok: true, dirOnly: true, segs: []string{"**", "build"}},
		{line: "!keep.txt", ok: true, negate: true, segs: []string{"**", "keep.txt"}},
		{line: "src/*.go", ok: true, segs: []string{"src", "*.go"}},
		{line: "**/gen/*.pb.go", ok: true, segs: []string{"**", "gen", "*.pb.go"}},
		{line: "a/b/", ok: true, dirOnly: true, segs: []string{"a", "b"}},
	}
	for _, c := range cases {
		r, ok := parseIgnorePattern(c.line, "")
		if ok != c.ok {
			t.Fatalf("parseIgnorePattern(%q) ok=%v want %v", c.line, ok, c.ok)
		}
		if !ok {
			continue
		}
		if r.negate != c.negate || r.dirOnly != c.dirOnly {
			t.Errorf("parseIgnorePattern(%q) negate=%v dirOnly=%v want %v/%v",
				c.line, r.negate, r.dirOnly, c.negate, c.dirOnly)
		}
		if strings.Join(r.segs, "/") != strings.Join(c.segs, "/") {
			t.Errorf("parseIgnorePattern(%q) segs=%v want %v", c.line, r.segs, c.segs)
		}
	}
}

// TestMatchIgnoreSegs 路径段匹配（含 ** 的零段/多段）。
func TestMatchIgnoreSegs(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"**/node_modules", "node_modules", true},
		{"**/node_modules", "a/b/node_modules", true},
		{"**/node_modules", "a/node_modules_x", false},
		{"build", "build", true},
		{"build", "a/build", false}, // 锚定（无 ** 前缀）
		{"**/build", "a/build", true},
		{"src/*.go", "src/a.go", true},
		{"src/*.go", "src/x/a.go", false},
		{"**/gen/*.pb.go", "x/gen/y.pb.go", true},
		{"**", "anything/at/all", true},
	}
	for _, c := range cases {
		got := matchIgnoreSegs(strings.Split(c.pat, "/"), strings.Split(c.path, "/"))
		if got != c.want {
			t.Errorf("matchIgnoreSegs(%q, %q) = %v want %v", c.pat, c.path, got, c.want)
		}
	}
}

// TestMatchIgnoreSegsNoExponential 多个 ** 不得指数退化（.gitignore 来自仓库内容）。
func TestMatchIgnoreSegsNoExponential(t *testing.T) {
	pat := strings.Split("**/**/**/x", "/")
	segs := strings.Split("a/b/c/d/e/f/g/h", "/")
	done := make(chan bool, 1)
	go func() { done <- matchIgnoreSegs(pat, segs) }()
	select {
	case got := <-done:
		if got {
			t.Fatalf("不该匹配")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("matchIgnoreSegs 疑似指数退化（>2s 未返回）")
	}
}

// ---------- ② 匹配器行为 ----------

func TestIgnoreMatcher(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(".gitignore")
	mk("node_modules/pkg/index.js")
	mk("src/main.go")
	mk("build/out.js")
	mk("logs/debug.log")
	mk("keep/important.txt")
	mk("sub/.gitignore")
	mk("sub/skipme/x.txt")
	mk("sub/keep/y.txt")

	// 根 .gitignore：忽略 logs/ 与 *.log
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("logs/\n*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 子目录 .gitignore：忽略 skipme/
	if err := os.WriteFile(filepath.Join(dir, "sub", ".gitignore"), []byte("skipme/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newIgnoreMatcher(dir, true)
	cases := []struct {
		rel    string
		isDir  bool
		ignore bool
	}{
		{"node_modules", true, true}, // 内置黑名单
		{"src", false, false},
		{"build", true, true},           // 内置黑名单
		{"logs", true, true},            // 根 .gitignore：目录
		{"logs/debug.log", false, true}, // 根 .gitignore：logs/ 规则连带（目录被剪）
		{"x.log", false, true},          // 根 .gitignore：*.log
		{"keep/important.txt", false, false},
		{"sub/skipme", true, true}, // 子目录 .gitignore
		{"sub/keep", false, false},
		{"sub/keep/y.txt", false, false},
		// 逐路径谓词不查祖先：被剪目录的子路径返回 false 是正确的 ——
		// 遍历时父目录已 SkipDir，子路径根本不会被访问。
		{"node_modules/pkg", true, false},
		{"sub/skipme/x.txt", false, false},
	}
	for _, c := range cases {
		abs := filepath.Join(dir, filepath.FromSlash(c.rel))
		got := m.ignoredDir(abs)
		if !c.isDir {
			got = m.ignoredFile(abs)
		}
		if got != c.ignore {
			t.Errorf("ignored(%q, isDir=%v) = %v want %v", c.rel, c.isDir, got, c.ignore)
		}
	}

	// enabled=false（no_ignore 模式）⇒ 什么都不忽略
	m2 := newIgnoreMatcher(dir, false)
	if m2.ignoredDir(filepath.Join(dir, "node_modules")) {
		t.Errorf("no_ignore 模式不该忽略 node_modules")
	}
	if m2.ignoredFile(filepath.Join(dir, "logs", "debug.log")) {
		t.Errorf("no_ignore 模式不该忽略 .gitignore 命中的文件")
	}
}

// TestIgnoreMatcherNegation 「后匹配者胜」+ 反选。
func TestIgnoreMatcherNegation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n!keep.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newIgnoreMatcher(dir, true)
	if !m.ignoredFile(filepath.Join(dir, "a.log")) {
		t.Errorf("a.log 应被忽略")
	}
	if m.ignoredFile(filepath.Join(dir, "keep.log")) {
		t.Errorf("keep.log 被 ! 反选，不该被忽略")
	}
}

// ---------- ③ 工具接线 ----------

func TestGrepRespectsIgnore(t *testing.T) {
	eng, f := engineWithFileTools(t)
	mk := func(rel string) {
		p := filepath.Join(f.root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("needle here\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("src/main.go")
	mk("node_modules/pkg/index.js")
	mk("logs/debug.log")
	if err := os.WriteFile(filepath.Join(f.root, ".gitignore"), []byte("logs/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := seq(t, eng, core.ToolCall{Id: "1", Name: "grep", Arguments: `{"pattern":"needle"}`})
	if r[0].IsError {
		t.Fatalf("grep failed: %+v", r[0])
	}
	if !strings.Contains(r[0].Result, "src/main.go") {
		t.Fatalf("应命中 src/main.go: %v", r[0].Result)
	}
	if strings.Contains(r[0].Result, "node_modules") {
		t.Fatalf("node_modules 应被跳过: %v", r[0].Result)
	}
	if strings.Contains(r[0].Result, "logs/") {
		t.Fatalf(".gitignore 命中的 logs/ 应被跳过: %v", r[0].Result)
	}

	// no_ignore=true ⇒ 都能搜到
	r = seq(t, eng, core.ToolCall{Id: "2", Name: "grep", Arguments: `{"pattern":"needle","no_ignore":true}`})
	if r[0].IsError {
		t.Fatalf("grep no_ignore failed: %+v", r[0])
	}
	if !strings.Contains(r[0].Result, "node_modules") || !strings.Contains(r[0].Result, "logs/") {
		t.Fatalf("no_ignore=true 应能搜到被忽略的路径: %v", r[0].Result)
	}
}

func TestGlobRespectsIgnore(t *testing.T) {
	eng, f := engineWithFileTools(t)
	mk := func(rel string) {
		p := filepath.Join(f.root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("src/main.go")
	mk("node_modules/pkg/index.js")
	if err := os.WriteFile(filepath.Join(f.root, ".gitignore"), []byte("logs/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := seq(t, eng, core.ToolCall{Id: "1", Name: "glob", Arguments: `{"pattern":"**/*.go"}`})
	if r[0].IsError {
		t.Fatalf("glob failed: %+v", r[0])
	}
	if !strings.Contains(r[0].Result, "src/main.go") {
		t.Fatalf("应列出 src/main.go: %v", r[0].Result)
	}
	if strings.Contains(r[0].Result, "node_modules") {
		t.Fatalf("node_modules 应被跳过: %v", r[0].Result)
	}

	r = seq(t, eng, core.ToolCall{Id: "2", Name: "glob", Arguments: `{"pattern":"**/*.js","no_ignore":true}`})
	if r[0].IsError || !strings.Contains(r[0].Result, "node_modules") {
		t.Fatalf("no_ignore=true 应能列出 node_modules: %v", r[0].Result)
	}
}

// TestExplicitPathOverridesIgnore 命令行显式给出的路径不受忽略规则约束（与 rg 一致）。
func TestExplicitPathOverridesIgnore(t *testing.T) {
	eng, f := engineWithFileTools(t)
	p := filepath.Join(f.root, "node_modules", "pkg")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "index.js"), []byte("needle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := seq(t, eng, core.ToolCall{Id: "1", Name: "grep", Arguments: `{"pattern":"needle","path":"node_modules/pkg"}`})
	if r[0].IsError {
		t.Fatalf("grep failed: %+v", r[0])
	}
	if !strings.Contains(r[0].Result, "node_modules/pkg/index.js") {
		t.Fatalf("显式 path 应绕过忽略规则: %v", r[0].Result)
	}
}
