package builtin

// cwd 回执相关不变式：后台命令不改会话 cwd（R5）、重复 cd 不多嘴、
// 越界重置且连续越界只提示一次。
//
// 来源：docs/AGENT_CWD_DESIGN_2026-11-19.md §14.5（R5）/ §15.3（回执）/ §17.5（不变式）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/workdir"
)

// TestBackgroundCommandDoesNotChangeCwd T3：后台命令的 cd 不改会话 cwd（R5）。
func TestBackgroundCommandDoesNotChangeCwd(t *testing.T) {
	root := t.TempDir()
	sub := t.TempDir()
	tool := NewBashTool(root, 0, nil, nil)
	ctx := bashCtx("run-1", "")
	if _, err := tool.Call(ctx, "", `{"command":"cd `+sub+` && sleep 0.01 &"}`); err != nil {
		t.Fatal(err)
	}
	if got := tool.currentCwd(ctx); got != root {
		t.Fatalf("后台命令改了会话 cwd：%q, want %q", got, root)
	}
}

// TestBashCwdRepeatedSameDirNoNotice §17.5：重复 cd 到同一目录 ⇒ **无回执**
// （上下文膨胀的闸门：模型每次命令都 `cd X && …` 时不能每次都多一行）。
func TestBashCwdRepeatedSameDirNoNotice(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd
	ctx := bashCtx("run-1", "")

	out1, err := tool.Call(ctx, "", `{"command":"cd `+sub+` && echo one"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out1, "Working directory is now "+sub) {
		t.Fatalf("首次 cd 应有回执：\n%s", out1)
	}
	out2, err := tool.Call(ctx, "", `{"command":"cd `+sub+` && echo two"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "Working directory") {
		t.Fatalf("重复 cd 到同一目录不该有回执：\n%s", out2)
	}
}

// TestBashCwdOutsideRootResetsToProjectRoot §17.5：越界 ⇒ cwd **真的**回到项目根。
func TestBashCwdOutsideRootResetsToProjectRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "sub")
	outside := t.TempDir()
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd
	ctx := bashCtx("run-1", "")

	if _, err := tool.Call(ctx, "", `{"command":"cd `+inside+`"}`); err != nil {
		t.Fatal(err)
	}
	if got := wd.Current(workdir.MainKey); got != inside {
		t.Fatalf("根内 cd 没生效：%q", got)
	}
	if _, err := tool.Call(ctx, "", `{"command":"cd `+outside+`"}`); err != nil {
		t.Fatal(err)
	}
	if got := wd.Current(workdir.MainKey); got != wd.Root() {
		t.Fatalf("越界后 cwd = %q, want 根 %q", got, wd.Root())
	}
}

// TestBashCwdOutOfBoundsNoticeThrottled §17.5：连续越界只提示一次；
// 成功 cd 到根内目录（真变化）后再越界，仍要提示。
func TestBashCwdOutOfBoundsNoticeThrottled(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "sub")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd
	ctx := bashCtx("run-1", "")

	cmd := `{"command":"cd ` + outside + `"}`
	out1, err := tool.Call(ctx, "", cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out1, "Working directory was reset to "+root) {
		t.Fatalf("第一次越界应提示：\n%s", out1)
	}
	out2, err := tool.Call(ctx, "", cmd)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "Working directory was reset") {
		t.Fatalf("连续越界不该再提示：\n%s", out2)
	}

	// 真变化（根内 cd）之后再越界 ⇒ 重新提示。
	if _, err := tool.Call(ctx, "", `{"command":"cd `+inside+`"}`); err != nil {
		t.Fatal(err)
	}
	out3, err := tool.Call(ctx, "", cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out3, "Working directory was reset to "+root) {
		t.Fatalf("真变化后越界应重新提示：\n%s", out3)
	}
}
