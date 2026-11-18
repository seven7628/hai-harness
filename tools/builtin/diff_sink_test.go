package builtin

// 产出 diff 的**并发隔离**测试（2026-11-17，第 5 步）。
//
// 问题：write_file / edit_file / run_python 原来把 diff 存在**工具实例字段**上，
// 而工具实例在注册表里共享 —— 主 agent 与后台子 agent（以及 codemode 的嵌套调用）
// 会并发调同一实例 ⇒ 实例字段互相清空/串味 ⇒ 宿主的「+/- 变更」挂到**别的文件**上。
//
// 现在：diff 走 ctx 槽（tools.DiffSink，每次调用独立）⇒ 按构造即无竞态。
//
// 这个测试在 `-race` 下能抓住旧实现的数据竞争（旧实现必然报 race 或串味）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

// TestConcurrentWritesKeepOwnDiff 并发写多个文件 ⇒ 每个调用拿到的 diff 都是**自己**的。
func TestConcurrentWritesKeepOwnDiff(t *testing.T) {
	dir := t.TempDir()
	ft := NewFileTools(dir)
	tool := newWriteFileTool(ft)

	const n = 16
	var wg sync.WaitGroup
	results := make([]*core.FileDiff, n) //nolint:staticcheck // 仅为测试里按索引取回
	errs := make([]error, n)
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		i := i
		paths[i] = filepath.Join(dir, fmt.Sprintf("f%02d.txt", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			sink := &tools.DiffSink{}
			ctx := tools.WithDiffSink(context.Background(), sink)
			_, err := tool.Call(ctx, "write_file", fmt.Sprintf(`{"path":%q,"content":"content-%d"}`, paths[i], i))
			errs[i] = err
			d, _ := sink.Diff()
			results[i] = d
		}()
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个写入失败: %v", i, errs[i])
		}
		if results[i] == nil {
			t.Fatalf("第 %d 个写入没有产出 diff", i)
		}
		if results[i].Path != paths[i] {
			t.Errorf("第 %d 个写入的 diff 串味了：Path=%q，want %q", i, results[i].Path, paths[i])
		}
	}
	// 文件确实写对了（内容没串）
	for i := 0; i < n; i++ {
		b, err := os.ReadFile(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("content-%d", i); string(b) != want {
			t.Errorf("第 %d 个文件内容不对：%q want %q", i, string(b), want)
		}
	}
}

// TestConcurrentEditsKeepOwnDiff 并发改文件 ⇒ diff 不串味。
func TestConcurrentEditsKeepOwnDiff(t *testing.T) {
	dir := t.TempDir()
	ft := NewFileTools(dir)
	tool := newEditFileTool(ft)

	const n = 12
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		paths[i] = filepath.Join(dir, fmt.Sprintf("e%02d.txt", i))
		if err := os.WriteFile(paths[i], []byte(fmt.Sprintf("old-%d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	diffs := make([]*core.FileDiff, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sink := &tools.DiffSink{}
			ctx := tools.WithDiffSink(context.Background(), sink)
			_, err := tool.Call(ctx, "edit_file", fmt.Sprintf(`{"path":%q,"old_string":"old-%d","new_string":"new-%d"}`, paths[i], i, i))
			errs[i] = err
			d, _ := sink.Diff()
			diffs[i] = d
		}()
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个编辑失败: %v", i, errs[i])
		}
		if diffs[i] == nil || diffs[i].Path != paths[i] {
			t.Errorf("第 %d 个编辑的 diff 串味了：%+v", i, diffs[i])
		}
	}
}
