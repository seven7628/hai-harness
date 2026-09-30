package memory

import (
	"path/filepath"
	"strings"
	"testing"
)

// 子 agent 的记忆目录**必须**跟随 Config.Directory。
//
// 回归动机：初版 AgentsRoot() 是包级函数、用 MemoryRoot() 取真实的
// ~/.go-code/memory，绕过了 Config.Directory。于是测试里
// `Config{Directory: <tmp>}` 的**主**目录正确落在临时目录，**子**目录却指向用户
// 真实目录 —— 两个根方向相反，归属判定必然失效，且测试会往真目录里写。
//
// 这与 idFromGitDir 那条同源：「当前恰好没出问题」不是「这一项正确」。
func TestSubagentDirHonoursConfigDirectory(t *testing.T) {
	withHome(t) // 即便 HOME 正常，也必须以 Config.Directory 为准
	root := t.TempDir()
	c := Config{Directory: root}
	repo := Dir{ID: "github.com-o-proj", Path: filepath.Join(root, "repos", "github.com-o-proj")}

	sub, ok := c.SubagentDir(repo, "docs-writer")
	if !ok {
		t.Fatal("SubagentDir 应当成功")
	}
	if !strings.HasPrefix(sub.Path, root+string(filepath.Separator)) {
		t.Fatalf("子目录 %q 不在 Config.Directory %q 之下——又绕过了配置", sub.Path, root)
	}

	// 逆映射必须给出同一个目录，否则 postWrite 的归属判定会失效。
	back, ok := c.SubagentDirFromPath(repo, filepath.Join(sub.Path, "topic.md"))
	if !ok || back.Path != sub.Path {
		t.Fatalf("SubagentDirFromPath = %q ok=%v, want %q", back.Path, ok, sub.Path)
	}
}

// 子 agent 目录与主目录必须不相交：整套"按路径判归属"的设计建立在这上面
// （postWrite 钩子拿不到调用者身份，只能靠路径）。
func TestSubagentDirDisjointFromRepoDir(t *testing.T) {
	withHome(t)
	root := t.TempDir()
	c := Config{Directory: root}
	repo := Dir{ID: "github.com-o-proj", Path: filepath.Join(root, "repos", "github.com-o-proj")}
	sub, _ := c.SubagentDir(repo, "docs-writer")

	if sub.Path == repo.Path {
		t.Fatalf("子目录与主目录相同：%q", sub.Path)
	}
	if strings.HasPrefix(sub.Path, repo.Path+string(filepath.Separator)) {
		t.Fatalf("子目录落在主目录之内：%q ⊂ %q（会混进仓库记忆列表）", sub.Path, repo.Path)
	}
	if _, ok := c.SubagentDirFromPath(repo, filepath.Join(repo.Path, "topic.md")); ok {
		t.Fatal("主目录下的路径不该被判成子 agent 目录")
	}
}

// 非法名字不启用（而不是落到意外目录）。
func TestSubagentDirRejectsBadNames(t *testing.T) {
	withHome(t)
	c := Config{Directory: t.TempDir()}
	repo := Dir{ID: "id", Path: filepath.Join(c.Directory, "repos", "id")}
	for _, bad := range []string{"", "   ", "///", "..", "."} {
		if _, ok := c.SubagentDir(repo, bad); ok {
			t.Fatalf("名字 %q 应被拒绝", bad)
		}
	}
}
