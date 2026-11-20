package builtin

// run_python 的 **cwd 可见性**测试（2026-11-20 收尾项）。
//
// 为什么要补：run_python 的脚本工作目录**同时是** outputs / script_path 的相对路径
// 锚点，而锚点会随 bash 的 cd 变 —— 「产物落在哪」对模型就成了一条隐式信息。
// 改造前描述里还写着 "workspace-relative"（实际锚点是 cwd，cd 之后就是错的）。
//
// 处置（与 bash 的 D' 载体同款，见 §15.2）：把当前工作目录写进**每请求重算**的工具
// 描述，而不是在结果里加一行一次性回执 —— 描述每轮都在（压缩后仍在、恢复后重算），
// 回执只在「声明了 outputs 的那一次」出现，覆盖不到「模型没声明产物却在算路径」。
//
// 断言两条：
//  1. 描述里的 cwd 按**调用方分区**取（主/子 agent 各自正确）；
//  2. 描述说的锚点是真的 —— declaredOutputs 把相对 outputs 解析到同一个目录。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/workdir"
)

// TestRunPythonDescribeIncludesWorkingDirPerCaller 描述带 cwd，且按分区取值。
func TestRunPythonDescribeIncludesWorkingDirPerCaller(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	wd.Set("task-py", sub)
	tool := NewRunPythonToolWithWorkdir(wd, nil, nil)

	mainDesc := tool.DescribeFor(workdir.WithKey(context.Background(), workdir.MainKey))
	if !strings.Contains(mainDesc, "Working directory: "+root) {
		t.Fatalf("主 agent 的描述里没有工作区根：\n%s", mainDesc)
	}
	subDesc := tool.DescribeFor(workdir.WithKey(context.Background(), "task-py"))
	if !strings.Contains(subDesc, "Working directory: "+sub) {
		t.Fatalf("子 agent 的描述应带**自己的** cwd：\n%s", subDesc)
	}
	// 静态版（无调用方上下文）回退到构造目录 = 工作区根，绝不空着。
	if got := tool.Description(); !strings.Contains(got, "Working directory: "+root) {
		t.Fatalf("静态描述应带回退目录：\n%s", got)
	}
}

// TestRunPythonOutputsAnchorAtCwd outputs 的相对锚点 = 会话 cwd（与描述一致）。
//
// 这条钉的是「描述没说谎」：描述说锚点是当前工作目录，解析必须真是它。
// 改造前描述写 "workspace-relative"（假话），代码一直锚 cwd。
func TestRunPythonOutputsAnchorAtCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tool := NewRunPythonToolWithWorkdir(wd, nil, nil)
	ctx := workdir.WithKey(context.Background(), workdir.MainKey)

	// cd 之前：锚在工作区根
	outs := tool.declaredOutputs(ctx, []string{"out/report.xlsx"})
	if len(outs) != 1 || outs[0].abs != filepath.Join(root, "out", "report.xlsx") {
		t.Fatalf("cd 前 outputs 锚点错：%+v", outs)
	}

	// cd 之后：锚点跟着走（与描述里的 "Working directory: <sub>" 同源）
	wd.Set(workdir.MainKey, sub)
	outs = tool.declaredOutputs(ctx, []string{"out/report.xlsx"})
	if len(outs) != 1 || outs[0].abs != filepath.Join(sub, "out", "report.xlsx") {
		t.Fatalf("cd 后 outputs 锚点没跟着 cwd：%+v", outs)
	}

	// 绝对路径不受 cwd 影响（与文件工具同口径）
	abs := filepath.Join(root, "abs.csv")
	outs = tool.declaredOutputs(ctx, []string{abs})
	if len(outs) != 1 || outs[0].abs != abs {
		t.Fatalf("绝对路径不该被 cwd 重锚：%+v", outs)
	}
}
