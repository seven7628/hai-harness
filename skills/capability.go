package skills

// capability.go：按「会话能力面」过滤技能（2026-10）。
//
// 要解决的问题：技能清单是**按会话**注入系统提示词的，而同一个工作区注册表被该
// 工作区的所有会话共享（desktop/bridge 的 workspaceRuntime.skills 就是这种共享）。
// 会话之间真正不同的是**选中的模型**与**该 persona 的工具集**：
//
//   - 纯文本模型（glm-4.6 / deepseek-chat）看不到图片 → 要求 image 输入的技能
//     （看图、截图复刻、OCR）在它下面必然做不成；
//   - Work persona 不注册 bash → 要求 bash 的技能同样做不成。
//
// 不做过滤的后果不是"报错"而是**更能误导的静默失败**：模型在清单里看到"截图复刻"
// 技能、load_skill 成功加载一大段流程、然后才发现自己看不见图 —— 白烧一轮上下文，
// 再向用户解释"我做不到"，而用户以为这个能力存在。
//
// 因此在**发现阶段**（清单）就滤掉，并在**激活阶段**（load_skill）拒绝，
// 两处同源（同一份 Capabilities），否则会出现"清单里没有但工具能加载"的错位。
//
// 谁负责构造 Capabilities：宿主（它认识 provider/model/工具集）。本包只认
// 已归一化的字符串集合，不认识 provider 概念 —— 与 Config.Skills 的分层理由一致。

import (
	"fmt"
	"sort"
	"strings"
)

// Capabilities 一次会话的能力面（由宿主按选中模型 + 工具集算好后注入）。
//
// 零值（&Capabilities{}）语义 = **什么都不能**（任何声明了 requires 的技能都不匹配）；
// nil（*Capabilities 为 nil）= **不过滤**（向后兼容：老宿主/测试/CLI 不知道能力这回事）。
// 这个区分很重要：把"没算"误当成"什么都不能"会让所有带 requires 的技能凭空消失。
type Capabilities struct {
	// Input 模型可接受的输入模态（"text" 恒有；"image"/"video"/"pdf" 视模型而定）。
	Input []string
	// Tools 本会话工具集里的工具名（如 run_python / bash / browser_navigate）。
	Tools []string
	// Label 人读标识（模型名，如 "glm-4.6"），只用于错误/提示文案。可为空。
	Label string

	inputs map[string]bool // 惰性索引
	tools  map[string]bool
}

// NewCapabilities 构造能力面并建立索引（切片后续被调用方改动不影响判定）。
func NewCapabilities(label string, input, tools []string) *Capabilities {
	c := &Capabilities{Label: label}
	c.Input = normalizeSet(input)
	c.Tools = normalizeSet(tools)
	c.reindex()
	return c
}

// reindex 重建查询索引（幂等；Input/Tools 被直接赋值时也走这里）。
func (c *Capabilities) reindex() {
	c.inputs = make(map[string]bool, len(c.Input))
	for _, s := range c.Input {
		c.inputs[strings.ToLower(strings.TrimSpace(s))] = true
	}
	c.tools = make(map[string]bool, len(c.Tools))
	for _, s := range c.Tools {
		c.tools[strings.ToLower(strings.TrimSpace(s))] = true
	}
}

// HasInput 模型是否接受该输入模态（大小写不敏感）。
func (c *Capabilities) HasInput(mode string) bool {
	if c == nil {
		return true // 未注入能力面 = 不过滤
	}
	if c.inputs == nil {
		c.reindex()
	}
	return c.inputs[strings.ToLower(strings.TrimSpace(mode))]
}

// HasTool 会话工具集里是否有该工具。
func (c *Capabilities) HasTool(name string) bool {
	if c == nil {
		return true
	}
	if c.tools == nil {
		c.reindex()
	}
	return c.tools[strings.ToLower(strings.TrimSpace(name))]
}

// Missing 返回该技能在当前能力面下缺失的需求（空 = 可用）。
// nil 能力面恒返回 nil（不过滤）。
//
// 未知的 input 值一律判为**不满足**（保守）：前端/技能里写错模态名时，宁可技能不出现，
// 也不要让模型以为"看见图"这件事成立却拿不到图。
func (c *Capabilities) Missing(r Requires) []string {
	if c == nil || r.Empty() {
		return nil
	}
	var missing []string
	for _, in := range r.Input {
		if !c.HasInput(in) {
			missing = append(missing, "model input: "+in)
		}
	}
	for _, t := range r.Tools {
		if !c.HasTool(t) {
			missing = append(missing, "tool: "+t)
		}
	}
	return missing
}

// reason 生成一句人读的不可用原因（技能名 + 缺什么 + 当前模型），供清单/工具错误复用。
// 措辞刻意写明「换一个模型」这条出路 —— 否则用户只会以为技能坏了。
func (c *Capabilities) reason(skillName string, missing []string) string {
	model := ""
	if c != nil && c.Label != "" {
		model = fmt.Sprintf("（当前模型 %s）", c.Label)
	}
	return fmt.Sprintf("skill %q is unavailable for this session%s: requires %s; "+
		"switch to a model or persona that provides it, or load the skill in a session that has it",
		skillName, model, strings.Join(missing, ", "))
}

// normalizeSet 去空、去重、保留原始大小写（大小写只在查询时归一）。
func normalizeSet(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		k := strings.ToLower(s)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
