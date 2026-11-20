import re

p = '/Users/admin/workspace/go-code/desktop/bridge/main.go'
s = open(p, encoding='utf-8').read()

old_head = "\tlimit := maxLiveSessions()\n\tfor len(ws.sessions) > limit {\n\t\tvar victim *bridgeSession\n"
new_head = """\tlimit := maxLiveSessions()
\tfor {
\t\toverCount := len(ws.sessions) > limit
\t\toverBytes := false
\t\tif !overCount {
\t\t\tvar total int64
\t\t\tfor _, bs := range ws.sessions {
\t\t\t\ttotal += sessionCostBytes(bs)
\t\t\t}
\t\t\toverBytes = total > sessionMemBudgetBytes
\t\t}
\t\tif !overCount && !overBytes {
\t\t\treturn
\t\t}
\t\tvar victim *bridgeSession
"""

assert s.count(old_head) == 1, s.count(old_head)
s = s.replace(old_head, new_head)

# 在 evictIdleSessionsLocked 之后追加 sessionCostBytes
anchor = "\t\tstartupLog(\"A7.evict: \"+victim.id, now)\n\t}\n}\n"
assert s.count(anchor) == 1, s.count(anchor)
addition = anchor + """
// sessionCostBytes 估算一个驻留会话的堆占用（0 = 没有 reducer 或还没加载）。
func sessionCostBytes(bs *bridgeSession) int64 {
\tif bs == nil || bs.reducer == nil {
\t\treturn 0
\t}
\treturn bs.reducer.ApproxBytes()
}
"""
s = s.replace(anchor, addition)

# 注释补一句（在 evictIdleSessionsLocked 的注释块末尾追加口径说明）
doc_anchor = "// 若候选都不满足（全在跑），**允许超预算**"
idx = s.index(doc_anchor)
end = s.index("\nfunc (m *manager) evictIdleSessionsLocked", idx)
doc_add = """
//
// 口径（2026-11-18 改）：在「会话数上限」之外**叠加一个字节预算**。
// 为什么：会话成本按事件数缩放（预算假设 96MB/会话，实测真实值 248MB，2.6 倍），
// 只按个数记账等于「1 个大会话顶掉 5 个普通会话的额度」——
// 实测 bridge 曾同时驻留 6 个会话 ≈ 1.2GB，把 16G 机器的 swap 撑到 91%。
// 字节预算取 reducer 的估算值（ApproxBytes）：口径稳定即可，淘汰本来就是启发式。
//
// 会话数上限**保留**：小会话场景行为不变（≤5 个不淘汰），
// 只是字节账算不过来时**额外**淘汰。"""
s = s[:end] + doc_add + s[end:]

open(p, 'w', encoding='utf-8').write(s)
print("patched main.go")
