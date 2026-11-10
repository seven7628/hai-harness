// mobile_hub_web_test.go —— 手机 Hub 静态资源契约（P4-03b）。
//
// 手机页面没有构建步骤，index.html 与 app.js/hub_model.js/avatar_model.js 的加载顺序、命令名、
// 事件分流都是「运行时接口」。这里用 embed FS + 本地 HTTP 各验一遍：
//   - 发布包不能漏掉 hub_model.js / avatar_model.js（漏了页面直接白屏）；
//   - /m/ 必须真的把脚本以 no-store 发出去；
//   - app.js 必须用 scope=hub 的独立连接、走 hub_* 命令，且不拼 HTML。
package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func readMobileWebFile(t *testing.T, name string) string {
	t.Helper()
	b, err := mobileWebFS.ReadFile("mobile_web/" + name)
	if err != nil {
		t.Fatalf("embed 缺少 mobile_web/%s: %v", name, err)
	}
	return string(b)
}

func TestMobileHubWebAssetsEmbedded(t *testing.T) {
	index := readMobileWebFile(t, "index.html")
	app := readMobileWebFile(t, "app.js")
	model := readMobileWebFile(t, "hub_model.js")
	avatar := readMobileWebFile(t, "avatar_model.js")

	// 脚本顺序：纯逻辑层先定义 window.HaiMobileHubModel / HaiMobileAvatar，app.js 才能消费。
	modelAt := strings.Index(index, `<script src="hub_model.js"></script>`)
	avatarAt := strings.Index(index, `<script src="avatar_model.js"></script>`)
	appAt := strings.Index(index, `<script src="app.js"></script>`)
	if modelAt < 0 || avatarAt < 0 || appAt < 0 || modelAt > avatarAt || avatarAt > appAt {
		t.Fatalf("index.html 必须先加载 hub_model.js / avatar_model.js 再加载 app.js（model=%d avatar=%d app=%d）", modelAt, avatarAt, appAt)
	}
	for _, marker := range []string{
		"screen-hub", "screen-hub-group", "screen-hub-dm", "open-hub",
		"hub-sheet", "mention-picker",
	} {
		if !strings.Contains(index, marker) {
			t.Errorf("index.html 缺少 Hub 页面标记 %q", marker)
		}
	}
	if !strings.Contains(model, "HaiMobileHubModel") {
		t.Error("hub_model.js 必须挂到 window.HaiMobileHubModel")
	}
	if !strings.Contains(avatar, "HaiMobileAvatar") || !strings.Contains(avatar, "deriveSigil") || !strings.Contains(avatar, "planStack") {
		t.Error("avatar_model.js 必须挂到 window.HaiMobileAvatar 并提供 deriveSigil/planStack")
	}

	// 独立 Hub 事件流 + 命令名：P4-03a 开放的手机协议要真的被页面用上。
	for _, marker := range []string{
		"scope=hub",
		"eventRoute(ev, 'chat')",
		"eventRoute(ev, 'hub')",
		"hubResyncTargets",
		"hub_agent_list",
		"hub_group_list",
		"hub_group_get",
		"hub_group_history",
		"hub_open",
		"hub_history",
		"hub_send",
		"hub_group_send",
		"hub_group_assign",
		"hub_conversation_stop",
		"hub_task_list",
		"hub_need_list",
		"hub_need_resolve",
		"hub_mark_read",
		"hub_search",
		"hub_message_delta",
	} {
		if !strings.Contains(app, marker) {
			t.Errorf("app.js 缺少 Hub 契约 %q", marker)
		}
	}
	// Hub resync 只能重拉快照，不能复用会话的 openSession(state.sid) 分支。
	resyncAt := strings.Index(app, "async function hubResync")
	resyncEnd := strings.Index(app, "function onHubEvent")
	if resyncAt < 0 || resyncEnd <= resyncAt {
		t.Fatal("app.js 缺少 hubResync 实现")
	}
	if strings.Contains(app[resyncAt:resyncEnd], "openSession") {
		t.Error("Hub resync 分支不能调用 openSession")
	}

	// 不拼 HTML：消息正文一律 textContent/createElement（XSS 底线）。
	for _, file := range []struct{ name, body string }{
		{"app.js", app},
		{"hub_model.js", model},
		{"avatar_model.js", avatar},
	} {
		if strings.Contains(file.body, ".innerHTML") {
			t.Errorf("%s 使用了 innerHTML，Hub 正文必须走 textContent", file.name)
		}
		for _, bad := range []string{"insertAdjacentHTML", "document.write("} {
			if strings.Contains(file.body, bad) {
				t.Errorf("%s 使用了 %s", file.name, bad)
			}
		}
	}
}

func TestMobileHubWebAssetsServed(t *testing.T) {
	_, ts := testMobileServer(t)
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/m/", "screen-hub"},
		{"/m/hub_model.js", "HaiMobileHubModel"},
		{"/m/avatar_model.js", "HaiMobileAvatar"},
		{"/m/app.js", "scope=hub"},
	} {
		r, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", tc.path, r.StatusCode, body)
		}
		if !strings.Contains(string(body), tc.want) {
			t.Fatalf("GET %s 内容缺少 %q", tc.path, tc.want)
		}
		if cc := r.Header.Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("GET %s Cache-Control = %q，期望 no-store", tc.path, cc)
		}
	}
}
