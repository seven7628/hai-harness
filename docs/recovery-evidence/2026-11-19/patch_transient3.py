p = '/Users/admin/workspace/go-code/desktop/bridge/checkpoint.go'
s = open(p, encoding='utf-8').read()

T = "\t"

old = (
    T + "// logical 取逻辑内容并校验摘要（hashes 为 nil 时只取内容）。\n"
    + T + "logical := func(i int) ([]byte, error) {\n"
    + T + T + "raw, err := spans[i].Logical()\n"
    + T + T + "if err != nil {\n"
    + T + T + T + "return nil, fmt.Errorf(\"block %d: %w\", i, err)\n"
    + T + T + "}\n"
    + T + T + "if hashes == nil {\n"
    + T + T + T + "return raw, nil\n"
    + T + T + "}\n"
    + T + T + "if d := session.PayloadDigest(raw); d != hashes[i] {\n"
    + T + T + T + "return nil, fmt.Errorf(\"block %d: payload checksum mismatch\", i)\n"
    + T + T + "}\n"
    + T + T + "return raw, nil\n"
    + T + "}\n"
)

new = (
    T + "// logical 取逻辑内容并校验摘要（hashes 为 nil 时只取内容），交给回调处理。\n"
    + T + "//\n"
    + T + "// 用 transient 变体（2026-11-18）：解压缓冲**只在回调期间**有效。\n"
    + T + "// 回调里只做两件事 —— 摘要校验 + JSON 解析 —— 两者都把数据拷走：\n"
    + T + "// Block 的字段全是值类型（**没有 RawMessage / []byte**）⇒ 解析结果不别名\n"
    + T + "// 原始缓冲 ⇒ 缓冲归池后不会污染已解析的块。\n"
    + T + "// ⇒ 省掉「每个块再拷一份」的那次 clone：真实大会话 1971 块 ⇒ 每次打开省 ~100MB。\n"
    + T + "logical := func(i int, fn func([]byte) error) error {\n"
    + T + T + "err := spans[i].LogicalTransient(func(raw []byte) error {\n"
    + T + T + T + "if hashes != nil {\n"
    + T + T + T + T + "if d := session.PayloadDigest(raw); d != hashes[i] {\n"
    + T + T + T + T + T + "return fmt.Errorf(\"block %d: payload checksum mismatch\", i)\n"
    + T + T + T + T + "}\n"
    + T + T + T + "}\n"
    + T + T + T + "return fn(raw)\n"
    + T + T + "})\n"
    + T + T + "if err != nil {\n"
    + T + T + T + "return fmt.Errorf(\"block %d: %w\", i, err)\n"
    + T + T + "}\n"
    + T + T + "return nil\n"
    + T + "}\n"
)

assert s.count(old) == 1, ("logical", s.count(old))
s = s.replace(old, new)

old2 = (
    T + T + "for i := range spans {\n"
    + T + T + T + "raw, err := logical(i)\n"
    + T + T + T + "if err != nil {\n"
    + T + T + T + T + "return err\n"
    + T + T + T + "}\n"
    + T + T + T + "if err := json.Unmarshal(raw, &out[i]); err != nil {\n"
    + T + T + T + T + "return fmt.Errorf(\"block %d: %w\", i, err)\n"
    + T + T + T + "}\n"
    + T + T + "}\n"
)

new2 = (
    T + T + "for i := range spans {\n"
    + T + T + T + "if err := logical(i, func(raw []byte) error {\n"
    + T + T + T + T + "return json.Unmarshal(raw, &out[i])\n"
    + T + T + T + "}); err != nil {\n"
    + T + T + T + T + "return err\n"
    + T + T + T + "}\n"
    + T + T + "}\n"
)

assert s.count(old2) == 1, ("serial", s.count(old2))
s = s.replace(old2, new2)

old3 = (
    T + T + T + T + "raw, err := logical(i)\n"
    + T + T + T + T + "if err != nil {\n"
    + T + T + T + T + T + "mu.Lock()\n"
    + T + T + T + T + T + "if firstErr == nil {\n"
    + T + T + T + T + T + T + "firstErr = err\n"
    + T + T + T + T + T + "}\n"
    + T + T + T + T + T + "mu.Unlock()\n"
    + T + T + T + T + T + "return\n"
    + T + T + T + T + "}\n"
    + T + T + T + T + "if err := json.Unmarshal(raw, &out[i]); err != nil {\n"
    + T + T + T + T + T + "mu.Lock()\n"
    + T + T + T + T + T + "if firstErr == nil {\n"
    + T + T + T + T + T + T + "firstErr = fmt.Errorf(\"block %d: %w\", i, err)\n"
    + T + T + T + T + T + "}\n"
    + T + T + T + T + T + "mu.Unlock()\n"
    + T + T + T + T + T + "return\n"
    + T + T + T + T + "}\n"
)

new3 = (
    T + T + T + T + "err := logical(i, func(raw []byte) error {\n"
    + T + T + T + T + T + "return json.Unmarshal(raw, &out[i])\n"
    + T + T + T + T + "})\n"
    + T + T + T + T + "if err != nil {\n"
    + T + T + T + T + T + "mu.Lock()\n"
    + T + T + T + T + T + "if firstErr == nil {\n"
    + T + T + T + T + T + T + "firstErr = err\n"
    + T + T + T + T + T + "}\n"
    + T + T + T + T + T + "mu.Unlock()\n"
    + T + T + T + T + T + "return\n"
    + T + T + T + T + "}\n"
)

assert s.count(old3) == 1, ("parallel", s.count(old3))
s = s.replace(old3, new3)

open(p, 'w', encoding='utf-8').write(s)
print("patched checkpoint.go decodeBlocksParallel")
