# ================= A) tools/builtin/run_python.go =================
p = 'tools/builtin/run_python.go'
s = open(p, encoding='utf-8').read()

# A1) 构造器拆分
old = '''func NewRunPythonTool(workspace string, py PythonRuntime, sbx sandbox.Sandbox) *RunPythonTool {
	return &RunPythonTool{'''
new = '''func NewRunPythonTool(workspace string, py PythonRuntime, sbx sandbox.Sandbox) *RunPythonTool {
	return NewRunPythonToolWithWorkdir(workdir.New(workspace), py, sbx)
}

// NewRunPythonToolWithWorkdir 建一个**共享会话级 cwd 状态**的 run_python 工具。
//
// 生产路径用它：脚本的工作目录与 bash / 文件工具**同一个 cwd**
// —— 模型 cd 到子目录后，python 脚本也该在那个子目录里跑。
func NewRunPythonToolWithWorkdir(wd *workdir.State, py PythonRuntime, sbx sandbox.Sandbox) *RunPythonTool {
	return &RunPythonTool{'''
assert old in s, 'rp ctor'
s = s.replace(old, new, 1)

# A2) 字段赋值
old = '''		Timeout: 0,
		Cwd:     workspace,
		py:      py,
		ft:      NewFileTools(workspace),
		sbx:     sbx,'''
new = '''		Timeout: 0,
		Cwd:     wd.Root(),
		py:      py,
		ft:      NewFileToolsWithWorkdir(wd),
		wd:      wd,
		sbx:     sbx,'''
assert old in s, 'rp fields'
s = s.replace(old, new, 1)

# A3) ExecSpec 的 Cwd 跟随 cwd
old = '''		Cwd:              t.Cwd,'''
new = '''		Cwd:              t.cwd(ctx),'''
assert old in s, 'rp exec cwd'
s = s.replace(old, new, 1)

# A4) cwd(ctx) 助手
old = '''// ToolTimeout 实现 tools.ToolTimeoutProvider：脚本默认时限 + bootstrap 余量（见常量注释）。'''
new = '''// cwd 返回该调用方分区的当前工作目录（脚本的工作目录）。
//
// 与 bash / 文件工具同一把钥匙（cwdKey）⇒ 模型 cd 到哪，脚本就在哪跑。
func (t *RunPythonTool) cwd(ctx context.Context) string {
	if t.wd != nil {
		return t.wd.Current(cwdKey(ctx))
	}
	return t.Cwd
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：脚本默认时限 + bootstrap 余量（见常量注释）。'''
assert old in s, 'rp cwd helper'
s = s.replace(old, new, 1)

# A5) struct 加 wd + import
old = '''	Cwd     string        // 脚本工作目录（= 工作区根，见 NewRunPythonTool）'''
new = '''	Cwd     string        // 脚本工作目录（= 工作区根，见 NewRunPythonTool；运行期按 cwd(ctx) 现取）
	wd      *workdir.State // 会话级 cwd 状态（与 bash / 文件工具共用）'''
assert old in s, 'rp struct'
s = s.replace(old, new, 1)

old = '''	"github.com/seven7628/hai-harness/tools"'''
assert old in s, 'rp import'
s = s.replace(old, '''	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/workdir"''', 1)

open(p, 'w', encoding='utf-8').write(s)
print('run_python.go ok')

# ================= B) desktop/bridge/main.go =================
p = 'desktop/bridge/main.go'
s = open(p, encoding='utf-8').read()

# B1) newSessionEngineAt：建会话级 State
old = '''	eng := tools.NewToolEngine(tools.WithSpillDir(toolSpillDir(sid)))
	ft := builtin.NewFileTools(workspace)'''
new = '''	eng := tools.NewToolEngine(tools.WithSpillDir(toolSpillDir(sid)))
	// 会话级 cwd 状态（唯一真值源）：bash / 文件工具 / run_python **共用**它
	// ⇒ 三者的「当前目录」永远是同一个值，不会各走各的。
	// 分区键 = 子 agent 的 task id ⇒ 主/子 agent 的目录互不干扰。
	wd := workdir.New(workspace)
	ft := builtin.NewFileToolsWithWorkdir(wd)'''
assert old in s, 'bridge engine'
s = s.replace(old, new, 1)

# B2) run_python 用同一个 State
old = '''		eng.RegisterTool(context.Background(), builtin.NewRunPythonTool(workspace, pyRT, sbx))'''
new = '''		eng.RegisterTool(context.Background(), builtin.NewRunPythonToolWithWorkdir(wd, pyRT, sbx))'''
assert old in s, 'bridge run_python'
s = s.replace(old, new, 1)

# B3) explore loop 传 State
old = '''		exploreLoop := buildExploreLoop(workspace, sid, pc, prov, model, effort, sk)'''
new = '''		exploreLoop := buildExploreLoop(workspace, wd, sid, pc, prov, model, effort, sk)'''
assert old in s, 'bridge explore call'
s = s.replace(old, new, 1)

# B4) buildExploreLoop 签名 + 内部
old = '''func buildExploreLoop(workspace, sid string, pc *providerConfig, prov, model, effort string, sk *skills.Registry) *agents.AgentLoop {'''
new = '''func buildExploreLoop(workspace string, wd *workdir.State, sid string, pc *providerConfig, prov, model, effort string, sk *skills.Registry) *agents.AgentLoop {'''
assert old in s, 'buildExploreLoop sig'
s = s.replace(old, new, 1)

old = '''		agents.WithToolEngine(exploreToolEngine(workspace, sk, exploreSkillCaps)),'''
new = '''		agents.WithToolEngine(exploreToolEngine(workspace, wd, sk, exploreSkillCaps)),'''
assert old in s, 'buildExploreLoop engine'
s = s.replace(old, new, 1)

# B5) exploreToolEngine 签名 + 内部
old = '''func exploreToolEngine(workspace string, sk *skills.Registry, caps *skills.Capabilities) *tools.Engine {'''
new = '''func exploreToolEngine(workspace string, wd *workdir.State, sk *skills.Registry, caps *skills.Capabilities) *tools.Engine {'''
assert old in s, 'exploreToolEngine sig'
s = s.replace(old, new, 1)

old = '''	for _, t := range builtin.NewFileTools(workspace).Tools() {'''
new = '''	// 与主会话**共用**同一个 workdir.State ⇒ explore 子 agent 的 cd 落在
	// **自己的分区**（分区键 = 它的 task id）⇒ 不影响主 agent。
	for _, t := range builtin.NewFileToolsWithWorkdir(wd).Tools() {'''
assert old in s, 'exploreToolEngine tools'
s = s.replace(old, new, 1)

# B6) import
old = '''	"github.com/seven7628/hai-harness/tools"'''
assert old in s, 'bridge import'
s = s.replace(old, '''	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/workdir"''', 1)

open(p, 'w', encoding='utf-8').write(s)
print('desktop/bridge/main.go ok')
