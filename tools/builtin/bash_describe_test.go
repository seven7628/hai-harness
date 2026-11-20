package builtin

// bash 的**模型可见 cwd 载体**测试（D' 工具描述 + B 工具结果回执）。
//
// 来源：docs/AGENT_CWD_DESIGN_2026-11-19.md §12.4（v3 改写，取代原「环境块注入」测试）
// + /private/tmp/patch_cwd_v3b.py 的 §12.4 代码块。
//
// 为什么载体是这两条（而不是每轮插一条环境消息）：
//   - 工具描述每请求重算（tools/engine.go ToolParams）⇒ 压缩/会话恢复后都还在；
//   - 工具结果回执进 history ⇒ 位置最显著（模型刚执行完就看见）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/workdir"
)

// TestDescribeForIncludesWorkingDir 工具描述里**固定输出当前工作目录**（D'）。
func TestDescribeForIncludesWorkingDir(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd

	ctx := workdir.WithKey(context.Background(), workdir.MainKey)
	desc := tool.DescribeFor(ctx)
	if !strings.Contains(desc, "Working directory: "+root) {
		t.Fatalf("描述里没有当前目录：\n%s", desc)
	}

	// cd 之后，描述要跟着变（每请求重算）
	wd.Set(workdir.MainKey, sub)
	desc2 := tool.DescribeFor(ctx)
	if !strings.Contains(desc2, "Working directory: "+sub) {
		t.Fatalf("cd 后描述没更新：\n%s", desc2)
	}
}

// TestDescribeForPerCaller 子 agent 拿到**自己的** cwd（不是主 agent 的）。
func TestDescribeForPerCaller(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd

	wd.Set(workdir.MainKey, root)
	wd.Set("task-1", sub)
	mainDesc := tool.DescribeFor(workdir.WithKey(context.Background(), workdir.MainKey))
	subDesc := tool.DescribeFor(workdir.WithKey(context.Background(), "task-1"))
	if !strings.Contains(mainDesc, "Working directory: "+root) {
		t.Fatalf("主 agent 描述错：\n%s", mainDesc)
	}
	if !strings.Contains(subDesc, "Working directory: "+sub) {
		t.Fatalf("子 agent 描述错（应为自己的 cwd）：\n%s", subDesc)
	}
}

// TestBashResultCarriesCwdNotice cd 生效时，**工具结果**里回执新目录（B）。
func TestBashResultCarriesCwdNotice(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd
	ctx := bashCtx("run-1", "")

	out, err := tool.Call(ctx, "", `{"command":"cd `+sub+` && echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Working directory is now "+sub) {
		t.Fatalf("结果里没有 cwd 回执：\n%s", out)
	}
	if got := wd.Current(workdir.MainKey); got != sub {
		t.Fatalf("cwd 没写回会话状态：%q, want %q", got, sub)
	}
}

// TestBashResultCarriesResetNotice 越界 ⇒ 回执重置（Claude Code 同款语义）。
func TestBashResultCarriesResetNotice(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd
	ctx := bashCtx("run-1", "")

	out, err := tool.Call(ctx, "", `{"command":"cd `+outside+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Working directory was reset to "+root) {
		t.Fatalf("结果里没有重置回执：\n%s", out)
	}
	if got := wd.Current(workdir.MainKey); got != root {
		t.Fatalf("越界后 cwd 没回到根：%q, want %q", got, root)
	}
}
