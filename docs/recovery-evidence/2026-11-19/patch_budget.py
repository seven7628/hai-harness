p = '/Users/admin/workspace/go-code/desktop/bridge/main.go'
s = open(p, encoding='utf-8').read()

old = """const (
	// sessionMemBudgetBytes 是「同时驻留的会话」内存预算（512MB）。
	sessionMemBudgetBytes = 512 << 20
"""
new = """// sessionMemBudgetBytes 是「同时驻留的会话」内存预算（512MB，真实堆口径）。
//
// **var 而不是 const**：淘汰判据要能被测试注入（生产恒为这个值）。
var sessionMemBudgetBytes int64 = 512 << 20

const (
"""
assert s.count(old) == 1, s.count(old)
s = s.replace(old, new)

open(p, 'w', encoding='utf-8').write(s)
print("patched budget var")
