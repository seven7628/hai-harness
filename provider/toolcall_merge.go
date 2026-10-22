package provider

import (
	"sort"

	"github.com/sashabaranov/go-openai"

	"github.com/seven7628/hai-harness/core"
)

// toolCallAccumulator 流式工具调用的「双索引」合并器（2026-10，对齐 pi
// packages/ai/src/api/openai-completions.ts:485-541）。
//
// 背景：工具调用是**分帧**下发的 —— name/id 通常只在首帧，arguments 拆成多帧。
// 拼接时必须有「这一帧属于哪个调用」的身份，而协议给了两个：
//   - index：本帧是第几个调用（顺序）；
//   - id：调用 ID（稳定身份）。
//
// 只用 index 会在两种真实形态下拼错（2026-10 用 httptest 复现）：
//  1. 网关全程不发 index + 两个并行调用 → 两个调用并成一个，arguments 串成垃圾
//     （`{"path":"a"}{"path":"b"}`）、id 被后者覆盖；
//  2. 首帧带 index、续帧只带 id → 凭空多出一个无名调用，原调用参数被截断。
//
// 规则：先按 index 找，再按 id 找；新建时**双登记**；后到的 index/id 只补内部映射。
// `call.Index`（已随 start/delta 事件发出）一旦确定**不再变更** —— 前端按它分组，
// 中途改值会让同一调用被拆成两行。
type toolCallAccumulator struct {
	byIndex map[int]*core.ToolCall
	byID    map[string]*core.ToolCall
	all     []*core.ToolCall
	used    map[int64]bool
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{
		byIndex: make(map[int]*core.ToolCall),
		byID:    make(map[string]*core.ToolCall),
		used:    make(map[int64]bool),
	}
}

// resolve 取这一帧所属的调用；isNew=true 表示本次新建（调用方据此发 ToolCallStart 事件）。
func (a *toolCallAccumulator) resolve(requestId string, tc openai.ToolCall) (call *core.ToolCall, isNew bool) {
	if tc.Index != nil {
		call = a.byIndex[*tc.Index]
	}
	if call == nil && tc.ID != "" {
		call = a.byID[tc.ID] // 辅键：续帧只带 id 时靠它接上
	}
	if call == nil && tc.Index == nil && tc.ID == "" && len(a.all) > 0 {
		// 两个身份都没给：视为上一个调用的续帧（而不是每帧新建一个空调用）。
		call = a.all[len(a.all)-1]
	}
	if call == nil {
		idx := a.pickIndex(tc.Index)
		call = &core.ToolCall{
			Index:     idx,
			RequestId: requestId,
			Id:        tc.ID,
			Name:      tc.Function.Name,
		}
		a.used[idx] = true
		a.all = append(a.all, call)
		isNew = true
	}
	// 双登记（只补不改）：后到的 index / id 都进映射，后续帧任一可用即可续上。
	if tc.Index != nil {
		if _, exists := a.byIndex[*tc.Index]; !exists {
			a.byIndex[*tc.Index] = call
		}
	}
	if tc.ID != "" {
		a.byID[tc.ID] = call
		if call.Id == "" {
			call.Id = tc.ID
		}
	}
	if call.Name == "" && tc.Function.Name != "" { // 补而不覆盖（pi 同规则）
		call.Name = tc.Function.Name
	}
	return call, isNew
}

// pickIndex 该调用的稳定序号：显式 index 且未被占用就用它；否则取最小空闲序号
// （避免「先由 id 建出的调用占 0、随后另一个调用显式 index=0」造成重号）。
func (a *toolCallAccumulator) pickIndex(explicit *int) int64 {
	if explicit != nil && !a.used[int64(*explicit)] {
		return int64(*explicit)
	}
	for i := int64(0); ; i++ {
		if !a.used[i] {
			return i
		}
	}
}

// list 按 Index 稳定排序输出（创建顺序即 Index 分配顺序，故同值时仍是出现顺序）。
func (a *toolCallAccumulator) list() []core.ToolCall {
	out := make([]core.ToolCall, 0, len(a.all))
	for _, c := range a.all {
		out = append(out, *c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}
