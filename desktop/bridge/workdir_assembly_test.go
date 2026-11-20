package main

// 装配层「同一份 cwd 真相」的结构化断言（tools.WorkdirAware）。
//
// 来源：docs/AGENT_CWD_DESIGN_2026-11-19.md §16.2 D6
// + /private/tmp/patch_s6.py（该脚本的 5 处编辑就是这一条的实施）。
//
// 为什么需要它：run_python 另建 State、文件工具另建 State 这类装配错位**行为层测不出来**
// —— 每个工具单独看都"对"（各自的相对路径都锚在自己的 root 上），只是彼此不一致。
// 只有拿指针比才能证明「bash / 文件工具 / run_python 用的是同一个 State」。
// （§16.4：把 run_python 装配改回 NewRunPythonTool(workspace, …) 时，本断言会红。）

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/workdir"
)

// TestEngineToolsShareOneWorkdirState 同一会话引擎里的 cwd 相关工具共用同一个 State。
func TestEngineToolsShareOneWorkdirState(t *testing.T) {
	m, _ := testManager(t)
	ws := t.TempDir()
	bs, err := m.create(ws, "wd-one-truth", "")
	if err != nil {
		t.Fatalf("create 会话失败: %v", err)
	}
	if bs.wd == nil {
		t.Fatal("会话没有 workdir 状态（装配漏了）")
	}

	type awd interface{ WorkdirState() *workdir.State }
	check := func(name string) {
		tl, err := bs.eng.GetTool(context.Background(), name)
		if err != nil {
			t.Fatalf("引擎没有 %s: %v", name, err)
		}
		w, ok := tl.(awd)
		if !ok {
			t.Fatalf("%s 没有实现 WorkdirState()（tools.WorkdirAware）", name)
		}
		if got := w.WorkdirState(); got != bs.wd {
			t.Fatalf("%s 的 State 不是会话那一份（指针不等 ⇒ 另建了一个，cwd 会各走各的）", name)
		}
	}
	for _, name := range []string{"bash", "read_file", "write_file", "edit_file", "grep", "glob", "run_python"} {
		check(name)
	}

	// 顺带证明 State 的 root 与**会话工作区**一致（否则"越界边界"也锚错了）。
	if got := bs.wd.Root(); filepath.Clean(got) != filepath.Clean(ws) {
		t.Fatalf("State root = %q, want 会话工作区 %q", got, ws)
	}
}
