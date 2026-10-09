package main

// Project Memory 的用户可见活动（官方 §2.7）：
//
//	"When you see messages like **'Saved 2 memories'** or **'Recalled 2
//	 memories'** … Claude is actively updating or reading from …/memory/."
//
// 宿主侧只负责报**事实**（哪个文件、读还是写、归谁的记忆），合并成"N memories"
// 是前端的活。理由见 events/memory_activity.go。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// memActivityCtx 造一个带工具上下文的 ctx，让钩子能拿到 Handler 与 RunId。
func memActivityCtx(t *testing.T) (context.Context, *[]*events.MemoryActivity) {
	t.Helper()
	var got []*events.MemoryActivity
	h := events.EventHandler(func(_ context.Context, ev events.Event) {
		if a, ok := ev.(*events.MemoryActivity); ok {
			got = append(got, a)
		}
	})
	ctx := events.WithToolContextForRun(context.Background(), "run-1", "", 0, h, nil, nil)
	return ctx, &got
}

// TestMemoryActivitySavedOnTopicWrite：写记忆 → 一条 saved 事件。
func TestMemoryActivitySavedOnTopicWrite(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	h := newMemoryHost(memory.Config{Directory: filepath.Join(home, "mem")})
	dir, err := h.resolve(context.Background(), ws)
	if err != nil || dir == nil {
		t.Fatalf("解析记忆目录失败: %v", err)
	}
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	topic := filepath.Join(dir.Path, "relay-backoff.md")
	if err := os.WriteFile(topic, []byte("---\ntype: project\n---\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, got := memActivityCtx(t)
	if _, err := h.postWrite(ctx, ws, topic); err != nil {
		t.Fatalf("postWrite: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("应发 1 条活动事件，得到 %d", len(*got))
	}
	a := (*got)[0]
	if a.Action != events.MemoryActionSaved || a.File != "relay-backoff.md" ||
		a.Scope != events.MemoryScopeMain || a.Count != 1 || a.RunId != "run-1" ||
		a.EventType != events.MemoryActivityType {
		t.Fatalf("活动事件读数不对: %+v", a)
	}
}

// TestMemoryActivityRecalledOnTopicRead：读记忆 → 一条 recalled 事件；
// 读索引**不算**（索引是清单，官方没把它当一条记忆）。
func TestMemoryActivityRecalledOnTopicRead(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	h := newMemoryHost(memory.Config{Directory: filepath.Join(home, "mem")})
	dir, _ := h.resolve(context.Background(), ws)
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	topic := filepath.Join(dir.Path, "relay-backoff.md")
	if err := os.WriteFile(topic, []byte("---\ntype: project\n---\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir.Path, memory.IndexFileName),
		[]byte("- relay-backoff.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, got := memActivityCtx(t)
	if _, err := h.postRead(ctx, ws, topic); err != nil {
		t.Fatalf("postRead: %v", err)
	}
	if len(*got) != 1 || (*got)[0].Action != events.MemoryActionRecalled {
		t.Fatalf("读 topic 应发 1 条 recalled，得到 %+v", *got)
	}

	// 读索引不计入：否则刚启动、只加载了索引时就会说 "Recalled N memories"。
	*got = nil
	if _, err := h.postRead(ctx, ws, filepath.Join(dir.Path, memory.IndexFileName)); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 0 {
		t.Fatalf("读索引不该被算成回忆了一条记忆，得到 %+v", *got)
	}
}

// TestMemoryActivityScopeSeparatesSubagent：子 agent 目录的读写标 subagent scope，
// 且主目录同一件仍是 main。
func TestMemoryActivityScopeSeparatesSubagent(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	h := newMemoryHost(memory.Config{Directory: filepath.Join(home, "mem")})
	main, _ := h.resolve(context.Background(), ws)
	sub, ok := h.cfg.SubagentDir(*main, "docs-writer")
	if !ok {
		t.Fatal("SubagentDir 应当成功")
	}
	for _, d := range []memory.Dir{*main, sub} {
		if err := os.MkdirAll(d.Path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	ctx, got := memActivityCtx(t)
	body := "---\ntype: project\n---\n\nbody\n"
	for _, p := range []string{
		filepath.Join(main.Path, "m.md"),
		filepath.Join(sub.Path, "s.md"),
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := h.postWrite(ctx, ws, p); err != nil {
			t.Fatalf("postWrite %s: %v", p, err)
		}
	}
	if len(*got) != 2 {
		t.Fatalf("应发 2 条，得到 %d", len(*got))
	}
	scopeOf := map[string]events.MemoryScope{}
	for _, a := range *got {
		scopeOf[a.File] = a.Scope
	}
	if scopeOf["m.md"] != events.MemoryScopeMain {
		t.Fatalf("主目录文件应是 main scope: %+v", scopeOf)
	}
	if scopeOf["s.md"] != events.MemoryScopeSubagent {
		t.Fatalf("子 agent 文件应是 subagent scope: %+v", scopeOf)
	}
}

// TestMemoryActivityNoEventWithoutToolContext：拿不到工具上下文就**静默跳过**。
//
// 没有 RunId 就没有可关联的运行，发出去的是孤儿事件；而它是纯可观测性事件，
// 不该让读写路径失败。
func TestMemoryActivityNoEventWithoutToolContext(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	h := newMemoryHost(memory.Config{Directory: filepath.Join(home, "mem")})
	dir, _ := h.resolve(context.Background(), ws)
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	topic := filepath.Join(dir.Path, "x.md")
	if err := os.WriteFile(topic, []byte("---\ntype: project\n---\n\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 裸 context.Background()：没有 ToolContext。不得 panic，也不得让写入失败。
	if _, err := h.postWrite(context.Background(), ws, topic); err != nil {
		t.Fatalf("无工具上下文时不该报错: %v", err)
	}
	if _, err := h.postRead(context.Background(), ws, topic); err != nil {
		t.Fatalf("无工具上下文时不该报错: %v", err)
	}
}

// 端到端：read_file 真的触发 postRead（钩子接线没漏）。
func TestReadFileTriggersMemoryActivity(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	h := newMemoryHost(memory.Config{Directory: filepath.Join(home, "mem")})
	dir, _ := h.resolve(context.Background(), ws)
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	topic := filepath.Join(dir.Path, "readme-topic.md")
	if err := os.WriteFile(topic, []byte("---\ntype: project\n---\n\n记住了什么\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var got []*events.MemoryActivity
	handler := events.EventHandler(func(_ context.Context, ev events.Event) {
		if a, ok := ev.(*events.MemoryActivity); ok {
			got = append(got, a)
		}
	})
	ft := builtin.NewFileTools(ws).WithPostRead(func(ctx context.Context, p string) (string, error) {
		return h.postRead(ctx, ws, p)
	})
	for _, tl := range ft.Tools() {
		if tl.Name() != "read_file" {
			continue
		}
		ctx := events.WithToolContextForRun(context.Background(), "run-e2e", "", 0, handler, nil, nil)
		args := `{"path":` + strconv.Quote(topic) + `}`
		if err := tl.ValidParams(ctx, tl.Name(), args); err != nil {
			t.Fatal(err)
		}
		if _, err := tl.Call(ctx, tl.Name(), args); err != nil {
			t.Fatalf("read_file: %v", err)
		}
	}
	if len(got) != 1 || got[0].Action != events.MemoryActionRecalled {
		t.Fatalf("read_file 应触发 recalled 事件，得到 %+v", got)
	}
}
