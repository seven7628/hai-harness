import re

# ============ 1) bridgeSession：加 wd ============
p = 'desktop/bridge/main.go'
s = open(p, encoding='utf-8').read()

old = '''type bridgeSession struct {
	id string'''
new = '''type bridgeSession struct {
	id string
	// wd 是**会话级**的 cwd 状态（workdir.State）。
	//
	// 为什么挂在会话上而不是引擎上：引擎（连同工具集）会随 **rebuildLoop** 重建
	//（切模型 / 切人格 / 切 provider）⇒ 状态若在引擎里，切一次模型就丢 cwd
	//（模型于是又开始每次 `cd X && ...`）。挂在会话上 ⇒ 跨重建存活。
	wd *workdir.State'''
assert old in s, 'bridgeSession'
s = s.replace(old, new, 1)

# ============ 2) buildLoop：加 wd 参数并透传 ============
old = '''func (m *manager) buildLoop(ws *workspaceRuntime, sid, prov, model, effort string, bgCtx func() context.Context, reg *subagent.Registry, qw *events.QuestionWaiter, sk *skills.Registry, profile personaProfile, gate sandbox.ReleaseGate) (*agents.AgentLoop, *subagentEffortRef, *skills.Capabilities, *tools.Engine) {'''
new = '''func (m *manager) buildLoop(ws *workspaceRuntime, sid, prov, model, effort string, wd *workdir.State, bgCtx func() context.Context, reg *subagent.Registry, qw *events.QuestionWaiter, sk *skills.Registry, profile personaProfile, gate sandbox.ReleaseGate) (*agents.AgentLoop, *subagentEffortRef, *skills.Capabilities, *tools.Engine) {'''
assert old in s, 'buildLoop sig'
s = s.replace(old, new, 1)

old = '''		eng, effortRef = m.buildPluginEngine(ws, sid, prov, model, effort, reg, qw, bgCtx)
	} else {
		eng, effortRef = newSessionEngineAt(ws.sessionsRoot(), workspace, sid, m.cron, m.provCfg, prov, model, effort, bgCtx, reg, qw, sk, ws.subagents, ws.mcp, ws.plugins, profile, sbx, m.hub, pyRT, m.loadSkillOnLoadHook(), m.bashEgoBeforeExec(), m.memoryConfig())'''
new = '''		eng, effortRef = m.buildPluginEngine(ws, sid, prov, model, effort, wd, reg, qw, bgCtx)
	} else {
		eng, effortRef = newSessionEngineAt(ws.sessionsRoot(), workspace, sid, wd, m.cron, m.provCfg, prov, model, effort, bgCtx, reg, qw, sk, ws.subagents, ws.mcp, ws.plugins, profile, sbx, m.hub, pyRT, m.loadSkillOnLoadHook(), m.bashEgoBeforeExec(), m.memoryConfig())'''
assert old in s, 'buildLoop engine calls'
s = s.replace(old, new, 1)

# ============ 3) newSessionEngineAt / newSessionEngine：加 wd 参数 ============
old = '''func newSessionEngine(workspace, sid string, cs *cron.Store, pc *providerConfig, prov, model, effort string, bgCtx func() context.Context, reg *subagent.Registry, qw *events.QuestionWaiter, sk *skills.Registry, subagentsReg *subagent.DefinitionRegistry, mcpm *mcp.Manager, plg *plugin.Registry, profile personaProfile, sbx sandbox.Sandbox, hub *session.Hub, pyRT *goruntime.Runtime, egoOnLoad func(string), egoBashBeforeExec func(string), memCfg ...memory.Config) (*tools.Engine, *subagentEffortRef) {
	return newSessionEngineAt(wsSessionsDir(workspace), workspace, sid, cs, pc, prov, model, effort, bgCtx, reg, qw, sk, subagentsReg, mcpm, plg, profile, sbx, hub, pyRT, egoOnLoad, egoBashBeforeExec, memCfg...)
}'''
new = '''func newSessionEngine(workspace, sid string, wd *workdir.State, cs *cron.Store, pc *providerConfig, prov, model, effort string, bgCtx func() context.Context, reg *subagent.Registry, qw *events.QuestionWaiter, sk *skills.Registry, subagentsReg *subagent.DefinitionRegistry, mcpm *mcp.Manager, plg *plugin.Registry, profile personaProfile, sbx sandbox.Sandbox, hub *session.Hub, pyRT *goruntime.Runtime, egoOnLoad func(string), egoBashBeforeExec func(string), memCfg ...memory.Config) (*tools.Engine, *subagentEffortRef) {
	return newSessionEngineAt(wsSessionsDir(workspace), workspace, sid, wd, cs, pc, prov, model, effort, bgCtx, reg, qw, sk, subagentsReg, mcpm, plg, profile, sbx, hub, pyRT, egoOnLoad, egoBashBeforeExec, memCfg...)
}'''
assert old in s, 'newSessionEngine'
s = s.replace(old, new, 1)

old = '''func newSessionEngineAt(sessionsRoot, workspace, sid string, cs *cron.Store,'''
new = '''func newSessionEngineAt(sessionsRoot, workspace, sid string, wd *workdir.State, cs *cron.Store,'''
assert old in s, 'newSessionEngineAt sig'
s = s.replace(old, new, 1)

old = '''	// 会话级 cwd 状态（唯一真值源）：bash / 文件工具 / run_python **共用**它
	// ⇒ 三者的「当前目录」永远是同一个值，不会各走各的。
	// 分区键 = 子 agent 的 task id ⇒ 主/子 agent 的目录互不干扰。
	wd := workdir.New(workspace)
	ft := builtin.NewFileToolsWithWorkdir(wd)'''
new = '''	// 会话级 cwd 状态（唯一真值源）：bash / 文件工具 / run_python **共用**它
	// ⇒ 三者的「当前目录」永远是同一个值，不会各走各的。
	// 分区键 = 子 agent 的 task id ⇒ 主/子 agent 的目录互不干扰。
	//
	// wd 由**会话**持有（bridgeSession.wd）⇒ 跨 rebuildLoop（切模型/切人格）存活；
	// 这里传 nil 时（独立/测试用法）退化为「按本引擎新建」。
	if wd == nil {
		wd = workdir.New(workspace)
	}
	ft := builtin.NewFileToolsWithWorkdir(wd)'''
assert old in s, 'engine wd'
s = s.replace(old, new, 1)

# ============ 4) rebuildLoop：传 bs.wd ============
old = '''	loop, effortRef, caps, eng := m.buildLoop(ws, bs.id, prov, model, effort,'''
new = '''	// 传 bs.wd ⇒ **cwd 跨重建存活**（切模型/切人格不该把工作目录清零）。
	loop, effortRef, caps, eng := m.buildLoop(ws, bs.id, prov, model, effort, bs.wd,'''
assert old in s, 'rebuildLoop'
s = s.replace(old, new, 1)

# ============ 5) 会话创建：建 State 并挂到 bs ============
old = '''	var hubEng *tools.Engine
	var eng *tools.Engine
	if opts.hub != nil {'''
new = '''	var hubEng *tools.Engine
	var eng *tools.Engine
	// 会话级 cwd 状态（跨 rebuildLoop 存活；见 bridgeSession.wd 的注释）。
	wd := workdir.New(wsPath)
	if opts.hub != nil {'''
assert old in s, 'session wd'
s = s.replace(old, new, 1)

old = '''		eng, eRef = m.buildHubEngine(ws, id, opts.hub, reg, qw, func() context.Context { return s.Context() })'''
new = '''		eng, eRef = m.buildHubEngine(ws, id, opts.hub, wd, reg, qw, func() context.Context { return s.Context() })'''
assert old in s, 'hub engine call'
s = s.replace(old, new, 1)

old = '''		loop, effortRef, skillCaps, eng = m.buildLoop(ws, id, m.provCfg.providerName(), m.preferredModel(), effort,
			func() context.Context { return s.Context() }, reg, qw, sk, profiles["code"], &sandboxReleaseGate{m: m, ws: wsPath, id: id}) // 默认 Persona = code'''
new = '''		loop, effortRef, skillCaps, eng = m.buildLoop(ws, id, m.provCfg.providerName(), m.preferredModel(), effort, wd,
			func() context.Context { return s.Context() }, reg, qw, sk, profiles["code"], &sandboxReleaseGate{m: m, ws: wsPath, id: id}) // 默认 Persona = code'''
assert old in s, 'buildLoop call'
s = s.replace(old, new, 1)

old = '''	bs = &bridgeSession{id: id, runtime: ws, s: s, loop: loop,'''
new = '''	bs = &bridgeSession{id: id, wd: wd, runtime: ws, s: s, loop: loop,'''
assert old in s, 'bs create'
s = s.replace(old, new, 1)

open(p, 'w', encoding='utf-8').write(s)
print('main.go ok')

# ============ 6) hub_tools.go：buildHubEngine 加 wd ============
p = 'desktop/bridge/hub_tools.go'
s = open(p, encoding='utf-8').read()
old = '''func (m *manager) buildHubEngine(ws *workspaceRuntime, sid string, spec *hubSessionSpec, reg *subagent.Registry, qw *events.QuestionWaiter, bgCtx func() context.Context) (*tools.Engine, *subagentEffortRef) {'''
new = '''func (m *manager) buildHubEngine(ws *workspaceRuntime, sid string, spec *hubSessionSpec, wd *workdir.State, reg *subagent.Registry, qw *events.QuestionWaiter, bgCtx func() context.Context) (*tools.Engine, *subagentEffortRef) {'''
assert old in s, 'buildHubEngine sig'
s = s.replace(old, new, 1)
old = '''	eng, effortRef := newSessionEngine(
		ws.path, sid, m.cron, m.provCfg, p.Provider, p.Model, effort,'''
new = '''	eng, effortRef := newSessionEngine(
		ws.path, sid, wd, m.cron, m.provCfg, p.Provider, p.Model, effort,'''
assert old in s, 'buildHubEngine call'
s = s.replace(old, new, 1)
if '"github.com/seven7628/hai-harness/workdir"' not in s:
    s = s.replace('\t"github.com/seven7628/hai-harness/tools"', '\t"github.com/seven7628/hai-harness/tools"\n\t"github.com/seven7628/hai-harness/workdir"', 1)
open(p, 'w', encoding='utf-8').write(s)
print('hub_tools.go ok')

# ============ 7) plugin_tools.go：buildPluginEngine 加 wd ============
p = 'desktop/bridge/plugin_tools.go'
s = open(p, encoding='utf-8').read()
old = '''func (m *manager) buildPluginEngine(ws *workspaceRuntime, sid, prov, model, effort string, reg *subagent.Registry, qw *events.QuestionWaiter, bgCtx func() context.Context) (*tools.Engine, *subagentEffortRef) {'''
new = '''func (m *manager) buildPluginEngine(ws *workspaceRuntime, sid, prov, model, effort string, wd *workdir.State, reg *subagent.Registry, qw *events.QuestionWaiter, bgCtx func() context.Context) (*tools.Engine, *subagentEffortRef) {'''
assert old in s, 'buildPluginEngine sig'
s = s.replace(old, new, 1)
old = '''	eng, effortRef := newSessionEngineAt('''
new = '''	eng, effortRef := newSessionEngineAt('''
assert old in s
# 在 newSessionEngineAt 的参数里插 wd：找到它调用的头几行
m = re.search(r'eng, effortRef := newSessionEngineAt\(\n?\s*([^\n]*)\n', s)
assert m, 'plugin call'
print('   plugin newSessionEngineAt 首参:', m.group(1)[:80])
if '"github.com/seven7628/hai-harness/workdir"' not in s:
    s = s.replace('\t"github.com/seven7628/hai-harness/tools"', '\t"github.com/seven7628/hai-harness/tools"\n\t"github.com/seven7628/hai-harness/workdir"', 1)
open(p, 'w', encoding='utf-8').write(s)
print('plugin_tools.go ok')
