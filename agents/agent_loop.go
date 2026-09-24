package agents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/agents/prompt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/todo"
	"github.com/seven7628/hai-harness/tools"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config 是 AgentLoop 的不可变配置，创建时经 Option 构建，跨多次 Run 复用。
type Config struct {
	Provider   provider.Provider
	ToolEngine tools.ToolEngine

	// 模型默认参数，每轮构建 StreamRequest 的取值来源
	Model           string
	ReasoningEffort provider.ReasoningEffortLevel
	Temperature     *float64
	TopP            *float64
	MaxTokens       int64
	// RetainReasoning 推理内容回传开关：nil = 按模型名自动判断（autoRetainReasoning）；
	// true = 全量回传（opencode 网关等上游要求所有思考模型回传 reasoning_content，
	// 缺失 400——bridge buildLoop 对 opencode provider 强制开启）；false = 一律丢弃。
	RetainReasoning *bool

	// SessionID 会话稳定标识（2026-09 新增）：Responses prompt cache key + session affinity
	// 头用（对齐 pi options.sessionId）。空 = 不启用。bridge 装配时 WithSessionID 注入。
	SessionID string

	// ProviderName 本 loop 绑定的 provider 名（价表/模型元数据 Lookup 用）。bridge
	// 装配时 WithProviderName 注入；空 = StreamRequest.Provider 缺省 → 注册表按模型名
	// 全局兜底（旧调用兼容）。
	ProviderName string

	// 运行边界
	MaxIterations int // 单次运行的最大轮数（防失控护栏）；0 = 不限制，
	// 失控兜底交给成本预算（Session WithMaxCost）或外部取消
	MaxParallelTools int           // 工具并发上限
	LLMTimeout       time.Duration // 可选挂死兜底：整轮 LLM 调用（含流）总时限；
	// 0 = 不限制（bridge 生产装配传 0 —— 首字预算在 FirstChunkTimeout，流开始后不限总时长）。
	// 测试/旧调用方传 >0 时生效。默认 90s（NewAgentLoop 零值语义，被 bridge 覆盖）。
	// FirstChunkTimeout 首字预算（2026-09-18 定策）：**单次尝试**从请求发出到收到模型
	// 首个流事件的上限；首个事件一到即解除，此后流内不再计时（长 reasoning / 大文档生成
	// 不受切 —— 与 LLMTimeout 的「整轮总时限」是两个东西，恰好互补）。超时按上游侧
	// 瞬时故障处理：可重试、走 LLMError 事件、耗尽后归因「供应商侧故障」。
	// 0 = 不限制。默认 30s。
	FirstChunkTimeout time.Duration

	// StreamStallTimeout 流内停滞预算（2026-09-22 定策）：**流已经开始**（首个事件已到）
	// 之后，相邻两个 provider 事件之间的最大静默时长 —— 超过即视为上游连接挂死（TCP
	// 不断开、也不再发字节），本次尝试以可重试的 provider.StreamStallError 终止。
	// 与 FirstChunkTimeout 互补：后者管「还没有第一个事件」，本项管「有了之后没有下一个」，
	// 两段不重叠（计时从首个事件才开始）。
	// **不界总时长**：每个事件重置计时，持续产出的长 reasoning / 大文档生成命中不了。
	// 0 = 不限制。默认 3min（远超健康 provider 的块间隔，又明显短于用户「卡死了」的
	// 容忍阈值 —— 反馈样本：GLM 流中段静默 >5min 无任何输出）。
	StreamStallTimeout time.Duration

	MaxRetries       int           // LLM 调用失败的最大尝试次数（含首次，默认 10；1 = 不重试）
	Backoff          Backoff       // 重试退避策略（nil = DefaultBackoff 指数+抖动）
	ToolBatchTimeout time.Duration // 整批工具执行超时（区别于 tools.WithToolTimeout 的单工具兜底）
	// EnablePostToolNudge 启用「工具轮后空回复轮」的一次性续跑提醒（问题十三）。
	// 零值 = 不启用（默认：空轮即结束，维持原语义）——空轮无法区分「卡住」与
	// 「已完成但静默」，是否兜底由宿主按模型特性显式开启（WithPostToolNudgeEnabled）。
	EnablePostToolNudge bool
	// PendingTasksNudge 启用 stop 检查点在途任务提醒（R1/B2，目录 §10.8）：
	// 模型产出最终回复即将结束时清点在途后台任务，非空则注入一次性
	// PendingTasksReminder 多跑一轮。零值 = 不启用（WithPendingTasksNudge 开启）。
	PendingTasksNudge bool
	// CompactBackoff 自动压缩失败后的退避时长（R3/C3.2；WithCompactBackoff 配置）：
	// 失败后该窗口内不触发自动压缩。零值 = 不退避。
	CompactBackoff time.Duration
	// HandoffReminderAsUser 压缩交接提醒以 user 角色注入（H2，兼容单 system 网关）。
	HandoffReminderAsUser bool
	// GoalAlignmentReminderRounds 连续无进展的 LLM 决策轮数；0 = 禁用。
	// 「进展」不止写文件（见 resetGoalAlignmentAfterSuccessfulWrite 的白名单）。
	GoalAlignmentReminderRounds int

	ContextWindow     int64
	CompressThreshold float64
	// CompressMargin 触发安全余量：预估偏低（字符/4 近似误差、工具参数等）
	// 会延迟触发导致超窗，按比例提前触发（默认 10%）
	CompressMargin float64

	// BaseSystemPrompt SDK 行为契约层（默认 DefaultSystemPrompt，字节最稳定，
	// 置于产品层/Agent.md/技能之前）。WithBaseSystemPrompt("") 可禁用；产品层
	// 内容（输出规范/人格/环境）用 WithSystemPrompt 拼接在其后。
	BaseSystemPrompt string

	SystemPrompt string

	// WorkingDir 当前工作区目录（Current Workspace 环境层）。注入系统提示词，向模型
	// 声明文件操作的根目录（对齐 Codex/Claude 形态：模型需要知道 cwd 才能正确解析
	// 相对路径、定位文件）。空 = 不注入该层。参与拼接（composeSystemPrompt 层1c）。
	WorkingDir string

	// AgentMDFiles 显式项目指令文件（工作记忆层，顺序即注入顺序）；缺失/不可读静默跳过。
	// AgentMDDirs 递归发现根（AGENTS.md/CLAUDE.md 协议，根优先排序）；可与显式文件叠加。
	AgentMDFiles []string
	AgentMDDirs  []string

	// Skills 技能注册表（渐进披露的发现来源：清单注入系统提示词），nil 表示无技能
	Skills *skills.Registry

	// Subagents 自定义 subagent 注册表（渐进披露的发现来源：清单注入系统提示词），
	// nil 表示无自定义 subagent。spawn_agent 工具在工具引擎侧注册（subagent 包）。
	Subagents SubagentLister

	// Compressor 上下文压缩策略，nil 表示不压缩
	Compressor Compressor

	// TodoResetOnCompact 压缩时清空会话 todo 的兼容选项（S1-F，默认 false）：
	// 默认语义——压缩只是替换 Messages，todo 是会话级独立状态（Session.State.Todo
	// 持久化、UI 面板数据源），压缩不清空、不需要模型重建；true = 恢复旧语义
	//（快照注入 + Clear + CompactTodoReminder，见 maybeCompact）。
	TodoResetOnCompact bool

	// Slim 工具结果瘦身配置（C：去重替换/占位替换/spill 溢出写文件；零 LLM 成本）。
	// nil = 关闭（默认）——行为变化项，产品验证后 WithToolResultSlim 启用。
	Slim *SlimConfig

	// RefExtractor 可选的 @引用内容抽取钩子（当前用于 PDF：抽出文本注入 FileContent）。
	// nil = 不抽取 —— PDF 保持旧行为（压缩流报「二进制，不读内容」/未压缩把源码当正文）。
	// 由宿主（desktop/bridge）经 WithRefExtractor 注入：抽取要真解析格式，属宿主能力面，
	// 本包不认识 Python/办公格式（理由与协议见 refextract.go）。
	RefExtractor RefExtractor
}

type Option func(*Config)

func WithProvider(p provider.Provider) Option {
	return func(c *Config) { c.Provider = p }
}

func WithToolEngine(te tools.ToolEngine) Option {
	return func(c *Config) { c.ToolEngine = te }
}

func WithModel(m string) Option {
	return func(c *Config) { c.Model = m }
}

func WithReasoningEffort(e provider.ReasoningEffortLevel) Option {
	return func(c *Config) { c.ReasoningEffort = e }
}

func WithTemperature(t float64) Option {
	return func(c *Config) { c.Temperature = &t }
}

func WithTopP(t float64) Option {
	return func(c *Config) { c.TopP = &t }
}

func WithMaxTokens(n int64) Option {
	return func(c *Config) { c.MaxTokens = n }
}

// WithRetainReasoning 强制/关闭推理内容回传（覆盖按模型名自动判断）：
// 思考模型（deepseek/glm/qwen 等）工具调用回传缺失 reasoning_content 会 400，
// opencode 网关等上游对全部思考模型有此要求，宿主应显式开启。
func WithRetainReasoning(b bool) Option {
	return func(c *Config) { c.RetainReasoning = &b }
}

// WithSessionID 注入会话稳定标识（Responses prompt cache key + session affinity 头）。
func WithSessionID(sid string) Option {
	return func(c *Config) { c.SessionID = sid }
}

// WithProviderName 绑定本 loop 的 provider 名（价表/模型元数据 Lookup 用）。
// bridge 装配时注入（主 loop/子 agent/压缩器共用）；不设 = StreamRequest.Provider
// 缺省 → 注册表按模型名全局兜底（旧调用/测试兼容）。
func WithProviderName(name string) Option {
	return func(c *Config) { c.ProviderName = name }
}

func WithMaxIterations(n int) Option {
	return func(c *Config) { c.MaxIterations = n }
}

// WithPostToolNudgeEnabled 启用工具轮后空轮的一次性续跑提醒（问题十三）。
// 默认不启用；对「工具结果返回后常静默 stop」的模型建议开启。
func WithPostToolNudgeEnabled() Option {
	return func(c *Config) { c.EnablePostToolNudge = true }
}

// WithPendingTasksNudge 启用 stop 检查点在途任务提醒（R1/B2，2026-08-24 §10.8）：
// 模型产出最终回复即将结束时，若仍有在途后台任务且本 Run 未提醒过 → 注入一次性
// PendingTasksReminder 并多跑一轮（再次坚持结束则放行，防死循环）。
// 默认不启用（宿主显式开启；桌面 Bridge 打开——弱模型"提前宣布完成"是高频失败模式）。
func WithPendingTasksNudge() Option {
	return func(c *Config) { c.PendingTasksNudge = true }
}

// WithCompactBackoff 设置自动压缩失败后的退避时长（R3/C3.2）：失败后该窗口内不再
// 触发自动压缩（forced 手动压缩不受限），防"压缩失败 → 上下文继续涨 → 立刻再试"
// 的失控循环（实测 211k/231k 失控样本）。0 = 不退避（旧行为）。建议 3min。
func WithCompactBackoff(d time.Duration) Option {
	return func(c *Config) { c.CompactBackoff = d }
}

// WithHandoffReminderAsUser 将压缩交接提醒以 user 角色注入（H2/目录 §10.7）：
// 兼容只接受一条 system 消息的网关（压缩边界一次注入多条 system 时摘要可能被
// 丢弃或 400）。默认 false = system 角色（语义正确）；纯说明性注记降级 user 无损。
func WithHandoffReminderAsUser() Option {
	return func(c *Config) { c.HandoffReminderAsUser = true }
}

func WithGoalAlignmentReminderRounds(n int) Option {
	return func(c *Config) { c.GoalAlignmentReminderRounds = n }
}

func WithMaxParallelTools(n int) Option {
	return func(c *Config) { c.MaxParallelTools = n }
}
func WithLLMTimeout(d time.Duration) Option {
	return func(c *Config) { c.LLMTimeout = d }
}

// WithFirstChunkTimeout 设置首字预算（每次尝试独立计时；0 = 不限制）。
// 语义与取值理由见 Config.FirstChunkTimeout。
func WithFirstChunkTimeout(d time.Duration) Option {
	return func(c *Config) { c.FirstChunkTimeout = d }
}

// WithStreamStallTimeout 设置流内停滞预算（每次尝试独立计时；0 = 不限制）。
// 语义与取值理由见 Config.StreamStallTimeout。
func WithStreamStallTimeout(d time.Duration) Option {
	return func(c *Config) { c.StreamStallTimeout = d }
}

func WithMaxRetries(n int) Option {
	return func(c *Config) { c.MaxRetries = n }
}

func WithToolBatchTimeout(d time.Duration) Option {
	return func(c *Config) { c.ToolBatchTimeout = d }
}

func WithContextWindow(n int64) Option {
	return func(c *Config) { c.ContextWindow = n }
}

func WithCompressThreshold(t float64) Option {
	return func(c *Config) { c.CompressThreshold = t }
}

func WithCompressMargin(m float64) Option {
	return func(c *Config) { c.CompressMargin = m }
}

// WithTodoResetOnCompact 恢复"压缩即清空会话 todo"的旧语义（S1-F 兼容选项）：
// 压缩成功后快照注入 + Store.Clear + prompt.CompactTodoReminder，模型用
// todo_add/todo_update 重建。默认 false：压缩不清空 todo（todo 是会话级独立状态）。
func WithTodoResetOnCompact(b bool) Option {
	return func(c *Config) { c.TodoResetOnCompact = b }
}

func WithSystemPrompt(s string) Option {
	return func(c *Config) { c.SystemPrompt = s }
}

// WithRefExtractor 注入 @引用内容抽取钩子（当前用于 PDF：抽取文本作为 FileContent 正文）。
//
// 为什么是宿主注入而不是内置：抽取依赖受管 Python 运行时 + pypdf + 沙箱，都是宿主
// 的能力面，agents 包不 import tools/runtime/sandbox（分层）。风格同
// tools/builtin.FileTools.WithBinaryHint：库定义协议与调用时机，实现由装配点提供。
//
// 不注入（nil）时行为与本选项存在之前逐字节一致：PDF 走既有 二进制/文本 判定。
// 三态协议（抽到 / 无文本层 / 抽取器不可用）见 RefExtractor 注释。
func WithRefExtractor(fn RefExtractor) Option {
	return func(c *Config) { c.RefExtractor = fn }
}

// WithWorkingDir 注入当前工作区目录（Current Workspace 环境层）。向模型声明文件操作
// 的根目录（cwd），使它能力图解析相对路径 / 定位文件。空串不注入该层。
// 区别于 WithAgentMDDir：后者管工作记忆（AGENTS.md/CLAUDE.md）的发现根；本选项是把
// 工作目录本身写进系统提示词（行为契约之后、工作记忆之前的层1c）。
func WithWorkingDir(dir string) Option {
	return func(c *Config) { c.WorkingDir = dir }
}

// WithAgentMDFiles 注入显式项目指令文件（工作记忆层）；顺序即注入顺序。
// 缺失/不可读文件静默跳过（与 skills 损坏跳过同风格）。
func WithAgentMDFiles(files ...string) Option {
	return func(c *Config) { c.AgentMDFiles = files }
}

// WithAgentMDDir 递归发现项目指令文件（AGENTS.md / CLAUDE.md，根优先排序）。
// 每次运行前重读：文件变更在下一次运行自动生效（字节稳定不变时缓存命中不受影响）。
func WithAgentMDDir(dir string) Option {
	return func(c *Config) { c.AgentMDDirs = append(c.AgentMDDirs, dir) }
}

func WithSkills(reg *skills.Registry) Option {
	return func(c *Config) { c.Skills = reg }
}

func WithCompressor(comp Compressor) Option {
	return func(c *Config) { c.Compressor = comp }
}

// AgentContext 是一次 Run 的运行状态，由 Run 创建并返回。
type AgentContext struct {
	RunId string // 本次运行唯一标识（事件流关联 / 断点恢复锚点）

	// ParentRunId 发起方运行的 RunId（子 agent 场景，经 RunOptions 注入）；空 = 根运行
	ParentRunId string
	Depth       int    // 运行深度（根 = 0）
	Name        string // 运行的短标签（子 agent 名，RunOptions.Name 注入）；空 = 无标签

	Handler events.EventHandler // 本次运行的事件消费者

	Messages   []core.Message // 当前上下文，每轮追加/压缩
	RunHistory []core.Message // 本轮产生的消息（注入的 system/assistant 回复/工具回执，压缩不影响），供会话完整历史落盘
	Iteration  int            // 当前轮数

	// inputMark @引用展开边界：ac.Messages 中已展开过的消息下标。streamOnce 只展开
	// [inputMark:] 的 user 消息（本轮新增输入），历史 user 消息不重复展开（防上下文
	// 膨胀与 refs_loaded 重复）。Poll 追加输入后更新；压缩替换 Messages 后重置。
	inputMark int

	// Todo 会话级待办清单（经 RunOptions 注入，随工具上下文暴露给 todo 工具）
	Todo *todo.Store

	// Approver 人工确认接口（经 RunOptions 注入，随工具上下文暴露给引擎）
	Approver events.Approver

	// Mode 本次运行的模式（经 RunOptions 注入；toolSchemas 过滤与 runTools
	// 防御据此翻译工具可见性，见 mode.go）
	Mode *Mode

	// batch 当前工具批控制器（runTools 期间持有；Session.PromoteTask 经
	// PromoteToolTask 摘离运行中工具为后台任务）。atomic.Pointer：Session
	//（其他 goroutine）并发读取，runTools 写/清（批结束摘除，防 stale promote）。
	batch atomic.Pointer[tools.BatchController]

	ctx     context.Context // per-run 控制信号
	cancel  context.CancelFunc
	aborted atomic.Bool

	Usage core.Usage // 累计用量与成本（含压缩调用；Cost 内嵌于 Usage）

	// failures 本 Run 工具失败账本（C5 去重守卫 + C6 压缩交接 [RecentFailures] 的
	// **共享前置件**）：对**所有** IsError 结果记账（不要求 ToolError 信封 —— bash 与
	// 散文错误因此才进得来；兜底分类器不写 ToolResult.ToolError 的既有决策未动），
	// 记录工具名 + 首行摘要 + code（可空）+ 工具轮序号。
	// 去重拦截只认「紧邻的上一轮工具轮 + 其间无状态变更」（见 runTools / failureLedger
	// .blockedIn）——**绝不**因「本 Run 内失败过」永久拦截：go build / go test 的合法
	// 重跑必须放行（batch1 书面决策）。
	failures *failureLedger
	// toolRound 工具轮序号（runTools 调用序，1 起）：C5「紧邻的上一轮工具轮」的判据
	// 来源。只有**含工具调用**的轮推进它（纯文本/纯推理轮不是工具轮，不推进）——
	// 因此「紧邻」的语义是「上一次工具执行」，不是「上一次 LLM 轮」。
	toolRound int

	// postToolNudgePending 工具轮后的空回复轮续跑提醒（问题十三）：stop 分支检测到
	// 「上一轮执行过工具 + 本轮无可见输出」时置位，streamOnce 组装请求时注入 ephemeral
	// 提醒并清位——一次 Run 仅一次机会（再空则按现状结束，防死循环）。
	postToolNudgePending bool
	// postToolNudgeUsed 本次 Run 是否已用过空轮续跑提醒（仅一次，防死循环）。
	postToolNudgeUsed bool
	// todoGateUsed 本次 Run 是否已做过收尾前完成度检查（A4/B4：每 Run 至多一次，
	// 模型再次坚持结束则放行，防死循环）。对齐 pendingTasksNudged 的既有范式。
	todoGateUsed bool
	// pendingTasksNudged 本次 Run 是否已注入过 stop 检查点在途任务提醒
	// （R1/B2：每 Run 至多一次，模型再次坚持结束则放行，防死循环）。
	pendingTasksNudged bool
	// stopHookBlocks Stop hook（hooks 能力扩展）要求"继续回合"的已用次数：带上限
	// （maxStopHookBlocks = 8，对标 Claude Code 的连续 block 上限），超出即放行结束
	// —— 一个写错的 hook 不能把会话卡死在循环里。
	stopHookBlocks int
	// overflowRetryUsed 本次 Run 是否已用过 overflow 强制压缩重试（R2/C7，一次性）。
	overflowRetryUsed bool
	// compactBackoffUntil 自动压缩失败退避截止时间（R3/C3.2；零值 = 无退避）。
	compactBackoffUntil time.Time
	// afterTools 上一 LLM 轮是否执行了工具批（空轮 nudge 的触发条件之一）。
	afterTools bool
	// goalAlignmentRounds 是本次 Run 的连续**无进展**轮数（进展判据见
	// resetGoalAlignmentAfterSuccessfulWrite / progressToolResult）。
	goalAlignmentRounds int
	// 每个连续无修改区间只注入一次；成功 edit/write 后随计数一起清零。
	goalAlignmentReminderInjected bool
	// sawAssistantText 本次 Run 是否产出过**任何** assistant 文本（A1(b)：每轮
	// round.content trim 后非空即置位）。只升不降——中途有输出就算有输出，不因收尾轮
	// 为空而回退。**自 C9（2026-09-18 主会话裁定）起它不再是 RunSilentEnd 的发射判据**，
	// 只作为 RunSilentEnd.SawAssistantText 回传给消费者，区分两种静默形态：
	// false = 全程一句话没说（A1）、true = 说过话但最后没交付（C2）；是否发射改看
	// 「收尾轮无文本」（见下方 stop 检查点处的裁定说明）。
	sawAssistantText bool
	est              *estimator

	// forceCompact 强制压缩标志（命令框架：compact 命令置位，本轮结算段
	// maybeCompact(forced=true) 直接压缩，跳过 ShouldCompact 预估）
	forceCompact bool

	// forceClear 清空上下文标志（命令框架：clear 命令置位；循环顶部清空全部历史
	// 仅保留 system，然后 Stop 结束本轮——上下文已空无需再跑 LLM，下轮从零开始）
	forceClear bool

	// loadedSkill 产品强制加载的技能（命令框架：skills load 命令置位；
	// 每轮循环顶部经 composeSystemPrompt 层4 重注入 system，见 appendLoadedSkill）
	loadedSkill string

	// stopped 策略性终止标志（E：PostToolBatch 返回 true 经 Stop() 置位；
	// 本轮工具批收尾后循环自然终止，AgentEnd finishReason=stop 非失败）。
	// 与 aborted 的区别：不取消 ctx、不中断任何执行；跨 goroutine 终止用 Abort。
	// 同 goroutine（loop 主体）读写。
	stopped bool

	// slim 工具结果瘦身状态（C；WithToolResultSlim 启用时初始化，per-Run）：
	// 去重索引/轮龄记录。压缩成功后 reset（下标失效）。
	slim *slimState

	// fileLedger 引擎侧文件账本（S1-B，per-Run）：工具调用触达的文件路径与最后操作。
	// 压缩时渲染 [FileLedger] 进压缩输入 + 交接提醒；压缩后不 reset（元数据，
	// 不携带消息下标，不随 Messages 替换失效）。
	fileLedger *fileLedger

	// FinalResponse 最终回复文本（stop 结束轮的 LLM 输出；子 agent 工具取此作为结果）
	FinalResponse string

	// Err 本次运行的最终错误（重试耗尽或不可重试），nil 表示成功
	Err error
}

// Abort 中断当前轮后退出。
func (ac *AgentContext) Abort() {
	ac.aborted.Store(true)
	ac.cancel()
}

// Cancel 立即取消整次运行。
func (ac *AgentContext) Cancel() {
	ac.cancel()
}

// IsAborted 是否收到中断请求。
func (ac *AgentContext) IsAborted() bool {
	return ac.aborted.Load()
}

// Stop 策略性终止本次运行（E：PostToolBatch 返回 true 时由 loop 调用）。
// 与 Abort 不同：不取消 ctx、不中断执行，本轮工具批正常收尾后循环自然终止
// （AgentEnd finishReason=stop，非失败）。同 goroutine 读写（loop 主体）。
func (ac *AgentContext) Stop() { ac.stopped = true }

// IsStopped 是否收到策略性终止请求。
func (ac *AgentContext) IsStopped() bool { return ac.stopped }

// Context 返回本次运行的根上下文。
func (ac *AgentContext) Context() context.Context {
	return ac.ctx
}

// PromoteToolTask 摘离当前运行中的工具任务为后台任务（Session.PromoteTask 入口）：
// 返回 true = 已摘离（主 loop 立即拿到占位结果并继续；原任务后台完成 → 结果自动回传
// 主会话，见 RunOptions.TaskSink）；false = 无此运行中的工具任务（批已结束/未知 id）。
func (ac *AgentContext) PromoteToolTask(taskID string) bool {
	b := ac.batch.Load()
	return b != nil && b.Promote(taskID)
}

// CompressStats 压缩触发评估输入（由 AgentLoop 计算）。
type CompressStats struct {
	EstimatedTokens int64 // 预估 token：上轮请求精确 Input + 新增消息字符/自适应系数
	Budget          int64 // 触发线（窗口 × 阈值）
}

// CompressResult 压缩结果；Usage 为压缩调用的用量（含成本，计入运行结算）。
type CompressResult struct {
	// Summary 是给 UI 展示的摘要正文；Messages 中的摘要消息可能被系统提醒包裹，
	// 因此由压缩器单独返回，避免客户端只能显示消息数和 token。
	Summary  string         // 面向用户展示的 Markdown 摘要正文
	Messages []core.Message // 压缩后上下文（[system, 摘要...]）
	Model    string         // 生成摘要的模型
	Usage    core.Usage     // 压缩调用用量（**成功那次**；含成本，计入运行结算）
	// FailedUsage 失败/重试尝试的已产生用量累计（含成本 µUSD，2026-09-23 决策：
	// 失败尝试也要记账）。压缩输入是整段上下文，失败一次可能白烧大量 input token。
	// 与 Usage 分开存：Usage.Input 是**压缩后上下文锚点**（est.observe），混入失败尝试会
	// 让锚点虚高、压缩触发错乱。两条路径都计入运行结算（addUsage），合计 = 本次压缩总花费。
	FailedUsage core.Usage
	// Warnings 压缩质量警告（S1-G 护栏）：摘要缺失关键节（Main Requests and
	// Intent / Current Task / File Context）等；前端折叠提示，不阻断压缩。
	Warnings []string
	// Analysis 压缩器 <analysis> 块留档（O1，2026-08-24 方案 G7）：仅观测——
	// 进事件日志与会话 jsonl 的 compaction 记录，绝不进 Messages/上下文。
	Analysis string
}

// Compressor 是上下文压缩策略，允许自定义实现并通过 WithCompressor 注入。
type Compressor interface {
	// ShouldCompact 判断当前上下文是否达到压缩条件。
	ShouldCompact(ctx context.Context, stats CompressStats) bool
	// Compact 执行压缩，返回压缩后的消息；失败时调用方降级为不压缩。
	Compact(ctx context.Context, messages []core.Message) (CompressResult, error)
}

// AgentLoop 是事件驱动的 Agent 运行时，只持有不可变 Config，可安全并发执行多个 Run。
type AgentLoop struct {
	cfg Config

	// 运行时可变配置：effort/model/max_tokens/contextWindow/goalAlignmentReminderRounds 会话期间可切换，下一次 LLM 请求生效
	// （初始值 = cfg 对应字段；与 Mode 同为"运行中快照、下轮生效"的运行边界配置）
	effortMu sync.RWMutex
	effort   provider.ReasoningEffortLevel
	modelMu  sync.RWMutex
	model    string
	// 单次输出上限 / 上下文窗口：随模型切换联动（SetMaxTokens/SetContextWindow），
	// 运行中的 Run 下一轮 LLM 立即读到新值（streamOnce / maybeCompact 均按 current 取值）
	cfgMu                       sync.RWMutex
	maxTokens                   int64
	contextWindow               int64
	goalAlignmentMu             sync.RWMutex
	goalAlignmentReminderRounds int
	enablePostToolNudge         bool
	pendingTasksNudge           bool
	// maxParallelOnce 并发上限同步（runTools 首轮执行一次，防重复设置与并发竞争）
	maxParallelOnce sync.Once
}

func NewAgentLoop(opts ...Option) *AgentLoop {
	cfg := Config{
		// 单次运行最大轮数（防失控护栏）。真实长时任务（调研/实现/测试/迭代）轮次需求大，
		// 护栏只作极端失控的兜底，真正失控交给成本预算（Session WithMaxCost）或外部取消。
		// 截断时以 FinishReasonMaxIterations 明确暴露（绝不伪装成自然 stop），客户端据此
		// 提示「已达轮数上限、任务未完成，可继续」而非误判为完成。
		MaxIterations:    1000,
		MaxParallelTools: 8,
		LLMTimeout:       90 * time.Second,
		// 首字预算 30s（2026-09-18 定策）：实测一次尝试在「无首字」状态下静默挂了 187s
		// （用户只看到「正在重试 1」），而 30s 足够覆盖正常建连+排队+首个 token 的漫长尾；
		// 超时即视为上游无响应 → 记一次失败并重试，与「流开始后不限总时长」互补。
		FirstChunkTimeout: 30 * time.Second,
		// 流内停滞预算 3min（2026-09-22 定策）：首个事件之后两个事件之间的最大静默。
		// 反馈样本（GLM）：流中段静默 >5min 没有任何输出（界面上表现为「卡住」）；
		// 取 3min = 远超健康流的块间隔（秒级），又明显短于用户的容忍阈值。
		StreamStallTimeout: 3 * time.Minute,
		MaxRetries:         10,
		ContextWindow:      128000,
		CompressThreshold:  0.8,
		CompressMargin:     0.1,
		BaseSystemPrompt:   DefaultSystemPrompt,
	}
	for _, o := range opts {
		o(&cfg)
	}
	return &AgentLoop{
		cfg:                         cfg,
		effort:                      cfg.ReasoningEffort,
		model:                       cfg.Model,
		maxTokens:                   cfg.MaxTokens,
		contextWindow:               cfg.ContextWindow,
		goalAlignmentReminderRounds: cfg.GoalAlignmentReminderRounds,
		enablePostToolNudge:         cfg.EnablePostToolNudge,
		pendingTasksNudge:           cfg.PendingTasksNudge,
	}
}

// SetReasoningEffort 会话期间切换思考档位：下一次 LLM 请求生效，不重建 loop/会话。
func (a *AgentLoop) SetReasoningEffort(e provider.ReasoningEffortLevel) {
	a.effortMu.Lock()
	a.effort = e
	a.effortMu.Unlock()
}

func (a *AgentLoop) currentEffort() provider.ReasoningEffortLevel {
	a.effortMu.RLock()
	defer a.effortMu.RUnlock()
	return a.effort
}

// SetModel 会话期间切换模型：下一次 LLM 请求生效，不重建 loop/会话
// （与 effort 同为运行边界配置；事件 Model 字段读 currentModel 保持同步）。
func (a *AgentLoop) SetModel(m string) {
	a.modelMu.Lock()
	a.model = m
	a.modelMu.Unlock()
}

func (a *AgentLoop) currentModel() string {
	a.modelMu.RLock()
	defer a.modelMu.RUnlock()
	return a.model
}

// SetMaxTokens 会话期间切换单次输出上限：下一次 LLM 请求生效，不重建 loop/会话
// （与 model/effort 同为运行边界配置；随模型切换联动，见 bridge switch_model）。
func (a *AgentLoop) SetMaxTokens(n int64) {
	a.cfgMu.Lock()
	a.maxTokens = n
	a.cfgMu.Unlock()
}

func (a *AgentLoop) currentMaxTokens() int64 {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.maxTokens
}

// SetContextWindow 会话期间切换上下文窗口：下一次压缩判定生效（压缩触发线
// ContextWindow × Threshold 随之变化）。随模型切换联动（1M 模型切走/切回）。
func (a *AgentLoop) SetContextWindow(n int64) {
	a.cfgMu.Lock()
	a.contextWindow = n
	a.cfgMu.Unlock()
}

func (a *AgentLoop) currentContextWindow() int64 {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.contextWindow
}

// ContextWindow 当前上下文窗口（随模型切换联动；只读）。
// 宿主用它与主 loop 的一致性校验：子 loop（Explore/spawn）必须拷贝同窗口配置，不得缺省
// （缺省 = 128k 默认，1M 模型下压缩触发线错误）。
func (a *AgentLoop) ContextWindow() int64 {
	return a.currentContextWindow()
}

// Compressor 当前压缩器（可能为 nil = 不压缩；只读）。
// 宿主用它校验"子 loop 与主 loop 同配置但独立实例"（拷贝而非共享 —— 共享同一实例
// 会让子/主运行的压缩互相影响配置与状态）。
func (a *AgentLoop) Compressor() Compressor {
	return a.cfg.Compressor
}

// SetGoalAlignmentReminderRounds 会话期间切换目标一致性提醒阈值：下一次 LLM 请求生效。
// 0 表示禁用；负数按禁用处理。该 setter 只更新运行时阈值，不重置当前 Run 的计数状态。
func (a *AgentLoop) SetGoalAlignmentReminderRounds(n int) {
	if n < 0 {
		n = 0
	}
	a.goalAlignmentMu.Lock()
	a.goalAlignmentReminderRounds = n
	a.goalAlignmentMu.Unlock()
}

func (a *AgentLoop) currentGoalAlignmentReminderRounds() int {
	a.goalAlignmentMu.RLock()
	defer a.goalAlignmentMu.RUnlock()
	return a.goalAlignmentReminderRounds
}

// RunOptions 运行选项（RunStream 参数）。
type RunOptions struct {
	// ParentRunId 发起方运行的 RunId（子 agent 场景，由 SubAgent 工具注入）；空 = 根运行。
	// Depth 运行深度（根 = 0）：父子事件流沿此重建调用树。
	ParentRunId string
	Depth       int
	// Name 运行的短标签（子 agent 名，SubAgent 工具注入 agent_spawn 的 name）；空 = 无标签。
	// 前端 AgentStart 事件据此展示子 agent 名。
	Name string

	// TaskId 后台任务 id（异步 SubAgent/Explore 的 runBackground 注入 tk.ID）：AgentStart/
	// AgentEnd 事件据此携带，供前端把运行与任务卡片确定性关联（停止按钮/结果回填/去重）。
	// 空 = 同步运行 / 非后台任务。
	TaskId string

	// Todo 会话级待办清单（todo 工具经此注入工具上下文）；nil = 无会话待办。
	Todo *todo.Store

	// Approver 人工确认接口（HITL）：工具轮执行前需要审批的调用会逐个请求确认。
	// nil = 不审批（现状）。
	Approver events.Approver

	// Mode 本次运行的模式（三维执行策略：工具可见性/审批决策/行为提示）。
	// 由注入边界组装（Session 快照当前模式 / subagent 按 Spec 指定）；nil = 现状
	// （全工具 + 工具自声明审批 + 无模式提示）。整个 Run 期间不变（快照语义：
	// 运行中切换模式只影响下一次 Run）。
	Mode *Mode

	// Poll 每轮 LLM End 自然结束（finish=stop）时的插话检查点；工具调用轮不插话。nil = 不插话。
	Poll func(ctx context.Context) []core.Message
	// OnRunning 运行开始后回调（ac 已就绪），供外层持有运行句柄（如打断）。
	OnRunning func(ac *AgentContext)
	// OnTurn 每轮结束检查点回调（ac 已含本轮产出：assistant 回复/工具回执/插话输入），
	// 供外层做增量持久化 —— 长时运行崩溃恢复的粒度 = 轮。
	OnTurn func(ac *AgentContext)

	// CommandHandler 自定义命令执行器（命令框架）：注入段剥出的非内置命令
	//（compact/skills 走内核路径）经此回调；nil = 未知命令报错。
	// Session.RegisterCommand 注册表经闭包注入（session → agents 单向依赖）。
	CommandHandler func(ctx context.Context, name string, args string) error

	// PreToolUse 工具执行前回调（E：模式过滤后、审批前；可拒绝、可改写输入）。
	// PostToolBatch 工具批执行后回调（E：返回 true = 策略性终止，AgentEnd stop）。
	// PreCompact 压缩前回调（E：返回串注入压缩输入，摘要可见、输出不含）。
	// 子 agent 继承通道：ToolContext.Hooks（subagent 组装子 RunOptions 时透传，
	// Spec.DisableHookInherit 可关）。
	PreToolUse    events.PreToolUseHook
	PostToolBatch events.PostToolBatchHook
	PreCompact    events.PreCompactHook

	// 以下为 hooks 能力扩展（2026-09-20，见 docs/graft-integration/05-hooks-能力设计规范.md）。
	// 全部为 nil 时行为与扩展前完全一致（向后兼容）：

	// PostToolUse 每个工具结果落定后回调（逐工具粒度）：注入串追加到该结果文本尾部。
	PostToolUse events.PostToolUseHook
	// Stop 回合自然结束的门控点：可要求"再跑一轮"（带上限，见 AgentContext.stopHookBlocks）。
	Stop events.StopHook
	// SystemExtra 每 Run 追加到系统提示词（**在 Mode.SystemHint 之前**）的额外内容。
	// 用途：hooks 的 SessionStart 注入——会话内保持稳定即不破坏 provider 前缀缓存；
	// 不要用它做"每轮变化"的注入（那会每轮击穿缓存，应该走 user 消息）。
	SystemExtra func() string

	// TaskSink 摘离（promote）工具任务完成回调（Session 接线 → PushTaskResultStatus）：
	// 运行中的长工具/同步子 agent 被摘离为后台任务后，完成后真实结果经此回调回传主会话
	//（模型下轮续跑）。interrupted = 该任务经精确中断（InterruptTask）终止——终态为
	// interrupted 而非 failed/completed，结果/错误文本仍完整保留。nil = 摘离结果仅事件
	//（ToolResponse），不推回（决策 #6）。
	TaskSink func(taskID string, r core.ToolResult, interrupted bool)
	// AsyncPromoteAfter 工具运行超过此阈值自动摘离为后台任务；0 = 关（默认，决策 #5）。
	// 用户主动 promote（Session.PromoteTask → bridge promote_task）恒可用，不受此影响。
	AsyncPromoteAfter time.Duration

	// TaskState 后台任务状态渲染回调（S1-B 交接提醒的 [TaskStates] 段来源）：
	// 压缩成功后注入的 CompactHandoffReminder 里附在途任务状态（id/名称/最后已知状态），
	// 续跑模型由此知道哪些任务还挂着（未见终态 = running，不得假设完成）。
	// 由 Session 组装时闭包取 Registry().List() 渲染；nil（子 agent 运行等无 Registry
	// 场景）= 不注入该段。
	TaskState func() string

	// BatchCreated 批控制器创建回调（Session 接线 → 注册 tooltask-* 精确中断路由）：
	// runTools 创建控制器后、RunBatch 前调用——任务启动即可被精确中断。
	// BatchClosed 批控制器回收回调（Session 接线 → 注销）：runBatch 返回且批内全部
	// 已摘离任务交付完成后触发一次（防注册表泄漏；零摘离批在 runBatch 返回即触发）。
	// 二者均 nil = 中断路由不可用（直接运行、无宿主的场景）。
	BatchCreated func(b *tools.BatchController)
	BatchClosed  func(b *tools.BatchController)
}

// Run 执行一次 agent 运行（无插话），返回携带最终状态与累计结果的 AgentContext。
func (a *AgentLoop) Run(ctx context.Context, input []core.Message, handler events.EventHandler) *AgentContext {
	return a.RunStream(ctx, input, handler, nil)
}

// RunStream 执行一次 agent 运行，支持插话与运行句柄回调（见 RunOptions）。
func (a *AgentLoop) RunStream(ctx context.Context, input []core.Message, handler events.EventHandler, opts *RunOptions) *AgentContext {
	if handler == nil {
		handler = func(context.Context, events.Event) {}
	}
	if opts == nil {
		opts = &RunOptions{}
	}

	// 产出采集（Artifacts）：包装 handler 旁路记录 write/edit 类工具的落盘变更，
	// 供 AgentEnd.Artifacts 携带（宿主的「本次产出」卡片）。在此处包装而非工具调用点
	// 埋点：一个点覆盖主/子 agent（子 agent 经 agent_spawn 走自己的 RunStream），
	// 且不侵入 runTools/engine（不改工具执行链路）。
	collector := newArtifactCollector(a.cfg.WorkingDir)
	handler = collector.wrap(handler)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	todoStore := opts.Todo
	if todoStore == nil {
		// 独立待办：未注入（子 agent / 直接运行）时每次运行自带一份独立 store，
		// 随运行销毁不持久化 —— 子 agent 不继承父的待办，有自己独立的 TODO 管理。
		todoStore = todo.New()
	}
	ac := &AgentContext{
		RunId:       newRunID(),
		Handler:     handler,
		ctx:         runCtx,
		cancel:      cancel,
		ParentRunId: opts.ParentRunId,
		Depth:       opts.Depth,
		Name:        opts.Name,
		Todo:        todoStore,
		Approver:    opts.Approver,
		Mode:        opts.Mode,
		est:         &estimator{},
		failures:    newFailureLedger(),
		slim:        nil,
	}
	if a.cfg.Slim != nil {
		ac.slim = newSlimState(*a.cfg.Slim)
	}
	ac.fileLedger = newFileLedger()

	// 命令消费（B）：剥出内部指令消息执行 —— 必须在 system 注入之前
	//（指令消息是 system 角色且 messageText 为空——command 块不在 text/summary
	// 白名单——若不先剥出会被下方的「内容比较替换」当旧 system 覆盖，命令丢失）。
	input = a.injectCommands(ac, opts, input)

	// 系统提示词注入：分层组装（行为契约 base + 产品层 + 工作记忆 Agent.md + 技能清单）
	// + 模式提示 Mode.SystemHint（最尾部：模式切换只影响尾段，稳定前缀保持缓存命中）。
	// 首条非 system / 为空 → 前置注入；是 system 但内容不同 → 原位替换
	// （恢复会话时 SystemPrompt/Agent.md/技能变更自动生效）；内容相同 → 不动
	// （字节稳定 = provider 前缀缓存命中，零历史噪音）。不修改调用方切片。
	systemChanged := false
	systemPrompt := a.composeSystemPrompt()
	// hooks 能力扩展：SystemExtra（如 SessionStart 注入的仓库图谱定向）追加在
	// Mode.SystemHint **之前** —— 模式提示保持最尾部（模式切换只影响尾段），
	// SystemExtra 在会话内稳定 → provider 前缀缓存不受影响。
	if opts != nil && opts.SystemExtra != nil {
		if extra := strings.TrimSpace(opts.SystemExtra()); extra != "" {
			if systemPrompt != "" {
				systemPrompt += "\n\n"
			}
			systemPrompt += extra
		}
	}
	if opts.Mode != nil && opts.Mode.SystemHint != "" {
		if systemPrompt != "" {
			systemPrompt += "\n\n"
		}
		systemPrompt += opts.Mode.SystemHint
	}
	if systemPrompt != "" {
		switch {
		case len(input) == 0 || input[0].Role != core.System:
			input = append([]core.Message{core.NewSystemMessage(systemPrompt)}, input...)
			systemChanged = true
		case messageText(input[0]) != systemPrompt:
			input[0] = core.NewSystemMessage(systemPrompt)
			systemChanged = true
		}
	}
	ac.Messages = append([]core.Message(nil), input...)
	// @引用展开边界（AgentHarness 层）：只展开「本次新增的 user 输入」，不重复展开
	// 历史 user 消息（否则每次请求都重新展开全部历史引用 → 上下文膨胀 + LLM 困惑 +
	// refs_loaded 重复通知）。inputMark 记录待展开消息的起始下标：初始输入从 0 展开；
	// Poll 追加新输入后更新为追加位置；压缩/clear 后重置。
	ac.inputMark = 0
	if systemChanged {
		ac.RunHistory = append(ac.RunHistory, input[0])
	}
	if opts != nil && opts.OnRunning != nil {
		opts.OnRunning(ac)
	}

	startTime := time.Now()
	ac.Handler(ac.ctx, &events.AgentStart{
		Index:       0,
		Content:     firstUserInput(input),
		RunId:       ac.RunId,
		Name:        ac.Name,
		TaskId:      opts.TaskId, // 后台任务 id（前端任务卡片关联锚；空 = 非后台任务）
		ParentRunId: ac.ParentRunId,
		Depth:       ac.Depth,
		Model:       a.currentModel(),
		// ContextWindow 上报（问题六）：前端占比分母单一事实源（1M 热切换后本地表滞后）
		ContextWindow: a.currentContextWindow(),
		Timestamp:     time.Now(),
		EventType:     events.AgentStartType,
	})

	lastContent := ""
	lastReasoning := ""
	var allToolCalls []core.ToolCall // 本轮所有工具调用（跨轮累计，供 AgentEnd 聚合）
	for ac.Iteration = 1; a.cfg.MaxIterations <= 0 || ac.Iteration <= a.cfg.MaxIterations; ac.Iteration++ {
		if ac.IsAborted() || ac.IsStopped() {
			break
		}

		// 每轮 LLM Start 前：尝试收集暂存用户输入（方案点1——每轮 Start 收集一波用户
		// Messages 拼到数组末尾）。复用 Poll 插话回调 drain inbox（并发出 UserInputsConsumed
		// 确认）；无输入则无操作。与 stop 边界插话互补：此处覆盖工具调用轮，让用户运行中
		// 连发的消息下一轮 LLM Start 就进上下文（而不用等到 stop 边界/run 结束）。
		if opts != nil && opts.Poll != nil {
			if inputs := opts.Poll(ac.ctx); len(inputs) > 0 {
				ac.Messages = append(ac.Messages, inputs...)
				ac.inputMark = len(ac.Messages) - len(inputs) // @展开边界：新输入从这开始
				a.onTurn(ac, opts)                            // 检查点：本轮输入已就绪
			}
		}

		// 运行中命令消费（B）：Poll 插话检查点注入的输入可能含命令消息
		//（onTurn 检查点先于本点，Session persist 过滤兜底双保险——指令消息不持久化）
		ac.Messages = a.injectCommands(ac, opts, ac.Messages)

		// clear 命令（B）：清空全部上下文历史，仅保留首条 system（系统提示词每轮
		// 由 composeSystemPrompt 重建，保留只为首条不变量；其余含旧摘要全部丢弃），
		// 然后 Stop 结束本轮——上下文已空，无需再跑 LLM，下轮从零开始。
		if ac.forceClear {
			ac.forceClear = false
			if len(ac.Messages) > 0 && ac.Messages[0].Role == core.System {
				ac.Messages = ac.Messages[:1]
			} else {
				ac.Messages = nil
			}
			ac.inputMark = len(ac.Messages) // clear 后无待展开原文
			ac.Stop()
			continue // 下轮顶部 IsStopped → break
		}

		// 已加载技能 system 重注入（B）：loadedSkill 变更后每轮顶部「内容比较替换」，
		// 复用入口注入逻辑（稳定字节不重写，零历史噪音；重拼亚毫秒级，低频可接受）
		if ac.loadedSkill != "" {
			sp := a.appendLoadedSkill(a.composeSystemPrompt(), ac)
			if sp != "" {
				switch {
				case len(ac.Messages) == 0 || ac.Messages[0].Role != core.System:
					ac.Messages = append([]core.Message{core.NewSystemMessage(sp)}, ac.Messages...)
					ac.RunHistory = append(ac.RunHistory, ac.Messages[0])
				case messageText(ac.Messages[0]) != sp:
					ac.Messages[0] = core.NewSystemMessage(sp)
					ac.RunHistory = append(ac.RunHistory, ac.Messages[0])
				}
			}
		}

		// 结算段（C）：占位替换在压缩前执行（减少压缩输入体积）；去重替换
		// 在回写循环内即时完成（见 recordRead）
		if ac.slim != nil {
			a.applyStale(ac)
		}

		forced := ac.forceCompact
		ac.forceCompact = false
		a.maybeCompact(ac, opts, forced)
		// 手动压缩是独立操作：压缩完成后停在当前状态，等待用户下一条输入，
		// 不应继续触发一次 LLM 对话。自动压缩则继续当前轮。
		if forced {
			ac.Stop()
			continue
		}

		// 本轮 LLM 调用（聚合结果经 roundResult 填充，重试复用同一结构）
		var round roundResult
		err := a.streamWithRetry(ac, &round)
		if err != nil {
			// R2/C7 overflow 重试：上下文超限错误 → 强制压缩一次 → 重试本轮。
			// 每 Run 限一次（overflowRetryUsed）：再超限说明压缩后仍放不下或压缩
			// 失败，原样暴露错误（最坏退化为现状）。
			if !ac.overflowRetryUsed && a.cfg.Compressor != nil && isContextOverflowError(err) {
				ac.overflowRetryUsed = true
				a.maybeCompact(ac, opts, true)
				continue
			}
			ac.Err = err
			break
		}
		lastContent, lastReasoning = round.content, round.reasoning
		// A1(b) 零输出 run 的可见化：本轮是否产出过 assistant 文本（trim 后非空）。
		// 只看聚合后的 round.content —— 流式增量只走事件，最终文本由 LLMEnd.Content 携带。
		if strings.TrimSpace(round.content) != "" {
			ac.sawAssistantText = true
		}
		// 一次成功的 LLMStart → LLMEnd 是一轮 Agent 决策。Provider 重试只有
		// 最终成功返回后才到达这里，因此同一逻辑轮只计一次。
		a.recordGoalAlignmentLLMRound(ac)

		if round.finishReason != core.FinishReasonToolCall || len(round.toolCalls) == 0 {
			// 工具轮后的空回复轮续跑（问题十三）：部分推理模型在工具结果回写后输出
			// 「仅 reasoning、无 content 无工具调用」的空轮即 stop——任务被静默中断，
			// 用户必须再输入才继续。此处注入一次性 ephemeral 提醒让模型继续；
			// 再空则按现状结束（防死循环）。非工具轮后的空轮维持原语义。
			if round.content == "" && a.enablePostToolNudge && ac.afterTools && !ac.postToolNudgePending && !ac.postToolNudgeUsed {
				ac.postToolNudgePending = true
				ac.postToolNudgeUsed = true
				ac.afterTools = false
				continue
			}
			ac.afterTools = false
			if opts != nil && opts.Poll != nil {
				// 空回复轮（模型只思考/无可见文本、无工具调用）不回写上下文 —— 否则
				// 历史里多一条空 assistant 消息，下一轮回传时上游 400 "content or
				// tool_calls must be set"（opencode 网关 2026-08-17 实证）。
				if round.content != "" {
					assistantMsg := core.NewAssistantMessageWithSignature(
						[]core.Content{{Type: "text", Content: round.content}},
						round.reasoning, round.reasoningSignature,
						nil,
					)
					assistantMsg.Model = a.currentModel() // 同模型 thinking 回传判断（对齐 pi isSameModel）
					ac.Messages = append(ac.Messages, assistantMsg)
					ac.RunHistory = append(ac.RunHistory, assistantMsg)
				}
				if inputs := opts.Poll(ac.ctx); len(inputs) > 0 {
					ac.Messages = append(ac.Messages, inputs...)
					a.onTurn(ac, opts) // 检查点：本轮产出 + 插话输入已就绪
					continue
				}
			} else {
				// 非流式模式：最终回复只计入本轮历史（不回写上下文，保持 Messages 语义）
				if round.content != "" || round.reasoning != "" {
					sigMsg := core.NewAssistantMessageWithSignature(
						[]core.Content{{Type: "text", Content: round.content}},
						round.reasoning, round.reasoningSignature,
						nil,
					)
					sigMsg.Model = a.currentModel() // 同模型 thinking 回传判断（对齐 pi isSameModel）
					ac.RunHistory = append(ac.RunHistory, sigMsg)
				}
			}
			// R1/B2 stop 检查点在途任务提醒（目录 §10.8）：模型产出"最终回复"的瞬间
			// 恰是后台任务语义离它最远的时刻——此处清点在途任务，非空且本 Run 未提醒过
			// → 注入一次性 PendingTasksReminder 并多跑一轮；模型再次坚持结束则放行
			// （nudged 位已置，防死循环）。插话优先级更高：上面 Poll 分支已 continue。
			if a.pendingTasksNudge && opts != nil && opts.TaskState != nil && !ac.pendingTasksNudged {
				if snap := opts.TaskState(); snap != "" {
					ac.pendingTasksNudged = true
					ac.Messages = append(ac.Messages,
						core.NewSystemMessage(fmt.Sprintf(prompt.PendingTasksReminder, snap)))
					a.onTurn(ac, opts) // 检查点：提醒已就绪
					continue
				}
			}
			// A4/B4 收尾前验收门控（走机制，不改 Code 提示词）：模型产出"最终回复"、即将
			// 自然结束，而会话 todo 仍有未完成项——引擎在此拦一次，把未完成清单放到决策
			// 眼前并多跑一轮；模型再次坚持结束则放行（todoGateUsed 已置，防死循环）。
			// 同 PendingTasksNudge 范式：提醒写进 ac.Messages（持久化，模型做完剩余工作
			// 期间一直可见）并立即 onTurn 落盘；顺序排在插话（上面 Poll 已 continue）与在途
			// 任务提醒之后。
			// 为何走机制而非提示词：Code 主会话提示词的工程流程节（含 Verification）是用户
			// 特意移除的（TestDesktopCodePromptV2Concise 防回归），且实测提示词约束行为会失效。
			// 为何默认开启、不加 Config 开关：postToolNudge 因 WithPostToolNudgeEnabled 仅测试
			// 接线而成为生产死代码（TestPostToolNudgeDefaultOff，2026-08-24）——本门控不接受
			// 同一命运，如需 opt-out 请在评审中提出而非预先埋开关。
			// 效果经会话长度 / 未完成 todo 率间接观测（本批不加事件类型，避免牵动 events/ 与桥接）。
			// 只在自然结束路径生效：abort / ctx 取消不做门控（此时收尾属打断，不该再拖一轮，
			// 更不该把提醒写进即将落盘的历史）；出错路径已在上面 err != nil 处 break。
			if !ac.todoGateUsed && !ac.IsAborted() && ac.ctx.Err() == nil && hasUnfinishedTodos(ac.Todo) {
				ac.todoGateUsed = true
				ac.Messages = append(ac.Messages,
					core.NewSystemMessage(fmt.Sprintf(prompt.IncompleteTodosReminder, renderUnfinishedTodos(ac.Todo))))
				a.onTurn(ac, opts) // 检查点：提醒已就绪
				continue
			}
			// Stop（hooks 能力扩展）：模型即将自然结束本轮前的**最后一个**门控点。
			// 外部 hook 可要求"再跑一轮"（社区里 Stop-hook loop 类工具的用法：让 agent
			// 继续未完成的工作）。三重护栏缺一不可：① 只在自然结束路径生效（abort /
			// ctx 取消 / 出错路径在上面已 break —— 那些属打断，不该再拖一轮）；
			// ② 每 Run 次数上限 maxStopHookBlocks=8（对标 Claude Code 的连续 block 上限）；
			// ③ hook panic → 视为"不继续"（坏 hook 不得卡住会话）。
			if opts != nil && opts.Stop != nil && !ac.IsAborted() && ac.ctx.Err() == nil &&
				ac.stopHookBlocks < maxStopHookBlocks {
				cont, panicked := callStop(opts.Stop, ac.ctx, events.StopContext{
					RunId:         ac.RunId,
					LastAssistant: round.content,
					Iteration:     int64(ac.Iteration),
				})
				if panicked {
					toolHookWarning(ac, "Stop", "hook panicked, treated as no-continue")
				}
				if cont.Continue {
					ac.stopHookBlocks++
					reason := strings.TrimSpace(cont.Reason)
					if reason == "" {
						reason = "the hook asked you to keep working on the current task"
					}
					ac.Messages = append(ac.Messages, core.NewSystemMessage(fmt.Sprintf(
						"<system-reminder>A Stop hook asked this turn to keep going (%d/%d):\n%s\n</system-reminder>",
						ac.stopHookBlocks, maxStopHookBlocks, reason)))
					a.onTurn(ac, opts) // 检查点：提醒已就绪
					continue
				}
			}
			// A1(b)/C9 静默结束的可见事件（**只加可观测性，不改模型行为**）：走到 stop
			// 检查点意味着模型这一轮既没产出文本、也没发起工具调用 —— 即**没有交付最终
			// 答复** —— 用户与宿主看不到任何最终回复，此前这种「静默结束」在事件流里没有
			// 任何痕迹（B4 收尾门控不覆盖该形态：从未建过 todo ⇒ 不触发）。
			//
			// 判据为何是「收尾轮无文本」而非「全程无文本」（C9，2026-09-18 主会话裁定）：
			// 旧判据（ac.sawAssistantText == false）会漏掉「说了话、干了活、最后没交付」这
			// 一形态 —— 当天 C2 委派（task-4mm4rucv）静默死亡，轨迹里 round 67 明确有文本
			//（"Now let me write the C2.3 quantification document:"）、跑满 72 轮、改动落盘
			// 完整，却没有送达任何报告：按旧判据这次死亡根本不会被记录，而它恰恰是最需要
			// 被记录的形态。放宽后两种形态仍可拆分：事件里的 SawAssistantText
			//（= ac.sawAssistantText）区分「全程一句话没说」（A1，基线 31/1.3%）与
			// 「说过话但最后没交付」（C2，基线 85/3.5%）。
			//
			// 这里只把静默结束变成可见事实，不注入提醒/nudge（模型可见的兜底是 A1(a) 的
			// postToolNudge，另一项工作）。
			// 为什么不是「失败」：IsError / AgentEnd 语义都不动 —— 静默结束也可能是
			// 用户主动放弃或本就不需要回复，把它报成 error 会污染失败率读数。
			// 守卫同 B4 门控：abort / ctx 取消属打断而非自然收尾，不发事件；
			// 出错路径（ac.Err != nil）在上面就已 break，到不了这里。
			if strings.TrimSpace(round.content) == "" && !ac.IsAborted() && ac.ctx.Err() == nil {
				a.emitRunSilentEnd(ac, len(allToolCalls))
			}
			a.onTurn(ac, opts)
			break
		}

		allToolCalls = append(allToolCalls, round.toolCalls...)
		results, err := a.runTools(ac, opts, round.toolCalls)
		if err != nil || results == nil {
			break
		}
		// LLM 轮已经计数；工具执行结束后仅处理成功 write/edit 的清零。
		a.resetGoalAlignmentAfterSuccessfulWrite(ac, round.toolCalls, results)

		// 工具结果按 Id 回写上下文，进入下一轮（assistant 消息完整回传 Reasoning/ToolCalls，
		// 推理模型如 DeepSeek 在工具调用场景要求完整回传，否则 400）
		assistantMsg := core.NewAssistantMessageWithSignature(
			[]core.Content{{Type: "text", Content: round.content}},
			round.reasoning, round.reasoningSignature,
			round.toolCalls,
		)
		assistantMsg.Model = a.currentModel() // 同模型 thinking 回传判断（对齐 pi isSameModel）
		ac.Messages = append(ac.Messages, assistantMsg)
		ac.RunHistory = append(ac.RunHistory, assistantMsg)
		byId := make(map[string]core.ToolResult, len(results))
		for _, r := range results {
			byId[r.Id] = r
		}
		for _, tc := range round.toolCalls {
			if r, ok := byId[tc.Id]; ok {
				// 工具结果瘦身（C）：spill 溢出写文件在入历史前执行（与截断同原理，
				// 缓存友好——历史只留 path + 预览，完整输出可检索）
				text := r.Result
				if ac.slim != nil {
					text = a.spillResult(ac, tc, text)
				}
				// 图片类工具结果（read_file 读图 / 截图 / MCP image）：图片块随 tool
				// 消息入上下文（模型可见）；slim 只作用于 text —— 图片不参与 spill/去重
				//（二进制无「重复读取替换为引用」语义，且截断会直接损坏图像）。
				// 图片在 spillResult 之后仍原样携带：即使文本被瘦身成文件引用，
				// 模型也能看到画面本身。
				var images []core.Content
				if len(r.Blocks) > 0 {
					for _, b := range r.Blocks {
						if b.Type == core.ContentTypeImage {
							images = append(images, b)
						}
					}
				}
				toolMsg := core.NewToolMessageWithImages(tc.Id, text, images)
				// 文件账本记账（S1-B）：与去重记录同位置（append 前）——
				// read/write 类工具触达的路径进账本；bash 等不记（避免误报）
				recordFileOps(ac, tc)
				// 去重记录（C）：append 前调用——newIdx = 本次结果下标；同 path
				// 同 hash 时旧结果替换短引用（下标替换不失效，安全）
				if ac.slim != nil {
					a.recordRead(ac, tc, text, len(ac.Messages))
				}
				ac.Messages = append(ac.Messages, toolMsg)
				ac.RunHistory = append(ac.RunHistory, toolMsg)
			}
		}
		ac.afterTools = true // 空轮 nudge 触发条件（问题十三）：标记上一轮为工具轮
		a.onTurn(ac, opts)   // 检查点：工具轮产出已回写
	}

	// 护栏截断时 for 的后置自增会让 Iteration 多 1，归一到实际执行轮数（不限制时跳过）。
	// hitMaxIter = 因 MaxIterations 护栏截断（模型仍在工具循环中就被强制终止），
	// 而非模型自然 stop / abort / error。自然 stop 时 Iteration ≤ MaxIterations，
	// 此处恒为 false；截断时最后一次工具批把 Iteration 推到 MaxIterations+1 → true。
	hitMaxIter := a.cfg.MaxIterations > 0 && ac.Iteration > a.cfg.MaxIterations
	if hitMaxIter {
		ac.Iteration = a.cfg.MaxIterations
	}

	// AgentEnd：整次运行的结算（与单次 LLMEnd 的区别见 events.AgentEnd 注释）
	endReason := core.FinishReasonStop
	errMsg := ""
	errKind := ""
	switch {
	case ac.IsAborted():
		endReason = core.FinishReasonAbort // 主动打断（如用户插话中断），非失败
		ac.Err = nil                       // abort 非失败：清掉 ctx 取消等错误，避免子 agent/后台任务把它当失败
	case ac.Err != nil:
		endReason = core.FinishReasonError
		errMsg = ac.Err.Error()
		errKind = provider.ClassifyError(ac.Err) // 类型标签：rate_limit/permanent/canceled/generic
	case ac.IsStopped():
		endReason = core.FinishReasonStop // 策略性终止（PostToolBatch 等）：正常 stop
	case hitMaxIter:
		// 达到轮数护栏被截断：任务未完成、无最终答复，须以 distinct reason 暴露，
		// 否则 UI 会误判为自然 stop「完成」——用户只能靠再次输入「继续」续跑。
		endReason = core.FinishReasonMaxIterations
	}
	usage := ac.Usage
	ac.FinalResponse = lastContent
	// 产出汇总（纯内存，见 agents/artifacts.go）：在任何结束原因下都携带——
	// 中断/失败前写入的文件是真实产出，不应因为 run 未正常结束而丢失。
	artifacts, artifactsTruncated := collector.collect()
	ac.Handler(ac.ctx, &events.AgentEnd{
		Index:              int64(ac.Iteration),
		Content:            lastContent,
		Reasoning:          lastReasoning,
		ToolCalls:          allToolCalls,
		FinishReason:       endReason,
		Usage:              &usage,
		Error:              errMsg,
		ErrorKind:          errKind,
		Artifacts:          artifacts,
		ArtifactsTruncated: artifactsTruncated,
		RunId:              ac.RunId,
		TaskId:             opts.TaskId, // 与 AgentStart 同源：前端终态收敛兜底锚点
		ParentRunId:        ac.ParentRunId,
		Depth:              ac.Depth,
		Model:              a.currentModel(),
		Timestamp:          time.Now(),
		DurationMS:         time.Since(startTime).Milliseconds(),
		EventType:          events.AgentEndType,
	})

	return ac
}

// buildSkillManifest 组装技能清单（渐进披露的发现阶段：仅元数据注入系统提示词）。
func buildSkillManifest(reg *skills.Registry) string {
	list := reg.List()
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(prompt.SkillManifestHeader)
	for _, s := range list {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	return b.String()
}

// recordGoalAlignmentLLMRound 记录一次成功的 Agent LLM 决策轮。
// 计数单位是逻辑上的 LLMStart → LLMEnd；Provider 重试在 streamWithRetry
// 内部完成，只有最终成功返回才会调用本方法，因此不会重复计数。
// 达到阈值后计数饱和，提醒注入后不再递增，直到有进展的工具批清零
// （resetGoalAlignmentAfterSuccessfulWrite；判据见 progressToolResult）。
func (a *AgentLoop) recordGoalAlignmentLLMRound(ac *AgentContext) {
	threshold := a.currentGoalAlignmentReminderRounds()
	if threshold <= 0 || ac.goalAlignmentReminderInjected || ac.goalAlignmentRounds >= threshold {
		return
	}
	ac.goalAlignmentRounds++
}

// resetGoalAlignmentAfterSuccessfulWrite 在工具批结束后按 ToolCall ID 匹配结果，
// 判定本批是否取得**进展**；有进展则清零连续无进展轮数并重新开始计算。
// 失败调用、结果乱序或白名单外的工具都不清零。
//
// 函数名保留历史：判据原先**只认**成功的 write_file/edit_file，现已扩展为「有进展」的
// 工具白名单（见 progressToolResult）——名字里的 SuccessfulWrite 是旧判据的残留，
// 改名的收益小于让审阅者按旧名对照方案的成本，故留名不留语义。
//
// 为何放宽（**刻意的取舍**，勿轻易收回）：实践读数 322 次提醒影响 157/251 = 62% 的
// 会话、`round` 字段恒等于阈值（min=p50=p90=max），说明判据太窄 —— 提醒与真实漂移
// 无关，而提醒是**软干预**（一次性、ephemeral、不进历史），每次误报都要注入上下文并
// 打断读研究型会话。放宽必然降低「真漂移」的检出率（模型可以靠刷 todo、空跑 test 来
// 规避提醒）——这是**故意**的：当前误报的代价高于一次漏报。
// 阈值语义、提醒文案与 B4 收尾门控都不在本函数职责内，均未改动。
func (a *AgentLoop) resetGoalAlignmentAfterSuccessfulWrite(ac *AgentContext, calls []core.ToolCall, results []core.ToolResult) {
	byID := make(map[string]core.ToolResult, len(results))
	for _, r := range results {
		byID[r.Id] = r
	}
	for _, tc := range calls {
		r, ok := byID[tc.Id]
		if !ok || r.IsError {
			continue // 无结果（乱序/未回传）或失败调用：不算进展
		}
		if !progressToolResult(tc, r) {
			continue
		}
		ac.goalAlignmentRounds = 0
		ac.goalAlignmentReminderInjected = false
		return
	}
}

// progressToolResult 判定一次**成功**（!IsError）的工具调用是否算「进展」。
//
// 白名单与理由：
//   - write_file / edit_file：产物已落盘（现状判据，未变）；
//   - todo_add / todo_update：计划被显式推进（模型自己声明了进展）；
//   - agent_spawn：委派已启动（本身就是行动，不是空转）；
//   - TaskOutput：拿到了子任务**终态**结论（只看进度不算——见 hasTerminalTaskStatus）；
//   - bash：验证动作（构建/测试类命令——见 isVerifyCommand）。
//
// 注：方案里还列了 todo_delete，但本仓 todo.Tools() 只定义 add/update（无删除工具），
// 登记一个不存在的名字属死代码，故不列入；将来真有该工具时漏列只会退化到「多提醒一次」。
//
// 白名单外的工具（read_file/grep/TaskList/...）一律不算进展 —— 只读研究型会话仍会被提醒，
// 这正是护栏要保留的行为（防止「纯读空转」被判为进展）。
func progressToolResult(tc core.ToolCall, r core.ToolResult) bool {
	switch tc.Name {
	case "write_file", "edit_file", "todo_add", "todo_update", "agent_spawn":
		return true
	case "TaskOutput":
		return hasTerminalTaskStatus(r.Result)
	case "bash":
		return isVerifyCommand(tc.Arguments)
	}
	return false
}

// terminalTaskStatusMarkers 子任务终态（subagent.TaskStatus）：跑完了才算「拿到结论」。
var terminalTaskStatusMarkers = []string{"completed", "failed", "interrupted", "abandoned"}

// hasTerminalTaskStatus 判定 TaskOutput 的结果文本是否带子任务**终态**结论。
// 结果形如 {"task_id":"task-1","status":"completed",...}（subagent/task_output.go 用
// json.Marshal 产出，紧凑无空格），故按结构化字段匹配而不是裸词匹配：
// 裸词会把 running 任务的 journal 正文里出现的 "completed" 也算成结论。
// running / interrupting（只看了一眼进度）不计进展。
func hasTerminalTaskStatus(result string) bool {
	for _, st := range terminalTaskStatusMarkers {
		if strings.Contains(result, `"status":"`+st+`"`) {
			return true
		}
	}
	return false
}

// verifyCommandMarkers bash「构建/测试类」判定的白名单（小写子串匹配）。
//
// **保守取舍（写在代码里，勿放宽成「任意 bash」）**：白名单外一律**不**计入进展
// ⇒ 行为退化到「现状」（可能多提醒一次），不会把 `ls`/`cat`/`git log` 这类浏览动作
// 当成验证而引入**新的**误报。判不出来就不算进展：新语言/新构建工具（如 bazel、mise run）
// 未被收录时会少清零一次，代价是软提醒，可接受；漏收录的补救成本远低于认错命令。
var verifyCommandMarkers = []string{
	"go build", "go test", "go vet",
	"npm test", "npm run build",
	"pytest", "cargo build", "cargo test",
	"make ", "tsc", "ruff", "eslint",
}

// isVerifyCommand 判定一次 bash 调用是否构建/测试类命令（= 验证动作 = 进展）。
// 参数解析失败（非法 JSON / 无 command 字段）返回 false：判不出来就不算进展。
func isVerifyCommand(arguments string) bool {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return false
	}
	cmd := strings.ToLower(a.Command)
	if cmd == "" {
		return false
	}
	for _, m := range verifyCommandMarkers {
		if strings.Contains(cmd, m) {
			return true
		}
	}
	return false
}

// emitRunSilentEnd 发出「静默结束」事件（A1(b)；判据已按 C9 裁定放宽为「收尾轮无文本」，
// 裁定理由见 stop 检查点处注释）。本项是**可观测性**，不改任何模型可见行为、
// 不改 AgentEnd / IsError 语义。
//
// SawAssistantText 负责拆分两种形态：false = 全程一句话没说（A1 形态）；
// true = 中途说过话、收尾轮却没交付最终答复（C2 形态）。
//
// nil-safe：RunStream 已把 nil handler 包成空函数（独立用 SDK 的调用方不传 handler），
// 此处再防一手，避免有人绕过 RunStream 直接构造 AgentContext 时 panic。
func (a *AgentLoop) emitRunSilentEnd(ac *AgentContext, toolCalls int) {
	if ac.Handler == nil {
		return // 无消费者：静默跳过（与既有事件一致）
	}
	ac.Handler(ac.ctx, &events.RunSilentEnd{
		RunId:            ac.RunId,
		Rounds:           ac.Iteration, // 此处即最终轮号（自然结束，未被 MaxIterations 截断）
		ToolCalls:        toolCalls,
		HadToolCalls:     toolCalls > 0,
		SawAssistantText: ac.sawAssistantText, // true = 「说过话但最后没交付」（C2 形态）
		Timestamp:        time.Now(),
		EventType:        events.RunSilentEndType,
	})
}

// onTurn 触发每轮检查点回调。集中封装 nil 判断，避免运行循环遗漏回调。
func (a *AgentLoop) onTurn(ac *AgentContext, opts *RunOptions) {
	if opts != nil && opts.OnTurn != nil {
		opts.OnTurn(ac)
	}
}

// maxUnfinishedTodosInReminder 收尾前完成度检查里最多列出的未完成项数：上限只为
// 避免超长清单塞进请求，超出部分以一行省略标记代替（清单本身仍在会话状态里）。
const maxUnfinishedTodosInReminder = 10

// maxStopHookBlocks Stop hook（hooks 能力扩展）单次 Run 内"要求继续回合"的次数上限。
// 对标 Claude Code 的连续 block 上限（默认 8，超限强制结束回合）：注入型 hook 写错、
// 或 Stop-hook loop 类工具陷入自激时，这是最后一道护栏。
const maxStopHookBlocks = 8

// hasUnfinishedTodos 会话 todo 是否存在未完成项（A4/B4 门控条件）。nil 安全：
// 未接线 todo 的运行（ac.Todo 被置空）不门控、不 panic。
func hasUnfinishedTodos(s *todo.Store) bool {
	if s == nil {
		return false
	}
	for _, it := range s.Items() {
		if !it.Done {
			return true
		}
	}
	return false
}

// renderUnfinishedTodos 渲染未完成项清单（每行 "- <id>: <title>"，上限
// maxUnfinishedTodosInReminder 项；nil / 全完成返回空串）。纯读取，遍历的是
// Store.Items 返回的副本，不改 todo 状态。
func renderUnfinishedTodos(s *todo.Store) string {
	if s == nil {
		return ""
	}
	var sb strings.Builder
	n := 0
	for _, it := range s.Items() {
		if it.Done {
			continue
		}
		if n >= maxUnfinishedTodosInReminder {
			sb.WriteString("\n- (more unfinished items omitted)")
			break
		}
		if n > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "- %s: %s", it.Id, it.Title)
		n++
	}
	return sb.String()
}

// newRunID 生成一次运行唯一标识（8 字节随机 hex）。
func newRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// composeSystemPrompt 组装分层 system 提示词（单条消息，拼接序固定）：
//
//	层1a 行为契约 cfg.BaseSystemPrompt（SDK 拥有，默认 DefaultSystemPrompt，字节最稳定）
//	层1b 产品层   cfg.SystemPrompt（输出规范/人格/环境，WithSystemPrompt 拼接）
//	层1c 环境层   cfg.WorkingDir（Current Workspace，WithWorkingDir 注入；动态 per-workspace）
//	层2 工作记忆  Agent.md 文件块（显式文件 + 递归发现；字节稳定于文件不变）
//	层3 动态层    技能清单（buildSkillManifest，进程内稳定）
//	层4 动态层    自定义 subagent 清单（buildSubagentManifest，进程内稳定）
//
// 稳定字节在前 = provider 前缀缓存命中最大化；任一层变化只影响其后的前缀。
// 每次运行重读 Agent.md（AgentLoop 不可变约束，不做本地缓存）；内容未变则拼接
// 结果逐字节相同，缓存命中不受重读影响。
func (a *AgentLoop) composeSystemPrompt() string {
	var b strings.Builder
	// 层1a：SDK 行为契约（恒非空，除非 WithBaseSystemPrompt("") 显式禁用）
	if a.cfg.BaseSystemPrompt != "" {
		b.WriteString(a.cfg.BaseSystemPrompt)
	}
	// 层1b：产品层
	if a.cfg.SystemPrompt != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(a.cfg.SystemPrompt)
	}
	// 层1c：Current Workspace（环境信息；向模型声明文件操作根目录）
	if a.cfg.WorkingDir != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## Current Workspace\n- " + a.cfg.WorkingDir)
	}
	// 工作记忆：显式文件（顺序即注入序，RelPath 显示为文件路径）
	var files []agentMDFile
	for _, f := range a.cfg.AgentMDFiles {
		if content, err := os.ReadFile(f); err == nil {
			files = append(files, agentMDFile{Name: filepath.Base(f), RelPath: f, Content: string(content)})
		}
	}
	for _, dir := range a.cfg.AgentMDDirs {
		files = append(files, discoverAgentMD(dir)...)
	}
	if mem := composeWorkingMemory(files); mem != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(mem)
	}
	// 技能清单（buildSkillManifest 自带 \n\n 前缀）
	if a.cfg.Skills != nil {
		b.WriteString(buildSkillManifest(a.cfg.Skills))
	}
	// 自定义 subagent 清单（buildSubagentManifest 自带 \n\n 前缀；渐进披露）
	if a.cfg.Subagents != nil {
		b.WriteString(buildSubagentManifest(a.cfg.Subagents))
	}
	return b.String()
}

// firstUserInput 取最近一条 user 消息的文本，作为 AgentStart 的输入摘要。
// 取「最后一条」而非首条：input 含全量历史（Session 组装），首条 user 恒为
// 最早任务（多任务场景误导审计——trace 实证 2026-08-10）；尾部 user = 本次
// 输入（插话/子 agent/恢复场景语义均正确）。
func firstUserInput(messages []core.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if m := messages[i]; m.Role == core.User && len(m.Content) > 0 {
			// 取第一个正文块（text）：跳过 mesh_from/command 等提示块——agent_start 的
			// content 摘要应反映用户实际说了什么，而非"[来自远程会话 node://...]"。
			for _, c := range m.Content {
				if c.Type == core.ContentTypeText {
					return c.Content
				}
			}
			return m.Content[0].Content
		}
	}
	return ""
}

// roundResult 一轮 LLM 调用的聚合结果（translate 填充；streamWithRetry 重试复用同一结构）。
type roundResult struct {
	toolCalls          []core.ToolCall
	finishReason       core.FinishReason
	content            string
	reasoning          string
	reasoningSignature string // Anthropic thinking signature（多轮回传）
}

// isContextOverflowError 识别上下文超限类错误（R2/C7）：优先用 provider 层的
// 错误分类（ContextExceededError / ClassifyError），未分类的裸错误再按上游常见
// 文案兜底匹配。误判代价可控：多做一次强制压缩后重试（每 Run 一次）。
func isContextOverflowError(err error) bool {
	if err == nil {
		return false
	}
	if provider.ClassifyError(err) == provider.ErrorKindContextExceeded {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{
		"context length", "maximum context", "context window",
		"too many tokens", "input length", "token limit",
	} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// streamWithRetry 发起一轮 LLM 调用，失败时自动重试（最多 MaxRetries 次尝试，
// 每次尝试超时 LLMTimeout）；每次失败发 LLMError 事件（WillRetry 标记是否还有机会）。
func (a *AgentLoop) streamWithRetry(ac *AgentContext, r *roundResult) error {
	maxAttempts := a.cfg.MaxRetries
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	for attempt := 1; ; attempt++ {
		// 中断守卫：abort 已请求则立即以 ctx 错误结束，不再发起/重试任何 LLM 调用。
		// 保证客户端中断（Abort）立即生效 —— 不依赖 provider 对取消的感知时机。
		if ac.IsAborted() {
			return ac.ctx.Err()
		}
		receivedEnd, err := a.streamOnce(ac, r)
		// 校验契约：一轮调用必须收到 LLMEnd 事件，否则视为失败（防「假成功」）
		if err == nil && !receivedEnd {
			err = errors.New("provider stream ended without LLMEnd event")
		}
		// 上下文超限识别兜底：provider 层分类漏包（自定义 provider / 流中错误帧）时
		// 由本层补齐 —— 超限必须不重试（重试只会再发同一份超限上下文）且语义化为
		// ContextExceededError（前端据此输出"上下文为什么结束"的专门提示）。
		if err != nil {
			err = provider.DetectContextExceeded(err)
		}
		if err == nil {
			return nil
		}

		willRetry := attempt < maxAttempts && retryable(err)
		// 退避等待时长（不重试则为 0）；事件先发，消费者立即看到错误与等待窗口
		delay := time.Duration(0)
		if willRetry {
			backoff := a.cfg.Backoff
			if backoff == nil {
				backoff = DefaultBackoff
			}
			delay = backoff(attempt, err)
		}
		// 失败尝试的已产生用量（上游已计费，如 Anthropic message_start 就带 input）：
		// 计入运行结算 + 随事件上抛。契约：**每次尝试各自记账一次** —— 失败尝试走本事件，
		// 成功那次走 LLMEnd.Usage，两者相加 = 本轮真实花费（不重复计）。
		var partial *core.Usage
		if u, ok := provider.PartialUsage(err); ok {
			ac.addUsage(u)
			partial = &u
		}
		ac.Handler(ac.ctx, &events.LLMError{
			RunId:        ac.RunId,
			Model:        a.currentModel(),
			Message:      err.Error(),
			Attempt:      attempt,
			MaxAttempts:  maxAttempts, // 总次数上限：前端显示「第 N/M 次失败 → 重试第 N+1/M 次」
			WillRetry:    willRetry,
			RetryDelayMs: delay.Milliseconds(),
			ErrorKind:    provider.ClassifyError(err), // context_exceeded 等：前端据此输出专门提示
			Usage:        partial,
			Timestamp:    time.Now(),
			EventType:    events.LLMErrorType,
		})
		if !willRetry {
			return err
		}
		// ctx 感知的退避等待：等待中取消 = 本轮以 ctx 错误结束（与 provider 层语义一致）
		select {
		case <-time.After(delay):
		case <-ac.ctx.Done():
			return ac.ctx.Err()
		}
	}
}

// retryable 判断错误是否值得重试：用户取消与永久错误不重试。
func retryable(err error) bool {
	return !errors.Is(err, context.Canceled) && !provider.IsPermanent(err)
}

// streamOnce 发起一轮模型调用，把 LLM 事件翻译为 Agent 事件，并聚合进 r。
// 返回值 receivedEnd 表示本轮是否收到过 LLMEndEvent。
// 超时策略（2026-09-18 定策，三层各管一段，勿混用）：
//   - 首字预算 FirstChunkTimeout（默认 30s，本函数实现）：请求发出 → 首个 provider 事件；
//     首个事件到达即解除，此后不再计时。命中 = 上游无响应 → 可重试的
//     provider.FirstChunkTimeoutError（见该类型注释：为什么必须可重试、为什么不能
//     以 context.Canceled 上抛）。
//   - 流内：**不限总时长**（长 reasoning / 大文档生成不受切）。
//   - LLMTimeout（Config）可选整轮挂死兜底：>0 时生效；bridge 装配传 0。
func (a *AgentLoop) streamOnce(ac *AgentContext, r *roundResult) (bool, error) {
	streamCtx := ac.ctx
	streamCancel := func() {}
	if a.cfg.LLMTimeout > 0 {
		streamCtx, streamCancel = context.WithTimeout(ac.ctx, a.cfg.LLMTimeout)
	}
	defer streamCancel()

	// 首字看门狗：到点 cancel 掉本次尝试（cause 带上策略值，供翻译/文案使用）。
	// 计时器在首字（首个 provider 事件）到达时 Stop —— 正常流只在开头这一小段被计时，
	// 之后的长流式与它无关（这就是「与整轮总时限不同」的实现落点）。
	streamCtx, cancelCause := context.WithCancelCause(streamCtx)
	defer cancelCause(context.Canceled) // 释放：cancel 函数必须被调用，避免 ctx 泄漏
	var firstChunkTimer *time.Timer
	if d := a.cfg.FirstChunkTimeout; d > 0 {
		firstChunkTimer = time.AfterFunc(d, func() {
			cancelCause(provider.NewFirstChunkTimeoutError(d, nil))
		})
		defer firstChunkTimer.Stop()
	}
	// 流内停滞看门狗（2026-09-22）：首个事件到达后才开始计时（「还没有第一个事件」归
	// 首字预算管，两段不重叠），此后每个事件重置；静默超过预算 → 以可重试的
	// provider.StreamStallError 终止本次尝试。语义/取值见 Config.StreamStallTimeout。
	stall := newStreamStallWatch(a.cfg.StreamStallTimeout, cancelCause)
	defer stall.stop()
	// gotFirstEvent 与 receivedEnd 同一线程假设：本仓 provider 都在 Stream 内同步调用
	// handler（SDK Recv 循环），故无需加锁。
	gotFirstEvent := false

	receivedEnd := false
	requestMessages := append([]core.Message(nil), ac.Messages...)
	// @引用展开（AgentHarness 层，2026-08）：用户消息原文里的 @./path 在请求前展开成
	// FilePath/Dir 文本块。展开后**同步写回 ac.Messages**（每条消息只展开一次，历史不
	// 重复展开 → 上下文不膨胀、refs_loaded 不重复）；user_inputs_consumed 事件已在展开
	// 前用原文发出（客户端回显原文），展开只影响 LLM 请求与后续恢复（历史不回显展开，
	// 用户已确认不做兼容）。
	// 只展开 [inputMark:]（本轮新增输入）；展开后 inputMark 前移到末尾。
	from := ac.inputMark
	if from < 0 {
		from = 0
	}
	if from < len(requestMessages) {
		expandUserRefsInMessages(ac, requestMessages[from:], a.cfg.WorkingDir, a.cfg.RefExtractor)
		// 展开结果只回写 [from:] 区间（避免整体替换导致后续 ephemeral append 污染
		// ac.Messages）；历史 [0:from] 保持原文/已展开内容不变。
		for i := from; i < len(requestMessages); i++ {
			ac.Messages[i] = requestMessages[i]
		}
		ac.inputMark = len(requestMessages) // 边界前移：本轮输入已展开
	}
	// 计数达到阈值后只注入一次；下一次有进展的工具批（写文件/todo/委派/验证命令）
	// 会在后续请求前把它重新打开。
	threshold := a.currentGoalAlignmentReminderRounds()
	if threshold > 0 && ac.goalAlignmentRounds >= threshold && !ac.goalAlignmentReminderInjected {
		requestMessages = append(requestMessages, core.Message{Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: prompt.GoalAlignmentReminder}}})
		ac.Handler(ac.ctx, &events.GoalAlignmentReminder{RunId: ac.RunId, Round: ac.goalAlignmentRounds, EventType: events.GoalAlignmentReminderType})
		ac.goalAlignmentReminderInjected = true
	}
	// 工具轮后空轮的续跑提醒（问题十三）：ephemeral user 消息，不落历史（同
	// GoalAlignmentReminder 模式），仅本次请求可见。
	if ac.postToolNudgePending {
		ac.postToolNudgePending = false
		requestMessages = append(requestMessages, core.Message{Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: prompt.PostToolContinueNudge}}})
	}
	ac.est.beginRequest(len(requestMessages)) // 压缩预估锚点：本次请求对应的消息数
	req := &provider.StreamRequest{
		Model:    a.currentModel(),
		Provider: a.cfg.ProviderName,
		Config: &provider.RequestConfig{
			Effort:          a.currentEffort(),
			Temperature:     a.cfg.Temperature,
			TopP:            a.cfg.TopP,
			MaxTokens:       a.currentMaxTokens(),
			RetainReasoning: a.cfg.RetainReasoning,
		},
		SessionID: a.cfg.SessionID, // Responses prompt cache key + session affinity（空 = 不启用）
		Messages:  requestMessages,
		Tools:     a.toolSchemas(streamCtx, ac.Mode),
	}

	err := a.cfg.Provider.Stream(streamCtx, req, func(e provider.StreamEvent) error {
		// 首字到达：解除看门狗（幂等，可重复 Stop）。此后本次尝试不再受首字预算约束 ——
		// 长 reasoning / 大文档生成跑到数分钟也不会被切（策略边界只到「首个事件」）。
		if !gotFirstEvent {
			gotFirstEvent = true
			if firstChunkTimer != nil {
				firstChunkTimer.Stop()
			}
		}
		// 中断守卫：abort 已请求 → 立即以 ctx 错误终止 provider 流，忽略其后缓冲内容。
		// 关键：go-openai 等 provider 的流式 reader 会 bufio 缓冲 SSE 行——ctx 取消只关闭
		// 连接，但已缓冲进内存的 reasoning/content 行仍会被 Recv 逐行吐出；若这里返回 nil，
		// 这些缓冲内容会继续 translate 成事件发给客户端（即「已中断后仍持续流式输出」）。
		// handler 返回错误时 provider 的 Stream 会立即 return，切断后续缓冲 chunk。
		if ac.IsAborted() {
			return ac.ctx.Err()
		}
		// 事件即活动：重置流内停滞看门狗（首个事件启动计时，其后每个事件重置）。
		stall.touch()
		a.translate(ac, e, &receivedEnd, r)
		return nil
	})
	// 停滞看门狗的取消在 provider 侧同样只表现为「context canceled」—— 按 cause 翻译成
	// 可重试的上游故障（理由同首字超时：绝不以 context.Canceled 上抛，会被 retryable()
	// 判成用户中断而不重试）。仅在流真以错误收场（err != nil）时替换：若本轮已收到
	// LLMEnd（err == nil），迟到的看门狗不得把一次成功的调用改判成失败。
	if err != nil {
		if stallErr, ok := context.Cause(streamCtx).(*provider.StreamStallError); ok {
			stallErr.SetCause(err)
			return receivedEnd, stallErr
		}
	}
	// 首字超时翻译：看门狗已经把 ctx cancel 掉，provider 侧只会报「context canceled」——
	// 直接上抛会被 retryable() 判成用户中断（不重试），也会让用户看到无信息量的取消文案。
	// 因此以 cause 为准替换成可重试的上游侧错误。仅在「一个事件都没到」时才替换：
	// 已收到首字说明是流内其它失败（或正常结束），与首字预算无关。
	if !gotFirstEvent {
		if ft, ok := context.Cause(streamCtx).(*provider.FirstChunkTimeoutError); ok {
			ft.SetCause(err) // 底层错误进文案（诊断用；取消语义由 SetCause 剥离）
			return receivedEnd, ft
		}
	}
	return receivedEnd, err
}

// translate 把 provider 的 LLM 事件翻译为 agents 层 Agent 事件（聚合进 r）。
func (a *AgentLoop) translate(ac *AgentContext, e provider.StreamEvent, receivedEnd *bool, r *roundResult) {
	switch ev := e.(type) {
	case provider.LLMStartEvent:
		ts := ev.Timestamp
		if ts.IsZero() {
			ts = time.Now() // provider 未设置时间戳时兜底
		}
		ac.Handler(ac.ctx, &events.LLMStart{
			RunId:     ac.RunId, // 事件自包含：多并发 agent 归属
			Model:     ev.Model,
			RequestId: ev.RequestId,
			Timestamp: ts,
			EventType: events.LLMStartType,
		})
	case provider.LLMContentDeltaEvent:
		ac.Handler(ac.ctx, &events.ContentChunk{
			RunId:     ac.RunId,
			RequestId: ev.RequestId,
			Index:     ev.Index,
			Content:   ev.Delta,
			EventType: events.ContentChunkType,
			Timestamp: time.Now(),
		})
	case provider.LLMReasoningDeltaEvent:
		ac.Handler(ac.ctx, &events.ReasoningChunk{
			RunId:     ac.RunId,
			RequestId: ev.RequestId,
			Index:     ev.Index,
			Content:   ev.Delta,
			EventType: events.ReasoningChunkType,
			Timestamp: time.Now(),
		})
	case provider.LLMToolCallStartEvent, provider.LLMToolCallDeltaEvent:
		// 工具调用增量不逐条透传，由 LLMEnd 聚合后统一触发工具执行
	case provider.LLMEndEvent:
		*receivedEnd = true
		r.toolCalls = ev.ToolCalls
		fr := ev.FinishReason
		if fr == "" {
			fr = core.FinishReasonStop // provider 未报告结束原因时按 stop 处理（防假成功边缘）
		}
		r.finishReason = fr
		r.content = ev.Content
		r.reasoning = ev.Reasoning
		r.reasoningSignature = ev.ReasoningSignature // Anthropic thinking signature（多轮回传）
		ac.addUsage(ev.Usage)
		ac.est.calibrate(&ev.Usage, ac.Messages) // 自适应校准字符→token 系数（压缩预估随运行收敛更准）
		if ev.Usage.Input > 0 {
			ac.est.observe(ev.Usage.Input) // 真实 input：下一轮压缩触发依据
		}
		usage := ev.Usage
		ts := ev.Timestamp
		if ts.IsZero() {
			ts = time.Now() // provider 未设置时间戳时兜底
		}
		// 零值用量不伪造（问题九）：上游未回 usage 时序列化为 null 而非 {"Input":0,…}，
		// 客户端据此保留上一轮上下文占用而非清零；聚合侧 addUsage(零值) 无副作用。
		var usagePtr *core.Usage
		if usage.Input > 0 || usage.Output > 0 || usage.Reasoning > 0 {
			u := usage
			usagePtr = &u
		}
		ac.Handler(ac.ctx, &events.LLMEnd{
			RunId:        ac.RunId, // 事件自包含：用量按 run 归集
			Content:      ev.Content,
			Reasoning:    ev.Reasoning,
			ToolCalls:    ev.ToolCalls,
			FinishReason: ev.FinishReason,
			Usage:        usagePtr,
			Model:        ev.Model,
			RequestId:    ev.RequestId,
			Timestamp:    ts,
			EventType:    events.LLMEndType,
		})
	}
}

// maybeCompact 每轮 LLM 调用前按预估 token 触发压缩（LLMStart 前检查点）。
// 压缩结果替换上下文；压缩调用的用量计入运行结算（Usage/Cost），失败降级为不压缩。
// forced = 强制压缩（命令框架：compact 命令），跳过 ShouldCompact 预估直接压缩；
// 空历史语义复用（无可压缩 → CompressEnd 带说明，不报错、不中断运行）。
// 压缩成功后：todo 默认不清空（S1-F，会话级独立状态；WithTodoResetOnCompact=true
// 时恢复旧语义：快照注入 + Clear + CompactTodoReminder）+ 注入统一交接提醒
// （CompactHandoffReminder，含文件账本/任务状态段）。
func (a *AgentLoop) maybeCompact(ac *AgentContext, opts *RunOptions, forced bool) {
	if a.cfg.Compressor == nil {
		return
	}
	// R3/C3.2 失败退避：上次自动压缩失败后的窗口期内不再自动压缩（forced 手动
	// 压缩不受限）——防"失败 → 上下文继续涨 → 立刻重试"的失控循环。
	if !forced && a.cfg.CompactBackoff > 0 && !ac.compactBackoffUntil.IsZero() && time.Now().Before(ac.compactBackoffUntil) {
		return
	}

	// PreCompact 注入（E）：压缩前保留关键上下文 —— 注入串作为 assistant 消息
	// 插在最后一段连续 user 块之前（进压缩输入、出压缩输出，见 injectPreCompactContext）；
	// panic 兜底按空串处理（无注入）。压缩失败降级时注入串随压缩一起丢弃（无副作用）。
	messages := ac.Messages
	if opts != nil && opts.PreCompact != nil {
		inject, panicked := callPreCompact(opts.PreCompact, ac.ctx)
		if panicked {
			toolHookWarning(ac, "PreCompact", "hook panicked, treated as empty")
		}
		if inject != "" {
			messages = injectPreCompactContext(messages, inject)
		}
	}
	// 引擎侧文件账本（S1-B）注入压缩输入（与 PreCompact 同位置）：摘要 LLM 可见
	// [FileLedger] 权威清单（SummaryPrompt 条款要求照抄），File Context 不再依赖
	// 模型从工具参数里自行提取。注入串进压缩输入、出压缩输出（输出侧由
	// CompactHandoffReminder 携带同一清单）。
	if ac.fileLedger != nil {
		if ledger := ac.fileLedger.render(); ledger != "" {
			messages = injectPreCompactContext(messages, ledger)
		}
	}
	// @引用展开（AgentHarness 层）：压缩输入必须与 LLM 请求同口径——压缩器看到
	// 的是展开后上下文（FilePath/Dir 块），摘要才不丢被引用文件语义；否则压缩后
	// 模型上下文丢失引用内容（与 streamOnce 请求展开同函数，幂等）。
	// 只展开 [inputMark:]（本轮新增），历史引用不重复展开（与 streamOnce 对齐）；
	// 此处不发 refs_loaded 事件（压缩路径非用户可见的请求展开，避免重复通知）。
	if len(messages) > 0 {
		expanded := append([]core.Message(nil), messages...)
		from := ac.inputMark
		if from < 0 {
			from = 0
		}
		if from < len(expanded) {
			expandUserRefsInMessagesEmit(ac, expanded[from:], a.cfg.WorkingDir, a.cfg.RefExtractor, false)
		}
		messages = expanded
	}
	// 压缩元数据三块双侧注入·输入侧（L3/§10.6/§7B B3）：[FileLedger] 之后依次
	// 注入 [TodoSnapshot]（SummaryPromptV2 Task List 节的逐字照抄源）与 [TaskStates]
	// （Background Tasks 权威源，同时是 B3 动态护栏的触发条件）。TodoResetOnCompact=true
	// 的兼容路径不注入 [TodoSnapshot]——快照由 CompactTodoReminder 携带，防双份；
	// [TaskStates] 不受该开关影响。空块省略（无 todo / 无任务 = 不注入）。
	if !a.cfg.TodoResetOnCompact && ac.Todo != nil && len(ac.Todo.Items()) > 0 {
		messages = injectPreCompactContext(messages,
			"[TodoSnapshot] (authoritative session task list)\n"+ac.Todo.List())
	}
	if opts != nil && opts.TaskState != nil {
		if snap := opts.TaskState(); snap != "" {
			messages = injectPreCompactContext(messages, snap)
		}
	}

	estimated := ac.est.estimate(messages)
	if a.cfg.CompressMargin > 0 {
		// 安全余量：宁早勿晚，防预估低估导致超窗
		estimated = int64(float64(estimated) * (1 + a.cfg.CompressMargin))
	}
	stats := CompressStats{
		EstimatedTokens: estimated,
		Budget:          int64(float64(a.currentContextWindow()) * a.cfg.CompressThreshold),
	}
	if !forced && !a.cfg.Compressor.ShouldCompact(ac.ctx, stats) {
		return
	}

	// before 用原上下文长度（不含注入串——注入串不持久化，事件语义对齐压缩产物）
	before := len(ac.Messages)
	reason := "automatic"
	if forced {
		reason = "manual"
	}
	ac.Handler(ac.ctx, &events.CompressStart{Before: before, Reason: reason, RunId: ac.RunId, Timestamp: time.Now(), EventType: events.CompressStartType})
	result, err := a.cfg.Compressor.Compact(ac.ctx, messages)
	if err != nil {
		// 失败退避（R3/C3.2）：记录退避截止，窗口内不再自动重试
		if a.cfg.CompactBackoff > 0 {
			ac.compactBackoffUntil = time.Now().Add(a.cfg.CompactBackoff)
		}
		// 失败压缩的**已产生用量**也要进账（2026-09-23 决策「失败尝试也要记账」）：
		// 压缩输入是整段上下文，失败一次可能白烧大量 input token —— 事件带 Usage，
		// 会话成本（reducer）与 metrics 都不丢；SDK 侧同步累计（与成功路径同口径）。
		var failUsage *core.Usage
		if !result.FailedUsage.IsZero() {
			ac.addUsage(result.FailedUsage)
			u := result.FailedUsage
			failUsage = &u
		}
		// 失败降级：保持原上下文（绝不丢消息），错误经事件暴露，下轮再试
		ac.Handler(ac.ctx, &events.CompressEnd{
			Before: before,
			After:  before,
			Reason: reason,
			RunId:  ac.RunId,
			// 未变上下文大小（请求组装口径：含工具/MCP schema 与系统提示词，
			// 与 ctx 分区面板同源 —— 只算消息会漏掉几千 token 的前缀）
			CtxTokens: a.RequestTokens(ac.ctx, ac.Messages, ac.Mode),
			Usage:     failUsage,
			Error:     err.Error(),
			Timestamp: time.Now(),
			EventType: events.CompressEndType,
		})
		return
	}
	ac.Messages = result.Messages
	ac.inputMark = len(ac.Messages) // 压缩替换 Messages：@展开边界重置（摘要无待展开原文）
	// 压缩后统一交接提醒（S1-B/S1-E，替代单句 CompactContinueReminder 的注入定位）：
	// 模型看到 [system, 摘要, TaskAnchorReminder?, 近窗?, 最新输入]，提醒它从摘要继续、
	// 不重启重做；原始任务目标以 TaskAnchorReminder 为权威；文件账本/在途任务状态/
	// todo 快照附在段尾（任一为空则省略）。H2：兼容单 system 网关时可配 user 角色。
	handoff := core.NewSystemMessage(a.renderCompactHandoff(ac, opts))
	if a.cfg.HandoffReminderAsUser {
		handoff = core.NewUserMessage(core.Content{Type: "text", Content: a.renderCompactHandoff(ac, opts)})
	}
	ac.Messages = append(ac.Messages, handoff)
	// 会话 todo 压缩时不清空（S1-F）：todo 是会话级独立状态（Session.State.Todo
	// 持久化、UI 面板数据源），压缩只是替换 Messages，清空没有技术必要性——
	// 默认不清空、不注入快照。向后兼容：WithTodoResetOnCompact(true) 恢复旧语义
	//（快照注入 + Clear + CompactTodoReminder，模型用 todo_add/todo_update 重建）。
	if ac.Todo != nil && a.cfg.TodoResetOnCompact {
		if items := ac.Todo.Items(); len(items) > 0 {
			snapshot := ac.Todo.List()
			ac.Todo.Clear()
			ac.Messages = append(ac.Messages, core.NewSystemMessage(fmt.Sprintf(prompt.CompactTodoReminder, snapshot)))
		}
	}
	// 压缩后 slim 状态重建（C）：Messages 整体替换，readIndex/roundStart 存的下标
	// 全部失效——不 reset 会按失效下标改写错误消息（可能改写摘要/system）
	if ac.slim != nil {
		ac.slim.reset()
	}
	// 压缩成本计入运行结算（用户规格：压缩计入成本）
	ac.addUsage(result.Usage)
	// 失败重试的已产生用量同样进账（上游已计费）—— 但**不进锚点**（est.observe 只用
	// 成功那次的 Input，混入失败尝试会让压缩后上下文锚点虚高、触发错乱）。
	if !result.FailedUsage.IsZero() {
		ac.addUsage(result.FailedUsage)
	}
	// 压缩后上下文的基础 = 压缩调用输入（后续预估延续精确值）
	ac.est.observe(result.Usage.Input) // 压缩后锚点重置为压缩调用真实输入
	// 事件口径：本次压缩**总花费**（成功那次 + 失败重试的已产生用量）—— 展示与 metrics
	// 都按这个数看；单轮口径（哪次有价）在 metrics 行里逐条可查。
	evUsage := result.Usage
	if !result.FailedUsage.IsZero() {
		evUsage = evUsage.Add(result.FailedUsage)
	}
	ac.Handler(ac.ctx, &events.CompressEnd{
		Before: before,
		After:  len(ac.Messages),
		Reason: reason,
		RunId:  ac.RunId,
		// 压后有效上下文大小（前端 ctx 环数据源；请求组装口径，含前缀 —— 见 RequestTokens）
		CtxTokens: a.RequestTokens(ac.ctx, ac.Messages, ac.Mode),
		Model:     result.Model,
		Summary:   result.Summary,
		Usage:     &evUsage,
		Warnings:  result.Warnings,
		Analysis:  result.Analysis, // O1/G7：压缩器决策过程留档（仅观测）
		Timestamp: time.Now(),
		EventType: events.CompressEndType,
	})
}

// estimator 压缩触发的 token 预估状态（值对象，AgentContext.est 持有）：
// 上轮请求的精确 Input 锚点 + 字符→token 自适应系数 —— 结合 Usage 精确值与
// 用户输入的实际字符密度，预估随运行收敛更准。
type estimator struct {
	ratio           float64 // 字符→token 系数（先验 4.0；保留仅作观测，无读者）
	lastInputTokens int64   // 上轮请求的精确 Input token（压缩触发唯一依据）
	requestMsgCount int     // 本次请求的消息数（校准分母与锚点对应）
}

// beginRequest 记录本次请求的消息数（streamOnce 发起时调用）。
func (e *estimator) beginRequest(msgCount int) { e.requestMsgCount = msgCount }

// observe 记录上轮请求的精确 Input（LLMEnd 后调用）——压缩触发的真实 usage 锚点。
func (e *estimator) observe(input int64) {
	e.lastInputTokens = input
}

// calibrate 自适应校准字符→token 系数（指数平滑，抑制单轮噪声）。
// 注意：2026-08-30 起 estimate 改用「真实 usage + 固定 /4」策略，ratio 不再参与
// 压缩预估（实测自适应在中文/代码场景偏低导致高估提前压缩）。calibrate 保留
// 仅作观测（ratio 字段暂无人读），后续可整体清理。
// 观察值 = 本轮请求消息的字符数 / 实际 Input token。异常值钳制（字符/token 合理区间
// [0.5, 16]，防超短请求或畸形 usage 带偏）；首轮向 4.0 先验平滑（单轮小样本直接采用会带偏）。
func (e *estimator) calibrate(usage *core.Usage, msgs []core.Message) {
	if usage == nil || usage.Input <= 0 || e.requestMsgCount <= 0 {
		return
	}
	n := e.requestMsgCount
	if n > len(msgs) {
		n = len(msgs)
	}
	chars := int64(0)
	for i := 0; i < n; i++ {
		chars += messageChars(msgs[i])
	}
	if chars <= 0 {
		return
	}
	observed := float64(chars) / float64(usage.Input)
	if observed < 0.5 || observed > 16 {
		return
	}
	if e.ratio <= 0 {
		e.ratio = 4.0
	}
	e.ratio = 0.7*e.ratio + 0.3*observed
}

// estimate 返回当前上下文 token：**完全以上轮请求的真实 Input（usage 精确值）为准**，
// 不叠加任何新增消息估算——压缩触发只信真实 usage，杜绝预估误差。
// 首轮（尚无真实 usage 锚点）冷启动用全量字符/4 估算兜底（此时尚未有任何精确值，
// 只能估算；一旦上轮 LLM 调用返回，后续全部走真实值）。
func (e *estimator) estimate(msgs []core.Message) int64 {
	if e.lastInputTokens > 0 {
		return e.lastInputTokens
	}
	return int64(float64(messagesChars(msgs)) / 4)
}

// estimateMessagesTokens 消息集估算（字符/4 近似 token，固定系数 —— 用于
// LatestUserMaxTokens 等独立阈值判断，不参与压缩预估的自适应）。
func estimateMessagesTokens(msgs []core.Message) int64 {
	return int64(float64(messagesChars(msgs)) / 4)
}

// 图片视觉 Token 估算：DeepSeek vision 模型按图像分块计费，不能把 data URL/Base64
// 字符长度当作文本。单图使用保守的 1600 visual tokens，参与上下文压缩预估。
const estimatedImageTokens = int64(1600)

func messageChars(m core.Message) int64 {
	var chars int64
	for _, c := range m.Content {
		if c.Type == core.ContentTypeImage {
			chars += estimatedImageTokens * 4 // 与字符/token 估算口径保持一致
			continue
		}
		chars += int64(len(c.Content))
	}
	chars += int64(len(m.Reasoning))
	for _, tc := range m.ToolCalls {
		chars += int64(len(tc.Name) + len(tc.Arguments))
	}
	return chars
}

// messagesChars 消息集字符数合计。
func messagesChars(msgs []core.Message) int64 {
	var n int64
	for _, m := range msgs {
		n += messageChars(m)
	}
	return n
}

// toolCallKey 工具调用去重键：sha256(工具名 \0 参数) 的 hex。
// 同一 (name,args) 在**不同轮**里产生同一个键——正是「同一调用重复失败」的判定对象
// （C5 用；键本身不落盘、不进事件，只在 Run 生命周期内使用）。
func toolCallKey(name, args string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + args))
	return hex.EncodeToString(sum[:])
}

// runTools 按 CanParallel 分组执行工具调用；引擎缺失或没有可执行工具时返回 nil（调用方终止本轮）。
//
// 执行链（含 E hooks）：分桶（seq/par/blocked）→ 模式过滤 AllowTool → PreToolUse
// （可 Block / 改写参数）→ WithToolContext → 引擎执行（内部审批：看到改写后参数）→
// PostToolBatch（批后策略性终止）。hooks 顺序 = 调用顺序（确定性）。
func (a *AgentLoop) runTools(ac *AgentContext, opts *RunOptions, calls []core.ToolCall) ([]core.ToolResult, error) {
	if a.cfg.ToolEngine == nil {
		return nil, nil
	}

	// C5：工具轮序号推进（本函数一次调用 = 一轮工具轮）。去重判定与失败记账都以它
	// 为「紧邻上一轮工具轮」的判据（ac.toolRound-1）；引擎缺失/无可执行工具时上面
	// 已提前返回，不推进——没执行过工具就不是工具轮。
	ac.toolRound++

	// 并发上限单一配置源（V2 P2-RUNTIME-04）：WithMaxParallelTools 是 AgentLoop 的
	// 对外配置，但实际并发由引擎内部 maxParallel 控制——首次执行时把配置同步给引擎
	//（引擎实现 SetMaxParallel 接口才生效；mock 引擎无此方法则保持自身默认）。
	a.maxParallelOnce.Do(func() {
		if e, ok := a.cfg.ToolEngine.(interface{ SetMaxParallel(int) }); ok {
			e.SetMaxParallel(a.cfg.MaxParallelTools)
		}
	})

	type blockedCall struct {
		call   core.ToolCall
		reason string
	}
	var seq, par []core.ToolCall
	var blockedCalls []blockedCall       // 模式/hook 拒绝（IsError 回传，不走审批）
	var blockedResults []core.ToolResult // 拒绝结果（调用方按 Id 回写，顺序无关）
	for _, c := range calls {
		// C5 去重守卫（换键后的拦截条件）：键仍是 (工具名, 参数) 的 sha256，但命中要求
		// **该键在紧邻的上一轮工具轮里也失败过**且其间无状态变更（清理规则保证后者）。
		// 刻意收窄：本 Run 内更早的失败不拦（`go build`/`go test` 的合法重跑），
		// RetryWithSameArguments=true 的信封永不拦（既有语义），同轮内重复不拦。
		key := toolCallKey(c.Name, c.Arguments)
		if prev, ok := ac.failures.blockedIn(key, ac.toolRound); ok {
			blockedResults = append(blockedResults, prev.blockedResult(c.Id))
			continue
		}
		t, err := a.cfg.ToolEngine.GetTool(ac.ctx, c.Name)
		if err != nil {
			// 未注册的工具：交给引擎标 IsError 结果
			// 破坏 assistant+tool_calls 配对（DeepSeek 等要求完整回传，否则 400）
			seq = append(seq, c)
			continue
		}
		if ac.Mode != nil && ac.Mode.AllowTool != nil && !ac.Mode.AllowTool(t) {
			// 模式防御与 schema 过滤同源（同一份 AllowTool）：plan 模式语义是
			// 禁止而非待审，直接拒绝结果回传，模型可见后自行调整。
			// 模式过滤在 hook 之前（hook 不得放宽模式硬约束——allow 不绕过 Block）
			blockedCalls = append(blockedCalls, blockedCall{c, "blocked: tool is not available in the current mode"})
			continue
		}

		// PreToolUse（E）：模式过滤后、执行前（审批在引擎内 preApprove，看到改写后
		// 参数——安全语义正确）。Block → 并入拒绝桶（与模式 block 同构：IsError 回传）；
		// UpdatedArguments → 改写执行参数（历史 ToolCall 保留原参，审计经
		// ToolResponse.Arguments 查实际执行）；非法 JSON → 回退原参 + 警告事件；
		// 回调 panic → 按 allow 处理 + 警告事件（不 kill 运行）。
		if opts != nil && opts.PreToolUse != nil {
			res, panicked := callPreToolUse(opts.PreToolUse, ac.ctx, events.PreToolUseContext{
				Call:      c,
				Arguments: c.Arguments,
			})
			if panicked {
				toolHookWarning(ac, "PreToolUse:"+c.Name, "hook panicked, treated as allow")
			}
			switch res.Decision {
			case events.ToolDecisionBlock:
				reason := res.Reason
				if reason == "" {
					reason = "blocked by PreToolUse hook"
				}
				blockedCalls = append(blockedCalls, blockedCall{c, reason})
				continue
			default:
				if res.UpdatedArguments != "" && res.UpdatedArguments != c.Arguments {
					if json.Valid([]byte(res.UpdatedArguments)) {
						c.Arguments = res.UpdatedArguments
					} else {
						// 非法 JSON：回退原参 + 事件警告（不 kill）
						toolHookWarning(ac, "PreToolUse:"+c.Name,
							"invalid UpdatedArguments JSON, using original arguments")
					}
				}
			}
		}

		if t.CanParallel() {
			par = append(par, c)
		} else {
			seq = append(seq, c)
		}
	}
	if len(blockedCalls) > 0 {
		results := make([]core.ToolResult, 0, len(blockedCalls))
		for _, b := range blockedCalls {
			results = append(results, core.ToolResult{
				Id:      b.call.Id,
				Result:  b.reason,
				IsError: true,
			})
		}
		if len(seq)+len(par) == 0 {
			return results, nil
		}
		blockedResults = results
	}
	if len(seq)+len(par) == 0 {
		// 全部调用都被去重守卫拦下：拦截结果**必须回传**。否则调用方 `results == nil`
		// 直接把本轮当作「无结果」结束运行（`if err != nil || results == nil { break }`），
		// 模型永远看不到拦截文案 —— 守卫退化成一次静默终止，与「拦截必须自带可操作出路」
		// 的设计意图正好相反。与上面模式/hook 拒绝桶完全同构（那条路径同样 return results）。
		if len(blockedResults) > 0 {
			return blockedResults, nil
		}
		return nil, nil
	}

	// 整批工具执行超时预算；注入工具上下文（子 agent 类工具据此关联父事件流/继承
	// hooks）与会话待办/审批
	var hooks *events.ToolHooks
	if opts != nil {
		hooks = &events.ToolHooks{
			PreToolUse:    opts.PreToolUse,
			PostToolBatch: opts.PostToolBatch,
			PreCompact:    opts.PreCompact,
		}
	}
	ctx := events.WithToolContextForRun(ac.ctx, ac.RunId, ac.ParentRunId, ac.Depth, ac.Handler, ac.Approver, hooks)
	ctx = todo.WithStore(ctx, ac.Todo)
	cancel := func() {}
	if a.cfg.ToolBatchTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, a.cfg.ToolBatchTimeout)
	}
	defer cancel()

	// 批控制器：运行时 promote 的摘离入口（Session.PromoteTask → ac.PromoteToolTask →
	// batch.Promote）与精确中断入口（Session.InterruptTask → batch.Interrupt）。
	// onDone = TaskSink（Session 接线 → PushTaskResultStatus 自动回传）；nil = 摘离结果仅
	// 事件（ToolResponse）、不推回。生命周期：BatchCreated 注册（任务可被精确中断）→
	// 批内全部摘离任务交付完成 → BatchClosed 注销（防 Session 注册表泄漏）。
	var onDone func(taskID string, r core.ToolResult, interrupted bool)
	var promoteAfter time.Duration
	if opts != nil {
		onDone = opts.TaskSink
		promoteAfter = opts.AsyncPromoteAfter
	}
	batch := tools.NewBatchController(len(seq)+len(par), onDone)
	if opts != nil {
		if opts.BatchCreated != nil {
			opts.BatchCreated(batch)
		}
		if opts.BatchClosed != nil {
			batch.SetOnIdle(opts.BatchClosed)
		}
	}
	ac.batch.Store(batch)
	defer ac.batch.Store(nil)

	var results []core.ToolResult
	if len(seq) > 0 {
		r, err := a.cfg.ToolEngine.RunBatch(ctx, seq, ac.Handler, nil, batch, promoteAfter)
		if err != nil {
			return nil, err
		}
		results = append(results, r...)
	}
	if len(par) > 0 {
		r, err := a.cfg.ToolEngine.RunBatch(ctx, par, ac.Handler, nil, batch, promoteAfter)
		if err != nil {
			return nil, err
		}
		results = append(results, r...)
	}
	// 子 agent 类工具的用量计入父运行结算（成本传导 → AgentEnd/Session 预算）
	for _, r := range results {
		ac.addUsage(r.Usage)
	}
	// 失败账本落账（C5 去重索引 + C6 交接段共享同一份记录）：**所有** IsError 结果都记
	// （不要求 ToolError 信封——bash/散文错误因此才进得来；信封可用时额外留
	// next_action/fix 供拦截文案用），随后按状态变更清理（见 recordToolOutcomes 注释）。
	// 位置与旧记录点一致（批末、回写前）：账本描述的是「这一轮工具执行发生了什么」。
	recordToolOutcomes(ac, calls, results)
	if len(blockedResults) > 0 {
		results = append(blockedResults, results...)
	}

	// PostToolBatch（E）：批后收尾检查点——true = 策略性终止本轮运行
	//（ac.Stop()：AgentEnd finishReason=stop 非失败；panic 兜底按 false）。
	// 工具结果已全部回传（调用方按 Id 回写），stop 轮上下文完整。
	if opts != nil && opts.PostToolBatch != nil {
		nameById := make(map[string]string, len(calls))
		for _, c := range calls {
			nameById[c.Id] = c.Name
		}
		batch := make([]events.ToolRunResult, 0, len(results))
		for _, r := range results {
			batch = append(batch, events.ToolRunResult{
				Name:    nameById[r.Id],
				CallID:  r.Id,
				IsError: r.IsError,
				Result:  r.Result,
			})
		}
		stop, panicked := callPostToolBatch(opts.PostToolBatch, ac.ctx, batch)
		if panicked {
			toolHookWarning(ac, "PostToolBatch", "hook panicked, treated as false")
		}
		if stop {
			ac.Stop()
		}
	}

	// PostToolUse（hooks 能力扩展）：**逐工具**结果落定后回调 —— 注入串追加到该结果
	// 文本尾部，模型下一轮必然读到（Graft 的 blast radius「谁依赖刚改的文件」挂这里）。
	// 位置在 PostToolBatch 之后：批级回调看到的是原始结果（其结果语义不变），注入只影响
	// 回写上下文的内容。被拦下的调用（blockedResults，未真正执行）不派发 —— 与
	// Claude Code 的 PostToolUse=「成功执行后」语义一致（失败/拒绝另有事件位）。
	if opts != nil && opts.PostToolUse != nil {
		blockedIDs := make(map[string]struct{}, len(blockedResults))
		for _, r := range blockedResults {
			blockedIDs[r.Id] = struct{}{}
		}
		callById := make(map[string]core.ToolCall, len(calls))
		for _, c := range calls {
			callById[c.Id] = c
		}
		for i := range results {
			if _, blocked := blockedIDs[results[i].Id]; blocked {
				continue
			}
			c, ok := callById[results[i].Id]
			if !ok {
				continue // 无对应调用（引擎合成结果等）：不派发
			}
			res, panicked := callPostToolUse(opts.PostToolUse, ac.ctx, events.PostToolUseContext{
				Call:      c,
				Arguments: c.Arguments,
				Result:    results[i].Result,
				IsError:   results[i].IsError,
			})
			if panicked {
				toolHookWarning(ac, "PostToolUse:"+c.Name, "hook panicked, injection skipped")
				continue
			}
			if res.ReplaceResult != "" && !results[i].IsError {
				results[i].Result = res.ReplaceResult
			}
			if res.AdditionalContext != "" {
				if results[i].Result != "" {
					results[i].Result += "\n\n"
				}
				results[i].Result += res.AdditionalContext
			}
		}
	}
	return results, nil
}

// toolSchemas 收集已注册工具的厂商无关 schema，供 provider 生成工具定义。
// toolSchemas 返回注入请求的工具 schema，按模式过滤并保持稳定序：
//   - 引擎已按名字典序输出（缓存命中的前提，见 Engine.ToolParams）；
//   - 只读桶（ReadOnlyTool 声明）恒在前、修改桶在后 —— plan/normal 共享只读前缀，
//     模式切换的缓存失配点被压到修改桶（+ 尾部 SystemHint）；
//   - mode.AllowTool 返回 false 的工具整体不注入（LLM 不可见）。
func (a *AgentLoop) toolSchemas(ctx context.Context, mode *Mode) []core.ToolSchema {
	if a.cfg.ToolEngine == nil {
		return nil
	}
	schemas, err := a.cfg.ToolEngine.ToolParams(ctx)
	if err != nil {
		return nil
	}
	// 分桶恒生效（只读桶在前）：plan/normal 共享只读前缀，模式切换的缓存
	// 失配点被压到修改桶。AllowTool nil = 不过滤但同样分桶。
	readOnly := make([]core.ToolSchema, 0, len(schemas))
	mutation := make([]core.ToolSchema, 0, len(schemas))
	for _, s := range schemas {
		t, err := a.cfg.ToolEngine.GetTool(ctx, s.Name)
		if err != nil {
			continue
		}
		if mode != nil && mode.AllowTool != nil && !mode.AllowTool(t) {
			continue // 模式不可见
		}
		if isReadOnly(t) {
			readOnly = append(readOnly, s)
		} else {
			mutation = append(mutation, s)
		}
	}
	return append(readOnly, mutation...) // 只读桶在前，修改桶在后
}

// addUsage 把单轮用量与成本累加到 AgentContext（Usage.Cost 与 Cost 保持同步，
// 子 agent 等经 ToolResult.Usage 的传导同样落入两个字段）。
func (ac *AgentContext) addUsage(u core.Usage) {
	ac.Usage = ac.Usage.Add(u)
}
