// hooks_commands.go —— 设置面板「钩子」页的三个 bridge 命令（契约 §5.3/§5.4/§5.5）。
//
//	hooks_list   汇总三类来源（用户 / Claude 兼容 / 各项目）+ 合并视角 + 信任状态 + 告警
//	hooks_set    写某一层（用户级 → ~/.go-code/settings.json 的 hooks 段；
//	             项目级 → {ws}/.go-code/hooks.json），严格校验 + 热替换到该层涉及的会话
//	hooks_trust  记录命令信任（~/.go-code/hooks-state.json）——**即刻生效**，不重建会话
//
// 两条纪律：
//  1. **不创建运行态**（契约 §2）：打开设置页不得建会话/连 MCP/起 watcher。有运行态 →
//     读它的加载视角，没有 → 纯读文件（peekRuntime，绝不 runtime()）。
//  2. 生效语义是「下一次 Run」（Session.SetHooks 换 Run 入口快照）：配置改了不打断
//     正在跑的一轮，也绝不半途换配置（同一轮里新旧混用不可解释）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/seven7628/hai-harness/hooks"
)

// hookSourceView 一个 hooks 配置来源（契约 §5.3；字段名与前端 HookSourceView 一一对齐）。
type hookSourceView struct {
	Id          string           `json:"id"`
	Kind        string           `json:"kind"` // user | claude-user | project
	Title       string           `json:"title"`
	Path        string           `json:"path"`
	Workspace   string           `json:"workspace,omitempty"`   // 仅项目来源
	Exists      *bool            `json:"exists,omitempty"`      // 仅项目来源（目录还在不在；前端过滤）
	Present     bool             `json:"present"`               // 文件存在且读到了内容
	OK          bool             `json:"ok"`                    // 内容有效（能解析）
	Error       string           `json:"error,omitempty"`       // 解析/读取失败原因
	Editable    bool             `json:"editable"`              // 只有 go-code 自己的文件可写（.claude/* 只读）
	UnknownKeys []string         `json:"unknownKeys,omitempty"` // 顶层未识别键（保存后不保留）
	Body        *hooks.Section   `json:"body"`                  // hooks 段模型（编辑与回写用）
	Claude      []hookClaudeFile `json:"claude,omitempty"`      // 仅项目来源：.claude 兼容层（只读）
}

// hookClaudeFile 项目里的 Claude Code 兼容层文件（只读，存在时才列出）。
type hookClaudeFile struct {
	Path    string         `json:"path"`
	Present bool           `json:"present"`
	OK      bool           `json:"ok"`
	Error   string         `json:"error,omitempty"`
	Body    *hooks.Section `json:"body"`
}

// hooksEffective 合并视角的全局开关 + 实际读到的来源文件（契约 §5.3 effective）。
type hooksEffective struct {
	DisableAllHooks    bool     `json:"disableAllHooks"`
	AllowProjectHooks  bool     `json:"allowProjectHooks"`
	RequireTrust       bool     `json:"requireTrust"`
	ReadClaudeSettings bool     `json:"readClaudeSettings"`
	Sources            []string `json:"sources"`
}

// hooksInfo hooks_list 的 data（契约 §5.3）。
type hooksInfo struct {
	User       hookSourceView    `json:"user"`
	ClaudeUser *hookSourceView   `json:"claudeUser"` // 文件不存在 → null（前端据此隐藏卡片）
	Projects   []hookSourceView  `json:"projects"`
	Effective  hooksEffective    `json:"effective"`
	Trust      map[string]string `json:"trust"` // 命令 → managed|trusted|untrusted|modified
	Warnings   []string          `json:"warnings"`
}

// projectHooksPath 项目级 hooks 文件（{ws}/.go-code/hooks.json，整文件即 hooks 段）。
func projectHooksPath(ws string) string {
	return filepath.Join(ws, ".go-code", "hooks.json")
}

// claudeUserHooksPath 用户级 Claude 兼容层（只读）。
func claudeUserHooksPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "settings.json")
}

// handleHooksList hooks_list：多来源汇总（用户 / Claude 兼容 / 项目）。
//
// 不创建运行态：所有数据都来自"读文件 + 纯函数合并视角"，与项目是否打开无关；
// 项目目录已删（exists=false）也照常返回，由前端过滤（前端要能解释"为什么没显示"）。
func (m *manager) handleHooksList(c command) {
	info := hooksInfo{Projects: []hookSourceView{}, Trust: map[string]string{}, Warnings: []string{}}

	// 用户级：~/.go-code/settings.json 的 hooks 段（可编辑）。
	userSrc := hooks.ReadSource(homeCfgPath("settings.json"), "hooks")
	info.User = sourceView("user", "user", "用户配置", "", true, userSrc)

	// Claude 兼容层（只读）：文件不存在 → null（卡片只在文件存在时出现）。
	if cs := hooks.ReadSource(claudeUserHooksPath(), "hooks"); cs.Present {
		v := sourceView("claude-user", "claude-user", "Claude 兼容（只读）", "", false, cs)
		info.ClaudeUser = &v
	}

	workspaces := dedupePaths(strSlice(c.Payload, "workspaces"))

	// 项目级：每个前端发来的工作区一项（前端按"已绑定 + 目录还在"过滤）。
	for _, ws := range workspaces {
		p := sourceView("project:"+ws, "project", filepath.Base(ws), ws, true, hooks.ReadSource(projectHooksPath(ws), ""))
		exists := false
		if st, err := os.Stat(ws); err == nil && st.IsDir() {
			exists = true
		}
		p.Exists = &exists
		// 项目里的 .claude 兼容层（只读；存在时才列出 —— 与 claude-user 卡片同规则）。
		for _, cp := range []string{
			filepath.Join(ws, ".claude", "settings.json"),
			filepath.Join(ws, ".claude", "settings.local.json"),
		} {
			if cs := hooks.ReadSource(cp, "hooks"); cs.Present {
				p.Claude = append(p.Claude, hookClaudeFile{Path: cs.Path, Present: cs.Present, OK: cs.OK, Error: cs.Err, Body: cs.Body})
			}
		}
		info.Projects = append(info.Projects, p)
	}

	// 合并视角（用户级 + 兼容层；项目层按项目各自的开关/信任单独判定，前端按 §4 规则算）。
	// 用工作区无关的一次 Load：effective 是**全局单值**，而项目文件里的开关只影响它自己。
	cfg, warnings := hooks.Load(hooks.LoadOptions{})
	info.Effective = hooksEffective{
		DisableAllHooks:    cfg.DisableAll,
		AllowProjectHooks:  cfg.AllowProjectHooks,
		RequireTrust:       cfg.RequireTrust,
		ReadClaudeSettings: cfg.ReadClaudeSettings == nil || *cfg.ReadClaudeSettings, // 缺省 = 读
		Sources:            cfg.Sources,
	}
	reqTrust := info.Effective.RequireTrust
	info.Warnings = dedupeStrings(warnings)
	// 项目层告警（非法正则、未知事件、allowProjectHooks 未开而项目 hooks 存在…）也汇总进来：
	// 它们是"配了却不跑"的第一现场，且消息本身带路径，能区分是哪个项目。
	for _, ws := range workspaces {
		_, wsWarnings := hooks.Load(hooks.LoadOptions{WorkspaceDir: ws})
		info.Warnings = dedupeStrings(append(info.Warnings, wsWarnings...))
	}

	// 信任状态：覆盖所有返回 body 里出现的命令（含只读兼容层，用户要能查到"为什么没跑"）。
	// 项目层放最后：同名命令在多层出现时以更严格的项目层判定为准。
	trust := m.hookTrustStore()
	collect := func(src hookSourceView, scope string) {
		for _, cmd := range commandsOf(src.Body) {
			info.Trust[cmd] = trustStatusOf(trust, cmd, scope, reqTrust)
		}
	}
	collect(info.User, "user")
	if info.ClaudeUser != nil {
		collect(*info.ClaudeUser, "claude-user")
	}
	for _, p := range info.Projects {
		collect(p, "project")
		for _, cf := range p.Claude {
			for _, cmd := range commandsOf(cf.Body) {
				info.Trust[cmd] = trustStatusOf(trust, cmd, "claude-project", reqTrust)
			}
		}
	}
	m.resp(c, true, "", map[string]any{
		"user":       info.User,
		"claudeUser": info.ClaudeUser, // nil → JSON null（前端据此隐藏卡片）
		"projects":   info.Projects,
		"effective":  info.Effective,
		"trust":      info.Trust,
		"warnings":   info.Warnings,
	})
}

// handleHooksSet hooks_set：写某一层的 hooks 配置（严格校验，拒绝式）。
//
// 写盘位置（契约 §5.4）：user → ~/.go-code/settings.json 的 hooks 段（保留文件其他键）；
// project → {ws}/.go-code/hooks.json（整文件）。两者都走 hooks.WriteSource：
// 事件名/handler type/timeout/matcher 正则严格校验 —— 运行期才告警太晚（用户会以为配好了）。
//
// 热应用：user → 全部已有运行态工作区的会话；project → 该工作区的会话（下一次 Run 生效）。
func (m *manager) handleHooksSet(c command) {
	scope := strings.TrimSpace(str(c.Payload, "scope"))
	body, err := hooksSectionFromPayload(c.Payload["body"])
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	var path, key, ws string
	switch scope {
	case "user":
		path, key = homeCfgPath("settings.json"), "hooks"
	case "project":
		ws = strings.TrimSpace(str(c.Payload, "workspace"))
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		if st, err := os.Stat(ws); err != nil || !st.IsDir() {
			// 拒绝式：不为"不存在的项目"凭空造目录树（项目被删/未挂载时要让用户看见原因）。
			m.resp(c, false, "工作区不存在或不是目录: "+ws, nil)
			return
		}
		path, key = projectHooksPath(ws), "" // 整文件 = hooks 段
	default:
		m.resp(c, false, fmt.Sprintf("未知 scope %q（支持 user|project）", scope), nil)
		return
	}

	// prev = 当前磁盘内容：条目级未识别字段按位置保留（第三方/未来字段不因一次保存丢失）。
	prev := hooks.ReadSource(path, key)
	if err := hooks.WriteSource(path, key, body, prev); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}

	if scope == "user" {
		m.reloadHooksAllWorkspaces()
	} else {
		m.reloadWorkspaceHooks(ws)
	}
	m.resp(c, true, "", map[string]any{"changed": true})
}

// handleHooksTrust hooks_trust：把命令记入信任库（内容哈希；~/.go-code/hooks-state.json）。
//
// **即刻生效**：信任判定发生在每次派发时（Runner.Dispatch → TrustStore.Check），而这里
// 写的是进程内共享的同一个实例（manager.hookTrustStore）—— 不需要重建会话，也不需要
// 热替换 hooks（契约 §5.5）。命令内容换了字符 → 哈希不匹配 → 自动回到 modified（需重新确认）。
func (m *manager) handleHooksTrust(c command) {
	scope := strings.TrimSpace(str(c.Payload, "scope"))
	if scope == "" {
		scope = "project" // 缺省按更严格的一方（前端默认也是 project）
	}
	reason := str(c.Payload, "reason")
	trust := m.hookTrustStore()
	n := 0
	for _, cmd := range strSlice(c.Payload, "commands") {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			continue // 空命令不是可信任对象（空命令在运行期也必被跳过）
		}
		if err := trust.Trust(cmd, scope, reason); err != nil {
			m.resp(c, false, "信任库写入失败: "+err.Error(), nil)
			return
		}
		n++
	}
	m.resp(c, true, "", map[string]any{"trusted": n})
}

// —— 组装与查询小工具 ——

// sourceView 把一次 ReadSource 的结果转成响应视图（含 id/kind/title/editable 等展示字段）。
func sourceView(id, kind, title, workspace string, editable bool, src hooks.Source) hookSourceView {
	return hookSourceView{
		Id:          id,
		Kind:        kind,
		Title:       title,
		Path:        src.Path,
		Workspace:   workspace,
		Present:     src.Present,
		OK:          src.OK,
		Error:       src.Err,
		Editable:    editable,
		UnknownKeys: src.UnknownKeys,
		Body:        src.Body,
	}
}

// commandsOf 一个 hooks 段里出现的全部命令（信任状态覆盖用；含被禁用/未信任的条目 ——
// 面板要能对每条都显示"为什么没跑"）。
func commandsOf(s *hooks.Section) []string {
	if s == nil {
		return nil
	}
	var out []string
	for name := range s.Events {
		for _, g := range s.Events[name] {
			for _, h := range g.Hooks {
				if cmd := strings.TrimSpace(h.Command); cmd != "" {
					out = append(out, cmd)
				}
			}
		}
	}
	sort.Strings(out) // 确定顺序（同一条重复出现无妨：map 赋值幂等）
	return out
}

// trustStatusOf 命令的信任状态（与 hooks 包运行期判定同口径，契约 §4）：
//   - project / claude-project：严格按信任库 —— 随仓库分发的配置默认**未信任**，
//     攻击者让你 clone 一个仓库就能执行命令正是这条要防的；
//   - user / claude-user：**默认可信**（写这个文件本身就是用户行为），只有 requireTrust
//     时才按信任库；库里已登记时如实回报（managed 显示"托管"，让人看得见来源）。
func trustStatusOf(trust *hooks.TrustStore, cmd, scope string, requireTrust bool) string {
	st := trust.Check(cmd)
	switch scope {
	case "project", "claude-project":
		return string(st)
	default:
		if requireTrust {
			return string(st)
		}
		if st == hooks.TrustManaged {
			return string(st)
		}
		return string(hooks.TrustTrusted) // 用户级默认可信（会执行），不显示"未信任"误导用户
	}
}

// hooksSectionFromPayload 把 payload.body 解析成可编辑模型（严格类型；空/缺省 = 空段）。
func hooksSectionFromPayload(raw any) (*hooks.Section, error) {
	if raw == nil {
		return &hooks.Section{}, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("body 序列化失败: %v", err)
	}
	var section hooks.Section
	if err := json.Unmarshal(b, &section); err != nil {
		return nil, fmt.Errorf("body 不是合法的 hooks 段: %v", err)
	}
	return &section, nil
}

// dedupePaths 原序去重 + 去空白（前端可能重复发同一路径）。
func dedupePaths(paths []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// dedupeStrings 原序去重（同一份文件被多个项目视角读到时告警会重复）。
func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
