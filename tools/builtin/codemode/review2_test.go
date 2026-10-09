package codemode

// review2_test.go：第二轮只读对抗复核（§7.18）发现的**回归钉子**。
//
// 每条对应一个已在真 node + 真引擎下复现过的缺陷，且都能被「改坏→红」验证：
//
//	F1 image() 不得替脚本读 ~/.go-code（沙箱与文件工具都拒绝该目录）
//	F2 image() 不得读非普通文件（FIFO 的 st.Size() 恒 0 ⇒ 绕过体积闸门 + 卡住宿主）
//	F3 写类调用必须按**脚本发起顺序**执行（ExecScope.Acquire 只保证互斥）
//	F4 墙钟杀掉脚本后，排在门后的写类调用不得执行（否则是无转录的真实副作用）
//	F5 max_output_tokens=0 是「只回结局声明」，不是「不限」（SkipTruncate 让 0 变 fail-open）
//
// FIFO 那条（F2）在 review2_fifo_test.go（需要 unix 的 Mkfifo）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/execproc"
	"github.com/seven7628/hai-harness/tools"
)

// headLines 取前 n 行（断言失败时打日志用；避免把整段结果灌进测试输出）。
func headLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// ---- F1：宿主读盘的路径策略 ----

func TestImageRefusesHarnessConfigDir(t *testing.T) {
	requireNode(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cfgDir, "settings-screenshot.png")
	writeTinyPNG(t, target) // 真图片：内容本身合法，被拒只能是因为路径策略

	h := newHarness(t)
	h.installCodemode(nil)
	res, _ := h.run(context.Background(), fmt.Sprintf(
		"const r = await image(%q);\ntext('GOT ' + r.bytes);\nreturn 'done';", target))
	if !res.IsError && strings.Contains(res.Result, "GOT") {
		t.Fatalf("image() 读到了 ~/.go-code 里的文件（沙箱与文件工具都拒绝该目录）: %s", headLines(res.Result, 6))
	}
	if !strings.Contains(res.Result, "harness config directory") {
		t.Fatalf("拒绝原因要能让人看懂（应点名 harness config directory）: %s", headLines(res.Result, 6))
	}
	// 拒绝文案不许泄漏「文件多大」（F1 的第二半：尺寸 oracle）。
	if strings.Contains(res.Result, "bytes") {
		t.Errorf("拒绝文案里不该带尺寸信息（会变成 oracle）: %s", headLines(res.Result, 6))
	}
	// 受管放行子树仍然可读（plugins/runtime 只含公开内容，沙箱内也要能读）。
	open := filepath.Join(cfgDir, "plugins", "pic.png")
	if err := os.MkdirAll(filepath.Dir(open), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTinyPNG(t, open)
	res2, _ := h.run(context.Background(), fmt.Sprintf(
		"await image(%q);\nreturn 'ok';", open))
	if res2.IsError {
		t.Fatalf("受管放行子树（plugins）里的图片应当可读: %s", headLines(res2.Result, 6))
	}
}

// ---- F3：写类调用的脚本次序 ----

// orderedWriteTool 写类替身：记录 enter/exit 次序（CanParallel=false ⇒ 过串行门）。
type orderedWriteTool struct {
	tools.BaseTool
	mu   *sync.Mutex
	log  *[]string
	hold time.Duration
}

func (t *orderedWriteTool) ValidParams(context.Context, string, string) error { return nil }

func (t *orderedWriteTool) Call(_ context.Context, _, args string) (string, error) {
	var a struct {
		X int `json:"x"`
	}
	_ = json.Unmarshal([]byte(args), &a)
	t.mu.Lock()
	*t.log = append(*t.log, fmt.Sprintf("enter x=%d", a.X))
	t.mu.Unlock()
	time.Sleep(t.hold)
	t.mu.Lock()
	*t.log = append(*t.log, fmt.Sprintf("exit x=%d", a.X))
	t.mu.Unlock()
	return "ok", nil
}

func TestWriteClassCallsRunInScriptOrder(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	var mu sync.Mutex
	var log []string
	ow := &orderedWriteTool{mu: &mu, log: &log, hold: 30 * time.Millisecond}
	ow.BaseTool = tools.BaseTool{
		Name_:        "ordered_write",
		Description_: "ordered write stub",
		Params_:      tools.Obj(map[string]any{"x": tools.Int("x")}),
		CanParallel_: false,
		ReadOnly_:    false,
	}
	h.add(ow)
	h.installCodemode(nil)

	// 两条**先发起**（不 await）的写类调用：帧各有一个 goroutine，谁先抢到门由调度决定。
	// 修前实测 20 次里 13 次反序 —— 这正是「先 write_file 后 edit_file 变成先改后写」。
	script := "const a = tools.ordered_write({x: 1});\n" +
		"const b = tools.ordered_write({x: 2});\n" +
		"await Promise.allSettled([a, b]);\nreturn 'done';"
	const runs = 20
	for i := 0; i < runs; i++ {
		mu.Lock()
		log = nil
		mu.Unlock()
		res, _ := h.run(context.Background(), script)
		if res.IsError {
			t.Fatalf("run %d failed: %s", i, headLines(res.Result, 6))
		}
		mu.Lock()
		got := strings.Join(log, " | ")
		mu.Unlock()
		if want := "enter x=1 | exit x=1 | enter x=2 | exit x=2"; got != want {
			t.Fatalf("第 %d 次执行的次序 = %q, want %q（脚本先发起必须先执行）", i, got, want)
		}
	}
}

// ---- F4：被杀之后，排在门后的写类调用不得执行 ----

type blockWriteTool struct {
	tools.BaseTool
	name    string
	release chan struct{}
	ran     atomic.Int64
}

func newBlockWriteTool(name string, release chan struct{}) *blockWriteTool {
	t := &blockWriteTool{name: name, release: release}
	t.BaseTool = tools.BaseTool{
		Name_:        name,
		Description_: "write stub " + name,
		Params_:      tools.Obj(map[string]any{"x": tools.Str("ignored")}),
		CanParallel_: false,
		ReadOnly_:    false,
	}
	return t
}

func (t *blockWriteTool) ValidParams(context.Context, string, string) error { return nil }

// Call **刻意忽略 ctx**（与真实 write_file 同形态）—— 回答「被杀之后还会不会写」。
func (t *blockWriteTool) Call(_ context.Context, _, _ string) (string, error) {
	if t.release != nil {
		<-t.release
	}
	t.ran.Add(1)
	return "wrote " + t.name, nil
}

func TestQueuedWriteDoesNotRunAfterWallClockKill(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	release := make(chan struct{})
	slow := newBlockWriteTool("slow_write", release)
	fast := newBlockWriteTool("fast_write", nil)
	h.add(slow, fast)
	h.installCodemode(nil)

	// p1 立刻拿住串行门；p2 在 100ms 后才发帧（那时门已被 p1 持有）⇒ p2 必然在门外等待。
	script := "// @options: {\"timeout_ms\": 500}\n" +
		"const p1 = tools.slow_write({});\n" +
		"setTimeout(() => { tools.fast_write({}); }, 100);\n" +
		"await p1.then(() => 'x', () => 'y');\n"
	res, _ := h.run(context.Background(), script)
	var st codemodeResult
	_ = json.Unmarshal(res.Structured, &st)
	if !strings.HasPrefix(res.Result, "[TIMEOUT") {
		t.Fatalf("这条用例的前提是墙钟杀掉脚本，实际结果: %s", headLines(res.Result, 4))
	}
	close(release) // 放掉占门的那条 → 排在后面的写类调用此刻才可能往下走
	time.Sleep(500 * time.Millisecond)
	if n := fast.ran.Load(); n > 0 {
		t.Errorf("墙钟杀脚本后，排队的写类调用仍执行了 %d 次（真实副作用 + 转录里看不见）", n)
	}
}

// ---- F5：max_output_tokens=0 的语义 ----

func TestZeroOutputBudgetKeepsOutcomeAndSpills(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	h.installCodemode(nil)

	// 200 KB 输出 + 声明 0 预算：修前 `budgetBytes<=0` 被当成「不限」，而本工具
	// SkipTruncate=true（引擎的 20 KB 兜底已豁免）⇒ 200 KB 直接进上下文且不 spill。
	res, _ := h.run(context.Background(),
		"// @options: {\"max_output_tokens\": 0}\nreturn \"w\".repeat(200000);")
	var st codemodeResult
	if err := json.Unmarshal(res.Structured, &st); err != nil {
		t.Fatalf("结构化结果解不出来: %v", err)
	}
	if len(res.Result) > 20000 {
		t.Errorf("声明 max_output_tokens=0 却回给模型 %d 字节（正文写明的取值域是 0-200000 上限）", len(res.Result))
	}
	// 结局面（首行）永不被截断：§6.5「结局声明必须在首行」在极小预算下同样成立。
	first := strings.SplitN(res.Result, "\n", 2)[0]
	if !strings.HasPrefix(first, "Script completed") && !strings.HasPrefix(first, "Script failed") {
		t.Errorf("首行不是结局声明（被预算截掉了）: %q", first)
	}
	if !st.Truncated {
		t.Error("声明 0 预算 + 200 KB 输出必须判为截断")
	}
	if st.FullOutputPath == "" {
		t.Error("被截断时必须落盘并回 full_output_path（否则正文既没给模型也没留档）")
	} else if _, err := os.Stat(st.FullOutputPath); err != nil {
		t.Errorf("full_output_path 指向的文件不存在: %v", err)
	}
}

// ---- 保序门的票号来源（纯逻辑，不需要 node）----

func TestScriptSeqPrefersDispatchIndex(t *testing.T) {
	cases := []struct {
		name string
		call execproc.ScriptCall
		want int64
	}{
		{"Index 优先（帧序）", execproc.ScriptCall{Index: 7, Id: "2"}, 7},
		{"Index 缺失 → 用 Id", execproc.ScriptCall{Id: " 12 "}, 12},
		{"Index 非法（负数）→ 用 Id", execproc.ScriptCall{Index: -1, Id: "3"}, 3},
		{"两者都缺 → 0（不排队）", execproc.ScriptCall{}, 0},
		{"Id 非数字 → 0", execproc.ScriptCall{Id: "abc"}, 0},
		{"Id 超 int64 → 0（不 panic）", execproc.ScriptCall{Id: "9223372036854775808"}, 0},
		{"Id 为 0/-3 → 0", execproc.ScriptCall{Id: "0"}, 0},
	}
	for _, c := range cases {
		if got := scriptSeq(c.call); got != c.want {
			t.Errorf("%s: scriptSeq(%+v) = %d, want %d", c.name, c.call, got, c.want)
		}
	}
}

// 票号缺失（非传输层来源的调用）时不得排队、也不得 panic —— 行为退回到达序。
func TestRunToolWithoutScriptIDStillRuns(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	ok := &orderedWriteTool{mu: &sync.Mutex{}, log: &[]string{}, hold: 0}
	ok.BaseTool = tools.BaseTool{
		Name_: "ordered_write", Description_: "d",
		Params_: tools.Obj(map[string]any{"x": tools.Int("x")}), CanParallel_: false,
	}
	h.add(ok)
	br := &bridge{ // 直接构造：模拟没有 ScriptCall.Id 的调用面
		tool: h.tool, engine: h.engine, ctx: context.Background(),
		clock: newWallClock(time.Second), store: h.store, pending: NewPending(),
		scope: tools.NewExecScope(nil, 0), cancels: map[int64]context.CancelFunc{},
		turns: newTurnGate(),
	}
	r := br.runTool(context.Background(), "ordered_write", json.RawMessage(`{"x":7}`), 0)
	if r.IsError {
		t.Fatalf("无票号的调用应当照常执行: %s", r.Value)
	}
}
