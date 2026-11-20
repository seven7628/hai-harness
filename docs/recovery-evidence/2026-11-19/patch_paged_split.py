p = '/Users/admin/workspace/go-code/desktop/bridge/view_reducer.go'
s = open(p, encoding='utf-8').read()

old = """type PagedBlocks struct {
	IDs     []uint64
	Spans   []session.BlockSpan
	Hashes  [][8]byte
	Release func()
}
"""
new = """type PagedBlocks struct {
	// IDs/Spans/Hashes：窗口外**全部**未解码的块（含卡片类块），按 id 升序。
	//
	// 两个用途：
	//   · ensureFullBlocksLocked 用它解出「全部历史」（要完整视图时）；
	//   · 提交时 allIDs = paged.IDs ∪ ids ⇒ 引用按**全集**写 ⇒
	//     卡片类块即使不在 reducer 手里，也不会被 pruneTo 从 sidecar 删掉
	//     （删了 = 不可逆丢数据，见 pruneTo 的事故说明）。
	IDs    []uint64
	Spans  []session.BlockSpan
	Hashes [][8]byte

	// PageIDs/PageSpans/PageHashes：UI「加载更早」要返回的**子集**
	// （与上面三个一一对应、同样按 id 升序）。
	//
	// 为什么必须与上面分开：卡片类块（子智能体/后台任务卡片）要进引用全集
	// （保住 sidecar 里的原文），但它们**不是对话正文** ⇒ 不该出现在
	// UI 的翻页结果里（否则左栏卡片会在时间线里重复出现）。
	//
	// nil ⇒ 与 IDs/Spans/Hashes 等价（旧行为；窗口外没有卡片类块时就是这种）。
	PageIDs    []uint64
	PageSpans  []session.BlockSpan
	PageHashes [][8]byte

	Release func()
}
"""
assert s.count(old) == 1, ("struct", s.count(old))
s = s.replace(old, new)

old2 = """	total = r.blocksTotal
	ids := r.paged.IDs
	end := sort.Search(len(ids), func(i int) bool { return ids[i] >= beforeID })
	start := end - limit
	if start < 0 {
		start = 0
	}
	if end <= 0 {
		return nil, 0, false, total, nil
	}
	spans := r.paged.Spans[start:end]
	var hashes [][8]byte
	if len(r.paged.Hashes) == len(ids) {
		hashes = r.paged.Hashes[start:end]
	}
"""
new2 = """	total = r.blocksTotal
	// 页集：默认与「全部未解码块」相同；有卡片类块时用单独的子集
	// （卡片不进 UI 的翻页结果，见 PagedBlocks 的注释）。
	ids := r.paged.IDs
	allSpans := r.paged.Spans
	allHashes := r.paged.Hashes
	if r.paged.PageIDs != nil {
		ids = r.paged.PageIDs
		allSpans = r.paged.PageSpans
		allHashes = r.paged.PageHashes
	}
	end := sort.Search(len(ids), func(i int) bool { return ids[i] >= beforeID })
	start := end - limit
	if start < 0 {
		start = 0
	}
	if end <= 0 {
		return nil, 0, false, total, nil
	}
	spans := allSpans[start:end]
	var hashes [][8]byte
	if len(allHashes) == len(ids) {
		hashes = allHashes[start:end]
	}
"""
assert s.count(old2) == 1, ("page", s.count(old2))
s = s.replace(old2, new2)

open(p, 'w', encoding='utf-8').write(s)
print("patched PagedBlocks split")
