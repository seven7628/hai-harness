import os

p = '/Users/admin/workspace/go-code/tools/builtin/workdir.go'
s = open(p, encoding='utf-8').read()

old = """func CwdKey(ctx context.Context) string {
	if k := workdir.KeyFrom(ctx); k != workdir.MainKey {"""

new = """func CwdKey(ctx context.Context) string {
	if ctx == nil {
		return workdir.MainKey // nil ctx（直调工具的极端用法）⇒ 主分区
	}
	if k := workdir.KeyFrom(ctx); k != workdir.MainKey {"""

assert old in s
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('ok')
