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
	for _, cmd := range []string{"delete_session", "mcp_set", "skill_install", "switch_model"} {
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
