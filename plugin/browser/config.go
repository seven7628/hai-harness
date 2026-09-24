// Browser Use 用户可调配置：模型（Config）、默认值、校验、config.json 读写。
// 持久化到插件独立配置文件 <pluginsDir>/browser/config.json（非 settings.json——
// 插件自治配置，契合「插件独立包管理」；组件卸载随之删除，重装回默认）。
// 校验策略：双层——LoadConfig/SaveConfig 均先 Validate；非法保存**拒绝**（不静默回退），
// 非法读取回默认（坏文件不破坏可用性）。默认值 = 现有行为 + 隐私友好化。
package browser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// 会话模式（登录策略）：persistent=持久 profile（默认，登录/状态跨会话保留）；
// isolated=每次临时无痕 profile（零状态残留）；existing=连已运行的 Chrome
// （--auto-connect，规避 WebDriver 登录检测）。
// 注意：登录策略与渲染位置是正交维度（presentation 字段），不要混为一谈。
const (
	SessionPersistent = "persistent"
	SessionIsolated   = "isolated"
	SessionExisting   = "existing"
)

// Presentation 渲染位置：window=独立浏览器窗口（默认）；embedded=右侧 web tab 内嵌视图。
const (
	PresentationWindow   = "window"
	PresentationEmbedded = "embedded"
)

// URL 限制模式：none=不限制（默认）；block=拦截匹配导航/子资源；allow=仅允许匹配
// （block 与 allow 互斥，UI 用单选天然避免同时配置）。对应上游
// blockedUrlPattern / allowedUrlPattern。
const (
	URLModeNone  = "none"
	URLModeBlock = "block"
	URLModeAllow = "allow"
)

// validChannels 有效 Chrome 渠道（空串 = 系统默认 stable）。
var validChannels = []string{"", "stable", "canary", "dev", "beta"}

// maxPatterns / maxPatternLen URL 允许/拦截模式条数与单条长度上限（防乱填）。
const (
	maxPatterns   = 20
	maxPatternLen = 200
)

// viewportRe 视口格式（如 1280x720；width/height 各 2-5 位数字，对齐上游解析）。
var viewportRe = regexp.MustCompile(`^\d{2,5}x\d{2,5}$`)

// DefaultPluginsDir 受管组件根目录（默认 ~/.go-code/plugins；New 与 bridge 共用）。
func DefaultPluginsDir() string {
	return filepath.Join(defaultUserHomeDir(), ".go-code", "plugins")
}

// configPath 本插件配置文件路径（<pluginsDir>/browser/config.json）。
func configPath(pluginsDir string) string {
	return filepath.Join(componentDir(pluginsDir), "config.json")
}

// DefaultConfig 返回默认配置（开箱可用）：现有行为（有头可见/persistent profile/
// pageId 路由/核心工具面）+ 隐私友好（敏感头脱敏、不发使用统计）。
func DefaultConfig() Config {
	return Config{
		SessionMode:          SessionPersistent,    // 登录默认持久（保留 session）
		Presentation:         PresentationEmbedded, // 渲染默认内嵌（右侧 web tab）
		Headless:             false,
		Viewport:             "1280x720",
		Channel:              "",
		RedactNetworkHeaders: boolPtr(true), // 隐私默认：脱敏敏感网络头（上游 chrome-devtools-mcp 默认 false）
		UsageStatistics:      false,         // 隐私默认：不发使用统计（上游默认 true → 需 --no-usage-statistics）
		URLMode:              URLModeNone,
		URLPatterns:          nil,
		AcceptInsecureCerts:  false,
		UserDataDir:          "",
	}
}

// boolPtr 布尔指针（nil=未设置→回默认）。
func boolPtr(b bool) *bool { return &b }

// Redact 敏感头脱敏是否开启（nil = 默认 true）。
func (c Config) Redact() bool { return c.RedactNetworkHeaders == nil || *c.RedactNetworkHeaders }

// applyDefaults 零值字段回默认（New 用）：结构性字段（模式/视口/渠道/URL 模式）。
// 布尔字段零值=false 恰为多数默认，无需处理；redact 默认 true 用指针区分「未设置」。
func (c Config) applyDefaults() Config {
	d := DefaultConfig()
	if c.SessionMode == "" {
		c.SessionMode = d.SessionMode
	}
	if c.Viewport == "" {
		c.Viewport = d.Viewport
	}
	if c.Channel == "" {
		c.Channel = d.Channel
	}
	if c.URLMode == "" {
		c.URLMode = d.URLMode
	}
	if c.Presentation == "" {
		c.Presentation = d.Presentation
	}
	if c.RedactNetworkHeaders == nil {
		c.RedactNetworkHeaders = d.RedactNetworkHeaders
	}
	return c
}

// Validate 校验并归一化配置：枚举字段非法 → 返回错误（**拒绝保存**，防止破坏可用性）；
// 合法则归一化（空值回默认、patterns 清理、URLMode=none 清空 patterns）。
func (c Config) Validate() (Config, error) {
	// 会话模式
	switch c.SessionMode {
	case "":
		c.SessionMode = SessionPersistent
	case SessionPersistent, SessionIsolated, SessionExisting:
	default:
		return c, fmt.Errorf("无效的会话模式 %q（可选 persistent/isolated/existing）", c.SessionMode)
	}
	// 渲染位置（presentation）
	switch c.Presentation {
	case "":
		c.Presentation = PresentationWindow
	case PresentationWindow, PresentationEmbedded:
	default:
		return c, fmt.Errorf("无效的渲染位置 %q（可选 window/embedded）", c.Presentation)
	}
	// 视口
	if c.Viewport == "" {
		c.Viewport = DefaultConfig().Viewport
	} else if !viewportRe.MatchString(c.Viewport) {
		return c, fmt.Errorf("无效的视口 %q（格式如 1280x720）", c.Viewport)
	}
	// Chrome 渠道
	if c.Channel != "" && !containsStr(validChannels, c.Channel) {
		return c, fmt.Errorf("无效的 Chrome 渠道 %q（可选 stable/canary/dev/beta）", c.Channel)
	}
	// URL 限制模式
	if c.URLMode == "" {
		c.URLMode = URLModeNone
	}
	switch c.URLMode {
	case URLModeNone, URLModeBlock, URLModeAllow:
	default:
		return c, fmt.Errorf("无效的 URL 限制模式 %q（可选 none/block/allow）", c.URLMode)
	}
	// URL 模式清理：trim/去空/上限/无内部空白
	patterns, err := normalizePatterns(c.URLPatterns)
	if err != nil {
		return c, err
	}
	c.URLPatterns = patterns
	if c.URLMode == URLModeNone {
		c.URLPatterns = nil
	}
	return c, nil
}

// normalizePatterns 清理 URL 允许/拦截模式：trim、去空、条数/长度/无空白校验。
func normalizePatterns(patterns []string) ([]string, error) {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > maxPatternLen {
			return nil, fmt.Errorf("URL 模式过长（上限 %d 字符）: %q", maxPatternLen, truncate(p, 80))
		}
		if strings.ContainsAny(p, " \t\r\n") {
			return nil, fmt.Errorf("URL 模式不能含空白（URLPattern 语法）: %q", truncate(p, 80))
		}
		out = append(out, p)
	}
	if len(out) > maxPatterns {
		return nil, fmt.Errorf("URL 模式过多（上限 %d 条）", maxPatterns)
	}
	return out, nil
}

// LoadConfig 读插件配置（<pluginsDir>/browser/config.json）。缺文件 / 坏 JSON /
// 坏配置 → 回默认（开箱可用，不破坏）。参照 mcp.LoadConfig（mcp/config.go:66）。
func LoadConfig(pluginsDir string) Config {
	cfg := DefaultConfig()
	raw, err := os.ReadFile(configPath(pluginsDir))
	if err != nil {
		return cfg
	}
	// 预置默认再 unmarshal：缺键保留默认，显式键（含 false/空）覆盖
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return DefaultConfig()
	}
	norm, err := cfg.Validate()
	if err != nil {
		return DefaultConfig()
	}
	return norm
}

// SaveConfig 校验并写插件配置（非法 → 返回错误不落盘）。目录不存在则创建。
func SaveConfig(pluginsDir string, cfg Config) error {
	norm, err := cfg.Validate()
	if err != nil {
		return err
	}
	dir := componentDir(pluginsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建插件配置目录: %w", err)
	}
	raw, err := json.MarshalIndent(norm, "", "  ")
	if err != nil {
		return err
	}
	tmp := configPath(pluginsDir) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, configPath(pluginsDir))
}

// containsStr 切片包含判断。
func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
