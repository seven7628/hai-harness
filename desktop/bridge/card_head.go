package main

// 「窗口外卡片」的**头部投影**（2026-11-19，bridge 内存爬山第 5 步·第一小步）。
//
// ## 为什么需要它
//
// 打开一个大会话时，窗口里 98.6% 的字节是**窗口外的子智能体卡片**
// （实测 2017 张卡片 / 119.9MB，而真正的对话尾部只有 1.7MB）——
// 而 `detachAgents` **必然**把它们的 `Items` 置空（`slim.Items = nil`）
// ⇒ 那 120MB 正文在交付时全被扔掉 ⇒ 纯浪费。
//
// 但**不能**只把 items 置空就完事：reducer 的 blocks 是 checkpoint 的来源
// ⇒ 瘦卡片一旦进 reducer，提交时会被写进 checkpoint / sidecar
// ⇒ **转录被永久抹掉**（实测：全量校验路径会用 holdsSame 比摘要 ⇒ 不一致就重写）。
//
// ⇒ 所以卡片**不进 reducer 的 blocks**：它们留在 paged 集合里（span + id，
// 提交时按全集引用保住），交付时**现取头部**。
//
// ## 头部投影怎么做
//
// 整张卡片照常解码，只有 `items` 换成**零尺寸元素**（`[]struct{}`）：
//   · item **条数**照样拿得到（ItemsTrimmed 要用）⇒ `len(head.Items)`；
//   · item 的**内容**零分配（`[]struct{}` 的元素大小是 0 ⇒ 底层数组不占空间）。
//
// 靠 Go 的 json 规则实现：外层 `Items`（深度 0）遮蔽嵌入的 `Block.Items`（深度 1）
// ⇒ 解码器用外层那个 ⇒ `Block.Items` 保持 nil，`Items` 有条数。
//
// 实测（真实会话 603,477 事件 / 2017 张窗口外卡片）：
// 取最后 500 张卡片的头 = **148ms / 1.1MB**（对比「全解码」= 717ms / 720.5MB）。

import (
	"encoding/json"
	"fmt"

	"github.com/seven7628/hai-harness/session"
)

// cardHeadBlock 是卡片块的**头部投影**。
//
// 嵌入 Block 拿到全部头部字段（kind/run_id/task_id/label/status/todos/usage…），
// 只有 items 换成零尺寸元素。
type cardHeadBlock struct {
	Block
	Items []struct{} `json:"items,omitempty"`
}

// cardHeadFromSpan 从一个块的 span 里解出**头部**（items 只留条数）。
//
// 返回的 Block：Items 恒为 nil，ItemsTrimmed = 原值 + item 条数
// ⇒ 与 detachAgents 里「把 items 置空并记进 ItemsTrimmed」的结果**逐字节等价**。
func cardHeadFromSpan(span session.BlockSpan) (Block, error) {
	if !span.Compressed {
		// 未压缩块：Raw 指向 sidecar 的只读映射（不是堆）⇒ 直接解。
		var h cardHeadBlock
		if err := json.Unmarshal(span.Raw, &h); err != nil {
			return Block{}, err
		}
		return headToBlock(h), nil
	}
	var h cardHeadBlock
	// transient：解压缓冲只在回调期间有效 ⇒ 解析完就归池（不残留一份拷贝）。
	if err := span.LogicalTransient(func(raw []byte) error {
		return json.Unmarshal(raw, &h)
	}); err != nil {
		return Block{}, err
	}
	return headToBlock(h), nil
}

// headToBlock 把头部投影转成「items 已置空」的 Block。
func headToBlock(h cardHeadBlock) Block {
	b := h.Block
	if len(h.Items) > 0 {
		b.ItemsTrimmed += len(h.Items)
		b.Items = nil
	}
	return b
}

// pickCardHeadsFromSpans 从 span 列表里挑出**卡片头**（旧 → 新）。
//
// 判据与预算**完全复刻** detachAgents（见 view_reducer.go）：
//
//	· 只挑 kind ∈ {agent, async_task} 或（tool 且 promoted 且有 task_id）的块；
//	· 预算：4MB / 500 张，超了就停（从**新到旧**收集，最后翻回旧→新）。
//
// upperIdx = 候选范围的**上界**（spans 索引）。
//
// ⚠️ 语义要点（第一版踩过）：detachAgents 迭代的是 `blocks[:start]` 这个**前缀**
// ⇒ 候选集是 spans 索引**最小**的一段 ⇒ 遍历要从 upperIdx 往回，不是从最新往回。
func pickCardHeadsFromSpans(spans []session.BlockSpan, kinds []uint8, upperIdx int) ([]Block, error) {
	var picked []Block
	var total int64
	for i := upperIdx; i >= 0; i-- {
		if i < len(kinds) && !session.HasCardKind(kinds[i]) {
			continue
		}
		b, err := cardHeadFromSpan(spans[i])
		if err != nil {
			return nil, fmt.Errorf("card head %d: %w", i, err)
		}
		switch {
		case b.Kind == blockKindAgent, b.Kind == blockKindAsyncTask:
		case b.Kind == blockKindTool && b.Promoted && b.TaskID != "":
		default:
			continue
		}
		sz := detachedCardBytes(b)
		if len(picked) > 0 && (total+sz > detachAgentsBudgetBytes || len(picked) >= detachAgentsMaxCards) {
			break
		}
		total += sz
		picked = append(picked, b)
	}
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return picked, nil
}
