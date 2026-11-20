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

# 1) 从 close() 的文档注释后面摘掉（它把 close 的注释顶掉了）。
old = """// close 收尾：取消所有仍在跑的嵌套调用。
//
// 触发点是「本次执行收尾」（ExecuteScript 返回后 defer 调用）——它覆盖正常收尾/超时/
// 取消/脚本报错/exit() 五条退出路径。边界见文件头「收尾的取消」。
""" + block + """func (b *bridge) close() {"""

new = """// close 收尾：取消所有仍在跑的嵌套调用。
//
// 触发点是「本次执行收尾」（ExecuteScript 返回后 defer 调用）——它覆盖正常收尾/超时/
// 取消/脚本报错/exit() 五条退出路径。边界见文件头「收尾的取消」。
func (b *bridge) close() {"""

assert old in s
s = s.replace(old, new, 1)

# 2) 放到 Tool.Call 之前（脚本执行路径上，读者顺路能看见）。
anchor = "func (t *Tool) Call(ctx context.Context, _ string, args string) (string, error) {"
assert anchor in s
s = s.replace(anchor, block + anchor, 1)

open(p, 'w', encoding='utf-8').write(s)
print('ok')
