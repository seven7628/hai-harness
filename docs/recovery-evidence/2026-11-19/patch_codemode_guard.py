import os

p = '/Users/admin/workspace/go-code/desktop/bridge/codemode.go'
s = open(p, encoding='utf-8').read()

old = """		CwdFunc: func(ctx context.Context) string {
			return wd.Current(builtin.CwdKey(ctx))
		},"""

new = """		CwdFunc: func(ctx context.Context) string {
			if wd == nil {
				return workspace // 没接会话级状态 ⇒ 沿用工作区根（与 Cwd 同口径）
			}
			return wd.Current(builtin.CwdKey(ctx))
		},"""

assert old in s
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('ok')
