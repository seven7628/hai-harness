// Package mcp 实现 MCP（Model Context Protocol）客户端集成：
// 配置解析（mcpServers 段，Claude Code 兼容格式；**分层加载**：用户级 + 项目级，
// 见 Layer/LoadLayers/Merge）、单服务器生命周期（惰性连接/Ping/关闭，mcp-go 客户端，
// stdio/http/sse 传输）、工具适配器（MCP 工具 → 引擎 tools.Tool，
// mcp__<server>__<tool> 命名）、每工作区一个 Manager（跨会话共享连接，配置变更热重载）。
//
// 分层语义：低优先级层在前传入，同名服务器后层覆盖前层（含 enabled:false 显式关闭）。
// 本包只做「路径 → 合并配置」，层从哪来（用户 settings.json / 项目 .mcp.json）由调用方决定。
//
// 两套入口，按是否需要「损坏恢复」选：
//   - LoadLayers / Merge：无状态纯函数。坏文件 = 该层贡献空（适合一次性解析/测试）。
//   - Loader（loader.go）：有状态，逐层保留上次有效快照——坏 JSON/半写/读失败沿用上次
//     有效内容并报 stale，恢复后自动切回。**产品路径（文件监听热重载）用这个。**
//
// 库选择：mark3labs/mcp-go（社区最成熟，传输全含 in-process 测试传输）。
// 适配层隔离库选择——换库只动本包。
package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ServerConfig 单个 MCP 服务器配置（与 Claude Code ~/.claude.json 的 mcpServers 同构，
// 用户可跨工具拷贝配置）。Type 取值 "stdio" | "http" | "sse"。
//
//	{
//	  "name": { "type": "stdio", "command": "npx", "args": ["-y", "@x/y"],
//	            "env": {"TOKEN": "..."} },
//	  "remote": { "type": "http", "url": "http://localhost:8080/mcp" }
//	}
type ServerConfig struct {
	Type    string            `json:"type"`              // stdio（默认）| http | sse
	Command string            `json:"command,omitempty"` // stdio 子进程命令
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"` // stdio 子进程附加环境（继承父进程环境）
	URL     string            `json:"url,omitempty"` // http/sse 端点
	// Enabled 显式关闭（false）：工具不注册、不连接。nil = 默认启用。
	Enabled *bool `json:"enabled,omitempty"`
}

// IsEnabled 解析启用状态（缺省 true）。
func (c ServerConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// Validate 配置校验：类型合法 + 按类型补全必填字段。返回可直接使用的归一化配置。
func (c ServerConfig) Validate() (ServerConfig, error) {
	typ := strings.ToLower(strings.TrimSpace(c.Type))
	if typ == "" {
		typ = "stdio"
	}
	switch typ {
	case "stdio":
		if strings.TrimSpace(c.Command) == "" {
			return c, fmt.Errorf("mcp stdio 服务器缺少 command")
		}
	case "http", "streamable_http", "sse":
		if strings.TrimSpace(c.URL) == "" {
			return c, fmt.Errorf("mcp %s 服务器缺少 url", typ)
		}
	default:
		return c, fmt.Errorf("mcp 未知 type %q（支持 stdio/http/streamable_http/sse）", c.Type)
	}
	c.Type = typ
	return c, nil
}

// Layer 一个配置层：文件路径 + 来源标签。低优先级层在前传给 LoadLayers/Merge。
// Source 只用于展示与诊断（ServerStatus.Source → mcp_list 的 source 字段），
// 不参与连接与差分语义——同名服务器来自哪一层对连接行为无影响。
type Layer struct {
	Path   string // settings.json / .mcp.json 路径；缺文件 = 该层贡献空
	Source string // 来源标签，如 "user"（用户级）| "project"（项目级）；可空
}

// LoadLayers 分层加载 mcpServers 段并合并（低 → 高优先级，同名后层覆盖前层）。
// 返回合并配置与每个服务器名的胜出来源标签（来自哪一层）。
// 全部层贡献空 → (nil, nil)（与单层 LoadConfig 同语义：SetConfig(nil) 即空配置）。
//
// 无状态：坏文件 → 该层贡献空（不保留上次有效）。需要损坏恢复语义用 Loader。
//
// 典型调用（bridge）：用户级 ~/.go-code/settings.json < 项目级 {ws}/.mcp.json
// < 项目级 {ws}/.go-code/settings.json —— 项目层可整体覆盖或经 enabled:false 关闭
// 用户层同名服务器。
func LoadLayers(layers ...Layer) (map[string]ServerConfig, map[string]string) {
	return Merge(nil, "", layers...)
}

// Merge 把分层叠加到 base（最低优先级，如 UI 刚保存、尚未落盘的用户层配置）之上：
// 逐层解析 + 校验，同名后层覆盖前层。返回合并配置与每个服务器名的胜出来源标签。
//
// 无状态：坏文件 → 该层贡献空（需要损坏恢复语义用 Loader.LoadWithBase）。
//
// 校验统一（V2 P1-PROTOCOL-06）：非法项跳过（不静默按 stdio 处理），其余正常。
// 因此「项目层写坏的覆盖项」不生效——用户层同名服务器保持原样（坏配置不改变行为）。
func Merge(base map[string]ServerConfig, baseSource string, layers ...Layer) (map[string]ServerConfig, map[string]string) {
	out := make(map[string]ServerConfig, len(base))
	src := make(map[string]string, len(base))
	for name, c := range base {
		if strings.TrimSpace(name) == "" {
			continue
		}
		norm, err := c.Validate()
		if err != nil {
			continue
		}
		out[name] = norm
		src[name] = baseSource
	}
	for _, l := range layers {
		for name, c := range loadFile(l.Path) {
			out[name] = c
			src[name] = l.Source
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, src
}

// LoadConfig 单文件加载（兼容旧调用）= LoadLayers 单层。
func LoadConfig(settingsPath string) map[string]ServerConfig {
	cfg, _ := LoadLayers(Layer{Path: settingsPath})
	return cfg
}

// loadFile 解析单个配置文件（Electron main 写盘、bridge 读盘同源）。
// 缺文件 / 无该段 / 解析失败 → nil（不报错，MCP 功能降级）。
func loadFile(path string) map[string]ServerConfig {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var v struct {
		MCPServers map[string]ServerConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	out := make(map[string]ServerConfig, len(v.MCPServers))
	for name, cfg := range v.MCPServers {
		if strings.TrimSpace(name) == "" {
			continue
		}
		norm, err := cfg.Validate()
		if err != nil {
			continue // 坏配置跳过（其余正常）
		}
		out[name] = norm
	}
	return out
}
