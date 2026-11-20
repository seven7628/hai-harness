import os, re, sys

ROOT = '/Users/admin/workspace/go-code'


def read(p):
    return open(os.path.join(ROOT, p), encoding='utf-8').read()


def write(p, s):
    open(os.path.join(ROOT, p), 'w', encoding='utf-8').write(s)


def sub1(path, old, new, tag):
    s = read(path)
    if old not in s:
        if new in s:
            print('SKIP already-applied:', tag)
            return
        raise SystemExit('MISS: %s in %s' % (tag, path))
    if s.count(old) != 1:
        raise SystemExit('MULTI(%d): %s in %s' % (s.count(old), tag, path))
    write(path, s.replace(old, new, 1))
    print('OK:', tag)


def resub(path, pat, new, tag, expect=None):
    s = read(path)
    s2, n = re.subn(pat, new, s)
    if n == 0:
        print('SKIP no-match:', tag)
        return
    if expect is not None and n != expect:
        raise SystemExit('COUNT %d != %d: %s in %s' % (n, expect, tag, path))
    write(path, s2)
    print('OK(%d):' % n, tag)


# ============================ tools/builtin ============================

# --- grep.go：相对路径按调用方的当前目录解析 ---
sub1('tools/builtin/grep.go',
     '\troot, err := t.resolve(start)',
     '\troot, err := t.resolveAt(ctx, start)',
     'grep.go resolveAt')

# --- run_python.go：脚本 cwd / script_path 锚点跟着会话 cwd ---
sub1('tools/builtin/run_python.go',
     '\tCwd     string        // 脚本工作目录（= 工作区根，见 NewRunPythonTool）',
     '\tCwd     string        // 脚本工作目录（= 工作区根，见 NewRunPythonTool）\n'
     '\n'
     '\t// wd 会话级 cwd 状态（nil = 脚本恒定跑在工作区根，见 NewRunPythonTool）。\n'
     '\t//\n'
     '\t// 为什么脚本也要接：脚本里的相对路径（open("out.csv")）写在**脚本工作目录**下，\n'
     '\t// 而脚本工作目录若恒为工作区根，模型 cd 到子目录后写出的产物就落在根上 ——\n'
     '\t// 与 bash / 文件工具各说各话。接上之后三者同一份真相。\n'
     '\twd *workdir.State',
     'run_python struct wd')

sub1('tools/builtin/run_python.go',
     '\t\tTimeout: 0,\n\t\tCwd:     workspace,',
     '\t\tTimeout: 0,\n\t\tCwd:     workspace,',
     'run_python ctor anchor (no-op check)')

# 新构造函数：插在 NewRunPythonTool 之后（以 ToolTimeout 注释为锚）
sub1('tools/builtin/run_python.go',
     '// ToolTimeout 实现 tools.ToolTimeoutProvider：脚本默认时限 + bootstrap 余量（见常量注释）。',
     '''// NewRunPythonToolWithWorkdir 创建**接会话级 cwd 状态**的 run_python 工具。
//
// wd = 会话级 cwd 状态（workdir.State）：脚本的**工作目录**取调用方分区的当前目录，
// script_path 的相对锚点同样跟着它 ⇒ 与 bash / 文件工具同一份真相。
//
// wd 为 nil ⇒ 退化成 NewRunPythonTool（工作区根），不 panic。
func NewRunPythonToolWithWorkdir(wd *workdir.State, py PythonRuntime, sbx sandbox.Sandbox) *RunPythonTool {
	if wd == nil {
		return NewRunPythonTool("", py, sbx)
	}
	t := NewRunPythonTool(wd.Root(), py, sbx)
	t.wd = wd
	t.ft = NewFileToolsWithWorkdir(wd)
	return t
}

// cwd 脚本工作目录（会话级）：有 workdir 状态时按调用方分区取；否则 = 构造时的目录。
func (t *RunPythonTool) cwd(ctx context.Context) string {
	if t.wd != nil {
		return t.wd.Current(CwdKey(ctx))
	}
	return t.Cwd
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：脚本默认时限 + bootstrap 余量（见常量注释）。''',
     'run_python NewRunPythonToolWithWorkdir')

sub1('tools/builtin/run_python.go',
     '\tsrc, scriptPath, err := t.resolveScript(a)',
     '\tsrc, scriptPath, err := t.resolveScript(ctx, a)',
     'run_python resolveScript call')

sub1('tools/builtin/run_python.go',
     'func (t *RunPythonTool) resolveScript(a runPythonArgs) (string, string, error) {',
     'func (t *RunPythonTool) resolveScript(ctx context.Context, a runPythonArgs) (string, string, error) {',
     'run_python resolveScript sig')

sub1('tools/builtin/run_python.go',
     '\tp, err := t.ft.resolve(a.ScriptPath)',
     '\tp, err := t.ft.resolveAt(ctx, a.ScriptPath)',
     'run_python resolveScript anchor')

sub1('tools/builtin/run_python.go',
     '\touts := t.declaredOutputs(a.Outputs)',
     '\touts := t.declaredOutputs(ctx, a.Outputs)',
     'run_python declaredOutputs call')

sub1('tools/builtin/run_python.go',
     'func (t *RunPythonTool) declaredOutputs(outputs []string) []runPythonOutput {',
     'func (t *RunPythonTool) declaredOutputs(ctx context.Context, outputs []string) []runPythonOutput {',
     'run_python declaredOutputs sig')

sub1('tools/builtin/run_python.go',
     '\t\tif abs, err := t.ft.resolve(raw); err == nil {',
     '\t\tif abs, err := t.ft.resolveAt(ctx, raw); err == nil {',
     'run_python declaredOutputs resolve')

sub1('tools/builtin/run_python.go',
     '\t\tCwd:              t.Cwd,',
     '\t\tCwd:              t.cwd(ctx),',
     'run_python exec cwd')

sub1('tools/builtin/run_python.go',
     '\t"github.com/seven7628/hai-harness/sandbox"',
     '\t"github.com/seven7628/hai-harness/sandbox"\n\t"github.com/seven7628/hai-harness/workdir"',
     'run_python import workdir')

# --- bash.go：cwd 记忆搬到会话级状态 ---
sub1('tools/builtin/bash.go',
     '\tmu     sync.Mutex\n\tcurCwd map[string]string // 当前工作目录，**按调用方分区**（见 cwdKey）；cd 命令成功后更新',
     '\tmu     sync.Mutex\n\tcurCwd map[string]string // 当前工作目录，**按调用方分区**（见 cwdKey）；cd 命令成功后更新\n'
     '\n'
     '\t// wd 会话级 cwd 状态（nil = 用上面的实例内 curCwd，见 NewBashToolWithWorkdir）。\n'
     '\t//\n'
     '\t// 为什么两套并存：wd != nil（生产装配）时 cwd 记忆放在**共享**的状态里 ⇒\n'
     '\t// bash / 文件工具 / run_python 同一份真相；wd == nil（库默认 / 独立使用）时\n'
     '\t// 保持实例内记忆，行为一字不变。\n'
     '\twd *workdir.State',
     'bash struct wd')

sub1('tools/builtin/bash.go',
     '// ToolTimeout 实现 tools.ToolTimeoutProvider：声明工具级默认时限',
     '''// NewBashToolWithWorkdir 创建**接会话级 cwd 状态**的 bash 工具。
//
// wd = 会话级 cwd 状态（workdir.State）：命令的**起始目录**取调用方分区的当前目录，
// cd 成功时也写回同一份状态 ⇒ 文件工具 / run_python 立刻看到同一个目录。
//
// 为什么不是"bash 记一份、别的工具再抄一份"：cwd 是运行期状态（模型随时 cd），
// 两份各记的必然漂移 —— 改造前 bash 与文件工具对不上账正是这么来的。
//
// wd 为 nil ⇒ 退化成 NewBashTool（工作区根），不 panic。
func NewBashToolWithWorkdir(wd *workdir.State, timeout time.Duration, sbx sandbox.Sandbox, appAllow []string) *BashTool {
	if wd == nil {
		return NewBashTool("", timeout, sbx, appAllow)
	}
	t := NewBashTool(wd.Root(), timeout, sbx, appAllow)
	t.wd = wd
	return t
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：声明工具级默认时限''',
     'bash NewBashToolWithWorkdir')

sub1('tools/builtin/bash.go',
     '''// cwdKey 调用方的 cwd 记忆分区键。
//
// **为什么分区**：工具实例在注册表里共享 —— 主 agent 与后台子 agent 共用同一个
// ToolEngine（desktop/bridge）⇒ 改造前 cwd 存在实例字段上 ⇒
// **子 agent 的 `cd` 会改掉主 agent 后续命令的工作目录**（两个 agent 互相踩）。
//
// 键取 TaskId（后台任务 id）：子 agent = 它的任务 id（跨轮稳定 ✓），
// 主 agent 直调 = ""（跨轮稳定 ✓ ⇒ 「cd 跨调用记忆」这个特性对主 agent 照常工作 ✓）。
func cwdKey(ctx context.Context) string {
	if tc := events.ToolContextFrom(ctx); tc != nil {
		return tc.TaskId
	}
	return ""
}
''',
     '''// cwdKey 调用方的 cwd 记忆分区键（实现见同包的 workdir.go）。
//
// **为什么分区**：工具实例在注册表里共享 —— 主 agent 与后台子 agent 共用同一个
// ToolEngine（desktop/bridge）⇒ cwd 若只有一份，子 agent 的 `cd` 会改掉主 agent
// 后续命令的工作目录（两个 agent 互相踩）。分区键 = 子 agent 的 task id
// （跨轮稳定 ✓），主 agent = workdir.MainKey（跨轮稳定 ✓）。
''',
     'bash cwdKey moved')

sub1('tools/builtin/bash.go',
     '''func (t *BashTool) currentCwd(ctx context.Context) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if dir := t.curCwd[cwdKey(ctx)]; dir != "" {
		return dir
	}
	return t.Cwd
}''',
     '''func (t *BashTool) currentCwd(ctx context.Context) string {
	key := cwdKey(ctx)
	if t.wd != nil {
		// 会话级状态：与文件工具 / run_python 共用同一份真相（见 workdir.go）。
		return t.wd.Current(key)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if dir := t.curCwd[key]; dir != "" {
		return dir
	}
	return t.Cwd
}''',
     'bash currentCwd')

sub1('tools/builtin/bash.go',
     '''func (t *BashTool) setCurrentCwd(ctx context.Context, dir string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.curCwd[cwdKey(ctx)] = dir
}''',
     '''func (t *BashTool) setCurrentCwd(ctx context.Context, dir string) {
	key := cwdKey(ctx)
	if t.wd != nil {
		t.wd.Set(key, dir)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.curCwd[key] = dir
}''',
     'bash setCurrentCwd')

sub1('tools/builtin/bash.go',
     '\t"github.com/seven7628/hai-harness/events"\n',
     '',
     'bash drop events import')

sub1('tools/builtin/bash.go',
     '\t"github.com/seven7628/hai-harness/tools"',
     '\t"github.com/seven7628/hai-harness/tools"\n\t"github.com/seven7628/hai-harness/workdir"',
     'bash import workdir')

# --- worktree.go：worktree 根取会话的工作区根 ---
sub1('tools/builtin/worktree.go',
     '// NewGitWorktreeTool 创建 git_worktree 工具；root 为工作区根目录（绝对路径）。',
     '''// NewGitWorktreeToolWithWorkdir 创建**接会话级 cwd 状态**的 git_worktree 工具。
//
// wd = 会话级 cwd 状态（workdir.State）：只用它的**工作区根**（worktree 统一放在
// {workspace}/.go-code/worktrees 下，是工作区级约定，不随 cd 变）。
//
// wd 为 nil ⇒ 退化成 NewGitWorktreeTool("")，不 panic。
func NewGitWorktreeToolWithWorkdir(wd *workdir.State) *GitWorktreeTool {
	if wd == nil {
		return NewGitWorktreeTool("")
	}
	return NewGitWorktreeTool(wd.Root())
}

// NewGitWorktreeTool 创建 git_worktree 工具；root 为工作区根目录（绝对路径）。''',
     'worktree NewGitWorktreeToolWithWorkdir')

sub1('tools/builtin/worktree.go',
     '\t"github.com/seven7628/hai-harness/tools"',
     '\t"github.com/seven7628/hai-harness/tools"\n\t"github.com/seven7628/hai-harness/workdir"',
     'worktree import workdir')

print('--- tools/builtin done ---')
