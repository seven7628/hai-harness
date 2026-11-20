p = '/Users/admin/workspace/go-code/desktop/bridge/detach_agents_equivalence_test.go'
s = open(p, encoding='utf-8').read()

# 1) 删掉本地原型（blockHead 类型 + pickCardHeadsFromSpans）
start = s.index("// blockHead 是「卡片头」投影")
end = s.index("// TestDetachedAgentsEquivalence：两条路产出的 agents 字节必须完全一致。")
s = s[:start] + s[end:]

# 2) 调用改成生产代码
old = """	agentsB, trimmed, kept, err := pickCardHeadsFromSpans(spans, kinds, upperIdx)
	if err != nil {
		t.Fatalf("取卡片头失败: %v", err)
	}
"""
new = """	agentsB, err := pickCardHeadsFromSpans(spans, kinds, upperIdx)
	if err != nil {
		t.Fatalf("取卡片头失败: %v", err)
	}
"""
assert s.count(old) == 1, ("call", s.count(old))
s = s.replace(old, new)

old2 = """	t.Logf("  路A agents %d 张 / %d 字节；路B agents %d 张 / %d 字节（置空 items 的 %d 张，仍带 items 的 %d 张）",
		len(agentsA), len(bytesA), len(agentsB), len(bytesB), trimmed, kept)
"""
new2 = """	t.Logf("  路A agents %d 张 / %d 字节；路B agents %d 张 / %d 字节",
		len(agentsA), len(bytesA), len(agentsB), len(bytesB))
"""
assert s.count(old2) == 1, ("log", s.count(old2))
s = s.replace(old2, new2)

# 3) import 里不再需要 encoding/json? 还要（比较时 marshal）⇒ 保留。
# 4) 头部注释补一句：路 B 用的是**生产代码**。
old3 = "// 这条测试**只测不改**：证明今天这两条路已经产出**字节完全相同**的 agents。"
new3 = ("// 这条测试**只测不改**：证明今天这两条路已经产出**字节完全相同**的 agents。\n"
        "// 路 B 调的是**生产代码**（card_head.go 的 pickCardHeadsFromSpans）⇒\n"
        "// 这条测试同时是那份生产代码的验收。")
assert s.count(old3) == 1
s = s.replace(old3, new3)

open(p, 'w', encoding='utf-8').write(s)
print("test now uses production code")
