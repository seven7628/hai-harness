p = '/Users/admin/workspace/go-code/desktop/bridge/checkpoint.go'
s = open(p, encoding='utf-8').read()

old = """	// logical 取逻辑内容并校验摘要（hashes 为 nil 时只取内容）。
	logical := func(i int) ([]byte, error) {
		raw, err := spans[i].Logical()
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", i, err)
		}
		if hashes == nil {
			return raw, nil
		}
		if d := session.PayloadDigest(raw); d != hashes[i] {
			return nil, fmt.Errorf("block %d: payload checksum mismatch", i)
		}
		return raw, nil
	}
"""
new = """	// logical 取逻辑内容并校验摘要（hashes 为 nil 时只取内容），交给回调处理。
	//
	// 用 transient 变体（2026-11-18）：解压缓冲**只在回调期间**有效。
	// 回调里只做两件事 —— 摘要校验 + JSON 解析 —— 两者都把数据拷走：
	// Block 的字段全是值类型（**没有 RawMessage / []byte**）⇒ 解析结果不别名
	// 原始缓冲 ⇒ 缓冲归池后不会污染已解析的块。
	// ⇒ 省掉「每个块再拷一份」的那次 clone：真实大会话 1971 块 ⇒ 每次打开省 ~100MB。
	logical := func(i int, fn func([]byte) error) error {
		err := spans[i].LogicalTransient(func(raw []byte) error {
			if hashes != nil {
				if d := session.PayloadDigest(raw); d != hashes[i] {
					return fmt.Errorf("block %d: payload checksum mismatch", i)
				}
			}
			return fn(raw)
		})
		if err != nil {
			return fmt.Errorf("block %d: %w", i, err)
		}
		return nil
	}
"""
assert s.count(old) == 1, s.count(old)
s = s.replace(old, new)

old2 = """			for i := range spans {
				raw, err := logical(i)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(raw, &out[i]); err != nil {
					return fmt.Errorf("block %d: %w", i, err)
				}
			}
			return nil
"""
new2 = """			for i := range spans {
				if err := logical(i, func(raw []byte) error {
					return json.Unmarshal(raw, &out[i])
				}); err != nil {
					return err
				}
			}
			return nil
"""
assert s.count(old2) == 1, s.count(old2)
s = s.replace(old2, new2)

open(p, 'w', encoding='utf-8').write(s)
print("patched decodeBlocksParallel (serial)")
