package tools

// exposure_test.go：Phase 2 接口冻结的钉子（exposure 分层 / loadout 钩子 /
// skip-truncate / 结构化结果）。这四件事是 codemode 工具与 bash 结构化输出赖以
// 落地的前提，因此契约一旦冻结就要有回归，不能只写在 godoc 里。

import (
	"context"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// ---- exposure 分层：callable ≠ declared ----

type tierTool struct {
	BaseTool
	exp ToolExposure
}

func (t *tierTool) Call(context.Context, string, string) (string, error) { return "ok", nil }
func (t *tierTool) ValidParams(context.Context, string, string) error    { return nil }
func (t *tierTool) Exposure() ToolExposure                               { return t.exp }

func TestExposureGatesDeclaredAndCallable(t *testing.T) {
	mk := func(name string, exp ToolExposure) *tierTool {
		return &tierTool{BaseTool: BaseTool{Name_: name, Description_: name, Params_: map[string]any{}}, exp: exp}
	}
	cases := []struct {
		name                      string
		tool                      Tool
		declared, callable, inbox bool
	}{
		{"direct", mk("a", ExposureDirect), true, true, true},
		{"model-only", mk("b", ExposureModelOnly), true, false, false},
		{"codemode", mk("c", ExposureCodemode), false, true, true},
		{"deferred", mk("d", ExposureDeferred), false, true, false},
		{"hidden", mk("e", ExposureHidden), false, false, false},
		{"未实现接口 = direct（零迁移）", &stubTool{BaseTool: BaseTool{Name_: "f", Description_: "f", Params_: map[string]any{}}}, true, true, true},
		{"非法档位按 hidden 处理", mk("g", ToolExposure("typo")), false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeclaredToModel(c.tool); got != c.declared {
				t.Errorf("DeclaredToModel = %v, want %v", got, c.declared)
			}
			if got := CallableFromScript(c.tool); got != c.callable {
				t.Errorf("CallableFromScript = %v, want %v", got, c.callable)
			}
			if got := ListedInCodemodeCatalog(c.tool); got != c.inbox {
				t.Errorf("ListedInCodemodeCatalog = %v, want %v", got, c.inbox)
			}
		})
	}
}

// TestToolParamsOmitsNonDeclaredTiers ToolParams（= 模型看到的工具表）不得出现
// codemode/deferred/hidden 档，且顺序字节稳定（provider 侧缓存对抖动极敏感）。
func TestToolParamsOmitsNonDeclaredTiers(t *testing.T) {
	e := NewToolEngine()
	for name, exp := range map[string]ToolExposure{
		"z_direct": ExposureDirect, "a_model_only": ExposureModelOnly,
		"m_codemode": ExposureCodemode, "b_deferred": ExposureDeferred, "c_hidden": ExposureHidden,
	} {
		e.RegisterTool(context.Background(), &tierTool{
			BaseTool: BaseTool{Name_: name, Description_: name, Params_: map[string]any{}}, exp: exp,
		})
	}
	first, err := e.ToolParams(context.Background())
	if err != nil {
		t.Fatalf("ToolParams: %v", err)
	}
	var names []string
	for _, s := range first {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "a_model_only,z_direct" {
		t.Fatalf("声明集 = %v, want [a_model_only z_direct]（仅 direct + model-only，按名排序）", names)
	}
	second, _ := e.ToolParams(context.Background())
	if len(first) != len(second) || first[0].Name != second[0].Name || first[1].Name != second[1].Name {
		t.Fatalf("ToolParams 两次调用不稳定: %v vs %v", names, second)
	}
}

// ---- LoadoutProvider：描述改写钩子 ----

// loadoutTool 把已声明工具的 Description 追加一段（mode:"on" 的最小形态）。
type loadoutTool struct {
	tierTool
	seen []string
}

func (l *loadoutTool) PrepareLoadout(_ context.Context, declared []core.ToolSchema) []core.ToolSchema {
	out := make([]core.ToolSchema, 0, len(declared))
	for _, s := range declared {
		l.seen = append(l.seen, s.Name)
		s.Description += " (also callable from a codemode script)"
		out = append(out, s)
	}
	return out
}

func TestLoadoutProviderRewritesDeclaredDescriptions(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &stubTool{
		BaseTool: BaseTool{Name_: "read_file", Description_: "Read a file", Params_: map[string]any{}},
	})
	// hidden 档不该出现在声明集里（也不该被钩子看到）。
	e.RegisterTool(context.Background(), &tierTool{
		BaseTool: BaseTool{Name_: "secret_tool", Description_: "hidden", Params_: map[string]any{}},
		exp:      ExposureHidden,
	})
	// 编排工具自身：model-only = 声明给模型但不可被编排（禁自嵌套），
	// 所以它**在**声明集里 —— 这正是 pi 给自己的档位。
	lo := &loadoutTool{tierTool: tierTool{
		BaseTool: BaseTool{Name_: "codemode", Description_: "Run JS", Params_: map[string]any{}},
		exp:      ExposureModelOnly,
	}}
	e.RegisterTool(context.Background(), lo)

	got, err := e.ToolParams(context.Background())
	if err != nil {
		t.Fatalf("ToolParams: %v", err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
		if !strings.Contains(s.Description, "codemode script") {
			t.Fatalf("%s 的 Description 未被 loadout 钩子改写: %q", s.Name, s.Description)
		}
	}
	if strings.Join(names, ",") != "codemode,read_file" {
		t.Fatalf("声明集 = %v, want [codemode read_file]（hidden 被过滤、排序稳定）", names)
	}
	if strings.Join(lo.seen, ",") != "codemode,read_file" {
		t.Fatalf("钩子入参 = %v, want [codemode read_file]（只见声明集）", lo.seen)
	}
}

// ---- SkipTruncateProvider：只豁免文本截断 ----

// bigTool 产出超过 defaultMaxResponseSize 的结果。
type bigTool struct {
	stubTool
	skip bool
}

func (b *bigTool) SkipTruncate() bool { return b.skip }

func TestSkipTruncateExemptsTextTruncation(t *testing.T) {
	big := strings.Repeat("x", defaultMaxResponseSize+5000)
	mk := func(skip bool) *Engine {
		e := NewToolEngine()
		e.RegisterTool(context.Background(), &bigTool{
			stubTool: stubTool{BaseTool: BaseTool{Name_: "big", Description_: "big", Params_: map[string]any{}}, result: big},
			skip:     skip,
		})
		return e
	}

	truncated := mk(false).ExecuteOne(context.Background(), core.ToolCall{Id: "1", Name: "big", Arguments: "{}"}, ExecuteOpts{}).Result
	if !strings.Contains(truncated, "bytes omitted from the middle") {
		t.Fatalf("普通工具的结果应被引擎截断（len=%d）", len(truncated))
	}
	full := mk(true).ExecuteOne(context.Background(), core.ToolCall{Id: "2", Name: "big", Arguments: "{}"}, ExecuteOpts{}).Result
	if len(full) != len(big) {
		t.Fatalf("SkipTruncate 工具的结果被截断: len=%d, want %d", len(full), len(big))
	}
}

// ---- OutputSchemaProvider：结构化结果落到 ToolResult.Structured ----

type structuredTool struct {
	stubTool
	payload []byte
}

func (s *structuredTool) OutputSchema() any { return map[string]any{"type": "object"} }

// Call 写引擎注入的**本次调用**槽（旧形态是实例字段 + getter，已因跨 run 串味废弃）。
func (s *structuredTool) Call(ctx context.Context, _, _ string) (string, error) {
	if sink := StructuredSinkFrom(ctx); sink != nil {
		sink.Set(s.payload)
	}
	return s.result, nil
}

func TestStructuredContentCapturedOnResult(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &structuredTool{
		stubTool: stubTool{BaseTool: BaseTool{Name_: "bash", Description_: "bash", Params_: map[string]any{}}, result: "text form"},
		payload:  []byte(`{"output":"text form","exit_code":0}`),
	})
	r := e.ExecuteOne(context.Background(), core.ToolCall{Id: "1", Name: "bash", Arguments: "{}"}, ExecuteOpts{})
	if string(r.Structured) != `{"output":"text form","exit_code":0}` {
		t.Fatalf("Structured = %q, want 结构化原文", r.Structured)
	}
	if r.Result != "text form" {
		t.Fatalf("Result = %q, want 文本路径不变（模型路径不受影响）", r.Result)
	}

	// 未实现该接口的工具：Structured 恒 nil（零行为变化）。
	plain := NewToolEngine()
	plain.RegisterTool(context.Background(), &stubTool{BaseTool: BaseTool{Name_: "p", Description_: "p", Params_: map[string]any{}}, result: "t"})
	if got := plain.ExecuteOne(context.Background(), core.ToolCall{Id: "2", Name: "p", Arguments: "{}"}, ExecuteOpts{}).Structured; got != nil {
		t.Fatalf("普通工具 Structured = %q, want nil", got)
	}
}

// crosstalkTool 两个并发调用会在 Call 内相遇（barrier），各自写入**自己**的结构化结果。
// 用于钉住「同一工具实例被并发调用」这条真实场景（主 agent 与后台子 agent 共享同一个
// ToolEngine）：实例字段形态会互相清空/串味（评审实测 3/300 复现），ctx 槽形态不会。
type crosstalkTool struct {
	stubTool
	entered chan struct{} // 每进入一次 Call 投递一次
	release chan struct{} // 两个调用都进来后才放行
}

func (c *crosstalkTool) OutputSchema() any { return map[string]any{"type": "object"} }

func (c *crosstalkTool) Call(ctx context.Context, _, args string) (string, error) {
	c.entered <- struct{}{}
	<-c.release
	if sink := StructuredSinkFrom(ctx); sink != nil {
		sink.Set([]byte(`{"payload":` + args + `}`))
	}
	return args, nil
}

// TestStructuredSinkIsPerCallNotPerToolInstance 并发调用同一实例：各拿各的结构化结果。
// 旧形态（实例字段 + getter）在此必然串味或清空 —— 这是「模型路径清空脚本路径结果」
// 那个真实回归的最小复现形态。
func TestStructuredSinkIsPerCallNotPerToolInstance(t *testing.T) {
	e := NewToolEngine()
	tool := &crosstalkTool{
		stubTool: stubTool{BaseTool: BaseTool{Name_: "x", Description_: "x", Params_: map[string]any{}}},
		entered:  make(chan struct{}, 2),
		release:  make(chan struct{}),
	}
	e.RegisterTool(context.Background(), tool)

	results := make(chan core.ToolResult, 2)
	for _, id := range []string{"1", "2"} {
		go func(id string) {
			results <- e.ExecuteOne(context.Background(),
				core.ToolCall{Id: id, Name: "x", Arguments: `"` + id + `"`}, ExecuteOpts{})
		}(id)
	}
	// 两个调用都已进入 Call（同一实例同时在跑）→ 放行
	<-tool.entered
	<-tool.entered
	close(tool.release)

	got := map[string]string{}
	for i := 0; i < 2; i++ {
		r := <-results
		got[r.Id] = string(r.Structured)
	}
	for _, id := range []string{"1", "2"} {
		want := `{"payload":"` + id + `"}`
		if got[id] != want {
			t.Fatalf("调用 %s 的 Structured = %q, want %q（并发调用同一实例串味了）", id, got[id], want)
		}
	}
}
