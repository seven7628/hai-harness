package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// —— 本文件锁住 headers（请求头）在 bridge 侧的三件事 ——
//
// 需求原文：「mcp http 模式不支持 Header 设置，导致链接无法成功」。内核（mcp 包）
// 已把 headers 送到传输上（见 mcp/headers_test.go 的端到端断言）；bridge 侧还会
// 以两种方式把它弄丢，而且都是**静默**的：
//   1. mcpServerKnownKeys 漏登记 → 保存时「未识别字段」逻辑用文件旧值覆盖回，
//      用户改了请求头却保存无效；
//   2. 项目视图不带 headers 字段 → 面板回显为空，一编辑保存就把用户的头清掉。
// 这两条都不会报错，只会让功能「看着有、其实不生效」，故各钉一个测试。

// TestMCPProjectSetKeepsHeaders 保存（写盘）与回读（项目视图）都带上 headers。
func TestMCPProjectSetKeepsHeaders(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	m.dispatch(cmd(1, "mcp_project_set", map[string]any{"workspace": ws, "servers": map[string]any{
		"remote": map[string]any{
			"type": "http", "url": "http://192.168.160.65:18000/mcp",
			"headers": map[string]any{"Authorization": "Bearer ct_MjQ0NTc5MTk4MzA", "X-Tenant": "acme"},
		},
	}}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("保存应通过: %v", resp)
	}

	// 1) 落盘：plaintext 写进 settings.json（与 env 同一决策）
	raw, err := os.ReadFile(filepath.Join(ws, ".go-code", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	srv := file["mcpServers"].(map[string]any)["remote"].(map[string]any)
	hdr, _ := srv["headers"].(map[string]any)
	if hdr == nil || hdr["Authorization"] != "Bearer ct_MjQ0NTc5MTk4MzA" || hdr["X-Tenant"] != "acme" {
		t.Fatalf("headers 未落盘: %v", srv)
	}

	// 2) 回读：项目视图必须带 headers（不带 → 面板空着，一保存就把用户的头清掉）
	projects := mcpProjects(t, m, buf, 2, []string{ws})
	row := mcpProjectServers(t, projects[ws])["remote"]
	if row == nil {
		t.Fatalf("项目服务器应可见: %v", projects[ws])
	}
	got, _ := row["headers"].(map[string]any)
	if got == nil || got["Authorization"] != "Bearer ct_MjQ0NTc5MTk4MzA" {
		t.Fatalf("项目视图的 headers 丢失: %v", row)
	}
	// url/type 也要如实回显（面板编辑回填依赖它们）
	if row["type"] != "http" || row["url"] != "http://192.168.160.65:18000/mcp" {
		t.Fatalf("type/url 回显不对: %v", row)
	}
}

// TestMCPProjectSetHeadersNotReverted 修改请求头后保存 → 新值生效（而不是被文件旧值覆盖回）。
// 这正是 mcpServerKnownKeys 漏登记 headers 时的表现：保存成功、值却是旧的。
func TestMCPProjectSetHeadersNotReverted(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	path := filepath.Join(ws, ".go-code", "settings.json")
	writeMCPConfig(t, path, `{"remote": {"type": "http", "url": "http://x/mcp", "headers": {"Authorization": "Bearer old"}}}`)

	// 面板读到的是旧值，用户改成新值并保存
	m.dispatch(cmd(1, "mcp_project_set", map[string]any{"workspace": ws, "servers": map[string]any{
		"remote": map[string]any{
			"type": "http", "url": "http://x/mcp",
			"headers": map[string]any{"Authorization": "Bearer new"},
		},
	}}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("保存应通过: %v", resp)
	}
	file := jsonFile(t, path)
	srv := file["mcpServers"].(map[string]any)["remote"].(map[string]any)
	hdr := srv["headers"].(map[string]any)
	if hdr["Authorization"] != "Bearer new" {
		t.Fatalf("请求头应更新为 new（被旧值覆盖回 = known keys 漏登记）: %v", hdr)
	}
}

// TestMCPProjectSetRemovingHeadersClearsThem 面板里删掉全部请求头 → 保存后真的清空。
// 「模型认识的键一律以模型为准」：清空和新增一样必须生效。
func TestMCPProjectSetRemovingHeadersClearsThem(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	path := filepath.Join(ws, ".go-code", "settings.json")
	writeMCPConfig(t, path, `{"remote": {"type": "http", "url": "http://x/mcp", "headers": {"Authorization": "Bearer old"}}}`)

	m.dispatch(cmd(1, "mcp_project_set", map[string]any{"workspace": ws, "servers": map[string]any{
		"remote": map[string]any{"type": "http", "url": "http://x/mcp"},
	}}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("保存应通过: %v", resp)
	}
	file := jsonFile(t, path)
	srv := file["mcpServers"].(map[string]any)["remote"].(map[string]any)
	if _, has := srv["headers"]; has {
		t.Fatalf("空请求头应被清掉: %v", srv)
	}
}

// TestMCPProjectSetKeepsUnknownFieldsButNotKnown 未识别字段（第三方/未来字段）按名保留，
// 而 headers 现在属于「认识的键」——两条一起验，防止把保留逻辑放宽/收严弄反。
func TestMCPProjectSetKeepsUnknownFieldsButNotKnown(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	path := filepath.Join(ws, ".go-code", "settings.json")
	writeMCPConfig(t, path, `{"remote": {"type": "http", "url": "http://x/mcp", "cwd": "/tmp/keep-me"}}`)

	m.dispatch(cmd(1, "mcp_project_set", map[string]any{"workspace": ws, "servers": map[string]any{
		"remote": map[string]any{"type": "http", "url": "http://y/mcp", "headers": map[string]any{"X-A": "1"}},
	}}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("保存应通过: %v", resp)
	}
	srv := jsonFile(t, path)["mcpServers"].(map[string]any)["remote"].(map[string]any)
	if srv["cwd"] != "/tmp/keep-me" {
		t.Fatalf("未识别字段 cwd 应保留: %v", srv)
	}
	if srv["url"] != "http://y/mcp" {
		t.Fatalf("认识的键应以面板为准: %v", srv)
	}
	if hdr, _ := srv["headers"].(map[string]any); hdr == nil || hdr["X-A"] != "1" {
		t.Fatalf("新增的 headers 应写入: %v", srv)
	}
}

// TestMCPProjectSetAcceptsStreamableAlias 别名归一：streamable-http / streamable_http
// 都能保存（不会当成未知类型被拒），且 loadLayers 后读回来是 http。
func TestMCPProjectSetAcceptsStreamableAlias(t *testing.T) {
	for _, typ := range []string{"streamable_http", "streamable-http", "STREAMABLE_HTTP"} {
		m, buf := testManager(t)
		ws := t.TempDir()
		m.dispatch(cmd(1, "mcp_project_set", map[string]any{"workspace": ws, "servers": map[string]any{
			"remote": map[string]any{"type": typ, "url": "http://x/mcp", "headers": map[string]any{"Authorization": "Bearer t"}},
		}}))
		resp, _ := lastResp(t, buf)
		if resp["ok"] != true {
			t.Fatalf("type %q 应被接受（别名）: %v", typ, resp)
		}
		row := mcpProjectServers(t, mcpProjects(t, m, buf, 2, []string{ws})[ws])["remote"]
		if row["type"] != "http" {
			t.Fatalf("type %q 归一后应为 http, got %v", typ, row["type"])
		}
		if row["headers"] == nil {
			t.Fatalf("type %q 的 headers 应保留: %v", typ, row)
		}
	}
}

// TestMCPProjectUnknownTypeStillRejected 归一没有放宽到「什么都收」：真未知类型仍拒绝。
func TestMCPProjectUnknownTypeStillRejected(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	m.dispatch(cmd(1, "mcp_project_set", map[string]any{"workspace": ws, "servers": map[string]any{
		"bad": map[string]any{"type": "websocket", "url": "ws://x/mcp"},
	}}))
	if resp, _ := lastResp(t, buf); resp["ok"] != false {
		t.Fatalf("未知类型应拒绝保存: %v", resp)
	}
	if _, err := os.Stat(filepath.Join(ws, ".go-code", "settings.json")); err == nil {
		t.Fatal("被拒的保存不该写盘")
	}
}
