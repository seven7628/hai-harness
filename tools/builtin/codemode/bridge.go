package codemode

// bridge.go：工具**主流程**（Wave 2）。
//
// 一次 codemode 调用长这样（对齐设计文档 §13.4 的八步，逐条标注落点）：
//
//	1. 解析输入（单字段 code）                     → parseInput
//	2. 首行 // @options 严格解析（非法 = 不进沙箱）  → descriptions.ParseOptionsLine
//	3. 生成目录（可编排名映射表 + 描述）            → BuildCatalog
//	4. 建 store 快照与待落盘批                     → store.Store / store.Pending
//	5. 生成 Init（工具表/store）与 Scaffold（全局） → protocol.go
//	6. 执行：ExecuteScript + OnCall 分派            → bridge.onCall → engine.ExecuteOne
//	7. 收尾：store 仅成功时 Commit、截断 + spill 0600 + 表头
//	8. 结构化结果写 ctx 槽 + ExecSink 上报结局
//
// # 三条最容易被写错、且错了很难查的纪律
//
//  1. **分派只认 Catalog.Names**（绝不用 engine.GetTool 兜底）：目录表是**第一层闸**
//     （由 BuildCatalog 按 exposure 分层产出，只含 direct/codemode/deferred）。
//     GetTool 兜底会让 hidden/model-only 从脚本里可达 ——「撤下工具」与「禁自嵌套」
//     当场失效。引擎侧另有第二层闸（ExecuteOne 的暴露档守卫，见 tools/nested.go），
//     两层各自独立才叫纵深：编排方的纪律不该依赖引擎记得拦。
//  2. **每次子调用都带 ParentCallId + Depth:1**：记录/用量/脚本标记三条路径都挂在
//     Depth>0 上（引擎的 finish 只在 Depth==0 取记录；WithScriptCall 只在 Depth>0 注入
//     —— 后者决定 bash 这类工具给的是结构化对象还是 20 KB 文本）。
//  3. **墙钟 owner 是宿主**：给 ExecuteScript 传 Timeout<0（沙箱侧不限时），自己用可取消
//     的 ctx 管预算，审批等待期间暂停（否则人还在看确认弹窗，脚本先被自己的 30s 判死）。
//
// # 收尾的取消（教学正文承诺的那条）
//
// 教学正文说 "When the script ends, calls that are still running are cancelled"。
// 本文件给每次子调用一个**可取消**的 ctx，并在执行收尾时统一取消（bridge.close /
// 每次调用返回后 cancel）。已知边界（如实记录，别在这里美化）：脚本 `return` 之后仍有一条
// **未被 await** 的调用在跑时，沙箱进程会一直等那条调用的回执（prelude_bridge.js 的保活
// 曲线：pending>0 ⇒ ref 住 stdin ⇒ 不退出），ExecuteScript 也就不返回 —— 这个窗口里宿主
// **没有**「脚本已结束」的信号（传输层没有 pi 的 done 帧），故取消要等到回执落地。
// 真正的修法在传输层（脚本结束时补一条收尾帧；pi 的 MsgDone 就是这个作用），
// Wave 2 不动 execproc/**，故这里做到「执行收尾即取消」，并由 bridge_test.go 的
// TestScriptErrorCancelsInFlightCall / TestExitCancelsInFlightCall 钉住可兑现的那一半。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/execproc"
	"github.com/seven7628/hai-harness/tools"
)

const (
	// spillDirPrefix spill 目录前缀（os.MkdirTemp 在系统临时目录下建 0700 目录 ——
	// 天然在工作区之外，同 bash 的取舍）。
	spillDirPrefix = "hai-harness-codemode-spill-"
	// spillFileMode 落盘权限：脚本输出里会有路径/凭据/私有数据，只有本人可读
	//（pi 的 codemode spill 是裸 writeFile —— 那是它的缺陷，§15.4 明确不照抄）。
	spillFileMode = 0o600
	// fullOutputNoteFormat 结果文本里点名 spill 路径的文案。
	//
	// **必须**与宿主前端 desktop/app/src/lib/codemodeNested.ts 的 SPILL_PATTERNS 第一条
	// 对齐（`[Full output: <path> …]`）：对不上时 UI 不点名路径，而且不报错（静默丢功能）。
	fullOutputNoteFormat = "[Full output: %s (read with offset/limit)]"
	// spillFailedNote 落盘失败时的替代文案（路径拿不到，就不能编一个）。
	spillFailedNote = "[the full output could not be saved to disk; the text above is all that is kept]"
	// execCommandLabel ExecSink 的命令标签：只用来过引擎的「良性非零退出」判定
	//（benignNonZeroExit 按 filepath.Base 匹配 grep/diff 等）。脚本不是那些命令，
	// 故任何非零退出都必须判失败。
	execCommandLabel = "codemode"
	// defaultSearchLimit / maxSearchLimit searchTools 的返回条数（对齐 pi
	// DEFAULT_TOOL_SEARCH_LIMIT = 8）；上限防脚本一次把整张目录拉回去。
	defaultSearchLimit = 8
	maxSearchLimit     = 50
	// maxImageFileBytes image(path) 单文件读入上限（防止把一个几百 MB 的文件读进内存）。
	// 真正的闸门在引擎：超上限的图片不进模型上下文，并在结果文本里明示降级。
	maxImageFileBytes = 8 << 20
)

// wallClockNote 墙钟到点时的结局声明（首行）。
//
// 措辞纪律（IMPLEMENTATION-SPEC §6.5）：脚本输出在沙箱进程内缓冲、只在脚本正常收尾时
// 随 result 帧回传 —— 被 kill 的脚本**一条也拿不回来**，所以「有没有真拿到输出」决定
// 后半句；拿不到就绝不能承诺 PARTIAL（模型会把空当成真结果继续推理，本仓实测 130 次）。
const (
	wallClockNoteNoOutput = "[TIMEOUT after %s - the script was killed at the codemode wall-clock " +
		"limit; script output is only returned when the script finishes, so nothing could be recovered]"
	wallClockNotePartial = "[TIMEOUT after %s - the script was killed at the codemode wall-clock " +
		"limit and the output below is PARTIAL]"
)

// input 工具入参（单字段 schema，与 Parameters() 一一对应）。
type input struct {
	Code string `json:"code"`
}

// parseInput 解析工具入参。
//
// 刻意不宽容（不进沙箱）：模型把源码塞进 JSON 字符串、用 markdown 围栏、或干脆传个对象
// 时，报错要直接说明「要的是原始 JS 源码」——把这三种写法放进去只会得到一句
// "SyntaxError"，而模型看不出是信封错了还是脚本错了。
func parseInput(args string) (input, error) {
	var in input
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return input{}, fmt.Errorf(
			"codemode: arguments must be a JSON object with a `code` field holding raw "+
				"JavaScript source (not JSON, not a quoted string, not a code fence): %w", err)
	}
	return in, nil
}

// Call 工具主流程（八步见文件头）。
func (t *Tool) Call(ctx context.Context, _ string, args string) (string, error) {
	// 1) 输入。
	in, err := parseInput(args)
	if err != nil {
		return "", err
	}
	// 2) 首行预算声明。非法即失败，**不进沙箱**（静默忽略会让模型以为自己设了预算）。
	so, code, err := ParseOptionsLine(in.Code)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(code) == "" {
		return "", errors.New("codemode: the script is empty - pass raw JavaScript source in `code`")
	}
	// 3) 目录 + 命名映射（硬失败 = 不进沙箱：一个静默错名会让模型的调用打到别的实现上）。
	snap := t.snapshot()
	cat, err := BuildCatalog(DescribeOptions{
		Tools:        snap.catalog,
		Namespaces:   snap.ns,
		ListDirect:   t.mode() == ModeOnly,
		InlineBudget: t.budget(),
	})
	if err != nil {
		return "", err
	}
	// 4) store 快照（load() 的初值）与本次的待落盘批。
	st := t.storeRef()
	pending := NewPending()

	// 5) 墙钟 owner = 宿主：给沙箱传 Timeout<0（它那一侧的固定 deadline 做不到
	//    「审批等待期间暂停墙钟」），自己用可取消的 ctx 管预算。
	//    （上限来源：@options.timeout_ms 优先，其次宿主配置 codemode.budget_seconds，
	//    最后 DefaultScriptTimeoutMs=30s —— 见 Options.ScriptTimeout / scriptTimeout）。
	clock := newWallClock(t.scriptTimeout(so))
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	clock.watch(runCtx, cancelRun)
	defer clock.stop()
	// 子调用的 ctx = runCtx 的派生 + 审批暂停钩子：取消信号照样级联（墙钟/用户中断），
	// 而「等人类」的那段不计入预算（见 pausingCtx）。
	childCtx := pausingCtx(runCtx, clock)

	br := &bridge{
		tool:    t,
		engine:  t.opts.Engine,
		ctx:     childCtx,
		cat:     cat,
		byRaw:   snap.byRaw,
		ns:      snap.ns,
		anchor:  t.nextAnchor(),
		clock:   clock,
		store:   st,
		pending: pending,
		scope:   tools.NewExecScope(events.NestedRecorderFrom(ctx), 0),
		cancels: map[int64]context.CancelFunc{},
	}
	defer br.close()

	res, err := t.executor().ExecuteScript(runCtx, execproc.ScriptOpts{
		Script:   code,
		Cwd:      t.opts.Cwd,
		Timeout:  -1, // 不限时：墙钟由上面的 clock 管（见 §7.1-2）
		Init:     br.initPayload(st.Values()),
		Scaffold: scaffoldSource,
		OnCall:   br.onCall,
	})
	if err != nil {
		// error 只留给「根本没跑起来 + 握手不兼容」：那类问题重试多少次都不会变好，
		// 调用方应把整个 codemode 标为不可用（脚本写错一律走结果文本，见 execproc 契约）。
		return "", err
	}
	// 6) 收尾。
	return br.finish(ctx, res, so), nil
}

// ---- 一次调用的宿主侧状态 ----

// bridge 一次 codemode 调用的状态（并发安全：沙箱内 Promise.all 会并发发起 call 帧）。
type bridge struct {
	tool   *Tool
	engine tools.ExecuteOneEngine
	cat    *Catalog
	byRaw  map[string]tools.Tool
	ns     map[string]*tools.ToolNamespace

	// anchor 本次调用的锚：子调用的 ParentCallId 与 id 前缀都用它。
	//
	// 为什么不是外层 ToolCall.Id：Tool.Call(ctx, name, args) 拿不到它，events.ToolContext
	// 里也没有（只有 RunId）—— 宿主侧唯一存在的身份是 RunId。锚只需满足「会话内唯一 +
	// 非空」（渲染层用 `parent_call_id != ""` 判定「这是嵌套调用，不单独占一行」）。
	anchor string

	// ctx 子调用的父 ctx（runCtx 的派生 + 审批暂停钩子）。
	ctx     context.Context
	clock   *wallClock
	store   *Store
	pending *Pending
	scope   *tools.ExecScope

	mu      sync.Mutex
	seq     int64
	cancels map[int64]context.CancelFunc

	// pendingMu 串行化对 pending 的读写。
	//
	// 为什么必须有：脚本里 `store(...)` 是**并发**发出的（Promise.all / 不 await 的连续
	// 调用都会在同一 tick 里发多个 call 帧），而传输层给每个 call 一个 goroutine
	// （真并发是它的合同）；store.Pending 自身不是并发安全的（它是「一次执行的写入批」，
	// 按设计只有一个写者）。故串行化落在编排方这一层，别去改 store 的语义。
	pendingMu sync.Mutex
}

// initPayload 组装沙箱的初始化载荷：
//   - tools：**只含可编排档**（direct/codemode/deferred）的「脚本名 → raw 名」映射；
//   - store：当前快照（load() 的初值；本地视图见 scaffold）。
func (b *bridge) initPayload(store map[string]json.RawMessage) json.RawMessage {
	names := make([]string, 0, len(b.cat.Names))
	for name := range b.cat.Names {
		names = append(names, name)
	}
	sort.Strings(names)
	list := make([]initTool, 0, len(names))
	for _, name := range names {
		list = append(list, initTool{Name: name, RawName: b.cat.Names[name]})
	}
	raw, err := json.Marshal(initPayload{
		Tools: list,
		Store: store,
		// 限额数值只有一份真源（store.go 的常量）；沙箱侧拿它做 store() 的快速失败。
		StoreLimits: &storeLimits{ValueBytes: MaxStoreValueBytes, TotalBytes: MaxStoreTotalBytes},
	})
	if err != nil {
		// 全是基础类型 + RawMessage（store 的值来自 store 自己的 compact JSON），编不出来
		// 只可能是 store 里混进了畸形值 —— 那属于「根本没跑起来」，让 ExecuteScript 报。
		return json.RawMessage(`{"tools":[]}`)
	}
	return raw
}

// close 收尾：取消所有仍在跑的嵌套调用。
//
// 触发点是「本次执行收尾」（ExecuteScript 返回后 defer 调用）——它覆盖正常收尾/超时/
// 取消/脚本报错/exit() 五条退出路径。边界见文件头「收尾的取消」。
func (b *bridge) close() {
	b.mu.Lock()
	cs := make([]context.CancelFunc, 0, len(b.cancels))
	for _, c := range b.cancels {
		cs = append(cs, c)
	}
	b.cancels = map[int64]context.CancelFunc{}
	b.mu.Unlock()
	for _, c := range cs {
		c()
	}
}

// track 登记一次在途调用（供 close/untrack 取消）。
func (b *bridge) track(id int64, cancel context.CancelFunc) {
	b.mu.Lock()
	b.cancels[id] = cancel
	b.mu.Unlock()
}

// untrack 摘掉一次已结束的调用。
func (b *bridge) untrack(id int64) {
	b.mu.Lock()
	delete(b.cancels, id)
	b.mu.Unlock()
}

// onCall 沙箱发起的每一次调用（传输层每个 call 一个 goroutine，回执允许乱序）。
func (b *bridge) onCall(ctx context.Context, c execproc.ScriptCall) execproc.ScriptResult {
	// 保留名先判：它们带 `$codemode:` 前缀，而归一化会把冒号变成下划线，故与目录表里的
	// 名字不可能撞（见 protocol.go）。
	if op, ok := reservedOp(c.Name); ok {
		return b.hostOp(ctx, op, c.Args)
	}
	raw, err := b.resolve(c.Name)
	if err != nil {
		return scriptError(err)
	}
	return b.runTool(ctx, raw, c.Args)
}

// resolve 把脚本侧名字解析成引擎内 raw 名：**只认目录表**（纪律见文件头）。
//
// 两种名字都认：脚本名（`tools.read_file`）与原始名双键（`tools["mcp__x__y"]`）——
// 传输层两把键都注册，这里必须与它一一对应，否则双键调用会变成「未知工具」。
func (b *bridge) resolve(name string) (string, error) {
	if raw, ok := b.cat.Names[name]; ok {
		return raw, nil
	}
	if _, ok := b.cat.RawNames[name]; ok {
		return name, nil
	}
	return "", fmt.Errorf("unknown tool %q: it is not in this call's codemode tool table "+
		"(hidden, model-only and otherwise non-callable tools are not listed) - use "+
		"searchTools(query) or read the tool list in the codemode description", name)
}

// runTool 派发一次子调用。
func (b *bridge) runTool(ctx context.Context, raw string, args json.RawMessage) execproc.ScriptResult {
	// 墙钟预算：到点后不再发起**新**调用（在跑的那些由 runCtx 取消级联打断）。
	// 文案与 tools.ExecScope.Enter 的预算拒绝保持同一口径（同一件事不该有两种说法）。
	if b.clock.expired() {
		return scriptError(fmt.Errorf(
			"script wall-clock budget exhausted (limit %s); no further tool calls will run - "+
				"return what you have", b.clock.limit()))
	}
	if b.engine == nil {
		return scriptError(errors.New("codemode: no tool engine is configured for this tool " +
			"(Options.Engine is nil) - scripts cannot call tools in this session"))
	}

	// 串行门：CanParallel()==false 的（写类）工具必须串行化 —— 脚本里 Promise.all
	// 发起的并发帧不得让两个写类调用交错。快照里找不到实例（理论上不会）时按**独占**
	// 处理（保守）。
	exclusive := true
	if tl := b.byRaw[raw]; tl != nil {
		exclusive = !tl.CanParallel()
	}
	// inherited 恒为 false：它表达的是「**祖先帧**是否已持锁」，而脚本不可能嵌套脚本
	// （codemode 自己是 model-only + core.NestedMaxDepth=1），故本作用域内的所有帧都是
	// 平级兄弟 —— 兄弟之间必须过闸，那正是这里要做的串行化。
	release := b.scope.Acquire(exclusive, false)
	defer release()

	b.mu.Lock()
	b.seq++
	n := b.seq
	b.mu.Unlock()
	id := childID(b.anchor, n)

	// 墙钟预算的调用级记账：墙钟 owner 在宿主（clock），这里只登记调用栈与次数。
	// （ExecScope 的 wallLimit 传 0：它的 wallStart 是固定的，做不到「审批等待暂停」，
	// 两套墙钟并存只会让「谁到点」变成掷骰子 —— 预算判定统一走 clock。）
	if ok, reason := b.scope.Enter(id); !ok {
		return scriptError(errors.New(reason))
	}
	defer b.scope.Exit()

	// 可取消的 ctx：脚本结束时（bridge.close）与本次调用结束后都会 cancel —— 教学正文
	// 承诺 "calls that are still running are cancelled"，引擎层不取消（它只负责把 ctx
	// 传下去），这条只能由编排方兑现。
	//
	// 基 ctx 用 b.ctx（不是传输层给 onCall 的那个）：它多带一层「审批等待暂停墙钟」的
	// Approver 包装 —— 审批发生在 ExecuteOne 内部，只有这个 ctx 上的 ToolContext 能带上它。
	callCtx, cancel := context.WithCancel(b.ctx)
	b.track(n, cancel)
	defer func() {
		b.untrack(n)
		cancel()
	}()

	argsJSON := strings.TrimSpace(string(args))
	if argsJSON == "" || argsJSON == "null" {
		argsJSON = "{}" // 传输层保证是对象；真拿到空值就补一个空对象（同「无参」语义）
	}
	call := core.ToolCall{
		Id:        id,
		Index:     n,
		Name:      raw,
		Arguments: argsJSON,
		// ParentCallId + Depth:1 是**契约**（不是建议）：记录/用量/脚本标记三条路径
		// 都挂在 Depth>0 上，引擎只在 Depth==0 取记录（见 tools/nested.go 的 godoc）。
	}
	r := b.engine.ExecuteOne(callCtx, call, tools.ExecuteOpts{ParentCallId: b.anchor, Depth: 1})
	return toScriptResult(r)
}

// toScriptResult 引擎结果 → 脚本侧值。**严格照设计文档 §15.4 的表**：
//
//	有结构化（含 IsError） → resolve（脚本拿到对象；错误信息也在字段里，可编程判断）
//	IsError 且无结构化     → reject（message = 结果文本，脚本可 catch）
//	仅文本                 → resolve 成单个 string
//
// 为什么 IsError + 结构化要 resolve：结构化结果是「可编程的事实」（bash 的 exit_code/
// output），脚本要靠字段做分支；把整条拒绝掉会让脚本只剩一个字符串，结构化输出的价值
// 直接蒸发。对齐 pi execute.ts:209-211。
func toScriptResult(r core.ToolResult) execproc.ScriptResult {
	if len(r.Structured) > 0 {
		return execproc.ScriptResult{Value: r.Structured}
	}
	v := jsonText(r.Result)
	if r.IsError {
		return execproc.ScriptResult{Value: v, IsError: true}
	}
	return execproc.ScriptResult{Value: v}
}

// ---- 保留操作（store / image / search / describe）----

// hostOp 保留操作：不走引擎，也不是「工具调用」—— 它们不产生嵌套记录、不占调用预算
// （对齐 pi：store 与 searchTools 都不算一次工具调用）。
func (b *bridge) hostOp(ctx context.Context, op string, args json.RawMessage) execproc.ScriptResult {
	switch op {
	case opStore:
		return b.opStore(args)
	case opImage:
		return b.opImage(ctx, args)
	case opSearch:
		return b.opSearch(args)
	case opDescribe:
		return b.opDescribe(args)
	}
	return scriptError(fmt.Errorf("codemode: unknown host operation %q", op))
}

// opStore 暂存一次写入。限额在 Pending.Set 里就查（单值 256 KiB + JSON 合法性）——
// 脚本当场看得见错误，而不是等脚本跑完才失败。总量上限只在 Commit 时能查（它要看
// apply 之后的整店），失败会走收尾的 store_error 路径。
func (b *bridge) opStore(args json.RawMessage) execproc.ScriptResult {
	var req storeRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return scriptError(fmt.Errorf("codemode store: bad request (%v)", err))
	}
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if req.Delete {
		if err := b.pending.Delete(req.Key); err != nil {
			return scriptError(err)
		}
		return jsonResult(storeReply{Key: req.Key, Deleted: true})
	}
	if err := b.pending.Set(req.Key, req.Value); err != nil {
		return scriptError(err)
	}
	rep := storeReply{Key: req.Key}
	if compact, err := compactJSON(req.Value); err == nil {
		rep.Bytes = len(compact)
	}
	return jsonResult(rep)
}

// opImage 把一张图片加进本次调用的结果。
//
// 为什么读文件与校验格式在宿主：沙箱只递路径；非法图片必须**在这里**就被拒掉 ——
// 一个坏块写进对话历史会让之后每一个请求失败（设计文档 §20.3 / pi #10215）。
func (b *bridge) opImage(ctx context.Context, args json.RawMessage) execproc.ScriptResult {
	var req imageRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return scriptError(fmt.Errorf("codemode image: bad request (%v)", err))
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		return scriptError(errors.New("codemode image: path is empty"))
	}
	st, err := os.Stat(path)
	if err != nil {
		return scriptError(fmt.Errorf("codemode image: %s is not readable: %w", path, err))
	}
	if st.IsDir() {
		return scriptError(fmt.Errorf("codemode image: %s is a directory", path))
	}
	if st.Size() > maxImageFileBytes {
		return scriptError(fmt.Errorf("codemode image: %s is %d bytes, over the %d-byte limit "+
			"for an embedded image - downscale it first and pass the smaller file",
			path, st.Size(), maxImageFileBytes))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return scriptError(fmt.Errorf("codemode image: reading %s failed: %w", path, err))
	}
	mime, ok := sniffImageMIME(data)
	if !ok {
		return scriptError(fmt.Errorf("codemode image: %s is not a supported image "+
			"(png/jpeg/gif/webp) - nothing was added to the result", path))
	}
	// 引擎的图片闸门（数量/单张体积）随后生效：超限的图片不进模型上下文，并在结果文本里
	// 明示降级 —— 这里不做二次判定（两处判定迟早会漂移）。
	tools.ImageSinkFrom(ctx).Add(core.Content{
		Type:     core.ContentTypeImage,
		Content:  "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
		MimeType: mime,
	})
	return jsonResult(imageReply{Path: path, Mime: mime, Bytes: len(data)})
}

// opSearch searchTools(query[, limit])：在**可调用**工具集上做一次朴素检索。
//
// 检索质量如实说：这是「子串/词命中 + 名字权重」的打分，不是 BM25（pi 的 Bm25Ranker
// 排在 P2-b）。它够用的场景是「我记得有个工具叫 xxx / 描述里有 yyy」；不够用的是同义词
// 与语义相近的查询。放在宿主侧正是为了将来换算法不动沙箱。
func (b *bridge) opSearch(args json.RawMessage) execproc.ScriptResult {
	var req searchRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return scriptError(fmt.Errorf("codemode searchTools: bad request (%v)", err))
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	hits := b.search(req.Query, limit)
	return jsonResult(searchReply{Tools: hits})
}

// opDescribe describeTool(name)：一个工具的完整说明（含 namespace 的 instructions ——
// 长使用指引刻意不进描述，只在这里给）。
func (b *bridge) opDescribe(args json.RawMessage) execproc.ScriptResult {
	var req describeRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return scriptError(fmt.Errorf("codemode describeTool: bad request (%v)", err))
	}
	name := strings.TrimSpace(req.Name)
	raw, err := b.resolve(name)
	if err != nil {
		return scriptError(err)
	}
	rep := describeReply{Name: raw, ScriptName: b.cat.RawNames[raw]}
	if tl := b.byRaw[raw]; tl != nil {
		rep.Description = tl.Description()
		for _, p := range paramsOf(tl.Parameters()) {
			rep.Params = append(rep.Params, paramView{Name: p.Name, Optional: p.Optional})
		}
	}
	if ns := b.ns[namespaceNameOfRaw(b.byRaw[raw])]; ns != nil {
		rep.Namespace = ns.Name
		rep.NamespaceDescription = ns.Description
		rep.NamespaceInstructions = ns.Instructions
	}
	return jsonResult(rep)
}

// search 朴素检索：命中词打分（名字权重最高），按分数降序、同分按名字升序（确定性）。
func (b *bridge) search(query string, limit int) []searchHit {
	terms := searchTerms(query)
	type scored struct {
		hit   searchHit
		score int
	}
	var out []scored
	names := make([]string, 0, len(b.cat.Names))
	for _, raw := range b.cat.Names {
		names = append(names, raw)
	}
	sort.Strings(names)
	for _, raw := range names {
		tl := b.byRaw[raw]
		summary := ""
		nsName := ""
		if tl != nil {
			summary = oneLine(tl.Description(), maxEntrySummaryChars)
			nsName = namespaceNameOf(tl)
		}
		lname := strings.ToLower(raw)
		lsummary := strings.ToLower(summary)
		lns := strings.ToLower(nsName)
		score := 0
		for _, t := range terms {
			switch {
			case strings.Contains(lname, t):
				score += 4
				if strings.HasPrefix(lname, t) {
					score += 2
				}
			case strings.Contains(lns, t):
				score += 2
			case strings.Contains(lsummary, t):
				score++
			}
		}
		if score == 0 {
			continue
		}
		out = append(out, scored{
			hit:   searchHit{Name: b.cat.RawNames[raw], Summary: summary, Namespace: nsName},
			score: score,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].hit.Name < out[j].hit.Name
	})
	if len(out) > limit {
		out = out[:limit]
	}
	hits := make([]searchHit, 0, len(out))
	for _, s := range out {
		hits = append(hits, s.hit)
	}
	return hits
}

// ---- 收尾 ----

// finish 组装结果：store 落盘 → 表头/正文/返回值 → 截断 + spill → 结构化槽 + ExecSink。
func (b *bridge) finish(ctx context.Context, res execproc.RunResult, so ScriptOptions) string {
	wall := b.clock.wallElapsed()
	killed := b.clock.expired()
	failed := res.ExitCode != 0 || killed

	// store：**只有成功脚本才 Commit**（失败/超时/取消一律把 Pending 丢掉 —— 不写盘是
	// 默认行为，不是需要额外判断的分支）。
	var storeErr error
	if !failed {
		// 锁：与在途的 store() 帧互斥（正常情况下此刻已经没有在途帧 —— 沙箱要收到每条
		// 回执才会退出，而 ExecuteScript 已返回；锁是防「将来某条路径提前返回」的那道底）。
		b.pendingMu.Lock()
		storeErr = b.store.Commit(b.pending)
		b.pendingMu.Unlock()
		if storeErr != nil {
			failed = true
		}
	}

	body := res.Text
	if killed {
		// 墙钟 owner 是宿主，故到点在沙箱侧表现为 **ctx 取消**（它按自己的口径印
		// [CANCELLED …]）—— 但对模型这是**超时**：用户中断与时限到点必须分开措辞
		//（§6.5 / tools/engine.go 的同款纪律）。这里只剥掉那一行的**前缀**，
		// 正文执行层的措辞仍归执行层。
		body = retimeoutBody(body, b.clock.limit(), recoveredOutput(res.Raw))
	}

	var text string
	switch {
	case killed, res.TimedOut, res.Canceled:
		// 结局声明必须是**首行**：前面插任何表头都会把它埋掉（模型读不到「被杀了」，
		// 就会把残缺文本当结果继续推理）。返回值同样不渲染（脚本没跑完）。
		text = body
	default:
		text = renderCompleted(res, body, wall, failed)
	}
	if storeErr != nil {
		text += storeFailureNote(storeErr)
	}

	text, truncated, spillPath := b.budget(ctx, text, so)

	b.writeStructured(ctx, res, structInfo{
		ok:        !failed,
		truncated: truncated,
		spillPath: spillPath,
		wall:      wall,
		nested:    int(b.scope.ExecCount()),
		storeErr:  errText(storeErr),
	})
	b.writeExecStatus(ctx, res, killed, failed)
	return text
}

// renderCompleted 正常收尾的结果文本：表头 + 脚本输出 + 返回值。
//
// 表头形态对齐 pi（execute.ts:309-319）：`Script completed|failed` / `Wall time …` /
// `Output:` 三行 —— 模型据此知道脚本是跑完了还是报错了（后者还会带 [script error] 段）。
func renderCompleted(res execproc.RunResult, body string, wall time.Duration, failed bool) string {
	outcome := "completed"
	if failed {
		outcome = "failed"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Script %s\nWall time %.1f seconds\nOutput:\n", outcome, wall.Seconds())
	if t := strings.TrimRight(body, "\n"); t != "" {
		sb.WriteString(t)
		sb.WriteByte('\n')
	}
	if v := renderValue(res.Value); v != "" {
		sb.WriteString(v)
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

// renderValue 把脚本 return/exit 值渲染成模型可读文本：
//   - JSON 字符串 → 原文（最常见的形态：`return "done"` 就该看到 done 而不是 "done"）；
//   - 其他 → 两空格缩进的 JSON（对象/数组可读；数字/布尔/null 原样）。
//
// 「无返回值」与「返回 null」要分得开：前者不渲染，后者渲染成 null（与 execproc 的
// RunResult.Value 契约一致：nil = 没写 return，null 字面量 = 显式返回 null）。
func renderValue(v json.RawMessage) string {
	trimmed := bytes.TrimSpace(v)
	if len(trimmed) == 0 {
		return ""
	}
	// 先看首字节再决定怎么解：`null` 解进 string **不报错**（原地不动），会被误当成
	// 「没有返回值」—— 而「脚本显式返回 null」与「没写 return」是两件事。
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return s
		}
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, trimmed, "", "  "); err == nil {
		return buf.String() // null / 数字 / 布尔 / 对象 / 数组
	}
	return string(trimmed)
}

// retimeoutBody 把执行层的取消声明换成超时声明（首行），其余内容原样保留。
//
// 两种后半句按「有没有真拿到**脚本输出**」分（§6.5）。判据是 recovered 而不是「文本非空」：
// Text 里除了脚本输出还有 sandbox 注记（如 "[exit: context canceled]"）与 [script error]
// 段 —— 把注记当成输出，就会对模型承诺「下面的是部分输出」而下面其实是空的，那正是这条
// 措辞纪律要根除的形态。
func retimeoutBody(body string, limit time.Duration, recovered bool) string {
	rest := stripCancelledNote(body)
	note := fmt.Sprintf(wallClockNoteNoOutput, limit)
	if recovered {
		note = fmt.Sprintf(wallClockNotePartial, limit)
	}
	if strings.TrimSpace(rest) == "" {
		return note
	}
	return note + "\n" + rest
}

// recoveredOutput 本次执行有没有真拿到脚本输出（从协议帧判定）。
//
// result 帧的 out 是脚本输出的**唯一**载体（prelude 把 stdout/stderr 攒进它），
// 故「有没有 out」比任何文本形态的猜测都准；value 一并算（exit(v)/return 也是成果）。
func recoveredOutput(raw string) bool {
	frames, _ := execproc.SplitFrames(raw)
	for _, f := range frames {
		if f.Notify == "result" && (f.Out != "" || len(f.Value) > 0) {
			return true
		}
	}
	return false
}

// stripCancelledNote 剥掉执行层印在首行的取消声明（只认前缀，不复制整句文案）。
func stripCancelledNote(text string) string {
	const prefix = "[CANCELLED "
	if !strings.HasPrefix(text, prefix) {
		return text
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[i+1:]
	}
	return ""
}

// storeFailureNote 落盘被拒时的结果文本补充（模型必须知道「这次调用的写入没生效」）。
func storeFailureNote(err error) string {
	return "\n[codemode store] the script finished, but its store writes were rejected and " +
		"NOTHING was written: " + err.Error()
}

// budget 输出预算：max_output_tokens × 4 字节（与 descriptions.charsPerToken 同一口径，
// 两处必须一致 —— 否则「描述说 4000 token」与实际给的字节数会差个倍数），超预算时
// 头尾各半 + 中间标注 + 全文 spill 到 0600 文件并在结果里点名路径。
//
// 返回（结果文本, 是否截断, spill 路径）。
func (b *bridge) budget(ctx context.Context, text string, so ScriptOptions) (string, bool, string) {
	budgetBytes := so.OutputTokens() * charsPerToken
	if budgetBytes <= 0 || len(text) <= budgetBytes {
		return text, false, ""
	}
	head := budgetBytes / 2
	tail := budgetBytes - head
	headCut := cutAtRuneBoundary(text, head, false)
	tailStart := cutAtRuneBoundary(text, len(text)-tail, true)
	if tailStart < headCut {
		tailStart = headCut
	}
	dropped := len(text) - headCut - (len(text) - tailStart)
	if dropped < 0 {
		dropped = 0
	}
	path := b.tool.spillFile(runIDOf(ctx), text)
	var sb strings.Builder
	sb.WriteString(text[:headCut])
	// 中间标注沿用设计文档的措辞锚点（pi 的 `…N tokens truncated…`）：模型看到它就知道
	// 「中间被挖掉了」，而不是把两段拼起来当连续内容读。
	fmt.Fprintf(&sb, "\n\n...[%d tokens truncated - over the max_output_tokens budget]...\n\n",
		(dropped+charsPerToken-1)/charsPerToken)
	sb.WriteString(text[tailStart:])
	sb.WriteString("\n")
	if path != "" {
		sb.WriteString(fmt.Sprintf(fullOutputNoteFormat, path))
	} else {
		sb.WriteString(spillFailedNote)
	}
	return sb.String(), true, path
}

// cutAtRuneBoundary 把字节偏移挪到 UTF-8 边界上（forward=false 往前退，true 往后进）。
// 不劈多字节字符：劈开的半个字符会让整段结果变成乱码（模型看不出哪里坏了）。
func cutAtRuneBoundary(s string, at int, forward bool) int {
	if at <= 0 {
		return 0
	}
	if at >= len(s) {
		return len(s)
	}
	for at > 0 && at < len(s) && !utf8.RuneStart(s[at]) {
		if forward {
			at++
		} else {
			at--
		}
	}
	return at
}

// structInfo 结构化结果的元信息（字段多，用具名结构避免位置参数拼错）。
type structInfo struct {
	ok        bool
	truncated bool
	spillPath string
	wall      time.Duration
	nested    int
	storeErr  string
}

// codemodeResult 结构化结果（写进**引擎注入的 ctx 槽**）。
//
// D2 的教训：不是实例字段（主 agent 与后台子 agent 共享同一个 ToolEngine，同一实例会被
// 并发调用，实例字段会互相清空/串味），也不是 getter（引擎与工具对「哪一次调用」达不成
// 一致）。ctx 槽是 ImageSink/ExecSink/StructuredSink 的同款范式：每次调用独立。
type codemodeResult struct {
	OK             bool            `json:"ok"`
	Value          json.RawMessage `json:"value,omitempty"`
	Truncated      bool            `json:"truncated"`
	FullOutputPath string          `json:"full_output_path,omitempty"`
	ExitCode       int             `json:"exit_code"`
	TimedOut       bool            `json:"timed_out,omitempty"`
	Canceled       bool            `json:"canceled,omitempty"`
	WallTimeSec    float64         `json:"wall_time_seconds"`
	NestedCalls    int             `json:"nested_calls"`
	StoreError     string          `json:"store_error,omitempty"`
	ScriptError    string          `json:"script_error,omitempty"`
}

// writeStructured 写结构化结果。
//
// 引擎只在工具声明了 OutputSchemaProvider 时回读这个槽（我们是），模型路径仍读文本 ——
// 结构化结果的消费者是宿主（spill 路径点名、指标）与将来的 tool_search。
func (b *bridge) writeStructured(ctx context.Context, res execproc.RunResult, in structInfo) {
	sink := tools.StructuredSinkFrom(ctx)
	if sink == nil {
		return // 直接调用（单测/别的宿主）时没有槽：不是错误
	}
	out := codemodeResult{
		OK:             in.ok,
		Value:          res.Value,
		Truncated:      in.truncated,
		FullOutputPath: in.spillPath,
		ExitCode:       res.ExitCode,
		TimedOut:       b.clock.expired() || res.TimedOut,
		Canceled:       !b.clock.expired() && res.Canceled,
		WallTimeSec:    in.wall.Seconds(),
		NestedCalls:    in.nested,
		StoreError:     in.storeErr,
		ScriptError:    firstLineOf(execprocErrorLine(res)),
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return // 字段全可序列化；真编不出来就退回文本路径（不影响结果正确性）
	}
	sink.Set(raw)
}

// writeExecStatus 上报执行结局（ExecSink）：引擎据此把「失败/超时/取消」升级为
// IsError，同时**保留完整结果文本**。
//
// 为什么不能靠 Call 返回 error：engine.execute 在 err != nil 时只用 err.Error() 作为
// 结果文本（engine.go 的 callTool 分支），脚本输出与已发生的调用清单会被整段丢掉 ——
// 那恰恰是模型最需要看到的东西（副作用不回滚，模型必须知道做了什么）。
func (b *bridge) writeExecStatus(ctx context.Context, res execproc.RunResult, killed, failed bool) {
	sink := tools.ExecSinkFrom(ctx)
	if sink == nil {
		return
	}
	code := res.ExitCode
	if killed {
		code = -1
	}
	// store 落盘被拒：脚本进程本身退出码是 0，但这次调用对模型是**失败**的
	//（写入没生效，模型不能当它成功）。结构化结果里另有 store_error 与真实 exit_code，
	// 两者不是一回事：一个是「脚本怎么结束的」，一个是「这次调用有没有兑现」。
	if failed && code == 0 {
		code = 1
	}
	// timed_out / canceled 互斥（sandbox.ExecSpec 的既有语义）：宿主墙钟到点是**超时**，
	// 外部 ctx 取消才是取消。
	sink.SetExit(execCommandLabel, code, res.TimedOut || killed, res.Canceled && !killed)
}

// execprocErrorLine 从结果文本里摘出脚本报错的首行（结构化结果的 script_error 字段）。
func execprocErrorLine(res execproc.RunResult) string {
	const marker = "[script error]"
	i := strings.Index(res.Text, marker)
	if i < 0 {
		return ""
	}
	return firstLineOf(strings.TrimSpace(res.Text[i+len(marker):]))
}

// ---- ctx 与文本工具 ----

// jsonText 把字符串编成 JSON 字符串字面量（ScriptResult.Value 的文本形态）。
func jsonText(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`"<unencodable>"`)
	}
	return b
}

// jsonResult 把回执编成 JSON（保留操作的成功回执）。
func jsonResult(v any) execproc.ScriptResult {
	raw, err := json.Marshal(v)
	if err != nil {
		return scriptError(err)
	}
	return execproc.ScriptResult{Value: raw}
}

// scriptError 把一段可读原因变成脚本侧的 reject（脚本可 catch）。
func scriptError(err error) execproc.ScriptResult {
	return execproc.ScriptResult{Value: jsonText(err.Error()), IsError: true}
}

// errText 错误文本（nil-safe）。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// namespaceNameOfRaw 用 raw 名取 namespace（快照里没有实例时返回 ""）。
func namespaceNameOfRaw(tl tools.Tool) string {
	if tl == nil {
		return ""
	}
	return namespaceNameOf(tl)
}

// runIDOf 取当前运行的 RunId（spill 文件名用；无 ToolContext 时为空）。
func runIDOf(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if tc := events.ToolContextFrom(ctx); tc != nil {
		return tc.RunId
	}
	return ""
}

// searchTerms 查询分词：小写 + 按非 [字母/数字] 切分（下划线/连字符/点都算分隔符，
// 于是 `read_file` 与 `read-file` 都能用 `read` 命中）。
func searchTerms(q string) []string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// spillFile 把未截断全文落盘并返回路径（失败 = ""）。
//
// 约定（§15.4 的硬要求）：0600、工作区之外（系统临时目录下的 0700 目录，同实例复用）、
// 命名沿用本仓 spill 先例 `spill-<runId>-<n>.txt`。不删文件：路径已经交给模型与宿主，
// 删掉等于给死链（同 bash 的取舍）。
func (t *Tool) spillFile(runID, full string) string {
	dir := t.spillDir()
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, t.nextSpillName(runID))
	if err := os.WriteFile(path, []byte(full), spillFileMode); err != nil {
		return ""
	}
	return path
}

// spillDir 惰性建 spill 目录（0700）；失败返回 ""（spill 是优化不是正确性）。
func (t *Tool) spillDir() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.spill != "" {
		return t.spill
	}
	dir, err := os.MkdirTemp("", spillDirPrefix)
	if err != nil {
		return ""
	}
	t.spill = dir
	return dir
}

// nextSpillName 生成落盘文件名（spill-<runId>-<n>.txt；无 runId 时省略该段）。
func (t *Tool) nextSpillName(runID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spillN++
	if runID = sanitizeRunID(runID); runID == "" {
		return fmt.Sprintf("spill-%d.txt", t.spillN)
	}
	return fmt.Sprintf("spill-%s-%d.txt", runID, t.spillN)
}

// sanitizeRunID 只保留文件名安全字符（runId 来自事件层，可能带路径分隔符）。
func sanitizeRunID(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// sniffImageMIME 只认四种格式（png/jpeg/gif/webp）—— 与本仓 read_file 的图片路径同一组
// （webp/heic/bmp 之类要么不可解码、要么在视觉模型侧不被接受，宁可在脚本里就失败）。
func sniffImageMIME(data []byte) (string, bool) {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png", true
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg", true
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif", true
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp", true
	}
	return "", false
}

// ---- 墙钟（宿主 owner，审批等待期间暂停）----

// wallClock 宿主侧墙钟：codemode 是唯一的 owner（给 ExecuteScript 传 Timeout<0）。
//
// 为什么不让沙箱管：只有宿主能实现「审批等待期间暂停墙钟」—— 沙箱侧的固定 deadline
// 天然做不到（§7.1-2），而 30s 墙钟 + 一次人工确认 = 脚本必死、副作用却已发生、模型
// 重试再审批（设计文档 §16 的那条链条）。
//
// 暂停的粒度：`pausedSince` 只在**第一次** pause 时置位、**最后一次** resume 时清零 ——
// 于是并发审批（Promise.all 里的两个写类调用同时等人确认）算的是暂停区间的**并集**，
// 而不是各段之和。
type wallClock struct {
	mu          sync.Mutex
	limitD      time.Duration
	start       time.Time
	pauses      int
	pausedSince time.Time
	pausedTotal time.Duration
	isExpired   bool

	stopOnce sync.Once
	done     chan struct{}
	wake     chan struct{}
}

func newWallClock(limit time.Duration) *wallClock {
	return &wallClock{
		limitD: limit,
		start:  time.Now(),
		done:   make(chan struct{}),
		wake:   make(chan struct{}, 1),
	}
}

// watch 起看门协程：预算到点就 cancel（= 杀沙箱 + 级联取消在途子调用）。
//
// 循环顶部**重新计算**剩余量而不是「睡一次就开火」：睡眠期间可能刚被 pause（审批开始了），
// 那时剩余量是冻结的，绝不能判超时。pause/resume 还会 wake 一次，让剩余量立刻重算
// （否则暂停期间的长睡眠会在 resume 后多睡一截）。
func (c *wallClock) watch(ctx context.Context, cancel context.CancelFunc) {
	if c.limitD <= 0 {
		return // 不限时（调用方未声明且默认值异常时也不该自杀）
	}
	go func() {
		for {
			d := c.remaining()
			if d <= 0 {
				c.mu.Lock()
				c.isExpired = true
				c.mu.Unlock()
				cancel()
				return
			}
			t := time.NewTimer(d)
			select {
			case <-c.done:
				t.Stop()
				return
			case <-ctx.Done(): // 外部取消（用户中断/会话关闭）：不是墙钟到点
				t.Stop()
				return
			case <-c.wake:
				t.Stop()
			case <-t.C: // 到点也要回到循环顶部复核（可能刚被暂停）
			}
		}
	}()
}

// stop 收尾看门协程（幂等）。
func (c *wallClock) stop() { c.stopOnce.Do(func() { close(c.done) }) }

// pause 进入「等人类」区间（审批注册/排队/等待）。可重入（计数）。
func (c *wallClock) pause() {
	c.mu.Lock()
	c.pauses++
	if c.pauses == 1 {
		c.pausedSince = time.Now()
	}
	c.mu.Unlock()
	c.signal()
}

// resume 离开「等人类」区间。
func (c *wallClock) resume() {
	c.mu.Lock()
	if c.pauses > 0 {
		c.pauses--
		if c.pauses == 0 {
			c.pausedTotal += time.Since(c.pausedSince)
			c.pausedSince = time.Time{}
		}
	}
	c.mu.Unlock()
	c.signal()
}

// signal 唤醒看门协程重算剩余量（缓冲 1，不阻塞）。
func (c *wallClock) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// expired 是否已因墙钟预算到点被杀。
func (c *wallClock) expired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isExpired
}

// remainOrZero 计算**有效**剩余量（不含暂停区间）。
func (c *wallClock) remaining() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limitD - c.usedLocked()
}

// usedLocked 有效已用时长（不含暂停区间）。调用方须持锁。
func (c *wallClock) usedLocked() time.Duration {
	used := time.Since(c.start) - c.pausedTotal
	if c.pauses > 0 && !c.pausedSince.IsZero() {
		used -= time.Since(c.pausedSince)
	}
	if used < 0 {
		used = 0
	}
	return used
}

// wallElapsed 真实墙钟耗时（含审批等待）—— 进表头的「Wall time」与结构化结果。
func (c *wallClock) wallElapsed() time.Duration { return time.Since(c.start) }

// limit 墙钟预算（结局声明的措辞用）。
func (c *wallClock) limit() time.Duration { return c.limitD }

// ---- 审批等待暂停墙钟的接线 ----

// pausingApprover 包一层 events.Approver：审批（含 session 级 FIFO 排队）期间暂停墙钟。
//
// 为什么是两个包装类型而不是「一律实现两个接口」：ApprovalPolicy 是**可选**接口，引擎在
// Approver 实现它时会用它**覆盖工具自声明**（preApprove）。无脑实现它等于给所有工具
// 关掉自声明审批（返回 false）—— 那是权限降级，不是无害的包装。ContextualApprover 则
// 可以无条件实现：内层不是 contextual 时退化成 BeginApproval，与引擎的非 contextual
// 分支逐字等价。
type pausingApprover struct {
	inner events.Approver
	clock *wallClock
}

// pausingPolicyApprover 内层同时实现 ApprovalPolicy 时的包装（原样透传策略）。
type pausingPolicyApprover struct {
	*pausingApprover
	policy events.ApprovalPolicy
}

func (a *pausingPolicyApprover) NeedsApproval(call core.ToolCall) bool {
	return a.policy.NeedsApproval(call)
}

func (a *pausingApprover) BeginApproval(call core.ToolCall) func(context.Context) (bool, error) {
	a.clock.pause()
	defer a.clock.resume()
	return a.wrapWait(a.inner.BeginApproval(call))
}

func (a *pausingApprover) BeginApprovalWithContext(ctx context.Context, call core.ToolCall) (func(context.Context) (bool, error), error) {
	// 注册本身也可能阻塞（session 级审批 FIFO：非队首请求要在这里排队）—— 排队同样是
	// 「等人类」，同样不吃脚本预算。
	a.clock.pause()
	defer a.clock.resume()
	if ca, ok := a.inner.(events.ContextualApprover); ok {
		wait, err := ca.BeginApprovalWithContext(ctx, call)
		if err != nil {
			return nil, err
		}
		return a.wrapWait(wait), nil
	}
	return a.wrapWait(a.inner.BeginApproval(call)), nil
}

// wrapWait 把「等人类决策」的那一段也包进暂停区间。
func (a *pausingApprover) wrapWait(wait func(context.Context) (bool, error)) func(context.Context) (bool, error) {
	if wait == nil {
		return nil
	}
	return func(ctx context.Context) (bool, error) {
		a.clock.pause()
		defer a.clock.resume()
		return wait(ctx)
	}
}

// pausingCtx 把墙钟暂停钩子接进工具上下文。
//
// 为什么插在 Approver 上而不是监听 ToolApprovalRequested 事件：事件只在审批**发起**时
// 发一次，事件与「等待结束」之间没有配对信号 —— 按事件暂停只能等到整次子调用结束才恢复，
// 于是工具的**执行**时间也被算进暂停区间（脚本预算被悄悄放大）。包 Approver 是唯一
// 能精确框住「等人类」那段区间的接法。
func pausingCtx(ctx context.Context, clock *wallClock) context.Context {
	tc := events.ToolContextFrom(ctx)
	if tc == nil || tc.Approver == nil {
		return ctx // 无审批通道（framework 外调用/单测）：没有等待要暂停
	}
	var wrapped events.Approver
	if policy, ok := tc.Approver.(events.ApprovalPolicy); ok {
		wrapped = &pausingPolicyApprover{
			pausingApprover: &pausingApprover{inner: tc.Approver, clock: clock},
			policy:          policy,
		}
	} else {
		wrapped = &pausingApprover{inner: tc.Approver, clock: clock}
	}
	return events.WithToolContextForRunTask(ctx, tc.RunId, tc.ParentRunId, tc.TaskId,
		tc.Depth, tc.Handler, wrapped, tc.Hooks)
}

// 编译期断言：包装类型必须同时满足两种审批接口的多态路径。
var (
	_ events.Approver           = (*pausingApprover)(nil)
	_ events.ContextualApprover = (*pausingApprover)(nil)
	_ events.Approver           = (*pausingPolicyApprover)(nil)
	_ events.ApprovalPolicy     = (*pausingPolicyApprover)(nil)
)
