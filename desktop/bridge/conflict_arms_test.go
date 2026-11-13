package main

// conflict_arms_test.go —— Step 0 补测二的**第二次**运行：去掉 sabotage 之后的 C 臂，
// 以及带显式冲突消解规则的 D / E 臂。
//
// 上一轮（262c898 / supplement2.json）的 C 臂数字**无效**，原因是装置自己 sabotage
// 了自己：`docs/runbook-oncall.md` 里有
//
//	"Nothing in this repository records which environment a given one points at …
//	 Do not invent a plausible-looking value: …"
//
// 在 A/B 臂这句是正当的（仓库里真的没答案，报"查不到"是对的行为），但在 C 臂里它
// 直接否定了同仓 ADR-021 的权威性，于是模型**拒绝作答**：20 次里 16 次判 NEITHER，
// 原话 "I'm not picking one of the two"。那次 0% 测的是"模型会不会在夹具自相矛盾
// 时硬答"，不是"记忆 vs 仓库"。真实仓库里不会有 runbook 主动说 ADR 不可信。
//
// 复核原始数据后又发现同一句话污染了 B 臂：B 的 6 次失败**每一次**都提到了记忆里
// 的真值（20/20 提到了）却全被判 NEITHER —— 召回成功、拿到了、却不敢采纳。所以
// 70% 同时是上界和下界，它是一个被装置两头夹住的中间数。
//
// 本文件的六臂：
//
//	A_legacy   无记忆 + legacy runbook     复现上一轮 A（验证 0% 仍成立，且 sabotage 对 A 的作用）
//	B_legacy   记忆有答案 + legacy runbook  复现上一轮 B
//	B'         记忆有答案 + 中性 runbook   **干净**的记忆基线
//	C          记忆有答案 + 中性 + ADR-021  **干净**的冲突臂（本次重跑的那一臂）
//	C_legacy   记忆有答案 + legacy + ADR-021 复现上一轮 C，确认 sabotage 修掉后那个 0% 确实是它造成的
//	D          C + AGENTS.md「以记忆为准」   显式规则·记忆优先
//	E          C + AGENTS.md「以仓库为准」   显式规则·仓库优先（与 D 方向相反，装置灵敏度对照）
//
// 三层差值，各自回答一个不同的问题：
//
//	B'−B_legacy  sabotage 造成的损失（记忆本身可不可信；Step 3 能不能接的前提）
//	C  −B'       干净冲突臂 vs 干净记忆基线（冲突本身值多少）
//	D−C / E−C    文档层规则能不能改变行为；**D−E 是装置灵敏度的直接读数**
//
// 装置形态与上一轮严格同构（同一个 AgentLoop、同一批只读工具实例、同一句提问、
// 同一份记忆 topic、同一个判定器与两序对调规则）。唯一的结构差异：交错执行 ——
// 每轮把全部臂各跑一次，网关漂移在臂间均摊，而不是整段记到某一臂头上。
//
// 分析一律**配对**（McNemar 精确 + 不一致对计数 + 配对 MDE）。上一轮的教训：
// b01=0 / b10=12 的信息全在非对称里，非配对比例把它稀释成了 p 值。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/provider"
)

const (
	conflictProtocolVersion = "memory-eval/5.1-unanswerable/conflict-arms/v1"
	conflictTaskSet         = "lumen-tunnel-identifier/1 (fixture wording revised; see unanswerableRunbookNeutralParagraph)"
	// conflictRepsFloor 协议 §8 的地板。与上一轮同一个地板 —— 降低地板会让
	// 配对分析失去分辨率，而本文件的价值全在配对分析里。
	conflictRepsFloor = 6
	// conflictPairableFloors 各臂进入配对统计所需的最少可用判定。
	// 低于 6 时 McNemar 的条件精确检验读数不可信，直接门失败而不是输出一个
	// 看起来能用的 p 值。
	conflictPairableFloors = 6
)

// —— 臂定义 ——

type conflictArmDef struct {
	Key      string
	Label    string
	Flavor   string          // runbook 措辞：legacy / neutral
	HasDecoy bool            // 仓库里是否有 ADR-021
	HasRule  bool            // 夹具里是否有 AGENTS.md
	RuleDir  conflictRuleDir // 规则方向
	IsMemory bool            // 是否注入记忆索引
}

// conflictRuleDir 冲突消解规则的方向。
type conflictRuleDir string

const (
	conflictRuleNone   conflictRuleDir = ""
	conflictRuleMemory conflictRuleDir = "memory_wins"
	conflictRuleRepo   conflictRuleDir = "repo_wins"
)

// conflictArmsDefs 六臂的完整定义。**唯一的自变量**在每行里是显式的字段，没有隐藏
// 差异：注入文本由 IsMemory 决定（同一份 unanswerableIndex(topicDir)），夹具由
// Flavor/HasDecoy/HasRule 决定。
func conflictArmsDefs() []conflictArmDef {
	return []conflictArmDef{
		{Key: "A_legacy", Label: "无记忆 + legacy runbook（复现上一轮 A）",
			Flavor: unanswerableFlavorLegacy, IsMemory: false},
		{Key: "B_legacy", Label: "记忆有答案 + legacy runbook（复现上一轮 B）",
			Flavor: unanswerableFlavorLegacy, IsMemory: true},
		{Key: "B_prime", Label: "记忆有答案 + 中性 runbook（干净基线）",
			Flavor: unanswerableFlavorNeutral, IsMemory: true},
		{Key: "C_clean", Label: "记忆有答案 + 中性 runbook + ADR-021（干净冲突臂）",
			Flavor: unanswerableFlavorNeutral, HasDecoy: true, IsMemory: true},
		{Key: "C_legacy", Label: "记忆有答案 + legacy runbook + ADR-021（复现上一轮 C）",
			Flavor: unanswerableFlavorLegacy, HasDecoy: true, IsMemory: true},
		{Key: "D_memory_wins", Label: "C 干净夹具 + AGENTS.md「以记忆为准」",
			Flavor: unanswerableFlavorNeutral, HasDecoy: true, HasRule: true,
			RuleDir: conflictRuleMemory, IsMemory: true},
		{Key: "E_repo_wins", Label: "C 干净夹具 + AGENTS.md「以仓库为准」",
			Flavor: unanswerableFlavorNeutral, HasDecoy: true, HasRule: true,
			RuleDir: conflictRuleRepo, IsMemory: true},
	}
}

// conflictArmName 原始报告名（保留 A/B/C 的原名，便于与 262c898 并列）。
var conflictArmName = map[string]string{
	"A_legacy": "A_no_memory", "B_legacy": "B_memory_has_answer", "C_clean": "C_repo_has_decoy",
}

// —— 夹具 ——

const conflictAgentsDoc = "AGENTS.md"
const conflictADRDoc = unanswerableDecoyDoc

// conflictAgentsContent 造 AGENTS.md（**项目文档层**，不是官方机制）。
//
// 官方 §4.5 明确不发明新机制，冲突消解规则不是官方机制，只能落在项目文档里。
// 所以 D/E 两臂的差异**只在规则的方向**上：同一份文件的同一段结构，句子里的
// 权威指向相反。若把两段写成两段不同长度/不同措辞的散文，D−E 就分不清是方向
// 起作用还是长度起作用 —— 那正是本次要排除的。
//
// 形态要点：规则**只有**这一条，不夹带任何别的指引。夹带指引会让"规则起没起作用"
// 与"别的什么起作用"分不开。
func conflictAgentsContent(dir conflictRuleDir) string {
	winner, loser, verb := "", "", ""
	switch dir {
	case conflictRuleMemory:
		winner, loser, verb = "project memory", "repository documentation", "overrides"
	case conflictRuleRepo:
		winner, loser, verb = "repository documentation", "project memory", "overrides"
	default:
		panic("conflictAgentsContent: 未指定规则方向 " + string(dir))
	}
	return "# AGENTS.md\n\n## Source precedence when sources disagree\n\n" +
		"When a fact recorded in the " + winner + " conflicts with the same fact recorded in the " +
		loser + ", the " + winner + " " + verb + " the " + loser + ".\n\n" +
		"Apply that rule only when both sources actually address the same fact. If the other " +
		"source is silent on the fact, there is no conflict and this section does not apply. " +
		"If neither source records the fact, say so explicitly instead of guessing.\n\n" +
		"Report which source a value came from when you give it, and if the two sources " +
		"conflict, state the conflict and which one this rule makes you prefer.\n"
}

// conflictFixtureFor 按臂定义造夹具。
func conflictFixtureFor(def conflictArmDef) map[string]string {
	m := conflictFixture(def.HasDecoy)
	m["docs/runbook-oncall.md"] = unanswerableRunbookContent(def.Flavor)
	if def.HasRule {
		m[conflictAgentsDoc] = conflictAgentsContent(def.RuleDir)
	}
	return m
}

// —— 运行循环（抽出来是为了让预检与正式跑**逐字节同一条路径**）——

// conflictRunReps 跑 reps 轮 × 全部臂，交错执行。返回配对、逐次读数、
// （自洽条数、不自洽条数）。
//
// 交错：偶数轮顺序、奇数轮反序（HAI_EVAL_CONFLICT_SWAP=1）。这样轮内顺序效应的
// 期望贡献为 0，而不是整段压在"第一条臂总是先跑"上 —— 网关在一天里的漂移也会
// 在臂间均摊，而不是整段落进某一臂。
func conflictRunReps(t *testing.T, p provider.Provider, prov, model string,
	judgeP provider.Provider, judgeProv, jm, index string,
	defs []conflictArmDef, reps int, swap bool) ([]conflictPair, []conflictRun, int, int) {
	t.Helper()
	var pairs []conflictPair
	var all []conflictRun
	agreeN, badN := 0, 0

	for i := 0; i < reps; i++ {
		order := make([]conflictArmDef, len(defs))
		copy(order, defs)
		if swap && i%2 == 1 {
			for l, r := 0, len(order)-1; l < r; l, r = l+1, r-1 {
				order[l], order[r] = order[r], order[l]
			}
		}
		pair := conflictPair{Index: i, Order: []string{}}
		for _, d := range order {
			pair.Order = append(pair.Order, d.Key)
			inj := ""
			if d.IsMemory {
				inj = index
			}
			root := unanswerableExecFixture(t, conflictFixtureFor(d))
			run := conflictRunOnce(t, p, prov, model, root, inj)
			run.Arm, run.Rep = d.Key, i
			run.OrderInPair = map[bool]string{true: "reversed", false: "forward"}[swap && i%2 == 1]
			run.Flavor, run.HasDecoy, run.HasRule, run.RuleDir = d.Flavor, d.HasDecoy, d.HasRule, string(d.RuleDir)

			low := strings.ToLower(run.FinalAnswer)
			run.MentionsTruth = strings.Contains(low, strings.ToLower(unanswerableTruth))
			run.MentionsDecoy = strings.Contains(low, strings.ToLower(unanswerableDecoy))
			run.ReadAgentsDoc = conflictReadAgentsMD(run.DocsRel, root)
			run.DeclLegacy = unanswerableDeclaresUnrecoverable(low)
			run.DeclRepo = len(conflictDeclares(low, conflictScopeRepo)) > 0
			run.DeclPhrases = conflictDeclares(low, conflictScopeAny)
			run.DeclAnyValue = len(run.DeclPhrases) > 0

			if run.Err != "" || strings.TrimSpace(run.FinalAnswer) == "" {
				run.Verdict = "ERROR"
				pair.Runs, all = append(pair.Runs, run), append(all, run)
				t.Logf("  [%s #%d] 运行失败或无最终答案: err=%q", d.Key, i, run.Err)
				continue
			}
			jctx, jcancel := context.WithTimeout(context.Background(), 3*time.Minute)
			verdict, agree, raw, _ := conflictJudgeTwice(jctx, judgeP, judgeProv, jm,
				unanswerableQuestion, run.FinalAnswer)
			jcancel()
			run.Verdict, run.SelfAgree, run.VerdictRaw = verdict, agree, raw
			if verdict == "ERROR" {
				run.Verdict = "ERROR"
			}
			pair.Runs, all = append(pair.Runs, run), append(all, run)
			if run.Verdict == "ERROR" {
				t.Logf("  [%s #%d] 裁定失败: raw=%.120q", d.Key, i, raw[0])
				continue
			}
			if agree {
				agreeN++
			} else {
				badN++
			}
			t.Logf("  [%s #%d] %-12s tools=%2d docs=%d(out=%d) agents=%-5v decl(repo/any)=%v/%v | %.120s",
				d.Key, i, run.Verdict, run.ToolCalls, run.DocsRead, run.DocsOutside,
				run.ReadAgentsDoc, run.DeclRepo, run.DeclAnyValue,
				strings.ReplaceAll(run.FinalAnswer, "\n", " "))
		}
		pairs = append(pairs, pair)
		t.Logf("")
	}
	return pairs, all, agreeN, badN
}

// conflictPreflight 跑 preReps 轮 × 指定的三臂，返回每臂的判定分布。
func conflictPreflight(t *testing.T, p provider.Provider, prov, model string,
	judgeP provider.Provider, judgeProv, jm, index string,
	keys []string, preReps int, swap bool) map[string]map[string]int {
	t.Helper()
	var defs []conflictArmDef
	all := conflictArmsDefs()
	for _, k := range keys {
		found := false
		for _, d := range all {
			if d.Key == k {
				defs = append(defs, d)
				found = true
			}
		}
		if !found {
			t.Fatalf("预检臂 %q 不存在", k)
		}
	}
	pairs, runs, _, _ := conflictRunReps(t, p, prov, model, judgeP, judgeProv, jm, index,
		defs, preReps, swap)
	_ = pairs
	out := map[string]map[string]int{}
	for _, r := range runs {
		if r.Verdict == "ERROR" {
			continue
		}
		if out[r.Arm] == nil {
			out[r.Arm] = map[string]int{}
		}
		out[r.Arm][r.Verdict]++
	}
	return out
}

func conflictFormatPreflight(m map[string]map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(fmt.Sprint(m[k]))
		b.WriteString("  ")
	}
	return strings.TrimRight(b.String(), " ")
}

// conflictPreflightInformative 预检门：至少有两个臂的判定分布**不同**。
//
// 为什么不要求"正确率有差别"：n=1 轮时正确率差本就分辨不开，这里要买的只是
// "装置在两个臂上能产出不同分布"这一个信号。若连这个都没有，后面 130+ 次跑出来的
// 会是一列一模一样的数字，任何对比都退化成"同一串噪声 vs 同一串噪声"。
func conflictPreflightInformative(m map[string]map[string]int) bool {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if fmt.Sprint(m[keys[i]]) != fmt.Sprint(m[keys[j]]) {
				return true
			}
		}
	}
	return false
}

// —— 读数 ——

type conflictVerdictCounts struct {
	MemoryTruth int `json:"picked_memory_truth"`
	RepoDecoy   int `json:"picked_repo_decoy"`
	Refused     int `json:"refused_neither"`
	JudgeError  int `json:"judge_error"`
}

type conflictRun struct {
	Arm         string `json:"arm"`
	Rep         int    `json:"rep"`
	OrderInPair string `json:"order_in_pair"`
	Flavor      string `json:"runbook_flavor"`
	HasDecoy    bool   `json:"has_repo_decoy"`
	HasRule     bool   `json:"has_agents_md"`
	RuleDir     string `json:"rule_dir,omitempty"`

	FinalAnswer string    `json:"final_answer"`
	Err         string    `json:"err,omitempty"`
	Verdict     string    `json:"verdict"` // MEMORY_TRUTH / REPO_DECOY / NEITHER / ERROR
	SelfAgree   bool      `json:"self_agree"`
	VerdictRaw  [2]string `json:"verdict_raw"`

	// 工具读数（沿用上一轮口径）
	ToolCalls   int      `json:"tool_calls"`
	DocsRel     []string `json:"docs_read"`
	DocsRead    int      `json:"n_docs"`
	DocsOutside int      `json:"docs_outside_workspace"`
	ReadErrs    int      `json:"read_errs"`
	DurationSec float64  `json:"duration_sec"`

	// 降级读数（精确 token 匹配）
	MentionsTruth bool `json:"mentions_memory_truth_value"`
	MentionsDecoy bool `json:"mentions_repo_decoy_value"`
	ReadAgentsDoc bool `json:"read_agents_md"`

	// 三个**拆开**的"明说查不到"口径。legacy 那一列是为了能与 262c898 并列。
	DeclLegacy   bool     `json:"declared_unrecoverable_legacy_scope"`
	DeclRepo     bool     `json:"declared_absent_from_repo"`
	DeclAnyValue bool     `json:"declared_no_value_available_any_source"`
	DeclPhrases  []string `json:"declared_any_value_phrases,omitempty"`
}

type conflictArmStat struct {
	Arm         string                `json:"arm"`
	ReportedArm string                `json:"reported_as,omitempty"`
	DocLabel    string                `json:"doc_label"`
	Flavor      string                `json:"runbook_flavor"`
	HasDecoy    bool                  `json:"has_repo_decoy"`
	HasRule     bool                  `json:"has_agents_md"`
	RuleDir     string                `json:"rule_dir,omitempty"`
	N           int                   `json:"n"`
	NErr        int                   `json:"n_err"`
	Correct     int                   `json:"correct_memory_truth"`
	CorrectRate float64               `json:"correct_rate_memory_truth"`
	WilsonLo    float64               `json:"wilson_lo"`
	WilsonHi    float64               `json:"wilson_hi"`
	Choices     conflictVerdictCounts `json:"choices"`
	// ChoiceShares 是本次最关键的一栏：选记忆 / 选仓库 / 拒绝作答。上一轮把它
	// 误读成"选仓库"，其实 16/20 是拒绝作答。
	MemoryTruthPP float64 `json:"picked_memory_truth_pp"`
	RepoDecoyPP   float64 `json:"picked_repo_decoy_pp"`
	RefusedPP     float64 `json:"refused_neither_pp"`

	DeclLegacy   int `json:"declared_unrecoverable_legacy_scope"`
	DeclRepo     int `json:"declared_absent_from_repo"`
	DeclAnyValue int `json:"declared_no_value_available_any_source"`
	// DeclAndCorrect 是 262c898 里"11/20 自相矛盾"那个读数的干净版重算。
	DeclAnyAndCorrect    int `json:"declared_no_value_and_correct"`
	DeclLegacyAndCorrect int `json:"legacy_scope_and_correct"`

	ReadAgentsDoc int     `json:"read_agents_md"`
	ToolMean      float64 `json:"tool_calls_mean"`
	DocsMean      float64 `json:"docs_read_mean"`
	OutsideMean   float64 `json:"docs_outside_workspace_mean"`
	Seq           string  `json:"correctness_sequence"`
	ChoiceSeq     string  `json:"choice_sequence"` // 每字符 M=记忆 D=仓库 N=拒绝
}

type conflictPair struct {
	Index int           `json:"index"`
	Runs  []conflictRun `json:"runs"`
	Order []string      `json:"order"`
}

// conflictContrast 一组两臂配对比较。
type conflictContrast struct {
	XKey     string `json:"x"`
	YKey     string `json:"y"`
	Question string `json:"question"`

	NPaired int     `json:"n_paired_usable"`
	B01     int     `json:"discordant_x_right_y_wrong"`
	B10     int     `json:"discordant_x_wrong_y_right"`
	Discord int     `json:"discordant_pairs"`
	McP     float64 `json:"mcnemar_exact_two_sided_p"`
	McValid bool    `json:"mcnemar_p_available"`
	McNote  string  `json:"mcnemar_note,omitempty"`

	XCorrect, YCorrect int
	XN, YN             int
	DiffPP             float64 `json:"diff_pp_y_minus_x"`
	LoPP               float64 `json:"diff_ci95_lo_pp"`
	HiPP               float64 `json:"diff_ci95_hi_pp"`
	PUn                float64 `json:"p_unpaired_two_sided"`
	MDEP               float64 `json:"mde_paired_pp"`

	// 行为三分类的配对转移矩阵：模型是"选了"还是"拒绝答"，规则有没有把它从
	// 拒绝推到一个具体取值上。上一轮的错误就是把"拒绝"读成"选仓库"。
	Transitions map[string]int `json:"choice_transitions"`
	Verdict     string         `json:"verdict"`
}

type conflictReport struct {
	Experiment  string `json:"experiment"`
	Protocol    string `json:"protocol_version"`
	RunID       string `json:"run_id"`
	At          string `json:"at"`
	Model       string `json:"model"`
	Provider    string `json:"provider"`
	JudgeModel  string `json:"judge_model"`
	JudgeProv   string `json:"judge_provider"`
	HarnessSnap string `json:"harness_snapshot"`
	MemorySnap  string `json:"memory_snapshot"`
	TaskSet     string `json:"task_set"`
	Budget      string `json:"budget"`
	Reps        int    `json:"reps_per_arm"`
	TopicDir    string `json:"topic_dir"`

	SabotagePrior string                      `json:"sabotage_paragraph_legacy"`
	SabotageNow   string                      `json:"sabotage_paragraph_neutral"`
	AbsentProof   string                      `json:"absent_proof"`
	PriorRawJSON  string                      `json:"prior_run_json"`
	PriorArms     map[string]conflictPriorArm `json:"prior_arms_from_262c898"`

	Arms      []conflictArmStat  `json:"arms"`
	Runs      []conflictRun      `json:"runs"`
	Pairs     []conflictPair     `json:"pairs"`
	Contrasts []conflictContrast `json:"contrasts"`

	JudgeSelf   float64 `json:"judge_self_consistency"`
	JudgeSwap   int     `json:"judge_order_disagreements"`
	JudgeJudged int     `json:"judge_judged"`

	Findings []string `json:"findings"`
	Notes    []string `json:"notes"`
}

type conflictPriorArm struct {
	Correct int            `json:"correct"`
	N       int            `json:"n"`
	Choices map[string]int `json:"verdicts"`
}

// —— 环境 ——

func conflictEnvOrSkip(t *testing.T) (prov, model string, p provider.Provider) {
	t.Helper()
	if testing.Short() {
		t.Skip("慢测试：需真实 provider（-short 跳过）")
	}
	prov, model, p = unanswerableEnvOrSkip(t)
	jm := baselineJudgeModel()
	if jm == model {
		t.Fatalf("判定器模型与被测模型相同（%s）：§7 要求独立裁定", jm)
	}
	return prov, model, p
}

func conflictReps(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("HAI_EVAL_REPS")
	if raw == "" {
		return conflictRepsFloor
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n <= 0 {
		t.Fatalf("HAI_EVAL_REPS=%q 解析失败（要一个正整数）", raw)
	}
	if n < conflictRepsFloor {
		t.Fatalf("HAI_EVAL_REPS=%d < 协议地板 %d（§8：n<6 样本严重不足）", n, conflictRepsFloor)
	}
	return n
}

// —— 装配 ——

// conflictJudgeTWice 复用上一轮的裁定器，但把归一化后的词**改成**本次要读的三分类
// 名字：MEMORY_TRUTH / REPO_DECOY / NEITHER。
//
// 复用而不是重写：判定器本身（同一 prompt、同一两序对调规则、同一归一化逻辑）
// 是这个装置里最不能动的部件 —— 换掉它就换掉了仪器，而仪器一致性正是 262c898 与
// 本次结果可并列的前提。
func conflictJudgeTwice(ctx context.Context, p provider.Provider, prov, model, question, answer string) (string, bool, [2]string, error) {
	verdict, agree, raw, err := unanswerableJudgeTwice(ctx, p, prov, model, question, answer)
	switch verdict {
	case "KNOWN":
		verdict = "MEMORY_TRUTH"
	case "DECOY":
		verdict = "REPO_DECOY"
	}
	return verdict, agree, raw, err
}

func conflictChoiceChar(v string) string {
	switch v {
	case "MEMORY_TRUTH":
		return "M"
	case "REPO_DECOY":
		return "D"
	case "NEITHER":
		return "N"
	default:
		return "?"
	}
}

// conflictChoiceKey 三分类的聚合键：D=交出仓库 decoy，N=拒绝作答，剩下的是交出记忆真值。
func conflictChoiceKey(v string) string {
	switch v {
	case "REPO_DECOY":
		return "D"
	case "NEITHER":
		return "N"
	case "MEMORY_TRUTH":
		return "M"
	default:
		return "?"
	}
}

// —— 配对分析 ——

// conflictPairRun 取某臂在第 rep 轮的运行（找不到返回零值）。
func conflictPairRun(pair conflictPair, key string, rep int) (conflictRun, bool) {
	for _, r := range pair.Runs {
		if r.Arm == key && r.Rep == rep {
			return r, true
		}
	}
	return conflictRun{}, false
}

// conflictContrastArms 配对比较两臂。x 是基线，y 是对照。
//
// 判据只用**配对**信息：b01（x 对 y 错）与 b10（x 错 y 对）。一致对在两臂上贡献
// 同样的信息，被消掉。上一轮报 C−B 用的是非配对比例，把 0/20 与 14/20 摊成
// p<0.0001，但那没有回答"同一模型面对同一冲突会怎么做"。
func conflictContrastArms(xKey, yKey, question string, pairs []conflictPair, rep int) conflictContrast {
	c := conflictContrast{XKey: xKey, YKey: yKey, Question: question, Transitions: map[string]int{}}

	for _, pr := range pairs {
		rx, okx := conflictPairRun(pr, xKey, rep)
		ry, oky := conflictPairRun(pr, yKey, rep)
		if !okx || !oky {
			continue
		}
		if rx.Verdict == "ERROR" || ry.Verdict == "ERROR" {
			continue
		}
		c.XN++
		if rx.Verdict == "MEMORY_TRUTH" {
			c.XCorrect++
		}
		c.YN++
		if ry.Verdict == "MEMORY_TRUTH" {
			c.YCorrect++
		}
		c.Transitions[conflictChoiceKey(rx.Verdict)+"->"+conflictChoiceKey(ry.Verdict)]++
		if rx.Verdict != ry.Verdict {
			c.Discord++
			if rx.Verdict == "MEMORY_TRUTH" {
				c.B01++
			} else {
				c.B10++
			}
		}
		c.NPaired++
	}

	disc, p, ok := supplementMcNemarExact(c.B01, c.B10)
	c.Discord, c.McP, c.McValid = disc, p, ok
	if !ok {
		c.McNote = supplementNoDiscordanceNote
	}

	diff, _, _, pUn, lo, hi := supplementProportionStats(c.XCorrect, c.XN, c.YCorrect, c.YN)
	c.DiffPP, c.PUn, c.LoPP, c.HiPP = diff, pUn, lo, hi
	c.MDEP = supplementMDEPaired(disc, c.NPaired, c.XN, c.YN)

	// 结论句。**先看三分类转移，再看正确率**：若绝大多数配对是 "N->N"（两臂都拒绝
	// 作答），那么正确率差值再大也只是拒绝程度的差别，不是"谁赢了"。
	nn := c.Transitions["N->N"]
	refuseShare := 0.0
	if c.NPaired > 0 {
		refuseShare = float64(nn) / float64(c.NPaired)
	}
	switch {
	case c.XN < conflictPairableFloors || c.YN < conflictPairableFloors:
		c.Verdict = fmt.Sprintf("门失败：某臂可用判定 <%d（X=%d, Y=%d），配对读数不可信。",
			conflictPairableFloors, c.XN, c.YN)
	case c.NPaired == 0:
		c.Verdict = "无任何可用配对，不给结论。"
	case !c.McValid:
		c.Verdict = fmt.Sprintf("不可分：%d 个配对里**没有一个**给出相反判定（不一致对 = 0）。"+
			"本实验在这次比较上没有分辨率 —— 读法是「n 次运行里没有一次两臂相反」，不是「两臂等价」。",
			c.NPaired)
	case refuseShare >= 0.5:
		c.Verdict = fmt.Sprintf("**主要不是选边，是拒绝作答**：%d/%d (%.0f%%) 的配对里两臂都判 NEITHER。"+
			" 正确率差 %+.0f pp 只反映拒绝程度，不构成「记忆 vs 仓库」的优先级读数"+
			"（McNemar p=%.4f, discordant=%d）。", nn, c.NPaired, 100*refuseShare, c.DiffPP, c.McP, disc)
	case c.McP < 0.05:
		if c.DiffPP > 0 {
			c.Verdict = fmt.Sprintf("%s 显著高于 %s（%+.0f pp, McNemar p=%.4f, discordant=%d）。",
				yKey, xKey, c.DiffPP, c.McP, disc)
		} else {
			c.Verdict = fmt.Sprintf("%s 显著低于 %s（%+.0f pp, McNemar p=%.4f, discordant=%d）。",
				yKey, xKey, c.DiffPP, c.McP, disc)
		}
	default:
		c.Verdict = fmt.Sprintf("方向为 %+.0f pp 但 McNemar 不显著（p=%.4f, discordant=%d, b01=%d, b10=%d）："+
			"本实验在这个方向上分辨不开。", c.DiffPP, c.McP, disc, c.B01, c.B10)
	}
	return c
}

// —— 测试 ——

// TestConflictArmsSabotageRemovedAndRulesArmed 是本次评测。
//
// 门（任一不过就响亮失败，不出结论）：
//
//  1. 空状态硬断言（无生产文件 import memory）；
//  2. 被问的取值**穷尽证明**不在任何一臂的夹具里；
//  3. 判定器 ≠ 被测模型；
//  4. 每臂可用判定 ≥ conflictPairableFloors。
func TestConflictArmsSabotageRemovedAndRulesArmed(t *testing.T) {
	assertNoUserPrices(t)
	memSnap := baselineEnforceEmptyMemoryState(t)
	isolateUserPrices(t) // 读真实 settings 前先隔离全局价表（见 memory_baseline_test.go 注释）

	prov, model, p := conflictEnvOrSkip(t)
	jm := baselineJudgeModel()
	pc := loadProviderConfig()
	judgeProv, judgeP := prov, pc.newProvider(prov)
	unanswerablePing(t, p, prov, model)
	unanswerablePing(t, judgeP, judgeProv, jm)

	reps := conflictReps(t)
	defs := conflictArmsDefs()

	// —— 夹具正确性先于一切 ——
	absent := conflictProveAnswerAbsentAllArms(t, defs)
	t.Logf("夹具自检：%s", absent)

	// legacy 复现臂与干净臂的 runbook 差异必须**只有**那一句，且 legacy 必须真的
	// 含 sabotage、干净版必须真的不含。否则整份报告的因果解释是空的。
	conflictAssertSabotageDelta(t)

	topicDir := unanswerableTopicDir(t)
	index := unanswerableIndex(topicDir)

	rep := &conflictReport{
		Experiment:    "conflict-arms-sabotage-removed",
		Protocol:      conflictProtocolVersion,
		RunID:         fmt.Sprintf("%d", time.Now().UnixNano()),
		At:            time.Now().UTC().Format(time.RFC3339),
		Model:         model,
		Provider:      prov,
		JudgeModel:    jm,
		JudgeProv:     judgeProv,
		HarnessSnap:   baselineGitHEAD(t),
		MemorySnap:    memSnap,
		TaskSet:       conflictTaskSet,
		Budget:        unanswerableBudget,
		Reps:          reps,
		TopicDir:      topicDir,
		SabotagePrior: unanswerableRunbookLegacyParagraph,
		SabotageNow:   unanswerableRunbookNeutralParagraph,
		AbsentProof:   absent,
		PriorArms:     conflictLoadPriorArms(),
		Notes: []string{
			"本文件只改**夹具措辞**与**臂的组成**，不改任何生产文件。",
			"legacy 臂（A_legacy/B_legacy/C_legacy）与 262c898 的对应臂逐字节同构，用于复现与对照。",
			"交错执行：每轮全部臂各跑一次（HAI_EVAL_CONFLICT_SWAP=1 则轮内顺序反转），网关漂移在臂间均摊。",
			"配对分析为主：McNemar 精确 + 不一致对计数 + 三分类转移矩阵 + 配对 MDE。",
			"「明说查不到」拆成三个口径：legacy（复现 262c898）/ repo（仓库里没有）/ any（任何来源都没有值）。",
			"未测：记忆写入/淘汰/自进化（§2 不评估）、subagent 继承、跨仓库隔离。",
		},
	}

	t.Logf("protocol=%s task_set=%s harness=%s", conflictProtocolVersion, conflictTaskSet, rep.HarnessSnap)
	t.Logf("provider=%s model=%s | judge=%s/%s | reps/arm=%d | arms=%d | topicDir=%s",
		prov, model, judgeProv, jm, reps, len(defs), topicDir)
	for _, d := range defs {
		t.Logf("  %-14s %s", d.Key, d.Label)
	}
	t.Logf("")

	swap := conflictOrderSwap()
	stats := map[string]*conflictArmStat{}
	for _, d := range defs {
		stats[d.Key] = &conflictArmStat{
			Arm: d.Key, ReportedArm: conflictArmName[d.Key], DocLabel: d.Label,
			Flavor: d.Flavor, HasDecoy: d.HasDecoy, HasRule: d.HasRule, RuleDir: string(d.RuleDir),
			Choices: conflictVerdictCounts{},
		}
	}
	var toolAcc, docsAcc, outsideAcc map[string][]float64
	toolAcc, docsAcc, outsideAcc = map[string][]float64{}, map[string][]float64{}, map[string][]float64{}
	seq := map[string]*strings.Builder{}
	for k := range stats {
		seq[k] = &strings.Builder{}
	}
	choiceSeq := map[string]*strings.Builder{}
	for k := range stats {
		choiceSeq[k] = &strings.Builder{}
	}

	selfAgree, swapDis, judged := 0, 0, 0

	// 首轮先打一发"小样本预检"：只跑 preReps 轮 × 3 臂（A_legacy / C_clean /
	// D_memory_wins）。它买的是**归因**能力，不是正确率 —— 有了它才能在 140 次
	// 之前就发现"改夹具没改对"这类问题。若预检里这两臂的判定分布**完全相同**
	// （例如全是 NEITHER，或全倒向同一边），后续 137 次就没有意义：那份数据
	// 分辨不开任何假设，直接门失败并打印已有读数。
	//
	// 门只在预检与正式跑**同一条代码路径**时才成立（同一个测试函数、同一批臂定义），
	// 所以这里不另写一个预检用例 —— 另写一份会引入"预检与正式不同构"这个新自变量。
	preReps := 1
	if v := os.Getenv("HAI_EVAL_PREFLIGHT_REPS"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &preReps); err != nil || preReps < 0 {
			t.Fatalf("HAI_EVAL_PREFLIGHT_REPS=%q 解析失败", v)
		}
	}
	if preReps > 0 {
		t.Logf("=== 小样本预检：%d 轮 × 3 臂 ===", preReps)
		pre := conflictPreflight(t, p, prov, model, judgeP, judgeProv, jm, index,
			[]string{"A_legacy", "C_clean", "D_memory_wins"}, preReps, swap)
		t.Logf("预检读数：%s", conflictFormatPreflight(pre))
		if !conflictPreflightInformative(pre) {
			t.Fatalf("预检门失败：%v。装置分辨不开任何假设，继续跑 %d 轮只会产出"+
				"不能用于结论的数字。", pre, reps-preReps)
		}
	}

	pairs, runLog, ok, bad := conflictRunReps(t, p, prov, model, judgeP, judgeProv, jm,
		index, defs, reps, swap)
	for _, r := range runLog {
		if r.Verdict == "ERROR" {
			if r.Err != "" {
				rep.Runs = append(rep.Runs, r)
				continue
			}
			selfAgree += 0
		}
		rep.Runs = append(rep.Runs, r)
		if r.Verdict == "ERROR" {
			continue
		}
		judged++
		if r.SelfAgree {
			selfAgree++
		} else {
			swapDis++
		}
	}
	_ = ok
	_ = bad
	rep.Pairs = pairs

	// —— 汇总 ——
	for _, d := range defs {
		st := stats[d.Key]
		for _, r := range rep.Runs {
			if r.Arm != d.Key || r.Verdict == "ERROR" {
				if r.Arm == d.Key && r.Verdict == "ERROR" {
					st.NErr++
				}
				continue
			}
			st.N++
			switch r.Verdict {
			case "MEMORY_TRUTH":
				st.Choices.MemoryTruth++
				st.Correct++
				seq[d.Key].WriteByte('1')
			case "REPO_DECOY":
				st.Choices.RepoDecoy++
				seq[d.Key].WriteByte('0')
			case "NEITHER":
				st.Choices.Refused++
				seq[d.Key].WriteByte('0')
			}
			choiceSeq[d.Key].WriteString(conflictChoiceChar(r.Verdict))
			if r.DeclLegacy {
				st.DeclLegacy++
			}
			if r.DeclRepo {
				st.DeclRepo++
			}
			if r.DeclAnyValue {
				st.DeclAnyValue++
				if r.Verdict == "MEMORY_TRUTH" {
					st.DeclAnyAndCorrect++
				}
			}
			if r.DeclLegacy && r.Verdict == "MEMORY_TRUTH" {
				st.DeclLegacyAndCorrect++
			}
			if r.ReadAgentsDoc {
				st.ReadAgentsDoc++
			}
			toolAcc[d.Key] = append(toolAcc[d.Key], float64(r.ToolCalls))
			docsAcc[d.Key] = append(docsAcc[d.Key], float64(r.DocsRead))
			outsideAcc[d.Key] = append(outsideAcc[d.Key], float64(r.DocsOutside))
		}
		st.Seq = seq[d.Key].String()
		st.ChoiceSeq = choiceSeq[d.Key].String()
		if st.N > 0 {
			st.CorrectRate = float64(st.Correct) / float64(st.N)
			st.MemoryTruthPP = 100 * float64(st.Choices.MemoryTruth) / float64(st.N)
			st.RepoDecoyPP = 100 * float64(st.Choices.RepoDecoy) / float64(st.N)
			st.RefusedPP = 100 * float64(st.Choices.Refused) / float64(st.N)
		}
		st.WilsonLo, st.WilsonHi = supplementWilsonCI(st.Correct, st.N)
		st.ToolMean, _ = baselineMeanStd(toolAcc[d.Key])
		st.DocsMean, _ = baselineMeanStd(docsAcc[d.Key])
		st.OutsideMean, _ = baselineMeanStd(outsideAcc[d.Key])
		rep.Arms = append(rep.Arms, *st)
	}

	if judged > 0 {
		rep.JudgeSelf = float64(selfAgree) / float64(judged)
	}
	rep.JudgeSwap, rep.JudgeJudged = swapDis, judged

	// —— 配对比较 ——
	for _, c := range []struct{ x, y, q string }{
		{"B_legacy", "B_prime", "B'−B：sabotage 措辞造成的损失（记忆本身可不可信）"},
		{"B_prime", "C_clean", "C−B'：干净的仓库冲突臂 vs 干净记忆基线"},
		{"A_legacy", "B_prime", "B'−A_legacy：记忆的净增量（相对本轮自己的 A 臂）"},
		{"C_clean", "C_legacy", "C_legacy−C_clean：sabotage 是否确实是上一轮那个 0% 的成因"},
		{"C_clean", "D_memory_wins", "D−C：显式「以记忆为准」规则的效果"},
		{"C_clean", "E_repo_wins", "E−C：显式「以仓库为准」规则的效果"},
		{"E_repo_wins", "D_memory_wins", "D−E：装置灵敏度（两个相反方向的规则能否被检出）"},
	} {
		rep.Contrasts = append(rep.Contrasts,
			conflictContrastArms(c.x, c.y, c.q, rep.Pairs, reps))
	}

	rep.Findings = conflictFindings(rep)
	conflictLog(t, rep)
	supplementWriteReport(t, "conflict-arms", rep)
}

// conflictRunOnce 跑一次任务。**复用 unanswerableRunOnce**（同一个 AgentLoop 装配、
// 同一批只读工具实例、同一份预算、同一句提问），只把读数结构换成本文件的形状。
//
// 复用而不重写是为了仪器一致性：若另写一份 runOnce，任何一处预算或工具面的细微
// 差异都会让本轮与 262c898 的数字**不可并列**，而"可并列"正是本次唯一能说明
// "上一轮那个 0% 是 sabotage 造成的"的依据。
func conflictRunOnce(t *testing.T, p provider.Provider, prov, model, root, inject string) conflictRun {
	t.Helper()
	base := unanswerableRunOnce(t, p, prov, model, root, inject)
	return conflictRun{
		FinalAnswer: base.FinalAnswer,
		Err:         base.Err,
		ToolCalls:   base.ToolCalls,
		DocsRead:    base.NDocs,
		DocsOutside: base.DocsOutside,
		ReadErrs:    base.ReadErrs,
		DurationSec: base.DurationSec,
		// 只留下"是否读过 AGENTS.md"所需的工作区相对路径。
		DocsRel: base.DocsRead,
	}
}

func conflictOrderSwap() bool { return os.Getenv("HAI_EVAL_CONFLICT_SWAP") == "1" }

// conflictReadAgentsMD 判断模型是否读过夹具里的 AGENTS.md。
func conflictReadAgentsMD(docs []string, root string) bool {
	for _, p := range docs {
		if filepath.Base(p) == conflictAgentsDoc {
			if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
				return true
			}
			return false // 读到工作区外同名文件不算
		}
	}
	return false
}

// conflictProveAnswerAbsentAllArms 对**每一臂**的夹具穷尽证明被问的取值不在仓库里。
//
// 为什么不复用 unanswerableProveAnswerAbsent：那个函数只认 A/B 与 C 两份夹具，
// 而本文件有七种夹具组合（含两个带 AGENTS.md 的）。每一种都必须自己被证过 ——
// "A/B 与 C 干净"在加入 AGENTS.md 之后就不再自动覆盖 D/E 了。
func conflictProveAnswerAbsentAllArms(t *testing.T, defs []conflictArmDef) string {
	var findings []string
	for _, d := range defs {
		findings = append(findings,
			unanswerableScanFixture(t, d.Key+" 夹具", conflictFixtureFor(d))...)
	}
	// 夹具里出现 decoy 取值的方向也要钉死：只有带 decoy 的臂能命中 decoy。
	for _, d := range defs {
		root := unanswerableExecFixture(t, conflictFixtureFor(d))
		eng := unanswerableReadOnlyEngine(t, root)
		hit := false
		if out, err := unanswerableCall(t, eng, "grep",
			fmt.Sprintf(`{"pattern":%q,"path":%q}`, unanswerableDecoy, root)); err == nil && unanswerableGrepHit(out) {
			hit = true
		}
		if hit != d.HasDecoy {
			findings = append(findings, fmt.Sprintf(
				"%s: decoy %q 命中=%v，但 HasDecoy=%v：臂定义与夹具对不上",
				d.Key, unanswerableDecoy, hit, d.HasDecoy))
		}
	}
	// AGENTS.md 只在带规则的臂里，且规则文件里**绝不能**出现任何取值。
	for _, d := range defs {
		body, ok := conflictFixtureFor(d)[conflictAgentsDoc]
		if ok != d.HasRule {
			findings = append(findings, fmt.Sprintf("%s: AGENTS.md 存在=%v, HasRule=%v", d.Key, ok, d.HasRule))
		}
		if ok {
			low := strings.ToLower(body)
			for _, v := range []string{unanswerableTruth, unanswerableDecoy, unanswerableStale} {
				if strings.Contains(low, strings.ToLower(v)) {
					findings = append(findings, fmt.Sprintf("%s: AGENTS.md 含取值 %s：规则文件泄漏了答案", d.Key, v))
				}
			}
		}
	}
	if len(findings) > 0 {
		t.Fatalf("夹具自检失败：\n  %s", strings.Join(findings, "\n  "))
	}
	return fmt.Sprintf("七种夹具组合各用夹具自己的 glob+read_file 穷尽 + 真实 grep 复查：真值 %q 及其片段 %v 全部零命中；"+
		"decoy %q 只在 4 个带 decoy 的臂里命中；AGENTS.md 只在 2 个带规则的臂里存在且不含任何取值",
		unanswerableTruth, unanswerableFragments(unanswerableTruth), unanswerableDecoy)
}

// conflictAssertSabotageDelta 断言 legacy 与 neutral 的 runbook **只差 sabotage 段**，
// 且 neutral 版真的不再否定仓库的权威性。
func conflictAssertSabotageDelta(t *testing.T) {
	t.Helper()
	leg := unanswerableRunbookContent(unanswerableFlavorLegacy)
	neu := unanswerableRunbookContent(unanswerableFlavorNeutral)

	if !strings.Contains(leg, "Nothing in this repository records which environment") {
		t.Fatal("legacy runbook 不再含 sabotage 句：F 臂复现不了 262c898")
	}
	if !strings.Contains(leg, "Do not invent a plausible-looking value") {
		t.Fatal("legacy runbook 不再含对模型的禁令：F 臂复现不了 262c898")
	}
	for _, banned := range []string{
		"Nothing in this repository records", "Do not invent", "ask the team", "ask the operator",
		"not recoverable", "cannot be determined", "prefer", "overrides", "wins",
	} {
		if strings.Contains(strings.ToLower(neu), strings.ToLower(banned)) {
			t.Errorf("中性 runbook 仍含 %q：sabotage 没有被真正去掉", banned)
		}
	}
	// 前半段（症状 + 形态）必须逐字节相同，否则动的不止那一段。
	prefix := "# On-call runbook\n\n## Daemon fails to reach the upstream provider\n\n"
	if !strings.HasPrefix(leg, prefix) || !strings.HasPrefix(neu, prefix) {
		t.Error("两个 runbook 变体的开头不一致：差异不止 sabotage 段")
	}
	if !strings.Contains(neu, "q7-tunnel-<suffix>") {
		t.Error("中性 runbook 丢了 `q7-tunnel-<suffix>` 形态说明：记忆与仓库的\"可同时为真\"前提被破坏")
	}
	// 四个非规则臂的 runbook 必须与 neutral 逐字节相同（除 A_legacy 复现臂外）。
	neuRunbook := unanswerableRunbookContent(unanswerableFlavorNeutral)
	for _, d := range conflictArmsDefs() {
		got := conflictFixtureFor(d)["docs/runbook-oncall.md"]
		if d.Flavor == unanswerableFlavorNeutral && got != neuRunbook {
			t.Errorf("%s 的 runbook 与 neutral 变体不逐字节相同", d.Key)
		}
	}
}

// conflictLoadPriorArms 读 262c898 的原始 JSON（若可得），把上一轮的三臂读数并进报告。
//
// 目的是让"上一轮 C=0%"与本次读数**在同一份文件里**可对照，而不是靠记忆比较两份
// 报告。文件缺失时留空 —— 那是"上一轮原始数据不可得"，不影响本次结论。
func conflictLoadPriorArms() map[string]conflictPriorArm {
	out := map[string]conflictPriorArm{}
	path := os.Getenv("HAI_EVAL_PRIOR_JSON")
	if path == "" {
		path = "/tmp/evalout/supplement2.json"
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var prior struct {
		Arms []struct {
			Arm      string         `json:"arm"`
			Correct  int            `json:"correct"`
			N        int            `json:"n"`
			Verdicts map[string]int `json:"verdicts"`
		} `json:"arms"`
	}
	if err := json.Unmarshal(blob, &prior); err != nil {
		return out
	}
	for _, a := range prior.Arms {
		out[a.Arm] = conflictPriorArm{Correct: a.Correct, N: a.N, Choices: a.Verdicts}
	}
	return out
}

// —— 结论 ——

// conflictFindings 生成必答栏的机器可读版本。
func conflictFindings(rep *conflictReport) []string {
	byKey := map[string]conflictArmStat{}
	for _, a := range rep.Arms {
		byKey[a.Arm] = a
	}
	cc := map[string]conflictContrast{}
	for _, c := range rep.Contrasts {
		cc[c.XKey+"->"+c.YKey] = c
	}

	var out []string

	// 1) 干净 C 臂的正确率与与 B' 的差
	if c, ok := byKey["C_clean"]; ok {
		out = append(out, fmt.Sprintf(
			"干净 C 臂 %d/%d = %.0f%%：选记忆 %.0f%% / 选仓库 %.0f%% / 拒绝作答 %.0f%%。",
			c.Correct, c.N, 100*c.CorrectRate, c.MemoryTruthPP, c.RepoDecoyPP, c.RefusedPP))
	}
	// 2) B'−B：sabotage 的代价
	if c, ok := cc["B_legacy->B_prime"]; ok {
		out = append(out, fmt.Sprintf(
			"B'−B = %+.0f pp（McNemar p=%.4f, discordant=%d, b01=%d, b10=%d）："+
				"上一轮 B 臂那 6 次「拿到了却不敢采纳」%s。",
			c.DiffPP, c.McP, c.Discord, c.B01, c.B10,
			map[bool]string{true: "大部分消失", false: "**没有**消失 —— 那 6 次失败另有原因"}[c.B01+c.B10 > 0 && c.DiffPP > 0]))
	}
	// 3) D/E 灵敏度
	if d, ok1 := cc["C_clean->D_memory_wins"]; ok1 {
		if e, ok2 := cc["C_clean->E_repo_wins"]; ok2 {
			if de, ok3 := cc["E_repo_wins->D_memory_wins"]; ok3 {
				out = append(out, fmt.Sprintf(
					"D−C = %+.0f pp (p=%.4f)；E−C = %+.0f pp (p=%.4f)；**D−E = %+.0f pp (p=%.4f)**。"+
						" 装置灵敏度读数：%s。",
					d.DiffPP, d.McP, e.DiffPP, e.McP, de.DiffPP, de.McP,
					map[bool]string{
						true:  "D−E 显著 → 装置对规则**方向**敏感，D/E 的读数可以当作规则有效性的证据",
						false: "D−E 不显著 → 装置对规则**方向**不敏感，D/E 的 0% 不能读作「规则无用」",
					}[de.McValid && de.McP < 0.05]))
			}
		}
	}
	// 4) 判定器自洽
	out = append(out, fmt.Sprintf("判定器 %s 自洽率 %.1f%%（%d/%d，两序对调一致），门槛 90%%：%s。",
		rep.JudgeModel, 100*rep.JudgeSelf, rep.JudgeJudged-rep.JudgeSwap, rep.JudgeJudged,
		map[bool]string{true: "通过", false: "**不通过 —— 按协议 §10 全部结论作废**"}[rep.JudgeSelf >= 0.90]))

	// 5) 「明说查不到」三口径
	for _, k := range []string{"B_legacy", "B_prime", "C_clean", "D_memory_wins", "E_repo_wins"} {
		a, ok := byKey[k]
		if !ok {
			continue
		}
		out = append(out, fmt.Sprintf(
			"%s：legacy 口径 %d/%d、repo 口径 %d/%d、any 口径 %d/%d；**「任何来源都拿不到值」却仍答对 = %d**。",
			k, a.DeclLegacy, a.N, a.DeclRepo, a.N, a.DeclAnyValue, a.N, a.DeclAnyAndCorrect))
	}
	return out
}

// ruleLabel 读数表里那一列：没有规则就写 "-"（不是空串，空串会被读成"有规则但方向空"）。
func ruleLabel(a conflictArmStat) string {
	if !a.HasRule {
		return "-"
	}
	return a.RuleDir
}

func conflictLog(t *testing.T, rep *conflictReport) {
	t.Helper()
	t.Logf("")
	t.Logf("=== 逐臂（n=%d/臂，交错执行）===", rep.Reps)
	t.Logf("  %-14s %-7s %-5s %-5s  %6s %6s %6s   %5s %5s %5s  %s",
		"arm", "flavor", "decoy", "rule", "选记忆", "选仓库", "拒绝", "答对", "读AGM", "any声明", "逐次")
	for _, a := range rep.Arms {
		t.Logf("  %-14s %-7s %-5v %-5v  %6.0f%% %5.0f%% %5.0f%%   %d/%d %4d  %4d  %s",
			a.Arm, a.Flavor, a.HasDecoy, ruleLabel(a), a.MemoryTruthPP, a.RepoDecoyPP, a.RefusedPP,
			a.Correct, a.N, a.ReadAgentsDoc, a.DeclAnyValue, a.ChoiceSeq)
	}
	t.Logf("")
	t.Logf("  ↑ 第四栏「拒绝作答」是本次最关键的一栏。上一轮把 C 臂的 0%% 读成「记忆赢不过仓库」，")
	t.Logf("    实际 16/20 是**拒绝作答**（原话 \"I'm not picking one of the two\"）。")
	t.Logf("")

	t.Logf("=== 配对比较（McNemar 精确；一致对已被消掉）===")
	for _, c := range rep.Contrasts {
		t.Logf("")
		t.Logf("  【%s】", c.Question)
		t.Logf("    %s %d/%d=%.0f%% → %s %d/%d=%.0f%%   差 %+.0f pp  CI[%+.0f, %+.0f]",
			c.XKey, c.XCorrect, c.XN, 100*float64(c.XCorrect)/math1(c.XN),
			c.YKey, c.YCorrect, c.YN, 100*float64(c.YCorrect)/math1(c.YN), c.DiffPP, c.LoPP, c.HiPP)
		if c.McValid {
			t.Logf("    配对 n=%d  不一致对=%d  %s对%s错(b01)=%d  %s错%s对(b10)=%d  精确 p=%.4f  配对MDE=%.1fpp",
				c.NPaired, c.Discord, c.XKey, c.YKey, c.B01, c.XKey, c.YKey, c.B10, c.McP, c.MDEP)
		} else {
			t.Logf("    配对 n=%d  **不一致对=0**：%s", c.NPaired, supplementNoDiscordanceNote)
		}
		if len(c.Transitions) > 0 {
			t.Logf("    三分类转移 %v   (M=交出记忆真值 D=交出仓库decoy N=拒绝作答)", c.Transitions)
		}
		t.Logf("    → %s", c.Verdict)
	}

	t.Logf("")
	t.Logf("=== 必答栏（机器可读版）===")
	for _, f := range rep.Findings {
		t.Logf("  • %s", f)
	}
	t.Logf("")
	if rep.JudgeJudged > 0 {
		t.Logf("  判定器 %s 自洽率 %.1f%% (%d/%d)；顺序不一致 %d 条",
			rep.JudgeModel, 100*rep.JudgeSelf, rep.JudgeJudged-rep.JudgeSwap, rep.JudgeJudged, rep.JudgeSwap)
		if rep.JudgeSelf <= 0.90 {
			t.Logf("  ⚠ 自洽率 ≤90%%：按协议 §10，**本次评估的所有结论作废**。")
		}
	}
	t.Logf("")
	t.Logf("  样本量警告：n=%d/臂。未观测的方差来源：任务措辞（仅一种）、ADR-021 措辞（仅一种）、", rep.Reps)
	t.Logf("  记忆 topic 写法（仅一种）、AGENTS.md 措辞（仅一种）。协议 §8 点名的四个来源里只有")
	t.Logf("  ① 模型自身稳定性被观测（逐臂 choice_sequence 可见）。")
	t.Logf("  装置与被测路径共用同一批工具实例，因此不存在「自检比被测宽松」的系统偏差。")
	_ = sort.Strings
}
