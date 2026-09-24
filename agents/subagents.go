package agents

import (
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/agents/prompt"
)

// SubagentDescriptor 自定义 subagent 的轻量元数据（发现阶段：仅 name/description）。
// 完整定义（Instructions/Tools/Model/Source）在 spawn_agent 激活时经
// DefinitionRegistry.Load 加载，不进系统提示词。
type SubagentDescriptor struct {
	Name        string
	Description string
}

// SubagentLister 自定义 subagent 注册表的只读视图（agents 包不依赖 subagent 包，
// 由 subagent.DefinitionRegistry 实现——避免 agents → subagent 循环依赖）。
type SubagentLister interface {
	List() []SubagentDescriptor
}

// WithSubagents 注入自定义 subagent 注册表：发现清单注入 system（渐进披露，
// 与 WithSkills 同层）。spawn_agent 工具在工具引擎侧注册（见 subagent 包）。
func WithSubagents(l SubagentLister) Option {
	return func(c *Config) { c.Subagents = l }
}

// buildSubagentManifest 组装自定义 subagent 清单（渐进披露的发现阶段：
// 仅元数据注入系统提示词，与 buildSkillManifest 同构）。
func buildSubagentManifest(l SubagentLister) string {
	if l == nil {
		return ""
	}
	list := l.List()
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(prompt.SubagentManifestHeader)
	for _, s := range list {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	return b.String()
}
