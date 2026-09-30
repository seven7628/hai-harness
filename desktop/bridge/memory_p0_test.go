package main

// P0 端到端验收：注入记忆目录说明之后，Step 3/4 那条链在真实 provider 上转不转。
//
// # 这份装置为什么必须存在
//
// 2026-09-30 之前，Step 3/4 的单元测试与接线断言**全绿**，而整条链在真实会话里
// 恒不触发：注入文本不含目录路径 → 模型无从得知写入目标 → postWrite 的
// "路径落在记忆目录内"永不成立。测试证明的是"每个零件都在"，不是"整条链会转"。
//
// # 三条设计约束，每条都来自 2026-09-29 那次实验作废的教训
//
//  1. **判定是确定性的，不��� LLM 判定器。** 指标全部是"某个文件是否存在、某个
//     frontmatter 字段是否被宿主补上"——读文件系统即可。9-29 那次用模型判"这段
//     是不是记忆"，自洽率只有 89%，整个读数因此作废。这里没有判定器可失效。
//  2. **两臂只差注入段。** A 臂用**生产代码路径**（newMemoryHost().injection）
//     现算，不是手写的实验文案——测的就是要发布的那段文本。B 臂完全不注入。
//     TestP0ArmsDifferOnlyByInjection 断言除注入外逐字节相同。
//  3. **每 rep 全隔离。** 独立 HOME、独立 git 仓库、独立记忆目录，避免上一 rep 的
//     记忆泄漏进下一 rep。
//
// # 预期与解读
//
// 官方明说 "Claude doesn't save something every session"，所以 A 臂**不该**是
// 100%。可证伪的预测是方向而不是水平：**A 显著高于 B，且 B 应为 0**
// （模型完全不知道有记忆这回事时不会凭空去写）。9-29 的旁证是 4/5 vs 0/5。
//
// 若 A 臂也是 0，那就是 P0 的反推论证（Wiki §4.1）在真实模型上不成立——
// 说明模型需要的不只是路径，而是某种更强的邀请。那正是 P1 判据文本要回答的问题。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// p0RepsFloor 是重复次数地板。
//
// 2026-09-30 从 8 提到 20，原因是**地板设错了**而不是模型变了：实测单次会话写入
// 概率约 0.17（4/24），于是 n=8 时"一次都没写"的概率是 0.83^8 ≈ **22%** ——
// 机制断言（至少一次全链转通）每跑五次就会误报一次。实测确实撞上过：整包跑批
// FAIL 而单独跑 PASS。
//
// n=20 时 0.83^20 ≈ 2.4%，可接受。代价是每轮约 11 分钟，所以跑批走 nohup 写日志文件。
//
// 更小的 n 也不是不能用来估写入**率**——只是不能用来断言"机制成立"，因为那个断言
// 是"出现一次即成立"，对 n 的要求是相反方向的：要的是"几乎不可能一次都不出现"。
const p0RepsFloor = 20

func p0Reps(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("HAI_EVAL_P0_REPS")
	if raw == "" {
		return p0RepsFloor
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		t.Fatalf("HAI_EVAL_P0_REPS=%q 解析失败（要一个正整数）", raw)
	}
	if n < p0RepsFloor {
		t.Fatalf("HAI_EVAL_P0_REPS=%d < 地板 %d：单次写入概率约 0.17，n 太小时"+
			"「一次都没写」是个大概率事件，机制断言会 flake", n, p0RepsFloor)
	}
	return n
}

// p0Run 一次 rep 的全部读数。字段全是**可从文件系统复核**的事实。
type p0Run struct {
	PairIndex int    `json:"pair_index"`
	Arm       string `json:"arm"`
	Err       string `json:"err,omitempty"`

	// 以下四项是判定，全部读文件系统，不经任何模型。
	WroteTopic      bool `json:"wrote_topic"`
	HasFrontmatter  bool `json:"has_frontmatter"`
	HasType         bool `json:"has_type"`
	StampedModified bool `json:"stamped_modified"` // ← 宿主 postWrite 真的跑过了
	IndexUpdated    bool `json:"index_updated"`

	Topics   []string `json:"topics,omitempty"`
	Duration float64  `json:"duration_sec"`

	// 以下三项把"没写成"拆开：模型**根本没试**、试了但**写错了地方**、
	// 试了且写对了地方但**被拒**。不拆开的话，"写入率只有 17%"这个数字
	// 无法区分"模型不感兴趣"与"我们的写入路径坏了"——而两者的处置完全相反。
	WriteAttempts  int      `json:"write_attempts"`
	WriteRejected  []string `json:"write_rejected,omitempty"`
	WriteElsewhere int      `json:"write_elsewhere"` // 写了 .md，但不在记忆目录里

	// —— P1 的两个判据（2026-09-30）——
	//
	// 之前只有 chainTurned（= 写了带 frontmatter 且被补戳的 topic），它只答
	// "写没写"，而**判据是精确率工具**：它压掉的是不该写的那部分。所以必须
	// 把"写了什么"也记下来，否则 C 臂因为正确地不写而拿 0 分（第一版就是这样）。
	//
	// 判定**读文件内容做关键词匹配**，不经 LLM 判定器——9-29 那次用模型判
	// "这段是不是记忆"，自洽率只有 89%，整个读数因此作废。
	//
	// Remembered 写了**用户口述、代码里没有**的事实（该记的记了）
	// RecordedDerivable 写了**代码里直接能读出**的内容（不该记的记了）
	Remembered        bool `json:"remembered"`
	RecordedDerivable bool `json:"recorded_derivable"`
	RecordedAllTokens int  `json:"recorded_all_tokens"` // 命中过的 token 数（诊断用）

	// Body 全部 topic 的正文拼接。仅供 classifyRun 与它的自检用，**不进 JSON**
	//（跑批的原始数据不该被一段可能很大的正文撑爆；要复现判定看 topics 即可）。
	Body string `json:"-"`
}

// classifyRun 按关键词把记忆正文分成"该记的"与"不该记的"。
//
// 判定**读文件内容做关键词匹配**，不经 LLM 判定器——9-29 那次用模型判"这段是不是
// 记忆"，自洽率只有 89%，整个读数因此作废。
//
// 词表在 p0Turns 旁边（那里说明了为什么这两个集合必须与仓库内容不相交）。
// 任一条命中即算命中：这是召回口径（"记没记下来"），不是"每条都对"。
func classifyRun(r *p0Run, body string) {
	for _, tok := range p0RememberTokens {
		if strings.Contains(body, tok) {
			r.Remembered = true
		}
	}
	for _, tok := range p0DerivableTokens {
		if strings.Contains(body, tok) {
			r.RecordedDerivable = true
			r.RecordedAllTokens++
		}
	}
}

// chainTurned 一条链是否真的转起来了：写了 topic、带 frontmatter、且 modified 被补上。
//
// 要求三者同时成立。少任何一环都说明链在某处断了：没有 topic = 模型没写；
// 没有 frontmatter = 格式没被传达（模型猜的）；没有 modified = 钩子没命中。
func (r p0Run) chainTurned() bool {
	return r.WroteTopic && r.HasFrontmatter && r.StampedModified
}

// p0Fixture 建一个"藏着一条非显然约定"的小仓库：它给模型一个值得记下的理由，
// 但**不**提示"去查记忆"、也**不**提示"你应该记录什么"——判据文本正是 P1 要测的东西，
// 这里必须为空，否则就污染了变量。
func p0Fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "# relay\n\n一个用于长连接转发的服务。\n")
	write("config/app.conf", "listen=0.0.0.0:8443\nbacklog=128\n")
	// 这条约定藏在代码里且带反直觉注释——正是"能从代码推导"的那一类，
	// 官方明说这类**不该**进记忆。所以它不能作为"写入了就算成功"的依据，
	// 只能提高模型去读代码的概率。是否写入由模型自行判断。
	write("internal/forwarder.go", "package internal\n\n"+
		"// 重连退避上限 5s：调高会让上游 429 放大成雪崩，别动。\n"+
		"const maxBackoff = 5\n")
	initGitRepo(t, dir)
	return dir
}

// p0Turns 是本次跑批的两轮提问，三臂**逐字节相同**，且都不含与记忆有关的暗示。
//
// # 为什么是两轮（2026-09-30 改）
//
// 第一版只有一轮、且是纯只读的仓库问答。那一版**测不出判据的作用**，原因是它把
// "唯一值得记的东西"设成了代码里那条注释：
//
//	// 重连退避上限 5s：调高会让上游 429 放大成雪崩，别动。
//
// 而官方判据第一条原文就是 "skips anything it can derive from the codebase"。
// 于是加判据的模型**照判据办事、不写**，装置却奖励写入——把"答对了"记成"失败"。
// 实测 C 臂 0/20 对 A 臂 4/20，方向正好是这么来的（见 Wiki §6.1）。
//
// 现在第一轮给出两条**仓库里根本不存在**的事实（发布窗口、排查顺序），
// 它们只存在于这次对话里，正是官方说的 "working preferences" 与
// "decisions that Claude can't derive from the code or git history"。
// 于是"该记的"与"不该记的"同时在场，判据才有可测的作用面。
//
// 第二轮保持原来的只读任务：它本身不提供新的可记内容，是**干扰项**。
var p0Turns = []string{
	// —— 第一轮：用户交代两条仓库里没有的约定 ——
	"补充两条这个仓库里查不到、但你之后每次都会用到的约定：\n" +
		"1) 发布窗口：每周四 15:00 之后到下周一 10:00 之间不要合并主干。\n" +
		"2) 排查线上问题：先看 relay-metrics 面板，再看日志，最后才考虑重启。\n" +
		"现在请阅读这个仓库，说明 relay 服务监听哪个端口、backlog 是多少，" +
		"以及重连退避上限为什么不能调高。",
	// —— 第二轮：普通追问，不含新事实 ——
	"再确认一下：刚才说的端口和 backlog，在配置文件里是哪几行写的？",
}

// p0FactTokens 把记忆内容分成两类，用于**确定性**判定（不经 LLM 判定器）。
//
// 该记的（只出现在用户口述里，仓库中无）：p0RememberTokens
// 不该记的（能从代码/配置直接读出）：p0DerivableTokens
var (
	// p0RememberTokens 用户口述事实的判定词。挑的是**只在对话里出现过**的词形：
	// 仓库里既没有这些中文表述，也没有这些数字组合，所以命中即说明它记的是
	// 用户说的东西，而不是从代码里抄的。
	p0RememberTokens = []string{"发布窗口", "relay-metrics", "排查"}
	// p0DerivableTokens 纯代码可推导内容的判定词。官方判据要求跳过这一类。
	p0DerivableTokens = []string{"8443", "backlog", "maxBackoff", "重连退避"}
)

func p0RunOnce(t *testing.T, p provider.Provider, prov, model, root string,
	mh *memoryHost, inject string, pairIdx int, arm string, turns ...string) p0Run {

	t.Helper()
	ft := builtin.NewFileTools(root)
	if mh != nil {
		ws := root
		ft = ft.WithPostWrite(func(ctx context.Context, p string) (string, error) {
			return mh.postWrite(ctx, ws, p)
		})
	}
	eng := tools.NewToolEngine()
	// 工具面刻意**不含 bash**：记忆必须由 write_file / edit_file 写，
	// 那正是 postWrite 钩子的唯一入口。放开 bash 等于给模型一条绕过钩子的路，
	// 测出来的 modified 率就不再代表产品。
	for _, tl := range ft.Tools() {
		switch tl.Name() {
		case "read_file", "grep", "glob", "write_file", "edit_file":
			eng.RegisterTool(context.Background(), tl)
		}
	}

	run := p0Run{PairIndex: pairIdx, Arm: arm}
	opts := []agents.Option{
		agents.WithProvider(p),
		agents.WithProviderName(prov),
		agents.WithModel(model),
		agents.WithToolEngine(eng),
		agents.WithWorkingDir(root),
		agents.WithMaxIterations(12),
		agents.WithMaxRetries(2),
		agents.WithLLMTimeout(4 * time.Minute),
	}
	// 走生产装配点：会话系统提示词里的 memory 分���就是这一段。
	opts = append(opts, agents.WithMemoryInjection(inject))

	var mu sync.Mutex
	var final string
	var writeAttempts int
	var writeRejected []string
	loop := agents.NewAgentLoop(opts...)
	rec := newBaselineToolRecorder()
	recHandle := rec.handle(root)
	handler := events.EventHandler(func(ctx context.Context, ev events.Event) {
		recHandle(ctx, ev)
		// 写尝试单独记：ToolResponse 带 Arguments，能看出模型写到了哪、是否被拒。
		if e, ok := ev.(*events.ToolResponse); ok &&
			(e.Name == "write_file" || e.Name == "edit_file") {
			mu.Lock()
			writeAttempts++
			if e.IsError {
				writeRejected = append(writeRejected, e.Result)
			}
			mu.Unlock()
		}
		if e, ok := ev.(*events.AgentEnd); ok {
			mu.Lock()
			defer mu.Unlock()
			final = e.Content
		}
	})

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	msgs := make([]core.Message, 0, len(turns))
	for _, t := range turns {
		msgs = append(msgs, core.NewUserMessage(core.Content{
			Type: core.ContentTypeText, Content: t,
		}))
	}
	ac := loop.Run(ctx, msgs, handler)
	cancel()
	run.Duration = time.Since(start).Seconds()
	if ac != nil && ac.Err != nil {
		run.Err = ac.Err.Error()
	}
	mu.Lock()
	run.WriteAttempts, run.WriteRejected = writeAttempts, writeRejected
	mu.Unlock()
	_ = final

	if mh != nil && mh.dir != nil {
		run = p0Inspect(t, *mh.dir, run)
	}
	return run
}

// p0Inspect 读记忆目录的落盘状态填进读数。**全部是确定性检查**。
func p0Inspect(t *testing.T, dir memory.Dir, run p0Run) p0Run {
	t.Helper()
	entries, err := os.ReadDir(dir.Path)
	if err != nil {
		return run
	}
	var topics []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.EqualFold(filepath.Ext(name), ".md") ||
			strings.EqualFold(name, memory.IndexFileName) {
			continue
		}
		topics = append(topics, name)
	}
	run.Topics = topics
	run.WroteTopic = len(topics) > 0

	// frontmatter / type / modified 取**任意一个** topic：只要有一条走通全链，
	// 就证明宿主那一侧是通的；多条之间的差异由分类表呈现，不在这里做取舍。
	if run.WroteTopic {
		body, rerr := os.ReadFile(filepath.Join(dir.Path, topics[0]))
		if rerr == nil {
			s := string(body)
			run.HasFrontmatter = strings.HasPrefix(strings.TrimLeft(s, "\uFEFF \t\r\n"), "---")
			run.HasType = strings.Contains(s, "type:")
			run.StampedModified = strings.Contains(s, "modified:")
		}
	}
	if b, rerr := os.ReadFile(filepath.Join(dir.Path, memory.IndexFileName)); rerr == nil {
		run.IndexUpdated = strings.Contains(string(b), topics[0])
	}

	// 分类要读**全部** topic 而不是 topics[0]：一个 session 可能写好几条，
	// 只看第一条会把"第一条写错了"误判成"没写"。任一条命中即算命中（这是
	// 召回口径：要问的是"记没记下来"，不是"每条都对"）。
	var all strings.Builder
	for _, name := range topics {
		if body, rerr := os.ReadFile(filepath.Join(dir.Path, name)); rerr == nil {
			all.Write(body)
			all.WriteString("\n")
		}
	}
	body := all.String()
	classifyRun(&run, body)
	run.Body = body
	return run
}

// precision 该次会话"写对了吗"：写了东西且**没**写纯代码可推导的内容。
//
// 只有"没写"才谈得上精确——不写既不精确也不失误，那是召回问题，由 Remembered 管。
func (r p0Run) precision() bool {
	return r.WroteTopic && !r.RecordedDerivable
}

// TestP0GuidanceCausesModelToWriteMemory 是 P0 的端到端验收。
//
// 三臂交错执行（A→B→C 一轮算一次配对），漂移在三臂间均摊：
//
//	A 现状注入（目录 + frontmatter 格式，无行为判据）
//	B 完全不注入（模型压根不知道有记忆这回事）
//	C A + 官方 §2.2 的行为判据（**P1 的候选**）
//
// C 臂的判据文本逐句照抄官方，不自创：
//
//	"Claude skips anything it can derive from the codebase, such as
//	 architecture, file paths, or debugging fixes. It also skips anything your
//	 CLAUDE.md files already say."
//	"Claude doesn't save something every session. It decides what's worth
//	 remembering based on whether the information would be useful in a future
//	 conversation."
//
// 上一版自创 6 条行为规则是 12%~62% 采纳率的来源；这次的区别是**有原文可依**。
// 但"照抄"不等于"该加"——C 是否真的更好仍要由本装置测出来，故它先是实验臂。
func TestP0GuidanceCausesModelToWriteMemory(t *testing.T) {
	prov, model, p := baselineEnvOrSkip(t)
	baselinePing(t, p, prov, model)
	reps := p0Reps(t)

	t.Logf("provider=%s model=%s | reps/臂=%d | 装置：真实产品注入 vs 完全不注入", prov, model, reps)

	var runs []p0Run
	for i := 0; i < reps; i++ {
		// A 臂：走生产路径算注入（newMemoryHost().injection），不手写文案。
		homeA := t.TempDir()
		t.Setenv("HOME", homeA)
		wsA := p0Fixture(t)
		hA := newMemoryHost(memory.Config{Directory: filepath.Join(homeA, "mem")})
		injA := hA.injection(context.Background(), wsA)
		if injA == "" {
			t.Fatalf("A 臂注入为空——生产路径本身坏了，实验无从谈起")
		}
		runA := p0RunOnce(t, p, prov, model, wsA, hA, injA, i, "A", p0Turns...)
		runs = append(runs, runA)
		p0LogRun(t, runA)

		// B 臂：**同样装配 memoryHost**（同样有目录、同样的 postWrite 钩子），
		// 只是不注入任何文本。
		//
		// 这一条是 2026-09-30 的自我修正：初版给 B 臂不装配 host，于是两臂差的
		// 是"注入段 + 记忆目录 + 钩子"三项而不是一项，B 臂的落盘状态也无从测量——
		// 正好违反本文件开头写下的约束 2。现在两臂在文件系统上完全对称，
		// 唯一的差别就是 inj。
		homeB := t.TempDir()
		t.Setenv("HOME", homeB)
		wsB := p0Fixture(t)
		hB := newMemoryHost(memory.Config{Directory: filepath.Join(homeB, "mem")})
		// 先解析出目录（B 臂不注入，但钩子与测量都需要它指向一个真实位置）。
		if _, err := hB.resolve(context.Background(), wsB); err != nil {
			t.Fatalf("B 臂解析记忆目录失败：%v", err)
		}
		runB := p0RunOnce(t, p, prov, model, wsB, hB, "", i, "B", p0Turns...)
		runs = append(runs, runB)
		p0LogRun(t, runB)

		// C 臂：现状注入 + 官方行为判据。与 A 臂**只差这一段**。
		homeC := t.TempDir()
		t.Setenv("HOME", homeC)
		wsC := p0Fixture(t)
		hC := newMemoryHost(memory.Config{Directory: filepath.Join(homeC, "mem")})
		injC := hC.injection(context.Background(), wsC)
		if injC == "" {
			t.Fatalf("C 臂基础注入为空")
		}
		injC += "\n\n" + p0CriteriaOfficial
		runC := p0RunOnce(t, p, prov, model, wsC, hC, injC, i, "C", p0Turns...)
		runs = append(runs, runC)
		p0LogRun(t, runC)
	}

	// 原始读数先落盘：判定是确定性的，但原始数据比任何汇总都值钱。
	if dir := os.Getenv("HAI_EVAL_REPORT_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			b, _ := json.MarshalIndent(runs, "", "  ")
			_ = os.WriteFile(filepath.Join(dir, "p0.json"), b, 0o644)
		}
	}

	// 四组读数，每臂一份：
	//   chain   全链转通（机制：写了 → 带 frontmatter → 宿主补 modified）
	//   recall  记下了**用户口述、代码里没有**的事实（该记的记了）
	//   prec    写了且**没**写纯代码可推导的内容（不该记的没记）
	//   junk    写了纯代码可推导的内容（不该记的记了）
	type armStat struct{ chain, recall, prec, junk, attempt, write int }
	stat := map[string]*armStat{"A": {}, "B": {}, "C": {}}
	for _, r := range runs {
		st := stat[r.Arm]
		st.attempt += r.WriteAttempts
		if r.WroteTopic {
			st.write++
		}
		if r.chainTurned() {
			st.chain++
		}
		if r.Remembered {
			st.recall++
		}
		if r.precision() {
			st.prec++
		}
		if r.RecordedDerivable {
			st.junk++
		}
	}
	a, b, c := stat["A"], stat["B"], stat["C"]

	// 统计口径与 Step 0 / 补测一**同源**（复用 supplementMcNemarExact /
	// supplementWilsonCI），避免两台仪器各报各的。
	b01, b10 := 0, 0
	for i := 0; i < reps; i++ {
		aOn := runs[i*3].chainTurned()
		bOn := runs[i*3+1].chainTurned()
		if aOn && !bOn {
			b01++
		} else if bOn && !aOn {
			b10++
		}
	}
	disc, pMc, pOK := supplementMcNemarExact(b01, b10)
	// A/C 的配对比较才是 P1 的问题（C = 加了行为判据）。单独算，不要拿 A/B 的
	// p 值去回答"C 有没有用"。
	//
	// **比较的是精确率，不是 chainTurned**（2026-09-30 修正）：初版这里还在算
	// chainTurned，而两臂在那个指标上都是 20/20，算出来是"无不一致对、p 不可报"，
	// 看起来像没差异；真正的差异（垃圾 4/20 vs 1/20）在 chainTurned 里根本看不见
	// ——它只看"有没有写"，而两臂都写了。拿错指标去回答问题，结论就是空的。
	c01, c10 := 0, 0 // 精确率不一致对
	r01, r10 := 0, 0 // 召回不一致对
	for i := 0; i < reps; i++ {
		aRun, cRun := runs[i*3], runs[i*3+2]
		if aRun.precision() && !cRun.precision() {
			c01++
		} else if cRun.precision() && !aRun.precision() {
			c10++
		}
		if aRun.Remembered && !cRun.Remembered {
			r01++
		} else if cRun.Remembered && !aRun.Remembered {
			r10++
		}
	}
	mde := supplementMDEUnpaired(0, reps, 1, reps)

	pc := func(st *armStat, k string) int {
		switch k {
		case "chain":
			return st.chain
		case "recall":
			return st.recall
		case "prec":
			return st.prec
		case "junk":
			return st.junk
		case "write":
			return st.write
		}
		return st.attempt
	}
	for _, k := range []string{"write", "chain", "recall", "prec", "junk"} {
		name := map[string]string{
			"write": "写了任何东西", "chain": "全链转通",
			"recall": "记下用户口述事实", "prec": "写对（无垃圾）", "junk": "记了代码可推导内容",
		}[k]
		t.Logf("%-18s A=%d/%d  B=%d/%d  C=%d/%d", name,
			pc(a, k), reps, pc(b, k), reps, pc(c, k), reps)
	}
	t.Logf("写尝试（分母都是 %d 次会话） A=%d  B=%d  C=%d", reps, a.attempt, b.attempt, c.attempt)

	// P1 的主问题：**精确率**（不该记的没记）在 C 臂有没有变好，代价是召回掉了多少。
	cPrecLo, cPrecHi := supplementWilsonCI(c.prec, reps)
	aPrecLo, aPrecHi := supplementWilsonCI(a.prec, reps)
	t.Logf("精确率 Wilson  A=%d/%d [%.0f%%–%.0f%%]  C=%d/%d [%.0f%%–%.0f%%]（MDE ≈ %.0f pp）",
		a.prec, reps, 100*aPrecLo, 100*aPrecHi, c.prec, reps, 100*cPrecLo, 100*cPrecHi, mde)
	if pOK {
		t.Logf("配对 McNemar：不一致对 %d（仅A %d / 仅B %d），双侧精确 p=%.3f", disc, b01, b10, pMc)
	} else {
		t.Logf("配对 McNemar：无不一致对，p 不可报（那是分辨率不足，不是零假设被接受）")
	}
	t.Logf("本装置在 n=%d/臂 下能分辨的最小差值 ≈ %.0f 个百分点", reps, mde)
	cDisc, cP, cPOK := supplementMcNemarExact(c01, c10)
	if cPOK {
		t.Logf("A vs C · 精确率：不一致对 %d（仅A %d / 仅C %d），双侧精确 p=%.3f",
			cDisc, c01, c10, cP)
	} else {
		t.Logf("A vs C · 精确率：无不一致对，p 不可报（分辨率不足，不是零假设被接受）")
	}
	rDisc, rP, rPOK := supplementMcNemarExact(r01, r10)
	if rPOK {
		t.Logf("A vs C · 召回：  不一致对 %d（仅A %d / 仅C %d），双侧精确 p=%.3f"+
			"（>0.05 = 判据没有伤召回）", rDisc, r01, r10, rP)
	} else {
		t.Logf("A vs C · 召回：  无不一致对（判据没有伤召回）")
	}

	// —— 断言一（强，可证伪）：机制真的转起来了 ——
	//
	// 这条只需要**一次**成功就成立：改动之前，"模型写入 → 宿主补 modified"在结构上
	// 不可能发生（模型不知道往哪写），所以 0 次是必然的，出现 1 次就是反证。
	if a.chain == 0 {
		t.Fatalf("A 臂（真实产品注入）0/%d 全链转通：模型从未写出带 frontmatter 的记忆，"+
			"或宿主钩子从未命中——P0 的机制假设不成立", reps)
	}

	// —— 断言二（弱，只查装置不脏）：B 臂必须为 0 ——
	//
	// B 臂的模型完全不知道有记忆这回事，它写出的任何文件都不可能带 frontmatter。
	// B > 0 说明装置被污染（上一 rep 泄漏、或工具面漏了），此时 A−B 的读数不可用。
	if b.chain > 0 {
		t.Fatalf("B 臂（完全不注入）却转通了 %d 次全链——装置有污染，B 臂不干净", b.chain)
	}

	// —— 断言三：不下结论 ——
	//
	// 写入**率**是不是被注入抬高了，本装置在 n=8 时给不出有意义的答案
	//（1/8 vs 0/8 的双侧精确 p=1.0）。那需要更多 rep，且属于 P1 的问题域
	//（"模型需要的不只是路径"）。此处只把分辨率打印出来，避免 PASS 被误读成
	// "效应显著"。
	if pOK && pMc >= 0.05 {
		t.Logf("注意：本次 n=%d 不足以断言效应显著（p=%.3f）。机制已证，效应量待测。", reps, pMc)
	}
	// P1 的判定门槛：**精确率变好，且召回没有塌**。
	//
	// 只看精确率是不够的——一条判据只要让模型什么都不写，精确率就是 100%。
	// 只看召回也不够——A 臂那种"什么都记"召回很高但全是垃圾。两个都要，且
	// 第二个必须是**硬门槛**。
	//
	// 仍然只打日志、不改断言：C 更好不是把判据搬进生产包的充分条件，那要在
	// 读完数据之后单独决定（判据文本至今留在测试文件里）。
	switch {
	case c.prec > a.prec && c.recall >= a.recall-2:
		if cPOK && cP < 0.05 {
			t.Logf("判据的效果：C 精确率 %d/%d vs A %d/%d（p=%.3f，显著）、召回未塌"+
				"（%d vs %d）——方向与统计都支持，可以考虑搬进 guidance.go。",
				c.prec, reps, a.prec, reps, cP, c.recall, a.recall)
		} else {
			t.Logf("判据的效果：C 精确率 %d/%d vs A %d/%d，召回未塌（%d vs %d）——"+
				"**方向**符合预期，但精确率的配对 p=%s 未达 0.05：**不要**据此搬进生产，"+
				"需要更多 rep（本装置 MDE ≈ %.0f pp）。",
				c.prec, reps, a.prec, reps, c.recall, a.recall,
				formatP(cP, cPOK), mde)
		}
	case c.recall < a.recall-2:
		t.Logf("判据把召回压下去了（C 记对 %d/%d vs A %d/%d）：**不要**加——它滤掉了本该记的。",
			c.recall, reps, a.recall, reps)
	default:
		t.Logf("判据未显示可测到的精确率改善（C %d/%d vs A %d/%d，p=%.3f）：**不要**加进生产注入。",
			c.prec, reps, a.prec, reps, cP)
	}
}

// TestP0ArmsDifferOnlyByInjection 是装置自身的自检。
//
// 9-29 那次实验作废的一半原因是"两臂到底差什么"没被证明过。这里把装置钉死：
// A 臂的注入必须等于生产路径现算的结果，且必须含目录路径。
func TestP0ArmsDifferOnlyByInjection(t *testing.T) {
	home := t.TempDir()
	ws := p0Fixture(t)
	h := newMemoryHost(memory.Config{Directory: filepath.Join(home, "mem")})
	inj := h.injection(context.Background(), ws)
	if inj == "" {
		t.Fatal("A 臂注入为空")
	}
	if !strings.Contains(inj, h.dir.Path) {
		t.Fatalf("A 臂注入必须含记忆目录路径：\n%s", inj)
	}
	// B 臂是空串——两臂之间**只有**这个差别，没有第二个变量。
	if b := newMemoryHost(memory.Config{Enabled: memory.Bool(false)}).injection(context.Background(), ws); b != "" {
		t.Fatalf("B 臂应完全不注入，得到：%q", b)
	}
	// 说明段不得含行为判据（P1 尚未做实验，见 memory/guidance_test.go 的守卫）。
	for _, bad := range []string{"请记录", "务必", "应该写", "不要每次", "CLAUDE.md"} {
		if strings.Contains(inj, bad) {
			t.Fatalf("注入混入行为判据 %q，P1 未做实验前不得出现：\n%s", bad, inj)
		}
	}
	// 提问措辞本身也不得暗示记忆（否则"模型写不写"测的是被暗示的程度，不是注入）。
	for _, bad := range []string{"记忆", "memory", "MEMORY"} {
		for i, turn := range p0Turns {
			if strings.Contains(strings.ToLower(turn), strings.ToLower(bad)) {
				t.Fatalf("第 %d 轮措辞暗示了记忆（%q），会污染变量：%q", i+1, bad, turn)
			}
		}
	}
	// 两轮必须都**不含**可记事实之外的提示：第二轮是干扰项，它只复述仓库内容。
	if len(p0Turns) < 2 {
		t.Fatalf("装置需要至少两轮提问（用户口述 + 干扰），实际 %d 轮", len(p0Turns))
	}
	// 判定词必须与仓库内容**不相交**：否则"该不该记"的分界线本身是错的。
	repoFact := "8443 backlog 128 maxBackoff 重连退避"
	for _, tok := range p0RememberTokens {
		if strings.Contains(repoFact, tok) {
			t.Fatalf("判定词 %q 同时出现在仓库可推导内容里，分类会自相矛盾", tok)
		}
	}
}

// p0LogRun 逐 rep 立刻打一行。
//
// 为什么必须**即时**：跑批全程阻塞在网络 I/O 上（单次 rep ~15s，24 次 ≈ 6 分钟），
// 若把日志攒到循环结束才打，整个过程就是 6 分钟静默——看起来与卡死无法区分。
// 2026-09-30 第一次跑就是这样被误判成"go test 有问题"而手动停掉的。
func p0LogRun(t *testing.T, r p0Run) {
	t.Helper()
	t.Logf("pair=%d arm=%s topic=%v fmtr=%v type=%v modified=%v index=%v "+
		"| 记下用户事实=%v 记了代码内容=%v | 写尝试=%d 被拒=%d 用时=%.0fs err=%q",
		r.PairIndex, r.Arm, r.WroteTopic, r.HasFrontmatter,
		r.HasType, r.StampedModified, r.IndexUpdated,
		r.Remembered, r.RecordedDerivable,
		r.WriteAttempts, len(r.WriteRejected), r.Duration, r.Err)
	for _, rej := range r.WriteRejected {
		t.Logf("  pair=%d arm=%s 写被拒: %s", r.PairIndex, r.Arm, truncForLog(rej))
	}
}

func truncForLog(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// p0CriteriaOfficial 是官方 §2.2 的行为判据，**逐句照抄**、只做中英直译所需的
// 最小改写（官方原文见下），没有加任何我们自己的话：
//
//	"Claude **skips anything it can derive from the codebase**, such as
//	 architecture, file paths, or debugging fixes. **It also skips anything your
//	 CLAUDE.md files already say.**"
//	"**Claude doesn't save something every session.** It decides what's worth
//	 remembering based on whether the information would be useful in a future
//	 conversation."
//
// # 它为什么在**测试文件**里而不在 memory 包里
//
// 这是**待验证的候选**，不是已发布的行为。放进生产包就等于把结论提前写进代码——
// 而本轮的全部教训恰恰是"我们判断该有的东西"与"官方有的东西"要分开。装置测出
// 结论之后再决定放不放进 guidance.go，届时把判据和它的实测数据一起提交。
//
// # 与上一版那 6 条协议的区别
//
// 上一版是**自创**的 6 条，内部互相矛盾（既说"不要每次都写"又说"持续更新索引"），
// 采纳率 12%~62%。这一次有原文可依，且只有三条、不含祈使句、不与形态说明冲突。
// 但"照抄"不构成"该加"的证据——所以它是 C 臂，要数据。
const p0CriteriaOfficial = "关于何时记录记忆：" +
	"跳过任何能从代码库推导出来的内容，例如架构、文件路径或调试修复；" +
	"也跳过项目指令文件（CLAUDE.md / AGENTS.md）里已经写明的内容。" +
	"并不是每个会话都要写入记忆——由你判断某条信息在未来对话里是否还用得上。"

// TestP0CriteriaClassificationIsSound 自检那套**确定性判定**本身对不对。
//
// 这条比跑批更重要：分类器错了，后面所有 A/B/C 读数都只是**把错误量化**。
// 而分类器最容易错的地方是 token 选取 —— 挑一个仓库里也有的词当"该记的"，
// 就会把"从代码里抄的"误判成"从对话里听的"，方向整个反过来。
func TestP0CriteriaClassificationIsSound(t *testing.T) {
	// ① 仓库可推导内容 → 判为 junk（不该记的记了）
	junk := p0Run{WroteTopic: true}
	for _, tok := range []string{"backlog=128", "listen=0.0.0.0:8443", "const maxBackoff = 5"} {
		junk.Body += tok
	}
	// ② 用户口述事实 → 判为 recall（该记的记了）
	good := p0Run{WroteTopic: true, Body: "发布窗口：周四 15:00 后不要合并主干；排查先看 relay-metrics 面板"}
	// ③ 两者混在一起 → 既是 recall 也是 junk（"什么都记"）
	mixed := p0Run{WroteTopic: true, Body: "发布窗口：周四 15:00 后不要合并主干。backlog=128"}
	// ④ 什么都没写 → 既不是 recall 也不是 prec（不写不是"写对"）
	none := p0Run{}

	cases := []struct {
		name                           string
		run                            p0Run
		wantRecall, wantJunk, wantPrec bool
	}{
		{"只记了代码内容", junk, false, true, false},
		{"只记了用户口述", good, true, false, true},
		{"混着记", mixed, true, true, false},
		{"什么都没写", none, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.run
			// 复用 p0Inspect 的同一段判定逻辑：把它抽出来，两处共用，
			// 免得"分类器"和"跑批用的分类"悄悄漂移。
			classifyRun(&got, got.Body)
			if got.Remembered != tc.wantRecall {
				t.Fatalf("Remembered = %v, want %v", got.Remembered, tc.wantRecall)
			}
			if got.RecordedDerivable != tc.wantJunk {
				t.Fatalf("RecordedDerivable = %v, want %v", got.RecordedDerivable, tc.wantJunk)
			}
			if got.precision() != tc.wantPrec {
				t.Fatalf("precision = %v, want %v", got.precision(), tc.wantPrec)
			}
		})
	}
}

// formatP 把 McNemar 的 p 值渲染成日志文本，区分"真的 p=0.000"与"算不出来"。
func formatP(p float64, ok bool) string {
	if !ok {
		return "不可报（无不一致对）"
	}
	return strconv.FormatFloat(p, 'g', 3, 64)
}
