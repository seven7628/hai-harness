package main

// git.go 单元测试：t.TempDir() 建真实 git 仓库做端到端解析验证
// （snapshot 状态/numstat 行数/rename 归一、log 分页、diff、非法 hash 拒绝、大 diff 截断）。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitInit：建临时仓库并配置本地身份（不依赖全局 git config）
func gitInit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@test.test")
	run("config", "user.name", "tester")
	return dir
}

// w：写文件（含父目录）
func w(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGitSnapshotClean(t *testing.T) {
	dir := gitInit(t)
	w(t, dir, "a.txt", "hello\n")
	c := exec.Command("git", "-C", dir, "add", "-A")
	_ = c.Run()
	c = exec.Command("git", "-C", dir, "commit", "-qm", "init")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	s, err := gitSnapshot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsRepo || !s.Clean || len(s.Files) != 0 {
		t.Fatalf("期望干净仓库，得到 %+v", s)
	}
}

func TestGitSnapshotDirtyAndNumstat(t *testing.T) {
	dir := gitInit(t)
	w(t, dir, "a.txt", "l1\nl2\nl3\n")
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-qm", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	// 未暂存修改：+2 -1；未跟踪文件；已暂存修改：+1 -0
	w(t, dir, "a.txt", "l1\nX\nX\nl3\n")
	w(t, dir, "untracked.txt", "new\n")
	w(t, dir, "staged.txt", "base\n")
	if out, err := exec.Command("git", "-C", dir, "add", "staged.txt").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	w(t, dir, "staged.txt", "base\nmore\n") // 暂存后再改：既有 staged 也有 unstaged

	s, err := gitSnapshot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsRepo || s.Clean {
		t.Fatalf("期望脏仓库: %+v", s)
	}
	if s.Modified != 1 || s.Untracked != 1 || s.Staged < 1 {
		t.Fatalf("计数不符: modified=%d untracked=%d staged=%d", s.Modified, s.Untracked, s.Staged)
	}
	byPath := map[string]gitFile{}
	for _, f := range s.Files {
		byPath[f.Path] = f
	}
	if f := byPath["a.txt"]; f.Additions != 2 || f.Deletions != 1 {
		t.Fatalf("a.txt numstat 期望 +2 -1，得到 %+v", f)
	}
	if f := byPath["staged.txt"]; !f.Staged {
		t.Fatalf("staged.txt 应标记已暂存: %+v", f)
	}
	if f := byPath["untracked.txt"]; f.Status != "untracked" || f.Additions != 0 {
		t.Fatalf("untracked.txt 解析不符: %+v", f)
	}
}

func TestGitSnapshotRename(t *testing.T) {
	dir := gitInit(t)
	w(t, dir, "old.txt", "content\n")
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-qm", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "mv", "old.txt", "new.txt").CombinedOutput(); err != nil {
		t.Fatalf("mv: %v\n%s", err, out)
	}
	s, err := gitSnapshot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Files) != 1 {
		t.Fatalf("期望 1 条 rename 记录: %+v", s.Files)
	}
	if s.Files[0].Path != "new.txt" {
		t.Fatalf("rename 应只保留新路径，得到 %q", s.Files[0].Path)
	}
}

func TestNumstatPath(t *testing.T) {
	cases := map[string]string{
		"plain.txt":            "plain.txt",
		"old.txt => new.txt":   "new.txt",
		"{old => new}.txt":     "new.txt",
		"head{a => b}tail":     "headbtail",
		" dir/nested file.txt": "dir/nested file.txt",
	}
	for in, want := range cases {
		if got := numstatPath(in); got != want {
			t.Errorf("numstatPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGitLogPagination(t *testing.T) {
	dir := gitInit(t)
	w(t, dir, "f.txt", "")
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-qm", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit init: %v\n%s", err, out)
	}
	for i := 0; i < 4; i++ {
		w(t, dir, "f.txt", strings.Repeat("x\n", i+1))
		if out, err := exec.Command("git", "-C", dir, "commit", "-aqm", string(rune('a'+i))).CombinedOutput(); err != nil {
			t.Fatalf("commit %d: %v\n%s", i, err, out)
		}
	}
	p1, err := gitLog(context.Background(), dir, 2, 0)
	if err != nil || len(p1) != 2 {
		t.Fatalf("第一页期望 2 条: %v %v", p1, err)
	}
	p2, err := gitLog(context.Background(), dir, 2, 2)
	if err != nil || len(p2) != 2 {
		t.Fatalf("第二页期望 2 条: %v %v", p2, err)
	}
	if p1[0].Hash == p2[0].Hash {
		t.Fatal("分页重叠：skip 未生效")
	}
	p3, err := gitLog(context.Background(), dir, 2, 4)
	if err != nil || len(p3) != 1 {
		t.Fatalf("尾页期望 1 条: %v %v", p3, err)
	}
	// has_more 判定与 dispatch 一致：len(commits) == limit
	if !(len(p1) == 2 && len(p3) != 2) {
		t.Fatal("has_more 粗判前提不成立")
	}
}

func TestGitCommitDiffInvalidHash(t *testing.T) {
	dir := gitInit(t)
	for _, bad := range []string{"abc", "xyz1234", "ggggggg", strings.Repeat("a", 65)} {
		if _, err := gitCommitDiff(context.Background(), dir, bad); err == nil {
			t.Errorf("非法 hash %q 应被拒绝", bad)
		}
	}
}

// TestGitLogGraphAndUnborn：merge 仓库验证 graph 前缀/延续行解析；空仓库验证 unborn HEAD 空历史
func TestGitLogGraphAndUnborn(t *testing.T) {
	// 空仓库：git log 128 → 应返回空历史而非错误（此前误报成面板错误）
	empty := gitInit(t)
	if commits, err := gitLog(context.Background(), empty, 30, 0); err != nil || len(commits) != 0 {
		t.Fatalf("unborn HEAD 应返回空历史: commits=%d err=%v", len(commits), err)
	}

	// 分支 + 合并：graph 应含 '*'，merge 提交应有延续行
	dir := gitInit(t)
	w(t, dir, "f.txt", "base\n")
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-qm", "base").CombinedOutput(); err != nil {
		t.Fatalf("commit base: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "checkout", "-qb", "feature").CombinedOutput(); err != nil {
		t.Fatalf("branch: %v\n%s", err, out)
	}
	w(t, dir, "f.txt", "feature\n")
	if out, err := exec.Command("git", "-C", dir, "commit", "-aqm", "feature").CombinedOutput(); err != nil {
		t.Fatalf("commit feature: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "checkout", "-q", "master").CombinedOutput(); err != nil {
		t.Fatalf("checkout: %v\n%s", err, out)
	}
	w(t, dir, "g.txt", "main\n")
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-qm", "main").CombinedOutput(); err != nil {
		t.Fatalf("commit main: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "merge", "-q", "--no-ff", "feature", "-m", "merge").CombinedOutput(); err != nil {
		t.Fatalf("merge: %v\n%s", err, out)
	}
	commits, err := gitLog(context.Background(), dir, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 4 {
		t.Fatalf("期望 4 条提交: %d", len(commits))
	}
	bySubject := map[string]gitCommit{}
	for _, c := range commits {
		bySubject[c.Subject] = c
		if !strings.Contains(c.Graph, "*") {
			t.Errorf("提交 %q 的 graph 应包含 '*': %q", c.Subject, c.Graph)
		}
		if c.Hash == "" || len(c.Hash) != 40 || !isHexRun(c.Hash) {
			t.Errorf("hash 解析异常: %q", c.Hash)
		}
	}
	m := bySubject["merge"]
	if m.GraphAfter == "" {
		t.Errorf("merge 提交应有 graph 延续行（|\\ 等），graph=%q", m.Graph)
	}
}

func TestGitSnapshotHead(t *testing.T) {
	dir := gitInit(t)
	s, err := gitSnapshot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Head != "" {
		t.Fatalf("unborn HEAD 时 head 应为空，得到 %q", s.Head)
	}
	w(t, dir, "a.txt", "x\n")
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-qm", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	s, err = gitSnapshot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Head) < 7 {
		t.Fatalf("提交后 head 应为 short hash，得到 %q", s.Head)
	}
}

func TestGitDiffTruncated(t *testing.T) {
	dir := gitInit(t)
	w(t, dir, "big.txt", "")
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-qm", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	// ~800KB 新增内容 → diff 超过 512KB 截断
	w(t, dir, "big.txt", strings.Repeat(strings.Repeat("x", 40)+"\n", 20000))
	out, err := gitDiff(context.Background(), dir, "big.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "[diff truncated]") {
		t.Fatalf("大 diff 应被截断，len=%d tail=%q", len(out), tailOf(out, 40))
	}
	if len(out) > 512*1024+64 {
		t.Fatalf("截断后仍超长: %d", len(out))
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// TestGitWriteOps：stage/unstage/discard/commit 往返 + 路径安全 + message 校验
func TestGitWriteOps(t *testing.T) {
	ctx := context.Background()
	dir := gitInit(t)
	w(t, dir, "a.txt", "v1\n")
	w(t, dir, "b.txt", "keep\n")
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-qm", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	// 修改 tracked a.txt + 新增 untracked c.txt
	w(t, dir, "a.txt", "v2\n")
	w(t, dir, "c.txt", "temp\n")

	// 路径安全：逃逸/绝对路径/空列表拒绝（空串条目宽容跳过，不视为错误）
	for _, bad := range [][]string{{"../x"}, {"/etc/passwd"}, {}, {"a.txt", "../.."}} {
		if _, err := gitStage(ctx, dir, bad); err == nil {
			t.Errorf("gitStage(%v) 应被拒绝", bad)
		}
	}

	// stage a + c → 快照标记 staged
	if n, err := gitStage(ctx, dir, []string{"a.txt", "c.txt"}); err != nil || n != 2 {
		t.Fatalf("stage 期望 2: n=%d err=%v", n, err)
	}
	s, err := gitSnapshot(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	staged := map[string]bool{}
	for _, f := range s.Files {
		staged[f.Path] = f.Staged
	}
	if !staged["a.txt"] || !staged["c.txt"] {
		t.Fatalf("a/c 应已暂存: %+v", s.Files)
	}

	// unstage a → 回到未暂存修改
	if n, err := gitUnstage(ctx, dir, []string{"a.txt"}); err != nil || n != 1 {
		t.Fatalf("unstage 期望 1: n=%d err=%v", n, err)
	}
	s, _ = gitSnapshot(ctx, dir)
	for _, f := range s.Files {
		if f.Path == "a.txt" && f.Staged {
			t.Fatal("a.txt 应已取消暂存")
		}
	}

	// discard tracked a.txt → 恢复 v1
	if n, err := gitDiscard(ctx, dir, []string{"a.txt"}, false); err != nil || n != 1 {
		t.Fatalf("discard 期望 1: n=%d err=%v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "v1\n" {
		t.Fatalf("a.txt 应恢复为 v1，得到 %q", b)
	}

	// discard untracked c.txt（先取消暂存）→ 文件删除
	if _, err := gitUnstage(ctx, dir, []string{"c.txt"}); err != nil {
		t.Fatal(err)
	}
	if n, err := gitDiscard(ctx, dir, []string{"c.txt"}, true); err != nil || n != 1 {
		t.Fatalf("clean 期望 1: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.txt")); !os.IsNotExist(err) {
		t.Fatal("c.txt 应已被删除")
	}

	// commit：先制造并暂存一个变更（discard 后工作区与 HEAD 一致，无可提交）
	w(t, dir, "a.txt", "v3\n")
	if _, err := gitStage(ctx, dir, []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	hash, err := gitCommitWorktree(ctx, dir, "  test commit  \n", false)
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" || len(hash) > 12 {
		t.Fatalf("期望 short hash，得到 %q", hash)
	}
	s, _ = gitSnapshot(ctx, dir)
	if !s.Clean {
		t.Fatalf("提交后应干净: %+v", s.Files)
	}

	// commit stage_all：修改 b + 新增 d，一次提交全部
	w(t, dir, "b.txt", "changed\n")
	w(t, dir, "d.txt", "new\n")
	if _, err := gitCommitWorktree(ctx, dir, "stage all", true); err != nil {
		t.Fatal(err)
	}
	s, _ = gitSnapshot(ctx, dir)
	if !s.Clean || len(s.Files) != 0 {
		t.Fatalf("stage_all 提交后应干净: %+v", s.Files)
	}

	// message 校验
	if _, err := gitCommitWorktree(ctx, dir, "   ", false); err == nil {
		t.Fatal("空 message 应报错")
	}
	if _, err := gitCommitWorktree(ctx, dir, strings.Repeat("x", 2001), false); err == nil {
		t.Fatal("超长 message 应报错")
	}
}

// TestGitUnstageNoHead：无 HEAD（首次提交前）取消暂存 —— 现代 git 优雅处理（等价清空 index），
// 文件应回到未跟踪态而非报错。
func TestGitUnstageNoHead(t *testing.T) {
	dir := gitInit(t)
	w(t, dir, "x.txt", "1\n")
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if n, err := gitUnstage(context.Background(), dir, []string{"x.txt"}); err != nil || n != 1 {
		t.Fatalf("unborn HEAD unstage 应成功: n=%d err=%v", n, err)
	}
	s, err := gitSnapshot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Untracked != 1 || len(s.Files) != 1 || s.Files[0].Staged {
		t.Fatalf("x.txt 应回到未跟踪态: %+v", s.Files)
	}
}

// TestGitBranchOps：分支清单/切换/名校验 + git init 幂等保护
func TestGitBranchOps(t *testing.T) {
	ctx := context.Background()
	dir := gitInit(t)
	w(t, dir, "f.txt", "1\n")
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-qm", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	def := strings.TrimSpace(func() string {
		out, _ := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
		return string(out)
	}())
	if out, err := exec.Command("git", "-C", dir, "checkout", "-qb", "feat").CombinedOutput(); err != nil {
		t.Fatalf("branch: %v\n%s", err, out)
	}

	// 清单：含默认分支与 feat，当前指向 feat
	branches, err := gitBranchList(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	var current string
	for _, b := range branches {
		names[b.Name] = true
		if b.Current {
			current = b.Name
		}
	}
	if !names[def] || !names["feat"] || current != "feat" {
		t.Fatalf("分支清单不符: %+v (def=%s)", branches, def)
	}

	// 切换回默认分支
	if name, err := gitCheckoutBranch(ctx, dir, def); err != nil || name != def {
		t.Fatalf("checkout 期望 %s: %q %v", def, name, err)
	}
	branches, _ = gitBranchList(ctx, dir)
	for _, b := range branches {
		if b.Name == def && !b.Current {
			t.Fatal("切换后默认分支应为 current")
		}
	}

	// 非法分支名拒绝（防 flag 注入 / ref 歧义）
	for _, bad := range []string{"", "-x", ".hidden", "a..b", "x~1", "x y", "tail/", "s.lock", strings.Repeat("a", 257)} {
		if _, err := gitCheckoutBranch(ctx, dir, bad); err == nil {
			t.Errorf("非法分支名 %q 应被拒绝", bad)
		}
	}

	// git init：已是仓库 → 报错；非仓库 → 成功且分支清单可用（unborn 为空）
	if _, err := gitInitRepo(ctx, dir); err == nil {
		t.Fatal("已是仓库时 git init 应报错")
	}
	empty := t.TempDir()
	created, err := gitInitRepo(ctx, empty)
	if err != nil || !created {
		t.Fatalf("非仓库 init 应成功: created=%v err=%v", created, err)
	}
	if bl, err := gitBranchList(ctx, empty); err != nil || len(bl) != 0 {
		t.Fatalf("unborn 仓库分支清单应为空: %v %v", bl, err)
	}
}
