// OAuth 订阅登录：bridge IPC 命令（oauth_login / oauth_logout / oauth_status）。
// 登录交互（auth_url / device_code 事件 + manual_code 输入）经 stdout 事件通道与前端
// 双通道协作：事件推送 + 命令响应轮询 manual 输入。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/provider/auth"
)

// oauthEventPrefix 前端识别 OAuth 登录事件的 event_type 前缀。
const oauthEventPrefix = "oauth_event"

// oauthInteraction 实现 auth.Interaction：
//   - Notify → 推送 oauth_event 事件行到桌面端（前端弹卡片展示 auth_url/device_code）；
//   - Prompt → 向桌面端发 oauth_prompt_request 事件并阻塞等待 oauth_prompt_answer 命令。
//
// 前端「粘贴授权码/重定向 URL」输入经 oauth_prompt_answer 命令回填。
type oauthInteraction struct {
	m        *manager
	provider string
}

// Notify 实现 auth.Interaction：推送登录事件到桌面端。
func (oi *oauthInteraction) Notify(event auth.AuthEvent) error {
	oi.m.out.writeLine(map[string]any{
		"event_type": oauthEventPrefix,
		"provider":   oi.provider,
		"event":      event,
	})
	return nil
}

// Prompt 实现 auth.Interaction：推送输入请求并等待 oauth_prompt_answer 命令。
// ctx 取消或超时返回 ErrCancelled。事件携带 request id（前端回填时精确路由）。
func (oi *oauthInteraction) Prompt(ctx context.Context, p auth.Prompt) (string, error) {
	// 注册等待表（request id 精确路由：同 provider 并发登录互不干扰）
	id, ch := oi.m.registerOAuthPrompt(oi.provider)
	// 推送输入请求事件
	oi.m.out.writeLine(map[string]any{
		"event_type": "oauth_prompt_request",
		"provider":   oi.provider,
		"request_id": id,
		"prompt":     p,
	})

	select {
	case answer, ok := <-ch:
		if !ok || answer == "" {
			return "", auth.ErrCancelled
		}
		return answer, nil
	case <-ctx.Done():
		oi.m.unregisterOAuthPrompt(id, oi.provider)
		return "", auth.ErrCancelled
	case <-time.After(15 * time.Minute):
		oi.m.unregisterOAuthPrompt(id, oi.provider)
		return "", auth.ErrCancelled
	}
}

// oauthPromptWaiter 维护 request id → 等待 oauth_prompt_answer 的通道表。
// 以「登录请求 id」而非 provider 为 key：同一 provider 并发登录（或登录中再次
// 触发）时各请求有独立通道，回答精确路由到对应流程，不互相覆盖（V2 观察项）。
type oauthPromptWaiter struct {
	mu     sync.Mutex
	seq    int64
	chans  map[string]chan string
	byProv map[string]string // provider → 最新 request id（兼容旧前端只传 provider）
}

func (w *oauthPromptWaiter) register(provider string) (string, <-chan string) {
	if w.chans == nil {
		w.chans = map[string]chan string{}
	}
	if w.byProv == nil {
		w.byProv = map[string]string{}
	}
	w.seq++
	id := fmt.Sprintf("%s#%d", provider, w.seq)
	ch := make(chan string, 1)
	w.chans[id] = ch
	w.byProv[provider] = id
	return id, ch
}

func (w *oauthPromptWaiter) answer(id, provider, value string) bool {
	// 优先精确 id；无 id 时按 provider 的最新请求兜底（旧前端）
	if id == "" {
		id = w.byProv[provider]
	}
	ch, ok := w.chans[id]
	if !ok {
		return false
	}
	select {
	case ch <- value:
		return true
	default:
		return false
	}
}

func (w *oauthPromptWaiter) unregister(id, provider string) {
	if id != "" {
		delete(w.chans, id)
	}
	if provider != "" {
		if cur, ok := w.byProv[provider]; ok && cur == id {
			delete(w.byProv, provider)
		}
	}
}

// registerOAuthPrompt 注册等待表（manager 内嵌 waiter）。返回 (request id, 通道)。
func (m *manager) registerOAuthPrompt(provider string) (string, <-chan string) {
	m.oauthWaiter.mu.Lock()
	defer m.oauthWaiter.mu.Unlock()
	return m.oauthWaiter.register(provider)
}

func (m *manager) answerOAuthPrompt(id, provider, value string) bool {
	m.oauthWaiter.mu.Lock()
	defer m.oauthWaiter.mu.Unlock()
	return m.oauthWaiter.answer(id, provider, value)
}

func (m *manager) unregisterOAuthPrompt(id, provider string) {
	m.oauthWaiter.mu.Lock()
	defer m.oauthWaiter.mu.Unlock()
	m.oauthWaiter.unregister(id, provider)
}

// persistAuthType 把某 provider 的 auth_type 持久化到 settings.json provider 段。
// 桌面端用户决策：provider 配置直接写 settings.json（与 api_key 明文同通道）。
// 原子写（tmp + rename）：崩溃不产生半写文件；写失败返回错误（调用方记日志）。
func persistAuthType(providerName, authType string) error {
	path := homeCfgPath("settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err // 无 settings（headless）→ 仅内存生效
	}
	var outer struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil || outer.Provider == nil {
		return fmt.Errorf("settings.json parse failed: %v", err)
	}
	block, ok := outer.Provider[providerName]
	if !ok {
		return fmt.Errorf("provider %s not persisted yet", providerName) // 纯内存 provider → 前端保存时会带 auth_type
	}
	var p map[string]any
	if err := json.Unmarshal(block, &p); err != nil {
		return err
	}
	p["auth_type"] = authType
	updated, err := json.Marshal(p)
	if err != nil {
		return err
	}
	outer.Provider[providerName] = updated
	out, err := json.MarshalIndent(outer, "", "  ")
	if err != nil {
		return err
	}
	// 原子写：tmp + rename（与 store.writeAll 同模式；防崩溃产生半写 settings.json）
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil { // settings 含明文 key，0600
		return err
	}
	return os.Rename(tmp, path)
}

// handleOAuthLogin oauth_login 命令：发起订阅登录。
// payload: {provider}
// 流程：构造 flow → 起登录（Notify 推送事件 / Prompt 等 manual）→ 成功后：
//   - 凭证写 oauthStore；
//   - authType 置 oauth（persist settings auth_type）；
//   - OpenRouter（KeyInstead）：产物是永久 key → 落 apiKey map + api_key 清空 auth_type；
//   - rebuildAll 重建会话 loop。
func (m *manager) handleOAuthLogin(c command) {
	providerName := str(c.Payload, "provider")
	if providerName == "" {
		m.resp(c, false, "provider is empty", nil)
		return
	}
	// 校验该 provider 支持 OAuth
	flow := m.provCfg.oauthFlowOf(providerName)
	if flow == nil {
		m.resp(c, false, fmt.Sprintf("provider %s does not support OAuth login", providerName), nil)
		return
	}

	oi := &oauthInteraction{m: m, provider: providerName}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	cred, err := flow.Login(ctx, oi)
	if err != nil {
		m.resp(c, false, "OAuth login failed: "+err.Error(), nil)
		return
	}

	// 写入凭证
	if err := m.provCfg.oauthStore.Modify(providerName, func(cur *auth.OAuthCredential) (*auth.OAuthCredential, error) {
		return cred, nil
	}); err != nil {
		m.resp(c, false, "OAuth credential save failed: "+err.Error(), nil)
		return
	}

	preset := provider.FindProviderPreset(providerName)
	if preset != nil && preset.OAuth != nil && preset.OAuth.KeyInstead {
		// OpenRouter：永久 key → 落 api_key 通道；auth_type 保持 api_key
		m.provCfg.setKey(providerName, cred.Access)
		m.provCfg.setAuthType(providerName, "api_key")
		if err := persistAuthType(providerName, "api_key"); err != nil {
			fmt.Fprintln(os.Stderr, "✗ oauth_login persist auth_type:", err)
		}
	} else {
		// 常规 OAuth：auth_type=oauth
		m.provCfg.setAuthType(providerName, "oauth")
		if err := persistAuthType(providerName, "oauth"); err != nil {
			fmt.Fprintln(os.Stderr, "✗ oauth_login persist auth_type:", err)
		}
	}

	m.provCfg.loadSettings()
	m.provCfg.syncRegistry()
	m.rebuildAll()
	m.resp(c, true, "", map[string]any{"provider": providerName, "source": "oauth"})
}

// handleOAuthLogout oauth_logout 命令：删除凭证并回退 api_key。
// OpenRouter（KeyInstead）特殊：登录时 key 落在 apiKey 通道，登出必须同时清 api_key，
// 否则登出无效（请求仍用旧 key 工作）。
func (m *manager) handleOAuthLogout(c command) {
	providerName := str(c.Payload, "provider")
	if providerName == "" {
		m.resp(c, false, "provider is empty", nil)
		return
	}
	if err := m.provCfg.oauthStore.Delete(providerName); err != nil {
		m.resp(c, false, "OAuth logout failed: "+err.Error(), nil)
		return
	}
	// KeyInstead（OpenRouter）：清掉登录时写入的 api_key（避免登出后仍可用）
	preset := provider.FindProviderPreset(providerName)
	if preset != nil && preset.OAuth != nil && preset.OAuth.KeyInstead {
		m.provCfg.setKey(providerName, "")
	}
	// OAuth transport 缓存失效（防已登出 provider 仍注入旧 token）
	if tr := m.provCfg.oauthTransportOf(providerName); tr != nil {
		tr.TokenSource().Invalidate()
	}
	m.provCfg.setAuthType(providerName, "api_key")
	if err := persistAuthType(providerName, "api_key"); err != nil {
		fmt.Fprintln(os.Stderr, "✗ oauth_logout persist auth_type:", err)
	}
	m.provCfg.loadSettings()
	m.provCfg.syncRegistry()
	m.rebuildAll()
	m.resp(c, true, "", map[string]any{"provider": providerName})
}

// handleOAuthStatus oauth_status 命令：查询某 provider 的 OAuth 登录状态（不含 token）。
func (m *manager) handleOAuthStatus(c command) {
	providerName := str(c.Payload, "provider")
	if providerName == "" {
		m.resp(c, false, "provider is empty", nil)
		return
	}
	flow := m.provCfg.oauthFlowOf(providerName)
	cred, _ := m.provCfg.oauthStore.Read(providerName)
	loggedIn := cred != nil && cred.Valid()
	out := map[string]any{
		"provider":       providerName,
		"logged_in":      loggedIn,
		"supports_oauth": flow != nil,
	}
	if flow != nil {
		out["name"] = flow.Name()
		out["subscription"] = flow.IsSubscription()
	}
	// 最近一次鉴权错误（未登录/刷新失败）→ 前端提示「请重新登录」
	if tr := m.provCfg.oauthTransportOf(providerName); tr != nil {
		if lerr := tr.LastError(); lerr != nil {
			out["auth_error"] = lerr.Error()
		}
	}
	if loggedIn {
		out["source"] = "oauth"
		if cred.Expires < int64(^uint64(0)>>1) {
			out["expires_at"] = cred.Expires
		}
	}
	m.resp(c, true, "", out)
}
