package builtin

// 会话级 cwd 状态（workdir.State）与工具层的接合测试。
//
// 问题（改造前）："我现在在哪个目录"有三份 —— bash 记自己的 cd，文件工具恒定锚
// 工作区根，run_python 恒定锚工作区根 ⇒ 模型 cd 之后三个工具给出三个答案。
//
// 现在：三者共用**同一个** workdir.State ⇒ 同一份真相；且按**分区**隔离
// （子 agent 的 cd 落在自己的分区，不改主 agent 的）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/workdir"
)

// wdCtx 造一个带 ToolContext 的 ctx（taskID 区分调用方 ⇒ 区分分区）。
func wdCtx(runID, taskID string) context.Context {
	return events.WithToolContextForRunTask(context.Background(), runID, runID, taskID, 0, nil, nil, nil)
}

// TestFileToolsFollowSessionCwd 文件工具的相对路径锚到**会话当前目录**。
//
// 没 cd 时与改造前逐字一致（锚工作区根）；cd 之后跟着走 —— 这正是"三份 cwd
// 合成一份"要的效果。
func TestFileToolsFollowSessionCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	f := NewFileToolsWithWorkdir(wd)

	// 未 cd：锚工作区根
	p, err := f.resolveAt(context.Background(), "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(root, "a.txt") {
		t.Fatalf("未 cd 时应锚工作区根：%q，want %q", p, filepath.Join(root, "a.txt"))
	}

	// cd 之后：锚点跟随
	wd.Set(workdir.MainKey, sub)
	p, err = f.resolveAt(context.Background(), "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(sub, "a.txt") {
		t.Fatalf("cd 后应锚当前目录：%q，want %q", p, filepath.Join(sub, "a.txt"))
	}
}

// TestReadFileToolFollowsSessionCwd 端到端：write_file 写在哪，read_file 就从哪读。
//
// 这是"同一份真相"的**可观测**形式：模型在 sub/ 里写的文件，随后用相对路径
// 就能读回来（改造前读回来的是"文件不存在"）。
func TestReadFileToolFollowsSessionCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	wd.Set(workdir.MainKey, sub)
	f := NewFileToolsWithWorkdir(wd)

	writeTool, readTool := wdFileTools(t, f)
	args := `{"path":"a.txt","content":"hello-workdir\n"}`
	if _, err := writeTool.Call(context.Background(), "write_file", args); err != nil {
		t.Fatalf("write_file 失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "a.txt")); err != nil {
		t.Fatalf("文件应落在会话当前目录：%v", err)
	}
	out, err := readTool.Call(context.Background(), "read_file", `{"path":"a.txt"}`)
	if err != nil {
		t.Fatalf("read_file 失败：%v", err)
	}
	if !strings.Contains(out, "hello-workdir") {
		t.Fatalf("read_file 应读到刚写的文件：%q", out)
	}
}

// TestBashCdUpdatesSharedWorkdir bash 的 cd 落到**共享**的状态里 ⇒
// 文件工具立刻看到同一个目录（改造前两者各记各的）。
func TestBashCdUpdatesSharedWorkdir(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	f := NewFileToolsWithWorkdir(wd)

	bash := wdBashTool(t, f)
	ctx := context.Background()
	if _, err := bash.Call(ctx, "", `{"command":"cd `+sub+`"}`); err != nil {
		t.Fatalf("bash cd 失败：%v", err)
	}
	p, err := f.resolveAt(ctx, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(sub, "a.txt") {
		t.Fatalf("bash 的 cd 应被文件工具看到：%q，want %q", p, filepath.Join(sub, "a.txt"))
	}
}

// TestWorkdirPartitionsAreIsolated 分区隔离：子 agent 的 cd 不影响主 agent。
//
// 场景就是生产里的那条：explore 子 agent 与主会话**共用同一个 workdir.State**
// ⇒ 共用的是"状态容器"，不是"目录" —— 各自 cd 互不干扰。
func TestWorkdirPartitionsAreIsolated(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	f := NewFileToolsWithWorkdir(wd)

	subCtx := wdCtx("run-sub", "task-123")
	wd.Set("task-123", sub)

	// 主 agent（无 task 身份）仍在工作区根。
	p, err := f.resolveAt(context.Background(), "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(root, "a.txt") {
		t.Fatalf("子 agent 的 cd 影响了主 agent：%q，want %q", p, filepath.Join(root, "a.txt"))
	}
	// 子 agent 在自己的分区。
	p, err = f.resolveAt(subCtx, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(sub, "a.txt") {
		t.Fatalf("子 agent 应在自己的分区：%q，want %q", p, filepath.Join(sub, "a.txt"))
	}
}

// TestRunPythonCwdFollowsWorkdir 脚本工作目录也跟同一份状态
// （否则脚本产物会落在根上，与 bash / 文件工具对不上账）。
func TestRunPythonCwdFollowsWorkdir(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tp := NewRunPythonToolWithWorkdir(wd, nil, nil)
	wd.Set(workdir.MainKey, sub)
	if got := tp.cwd(context.Background()); got != sub {
		t.Fatalf("脚本 cwd = %q，want %q", got, sub)
	}
}

// TestGitWorktreeUsesWorkspaceRoot worktree 工具只取**工作区根**（worktree 统一
// 放在 {workspace}/.go-code/worktrees，是工作区级约定，不随 cd 变）。
func TestGitWorktreeUsesWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	wd := workdir.New(root)
	wt := NewGitWorktreeToolWithWorkdir(wd)
	if wt.root != root {
		t.Fatalf("worktree 根 = %q，want %q", wt.root, root)
	}
}

// wdFileTools 从工具集里取 write_file / read_file。
func wdFileTools(t *testing.T, f *FileTools) (write, read tools.Tool) {
	t.Helper()
	for _, tl := range f.Tools() {
		switch tl.Name() {
		case "write_file":
			write = tl
		case "read_file":
			read = tl
		}
	}
	if write == nil || read == nil {
		t.Fatal("工具集里没有 write_file / read_file")
	}
	return write, read
}

// wdBashTool 从工具集里取 bash 工具。
func wdBashTool(t *testing.T, f *FileTools) *BashTool {
	t.Helper()
	for _, tl := range f.Tools() {
		if b, ok := tl.(*BashTool); ok {
			return b
		}
	}
	t.Fatal("工具集里没有 bash")
	return nil
}
