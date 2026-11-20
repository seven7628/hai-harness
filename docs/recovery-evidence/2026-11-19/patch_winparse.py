p = '/Users/admin/workspace/go-code/desktop/bridge/checkpoint.go'
s = open(p, encoding='utf-8').read()

old = """	n := len(spans)
	start := n - limit
	need := make([]int, 0, limit+16)
	decoded := make(map[int]struct{}, limit+16)
	for i := 0; i < start; i++ {
		if session.HasCardKind(rec.LoadedBlockKinds[i]) {
			need = append(need, i)
			decoded[i] = struct{}{}
		}
	}
	for i := start; i < n; i++ {
		need = append(need, i)
		decoded[i] = struct{}{}
	}

	selSpans := make([]session.BlockSpan, len(need))
	selHashes := make([][8]byte, len(need))
	hasHashes := len(rec.LoadedBlockHashes) == n
	for j, i := range need {
		selSpans[j] = spans[i]
		if hasHashes {
			selHashes[j] = rec.LoadedBlockHashes[i]
		}
	}
	blocks := make([]Block, len(need))
	var hsh [][8]byte
	if hasHashes {
		hsh = selHashes
	}
	if err := decodeBlocksParallel(selSpans, hsh, blocks); err != nil {
		return nil, err
	}

	pagedIDs := make([]uint64, 0, start)
	pagedSpans := make([]session.BlockSpan, 0, start)
	pagedHashes := make([][8]byte, 0, start)
	for i := 0; i < start; i++ {
		if _, ok := decoded[i]; ok {
			continue
		}
		pagedIDs = append(pagedIDs, rec.LoadedBlockIDs[i])
		pagedSpans = append(pagedSpans, spans[i])
		if hasHashes {
			pagedHashes = append(pagedHashes, rec.LoadedBlockHashes[i])
		}
	}

	cp.Blocks = blocks
	cp.BlocksTotal = n
	cp.PagedBlocks = &PagedBlocks{
		IDs:     pagedIDs,
		Spans:   pagedSpans,
		Hashes:  pagedHashes,
		Release: rec.ReleaseView,
	}
	return &cp, nil
}
"""

new = """	n := len(spans)
	start := n - limit
	hasHashes := len(rec.LoadedBlockHashes) == n

	// ——— 尾部窗口：完整解码 ———
	tailSpans := make([]session.BlockSpan, 0, limit)
	tailHashes := make([][8]byte, 0, limit)
	for i := start; i < n; i++ {
		tailSpans = append(tailSpans, spans[i])
		if hasHashes {
			tailHashes = append(tailHashes, rec.LoadedBlockHashes[i])
		}
	}
	blocks := make([]Block, len(tailSpans))
	var hsh [][8]byte
	if hasHashes {
		hsh = tailHashes
	}
	if err := decodeBlocksParallel(tailSpans, hsh, blocks); err != nil {
		return nil, err
	}

	// ——— 窗口外的卡片类块：**不解码正文**，只取头部 ———
	//
	// 为什么（2026-11-19 实测）：这些卡片的 Items 在交付时**必然**被
	// detachAgents 置空（`slim.Items = nil`）⇒ 解它们的正文是纯浪费
	// （实测该会话 2017 张卡片 = 119.9MB，而真正的对话尾部只有 1.7MB）。
	// ⇒ 它们**不进 reducer 的堆**（reducer 的 blocks 是 checkpoint 的来源，
	//    瘦卡片进去会被写回 sidecar ⇒ 覆盖完整转录 ⇒ 不可逆丢数据）。
	// ⇒ 改成：留在 paged 的**引用全集**里（提交时按全集写 ⇒ sidecar 原文
	//    不会被 pruneTo 删掉），交付时**现取头部**（DetachedCards）。
	cardIdx := make([]int, 0, start)
	for i := 0; i < start; i++ {
		if session.HasCardKind(rec.LoadedBlockKinds[i]) {
			cardIdx = append(cardIdx, i)
		}
	}

	// 候选范围 = 最旧的 N 张卡片类块（N 与 detachAgents 的 start' 同口径：
	// start' = 卡片类块数 + 尾部存活数 - limit；尾部存活数要按**归一化后**算，
	// 因为 detachAgents 迭代的是归一化后的块数组）。
	// 详见 detach_agents_equivalence_test.go 的推导（实测 k=17 ⇒ N=2000）。
	probeBlocks := append([]Block(nil), blocks...)
	probe := &Checkpoint{Blocks: probeBlocks}
	normalizeDanglingState(probe)
	tailKept := len(probe.Blocks)
	candidateCount := len(cardIdx) + tailKept - limit
	if candidateCount < 0 {
		candidateCount = 0
	}
	upperIdx := -1
	if candidateCount > 0 {
		for j, i := range cardIdx {
			if j == candidateCount-1 {
				upperIdx = i
				break
			}
		}
	}
	var detached []Block
	if upperIdx >= 0 {
		var derr error
		detached, derr = pickCardHeadsFromSpans(spans, rec.LoadedBlockKinds, upperIdx)
		if derr != nil {
			return nil, derr
		}
	}

	// ——— paged 集合分两栏 ———
	//
	// 引用全集（IDs/Spans/Hashes）：窗口外的**全部**未解码块（含卡片类块）
	//   ⇒ 提交时 allIDs = paged.IDs ∪ ids ⇒ 卡片也在引用里 ⇒ 不被 pruneTo 删。
	// 页集（Page*）：**只有非卡片类块**
	//   ⇒ UI「加载更早」的结果里不该出现子智能体卡片。
	pagedIDs := make([]uint64, 0, start)
	pagedSpans := make([]session.BlockSpan, 0, start)
	pagedHashes := make([][8]byte, 0, start)
	pageIDs := make([]uint64, 0, start)
	pageSpans := make([]session.BlockSpan, 0, start)
	pageHashes := make([][8]byte, 0, start)
	for i := 0; i < start; i++ {
		pagedIDs = append(pagedIDs, rec.LoadedBlockIDs[i])
		pagedSpans = append(pagedSpans, spans[i])
		if hasHashes {
			pagedHashes = append(pagedHashes, rec.LoadedBlockHashes[i])
		}
		if session.HasCardKind(rec.LoadedBlockKinds[i]) {
			continue // 卡片不进页集
		}
		pageIDs = append(pageIDs, rec.LoadedBlockIDs[i])
		pageSpans = append(pageSpans, spans[i])
		if hasHashes {
			pageHashes = append(pageHashes, rec.LoadedBlockHashes[i])
		}
	}

	cp.Blocks = blocks
	cp.BlocksTotal = n
	cp.DetachedCards = detached
	cp.PagedBlocks = &PagedBlocks{
		IDs:       pagedIDs,
		Spans:     pagedSpans,
		Hashes:    pagedHashes,
		PageIDs:   pageIDs,
		PageSpans: pageSpans,
		PageHashes: pageHashes,
		Release:   rec.ReleaseView,
	}
	return &cp, nil
}
"""
assert s.count(old) == 1, ("parse", s.count(old))
s = s.replace(old, new)

open(p, 'w', encoding='utf-8').write(s)
print("patched parseCheckpointFromRecordWindow")
