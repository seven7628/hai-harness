package agents

import (
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// Memory 命中的可观测性（Phase A 收尾，不改变任何运行时行为）。
//
// 要回答的问题是可测量的那一个：*模型在 memory_view 命中之后，还额外做了多少
// 仓库探查？* 真实任务评估里观察到过「成功读取记忆后仍连续 bash/read_file」，
// 但当时没有任何读数能证明或否证它。没有读数就只能靠印象改提示词。
//
// 因此本文件只做记账，不做任何干预：
//   - 不阻断、不改写、不注入提醒；
//   - AgentContext 上的字段全部无消费者时也不产生任何输出差异；
//   - 事件只在本次 Run **确实发生过 memory_view 成功**时发出，其余 run 零噪声。

const (
	memoryViewToolName  = "memory_view"
	memoryWriteToolName = "memory_write"
)

// memoryProbeTools 视为「探查仓库事实」的工具集合。
//
// 口径：命中记忆后，读代码/跑命令确认同一件事，属于重复劳动；写文件/改文件/
// 与用户交互不属于，因此不进这个集合。集合是**读数口径**而非权限判断 ——
// 无论是否命中，这些工具的可用性都由既有的模式/审批/hook 机制决定。
var memoryProbeTools = map[string]bool{
	"bash":       true,
	"read_file":  true,
	"grep":       true,
	"glob":       true,
	"run_python": true,
}

// noteMemoryTools 在每个工具批结束后记账。
//
// 命中轮口径：只有**先于**本批的工具轮已经成功命中过记忆，本批里的探查才计入
// postHit —— 同一批里的调用是模型在看到记忆内容之前就一并发起的，把它算成
// 「命中后仍探查」会把并发调用误判成不信任记忆。
func noteMemoryTools(ac *AgentContext, calls []core.ToolCall, results []core.ToolResult) {
	if ac == nil || len(results) == 0 {
		return
	}
	nameByID := make(map[string]string, len(calls))
	for _, c := range calls {
		nameByID[c.Id] = c.Name
	}

	// 先结算命中，再判定本批探查：批内顺序无意义（模型同轮发起），只有跨轮先后可判。
	for _, r := range results {
		if r.IsError || nameByID[r.Id] != memoryViewToolName {
			continue
		}
		ac.memoryViews++
		if ac.memoryHitRound == 0 {
			ac.memoryHitRound = ac.toolRound
		}
	}
	for _, r := range results {
		if r.IsError {
			continue
		}
		switch name := nameByID[r.Id]; name {
		case memoryViewToolName, memoryWriteToolName:
			// memory_write 不计入探查：它是记忆维护动作，不是重复探索。
		default:
			if ac.memoryHitRound > 0 && ac.memoryHitRound < ac.toolRound && memoryProbeTools[name] {
				if ac.postHitTools == nil {
					ac.postHitTools = make(map[string]int, 4)
				}
				ac.postHitTools[name]++
				ac.postHitProbeCalls++
			}
		}
	}
}

// emitMemoryProbe 在 Run 自然结算处发出命中读数。
//
// 边界：
//   - 只统计**主 agent**的 Run（Depth > 0 直接跳过）——记忆工具对 subagent
//     本就不可用，子运行出现这些工具名只可能是异常，不该混进质量读数；
//   - 没有任何 memory_view 时不发出（Views == 0），绝大多数 run 零噪声；
//   - abort / 出错路径不与 AgentEnd 的结算语义混在一起：读数只在正常收尾处
//     结算一次，缺席即表示该 run 没有可比的探查数据。
func (a *AgentLoop) emitMemoryProbe(ac *AgentContext) {
	if ac == nil || ac.memoryViews == 0 || ac.Depth > 0 {
		return
	}
	tools := make(map[string]int, len(ac.postHitTools))
	for k, v := range ac.postHitTools {
		tools[k] = v
	}
	ac.Handler(ac.ctx, &events.MemoryProbe{
		RunId:             ac.RunId,
		ParentRunId:       ac.ParentRunId,
		Views:             ac.memoryViews,
		PostHitProbeCalls: ac.postHitProbeCalls,
		PostHitTools:      tools,
		EventType:         events.MemoryProbeType,
	})
}
