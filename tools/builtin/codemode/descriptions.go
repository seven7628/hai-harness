// Package codemode 实现 codemode 工具：把「调用其他工具」压进一段 JavaScript。
//
// 分波次交付：descriptions.go + store.go 是 Wave 1-B 的**纯逻辑**部分（描述生成、
// 命名映射、脚本首行预算解析、store 持久化）；tool.go / bridge.go / protocol.go 是
// Wave 2 的接线部分。纯逻辑部分刻意不 import execproc、不碰沙箱、不碰注册表 ——
// 全部输入都是普通数据结构，因此可以在没有 node、没有引擎的情况下单测与对拍。
package codemode

// descriptions.go：描述生成（模型看到的工具目录）与命名映射（脚本内 tools.<name>
// ↔ 引擎内 raw 名）。
//
// 为什么描述生成值得单列且必须**字节稳定**：codemode 把「其他工具的存在」压进它
// 自己的 description 里（对齐 pi createCodemodeDescription），而 provider 侧的前缀
// 缓存对 description 的字节抖动极敏感 —— MCP 连/断、tool_search 延迟加载一个工具，
// 都不允许改变**任何其他条目**的字节（pi #10212 那类 bug 的根因就在这条路径）。
// 所以本文件里每一条渲染路径都只依赖「条目自身 + 所属分组」，不依赖集合的其余成员；
// 所有集合遍历都先排序。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/seven7628/hai-harness/tools"
)

const (
	// ToolName codemode 工具名（Wave 2 的 tool.go 用它注册；bridge 也按它过滤自身）。
	ToolName = "codemode"

	// DefaultInlineBudget 目录描述可用的估算 token 上限（对齐 pi
	// DEFAULT_CODEMODE_INLINE_BUDGET，tool.ts:224）。放不下的工具不写进描述，
	// 由脚本内 searchTools() 现查 —— 这是「描述不随工具集膨胀」的闸门。
	DefaultInlineBudget = 3000

	// charsPerToken 估算口径（对齐 pi CHARS_PER_TOKEN，tool.ts:226）：4 字符 ≈ 1 token。
	charsPerToken = 4

	// catalogHeading 目录段标题（工具条目挂在它下面）。
	catalogHeading = "Nested tools:"
	// catalogEmptyNote 一个条目都没列出来时的说明。不能让模型把空目录当成
	// 「脚本调不了任何工具」——预算为 0 或整集都是 deferred 时就是这个形态。
	catalogEmptyNote = "Nested tools: none listed here - use searchTools(query) inside the script to find and call them."

	// entryIndent 条目缩进（视觉上从属于所属分组标题）。
	entryIndent = "  "
	// defaultGroupLabel 无 namespace 的默认组的标题标签。
	defaultGroupLabel = "default"

	// markerSomeOmitted / markerAllOmitted 分组内「有工具没列出来」的提示。
	// pi 同款语义：部分没列 = some，整组都没列 = tools（连一个都没列出来时模型
	// 必须知道这个 namespace 是存在的，否则会以为工具不存在）。
	markerSomeOmitted = " (some tools not listed; use searchTools)"
	markerAllOmitted  = " (tools not listed; use searchTools)"

	// maxEntrySummaryChars 单条摘要的字符上限；maxNamespaceSummaryChars 组说明的上限。
	// 描述是**目录**不是文档：一段多行长文进来，一条就能吃掉整个预算，轮转装箱也
	// 就失去意义（这是 pi 把长使用指引挪去 describeNamespace() 的同一条理由）。
	maxEntrySummaryChars     = 160
	maxNamespaceSummaryChars = 120
)

// 脚本首行 `// @options:` 的取值边界。
const (
	// DefaultScriptOutputTokens 未声明 max_output_tokens 时的默认值（对齐 pi
	// execute.ts:130 = 10000 token ≈ 40 KB）。
	DefaultScriptOutputTokens = 10000
	// MaxScriptOutputTokens max_output_tokens 上限：脚本产物最终要回给模型，
	// 20 万 token（≈ 800 KB）已远超任何模型的输出预算，再往上只会让 spill 文件
	// 更大、更慢，而不会有任何一条能被读完。
	MaxScriptOutputTokens = 200000
	// MaxScriptTimeoutMs timeout_ms 上限 = 1 小时。pi 允许到 2147483647；本仓收紧：
	// 脚本是「一次工具调用」，跑满 1 小时以上基本只能是写错了，而时限越大，
	// 用户中断前被卡住的时间也越长（审批等待期不算墙钟，见 §7.1-2）。
	MaxScriptTimeoutMs = 3600000
	// minScriptTimeoutMs timeout_ms 下限（0 无意义：0 毫秒的脚本预算不是「不限时」，
	// 而是必然失败，pi 的取值域同样从 1 开始）。
	minScriptTimeoutMs = 1
)

// optionsLinePrefix 首行预算声明的前缀（允许前导空格/Tab，同 pi 文法
// OPTIONS_LINE: /[ \t]*\/\/ @options:[^\r\n]*/）。
const optionsLinePrefix = "// @options:"

// DESCRIPTION_INTRO 给模型的教学正文（英文：模型面文本，与本仓代码注释语言无关）。
//
// 契约（改动前先读）：
//  1. **绝不承诺做不到的隔离**。本仓 Phase 2 不加 node --permission（§7.1-3），
//     所以不许出现 pi 原文的 "no file system, no network access" —— 那是谎言，
//     模型据此把危险操作当成无害，代价比不写隔离承诺大得多。只说实话：
//     独立进程 + 环境白名单 + 配额，并指明唯一受支持的对外通道是 tools.*。
//  2. 只描述 Wave 1-A prelude 与 Wave 2 tool.go 真正会装上的全局
//     （tools / text / image / exit / console / store / load / searchTools /
//     describeTool）；新增/删除全局必须同步改这里。
//  3. 这段文本进的是**每次请求都带的工具 schema**，属于前缀缓存的一部分：
//     改一个字就击穿一次全量缓存，所以只在契约变化时改，别做「顺手润色」。
//  4. 预算上它和目录共享 DefaultInlineBudget，长度受 TestDescriptionIntroBudget 守护。
const DESCRIPTION_INTRO = `Run JavaScript that calls other tools: chain them, loop over them, run independent
calls concurrently, and filter large results down to what you need instead of issuing
many separate tool calls.

Pass raw JavaScript source - not JSON, not a quoted string, not a markdown code fence.
Top-level "await" and "return" work, and the value you return becomes this tool's result.

Calling tools:
- tools.<name>(args) calls a tool; args is one object. Names below are in script form
  (characters that cannot appear in a JavaScript identifier become "_"); the engine's
  original name works too: tools["mcp__some-server__some-tool"](args).
- A call resolves to the tool's structured value when it has one, otherwise to its text;
  a failed call rejects, so catch it if you want to keep going.
- Independent calls run together with await Promise.allSettled([...]).
- Calls are real: they have exactly the side effects of a direct tool call and are NOT
  rolled back if the script later fails, times out, or is cancelled. Do read-only work
  first, keep mutations last, and check each result before assuming it applied.
- When the script ends, calls that are still running are cancelled and unawaited
  promises are discarded - await everything you start.

Budget line (optional, must be the first line of the script):
  // @options: {"max_output_tokens": 4000, "timeout_ms": 60000}
max_output_tokens caps the output sent back to you (0-200000, default 10000);
timeout_ms caps wall-clock time (1-3600000, at most 1 hour; default: unlimited).
Only these two fields are accepted - anything else fails the call before the script runs.

Helpers:
- text(x) and image(path) add text or an image to the result, exit(msg) ends the script
  early, and console.log(...) is collected into the output.
- store(key, value) and load(key) / load() carry JSON values between codemode calls in
  this session: what one call stores, the next call can load. One value is limited to
  256 KiB of JSON and the whole store to 1 MiB; exceeding either fails the call. Use
  store(key, undefined) to delete a key. Writes are kept only when the script finishes
  successfully - a failed script discards all of its writes.
- searchTools(query) and describeTool(name) find tools that are not listed below, so this
  catalog is never the complete list of what you can call.

Isolation: the script runs in its own process with an allowlisted environment and enforced
quotas (memory, wall clock, output size). The supported way to reach anything outside that
process is tools.*.`

// DEFERRED_TOOLS_GUIDANCE 有 deferred 档工具时追加的指引（对齐 pi
// DEFERRED_TOOLS_GUIDANCE，tool.ts:220-221）。
//
// 刻意**不列名字**：MCP 工具一旦被列举进描述，连接/断开一个 server 就会改写描述、
// 击穿前缀缓存，这正是 pi #10212 的坑。这里只教「去 searchTools 现查」。
const DEFERRED_TOOLS_GUIDANCE = `Some tools are not listed above on purpose (for example tools from servers that are
connected on demand). Call searchTools(query) inside the script to find and call them.`

// reservedGlobals 沙箱全局白名单（对齐 pi prelude 的 RESERVED_GLOBALS）。
// namespace 归一后命中其一 → 直接报错：否则会在脚本里遮蔽 store()/tools 这类全局，
// 模型写 store(...) 实际调到别的东西，症状极难查。
// 大小写敏感（JS 标识符本就大小写敏感，Store 不冲突）。
var reservedGlobals = map[string]bool{
	"tools": true, "ALL_TOOLS": true, "text": true, "image": true, "exit": true,
	"store": true, "load": true, "console": true, "globalThis": true,
	"searchTools": true, "describeTool": true, "describeNamespace": true,
}

// ScriptOptions 脚本首行 `// @options: {...}` 声明的执行预算。
//
// 与 Wave 2 tool.go 里「工具构造参数 Options」刻意不同名：同一个 package 内不得重名，
// 且两者语义无关（一个是脚本声明，一个是宿主装配参数）。
type ScriptOptions struct {
	// MaxOutputTokens 模型可见输出预算（token）。nil = 未声明 → DefaultScriptOutputTokens。
	MaxOutputTokens *int
	// TimeoutMs 脚本墙钟上限（毫秒）。nil = 未声明 → 不限时（宿主 ExecScope 管墙钟，
	// 见 §7.1-2：沙箱侧固定 deadline 做不到「审批等待期暂停墙钟」）。
	TimeoutMs *int
}

// OutputTokens 生效的输出预算（未声明 → DefaultScriptOutputTokens）。
func (o ScriptOptions) OutputTokens() int {
	if o.MaxOutputTokens != nil {
		return *o.MaxOutputTokens
	}
	return DefaultScriptOutputTokens
}

// Timeout 生效的墙钟上限；0 = 不限时（由宿主 ExecScope 兜底）。
func (o ScriptOptions) Timeout() time.Duration {
	if o.TimeoutMs == nil {
		return 0
	}
	return time.Duration(*o.TimeoutMs) * time.Millisecond
}

// ParseOptionsLine 严格解析脚本首行的 `// @options: {...}`，
// 返回（声明、剥离首行后的脚本正文、error）。
//
// 严格性对齐 pi parseCodemodeSource —— 非法即**报错**（工具调用失败，不进沙箱），
// 绝不静默忽略：
//   - 只认第一行，且必须是 `// @options:` 前缀（容忍前导空格/Tab）；不匹配 = 没有声明，
//     正文原样返回（逐字节相同）。
//   - 只接受 max_output_tokens / timeout_ms 两个字段，其他字段一律报错。理由：模型
//     写 {"maxOutputTokens": 4000} 这类驼峰名时，静默忽略会变成「预算没生效但脚本
//     照样跑、照样烧完内存」，比直接失败难查得多。
//   - 值必须是十进制整数（拒绝 1.5 / 1e3 / "4000" / true / null），且在
//     [0, MaxScriptOutputTokens] / [1, MaxScriptTimeoutMs] 内。
//   - 行尾 JSON 之后不允许再有内容（严格解码，不留尾巴）。
func ParseOptionsLine(code string) (ScriptOptions, string, error) {
	first, rest, _ := strings.Cut(code, "\n")
	trimmed := strings.TrimLeft(first, " \t")
	if !strings.HasPrefix(trimmed, optionsLinePrefix) {
		return ScriptOptions{}, code, nil
	}
	raw := strings.TrimSpace(trimmed[len(optionsLinePrefix):])
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return ScriptOptions{}, "", fmt.Errorf("codemode: %s line is not a JSON object (%v)", optionsLinePrefix, err)
	}
	if fields == nil {
		// `null` 也能 unmarshal 进 map（得到 nil map），但它不是对象：
		// 静默当成「没声明预算」会让模型以为设置了预算而实际没有。
		return ScriptOptions{}, "", fmt.Errorf("codemode: %s line is not a JSON object (%s)", optionsLinePrefix, raw)
	}
	var out ScriptOptions
	// 排序后校验：报错文案不随 map 迭代顺序变（模型与测试都该看到确定的错误）。
	for _, field := range sortedKeys(fields) {
		value := fields[field]
		switch field {
		case "max_output_tokens":
			n, err := intField(field, value)
			if err != nil {
				return ScriptOptions{}, "", err
			}
			if n < 0 || n > MaxScriptOutputTokens {
				return ScriptOptions{}, "", fmt.Errorf(
					"codemode: max_output_tokens must be in [0, %d], got %d", MaxScriptOutputTokens, n)
			}
			v := int(n)
			out.MaxOutputTokens = &v
		case "timeout_ms":
			n, err := intField(field, value)
			if err != nil {
				return ScriptOptions{}, "", err
			}
			if n < minScriptTimeoutMs || n > MaxScriptTimeoutMs {
				return ScriptOptions{}, "", fmt.Errorf(
					"codemode: timeout_ms must be in [%d, %d] (at most 1 hour), got %d",
					minScriptTimeoutMs, MaxScriptTimeoutMs, n)
			}
			v := int(n)
			out.TimeoutMs = &v
		default:
			return ScriptOptions{}, "", fmt.Errorf(
				"codemode: unsupported option %q (allowed: max_output_tokens, timeout_ms)", field)
		}
	}
	return out, rest, nil
}

// intField 严格取整数字面量：只接受 JSON 数字（拒绝 "1000" 这类字符串、1.5、1e3、
// true、null）。不能直接 Unmarshal 到 json.Number —— json.Number 的底层是 string，
// 编码器会把 JSON 字符串也塞进去（"1000" 会被当成合法的 1000）。
func intField(field string, raw json.RawMessage) (int64, error) {
	fail := func(got string) error {
		return fmt.Errorf("codemode: option %q must be an integer, got %s", field, got)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return 0, fail(string(raw))
	}
	num, ok := v.(json.Number)
	if !ok {
		return 0, fail(string(raw))
	}
	n, err := strconv.ParseInt(num.String(), 10, 64)
	if err != nil {
		return 0, fail(num.String())
	}
	return n, nil
}

// ---- 命名映射（对齐 pi datasheet §15.1 的四步）----

// normalize 把引擎内 raw 工具名归一成合法的 JS 标识符：
//   - 非 [字母|数字|_|$] 的字符 → '_'（主要来源是连字符：mcp__dev-radius__get）；
//   - 首字符不允许数字（"1password"）→ 同样落成 '_'（整体替换而非加前缀，保持
//     「非法字符一律变 _」这条规则的单一语义）。
//
// 归一后仍可能撞名（read-file vs read_file），撞名由 BuildCatalog 用 fnv32 后缀消解。
func normalize(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for i, r := range raw {
		switch {
		case r == '_' || r == '$' || unicode.IsLetter(r):
			b.WriteRune(r)
		case unicode.IsDigit(r) && i > 0:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		// 空名（注册表里不该有，但桥接拿它建键会直接炸）也必须是合法标识符。
		return "_"
	}
	return b.String()
}

// collisionSuffix 撞名后缀：fnv32(raw) 的前 6 位十六进制。
//
// 必须哈希 **raw 名** 而不是归一后的名字：撞名双方的归一结果相同，用归一结果做哈希
// 等于给两边加同一个后缀，撞名照旧（pi #10239 的修法就是这一步）。
func collisionSuffix(id, raw string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(raw))
	return id + "_" + fmt.Sprintf("%08x", h.Sum32())[:6]
}

// ---- 条目与分组 ----

// Param 工具签名里的一个顶层参数（渲染成 name 或 name?）。
type Param struct {
	Name     string
	Optional bool
}

// CatalogTool 一个候选工具。Wave 2 的 tool.go 从注册表投影成这个纯数据结构，
// 本文件不依赖引擎。
type CatalogTool struct {
	// Name 引擎内 raw 名（read_file / mcp__dev-radius__get）。
	Name string
	// Summary 一行摘要（取描述首行，超长会被裁；空则只渲染签名）。
	Summary string
	// Params 顶层参数，**给出的顺序就是渲染顺序**。JSON Schema 的 properties 是
	// map，投影方必须先把它变成确定顺序，否则描述字节会随 map 迭代抖动。
	Params []Param
	// Namespace 所属 namespace；空 = 默认组（排最前）。
	Namespace string
	// Exposure 暴露档位。"" = 未声明 = direct（与未实现 ExposureProvider 的工具同义，
	// 见 tools.ToolExposureOf）；非法值按 tools 包的既定语义处理（= hidden）。
	//
	// 模式（pi prepareLoadout 的 on/only）不在这个结构里 —— 由 DescribeOptions.ListDirect
	// 表达：mode:"on" 时 direct 工具不进目录（它们本来就在模型工具表里）。
	Exposure tools.ToolExposure

	// scriptName 归一 + 撞名消解后的脚本可调用名，由 BuildCatalog 写入。
	// 直接调 SelectCatalog 的调用方没有映射表，退化为单名归一（只影响成本估算的
	// 字节数，不影响正确性）。
	scriptName string
}

// CatalogGroup 一个 namespace 分组（描述里的一个标题块）。
type CatalogGroup struct {
	Namespace   string        // 空 = 默认组
	Description string        // 标题说明（来自 tools.ToolNamespace.Description；Instructions 刻意不进描述）
	Tools       []CatalogTool // 组内候选（本文件不改动入参切片）
}

// asTool 把 CatalogTool 包成最小 tools.Tool，**只**为复用 tools 包的分层判定
// （CallableFromScript / ListedInCodemodeCatalog 的入参是 Tool）。
//
// 为什么绕这一下：五档 exposure 的规则只有一份真源（tools/exposure.go）。codemode 侧
// 再抄一遍 switch，就一定会和它漂移 —— 比如未来新增一档时只改了一边。
//
// 空档位用**不实现 ExposureProvider** 的类型表达：tools.ToolExposureOf 对「未实现接口」
// 的语义是 direct，对「实现了但值非法」是 hidden（保守），两者不能混。
func (t CatalogTool) asTool() tools.Tool {
	base := &catalogStub{BaseTool: &tools.BaseTool{Name_: t.Name, Description_: t.Summary}}
	if t.Exposure == "" {
		return base
	}
	return &catalogExposed{catalogStub: base, exp: t.Exposure}
}

// catalogStub 只有元数据；用指针嵌入 BaseTool（它的方法是指针接收者，值嵌入不会提升）。
type catalogStub struct{ *tools.BaseTool }

func (*catalogStub) Call(context.Context, string, string) (string, error) { return "", nil }
func (*catalogStub) ValidParams(context.Context, string, string) error    { return nil }

type catalogExposed struct {
	*catalogStub
	exp tools.ToolExposure
}

func (c *catalogExposed) Exposure() tools.ToolExposure { return c.exp }

// scriptNameOf 条目在脚本里的可调用名。
func scriptNameOf(t CatalogTool) string {
	if t.scriptName != "" {
		return t.scriptName
	}
	return normalize(t.Name)
}

// ---- 渲染：只依赖条目自身 ----

// estimateTokens 估算一段文本占用的 token（charsPerToken = 4）。
//
// 按**字符数**而不是字节数：中文摘要按字节会算成 3 倍成本，直接挤掉本仓以中文
// 描述为主的工具；pi 的 text.length 也是按字符计的。
//
// 向上取整而不是整除：整除会把 1~3 字符的条目算成 0 token，于是「0 成本条目」
// 可以无限塞，预算永不饱和，轮转装箱的「最便宜的放不下 ⇒ 整组放不下」的推理
// 也会在边界上失真。
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := utf8.RuneCountInString(s)
	return (n + charsPerToken - 1) / charsPerToken
}

// oneLine 取首行并压到上限。描述是目录不是文档：一段多行长文进来，一条就能吃掉
// 整个预算（长使用指引应走 describeNamespace()，不进描述）。
func oneLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" {
		return ""
	}
	if r := []rune(s); len(r) > max {
		s = strings.TrimRight(string(r[:max]), " ") + "..."
	}
	return s
}

// renderEntry 渲染一条工具条目 —— 字节稳定性的基本单位：只依赖这一个条目。
//
// 形如：`  read_file(path, offset?, limit?): Read a file and return its contents.`
func renderEntry(t CatalogTool) string {
	var b strings.Builder
	b.WriteString(entryIndent)
	b.WriteString(scriptNameOf(t))
	b.WriteByte('(')
	for i, p := range t.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p.Name)
		if p.Optional {
			b.WriteByte('?')
		}
	}
	b.WriteByte(')')
	if s := oneLine(t.Summary, maxEntrySummaryChars); s != "" {
		b.WriteString(": ")
		b.WriteString(s)
	}
	b.WriteByte('\n')
	return b.String()
}

// renderGroupHeader 渲染分组标题（含换行）。
//
// omitted/total 决定尾巴：0 → 没有尾巴；partial → some；全部没列 → tools。
// 差异只取决于**本组自己的**条目数，与别的组无关（缓存不变量）。
func renderGroupHeader(ns, desc string, omitted, total int) string {
	label := ns
	if label == "" {
		label = defaultGroupLabel
	}
	var b strings.Builder
	b.WriteByte('[')
	b.WriteString(label)
	b.WriteByte(']')
	if d := oneLine(desc, maxNamespaceSummaryChars); d != "" {
		b.WriteByte(' ')
		b.WriteString(d)
	}
	switch {
	case omitted > 0 && omitted >= total:
		b.WriteString(markerAllOmitted)
	case omitted > 0:
		b.WriteString(markerSomeOmitted)
	}
	b.WriteByte('\n')
	return b.String()
}

// ---- 轮转公平装箱 ----

// SelectResult 轮转公平装箱的结果。
type SelectResult struct {
	Selected   map[string]bool // 被选中的工具 raw 名
	Omitted    map[string]int  // namespace → 因预算未列出的条目数（键 = "" 表示默认组）
	UsedTokens int             // 固定开销 + 被选中条目的估算 token
}

// SelectCatalog 轮转公平装箱（对齐 pi tool.ts:281-298）。
//
// budgetTokens 是整个目录段的估算 token 上限（charsPerToken = 4，向上取整）：
//   - < 0 → 不限（全部列出，对齐 pi「未配置 → 全部列出」的兜底）
//   - 0   → 只列组标题
//
// overheadTokens = 目录段之外的固定开销（INTRO + 指引 + 标题），先扣掉：预算的意义
// 就是「这段描述在上下文里占多少」，只算条目会低估整段描述的真实体积。
//
// 算法：组内按「成本升序、名字升序」排队；每轮每组各出**一个当前最便宜的**条目，
// 放得下就收、放不下该组退出；组标题在收下该组第一条时计费一次。组内成本单调不减，
// 所以「最便宜的放不下 ⇒ 该组其余都放不下」，退出不会漏掉本可放下的小条目。
//
// 不变量（pi 原文的 Every namespace is represented before any namespace is complete）：
// **任何 namespace 都不会在任何 namespace 完成前被整体跳过** —— 每轮每组只取一条，
// 大 namespace 无法用「先把自己的条目填满」把小 namespace 挤到 0 条。
//
// 两个诚实的边界（都有测试钉住）：
//  1. 预算连「每组一条」都放不下时，同轮内按组的先后顺序先到先得（排后面的组可能
//     一条都拿不到）。这不是轮转能解决的 —— 没有预算就是没有预算；能保证的是
//     「有预算时不会让前面的组把后面的组挤到 0 条」，从第二轮的加量开始更是严格轮转。
//  2. 组标题在装箱时按**不带** 「未列出」尾巴的长度计费；最终渲染若带上尾巴，
//     整段描述会比预算多出每个被裁分组约 10 个 token。取舍：宁可能够如实告诉模型
//     「这里还有工具」，也不为了几个 token 把提示吞掉。
//
// 空组（无候选）直接跳过：连标题都不渲染，否则纯烧预算。
func SelectCatalog(groups []CatalogGroup, overheadTokens, budgetTokens int) SelectResult {
	type cand struct {
		tool CatalogTool
		cost int
	}
	type bucket struct {
		ns         string
		headerCost int
		cands      []cand
		charged    bool // 组标题是否已计费
		alive      bool
		idx        int
	}
	res := SelectResult{Selected: map[string]bool{}, Omitted: map[string]int{}}
	used := overheadTokens
	unlimited := budgetTokens < 0

	buckets := make([]*bucket, 0, len(groups))
	for _, g := range groups {
		if len(g.Tools) == 0 {
			continue
		}
		b := &bucket{ns: g.Namespace, alive: true, idx: 0}
		for _, t := range g.Tools {
			b.cands = append(b.cands, cand{tool: t, cost: estimateTokens(renderEntry(t))})
		}
		sort.SliceStable(b.cands, func(i, j int) bool {
			if b.cands[i].cost != b.cands[j].cost {
				return b.cands[i].cost < b.cands[j].cost
			}
			return b.cands[i].tool.Name < b.cands[j].tool.Name
		})
		b.headerCost = estimateTokens(renderGroupHeader(g.Namespace, g.Description, 0, len(g.Tools)))
		buckets = append(buckets, b)
	}

	for {
		progress := false
		for _, b := range buckets {
			if !b.alive {
				continue
			}
			charge := b.cands[b.idx].cost
			if !b.charged {
				charge += b.headerCost
			}
			if !unlimited && used+charge > budgetTokens {
				b.alive = false // 组内成本升序 ⇒ 其余只会更贵
				continue
			}
			used += charge
			b.charged = true
			res.Selected[b.cands[b.idx].tool.Name] = true
			b.idx++
			progress = true
			if b.idx == len(b.cands) {
				b.alive = false
			}
		}
		if !progress {
			break
		}
	}
	for _, b := range buckets {
		if rest := len(b.cands) - b.idx; rest > 0 {
			res.Omitted[b.ns] += rest
		}
	}
	res.UsedTokens = used
	return res
}

// ---- 描述生成 ----

// DescribeOptions BuildCatalog 的入参。
type DescribeOptions struct {
	// Tools 全部候选（含 deferred / hidden）。分层过滤由 BuildCatalog 负责 ——
	// 调用方漏过滤也不会把 deferred/hidden 工具泄漏进描述（双保险，有测试钉住）。
	Tools []CatalogTool
	// Namespaces namespace 标题与说明（nil = 只用名字）。
	// 只取 ToolNamespace.Description；Instructions 刻意不进描述（预算只有 3000）。
	Namespaces map[string]*tools.ToolNamespace
	// ListDirect direct 档（= 已声明给模型）的工具是否也进目录，对齐 pi prepareLoadout
	// 的 mode：pi 默认 mode:"on" 时**不**列举（它们已经躺在模型的工具表里，再列一遍
	// 纯浪费 3000 token 预算），mode:"only" 时列举（模型只拿到 codemode，其余全靠脚本）。
	//
	// 零值 false = pi 的默认行为。模式枚举（Mode/ModeOn/ModeOnly）由 Wave 2 tool.go 定义，
	// 本文件只收「是否列举」这一位，避免同 package 重名。
	//
	// 注意这只影响**列举**：两种取值下 direct 工具都在 Names 映射表里（可编排性由
	// tools.CallableFromScript 决定，与列举无关 —— exposure 表里 callable ≠ declared）。
	ListDirect bool
	// InlineBudget 目录描述的估算 token：nil = DefaultInlineBudget；
	// <0 = 不限（全部列出）；0 = 只列组标题。
	InlineBudget *int
}

// CatalogEntry 一条被列出的条目。
type CatalogEntry struct {
	RawName   string // 引擎内 raw 名
	Name      string // 脚本可调用名（归一 + 撞名后缀后）
	Namespace string
	Line      string // 渲染出的条目行（含换行）—— 「字节不变」的比较单位
	Cost      int    // Line 的估算 token
}

// Catalog 一次描述生成的完整产物。
type Catalog struct {
	Description string              // 描述正文（同输入 ⇒ 同字节）
	Groups      []CatalogGroup      // 被列出的分组（默认组最前，组内名字序）
	Entries     []CatalogEntry      // 扁平化的被列出条目（渲染顺序）
	GroupLines  map[string][]string // namespace → 该组被列出条目的行（不变量测试与诊断用）

	// Names 脚本可调用名 → raw 名（桥接分派表）：含 deferred（可编排但不列举），
	// 不含 hidden/model-only（二者都不可编排）。
	Names map[string]string
	// RawNames raw 名 → 脚本可调用名（tools["<raw>"] 双键注册用）。
	RawNames map[string]string
	// Deferred deferred 档 raw 名（排序；决定是否追加 DEFERRED_TOOLS_GUIDANCE）。
	Deferred []string

	Budget     int // 生效预算（估算 token；-1 = 不限）
	UsedTokens int // 整段描述的估算 token
	Omitted    int // 因预算未列出的条目数
}

// BuildCatalog 生成 codemode 描述 + 命名映射。
//
// 硬失败（返回 error，不静默）：namespace 归一后撞名、namespace 命中沙箱保留全局、
// 工具 raw 名重复、归一 + 后缀后仍撞名。这些都不会「退化着继续跑」——一个静默错名
// 会让模型的调用打到另一个实现上（pi #10239 的形态）。
func BuildCatalog(opts DescribeOptions) (*Catalog, error) {
	budget := DefaultInlineBudget
	if opts.InlineBudget != nil {
		budget = *opts.InlineBudget
	}

	// 1) raw 名去重 + 排序（集合遍历必须有序，否则描述字节随注册顺序抖动）。
	sorted := append([]CatalogTool(nil), opts.Tools...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	seen := make(map[string]bool, len(sorted))
	for _, t := range sorted {
		if t.Name == "" {
			return nil, fmt.Errorf("codemode: 工具名为空")
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("codemode: 工具 raw 名重复: %q（注册期撞名覆盖会让模型调错实现）", t.Name)
		}
		seen[t.Name] = true
	}

	// 2) namespace 校验：归一后撞名 / 命中保留全局，都要在生成描述之前挡下来。
	nsNames := map[string]bool{}
	for name := range opts.Namespaces {
		nsNames[name] = true
	}
	for _, t := range sorted {
		if t.Namespace != "" {
			nsNames[t.Namespace] = true
		}
	}
	ordered := make([]string, 0, len(nsNames))
	for name := range nsNames {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		if reservedGlobals[normalize(name)] {
			return nil, fmt.Errorf(
				"codemode: namespace %q 归一后命中沙箱保留全局 %q（会在脚本里遮蔽它）", name, normalize(name))
		}
	}
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if normalize(ordered[i]) == normalize(ordered[j]) {
				return nil, fmt.Errorf(
					"codemode: namespace %q 与 %q 归一后同名（仅 -/_ 之差）—— 请改掉其一，本工具不做静默取舍",
					ordered[i], ordered[j])
			}
		}
	}

	// 3) 分层：可编排（进映射表）与列举（进目录）是两件事，见 tools/exposure.go 表。
	cat := &Catalog{
		Names:      map[string]string{},
		RawNames:   map[string]string{},
		GroupLines: map[string][]string{},
		Budget:     budget,
	}
	callable := make([]CatalogTool, 0, len(sorted)) // 进映射表（direct/codemode/deferred）
	listed := make([]CatalogTool, 0, len(sorted))   // 进目录（direct[可选]/codemode）
	for _, t := range sorted {
		st := t.asTool()
		if !tools.CallableFromScript(st) {
			continue // model-only（codemode 自己）/ hidden：既不可编排也不列举
		}
		callable = append(callable, t)
		if !tools.ListedInCodemodeCatalog(st) {
			// deferred：可编排、永不列举（否则 MCP 工具会重新撑爆描述 —— pi #10212）。
			cat.Deferred = append(cat.Deferred, t.Name)
			continue
		}
		if tools.ToolExposureOf(st) == tools.ExposureDirect && !opts.ListDirect {
			// direct 且非 mode:"only"：模型工具表里已经有了，不重复列举。
			// （空档位经 asTool 的 catalogStub 也是 direct —— 与未声明档位的工具同义。）
			continue
		}
		listed = append(listed, t)
	}
	sort.Strings(cat.Deferred)

	// 4) 撞名消解 + 双键表。作用域是**全部可编排名**（deferred 也要能派发）：
	//    若只按已列举的工具去重，deferred 与 direct 撞名时桥接就分不清该投给谁。
	collide := map[string]int{}
	for _, t := range callable {
		collide[normalize(t.Name)]++
	}
	for i := range callable {
		id := normalize(callable[i].Name)
		if collide[id] > 1 {
			id = collisionSuffix(id, callable[i].Name)
		}
		if prev, ok := cat.Names[id]; ok {
			return nil, fmt.Errorf(
				"codemode: 归一 + 后缀后仍撞名: %q 与 %q → %q（请改名，静默取舍会让模型调错实现）",
				prev, callable[i].Name, id)
		}
		callable[i].scriptName = id
		cat.Names[id] = callable[i].Name
		cat.RawNames[callable[i].Name] = id
	}
	// listed 是对 sorted 的筛选副本，scriptName 要同步过去（渲染用最终名）。
	for i := range listed {
		listed[i].scriptName = cat.RawNames[listed[i].Name]
	}

	// 5) 分组：默认组最前，其余按 namespace 字典序；组内按 raw 名字典序（渲染顺序）。
	byNS := map[string][]CatalogTool{}
	for _, t := range listed {
		byNS[t.Namespace] = append(byNS[t.Namespace], t)
	}
	groups := make([]CatalogGroup, 0, len(byNS))
	if g, ok := byNS[""]; ok {
		groups = append(groups, CatalogGroup{Tools: g})
	}
	names := make([]string, 0, len(byNS))
	for ns := range byNS {
		if ns != "" {
			names = append(names, ns)
		}
	}
	sort.Strings(names)
	for _, ns := range names {
		desc := ""
		if n := opts.Namespaces[ns]; n != nil {
			desc = n.Description
		}
		groups = append(groups, CatalogGroup{Namespace: ns, Description: desc, Tools: byNS[ns]})
	}

	// 6) 前缀（固定开销）+ 装箱 + 渲染。
	prefix := DESCRIPTION_INTRO + "\n\n"
	if len(cat.Deferred) > 0 {
		prefix += DEFERRED_TOOLS_GUIDANCE + "\n\n"
	}
	overhead := estimateTokens(prefix + catalogHeading + "\n")

	sel := SelectCatalog(groups, overhead, budget)
	// 裁剪统计与渲染无关：预算连一条都放不下时也要如实给出「有多少条没列」。
	for _, g := range groups {
		cat.Omitted += sel.Omitted[g.Namespace]
	}
	var b strings.Builder
	b.WriteString(prefix)
	if len(sel.Selected) == 0 {
		// 一个条目都没列出来：给一句可执行的下一步，别让模型把空目录当成
		// 「脚本里没有工具可调」。
		b.WriteString(catalogEmptyNote)
		b.WriteByte('\n')
	} else {
		b.WriteString(catalogHeading)
		b.WriteByte('\n')
		for _, g := range groups {
			b.WriteString(renderGroupHeader(g.Namespace, g.Description, sel.Omitted[g.Namespace], len(g.Tools)))
			var lines []string
			for _, t := range g.Tools {
				if !sel.Selected[t.Name] {
					continue
				}
				line := renderEntry(t)
				lines = append(lines, line)
				b.WriteString(line)
				cat.Entries = append(cat.Entries, CatalogEntry{
					RawName:   t.Name,
					Name:      t.scriptName,
					Namespace: t.Namespace,
					Line:      line,
					Cost:      estimateTokens(line),
				})
			}
			if lines != nil {
				cat.Groups = append(cat.Groups, g)
				cat.GroupLines[g.Namespace] = lines
			}
		}
	}
	cat.Description = b.String()
	cat.UsedTokens = estimateTokens(cat.Description)
	return cat, nil
}

// sortedKeys 取 map 的键并排序（确定性报错文案 / 确定性遍历）。
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
