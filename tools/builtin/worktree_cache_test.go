package builtin

// git_worktree 的**判定缓存 + 输出上限 + 工具级时限**测试（2026-11-17，第 7 步）。
//
// 改造前：每次调用都跑一个 `git rev-parse --is-inside-work-tree` 进程
// （list 是最高频动作 ⇒ 每次 15–50 ms 的进程启动 + git 二进制加载）；
// 且 git 的输出缓冲**无上限**（引擎的 20 KB 闸门在这之后才生效）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGit 装一个假的 git（记录每次被调用），返回 PATH 前置。
func fakeGit(t *testing.T, countFile string) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\necho x >> " + countFile + "\nfor a in \"$@\"; do\n  if [ \"$a\" = \"rev-parse\" ]; then echo true; exit 0; fi\ndone\necho 'worktree a  /worktrees/a [main]'\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
}

// TestGitWorktreeRepoCheckCached 「是不是 git 工作树」只判定一次。
func TestGitWorktreeRepoCheckCached(t *testing.T) {
	countFile := filepath.Join(t.TempDir(), "count")
	fakeGit(t, countFile)

	tool := NewGitWorktreeTool(t.TempDir())
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := tool.Call(ctx, "", `{"action":"list"}`); err != nil {
			t.Fatalf("第 %d 次调用失败: %v", i+1, err)
		}
	}
	b, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Count(strings.TrimSpace(string(b)), "\n") + 1
	// 3 次 list：判定只跑 1 次 + 每次 list 自己 1 次 ⇒ 4 次 git 进程
	// （没有缓存就是 6 次）。
	if calls != 4 {
		t.Errorf("git 进程数 = %d，want 4（判定应被缓存：3 次判定 + 3 次 list ⇒ 缓存后只 1 次判定）", calls)
	}
}

// TestGitWorktreeDeclaresTimeout 声明工具级时限。
func TestGitWorktreeDeclaresTimeout(t *testing.T) {
	tool := NewGitWorktreeTool(t.TempDir())
	d := tool.ToolTimeout()
	if d <= 0 || d > 5*time.Minute {
		t.Errorf("工具级时限不合理: %v", d)
	}
}

// TestGitWorktreeOutputCapped git 输出有上限（超限截断并明说）。
func TestGitWorktreeOutputCapped(t *testing.T) {
	var b lockedBuffer
	chunk := make([]byte, 64<<10)
	for i := 0; i < 40; i++ { // 2.5 MB > 1 MiB 上限
		if _, err := b.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	s := b.String()
	if len(s) > gitOutputCap+1024 {
		t.Errorf("输出应被截断到上限附近，实际 %d 字节", len(s))
	}
	if !strings.Contains(s, "git output truncated") {
		t.Errorf("截断时应明说:\n%s", s[len(s)-120:])
	}
}
