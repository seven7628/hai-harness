package cdp

import (
	"encoding/json"
)

// Message JSON-RPC 请求（chrome-devtools-mcp 经 ws 发来的浏览器级命令）。
// ID 为空表示通知（无需响应）。SessionID：flatten 模式页面级命令在**顶层**携带
// （Puppeteer 单 ws 直发），必须参与路由——此前丢弃导致页面级命令全被当浏览器级
// 处理（setAutoAttach 重发 attachToTarget 死循环 / Network.enable 走错 target）。
type Message struct {
	ID        *int64         `json:"id"`
	Method    string         `json:"method"`
	Params    map[string]any `json:"params"`
	SessionID string         `json:"sessionId"`
}

// HandleMessage 处理一条浏览器级 CDP JSON-RPC 请求，返回响应 JSON 字节。
// 通知（无 id）返回 nil（不回复）。错误映射为 JSON-RPC 错误响应。
// 纯逻辑：无 socket 依赖，可单测。
func HandleMessage(f *Forwarder, msg []byte) []byte {
	var req Message
	if err := json.Unmarshal(msg, &req); err != nil {
		return rpcErr(nil, -32700, "parse error: "+err.Error())
	}
	var res map[string]any
	var err error
	if req.SessionID != "" {
		res, err = f.HandleSession(req.SessionID, req.Method, req.Params)
	} else {
		res, err = f.Handle(req.Method, req.Params)
	}
	if req.ID == nil {
		return nil // 通知：执行但无需响应
	}
	if err != nil {
		if req.SessionID != "" {
			return rpcErrSess(req.ID, -32000, err.Error(), req.SessionID)
		}
		return rpcErr(req.ID, -32000, err.Error())
	}
	resp := map[string]any{"id": *req.ID, "result": res}
	// §1.8.0 实测补丁（关键）：**响应必须带 sessionId**（请求是 session 命令时）。
	// Puppeteer 的 CDPSession 按 sessionId 路由响应——不带则响应被丢（找不到对应
	// session）→ getFrameTree 永不 resolve → frameTreeHandled 挂起 → 所有事件
	// handler（lifecycle/executionContext 前都 await frameTreeHandled）挂起 →
	// goto/evaluate 等满超时（new_page 15-20s、fill 30s 的最终根因）。
	if req.SessionID != "" {
		resp["sessionId"] = req.SessionID
	}
	out, _ := json.Marshal(resp)
	return out
}

func rpcErr(id *int64, code int, message string) []byte {
	out, _ := json.Marshal(map[string]any{"id": id, "error": map[string]any{"code": code, "message": message}})
	return out
}

// rpcErrSess 带 sessionId 的错误响应（session 命令失败时，Puppeteer 同样按 sessionId 路由）。
func rpcErrSess(id *int64, code int, message, sessionID string) []byte {
	out, _ := json.Marshal(map[string]any{
		"id": id, "sessionId": sessionID,
		"error": map[string]any{"code": code, "message": message},
	})
	return out
}
