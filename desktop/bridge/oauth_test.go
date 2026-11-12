// OAuth 订阅登录：bridge 层测试（authType 接线 + oauth 命令分发）。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/provider"
)

// TestLoadSettingsAuthType settings.json auth_type 解析：缺省 api_key / 显式 oauth / 非法回退。
func TestLoadSettingsAuthType(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"provider":{"active":"anthropic",` +
		`"anthropic":{"base_url":"https://api.anthropic.com","protocol":"anthropic","auth_type":"oauth"},` +
		`"deepseek":{"base_url":"https://api.deepseek.com/v1","auth_type":"bogus"},` +
		`"openai":{"base_url":"https://api.openai.com/v1"}}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := loadProviderConfig()
	if got := pc.authTypeOf("anthropic"); got != "oauth" {
		t.Errorf("anthropic auth_type = %q, want oauth", got)
	}
	if got := pc.authTypeOf("deepseek"); got != "api_key" {
		t.Errorf("deepseek auth_type = %q, want api_key（非法值回退）", got)
	}
	if got := pc.authTypeOf("openai"); got != "api_key" {
		t.Errorf("openai auth_type = %q, want api_key（缺省）", got)
	}
}

// TestOAuthFlowOf 预设 OAuth 声明：支持 OAuth 的 provider 能构造 flow；无声明返回 nil。
func TestOAuthFlowOf(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.MkdirAll(filepath.Join(home, ".go-code"), 0o755)
	pc := loadProviderConfig()

	for _, id := range []string{"anthropic", "xai", "openrouter", "openai-codex", "github-copilot", "kimi-coding"} {
		if pc.oauthFlowOf(id) == nil {
			t.Errorf("%s 应支持 OAuth flow", id)
		}
	}
	// 无 OAuth 的 provider
	if pc.oauthFlowOf("deepseek") != nil {
		t.Error("deepseek 不应支持 OAuth flow")
	}
	// 自定义 provider（无预设）
	pc.setBaseURL("my-gateway", "http://localhost:8080/v1")
	pc.setKey("my-gateway", "sk-x")
	if pc.oauthFlowOf("my-gateway") != nil {
		t.Error("自定义 provider 不应支持 OAuth flow")
	}
}

// TestNewProviderOAuthMode OAuth 模式下 newProvider 仍可构造（未登录时请求层报 401）。
func TestNewProviderOAuthMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.MkdirAll(filepath.Join(home, ".go-code"), 0o755)
	pc := loadProviderConfig()

	// anthropic OAuth 模式（未登录）：构造 AnthropicProvider（transport 注入 HTTP 层）
	pc.setAuthType("anthropic", "oauth")
	if p := pc.newProvider("anthropic"); p == nil {
		t.Fatal("anthropic OAuth 模式 newProvider 返回 nil")
	}

	// OpenRouter OAuth 模式：登录产物是永久 key → KeyInstead 走 api_key
	pc.setAuthType("openrouter", "oauth")
	pc.setKey("openrouter", "sk-or-123")
	if got := pc.keyLocked("openrouter"); got != "sk-or-123" {
		t.Errorf("openrouter keyLocked = %q, want sk-or-123", got)
	}
}

// lastCmdResponse 取最近一次 command_response 的 data。
func lastCmdResponse(t *testing.T, m *manager, buf *syncBuf) map[string]any {
	t.Helper()
	lines := readLines(buf)
	resp := firstCommandResponse(lines)
	if resp == nil {
		t.Fatal("no command_response")
	}
	data, _ := resp["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	return data
}

// TestOAuthStatusCommand oauth_status 命令：未登录返回 logged_in=false + supports_oauth。
func TestOAuthStatusCommand(t *testing.T) {
	m, buf := testManager(t)

	m.dispatch(cmd(1, "oauth_status", map[string]any{"provider": "anthropic"}))
	data := lastCmdResponse(t, m, buf)
	if got := data["supports_oauth"]; got != true {
		t.Errorf("anthropic supports_oauth = %v, want true", got)
	}
	if got := data["logged_in"]; got != false {
		t.Errorf("anthropic logged_in = %v, want false（未登录）", got)
	}

	m.dispatch(cmd(2, "oauth_status", map[string]any{"provider": "deepseek"}))
	data = lastCmdResponse(t, m, buf)
	if got := data["supports_oauth"]; got != false {
		t.Errorf("deepseek supports_oauth = %v, want false", got)
	}
}

// TestOAuthLogoutCommand oauth_logout 命令：无凭证时幂等成功。
func TestOAuthLogoutCommand(t *testing.T) {
	m, buf := testManager(t)

	m.dispatch(cmd(1, "oauth_logout", map[string]any{"provider": "anthropic"}))
	lines := readLines(buf)
	resp := firstCommandResponse(lines)
	if resp == nil || resp["ok"] != true {
		t.Fatalf("oauth_logout 应成功：%v", resp)
	}
}

// TestOAuthPromptAnswer 未等待中的输入 → 报错；注册后 → 成功；request_id 精确路由。
func TestOAuthPromptAnswer(t *testing.T) {
	m, buf := testManager(t)

	// 无等待者
	m.dispatch(cmd(1, "oauth_prompt_answer", map[string]any{"provider": "anthropic", "value": "code-123"}))
	lines := readLines(buf)
	if resp := firstCommandResponse(lines); resp == nil || resp["ok"] == true {
		t.Fatal("oauth_prompt_answer 无等待者应失败")
	}

	// 注册等待者（带 request id）
	id, ch := m.registerOAuthPrompt("anthropic")
	defer m.unregisterOAuthPrompt(id, "anthropic")
	m.dispatch(cmd(2, "oauth_prompt_answer", map[string]any{"provider": "anthropic", "value": "code-456", "request_id": id}))
	lines = readLines(buf)
	if resp := firstCommandResponse(lines); resp == nil || resp["ok"] != true {
		t.Fatalf("oauth_prompt_answer 应成功：%v", resp)
	}
	select {
	case got := <-ch:
		if got != "code-456" {
			t.Errorf("answer = %q, want code-456", got)
		}
	default:
		t.Fatal("no answer delivered")
	}
}

// TestOAuthPromptAnswerConcurrentIsolation 同 provider 并发登录：request_id 精确路由，
// 回答不串流（各自拿到自己的值）。
func TestOAuthPromptAnswerConcurrentIsolation(t *testing.T) {
	m, _ := testManager(t)

	// 同一 provider 注册两个等待者（模拟并发登录）
	id1, ch1 := m.registerOAuthPrompt("anthropic")
	defer m.unregisterOAuthPrompt(id1, "anthropic")
	id2, ch2 := m.registerOAuthPrompt("anthropic")
	defer m.unregisterOAuthPrompt(id2, "anthropic")
	if id1 == id2 {
		t.Fatalf("并发注册应产生不同 request id: %q == %q", id1, id2)
	}

	// 各自按 id 回答
	if !m.answerOAuthPrompt(id1, "anthropic", "first") {
		t.Fatal("answer(id1) 应成功")
	}
	if !m.answerOAuthPrompt(id2, "anthropic", "second") {
		t.Fatal("answer(id2) 应成功")
	}
	if got := <-ch1; got != "first" {
		t.Errorf("ch1 = %q, want first", got)
	}
	if got := <-ch2; got != "second" {
		t.Errorf("ch2 = %q, want second", got)
	}

	// 无 id 兜底：路由到最新注册的等待者
	if !m.answerOAuthPrompt("", "anthropic", "legacy") {
		t.Fatal("无 id 兜底应成功")
	}
	if got := <-ch2; got != "legacy" {
		t.Errorf("legacy fallback ch2 = %q, want legacy", got)
	}
}

// TestPersistAuthType settings.json auth_type 持久化（oauth_login/logout 调用）。
func TestPersistAuthType(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"provider":{"active":"anthropic","anthropic":{"base_url":"https://api.anthropic.com","protocol":"anthropic"}}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := persistAuthType("anthropic", "oauth"); err != nil {
		t.Fatalf("persistAuthType: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var outer struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		t.Fatal(err)
	}
	var anthropic map[string]any
	if err := json.Unmarshal(outer.Provider["anthropic"], &anthropic); err != nil {
		t.Fatal(err)
	}
	if got := anthropic["auth_type"]; got != "oauth" {
		t.Errorf("persisted auth_type = %v, want oauth", got)
	}
}

// TestPersistAuthTypePreservesRestOfFile oauth_login/logout 的持久化**只改一个键**：
// settings.json 是多写者文件（Electron 写 theme/lang/mcpServers/其余 provider），按「只认识
// provider 段的 struct」整体重写会把别的顶层键静默抹掉 —— 真机事故：一次 OAuth 登录后
// settings.json 只剩 provider 一个键。这里连同「兄弟 provider 的未识别字段」一起钉住。
func TestPersistAuthTypePreservesRestOfFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	cfg := `{
  "theme": "dark",
  "lang": "zh",
  "mcpServers": {"srv": {"command": "npx", "args": ["-y", "x"]}},
  "provider": {
    "active": "anthropic",
    "anthropic": {"base_url": "https://api.anthropic.com", "protocol": "anthropic"},
    "opencode2": {"base_url": "https://opencode.ai/zen/go/v1", "api_key": "sk-x", "models": {"m": 100}, "future_field": {"keep": true}}
  }
}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := persistAuthType("anthropic", "oauth"); err != nil {
		t.Fatalf("persistAuthType: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(raw, &outer); err != nil {
		t.Fatalf("重写后的 settings.json 不是合法 JSON: %v", err)
	}
	for _, k := range []string{"theme", "lang", "mcpServers"} {
		if _, ok := outer[k]; !ok {
			t.Errorf("顶层键 %s 被 persistAuthType 抹掉了（只该改 provider.<name>.auth_type）", k)
		}
	}
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(outer["provider"], &providers); err != nil {
		t.Fatal(err)
	}
	// active 是保留键（字符串），逐个 provider 单独解析。
	cfgOf := func(name string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(providers[name], &m); err != nil {
			t.Fatalf("provider %s 解析失败: %v", name, err)
		}
		return m
	}
	if got := cfgOf("anthropic")["auth_type"]; got != "oauth" {
		t.Errorf("anthropic.auth_type = %v, want oauth", got)
	}
	if got := cfgOf("opencode2")["api_key"]; got != "sk-x" {
		t.Errorf("兄弟 provider api_key = %v, want sk-x（不得丢失）", got)
	}
	if _, ok := cfgOf("opencode2")["future_field"]; !ok {
		t.Error("兄弟 provider 的未识别字段被抹掉了（应按原始字节保留）")
	}
}

// TestPersistAuthTypeRejectsCorruptFile 坏 settings.json（半截/非 JSON）必须**拒绝写入**：
// 就地覆盖等于把用户整份配置换成半截内容，且没有任何备份可恢复。
func TestPersistAuthTypeRejectsCorruptFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	bad := `{"provider": {"anthropic": {`
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := persistAuthType("anthropic", "oauth"); err == nil {
		t.Fatal("坏 settings.json 应拒绝写入")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != bad {
		t.Errorf("坏文件被改写了：%q", string(got))
	}
}

// 确保 provider 包类型引用（编译期验证 presets.OAuth 可访问）。
var _ = provider.BuiltinProviderPresets[0].OAuth
