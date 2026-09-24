package main

// Browser 引擎开关（ego-lite / Browser Use MCP 互斥，2026-09）。
//
// settings.json 顶层字段 browser_engine: "mcp" | "ego"，默认 "mcp"（缺省 = 现状）。
// - mcp：Browser Use 插件（chrome-devtools-mcp）正常启用，Agent 工具面含 browser_*；
// - ego：Browser 插件不自动启用、工具不注册，Agent 走 ego-browser skill（bash heredoc）。
// bridge 只读 settings.json（客户端落盘），启动时 + reload_settings 热读。
// 注意：settings.json 是自由 JSON（provider 段扁平 key 先例），加顶层字段无冲突。

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// BrowserEngine 浏览器引擎值。
type BrowserEngine string

const (
	// EngineMCP Browser Use / chrome-devtools-mcp（默认，现状）。
	EngineMCP BrowserEngine = "mcp"
	// EngineEgo ego lite app + ego-browser skill。
	EngineEgo BrowserEngine = "ego"
)

// DefaultBrowserEngine 缺省引擎（settings.json 无 browser_engine 或非法值）。
const DefaultBrowserEngine = EngineMCP

// browserEngineMu 保护 manager.browserEngine（热读与命令切换并发）。
// 独立小锁：engine 读取可能发生在持 manager.mu 的路径之外
// （autoEnablePluginsAsync 在 goroutine），避免锁序问题。
var browserEngineMu sync.RWMutex

// loadBrowserEngine 读 settings.json 顶层 browser_engine（启动/热读共用）。
func loadBrowserEngine() BrowserEngine {
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return DefaultBrowserEngine
	}
	var outer struct {
		BrowserEngine string `json:"browser_engine"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return DefaultBrowserEngine
	}
	return normalizeBrowserEngine(outer.BrowserEngine)
}

// normalizeBrowserEngine 规范化引擎值（非法/空 → 默认 mcp）。
func normalizeBrowserEngine(v string) BrowserEngine {
	switch BrowserEngine(strings.TrimSpace(v)) {
	case EngineEgo:
		return EngineEgo
	default:
		return EngineMCP // 含空/非法 → mcp（现状兼容）
	}
}

// setBrowserEngine 热切换引擎（内存值；落盘由客户端负责，对齐 set_provider 模式：
// 客户端已写 settings.json，payload 兜底热应用）。
func (m *manager) setBrowserEngine(e BrowserEngine) {
	browserEngineMu.Lock()
	m.browserEngine = e
	browserEngineMu.Unlock()
}

// getBrowserEngine 当前引擎（默认 mcp）。
func (m *manager) getBrowserEngine() BrowserEngine {
	browserEngineMu.RLock()
	defer browserEngineMu.RUnlock()
	if m.browserEngine == "" {
		return DefaultBrowserEngine
	}
	return m.browserEngine
}

// refreshBrowserEngine 从 settings.json 重读引擎（启动/reload_settings 调用）。
func (m *manager) refreshBrowserEngine() {
	m.setBrowserEngine(loadBrowserEngine())
}

// —— ego lite 可用性检测 ——

// egoAppPattern ego lite 主进程匹配名（Chromium 主进程；精确到 .app 路径避免 Helper 误报）。
const egoAppPattern = "ego lite.app/Contents/MacOS/ego lite"

// egoStatus 返回 ego lite 检测结果（供 UI / ego_status 命令；仅本地命令，秒级返回）。
// installed 与 available 分层（2026-09 修复「已安装却仍提示安装」）：
//   - installed：app 已安装（磁盘检测 + ~/.go-code/ego.json 持久记录）→ 前端不再显示安装按钮；
//   - available：cli_found && app_running（真正能跑浏览器任务）；
//     已安装但未运行/未 onboarding 时 available=false 而 installed=true —— 此时提示
//     「启动 ego lite」，而不是「下载安装」。
type egoStatus struct {
	Installed  bool   `json:"installed"`          // app 已安装（磁盘/持久记录命中）
	Available  bool   `json:"available"`          // ego-browser CLI 可用且 app 进程在跑
	AppRunning bool   `json:"app_running"`        // ego lite 主进程在跑
	CliFound   bool   `json:"cli_found"`          // ego-browser 命令在 PATH（或 ~/.local/bin）
	AppPath    string `json:"app_path,omitempty"` // app bundle 路径（诊断/激活）
	Version    string `json:"version,omitempty"`  // ego-browser --version 首行（如 "ego-browser 0.4.7.4"）
	Error      string `json:"error,omitempty"`    // 不可用原因（Available=false 时）
}

// checkEgoStatus 检测 ego 可用性：安装检测（磁盘 + 持久记录）+ pgrep app 进程 +
// LookPath ego-browser + --version。全部本地命令，无网络；--version 加 1.5s 超时兜底。
func checkEgoStatus() egoStatus {
	st := egoStatus{}
	// 0. 是否已安装（磁盘检测 + ~/.go-code/ego.json 持久记录；装了就落盘，卸载则记录失效）
	inst := detectEgoInstall()
	st.Installed = inst.Installed
	st.AppPath = inst.AppPath
	// 1. CLI 在 PATH？
	if p, err := exec.LookPath("ego-browser"); err == nil && p != "" {
		st.CliFound = true
	} else {
		// 兜底：~/.local/bin（ego onboarding 注册位置；GUI app 启动的进程 PATH 可能不含）
		home, _ := os.UserHomeDir()
		alt := filepath.Join(home, ".local", "bin", "ego-browser")
		if _, err := os.Stat(alt); err == nil {
			st.CliFound = true
		}
	}
	// 2. app 主进程在跑？
	if out, err := exec.Command("pgrep", "-f", egoAppPattern).Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		st.AppRunning = true
	}
	// 3. version（CLI 与 app 都在才查；1.5s 超时）
	if st.CliFound && st.AppRunning {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ego-browser", "--version").CombinedOutput()
		if err == nil {
			if first := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]); first != "" {
				st.Version = first
			}
		}
	}
	st.Available = st.CliFound && st.AppRunning
	switch {
	case !st.CliFound && st.AppRunning:
		// 已装且 app 在跑，但 CLI 不可用 → 未完成 onboarding（错过 CLI 注册）
		st.Error = "ego lite 已安装，但 ego-browser 命令不可用（请在 app 内完成一次引导）"
	case !st.CliFound && st.Installed:
		st.Error = "ego lite 已安装，但尚未完成引导（ego-browser 命令未注册）"
	case !st.CliFound:
		st.Error = "ego-browser 命令未找到（未安装 ego lite 或未完成 onboarding）"
	case !st.AppRunning:
		st.Error = "ego lite app 未运行"
	}
	return st
}

// egoStatusToMap 转 map（resp data 用；json tag 对齐前端字段名）。
func egoStatusToMap(st egoStatus) map[string]any {
	return map[string]any{
		"installed":   st.Installed,
		"available":   st.Available,
		"app_running": st.AppRunning,
		"cli_found":   st.CliFound,
		"app_path":    st.AppPath,
		"version":     st.Version,
		"error":       st.Error,
	}
}
