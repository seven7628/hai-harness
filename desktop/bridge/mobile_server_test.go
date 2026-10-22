package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testMobileServer 用假的 dispatch 替身跑通协议，不碰真 manager。
func testMobileServer(t *testing.T) (*mobileServer, *httptest.Server) {
	t.Helper()
	s := &mobileServer{
		pair:    newMobilePairing(filepath.Join(t.TempDir(), "devices.json")),
		hub:     newMobileHub(),
		nextID:  1_000_000_000,
		pending: map[int]chan map[string]any{},
	}
	s.dispatch = func(c command) {
		s.tap(map[string]any{
			"event_type": "command_response",
			"id":         c.Id,
			"ok":         true,
			"data":       map[string]any{"echo": c.Type.String(), "workspace": str(c.Payload, "workspace")},
		})
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestMobilePingIsOpen(t *testing.T) {
	_, ts := testMobileServer(t)
	r, err := http.Get(ts.URL + "/api/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("ping = %d", r.StatusCode)
	}
	body, _ := io.ReadAll(r.Body)
	if !bytes.Contains(body, []byte(`"app":"hai"`)) {
		t.Fatalf("ping body = %s", body)
	}
}

func TestMobileCmdRequiresToken(t *testing.T) {
	_, ts := testMobileServer(t)
	r := postJSON(t, ts.URL+"/api/cmd", map[string]any{"cmd": "list"}, "")
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 应为 401，得到 %d", r.StatusCode)
	}
}

func TestMobilePairThenCmd(t *testing.T) {
	s, ts := testMobileServer(t)
	code, _ := s.pair.newCode(time.Now())

	r := postJSON(t, ts.URL+"/api/pair", map[string]any{"code": code, "name": "iPhone"}, "")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("pair = %d: %s", r.StatusCode, b)
	}
	var pr struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&pr); err != nil || pr.Token == "" {
		t.Fatalf("token 解析失败：%v", err)
	}

	r2 := postJSON(t, ts.URL+"/api/cmd", map[string]any{
		"cmd": "list", "payload": map[string]any{"workspace": "/w"},
	}, pr.Token)
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(r2.Body)
		t.Fatalf("cmd = %d: %s", r2.StatusCode, b)
	}
	var resp struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r2.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Data["echo"] != "list" || resp.Data["workspace"] != "/w" {
		t.Fatalf("响应不符：%+v", resp)
	}
}

func TestMobileCmdAllowlistRejectsDangerous(t *testing.T) {
	s, ts := testMobileServer(t)
	code, _ := s.pair.newCode(time.Now())
	token, _, err := s.pair.pair(code, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// delete_session 现在**允许**（手机端会话管理，UI 有二次确认），
	// 这里只保留真正的越权面：改设置、装技能、放开监听、回填目录选择结果。
	for _, cmd := range []string{"mcp_set", "skill_install", "switch_model", "mobile_set_listen", "workspace_pick_result"} {
		r := postJSON(t, ts.URL+"/api/cmd", map[string]any{"cmd": cmd}, token)
		if r.StatusCode != http.StatusForbidden {
			r.Body.Close()
			t.Fatalf("%s 应被拒绝，得到 %d", cmd, r.StatusCode)
		}
		r.Body.Close()
	}
}

func TestMobileWebIsServed(t *testing.T) {
	_, ts := testMobileServer(t)
	r, err := http.Get(ts.URL + "/m/")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("/m/ = %d", r.StatusCode)
	}
	body, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(body), "HAI") {
		t.Fatal("首页内容不对")
	}
	if cc := r.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
}

func postJSON(t *testing.T, url string, body map[string]any, token string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMobilePairQROnlyForReachableURLs(t *testing.T) {
	// 回环地址手机扫了也打不开 → 不生成
	if got := mobilePairQR([]string{"http://127.0.0.1:47800/m/"}, "123456"); got != "" {
		t.Fatal("回环地址不应生成二维码")
	}
	if got := mobilePairQR(nil, "123456"); got != "" {
		t.Fatal("没有地址不应生成二维码")
	}
	got := mobilePairQR([]string{"http://127.0.0.1:47800/m/", "http://192.168.1.7:47800/m/"}, "123456")
	if !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("局域网地址应生成 PNG data URL，得到 %q", got)
	}
	if mobilePairQR([]string{"http://192.168.1.7:47800/m/"}, "") != "" {
		t.Fatal("没有配对码不应生成二维码")
	}
}

func TestWorkspacePickTableTTLAndReplay(t *testing.T) {
	p := newWorkspacePickTable()
	now := time.Now()

	id := p.begin(now)
	if id == "" {
		t.Fatal("request id 不应为空")
	}
	if !p.consume(id, now) {
		t.Fatal("首次回填应成功")
	}
	if p.consume(id, now) {
		t.Fatal("同一请求不应能被重放")
	}

	old := p.begin(now)
	if p.consume(old, now.Add(workspacePickTTL+time.Second)) {
		t.Fatal("过期请求不应被接受")
	}
	if p.consume("wp_missing", now) {
		t.Fatal("不存在的请求不应被接受")
	}
}

func TestMobileCmdAllowsSessionManagement(t *testing.T) {
	// 会话管理是手机端的正当能力（重命名/置顶/删除），但删除必须走 UI 二次确认。
	s, ts := testMobileServer(t)
	code, _ := s.pair.newCode(time.Now())
	token, _, err := s.pair.pair(code, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"session_rename", "session_pin", "delete_session", "workspace_update"} {
		r := postJSON(t, ts.URL+"/api/cmd", map[string]any{"cmd": cmd, "payload": map[string]any{}}, token)
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			t.Fatalf("%s 应被允许，得到 %d", cmd, r.StatusCode)
		}
		r.Body.Close()
	}
}

func TestPickPairURLPrefersTailscale(t *testing.T) {
	// 同 Wi-Fi 时两者都能用；在外面只有 100.x 能用 → 二维码必须选它
	got := pickPairURL([]string{"http://192.168.1.7:47800/m/", "http://100.101.102.103:47800/m/"})
	if got != "http://100.101.102.103:47800/m/" {
		t.Fatalf("应优先 Tailscale，得到 %q", got)
	}
	// 没有 Tailscale → 退回第一个非回环
	if got := pickPairURL([]string{"http://127.0.0.1:47800/m/", "http://10.0.0.5:47800/m/"}); got != "http://10.0.0.5:47800/m/" {
		t.Fatalf("应退回非回环地址，得到 %q", got)
	}
	// 只有回环 → 不编
	if got := pickPairURL([]string{"http://127.0.0.1:47800/m/"}); got != "" {
		t.Fatalf("只有回环时不应出码，得到 %q", got)
	}
}
