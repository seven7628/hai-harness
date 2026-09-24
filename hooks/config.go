package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Handler 单个 hook handler（与 Claude Code / Codex 同形，多两个 Codex 增强字段）。
type Handler struct {
	Type                   string  `json:"type"`                             // "command"（当前唯一实现）+ http|mcp_tool|prompt（预留）
	Name                   string  `json:"name,omitempty"`                   // 日志/禁用标识
	Command                string  `json:"command,omitempty"`                //
	Timeout                float64 `json:"timeout,omitempty"`                // 秒；>=1000 视为毫秒（Qwen 兼容规则）
	Async                  bool    `json:"async,omitempty"`                  //
	StatusMessage          string  `json:"statusMessage,omitempty"`          //
	FailureMode            string  `json:"failureMode,omitempty"`            // fail_open(默认)|fail_closed
	AdditionalContextLimit int     `json:"additionalContextLimit,omitempty"` // 字节
	Enabled                *bool   `json:"enabled,omitempty"`                //
}

// Group 一个 matcher 分组。
type Group struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []Handler `json:"hooks"`

	re    *regexp.Regexp // 解析期产物（不参与序列化）
	scope string         // 来源层级：user|project|claude-user|claude-project（信任判定用）
}

// 配置来源层级（信任模型的关键输入，见规范 §6）：
//   - user 层：用户自己写的配置（~/.go-code/settings.json / ~/.claude/settings.json）
//     → **默认可信**：写这个文件本身就是显式的用户行为，不构成"打开别人的仓库即执行"的风险；
//   - project 层：随仓库分发的配置（{ws}/.go-code/hooks.json、{ws}/.claude/settings.json）
//     → **默认需显式信任**（内容哈希入库），否则攻击者只要让你 clone 一个仓库就能执行命令。
const (
	scopeUser          = "user"
	scopeProject       = "project"
	scopeClaudeUser    = "claude-user"
	scopeClaudeProject = "claude-project"
)

func (g Group) Scope() string { return g.scope }

// trustedByScope 该来源层级是否"默认可信"。
func (c *Config) trustedByScope(scope string) bool {
	if c != nil && c.RequireTrust {
		return false // 逃生舱：要求所有 hook 都必须显式信任（企业/高安全场景）
	}
	switch scope {
	case scopeUser, scopeClaudeUser:
		return true
	default:
		return false
	}
}

// Config 合并后的 hook 配置。
type Config struct {
	DisableAll         bool     `json:"disableAllHooks,omitempty"`
	Disabled           []string `json:"disabled,omitempty"`
	ReadClaudeSettings *bool    `json:"readClaudeSettings,omitempty"`
	AllowProjectHooks  bool     `json:"allowProjectHooks,omitempty"`
	// RequireTrust 要求**所有** hook（含用户级配置）都必须先显式信任才执行。
	// 默认 false：用户级配置默认可信（见 scopeUser 注释），项目级始终需信任。
	RequireTrust bool              `json:"requireTrust,omitempty"`
	Events       map[Event][]Group `json:"events,omitempty"`

	Sources []string `json:"-"` // 来源追踪（审计与排错用）
}

// section 三层配置的原始形态。
type section struct {
	DisableAll         bool               `json:"disableAllHooks"`
	Disabled           []string           `json:"disabled"`
	ReadClaudeSettings *bool              `json:"readClaudeSettings"`
	AllowProjectHooks  bool               `json:"allowProjectHooks"`
	RequireTrust       bool               `json:"requireTrust"`
	Events             map[string][]Group `json:"events"`
}

// LoadOptions 描述从哪里加载。
type LoadOptions struct {
	HomeDir      string
	WorkspaceDir string
	// ClaudeSettingsPaths 允许测试注入；留空则按规范 §3.1 推导（项目两处 + 用户一处）。
	ClaudeSettingsPaths []string
}

// Load 按"用户级 → 项目级 → .claude 兼容层"顺序加载并**追加合并**（规范 §3.1）。
// 返回的 warnings 供产品展示（非法正则、非法 timeout、未知事件等），永不 panic。
func Load(opts LoadOptions) (*Config, []string) {
	cfg := &Config{Events: map[Event][]Group{}}
	var warnings []string

	home := opts.HomeDir
	if home == "" {
		home, _ = os.UserHomeDir()
	}

	// 1) 用户级：~/.go-code/settings.json 的 hooks 段
	mergeFile(cfg, filepath.Join(home, ".go-code", "settings.json"), "hooks", scopeUser, &warnings, true)

	// 2) 项目级：{ws}/.go-code/hooks.json —— 默认需 allowProjectHooks（防止"打开别人的仓库即执行其命令"）
	if opts.WorkspaceDir != "" {
		projPath := filepath.Join(opts.WorkspaceDir, ".go-code", "hooks.json")
		if cfg.AllowProjectHooks || peekAllowProject(projPath) {
			mergeFile(cfg, projPath, "", scopeProject, &warnings, true)
		} else if _, err := os.Stat(projPath); err == nil {
			warnings = append(warnings, "project hooks present but allowProjectHooks=false — skipped: "+projPath)
		}
	}

	// 3) 兼容层：Claude Code 的 settings（只读）
	if cfg.ReadClaudeSettings == nil || *cfg.ReadClaudeSettings {
		paths := opts.ClaudeSettingsPaths
		if paths == nil {
			if opts.WorkspaceDir != "" {
				paths = append(paths,
					filepath.Join(opts.WorkspaceDir, ".claude", "settings.json"),
					filepath.Join(opts.WorkspaceDir, ".claude", "settings.local.json"))
			}
			paths = append(paths, filepath.Join(home, ".claude", "settings.json"))
		}
		for _, p := range paths {
			mergeFile(cfg, p, "hooks", scopeForClaudeSettings(p, home), &warnings, true)
		}
	}

	validate(cfg, &warnings)
	return cfg, warnings
}

// peekAllowProject 只读判断项目级配置是否自带 allowProjectHooks。
func peekAllowProject(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var s section
	if json.Unmarshal(raw, &s) != nil {
		return false
	}
	return s.AllowProjectHooks
}

// mergeFile 读取一个 JSON 文件并合并其 hooks 段。topKey 非空时先解一层（settings.json 形态）。
func mergeFile(cfg *Config, path, topKey, scope string, warnings *[]string, markSource bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return // 缺文件是正常情况
	}
	body := raw
	if topKey != "" {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			*warnings = append(*warnings, fmt.Sprintf("%s: unparseable JSON — skipped", path))
			return
		}
		b, ok := envelope[topKey]
		if !ok {
			return
		}
		body = b
	}

	// 形态 1（go-code 原生）：{"hooks": {"events": {"PreToolUse": [...]}, "disableAllHooks": …}}
	var s section
	if err := json.Unmarshal(body, &s); err == nil {
		if s.DisableAll {
			cfg.DisableAll = true
		}
		cfg.Disabled = append(cfg.Disabled, s.Disabled...)
		if s.ReadClaudeSettings != nil {
			cfg.ReadClaudeSettings = s.ReadClaudeSettings
		}
		if s.AllowProjectHooks {
			cfg.AllowProjectHooks = true
		}
		if s.RequireTrust {
			cfg.RequireTrust = true
		}
		for name, groups := range s.Events {
			appendEvent(cfg, path, name, groups, scope, warnings)
		}
	} else {
		*warnings = append(*warnings, fmt.Sprintf("%s: hooks section unparseable — skipped", path))
		return
	}

	// 形态 2（Claude Code / Codex 兼容）：事件名**直接**挂在 hooks 下，
	// {"hooks": {"PreToolUse": [{"matcher": …, "hooks": [ … ]}]}}
	var flat map[string]json.RawMessage
	if err := json.Unmarshal(body, &flat); err == nil {
		for key, v := range flat {
			ev := Event(key)
			if _, known := Anchors[ev]; !known {
				continue // events / disableAllHooks / disabled 等非事件键
			}
			var groups []Group
			if err := json.Unmarshal(v, &groups); err != nil {
				*warnings = append(*warnings, fmt.Sprintf("%s: %s groups unparseable — skipped", path, key))
				continue
			}
			appendEvent(cfg, path, key, groups, scope, warnings)
		}
	}

	if markSource {
		cfg.Sources = append(cfg.Sources, path)
	}
}

// appendEvent 追加某事件的 matcher 组，并对未知事件名告警。
func appendEvent(cfg *Config, path, name string, groups []Group, scope string, warnings *[]string) {
	ev := Event(name)
	if _, ok := Anchors[ev]; !ok {
		*warnings = append(*warnings, fmt.Sprintf("%s: unknown hook event %q — ignored", path, name))
		return
	}
	for i := range groups {
		groups[i].scope = scope // 信任判定用（user 层默认可信；project 层需显式信任）
	}
	cfg.Events[ev] = append(cfg.Events[ev], groups...)
}

// scopeForClaudeSettings 判断某个 .claude/settings*.json 属于用户层还是项目层。
// 依据：路径是否在 home 下的 .claude 目录（用户级）还是工作区的 .claude 目录（项目级）。
func scopeForClaudeSettings(path, home string) string {
	if home != "" {
		userDir := filepath.Join(home, ".claude")
		if strings.HasPrefix(filepath.Clean(path), filepath.Clean(userDir)+string(filepath.Separator)) {
			return scopeClaudeUser
		}
	}
	return scopeClaudeProject
}

// validate 解析 matcher 正则、校验命令与单位，产出告警（不 panic、不静默）。
func validate(cfg *Config, warnings *[]string) {
	disabled := map[string]bool{}
	for _, n := range cfg.Disabled {
		disabled[n] = true
	}
	for ev, groups := range cfg.Events {
		anchor := Anchors[ev]
		for gi := range groups {
			g := &groups[gi]
			g.re = nil
			if p := strings.TrimSpace(g.Matcher); p != "" && p != "*" && p != ".*" && !alnum.MatchString(p) {
				re, err := regexp.Compile(p)
				if err != nil {
					*warnings = append(*warnings, fmt.Sprintf("%s[%d]: invalid matcher %q — group skipped", ev, gi, g.Matcher))
					g.Hooks = nil // 非法 matcher 不执行（绝不静默全匹配）
					continue
				}
				g.re = re
			}
			for hi := range g.Hooks {
				h := &g.Hooks[hi]
				if h.Type == "" {
					h.Type = "command"
				}
				if h.Kind() != "command" {
					*warnings = append(*warnings, fmt.Sprintf("%s[%d][%d]: handler type %q not implemented — skipped", ev, gi, hi, h.Type))
					h.Enabled = boolPtr(false)
					continue
				}
				if strings.TrimSpace(h.Command) == "" {
					*warnings = append(*warnings, fmt.Sprintf("%s[%d][%d]: empty command — skipped", ev, gi, hi))
					h.Enabled = boolPtr(false)
					continue
				}
				if h.Name != "" && disabled[h.Name] {
					h.Enabled = boolPtr(false)
				}
				if _, ok := h.TimeoutDuration(anchor); !ok {
					*warnings = append(*warnings, fmt.Sprintf("%s[%d][%d]: invalid timeout %v — default applied", ev, gi, hi, h.Timeout))
					h.Timeout = 0
				}
			}
			groups[gi] = *g
		}
		cfg.Events[ev] = groups
	}
}

// TimeoutDuration 实现单位兼容（规范 §3.3）：
// 0 → 事件默认值；0<v<1000 → 秒；>=1000 → 毫秒（Graft 写进 Codex 的 10000/15000 走这档）；负值非法。
func (h Handler) TimeoutDuration(a Anchor) (time.Duration, bool) {
	if h.Timeout == 0 {
		return a.DefaultTimeout, true
	}
	if h.Timeout < 0 {
		return 0, false
	}
	d := time.Duration(h.Timeout * float64(time.Second))
	if h.Timeout >= 1000 {
		d = time.Duration(h.Timeout) * time.Millisecond
	}
	if d > 120*time.Second {
		d = 120 * time.Second
	}
	return d, true
}

// Kind 归一化 handler 类型。
func (h Handler) Kind() string {
	if strings.TrimSpace(h.Type) == "" {
		return "command"
	}
	return strings.ToLower(strings.TrimSpace(h.Type))
}

// FailureClosed 该 handler 失败时是否按阻断处理（默认 fail_open）。
func (h Handler) FailureClosed() bool { return strings.EqualFold(h.FailureMode, "fail_closed") }

// Async 该事件是否允许 async handler（规范 §4.4：只有非决策类事件允许）。
func (a Anchor) Async() bool {
	return a.Event == EventSessionStart || a.Event == EventPostToolUse || a.Event == EventStop
}

func boolPtr(b bool) *bool { return &b }
