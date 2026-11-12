package main

import (
	"context"
	"errors"
	"testing"

	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/seven7628/hai-harness/plugin"
)

// fakeBrowserCap 实现 plugin.Capability + browserExt（CallRaw），web tab 命令测试用。
type fakeBrowserCap struct {
	*fakePluginCap
	callRaw     func(ctx context.Context, tool string, args map[string]any) (*gomcp.CallToolResult, error)
	snap        map[string]any                  // 可选：browser_status 分层测试（nil=不提供 StatusSnapshot）
	refreshConn func(ctx context.Context) error // 可选：transport-closed 重连测试（nil=不实现 RefreshConn 语义）
}

func (f *fakeBrowserCap) CallRaw(ctx context.Context, tool string, args map[string]any) (*gomcp.CallToolResult, error) {
	return f.callRaw(ctx, tool, args)
}

func (f *fakeBrowserCap) StatusSnapshot() map[string]any { return f.snap }

func (f *fakeBrowserCap) RefreshConn(ctx context.Context) error {
	if f.refreshConn != nil {
		return f.refreshConn(ctx)
	}
	return nil // 无自定义重连：视为成功（接口命中但无副作用）
}

func TestExtractPages(t *testing.T) {
	res := &gomcp.CallToolResult{StructuredContent: map[string]any{
		"pages": []any{
			map[string]any{"id": float64(2), "url": "https://a.com", "title": "A", "selected": true},
			map[string]any{"id": float64(3), "url": "https://b.com", "title": "B"},
		},
	}}
	got := extractPages(res)
	if len(got) != 2 || got[0]["id"] != float64(2) || got[0]["title"] != "A" || got[0]["selected"] != true {
		t.Fatalf("extractPages = %v", got)
	}
	// 无结构化内容 → nil
	if extractPages(&gomcp.CallToolResult{}) != nil {
		t.Errorf("空结果应 nil")
	}
}

func TestExtractImage(t *testing.T) {
	res := &gomcp.CallToolResult{Content: []gomcp.Content{gomcp.NewImageContent("QUJD", "image/png")}}
	data, mime, ok := extractImage(res)
	if !ok || data != "QUJD" || mime != "image/png" {
		t.Fatalf("extractImage = %q/%q/%v", data, mime, ok)
	}
	if _, _, ok := extractImage(&gomcp.CallToolResult{}); ok {
		t.Errorf("无图片应 false")
	}
}

// injectBrowserWS 注入带 fakeBrowserCap 的工作区。
func injectBrowserWS(m *manager, ws string, f *fakeBrowserCap) {
	injectWS(m, ws, plugin.NewRegistry(f))
}

func TestBrowserStatusCommand(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	fake := &fakeBrowserCap{
		fakePluginCap: &fakePluginCap{id: "browser", tool: fakeBridgeTool{name: "browser_list_pages"}},
	}
	injectBrowserWS(m, ws, fake)
	if _, err := m.runtime(ws).plugins.Enable(context.Background(), "browser"); err != nil {
		t.Fatal(err)
	}

	m.dispatch(cmd(1, "browser_status", map[string]any{"workspace": ws}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("browser_status 应 ok: %v", resp)
	}
	d := resp["data"].(map[string]any)
	if d["status"] != "enabled" || d["tool_count"] != float64(1) {
		t.Errorf("browser_status = %v", d)
	}
}

func TestBrowserPagesCommand(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	fake := &fakeBrowserCap{
		fakePluginCap: &fakePluginCap{id: "browser"},
		callRaw: func(_ context.Context, tool string, _ map[string]any) (*gomcp.CallToolResult, error) {
			return &gomcp.CallToolResult{StructuredContent: map[string]any{
				"pages": []any{map[string]any{"id": float64(1), "url": "https://x.com", "title": "X"}},
			}}, nil
		},
	}
	injectBrowserWS(m, ws, fake)

	m.dispatch(cmd(2, "browser_pages", map[string]any{"workspace": ws}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("browser_pages 应 ok: %v", resp)
	}
	pages, _ := resp["data"].(map[string]any)["pages"].([]any)
	if len(pages) != 1 || pages[0].(map[string]any)["url"] != "https://x.com" {
		t.Errorf("pages = %v", resp["data"])
	}
}

func TestBrowserScreenshotCommand(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	fake := &fakeBrowserCap{
		fakePluginCap: &fakePluginCap{id: "browser"},
		callRaw: func(_ context.Context, tool string, args map[string]any) (*gomcp.CallToolResult, error) {
			if args["pageId"] != int64(2) {
				t.Errorf("pageId 应透传: %v", args)
			}
			return &gomcp.CallToolResult{Content: []gomcp.Content{gomcp.NewImageContent("QUJD", "image/png")}}, nil
		},
	}
	injectBrowserWS(m, ws, fake)

	m.dispatch(cmd(3, "browser_screenshot", map[string]any{"workspace": ws, "page_id": 2}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("browser_screenshot 应 ok: %v", resp)
	}
	img, _ := resp["data"].(map[string]any)["image"].(string)
	if img != "data:image/png;base64,QUJD" {
		t.Errorf("image = %q", img)
	}
}

func TestBrowserCloseCommand(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	fake := &fakeBrowserCap{
		fakePluginCap: &fakePluginCap{id: "browser"},
		callRaw: func(_ context.Context, tool string, args map[string]any) (*gomcp.CallToolResult, error) {
			switch tool {
			case "close_page":
				if args["pageId"] != int64(2) {
					t.Errorf("close_page pageId 应透传: %v", args)
				}
				return &gomcp.CallToolResult{}, nil
			case "list_pages": // 关闭成功后刷新页列表
				return &gomcp.CallToolResult{StructuredContent: map[string]any{
					"pages": []any{map[string]any{"id": float64(3), "url": "https://y.com", "title": "Y"}},
				}}, nil
			}
			return nil, errors.New("unexpected tool: " + tool)
		},
	}
	injectBrowserWS(m, ws, fake)

	m.dispatch(cmd(5, "browser_close", map[string]any{"workspace": ws, "page_id": 2}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("browser_close 应 ok: %v", resp)
	}
	pages, _ := resp["data"].(map[string]any)["pages"].([]any)
	if len(pages) != 1 || pages[0].(map[string]any)["url"] != "https://y.com" {
		t.Errorf("关闭后应返回刷新后的页列表: %v", resp["data"])
	}
}

func TestBrowserCommandErrors(t *testing.T) {
	// 缺 workspace
	m, buf := testManager(t)
	m.dispatch(cmd(1, "browser_status", map[string]any{}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != false {
		t.Fatalf("缺 workspace 应报错: %v", resp)
	}
	// 插件不存在（默认 registry 未启用也应有 browser 能力，但这里用空 registry 模拟缺失）
	m2, buf2 := testManager(t)
	ws := t.TempDir()
	injectWS(m2, ws, plugin.NewRegistry()) // 空 registry
	m2.dispatch(cmd(2, "browser_status", map[string]any{"workspace": ws}))
	if resp, ok := lastResp(t, buf2); !ok || resp["ok"] != false {
		t.Fatalf("缺 browser 插件应报错: %v", resp)
	}
	// 原始调用失败透传
	m3, buf3 := testManager(t)
	ws3 := t.TempDir()
	fake := &fakeBrowserCap{
		fakePluginCap: &fakePluginCap{id: "browser"},
		callRaw: func(context.Context, string, map[string]any) (*gomcp.CallToolResult, error) {
			return nil, errors.New("boom")
		},
	}
	injectBrowserWS(m3, ws3, fake)
	m3.dispatch(cmd(3, "browser_pages", map[string]any{"workspace": ws3}))
	if resp, ok := lastResp(t, buf3); !ok || resp["ok"] != false {
		t.Fatalf("调用失败应报错: %v", resp)
	}
}

// TestBrowserStatusCommandLayered（§3.4）：browser_status 返回分层结构（mcp 连接阶段）。
func TestBrowserStatusCommandLayered(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	fake := &fakeBrowserCap{
		fakePluginCap: &fakePluginCap{id: "browser", tool: fakeBridgeTool{name: "browser_list_pages"}},
		snap: map[string]any{
			"mcp": map[string]any{"state": "connected", "phase": "connected", "tool_count": 25, "error_code": ""},
		},
	}
	injectBrowserWS(m, ws, fake)
	if _, err := m.runtime(ws).plugins.Enable(context.Background(), "browser"); err != nil {
		t.Fatal(err)
	}

	m.dispatch(cmd(2, "browser_status", map[string]any{"workspace": ws}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("browser_status 应 ok: %v", resp)
	}
	d := resp["data"].(map[string]any)
	// 旧字段保留
	if d["status"] != "enabled" || d["tool_count"] != float64(1) {
		t.Errorf("旧字段缺失: %v", d)
	}
	// 新分层字段（enabled 来自 IsEnabled()，fake 未实现 → false；存在即可）
	if _, ok := d["enabled"]; !ok {
		t.Errorf("enabled 分层缺失: %v", d)
	}
	if d["installed"] != true {
		t.Errorf("installed 分层缺失: %v", d)
	}
	mcp, _ := d["mcp"].(map[string]any)
	if mcp == nil || mcp["state"] != "connected" || mcp["phase"] != "connected" {
		t.Errorf("mcp 分层缺失: %v", d["mcp"])
	}
	// provider：Browser Use 不依赖 Provider key
	prov, _ := d["provider"].(map[string]any)
	if prov == nil || prov["required"] != false {
		t.Errorf("provider 分层缺失: %v", d["provider"])
	}
}

// TestBrowserPagesTransportClosedRetry（§15.1）：CallRaw 报 transport closed → RefreshConn → 重发成功。
func TestBrowserPagesTransportClosedRetry(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	first := true
	refreshCalls := 0
	fake := &fakeBrowserCap{
		fakePluginCap: &fakePluginCap{id: "browser"},
		callRaw: func(_ context.Context, tool string, _ map[string]any) (*gomcp.CallToolResult, error) {
			if first && tool == "list_pages" {
				first = false
				return nil, errors.New("transport error: transport closed")
			}
			return &gomcp.CallToolResult{StructuredContent: map[string]any{
				"pages": []any{map[string]any{"id": float64(1), "url": "https://x.com"}},
			}}, nil
		},
		refreshConn: func(_ context.Context) error { refreshCalls++; return nil },
	}
	injectBrowserWS(m, ws, fake)
	m.dispatch(cmd(3, "browser_pages", map[string]any{"workspace": ws}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("transport closed 应重连重发成功: %v", resp)
	}
	if refreshCalls != 1 {
		t.Errorf("RefreshConn 应调用 1 次: %d", refreshCalls)
	}
	pages, _ := resp["data"].(map[string]any)["pages"].([]any)
	if len(pages) != 1 {
		t.Errorf("重发后应返回页列表: %v", resp["data"])
	}
}
