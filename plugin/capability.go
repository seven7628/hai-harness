// Package plugin 实现 go-code 桌面端插件框架（轻量能力包）：
// 插件 = 一个能力包，把「运行时组件 + 默认配置 + 工具面 + 生命周期」绑成一次安装。
// 本包是通用扩展点——Capability 接口 + Registry 注册表 + 通用 plugin_* 桥接命令
// （按 id 寻址，与具体插件解耦）；plugin/browser 是首个实例（Browser Use）。
//
// 引入插件概念是为了后续支持更多插件能力：加插件 = 实现 Capability + 注册，
// 框架零改动。v1 只做内建插件 + 受管安装（npm）后端；市场/动态加载/多后端留待真实需求。
package plugin

import (
	"context"
	"github.com/seven7628/hai-harness/tools"
)

// Status 插件生命周期状态（从 fs / 运行时计算，只读）。
type Status string

const (
	// StatusNotInstalled 组件未安装。
	StatusNotInstalled Status = "not-installed"
	// StatusInstalling 组件安装中（临时态，Go 侧不持有）。
	StatusInstalling Status = "installing"
	// StatusReady 组件就绪（已安装、依赖可用），尚未启用。
	StatusReady Status = "ready"
	// StatusEnabled 已启用（连接 up，工具面已注册）。
	StatusEnabled Status = "enabled"
	// StatusUpdateAvail 已装版本落后于内置已知好版本（可一键重装升级）。
	StatusUpdateAvail Status = "update-available"
	// StatusError 依赖缺失或组件损坏（Reason 说明）。
	StatusError Status = "error"
)

// State 插件当前状态快照（UI 渲染数据源；plugin_list 返回该项）。
type State struct {
	Status    Status `json:"status"`
	Reason    string `json:"reason,omitempty"`
	Installed string `json:"installed,omitempty"`
	Pinned    string `json:"pinned,omitempty"`
}

// Capability 插件能力接口（通用扩展点）。实现者负责：
//   - State：从 fs / 运行时计算当前状态（只读）；
//   - Install/Uninstall：受管组件安装/移除；
//   - Enable/Disable：启用（返回注入引擎的工具面，可持有私有连接）/ 释放。
type Capability interface {
	ID() string
	Name() string
	Description() string

	// State 返回当前状态快照（不触发副作用，只读）。
	State(ctx context.Context) State
	// Install 受管安装组件（幂等：已装且版本一致应 no-op）。
	Install(ctx context.Context) error
	// Uninstall 移除组件（已启用时先释放连接）。
	Uninstall(ctx context.Context) error
	// Enable 启用插件，返回注入引擎的工具面（可持有私有 MCP 连接）。幂等。
	Enable(ctx context.Context) ([]tools.Tool, error)
	// Disable 释放资源（私有连接等）。幂等。
	Disable(ctx context.Context) error
}
