// execproc/script.go —— codemode 的**双向传输层**：宿主 ↔ 沙箱的 NDJSON 桥。
//
// 与 executor.go（一次性、单向：脚本进 stdin，输出跑完才收回）的关系是「多一条边」：
// 脚本仍只进不出，但**多了宿主 → 沙箱的回执边**，于是脚本里的 `await tools.x()`
// 能在运行期间真的往返宿主一次。
//
// # 通道划分（Phase 2 冻结，见 IMPLEMENTATION-SPEC §7.1-1 / §7.2）
//
//	宿主 → 沙箱：stdin，一行一条 JSON —— init（首条，**含脚本正文**）/ reply / shutdown
//	沙箱 → 宿主：fd1，带哨兵前缀的帧 —— hello / call（新增）/ result / error
//
// 脚本正文在 **init 里**（init.script，Phase 1-A2 起）：临时文件那条路（Phase 1-A 把
// 脚本写成 `script.mjs` 当 ESM 入口模块跑）已经拆掉 —— 顶层 `return` 在入口模块里是
// SyntaxError，而模型契约承诺它；顺带把「脚本不落盘」这条 Phase 1 用 stdin 换来的性质
// 收回来（脚本可能含密钥/路径，属凭据面）。
//
// # 为什么 fd1 要重定向到 spool 文件（最容易被误读的一处）
//
// sandbox.Sandbox.Run 的契约是「**跑完**才把 stdout+stderr 合并成一个字符串返回」
// （sandbox/sandbox.go 的 Run，且那份实现不允许新增面）。请求/应答却必须在脚本
// **运行期间**完成，故本层把子进程的 fd1/fd2 经命令行重定向到一个宿主可 tail 的
// 文件（`> spool 2>&1`，见 scriptCommand）：
//
//   - 对沙箱里的 prelude 而言什么都没变：帧仍写在 fd1 上、仍带哨兵、仍由 SplitFrames
//     解析（帧形状零变化），用户直写 fd1/fd2 也仍落在同一份合并输出里；
//   - 对宿主而言多了一个「增量读」的面，且它是**随机访问的普通文件**：不新增监听面
//     （loopback socket 要开监听、fd3 要改 sandbox 的命令面），也不会像管道那样被逃逸
//     后代握住写端（runExec 的 WaitDelay 事故机制，见 sandbox 文件头的 C8 事故）。
//
// # 收尾与「谁来关 stdin」
//
// os/exec 的 stdin 拷贝协程阻塞在**我们的** io.Pipe 上：子进程退出时 Wait 只会再等
// waitDelayAfterKill(3s) 就强制关管道并返回 ErrWaitDelay —— 不主动关的话每次执行都
// 白付 3s，且 runExec 会在给模型的文本里印一句「输出可能被进程组外的后代截断」
// （此处并不是真的）。故终帧（result/error，prelude.js 只在 'exit' 上发）一到就关
// stdin；超时/取消路径在 ctx 结束时提前关（见 bridgeHost.shutdown 的三个触发点）。
//
// # 错误面（与 Execute 同契约）
//
// 脚本抛错 / 非零退出 / 超时 / 取消**都不是** error：全部并入 RunResult.Text，模型看完
// 自行修正。error 只留给「根本没跑起来 + 握手不兼容」（node 不可用、临时目录写不出、
// protoMajor 不匹配、Init 不是对象）—— 那类问题重试多少次都不会变好，调用方应把
// codemode 标为不可用。
package execproc

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seven7628/hai-harness/sandbox"
)

//go:embed prelude_bridge.js
var bridgeJS string

// bridgeDataURL 桥接 prelude 的 data: URL（理由与 preludeDataURL 逐条相同：base64
// 字母表不含单引号，整段单引号包裹即无注入面）。
func bridgeDataURL() string {
	return "data:text/javascript;base64," + base64.StdEncoding.EncodeToString([]byte(bridgeJS))
}

// ---- 冻结 API（IMPLEMENTATION-SPEC §7.2，签名不得改）----

// ScriptCall 一次沙箱发起（沙箱 → 宿主）的工具调用。
type ScriptCall struct {
	Id   string          // 双向关联锚（沙箱生成，会话内唯一）
	Name string          // 工具名（已归一化，见 descriptions 的 normalize）
	Args json.RawMessage // 对象参数原文

	// Index 派发序号：读帧协程按**帧到达顺序**单调递增地发号（见 dispatch）。
	//
	// 为什么要有它（Phase 2 复核 F3）：帧一到就各起一个 goroutine（脚本里 Promise.all
	// 要真并发），于是**谁先到 OnCall 取决于调度**，而不是脚本里的先后。编排方要恢复
	// 「脚本先发起先执行」的唯一确定信息就是这个按帧序发的号 —— 靠别的办法（按时延猜
	// 前驱是否快到了）都是启发式。Id 是**脚本**顺序的锚，这里刻意不复用它：两者在
	// 「帧到达顺序」这一维上可能不同，而 Index 正是那一维的权威。
	Index int64
}

// ScriptResult 一次工具调用的回执（宿主 → 沙箱）。
type ScriptResult struct {
	Value   json.RawMessage // 回给脚本的值（结构化优先，否则 JSON 字符串）
	IsError bool            // true → 沙箱内 reject（脚本可 catch）
}

// ScriptOpts 一次带桥接的脚本执行。
type ScriptOpts struct {
	Script        string
	Cwd           string
	Timeout       time.Duration // <0 = 不限时（宿主管墙钟，见 7.1-2）
	MaxOldSpaceMB int
	Init          json.RawMessage // 注入沙箱的初始化载荷（工具目录/store），由 codemode 生成
	// Scaffold 宿主注入的 JS 片段（可选）：在用户脚本**之前**求值，签名为 (ctx)，
	// ctx = { init, hostCall }（init = 上面那个载荷的原文，hostCall(name, args) 走与
	// OnCall 同一条 call/reply 路径）。Wave 2 用它装 text/image/store/load/searchTools/
	// describeTool 这些全局；传输层自己只提供 exit()（见 prelude_bridge.js）。
	//
	// 契约（三条都与「怎么求值」有关，写在宿主侧是因为写 scaffold 的是宿主）：
	//   - 本片段是**函数体**（不是函数表达式）：`(ctx) => {...}` 这种写法只会求值出
	//     一个函数对象、不会被调用，请直接写语句（可含 await）；
	//   - 要用 `globalThis.<name> = …` 定义全局才**对用户脚本可见**（函数体里
	//     `const`/`var` 都是局部的）；用户脚本与它同 realm，globalThis 上的键彼此可见；
	//   - 与脚本一样**永不落盘**（随 init 走 stdin），报错即收尾（kind=scaffold，
	//     用户脚本不执行）—— scaffold 装不上全局时硬跑只会把宿主的 bug 伪装成
	//     「模型写了不存在的函数」。
	Scaffold string
	OnCall   func(ctx context.Context, c ScriptCall) ScriptResult
	OnOutput func(text string) // 可选：脚本输出增量（Phase 2 允许只在结束时给）
}

// Init 载荷形状 —— 宿主侧的唯一权威定义（沙箱侧 prelude_bridge.js 按宽容规则解析它）：
//
//		{"tools": [{"name": "read_file", "rawName": "mcp__dev-radius__read-file"}, "grep"],
//		 "store": {...}}
//
//	  - `tools` 数组的每一项可以是字符串（只有归一化名）或对象（`name` 必填；
//	    `rawName`（别名 `raw`）是**原始名**，用于沙箱里 `tools["mcp__dev-radius__x"]`
//	    那种双键调用）。写成 map（`{"read_file": {...}}`）也认，键即归一化名。
//	  - 名字归一化与撞名解决在 **codemode 侧**（datasheet §15.1），桥接只认这里给的
//	    最终名：两处各归一化一次会让撞名判定失真。归一化名撞名时沙箱内**先到先得**
//	    （对齐 pi）：后到的仍可经自己的原始名调用，但不顶掉先到的键 —— 静默覆盖会让
//	    脚本调到另一个工具，是最难查的一类错。
//	  - 其余键（store 等）本波不解释，原样挂在沙箱的 `globalThis.__codemode.init` 上，
//	    由 Wave 2 的 tools/builtin/codemode/protocol.go 决定怎么用。
//
// 另有一条**调用方须知**（与上面的形状分开写：一条是「载荷长什么样」，一条是
// 「哪些键不归你」）：**`script` / `scaffold` 是传输字段，调用方别用** —— 宿主会把 ScriptOpts.Script 与 .Scaffold 塞进
// 这条消息的这两个键（源码只能经 stdin 进沙箱，见 ExecuteScript 的生命周期注释），
// 沙箱侧取走后立刻从暴露给脚本的那份 init 上删掉；调用方若自己用了这两个键，会被覆盖。
func initPayload(init json.RawMessage, script, scaffold string) (json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	trimmed := bytes.TrimSpace(init)
	// 空载荷与字面量 null 都按「没有额外载荷」处理（Init 是可选字段，零值即空）。
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, fmt.Errorf("codemode: ScriptOpts.Init 必须是 JSON 对象（脚本与初始化载荷"+
				"同一条消息送出，非对象的载荷没地方挂 script 字段）: %w", err)
		}
	}
	obj["script"] = jsonString(script)
	if scaffold != "" {
		obj["scaffold"] = jsonString(scaffold)
	}
	b, err := json.Marshal(obj)
	if err != nil {
		// map[string]json.RawMessage 里塞的都是合法 JSON 与字符串字面量，编不出来只可能是
		// 调用方给的 RawMessage 本身畸形（json.Marshal 会做一次合法性检查）。
		return nil, fmt.Errorf("codemode: 编码 init 载荷失败（ScriptOpts.Init 里有非法 JSON？）: %w", err)
	}
	return b, nil
}

// ExecuteScript 跑一段带桥接的脚本。契约与 Execute 一致：脚本错误/非零退出不是 error；
// **默认不返回 error**，error 只留给「根本没跑起来 + 握手不兼容」。
//
// 时限三态（opts.Timeout，与 Execute 的 e.timeout() 同一口径）：
//
//	< 0：不限时 —— 只随 ctx 取消，墙钟由宿主（codemode 的 ExecScope）管。这是 codemode
//	     的实际用法（§7.1-2）：只有上层能实现「审批等待期间暂停墙钟」，沙箱侧的固定
//	     deadline 天然做不到。
//	== 0：用本执行器的缺省时限（未配置 Executor 时即 DefaultTimeout）——「0 = 用默认」
//	     与「<0 = 不限」必须分开，否则「没设」会被读成「不限制」。
//	> 0：本次就用它（进 ExecSpec.Timeout）。
//
// 并发：脚本内 `Promise.all` 发起的多个 call 由**每个 call 一个 goroutine** 分派，
// 回执允许乱序（按 id 关联）。本层刻意**不加串行门** —— 串行/独占语义是上层
// （ExecScope）的职责，加在这里会让上层的闸门失效（嵌套调用会绕过它自己的判定）。
//
// 生命周期：临时目录 os.MkdirTemp(0700) 里**只有帧 spool**（0600）—— 脚本正文与
// scaffold 都随 init 走 stdin，永不落盘（Phase 1 用 stdin 换来的这条性质，Phase 2
// 把 stdin 让给桥接通道之后由 init.script 继续承担）；正常/超时/取消/error 四类退出
// 路径都会删掉整个目录（defer）。
func (e *Executor) ExecuteScript(ctx context.Context, opts ScriptOpts) (RunResult, error) {
	if strings.TrimSpace(opts.Script) == "" {
		return RunResult{}, errors.New("codemode: script is empty (nothing to execute)")
	}
	node, err := e.node(ctx)
	if err != nil {
		return RunResult{}, err
	}
	// init 在**建临时目录之前**组装：Init 不是对象这类调用方错误不该留下临时资材。
	init, err := initPayload(opts.Init, opts.Script, opts.Scaffold)
	if err != nil {
		return RunResult{}, err
	}
	files, err := newRunFiles()
	if err != nil {
		return RunResult{}, err
	}
	defer files.remove()
	if onRunFiles != nil {
		onRunFiles(files)
	}

	sbx := e.sbx
	if sbx == nil {
		sbx = sandbox.NoSandbox{}
	}
	to := e.scriptTimeout(opts.Timeout)

	// 取消/超时的统一收口：opts.Timeout > 0 时**宿主侧**也挂一个同样的 deadline。
	// 为什么两处都要：沙箱内部的 deadline 决定 ExecSpec 语义（TimedOut 回填），宿主侧
	// 的 deadline 让「ctx 结束」这个信号真的会到 —— bridgeHost 靠它在收尾时松手（关
	// stdin），否则沙箱自己超时时 runExec 要先白等 waitDelayAfterKill(3s)：stdin 拷贝
	// 协程还挂在我们的管道上（见文件头「谁来关 stdin」）。
	runCtx, cancelRun := ctx, context.CancelFunc(nil)
	if to > 0 {
		runCtx, cancelRun = context.WithTimeout(ctx, to)
		defer cancelRun()
	}

	// 子调用的 ctx：随本次执行结束而取消（沙箱都死了，还在跑的 OnCall 不该继续占资源
	// —— 设计文档 §16.3 的级联取消）；OnCall 实现按自己的策略响应与否。
	callCtx, cancelCalls := context.WithCancel(runCtx)
	defer cancelCalls()

	br := newBridgeHost(callCtx, files.spool, init, opts.OnCall)
	if err := br.start(runCtx); err != nil {
		return RunResult{}, err
	}

	cwd := opts.Cwd
	if cwd == "" {
		cwd = e.opt.Cwd
	}
	var exitCode int
	var timedOut, canceled bool
	out, runErr := sbx.Run(runCtx, sandbox.ExecSpec{
		Command:  e.scriptCommand(node, files, opts.MaxOldSpaceMB),
		Cwd:      cwd,
		Timeout:  to,
		Stdin:    br.stdin(), // stdin 整条让给宿主→沙箱通道（脚本与 scaffold 都走 init）
		ExitCode: &exitCode,
		TimedOut: &timedOut,
		Canceled: &canceled,
	})

	res := RunResult{ExitCode: exitCode, TimedOut: timedOut, Canceled: canceled, Timeout: to}
	cancelCalls()
	raw := br.finish() // 停 tail + 排干剩余帧 + 关 stdin（幂等）
	if out != "" {
		// sandbox 自己的注记（[exit: N] / kill 说明）：与 Execute 同一位置 —— 帧在前、
		// 注记在后，assemble 统一按「帧里的输出 → 脚本错误 → tail」重组。
		raw = strings.TrimRight(raw, "\n") + "\n" + out
	}
	if runErr != nil {
		// 进程根本没起来：Run 不回填 ExitCode（零值 0），纠正成 -1，否则调用方会把
		// 「没跑起来」当成「跑完且成功」（与 Execute 同一处纠正）。
		res.ExitCode = -1
		res.Raw = raw
		return res, runErr
	}
	res.Raw = raw
	// 与 Execute **逐字同一条**组装路径：帧解析、结局首行、超时/取消措辞（§6.5 的
	// 「拿不到输出就不得承诺 PARTIAL」）都由它负责，两条执行路径不会各自漂移。
	frames, tail := SplitFrames(raw)
	parts := collectFrames(frames)
	res.Hello, res.Text = assembleParts(parts, tail, res)
	// 脚本结局值（return / exit(v)）：nil = 没有返回值。Execute 路径不填它 ——
	// 那条路径的脚本没有 return 承诺（见 RunResult.Value 的注释）。
	res.Value = parts.value
	if len(frames) > 0 && frames[0].Notify == notifyHello {
		if _, err := VerifyHandshake(mustMarshalLine(frames[0]), protoMajor); err != nil {
			return res, err
		}
	}
	if opts.OnOutput != nil {
		// Phase 2 允许只在结束时给一次（流式留给常驻 worker）：只给脚本自己产生的
		// 输出，不含结局注记与直写 fd 的 tail —— 那些是宿主侧的诊断信息。
		if parts.out != "" {
			opts.OnOutput(parts.out)
		}
	}
	return res, nil
}

// scriptTimeout 解析本次时限（三态，见 ExecuteScript 的 godoc）。
//
// ==0 复用 e.timeout() 而不是硬写 DefaultTimeout：Execute 的缺省时限同源，两条执行
// 路径的「没设时限」不该给出两个不同的数（未配置 Executor 时二者都落在 DefaultTimeout）。
func (e *Executor) scriptTimeout(t time.Duration) time.Duration {
	switch {
	case t < 0:
		return -1
	case t == 0:
		return e.timeout()
	default:
		return t
	}
}

// scriptCommand 带桥接的命令行。
//
//	形如：node --max-old-space-size=512 --import '<prelude>' --import '<bridge>' \
//	      --eval '' > '/tmp/codemode-run-x/frames.ndjson' 2>&1
//
// 四个部分各自不可省：
//   - 两个 --import：协议端（握手/截获/result 帧）在前、桥接端（init/call/tools/求值）
//     在后 —— 顺序是硬语义，桥接端依赖协议端已就位（它要发 error/result 帧）；
//   - `--eval ”`：**入口只是一个占位的空脚本**。为什么不能省：node 在没有脚本参数
//     且 stdin 不是 TTY 时会把 **stdin 当脚本源码读**，而 stdin 已整条让给桥接通道
//     （init/reply/shutdown），那会把 init 那一行当脚本求值掉。用户脚本由桥接端用
//     `new AsyncFunction` 求值（见 prelude_bridge.js 的「求值形态」），故这里不需要
//     任何入口模块 —— 也就没有 `script.mjs`（脚本永不落盘）；
//   - `> spool 2>&1`：fd1/fd2 重定向到宿主可 tail 的文件（见文件头）。顺序不能反
//     （`2>&1 > file` 会把 stderr 留在原 fd 上），故两半由同一条语句生成，不给调用方
//     拼错的余地。
//
// 脚本正文**不在命令行里**（会经 sh -c 并在进程表里裸奔）：它走 init.script。
func (e *Executor) scriptCommand(n NodeInfo, f runFiles, mb int) string {
	if mb <= 0 {
		mb = e.maxOldSpaceMB()
	}
	var b strings.Builder
	b.WriteString(e.nodePrefix(n, mb))
	b.WriteString(" --import " + shellQuote(preludeDataURL()))
	b.WriteString(" --import " + shellQuote(bridgeDataURL()))
	b.WriteString(" --eval ''")
	b.WriteString(" > " + shellQuote(f.spool) + " 2>&1")
	return b.String()
}

// ---- 临时资材（只有帧 spool 一个文件）----

// runFiles 一次执行的临时资材（都在同一个 0700 目录里）。
//
// 只有一个文件是**设计结果**，不是简化：脚本正文与 scaffold 都随 init 走 stdin
// （Phase 1-A2，见 IMPLEMENTATION-SPEC §7.8-2），临时目录里只剩子进程 fd1/fd2 的
// 重定向目标。0600/0700 的断言（spool 里有工具回执与用户输出）仍由本结构承载。
type runFiles struct {
	dir   string
	spool string // 子进程 fd1/fd2 的重定向目标（0600）
}

// onRunFiles 测试观察点（生产恒 nil）：把两个路径交给它。
//
// 为什么需要这条缝：0600 这条断言只能在**运行期间**成立（跑完就删了），而调用方在
// ExecuteScript 返回后拿不到任何路径 —— 没有这个钩子就只能让沙箱里的脚本「自证」
// 权限（被观察者自报，不是宿主观察）。同 sandbox 包 killProcGroup 的测试注入点。
var onRunFiles func(files runFiles)

// newRunFiles 建临时目录并**先建**帧 spool。
//
// 目录选 /tmp 而**不是** os.TempDir()：Seatbelt 策略里 /tmp 是唯一「可读**且**可写」
// 的位置（file-read* 见 systemReadPaths，file-write* 见 buildPolicy），而 macOS 的
// $TMPDIR 是 /var/folders/... —— 只读不可写，真开着沙箱时帧 spool 会写失败
// （sandbox.isolatedEnv 把子进程的 TMPDIR 钉成 /tmp 也是同一个理由）。/tmp 是 1777，
// 但本目录是 0700，只有本人能进。
func newRunFiles() (runFiles, error) {
	dir, err := os.MkdirTemp("/tmp", "codemode-run-")
	if err != nil {
		return runFiles{}, fmt.Errorf("codemode: 建临时目录失败（帧 spool 要落盘）: %w", err)
	}
	f := runFiles{
		dir:   dir,
		spool: filepath.Join(dir, "frames.ndjson"),
	}
	// spool 由宿主**先建**：sh 的 `>` 只截断不设权限，先建才能钉住 0600（否则按 umask
	// 出生，可能是 0644 —— 里面有工具回执与用户输出）。
	if err := os.WriteFile(f.spool, nil, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return runFiles{}, fmt.Errorf("codemode: 写帧 spool 失败: %w", err)
	}
	return f, nil
}

// remove 删掉整份资材（四类退出路径共用）。删失败不改结局：删不掉的文件在 /tmp 里是
// 遗留物而非正确性问题，调用方无从补救，故这里不把它升级成 error。
func (f runFiles) remove() {
	if f.dir != "" {
		_ = os.RemoveAll(f.dir)
	}
}

// ---- 宿主 → 沙箱：三条出站消息（形状的唯一事实源 = prelude_bridge.js 的 handle）----

// bridgeInit 首条消息：注入初始化载荷 + **脚本正文**（initPayload 组装：opts.Init 原文
// 上多挂 script / scaffold 两个键）。
// 沙箱侧**同步**读它（readSync），读完才求值 scaffold 与用户脚本 —— 没到之前不会求值
// 任何用户代码（否则脚本里的 tools.x 会先于工具表就位）。
type bridgeInit struct {
	Notify string          `json:"notify"` // 恒 "init"
	Init   json.RawMessage `json:"init"`   // initPayload 的产物（恒为对象：至少含 script）
}

// bridgeReply 一次 call 的回执。Id 必须回抄沙箱给的 ScriptCall.Id（按 id 关联，
// 回执允许乱序）。IsError 的调用只填 Error（人类可读消息，脚本侧成为 Error.message）。
type bridgeReply struct {
	Notify string          `json:"notify"`           // 恒 "reply"
	Id     string          `json:"id"`               // 关联锚（= ScriptCall.Id）
	Result json.RawMessage `json:"result,omitempty"` // 成功：回给脚本的值（结构化或 JSON 字符串）
	Error  string          `json:"error,omitempty"`  // 失败：脚本内 reject 的 message
}

// bridgeShutdown 收尾消息：宿主不再回执。沙箱侧收到后立刻把所有在途调用一次性拒掉
// （悬空 = 脚本挂满整段时限且看不出原因），此后新调用直接失败。
//
// 什么时候发：① 超时/取消——此刻子进程还活着，这条消息让脚本的 catch/finally 有机会
// 跑（与 runExec 的 kill 同时发生，谁先到不保证，属于零成本礼让）；② 本次执行收尾
// （子进程通常已经退出，写不进去就算了）。
type bridgeShutdown struct {
	Notify string `json:"notify"`           // 恒 "shutdown"
	Reason string `json:"reason,omitempty"` // 给脚本看的原因（可选）
}

// ---- 帧 tail 读取器 ----

// framePollInterval 轮询间隔。spool 是普通文件（没有阻塞读可用），只能轮询；2ms 的
// 代价是每次工具往返多 ~1–2 次轮询（相对一次工具执行可忽略），换来的是不引入
// FIFO/socket 那类新面（见文件头）。
const framePollInterval = 2 * time.Millisecond

// frameTail 增量读一个**只增**的行文件，把新到的一行交给 onLine。
//
// 并发约定：run() 是唯一读者，直到 finish() 关闭 stopped 并 join 之后，finish() 才
// 接过 fd 做最后一次排干（两者不并发读同一个 fd）。onLine 在**锁外**调用 —— onFrame
// 会 spawn 分派 goroutine 并可能关 stdin，占着锁会把 finish 一起卡住。
type frameTail struct {
	path   string
	onLine func(line string)

	stopped chan struct{}
	done    chan struct{}
	file    *os.File

	mu   sync.Mutex
	raw  []byte // 已读到的**全部**字节（append-only，含已消费的行）—— finish 的返回值
	part []byte // 尚未遇到换行的尾巴（每消费一行就左移掉）
}

func newFrameTail(path string, onLine func(line string)) *frameTail {
	return &frameTail{path: path, onLine: onLine, stopped: make(chan struct{}), done: make(chan struct{})}
}

// start 打开 spool 并起读（宿主先建好了空文件，故此刻必然存在）。
func (t *frameTail) start() error {
	f, err := os.Open(t.path)
	if err != nil {
		return fmt.Errorf("codemode: 打开帧 spool 失败（本次执行无法取得任何帧）: %w", err)
	}
	t.file = f
	go t.run()
	return nil
}

func (t *frameTail) run() {
	defer close(t.done)
	buf := make([]byte, 64<<10)
	for {
		n, err := t.file.Read(buf) // 顺序读：位置由 fd 维护，天然只读新增部分
		if n > 0 {
			t.consume(buf[:n])
		}
		if err != nil && !errors.Is(err, io.EOF) {
			// 读不下去（文件被外力删/权限变化）：停止读，剩下的交给 finish 的排干与
			// 结局判定 —— 这里不报错，帧缺失最终会表现为「没有 result 帧」，而那是
			// 更准确的事实（不是「桥坏了」，是「没读到收尾帧」）。
			return
		}
		if n == 0 {
			select {
			case <-t.stopped:
				return
			case <-time.After(framePollInterval):
			}
		}
	}
}

// consume 收下新字节：原始字节全部留档（finish 的返回值 = SplitFrames 的输入），
// 成行的部分逐行交给 onLine（锁外）。
func (t *frameTail) consume(chunk []byte) {
	var lines []string
	t.mu.Lock()
	t.raw = append(t.raw, chunk...)
	t.part = append(t.part, chunk...)
	for {
		i := bytes.IndexByte(t.part, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, strings.TrimSuffix(string(t.part[:i]), "\r"))
		t.part = append(t.part[:0], t.part[i+1:]...) // 就地左移：part 通常只有一行的量
	}
	t.mu.Unlock()
	for _, ln := range lines {
		t.onLine(ln)
	}
}

// finish 停读、排干剩余字节（子进程已退出，剩下的都是它最后写的）、关文件，返回
// **完整文本**（宿主侧的 Raw：帧与用户输出混在一起，交给 SplitFrames 分）。
func (t *frameTail) finish() string {
	close(t.stopped)
	<-t.done
	if t.file != nil {
		// 排干：run 已停，此刻只有本函数读这个 fd。
		buf := make([]byte, 64<<10)
		for {
			n, err := t.file.Read(buf)
			if n > 0 {
				t.consume(buf[:n])
			}
			if n == 0 || err != nil {
				break
			}
		}
		_ = t.file.Close()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.raw)
}

// ---- 宿主侧桥接（帧分派 + 出站写侧）----

// bridgeHost 一次执行的双向端点：读侧（frameTail → call 分派）与写侧（init/reply/
// shutdown，共享一条 stdin，必须互斥）。
type bridgeHost struct {
	ctx       context.Context // 传给 OnCall 的 ctx（本次执行的派生 ctx）
	onCall    func(ctx context.Context, c ScriptCall) ScriptResult
	initBytes json.RawMessage // init 的载荷原文

	pr *io.PipeReader // 交给 sandbox.ExecSpec.Stdin
	pw *io.PipeWriter

	tail *frameTail

	mu sync.Mutex // 出站写侧互斥：并发回执 + init + shutdown 共用一条 stdin

	// closed 用原子量而不是「受 mu 保护的普通字段」：写侧可能**持着 mu 卡在管道上**
	//（沙箱不读 stdin：缓冲区满或根本没起来），而 closeStdin 必须能在那一刻把管道关掉
	// 来解出那次写（io.Pipe 的 Write 在 close 时立刻返回）—— 如果 closeStdin 要先拿 mu，
	// 它就与那次写互等，收尾路径永久挂死。实测：假 Sandbox（没人读管道）必挂。
	closed    atomic.Bool
	closeOnce sync.Once

	// dispatchSeq 派发序号（见 ScriptCall.Index）。
	dispatchSeq atomic.Int64
}

func newBridgeHost(ctx context.Context, spool string, init json.RawMessage, onCall func(context.Context, ScriptCall) ScriptResult) *bridgeHost {
	pr, pw := io.Pipe()
	h := &bridgeHost{ctx: ctx, onCall: onCall, initBytes: init, pr: pr, pw: pw}
	h.tail = newFrameTail(spool, h.onLine)
	return h
}

// stdin 交给 ExecSpec.Stdin（宿主 → 沙箱通道）。
func (h *bridgeHost) stdin() io.Reader { return h.pr }

// start 起读帧 + 写 init + 挂超时/取消的松手钩子。
//
// init 必须**在任何回执之前**落到管道上：沙箱侧在顶层 await 里阻塞等它，读到之前不会
// 求值用户脚本，也就发不出 call。故用一个 goroutine 写 —— 直接写会死锁：io.Pipe 的读者
// 是 os/exec 的拷贝协程，那个协程 Start 之后才存在，而本函数在 Run 之前。互斥锁保证
// init 与后续回执的先后（回执只可能发生在沙箱读过 init 之后）。
func (h *bridgeHost) start(ctx context.Context) error {
	if err := h.tail.start(); err != nil {
		return err
	}
	go func() {
		// 写不进去 = 沙箱根本读不到 init（进程没起来/立刻死了）：结局由 Run 侧的输出
		//（缺 hello/result 帧）决定，这里不重复报错。
		_ = h.write(bridgeInit{Notify: notifyInit, Init: h.initBytes})
	}()
	go func() {
		select {
		case <-ctx.Done():
			h.shutdown("the run was cancelled or timed out")
		case <-h.tail.done:
			// 帧读协程先停了（finish 已接手）：无需再管。
		}
	}()
	return nil
}

// onLine 一行（来自 frameTail：去掉换行的整行，**含哨兵** —— 那就是线上的样子）。
// 解析复用 SplitFrames 的同一条规则（哨兵 + notify 白名单），故这里**不能**再补一次
// 哨兵：补了就变成 \x1e\x1e{...}，stripFrameSentinel 只去一层，JSON 解不开、帧被静默丢弃
// （实测：call 帧解析失败 → 回执永远发不出去 → 脚本挂到超时）。
func (h *bridgeHost) onLine(line string) {
	f, ok := parseFrameLine(line)
	if !ok {
		return // 非帧行（用户直写 / sandbox 注记）：留给 SplitFrames 归入 tail
	}
	switch f.Notify {
	case notifyCall:
		h.dispatch(f)
	case notifyResult, notifyError:
		// 终结帧：prelude.js 只在 'exit' 上发 result，其后的写入只可能来自用户自己的
		// 退出钩子（同步）—— 管道那头没人再读了，立刻松手，别让 Wait 白等 3s。
		h.closeStdin()
	}
}

// dispatch 分派一次 call：**每次调用一个 goroutine**（脚本内 Promise.all 要真并发）
// 回执按 id 关联、允许乱序。串行/独占语义由上层（ExecScope）决定，本层不设门。
func (h *bridgeHost) dispatch(f Frame) {
	// 发号必须在**本函数**（读帧协程里、按帧序调用）完成，不能挪进下面的 goroutine：
	// 挪进去就跟着调度乱了，而这个号的全部价值就是「不受调度影响」。
	call := ScriptCall{Id: f.Id, Name: f.Name, Args: f.Args, Index: h.dispatchSeq.Add(1)}
	go func() {
		// 子进程已死（超时/取消后的在途回执）时写会失败——无人可收，静默丢弃。
		_ = h.writeReply(call, h.invoke(call))
	}()
}

// invoke 调上层分发器；panic 也要变成回执 —— 否则脚本永远等不到这次回执，只能等满整段
// 时限，一次上层 bug 会伪装成「脚本太慢」。
func (h *bridgeHost) invoke(c ScriptCall) (res ScriptResult) {
	defer func() {
		if p := recover(); p != nil {
			res = ScriptResult{
				Value:   jsonString(fmt.Sprintf("codemode: tool dispatcher panicked on %q: %v", c.Name, p)),
				IsError: true,
			}
		}
	}()
	if h.onCall == nil {
		return ScriptResult{
			Value:   jsonString("codemode: no tool dispatcher is configured (ScriptOpts.OnCall is nil)"),
			IsError: true,
		}
	}
	return h.onCall(h.ctx, c)
}

// writeReply 把回执编码成出站消息。
func (h *bridgeHost) writeReply(c ScriptCall, r ScriptResult) error {
	msg := bridgeReply{Notify: notifyReply, Id: c.Id}
	if r.IsError {
		msg.Error = scriptErrorText(r.Value)
	} else {
		msg.Result = ensureJSON(r.Value)
	}
	return h.write(msg)
}

// finish 收尾：停读 + 排干，发 shutdown（子进程通常已退出），关 stdin。
func (h *bridgeHost) finish() string {
	raw := h.tail.finish()
	h.shutdown("the script run has finished")
	return raw
}

// shutdown 告诉沙箱「不会再有回执」，然后关掉宿主 → 沙箱管道。
//
// 写入给一个小预算而不是无限等：写侧可能正阻塞在管道上（沙箱没在收 —— 它正忙循环，
// 或者根本没起来），而收尾路径不能为此挂住。超时就放弃 —— 紧接的 closeStdin 会解出
// 那次阻塞的写（io.Pipe 的 Write 在管道关闭时立刻返回 ErrClosedPipe），消息本身在
// 子进程已死的情况下本来也送不到（那是尽力而为，见 bridgeShutdown）。
//
// 关闭动作在锁**外**（见 closeStdin）：写侧可能正持着锁等在管道上，持锁关管会与它
// 互等 —— 只有真关掉，那次 Write 才会返回并放锁。
func (h *bridgeHost) shutdown(reason string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.write(bridgeShutdown{Notify: notifyShutdown, Reason: reason})
	}()
	select {
	case <-done:
	case <-time.After(shutdownWriteBudget):
	}
	h.closeStdin()
}

// shutdownWriteBudget 收尾消息的写入预算（见 shutdown）。
//
// 100ms 的依据：沙箱**在收**时这条消息只有几十字节，落到管道是微秒级；会吃掉整个
// 预算的只有「写侧已经卡在管道上」这一种情况，那时等多久都没用（要靠关管道解），
// 所以这个数只需要小到不让人察觉，而不需要够大到「给它一个机会」。
const shutdownWriteBudget = 100 * time.Millisecond

// closeStdin 关掉宿主 → 沙箱管道（幂等）。三个触发点：终帧（子进程开始收尾）、
// ctx 结束（超时/取消）、finish（兜底）。不关的代价见文件头「谁来关 stdin」。
//
// 全程不碰 mu（见 closed 字段的注释）：关管道本身就会让卡在里面的那次写立刻返回，
// 先拿 mu 反而会与它互等。
func (h *bridgeHost) closeStdin() {
	h.closeOnce.Do(func() {
		h.closed.Store(true)
		_ = h.pw.Close()
	})
}

// write 写一条出站消息（一行 JSON）。
//
// 互斥的意义：一行 = 一次 Write，且与 shutdown 的先后可判。注意 io.Pipe 恰好把单次
// Write 串行化（内部持锁 + 整块交付），所以「去掉互斥」在本写路径下不会立刻表现为行
// 撕裂 —— 但那是 io.Pipe 的实现细节而非契约（os/exec 还隔着一条拷贝协程与 OS 管道），
// 拿它当原子性保证是把正确性押在别人的实现细节上（变异结论见交付报告）。
func (h *bridgeHost) write(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		// 只有畸形 json.RawMessage 才可能走到这里（消息全由基础类型 + 原始 JSON 组成）。
		return fmt.Errorf("codemode: 编码出站消息失败: %w", err)
	}
	b = append(b, '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed.Load() {
		return errors.New("codemode: bridge is closed (the sandbox is gone)")
	}
	if _, err := h.pw.Write(b); err != nil {
		h.closed.Store(true)
		return err
	}
	return nil
}

// ensureJSON 保证回执值能编成 JSON。
//
// 为什么需要兜一层：ScriptResult.Value 是「已经编好的 JSON」，上层若塞了**裸文本**
// （不是 JSON 字符串字面量），json.Marshal 会失败 —— 那条回执就发不出去，脚本只能等满
// 整段时限，一次上层笔误被伪装成「脚本太慢」。这里退化成 JSON 字符串（脚本拿到的仍是
// 可读文本），代价是畸形输入变成「能看见的内容」而不是「卡死」。
func ensureJSON(v json.RawMessage) json.RawMessage {
	if len(v) == 0 {
		return nil // 回执不带 result 字段 ⇒ 脚本侧 resolve(undefined)
	}
	if json.Valid(v) {
		return v
	}
	return jsonString(string(v))
}

// scriptErrorText 把 IsError 的 Value 变成脚本侧 Error.message。
//
// 三种输入：JSON 字符串 ⇒ 去掉引号的原文（最常见：上层直接给一段错误文本）；
// 结构化值 ⇒ JSON 原文（脚本至少能看到字段，而不是 "[object Object]"）；空 ⇒ 兜底。
func scriptErrorText(v json.RawMessage) string {
	if len(v) == 0 {
		return "the tool call failed (no error detail was provided)"
	}
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		return s
	}
	return string(v)
}

// jsonString 把 Go 字符串编成 JSON 字符串字面量（ScriptResult.Value 用）。
func jsonString(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`"<unencodable>"`)
	}
	return b
}
