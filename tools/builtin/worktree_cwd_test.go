package builtin

// git_worktree 的 enter/exit（P1）与「create 后切进隔离目录」回归测试。
//
// 来源：docs/AGENT_CWD_DESIGN_2026-11-19.md §11.6（enter/exit 语义）+ §12.7（V7 端到端）
// + /private/tmp/patch_p1.py。
//
// ⚠️ 与 §11.6 草案的偏差（按 §16.2 D5 实施）：
//   - Scope 判据是**目录边界**（{workspace}/.go-code/worktrees/<name>），
//     不是「本会话创建过哪些」—— 工具实例随引擎重建而重建（切模型/切人格），
//     记不住创建历史；
//   - workdir.State 的 Set/Reset **不返回**旧值/状态（见 workdir/workdir.go），
//     回执由 tools/builtin 自己拼（cwdChangeNotice）。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/workdir"
)

// initGitRepo 造一个至少有 1 个提交的临时 git 仓库（worktree add 需要 HEAD）。
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

// TestGitWorktreeCreateSwitchesCwdAndExitReturns create 之后会话 cwd 进隔离目录；
// exit 之后回到工作区根。
func TestGitWorktreeCreateSwitchesCwdAndExitReturns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}
	root := initGitRepo(t)
	wd := workdir.New(root)
	tool := NewGitWorktreeToolWithWorkdir(wd)
	ctx := bashCtx("run-1", "")

	out, err := tool.Call(ctx, "", `{"action":"create","name":"wt-a"}`)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	want := filepath.Join(root, worktreeBaseRel, "wt-a")
	if !strings.Contains(out, "Working directory is now "+want) {
		t.Fatalf("create 结果里没有切目录回执：\n%s", out)
	}
	if got := wd.Current(workdir.MainKey); filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("create 后会话 cwd = %q, want %q", got, want)
	}

	// enter 幂等（同一目录，回执照样给：这是显式动作）
	if _, err := tool.Call(ctx, "", `{"action":"enter","name":"wt-a"}`); err != nil {
		t.Fatalf("enter 失败: %v", err)
	}

	out, err = tool.Call(ctx, "", `{"action":"exit"}`)
	if err != nil {
		t.Fatalf("exit 失败: %v", err)
	}
	if !strings.Contains(out, "Working directory is now "+wd.Root()) {
		t.Fatalf("exit 结果里没有回到根的回执：\n%s", out)
	}
	if got := wd.Current(workdir.MainKey); filepath.Clean(got) != filepath.Clean(wd.Root()) {
		t.Fatalf("exit 后会话 cwd = %q, want 根 %q", got, wd.Root())
	}
}

// TestGitWorktreeEnterRejectsEscape 只认管理目录下的 worktree（拒绝逃逸 name）。
func TestGitWorktreeEnterRejectsEscape(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}
	root := initGitRepo(t)
	wd := workdir.New(root)
	tool := NewGitWorktreeToolWithWorkdir(wd)
	ctx := bashCtx("run-1", "")

	for _, name := range []string{"../escape", "/etc", "~/x"} {
		if _, err := tool.Call(ctx, "", `{"action":"enter","name":"`+name+`"}`); err == nil {
			t.Fatalf("enter %q 应被拒绝", name)
		}
	}
	if got := wd.Current(workdir.MainKey); filepath.Clean(got) != filepath.Clean(root) {
		t.Fatalf("被拒绝的 enter 改了 cwd：%q", got)
	}
}

// TestGitWorktreeRemoveCurrentResetsCwd 删掉**当前所在**的 worktree ⇒ cwd 回根。
func TestGitWorktreeRemoveCurrentResetsCwd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}
	root := initGitRepo(t)
	wd := workdir.New(root)
	tool := NewGitWorktreeToolWithWorkdir(wd)
	ctx := bashCtx("run-1", "")

	if _, err := tool.Call(ctx, "", `{"action":"create","name":"wt-b"}`); err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	out, err := tool.Call(ctx, "", `{"action":"remove","name":"wt-b"}`)
	if err != nil {
		t.Fatalf("remove 失败: %v", err)
	}
	if got := wd.Current(workdir.MainKey); filepath.Clean(got) != filepath.Clean(wd.Root()) {
		t.Fatalf("remove 当前 worktree 后 cwd = %q, want 根", got)
	}
	if !strings.Contains(out, "Working directory is now "+wd.Root()) {
		t.Fatalf("remove 结果里没有回执：\n%s", out)
	}
}
