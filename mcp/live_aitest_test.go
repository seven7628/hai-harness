package mcp

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestLiveAITestStructured 真机验证（默认跳过）：连真实的 ai-test MCP，确认脚本侧拿到的是
// 对象而不是 JSON 字符串。GO_CODE_LIVE_MCP=1 时运行。
func TestLiveAITestStructured(t *testing.T) {
	if os.Getenv("GO_CODE_LIVE_MCP") != "1" {
		t.Skip("设置 GO_CODE_LIVE_MCP=1 才连真机 MCP")
	}
	cfg := map[string]ServerConfig{
		"ai-test": {Type: "http", URL: "http://192.168.160.66:18111/mcp"},
	}
	m := NewManager(cfg)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var target string
	for _, tl := range m.Tools(ctx) {
		if tl.Name() == "mcp__ai-test__search_scenarios" {
			target = tl.Name()
		}
	}
	if target == "" {
		t.Fatalf("没找到 search_scenarios，工具表 %v", names(m.Tools(ctx)))
	}
	_, structured, err := callAsScript(t, m, target, `{"project":"agent-server-g-refactor","query":"readonly"}`)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if structured == nil {
		t.Fatalf("没有结构化结果（脚本侧仍会拿到字符串）")
	}
	var out map[string]any
	if err := json.Unmarshal(structured, &out); err != nil {
		t.Fatalf("structured 非法: %v", err)
	}
	t.Logf("Structured OK: count=%v scanned=%v", out["count"], out["scanned"])
	if out["scanned"] == nil {
		t.Fatalf("scanned 缺失: %v", out)
	}
}
