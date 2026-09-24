package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

// CallbackServer 一次性 loopback OAuth 回调服务器（对齐 pi oauth/anthropic.ts startCallbackServer）。
// 监听 127.0.0.1 随机端口，路径固定 /callback；校验 state 后返回成功 HTML。
// 与 manual_code 手动粘贴通道竞争：用户可把重定向 URL 粘到对话框（由上层 Flow 编排）。
type CallbackServer struct {
	server      *http.Server
	RedirectURI string // http://127.0.0.1:<port>/callback

	mu         sync.Mutex
	settled    bool
	waitCh     chan callbackResult
	cancelCh   chan struct{}
	cancelOnce sync.Once
}

type callbackResult struct {
	code  string
	state string
	err   error
}

// NewCallbackServer 启动回调服务器。expectedState 用于校验回调 state（= PKCE verifier）。
// ctx 取消时服务器自动关闭。
func NewCallbackServer(ctx context.Context, expectedState string) (*CallbackServer, error) {
	host := getCallbackHost()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0")) // 随机端口，避免 pi 固定端口的冲突
	if err != nil {
		return nil, fmt.Errorf("oauth callback listen: %w", err)
	}

	cs := &CallbackServer{
		waitCh:   make(chan callbackResult, 1),
		cancelCh: make(chan struct{}),
	}
	port := ln.Addr().(*net.TCPAddr).Port
	cs.RedirectURI = fmt.Sprintf("http://%s:%d/callback", host, port)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errParam := q.Get("error"); errParam != "" {
			cs.settle(callbackResult{err: fmt.Errorf("oauth authorization error: %s", errParam)})
			writeHTML(w, http.StatusBadRequest, oauthErrorHTML("Authentication did not complete.", "Error: "+errParam))
			return
		}
		code := q.Get("code")
		state := q.Get("state")
		if code == "" || state == "" {
			cs.settle(callbackResult{err: errors.New("missing code or state parameter")})
			writeHTML(w, http.StatusBadRequest, oauthErrorHTML("Missing code or state parameter.", ""))
			return
		}
		if state != expectedState {
			cs.settle(callbackResult{err: errors.New("oauth state mismatch")})
			writeHTML(w, http.StatusBadRequest, oauthErrorHTML("State mismatch.", ""))
			return
		}
		cs.settle(callbackResult{code: code, state: state})
		writeHTML(w, http.StatusOK, oauthSuccessHTML("Authentication completed. You can close this window."))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeHTML(w, http.StatusNotFound, oauthErrorHTML("Callback route not found.", ""))
	})

	cs.server = &http.Server{Handler: mux}
	go func() {
		// Serve 错误（监听被占用后失效/异常退出）上报到 serveErr，Close 时返回——
		// 不能让回调服务器静默死掉（用户会一直等不到回调）。
		if err := cs.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cs.settle(callbackResult{err: fmt.Errorf("oauth callback server: %w", err)})
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			cs.settle(callbackResult{err: ErrCancelled})
			cs.Close()
		case <-cs.cancelCh:
		}
	}()
	return cs, nil
}

// WaitCode 阻塞直到收到 code+state、被 Cancel 或 ctx 取消。
// 返回 (code, state, nil)；取消返回 ErrCancelled。
func (cs *CallbackServer) WaitCode() (string, string, error) {
	res := <-cs.waitCh
	if res.err != nil {
		return "", "", res.err
	}
	return res.code, res.state, nil
}

// Cancel 停止等待（manual 通道接管时调用；对齐 pi cancelWait）。
func (cs *CallbackServer) Cancel() {
	cs.settle(callbackResult{err: ErrCancelled})
}

// Close 关闭服务器（幂等）。
func (cs *CallbackServer) Close() {
	cs.cancelOnce.Do(func() { close(cs.cancelCh) })
	if cs.server != nil {
		_ = cs.server.Close()
	}
}

func (cs *CallbackServer) settle(res callbackResult) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.settled {
		return
	}
	cs.settled = true
	cs.waitCh <- res
}

// CallbackHostEnv 覆盖回调 host 的环境变量（pi PI_OAUTH_CALLBACK_HOST 同名对齐）。
const CallbackHostEnv = "PI_OAUTH_CALLBACK_HOST"

// getCallbackHost 回调监听 host：默认 127.0.0.1；env 覆盖仅接受 loopback 地址
// （127.0.0.1/localhost/::1）——回调服务器承载 OAuth code，监听非 loopback 会被
// 本机其他进程/网络嗅探，属安全边界（V2 SECURITY 观察项）。非法值回退默认。
func getCallbackHost() string {
	if h := os.Getenv(CallbackHostEnv); h != "" {
		switch strings.ToLower(h) {
		case "127.0.0.1", "localhost", "::1":
			return h
		default:
			// 非 loopback 覆盖：拒绝并回退 127.0.0.1（避免 OAuth code 暴露到非回环接口）
			return "127.0.0.1"
		}
	}
	return "127.0.0.1"
}

// ParseAuthorizationInput 解析 manual_code 输入（对齐 pi parseAuthorizationInput）：
// 支持完整 URL（取 code/state 参数）、"code#state"、query 字符串、裸 code。
func ParseAuthorizationInput(input string) (code, state string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ""
	}
	if u, err := url.Parse(input); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Query().Get("code"), u.Query().Get("state")
	}
	if i := strings.IndexByte(input, '#'); i >= 0 {
		return input[:i], input[i+1:]
	}
	if strings.Contains(input, "code=") {
		if q, err := url.ParseQuery(input); err == nil {
			return q.Get("code"), q.Get("state")
		}
	}
	return input, ""
}

func writeHTML(w http.ResponseWriter, status int, html string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(html))
}

func oauthSuccessHTML(message string) string {
	return "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>Success</title></head>" +
		"<body style=\"font-family:system-ui;display:flex;align-items:center;justify-content:center;height:100vh;margin:0\">" +
		"<div style=\"text-align:center\"><h2 style=\"color:#16a34a\">✓ " + escapeHTML(message) + "</h2>" +
		"<p>You can close this window now.</p></div></body></html>"
}

func oauthErrorHTML(title, detail string) string {
	body := "<h2 style=\"color:#dc2626\">✗ " + escapeHTML(title) + "</h2>"
	if detail != "" {
		body += "<p>" + escapeHTML(detail) + "</p>"
	}
	return "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>Error</title></head>" +
		"<body style=\"font-family:system-ui;display:flex;align-items:center;justify-content:center;height:100vh;margin:0\">" +
		"<div style=\"text-align:center\">" + body + "</div></body></html>"
}

func escapeHTML(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&':
			b = append(b, "&amp;"...)
		case '<':
			b = append(b, "&lt;"...)
		case '>':
			b = append(b, "&gt;"...)
		case '"':
			b = append(b, "&quot;"...)
		default:
			b = append(b, s[i])
		}
	}
	return string(b)
}
