package mcp

import (
	"context"
	"sort"
)

// MCP server instructions（自述使用策略）的收集与暴露。
//
// 背景：MCP 的 initialize 响应可携带 instructions 字段（服务器告诉客户端"这些工具该
// 怎么用、什么时候用"）。Claude Code / Codex 等宿主会把它注入系统提示词；本仓库早期
// 版本在 mcp/server.go 里把它丢掉了（`if _, err := c.Initialize(...)`），于是模型只
// 看到一堆工具名，缺少"选哪个、别链式调用"这类关键引导。
//
// 用法（宿主侧，一行）：
//
//	for name, txt := range mgr.Instructions(ctx) { /* 注入系统提示词的一个小节 */ }
//
// 注意：Instructions 会**触发惰性连接**（与 Tools 一致）。未连接/无 instructions 的
// 服务器会被跳过，因此调用方可以无条件调用。

// Instructions 返回 serverName → instructions（仅非空项；按名字排序稳定）。
// ctx 用于连接与 initialize；并发安全（Manager 内部按服务器加锁）。
func (m *Manager) Instructions(ctx context.Context) map[string]string {
	if m == nil {
		return nil
	}
	out := map[string]string{}
	for _, s := range m.enabledServers() {
		if txt := s.Instructions(ctx); txt != "" {
			out[s.name] = txt
		}
	}
	return out
}

// Instructions 返回该服务器的 instructions（空串 = 无/未连接）。
// 会触发 EnsureConnected（惰性连接，与 Tools 语义一致）。
func (s *Server) Instructions(ctx context.Context) string {
	if s == nil {
		return ""
	}
	if err := s.EnsureConnected(ctx); err != nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.instructions
}

// InstructionsText 把多个服务器的 instructions 渲染成一个可直接拼进系统提示词的文本块
// （按服务器名排序，保证字节稳定 → provider 前缀缓存友好）。空 map 返回空串。
func InstructionsText(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)

	var b []byte
	for _, n := range names {
		b = append(b, ("### " + n + "\n")...)
		b = append(b, m[n]...)
		b = append(b, '\n', '\n')
	}
	// 去掉尾部多出的一个空行，避免与调用方的分隔逻辑叠加
	return string(b[:len(b)-1])
}
