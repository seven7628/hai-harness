package main

// memory_supplement_test.go —— Memory Step 0 补充评测之一：把 B vs A 测准。
//
// 背景（Step 0 已测出，不要重复）：
//
//	A 冲突注入   81%   工具 7.86  文档 4.39
//	B 指针注入   92%   工具 6.64  文档 3.19
//	C 无注入     89%   工具 6.83  文档 4.50
//	B−A = +11pp，标准差 13pp → 差异在噪声内
//
// Step 0 用的是**非配对**的读法（比较两个独立比例），而这个实验的设计其实**是配对的**：
// 每一轮重复里，A 与 B 跑在同一份 fixture、同一句提问、同一套工具面上。这允许一个
// 远比"两个独立比例相减"更有力的分析 —— **McNemar（不一致对）**：
//
//	A 对 B 错的对数 b01，与 A 错 B 对的对数 b10
//
// 只有这两个数携带信息。一致对（A 对 B 也对 / 都错）在两个臂上贡献同样的信息，
// 配对分析把它们消掉，于是有效样本量取决于 discordant pairs 而不是 n。
// Step 0 报 "+11pp vs 标准差 13pp" 时丢掉的正是这部分信息 —— n=36 里 discordant
// pairs 可能只有个位数，非配对读法会把它们稀释成"噪声"。
//
// 第二个改进：**交错执行**（interleaved）。Step 0 是 A 跑完 36 次再跑 B。网关在一天内
// 的行为漂移（限流、模型后端换权重、上游超时）会被**整段**记到某一臂头上。本文件
// 每一轮先跑 A 再跑 B（HAI_EVAL_SUPP_SWAP=1 则先 B 后 A，用来检验顺序效应），
// 于是漂移在两臂间均摊。
//
// 纪律与 Step 0 同源，不新增发明：
//
//   - 空状态硬断言（baselineEnforceEmptyMemoryState）：不得有生产文件 import memory。
//   - 判定器是**另一个模型**，每答判两次且候选顺序对调，自洽率 <90% 则全部结论作废。
//   - 不用"答案含关键词"判定。
//   - n 有下限（本文件 HAI_EVAL_SUPP_REPS，地板 30 —— 这是为了把一个已知落在噪声
//     里的差值测准；低于 30 就直接响亮失败而不是静默接受）。
//   - 数字难看照样报。若 B≈A，结论就是"数据不足以区分"。
//
// -short 跳过；缺凭证 skip（绝不假失败）。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
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

const (
	supplementProtocolVersion = "memory-eval/3.1-arm-baseline/supplement-1/v1"
	supplementTaskSet         = "cache-backend/1" // 与 Step 0 逐字节相同的任务集
	supplementRepsFloor       = 30                // 本文件的地板：低于此不足以"测准"
	// 配对分析在 discordant pairs 为 0 时无法给出 p 值。那不是"无差异"，是"没有
	// 任何一次运行在两臂上给出相反结果"—— 读法完全不同，故单列。
	supplementNoDiscordanceNote = "0 discordant pairs: McNemar 无 p 值可给。这不是「两臂等价」的证据，" +
		"而是「n 次运行里没有一次两臂给出相反结果」。此时唯一能说的是本实验的分辨率上限。"
)

// —— 读数 ——

// supplementRun 一次运行的读数。与 Step 0 同一口径，便于与既有数字并列。
type supplementRun struct {
	Arm         string    `json:"arm"`
	PairIndex   int       `json:"pair_index"`
	OrderInPair string    `json:"order_in_pair"`
	Verdict     string    `json:"verdict"` // TRUE / STALE / NEITHER / ERROR
	SelfAgree   bool      `json:"self_agree"`
	ToolCalls   int       `json:"tool_calls"`
	DocsRead    []string  `json:"docs_read"`
	ReadTruth   bool      `json:"read_truth"`
	ReadDecoy   bool      `json:"read_decoy"`
	DurationSec float64   `json:"duration_sec"`
	Err         string    `json:"err,omitempty"`
	FinalAnswer string    `json:"final_answer"`
	VerdictRaw  [2]string `json:"verdict_raw"`
}

// supplementPair 一个重复轮次里的 A、B 两次运行（配对单位）。
type supplementPair struct {
	Index  int              `json:"index"`
	Runs   [2]supplementRun `json:"runs"`
	Usable bool             `json:"usable"` // 两臂都有可用判定
	AOk    bool             `json:"a_ok"`
	BOk    bool             `json:"b_ok"`
}

// supplementArmStat 一臂汇总。
type supplementArmStat struct {
	Arm         string         `json:"arm"`
	N           int            `json:"n"`
	NErr        int            `json:"n_err"`
	Correct     int            `json:"correct"`
	CorrectRate float64        `json:"correct_rate"`
	WilsonLo    float64        `json:"correct_rate_wilson_lo"`
	WilsonHi    float64        `json:"correct_rate_wilson_hi"`
	ToolMean    float64        `json:"tool_calls_mean"`
	ToolStd     float64        `json:"tool_calls_std"`
	DocsMean    float64        `json:"docs_read_mean"`
	DocsStd     float64        `json:"docs_read_std"`
	Verdicts    map[string]int `json:"verdicts"`
	ReadTruth   int            `json:"read_truth"`
	Seq         string         `json:"correctness_sequence"` // 逐次 0/1，便于事后核对
}

// supplementReport 完整报告（§4 八项 + §10 报告项）。
type supplementReport struct {
	Experiment  string              `json:"experiment"`
	Protocol    string              `json:"protocol_version"`
	RunID       string              `json:"run_id"`
	At          string              `json:"at"`
	Model       string              `json:"model"`
	Provider    string              `json:"provider"`
	JudgeModel  string              `json:"judge_model"`
	JudgeProv   string              `json:"judge_provider"`
	HarnessSnap string              `json:"harness_snapshot"`
	MemorySnap  string              `json:"memory_snapshot"`
	TaskSet     string              `json:"task_set"`
	Budget      string              `json:"budget"`
	Reps        int                 `json:"reps_per_arm"`
	OrderSwap   bool                `json:"order_swap"`
	Arms        []supplementArmStat `json:"arms"`
	Pairs       []supplementPair    `json:"pairs"`
	Analysis    supplementAnalysis  `json:"analysis"`
	JudgeSelf   float64             `json:"judge_self_consistency"`
	JudgeSwap   int                 `json:"judge_order_disagreements"`
	JudgeJudged int                 `json:"judge_judged"`
	Notes       []string            `json:"notes"`
}

// supplementAnalysis 差异分析。配对与非配对**都**给：配对更有力，非配对是为了与
// Step 0 的读法对齐（那份报告用的是非配对）。
type supplementAnalysis struct {
	NAvailable   int     `json:"n_paired_available"`
	Discordant   int     `json:"discordant_pairs"`
	B01          int     `json:"b01_a_right_b_wrong"`
	B10          int     `json:"b10_a_wrong_b_right"`
	McNemarP     float64 `json:"mcnemar_exact_two_sided_p"`
	McNemarValid bool    `json:"mcnemar_p_available"`

	DiffPP     float64 `json:"diff_pp_unpaired"`
	SEUnpaired float64 `json:"se_pp_unpaired_pooled"`
	ZUnpaired  float64 `json:"z_unpaired_pooled"`
	PUnpaired  float64 `json:"p_unpaired_two_sided"`
	CILoPP     float64 `json:"diff_ci95_lo_pp"`
	CIHiPP     float64 `json:"diff_ci95_hi_pp"`

	ToolDiff float64 `json:"tool_calls_diff"`
	DocsDiff float64 `json:"docs_read_diff"`

	// MDE：在观测到的正确率与本样本量下，本实验**能**分辨的最小差值（双侧 α=0.05，
	// power=0.80，配对近似按 discordant 比例折算）。它是"数据不足以区分"这句话的
	// 定量版本 —— 报告里必须带着它，否则"没测出差异"会被读成"没有差异"。
	MDEPairedPP   float64 `json:"mde_paired_pp"`
	MDEUnpairedPP float64 `json:"mde_unpaired_pp"`

	Verdict string `json:"verdict"`
}

// —— 统计 ——
//
// 全部是无外部依赖的闭式实现，并在 memory_supplement_stats_test.go 里对着
// 解析可算的样例做断言。理由：p 值算错是**静默**灾难 —— 数字照样漂亮，只是全错。
// 所以这些函数不与其他测试共用一份"看起来对"的实现。

const (
	supplementZ975 = 1.959963984540054 // 双侧 α=0.05
	supplementZ80  = 0.8416212335729143
)

// supplementNormalCDF 标准正态累积分布（Φ）。
func supplementNormalCDF(x float64) float64 { return 0.5 * (1 + math.Erf(x/math.Sqrt2)) }

// supplementTwoSidedP 已知 z 统计量的双侧 p。
func supplementTwoSidedP(z float64) float64 { return 2 * (1 - supplementNormalCDF(math.Abs(z))) }

// supplementBinomialTailAtMost P(X<=k), X~Bin(n,0.5)。
//
// McNemar 的精确检验：n = discordant 总数，零假设下每个不一致对归属 A 或 B 各半
// （在"两臂边际相等"下这是标准的条件精确检验）。本函数用二项系数前缀和，
// n 在本实验的量级（<60）下完全够用，且不需要组合数表。
func supplementBinomialTailAtMost(n, k int) float64 {
	if n <= 0 {
		return 1
	}
	if k < 0 {
		return 0
	}
	if k > n {
		k = n
	}
	// 递推 C(n,i) = C(n,i-1) * (n-i+1)/i，配合 2^-n 一起算，避免溢出。
	sum := 0.0
	comb := 1.0
	for i := 0; i <= k; i++ {
		if i > 0 {
			comb = comb * float64(n-i+1) / float64(i)
		}
		sum += comb
	}
	return sum / math.Pow(2, float64(n))
}

// supplementMcNemarExact 返回不一致对总数、b01（仅 A 对）、b10（仅 B 对）、
// 双侧精确 p 与 p 是否可报。
//
// 退化情形单独认领：discordant==0 时二项检验给 p=1，那是"没有反例"而不是"无差异"，
// 直接报 1.0 会把分辨率不足包装成零假设被接受。故返回 pAvailable=false。
func supplementMcNemarExact(b01, b10 int) (n int, p float64, ok bool) {
	n = b01 + b10
	if n == 0 {
		return 0, 0, false
	}
	lo := b01
	if b10 < lo {
		lo = b10
	}
	p = 2 * supplementBinomialTailAtMost(n, lo)
	if p > 1 {
		p = 1
	}
	return n, p, true
}

// supplementProportionStats 两比例比较：差值（百分点）、pooled 标准误、z、双侧 p、
// 以及非 pooled（Wilson 型区间的常用近似）的 95% CI。
func supplementProportionStats(x1, n1, x2, n2 int) (diffPP, sePP, z, p, ciLoPP, ciHiPP float64) {
	if n1 == 0 || n2 == 0 {
		return
	}
	p1 := float64(x1) / float64(n1)
	p2 := float64(x2) / float64(n2)
	diff := p2 - p1
	pooled := float64(x1+x2) / float64(n1+n2)
	se := math.Sqrt(pooled * (1 - pooled) * (1/float64(n1) + 1/float64(n2)))
	diffPP = 100 * diff
	sePP = 100 * se
	if se > 0 {
		z = diff / se
		p = supplementTwoSidedP(z)
	} else {
		// 两侧都 100% 或都 0%：se=0，任何检验都没有意义。不假装有 p 值。
		z, p = 0, 1
	}
	// 非 pooled 标准误下的 Wald 区间：pooled 在 p1、p2 都贴近 0 或 1 时覆盖很差。
	seU := math.Sqrt(p1*(1-p1)/float64(n1) + p2*(1-p2)/float64(n2))
	ciLoPP = 100 * (diff - supplementZ975*seU)
	ciHiPP = 100 * (diff + supplementZ975*seU)
	return
}

// supplementWilsonCI 单个比例的 Wilson 区间（不是 0%/100% 时也能给出有限区间）。
func supplementWilsonCI(x, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 0
	}
	p := float64(x) / float64(n)
	z := supplementZ975
	den := 1 + z*z/float64(n)
	center := (p + z*z/(2*float64(n))) / den
	half := z * math.Sqrt(p*(1-p)/float64(n)+z*z/(4*float64(n*n))) / den
	lo, hi = center-half, center+half
	if lo < 0 {
		lo = 0
	}
	if hi > 1 {
		hi = 1
	}
	return
}

// supplementMDEUnpaired 在正确率 p、每臂 n 下，双侧 α=0.05 / power=0.80 能分辨的
// 最小绝对差值（百分点）。给的是"本实验的分辨率"，不是"真实差异"。
func supplementMDEUnpaired(x1, n1, x2, n2 int) float64 {
	if n1 == 0 || n2 == 0 {
		return 0
	}
	p := float64(x1+x2) / float64(n1+n2)
	se := math.Sqrt(p * (1 - p) * (1/float64(n1) + 1/float64(n2)))
	return 100 * (supplementZ975 + supplementZ80) * se
}

// supplementMDEPaired 配对版可分辨差值。
//
// 配对设计的有效样本量由 discordant 比例 d 决定：配对比例之差的标准误约为
// sqrt(d · (1/n1 + 1/n2))（d = 每配对的净不一致概率），所以
//
//	MDE_paired = (z_{1-α/2} + z_{1-β}) · sqrt(d · (1/n1 + 1/n2))
//
// 对比非配对的 sqrt(p(1-p)·(1/n1+1/n2))。d 越小，本实验越钝 —— 而"两臂从不给出
// 相反结果"（d=0）恰恰是最可能的情形之一，那时 MDE 不是 0，而是"无分辨率"：
// 没有反例就什么也测不出来。故 d==0 显式退化成 100（满档），不返回 0。
func supplementMDEPaired(discordant, pairs, n1, n2 int) float64 {
	if pairs == 0 || n1 == 0 || n2 == 0 {
		return 100 // 无配对 → 本设计无法给出任何分辨率读数
	}
	d := float64(discordant) / float64(pairs)
	se := math.Sqrt(d * (1/float64(n1) + 1/float64(n2)))
	if se == 0 {
		return 100 // 零 discordance：分辨率不足，不是分辨率无穷好
	}
	return 100 * (supplementZ975 + supplementZ80) * se
}

// supplementClassifyVerdict 把统计读数翻译成一句可判的结论。阈值写在这里是为了让
// "能不能宣称有差异"成为代码里的一处断言，而不是报告里的一句措辞。
func supplementClassifyVerdict(pUnpaired float64, mdePP float64, pMcNemar float64, mcValid bool) string {
	switch {
	case mcValid && pMcNemar < 0.05:
		return "SIGNIFICANT: 配对检验在 α=0.05 下拒绝「两臂等价」"
	case pUnpaired < 0.05:
		return "SIGNIFICANT (unpaired only): 非配对检验显著但配对检验不显著 —— " +
			"多半是 n1=n2 下的偶然，不足以宣称 B 明显高于 A"
	case mdePP > 15:
		return "INSUFFICIENT: 本实验的可分辨下限是 " + fmt.Sprintf("%.0f", mdePP) +
			" 个百分点，差值必须大于它才可能被观测到。数据不足以区分两臂。"
	default:
		return "NO EFFECT DETECTED: 差值小且本实验分辨得开这个量级 —— " +
			"在本任务集与本模型上，冲突注入与指针注入没有可观测差异。"
	}
}

// —— 装配与运行 ——

// supplementReps 读重复次数。地板是 30：Step 0 已经报出"B−A=+11pp，标准差 13pp"，
// 本文件的全部意义就是把那个 11pp 测准；用 n=6 重跑一遍不叫补充。
func supplementReps(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("HAI_EVAL_SUPP_REPS")
	if raw == "" {
		return supplementRepsFloor
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n <= 0 {
		t.Fatalf("HAI_EVAL_SUPP_REPS=%q 解析失败（要一个正整数）", raw)
	}
	if n < supplementRepsFloor {
		t.Fatalf("HAI_EVAL_SUPP_REPS=%d < 本文件地板 %d。\n"+
			"本文件的目的就是把 Step 0 那个「+11pp vs 标准差 13pp」的差值测准；"+
			"用更小的 n 重跑一遍得到的是同一句话的更弱版本。", n, supplementRepsFloor)
	}
	return n
}

// supplementOrderSwap 是否把配对内的执行顺序对调（先 B 后 A）。默认 A 先跑。
func supplementOrderSwap() bool { return os.Getenv("HAI_EVAL_SUPP_SWAP") == "1" }

// supplementRunOnce 跑一次任务。工具面、预算、提问、fixture 与 Step 0 逐字节相同
// （直接复用 baselineExecFixture / baselineQuestion / baselineRunOnce 的等价体），
// **唯一**区别是这里多记了 PairIndex 与顺序标签。
//
// 为什么不直接调用 baselineRunOnce：它的读数结构是 baselineRun，多一层包一层
// 反而容易在两处漂移。这里显式重写一遍，让"本文件测的是同一台仪器"这件事在代码里
// 可见 —— 两处的工具集注册、预算常量、提问串必须逐字节相同，由
// memory_supplement_wiring_test.go 断言。
func supplementRunOnce(t *testing.T, p provider.Provider, prov, model, root, inject string,
	pairIdx int, arm, order string) supplementRun {

	t.Helper()
	eng := tools.NewToolEngine()
	for _, tl := range builtin.NewFileTools(root).Tools() {
		switch tl.Name() {
		case "read_file", "grep", "glob":
			eng.RegisterTool(context.Background(), tl)
		}
	}

	run := supplementRun{
		Arm: arm, PairIndex: pairIdx, OrderInPair: order,
	}
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
	var mu sync.Mutex
	var final string
	loop := agents.NewAgentLoop(opts...)
	handler := events.EventHandler(func(ctx context.Context, ev events.Event) {
		recHandle(ctx, ev)
		if e, ok := ev.(*events.AgentEnd); ok {
			mu.Lock()
			defer mu.Unlock()
			final = e.Content
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
	run.DocsRead, run.ReadTruth, run.ReadDecoy, _ = rec.snapshot(root)
	return run
}

// —— 测试 ——

// TestSupplementPointerVsConflictPaired 是补测一。
//
// 装置与 Step 0 完全同构（同一 fixture、同一提问、同一只读工具面、同一个判定器），
// 改的是三件事：
//
//  1. n 从 6 提到 30+（HAI_EVAL_SUPP_REPS）；
//  2. 交错执行 —— 每轮 A、B 各跑一次，漂移在两臂间均摊；
//  3. 配对分析 —— McNemar 精确检验 + 不一致对计数 + MDE。
//
// 判据不变：§3.1 的 B 必须明显高于 A。但本文件额外要求"能说得出"而不是"看起来
// 像"：结论字符串由 supplementClassifyVerdict 生成，MDE 一并输出。
func TestSupplementPointerVsConflictPaired(t *testing.T) {
	memSnap := baselineEnforceEmptyMemoryState(t)

	prov, model, p := baselineEnvOrSkip(t)
	jm := baselineJudgeModel()
	pc := loadProviderConfig()
	if pc.baseURL[prov] == "" || strings.TrimSpace(pc.keyLocked(prov)) == "" {
		t.Skipf("判定器复用被测 provider 配置，%q 不可用", prov)
	}
	judgeProv, judgeP := prov, pc.newProvider(prov)
	if jm == model {
		t.Logf("警告：判定器模型与被测模型相同（%s）。§7 要求独立裁定。", jm)
	}
	baselinePing(t, p, prov, model)
	baselinePing(t, judgeP, judgeProv, jm)

	reps := supplementReps(t)
	swap := supplementOrderSwap()
	arms := baselineArms() // A_conflicting_memory / B_pointer_memory（Step 0 同一份定义）

	// 只跑 A 与 B：C 臂在 Step 0 已被证明模型会主动核对，重复它不增加关于
	// "B 是否比 A 安全"的信息。C 臂的 89% 才是本次报告的基准（见报告正文）。
	armA, armB := arms[0], arms[1]

	rep := &supplementReport{
		Experiment:  "supplement-1-pointer-vs-conflict-paired",
		Protocol:    supplementProtocolVersion,
		RunID:       fmt.Sprintf("%d", time.Now().UnixNano()),
		At:          time.Now().UTC().Format(time.RFC3339),
		Model:       model,
		Provider:    prov,
		JudgeModel:  jm,
		JudgeProv:   judgeProv,
		HarnessSnap: baselineGitHEAD(t),
		MemorySnap:  memSnap,
		TaskSet:     supplementTaskSet + " (byte-identical to Step 0)",
		Budget:      "max_iterations=8 max_retries=2 llm_timeout=4m run_timeout=6m read-only tools (read_file/grep/glob)",
		Reps:        reps,
		OrderSwap:   swap,
		Notes: []string{
			"与 Step 0 同构：同一 fixture、同一提问、同一只读工具面、同一个判定器模型与两序对调规则。改的只有 n、交错执行与配对分析。",
			"交错执行：每轮 A、B 各一次，HAI_EVAL_SUPP_SWAP=1 时配对内顺序对调。",
			"配对分析（McNemar 精确）只用不一致对携带的信息；一致对在两臂上贡献相同，已被消掉。",
			"Step 0 的 C 臂（无注入）为 89%：本模型无记忆时已主动核对。**任何「记忆提升多少」都以 89% 为基准，不是 0%。**",
			"discordant=0 时不报 p 值：那是「没有反例」，不是「无差异」。",
		},
	}

	statA := supplementArmStat{Arm: armA.Name, Verdicts: map[string]int{}}
	statB := supplementArmStat{Arm: armB.Name, Verdicts: map[string]int{}}
	var toolsA, toolsB, docsA, docsB []float64
	seqA, seqB := strings.Builder{}, strings.Builder{}
	selfAgree, swapDis, judged := 0, 0, 0

	t.Logf("protocol=%s task_set=%s harness=%s", supplementProtocolVersion, supplementTaskSet, baselineGitHEAD(t))
	t.Logf("provider=%s model=%s | judge=%s/%s | reps/arm=%d | 配对内顺序=%s",
		prov, model, judgeProv, jm, reps, map[bool]string{true: "B→A（已对调）", false: "A→B"}[swap])
	t.Logf("A = %s", armA.DocLabel)
	t.Logf("B = %s", armB.DocLabel)
	t.Logf("")

	for i := 0; i < reps; i++ {
		order := [2]struct {
			arm    string
			inject string
		}{{armA.Name, armA.Inject}, {armB.Name, armB.Inject}}
		if swap {
			order[0], order[1] = order[1], order[0]
		}

		var pair supplementPair
		pair.Index = i
		var verdicts [2]string
		var agrees [2]bool

		for k, o := range order {
			root := baselineExecFixture(t)
			run := supplementRunOnce(t, p, prov, model, root, o.inject, i, o.arm,
				fmt.Sprintf("slot%d", k))
			if run.Err != "" || strings.TrimSpace(run.FinalAnswer) == "" {
				run.Verdict = "ERROR"
				pair.Runs[k] = run
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
			verdicts[k], agrees[k] = verdict, agree
			pair.Runs[k] = run
			t.Logf("  pair#%-2d %-20s verdict=%-7s selfagree=%-5v tools=%2d docs=%d | %.140s",
				i, o.arm, verdict, agree, run.ToolCalls, len(run.DocsRead),
				strings.ReplaceAll(run.FinalAnswer, "\n", " "))
		}

		// 只有两臂都判成功才进配对统计：缺失一半的配对不能被当成"不一致"。
		usable := verdicts[0] != "ERROR" && verdicts[1] != "ERROR" && agrees[0] && agrees[1]
		pair.Usable = usable
		if usable {
			pair.AOk = verdicts[0] == "TRUE"
			pair.BOk = verdicts[1] == "TRUE"
		}
		rep.Pairs = append(rep.Pairs, pair)

		for k, run := range pair.Runs {
			isA := (k == 0) != swap // slot0 在未对调时是 A
			st := &statB
			var toolsAcc, docsAcc *[]float64
			var seq *strings.Builder
			if isA {
				st = &statA
				toolsAcc, docsAcc, seq = &toolsA, &docsA, &seqA
			} else {
				toolsAcc, docsAcc, seq = &toolsB, &docsB, &seqB
			}
			if run.Verdict == "ERROR" {
				st.NErr++
				continue
			}
			st.N++
			st.Verdicts[run.Verdict]++
			if run.Verdict == "TRUE" {
				st.Correct++
				seq.WriteByte('1')
			} else {
				seq.WriteByte('0')
			}
			*toolsAcc = append(*toolsAcc, float64(run.ToolCalls))
			*docsAcc = append(*docsAcc, float64(len(run.DocsRead)))
			if run.ReadTruth {
				st.ReadTruth++
			}
		}
	}

	statA.Seq, statB.Seq = seqA.String(), seqB.String()
	for _, s := range []*supplementArmStat{&statA, &statB} {
		if s.N > 0 {
			s.CorrectRate = float64(s.Correct) / float64(s.N)
		}
		s.WilsonLo, s.WilsonHi = supplementWilsonCI(s.Correct, s.N)
	}
	statA.ToolMean, statA.ToolStd = baselineMeanStd(toolsA)
	statB.ToolMean, statB.ToolStd = baselineMeanStd(toolsB)
	statA.DocsMean, statA.DocsStd = baselineMeanStd(docsA)
	statB.DocsMean, statB.DocsStd = baselineMeanStd(docsB)
	rep.Arms = []supplementArmStat{statA, statB}

	// —— 配对统计 ——
	var usablePairs, b01, b10 int
	for _, pr := range rep.Pairs {
		if !pr.Usable {
			continue
		}
		usablePairs++
		switch {
		case pr.AOk && !pr.BOk:
			b01++
		case !pr.AOk && pr.BOk:
			b10++
		}
	}
	disc, pMc, mcOK := supplementMcNemarExact(b01, b10)
	diffPP, sePP, zUn, pUn, ciLo, ciHi := supplementProportionStats(statA.Correct, statA.N, statB.Correct, statB.N)
	mdeU := supplementMDEUnpaired(statA.Correct, statA.N, statB.Correct, statB.N)
	mdeP := supplementMDEPaired(disc, usablePairs, statA.N, statB.N)

	rep.Analysis = supplementAnalysis{
		NAvailable: usablePairs, Discordant: disc, B01: b01, B10: b10,
		McNemarP: pMc, McNemarValid: mcOK,
		DiffPP: diffPP, SEUnpaired: sePP, ZUnpaired: zUn, PUnpaired: pUn,
		CILoPP: ciLo, CIHiPP: ciHi,
		ToolDiff:    statB.ToolMean - statA.ToolMean,
		DocsDiff:    statB.DocsMean - statA.DocsMean,
		MDEPairedPP: mdeP, MDEUnpairedPP: mdeU,
		Verdict: supplementClassifyVerdict(pUn, mdeU, pMc, mcOK),
	}

	if judged > 0 {
		rep.JudgeSelf = float64(selfAgree) / float64(judged)
	}
	rep.JudgeSwap, rep.JudgeJudged = swapDis, judged

	// —— 输出 ——
	t.Logf("")
	t.Logf("=== 逐臂（n=%d/臂）===", reps)
	for _, s := range rep.Arms {
		t.Logf("  %-20s 正确 %d/%d = %.0f%%  (Wilson 95%% %.0f%%~%.0f%%)  工具 %5.2f±%.2f  文档 %4.2f±%.2f",
			s.Arm, s.Correct, s.N, 100*s.CorrectRate, 100*s.WilsonLo, 100*s.WilsonHi,
			s.ToolMean, s.ToolStd, s.DocsMean, s.DocsStd)
		t.Logf("  %-20s 判定 %v  逐次 %s", "", s.Verdicts, s.Seq)
	}
	t.Logf("")
	t.Logf("=== 判据：B（指针）必须明显高于 A（冲突注入）===")
	t.Logf("  非配对差值 B-A = %+.1f pp   (A %.0f%% vs B %.0f%%,  pooled SE = %.1f pp)",
		diffPP, 100*statA.CorrectRate, 100*statB.CorrectRate, sePP)
	t.Logf("  95%% CI = [%+.1f, %+.1f] pp", ciLo, ciHi)
	t.Logf("  非配对 z = %.2f   双侧 p = %.3f", zUn, pUn)
	t.Logf("")
	t.Logf("  配对（McNemar 精确，n=%d 个可用配对）:", usablePairs)
	t.Logf("    不一致对 = %d   A 对 B 错 (b01) = %d   A 错 B 对 (b10) = %d", disc, b01, b10)
	if mcOK {
		t.Logf("    双侧精确 p = %.4f", pMc)
	} else {
		t.Logf("    p 不可给：%s", supplementNoDiscordanceNote)
	}
	t.Logf("")
	t.Logf("  本实验可分辨下限（MDE, α=0.05, power=0.80）: 配对 %.1f pp / 非配对 %.1f pp", mdeP, mdeU)
	t.Logf("  成本读数：工具调用 B-A = %+.2f   读文档数 B-A = %+.2f", rep.Analysis.ToolDiff, rep.Analysis.DocsDiff)
	t.Logf("")
	t.Logf("  结论：%s", rep.Analysis.Verdict)
	if rep.JudgeJudged > 0 {
		t.Logf("")
		t.Logf("  判定器 %s 自洽率 %.0f%% (%d/%d，两序对调一致)；顺序不一致 %d 条",
			jm, 100*rep.JudgeSelf, selfAgree, judged, swapDis)
		if rep.JudgeSelf <= 0.90 {
			t.Logf("  ⚠ 自洽率 ≤90%%：按协议 §10，**本次评估的所有结论作废**。")
		}
	}
	t.Logf("")
	t.Logf("  基准提醒：Step 0 的 C 臂（无任何注入）为 89%%。本实验只回答 B 与 A 的差，")
	t.Logf("  不回答「记忆比无记忆好多少」—— 那个问题由 supplement-2 在仓库无答案的")
	t.Logf("  任务上测（那个任务的 A 臂正确率会是 0 附近，基准不同）。")
	supplementWriteReport(t, "supplement1", rep)
}

func supplementWriteReport(t *testing.T, name string, rep any) {
	t.Helper()
	dir := os.Getenv("HAI_EVAL_REPORT_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Logf("report dir: %v", err)
		return
	}
	path := filepath.Join(dir, name+".json")
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
