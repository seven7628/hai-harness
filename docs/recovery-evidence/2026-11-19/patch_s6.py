# 1) 回滚变异
p = 'desktop/bridge/main.go'
s = open(p, encoding='utf-8').read()
old = '\t\teng.RegisterTool(context.Background(), builtin.NewRunPythonTool(workspace, pyRT, sbx)) // ← 变异：run_python 另建 State'
new = '\t\teng.RegisterTool(context.Background(), builtin.NewRunPythonToolWithWorkdir(wd, pyRT, sbx))'
assert old in s, 'revert mutation'
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('mutation reverted')

# 2) tools/runtime.go：WorkdirAware 接口
p = 'tools/runtime.go'
s = open(p, encoding='utf-8').read()
old = '''type Tool interface {
	Name() string
	Description() string'''
new = '''// WorkdirAware 工具可暴露其**会话级 cwd 状态**。
//
// 用途：装配层要能证明「同一引擎里的 bash / 文件工具 / run_python 用的是**同一个**
// workdir.State」—— 否则它们的「当前目录」可能各走各的（静默错位，且行为层测不出来：
// 每个工具单独看都"对"）。
//
// ⚠️ 这是**装配断言**用的接口，不是业务接口：业务上工具之间不靠它通信
// （通信走 ctx 上的分区键，见 workdir.KeyFrom）。
type WorkdirAware interface {
	WorkdirState() *workdir.State
}

type Tool interface {
	Name() string
	Description() string'''
assert old in s, 'runtime'
s = s.replace(old, new, 1)
old_imp = '''	"github.com/seven7628/hai-harness/events"'''
assert old_imp in s, 'import'
s = s.replace(old_imp, '''	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/workdir"''', 1)
open(p, 'w', encoding='utf-8').write(s)
print('tools/runtime.go ok')

# 3) builtin.go：FileTools 暴露 State
p = 'tools/builtin/builtin.go'
s = open(p, encoding='utf-8').read()
old = '''// cwd 返回该调用方分区的当前工作目录（相对路径的解析基准）。'''
new = '''// WorkdirState 暴露本工具集使用的会话级 cwd 状态（tools.WorkdirAware）。
//
// 装配层用它证明「bash / 文件工具 / run_python 共用同一份」—— 见 tools.WorkdirAware。
func (f *FileTools) WorkdirState() *workdir.State { return f.wd }

// cwd 返回该调用方分区的当前工作目录（相对路径的解析基准）。'''
assert old in s, 'filetools'
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('tools/builtin/builtin.go ok')

# 4) bash.go：BashTool 暴露 State
p = 'tools/builtin/bash.go'
s = open(p, encoding='utf-8').read()
old = '''// projectRoot 返回项目根（越界重置的目标）。'''
new = '''// WorkdirState 暴露本工具使用的会话级 cwd 状态（tools.WorkdirAware）。
func (t *BashTool) WorkdirState() *workdir.State { return t.wd }

// projectRoot 返回项目根（越界重置的目标）。'''
assert old in s, 'bash'
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('tools/builtin/bash.go ok')

# 5) run_python.go：RunPythonTool 暴露 State
p = 'tools/builtin/run_python.go'
s = open(p, encoding='utf-8').read()
old = '''// cwd 返回该调用方分区的当前工作目录（脚本的工作目录）。'''
new = '''// WorkdirState 暴露本工具使用的会话级 cwd 状态（tools.WorkdirAware）。
func (t *RunPythonTool) WorkdirState() *workdir.State { return t.wd }

// cwd 返回该调用方分区的当前工作目录（脚本的工作目录）。'''
assert old in s, 'run_python'
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('tools/builtin/run_python.go ok')
