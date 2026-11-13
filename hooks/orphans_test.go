package hooks

import (
	"os"
	"path/filepath"
	"testing"
)

// 这条测试守的是"脚本在、配置里没引用"这个现场——它此前完全是无声的。
func TestUnreferencedScripts(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"guard.sh", "fmt.sh", ".hidden.sh"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Events: map[Event][]Group{
		EventPostToolUse: {{
			Hooks: []Handler{{Type: "command", Command: "bash ~/.go-code/hooks/guard.sh"}},
			scope: scopeUser,
		}},
	}}

	got, err := UnreferencedScripts(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "fmt.sh" {
		t.Fatalf("应只报 fmt.sh（hidden 与目录要跳过，guard.sh 已引用），得到 %v", got)
	}
}

// 配置为空时，目录里的一切都算没被引用——这正是"写了脚本但没配 hooks"的现场。
func TestUnreferencedScriptsWithNilConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.sh"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := UnreferencedScripts(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "a.sh" {
		t.Fatalf("空配置下 a.sh 应被报出，得到 %v", got)
	}
}

// 目录不存在不是错误（大多数机器上没有这个目录）。
func TestUnreferencedScriptsMissingDir(t *testing.T) {
	if _, err := UnreferencedScripts(nil, filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("目录不存在应返回错误，由调用方决定是否静默")
	}
}
