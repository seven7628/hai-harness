// mcp_projects.go —— 项目级 MCP 的**多项目汇总**与**项目层写盘**（契约 §5.1/§5.2）。
//
// 为什么单列一个文件（而不是塞进 main.go 的 mcp_* 命令）：
//   - 既有 mcp_list/mcp_set/mcp_refresh/mcp_reload 都服务「一个工作区 + 用户层」；
//     本文件服务「前端绑定的任意多个项目 + 项目层」，数据源与热应用路径都不同；
//   - 关键约束是**不创建运行态**（契约 §2）：打开设置页不得连接 MCP、不得起 watcher、
//     不得建会话 —— 只有当前已打开的项目才有运行态（连接状态/工具数才拿得到）。
//     "peek 已有运行态，没有就退化为无状态读文件"这条纪律集中在本文件，便于审查。
package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/seven7628/hai-harness/mcp"
)

// mcpServerKnownKeys 模型认识的服务器条目字段：这些键一律以模型（面板）为准，
// 其余键（第三方/未来字段，如 cwd/headers）在名字未变时原样保留（见 config_patch.go）。
var mcpServerKnownKeys = map[string]bool{
	"type": true, "command": true, "args": true, "env": true, "url": true, "enabled": true,
}

// mcpProjectLayerInfo 项目层单文件状态（契约 §5.1 layers；字段名与 mcp.LayerState 对齐，
// 但只暴露面板用得到的：path/present/ok/stale/error/servers/skipped）。
type mcpProjectLayerInfo struct {
	Path    string   `json:"path"`
	Present bool     `json:"present"`
	OK      bool     `json:"ok"`
	Stale   bool     `json:"stale"`
	Error   string   `json:"error"`
	Servers int      `json:"servers"`
	Skipped []string `json:"skipped"`
}

// mcpProjectServerInfo 项目层合并后的一条服务器（契约 §5.1 servers）。
// state/tool_count/error 只在「该项目有运行态且运行态确有这台」时出现（omitempty）。
type mcpProjectServerInfo struct {
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	URL       string            `json:"url"`
	Env       map[string]string `json:"env"`
	Enabled   bool              `json:"enabled"`
	File      string            `json:"file"`
	Editable  bool              `json:"editable"`
	State     string            `json:"state,omitempty"`
	ToolCount int               `json:"tool_count,omitempty"`
	Error     string            `json:"error,omitempty"`
}

// mcpProjectInfo 一个项目（已绑定的工作区）的项目级 MCP 视图（契约 §5.1）。
type mcpProjectInfo struct {
	Path    string                 `json:"path"`
	Name    string                 `json:"name"`
	Exists  bool                   `json:"exists"`
	Running bool                   `json:"running"`
	Layers  []mcpProjectLayerInfo  `json:"layers"`
	Servers []mcpProjectServerInfo `json:"servers"`
}

// peekRuntime 取**已存在**的工作区运行态；不存在返回 nil（绝不创建）。
// 与 runtime() 的差别只有这一点，但正是"打开设置页不产生副作用"的关键。
func (m *manager) peekRuntime(path string) *workspaceRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workspaces[path]
}

// mcpProjectsData 汇总多个项目的项目层 MCP 状态（payload.workspaces 的原序去重）。
func (m *manager) mcpProjectsData(workspaces []string) []mcpProjectInfo {
	seen := map[string]bool{}
	out := make([]mcpProjectInfo, 0, len(workspaces))
	for _, ws := range workspaces {
		ws = strings.TrimSpace(ws)
		if ws == "" || seen[ws] {
			continue
		}
		seen[ws] = true
		out = append(out, m.mcpProjectView(ws))
	}
	return out
}

// mcpProjectView 单个项目的项目层视图。
func (m *manager) mcpProjectView(ws string) mcpProjectInfo {
	info := mcpProjectInfo{Path: ws, Name: filepath.Base(ws), Layers: []mcpProjectLayerInfo{}, Servers: []mcpProjectServerInfo{}}
	// exists：目录被删（项目失效）由前端过滤，后端仍返回（前端要知道"这条为什么没显示"）。
	if st, err := os.Stat(ws); err == nil && st.IsDir() {
		info.Exists = true
	}
	layers := projectMCPLayers(ws) // 仅项目两层：.mcp.json < .go-code/settings.json

	// 有运行态 → 用它的 Loader 读（含每层 last-good：文件坏时 stale/error 对用户可见）；
	// 无运行态 → 一次性 Loader（无 last-good，纯读当前文件）。两条路都不"应用"配置。
	rt := m.peekRuntime(ws) // 唯一一次 peek：下面读层状态与连接状态都用同一份
	var loader *mcp.Loader
	if rt != nil {
		info.Running = true
		loader = rt.mcpCfg
	}
	if loader == nil {
		loader = &mcp.Loader{}
	}
	res := loader.Load(layers...)

	// 逐层"当前文件内容"（无 last-good）：只为给每台服务器标注它来自哪个文件
	// （editable 判定与"随仓库分发"徽标的依据）。文件坏 → 该层空，下面按 stale 层兜底归属。
	perLayer := make([]map[string]mcp.ServerConfig, len(layers))
	for i, ly := range layers {
		perLayer[i], _ = mcp.LoadLayers(ly)
	}
	fileOf := map[string]string{}
	for i, ly := range layers {
		for name := range perLayer[i] {
			fileOf[name] = ly.Path
		}
	}

	for i, st := range res.Layers {
		if i >= len(layers) {
			break
		}
		skipped := st.Skipped
		if skipped == nil {
			skipped = []string{}
		}
		info.Layers = append(info.Layers, mcpProjectLayerInfo{
			Path: layers[i].Path, Present: st.Present, OK: st.OK, Stale: st.Stale,
			Error: st.Err, Servers: st.Servers, Skipped: skipped,
		})
	}

	// 连接状态只在有运行态时取（Status 是该工作区已应用配置的实时快照）。
	status := map[string]mcp.ServerStatus{}
	if rt != nil && rt.mcp != nil {
		for _, st := range rt.mcp.Status() {
			status[st.Name] = st
		}
	}

	names := make([]string, 0, len(res.Config))
	for name := range res.Config {
		names = append(names, name)
	}
	sort.Strings(names) // 文件里的顺序在 loader 合并时已丢（map），排序只求确定性
	for _, name := range names {
		c := res.Config[name]
		file := fileOf[name]
		if file == "" {
			// 归不到任何层的当前内容 = 来自某层 last-good（该文件当前坏/半写）。
			// 归到那层，才能正确区分"可编辑（.go-code/settings.json）"与"随仓库分发"。
			for i := range res.Layers {
				if res.Layers[i].Stale {
					file = res.Layers[i].Path
					break
				}
			}
		}
		row := mcpProjectServerInfo{
			Name:     name,
			Type:     c.Type,
			Command:  c.Command,
			Args:     c.Args,
			URL:      c.URL,
			Env:      c.Env,
			Enabled:  c.IsEnabled(),
			File:     file,
			Editable: file == layers[len(layers)-1].Path, // 只有 {ws}/.go-code/settings.json 可编辑
		}
		if row.Args == nil {
			row.Args = []string{}
		}
		if row.Env == nil {
			row.Env = map[string]string{}
		}
		if st, ok := status[name]; ok {
			row.State, row.ToolCount, row.Error = st.State, st.ToolCount, st.Error
		}
		info.Servers = append(info.Servers, row)
	}
	return info
}

// mcpProjectSettingsPath 项目层可编辑文件（{ws}/.go-code/settings.json）。
func mcpProjectSettingsPath(ws string) string {
	return filepath.Join(ws, ".go-code", "settings.json")
}

// handleMCPProjectSet mcp_project_set：写某项目的 {ws}/.go-code/settings.json 的 mcpServers 段。
//
// 语义（契约 §5.2）：
//   - 校验与 mcp_set 同源（parseMCPConfig → mcp.ServerConfig.Validate）：name 非空、
//     type ∈ {stdio,http,streamable_http,sse}、stdio 需 command、其余需 url；非法**拒绝保存**；
//   - servers 是**该项目层可编辑文件的完整期望值**（面板只把 editable=true 的条目发上来）：
//     空 map → 删除该键（不留空壳），非空 → 整段替换（文件其他键保留）；
//   - 条目级未识别字段按名保留（第三方/未来字段不因一次保存被抹掉）；
//   - 热应用：该项目**有运行态** → reloadMCP（重读分层 + 有变化则重建会话工具面）；
//     无运行态 → 仅写盘（下次打开该项目时读取）。写盘也会触发文件监听自愈应用。
func (m *manager) handleMCPProjectSet(c command) {
	ws := strings.TrimSpace(str(c.Payload, "workspace"))
	if ws == "" {
		m.resp(c, false, "workspace 为空", nil)
		return
	}
	if st, err := os.Stat(ws); err != nil || !st.IsDir() {
		// 拒绝式：不为"不存在的项目"凭空造目录树（项目目录被删/未挂载时用户要知道原因）。
		m.resp(c, false, "工作区不存在或不是目录: "+ws, nil)
		return
	}
	cfg, err := parseMCPConfig(c.Payload["servers"])
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	path := mcpProjectSettingsPath(ws)
	patch := map[string]any{}
	if len(cfg) == 0 {
		patch["mcpServers"] = deleteKey // 空 map → 删键（契约 §5.2）
	} else {
		merged := mergeUnknownByName(rawMessageMap(cfg), rawSection(path, "mcpServers"), mcpServerKnownKeys)
		patch["mcpServers"] = merged
	}
	if err := patchJSONFile(path, patch); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	if m.peekRuntime(ws) != nil {
		m.reloadMCP(ws) // 有运行态：立即重读 + 有变化则重建会话工具面
	}
	m.resp(c, true, "", map[string]any{"changed": true})
}
