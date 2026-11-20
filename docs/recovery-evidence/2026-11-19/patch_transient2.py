p = '/Users/admin/workspace/go-code/desktop/bridge/checkpoint.go'
s = open(p, encoding='utf-8').read()

old = """					raw, err := logical(i)
					if err != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
						return
					}
					if err := json.Unmarshal(raw, &out[i]); err != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = fmt.Errorf("block %d: %w", i, err)
						}
						mu.Unlock()
						return
					}
"""
new = """					err := logical(i, func(raw []byte) error {
						return json.Unmarshal(raw, &out[i])
					})
					if err != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
						return
					}
"""
assert s.count(old) == 1, s.count(old)
s = s.replace(old, new)

open(p, 'w', encoding='utf-8').write(s)
print("patched decodeBlocksParallel (parallel)")
