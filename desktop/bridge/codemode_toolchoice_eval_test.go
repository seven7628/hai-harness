// codemode_toolchoice_eval_test.go —— codemode 的「工具选择」评测（真实模型调用）。
//
// # 为什么需要这个评测
//
// codemode 的本意是编排 **MCP 工具**：MCP 工具在引擎里是 deferred（既不进模型工具表，
// 也不进 codemode 描述），模型只能靠脚本里的 searchTools() 现查现调。但 mode:"on" 的
// PrepareLoadout 会给**每一个已声明工具**追加一句
//
//	Also callable from a codemode script: tools.bash({...}) - same tool, ...
//
// 而 MCP 工具恰恰一个都不在「已声明」集合里 —— 于是这句推销语落在的**只有内置工具**。
// 真实数据（~/.go-code/events 全量 354,844 条 tool_run_start，截至 2026-10-23）：
//
//	codemode 调用 129 次：单次内置调用 46 次（35.7%，本该直调：bash 17 / todo_update 12 /
//	todo_add 3 等）、多次内置批处理 35 次（27.1%）、真正 MCP 26 次（20.2%）、
//	用到 searchTools/describeTool 仅 22 次（17.1%）。
//
// 也就是说：codemode 现在主要被用来包内置工具，而不是它设计的 MCP 场景。
//
// # 这个评测回答什么
//
// 「工具表的描述」如何改变模型**第一个**工具选择。三个臂，同一批 prompt、同一模型，
// 只换工具表：
//
//	current 线上形态：内置工具不带推销片段，intro 已收窄到「工具表外的工具」
//	legacy  2026-10-24 之前的形态（每个已声明工具都追加推销片段 + 旧 intro）
//	strict  拟议形态（intro 明说脚本里调内置会被拒；需引擎侧真拦截才成立）
//
// 判据是**确定性指标**（第一个工具调用的名字 + 脚本内容的静态分类），不用 LLM judge
// ——memory_p0_test.go 记过 judge 自洽率 89% 让整批结论作废的教训。
//
// # 怎么跑
//
//	HAI_EVAL_TOOLCHOICE=1 HAI_EVAL_PROVIDER=opendesign HAI_EVAL_MODEL=deepseek-v4-flash \
//	  HAI_EVAL_REPS=2 HAI_EVAL_REPORT=/tmp/codemode-eval.json \
//	  go test ./desktop/bridge -run TestCodemodeToolChoiceEval -v
//
// 默认**不跑**（HAI_EVAL_TOOLCHOICE 未设即 skip）：这是要花真金白银的真实调用，
// 不能混在常规 `go test` 里。凭证缺失/网关不可达走 baselineEnvOrSkip / baselinePing
// 的既有纪律（skip 或响亮失败，绝不假绿）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/todo"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin"
	"github.com/seven7628/hai-harness/tools/builtin/codemode"
)

// —— 工具表装配 ——

// evalMCPServer 复刻本机 settings.json 里配的两个 MCP server 的形态：exposure 写成
// "codemode"（mcp/tool.go 的 toToolExposure 会折成 deferred —— 不进模型工具表、不进
// codemode 描述，只能 searchTools 现查）。名字带与真实日志同款的哈希后缀，避免评测
// 里出现「比线上更容易猜到的工具名」这种失真。
type evalMCPServer struct {
	ns      string
	tools   map[string]string // 工具名 → canned 结果（脚本真跑时返回它）
	desc    string
	exposed tools.ToolExposure
}

var evalMCPServers = []evalMCPServer{
	{
		ns:      "ai-test",
		desc:    "ai-test 测试平台：环境、场景、数据集、任务、覆盖率",
		exposed: tools.ExposureDeferred,
		tools: map[string]string{
			"list_environments_7e382c": `{"project":"agent-server","environments":[{"name":"env-prod-eu","status":"running","owner":"platform"},{"name":"env-staging","status":"running"},{"name":"env-perf","status":"stopped"}]}`,
			"list_scenarios_c9b05b":    `{"project":"agent-server","scenarios":[{"name":"login-smoke","last_result":"passed"},{"name":"billing-refund","last_result":"failed"},{"name":"quota-guard","last_result":"passed"}]}`,
			"list_datasets_5b1f0e":     `{"datasets":[{"name":"ds-orders-2026q3","rows":120000}]}`,
			"describe_platform_2e9786": `{"platform":"ai-test","version":"3.4.1","projects":["agent-server","voice-server"]}`,
			"get_task_9d2a41":          `{"task":"t-8842","status":"passed","duration_s":312}`,
			"run_coverage_4c7e88":      `{"coverage":0.87,"covered":1740,"total":2000}`,
			"list_projects_1f6a03":     `{"projects":["agent-server","voice-server","api-test-platform"]}`,
			"get_environment_7b3d55":   `{"name":"env-staging","status":"running","pods":3}`,
		},
	},
	{
		ns:      "ai-deployment",
		desc:    "ai-deployment 发布平台：部署单、流水线、镜像、回滚",
		exposed: tools.ExposureDeferred,
		tools: map[string]string{
			"list_deployments_3a9c12":    `{"deployments":[{"id":"d-5511","image":"registry/agent-server:2026.10.23-1","status":"success","at":"2026-10-23T09:12:00Z"},{"id":"d-5509","image":"registry/agent-server:2026.10.22-3","status":"success","at":"2026-10-22T18:40:00Z"}]}`,
			"get_pipeline_6e2f70":        `{"pipeline":"p-221","stages":["build","test","deploy"],"status":"success"}`,
			"list_images_8c4b21":         `{"images":["registry/agent-server:2026.10.23-1","registry/agent-server:2026.10.22-3"]}`,
			"rollback_deployment_1d7a94": `{"deployment":"d-5511","rolled_back":true}`,
			"describe_deployment_5f3e28": `{"id":"d-5511","image":"registry/agent-server:2026.10.23-1","replicas":3,"status":"success"}`,
		},
	},
}

// evalStubTool MCP 工具替身：既能进工具表（评测 1 只读 schema），也能被脚本真的调用
// （评测 2 端到端：canned 结果 + 调用计数，不外连内网 MCP）。
type evalStubTool struct {
	name     string
	desc     string
	params   any
	exposure tools.ToolExposure
	ns       *tools.ToolNamespace
	result   string
	calls    atomic.Int64
}

func (t *evalStubTool) Name() string        { return t.name }
func (t *evalStubTool) Description() string { return t.desc }
func (t *evalStubTool) Parameters() any     { return t.params }
func (t *evalStubTool) CanParallel() bool   { return true }

func (t *evalStubTool) ValidParams(context.Context, string, string) error { return nil }

func (t *evalStubTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}

func (t *evalStubTool) Call(_ context.Context, _, args string) (string, error) {
	t.calls.Add(1)
	if t.result == "" {
		return "", errors.New("codemode eval stub: 本工具未配置 canned 结果")
	}
	return t.result, nil
}

func (t *evalStubTool) AfterCall(context.Context, core.ToolCall) {}

func (t *evalStubTool) Exposure() tools.ToolExposure { return t.exposure }

func (t *evalStubTool) Namespace() *tools.ToolNamespace { return t.ns }

// evalToolEngine 装配一张与生产同构的工具表：内置工具（文件/bash/todo/run_python）
// 走 direct（零迁移成本，见 tools/exposure.go 文件头），codemode 走 model-only，
// MCP 走 deferred。workdir 只用于让描述里的路径占位是真实路径。
// evalToolEngine 返回引擎 + 本次注册的 MCP 替身。
//
// **每样本一份引擎**（不再用包级变量收集替身）：计数是样本级状态，共享一份会让并发
// 采样互相污染（也会让「清零」变成竞态）。引擎构造只是把工具对象塞进 map，成本可忽略。
func evalToolEngine(workdir string) (*tools.Engine, []*evalStubTool) {
	stubs := make([]*evalStubTool, 0, 16)
	eng := tools.NewToolEngine()

	ctx := context.Background()
	ft := builtin.NewFileTools(workdir)
	for _, tl := range ft.Tools() { // read_file / write_file / edit_file / grep / glob / bash
		eng.RegisterTool(ctx, tl)
	}
	for _, tl := range todo.Tools() { // todo_add / todo_update
		eng.RegisterTool(ctx, tl)
	}
	// pyRT / sbx 传 nil：本评测只读 schema，不执行（run_python 的构造对二者无要求）。
	eng.RegisterTool(ctx, builtin.NewRunPythonTool(workdir, nil, nil))

	for _, srv := range evalMCPServers {
		ns := &tools.ToolNamespace{Name: srv.ns, Description: srv.desc}
		for name, result := range srv.tools {
			stub := &evalStubTool{
				name:     "mcp__" + strings.ReplaceAll(srv.ns, "-", "_") + "__" + name,
				desc:     srv.desc + "（" + name + "）",
				params:   tools.Obj(map[string]any{"project": tools.Str("项目名")}),
				exposure: srv.exposed,
				ns:       ns,
				result:   result,
			}
			eng.RegisterTool(ctx, stub)
			stubs = append(stubs, stub)
		}
	}

	// codemode 最后注册：它要能看见全部工具（Options.Tools 是闭包，顺序无关，但这样更直观）。
	eng.RegisterTool(ctx, codemode.New(codemode.Options{
		Engine: eng,
		Tools:  func() []tools.Tool { return eng.Tools() },
		Mode:   func() codemode.Mode { return codemode.ModeOn },
	}))
	return eng, stubs
}

// legacyIntroParagraph 2026-10-24 之前的 DESCRIPTION_INTRO 第二段（legacy 臂复原用）。
const legacyIntroParagraph = `Reach for this only when the step is a script. One tool call is not a script: a shell
step belongs in ONE bash call (commands joined with &&, loops, heredocs), a file read
belongs in read_file. A single-call script only adds a process spawn and returns the same
result as JSON instead of text.`

// currentIntroParagraph 线上 DESCRIPTION_INTRO 的第二段（strict 臂替换的对象）。
const currentIntroParagraph = `Use this mainly for tools that are NOT in your tool list - e.g. MCP server tools: call
searchTools(query), then call them. Built-in tools (bash, read_file, grep, todo_*) are
called directly, never through a script - a single call only adds a process spawn and
returns JSON instead of text. Scripts are for loops, concurrency, filtering large results.`

// strictIntroParagraph 拟议的「强拦截」措辞：脚本里调内置工具会被直接拒绝。
// 只有真的加了引擎侧拒绝，这段才成立——现在只用来**测**它在描述层的威慑力。
const strictIntroParagraph = `Use this only for tools that are NOT in your tool list - e.g. MCP server tools: call
searchTools(query), then call them. Built-in tools (bash, read_file, grep, todo_*) are not
available inside a script; calling one is rejected with an error. Call them directly
instead. Scripts are for loops, concurrency, filtering large results.`

// legacyNudge 复原旧形态的推销片段（codemodeDeclarationFormat 原文）。
func legacyNudge(name string) string {
	return "\n\nAlso callable from a codemode script: tools." + scriptName(name) +
		"({...}) - same tool, same permissions, same side effects."
}

// scriptName 与 codemode 的 scriptNameFor 同款归一（非标识符字符 → "_"）。
func scriptName(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

// toolSchemas 生成某臂的工具表。
//
// 三个臂（同一批 prompt、同一模型，只换工具表）：
//
//	current 线上形态：内置工具不带推销片段，intro 已收窄到「工具表外的工具」。
//	legacy  2026-10-24 之前的形态：每个已声明可编排工具都被追加
//	        "Also callable from a codemode script"，intro 还是旧措辞。
//	strict  拟议形态：intro 明说「脚本里调内置工具会被拒绝」（需引擎侧真拦截才成立，
//	        这里只测描述层的威慑力）。
//
// 复原 legacy 是为了让这份评测**长期可重跑**：改动落地后线上即 current，若没有
// legacy 臂，就没有任何可对照的基线了。
func toolSchemas(t *testing.T, eng *tools.Engine, arm string) []core.ToolSchema {
	t.Helper()
	schemas, err := eng.ToolParams(context.Background())
	if err != nil {
		t.Fatalf("ToolParams: %v", err)
	}
	switch arm {
	case "current":
		return schemas
	case "order":
		return codemodeFirst(schemas)
	case "legacy":
		out := make([]core.ToolSchema, 0, len(schemas))
		for _, s := range schemas {
			if s.Name == codemode.ToolName {
				s.Description = strings.Replace(s.Description, currentIntroParagraph, legacyIntroParagraph, 1)
			} else {
				s.Description += legacyNudge(s.Name)
			}
			out = append(out, s)
		}
		return out
	case "strict":
		return rewriteCodemodeIntro(schemas, currentIntroParagraph, strictIntroParagraph)
	}
	t.Fatalf("未知臂 %q", arm)
	return nil
}

func rewriteCodemodeIntro(in []core.ToolSchema, from, to string) []core.ToolSchema {
	out := make([]core.ToolSchema, 0, len(in))
	for _, s := range in {
		if s.Name == codemode.ToolName && strings.Contains(s.Description, from) {
			s.Description = strings.Replace(s.Description, from, to, 1)
		}
		out = append(out, s)
	}
	return out
}

// —— prompt 集 ——

// evalPrompt 一条评测用例。
//
// want 是「按产品意图应当发生的事」，用于汇总：builtin 类出现 codemode 即「被抢」；
// mcp 类出现 searchTools 才是「按设计工作」；control 类不该有任何工具调用。
type evalPrompt struct {
	id       string
	category string // builtin-single / builtin-batch / mcp-unknown / control
	text     string
	// want 期望的首选工具（"bash"/"todo_add"/... /"searchTools"/""=不该调用）。
	want string

	// followup 非空 = 第二回合。真实会话里「被抢」几乎都发生在**多步串联**：
	// 模型先直调一个工具拿到结果，再决定第二步怎么写。单回合的首调率测不到这一步，
	// 而生产日志里那 35.7% 的单次内置包装正是这种二步形态。第二回合喂一条桩结果
	// （不真的执行工具）再接这句追问，看它会不会突然改用脚本。
	followup   string
	stubResult string
}

func evalFollowupCount() int {
	n := 0
	for _, p := range evalPrompts {
		if p.followup != "" {
			n++
		}
	}
	return n
}

var evalPrompts = []evalPrompt{
	{
		id: "bash-test", category: "builtin-single", want: "bash",
		text:       "跑一下 go test ./tools/... 看看有没有失败的，只告诉我结论。",
		followup:   "刚那条命令的 tail 被截断了，把失败包里第一个测试的完整输出再抓一次。",
		stubResult: "ok  github.com/seven7628/hai-harness/tools\nok  github.com/seven7628/hai-harness/tools/builtin\nFAIL github.com/seven7628/hai-harness/tools/builtin/codemode\n--- FAIL: TestSelectCatalog (0.01s)\n[输出已截断：只保留头尾]",
	},
	{
		id: "bash-git", category: "builtin-single", want: "bash",
		text: "看看当前工作区有哪些未提交的改动，按文件列一下。",
	},
	{
		id: "todo-add", category: "builtin-single", want: "todo_add",
		text: "记一条 todo：把 codemode 的描述改成只服务 MCP 工具。",
	},
	{
		id: "grep-find", category: "builtin-single", want: "grep",
		text:       "全仓搜一下 PrepareLoadout 在哪些文件里被用到，把文件路径列出来。",
		followup:   "再统计一下每个文件里各命中了多少处，按命中数从多到少排。",
		stubResult: "tools/engine.go\ntools/exposure.go\ndesktop/bridge/codemode.go\ntools/builtin/codemode/tool.go\ntools/exposure_test.go",
	},
	{
		id: "read-head", category: "builtin-single", want: "read_file",
		text: "把 tools/exposure.go 的前 40 行原文给我看一下。",
	},
	{
		id: "python-calc", category: "builtin-single", want: "run_python",
		text:       "算一下 1 到 1000 里有多少个素数，用 Python 算。",
		followup:   "再从 1000 扫到 100000，把每个千位区间的素数个数都列出来。",
		stubResult: "168",
	},
	{
		id: "todo-batch", category: "builtin-batch", want: "todo_update",
		text:       "把当前所有未完成的 todo 全部标记为完成。",
		followup:   "再补 6 条 todo：E1 到 E6，都是「把 codemode 描述里的 MCP 说法写准」。",
		stubResult: "- [x] t1 改描述\n- [ ] t2 加测试\n- [x] t3 跑评测",
	},
	{
		id: "mcp-discover", category: "mcp-unknown", want: "searchTools",
		text:       "有没有工具能查 ai-test 平台的环境列表？有的话把 agent-server 项目的环境列出来。",
		followup:   "用你刚找到的那个工具，把 agent-server 项目下的环境查出来。",
		stubResult: "searchTools(\"ai-test environment\") => [{\"name\":\"mcp__ai_test__list_environments_7e382c\",\"description\":\"列出测试环境\"}]",
	},
	{
		id: "mcp-inventory", category: "mcp-unknown", want: "searchTools",
		text: "看看当前这个会话能用到哪些 MCP 工具，各自能干什么，简单列一下。",
	},
	{
		id: "mcp-deploy", category: "mcp-unknown", want: "searchTools",
		text: "能查 ai-deployment 的发布单吗？能的话看看最近一次部署是哪个镜像。",
	},
	{
		id: "control-no-tool", category: "control", want: "",
		text: "用一句话说明这个仓库是做什么的，不用看代码。",
	},
}

// —— 结果分类 ——

// evalCase 一条用例的一次采样结果（报告最小单元）。
type evalCase struct {
	Arm         string `json:"arm"`
	PromptID    string `json:"prompt_id"`
	Category    string `json:"category"`
	Rep         int    `json:"rep"`
	FirstTool   string `json:"first_tool"`
	ScriptKind  string `json:"script_kind,omitempty"` // codemode 脚本的静态分类
	SearchTools bool   `json:"search_tools,omitempty"`
	ArgsHead    string `json:"args_head,omitempty"`
	ContentHead string `json:"content_head,omitempty"`
	InputTokens int64  `json:"input_tokens,omitempty"`
	OutputToken int64  `json:"output_tokens,omitempty"`
	Err         string `json:"err,omitempty"`

	// 第二回合（多步串联）。生产日志里「被抢」的形态是：第一步直调、第二步把
	// 后续动作包进脚本 —— 单回合首调率看不见它。
	Turn2Tool        string `json:"turn2_tool,omitempty"`
	Turn2ScriptKind  string `json:"turn2_script_kind,omitempty"`
	Turn2SearchTools bool   `json:"turn2_search_tools,omitempty"`
	Turn2ArgsHead    string `json:"turn2_args_head,omitempty"`
}

var (
	evalDotRefRe     = regexp.MustCompile(`tools\.([A-Za-z_][A-Za-z0-9_]*)`)
	evalBracketRefRe = regexp.MustCompile(`tools\[\s*['"]([^'"]+)['"]\s*\]`)
	evalDotCallRe    = regexp.MustCompile(`tools\.([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	evalBracketCall  = regexp.MustCompile(`tools\[\s*['"]([^'"]+)['"]\s*\]\s*\(`)
)

// classifyScript codemode 脚本的静态分类：它到底在干什么。
//
// 只看源码文本，不看执行结果 —— 判据必须与模型措辞无关（同 memory eval 的教训）。
func classifyScript(code string) (kind string, search bool) {
	refs := append(evalDotRefRe.FindAllStringSubmatch(code, -1), evalBracketRefRe.FindAllStringSubmatch(code, -1)...)
	calls := append(evalDotCallRe.FindAllStringSubmatch(code, -1), evalBracketCall.FindAllStringSubmatch(code, -1)...)
	search = strings.Contains(code, "searchTools") || strings.Contains(code, "describeTool(")
	if len(calls) == 0 {
		if search {
			return "discover", true
		}
		return "compute", false
	}
	var mcp, builtinCalls int
	for _, m := range calls {
		if strings.HasPrefix(m[1], "mcp__") {
			mcp++
		} else {
			builtinCalls++
		}
	}
	_ = refs
	switch {
	case mcp > 0 && builtinCalls > 0:
		return "mixed", search
	case mcp > 0:
		return "mcp", search
	case len(calls) == 1:
		return "single-builtin", search
	default:
		return "batch-builtin", search
	}
}

// —— 真实调用 ——

// evalSystemPrompt 刻意保持精简：本次要测的是**工具表描述**的影响，system prompt
// 里任何「优先用专用工具」之类的偏好都会污染变量。生产 system prompt 的完整文本
// 见 agents/prompt/，它在这三个臂之间是常量，因此不影响相对比较。
func evalSystemPromptFor(workdir string) string {
	return `You are an interactive agent helping with a software repository.

## Harness
- Current workspace: ` + workdir + `
- Output format: prefer GFM.
- Use tools for anything beyond simple conversation; prefer the dedicated tool over raw shell where one fits. Independent tool calls may run in parallel — batch them when you can.
- Reply in the user's language. Do not explain your tool choice.`
}

// evalTurnResult 一次请求的归约结果：第一条工具调用 + 文本 + 可续接的上下文。
type evalTurnResult struct {
	tool     *core.ToolCall
	end      provider.LLMEndEvent
	content  string
	messages []core.Message
}

// evalTransientRe 判定「该重试」的瞬时限流错误（网关 tier_limit / 429 / rate limit）。
var evalTransientRe = regexp.MustCompile(`(?i)tier_limit|rate.?limit|429|too many requests|overloaded`)

// evalStreamRetry 对瞬时限流做指数退避重试（5s/15s/45s，最多 4 次尝试）。
//
// 为什么必须重试而不是把它记成一次「模型没调工具」：实测一轮 200 样本里，网关
// tier_limit_exceeded 会让某个臂的 84% 样本直接 400 —— 那些样本会被算成 text-only，
// 于是「成功率」变成「谁赶上了限流」。这不是模型行为，必须与模型行为分开。
func evalStreamRetry(ctx context.Context, p provider.Provider, prov, model string,
	schemas []core.ToolSchema, msgs []core.Message) (evalTurnResult, error) {
	backoff := []time.Duration{0, 5 * time.Second, 15 * time.Second, 45 * time.Second}
	var lastErr error
	for attempt, wait := range backoff {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return evalTurnResult{}, ctx.Err()
			case <-time.After(wait):
			}
		}
		res, err := evalStream(ctx, p, prov, model, schemas, msgs)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !evalTransientRe.MatchString(err.Error()) {
			return res, err // 非限流错误：不重试，直接暴露
		}
		_ = attempt
	}
	return evalTurnResult{}, fmt.Errorf("限流重试耗尽: %w", lastErr)
}

// evalStream 发一次真实请求（工具表原样传，非流式消费：只关心最终工具调用）。
func evalStream(ctx context.Context, p provider.Provider, prov, model string,
	schemas []core.ToolSchema, msgs []core.Message) (evalTurnResult, error) {

	out := evalTurnResult{messages: msgs}
	var content strings.Builder
	err := p.Stream(ctx, &provider.StreamRequest{
		Model:     model,
		Provider:  prov,
		SessionID: "codemode-toolchoice-eval",
		Messages:  msgs,
		Tools:     schemas,
	}, func(ev provider.StreamEvent) error {
		switch e := ev.(type) {
		case provider.LLMContentDeltaEvent:
			content.WriteString(e.Delta)
		case provider.LLMEndEvent:
			out.end = e
			if len(e.ToolCalls) > 0 {
				c := e.ToolCalls[0]
				out.tool = &c
			}
		}
		return nil
	})
	out.content = content.String()
	return out, err
}

// annotate 把一条工具调用写进 case（含 codemode 脚本的静态分类）。
func annotate(c *evalCase, turn int, call *core.ToolCall) {
	if call == nil {
		return
	}
	kind, search := "", false
	if call.Name == codemode.ToolName {
		var args struct {
			Code string `json:"code"`
		}
		if json.Unmarshal([]byte(call.Arguments), &args) == nil {
			kind, search = classifyScript(args.Code)
		} else {
			kind = "unparsable"
		}
	}
	if turn == 1 {
		c.FirstTool, c.ScriptKind, c.SearchTools = call.Name, kind, search
		c.ArgsHead = head(call.Arguments, 200)
		return
	}
	c.Turn2Tool, c.Turn2ScriptKind, c.Turn2SearchTools = call.Name, kind, search
	c.Turn2ArgsHead = head(call.Arguments, 200)
}

// evalCallOnce 跑一条用例（可能两回合）。
//
// 第二回合的意义：生产日志里那 46 次「单次内置调用」全是**串联中的第二步**——
// 模型先直调 bash/grep 拿到结果，再决定下一步怎么写，这时「也可从脚本调用」的
// 推销语才真正起作用。单回合首调率测不到这个面。
//
// 第二回合的桩结果不是执行结果（本评测不执行工具），只为让上下文形状与生产一致；
// 因此它只测「接着做下一步时选直调还是包脚本」，不测工具真实行为。
func evalCallOnce(ctx context.Context, p provider.Provider, prov, model string,
	schemas []core.ToolSchema, workdir string, pr evalPrompt) (evalCase, error) {

	msgs := []core.Message{
		core.NewSystemMessage(evalSystemPromptFor(workdir)),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: pr.text}),
	}
	r1, err := evalStreamRetry(ctx, p, prov, model, schemas, msgs)
	if err != nil {
		return evalCase{Err: err.Error()}, err
	}
	out := evalCase{ContentHead: head(r1.content, 160)}
	out.InputTokens = r1.end.Usage.Input
	out.OutputToken = r1.end.Usage.Output
	annotate(&out, 1, r1.tool)

	if pr.followup == "" || r1.tool == nil {
		return out, nil
	}

	turnMsgs := append([]core.Message(nil), msgs...)
	turnMsgs = append(turnMsgs,
		core.Message{
			Role:      core.Assistant,
			Content:   []core.Content{{Type: core.ContentTypeText, Content: r1.content}},
			Reasoning: r1.end.Reasoning,
			ToolCalls: []core.ToolCall{*r1.tool},
		},
		core.NewToolMessage(r1.tool.Id, pr.stubResult),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: pr.followup}),
	)
	r2, err := evalStreamRetry(ctx, p, prov, model, schemas, turnMsgs)
	if err != nil {
		out.Err = "turn2: " + err.Error()
		return out, nil
	}
	annotate(&out, 2, r2.tool)
	return out, nil
}

func head(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " ⏎ "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// —— 汇总 ——

type evalSummary struct {
	Arm                string  `json:"arm"`
	BuiltinIntentCases int     `json:"builtin_intent_cases"`
	CodemodeOnBuiltin  int     `json:"codemode_on_builtin"`
	BuiltinHijackRate  float64 `json:"builtin_hijack_rate"`
	DiscoveryCases     int     `json:"discovery_cases"`
	DiscoveryViaSearch int     `json:"discovery_via_searchtools"`
	DiscoveryRate      float64 `json:"discovery_rate"`
	DirectFirstTool    int     `json:"direct_first_tool"`
	TextOnly           int     `json:"text_only"`
	Errors             int     `json:"errors"`

	// 第二回合（多步串联）的「被抢」：第一步正常，第二步改用脚本。
	Turn2Cases         int     `json:"turn2_cases"`
	Turn2Codemode      int     `json:"turn2_codemode"`
	Turn2HijackRate    float64 `json:"turn2_hijack_rate"`
	Turn2ScriptOneshot int     `json:"turn2_single_builtin_script"`

	ToolSchemaChars       int      `json:"tool_schema_chars"`
	CodemodeDescChars     int      `json:"codemode_desc_chars"`
	AvgBuiltinDescChars   int      `json:"avg_builtin_desc_chars"`
	FirstToolDistribution []string `json:"first_tool_distribution"`
}

func summarize(arm string, cases []evalCase, schemas []core.ToolSchema) evalSummary {
	sum := evalSummary{Arm: arm}
	for _, s := range schemas {
		sum.ToolSchemaChars += len(s.Name) + len(s.Description)
		if s.Name == codemode.ToolName {
			sum.CodemodeDescChars = len(s.Description)
		} else {
			sum.AvgBuiltinDescChars += len(s.Description)
		}
	}
	if n := len(schemas) - 1; n > 0 {
		sum.AvgBuiltinDescChars /= n
	}
	dist := map[string]int{}
	for _, c := range cases {
		if c.Err != "" {
			sum.Errors++
			continue
		}
		if c.FirstTool == "" {
			sum.TextOnly++
		} else {
			sum.DirectFirstTool++
		}
		dist[c.FirstTool]++
		switch c.Category {
		case "builtin-single", "builtin-batch":
			sum.BuiltinIntentCases++
			if c.FirstTool == codemode.ToolName {
				sum.CodemodeOnBuiltin++
			}
		case "mcp-unknown":
			sum.DiscoveryCases++
			if c.FirstTool == codemode.ToolName && c.SearchTools {
				sum.DiscoveryViaSearch++
			}
		}
		if c.Turn2Tool != "" {
			sum.Turn2Cases++
			if c.Turn2Tool == codemode.ToolName {
				sum.Turn2Codemode++
				if c.Turn2ScriptKind == "single-builtin" {
					sum.Turn2ScriptOneshot++
				}
			}
		}
	}
	if sum.BuiltinIntentCases > 0 {
		sum.BuiltinHijackRate = float64(sum.CodemodeOnBuiltin) / float64(sum.BuiltinIntentCases)
	}
	if sum.DiscoveryCases > 0 {
		sum.DiscoveryRate = float64(sum.DiscoveryViaSearch) / float64(sum.DiscoveryCases)
	}
	if sum.Turn2Cases > 0 {
		sum.Turn2HijackRate = float64(sum.Turn2Codemode) / float64(sum.Turn2Cases)
	}
	keys := make([]string, 0, len(dist))
	for k := range dist {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return dist[keys[i]] > dist[keys[j]] })
	for _, k := range keys {
		name := k
		if name == "" {
			name = "(no-tool)"
		}
		sum.FirstToolDistribution = append(sum.FirstToolDistribution, fmt.Sprintf("%s×%d", name, dist[k]))
	}
	return sum
}

// TestCodemodeToolChoiceEval 三臂工具选择评测（默认 skip；HAI_EVAL_TOOLCHOICE=1 才跑）。
//
// 为什么默认不跑：每条用例都是一次真实付费请求（工具表本身就有上万 token），
// 混进常规 `go test` 既慢又贵。这不是「怕失败」，是成本闸门。
func TestCodemodeToolChoiceEval(t *testing.T) {
	if os.Getenv("HAI_EVAL_TOOLCHOICE") != "1" {
		t.Skip("工具选择评测：默认不跑（要真实付费调用）。启用：HAI_EVAL_TOOLCHOICE=1")
	}
	prov, model, p := baselineEnvOrSkip(t)
	baselinePing(t, p, prov, model)

	reps := evalReps(t)
	workdir := evalWorkdir(t)
	arms := []string{"current", "legacy", "strict"}
	if v := os.Getenv("HAI_EVAL_ARMS"); v != "" {
		arms = strings.Split(v, ",")
	}

	// 工具表按臂生成一次（同一臂内所有采样共用同一份 schema —— 前缀缓存友好，
	// 也保证同一臂内字节完全一致）。
	eng, _ := evalToolEngine(workdir)
	schemasByArm := map[string][]core.ToolSchema{}
	for _, arm := range arms {
		schemasByArm[arm] = toolSchemas(t, eng, arm)
	}

	if os.Getenv("HAI_EVAL_DRY") == "1" {
		for _, arm := range arms {
			s := summarize(arm, nil, schemasByArm[arm])
			t.Logf("[dry] %-8s schema_chars=%d codemode_desc=%d avg_builtin_desc=%d prompts=%d(2回合=%d) reps=%d → 约 %d 次真实调用，%d 次带工具表",
				arm, s.ToolSchemaChars, s.CodemodeDescChars, s.AvgBuiltinDescChars,
				len(evalPrompts), evalFollowupCount(), reps,
				(len(evalPrompts)+evalFollowupCount())*reps,
				(len(evalPrompts)+evalFollowupCount())*reps)
		}
		return
	}

	// 按 (臂, prompt, rep) 展开；并发 4 控制对网关的压力（不是越高越好：限流会污染数据）。
	type job struct {
		arm  string
		prom evalPrompt
		rep  int
	}
	var jobs []job
	for _, arm := range arms {
		for _, pr := range evalPrompts {
			for rep := 0; rep < reps; rep++ {
				jobs = append(jobs, job{arm: arm, prom: pr, rep: rep})
			}
		}
	}

	results := make([]evalCase, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, jb := range jobs {
		wg.Add(1)
		go func(i int, jb job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			c, err := evalCallOnce(ctx, p, prov, model, schemasByArm[jb.arm], workdir, jb.prom)
			c.Arm, c.PromptID, c.Category, c.Rep = jb.arm, jb.prom.id, jb.prom.category, jb.rep
			if err != nil && c.Err == "" {
				c.Err = err.Error()
			}
			results[i] = c
		}(i, jb)
	}
	wg.Wait()

	// 汇总 + 打印（测试日志即报告的可读部分）。
	byArm := map[string][]evalCase{}
	for _, c := range results {
		byArm[c.Arm] = append(byArm[c.Arm], c)
	}
	sums := make([]evalSummary, 0, len(arms))
	for _, arm := range arms {
		sums = append(sums, summarize(arm, byArm[arm], schemasByArm[arm]))
	}
	t.Logf("%-8s %-16s %-13s %-18s %-8s %s",
		"arm", "首回合抢用率", "MCP发现率", "第二回合抢用率", "错误", "首调分布")
	for _, s := range sums {
		t.Logf("%-8s %5.1f%% (%2d/%2d)  %5.1f%% (%2d/%2d)  %5.1f%% (%2d/%2d)   %d   %s",
			s.Arm,
			s.BuiltinHijackRate*100, s.CodemodeOnBuiltin, s.BuiltinIntentCases,
			s.DiscoveryRate*100, s.DiscoveryViaSearch, s.DiscoveryCases,
			s.Turn2HijackRate*100, s.Turn2Codemode, s.Turn2Cases,
			s.Errors, strings.Join(s.FirstToolDistribution, " "))
	}
	for _, c := range results {
		if c.Err != "" {
			t.Logf("  [错误] %s/%s rep%d: %s", c.Arm, c.PromptID, c.Rep, head(c.Err, 200))
			continue
		}
		kind := c.FirstTool
		if c.ScriptKind != "" {
			kind += "/" + c.ScriptKind
		}
		t.Logf("  %-8s %-14s rep%d → %-28s %s", c.Arm, c.PromptID, c.Rep, kind, head(c.ArgsHead, 90))
	}

	report := map[string]any{
		"meta": map[string]any{
			"provider": prov, "model": model, "reps": reps, "arms": arms,
			"git_head": baselineGitHEAD(t), "prompts": len(evalPrompts),
			"system_prompt": evalSystemPromptFor(workdir),
		},
		"summary": sums,
		"cases":   results,
	}
	if path := os.Getenv("HAI_EVAL_REPORT"); path != "" {
		raw, _ := json.MarshalIndent(report, "", " ")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("写报告 %s: %v", path, err)
		}
		t.Logf("报告已落盘：%s", path)
	}

	// 断言的取舍：n 小的时候「asis 一定比 nonudge 差」会被竞态/采样噪声推翻，
	// 所以默认只测量不断言；HAI_EVAL_ASSERT=1 时断言方向（去掉推销语不应变差）。
	if os.Getenv("HAI_EVAL_ASSERT") == "1" {
		var cur, legacy *evalSummary
		for i := range sums {
			switch sums[i].Arm {
			case "current":
				cur = &sums[i]
			case "legacy":
				legacy = &sums[i]
			}
		}
		if cur != nil && legacy != nil && cur.BuiltinHijackRate > legacy.BuiltinHijackRate {
			t.Fatalf("线上形态的内置工具被抢率反而高于旧形态：current=%.1f%% legacy=%.1f%%（样本 n=%d×%d，注意噪声）",
				cur.BuiltinHijackRate*100, legacy.BuiltinHijackRate*100, len(evalPrompts), reps)
		}
	}
	if sums[0].Errors == len(results) {
		t.Fatalf("全部 %d 次调用失败：玩法本身坏了，先修通路", len(results))
	}
}

// evalConcurrency 采样并发度（默认 6：对网关与 node 子进程都温和，又不至于把 n=100 拖成半小时）。
func evalConcurrency() int {
	if v := os.Getenv("HAI_EVAL_CONCURRENCY"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 6
}

// evalWorkdir 评测用的工作区根。
//
// 默认往上找到含 go.mod 的目录：`go test` 的 cwd 是**包目录**（desktop/bridge），
// 拿它当工作区会让模型把第一次调用花在 `ls`/`cd /workspace` 上 —— 测到的是定向
// 成本，不是工具选择。
func evalWorkdir(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("HAI_EVAL_WORKDIR"); v != "" {
		return v
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("找不到含 go.mod 的工作区根：用 HAI_EVAL_WORKDIR 显式指定")
		}
		dir = parent
	}
}

// evalReps 采样次数。默认 2（一次真实调用 ≈ 一份完整工具表，成本随 reps 线性上升），
// 上限不设：要更强的统计就显式给 HAI_EVAL_REPS。
func evalReps(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("HAI_EVAL_REPS")
	if raw == "" {
		return 2
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n <= 0 {
		t.Fatalf("HAI_EVAL_REPS=%q 解析失败（要一个正整数）", raw)
	}
	return n
}

// —— 评测 2：MCP 工具的「发现 + 调用」（隐式需求，端到端真执行）——

// 评测 1 的 MCP 用例全都写着「有没有工具能查…」—— 那是**指路**，测的是「被提示后会不会搜」。
// 真正的产品问题在这里：用户只描述业务任务（不提工具、不提 MCP），模型会不会**自己**
// 想到「这能力可能来自某个 server → 用 codemode + searchTools 找 → 找到就调」。
//
// 所以这个评测不看「选了哪个工具」就收工，而是把模型写出的脚本**真的执行**（真 node +
// 真引擎分派），用 MCP 替身的调用计数判断：它到底查到了没有、调到了没有。
type evalMCPPrompt struct {
	id   string
	text string
}

var evalMCPPrompts = []evalMCPPrompt{
	{id: "envs-implicit", text: "agent-server 项目现在有哪些测试环境？"},
	{id: "deploy-implicit", text: "ai-deployment 上最近一次部署用的是哪个镜像？"},
	{id: "scenarios-implicit", text: "把 agent-server 的测试场景列出来，标注哪些最近失败过。"},
	{id: "status-implicit", text: "帮我看下今天 agent-server 的部署状态，有没有回滚。"},
	{id: "leading-control", text: "有没有工具能查 ai-test 平台的环境列表？有的话把 agent-server 的环境列出来。"},
}

// evalMCPCase 一次采样的结局。stage 是**唯一判据**，取值见 classifyMCPStage。
type evalMCPCase struct {
	Arm        string `json:"arm"`
	PromptID   string `json:"prompt_id"`
	Rep        int    `json:"rep"`
	Stage      string `json:"stage"`
	FirstTool  string `json:"first_tool"`
	SearchUsed bool   `json:"search_used,omitempty"`
	StubCalls  int64  `json:"stub_calls"`
	CodeHead   string `json:"code_head,omitempty"`
	ResultHead string `json:"result_head,omitempty"`
	Err        string `json:"err,omitempty"`
}

// mcpServersIntro 只在 servers 臂追加：把连着的 server 名字写进描述。
//
// 动机（要验证的假设）：deferred 指引只说「有些工具没列出来，去 searchTools 找」，
// 却**从不提 server 名字**。模型面对「agent-server 有哪些测试环境」时，没有任何线索
// 把这句话和「ai-test 平台」联系起来。名字抖动会击穿一次前缀缓存（连/断 server 时），
// 但那是会话级的，不是每请求。
const mcpServersIntro = "\n\nConnected MCP servers: ai-test (test platform: environments, scenarios, datasets), " +
	"ai-deployment (release platform: deployments, images, pipelines). Call searchTools(query) to find their tools."

// mcpToolNamesIntro 自动推导版：只用**已注册工具的名字**当能力提示（去掉哈希后缀），
// 不依赖用户手写描述。生产可直接从引擎里已注册的 mcp__<server>__ 工具算出这句，
// 不需要额外配置字段、也不需要额外连接。
func mcpToolNamesIntro(limit int) string {
	var b strings.Builder
	b.WriteString("\n\nConnected MCP servers:")
	for _, srv := range evalMCPServers {
		b.WriteString(" " + srv.ns + " (")
		i := 0
		names := make([]string, 0, len(srv.tools))
		for name := range srv.tools {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if i >= limit {
				b.WriteString(", …")
				break
			}
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(strings.Split(name, "_2")[0]) // 去掉 _7e382c 这类哈希后缀
			i++
		}
		b.WriteString(")")
	}
	b.WriteString(". Call searchTools(query) to find these tools, then call them.")
	return b.String()
}

// mcpNamesIntro 只有名字（settings.json 的 mcpServers 里真实存在的信息量：没有描述）。
// 用来验证「必须带能力提示」还是「知道有这么个 server 就够」。
const mcpNamesIntro = "\n\nConnected MCP servers: ai-test, ai-deployment. " +
	"Call searchTools(query) to find their tools."

// sysPromptCapabilities 另一个候选落点：写进**宿主系统提示词**而不是工具 schema。
//
// 为什么可能更好：工具 schema 是前缀缓存的主体，改一个字节就全量失效；系统提示词每会话
// 一份、本来就带环境信息（workspace 路径、AGENTS.md），把「接了哪些 MCP server」放这里
// 既不影响 schema 字节，也天然随会话变化。
const sysPromptCapabilities = `

## Capabilities
- Some capabilities live behind MCP servers connected to this session: ai-test (test platform:
  environments, scenarios, datasets) and ai-deployment (release platform: deployments, images,
  pipelines).
- Their tools are NOT in your tool list. To use them, call the codemode tool and run
  searchTools(query) inside the script to find the tool, then call it in the same script.`

// sysPromptFor 评测臂 → 系统提示词（只有 sysprompt 臂加能力段）。
func sysPromptFor(workdir, arm string) string {
	base := evalSystemPromptFor(workdir)
	if arm == "sysprompt" || arm == "both" {
		return base + sysPromptCapabilities
	}
	return base
}

var deferredGuidanceTail = "Call searchTools(query) inside the script to find and call them."

// serverHintRe 匹配生产代码自动生成的「能力提示句」（descriptions.go:renderServerHints）。
//
// 为什么需要它：生产现在**自带**这句，所以任何对照臂都必须从「去掉这句」的基线上重建，
// 否则就是「生产提示 + 臂提示」叠两层 —— 一个臂自己跟自己比，读数全废。这是评测装置
// 的自身正确性问题，不是文案问题。
var serverHintRe = regexp.MustCompile(`\n\nConnected MCP servers \(their tools are not in your tool list\):[^\n]*\. Call searchTools\(query\) to find them, then call them\.`)

// stripServerHint 去掉自动能力提示句（= 改动前的线上形态）。
func stripServerHint(in []core.ToolSchema) []core.ToolSchema {
	out := make([]core.ToolSchema, 0, len(in))
	for _, s := range in {
		if s.Name == codemode.ToolName {
			s.Description = serverHintRe.ReplaceAllString(s.Description, "")
		}
		out = append(out, s)
	}
	return out
}

// withServerHint 在**无提示基线**上追加一句候选提示（紧跟延迟档指引之后）。
func withServerHint(in []core.ToolSchema, hint string) []core.ToolSchema {
	out := make([]core.ToolSchema, 0, len(in))
	for _, s := range in {
		if s.Name == codemode.ToolName && strings.Contains(s.Description, deferredGuidanceTail) {
			s.Description = strings.Replace(s.Description, deferredGuidanceTail, deferredGuidanceTail+hint, 1)
		}
		out = append(out, s)
	}
	return out
}

// mcpSchemas 生成某臂的工具表。**除 current 外，所有臂都从无提示基线重建**。
//
//	current    线上形态（自动提示句由生产代码生成）
//	nohint     无提示基线（改动前的线上形态，负对照）
//	names      只列 server 名（能力词缺位时的下限）
//	toolnames  名字 + 自动推导的工具名（= 生产现在用的形态，删掉后用臂复现）
//	servers    名字 + 手写能力描述（上界）
//	sysprompt  提示写进系统提示词（另一处落点）
//	both       servers + sysprompt
//	guidance   线上形态 + 改写的延迟档指引（本轮要 A/B 的候选）
func mcpSchemas(t *testing.T, eng *tools.Engine, arm string) []core.ToolSchema {
	t.Helper()
	production := toolSchemas(t, eng, "current")
	base := stripServerHint(production)
	switch arm {
	case "order":
		// 候选：把 codemode 提到工具表最前面（其余保持字典序）。
		// 动机：ToolParams 按名字排序 → bash 永远排在 codemode 之前；长工具表下模型可能
		// 只看前几个。代价是**整张工具表字节变化**（一次前缀缓存失效），所以必须用数据决定。
		return codemodeFirst(production)
	case "current", "searchhint":
		// searchhint 的工具表与线上一致：它的变量是**结果文本**，不是描述。
		return production
	case "nohint", "sysprompt":
		return base
	case "names":
		return withServerHint(base, mcpNamesIntro)
	case "toolnames":
		return withServerHint(base, mcpToolNamesIntro(5))
	case "servers", "both":
		return withServerHint(base, mcpServersIntro)
	case "guidance":
		return withGuidance(production, evalGuidanceCandidate)
	}
	t.Fatalf("未知臂 %q", arm)
	return nil
}

// codemodeFirst 把 codemode 条目挪到最前（其余顺序不变）。
func codemodeFirst(in []core.ToolSchema) []core.ToolSchema {
	out := make([]core.ToolSchema, 0, len(in))
	for _, s := range in {
		if s.Name == codemode.ToolName {
			out = append(out, s)
		}
	}
	for _, s := range in {
		if s.Name != codemode.ToolName {
			out = append(out, s)
		}
	}
	return out
}

// withGuidance 把延迟档指引换成候选文案（只动那一段，其余字节不变）。
func withGuidance(in []core.ToolSchema, newGuidance string) []core.ToolSchema {
	out := make([]core.ToolSchema, 0, len(in))
	for _, s := range in {
		if s.Name == codemode.ToolName {
			s.Description = strings.Replace(s.Description, codemode.DEFERRED_TOOLS_GUIDANCE, newGuidance, 1)
		}
		out = append(out, s)
	}
	return out
}

// searchResultHint 候选：追加在「只搜没调」的脚本结果后面（评测臂 searchhint）。
//
// 针对的是 search-only 那类失败：模型搜到了工具名就停下来（等用户确认 / 忘了下一步）。
// 放在结果里而不是描述里，是因为此刻它刚好在读这段内容。
const searchResultHint = `

[next step] These are the real tools. Call the matching one now in the same style, e.g. ` +
	`await tools["<name>"]({...}), and return its result - do not stop at the search.`

// evalGuidanceExtra 候选增量：在现有指引**后面加两句**（不改前文，最小改动）。
//
// 针对两类残留失败：① 部分样本仍直接走 bash（不知道该先看已连接 server）；② 少数样本
// 搜到工具名后停住、不调（指望用户确认）。
//
// **已测，未采纳（2026-10-24）**：隐式需求、n=32/臂
//
//	线上（不带这两句）  56.2%（18/32）
//	带这两句            68.8%（22/32）
//
// 方向为正但置信区间大幅重叠（two-proportion z≈0.66，p≈0.5），且另一次 n=12 的反向结果
// （66.7% vs 58.3%）抵消了它。结论：**证据不足，不进生产**。保留此臂是为了下次重跑时
// 不必重新推导同一假设 —— 要翻案就先把 n 翻到 ≥100/臂。
const evalGuidanceExtra = ` The servers listed below are already connected: if one looks relevant, search it
first, then call the matching tool in the same script - do not ask the user first.`

// evalGuidanceCandidate = 现有指引 + 候选增量。
const evalGuidanceCandidate = codemode.DEFERRED_TOOLS_GUIDANCE + evalGuidanceExtra

// classifyMCPStage 把一次采样归到唯一阶段。
//
//	codemode:invoke     ★ 期望：脚本里搜到并调用了 MCP 工具（stub 计数 > 0）
//	codemode:search-only 搜了但没调（或调错了名字）
//	codemode:no-mcp    用了脚本，但既没搜也没调 MCP
//	direct:<tool>      选了非 codemode 工具（MCP 不可达，这条路径必然失败）
//	text-only          直接文字回答（多半是「我没有这个能力」）
//	exec-error         脚本执行失败
func classifyMCPStage(first *core.ToolCall, stubCalls int64, searchUsed bool, execErr error) string {
	switch {
	case first == nil:
		return "text-only"
	case execErr != nil:
		return "exec-error"
	case first.Name != codemode.ToolName:
		return "direct:" + first.Name
	case stubCalls > 0:
		return "codemode:invoke"
	case searchUsed:
		return "codemode:search-only"
	default:
		return "codemode:no-mcp"
	}
}

// evalMCPSample 跑一条隐式需求用例：模型选路 →（若选 codemode）真执行脚本 → 归一阶段。
//
// 多轮而不是单轮：真实会话里 searchTools 的结果会作为工具结果回灌给模型，它应当**接着**
// 把找到的 MCP 工具调出来。只测第一轮会把「搜了、但还没到调用那一步」误判成失败。
// 上限 3 轮（发现 → 调用 → 收尾），超了就按当前阶段计。
func evalMCPSample(ctx context.Context, p provider.Provider, prov, model, workdir string,
	eng *tools.Engine, stubs []*evalStubTool, schemas []core.ToolSchema, arm string, pr evalMCPPrompt) evalMCPCase {

	for _, st := range stubs { // 只统计本次采样真正发生的调用
		st.calls.Store(0)
	}
	countCalls := func() int64 {
		var n int64
		for _, st := range stubs {
			n += st.calls.Load()
		}
		return n
	}
	msgs := []core.Message{
		core.NewSystemMessage(sysPromptFor(workdir, arm)),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: pr.text}),
	}
	out := evalMCPCase{PromptID: pr.id}
	var searchUsed bool
	var execErr error
	var modelErr bool // 模型/网关侧失败（与「模型没调工具」严格区分）
	var first *core.ToolCall

	for round := 0; round < 3; round++ {
		r, err := evalStreamRetry(ctx, p, prov, model, schemas, msgs)
		if err != nil {
			out.Err = "round" + strconv.Itoa(round) + ": " + err.Error()
			modelErr = true
			break
		}
		if r.tool == nil {
			break // 纯文本收尾
		}
		if round == 0 {
			first = r.tool
			out.FirstTool = r.tool.Name
		}
		if r.tool.Name != codemode.ToolName {
			// 非 codemode 工具：本评测不执行内置工具（bash 会真跑命令），按选路结果计。
			// 注意**不写 Err**：Err 只留给真错误（限流/网络/执行失败），否则错误率会被污染。
			break
		}
		var args struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal([]byte(r.tool.Arguments), &args)
		if round == 0 {
			out.CodeHead = head(args.Code, 240)
		}
		if kind, search := classifyScript(args.Code); search || kind == "discover" {
			searchUsed = true
		}
		before := countCalls()
		result, cerr := evalExecCodemode(ctx, eng, r.tool.Arguments)
		// 候选臂 searchhint：这一轮只搜没调时，把「下一步」写进**工具结果**里。
		// 作用点与「改工具描述」不同 —— 描述是常驻上下文，结果是模型刚看一眼的地方。
		if arm == "searchhint" && cerr == nil && countCalls() == before {
			if _, searched := classifyScript(args.Code); searched {
				result += searchResultHint
			}
		}
		if cerr != nil {
			execErr = cerr
			out.Err = cerr.Error()
			break
		}
		if round == 0 {
			out.ResultHead = head(result, 240)
		}
		msgs = append(msgs,
			core.Message{Role: core.Assistant, Content: []core.Content{{Type: core.ContentTypeText, Content: r.content}},
				Reasoning: r.end.Reasoning, ReasoningSignature: r.end.ReasoningSignature,
				ToolCalls: []core.ToolCall{*r.tool}},
			core.NewToolMessage(r.tool.Id, result),
		)
	}

	out.StubCalls = countCalls()
	out.SearchUsed = searchUsed
	out.Stage = finalMCPStage(first, out.StubCalls, searchUsed, execErr, modelErr)
	return out
}

// finalMCPStage 归一阶段：模型/网关侧失败**优先**（不进分母），其余交给 classifyMCPStage。
func finalMCPStage(first *core.ToolCall, stubCalls int64, searchUsed bool, execErr error, modelErr bool) string {
	if modelErr {
		return "model-error"
	}
	return classifyMCPStage(first, stubCalls, searchUsed, execErr)
}

// evalExecCodemode 用生产同款入口执行一段脚本（codemode.Call → 引擎 → 替身工具）。
func evalExecCodemode(ctx context.Context, eng *tools.Engine, arguments string) (string, error) {
	tool, err := eng.GetTool(ctx, codemode.ToolName)
	if err != nil {
		return "", err
	}
	ectx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return tool.Call(ectx, codemode.ToolName, arguments)
}

// TestCodemodeMCPDiscoveryEval 隐式 MCP 需求下，「发现 + 调用」的端到端成功率。
//
// 与 TestCodemodeToolChoiceEval 的分工：那个测「首调选谁」（描述层的影响），这个测
// 「能不能把 MCP 工具真的找出来并调用」（能力层的结果）。默认 skip，同款成本闸门。
func TestCodemodeMCPDiscoveryEval(t *testing.T) {
	if os.Getenv("HAI_EVAL_TOOLCHOICE") != "1" {
		t.Skip("MCP 发现评测：默认不跑（要真实付费调用 + 真 node 执行）。启用：HAI_EVAL_TOOLCHOICE=1")
	}
	prov, model, p := baselineEnvOrSkip(t)
	baselinePing(t, p, prov, model)

	reps := evalReps(t)
	workdir := evalWorkdir(t)
	build, _ := evalToolEngine(workdir) // 只为生成 schema（字节与逐样本引擎一致）
	arms := []string{"current", "searchhint", "servers", "sysprompt", "legacy"}
	if v := os.Getenv("HAI_EVAL_ARMS"); v != "" {
		arms = strings.Split(v, ",")
	}
	schemasByArm := map[string][]core.ToolSchema{}
	for _, arm := range arms {
		schemasByArm[arm] = mcpSchemas(t, build, arm)
	}

	// 样本并行：每样本**独立引擎**（计数是样本级状态，共享会互相污染）。
	// 串行时 100 样本 ≈ 半小时，并行 6 路后 ≈ 5 分钟 —— n 才拉得起来。
	type mcpJob struct {
		arm  string
		prom evalMCPPrompt
		rep  int
	}
	var jobs []mcpJob
	for _, arm := range arms {
		for _, pr := range evalMCPPrompts {
			for rep := 0; rep < reps; rep++ {
				jobs = append(jobs, mcpJob{arm: arm, prom: pr, rep: rep})
			}
		}
	}
	cases := make([]evalMCPCase, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, evalConcurrency())
	for i, jb := range jobs {
		wg.Add(1)
		go func(i int, jb mcpJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			eng, stubs := evalToolEngine(workdir)
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()
			c := evalMCPSample(ctx, p, prov, model, workdir, eng, stubs, schemasByArm[jb.arm], jb.arm, jb.prom)
			c.Arm, c.Rep = jb.arm, jb.rep
			cases[i] = c
		}(i, jb)
	}
	wg.Wait()

	// 汇总：触发率 / 成功发现并调用率 / 端到端数据到达率。
	type mcpSum struct {
		Arm          string         `json:"arm"`
		Cases        int            `json:"cases"`
		CodemodeUsed int            `json:"codemode_used"`
		Invoked      int            `json:"invoked"`
		SearchOnly   int            `json:"search_only"`
		DirectOther  int            `json:"direct_other"`
		TextOnly     int            `json:"text_only"`
		Errors       int            `json:"errors"`
		InvokeRate   float64        `json:"invoke_rate"`
		InvokeCILo   float64        `json:"invoke_ci_low"`
		InvokeCIHi   float64        `json:"invoke_ci_high"`
		Stages       map[string]int `json:"stages"`
	}
	sums := make([]mcpSum, 0, len(arms))
	for _, arm := range arms {
		s := mcpSum{Arm: arm, Stages: map[string]int{}}
		for _, c := range cases {
			if c.Arm != arm {
				continue
			}
			s.Cases++
			s.Stages[c.Stage]++
			switch {
			case c.Stage == "codemode:invoke":
				s.Invoked++
				s.CodemodeUsed++
			case c.Stage == "codemode:search-only" || c.Stage == "codemode:no-mcp":
				s.CodemodeUsed++
				if strings.HasSuffix(c.Stage, "search-only") {
					s.SearchOnly++
				}
			case strings.HasPrefix(c.Stage, "direct:"):
				s.DirectOther++
			case c.Stage == "text-only":
				s.TextOnly++
			case c.Stage == "model-error":
				s.Errors++
			default:
				s.Errors++
			}
		}
		if valid := s.Cases - s.Errors; valid > 0 {
			s.InvokeRate = float64(s.Invoked) / float64(valid)
			s.InvokeCILo, s.InvokeCIHi = wilson(s.Invoked, valid)
		}
		sums = append(sums, s)
	}

	t.Logf("%-10s %-22s %-28s %-14s %s", "arm", "用了 codemode", "★真调到 MCP（95% CI）", "选了别的工具", "阶段分布")
	for _, s := range sums {
		lo, hi := wilson(s.Invoked, s.Cases)
		t.Logf("%-10s %5.1f%% (%2d/%2d)        %5.1f%% (%2d/%2d)  [%4.1f%%, %4.1f%%]   %5.1f%% (%2d/%2d)  %v",
			s.Arm,
			pct(s.CodemodeUsed, s.Cases), s.CodemodeUsed, s.Cases,
			pct(s.Invoked, s.Cases), s.Invoked, s.Cases, lo, hi,
			pct(s.DirectOther, s.Cases), s.DirectOther, s.Cases,
			s.Stages)
	}
	for _, c := range cases {
		t.Logf("  %-8s %-20s rep%d → %-22s stubcalls=%d  %s",
			c.Arm, c.PromptID, c.Rep, c.Stage, c.StubCalls, head(c.CodeHead, 80))
	}

	report := map[string]any{
		"meta": map[string]any{
			"provider": prov, "model": model, "reps": reps, "arms": arms,
			"git_head": baselineGitHEAD(t), "prompts": len(evalMCPPrompts),
			"note": "评测 2：隐式 MCP 需求；脚本真执行，MCP 工具是 canned 替身（内网不可达）",
		},
		"summary": sums,
		"cases":   cases,
	}
	if path := os.Getenv("HAI_EVAL_REPORT"); path != "" {
		raw, _ := json.MarshalIndent(report, "", " ")
		if err := os.WriteFile(path+".mcp.json", raw, 0o600); err != nil {
			t.Fatalf("写报告: %v", err)
		}
		t.Logf("报告已落盘：%s.mcp.json", path)
	}
}

// wilson 比例的 95% Wilson 置信区间（小 n 时比正态近似可靠；用来判断「差异是否只是噪声」）。
func wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 100
	}
	const z = 1.96
	p := float64(k) / float64(n)
	den := 1 + z*z/float64(n)
	mid := (p + z*z/(2*float64(n))) / den
	half := z * math.Sqrt(p*(1-p)/float64(n)+z*z/(4*float64(n)*float64(n))) / den
	lo, hi = (mid-half)*100, (mid+half)*100
	if lo < 0 {
		lo = 0
	}
	if hi > 100 {
		hi = 100
	}
	return lo, hi
}

func pct(a, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(a) / float64(n) * 100
}

// TestCodemodeMCPEvalSelfCheck 评测装置的**自检**（不联网、不进真实调用，常规 `go test` 就跑）。
//
// 为什么必须先有它：评测给出的数字只有装置本身可信才有意义。它钉四件事：
//  1. 各对照臂之间**只差候选文案**：生产已自带能力提示句，任何臂若在 current 上再叠一句，
//     就成了「自己跟自己比」——这里用出现次数当场抓住（0 次或恰好 1 次，绝不允许 2 次）。
//  2. 脚本**真执行**确实能调到一个 MCP 替身：结果里必须出现 canned 标记，且调用计数 == 1。
//  3. `searchTools` 本身**不算**「调到 MCP」：只搜不调的脚本必须计 0 —— 否则 invoke 指标
//     把「搜索」当成「调用」，成功率会凭空变高。
//  4. 阶段判定是纯函数、取值互斥（真值表），避免分类漂移悄悄改结论。
func TestCodemodeMCPEvalSelfCheck(t *testing.T) {
	eng, stubs := evalToolEngine(evalWorkdir(t))
	if len(stubs) == 0 {
		t.Fatal("评测装置没注册任何 MCP 替身")
	}
	descOf := func(schemas []core.ToolSchema) string {
		for _, s := range schemas {
			if s.Name == codemode.ToolName {
				return s.Description
			}
		}
		return ""
	}

	// ① 臂隔离：nohint/sysprompt 该是 0 次，其余恰好 1 次。
	for _, c := range []struct {
		arm  string
		want int
	}{
		{"current", 1}, {"nohint", 0}, {"names", 1}, {"toolnames", 1},
		{"servers", 1}, {"both", 1}, {"sysprompt", 0}, {"guidance", 1},
		{"searchhint", 1}, // searchhint 只改**结果文本**，不动描述
	} {
		got := strings.Count(descOf(mcpSchemas(t, eng, c.arm)), "Connected MCP servers")
		if got != c.want {
			t.Errorf("臂 %s：能力提示句出现 %d 次，want %d（2 次 = 叠了两层，读数会失真）", c.arm, got, c.want)
		}
	}
	// 生产描述确实带提示句且带延迟档指引（两者都是模型面文案，缺一个都说明装配没接上）。
	prod := descOf(mcpSchemas(t, eng, "current"))
	for _, want := range []string{"Connected MCP servers", codemode.DEFERRED_TOOLS_GUIDANCE} {
		if !strings.Contains(prod, want) {
			t.Fatalf("线上描述缺 %q:\n%s", want, prod)
		}
	}
	// guidance 臂只换指引段：其余字节（含刚加的能力提示句）必须原样保留。
	g := descOf(mcpSchemas(t, eng, "guidance"))
	if !strings.Contains(g, "The servers listed below are already connected") ||
		!strings.Contains(g, "do not ask the user first") ||
		!strings.Contains(g, "Connected MCP servers") {
		t.Errorf("guidance 臂文案不对:\n%s", g)
	}
	// 增量式改动：原指引逐字保留（一次），不是替换/重复。
	if n := strings.Count(g, codemode.DEFERRED_TOOLS_GUIDANCE); n != 1 {
		t.Errorf("原指引段出现 %d 次，want 1（增量文本必须是「追加」，不能重复或替换）", n)
	}

	// ②③ 真执行 + 计数语义。
	run := func(code string) (string, int64) {
		t.Helper()
		for _, st := range stubs {
			st.calls.Store(0)
		}
		raw, err := json.Marshal(map[string]string{"code": code})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		res, err := evalExecCodemode(context.Background(), eng, string(raw))
		if err != nil {
			t.Fatalf("执行脚本失败: %v", err)
		}
		var n int64
		for _, st := range stubs {
			n += st.calls.Load()
		}
		return res, n
	}
	const envTool = "mcp__ai_test__list_environments_7e382c"
	res, n := run(`const r = await tools["` + envTool + `"]({project:"agent-server"}); return r;`)
	if n != 1 {
		t.Errorf("直接调 MCP 替身：计数 = %d, want 1（计数错则 invoke 指标全错）", n)
	}
	if !strings.Contains(res, "env-prod-eu") {
		t.Errorf("脚本没拿到 canned 结果（真执行没接通）:\n%s", res)
	}
	res, n = run(`const r = await searchTools("environments"); return r;`)
	if n != 0 {
		t.Errorf("只搜不调：计数 = %d, want 0（searchTools 不是 MCP 调用）", n)
	}
	if !strings.Contains(res, envTool) {
		t.Errorf("searchTools 没返回替身工具（搜索索引没接上）:\n%s", res)
	}

	// ④ 阶段真值表（纯函数）。
	code := &core.ToolCall{Name: codemode.ToolName}
	bash := &core.ToolCall{Name: "bash"}
	for _, c := range []struct {
		name   string
		first  *core.ToolCall
		calls  int64
		search bool
		err    error
		want   string
	}{
		{"纯文本", nil, 0, false, nil, "text-only"},
		{"选了 bash", bash, 0, false, nil, "direct:bash"},
		{"脚本没碰 MCP", code, 0, false, nil, "codemode:no-mcp"},
		{"只搜不调", code, 0, true, nil, "codemode:search-only"},
		{"搜到并调了", code, 2, true, nil, "codemode:invoke"},
		{"执行失败", code, 0, true, errors.New("boom"), "exec-error"},
	} {
		got := classifyMCPStage(c.first, c.calls, c.search, c.err)
		if got != c.want {
			t.Errorf("%s：阶段 = %q, want %q", c.name, got, c.want)
		}
	}
	// 限流/网络错误必须单列（否则「没调工具」与「400」混在一起，成功率失真）。
	for _, c := range []struct {
		name  string
		first *core.ToolCall
		calls int64
	}{{"首轮就 400", nil, 0}, {"第二轮 400", code, 1}} {
		if got := finalMCPStage(c.first, c.calls, true, nil, true); got != "model-error" {
			t.Errorf("%s：阶段 = %q, want model-error（必须与 text-only 分开）", c.name, got)
		}
	}
	if got := finalMCPStage(code, 1, true, nil, false); got != "codemode:invoke" {
		t.Errorf("无错误时阶段被污染：%q", got)
	}
}
