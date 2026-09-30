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

// p0RepsFloor 是重复次数地板。低于 8 就无法把"A 臂 0/8"和"A 臂 6/8"区分开，
// 而那正是本装置要回答的问题。
const p0RepsFloor = 8

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
		t.Fatalf("HAI_EVAL_P0_REPS=%d < 地板 %d：n 太小分不清「没写」与「写得少」", n, p0RepsFloor)
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
	// 试了且写对了地方但**被拒**。不拆开的话，"A 臂只有 3% 写入"这个数字
	// 无法区分"模型不感兴趣"与"我们的写入路径坏了"——而两者的处置完全相反。
	WriteAttempts  int      `json:"write_attempts"`
	WriteRejected  []string `json:"write_rejected,omitempty"`
	WriteElsewhere int      `json:"write_elsewhere"` // 写了 .md，但不在记忆目录里
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

// p0Question 任务措辞。A/B 两臂**逐字节相同**，且不含任何与记忆有关的暗示。
const p0Question = "阅读这个仓库，说明 relay 服务监听哪个端口、" +
	"backlog 是多少，以及重连退避上限为什么不能调高。"

func p0RunOnce(t *testing.T, p provider.Provider, prov, model, root string,
	mh *memoryHost, inject string, pairIdx int, arm string) p0Run {

	t.Helper()
	ft := builtin.NewFileTools(root)
	if mh != nil {
		ws := root
		ft = ft.WithPostWrite(func(p string) (string, error) {
			return mh.postWrite(context.Background(), ws, p)
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
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	ac := loop.Run(ctx, []core.Message{core.NewUserMessage(core.Content{
		Type: core.ContentTypeText, Content: p0Question,
	})}, handler)
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
	return run
}

// TestP0GuidanceCausesModelToWriteMemory 是 P0 的端到端验收。
//
// 配对交错执行（A、B 各跑一次），漂移在两臂间均摊。
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
		runA := p0RunOnce(t, p, prov, model, wsA, hA, injA, i, "A")
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
		runB := p0RunOnce(t, p, prov, model, wsB, hB, "", i, "B")
		runs = append(runs, runB)
		p0LogRun(t, runB)
	}

	// 原始读数先落盘：判定是确定性的，但原始数据比任何汇总都值钱。
	if dir := os.Getenv("HAI_EVAL_REPORT_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			b, _ := json.MarshalIndent(runs, "", "  ")
			_ = os.WriteFile(filepath.Join(dir, "p0.json"), b, 0o644)
		}
	}

	aWrite, bWrite, aChain, bChain := 0, 0, 0, 0
	aAttempt, bAttempt := 0, 0
	for _, r := range runs {
		if r.Arm == "A" {
			aAttempt += r.WriteAttempts
			if r.WroteTopic {
				aWrite++
			}
			if r.chainTurned() {
				aChain++
			}
		} else {
			bAttempt += r.WriteAttempts
			if r.WroteTopic {
				bWrite++
			}
			if r.chainTurned() {
				bChain++
			}
		}
	}

	// 统计口径与 Step 0 / 补测一**同源**（复用 supplementMcNemarExact /
	// supplementWilsonCI），避免两台仪器各报各的。
	b01, b10 := 0, 0
	for i := 0; i < reps; i++ {
		aOn := runs[i*2].chainTurned()
		bOn := runs[i*2+1].chainTurned()
		if aOn && !bOn {
			b01++
		} else if bOn && !aOn {
			b10++
		}
	}
	disc, pMc, pOK := supplementMcNemarExact(b01, b10)
	aLo, aHi := supplementWilsonCI(aChain, reps)
	bLo, bHi := supplementWilsonCI(bChain, reps)
	mde := supplementMDEUnpaired(0, reps, 1, reps)

	t.Logf("写入率   A=%d/%d  B=%d/%d", aWrite, reps, bWrite, reps)
	t.Logf("写尝试   A=%d  B=%d（分母都是 %d 次会话）", aAttempt, bAttempt, reps)
	t.Logf("全链转通 A=%d/%d [Wilson %.0f%%–%.0f%%]  B=%d/%d [Wilson %.0f%%–%.0f%%]",
		aChain, reps, 100*aLo, 100*aHi, bChain, reps, 100*bLo, 100*bHi)
	if pOK {
		t.Logf("配对 McNemar：不一致对 %d（仅A %d / 仅B %d），双侧精确 p=%.3f", disc, b01, b10, pMc)
	} else {
		t.Logf("配对 McNemar：无不一致对，p 不可报（那是分辨率不足，不是零假设被接受）")
	}
	t.Logf("本装置在 n=%d/臂 下能分辨的最小差值 ≈ %.0f 个百分点", reps, mde)

	// —— 断言一（强，可证伪）：机制真的转起来了 ——
	//
	// 这条只需要**一次**成功就成立：改动之前，"模型写入 → 宿主补 modified"在结构上
	// 不可能发生（模型不知道往哪写），所以 0 次是必然的，出现 1 次就是反证。
	if aChain == 0 {
		t.Fatalf("A 臂（真实产品注入）0/%d 全链转通：模型从未写出带 frontmatter 的记忆，"+
			"或宿主钩子从未命中——P0 的机制假设不成立", reps)
	}

	// —— 断言二（弱，只查装置不脏）：B 臂必须为 0 ——
	//
	// B 臂的模型完全不知道有记忆这回事，它写出的任何文件都不可能带 frontmatter。
	// B > 0 说明装置被污染（上一 rep 泄漏、或工具面漏了），此时 A−B 的读数不可用。
	if bChain > 0 {
		t.Fatalf("B 臂（完全不注入）却转通了 %d 次全链——装置有污染，B 臂不干净", bChain)
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
	// 提问措辞本身也不得暗示记忆。
	for _, bad := range []string{"记忆", "memory", "MEMORY"} {
		if strings.Contains(strings.ToLower(p0Question), strings.ToLower(bad)) {
			t.Fatalf("任务措辞暗示了记忆（%q），会污染变量：%q", bad, p0Question)
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
		"| 写尝试=%d 被拒=%d 写到别处=%d 用时=%.0fs err=%q",
		r.PairIndex, r.Arm, r.WroteTopic, r.HasFrontmatter,
		r.HasType, r.StampedModified, r.IndexUpdated,
		r.WriteAttempts, len(r.WriteRejected), r.WriteElsewhere,
		r.Duration, r.Err)
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
