package agents

import (
	"reflect"
	"strings"

	"github.com/seven7628/hai-harness/memory"
)

// MemoryProvider 是 AgentLoop 对长期记忆的最小依赖。
//
// 设计边界：AgentLoop 只消费已经打开的 Repository Memory，不负责解析 git 身份、
// 创建目录或决定写入时机。这样 SDK 可以在没有 Memory 的场景保持零行为变化，
// 宿主/Bridge 负责在正确的工作区装配具体实现。
type MemoryProvider interface {
	// BootstrapIndex 返回当前仓库的有界 MEMORY.md 索引。
	// 返回错误时不注入任何内容，不阻断普通 Run。
	BootstrapIndex() (*memory.Index, error)
}

// memoryProtocol 告诉模型记忆怎么用、什么时候信、什么时候写。
//
// 措辞取向：短句 + 祈使句 + 明确的不变式。第三条（命中后不重复探查）是本次真实
// 任务评估暴露的问题的直接对策——评估中出现过「成功 memory_view 之后仍连续
// bash/read_file 探查同一件事」。协议只**陈述期望行为**，不改变任何工具可用性：
// 违反不会失败，agents/memory_probe.go 负责把违反量变成可测读数。改成硬约束
// （拦截探查工具）会误伤合法验证——记忆可能确实过期，模型有权核对。
const memoryProtocol = `## Memory Protocol
- Project Memory is a map of previously recorded, repository-scoped experience.
- Read a topic with memory_view when its title or description matches the task.
- Once a topic answers the question, use it as the answer. Do not re-derive the same fact by grepping, reading, or running the same commands again.
- Verify with the repository only when the topic names something concrete that could have changed, and say that you verified it.
- Current repository files, tests, CI, AGENTS.md, and explicit user instructions take priority over an older memory.
- Write only non-obvious, reusable knowledge that is not already derivable from the repository, and not something you just read from a file.`

// memoryIndexBlock 将索引转换为稳定的 system prompt 层。
// 索引本身已经是 Markdown；这里只加固定协议，不把 Topic 正文带入 system。
func memoryIndexBlock(p MemoryProvider) string {
	if p == nil {
		return ""
	}
	// 防止宿主把 *memory.MemoryService 的 nil 指针装进接口：接口本身非 nil，
	// 但调用方法会 panic。Phase A 允许无法打开仓库身份时保持禁用。
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return ""
		}
	}
	idx, err := p.BootstrapIndex()
	if err != nil || idx == nil || idx.Raw == "" || strings.Contains(idx.Raw, "- (no topics yet)") {
		return ""
	}
	return "\n\n" + memoryProtocol + "\n\n" + idx.Raw
}
