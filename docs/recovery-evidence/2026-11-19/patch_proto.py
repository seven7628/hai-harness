p = '/Users/admin/workspace/go-code/desktop/bridge/detach_agents_equivalence_test.go'
s = open(p, encoding='utf-8').read()

old = """// candidateCount = 「尾部窗口之前的卡片类块」的个数（detachAgents 迭代的整个范围）；
// 它比"实际会被挑中的卡片数"大（task_result / task_delivery 会被跳过）。
func pickCardHeadsFromSpans(spans []session.BlockSpan, kinds []uint8, candidateCount int) ([]Block, int, int, error) {
	var picked []Block
	var total int64
	trimmedCount := 0
	keptItems := 0
	seen := 0 // 已经过的「卡片类块」个数（对应 detachAgents 的下标）
	for i := len(spans) - 1; i >= 0; i-- {
		if i < len(kinds) && !session.HasCardKind(kinds[i]) {
			continue
		}
		seen++
		if seen > candidateCount {
			break // 超出候选范围（等价于 detachAgents 的 blocks[:start] 截断）
		}
"""

new = """// upperIdx = 候选范围的**上界**（spans 索引）。
//
// 关键语义（第一版踩过）：detachAgents 迭代的是 blocks[:start]，而 blocks[:start]
// 取的是**最旧**的那 start 个 ⇒ 候选集 = 尾部窗口之前**最旧**的 start 张卡片类块
// ⇒ 候选集在 spans 里是**索引最小**的一段 ⇒ 遍历要从 upperIdx 往回，不是从最新往回。
func pickCardHeadsFromSpans(spans []session.BlockSpan, kinds []uint8, upperIdx int) ([]Block, int, int, error) {
	var picked []Block
	var total int64
	trimmedCount := 0
	keptItems := 0
	for i := upperIdx; i >= 0; i-- {
		if i < len(kinds) && !session.HasCardKind(kinds[i]) {
			continue
		}
"""
assert s.count(old) == 1, ("proto", s.count(old))
s = s.replace(old, new)

old2 = """	candidateCount := cardKindBefore + tailKept - restoreWindowBlocks
	if candidateCount < 0 {
		candidateCount = 0
	}
	agentsB, trimmed, kept, err := pickCardHeadsFromSpans(spans[:boundary], kinds, candidateCount)
"""
new2 = """	candidateCount := cardKindBefore + tailKept - restoreWindowBlocks
	if candidateCount < 0 {
		candidateCount = 0
	}
	// 候选集 = spans 里**最旧**的 candidateCount 张卡片类块 ⇒ 找出它的索引上界。
	upperIdx := -1
	seen := 0
	for i := 0; i < boundary && i < len(kinds); i++ {
		if !session.HasCardKind(kinds[i]) {
			continue
		}
		seen++
		if seen == candidateCount {
			upperIdx = i
			break
		}
	}
	agentsB, trimmed, kept, err := pickCardHeadsFromSpans(spans, kinds, upperIdx)
"""
assert s.count(old2) == 1, ("call", s.count(old2))
s = s.replace(old2, new2)

open(p, 'w', encoding='utf-8').write(s)
print("patched prototype direction")
