package evolution

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/memory"
)

// Verdict 单条校验的结论。
type Verdict struct {
	// Gate 校验器名字（用于报告与测试断言）。
	Gate string `json:"gate"`
	// Pass 是否通过。
	Pass bool `json:"pass"`
	// Reason 未通过时的原因（人可读）。
	Reason string `json:"reason,omitempty"`
}

// Validator 一条可注入的静态校验门禁（Phase B 的扩展点）。
//
// 注入式设计的原因：门禁的可信度完全来自"能被独立替换与扩充"——如果校验写死
// 在流程里，宿主就无法按仓库类型加规则，测试也无法构造"某条门禁失败"的场景。
// 因此 Verify 只认接口，DefaultValidatorChain 才是"默认装了什么"的答案。
//
// 实现必须满足：
//   - **无副作用**：不改文件、不发网络请求、不调用 LLM（静态门禁的定义）；
//   - **确定性**：同输入同结论（Replay 才是有副作用的那一层）；
//   - 返回 error 即失败；返回 nil 即通过。
type Validator interface {
	// Name 门禁名（出现在报告里，必须稳定）。
	Name() string
	// Check 校验一条候选。
	Check(c *Candidate) error
}

// ValidatorFunc 把普通函数适配成 Validator。
type ValidatorFunc struct {
	ValidatorName string
	Fn            func(c *Candidate) error
}

func (f ValidatorFunc) Name() string { return f.ValidatorName }
func (f ValidatorFunc) Check(c *Candidate) error {
	if f.Fn == nil {
		return errors.New("evolution: validator has nil function")
	}
	return f.Fn(c)
}

// 默认门禁名（稳定标识，报告与测试都按名字断言）。
const (
	GateScope        = "scope"
	GateUTF8         = "utf8"
	GateNotEmpty     = "not-empty"
	GateNoPrivileged = "no-privileged-surface"
	GateNoDerivative = "not-derivable-from-repo"
	GateNoDupe       = "no-duplicate"
)

// DefaultValidatorChain Phase B 的默认静态门禁链（按顺序执行，全部通过才算通过）。
//
// 为什么是这个顺序：先做**最便宜、最不可能误判**的（标识/编码/非空），
// 再做**需要扫描内容**的（越权面/可推导性/重复），贵的放后面。任何一个失败都立即
// 短路——后面的门禁没有意义（一个空 Topic 通过"无越权"不代表它值得写）。
func DefaultValidatorChain() []Validator {
	return []Validator{
		ScopeValidator(),
		UTF8Validator(),
		NonEmptyValidator(),
		PrivilegedSurfaceValidator(),
		DerivabilityValidator(),
	}
}

// ScopeValidator 范围门禁：候选必须属于当前仓库、必须是 topic 或 skill。
//
// 这是 Phase A「Repository-scoped」不变式在候选侧的复述：跨仓库候选即使内容
// 完美也不能进本仓库的索引。
func ScopeValidator() Validator {
	return ValidatorFunc{ValidatorName: GateScope, Fn: func(c *Candidate) error {
		if c.RepoID == "" {
			return errors.New("candidate has no repo id")
		}
		switch c.Kind {
		case KindTopic, KindSkill:
		default:
			return fmt.Errorf("unknown candidate kind %q", c.Kind)
		}
		return nil
	}}
}

// UTF8Validator 编码门禁：所有自由文本字段必须是合法 UTF-8。
//
// 为什么要单独一条：模型输出截断的多字节字符会产生替换字符，肉眼几乎不可见，
// 但会让"从文件读到的"与"从 API 读到的"结构不一致——这类问题在 Phase A 是靠
// 解析端归一解决的，候选是结构化 JSON（没有解析端），所以必须显式拦。
func UTF8Validator() Validator {
	return ValidatorFunc{ValidatorName: GateUTF8, Fn: func(c *Candidate) error {
		fields := map[string]string{
			"topic.body":    c.Topic.Body,
			"topic.title":   c.Topic.Title,
			"topic.summary": c.Topic.Summary,
			"rationale":     c.Rationale,
		}
		for name, v := range fields {
			if !validUTF8(v) {
				return fmt.Errorf("field %s is not valid UTF-8", name)
			}
		}
		if c.Skill != nil && !validUTF8(c.Skill.Instructions) {
			return errors.New("field skill.instructions is not valid UTF-8")
		}
		return nil
	}}
}

// NonEmptyValidator 非空门禁：候选必须真的带内容。
//
// 空 Topic 过了校验也会浪费一次索引名额并让 memory_view 拿到空字符串。
func NonEmptyValidator() Validator {
	return ValidatorFunc{ValidatorName: GateNotEmpty, Fn: func(c *Candidate) error {
		if c.Kind == KindSkill {
			if c.Skill == nil {
				return errors.New("skill candidate has nil payload")
			}
			return c.Skill.checkNonEmpty()
		}
		if strings.TrimSpace(c.Topic.Body) == "" {
			return errors.New("topic body is empty")
		}
		return nil
	}}
}

// forbiddenSurfaceRe 匹配"试图通过记忆/技能改变受保护对象"的措辞。
//
// 为什么用词法而不是结构化检查：候选内容是自由 Markdown，宿主无法从结构上
// 证明它不会诱导越权；这里做**保守的关键词拦截**。它有假阳性（讨论"不要关闭
// sandbox"的文档也会命中），所以它是"软拦截"：命中即拒绝，但报告里明确标注，
// 宿主可以改成告警。这比"放任一篇教人关闭 sandbox 的记忆进索引"要好。
var forbiddenSurfaceRe = regexp.MustCompile(`(?i)\b(disallow|disable|bypass|override|weaken|turn\s+off|skip|remove|lower|relax)\b[^.\n]{0,40}\b(sandbox|approval|permission|permissions|hook\s+trust|mcp\s+trust|oauth|api\s*key|credential|credentials|plugin\s+trust|evaluator|grader|safety\s*(gate|check|rule))\b`)

// 另一条更宽的兜底：直接出现"关闭/取消"与受保护名词的相邻搭配（中文也覆盖）。
var forbiddenSurfaceCJKRe = regexp.MustCompile(`(?i)(关闭|取消|绕过|跳过|禁用|解除|降低|放宽)[^。\n]{0,20}(沙箱|沙盒|审批|权限|凭证|凭据|密钥|钩子信任|插件信任|评估器|评审器|安全(门禁|检查|规则))`)

// PrivilegedSurfaceValidator 越权面门禁：拒绝建议修改受保护对象的候选。
//
// 受保护对象清单与自进化方案第 7 节一致：评估器、权限、Sandbox、Hook Trust、
// MCP Trust、Provider 翻译、凭证、插件信任。**结构上**本包无法改这些系统
// （没有任何写它们的代码路径），但记忆内容可以*建议*这么做，所以要在内容层拦。
func PrivilegedSurfaceValidator() Validator {
	return ValidatorFunc{ValidatorName: GateNoPrivileged, Fn: func(c *Candidate) error {
		blob := c.Topic.Title + "\n" + c.Topic.Summary + "\n" + c.Topic.Body
		if c.Skill != nil {
			blob += "\n" + c.Skill.Description + "\n" + c.Skill.Instructions
		}
		if forbiddenSurfaceRe.MatchString(blob) || forbiddenSurfaceCJKRe.MatchString(blob) {
			return fmt.Errorf("%w: content suggests changing a protected surface", ErrForbiddenTarget)
		}
		return nil
	}}
}

// derivableRe 匹配"这句话就是在复述仓库里已有的东西"的结构。
//
// 依据是评估协议里"坏 Memory"清单的第一条：与代码/README/CI/AGENTS.md 冲突或
// 完全可推导的记忆是纯成本（占索引名额 + 引入过期风险）。
//
// 口径说明：这是**启发式**，只看表述结构（"see X for Y" 这类指针句），
// 不去真的读仓库比对——真比对需要宿主注入的仓库快照，超出静态门禁的职责。
// 它的假阳性（确实值得写的"参考清单"）由宿主通过自定义链覆盖掉这一条。
//
// 触发条件刻意收窄为：整行就是一个"看 X"的动作，且行内出现具体文件名。
// 正常记忆正文里顺带提到一个文件（"构建脚本见 make/ci.mk 的 target ci"）不该被拦。
var derivableRe = regexp.MustCompile(`(?im)^\s*(see|refer\s+to|read|详见|参见)\b[^\n]{0,60}?\b[\w./-]+\.(md|json|ya?ml|toml|go|ts|py)\b`)

// DerivabilityValidator 可推导性门禁：拒绝纯指针型 / 复述型内容。
func DerivabilityValidator() Validator {
	return ValidatorFunc{ValidatorName: GateNoDerivative, Fn: func(c *Candidate) error {
		body := strings.TrimSpace(c.Topic.Body)
		if body == "" {
			return nil
		}
		if derivableRe.MatchString(body) {
			return errors.New("body is only a pointer to repository files; record the non-obvious fact instead")
		}
		// 纯标题式内容（"## Rule" 下面只有标题没有正文）没有行动价值。
		if strings.Trim(body, "# \n\t") == "" {
			return errors.New("body has no prose")
		}
		return nil
	}}
}

// WithDuplicateGuard 在链尾追加一条"与现有记忆重复"的门禁。
//
// 与既有索引/主题的重复比对需要读 Phase A 的 store，因此**不**放进
// DefaultValidatorChain（那会让默认链带 IO 依赖）；由 Service.Verify 在
// 组装实际执行链时按需追加。这样默认链保持纯函数、可单测。
func WithDuplicateGuard(existing func(name string) (memory.Topic, bool)) Validator {
	return ValidatorFunc{ValidatorName: GateNoDupe, Fn: func(c *Candidate) error {
		if c.Kind != KindTopic || existing == nil {
			return nil
		}
		if old, ok := existing(c.Topic.Name); ok && strings.TrimSpace(old.Body) == strings.TrimSpace(c.Topic.Body) {
			return fmt.Errorf("topic %q already exists with identical content", c.Topic.Name)
		}
		return nil
	}}
}

// VerificationReport 一次门禁链执行的报告（写入候选的 report 字段）。
//
// 报告必须**完整**记录所有执行过的门禁（包括通过的），因为事后审计需要回答
// "这条记忆当时经过了哪些检查"——只记失败项就答不上。
type VerificationReport struct {
	// CandidateID 被校验的候选 id。
	CandidateID string `json:"candidate_id"`
	// Verdict 各门禁结论（按执行顺序）。
	Verdicts []Verdict `json:"verdicts"`
	// Replay Replay 结果（未执行时为 nil，Skipped 为真）。
	Replay *ReplayResult `json:"replay,omitempty"`
	// ReplaySkipped 为真表示未注入 Replayer，按策略跳过了 Replay。
	ReplaySkipped bool `json:"replay_skipped,omitempty"`
	// Passed 全链路是否通过。
	Passed bool `json:"passed"`
	// At 执行时间。
	At time.Time `json:"at"`
}

// firstFailure 返回第一个失败门禁（用于生成 Reject 原因）。
func (r *VerificationReport) firstFailure() *Verdict {
	if r == nil {
		return nil
	}
	for i := range r.Verdicts {
		if !r.Verdicts[i].Pass {
			return &r.Verdicts[i]
		}
	}
	return nil
}

// String 把报告压成一行人可读摘要（工具返回 / 日志用）。
func (r *VerificationReport) String() string {
	if r == nil {
		return "<no report>"
	}
	parts := make([]string, 0, len(r.Verdicts))
	for _, v := range r.Verdicts {
		mark := "ok"
		if !v.Pass {
			mark = "fail:" + v.Reason
		}
		parts = append(parts, v.Gate+"="+mark)
	}
	if r.ReplaySkipped {
		parts = append(parts, "replay=skipped")
	} else if r.Replay != nil {
		parts = append(parts, fmt.Sprintf("replay=%s", r.Replay.Verdict))
	}
	return fmt.Sprintf("passed=%t [%s]", r.Passed, strings.Join(parts, " "))
}
