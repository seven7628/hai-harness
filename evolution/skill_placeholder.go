package evolution

import (
	"errors"
	"fmt"
	"strings"
)

// SkillCandidate 一条待验证的技能候选（Phase D 的核心对象，Phase B 仅占位）。
//
// **Phase B 不实现任何 Skill Candidate 行为**：B 的门禁链与 Replay 对技能候选
// 一律按"结构回放直接 pass"处理，详见 structuralReplayer.Replay。之所以这里先
// 放一个最小类型声明，是为了 Candidate 的字段（Skill *SkillCandidate）在 B 阶段
// 就固定下来——否则 Phase C/D 回来加字段会变成对已发布 API 的破坏性变更。
//
// Phase D 补全的字段与方法见 skill_candidate.go（与本文件同包，届时删除占位部分）。
type SkillCandidate struct {
	// Name 技能名（小写连字符，映射到 <skillsdir>/<name>/SKILL.md）。
	Name string `json:"name"`
	// Description 触发描述（对齐 skills.Skill.Description 语义：模型靠它决定何时加载）。
	Description string `json:"description"`
	// Instructions 完整指令正文（对应 SKILL.md body）。
	Instructions string `json:"instructions"`
	// Version 内容版本（可选，同 skills.Skill.Version 语义）。
	Version string `json:"version,omitempty"`
	// MemoryRefs 该技能依赖的记忆主题（Phase D 的核心关联，见 MemoryRef）。
	//
	// 边界：MemoryRefs **不具备**加载或权限语义——它只是"这个技能的结论来自哪条
	// 记忆"的溯源信息。技能实际运行时是否读这些记忆，仍由 Agent 按 Phase A 的
	// memory_view 自主决定。文档里对 MemoryRefs 的这条限定必须保持：把它升级成
	// 自动加载钩子会破坏"Agent 自主读取"这条 Phase A 契约。
	MemoryRefs []string `json:"memory_refs,omitempty"`
}

// normalize 规范化并校验技能候选（Phase D 补全；Phase B 是最小可用版本）。
func (c *SkillCandidate) normalize() error {
	if c == nil {
		return errors.New("skill candidate is nil")
	}
	n := sanitizeSegment(c.Name)
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
	for _, ref := range c.MemoryRefs {
		if _, err := normalizeMemoryRef(ref); err != nil {
			return fmt.Errorf("skill %s memory ref %q: %w", c.Name, ref, err)
		}
	}
	return nil
}

// checkNonEmpty 非空检查（Phase D 会加上标题/资源等更细的检查）。
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
