package execproc

// executor_test.go：沙箱执行层的端到端行为测试。
//
// 全部用**真 node** 跑（不注入假运行时）—— 这一层的价值全在真实进程行为里：
// stdin 喂脚本、--import 注入 prelude、超时被杀、进程组无残留、环境白名单。
// mock 掉它就等于把这些最该被钉住的东西全测没了。
//
// 依赖 node：node 可用性由 requireNode 在每个测试开头判定，不可用则 t.Skip
// （理由写在 helper 里）。**Node 缺失路径本身**由 runtime 包的纯单测覆盖
// （用注入的 RunCmd 模拟「什么都没装」），不依赖本机环境。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/runtime"
	gosandbox "github.com/seven7628/hai-harness/sandbox"
)

// requireNode 判定 node 可用；不可用则跳过（理由明确写在跳过原因里）。
func requireNode(t *testing.T) {
	t.Helper()
	// 与生产探测同一条路径（PATH + 候选目录），但不做版本门：版本门本身
	// 由 runtime.NodeRuntime 的单测覆盖，这里只关心「有没有能跑的 node」。
	e := New(nil, nil)
	n, err := e.node(context.Background())
	if err != nil {
		requireOrSkip(t, "本机无可用 node（%v）", err)
	}
	if n.Path == "" {
		t.Fatal("node 探测返回空路径")
	}
}

// requireOrSkip 环境依赖缺失时的统一处置：默认**跳过**（开发者机器上没装 node 不该
// 让整套测试红），但设了 GO_CODE_REQUIRE_NODE=1 就**失败**。
//
// 为什么必须有这个开关：CI 上一台没装 node 的机器会让 execproc 的 20+ 条端到端用例
// 全部 skip —— 流水线一片绿，实际零覆盖（本仓历史上正是这么丢过整包回归网）。
// release.yml 的 Go 测试步骤显式设这个变量，把「静默跳过」变成「响亮失败」。
func requireOrSkip(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("GO_CODE_REQUIRE_NODE") == "1" {
		t.Fatalf("环境依赖缺失且 GO_CODE_REQUIRE_NODE=1（CI 不允许静默跳过）："+format, args...)
	}
	t.Skipf("跳过："+format+"（本地开发允许跳过；CI 设 GO_CODE_REQUIRE_NODE=1 即失败）", args...)
}

// newTestExecutor 建一个 cwd=临时目录的执行器（工作区根语义）。
func newTestExecutor(t *testing.T) *Executor {
	t.Helper()
	return New(gosandbox.NoSandbox{}, nil)
}

// TestScriptViaStdinRunsAndReturnsOutput 核心路径：脚本源码经 **stdin** 进入
// 解释器（不是命令行、不是临时文件），输出被收回。
func TestScriptViaStdinRunsAndReturnsOutput(t *testing.T) {
	requireNode(t)
	e := newTestExecutor(t)
	e.opt.Cwd = t.TempDir()

	script := `
const r = await Promise.all([1, 2, 3].map(async (n) => n * 2));
console.log("doubled:", r.join(","));
console.error("to stderr too");
const j = JSON.stringify({ ok: true, n: r.length });
console.log(j);
`
	res, err := e.Execute(context.Background(), script)
	if err != nil {
		t.Fatalf("Execute: %v (raw=%q)", err, res.Raw)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (text=%q)", res.ExitCode, res.Text)
	}
	for _, want := range []string{"doubled: 2,4,6", "to stderr too", `{"ok":true,"n":3}`} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("Text missing %q\nText=%q\nRaw=%q", want, res.Text, res.Raw)
		}
	}
	// 握手必须发生过（prelude 起来了）。
	if res.Hello.Major != protoMajor {
		t.Errorf("Hello.Major = %d, want %d", res.Hello.Major, protoMajor)
	}
	// Execute 路径**不填** Value：脚本在这里是 ESM 入口模块（没有 return 承诺，
	// 也没有桥接层把值带回来）。这条断言钉住「Execute 行为不变」——
	// 值通道只属于 ExecuteScript（见 RunResult.Value 的注释）。
	if res.Value != nil {
		t.Errorf("Execute 路径的 Value = %s, want nil（该路径没有 return 承诺）", res.Value)
	}
}

// TestScriptSourceNotInCommandLine 钉住「脚本走 stdin」这条设计约束：
// 脚本正文不得出现在命令行里（命令行会经 sh -c，且会在进程表里裸奔）。
func TestScriptSourceNotInCommandLine(t *testing.T) {
	requireNode(t)
	e := newTestExecutor(t)
	n, err := e.node(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secret := "CANARY_MUST_NOT_BE_IN_ARGV"
	cmd := e.command(n)
	if strings.Contains(cmd, secret) {
		t.Fatalf("command line contains the script source: %s", cmd)
	}
	// 但注入标志必须在，否则脚本根本不会被评估。
	for _, want := range []string{"--import", "--input-type=module", " -"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q: %s", want, cmd)
		}
	}
}

// TestStackTraceLineNumbersMatchUserScript 钉住 prelude 的注入方式选型理由：
// 拼接 prelude 会让堆栈行号整体偏移（模型无法据此自我修正），
// --import 是独立模块，行号必须与用户脚本逐行对齐。
func TestStackTraceLineNumbersMatchUserScript(t *testing.T) {
	requireNode(t)
	e := newTestExecutor(t)

	// 用户脚本第 2 行是 throw（首行是空行，只为把 throw 顶到第 2 行）。
	script := "\nthrow new Error('line-marker');"
	res, err := e.Execute(context.Background(), script)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "line-marker") {
		t.Fatalf("script error not surfaced: %q", res.Text)
	}
	// 堆栈里应出现 stdin 模块的形态与第 2 行。行号出现即证明没有整体偏移
	// （prelude 若被拼进来，报错会指到第 8 行或更后 —— prelude 有 100+ 行）。
	if !strings.Contains(res.Text, ":2") && !strings.Contains(res.Text, "at 1:2") {
		t.Logf("堆栈未出现可判定的行号（不判失败，Node 各版本形态不同）:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "prelude.js") {
		t.Errorf("prelude 帧泄漏进用户堆栈（prelude 应是独立模块，不该出现在用户报错里）:\n%s", res.Text)
	}
}

// TestTimeoutKillsNoResidueAndReturnsInBudget 硬性验收：死循环被杀后
//
//	① `ps` 无残留；② 在 timeout + 6s（killWaitBudget+killEscalationWait）内返回。
//
// 这是 C8 事故（602s 挂起）的回归钉子 —— 那一版的 kill 是无上界等待。
func TestTimeoutKillsNoResidueAndReturnsInBudget(t *testing.T) {
	requireNode(t)
	e := newTestExecutor(t)
	const budget = 2 * time.Second

	e.opt.Timeout = budget
	// 脚本 fork 一个孙进程并让它一直跑：只杀直接子进程时孙进程会残留
	// （这正是 sandbox 杀整组的原因），同时脚本自身死循环。
	//
	// 孙进程刻意用 `sleep` 而**不是**忙等：go test 会并行跑各包，本测试的
	// 2s 窗口正好与 sandbox 包的计时敏感用例重叠。忙等会烧掉一整个核，
	// 实测把 sandbox.TestRunExecCancelStillBackfills 顶成 1/3 概率失败
	// （它断言取消路径 elapsed < 3s）。sleep 同样保持「进程活着」这一
	// 残留断言所需的性质，且零 CPU —— 别把它「优化」回忙等。
	// pid 不好从外部拿（都发生在子进程内），故让脚本把两个 pid 写进文件。
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	e.opt.Cwd = dir
	script := `
import { writeFileSync } from 'node:fs';
import { spawn } from 'node:child_process';
const kid = spawn('/bin/sh', ['-c', 'while true; do sleep 3600; done']);
writeFileSync(` + strconvQuote(pidFile) + `, process.pid + ' ' + kid.pid);
while (true) { await new Promise(r => setTimeout(r, 50)); }
`
	start := time.Now()
	res, err := e.Execute(context.Background(), script)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// ① 有界返回：timeout + killWaitBudget(4s) + killEscalationWait(2s)。
	// 这里给 1s 余量吸收调度抖动。
	limit := budget + 6*time.Second
	if elapsed > limit {
		t.Fatalf("returned in %v, want <= %v (text=%q)", elapsed, limit, res.Text)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true (text=%q raw=%q)", res.Text, res.Raw)
	}
	// 首行必须声明结局，且**不得**承诺一个拿不回来的「部分输出」：
	// 脚本输出在沙箱进程内缓冲、只在脚本正常收尾时回传，被杀 = 一条都拿不回来。
	if !strings.HasPrefix(res.Text, "[TIMEOUT after") {
		t.Errorf("Text 首行未声明 TIMEOUT:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "nothing could be recovered") {
		t.Errorf("Text 未说明输出不可回收（模型会把空当成真结果）:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "the output below is PARTIAL") {
		t.Errorf("无输出时仍承诺「下面的是部分输出」:\n%s", res.Text)
	}

	// ② 无残留。**pid 读不到即判失败**（不是跳过）：脚本在进入死循环前就写了
	// 文件，2s 预算足够它写完；读不到说明这条断言空转了 —— 空转的残留检查比
	// 没有更坏（它给人「验过无残留」的错觉）。
	pids := readPIDs(t, pidFile)
	if len(pids) == 0 {
		t.Fatalf("脚本没写出 pid 文件（%s），残留断言空转：\ntext=%q\nraw=%q", pidFile, res.Text, res.Raw)
	}
	waitNoLivePIDs(t, pids, 3*time.Second)
}

// TestIsolatedEnvHidesHostSecrets 硬性验收：isolatedEnv 生效 —— 脚本读不到
// 宿主环境变量（凭据面是最关键的那一条）。
func TestIsolatedEnvHidesHostSecrets(t *testing.T) {
	requireNode(t)
	// 在宿主进程里设一个「密钥」，脚本里读它：必须读不到。
	const key = "GO_CODE_CODEMODE_CANARY_SECRET"
	const val = "sk-should-never-be-visible-to-scripts"
	t.Setenv(key, val)

	e := newTestExecutor(t)
	res, err := e.Execute(context.Background(), `
console.log("has_secret=" + (process.env.`+key+` !== undefined));
console.log("has_anthropic=" + (process.env.ANTHROPIC_API_KEY !== undefined));
`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "has_secret=false") {
		t.Errorf("脚本读到了宿主环境变量 %s（isolatedEnv 未生效）:\n%s", key, res.Text)
	}
	if strings.Contains(res.Text, val) {
		t.Errorf("宿主密钥值泄漏进脚本输出（isolatedEnv 未生效）:\n%s", res.Text)
	}
	// 白名单内的变量仍在（否则连 PATH 都没有，是另一种故障）。
	if !strings.Contains(res.Text, "has_anthropic=false") {
		t.Errorf("缺失 ANTHROPIC_API_KEY 判定行:\n%s", res.Text)
	}
}

// TestNonZeroExitIsNotError 对齐 bash 工具语义：非零退出**不是** error，
// 输出与原因并入返回文本，模型自行修正。
func TestNonZeroExitIsNotError(t *testing.T) {
	requireNode(t)
	e := newTestExecutor(t)

	res, err := e.Execute(context.Background(), `console.log("before the throw"); null.x;`)
	if err != nil {
		t.Fatalf("脚本抛错却返回了 error（应并入文本）: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want non-zero (text=%q)", res.Text)
	}
	if !strings.Contains(res.Text, "before the throw") {
		t.Errorf("抛错前的输出丢了:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "script error") {
		t.Errorf("未标注 [script error]:\n%s", res.Text)
	}
}

// TestSyntaxErrorSurfaced 语法错误（模型手滑高频）必须可读地回到模型。
func TestSyntaxErrorSurfaced(t *testing.T) {
	requireNode(t)
	e := newTestExecutor(t)
	res, err := e.Execute(context.Background(), `const = ;;;`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("语法错误却退出 0:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "script error") {
		t.Errorf("语法错误未标注:\n%s", res.Text)
	}
}

// TestHelloVersionMismatchRejected 握手 major 不兼容必须被拒（且可读）。
func TestHelloVersionMismatchRejected(t *testing.T) {
	// 纯函数路径：直接喂一帧 major 不匹配的 hello。
	line := `{"notify":"hello","version":{"version_major":99,"version_minor":0,"runtime":"node"},"pid":1}`
	if _, err := VerifyHandshake(line, protoMajor); err == nil {
		t.Fatal("major 不匹配却通过了校验")
	} else if !strings.Contains(err.Error(), "99") || !strings.Contains(err.Error(), "protocol mismatch") {
		t.Errorf("错误未同时说明双方版本: %v", err)
	}

	// 真实路径：prelude 版本被改高 → 执行必须失败且给出可读原因。
	old := preludeJS
	preludeJS = strings.Replace(old, "const PROTO_MAJOR = 1;", "const PROTO_MAJOR = 99;", 1)
	defer func() { preludeJS = old }()

	e := newTestExecutor(t)
	res, err := e.Execute(context.Background(), `console.log("x")`)
	if err == nil {
		t.Fatalf("握手不兼容却没报错: text=%q", res.Text)
	}
	if !strings.Contains(err.Error(), "protocol") {
		t.Errorf("错误文案不像是协议不兼容: %v", err)
	}
}

// TestMissingHandshakeReported 沙箱没握手（prelude 没起来）时必须**说人话**，
// 而不是把 V8 崩溃栈灌给模型。
func TestMissingHandshakeReported(t *testing.T) {
	old := preludeJS
	// 注入一个会立刻崩掉的 prelude：握手永远发不出来。
	preludeJS = `throw new Error("prelude blew up");`
	defer func() { preludeJS = old }()

	e := newTestExecutor(t)
	res, err := e.Execute(context.Background(), `console.log("x")`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "no handshake") {
		t.Errorf("未报告握手缺失:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "stack") {
		t.Errorf("把崩溃栈灌给了模型:\n%s", res.Text)
	}
}

// TestEmptyScriptRejected 空脚本是参数错误，不该 spawn 一个进程。
func TestEmptyScriptRejected(t *testing.T) {
	e := newTestExecutor(t)
	if _, err := e.Execute(context.Background(), "   \n\t "); err == nil {
		t.Fatal("空脚本未被拒绝")
	}
}

// TestNodeMissingGivesReadableError Node 缺失时错误必须对模型可读。
//
// 走**自动探测全落空**那条真实路径（用户机器上没装 node），而不是「配置指错了」
// —— 后者的文案不同，测它等于没测模型真正会看到的那句。
func TestNodeMissingGivesReadableError(t *testing.T) {
	// 屏蔽候选列表（与 plugin/browser 的 GO_CODE_NODE_CANDIDATES 同款隔离手段）。
	t.Setenv("GO_CODE_NODE_CANDIDATES", filepath.Join(t.TempDir(), "definitely-absent", "node"))

	e := New(gosandbox.NoSandbox{}, runtime.NewNodeRuntime(runtime.NodeConfig{}))
	_, err := e.Execute(context.Background(), `console.log(1)`)
	if err == nil {
		t.Fatal("Node 缺失却没报错")
	}
	msg := err.Error()
	// 模型据这句话自我修正 ⇒ 必须同时有「缺什么」与「怎么装」。
	for _, want := range []string{"Node.js", ">= 20", "brew install node", "nodejs.org"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误缺 %q（模型无法据此自我修正）:\n%s", want, msg)
		}
	}
}

// TestConfiguredNodeBadGivesReadableError 配置的解释器不可用时也要可读。
func TestConfiguredNodeBadGivesReadableError(t *testing.T) {
	e := New(gosandbox.NoSandbox{}, runtime.NewNodeRuntime(runtime.NodeConfig{
		Node: filepath.Join(t.TempDir(), "no-such-node"),
	}))
	_, err := e.Execute(context.Background(), `console.log(1)`)
	if err == nil {
		t.Fatal("配置的解释器不存在却没报错")
	}
	for _, want := range []string{"不可用", "自动探测", ">= 20"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误缺 %q:\n%v", want, err)
		}
	}
}

// TestCommandShellSafe 命令行经 `sh -c` 解析：路径里的单引号必须被转义，
// 否则一条奇怪的工作区路径就能改写命令行。
func TestCommandShellSafe(t *testing.T) {
	n := NodeInfo{Path: "/weird/pa'th/node"}
	e := New(gosandbox.NoSandbox{}, nil)
	cmd := e.command(n)
	if !strings.Contains(cmd, `'/weird/pa'\''th/node'`) {
		t.Errorf("单引号未转义: %s", cmd)
	}
}

// ---- 辅助 ----

// strconvQuote 生成 JS 字符串字面量（测试脚本里嵌 Go 路径用）。
func strconvQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

// readPIDs 读脚本写下的 pid 列表（可能还没写 —— 超时太快时）。
func readPIDs(t *testing.T, path string) []int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		n, err := strconv.Atoi(f)
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// waitNoLivePIDs 轮询直到这些 pid 全部不再存活（或超时）。
func waitNoLivePIDs(t *testing.T, pids []int, within time.Duration) {
	t.Helper()
	if len(pids) == 0 {
		t.Fatalf("没有 pid 可检查（残留断言空转）")
	}
	deadline := time.Now().Add(within)
	for {
		live := livePIDs(pids)
		if len(live) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("超时后仍有存活进程 %v（进程组没杀干净）", live)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// livePIDs 返回仍存活的 pid 列表（signal 0 探活；EPERM 也算活着 ——
// 别人进程的权限问题不等于进程没了）。
func livePIDs(pids []int) []int {
	var live []int
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		if err := syscall.Kill(pid, 0); err == nil || err == syscall.EPERM {
			live = append(live, pid)
		}
	}
	return live
}

// TestMaxOldSpaceOOMKillsScriptNotHost 硬性验收：`--max-old-space-size` 生效 ——
// 分配超限抛 OOM，**宿主存活**，且模型能看到「是内存超限」而非一堆 V8 栈。
//
// 为什么不能用 RLIMIT_AS 替代：V8 启动即预留大块虚拟地址空间（heap reservation +
// JIT code range），设 RLIMIT_AS 的典型结果是「启动即失败或随机 OOM abort」——
// 设计文档 §14 评审 B8 明确否掉了那条路线。
func TestMaxOldSpaceOOMKillsScriptNotHost(t *testing.T) {
	requireNode(t)
	e := newTestExecutor(t)
	e.opt.MaxOldSpaceMB = 64
	e.opt.Timeout = 30 * time.Second

	// 反复分配 1MB 数组直到 V8 拒绝（约 2000 次即越过 64MB 堆上限）。
	res, err := e.Execute(context.Background(), `
const blocks = [];
try {
  for (let i = 0; i < 100000; i++) blocks.push(new Array(1e6).fill(i));
  console.log("allocated without hitting the heap limit");
} catch (e) {
  console.log("caught:", e.message);
}
`)
	// 无论脚本是撞墙 OOM 崩掉还是自己 catch 住，都**不是**宿主的 error。
	if err != nil {
		t.Fatalf("宿主报错（不该发生）: %v (raw=%q)", err, res.Raw)
	}
	// 必须真的撞墙。V8 的堆耗尽是**不可 catch** 的 abort（不是 RangeError）——
	// 所以「脚本自己 catch 住了」恰恰说明堆上限没生效（它还有余量分配）。
	if strings.Contains(res.Text, "allocated without hitting the heap limit") ||
		strings.Contains(res.Text, "caught:") {
		t.Fatalf("堆上限未生效（脚本仍分配成功或自行 catch 住 = 还有余量）：\n%s", res.Text)
	}
	oomish := strings.Contains(res.Text, "out of memory") ||
		strings.Contains(res.Text, "heap") ||
		strings.Contains(res.Text, "OOM") ||
		strings.Contains(res.Text, "Allocation failed")
	if !oomish {
		t.Errorf("撞了墙但看不出是内存超限（模型无法据此自我修正）：\n%s", res.Text)
	}
	// 宿主必须还能继续执行下一个脚本（这是「进程隔离」的核心价值）。
	res2, err2 := e.Execute(context.Background(), `console.log("host still alive");`)
	if err2 != nil {
		t.Fatalf("OOM 之后宿主无法继续执行: %v", err2)
	}
	if !strings.Contains(res2.Text, "host still alive") {
		t.Errorf("OOM 之后脚本未正常执行:\n%s", res2.Text)
	}
}

// TestCommandCarriesMaxOldSpaceFlag 断言命令行**真的带上** --max-old-space-size 及正确取值。
//
// 为什么需要这条：TestMaxOldSpaceOOMKillsScriptNotHost 分配约 100GB，**任何**堆上限
// （含 Node 默认值）都会让它 OOM，所以它证明的是「脚本 OOM 不伤宿主」这个隔离不变量，
// 但**证明不了 flag 真的传了** —— 实测把 flag 删掉，该测试仍然 PASS。
// 这条断言对「flag 丢失」这个变异有牙齿。
func TestCommandCarriesMaxOldSpaceFlag(t *testing.T) {
	e := newTestExecutor(t)
	e.opt.MaxOldSpaceMB = 123
	cmd := e.command(NodeInfo{Path: "/usr/bin/node"})
	if !strings.Contains(cmd, "--max-old-space-size=123") {
		t.Fatalf("command = %q, want it to carry --max-old-space-size=123", cmd)
	}

	// <=0 表示「用默认值」而非「不限制」（见 maxOldSpaceMB 契约），
	// 故缺省也必须带flag，且带的是默认值 —— 否则就是「无上限」，V8 崩溃点不可控。
	e2 := newTestExecutor(t)
	e2.opt.MaxOldSpaceMB = 0
	if cmd2 := e2.command(NodeInfo{Path: "/usr/bin/node"}); !strings.Contains(cmd2, "max-old-space-size=512") {
		t.Fatalf("command = %q, want the default heap limit to be applied", cmd2)
	}
}

// ---- 分帧：直写 fd1/fd2 不得吞掉 result 帧（评审发现 7）----

// TestSplitFramesToleratesStrayLines 非协议行出现在**任意位置**时，结果帧都必须被收走。
//
// 回归背景：初版是「首行不是帧就停止扫描」，于是一行 fs.writeSync(1, ...) 就能让后面
// 的真 result 帧落进 tail —— 用户的 console 输出全丢，原始协议 JSON 反而泄进模型上下文。
func TestSplitFramesToleratesStrayLines(t *testing.T) {
	text := strings.Join([]string{
		frameSentinel + `{"notify":"hello","version":{"version_major":1,"version_minor":0}}`,
		"stray-direct-write",
		frameSentinel + `{"notify":"result","out":"captured-log\n","bytes":13}`,
		"[exit: 0]",
	}, "\n")

	frames, tail := SplitFrames(text)
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2（中间那行不该终止扫描）: %+v", len(frames), frames)
	}
	if frames[1].Out != "captured-log\n" {
		t.Fatalf("result 帧内容 = %q, want captured-log", frames[1].Out)
	}
	if !strings.Contains(tail, "stray-direct-write") || !strings.Contains(tail, "[exit: 0]") {
		t.Fatalf("tail = %q, want 直写内容与结局注记都在", tail)
	}
	if strings.Contains(tail, "notify") {
		t.Fatalf("协议行混进了 tail（会泄进模型上下文）: %q", tail)
	}
}

// TestScriptDirectFDWriteDoesNotSwallowOutput 端到端：脚本直写 fd1 后再 console.log，
// 用户输出必须完整回到模型，且模型上下文里不得出现裸协议 JSON。
func TestScriptDirectFDWriteDoesNotSwallowOutput(t *testing.T) {
	requireNode(t)
	e := newTestExecutor(t)

	res, err := e.Execute(context.Background(), `
import fs from 'node:fs';
fs.writeSync(1, "stray-direct-write\n");
fs.writeSync(2, "stray-direct-write-stderr\n");
console.log("captured-log");
`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "captured-log") {
		t.Fatalf("console 输出丢了（result 帧被直写行吞掉）:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "stray-direct-write") {
		t.Errorf("直写内容应原样透出给模型:\n%s", res.Text)
	}
	if strings.Contains(res.Text, `"notify"`) {
		t.Fatalf("裸协议 JSON 泄进了模型上下文:\n%s", res.Text)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestVerifyHandshakeAcceptsSentinel 纯函数必须同时接受带哨兵（线上真实形状）与裸 JSON
// （旧帧/单测形状），避免协议形状演进时把校验写成只认一种。
func TestVerifyHandshakeAcceptsSentinel(t *testing.T) {
	bare := `{"notify":"hello","version":{"version_major":1,"version_minor":0}}`
	if _, err := VerifyHandshake(bare, protoMajor); err != nil {
		t.Fatalf("裸 JSON 应通过: %v", err)
	}
	if _, err := VerifyHandshake(frameSentinel+bare, protoMajor); err != nil {
		t.Fatalf("带哨兵应通过: %v", err)
	}
	if _, err := VerifyHandshake("just-a-log-line", protoMajor); err == nil {
		t.Fatal("非帧行必须报握手缺失")
	}
}
