package main

// 多 agent **并发**下的 cwd 分区隔离（装配链集成测试）。
//
// 为什么必须在这个包、拿真引擎测：单元级（tools/builtin 的 TestBashCwdIsolatedPerCaller）
// 只证明「工具内部按 key 分区」这一件事。真正会出问题的是**装配**——
// 会话只装一套 tools.Engine、工具实例共享（见 workdir 包注释与 tools.WorkdirAware），
// 所以「两个 caller 各自的相对路径锚点 / schema 里的 cwd 描述」是否真的各走各的，
// 只有走 bs.eng 这条真实链路才能证明。
//
// 链路对齐生产：
//   - ctx 用 workdir.WithKey 标分区键 —— 与 agents.runTools 里
//     `events.WithToolContextForRunTask(..., taskID, ...)` + workdir.WithKey 同一套口径；
//   - schema 用 bs.eng.ToolParams(ctx) —— agents.toolSchemas 内部就是它；
//   - 文件工具 / bash 都从同一个引擎取（共享实例 + 共享 workdir.State）。
//
// 三个 caller 并发跑：
//   - task-aaa / task-bbb：两个「子 agent」分区，各 cd 到自己的工作目录；
//   - 主分区（workdir.MainKey）：全程不 cd，断言**始终**在根（它是被隔离噪音的一方）。
//
// 断言（每个 caller 在自己的 cwd 下）：
//  1. bash 的 `pwd` 输出 = 自己的目录（真执行，不是只读状态）；
//  2. read_file / write_file 的相对路径落在自己的目录（互不串门）；
//  3. ToolParams 里 bash 与 run_python 的 "Working directory: …" 是自己的目录；
//  4. 别人的分区没被自己改（收尾统一对照）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/workdir"
)

// TestCwdIsolationAcrossConcurrentCallers 两个不同 workKey 的并发运行各自 cd、各自锚相对路径。
func TestCwdIsolationAcrossConcurrentCallers(t *testing.T) {
	m, _ := testManager(t)
	ws := t.TempDir()
	bs, err := m.create(ws, "cwd-parallel", "")
	if err != nil {
		t.Fatalf("create 会话失败: %v", err)
	}

	partitions := []struct {
		key  string // cwd 分区键（生产里 = 子 agent 的 task id）
		dir  string // 该分区要 cd 进去的目录（相对工作区根）
		name string
	}{
		{key: "task-aaa", dir: "agent-a", name: "A"},
		{key: "task-bbb", dir: "agent-b", name: "B"},
	}
	for _, p := range partitions {
		if err := os.MkdirAll(filepath.Join(ws, p.dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// tool 缓存每个 caller 自己的引擎工具实例（引擎装配一次，工具实例共享）。
	getTool := func(ctx context.Context, name string) (interface {
		Call(context.Context, string, string) (string, error)
	}, error) {
		tl, err := bs.eng.GetTool(ctx, name)
		if err != nil {
			return nil, err
		}
		return tl, nil
	}

	runPartition := func(p struct {
		key  string
		dir  string
		name string
	}) error {
		ctx := workdir.WithKey(context.Background(), p.key)
		want := filepath.Join(ws, p.dir)

		// ① cd 到自己目录（相对路径：锚点 = 分区当前目录 = 根）。
		bash, err := getTool(ctx, "bash")
		if err != nil {
			return err
		}
		out, err := bash.Call(ctx, "bash", `{"command":"cd `+p.dir+` && pwd"}`)
		if err != nil {
			return fmt.Errorf("%s cd 失败: %w", p.name, err)
		}
		if got := strings.TrimSpace(strings.Split(out, "\n")[0]); !sameDir(got, want) {
			return fmt.Errorf("%s 的 bash pwd = %q，want %q", p.name, got, want)
		}
		if got := bs.wd.Current(p.key); got != want {
			return fmt.Errorf("%s 分区 cwd = %q，want %q", p.name, got, want)
		}

		// ② 后续调用（不带 cd）仍在自己的目录 + 文件工具相对路径锚到这里。
		for i := 0; i < 4; i++ {
			out, err := bash.Call(ctx, "bash", `{"command":"pwd"}`)
			if err != nil {
				return fmt.Errorf("%s 第 %d 轮 pwd 失败: %w", p.name, i, err)
			}
			if got := strings.TrimSpace(strings.Split(out, "\n")[0]); !sameDir(got, want) {
				return fmt.Errorf("%s 第 %d 轮 pwd = %q（cwd 被别的分区改走了？）want %q", p.name, i, got, want)
			}

			file := fmt.Sprintf("note-%d.txt", i)
			body := p.name + "-" + file
			wt, err := getTool(ctx, "write_file")
			if err != nil {
				return err
			}
			if _, err := wt.Call(ctx, "write_file", `{"path":"`+file+`","content":"`+body+`"}`); err != nil {
				return fmt.Errorf("%s 写 %s 失败: %w", p.name, file, err)
			}
			// 落在自己的目录，而不是工作区根（也不是别人的目录）。
			got, err := os.ReadFile(filepath.Join(want, file))
			if err != nil {
				return fmt.Errorf("%s 的相对路径没锚到自己的 cwd（%v）", p.name, err)
			}
			if string(got) != body {
				return fmt.Errorf("%s 读回了别人的内容: %q", p.name, got)
			}
			if _, err := os.Stat(filepath.Join(ws, file)); !os.IsNotExist(err) {
				return fmt.Errorf("%s 的文件写到了工作区根（相对路径没跟 cwd 走）", p.name)
			}

			// ③ schema 里的 cwd 描述也必须是自己的（agents.toolSchemas 走这条路）。
			schemas, err := bs.eng.ToolParams(ctx)
			if err != nil {
				return err
			}
			for _, tool := range []string{"bash", "run_python"} {
				desc, ok := schemaDesc(schemas, tool)
				if !ok {
					return fmt.Errorf("ToolParams 里没有 %s", tool)
				}
				if !strings.Contains(desc, "Working directory: "+want) {
					return fmt.Errorf("%s 的 %s 描述 cwd 不对（want %q）：%s", p.name, tool, want, descFirstLine(desc))
				}
			}
		}
		return nil
	}

	// 并发跑两个分区（真并发：共享引擎 + 共享 State，正是会互相踩的那种形态）。
	var wg sync.WaitGroup
	errCh := make(chan error, len(partitions))
	start := make(chan struct{})
	for _, p := range partitions {
		wg.Add(1)
		go func(p struct {
			key  string
			dir  string
			name string
		}) {
			defer wg.Done()
			<-start
			errCh <- runPartition(p)
		}(p)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Error(err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// ④ 收尾对照：谁的目录是谁的，主分区自始至终在根（没被任何子 agent 带跑）。
	for _, p := range partitions {
		if got := bs.wd.Current(p.key); got != filepath.Join(ws, p.dir) {
			t.Errorf("%s 分区最终 cwd = %q，want %q", p.name, got, filepath.Join(ws, p.dir))
		}
	}
	if got := bs.wd.Current(workdir.MainKey); got != ws {
		t.Errorf("主分区 cwd = %q，want 工作区根 %q（子 agent 的 cd 漏进了主分区）", got, ws)
	}
	// 主分区的文件工具仍锚根（它从没 cd 过）。
	mainCtx := workdir.WithKey(context.Background(), workdir.MainKey)
	rt, err := bs.eng.GetTool(mainCtx, "read_file")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Call(mainCtx, "read_file", `{"path":"agent-a/note-0.txt"}`); err != nil {
		t.Fatalf("主分区读 agent-a/note-0.txt 失败（相对根应能读到）: %v", err)
	}
}

// schemaDesc 从 ToolParams 结果里取某工具的 schema 描述。
func schemaDesc(schemas []core.ToolSchema, name string) (string, bool) {
	for _, s := range schemas {
		if s.Name == name {
			return s.Description, true
		}
	}
	return "", false
}

// sameDir 比较两个目录是否同一处（macOS 的 /tmp 是 /private/tmp 的软链接，
// shell 的 pwd 给物理路径，而 workdir 记的是词法路径）。
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	return ea == nil && eb == nil && ra == rb
}

// descFirstLine 只回描述首行（错误信息不刷屏）。
func descFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
