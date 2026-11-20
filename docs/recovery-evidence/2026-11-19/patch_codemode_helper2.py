import os

p = '/Users/admin/workspace/go-code/tools/builtin/codemode/bridge.go'
s = open(p, encoding='utf-8').read()

block = """// cwd 解析脚本工作目录（优先按调用方现取 ⇒ 跟着会话的当前工作目录走）。
func (t *Tool) cwd(ctx context.Context) string {
	if t.opts.CwdFunc != nil {
		if d := t.opts.CwdFunc(ctx); d != "" {
			return d
		}
	}
	return t.opts.Cwd
}

"""

old = block + """// Call 工具主流程（八步见文件头）。
func (t *Tool) Call(ctx context.Context, _ string, args string) (string, error) {"""

new = block + """// Call 工具主流程（八步见文件头）。
func (t *Tool) Call(ctx context.Context, _ string, args string) (string, error) {"""

# 上面已经是期望形状（block 在注释之前）——若不匹配说明顺序还不对。
if old not in s:
    raise SystemExit('shape mismatch')

# 现状：注释在 block 之前、block 在 Call 之前 ⇒ 需要交换。
cur = """// Call 工具主流程（八步见文件头）。
""" + block + """func (t *Tool) Call(ctx context.Context, _ string, args string) (string, error) {"""
assert cur in s
s = s.replace(cur, new, 1)

open(p, 'w', encoding='utf-8').write(s)
print('ok')
