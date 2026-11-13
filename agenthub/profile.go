package agenthub

import (
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// AgentID 是智能体的稳定标识。展示名可改，ID 不可改。
type AgentID string

// Avatar 是智能体头像。图片头像存 agents/<id>/avatar.png，不把二进制塞进 profile.json。
type Avatar struct {
	Kind  string `json:"kind"`           // emoji | image
	Value string `json:"value"`          // emoji
	BG    string `json:"bg,omitempty"`   // emoji 背景色（#rrggbb）
	Path  string `json:"path,omitempty"` // image: agents/<id>/avatar.png
}

// MemoryPrefs 是智能体自己的记忆偏好。记忆正文在 agents/<id>/memory，不进 profile。
type MemoryPrefs struct {
	// Enabled 指针三态：nil = 没设置过（Normalize 置为 true），false = 用户显式关闭。
	// 用 bool 会在「从没设置过」与「用户关掉」之间丢失信息 —— 面板的关闭开关就再也
	// 分不清该显示什么（P1-05 复核：这个字段此前全仓无人读取，等于死字段）。
	Enabled       *bool `json:"enabled,omitempty"`
	MaxIndexLines int   `json:"max_index_lines,omitempty"` // 默认 200
}

// IsEnabled 记忆是否开启（nil = 默认开）。
func (m MemoryPrefs) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// MCPPolicy 是 MCP 的 server/tool 级开关。零值 = 全开。
//
// 注意：MCP 工具名是 mcp__<server>__<tool>；DisableTools 用 "server.tool" 或 glob，
// 避免把协议前缀泄漏到用户配置里。
type MCPPolicy struct {
	Disable        bool     `json:"disable,omitempty"`         // true = 全部 MCP 不注册
	DisableServers []string `json:"disable_servers,omitempty"` // server 名
	DisableTools   []string `json:"disable_tools,omitempty"`   // "server.tool" / glob
	AllowTools     []string `json:"allow_tools,omitempty"`     // 非空 = 只允许这些
}

// ToolPolicy 是智能体的工具面开关。
//
// 零值 = 全开：安全靠 cwd/沙箱/审批/预算，不靠默认隐藏工具。新增工具默认可见，
// 不会因为忘记加字段而被静默关闭（D18）。
type ToolPolicy struct {
	Preset         string    `json:"preset,omitempty"`  // all | readonly | custom；空 = all
	Disable        []string  `json:"disable,omitempty"` // 精确名或前缀：browser_*、mcp__github__*
	Allow          []string  `json:"allow,omitempty"`   // custom 时白名单；空 = 全部
	BashRoots      []string  `json:"bash_roots,omitempty"`
	DisableNetwork bool      `json:"disable_network,omitempty"`
	MCP            MCPPolicy `json:"mcp"`
}

// 工具预设取值。空字符串等价于 ToolPresetAll。
const (
	ToolPresetAll      = "all"
	ToolPresetReadonly = "readonly"
	ToolPresetCustom   = "custom"
)

// RunPolicy 是智能体“能对世界做什么”的权限与预算。
type RunPolicy struct {
	CanCreateWorkspace bool     `json:"can_create_workspace"`
	CanDispatch        bool     `json:"can_dispatch"`
	CanHandoff         bool     `json:"can_handoff"`
	CanShareArtifact   bool     `json:"can_share_artifact"`
	WorkspaceRoots     []string `json:"workspace_roots,omitempty"`
	MaxTaskCostUSD     float64  `json:"max_task_cost_usd"`
	DailyBudgetUSD     float64  `json:"daily_budget_usd"`
	MaxAutoTurns       int      `json:"max_auto_turns"`
}

// AgentProfile 是智能体的全部可配置状态。运行态（session/run）不在里面。
//
// 注意：Provider/Model 是显式装配的，不依赖全局 active provider。
type AgentProfile struct {
	ID                AgentID     `json:"id"`
	Name              string      `json:"name"`
	Avatar            Avatar      `json:"avatar"`
	Accent            string      `json:"accent"` // #rrggbb；名字/chip/状态环的唯一识别色
	Role              string      `json:"role"`
	Description       string      `json:"description"`
	Instructions      string      `json:"instructions"`
	Persona           string      `json:"persona"`
	Provider          string      `json:"provider"`
	Model             string      `json:"model"`
	Effort            string      `json:"effort"`
	CompressThreshold float64     `json:"compress_threshold"`
	Memory            MemoryPrefs `json:"memory"`
	Tools             ToolPolicy  `json:"tools"`
	Policy            RunPolicy   `json:"policy"`
	Version           int         `json:"version"`
	Deleted           bool        `json:"deleted,omitempty"` // 软删；DM/群记录与记忆保留
	CreatedAt         time.Time   `json:"created_at"`
	UpdatedAt         time.Time   `json:"updated_at"`
}

// P0-03：Profile 的默认值、校验与派生色。
//
// 约定：Normalize = ApplyDefaults + Validate；Validate 假定默认值已应用，
// 但会容忍零值字段（零值语义见 §5.9）。错误统一 wrap ErrInvalid，UI 可直接显示。
const (
	// MaxAgents 是 Hub 的智能体总数上限。
	MaxAgents = 50
	// MaxGroupMembers 是单个群的成员上限。
	MaxGroupMembers = 12
)

const (
	AvatarKindEmoji = "emoji"
	AvatarKindImage = "image"

	DefaultCompressThreshold = 0.8
	// MinCompressThreshold / MaxCompressThreshold 是 compress_threshold 的钳位区间（§5.1）。
	// 太低会让每轮都压缩（成本爆炸），太高等于不压缩（上下文溢出）。
	MinCompressThreshold    = 0.5
	MaxCompressThreshold    = 0.95
	DefaultMemoryIndexLines = 200
	DefaultMaxAutoTurns     = 6
)

// Normalize 应用默认值并校验；只有全部通过才原地更新，失败时对象保持原样。
func (p *AgentProfile) Normalize() error {
	if p == nil {
		return fmt.Errorf("%w: profile is nil", ErrInvalid)
	}
	cp := *p
	cp.ApplyDefaults()
	if err := cp.Validate(); err != nil {
		return err
	}
	*p = cp
	return nil
}

// ApplyDefaults 填充零值默认值，并做无语义损失的规范化（trim、颜色小写、去重）。
// 它不返回错误：非法值保留到 Validate 报告，避免“默认值把错误吞掉”。
func (p *AgentProfile) ApplyDefaults() {
	p.Name = strings.TrimSpace(p.Name)
	p.Provider = strings.TrimSpace(p.Provider)
	p.Model = strings.TrimSpace(p.Model)
	p.Effort = strings.TrimSpace(p.Effort)

	p.Avatar.Kind = strings.ToLower(strings.TrimSpace(p.Avatar.Kind))
	p.Avatar.Value = strings.TrimSpace(p.Avatar.Value)
	p.Avatar.BG = strings.TrimSpace(p.Avatar.BG)
	p.Avatar.Path = strings.TrimSpace(p.Avatar.Path)
	if p.Avatar.Kind == "" {
		if p.Avatar.Path != "" {
			p.Avatar.Kind = AvatarKindImage
		} else {
			p.Avatar.Kind = AvatarKindEmoji
		}
	}
	if p.Avatar.Kind == AvatarKindEmoji && p.Avatar.Value == "" {
		p.Avatar.Value = "🤖"
	}

	p.Accent = strings.TrimSpace(p.Accent)
	if c, ok := normalizeHexColor(p.Accent); ok {
		p.Accent = c
	}
	if p.Accent == "" {
		if c, ok := normalizeHexColor(p.Avatar.BG); ok {
			p.Accent = c
		} else {
			p.Accent = defaultAccent(p.Name)
		}
	}
	if p.Avatar.BG == "" {
		if c, ok := normalizeHexColor(p.Accent); ok {
			p.Avatar.BG = c
		}
	}
	if c, ok := normalizeHexColor(p.Avatar.BG); ok {
		p.Avatar.BG = c
	}

	if p.CompressThreshold == 0 {
		p.CompressThreshold = DefaultCompressThreshold
	} else if !math.IsNaN(p.CompressThreshold) && !math.IsInf(p.CompressThreshold, 0) {
		p.CompressThreshold = math.Min(MaxCompressThreshold, math.Max(MinCompressThreshold, p.CompressThreshold))
	}
	if p.Memory.MaxIndexLines == 0 {
		p.Memory.MaxIndexLines = DefaultMemoryIndexLines
	}
	if p.Memory.Enabled == nil {
		on := true
		p.Memory.Enabled = &on
	}

	p.Tools.ApplyDefaults()
	p.Policy.ApplyDefaults()

	if p.Version == 0 {
		p.Version = 1
	}
}

// Validate 校验已归一化的 Profile；不检查 provider 是否存在/有 key（那是 Service + settings 的职责）。
func (p *AgentProfile) Validate() error {
	if p == nil {
		return fmt.Errorf("%w: profile is nil", ErrInvalid)
	}
	if p.ID != "" {
		if err := ValidateAgentID(p.ID); err != nil {
			return err
		}
	}
	if err := validateProfileName(p.Name); err != nil {
		return err
	}
	// 零值代表“用默认值”（Normalize 会补），这里只拒绝显式给出的非法值。
	if p.Avatar.Kind != "" {
		switch p.Avatar.Kind {
		case AvatarKindEmoji:
			// 空 value 由 ApplyDefaults 补默认 emoji。
		case AvatarKindImage:
			if strings.TrimSpace(p.Avatar.Path) == "" {
				return fmt.Errorf("%w: profile.avatar.path is required for image", ErrInvalid)
			}
		default:
			return fmt.Errorf("%w: profile.avatar.kind must be %q or %q", ErrInvalid, AvatarKindEmoji, AvatarKindImage)
		}
	}
	if p.Avatar.BG != "" {
		if _, ok := normalizeHexColor(p.Avatar.BG); !ok {
			return fmt.Errorf("%w: profile.avatar.bg must be #rrggbb", ErrInvalid)
		}
	}
	if p.Accent != "" {
		if _, ok := normalizeHexColor(p.Accent); !ok {
			return fmt.Errorf("%w: profile.accent must be #rrggbb", ErrInvalid)
		}
	}
	if err := validateProviderName(p.Provider); err != nil {
		return err
	}
	if p.Model == "" || len(p.Model) > 256 || strings.ContainsAny(p.Model, "\r\n\t") {
		return fmt.Errorf("%w: profile.model must be a non-empty single-line string (<=256)", ErrInvalid)
	}
	if p.CompressThreshold != 0 &&
		(math.IsNaN(p.CompressThreshold) || math.IsInf(p.CompressThreshold, 0) ||
			p.CompressThreshold < MinCompressThreshold || p.CompressThreshold > MaxCompressThreshold) {
		return fmt.Errorf("%w: profile.compress_threshold must be within [0.5, 0.95]", ErrInvalid)
	}
	if p.Memory.MaxIndexLines < 0 {
		return fmt.Errorf("%w: profile.memory.max_index_lines must be >= 0", ErrInvalid)
	}
	if err := p.Tools.Validate(); err != nil {
		return err
	}
	if err := p.Policy.Validate(); err != nil {
		return err
	}
	if p.Version < 0 {
		return fmt.Errorf("%w: profile.version must be >= 0", ErrInvalid)
	}
	return nil
}

// ApplyDefaults 填 ToolPolicy 的零值；空 preset 等价 all（D18）。
func (t *ToolPolicy) ApplyDefaults() {
	if t.Preset == "" {
		t.Preset = ToolPresetAll
	}
	t.Disable = cleanStrings(t.Disable)
	t.Allow = cleanStrings(t.Allow)
	t.BashRoots = cleanStrings(t.BashRoots)
	t.MCP.DisableServers = cleanStrings(t.MCP.DisableServers)
	t.MCP.DisableTools = cleanStrings(t.MCP.DisableTools)
	t.MCP.AllowTools = cleanStrings(t.MCP.AllowTools)
}

// Validate 校验工具开关：preset 合法；allow 只配 custom；disable 不得被 allow 精确放回。
func (t ToolPolicy) Validate() error {
	switch t.Preset {
	case "", ToolPresetAll, ToolPresetReadonly, ToolPresetCustom:
	default:
		return fmt.Errorf("%w: tools.preset must be all, readonly or custom", ErrInvalid)
	}
	if t.Preset != ToolPresetCustom && len(t.Allow) > 0 {
		return fmt.Errorf("%w: tools.allow is only valid with preset=custom", ErrInvalid)
	}
	if t.Preset == ToolPresetCustom && len(t.Allow) == 0 {
		return fmt.Errorf("%w: tools.allow must be non-empty with preset=custom", ErrInvalid)
	}
	for _, v := range append(append([]string{}, t.Disable...), t.Allow...) {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: tools disable/allow entries must be non-empty", ErrInvalid)
		}
	}
	// 精确同名禁止放回；glob 的覆盖关系交给工具引擎（disable 优先级更高）。
	for _, d := range t.Disable {
		for _, a := range t.Allow {
			if d == a {
				return fmt.Errorf("%w: tools.disable %q cannot be re-enabled by tools.allow", ErrInvalid, d)
			}
		}
	}
	for _, d := range t.MCP.DisableTools {
		for _, a := range t.MCP.AllowTools {
			if d == a {
				return fmt.Errorf("%w: mcp.disable_tools %q cannot be re-enabled by mcp.allow_tools", ErrInvalid, d)
			}
		}
	}
	return nil
}

// ApplyDefaults 填 RunPolicy 的默认值（§5.9：MaxAutoTurns 零值 = 6）。
func (r *RunPolicy) ApplyDefaults() {
	if r.MaxAutoTurns == 0 {
		r.MaxAutoTurns = DefaultMaxAutoTurns
	}
	r.WorkspaceRoots = cleanStrings(r.WorkspaceRoots)
}

// Validate 校验预算/权限；预算 0 表示不限额（仍受全局护栏）。
func (r RunPolicy) Validate() error {
	if r.MaxAutoTurns < 0 {
		return fmt.Errorf("%w: policy.max_auto_turns must be >= 0", ErrInvalid)
	}
	if r.MaxTaskCostUSD < 0 || math.IsNaN(r.MaxTaskCostUSD) || math.IsInf(r.MaxTaskCostUSD, 0) {
		return fmt.Errorf("%w: policy.max_task_cost_usd must be >= 0", ErrInvalid)
	}
	if r.DailyBudgetUSD < 0 || math.IsNaN(r.DailyBudgetUSD) || math.IsInf(r.DailyBudgetUSD, 0) {
		return fmt.Errorf("%w: policy.daily_budget_usd must be >= 0", ErrInvalid)
	}
	if r.CanCreateWorkspace && len(r.WorkspaceRoots) == 0 {
		return fmt.Errorf("%w: policy.workspace_roots is required when can_create_workspace is true", ErrInvalid)
	}
	for _, root := range r.WorkspaceRoots {
		if strings.TrimSpace(root) == "" {
			return fmt.Errorf("%w: policy.workspace_roots entries must be non-empty", ErrInvalid)
		}
	}
	return nil
}

// ValidateAgentName 校验名字形状；唯一性见 ValidateAgentNameUnique。
func ValidateAgentName(name string) error {
	return validateProfileName(name)
}

// ValidateAgentNameUnique 在已有 Profile 中做大小写不敏感的重名检查。
// except 用于更新自己时排除自身。
func ValidateAgentNameUnique(name string, existing []*AgentProfile, except AgentID) error {
	if err := validateProfileName(name); err != nil {
		return err
	}
	want := strings.TrimSpace(name)
	for _, p := range existing {
		if p == nil || p.ID == except || p.Deleted {
			continue
		}
		if strings.ToLower(strings.TrimSpace(p.Name)) == strings.ToLower(want) {
			return fmt.Errorf("%w: agent name %q already used by %s", ErrInvalid, want, p.ID)
		}
	}
	return nil
}

// ValidateAgentCount 检查还能否新增智能体；软删（Deleted）不占名额。
func ValidateAgentCount(existing []*AgentProfile) error {
	count := 0
	for _, p := range existing {
		if p != nil && !p.Deleted {
			count++
		}
	}
	if count >= MaxAgents {
		return fmt.Errorf("%w: agent limit %d reached", ErrInvalid, MaxAgents)
	}
	return nil
}

// ValidateAgentID / ValidateGroupID 只用于新分配的 ID；历史 ID 的兼容性由迁移处理。
func ValidateAgentID(id AgentID) error {
	return validatePrefixedID(string(id), PrefixAgent, 8)
}

func ValidateGroupID(id GroupID) error {
	return validatePrefixedID(string(id), PrefixGroup, 6)
}

func validateProfileName(name string) error {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n < 1 || n > 32 {
		return fmt.Errorf("%w: profile.name must be 1-32 characters", ErrInvalid)
	}
	switch strings.ToLower(name) {
	case "all", "所有人", "你", "user":
		return fmt.Errorf("%w: profile.name %q is reserved", ErrInvalid, name)
	}
	if strings.ContainsRune(name, '@') {
		return fmt.Errorf("%w: profile.name must not contain '@'", ErrInvalid)
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%w: profile.name must not contain control/format characters", ErrInvalid)
		}
	}
	return nil
}

func validateProviderName(provider string) error {
	if provider == "" {
		return fmt.Errorf("%w: profile.provider is required", ErrInvalid)
	}
	for _, r := range provider {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.') {
			return fmt.Errorf("%w: profile.provider %q has invalid characters", ErrInvalid, provider)
		}
	}
	return nil
}

func validatePrefixedID(id, prefix string, suffixLen int) error {
	wantPrefix := prefix + "_"
	if !strings.HasPrefix(id, wantPrefix) {
		return fmt.Errorf("%w: id %q must start with %q", ErrInvalid, id, wantPrefix)
	}
	suffix := strings.TrimPrefix(id, wantPrefix)
	if len(suffix) != suffixLen {
		return fmt.Errorf("%w: id %q must have %d chars after %q", ErrInvalid, id, suffixLen, wantPrefix)
	}
	for _, r := range suffix {
		if !((r >= 'a' && r <= 'z') || (r >= '2' && r <= '7')) {
			return fmt.Errorf("%w: id %q must be lowercase base32", ErrInvalid, id)
		}
	}
	return nil
}

func cleanStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeHexColor(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) == 4 && s[0] == '#' {
		s = "#" + string([]byte{s[1], s[1], s[2], s[2], s[3], s[3]})
	}
	if len(s) != 7 || s[0] != '#' {
		return "", false
	}
	for _, r := range s[1:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return "", false
		}
	}
	return strings.ToLower(s), true
}

// defaultAccent 从名字稳定派生一个 accent；同一名字跨进程/跨端一致。
func defaultAccent(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(name))))
	return hsvToHex(float64(h.Sum32()%360), 0.55, 0.62)
}

func hsvToHex(h, s, v float64) string {
	c := v * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := v - c
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return fmt.Sprintf("#%02x%02x%02x", int(math.Round((r+m)*255)), int(math.Round((g+m)*255)), int(math.Round((b+m)*255)))
}
