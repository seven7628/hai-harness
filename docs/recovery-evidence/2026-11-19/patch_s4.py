import re

# ================= 1) tools/runtime.go：ContextDescriber =================
p = 'tools/runtime.go'
s = open(p, encoding='utf-8').read()
old = '''type Tool interface {
	Name() string
	Description() string'''
new = '''// ContextDescriber 允许工具按**调用方上下文**给出描述。
//
// 用途：bash 工具的描述里要输出**该调用方当前的工作目录**
// —— 主 agent 与子 agent 各有自己的 cwd 分区 ⇒ 静态 Description() 给不出正确的值。
//
// 引擎侧：ToolParams(ctx) 优先用 DescribeFor(ctx)，没有则退回 Description()。
// （schema 每请求重建 ⇒ 描述里的 cwd 每次都是当前值。）
type ContextDescriber interface {
	DescribeFor(ctx context.Context) string
}

type Tool interface {
	Name() string
	Description() string'''
assert old in s, 'runtime'
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('tools/runtime.go ok')

# ================= 2) tools/engine.go：优先用 ctx 版描述 =================
p = 'tools/engine.go'
s = open(p, encoding='utf-8').read()
old = '''		schemas = append(schemas, core.ToolSchema{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})'''
new = '''		desc := t.Description()
		if cd, ok := t.(ContextDescriber); ok {
			// 按调用方上下文给描述（如 bash 的当前工作目录）。
			// schema 每请求重建 ⇒ 这里的值总是**当前**的。
			desc = cd.DescribeFor(ctx)
		}
		schemas = append(schemas, core.ToolSchema{
			Name:        t.Name(),
			Description: desc,
			Parameters:  t.Parameters(),
		})'''
assert old in s, 'engine'
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('tools/engine.go ok')

# ================= 3) tools/builtin/bash.go：描述带 cwd =================
p = 'tools/builtin/bash.go'
s = open(p, encoding='utf-8').read()

# 3a) 把长描述抽成包级常量
old = '''		BaseTool: tools.BaseTool{
			Name_:        "bash",
			Description_: "Execute a bash/shell command and returns its output'''
assert old not in s
m = re.search(r'\t\t\tDescription_: "(Execute a bash/shell command.*?)",\n', s, re.S)
assert m, 'desc literal'
literal = m.group(1)
# 从字面量还原成 Go 源码（它本身已是转义好的双引号串）
const_block = '''// bashDescription 是 bash 工具的描述正文（**不含**工作目录那一行）。
//
// ⚠️ 别在这里写"当前目录"：那一行是**每请求重算**的（见 DescribeFor），
// 写死就等于告诉模型一个过期的目录。
const bashDescription = "%s"

// describe 拼工具描述：固定格式 + 该调用方当前的工作目录。
//
// 为什么 cwd 要放进**工具描述**（而不是靠一条额外消息）：
//   · 模型每个请求都看得到工具 schema ⇒ 这是**最稳的 cwd 载体**
//   · 上下文压缩不动 schema ⇒ 压缩后模型仍然知道自己在哪
//   · 会话恢复后每请求重算 ⇒ 恢复后不会拿着历史里的旧目录
//   · 首轮就有 ⇒ 模型第一次 cd 之前就知道起点（不必先 cd 一次探路）
func (t *BashTool) describe(cwd string) string {
	return "Working directory: " + cwd + "\\n" + bashDescription
}

// DescribeFor 按调用方给出描述（含**该调用方当前的工作目录**）。
//
// 必须按 ctx 取分区键：主 agent 与子 agent 各有自己的 cwd
//（语义对齐 Claude Code：子 agent 不继承父级的 cd）。
func (t *BashTool) DescribeFor(ctx context.Context) string {
	cwd := t.Cwd
	if t.wd != nil {
		cwd = t.wd.Current(cwdKey(ctx))
	}
	return t.describe(cwd)
}
''' % literal
s = s[:m.start()] + '\t\t\tDescription_: t.describe(wd.Root()),\n' + s[m.end():]

# 3b) 插入常量与 describe/DescribeFor（放在构造函数之前）
anchor = '// NewBashTool 创建 bash 工具（FileTools.Tools() 默认注册）。'
assert anchor in s, 'anchor'
s = s.replace(anchor, const_block + '\n' + anchor, 1)

# 3c) Description() 也要带 cwd（无 ctx 时用根）
old = '''// ToolTimeout 实现 tools.ToolTimeoutProvider：声明工具级默认时限'''
new = '''// Description 静态版（无调用方上下文时用）：带项目根。
//
// 引擎在工具实现 ContextDescriber 时会优先用 DescribeFor(ctx)；
// 这里的返回值给「没有调用方」的场景（工具清单展示、审批文案等）。
func (t *BashTool) Description() string { return t.describe(t.Cwd) }

// ToolTimeout 实现 tools.ToolTimeoutProvider：声明工具级默认时限'''
assert old in s, 'desc method'
s = s.replace(old, new, 1)

open(p, 'w', encoding='utf-8').write(s)
print('tools/builtin/bash.go ok')

# ================= 4) agents/agent_loop.go =================
p = 'agents/agent_loop.go'
s = open(p, encoding='utf-8').read()

# 4a) AgentContext 加 workKey
old = '''	AgentTaskId string

	Handler events.EventHandler // 本次运行的事件消费者'''
new = '''	AgentTaskId string

	// workKey 是本次运行的 **cwd 分区键**（workdir.Key）。
	//
	// 与 AgentTaskId 不同：AgentTaskId 是「子智能体身份」（空 = 主会话运行 / 同步子运行），
	// workKey 是「哪个 agent 的目录」——同步子运行也要有**独立**分区，否则它的 cd
	// 会改掉主 agent 的目录（键为空 = 主 agent 的分区）。
	workKey string

	Handler events.EventHandler // 本次运行的事件消费者'''
assert old in s, 'ac struct'
s = s.replace(old, new, 1)

# 4b) taskID：同步子运行合成唯一分区键（R6）
old = '''		taskID = opts.TaskId // 本次运行的后台任务 id（空 = 主 Agent 运行）：子 agent 派子 agent时
		// spawn 工具据此把孙任务结果投回本 loop 的 inbox，而非主会话（见 subagent.Tool.runBackground）
	}'''
new = '''		taskID = opts.TaskId // 本次运行的后台任务 id（空 = 主 Agent 运行）：子 agent 派子 agent时
		// spawn 工具据此把孙任务结果投回本 loop 的 inbox，而非主会话（见 subagent.Tool.runBackground）
	}
	if taskID == "" && ac.Depth > 0 {
		// **同步子运行**（subagent.runSync 不设 opts.TaskId）必须有自己的 cwd 分区：
		// 键为空 = 主 agent 的分区 ⇒ 子 agent 的 cd 会改掉主 agent 的目录。
		// 键用 RunId+Depth：RunId 每次 Run 唯一（够区分不同同步子运行），Depth 区分嵌套层；
		// 该子运行内稳定（同步子运行只跑一个 Run）。
		taskID = fmt.Sprintf("sync:%s:%d", ac.RunId, ac.Depth)
	}
	ac.workKey = taskID'''
assert old in s, 'taskID'
s = s.replace(old, new, 1)

# 4c) schema 装配带上分区键
old = '''		Tools:     a.toolSchemas(streamCtx, ac.Mode),'''
new = '''		Tools:     a.toolSchemas(workdir.WithKey(streamCtx, ac.workKey), ac.Mode),'''
assert old in s, 'toolSchemas call'
s = s.replace(old, new, 1)

# 4d) import
old = '''	"github.com/seven7628/hai-harness/tools"'''
assert old in s, 'import'
s = s.replace(old, '''	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/workdir"''', 1)

open(p, 'w', encoding='utf-8').write(s)
print('agents/agent_loop.go ok')
