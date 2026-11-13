// OAuth 登录异步化端到端测试：验证 oauth_login 不阻塞 dispatch（严重问题 1 修复）。
// 场景：dispatch oauth_login → flow.Login 内 Prompt 阻塞等待 manual 输入 →
//
//	dispatch 必须能继续处理 oauth_prompt_answer → 登录完成。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/provider/auth"
)

// fakeOAuthFlow 模拟一个需要 manual 输入的 OAuth flow：
// Login 内 Prompt 等待用户粘贴授权码，收到后返回凭证。
type fakeOAuthFlow struct {
	mu       sync.Mutex
	prompted bool
	done     chan struct{}
}

func (f *fakeOAuthFlow) Name() string         { return "fake-oauth" }
func (f *fakeOAuthFlow) IsSubscription() bool { return true }
func (f *fakeOAuthFlow) Login(ctx context.Context, interaction auth.Interaction) (*auth.OAuthCredential, error) {
	// 通知 auth_url（前端展示）
	if err := interaction.Notify(auth.AuthEvent{Type: auth.EventAuthURL, URL: "https://example.com/auth"}); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.prompted = true
	f.mu.Unlock()
	// Prompt 阻塞等待 manual 输入（oauth_prompt_answer 命令送达）
	code, err := interaction.Prompt(ctx, auth.Prompt{
		Type:    auth.PromptManualCode,
		Message: "paste code",
	})
	if err != nil {
		return nil, err
	}
	close(f.done)
	return &auth.OAuthCredential{Type: "oauth", Access: "access-" + code, Expires: time.Now().Add(time.Hour).UnixMilli()}, nil
}
func (f *fakeOAuthFlow) Refresh(ctx context.Context, cred *auth.OAuthCredential) (*auth.OAuthCredential, error) {
	return nil, nil
}
func (f *fakeOAuthFlow) ToAuth(cred *auth.OAuthCredential) (auth.ModelAuth, error) {
	return auth.Bearer(cred.Access), nil
}

// registerFakeFlow 把 fake flow 注入 providerConfig（测试专用：直接塞 oauthFlow map）。
func registerFakeFlow(pc *providerConfig, name string, f auth.Flow) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.oauthFlow[name] = f
}

// TestOAuthLoginAsyncNoDeadlock 验证：oauth_login 异步执行，oauth_prompt_answer 不被阻塞。
// 修复前：oauth_login 同步阻塞 dispatch → prompt_answer 永远无法处理 → 测试超时失败。
// 修复后：oauth_login 在 goroutine 跑 → dispatch 立即返回 → prompt_answer 送达 → 登录完成。
func TestOAuthLoginAsyncNoDeadlock(t *testing.T) {
	m, buf := testManager(t)

	// 预置 settings.json（anthropic provider 块）→ persistAuthType 有落盘点
	home := os.Getenv("HOME")
	_ = os.MkdirAll(filepath.Join(home, ".go-code"), 0o755)
	cfg := `{"provider":{"active":"anthropic","anthropic":{"base_url":"https://api.anthropic.com","protocol":"anthropic"}}}`
	_ = os.WriteFile(filepath.Join(home, ".go-code", "settings.json"), []byte(cfg), 0o644)

	// 注册 fake flow（provider "anthropic" 已有真实 flow，但测试要可预测的 fake）
	flow := &fakeOAuthFlow{done: make(chan struct{})}
	registerFakeFlow(m.provCfg, "anthropic", flow)

	// 发起 oauth_login（dispatch 应立即返回，不阻塞）
	m.dispatch(cmd(1, "oauth_login", map[string]any{"provider": "anthropic"}))

	// 轮询等待 Prompt 已注册（oauth_prompt_request 事件或 flow.prompted）
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		flow.mu.Lock()
		p := flow.prompted
		flow.mu.Unlock()
		if p {
			break
		}
		// 也可以从 buf 读事件
		time.Sleep(20 * time.Millisecond)
	}
	flow.mu.Lock()
	prompted := flow.prompted
	flow.mu.Unlock()
	if !prompted {
		t.Fatal("flow.Prompt 未在 5s 内被调用（oauth_login 未启动或异步化失效）")
	}

	// 关键验证：dispatch 还能处理其它命令（未被 oauth_login 阻塞）
	done := make(chan struct{})
	go func() {
		m.dispatch(cmd(2, "oauth_prompt_answer", map[string]any{"provider": "anthropic", "value": "manual-code-42"}))
		close(done)
	}()
	select {
	case <-done:
		// prompt_answer 命令执行完成 = dispatch 未被阻塞（死锁已修复）
	case <-time.After(5 * time.Second):
		t.Fatal("oauth_prompt_answer 5s 未返回 → dispatch 被 oauth_login 阻塞（死锁未修复）")
	}

	// 登录完成：flow.Login 返回 → handleOAuthLogin 写凭证 → resp ok
	select {
	case <-flow.done:
		// Prompt 收到输入，Login 继续
	case <-time.After(5 * time.Second):
		t.Fatal("flow.Login 未完成（manual 输入未送达）")
	}

	// 等 handleOAuthLogin 完成（异步 goroutine）：轮询凭证落盘
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cred, _ := m.provCfg.oauthStore.Read("anthropic")
		if cred != nil && cred.Access == "access-manual-code-42" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cred, _ := m.provCfg.oauthStore.Read("anthropic")
	if cred == nil || cred.Access != "access-manual-code-42" {
		t.Fatalf("登录凭证未落盘: %+v", cred)
	}
	// auth_type 应为 oauth
	if got := m.provCfg.authTypeOf("anthropic"); got != "oauth" {
		t.Errorf("auth_type = %q, want oauth", got)
	}
	_ = buf
}

// TestOAuthLoginUnsupported 不支持的 provider 立即报错（不异步悬挂）。
func TestOAuthLoginUnsupported(t *testing.T) {
	m, buf := testManager(t)
	m.dispatch(cmd(1, "oauth_login", map[string]any{"provider": "deepseek"}))
	// oauth_login 异步化：等响应到达（轮询 buf）
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		lines := readLines(buf)
		if resp := firstCommandResponse(lines); resp != nil {
			if resp["ok"] != false {
				t.Fatalf("deepseek oauth_login 应失败：%v", resp)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("deepseek oauth_login 5s 无响应")
	_ = fmt.Sprintf
}

// TestOAuthLoginPersistsAuthType 验证 oauth_login 成功后 settings.json 的 auth_type=oauth 已原子持久化。
func TestOAuthLoginPersistsAuthType(t *testing.T) {
	m, _ := testManager(t)
	home := os.Getenv("HOME")
	_ = os.MkdirAll(filepath.Join(home, ".go-code"), 0o755)
	cfg := `{"provider":{"active":"anthropic","anthropic":{"base_url":"https://api.anthropic.com","protocol":"anthropic"}}}`
	_ = os.WriteFile(filepath.Join(home, ".go-code", "settings.json"), []byte(cfg), 0o644)

	flow := &fakeOAuthFlow{done: make(chan struct{})}
	registerFakeFlow(m.provCfg, "anthropic", flow)

	m.dispatch(cmd(1, "oauth_login", map[string]any{"provider": "anthropic"}))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		flow.mu.Lock()
		p := flow.prompted
		flow.mu.Unlock()
		if p {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.dispatch(cmd(2, "oauth_prompt_answer", map[string]any{"provider": "anthropic", "value": "c1"}))
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(home, ".go-code", "settings.json"))
		if err == nil {
			var outer struct {
				Provider map[string]map[string]any `json:"provider"`
			}
			_ = json.Unmarshal(raw, &outer)
			if outer.Provider["anthropic"]["auth_type"] == "oauth" {
				return // 成功
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("settings.json auth_type 未持久化为 oauth")
}
