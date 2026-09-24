package agents

import (
	"fmt"
	"github.com/seven7628/hai-harness/agents/prompt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"strings"
	"time"
)

// injectCommands 消费内部指令消息（命令框架）：遍历 msgs 剥出 command 消息执行，
// 返回剔除后的消息（不送 LLM、不入 ac.Messages 的持久化链）。
//
// 调用点（必须保证的顺序）：
//  1. RunStream 入口、system 注入之前 —— 指令消息是 system 角色且 messageText
//     为空（command 块不在 text/summary 白名单），若不先剥出会被「内容比较替换」
//     当旧 system 覆盖（首条命令丢失，见 B 节「消费点在 system 注入之前」）；
//  2. 每轮循环顶部、maybeCompact 之前 —— 运行中命令（Poll 插话检查点注入的
//     输入可能含命令消息；onTurn 检查点先于本点，persist 过滤兜底双保险）。
func (a *AgentLoop) injectCommands(ac *AgentContext, opts *RunOptions, msgs []core.Message) []core.Message {
	kept := make([]core.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.IsCommandMessage() {
			a.executeCommand(ac, opts, m)
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

// executeCommand 执行单条指令消息。命令失败不 kill Run（带外操作）：
// 结果经 CommandResult 事件回传调用者（compact 例外：结果经 CompressStart/End
// 事件感知，不重复发 CommandResult）。
func (a *AgentLoop) executeCommand(ac *AgentContext, opts *RunOptions, m core.Message) {
	name, args := parseCommand(m)
	emit := func(result, errMsg string) {
		ac.Handler(ac.ctx, &events.CommandResult{
			RunId:     ac.RunId,
			Name:      name,
			Result:    result,
			Error:     errMsg,
			Timestamp: time.Now(),
			EventType: events.CommandResultType,
		})
	}

	switch name {
	case "compact":
		if a.cfg.Compressor == nil {
			emit("", "compression is not configured")
			return
		}
		// 置强制标志：本轮结算段 maybeCompact(forced=true) 直接压缩
		//（跳过 ShouldCompact 预估；空历史语义复用：CompressEnd 带说明不报错）
		ac.forceCompact = true
	case "clear":
		// 清空上下文：置强制标志，循环顶部清空全部历史（仅保留 system）后 Stop 结束本轮
		ac.forceClear = true
		emit("context cleared", "")
	case "skills":
		if a.cfg.Skills == nil {
			emit("", "skills registry is not configured")
			return
		}
		// "skills load <name>"：强制加载（越过 load_skill 工具，产品驱动）——
		// 下轮 composeSystemPrompt 拼入「已加载技能」层；与 load_skill 并存
		//（LLM 自主 vs 产品强制），system 层一次断层（低频可接受）。
		if name, ok := strings.CutPrefix(strings.TrimSpace(args), "load "); ok && strings.TrimSpace(name) != "" {
			skillName := strings.TrimSpace(name)
			if _, err := a.cfg.Skills.Load(skillName); err != nil {
				emit("", err.Error())
				return
			}
			ac.loadedSkill = skillName
			emit(fmt.Sprintf("skill %q loaded into system prompt", skillName), "")
			return
		}
		// "skills"：发现列表（渐进披露的清单视图，同 buildSkillManifest 数据源）
		var b strings.Builder
		for _, d := range a.cfg.Skills.List() {
			fmt.Fprintf(&b, "- %s: %s\n", d.Name, d.Description)
		}
		emit(b.String(), "")
	default:
		// 自定义命令：经 Session 注入的执行器（注册表查 miss 报错）
		if opts != nil && opts.CommandHandler != nil {
			err := opts.CommandHandler(ac.ctx, name, args)
			if err != nil {
				emit("", err.Error())
			} else {
				emit("", "")
			}
			return
		}
		emit("", fmt.Sprintf("unknown command %q", name))
	}
}

// parseCommand 解析指令消息：Content[0].Content = "name\nargs"。
// 编码约定（core.NewCommandMessage）：命令名不含换行（RegisterCommand 校验），
// \n 分隔无歧义；args 为自由文本。
func parseCommand(m core.Message) (name, args string) {
	raw := ""
	for _, c := range m.Content {
		if c.Type == core.ContentTypeCommand {
			raw = c.Content
			break
		}
	}
	name, args, _ = strings.Cut(raw, "\n")
	return
}

// appendLoadedSkill 层4「已加载技能」：在 composeSystemPrompt 结果尾部追加
// 产品强制加载的技能完整指令（格式与 load_skill 工具输出同构，LLM 视角一致）。
// 技能加载失败（目录被删等异常）静默跳过 —— system 保持现状，命令执行时已校验过存在。
func (a *AgentLoop) appendLoadedSkill(base string, ac *AgentContext) string {
	if ac == nil || ac.loadedSkill == "" || a.cfg.Skills == nil {
		return base
	}
	s, err := a.cfg.Skills.Load(ac.loadedSkill)
	if err != nil {
		return base
	}
	var b strings.Builder
	b.WriteString(base)
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(prompt.LoadedSkillHeader)
	fmt.Fprintf(&b, "# Skill: %s\n\n%s", s.Name, s.Instructions)
	if len(s.Resources) > 0 {
		b.WriteString("\n" + prompt.LoadedSkillResourcesHeader)
		for _, r := range s.Resources {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}
	return b.String()
}
