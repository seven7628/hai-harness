package tools

// engine_tools_test.go：Engine.Tools() 的钉子（Wave 3 装配新增的加性面）。
//
// 这个方法存在的唯一理由：codemode（编排型工具）的目录需要**全部已注册工具**
//（含 deferred/hidden/model-only），而引擎此前没有这个面 —— ToolParams 只回
// 「声明给模型」的档位。它是装配点的输入，故不变量必须钉在引擎这一侧：
//
//   - 不过滤：五档一个不漏（漏 deferred = MCP 工具在脚本里不可达）；
//   - 顺序稳定：按名字典序（描述字节稳定的前提，见 ToolParams 注释）；
//   - 快照语义：返回后注册不影响已取出的切片；
//   - 并发安全：与 RegisterTool 并发跑 -race 不报数据竞争。

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
)

// tierToolOf 造一个指定档位的工具（复用 exposure_test.go 的 tierTool）。
func tierToolOf(name string, exp ToolExposure) *tierTool {
	return &tierTool{BaseTool: BaseTool{Name_: name, Description_: name, Params_: map[string]any{}}, exp: exp}
}

// toolNamesOf 取 Tools() 的名字序列（断言用）。
func toolNamesOf(ts []Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

// TestEngineToolsReturnsEveryTier 五档全回、按名排序、与 ToolParams 的差集就是
// 「模型看不见的那三档」。
func TestEngineToolsReturnsEveryTier(t *testing.T) {
	e := NewToolEngine()
	// 注册顺序刻意打乱（顺序不得影响结果）。d_plain 用**不实现 ExposureProvider** 的
	// stubTool：未实现 = direct（既有工具零迁移），与「实现了但返回空串」（引擎按最保守
	// 的 hidden 处理）是两回事。
	regs := []Tool{
		tierToolOf("z_direct", ExposureDirect),
		tierToolOf("c_hidden", ExposureHidden),
		tierToolOf("a_model_only", ExposureModelOnly),
		tierToolOf("e_deferred", ExposureDeferred),
		tierToolOf("b_codemode", ExposureCodemode),
		&stubTool{BaseTool: BaseTool{Name_: "d_plain", Description_: "d_plain", Params_: map[string]any{}}},
	}
	for _, tl := range regs {
		e.RegisterTool(context.Background(), tl)
	}

	got := toolNamesOf(e.Tools())
	want := []string{"a_model_only", "b_codemode", "c_hidden", "d_plain", "e_deferred", "z_direct"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Tools() = %v, want %v（全部档位 + 字典序）", got, want)
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("Tools() 顺序不稳定: %v", got)
	}

	// 声明给模型的那批走 ToolParams（hidden/codemode/deferred 不在），差的正是编排方
	// 需要而模型不需要的那三档。
	schemas, err := e.ToolParams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, s := range schemas {
		declared[s.Name] = true
	}
	for _, name := range []string{"c_hidden", "b_codemode", "e_deferred"} {
		if declared[name] {
			t.Errorf("%s 不该进 ToolParams（exposure 分层失效）", name)
		}
	}
	for _, name := range []string{"z_direct", "a_model_only", "d_plain"} {
		if !declared[name] {
			t.Errorf("%s 必须进 ToolParams", name)
		}
	}
}

// TestEngineToolsSnapshotIsStable 取出的切片是快照：之后再注册不改变它
// （装配点拿它做一次目录投影，中途注册不该让描述半途变样）。
func TestEngineToolsSnapshotIsStable(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), tierToolOf("first", ExposureDirect))
	snap := e.Tools()
	e.RegisterTool(context.Background(), tierToolOf("second", ExposureDirect))

	if got := toolNamesOf(snap); fmt.Sprint(got) != fmt.Sprint([]string{"first"}) {
		t.Fatalf("快照被后续注册改写: %v", got)
	}
	if got := len(e.Tools()); got != 2 {
		t.Fatalf("重新取表长度 = %d, want 2", got)
	}
}

// TestEngineToolsDedupesReregisteredName 同名重注册（hidden 档靠它撤下工具）不得
// 在清单里出现两条。
func TestEngineToolsDedupesReregisteredName(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), tierToolOf("x", ExposureDirect))
	hidden := tierToolOf("x", ExposureHidden) // 重新注册 = 撤下（引擎无 unregister）
	e.RegisterTool(context.Background(), hidden)

	got := e.Tools()
	if len(got) != 1 || got[0] != Tool(hidden) {
		t.Fatalf("Tools() = %v, want 后注册的那一个（去重 + 后注册胜出）", toolNamesOf(got))
	}
}

// TestEngineToolsConcurrentWithRegister 并发注册与取表（-race 下才是真断言）：
// 每次快照都必须仍是字典序（排序在锁内完成 —— 漏锁会让这里读到半建的切片）。
func TestEngineToolsConcurrentWithRegister(t *testing.T) {
	e := NewToolEngine()
	var wg sync.WaitGroup
	const n = 30
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			e.RegisterTool(context.Background(), tierToolOf(fmt.Sprintf("t%02d", i), ExposureDeferred))
		}(i)
		go func() {
			defer wg.Done()
			for _, name := range toolNamesOf(e.Tools()) {
				_ = name
			}
			if !sort.StringsAreSorted(toolNamesOf(e.Tools())) {
				t.Errorf("并发快照顺序不稳定")
				return
			}
		}()
	}
	wg.Wait()
	if got := len(e.Tools()); got != n {
		t.Fatalf("Tools() 长度 = %d, want %d", got, n)
	}
}
