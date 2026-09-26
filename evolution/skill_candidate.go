package evolution

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SkillCandidate 一条待验证的技能候选（Phase D 的核心对象）。
//
// 与 skills.Skill 的关系：字段**对齐** skills.Skill（Name / Description /
// Instructions / Version），但不 import 那个包。原因是依赖方向必须是
// skills/ ← evolution/ 单向，skills/ 绝不能反向依赖 evolution/（否则技能注册
// 这条主链路会被自进化控制面拖住）。宿主注入的 SkillInstaller 负责把本结构
// 翻译成真正的 SKILL.md。
type SkillCandidate struct {
	// Name 技能名（小写连字符，映射到 <skillsdir>/<name>/SKILL.md）。
	Name string `json:"name"`
	// Description 触发描述（对齐 skills.Skill.Description 语义：模型靠它决定何时加载）。
	Description string `json:"description"`
	// Instructions 完整指令正文（对应 SKILL.md body）。
	Instructions string `json:"instructions"`
	// Version 内容版本（可选，同 skills.Skill.Version 语义）。
	Version string `json:"version,omitempty"`
	// MemoryRefs 该技能依赖的记忆主题（Phase D 的核心关联）。
	//
	// 规范化成 "topic:<name>"，取值必须是**真实存在**的主题（提交时校验）。
	//
	// 边界：MemoryRefs **不具备**加载或权限语义 —— 它只是"这个技能的结论来自
	// 哪条记忆"的溯源信息。技能运行时是否读这些记忆，仍由 Agent 按 Phase A 的
	// memory_view 自主决定。把它升级成自动加载钩子会同时破坏两条 Phase A 契约：
	// "Agent 自主读取"与"Subagent 默认隔离"（技能对 subagent 可见，自动注入
	// 等于让 subagent 隐式获得父记忆）。
	MemoryRefs []string `json:"memory_refs,omitempty"`
}

// Refs 解析后的 MemoryRefs（Ref 结构，含关联性质）。
//
// 注意：SkillCandidate.MemoryRefs 存的是**字符串**（与 skills.Skill 的
// frontmatter 形态一致，便于宿主直接翻译成 SKILL.md），关联性质（derived /
// supports / contradicts）由宿主另行维护。把它放回字符串是刻意的：技能文件
// 是给人读的，关联性质的解释不该藏在结构里。
func (c *SkillCandidate) Refs() []string { return c.MemoryRefs }

// SKILLMD 渲染候选的 SKILL.md 文本（frontmatter + 正文）。
//
// 供宿主注入的 SkillInstaller 直接使用：把"我们认为的技能格式"和"skills 包
// 要求的格式"之间的差异收敛在这里一处，而不是散落在每个调用点。
func (c *SkillCandidate) SKILLMD() string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", c.Name)
	fmt.Fprintf(&b, "description: %s\n", yamlSafeString(c.Description))
	if c.Version != "" {
		fmt.Fprintf(&b, "version: %s\n", yamlSafeString(c.Version))
	}
	if len(c.MemoryRefs) > 0 {
		b.WriteString("metadata:\n")
		b.WriteString("  memory_refs:\n")
		for _, r := range c.MemoryRefs {
			fmt.Fprintf(&b, "    - %s\n", yamlSafeString(r))
		}
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(c.Instructions))
	b.WriteString("\n")
	return b.String()
}

// yamlSafeString 把字符串写成 YAML 安全的形态（含标点时加引号）。
func yamlSafeString(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`,") || strings.HasPrefix(s, " ") {
		return strconv.Quote(s)
	}
	return s
}

// normalize 规范化并校验技能候选。
func (c *SkillCandidate) normalize() error {
	if c == nil {
		return errors.New("skill candidate is nil")
	}
	// 技能名**先按路径语义拒绝**再折叠大小写/分隔符。
	//
	// 与 Phase A 的 validateTopicName 同一个立场：把 `../../escape` 折叠成
	// `escape` 会让调用方以为安装/激活了自己点名的那个技能，实际上装到了别处。
	// 静默改名在这里是安全问题，不是便利。
	raw := strings.TrimSpace(c.Name)
	if raw == "" {
		return errors.New("skill name is empty")
	}
	if strings.ContainsAny(raw, `/\`) || raw == "." || raw == ".." || strings.Contains(raw, "..") {
		return fmt.Errorf("skill name %q must be a single safe slug, not a path", c.Name)
	}
	n := sanitizeSegment(raw)
	if n == "" {
		return errors.New("skill name is empty")
	}
	if err := safeSegment("skill", n); err != nil {
		return fmt.Errorf("skill name %q: %w", c.Name, err)
	}
	c.Name = n
	c.Description = strings.TrimSpace(c.Description)
	c.Instructions = strings.TrimSpace(c.Instructions)
	if c.Description == "" {
		return errors.New("skill description is required (it is what the model matches against)")
	}
	if c.Instructions == "" {
		return errors.New("skill instructions are required (SKILL.md body)")
	}
	refs := make([]string, 0, len(c.MemoryRefs))
	seen := make(map[string]bool, len(c.MemoryRefs))
	for _, ref := range c.MemoryRefs {
		n, err := normalizeMemoryRef(ref)
		if err != nil {
			return fmt.Errorf("skill %s memory ref %q: %w", c.Name, ref, err)
		}
		if seen[n] {
			continue // 去重：同一记忆被引两次没有信息量
		}
		seen[n] = true
		refs = append(refs, n)
	}
	// 排序：引用列表顺序不应由提交顺序决定，否则同一份技能会因提交顺序不同
	// 而渲染出不同字节，破坏 SKILL.md 的可复现性。
	sort.Strings(refs)
	c.MemoryRefs = refs
	return nil
}

// checkNonEmpty 非空检查。
func (c *SkillCandidate) checkNonEmpty() error {
	if c == nil {
		return errors.New("skill candidate is nil")
	}
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("skill name is empty")
	}
	if strings.TrimSpace(c.Instructions) == "" {
		return errors.New("skill instructions are empty")
	}
	return nil
}

// normalizeMemoryRef 规范化记忆引用（"topic:foo" / "memory://project/<id>/foo" / "foo"）。
//
// 三种写法归一到 "topic:<name>"。与 Phase A 的 ParseRef 保持同一套主题名规范
// （小写连字符 slug），因此 Skill 里的 MemoryRef 一定能命中 topics/<name>.md。
func normalizeMemoryRef(ref string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(ref))
	if r == "" {
		return "", errors.New("memory ref is empty")
	}
	if strings.HasPrefix(r, "memory://project/") {
		rest := strings.TrimPrefix(r, "memory://project/")
		slash := strings.LastIndex(rest, "/")
		if slash < 0 {
			return "", errors.New("memory ref is missing a topic segment")
		}
		r = rest[slash+1:]
	}
	r = strings.TrimPrefix(r, "topic:")
	r = strings.TrimPrefix(r, "topics/")
	r = strings.TrimSuffix(r, ".md")
	if err := safeSegment("topic", r); err != nil {
		return "", err
	}
	return "topic:" + r, nil
}
