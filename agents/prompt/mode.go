package prompt

// PlanModeHint 是 PlanMode 的行为契约片段,拼在 system 最尾部
// (稳定前缀在前,模式切换不破坏缓存命中)。行为约束靠机制
// (工具不可见 = 无法调用),Hint 仅提升计划输出质量。
//
// 中文释义(正文为英文):"你处于 plan 模式:用可用的只读工具研究问题并产出计划。
// 不要修改文件或运行变更类命令。计划就绪后用 plan_submit 工具提交,然后等待确认。"
const PlanModeHint = "You are in plan mode: research the problem and produce a plan using " +
	"the available read-only tools. Do not modify files or run mutating commands. " +
	"When the plan is ready, submit it with the plan_submit tool and wait for confirmation."
