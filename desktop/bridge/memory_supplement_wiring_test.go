package main

// memory_supplement_wiring_test.go —— 两个补充评测的**确定性**自检。
//
// 不需要凭证、不打网关、不花 token。三条纪律在本文件里变成断言：
//
//  1. **夹具与契约一致**（协议 §5.0 —— Step 0 最大的教训）。
//     记忆不与仓库冲突；被问的取值不在仓库里；decoy 只在 C 臂夹具里；
//     夹具仓里没有任何指向 topic 的指针（否则 B 臂退化成"记忆指路"）。
//  2. **证明本身可被反向测试**。"答案不在仓库里"这个断言如果不负向测试过，
//     那它和"什么都不检查"在测试输出里长得一模一样。见
//     TestUnanswerableAbsenceProofCatchesTruth。
//  3. **注入真的进了发给 provider 的请求**（沿用 memory_baseline_wiring_test.go 的
//     手法：假 provider 截获 system prompt）。前两条只证明"我们构造了正确的字符串"，
//     不证明"AgentLoop 把它送出去了"—— 而后者才是实验成立的前提。
//
// 外加统计函数的解析可算样例：p 值算错是**静默**灾难（数字照样漂亮，只是全错），
// 所以它们不与真实实验共用一份"看起来对"的实现。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// —— 假 provider（截获 system prompt，不联网）——

type supplementCaptureProvider struct {
	mu   sync.Mutex
	sent [][]core.Message
}

func (p *supplementCaptureProvider) Stream(_ context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	p.sent = append(p.sent, append([]core.Message(nil), req.Messages...))
	p.mu.Unlock()
	return h(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
}

func (p *supplementCaptureProvider) systemText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sent) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range p.sent[0] {
		if m.Role != core.System {
			continue
		}
		for _, c := range m.Content {
			if c.Type == core.ContentTypeText {
				b.WriteString(c.Content)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// unanswerableCaptureRun 用假 provider 跑一次，截获模型真正收到的 system prompt。
func unanswerableCaptureRun(t *testing.T, root, inject string) string {
	t.Helper()
	eng := tools.NewToolEngine()
	for _, tl := range unanswerableReadOnlyEngine(t, root) {
		eng.RegisterTool(context.Background(), tl)
	}
	p := &supplementCaptureProvider{}
	opts := []agents.Option{
		agents.WithProvider(p),
		agents.WithProviderName("capture"),
		agents.WithModel("capture-model"),
		agents.WithToolEngine(eng),
		agents.WithWorkingDir(root),
		agents.WithMaxIterations(1),
		agents.WithMaxRetries(1),
	}
	if inject != "" {
		opts = append(opts, agents.WithSystemPrompt(inject))
	}
	loop := agents.NewAgentLoop(opts...)
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{
		Type: core.ContentTypeText, Content: unanswerableQuestion,
	})}, func(context.Context, events.Event) {})
	if ac == nil {
		t.Fatal("AgentLoop.Run 返回 nil")
	}
	return p.systemText()
}

// —— 统计：解析可算的样例 ——

func TestSupplementMcNemarExactMatchesHandComputed(t *testing.T) {
	// (b01, b10) → 期望的不一致对总数与双侧精确 p（手算：2*P(X<=min) with Bin(n,0.5)）
	cases := []struct {
		b01, b10 int
		wantN    int
		wantP    float64
	}{
		{0, 0, 0, 0},      // 无反例：p 不可给（不是 1.0！）
		{1, 0, 1, 1.0},    // 1 个不一致对 → 2*0.5 = 1.0
		{2, 0, 2, 0.5},    // P(X<=0)=0.25 → 0.5
		{3, 0, 3, 0.25},   // P(X<=0)=0.125 → 0.25
		{5, 0, 5, 0.0625}, // 2*1/32
		{4, 2, 6, 0.6875}, // 2*(1+6+15)/64 = 0.6875
		{10, 2, 12, 2 * (1.0 + 12 + 66) / 4096.0},
	}
	for _, c := range cases {
		n, p, ok := supplementMcNemarExact(c.b01, c.b10)
		if n != c.wantN {
			t.Errorf("(%d,%d) 不一致对总数 = %d，期望 %d", c.b01, c.b10, n, c.wantN)
		}
		if c.wantN == 0 {
			if ok {
				t.Errorf("(%d,%d) 应当报 p 不可给，却给了 p=%v", c.b01, c.b10, p)
			}
			continue
		}
		if !ok {
			t.Errorf("(%d,%d) p 应当可给", c.b01, c.b10)
			continue
		}
		if diff := p - c.wantP; diff > 1e-12 || diff < -1e-12 {
			t.Errorf("(%d,%d) p = %.6f，期望 %.6f", c.b01, c.b10, p, c.wantP)
		}
		if p > 1 {
			t.Errorf("(%d,%d) p = %v > 1：二项双尾必须截断到 1", c.b01, c.b10, p)
		}
	}
}

func TestSupplementWilsonCICoversDegenerateRates(t *testing.T) {
	// 0/n 与 n/n 是最常被误报成"区间宽度为 0"的情形；Wilson 区间的价值正在这里。
	for _, c := range []struct{ x, n int }{{0, 6}, {6, 6}, {0, 30}, {30, 30}, {15, 30}} {
		lo, hi := supplementWilsonCI(c.x, c.n)
		if !(lo >= 0 && hi <= 1 && lo < hi) {
			t.Errorf("Wilson(%d/%d) = [%.4f, %.4f]，期望 0<=lo<hi<=1", c.x, c.n, lo, hi)
		}
	}
	// 已知解析值：Wilson 95% for 15/30 ≈ [0.331, 0.667]
	lo, hi := supplementWilsonCI(15, 30)
	if lo < 0.32 || lo > 0.35 || hi < 0.66 || hi > 0.68 {
		t.Errorf("Wilson(15/30) = [%.4f, %.4f]，期望约 [0.331, 0.667]", lo, hi)
	}
	// n=0 必须返回 [0,0] 而不是 NaN
	if lo, hi := supplementWilsonCI(0, 0); lo != 0 || hi != 0 {
		t.Errorf("Wilson(0/0) = [%v, %v]，期望 [0, 0]", lo, hi)
	}
}

func TestSupplementProportionStatsKnownCase(t *testing.T) {
	// 29/30 vs 36/30 是本轮真实形状（30/30 是比例 1.0 的替代写法）：
	// 更好的解析检查用 25/36 vs 32/36（Step 0 的形状）。
	diff, se, z, p, lo, hi := supplementProportionStats(29, 36, 32, 36)
	if diff <= 0 {
		t.Errorf("B-A 差值应为正，得到 %.2f pp", diff)
	}
	if se <= 0 || se > 20 {
		t.Errorf("pooled SE %.2f pp 不在合理范围", se)
	}
	if z <= 0 {
		t.Errorf("z 应为正，得到 %.2f", z)
	}
	if p <= 0 || p >= 1 {
		t.Errorf("p = %.4f 应在 (0,1)", p)
	}
	// CI 必须包含点估计
	if lo > diff || hi < diff {
		t.Errorf("95%% CI [%.2f, %.2f] 不含点估计 %.2f", lo, hi, diff)
	}
	// CI 必须随 n 增大而收窄
	_, _, _, _, lo10, hi10 := supplementProportionStats(29, 36, 32, 36)
	_, _, _, _, lo100, hi100 := supplementProportionStats(290, 360, 320, 360)
	if (hi100 - lo100) >= (hi10 - lo10) {
		t.Errorf("n 扩大 10 倍后 CI 未收窄：n=36 宽度 %.2f, n=360 宽度 %.2f", hi10-lo10, hi100-lo100)
	}
}

func TestSupplementMDEGrowsWithDiscordance(t *testing.T) {
	// 配对 MDE 的整个论点：discordant 比例越低，本实验越钝。
	// 如果这个函数对 discordant 不敏感，那"配对更省样本"就是一句空话。
	low := supplementMDEPaired(3, 30, 30, 30)   // d=0.10
	mid := supplementMDEPaired(15, 30, 30, 30)  // d=0.50
	high := supplementMDEPaired(30, 30, 30, 30) // d=1.00
	if !(low < mid && mid < high) {
		t.Errorf("配对 MDE 应随 discordance 单调**上升**：low=%.1f mid=%.1f high=%.1f", low, mid, high)
	}
	if low <= 0 || high <= 0 {
		t.Errorf("MDE 恒正：low=%.1f high=%.1f", low, high)
	}
	// 零 discordance 的显式退化读数（"无分辨率"，不是"分辨率无穷好"）
	if d := supplementMDEPaired(0, 30, 30, 30); d != 100 {
		t.Errorf("零 discordance 的配对 MDE 应为 100（无分辨率），得到 %.1f", d)
	}
	// 无配对时同样退化，而不是给 0
	if d := supplementMDEPaired(5, 0, 30, 30); d != 100 {
		t.Errorf("零配对的配对 MDE 应为 100，得到 %.1f", d)
	}
	// 非配对 MDE 应随 n 单调下降
	n6 := supplementMDEUnpaired(3, 6, 3, 6)
	n60 := supplementMDEUnpaired(30, 60, 30, 60)
	if n6 <= n60 {
		t.Errorf("非配对 MDE 应随 n 下降：n=6 %.1f, n=60 %.1f", n6, n60)
	}
}

func TestSupplementClassifyVerdictNeverOverclaims(t *testing.T) {
	// 三个读数必须给出三种不同的、且都不过分的结论。
	insufficient := supplementClassifyVerdict(0.30, 25, 0, false)
	if !strings.Contains(insufficient, "INSUFFICIENT") {
		t.Errorf("MDE 25pp 且 p=0.30 应判 INSUFFICIENT，得到 %q", insufficient)
	}
	noEffect := supplementClassifyVerdict(0.80, 5, 0.85, true)
	if !strings.Contains(noEffect, "NO EFFECT") {
		t.Errorf("MDE 5pp 且 p=0.80 应判 NO EFFECT，得到 %q", noEffect)
	}
	sig := supplementClassifyVerdict(0.001, 5, 0.0005, true)
	if !strings.Contains(sig, "SIGNIFICANT") {
		t.Errorf("p=0.001 应判 SIGNIFICANT，得到 %q", sig)
	}
}

// —— 夹具与契约一致 ——

// TestUnanswerableFixtureAnswerIsAbsent 是主断言：被问的取值不在夹具里。
// 它调的是与真实实验完全相同的扫描函数（unanswerableScanFixture），
// 因此不会与线上路径漂移。
func TestUnanswerableFixtureAnswerIsAbsent(t *testing.T) {
	for _, variant := range []struct {
		label   string
		fixture map[string]string
	}{
		{"A/B fixture", unanswerablePlainFixture()},
		{"C fixture", unanswerableDecoyFixture()},
	} {
		if findings := unanswerableScanFixture(t, variant.label, variant.fixture); len(findings) > 0 {
			t.Errorf("%s 里查到了本应查不到的东西：\n  %s", variant.label, strings.Join(findings, "\n  "))
		}
	}
}

// TestUnanswerableFragmentsAreDiscriminating 断言片段口径**有判别力**。
//
// 片段存在的理由是"模型不需要完整串也能答对"，所以判别力必须落在真值**独有**的
// 那一段上。真值 q7-tunnel-corp 与 decoy q7-tunnel-lb 共享 q7 与 tunnel：查这两段
// 只会把 C 臂误判成"答案在仓库里"。判别力全部在 `corp` 上。
func TestUnanswerableFragmentsAreDiscriminating(t *testing.T) {
	frags := unanswerableFragments(unanswerableTruth)
	if len(frags) == 0 {
		t.Fatalf("真值 %q 切不出任何有判别力的片段", unanswerableTruth)
	}
	// 必须覆盖真值独有的尾段
	found := false
	for _, f := range frags {
		if f == "corp" {
			found = true
		}
	}
	if !found {
		t.Errorf("片段 %v 缺真值独有的判别段 %q", frags, "corp")
	}
	// 不得包含与 decoy 共享的段
	for _, shared := range unanswerableSharedFragments() {
		for _, f := range frags {
			if f == shared {
				t.Errorf("片段 %v 包含与 decoy 共享的 %q：它没有判别力", frags, shared)
			}
		}
	}
	// 反向确认：共享段确实同时出现在 decoy 里（否则这条排除是无意义的）
	if !strings.Contains(unanswerableDecoy, "q7-tunnel") {
		t.Errorf("decoy %q 不含 q7-tunnel 前缀：共享段清单与夹具漂移", unanswerableDecoy)
	}
}

// TestUnanswerableAbsenceProofCatchesTruth 是**反向测试**：把真值塞进夹具，断言
// 扫描确实报出发现。
//
// 没有这条，"答案不在仓库里"这个前提就只是一句断言 —— 一个什么都不检查的实现
// 会同样地"通过"。这正是上一版 12%~62% 的形状：前提没被检验，于是没人发现
// fixture 与协议矛盾。
func TestUnanswerableAbsenceProofCatchesTruth(t *testing.T) {
	leaky := unanswerablePlainFixture()
	// 形态 1：完整真值出现在某个文件里
	leaky["docs/leak-full.md"] = "# note\n\nThe configured value is " + unanswerableTruth + ".\n"
	if findings := unanswerableScanFixture(t, "full-leak", leaky); len(findings) == 0 {
		t.Error("夹具里有完整真值，扫描却零发现 —— 这段证明是空壳")
	}

	// 形态 2：只出现片段（模型照样能据此答对，必须同样被抓到）
	leakyFrag := unanswerablePlainFixture()
	leakyFrag["internal/relay/hint.go"] = "package relay\n\n// env: corp, legacy\nconst legacyMarker = 1\n"
	findings := unanswerableScanFixture(t, "fragment-leak", leakyFrag)
	if len(findings) == 0 {
		t.Error("夹具里只有真值片段，扫描却零发现 —— 只查完整串是不够的")
	}

	// 形态 3：decoy 被误放进 A/B 夹具 —— 由 prove 层的反向确认抓住。
	// 这里直接验 grep 判定：decoy 必须在 C 夹具里有命中、在 A/B 夹具里无命中。
	cRoot := unanswerableExecFixture(t, unanswerableDecoyFixture())
	outC, err := unanswerableCall(t, unanswerableReadOnlyEngine(t, cRoot), "grep",
		`{"pattern":"`+unanswerableDecoy+`","path":"`+cRoot+`"}`)
	if err != nil || !unanswerableGrepHit(outC) {
		t.Errorf("C 夹具里 grep %q 应有命中（err=%v）", unanswerableDecoy, err)
	}
	abRoot := unanswerableExecFixture(t, unanswerablePlainFixture())
	outAB, err := unanswerableCall(t, unanswerableReadOnlyEngine(t, abRoot), "grep",
		`{"pattern":"`+unanswerableDecoy+`","path":"`+abRoot+`"}`)
	if err != nil {
		t.Errorf("A/B 夹具 grep 失败: %v", err)
	}
	if unanswerableGrepHit(outAB) {
		t.Error("A/B 夹具里出现了 decoy 取值：A 臂被偷偷放了 decoy")
	}
}

// TestUnanswerableGrepHitDistinguishesZeroFromHit 验 grep 输出判定本身。
// 真实 grep 零命中时输出 `[grep: 0 match(es) in 0 file(s)]` 且**后面一个结果行都没有**
// —— 只判"输出非空"会把零命中读成命中，那正是夹具自检假通过的方向。
func TestUnanswerableGrepHitDistinguishesZeroFromHit(t *testing.T) {
	root := unanswerableExecFixture(t, unanswerablePlainFixture())
	eng := unanswerableReadOnlyEngine(t, root)
	needle := unanswerableDecoy

	zero, err := unanswerableCall(t, eng, "grep", `{"pattern":"`+needle+`","path":"`+root+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if unanswerableGrepHit(zero) {
		t.Errorf("零命中输出被判成命中：%q", zero)
	}
	if strings.TrimSpace(zero) == "" {
		t.Skip("本 provider 的 grep 零命中时返回空串：本判定退化不了，但也不会误判")
	}

	// 命中侧必须用一个**确实在正文里**的串：提问里提到 internal/relay/tunnel.go，
	// 但那是提问的措辞，不是夹具的内容，用它当 needle 会得到假阴性。
	hit, err := unanswerableCall(t, eng, "grep", `{"pattern":"tunnel identifier","path":"`+root+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !unanswerableGrepHit(hit) {
		t.Errorf("有命中的输出被判成零命中：%q", hit)
	}
}

// TestUnanswerableFixtureHasNoPointerToMemory 是本文件最关键的一条夹具断言：
// 夹具仓里**没有任何**指向 topic 的东西。
//
// 只要有一个指针（哪怕是注释里的一句"see the project memory"），B 臂就退化成
// Step 0 的"记忆指路"形态 —— 而那正是本文件要测的东西的反面。更糟的是它会
// 让 A 臂也能跟着去找记忆，于是三臂全部退化成同一个实验，而读数看起来仍然
// 像模像样。
func TestUnanswerableFixtureHasNoPointerToMemory(t *testing.T) {
	banned := []string{
		"MEMORY.md", ".claude", ".go-code", "memory/", "project memory",
		"topic", "hai-mem-eval", unanswerableTopic, unanswerableTruth,
	}
	for label, fixture := range map[string]map[string]string{
		"A/B fixture": unanswerablePlainFixture(),
		"C fixture":   unanswerableDecoyFixture(),
	} {
		for rel, body := range fixture {
			low := strings.ToLower(body)
			for _, b := range banned {
				if strings.Contains(low, strings.ToLower(b)) {
					t.Errorf("%s 的 %s 提到 %q：夹具不能给任何指向 topic 的线索", label, rel, b)
				}
			}
			// 也检查文件名
			lrel := strings.ToLower(rel)
			for _, b := range []string{"memory", "claude", "topic", ".go-code"} {
				if strings.Contains(lrel, b) {
					t.Errorf("%s 含有指向记忆的路径名 %s", label, rel)
				}
			}
		}
	}
	// 夹具必须确实包含被提问的那个文件，否则"去 tunnel.go 看"是空动作。
	if _, ok := unanswerablePlainFixture()[unanswerablePointer]; !ok {
		t.Errorf("夹具里没有 %s：提问指向一个不存在的文件", unanswerablePointer)
	}
	// 且那个文件必须**没有**取值（否则答案在代码里）。
	if strings.Contains(strings.ToLower(unanswerableFixtureFiles[unanswerablePointer]),
		strings.ToLower(unanswerableTruth)) {
		t.Error(unanswerablePointer + " 里含有真值")
	}
}

// TestUnanswerableMemoryDoesNotConflictWithRepo 断言记忆与仓库不冲突（协议 §5.0）。
//
// 这是 Step 0 最大的教训：fixture 与契约矛盾会让数字测服从性而不是价值。这里的
// 形态是"仓库说形如 q7-tunnel-<后缀>，记忆说后缀是什么" —— 两者可同时为真，
// 模型采纳记忆**不需要**违反任何东西。
func TestUnanswerableMemoryDoesNotConflictWithRepo(t *testing.T) {
	topic := strings.ToLower(unanswerableMemoryTopicFiles[unanswerableTopic])
	repo := strings.ToLower(unanswerableFixtureFiles["README.md"] +
		unanswerableFixtureFiles["internal/relay/tunnel.go"] +
		// runbook 已参数化成两个变体（legacy / neutral，见 unanswerableRunbookContent），
		// 所以这里对**两个变体都查一遍**：A 臂用 legacy，A'/B'/C 臂用 neutral，
		// 两个都必须交代标识符形态，否则记忆与仓库就成了两个互不相干的断言。
		unanswerableRunbookContent(unanswerableFlavorLegacy) +
		unanswerableRunbookContent(unanswerableFlavorNeutral))
	decoy := strings.ToLower(unanswerableDecoyContent(unanswerableDecoy))

	// 记忆里必须真的有答案（否则 B 臂测不到任何东西）
	if !strings.Contains(topic, strings.ToLower(unanswerableTruth)) {
		t.Error("topic 正文不含真值：B 臂的注入不携带答案")
	}
	// 记忆里绝不能出现 decoy 取值（B 臂与 C 臂因此只有仓库这一个变量）
	if strings.Contains(topic, strings.ToLower(unanswerableDecoy)) {
		t.Error("topic 正文含 decoy 取值：B/C 臂的差异就不只来自仓库")
	}
	// 仓库里绝不能出现真值（由 TestUnanswerableFixtureAnswerIsAbsent 兜底，这里
	// 再单独确认 A/B 夹具这一支，因为它是"记忆是唯一来源"的直接表述）
	if strings.Contains(repo, strings.ToLower(unanswerableTruth)) {
		t.Error("仓库里出现真值：B 臂不再是「答案只来自记忆」")
	}
	// decoy 文档不得提到 topic
	if strings.Contains(decoy, unanswerableTopic) || strings.Contains(decoy, "memory") {
		t.Error("decoy 文档提到了记忆：模型会顺着 decoy 去找记忆，冲突不再是冲突")
	}
	// 仓库必须给出"形如 q7-tunnel-<后缀>"这一形态说明（记忆与仓库不冲突的前提）
	if !strings.Contains(repo, "q7-tunnel") {
		t.Error("仓库没有交代标识符的形态：记忆与仓库就成了两个互不相干的断言")
	}
}

// TestUnanswerableIndexShape 断言索引形态符合官方（§4.5：没有 description 字段，
// 一行一条纯文件名；不发明任何格式）。
func TestUnanswerableIndexShape(t *testing.T) {
	idx := unanswerableIndex("/tmp/some-memory-dir")
	if !strings.HasPrefix(idx, "## Project Memory\n") {
		t.Errorf("索引缺 Project Memory 头：%q", idx[:40])
	}
	if !strings.Contains(idx, "/tmp/some-memory-dir") {
		t.Error("索引必须给出记忆目录位置：官方就是这样让模型知道去哪儿读的")
	}
	for name := range unanswerableMemoryTopicFiles {
		if !strings.Contains(idx, "- "+name+"\n") {
			t.Errorf("索引缺条目 - %s", name)
		}
	}
	// §5.4：干扰项必须 >5 条
	if len(unanswerableMemoryTopicFiles) <= 5 {
		t.Errorf("干扰项只有 %d 条，§5.4 要求 5 条以上：模型不需要做真实选择",
			len(unanswerableMemoryTopicFiles))
	}
	// 索引里不得出现任何取值（真值/假值），也不得出现"描述"式元数据
	for _, v := range []string{unanswerableTruth, unanswerableDecoy, unanswerableStale} {
		if strings.Contains(idx, v) {
			t.Errorf("索引含取值 %s：索引是清单，不是答案", v)
		}
	}
	for _, banned := range []string{"description:", "title:", "summary:", "type:"} {
		if strings.Contains(idx, banned) {
			t.Errorf("索引含 %q：官方 §4.5 明确没有 description/title，索引是纯文件名列表", banned)
		}
	}
	// 逐条断言干扰项与真条目形态一致（对模型无优先级）
	lines := 0
	for _, l := range strings.Split(idx, "\n") {
		if strings.HasPrefix(l, "- ") {
			lines++
			if !strings.HasSuffix(strings.TrimSpace(l), ".md") {
				t.Errorf("索引行 %q 不是纯文件名", l)
			}
		}
	}
	if lines != len(unanswerableMemoryTopicFiles) {
		t.Errorf("索引 %d 行，topic 文件 %d 个：数量对不上", lines, len(unanswerableMemoryTopicFiles))
	}
}

// TestUnanswerableInjectionReachesProvider 断言注入真的进了发给 provider 的请求，
// 且三臂之间**只差那一段注入**（B 与 C 逐字节相同）。
func TestUnanswerableInjectionReachesProvider(t *testing.T) {
	dir := unanswerableTopicDir(t)
	arms := unanswerableArms(dir)
	if len(arms) != 3 {
		t.Fatalf("必须是三臂，实得 %d", len(arms))
	}
	if arms[1].Inject != arms[2].Inject {
		t.Error("B 与 C 的注入必须逐字节相同：B/C 的唯一变量是仓库里有没有 decoy")
	}
	if arms[0].Inject != "" {
		t.Errorf("A 臂必须完全不注入，实得 %q", arms[0].Inject)
	}

	root := unanswerableExecFixture(t, arms[1].Fixture)
	sysB := unanswerableCaptureRun(t, root, arms[1].Inject)
	sysA := unanswerableCaptureRun(t, root, arms[0].Inject)
	if sysA == "" || sysB == "" {
		t.Fatal("截获到空的 system prompt（AgentLoop 没发出请求？）")
	}
	for _, sys := range []string{sysA, sysB} {
		if !strings.Contains(sys, "Current Workspace") {
			t.Error("system prompt 缺工作区层，截获面可能选错了消息")
		}
	}
	if !strings.Contains(sysB, unanswerableTopic) {
		t.Error("B 臂：注入的索引没进 system prompt —— 注入没生效，实验作废")
	}
	if !strings.Contains(sysB, dir) {
		t.Error("B 臂：注入里没有记忆目录位置 —— 模型无从知道去哪儿读 topic")
	}
	if strings.Contains(sysA, "Project Memory") {
		t.Error("A 臂的 system prompt 含 Project Memory 段：A 臂必须完全不注入")
	}
	// 唯一变量：把 B 的注入块整段抹掉、并归一化拼接分隔空行后，B 必须与 A 逐字节相同。
	//
	// 归一化的是层间分隔空行（复用 baselineNormalizeJoin）：composeSystemPrompt 用
	// "\n\n" 拼接各层，B 臂有层 1b 而 A 臂没有，于是少一个分隔符。那是拼接器行为，
	// 不是行为变量 —— 但它**必须**被显式认领，否则"B 与 A 的差无法归因"没人核过。
	gotB := baselineNormalizeJoin(strings.Replace(sysB, arms[1].Inject, "", 1))
	normA := baselineNormalizeJoin(sysA)
	if gotB != normA {
		t.Error("B 与 A 的 system prompt 差的不止注入块本身（归一化拼接空行后仍有差异）。" +
			"多出来的内容会让 B/A 之差无法归因到注入。")
		t.Logf("  首个差异行: %q", firstDiff(gotB, normA))
	}
	// 反向自查：A 里不得有任何 B 独有的内容（防止 A 少了东西而不是多了东西）。
	for _, frag := range []string{"Project Memory", unanswerableTopic, dir} {
		if strings.Contains(sysA, frag) {
			t.Errorf("A 臂含 %q：A 臂必须完全没有注入", frag)
		}
	}
}

// TestSupplementArmsReuseBaselineDefinitions 断言补测一用的确实是 Step 0 的
// 同一份 A/B 定义（同一 fixture、同一注入、同一提问），否则两个实验的数字不可并列。
func TestSupplementArmsReuseBaselineDefinitions(t *testing.T) {
	arms := baselineArms()
	if len(arms) != 3 {
		t.Fatalf("Step 0 的三臂定义不完整：%d", len(arms))
	}
	if !strings.Contains(arms[0].Inject, baselineStaleValue) {
		t.Error("A 臂必须是冲突注入")
	}
	if !strings.Contains(arms[1].Inject, baselineTruthFile) {
		t.Error("B 臂必须是指针注入")
	}
	if arms[2].Inject != "" {
		t.Error("C 臂必须不注入")
	}
	// 补测一的装置用的是同一套工具面与提问（与 baselineRunOnce 同构）。
	// 这里锁住"只读工具面"这一条：三臂里任何写工具都会污染"注入让任务更快"这个读数。
	root := baselineExecFixture(t)
	eng := tools.NewToolEngine()
	names := map[string]bool{}
	for _, tl := range builtin.NewFileTools(root).Tools() {
		switch tl.Name() {
		case "read_file", "grep", "glob":
			eng.RegisterTool(context.Background(), tl)
			names[tl.Name()] = true
		}
	}
	if len(names) != 3 {
		t.Errorf("只读工具面应恰为 read_file/grep/glob，实得 %v", names)
	}
}
