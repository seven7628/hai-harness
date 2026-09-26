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

const memoryProtocol = `## Memory Protocol
- Project Memory is a map of previously recorded, repository-scoped experience.
- Read a topic with memory_view when its title or description matches the task.
- Current repository files, tests, CI, AGENTS.md, and explicit user instructions take priority over an older memory.
- Write only non-obvious, reusable knowledge that is not already derivable from the repository.`

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
