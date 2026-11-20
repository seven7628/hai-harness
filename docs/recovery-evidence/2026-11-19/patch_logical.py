p = '/Users/admin/workspace/go-code/session/block_span.go'
s = open(p, encoding='utf-8').read()

old = """// logicalSpans 把裸字节切片包成 BlockSpan（全部视为「已是逻辑内容」）。"""
new = """// LogicalTransient 把逻辑内容交给回调（**只在回调期间有效**）。
//
// 什么时候用：调用方只是「读一遍就走」（校验摘要 / 解析成结构体）。
// 这时不必为每个块再拷一份解压结果 —— 走池化缓冲，回调返回即归池。
//
// 契约：回调**不得**保留传入的字节（或把它放进任何长期存活的结构里）。
// 安全前提：解析结果不别名这块缓冲（Block 的字段全是值类型，没有 RawMessage）
// —— 有 RawMessage 的结构不能用这个变体。
//
// 未压缩块：Raw 指向 sidecar 的只读映射（本来就不是堆），直接给出去。
func (s BlockSpan) LogicalTransient(fn func([]byte) error) error {
	if !s.Compressed {
		return fn(s.Raw)
	}
	return decompressBlockTransient(s.Raw, fn)
}

// logicalSpans 把裸字节切片包成 BlockSpan（全部视为「已是逻辑内容」）。"""
assert s.count(old) == 1, s.count(old)
s = s.replace(old, new)

open(p, 'w', encoding='utf-8').write(s)
print("patched block_span.go")
