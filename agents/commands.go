package agents

import (
	"fmt"
	"github.com/seven7628/hai-harness/agents/prompt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/skills"
	"strings"
	"time"
)

// injectCommands 消费内部指令消息（命令框架）：遍历 msgs 剥出 command 消息执行，
// 返回剔除后的消息（指令消息本身不送 LLM、不落盘）；命令产出的上下文内容
// （如 skills load 的技能指令）**原位**替换该指令消息 —— 位置即命令发生的时刻
// （正文跟在命令之后时顺序天然正确：技能指令在前、用户任务在后），并同步进本轮
// 历史（RunHistory，与工具结果回写同一语义：append-only 日志记下模型看到了什么）。
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
		if !m.IsCommandMessage() {
			kept = append(kept, m)
			continue
		}
		if injected, ok := a.executeCommand(ac, opts, m); ok {
			kept = append(kept, injected)
			ac.RunHistory = append(ac.RunHistory, injected)
		}
	}
	return kept
}

// executeCommand 执行单条指令消息，返回该命令的上下文产物（injected=true 时调用方
// 需把它放进消息列表，位置 = 被消费的指令消息处）。
// 命令失败不 kill Run（带外操作）：结果经 CommandResult 事件回传调用者
// （compact 例外：结果经 CompressStart/End 事件感知，不重复发 CommandResult）。
func (a *AgentLoop) executeCommand(ac *AgentContext, opts *RunOptions, m core.Message) (core.Message, bool) {
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
			return core.Message{}, false
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
			return core.Message{}, false
		}
		// "skills load <name>"：用户主动加载技能（越过 load_skill 工具，产品驱动）——
		// 与模型 load_skill **同构**：同一段指令正文（skills.InstructionsBlock）追加进
		// 对话历史，唯一差别是决策来源（用户 vs 模型）。
		//
		// 为何不进系统提示词（2026-09-24 改，此前为 composeSystemPrompt 末尾的
		//「已加载技能」层）：provider 的缓存前缀顺序是 tools → system → messages，
		// 会话中途给 system 加层 ⇒ 已有整段上下文都不再是本请求的前缀，按全价重写一次
		// 缓存（1h 档写入价 2× 基础输入价；上下文越长代价越大，正是长会话最贵的时刻）。
		// 历史尾插只把新增消息写进缓存，前缀逐字不动。
		if skillName, ok := strings.CutPrefix(strings.TrimSpace(args), "load "); ok && strings.TrimSpace(skillName) != "" {
			skillName = strings.TrimSpace(skillName)
			s, err := a.cfg.Skills.Load(skillName)
			if err != nil {
				emit("", err.Error())
				return core.Message{}, false
			}
			emit(fmt.Sprintf("skill %q loaded into context", skillName), "")
			return core.NewSkillMessage(prompt.LoadedSkillHeader + skills.InstructionsBlock(s)), true
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
			return core.Message{}, false
		}
		emit("", fmt.Sprintf("unknown command %q", name))
	}
	return core.Message{}, false
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
