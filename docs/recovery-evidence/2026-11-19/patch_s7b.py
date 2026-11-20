import re, glob

# ============ 1) cwdKey → CwdKey（导出；宿主层也要用） ============
p = 'tools/builtin/bash.go'
s = open(p, encoding='utf-8').read()
old = '''// cwdKey 调用方的 cwd 记忆分区键。'''
new = '''// CwdKey 调用方的 cwd 记忆分区键（**导出**：宿主层的工具也要按它取当前目录，
// 例如 codemode 的脚本工作目录）。
//
// ⚠️ 两个来源，必须给同一个值：
//   · workdir 的 ctx key（workdir.WithKey）—— **工具描述**那条路用
//     （描述在「拼 schema」时算，那里还没有 ToolContext）
//   · ToolContext.TaskId —— **工具调用**那条路用
// 优先读前者、后者兜底 ⇒ 两条路永远一致（agent loop 在两处注入的是同一个值）。
func CwdKey(ctx context.Context) string {
	if k := workdir.KeyFrom(ctx); k != workdir.MainKey {
		return k
	}
	if tc := events.ToolContextFrom(ctx); tc != nil && tc.TaskId != "" {
		return tc.TaskId
	}
	return workdir.MainKey
}

// cwdKey 调用方的 cwd 记忆分区键。'''
assert old in s, 'cwdKey comment'
s = s.replace(old, new, 1)

# 把旧 cwdKey 的实现换成委托
old = '''func cwdKey(ctx context.Context) string {
	if k := workdir.KeyFrom(ctx); k != workdir.MainKey {
		return k
	}
	if tc := events.ToolContextFrom(ctx); tc != nil && tc.TaskId != "" {
		return tc.TaskId
	}
	return workdir.MainKey
}'''
new = '''func cwdKey(ctx context.Context) string { return CwdKey(ctx) }'''
assert old in s, 'cwdKey body'
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('bash.go ok')

# ============ 2) codemode Options：加 CwdFunc ============
p = 'tools/builtin/codemode/tool.go'
s = open(p, encoding='utf-8').read()
old = '''	// Cwd 脚本工作目录（"" = 执行器默认，通常即工作区根，与 bash/run_python 同锚）。
	Cwd string
}'''
new = '''	// Cwd 脚本工作目录（"" = 执行器默认，通常即工作区根，与 bash/run_python 同锚）。
	Cwd string

	// CwdFunc 按**调用方**解析脚本工作目录（nil = 用 Cwd 字段）。
	//
	// 为什么需要函数而不是常量：脚本工作目录要跟着会话的**当前工作目录**走
	//（模型 cd 之后），而"当前目录"是**运行期状态** ⇒ 装配时冻结成常量会立刻过期
	//（与 bash/run_python 的新口径就不一致了）。
	CwdFunc func(ctx context.Context) string
}'''
assert old in s, 'codemode opts'
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('codemode/tool.go ok')

# ============ 3) codemode bridge：按 ctx 取 cwd ============
p = 'tools/builtin/codemode/bridge.go'
s = open(p, encoding='utf-8').read()
old = '''	res, err := t.executor().ExecuteScript(runCtx, execproc.ScriptOpts{
		Script:   code,
		Cwd:      t.opts.Cwd,'''
new = '''	res, err := t.executor().ExecuteScript(runCtx, execproc.ScriptOpts{
		Script:   code,
		Cwd:      t.cwd(ctx),'''
assert old in s, 'codemode exec'
s = s.replace(old, new, 1)

# 加 cwd 助手（放在 bridge 的合适位置）
old = '''func (b *bridge) close() {'''
new = '''// cwd 解析脚本工作目录（优先按调用方现取 ⇒ 跟着会话的当前工作目录走）。
func (t *Tool) cwd(ctx context.Context) string {
	if t.opts.CwdFunc != nil {
		if d := t.opts.CwdFunc(ctx); d != "" {
			return d
		}
	}
	return t.opts.Cwd
}

func (b *bridge) close() {'''
if old in s:
    s = s.replace(old, new, 1)
else:
    # 找别的锚点
    m = re.search(r'\nfunc \(b \*bridge\) ', s)
    assert m
    s = s[:m.start()] + '\n' + new.split('\nfunc (b *bridge) close() {')[0] + s[m.start():]
open(p, 'w', encoding='utf-8').write(s)
print('codemode/bridge.go ok')

# ============ 4) desktop/bridge/codemode.go：装配 CwdFunc ============
p = 'desktop/bridge/codemode.go'
s = open(p, encoding='utf-8').read()
old = '''		Cwd:        workspace,
	}))'''
new = '''		Cwd: workspace,
		// 脚本工作目录跟着会话的**当前工作目录**走（模型 cd 之后）——
		// 与 bash / run_python / 文件工具同一口径。
		CwdFunc: func(ctx context.Context) string {
			return wd.Current(builtin.CwdKey(ctx))
		},
	}))'''
assert old in s, 'codemode assembly'
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('desktop/bridge/codemode.go ok')
