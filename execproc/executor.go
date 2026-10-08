// Package execproc codemode 的沙箱执行层：把模型写的一段 JavaScript 跑在
// **独立子进程**里，把结果收回宿主。
//
// # 一次执行长什么样
//
//	宿主 ──sandbox.Run(ExecSpec{Stdin: 脚本源码})──> node --import <prelude> - -
//	握手：子进程先发 hello（stdjson.CheckHello 校验 major）
//	执行：stdin 整条是模型写的脚本（顶层 await 合法，因为按 ESM 评估）
//	收尾：退出时子进程补发 result 帧（用户 stdout+stderr 合并在内）
//	超时/取消：sandbox 的三层 kill 预算兜底，返回部分输出 + 明示结局
//
// # 三个关键取舍（Phase 1 定稿，改动前先读）
//
//  1. **一次调用一个进程**（不是常驻 worker 池）。设计文档 §14 要求「常驻
//     worker + 空闲回收」来摊薄 Node 30–80ms 的冷启动，但那一档必须建立在
//     **常驻进程 + 请求/响应协议**之上（宿主向已活着的进程发 "run 这段脚本"），
//     而本包的形态是「把脚本经 stdin 喂进去」—— 进程在读到 EOF 时必然退出。
//     二者不是同一个设计：常驻方案里脚本要进别的通道（临时文件/第二条管道），
//     那样就绕开了「脚本不落盘」这一属性（落盘 = 用户可读的脚本副本进了
//     ~/.go-code 或 /tmp，是凭据面的一部分）。
//     故 Phase 1 明确走**一次一进程**，常驻池留给 Phase 2（有真实调用量与冷启动
//     数据之后再优化，且它需要 scripts 走第二条通道 —— 那是一次独立的改动）。
//
//  2. **prelude 经 --import 注入，脚本整条走 stdin**。见 prelude.js 头注：
//     拼接 prelude 会让堆栈行号整体偏移，模型无法自我修正；--import 是独立
//     模块，加载完再评估 stdin 模块，行号与用户脚本逐行对齐（实测已验）。
//
//  3. **超时不重试、不复用进程**。一个进程只服务一次脚本，不存在「上一条脚本
//     污染下一次」的窗口；沙箱后端（NoSandbox/Seatbelt）每 Run 都是新进程组，
//     三层 kill 预算天然覆盖每次执行。
//
// # 为什么不自己 exec.Command
//
// 必须经 sandbox.Sandbox：那条路上有 2026-09-18 C8 生产事故（602s 挂起）换来的
// 三层 kill 预算（waitDelayAfterKill=3s / killWaitBudget=4s / killEscalationWait=2s，
// 见 sandbox/sandbox.go 文件头）与 isolatedEnv 环境白名单。自己 exec 就会同时丢掉
// 两者：超时管不住一个「自己已退出、但管道被孙进程持有」的等待，脚本也能读到宿主
// 的 ANTHROPIC_API_KEY。
package execproc

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/runtime/stdjson"
	"github.com/seven7628/hai-harness/sandbox"
)

//go:embed prelude.js
var preludeJS string

// 协议版本：宿主与 prelude.js 共同遵守。major 变了 = 协议形状变了，两端必须同版本。
//
// 与 computer.ProtoMajor 各自独立：两者的帧形状与命令面不同，绑在一起会让
// 桌面 helper 的一次协议变更连带要求 codemode 沙箱同步发版。
const (
	protoMajor = 1
	protoMinor = 0
)

// 默认堆上限（MB）。V8 系不能用 RLIMIT_AS（启动即预留大块虚拟地址空间，
// 设了就是「启动即失败或随机 OOM abort」—— 见设计文档 §14 评审 B8），只能用堆参数。
const defaultMaxOldSpaceMB = 512

// DefaultTimeout 单次脚本缺省时限。
//
// 30s 对齐设计文档 §14.2 的墙钟建议值（低于引擎 defaultToolTimeout=5min，
// 使「脚本超时报错」先于「工具超时被引擎杀掉」发生 —— 后者会给模型一个更烂的
// 错误形态）。调用方可经 Opts.Timeout 覆盖。
const DefaultTimeout = 30 * time.Second

// preludeDataURL prelude 的 data: URL（base64 承载）。
//
// 用 base64 而非 percent-encoding：prelude 里有大量 `/`、引号、换行，
// percent-encoding 后的命令行长 3–4 倍且必然要再做一次 shell 转义；
// base64 是 URL-safe 字母表，转义面只剩「整段单引号包裹」。
//
// 这条字符串要进 sandbox.ExecSpec.Command（即经 `sh -c` 解析），所以整个
// data: URL 必须是**单引号包裹**的，且 base64 字母表不含单引号 —— 天然无注入面
// （见 newCommand 的注释与 execproc_test.go 的 TestCommandShellSafe）。
func preludeDataURL() string {
	return "data:text/javascript;base64," + base64.StdEncoding.EncodeToString([]byte(preludeJS))
}

// Executor 在沙箱子进程里执行 JavaScript 脚本。
//
// 并发安全：脚本内可用 Promise.all 并发调用宿主，多个 Executor 调用可并行，
// 故惰性解析解释器与共享状态全部受锁保护（Phase 0 教训 2）。
type Executor struct {
	sbx sandbox.Sandbox      // nil = sandbox.NoSandbox（调用方通常注入与 bash 同一个实例）
	nod *runtime.NodeRuntime // nil = 默认探测器
	opt Opts

	mu   sync.Mutex
	memo NodeInfo // 惰性探测结果（探测在锁内做，见 NodeRuntime.Ensure）
}

// Opts 执行器配置（零值即生产默认）。
type Opts struct {
	// Timeout 单次脚本缺省时限（<=0 → DefaultTimeout）。
	Timeout time.Duration
	// MaxOldSpaceMB V8 堆上限（MB；<=0 → defaultMaxOldSpaceMB）。
	// 只对 Node 生效（Bun 无同名参数）。
	MaxOldSpaceMB int
	// Cwd 脚本工作目录（空 = agent 进程默认目录；调用方通常传工作区根，
	// 与 bash / run_python 同锚）。
	Cwd string
}

// NodeInfo 探测到的解释器信息（Executor 内部缓存 + 供诊断）。
type NodeInfo struct {
	Path    string
	Version string
	Major   int
}

// New 构造执行器。sbx 为 nil 时按 sandbox.NoSandbox 执行（与 bash 工具同约定）。
func New(sbx sandbox.Sandbox, nod *runtime.NodeRuntime) *Executor {
	return &Executor{sbx: sbx, nod: nod}
}

// RunResult 一次脚本执行的结局（对齐 bash 工具的结构化结局字段）。
type RunResult struct {
	// Text **给模型看**的文本：脚本输出 + 结局首行（超时/取消/非零退出/脚本错误）。
	// 协议帧本身不进这里（宿主与沙箱的私事，见 SplitFrames 注释）。
	Text string
	// Raw sandbox 返回的原始合并输出（含协议帧），供诊断与测试断言。
	Raw string
	// ExitCode 进程退出码（-1 = 未正常退出：超时/取消/被杀）。
	ExitCode int
	// TimedOut 时限到点被杀。
	TimedOut bool
	// Canceled 父 ctx 被取消（用户中断/会话关闭）—— 与超时**分开**：
	// 两者对模型是不同的事实（前者不是脚本慢，也不该建议「调大 timeout」）。
	Canceled bool
	// Timeout 本次实际使用的时限（<=0/-1 = 不限时）。仅用于超时文案的措辞。
	Timeout time.Duration
	// Hello 子进程自报的运行时版本（握手成功时非零；失败时零值）。
	Hello stdjson.Version
}

// Execute 执行一段 JavaScript，返回合并输出与结构化结局。
//
// # 语义对齐 bash 工具（刻意逐条对齐，不要各自发明）
//
//   - **非零退出不是 error**：脚本抛异常、语法错误、process.exit(1) 都返回
//     (结果, nil)，输出与原因并入 Text，模型看完自行修正；
//   - **超时返回部分输出且首行必须声明**：与 bash 的 `[TIMEOUT after … — the
//     output below is PARTIAL]` 同一形态。否则模型会把被截断的中间结果当成完整
//     结果继续推理（本仓实测 130 次的教训，见 tools/builtin/bash.go）；
//   - **超时与取消分开**：取消不写「调大 timeout」的建议（用户中断不是脚本慢）。
//
// error 只留给「根本没跑起来」（Node 不可用、sandbox 起不了进程、握手不兼容）：
// 那类调用方应当把它当成**工具不可用**上报，而不是「脚本写错了」。
func (e *Executor) Execute(ctx context.Context, script string) (RunResult, error) {
	if strings.TrimSpace(script) == "" {
		return RunResult{}, errors.New("codemode: script is empty (nothing to execute)")
	}
	node, err := e.node(ctx)
	if err != nil {
		return RunResult{}, err
	}
	sbx := e.sbx
	if sbx == nil {
		sbx = sandbox.NoSandbox{}
	}

	var exitCode int
	var timedOut, canceled bool
	to := e.timeout()
	out, runErr := sbx.Run(ctx, sandbox.ExecSpec{
		Command:  e.command(node),
		Cwd:      e.opt.Cwd,
		Timeout:  to,
		Stdin:    strings.NewReader(script),
		ExitCode: &exitCode,
		TimedOut: &timedOut,
		Canceled: &canceled,
	})
	res := RunResult{ExitCode: exitCode, TimedOut: timedOut, Canceled: canceled, Timeout: to}
	if runErr != nil {
		// sandbox 只在「进程根本没起来」时返回 error（sh 无法启动、Start 失败）；
		// 此时 runExec 不回填 ExitCode，会是零值 0 —— 必须纠正，否则调用方会
		// 把「没跑起来」当成「跑完了且成功」。
		res.ExitCode = -1
		res.Raw = out
		return res, runErr
	}
	res.Raw = out
	frames, tail := SplitFrames(out)
	res.Hello, res.Text = assemble(frames, tail, res)

	// 握手校验放在 assemble **之后**而非之前：SplitFrames 已把帧与 sandbox 注记
	// 分开，这里读第一帧的版本号做 major 比对（VerifyHandshake 是纯函数）。
	//
	// 为什么握手不兼容是 **error** 而非「并入文本」：它不是脚本写错了，模型无论
	// 怎么改脚本都不会变好 —— 继续重试只是浪费一整轮工具调用。报 error 让调用方
	// 把整个 codemode 能力标为不可用（对齐 run_python 遇 Ensure 失败的处理）。
	if len(frames) > 0 && frames[0].Notify == notifyHello {
		if _, err := VerifyHandshake(mustMarshalLine(frames[0]), protoMajor); err != nil {
			return res, err
		}
	}
	return res, nil
}

// mustMarshalLine 把帧重新编码成一行**带哨兵前缀**的协议行（喂给纯函数 VerifyHandshake）。
//
// 绕这一圈而不是直接比字段：VerifyHandshake 需要**行**作为输入（它是 stdjson
// 解码层的测试面），而这里手上已是解码后的结构。重新编码的成本可忽略
// （每执行一次一次 marshal），换来握手校验只有一份实现（纯函数可单测）；
// 顺带让校验走的是**线上真实帧形状**（含哨兵），而不是另一种形状。
func mustMarshalLine(f Frame) string {
	b, err := json.Marshal(f)
	if err != nil {
		// Frame 全是基础类型 + string，marshal 不可能失败；真失败就退化成只有哨兵的空行，
		// 让 VerifyHandshake 报「没有握手」而不是 panic。
		return frameSentinel
	}
	return frameSentinel + string(b)
}

// frameParts 一帧流里「给模型看」的三样东西（hello 版本 / 用户输出 / 脚本错误）。
type frameParts struct {
	hello     stdjson.Version
	out       string
	scriptErr string
	sawHello  bool
}

// collectFrames 从帧流里抽取给模型看的内容（唯一实现）。
//
// 独立成函数是为了让 assemble（Execute）与 ExecuteScript 的 OnOutput 共用同一份口径：
// 两处各写一遍解帧逻辑，迟早会出现「同一个帧在两条路径上被解读成不同的东西」。
func collectFrames(frames []Frame) frameParts {
	var p frameParts
	for _, f := range frames {
		switch f.Notify {
		case notifyHello:
			p.hello, p.sawHello = f.Version, true
		case notifyResult:
			if f.Out != "" {
				if p.out != "" {
					p.out += "\n"
				}
				p.out += f.Out
			}
		case notifyError:
			if p.scriptErr == "" {
				p.scriptErr = strings.TrimSpace(f.Msg)
			}
		}
	}
	return p
}

// assemble 把协议帧 + sandbox 尾巴组装成给模型的文本。
//
// 为什么**不**在超时时保留部分输出之外还强调什么：形态逐字对齐 bash 工具，
// 这样模型在 bash 与 codemode 两个工具上看到的是同一套「首行声明 + 后续输出」
// 的读法，不需要学第二种。
func assemble(frames []Frame, tail string, res RunResult) (stdjson.Version, string) {
	p := collectFrames(frames)
	hello, out, scriptErr := p.hello, p.out, p.scriptErr
	// 没握手成功：prelude 都没起来（node 崩溃/被杀/旗标不被支持）。此时**不**把
	// 整段 raw 灌给模型 —— 那可能是 Node 的 V8 崩溃栈，噪声远大于信息；
	// 但也不能什么都不说（模型会以为脚本静默失败），故给一句可行动的判断。
	if !p.sawHello {
		return hello, "the sandbox runtime produced no handshake — the script never started.\n" +
			"tail: " + truncate(tail, 500)
	}

	var b strings.Builder
	if note := outcomeNote(res, out == ""); note != "" {
		b.WriteString(note + "\n")
	}
	if out != "" {
		b.WriteString(strings.TrimRight(out, "\n"))
		b.WriteString("\n")
	}
	if scriptErr != "" {
		b.WriteString("[script error]\n" + scriptErr + "\n")
	}
	// sandbox 的结局注记（[exit: N] / kill 说明）已含在 tail 里，只在有话说时附上。
	if t := strings.TrimSpace(tail); t != "" {
		b.WriteString(t + "\n")
	}
	return hello, strings.TrimRight(b.String(), "\n")
}

// e_timeoutText 时限文案（与 bash 同口径：<=0/-1 表示不限时，此时只可能是取消）。
func e_timeoutText(res RunResult) string {
	if res.Timeout > 0 {
		return res.Timeout.String()
	}
	return "the configured limit"
}

// outcomeNote 结局首行（超时/取消）。形态与 bash 工具对齐 —— 模型在两个工具上看到
// 同一套「首行声明 + 后续输出」的读法，不必学第二种。
//
// 但与 bash 有一处**硬差异**，措辞必须分开：bash 的输出由 sandbox 管道边跑边收，
// 超时被杀也能拿到已产生的部分输出；本执行层的用户输出是在沙箱进程内缓冲的，
// 只在脚本正常收尾时随 result 帧回传（见 prelude.js）。被 kill 的脚本因此
// **一条也拿不回来** —— 此时说「下面的输出是部分结果」而下面是空的，模型会把空
// 当成真结果继续推理，比不声明更坏（本仓实测 130 次的教训，见 bash 工具的同类注释）。
//
// noOutput 为 true 时改用「输出不可回收」措辞，并说明为什么（脚本输出只在脚本
// 跑完时才回传），让模型知道这是**机制**限制、不是脚本没输出。
func outcomeNote(res RunResult, noOutput bool) string {
	switch {
	case res.Canceled && noOutput:
		return "[CANCELLED — the script was interrupted before it could report output; " +
			"script output is only returned when the script finishes, so nothing could be recovered]"
	case res.Canceled:
		return "[CANCELLED — the script was interrupted before finishing; " +
			"the output below is PARTIAL]"
	case res.TimedOut && noOutput:
		return "[TIMEOUT after " + e_timeoutText(res) + " — the script was killed before it could " +
			"report output; script output is only returned when the script finishes, " +
			"so nothing could be recovered]"
	case res.TimedOut:
		return "[TIMEOUT after " + e_timeoutText(res) + " — the script was killed and the " +
			"output below is PARTIAL]"
	}
	return ""
}

// truncate 按字节截断并标注省略量（给模型看，必须说明白丢了多少）。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("%s… [+%d bytes omitted]", s[:max], len(s)-max)
}

// timeout 解析本次时限（照搬 bash 的三态，见 tools/builtin/bash.go Call：
// 后台不限时 / 跟随 ctx / 兜底默认）。
//
// 执行层**不自叠 WithTimeout**：若任务被 promote 为后台，引擎的 liftableTimeout
// 已解除时限（ctx 标记 Unlimited），本层自叠的快照定时器反而会挡住「后台不限时」
// 的语义 —— 这是 bash 工具注释里记下的教训，编排脚本同理（长时间跑的工具编排
// 是典型场景）。
func (e *Executor) timeout() time.Duration {
	switch {
	case e.opt.Timeout < 0:
		return -1
	case e.opt.Timeout == 0:
		return DefaultTimeout
	default:
		return e.opt.Timeout
	}
}

// command 拼命令行。
//
// 形如：node --max-old-space-size=512 --import 'data:...' --input-type=module -
//
// 三个标志各自不可省：
//   - `--input-type=module`：stdin 里的脚本按 **ESM** 评估。CommonJS 下 `await`
//     是语法错误，而顶层 await 是编排脚本的核心（`await Promise.all([...])`）。
//   - `-`：从 stdin 读脚本源码。这是本包不落盘的根据（见包注释取舍 1）。
//   - `--import`：注入 prelude（见 prelude.js 头注）。
func (e *Executor) command(n NodeInfo) string {
	return e.nodePrefix(n, e.maxOldSpaceMB()) +
		" --import " + shellQuote(preludeDataURL()) +
		" --input-type=module -"
}

// nodePrefix 命令行的公共前缀：解释器路径 + 堆上限旗标。
//
// 抽出来给两条执行路径共用（Execute 走 stdin 脚本；ExecuteScript 走临时文件 + 桥接）：
// 堆上限与路径转义这两件事必须只有一份实现，否则「哪条路径少了一个 --max-old-space-size」
// 会变成一条静默的隔离降级（对照 TestCommandCarriesMaxOldSpaceFlag 的变异说明）。
func (e *Executor) nodePrefix(n NodeInfo, mb int) string {
	var b strings.Builder
	b.WriteString(shellQuote(n.Path))
	if mb > 0 {
		// --max-old-space-size 是 V8 的**堆**上限：脚本 OOM 时 Node 自己 abort，
		// 宿主存活，且退出码与 stderr 文本足以让模型看懂是「分配超限」。
		fmt.Fprintf(&b, " --max-old-space-size=%d", mb)
	}
	return b.String()
}

// maxOldSpaceMB 解析堆上限（MB）。
func (e *Executor) maxOldSpaceMB() int {
	if e.opt.MaxOldSpaceMB > 0 {
		return e.opt.MaxOldSpaceMB
	}
	return defaultMaxOldSpaceMB
}

// node 惰性探测解释器（结果缓存在 Executor 上）。
//
// 探测在 NodeRuntime 自己的锁内做；本层锁只保护 memo 的读/写 ——
// 双重加锁但不会死锁（无嵌套反向顺序）。缓存的理由：Execute 可能被脚本内的
// Promise.all 并发高频调用，每次 spawn 一次 `node --version` 是白付的 ~50ms。
func (e *Executor) node(ctx context.Context) (NodeInfo, error) {
	e.mu.Lock()
	cached := e.memo
	e.mu.Unlock()
	if cached.Path != "" {
		return cached, nil
	}
	r := e.nod
	if r == nil {
		r = runtime.NewNodeRuntime(runtime.NodeConfig{})
	}
	n, err := r.Ensure(ctx)
	if err != nil {
		return NodeInfo{}, err
	}
	info := NodeInfo{Path: n.Path, Version: n.Version, Major: n.Major}
	e.mu.Lock()
	e.memo = info
	e.mu.Unlock()
	return info, nil
}

// shellQuote 单引号包裹（命令要经 `sh -c`）。
//
// 单引号内除单引号本身外全部字面，故只需处理一种转义（bash 工具
// computer/stdio.go 的 shellQuote 同法）。执行失败时脚本会看到被改坏的命令行 ——
// 所以调用方给的每段文本要么不含单引号（base64/路径），要么先经过这里。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---- 协议帧 ----
// 帧形状的唯一事实源是 prelude.js（改一处必须改两处）。这里用单一 Frame 类型
// （而非 hello/result/error 三个类型）—— 三者的 notify 字段互斥，合成一个结构体
// 的代价（未用字段留空）远小于三处重复的解码样板；SplitFrames 只认 notify 白名单。

// notifyKind 帧的合法 notify 值。出现白名单外的即协议漂移（prelude 发了宿主不认识的
// 东西）。call 是 Phase 2 桥接（prelude_bridge.js）新增的一类：沙箱 → 宿主的工具调用
// 请求；它只出现在带桥接的 ExecuteScript 路径上，Execute 不会收到。
const (
	notifyHello  = "hello"
	notifyResult = "result"
	notifyError  = "error"
	notifyCall   = "call"
)

// notify 宿主 → 沙箱的三条出站消息（executeScript 的桥接通道）。它们**不是**帧白名单
// 的一员：方向相反、且只走 stdin 的 NDJSON，不进 SplitFrames/knownNotify。
const (
	notifyInit     = "init"
	notifyReply    = "reply"
	notifyShutdown = "shutdown"
)

// VerifyHandshake 校验一次执行产出的**第一帧**是否为兼容握手。
//
// # 为什么只能拿到「合并文本」而不是结构化帧
//
// 执行经 sandbox.Sandbox，它把 stdout+stderr 合并成一个字符串返回（runExec 的
// 契约，bash 工具同款）—— 没有「逐帧读」这条通道。协议行因此带哨兵前缀
// （frameSentinel，见 prelude.js）：宿主据此把「协议行」与「用户输出/sandbox 注记」
// 分开，而不是靠「首行不是帧就停」那种一戳就破的启发式。
//
// 入口收的是**第一帧那一行**（execproc 自己 marshal 回来的），哨兵可有可无 ——
// 本函数是纯函数，测试直接喂裸 JSON 也要能判版本。
//
// 独立成纯函数（无 I/O、可单测）：握手失败与帧错乱这两条路径在真实执行里极难
// 构造，而它们恰是最该被钉住的两条。
func VerifyHandshake(line string, wantMajor int) (stdjson.Version, error) {
	var fr Frame
	if err := stdjson.NewReader(strings.NewReader(stripFrameSentinel(line) + "\n")).Next(&fr); err != nil {
		if stdjson.LineTooLongErr(err) {
			return stdjson.Version{}, fmt.Errorf(
				"sandbox runtime produced a first line larger than %d bytes before the handshake "+
					"(something other than the protocol banner went to stdout)", stdjson.MaxLine)
		}
		return stdjson.Version{}, fmt.Errorf(
			"sandbox runtime did not send a protocol handshake on stdout (first line: %s)",
			quoteFirstLine(line))
	}
	if fr.Notify != notifyHello {
		return fr.Version, fmt.Errorf(
			"unexpected first message %q from the sandbox runtime (expected a hello handshake)",
			fr.Notify)
	}
	if fr.Version.Major != wantMajor {
		return fr.Version, stdjson.CheckHello(
			stdjson.HelloResult{Version: fr.Version},
			stdjson.Version{Major: wantMajor})
	}
	return fr.Version, nil
}

// frameSentinel 协议行的哨兵前缀（ASCII RS）。**唯一事实源是 prelude.js**，
// 两边改一处必须改另一处（协议版本号 protoMajor 就是这个耦合的兜底检查）。
//
// 为什么不是「首行不是帧就停」：脚本直写 fd1/fd2 会插进任意位置，那种解析会
// 把后面的真 result 帧一起当成用户文本（输出全丢 + 原始 JSON 泄进模型上下文）。
const frameSentinel = "\x1e"

// stripFrameSentinel 去掉协议行哨兵（无哨兵时原样返回 —— 便于纯函数单测与旧帧兼容）。
func stripFrameSentinel(line string) string {
	return strings.TrimPrefix(line, frameSentinel)
}

// SplitFrames 从合并输出里分出协议帧与**其余全部文本**（sandbox 注记 + 用户直写）。
//
// 逐行**全量**扫描：带哨兵且能解成已知 notify 的行收作帧，其余行按原序留在 tail。
// 不因一个非帧行就停止（那是初版实现，见 frameSentinel 注释里的一戳就破）。
//
// 代价与取舍：tail 与帧的相对顺序信息会丢（输出的相对顺序可能被打散）——
// 故给模型的文本统一按「帧里的用户输出 → 脚本错误 → tail」重组（见 assemble）。
// 直写 fd 本就绕过了输出截获，其内容仍需让模型看见（原样留在 tail），
// 但不能让它把 result 帧吞掉。
func SplitFrames(text string) (frames []Frame, tail string) {
	var tailLines []string
	for _, ln := range strings.Split(text, "\n") {
		if f, ok := parseFrameLine(ln); ok {
			frames = append(frames, f)
			continue
		}
		tailLines = append(tailLines, ln)
	}
	return frames, strings.TrimSpace(strings.Join(tailLines, "\n"))
}

// parseFrameLine 解析一行协议帧：必须带哨兵前缀，且 notify 是 prelude 会发的三种之一。
//
// 哨兵 + notify 白名单双重要求的结果：用户输出（哪怕是合法 JSON、哪怕自己写了
// notify 字段）永远不可能被误判成协议帧 —— 那些内容都在 result 帧的 out 字段里
// （JSON 转义过），或经直写留在 tail。
func parseFrameLine(line string) (Frame, bool) {
	if !strings.HasPrefix(line, frameSentinel) {
		return Frame{}, false
	}
	var f Frame
	if err := stdjson.NewReader(strings.NewReader(stripFrameSentinel(line) + "\n")).Next(&f); err != nil {
		return Frame{}, false
	}
	if !knownNotify(f.Notify) {
		return Frame{}, false
	}
	return f, true
}

// knownNotify notify 是否是 prelude 会发的合法值之一。
//
// 出现白名单外的 notify 即协议漂移（prelude 发了宿主不认识的东西）—— 此时**不**收该帧，
// 交给 tail 原样透出给模型看（比静默丢弃更利于排查），也不会让 assemble 误判「有握手」。
func knownNotify(n string) bool {
	return n == notifyHello || n == notifyResult || n == notifyError || n == notifyCall
}

// Frame 一帧协议消息（prelude 只会发这四种 notify）。用单一类型而非四个结构体：四者的
// notify 互斥，合成一个的代价（未用字段留空）远小于四处重复的解码样板。
type Frame struct {
	Notify string `json:"notify"`
	// hello
	Version stdjson.Version `json:"version"`
	PID     int             `json:"pid,omitempty"`
	// result
	Out   string `json:"out,omitempty"`
	Bytes int    `json:"bytes,omitempty"`
	// error
	Kind string `json:"kind,omitempty"`
	Msg  string `json:"msg,omitempty"`
	// call（Phase 2 桥接）
	// Id 是沙箱生成的关联锚：回执必须回抄它（见 script.go 的 bridgeReply）。
	Id   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

// quoteFirstLine 取第一行并截断（错误文案用，避免把整段输出灌进一条错误）。
//
// 用 %q 引用：子进程首行可能含控制字符/换行，直接内联会把错误文本结构搞乱，
// 而这类文本是要给模型看的（它会照着这句话自我修正）。
func quoteFirstLine(s string) string {
	line := s
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	const max = 200
	if len(line) > max {
		return fmt.Sprintf("%q… (+%d bytes)", line[:max], len(line)-max)
	}
	return fmt.Sprintf("%q", line)
}
