package computer

import "encoding/json"

// 平台协议 v1：Go stdio 客户端 ↔ Swift helper（或未来 Windows/Linux helper）。
//
// 传输：行分隔 JSON（NDJSON，对齐 bridge↔Electron 惯例）。
//
//	请求  {"id":1, "cmd":"hello",            "params":{...}}
//	响应  {"id":1, "ok":true, "result":{...}}
//	      {"id":1, "ok":false, "error":{"code":"perm_denied","msg":"..."}}
//	通知  {"notify":"perm_status", "result":{...}}（helper 主动推送，预留）
//
// 版本化：hello 握手带 version（本包 v1 = {major:1, minor:0}），Go 侧校验
// major，minor 向后兼容。未来 Windows helper 直接复用本编解码（只换可执行名）。
// Swift 只做「请求 → API → 响应 JSON」，零业务规则（COMPUTER_USE_DESIGN.md §3.6）。
//
// M0 命令面（§6.6 / §11 M0）：hello / perm_status / screen_info / snapshot。
// 后续命令（P0 全命令面）按 §6.6 表逐增：press/click/type/key/scroll/verify/
// open_app/activate/find/screenshot。本文件为纯编解码 + 客户端骨架：
// 命令实现位于 stdio.go（连接管理）/ cmd 分发在 helper。

// ProtoVersion 当前协议版本（hello 握手携带；major 不兼容变化 +1，minor 向后兼容 +1）。
const (
	ProtoMajor = 1
	ProtoMinor = 0
)

// Request 单行请求（Go → helper）。
type Request struct {
	ID     int             `json:"id"`
	Cmd    string          `json:"cmd"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response 单行响应（helper → Go）。ok=true 时 Result 为命令结果 JSON；
// ok=false 时 Error 非空（结构化平台错误码，Executor 转模型可见文本）。
type Response struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ProtoError     `json:"error,omitempty"`
}

// ProtoError 结构化错误（跨语言稳定，code 供 Executor 分支）。
type ProtoError struct {
	Code string `json:"code"` // perm_denied / not_found / timeout / internal / unsupported
	Msg  string `json:"msg"`
}

// Notify helper 主动推送（权限状态变化等，预留；M0 客户端只读跳过）。
type Notify struct {
	Notify string          `json:"notify"`
	Result json.RawMessage `json:"result,omitempty"`
}

// HelloParams hello 握手参数（Go → helper）。
type HelloParams struct {
	VersionMajor int `json:"version_major"`
	VersionMinor int `json:"version_minor"`
}

// HelloResult hello 握手结果（helper → Go）。
type HelloResult struct {
	VersionMajor int    `json:"version_major"`
	VersionMinor int    `json:"version_minor"`
	Helper       string `json:"helper"` // 自报标识，如 "macos-ax"
}

// ---- 动作命令参数/结果（与 Swift helper 契约一致） ----

// PressParams press 命令：uid（当前快照）或 app+menuPath（菜单路径）或 title（文本匹配）。
type PressParams struct {
	UID      string   `json:"uid,omitempty"`
	App      string   `json:"app,omitempty"`
	MenuPath []string `json:"menuPath,omitempty"`
	Title    string   `json:"title,omitempty"`
}

// ClickParams click 命令：逻辑屏幕坐标点。
type ClickParams struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// TypeParams type 命令：要键入的文本。
type TypeParams struct {
	Text string `json:"text"`
}

// KeyParams key 命令：键组合（cmd+shift+p / Return / Tab…）。
type KeyParams struct {
	Keys string `json:"keys"`
}

// OpenAppParams open_app 命令：应用名或路径。
type OpenAppParams struct {
	Name string `json:"name"`
}

// ActivateParams activate 命令：pid（0 = 按 app 名解析）+ app 名 + 窗口标题。
type ActivateParams struct {
	Pid         int    `json:"pid,omitempty"`
	App         string `json:"app,omitempty"`
	WindowTitle string `json:"windowTitle,omitempty"`
}

// VerifyResult verify 命令结果。
type VerifyResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// ScreenshotResult screenshot 命令结果（helper 返回 base64 data URL）。
type ScreenshotResult struct {
	Mime string `json:"mime"`
	Data string `json:"data"` // data:image/png;base64,...
}

// 协议错误码（保持与文档 §6.6 / Swift 侧一致）。
const (
	ErrPermDenied  = "perm_denied"
	ErrNotFound    = "not_found"   // uid 失稳/目标不存在
	ErrTimeout     = "timeout"     // helper 调用超时
	ErrInternal    = "internal"    // 平台 API 失败等
	ErrUnsupported = "unsupported" // 命令/参数未实现
)
