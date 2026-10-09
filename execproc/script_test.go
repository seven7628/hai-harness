package execproc

// script_test.go —— 双向传输层（宿主 ↔ 沙箱 NDJSON 桥）的端到端行为测试。
//
// 与 executor_test.go 同规矩：全部真跑子进程（不 mock node）—— 这一层的价值全在真实
// 进程行为里：stdin 双向通道、事件循环的 ref/unref 保活、Promise.all 的真并发、被杀
// 之后的残留、临时文件的权限与清理。mock 掉这些等于把最该钉住的东西全测没了。
//
// 只有一条测试用假 Sandbox（TestExecuteScriptSpecContract）：那条钉的是**宿主侧规格
// 构造**（三态时限 / 命令行 / stdin 让给桥接），不涉及任何进程行为，故不需要 node，
// 也不依赖本机装没装解释器。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/runtime"
	gosandbox "github.com/seven7628/hai-harness/sandbox"
)

// callRecorder 记录沙箱发来的调用（宿主侧唯一的「记录」载体）。
//
// 刻意不调用 t.* ：OnCall 跑在分派 goroutine 上，只做记录，断言全部回到测试
// goroutine 里做（唯一例外见 TestExecuteScriptTempFilesModeAndCleanup 的 0600 断言，
// 那里必须在飞行中观察，用的是 t.Errorf —— 它可以从别的 goroutine 调用，
// 只有 FailNow/Fatalf 不行）。
type callRecorder struct {
	mu    sync.Mutex
	calls []ScriptCall
}

func (r *callRecorder) add(c ScriptCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *callRecorder) all() []ScriptCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ScriptCall(nil), r.calls...)
}

func (r *callRecorder) names() []string {
	var out []string
	for _, c := range r.all() {
		out = append(out, c.Name)
	}
	return out
}

// newScriptExecutor 建一个 cwd 指向测试临时目录的执行器。
func newScriptExecutor(t *testing.T) *Executor {
	t.Helper()
	e := New(gosandbox.NoSandbox{}, nil)
	e.opt.Cwd = t.TempDir()
	return e
}

// initTool 造 init 载荷里的一个工具项（形状见 ScriptOpts.Init 的 godoc）。
func initTool(name string, rawName string) map[string]string {
	m := map[string]string{"name": name}
	if rawName != "" {
		m["rawName"] = rawName
	}
	return m
}

// mustJSON 把任意值编成 json.RawMessage（测试里的 init 载荷/回执值）。
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ---- ① 单次往返：结构化值 resolve + IsError reject ----

// TestExecuteScriptSingleCallRoundTrip 钉住一次 `await tools.x()` 的完整往返：
// 脚本侧发出的 name/args 原样到达宿主，宿主的回执按类型语义回到脚本
// （结构化值 ⇒ resolve 成对象；IsError ⇒ reject 且 message 可读）。
func TestExecuteScriptSingleCallRoundTrip(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)
	rec := &callRecorder{}

	script := `
const v = await tools.alpha({ q: "hello world", n: 7 });
console.log("type:", typeof v, "ok:", v.ok, "n:", v.n);
try {
  await tools.boom({ path: "/etc/shadow" });
  console.log("BUG: an IsError result resolved instead of rejecting");
} catch (err) {
  console.log("caught:", err.message);
}
`
	start := time.Now()
	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script:  script,
		Timeout: 15 * time.Second,
		Init: mustJSON(t, map[string]any{
			"tools": []map[string]string{initTool("alpha", ""), initTool("boom", "")},
		}),
		OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
			rec.add(c)
			if c.Name == "boom" {
				return ScriptResult{Value: jsonString("permission denied: /etc/shadow is not readable"), IsError: true}
			}
			return ScriptResult{Value: json.RawMessage(`{"ok":true,"n":42}`)}
		},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ExecuteScript: %v (raw=%q)", err, res.Raw)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (text=%q raw=%q)", res.ExitCode, res.Text, res.Raw)
	}
	for _, want := range []string{
		"type: object ok: true n: 42",
		"caught: permission denied: /etc/shadow is not readable",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("Text 缺少 %q\nText=%q\nRaw=%q", want, res.Text, res.Raw)
		}
	}
	if strings.Contains(res.Text, "BUG:") {
		t.Errorf("IsError 的回执没有 reject:\n%s", res.Text)
	}

	// 宿主侧拿到的调用必须与脚本发出的逐字段一致（Id 由沙箱生成、非空）。
	calls := rec.all()
	if len(calls) != 2 {
		t.Fatalf("宿主收到 %d 次调用，want 2: %+v", len(calls), calls)
	}
	if calls[0].Name != "alpha" || calls[1].Name != "boom" {
		t.Errorf("调用顺序/名字不对: %+v", calls)
	}
	var args struct {
		Q string `json:"q"`
		N int    `json:"n"`
	}
	if err := json.Unmarshal(calls[0].Args, &args); err != nil {
		t.Fatalf("alpha 的 Args 不是对象原文: %v (%s)", err, calls[0].Args)
	}
	if args.Q != "hello world" || args.N != 7 {
		t.Errorf("alpha 的 Args = %+v, want {hello world 7}", args)
	}
	if calls[0].Id == "" || calls[1].Id == "" || calls[0].Id == calls[1].Id {
		t.Errorf("Id 必须非空且互不相同: %+v", calls)
	}

	// 协议帧不得泄进给模型的文本（call 帧里有参数原文，泄了就是双份上下文）。
	if strings.Contains(res.Text, `"notify"`) {
		t.Errorf("裸协议 JSON 泄进了模型上下文:\n%s", res.Text)
	}

	// 终帧到达后必须立刻关掉宿主→沙箱管道。不关的话 os/exec 的 Wait 要白等
	// waitDelayAfterKill(3s) 再强制关管，并走 ErrWaitDelay 分支在文本里印一句
	// 「输出可能被进程组外的后代截断」—— 本用例没有后代，出现那句话就是没关。
	if strings.Contains(res.Text, "截断") {
		t.Errorf("终帧后没关 stdin（Wait 白等 WaitDelay 并印了截断注记）:\n%s", res.Text)
	}
	if elapsed > 5*time.Second {
		t.Errorf("一次往返花了 %v（正常应在 1s 量级；异常慢通常意味着 Wait 等了 WaitDelay）", elapsed)
	}
}

// ---- ② 并发：真并发 + 乱序回执按 id 关联 ----

// TestExecuteScriptConcurrentCallsAreTrulyConcurrent 钉住「脚本内 Promise.all 是真并发」。
//
// 两条断言，时间只用来兜底：
//
//  1. **并发峰值必须等于调用数**（与机器快慢无关的硬断言）：宿主侧同时在飞的 OnCall
//     数量。串行分派（变异之一）会把它压成 1。
//  2. **耗时显著小于串行基线**：每次调用的等待刻意取 (5-i)*200ms（i 是调用序号），
//     即 1000/800/600/400/200ms —— 串行基线 = 3000ms（sleep 依次相加），并发下限 =
//     max(sleep) + 冷启动 ≈ 1000 + 100~300ms。阈值取 2000ms：在两者之间，且给调度留了
//     ~700ms 余量（本机实测并发路径 ≈1.1s）。
//
// 等待取「随序号递减」还有一个作用：回执必然**乱序**到达（先回后面的调用），
// 于是脚本里 `results[i].echo === i` 同时钉住了「回执按 id 关联」这件事
// （把回执的 id 写错，这次调用就永远等不到回执 → 只能超时失败）。
func TestExecuteScriptConcurrentCallsAreTrulyConcurrent(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)
	const n = 5
	const perCallStep = 200 * time.Millisecond

	// 回执值在测试 goroutine 里先编好：OnCall 跑在分派 goroutine 上，不该碰 t
	//（t.Fatalf 只能在测试 goroutine 调，mustJSON 里的 t.Fatalf 同理）。
	echoValue := func(i int) json.RawMessage {
		b, _ := json.Marshal(map[string]int{"echo": i, "waited_ms": (n - i) * 200})
		return b
	}

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	entered := make(chan struct{}, n)

	script := `
const results = await Promise.all([0, 1, 2, 3, 4].map((i) => tools.slow({ i })));
const mismatched = results.filter((r, i) => r.echo !== i).length;
console.log("mismatches:", mismatched);
console.log("values:", results.map((r) => r.echo).join(","));
`
	start := time.Now()
	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script:  script,
		Timeout: 20 * time.Second,
		Init: mustJSON(t, map[string]any{
			"tools": []map[string]string{initTool("slow", "")},
		}),
		OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
			var a struct {
				I int `json:"i"`
			}
			_ = json.Unmarshal(c.Args, &a)
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			entered <- struct{}{}

			// 序号越小等得越久 ⇒ 回执乱序（后面的先回）。
			time.Sleep(time.Duration(n-a.I) * perCallStep)

			mu.Lock()
			inFlight--
			mu.Unlock()
			return ScriptResult{Value: echoValue(a.I)}
		},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ExecuteScript: %v (raw=%q)", err, res.Raw)
	}
	if res.TimedOut {
		t.Fatalf("脚本超时：回执没能按 id 关联上（text=%q）", res.Text)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (text=%q)", res.ExitCode, res.Text)
	}

	mu.Lock()
	peak := maxInFlight
	mu.Unlock()
	if peak != n {
		t.Errorf("宿主侧并发峰值 = %d, want %d（串行分派会让它变成 1）", peak, n)
	}
	if len(entered) != n {
		t.Errorf("宿主收到 %d 次调用, want %d", len(entered), n)
	}

	if !strings.Contains(res.Text, "mismatches: 0") {
		t.Errorf("回执与调用的 id 对不上（乱序回执必须按 id 关联）:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "values: 0,1,2,3,4") {
		t.Errorf("返回值与调用序号错位:\n%s", res.Text)
	}

	// 串行基线 = 1000+800+600+400+200 = 3000ms；并发下限 ≈ max(sleep)=1000ms。
	const threshold = 2 * time.Second
	if elapsed > threshold {
		t.Errorf("5 个并发调用花了 %v, want <= %v（串行基线 3s；超过阈值说明没真并发）", elapsed, threshold)
	}
	t.Logf("并发 5 call 实测耗时 %v（串行基线 3s，阈值 %v）", elapsed, threshold)
}

// ---- ③ 脚本抛错：已发生的调用记录仍在 ----

// TestExecuteScriptThrowAfterCallKeepsEarlierRecords 钉住「副作用不可回滚」这条事实：
// 脚本在调用成功之后抛错，宿主侧那次的记录必须还在（模型要靠它知道「什么已经做过了」），
// 且抛错**不是** error（并入 Text，与 Execute 同契约）。
func TestExecuteScriptThrowAfterCallKeepsEarlierRecords(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)
	rec := &callRecorder{}

	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script: `
const r = await tools.write_thing({ path: "out.txt" });
console.log("call-returned:", r.wrote);
throw new Error("boom-after-call");
`,
		Timeout: 15 * time.Second,
		Init: mustJSON(t, map[string]any{
			"tools": []map[string]string{initTool("write_thing", "")},
		}),
		OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
			rec.add(c)
			return ScriptResult{Value: json.RawMessage(`{"wrote":true}`)}
		},
	})
	if err != nil {
		t.Fatalf("脚本抛错不该是 error（应并入 Text）: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want 非零（抛错必须反映在结局里）: text=%q", res.Text)
	}
	for _, want := range []string{"call-returned: true", "boom-after-call", "[script error]"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("Text 缺少 %q:\n%s", want, res.Text)
		}
	}
	if got := rec.names(); len(got) != 1 || got[0] != "write_thing" {
		t.Errorf("抛错前的调用记录丢了: %v（want [write_thing]）", got)
	}
}

// ---- ④ 死循环被超时杀掉：无残留 + 有界返回 + 措辞 ----

// TestExecuteScriptTimeoutKillsNoResidueAndReturnsInBudget 硬性验收：
//
//	① `ps` 无残留（含脚本 fork 的孙进程）；② 在 timeout + 6s 内返回；
//	③ 文案沿用 §6.5 修好的口径：被杀脚本的输出**一条都拿不回来**，故不得承诺
//	   「the output below is PARTIAL」。
func TestExecuteScriptTimeoutKillsNoResidueAndReturnsInBudget(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)
	const budget = 2 * time.Second

	// 孙进程承接「只杀直接子进程会残留」这条断言（sandbox 杀整组的理由）；用 sleep
	// 而不是忙等：go test 各包并行跑，忙等会烧掉一整个核并顶坏 sandbox 包的计时敏感
	// 用例（executor_test.go 里有同款说明）。脚本在死循环前**先做一次工具调用**：
	// 顺带证明泵在超时前是通的。
	pidFile := filepath.Join(t.TempDir(), "pids")
	script := `
// Phase 1-A2：脚本是 AsyncFunction 的**函数体**（顶层 return/await 合法，静态 import
// 不可用），故这里用 await import() —— 语义相同，是「需要模块的脚本」的受支持写法。
const { writeFileSync } = await import('node:fs');
const { spawn } = await import('node:child_process');
const kid = spawn('/bin/sh', ['-c', 'while true; do sleep 3600; done']);
writeFileSync(` + strconvQuote(pidFile) + `, process.pid + ' ' + kid.pid);
const r = await tools.ping({});
console.log("called-before-the-loop:", r.pong);
while (true) { await new Promise((res) => setTimeout(res, 50)); }
`
	start := time.Now()
	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script:  script,
		Timeout: budget,
		Init: mustJSON(t, map[string]any{
			"tools": []map[string]string{initTool("ping", "")},
		}),
		OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
			return ScriptResult{Value: json.RawMessage(`{"pong":true}`)}
		},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}

	// ② 有界返回：timeout + killWaitBudget(4s) + killEscalationWait(2s)。
	const limit = budget + 6*time.Second
	if elapsed > limit {
		t.Fatalf("返回耗时 %v, want <= %v (text=%q)", elapsed, limit, res.Text)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true (text=%q raw=%q)", res.Text, res.Raw)
	}
	if res.Timeout != budget {
		t.Errorf("RunResult.Timeout = %v, want %v（文案要用调用方给的那条时限）", res.Timeout, budget)
	}

	// ③ 措辞：首行必须声明结局，且**不得**承诺拿不回来的部分输出。
	if !strings.HasPrefix(res.Text, "[TIMEOUT after") {
		t.Errorf("Text 首行未声明 TIMEOUT:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "nothing could be recovered") {
		t.Errorf("Text 未说明输出不可回收（模型会把空当成真结果）:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "the output below is PARTIAL") {
		t.Errorf("无输出时仍承诺「下面的是部分输出」:\n%s", res.Text)
	}
	if !strings.Contains(res.Timeout.String(), "2s") {
		t.Errorf("RunResult.Timeout = %v, want 2s", res.Timeout)
	}

	// ① 无残留。pid 读不到即判失败（不是跳过）：脚本进入死循环前就写了文件。
	pids := readPIDs(t, pidFile)
	if len(pids) == 0 {
		t.Fatalf("脚本没写出 pid 文件（%s），残留断言空转：\ntext=%q\nraw=%q", pidFile, res.Text, res.Raw)
	}
	waitNoLivePIDs(t, pids, 3*time.Second)
}

// ---- ⑤ 临时文件：0600 + 0700 目录 + 四类退出一律删除 ----

// TestExecuteScriptFilesModeAndCleanup 硬性验收：帧 spool 落在 os.MkdirTemp(0700)
// 的下层、权限 0600，且**四类退出路径**（正常/超时/取消/error）都会把目录删掉。
//
// 0600 只能在运行期间观察（跑完就删了），故用 onRunFiles 这条测试缝拿到路径、
// 在 OnCall 里断言（此刻脚本正阻塞在这次调用上，进程确实活着、文件确实存在）。
// 这条缝的存在理由与被观察者自证的区别写在 script.go 的 onRunFiles 注释里。
//
// Phase 1-A2 起临时目录里**只有 spool**（脚本正文走 init.script，不落盘）：飞行中的
// 目录清单也一并断言 —— 多出任何文件都意味着某条路径又把资材落盘了。
// 「文件内容里没有脚本正文」由 TestExecuteScriptSourceNeverHitsDisk 钉住。
func TestExecuteScriptFilesModeAndCleanup(t *testing.T) {
	requireNode(t)

	var mu sync.Mutex
	var seen runFiles
	oldHook := onRunFiles
	onRunFiles = func(f runFiles) {
		mu.Lock()
		defer mu.Unlock()
		seen = f
	}
	defer func() { onRunFiles = oldHook }()
	takeSeen := func() runFiles {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}

	// assertDuringRun 在飞行中断言权限（只用于会真正发起调用的那条路径）。
	assertDuringRun := func(t *testing.T) {
		t.Helper()
		f := takeSeen()
		if f.dir == "" {
			t.Errorf("onRunFiles 没拿到路径（测试缝失效，0600 断言会空转）")
			return
		}
		dirInfo, err := os.Stat(f.dir)
		if err != nil {
			t.Errorf("运行期间临时目录不存在: %v", err)
			return
		}
		if perm := dirInfo.Mode().Perm(); perm != 0o700 {
			t.Errorf("临时目录权限 = %o, want 700（里面是脚本正文与工具回执）", perm)
		}
		info, err := os.Stat(f.spool)
		if err != nil {
			t.Errorf("运行期间帧 spool 不存在: %v", err)
		} else if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("运行期间帧 spool 权限 = %o, want 600 (%s)", perm, f.spool)
		}
		entries, err := os.ReadDir(f.dir)
		if err != nil {
			t.Errorf("运行期间读临时目录失败: %v", err)
			return
		}
		var names []string
		for _, en := range entries {
			names = append(names, en.Name())
		}
		if len(names) != 1 || names[0] != "frames.ndjson" {
			t.Errorf("临时目录内容 = %v, want 只有 [frames.ndjson]（脚本正文走 init，不该落盘）", names)
		}
	}

	// assertGone 断言运行结束后三个路径都不存在（含目录本身）。
	assertGone := func(t *testing.T) {
		t.Helper()
		f := takeSeen()
		if f.dir == "" {
			t.Errorf("onRunFiles 没拿到路径，删除断言会空转")
			return
		}
		for _, p := range []string{f.dir, f.spool} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s 在运行结束后仍存在（err=%v）", p, err)
			}
		}
	}

	t.Run("正常退出", func(t *testing.T) {
		e := newScriptExecutor(t)
		res, err := e.ExecuteScript(context.Background(), ScriptOpts{
			Script:  `const r = await tools.ping({}); console.log("ok:", r.pong);`,
			Timeout: 15 * time.Second,
			Init:    mustJSON(t, map[string]any{"tools": []map[string]string{initTool("ping", "")}}),
			OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
				assertDuringRun(t) // 飞行中：进程活着，文件在
				return ScriptResult{Value: json.RawMessage(`{"pong":true}`)}
			},
		})
		if err != nil {
			t.Fatalf("ExecuteScript: %v", err)
		}
		if !strings.Contains(res.Text, "ok: true") {
			t.Errorf("脚本没跑完:\n%s", res.Text)
		}
		assertGone(t)
	})

	t.Run("超时被杀", func(t *testing.T) {
		e := newScriptExecutor(t)
		res, err := e.ExecuteScript(context.Background(), ScriptOpts{
			Script:  `while (true) { await new Promise((r) => setTimeout(r, 50)); }`,
			Timeout: 1500 * time.Millisecond,
			Init:    mustJSON(t, map[string]any{"tools": []map[string]string{initTool("ping", "")}}),
		})
		if err != nil {
			t.Fatalf("ExecuteScript: %v", err)
		}
		if !res.TimedOut {
			t.Errorf("TimedOut = false（本用例靠它走超时路径）: text=%q", res.Text)
		}
		assertGone(t)
	})

	t.Run("ctx 取消", func(t *testing.T) {
		e := newScriptExecutor(t)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(300 * time.Millisecond)
			cancel()
		}()
		res, err := e.ExecuteScript(ctx, ScriptOpts{
			Script: `while (true) { await new Promise((r) => setTimeout(r, 50)); }`,
			// 不限时：这条路径必须只由 ctx 取消结束（§7.1-2 的「宿主管墙钟」形态）
			Timeout: -1,
			Init:    mustJSON(t, map[string]any{"tools": []map[string]string{initTool("ping", "")}}),
		})
		if err != nil {
			t.Fatalf("ExecuteScript: %v", err)
		}
		if !res.Canceled {
			t.Errorf("Canceled = false, want true (timedout=%v text=%q)", res.TimedOut, res.Text)
		}
		if !strings.HasPrefix(res.Text, "[CANCELLED") {
			t.Errorf("Text 首行未声明 CANCELLED:\n%s", res.Text)
		}
		if !strings.Contains(res.Text, "nothing could be recovered") {
			t.Errorf("取消路径也要说明输出不可回收:\n%s", res.Text)
		}
		assertGone(t)
	})

	t.Run("握手不兼容", func(t *testing.T) {
		oldPrelude := preludeJS
		preludeJS = strings.Replace(oldPrelude, "const PROTO_MAJOR = 1;", "const PROTO_MAJOR = 99;", 1)
		defer func() { preludeJS = oldPrelude }()

		e := newScriptExecutor(t)
		res, err := e.ExecuteScript(context.Background(), ScriptOpts{
			Script:  `console.log("never runs")`,
			Timeout: 15 * time.Second,
			Init:    mustJSON(t, map[string]any{"tools": []map[string]string{initTool("ping", "")}}),
		})
		if err == nil {
			t.Fatalf("握手不兼容却没报 error（这是「根本没跑起来」那一类）: text=%q", res.Text)
		}
		if !strings.Contains(err.Error(), "protocol") {
			t.Errorf("错误文案不像是协议不兼容: %v", err)
		}
		assertGone(t)
	})
}

// ---- ⑥ init 载荷真的到了沙箱 ----

// TestExecuteScriptInitReachesSandbox 钉住 init 的**内容**（不只是「发过一条」）：
// 工具表按归一化名建函数、原始名双键可用、其余键原样可见。
func TestExecuteScriptInitReachesSandbox(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)
	rec := &callRecorder{}

	init := json.RawMessage(`{"tools":[{"name":"alpha","rawName":"alpha-raw"},{"name":"beta"}],` +
		`"marker":"INIT-MARKER-9f3"}`)

	script := `
console.log("names:", Object.keys(tools).sort().join(","));
console.log("marker:", __codemode.init.marker);
console.log("raw-type:", typeof tools["alpha-raw"], "in-table:", "alpha-raw" in tools);
const r = await tools["alpha-raw"]({ via: "raw-name" });
console.log("raw-call:", r.via);
`
	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script:  script,
		Timeout: 15 * time.Second,
		Init:    init,
		OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
			rec.add(c)
			var a struct {
				Via string `json:"via"`
			}
			_ = json.Unmarshal(c.Args, &a)
			return ScriptResult{Value: mustJSON(t, map[string]string{"via": a.Via})}
		},
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v (raw=%q)", err, res.Raw)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (text=%q)", res.ExitCode, res.Text)
	}
	for _, want := range []string{
		"names: alpha,alpha-raw,beta", // 归一化名 + 原始名双键都在（排序后）
		"marker: INIT-MARKER-9f3",     // init 原文（不只 tools）也到了
		"raw-type: function in-table: true",
		"raw-call: raw-name", // 经原始名调用，宿主侧看到的仍是归一化名
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("Text 缺少 %q\nText=%q", want, res.Text)
		}
	}
	if got := rec.names(); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("经原始名调用时宿主侧收到的名字 = %v, want [alpha]（归一化名，桥接不做二次改写）", got)
	}
}

// ---- ⑦ 未实现的工具名：脚本内可 catch ----

// TestExecuteScriptUnknownToolIsCatchable 钉住「工具表就是权威」：表里没有的名字必须在
// **沙箱内**给一个可 catch 的错误（而不是变成 undefined 让调用报
// "tools.x is not a function"，那会让模型去猜调用语法而不是去核对工具表）。
func TestExecuteScriptUnknownToolIsCatchable(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)
	rec := &callRecorder{}

	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script: `
try {
  await tools.ghost({});
  console.log("BUG: an unknown tool resolved");
} catch (err) {
  console.log("caught:", err.message.split(":")[0]);
}
console.log("in-table:", "ghost" in tools, "listed:", tools.ghost === undefined);
`,
		Timeout: 15 * time.Second,
		Init:    mustJSON(t, map[string]any{"tools": []map[string]string{initTool("alpha", "")}}),
		OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
			rec.add(c) // 不该被调到
			return ScriptResult{Value: json.RawMessage(`{}`)}
		},
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("脚本 catch 住了错误，ExitCode 应为 0（text=%q）", res.Text)
	}
	if !strings.Contains(res.Text, `caught: unknown tool "ghost"`) {
		t.Errorf("未知工具的错误不可读（模型无法据此核对工具表）:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "in-table: false") {
		t.Errorf("`in` 判定必须准确（特性探测只能靠它）:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "BUG:") {
		t.Errorf("未知工具居然 resolve 了:\n%s", res.Text)
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Errorf("未知工具不该发到宿主（工具表是权威）: %+v", calls)
	}
}

// ---- ⑧ 求值语义：顶层 return 的值回到宿主（Phase 1-A2，§7.8/§7.9）----

// TestExecuteScriptTopLevelReturnBecomesValue 钉住模型侧契约
// 「the value you return becomes this tool's result」（DESCRIPTION_INTRO）：
// 顶层 `return` 合法，且值经 result 帧的 value 字段回到 RunResult.Value。
//
// 子用例覆盖「有值 / 无值 / 字面量 null」三种边界：nil 与 `null` 必须区分得开
// （前者 = 没有返回值，后者 = 脚本明确返回了 null），否则上层没法照 §15.4 的表映射。
func TestExecuteScriptTopLevelReturnBecomesValue(t *testing.T) {
	requireNode(t)

	cases := []struct {
		name  string
		body  string
		want  string // 期望的 Value 原文（"" = 期望 nil）
		isNil bool
	}{
		{"数字", `return 42;`, `42`, false},
		{"字符串", `return "plain text";`, `"plain text"`, false},
		{"对象", `return { ok: true, n: 42 };`, `{"ok":true,"n":42}`, false},
		{"数组", `return [1, "two", null];`, `[1,"two",null]`, false},
		{"null", `return null;`, `null`, false},
		{"没有 return", `console.log("no return here");`, ``, true},
		{"return undefined", `return undefined;`, ``, true},
		{"await 之后再 return", `const r = await tools.ping({}); return r;`, `{"pong":true}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newScriptExecutor(t)
			rec := &callRecorder{}
			res, err := e.ExecuteScript(context.Background(), ScriptOpts{
				Script:  tc.body,
				Timeout: 15 * time.Second,
				Init:    mustJSON(t, map[string]any{"tools": []map[string]string{initTool("ping", "")}}),
				OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
					rec.add(c)
					return ScriptResult{Value: json.RawMessage(`{"pong":true}`)}
				},
			})
			if err != nil {
				t.Fatalf("ExecuteScript: %v (raw=%q)", err, res.Raw)
			}
			if res.ExitCode != 0 {
				t.Fatalf("ExitCode = %d, want 0（return 是正常收尾，不是错误路径）\nText=%q\nRaw=%q",
					res.ExitCode, res.Text, res.Raw)
			}
			if tc.isNil {
				if res.Value != nil {
					t.Errorf("Value = %s, want nil（没有返回值）", res.Value)
				}
				return
			}
			if string(res.Value) != tc.want {
				t.Errorf("Value = %s, want %s\nText=%q\nRaw=%q", res.Value, tc.want, res.Text, res.Raw)
			}
			// 值只走 value 通道：不该被宿主塞进给模型的文本（否则模型看两遍）。
			if strings.Contains(res.Text, tc.want) && !strings.Contains(tc.body, "console.log") {
				t.Errorf("Value 泄漏进了 Text（值应当只走 RunResult.Value）:\n%s", res.Text)
			}
		})
	}
}

// TestExecuteScriptNonJSONSerializableValueStillReturnsFrame 返回值不可 JSON 编码时
// （函数/BigInt/循环引用）必须**退化成可读文本**，而不是让 result 帧发不出去 ——
// 丢帧在宿主侧表现为「脚本没有收尾」，比一条「值没法编码」的文本难查得多。
func TestExecuteScriptNonJSONSerializableValueStillReturnsFrame(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)

	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script:  `return () => 1;`,
		Timeout: 15 * time.Second,
		Init:    mustJSON(t, map[string]any{"tools": []map[string]string{}}),
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	if res.TimedOut {
		t.Fatalf("进程没正常收尾（result 帧没发出来）: text=%q raw=%q", res.Text, res.Raw)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (text=%q)", res.ExitCode, res.Text)
	}
	// 函数在 JSON 里没有表示：按「无返回值」处理（不是失败）。
	if res.Value != nil {
		t.Errorf("Value = %s, want nil（函数没有 JSON 表示）", res.Value)
	}

	// 循环引用：stringify 会抛，必须退化成文本而不是丢帧。
	res, err = e.ExecuteScript(context.Background(), ScriptOpts{
		Script:  `const a = {}; a.self = a; return a;`,
		Timeout: 15 * time.Second,
		Init:    mustJSON(t, map[string]any{"tools": []map[string]string{}}),
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	if res.TimedOut {
		t.Fatalf("循环引用把 result 帧弄丢了: text=%q raw=%q", res.Text, res.Raw)
	}
	var v string
	if err := json.Unmarshal(res.Value, &v); err != nil {
		t.Fatalf("循环引用的返回值应退化成 JSON 字符串: %s (%v)", res.Value, err)
	}
	if !strings.Contains(v, "not JSON-serializable") {
		t.Errorf("退化文本没有说明原因: %q", v)
	}
}

// TestExecuteScriptOversizedReturnValueDegradesToNotice 返回值大到一行 JSON 装不下时
// （宿主 stdjson.MaxLine = 4 MiB 是**按行**读的），必须降级成一句可读文本。
//
// 为什么这条不能省：超限的那一行宿主解不开 → 既丢 value 也丢 out（整条 result 帧被
// 当成非帧文本），而且那行 JSON 会被当作 tail 灌进模型上下文 —— 一次「返回了大对象」
// 的笔误能顶掉整次执行的结局，且失败形态极难从现象反推。
func TestExecuteScriptOversizedReturnValueDegradesToNotice(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)

	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		// 2 MiB 的字符串：超过传输上限（1 MiB），也超过宿主单行解码上限（4 MiB）的余量
		Script:  `return "y".repeat(2 * 1024 * 1024);`,
		Timeout: 15 * time.Second,
		Init:    mustJSON(t, map[string]any{"tools": []map[string]string{}}),
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	if res.TimedOut || res.ExitCode != 0 {
		t.Fatalf("超限的返回值不该把整次执行弄坏: timedout=%v exit=%d text=%q",
			res.TimedOut, res.ExitCode, truncate(res.Text, 300))
	}
	if res.Value == nil {
		t.Fatalf("Value = nil, want 一句降级说明（帧必须发出来）: text=%q raw len=%d",
			truncate(res.Text, 300), len(res.Raw))
	}
	var notice string
	if err := json.Unmarshal(res.Value, &notice); err != nil {
		t.Fatalf("降级说明应当是 JSON 字符串: %s (%v)", truncate(string(res.Value), 200), err)
	}
	if !strings.Contains(notice, "exceeds") || !strings.Contains(notice, "transport limit") {
		t.Errorf("降级说明没讲清原因（模型无法据此自我修正）: %q", notice)
	}
	// 宿主的帧解码上限是 4 MiB（按行）：整条 result 帧必须远低于它，否则帧会被丢掉。
	if len(res.Raw) > 1<<20 {
		t.Errorf("Raw 长度 = %d, want 远小于 4 MiB 的单行上限（帧会被宿主丢掉）", len(res.Raw))
	}
}

// ---- ⑨ 行号对齐：堆栈里的行号与用户脚本 1:1 ----

// TestExecuteScriptStackTraceLineNumbersMatchUserScript Phase 1 为行号对齐付过代价
// （prelude 经 --import 注入而不是拼接），Phase 1-A2 换了求值形态之后这条必须继续成立：
// 用户脚本第 N 行抛错，报错文本里的帧就必须指向第 N 行。
//
// 为什么断言**精确等号**而不是「含 :3」：AsyncFunction 的包装头会给行号加一个常数偏移
// （本机 V8 实测 +2），「含 :3」在 5 或 1 上也能凑巧命中；只有等号能钉住「没有偏移」。
// 两条路径都要钉：本模块 await 到的脚本异常（同步 throw / reject），以及**不走这个
// await** 的报错（setTimeout 回调里抛 → prelude 的 uncaughtException → 同样经过
// 行号回补，见 prelude_bridge.js 的 rewriteStack）。
func TestExecuteScriptStackTraceLineNumbersMatchUserScript(t *testing.T) {
	requireNode(t)

	cases := []struct {
		name     string
		script   string
		wantLine int
	}{
		{
			name: "顶层 throw",
			// 第 3 行 throw（前两行是陪衬，只为把行号顶到非 1 的位置）
			script:   "const a = 1;\nconst b = 2;\nthrow new Error('line-marker-top');",
			wantLine: 3,
		},
		{
			name: "await 之后 throw",
			script: "const r = await tools.ping({});\n" +
				"if (r.pong) {\n" +
				"  throw new Error('line-marker-after-await');\n" +
				"}",
			wantLine: 3,
		},
		{
			name: "setTimeout 回调里 throw",
			// 回调体第 2 行 throw：这条路径由 prelude 的 uncaughtException 上报，
			// 不经过本模块的 await，故它单独证明 rewriteStack 插槽在起作用。
			script: "setTimeout(() => {\n" +
				"  throw new Error('line-marker-async-callback');\n" +
				"}, 20);\n" +
				"await tools.ping({});",
			wantLine: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newScriptExecutor(t)
			res, err := e.ExecuteScript(context.Background(), ScriptOpts{
				Script:  tc.script,
				Timeout: 15 * time.Second,
				Init:    mustJSON(t, map[string]any{"tools": []map[string]string{initTool("ping", "")}}),
				OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
					return ScriptResult{Value: json.RawMessage(`{"pong":true}`)}
				},
			})
			if err != nil {
				t.Fatalf("ExecuteScript: %v (raw=%q)", err, res.Raw)
			}
			if !strings.Contains(res.Text, "[script error]") {
				t.Fatalf("报错没进 [script error] 段:\n%s", res.Text)
			}

			m := userFrameRe.FindStringSubmatch(res.Text)
			if m == nil {
				t.Fatalf("堆栈里没有用户脚本的帧（sourceURL 丢了？）:\n%s", res.Text)
			}
			if got := m[1]; got != strconv.Itoa(tc.wantLine) {
				t.Errorf("堆栈行号 = %s, want %d（脚本第 %d 行 throw）—— 行号偏移会让模型改错行\n%s",
					got, tc.wantLine, tc.wantLine, res.Text)
			}
			// 帧名必须是 sourceURL 给的那个短名字，不是 data: URL 或 <anonymous>：
			// 后者会把整份桥接源码的 base64 灌进模型上下文（实测上万字节）。
			if strings.Contains(res.Text, "data:text/javascript") {
				t.Errorf("报错文本里出现 data: URL 帧（会把 base64 源码灌进上下文）:\n%s", res.Text)
			}
		})
	}
}

// userFrameRe 取用户脚本帧的行号（prelude_bridge.js 的 sourceURL）。
var userFrameRe = regexp.MustCompile(`codemode-script\.mjs:(\d+):`)

// TestExecuteScriptStaticImportGivesActionableError AsyncFunction 形态**唯一**的语法级
// 取舍是静态 import 不可用（函数体不是模块，pi 同样不支持）。裸的
// "Cannot use import statement outside a module" 会被模型读成「沙箱坏了」，故报错里
// 必须带一句可行动的替代写法（await import）。
func TestExecuteScriptStaticImportGivesActionableError(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)

	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script:  "import fs from 'node:fs';\nconsole.log('never runs', typeof fs);\n",
		Timeout: 15 * time.Second,
		Init:    mustJSON(t, map[string]any{"tools": []map[string]string{}}),
	})
	if err != nil {
		t.Fatalf("语法错误不该是 error（属脚本面）: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want 非零（编译失败必须反映在结局里）: %q", res.Text)
	}
	for _, want := range []string{"SyntaxError", "static `import` does not", "await import"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("Text 缺少 %q（模型需要可行动的替代写法）:\n%s", want, res.Text)
		}
	}
	// 语法错误下 V8 不给行号（构造函数形态的既有事实），故这里**不**断言行号；
	// 但绝不能把脚本正文回显出来（正文里可能有凭据）。
	if strings.Contains(res.Text, "typeof fs") {
		t.Errorf("报错回显了脚本正文:\n%s", res.Text)
	}
}

// TestExecuteScriptUnsatisfiableAwaitIsBoundedByHostClock 脚本 await 一个永远不会 settle
// 的 promise、且没有任何在途调用时，进程必须**活着**等宿主的墙钟。
//
// 这是 Phase 1-A2 明确接受的一处语义变化，写进测试是为了让它「被知道」而不是「被撞上」：
// Phase 1-A 的入口模块形态下 node 会打一行 "unsettled top-level await" 并以 13 快速退出；
// 本形态下脚本由桥接层求值（预加载模块的顶层 await node 不为它保命），故由本层 ref 住
// stdin 保活（见 prelude_bridge.js 的保活曲线），结局统一成 [TIMEOUT …] —— 与「死循环」
// 「调用不回执」两种挂死同一形态，且**不会**出现「有握手、没有 result 帧」的静默退出。
func TestExecuteScriptUnsatisfiableAwaitIsBoundedByHostClock(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)
	const budget = 1500 * time.Millisecond

	start := time.Now()
	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script:  `console.log("before-the-hang"); await new Promise(() => {});`,
		Timeout: budget,
		Init:    mustJSON(t, map[string]any{"tools": []map[string]string{}}),
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("TimedOut = false（进程静默退出了？）: exit=%d text=%q raw=%q",
			res.ExitCode, res.Text, res.Raw)
	}
	// 有界返回：timeout + killWaitBudget(4s) + killEscalationWait(2s)。
	if limit := budget + 6*time.Second; elapsed > limit {
		t.Errorf("返回耗时 %v, want <= %v", elapsed, limit)
	}
	if !strings.HasPrefix(res.Text, "[TIMEOUT after") {
		t.Errorf("Text 首行未声明 TIMEOUT:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "nothing could be recovered") {
		t.Errorf("挂死脚本的输出确实拿不回来，文案必须说清:\n%s", res.Text)
	}
	// 结果帧没到 ⇒ 没有结局值（不是「返回了 null」）。
	if res.Value != nil {
		t.Errorf("Value = %s, want nil（脚本没跑到收尾）", res.Value)
	}
}

// ---- ⑩ 脚本正文不落盘（Phase 1 的性质，Phase 1-A2 必须收回）----

// TestExecuteScriptSourceNeverHitsDisk 硬性验收：脚本正文不出现在临时目录的**任何文件**
// 里 —— 执行期间（OnCall 时刻，进程正活着）与结束后都查一遍。
//
// 为什么这条值钱：Phase 1-A 把脚本写成 `script.mjs`（0600、退出即删），等于把
// 「脚本可能含密钥/路径」这件事又摆回了 /tmp。Phase 1 用 stdin 换来的性质是**不落盘**，
// 不是「落盘了但删掉」。本用例用一条只可能来自脚本正文的独有标记串来查（脚本自己
// 不会打印它 —— 否则断言会被自己的输出打脸）。
func TestExecuteScriptSourceNeverHitsDisk(t *testing.T) {
	requireNode(t)

	var mu sync.Mutex
	var seen runFiles
	oldHook := onRunFiles
	onRunFiles = func(f runFiles) {
		mu.Lock()
		defer mu.Unlock()
		seen = f
	}
	defer func() { onRunFiles = oldHook }()
	takeSeen := func() runFiles {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}

	const marker = "SOURCE-MARKER-8c1d4f"
	script := "// " + marker + " — this comment must never reach a file on disk\n" +
		"const secretish = 'API-KEY-LOOKALIKE-9f2a';\n" +
		"const r = await tools.ping({});\n" +
		"console.log('ran:', r.pong);\n"

	// scan 列出目录下所有文件的内容（运行时目录里只有 spool，但这里不假设文件名：
	// 将来谁又加了资材，这条断言照样拦得住）。目录已被删掉 = 没有任何内容留在盘上，
	// 对「脚本不落盘」这条断言是**通过**（收尾路径删干净了）。
	scan := func(t *testing.T, when string, wantPresent bool) {
		t.Helper()
		f := takeSeen()
		if f.dir == "" {
			t.Errorf("onRunFiles 没拿到路径（%s的内容断言会空转）", when)
			return
		}
		entries, err := os.ReadDir(f.dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && !wantPresent {
				return // 结束后目录已删：盘上没有任何副本，正是期望
			}
			t.Errorf("%s读临时目录失败: %v", when, err)
			return
		}
		if len(entries) == 0 && wantPresent {
			t.Errorf("%s临时目录是空的（断言空转：spool 应当在）", when)
		}
		for _, en := range entries {
			b, err := os.ReadFile(filepath.Join(f.dir, en.Name()))
			if err != nil {
				t.Errorf("%s读 %s 失败: %v", when, en.Name(), err)
				continue
			}
			for _, needle := range []string{marker, "API-KEY-LOOKALIKE-9f2a"} {
				if strings.Contains(string(b), needle) {
					t.Errorf("%s：脚本正文出现在临时文件 %s 里（标记 %q）—— 脚本必须只走 init.script",
						when, en.Name(), needle)
				}
			}
		}
	}

	e := newScriptExecutor(t)
	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script:  script,
		Timeout: 15 * time.Second,
		Init:    mustJSON(t, map[string]any{"tools": []map[string]string{initTool("ping", "")}}),
		OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
			scan(t, "执行期间", true) // 进程正阻塞在这次调用上：文件都在、spool 有内容
			return ScriptResult{Value: json.RawMessage(`{"pong":true}`)}
		},
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	if !strings.Contains(res.Text, "ran: true") {
		t.Fatalf("脚本没跑通（这条用例的意义在于它真的执行了）:\n%s", res.Text)
	}
	scan(t, "执行结束后（目录还在时）", false)
	// 结束后目录应当已被删掉：删不掉也意味着内容留在 /tmp（同一条凭据面）。
	if f := takeSeen(); f.dir != "" {
		if _, err := os.Stat(f.dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("临时目录在结束后仍存在: %s (err=%v)", f.dir, err)
		}
	}
}

// ---- ⑪ exit(v)：提前、干净地结束脚本 ----

// TestExecuteScriptExitEndsScriptEarlyWithValue 钉住传输层提供的 globalThis.exit(value)：
// 置结果 + 立刻结束（**不是**异常路径 —— 用户自己的 try/catch 不该把它吞掉），
// 且与 process.exitCode 无关（本次执行以 0 收尾）。
func TestExecuteScriptExitEndsScriptEarlyWithValue(t *testing.T) {
	requireNode(t)

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "exit(对象)",
			body: "console.log('before-exit');\n" +
				"exit({ early: true, n: 7 });\n" +
				"console.log('BUG: kept running');\n",
			want: `{"early":true,"n":7}`,
		},
		{
			name: "exit(字符串)",
			body: `exit("stopped-here");` + "\n" + `console.log("BUG: kept running");`,
			want: `"stopped-here"`,
		},
		{
			name: "try/catch 里 exit 不被吞",
			body: "try {\n" +
				"  exit('early-in-try');\n" +
				"} catch (e) {\n" +
				"  console.log('BUG: exit() 被当成异常吞掉了');\n" +
				"}\n" +
				"console.log('BUG: kept running');\n",
			want: `"early-in-try"`,
		},
		{
			name: "await 之后 exit",
			body: "const r = await tools.ping({});\n" +
				"exit({ pong: r.pong });\n",
			want: `{"pong":true}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newScriptExecutor(t)
			start := time.Now()
			res, err := e.ExecuteScript(context.Background(), ScriptOpts{
				Script:  tc.body,
				Timeout: 15 * time.Second,
				Init:    mustJSON(t, map[string]any{"tools": []map[string]string{initTool("ping", "")}}),
				OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
					return ScriptResult{Value: json.RawMessage(`{"pong":true}`)}
				},
			})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("ExecuteScript: %v (raw=%q)", err, res.Raw)
			}
			if res.TimedOut || res.Canceled {
				t.Fatalf("exit() 应当是干净收尾（timedout=%v canceled=%v）: %q", res.TimedOut, res.Canceled, res.Text)
			}
			if res.ExitCode != 0 {
				t.Errorf("ExitCode = %d, want 0（exit 不是异常路径；与 process.exitCode 无关）\nText=%q",
					res.ExitCode, res.Text)
			}
			if string(res.Value) != tc.want {
				t.Errorf("Value = %s, want %s\nText=%q\nRaw=%q", res.Value, tc.want, res.Text, res.Raw)
			}
			if strings.Contains(res.Text, "BUG:") {
				t.Errorf("exit() 之后的代码还在跑（提前结束没生效）:\n%s", res.Text)
			}
			if elapsed > 5*time.Second {
				t.Errorf("exit() 提前结束却花了 %v", elapsed)
			}
		})
	}
}

// ---- ⑫ scaffold：宿主注入的全局槽 + hostCall 往返 ----

// TestExecuteScriptScaffoldGlobalsAndHostCall 钉住 ScriptOpts.Scaffold 的契约：
// 在用户脚本**之前**求值、签名 (ctx)、ctx.hostCall 走既有 call/reply 路径、
// 它挂在 globalThis 上的键对用户脚本可见；顺带钉住「传输字段不进 __codemode.init」。
func TestExecuteScriptScaffoldGlobalsAndHostCall(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)
	rec := &callRecorder{}

	scaffold := `
const { hostCall, init } = ctx;
globalThis.text = (s) => "text:" + s;
const r = await hostCall("ping", { from: "scaffold" });
globalThis.scaffoldSaw = r.pong + "/" + init.marker;
`
	script := `
console.log("text:", text("hi"));
console.log("scaffoldSaw:", scaffoldSaw);
console.log("initKeys:", Object.keys(__codemode.init).sort().join(","));
console.log("scaffoldCtxLeaked:", typeof ctx, typeof hostCall);
const r = await tools.ping({ from: "script" });
return { pong: r.pong, text: text("bye") };
`
	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script:   script,
		Scaffold: scaffold,
		Timeout:  15 * time.Second,
		Init:     json.RawMessage(`{"tools":[{"name":"ping"}],"marker":"INIT-MARKER-7d"}`),
		OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
			rec.add(c)
			return ScriptResult{Value: json.RawMessage(`{"pong":true}`)}
		},
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v (raw=%q)", err, res.Raw)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0\nText=%q\nRaw=%q", res.ExitCode, res.Text, res.Raw)
	}
	for _, want := range []string{
		"text: text:hi",                          // scaffold 定义的全局对用户脚本可见
		"scaffoldSaw: true/INIT-MARKER-7d",       // scaffold 经 hostCall 往返了一次，且能读到 ctx.init
		"initKeys: marker,tools",                 // script/scaffold 是传输字段，不进 __codemode.init
		"scaffoldCtxLeaked: undefined undefined", // ctx/hostCall 是 scaffold 的入参，不泄漏成全局
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("Text 缺少 %q\nText=%q", want, res.Text)
		}
	}
	if string(res.Value) != `{"pong":true,"text":"text:bye"}` {
		t.Errorf("Value = %s, want {\"pong\":true,\"text\":\"text:bye\"}", res.Value)
	}
	// hostCall 与 tools.* 走同一条路径：宿主看到的两次调用都按顺序、名字与参数都对。
	calls := rec.all()
	if len(calls) != 2 {
		t.Fatalf("宿主收到 %d 次调用, want 2: %+v", len(calls), calls)
	}
	if calls[0].Name != "ping" || !strings.Contains(string(calls[0].Args), "scaffold") {
		t.Errorf("第一次调用应当来自 scaffold 的 hostCall: %+v", calls[0])
	}
	if calls[1].Name != "ping" || !strings.Contains(string(calls[1].Args), "script") {
		t.Errorf("第二次调用应当来自用户脚本的 tools.ping: %+v", calls[1])
	}
}

// TestExecuteScriptScaffoldErrorStopsBeforeScript scaffold 报错时必须**在用户脚本之前**
// 收尾：装不上全局还硬跑，只会把宿主的一个 bug 变成「模型写了不存在的函数」。
func TestExecuteScriptScaffoldErrorStopsBeforeScript(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)

	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script:   `console.log("SCRIPT-RAN-SHOULD-NOT-HAPPEN");`,
		Scaffold: `throw new Error("scaffold-boom-marker");`,
		Timeout:  15 * time.Second,
		Init:     mustJSON(t, map[string]any{"tools": []map[string]string{}}),
	})
	if err != nil {
		t.Fatalf("scaffold 报错不该是 error（属脚本面）: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want 非零（scaffold 失败必须反映在结局里）: %q", res.Text)
	}
	if !strings.Contains(res.Text, "scaffold-boom-marker") {
		t.Errorf("Text 里没有 scaffold 的报错:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "the host-injected scaffold failed") {
		t.Errorf("报错没点名是宿主注入的 scaffold（模型会以为是自己写错了）:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "SCRIPT-RAN-SHOULD-NOT-HAPPEN") {
		t.Errorf("scaffold 失败后用户脚本仍然跑了:\n%s", res.Text)
	}
	if res.Value != nil {
		t.Errorf("Value = %s, want nil（脚本没跑完）", res.Value)
	}
}

// ---- ⑬ 1 MiB 量级脚本经 init 通道 ----

// TestExecuteScriptLargeScriptThroughInit 脚本正文现在整条走 init（stdin），故要钉住
// 「大脚本也进得去、跑得动」。1 MiB 是**量级**验证，不是边界验证：宿主→沙箱这条 stdin
// 通道没有逐行上限（stdjson.MaxLine 4 MiB 只管宿主**读** spool 那一侧），本用例只证明
// 常规大脚本不会被某处静默截断（截断会表现为语法错误或执行到一半的怪状态）。
func TestExecuteScriptLargeScriptThroughInit(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)

	// 造一个 ≥1 MiB 的脚本：每行都是合法且互不干扰的语句（不重复声明同名变量），
	// 末尾留一个只有执行到最后才会打印的标记。
	var b strings.Builder
	b.WriteString("const MARKER_BIG = 'BIG-SCRIPT-MARKER-7f2a';\n")
	const fillerLine = "void '" + "x" + "';\n"
	for b.Len() < 1<<20 {
		b.WriteString(fillerLine)
	}
	b.WriteString("console.log('big-script-reached');\n")
	b.WriteString("return { marker: MARKER_BIG, bytes: " + strconv.Itoa(b.Len()) + " };\n")
	script := b.String()
	if len(script) < 1<<20 {
		t.Fatalf("用例自身没造出 1 MiB 脚本（%d 字节）—— 断言会空转", len(script))
	}

	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script:  script,
		Timeout: 30 * time.Second,
		Init:    mustJSON(t, map[string]any{"tools": []map[string]string{}}),
	})
	if err != nil {
		t.Fatalf("ExecuteScript(%d 字节脚本): %v (raw=%q)", len(script), err, truncate(res.Raw, 500))
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0（%d 字节脚本没跑通）\nText=%q",
			res.ExitCode, len(script), truncate(res.Text, 500))
	}
	if !strings.Contains(res.Text, "big-script-reached") {
		t.Errorf("脚本没执行到最后一行（中途被截断？）:\n%s", truncate(res.Text, 500))
	}
	if !strings.Contains(string(res.Value), "BIG-SCRIPT-MARKER-7f2a") {
		t.Errorf("Value = %s, want 含 BIG-SCRIPT-MARKER-7f2a", res.Value)
	}
	t.Logf("1 MiB 量级脚本（%d 字节）经 init 通道执行成功", len(script))
}

// ---- 宿主侧规格构造（假 Sandbox：不涉及进程行为）----

// recordingSandbox 只记录 ExecSpec，不跑任何进程。
type recordingSandbox struct {
	mu   sync.Mutex
	spec gosandbox.ExecSpec
	out  string
}

func (r *recordingSandbox) Run(ctx context.Context, spec gosandbox.ExecSpec) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spec = spec
	return r.out, nil
}

func (r *recordingSandbox) last() gosandbox.ExecSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spec
}

// TestExecuteScriptSpecContract 钉住宿主侧的**规格构造**：三态时限、命令行、stdin 让给
// 桥接、脚本既不落命令行也不落盘（Phase 1-A2：入口是占位的 `--eval ”`，正文走
// init.script）。用假 Sandbox 是因为这几条与进程行为无关（真跑一遍也看不出
// Timeout=-1 与 Timeout=0 的差别，那条差别只在 ExecSpec 里），且这样不依赖本机 node。
func TestExecuteScriptSpecContract(t *testing.T) {
	cases := []struct {
		name string
		opts time.Duration
		want time.Duration
	}{
		{"<0 不限时", -1, -1},
		{"==0 用默认", 0, DefaultTimeout},
		{">0 用调用方的", 7 * time.Second, 7 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sbx := &recordingSandbox{}
			e := New(sbx, nil)
			// 直接填 memo：跳过解释器探测（本测试与 node 无关）。
			e.memo = NodeInfo{Path: "/usr/bin/node", Version: "v25.8.1", Major: 25}

			_, err := e.ExecuteScript(context.Background(), ScriptOpts{
				Script:        `console.log("x");`,
				Timeout:       tc.opts,
				MaxOldSpaceMB: 123,
				Init:          json.RawMessage(`{"tools":[]}`),
			})
			if err != nil {
				t.Fatalf("ExecuteScript: %v", err)
			}
			spec := sbx.last()
			if spec.Timeout != tc.want {
				t.Errorf("ExecSpec.Timeout = %v, want %v", spec.Timeout, tc.want)
			}
			// stdin 必须让给桥接（不能是 nil：nil = 子进程读到 /dev/null，init 就没处送）。
			if spec.Stdin == nil {
				t.Error("ExecSpec.Stdin == nil：宿主→沙箱通道没接上（脚本走临时文件，stdin 整条归桥接）")
			}
			cmd := spec.Command
			if got := strings.Count(cmd, "--import "); got != 2 {
				t.Errorf("命令行有 %d 个 --import, want 2（协议端 + 桥接端）: %s", got, cmd)
			}
			if !strings.Contains(cmd, "max-old-space-size=123") {
				t.Errorf("命令行没带上调用方给的堆上限: %s", cmd)
			}
			if strings.Contains(cmd, "--input-type=module -") {
				t.Errorf("stdin 已让给桥接，不该再从 stdin 读脚本: %s", cmd)
			}
			if !strings.Contains(cmd, "frames.ndjson") || !strings.Contains(cmd, " 2>&1") {
				t.Errorf("fd1/fd2 没重定向到帧 spool（宿主读不到 call 帧）: %s", cmd)
			}
			if strings.Contains(cmd, "console.log") {
				t.Errorf("脚本正文出现在命令行里（会经 sh -c 并在进程表里裸奔）: %s", cmd)
			}
			// Phase 1-A2：入口是占位的空脚本。少了它，node 在 stdin 非 TTY 时会把 stdin
			// 当脚本源码读 —— 而 stdin 整条是桥接通道，init 那一行会被当代码求值掉。
			if !strings.Contains(cmd, "--eval ''") {
				t.Errorf("命令行缺 `--eval ''` 占位入口（node 会把 stdin 当脚本读）: %s", cmd)
			}
			// 脚本正文只走 init.script：命令行里既没有脚本路径，也没有别处的副本。
			if strings.Contains(cmd, "script.mjs") {
				t.Errorf("脚本路径仍在命令行里（脚本必须只经 init.script 进沙箱）: %s", cmd)
			}
		})
	}
}

// TestExecuteScriptNodeMissingGivesReadableError Node 缺失（自动探测全落空）时，
// 必须在**建临时文件之前**失败，且错误对模型可读。
func TestExecuteScriptNodeMissingGivesReadableError(t *testing.T) {
	t.Setenv("GO_CODE_NODE_CANDIDATES", filepath.Join(t.TempDir(), "definitely-absent", "node"))

	e := New(gosandbox.NoSandbox{}, runtime.NewNodeRuntime(runtime.NodeConfig{}))
	_, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script: `console.log(1)`,
		Init:   json.RawMessage(`{"tools":[]}`),
	})
	if err == nil {
		t.Fatal("Node 缺失却没报 error")
	}
	for _, want := range []string{"Node.js", ">= 20", "brew install node"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误缺 %q（模型无法据此自我修正）:\n%v", want, err)
		}
	}
}

// TestExecuteScriptEmptyScriptRejected 空脚本是参数错误，不该建临时文件、更不该 spawn。
func TestExecuteScriptEmptyScriptRejected(t *testing.T) {
	var filesSeen bool
	oldHook := onRunFiles
	onRunFiles = func(runFiles) { filesSeen = true }
	defer func() { onRunFiles = oldHook }()

	e := New(gosandbox.NoSandbox{}, nil)
	e.memo = NodeInfo{Path: "/usr/bin/node", Major: 25}
	if _, err := e.ExecuteScript(context.Background(), ScriptOpts{Script: "  \n\t "}); err == nil {
		t.Fatal("空脚本未被拒绝")
	}
	if filesSeen {
		t.Error("空脚本不该建临时文件（还没到落盘那一步）")
	}
}

// TestExecuteScriptOnOutputReceivesScriptOutput OnOutput 只在结束时给一次
// （Phase 2 明确允许），且只给脚本自己产生的输出 —— 不含结局注记与方框文本。
func TestExecuteScriptOnOutputReceivesScriptOutput(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)

	var mu sync.Mutex
	var got []string
	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script: `
console.log("first line");
const r = await tools.ping({});
console.log("second line:", r.pong);
`,
		Timeout: 15 * time.Second,
		Init:    mustJSON(t, map[string]any{"tools": []map[string]string{initTool("ping", "")}}),
		OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
			return ScriptResult{Value: json.RawMessage(`{"pong":true}`)}
		},
		OnOutput: func(text string) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, text)
		},
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("OnOutput 调用了 %d 次, want 1（Phase 2 允许只在结束时给）: %q", len(got), got)
	}
	for _, want := range []string{"first line", "second line: true"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("OnOutput 文本缺少 %q: %q", want, got[0])
		}
	}
	if strings.Contains(got[0], "[exit:") || strings.Contains(got[0], "TIMEOUT") {
		t.Errorf("OnOutput 不该带宿主侧的结局注记: %q", got[0])
	}
	if !strings.Contains(res.Text, "second line: true") {
		t.Errorf("Text 里没有脚本输出:\n%s", res.Text)
	}
}

// TestExecuteScriptDirectFDWriteStaysVisible 脚本直写 fd1（绕开被替换的
// process.stdout.write）的内容仍要让模型看见：spool 重定向没有改变这条 Phase 1 契约。
func TestExecuteScriptDirectFDWriteStaysVisible(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)

	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script: `
const fs = await import('node:fs');
fs.writeSync(1, "stray-direct-write\n");
fs.writeSync(2, "stray-direct-write-stderr\n");
console.log("captured-log");
`,
		Timeout: 15 * time.Second,
		Init:    mustJSON(t, map[string]any{"tools": []map[string]string{}}),
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	for _, want := range []string{"captured-log", "stray-direct-write", "stray-direct-write-stderr"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("Text 缺少 %q:\n%s", want, res.Text)
		}
	}
	if strings.Contains(res.Text, `"notify"`) {
		t.Errorf("裸协议 JSON 泄进了模型上下文:\n%s", res.Text)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestExecuteScriptNonJSONResultValueDoesNotHang ScriptResult.Value 不是合法 JSON 时
// 必须退化成文本，而不是让回执发不出去（那条路会伪装成「脚本超时」，是一次上层笔误
// 最难查的形态）。
func TestExecuteScriptNonJSONResultValueDoesNotHang(t *testing.T) {
	requireNode(t)
	e := newScriptExecutor(t)

	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Script: `
const v = await tools.rawish({});
console.log("type:", typeof v, "value:", v);
console.log("as-is:", await tools.rawish2({}));
`,
		Timeout: 8 * time.Second, // 卡死会撞到这个上限 —— 撞到即说明兜底失效
		Init: mustJSON(t, map[string]any{
			"tools": []map[string]string{initTool("rawish", ""), initTool("rawish2", "")},
		}),
		OnCall: func(ctx context.Context, c ScriptCall) ScriptResult {
			if c.Name == "rawish" {
				// 裸文本：不是 JSON（上层把 Go 字符串直接塞进 RawMessage 的典型笔误）
				return ScriptResult{Value: json.RawMessage(`ls: cannot access '/nope': No such file`)}
			}
			return ScriptResult{} // Value 为空 ⇒ 脚本侧 resolve(undefined)
		},
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	if res.TimedOut {
		t.Fatalf("回执没发到沙箱（Value 不是合法 JSON 时卡死了）: text=%q raw=%q", res.Text, res.Raw)
	}
	if !strings.Contains(res.Text, "type: string value: ls: cannot access '/nope': No such file") {
		t.Errorf("非法 JSON 的 Value 没有退化成可读文本:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "as-is: undefined") {
		t.Errorf("空 Value 应 resolve(undefined):\n%s", res.Text)
	}
}

// TestExecuteScriptFrameWithCallIsNotLeakedIntoText 帧白名单里新加的 call 必须被
// SplitFrames 收走（而不是落进 tail 泄给模型）：这是「多一种 notify」的回归钉子。
func TestExecuteScriptFrameWithCallIsNotLeakedIntoText(t *testing.T) {
	line := frameSentinel + `{"notify":"call","id":"3","name":"alpha","args":{"q":1}}`
	frames, tail := SplitFrames(strings.Join([]string{
		frameSentinel + `{"notify":"hello","version":{"version_major":1,"version_minor":0}}`,
		line,
		"user-output",
	}, "\n"))
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2（call 必须被认作帧）: %+v", len(frames), frames)
	}
	if frames[1].Notify != notifyCall || frames[1].Id != "3" || frames[1].Name != "alpha" {
		t.Errorf("call 帧解码不对: %+v", frames[1])
	}
	if frames[1].Args == nil || string(frames[1].Args) != `{"q":1}` {
		t.Errorf("call 帧的 Args 丢了: %s", frames[1].Args)
	}
	if !strings.Contains(tail, "user-output") || strings.Contains(tail, "notify") {
		t.Errorf("tail = %q, want 只有用户输出", tail)
	}
}

// TestScriptCallIndexFollowsFrameOrder dispatch 发的号必须按**帧顺序**单调，且与
// OnCall 何时被调度、回执谁先回来都无关（Phase 2 复核 F3 的机制钉子）。
//
// 用例形态刻意做成「到达/完成顺序必然反」：slow（脚本里第一条）在 OnCall 里等 fast
// 跑完才返回 ⇒ 完成顺序是 fast→slow。若 Index 是「谁先跑谁拿小号」或者干脆不单调，
// 断言立刻红；只有「在读帧处发号」才能同时满足两条。
func TestScriptCallIndexFollowsFrameOrder(t *testing.T) {
	requireNode(t)
	e := New(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var mu sync.Mutex
	byIndex := map[int64]string{}
	var arrived []string
	slowArrived := make(chan struct{})
	fastDone := make(chan struct{})

	res, err := e.ExecuteScript(ctx, ScriptOpts{
		Timeout: 20 * time.Second,
		// 工具表（沙箱的 tools Proxy 只放行表里的名字 —— 名字不在表里连帧都发不出来）。
		Init: json.RawMessage(`{"tools":[{"name":"slow_call","rawName":"slow_call"},` +
			`{"name":"fast_call","rawName":"fast_call"}]}`),
		Script: "const a = tools.slow_call({});\n" +
			"const b = tools.fast_call({});\n" +
			"const [ra, rb] = await Promise.all([a, b]);\n" +
			"return ra + '/' + rb;",
		OnCall: func(_ context.Context, c ScriptCall) ScriptResult {
			mu.Lock()
			byIndex[c.Index] = c.Name
			arrived = append(arrived, c.Name)
			mu.Unlock()
			switch c.Name {
			case "slow_call":
				close(slowArrived)
				<-fastDone // 等第二条先返回：证明两者真并发、且顺序信息不来自调度
				return ScriptResult{Value: json.RawMessage(`"slow"`)}
			case "fast_call":
				<-slowArrived // 反序保护：确保 slow 先登记
				close(fastDone)
				return ScriptResult{Value: json.RawMessage(`"fast"`)}
			}
			return ScriptResult{IsError: true, Value: json.RawMessage(`"unexpected"`)}
		},
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("脚本没跑完: exit=%d text=%q", res.ExitCode, res.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := byIndex[1]; got != "slow_call" {
		t.Errorf("Index=1 是 %q, want slow_call（脚本里第一条发起的）—— 号没在读帧处发？实测映射 %v", got, byIndex)
	}
	if got := byIndex[2]; got != "fast_call" {
		t.Errorf("Index=2 是 %q, want fast_call；实测映射 %v", got, byIndex)
	}
	// 否定控制：完成顺序确实是反的（fast 先返回），否则这条用例不构成对「调度无关」的证明。
	if len(arrived) != 2 || arrived[0] != "slow_call" {
		t.Logf("到达顺序 = %v（本机调度恰好正序，用例仍有效但没覆盖反序路径）", arrived)
	}
}
