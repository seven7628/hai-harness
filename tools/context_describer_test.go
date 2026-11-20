package tools

// 引擎侧的 cwd 描述通道测试（tools.ContextDescriber）。
//
// 来源：docs/AGENT_CWD_DESIGN_2026-11-19.md §12.4 / §15.2 ②
// + /private/tmp/patch_cwd_v3b.py 的 §12.4 代码块。
//
// 不变式：ToolParams(ctx) 每请求重建 schema，**优先**用 DescribeFor(ctx)；
// 这样 bash 的「Working directory: …」每次都是**当前**值（cd 之后立刻反映）。

import (
	"context"
	"strings"
	"testing"
)

// ctxDescTool 实现 ContextDescriber，用来钉住「引擎优先用 ctx 版描述」。
type ctxDescTool struct {
	BaseTool
}

func (d *ctxDescTool) Name() string                                      { return "ctxdesc" }
func (d *ctxDescTool) Description() string                               { return "static" }
func (d *ctxDescTool) Parameters() any                                   { return map[string]any{"type": "object"} }
func (d *ctxDescTool) CanParallel() bool                                 { return false }
func (d *ctxDescTool) DescribeFor(context.Context) string                { return "ctx-aware" }
func (d *ctxDescTool) ValidParams(context.Context, string, string) error { return nil }
func (d *ctxDescTool) Call(context.Context, string, string) (string, error) {
	return "", nil
}

// TestToolParamsPrefersContextDescriber 引擎优先用 ctx 版描述。
func TestToolParamsPrefersContextDescriber(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &ctxDescTool{})
	schemas, err := e.ToolParams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(schemas) != 1 {
		t.Fatalf("schema 数 = %d, want 1", len(schemas))
	}
	if !strings.Contains(schemas[0].Description, "ctx-aware") {
		t.Fatalf("引擎没优先用 DescribeFor：%q", schemas[0].Description)
	}
	if got := schemas[0].Name; got != "ctxdesc" {
		t.Fatalf("name = %q", got)
	}
}
