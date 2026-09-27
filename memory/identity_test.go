package memory

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo 建一个带一次提交的仓库；remote 非空时配置 origin。
func newRepo(t *testing.T, dir, remote string) {
	t.Helper()
	run(t, dir, "init", "-q")
	run(t, dir, "config", "user.email", "t@example.com")
	run(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-qm", "init")
	if remote != "" {
		run(t, dir, "remote", "add", "origin", remote)
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// 与生产路径同口径：禁用交互式认证，避免个别环境里 git 卡在 stdin 上。
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir=%s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// withHome 把 home 指到临时目录，避免测试碰真实 ~/.go-code。
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows
	return home
}

// ---------------------------------------------------------------- 身份派生

// 有 remote：由 remote URL 规范化出 repo id（官方：<project> 由 git 仓库派生）。
func TestResolve_FromRemote(t *testing.T) {
	withHome(t)
	repo := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	newRepo(t, repo, "git@github.com:seven7628/hai-harness.git")

	// 子目录也必须落到同一份记忆（官方：subdirectories share one memory directory）。
	for _, workdir := range []string{repo, filepath.Join(repo, "a", "b")} {
		if err := os.MkdirAll(workdir, 0o750); err != nil {
			t.Fatal(err)
		}
		d, err := Resolve(context.Background(), workdir)
		if err != nil {
			t.Fatalf("workdir=%s: %v", workdir, err)
		}
		if d.ID != "github.com-seven7628-hai-harness" {
			t.Fatalf("workdir=%s: ID = %q, want github.com-seven7628-hai-harness", workdir, d.ID)
		}
		if want := filepath.Join(homeDir(t), ".go-code", "memory", "repos", d.ID); d.Path != want {
			t.Fatalf("Path = %q, want %q", d.Path, want)
		}
	}
}

// 同一 remote 的不同 URL 写法必须归一到同一个 ID（换协议/端口不算新仓库）。
func TestResolve_RemoteURLNormalization(t *testing.T) {
	withHome(t)
	cases := []struct {
		remote string
		want   string
	}{
		{"git@github.com:Owner/Repo.git", "github.com-owner-repo"},
		{"https://github.com/Owner/Repo.git", "github.com-owner-repo"},
		{"https://github.com/Owner/Repo", "github.com-owner-repo"},
		{"ssh://git@github.com:22/Owner/Repo.git", "github.com-owner-repo"},
		{"https://user:token@github.com/Owner/Repo.git", "github.com-owner-repo"},
	}
	for _, c := range cases {
		repo := filepath.Join(t.TempDir(), "r")
		if err := os.MkdirAll(repo, 0o750); err != nil {
			t.Fatal(err)
		}
		newRepo(t, repo, c.remote)
		d, err := Resolve(context.Background(), repo)
		if err != nil {
			t.Fatalf("%s: %v", c.remote, err)
		}
		if d.ID != c.want {
			t.Fatalf("remote %s: ID = %q, want %q", c.remote, d.ID, c.want)
		}
	}
}

// 无 remote：由 git common dir 派生，且**真的建两个 worktree** 比较 ID 必须相等。
// 这是官方共享语义的承重测试——用 toplevel 派生会在这里失败。
func TestResolve_WorktreesShareIdentity(t *testing.T) {
	withHome(t)
	base := t.TempDir()
	main := filepath.Join(base, "main")
	if err := os.MkdirAll(main, 0o750); err != nil {
		t.Fatal(err)
	}
	newRepo(t, main, "") // 无 remote
	wt := filepath.Join(base, "wt")
	run(t, main, "worktree", "add", "-q", wt, "-b", "wt-branch")

	// 前提校验：worktree 的 toplevel 确实不同（否则本测试是空转的）。
	topMain := run(t, main, "rev-parse", "--show-toplevel")
	topWT := run(t, wt, "rev-parse", "--show-toplevel")
	if topMain == topWT {
		t.Fatalf("toplevel 相同（%q），worktree 未真正建立", topMain)
	}

	dMain, err := Resolve(context.Background(), main)
	if err != nil {
		t.Fatal(err)
	}
	dWT, err := Resolve(context.Background(), wt)
	if err != nil {
		t.Fatal(err)
	}
	if dMain.ID != dWT.ID {
		t.Fatalf("worktree 身份不一致: main=%q wt=%q（官方要求同一仓库共享一份记忆）", dMain.ID, dWT.ID)
	}
	if dMain.Path != dWT.Path {
		t.Fatalf("记忆目录不一致: %q vs %q", dMain.Path, dWT.Path)
	}
	// 无 remote 的 ID 形如 `<目录名>-<12 位哈希>`，前缀可辨认。
	if !strings.HasPrefix(dMain.ID, "main-") {
		t.Fatalf("ID = %q, 期望以目录名 main- 开头", dMain.ID)
	}
}

// 无 remote 的两个不同仓库不能撞车（同名目录位于不同父目录）。
func TestResolve_NoRemoteDistinctRepos(t *testing.T) {
	withHome(t)
	base := t.TempDir()
	var ids []string
	for _, parent := range []string{"a", "b"} {
		repo := filepath.Join(base, parent, "api")
		if err := os.MkdirAll(repo, 0o750); err != nil {
			t.Fatal(err)
		}
		newRepo(t, repo, "")
		d, err := Resolve(context.Background(), repo)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.ID)
	}
	if ids[0] == ids[1] {
		t.Fatalf("两个同名仓库得到同一 ID %q（会静默共用一份记忆）", ids[0])
	}
}

// 无 git：返回明确错误，不启用记忆；不发明绝对路径哈希 fallback。
func TestResolve_NoGit(t *testing.T) {
	withHome(t)
	plain := filepath.Join(t.TempDir(), "not-a-repo")
	if err := os.MkdirAll(plain, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(context.Background(), plain); err == nil {
		t.Fatal("非 git 目录应当报错（待定项 1：无 git 时不启用记忆）")
	} else if !errors.Is(err, ErrNoGit) {
		t.Fatalf("err = %v, 期望包装 ErrNoGit", err)
	}
}

// 非 origin 的 remote 也能取到身份（官方未规定取哪个，本包取第一个）。
func TestResolve_NonOriginRemote(t *testing.T) {
	withHome(t)
	repo := filepath.Join(t.TempDir(), "r")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	newRepo(t, repo, "")
	run(t, repo, "remote", "add", "upstream", "https://gitlab.com/g/p.git")
	d, err := Resolve(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "gitlab.com-g-p" {
		t.Fatalf("ID = %q, want gitlab.com-g-p", d.ID)
	}
}

// 本地路径 remote 没有 host：解析不出可辨认 ID 时退回 git common dir，而不是猜名字。
func TestResolve_LocalPathRemoteFallsBack(t *testing.T) {
	withHome(t)
	repo := filepath.Join(t.TempDir(), "r")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	newRepo(t, repo, "")
	run(t, repo, "remote", "add", "origin", "/tmp/some/local/repo")
	d, err := Resolve(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.ID, "tmp") {
		t.Fatalf("ID = %q, 本地路径 remote 不应被拆成 host-owner-repo", d.ID)
	}
	if !strings.HasPrefix(d.ID, "r-") {
		t.Fatalf("ID = %q, 期望退回 git common dir 派生的 r-<hash>", d.ID)
	}
}

func TestResolve_EmptyWorkdir(t *testing.T) {
	withHome(t)
	if _, err := Resolve(context.Background(), "  "); err == nil {
		t.Fatal("空工作目录应当报错")
	}
}

// 注入式 Run：确认默认实现不依赖真实 git 也能被替换（后续 Step 3/4 测试用得上）。
func TestResolver_InjectedRun(t *testing.T) {
	withHome(t)
	r := &Resolver{Run: func(_ context.Context, dir string, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "rev-parse --show-toplevel":
			return "/tmp/x\n", nil
		case "rev-parse --git-common-dir":
			return "/tmp/x/.git\n", nil
		case "remote get-url origin":
			return "https://github.com/a/b.git\n", nil
		}
		return "", exec.ErrNotFound
	}}
	d, err := r.Resolve(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "github.com-a-b" {
		t.Fatalf("ID = %q", d.ID)
	}
}

func TestMemoryRootAndReposRoot(t *testing.T) {
	home := withHome(t)
	root, err := MemoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(home, ".go-code", "memory") {
		t.Fatalf("MemoryRoot = %q", root)
	}
	repos, err := ReposRoot()
	if err != nil {
		t.Fatal(err)
	}
	if repos != filepath.Join(home, ".go-code", "memory", "repos") {
		t.Fatalf("ReposRoot = %q", repos)
	}
}

func TestMemoryRoot_NoHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("该平台不依赖 HOME 环境变量")
	}
	if _, err := MemoryRoot(); err != ErrNoHome {
		t.Fatalf("err = %v, want ErrNoHome", err)
	}
}

func homeDir(t *testing.T) string {
	t.Helper()
	h, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return h
}
