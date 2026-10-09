package builtin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 2026-10-09 审查发现并封堵的两条密钥泄漏链。
//
// 已有一条（软链 + 绝对路径）由 memory_path_guard_test.go 钉住。这里补的是**同型但
// 未覆盖**的两种形态，以及"不能把正常读写一起拦掉"的反向护栏。
//
// 共同的背景：~/.go-code/memory 是官方 auto memory 目录，官方形态要求模型用**标准
// 文件工具**读写它，所以该子树在放行名单里；而 read_file 跑在主 Agent 进程里、不进
// 沙箱，denyHarnessConfigPath 是它唯一的防线。

// setupSecretHome 造一个带 settings.json 的假 home，并让 HOME 与后续
// EvalSymlinks 的结果同基准（macOS 上 /tmp 是 /private/tmp 的链接，基准不一致会让
// 整个判定失效——这条基准约定见 builtin.go 的 denyHarnessConfigPath）。
func setupSecretHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"),
		[]byte(`{"api_key":"SECRET-VALUE-123"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestMemoryDirSymlinkCannotEscapeViaRelativePath：**相对路径**形态的软链接逃逸。
//
// 这是本次修复的核心。此前 resolve() 只在 `filepath.IsAbs(raw)` 分支里调
// denyHarnessConfigPath，相对分支只做 workspace 前缀判定。于是同一个文件只因写法
// 不同而结论相反：
//
//	绝对 "/workspace/cfg/settings.json"  → 被拦（解析出真实目标在 .go-code 内）
//	相对 "cfg/settings.json"             → 放行 → 读到明文 api_key
//
// 根因是"路径从哪来"被当成了"路径到哪去"的判据，而 deny 的本职是后者——Join 之后
// 两条分支产出的都是绝对路径，就该受同一套判定。
func TestMemoryDirSymlinkCannotEscapeViaRelativePath(t *testing.T) {
	home := setupSecretHome(t)
	cfgDir := filepath.Join(home, ".go-code")

	// 工作区就是记忆目录（Project Memory 的真实工作形态）
	ws := filepath.Join(cfgDir, "memory", "repos", "t1")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	// 模型用 bash（ln 不在危险命令名单里）建一个指向整个 .go-code 的链接
	link := filepath.Join(ws, "steal")
	if err := os.Symlink(cfgDir, link); err != nil {
		t.Skipf("本机不支持创建软链接: %v", err)
	}

	ft := NewFileTools(ws)
	readTool := toolByName(t, ft, "read_file")
	// 相对路径：join(ws, "steal/settings.json") 词法上确实落在 memory 子树内
	out, err := readTool.Call(context.Background(), "read_file", `{"path":"steal/settings.json"}`)
	if err == nil {
		t.Fatalf("❌ 相对路径软链接逃逸未被拦截，读到了: %.120s", out)
	}
	if containsSecret(out) {
		t.Fatalf("❌ 密钥泄漏: %s", out)
	}
}

// 变体：工作区是普通目录、链接只指向 .go-code 里那个文件本身。
func TestRelativeSymlinkToSecretFileIsBlockedFromPlainWorkspace(t *testing.T) {
	home := setupSecretHome(t)
	ws := t.TempDir()
	if err := os.Symlink(filepath.Join(home, ".go-code", "settings.json"),
		filepath.Join(ws, "leak.md")); err != nil {
		t.Skipf("本机不支持创建软链接: %v", err)
	}

	ft := NewFileTools(ws)
	readTool := toolByName(t, ft, "read_file")
	for _, p := range []string{`"leak.md"`, jsonStr(filepath.Join(ws, "leak.md"))} {
		out, err := readTool.Call(context.Background(), "read_file",
			`{"path":`+p+`}`)
		if err == nil || containsSecret(out) {
			t.Fatalf("❌ %s 泄漏: err=%v out=%.80s", p, err, out)
		}
	}
}

// TestMemoryDirHardlinkCannotEscapeToConfig：**硬链接**形态。
//
// EvalSymlinks 对硬链接完全无感——它不是软链，没有"链接"可解析，它自己就是真实名字。
// 而 `ln`（不带 -s）与 `ln -s` 同样常用，memory 子树又在沙箱放行名单里：
//
//	模型: bash "ln ~/.go-code/settings.json ~/.go-code/memory/repos/<id>/note.md"
//	模型: read_file ~/.go-code/memory/repos/<id>/note.md
//	  → 词法命中放行名单、EvalSymlinks 看不出异样 → 放行 → 读到明文 api_key
//
// 硬链与源文件共享 inode，所以唯一的判据是 inode 比对（见 isHardlinkToProtected）。
func TestMemoryDirHardlinkCannotEscapeToConfig(t *testing.T) {
	home := setupSecretHome(t)
	cfgDir := filepath.Join(home, ".go-code")
	mem := filepath.Join(cfgDir, "memory", "repos", "t1")
	if err := os.MkdirAll(mem, 0o700); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(mem, "note.md")
	if err := os.Link(filepath.Join(cfgDir, "settings.json"), note); err != nil {
		t.Skipf("本机不支持创建硬链接: %v", err)
	}

	ft := NewFileTools(t.TempDir())
	readTool := toolByName(t, ft, "read_file")
	out, err := readTool.Call(context.Background(), "read_file", `{"path":`+jsonStr(note)+`}`)
	if err == nil {
		t.Fatalf("❌ 硬链接逃逸未被拦截，读到了: %.120s", out)
	}
	if containsSecret(out) {
		t.Fatalf("❌ 密钥泄漏: %s", out)
	}

	// 写侧同样要拦：硬链可写意味着改的是 settings.json 本身
	writeTool := toolByName(t, ft, "write_file")
	if _, err := writeTool.Call(context.Background(), "write_file",
		`{"path":`+jsonStr(note)+`,"content":"PWNED"}`); err == nil {
		b, _ := os.ReadFile(filepath.Join(cfgDir, "settings.json"))
		if string(b) != `{"api_key":"SECRET-VALUE-123"}` {
			t.Fatal("❌ 经硬链改写了 settings.json")
		}
	}
}

// 反向护栏一：正常记忆文件仍然读写得动（过度拦截和漏防一样是 bug）。
func TestMemoryWritesStillWorkAfterSecurityFix(t *testing.T) {
	home := setupSecretHome(t)
	mem := filepath.Join(home, ".go-code", "memory", "repos", "t1")
	if err := os.MkdirAll(mem, 0o700); err != nil {
		t.Fatal(err)
	}
	topic := filepath.Join(mem, "note.md")
	body := "--- type: project --- 正文"
	if err := os.WriteFile(topic, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTools(t.TempDir())
	readTool := toolByName(t, ft, "read_file")
	if _, err := readTool.Call(context.Background(), "read_file", `{"path":`+jsonStr(topic)+`}`); err != nil {
		t.Fatalf("❌ 修复过度：正常记忆文件读不到了: %v", err)
	}

	// 相对路径也要能写（Project Memory 的真实用法：workspace = 记忆目录）
	ftWS := NewFileTools(mem)
	writeTool := toolByName(t, ftWS, "write_file")
	args := `{"path":"note.md","content":` + jsonStr(body) + `}`
	if _, err := writeTool.Call(context.Background(), "write_file", args); err != nil {
		t.Fatalf("❌ 修复过度：相对路径写不进正常记忆文件: %v", err)
	}
}

// 反向护栏二：工作区内的普通相对路径完全不受影响。
func TestPlainRelativePathsUnaffected(t *testing.T) {
	setupSecretHome(t)
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "ok.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(ws)
	readTool := toolByName(t, ft, "read_file")
	out, err := readTool.Call(context.Background(), "read_file", `{"path":"ok.md"}`)
	if err != nil {
		t.Fatalf("❌ 普通相对路径读不到: %v", err)
	}
	if !containsSub(out, "hello") {
		t.Fatalf("内容不对: %.80s", out)
	}
	// 目录外越界仍要拒（既有语义不变）
	if _, err := readTool.Call(context.Background(), "read_file", `{"path":"../escape.md"}`); err == nil {
		t.Fatal("❌ 相对路径逃逸不再被拒")
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
