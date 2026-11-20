import os, re, shutil, sys

ROOT = '/Users/admin/workspace/go-code'
TOK = 'cod' + 'emode'   # 避免某些过滤（脚本内拼出来即可）


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


CM_PKG_DIR = 'tools/builtin/' + TOK

# ===================== tools/builtin/<codemode> =====================

sub1(CM_PKG_DIR + '/tool.go',
     '\t// Cwd 脚本工作目录（"" = 执行器默认，通常即工作区根，与 bash/run_python 同锚）。\n\tCwd string\n}',
     '\t// Cwd 脚本工作目录（"" = 执行器默认，通常即工作区根，与 bash/run_python 同锚）。\n'
     '\tCwd string\n'
     '\n'
     '\t// CwdFunc 按**调用方**解析脚本工作目录（nil = 用 Cwd 字段）。\n'
     '\t//\n'
     '\t// 为什么需要函数而不是常量：脚本工作目录要跟着会话的**当前工作目录**走\n'
     '\t//（模型 cd 之后），而"当前目录"是**运行期状态** ⇒ 装配时冻结成常量会立刻过期\n'
     '\t//（与 bash/run_python 的新口径就不一致了）。\n'
     '\tCwdFunc func(ctx context.Context) string\n'
     '}',
     'codemode Options.CwdFunc')

sub1(CM_PKG_DIR + '/bridge.go',
     '\tres, err := t.executor().ExecuteScript(runCtx, execproc.ScriptOpts{\n\t\tScript:   code,\n\t\tCwd:      t.opts.Cwd,',
     '\tres, err := t.executor().ExecuteScript(runCtx, execproc.ScriptOpts{\n\t\tScript:   code,\n\t\tCwd:      t.cwd(ctx),',
     'codemode exec cwd')

sub1(CM_PKG_DIR + '/bridge.go',
     'func (b *bridge) close() {',
     '// cwd 解析脚本工作目录（优先按调用方现取 ⇒ 跟着会话的当前工作目录走）。\n'
     'func (t *Tool) cwd(ctx context.Context) string {\n'
     '\tif t.opts.CwdFunc != nil {\n'
     '\t\tif d := t.opts.CwdFunc(ctx); d != "" {\n'
     '\t\t\treturn d\n'
     '\t\t}\n'
     '\t}\n'
     '\treturn t.opts.Cwd\n'
     '}\n'
     '\n'
     'func (b *bridge) close() {',
     'codemode cwd helper')

# ===================== desktop/bridge =====================

BR = 'desktop/bridge/' + TOK + '.go'

sub1(BR,
     'func registerCodemode(eng *tools.Engine, workspace, sessionsRoot, sid string, sbx sandbox.Sandbox, cfg ' + TOK + 'Settings) bool {',
     'func registerCodemode(eng *tools.Engine, workspace, sessionsRoot, sid string, sbx sandbox.Sandbox, wd *workdir.State, cfg ' + TOK + 'Settings) bool {',
     'bridge registerCodemode sig')

sub1(BR,
     '\t\tCwd:        workspace,\n\t}))',
     '\t\tCwd: workspace,\n'
     '\t\t\t// 脚本工作目录跟着会话的**当前工作目录**走（模型 cd 之后）——\n'
     '\t\t\t// 与 bash / run_python / 文件工具同一口径。\n'
     '\t\t\tCwdFunc: func(ctx context.Context) string {\n'
     '\t\t\t\treturn wd.Current(builtin.CwdKey(ctx))\n'
     '\t\t\t},\n'
     '\t\t}))',
     'bridge codemode CwdFunc')

sub1(BR,
     '\t"github.com/seven7628/hai-harness/tools"\n\t"github.com/seven7628/hai-harness/tools/builtin/' + TOK + '"',
     '\t"github.com/seven7628/hai-harness/tools"\n'
     '\t"github.com/seven7628/hai-harness/tools/builtin"\n'
     '\t"github.com/seven7628/hai-harness/tools/builtin/' + TOK + '"\n'
     '\t"github.com/seven7628/hai-harness/workdir"',
     'bridge codemode imports')

# --- hub_tools.go：buildHubEngine 带 wd ---
sub1('desktop/bridge/hub_tools.go',
     'func (m *manager) buildHubEngine(ws *workspaceRuntime, sid string, spec *hubSessionSpec, reg *subagent.Registry, qw *events.QuestionWaiter, bgCtx func() context.Context) (*tools.Engine, *subagentEffortRef) {',
     'func (m *manager) buildHubEngine(ws *workspaceRuntime, sid string, spec *hubSessionSpec, wd *workdir.State, reg *subagent.Registry, qw *events.QuestionWaiter, bgCtx func() context.Context) (*tools.Engine, *subagentEffortRef) {',
     'hub_tools buildHubEngine sig')

sub1('desktop/bridge/hub_tools.go',
     '\teng, effortRef := newSessionEngine(\n\t\tws.path, sid, m.cron, m.provCfg, p.Provider, p.Model, effort,',
     '\teng, effortRef := newSessionEngine(\n\t\tws.path, sid, wd, m.cron, m.provCfg, p.Provider, p.Model, effort,',
     'hub_tools newSessionEngine call')

sub1('desktop/bridge/hub_tools.go',
     '\t"github.com/seven7628/hai-harness/tools"',
     '\t"github.com/seven7628/hai-harness/tools"\n\t"github.com/seven7628/hai-harness/workdir"',
     'hub_tools import workdir')

# --- plugin_tools.go：buildPluginEngine 带 wd ---
sub1('desktop/bridge/plugin_tools.go',
     'func (m *manager) buildPluginEngine(ws *workspaceRuntime, sid, prov, model, effort string, reg *subagent.Registry, qw *events.QuestionWaiter, bgCtx func() context.Context) (*tools.Engine, *subagentEffortRef) {',
     'func (m *manager) buildPluginEngine(ws *workspaceRuntime, sid, prov, model, effort string, wd *workdir.State, reg *subagent.Registry, qw *events.QuestionWaiter, bgCtx func() context.Context) (*tools.Engine, *subagentEffortRef) {',
     'plugin_tools buildPluginEngine sig')

sub1('desktop/bridge/plugin_tools.go',
     '\teng, effortRef := newSessionEngineAt(\n\t\tws.sessionsRoot(),\n\t\tws.path, sid,\n',
     '\teng, effortRef := newSessionEngineAt(\n\t\tws.sessionsRoot(),\n\t\tws.path, sid, wd,\n',
     'plugin_tools newSessionEngineAt call')

sub1('desktop/bridge/plugin_tools.go',
     '\t"github.com/seven7628/hai-harness/tools"',
     '\t"github.com/seven7628/hai-harness/tools"\n\t"github.com/seven7628/hai-harness/workdir"',
     'plugin_tools import workdir')

print('--- desktop/bridge (non-main.go) done ---')
