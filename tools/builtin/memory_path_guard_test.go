package builtin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 记忆子树的软链接逃逸防护。
//
// 背景（2026-09-29 真实漏洞，已复现并封堵）：放行 ~/.go-code/memory 是官方形态的
// 必要条件（模型用标准文件工具读写记忆，没有记忆专用工具），但它同时让模型获得了
// **在记忆目录里创建任意文件**的能力 —— bash 的 `ln -s` 不在危险命令名单里，而
// memory 子树在沙箱放行子树内。于是：
//
//	ln -s ~/.go-code/settings.json ~/.go-code/memory/repos/<id>/steal.md
//	read_file ~/.go-code/memory/repos/<id>/steal.md   → 词法落在 memory → 放行
//	→ os.ReadFile 跟随软链接 → 读到明文 api_key
//
// 关键点：read_file 跑在主 Agent 进程里、**不进沙箱**，denyHarnessConfigPath 是
// 它唯一的防线。所以防线必须解析软链接，且**解析后的 home 与解析后的目标要在
// 同一基准上比较**（macOS 上 /tmp → /private/tmp，基准不一致会让整个判定失效）。

func TestMemoryDirSymlinkCannotEscapeToConfig(t *testing.T) {
	home := setupCfgHome(t)
	cfgDir := filepath.Join(home, ".go-code")
	secret := filepath.Join(cfgDir, "settings.json")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("建配置目录: %v", err)
	}
	if err := os.WriteFile(secret, []byte(`{"api_key":"SECRET-VALUE-123"}`), 0o600); err != nil {
		t.Fatalf("写 settings: %v", err)
	}

	steal := filepath.Join(cfgDir, "memory", "repos", "t1", "steal.md")
	if err := os.MkdirAll(filepath.Dir(steal), 0o700); err != nil {
		t.Fatalf("建记忆目录: %v", err)
	}
	if err := os.Symlink(secret, steal); err != nil {
		t.Skipf("本机不支持创建软链接: %v", err)
	}

	ft := NewFileTools(t.TempDir())
	readTool := toolByName(t, ft, "read_file")
	out, err := readTool.Call(context.Background(), "read_file", `{"path":`+jsonStr(steal)+`}`)
	if err == nil {
		t.Fatalf("❌ 软链接逃逸未被拦截，读到了: %s", out)
	}
	if containsSecret(out) {
		t.Fatalf("❌ 密钥泄漏: %s", out)
	}
}

// 绕过变体：软链接指向上级目录（整棵子树都变成桥），判定不能只处理"直接指向文件"。
func TestMemoryDirSymlinkToParentDirCannotEscape(t *testing.T) {
	home := setupCfgHome(t)
	cfgDir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(filepath.Join(cfgDir, "memory", "repos", "t1"), 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{"api_key":"S2"}`), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	// 链接指向 ~/.go-code 本身：<memory>/repos/t1/up -> ~/.go-code
	up := filepath.Join(cfgDir, "memory", "repos", "t1", "up")
	if err := os.Symlink(cfgDir, up); err != nil {
		t.Skipf("本机不支持软链接: %v", err)
	}

	ft := NewFileTools(t.TempDir())
	readTool := toolByName(t, ft, "read_file")
	// <memory>/repos/t1/up/settings.json 词法上仍在 memory 子树内
	target := filepath.Join(up, "settings.json")
	if _, err := readTool.Call(context.Background(), "read_file", `{"path":`+jsonStr(target)+`}`); err == nil {
		t.Fatalf("❌ 经目录软链接读到了 settings.json（%s）", target)
	}
}

// 反向护栏：防护不能把"正常读记忆"一起拦掉 —— 过度拦截和漏防一样是 bug。
func TestMemoryDirStillReachableAfterSymlinkFix(t *testing.T) {
	home := setupCfgHome(t)
	topic := filepath.Join(home, ".go-code", "memory", "repos", "t1", "note.md")
	if err := os.MkdirAll(filepath.Dir(topic), 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(topic, []byte("---\ntype: project\n---\n\nbody\n"), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	ft := NewFileTools(t.TempDir())
	readTool := toolByName(t, ft, "read_file")
	if _, err := readTool.Call(context.Background(), "read_file", `{"path":`+jsonStr(topic)+`}`); err != nil {
		t.Fatalf("❌ 修复过度：正常记忆文件读不到了: %v", err)
	}
}

// 越界目标在 ~/.go-code 之外时仍应放行：那与模型直接读该绝对路径无区别，
// 不是 denyHarnessConfigPath 的职责（它是"保护 .go-code"，不是"限制工作区"）。
func TestNonConfigSymlinkTargetStillAllowed(t *testing.T) {
	home := setupCfgHome(t)
	outside := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(outside, []byte("plain\n"), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	link := filepath.Join(home, ".go-code", "memory", "repos", "t1", "n.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("本机不支持软链接: %v", err)
	}
	ft := NewFileTools(t.TempDir())
	readTool := toolByName(t, ft, "read_file")
	if _, err := readTool.Call(context.Background(), "read_file", `{"path":`+jsonStr(link)+`}`); err != nil {
		t.Fatalf("❌ 拦掉了 .go-code 之外的普通文件（职责越界）: %v", err)
	}
}

// TestAllowedSubtreesMatchSandboxOpenSubpaths 钉住两处放行名单同步。
//
// sandbox.defaultOpenSubpaths 放行 shell 侧、allowedConfigSubtrees 放行文件工具侧，
// 两者语义必须一致。注释里写了"两处必须同步"，但那只是注释 —— 本仓库吃过
// 「机制在生产悄悄失效数月」的亏，所以要有断言。
//
// 这里比对**去掉前缀后的相对路径集合**（defaultOpenSubpaths 用 "~/..." 相对 home）。
func TestAllowedSubtreesMatchSandboxOpenSubpaths(t *testing.T) {
	got := map[string]bool{}
	for _, s := range allowedConfigSubtrees {
		got[s] = true
	}
	// 预期集合：与 sandbox.defaultOpenSubpaths 的三项逐字对应
	want := map[string]bool{"plugins": true, "runtime": true, "memory": true}
	for k := range want {
		if !got[k] {
			t.Errorf("文件工具放行名单缺少 %q（沙箱侧 defaultOpenSubpaths 有）—— 两处不同步", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("文件工具放行名单多出 %q —— 确认它是否也需要加进 sandbox.defaultOpenSubpaths", k)
		}
	}
}

func setupCfgHome(t *testing.T) string {
	t.Helper()
	// 避开 /tmp（macOS 上它是 /private/tmp 的软链接，会掩盖基准不一致的 bug）；
	// t.TempDir() 在 macOS 上返回 /var/folders/... 也是软链接链，但 EvalSymlinks
	// 会对两边同时生效，所以一致。
	home := t.TempDir()
	// 解析一次，让 HOME 与后续 EvalSymlinks 结果同基准
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	return home
}

func toolByName(t *testing.T, ft *FileTools, name string) interface {
	Call(ctx context.Context, name, args string) (string, error)
} {
	t.Helper()
	for _, tl := range ft.Tools() {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("工具未注册: %s", name)
	return nil
}

func containsSecret(s string) bool {
	for i := 0; i+4 <= len(s); i++ {
		if s[i:i+4] == "SECR" {
			return true
		}
	}
	return false
}

func jsonStr(s string) string {
	out := []byte{'"'}
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			out = append(out, '\\', s[i])
		} else {
			out = append(out, s[i])
		}
	}
	return string(append(out, '"'))
}
