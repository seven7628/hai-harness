package main

// memory_effectiveness_test.go —— Memory「有效性与准确性」的真实测量。
//
// # 这份测试和 memory_evolution_real_test.go 测的不是同一件事
//
// 前一份测的是**行为形态**：命中记忆之后模型还探查几次、工具调用几次。那是代理
// 指标（proxy metric）——它假设"少探查 = 记忆有用"，但探查次数下降同样可能来自
// 模型变懒、变自信、或者被协议话术劝退。这份测试直接测三件被代理指标遮住的事：
//
//  1. **召回率（recall）**——索引里那条记忆，这次到底被读了吗？
//     关键在于**静默丢失**：MEMORY.md 给每个 topic 一行摘要，模型只凭这一行决定
//     要不要 memory_view。摘要写得偏 → 那条记忆永远不被读 → 而**没有任何报错**。
//     更糟的是 agents/memory_probe.go 的 emitMemoryProbe 在 Views==0 时直接 return，
//     于是"索引里有但从未被读"这件事在生产事件流里**根本不存在**。没有读数就没有
//     召回率，只有印象。本文件在**测试侧**补出这个观测维度（不改生产文件）。
//
//  2. **准确性基线**——真实写下来的记忆，现在还对不对？
//     语料不是构造的 fixture，而是 ~/.claude/projects/.../memory/ 下 24 条 Claude
//     Code 在本仓库上跨 2026-08-11 ~ 09-24 真实累积的记忆。仓库在记忆写下之后
//     继续演进（2026-09-23 项目改名 go-code → hai），所以"还有几条准"是一个真
//     的开放问题，不是走过场。
//
//  3. **常驻 vs JIT**——用户的直觉："记忆应该是存在脑子里的，而不是需要的时候
//     才去取。"本文件把少量 topic 正文**预注入 system prompt**（常驻层），
//     与现状的 JIT（索引 + memory_view）做对照。**只做实验测量路径，不做成产品
//     功能**。其中最有价值的一格不是"常驻更准"，而是"常驻一条**过期**记忆，模型
//     会不会把它当成背景事实直接复述，而不再核对仓库"——这正是 JIT 声称的护栏，
//     但 JIT 从没在"过期记忆"这个格上被测过。
//
// # 为什么不设通过阈值
//
// 单样本的波动远大于这里任何预期效应。这份测试的产出是**读数**，不是断言：
// 除了"基础设施确实跑在真实 provider 上"和"观测维度确实能算出召回率"这两类
// 结构性断言外，行为类结果一律只 t.Logf。样本不足时报告里明说样本不足。
//
// # 运行方式
//
//	go test ./desktop/bridge/ -run TestRealMemoryEffectiveness -v -count=1 -timeout 90m
//
// 环境变量：
//
//	HAI_EVAL_PROVIDER / HAI_EVAL_MODEL  provider 与模型（默认 opencode2 / space-bunny-free）
//	HAI_EVAL_REPS                        每个实验臂的重复次数（默认 2；越大越能估方差）
//	HAI_EVAL_ACCURACY_CORPUS             准确性基线语料目录（默认 Claude Code 项目记忆目录）
//	HAI_EVAL_REPORT                      把完整 JSON 读数写到该路径（便于复现/聚合）
//
// -short 一律 skip；缺凭证一律 skip。绝不在没网/没 key 的环境里假失败。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/evolution"
	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// defaultAccuracyCorpus 是本机上真实累积的记忆语料：Claude Code 为**本仓库**
// 写的项目记忆，跨 2026-08-11 ~ 09-24。
//
// 为什么用它而不是造 fixture：准确性问题只在**记忆比它描述的世界活得久**时才
// 出现。造出来的记忆和造出来的仓库同生共死，永远"准确"，测不到任何东西。
const defaultAccuracyCorpus = "/Users/admin/.claude/projects/-Users-admin-workspace-go-code/memory"

// effReps 返回每个实验臂的重复次数。
func effReps() int {
	if v := os.Getenv("HAI_EVAL_REPS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 2
}

// —— 观测层：把"召回到哪条"从事件流里挖出来 ————————————————————

// effView 是一次成功（或失败）的 memory_view。
type effView struct {
	Ref     string
	Success bool
	Err     string
}

// effRun 是一次真实运行的完整读数。
//
// 为什么不用 memory_probe 事件算召回：它只带**聚合计数**（Views / PostHit*），
// 没有"读的是哪一条"。而召回问题恰恰是逐条的——"索引里有 3 条，被读的是哪条"。
// 逐条归因只能从 ToolResponse.Arguments 里的 ref 拿。这正是本文件要暴露的
// 生产读数缺口。
type effRun struct {
	// IndexNames 本次 system prompt 索引里**实际可见**的 topic 名（分母）。
	IndexNames []string
	// IndexRaw 注入的 MEMORY.md 原文（核验索引确实进了 system prompt）。
	IndexRaw string
	// Views 逐条 memory_view 记录（分子，按 ref 归一化）。
	Views []effView
	// ProbeEvents 生产侧 memory_probe 事件数；Views==0 时应当为 0。
	ProbeEvents int
	// ProbeEmitted 本次运行是否发出了 memory_probe 事件（Views==0 时生产侧不发出）。
	ProbeEmitted bool
	// Tools 全部工具调用名。
	Tools []string
	// ReadFiles read_file 实际读过的路径（逐条，用于"有没有去核对仓库"这个
	// 机械口径的判断）。为什么不能只看工具名：模型可以用 grep/glob 得到同样
	// 的信息，"核对过"是行为而不是工具选择。
	ReadFiles []string
	// Text 最终回复。
	Text string
	// Finished 运行是否正常收尾。
	Finished bool
	// InputTokens AgentEnd 报告的输入 token（常驻臂与 JIT 臂的上下文成本差）。
	InputTokens int64
	// DurationMs 运行耗时。
	DurationMs int64
	// Err 运行错误。
	Err string
}

// viewedTopics 返回本次**成功**读到的 topic 名集合。
func (r *effRun) viewedTopics() map[string]int {
	out := map[string]int{}
	for _, v := range r.Views {
		if !v.Success {
			continue
		}
		if n, err := memory.ParseRef(v.Ref); err == nil && !n.IsIndex {
			out[n.Name]++
		}
	}
	return out
}

// countTools 统计某工具的调用次数。
func (r *effRun) countTools(name string) int {
	n := 0
	for _, x := range r.Tools {
		if x == name {
			n++
		}
	}
	return n
}

// effRecorder 收集一次运行的读数。
type effRecorder struct {
	mu   sync.Mutex
	run  effRun
	done bool
}

func (rec *effRecorder) handle(_ context.Context, e events.Event) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	switch ev := e.(type) {
	case *events.ToolResponse:
		rec.run.Tools = append(rec.run.Tools, ev.Name)
		if ev.Name == "read_file" {
			var a struct {
				Path string `json:"path"`
			}
			// 参数名在失败时也可能与成功时不同；解析不出就退化成原始参数串，
			// 让"读没读过"的最坏情况是假阴性（少算核对），不阻塞。
			_ = json.Unmarshal([]byte(ev.Arguments), &a)
			rec.run.ReadFiles = append(rec.run.ReadFiles, a.Path)
		}
		if ev.Name == "memory_view" {
			var a struct {
				Ref string `json:"ref"`
			}
			// 参数解析失败也要记账：模型调了 memory_view 但参数非法，
			// 这本身就是索引摘要没引导出正确 ref 的证据。
			if err := json.Unmarshal([]byte(ev.Arguments), &a); err != nil {
				rec.run.Views = append(rec.run.Views, effView{Ref: "<unparseable:" + ev.Arguments + ">", Success: false, Err: err.Error()})
			} else {
				rec.run.Views = append(rec.run.Views, effView{Ref: a.Ref, Success: !ev.IsError, Err: ev.Result})
			}
		}
	case *events.MemoryProbe:
		rec.run.ProbeEvents++
		rec.run.ProbeEmitted = true
	case *events.AgentEnd:
		rec.run.Text = ev.Content
		rec.run.Finished = true
		rec.run.DurationMs = ev.DurationMS
		if ev.Usage != nil {
			rec.run.InputTokens = ev.Usage.Input
		}
		rec.done = true
	}
}

func (rec *effRecorder) snapshot() effRun {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	r := rec.run
	r.IndexNames = append([]string(nil), r.IndexNames...)
	r.Views = append([]effView(nil), r.Views...)
	r.Tools = append([]string(nil), r.Tools...)
	r.ReadFiles = append([]string(nil), r.ReadFiles...)
	return r
}

// —— 实验 1：召回率与静默丢失 ——————————————————————————————

// effTopicSpec 是一条待写入记忆的完整规格，含**索引摘要的写法风格**。
type effTopicSpec struct {
	Name string
	// Title 主题标题（不参与本次变量控制，进 frontmatter）。
	Title string
	// Summary 索引里那一行摘要。**这是本实验的自变量。**
	Summary string
	// Body 正文（只被 memory_view 读到）。
	Body string
	// Needle 正文里那个"只有记忆里有"的关键串，命中它 = 召回成功。
	Needle string
	// Decoy 仓库里那条错误答案串（见 effRecallFacts.Decoy）。
	Decoy string
	// File 该主题对应的仓库文件（仓库里有一份**不完整**的说明，
	// 缺 Needle 描述的那条规矩——所以"读了仓库"不能替代"读了记忆"）。
	File     string
	FileBody string
	// TaskQuestion 该主题在任务里的那一句。
	TaskQuestion string
}

// 摘要写法两档。**自变量只有摘要**：同一批记忆在两种摘要下各跑一次，正文、
// 任务句、仓库、工具面、模型全部同构，所以档位之间的差异只能归因到
// "索引那一行写了什么"。
//
// 为什么只留两档：第三档（opaque，摘要只有一个标题词）在第一次运行里
// n=3 全部未被召回，与 literal 档 3/3 构成对比，但 n 太小不能当成效应量。
// 保留两档是为了每档能拿到更多重复次数——在这个 provider 上单次运行要十几秒，
// 档位越多每档越薄，而"薄到看不出方差"的数字不如不报。
const (
	// summaryMatched 摘要复现了任务句里的触发词（字面就能对上）。
	// 这是记忆写得"好"的一种形态。
	summaryMatched = "matched"
	// armNoMemory 无记忆对照臂：工具面/协议/任务句与另两臂完全一致，只是索引里
	// 没有条目。没有它就无法区分"记忆起了作用"与"答案本来就是这样"。
	armNoMemory = "no-memory-control"
	// summaryDivergent 摘要写的是这条记忆**讲什么**（人类标题式），
	// 没有任何"什么时候该读它"的线索。模型自己用 memory_write 写摘要时
	// 经常产出这种形态：题干问的是"当这种情况该怎么办"，摘要答的是
	// "这个 topic 讨论 X"。人读索引会觉得相关，字面/语义匹配都不会命中。
	// **这是本实验真正要测的召回失败形态。**
	summaryDivergent = "divergent"
)

// effRecallFacts 是三条记忆的事实内容。
//
// 抽出成独立结构是因为"同一条记忆"要出现在两种摘要下才能配对比较。
// 配对是必须的：若两次运行用的是不同记忆，任何差异都可能是记忆内容的差异
// 而不是摘要写法的差异。
type effRecallFacts struct {
	Name string
	// Title 主题标题（进 frontmatter，不进索引行）。
	Title string
	// Fact 唯一答案串：只出现在记忆正文里，仓库的 decoy 写的是别的值。
	// 答案里出现 Fact = 用了记忆；出现 Decoy = 用了仓库那份错的。
	Fact string
	// Decoy 仓库里那条**貌似可信但错误**的答案串。
	//
	// 为什么要放 decoy：没有它，模型在仓库里找不到任何答案，只能去读记忆，
	// 召回率必然 100%——那是"没有替代方案"的必然，不是记忆摘要写得好的功劳。
	// 真实场景恰恰相反：仓库里往往有一条**旧的、错的**说明，而记忆的存在
	// 理由就是纠正它。召回失败在这种场景里才有机会发生，而且**不会有任何报错**：
	// 模型读到了仓库、给出了答案、任务正常收尾。
	Decoy string
	// Body 正文（只有 memory_view 能读到）。
	Body string
	// RepoFile 仓库里对应的文件（内容刻意不含 Fact）。
	RepoFile string
	// RepoBody 该文件的内容：一份"看起来相关但没有答案"的说明。
	RepoBody string
	// TaskQuestion 任务句：措辞**不**包含记忆标题的任何词，
	// 也不包含 Body 里的术语——这样"标题命中"这条捷径被切断。
	TaskQuestion string
	// MatchedSummary 匹配档摘要：复现任务句的用词。
	MatchedSummary string
	// DivergentSummary 偏写档摘要：只说这条记忆讲什么。
	DivergentSummary string
}

func effRecallFactsFixture() []effRecallFacts {
	return []effRecallFacts{
		{
			Name:     "release-branch-policy",
			Title:    "Release branch naming policy",
			Fact:     "CERTIFIED",
			Body:     "Before any tag is cut, run `make golden`. It prints CERTIFIED on success and exits non-zero otherwise.\n",
			RepoFile: "docs/releasing.md",
			RepoBody: "# Releasing\n\nReleases are cut from a `release/*` branch after review.\n" +
				"Before shipping, run `make ship-check`; it prints SHIPPED on success.\n",
			// 任务句用 "ship a new build"，记忆用 "cut a tag" —— 不同措辞。
			TaskQuestion:     "Before we ship a new build, what command has to pass first, and what does it print on success?",
			MatchedSummary:   "matched: what has to run before shipping a new build, and what it prints on success",
			DivergentSummary: "Release branch naming policy and the tag-cutting procedure",
		},
		{
			Name:     "staging-schema-drift",
			Title:    "Staging database schema drift",
			Fact:     "drain-and-replay",
			Body:     "When the pre-production datastore rejects a column that local has, do a drain-and-replay: `make stage-drain` then `make stage-replay`. Do NOT use `stage-migrate`; it is destructive on staging.\n",
			RepoFile: "docs/staging.md",
			RepoBody: "# Staging\n\nStaging uses a single Postgres instance shared by all feature branches.\n" +
				"If a column is missing on staging, run `make stage-migrate --force`.\n",
			// 任务句用 "a column that only works on my laptop"，记忆用 "rejects a column that local has"。
			TaskQuestion:     "A column I added works on my laptop but the pre-production datastore refuses it. What is the documented procedure?",
			MatchedSummary:   "matched: a column that works on my laptop but is refused by the pre-production datastore, and the documented procedure",
			DivergentSummary: "Staging database schema drift: how pre-production schemas diverge from local",
		},
		{
			Name:     "oncall-pager-routing",
			Title:    "On-call pager routing",
			Fact:     "secondary-rotation",
			Body:     "Pages for services you do not own go to the secondary-rotation alias (`@oncall-secondary`), never to the owning team's direct channel.\n",
			RepoFile: "docs/oncall.md",
			RepoBody: "# On-call\n\nPaging is handled by the platform team. Rotations are published in the team calendar.\n" +
				"For another team's service, page the owning team directly in their Slack channel.\n",
			// 任务句用 "someone else's service"，记忆用 "services you do not own"。
			TaskQuestion:     "I just got paged for someone else's service. Who should it be forwarded to, and by what mechanism?",
			MatchedSummary:   "matched: getting paged for a service that belongs to another team, who it is forwarded to and by what mechanism",
			DivergentSummary: "On-call pager routing: who receives alerts in this project",
		},
	}
}

// effRecallDistractors 是**与三个子问题无关**的记忆。
//
// 数量是关键设计：只有 3 条索引条目对 3 个问题时，"召回"退化成"把索引全读一遍"，
// 模型不需要做任何选择，召回率必然 100%（第一次运行就是这样，9/9 与 12/12）。
// 真实记忆库里有几十条，模型必须**先判断哪条相关**再决定读不读——那才是
// "摘要写得偏不偏"起作用的环节。加入干扰项后，分母（目标条目）与分子
// （被读到的目标条目）才对应真实的召回问题。
func effRecallDistractors() []effRecallFacts {
	names := []struct{ name, title, body, decoy string }{
		{"typography-tokens", "Typography scale tokens",
			"The type scale is defined in tokens.json: --size-sm 12px, --size-md 14px, --size-lg 18px.\n", "rem"},
		{"i18n-string-extraction", "Translation string extraction",
			"UI strings are extracted by `make i18n-extract` into locales/en.json. Never edit locales/*.json by hand.\n", "webpack plugin"},
		{"feature-flag-lifecycle", "Feature flag lifecycle",
			"Flags are born in the config repo and die by removing them in a follow-up PR; no permanent `false` defaults.\n", "config repo"},
		{"log-sampling-policy", "Log sampling policy",
			"Debug logs are sampled at 10% in staging and 100% locally; raise with LOG_SAMPLE.\n", "0.1%"},
		{"incident-severity-levels", "Incident severity levels",
			"SEV1 pages immediately, SEV2 within 30 minutes, SEV3 goes to the next standup.\n", "SEV1 within 1 hour"},
	}
	out := make([]effRecallFacts, 0, len(names))
	for _, n := range names {
		out = append(out, effRecallFacts{
			Name: n.name, Title: n.title, Fact: n.body, Body: n.body, Decoy: n.decoy,
			RepoFile: "docs/" + n.name + ".md",
			RepoBody: "# " + n.title + "\n\nSee the project documentation for details.\n",
		})
	}
	return out
}

// effRecallSpec 按摘要风格实例化一组记忆规格。
func effRecallSpec(facts []effRecallFacts, style string) []effTopicSpec {
	out := make([]effTopicSpec, 0, len(facts))
	for _, f := range facts {
		summary := f.MatchedSummary
		if style == summaryDivergent {
			summary = f.DivergentSummary
		}
		out = append(out, effTopicSpec{
			Name: f.Name, Title: f.Title, Summary: summary, Body: f.Body,
			Needle: f.Fact, Decoy: f.Decoy, File: f.RepoFile, FileBody: f.RepoBody,
			TaskQuestion: f.TaskQuestion,
		})
	}
	// 干扰项用"matched"档摘要：它们必须**容易**被识别为无关，才不会把
	// "读了没用的" 混进召回失败的分子分母里。干扰项的召回单独统计。
	for _, d := range effRecallDistractors() {
		out = append(out, effTopicSpec{
			Name: d.Name, Title: d.Title,
			Summary: d.Title + " — background reference, not related to any specific question",
			Body:    d.Body, Needle: d.Fact, Decoy: d.Decoy,
			File: d.RepoFile, FileBody: d.RepoBody,
		})
	}
	return out
}

// effTargetNames 是三个子问题对应的目标记忆名（干扰项不在其中）。
func effTargetNames(facts []effRecallFacts) []string {
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		out = append(out, f.Name)
	}
	return out
}

// effRecallTask 拼出实验 1 里的完整任务：三个子问题对应三条记忆。
//
// 子问题**不给任何"去看记忆"的显式提示**（与 memory_evolution_real_test.go 的
// "Check the project memory first if it helps" 相反）。生产场景里没人会提醒
// 模型去用记忆；给了提示就等于把召回率测成"听不听话"，而不是"摘要能不能把
// 它引到那条记忆上"。
func effRecallTask(specs []effTopicSpec) string {
	var b strings.Builder
	b.WriteString("Answer all three questions about this project. The repository files are incomplete on purpose.\n")
	for i, s := range specs {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, s.TaskQuestion)
	}
	return b.String()
}

func effRepoWith(t *testing.T, specs []effTopicSpec) string {
	t.Helper()
	dir := t.TempDir()
	for _, s := range specs {
		full := filepath.Join(dir, s.File)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s.FileBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"),
		[]byte("# recall fixture\n\nThis repository exists only for the memory recall evaluation.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// effRecallResult 是一次召回运行的归约结果。
type effRecallResult struct {
	Style string
	// Target topic 名。
	Target string
	// IsDistractor true = 干扰项（没有对应子问题）。干扰项的"被读到"不是
	// 召回失败——多读一条无关记忆只是浪费上下文，把它算进召回分母会
	// 把"选择准确"和"选择全"混成一个数。
	IsDistractor bool
	Viewed       bool
	ViewCount    int
	// ViewsTotal 本次总 memory_view 次数（含读错目标的）。
	ViewsTotal int
	// AdoptedMemory 回复里出现了记忆正文的答案串（而不是仓库的 decoy）。
	//
	// 与 Recalled 分开是本实验最重要的一个修正：读到 ≠ 用上。memory_view 被调用
	// 100% 并不代表记忆起了作用——模型完全可能读完记忆、发现它与仓库冲突、
	// 然后按协议里"仓库优先"的明文规则**丢弃**记忆采信仓库。把这两个口径
	// 合成一个"召回率"会把"读了不用"算成成功。
	AdoptedMemory bool
	// FellForDecoy 回复里出现了仓库里那条**错误**答案。
	FellForDecoy bool
	// Adjudicated 一次**独立裁定**的结论（见 effAdjudicate）。
	Adjudicated string
	// AdjudicateReason 裁定理由（模型原文，截断）。
	AdjudicateReason string
	// Question 该条目对应的子问题（裁定用）。
	Question string
	// RunText 本次运行的最终答复全文（裁定要读整段，见 runTextFor）。
	RunText string
	// ProbeEmitted 生产侧是否发出了 memory_probe。
	ProbeEmitted bool
	// RunHasProbe 同 ProbeEmitted，但语义上指"整次运行"而不是"这条 topic"，
	// 避免读数里出现"这条记忆发出了 probe 事件"这种误解。
	RunHasProbe bool
	Tools       int
	InputTokens int64
	Err         string
}

// TestRealMemoryRecallIsObservable 测量「索引里有、但被读到没有」——即召回率。
//
// 这是三个实验里唯一一个**结构性断言**测试：它要证明的不是模型表现，而是
// "召回失败在当前生产读数里不可观测"这件事是真的。看两处：
//
//	a) 我们能从事件流里**逐条**算出三条记忆各自被读没有（Test 通过的必要条件）；
//	b) 当某次运行一条都没读时，生产侧 memory_probe 事件的条数是 0
//	   （emitMemoryProbe 在 Views==0 时 return）——即"全部静默丢失"与
//	   "这次运行压根没用记忆"在生产事件流里是**同一种观测**。
//
// b 是本测试存在的核心理由：如果 memory_probe 在 Views==0 时照发，这条测试的
// 价值就只剩"补一个索引条目数"的读数。生产文件不能改，所以 a/b 都只能在测试侧
// 观测，结论留给报告。
func TestRealMemoryRecallIsObservable(t *testing.T) {
	provName, model, pc := evalProviderOrSkip(t)
	facts := effRecallFactsFixture()
	p := pc.newProvider(provName)
	reps := effReps()

	// 两种摘要风格各跑一遍。**配对**：同一批记忆、同一批任务句、同一个仓库，
	// 唯一变的是索引那一行写了什么。
	results := make([]effRecallResult, 0, 2*len(facts)*reps)
	// 三臂：两种摘要 + 一条**无记忆对照**。
	//
	// 对照臂是必需的：只有它能区分"记忆让答案变好"与"答案本来就这样"。
	// 它装 memory 工具（工具面完全一致）但索引里没有条目，所以工具可用性、
	// 协议文本、任务句全部同构，唯一变的是"模型能不能读到记忆"。
	for _, style := range []string{summaryMatched, summaryDivergent, armNoMemory} {
		specs := effRecallSpec(facts, style)
		repo := effRepoWith(t, specs)
		withMemory := style != armNoMemory
		for i := 0; i < reps; i++ {
			run := effRunTask(t, effTaskOpts{
				Repo: repo, Provider: p, ProvName: provName, Model: model,
				Task: effRecallTask(specs), Topics: specs,
				WithMemory:      withMemory,
				NoMemoryControl: !withMemory,
			})
			viewed := run.viewedTopics()
			// 结构性前提 a：索引条目必须真的进了 system prompt，否则分母是假的。
			// 对照臂的分母恒为 0（索引里没有条目），跳过这条检查。
			if withMemory && len(run.IndexNames) != len(specs) {
				t.Fatalf("rep %d (%s): index carried %d entries, want %d — the recall denominator would be wrong\nindex:\n%s",
					i, style, len(run.IndexNames), len(specs), run.IndexRaw)
			}
			// 目标条目 = 有对应子问题的那几条；其余是干扰项。分类在这里做一次，
			// 后面所有分母都从它派生，避免"哪些算目标"在不同读数里漂移。
			targets := effTargetNames(facts)
			for _, s := range specs {
				if withMemory && !containsStr(run.IndexNames, s.Name) {
					t.Fatalf("rep %d (%s): topic %q missing from the injected index", i, style, s.Name)
				}
				results = append(results, effRecallResult{
					Style:         style,
					Target:        s.Name,
					IsDistractor:  !containsStr(targets, s.Name),
					Viewed:        viewed[s.Name] > 0,
					ViewCount:     viewed[s.Name],
					ViewsTotal:    len(run.Views),
					AdoptedMemory: strings.Contains(run.Text, s.Needle),
					FellForDecoy:  strings.Contains(run.Text, s.Decoy),
					ProbeEmitted:  run.ProbeEmitted,
					Tools:         len(run.Tools),
					InputTokens:   run.InputTokens,
					RunHasProbe:   run.ProbeEmitted,
					Err:           run.Err,
					Question:      s.TaskQuestion,
					RunText:       run.Text,
				})
			}
			t.Logf("[%s] rep %d: tools=%d views=%d probe_events=%d finished=%v answer=%q",
				style, i, len(run.Tools), len(run.Views), run.ProbeEvents, run.Finished, effFirstLine(run.Text))
		}
	}

	// —— 逐条裁定：谁交付了答案 ——
	//
	// 只对**有记忆**的两个臂裁定（对照臂没有记忆可交付，裁定无意义）。
	// 每次裁定是一次真实模型调用，成本与时间都计在预算里；裁定数量有限
	// 本身也是"样本不足"这一结论的一部分。
	t.Log("")
	nTarget := 0
	for _, r := range results {
		if r.Style != armNoMemory && !r.IsDistractor {
			nTarget++
		}
	}
	t.Logf("=== 逐条裁定（每个子问题一次独立模型调用，判断交付的答案来自哪份）n=%d ===", nTarget)
	adjudicated := 0
	for i := range results {
		r := &results[i]
		if r.Style == armNoMemory || r.IsDistractor {
			continue
		}
		verdict, reason := effAdjudicate(t, p, provName, model, r.Question, runTextFor(results, i))
		r.Adjudicated, r.AdjudicateReason = verdict, reason
		adjudicated++
		t.Logf("  [%s] %-24s recall=%v str_fact=%v str_decoy=%v → %s (%s)",
			r.Style, r.Target, r.Viewed, r.AdoptedMemory, r.FellForDecoy, verdict, reason)
	}
	t.Logf("  已裁定 %d 条（其余为对照臂与干扰项，不适用）", adjudicated)

	// —— 读数 ——
	byStyle := map[string][]effRecallResult{}
	for _, r := range results {
		byStyle[r.Style] = append(byStyle[r.Style], r)
	}
	t.Log("")
	t.Log("=== 召回率：读到了没有（分母 = 索引中可见的**目标** topic 数）===")
	t.Logf("  %-11s %-12s %-12s %-8s %s", "arm", "recall(目标)", "读了干扰项", "工具数", "n")
	for _, style := range []string{summaryMatched, summaryDivergent, armNoMemory} {
		rs := effTargets(byStyle[style])
		if len(rs) == 0 {
			continue
		}
		viewed, tools := 0, 0
		distract := 0
		for _, d := range byStyle[style] {
			if d.IsDistractor && d.Viewed {
				distract++
			}
		}
		for _, r := range rs {
			if r.Viewed {
				viewed++
			}
			tools += r.Tools
		}
		t.Logf("  %-11s %d/%d %5.0f%%     %d/%d       %5.1f   n=%d",
			style, viewed, len(rs), 100*float64(viewed)/float64(len(rs)),
			distract, len(effDistractors(byStyle[style])), float64(tools)/float64(len(rs)), len(rs))
	}
	t.Logf("  %s", effSampleVerdict(len(results)))
	t.Logf("  注：干扰项被读不是召回失败（那是多读了无关记忆，浪费上下文），单列。")

	// —— 有效性：采纳了没有 ——
	t.Log("")
	t.Log("=== 有效性：读到了之后用上了没有（裁定口径）===")
	t.Logf("  %-11s %-14s %-14s %-12s %s", "arm", "FROM_MEMORY", "FROM_REPO", "无法裁定", "n")
	for _, style := range []string{summaryMatched, summaryDivergent} {
		rs := effTargets(byStyle[style])
		mem, repo, unres := 0, 0, 0
		for _, r := range rs {
			switch r.Adjudicated {
			case "FROM_MEMORY":
				mem++
			case "FROM_REPO":
				repo++
			default:
				unres++
			}
		}
		t.Logf("  %-11s %2d/%d %3.0f%%    %2d/%d %3.0f%%   %d        n=%d",
			style, mem, len(rs), 100*float64(mem)/float64(len(rs)),
			repo, len(rs), 100*float64(repo)/float64(len(rs)), unres, len(rs))
	}
	t.Logf("  %s", effSampleVerdict(len(results)))
	t.Logf("  这一格才是「记忆有没有用」：recall 高而 FROM_REPO 高 = 读到了但被仓库的")
	t.Logf("  旧说明覆盖掉。生产侧 memory_probe 只统计前者，会把后者记成成功。")

	// —— 静默丢失的可观测性 ——
	t.Log("")
	t.Log("=== 静默丢失：逐条召回失败在生产事件流里的可见性 ===")
	for _, style := range []string{summaryMatched, summaryDivergent, armNoMemory} {
		rs := effTargets(byStyle[style])
		if len(rs) == 0 {
			continue
		}
		failed, probeInFailedRuns := 0, 0
		for _, r := range rs {
			if !r.Viewed {
				failed++
				if r.RunHasProbe {
					probeInFailedRuns++
				}
			}
		}
		t.Logf("  [%-18s] 目标条目未被读 %d/%d；其中同一次运行仍发出了 memory_probe 事件的 = %d",
			style, failed, len(rs), probeInFailedRuns)
	}
	t.Log("")
	t.Log("  结构性结论：")
	t.Log("   - memory_probe 只在 Views>0 时发出（agents/memory_probe.go:89）。")
	t.Log("     一次运行若一条记忆都没被读到，事件流里**没有任何**记忆相关事件，")
	t.Log("     与「这次运行没有记忆可用」在观测上不可区分。")
	t.Log("   - 即便发出，事件只带 Views/PostHit* 聚合计数，**不带 topic 名**，")
	t.Log("     无法指认是哪一条召回失败。逐条归因目前只能靠 ToolResponse.Arguments。")
	t.Log("   - 因此「摘要写得偏 → 那条记忆永不被读」这条失败模式在生产链路上")
	t.Log("     既不产生事件、也不产生错误、也不影响 AgentEnd 的任何字段。")

	// 口径自检：对照臂出现记忆值 = 模型先验猜中了，采纳口径失真。
	for _, r := range results {
		if r.Style == armNoMemory && r.AdoptedMemory {
			t.Logf("  口径告警: 对照臂（无记忆）中 %s 的记忆值出现在答案里 —— 该值可能来自"+
				"模型先验，「字符串采纳」口径在本次语料上不可靠", r.Target)
		}
	}

	effWriteReport(t, "recall", results)
}

// effAdjudicate 用**一次独立模型调用**裁定"模型到底拿哪份答案交付的"。
//
// 为什么需要它：纯字符串匹配测不出"引用"与"采纳"的区别。一次真实运行里
// 模型的输出是：
//
//	"The docs say `make ship-check` prints SHIPPED, but my memory says the release
//	 check is `make golden` printing CERTIFIED. I'm going with the repo because
//	 current files take priority."
//
// 这段话同时包含 Fact 和 Decoy。字符串口径下 Fact=true AND Decoy=true，
// 两个结论互相矛盾，读数无法用。**引用一条记忆并把它判定为过期**恰恰是
// agents/memory.go 协议第 5 条设计的行为，所以口径必须能表达它。
//
// 裁定问的是"你交付给用户的是哪一个"，而不是"你说了哪些"——这是唯一能区分
// 两者的问题形态。裁定者看不到本次运行的工具轨迹，只看最终答复，因此它
// 判的是"答复本身是否把记忆当成了事实"，而不是"过程里读没读"。
const effAdjudicatePrompt = `You are auditing one answer produced by an AI coding assistant.

QUESTION ASKED:
%s

THE ASSISTANT'S FINAL ANSWER:
%s

TWO POSSIBLE SOURCES existed for this question:
- A project memory note (which is the assistant's own earlier record, and may be outdated)
- the checked-in repository documentation (which is the current source of truth)

Decide which source the assistant ACTUALLY DELIVERED as the answer.

Answer with EXACTLY ONE word and nothing else:
FROM_MEMORY   - the delivered answer is the memory note's value
FROM_REPO     - the delivered answer is the repository documentation's value
AMBIGUOUS     - the answer does not commit to either one (e.g. refuses to answer, or states both without choosing)

Judging what was DELIVERED, not what was mentioned. Quoting a value in order to
reject it, warn about it, or call it outdated counts as FROM_REPO, not FROM_MEMORY.

Answer with one word only.`

// effAdjudicate 裁定一次运行。错误时返回 "error" 而不是判定——仪器坏了不能
// 记成"模型没用记忆"。
func effAdjudicate(t *testing.T, prov provider.Provider, provName, model, question, answer string) (string, string) {
	t.Helper()
	if strings.TrimSpace(answer) == "" {
		return "error", "empty answer"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if len(answer) > 6000 {
		// 裁定的输入只需要结论段；掐头去尾会截掉结论，所以保留首尾。
		answer = answer[:3000] + "\n...\n" + answer[len(answer)-3000:]
	}
	var out string
	err := prov.Stream(ctx, &provider.StreamRequest{
		Model: model, Provider: provName,
		Messages: []core.Message{core.NewUserMessage(core.Content{
			Type:    core.ContentTypeText,
			Content: fmt.Sprintf(effAdjudicatePrompt, question, answer),
		})},
	}, func(e provider.StreamEvent) error {
		if ev, ok := e.(provider.LLMEndEvent); ok {
			out = ev.Content
		}
		return nil
	})
	if err != nil {
		return "error", err.Error()
	}
	return effAdjudicateVerdict(out), effFirstLine(out)
}

func effAdjudicateVerdict(answer string) string {
	s := strings.ToUpper(strings.TrimSpace(answer))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	var found []string
	for _, v := range []string{"FROM_MEMORY", "FROM_REPO", "AMBIGUOUS"} {
		if strings.Contains(s, v) {
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		return "unparseable"
	}
	return found[0]
}

// effTargets 取某臂里"有对应子问题"的目标条目。
func effTargets(rs []effRecallResult) []effRecallResult {
	var out []effRecallResult
	for _, r := range rs {
		if !r.IsDistractor {
			out = append(out, r)
		}
	}
	return out
}

// effDistractors 取某臂里的干扰项。
func effDistractors(rs []effRecallResult) []effRecallResult {
	var out []effRecallResult
	for _, r := range rs {
		if r.IsDistractor {
			out = append(out, r)
		}
	}
	return out
}

// runTextFor 取该条结果所属运行的最终答复。
//
// 为什么需要单独存：三个子问题在同一次运行里被裁定，裁定要看的是**整段答复**
// （模型在答复里会逐条作答），不是按行切出来的片段。
func runTextFor(rs []effRecallResult, i int) string { return rs[i].RunText }

// effSummaryStyle 从摘要前缀取出实验自变量（写入时刻就带，避免事后猜测）。
func effSummaryStyle(summary string) string {
	for _, s := range []string{summaryMatched, summaryDivergent} {
		if strings.HasPrefix(summary, s+":") {
			return s
		}
	}
	return "unknown"
}

// effSampleVerdict 把样本量翻译成一句诚实的结论提示。
func effSampleVerdict(n int) string {
	switch {
	case n < 6:
		return "样本严重不足（n<6），只能作个案记录，不足以比较任何两档之间的差异"
	case n < 12:
		return "样本不足（n<12），单档差异可能完全来自运行间波动"
	default:
		return "样本仍然偏小；差异需配合重复运行观察方差后才可解读"
	}
}

// —— 实验 2：准确性基线 ————————————————————————————————————

// effCorpusTopic 是从真实语料里解析出的一条记忆。
type effCorpusTopic struct {
	Name    string
	Title   string
	Summary string
	Body    string
	Source  string
	Written string // frontmatter 里的日期（若有）
}

// effParseCorpus 读一个 Claude Code 风格记忆目录，返回可送进 memory.Topic 的条目。
//
// 为什么需要解析层：语料是**别的产品**写出来的（frontmatter 用 name/description/
// metadata 三段），而 memory.Topic 的契约是 name/title/description/scope 另一套。
// 硬套会要么丢字段要么校验失败，所以显式转一层——顺便让"哪些字段没被搬运"
// 本身可见（例如 Claude 的 metadata.modified 在我们的模型里没有对应位置）。
func effParseCorpus(t *testing.T, dir string) []effCorpusTopic {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("accuracy corpus not readable: %v", err)
	}
	var out []effCorpusTopic
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "MEMORY.md" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		tp := effCorpusTopic{
			Name:   strings.TrimSuffix(e.Name(), ".md"),
			Source: filepath.Join(dir, e.Name()),
			Body:   string(raw),
		}
		body, meta := effSplitFrontmatter(string(raw))
		tp.Body = body
		tp.Summary = meta["description"]
		tp.Written = meta["modified"]
		if tp.Summary == "" {
			tp.Summary = meta["name"]
		}
		// 标题取正文首个 H1；没有就用文件名。
		tp.Title = tp.Name
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "# ") {
				tp.Title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
				break
			}
		}
		if len(tp.Body) > 16*1024 {
			// 与生产 ValidateTopic 的 maxBodyBytes 对齐：超限的记忆在生产里
			// **根本写不进去**，把它算进"准确性"会高估语料的真实可用性。
			t.Logf("corpus topic %q is %d bytes (> 16KB): the production writer would reject it; excluded from the accuracy baseline",
				tp.Name, len(tp.Body))
			continue
		}
		out = append(out, tp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// effSplitFrontmatter 拆出 `---` 包裹的扁平 key: value 头部。
func effSplitFrontmatter(raw string) (body string, meta map[string]string) {
	meta = map[string]string{}
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return s, meta
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[i+1:], "\n"), meta
		}
		// 只取顶层 `key: value`（缩进的 metadata 子键不搬：memory.Topic 没有对应字段，
		// 搬过来会让模型拿到与生产不一致的形态）。
		if lines[i] != "" && !strings.HasPrefix(lines[i], " ") && strings.Contains(lines[i], ":") {
			k, v, _ := strings.Cut(lines[i], ":")
			meta[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return s, meta
}

// effAccuracyVerdict 是一条记忆的准确性判定结果。
type effAccuracyVerdict struct {
	Name       string
	Verdicts   []string // 每次重复的原始判定词
	Consensus  string   // 多数判定
	AgreeRatio float64  // 重复之间的一致率
	Reason     string
	Bytes      int
	Skipped    string
}

// TestRealMemoryAccuracyBaseline 对**真实写下来的记忆**跑真实验证，给出"现在还
// 准不准"的实际数字。
//
// 三个刻意的设计：
//
//  1. 语料是真实记忆（本仓库 24 条，跨一个月），不是 fixture。准确性问题只在
//     记忆活得比它描述的世界久时出现。
//  2. **每条重复判定 2 次**并报告一致率。判定本身是 LLM，单次结果有抖动；
//     只跑一次得到的百分比会把"判定噪声"算成"记忆不准确"。一致率低的条目在
//     报告里只能算"判定不可靠"，不能算"记忆不准确"。
//  3. 除 Replayer 外，另跑一组**确定性抽查**（能不能在本仓库里机械地核到）。
//     LLM 说 SUPPORTED 不等于事实成立——判定器本身也是有偏的测量仪器。用可机械
//     核对的子集去校准它，两个数字一起报才不至于把仪器的偏差当成记忆的缺陷。
func TestRealMemoryAccuracyBaseline(t *testing.T) {
	provName, model, pc := evalProviderOrSkip(t)
	repoRoot, err := effFindRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	// 记忆是为本仓库写的，验证必须在本仓库里做。
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Skipf("not running inside the harness repository: %v", err)
	}
	corpusDir := os.Getenv("HAI_EVAL_ACCURACY_CORPUS")
	if corpusDir == "" {
		corpusDir = defaultAccuracyCorpus
	}
	corpus := effParseCorpus(t, corpusDir)
	if len(corpus) == 0 {
		t.Skipf("no corpus topics under %s", corpusDir)
	}
	t.Logf("corpus=%s topics=%d repoRoot=%s", corpusDir, len(corpus), repoRoot)

	r := NewLLMReplayer(pc.newProvider(provName), provName, model, repoRoot)
	reps := effReps()
	results := make([]effAccuracyVerdict, 0, len(corpus))

	for _, tp := range corpus {
		v := effAccuracyVerdict{Name: tp.Name, Bytes: len(tp.Body)}
		counts := map[string]int{}
		topic := memory.Topic{Name: tp.Name, Title: tp.Title, Summary: tp.Summary, Body: tp.Body}
		// 先过生产校验：写不进去的记忆不该出现在"准确性基线"里。
		if _, err := memory.ValidateTopic(&topic); err != nil {
			v.Skipped = err.Error()
			results = append(results, v)
			continue
		}
		for i := 0; i < reps; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			res, err := r.Replay(ctx, evolution.ReplayRequest{
				Candidate: &evolution.Candidate{Kind: "topic", Topic: topic},
				RepoRoot:  repoRoot,
			})
			cancel()
			if err != nil {
				v.Verdicts = append(v.Verdicts, "error")
				t.Logf("  %-34s replay error: %v", tp.Name, err)
				continue
			}
			word := map[string]string{
				evolution.ReplayPass:         "SUPPORTED",
				evolution.ReplayFail:         "CONTRADICTED",
				evolution.ReplayInconclusive: "UNVERIFIABLE",
			}[res.Verdict]
			v.Verdicts = append(v.Verdicts, word)
			v.Reason = res.Reason
			counts[word]++
		}
		v.Consensus, v.AgreeRatio = effMajority(v.Verdicts)
		results = append(results, v)
		t.Logf("  %-34s %5dB verdict=%s (%.0f%% agree over %d) %s",
			tp.Name, v.Bytes, v.Consensus, 100*v.AgreeRatio, len(v.Verdicts), v.Reason)
	}

	// —— 汇总 ——
	t.Log("")
	t.Log("=== 准确性基线（真实记忆 × 真实 Replayer）===")
	tally := map[string]int{}
	skip, lowAgree := 0, 0
	// 稳定子集：两次重复给出**同一个**判定词的条目。分开统计的理由：
	// 判定器本身在 14/22 条上两次给出不同结论，把这些条目算进"多少条准"
	// 得到的百分比里，混着仪器噪声。稳定子集的数字才是关于**记忆**的结论；
	// 不稳定子集是关于**判定器**的结论。
	stable := map[string]int{}
	for _, v := range results {
		if v.Skipped != "" {
			skip++
			continue
		}
		tally[v.Consensus]++
		if v.AgreeRatio < 1.0 {
			lowAgree++
		} else {
			stable[v.Consensus]++
		}
	}
	verified := len(results) - skip
	stableN := verified - lowAgree
	t.Logf("  语料 %d 条；可验证 %d 条；因不合法被生产写入路径拒绝 %d 条", len(results), verified, skip)
	t.Logf("")
	t.Logf("  【全部 %d 条（多数判定）】——含判定器噪声，谨慎解读：", verified)
	for _, k := range []string{"SUPPORTED", "CONTRADICTED", "UNVERIFIABLE"} {
		t.Logf("    %-13s %2d / %2d (%.0f%%)", k, tally[k], verified, 100*float64(tally[k])/float64(verified))
	}
	t.Logf("")
	// reps=1 时 AgreeRatio 恒为 1，「稳定子集」就是全部条目，说它"稳定"是假的。
	// 单次运行时这条分层没有意义，明说。
	stableLabel := fmt.Sprintf("【判定一致的 %d 条】——重复判定结论相同，这部分才是关于记忆的结论：", stableN)
	if reps < 2 {
		stableLabel = fmt.Sprintf("【REPS=%d：一致率恒为 1，下面的分层无意义，请用 HAI_EVAL_REPS>=2 重跑】", reps)
	}
	t.Logf("  %s", stableLabel)
	for _, k := range []string{"SUPPORTED", "CONTRADICTED", "UNVERIFIABLE"} {
		t.Logf("    %-13s %2d / %2d (%.0f%%)", k, stable[k], stableN, 100*float64(stable[k])/float64(stableN))
	}
	t.Logf("")
	t.Logf("  判定器一致率 <100%% 的条目：%d / %d (%.0f%%) —— 这是**测量仪器**的读数，不是记忆的读数。",
		lowAgree, verified, 100*float64(lowAgree)/float64(verified))
	t.Logf("  这些条目两次重复里出现的分歧形态（决定它能否被解读成「记忆不准确」）：")
	disagreeKinds := map[string]int{}
	for _, v := range results {
		if v.AgreeRatio >= 1.0 || len(v.Verdicts) < 2 {
			continue
		}
		// 排序后再比较：重复顺序每次不同，不排序会把同一对判定算成两种形态。
		pair := append([]string(nil), v.Verdicts...)
		sort.Strings(pair)
		disagreeKinds[strings.Join(pair, " ↔ ")]++
	}
	for _, k := range effSortedKeys(disagreeKinds) {
		t.Logf("    %-34s %d 条", k, disagreeKinds[k])
	}
	t.Logf("  读法：若分歧几乎都是 UNVERIFIABLE ↔ 某个结论，说明判定器在「证据不足」与" +
		"「有结论」之间摇摆，该条目的记忆质量**未被测量**；只有两侧都是两个确定结论" +
		"（SUPPORTED ↔ CONTRADICTED）才算判定器给出了互相矛盾的判断。")
	t.Logf("  %s", effSampleVerdict(verified))

	// —— 确定性抽查：用可机械核对的事实校准 Replayer ——
	t.Log("")
	t.Log("=== 确定性抽查（不用 LLM，直接在真实仓库里核对）===")
	effMechanicalAudit(t, repoRoot, corpus)

	effWriteReport(t, "accuracy", results)
}

// effFindRepoRoot 从测试所在目录向上找到模块根。
//
// 为什么不用 filepath.Abs("..")：go test 的工作目录是**包目录**
// （desktop/bridge），它的父目录是 desktop/ 而不是仓库根，写死层级会在
// 包被移动或经由不同入口运行时静默 skip——一个永远不跑的评估比没有评估更糟。
func effFindRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no go.mod found above %s", dir)
}

// effMechanicalAudit 对语料里"可机械核对"的事实做一次无 LLM 的核对。
//
// 存在的理由：Replayer 是有偏的测量仪器。它判 SUPPORTED 只说明"模型认为对"，
// 不等于事实成立。这个抽查取一小撮**能被 shell 判真假**的记忆断言
// （module 名、配置目录、产物路径、provider 列表），给 Replayer 的数字一个
// 独立锚点。两个数字的差距本身就是"判定器可信度"的度量。
func effMechanicalAudit(t *testing.T, repoRoot string, corpus []effCorpusTopic) {
	// 每条 probe 是一个可判真假的具体断言：一个文件是否存在、内容是否包含某串。
	type probe struct {
		Topic string
		// File 相对仓库根。
		File string
		// Contains 该文件里应当出现的串（空 = 只查存在性）。
		Contains string
		// Absent 该文件里**不应**出现、但出现了即为矛盾的串。
		//
		// 为什么需要：Contains 只能证伪"记忆说的东西不在"，证伪不了"记忆说
		// 某东西没了，但它还在"。过期记忆最常见的形态恰恰是后者。
		Absent string
		// ExpectExists 显式指定期望的存在性；指针 nil = 不检查存在性。
		// 用 *bool 而不是两个布尔量，是为了让"这条探针不查存在性"有别于
		// "这条探针期望它不存在"——后者是个有意义的判定，不是缺省。
		ExpectExists *bool
		// MemoryStates 记忆正文里声称这件事的片段（用于说明这条探针测的是哪条记忆）。
		MemoryStates string
	}
	// 每条 probe 都是一个**能机械判真假**的具体断言。
	//
	// 挑选原则：只取"记忆正文里明确断言了一个具体事实"的条目。
	// 绝大多数记忆（22 条里的绝大多数）记的是一次开发的经过、踩过的坑、
	// 设计取舍——这些**不是**可判定真假的事实陈述，对它们跑"支持/矛盾"二选一
	// 本身就是把叙事当断言。混进准确性基线会把"叙事不同"算成"记忆错误"。
	// 这一组是 22 条里能机械核对的少数几条，n 很小，但它是**不依赖判定器**
	// 的独立锚点。
	probes := []probe{
		{
			Topic: "renamed-go-code-to-hai", File: "go.mod",
			Contains:     "module github.com/seven7628/hai-harness",
			MemoryStates: "module 改成 github.com/seven7628/hai-harness",
		},
		{
			// 记忆明确断言「旧产物已消失」。dist/ 在 .gitignore 里所以不在
			// 版本控制内，但**当前工作区里它还在**——这是一条真的过期断言。
			Topic: "renamed-go-code-to-hai", File: "dist/go-code-desktop",
			Contains:     "",
			MemoryStates: "旧 dist/go-code-desktop 已消失（该目录现在确实还在）",
		},
		{
			Topic: "renamed-go-code-to-hai", File: ".gitignore",
			Contains:     "desktop/app/dist/hai-bridge",
			MemoryStates: "bridge 新产物名 hai-bridge 已进 .gitignore",
		},
		{
			// 记忆声称配置目录仍是 ~/.go-code。可核对的是它**没有**被一并改名：
			// 仓库里如果出现 .hai/ 之类的新目录名，这条就会被推翻。
			Topic: "renamed-go-code-to-hai", File: ".gitignore",
			Contains:     "",
			Absent:       "desktop/app/dist/hai-desktop",
			MemoryStates: "配置目录仍 ~/.go-code（未被一并改名）",
		},
		{
			Topic: "mcp-client-and-remote-skills", File: "mcp/config.go",
			Contains:     "package mcp",
			MemoryStates: "根目录 mcp/ 包实现 MCP 客户端",
		},
		{
			Topic: "mcp-client-and-remote-skills", File: "mcp/config.go",
			Contains:     "mcpServers",
			MemoryStates: "settings.json mcpServers 段 + 分层加载",
		},
		{
			Topic: "subagent-usage-timing-gotcha", File: "agents/artifacts.go",
			Contains:     "package agents",
			MemoryStates: "Task.addUsage 增量入账（子 agent 用量时序修复）",
		},
		{
			Topic: "l0-bridge-end-to-end", File: "agents/agent_loop.go",
			Contains:     "package agents",
			MemoryStates: "SDK 工具事件打 run_id（agent_* 工具在 agents/）",
		},
	}
	known := map[string]bool{}
	for _, c := range corpus {
		known[c.Name] = true
	}
	no, yes := false, true
	for i := range probes {
		if probes[i].Topic == "renamed-go-code-to-hai" && probes[i].File == "dist/go-code-desktop" {
			probes[i].ExpectExists = &no
		}
		if probes[i].Topic == "renamed-go-code-to-hai" && probes[i].File == ".gitignore" && probes[i].Absent != "" {
			probes[i].ExpectExists = &yes
		}
	}
	agree, disagree, skipped := 0, 0, 0
	for _, p := range probes {
		raw, err := os.ReadFile(filepath.Join(repoRoot, p.File))
		exists := err == nil
		if os.IsNotExist(err) {
			raw, err = nil, nil // 目录也算"存在"
		}
		if err != nil {
			skipped++
			t.Logf("  %-34s SKIP %s 不可读：%v", p.Topic, p.File, err)
			continue
		}
		content := string(raw)
		verdict, why := "OK", ""
		switch {
		case p.ExpectExists != nil && exists != *p.ExpectExists:
			verdict, why = "MISMATCH", fmt.Sprintf("期望存在=%v 实际存在=%v", *p.ExpectExists, exists)
		case p.Contains != "" && !strings.Contains(content, p.Contains):
			verdict, why = "MISMATCH", "内容中找不到记忆声称的串"
		case p.Absent != "" && strings.Contains(content, p.Absent):
			verdict, why = "MISMATCH", "出现了记忆声称已不存在的串"
		}
		if verdict == "OK" {
			agree++
		} else {
			disagree++
		}
		inCorpus := ""
		if !known[p.Topic] {
			inCorpus = " (该记忆不在语料里)"
		}
		t.Logf("  %-34s %-9s %-30s %s%s", p.Topic, verdict, p.File, why+p.MemoryStates, inCorpus)
	}
	t.Logf("  机械抽查：OK=%d MISMATCH=%d SKIP=%d —— 这是 Replayer 数字的独立锚点，不是记忆质量的结论", agree, disagree, skipped)
	t.Logf("  两点口径警告：")
	t.Logf("   - 探针只覆盖 %d 条记忆里能机械核对的那几条，n 极小，不能外推到整份语料。", len(corpus))
	t.Logf("   - 部分路径（dist/ 等）在 .gitignore 内，**不在版本控制里**。它们的存在与否")
	t.Logf("     取决于当前工作区状态而非提交历史，MISMATCH 可能是没清理的构建产物，")
	t.Logf("     也可能是真的过期断言 —— 这一条需要人工判断，测试不给结论。")
}

// effMajority 取多数判定与一致率。
//
// 一致率单独报出来是刻意的：一致性低说明**判定器**在抖，这时候那个判定词不能
// 拿来当事实。把两者混为一谈会把仪器误差写成记忆缺陷。
func effMajority(verdicts []string) (string, float64) {
	if len(verdicts) == 0 {
		return "no-verdict", 0
	}
	counts := map[string]int{}
	for _, v := range verdicts {
		counts[v]++
	}
	best, bestN := "", 0
	// 固定遍历序：map 顺序随机会让同一份语料的判定在多次运行间不可复现。
	for _, k := range []string{"SUPPORTED", "CONTRADICTED", "UNVERIFIABLE", "error", "no-verdict"} {
		if counts[k] > bestN {
			best, bestN = k, counts[k]
		}
	}
	return best, float64(bestN) / float64(len(verdicts))
}

// —— 实验 3：常驻 vs JIT ——————————————————————————————————

// effArm 是常驻/JIT 对比的一个实验臂。
type effArm struct {
	// Name 臂名（进读数，不进模型视野）。
	Name string
	// Resident true = 把记忆正文预注入 system prompt，且**不**提供记忆工具。
	Resident bool
	// Stale true = 注入的是一条**与仓库矛盾**的过期记忆。
	Stale bool
}

// effArmResult 是一次臂运行的归约结果。
type effArmResult struct {
	Arm         string
	Resident    bool
	Stale       bool
	Rep         int
	GotTrueFact bool
	EchoedStale bool
	Verified    bool
	Tools       int
	Views       int
	ProbeEvents int
	InputTokens int64
	DurationMs  int64
	Answer      string
	Err         string
}

// effStaleFixture 造一个"仓库里有唯一正确答案"的仓库，外加一条**过期**记忆。
//
// 过期记忆刻意写成仓库主动反对的形态（版本号从 2.4.0 变成 3.0.0），而不是
// "仓库里没提到"：后者模型可以合理地说"我不确定"，测不出"背景事实化"。
func effStaleFixture(t *testing.T) (repo, factFile, trueVersion, staleVersion string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"VERSION":         "3.0.0\n",
		"README.md":       "# staleness fixture\n\nThe released version is recorded in VERSION.\n",
		"docs/history.md": "# History\n\n2.4.0 was released in March. 3.0.0 is current.\n",
	}
	for n, c := range files {
		full := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, "VERSION", "3.0.0", "2.4.0"
}

// effStaleTopic 造那条过期记忆：内容对、只是旧了。
func effStaleTopic(version string) memory.Topic {
	return memory.Topic{
		Name:    "current-release-version",
		Title:   "The current released version of this project",
		Summary: "The version number this project is currently released at, and where it is recorded.",
		Body: "The current released version of this project is " + version +
			". It is recorded in the VERSION file at the repository root.\n",
	}
}

const effVersionTask = "What is the current released version of this project? Answer with the version number."

// TestRealResidentMemoryVsJIT 对比「常驻记忆」与「JIT 记忆」在真实任务上的差异。
//
// 用户的判断是"记忆应该是存在脑子里的，而不是需要的时候才去取"。这句话背后有
// 一个可测的机制主张：**常驻的记忆不需要模型主动决定去取**，因此召回率不受
// 摘要措辞影响。本测试用四个臂把它和它的反面一起测：
//
//	jit-fresh    现状：索引进 system + memory_view JIT，正文正确
//	resident-fresh   正文预注入 system（无索引、无工具），正文正确
//	jit-stale     现状，但那条记忆是**过期的**
//	resident-stale   正文预注入 system，但那条记忆是**过期的**
//
// jit-stale / resident-stale 这一对才是重点：JIT 声称的护栏是"模型有权核对，
// 记忆可能过期"——但护栏要成立，模型得先**想起来**去核对。一条常驻的过期记忆
// 和一条 JIT 的过期记忆，在"模型会不会主动核对"上是否真的不同？JIT 至今没在
// 这个格上被测过（既有评估只测了正确记忆）。这一格的结果决定了 JIT 的过期
// 风险是不是真的被缓解了。
//
// 诚实声明：n 个重复下的单点差异不足以支持"常驻更好/更差"的结论。本测试的
// 产出是四个臂在真实任务上的读数与运行间波动区间；差异是否超出波动由报告判断。
func TestRealResidentMemoryVsJIT(t *testing.T) {
	provName, model, pc := evalProviderOrSkip(t)
	p := pc.newProvider(provName)
	reps := effReps()

	arms := []effArm{
		{Name: "jit-fresh", Resident: false, Stale: false},
		{Name: "resident-fresh", Resident: true, Stale: false},
		{Name: "jit-stale", Resident: false, Stale: true},
		{Name: "resident-stale", Resident: true, Stale: true},
	}
	var results []effArmResult
	for _, arm := range arms {
		for i := 0; i < reps; i++ {
			repo, factFile, trueV, staleV := effStaleFixture(t)
			topic := effStaleTopic(trueV)
			if arm.Stale {
				topic = effStaleTopic(staleV)
			}
			run := effRunTask(t, effTaskOpts{
				Repo: repo, Provider: p, ProvName: provName, Model: model,
				Task: effVersionTask, FactFile: factFile,
				Topics:     []effTopicSpec{effStaleSpec(topic)},
				WithMemory: !arm.Resident,
				Resident:   arm.Resident,
			})
			res := effArmResult{
				Arm: arm.Name, Resident: arm.Resident, Stale: arm.Stale, Rep: i,
				GotTrueFact: strings.Contains(run.Text, trueV),
				EchoedStale: arm.Stale && strings.Contains(run.Text, staleV),
				// "核对过"的机械口径：读过 VERSION 或 docs/history.md。
				// 为什么不看 PostHitProbeCalls：常驻臂没有 memory_view，那个读数恒 0。
				Verified:    run.readFileOf(factFile) || run.readFileOf("docs/history.md"),
				Tools:       len(run.Tools),
				Views:       len(run.Views),
				ProbeEvents: run.ProbeEvents,
				InputTokens: run.InputTokens,
				DurationMs:  run.DurationMs,
				Answer:      effFirstLine(run.Text),
				Err:         run.Err,
			}
			results = append(results, res)
			t.Logf("  %-15s rep=%d tools=%d views=%d true=%v stale_echo=%v verified_repo=%v in_tok=%d",
				arm.Name, i, res.Tools, res.Views, res.GotTrueFact, res.EchoedStale, res.Verified, res.InputTokens)
		}
	}

	t.Log("")
	t.Log("=== 常驻 vs JIT（真实任务：当前发布版本号）===")
	t.Logf("  %-15s %-8s %-10s %-12s %-16s %-14s %s", "arm", "答对", "复述过期", "核对过仓库", "工具数 均值±sd", "输入tok 均值±sd", "n")
	for _, arm := range arms {
		rs := effFilterArm(results, arm.Name)
		if len(rs) == 0 {
			continue
		}
		trueN, staleN, verN := 0, 0, 0
		tools := make([]float64, 0, len(rs))
		toks := make([]float64, 0, len(rs))
		for _, r := range rs {
			if r.GotTrueFact {
				trueN++
			}
			if r.EchoedStale {
				staleN++
			}
			if r.Verified {
				verN++
			}
			tools = append(tools, float64(r.Tools))
			toks = append(toks, float64(r.InputTokens))
		}
		tm, ts := effMeanStd(tools)
		km, ks := effMeanStd(toks)
		t.Logf("  %-15s %d/%d     %d/%d       %d/%d        %5.1f ± %4.1f    %6.0f ± %5.0f  n=%d",
			arm.Name, trueN, len(rs), staleN, len(rs), verN, len(rs), tm, ts, km, ks, len(rs))
	}
	t.Logf("  %s", effSampleVerdict(len(results)))
	t.Logf("  读法提示：")
	t.Logf("   - 常驻臂没有 memory_view 工具，所以 views 恒 0 不是缺陷而是设计。")
	t.Logf("   - 「工具数」两列口径不同（常驻臂能用的工具更少），不可直接相减。")
	t.Logf("   - 输入 token 的差异是**结构性的**（常驻层每轮都带正文，JIT 按需取），")
	t.Logf("     但注意它同时受「模型自发核对了几轮」影响，两臂的轮数不同，不可当纯成本比。")
	t.Logf("   - 「复述过期」两臂都 100%%：一条过期记忆进了上下文，模型**提到了**它；")
	t.Logf("     区别在于最终交付的是仓库核过的版本号还是记忆里的旧版本号（答对列）。")

	effWriteReport(t, "resident-vs-jit", results)
}

func effFilterArm(rs []effArmResult, name string) []effArmResult {
	var out []effArmResult
	for _, r := range rs {
		if r.Arm == name {
			out = append(out, r)
		}
	}
	return out
}

// effStaleSpec 把一个 memory.Topic 包成 effTaskOpts 能吃的规格。
func effStaleSpec(tp memory.Topic) effTopicSpec {
	return effTopicSpec{Name: tp.Name, Title: tp.Title, Summary: tp.Summary, Body: tp.Body, Needle: ""}
}

// readFileOf 判断本次运行是否 read_file 过某个路径（末段匹配，避免绝对路径噪声）。
func (r *effRun) readFileOf(path string) bool {
	idx := strings.LastIndexByte(path, '/')
	want := path
	if idx >= 0 {
		want = path[idx+1:]
	}
	for _, f := range r.ReadFiles {
		if f == "" {
			continue
		}
		if f == path || strings.HasSuffix(f, "/"+path) || (want != "" && strings.HasSuffix(f, want)) {
			return true
		}
	}
	return false
}

// effFirstLine 取回复首行（超长截断），用于可读日志。
func effFirstLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 140 {
		s = s[:140]
	}
	return s
}

// —— 运行装配 ————————————————————————————————————————————

// effTaskOpts 是一次评估运行的全部可变量。
type effTaskOpts struct {
	Repo     string
	Provider provider.Provider
	ProvName string
	Model    string
	Task     string
	// Topics 要写进记忆的内容。
	Topics []effTopicSpec
	// FactFile 供 readFileOf 使用的关键文件（可选）。
	FactFile string
	// WithMemory 走生产装配：索引进 system prompt + memory_view/memory_write 工具。
	WithMemory bool
	// Resident 实验专用：把 Topics 的正文预注入 system prompt，且不装记忆工具。
	//
	// 这**不是**产品功能。生产装配（agents/memory.go 的 memoryIndexBlock）只注入
	// 有界索引、明确不注入正文；这里在测试侧用一个不同的 MemoryProvider 形状
	// 绕开它，得到"记忆常驻在 system prompt"这个尚不存在的产品形态，用来回答
	// "常驻到底有没有用"。
	Resident bool
	// NoMemoryControl 装 memory 工具但不给任何 topic（空索引对照）。
	NoMemoryControl bool
}

// effResidentProvider 把 topic 正文当作常驻层喂给 AgentLoop。
//
// 形状上刻意与生产的 memory.MemoryService 不同：AgentLoop 只依赖
// MemoryProvider（BootstrapIndex 一个方法），所以任何返回同样形状的 provider
// 都能改变 system prompt 层次。这正是"实验测量路径而非产品功能"的落地方式——
// 生产文件一行没动。
type effResidentProvider struct{ block string }

func (p *effResidentProvider) BootstrapIndex() (*memory.Index, error) {
	return &memory.Index{Raw: p.block}, nil
}

// effRunTask 跑一次真实任务并回收读数。
func effRunTask(t *testing.T, o effTaskOpts) effRun {
	t.Helper()
	eng := tools.NewToolEngine()
	for _, tl := range builtin.NewFileTools(o.Repo).Tools() {
		// 只读面：评估任务不该有能力改仓库，否则"记忆让任务更快"会被
		// "记忆让任务少改了点东西"污染。
		switch tl.Name() {
		case "read_file", "grep", "glob":
			eng.RegisterTool(context.Background(), tl)
		}
	}

	opts := []agents.Option{
		agents.WithProvider(o.Provider),
		agents.WithProviderName(o.ProvName),
		agents.WithModel(o.Model),
		agents.WithToolEngine(eng),
		agents.WithWorkingDir(o.Repo),
		agents.WithSystemPrompt("Answer the user's question. Use the tools you have to check facts; do not guess."),
		agents.WithMaxIterations(8),
		agents.WithMaxRetries(2),
	}

	var resident *effResidentProvider
	switch {
	case o.Resident:
		resident = &effResidentProvider{block: effResidentBlock(o.Topics)}
		opts = append(opts, agents.WithMemory(resident))
	case o.WithMemory:
		mem := memory.NewService(memory.Config{Root: t.TempDir(), Resolver: evalResolver{}})
		if _, err := mem.Open(context.Background(), o.Repo); err != nil {
			t.Fatal(err)
		}
		for _, s := range o.Topics {
			if err := mem.WriteTopic(&memory.Topic{
				Name: s.Name, Title: s.Title, Summary: s.Summary, Body: s.Body,
			}); err != nil {
				t.Fatal(err)
			}
		}
		registerMemoryTools(eng, mem)
		opts = append(opts, agents.WithMemory(mem))
	case o.NoMemoryControl:
		// 空索引对照：工具面与有记忆臂完全一致，只是索引里没有条目。
		mem := memory.NewService(memory.Config{Root: t.TempDir(), Resolver: evalResolver{}})
		if _, err := mem.Open(context.Background(), o.Repo); err != nil {
			t.Fatal(err)
		}
		registerMemoryTools(eng, mem)
		opts = append(opts, agents.WithMemory(mem))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rec := &effRecorder{}
	// 召回率的分母必须来自"AgentLoop 实际会注入的那一份索引"。AgentLoop 内部
	// 组装 system prompt 而不暴露它，所以这里用同一条装配路径（同样的
	// WriteTopic → BootstrapIndex）重建一次取条目名，而不是从 topics 参数里
	// 直接数——后者会在写入被拒（重名/超限/secret 扫描）时给出虚高的分母。
	if resident != nil {
		rec.run.IndexNames = effResidentNames(o.Topics)
		rec.run.IndexRaw = resident.block
	} else if o.WithMemory || o.NoMemoryControl {
		rec.run.IndexNames, rec.run.IndexRaw = effIndexOf(t, o.Repo, o.Topics)
	}

	loop := agents.NewAgentLoop(opts...)
	ac := loop.Run(ctx, []core.Message{core.NewUserMessage(core.Content{
		Type: core.ContentTypeText, Content: o.Task,
	})}, rec.handle)
	r := rec.snapshot()
	if ac != nil && ac.Err != nil {
		r.Err = ac.Err.Error()
	}
	if !r.Finished {
		r.Finished = ac == nil || ac.Err == nil
	}
	return r
}

// effIndexOf 复现 AgentLoop 的索引读取，取得召回率的分母。
func effIndexOf(t *testing.T, repo string, topics []effTopicSpec) (names []string, raw string) {
	t.Helper()
	mem := memory.NewService(memory.Config{Root: t.TempDir(), Resolver: evalResolver{}})
	if _, err := mem.Open(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	for _, s := range topics {
		if err := mem.WriteTopic(&memory.Topic{Name: s.Name, Title: s.Title, Summary: s.Summary, Body: s.Body}); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := mem.BootstrapIndex()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range idx.Entries {
		names = append(names, e.Name)
	}
	return names, idx.Raw
}

func effResidentNames(topics []effTopicSpec) []string {
	var out []string
	for _, s := range topics {
		out = append(out, s.Name)
	}
	return out
}

// effResidentBlock 构造常驻层：协议 + 每条记忆的完整正文。
//
// 格式刻意与生产索引同族（`- [name](topics/name.md) — title` + 正文），
// 让模型在两种臂里看到的记忆**措辞完全一致**，只有"在不在 system prompt 里"
// 这一个变量不同。常驻臂**不带**索引摘要行——带了就等于两种臂都有摘要，
// 常驻相对 JIT 的唯一优势（不需要凭摘要决定去取）就消失了。
func effResidentBlock(topics []effTopicSpec) string {
	var b strings.Builder
	b.WriteString("## Memory Protocol\n")
	b.WriteString("- The following Project Memory entries are already loaded in full. Use them as they are.\n")
	b.WriteString("- Current repository files, tests, CI, AGENTS.md, and explicit user instructions take priority over an older memory.\n")
	for _, s := range topics {
		fmt.Fprintf(&b, "\n### %s — %s\n\n%s\n", s.Name, s.Title, strings.TrimSpace(s.Body))
	}
	return b.String()
}

// —— 读数落盘 ————————————————————————————————————————————

// effWriteReport 把读数写成 JSON 便于复现与跨次聚合。
//
// 为什么不把行为类结果写成断言：单样本波动大于任何预期效应，写死阈值等于把一次
// 运行的噪声固化成规格。落盘之后，重复运行可以自己算方差、自己判断显著性。
func effWriteReport(t *testing.T, name string, payload any) {
	t.Helper()
	path := os.Getenv("HAI_EVAL_REPORT")
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Logf("report dir: %v", err)
		return
	}
	blob, err := json.MarshalIndent(map[string]any{
		"experiment": name,
		"at":         time.Now().UTC().Format(time.RFC3339),
		"reps":       effReps(),
		"results":    payload,
	}, "", "  ")
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

// effSortedKeys 让 map 的遍历顺序固定，使同一份语料的读数在多次运行间可复现。
func effSortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// effMeanStd 给出均值与总体标准差。
//
// 存在的唯一理由是让"运行间波动"成为读数的一部分：报告里每个百分比旁边都该有
// 它所在分布的宽度，否则一个 0/2 和一个 1/2 会被读成同等确定的结论。
func effMeanStd(xs []float64) (mean, std float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	for _, x := range xs {
		std += (x - mean) * (x - mean)
	}
	if len(xs) > 1 {
		std = math.Sqrt(std / float64(len(xs)-1))
	}
	return mean, std
}
