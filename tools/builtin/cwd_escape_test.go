package builtin

// 「cd 越界 ⇒ 重置回根」与「文件工具不得写出工作区」这两条边界的**组合**测试。
//
// 为什么需要（取代历史 §12.3 的 TestCwdResetOutsideRoot）：
// 那条测试断言的是「文件工具在**越界 cwd** 下被拒」。新语义下 cd 越界当场就把记忆
// 重置回根 ⇒ 越界 cwd 不可达，那条断言已经无法被触发（不是删了它，是它测的状态没了）。
// 不可达的断言 = 永远绿的假保护，所以换成本条**能被证伪**的：
//
//	① cd 到根内子目录后，相对路径真的锚到子目录（前提取到）；
//	② cd 越界后 Current() 立刻回根，相对路径**重新以根为锚**（若重置失效，这里会是
//	   越界目录下的路径，或者直接报错）；
//	③ 即便如此，试图用相对路径逃出根（../escape.txt）仍被拒，且**磁盘上没有**写出文件。
//
// ①+② 钉「重置真的发生了」，③ 钉「重置不是放宽边界的理由」—— 两条都失败过才算证明。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/workdir"
)

// writeToolOf 从文件工具集里取 write_file（直调，绕开引擎的审批层）。
func writeToolOf(t *testing.T, f *FileTools) tools.Tool {
	t.Helper()
	for _, tl := range f.Tools() {
		if tl.Name() == "write_file" {
			return tl
		}
	}
	t.Fatal("文件工具集里没有 write_file")
	return nil
}

// TestCwdResetOutsideRootKeepsFileToolsBounded 越界重置 + 文件工具边界（组合不变量）。
func TestCwdResetOutsideRootKeepsFileToolsBounded(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir() // 根的兄弟目录：真实存在，所以 cd 会成功
	wd := workdir.New(root)
	bash := NewBashToolWithWorkdir(wd, 0, nil, nil)
	ft := NewFileToolsWithWorkdir(wd)
	ctx := bashCtx("run-escape", "")

	// ① 根内 cd ⇒ 相对路径锚到子目录（证明「锚点会跟着 cd 走」这个大前提成立）。
	if _, err := bash.Call(ctx, "", `{"command":"cd sub"}`); err != nil {
		t.Fatal(err)
	}
	if got := wd.Current(workdir.MainKey); got != sub {
		t.Fatalf("根内 cd 后 cwd = %q，want %q", got, sub)
	}
	if p, err := ft.resolveWriteAt(ctx, "a.txt"); err != nil || p != filepath.Join(sub, "a.txt") {
		t.Fatalf("cd 后相对路径应锚到子目录：%q, %v", p, err)
	}

	// ② cd 越界 ⇒ 记忆**立刻**回根（不是等下一条命令才发现）。
	if _, err := bash.Call(ctx, "", `{"command":"cd `+outside+`"}`); err != nil {
		t.Fatal(err)
	}
	if got := wd.Current(workdir.MainKey); got != wd.Root() {
		t.Fatalf("越界后 cwd = %q，want 回到根 %q", got, wd.Root())
	}
	p, err := ft.resolveWriteAt(ctx, "a.txt")
	if err != nil {
		t.Fatalf("越界重置后普通相对路径不该失败：%v", err)
	}
	if p != filepath.Join(root, "a.txt") {
		t.Fatalf("越界重置后相对路径应回到根的锚点：%q，want %q（重置失效会锚在 %q）",
			p, filepath.Join(root, "a.txt"), outside)
	}

	// ③ 逃出根的相对路径仍被拒，且**磁盘上没有**任何越界写入。
	wt := writeToolOf(t, ft)
	if _, err := wt.Call(ctx, "write_file", `{"path":"../escape.txt","content":"x"}`); err == nil {
		t.Fatal("write_file 应拒绝逃出工作区根的相对路径")
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(root), "escape.txt")); statErr == nil {
		t.Fatalf("越界写入居然落盘了：%q", filepath.Join(filepath.Dir(root), "escape.txt"))
	}
	// 反向对照：根内的正常写入照常成功（拒绝的是逃逸，不是所有写）。
	if _, err := wt.Call(ctx, "write_file", `{"path":"ok.txt","content":"ok"}`); err != nil {
		t.Fatalf("根内相对写入不该被拒：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ok.txt")); err != nil {
		t.Fatalf("根内写入没落盘：%v", err)
	}
}
