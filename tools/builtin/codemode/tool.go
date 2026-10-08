package codemode

// tool.go：codemode 工具本体（Wave 2）—— 元数据、可选接口、工具目录投影。
//
// 主流程在 bridge.go（Call 的骨架：解析 → 建目录 → 建沙箱 → 分派 → 收尾），
// 协议与 scaffold 在 protocol.go，描述生成/store 在 descriptions.go / store.go。
//
// # 这个工具为什么长这样（四条不可协商的性质，改之前先读）
//
//  1. `Exposure() = model-only`：它**声明给模型**，但**永不进脚本的工具表** ——
//     禁自嵌套（pi 的原话 "Scripts must not start other scripts"，tool.ts:424）。
//     一道闸在目录表（BuildCatalog 不列它）、一道在引擎（ExecuteOne 的暴露档守卫）。
//  2. `ToolTimeout() = -1`：生命周期自管 —— 墙钟 owner 是**宿主**（ExecScope 那一路），
//     只有宿主能做到「审批等待期间暂停墙钟」（IMPLEMENTATION-SPEC §7.1-2）。
//     引擎侧再叠一层超时会与它打架（引擎默认 5min，而本工具的预算是 @options 的
//     timeout_ms、默认 30s）。
//  3. `SkipTruncateProvider() = true`：输出预算自管（max_output_tokens × 4 字节 +
//     头尾截断 + spill）。引擎那 20 KB 无条件截断会把「脚本已经按模型要求筛过」的
//     输出再砍一刀（设计文档 §4.2 阻断项 B）。
//  4. `LoadoutProvider`：mode:"on" 的定义就是「给**已声明**工具的描述追加一句『它也能
//     从脚本里调』」（对齐 pi prepareLoadout，tool.ts:382-386）。没有它，模型看不到
//     read_file 也能写进脚本 —— codemode 的收益直接打折。
//
// # 描述为什么必须字节稳定
//
// provider 的前缀缓存按字节匹配；codemode 把「其他工具的存在」压进自己的 description
// （目录），任何随工具集合抖动的写法都会击穿整段缓存（pi #10212）。故本文件的每条
// 渲染路径都只依赖「这一个工具自己」+ 排序后的集合，且 SetCatalog 未选中者不入描述。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/execproc"
	"github.com/seven7628/hai-harness/tools"
)

// Mode codemode 的工作模式（对齐 pi settings.md:41 的 codemode.mode）。
type Mode string

const (
	// ModeOn 已声明工具保持声明（进模型工具表），其描述追加「也可从脚本调用」片段。
	ModeOn Mode = "on"
	// ModeOnly 已声明工具**对模型隐藏**，全部经脚本触达；描述改列全部可编排名。
	//
	// Wave 2 只兑现「描述改列全部」这一半；「对模型隐藏」需要 ToolParams 侧的
	// hiddenDeclarations 语义（pi prepareLoadout 的第二个返回值），本仓尚未接
	// —— 见 P2-d。装配方现在**不要**用 ModeOnly。
	ModeOnly Mode = "only"
)

// codemodeDeclarationFormat mode:"on" 追加给已声明工具的片段。
//
// 字节稳定的前提：%s 只依赖**该工具自己的 raw 名**（scriptNameFor 是纯函数），
// 不依赖集合里的其他成员 —— MCP 连/断、tool_search 加载、mode 切换都不得改写别条的
// 字节（§18 / pi #10212）。
const codemodeDeclarationFormat = "\n\nAlso callable from a codemode script: " +
	"tools.%s({...}) - same tool, same permissions, same side effects."

// Options 工具的构造参数（宿主装配时注入，仿 run_python 的 PythonRuntime）。
//
// 每个字段都是可选的：零值 Options 也能跑（脚本照常执行，只是没有可调用的工具）。
// 生产装配（Wave 3）至少要给 Engine / Tools / Store / Exec。
type Options struct {
	// Engine 执行引擎：嵌套调用经它走完整管线（审批 → 校验 → 时限 → 记录 → 用量）。
	// nil = 脚本里调不到任何工具（每次调用回一条可读错误），纯计算脚本仍可用。
	Engine tools.ExecuteOneEngine

	// Tools 工具目录来源：**返回全部已注册工具**（含 deferred 与 hidden）。
	// 分层是 BuildCatalog 的职责（按 exposure 判定「可编排 / 可列举」），调用方
	// 不要自己过滤：漏了 deferred 会让 MCP 工具在脚本里不可达（那是它的设计用途），
	// 而 hidden/model-only 会被 BuildCatalog 挡在目录表外。
	//
	// 为什么是注入的函数而不是从引擎枚举：引擎没有「列出全部工具」的面（ToolParams
	// 只回声明给模型的那些），而注册表在装配点（desktop/bridge）手上。注入也让
	// 描述生成可以在没有引擎的单测里跑。
	Tools func() []tools.Tool

	// Namespaces namespace 说明（描述分组标题 + describeTool 的 instructions）。
	// nil = 只用名字。工具自己实现 tools.NamespaceProvider 时以它为准，两者叠加。
	Namespaces func() map[string]*tools.ToolNamespace

	// Mode 当前模式（nil 或返回空串 = ModeOn）。
	Mode func() Mode

	// InlineBudget 目录描述的估算 token 上限（nil = DefaultInlineBudget；
	// 返回 <0 = 不限）。装配方读 codemode.inline_budget 配置。
	InlineBudget func() int

	// Store 会话级 store（OpenStore 的产物；nil = 进程内内存 store：跨调用可见、
	// 不落盘）。脚本内 store()/load() 的权威侧。
	Store *Store

	// Exec 沙箱执行器（nil = 进程内默认执行器：sandbox.NoSandbox + 自动探测 node）。
	// 生产装配应与 bash 共用同一个 sandbox 实例 —— 那是同一套 Seatbelt 策略。
	Exec *execproc.Executor

	// Cwd 脚本工作目录（"" = 执行器默认，通常即工作区根，与 bash/run_python 同锚）。
	Cwd string
}

// Tool codemode 工具。
type Tool struct {
	*tools.BaseTool
	opts Options

	mu     sync.Mutex
	exec   *execproc.Executor // 惰性建（Options.Exec 未给时）
	store  *Store             // 惰性建（Options.Store 未给时 = 进程内内存 store）
	anchor uint64             // 外层调用锚的自增序号（见 nextAnchor）
	spill  string             // spill 目录（惰性建，0700）
	spillN uint64             // spill 文件序号
}

// 编译期断言：本工具声明了哪些可选能力（每一条都有真实消费者）。
var (
	_ tools.ExposureProvider     = (*Tool)(nil)
	_ tools.ToolTimeoutProvider  = (*Tool)(nil)
	_ tools.SkipTruncateProvider = (*Tool)(nil)
	_ tools.LoadoutProvider      = (*Tool)(nil)
	_ tools.OutputSchemaProvider = (*Tool)(nil)
	_ tools.Tool                 = (*Tool)(nil)
)

// New 构造工具。opts 全零也合法（见 Options 注释）。
func New(opts Options) *Tool {
	t := &Tool{opts: opts}
	t.BaseTool = &tools.BaseTool{
		Name_: ToolName,
		// Description 由 Description() 现算（目录随工具集变化）—— 这里填的只是
		// 「无目录可算」时的兜底，正常路径不会用到它。
		Description_: DESCRIPTION_INTRO,
		Params_: tools.Obj(map[string]any{
			// 文案对齐设计文档 §13（单字段 code）：讲清「是原始 JS 源码」「顶层
			// await/return 合法」「首行可声明预算」。**不许**出现 pi 原文的
			// "no file system, no network access"（§7.1-3：那是谎言），隔离承诺的
			// 实话写在 DESCRIPTION_INTRO 的 Isolation 段里。
			"code": tools.Str("Raw JavaScript source - not JSON, not a quoted string, not a " +
				"markdown code fence. Top-level await and return work, and the value you return " +
				"becomes this tool's result. May start with a " +
				"`// @options: {\"max_output_tokens\": 4000, \"timeout_ms\": 60000}` line."),
		}, "code"),
		// 脚本可以调写类工具，且脚本之间共享 store 与工作区 —— 与写类工具同款保守：
		// 批内**串行**（脚本内仍有真并发，那是 Promise.all 的事）。对齐 pi 对 codemode
		// 的 FIFO 限流（execute.ts:90-103）的保守面。
		CanParallel_: false,
		ReadOnly_:    false,
	}
	return t
}

// ValidParams 参数校验（引擎在 Call 之前调）。
//
// 只做「信封」这一层：args 是不是对象、code 在不在。**不**在这里解析 @options 行 ——
// 那是 Call 的第一步（解析失败同样是「不进沙箱」，但错误文案由 ParseOptionsLine 给，
// 它对近似写法（漏冒号/全角冒号）有专门的提示，在这儿重复只会分叉成两套说法）。
func (t *Tool) ValidParams(_ context.Context, _ string, arguments string) error {
	var in input
	if err := json.Unmarshal([]byte(arguments), &in); err != nil {
		return fmt.Errorf("codemode: %w", err)
	}
	if strings.TrimSpace(in.Code) == "" {
		return errors.New("codemode: code is required")
	}
	return nil
}

// Exposure 声明给模型，但**永不进脚本的工具表**（禁自嵌套）。
func (t *Tool) Exposure() tools.ToolExposure { return tools.ExposureModelOnly }

// ToolTimeout -1 = 生命周期自管（豁免引擎默认时限；墙钟 owner 是宿主，见文件头）。
func (t *Tool) ToolTimeout() time.Duration { return -1 }

// SkipTruncate 输出预算自管（max_output_tokens + 头尾截断 + spill），豁免引擎的 20 KB。
func (t *Tool) SkipTruncate() bool { return true }

// OutputSchema 结构化结果契约（宿主的 spill 点名与指标消费；模型路径仍看文本）。
//
// 声明它有一个硬性副作用：引擎**只对声明了本接口的工具**回读 StructuredSink
// （engine.go 的槽位读数），所以 bridge 写进槽里的结构化结果要靠这一行才生效。
func (t *Tool) OutputSchema() any {
	return tools.Obj(map[string]any{
		"ok":               tools.Bool("脚本是否正常收尾且写入全部落盘（false = 脚本报错/超时/取消，或 store 落盘被拒）"),
		"value":            tools.Map{"type": "object", "description": "脚本 return/exit 值的 JSON 原文（无返回值时该字段缺省）"},
		"truncated":        tools.Bool("回给模型的结果文本是否被 max_output_tokens 预算截断"),
		"full_output_path": tools.Str("未截断全文的落盘路径（truncated=true 且落盘成功时有值；落盘失败时为空串）。文件权限 0600 且在工作区之外"),
		"exit_code":        tools.Int("脚本进程的真实退出码（被墙钟/取消杀掉为 -1）"),
		"timed_out":        tools.Bool("是否被 codemode 的墙钟预算（timeout_ms）杀掉"),
		"canceled":         tools.Bool("是否被外部取消（用户中断/会话关闭），与 timed_out 互斥"),
		"wall_time_seconds": tools.Number(
			"本次调用的真实墙钟耗时（秒，含审批等待；墙钟预算只计不含审批等待的部分）"),
		"nested_calls": tools.Int("脚本内发起的工具调用次数"),
		"store_error":  tools.Str("store 落盘被拒时的原因（正常为空串）"),
		"script_error": tools.Str("脚本报错时的首行摘要（正常为空串）"),
	}, "ok", "truncated", "exit_code", "wall_time_seconds", "nested_calls")
}

// Description 模型看到的工具描述 = 教学正文 + deferred 指引 + 当前目录（BuildCatalog 产出）。
//
// 无 ctx 通道（Tool.Description() 没有入参），故目录来源是**无 ctx 的** Options.Tools
// —— 这也是那些 provider 刻意不接 ctx 的原因（描述可以在装配点随时现算）。
//
// 出错时（BuildCatalog 的硬失败：名字重复、namespace 归一撞名…）不能返回空串：
// 空描述会让模型看到一个「没有说明的工具」而不是一个错误。这里退化成「正文 + 一句
// 说明」，并让 Call 侧硬失败（那里有 error 通道）。
func (t *Tool) Description() string {
	cat, err := t.catalog()
	if err != nil {
		return DESCRIPTION_INTRO + "\n\n(the tool catalog is unavailable: " + err.Error() + ")"
	}
	return cat.Description
}

// PrepareLoadout 实现 tools.LoadoutProvider：mode:"on" 时给**已声明**工具的描述追加
// 「本工具也可从脚本调用」片段。
//
// 三条纪律：
//   - 只给**脚本里真能调到**的工具追加（tools.CallableFromScript 是唯一真源）：
//     给 model-only / hidden 的条目写「也能从脚本调」是纯谎话，而 codemode 自己就是
//     model-only —— 于是它天然不会被追加（不需要特判名字）；
//   - 片段只依赖**该条自己**的 raw 名（字节稳定，见 codemodeDeclarationFormat）；
//   - 不改 Name / Parameters，也不新增/删除条目（只改 Description）。
//
// mode 不是 "on" 时返回 nil（= 不改写）。
func (t *Tool) PrepareLoadout(_ context.Context, declared []core.ToolSchema) []core.ToolSchema {
	if t.mode() != ModeOn {
		return nil
	}
	snap := t.snapshot()
	out := make([]core.ToolSchema, 0, len(declared))
	changed := false
	for _, s := range declared {
		tl := snap.byRaw[s.Name]
		if tl == nil || !tools.CallableFromScript(tl) {
			out = append(out, s)
			continue
		}
		s.Description = s.Description + fmt.Sprintf(codemodeDeclarationFormat, scriptNameFor(s.Name))
		changed = true
		out = append(out, s)
	}
	if !changed {
		return nil
	}
	return out
}

// ---- 目录投影（注册表 → descriptions.CatalogTool）----

// toolSnapshot 一次调用/一次描述生成看到的工具目录快照。
type toolSnapshot struct {
	catalog []CatalogTool
	byRaw   map[string]tools.Tool
	ns      map[string]*tools.ToolNamespace
}

// snapshot 投影当前注册表。
//
// 参数顺序为什么在这里就定下来：CatalogTool.Params 的**顺序就是渲染顺序**，而
// JSON Schema 的 properties 是 map（遍历顺序随机）—— 排序必须发生在渲染之前，
// 否则同一工具集两次生成的描述字节不同，前缀缓存当场失效。
func (t *Tool) snapshot() toolSnapshot {
	snap := toolSnapshot{byRaw: map[string]tools.Tool{}, ns: map[string]*tools.ToolNamespace{}}
	if t.opts.Namespaces != nil {
		for name, ns := range t.opts.Namespaces() {
			if name != "" {
				snap.ns[name] = ns
			}
		}
	}
	if t.opts.Tools == nil {
		return snap
	}
	for _, tl := range t.opts.Tools() {
		if tl == nil {
			continue
		}
		name := tl.Name()
		if name == "" || name == ToolName {
			continue // codemode 自己不进目录（model-only，且列举自己在语义上是循环）
		}
		if _, dup := snap.byRaw[name]; !dup {
			// byRaw 取先到的那一个（只用于读 CanParallel/namespace/摘要等元数据；
			// 真正执行走引擎按**名字**查找，与这里的取舍无关）。
			snap.byRaw[name] = tl
		}
		// 但仍然**全部**交给 BuildCatalog：raw 名重复是「注册表坏了」，它对此硬失败
		// （不静默取舍）—— 在这里悄悄去重等于把一条硬错误变成「描述里少一条」。
		snap.catalog = append(snap.catalog, CatalogTool{
			Name:      name,
			Summary:   oneLine(tl.Description(), maxEntrySummaryChars),
			Params:    paramsOf(tl.Parameters()),
			Namespace: namespaceNameOf(tl),
			Exposure:  tools.ToolExposureOf(tl),
		})
		if ns := tools.NamespaceOf(tl); ns != nil && ns.Name != "" {
			if _, ok := snap.ns[ns.Name]; !ok {
				snap.ns[ns.Name] = ns
			}
		}
	}
	return snap
}

// catalog 生成描述与命名映射（描述生成 + Call 的目录表共用同一份调用）。
func (t *Tool) catalog() (*Catalog, error) {
	snap := t.snapshot()
	return BuildCatalog(DescribeOptions{
		Tools:      snap.catalog,
		Namespaces: snap.ns,
		// mode:"only" = 已声明工具对模型隐藏，故描述里要把 direct 档也列出来；
		// mode:"on"（默认）= 它们已经在模型工具表里，再列一遍纯烧 3000 token 预算。
		ListDirect:   t.mode() == ModeOnly,
		InlineBudget: t.budget(),
	})
}

// mode 当前模式（未配置/空串 = ModeOn，对齐 pi 的 `mode ?? "on"`）。
func (t *Tool) mode() Mode {
	if t.opts.Mode == nil {
		return ModeOn
	}
	if m := t.opts.Mode(); m != "" {
		return m
	}
	return ModeOn
}

// budget 目录的估算 token 上限（nil = 默认；显式 0 = 一条都不列，<0 = 不限）。
func (t *Tool) budget() *int {
	if t.opts.InlineBudget == nil {
		return nil
	}
	b := t.opts.InlineBudget()
	return &b
}

// scriptNameFor 一个 raw 名在脚本里的**主**名字（纯函数）。
//
// 与 BuildCatalog 的撞名消解同一条主规则：名字本身需要归一（normalize(raw) != raw）
// 就带自己的 fnv32 后缀，否则保持原名。**不**看「谁在场」—— 否则一个无关工具进出就会
// 改写别的条目的名字，描述字节随 MCP 连接抖动（复核实测的那类缺陷）。
//
// 残留冲突（第三方名字恰好长成 read_file_ab12cd）时 BuildCatalog 会用计数器兜底
// （_2/_3），那种情况下本例算出的名字与目录里的最终名可能差一个后缀 —— 这是本函数
// 与 BuildCatalog 允许存在的唯一分歧，代价是「描述里写的脚本名要多试一次」，
// 远小于「为了极端命名让片段字节随工具集变化」的代价。
func scriptNameFor(raw string) string {
	id := normalize(raw)
	if id != raw {
		return collisionSuffix(id, raw)
	}
	return id
}

// namespaceNameOf 工具所属 namespace 名（未实现 provider / 无名 = ""，即默认组）。
func namespaceNameOf(tl tools.Tool) string {
	if ns := tools.NamespaceOf(tl); ns != nil {
		return ns.Name
	}
	return ""
}

// paramsOf 把工具的 Parameters()（JSON Schema 片段）投影成确定顺序的参数表。
//
// 认识的形状是 tools.Obj 的产物（{"type":"object","properties":{…},"required":[…]}}）；
// 认不出来的（第三方工具的奇怪 schema）退化成「没有参数」，渲染成 `name()` —— 宁可
// 少写两个参数名，也不要让描述字节随 map 迭代顺序抖动或直接 panic。
func paramsOf(schema any) []Param {
	obj, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	props, ok := obj["properties"].(map[string]any)
	if !ok {
		return nil
	}
	required := map[string]bool{}
	switch r := obj["required"].(type) {
	case []string:
		for _, n := range r {
			required[n] = true
		}
	case []any:
		for _, v := range r {
			if s, ok := v.(string); ok {
				required[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Param, 0, len(names))
	for _, name := range names {
		out = append(out, Param{Name: name, Optional: !required[name]})
	}
	return out
}

// ---- 惰性资源 ----

// executor 沙箱执行器（Options.Exec 未给时建一个默认的）。
func (t *Tool) executor() *execproc.Executor {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.opts.Exec != nil {
		return t.opts.Exec
	}
	if t.exec == nil {
		t.exec = execproc.New(nil, nil)
	}
	return t.exec
}

// storeRef 会话级 store（Options.Store 未给时建一个进程内内存 store：同一次会话里的
// 多次调用仍能互相看见，只是不落盘 —— 对齐 pi 拿不到 appendEntry 时的降级形态）。
func (t *Tool) storeRef() *Store {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.opts.Store != nil {
		return t.opts.Store
	}
	if t.store == nil {
		st, err := OpenStore("")
		if err != nil {
			// OpenStore("") 是纯内存路径，只有「目录不存在」这类检查才会失败 —— 它不可能
			// 触发（path 为空）。真触发了也不能返回 nil：调用方会 panic。
			panic("codemode: 内存 store 建不起来: " + err.Error())
		}
		t.store = st
	}
	return t.store
}

// nextAnchor 生成本次调用的锚（子调用的 ParentCallId 前缀）。
//
// 为什么不用「外层调用 id」：Tool.Call(ctx, name, args) 拿不到 ToolCall.Id，而
// events.ToolContext 里也没有它（只有 RunId）—— 宿主侧唯一存在的身份是 RunId。
// 锚只需满足两件事：会话内唯一、非空（宿主渲染器用 `parent_call_id != ""` 判定
// 「这是嵌套调用，不单独占一行」，见 useAppStore 的 tool_start 契约）。
func (t *Tool) nextAnchor() string {
	t.mu.Lock()
	t.anchor++
	n := t.anchor
	t.mu.Unlock()
	return fmt.Sprintf("%s#%d", ToolName, n)
}

// childID 子调用 id（对齐 pi 的 "<parent>/<n>" 形态：一眼看出归属与序号）。
func childID(anchor string, n int64) string { return fmt.Sprintf("%s/%d", anchor, n) }

// firstLineOf 取一段文本的首行（脚本报错摘要用）。
func firstLineOf(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
