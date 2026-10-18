package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// mcp_http_fixture 一个**需要 Authorization 请求头**的真实 Streamable HTTP MCP 服务器
// （手工联调/取证用：curl 或 mcp 客户端都能打）。
//   - 携带 Authorization: Bearer <token> → 正常服务（工具 whoami）
//   - 缺头/头错 → 401 + WWW-Authenticate
//
// 用法：go run ./desktop/bridge/cmd/mcp_http_fixture
// 输出监听地址（默认 :18081，可用 PORT 覆盖；token 可用 TOKEN 覆盖）。
//
// 配合面板验证「请求头填对就通、删掉就 401」：在 MCP 页把 headers 配成
// {"Authorization":"Bearer demo-token"} 指向 http://127.0.0.1:18081/mcp 即可。
func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "18081"
	}
	token := os.Getenv("TOKEN")
	if token == "" {
		token = "demo-token"
	}
	srv := server.NewMCPServer("authed-fixture", "1.0.0", server.WithToolCapabilities(true))
	srv.AddTool(
		mcp.NewTool("whoami", mcp.WithDescription("回显通过鉴权的调用方标识")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent("authenticated")}}, nil
		},
	)
	inner := server.NewStreamableHTTPServer(srv)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
	fmt.Printf("需要 Authorization: Bearer %s 的 MCP streamable-http 服务器监听于 :%s/mcp\n", token, port)
	if err := http.ListenAndServe(":"+port, handler); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
