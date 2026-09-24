package im

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GatewayConfig 单个 Gateway 实例配置（一条配置 = 一个 Gateway 实例）。
// 路由与授权名单是渠道维度，归属到实例（不再全局平铺）。
type GatewayConfig struct {
	ID      string          `json:"id"`      // 实例 id（管理面寻址）
	Type    string          `json:"type"`    // Gateway 类型（注册表查表）
	Enabled bool            `json:"enabled"` // 显式启用
	Config  json.RawMessage `json:"config"`  // 渠道特定配置（Schema.Fields 渲染出的键值）

	// Routes 该渠道的路由：Chat → workspace（决定 IM 消息驱动哪个工作区）。
	Routes []RouteConfig `json:"routes,omitempty"`
	// AllowUsers 该渠道的授权名单（允许驱动 agent 的用户）。
	// nil（未配置）= 默认放行所有用户（"*"，自用/开发开箱即用）；
	// 显式空数组 [] = 全拒绝（安全收紧）；["*"] = 放行所有；[id...] = 精确名单。
	AllowUsers []string `json:"allow_users"`
}

// RouteConfig 路由：Chat → workspace（决定 IM 消息驱动哪个工作区）。
type RouteConfig struct {
	Chat      Chat   `json:"chat"`
	Workspace string `json:"workspace"` // workspace 绝对路径
}

// MirrorMode 镜像模式。
type MirrorMode string

const (
	MirrorNone          MirrorMode = "none"
	MirrorPush          MirrorMode = "push"          // 单向推送（会话 → IM）
	MirrorBidirectional MirrorMode = "bidirectional" // 双向（会话 ↔ IM）
)

// MirrorConfig 镜像：session ↔ Chat（决定哪些会话推送到哪个聊天框）。
type MirrorConfig struct {
	SessionID string     `json:"session_id"`
	Chat      Chat       `json:"chat"`
	Mode      MirrorMode `json:"mode"`
}

// SecurityConfig 安全模型（全局项：绑定确认等渠道无关设置）。
type SecurityConfig struct {
	// RequireBindConfirm 陌生 Chat 首次发消息需桌面端确认绑定后才路由。
	RequireBindConfirm bool `json:"require_bind_confirm"`
}

// Config 全局 IM 配置（~/.go-code/im-config.json）。
type Config struct {
	Enabled  bool            `json:"enabled"`            // ① 主开关（默认关）
	Gateways []GatewayConfig `json:"gateways,omitempty"` // ② 注册：一条配置 = 一个 Gateway 实例（含渠道级路由/授权）
	Mirrors  []MirrorConfig  `json:"mirrors,omitempty"`  // ③ 镜像：session ↔ Chat（跨渠道全局）
	Security SecurityConfig  `json:"security"`           // ④ 安全（全局）
}

// DefaultConfig 默认配置（主开关关、无 Gateway、无路由、无镜像、安全默认开）。
func DefaultConfig() Config {
	return Config{
		Enabled: false,
		Security: SecurityConfig{
			RequireBindConfirm: true,
		},
	}
}

// LoadConfig 从指定路径读取 IM 配置。缺文件 → 返回默认配置（不报错，功能降级）。
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return Config{}, fmt.Errorf("im config: read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("im config: parse %s: %w", path, err)
	}
	// 归一化：空 Security 结构补默认
	if !cfg.Security.RequireBindConfirm {
		cfg.Security.RequireBindConfirm = true
	}
	// 归一化：未配置授权名单（nil）的 gateway 默认放行所有用户（"*"）——
	// 自用/开发开箱即用；显式空数组 [] 保留为全拒绝（安全收紧）。
	for i := range cfg.Gateways {
		if cfg.Gateways[i].AllowUsers == nil {
			cfg.Gateways[i].AllowUsers = []string{"*"}
		}
	}
	return cfg, nil
}

// SaveConfig 原子写配置（临时文件 + rename）。
func SaveConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("im config: mkdir: %w", err)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("im config: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("im config: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("im config: rename: %w", err)
	}
	return nil
}

// Validate 配置校验：主开关、Gateway 类型/必填、渠道级路由 Chat 非零、镜像模式合法。
// 返回归一化后的配置（补默认值）。
func (c Config) Validate() (Config, error) {
	for i := range c.Gateways {
		g := &c.Gateways[i]
		g.ID = strings.TrimSpace(g.ID)
		g.Type = strings.ToLower(strings.TrimSpace(g.Type))
		if g.ID == "" {
			return c, fmt.Errorf("im config: gateway 缺少 id")
		}
		if g.Type == "" {
			return c, fmt.Errorf("im config: gateway %q 缺少 type", g.ID)
		}
		// 归一化：未配置授权名单（nil）→ 默认放行所有用户（"*"，开箱即用）；
		// 显式空数组 [] 保留 = 全拒绝（安全收紧）。
		if g.AllowUsers == nil {
			g.AllowUsers = []string{"*"}
		}
		// 渠道级路由校验
		for j := range g.Routes {
			r := &g.Routes[j]
			if _, ok := r.Chat.Normalize(); !ok {
				return c, fmt.Errorf("im config: gateway %s route 缺少 chat", g.ID)
			}
			if strings.TrimSpace(r.Workspace) == "" {
				return c, fmt.Errorf("im config: gateway %s route %s 缺少 workspace", g.ID, r.Chat.String())
			}
		}
		if !g.Enabled {
			continue // 未启用的不校验类型存在性（允许配置了但没编译进二进制）
		}
		if _, err := New(g.Type, g.Config); err != nil {
			return c, fmt.Errorf("im config: gateway %q: %w", g.ID, err)
		}
	}
	for i := range c.Mirrors {
		m := &c.Mirrors[i]
		if _, ok := m.Chat.Normalize(); !ok {
			return c, fmt.Errorf("im config: mirror 缺少 chat")
		}
		if m.Mode != MirrorPush && m.Mode != MirrorBidirectional {
			return c, fmt.Errorf("im config: mirror %s 非法 mode %q", m.Chat.String(), m.Mode)
		}
	}
	return c, nil
}

// GatewayByID 按 id 查 Gateway 配置。
func (c Config) GatewayByID(id string) (GatewayConfig, bool) {
	for _, g := range c.Gateways {
		if g.ID == id {
			return g, true
		}
	}
	return GatewayConfig{}, false
}

// RouteForChat 查 Chat 路由（命中返回 workspace + true）。
// 路由是渠道维度：遍历各 Gateway 实例的 Routes。
func (c Config) RouteForChat(chat Chat) (string, bool) {
	for _, g := range c.Gateways {
		for _, r := range g.Routes {
			if r.Chat.Gateway == chat.Gateway && r.Chat.ChatID == chat.ChatID && r.Chat.ThreadID == chat.ThreadID {
				return r.Workspace, true
			}
		}
	}
	return "", false
}

// UserAllowed 查用户是否在该渠道的授权名单（渠道维度）。
// 名单为空 = 全拒绝（安全默认）。
func (c Config) UserAllowed(gateway, userID string) bool {
	for _, g := range c.Gateways {
		if g.Type != gateway {
			continue
		}
		for _, u := range g.AllowUsers {
			// "*" = 放行该渠道所有用户（开发/自用场景；生产慎用）
			if u == "*" || u == userID {
				return true
			}
		}
	}
	return false
}

// RoutesOf 某 Gateway 实例的路由。
func (c Config) RoutesOf(id string) []RouteConfig {
	if g, ok := c.GatewayByID(id); ok {
		return g.Routes
	}
	return nil
}

// AllowUsersOf 某 Gateway 实例的授权名单。
func (c Config) AllowUsersOf(id string) []string {
	if g, ok := c.GatewayByID(id); ok {
		return g.AllowUsers
	}
	return nil
}

// MirrorsForSession 查某会话的镜像（会话可关联多个 Chat）。
func (c Config) MirrorsForSession(sessionID string) []MirrorConfig {
	var out []MirrorConfig
	for _, m := range c.Mirrors {
		if m.SessionID == sessionID {
			out = append(out, m)
		}
	}
	return out
}
