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

cur = "// Call 工具主流程（八步见文件头）。\n" + block + "func (t *Tool) Call(ctx context.Context, _ string, args string) (string, error) {"
want = block + "// Call 工具主流程（八步见文件头）。\nfunc (t *Tool) Call(ctx context.Context, _ string, args string) (string, error) {"

assert cur in s, 'shape not found'
s = s.replace(cur, want, 1)
open(p, 'w', encoding='utf-8').write(s)
print('ok')
