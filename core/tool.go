package core

// FinishReason 模型本次调用的结束原因。
type FinishReason string

const (
	FinishReasonStop          FinishReason = "stop"
	FinishReasonLength        FinishReason = "length"
	FinishReasonToolCall      FinishReason = "tool_call"
	FinishReasonContentFilter FinishReason = "content_filter"
	FinishReasonAbort         FinishReason = "abort"
	FinishReasonError         FinishReason = "error"
	// FinishReasonMaxIterations 达到单次运行最大轮数护栏（MaxIterations）被截断，
	// 而非模型主动停止：任务未完成，需「继续」续跑。区别于 stop——stop 是模型
	// 自然给出最终答复结束；max_iterations 是仍在工具循环中就被护栏截断。
	FinishReasonMaxIterations FinishReason = "max_iterations"
)

// ToolCall 一次工具调用（由 LLM 发起），provider / agents / tools 共用。
type ToolCall struct {
	Id        string `json:"id"`
	Index     int64  `json:"index"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`

	// RequestId 发起该调用的请求 ID（事件自包含：工具事件可关联到具体 LLM 调用）
	RequestId string `json:"request_id,omitempty"`
}

type ToolError struct {
	Code                   string `json:"code,omitempty"`
	Changed                string `json:"changed,omitempty"` // true, false, or unknown
	Retryable              bool   `json:"retryable,omitempty"`
	RetryWithSameArguments bool   `json:"retry_with_same_arguments,omitempty"`
	NextAction             string `json:"next_action,omitempty"`
	Recovery               string `json:"recovery,omitempty"`
}

// ExecStatus 命令类工具（bash 等）的执行结局；nil = 该工具不适用/未采集。
// 存在意义：把「失败/超时/取消」从结果文本升级为结构化信号（引擎/事件/UI/指标共用），
// 同时**保留完整输出文本**（Call 不返回 error —— 否则 engine.go:659-662 会丢弃 out）。
type ExecStatus struct {
	ExitCode int  `json:"exit_code"`
	TimedOut bool `json:"timed_out,omitempty"`
	// Canceled = 命令被中断（父 ctx 取消：用户中断 / 会话关闭 / 后台任务中断）而非
	// 时限到点。与 TimedOut 互斥；语义不同（取消不是命令的问题，也不该建议调大 timeout）。
	Canceled bool `json:"canceled,omitempty"`
}

// ToolResult 一次工具执行结果，Id 与 ToolCall.Id 对应。
type ToolResult struct {
	Id        string
	Result    string
	IsError   bool
	ToolError *ToolError

	// Exec 命令类工具的执行结局（bash 等经 ExecSink 旁路上报；nil = 不适用）。
	// 与 IsError 的关系：TimedOut/Canceled 或非零退出（非良性）→ IsError=true，但
	// Result 仍是**完整命令输出**（不走 error 返回，见 ExecStatus 注释）。
	Exec *ExecStatus

	// Blocks 结果的完整内容块（text + image，可选）：图片类工具（read_file 读图 /
	// 截图 / MCP image 内容）把图像随文本一起回传，模型据此「看见」画面。
	// 语义（与 Result 的关系）：
	//   - nil/空 = 纯文本结果（绝大多数工具；Result 即全部内容，向后兼容）；
	//   - 非空 = 权威内容块，首个 text 块即 Result 文本（Result 保留为文本投影，
	//     供事件日志 / UI / 截断统计使用，不重复送两个 text 块）。
	// 消费方：AgentLoop 构造 tool 消息（文本 + 图片块）；
	// provider 翻译层按协议投放（Anthropic 原生 tool_result 图片；
	// OpenAI 系 tool 角色不支持图片 → 拆成紧随的 user 消息，见 translate）。
	// 非视觉模型由 provider.DowngradeImages 降级为占位文本（同 user 消息语义）。
	Blocks []Content

	// Usage 本次执行的用量（子 agent 类工具回传子运行用量，成本传导给父结算）
	Usage Usage
}

// ImageBlocks 返回结果中的图片块（无则 nil）。
func (r ToolResult) ImageBlocks() []Content {
	var out []Content
	for _, c := range r.Blocks {
		if c.Type == ContentTypeImage {
			out = append(out, c)
		}
	}
	return out
}

// ToolSchema 厂商无关的工具描述，由 provider 实现翻译为 SDK 格式。
type ToolSchema struct {
	Name        string
	Description string
	Parameters  any // JSON Schema 格式，由工具自行定义
}

// FileDiff 一次工具调用对单个文件的变更描述（实现了 ToolDiffProvider 的工具执行时产出：
// write/edit 类，以及声明了 outputs 的 run_python —— 后者产物路径只能由模型显式声明，
// 脚本源码与 stdout 都推不出可靠路径）。
// 宿主 UI 据此渲染 +/- 变更；经 ToolResponse.Diff 在事件层携带，不进入 LLM 上下文。
// 信息源头原则：工具执行时是唯一同时持有旧/新内容的点，宿主无法从事件推导
// （ToolResponse 不带旧内容），故由工具侧产出。
type FileDiff struct {
	Path      string `json:"path"`                // 变更文件路径（相对工作区根）
	Added     int    `json:"added"`               // 新增行数
	Removed   int    `json:"removed"`             // 删除行数
	Unified   string `json:"unified,omitempty"`   // unified diff 文本（有界，超阈值截断）
	Truncated bool   `json:"truncated,omitempty"` // 大 diff 已截断（仅保留统计）

	// Created 本次调用**创建**了该文件（此前不存在）。判定源同 FileDiff 本身：
	// 工具执行时是唯一同时持有旧/新内容的点（如 write_file 覆盖分支的 lock==nil）。
	// 不能用 Removed==0 推断——向既有文件纯插入内容（加一个函数）同样是 +N -0。
	// 消费方：Artifact.Kind（新建/修改徽标）。不影响既有 UI 渲染。
	Created bool `json:"created,omitempty"`

	// Size / Lines / SHA256 落盘后的事实（产物汇总用，见 Artifact）。
	// 由工具从**内存中的最终内容**算出——写的那一刻工具已持有完整新内容，
	// 故零额外 IO（不读磁盘）。Size=字节数；Lines=换行数（二进制为 0）；
	// SHA256=内容指纹（前 16 字节 hex，超 maxHashBytes 为空）。
	// 注意：对既有文件做 append 时 content 仅为增量，Size/Lines 留空（语义见工具侧）。
	Size   int64  `json:"size,omitempty"`
	Lines  int64  `json:"lines,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// Artifact 一个产出文件（本轮运行中由 write/edit 类工具创建或修改）。
//
// 与 FileDiff 的关系：FileDiff 描述「单次工具调用对单个文件的变更」；
// Artifact 是「整轮运行的成果汇总」——同路径多次编辑合并为一条
// （± 行数累加；size/lines/指纹取最后一次，代表最终落盘状态）。
// 两者同源（都出自工具执行点），消费方不同：FileDiff 喂行内 diff 渲染，
// Artifact 喂运行结束后的产出卡片（对标云端产品的 artifacts 卡片）。
//
// 生命周期：per-Run 采集（agents 层）→ 随 AgentEnd 事件发给宿主 → 宿主渲染。
// 不进入 LLM 上下文（同 FileDiff：给宿主看的，模型看不到）。
type Artifact struct {
	// Name 文件名（basename）。冗余存一份，避免客户端做路径解析
	//（分隔符差异、Windows 反斜杠等）。
	Name string `json:"name"`

	// Path 展示路径：工作区内 → 工作区相对路径（如 out/data.csv）；
	// 工作区外 → 绝对路径（write_file 支持绝对路径）。
	// 客户端按「以 / 开头即绝对」的既有约定还原绝对路径（本地客户端直接用）。
	Path string `json:"path"`

	// Kind created（本次运行首次创建）| modified（修改既有文件）。见 ArtifactKind*。
	Kind string `json:"kind"`

	// Added/Removed 本路径累计 +/− 行数（同路径多次编辑求和）——diff 汇总，
	// 用户据此一眼看出改动规模。
	Added   int `json:"added,omitempty"`
	Removed int `json:"removed,omitempty"`

	// Size 落盘字节数；Lines 文本行数（二进制为 0）。
	// 由工具从内存中的最终内容算出（零额外 IO）。对既有文件的 append 为空。
	Size  int64 `json:"size,omitempty"`
	Lines int64 `json:"lines,omitempty"`

	// SHA256 内容指纹（前 16 字节 hex，128 bit）。用途：
	//  1) 同内容去重（两个路径内容一致时宿主标注「与 X 相同」）；
	//  2) 变更判定（重放/恢复时对比历史指纹，判断产物是否又被改过）。
	// 超 maxArtifactHashBytes 的大文件为空（哈希收益低、CPU 成本高）。
	SHA256 string `json:"sha256,omitempty"`
}

// Artifact.Kind 取值。
const (
	ArtifactKindCreated  = "created"
	ArtifactKindModified = "modified"
)

// Usage 单轮模型调用用量。语义（三协议统一）：
//   - Input = 总输入 token（含缓存命中/写入；Anthropic 协议在 provider 层归一）
//   - CacheRead / CacheWrite = 其中命中缓存/写入缓存的部分（未命中段用 CacheMiss()）
//   - Output = 总输出 token（含 Reasoning，Reasoning ⊆ Output）
//   - TotalTokens = 处理量口径 = Input + Output。**计费量当前与之同值**（四段各按自身单价
//     计费，没有不计费的 token 类），故不另立 BillableTokens 字段 —— 若未来出现免费/不计费
//     的 token 类（如某些网关的免费缓存读），两者才会分叉，届时再加字段并说明口径。
//   - Cost = 本轮实际支出金额（µUSD，provider 按价表填充）
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64

	// CacheWrite1h ⊆ CacheWrite：按 1h extended TTL 写入的部分（Anthropic 专用；
	// 0 = 未知/不适用）。**仅 token 维度拆分** —— 成本侧仍按既有规则（价表 cache_write，
	// 1h 时由 provider 按 2× 输入价修正）；价格表形态变更随「分层计价统一设计」一并处理。
	CacheWrite1h int64

	Reasoning   int64
	TotalTokens int64

	// Details 模态/预测明细（仅上游提供时非 nil；纯文本轮恒 nil —— 不伪造 0）。
	// 约定见 UsageDetails：**上游不报就不填**。
	Details *UsageDetails

	// ReasoningSummarized 标记本轮 reasoning 是「摘要形态」（Anthropic display=summarized）：
	// Output/Reasoning 记的是完整思考 token 量，但流里只到摘要文本 —— 生成这些 token 的
	// 时间不在「首个流式 token 之后」的窗口里。展示层（tokens/s 分母）据此改用整轮耗时，
	// 否则分子含全部思考、分母只剩正文窗口 → 速度虚高数倍（三方协议里只有 Anthropic 会置位）。
	ReasoningSummarized bool

	// InputSemantics 本轮 Input 口径的判定来源（自证字段，取值见 UsageSemantics* 常量）。
	// 用途：端点违背所声明协议时（如 Anthropic 端点回 OpenAI 形状计数），事后能从事后
	// 数据判断「是端点错了还是本地判错了」，不必重放请求。空 = 未标注（第三方构造的
	// Usage / 旧数据）。
	InputSemantics string `json:"InputSemantics,omitempty"`

	Cost Cost
}

// Usage.InputSemantics 取值：Input 是否已含缓存命中/写入 + 该判定从哪来。
// 判定优先级（见 provider/anthropic 的三级判定）：注册表标注 > 响应自报形状 > 协议默认。
const (
	UsageSemanticsAnnotationInclusive = "annotation:inclusive" // 注册表标注：已含（OpenAI 语义）
	UsageSemanticsAnnotationExclusive = "annotation:exclusive" // 注册表标注：未含（Anthropic 语义）
	UsageSemanticsShapeInclusive      = "shape:inclusive"      // 响应形状嗅探：已含（OpenAI 形状网关）
	UsageSemanticsProtocolOpenAI      = "protocol:openai"      // chat-completions 协议默认：已含
	UsageSemanticsProtocolResponses   = "protocol:responses"   // Responses 协议默认：已含
	UsageSemanticsProtocolAnthropic   = "protocol:anthropic"   // Anthropic 协议默认：未含（总输入 = input + cache_read + cache_write）
)

// Add 值语义相加（逐段累加；Cost 一并累加）。**累加口径的唯一实现** ——
// agents.AgentContext.addUsage / session.addUsage / 压缩器累计失败尝试用量都走它，
// 避免字段新增时多处漂移。
//
// 口径标记（InputSemantics / ReasoningSummarized）不参与求和：它们是**单轮**的判定来源
// 标记，跨轮聚合后没有单一含义（展示层要看单轮就看 LLMEnd.Usage）。
func (u Usage) Add(v Usage) Usage {
	u.Input += v.Input
	u.Output += v.Output
	u.CacheRead += v.CacheRead
	u.CacheWrite += v.CacheWrite
	u.CacheWrite1h += v.CacheWrite1h
	u.Reasoning += v.Reasoning
	u.TotalTokens += v.TotalTokens
	u.Details = addDetails(u.Details, v.Details)
	u.Cost = u.Cost.Add(v.Cost)
	return u
}

// addDetails 明细相加：两边都 nil → nil（保持「没有信息」而不是伪造一个全 0 结构）。
func addDetails(a, b *UsageDetails) *UsageDetails {
	if a == nil && b == nil {
		return nil
	}
	var out UsageDetails
	if a != nil {
		out = *a
	}
	if b != nil {
		out.InputAudio += b.InputAudio
		out.OutputAudio += b.OutputAudio
		out.InputImage += b.InputImage
		out.PredictionAccepted += b.PredictionAccepted
		out.PredictionRejected += b.PredictionRejected
	}
	return &out
}

// Add 成本逐段相加（**只加数值段**）。
// Priced/Source 是单轮口径标记：跨轮聚合后无法用单一值表达（一轮有价一轮无价），
// 因此聚合结果恒为零值 —— 需要「这一笔有没有价」看单轮 Usage.Cost（metrics 按行统计）。
func (c Cost) Add(v Cost) Cost {
	c.Input += v.Input
	c.Output += v.Output
	c.Reasoning += v.Reasoning
	c.CacheRead += v.CacheRead
	c.CacheWrite += v.CacheWrite
	c.Total += v.Total
	return c
}

// UsageDetails 上游 usage 明细里本 Harness 建模的附加维度（2026-09-23 补齐）。
//
// 约定：**上游不报就不填** —— nil（或字段 0）表示「没有这个信息」，不表示「0 个 token」。
// 例：图片输入 token 当前 SDK 不上报，InputImage 恒 0（字段留给会报的网关/后续 SDK）。
// 只建模「会影响成本归因」的维度，不做上游字段全集搬运。
type UsageDetails struct {
	InputAudio         int64 // 输入音频 token（OpenAI prompt_tokens_details.audio_tokens）
	OutputAudio        int64 // 输出音频 token（completion_tokens_details.audio_tokens）
	InputImage         int64 // 输入图片 token（当前 SDK 不上报 → 恒 0）
	PredictionAccepted int64 // 预测输出被采纳（accepted_prediction_tokens，按输出价计费）
	PredictionRejected int64 // 预测输出被拒（rejected_prediction_tokens，通常不计费）
}

// CacheMiss 未命中缓存的输入 token（= Input - CacheRead - CacheWrite，下限 0）。
// 这是成本里按**全价**计费的那一段，也是「本轮新读入的上下文」量 —— 此前 provider 与
// bridge 各自手算一遍，现收进这里（口径唯一）。异常数据（CacheRead > Input，网关计数
// 形状判错时会出现）钳到 0，不产生负值。
func (u Usage) CacheMiss() int64 {
	miss := u.Input - u.CacheRead - u.CacheWrite
	if miss < 0 {
		return 0
	}
	return miss
}

// IsZero 是否无任何用量/成本（宿主据此决定是否携带 usage 字段：全零 = 无信息，
// 带出去只会多一个空白展示位，如不花模型钱的普通后台工具任务）。
// 只比数值段：Priced/Source/InputSemantics/Details 是口径与明细标记而非用量本身 ——
// 零 token 的无价调用仍是「无用量」。
func (u Usage) IsZero() bool {
	return u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 &&
		u.CacheWrite1h == 0 && u.Reasoning == 0 && u.TotalTokens == 0 &&
		u.Cost.Input == 0 && u.Cost.Output == 0 && u.Cost.Reasoning == 0 &&
		u.Cost.CacheRead == 0 && u.Cost.CacheWrite == 0 && u.Cost.Total == 0
}

// Cost 单次 LLM 调用（每轮）的实际支出金额，单位 µUSD（1e-6 USD，int64）。
// 由 provider 层按价表（ModelPrice：每百万 token 单价）折算并填充：
//   - Input = 未命中输入 × 全价；CacheRead = 命中 × 低价；CacheWrite = 写入 × 写价；
//     Output = 输出 × 输出价；Total = 四段之和。
//   - Reasoning 是 Output 的**拆分视图**（思考部分按输出价折出），**不参与 Total 求和**
//     —— 它回答「思考花了多少钱」，与 Output 相加会双计。
//
// 逐轮累计（agents.addUsage → Session.Usage.Cost）→ 持久化（State.Cost）→
// 子 agent 回传（ToolResult.Usage）→ MaxCost 熔断（budgetExceeded）。
type Cost struct {
	Input  int64
	Output int64
	// Reasoning ⊆ Output：思考部分成本（视图，勿与 Output 相加）。
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
	Total      int64

	// Priced 是否命中**可用**价表。false = 无价 —— 此时 Cost 全 0 表示「不知道花了多少」
	// 而非「免费」，报表/展示据此把两者分开（否则静默少算：2026-09 实测 96,738 次调用
	// 仅 33.4% 有价）。
	Priced bool `json:"Priced"`
	// Source 价表来源（CostSource* 常量）。explicit_zero = 价表有条目但价全 0
	//（内置数据里 68 个模型如此，真实语义是「价源未收录/未知价」，不是免费）。
	Source string `json:"Source,omitempty"`
}

// Cost.Source 取值。
const (
	CostSourceBuiltin      = "builtin"       // providers.json 内置价
	CostSourceOverride     = "override"      // override 表：用户手填价 / 宿主价 / 价源补价
	CostSourceExplicitZero = "explicit_zero" // 有条目但价全 0（未知价，非免费）
	CostSourceNone         = "none"          // 无条目
)
