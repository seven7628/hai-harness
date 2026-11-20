import re

p = '/Users/admin/workspace/go-code/desktop/bridge/view_reducer.go'
s = open(p, encoding='utf-8').read()

# 1) 撤掉调试打印
dbg = '''	if os.Getenv("HAI_DEBUG_DETACHED") != "" {
		fmt.Fprintf(os.Stderr, "DEBUG MarshalTail: detachedCards=%d start=%d blocks=%d\\n", len(r.detachedCards), start, len(blocks))
	}
'''
assert s.count(dbg) == 1, ("dbg", s.count(dbg))
s = s.replace(dbg, '')

# 2) 撤掉调试用的 import
old_imp = '''import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"'''
new_imp = '''import (
	"bytes"
	"encoding/json"
	"sort"'''
assert s.count(old_imp) == 1, ("imp", s.count(old_imp))
s = s.replace(old_imp, new_imp)

# 3) 修 MarshalTail 的「不截断」分支条件
old = """	if limit <= 0 || limit >= len(blocks) {
		// 不截断：走全量编码（带逐块字节缓存）。**不能**在这里自己拼 JSON ——
		// 那会绕过 blockBytes 缓存，让每个会话每次打开都重新序列化全部块（小会话也变慢）。
		view, _, _, err := r.marshalLockedSpans()
		if err != nil {
			return nil, err
		}
		return &PartialView{View: view, Total: total, Returned: total}, nil
	}
"""
new = """	// 「不截断」分支的条件多了 detachedCards 一项（2026-11-19）：
	//
	// 骨架级优化后，窗口外卡片的**头部**单独存在 r.detachedCards 里
	// （正文不进内存）⇒ 它们**不在 blocks 里** ⇒ 必须走下面的截断分支
	// 才能把 agents 带上（这条分支按构造不带 Agents）。
	//
	// 而「窗口里没有 detachedCards」时（旧的单窗口会话 / 全量路径）行为不变：
	// 块数不超过 limit ⇒ 全量编码等价于尾部切片（见函数头的说明）。
	if limit <= 0 || (limit >= len(blocks) && len(r.detachedCards) == 0) {
		// 不截断：走全量编码（带逐块字节缓存）。**不能**在这里自己拼 JSON ——
		// 那会绕过 blockBytes 缓存，让每个会话每次打开都重新序列化全部块（小会话也变慢）。
		view, _, _, err := r.marshalLockedSpans()
		if err != nil {
			return nil, err
		}
		return &PartialView{View: view, Total: total, Returned: total}, nil
	}
"""
assert s.count(old) == 1, ("branch", s.count(old))
s = s.replace(old, new)

open(p, 'w', encoding='utf-8').write(s)
print("patched MarshalTail branch + removed debug")
