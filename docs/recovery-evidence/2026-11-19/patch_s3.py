p = 'tools/builtin/builtin.go'
s = open(p, encoding='utf-8').read()

# ---------- 1) struct：加 wd ----------
old = '''type FileTools struct {
	root string
	sbx  sandbox.Sandbox // bash 执行后端（nil = NoSandbox 进程隔离直通）
'''
new = '''type FileTools struct {
	root string
	// wd 是**会话级**的 cwd 状态（与 bash 工具共用同一个 workdir.State）。
	//
	// 2026-11-19：相对路径的**解析基准**从「固定的工作区根」改为「当前工作目录」
	// —— 模型 cd 到子目录后，write_file("x.txt") 应该落在那个子目录，
	// 而不是工作区根（现状是**静默落错地方**，模型与用户都看不出来）。
	//
	// ⚠️ 三个锚点各司其职，别混：
	//   · 解析基准 = **当前 cwd**（跟着 cd 走）
	//   · 越界边界 = **工作区根**（不变：任何路径都不得逃出工作区）
	//   · gitignore 匹配器 root = **工作区根**（不变：.gitignore 是仓库级语义，
	//     跟着 cwd 走会截断向上回溯 ⇒ node_modules 之类不再被忽略）
	wd  *workdir.State
	sbx sandbox.Sandbox // bash 执行后端（nil = NoSandbox 进程隔离直通）
}'''
assert old in s, 'struct'
s = s.replace(old, new, 1)

# ---------- 2) 构造器 ----------
old = '''// NewFileTools 创建文件工具集；root 为工作区根目录（绝对路径，不存在不报错）。
func NewFileTools(root string) *FileTools {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = filepath.Clean(root)
	}
	return &FileTools{root: filepath.Clean(abs)}
}'''
new = '''// NewFileTools 创建文件工具集；root 为工作区根目录（绝对路径，不存在不报错）。
//
// ⚠️ **独立状态**（自己建一个 workdir.State）：不与其它工具共享 cwd。
// 生产路径用 NewFileToolsWithWorkdir（与 bash 共用同一个 State ⇒ 两边 cwd 一致）。
func NewFileTools(root string) *FileTools {
	return NewFileToolsWithWorkdir(workdir.New(root))
}

// NewFileToolsWithWorkdir 创建文件工具集，共享会话级 cwd 状态。
//
// 生产路径用它：同一个 workdir.State 同时喂给 bash 与文件工具 ⇒
// 「bash 的 cwd」与「文件工具的相对路径基准」永远是同一个值。
func NewFileToolsWithWorkdir(wd *workdir.State) *FileTools {
	return &FileTools{root: wd.Root(), wd: wd}
}'''
assert old in s, 'ctor'
s = s.replace(old, new, 1)

# ---------- 3) Tools()：bash 共享同一个 State ----------
old = '''		// cwd 传 f.root：bash 初始目录 = 工作区根（每条命令独立进程、从工作区出发；
		// AI 可在单条命令内 cd 切换，下条命令回到工作区起点——对齐 Claude Code 语义）。
		// appAllow 参数已弃用（2026-09：open 全放行），传 nil。
		NewBashTool(f.root, 0, f.sbx, nil),'''
new = '''		// bash 与文件工具**共享同一个 workdir.State** ⇒ 两边的 cwd 永不漂移
		// （2026-11-19：cwd 记忆原先挂在 BashTool 实例上，而实例每次 Tools() 都新建
		//  ⇒ 切模型/切人格重建引擎时记忆被清空）。
		// appAllow 参数已弃用（2026-09：open 全放行），传 nil。
		NewBashToolWithWorkdir(f.wd, 0, f.sbx, nil),'''
assert old in s, 'tools()'
s = s.replace(old, new, 1)

# ---------- 4) pathParamDesc ----------
old = '''const pathParamDesc = "File path. A relative path is resolved against the workspace root " +
	"and may not escape it (a leading ../ is rejected); use an absolute path for files " +
	"outside the workspace."'''
new = '''const pathParamDesc = "File path. A relative path is resolved against the current working directory " +
	"(the workspace root until a `cd` changes it) and may not escape the workspace root; " +
	"use an absolute path for files outside the workspace."'''
assert old in s, 'desc'
s = s.replace(old, new, 1)

# ---------- 5) resolve 三兄弟 ----------
old = '''func (f *FileTools) resolve(raw string) (string, error) { return f.resolveFor(raw, PathRead) }'''
new = '''func (f *FileTools) resolve(ctx context.Context, raw string) (string, error) {
	return f.resolveFor(ctx, raw, PathRead)
}'''
assert old in s, 'resolve'
s = s.replace(old, new, 1)

old = '''func (f *FileTools) resolveWrite(raw string) (string, error) { return f.resolveFor(raw, PathWrite) }'''
new = '''func (f *FileTools) resolveWrite(ctx context.Context, raw string) (string, error) {
	return f.resolveFor(ctx, raw, PathWrite)
}'''
assert old in s, 'resolveWrite'
s = s.replace(old, new, 1)

old = '''func (f *FileTools) resolveFor(raw string, op PathOp) (string, error) {
	if raw == "" {
		return "", errors.New("path is required")
	}
	base := ""
	if !filepath.IsAbs(raw) {
		base = f.root
	}
	p := filepath.Clean(filepath.Join(base, raw))
	if base != "" && p != base && !strings.HasPrefix(p, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes workspace root %q", raw, base)
	}
	if err := denyHarnessConfigPath(p, op); err != nil {
		return "", err
	}
	return p, nil
}'''
new = '''func (f *FileTools) resolveFor(ctx context.Context, raw string, op PathOp) (string, error) {
	if raw == "" {
		return "", errors.New("path is required")
	}
	base := ""
	if !filepath.IsAbs(raw) {
		// 相对路径的**解析基准 = 当前工作目录**（2026-11-19：跟着 cd 走）。
		base = f.cwd(ctx)
	}
	p := filepath.Clean(filepath.Join(base, raw))
	// 越界边界**仍然是工作区根**（不是 cwd）：cwd 可以是 root 的子目录，
	// 但任何相对路径都不得逃出 root —— 既有不变量不变。
	// （cwd == root 时本判定与改造前逐字等价。）
	if base != "" && p != f.root && !strings.HasPrefix(p, f.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes workspace root %q", raw, f.root)
	}
	if err := denyHarnessConfigPath(p, op); err != nil {
		return "", err
	}
	return p, nil
}

// cwd 返回该调用方分区的当前工作目录（相对路径的解析基准）。
//
// 分区键与 bash 工具一致（子 agent 的 task id）⇒ 主/子 agent 的目录互不干扰。
func (f *FileTools) cwd(ctx context.Context) string {
	if f.wd != nil {
		return f.wd.Current(cwdKey(ctx))
	}
	return f.root
}'''
assert old in s, 'resolveFor'
s = s.replace(old, new, 1)

# ---------- 6) 三个调用点 ----------
old = '''	p, err := t.resolve(a.Path)'''
new = '''	p, err := t.resolve(ctx, a.Path)'''
assert old in s, 'read'
s = s.replace(old, new, 1)

old = '''	p, err := t.resolveWrite(a.Path)'''
new = '''	p, err := t.resolveWrite(ctx, a.Path)'''
assert s.count(old) == 2, 'write/edit count=%d' % s.count(old)
s = s.replace(old, new)

# ---------- 7) import ----------
old = '''	"github.com/seven7628/hai-harness/tools"'''
assert old in s, 'import'
s = s.replace(old, '''	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/workdir"''', 1)

open(p, 'w', encoding='utf-8').write(s)
print('builtin.go ok')

# ================= grep.go / glob.go =================
for path, old, new in [
    ('tools/builtin/grep.go',
     '\troot, err := t.resolve(start)',
     '\troot, err := t.resolve(ctx, start)'),
    ('tools/builtin/glob.go',
     '\troot, err := t.resolve(start)',
     '\troot, err := t.resolve(ctx, start)'),
]:
    s = open(path, encoding='utf-8').read()
    assert old in s, path
    s = s.replace(old, new, 1)
    open(path, 'w', encoding='utf-8').write(s)
    print(path, 'ok')

# ================= run_python.go =================
path = 'tools/builtin/run_python.go'
s = open(path, encoding='utf-8').read()
old = '''func (t *RunPythonTool) declaredOutputs(outputs []string) []runPythonOutput {'''
new = '''func (t *RunPythonTool) declaredOutputs(ctx context.Context, outputs []string) []runPythonOutput {'''
assert old in s, 'declaredOutputs'
s = s.replace(old, new, 1)

old = '''		if abs, err := t.ft.resolve(raw); err == nil {'''
new = '''		if abs, err := t.ft.resolve(ctx, raw); err == nil {'''
assert old in s, 'declaredOutputs resolve'
s = s.replace(old, new, 1)

old = '''func (t *RunPythonTool) resolveScript(a runPythonArgs) (string, string, error) {'''
new = '''func (t *RunPythonTool) resolveScript(ctx context.Context, a runPythonArgs) (string, string, error) {'''
assert old in s, 'resolveScript'
s = s.replace(old, new, 1)

old = '''	p, err := t.ft.resolve(a.ScriptPath)'''
new = '''	p, err := t.ft.resolve(ctx, a.ScriptPath)'''
assert old in s, 'resolveScript resolve'
s = s.replace(old, new, 1)

old = '''	src, scriptPath, err := t.resolveScript(a)'''
new = '''	src, scriptPath, err := t.resolveScript(ctx, a)'''
assert old in s, 'resolveScript call'
s = s.replace(old, new, 1)

open(path, 'w', encoding='utf-8').write(s)
print('run_python.go ok')
