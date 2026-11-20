p = '/Users/admin/workspace/go-code/desktop/bridge/view_reducer.go'
s = open(p, encoding='utf-8').read()

# 1) Checkpoint.DetachedCards
old = """	// BlocksTotal 是**历史块总数**（窗口恢复时 Blocks 只是窗口 + 卡片块）。
	// PagedBlocks 非 nil 表示还有未解码的块在 sidecar 里，按需解。
	BlocksTotal int          `json:"blocks_total,omitempty"`
	PagedBlocks *PagedBlocks `json:"-"`
"""
new = """	// BlocksTotal 是**历史块总数**（窗口恢复时 Blocks 只是窗口 + 卡片块）。
	// PagedBlocks 非 nil 表示还有未解码的块在 sidecar 里，按需解。
	BlocksTotal int          `json:"blocks_total,omitempty"`
	PagedBlocks *PagedBlocks `json:"-"`

	// DetachedCards 是**窗口外卡片的头部**（items 只留条数，正文不进内存）。
	//
	// 为什么单独一份：这些卡片的正文在交付时**必然**被 detachAgents 置空
	// （实测该会话 2017 张 / 119.9MB，而真正的对话尾部只有 1.7MB）⇒
	// 把正文解码进 reducer 的堆是纯浪费。
	// ⇒ 但它们**不能**进 reducer 的 blocks（blocks 是 checkpoint 的来源 ⇒
	//    瘦卡片会被写回 sidecar ⇒ 覆盖完整转录 ⇒ 不可逆丢数据）
	// ⇒ ⇒ 单独存头部，交付时与 blocks[:start] 合成候选（见 MarshalTail）。
	//
	// 与今天的等价性由 detach_agents_equivalence_test.go 守着。
	DetachedCards []Block `json:"-"`
"""
assert s.count(old) == 1, ("checkpoint", s.count(old))
s = s.replace(old, new)

# 2) ViewReducer.detachedCards
old2 = """	// agentRunIdx 是「存在 agent 卡片」的 run_id 集合（O(1) 判子 run）。
"""
new2 = """	// detachedCards 是**窗口外卡片的头部**（正文不进内存，见 Checkpoint.DetachedCards）。
	// 交付时与 blocks[:start] 合成候选；全量路径（ensureFullBlocksLocked）
	// 解出完整卡片后会清空它（否则同一张卡会被交付两次）。
	detachedCards []Block

	// agentRunIdx 是「存在 agent 卡片」的 run_id 集合（O(1) 判子 run）。
"""
assert s.count(old2) == 1, ("field", s.count(old2))
s = s.replace(old2, new2)

# 3) Restore：存 detachedCards
old3 = """	r.paged = cp.PagedBlocks
	r.blocksTotal = cp.BlocksTotal
"""
new3 = """	r.paged = cp.PagedBlocks
	r.detachedCards = cp.DetachedCards
	r.blocksTotal = cp.BlocksTotal
"""
assert s.count(old3) == 1, ("restore", s.count(old3))
s = s.replace(old3, new3)

# 4) ensureFullBlocksLocked：清空 detachedCards（卡片已解出完整内容）
old4 = """	r.paged = nil
	r.blocks = out
	r.blockIdxDirty = true
	r.invalidateMarshalCache()
	return nil
}
"""
new4 = """	r.paged = nil
	r.blocks = out
	// 卡片已被解出**完整**内容并合入 r.blocks ⇒ 清空头部副本，
	// 否则同一张卡会在交付时被算两遍（头部 + 完整块）。
	r.detachedCards = nil
	r.blockIdxDirty = true
	r.invalidateMarshalCache()
	return nil
}
"""
assert s.count(old4) == 1, ("full", s.count(old4))
s = s.replace(old4, new4)

# 5) MarshalTail：候选 = 头部 ++ blocks[:start]
old5 = """	agents := detachAgents(blocks[:start])
"""
new5 = """	//
	// 候选 = 窗口外卡片的**头部**（打开时现取）++ 尾部窗口之前滑出来的块
	// ⇒ 顺序与「改造前 r.blocks 里卡片在前、尾部在后」的结构一致
	// ⇒ detachAgents 挑出来的集合与改造前**逐字节相同**
	// （等价性测试 detach_agents_equivalence_test.go 守着这条）。
	candidates := make([]Block, 0, len(r.detachedCards)+start)
	candidates = append(candidates, r.detachedCards...)
	candidates = append(candidates, blocks[:start]...)
	agents := detachAgents(candidates)
"""
assert s.count(old5) == 1, ("agents", s.count(old5))
s = s.replace(old5, new5)

open(p, 'w', encoding='utf-8').write(s)
print("patched view_reducer.go")
