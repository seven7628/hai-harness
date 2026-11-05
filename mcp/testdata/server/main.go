// 端到端测试用 MCP 服务器 fixture（stdio 子进程）：
// 由 mcp/stdio_e2e_test.go 用 `go build` 编译成可执行文件，作为真实 stdio 传输的子进程。
package main

import (
	"context"
	"fmt"
	"os"

	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	srv := server.NewMCPServer("e2e-server", "1.0.0", server.WithToolCapabilities(true))
	srv.AddTool(
		gomcp.NewTool("echo",
			gomcp.WithDescription("回显文本"),
			gomcp.WithReadOnlyHintAnnotation(true),
			gomcp.WithString("text", gomcp.Required())),
		func(_ context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			text, _ := req.GetArguments()["text"].(string)
			return &gomcp.CallToolResult{Content: []gomcp.Content{gomcp.NewTextContent("echo: " + text)}}, nil
		},
	)
	if err := server.NewStdioServer(srv).Listen(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
