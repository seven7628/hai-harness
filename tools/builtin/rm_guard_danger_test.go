package builtin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewGuardBlocks(t *testing.T) {
	// 造一个临时家目录来测家目录一级子目录
	home, _ := os.UserHomeDir()
	t.Logf("真实 HOME = %s", home)

	blocked := []string{
		// 事故复现（这些曾全部放行）
		"rm -rf /Users/admin/workspace/go-code",
		"rm -rf ~/workspace/go-code",
		"rm -rf $HOME/workspace/go-code",
		"rm -rf /Users/admin/cybertron-gogroup",
		"rm -rf ~/Desktop",
		"rm -rf ~/.go-code",
		"rm -rf ~/Documents",
		"find /Users/admin -maxdepth 2 -name go-code -exec rm -rf {} +",
		"find ~ -name '*.log' -delete",
	}
	for _, c := range blocked {
		r := checkHighRisk(c)
		if r == "" {
			t.Errorf("❌ 应拦截但放行了: %q", c)
		} else {
			t.Logf("✅ 拦截 %q → %s", c, r)
		}
	}

	allowed := []string{
		"rm -rf dist/",
		"rm -rf build/ tmp/",
		"rm old.txt",
		"rm -rf .git/",
		"rm -rf /tmp/foo",
		"rm -rf ~/.cache/go-build",
		"rm -rf ~/Library/Caches/foo",
		"find ./dist -name '*.map' -delete",
		"echo hi",
		"ls -la",
	}
	for _, c := range allowed {
		if r := checkHighRisk(c); r != "" {
			t.Errorf("❌ 应放行但拦截了: %q → %s", c, r)
		} else {
			t.Logf("✅ 放行 %q", c)
		}
	}
}

func TestGuardBlocksRepoRoot(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := checkHighRisk("rm -rf " + repo); r == "" {
		t.Errorf("❌ 仓库根应被拦截: %q", repo)
	} else {
		t.Logf("✅ 仓库根被拦截: %s", r)
	}
	// 非仓库的普通目录应放行
	plain := filepath.Join(dir, "plain")
	os.MkdirAll(plain, 0o755)
	if r := checkHighRisk("rm -rf " + plain); r != "" {
		t.Errorf("❌ 普通目录应放行: %s", r)
	} else {
		t.Logf("✅ 普通目录放行")
	}
}
