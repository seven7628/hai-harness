package im

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/seven7628/hai-harness/owner"
)

// Clone 深拷贝配置（Owners 映射、Gateways 及其 Routes/AllowUsers/Config、Mirrors）。
//
// 为什么必须有：Runtime 的配置快照与调用方共享底层 map 时，一方原地写就是
// data race（-race 下 UB，线上表现为偶发 panic/串数据）。规矩是
// 「**改副本 → 整体发布**」：读方 ConfigSnapshot() 拿到独立副本，写方先 Clone 再改，
// 最后 SetConfig 发布。切片/映射一律重新分配，不全信 append 的剩余容量。
func (c Config) Clone() Config {
	out := c
	if c.Owners != nil {
		out.Owners = make(map[string]OwnerState, len(c.Owners))
		for k, v := range c.Owners {
			if v.Enabled != nil {
				b := *v.Enabled
				v.Enabled = &b
			}
			out.Owners[k] = v
		}
	}
	if c.Gateways != nil {
		out.Gateways = make([]GatewayConfig, len(c.Gateways))
		for i, g := range c.Gateways {
			if g.Routes != nil {
				g.Routes = append([]RouteConfig(nil), g.Routes...)
			}
			if g.AllowUsers != nil {
				g.AllowUsers = append([]string(nil), g.AllowUsers...)
			}
			if g.Config != nil {
				g.Config = append(json.RawMessage(nil), g.Config...)
			}
			out.Gateways[i] = g
		}
	}
	if c.Mirrors != nil {
		out.Mirrors = append([]MirrorConfig(nil), c.Mirrors...)
	}
	return out
}

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

// RouteConfig 路由：Chat → 投递目标（决定 IM 消息驱动谁）。
//
// 投递目标三选一，与 owner 维度一一对应（**不造第二套真相**：owner 由目标派生）：
//
//	Workspace 非空          → workspace:<path>，驱动该工作区的项目会话（原有语义）
//	Agent 非空（Group 空）  → agent:<agent>，驱动该智能体的 DM 会话（hub.<agent>.dm）
//	Group + Agent 非空      → group:<group>，驱动该群会话（hub.<agent>.g.<group>）；
//	                          Agent 是该群的执行者（群成员，由 hubGroupSession 校验）
//
// Owner 一般留空（派生）；显式填写时必须与目标一致，用于「把已有路由固定到某个归属」。
// 归属决定两件事：**启用状态**（该 owner 关了就不投递）与**hooks 信任**（同 owner 维度）。
type RouteConfig struct {
	Chat      Chat   `json:"chat"`
	Workspace string `json:"workspace,omitempty"` // 项目会话目标（绝对路径）
	Agent     string `json:"agent,omitempty"`     // 智能体目标（DM 或群内执行者）
	Group     string `json:"group,omitempty"`     // 群目标（与 Agent 同时出现）
	Owner     string `json:"owner,omitempty"`     // 显式归属（空 = 由上面三个目标派生）
}

// EffectiveOwner 路由的归属（信任/启用维度）。
//
// 派生顺序与投递目标的含义一致：群 > 智能体 > 工作区。显式 Owner 优先，但必须合法
// （非法/不可识别 → 空串：该路由不投递，见 Config.OwnerEnabled）。空串意味着
// 「不知道属于谁的投递」—— 一律按未启用处理，绝不回落到"全局放行"。
func (r RouteConfig) EffectiveOwner() string {
	if o := strings.TrimSpace(r.Owner); o != "" {
		if owner.Kind(o) == "" {
			return ""
		}
		return o
	}
	if g := strings.TrimSpace(r.Group); g != "" {
		return owner.Group(g)
	}
	if a := strings.TrimSpace(r.Agent); a != "" {
		return owner.Agent(a)
	}
	if w := strings.TrimSpace(r.Workspace); w != "" {
		return owner.Workspace(w)
	}
	return ""
}

// TargetKind 投递目标的种类：workspace | agent | group | ""（配置不完整）。
func (r RouteConfig) TargetKind() string {
	switch {
	case strings.TrimSpace(r.Group) != "":
		return "group"
	case strings.TrimSpace(r.Agent) != "":
		return "agent"
	case strings.TrimSpace(r.Workspace) != "":
		return "workspace"
	default:
		return ""
	}
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

// OwnerState 某个归属（agent/group/workspace）的 IM 状态。
//
// Enabled 是三态：nil = 未显式设置 → **按该 owner 是否有路由**推导（升级兼容：旧配置
// 只有路由、没有 owners 段，升级后消息照常投递，不会静默丢）；显式 true/false = 用户选择。
type OwnerState struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// Config 全局 IM 配置（~/.go-code/im-config.json）。
//
// 连接与归属分两层（这是 owner 隔离的关键）：
//   - 连接面：Enabled + Gateways（一条配置 = 一个账号长连接，**进程级共享**，绝不为
//     每个 owner 各起一条）；
//   - 归属面：Owners（owner → 启用状态）+ 各路由的 EffectiveOwner（投递去谁）。
//     A 群关了 IM 只影响 A 群的路由，B 智能体照常收发。
type Config struct {
	Enabled  bool                  `json:"enabled"`            // ① 主开关（默认关；关 = 全部连接停）
	Gateways []GatewayConfig       `json:"gateways,omitempty"` // ② 注册：一条配置 = 一个 Gateway 实例（含渠道级路由/授权）
	Mirrors  []MirrorConfig        `json:"mirrors,omitempty"`  // ③ 镜像：session ↔ Chat（跨渠道全局）
	Security SecurityConfig        `json:"security"`           // ④ 安全（全局）
	Owners   map[string]OwnerState `json:"owners,omitempty"`   // ⑤ 归属账本：owner → 启用状态
}

// OwnerEnabled 某归属是否启用 IM。
//
// 规则（顺序即语义）：
//  1. 主开关关 → 一律不启用（连接都不在，投递没有意义）；
//  2. Owners 里有显式记录 → 取值；
//  3. 没有记录 → 该 owner 有路由即启用（**迁移默认**：旧配置没有 owners 段，
//     升级后不能因为「没登记」就把消息丢掉）；
//  4. owner 为空/非法 → 不启用（不知道属于谁，就不投递）。
func (c Config) OwnerEnabled(ownerID string) bool {
	if !c.Enabled {
		return false
	}
	if owner.Kind(ownerID) == "" {
		return false
	}
	if st, ok := c.Owners[ownerID]; ok && st.Enabled != nil {
		return *st.Enabled
	}
	return c.HasRoutesFor(ownerID)
}

// HasRoutesFor 该 owner 名下有路由（迁移默认与「有没有配过」的判据）。
func (c Config) HasRoutesFor(ownerID string) bool {
	if ownerID == "" {
		return false
	}
	for _, g := range c.Gateways {
		for _, r := range g.Routes {
			if r.EffectiveOwner() == ownerID {
				return true
			}
		}
	}
	return false
}

// SetOwnerEnabled 写某归属的启用状态（nil 表示清除显式值 → 回到「有路由即启用」）。
func (c *Config) SetOwnerEnabled(ownerID string, enabled *bool) error {
	if !owner.Valid(ownerID) {
		return fmt.Errorf("im config: 非法 owner %q", ownerID)
	}
	if c.Owners == nil {
		c.Owners = map[string]OwnerState{}
	}
	if enabled == nil {
		delete(c.Owners, ownerID)
		return nil
	}
	v := *enabled
	c.Owners[ownerID] = OwnerState{Enabled: &v}
	return nil
}

// OwnersAll 全部归属及其生效启用状态（管理面展示：谁开着、谁关着）。
func (c Config) OwnersAll() map[string]bool {
	out := map[string]bool{}
	for own := range c.Owners {
		if owner.Kind(own) == "" {
			continue
		}
		out[own] = c.OwnerEnabled(own)
	}
	for _, g := range c.Gateways {
		for _, r := range g.Routes {
			if o := r.EffectiveOwner(); o != "" {
				if _, seen := out[o]; !seen {
					out[o] = c.OwnerEnabled(o)
				}
			}
		}
	}
	return out
}

// MigrateOwners 迁移：为「已存在但没有显式状态」的归属补一条 enabled=true。
//
// 为什么需要：迁移前的语义是「配置了路由就会投递」。升级后如果只靠第 3 条推导也等价，
// 但**显式落一条**能让管理面看出「这是升级带来的默认值」，也让用户随后关闭某归属时
// 不会误以为「本来就没开」。返回补了多少条（0 = 无需迁移）。
func (c *Config) MigrateOwners() int {
	n := 0
	seen := map[string]bool{}
	for _, g := range c.Gateways {
		for _, r := range g.Routes {
			o := r.EffectiveOwner()
			if o == "" || seen[o] {
				continue
			}
			seen[o] = true
			if st, ok := c.Owners[o]; ok && st.Enabled != nil {
				continue
			}
			if c.Owners == nil {
				c.Owners = map[string]OwnerState{}
			}
			t := true
			c.Owners[o] = OwnerState{Enabled: &t}
			n++
		}
	}
	return n
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
			// 投递目标：workspace / agent / group(+agent) 三者至少一个，且组合合法。
			kind := r.TargetKind()
			if kind == "" {
				return c, fmt.Errorf("im config: gateway %s route %s 缺少投递目标（workspace/agent/group）", g.ID, r.Chat.String())
			}
			if kind == "group" && strings.TrimSpace(r.Agent) == "" {
				return c, fmt.Errorf("im config: gateway %s route %s 是群路由，必须指定 agent（群内执行者）", g.ID, r.Chat.String())
			}
			if kind == "agent" && strings.TrimSpace(r.Group) != "" {
				return c, fmt.Errorf("im config: gateway %s route %s 同时给了 agent 与 group，请用 group(+agent) 表达群路由", g.ID, r.Chat.String())
			}
			// 显式 owner 必须与目标一致：否则「关掉 X 的启用」会莫名其妙停掉 Y 的投递。
			if raw := strings.TrimSpace(r.Owner); raw != "" {
				derived := RouteConfig{Chat: r.Chat, Workspace: r.Workspace, Agent: r.Agent, Group: r.Group}.EffectiveOwner()
				if owner.Kind(raw) == "" || raw != derived {
					return c, fmt.Errorf("im config: gateway %s route %s 的 owner %q 与投递目标（%s）不一致", g.ID, r.Chat.String(), raw, derived)
				}
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
