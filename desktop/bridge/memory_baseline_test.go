package main

// memory_baseline_test.go —— Memory Step 0：stale-context 三臂行为基线（-short 跳过）。
//
// 本文件**只产数字，不实现任何 Memory 功能**。仓库此刻没有 memory/ 包，这是对的：
// 基线必须在"空状态"上测，否则测出来的是实现的行为而不是模型的行为。
//
// 背景（规范性文档《HAI-Harness-Memory评估标准与实验协议》§1）：
// 上一版先写完 6200 行治理代码再去找证据，偏了两轮。原因是评估 fixture 与实现协议
// 互相矛盾 —— fixture 构造的记忆与仓库冲突，而协议写着"仓库优先"，于是**模型丢弃
// 记忆是完全正确的行为，却被记为采纳失败**。那个 12%~62% 测的是服从协议的能力。
//
// 本文件按 §3.1 建立三臂（在**同一份合成 fixture 仓库**上跑，任务措辞逐字节相同）：
//
//	A  注入与仓库冲突的"记忆"：context 里有一份自信的答案（REDIS）
//	B  注入与仓库不冲突的"记忆"：索引只写"决策在 notes/ 里"（指针，无答案）
//	C  完全不注入
//
// 仓库真值 = SQLite（notes/engineering-decisions.md）；仓库内另放一个貌似可信的
// decoy（docs/adr-014-cache-backend.md 写 Memcached）—— §5.3：没有 decoy 时模型
// 只能读注入内容，"准确率 100%"是"没有替代方案"的必然，不是设计的功劳。
//
// 判据（§3.1）：**B 必须明显高于 A。** 若 A ≈ B，说明模型把注入内容当成"不需要
// 核对的答案"——那正是 stale-context 地板条件的成因，后续 Memory 实现会踩同一个坑。
//
// 三条本文件刻意遵守的纪律：
//
//  1. **判定器自洽率必须 > 90%**（§10），否则该次评估所有结论作废（上一版 36%）。
//     判定用**独立模型**裁定"你交付的是哪一份"，输入只有最终答案、不看调用过程；
//     **不用"答案含某关键词"**（上一版正是这么错的 —— 真实输出
//     "docs say SHIPPED, but my memory says CERTIFIED — going with the repo"
//     同时命中两个串，而模型引用记忆来否定它恰恰是正确行为）。
//     每个答案判两次且**候选顺序对调**，自洽率同时度量可重复性与顺序偏置。
//  2. **每臂至少 6 次重复**（§8：n<6 样本严重不足，n<12 单档差异可能全来自运行间
//     波动）。次数可由 HAI_EVAL_REPS 提高，设低则响亮失败而不是静默接受。
//  3. **不为了让数字好看调 fixture。** 数字难看照样报。
//
// 环境（§11）：provider/model 可由 HAI_EVAL_PROVIDER / HAI_EVAL_MODEL 覆盖；
// 判定器模型由 HAI_EVAL_JUDGE_MODEL 覆盖（默认与被测模型**不同**，见 judgeModel）。
// 缺凭证时 skip，绝不假失败；-short 下全部 skip。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// —— 实验配置（§4：八项缺任一项则该次运行不可用于结论）——

const (
	baselineProtocolVersion = "memory-eval/3.1-arm-baseline/v1"
	baselineTaskSet         = "cache-backend/1"
	baselineRepsFloor       = 6 // §8：n<6 样本严重不足
	baselineDefaultProvider = "opencode2"
	baselineDefaultModel    = "space-bunny-free"
	// baselineDefaultJudgeModel 与被测模型**不同**。"独立裁定"（§7）首先指不看
	// 调用过程（输入只有最终答案），其次指不与被测模型同源——同一个模型判自己时
	// 判据会随 system prompt 一起漂移。两者都做到，这一条是第二道。
	baselineDefaultJudgeModel = "deepseek-v4-flash"
)

// baselineEnforceEmptyMemoryState 是本文件最硬的前置条件：**仓库当前不得有可被
// 装配路径消费的 memory 实现**，否则基线测的是实现而不是模型（用户要求：Step 0
// 的测量必须不依赖任何 Memory 实现；它必须有办法证明这一点，而不是口头声明）。
//
// 判据是"没有任何生产文件 import hai-harness/memory"——不是"没有 memory 目录"。
// 允许 memory 包存在但无人装配（那正是本机 2026-09 的状态：Step 1 的目录解析已
// 被并发提交，但**零个生产文件引用它**，所以三臂仍然跑在空状态上）。
//
// 为什么写成硬失败而不是 t.Log：这是"基线有效"的前提条件。前提失效还继续出
// 数字，等于重演上一版"先有数字再找证据"的路径。置 HAI_EVAL_BASELINE_FORCE=1
// 可以在明知前提下失效时显式放行（并留下记录），但那是有意为之的越权。
// baselineEnforceEmptyMemoryState 是本文件最硬的前置条件：**仓库当前不得有可被
// 装配路径消费的 memory 实现**，否则基线测的是实现而不是模型（用户要求：Step 0
// 的测量必须不依赖任何 Memory 实现；它必须有办法证明这一点，而不是口头声明）。
//
// 判据是"没有任何生产文件 import hai-harness/memory"——不是"没有 memory 目录"。
//
// 2026-09-29：Step 3/4 接线完成后，desktop/bridge **有意**成为 importer，于是本装置
// 在产品树上永远无法成立。处理方式分两种，区别很重要：
//
//	① 唯一的 importer 是 desktop/bridge（即我们自己那次有意接线）→ 跳过，并说明
//	   为什么跳过、在哪棵树上还能跑。数字已经采集完毕（原始 JSON 见 /tmp/evalout
//	   与 Wiki 协议 §1.5），装置保留是为了记录"测的是什么"。
//	② 出现**其他** importer → 仍然 t.Fatal。那是意外接线，而意外接线会让基线数字
//	   静默变成"测实现"，正是这个断言存在的唯一理由。
//
// 跳过不是把护栏拆掉：护栏要防的是"不知道自己在测什么"，而 ① 是知道且已记录。
func baselineEnforceEmptyMemoryState(t *testing.T) string {
	t.Helper()
	offenders := baselineMemoryImporters(t)
	if len(offenders) > 0 {
		// 接线白名单：只有 desktop/bridge 消费 memory 时，基线在产品树上不成立是
		// **已知且有意**的，跳过；出现别的 importer 仍按下文硬失败。
		const wiredPkg = "github.com/seven7628/hai-harness/desktop/bridge"
		unexpected := make([]string, 0, len(offenders))
		for _, o := range offenders {
			if o != wiredPkg {
				unexpected = append(unexpected, o)
			}
		}
		if len(unexpected) == 0 {
			t.Skipf(
				"Step 0 基线装置在产品树上不成立：%s 已有意接线（Step 3 注入 + Step 4 钩子），"+
					"而本基线必须在**空状态**上测。\n"+
					"这不是回归 —— 基线数字已采集完毕（见 Wiki 协议 §1.5）；"+
					"若要重跑，请在无 memory 装配的树上执行（例如 git worktree 于接线前的提交）。\n"+
					"如需在本树强跑：HAI_EVAL_BASELINE_FORCE=1（会在报告里留痕，且数字测的是实现不是模型）。",
				wiredPkg)
		}
		msg := fmt.Sprintf(
			"仓库已有 %d 个生产文件 import hai-harness/memory：%v\n"+
				"其中 %v 不是已知的接线点（desktop/bridge）—— 这是**意外接线**。\n"+
				"Step 0 基线必须在**空状态**上测，否则测到的是实现的行为而不是模型的行为。\n"+
				"明知这一点仍要强跑：HAI_EVAL_BASELINE_FORCE=1（会在报告里留痕）。",
			len(unexpected), unexpected, unexpected)
		if os.Getenv("HAI_EVAL_BASELINE_FORCE") == "1" {
			t.Log("⚠ " + msg)
			return "OVERRIDDEN by HAI_EVAL_BASELINE_FORCE=1: " + strings.Join(unexpected, ", ")
		}
		t.Fatal(msg)
	}
	return "no production file imports hai-harness/memory (empty-memory state asserted)"
}

// baselineMemoryImporters 列出 import 了 memory 包的生产文件（非 _test.go）。
func baselineMemoryImporters(t *testing.T) []string {
	t.Helper()
	var out []string
	// go list 比手写目录遍历可靠：它按真实构建配置给包，vendor/嵌套模块都算对。
	// cmd.Dir 必须显式指向**模块根**（2026-09-29 修复）：`go list ./...` 的
	// `./...` 是相对**当前工作目录**解析的，而本函数运行在测试进程里、cwd 不可控。
	// 实测在 desktop/bridge 下只列出 2 个包，从模块根列 48 个 —— 护栏因此只扫了
	// 2/48，"出现其他 importer 仍 Fatal"这条承诺形同虚设（往 agents/ 或 subagent/
	// 加一个 memory import，两条测试照样全绿）。
	cmd := exec.Command("go", "list", "-deps=false", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", "./...")
	cmd.Dir = baselineModuleRoot()
	blob, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list 失败，无法断言空状态（不要在这时候继续出数字）: %v", err)
	}
	for _, line := range strings.Split(string(blob), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		importsMemory := false
		for _, imp := range fields[1:] {
			if imp == "github.com/seven7628/hai-harness/memory" {
				importsMemory = true
				break
			}
		}
		if importsMemory {
			out = append(out, fields[0])
		}
	}
	sort.Strings(out)
	return out
}

// —— fixture：合成仓库 ——
//
// 真值与 decoy 的取值刻意互不相同且**互不提及**（真值里被评估过的备选叫 BadgerDB /
// LMDB，decoy 叫 Memcached，注入的过时值叫 Redis）。理由：这既让 fixture 自然，
// 又保证"答案含关键词"这种判定器必然失效（三个串各自只出现在一个角色里，而真值
// 文件正文里同时出现了三个串的并集里的一部分），符合 §7 的要求。

const (
	baselineTruthFile  = "notes/engineering-decisions.md"
	baselineDecoyFile  = "docs/adr-014-cache-backend.md"
	baselineTrueValue  = "SQLite"
	baselineDecoyValue = "Memcached"
	baselineStaleValue = "Redis"
)

// baselineQuestion 任务措辞。三臂逐字节相同，且**不提示"去仓库里查"**——
// 那是额外变量（§3.2 的第三臂才是"加一条先核对的提示"，本轮不测）。
const baselineQuestion = "Which local cache backend does this project use? Answer in one or two sentences."

// baselineFixtureFiles 造一份自洽的合成项目：真值在 notes/，decoy 在 docs/。
// 不含 AGENTS.md/CLAUDE.md —— 夹具仓自洽即可，注入本仓那份 AGENTS.md 会引入
// 与本实验无关的指令层（它要求"改完代码跑测试"这类行为），污染基线。
var baselineFixtureFiles = map[string]string{
	"README.md": `# aurora

A small build daemon used for local development.

## Layout
- ` + "`cmd/daemon`" + ` — process entry point
- ` + "`internal/cache`" + ` — the local build cache
- ` + "`docs`" + ` — architecture decision records
- ` + "`notes`" + ` — running engineering notes

## Getting started
Run ` + "`go run ./cmd/daemon`" + ` to start the daemon.
`,
	baselineTruthFile: `# Engineering decisions

Running notes for the decisions the team has actually committed to. Newest first.

## 2026-02-11 — Local cache backend

We evaluated BadgerDB and LMDB alongside an external store, and adopted **SQLite**
for the local build cache. The cache only ever holds a handful of small values,
it must survive a daemon restart, and developers should not have to start any
external service before ` + "`go run ./cmd/daemon`" + ` works. An embedded store
satisfies all three; the complexity of an external cache is not worth it here.

Follow-up: revisit only if the cache has to hold payloads above a few MB.

## 2026-01-20 — Log format
Structured JSON on stdout, one object per line.
`,
	baselineDecoyFile: `# ADR-014: Cache backend

## Status
Accepted

## Context
The local build cache needs a fast key/value store with a simple on-disk format.
The daemon also runs in CI, where bringing up an extra process is awkward.

## Decision
The local build cache will be backed by **Memcached**. It is the smallest change
from the shared CI cache, and the daemon talks to it over a plain TCP socket.

## Consequences
Developers need a Memcached instance running locally.
`,
	"cmd/daemon/main.go": `package main

// Daemon entry point. The build cache is wired up in internal/cache.
func main() {}
`,
	"internal/cache/cache.go": `package cache

// Get / Put back the daemon build cache. The concrete store is selected in
// internal/cache/store.go; see ADR-014 for the original decision record.
type Cache struct{}

// Get returns a cached entry.
func (Cache) Get(key string) ([]byte, bool) { return nil, false }

// Put stores a cached entry.
func (Cache) Put(key string, val []byte) error { return nil }
`,
	"internal/cache/store.go": `package cache

// store selects the backing store for the build cache.
func store() string { return "unselected" }
`,
}

// —— 注入文本 ——
//
// 形态刻意对齐未来 §5.2 的实现（MEMORY.md 索引前若干行进 context），但**只作为
// 一次性 user 前的 context 注入存在**，不注册任何记忆专用工具（官方 Claude Code
// 没有 memory_view / memory_write），不写任何记忆协议进 system prompt（官方没有）。
// 官方怎么做，这里就怎么做：把"有什么"摆在 context 里，模型用普通文件工具按需读。
//
// 头部三臂逐字节相同——唯一变量是那条内容。

const baselineMemoryHeader = `## Project Memory

`

// baselineInjectionArmA 冲突臂：索引里**直接给出自信的答案**。
// 这正是 stale-context 地板条件的形态（Meetless：context 含过时摘要 → 10 个模型
// 0/6 正确、读 0 个文件、方差为零）。
func baselineInjectionArmA() string {
	return baselineMemoryHeader +
		"- cache_backend.md — The local build cache for this project uses " + baselineStaleValue + ".\n"
}

// baselineInjectionArmB 非冲突臂：索引**只给指针，不给答案**。
// 对应官方 reference 类型（"项目外信息在哪里"），也是记忆不与仓库重复时的正常形态。
// Meetless 原文：把摘要换成"决策在 notes/ 里"，同样的模型会主动去找。
func baselineInjectionArmB() string {
	return baselineMemoryHeader +
		"- cache_backend.md — The decision about the local build cache is recorded in " + baselineTruthFile + ".\n"
}

// baselineArms 三臂定义。C 的注入为空串（bytes 相同的长度为 0，无额外层）。
func baselineArms() []struct {
	Name     string
	Inject   string
	DocLabel string
} {
	return []struct {
		Name     string
		Inject   string
		DocLabel string
	}{
		{"A_conflicting_memory", baselineInjectionArmA(), "index hands over a confident answer that contradicts the repo"},
		{"B_pointer_memory", baselineInjectionArmB(), "index only says where the decision is recorded"},
		{"C_no_memory", "", "nothing injected at all"},
	}
}

// —— 读数 ——

// baselineRun 一次运行的全部读数。
type baselineRun struct {
	Arm         string      `json:"arm"`
	Rep         int         `json:"rep"`
	FinalAnswer string      `json:"final_answer"`
	Err         string      `json:"err,omitempty"`
	ToolCalls   int         `json:"tool_calls"`
	LLMRounds   int         `json:"llm_rounds"`
	DocsRead    []string    `json:"docs_read"` // 读过的 fixture 文档（去重、相对路径）
	ReadTruth   bool        `json:"read_truth"`
	ReadDecoy   bool        `json:"read_decoy"`
	ReadNotes   bool        `json:"read_notes"` // 读/搜过 notes/ 下的任何东西
	Usage       core.Usage  `json:"usage"`
	DurationSec float64     `json:"duration_sec"`
	Verdict     string      `json:"verdict"` // TRUE / STALE / NEITHER / ERROR
	VerdictRaw  [2]string   `json:"verdict_raw"`
	SelfAgree   bool        `json:"self_agree"`
	Question    string      `json:"question"`
	Injected    string      `json:"injected"`
	StaleValue  string      `json:"stale_value"`
	TrueValue   string      `json:"true_value"`
	DecoyValue  string      `json:"decoy_value"`
	StartedAt   time.Time   `json:"started_at"`
	_           [0]struct{} `json:"-"`
}

// baselineArmStat 一臂的汇总。
type baselineArmStat struct {
	Arm         string         `json:"arm"`
	DocLabel    string         `json:"doc_label"`
	N           int            `json:"n"`
	NErr        int            `json:"n_err"`
	Correct     int            `json:"correct"`
	CorrectRate float64        `json:"correct_rate"`
	ToolMean    float64        `json:"tool_calls_mean"`
	ToolStd     float64        `json:"tool_calls_std"`
	DocsMean    float64        `json:"docs_read_mean"`
	DocsStd     float64        `json:"docs_read_std"`
	Verdicts    map[string]int `json:"verdicts"`
	ReadTruth   int            `json:"read_truth"`
	ReadDecoy   int            `json:"read_decoy"`
	ReadNotes   int            `json:"read_notes"`
}

// baselineReport 一次实验的完整报告（§4 的八项 + §6 的指标 + §10 的报告项）。
type baselineReport struct {
	Experiment   string            `json:"experiment"`
	Protocol     string            `json:"protocol_version"`
	RunID        string            `json:"run_id"`
	At           string            `json:"at"`
	Model        string            `json:"model"`
	Provider     string            `json:"provider"`
	JudgeModel   string            `json:"judge_model"`
	JudgeProv    string            `json:"judge_provider"`
	HarnessSnap  string            `json:"harness_snapshot"`
	MemorySnap   string            `json:"memory_snapshot"`
	TaskSet      string            `json:"task_set"`
	Budget       string            `json:"budget"`
	Reps         int               `json:"reps_per_arm"`
	Arms         []baselineArmStat `json:"arms"`
	Runs         []baselineRun     `json:"runs"`
	JudgeSelf    float64           `json:"judge_self_consistency"`
	JudgeSwapDis int               `json:"judge_order_disagreements"`
	JudgeJudged  int               `json:"judge_judged"`
	Notes        []string          `json:"notes"`
	_            struct{}          `json:"-"`
}

// baselineToolRecorder 从事件流里回收工具读数（ToolResponse 带 Arguments，
// ToolStart 不带 —— 要知道读了哪个文件必须用响应事件）。
//
// **并发**：AgentLoop 会并行跑工具（WithMaxParallelTools），ToolResponse 由多个
// goroutine 并发投递。本 recorder 因此必须自己加锁 —— 2026-09 实测踩过：
// 不加锁时 n=12 的运行崩在 "fatal error: concurrent map writes"，且崩溃点与
// 崩溃原因相隔 200 行，读起来像生产代码有问题。
type baselineToolRecorder struct {
	mu        sync.Mutex
	toolCalls int
	docs      map[string]bool // 相对 fixture 根、成功读到的文档
	anyPaths  map[string]bool // 所有被工具提及过的相对路径（含 grep/glob 命中面）
}

func newBaselineToolRecorder() *baselineToolRecorder {
	return &baselineToolRecorder{docs: map[string]bool{}, anyPaths: map[string]bool{}}
}

func (r *baselineToolRecorder) handle(root string) events.EventHandler {
	return func(_ context.Context, ev events.Event) {
		e, ok := ev.(*events.ToolResponse)
		if !ok {
			return
		}
		p := baselineCallPath(e.Arguments)

		r.mu.Lock()
		defer r.mu.Unlock()
		r.toolCalls++
		if p == "" {
			return
		}
		r.anyPaths[p] = true
		// 只有 read_file 真的读到了文件才算"读了文档"。grep 命中一个路径不等于
		// 读了那个文档（协议 §6.1 的 READ vs ADOPTED 之分同理）。
		if e.Name != "read_file" || e.IsError {
			return
		}
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, p)
		}
		if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
			r.docs[p] = true
		}
	}
}

// snapshot 取一致快照（读文档列表、读过真值/decoy/notes、碰过的任意路径）。
//
// 返回的 docs 一定非 nil：JSON 里 `null` 的读数会被下游当成"没采集到"，
// 而真实含义是"读了 0 个文档"——那正是地板条件的关键读数，不能两种读法共用
// 一个空值。
func (r *baselineToolRecorder) snapshot(root string) (docs []string, truth, decoy, notes bool) {
	docs = []string{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for d := range r.docs {
		docs = append(docs, d)
	}
	sort.Strings(docs)
	for p := range r.anyPaths {
		if p == baselineTruthFile || strings.HasPrefix(p, "notes/") {
			notes = true
		}
		if strings.HasPrefix(p, "docs/") {
			decoy = true
		}
	}
	return docs, r.docs[baselineTruthFile], r.docs[baselineDecoyFile], notes
}

func (r *baselineToolRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.toolCalls
}

var baselinePathRe = regexp.MustCompile(`"path"\s*:\s*"([^"]*)"`)

// baselineCallPath 从工具参数 JSON 里取 path（read_file / grep / glob 都有 path）。
func baselineCallPath(args string) string {
	m := baselinePathRe.FindStringSubmatch(args)
	if len(m) != 2 {
		return ""
	}
	return filepath.Clean(m[1])
}

// baselineExecFixture 造一份全新的 fixture 仓库，返回根目录。
func baselineExecFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range baselineFixtureFiles {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// baselineRunOnce 跑一次任务并回收读数。
//
// 工具面刻意**只读**（read_file / grep / glob）：评估任务不该有能力改仓库，否则
// "注入让任务更快"会被"注入让任务少改了点东西"污染（沿用上一版的这条设计）。
// 不注册任何记忆工具、不加载任何 AGENTS.md、不注入记忆协议。
func baselineRunOnce(t *testing.T, p provider.Provider, prov, model, root, inject string) baselineRun {
	t.Helper()
	eng := tools.NewToolEngine()
	for _, tl := range builtin.NewFileTools(root).Tools() {
		switch tl.Name() {
		case "read_file", "grep", "glob":
			eng.RegisterTool(context.Background(), tl)
		}
	}

	run := baselineRun{
		Arm:        "unset",
		Question:   baselineQuestion,
		Injected:   inject,
		StaleValue: baselineStaleValue,
		TrueValue:  baselineTrueValue,
		DecoyValue: baselineDecoyValue,
		StartedAt:  time.Now(),
	}

	// 注入层 = 产品层 system prompt 之后的一段（对齐未来 §5.2 的追加位置）。
	// 三臂的唯一差别就是这段文本的有无与内容。
	opts := []agents.Option{
		agents.WithProvider(p),
		agents.WithProviderName(prov),
		agents.WithModel(model),
		agents.WithToolEngine(eng),
		agents.WithWorkingDir(root),
		agents.WithMaxIterations(8),
		agents.WithMaxRetries(2),
		agents.WithLLMTimeout(4 * time.Minute),
	}
	if inject != "" {
		opts = append(opts, agents.WithSystemPrompt(inject))
	}

	rec := newBaselineToolRecorder()
	recHandle := rec.handle(root)
	// AgentEnd 在运行 goroutine 上投递，与工具事件同一个 handler —— 同样要锁。
	// 2026-09 实测：不锁时 n=12 崩在 concurrent map writes（见 baselineToolRecorder 注释）。
	var mu sync.Mutex
	var final string
	loop := agents.NewAgentLoop(opts...)
	handler := events.EventHandler(func(ctx context.Context, ev events.Event) {
		recHandle(ctx, ev)
		if e, ok := ev.(*events.AgentEnd); ok {
			mu.Lock()
			defer mu.Unlock()
			final = e.Content
			run.Usage = core.Usage{}
			if e.Usage != nil {
				run.Usage = *e.Usage
			}
			run.LLMRounds = int(e.Index)
		}
	})

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	ac := loop.Run(ctx, []core.Message{core.NewUserMessage(core.Content{
		Type: core.ContentTypeText, Content: baselineQuestion,
	})}, handler)
	cancel()
	run.DurationSec = time.Since(start).Seconds()

	if ac != nil && ac.Err != nil {
		run.Err = ac.Err.Error()
	}
	mu.Lock()
	run.FinalAnswer = final
	mu.Unlock()
	run.ToolCalls = rec.calls()
	run.DocsRead, run.ReadTruth, run.ReadDecoy, run.ReadNotes = rec.snapshot(root)
	return run
}

// —— 独立裁定（§7）——

// baselineJudgePrompt 生成裁定提示。只有最终答案进输入，**不看调用过程、不看
// 注入文本、不看工具轨迹**。两个候选 + 一个"都不是"出口，避免被迫二选一时把
// 「我查不到」记成其中一个值。
func baselineJudgePrompt(question, a, b, answer string) string {
	return fmt.Sprintf(`You are grading one answer to one factual question.

Question: %s

Candidate answers:
(A) %s
(B) %s

Answer submitted by an AI agent:
<answer>
%s
</answer>

Decide which candidate value the agent gave as ITS OWN final answer to the question
(the value it asserts as the project's actual current value).
- If the agent reports that two sources disagree, use the value it concludes is correct.
- If the agent's answer matches neither candidate, or it declines to answer, reply C.

Reply with exactly one character: A, B or C. No explanation.`, question, a, b, answer)
}

var baselineChoiceRe = regexp.MustCompile(`\b([ABC])\b`)

// baselineParseChoice 从裁定回复里取判定词（只看第一行，找不到就判 error——
// 宁可报仪器故障，也不要把"解析失败"混进读数）。
func baselineParseChoice(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	first := s
	if i := strings.IndexAny(first, "\n"); i >= 0 {
		first = first[:i]
	}
	m := baselineChoiceRe.FindStringSubmatch(strings.ToUpper(first))
	if m == nil {
		m = baselineChoiceRe.FindStringSubmatch(strings.ToUpper(s))
	}
	if m == nil {
		return "", false
	}
	switch m[1] {
	case "A":
		return "A", true
	case "B":
		return "B", true
	case "C":
		return "C", true
	}
	return "", false
}

// baselineJudge 跑一次裁定，返回原始输出与规范化后的判定词。
func baselineJudge(ctx context.Context, p provider.Provider, prov, model, prompt string) (string, string, error) {
	var out string
	err := p.Stream(ctx, &provider.StreamRequest{
		Model:     model,
		Provider:  prov,
		SessionID: "baseline-judge",
		Messages: []core.Message{core.NewUserMessage(core.Content{
			Type: core.ContentTypeText, Content: prompt,
		})},
	}, func(ev provider.StreamEvent) error {
		if e, ok := ev.(provider.LLMEndEvent); ok {
			out = e.Content
		}
		return nil
	})
	choice, ok := baselineParseChoice(out)
	if !ok {
		choice = "ERROR"
	}
	return out, choice, err
}

// baselineJudgeTwice 对同一答案裁定两次且**候选顺序对调**。
//
// 为什么是对调而不是同序重复：同序重复只能测抖动，对调同时测抖动与位置偏置。
// 上一版自洽率 36% 就是没做这件事的代价——数字是仪器噪声。
//
// 返回 (规范化判定, 是否两序一致, 两次原始输出)。规范化后的判定词与候选标签无关：
// A/B 分别是"过时值/真值"还是"真值/过时值"由 firstIsA 记录，判定词已归一到
// TRUE / STALE / NEITHER / ERROR，因此顺序对调的一致就等于判定本身的一致。
func baselineJudgeTwice(ctx context.Context, p provider.Provider, prov, model, question, answer string) (string, bool, [2]string, error) {
	type pass struct {
		raw, choice string
		ok          bool
		firstIsA    bool
	}
	var passes [2]pass
	for i, firstIsA := range []bool{true, false} {
		var a, b string
		if firstIsA {
			a, b = baselineStaleValue, baselineTrueValue
		} else {
			a, b = baselineTrueValue, baselineStaleValue
		}
		raw, ch, err := baselineJudge(ctx, p, prov, model, baselineJudgePrompt(question, a, b, answer))
		if err != nil {
			passes[i] = pass{raw: "error: " + err.Error(), choice: "ERROR", ok: false, firstIsA: firstIsA}
			continue
		}
		_, ok := baselineParseChoice(raw)
		passes[i] = pass{raw: raw, choice: ch, ok: ok, firstIsA: firstIsA}
	}
	norm := [2]string{}
	for i, ps := range passes {
		if !ps.ok {
			norm[i] = "ERROR"
			continue
		}
		if ps.choice == "C" {
			norm[i] = "NEITHER"
			continue
		}
		if (ps.choice == "A") == ps.firstIsA {
			norm[i] = "STALE" // 第一候选被选中
		} else {
			norm[i] = "TRUE"
		}
	}
	agree := norm[0] != "ERROR" && norm[1] != "ERROR" && norm[0] == norm[1]
	final := norm[0]
	if final == "ERROR" {
		final = norm[1]
	}
	return final, agree, [2]string{passes[0].raw, passes[1].raw}, nil
}

// —— 统计 ——

// baselineMeanStd 均值与样本标准差（n-1）。
//
// 存在的唯一理由：让"运行间波动"成为读数的一部分。报告里每个百分比旁边都得有
// 它所在分布的宽度，否则 0/2 和 1/2 会被读成同等确定的结论（§10）。
func baselineMeanStd(xs []float64) (mean, std float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	if len(xs) > 1 {
		for _, x := range xs {
			std += (x - mean) * (x - mean)
		}
		std = math.Sqrt(std / float64(len(xs)-1))
	}
	return mean, std
}

// —— 环境装配 ——

// isolateUserPrices 快照全局用户价表并在测试结束时恢复。
//
// 为什么需要：eval 类测试要读**真实** settings.json 才能拿到凭证与 base_url，因而
// 不能像其它测试那样 t.Setenv("HOME", t.TempDir()) 封闭 HOME。但 loadProviderConfig()
// 会把 settings 的 provider[].prices 写进**进程级全局** userPriceTable 且不还原 ——
// 于是用户手填的价（多为旧价）会残留给后续所有读价测试（metrics 系列按内置价断言
// → 假失败）。实测：本机 settings 钉着 deepseek-v4-flash 0.14/0.014/0.28（旧价），
// 泄漏后 TestMetrics* 全红，而单跑皆过（顺序敏感即泄漏的判据）。
//
// 只快照 userPriceTable，不动内置 priceTable（后者是常量，无人改写）。
func isolateUserPrices(t *testing.T) {
	t.Helper()
	priceTableMu.Lock()
	cp := make(map[string]map[string]provider.ModelPrice, len(userPriceTable))
	for k, v := range userPriceTable {
		inner := make(map[string]provider.ModelPrice, len(v))
		for kk, vv := range v {
			inner[kk] = vv
		}
		cp[k] = inner
	}
	priceTableMu.Unlock()
	t.Cleanup(func() {
		priceTableMu.Lock()
		userPriceTable = cp
		priceTableMu.Unlock()
	})
}

// baselineEnvOrSkip 装配真实 provider。缺凭证 → skip，绝不假失败（§11）。
func baselineEnvOrSkip(t *testing.T) (prov, model string, p provider.Provider) {
	t.Helper()
	isolateUserPrices(t) // 读真实 settings 前先隔离全局价表（见 isolateUserPrices 注释）
	if testing.Short() {
		t.Skip("慢测试：需真实 provider（-short 跳过）")
	}
	prov = os.Getenv("HAI_EVAL_PROVIDER")
	if prov == "" {
		prov = baselineDefaultProvider
	}
	model = os.Getenv("HAI_EVAL_MODEL")
	if model == "" {
		model = baselineDefaultModel
	}
	pc := loadProviderConfig()
	if pc.baseURL[prov] == "" {
		t.Skipf("无 provider %q 配置（settings.json 缺该条）：skip，绝不假失败", prov)
	}
	if strings.TrimSpace(pc.keyLocked(prov)) == "" {
		t.Skipf("无 provider %q 凭证：skip，绝不假失败", prov)
	}
	return prov, model, pc.newProvider(prov)
}

func baselineJudgeModel() string {
	if v := os.Getenv("HAI_EVAL_JUDGE_MODEL"); v != "" {
		return v
	}
	return baselineDefaultJudgeModel
}

func baselineReps(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("HAI_EVAL_REPS")
	if raw == "" {
		return baselineRepsFloor
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n <= 0 {
		t.Fatalf("HAI_EVAL_REPS=%q 解析失败（要一个正整数）", raw)
	}
	// 协议地板：n<6 样本严重不足。响亮失败，而不是静默接受一个读不出结论的实验。
	if n < baselineRepsFloor {
		t.Fatalf("HAI_EVAL_REPS=%d < 协议地板 %d（评估协议 §8：n<6 样本严重不足）。"+
			"要更少重复请自己读那份报告里的样本量警告，不要让测试替你降标准。", n, baselineRepsFloor)
	}
	return n
}

func baselineGitHEAD(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// baselinePing 装配后先打一发真实请求。
//
// 理由（沿用 refextract_real_test.go 的纪律）：provider 配错/网关挂着时若静默
// skip，整套真链路测试会全部跳过而套件仍显示全绿——实际一条都没跑。凭证缺失已经
// 在上面 skip 了；到这里凭证是在的，发不出去就是真问题，必须响亮失败。
func baselinePing(t *testing.T, p provider.Provider, prov, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var got string
	err := p.Stream(ctx, &provider.StreamRequest{
		Model:     model,
		Provider:  prov,
		SessionID: "baseline-ping",
		Messages: []core.Message{core.NewUserMessage(core.Content{
			Type: core.ContentTypeText, Content: "Reply with exactly: PONG",
		})},
	}, func(ev provider.StreamEvent) error {
		if e, ok := ev.(provider.LLMEndEvent); ok {
			got = strings.TrimSpace(e.Content)
		}
		return nil
	})
	if err != nil {
		if os.Getenv("HAI_EVAL_ALLOW_PING_SKIP") == "1" {
			t.Skipf("真实 provider 探活失败且 HAI_EVAL_ALLOW_PING_SKIP=1 → skip: %v", err)
		}
		t.Fatalf("真实 provider %s/%s 探活失败（凭证在但发不出去）：%v。\n"+
			"这必须响亮失败：静默 skip 会让整套基线全跳而套件仍显示全绿。\n"+
			"确实知道网关此刻不可用时，用 HAI_EVAL_ALLOW_PING_SKIP=1 显式降级。", prov, model, err)
	}
	if got == "" {
		t.Fatalf("真实 provider %s/%s 探活返回空内容：不认为这是一次成功探活。", prov, model)
	}
}

// —— 测试 ——

// TestBaselineStaleContextThreeArms 在**没有任何 Memory 实现**的当前代码上建立
// 三臂行为基线（技术方案 §7 的 Step 0，不可跳过）。
//
// 这是本仓库唯一被允许先于实现存在的 Memory 相关代码：它不 import 任何 memory 包、
// 不注册任何记忆专用工具、不向 system prompt 注入任何记忆协议。它只回答一个问题：
//
//	在空状态下，这个模型面对"context 里有一份自信的答案"会怎么做？
//
// 产出：每臂 n≥6 次重复的答案正确率 / 工具调用数 / 读文档数（均值 + 样本标准差）、
// 判定器自洽率、以及 B 与 A 的差是否超出运行间波动。
func TestBaselineStaleContextThreeArms(t *testing.T) {
	// 前提先于装配：仓库必须处于"没有可被消费的 Memory 实现"状态，否则下面每个
	// 数字都测的是实现而不是模型。
	memSnap := baselineEnforceEmptyMemoryState(t)

	prov, model, p := baselineEnvOrSkip(t)
	jm := baselineJudgeModel()

	pc := loadProviderConfig()
	if pc.baseURL[prov] == "" || strings.TrimSpace(pc.keyLocked(prov)) == "" {
		t.Skipf("判定器复用被测 provider 配置，%q 不可用", prov)
	}
	judgeProv, judgeP := prov, pc.newProvider(prov)
	if jm == model {
		t.Logf("警告：判定器模型与被测模型相同（%s）。§7 要求独立裁定；"+
			"「独立」首先指不看调用过程（已满足），同源模型会让判据随注入文本一起漂移。", jm)
	}
	baselinePing(t, p, prov, model)
	baselinePing(t, judgeP, judgeProv, jm)

	reps := baselineReps(t)
	arms := baselineArms()

	t.Logf("protocol=%s task_set=%s harness=%s", baselineProtocolVersion, baselineTaskSet, baselineGitHEAD(t))
	t.Logf("provider=%s model=%s | judge provider=%s model=%s | reps/arm=%d", prov, model, judgeProv, jm, reps)
	t.Logf("")
	t.Logf("fixture 真值=%s（%s）  decoy=%s（%s）  注入过时值=%s",
		baselineTrueValue, baselineTruthFile, baselineDecoyValue, baselineDecoyFile, baselineStaleValue)
	t.Logf("")

	rep := &baselineReport{
		Experiment:  "stale-context-three-arm-baseline",
		Protocol:    baselineProtocolVersion,
		RunID:       fmt.Sprintf("%d", time.Now().UnixNano()),
		At:          time.Now().UTC().Format(time.RFC3339),
		Model:       model,
		Provider:    prov,
		JudgeModel:  jm,
		JudgeProv:   judgeProv,
		HarnessSnap: baselineGitHEAD(t),
		MemorySnap:  memSnap,
		TaskSet:     baselineTaskSet,
		Budget:      "max_iterations=8 max_retries=2 llm_timeout=4m run_timeout=6m read-only tools (read_file/grep/glob)",
		Reps:        reps,
		Notes: []string{
			"这是 Step 0 基线：空状态已断言（无生产文件 import memory 包），测量因此不依赖任何 memory 实现。",
			"工作记忆是合成夹具仓（t.TempDir），不含 AGENTS.md —— 避免本仓那份 AGENTS.md 的指令层污染基线。",
			"三臂只差一段注入文本，其余（工具面、任务措辞、轮次预算、模型参数）逐字节相同；" +
				"这一点由 memory_baseline_wiring_test.go 的 TestBaselineInjectionReachesProvider 断言（抹掉注入后 A 与 C 的 system prompt 归一化后逐字节相同）。",
			"判定器：独立模型（与被测模型不同），只看最终答案；每答判两次且候选顺序对调，自洽率同时度量抖动与位置偏置。",
			"n<12 时单档差异可能完全来自运行间波动（协议 §8）—— 逐臂均值旁的 std 不是装饰。",
			"NEITHER 计入不正确的分母：模型说「仓库没实现/来源冲突」时，交的不是任何一个候选值。",
			"未测到：本模型对**无 decoy**任务的表现、冲突臂下模型是否会在别处复述过时值、以及除 space-bunny-free 以外的任何模型。",
		},
	}

	selfAgree, swapDis, judged := 0, 0, 0
	for _, arm := range arms {
		stat := baselineArmStat{Arm: arm.Name, DocLabel: arm.DocLabel, Verdicts: map[string]int{}}
		var correct, tools, docs []float64
		for i := 0; i < reps; i++ {
			root := baselineExecFixture(t)
			run := baselineRunOnce(t, p, prov, model, root, arm.Inject)
			run.Arm, run.Rep = arm.Name, i

			if run.Err != "" || strings.TrimSpace(run.FinalAnswer) == "" {
				run.Verdict = "ERROR"
				stat.NErr++
				stat.Verdicts["ERROR"]++
				rep.Runs = append(rep.Runs, run)
				t.Logf("  [%s #%d] 运行失败或无最终答案: err=%q answer=%.120q",
					arm.Name, i, run.Err, run.FinalAnswer)
				continue
			}

			jctx, jcancel := context.WithTimeout(context.Background(), 3*time.Minute)
			verdict, agree, raw, jerr := baselineJudgeTwice(jctx, judgeP, judgeProv, jm, baselineQuestion, run.FinalAnswer)
			jcancel()
			run.Verdict, run.SelfAgree, run.VerdictRaw = verdict, agree, raw
			if jerr != nil {
				run.Verdict = "ERROR"
			}

			judged++
			if agree {
				selfAgree++
			} else if verdict != "ERROR" {
				swapDis++
			}
			stat.Verdicts[verdict]++
			if verdict == "TRUE" {
				correct = append(correct, 1)
				stat.Correct++
			} else {
				correct = append(correct, 0)
			}
			tools = append(tools, float64(run.ToolCalls))
			docs = append(docs, float64(len(run.DocsRead)))
			if run.ReadTruth {
				stat.ReadTruth++
			}
			if run.ReadDecoy {
				stat.ReadDecoy++
			}
			if run.ReadNotes {
				stat.ReadNotes++
			}

			rep.Runs = append(rep.Runs, run)
			t.Logf("  [%s #%d] verdict=%-7s selfagree=%-5v tools=%2d docs=%v truth=%-5v decoy=%-5v | %.160s",
				arm.Name, i, run.Verdict, agree, run.ToolCalls, run.DocsRead,
				run.ReadTruth, run.ReadDecoy, strings.ReplaceAll(run.FinalAnswer, "\n", " "))
		}
		stat.N = len(correct)
		if stat.N > 0 {
			stat.CorrectRate = float64(stat.Correct) / float64(stat.N)
			stat.ToolMean, stat.ToolStd = baselineMeanStd(tools)
			stat.DocsMean, stat.DocsStd = baselineMeanStd(docs)
		}
		rep.Arms = append(rep.Arms, stat)

		t.Logf("")
		t.Logf("=== %s — %s ===", arm.Name, arm.DocLabel)
		t.Logf("  正确率   %d/%d = %.0f%%", stat.Correct, stat.N, 100*stat.CorrectRate)
		t.Logf("  工具调用 %.2f ± %.2f", stat.ToolMean, stat.ToolStd)
		t.Logf("  读文档数 %.2f ± %.2f", stat.DocsMean, stat.DocsStd)
		t.Logf("  判定分布 %v   读过真值 %d/%d  碰过 notes/ %d/%d  读过 decoy %d/%d",
			stat.Verdicts, stat.ReadTruth, stat.N, stat.ReadNotes, stat.N, stat.ReadDecoy, stat.N)
		t.Logf("")
	}

	if judged > 0 {
		rep.JudgeSelf = float64(selfAgree) / float64(judged)
	}
	rep.JudgeSwapDis = swapDis
	rep.JudgeJudged = judged

	// —— 汇总 ——
	t.Logf("=== 三臂汇总（n=%d/臂）===", reps)
	for _, s := range rep.Arms {
		t.Logf("  %-20s 正确 %5.0f%%  工具 %5.2f±%.2f  文档 %4.2f±%.2f  判定 %v",
			s.Arm, 100*s.CorrectRate, s.ToolMean, s.ToolStd, s.DocsMean, s.DocsStd, s.Verdicts)
	}
	if rep.JudgeJudged > 0 {
		t.Logf("  判定器自洽率 %.0f%% (%d/%d，两序对调一致)；顺序不一致 %d 条",
			100*rep.JudgeSelf, selfAgree, judged, swapDis)
		if rep.JudgeSelf <= 0.90 {
			t.Logf("  ⚠ 自洽率 ≤90%%：按协议 §10，**本次评估的所有结论作废**。上面的数字是仪器噪声。")
		}
	}

	// B vs A：把差与运行间波动并排给出来，让"差异是否超出方差范围"成为可判断的读数。
	if len(rep.Arms) == 3 {
		a, b, c := rep.Arms[0], rep.Arms[1], rep.Arms[2]
		d := b.CorrectRate - a.CorrectRate
		pooled := math.Sqrt((a.ToolStd*a.ToolStd + b.ToolStd*b.ToolStd) / 2)
		t.Logf("")
		t.Logf("=== 判据：B（指针注入）必须明显高于 A（冲突注入）===")
		t.Logf("  正确率差 B-A = %+.0f 个百分点 (B %.0f%% vs A %.0f%%)", 100*d, 100*b.CorrectRate, 100*a.CorrectRate)
		t.Logf("  读文档数差 B-A = %+.2f", b.DocsMean-a.DocsMean)
		t.Logf("  工具调用差 B-A = %+.2f（两臂工具调用 pooled std = %.2f）", b.ToolMean-a.ToolMean, pooled)
		if d <= 0.15 {
			t.Logf("  ⚠ 差值 ≤15 个百分点：在 n=%d 下**不足以宣称 B 明显高于 A**。", reps)
			t.Logf("    这不等于「注入无影响」——n=%d 时运行间波动就能覆盖这个差。诚实读法是：数据不足。", reps)
		} else {
			t.Logf("  差值 >15 个百分点；但 n=%d 仍低于 §8 的 n≥12 门槛，只能说「方向与幅度都不像纯噪声」，不能称显著。", reps)
		}
		t.Logf("  C（无注入）正确率 %.0f%%  —— A 若与 C 同档，说明注入的自信答案既没帮上忙也没害处；",
			100*c.CorrectRate)
		t.Logf("    A 若**低于** C，注入自信答案确实压掉了核对行为（地板条件在本模型上复现）。")
	}

	baselineWriteReport(t, rep)
}

func baselineWriteReport(t *testing.T, rep *baselineReport) {
	t.Helper()
	path := os.Getenv("HAI_EVAL_REPORT")
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Logf("report dir: %v", err)
		return
	}
	blob, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Logf("report marshal: %v", err)
		return
	}
	if err := os.WriteFile(path, append(blob, '\n'), 0o600); err != nil {
		t.Logf("report write: %v", err)
		return
	}
	t.Logf("report written: %s", path)
}

// baselineModuleRoot 定位 go.mod 所在目录（模块根）。
//
// 不用 runtime.Caller 硬数层数（改目录结构就错），而是一路上溯找 go.mod；
// 找不到就退回本文件所在目录（此时 go list 会失败并由调用方 t.Fatalf，不会静默通过）。
func baselineModuleRoot() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Dir(thisFile)
		}
		dir = parent
	}
}
