// mcp_health.go —— 工作区 MCP 连接的低频探活（"放着不用也能自愈"那条通道）。
//
// 为什么需要两条通道（缺一不可）：
//   - 用一次就自愈：mcp.Server.callTool 失败 → markBroken → 下一次调用重拨。延迟最低，
//     但只覆盖"真的被调用过"的服务器。
//   - 闲着也自愈：本文件。每 mcpHealthInterval 对**已连接**的服务器 Ping 一次，失败即
//     Refresh（Close + 重连）。覆盖"放着不用、服务端先重启/回收"的形态 —— 用户下次打开
//     面板或发起对话时连接已经是好的，不必先吃一次失败再等模型自己反应过来。
//
// 只在服务器**已连接**时探活（Manager.WatchHealth 内部判定）：没人用过的 server 不该被
// 探活顺手 dial 出来（stdio 会 spawn 子进程），"惰性连接"这条设计不能被探活破坏。
//
// 生命周期：随工作区运行态创建启动（create → startMCPHealth），delete_workspace /
// shutdownAll 停止（与配置监听 mcp_watch.go 同规矩）。注意 shutdown_workspace（只关会话
// 和连接、工作区仍在册）**不**停：Close 之后所有服务器回到 idle，探活自然空转，用户再
// 用时不必重新拉起 goroutine。
package main

import (
	"context"
	"time"
)

// mcpHealthInterval 探活周期。比一轮对话短（撞上死连接的概率可忽略），又远大于服务端
// 重启的时间尺度，面板里零噪声。
const mcpHealthInterval = 60 * time.Second

// startMCPHealth 启动该工作区的探活 goroutine（幂等：已在跑则直接返回）。
func (m *manager) startMCPHealth(ws *workspaceRuntime) {
	if ws == nil || ws.mcp == nil {
		return
	}
	ws.mu.Lock()
	if ws.mcpHealthCancel != nil {
		ws.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	ws.mcpHealthCancel = cancel
	ws.mcpHealthDone = done
	ws.mu.Unlock()
	// ctx/done/mgr 走局部变量：stop 会把字段置 nil，goroutine 内再读字段有竞态
	// （mcp_watch.go 里同一类崩溃踩过一次）。
	mgr := ws.mcp
	go func() {
		defer close(done)
		mgr.WatchHealth(ctx, mcpHealthInterval)
	}()
}

// stopMCPHealth 停止探活并等 goroutine 退出（幂等）。**调用方不得持 ws.mu/m.mu**：
// 探活里的 Refresh 会取 Manager 锁与 Server 锁，持锁等待会死锁。
func (ws *workspaceRuntime) stopMCPHealth() {
	ws.mu.Lock()
	cancel, done := ws.mcpHealthCancel, ws.mcpHealthDone
	ws.mcpHealthCancel, ws.mcpHealthDone = nil, nil
	ws.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done // 等这一轮探活收尾（避免停止后仍在改连接状态）
}
