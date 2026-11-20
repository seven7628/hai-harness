package main

// 会话级 cwd 状态（workdir.State）的**会话级**不变量测试。
//
// 为什么这条值得钉死：引擎（连同工具集）会随 **rebuildLoop** 重建（切模型 /
// 切人格 / 切 provider）。状态若在引擎里，切一次模型就丢 cwd（模型于是又开始
// 每次 `cd X && ...`）。所以 wd 挂在**会话**上（bridgeSession.wd），重建时透传给
// 新引擎 ⇒ 跨重建存活。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/workdir"
)

// TestSessionCwdSurvivesRebuildLoop cwd 跨 loop 重建存活，且新引擎用的是**同一个**
// 状态（相对路径锚点跟着走）。
func TestSessionCwdSurvivesRebuildLoop(t *testing.T) {
	m, _ := testManager(t)
	ws := t.TempDir()
	bs, err := m.create(ws, "cwd-survive", "")
	if err != nil {
		t.Fatalf("create 会话失败: %v", err)
	}
	sub := filepath.Join(ws, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// 模型 cd 到子目录（会话级状态）。
	bs.wd.Set(workdir.MainKey, sub)

	// 重建 loop（生产路径：switch_model / switch_persona / set_provider 都走它）。
	m.rebuildLoop(bs)

	// 1) 状态跨重建存活：切模型不该把工作目录清零。
	if got := bs.wd.Current(workdir.MainKey); got != sub {
		t.Fatalf("重建后 cwd = %q，want %q（切模型不该把工作目录清零）", got, sub)
	}

	// 2) 新引擎的文件工具用的是**同一个** State ⇒ 相对路径锚在 sub。
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("session-cwd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := bs.eng.GetTool(context.Background(), "read_file")
	if err != nil {
		t.Fatalf("新引擎没有 read_file: %v", err)
	}
	out, err := rt.Call(context.Background(), "read_file", `{"path":"a.txt"}`)
	if err != nil {
		t.Fatalf("read_file 的相对路径应锚在会话 cwd: %v", err)
	}
	if !strings.Contains(out, "session-cwd") {
		t.Fatalf("read_file 没读到会话 cwd 下的文件: %q", out)
	}
}

// TestSessionCwdSurvivesRebuildLoopPerPartition 重建 loop 后**每个 workKey** 的 cwd 都还在，
// 且新引擎用的是**同一个** State（Q4 = 保留；历史 §12.5 的 V4）。
//
// 与上面那条的分工：TestSessionCwdSurvivesRebuildLoop 钉「主分区 + 新引擎文件工具跟着走」；
// 本条补的是**多分区**维度 —— rebuildLoop 若误传 wd.Root() 新建一个 State、或只搬迁主分区，
// 上面那条仍绿（主分区看起来对），只有分区并存时才暴露。
func TestSessionCwdSurvivesRebuildLoopPerPartition(t *testing.T) {
	m, _ := testManager(t)
	ws := t.TempDir()
	bs, err := m.create(ws, "cwd-survive-parts", "")
	if err != nil {
		t.Fatalf("create 会话失败: %v", err)
	}
	mainDir := filepath.Join(ws, "main-dir")
	childDir := filepath.Join(ws, "child-dir")
	for _, d := range []string{mainDir, childDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const childKey = "task-child-1"
	bs.wd.Set(workdir.MainKey, mainDir)
	bs.wd.Set(childKey, childDir)
	wdBefore := bs.wd

	// 生产重建路径：switch_model / switch_persona / set_provider 都走它。
	m.rebuildLoop(bs)

	if bs.wd != wdBefore {
		t.Fatal("重建后会话换了 State（cwd 会随下一次重建清零）")
	}
	if got := bs.wd.Current(workdir.MainKey); got != mainDir {
		t.Fatalf("重建后主分区 cwd = %q，want %q", got, mainDir)
	}
	if got := bs.wd.Current(childKey); got != childDir {
		t.Fatalf("重建后子分区 cwd = %q，want %q（分区不该被重建搬迁或合并）", got, childDir)
	}

	// 新引擎的工具必须还是那一份 State（指针），schema 里的 cwd 也按分区正确。
	type awd interface{ WorkdirState() *workdir.State }
	for _, name := range []string{"bash", "read_file", "write_file", "run_python"} {
		tl, err := bs.eng.GetTool(context.Background(), name)
		if err != nil {
			t.Fatalf("新引擎没有 %s: %v", name, err)
		}
		if w, ok := tl.(awd); !ok || w.WorkdirState() != wdBefore {
			t.Fatalf("重建后 %s 用的不是会话那份 State", name)
		}
	}
	for _, tc := range []struct{ key, want string }{
		{workdir.MainKey, mainDir},
		{childKey, childDir},
	} {
		schemas, err := bs.eng.ToolParams(workdir.WithKey(context.Background(), tc.key))
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, s := range schemas {
			if s.Name != "bash" {
				continue
			}
			found = true
			if !strings.Contains(s.Description, "Working directory: "+tc.want) {
				t.Fatalf("key=%s 的 bash 描述 cwd 不对（want %q）：%s", tc.key, tc.want, s.Description)
			}
		}
		if !found {
			t.Fatal("schema 里没有 bash")
		}
	}
}
