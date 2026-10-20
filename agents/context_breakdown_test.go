package agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

// sizedTool 分区计量用的测试工具（名字/描述可控：断言按 JSON 字节对账）。
type sizedTool struct{ name, desc string }

func (t sizedTool) Name() string                                      { return t.name }
func (t sizedTool) Description() string                               { return t.desc }
func (t sizedTool) Parameters() any                                   { return map[string]any{"type": "object"} }
func (t sizedTool) CanParallel() bool                                 { return true }
func (t sizedTool) ValidParams(context.Context, string, string) error { return nil }
func (t sizedTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}
func (t sizedTool) Call(context.Context, string, string) (string, error) { return "ok", nil }
func (t sizedTool) AfterCall(context.Context, core.ToolCall)             {}

func partOf(t *testing.T, bd ContextBreakdown, key string) ContextPart {
	t.Helper()
	for _, p := range bd.Parts {
		if p.Key == key {
			return p
		}
	}
	t.Fatalf("分区 %s 缺失: %+v", key, bd.Parts)
	return ContextPart{}
}

func schemaChars(t *testing.T, s core.ToolSchema) int64 {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(b))
}

// TestContextBreakdownParts 分区对账：六个分区键恒在（稳定顺序），
// 系统提示词/技能/工具/MCP/消息/其他 各自计量口径与请求组装一致。
func TestContextBreakdownParts(t *testing.T) {
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), sizedTool{name: "read_file", desc: "读文件"})
	eng.RegisterTool(context.Background(), sizedTool{name: "mcp__github__search", desc: "search repos"})
	sk := skillRegistryWith(t, "code-review", "审查代码变更的正确性")

	loop := NewAgentLoop(
		WithBaseSystemPrompt("BASE 行为契约"),
		WithSystemPrompt("产品层 persona"),
		WithWorkingDir("/ws/demo"),
		WithToolEngine(eng),
		WithSkills(sk),
	)

	// 首条 system 是「上一轮的」系统提示词：不应重复计入消息分区。
	msgs := []core.Message{
		core.NewSystemMessage("旧系统提示词（不应计入消息分区）"),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "帮我看看这个 bug"}),
		{Role: core.Assistant, Content: []core.Content{{Type: core.ContentTypeText, Content: "好的，先看日志"}}},
	}

	bd := loop.ContextBreakdown(context.Background(), msgs, nil)

	wantKeys := []string{
		ContextPartSystem, ContextPartSkills, ContextPartMemory, ContextPartTools,
		ContextPartMCP, ContextPartMessages, ContextPartOther,
	}
	if len(bd.Parts) != len(wantKeys) {
		t.Fatalf("分区数 = %d, want %d: %+v", len(bd.Parts), len(wantKeys), bd.Parts)
	}
	for i, k := range wantKeys {
		if bd.Parts[i].Key != k {
			t.Fatalf("分区顺序[%d] = %s, want %s", i, bd.Parts[i].Key, k)
		}
	}

	// 系统提示词 = composeSystemPrompt 去掉两个清单的字节；技能 = 技能清单字节。
	manifest := buildSkillManifest(sk, nil)
	if manifest == "" {
		t.Fatal("技能清单不应为空")
	}
	wantSystem := int64(len(loop.composeSystemPrompt())) - int64(len(manifest))
	if got := partOf(t, bd, ContextPartSystem).Chars; got != wantSystem {
		t.Errorf("系统提示词字符 = %d, want %d", got, wantSystem)
	}
	if got := partOf(t, bd, ContextPartSkills).Chars; got != int64(len(manifest)) {
		t.Errorf("技能字符 = %d, want %d", got, int64(len(manifest)))
	}

	// 工具 / MCP：按名字前缀分流 schema JSON 字节。
	wantTools := schemaChars(t, core.ToolSchema{Name: "read_file", Description: "读文件", Parameters: map[string]any{"type": "object"}})
	wantMCP := schemaChars(t, core.ToolSchema{Name: "mcp__github__search", Description: "search repos", Parameters: map[string]any{"type": "object"}})
	if got := partOf(t, bd, ContextPartTools).Chars; got != wantTools {
		t.Errorf("工具字符 = %d, want %d", got, wantTools)
	}
	if got := partOf(t, bd, ContextPartMCP).Chars; got != wantMCP {
		t.Errorf("MCP 字符 = %d, want %d", got, wantMCP)
	}

	// 消息：跳过首条 system；其他 = 每条消息的框架开销（无 subagent 清单时）。
	body := msgs[1:]
	if got, want := partOf(t, bd, ContextPartMessages).Chars, messagesChars(body); got != want {
		t.Errorf("消息字符 = %d, want %d（首条 system 不应计入）", got, want)
	}
	if got, want := partOf(t, bd, ContextPartOther).Chars, int64(len(body))*messageFrameChars; got != want {
		t.Errorf("其他字符 = %d, want %d", got, want)
	}
	if bd.MessageCount != len(body) {
		t.Errorf("MessageCount = %d, want %d", bd.MessageCount, len(body))
	}

	// 合计与 token 折算（/4 口径）。
	var sumChars, sumTokens int64
	for _, p := range bd.Parts {
		sumChars += p.Chars
		sumTokens += p.Chars / 4
		if p.Tokens != p.Chars/4 {
			t.Errorf("分区 %s token 折算 = %d, want %d", p.Key, p.Tokens, p.Chars/4)
		}
	}
	if bd.TotalChars != sumChars || bd.TotalTokens != sumTokens {
		t.Errorf("合计 = (%d,%d), want (%d,%d)", bd.TotalChars, bd.TotalTokens, sumChars, sumTokens)
	}
}

// TestContextBreakdownModeFilter 模式不可见的工具不进请求，也不进面板；
// 模式提示（SystemHint）计入系统提示词分区。
func TestContextBreakdownModeFilter(t *testing.T) {
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), sizedTool{name: "read_file", desc: "读文件"})
	eng.RegisterTool(context.Background(), sizedTool{name: "write_file", desc: "写文件"})

	loop := NewAgentLoop(WithBaseSystemPrompt("BASE"), WithToolEngine(eng))
	plain := loop.ContextBreakdown(context.Background(), nil, nil)

	mode := &Mode{
		Name:       "plan",
		AllowTool:  func(tl tools.Tool) bool { return tl.Name() == "read_file" },
		SystemHint: "## 计划模式\n- 只读",
	}
	filtered := loop.ContextBreakdown(context.Background(), nil, mode)

	// 按 key 取，不按下标：分区序会随新分区插入而变（2026-09-29 插入 memory），
	// 下标取值的写法在某次新增分区后静默取到别的分区，断言还"看起来在测"。
	partOf := func(bd ContextBreakdown, key string) int64 {
		for _, p := range bd.Parts {
			if p.Key == key {
				return p.Chars
			}
		}
		t.Fatalf("缺分区 %s: %+v", key, bd.Parts)
		return 0
	}
	fTools, pTools := partOf(filtered, ContextPartTools), partOf(plain, ContextPartTools)
	if fTools >= pTools {
		t.Errorf("plan 模式工具字符应少于全量：%d vs %d", fTools, pTools)
	}
	wantHint := int64(len("## 计划模式\n- 只读") + 2) // "\n\n" 连接符
	fSys, pSys := partOf(filtered, ContextPartSystem), partOf(plain, ContextPartSystem)
	if got := fSys - pSys; got != wantHint {
		t.Errorf("系统提示词增量 = %d, want %d（模式提示）", got, wantHint)
	}
	// 无消息：消息分区为 0，其他分区也为 0（无框架开销）。
	if filtered.Parts[4].Chars != 0 || filtered.Parts[5].Chars != 0 {
		t.Errorf("空消息下消息/其他分区应为 0: %+v", filtered.Parts[4:])
	}
}

// TestContextBreakdownNoLoopConfig 最小装配（无技能/无工具/无消息）：
// 恒返回六个分区键且全 0，调用方无需判空。
func TestContextBreakdownNoLoopConfig(t *testing.T) {
	loop := NewAgentLoop(WithBaseSystemPrompt(""))
	bd := loop.ContextBreakdown(context.Background(), nil, nil)
	if len(bd.Parts) != 7 { // 2026-09-29 起含 memory（Project Memory 索引）
		t.Fatalf("分区数 = %d, want 7", len(bd.Parts))
	}
	if bd.TotalChars != 0 || bd.TotalTokens != 0 {
		t.Errorf("空装配合计应为 0: %+v", bd)
	}
}
