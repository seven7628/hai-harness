package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// 脚本路径的结果闸门：**读零损耗**，超过硬上限则整条落盘 + 只回一句说明。
//
// 起因（真机）：MCP search_scenarios(limit=100) 原文 20427 字节，被引擎的 20 KB 头尾
// 截断从中间挖掉 427 字节 —— 那句「[... 427 bytes omitted ...]」是**值的一部分**，于是
// 返回值不再是合法 JSON：脚本 JSON.parse 抛 "Bad control character"，只能
// s.includes("bytes omitted") 嗅探。而脚本本可以读 100 KB、parse 后只 text() 一个字段。
// 结论：截断该发生在**脚本打印**（codemode 的 budget + spill），不该发生在**脚本读取**。

// bigDoc 造一段 30 KB 的「合法 JSON」文本（远超模型路径的 20 KB）。
func bigDoc() string {
	var b strings.Builder
	b.WriteString(`{"scanned":154,"scenarios":[`)
	for i := 0; i < 800; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":4586,"name":"场景","step_count":70}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// TestScriptCallKeepsFullResult 脚本路径下 30 KB 的 JSON 结果**逐字节完整**（不打洞）。
func TestScriptCallKeepsFullResult(t *testing.T) {
	doc := bigDoc()
	if len(doc) <= defaultMaxResponseSize {
		t.Fatalf("用例前提不成立：文档只有 %d 字节（应超过模型路径的 %d）", len(doc), defaultMaxResponseSize)
	}
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &stubTool{
		BaseTool: BaseTool{Name_: "doc", Description_: "doc", Params_: map[string]any{}},
		result:   doc,
	})

	// ① 模型路径（Depth=0）：照旧 20 KB 头尾截断（上下文预算不能被单个结果炸掉）。
	top := e.ExecuteOne(context.Background(), core.ToolCall{Id: "top", Name: "doc", Arguments: "{}"}, ExecuteOpts{})
	if !strings.Contains(top.Result, "bytes omitted from the middle") {
		t.Fatalf("模型路径应仍然截断（20 KB 预算）")
	}

	// ② 脚本路径（Depth=1）：完整，且不含任何「omitted」标记（否则 JSON 就被破坏了）。
	got := e.ExecuteOne(context.Background(), core.ToolCall{Id: "n1", Name: "doc", Arguments: "{}"}, ExecuteOpts{Depth: 1})
	if got.Result != doc {
		t.Fatalf("脚本路径结果被改动：got %d bytes, want %d", len(got.Result), len(doc))
	}
	if strings.Contains(got.Result, "bytes omitted") {
		t.Fatalf("脚本路径不该出现截断标记（它是值的一部分，会破坏 JSON）")
	}
}

// TestScriptCallOverLimitSpillsWholeResult 超过脚本硬上限 → 只回一句带路径的说明，
// **不返回半截数据**（半截 JSON 会被当值用；说明则 parse 必失败、必然被看见）。
func TestScriptCallOverLimitSpillsWholeResult(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spill")
	e := NewToolEngine(WithMaxScriptResponseSize(10), WithSpillDir(dir))
	doc := bigDoc()
	e.RegisterTool(context.Background(), &stubTool{
		BaseTool: BaseTool{Name_: "doc", Description_: "doc", Params_: map[string]any{}},
		result:   doc,
	})

	got := e.ExecuteOne(context.Background(), core.ToolCall{Id: "n1", Name: "doc", Arguments: "{}"}, ExecuteOpts{Depth: 1})
	if !strings.Contains(got.Result, "tool result not inlined") {
		t.Fatalf("超限应回「未内联」说明: %q", got.Result)
	}
	if strings.Contains(got.Result, `"scanned"`) {
		t.Fatalf("超限不该返回半截数据（说明必须整条替换结果）: %q", got.Result)
	}
	path := spillPathOf(t, got.Result)
	if !strings.HasPrefix(path, dir) {
		t.Fatalf("路径应在 spill 目录内: %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("落盘文件应可读: %v", err)
	}
	if string(data) != doc {
		t.Fatalf("落盘的必须是完整原文: got %d bytes, want %d", len(data), len(doc))
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
		t.Fatalf("落盘文件应 0600，实际 %o", fi.Mode().Perm())
	}
}

// TestScriptCallOverLimitNoSpillDir 落盘不可用时也不返回半截数据（宁可只给一句话）。
func TestScriptCallOverLimitNoSpillDir(t *testing.T) {
	e := NewToolEngine(WithMaxScriptResponseSize(10))
	e.RegisterTool(context.Background(), &stubTool{
		BaseTool: BaseTool{Name_: "doc", Description_: "doc", Params_: map[string]any{}},
		result:   bigDoc(),
	})
	got := e.ExecuteOne(context.Background(), core.ToolCall{Id: "n1", Name: "doc", Arguments: "{}"}, ExecuteOpts{Depth: 1})
	if !strings.Contains(got.Result, "tool result not inlined") {
		t.Fatalf("应回「未内联」说明: %q", got.Result)
	}
	if strings.Contains(got.Result, `"scanned"`) {
		t.Fatalf("不该返回半截数据: %q", got.Result)
	}
	if !strings.Contains(got.Result, "could not be saved to disk") {
		t.Fatalf("落盘不可用时应说明原因: %q", got.Result)
	}
}
