import { create } from 'zustand'
import { getTransport } from '../transport'
import type { BridgeExitInfo, AppSettings, ProviderSaveInput, ProviderCfg, ProviderPreset, MCPServerCfg, CronJob, CronRun, CronRunDetail, ExternalSkillsStatus, ExternalSkillsImportResult, OAuthStatus, IMConfig, IMGatewaySchema, IMChat, IMBindRequest, IMChatInfo, STTSettings, MeshSettings, MeshStatusInfo, ModelPrices, AgentSettings } from '../transport/types'
import type { AnyEvent, BridgeCommand } from './events'
import type { CheckpointView } from './checkpoint'
import { TRACE_SPAN_TYPES } from './trace'
import type { TraceSpan, TraceSummary, TraceDetail } from './trace'
import { toolNodeLabel } from '../lib/toolSummary'
import { parseQuestionOptions, type QuestionOption } from '../lib/questionOptions'
import { visualKindOf, isVisualPath } from '../lib/visualFile'
import { dataUrlBytes, fmtBytes, MAX_MESSAGE_IMAGE_BYTES } from '../lib/imageAttach'
import { t as i18nT } from '../i18n'

export type RightTab = 'activity' | 'git' | 'agent' | 'file' | 'web'

// —— 右侧栏标签模型（docs/RIGHT_PANEL_TABS_PLAN.md 阶段 1）——
//
// 分工（阶段 1 刻意保留双轨，行为等价重构）：
//   · rightTab  = 「当前显示哪一类内容」的**权威**字段。e2e 直接读写它
//     （verify_auto_open_visual.mjs:114 用它做用例复位、verify_pdf_preview.mjs:284 订阅它做诊断），
//     且它经 SessionView 参与会话级恢复 —— 动它就等于同时改这两条契约。
//   · tabsByScope = 「有哪些标签、顺序、激活的是哪个实例、最近关了哪些」。
//   两者的一致性由 syncTabsToRightTab **单向收敛**（标签集跟随 rightTab）：rightTab 有多条
//   改写路径（动作 / loadViewToTop 恢复 / e2e 直接 setState），逐个调用点同步必漏。
export type ScopeKey = string // 会话 id；无会话时退化为工作区路径（D7）
export type TabKind = RightTab // 复用 5 个既有取值：e2e 断言与 i18n key（rp.tab.*）都按它走
// file 标签是**多实例**的（阶段 2）：身份 = (path, mode)，同一文件可以同时有 diff 与
// source 两个标签（D5），但同一 (path, mode) 只有一个实例。openSeq 是「每次打开都回默认
// 视图」的触发器 —— 阶段 1 靠全局 fileFocus.seq，阶段 2 起内容与视图都归标签所有，
// 触发器也必须跟着标签走（否则重开 A 会把 B 的视图复位）。
export type FileTabInstance = { id: string; kind: 'file'; path?: string; mode?: 'diff' | 'source'; toolId?: string; startLine?: number; openSeq?: number }
// web 标签的两种形态（阶段 5 起）：
//   · 活的：有池身份（viewId = 原生视图池的 'tN'，或 pageId = MCP 页列表的数字 id）；
//   · 待重建的（重启恢复出来的）：**没有**池身份，只有 url 快照 —— 池身份是运行时 id
//     （主进程 't'+自增序号，重启后从 t1 重来），存下来只会指向另一个页面。
export type WebTabInstance = { id: string; kind: 'web'; viewId?: string; pageId?: number; srcPath?: string; url?: string }
export type TabInstance =
  | { id: string; kind: 'activity' }
  | { id: string; kind: 'git' }
  | { id: string; kind: 'agent' }
  | FileTabInstance
  | WebTabInstance
export interface TabSet {
  tabs: TabInstance[]
  activeId: string | null // null = 用户把标签关光了（显示空态，不自动补默认标签）
  closed: TabInstance[] // 最近关闭（⌄ 下拉「重开」），上限 TAB_CLOSED_MAX
  // 阶段 5：本次进程内**刚重启恢复**、还没被任何动作碰过（见 syncTabsToRightTab 的反向收敛）。
  // 不持久化，用完即清。
  restored?: boolean
}
export const TAB_CLOSED_MAX = 10
// 标签条同时可见的上限（超出收窄 + 「+N」，与 R4 的 PAGES_MAX_VISIBLE 同口径）。
export const TABS_MAX_VISIBLE = 5
// TabSpec = 标签实例去掉 id（Omit 在联合类型上不分配，故用分配式 Omit）。
type DistributiveOmit<T, K extends PropertyKey> = T extends unknown ? Omit<T, K> : never
export type TabSpec = DistributiveOmit<TabInstance, 'id'>

// traceEvents 上限：单会话保留的 span 类事件条数。实测单 trace span 中位 23、p99 669、
// 最大 3,111，故 20k 足以覆盖「当前 trace + 若干历史 trace」的实时投影；
// 超出从头丢弃（旧事件已可由 bridge 按需拉取，不必常驻内存）。
export const TRACE_EVENTS_MAX = 20000

// 全局 Toast 状态（showToast 写入；ToastView 渲染并自动消失）。
export interface ToastState {
  msg: string
  kind: 'ok' | 'error'
  id: number // 每次弹唯一（ToastView 据此重置计时器）
}

// 内置 provider 预设清单（对齐 SettingsModal BUILTIN_PROVIDERS；addProvider 判断
// 是否该用 minimal compat 默认 —— 内置/preset 走官方完整能力，只有自定义走 minimal）。
export const BUILTIN_PROVIDER_IDS = ['deepseek', 'openai', 'opencode', 'kimi', 'zhipu', 'anthropic', 'groq', 'xai', 'mistral', 'minimax', 'qwen', 'xiaomi', 'together', 'fireworks', 'cerebras', 'baseten', 'nvidia', 'huggingface', 'openrouter', 'openai-codex', 'github-copilot', 'kimi-coding']

export interface GitStatus { is_repo: boolean; root?: string; branch?: string; head?: string; detached?: boolean; ahead: number; behind: number; staged: number; modified: number; untracked: number; conflicted: number; clean: boolean }
export interface GitFile { path: string; status: string; staged: boolean; additions: number; deletions: number; diff?: string; changedAt?: number }
export interface GitCommit { hash: string; short_hash: string; subject: string; author: string; date: string; diff?: string; graph?: string; graph_after?: string }
export interface GitBranchInfo { name: string; current: boolean }


// 每轮 LLM 调用指标（llm_end 一条；L2a 实时单会话指标数据源；runId 归属 → 链路 tab 按 trace 汇总）
export interface TurnMetric {
  model: string
  input: number
  output: number
  cacheRead: number
  cacheWrite: number // 本轮写入缓存量（Anthropic cache_creation；其余协议恒 0）
  reasoning: number
  durMs?: number // llm_start → llm_end（流式时长）
  thinkMs?: number // 思考耗时（首个 reasoning_chunk → 首个 content_chunk / llm_end）
  costUsd: number // 桥按价表打标（cost_usd）
  runId?: string // 归属 run（llm_end 的 run_id；'' = 主 agent；子 agent 为自己的 run_id）
}

// 跨会话聚合看板（bridge metrics 命令报告；字段对齐 Go MetricsReport，大写）
export interface MetricsTotals {
  Input: number
  // Output 总输出（含 Reasoning）；Reasoning 是它的**拆分视图**（勿相加）——
  // 统计页据此显示思考占比（2026-09-23 补齐维度）
  Output: number
  Reasoning?: number
  CacheRead: number
  CacheWrite: number // 写入缓存量（Anthropic cache_creation；其余端点 0）
  CacheWrite1h?: number // ⊆ CacheWrite：1h extended TTL 写入量（Anthropic 专用）
  CostUsd: number
  // UnpricedCalls 本组无可用价的调用数（Cost.Priced=false 且非旧行）：成本 0 是
  // 「不知道花了多少」而非免费 —— 展示层据此标注该组成本偏低（如某模型未收录价）。
  UnpricedCalls?: number
}
export interface MetricsGroup extends MetricsTotals {
  Key: string
}
// 每日合计（热力图/趋势图数据源）。CumTokens = 从最早记录累计到该天的 token（「累计」视图，
// 不受查询窗口截断 —— 窗口只决定画哪些天）。
export interface MetricsDayGroup extends MetricsGroup {
  CumTokens: number
}
// 每日 × 模型（趋势图分模型折线）。Model = "provider/model"（无 provider → 裸模型名）。
export interface MetricsDayModelGroup extends MetricsTotals {
  Day: string
  Model: string
}
// 全局统计（顶部卡片）：全历史口径，不受 from/to 影响（Workspace 过滤生效）。
export interface MetricsStats {
  TotalTokens: number
  TotalCostUsd: number
  PeakDayTokens: number
  PeakDay: string
  LongestChatMs: number
  LongestChatAt: string
  CurrentStreak: number
  LongestStreak: number
  ActiveDays: number
  Sessions: number
  FirstDay: string
  LastDay: string
  // 有价性三分桶（2026-09-23）：成本报表的可信度 —— 无价调用的成本记 0 是「不知道花了
  // 多少」而非免费；旧行（字段未落盘）单列 unknown，不进无价分母。
  PricedCalls?: number
  UnpricedCalls?: number
  UnknownPricingCalls?: number
}
export interface MetricsReport {
  Total: MetricsTotals
  Stats: MetricsStats
  ByDay: MetricsDayGroup[]
  ByDayModel: MetricsDayModelGroup[]
  ByModel: MetricsGroup[]
  ByWorkspace: MetricsGroup[]
}

// 文件预览（file_preview 命令响应：路径 + 内容 + 行数 + 截断标记）
export interface FilePreview {
  path: string
  content: string
  lines: number
  truncated?: boolean
  // binary：bridge 判定该文件是二进制（xlsx/docx/pdf/图片等），content 为空串。
  // 前端据此显示「不适用文本预览」提示，而不是把二进制喂给 shiki（乱码 + 白耗渲染）。
  // 适用入口：文件树/工具行的 open-file（产出卡另有 xlsx/pdf 专门分支）。
  binary?: boolean
}

export interface FileFocus {
  path: string
  toolId?: string
  seq: number
  /** read_file 的 start_line：右侧「最终文件」视图渲染后定位滚动到该行（1-based） */
  startLine?: number
}

// 技能（SkillsPage / /skill 面板数据源）：bridge skills 命令返回全部技能（含启用标志，真过滤）
export interface SkillInfo {
  name: string
  description: string
  enabled: boolean
  source?: string // 远程安装的技能：来源 URL（SkillsPage 区分远端 + 更新/卸载）
}
export interface SkillDetail {
  name: string
  instructions: string
  resources: string[]
}

// 上下文构成分区（bridge context_breakdown 命令）。
// 分区键与 SDK 常量一一对应：system_prompt / skills / tools / mcp / messages / other。
// 口径（2026-09-20 重定）：
//   · 静态分区（系统提示词/技能/工具/MCP/其他）= 请求组装字节估算，**固定不缩放**；
//   · 消息上下文 = 锚点 − 静态合计（残差）——锚点即 ctx 环那个数（真实 usage 或压后估算）。
export interface CtxBreakdownPart {
  key: string
  tokens: number
}
export interface CtxBreakdown {
  parts: CtxBreakdownPart[]
  total: number // 面板总量（= ctx 环显示占用；无锚点时退化为估算合计）
  estimated: boolean // true = 总量不是 provider 真实 usage（无锚点或锚点是压后估算）
  anchorSource: 'real' | 'estimate' | 'none' // 锚点口径（决定面板底部说明文案）
  degraded: boolean // true = 锚点 < 静态前缀估算（锚点过期：刚压缩/切模型），消息分区钳到 0
  messageCount: number
  sessionId: string // 归属会话（渲染前校验）
  at: number // 拉取时刻（新鲜度判断）
}

// 市场浏览结果（skill_browse 响应：只读元数据 + 已装标记 + 更新标记；SkillsPage 市场 Tab 数据源）
export interface MarketSkill {
  name: string
  description: string
  installed: boolean
  update_available?: boolean // 已安装且同源仓库 HEAD 落后于本地记录 → 可更新（刷新市场列表时对比本地）
}

// 已保存的市场（market_list 响应；SkillsPage 市场管理：免手填 + 按市场更新技能）
export interface MarketInfo {
  name: string
  url: string
  added_at: string
}

// MCP 服务器（SettingsModal MCP tab）：bridge mcp_list 返回（连接状态/工具数/配置来源）
export interface MCPServerInfo {
  name: string
  type: string
  command: string
  enabled: boolean
  state: string // idle | connected | error
  tool_count: number
  error?: string
  // 配置分层来源：user（~/.go-code/settings.json，本面板可编辑）| project（项目文件，只读）
  source?: 'user' | 'project'
}

// MCP 配置层状态（bridge mcp_list.layers）：三层文件各自的存在性/有效性/贡献。
// stale = 文件当前坏（半写/坏 JSON）→ 沿用上次有效内容（损坏恢复语义，见 docs/MCP_CONFIG_LAYERS.md）。
export interface MCPLayerInfo {
  path: string
  source: 'user' | 'project' | string
  present: boolean
  ok: boolean
  stale: boolean
  error?: string
  servers: number
  skipped?: string[]
  updated_at?: string
}

// —— MCP / 钩子 设置面板（全局 × 项目两级；契约见 docs/MCP_HOOKS_SETTINGS_PANEL_2026-09-21.md）——

// 某项目的一个 MCP 配置层文件状态（bridge mcp_projects.layers；仅项目两层，低 → 高）。
export interface MCPProjectLayer {
  path: string
  present: boolean // 文件存在且读到了内容
  ok: boolean // 内容有效（能解析）
  stale?: boolean // 沿用上次有效内容（有运行态时才可能）
  error?: string
  servers: number // 该层贡献的服务器数
  skipped?: string[] // 校验失败被跳过的条目名
}

// 某项目的一条 MCP 服务器（项目层合并结果；file = 定义所在文件）。
export interface MCPProjectServer {
  name: string
  type: string
  command?: string
  args?: string[]
  url?: string
  env?: Record<string, string>
  enabled: boolean
  file: string // 定义文件：.mcp.json（只读）| .go-code/settings.json（可编辑）
  editable: boolean
  state?: string // 连接状态（仅该项目有运行态时出现）
  tool_count?: number
  error?: string
}

// 一个项目（已绑定的工作区）的项目级 MCP 配置（bridge mcp_projects 返回）。
// exists=false（目录被删）后端也返回，前端直接过滤 —— 已删除的项目不展示。
export interface MCPProjectInfo {
  path: string
  name: string // 目录名（卡片标题）
  exists: boolean
  running: boolean // 是否有运行态（打开中 → 才有连接状态/工具数）
  layers: MCPProjectLayer[]
  servers: MCPProjectServer[]
}

// 一条 hook handler（hooks 包 Handler 同形，面板可编辑形态）。
export interface HookHandlerView {
  type?: string
  name?: string
  command?: string
  timeout?: number // 秒；>=1000 视为毫秒（Qwen 兼容规则）；空 = 事件默认
  async?: boolean
  statusMessage?: string
  failureMode?: string // fail_open（默认）| fail_closed
  additionalContextLimit?: number // 注入字节上限
  enabled?: boolean // 缺省 = 启用
}

// 一个 matcher 分组（hooks 包 Group 同形）。
export interface HookGroupView {
  matcher?: string
  hooks: HookHandlerView[]
}

// 一个 hooks 配置文件的 hooks 段（hooks 包 section 同形；保存时整段回写）。
export interface HookSectionView {
  disableAllHooks?: boolean
  disabled?: string[]
  readClaudeSettings?: boolean
  allowProjectHooks?: boolean
  requireTrust?: boolean
  events?: Record<string, HookGroupView[]>
}

// 一个 hooks 配置来源（用户配置 / Claude 兼容层 / 某项目的 hooks.json）。
export interface HookSourceView {
  id: string
  kind: 'user' | 'claude-user' | 'project'
  title: string
  path: string
  workspace?: string
  exists?: boolean
  present: boolean // 文件存在且读到了内容
  ok: boolean // 内容有效（能解析）
  error?: string
  editable: boolean
  unknownKeys?: string[] // 顶层未识别键（保存后不保留 → UI 提示）
  body?: HookSectionView
}

// 某项目的 Claude Code 兼容层文件（只读；存在时才列出）。
export interface HookClaudeFile {
  path: string
  present: boolean
  ok: boolean
  error?: string
  body?: HookSectionView
}

// 某项目的 hooks 来源（bridge hooks_list.projects 一项）。
export interface HookProjectView extends HookSourceView {
  claude?: HookClaudeFile[]
}

// hooks_list 响应（SettingsModal 钩子页数据源）。
// 「某条 hook 是否真的会执行」由前端按 effective 的全局开关 + trust + enabled 计算（规则见契约 §4）。
export interface HooksInfo {
  user: HookSourceView
  claudeUser?: HookSourceView | null
  projects: HookProjectView[]
  effective: {
    disableAllHooks: boolean
    allowProjectHooks: boolean
    requireTrust: boolean
    readClaudeSettings: boolean
    sources: string[]
  }
  trust: Record<string, 'managed' | 'trusted' | 'untrusted' | 'modified'> // 命令 → 信任状态
  warnings: string[]
  at?: number // 前端回填的取回时刻（保存/刷新后递增 → 详情面板据此把本地副本重同步为磁盘态）
}

// 插件（SettingsModal 插件 tab）：bridge plugin_list 返回（能力包状态/版本）
export interface PluginInfo {
  id: string
  name: string
  description: string
  status: string // not-installed | installing | ready | enabled | error | update-available
  reason?: string
  installed?: string
  pinned?: string
  configurable?: boolean // 插件有用户可调配置（plugin_list；UI 据此显示「设置」入口）
  enabled?: boolean // 运行时是否已启用（IsEnabled；State 的 update-available 会掩盖 enabled）
  error?: string // 最近一次异步操作（安装/升级）的错误（plugin_status 事件透传；成功后清除）
  mcp?: { state: string; phase: string; tool_count?: number; error?: string; error_code?: string } // P0：私有 MCP 连接阶段
  computer?: { // Computer Use 详情（plugin_list 附带；ComputerSettingsEditor 数据源）
    enabled: boolean
    toolCount: number
    helperPath: string
    accessibility: boolean // 辅助功能（AX）已授权
    screenCapture: boolean // 屏幕录制已授权
    trustedApp?: string // 授权归属主体描述
    permError?: string
  }
}

// 浏览器插件配置（browser_config_get/set；持久化到插件 config.json，默认 = 隐私友好）
export interface BrowserCfg {
  session_mode: 'persistent' | 'isolated' | 'existing' // 登录策略
  presentation: 'window' | 'embedded' // 渲染位置：window=独立窗口；embedded=右侧 web tab 内嵌（默认）
  headless: boolean // 有头可见 = false（真实 Chrome 窗口）
  viewport: string // 视口，如 1280x720
  channel: '' | 'stable' | 'canary' | 'dev' | 'beta' // Chrome 渠道（空 = 系统默认）
  redact_network_headers: boolean // 敏感网络头脱敏（默认 true）
  usage_statistics: boolean // 发送使用统计（默认 false = 不发）
  url_mode: 'none' | 'block' | 'allow' // URL 限制（block/allow 互斥）
  url_patterns: string[] // URLPattern 语法，每行一条
  accept_insecure_certs: boolean // 忽略证书错误（慎用）
  user_data_dir?: string // 自定义 profile 目录（空 = 受管默认）
}

// 浏览器页面（web tab）：bridge browser_pages 返回（structuredContent.pages）
export interface BrowserPage {
  id: number
  url: string
  title: string
  selected?: boolean
}

// 内容块（对齐服务端 core.Content：text / image，图文可混排；可携带文件/图片附件）。
export interface QueuedContent {
  type: 'text' | 'image'
  content: string // 文本，或图片的 URL / data URL
  mimeType?: string // image/jpeg, image/png
}
// 客户端暂存队列项（Q1 体验优化）：用户在 Agent 运行期间连发的消息先入本地暂存队列
// （不立即推送到后端），LLM End（或空闲发送）时整队一次性 ask_batch 推送；每条消息 =
// 一组内容块（普通文本 / 文本+图片），结构对齐服务端 Message.Content。
// pushed=true 表示已推送、等待服务端确认消费（UserInputsConsumed 事件把它移入对话页）。
// 推送前每条可独立编辑（updatePending），确认后生效；上限 QUEUE_CAP。
export interface QueuedInput {
  id: number
  contents: QueuedContent[]
  pushed?: boolean // 已推送后端、等待确认消费（推送后不可再编辑）
}

// 暂存队列项展示/编辑用的文本（拼接所有 text 块；图片块以占位符标记，编辑框不展示二进制）。
export function queuedText(q: QueuedInput): string {
  return q.contents.map((c) => (c.type === 'text' ? c.content : `[${c.type}]`)).join('\n').trim()
}
// 暂存队列项是否带附件（非 text 块，如图片）—— 编辑提示 / 展示标记用。
export function queuedHasAttach(q: QueuedInput): boolean {
  return q.contents.some((c) => c.type !== 'text')
}

// messageImageBytes 一条消息的图片总量（推送前体积预检用；口径同 bridge
// maxImageBlockBytes —— 只算 image 块，文本由命令单行上限兜底）。
export function messageImageBytes(contents: QueuedContent[] | undefined): number {
  return (contents ?? []).reduce((n, c) => (c.type === 'image' ? n + dataUrlBytes(c.content) : n), 0)
}

export type AsyncTaskStatus = 'running' | 'interrupting' | 'completed' | 'interrupted' | 'failed' | 'abandoned'
export type TerminalTaskStatus = Exclude<AsyncTaskStatus, 'running' | 'interrupting'>

// @引用展开结果（refs_loaded 事件）：对话页用户消息下方状态行展示
export type RefLoadStatus = 'loaded' | 'missing' | 'blocked'
export interface RefLoadInfo {
  path: string // 用户输入原文（含 @）
  status: RefLoadStatus
  resolved?: string // 解析后的绝对路径（loaded）
  kind?: 'file' | 'dir' // 目标类型（loaded）
}

export interface CompressionInfo {
  kind: 'compression'
  id: number
  before: number
  after: number
  ctxTokens?: number
  reason?: string
  model?: string
  summary?: string
  analysis?: string
  usage?: UsageAgg
  error?: string
  aborted?: boolean
  durMs?: number
  active?: boolean // 自动压缩正在进行中时只显示在右侧任务区
  startedAt?: number
  runId?: string
}

export type MsgBlock =
  | { kind: 'user'; id: number; text: string; ts?: number; contents?: QueuedContent[]; refs?: RefLoadInfo[] } // contents = 附件（图片等；对话页渲染缩略图）；refs = @引用展开结果（refs_loaded 事件，对话页状态行）
  | { kind: 'assistant'; id: number; text: string; streaming: boolean; model?: string; runId?: string; thinking: string; thinkMs?: number; ttftMs?: number; durMs?: number; agentDurMs?: number; thinkStart?: number; outputTokens?: number; reasoningTokens?: number; thinkingSummarized?: boolean; genMs?: number; ts?: number }
  | { kind: 'tool'; id: number; toolId: string; name: string; args?: string; result?: string; diff?: Diff; images?: QueuedContent[]; isError?: boolean; errorInfo?: ToolErrorInfo; durMs?: number; status: 'running' | 'done'; taskId?: string; promoted?: boolean; interrupting?: boolean; startedAt?: number; taskStatus?: AsyncTaskStatus; todos?: TodoItem[]; usage?: UsageAgg }
  | { kind: 'agent'; id: number; runId: string; label: string; status: AsyncTaskStatus; spawnedAt: number; taskId?: string; toolName?: string; durMs?: number; thinkMs?: number; usage: UsageAgg; items: AgentItem[]; taskResult?: string; taskError?: string; deliveredToMain?: boolean; retrying?: boolean; retryCount?: number; retryMax?: number; retryMessage?: string; compressing?: boolean; compressStartTs?: number; artifacts?: ArtifactItem[]; artifactsTruncated?: boolean }
  // 后台任务结果会优先挂到对应任务卡片；无法匹配旧事件/占位任务时才保留独立结果块。
  | { kind: 'async_task'; id: number; taskId: string; label: string; toolName?: string; status: AsyncTaskStatus; startedAt: number; endedAt?: number; taskResult?: string; taskError?: string; deliveredToMain?: boolean; usage?: UsageAgg }
  | { kind: 'task_result'; id: number; taskId: string; result: string; error?: string }
  | { kind: 'task_delivery'; id: number; taskId: string; status: TerminalTaskStatus; result?: string; error?: string }
  | CompressionInfo
  | { kind: 'system'; id: number; text: string; tone?: 'info' | 'success' | 'error' }
  // 运行失败反馈（llm_error 重试 / session_run_error 最终失败）：叙述通道醒目错误块。
  // 重试进度字段（2026-09-18）：ts = 本次失败时刻，下一次尝试起点 = ts + retryDelayMs；
  // RetryProgress 组件据此渲染「退避倒计时 → 本次尝试已进行 Xs」（挂死时可见「在跑」）；
  // retryStartedAt = 本轮重试的首个失败时刻（跨多次失败不重置）→ 累计「已等待时长」。
  // retryCostUsd = 本轮重试**已花费**金额（USD，2026-09-23）：失败尝试的已产生用量
  //（上游按已生成 token 计费）折算，跨多次重试累加 —— 重试块显示「+$x」，会话成本含它。
  | { kind: 'error'; id: number; text: string; ts?: number; retrying?: boolean; retryAttempt?: number; retryMax?: number; retryDelayMs?: number; retryStartedAt?: number; retryCostUsd?: number }
  // 上下文隔离分隔线：进程关闭/重启（shutdown）、用户中断（abort）或清空上下文（clear）
  // 时插入的横线。UI 统一渲染为分隔线，内部按 reason 区分语义。
  | { kind: 'divider'; id: number; reason?: 'shutdown' | 'abort' | 'clear'; ts?: number }
  // 本轮产出汇总（AgentEnd.Artifacts）：对话页尾部「本次产出」卡片。
  // 数据来自运行结束时的文件汇总（write_file/edit_file 落盘），不含文件内容。
  | { kind: 'artifacts'; id: number; runId?: string; items: ArtifactItem[]; truncated?: boolean }

// 单个产出文件（对齐服务端 core.Artifact）。
export interface ArtifactItem {
  name: string
  path: string
  kind: 'created' | 'modified'
  added?: number
  removed?: number
  size?: number
  lines?: number
  sha256?: string
}

export interface Approval {
  id: string
  name: string
  arguments: string
  index: number
  runId?: string // 审批所属运行；队列仍按 Session 统一 FIFO，不按 SubAgent 分组
  diff?: Diff // 写/改文件审批：桥接层按 arguments 预计算的文件变更（审批时展示）
  // 决策倒计时（2026-09-20）：等待窗口（毫秒，桥接层注入 timeout_ms，与后端审批等待窗口同源）。
  // 缺省 = 未知窗口（老事件/其他宿主）→ 倒计时回退 APPROVAL_TIMEOUT，且不作为唯一裁决入口。
  timeoutMs?: number
  // 审批请求到达时刻（本地）——倒计时起点，避免面板重挂（切会话再回来）时倒计时重头开始。
  arrivedAt?: number
}

export interface RunNode {
  id: string
  parentId?: string
  label: string
  status: 'running' | 'done' | 'error' | 'interrupted'
  depth: number
  kind: 'run' | 'tool'
  name?: string // 工具名（kind='tool'）
  args?: string
  result?: string
  isError?: boolean
  startedAt?: number // span 开始时刻（agent_start / tool_run_start 记录）
  durMs?: number // span 时长（run：agent_end 结算；tool：tool_run_end 结算）
  taskId?: string // 工具任务 id（tool_run_start 携带；task_promoted 据此标记后台态）
  promoted?: boolean // 已被摘离为后台任务
}

export interface SessionMeta {
  id: string
  title: string
  updatedAt?: number // 最近活跃时刻（unix ms；bridge 返回 updated_at，用于「最新在最上」排序）
  pinned?: boolean // 会话置顶（bridge 从 ~/.go-code/sessions/<wsKey>/sessions.json 读）
}

// 项目管理：项目 = 一个工作区目录，内含多个会话；pinned 只表示左栏工作区置顶状态。
export const MAX_WORKSPACE_PINS = 5
// 每工作区会话置顶上限（与 bridge session_pins.go maxSessionPins 一致）。
export const MAX_SESSION_PINS = 5
export interface Project {
  path: string
  sessions: SessionMeta[]
  pinned?: boolean
}

export interface ToolErrorInfo {
  code?: string
  changed?: 'true' | 'false' | 'unknown'
  retryable?: boolean
  nextAction?: string
  recovery?: string
}

export interface Diff {
  path: string
  added: number
  removed: number
  unified?: string // ToolResponse.Diff.Unified（事件产出，非 git）
  truncated?: boolean // 引擎侧已截断 unified（core.FileDiff.Truncated；diff.go 超 8 KB）
}

// 会话待办（SDK todo.Item：id/title/done/blocked_by；宿主悬浮面板渲染）
export interface TodoItem {
  id: string
  title: string
  done: boolean
  blockedBy?: string[]
}

// 模型→用户提问批次（ask_user 工具事件；UI 一次性展示全部问题，提交后 send answer_question）
export interface PendingQuestionItem {
  id: string // 单题锚（answers[].question_id 匹配）
  question: string
  // 快捷选项（可带预览，见 lib/questionOptions）；事件里的纯字符串形态在派发时归一化
  options: QuestionOption[]
  multi?: boolean // true = 可多选（选项可同时勾选多个）
}
export interface PendingQuestion {
  id: string // 批次 id（answer_question 命令的 batch_id 锚）
  runId?: string
  items: PendingQuestionItem[]
  // 决策倒计时（2026-09-20）：timeoutMs = 等待窗口（后端 QuestionWaiter.Timeout() 同源，
  // 事件 timeout_ms；0/缺省 = 无时限 → 不显示倒计时）；ts = 提问时刻（事件时间戳，
  // 缺省回退接收时刻）——面板据此算「剩余决策时间」，不用客户端挂额外定时器。
  timeoutMs?: number
  ts?: number
}

// 沙箱拦截放行确认（sandbox_blocked 事件 → sandbox_answer 命令；随 bash 工具结束清除）
export interface PendingRelease {
  id: string // 会话 id（锚；放行每次只有一个未决确认，bash 串行不并发）
  hint: string // 被拦截命令摘要（≤120 字符）
}

// 子 agent 活动记录（与主 agent 同构：思考/输出/工具调用按时间序，SDK 事件全量转发）
export type AgentItem =
  | { kind: 'text'; text: string; error?: boolean } // error=true = LLM 失败/上下文超限提示（卡片内醒目显示）
  | { kind: 'thinking'; text: string; thinkStart?: number }
  | { kind: 'tool'; toolId: string; name: string; args?: string; status: 'running' | 'done' | 'error'; result?: string; errorInfo?: ToolErrorInfo; durMs?: number; startedAt?: number; taskId?: string; diff?: Diff; images?: QueuedContent[]; usage?: UsageAgg }
  | { kind: 'compression'; before: number; after: number; ctxTokens?: number; reason?: string; model?: string; summary?: string; analysis?: string; usage?: UsageAgg; error?: string; aborted?: boolean; durMs?: number; active?: boolean; startedAt?: number }

// PriceRefreshSummary refresh_prices 结果摘要（UI toast 用；字段对齐 bridge 响应）
export interface PriceRefreshSummary {
  source: string // 价源标识，如 "models.dev:opencode-go" / "openrouter"
  filled: string[] // 本次补上的模型
  filledCount: number
  skippedCount: number // 价源无此模型 / 已有价未覆盖
  total: number // 参与补价的模型数（provider 已配置模型）
}

export interface UsageAgg {
  input: number
  output: number
  cacheRead: number
  cacheWrite: number // 写入缓存量（Anthropic cache_creation；其余协议恒 0）
  // cacheWrite1h ⊆ cacheWrite：1h extended TTL 写入量（Anthropic 专用；其余端点 0）。
  // 状态栏缓存 tooltip 据此区分 5m/1h 缓存规模（2026-09-23 补齐维度）。
  cacheWrite1h: number
  reasoning: number
  // costUsd 成本（USD）：仅**任务/子 agent 卡片**填充（TaskEnd / task_result_delivered 的
  // usage.Cost.Total µUSD 折算，或运行中按每轮 llm_end 的 cost_usd 累加）。
  // 会话级累计成本另由 SessionView.costUsd 承载——两者独立记账，不叠加。
  costUsd?: number
}

// SDK 事件 usage 字段（core.Usage json：大写字段名）
interface RawUsage {
  Input?: number
  Output?: number
  CacheRead?: number
  CacheWrite?: number
  // CacheWrite1h ⊆ CacheWrite：1h extended TTL 写入量（Anthropic 专用）
  CacheWrite1h?: number
  ReasoningSummarized?: boolean
  Reasoning?: number
  // Cost 由 provider 按价表填充（µUSD）：任务/子 agent 卡片据此展示成本
  Cost?: { Input?: number; Output?: number; CacheRead?: number; CacheWrite?: number; Total?: number }
}

// 会话级视图状态（views 真相源；顶层字段是活跃会话的镜像，组件读顶层零改动）
export interface SessionView {
  blocks: MsgBlock[]
  approvals: Approval[]
  runNodes: RunNode[]
  diffs: Diff[]
  desktopTool?: string // 当前正在执行的 computer_* 工具名（tool_run_start 置 / end 清；状态栏「正在操作桌面」指示）
  compressing: boolean
  compressStartTs?: number
  compressBefore?: number // 主 Agent 当前压缩的开始消息数（右侧活动镜像）
  compressRunId?: string // 主 Agent 当前压缩所属 run（旧事件缺失时为空）
  compressReason?: string
  todos: TodoItem[]
  usage: UsageAgg
  ctxTokens?: number // 当前主 Agent 上下文占用
  // ctxTokens 的口径：true = 值来自 compress_end 的字符/token 估算（压缩后尚无新一轮真实
  // usage），UI 以 ≈ 标注；llm_end（provider 真实 usage）置回 false。见 bridge CtxAnchor。
  ctxTokensEstimated?: boolean
  contextWindow?: number // harness 上报的上下文窗口（agent_start.context_window；占比分母单一事实源，问题六）
  pendingQueueError?: string // 暂存队列推送失败原因（ask_batch ok:false → 回滚 pushed 并提示，问题十）
  git: GitStatus
  gitFiles: GitFile[]
  gitHistory: GitCommit[]
  gitLoading: boolean
  gitError?: string
  gitHasMore?: boolean // 会话级历史分页：是否还有更多
  gitHistLoading?: boolean // 会话级历史加载中（「加载更多」防连点）
  turns: TurnMetric[] // 每轮指标（llm_end 一条；Composer 命中率/状态栏数据源）
  costUsd: number     // 会话累计成本（桥按价表打标累加）
  ttftMs?: number
  llmStartTs?: number
  turnStartTs?: number
  toolStartTs: Record<string, number>
  streaming: boolean
  aborted: boolean // 本次 run 已被用户中断（agent_end abort 置位；agent_start 复位）——chunk 防御
  reqBlock: Record<string, number> // 流式块归属（per-session，防跨会话 request_id 串）
  lastLLMBlockId?: number
  runningAgentCount: number // 本会话运行中的子 agent 数（agent_start/end 维护；右栏角标与 Agent tab 计数）
  thinkStartTs: Record<string, number> // 思考开始时刻（key = run_id，'' = 主 agent；首个 reasoning_chunk 记）
  turnThinkMs: Record<string, number> // 已结算的思考耗时（key = run_id；content_chunk 记，llm_end 并入每轮指标）
  question?: PendingQuestion // 模型→用户提问（ask_user；一次性，回答/工具结束清除）
  release?: PendingRelease // 沙箱拦截放行确认（sandbox_blocked；随 bash 工具结束清除）
  pendingQueue: QueuedInput[] // 客户端暂存队列（未推送 / 已推送待确认；逐条可编辑，LLM End 时整队推送）
  rightExpanded: boolean // 右侧栏是否展开（会话级，切换会话各自恢复）
  rightTab: RightTab // 右侧栏当前 tab（会话级）

  // —— 链路 tab：trace（docs/TRACE_CHAIN_REDESIGN.md）——
  // traceEvents：本会话收到的 span 类事件（白名单 TRACE_SPAN_TYPES），实时投影的数据源。
  //   上限 traceEventsMax 条，超出从头丢弃（长会话防内存膨胀；只影响「当前 trace」的完整度，
  //   历史 trace 仍可从 bridge 按需拉取）。
  traceEvents: AnyEvent[]
  // traceList：历史 trace 摘要（bridge `trace` 命令回；顶部选择器数据源）
  traceList?: TraceSummary[]
  traceListTotal?: number
  traceListLoading?: boolean
  traceHasMore?: boolean
  // activeTraceId：当前查看的 trace（undefined = 跟随最新/实时）
  activeTraceId?: string
  // traceDetail：从 bridge 拉取的历史 trace 详情（activeTraceId 非实时 trace 时使用）
  traceDetail?: TraceDetail
  // traceFallback：**刷新/切回会话后**的兜底详情 —— traceEvents 只在内存里累积，
  // checkpoint 恢复不含它，刷新后实时投影为空（会误显示「尚无运行」）。
  // 此时用命令拉「最新一条 trace」的详情兜底，保证刷新后链路仍可回显。
  // 优先级低于实时投影：一旦收到新的 agent_start，实时投影即接管。
  traceFallback?: TraceDetail

  // checkpoint hydrate 元数据（restored="checkpoint" 时回填；供后续 revision/generation 校验）。
  // 仅存于 view，不进顶层镜像（当前 UI 不直接读）。
  checkpointRevision?: number
  checkpointGeneration?: number
  degraded?: boolean
}

// 会话是否在运行：存在进行中的 run 节点（agent_start→agent_end 全程 status=running，
// 涵盖工具调用/提问/审批/子 agent 整段）。左栏据此点亮非活跃会话的运行点。
export function sessionBusy(v: SessionView | undefined): boolean {
  return !!v && v.runNodes.some((n) => n.kind === 'run' && n.status === 'running')
}

// 主 Agent（顶层 run，无 parent_run_id）是否运行中。
// agent_start→agent_end 全程 true（流式/工具/压缩/审批/重试整段），agent_end 收敛为 false；
// 崩溃恢复时 finalizeReplayView/NormalizeForRestore 把残留 running 收敛为 done，不会卡死。
// 与 sessionBusy 的区别：sessionBusy 把子 agent 的 run 节点也算作「运行中」，
// 而主会话停止按钮/运行指示只应反映主 run（子 agent 停止走 interrupt_task，独立于主会话）。
// 注意：不含 aborted 判定——中断在途（点击停止后、agent_end 到达前）主 run 仍 running，
// 由调用方自行结合 view.aborted 过滤（interrupt() 乐观置位后按钮立即复位）。
export function mainRunActive(v: SessionView | undefined): boolean {
  return !!v && v.runNodes.some((n) => n.kind === 'run' && n.status === 'running' && !n.parentId)
}

function emptyView(): SessionView {
  return {
    blocks: [],
    approvals: [],
    runNodes: [],
    diffs: [],
    compressing: false,
    compressStartTs: undefined,
    compressBefore: undefined,
    compressRunId: undefined,
    compressReason: undefined,
    todos: [],
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 },
    ctxTokens: undefined,
    ctxTokensEstimated: undefined,
    contextWindow: undefined,
    git: { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true },
    gitFiles: [],
    gitHistory: [],
    gitHasMore: false,
    gitHistLoading: false,
    gitLoading: false,
    gitError: undefined,
    turns: [],
    costUsd: 0,
    toolStartTs: {},
    streaming: false,
    aborted: false,
    reqBlock: {},
    lastLLMBlockId: undefined,
    runningAgentCount: 0,
    thinkStartTs: {},
    turnThinkMs: {},
    question: undefined,
    release: undefined,
    pendingQueue: [],
    rightExpanded: false,
    rightTab: 'activity',
    traceEvents: [],
  }
}

// view 的顶层镜像字段（组件读顶层）
function viewFields(nv: SessionView): Partial<AppState> {
  return {
    blocks: nv.blocks,
    approvals: nv.approvals,
    runNodes: nv.runNodes,
    diffs: nv.diffs,
    todos: nv.todos,
    usage: nv.usage,
    ctxTokens: nv.ctxTokens,
    ctxTokensEstimated: nv.ctxTokensEstimated,
    contextWindow: nv.contextWindow,
    pendingQueueError: nv.pendingQueueError,
    git: nv.git,
    gitFiles: nv.gitFiles,
    gitHistory: nv.gitHistory,
    gitHasMore: nv.gitHasMore,
    gitHistLoading: nv.gitHistLoading,
    gitLoading: nv.gitLoading,
    gitError: nv.gitError,
    turns: nv.turns,
    costUsd: nv.costUsd,
    ttftMs: nv.ttftMs,
    llmStartTs: nv.llmStartTs,
    turnStartTs: nv.turnStartTs,
    toolStartTs: nv.toolStartTs,
    streaming: nv.streaming,
    compressing: nv.compressing,
    compressStartTs: nv.compressStartTs,
    compressBefore: nv.compressBefore,
    compressRunId: nv.compressRunId,
    compressReason: nv.compressReason,
    aborted: nv.aborted,
    thinkStartTs: nv.thinkStartTs,
    turnThinkMs: nv.turnThinkMs,
    lastLLMBlockId: nv.lastLLMBlockId,
    runningAgentCount: nv.runningAgentCount,
    question: nv.question,
    release: nv.release,
    pendingQueue: nv.pendingQueue,
    rightExpanded: nv.rightExpanded,
    rightTab: nv.rightTab,
    traceEvents: nv.traceEvents,
    traceList: nv.traceList,
    traceListTotal: nv.traceListTotal,
    traceListLoading: nv.traceListLoading,
    traceHasMore: nv.traceHasMore,
    activeTraceId: nv.activeTraceId,
    traceDetail: nv.traceDetail,
    traceFallback: nv.traceFallback,
  }
}

// 更新 views[sid]；若该会话是活跃会话（或 active=true 强制），同步顶层镜像
function setView(s: AppState, sid: string, nv: SessionView, extra?: Partial<AppState>, active?: boolean): Partial<AppState> {
  const isNew = !s.views[sid]
  touchView(sid)
  const views = { ...s.views, [sid]: nv }
  const isActive = active ?? sid === s.activeSessionId
  // 后台事件可能首次创建 view；提交后立即执行 LRU 收敛，保证缓存不会随 session 数增长。
  if (isNew) queueMicrotask(() => evictViews())
  if (isActive) return { views, ...viewFields(nv), ...extra }
  return { views, ...extra }
}

function topViewFields(s: AppState): Partial<SessionView> {
  return {
    blocks: s.blocks,
    approvals: s.approvals,
    runNodes: s.runNodes,
    diffs: s.diffs,
    todos: s.todos,
    usage: s.usage,
    turns: s.turns,
    costUsd: s.costUsd,
    ttftMs: s.ttftMs,
    llmStartTs: s.llmStartTs,
    turnStartTs: s.turnStartTs,
    toolStartTs: s.toolStartTs,
    streaming: s.streaming,
    compressing: s.compressing,
    compressStartTs: s.compressStartTs,
    compressBefore: s.compressBefore,
    compressRunId: s.compressRunId,
    compressReason: s.compressReason,
    aborted: s.aborted,
    thinkStartTs: s.thinkStartTs,
    turnThinkMs: s.turnThinkMs,
    lastLLMBlockId: s.lastLLMBlockId,
    runningAgentCount: s.runningAgentCount,
    question: s.question,
    release: s.release,
    pendingQueue: s.pendingQueue,
    rightExpanded: s.rightExpanded,
    rightTab: s.rightTab,
    traceEvents: s.traceEvents,
    traceList: s.traceList,
    traceListTotal: s.traceListTotal,
    traceListLoading: s.traceListLoading,
    traceHasMore: s.traceHasMore,
    activeTraceId: s.activeTraceId,
    traceDetail: s.traceDetail,
    traceFallback: s.traceFallback,
  }
}

// 把缓存 view 镜像到顶层（切到已有缓存的会话时用，不发请求）
function loadViewToTop(s: AppState, sid: string): Partial<AppState> {
  const v = s.views[sid]
  if (!v) return viewFields(emptyView())
  return viewFields(v)
}

// 切项目/切会话时重置当前会话视图镜像（顶层字段）
function resetView(): Partial<AppState> {
  return viewFields(emptyView())
}

// —— 右侧栏标签模型 helper（阶段 1）——

// scopeKeyOf：标签集的归属键。无会话（加了工作区但还没建会话）时退化为工作区路径——
// 否则右栏在「已添加工作区、尚未建会话」这个窗口期没有归属（D7）。
function scopeKeyOf(s: { activeSessionId?: string; workspace?: string }): ScopeKey {
  return s.activeSessionId || s.workspace || ''
}

let tabSeq = 0
// focusSpec：fileFocus（用户/Agent 最后一次定位的文件）→ file 标签的初始 payload。
// mode 口径与 locateFile 一致：显式 source，或缺 toolId → 完整预览；否则 diff/操作模式。
//
// 阶段 2 起 fileFocus **不再是内容源**（内容按归一化路径索引，见 filePreviews），它只剩
// 两个用途：① 无路径打开 file 标签时的初值（本函数 —— 用户点 rail 的「文件」按钮 /
// setRightTab('file') / e2e 直接写 rightTab 后 sync）；② 左栏文件树的选中行。
function focusSpec(s: { fileFocus?: FileFocus }): TabSpec {
  const f = s.fileFocus
  if (!f?.path) return { kind: 'file' }
  return {
    kind: 'file',
    path: f.path,
    mode: f.toolId ? 'diff' : 'source',
    ...(f.toolId ? { toolId: f.toolId } : {}),
    ...(f.startLine != null ? { startLine: f.startLine } : {}),
  }
}

// tabKeyOf：标签的**身份键**（阶段 2）。
//   · file → path + mode：同一文件可同时有 diff 与 source 两个标签（D5），
//     但同一 (path, mode) 只有一个实例。toolId **不进键** —— 同一文件的 diff 标签只有一个，
//     再次打开（点另一处改动）只更新它的 toolId/startLine，否则用户会看到两个同名 diff 标签。
//   · 其它 kind → undefined（沿用阶段 1 的「每类每 scope 一个实例」，匹配由调用方按 kind 做）。
// 路径先归一化（fileKeyOf）：工作区内的绝对路径与相对路径指向同一个文件，必须落同一个标签，
// 否则「从对话点开」与「从左栏文件树点开」会各开一个（用户看到的就是重复）。
function tabKeyOf(t: TabInstance | TabSpec, ws: string | undefined): string | undefined {
  if (t.kind !== 'file' || !t.path) return undefined
  return `file\u0000${fileKeyOf(t.path, ws)}\u0000${t.mode ?? 'source'}`
}

// activePageIdOf：原生视图池里「当前激活的**真实页面**」的 id。
//
// 判据必须与 onEmbedNav 喂给 syncWebTabs 的 items **逐字同口径**（有 url 且非 about:blank）：
// items 里没有的 key，syncWebTabs 一律当「这个页面已经关了」处理。而池里**常驻**一个 about:blank
// 自举载体（主进程建连即存在，且它就是 activeId）—— 标签一旦认了这个 id，下一次 embed-nav 事件
// （紧跟着就来）就会把它判死删掉。用户看到的就是「点了没反应」：
//   ＋ → 网页 / 折叠态 rail 的「网页」图标 → 空 web 标签 → 4ms 后标签消失 + rightTab 被顶回
//   上一个标签（2026-09-21 用户报障「右侧侧边栏点击加号、选择网页无效」，真机 trace 实测）。
// 认不出真实页面时**不认 id**：交给「空 web 标签」形态 —— 它被 syncWebTabs 的收养分支接住，
// 池里出现第一个真实页面时复用它的标签实例（不闪、不多一个标签）。
function activePageIdOf(s: { embedNav?: EmbedNavState }): string | undefined {
  const nav = s.embedNav
  if (!nav?.activeId) return undefined
  const tp = nav.targets?.find((x) => x.id === nav.activeId)
  return tp?.url && tp.url !== 'about:blank' ? nav.activeId : undefined
}

// makeTab 造一个标签实例。file / web 的 payload 从**当前上下文**取初值：
//   file → fileFocus（见 focusSpec）
//   web  → 池里当前激活的**真实页面**（见 activePageIdOf；认不出就是空标签）
const newTabId = (): string => `tab${++tabSeq}`
function makeTab(kind: TabKind, s: AppState): TabInstance {
  const id = newTabId()
  if (kind === 'file') return { ...focusSpec(s), id, openSeq: 1 } as TabInstance
  if (kind === 'web') {
    const viewId = activePageIdOf(s)
    return { id, kind, ...(viewId ? { viewId } : {}) }
  }
  return { id, kind }
}

// —— 阶段 5：标签集持久化（docs/RIGHT_PANEL_TABS_PLAN.md §6 阶段 5 / §7 未决 4）——
//
// 通道 = localStorage（键 go-code.sessionTabs，与 go-code.projects / .titles / .prefs 同族）：
// 标签是**纯 UI 偏好**（有哪些标签、顺序、激活哪个、文件标签是 diff 还是 source），不参与
// AI 路由，不值得为它加一条 bridge 文件通道（那条路的先例是 session_pins.go，服务于「要跟
// 会话一起被 AI 读到」的数据）。
//
// 存的是**标签的地址**，不是内容：file 标签存 path/mode/toolId/startLine，web 标签存 url
// 快照。文件内容/页面本身都不进存储（内容最大 1 MiB，几十个标签就是几十 MB）。
//
// 恢复出来的 web 标签**不带池身份**（见 WebTabInstance 注释），落在「待重建」态：视图池
// 为空、主进程里没有任何 WebContentsView —— 直到用户点它才重建（「视图按需重建」，
// 阶段 3 刚用休眠解决过视图常驻的内存问题，启动时 eager 重建等于把它原样还回去）。
export const LS_SESSION_TABS = 'go-code.sessionTabs'
// 结构版本：不匹配 → 整体丢弃（与 loadJSON 的「坏数据静默忽略」同一口径，见 loadPersistedTabs）。
const TABS_PERSIST_VERSION = 1
// 每个 scope 持久化的标签数上限（防 localStorage 无界增长；超出丢弃最旧的）。
export const TABS_PERSIST_MAX = 20
// 持久化的 scope 数上限（最近会话优先保留）。标签集按会话隔离，会话数无上界 ——
// 只有每 scope 的上限挡不住「几百个会话各存一排标签」。
export const TABS_PERSIST_MAX_SCOPES = 40
// 单个 url 快照的长度上限：超过即丢弃该 web 标签。data: URL（```html 代码块预览）把**整段
// HTML** 编进 url，可达数百 KB —— 存进 localStorage 会顶到配额，而它是「当时那一段内容」的
// 快照，跨重启没有意义（内容源在对话里，用户想看会重新点一次）。
const TABS_PERSIST_URL_MAX = 2048
// 写入防抖：标签集变化常成串发生（syncWebTabs 在每个 embed-nav 事件上都会跑；打开一个文件
// 会同时动标签集与激活项）→ 攒一攒再整体序列化一次，而不是每个 action 都 stringify 整表。
const TABS_PERSIST_DEBOUNCE_MS = 300

// 持久化形态：标签实例**剥掉运行时字段**（viewId / pageId / openSeq），web 换成 url 快照。
type PersistedTab =
  | { id: string; kind: 'activity' | 'git' | 'agent' }
  | { id: string; kind: 'file'; path?: string; mode?: 'diff' | 'source'; toolId?: string; startLine?: number }
  | { id: string; kind: 'web'; url: string; srcPath?: string }
interface PersistedTabSet { tabs: PersistedTab[]; activeId: string | null }
interface PersistedTabs { v: number; scopes: Record<ScopeKey, PersistedTabSet> }

// isPendingWebTab：待重建的 web 标签（重启恢复出来的那种）。
// 判据 = 「有 url 快照 + 没有任何池身份」：池身份是运行时 id，跨进程不复用，故恢复时一律
// 不带；带上就会指向重启后新建的**另一个**页面（主进程 't'+自增序号从 t1 重来）。
export function isPendingWebTab(t: TabInstance): t is WebTabInstance {
  return t.kind === 'web' && !t.viewId && t.pageId == null && !!t.url
}

// webUrlSnapshot：一个 web 标签此刻能拿到的最可信的 url（+ 源文件路径）快照。
// 三级反查见 toPersistedTab 的注释；返回 undefined = 这个标签**不可恢复**。
function webUrlSnapshot(t: WebTabInstance, s: AppState): { url: string; srcPath?: string } | undefined {
  if (t.url) return { url: t.url, ...(t.srcPath ? { srcPath: t.srcPath } : {}) }
  if (t.viewId) {
    const tp = s.embedNav?.targets?.find((x) => x.id === t.viewId)
    // about:blank = 自举占位（还没导航到真实页面）→ 不是「用户打开的页面」，不恢复。
    if (tp?.url && tp.url !== 'about:blank') return { url: tp.url, ...(tp.srcPath ? { srcPath: tp.srcPath } : {}) }
    return undefined
  }
  if (t.pageId != null) {
    const pg = s.browserPages.find((p) => p.id === t.pageId)
    if (pg?.url) return { url: pg.url }
  }
  return undefined
}

// toPersistedTab：运行时标签 → 存储形态。返回 undefined = **不持久化这个标签**。
//
// web 标签的 url 从哪来（本阶段最容易做歪的一处）：TabInstance **没有** url 字段，而
// embedNav.targets 是主进程推来的**运行时状态**（重启后为空）→ 持久化时必须此刻反查并快照。
// 反查顺序：① 标签自带的快照（待重建的）→ ② 视图池 targets（按 viewId）→ ③ MCP 页列表
// （按 pageId）。反查不到就**丢弃**（池里已被 LRU 关掉/从未导航成功）——宁可不恢复，
// 也不要恢复一个点不开的死标签。
function toPersistedTab(t: TabInstance, s: AppState): PersistedTab | undefined {
  switch (t.kind) {
    case 'activity': case 'git': case 'agent':
      return { id: t.id, kind: t.kind }
    case 'file':
      // openSeq **不存**：它是「每次打开回默认视图」的触发器，恢复时从 1 起（见 fromPersistedTab）。
      return {
        id: t.id,
        kind: 'file',
        ...(t.path ? { path: t.path } : {}),
        ...(t.mode ? { mode: t.mode } : {}),
        ...(t.toolId ? { toolId: t.toolId } : {}),
        ...(t.startLine != null ? { startLine: t.startLine } : {}),
      }
    default: {
      const snap = webUrlSnapshot(t, s)
      if (!snap || snap.url.length > TABS_PERSIST_URL_MAX) return undefined
      return { id: t.id, kind: 'web', url: snap.url, ...(snap.srcPath ? { srcPath: snap.srcPath } : {}) }
    }
  }
}

// fromPersistedTab：存储形态 → 运行时标签（**新的 id**，见 loadPersistedTabs 的注释）。
// web → 待重建态（只带 url/srcPath 快照，不带池身份）；file 的 openSeq 从 1 起。
function fromPersistedTab(p: PersistedTab, id: string): TabInstance {
  switch (p.kind) {
    case 'activity': case 'git': case 'agent':
      return { id, kind: p.kind }
    case 'file':
      return { id, kind: 'file', ...(p.path ? { path: p.path } : {}), ...(p.mode ? { mode: p.mode } : {}), ...(p.toolId ? { toolId: p.toolId } : {}), ...(p.startLine != null ? { startLine: p.startLine } : {}), openSeq: 1 }
    default:
      return { id, kind: 'web', url: p.url, ...(p.srcPath ? { srcPath: p.srcPath } : {}) }
  }
}

const TAB_KINDS: TabKind[] = ['activity', 'git', 'agent', 'file', 'web']

// sanitizePersistedTab：逐字段校验一条存储记录。**坏数据不炸**（规格 §2 要求 4）——
// 结构不对/类型不对的字段一律丢弃（能救的字段救回来，救不了的整条丢），绝不抛错。
function sanitizePersistedTab(raw: unknown): PersistedTab | undefined {
  if (!raw || typeof raw !== 'object') return undefined
  const r = raw as Record<string, unknown>
  const kind = typeof r.kind === 'string' && (TAB_KINDS as string[]).includes(r.kind) ? r.kind as TabKind : undefined
  if (!kind) return undefined // 非法 kind（含旧版本枚举）→ 丢弃这一条
  const id = typeof r.id === 'string' && r.id ? r.id : newTabId()
  const str = (v: unknown): string | undefined => (typeof v === 'string' && v ? v : undefined)
  const num = (v: unknown): number | undefined => (typeof v === 'number' && Number.isFinite(v) ? v : undefined)
  if (kind === 'file') {
    // mode 非法 → 归一化成缺省（缺省 = source 完整预览，与 focusSpec/tabKeyOf 的口径一致），
    // 而不是丢掉整个标签：路径还在，用户点开还能看到内容。
    const mode = r.mode === 'diff' || r.mode === 'source' ? r.mode : undefined
    const path = str(r.path)
    const toolId = str(r.toolId)
    const startLine = num(r.startLine)
    return { id, kind, ...(path ? { path } : {}), ...(mode ? { mode } : {}), ...(toolId ? { toolId } : {}), ...(startLine != null ? { startLine } : {}) }
  }
  if (kind === 'web') {
    // 没有 url 就**无法重建** → 整条丢弃（规格 §3.3：不恢复点不开的死标签）。
    const url = str(r.url)
    if (!url || url.length > TABS_PERSIST_URL_MAX) return undefined
    const srcPath = str(r.srcPath)
    return { id, kind, url, ...(srcPath ? { srcPath } : {}) }
  }
  return { id, kind }
}

// sanitizePersistedTabSet：一个 scope 的记录。tabs 不是数组/为空数组的处理见下。
function sanitizePersistedTabSet(raw: unknown): PersistedTabSet | undefined {
  if (!raw || typeof raw !== 'object') return undefined
  const r = raw as Record<string, unknown>
  if (!Array.isArray(r.tabs)) return undefined
  const seen = new Set<string>()
  const tabs: PersistedTab[] = []
  for (const item of r.tabs) {
    const t = sanitizePersistedTab(item)
    if (!t) continue
    if (seen.has(t.id)) continue // 重复 id：标签身份必须唯一（后一条丢弃）
    seen.add(t.id)
    tabs.push(t)
  }
  // 空集是**合法**的（用户把标签关光了，K15/K16 的口径）→ 原样恢复成空态，不补默认标签。
  const activeId = typeof r.activeId === 'string' && seen.has(r.activeId) ? r.activeId : (tabs[0]?.id ?? null)
  return { tabs, activeId }
}

// loadPersistedTabs：模块加载时同步恢复（与 persistedBoot 同序）。为什么必须**同步**且
// 在首帧之前：面板的 syncTabsToRightTab 分支① 会给「还没有标签集」的 scope 建默认集 ——
// 恢复晚一步，默认集就把位置占了，恢复出来的标签集再也进不去。
function loadPersistedTabs(): Record<ScopeKey, TabSet> {
  const out: Record<ScopeKey, TabSet> = {}
  const raw = loadJSON<Partial<PersistedTabs>>(LS_SESSION_TABS) // 截断/非法 JSON → null（容错在 loadJSON）
  if (!raw || typeof raw !== 'object') return out
  if (raw.v !== TABS_PERSIST_VERSION) return out // 旧版本结构：整体丢弃，不做逐字段猜测
  const scopes = raw.scopes
  if (!scopes || typeof scopes !== 'object') return out
  for (const [scope, value] of Object.entries(scopes as Record<string, unknown>)) {
    if (!scope) continue
    const set0 = sanitizePersistedTabSet(value)
    if (!set0) continue
    // 重新发 id：存储里的 id 只是「同一份数据里的引用」（activeId 指向它），跨进程没有意义；
    // 直接沿用则必须把 tabSeq 顶到最大值之上，否则新开的标签会撞上恢复出来的 id。
    const idMap = new Map<string, string>()
    const tabs = set0.tabs.map((p) => { const id = newTabId(); idMap.set(p.id, id); return fromPersistedTab(p, id) })
    out[scope] = { tabs, activeId: set0.activeId ? (idMap.get(set0.activeId) ?? null) : null, closed: [], restored: true }
  }
  return out
}

// initTabsByScope：首帧就位（见 loadPersistedTabs）。
const initTabsByScope = loadPersistedTabs()

// persistTabsNow：把当前标签集整体写进 localStorage。
// 只写**地址**：见 toPersistedTab。closed 栈**不持久化**（它是「刚关掉」的短期记忆，
// 跨重启没有意义 —— 用户重启后想重开的是「上次在看的东西」，不是上次关掉的东西）。
function persistTabsNow(s: AppState): void {
  const keys = Object.keys(s.tabsByScope)
  // scope 数上限：最近用过的（recentSessions = 顶部快捷栏队列，同样是持久化的）优先保留，
  // 其余按插入序。超限的 scope 整体不写（下次用到它时又会重新进队）。
  const recentIds = s.recentSessions.map((r) => r.id)
  const ordered = [...recentIds.filter((k) => keys.includes(k)), ...keys.filter((k) => !recentIds.includes(k))]
  const scopes: Record<ScopeKey, PersistedTabSet> = {}
  for (const scope of ordered.slice(0, TABS_PERSIST_MAX_SCOPES)) {
    const set0 = s.tabsByScope[scope]
    if (!set0) continue
    const mapped = set0.tabs.map((t) => toPersistedTab(t, s)).filter((p): p is PersistedTab => !!p)
    // 每 scope 上限：丢最旧的（标签条顺序 = 打开顺序，队首最旧）。
    let kept = mapped.slice(-TABS_PERSIST_MAX)
    // 激活标签必须留在集合里 —— 否则重启后恢复出来的是「别的标签被激活」。极端情形
    //（>20 个标签且激活的是最旧那个）下挤掉最旧的一条，activeId 在集合里的位置退化为队首。
    if (set0.activeId && !kept.some((p) => p.id === set0.activeId)) {
      const act = mapped.find((p) => p.id === set0.activeId)
      if (act) kept = [act, ...kept.slice(1)]
    }
    scopes[scope] = { tabs: kept, activeId: set0.activeId }
  }
  saveJSON(LS_SESSION_TABS, { v: TABS_PERSIST_VERSION, scopes } satisfies PersistedTabs)
}

let persistTabsTimer: ReturnType<typeof setTimeout> | undefined
// schedulePersistTabs：写入的**唯一收口**。挂在 store 订阅上（见文件末尾的 subscribe），
// 而不是逐个 action 里插桩 —— 本仓的教训是「逐个调用点同步必漏」（syncTabsToRightTab 的注释
// 里写着同一句话）：tabsByScope 有 openTab/activateTab/closeTab/reopenTab/syncWebTabs/
// syncTabsToRightTab/deleteSession/deleteWorkspace 八条改写路径。
function schedulePersistTabs(): void {
  if (persistTabsTimer) clearTimeout(persistTabsTimer)
  persistTabsTimer = setTimeout(() => {
    persistTabsTimer = undefined
    persistTabsNow(useAppStore.getState())
  }, TABS_PERSIST_DEBOUNCE_MS)
}
// flushPersistTabs：把防抖窗口里的待写立即落盘（e2e/L1 断言与「关窗前收尾」用）。
function flushPersistTabs(): void {
  if (persistTabsTimer) { clearTimeout(persistTabsTimer); persistTabsTimer = undefined }
  persistTabsNow(useAppStore.getState())
}
// flushPendingPersistTabs：**只在有在途写入时**落盘。给 beforeunload 用 —— 关窗前把还没攒够
// 防抖窗口的那一次改动补上；没有在途改动就不写（免得每次关窗都白写一次整表，也免得把
// 「刚被外部改过的 localStorage」覆盖掉）。
function flushPendingPersistTabs(): void {
  if (!persistTabsTimer) return
  flushPersistTabs()
}

// syncTabsToRightTab：把标签集**单向**收敛到 rightTab。幂等，且「已一致时返回同一个对象引用」
// —— 调用方（含 React effect）据此判断「无需 set」，避免 effect → setState → effect 的循环。
//
// 三条分支对应三种真实场景：
//   ① 该 scope 还没有标签集（首次进入）→ 以 rightTab 为准建默认集
//   ② 集非空但激活项与 rightTab 不符（e2e 直接 setState 复位 / loadViewToTop 恢复了别的会话）→ 切到同 kind 的标签
//   ③ 集为空（用户把标签关光了）→ **保持空**，否则刚关掉的标签会被立刻建回来
function syncTabsToRightTab(s: AppState): TabSet {
  const cur = s.tabsByScope[scopeKeyOf(s)]
  if (!cur) {
    const tab = makeTab(s.rightTab, s)
    return { tabs: [tab], activeId: tab.id, closed: [] }
  }
  const active = cur.tabs.find((t) => t.id === cur.activeId)
  if (active && active.kind === s.rightTab) {
    // 已一致：只把「刚恢复」标记清掉（它是一次性的，不清的话用户之后手动切标签会被它拦住）。
    return cur.restored ? { ...cur, restored: false } : cur
  }
  if (!cur.tabs.length) return cur
  // 阶段 5：**重启恢复**的标签集要先于 rightTab 存在 —— rightTab 是会话 view 的字段，重启后由
  // 恢复响应才建出来（默认 'activity'），与恢复的激活标签毫无关系。首次同步时由标签集**反向**
  // 收敛 rightTab（tabPatch 会把激活标签的 kind 镜像过去），而不是把激活标签改成 rightTab 的
  // kind（那正是「重启后标签集回来了、激活的却是别的标签」）。
  //
  // 标记**不在这里清**：`restored` 的含义是「这个 scope 的标签集自本次启动起还没被用户碰过」，
  // 期间 rightTab 只可能来自 resetView / loadViewToTop 的默认值（用户的选择一定走
  // openTab/activateTab/closeTab/reopenTab，那些路径会把标记连标签集一起换掉）→ 标签集权威。
  // 清标记的时机有二：① 已一致（上面那条分支）；② 任何显式标签动作。这样「重启后切走再切回
  // 来」也仍然是该 scope 自己的激活标签（否则切回来会被默认的 activity 顶掉）。
  if (cur.restored) return { ...cur }
  const hit = cur.tabs.find((t) => t.kind === s.rightTab)
  if (hit) return { ...cur, activeId: hit.id }
  const tab = makeTab(s.rightTab, s)
  return { tabs: [...cur.tabs, tab], activeId: tab.id, closed: cur.closed }
}

// tabPatch 把新的标签集写回 state，**同时**把 rightTab 镜像成激活标签的 kind
//（有会话 → 写该会话 view；无会话 → 只写顶层，与 setRightTab 的既有口径一致）。
// 集为空时保留原 rightTab：此时内容区由 activeTab===null 渲染空态，rightTab 只是个遗留值。
function tabPatch(s: AppState, scope: ScopeKey, set0: TabSet): Partial<AppState> {
  const active = set0.tabs.find((t) => t.id === set0.activeId)
  const kind: RightTab = active?.kind ?? s.rightTab
  const tabsByScope = { ...s.tabsByScope, [scope]: set0 }
  const sid = s.activeSessionId
  if (!sid) return { tabsByScope, rightTab: kind }
  const v = s.views[sid] ?? emptyView()
  return { ...setView(s, sid, { ...v, rightTab: kind }), tabsByScope }
}

// 标签列表的结构等价判断：syncWebTabs 在每个 embed-nav 事件上都会跑（导航/标题变化很频繁），
// 没有这个短路就会每次都造新数组 → 无谓重渲染。逐元素比引用即可（复用原实例是刻意的）。
function sameTabList(a: TabInstance[], b: TabInstance[]): boolean {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
  return true
}

// makeWebTab：给「内嵌浏览器的一个页面」造标签实例。两条数据源的 id 空间不同：
//   native = 原生视图池的 'tN' → 存 viewId（切/关走 embedActivate/embedCloseTarget）
//   mcp    = MCP 页列表的数字 pageId → 存 pageId（切/关走截图与 closeBrowserPage）
function makeWebTab(source: 'native' | 'mcp', key: string): TabInstance {
  const id = `tab${++tabSeq}`
  return source === 'native'
    ? { id, kind: 'web', viewId: key }
    : { id, kind: 'web', pageId: Number(key.replace(/^p/, '')) }
}

// webTabKey：web 标签在**当前数据源**下的身份（另一数据源的身份返回 undefined → 不参与本次同步）。
function webTabKey(t: TabInstance, source: 'native' | 'mcp'): string | undefined {
  if (t.kind !== 'web') return undefined
  if (source === 'native') return t.viewId
  return t.pageId != null ? `p${t.pageId}` : undefined
}

// samePreviewTarget：一个「待重建的恢复标签」（阶段 5）是不是就对应这个页面 —— 用户点它 →
// 重建出视图 → syncWebTabs 用它把视图接回**原来那个标签实例**（否则恢复的标签永远停在
// 待重建态，点一次还多一个重复标签）。
// 口径与主进程 embed:open-target 的去重一致：两侧都有 srcPath 时比 srcPath（office 转换
// 产物每次换临时 html_path，只有源文件认得出「是同一个」），否则比 url。
function samePreviewTarget(t: WebTabInstance, x: { url: string; srcPath?: string }, ws: string | undefined): boolean {
  const a = t.srcPath
  const b = x.srcPath
  if (a && b) return relIfInside(a, ws) === relIfInside(b, ws)
  return !!t.url && t.url === x.url
}


// 浏览器面板状态（web tab）按工作区隔离：切换工作区时清空上一个工作区的页列表/截图/
// loading。浏览器连接与页/截图都是 per-workspace（bridge runtime(ws).plugins 隔离），
// 跨工作区残留会造成「显示上一个工作区的浏览器调用」+ 拉取状态错乱。
// 文件内容缓存（filePreviews）同批清空，理由同构：它的键是**工作区相对**路径，而两个工作区
// 里同名文件（src/a.ts）的相对路径一模一样 —— 不清就会把 A 的内容显示在 B 的标签里。
// 清空不丢功能：文件面板在缓存缺失时会经 ensureFilePreview 重新拉取。
function clearBrowser(): Partial<AppState> {
  // §6.2：切换工作区/会话 → 递增代际；**保留 inFlight 记录**（不清空）——
  // 旧 workspace 的迟到响应到达时仍能查到 inFlight.gen（旧代际）→ browserStale=true
  // → 丢弃，不写入新 workspace。若清空 inFlight，旧响应 inFlight=undefined → 误判
  // 非 stale → 旧数据写入新 workspace（代际保护失效）。
  browserGeneration++
  return { browserPages: [], browserShot: undefined, browserBusy: false, filePreviews: {} }
}

// —— M6：内嵌浏览器全局事件（导航状态 / AI 动作）——
// 订阅挂在 store 层而非 BrowserTab 组件：无论网页 tab 是否打开都能收到，
// 从而实现「首次打开页面 / 出现浏览器操作 → 自动展开右侧 web tab」的产品语义。
// 注意：开启插件本身不展开侧栏（用户明确要求）。
export interface EmbedTargetInfo {
  id: string
  url: string
  title: string
  loading: boolean
  // R0：与主进程 embed.ts 的 EmbedTargetInfo 保持镜像（sid 用于按会话过滤页列表；
  // visible 是 applyPanelVisibility 的判定结果，供诊断/验收读）。
  sid?: string
  visible?: boolean
  // 预览的**源文件路径**（'' = 非文件预览）：office 转换产物每次换临时 html_path，只有源文件
  // 认得出「是同一个文件」。阶段 5 起随 url 一起进持久化快照（恢复时按它走预览路径重建）。
  srcPath?: string
  // 阶段 3：视图池 LRU 休眠（见 docs/RIGHT_PANEL_TABS_PLAN.md §11.4）。
  // sleeping 条目**仍在 targets 里**（原 id）—— 标签身份靠它，故 syncWebTabs 必须把它们
  // 当作「页面存在」（从 targets 消失才是真关闭）；restoring = 重建中（标签显示「正在恢复…」）。
  sleeping?: boolean
  restoring?: boolean
}
export interface EmbedNavState extends EmbedTargetInfo {
  canGoBack: boolean
  canGoForward: boolean
  activeId: string | null
  targets: EmbedTargetInfo[]
}
export interface EmbedAiAction {
  kind: 'move' | 'down' | 'up' | 'wheel' | 'type' | 'key' | 'nav'
  x?: number
  y?: number
  text?: string
}

// describeEmbedAction 把 AI 动作转成一句人话（move 高频且无信息量，不展示）。
export function describeEmbedAction(a: EmbedAiAction): string {
  switch (a.kind) {
    case 'up': return i18nT('embed.click').replace('{x}', String(Math.round(a.x ?? 0))).replace('{y}', String(Math.round(a.y ?? 0)))
    case 'wheel': return i18nT('embed.wheel')
    case 'type': return a.text ? i18nT('embed.type').replace('{text}', a.text) : ''
    case 'key': return a.text ? i18nT('embed.key').replace('{text}', a.text) : ''
    case 'nav': return a.text ? i18nT('embed.nav').replace('{text}', a.text) : ''
    default: return ''
  }
}

let embedAiQuietTimer = 0
// 「首个真实页面」触发的展开记账（阶段 4：单值 → 按**视图**的一次性集合）。
//
// 修的两件事：
//   ① 原来是单值 `embedAutoOpenedForSid`（只记得**最后触发**的那个会话）→ 切到任何别的会话后
//      `!== curSid` 立刻成立，下一次任何 nav 事件都会再展开一次当前面板（本阶段修的主缺陷）。
//   ② 判据从「有没有页面」改成「**这个页面属不属于你当前看的会话**」，且只有**用户显式动作**
//      打开的那个视图（previewOwnedViews）才有资格触发展开（见 onEmbedNav）。
// 记账按**视图 id** 而不是 scope：展开是「用户这次显式打开的动作」的一次性效果 —— 同一会话里
// 用户再点一个文件（新的视图 id）时该展开仍然生效，而同一个视图的迟到 nav 事件只展开一次
//（用户中途收起面板后不该被弹回来）。
// 视图 id 由主进程 't'+自增序号分配、不复用 → 关掉后残留的 id 不会误命中新视图。
const embedAutoOpenedViews = new Set<string>()
let embedPagesFetchAt = 0
let embedBridgeAlive = false // 收到任意命令响应 = bridge stdin/stdout 链路已通（防自举竞态丢命令）
// R3：由**用户显式预览**打开的视图 id（openInBrowserTab 的 activate:true 路径登记，
// 见 rememberPreviewView）。
//
// 为什么需要：nav 事件里「内嵌视图出现首个真实页面」的几种来源长得一模一样，只有**打开时**的
// 来源能区分，而它们该做的聚焦**不同** ——
//   · 用户显式预览（点产出卡 / 文件树 / 消息里的「在浏览器打开」）→ 展开右栏 + 切 web，
//     但绝不收起左栏（收起左栏是「Browser Use 接管视界」的语义，用在「预览一个本地文件」上
//     是误伤：用户没要求收起文件树）
//   · AI/Browser Use 开页、Agent 后台产出（activate:false）→ **一律不动面板**（阶段 4 口径）。
//     它们**不登记**进这个集合，于是落到 onEmbedNav 的「其余情形」分支。
// id 是主进程的 't'+自增序号、不复用 → 视图被关掉后残留的 id 不会误命中新视图。
const previewOwnedViews = new Set<string>()
// §6.9/R3：用户显式预览的视图出现首个真实页面时，展开右侧 web tab。
// 语义：已打开（右栏展开且 tab 是 web）不重复；收起后重开。
// 阶段 4：**只有这一个调用方**（onEmbedNav 里预览来源的分支）—— 其余三条自动路径
//（AI 动作 / AI 首个页面 / Agent 后台产出）一律不再动面板，见 onEmbedNav / onEmbedAiAction。
// 用 focusBrowserForPreview 而非 focusBrowser：预览本地文件不该收起左栏（收起左栏是
// 「Browser Use 接管视界」的语义，用在预览上是误伤）。
function focusWebTabIfClosed(): void {
  const st = useAppStore.getState()
  if (st.rightExpanded && st.rightTab === 'web') return
  st.focusBrowserForPreview()
}

// 登记「这个视图是预览打开的」（供 focusWebTabIfClosed 的来源判断，见 previewOwnedViews）。
// activate:true：主进程必然把新视图设为 activeId（activate 的语义就是「现在就显示它」）→ 直接取。
// activate:false：主进程**故意**不改 activeId（那正是「不抢视线」本身），拿不到 id → 与打开前的
// 池快照求差集（与 e2e 探针同一写法：不依赖实现把 id 放在响应的哪个字段）。
// 时序上足够安全：登记发生在 embedOpenTarget 的 IPC 响应之后**同一个微任务**里，而「真实页面出现」
// 的 nav 事件要等页面加载（另一次 IPC 推送）→ 登记必先于判定。
function rememberPreviewView(res: { state?: unknown } | undefined, activate: boolean, beforeIds: Set<string>): void {
  const s = res?.state as { activeId?: string | null; targets?: { id?: string }[] } | undefined
  // activate:true = **用户显式预览** → 登记为「预览来源」：它的首个真实页面到达时走
  // focusWebTabIfClosed（展开 + 切 web，不收左栏），这是 T1「点文件即见内容」的兜底。
  if (activate) {
    if (s?.activeId) previewOwnedViews.add(s.activeId)
    return
  }
  // activate:false = **后台打开**（阶段 4 起 Agent 产出走这条）→ **不登记**为预览来源。
  //
  // 为什么不登记（阶段 4 的关键收口）：nav 事件里唯一会展开面板的分支就是「activeId 属于
  // previewOwnedViews」；后台产出的视图若也登记进去，它的首个真实页面一到就会把面板弹开 ——
  // 正是本阶段要修的「自动激活」。不登记后它落到「其余情形一律不动面板」，
  // 与 AI/Browser Use 开页同一处置（三档规则见 docs/RIGHT_PANEL_TABS_PLAN.md §4）。
  // 视图 id 由**池快照求差集**得到（同 previewOwnedViews 的写法，不依赖实现把 id 放在响应
  // 哪个字段），只用来标「未读」：标签条上显示未读点，直到用户真的把它切到前台（见 onEmbedNav
  // 的清除）。这是原型 docs/demo/right-panel-preview.html 的口径：「Agent 产出在后台加入标签，
  // 不切换面板」，用户靠未读点知道「有东西在等我」——没有未读点，后台打开就是**看不见**的
  //（等于没打开），这正是二次修正当初要解决的问题。
  const fresh: string[] = []
  for (const t of s?.targets ?? []) if (t.id && !beforeIds.has(t.id)) fresh.push(t.id)
  if (fresh.length) {
    useAppStore.setState((st) => ({ unreadViews: [...new Set([...st.unreadViews, ...fresh])] }))
  }
}

// rebuildWebTab：把一个**重启恢复**出来的 web 标签在「它即将被显示」这一刻真正建出来
//（阶段 5「视图按需重建」的落点）。
//
// 为什么不在恢复时建：阶段 3 刚用「视图池 LRU + 休眠」解决了「十几个 WebContentsView 常驻」
// 的内存问题，启动时按存储 eager 重建等于把它原样还回去 —— 而用户很可能整个会话都不会点其中
// 任何一个。所以恢复只把**标签**放回标签条（待重建态），视图留给真正要看它的那一刻。
//
// 唯一调用方 = RightPanel 的 effect（判据：面板展开 + rightTab=web + 激活标签就是待重建的那个）。
// 为什么收口在渲染层而不是 activateTab：待重建标签成为激活标签有**两条**路径 —— 用户点它、
// 以及「重启时它就是激活标签，用户只是把面板展开」；后者若不在渲染层兜住，用户看到的就是一个
// 永远空的网页面板（点已激活的标签不会再触发任何东西）。两处都做则会双开视图。
//
// 两条重建路径（与 openFile 对可视类文件的处置同源）：
//   · 有 srcPath 且是可视类文件 → 走 **openFile**（= 用户点文件的同一条路）：office 会被
//     **重新转换**（重启后上一次的临时 html_path 已失效），pdf/图片/html 走 file:// 预览；
//   · 其余（http(s) 网页 / data: 内容 / 源文件已不是可视类）→ 按 url 快照直接开。
// 建出来的视图由 syncWebTabs 的**认领**逻辑接回这个标签实例（按 srcPath/url 命中），
// 不会多出一个新标签。失败口径与其它用户显式动作一致（响亮报错），两条路径内部各自 toast。
export function rebuildWebTab(tab: WebTabInstance): void {
  const url = tab.url ?? ''
  const src = tab.srcPath ?? ''
  const st = useAppStore.getState()
  if (src && visualKindOf(src)) {
    st.openFile(src)
    return
  }
  if (!url) return
  const d = typeof window !== 'undefined' ? window.desktop : undefined
  if (!d?.embedOpenTarget) return
  void d.embedOpenTarget(url, { sid: st.activeSessionId ?? '', activate: true })
    .then((r) => {
      if (r && r.ok === false) {
        st.showToast(i18nT('browser.restoreFail').replace('{name}', url).replace('{err}', r.error ?? ''), 'error')
      }
    })
    .catch(() => { /* IPC 失败：主进程不可用，静默（与其它 embed 调用的容错一致） */ })
}

// —— §6.10：写类工具产出**可视类**文件 → 在右侧栏准备好（文件版的 §6.9）——
//
// 产品口径（ADR-2″，2026-09 **三次修正** —— 阶段 4 自动激活收口）：
//   1. 只对「二进制/可视类」自动打开（pdf/xlsx/图片/html/svg）。文本类（md/go/txt/代码）
//      **不**自动打开 —— 每次改代码都抢视线会刷屏；文本改动仍靠既有产出卡。
//   2. **只加标签 + 未读点**：Agent 后台产出可视文件后，右侧栏**不展开、不切标签**，
//      那个标签带未读点等用户自己点（口径见 docs/RIGHT_PANEL_TABS_PLAN.md §4 三档规则）。
//      为什么推翻二次修正的「产出即展示」（原文：「展开 + 切到 web tab + 激活那个标签」）：
//      那是**进程级**口径 —— 不管用户在哪个会话、正在看什么，一次产出就把界面切走；
//      用户 2026-09-20 的明确口径是「Agent 后台产出只加/刷新标签 + 未读点，不切、不展开」。
//      二次修正当时要解决的**真问题**（「原实现没有任何一步会把它显示出来」）由**未读点 +
//      折叠态 rail 角标**解决 —— 见 rememberPreviewView 的 activate:false 分支（它把新视图
//      标成 unreadViews）与 RightPanel 的 rail 角标：用户随时看得见「有东西在等我」，
//      但看不看、什么时候看由用户决定。
//      左栏一如既往**不**收（收左栏是 Browser Use「AI 接管视界」的语义，用在预览上是误伤）。
//   3. 要**定位到那个文件**（不是笼统打开侧栏）：locateFile 落文件栏选中态 + 请求内容 ——
//      这条与「展不展开」无关，保留：用户下次进文件栏时停在该文件上。
//   4. 失败口径**不变**（原 R3 ④）：自动化路径一律带 quiet:true —— 撞视图上限
//      （阶段 3 起是 LRU 休眠，只有「保护集里一个可驱逐对象都没有」才会失败）是背景条件，
//      响亮报错只属于用户显式点击。
//
// 为什么预览走内嵌浏览器而不是文件栏：pdf/xlsx/图片/html 在本仓库只有**一条**渲染通道 =
// 内嵌浏览器（file:// / bridge 转 HTML）。文件栏（rightTab='file'）只能内联渲染文本
//（shiki/Markdown/CSV），承载不了这些类型。自动打开也不再切过去（那会换掉用户正在看的 tab），
// fileFocus 只是给用户自己去看时留的定位锚。
//
// 去重锚 = 「本会话有没有开过这个文件」。为什么不能只靠「已经打开过就不开」：
// 工具过程中（tool_run_end）先开一次、agent_end 的 artifacts 再兜底收敛一次，
// 是**两套触发源指向同一个文件**的正常情形。
// 2026-09 语义修正（用户口径）：命中这个锚**不再什么都不做**，而是**刷新已存在的标签** ——
// Agent 反复写同一个产出文件时，用户已经打开过的那个标签必须跟到最新内容，否则他看到的是
// 过期结果。新建/刷新的判定不在这里（这里没有池快照，只有「开过没有」的记账），而由
// openInBrowserTab / previewOfficeFile 按**主进程真实视图池**（srcPath → URL）决定：
// 命中 → 刷新/导航；没有 → 新建。故这里两条路径合并成「照常走预览通道」。
const autoOpenedVisual = new Map<string, string>() // sid → 本会话最后自动打开的可视文件路径
let autoOpenSeq = 0 // 供 e2e 断言「真的打开过」的自增序号（不影响生产逻辑）

/** 自动打开状态（仅 e2e 断言用：无法从 DOM 反推「没有打开第二次」）。 */
export function autoOpenVisualState(): { path: string; seq: number } {
  return { path: autoOpenedVisual.get(useAppStore.getState().activeSessionId ?? '') ?? '', seq: autoOpenSeq }
}

// 工作区内绝对路径 → 相对（与 RightPanel.toRelIfInside 同口径）。
// 为什么 store 侧也要做：openFile 的选中态匹配是**字面比较**（RightPanel 里
// filePreview.path === item.path），而文件活动列表的 path 来自工具 args、是相对路径。
// 传绝对路径会让列表里已存在的文件永不匹配 → 详情永远停在「正在读取文件…」
//（实测复现，见 ArtifactCard 的同类注释）。
function relIfInside(path: string, ws: string | undefined): string {
  const n = path.replaceAll('\\', '/').replace(/^\.\//, '')
  if (!ws || !n.startsWith('/')) return n
  const w = ws.replaceAll('\\', '/').replace(/\/+$/, '')
  if (n === w) return ''
  if (n.startsWith(w + '/')) return n.slice(w.length + 1)
  return n // 工作区外：保持绝对（bridge 的 resolveWorkspacePath 支持绝对路径）
}

/**
 * fileKeyOf：文件内容缓存与 file 标签身份的**归一化键**（阶段 2）。
 *
 * 为什么需要它：`file_preview` 的路径有三种写法（对话/产出卡传工作区相对、左栏文件树传
 * 绝对、bridge 原样回显请求路径），而「同一个文件」必须落同一个标签、同一份内容 ——
 * 否则从对话点开与从文件树点开会各开一个标签（用户看到的就是重复），内容也会各读一份。
 * 口径与 RightPanel.sameFilePath 完全一致（工作区内绝对 → 相对），故两处的判定不会漂移。
 */
export function fileKeyOf(path: string, ws: string | undefined): string {
  return relIfInside(path, ws)
}

// —— 文件内容缓存（阶段 2）——
//
// 为什么从「全局单例 filePreview」改成「按归一化路径索引的映射」：
//   ① `file_preview` 的响应只带 path，**不带「是哪个标签请求的」**（bridge main.go 的
//      file_preview 分支原样回显请求路径）→ 响应处理方无法把内容投递给某个标签；
//   ② 而内容本来就是 path 的函数 → 按 path 索引天然无竞态，同一文件的 diff / source
//      两个标签还能共享同一份内容（不必读两遍）。
// 上限：文件内容最大 1 MiB（bridge maxPreviewBytes，超限截断），不设上限就会随「开过的
// 文件数」无界增长。超出按**写入序**淘汰最久未刷新的一条 —— 被淘汰不丢功能：组件在
// 「该 path 没有内容」时会经 ensureFilePreview 重新拉取（见该 action）。
export const FILE_PREVIEWS_MAX = 8
// 已发出但还没回来的 file_preview 请求（键 → 发起时刻）。作用有两个：
//   · 去重（React StrictMode 下 effect 会跑两遍，同一 path 不该发两次 IPC）；
//   · 自愈（响应因任何原因没回来时，超过 FILE_PREVIEW_RETRY_MS 允许重发）。
const filePreviewInFlight = new Map<string, number>()
const FILE_PREVIEW_RETRY_MS = 5000
// requestFilePreview：发一次 file_preview 并登记在途（键按**发起时**的工作区归一化）。
// 顺序很重要：**先登记再 send** —— mock transport 的响应是**同步**发出的（见 send 的
// im_chat_list 注释），后登记会留下一条永远没人释放的在途记录（那条 path 从此不再补拉）。
// 释放走 pending[id].path + respWs（响应回显的发起时工作区）→ 与这里的键一致（见 dispatchEvent）。
function requestFilePreview(path: string): void {
  filePreviewInFlight.set(fileKeyOf(path, useAppStore.getState().workspace), Date.now())
  send({ type: 'file_preview', payload: { path } })
}

// putFilePreview：写入一条内容缓存（已存在则移到末尾 = 最近刷新），并裁剪到上限。
// 返回**新对象**（zustand 的浅比较靠引用变化触发重渲染；原地改会让面板不更新）。
// 导出仅为单测（scripts/test-right-panel-tabs.mjs 的 K46-K48 直接验证淘汰策略）：
// 它无法从 store 的公开动作触发（只有 bridge 的 file_preview 响应会写缓存）。
export function putFilePreview(map: Record<string, FilePreview>, key: string, v: FilePreview): Record<string, FilePreview> {
  const next: Record<string, FilePreview> = {}
  for (const [k, val] of Object.entries(map)) if (k !== key) next[k] = val
  next[key] = v
  const keys = Object.keys(next)
  if (keys.length > FILE_PREVIEWS_MAX) for (const k of keys.slice(0, keys.length - FILE_PREVIEWS_MAX)) delete next[k]
  return next
}

/**
 * 产出可视文件后自动打开/更新右侧栏（§6.10）。path 为绝对或工作区相对均可。
 *
 * 返回是否**新开**了标签（false = 这个文件本会话已经开过，这次只刷新了已有标签）。
 * e2e 与调用方据此判定「有没有多开重复标签」。
 * 2026-09 三次修正（阶段 4）：**不切标签、不展开面板**（只加/刷新标签 + 未读点）；
 * 返回值仍是「有没有新开标签」，与面板动不动无关（完整口径见上方 §6.10 注释块）。
 * 异步的原因：未读点由 openInBrowserTab 的 activate:false 分支在 IPC 响应后写入
 *（rememberPreviewView），要等它落地 —— 否则「标签有了但没未读点」这一段窗口里
 * 后台产出等于看不见。调用方不 await 也没关系（两处调用点都不看返回值）。
 * 触发源（两处，都指向同一个文件也不会多开标签）：
 *   · tool_run_end：write/edit（以及带 Diff.path 的 run_python）结束 → 过程中就打开，
 *     用户能实时看到产出（这是用户原话「生成完成后打开侧边栏」的主路径）；
 *   · agent_end 的 artifacts 清单：兜底收敛（Go 侧已判定 created/modified，比工具名
 *     更权威；工具结果缺 diff 的异常序也由它补齐）。
 */
export async function autoOpenVisualFile(rawPath: string, sid: string): Promise<boolean> {
  if (!rawPath) return false
  const kind = visualKindOf(rawPath)
  if (!kind) return false // 规则 1：文本类不自动打开
  const st = useAppStore.getState()
  if (sid && st.activeSessionId && sid !== st.activeSessionId) return false // 后台会话不得抢当前视图
  const rel = relIfInside(rawPath, st.workspace)
  // 去重锚（见 autoOpenedVisual 的注释）：开过 → 下面的预览通道会**刷新那个标签**；没开过 → 新建。
  // 两条路径调的是同一段代码，区别只在主进程视图池里有没有这个 srcPath/URL 的标签。
  const alreadyOpened = autoOpenedVisual.get(sid) === rel
  autoOpenedVisual.set(sid, rel)
  autoOpenSeq++
  // 阶段 4：**不再** setRightExpanded(true)。展开与否由用户决定 —— 后台产出只把内容准备好
  //（标签 + 未读点），用户下次进右栏/点标签就能看到；旧行为（§6.10 二次修正的「产出即展示」）
  // 会把正在看别的会话/别的标签的用户切走（用户 2026-09-20 明确口径）。
  // 规则 3：定位到该文件（文件栏选中/滚动 + 内容请求）——locateFile **不切 tab**（切 tab 会
  // 换掉用户正在看的那一页，正是 ADR-2 要修的失败模式）。
  st.locateFile(rel)
  // 规则 2（阶段 4 =「后台加标签 + 未读点」）：再按类型走既有预览通道，一律 activate:false +
  // quiet:true —— 新建/刷新**后台**标签、不激活、不碰面板（activate:false 分支的语义由
  // e2e:container 的 T3b/T13 钉住），失败也不因背景条件弹错（口径见上面 §6.10 第 4 条）。
  // 为什么 pdf/xlsx/html/svg 都进内嵌浏览器：本仓库对它们的**唯一**渲染机制就是它
  //（file:// + Chromium；xlsx 经 bridge 转 HTML 后同一条路）。
  // 图片同理走 file://（Chromium 原生渲染图片）。
  // 两个通道的 Promise 都要 await：未读点是在 IPC 响应之后写进 unreadViews 的（见上 doc）。
  const visual = kind === 'xlsx' || kind === 'docx' || kind === 'pptx'
    ? st.previewOfficeFile(rel, kind as 'xlsx' | 'docx' | 'pptx', { activate: false, quiet: true })
    : kind === 'pdf' || kind === 'image' || kind === 'browser'
      ? st.openInBrowserTab(rel, { activate: false, quiet: true })
      : Promise.resolve()
  try {
    await visual
  } catch { /* 失败口径：背景路径不弹错（见上面 §6.10 第 4 条）；标签有没有开成由池状态决定 */ }
  // 返回值语义（与上面 doc 一致）：true = **新开**了标签（本会话第一次产出这个文件）。
  // 刷新已有标签返回 false —— 调用方 / e2e（verify_auto_open_visual 的 D1）用它判定
  // 「有没有多开标签 / 把用户拽回去」，刷新这两件事都没做。
  return !alreadyOpened
}

function subscribeEmbedEvents(): void {
  if (typeof window === 'undefined' || !window.desktop?.onEmbedNav) return

  // —— 阶段 3：运行中会话集合 → 主进程（视图池 LRU 保护集）——
  // 会话在跑任务时 Agent 随时可能再命令它的页面 → 那些视图**不得**被休眠（命令打到死
  // target 是硬禁忌）。主进程看不到会话运行状态，故由渲染层推送；口径 = store 的 busySids
  //（与左栏运行点同源：有 running run 节点的会话）。
  // 用**订阅**而不是在各转移点插桩：busySids 由事件级维护（agent_start/end、tool_run_*、
  // 快照归一化等十几处 setView 顺带更新），订阅是唯一不会漏的收口点。比较序列化键 → 只在
  // 集合真的变化时发一次 IPC（state 变化本身很频繁，不能每次变化都发）。
  let lastBusyKey = ''
  const pushBusySids = (sids: ReadonlySet<string>): void => {
    const key = [...sids].sort().join(',')
    if (key === lastBusyKey) return
    lastBusyKey = key
    void window.desktop?.embedSetBusySids?.(key ? key.split(',') : [])
  }
  useAppStore.subscribe((s) => pushBusySids(s.busySids))

  // —— 自举：不依赖网页 tab 挂载，尽早把「默认视图 + 调试代理 + attach」链路立起来 ——
  // 这样外部（AI 工具流之外）的浏览器操作也能被承载。注意阶段 4 起「首个真实页面」**不再**
  // 自动展开右侧栏（只有用户显式预览会，见 onEmbedNav）；自举只建链路，不动界面。
  // 视图零常驻（§4.6.3）：自举创建**隐藏**的 about:blank 视图作为 attach/页面枚举载体
  // （embedShow 创建 + embedHide 立即隐藏——不占据侧边栏、不可见，满足「不期望
  // about:blank 占位影响用户」）；视图不显示，只有真实页面出现时才由 tab/导航显示。
  void (async () => {
    const d = window.desktop!
    // 等待 bridge 就绪信号：Go 侧 bridge_ready 事件（§4.4）或任意命令响应兜底。
    for (let w = 0; w < 240 && !embedBridgeAlive; w++) await new Promise((r) => setTimeout(r, 500))
    for (let i = 0; i < 36; i++) {
      try {
        const st0 = (await d.embedStatus()) as { ready?: boolean; attached?: boolean } | undefined
        if (!st0?.ready) {
          await d.embedShow({}) // 创建隐藏视图（供 attach/页面枚举）
          await d.embedHide() // 立即隐藏：不占据侧边栏（视图零常驻的可见性语义）
        }
        const proxy = await d.embedDebuggerProxy()
        if (proxy?.ok && proxy.endpoint) {
          useAppStore.getState().attachEmbedView(proxy.endpoint)
          const st1 = (await d.embedStatus()) as { attached?: boolean }
          if (st1.attached) {
            return
          }
        }
      } catch { /* 未就绪：下轮重试 */ }
      await new Promise((r) => setTimeout(r, 5000))
    }
  })()

  window.desktop.onEmbedNav((s) => {
    const nav = s as EmbedNavState
    useAppStore.setState({ embedNav: nav })
    // 未读点清除（R4）：激活页 + 面板可见 = 用户此刻真的看到了它 → 不再是未读。
    // 只认「激活 且 面板可见」：仅激活而面板收起（如自动打开时的顺延）不算看过 —— 否则
    // 切会话/后台动作会把未读点悄悄吃掉，用户永远不知道有产出在等他。
    const stv = useAppStore.getState()
    if (nav.activeId && stv.rightExpanded && stv.rightTab === 'web' && stv.unreadViews.includes(nav.activeId)) {
      stv.markPreviewSeen(nav.activeId)
    }
    // —— 内嵌页面 → 面板级 web 标签（阶段 1.5）——
    // 一页一标签：视图池是真相源，这里做声明式收敛（用户/AI 都可能绕过 UI 直接关页）。
    // 过滤口径与旧内层标签条逐字一致：真实 URL（非 about:blank 占位）+ 归属本会话。
    const navSid = stv.activeSessionId ?? ''
    stv.syncWebTabs(
      'native',
      nav.targets
        // 阶段 3：sleeping 条目**必须**算「存在」—— 主进程让它们以原 id 留在 targets 里
        //（url 取休眠快照），正是为了让这里不把标签当「页面已关闭」删掉（§11.4）。
        // 只有真关闭（条目从 targets 消失）才会删标签。
        .filter((tp) => tp.url && tp.url !== 'about:blank' && (!tp.sid || tp.sid === navSid))
        .map((tp) => ({ key: tp.id, url: tp.url ?? '', title: tp.title ?? '', srcPath: tp.srcPath ?? '' })),
      nav.activeId ?? null,
    )
    // 首次打开：**本会话**的内嵌视图出现真实 URL（非 about:blank 占位）→ 展开右侧 web tab。
    //
    // 阶段 4 的三处收口（用户口径：「切会话/切工作区/新建会话时自动打开右栏很别扭」）：
    //   ① 判据按 **scope** 过滤 —— 与上面 syncWebTabs 逐字同一口径（`!tp.sid || tp.sid === navSid`）。
    //      过滤前是 `targets.some(real)`：别的会话/AI 在别处开的页也命中 → 切到新会话后
    //      下一次任何 nav 事件又把当前面板弹开（这是本阶段修的主缺陷）。
    //   ② 记账按视图 id（embedAutoOpenedViews），不再是「只记得最后一个会话」的单值。
    //   ③ **只有用户显式动作会展开**：activeId 是 previewOwnedViews 里的视图（用户点产出卡/
    //      文件树/消息里的「在浏览器打开」→ openInBrowserTab 的 activate:true 路径）→ 展开 + 切 web。
    //      其余来源（AI/Browser Use 开页、Agent 后台产出）**一律不动面板** —— 标签由 syncWebTabs
    //      建好、未读点由 rememberPreviewView 标上，用户想切再切（三档规则见
    //      docs/RIGHT_PANEL_TABS_PLAN.md §4）。
    const realScoped = nav.targets.filter((tp) => tp.url && tp.url !== 'about:blank' && (!tp.sid || tp.sid === navSid))
    const activeView = nav.activeId ?? ''
    if (realScoped.length && previewOwnedViews.has(activeView) && !embedAutoOpenedViews.has(activeView)) {
      embedAutoOpenedViews.add(activeView)
      // 用户显式预览（activate:true）→ 展开 + 切 web tab，但**不收左栏**。
      //（正常路径下 tab 已被 openInBrowserTab 同步切到 web，此处是「用户中途收起了面板」的兜底。）
      focusWebTabIfClosed()
    }
    // 其余情形一律**不动面板**（阶段 4 明确反转的旧行为）：
    //   · 真实页面来自预览打开的视图但不是当前激活那个 → 用户没要求看它，不切、不展开；
    //   · AI/Browser Use 打开的视图 → 旧行为是 focusBrowser（展开 + 切 web + **收起左栏**），
    //     现已去掉：AI 在别的会话/后台操作浏览器不该改写用户正在看的界面。
    //     AI 的可见性兜底是 embedAiActive 呼吸徽标（见 onEmbedAiAction）+ 标签未读点。
    //     「用户让 AI 用浏览器」这条路径仍会展开：browser_* 工具开始时走 focusBrowserForUse
    //    （tool_run_start），那是用户显式发起的任务。
    // 导航/新开 tab → 防抖刷新 MCP 页列表（回退截图模式数据源）
    const now = Date.now()
    if (now - embedPagesFetchAt > 1200) {
      embedPagesFetchAt = now
      useAppStore.getState().fetchBrowserPages()
    }
  })
  window.desktop.onEmbedAiAction((a) => {
    const act = a as EmbedAiAction
    useAppStore.setState({ embedAiActive: true })
    window.clearTimeout(embedAiQuietTimer)
    embedAiQuietTimer = window.setTimeout(() => useAppStore.setState({ embedAiActive: false }), 2500)
    const desc = describeEmbedAction(act)
    if (desc) useAppStore.setState({ embedLastAction: desc })
    // 阶段 4：这里**不再** focusWebTabIfClosed()。AI 操作浏览器只该「确保标签存在 + 标未读 +
    // 徽标呼吸」，不该切标签/展开右栏/收左栏（三档规则见 docs/RIGHT_PANEL_TABS_PLAN.md §4）。
    // 旧行为（focusBrowser：展开 + 切 web + 收起左栏）会把用户正在看的界面改写掉 ——
    // AI 在**别的会话**里操作浏览器时，当前会话的面板也被掀开，正是用户反馈的那类「很别扭」。
    // 上面两行（embedAiActive 呼吸徽标 + embedLastAction 动作说明）就是本条档位的全部可见反馈；
    // 标签与未读点由 onEmbedNav 的 syncWebTabs / rememberPreviewView 负责。
  })
}

// v0.1：模型窗口常量（DeepSeek 视觉模型按 128k 估；后续走 profile 配置）
export const MODEL_WINDOW = 128_000
// 1M 上下文窗口（模型窗口标记为 1M 时使用）
export const MODEL_WINDOW_1M = 1_000_000

// 已知模型上下文窗口（客户端维护；设置模型时直接传该模型的最大 max_tokens）。
// 模型列表 API 不返回窗口 —— OpenAI 兼容 /v1/models 只回 id/created/owned_by 等元数据
// （go-openai Model struct 实证），无法得知各模型 max_tokens。故拉取模型列表/设置模型时
// 用此表直接传最大上下文，未知模型回退 MODEL_WINDOW(128k)。如需调整在此扩行。
// 数据源：provider 注册表（provider/models_*.go，models.dev 2026-08，与 pi 对齐）。
export const KNOWN_MODEL_WINDOWS: Record<string, Record<string, number>> = {
  deepseek: { 'deepseek-v4-flash': 1000000, 'deepseek-v4-pro': 1000000, 'deepseek-v4-flash-vision-exp': 1000000, 'deepseek-chat': 64000 },
  openai: { 'gpt-4o': 128000, 'gpt-4o-mini': 128000, 'gpt-4.1': 1047576, 'gpt-5-nano': 400000, 'gpt-5': 400000, 'gpt-5-mini': 400000, 'gpt-5.3-chat-latest': 128000, 'gpt-3.5-turbo': 16385 }, // gpt-4.1 = 1M 上下文
  // OpenCode Go（chat/completions 族）：模型真实窗口（models.dev 2026-08；与注册表一致）
  opencode: {
    'deepseek-v4-flash': 1000000, 'deepseek-v4-pro': 1000000, 'deepseek-v4-flash-vision-exp': 1000000,
    'glm-5.1': 202752, 'glm-5.2': 1000000, 'glm-5.3': 1000000, 'glm-5.3-flash': 1000000,
    'kimi-k3': 1048576, 'kimi-k2.7-code': 262144, 'kimi-k2.6': 262144, 'kimi-k2.5': 262144,
    'mimo-v2.6-pro': 1048576, 'mimo-v2.6-flash': 1048576, 'mimo-v2.5': 1000000, 'mimo-v2.5-pro': 1048576, 'hy3': 256000, 'hy4-preview': 1024000,
    'qwen3.6-plus': 1000000, 'qwen3.7-max': 1000000, 'qwen3.7-plus': 1000000, 'qwen3.8-max': 1000000, 'qwen3.8-flash': 1000000,
    'minimax-m2.7': 204800, 'minimax-m3': 1000000, 'gpt-5.6-luna': 1050000, 'grok-4.6': 500000,
    'muse-spark-1.2-contributor': 1048576, 'longcat-2.0': 1000000,
  },
  kimi: { 'kimi-k2.6': 262144, 'kimi-k2.7-code': 262144, 'kimi-k2.7-code-highspeed': 262144, 'kimi-k2.7': 262144, 'kimi-k2.5': 262144, 'kimi-k2-thinking': 262144, 'kimi-k3': 1048576 },
  zhipu: { 'glm-4.6': 204800, 'glm-4.7': 204800, 'glm-5-turbo': 200000, 'glm-5.1': 202752, 'glm-5.2': 1000000, 'glm-5.2-highspeed': 1000000, 'glm-5.3': 1000000, 'glm-5.3-flash': 1000000, 'glm-5.3-highspeed': 1000000, 'glm-5': 1000000, 'glm-5v-turbo': 200000, 'glm-4.5-flash': 131072, 'glm-4.5': 131072, 'glm-4.5-air': 131072 },
  anthropic: { 'claude-sonnet-4-5': 1000000, 'claude-sonnet-4-6': 1000000, 'claude-sonnet-5': 1000000, 'claude-opus-4-5': 200000, 'claude-opus-4-6': 1000000, 'claude-opus-4-7': 1000000, 'claude-opus-4-8': 1000000, 'claude-opus-5': 1000000, 'claude-haiku-4-5': 200000, 'claude-fable-5': 1000000 },
}

// 单次输出上限兜底（未知模型）：与 bridge defaultMaxTokens 一致。
export const MODEL_MAX_TOKENS_DEFAULT = 8192

// 子 agent 并发槽上限缺省值（与 bridge 的 subagent.DefaultConcurrency 对齐 = 1000）：
// 主池（agent_spawn）与辅池（subagent_explore）各自独立。缺省高是为了「永远不撞上限」——
// 槽满时工具调用是同步阻塞的（主 Agent 整批挂住，任何超时都解不开），所以这个值只能
// 当失控护栏用，别按「保守点更安全」的直觉调小。
export const SUBAGENT_CONCURRENCY_DEFAULT = 1000

// 已知模型默认单次输出上限（max_tokens；Provider 面板未配置时回退用）。
// 数据源：provider 注册表（provider/models_*.go，models.dev 2026-08，与 pi 对齐）。
export const KNOWN_MODEL_MAX_TOKENS: Record<string, Record<string, number>> = {
  deepseek: { 'deepseek-v4-flash': 384000, 'deepseek-v4-pro': 384000, 'deepseek-v4-flash-vision-exp': 384000, 'deepseek-chat': 8192 },
  openai: { 'gpt-4o': 16384, 'gpt-4o-mini': 16384, 'gpt-4.1': 32768, 'gpt-5-nano': 128000, 'gpt-5': 128000, 'gpt-5-mini': 128000, 'gpt-5.3-chat-latest': 16384, 'gpt-3.5-turbo': 4096 },
  opencode: {
    'deepseek-v4-flash': 384000, 'deepseek-v4-pro': 384000, 'deepseek-v4-flash-vision-exp': 384000,
    'glm-5.1': 32768, 'glm-5.2': 131072, 'glm-5.3': 131072, 'glm-5.3-flash': 131072,
    'kimi-k3': 131072, 'kimi-k2.7-code': 262144, 'kimi-k2.6': 65536, 'kimi-k2.5': 262144,
    'mimo-v2.6-pro': 131072, 'mimo-v2.6-flash': 131072, 'mimo-v2.5': 128000, 'mimo-v2.5-pro': 128000, 'hy3': 64000, 'hy4-preview': 64000,
    'qwen3.6-plus': 65536, 'qwen3.7-max': 65536, 'qwen3.7-plus': 65536, 'qwen3.8-max': 131072, 'qwen3.8-flash': 131072,
    'minimax-m2.7': 131072, 'minimax-m3': 131072, 'gpt-5.6-luna': 128000, 'grok-4.6': 500000,
    'muse-spark-1.2-contributor': 131072, 'longcat-2.0': 131072,
  },
  kimi: { 'kimi-k2.6': 262144, 'kimi-k2.7-code': 262144, 'kimi-k2.7-code-highspeed': 262144, 'kimi-k2.7': 262144, 'kimi-k2.5': 262144, 'kimi-k2-thinking': 262144, 'kimi-k3': 131072 },
  zhipu: { 'glm-4.6': 131072, 'glm-4.7': 131072, 'glm-5-turbo': 131072, 'glm-5.1': 32768, 'glm-5.2': 131072, 'glm-5.2-highspeed': 131072, 'glm-5.3': 131072, 'glm-5.3-flash': 131072, 'glm-5.3-highspeed': 131072, 'glm-5': 131072, 'glm-5v-turbo': 131072, 'glm-4.5-flash': 98304, 'glm-4.5': 98304, 'glm-4.5-air': 98304 },
  anthropic: { 'claude-sonnet-4-5': 64000, 'claude-sonnet-4-6': 128000, 'claude-sonnet-5': 128000, 'claude-opus-4-5': 64000, 'claude-opus-4-6': 128000, 'claude-opus-4-7': 128000, 'claude-opus-4-8': 128000, 'claude-opus-5': 128000, 'claude-haiku-4-5': 64000, 'claude-fable-5': 128000 },
}

// DeepSeek V4 Flash supports vision input through the OpenAI-compatible
// chat-completions content-part format (text + image_url/data URL).
// 视觉模型默认表（对齐后端 provider 注册表 models.dev 2026-08）：
// 仅列真实支持 image 输入的模型；其余默认纯文本（前端上传按钮据此显隐）。
export const KNOWN_MODEL_INPUT_TYPES: Record<string, Record<string, string[]>> = {
  deepseek: { 'deepseek-v4-flash-vision-exp': ['text', 'image'] },
  openai: { 'gpt-4o': ['text', 'image'], 'gpt-4o-mini': ['text', 'image'], 'gpt-4.1': ['text', 'image'], 'gpt-5-nano': ['text', 'image'], 'gpt-5': ['text', 'image'], 'gpt-5-mini': ['text', 'image'], 'gpt-5.3-chat-latest': ['text', 'image'] },
  zhipu: { 'glm-5v-turbo': ['text', 'image'], 'glm-5.3-flash': ['text', 'image'] },
  kimi: { 'kimi-k2.6': ['text', 'image'], 'kimi-k2.7-code': ['text', 'image'], 'kimi-k2.7-code-highspeed': ['text', 'image'], 'kimi-k2.5': ['text', 'image'], 'kimi-k3': ['text', 'image'] },
  anthropic: { 'claude-sonnet-4-5': ['text', 'image'], 'claude-sonnet-4-6': ['text', 'image'], 'claude-sonnet-5': ['text', 'image'], 'claude-opus-4-5': ['text', 'image'], 'claude-opus-4-6': ['text', 'image'], 'claude-opus-4-7': ['text', 'image'], 'claude-opus-4-8': ['text', 'image'], 'claude-opus-5': ['text', 'image'], 'claude-haiku-4-5': ['text', 'image'], 'claude-fable-5': ['text', 'image'] },
  opencode: { 'deepseek-v4-flash-vision-exp': ['text', 'image'], 'kimi-k3': ['text', 'image'], 'kimi-k2.6': ['text', 'image'], 'kimi-k2.7-code': ['text', 'image'], 'kimi-k2.5': ['text', 'image'], 'mimo-v2.6-pro': ['text', 'image'], 'mimo-v2.6-flash': ['text', 'image'], 'mimo-v2.5': ['text', 'image'], 'glm-5.3-flash': ['text', 'image'], 'qwen3.6-plus': ['text', 'image'], 'qwen3.7-plus': ['text', 'image'], 'qwen3.8-max': ['text', 'image'], 'qwen3.8-flash': ['text', 'image'], 'minimax-m3': ['text', 'image'], 'gpt-5.6-luna': ['text', 'image'], 'grok-4.6': ['text', 'image'], 'muse-spark-1.2-contributor': ['text', 'image'] },
  minimax: { 'MiniMax-M3': ['text', 'image'] },
  xai: { 'grok-4.6': ['text', 'image'], 'grok-4.5': ['text', 'image'] },
}

// 全局设置默认值（对齐 electron/main.ts + mock；API key 不在此，走 providerSave 加密存储）
// 模型窗口/输出上限/输入类型 = provider 注册表真实值（models.dev 2026-08，与 pi 对齐）。
export const DEFAULT_SETTINGS: AppSettings = {
  theme: 'dark',
  lang: 'zh',
  narrative_skin: 'current', // 会话页样式：默认现状样式（v3 为组件样式，见 lib/narrativeSkin.ts）
  provider: {
    active: 'deepseek',
    deepseek: {
      base_url: 'https://api.deepseek.com/v1',
      models: { 'deepseek-v4-flash': 1000000, 'deepseek-v4-pro': 1000000, 'deepseek-v4-flash-vision-exp': 1000000, 'deepseek-chat': 64000 },
      max_tokens: { 'deepseek-v4-flash': 384000, 'deepseek-v4-pro': 384000, 'deepseek-v4-flash-vision-exp': 384000, 'deepseek-chat': 8192 },
      protocol: 'chat_completions',
      input_types: { 'deepseek-v4-flash-vision-exp': ['text', 'image'] }, // DeepSeek vision 实验模型
    },
    openai: { base_url: 'https://api.openai.com/v1', models: { 'gpt-4o': 128000 }, max_tokens: { 'gpt-4o': 16384 }, protocol: 'chat_completions', input_types: { 'gpt-4o': ['text', 'image'], 'gpt-4o-mini': ['text', 'image'], 'gpt-4.1': ['text', 'image'] } },
    opencode: {
      base_url: 'https://opencode.ai/zen/go/v1',
      models: { 'deepseek-v4-flash': 1000000, 'deepseek-v4-pro': 1000000, 'deepseek-v4-flash-vision-exp': 1000000, 'glm-5.1': 202752, 'glm-5.2': 1000000, 'glm-5.3': 1000000, 'glm-5.3-flash': 1000000, 'kimi-k3': 1048576, 'kimi-k2.7-code': 262144, 'kimi-k2.6': 262144, 'kimi-k2.5': 262144, 'mimo-v2.6-pro': 1048576, 'mimo-v2.6-flash': 1048576, 'mimo-v2.5': 1000000, 'mimo-v2.5-pro': 1048576, 'hy3': 256000, 'hy4-preview': 1024000, 'qwen3.6-plus': 1000000, 'qwen3.7-max': 1000000, 'qwen3.7-plus': 1000000, 'qwen3.8-max': 1000000, 'minimax-m2.7': 204800, 'minimax-m3': 1000000, 'gpt-5.6-luna': 1050000, 'grok-4.6': 500000, 'muse-spark-1.2-contributor': 1048576, 'longcat-2.0': 1000000 },
      max_tokens: { 'deepseek-v4-flash': 384000, 'deepseek-v4-pro': 384000, 'deepseek-v4-flash-vision-exp': 384000, 'glm-5.1': 32768, 'glm-5.2': 131072, 'glm-5.3': 131072, 'glm-5.3-flash': 131072, 'kimi-k3': 131072, 'kimi-k2.7-code': 262144, 'kimi-k2.6': 65536, 'kimi-k2.5': 262144, 'mimo-v2.6-pro': 131072, 'mimo-v2.6-flash': 131072, 'mimo-v2.5': 128000, 'mimo-v2.5-pro': 128000, 'hy3': 64000, 'hy4-preview': 64000, 'qwen3.6-plus': 65536, 'qwen3.7-max': 65536, 'qwen3.7-plus': 65536, 'qwen3.8-max': 131072, 'minimax-m2.7': 131072, 'minimax-m3': 131072, 'gpt-5.6-luna': 128000, 'grok-4.6': 500000, 'muse-spark-1.2-contributor': 131072, 'longcat-2.0': 131072 },
      protocol: 'chat_completions',
      input_types: {}, // 默认纯文本；多模态模型可在 Provider 设置勾选「图片」
    },
    kimi: {
      base_url: 'https://api.moonshot.cn/v1',
      models: { 'kimi-k2.6': 262144, 'kimi-k2.7-code': 262144, 'kimi-k2.7-code-highspeed': 262144, 'kimi-k2.5': 262144, 'kimi-k2-thinking': 262144, 'kimi-k3': 1048576 },
      max_tokens: { 'kimi-k2.6': 262144, 'kimi-k2.7-code': 262144, 'kimi-k2.7-code-highspeed': 262144, 'kimi-k2.5': 262144, 'kimi-k2-thinking': 262144, 'kimi-k3': 131072 },
      protocol: 'chat_completions',
      input_types: {}, // 默认纯文本；多模态模型可在 Provider 设置勾选「图片」
    },
    zhipu: {
      base_url: 'https://open.bigmodel.cn/api/paas/v4',
      models: { 'glm-4.6': 204800, 'glm-4.7': 204800, 'glm-5-turbo': 200000, 'glm-5.1': 202752, 'glm-5.2': 1000000, 'glm-5.3': 1000000, 'glm-5.3-flash': 1000000, 'glm-5v-turbo': 200000, 'glm-4.5-flash': 131072 },
      max_tokens: { 'glm-4.6': 131072, 'glm-4.7': 131072, 'glm-5-turbo': 131072, 'glm-5.1': 32768, 'glm-5.2': 131072, 'glm-5.3': 131072, 'glm-5.3-flash': 131072, 'glm-5v-turbo': 131072, 'glm-4.5-flash': 98304 },
      protocol: 'chat_completions',
      input_types: {}, // 默认纯文本；多模态模型可在 Provider 设置勾选「图片」
    },
  },
  mcpServers: {},
  agent: { reminder_rounds: 30, max_subagents: SUBAGENT_CONCURRENCY_DEFAULT, max_explore_subagents: SUBAGENT_CONCURRENCY_DEFAULT },
  sandbox: { mode: 'seatbelt' },
  permissions: { computer_approved_apps: [] }, // Computer Use 已授权 App（设置 → 插件 → Computer Use）；空 = 全部拒绝
  cron: { enabled: true, auto_clean: true }, // 定时任务默认全开（2026-08-19 用户决策）
  mesh: { session_enabled: false, remote_enabled: false }, // Session Mesh 默认全关（安全默认：开启后才注册工具）
  runtime: { stop_background_on_interrupt: false }, // 主停止只停主会话（S3-B 缺省 false，2026-08-30 用户决策）
  stt: { enabled: false }, // 语音输入默认关闭（配置 base_url/model/api_key 后开启）
}

// activeProviderCfg 当前活跃 provider 的配置（active 恒为 provider id，其值恒为 ProviderCfg；
// 索引签名值类型是 ProviderCfg | string，此处收窄）。
function activeProviderCfg(s: AppSettings): ProviderCfg | undefined {
  return s.provider[s.provider.active] as ProviderCfg | undefined
}

// 某会话所属的工作区：遍历项目表反查（问题十四——用户会切走查看其他会话，
// 后台会话的 llm_end flush 发 ask_batch 时全局 workspace 可能已变，send() 注入的
// 「当前工作区」会把命令路由到错误的 workspace → bridge session not found）。
function sessionWorkspaceOf(s: AppState, sid: string): string {
  for (const proj of s.projects) {
    if (proj.sessions?.some((x) => x.id === sid)) return proj.path
  }
  return ''
}

// 某模型的上下文窗口：优先取 harness 上报值（agent_start.context_window——loop 实际
// 记录的窗口，1M 热切换后本地表可能滞后，问题六）；否则设置里该 provider 配置的窗口；
// 未知模型回退默认 128k。
export function modelWindowOf(s: AppSettings, model: string, harnessWindow?: number): number {
  if (typeof harnessWindow === 'number' && harnessWindow > 0) return harnessWindow
  const prov = activeProviderCfg(s)
  if (prov && typeof prov.models[model] === 'number') return prov.models[model]
  return MODEL_WINDOW
}

// 某模型的单次输出上限：设置里显式配置优先；否则已知模型默认；兜底 8192。
export function modelMaxTokensOf(s: AppSettings, model: string): number {
  const prov = activeProviderCfg(s)
  const stored = prov?.max_tokens?.[model]
  if (prov && typeof stored === 'number') return stored
  const known = KNOWN_MODEL_MAX_TOKENS[s.provider.active]?.[model]
  if (typeof known === 'number') return known
  return MODEL_MAX_TOKENS_DEFAULT
}

// 某（provider, model）的输入类型：设置里显式配置优先 → 已知多模态默认 → 默认 ["text"]。
// Provider 面板可逐模型把 ["text"] 调整为 ["text","image"]（多模态 → 允许图片上传）。
export function inputTypesForProvider(s: AppSettings, pid: string, model: string): string[] {
  const cfg = s.provider[pid] as ProviderCfg | undefined
  const stored = cfg?.input_types?.[model]
  if (Array.isArray(stored) && stored.length) return stored
  const known = KNOWN_MODEL_INPUT_TYPES[pid]?.[model]
  if (Array.isArray(known) && known.length) return known
  return ['text']
}

// 当前活跃 provider 下某模型的输入类型（图片上传按钮门控）
export function modelInputTypes(s: AppSettings, model: string): string[] {
  return inputTypesForProvider(s, s.provider.active, model)
}

// 当前模型是否支持图片输入（图片上传按钮门控）
export function modelSupportsImage(s: AppSettings, model: string): boolean {
  return modelInputTypes(s, model).includes('image')
}

// 某 Provider 的默认模型（对齐 bridge defaultModel：deepseek 固定 deepseek-v4-flash-vision-exp；
// openai/opencode 取排序首个；缺失时取配置首个）。多 Provider 下 UI 默认模型与 bridge loop
// 一致，避免水合后 UI 显示一个不在活跃 Provider 模型列表里的模型。
export function defaultModelFor(s: AppSettings): string {
  const cfg = activeProviderCfg(s)
  if (!cfg) return 'deepseek-v4-flash-vision-exp'
  if (s.provider.active === 'openai' || s.provider.active === 'opencode' || s.provider.active === 'kimi' || s.provider.active === 'zhipu') {
    const keys = Object.keys(cfg.models).sort()
    if (keys.length) return keys[0]
    const fallback: Record<string, string> = { openai: 'gpt-4o', kimi: 'kimi-k2.7', zhipu: 'glm-4.6' }
    return fallback[s.provider.active] ?? 'deepseek-v4-flash-vision-exp'
  }
  if (typeof cfg.models['deepseek-v4-flash-vision-exp'] === 'number') return 'deepseek-v4-flash-vision-exp'
  return Object.keys(cfg.models)[0] ?? 'deepseek-v4-flash-vision-exp'
}

// 输入队列上限：运行中最多排队 10 条，满则禁用输入（SDK inbox 实际缓冲 64，前端门禁先行）
export const QUEUE_CAP = 10

let blockSeq = 1
let cmdSeq = 0
let booting = false
// 启动水合时登记活跃路径：仅其 list 响应允许「自动恢复最近会话 / 空则自动建首个会话」。
// 主动添加/切换工作区不登记 → list 只并入列表，绝不偷偷建会话（新建会话是显式动作）。
const recoverPaths = new Set<string>()

// —— 发送时创建会话（2026-08-16）：无活跃会话（新工作区 / 切工作区未选会话）时，首次 ask
// 自动新建会话 —— 文本/附件入 firstAsks 缓冲 + 发 new_session；响应激活会话后统一 flush 进叙述。
// 发送时才建：未发送不残留空会话；sessionPending 合并等待期的多次连发为一次创建。
// （2026-08-22）缓冲项携带完整内容块（含图片）：无会话时发送的图片不再被静默丢弃，
// 且恢复/切换时可按内容块原样投递。
let sessionPending = false
const firstAsks: { text: string; contents?: QueuedContent[] }[] = []
// 会话切换加载态定时器（issue 4）：无论有无缓存都短暂显示模糊遮罩，超时兜底关闭。
let switchTimer: ReturnType<typeof setTimeout> | undefined
// Compressor failures are not guaranteed to produce CompressEnd (for example
// when the SDK's request context expires). Keep one watchdog per session/run so a
// SubAgent compression cannot clear a different session or the main Agent state.
const compressTimers = new Map<string, ReturnType<typeof setTimeout>>()
function compressionKey(sid: string, runId?: string): string {
  return `${sid}\u0000${runId || ''}`
}
function armCompressWatchdog(sid: string, runId?: string, ms = 5 * 60_000 + 15_000): void {
  const key = compressionKey(sid, runId)
  const previous = compressTimers.get(key)
  if (previous) clearTimeout(previous)
  const timer = setTimeout(() => {
    compressTimers.delete(key)
    useAppStore.setState((s) => {
      const v = s.views[sid]
      if (!v) return s
      if (runId) {
        const hasAgent = v.blocks.some((b) => b.kind === 'agent' && b.runId === runId)
        if (!hasAgent) return s
        const blocks = abortAgentCompression(v.blocks, runId)
        return setView(s, sid, { ...v, blocks })
      }
      if (!v.compressing) return s
      return setView(s, sid, {
        ...v,
        compressing: false,
        compressStartTs: undefined,
        compressBefore: undefined,
        compressRunId: undefined,
        compressReason: undefined,
      })
    })
  }, ms)
  compressTimers.set(key, timer)
}
function disarmCompressWatchdog(sid?: string, runId?: string): void {
  if (!sid) {
    for (const [key, timer] of compressTimers) {
      clearTimeout(timer)
      compressTimers.delete(key)
    }
    return
  }
  const key = compressionKey(sid, runId)
  const timer = compressTimers.get(key)
  if (timer) {
    clearTimeout(timer)
    compressTimers.delete(key)
  }
}
function disarmSessionCompressWatchdogs(sid: string): void {
  const prefix = `${sid}\u0000`
  for (const [key, timer] of compressTimers) {
    if (!key.startsWith(prefix)) continue
    clearTimeout(timer)
    compressTimers.delete(key)
  }
}
let gitRefreshTimer: ReturnType<typeof setTimeout> | undefined
let gitRefreshQueued = false
let wsHistAppend = false // 工作区级 git_history「加载更多」意图标记（请求前置位，响应消费后复位）
let sessHistAppend = false // 会话级 git_history「加载更多」意图标记（同上）
// 为什么 run_python 单独一条：脚本能改工作区里的任意文件，但 args 是**脚本源码**、result 是
// stdout，都推不出「改了哪个文件」—— 只有 Go 侧在产出点填的 Diff.path 能回答「这次真的动了
// 工作区吗」。所以它按 write 类待遇的**唯一**条件是带 diff.path；无条件下齐（每次跑脚本都刷新
// git 快照）会把「只是算个数」的调用也算成写改动（多一次 git 快照/历史拉取）。
function gitMutationLevel(name: string, args: string, result: string, hasDiffPath = false): 'none' | 'snapshot' | 'history' {
  if (name === 'run_python') return hasDiffPath ? 'snapshot' : 'none'
  if (name !== 'bash' && ['write_file', 'edit_file', 'move', 'delete', 'mkdir'].includes(name)) return 'snapshot'
  if (name !== 'bash') return 'none'
  const cmd = `${args}\n${result}`.toLowerCase()
  if (/\bgit\s+(commit|checkout|switch|merge|rebase|reset|stash|cherry-pick|pull|fetch|clone)\b/.test(cmd)) return 'history'
  if (/(^|[;&|]\s*)(sed|perl)\s+-i\b|(^|\s)(cp|mv|rm|touch|mkdir)\s|>|>>/.test(cmd)) return 'snapshot'
  return 'none'
}
function scheduleGitRefresh(sid: string, history = false): void {
  const st = useAppStore.getState()
  if (st.activeSessionId !== sid) return
  gitRefreshQueued = gitRefreshQueued || history
  if (gitRefreshTimer) clearTimeout(gitRefreshTimer)
  gitRefreshTimer = setTimeout(() => {
    gitRefreshTimer = undefined
    const cur = useAppStore.getState()
    if (cur.activeSessionId !== sid) { gitRefreshQueued = false; return }
    const reloadHistory = gitRefreshQueued
    gitRefreshQueued = false
    cur.loadGitSnapshot()
    if (reloadHistory) cur.loadGitHistory()
    // 变更 tab 打开时保持工作区级快照同步（双层一致性；无会话场景由 tab 自身 stale 重拉兜底）
    if (cur.rightTab === 'git') cur.loadWsGit()
  }, 500)
}
// —— 启动水合门控（2026-08-16）：list 水合尚未完成时，首次 ask 只缓冲不自动建会话 ——
// 否则重载后在会话列表到达前打字会新建空会话、丢掉要恢复的历史上下文（P3 竞态）。
// hydrating 在 start() 发起 list 时置位、活跃工作区 list 响应处理完复位；
// 水合期缓冲的 firstAsks 由「恢复会话激活」或「空列表回退自动建会话」统一 flush。
let hydrating = false

// —— 会话持久化 UX（2026-08-14）：项目列表 + 会话标题覆盖存 localStorage ——
// 项目列表：{paths, active, pinned, lastSession} —— 重启后左栏项目树、置顶状态和最近会话恢复。
// 标题覆盖：{titleKey(path,sid) -> title} —— 左栏展示优先取覆盖，留空/删除 = 恢复自动标题。
const LS_PROJECTS = 'go-code.projects'
const LS_TITLES = 'go-code.titles'
type PersistProjects = { paths: string[]; active: string; lastSession?: { path: string; id: string }; lastSessionAt?: number; pinned?: string[]; recent?: { path: string; id: string }[] }

// lastSession 单调守卫（2026-08-22）：只有「比已持久化的最近会话更新」的激活才能改写
// lastSession。跨实例并发 / 过期状态回写会把 lastSession 覆盖回旧会话（重启后恢复错会话、
// 左栏活跃标记错位的根因）。activeAtNow = 当前活跃会话的激活时刻（wall-clock），
// 同一会话的重复持久化保持首次时间；无活跃会话时清零。
let activeSidNow: string | undefined
let activeAtNow = 0

// 工作区置顶顺序独立于会话顺序：置顶项按用户最近一次置顶的相对顺序排列，
// 旧版本只保存 paths 时保持原有项目顺序；异常/重复数据在启动时归一化。
function orderedProjectPaths(paths: string[], pinned?: string[]): string[] {
  const uniquePaths = [...new Set(paths.filter((path) => typeof path === 'string' && path.length > 0))]
  const available = new Set(uniquePaths)
  const pinnedPaths: string[] = []
  const savedPinned = Array.isArray(pinned) ? pinned : []
  for (const path of savedPinned) {
    if (pinnedPaths.length >= MAX_WORKSPACE_PINS) break
    if (available.has(path) && !pinnedPaths.includes(path)) pinnedPaths.push(path)
  }
  const pinnedSet = new Set(pinnedPaths)
  return [...pinnedPaths, ...uniquePaths.filter((path) => !pinnedSet.has(path))]
}

function orderProjects(projects: Project[]): Project[] {
  let pinnedSeen = 0
  const normalized = projects.map((project) => {
    if (!project.pinned || pinnedSeen++ < MAX_WORKSPACE_PINS) return project
    return { ...project, pinned: undefined }
  })
  return [
    ...normalized.filter((project) => project.pinned),
    ...normalized.filter((project) => !project.pinned),
  ]
}

// —— 用户偏好持久化（2026-08-17）：mode/model/effort/persona ——
// 分层记录：全局默认（最近一次选择，重启后新会话/恢复会话的初值）+ 会话级记录
// （每个会话自己的选择，切回该会话时恢复）。全局放 LS_PREFS，会话级放
// LS_SESSION_PREFS（key = titleKey(path,sid)，与标题覆盖同构）。
const LS_PREFS = 'go-code.prefs'
const LS_SESSION_PREFS = 'go-code.sessionPrefs'
export interface Prefs {
  mode: string
  model: string
  provider: string
  effort: string
  persona: string
}
const DEFAULT_MODEL = 'deepseek-v4-flash-vision-exp'

// titleKey 标题覆盖的 key：path 与 sid 均不可能含 NUL，作安全分隔
export function titleKey(path: string, sid: string): string {
  return `${path}\u0000${sid}`
}

function loadJSON<T>(key: string): T | null {
  try {
    const raw = localStorage.getItem(key)
    return raw ? (JSON.parse(raw) as T) : null
  } catch {
    return null
  }
}
function saveJSON(key: string, v: unknown): void {
  try {
    localStorage.setItem(key, JSON.stringify(v))
  } catch {
    /* 配额/隐私模式：忽略，持久化非致命 */
  }
}

// 模块加载时从 localStorage 恢复项目树（同步）：让首帧 needsBoot 判定正确——
// 有持久项目 → 直接进应用（不挂 BootScreen，其 mock 自动 start 不触发）；
// 无 → 保持 idle，等 BootScreen 引导选目录。
const persistedBoot = loadJSON<PersistProjects>(LS_PROJECTS)
const persistedPaths = Array.isArray(persistedBoot?.paths) ? persistedBoot.paths : []
const persistedPinPaths = Array.isArray(persistedBoot?.pinned) ? persistedBoot.pinned : []
const persistedPinned = new Set(persistedPinPaths.slice(0, MAX_WORKSPACE_PINS))
const initPaths = orderedProjectPaths(persistedPaths, persistedPinPaths)
const initWorkspace = persistedBoot?.active
const initProjects = initPaths.map((path) => ({ path, sessions: [], ...(persistedPinned.has(path) ? { pinned: true } : {}) }))
const initTitles = loadJSON<Record<string, string>>(LS_TITLES) ?? {}
// —— 顶部快捷栏（recentSessions）队列语义 ——
// RECENT_MAX 与视图缓存上限一致（VIEW_CACHE_MAX）：队列容量 = 可缓存视图数。
const RECENT_MAX = 10
// withRecent 把一个会话登记进快捷栏：新会话追加到队尾；已存在的保持原位不动
// （不因重复切换而换位置）；满员时挤掉队首（index 0 = 最久未看）。
function withRecent(list: { path: string; id: string }[], path: string, id: string): { path: string; id: string }[] {
  if (!path || !id) return list
  if (list.some((r) => r.path === path && r.id === id)) return list // 已存在：位置不变
  return [...list, { path, id }].slice(-RECENT_MAX)
}

// 最近打开的会话（顶部横条 + ⌘1..10）：从持久化恢复（去重 + 上限 RECENT_MAX，与视图缓存一致）
const initRecent = Array.isArray(persistedBoot?.recent)
  ? persistedBoot.recent
      .filter((r): r is { path: string; id: string } => Boolean(r && r.path && r.id))
      .filter((r, i, arr) => arr.findIndex((x) => x.path === r.path && x.id === r.id) === i)
      .slice(-RECENT_MAX)
  : []
// 全局偏好（最近一次选择）：模块加载时同步恢复，首帧即生效（问题 2，2026-08-17）
const persistedPrefs = loadJSON<Partial<Prefs>>(LS_PREFS) ?? {}

// persistProjects 把当前项目树（paths + 活跃项目 + 最近会话）同步到 localStorage。
// 在启动、新建会话、切会话这些项目状态变化的时点调用。
function persistProjects(s: AppState): void {
  const projects = orderProjects(s.projects)
  const paths = projects.map((p) => p.path)
  const prev = loadJSON<PersistProjects>(LS_PROJECTS) ?? { paths: [], active: '' }
  const cur = s.activeSessionId ?? ''
  // 记录激活时刻：同一会话重复持久化保持首次时间；会话切换 = 新激活；无会话 = 清零。
  if (cur && cur !== activeSidNow) {
    activeSidNow = cur
    activeAtNow = Date.now()
  } else if (!cur) {
    activeSidNow = undefined
    activeAtNow = 0
  }
  const prevLastAt = typeof prev.lastSessionAt === 'number' ? prev.lastSessionAt : 0
  // 单调守卫：过期激活（比已持久化的最近会话旧）不得覆盖 lastSession ——
  // 防「实例 B 新建会话后，实例 A 的旧状态持久化把 lastSession 写回旧会话」的回归。
  const canWrite = Boolean(cur && activeAtNow >= prevLastAt)
  saveJSON(LS_PROJECTS, {
    paths,
    active: s.workspace || (paths.includes(prev.active) ? prev.active : ''),
    pinned: projects.filter((p) => p.pinned).map((p) => p.path).slice(0, MAX_WORKSPACE_PINS),
    lastSession: canWrite
      ? { path: s.workspace || prev.active, id: cur }
      : (prev.lastSession && paths.includes(prev.lastSession.path) ? prev.lastSession : undefined),
    lastSessionAt: Math.max(prevLastAt, activeAtNow),
    recent: Array.isArray(s.recentSessions) ? s.recentSessions.slice(-RECENT_MAX) : undefined,
  })
}

// persistPrefs 持久化用户选择（问题 2，2026-08-17）：全局默认（LS_PREFS = 最近一次选择）
// + 当前会话级记录（LS_SESSION_PREFS，key = titleKey(path,sid)）。switch* 时调用。
function persistPrefs(s: AppState): void {
  const cur: Prefs = { mode: s.mode, model: s.model, provider: s.settings.provider.active, effort: s.effort, persona: s.persona }
  saveJSON(LS_PREFS, cur)
  const sid = s.activeSessionId
  if (!sid || !s.workspace) return
  const prev = loadJSON<Record<string, Partial<Prefs>>>(LS_SESSION_PREFS) ?? {}
  saveJSON(LS_SESSION_PREFS, { ...prev, [titleKey(s.workspace, sid)]: cur })
}

// sessionPrefs 读某会话的偏好记录（无记录 → null，用全局默认）。
function sessionPrefs(path: string, sid: string): Partial<Prefs> | null {
  const rec = loadJSON<Record<string, Partial<Prefs>>>(LS_SESSION_PREFS) ?? {}
  return rec[titleKey(path, sid)] ?? null
}

// applySessionPrefs 恢复会话偏好：与当前 state 不同 → 更新 UI 并同步 bridge。
// new_session 响应（切会话/恢复）时调用；model 校验失败由 command_response 回退兜底。
export function applySessionPrefs(path: string, sid: string): void {
  const rec = sessionPrefs(path, sid)
  const st = useAppStore.getState()
  const globalPrefs = loadJSON<Partial<Prefs>>(LS_PREFS) ?? {}
  // 新记录精确保存 provider；兼容旧记录时，优先当前 provider，若模型仅存在于其他
  // provider 则按配置表反查。模型重名的旧记录无法无损推断，沿用当前 provider。
  const wantedModel = rec?.model ?? globalPrefs.model ?? st.model
  const requestedProvider = rec?.provider ?? globalPrefs.provider
  let provider = requestedProvider && st.settings.provider[requestedProvider]
    ? requestedProvider
    : st.settings.provider.active
  const providerCfg = st.settings.provider[provider] as ProviderCfg | undefined
  if ((!providerCfg || typeof providerCfg.models[wantedModel] !== 'number') && !requestedProvider) {
    const matches = Object.entries(st.settings.provider)
      .filter(([name, cfg]) => name !== 'active' && cfg && typeof cfg !== 'string' && typeof (cfg as ProviderCfg).models[wantedModel] === 'number')
      .map(([name]) => name)
    if (matches.length === 1) provider = matches[0]
  }
  const targetCfg = st.settings.provider[provider] as ProviderCfg | undefined
  const targetSettings = provider === st.settings.provider.active
    ? st.settings
    : { ...st.settings, provider: { ...st.settings.provider, active: provider } }
  const model = targetCfg && typeof targetCfg.models[wantedModel] === 'number'
    ? wantedModel
    : defaultModelFor(targetSettings)
  const mode = rec?.mode ?? globalPrefs.mode ?? st.mode
  const effort = rec?.effort ?? globalPrefs.effort ?? st.effort
  const persona = rec?.persona ?? globalPrefs.persona ?? st.persona
  const patch: Partial<AppState> = {}
  if (mode !== st.mode) patch.mode = mode
  if (effort !== st.effort) patch.effort = effort
  if (persona !== st.persona) patch.persona = persona as 'code' | 'work'
  if (model !== st.model) patch.model = model
  if (provider !== st.settings.provider.active) patch.settings = targetSettings
  // 即使 UI 值恰好相同，也要重应用到刚由 bridge 创建/恢复的 session；bridge 的初始值是
  // provider 默认值。缓存命中时这些命令幂等，并确保每个 session 的运行配置独立。
  send({ type: 'switch_mode', payload: { session_id: sid, mode } })
  send({ type: 'switch_effort', payload: { session_id: sid, effort } })
  send({ type: 'switch_persona', payload: { session_id: sid, persona } })
  send({ type: 'switch_model', payload: { session_id: sid, model, provider } })
  if (Object.keys(patch).length > 0) useAppStore.setState(patch)
}

interface AppState {
  bridgeStatus: 'idle' | 'starting' | 'running' | 'stopped' | 'error'
  bridgeError?: string
  workspace?: string
  projects: Project[]
  sessions: SessionMeta[]
  activeSessionId?: string
  // 最近点开的会话（顶部横条 + 快捷键切换）：{path,id} 队列，新打开追加队尾、
  // 超限移除队首，去重，上限与视图缓存一致（VIEW_CACHE_MAX=10）。setActiveSession 时更新。
  recentSessions: { path: string; id: string }[]
  titleOverrides: Record<string, string> // 会话标题覆盖（titleKey(path,sid) → 自定义标题；左栏渲染优先取）
  views: Record<string, SessionView> // 会话视图真相源（顶层字段 = 活跃会话镜像）
  // 右侧栏标签集（阶段 1）：按 scope 归属（会话 id；无会话时 = 工作区路径）。
  // **刻意不放进 SessionView**：views 有 LRU 淘汰（VIEW_CACHE_MAX=10），标签集会随会话切走被删掉。
  tabsByScope: Record<ScopeKey, TabSet>

  leftExpanded: boolean
  leftPinned: boolean
  rightExpanded: boolean
  rightTab: RightTab
  rightSize: number | null // M3：期望的右侧面板宽度（null = 不强制；浏览器内嵌时按 viewport 设）
  sessionLoading: boolean // 切换会话时的短暂加载态（导航模糊遮罩）
  inSkills: boolean
  showNewWsModal: boolean // 「新建文件夹…」modal（创建文件夹并注册工作区）
  inSettings: boolean // 设置弹窗
  inCron: boolean // 定时任务管理页（左栏入口；设置上方，CronPage 全屏子页）
  cronJobs: CronJob[] // 定时任务清单（cron_list；CronPage 数据源）
  cronRuns: CronRun[] // 运行账本（cron_runs；CronRunsPage「执行历史」）
  cronLoading: boolean // CronPage 拉取中
  cronEnabled: boolean // 定时任务功能开关（cron_list.enabled 镜像；未开启 → 页面提示先去设置开启）
  cronAutoClean: boolean // 自动清理开关镜像
  cronRunsJob: CronJob | null // 正在查看执行历史的任务（CronRunsPage 数据源；进入详情页时保留以便返回）
  activeCronRun?: CronRunDetail // 正在查看的某次执行详情（cron_runs_detail；CronRunDetailPage 数据源）
  cronDetailLoading: boolean // 详情拉取中
  cronDetailOpen: boolean // 详情子页开关（CronRunsPage → CronRunDetailPage）
  settings: AppSettings // 全局设置（theme/lang/provider；boot 水合）
  browserEngine?: 'mcp' | 'ego' // 浏览器引擎（browser_engine_get；SettingsModal 插件 tab）
  egoStatus?: { installed?: boolean; available?: boolean; app_running?: boolean; cli_found?: boolean; app_path?: string; version?: string; error?: string } // ego lite 检测结果
  meshStatus: MeshStatusInfo | null // Session Mesh 运行状态（mesh_status 查询；「别人怎么连我」）
  providerKeys: Record<string, boolean> // provider → 是否已配置 key（仅布尔指示，不回明文）
  oauthStatus: Record<string, OAuthStatus> // provider → OAuth 登录状态（oauth_status 响应）
  fetchedModels: Record<string, string[]> // provider → 拉取的模型列表（list_models 响应）
  modelCosts: Record<string, Record<string, ModelPrices>> // provider → model → 注册表内置价（list_models capabilities.cost；价格编辑器空覆盖时回显）
  providerPresets: ProviderPreset[] // 内置 Provider 预设（list_provider_presets ← providers.json 单一事实源）
  mode: string // hitl(用户确认) / auto(自动执行) / full-access(完全访问)；会话中可随时切换，下一轮生效
  effort: string // none / minimal / low / medium / high / xhigh / max
  persona: 'code' | 'work' // 双 Persona（Code 工程师 / Work 办公助理；切换当前会话下一轮生效）
  model: string // 当前模型（switch_model；下一次请求生效）
  filePreviews: Record<string, FilePreview> // 文件内容缓存（file_preview 响应；键 = fileKeyOf 归一化路径。阶段 2：按标签隔离，不再是全局单例）
  fileFocus?: FileFocus // 对话文件点击 → 文件 Tab 精确定位（seq 允许重复点击同一路径）
  // officeBusy 正在转换的文件路径（xlsx/docx/pptx 任一在途；驱动按钮禁用与「转换中…」）。
  // 三种格式共用一个槽位而不是各一个：同一时刻用户点的是**一个**文件，多槽位只会让
  // 按钮态多出「A 格式在转、B 格式的按钮该不该亮」这类无意义的组合判定。
  //
  // 为什么字段名不叫 xlsxBusy：它从 2026-09 起覆盖三种格式，旧名会让后来者以为只跟 xlsx 相关。
  officeBusy?: string
  question?: PendingQuestion // 模型→用户提问（ask_user；活跃会话镜像，弹提问 modal）
  release?: PendingRelease // 沙箱拦截放行确认（sandbox_blocked；活跃会话镜像，弹放行 modal）

  blocks: MsgBlock[]
  approvals: Approval[]
  runNodes: RunNode[]
  diffs: Diff[]
  todos: TodoItem[]
  usage: UsageAgg
  ctxTokens?: number // 顶层镜像：当前上下文占用（Composer ctx 环 / Statusbar）
  // 顶层镜像：ctxTokens 口径（true = 压后字符估算，非真实 usage；UI 以 ≈ 标注）
  ctxTokensEstimated?: boolean
  contextWindow?: number // 顶层镜像：harness 上报的上下文窗口（占比分母单一事实源，问题六）
  pendingQueueError?: string // 顶层镜像：暂存队列推送失败原因
  turns: TurnMetric[]
  costUsd: number
  // —— Git 状态（会话级镜像 + 工作区级探测）——
  // 会话级：git_snapshot 响应写入活跃会话 view，经 viewFields 镜像顶层（组件读顶层零改动）。
  // 工作区级（wsGit*）：loadWsGit 不依赖会话 —— 添加/切换工作区、启动水合时自动探测，
  // 让「添加本地 git 项目为工作区」无需先建会话即可在状态栏 / 变更面板识别出仓库。
  git: GitStatus
  gitFiles: GitFile[]
  gitLoading: boolean
  gitError?: string
  wsGit?: GitStatus
  wsGitLoading: boolean
  wsGitError?: string
  wsGitFiles: GitFile[] // 工作区级变更文件（含 staged 标记与 +/- 行数）
  wsGitHistory: GitCommit[] // 工作区级提交历史（无会话也可浏览）
  wsGitHasMore?: boolean // 历史分页：是否还有更多（len == limit 粗判）
  wsGitHistLoading?: boolean // 历史加载中（「加载更多」防连点）
  wsGitAt?: number // 快照到达时刻（变更 tab 打开时 stale 判断，>30s 重拉）
  wsFileDiffs: Record<string, string> // 工作区级文件 diff 缓存（path → unified）
  wsCommitDiffLoading?: string // 正在加载 diff 的提交 hash
  // —— Git 写操作（P1）——
  gitOpBusy?: string // 进行中的写操作命令类型（git_stage/...），防并发点击
  gitOpError?: string // 写操作失败信息（面板顶部错误条；可关闭）
  gitBranches?: GitBranchInfo[] // 本地分支清单（P2 分支切换器；点开弹层时拉取）
  // —— 主 Agent 压缩活动镜像（右栏任务 Tab 数据源；SessionView 同名字段的顶层镜像）——
  compressing: boolean
  compressStartTs?: number
  compressBefore?: number
  compressRunId?: string
  compressReason?: string
  // —— 链路 tab：trace（SessionView 同名字段的顶层镜像）——
  traceEvents: AnyEvent[]
  traceList?: TraceSummary[]
  traceListTotal?: number
  traceListLoading?: boolean
  traceHasMore?: boolean
  activeTraceId?: string
  traceDetail?: TraceDetail
  traceFallback?: TraceDetail
  // —— 其余会话级镜像（viewFields/topViewFields 往返完整）——
  aborted: boolean
  gitHistory: GitCommit[]
  gitHasMore?: boolean // 会话级历史分页镜像（变更 tab「加载更多」）
  gitHistLoading?: boolean // 会话级历史加载态镜像
  thinkStartTs: Record<string, number>
  turnThinkMs: Record<string, number>
  lastLLMBlockId?: number
  metrics?: MetricsReport // 跨会话聚合看板（metrics 命令报告）
  metricsAt?: number // 报告到达时刻（使用统计页/空会话图表判断是否 stale，避免重复全量扫描）
  metricsFrom?: string // 报告窗口起点（YYYY-MM-DD；null = 全历史）
  skills: SkillInfo[] // 工作区技能清单（skills 命令；SkillsPage + /skill 面板数据源）
  // 上下文构成分区（context_breakdown 命令；Composer ctx 环 hover 面板数据源）。
  // 会话级：带 sessionId，渲染方须校验归属（切会话后旧响应不得当新会话展示）。
  ctxBreakdown?: CtxBreakdown
  externalSkillsStatus?: ExternalSkillsStatus
  externalSkillsLoading: boolean
  externalSkillsImporting: boolean
  externalSkillsError?: string
  externalSkillsImported?: number
  skillDetail?: SkillDetail // 技能详情（skill_get；SkillsPage 预览）
  marketSkills?: MarketSkill[] // 远程市场浏览结果（skill_browse；SkillsPage 市场区块）
  markets: MarketInfo[] // 已保存市场（market_list；SkillsPage 市场管理）
  mcpServers: MCPServerInfo[] // MCP 服务器清单 + 状态（mcp_list；SettingsModal MCP tab）
  mcpLayers: MCPLayerInfo[] // 配置层状态（mcp_list.layers；项目维度查看 + 损坏恢复可见）
  mcpSaving: boolean // MCP 配置保存中（mcpSave + mcp_set 热应用）
  // 项目级 MCP 汇总（bridge mcp_projects；MCP 页「项目」区数据源）。
  // 只含前端发过去的工作区；exists=false（目录被删）在 UI 侧过滤掉。
  mcpProjects: MCPProjectInfo[]
  mcpProjectsLoading: boolean
  mcpProjectSaving: boolean // 项目级保存中（mcp_project_set）
  // hooks 配置来源汇总（bridge hooks_list；钩子页数据源）。
  hooksInfo?: HooksInfo
  hooksLoading: boolean
  hooksSaving: boolean
  plugins: PluginInfo[] // 插件清单 + 状态（plugin_list；SettingsModal 插件 tab）
  imConfig?: IMConfig // IM 集成配置（im_config_get；SettingsModal IM tab）
  imSchemas: IMGatewaySchema[] // IM 网关 Schema（im_schema_list；动态表单）
  imStatuses: Record<string, string> // 网关 id → 状态（im_status）
  imChats: Record<string, IMChatInfo[]> // gateway_id → 群列表（im_chat_list）
  imBindPending?: IMBindRequest // 待确认的 IM 绑定请求（im_bind_confirm_requested 事件）
  browserConfig?: BrowserCfg // 浏览器插件配置（browser_config_get；插件详情设置表单）
  browserPages: BrowserPage[] // 浏览器打开页面（browser_pages；web tab）
  embedNav?: EmbedNavState // M6：内嵌视图导航状态（store 层订阅，tab 未开也更新）
  /**
   * R4：后台（activate:false）打开的预览标签 id —— 用户还没看过，标签条上显示未读点。
   * 与 `previewOwnedViews`（模块级 Set，来源判定）分开：那个是「谁开的」（不参与渲染），
   * 这个是「用户看过没」（要驱动 UI 重渲染，故必须在 state 里）。
   */
  unreadViews: string[]
  markPreviewSeen(id: string): void
  embedAiActive: boolean // M5/M6：AI 正在操作内嵌浏览器（呼吸徽标）
  embedLastAction: string // 最近一条 AI 动作说明（点击/输入/导航）
  browserShot?: string // 当前页截图 data URL（browser_screenshot；web tab）
  browserBusy: boolean // 浏览器面板拉取中（页列表/截图）
  ttftMs?: number
  llmStartTs?: number
  turnStartTs?: number
  toolStartTs: Record<string, number>
  streaming: boolean
  pendingQueue: QueuedInput[]
  runningAgentCount: number // 活跃会话镜像：运行中的子 agent 数（右栏角标/Agent tab）
  busySids: ReadonlySet<string> // 正在运行（有 running run 节点）的会话 id 集合（左栏运行点；事件级维护，不随 chunk 扫描）

  start(workspace?: string): Promise<void>
  autoBoot(): void // 启动：有持久项目 → 直接恢复（跳过 BootScreen）；无 → 保持 idle
  newSession(): void
  renameSession(path: string, sid: string, title: string): void
  setActiveSession(id: string, projectPath?: string): void
  setActiveWorkspace(path: string): void // 切换活跃工作区（注册缺失项目 + 水合列表，不自动建会话）
  toggleWorkspacePin(path: string): void // 工作区置顶/取消置顶（最多 5 个，持久化）
  toggleSessionPin(path: string, sid: string): void // 会话置顶/取消置顶（每工作区最多 5 个，持久化到 bridge）
  removeRecentSession(path: string, sid: string): void // 从顶部快捷栏移除该会话（仅移除快捷项，不删除会话）
  addWorkspace(): void // 「选择文件夹…」入口：pickWorkspace → setActiveWorkspace
  createSessionIn(path: string): void // 在工作区下直接建会话（先切为活跃；每工作区悬停 +）
  deleteSession(path: string, sid: string): void // 删除会话：桥停进程+删磁盘 + 本地状态移除
  deleteWorkspace(path: string): void // 删除工作区：桥删 .go-code 元数据 + 本地项目移除（保留用户文件）
  ask(text: string, contents?: QueuedContent[]): void
  updatePending(id: number, text: string): void // 编辑暂存队列某条（推送前；确认后生效）
  removePending(id: number): void // 从暂存队列移除某条（推送前）
  flushPending(sid?: string): void // 把暂存队列整队一次性推送（ask_batch）；幂等（仅推送未推送项）
  approve(id: string, approve: boolean, editedArgs?: string): void
  answerQuestion(answers: { question_id: string; answer: string }[]): void
  releaseSandbox(approve: boolean): void // 沙箱拦截放行决策（放行=该命令绕过沙箱重跑一次）
  promoteTask(taskId: string): void // 运行中长工具 → 后台任务（主 loop 立即拿占位继续；后台完成自动回传）
  interruptTask(taskId: string): void
  interrupt(): void
  compact(): void
  switchMode(mode: string): void
  switchEffort(effort: string): void
  switchPersona(name: 'code' | 'work'): void
  switchModel(model: string, provider?: string): void
  refreshGit(): void
  loadGitSnapshot(): void
  loadGitDiff(path: string, staged?: boolean): void
  loadWsGit(): void // 工作区级 git 探测（不依赖会话；添加/切换工作区即自动识别）
  loadWsGitHistory(appendMore?: boolean): void // 工作区级提交历史（appendMore=追加下一页）
  loadWsFileDiff(path: string, staged?: boolean): void // 工作区级单文件 diff
  loadWsCommitDiff(hash: string): void // 工作区级单提交 diff
  loadGitHistory(appendMore?: boolean): void // 会话级提交历史（appendMore=追加下一页）
  // —— Git 写操作（P1；成功后自动双层重拉，失败置 gitOpError）——
  gitStage(paths: string[]): void
  gitUnstage(paths: string[]): void
  gitDiscard(paths: string[], untracked?: boolean): void // 破坏性：调用方必须先经 ConfirmModal 确认
  gitCommit(message: string, stageAll?: boolean): void
  clearGitOpError(): void
  // —— 分支与仓库初始化（P2）——
  loadGitBranches(): void // 拉取本地分支清单（点开分支弹层时）
  gitCheckout(branch: string): void // 切换本地分支（成功后双层重拉 + 分支清单刷新）
  gitInitRepo(): void // 非仓库空态引导：在工作区初始化仓库
  loadCommitDiff(hash: string): void
  loadGitFileDiff(path: string, staged?: boolean): void
  openFile(path: string, toolId?: string, mode?: 'source', startLine?: number): void
  locateFile(path: string, toolId?: string, mode?: 'source', startLine?: number): void // R3：只定位（fileFocus + file_preview），**不切 tab、不展开右栏**
  ensureFilePreview(path: string): void // 阶段 2：内容缓存缺失时按需补拉（切回标签 / 缓存被淘汰 / 切工作区后）
  closeFile(): void
  fetchMetrics(opts?: { from?: string; to?: string; project?: string }): void // 强制拉取聚合报告
  ensureMetrics(): void // 按需拉取：报告缺失或超龄才发命令（空会话图表等高频入口）
  fetchSkills(): void // 技能清单（SkillsPage + /skill 命令补全数据源）
  fetchCtxBreakdown(): void // 上下文构成分区（Composer ctx 环 hover 面板）
  refreshExternalSkillsStatus(): Promise<void>
  importExternalSkills(): Promise<boolean>
  toggleSkill(name: string, enabled: boolean): void // 启用/禁用（真过滤：发现清单 + load_skill）
  getSkill(name: string): void // 技能详情预览（skill_get）
  closeSkill(): void
  installSkill(url: string, scope: string, name?: string): void // 远程技能下载（skill_install）；name 可选 = 市场浏览后精确安装单个
  browseMarket(url: string): void // 浏览远程市场技能（skill_browse，只读；结果落 marketSkills）
  refreshMarket(url: string): void // 刷新当前浏览的市场（重拉 + 对比本地；安装/更新后调）
  updateSkill(name: string): void // 更新远程技能（skill_update）
  uninstallSkill(name: string): void // 卸载远程技能（skill_uninstall）
  fetchMarkets(): void // 已保存市场（market_list；SkillsPage 市场管理）
  addMarket(url: string): void // 保存市场（market_add，幂等）
  removeMarket(url: string): void // 移除市场（market_remove）
  updateMarketSkills(url: string): void // 按市场更新该市场全部已装技能（skill_update_market）
  fetchMCPServers(): void // MCP 服务器清单 + 状态（mcp_list；SettingsModal MCP tab）
  saveMCP(servers: Record<string, MCPServerCfg>): Promise<{ ok: boolean; error?: string }> // 持久化（mcpSave）+ 热应用（mcp_set）
  refreshMCP(name: string): void // 强制重连单个服务器（mcp_refresh）
  // 项目级 MCP（MCP 页「项目」区）：按前端项目列表汇总 / 保存某项目的项目层配置
  fetchMCPProjects(workspaces: string[]): void // 多项目汇总（mcp_projects；不创建运行态）
  saveMCPProject(workspace: string, servers: Record<string, MCPServerCfg>): Promise<{ ok: boolean; error?: string }> // 写 {ws}/.go-code/settings.json（mcp_project_set）
  // hooks（钩子页）：来源汇总 / 保存某来源 / 信任项目命令
  fetchHooks(workspaces: string[]): void // hooks_list
  saveHooks(scope: 'user' | 'project', workspace: string | undefined, body: HookSectionView): Promise<{ ok: boolean; error?: string }> // hooks_set
  trustHooks(commands: string[], scope: string): Promise<{ ok: boolean; error?: string }> // hooks_trust
  fetchPlugins(): void // 插件清单 + 状态（plugin_list；SettingsModal 插件 tab）
  installPlugin(id: string): void // 受管安装组件（plugin_install）
  uninstallPlugin(id: string): void // 移除组件（plugin_uninstall）
  enablePlugin(id: string): void // 启用 → 工具注册进引擎（plugin_enable）
  disablePlugin(id: string): void // 禁用（plugin_disable）
  fetchBrowserEngine(): void // 查询浏览器引擎（browser_engine_get；ego 状态一并返回）
  setBrowserEngine(engine: 'mcp' | 'ego'): Promise<{ ok: boolean; error?: string }> // 切换引擎（落盘 + browser_engine_set 热应用）
  refreshEgoStatus(): void // 强制重新检测 ego lite（忽略 bridge 检测缓存；安装完成后调，立即落持久化记录）
  fetchIMConfig(): void // IM 配置 + Schema + 状态（im_config_get/im_schema_list/im_status；SettingsModal IM tab）
  saveIMConfig(cfg: IMConfig): Promise<{ ok: boolean; error?: string }> // 保存并热应用（im_config_set）
  imBindConfirm(chat: IMChat, approve: boolean, workspace: string): void // 绑定确认决策（im_bind_confirm）
  fetchIMChats(gatewayId: string): void // 群列表（im_chat_list；路由绑定下拉）
  fetchBrowserConfig(): void // 浏览器插件配置（browser_config_get；插件详情设置表单）
  startEmbedEndpoint(): void // M3：起内嵌端点（后台、不可见；enable 时）
  attachEmbedView(debuggerAddr: string): void // M3：把内嵌视图 attach 到端点（真正使用时）
  saveBrowserConfig(cfg: BrowserCfg): Promise<{ ok: boolean; error?: string }> // 保存并热应用（browser_config_set）
  fetchBrowserPages(): void // 浏览器打开页面（browser_pages；web tab）
  fetchBrowserShot(pageId?: number): void // 页面截图（browser_screenshot；web tab）
  closeBrowserPage(pageId: number): void // 关闭指定网页（browser_close；web tab 页列表 ✕）
  shutdown(): void

  toggleLeft(): void
  toggleRight(): void
  setLeftExpanded(b: boolean): void // Panel onCollapse/onExpand 同步
  setRightExpanded(b: boolean): void
  togglePin(): void
  setRightTab(t: RightTab): void
  // —— 右侧栏标签（阶段 1，docs/RIGHT_PANEL_TABS_PLAN.md）——
  openTab(spec: TabSpec, opts?: { activate?: boolean }): string // 打开或激活（阶段 1 幂等键 = kind）；返回标签 id
  activateTab(id: string): void // 激活指定标签（切 scope 内的显示）
  closeTab(id: string): void // 关闭（激活项顺延到右邻居，无则左邻居）
  reopenTab(id?: string): void // 重开最近关闭的一个（给 id 则重开指定的那个）
  syncTabsToRightTab(): void // 标签集单向收敛到 rightTab（组件 effect 调用；已一致时不 set）
  // 阶段 5：把防抖窗口里的标签集立即落盘（e2e/L1 断言与关窗前收尾用；正常路径靠订阅防抖写）。
  flushSessionTabs(): void
  // 内嵌页面 → 面板级 web 标签（阶段 1.5：一页一标签，浏览器内层标签条退役）。
  // source 决定 id 空间（native='tN' / mcp=数字 pageId）与「哪一方权威」：有数据的一方权威。
  // srcPath（阶段 5）：预览的源文件路径 —— 恢复出来的待重建标签按它**认领**重建出来的视图
  //（否则会多出一个新标签，恢复的那个成了永远点不开的残留）。
  syncWebTabs(source: 'native' | 'mcp', items: { key: string; url: string; title: string; srcPath?: string }[], activeKey: string | null): void
  setRightSize(n: number | null): void // M3：期望的右侧面板宽度（浏览器内嵌按 viewport 设）
  focusBrowser(): void // M3：使用 Browser Use 时聚焦右侧 web tab（收起左栏 + 展开右栏 + 切网页 tab）
  focusBrowserForUse(fromSid?: string): void // §6.9：Browser 动作时自动打开右栏（已打开不重复；收起后重开）。2026-09-20：只在本会话触发（fromSid ≠ 当前会话 → 不动视线）
  focusBrowserForPreview(): void // 预览本地文件：只展开右栏 + 切 web tab，**不动** leftExpanded
  // §3.1：预览统一入口 —— 没开过 → 新建标签；**打开过 → 刷新那个标签**（不新建）。
  // opts.srcPath = 源文件路径（office 转换产物必须显式给：产物 URL 每次不同，去重靠源文件）；
  // 不给则源就是 path 本身。activate 见其注释。
  // 实现是 async（内部要查视图池/等 IPC），故声明为 Promise<void> —— 阶段 4 起
  // autoOpenVisualFile 需要 await 它（未读点在该 Promise 落地后才写进 unreadViews）。
  openInBrowserTab(path: string, opts?: { activate?: boolean; srcPath?: string; quiet?: boolean }): Promise<void>
  openHtmlInBrowser(path: string): void // write_file 落盘 .html → 右侧内嵌浏览器 file:// 打开（= openInBrowserTab activate:true）
  openHtmlContentInBrowser(rawHtml: string): void // 纯展示 ```html（未落盘）→ 右侧内嵌浏览器 data URL 展示
  previewXlsx(path: string): void // xlsx/xls → bridge 转 HTML（受管 Python + openpyxl）→ 内嵌浏览器 file:// 预览
  previewDocx(path: string): void // docx/docm → bridge 转 HTML（受管 Python + mammoth）→ 同一条内嵌浏览器
  previewPptx(path: string): void // pptx/pptm → bridge 转 HTML（受管 Python + python-pptx 近似版式）→ 同上
  previewOfficeFile(path: string, kind: 'xlsx' | 'docx' | 'pptx', opts?: { activate?: boolean; quiet?: boolean }): Promise<void> // 三者共用的实现（见其长注释）；opts.activate 默认 true；Agent 自动产出传 activate:false + quiet:true（阶段 4：后台加标签 + 未读点，不激活不弹错）
  openSkills(): void
  closeSkills(): void
  openCron(): void // 定时任务管理页（左栏入口；进入时拉取 cron_list）
  closeCron(): void
  openCronRuns(job: CronJob): void // 点击任务 → 全新「执行历史」页（拉取该任务 cron_runs）
  closeCronRuns(): void // 从执行历史页返回任务列表
  fetchCron(): void // 拉取 cron_list → 更新 jobs + enabled/auto_clean
  fetchCronRuns(jobId?: string): void // 拉取运行账本（cron_runs）
  fetchCronRunDetail(jobId: string, runId: string): void // 拉取某次执行详情（cron_runs_detail）
  // —— 链路 tab：trace ——
  fetchTraces(sid?: string, limit?: number): void // 拉历史 trace 摘要列表（顶部选择器；limit 用于「加载更多」）
  selectTrace(id: string | undefined, sid?: string): void // 选中某条 trace（undefined = 跟随最新）
  getTraceEvents(sid?: string): AnyEvent[] // 取该会话的 span 类事件流（实时投影数据源）
  clearCronRunDetail(): void // 关闭详情页时清空
  setCronDetail(open: boolean): void // 详情子页开关
  deleteCron(id: string): void // 删除任务（cron_delete；cron_changed 后自动刷新）
  updateCron(id: string, patch: { prompt?: string; cron?: string; recurring?: boolean; paused?: boolean }): void // 编辑任务（cron_update；paused=true 暂停 / false 恢复）
  setCronEnabled(on: boolean): void // 定时任务开关（乐观 + 持久化 + bridge 重建：加载/卸载 cron 工具）
  setCronAutoClean(on: boolean): void // 自动清理开关（乐观 + 持久化）
  setMeshSessionEnabled(on: boolean): void // Session Mesh C1 开关（乐观 + 持久化 + bridge 热重载：加载/卸载 session_* 工具）
  setMeshRemoteEnabled(on: boolean): void // Session Mesh C2 远程开关（依赖 C1；乐观 + 持久化 + 网关启停）
  setMeshRemoteCfg(remote: NonNullable<MeshSettings['remote']>): void // 远程网关配置（listen/peers/token；保存即热重载）
  fetchMeshStatus(): void // 查询 mesh 运行状态（node_id / connect_urls / links / peers / tls）
  pingMeshNode(nodeId: string): Promise<{ ok: boolean; rttMs?: number; error?: string }> // 主动 ping 节点（mesh_ping；连接检测）
  setStopBackgroundOnInterrupt(on: boolean): void // 主停止是否连后台任务一起停（乐观 + 持久化）
  setGoalAlignmentRounds(n: number): void
  setSubagentConcurrency(patch: Partial<Pick<AgentSettings, 'max_subagents' | 'max_explore_subagents'>>): void // 子 agent 并发槽上限（乐观 + 持久化 + bridge 热更新）
  setSTTSettings(patch: Partial<STTSettings>): void // 语音输入（话筒）配置（乐观 + 持久化）
  openNewFolder(): void // 「新建文件夹…」：打开创建文件夹 modal
  closeNewFolder(): void
  loadSettings(): Promise<void> // boot 水合全局设置（settings:get）
  openSettings(): void
  closeSettings(): void
  toast: ToastState | null // 全局 Toast（保存成功/失败等轻提示；自动消失）
  showToast(msg: string, kind?: 'ok' | 'error'): void // 弹全局 Toast（覆盖旧 Toast；~2.5s 自动消失）
  setTheme(theme: 'dark' | 'light' | 'system'): void // 乐观 + 持久化 + Electron 窗口底色跟随
  setNarrativeSkin(skin: 'current' | 'v3'): void // 会话页样式（乐观 + 持久化；实际视觉由 App.tsx 的 effect 写 data-skin）
  setLang(lang: 'zh' | 'en'): void // 乐观 + 持久化（i18n 立即重渲染）
  setSandboxMode(mode: 'none' | 'seatbelt'): void // 命令沙箱模式（乐观 + 持久化；bridge loop 重建时生效）
  computerApprovedSave(apps: string[]): void // Computer Use 已授权 App（统一授权；乐观 + 持久化 + 热生效）
  pickAppFromSystem(): Promise<string | null> // 系统「选择应用程序」对话框 → 返回所选 App 显示名（取消 → null）
  providerSave(p: ProviderSaveInput): Promise<boolean> // 持久化 provider 配置 + key（热切换由调用方另发 set_provider）
  addProvider(p: { provider: string; base_url: string; protocol?: string; openai_compat?: import('../transport/types').OpenAICompatCapabilities; api_key?: string }): Promise<{ ok: boolean; error?: string }> // 新增自定义 provider（创建配置 + 存 key + 通知 bridge）
  renameProvider(oldName: string, newName: string): Promise<{ ok: boolean; error?: string }> // 自定义 provider 改名（整卡配置迁移到新 id：base_url/key/models/…；内置 5 家不可改名）
  removeProvider(provider: string): Promise<{ ok: boolean; error?: string }> // 删除自定义 provider（配置 + key + 重建）
  refreshProviderKeys(): void // 各 provider 是否已设 key（仅布尔，Provider 面板「已设置 ✓」）
  fetchModels(provider: string): void // 拉取 provider 模型列表（list_models）
  // 从实时价源补价（refresh_prices → models.dev / openrouter）；返回补价汇总（失败 undefined）。
  // 默认**覆盖**已有价（用户 2026-09-21 决策：补价即覆盖）；overwrite:false 才走保守路径（只补无价模型）
  refreshPrices(p: { provider: string; source?: string; overwrite?: boolean }): Promise<PriceRefreshSummary | undefined>
  fetchProviderPresets(): void // 拉取内置 Provider 预设（list_provider_presets ← providers.json）
  testProvider(base_url: string, api_key: string): Promise<{ ok: boolean; error?: string; count?: number }> // 测试连接（test_connection；瞬态探活，不改配置）
  saveProvider(p: ProviderSaveInput): Promise<boolean> // 持久化（providerSave）+ 热切换（set_provider 命令）；返回是否成功（Toast 用）
  toggleModelWindow(provider: string, model: string, oneM: boolean): void // 标 1M → 窗口 1000000；否则回 128000
  setModelWindow(provider: string, model: string, window: number): void // 每模型上下文窗口（contextWindow，数值）
  setModelMaxTokens(provider: string, model: string, tokens: number): void // 每模型单次输出上限（max_tokens）
  toggleModelImage(provider: string, model: string, on: boolean): void // 每模型输入类型（input_types）：勾选图片 = 多模态（text+image）
  // —— 模型清单编辑（2026-09）：改名/删除/手动添加（models/max_tokens 同 key 迁移）——
  addModel(provider: string, model: string, window?: number, maxTokens?: number): Promise<{ ok: boolean; error?: string }> // 手动添加模型（未拉取列表的本地/代理端点用）；重名拒绝
  renameModel(provider: string, oldName: string, newName: string): Promise<{ ok: boolean; error?: string }> // 模型改名（models/max_tokens/input_types/prices 同 key 迁移）
  removeModel(provider: string, model: string): void // 删除模型（models/max_tokens/input_types/prices 同 key 清除）
  // —— 每模型价表覆盖（USD/1M tokens；缺省用内置注册表价表）——
  setModelPrices(provider: string, model: string, prices: import('../transport/types').ModelPrices): void
  clearModelPrices(provider: string, model: string): void // 清除价表覆盖（回退内置注册表价）
  // —— OAuth 订阅登录（2026-08 P2）——
  oauthLogin(provider: string): Promise<{ ok: boolean; error?: string }> // 发起 OAuth 登录（bridge oauth_login；事件经 onEvent 通道回前端）
  oauthLogout(provider: string): Promise<{ ok: boolean; error?: string }> // OAuth 登出（删凭证 + 回退 api_key + 重建）
  refreshOAuthStatus(provider: string): Promise<void> // 刷新某 provider OAuth 状态（oauth_status）
  refreshAllOAuthStatus(): void // 刷新全部 provider OAuth 状态
  oauthPromptAnswer(provider: string, value: string, requestId?: string): Promise<{ ok: boolean; error?: string }> // 登录卡片输入回填 bridge（request_id 精确路由）
}

const transport = getTransport()

// __traceSpy：测试专用（Node 回归脚本 scripts/test-trace.mjs）—— 记录实际发出的命令，
// 用于断言 trace 命令的 payload（如「加载更多」必须显式传 limit）。
// 生产恒为 null（不推入任何东西），零开销。
export const __traceSpy: { sent: unknown[] | null } = { sent: null }
const _traceOrigSend = transport.send.bind(transport)
transport.send = (cmd: BridgeCommand) => {
  if (__traceSpy.sent) __traceSpy.sent.push(cmd)
  return _traceOrigSend(cmd)
}

interface PendingCommand {
  type: string
  workspace?: string
  sessionId?: string
  navigationGeneration: number
  // file_preview 的请求路径（阶段 2）：响应只带 path、错误响应连 path 都没有，而在途登记
  // 必须精确释放 —— 记在这里，成功/失败两条分支都能按**发起时**的键释放（见 filePreviewInFlight）。
  path?: string
}

const pending: Record<number, PendingCommand> = {}
// interrupt_task 在途请求（命令 id → 目标任务）：失败时按 id 回滚乐观 interrupting 态。
const interruptPending = new Map<number, { sid: string; taskId: string }>()
// promote_task 在途请求（命令 id → 目标任务）：乐观置位后若命令失败，按 id 回滚
// promoted 态并给出行内提示。mock 命令响应是同步发出 → 必须先登记再 send。
const promotePending = new Map<number, { sid: string; taskId: string }>()
// 单调导航代际：只有最后一次 workspace/session 导航请求可以改写 activeSessionId 和顶层视图。
let navigationGeneration = 0
// command 命令的原始行（line），供命令响应按内容分流（如 /reload_skills 成功后刷新技能清单）。
const pendingLines: Record<number, string> = {}

// browser 面板拉取守卫：fetch 置 browserBusy=true 后，若 N 秒内既无成功响应也无错误响应
//（请求丢失 / 切会话后失效 / 截图挂起），强制回落 busy=false。否则 web tab 会永久卡在
//「拉取中…」，且后续 fetch 都被 `!browserBusy` 挡住永不重试（跨会话残留的全局 busy 即此成因）。
let browserBusyTimer: ReturnType<typeof setTimeout> | undefined
// §6.2 请求代际：workspace/session 切换时递增；迟到响应据此丢弃（不覆盖当前状态）。
let browserGeneration = 0
// 每个请求的（generation, type），响应消费时校验（防跨 workspace/page 串写）。
const browserInFlight = new Map<number, { gen: number; kind: 'pages' | 'screenshot' | 'close'; pageId?: number }>()
const browserRequestSeq = 0
function armBrowserBusyWatchdog(ms = 10000): void {
  if (browserBusyTimer) clearTimeout(browserBusyTimer)
  browserBusyTimer = setTimeout(() => {
    browserBusyTimer = undefined
    if (useAppStore.getState().browserBusy) useAppStore.setState({ browserBusy: false })
  }, ms)
}
function disarmBrowserBusyWatchdog(): void {
  if (browserBusyTimer) {
    clearTimeout(browserBusyTimer)
    browserBusyTimer = undefined
  }
}

// 工作区级 git_history「加载更多」意图标记（请求前置位，响应消费后复位）由 wsHistAppend 承担；
// Git 写操作统一入口：无 workspace / 已有在途操作时忽略；busy 标识驱动按钮禁用与 spinner。
function gitWriteOp(type: 'git_stage' | 'git_unstage' | 'git_discard' | 'git_commit' | 'git_checkout' | 'git_init', payload: Record<string, unknown>): void {
  const st = useAppStore.getState()
  if (!st.workspace || st.gitOpBusy) return
  useAppStore.setState({ gitOpBusy: type, gitOpError: undefined })
  send({ type, payload })
}

// 所有命令注入 workspace：单进程多 workspace 下，bridge 靠 payload.workspace 路由会话。
// payload 显式带 workspace（如恢复时水合非活跃项目）→ 优先它；否则注入当前活跃项目。
// 内置斜杠命令（与 Composer 补全共用同一份定义）：/技能名 走 skills 清单动态判定
export const BUILTIN_SLASH = ['/summary', '/clear', '/reload_skills']

// onResp：命令响应回调（可选）。**必须在 transport.send 之前登记**——mock transport 是
// 同步回响应（真 bridge 异步），调用方若「先 send 再 onCommandResponseOnce」会漏掉响应
// （表现为 10s 超时失败）。与下面 im_chat_list 先登记 id→gateway_id 同因。
function send(cmd: Omit<BridgeCommand, 'id'>, navigation = false, onResp?: (r: { ok: boolean; error?: string }) => void): number {
  const id = ++cmdSeq
  const ws = String(cmd.payload?.workspace || useAppStore.getState().workspace || '')
  const sessionId = String(cmd.payload?.session_id || cmd.payload?.id || '')
  pending[id] = {
    type: cmd.type,
    workspace: ws || undefined,
    sessionId: sessionId || undefined,
    navigationGeneration: navigation ? ++navigationGeneration : navigationGeneration,
    ...(cmd.type === 'file_preview' ? { path: String(cmd.payload?.path ?? '') } : {}),
  }
  // 恢复协议标识（问题四）：new_session 带 restore_id + attempt；响应回显，
  // renderer 据此丢弃陈旧响应（迟到的旧请求响应不得覆盖新会话）。
  let payload = cmd.payload
  if (cmd.type === 'new_session') {
    // 新建会话（无 id = 真新建；带 id = 恢复已有会话）→ 注入当前活跃 Provider，
    // bridge 以 UI 选择的 Provider 归属会话（2026-09：多 Provider 同名模型时避免
    // 会话被 bridge 按模型名猜错 Provider，如 glm-5.3-flash 同时存在于 opencode/zhipu）。
    if (!sessionId) {
      const active = useAppStore.getState().settings.provider.active
      if (active) payload = { ...(payload || {}), provider: active }
    }
    const rid = `r-${id}-${Date.now()}`
    const attempt = (pendingRestoreAttempt[sessionId || ''] ?? 0) + 1
    pendingRestoreAttempt[sessionId || ''] = attempt
    payload = { ...(payload || {}), restore_id: rid, attempt }
  }
  // transport mock 可同步回响应；必须在 send 前登记恢复请求，避免响应先于调用方 bookkeeping。
  if (cmd.type === 'new_session' && sessionId) {
    snapshotPending.set(sessionId, id)
    armSnapshotWatchdog(sessionId, id) // 响应丢失兜底：超时清 pending + preBuf 转正常 dispatch
  }
  if (cmd.type === 'command') pendingLines[id] = String((cmd.payload as { line?: unknown })?.line ?? '')
  // im_chat_list：先登记 id→gateway_id 再发（mock 同步响应——必须在 transport.send 前登记）
  if (cmd.type === 'im_chat_list') {
    const gid = String((cmd.payload as { gateway_id?: unknown })?.gateway_id ?? '')
    if (gid) imChatReq.set(id, gid)
  }
  const finalPayload = ws ? { ...(payload || {}), workspace: ws } : payload
  if (onResp) onCommandResponseOnce(id, onResp)
  void transport.send({ ...cmd, id, payload: finalPayload })
  return id
}

// onCommandResponseOnce 注册一次性 command_response 回调（按命令 id 精确匹配）。
// 用于需要知道命令结果的动作（如 session_pin 失败回滚）；超时 10s 自动释放。
function onCommandResponseOnce(id: number, cb: (r: { ok: boolean; error?: string }) => void): void {
  let timer: ReturnType<typeof setTimeout> | undefined
  const off = transport.onEvent((line) => {
    try {
      const ev = JSON.parse(line) as { event_type?: string; id?: number; ok?: boolean; error?: string }
      if (ev.event_type !== 'command_response' || ev.id !== id) return
      off()
      if (timer) clearTimeout(timer)
      cb({ ok: ev.ok === true, error: ev.error })
    } catch {
      /* 非 JSON 事件忽略 */
    }
  })
  timer = setTimeout(() => {
    off()
    // 超时：bridge 可能已处理、也可能未处理——不能伪报成功（V2 P1-PROTOCOL-02）。
    // 返回失败让调用方回滚乐观状态；需要确认的调用方可重拉服务端状态。
    cb({ ok: false, error: 'bridge 响应超时' })
  }, 10_000)
}

// onCommandResponseData 一次性 command_response 回调（带完整 data；如 mesh_status）。
// 与 onCommandResponseOnce 同构，但回传 data 供展示类查询用；超时 10s 释放。
function onCommandResponseData(id: number, cb: (r: { ok: boolean; error?: string; data?: unknown }) => void): void {
  let timer: ReturnType<typeof setTimeout> | undefined
  const off = transport.onEvent((line) => {
    try {
      const ev = JSON.parse(line) as { event_type?: string; id?: number; ok?: boolean; error?: string; data?: unknown }
      if (ev.event_type !== 'command_response' || ev.id !== id) return
      off()
      if (timer) clearTimeout(timer)
      cb({ ok: ev.ok === true, error: ev.error, data: ev.data })
    } catch {
      /* 非 JSON 事件忽略 */
    }
  })
  timer = setTimeout(() => {
    off()
    cb({ ok: false, error: 'bridge 响应超时' })
  }, 10_000)
}
// pendingRestoreAttempt：会话 → 最近一次 new_session 的 attempt 序号（陈旧响应丢弃用）。
const pendingRestoreAttempt: Record<string, number> = {}
// imChatReq：im_chat_list 请求 id → gateway_id（响应分发时定位群列表归属）。
const imChatReq = new Map<number, string>()

// 把某工作区会话列表并入 projects；若该工作区是当前活跃项目，同步 sessions 快照
function applySessions(s: AppState, wsPath: string, list: SessionMeta[]): { projects: Project[]; sessions: SessionMeta[] } {
  const sessions = sortSessions(normalizeSessions(list))
  const projects = s.projects.some((p) => p.path === wsPath)
    ? s.projects.map((p) => (p.path === wsPath ? { ...p, sessions } : p))
    : [...s.projects, { path: wsPath, sessions }]
  return { projects: orderProjects(projects), sessions: wsPath === s.workspace ? sessions : s.sessions }
}

// bridge 会话条目归一化：updated_at(unix ms) → updatedAt；缺 id/title 兜底。
function normalizeSessions(list: SessionMeta[]): SessionMeta[] {
  return (list ?? []).map((x) => {
    const o = (x ?? {}) as unknown as Record<string, unknown>
    return {
      id: String(o.id ?? ''),
      title: String(o.title ?? o.id ?? ''),
      updatedAt: typeof o.updated_at === 'number' ? Number(o.updated_at) : (typeof o.updatedAt === 'number' ? Number(o.updatedAt) : undefined),
      pinned: o.pinned === true,
    }
  })
}

// 会话排序：bridge 已排好则幂等；缺失 updatedAt 的旧数据按 id 降序（genID 单调 ≈ 创建逆序）。
function sortSessions(list: SessionMeta[]): SessionMeta[] {
  return [...list].sort((a, b) => {
    const ta = a.updatedAt ?? 0
    const tb = b.updatedAt ?? 0
    if (ta !== tb) return tb - ta
    return a.id === b.id ? 0 : a.id < b.id ? 1 : -1 // 字符串降序（stable 兜底）
  })
}

// 运行中的子 agent 块数量（agent_start/end 与快照归一化时维护 runningAgentCount 用）
export function countRunningAgents(blocks: MsgBlock[]): number {
  let n = 0
  for (const b of blocks) if (b.kind === 'agent' && (b.status === 'running' || b.status === 'interrupting')) n++
  return n
}

// 由 views 全表导出 busy 会话集合（仅在运行/工具状态转移时调用，代价 O(views×runNodes)）
function busySidsFromViews(views: Record<string, SessionView> | undefined): ReadonlySet<string> {
  const out = new Set<string>()
  if (!views) return out
  for (const [sid, v] of Object.entries(views)) if (sessionBusy(v)) out.add(sid)
  return out
}

// 事件路由目标会话（SDK 事件带 session_id；缺省回退活跃会话，避免建空 '' view）
function getSid(raw: AnyEvent, fallback?: string): string {
  const sid = typeof raw.session_id === 'string' && raw.session_id ? raw.session_id : ''
  return sid || fallback || ''
}

// 事件时间戳（RFC3339Nano 字符串或毫秒数）：所有耗时计算以事件自带 timestamp 为准
//（写入事件日志 → 重放可原样重建；不依赖客户端 Date.now()——重放时客户端时钟是「此刻」，
// 逐条流式重放会得到全 0 耗时）。缺失/非时间戳回退 undefined（调用方兜底 Date.now()）。
function evTs(raw: AnyEvent): number | undefined {
  const v = (raw as { timestamp?: unknown }).timestamp
  if (typeof v === 'number') return v
  if (typeof v === 'string') {
    const t = Date.parse(v)
    return Number.isFinite(t) ? t : undefined
  }
  return undefined
}

// finish_reason → 状态映射。关键：max_iterations（达轮数护栏被截断）≠ done，
// 需以 interrupted 暴露「任务未完成」，否则截断会伪装成正常完成、用户只能盲输「继续」。
// error → error；max_iterations → interrupted；其余（stop 等自然结束）→ done。
function finishToStatus(fr: unknown): 'done' | 'error' | 'interrupted' {
  if (fr === 'error') return 'error'
  if (fr === 'abort') return 'interrupted' // 用户中断：非失败但未完成 → trace/面板显示 ⏸ 未完成
  if (fr === 'max_iterations') return 'interrupted'
  return 'done'
}

// 后端新状态统一为六态；同时接受旧事件/快照中的 done/error/aborted 等值。
function asyncTaskStatus(status: unknown, error?: unknown, fallback: AsyncTaskStatus = 'completed'): AsyncTaskStatus {
  const value = String(status || '').toLowerCase()
  if (value === 'running') return 'running'
  if (value === 'interrupting') return 'interrupting'
  if (value === 'completed' || value === 'done' || value === 'success' || value === 'succeeded') return 'completed'
  if (value === 'interrupted' || value === 'aborted' || value === 'canceled' || value === 'cancelled') return 'interrupted'
  if (value === 'failed' || value === 'error') return 'failed'
  if (value === 'abandoned') return 'abandoned'
  return error ? 'failed' : fallback
}

function finishToAsyncTaskStatus(fr: unknown): AsyncTaskStatus {
  if (fr === 'error') return 'failed'
  if (fr === 'abort' || fr === 'max_iterations') return 'interrupted'
  return 'completed'
}

function terminalTaskStatus(status: unknown, error?: unknown, fallback: AsyncTaskStatus = 'completed'): TerminalTaskStatus {
  const normalized = asyncTaskStatus(status, error, fallback)
  // delivery 按协议应只携终态；防御异常 running/interrupting，避免通知谎称仍在执行。
  return normalized === 'running' || normalized === 'interrupting' ? (error ? 'failed' : 'completed') : normalized
}

// taskUsageFromEvent 后台任务事件携带的 usage → 卡片用量（含成本 USD）。
// 事件形态是 core.Usage（大写字段名 + Cost.Total µUSD），与 llm_end 的单轮 usage 不同：
// 这是**任务全量**（含子 agent 的压缩轮——压缩走私有 provider 流、不发 llm_end，
// 因此按轮累加的近似值会少算，终态事件的值才是权威）。全零/缺失 → undefined，
// 不给不花模型钱的任务（普通后台工具）留空白用量位。
function taskUsageFromEvent(u?: RawUsage): UsageAgg | undefined {
  if (!u) return undefined
  const cost = (u.Cost?.Total ?? 0) / 1e6
  const agg: UsageAgg = {
    input: u.Input ?? 0,
    output: u.Output ?? 0,
    cacheRead: u.CacheRead ?? 0,
    cacheWrite: u.CacheWrite ?? 0,
    cacheWrite1h: u.CacheWrite1h ?? 0,
    reasoning: u.Reasoning ?? 0,
    ...(cost > 0 ? { costUsd: cost } : {}),
  }
  return agg.input || agg.output || agg.cacheRead || agg.cacheWrite || agg.reasoning || cost > 0 ? agg : undefined
}

// applyTaskUsage 把任务终态用量挂到匹配的卡片（agent / async_task / promoted tool）。
// 覆盖而非累加：TaskEnd 与 task_result_delivered 同源同值（重复投递幂等），
// 且终态值是任务全量（覆盖运行中按轮累加的近似值）。
function applyTaskUsage(blocks: MsgBlock[], taskId: string, usage?: UsageAgg): MsgBlock[] {
  if (!taskId || !usage) return blocks
  let hit = false
  const out = blocks.map((b) => {
    if ((b.kind === 'agent' || b.kind === 'async_task' || b.kind === 'tool') && b.taskId === taskId) {
      hit = true
      return { ...b, usage }
    }
    return b
  })
  return hit ? out : blocks
}

// 运行中的异步任务数（子 agent 运行中 + 已 promote 为后台的工具任务运行中）。
// 普通 in-loop 工具调用不在此列（它们在主对话流里展示，不算异步任务）。
// Statusbar 底部统计与右侧「异步任务」面板共用。
export function runningTaskCount(blocks: MsgBlock[]): number {
  const ids = new Set<string>()
  let anonymous = 0
  for (const b of blocks) {
    if (b.kind === 'async_task' && (b.status === 'running' || b.status === 'interrupting')) ids.add(b.taskId)
    else if (b.kind === 'agent' && (b.status === 'running' || b.status === 'interrupting')) {
      if (b.taskId) ids.add(b.taskId)
      else anonymous++
    } else if (b.kind === 'tool' && b.promoted && (b.status === 'running' || b.interrupting)) {
      if (b.taskId) ids.add(b.taskId)
      else anonymous++
    }
  }
  return ids.size + anonymous
}

// rollbackInterrupting interrupt_task 失败时按 id 回滚乐观 interrupting 态：
// 仅回滚仍在「中断中」的任务（已收敛终态/已完成的不受影响——事件先于响应到达的竞态安全）。
function rollbackInterrupting(sid: string, taskId: string): void {
  useAppStore.setState((s) => {
    const v = s.views[sid] ?? emptyView()
    const blocks = v.blocks.map((b) => {
      if (!(b.kind === 'tool' || b.kind === 'agent' || b.kind === 'async_task') || b.taskId !== taskId) return b
      if (b.kind === 'tool') {
        // 仅回滚仍在运行的任务块：已收敛 done 的块（tool_run_end 先于失败响应到达）
        // 不能被复活成 running —— 否则 taskStatus='running' + status='done' 又会卡出
        // 「执行中空转计时器、角标却不计数」的矛盾态。
        return b.interrupting && b.status === 'running'
          ? { ...b, interrupting: false, taskStatus: 'running' as const }
          : b
      }
      if (b.kind === 'agent' || b.kind === 'async_task') {
        return b.status === 'interrupting'
          ? { ...b, status: 'running' as const }
          : b
      }
      return b
    })
    return setView(s, sid, { ...v, blocks })
  })
}

// rollbackPromote promote_task 失败时按 id 回滚乐观 promoted 态（问题 1 静默失败修复）：
// 仅回滚「已被乐观置为后台、但仍在主 run 内运行」的工具块——task_promoted 事件先到的
//（真成功）或工具已收敛终态的块均不受影响（竞态安全/幂等）。失败细节以 system(error)
// 块落进对话流，替代全局 bridgeError 状态栏一闪而过（用户「点了没反应」的根因之一）。
function rollbackPromote(sid: string, taskId: string, error?: string): void {
  useAppStore.setState((s) => {
    const v = s.views[sid] ?? emptyView()
    let rolledBack = false
    const blocks = v.blocks.map((b) => {
      if (b.kind !== 'tool' || b.taskId !== taskId) return b
      // 乐观置位后的形态：promoted=true + status='running' 且无 interrupting；
      // task_promoted 事件已到（真成功）→ 此分支不会命中失败回滚（响应 ok 才会清登记）。
      if (b.promoted && b.status === 'running' && !b.interrupting) {
        rolledBack = true
        return { ...b, promoted: false }
      }
      return b
    })
    const notice = `${i18nT('tool.promote.fail')}${error ? `：${error}` : ''}`
    const withNotice: MsgBlock[] = rolledBack
      ? [...blocks, { kind: 'system' as const, id: blockSeq++, text: notice, tone: 'error' as const }]
      : blocks // 极端竞态：块已不存在/已终态 → 不回滚但仍然提示失败原因
    return setView(s, sid, { ...v, blocks: withNotice })
  })
}

// 异步任务完整清单。task_started 会先建立 async_task 占位，agent_start 到达后会把同一
// taskId 合并进 agent 卡片；因此通常不会重复，异常/延迟事件下仍能展示占位和数量徽章。
export function asyncTaskBlocks(blocks: MsgBlock[]): Extract<MsgBlock, { kind: 'async_task' } | { kind: 'agent' } | { kind: 'tool' }>[] {
  const out: Extract<MsgBlock, { kind: 'async_task' } | { kind: 'agent' } | { kind: 'tool' }>[] = []
  for (const b of blocks) {
    if (b.kind === 'async_task' || b.kind === 'agent') out.push(b)
    else if (b.kind === 'tool' && b.promoted) out.push(b)
  }
  return out
}

// view 级辅助：开新 assistant 块 / 流式追加（llmStartTs 用 llm_start 事件 timestamp）
function pushAssistant(v: SessionView, model?: string, requestId?: string, startTs?: number, runId?: string): SessionView {
  const id = blockSeq++
  const reqBlock = { ...v.reqBlock }
  if (requestId) reqBlock[requestId] = id
  const ts = startTs ?? Date.now()
  return {
    ...v,
    blocks: [...v.blocks, { kind: 'assistant', id, text: '', thinking: '', streaming: true, model, runId, ts }],
    reqBlock,
    lastLLMBlockId: id,
    llmStartTs: ts,
    ttftMs: undefined,
    streaming: true,
  }
}

// 追加流式文本/思考到目标 assistant 块；目标缺失时兜底最后一条 assistant（无 request 的旧流）
function appendToAssistant(v: SessionView, bid: number | undefined, field: 'text' | 'thinking', c: string, thinkStart?: number): SessionView {
  const blocks = [...v.blocks]
  let idx = bid != null ? blocks.findIndex((b) => b.id === bid && b.kind === 'assistant') : -1
  if (idx < 0) {
    const last = blocks[blocks.length - 1]
    idx = last && last.kind === 'assistant' ? blocks.length - 1 : -1
  }
  let lastLLMBlockId = v.lastLLMBlockId
  if (idx >= 0) {
    const b = blocks[idx] as Extract<MsgBlock, { kind: 'assistant' }>
    // 思考首块：挂思考起始时刻（实时计时器起点；已挂不覆盖）
    const thinkStartAttach = field === 'thinking' && thinkStart != null && b.thinkStart == null ? { thinkStart } : {}
    blocks[idx] = { ...b, [field]: b[field] + c, ...thinkStartAttach }
  } else {
    const id = blockSeq++
    lastLLMBlockId = id
    blocks.push({ kind: 'assistant', id, text: field === 'text' ? c : '', thinking: field === 'thinking' ? c : '', streaming: true, ts: Date.now(), ...(field === 'thinking' && thinkStart != null ? { thinkStart } : {}) })
  }
  return { ...v, blocks, lastLLMBlockId }
}

// 子 agent 流式追加：思考/文本归并到其卡片活动记录（同类型连续追加，跨轮/跨工具各自成项）
function appendAgentStream(blocks: MsgBlock[], runId: string, kind: 'text' | 'thinking', c: string, thinkStart?: number): MsgBlock[] {
  return blocks.map((b) => {
    if (b.kind !== 'agent' || b.runId !== runId) return b
    const items = [...b.items]
    const last = items[items.length - 1]
    // kind 是变量参数，TS 无法经 last.kind === kind 收缩 last——显式判 text 分支
    if (last && last.kind === kind && !(kind === 'text' && last.kind === 'text' && last.error)) items[items.length - 1] = { ...last, text: last.text + c }
    else if (kind === 'thinking') items.push({ kind, text: c, ...(thinkStart != null ? { thinkStart } : {}) } as AgentItem)
    else items.push({ kind, text: c } as AgentItem)
    return { ...b, items }
  })
}

type AgentCompressionItem = Extract<AgentItem, { kind: 'compression' }>

// 子 agent 压缩活动按 run_id 原位更新：开始事件建立 active 项，结束事件
// 收敛同一项，避免并行压缩在卡片里显示成两条互相无法对应的记录。
function upsertAgentCompression(blocks: MsgBlock[], runId: string, item: AgentCompressionItem): MsgBlock[] {
  return blocks.map((b) => {
    if (b.kind !== 'agent' || b.runId !== runId) return b
    const items = [...b.items]
    const activeIndex = items.findLastIndex((it) => it.kind === 'compression' && it.active)
    if (activeIndex >= 0) items[activeIndex] = { ...(items[activeIndex] as AgentCompressionItem), ...item }
    else items.push(item)
    return {
      ...b,
      items,
      compressing: item.active === true,
      compressStartTs: item.active === true ? item.startedAt : undefined,
    }
  })
}

function abortAgentCompression(blocks: MsgBlock[], runId: string): MsgBlock[] {
  return blocks.map((b) => {
    if (b.kind !== 'agent' || b.runId !== runId) return b
    const items = b.items.map((it) => it.kind === 'compression' && it.active
      ? { ...it, active: false, aborted: true, error: undefined }
      : it)
    return { ...b, items, compressing: false, compressStartTs: undefined }
  })
}

function upsertAgentErrorItem(items: AgentItem[], text: string): AgentItem[] {
  const out = [...items]
  const last = out[out.length - 1]
  if (last?.kind === 'text' && last.error) {
    out[out.length - 1] = { ...last, text }
  } else {
    out.push({ kind: 'text', text, error: true })
  }
  return out
}

// genMsOf 输出速度（tokens/s）的分母：本轮的「生成窗口」。
// 分子 outputTokens（usage.Output）**含 reasoning token**，分母必须覆盖产出这些
// token 的时间。规则：
//   - 思考被摘要化（thinkingSummarized；Anthropic display=summarized）→ 流里只有摘要
//     文本，token 数却是**完整**思考量，产出它们的时间在首个流式 token 之前 →
//     必须用整轮 durMs（否则 866 out / 3.9s 窗口 → 222 tok/s，真值约 77 tok/s）；
//   - 思考已流式到达（block.thinking 非空）→ ttftMs 落在首个 reasoning chunk 上，
//     durMs - ttftMs 已含思考时间，与分子自洽；
//   - 思考未流式但 usage 有 reasoning（部分上游把 reasoning 只放在末次 usage 里）→
//     ttftMs 实际落在首个 content chunk 上，durMs - ttftMs 只剩正文那零点几秒。
//     此时减去 ttft 会让分子含思考、分母不含 → 速度虚高几十倍
//     （实测 deepseek-v4-flash：2236 out / 2028 reasoning，0.3s 分母 → 7453 tok/s，
//      真值约 196 tok/s）。故该情形直接用整轮 durMs。
// 纯函数：实时 llm_end 结算与 checkpoint 重放共用，保证刷新前后数字一致。
export function genMsOf(b: { thinking?: string; durMs?: number; ttftMs?: number; reasoningTokens?: number; thinkingSummarized?: boolean }): number | undefined {
  if (b.durMs == null) return undefined
  const reasoning = b.reasoningTokens ?? 0
  // 摘要化思考（Anthropic）与「思考只在末次 usage 里出现」同属「思考时间不在窗口内」，
  // 都退回整轮耗时做分母。
  const unstreamedThink = b.thinkingSummarized === true || (!b.thinking && reasoning > 0)
  const waitMs = unstreamedThink ? 0 : (b.ttftMs ?? 0)
  return Math.max(0, b.durMs - waitMs)
}

// 思考耗时结算：挂到目标 assistant 块（首个 content_chunk / llm_end；已有 thinkMs 不覆盖）。
// bid 缺失时兜底最后一条 assistant（与 appendToAssistant 同策略）。
function setAssistantThinkMs(blocks: MsgBlock[], bid: number | undefined, ms: number): MsgBlock[] {
  const out = [...blocks]
  let idx = bid != null ? out.findIndex((b) => b.id === bid && b.kind === 'assistant') : -1
  if (idx < 0) {
    const last = out[out.length - 1]
    idx = last && last.kind === 'assistant' ? out.length - 1 : -1
  }
  if (idx >= 0) {
    const b = out[idx] as Extract<MsgBlock, { kind: 'assistant' }>
    if (b.thinkMs == null) out[idx] = { ...b, thinkMs: ms }
  }
  return out
}

// 思考耗时结算：挂到子 agent 卡片（并发多 agent，key 归属 run_id）
function setAgentThinkMs(blocks: MsgBlock[], runId: string, ms: number): MsgBlock[] {
  return blocks.map((b) =>
    b.kind === 'agent' && b.runId === runId && b.thinkMs == null ? { ...b, thinkMs: ms } : b,
  )
}

// 是否已存在对应子 agent 卡片（agent_start 建立后，其 LLM/流式事件按 run_id 归属该卡片）
function isAgentRun(v: SessionView, runId: string): boolean {
  return v.blocks.some((b) => b.kind === 'agent' && b.runId === runId)
}

// 审批/提示事件可能在子 Agent 卡片建立前到达（旧日志/事件重排时尤其如此）。
// RunNode 仍保留父子关系，因此用它作为 SubAgent 归属的只读兜底；没有
// run_id 的旧事件继续按主 Agent 处理。
export function isSubAgentRun(v: SessionView, runId: string): boolean {
  if (!runId) return false
  if (isAgentRun(v, runId)) return true
  return v.runNodes.some((n) => n.kind === 'run' && n.id === runId && Boolean(n.parentId))
}

// 审批/提示归属的只读判定；view 缺失（尚未建立）时按主 Agent 处理。
export function approvalOwnerIsSub(v: SessionView | undefined, runId?: string): boolean {
  if (!runId) return false
  return isSubAgentRun(v ?? emptyView(), runId)
}

// 错误反馈块：叙述通道已有一条 error 块 → 原位更新（重试多次只留一条，不刷屏）；
// 否则追加。文本即错误信息（含 raw 桥错误串）。
// retry 非空 = 携带重试进度（llm_error）：覆盖旧进度；缺省（运行失败/中止）→ 清空进度，
// 于是 RetryProgress 停止计时（否则「重试已耗尽」后计时器还会一直走）。
// retryCostUsd 语义 = 本次失败尝试的**增量**（USD）：由 upsertErrorBlock 与旧值累加
//（跨多次重试累计；新一次运行开始时错误块被清除 → 自然重新起算）。
type RetryPatch = Pick<Extract<MsgBlock, { kind: 'error' }>, 'ts' | 'retrying' | 'retryAttempt' | 'retryMax' | 'retryDelayMs' | 'retryStartedAt' | 'retryCostUsd'>

function upsertErrorBlock(blocks: MsgBlock[], text: string, retry?: RetryPatch): MsgBlock[] {
  const idx = blocks.findIndex((b) => b.kind === 'error')
  const cur = idx >= 0 ? (blocks[idx] as Extract<MsgBlock, { kind: 'error' }>) : undefined
  let patch: RetryPatch | undefined
  if (retry) {
    patch = { ...retry }
    if (retry.retryCostUsd) {
      patch.retryCostUsd = (cur?.retryCostUsd ?? 0) + retry.retryCostUsd
    }
    if (retry.retrying) {
      // 本轮重试的起点：同一次重试过程内沿用首个失败时刻（跨多次 llm_error 不重置）→
      // 「已等待时长」是累计值；上一次已结束（成功/耗尽/中止）则本次失败重新起算。
      patch.retryStartedAt = cur?.retrying && cur.retryStartedAt != null ? cur.retryStartedAt : retry.ts
    }
  }
  if (cur) {
    const out = [...blocks]
    // 重试成本随块保留（终态错误不带 retry patch 时也不丢 —— 那是同一轮重试的累计值）
    out[idx] = { kind: 'error', id: cur.id, text, ...(cur.retryCostUsd ? { retryCostUsd: cur.retryCostUsd } : {}), ...patch }
    return out
  }
  return [...blocks, { kind: 'error', id: blockSeq++, text, ...patch }]
}

// 清除错误反馈块（成功响应 / 新一轮 run 开始时调用，避免旧错误残留）
function clearErrorBlocks(blocks: MsgBlock[]): MsgBlock[] {
  return blocks.filter((b) => b.kind !== 'error')
}

function pickToolError(raw: Record<string, unknown>): ToolErrorInfo | undefined {
  if (!raw.is_error) return undefined
  const info: ToolErrorInfo = {
    code: raw.error_code ? String(raw.error_code) : undefined,
    changed: raw.changed === 'true' || raw.changed === 'false' || raw.changed === 'unknown' ? raw.changed : undefined,
    retryable: typeof raw.retryable === 'boolean' ? raw.retryable : undefined,
    nextAction: raw.next_action ? String(raw.next_action) : undefined,
    recovery: raw.recovery ? String(raw.recovery) : undefined,
  }
  return Object.values(info).some((v) => v !== undefined) ? info : undefined
}


// pickImages 事件/checkpoint 的图片内容块 → 前端 QueuedContent（UI 直显用）。
// 返回 undefined（而非空数组）表示「无图片」—— 渲染层据此跳过图片区，也避免
// 每次工具结束都新建一个空数组导致无谓的引用变化。
function pickImages(imgs?: unknown): QueuedContent[] | undefined {
  if (!Array.isArray(imgs) || imgs.length === 0) return undefined
  const out: QueuedContent[] = []
  for (const c of imgs) {
    if (!c || typeof c !== 'object') continue
    const o = c as { type?: unknown; content?: unknown; mime_type?: unknown }
    if (o.type !== 'image') continue
    const content = String(o.content ?? '')
    if (!content) continue
    out.push({ type: 'image', content, ...(o.mime_type ? { mimeType: String(o.mime_type) } : {}) })
  }
  return out.length ? out : undefined
}

// pickArtifacts 解析 AgentEnd.artifacts（本轮产出清单）。
// 逐项校验：name/path 缺失或 kind 非 created/modified 的项跳过（防御脏数据）。
// 返回 undefined = 无产出（调用方不产生块，保持叙述区整洁）。
function pickArtifacts(raw?: unknown): ArtifactItem[] | undefined {
  if (!Array.isArray(raw) || raw.length === 0) return undefined
  const out: ArtifactItem[] = []
  for (const it of raw) {
    if (!it || typeof it !== 'object') continue
    const o = it as { name?: unknown; path?: unknown; kind?: unknown; added?: unknown; removed?: unknown; size?: unknown; lines?: unknown; sha256?: unknown }
    const name = String(o.name ?? '')
    const path = String(o.path ?? '')
    if (!name || !path) continue
    const kind = o.kind === 'created' ? 'created' : 'modified'
    const size = Number(o.size ?? 0)
    const lines = Number(o.lines ?? 0)
    out.push({
      name, path, kind,
      ...(Number(o.added ?? 0) ? { added: Number(o.added) } : {}),
      ...(Number(o.removed ?? 0) ? { removed: Number(o.removed) } : {}),
      // size/lines 为 0 = 未知（如对既有文件 append 的增量语义），不展示该维度
      ...(size > 0 ? { size } : {}),
      ...(lines > 0 ? { lines } : {}),
      ...(o.sha256 ? { sha256: String(o.sha256) } : {}),
    })
  }
  return out.length ? out : undefined
}

function pickDiff(d?: unknown): Diff | undefined {
  if (!d || typeof d !== 'object') return undefined
  const o = d as { path?: unknown; added?: unknown; removed?: unknown; unified?: unknown; truncated?: unknown }
  const unified = String(o.unified ?? '')
  return {
    path: String(o.path ?? ''), added: Number(o.added ?? 0), removed: Number(o.removed ?? 0),
    ...(unified ? { unified } : {}),
    // 引擎截断标记必须透传：抽屉/审批卡据此区分「UI 折叠了」与「引擎本来就截断了」
    //（此前丢弃 → 超 8 KB 的大 diff 被显示成完整内容）。
    ...(o.truncated === true ? { truncated: true } : {}),
  }
}

// —— 流式 chunk 合并（FIX_OPTIMIZATION 问题3①）：实时 reasoning/content chunk 先入缓冲，
// 每 ~32ms 合并一次写入 blocks（一次 React 提交），避免逐 token 全量重渲。
// 快照恢复期间（materializing）不启用：快照事件已提前 coalesce，需同步落库。 ——
let materializing = false
const chunkBuf: AnyEvent[] = []
let chunkFlushTimer: ReturnType<typeof setTimeout> | null = null
function flushChunkBuf(): void {
  chunkFlushTimer = null
  if (chunkBuf.length === 0) return
  const evs = chunkBuf.splice(0)
  // flush 期间同步处理（已提前 coalesce 的事件不得再次入队——否则 32ms 无限循环）
  const wasMaterializing = materializing
  materializing = true
  try {
    for (const ev of coalesceEvents(evs)) {
      try {
        dispatchEvent(ev)
      } catch (err) {
        // 单事件异常不得丢弃同批其余事件（此前 goal_alignment_reminder 的签名错误
        // 会让整批 chunk 在异常点之后全部丢失）。
        console.error('事件分发跳过:', (ev as AnyEvent)?.event_type, err)
      }
    }
  } finally {
    materializing = wasMaterializing
  }
}
function queueChunk(ev: AnyEvent): void {
  chunkBuf.push(ev)
  if (!chunkFlushTimer) chunkFlushTimer = setTimeout(flushChunkBuf, 32)
}
// 终止性事件（llm_end/工具/agent 等）到来前先落库未决 chunk，保证顺序
function drainChunksNow(): void {
  if (chunkFlushTimer) {
    clearTimeout(chunkFlushTimer)
    flushChunkBuf()
  }
}

// flushQueueOnRunEnd 主 run「非自然结束」时把暂存队列整队推送（用户中断 / 运行失败）。
//
// 为什么需要：暂存队列的推送时机原本只有两条 —— 当前 run 的 llm_end（下一轮插话消费）
// 与空闲时发送。而 run 非自然结束时**不会再有 llm_end**：用户「停止前」输入的消息
//（或失败前排队等待的消息）会一直挂在本机「待发送」，要等用户下一次手动发送才落到
// 服务端（用户反馈：点了停止，队列里的数据没有自动推送）。这两类结束都是「本轮已结束，
// 但用户的输入仍应送达」，故整队推送 —— 与 llm_end 的 flush 同一路径、同样幂等
//（只推未推送项 → 服务端 inbox → 新 run 消费 → user_inputs_consumed 回执）。
//
// 编辑窗口（用户明确的语义）：队列项在**推送前**始终可逐条编辑/删除 ——
// llm_start→llm_end 之间、点「停止」后中断生效之前、LLM 异常重试期间都保持可编辑。
// 因此推送只发生在「run 真的结束了」这一刻（agent_end(abort) / session_run_error 到达时），
// **不在 interrupt() 里推**：中断在途时 run 可能仍在收尾，此间推送会被正在中断的 run 的
// 插话 Poll 吞掉（进上下文却不产出回复）。
//
// 自然结束（finish_reason=stop）与轮数上限（max_iterations）**不推送**：既有语义
//（2026-08-17 实测决策：避免 agent_end 立刻用一条旧输入触发「意外新 run」）。那时会话空闲，
// 用户下一条输入/按发送键即会整队推送，消息不会丢。
function flushQueueOnRunEnd(sid: string): void {
  if (materializing) return // 恢复重放（快照/checkpoint）：历史事件不产生新请求
  useAppStore.getState().flushPending(sid)
}

// bridge 事件 → 状态更新：按 session_id 路由进 views[sid]（后台会话事件进自己 view，不打扰当前显示）；
// 活跃会话同步顶层镜像（组件读顶层零改动）。
export function dispatchEvent(raw: AnyEvent): void {
  const setState = useAppStore.setState
  const getState = useAppStore.getState
  // 流式 chunk 合并（FIX_OPTIMIZATION 问题3①）：非快照重建时 chunk 先入缓冲 ~32ms 合并一次；
  // 快照重建（materializing，事件已提前 coalesce）与终止性事件（顺序边界）同步处理。
  if (raw.event_type === 'reasoning_chunk' || raw.event_type === 'content_chunk') {
    if (!materializing) {
      queueChunk(raw)
      return // 稍后 flushChunkBuf 重新 dispatch 处理（合并后）
    }
  } else {
    drainChunksNow()
  }

  // 链路 tab：span 类事件累积（实时投影数据源，见 store/trace.ts）。
  // 只留白名单事件、只存 view 内存（不落盘、不进 wire）；超上限从头丢弃。
  // 与其他事件处理器同款 `?? emptyView()`：真实事件到达即建视图（命令响应才需避免建空视图）。
  if (TRACE_SPAN_TYPES.has(raw.event_type)) {
    const sid = getSid(raw, getState().activeSessionId)
    if (sid) {
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        const arr = [...v.traceEvents, raw]
        if (arr.length > TRACE_EVENTS_MAX) arr.splice(0, arr.length - TRACE_EVENTS_MAX)
        return setView(s, sid, { ...v, traceEvents: arr })
      })
    }
  }

  switch (raw.event_type) {
    case 'bridge_ready':
      // P0/P1（§4.4）：Go 侧启动完成后发一次，作为 embed 自举/命令的精确就绪信号
      // （替代「任意 command response 猜测」；保留 command_response 兜底）。
      embedBridgeAlive = true
      break

    case 'git_snapshot': {
      const sid = getSid(raw, getState().activeSessionId)
      const data = raw.data as { git?: GitStatus; files?: GitFile[] } | undefined
      // 只更新已存在的 view（已恢复会话）——git 响应不得创建空 view：
      // new_session checkpoint 响应回来前，git_snapshot 先到会 setView 创建 blocks=0
      // 的空 view，快速切换时命中缓存分支 → 不发 new_session → 历史永不加载。
      if (sid && data?.git) setState((s) => { const v=s.views[sid]; if(!v) return {}; return setView(s,sid,{...v,git:data.git!,gitFiles:data.files??[],gitLoading:false,gitError:undefined}) })
      break
    }

    case 'git_history': {
        const d = raw.data as { commits?: GitCommit[] } | undefined; const sid=getSid(raw,getState().activeSessionId)
        if (sid) setState((s)=>{const v=s.views[sid]; if(!v) return {}; return setView(s,sid,{...v,gitHistory:Array.isArray(d?.commits)?d!.commits!:[]})}); break
      }
    case 'git_commit_diff': {
        const d=raw.data as {hash?:string;diff?:string}|undefined; const sid=getSid(raw,getState().activeSessionId)
        if(sid&&d?.hash) setState((s)=>{const v=s.views[sid]??emptyView(); return setView(s,sid,{...v,gitHistory:v.gitHistory.map(c=>c.hash===d.hash?{...c,diff:String(d.diff||'')}:c)})}); break
      }

      // 插件异步安装完成 → 状态刷新（installing → ready/error）；bridge 事件，非会话事件。
      // error：异步操作失败的随事件透传（此前只进 stderr，UI 表现为「点了没反应」）；
      // 每次事件全量重建列表 → 上一次的 error 自然清除。
      case 'plugin_status': {
        const d = raw.data as { plugins?: PluginInfo[]; error?: { id: string; message: string } } | undefined
        if (d && Array.isArray(d.plugins)) {
          const list = d.plugins.map((p) =>
            d.error && d.error.id === p.id ? { ...p, error: d.error.message } : { ...p, error: undefined },
          ) as PluginInfo[]
          setState({ plugins: list })
        }
        break
      }
      // ego lite 激活提示（引擎=ego + load_skill(ego-browser) 时 bridge 发；方案 1）：
      // 窗口已带到前台，提示用户切到 agent 的 Space 查看实时操作。
      case 'ego_activate_status': {
        const d = raw.data as { ok?: boolean; message?: string; error?: string } | undefined
        if (d?.ok) {
          getState().showToast(d.message || i18nT('plugin.engine.egoActivatedHint'), 'ok')
        } else {
          getState().showToast(d?.error || i18nT('plugin.engine.egoActivateFail'), 'error')
        }
        break
      }
    case 'session_title': {
      // AI 生成的会话标题（后端异步任务完成广播）：就地更新左栏对应会话的标题
      //（不覆盖用户手动重命名——titleOverrides 渲染优先级更高，此处只改服务端 title）。
      const sid = String(raw.session_id ?? '')
      const wsPath = String(raw.workspace ?? '')
      const title = String(raw.title ?? '')
      if (!sid || !title) break
      setState((s) => {
        const projects = s.projects.map((p) =>
          p.path === wsPath && p.sessions.some((x) => x.id === sid)
            ? { ...p, sessions: p.sessions.map((x) => (x.id === sid ? { ...x, title } : x)) }
            : p,
        )
        const sessions = wsPath === s.workspace
          ? projects.find((p) => p.path === wsPath)?.sessions ?? s.sessions
          : s.sessions
        return { projects, sessions }
      })
      break
    }
    case 'command_response': {
      const r = raw as AnyEvent & { id: number; ok: boolean; error?: string; data?: Record<string, unknown>; workspace?: string }
      const meta = pending[r.id]
      delete pending[r.id]
      const t = meta?.type ?? ''
      embedBridgeAlive = true // 任意响应到达 ⇒ bridge 已就绪（embed 自举的放行信号）
      // §6.2 请求代际：browser_* 响应校验「发起时的 generation」——workspace/session 切换后
      // 递增 browserGeneration，迟到响应（旧代际）不写 UI（防跨 workspace/page 串写）。
      const isBrowserResp = t === 'browser_pages' || t === 'browser_screenshot' || t === 'browser_close'
      const inFlight = isBrowserResp ? browserInFlight.get(r.id) : undefined
      if (inFlight) browserInFlight.delete(r.id)
      const browserStale = isBrowserResp && !!inFlight && inFlight.gen !== browserGeneration
      const line = pendingLines[r.id]
      delete pendingLines[r.id]
      // file_preview 在途登记：无论成功/失败都释放（成功写缓存、失败不写 —— 与阶段 1
      // 「读不到就不给内容」同口径，不把过期内容当新内容显示）。键用发起时的 workspace
      //（响应回显的 respWs）算，才能和 requestFilePreview 登记时的那一个对上。
      if (t === 'file_preview') {
        // respWs 在这个 case 里要到很后面才算出来（这里要覆盖成功/失败两条分支，必须更早）→ 就地取
        const fpWs = typeof r.workspace === 'string' ? r.workspace : ''
        filePreviewInFlight.delete(fileKeyOf(String(meta?.path ?? ''), fpWs || getState().workspace))
      }
      // interrupt_task 在途请求收敛：成功清登记；失败按 id 回滚乐观 interrupting 态
      //（任务恢复 running；若事件已把任务收敛为终态则不受影响）
      if (t === 'interrupt_task') {
        const ip = interruptPending.get(r.id)
        if (ip) {
          interruptPending.delete(r.id)
          if (!r.ok) rollbackInterrupting(ip.sid, ip.taskId)
        }
      }
      // promote_task 在途请求收敛（问题 1）：失败 → 回滚乐观 promoted 态 + 行内提示；
      // 成功只清登记（工具行切后台态由 task_promoted 事件负责，渲染语义不变）
      if (t === 'promote_task') {
        const pp = promotePending.get(r.id)
        if (pp) {
          promotePending.delete(r.id)
          if (!r.ok) rollbackPromote(pp.sid, pp.taskId, r.error)
        }
      }
      // test_connection：结果由 testProvider 的 Promise（onEvent 监听）直接消费——
      // ok/error 都不走 dispatchEvent，避免「测试失败」污染全局 bridgeError 状态栏
      if (t === 'git_snapshot' && r.ok && r.data) {
        const data = r.data as { git?: GitStatus; files?: GitFile[] }
        // 会话级请求：meta 显式带 / 响应回显 session_id → 写入该会话 view；
        // 工作区级探测（loadWsGit，无 session 归属）→ 写 wsGit（Statusbar / 无会话变更面板）。
        // 注意不能用 r.workspace 兜底当 sid —— 那是响应回填的工作区路径，会造出幽灵 view。
        const sid = String(meta?.sessionId || r.data.session_id || '')
        if (sid) {
          if (data.git) setState((s) => { const v=s.views[sid]??emptyView(); return setView(s,sid,{...v,git:data.git!,gitFiles:data.files??[],gitLoading:false,gitError:undefined}) })
          else setState({ gitLoading: false }) // ok 但无 git 数据：复位加载态，避免面板卡「读取中」
          return
        }
        setState(data.git
          ? { wsGit: data.git, wsGitFiles: data.files ?? [], wsGitAt: Date.now(), wsGitLoading: false, wsGitError: undefined }
          : { wsGitLoading: false })
        return
      }
      if (t === 'git_file_diff' && r.ok && r.data) {
        const sid = meta?.sessionId || ''
        const path = String(r.data.path || '')
        const diff = String(r.data.diff || '')
        if (sid && path) setState((s) => { const v=s.views[sid]??emptyView(); return setView(s,sid,{...v,gitFiles:v.gitFiles.map((f) => f.path === path ? {...f, diff} : f)}) })
        else if (path) setState((s) => ({ wsFileDiffs: { ...s.wsFileDiffs, [path]: diff } })) // 工作区级：path → diff 缓存
        return
      }
      if (t === 'git_history' && r.ok && r.data) {
        const commits = Array.isArray(r.data.commits) ? (r.data.commits as GitCommit[]) : []
        const hasMore = r.data.has_more === true
        const sid = String(meta?.sessionId || r.data.session_id || '')
        if (sid) setState((s) => {
          const v = s.views[sid] ?? emptyView()
          const merged = sessHistAppend ? [...v.gitHistory, ...commits] : commits
          return setView(s, sid, { ...v, gitHistory: merged, gitHasMore: hasMore, gitHistLoading: false })
        })
        else setState((s) => ({ wsGitHistory: wsHistAppend ? [...s.wsGitHistory, ...commits] : commits, wsGitHasMore: hasMore, wsGitHistLoading: false }))
        sessHistAppend = false
        wsHistAppend = false
        return
      }
      if (t === 'git_commit_diff' && r.ok && r.data) {
        const hash = String(r.data.hash || '')
        const diff = String(r.data.diff || '')
        const sid = String(meta?.sessionId || r.data.session_id || '')
        const patch = (c: GitCommit) => (c.hash === hash ? { ...c, diff } : c)
        if (sid) setState((s) => { const v=s.views[sid]??emptyView(); return setView(s,sid,{...v,gitHistory:v.gitHistory.map(patch)}) })
        else if (hash) setState((s) => ({ wsGitHistory: s.wsGitHistory.map(patch), wsCommitDiffLoading: undefined }))
        return
      }
      // Git 失败分流（区分「报错」与「no git/空历史」）：只有 snapshot 失败才升级为面板级
      // 错误——否则非仓库/空仓库的 git_history 128 会把整个面板污染成「读取失败」
      //（用户看到的 "exit status 128" 即此误报）。
      if (t === 'git_snapshot' && !r.ok) {
        const sid = meta?.sessionId || ''
        if (sid) setState((s) => { const v=s.views[sid]??emptyView(); return setView(s,sid,{...v,gitLoading:false,gitError:r.error || 'Git 请求失败'}) })
        else setState({ wsGitLoading: false, wsGitError: r.error || 'Git 请求失败' })
        return
      }
      if (t === 'git_file_diff' && !r.ok) {
        // 单文件 diff 失败：就地写入失败文案，避免该文件行永远卡「读取 diff…」
        const sid = meta?.sessionId || ''
        const path = String(r.data?.path || '')
        if (!path) return
        const msg = '⚠︎ diff 读取失败'
        if (sid) setState((s) => { const v=s.views[sid]??emptyView(); return setView(s,sid,{...v,gitFiles:v.gitFiles.map((f) => f.path === path ? { ...f, diff: msg } : f)}) })
        else setState((s) => ({ wsFileDiffs: { ...s.wsFileDiffs, [path]: msg } }))
        return
      }
      if (t === 'cron_runs_detail' && !r.ok) {
        // 详情拉取失败：清空详情（页面显示空态提示）
        setState({ activeCronRun: undefined, cronDetailLoading: false })
        return
      }
      // —— 链路 tab：trace 命令响应（列表 / 详情两粒度；见 store/trace.ts）——
      if (t === 'trace') {
        const sid = meta?.sessionId || ''
        if (!sid) return
        const d = r.data as {
          traces?: TraceSummary[]
          total?: number
          has_more?: boolean
          found?: boolean
          trace?: TraceDetail
          latest?: TraceDetail
        } | undefined
        if (!r.ok || !d) {
          // 失败：复位加载态（面板空态兜底），不打断实时视图
          setState((s) => {
            const v = s.views[sid] ?? emptyView()
            return setView(s, sid, { ...v, traceListLoading: false })
          })
          return
        }
        setState((s) => {
          const v = s.views[sid] ?? emptyView()
          const next: Partial<SessionView> = { traceListLoading: false }
          if (Array.isArray(d.traces)) {
            next.traceList = d.traces
            next.traceListTotal = typeof d.total === 'number' ? d.total : d.traces.length
            next.traceHasMore = d.has_more === true
          }
          // 详情响应：写入 traceDetail（仅当仍是当前选中的 trace，防陈旧响应覆盖）
          if (d.found && d.trace) {
            const want = s.views[sid]?.activeTraceId
            if (!want || want === d.trace.summary.id) next.traceDetail = d.trace
          } else if (d.trace === undefined && !Array.isArray(d.traces)) {
            next.traceDetail = undefined
          }
          // 列表响应里附带的 latest（刷新兜底）：存 traceFallback，供实时投影为空时回显
          if (Array.isArray(d.traces) && d.latest) next.traceFallback = d.latest
          return setView(s, sid, { ...v, ...next })
        })
        return
      }
      if ((t === 'git_history' || t === 'git_commit_diff') && !r.ok) {
        // 历史/提交 diff 失败：静默复位加载态（面板以空态兜底），不打扰
        setState({ wsGitHistLoading: false, wsCommitDiffLoading: undefined })
        const sid = meta?.sessionId || ''
        if (sid) setState((s) => { const v=s.views[sid]??emptyView(); return setView(s,sid,{...v,gitHistLoading:false}) })
        return
      }
      // —— Git 写操作（P1/P2）：成功 → 双层重拉（快照+历史；HEAD 可能已变）；失败 → gitOpError ——
      if ((t === 'git_stage' || t === 'git_unstage' || t === 'git_discard' || t === 'git_commit' || t === 'git_checkout' || t === 'git_init') && r.ok) {
        setState({ gitOpBusy: undefined })
        const st = getState()
        st.loadWsGit()
        st.loadWsGitHistory()
        if (st.activeSessionId) { st.refreshGit(); st.loadGitHistory() }
        if (t === 'git_checkout') st.loadGitBranches() // 分支切换后刷新清单的 current 标记
        return
      }
      if ((t === 'git_stage' || t === 'git_unstage' || t === 'git_discard' || t === 'git_commit' || t === 'git_checkout' || t === 'git_init') && !r.ok) {
        setState({ gitOpBusy: undefined, gitOpError: r.error || 'Git 操作失败' })
        return
      }
      if (t === 'git_branch_list' && r.ok && r.data) {
        // 分支清单（工作区级；弹层打开时拉取）
        const branches = Array.isArray(r.data.branches) ? (r.data.branches as GitBranchInfo[]) : []
        setState({ gitBranches: branches })
        return
      }

      if (!r.ok) {
        // ask_batch 推送失败（session not found / 预算超限 / 队列满 / 全部被跳过）：
        // 回滚该会话暂存队列的 pushed 标记——消息回到「待发送」可编辑可重发，
        // 消灭「乐观标记已推送但服务端从未入队」的静默死局（问题十）。
        if (t === 'ask_batch') {
          const sid = String(meta?.sessionId || r.data?.session_id || getState().activeSessionId || '')
          if (sid) setState((st) => {
            const v = st.views[sid] ?? emptyView()
            return setView(st, sid, {
              ...v,
              pendingQueue: v.pendingQueue.map((q) => (q.pushed ? { ...q, pushed: false } : q)),
              pendingQueueError: r.error || 'ask_batch 推送失败',
            })
          })
          return
        }
        // 自动建会话失败：作废等待态并丢弃缓冲（消息无法投递，不再等）
        if (t === 'new_session') {
          sessionPending = false
          firstAsks.length = 0
        }
        if (t === 'compact') {
          // Manual compact targets the main Agent. Do not clear concurrent
          // SubAgent compression watchdogs in the same Session.
          const sid = String(meta?.sessionId || getState().activeSessionId || '')
          disarmCompressWatchdog(sid)
          if (sid) setState((s) => {
            const v = s.views[sid]
            return v ? setView(s, sid, {
              ...v,
              compressing: false,
              compressStartTs: undefined,
              compressBefore: undefined,
              compressRunId: undefined,
              compressReason: undefined,
            }) : s
          })
        }
        // switch_model 失败（恢复的会话记录模型在当前 provider 不可用）：静默回退
        // 全局默认并修正【该命令目标会话】的记录（避免每次切回该会话都失败重试），
        // 不置 bridgeError。注意：必须按 meta.sessionId 定位目标会话 —— 命令在途时
        // 用户可能已切到别的会话，若按「当前活跃会话」写记录会把别的会话的模型
        // 覆盖成全局默认（模型串 Session 的另一个根因）。
        if (t === 'switch_model') {
          const prefs = loadJSON<Partial<Prefs>>(LS_PREFS) ?? {}
          const fb = prefs.model ?? DEFAULT_MODEL
          const st = getState()
          const targetSid = String(meta?.sessionId || st.activeSessionId || '')
          const ws = st.workspace || meta?.workspace || ''
          if (ws && targetSid) {
            const key = titleKey(ws, targetSid)
            const prev = loadJSON<Record<string, Partial<Prefs>>>(LS_SESSION_PREFS) ?? {}
            saveJSON(LS_SESSION_PREFS, { ...prev, [key]: { ...(prev[key] ?? {}), model: fb } })
          }
          // 仅当目标会话仍是当前所见会话时才改全局 model（否则不动，避免污染新会话的 UI 配置）
          if (targetSid === st.activeSessionId) setState({ model: fb })
          return
        }
        // 斜杠命令失败（未知命令等）：回显错误到叙述；其余命令走 bridgeError 状态
        if (t === 'command' && r.error) {
          const sid = getState().activeSessionId
          if (sid) {
            setState((s) => {
              const v = s.views[sid] ?? emptyView()
              return setView(s, sid, {
                ...v,
                blocks: [...v.blocks, { kind: 'assistant', id: blockSeq++, text: `（命令失败）${r.error}`, streaming: false, thinking: '' }],
              })
            })
          }
          return
        }
        // 浏览器面板数据拉取失败（browser_pages / browser_screenshot）：务必清 busy，
        // 否则 web tab 会一直卡在「拉取中…」（响应失败路径此前不回落 busy，切会话后
        // 上一个请求可能已失效 → loading 永久卡死）。浏览器错误是本地的（插件未连接/
        // 无页面等），不当全局 bridgeError，前端 web tab 有自身 loading/空态承接。
        if (t === 'browser_pages' || t === 'browser_screenshot' || t === 'browser_close') {
          disarmBrowserBusyWatchdog()
          setState({ browserBusy: false })
          return
        }
        setState({ bridgeError: r.error || 'command failed' })
        return
      }
      // /reload_skills 成功：bridge 已重扫 .agents/skills + .go-code/skills —— 立即刷新技能清单
      //（/ 自动补全 + SkillsPage 即时反映新增/修改的技能）
      if (t === 'command' && (line === '/reload_skills' || line === '/reload_skills ')) {
        send({ type: 'skills', payload: {} })
      }
      const d = r.data || {}
      const respWs = typeof r.workspace === 'string' ? r.workspace : ''

      // list 分流：并入对应项目会话列表；仅启动水合的活跃项目 list 才自动恢复最近/新建
      //（recoverPaths 门控：主动添加/切换工作区不自动建会话，建会话是显式动作）
      if (t === 'list') {
        const sessions = Array.isArray(d.sessions) ? normalizeSessions(d.sessions as SessionMeta[]) : []
        if (respWs) setState((s) => applySessions(s, respWs, sessions))
        if (respWs === getState().workspace) {
          let recovered = false
          if (recoverPaths.has(respWs)) {
            recoverPaths.delete(respWs)
            if (sessions.length > 0) {
              // 恢复上次活跃会话（persisted lastSession），找不到则回到列表第一条。
              // sessionPending 置位：恢复 new_session 在途 → 此窗口的 ask 只缓冲、等激活后 flush
              //（防恢复期间二次自动建会话）。
              const saved = loadJSON<PersistProjects>(LS_PROJECTS)
              const pref = saved?.lastSession && saved.lastSession.path === respWs ? saved.lastSession.id : sessions[0].id
              sessionPending = true
              recovered = true
              const recoverId = sessions.some((x) => x.id === pref) ? pref : sessions[0].id
              send({ type: 'new_session', payload: { id: recoverId } }, true)
            }
            // 空列表不自动建会话（用户拍板 2026-08-14：不建空会话；首会话由用户显式「新建会话」）
          }
          // 活跃工作区 list 已到：水合完成，恢复后续 ask 走正常路径
          hydrating = false
          // 水合期缓冲的首条消息无会话可恢复（list 为空）→ 回退自动建会话，激活后统一 flush
          if (!recovered && firstAsks.length && !getState().activeSessionId) {
            sessionPending = true
            send({ type: 'new_session' }, true)
          }
        }
        break
      }

      // metrics 分流：聚合看板报告（全局，无 session_id）。到达时刻/窗口一并落库 ——
      // 使用统计页与空会话图表据此判断是否 stale（全量扫描数百毫秒，不做无谓重拉）。
      if (t === 'metrics') {
        setState({ metrics: d.report as MetricsReport | undefined, metricsAt: Date.now(), metricsFrom: metricsQueryFrom })
        break
      }

      // 会话配置响应只能更新当前所见 session。切换后迟到的旧响应已在 bridge 内正确作用于
      // 原 session，但不得覆盖新 session 的 UI 配置。
      const currentConfigResponse = Boolean(
        meta?.sessionId
        && meta.sessionId === getState().activeSessionId
        && (!meta.workspace || meta.workspace === getState().workspace)
        && meta.navigationGeneration === navigationGeneration,
      )
      // switch_mode / switch_effort 的 UI 已乐观更新；回执仅作目标会话确认。
      if (t === 'switch_mode' || t === 'switch_effort') break
      // switch_persona / switch_model：确认（store 已乐观设置，bridge 回执数据兜底）
      if (t === 'switch_persona') {
        if (currentConfigResponse && typeof d.persona === 'string') setState({ persona: d.persona as 'code' | 'work' })
        break
      }
      if (t === 'switch_model') {
        if (!currentConfigResponse) break
        if (typeof d.model === 'string') setState({ model: d.model })
        // bridge 回执目标 provider（自动切活跃后）→ 同步 store 活跃 Provider，保持 UI 一致
        if (typeof d.provider === 'string' && d.provider) {
          setState((s) => {
            const cur = s.settings
            if (!cur || cur.provider.active === d.provider) return {}
            return { settings: { ...cur, provider: { ...cur.provider, active: d.provider as string } } }
          })
        }
        break
      }
      // file_preview：内容按**归一化路径**写进缓存（阶段 2）。
      // 为什么键里带 workspace：缓存是全局的（标签集才按 scope 隔离），而「相对路径」只有
      // 相对于工作区才有意义 —— 不带工作区的话，两个工作区里的同名 src/a.ts 会互相覆盖。
      // 用 respWs（响应回显的发起时工作区）而不是当前工作区：切换工作区后迟到的响应仍归属
      // 它自己的工作区，不会把 A 的内容写进 B 的键。切工作区时缓存整体清空（见 clearBrowser）。
      if (t === 'file_preview') {
        const pv = d as unknown as FilePreview
        const key = fileKeyOf(String(pv.path ?? ''), respWs || getState().workspace)
        setState((s) => ({ filePreviews: putFilePreview(s.filePreviews, key, pv) }))
        break
      }
      // skills：工作区技能清单（含启用标志）。全局导入后会并发刷新多个工作区；
      // 迟到的非活跃工作区响应只能更新 bridge 对应工作区，不能覆盖当前 UI。
      if (t === 'skills') {
        const skillWorkspace = respWs || meta?.workspace || ''
        if (skillWorkspace && skillWorkspace !== getState().workspace) break
        if (Array.isArray(d.skills)) setState({ skills: d.skills as SkillInfo[] })
        break
      }
      // skill_get：技能详情（SKILL.md 指令 + 资源清单；预览）
      if (t === 'skill_get') {
        setState({ skillDetail: d as unknown as SkillDetail })
        break
      }      // context_breakdown：上下文构成分区（Composer ctx 环 hover 面板）。
      // 会话级数据：响应归属的会话 ≠ 当前活跃会话（切会话后迟到）→ 丢弃，避免面板
      // 把上个会话的占比挂到新会话上。
      if (t === 'context_breakdown') {
        const bdSid = String(meta?.sessionId || '')
        if (!r.ok || bdSid !== getState().activeSessionId) break
        const parts = Array.isArray(d.parts)
          ? (d.parts as { key?: unknown; tokens?: unknown }[]).map((p) => ({
              key: String(p.key ?? ''),
              tokens: Number(p.tokens ?? 0),
            }))
          : []
        const anchorSource = d.anchor_source === 'real' || d.anchor_source === 'estimate' || d.anchor_source === 'none'
          ? d.anchor_source
          : (d.estimated === true ? 'none' : 'real')
        setState({
          ctxBreakdown: {
            parts,
            total: Number(d.total ?? 0),
            estimated: d.estimated === true,
            anchorSource,
            degraded: d.degraded === true,
            messageCount: Number(d.message_count ?? 0),
            sessionId: bdSid,
            at: Date.now(),
          },
        })
        break
      }
      // skill_browse：远程市场浏览（只读元数据 + 已装标记；SkillsPage 市场区块）
      if (t === 'skill_browse') {
        if (Array.isArray(d.skills)) setState({ marketSkills: d.skills as MarketSkill[] })
        break
      }
      // market_list：已保存市场（SkillsPage 市场管理）
      if (t === 'market_list') {
        if (Array.isArray(d.markets)) setState({ markets: d.markets as MarketInfo[] })
        break
      }
      // mcp_projects：多项目项目层汇总（MCP 页「项目」区；exists=false 的前端过滤掉）
      if (t === 'mcp_projects') {
        setState({
          mcpProjects: Array.isArray(d.projects) ? (d.projects as MCPProjectInfo[]) : [],
          mcpProjectsLoading: false,
        })
        break
      }
      // hooks_list：hooks 配置来源汇总（钩子页数据源）
      if (t === 'hooks_list') {
        const info = d as unknown as HooksInfo
        if (info && typeof info === 'object' && info.user) {
          setState({ hooksInfo: { ...info, at: Date.now() }, hooksLoading: false, hooksSaving: false })
        } else {
          setState({ hooksLoading: false, hooksSaving: false })
        }
        break
      }
      // mcp_list：MCP 服务器清单 + 状态 + 配置层状态（SettingsModal MCP tab 数据源）
      if (t === 'mcp_list') {
        if (Array.isArray(d.servers)) setState({ mcpServers: d.servers as MCPServerInfo[], mcpSaving: false })
        setState({ mcpLayers: Array.isArray(d.layers) ? (d.layers as MCPLayerInfo[]) : [] })
        break
      }
      // cron_list：定时任务清单 + 开关态（CronPage 数据源）
      if (t === 'cron_list') {
        setState({
          cronJobs: Array.isArray(d.jobs) ? (d.jobs as CronJob[]) : [],
          cronEnabled: d.enabled !== false,
          cronAutoClean: d.auto_clean !== false,
          cronLoading: false,
        })
        break
      }
      // cron_runs：运行账本（CronPage「运行历史」）
      if (t === 'cron_runs') {
        setState({ cronRuns: Array.isArray(d.runs) ? (d.runs as CronRun[]) : [] })
        break
      }
      // cron_runs_detail：某次执行的本地化历史（CronRunDetailPage 数据源）。
      // snapshot 事件与主对话页恢复同构 → materializeSnapshot 重建视图（专用 sid），
      // 详情页用 Narrative 渲染同一套 blocks（消息/工具卡片/思考块全复用）。
      if (t === 'cron_runs_detail') {
        const detail = (d as unknown as CronRunDetail) ?? undefined
        if (detail?.snapshot && Array.isArray((detail.snapshot as { events?: unknown[] }).events)) {
          const cronSid = `cron:${detail.run_id}`
          materializeSnapshot(cronSid, (detail.snapshot as { events: unknown[] }).events)
          setState({
            activeCronRun: { ...detail, viewSid: cronSid },
            cronDetailLoading: false,
          })
        } else {
          setState({ activeCronRun: detail, cronDetailLoading: false })
        }
        break
      }
      // plugin_list：插件清单 + 状态（SettingsModal 插件 tab 数据源）
      if (t === 'plugin_list') {
        if (Array.isArray(d.plugins)) setState({ plugins: d.plugins as PluginInfo[] })
        break
      }
      // browser_pages：浏览器打开页面（web tab）
      if (t === 'browser_pages') {
        if (Array.isArray(d.pages)) {
          disarmBrowserBusyWatchdog()
          if (!browserStale) {
            const pages = d.pages as BrowserPage[]
            setState({ browserPages: pages, browserBusy: false })
            // 回退/window 模式：MCP 页列表也是**同一套面板级标签**的数据源（数字 pageId 空间）。
            // 权威口径：原生视图池有页面时以它为准（syncWebTabs 内部会清掉另一数据源的标签）。
            const sel = pages.find((p) => p.selected)
            useAppStore.getState().syncWebTabs(
              'mcp',
              pages.map((p) => ({ key: `p${p.id}`, url: p.url, title: p.title })),
              sel ? `p${sel.id}` : null,
            )
          }
        }
        break
      }
      // browser_screenshot：页面截图 data URL（web tab）
      if (t === 'browser_screenshot') {
        if (typeof d.image === 'string') {
          disarmBrowserBusyWatchdog()
          if (!browserStale) setState({ browserShot: d.image, browserBusy: false })
        }
        break
      }
      // browser_close：关闭页面成功 → 用 bridge 返回的最新页列表覆盖（含选中页变化），
      // 并把截图清掉（当前展示页可能已关），随后 web tab 自动重拉截图。
      if (t === 'browser_close') {
        disarmBrowserBusyWatchdog()
        if (browserStale) break // 迟到关闭响应：不写 UI（页面已切换）
        const pages = Array.isArray(d.pages) ? (d.pages as BrowserPage[]) : undefined
        setState(pages ? { browserPages: pages, browserBusy: false } : { browserBusy: false })
        if (pages) setState({ browserShot: undefined }) // 展示页可能已关，截图作废
        // §6.6：bridge 响应已带最新页表时直接消费，不再二次 list（去重）
        if (!pages) getState().fetchBrowserPages()
        break
      }
      // im_config_get：IM 集成配置（SettingsModal IM tab 数据源）
      if (t === 'im_config_get') {
        if (d.config && typeof d.config === 'object') setState({ imConfig: d.config as IMConfig })
        break
      }
      // im_schema_list：IM 网关 Schema（动态表单）
      if (t === 'im_schema_list') {
        if (Array.isArray(d.schemas)) setState({ imSchemas: d.schemas as IMGatewaySchema[] })
        break
      }
      // im_status：网关连接状态
      if (t === 'im_status') {
        if (Array.isArray(d.gateways)) {
          const m: Record<string, string> = {}
          for (const g of d.gateways as Array<{ id: string; status: string }>) m[g.id] = g.status
          setState({ imStatuses: m })
        }
        break
      }
      // im_chat_list：群列表（路由绑定下拉；gateway_id 从请求登记表取）
      if (t === 'im_chat_list') {
        const gid = imChatReq.get(r.id)
        imChatReq.delete(r.id)
        if (Array.isArray(d.chats) && gid) {
          setState((st) => ({ imChats: { ...st.imChats, [gid]: d.chats as IMChatInfo[] } }))
        }
        break
      }
      // browser_config_get：浏览器插件配置（插件详情设置表单数据源）
      if (t === 'browser_config_get') {
        if (d.config && typeof d.config === 'object') setState({ browserConfig: d.config as BrowserCfg })
        break
      }
      // browser_engine_get：浏览器引擎 + ego 状态（SettingsModal 插件 tab）
      if (t === 'browser_engine_get') {
        const eng = d.browser_engine === 'ego' || d.browser_engine === 'mcp' ? d.browser_engine : undefined
        const patch: Partial<AppState> = {}
        if (eng) {
          patch.browserEngine = eng
          patch.settings = { ...(useAppStore.getState().settings ?? ({} as AppSettings)), browser_engine: eng }
        }
        if (d.ego && typeof d.ego === 'object') patch.egoStatus = d.ego as AppState['egoStatus']
        if (Object.keys(patch).length) setState(patch as AppState)
        break
      }
      // list_provider_presets：内置 Provider 预设（providers.json 单一事实源）→ providerPresets；
      // models_cost（预设模型内置价，纯本地注册表读取）→ 并入 modelCosts —— 价格编辑器
      // 打开设置即回显内置价，无需先「拉取模型列表」（那依赖网络 + 用户点击）。
      if (t === 'list_provider_presets') {
        if (Array.isArray(d.presets)) {
          const costs = (d.models_cost ?? {}) as Record<string, Record<string, ModelPrices>>
          setState((s) => {
            const next = { ...s.modelCosts }
            for (const [pid, m] of Object.entries(costs)) {
              if (!m || typeof m !== 'object') continue
              next[pid] = { ...(next[pid] ?? {}), ...m }
            }
            return { providerPresets: d.presets as ProviderPreset[], modelCosts: next }
          })
        }
        break
      }
      // list_models：provider 模型列表（Provider 面板「拉取模型列表」）。
      // 拉到的新模型并入 settings.models；窗口取已知模型最大 max_tokens（KNOWN_MODEL_WINDOWS，
      // 未知 128k）→ Composer 下拉 + Provider 面板即时可见；max_tokens 一并补默认
      // （KNOWN_MODEL_MAX_TOKENS，未知 8192）；
      // 持久化 + set_provider 让 bridge 的模型注册表/窗口/输出上限同步。
      if (t === 'list_models') {
        if (Array.isArray(d.models)) {
          const prov = String(d.provider ?? '') as 'deepseek' | 'openai' | 'opencode' | 'kimi' | 'zhipu'
          setState((s) => {
            const pc = s.settings.provider[prov]
            if (!pc) return { fetchedModels: { ...s.fetchedModels, [prov]: d.models as string[] } }
            const merged = { ...pc.models }
            const known = KNOWN_MODEL_WINDOWS[prov] ?? {}
            const mergedMax = { ...pc.max_tokens }
            const knownMax = KNOWN_MODEL_MAX_TOKENS[prov] ?? {}
            // capabilities（后端注册表真实 inputs/窗口/上限/cost）：并入 input_types（图片门控）
            // 与 modelCosts（注册表内置价 —— 价格编辑器空覆盖回显）
            const caps = (d.capabilities ?? {}) as Record<string, { inputs?: string[]; context_window?: number; max_tokens?: number; reasoning?: boolean; cost?: { input?: number; cache_read?: number; cache_write?: number; output?: number } }>
            const mergedInputTypes = { ...(pc.input_types ?? {}) }
            const mergedCosts = { ...(s.modelCosts[prov] ?? {}) }
            for (const name of d.models as string[]) {
              const cur = merged[name]
              // 窗口：后端 capabilities（注册表真实窗口，权威）> 已知表 > 默认 128k；
              // 已配置且已知更大窗口 → 顶到模型最大（不低于用户已设更大值，如 1M 标记）
              if (cur == null) merged[name] = caps[name]?.context_window ?? known[name] ?? MODEL_WINDOW
              else if ((caps[name]?.context_window ?? known[name]) && (caps[name]?.context_window ?? known[name]) > cur) merged[name] = caps[name]?.context_window ?? known[name]
              // 输出上限：新模型补后端真实上限（权威）→ 已知默认 → 8192；已配置保持用户值
              if (mergedMax[name] == null) mergedMax[name] = caps[name]?.max_tokens ?? knownMax[name] ?? MODEL_MAX_TOKENS_DEFAULT
              // input_types：后端 capabilities 提供真实 inputs（含 image → 允许上传）；用户显式配置优先
              if (mergedInputTypes[name] == null && caps[name]?.inputs?.length) {
                mergedInputTypes[name] = caps[name].inputs as string[]
              }
              // 内置价：capabilities.cost（注册表 providers.json 价 + 用户覆盖的生效价）。
              // 全 0/缺省不记录 —— 与「无价表/免费」等价，前端保持空覆盖占位。
              const c = caps[name]?.cost
              if (c && (c.input || c.output || c.cache_read || c.cache_write)) {
                mergedCosts[name] = { input: c.input ?? 0, cache_read: c.cache_read ?? 0, cache_write: c.cache_write ?? 0, output: c.output ?? 0 }
              }
            }
            return {
              settings: { ...s.settings, provider: { ...s.settings.provider, [prov]: { ...pc, models: merged, max_tokens: mergedMax, input_types: mergedInputTypes } } },
              fetchedModels: { ...s.fetchedModels, [prov]: d.models as string[] },
              modelCosts: { ...s.modelCosts, [prov]: mergedCosts },
            }
          })
          const cur = getState().settings.provider[prov]
          if (cur) {
            void transport.providerSave({ provider: prov, models: cur.models, max_tokens: cur.max_tokens, input_types: cur.input_types, openai_compat: cur.openai_compat })
            send({ type: 'set_provider', payload: { provider: prov, base_url: cur.base_url, models: cur.models, max_tokens: cur.max_tokens, input_types: cur.input_types, openai_compat: cur.openai_compat } })
            // 拉完列表自动补价（回填）：从实时价源按**覆盖**语义重写一次并落盘（2026-09-21
            // 用户决策「拉取列表的时候自动覆盖就行」）—— 拉到的新模型立刻有价，上游改价也随
            // 列表刷新同步；失败静默（不打扰拉列表这个动作本身）。
            void getState().refreshPrices({ provider: prov })
          }
        }
        break
      }

      if (t === 'new_session' && d.session_id) {
        const sid = String(d.session_id)
        const responseWorkspace = respWs || meta?.workspace || ''
        const expectedSession = meta?.sessionId
        // 恢复协议 attempt 校验（问题四）：响应回显 restore_id+attempt；
        // 若该会话已有更新的恢复请求（attempt 更大），本响应为陈旧响应，直接丢弃。
        const respAttempt = typeof d.attempt === 'number' ? d.attempt : undefined
        const isStaleAttempt = respAttempt != null
          && (pendingRestoreAttempt[sid] ?? 0) > respAttempt
        const navigationCurrent = Boolean(
          !isStaleAttempt
          && meta
          && meta.navigationGeneration === navigationGeneration
          && (!responseWorkspace || responseWorkspace === getState().workspace)
          && (!expectedSession || expectedSession === sid),
        )
        // 过期导航响应不得重建/激活视图，也不得关闭当前导航的 loading。若同 sid 已有
        // 更新的恢复请求，保留其 pending/buffer；否则清理本请求留下的恢复标记。
        if (!navigationCurrent) {
          if (snapshotPending.get(sid) === r.id) {
            snapshotPending.delete(sid)
            // 此前这里直接丢弃缓冲事件 → 该会话这段时间的事件在渲染进程永久消失
            //（视图只剩更晚的内容）。改为照常 dispatch：它们本就属于该会话的视图，
            // 且后续恢复（checkpoint 是更新的快照）会整体替换视图，不会重复计。
            const buffered = preBuf.get(sid)
            preBuf.delete(sid)
            if (buffered && buffered.length) {
              for (const ev of coalesceEvents(buffered)) dispatchEvent(ev)
            }
          }
          // 2026-08-22：过期 new_session 响应必须释放"发送时创建"锁。否则 sessionPending
          // 永久卡 true → 后续无会话时的 ask 只进 firstAsks、永不建会话（输入石沉大海）。
          // 注意：firstAsks 不在此清空 —— 已有明确会话激活时由 setActiveSession 转入其
          // 暂存队列（不丢不误投）；仍在等待新会话时由下一次 new_session 响应 flush。
          if (t === 'new_session') sessionPending = false
          // 显式恢复的响应被丢弃（陈旧代际/陈旧 attempt）：若该会话视图仍为空 → 有限重试。
          // 同 sid 有更新请求在途时 scheduleRestoreRetry 会自行退出（不叠发、不误报）。
          if (t === 'new_session' && meta?.sessionId === sid) {
            scheduleRestoreRetry(
              sid,
              isStaleAttempt ? '响应按陈旧 attempt 丢弃' : '响应按陈旧导航丢弃',
              true,
              meta.workspace,
            )
          }
          break
        }
        // 快照恢复（FIX_OPTIMIZATION 问题2③）：bridge 已把事件日志聚合成单行快照 ——
        // 先批量重建视图（coalesce + 逐事件 dispatch + 归一化，一次提交），再统一激活。
        // checkpoint-first（目标主路径）：bridge 直接回 view_checkpoint 投影，不再重放 events。
        const restored = String(d.restored ?? '')
        // 诊断：冷恢复响应关键状态（保留，便于定位"恢复后空视图"问题）
        if (restored === 'checkpoint' || restored === 'snapshot') {
          console.log('[restore-debug]', 'sid=' + sid, 'restored=' + restored, 'revision=' + (d.revision ?? '-'), 'hasVC=' + (!!d.view_checkpoint), 'vcType=' + typeof d.view_checkpoint, 'msgLen=' + (Array.isArray(d.messages) ? d.messages.length : '-'))
        }
        const snapEvents = restored === 'snapshot' && d.snapshot && typeof d.snapshot === 'object'
          ? (d.snapshot as { events?: unknown[] }).events
          : undefined
        const cpData = restored === 'checkpoint' && d.view_checkpoint && typeof d.view_checkpoint === 'object' && d.view_checkpoint !== null
          ? (d.view_checkpoint as CheckpointView)
          : undefined
        let hydrateFailed = false // hydrate 抛错 → 下方恢复落地校验据此重试（见 scheduleRestoreRetry）
        if (Array.isArray(snapEvents)) {
          materializeSnapshot(sid, snapEvents, r.id, d.live !== true)
        } else if (cpData) {
          try {
            hydrateCheckpoint(sid, cpData, r.id, {
              revision: typeof d.revision === 'number' ? d.revision : undefined,
              clearGeneration: typeof d.clear_generation === 'number' ? d.clear_generation : undefined,
              degraded: d.degraded === true,
            })
          } catch (err) {
            // hydrate 失败（checkpoint 结构异常）：不残留空 view，回退 legacy snapshot /
            // messages 路径（下方 setState 走 nv=v 分支；若也无 snapshot/messages 则空视图，
            // 但至少不因异常中断后续激活逻辑）。
            console.error('checkpoint hydrate 失败，回退:', err)
            hydrateFailed = true
          }
        }
        setState((s) => {
          const v = s.views[sid] ?? emptyView()
          let nv = v
          // 快照/checkpoint 路径：视图已重建（含归一化）；messages 路径仅无事件日志时走到
          if (!Array.isArray(snapEvents) && !cpData && Array.isArray(d.messages) && d.messages.length) {
            nv = {
              ...nv,
              // 2026-08-22：messages 降级路径支持内容块（image）—— 恢复时图片附件不丢
              blocks: (d.messages as { role: string; text: string; contents?: { type?: string; content?: string; mime_type?: string }[] }[]).map((m) => {
                const contents = Array.isArray(m.contents) && m.contents.length
                  ? m.contents.map((c) => ({
                      type: c.type === 'image' ? ('image' as const) : ('text' as const),
                      content: String(c.content ?? ''),
                      ...(c.mime_type ? { mimeType: c.mime_type } : {}),
                    }))
                  : undefined
                return m.role === 'user'
                  ? { kind: 'user' as const, id: blockSeq++, text: m.text, contents }
                  : { kind: 'assistant' as const, id: blockSeq++, text: m.text, streaming: false, thinking: '' }
              }),
            }
          }
          if (Array.isArray(d.todos)) nv = { ...nv, todos: d.todos as TodoItem[] }
          const target = respWs || s.workspace
          let base: Partial<AppState> = { activeSessionId: sid, sessionLoading: false } // issue 4：会话激活即关加载遮罩
          if (switchTimer) clearTimeout(switchTimer) // 内容已加载：撤掉兜底定时器
          if (target) {
            let list: SessionMeta[]
            if (Array.isArray(d.sessions) && d.sessions.length) {
              list = normalizeSessions(d.sessions as SessionMeta[])
              // 防御（问题 3）：恢复/新建的会话必须出现在列表中，否则左栏活跃标记无行可亮。
              // 正常路径响应自带完整列表；异常/截断响应漏掉本会话时补一行。
              if (!list.some((x) => x.id === sid)) list.push({ id: sid, title: 'New Chat' })
            } else {
              const cur = s.projects.find((p) => p.path === target)?.sessions ?? []
              list = cur.some((x) => x.id === sid) ? cur : [...cur, { id: sid, title: 'New Chat' }]
            }
            base = { ...base, ...applySessions(s, target, list) }
          }
          // active=true：new_session 响应激活该会话，镜像其 view 到顶层；
          // busySids 随本次状态转移收敛（快照已把 running→done，该会话不再 busy）
          const upd = setView(s, sid, nv, base, true)
          // 新建/恢复的会话登记进顶部快捷栏：此前只有 setActiveSession（点击/快捷键切换）
          // 会写入 —— 新建会话走的是 new_session 响应（不经 setActiveSession），新会话因此
          // 从不入栏，满员时也无从「挤掉 index 0」。此处与切换同一队列语义（withRecent）。
          const recentSessions = withRecent(s.recentSessions, target || s.workspace || '', sid)
          return { ...upd, recentSessions, busySids: busySidsFromViews(upd.views) }
        })
        persistProjects(getState()) // 记录最近激活会话（重启后恢复用）
        // 恢复落地校验（2026-09-21）：显式恢复（带 id 的 new_session）却没有拿到任何历史数据，
        // 或 hydrate 抛错 → 此刻视图为空、活跃会话已激活，正是「footbar 只算当前」的现场
        //（用户实测：280 轮 / 80M / $0.51 的会话重载后只显示最后 4 轮）。有限重试兜底。
        if (meta?.sessionId === sid) {
          const gotSnapshot = Array.isArray(snapEvents)
          const gotMessages = Array.isArray(d.messages) && d.messages.length > 0
          if (hydrateFailed) {
            scheduleRestoreRetry(sid, 'hydrate 异常（回退空视图）', true, meta.workspace, r.id)
          } else if (!gotSnapshot && !cpData && !gotMessages) {
            // bridge 既无 record 也读不到事件日志时也走这里（真·空会话）→ 不 loud，
            // 只留 warn；确有历史的会话会因重试拿回内容。
            scheduleRestoreRetry(sid, '响应无历史数据', false, meta.workspace, r.id)
          } else if (!gotSnapshot && !cpData && gotMessages) {
            // messages 降级：只有纯文本历史，turns/usage/cost 无从恢复 → 累计从 0 起算。
            // 显式告警（此前静默，表现为「footbar 不算历史」）。
            console.warn(`[restore] 会话 ${sid} 走 messages 降级：回合/用量/成本无法恢复，将从 0 起算`)
          }
        }
        // 恢复该会话上次的 mode/model/effort/persona（会话级记录；无记录保持全局默认）。
        // 在 setState 之后应用：需要已激活会话的最新 state（问题 2，2026-08-17）。
        applySessionPrefs(respWs || getState().workspace || '', sid)
        // 新建 session 没有会话级记录：applySessionPrefs 已采用最近一次全局选择作为默认值；
        // 立即固化成该 session 自己的配置，后续全局默认变化不会反向改变它。已有 session 幂等覆盖。
        persistPrefs(getState())
        // 发送时创建：new_session 激活后 flush 缓冲的首条消息。仅当该会话仍活跃且
        // 工作区未变（等待期用户切走 → setActiveWorkspace 已作废缓冲，不会误投）。
        sessionPending = false
        if (firstAsks.length && respWs === getState().workspace && getState().activeSessionId === sid) {
          const flush = firstAsks.splice(0)
          for (const t of flush) getState().ask(t.text, t.contents)
        }
      }
      break
    }
      case 'llm_start': {
      const sid = getSid(raw, getState().activeSessionId)
      const runId = raw.run_id ? String(raw.run_id) : undefined
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        if (runId && isSubAgentRun(v, runId)) {
          // A retry starts a new LLM attempt. Keep the historical count and
          // clear the transient error only when the next attempt actually starts.
          const blocks = v.blocks.map((b) =>
            b.kind === 'agent' && b.runId === runId
              ? {
                  ...b,
                  retrying: b.retrying === true,
                  ...(b.retrying ? { status: 'running' as const } : {}),
                  // A successful attempt is the only event that clears the
                  // previous card-local retry message; llm_start just marks it running.
                }
              : b,
          )
          return setView(s, sid, { ...v, blocks })
        }
        // 新轮起始：重置主流思考标记（中断残留的 thinkStartTs/turnThinkMs 不能串到本轮）
        const thinkStartTs = { ...v.thinkStartTs }
        const turnThinkMs = { ...v.turnThinkMs }
        delete thinkStartTs['']
        delete turnThinkMs['']
        // 记本 LLM 轮起始时刻（llm_end 差分得每轮耗时；子 agent 轮 best-effort）。
        // 用事件 timestamp（重放可重建）；缺省兜底 Date.now()（旧日志/无时间戳事件）。
        const ts = evTs(raw) ?? Date.now()
        return setView(s, sid, { ...pushAssistant(v, raw.model as string | undefined, raw.request_id ? String(raw.request_id) : undefined, ts, runId), turnStartTs: ts, thinkStartTs, turnThinkMs })
      })
      break
    }
    case 'reasoning_chunk': {
      // 注：chunk 事件通常已在 dispatchEvent 入口被缓冲合并（materializing 或 flush 时同步处理到这里）
      const sid = getSid(raw, getState().activeSessionId)
      const c = String(raw.content || '')
      const runId = raw.run_id ? String(raw.run_id) : undefined
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        // 中断防御：主 run 已被 abort，忽略其后的残留 reasoning（后端已切断，此处兜底）
        if (v.aborted && !(runId && isSubAgentRun(v, runId))) return s
        // 思考开始时刻：首个 reasoning_chunk 的事件 timestamp 记（key = run_id，'' = 主 agent；
        // 并发子 agent 互不干扰）——「接收第一个字符」起算，reasoning 结束在 content_chunk/llm_end 结算。
        // 块上的 thinkStart 仅用于「是否在思考」的 running 判定；实时计时器由 Thinking 组件
        // 自己用本地基准起跳（不依赖 thinkStart 的值域，见 Narrative.tsx Thinking）。
        const key = runId ?? ''
        const thinkStartTs = { ...v.thinkStartTs }
        const ts = evTs(raw) ?? Date.now()
        if (thinkStartTs[key] == null) thinkStartTs[key] = ts // 事件时间戳 → 结算 thinkMs（重放可重建）
        const startClient = Date.now() // 客户端接收时刻 → 块 thinkStart（实时时钟锚点，重放不虚高）
        if (runId && isSubAgentRun(v, runId)) {
          return setView(s, sid, { ...v, blocks: appendAgentStream(v.blocks, runId, 'thinking', c, startClient), thinkStartTs })
        }
        const bid = raw.request_id ? v.reqBlock[String(raw.request_id)] : undefined
        const nv = appendToAssistant(v, bid, 'thinking', c, startClient)
        // TTFT（问题七）：首个任意 chunk（reasoning 或 content）到达 = 本轮首字耗时。
        // 思考模型 reasoning 先行——此前只在 content_chunk 结算，TTFT 被整段思考虚高。
        nv.ttftMs = nv.ttftMs ?? (nv.llmStartTs ? ts - nv.llmStartTs : undefined)
        const idxT = bid != null ? nv.blocks.findIndex((b) => b.id === bid) : -1
        if (idxT >= 0 && nv.blocks[idxT].kind === 'assistant') {
          const ab = nv.blocks[idxT] as Extract<MsgBlock, { kind: 'assistant' }>
          nv.blocks[idxT] = { ...ab, ttftMs: ab.ttftMs ?? nv.ttftMs }
        }
        return setView(s, sid, { ...nv, thinkStartTs })
      })
      break
    }
    case 'content_chunk': {
      // 注：chunk 事件通常已在 dispatchEvent 入口被缓冲合并
      const sid = getSid(raw, getState().activeSessionId)
      const c = String(raw.content || '')
      const runId = raw.run_id ? String(raw.run_id) : undefined
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        // 中断防御：主 run 已被 abort，忽略其后的残留 content（后端已切断，此处兜底）
        if (v.aborted && !(runId && isSubAgentRun(v, runId))) return s
        // 首个文本 token 到达 = 思考结束：结算思考耗时（挂块 + 记 turnThinkMs 供 llm_end 并入指标）。
        // 起点/终点都用事件 timestamp（重放可重建）；缺失回退 Date.now()。
        const now = evTs(raw) ?? Date.now()
        const key = runId ?? ''
        const started = v.thinkStartTs[key]
        const thinkMs = started != null ? now - started : undefined
        const thinkStartTs = { ...v.thinkStartTs }
        const turnThinkMs = { ...v.turnThinkMs }
        if (started != null) {
          delete thinkStartTs[key]
          turnThinkMs[key] = thinkMs as number
        }
        if (runId && isSubAgentRun(v, runId)) {
          const blocks = thinkMs != null
            ? setAgentThinkMs(appendAgentStream(v.blocks, runId, 'text', c), runId, thinkMs)
            : appendAgentStream(v.blocks, runId, 'text', c)
          return setView(s, sid, { ...v, blocks, thinkStartTs, turnThinkMs })
        }
        const bid = raw.request_id ? v.reqBlock[String(raw.request_id)] : undefined
        const nv = appendToAssistant(v, bid, 'text', c)
        nv.ttftMs = nv.ttftMs ?? (nv.llmStartTs ? now - nv.llmStartTs : undefined)
        const textIdx = bid != null ? nv.blocks.findIndex((b) => b.id === bid) : -1
        if (textIdx >= 0 && nv.blocks[textIdx].kind === 'assistant') {
          const ab = nv.blocks[textIdx] as Extract<MsgBlock, { kind: 'assistant' }>
          nv.blocks[textIdx] = { ...ab, ttftMs: ab.ttftMs ?? nv.ttftMs }
        }
        const blocks = thinkMs != null ? setAssistantThinkMs(nv.blocks, bid, thinkMs) : nv.blocks
        return setView(s, sid, { ...nv, blocks, thinkStartTs, turnThinkMs })
      })
      break
    }
    case 'llm_end': {
      const sid = getSid(raw, getState().activeSessionId)
      const ru = (raw.usage as RawUsage | undefined) || {}
      const runId = raw.run_id ? String(raw.run_id) : undefined
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        const isSub = runId ? isSubAgentRun(v, runId) : false
        // 思考耗时：优先取 content_chunk 已结算的 turnThinkMs（首个文本 token 为终点）；
        // 未结算（想到最后 / 无正文直接工具）→ 在此以 llm_end 结算。两者都清标记。
        // 全部基于事件 timestamp（重放可重建耗时）。
        const now = evTs(raw) ?? Date.now()
        const key = runId ?? ''
        const started = v.thinkStartTs[key]
        const thinkStartTs = { ...v.thinkStartTs }
        const turnThinkMs = { ...v.turnThinkMs }
        const thinkMs = turnThinkMs[key] ?? (started != null ? now - started : undefined)
        if (started != null) delete thinkStartTs[key]
        if (turnThinkMs[key] != null) delete turnThinkMs[key]
        const g = {
          input: v.usage.input + (ru.Input ?? 0),
          output: v.usage.output + (ru.Output ?? 0),
          cacheRead: v.usage.cacheRead + (ru.CacheRead ?? 0),
          cacheWrite: v.usage.cacheWrite + (ru.CacheWrite ?? 0),
          cacheWrite1h: v.usage.cacheWrite1h + (ru.CacheWrite1h ?? 0),
          reasoning: v.usage.reasoning + (ru.Reasoning ?? 0),
        }
        // 每轮指标：llm_end 一条（含主/子 agent），差分耗时时长 + 思考耗时 + 桥打标 cost
        const durMs = v.turnStartTs ? now - v.turnStartTs : undefined
        const cost = typeof raw.cost_usd === 'number' ? raw.cost_usd : 0
        const turn: TurnMetric = {
          model: String(raw.model || ''),
          input: ru.Input ?? 0,
          output: ru.Output ?? 0,
          cacheRead: ru.CacheRead ?? 0,
          cacheWrite: ru.CacheWrite ?? 0,
          reasoning: ru.Reasoning ?? 0,
          durMs,
          thinkMs,
          costUsd: cost,
          runId,
        }
        const turns = [...v.turns, turn]
        const costUsd = v.costUsd + cost
        const reqBlock = { ...v.reqBlock }
        if (raw.request_id) delete reqBlock[String(raw.request_id)]
        // A sub-agent completion must never clear a main-agent error block.
        // Main-run success still clears its own retry/error presentation.
        const blocks = isSub ? [...v.blocks] : v.blocks.filter((b) => b.kind !== 'error')
        if (isSub && runId) {
          // 子 agent：只归集用量到其卡片（tokens 实时累计），并在成功轮次
          // 清除卡片内临时 retry 状态；历史失败文本仍保留在卡片活动里。
          const ai = blocks.findIndex((b) => b.kind === 'agent' && b.runId === runId)
          if (ai >= 0) {
            const ag = blocks[ai] as Extract<MsgBlock, { kind: 'agent' }>
            blocks[ai] = {
              ...ag,
              retrying: false,
              retryMessage: undefined,
              ...(thinkMs != null ? { thinkMs } : {}),
              usage: {
                input: ag.usage.input + (ru.Input ?? 0),
                output: ag.usage.output + (ru.Output ?? 0),
                cacheRead: ag.usage.cacheRead + (ru.CacheRead ?? 0),
                cacheWrite: ag.usage.cacheWrite + (ru.CacheWrite ?? 0),
                cacheWrite1h: ag.usage.cacheWrite1h + (ru.CacheWrite1h ?? 0),
                reasoning: ag.usage.reasoning + (ru.Reasoning ?? 0),
                // 卡片成本：按轮累加（桥已按价表打标）；任务结束（task_end）时被
                // 任务全量值覆盖（含压缩轮 —— 压缩不发 llm_end，增量会少算）
                ...(cost > 0 || (ag.usage.costUsd ?? 0) > 0 ? { costUsd: (ag.usage.costUsd ?? 0) + cost } : {}),
              },
            }
          }
          return setView(s, sid, { ...v, blocks, reqBlock, usage: g, turns, costUsd, turnStartTs: undefined, thinkStartTs, turnThinkMs })
        }
        const bid = raw.request_id ? v.reqBlock[String(raw.request_id)] : v.lastLLMBlockId
        const idx = bid != null ? blocks.findIndex((b) => b.id === bid) : -1
        // 重放兜底（与 Go view_reducer.applyLLMEndLocked 同口径）：**事件日志不落盘流式
        // chunk**（bridge isStreamingChunkType 过滤，省 ~78.5% 体积），历史重放只剩 llm_end
        // —— 它携带本轮最终正文/思考。块里为空时用它们补全（已有 chunk 的不覆盖）。
        // 缺这层兜底时：一切走 legacy snapshot 恢复（restored=snapshot）的会话，assistant 块
        // text/thinking 全空 → Narrative 按「空且非流式」跳过 → 对话只剩工具/产出块
        //（2026-09-23 现场：「完全看不到 Reasoning、Content」）。
        const settleAssistant = (b: Extract<MsgBlock, { kind: 'assistant' }>): Extract<MsgBlock, { kind: 'assistant' }> => ({
          ...b,
          streaming: false,
          model: (raw.model as string) || b.model,
          ...(b.text ? {} : { text: String(raw.content ?? '') }),
          ...(b.thinking ? {} : { thinking: String(raw.reasoning ?? '') }),
        })
        if (idx >= 0 && blocks[idx].kind === 'assistant') {
          blocks[idx] = settleAssistant(blocks[idx] as Extract<MsgBlock, { kind: 'assistant' }>)
        } else {
          const last = blocks[blocks.length - 1]
          if (last && last.kind === 'assistant') blocks[blocks.length - 1] = settleAssistant(last)
        }
        const withThink = thinkMs != null ? setAssistantThinkMs(blocks, bid, thinkMs) : blocks
        // 主 agent：当前上下文占用 = 本轮模型实际输入（system + 有效上下文 + 最新输入，
        // 压缩后自动变小）；子 agent 的不进主窗口
        // 用量 0 视为缺数（上游未回 usage）：保留上一轮占用，不清零（问题九防御纵深）
        const ctxHasUsage = typeof ru.Input === 'number' && ru.Input > 0
        const ctxTokens = ctxHasUsage ? ru.Input : v.ctxTokens
        // provider 真实报数 → 口径回到「真实」（清掉压缩后的 ≈ 估算标记）
        const ctxTokensEstimated = ctxHasUsage ? false : v.ctxTokensEstimated
        // 非工具调用的最终文本回复在消息底部展示本轮 TTFT 与总耗时；
        // 纯 ToolCall 回复没有可展示的文本，不附加到消息。
        // 注意：最终块编辑与落库必须基于 withThink（已含思考结算的数组），否则
        // 「纯思考→工具调用」轮的 thinkMs 结算会被丢弃 → 思考计时器永不停止。
        const finalBid = bid
        const finalIdx = finalBid != null ? withThink.findIndex((b) => b.id === finalBid) : -1
        if (finalIdx >= 0 && withThink[finalIdx].kind === 'assistant' && (withThink[finalIdx] as Extract<MsgBlock, { kind: 'assistant' }>).text) {
          const ab = withThink[finalIdx] as Extract<MsgBlock, { kind: 'assistant' }>
          // ttftMs：块上 content_chunk 已结算则沿用；缺失（无 request_id 的旧流 /
          // chunk 缓冲时序）兜底用 llm_start → llm_end 差分 —— 保证消息底部
          // TTFT/耗时行稳定显示。
          // outputTokens：本轮输出 token（usage.Output，**含 reasoning**）——消息底部
          // 展示输出速度（tokens/s）用。仅主回复轮挂（有 text 才展示）。
          // genMs：分母（生成窗口）——口径见 genMsOf 注释（思考未流式时必须用整轮 durMs）。
          const reasoningTok = ru.Reasoning ?? 0
          // 摘要化思考（Anthropic display=summarized）：provider 标记进 usage，落块以便
          // checkpoint 重放时 genMsOf 用同一口径重算（刷新前后 tokens/s 不跳变）。
          const thinkingSummarized = ru.ReasoningSummarized === true
          const ttft = ab.ttftMs ?? (v.llmStartTs != null ? now - v.llmStartTs : undefined)
          const genMs = genMsOf({ thinking: ab.thinking, durMs, ttftMs: ttft, reasoningTokens: reasoningTok, thinkingSummarized })
          withThink[finalIdx] = { ...ab, ttftMs: ttft, durMs, outputTokens: ru.Output ?? 0, reasoningTokens: reasoningTok, thinkingSummarized, genMs }
        }
        return setView(s, sid, { ...v, blocks: withThink, reqBlock, streaming: false, llmStartTs: undefined, turnStartTs: undefined, usage: g, turns, costUsd, thinkStartTs, turnThinkMs, ctxTokens, ctxTokensEstimated })
      })
      // LLM End：把暂存队列整队一次性推送到服务端（下一个 LLM Start 前经插话 Poll /
      // takeBatch 成批消费并回 UserInputsConsumed 确认）。幂等：仅推未推送项。
      getState().flushPending(sid)
      // issue 6：主 agent 一轮结束（新会话已由服务端生成自动标题 / 更新 updatedAt）→
      // 刷新会话列表，让左侧 session 管理的名字与排序即时更新（子 agent 轮不刷）。
      const v = getState().views[sid]
      const isSubRun = runId ? isSubAgentRun(v, runId) : false
      if (!isSubRun) send({ type: 'list', payload: {} })
      break
    }
    // llm_error：LLM 调用失败（SDK 重试）。will_retry=true 时叙述通道显示重试进度块
    //（原位更新，多次重试只留一条）；最终失败由 session_run_error 接管为最终错误。
    // 2026-09-18 文案/状态改版：Attempt 是**已失败的那一次**，旧的「正在重试 1…」在下一次
    // 尝试挂住时（实测 187s 无首字）被读成「卡住/重试没了」→ 改为「第 N/M 次失败 →
    // 重试第 N+1/M 次」，并把 ts/retryDelayMs 落进块里，由 Narrative 的 RetryProgress
    // 实时渲染「退避倒计时 → 本次尝试已进行 Xs」。挂死时这一行是唯一能证明「在重试」的信息。
    // 2026-08-22：error_kind=context_exceeded → 专门语义（上下文超限终止，不再重试）：
    //   - 子 agent：错误挂到其卡片活动记录（醒目红色项），不污染主叙述；
    //   - 主 run：叙述通道专用文案（含"为什么结束/怎么办"），并复位流式态。
    case 'llm_error': {
      const sid = getSid(raw, getState().activeSessionId)
      const msg = String(raw.message || 'LLM 调用失败')
      const willRetry = raw.will_retry !== false
      const attempt = Number(raw.attempt ?? 0)
      const maxAttempts = Number(raw.max_attempts ?? 0)
      const retryDelayMs = Number(raw.retry_delay_ms ?? 0)
      // 失败尝试的已产生成本（2026-09-23）：桥按价表折算后随事件 cost_usd 下发 ——
      // 会话成本累计（与 llm_end 同口径）+ 重试块显示 + 子 agent 卡片成本。
      const attemptCost = typeof raw.cost_usd === 'number' ? raw.cost_usd : 0
      const errorKind = String(raw.error_kind || '')
      const runId = raw.run_id ? String(raw.run_id) : undefined
      const isContextExceeded = errorKind === 'context_exceeded'
      const failedLabel = maxAttempts > 0 && attempt > 0 ? `第 ${attempt}/${maxAttempts} 次尝试失败` : `第 ${attempt} 次尝试失败`
      const text = isContextExceeded
        ? `（LLM 上下文超限终止）${msg}`
        : willRetry
          ? `（LLM 请求失败：${failedLabel} → 重试第 ${attempt + 1}${maxAttempts > 0 ? `/${maxAttempts}` : ''} 次）${msg}`
          : errorKind === 'upstream_unavailable'
            ? `（上游模型服务连续不可用，${failedLabel}、重试已耗尽后终止；这是供应商侧瞬时故障，不是你的请求造成的，稍后重试即可）${msg}`
            : `（LLM 请求失败：${failedLabel}、重试已耗尽）${msg}`
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        // 子 agent 的 LLM 错误：挂到其卡片活动记录（卡片终态一并收敛；上下文超限 = failed）
        if (runId && isSubAgentRun(v, runId)) {
          const agentBlock = v.blocks.find((b) => b.kind === 'agent' && b.runId === runId)
          const retryCount = Math.max(agentBlock?.kind === 'agent' ? (agentBlock.retryCount ?? 0) : 0, attempt)
          const blocks = v.blocks.map((b) =>
            b.kind === 'agent' && b.runId === runId
              ? {
                  ...b,
                  retrying: willRetry && !isContextExceeded,
                  retryMessage: willRetry && !isContextExceeded ? text : undefined,
                  items: upsertAgentErrorItem(b.items, text),
                  usage: { ...b.usage, costUsd: (b.usage?.costUsd ?? 0) + attemptCost },
                  retryCount,
                  // 总次数：卡片徽章显示「重试 N/M」（与主叙述同口径）
                  ...(maxAttempts > 0 ? { retryMax: maxAttempts } : {}),
                  ...(isContextExceeded ? { taskError: msg, status: 'failed' as const } : willRetry ? { status: 'running' as const } : { status: 'failed' as const, taskError: msg }),
                }
              : b,
          )
          const upd = setView(s, sid, {
            ...v,
            blocks,
            costUsd: v.costUsd + attemptCost, // 子 agent 的重试浪费同样进会话成本（与 llm_end 同口径）
          })
          return { ...upd, busySids: busySidsFromViews(upd.views) }
        }
        // 重试会整轮重发（新 LLMStart 新建 assistant 块）：丢弃失败尝试的流式残留块。
        // mid-stream 失败（如 HTTP/2 INTERNAL_ERROR）已渲染半截文本 + streaming 常 true，
        // 不清理会留「半截消息 + 永不结束的流式标记」的僵尸块；connect 失败无流式块 → 过滤无操作。
        // 只删 streaming 块：已结算（llm_end 落地）的块 streaming=false，绝不动。
        let blocks = v.blocks
        if (willRetry) {
          let removed = false
          blocks = blocks.filter((b) => {
            if (!removed && b.kind === 'assistant' && b.streaming) {
              removed = true
              return false
            }
            return true
          })
        }
        // 重试期保持「停止」可用：建连失败的重试循环往往没有 llm_start（streaming 仍是 false，
        // 发送钮显示「发送」而非「停止」→ 用户无法终止重试）。此处镜像服务端退避/重试状态：
        // will_retry=true 期间 streaming 恒 true（点停止走 interrupt，后端 ctx abort 立即切断）；
        // 最终失败将 streaming 复位，等 session_run_error 收敛为错误块。
        // 上下文超限：终止性错误（后端已停止，不会再有 session_run_error 级联重试块——
        // 实际 session_run_error 仍会收敛，此处先复位流式态让 UI 不再显示"重试中"）。
        // 重试进度（ts/retryDelayMs/attempt/max）随块落库：RetryProgress 组件据此做
        // 退避倒计时与「本次尝试已进行 Xs」—— 只有 ts 与 delay 两个静态值，实时数字在渲染层算。
        return setView(s, sid, {
          ...v,
          costUsd: v.costUsd + attemptCost, // 失败尝试的已产生成本计入会话累计（重试浪费不丢）
          blocks: upsertErrorBlock(blocks, text, {
            ts: evTs(raw) ?? Date.now(),
            retrying: willRetry && !isContextExceeded,
            retryAttempt: attempt,
            ...(maxAttempts > 0 ? { retryMax: maxAttempts } : {}),
            ...(retryDelayMs > 0 ? { retryDelayMs } : {}),
            ...(attemptCost > 0 ? { retryCostUsd: attemptCost } : {}),
          }),
          streaming: v.aborted ? false : isContextExceeded ? false : willRetry ? true : false,
        })
      })
      break
    }
    // SDK 事件名 = tool_run_start/tool_run_end（tool_start/tool_response 为旧别名，兼容保留）
    case 'tool_run_start':
    case 'tool_start': {
      const sid = getSid(raw, getState().activeSessionId)
      const toolId = String(raw.id || '')
      const runId = raw.run_id ? String(raw.run_id) : undefined
      const name = String(raw.name || '')
      const args = String(raw.arguments || '')
      const taskId = String(raw.task_id || '') // 工具任务 id（promote 关联锚；无 = 旧版/无批任务）
      const startTs = evTs(raw) ?? Date.now() // 工具开始时刻（事件时间戳，结算 durMs 用，重放可重建）
      const startClient = Date.now() // 客户端接收时刻（实时时钟锚点：从「接收到 Start」起走，重放不虚高）
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        const isSub = runId ? isSubAgentRun(v, runId) : false
        const tid = taskId || undefined
        // 同 toolId 幂等去重：重复投递的 start（重连补发/日志重放尾段/新旧别名并存）
        // 只更新原块，不再追加副本 —— 否则同一次调用会渲染 N 张同内容卡片（计划卡、
        // 任务卡均翻倍），链路树节点同步重复。
        const dupMain = !isSub && v.blocks.some((b) => b.kind === 'tool' && b.toolId === toolId)
        const upd = setView(s, sid, {
          ...v,
          blocks: isSub && runId
            ? v.blocks.map((b) => {
                if (!(b.kind === 'agent' && b.runId === runId)) return b
                const dup = b.items.some((it) => it.kind === 'tool' && it.toolId === toolId)
                return dup
                  ? b
                  : { ...b, items: [...b.items, { kind: 'tool', toolId, name, args, status: 'running', startedAt: startClient, ...(tid ? { taskId: tid } : {}) } as AgentItem] }
              })
            : dupMain
              ? v.blocks.map((b) => (b.kind === 'tool' && b.toolId === toolId ? { ...b, name, args, ...(tid ? { taskId: tid } : {}) } : b))
              : [...v.blocks, { kind: 'tool', id: blockSeq++, toolId, name, args, status: 'running', startedAt: startClient, ...(tid ? { taskId: tid } : {}) }],
          toolStartTs: { ...v.toolStartTs, [toolId]: startTs },
          runNodes: runId
            ? (v.runNodes.some((n) => n.id === `tool-${toolId}`)
                ? v.runNodes
                : [...v.runNodes, { id: `tool-${toolId}`, parentId: runId, label: toolNodeLabel(name, args), status: 'running', depth: 0, kind: 'tool', name, args, startedAt: startClient, ...(tid ? { taskId: tid } : {}) }])
            : v.runNodes,
          // 桌面动作运行中指示：computer_* 工具执行期间置 desktopTool（状态栏「正在操作桌面」），
          // tool_run_end 清除。语义工具全部串行（CanParallel=false），后到覆盖先到即可。
          desktopTool: name.startsWith('computer_') ? name : v.desktopTool,
        })
        return { ...upd, busySids: busySidsFromViews(upd.views) }
      })
      // §6.9：Browser 工具**开始时**自动打开右侧 web 栏（已打开不重复；收起后重开）。
      // 排除 list_pages/take_screenshot（面板自身数据源，非用户可见动作）。
      const btName0 = String(raw.name || '')
      if (btName0.startsWith('browser_') && !/browser_(list_pages|take_screenshot)$/.test(btName0)) {
        // 2026-09-20：传 sid → 只有工具所属会话 == 当前会话才展开（后台会话的 Agent 不该掀开
        // 用户正在看的面板；见 focusBrowserForUse 的注释与 test:right-tabs 的 F9-F12）。
        getState().focusBrowserForUse(sid)
      }
      break
    }
    case 'im_bind_confirm_requested': {
      // IM 陌生聊天首次消息 → 桌面端绑定确认弹窗（data: {chat, user_id, text}）
      const bd = raw.data as { chat?: IMChat; user_id?: string; text?: string } | undefined
      if (bd?.chat) {
        setState({
          imBindPending: {
            chat: bd.chat,
            userId: bd.user_id ?? '',
            text: bd.text ?? '',
          },
        })
      }
      break
    }
    case 'tool_approval_requested': {
      const sid = getSid(raw, getState().activeSessionId)
      const approvalId = String(raw.id || '')
      if (!approvalId) break
      const runId = raw.run_id ? String(raw.run_id) : undefined
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        // The backend owns the Session-level FIFO. The renderer preserves that
        // order and only deduplicates replay/duplicate delivery by call id.
        if (v.approvals.some((a) => a.id === approvalId)) return s
        const isActive = sid === s.activeSessionId
        return setView(s, sid, {
          ...v,
          approvals: [
            ...v.approvals,
            {
              id: approvalId,
              name: String(raw.name || ''),
              arguments: String(raw.arguments || ''),
              index: Number(raw.index ?? 0),
              runId,
              diff: pickDiff(raw.diff),
              timeoutMs: typeof raw.timeout_ms === 'number' && raw.timeout_ms > 0 ? raw.timeout_ms : undefined,
              arrivedAt: Date.now(),
            },
          ],
          // Only a SubAgent-originated HITL may take over the active right rail.
          // Main-agent approvals keep the existing modal behavior; both kinds
          // still live in the same Session-level FIFO queue.
          ...((isActive && Boolean(runId) && isSubAgentRun(v, runId ?? ''))
            ? { rightExpanded: true, rightTab: 'agent' as const }
            : {}),
        })
      })
      break
    }
    case 'compress_start': {
      const sid = getSid(raw, getState().activeSessionId)
      const runId = raw.run_id ? String(raw.run_id) : undefined
      const current = getState().views[sid] ?? emptyView()
      const isSub = Boolean(runId && isSubAgentRun(current, runId))
      const startedAt = evTs(raw) ?? Date.now()
      armCompressWatchdog(sid, isSub ? runId : undefined)
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        if (isSub && runId) {
          const before = Number(raw.before ?? 0)
          const blocks = upsertAgentCompression(v.blocks, runId, {
            kind: 'compression',
            before,
            after: before,
            reason: raw.reason ? String(raw.reason) : undefined,
            active: true,
            startedAt,
          })
          return setView(s, sid, { ...v, blocks })
        }
        return setView(s, sid, {
          ...v,
          compressing: true,
          compressStartTs: startedAt,
          compressBefore: Number(raw.before ?? 0),
          compressRunId: runId,
          compressReason: raw.reason ? String(raw.reason) : undefined,
        })
      })
      break
    }
    case 'compress_end': {
      // The SDK normally emits this for both success and failure. Disarm only
      // the matching session/run watchdog; a concurrent SubAgent must survive.
      const sid = getSid(raw, getState().activeSessionId)
      const runId = raw.run_id ? String(raw.run_id) : undefined
      const current = getState().views[sid] ?? emptyView()
      const isSub = Boolean(runId && isSubAgentRun(current, runId))
      disarmCompressWatchdog(sid, isSub ? runId : undefined)
      const err = raw.error ? String(raw.error) : ''
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        if (isSub && runId) {
          const agent = v.blocks.find((b) => b.kind === 'agent' && b.runId === runId)
          const activeItem = agent?.kind === 'agent'
            ? [...agent.items].reverse().find((it) => it.kind === 'compression' && it.active)
            : undefined
          const startedAt = agent?.kind === 'agent' && agent.compressStartTs != null
            ? agent.compressStartTs
            : activeItem?.kind === 'compression' ? activeItem.startedAt : undefined
          const ts = evTs(raw) ?? Date.now()
          const aborted = Boolean(raw.aborted) || raw.finish_reason === 'abort'
          const before = Number(raw.before ?? (activeItem?.kind === 'compression' ? activeItem.before : 0))
          const after = Number(raw.after ?? before)
          const ctxTokens = typeof raw.ctx_tokens === 'number' ? raw.ctx_tokens : undefined
          // 压缩调用用量（含成本）：与任务卡/工具行同一投影（taskUsageFromEvent）
          const usage = taskUsageFromEvent(raw.usage as RawUsage | undefined)
          const item: AgentCompressionItem = {
            kind: 'compression',
            before,
            after,
            ctxTokens,
            reason: raw.reason ? String(raw.reason) : undefined,
            model: raw.model ? String(raw.model) : undefined,
            summary: raw.summary ? String(raw.summary) : undefined,
            analysis: raw.analysis ? String(raw.analysis) : undefined,
            usage,
            error: aborted ? undefined : (err || undefined),
            aborted: aborted || undefined,
            active: false,
            ...(startedAt != null ? { startedAt } : {}),
            durMs: startedAt != null ? Math.max(0, ts - startedAt) : undefined,
          }
          const blocks = upsertAgentCompression(v.blocks, runId, item)
          return setView(s, sid, { ...v, blocks })
        }
        // Main Agent compression remains a first-class conversation block;
        // the right Task tab mirrors that same block below.
        const aborted = Boolean(err) && Boolean(v.aborted)
        const todos = Array.isArray(raw.todos) ? (raw.todos as TodoItem[]) : v.todos
        const rawCtxTokens = typeof raw.ctx_tokens === 'number' ? raw.ctx_tokens : undefined
        const ctxTokens = rawCtxTokens ?? v.ctxTokens
        // 压后 ctx_tokens 来自 SDK 字符/token 估算（非 provider 真实 usage）→ 标注 ≈
        const ctxTokensEstimated = rawCtxTokens != null ? true : v.ctxTokensEstimated
        // 压缩调用用量（含成本）：同一投影（压缩成本此前只在会话累计里体现）
        const usage = taskUsageFromEvent(raw.usage as RawUsage | undefined)
        const ts = evTs(raw) ?? Date.now()
        const startedAt = v.compressStartTs
        const durMs = startedAt != null ? Math.max(0, ts - startedAt) : undefined
        return setView(s, sid, {
          ...v,
          compressing: false,
          compressStartTs: undefined,
          compressBefore: undefined,
          compressRunId: undefined,
          compressReason: undefined,
          todos,
          ctxTokens,
          ctxTokensEstimated,
          blocks: [...v.blocks, {
            kind: 'compression',
            id: blockSeq++,
            before: Number(raw.before ?? 0),
            after: Number(raw.after ?? 0),
            ctxTokens,
            reason: raw.reason ? String(raw.reason) : undefined,
            model: raw.model ? String(raw.model) : undefined,
            summary: raw.summary ? String(raw.summary) : undefined,
            analysis: raw.analysis ? String(raw.analysis) : undefined,
            usage,
            error: aborted ? undefined : (err || undefined),
            aborted: aborted || undefined,
            durMs,
            runId,
          }],
        })
      })
      break
    }
    case 'command_result': {
      // 命令结果：skills 加载 → 工具调用样式块（SKILL(code-review)）；clear → 重置该会话上下文；
      // 其余命令回退 assistant 文本。
      const sid = getSid(raw, getState().activeSessionId)
      const name = String(raw.name || '')
      const isErr = Boolean(raw.error)
      const text = isErr ? String(raw.error) : String(raw.result ?? '')
      if (!text.trim()) break
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        if (name === 'clear') {
          // 清空上下文：会话视图（叙述/链路/耗时）重置为全新，待办与待发送保留
          //（会话 todo 不随上下文清空；pendingQueue 已推送服务端，清空后仍会消费进新上下文）
          disarmSessionCompressWatchdogs(sid) // 上下文清空：取消该会话全部压缩看门狗
          return setView(s, sid, {
            ...emptyView(),
            todos: v.todos,
            pendingQueue: v.pendingQueue,
            // 清空上下文：不推「（上下文已清空）」文本，插一条 divider（横线）作上下文隔离
            blocks: [{ kind: 'divider', id: blockSeq++, reason: 'clear', ts: Date.now() }],
          })
        }
        if (name === 'skills') {
          const skillName = text.match(/skill "([^"]+)"/)?.[1] ?? ''
          if (skillName) {
            const bid = blockSeq++
            return setView(s, sid, {
              ...v,
              blocks: [...v.blocks, {
                kind: 'tool', id: bid, toolId: `cmd-${bid}`,
                name: 'SKILL', args: skillName, isError: isErr,
                status: 'done', result: text,
              }],
            })
          }
        }
        if (name === 'reload_skills') {
          return setView(s, sid, { ...v, blocks: [...v.blocks, { kind: 'system', id: blockSeq++, text, tone: isErr ? 'error' : 'success' }] })
        }
        return setView(s, sid, { ...v, blocks: [...v.blocks, { kind: 'assistant', id: blockSeq++, text, streaming: false, thinking: '' }] })
      })
      break
    }
    // —— 定时任务事件：增删改/触发/运行态变化 → 实时刷新 CronPage ——
    case 'cron_changed': // 任务增删改（LLM 工具或宿主操作后）
      setState({ cronLoading: false })
      send({ type: 'cron_list', payload: {} })
      break
    case 'cron_fired': // 到点触发（新建派生会话）
      send({ type: 'cron_list', payload: {} }) // 刷新 next_run/run_count
      break
    case 'cron_run_status': // 某次运行状态变化 → 刷新任务态（账本由页面主动拉）
      send({ type: 'cron_list', payload: {} })
      break
    case 'ask_user_question': {
      // 模型→用户提问批次（ask_user 工具）：记录待答问题，UI 停靠提问面板展示
      //（仅活跃会话镜像到顶层）。timeout_ms/ts 供面板显示「剩余决策时间」倒计时。
      const sid = getSid(raw, getState().activeSessionId)
      const rawItems = Array.isArray(raw.questions)
        ? (raw.questions as { id?: unknown; question?: unknown; options?: unknown; multi?: unknown }[])
        : []
      const timeoutMs = typeof raw.timeout_ms === 'number' && raw.timeout_ms > 0 ? raw.timeout_ms : 0
      const evTs = typeof raw.timestamp === 'string' ? Date.parse(raw.timestamp) : NaN
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        return setView(s, sid, {
          ...v,
          question: {
            id: String(raw.id || ''),
            runId: raw.run_id ? String(raw.run_id) : undefined,
            timeoutMs,
            // 提问时刻：优先事件时间戳（与后端计时同源），缺失时回退接收时刻
            ts: Number.isFinite(evTs) ? evTs : Date.now(),
            items: rawItems.map((it) => ({
              id: String(it.id ?? ''),
              question: String(it.question ?? ''),
              // 选项兼容 {"label","preview"} 与纯字符串（历史事件/回放）——归一化在 lib 里
              options: parseQuestionOptions(it.options),
              multi: it.multi === true,
            })),
          },
        })
      })
      break
    }
    case 'sandbox_blocked': {
      // 沙箱拦截（seatbelt EPERM/deny）→ 弹放行确认（还原命令摘要；用户应答后随 bash 工具结束清除）
      const sid = getSid(raw, getState().activeSessionId)
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        return setView(s, sid, { ...v, release: { id: String(raw.id || sid), hint: String(raw.hint ?? '') } })
      })
      break
    }
    case 'tool_run_end':
    case 'tool_response': {
      const sid = getSid(raw, getState().activeSessionId)
      const toolId = String(raw.id || '')
      const runId = raw.run_id ? String(raw.run_id) : undefined
      const errorInfo = pickToolError(raw)
      const toolResult = String(raw.result ?? '')
      // 本次调用用量（ToolResponse.Usage）：仅子 agent 类工具（ToolUsageProvider）非零——
      // 普通工具不花模型钱，全零 → undefined（不留空白用量位）。同源展示：主对话工具行
      // 与子 agent 卡片内的工具项都据此显示 tokens + 成本。
      const callUsage = taskUsageFromEvent(raw.usage as RawUsage | undefined)
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        const started = v.toolStartTs[toolId]
        const durMs = started ? (evTs(raw) ?? Date.now()) - started : undefined
        const isSub = runId ? isSubAgentRun(v, runId) : false
        const blocks = isSub && runId
          ? v.blocks.map((b) =>
              b.kind === 'agent' && b.runId === runId
                ? {
                    ...b,
                    items: b.items.map((it) =>
                      it.kind === 'tool' && it.toolId === toolId
                        ? { ...it, status: (raw.is_error ? 'error' : 'done') as Extract<AgentItem, { kind: 'tool' }>['status'], result: toolResult, errorInfo, durMs, diff: raw.diff ? pickDiff(raw.diff) : undefined, images: pickImages(raw.images), ...(callUsage ? { usage: callUsage } : {}) }
                        : it,
                    ),
                  }
                : b,
            )
          : v.blocks.map((b) => {
              if (!(b.kind === 'tool' && b.toolId === toolId)) return b
              // promoted 后台任务：工具运行结束即收敛任务态 —— task_end / task_result_delivered
              // 只是「回执」，可能迟到或因会话结束未达。此前只置 status='done' 不动 taskStatus，
              // 卡片读 taskStatus 永远停在「执行中/中断中」（计时器空转），角标读 status 却已
              // 归零 —— 列表与徽标互相矛盾。中断在途 → interrupted（取消回执即终态）；正常
              // 结束 → completed（后续回执若带来不同终态会覆盖，语义一致）。
              if (b.promoted && b.taskId && (b.status === 'running' || b.interrupting)) {
                return {
                  ...b,
                  status: 'done' as const,
                  result: toolResult,
                  diff: raw.diff ? pickDiff(raw.diff) : undefined,
                  images: pickImages(raw.images),
                  isError: Boolean(raw.is_error),
                  errorInfo,
                  durMs,
                  interrupting: false,
                  taskStatus: (b.interrupting ? 'interrupted' : 'completed') as AsyncTaskStatus,
                  ...(callUsage ? { usage: callUsage } : {}),
                  ...(Array.isArray(raw.todos) ? { todos: raw.todos as TodoItem[] } : {}),
                }
              }
              return {
                ...b,
                status: 'done' as const,
                result: toolResult,
                diff: raw.diff ? pickDiff(raw.diff) : undefined,
                images: pickImages(raw.images),
                isError: Boolean(raw.is_error),
                errorInfo,
                durMs,
                ...(callUsage ? { usage: callUsage } : {}),
                // todo_* 工具：定格「本次调用完成时刻」的待办快照（bridge 随事件携带）——
                // 历史计划卡据此渲染当时状态，而不是永远跟随 store 最新列表（否则每张
                // 卡都显示最终态，多次更新看起来一模一样）。
                ...(Array.isArray(raw.todos) ? { todos: raw.todos as TodoItem[] } : {}),
              }
            })
        const toolStartTs = { ...v.toolStartTs }
        delete toolStartTs[toolId]
        const upd = setView(s, sid, {
          ...v,
          blocks,
          toolStartTs,
          diffs: raw.diff ? [...v.diffs, pickDiff(raw.diff)!] : v.diffs,
          runNodes: v.runNodes.map((n) =>
            n.id === `tool-${toolId}`
              ? { ...n, status: (raw.is_error ? 'error' : 'done') as RunNode['status'], result: toolResult, isError: Boolean(raw.is_error), errorInfo, durMs }
              : n,
          ),
          todos: Array.isArray(raw.todos) ? (raw.todos as TodoItem[]) : v.todos,
          approvals: v.approvals.filter((a) => a.id !== toolId),
          // ask_user 工具结束（回答/超时/打断）→ 清除提问（modal 关闭；避免超时后残留）
          question: raw.name === 'ask_user' ? undefined : v.question,
          // bash 工具结束（放行/拒绝/超时均汇于此）→ 清除沙箱放行确认（modal 关闭）
          release: raw.name === 'bash' ? undefined : v.release,
          // computer_* 工具结束 → 清除「正在操作桌面」指示（只清同名，防跨工具误清）
          desktopTool: raw.name === v.desktopTool ? undefined : v.desktopTool,
        },
          // plan_submit → bridge 已自动切 auto（下一次 Run 生效），前端镜像模式 chip 防脱节
          raw.name === 'plan_submit' ? { mode: 'auto' } : undefined)
        return { ...upd, busySids: busySidsFromViews(upd.views) }
      })
      // 浏览器工具执行完 → 自动刷新 web tab 页列表 + 聚焦右侧 web tab。
      // 仅排除 list_pages/take_screenshot：面板自身经 bridge 命令拉页列表/截图，
      // 不产生引擎工具事件，排除只为防御。take_snapshot 不再排除——它是 agent
      // 驱动浏览器的首个动作，此刻展开让用户立即看到浏览器正被接管。
      const btName = String(raw.name || '')
      // 结构化产物路径（ToolResponse.Diff.path）—— 本段两个判断共用同一个来源：
      //   · git 快照刷新：run_python 只有带它才算「改了工作区」（见 gitMutationLevel）；
      //   · 可视类自动打开（下方）。
      // 失败调用不认 diff：is_error 时 diff 可能残留上一次的路径。
      const endDiff = raw.is_error ? undefined : pickDiff(raw.diff)
      const mutation = gitMutationLevel(btName, String(raw.arguments || ''), toolResult, Boolean(endDiff?.path))
      if (mutation !== 'none') scheduleGitRefresh(sid, mutation === 'history')
      if (btName.startsWith('browser_') && !/browser_(list_pages|take_screenshot)$/.test(btName)) {
        getState().fetchBrowserPages()
        // §6.9：focus 已移到 tool_run_start（执行开始时打开）；此处仅刷新页列表
      }
      // §6.10：写类工具产出**可视类**文件 → 自动打开/更新右侧栏（过程中就打开，
      // 用户能实时看到效果）。路径来自 ToolResponse.Diff.path —— 这是**唯一**结构化
      // 的产物路径来源（Go 侧 diff.path 由工具在执行点填写：相对工作区 / 工作区外绝对），
      // 而不是从 args/result 里正则猜文件名（run_python 的 args 是脚本源码、result 是
      // stdout，都不含可靠产物路径 —— 猜必然误判/漏判）。
      // 2026-09：run_python 产出可视文件也走这里（Go 侧把产出填进 Diff.path）——同文件
      // 再次产出由 autoOpenVisualFile 刷新已有标签，不会多开。
      if (endDiff?.path) autoOpenVisualFile(endDiff.path, sid)
      break
    }
    case 'user_inputs_consumed': {
      // 精确感知：这批用户输入已被服务端消费进本轮（SDK takeBatch / 插话 Poll）。
      // 前端把「待发送」中对应消息（FIFO 取最旧的 N 条）移入对话页，其余仍悬浮等待。
      // 2026-08-22：服务端事件随 user_contents 携带每条消息的完整内容块（含图片 data URL）——
      // 实时渲染与恢复重放共用同一数据源（快照重放时 pendingQueue 为空，据此重建图片附件，
      // 否则图片只在客户端暂存队列、重启后消失）。旧日志无该字段 → 回退队列 contents / 纯文本。
      const sid = getSid(raw, getState().activeSessionId)
      const texts = Array.isArray(raw.user_texts) ? (raw.user_texts as string[]) : []
      const contentGroups = Array.isArray(raw.user_contents)
        ? (raw.user_contents as { type?: string; content?: string; mime_type?: string }[][])
        : undefined
      if (!texts.length && !(contentGroups && contentGroups.length)) break
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        // 消费数按"消息条数"取：内容块组（每消息一组）为权威；旧日志无组 → 按文本条数。
        // （纯图片消息 texts=[] 但组数=1 —— 若按 texts 取 n=0，队列项会残留成"已推送"幽灵）
        const consumeCount = contentGroups ? contentGroups.length : texts.length
        const n = Math.min(consumeCount, v.pendingQueue.length)
        const consumed = v.pendingQueue.slice(0, n)
        const rest = v.pendingQueue.slice(n)
        // 服务端已确认消费 → 清除推送失败提示（问题十）
        const ts = evTs(raw) ?? Date.now() // 用户消息时间戳（回显）
        const normGroup = (g: { type?: string; content?: string; mime_type?: string }[]): QueuedContent[] =>
          g.map((c) => ({
            type: c.type === 'image' ? ('image' as const) : ('text' as const),
            content: String(c.content ?? ''),
            ...(c.mime_type ? { mimeType: c.mime_type } : {}),
          }))
        const groupText = (g: QueuedContent[] | undefined): string =>
          g ? g.filter((c) => c.type === 'text').map((c) => c.content).join('\n').trim() : ''
        // 把待发送中对应消息（FIFO 取最旧的 N 条）移入对话页：优先事件内容块
        //（权威 + 恢复可重建），缺失（旧日志）回退队列 contents。
        const blocks: MsgBlock[] = consumed.map((q, i) => {
          const g = contentGroups?.[i]
          if (g && g.length) {
            const contents = normGroup(g)
            return { kind: 'user' as const, id: blockSeq++, text: groupText(contents) || '(附件)', ts, contents }
          }
          return {
            kind: 'user' as const,
            id: blockSeq++,
            text: queuedText(q) || '(附件)',
            ts,
            contents: q.contents?.length ? q.contents : undefined,
          }
        })
        // 事件多于待发送（其他来源输入，如 agent_send / 恢复重放无本地队列）→ 按事件内容补齐
        for (let i = n; i < Math.max(texts.length, contentGroups?.length ?? 0); i++) {
          const g = contentGroups?.[i]
          if (g && g.length) {
            const contents = normGroup(g)
            blocks.push({ kind: 'user', id: blockSeq++, text: groupText(contents) || '(附件)', ts, contents })
          } else {
            blocks.push({ kind: 'user', id: blockSeq++, text: texts[i] ?? '', ts })
          }
        }
        return setView(s, sid, { ...v, pendingQueue: rest, pendingQueueError: undefined, blocks: [...v.blocks, ...blocks] })
      })
      break
    }
    case 'goal_alignment_reminder': {
      // 修复（2026-08-22）：此前误写为 setView(sid, fn)（签名不符）+ 调用不存在的 nextId()，
      // 每次该事件都抛异常：实时只丢一条系统提示；快照恢复时中断整个回放循环 →
      // 「只加载一段」+ 会话不激活（无活跃标记）。改为标准 setState + setView + blockSeq。
      const sid = getSid(raw, getState().activeSessionId)
      const round = typeof raw.round === 'number' ? raw.round : undefined
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        return setView(s, sid, {
          ...v,
          blocks: [...v.blocks, { kind: 'system', id: blockSeq++, text: round ? `已注入目标一致性提醒（第 ${round} 轮）` : '已注入目标一致性提醒', tone: 'info' }],
        })
      })
      return
    }
    case 'refs_loaded': {
      // @引用展开结果（AgentHarness 层）：批量返回该请求中全部引用判定。
      // 挂到「最近的 user 消息块」下方（同一请求通常紧跟用户消息）；若无 user 块则忽略
      //（不单独成块，避免噪音）。items 保持输入顺序。
      const sid = getSid(raw, getState().activeSessionId)
      const items = Array.isArray(raw.items)
        ? (raw.items as { path?: string; status?: string; resolved?: string; kind?: string }[])
            .filter((it) => it && typeof it.path === 'string' && it.status)
            .map((it) => ({
              path: String(it.path),
              status: (['loaded', 'missing', 'blocked'].includes(String(it.status)) ? String(it.status) : 'blocked') as RefLoadStatus,
              ...(it.resolved ? { resolved: String(it.resolved) } : {}),
              ...(it.kind === 'file' || it.kind === 'dir' ? { kind: it.kind as 'file' | 'dir' } : {}),
            }))
        : []
      if (!items.length) break
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        const lastUser = [...v.blocks].reverse().find((b) => b.kind === 'user')
        if (!lastUser || lastUser.kind !== 'user') return s
        return setView(s, sid, {
          ...v,
          blocks: v.blocks.map((b) =>
            b.kind === 'user' && b.id === lastUser.id ? { ...b, refs: items } : b,
          ),
        })
      })
      break
    }
    case 'agent_start': {
      const sid = getSid(raw, getState().activeSessionId)
      const id = String(raw.run_id || '')
      const parentId = raw.parent_run_id ? String(raw.parent_run_id) : undefined
      // span 起点：root/子 agent 都记，agent_end 结算 durMs（事件 timestamp → 重放可重建）
      const ts = evTs(raw) ?? Date.now()
      const subName = String(raw.name || '')
      const node: RunNode = { id, parentId, label: subName || String(raw.content || 'run'), status: 'running', depth: Number(raw.depth ?? 0), kind: 'run', startedAt: ts }
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        // 新顶层 run = 新 trace：直接丢弃旧 trace（链路只保留最近一次 agent_start→agent_end 子树，减少数据量）
        const runNodes = parentId ? [...v.runNodes, node] : [node]
        // 新顶层 run → 链路 tab 自动跟随最新（用户在历史 trace 上查看时也切回实时，
        // 否则新 run 开始后链路 tab 会「停在过去」且无提示）。
        const followLatest = parentId ? {} : { activeTraceId: undefined, traceDetail: undefined }
        // harness 上报的上下文窗口（问题六）：占比分母单一事实源，本地模型表仅兜底
        const cw = Number(raw.context_window ?? 0)
        const upd = setView(s, sid, {
          ...v,
          ...(cw > 0 ? { contextWindow: cw } : {}),
          ...followLatest,
          runNodes,
          // 子 agent（有父 run）→ 对话页异步 Agent 卡片 + 右侧 Agent 监视器数据源
          blocks: parentId
            ? (() => {
                let taskId = raw.task_id ? String(raw.task_id) : undefined
                if (!taskId) {
                  const placeholder = [...v.blocks].reverse().find((b) => b.kind === 'async_task' && (b.status === 'running' || b.status === 'interrupting') && b.label === subName)
                  if (placeholder?.kind === 'async_task') taskId = placeholder.taskId
                }
                const withoutPlaceholder = taskId
                  ? v.blocks.filter((b) => !(b.kind === 'async_task' && b.taskId === taskId))
                  : v.blocks
                return [...withoutPlaceholder, { kind: 'agent', id: blockSeq++, runId: id, label: subName || String(raw.content || 'agent'), status: 'running', spawnedAt: ts, taskId, usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 }, items: [] }]
              })()
            : clearErrorBlocks(v.blocks), // 新顶层 run 开始：清上次失败的错误块
          runningAgentCount: parentId ? v.runningAgentCount + 1 : v.runningAgentCount,
          aborted: parentId ? v.aborted : false, // 新主 run 开始：复位中断标记
        })
        return { ...upd, busySids: busySidsFromViews(upd.views) }
      })
      break
    }
    case 'agent_end': {
      const sid = getSid(raw, getState().activeSessionId)
      const id = String(raw.run_id || '')
      const now = evTs(raw) ?? Date.now()
      const aborted = raw.finish_reason === 'abort'
      const truncated = raw.finish_reason === 'max_iterations' // 达轮数上限被截断：非完成，需「继续」
      const runStatus = finishToStatus(raw.finish_reason)
      const taskStatus = finishToAsyncTaskStatus(raw.finish_reason)
      // 子运行权威用量（AgentEnd.Usage = 整次运行跨轮累计，含压缩轮与成本）：覆盖按轮
      // llm_end 累加的近似值。异步子 agent 随后还有 task_end（同源同值，幂等覆盖）；
      // **同步子 agent（in-loop，非后台任务）没有 task_end** —— 这里是它唯一的权威来源。
      const runUsage = taskUsageFromEvent(raw.usage as RawUsage | undefined)
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        // 主 run（无 agent 卡片）abort = 用户中断：复位流式态让发送钮恢复、叙述追加
        // "（已中断）"、清未决审批/提问/放行（被 abort 的等待通道已取消，残留 UI 要清）、
        // 运行中工具行收尾为 done（abort 时工具结果事件可能不达）。
        // SubAgent 的 abort/完成不得清 Session 级等待态（审批队列是 Session 统一 FIFO，
        // SubAgent 中断只收敛它自己的调用，后端按 ctx/超时自行出队）。
        const isSub = id !== '' && isSubAgentRun(v, id)
        const finalAssistantIdx = isSub ? -1 : v.blocks.findLastIndex((b) =>
          b.kind === 'assistant' && !b.streaming && Boolean(b.text) && b.runId === id,
        )
        let blocks = v.blocks.map((b, index) =>
          b.kind === 'assistant' && index === finalAssistantIdx && typeof raw.duration_ms === 'number'
            ? { ...b, agentDurMs: raw.duration_ms }
            : b.kind === 'agent' && b.runId === id
              ? {
                  ...b,
                  status: taskStatus,
                  // 2026-08-22：任务 id 兜底补挂（AgentStart/AgentEnd 现已携带；旧日志缺失时
                  // 靠 task_started/result 匹配的兜底锚）—— 有 taskId 右栏才有精确停止按钮
                  taskId: b.taskId || (raw.task_id ? String(raw.task_id) : undefined),
                  durMs: typeof raw.duration_ms === 'number' ? raw.duration_ms : now - b.spawnedAt,
                  // 权威用量（含成本）：同步子 agent 唯一的结算来源；异步任务随后被
                  // task_end 以同值覆盖（幂等）
                  ...(runUsage ? { usage: runUsage } : {}),
                  // 失败原因随 agent_end 立即挂卡片（不依赖 task_result_delivered 事件顺序）
                  ...(taskStatus === 'failed' && raw.error ? { taskError: String(raw.error) } : {}),
                  // 终态清理：中断/完成时 if llm_end 未到，retry 徽章不得残留成矛盾展示
                  retrying: false,
                  retryMessage: undefined,
                }
              : aborted && !isSub && b.kind === 'tool' && b.status === 'running'
                ? { ...b, status: 'done' as const }
                : b,
        )
        // SubAgent abort：压缩进行中（compress_end 可能不达）→ 卡片内压缩项收尾为已中止
        if (aborted && isSub) blocks = abortAgentCompression(blocks, id)
        // 本轮产出汇总（AgentEnd.Artifacts）→ 尾部「本次产出」卡片。
        // 只对主 run（子 agent 的产出归其 agent 卡片，见 AgentCard）。
        // 位置：此处追加即落在 divider 之前——abort 的 divider 在下方才追加，
        // 语义正确（产出属于本轮成果，横线分隔的是下一轮）；
        // 与 Go 侧 view_reducer.appendArtifactsBlockLocked 的顺序一致。
        // 幂等：同 runId（重连补发 / 重放重复投递）不追加第二块。
        if (!isSub) {
          const arts = pickArtifacts(raw.artifacts)
          if (arts && !blocks.some((b) => b.kind === 'artifacts' && b.runId === id)) {
            blocks = [...blocks, {
              kind: 'artifacts', id: blockSeq++, items: arts,
              ...(id ? { runId: id } : {}),
              ...(raw.artifacts_truncated === true ? { truncated: true } : {}),
            }]
          }
        }
        if (aborted && !isSub) {
          // 中断到达：结算所有进行中的思考计时（防用户可见「思考 ·N…」持续跳动）
          blocks = blocks.map((b) =>
            b.kind === 'assistant' && b.thinkStart != null && b.thinkMs == null
              ? { ...b, thinkMs: now - b.thinkStart }
              : b,
          )
          // 清 abort 副作用的 error 块（LLMError "context canceled" 等非真错误），
          // 追加 divider（横线）作上下文隔离——不再补「（已中断）」文本。
          // 恢复重放（materializing）不追加：历史中断隔离由 checkpoint 里的 divider
          // 块提供（Go 侧 NormalizeForRestore / agent_end 已写入）；仅实时到达的
          // agent_end(abort) 即时插入，避免重放重复横线。
          blocks = [...clearErrorBlocks(blocks), ...(materializing ? [] : [{ kind: 'divider' as const, id: blockSeq++, reason: 'abort' as const, ts: now }])]
        } else if (truncated && !isSub) {
          // 达 MaxIterations 护栏被截断：任务未完成、无最终答复——明确提示可「继续」续跑
          blocks = [...blocks, { kind: 'assistant', id: blockSeq++, text: '（已达本轮轮数上限，任务未完成；输入「继续」可续跑）', streaming: false, thinking: '' }]
        }
        const upd = setView(s, sid, {
          ...v,
          runNodes: v.runNodes.map((n) =>
            id && n.id === id
              ? { ...n, status: runStatus, durMs: typeof raw.duration_ms === 'number' ? raw.duration_ms : (n.startedAt != null ? now - n.startedAt : n.durMs) }
              : n,
          ),
          blocks,
          runningAgentCount: countRunningAgents(blocks),
          streaming: (aborted || truncated) && !isSub ? false : v.streaming,
          aborted: aborted && !isSub ? true : v.aborted, // 主 run 中断置位：拦截后续残留 chunk
          approvals: aborted && !isSub ? [] : v.approvals,
          question: aborted && !isSub ? undefined : v.question,
          release: aborted && !isSub ? undefined : v.release,
        })
        return { ...upd, busySids: busySidsFromViews(upd.views) }
      })
      // run 结束后该会话不再受 busy 保护，立即让缓存按 LRU 收敛；若仍有子 run，sessionBusy
      // 保持 true，evictViews 会继续保护它。
      queueMicrotask(() => evictViews())
      // 注意：不在 agent_end 推送暂存队列。Run 结束前未消费的暂存消息（末轮后新输入）
      // 保持未推送、停留本地暂存队列（可编辑）——下一次 LLM End 或用户空闲发送时
      // 与后续输入整队一起 ask_batch 推送，避免 agent_end 立刻推一条旧输入触发「意外新 run」
      // 让会话看似仍忙碌、用户新消息又被塞进暂存队列的体验问题（2026-08-17 实测）。
      // 例外：用户主动中断（finish_reason=abort）必须推送 —— 那时不会再有自己的 llm_end，
      // 见本 case 末尾的 flushPending。
      //
      // §6.10 兜底收敛：本轮产出清单（Go 侧判定 created/modified，比工具名更权威）
      // 里若有可视类文件 → 自动打开。为什么两处都接：tool_run_end 让用户在**过程中**
      // 就实时看到产出（主路径），agent_end 补齐「工具结果缺 diff / 事件顺序异常」的
      // 漏网情形；同一文件重复由 autoOpenVisualFile 内部的路径去重挡住（实测两次事件
      // 只打开一次），不会把用户已切走的视图反复拽回来。
      // 只对主 run 生效（与产出块同理：子 agent 的产出归其卡片，不抢主视图）。
      // 判定方式与上方 setState 内一致：runId 已在视图里登记为子 run。
      const endView = getState().views[sid]
      const isSubRun = id !== '' && isSubAgentRun(endView ?? emptyView(), id)
      if (!isSubRun) {
        const arts = pickArtifacts(raw.artifacts)
        const visual = arts?.find((a) => isVisualPath(a.path))
        if (visual) autoOpenVisualFile(visual.path, sid)
      }
      // 用户点「停止」（interrupt → agent_end finish_reason=abort）：把暂存队列整队推送
      //（中断后本 run 不会再有自己的 llm_end）。只认主 run 的中断 —— 子 agent 中断不改变
      // 主 run 状态，不该借它启动新一轮。语义/编辑窗口见 flushQueueOnRunEnd。
      if (aborted && !isSubRun) flushQueueOnRunEnd(sid)
      break
    }
    // session_run_error：整个 run 致命失败（SDK 事件，主 agent 最终失败）。叙述通道显示醒目错误块
    //（原位置更新 llm_error 的重试块）；清流式态让发送钮复位。此前 401 等失败对用户完全静默——补上。
    // 该事件是**会话级**终态（Go 侧只在主 run ac.Err != nil 且非 abort 时发出，子 agent 失败不发），
    // 因此这里不需要 runId 归属判定。
    case 'session_run_error': {
      const sid = getSid(raw, getState().activeSessionId)
      const msg = String(raw.error || '任务运行失败')
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        const upd = setView(s, sid, {
          ...v,
          blocks: upsertErrorBlock(v.blocks, `（运行失败）${msg}`),
          streaming: false,
          llmStartTs: undefined,
          turnStartTs: undefined,
        })
        return { ...upd, busySids: busySidsFromViews(upd.views) }
      })
      // 运行失败 = 本轮结束且不会再有 llm_end：把暂存队列整队推送，消息不滞留本机
      //（用户要求：run 异常失败后同样推送）。语义/编辑窗口/为什么不在 interrupt() 里推，
      // 见 flushQueueOnRunEnd。
      flushQueueOnRunEnd(sid)
      break
    }
    case 'session_closed': {
      const sid = getSid(raw, getState().activeSessionId)
      disarmSessionCompressWatchdogs(sid) // 会话关闭：取消该会话全部压缩看门狗（主 + 各 SubAgent）
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        const upd = setView(s, sid, { ...v, streaming: false, pendingQueue: [] }, { bridgeStatus: 'idle' })
        return { ...upd, busySids: busySidsFromViews(upd.views) }
      })
      break
    }
    case 'task_started': {
      const sid = getSid(raw, getState().activeSessionId)
      const taskId = String(raw.task_id || '')
      if (!taskId) break
      const ts = evTs(raw) ?? Date.now()
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        const existing = v.blocks.findIndex((b) => (b.kind === 'async_task' || b.kind === 'agent') && b.taskId === taskId)
        const status = asyncTaskStatus(raw.status, raw.error, 'running')
        if (existing >= 0) {
          const blocks = [...v.blocks]
          const task = blocks[existing] as Extract<MsgBlock, { kind: 'agent' | 'async_task' }>
          blocks[existing] = { ...task, status }
          const upd = setView(s, sid, { ...v, blocks })
          return { ...upd, busySids: busySidsFromViews(upd.views) }
        }
        const block = { kind: 'async_task' as const, id: blockSeq++, taskId, label: String(raw.name || raw.tool_name || '后台任务'), toolName: raw.tool_name ? String(raw.tool_name) : undefined, status, startedAt: ts }
        const upd = setView(s, sid, { ...v, blocks: [...v.blocks, block] })
        return { ...upd, busySids: busySidsFromViews(upd.views) }
      })
      break
    }
    case 'task_end': {
      const sid = getSid(raw, getState().activeSessionId)
      const taskId = String(raw.task_id || '')
      const status = asyncTaskStatus(raw.status, raw.error)
      const ts = evTs(raw) ?? Date.now()
      // 任务全量用量（含成本）：卡片展示 tokens + 成本（不依赖 task_result_delivered 到达）
      const usage = taskUsageFromEvent(raw.usage as RawUsage | undefined)
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        const upd = setView(s, sid, { ...v, blocks: applyTaskUsage(v.blocks.map((b) =>
          (b.kind === 'async_task' || b.kind === 'agent') && b.taskId === taskId
            ? { ...b, status, ...(b.kind === 'async_task' ? { endedAt: ts } : { durMs: ts - b.spawnedAt }) }
            // promoted 工具任务同样按 task_end 收敛（task_result_delivered 可能因
            // 入队失败未达 —— 事件与入队已解耦，但仍保留双通道收敛，防止卡片永久停在
            // 「执行中/中断中」，角标「N 运行中」随之归零）。durMs 缺失（tool_run_end
            // 未达，如硬杀）时按 startedAt 兜底结算，避免终态卡片无时长可显。
            : b.kind === 'tool' && b.promoted && b.taskId === taskId
              ? {
                  ...b,
                  status: 'done' as const,
                  interrupting: false,
                  taskStatus: status,
                  ...(b.durMs == null && b.startedAt != null ? { durMs: Math.max(0, ts - b.startedAt) } : {}),
                }
              : b,
        ), taskId, usage) })
        return { ...upd, busySids: busySidsFromViews(upd.views) }
      })
      break
    }
    case 'task_promoted': {
      // 运行中的工具已被摘离为后台任务（promote_task 命令成功）：该工具行 → 后台任务态
      const sid = getSid(raw, getState().activeSessionId)
      const taskId = String(raw.task_id || '')
      if (!taskId) break
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        return setView(s, sid, {
          ...v,
          blocks: v.blocks.map((b) =>
            b.kind === 'tool' && b.taskId === taskId
              ? { ...b, promoted: true, taskStatus: b.interrupting ? ('interrupting' as const) : ('running' as const) }
              : b,
          ),
          runNodes: v.runNodes.map((n) => (n.kind === 'tool' && n.taskId === taskId ? { ...n, promoted: true } : n)),
        })
      })
      break
    }
    case 'task_result_delivered': {
      // 完整结果只挂到任务卡片；叙述区总追加一条轻量交付通知。事件重放/重复投递按 taskId 去重。
      // 旧日志或异常顺序下找不到任务时，保留独立结果块，避免结果丢失。
      const sid = getSid(raw, getState().activeSessionId)
      const taskId = String(raw.task_id || '')
      const result = String(raw.result || '')
      const error = String(raw.error || '')
      // 任务全量用量（含成本）：与 task_end 同源；旧宿主可能只发本事件 → 这里兜底
      const usage = taskUsageFromEvent(raw.usage as RawUsage | undefined)
      setState((s) => {
        const v = s.views[sid] ?? emptyView()
        // 三类异步任务统一按 taskId 匹配：agent / async_task 占位 / promoted 工具
        const taskIndex = v.blocks.findIndex((b) =>
          (b.kind === 'agent' || b.kind === 'async_task' || b.kind === 'tool') && b.taskId === taskId,
        )
        const task = taskIndex >= 0 ? v.blocks[taskIndex] : undefined
        const fallbackStatus: AsyncTaskStatus | undefined = task
          ? task.kind === 'tool'
            ? task.status === 'running' ? 'running' : 'completed'
            : task.kind === 'agent' || task.kind === 'async_task'
              ? task.status
              : undefined
          : undefined
        const deliveryStatus = terminalTaskStatus(raw.status, error, fallbackStatus)
        const alreadyNoticed = v.blocks.some((b) => b.kind === 'task_delivery' && b.taskId === taskId)
        if (taskIndex < 0) {
          if (alreadyNoticed || v.blocks.some((b) => b.kind === 'task_result' && b.taskId === taskId)) return s
          return setView(s, sid, {
            ...v,
            blocks: [
              ...v.blocks,
              { kind: 'task_result', id: blockSeq++, taskId, result, error: error || undefined },
              { kind: 'task_delivery', id: blockSeq++, taskId, status: deliveryStatus, result: result || undefined, error: error || undefined },
            ],
          })
        }
        const blocks = [...v.blocks]
        const t = blocks[taskIndex]
        if (t.kind === 'tool') {
          // promoted 工具：收敛终态（interrupted 保留完整取消结果/错误）+ 停中断标记。
          // durMs 缺失（tool_run_end 未达）时按 startedAt 兜底，终态卡片不缺时长。
          blocks[taskIndex] = {
            ...t,
            status: 'done' as const,
            interrupting: false,
            promoted: true,
            taskStatus: deliveryStatus,
            isError: deliveryStatus === 'failed',
            result: result || error || t.result,
            ...(t.durMs == null && t.startedAt != null ? { durMs: Math.max(0, (evTs(raw) ?? Date.now()) - t.startedAt) } : {}),
          }
        } else if (t.kind === 'agent' || t.kind === 'async_task') {
          blocks[taskIndex] = {
            ...t,
            status: deliveryStatus,
            taskResult: result,
            taskError: error || undefined,
            deliveredToMain: true,
          }
        }
        if (!alreadyNoticed) {
          blocks.push({ kind: 'task_delivery', id: blockSeq++, taskId, status: deliveryStatus, result: result || undefined, error: error || undefined })
        }
        // 用量覆盖（终态权威值）：TaskEnd 已写过则同值覆盖，未写过则补齐
        return setView(s, sid, { ...v, blocks: applyTaskUsage(blocks, taskId, usage) })
      })
      break
    }
    default:
      break
  }
}

// —— 恢复落地看门狗（2026-09-21）——
// 实证背景：刷新/重启后若这一次会话恢复**没落地**，视图会从「空」开始重建 —— 历史与
// 累计（回合 / in-out / 缓存 / cost）只剩重载之后的部分，而且**全程静默**。
// 用户实测：280 轮 / 80M / $0.51 的会话在渲染进程重载后 footbar 只显示最后 4 轮
// （in 1.9M / 缓存 99.89% / cost $0.0081，逐位可复现），record 里数据完好 → 不是丢数据，
// 是恢复响应没被应用。四条已知不落地路径：响应按陈旧代际丢弃（下方 !navigationCurrent）、
// 响应永久丢失（看门狗 15s 只把 preBuf 灌进空视图）、hydrate 抛错回退空视图、
// 响应无历史数据（bridge 既无 record 也无事件日志）。
// 对策：对这四条路径做**有限重试**（只针对显式恢复且有 id 的会话；不递增导航代际、
// 不动用户的新导航；视图已有内容就停）。上限仍失败 → 明示（console + toast），不再静默。
const RESTORE_RETRY_DELAYS_MS = [800, 2500]
const restoreRetry = new Map<string, number>() // sid → 已重试次数
const restoreGaveUp = new Set<string>() // 已达上限放弃的 sid（成功恢复/显式切换时清除）

// isViewHydrated 该会话视图是否已有内容（与 setActiveSession 的 isHydrated 同判据）。
// 「有 blocks」或「带 checkpointRevision 元数据」都算已恢复：空视图（只有 git 字段）不算。
function isViewHydrated(sid: string): boolean {
  const v = useAppStore.getState().views[sid]
  if (!v) return false
  return v.blocks.length > 0 || v.checkpointRevision != null
}

// scheduleRestoreRetry 恢复未落地时的有限重试。
// loud=true 表示「本来有数据却没拿到/被丢弃」（可明示失败）；false 用于 bridge 明确回答
// 「无历史数据」的歧义场景（可能是真·空会话），只 warn 不弹 toast，避免误报。
// expectWorkspace：请求发起时的工作区。切换工作区后同一 sid 的恢复已无意义（会落到别的
// 工作区目录），因此调用点都传它，不匹配就直接放弃重试。
// requestId：正在处理的响应 id。响应分发期间 snapshotPending 尚未清理（wire() 的清理
// 在 dispatchEvent 之后），传它就只在**别的**恢复请求在途时才跳过重试；延迟回调里
// 该条目已清，用普通在途判断即可。
function scheduleRestoreRetry(sid: string, reason: string, loud: boolean, expectWorkspace?: string, requestId?: number): void {
  if (!sid || restoreGaveUp.has(sid)) return
  const st = useAppStore.getState()
  if (expectWorkspace && st.workspace !== expectWorkspace) return // 已切走工作区：目标失效
  // 只重试「仍是当前目标」的会话：已切到别的会话（或别的会话已激活）时不打扰用户，
  // 那条路径自己会走恢复。
  if (st.activeSessionId !== sid && st.activeSessionId !== undefined) return
  if (isViewHydrated(sid)) return // 已有历史（可能是另一条响应刚恢复）：无需重试
  const inflight = snapshotPending.get(sid)
  if (inflight != null && inflight !== requestId) return // 已有别的恢复请求在途：等它，别叠发
  const tries = restoreRetry.get(sid) ?? 0
  if (tries >= RESTORE_RETRY_DELAYS_MS.length) {
    restoreGaveUp.add(sid)
    console.error(`[restore] 会话 ${sid} 恢复失败（${reason}）：本视图只有重载之后的内容，历史与累计可能不全`)
    if (loud) useAppStore.getState().showToast(i18nT('restore.incomplete'), 'error')
    return
  }
  restoreRetry.set(sid, tries + 1)
  const delay = RESTORE_RETRY_DELAYS_MS[tries]
  console.warn(`[restore] 会话 ${sid} 恢复未落地（${reason}）→ ${delay}ms 后重试 ${tries + 1}/${RESTORE_RETRY_DELAYS_MS.length}`)
  setTimeout(() => {
    const s2 = useAppStore.getState()
    if (expectWorkspace && s2.workspace !== expectWorkspace) return
    if (s2.activeSessionId !== sid && s2.activeSessionId !== undefined) return
    if (isViewHydrated(sid) || snapshotPending.has(sid)) return
    // navigation=false：重试不占导航代际 —— 不打断用户的新导航；若期间用户又导航了，
    // 本响应会按陈旧丢弃，由那条路径的失败点再触发一次重试（直至上限）。
    send({ type: 'new_session', payload: { id: sid } }, false)
  }, delay)
}

// clearRestoreRetry 恢复成功（或用户显式切换会话）后清空该会话的重试账。
function clearRestoreRetry(sid: string): void {
  restoreRetry.delete(sid)
  restoreGaveUp.delete(sid)
}

// —— 快照恢复（FIX_OPTIMIZATION 问题2③）：冷恢复不再逐行重放事件日志 ——
// bridge 把 events/{id}.jsonl 全量读一次，随 new_session 的 command_response 单行返回
// snapshot.events（有序数组，保留 event_type）。恢复期间发往该 sid 的事件先入 preBuf（防御，正常为空）；
// 响应到达后：materializeSnapshot → coalesce（合并连续 chunk）→ 逐事件 dispatch（单次 React 提交）
// → 归一化残留态 + 维护 busy/runningAgentCount + 视图 LRU。
// 相比旧 replay：无逐行 IPC、无逐行 JSON.parse、合并后数组拷贝从 O(N×M) 降为 O(M)。
const snapshotPending = new Map<string, number>() // sid → 当前有效 new_session 请求 id
const preBuf = new Map<string, AnyEvent[]>() // sid → 恢复期间到达的事件（响应后按序补应用）

// 快照恢复看门狗（2026-08-22）：new_session 响应永久丢失（bridge 崩溃 / 会话在途被删）时，
// snapshotPending 不清理 → 该 sid 后续事件全部积压 preBuf 永不落地（视图冻结，UI 看似"卡死"）。
// 超时（15s，覆盖大日志聚合耗时）后：清 pending + 缓冲转正常 dispatch 兜底应用。
const SNAPSHOT_WATCHDOG_MS = 15000
function armSnapshotWatchdog(sid: string, requestId: number): void {
  setTimeout(() => {
    if (snapshotPending.get(sid) !== requestId) return // 响应已到达/已被更新的请求接管
    const buffered = preBuf.get(sid)
    preBuf.delete(sid)
    snapshotPending.delete(sid)
    if (buffered && buffered.length) {
      for (const ev of coalesceEvents(buffered)) dispatchEvent(ev)
    }
    // 响应永久丢失（请求 id 仍在 pending，说明从未被消费）：视图此刻只含重载后的事件
    // → 触发有限重试。必须在 flush 之后（snapshotPending 已释放，否则被「在途」挡住）。
    // 只有显式恢复（带 id 的 new_session）才会登记看门狗，故这里 loud=true 可明示失败。
    if (pending[requestId]) {
      scheduleRestoreRetry(sid, '恢复响应丢失（看门狗超时）', true, pending[requestId]?.workspace)
    }
  }, SNAPSHOT_WATCHDOG_MS)
}

// finalizeReplayView 快照/历史归一化：残留的流式/running 态强制落地
//（被中断的会话不能卡输入/显示幽灵运行点），交互态（提问/审批/沙箱放行）不跨重启存活；
// 运行中子 agent 计数收敛到归一化后的最终 blocks。
function finalizeReplayView(v: SessionView): SessionView {
  const blocks = v.blocks.map((b) =>
    b.kind === 'assistant'
      // 恢复归一化：流式态复位 + 未结算的思考计时强制落地 —— 会话中断/日志截断时
      // thinkStart 已打点但 llm_end 未到，不结算会留下永远跳动的「思考 · Ns…」幽灵计时器。
      ? { ...b, streaming: false, ...(b.thinkStart != null && b.thinkMs == null ? { thinkMs: Math.max(0, Date.now() - b.thinkStart) } : {}) }
      : b.kind === 'tool' && b.status === 'running'
        ? { ...b, status: 'done' as const, interrupting: false, ...(b.taskStatus ? { taskStatus: 'abandoned' as const } : {}) }
        : b.kind === 'agent' && (b.status === 'running' || b.status === 'interrupting')
          ? { ...b, status: 'abandoned' as const }
          : b.kind === 'async_task' && (b.status === 'running' || b.status === 'interrupting')
            ? { ...b, status: 'abandoned' as const }
            : b,
  )
  return {
    ...v,
    streaming: false,
    llmStartTs: undefined,
    turnStartTs: undefined,
    toolStartTs: {},
    thinkStartTs: {},
    turnThinkMs: {},
    question: undefined,
    release: undefined,
    approvals: [],
    // 快照中 compress_start 无配套 compress_end（中断）→ 复位活动态，防右栏幽灵「压缩中」
    compressing: false,
    compressStartTs: undefined,
    compressBefore: undefined,
    compressRunId: undefined,
    compressReason: undefined,
    blocks,
    runningAgentCount: countRunningAgents(blocks),
    runNodes: v.runNodes.map((n) => (n.status === 'running' ? { ...n, status: 'done' as const } : n)),
  }
}

// coalesceEvents 合并连续同类文本 chunk（reasoning/content，同一 session/request/run）为一个事件，
// 把 O(N×M) 的逐 chunk 数组拷贝降为 O(M)。仅文本增长类事件安全合并，整体顺序保持不变。
function coalesceEvents(events: AnyEvent[]): AnyEvent[] {
  const out: AnyEvent[] = []
  let pending: AnyEvent | null = null
  const isChunk = (ev: AnyEvent) => ev.event_type === 'reasoning_chunk' || ev.event_type === 'content_chunk'
  const keyOf = (ev: AnyEvent) =>
    `${ev.event_type}:${String(ev.session_id ?? '')}:${String(ev.request_id ?? '')}:${String(ev.run_id ?? '')}`
  for (const ev of events) {
    if (!isChunk(ev)) {
      if (pending) {
        out.push(pending)
        pending = null
      }
      out.push(ev)
      continue
    }
    if (pending && keyOf(pending) === keyOf(ev)) {
      // 保留首个事件字段（timestamp 等），累加 content
      pending = Object.assign({}, pending, { content: String(pending.content ?? '') + String(ev.content ?? '') })
    } else {
      if (pending) out.push(pending)
      pending = ev
    }
  }
  if (pending) out.push(pending)
  return out
}

// materializeSnapshot 快照恢复：coalesce 后逐事件 dispatch 重建视图（一次 React 提交），
// 归一化残留态，维护 busy/runningAgentCount，并触发视图 LRU。随后由调用方统一做激活/会话列表回填。
// 导出：Node 回归脚本（scripts/test-agent-isolation.mjs）直接调用验证恢复归一化。
export function materializeSnapshot(sid: string, snapEvents: unknown[], requestId?: number, cold = true): void {
  // 完整快照必须从空视图重建，不能追加到缓存 view；否则重复恢复会把消息、usage、turns 全部翻倍。
  useAppStore.setState((st) => ({ views: { ...st.views, [sid]: emptyView() } }))
  // buildSnapshot（Go）为省空间去掉了每条事件的 session_id，假定前端已知 sid。
  // 但各 dispatchEvent 处理器以 getState().activeSessionId 兜底路由，恢复期间活跃会话还是旧的
  // （或空）—— 若不补回 session_id，快照历史会投到错误/空视图，导致冷启动「历史没加载进来」。
  // 故在此统一补回恢复目标 sid（含 coalesce 前，保证按会话合并 chunk 的 key 正确）。
  const events = ((snapEvents ?? []) as AnyEvent[]).map((ev) =>
    ev.session_id ? ev : { ...ev, session_id: sid },
  )
  materializing = true
  try {
    for (const ev of coalesceEvents(events)) {
      try {
        dispatchEvent(ev)
      } catch (err) {
        // 单条事件回放失败不得中断整体恢复：否则异常点之后的历史全部丢失
        //（「只加载一段」）且后续激活步骤不执行（会话无活跃标记）。
        console.error('快照回放跳过异常事件:', (ev as AnyEvent)?.event_type, err)
      }
    }
  } finally {
    const buffered = preBuf.get(sid)
    preBuf.delete(sid)
    if (buffered && buffered.length) {
      for (const ev of coalesceEvents(buffered)) {
        try {
          dispatchEvent(ev)
        } catch (err) {
          console.error('快照回放补应用跳过异常事件:', (ev as AnyEvent)?.event_type, err)
        }
      }
    }
    materializing = false
    if (requestId == null || snapshotPending.get(sid) === requestId) snapshotPending.delete(sid)
    useAppStore.setState((st) => {
      const v = st.views[sid]
      if (!v) return {}
      return setView(st, sid, cold ? finalizeReplayView(v) : v)
    })
    // 快照落地成功 → 清空该会话的重试账（见 scheduleRestoreRetry）
    if (isViewHydrated(sid)) clearRestoreRetry(sid)
    evictViews(sid)
  }
}

// —— checkpoint hydrate（目标主路径，替代 materializeSnapshot）——
// bridge ViewReducer 产出的稳定投影直接映射为 SessionView。
// 不经过事件 materializer；归一化残留 running 态与瞬时交互态（finalizeReplayView），
// 并把 checkpoint 的 revision/clear_generation/degraded 元数据留在 view 上。

function cpQueuedContent(c: { type: string; content: string; mime_type?: string }): QueuedContent {
  return { type: c.type === 'image' ? 'image' : 'text', content: c.content, ...(c.mime_type ? { mimeType: c.mime_type } : {}) }
}
function cpTodo(it: { id: string; title: string; done: boolean; blockedBy?: string[] }): TodoItem {
  return { id: it.id, title: it.title, done: it.done, ...(it.blockedBy?.length ? { blockedBy: it.blockedBy } : {}) }
}
function cpDiff(d: { path: string; added: number; removed: number; unified?: string; truncated?: boolean }): Diff {
  return {
    path: d.path, added: d.added, removed: d.removed,
    ...(d.unified ? { unified: d.unified } : {}),
    ...(d.truncated ? { truncated: true } : {}),
  }
}
function cpToolError(e?: { code?: string; changed?: string; retryable?: boolean; nextAction?: string; recovery?: string }): ToolErrorInfo | undefined {
  if (!e) return undefined
  const out: ToolErrorInfo = {}
  if (e.code) out.code = e.code
  if (e.changed === 'true' || e.changed === 'false' || e.changed === 'unknown') out.changed = e.changed
  if (e.retryable) out.retryable = true
  if (e.nextAction) out.nextAction = e.nextAction
  if (e.recovery) out.recovery = e.recovery
  return Object.keys(out).length ? out : undefined
}
// cpUsage 把 checkpoint 里的 usage 映射成客户端 UsageAgg。
// 注意 wire 形态是**蛇形**（bridge view_reducer.go：input/output/cache_read/reasoning）——
// 早先这里读 u.cacheRead（camelCase）恒为 undefined → 每次「重启应用/恢复会话」水合后
// 会话累计 cacheRead 归零（input/output 正常），状态栏「缓存」= cacheRead/input 因此掉到
// 个位数百分比（用户实测 4.20% / 23.88%），而每轮指标（turns，按 cache_read 映射）正常，
// 于是出现「当前上下文 99.4% vs 会话累计 4.2%」的假差距。两种拼写都接受，兼容旧数据。
// cost_usd 是**卡片级**成本（USD，仅任务卡有；会话级总成本走顶层 cost_usd 字段）。
function cpUsage(u?: { input?: number; output?: number; cache_read?: number; cacheRead?: number; cache_write?: number; cacheWrite?: number; cache_write_1h?: number; cacheWrite1h?: number; reasoning?: number; cost_usd?: number; costUsd?: number }): UsageAgg {
  const cost = u?.cost_usd ?? u?.costUsd ?? 0
  return {
    input: u?.input ?? 0,
    output: u?.output ?? 0,
    cacheRead: u?.cache_read ?? u?.cacheRead ?? 0,
    cacheWrite: u?.cache_write ?? u?.cacheWrite ?? 0,
    cacheWrite1h: u?.cache_write_1h ?? u?.cacheWrite1h ?? 0,
    reasoning: u?.reasoning ?? 0,
    ...(cost > 0 ? { costUsd: cost } : {}),
  }
}
function cpAgentItem(it: {
  kind: string; text?: string; is_error?: boolean; think_start?: number
  tool_id?: string; name?: string; args?: string; status?: string; result?: string
  error_info?: { code?: string; changed?: string; retryable?: boolean; nextAction?: string; recovery?: string }
  dur_ms?: number; started_at?: number; task_id?: string
  diff?: { path: string; added: number; removed: number; unified?: string; truncated?: boolean }
  images?: { type: string; content: string; mime_type?: string }[]
  before?: number; after?: number; ctx_tokens?: number; reason?: string; model?: string; summary?: string
  usage?: { input?: number; output?: number; cache_read?: number; cacheRead?: number; cache_write?: number; cacheWrite?: number; cache_write_1h?: number; cacheWrite1h?: number; reasoning?: number; cost_usd?: number; costUsd?: number }
  error?: string; aborted?: boolean; active?: boolean; analysis?: string
}): AgentItem {
  if (it.kind === 'text') return { kind: 'text', text: it.text ?? '', error: it.is_error === true }
  if (it.kind === 'thinking') return { kind: 'thinking', text: it.text ?? '', ...(it.think_start != null ? { thinkStart: it.think_start } : {}) }
  if (it.kind === 'tool') {
    return {
      kind: 'tool', toolId: it.tool_id ?? '', name: it.name ?? '', args: it.args,
      images: pickImages(it.images),
      status: it.status === 'error' ? 'error' : it.status === 'running' ? 'running' : 'done',
      result: it.result, errorInfo: cpToolError(it.error_info), durMs: it.dur_ms,
      ...(it.started_at != null ? { startedAt: it.started_at } : {}),
      ...(it.task_id ? { taskId: it.task_id } : {}),
      diff: it.diff ? cpDiff(it.diff) : undefined,
      // 本次调用用量（仅子 agent 类工具非零）：恢复后卡片内工具项仍显示 tokens/成本
      ...(it.usage && (it.usage.input || it.usage.output || it.usage.cost_usd) ? { usage: cpUsage(it.usage) } : {}),
    }
  }
  // compression
  return {
    kind: 'compression', before: it.before ?? 0, after: it.after ?? 0,
    ctxTokens: it.ctx_tokens, reason: it.reason, model: it.model, summary: it.summary,
    usage: it.usage ? cpUsage(it.usage) : undefined, error: it.error,
    aborted: it.aborted === true || undefined, durMs: it.dur_ms,
    active: it.active === true, ...(it.started_at != null ? { startedAt: it.started_at } : {}),
    ...(it.analysis ? { analysis: it.analysis } : {}),
  }
}

function checkpointToView(cp: CheckpointView): SessionView {
  const blocks: MsgBlock[] = []
  for (const b of cp.blocks ?? []) {
    switch (b.kind) {
      case 'user':
        blocks.push({ kind: 'user', id: b.id, text: b.text ?? '', ts: b.ts, ...(b.contents?.length ? { contents: b.contents.map(cpQueuedContent) } : {}) })
        break
      case 'assistant':
        // 过滤历史「（已中断）」「（上下文已清空）」文本块：旧版 Go 把它们作为文本块
        // 固化进 checkpoint，恢复时不应展示。新语义：中断/关闭/清空由 divider 块表达
        //（见下方 divider case）。
        if (b.text === '（已中断）' || b.text === '（上下文已清空）') break
        blocks.push({
          kind: 'assistant', id: b.id, text: b.text ?? '', thinking: b.thinking ?? '', streaming: false,
          model: b.model, runId: b.run_id, ts: b.ts,
          ...(b.think_ms != null ? { thinkMs: b.think_ms } : {}),
          ...(b.ttft_ms != null ? { ttftMs: b.ttft_ms } : {}),
          ...(b.dur_ms != null ? { durMs: b.dur_ms } : {}),
          ...(b.agent_dur_ms != null ? { agentDurMs: b.agent_dur_ms } : {}),
          // 输出速度（tokens/s）分子随 checkpoint 复原；genMs 由下面 useMemo 式的
          // 纯函数按同一口径重算（Thinking 是否流式决定分母），重放/刷新后数字不丢。
          ...(b.output_tokens != null ? { outputTokens: b.output_tokens } : {}),
          ...(b.reasoning_tokens != null ? { reasoningTokens: b.reasoning_tokens } : {}),
          ...(b.thinking_summarized ? { thinkingSummarized: true } : {}),
        })
        break
      case 'divider':
        // 上下文隔离分隔线（shutdown=进程关闭/重启，abort=用户中断，clear=清空上下文）：
        // 渲染为横线。无法识别 reason（旧数据/异常）→ 静默跳过（不展示任何样式/文本）。
        if (b.divider_reason !== 'shutdown' && b.divider_reason !== 'abort' && b.divider_reason !== 'clear') break
        blocks.push({
          kind: 'divider', id: b.id, reason: b.divider_reason,
          ...(b.ts != null ? { ts: b.ts } : {}),
        })
        break
      case 'tool':
        blocks.push({
          kind: 'tool', id: b.id, toolId: b.tool_id ?? '', name: b.name ?? '', args: b.args,
          result: b.result, diff: b.diff ? cpDiff(b.diff) : undefined,
          images: pickImages(b.images), isError: b.is_error === true,
          errorInfo: cpToolError(b.error_info), durMs: b.dur_ms,
          status: b.status === 'running' ? 'running' : 'done',
          ...(b.task_id ? { taskId: b.task_id } : {}),
          ...(b.promoted ? { promoted: true } : {}),
          ...(b.task_status ? { taskStatus: b.task_status as AsyncTaskStatus } : {}),
          ...(b.started_at != null ? { startedAt: b.started_at } : {}),
          ...(b.todos?.length ? { todos: b.todos.map(cpTodo) } : {}),
          // 任务用量（promoted 工具任务：TaskEnd 写入 checkpoint）——恢复后卡片仍显示成本
          ...(b.usage && (b.usage.input || b.usage.output || b.usage.cost_usd) ? { usage: cpUsage(b.usage) } : {}),
        })
        break
      case 'agent': {
        const artItems = pickArtifacts(b.artifacts)
        blocks.push({
          kind: 'agent', id: b.id, runId: b.run_id ?? '', label: b.label ?? 'agent',
          status: b.status as AsyncTaskStatus, spawnedAt: b.spawned_at ?? b.ts ?? Date.now(),
          usage: cpUsage(b.usage),
          items: (b.items ?? []).map(cpAgentItem),
          ...(b.task_id ? { taskId: b.task_id } : {}),
          ...(b.dur_ms != null ? { durMs: b.dur_ms } : {}),
          ...(b.think_ms != null ? { thinkMs: b.think_ms } : {}),
          ...(b.task_result ? { taskResult: b.task_result } : {}),
          ...(b.task_error ? { taskError: b.task_error } : {}),
          ...(b.delivered_to_main ? { deliveredToMain: true } : {}),
          // 子 agent 的产出（AgentEnd.Artifacts 挂卡片）——恢复后卡片内仍可见
          ...(artItems ? { artifacts: artItems } : {}),
          ...(b.artifacts_truncated === true ? { artifactsTruncated: true } : {}),
        })
        break
      }
      case 'async_task':
        blocks.push({
          kind: 'async_task', id: b.id, taskId: b.task_id ?? '', label: b.label ?? '后台任务',
          status: b.status as AsyncTaskStatus, startedAt: b.started_at ?? b.ts ?? Date.now(),
          ...(b.name ? { toolName: b.name } : {}),
          ...(b.ended_at != null ? { endedAt: b.ended_at } : {}),
          ...(b.task_result ? { taskResult: b.task_result } : {}),
          ...(b.task_error ? { taskError: b.task_error } : {}),
          ...(b.delivered_to_main ? { deliveredToMain: true } : {}),
          ...(b.usage && (b.usage.input || b.usage.output || b.usage.cost_usd) ? { usage: cpUsage(b.usage) } : {}),
        })
        break
      case 'task_result':
        blocks.push({ kind: 'task_result', id: b.id, taskId: b.task_id ?? '', result: b.result ?? '', ...(b.error ? { error: b.error } : {}) })
        break
      case 'task_delivery':
        blocks.push({
          kind: 'task_delivery', id: b.id, taskId: b.task_id ?? '',
          status: (b.status ?? 'completed') as TerminalTaskStatus,
          ...(b.result ? { result: b.result } : {}),
          ...(b.error ? { error: b.error } : {}),
        })
        break
      case 'compression':
        blocks.push({
          kind: 'compression', id: b.id, before: b.before ?? 0, after: b.after ?? 0,
          ctxTokens: b.ctx_tokens, reason: b.reason, model: b.model, summary: b.summary,
          usage: b.usage ? cpUsage(b.usage) : undefined, error: b.error,
          aborted: b.aborted === true || undefined, durMs: b.dur_ms, runId: b.run_id,
          ...(b.analysis ? { analysis: b.analysis } : {}),
        })
        break
      case 'system':
        blocks.push({ kind: 'system', id: b.id, text: b.text ?? '', ...(b.tone ? { tone: b.tone as 'info' | 'success' | 'error' } : {}) })
        break
      case 'error':
        // 重试进度随快照恢复（2026-09-18）：刷新/重连后倒计时与「已等待时长」仍能算出来。
        blocks.push({
          kind: 'error', id: b.id, text: b.text ?? '',
          ...(b.ts != null ? { ts: b.ts } : {}),
          ...(b.retrying === true ? { retrying: true } : {}),
          ...(b.retry_attempt != null ? { retryAttempt: b.retry_attempt } : {}),
          ...(b.retry_max_attempts != null ? { retryMax: b.retry_max_attempts } : {}),
          ...(b.retry_delay_ms != null ? { retryDelayMs: b.retry_delay_ms } : {}),
          ...(b.retry_started_at != null ? { retryStartedAt: b.retry_started_at } : {}),
          ...(b.retry_cost_usd != null ? { retryCostUsd: b.retry_cost_usd } : {}),
        })
        break
      case 'artifacts':
        // 本轮产出汇总（AgentEnd.Artifacts 落库）→ 恢复/刷新后产出卡片仍在。
        // 无有效项 → 跳过（不产生空卡片）。
        {
          const items = pickArtifacts(b.artifacts)
          if (items) {
            blocks.push({
              kind: 'artifacts', id: b.id, items,
              ...(b.run_id ? { runId: b.run_id } : {}),
              ...(b.artifacts_truncated === true ? { truncated: true } : {}),
            })
          }
        }
        break
      default:
        break // 未知 kind：跳过（受版本约束的诊断不由 renderer 渲染）
    }
  }
  return {
    ...emptyView(),
    blocks,
    approvals: [],
    runNodes: (cp.run_nodes ?? []).map((n) => ({
      id: n.id, parentId: n.parent_id, label: n.label, status: n.status as RunNode['status'],
      depth: n.depth, kind: n.kind as RunNode['kind'],
      ...(n.name ? { name: n.name } : {}),
      ...(n.args ? { args: n.args } : {}),
      ...(n.result ? { result: n.result } : {}),
      ...(n.is_error ? { isError: true } : {}),
      ...(n.started_at != null ? { startedAt: n.started_at } : {}),
      ...(n.dur_ms != null ? { durMs: n.dur_ms } : {}),
      ...(n.task_id ? { taskId: n.task_id } : {}),
      ...(n.promoted ? { promoted: true } : {}),
    })),
    diffs: (cp.diffs ?? []).map(cpDiff),
    compressing: false,
    todos: (cp.todos ?? []).map(cpTodo),
    usage: cpUsage(cp.usage),
    ctxTokens: cp.ctx_tokens,
    ctxTokensEstimated: cp.ctx_tokens_estimated,
    contextWindow: cp.context_window,
    turns: (cp.turns ?? []).map((t) => ({
      model: t.model, input: t.input, output: t.output, cacheRead: t.cache_read, cacheWrite: t.cache_write ?? 0, reasoning: t.reasoning,
      durMs: t.dur_ms, thinkMs: t.think_ms, ttftMs: t.ttft_ms, costUsd: t.cost_usd, runId: t.run_id,
    })),
    costUsd: cp.cost_usd ?? 0,
  }
}

// hydrateCheckpoint 用 bridge 的 view_checkpoint 投影重建会话视图（一次提交）。
// 不在 hydrate 前叠加 preBuf；响应在途事件由 preBuf 暂存、hydrate 后补应用（同 snapshot 路径）。
export function hydrateCheckpoint(
  sid: string,
  cp: CheckpointView,
  requestId?: number,
  meta?: { revision?: number; clearGeneration?: number; degraded?: boolean },
): void {
  // 同步块 id 计数器：checkpoint 的块 id 由 bridge 分配（从 1 起），后端实时事件的
  // 新块必须以更大 id 追加，否则与历史块 id 重复导致按 id 更新错块。
  let maxID = 0
  for (const b of cp.blocks ?? []) {
    if (typeof b.id === 'number' && b.id > maxID) maxID = b.id
  }
  if (maxID >= blockSeq) blockSeq = maxID + 1
  // 完整快照必须从空视图重建（防重复恢复翻倍）
  useAppStore.setState((st) => ({ views: { ...st.views, [sid]: emptyView() } }))
  materializing = true
  try {
    // checkpoint 结构校验：非法/缺字段时抛错，走 finally 恢复，不留空 view
    if (!cp || typeof cp !== 'object' || !Array.isArray(cp.blocks)) {
      throw new Error('checkpoint 结构非法: blocks 缺失')
    }
    // checkpoint 已是稳定投影（streaming 恒 false、running 已由 bridge 归一化），
    // 仍防御式归一化残留态（与 snapshot 路径共用 finalizeReplayView 语义）。
    const v = finalizeReplayView(checkpointToView(cp))
    useAppStore.setState((st) =>
      setView(st, sid, {
        ...v,
        ...(meta?.revision != null ? { checkpointRevision: meta.revision } : {}),
        ...(meta?.clearGeneration != null ? { checkpointGeneration: meta.clearGeneration } : {}),
        // degraded 始终写入（false 也保留可读标记，供 UI/后续校验）
        ...(meta?.degraded != null ? { degraded: meta.degraded } : {}),
      }),
    )
  } finally {
    const buffered = preBuf.get(sid)
    preBuf.delete(sid)
    if (buffered && buffered.length) {
      for (const b of coalesceEvents(buffered)) {
        try {
          dispatchEvent(b)
        } catch (err) {
          console.error('checkpoint 补应用跳过异常事件:', (b as AnyEvent)?.event_type, err)
        }
      }
    }
    materializing = false
    if (requestId == null || snapshotPending.get(sid) === requestId) snapshotPending.delete(sid)
    // checkpoint 落地成功 → 清空该会话的重试账（见 scheduleRestoreRetry）。放 finally 里
    // 是为了覆盖「hydrate 抛错」的反面：只有真落地了才清（抛错时视图为空，重试账保留）。
    if (isViewHydrated(sid)) clearRestoreRetry(sid)
    evictViews(sid)
  }
}

// —— 视图缓存 LRU：最多保留 5 个非运行 session view；当前会话和所有正在运行的会话
// 均不可淘汰。因此并发运行数超过 5 时允许临时突破上限，运行结束后立即按 LRU 收敛。
const VIEW_CACHE_MAX = 10
const viewOrder: string[] = [] // 最近访问在尾部
function touchView(sid: string): void {
  const i = viewOrder.indexOf(sid)
  if (i >= 0) viewOrder.splice(i, 1)
  viewOrder.push(sid)
}
function evictViews(keep?: string): void {
  const st = useAppStore.getState()
  const keepSet = new Set([st.activeSessionId, keep].filter((x): x is string => Boolean(x)))
  for (const [sid, view] of Object.entries(st.views)) if (sessionBusy(view)) keepSet.add(sid)
  const cands = Object.keys(st.views)
    .filter((sid) => !keepSet.has(sid))
    .sort((a, b) => viewOrder.indexOf(a) - viewOrder.indexOf(b))
  const over = Object.keys(st.views).length - VIEW_CACHE_MAX
  if (over <= 0 || cands.length === 0) return
  const drop = cands.slice(0, Math.min(over, cands.length))
  const views = { ...st.views }
  for (const sid of drop) delete views[sid]
  for (const sid of drop) {
    const i = viewOrder.indexOf(sid)
    if (i >= 0) viewOrder.splice(i, 1)
  }
  useAppStore.setState({ views })
}

let wired = false
function wire(): void {
  if (wired) return
  wired = true
  transport.onEvent((line) => {
    try {
      const ev = JSON.parse(line) as AnyEvent
      // 快照分流（FIX_OPTIMIZATION 问题2③）：new_session 快照在途时，发往该 sid 的事件先入
      // preBuf 暂存（防御，正常为空；响应到达后由 materializeSnapshot / 下方补应用），
      // 其余事件正常分发。
      if (ev.event_type === 'command_response') {
        // 先记命令类型（dispatchEvent 内会清理 pending），再分发
        const isNewSessionResp = pending[ev.id as number]?.type === 'new_session'
        dispatchEvent(ev)
        // messages 降级路径（restored!=="snapshot"）的恢复窗口事件补应用；
        // snapshot 路径由 materializeSnapshot 内部处理（此处 preBuf 已空，幂等）。
        if (isNewSessionResp) {
          const resp = ev as AnyEvent & { data?: { session_id?: unknown; restored?: unknown } }
          const sid = typeof resp.data?.session_id === 'string' ? resp.data.session_id : undefined
          if (sid && snapshotPending.get(sid) === ev.id && resp.data?.restored !== 'snapshot') {
            snapshotPending.delete(sid)
            const buffered = preBuf.get(sid)
            preBuf.delete(sid)
            if (buffered) for (const b of coalesceEvents(buffered)) dispatchEvent(b)
          }
        }
      } else {
        const sid = typeof ev.session_id === 'string' && ev.session_id ? ev.session_id : ''
        if (sid && snapshotPending.has(sid)) {
          const arr = preBuf.get(sid) ?? []
          arr.push(ev)
          preBuf.set(sid, arr)
        } else {
          dispatchEvent(ev)
        }
      }
    } catch {
      /* 非 JSON 行（bridge 日志）忽略 */
    }
  })
  transport.onExit((info: BridgeExitInfo) => {
    useAppStore.setState({ bridgeStatus: 'error', bridgeError: `bridge 退出 (${info.code})` })
  })
}

// —— 使用统计（设置页「使用统计」/ 空会话近 30 天图表）——
// 报告缓存 TTL：全量 JSONL 扫描是数百毫秒级，入口（设置页、新建会话）不得每次都重跑。
const METRICS_TTL_MS = 60_000
// metricsQueryFrom 最近一次 metrics 查询的窗口起点（bridge 不回显查询 → 记在此处供响应落库）。
let metricsQueryFrom = ''
// localDayKey 本地自然日 key（YYYY-MM-DD）；与 bridge 的 ByDay 分组口径一致（Local()）。
export function localDayKey(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}
// metricsWindowFrom 统计窗口起点：近一年（今天往前 364 天 = 365 天窗口，含今天）。
// 热力图要画一年，7/30 天视图由前端切这份报告 —— 一次扫描服务两个时间粒度。
export function metricsWindowFrom(days = 365): string {
  const d = new Date()
  d.setDate(d.getDate() - (days - 1))
  return localDayKey(d)
}

export const useAppStore = create<AppState>((set, get) => ({
  bridgeStatus: 'idle',
  workspace: initWorkspace, // 持久恢复（模块加载时读 localStorage）；无 → undefined（BootScreen）
  projects: initProjects,
  sessions: [],
  titleOverrides: initTitles,
  skills: [],
  externalSkillsStatus: undefined,
  externalSkillsLoading: false,
  externalSkillsImporting: false,
  externalSkillsError: undefined,
  externalSkillsImported: undefined,
  markets: [],
  mcpServers: [],
  mcpLayers: [],
  mcpSaving: false,
  mcpProjects: [],
  mcpProjectsLoading: false,
  mcpProjectSaving: false,
  hooksInfo: undefined,
  hooksLoading: false,
  hooksSaving: false,
  plugins: [],
  imSchemas: [],
  imStatuses: {},
  imChats: {},
  browserPages: [],
  unreadViews: [],
  browserBusy: false,
  embedAiActive: false,
  embedLastAction: '',
  views: {},
  // 阶段 5：重启恢复的标签集（模块加载时同步读 localStorage，见 loadPersistedTabs）。
  tabsByScope: initTabsByScope,
  filePreviews: {},
  recentSessions: initRecent,
  leftExpanded: true,
  leftPinned: false,
  rightExpanded: false,
  rightTab: 'activity',
  rightSize: null,
  sessionLoading: false,
  inSkills: false,
  showNewWsModal: false,
  inSettings: false,
  inCron: false,
  cronJobs: [],
  cronRuns: [],
  cronLoading: false,
  cronEnabled: true,
  cronAutoClean: true,
  cronRunsJob: null,
  activeCronRun: undefined,
  cronDetailLoading: false,
  cronDetailOpen: false,
  settings: DEFAULT_SETTINGS,
  meshStatus: null,
  toast: null,
  providerKeys: {},
  oauthStatus: {},
  fetchedModels: {},
  modelCosts: {}, // 注册表内置价（list_models capabilities.cost；价格编辑器空覆盖回显）
  providerPresets: [], // 启动后经 fetchProviderPresets 拉取（providers.json 单一事实源）
  // 用户偏好持久恢复（问题 2，2026-08-17）：mode/model/effort/persona 从
  // localStorage 全局默认恢复；会话级记录在 new_session 响应时应用（applySessionPrefs）
  mode: persistedPrefs.mode ?? 'auto', // 默认自动执行（bash/write/edit 均不弹审批；工具层静态拦截/沙箱仍生效）
  effort: persistedPrefs.effort ?? 'low',
  persona: (persistedPrefs.persona ?? 'code') as 'code' | 'work',
  model: persistedPrefs.model ?? DEFAULT_MODEL,
  blocks: [],
  approvals: [],
  runNodes: [],
  traceEvents: [],
  diffs: [],
  todos: [],
  usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 },
  git: { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true },
  gitFiles: [],
  gitLoading: false,
  wsGit: undefined,
  wsGitLoading: false,
  wsGitError: undefined,
  wsGitFiles: [],
  wsGitHistory: [],
  wsGitHasMore: false,
  wsGitHistLoading: false,
  wsGitAt: undefined,
  wsFileDiffs: {},
  wsCommitDiffLoading: undefined,
  gitOpBusy: undefined,
  gitOpError: undefined,
  gitBranches: undefined,
  ctxTokens: undefined,
  ctxTokensEstimated: undefined,
  turns: [],
  costUsd: 0,
  toolStartTs: {},
  streaming: false,
  pendingQueue: [],
  runningAgentCount: 0,
  busySids: new Set<string>(),
  compressing: false,
  compressStartTs: undefined,
  compressBefore: undefined,
  compressRunId: undefined,
  compressReason: undefined,
  aborted: false,
  gitHistory: [],
  thinkStartTs: {},
  turnThinkMs: {},
  lastLLMBlockId: undefined,

  // 启动：传 workspace = 用户选目录（全新单项目）；不传 = 持久恢复（workspace/projects 已在
  // 模块初始化时从 localStorage 同步设好，此处只起 bridge + 水合各项目会话）。
  start: async (workspace?: string) => {
    void get().loadSettings() // 全局设置水合（theme/lang/provider；不依赖 workspace，先于 bridge 起）
    if (booting || get().bridgeStatus === 'starting' || get().bridgeStatus === 'running') return
    booting = true
    wire()
    if (workspace) {
      // 用户选目录（BootScreen）：全新项目树
      set({
        bridgeStatus: 'starting',
        workspace,
        projects: [{ path: workspace, sessions: [] }],
        sessions: [],
        views: {},
        ...resetView(),
      })
    }
    const active = workspace || get().workspace
    if (!active) {
      booting = false
      return // 无持久项目 → BootScreen 引导选目录
    }
    persistProjects(get())
    const res = await transport.start(active)
    if (!res.ok) {
      booting = false
      set({ bridgeStatus: 'error', bridgeError: res.error })
      return
    }
    booting = false
    set({ bridgeStatus: 'running' })
    // 启动即自动加载当前工作区 .agents/skills + .go-code/skills 技能清单（SkillsPage / 斜杠命令补全数据源；
    // 避免用户必须先进 Skills 页才会拉取技能列表）。
    get().fetchSkills()
    get().loadWsGit() // 启动水合同样自动识别当前工作区的 git 仓库状态（与 setActiveWorkspace 对齐）
    get().loadWsGitHistory()
    // 水合所有已注册项目会话列表；仅登记活跃路径到 recoverPaths —— 其 list 响应
    // 自动恢复上次会话 / 空则建首个会话（BootScreen 选目录 → 落一个会话的既有体验）。
    recoverPaths.add(active)
    // 水合门控：list 到达前，ask 只缓冲不自动建会话（防重载后打字新建空会话丢上下文）
    hydrating = true
    for (const p of get().projects.map((x) => x.path)) send({ type: 'list', payload: { workspace: p } })
  },

  // 启动自动恢复：App 挂载时调用。有持久项目（模块初始化已设 workspace）→ 起 bridge + 水合；
  // 无 → 不动（BootScreen 的 mock 自动 start 兜底；真 Electron 等用户选目录）。
  // 注意：React passive effect 子先于父，BootScreen 的自动 start 会先跑——
  // 但持久恢复路径下 workspace 已初始化，BootScreen 根本不会挂载，不会覆盖项目树。
  autoBoot: () => {
    if (booting || get().bridgeStatus !== 'idle') return
    if (!get().workspace) return
    void get().start()
  },

  // 重命名会话：标题覆盖写 localStorage + 项目树会话标题即时更新；清空 = 恢复自动标题
  renameSession: (path, sid, title) => {
    const clean = title.trim()
    const overrides = { ...get().titleOverrides }
    const key = titleKey(path, sid)
    if (clean) overrides[key] = clean
    else delete overrides[key]
    saveJSON(LS_TITLES, overrides)
    set((s) => ({
      titleOverrides: overrides,
      projects: s.projects.map((p) =>
        p.path === path
          ? { ...p, sessions: p.sessions.map((x) => (x.id === sid ? { ...x, title: clean || x.title } : x)) }
          : p
      ),
    }))
  },

  // 新建会话：先判断是否已选工作区 —— 已选 → 直接在当前工作区建会话（不弹选择器）；
  // 未选 → 先选/建文件夹注册为工作区，再在其中建会话。
  newSession: async () => {
    const ws = get().workspace
    if (!ws) {
      const path = await transport.pickWorkspace() // Electron=真实目录；mock=轮换假项目
      if (!path) return
      get().setActiveWorkspace(path)
    }
    persistProjects(get())
    send({ type: 'new_session' }, true) // send() 注入当前 workspace → 在活跃工作区下建会话
  },

  // 切换活跃工作区：注册（缺失则加入项目树）→ 激活 → 水合该工作区会话列表。
  // 不登记 recoverPaths → list 只并入列表，不自动建/恢复会话（新建会话是显式动作）。
  setActiveWorkspace: (path) => {
    const s = get()
    if (path === s.workspace) return
    ++navigationGeneration
    // 切换工作区：作废未投递的自动建会话缓冲（等待期切走，防消息进错工作区）
    sessionPending = false
    firstAsks.length = 0
    const existing = s.projects.find((p) => p.path === path)
    set({
      ...resetView(),
      ...clearBrowser(), // 浏览器按工作区隔离：切走清空上一个工作区的页列表/截图/loading
      activeSessionId: undefined,
      workspace: path,
      skills: [], // 切工作区：清空上个工作区的技能清单，随即拉取新工作区 .agents/skills + .go-code/skills
      ctxBreakdown: undefined, // 切工作区：上下文构成分区属会话，随 activeSessionId 一并清空
      projects: orderProjects(existing ? s.projects : [...s.projects, { path, sessions: [] }]),
      sessions: existing ? existing.sessions : [],
    })
    persistProjects(get())
    send({ type: 'list', payload: { workspace: path } })
    send({ type: 'skills', payload: { workspace: path } }) // 自动加载对应工作目录内 .agents/skills + .go-code/skills
    get().loadWsGit() // 工作区级 git 自动识别：无需先建会话，状态栏/变更面板立即可见
    get().loadWsGitHistory() // 提交历史同步拉取（docs/DESKTOP_GIT.md 刷新策略矩阵）
  },

  // 工作区置顶只影响左栏排序，不改变活跃工作区；置顶状态随项目树写入 localStorage。
  // 置顶顺序按操作顺序追加到已置顶区，最多允许 5 个工作区。
  toggleWorkspacePin: (path) => {
    const s = get()
    const project = s.projects.find((p) => p.path === path)
    if (!project) return
    const pinnedCount = s.projects.filter((p) => p.pinned).length
    if (!project.pinned && pinnedCount >= MAX_WORKSPACE_PINS) return
    const projects = orderProjects(s.projects.map((p) => (p.path === path ? { ...p, pinned: !p.pinned } : p)))
    set({ projects })
    persistProjects(get())
  },

  // 会话置顶/取消置顶（每工作区上限 5，持久化到 bridge ~/.go-code/sessions/<wsKey>/sessions.json）。
  // 乐观更新本地 + 调 bridge 落盘；失败回滚（上限拒绝等）。
  toggleSessionPin: (path, sid) => {
    const s = get()
    const project = s.projects.find((p) => p.path === path)
    if (!project) return
    const cur = project.sessions.find((x) => x.id === sid)
    if (!cur) return
    const nextPinned = !cur.pinned
    const pinnedCount = project.sessions.filter((x) => x.pinned).length
    if (!cur.pinned && pinnedCount >= MAX_SESSION_PINS) return // 已达上限
    const projects = s.projects.map((p) =>
      p.path === path
        ? { ...p, sessions: p.sessions.map((x) => (x.id === sid ? { ...x, pinned: nextPinned } : x)) }
        : p,
    )
    set({ projects })
    const id = send({ type: 'session_pin', payload: { workspace: path, session_id: sid, pinned: nextPinned } })
    // 失败回滚（bridge 拒绝：上限/未知会话）
    onCommandResponseOnce(id, (r) => {
      if (!r.ok) {
        const s2 = get()
        set({
          projects: s2.projects.map((p) =>
            p.path === path
              ? { ...p, sessions: p.sessions.map((x) => (x.id === sid ? { ...x, pinned: cur.pinned } : x)) }
              : p,
          ),
        })
      }
    })
  },

  // 从顶部快捷栏移除该会话（仅移除快捷项，不删除会话）；持久化同步。
  removeRecentSession: (path, sid) => {
    const s = get()
    const recentSessions = s.recentSessions.filter((r) => !(r.path === path && r.id === sid))
    if (recentSessions.length === s.recentSessions.length) return // 不在队列中
    set({ recentSessions })
    persistProjects(get())
  },

  // 「选择文件夹…」入口：原生选择器选已有文件夹（对话框内也可新建文件夹）
  addWorkspace: async () => {
    const path = await transport.pickWorkspace()
    if (!path) return
    get().setActiveWorkspace(path)
  },

  // 在指定工作区下直接建会话（每工作区悬停 +）：非活跃先切为活跃，再建会话
  createSessionIn: (path) => {
    if (path !== get().workspace) get().setActiveWorkspace(path)
    send({ type: 'new_session' }, true)
  },

  // 会话切换：有缓存 view（进程内看过/后台累积）→ 直接镜像顶层，不发请求；无缓存 → 发 new_session 恢复
  setActiveSession: (id, projectPath) => {
    const s = get()
    const path = projectPath || s.workspace || '' // 兜底空串：recentSessions 要求 string（无 workspace 时不会匹配真实会话）
    const sameProject = path === s.workspace
    // 先守卫「已活跃会话重点击」：无变化直接返回，避免误闪加载遮罩。
    // 例外（2026-09-21）：该会话的恢复曾失败（重试上限用尽、视图只剩重载后的内容）时，
    // 重点击就是用户的手动补救动作 —— 不拦，走下面的恢复分支重拉一次（否则只能切走再切回）。
    if (id === s.activeSessionId && sameProject) {
      if (!restoreGaveUp.has(id)) return
      clearRestoreRetry(id)
      set({ sessionLoading: true })
      if (switchTimer) clearTimeout(switchTimer)
      switchTimer = setTimeout(() => set({ sessionLoading: false }), 3000)
      send({ type: 'new_session', payload: { id } }, false)
      return
    }
    // 记录最近点开（顶部横条 + 快捷键切换）：队列语义见 withRecent —— 新打开的
    // 追加到队尾；已存在的保持原位不动；满员（RECENT_MAX）才挤掉队首（最久未看）。
    const recentSessions = withRecent(s.recentSessions, path, id)
    // 2026-08-22：显式会话点击 = 一次导航 —— 递增代际。否则在途的「发送时自动建会话」
    // new_session 响应仍会被判定为当前导航，覆盖用户点选的会话（session 串的主因：
    // 缓存分支无命令发出，此前不递增 → 旧响应"劫持"点击 + 把 flush 消息投进错误会话）。
    ++navigationGeneration
    // 切换前把当前顶层镜像写回当前会话。事件处理器通常会同步更新
    // views，但切换可能与最后一批 React/Zustand 更新交错；只读 views 会
    // 导致切回时加载到旧历史（常见表现：只剩最后一条 user）。
    const currentId = s.activeSessionId
    const views = currentId && s.views[currentId]
      ? { ...s.views, [currentId]: { ...s.views[currentId], ...topViewFields(s) } }
      : s.views
    // 发送时创建在途（firstAsks 缓冲）且用户已点选明确的缓存会话 → 缓冲消息转入该会话
    // 暂存队列：确定归属（不误投自动建的新会话）、不丢失（过期响应不再 flush 它们——
    // 代际已递增；会话空闲立即推送）。无缓存会话时保留缓冲，交给本次 new_session 响应 flush。
    let movedToSession = false
    if (firstAsks.length && views[id]) {
      const moves = firstAsks.splice(0)
      sessionPending = false
      movedToSession = true
      const target = views[id] ?? emptyView()
      views[id] = {
        ...target,
        pendingQueue: [
          ...target.pendingQueue,
          ...moves.map((m) => ({
            id: blockSeq++,
            contents: m.contents ?? (m.text ? [{ type: 'text' as const, content: m.text }] : []),
          })),
        ],
      }
    }
    const nextState = { ...s, views }
    // 缓存 view 是否「已恢复」：git_snapshot 等响应会在 new_session checkpoint 响应
    // 回来前就创建 views[sid]（只有 git 字段、blocks 为空）。若仅按 views[id] 存在
    // 判断缓存，快速切换时会命中「空 view」→ 不发 new_session → 历史永不加载。
    // 判定：blocks 有内容，或带 checkpointRevision（hydrate 元数据），或有消息块。
    const isHydrated = (sid: string): boolean => {
      const v = views[sid]
      if (!v) return false
      if (v.blocks.length > 0) return true
      if (v.checkpointRevision != null) return true
      return false
    }
    // 切换会话 → 短暂加载态（模糊遮罩通知「正在加载会话」）。
    // 缓存会话：内容即时可见，仅一小段模糊淡出后清除；未缓存会话：等 new_session 真实加载
    // 完成（其处理里 clearTimeout + 置 sessionLoading:false）再清除——避免「模糊先消失、历史
    // 才姗姗来迟」的割裂感。下方 timeout 只是兜底护栏，防止加载失败时卡死。
    set({ sessionLoading: true })
    if (switchTimer) clearTimeout(switchTimer)
    switchTimer = setTimeout(() => set({ sessionLoading: false }), isHydrated(id) ? 250 : 3000)
    // 浏览器拉取是全局的：切会话后上一个会话在途的 browser_* 请求可能已失效/挂起，残留的
    // browserBusy 会挡住新挂载 web tab 的 fetch（都看 `!browserBusy`）→ 先回落 busy，让
    // 新会话的页面/截图即时重新拉取，不再永久卡「拉取中…」。
    disarmBrowserBusyWatchdog()
    if (!sameProject) {
      const proj = s.projects.find((p) => p.path === path)
      set({ ...resetView(), browserBusy: false, ...clearBrowser(), activeSessionId: undefined, workspace: path, sessions: proj ? proj.sessions : [] })
    }
    if (isHydrated(id)) {
      touchView(id)
      set({ activeSessionId: id, views, ...loadViewToTop(nextState, id), browserBusy: false })
      applySessionPrefs(path || get().workspace || '', id)
      // 恢复后立即固化为该会话自己的记录：老会话（无会话级记录）首次查看时即
      // 冻结本次恢复的配置，不再持续跟随「最近一次全局选择」（模型串 Session 的表现之一）。
      persistPrefs(get())
      get().refreshGit()
      // 缓冲消息已转入本会话暂存队列：空闲立即推送（与 ask 的即时发送语义一致）
      if (movedToSession) get().flushPending(id)
    } else {
      set({ ...resetView(), activeSessionId: id, browserBusy: false })
      touchView(id)
      // 显式切换 = 一次新的恢复意图：清掉该会话的重试账，给这次尝试完整的重试预算
      //（此前若已放弃，不重置就永远不再自动重试 —— 见 scheduleRestoreRetry）。
      clearRestoreRetry(id)
      // 注意：setActiveSession 开头已 ++navigationGeneration（本次点击 = 一次导航），
      // send 不能再传 navigation=true（否则 pending 记录的代际 = 二次递增后的值，
      // 响应在途时任何并发导航会再 ++ → 校验失败 → 响应被静默丢弃 → 历史不显示）。
      send({ type: 'new_session', payload: { id } }, false)
      get().refreshGit()
    }
    evictViews(id)
    set({ recentSessions })
    persistProjects(get()) // 记录最近会话（重启后恢复用）
  },

  // 删除会话：桥命令（停进程内会话 + 删磁盘 sessions/events 日志）+ 本地状态移除
  //（项目树 / 视图 / 标题覆盖 / 顶部快捷栏 recentSessions）。活跃会话被删 → 切同工作区第一条，无剩余则清空。
  deleteSession: (path, sid) => {
    send({ type: 'delete_session', payload: { workspace: path, session_id: sid } })
    const s = get()
    const overrides = { ...s.titleOverrides }
    delete overrides[titleKey(path, sid)]
    const views = { ...s.views }
    delete views[sid]
    // 标签集随会话一起回收（否则重开同名会话会捡到旧标签；也防 tabsByScope 无界增长）
    const tabsByScope = { ...s.tabsByScope }
    delete tabsByScope[sid]
    const recentSessions = s.recentSessions.filter((r) => !(r.path === path && r.id === sid))
    saveJSON(LS_TITLES, overrides)
    const projects = s.projects.map((p) => (p.path === path ? { ...p, sessions: p.sessions.filter((x) => x.id !== sid) } : p))
    set({ projects, titleOverrides: overrides, views, tabsByScope, recentSessions })
    if (s.activeSessionId === sid) {
      const next = projects.find((p) => p.path === path)?.sessions[0]?.id
      if (next) get().setActiveSession(next, path)
      else set({ activeSessionId: undefined, ...resetView() })
    }
    persistProjects(get())
    // 清理持久化 lastSession：若指向被删会话 → 清掉（连同 lastSessionAt，否则重启会重建一个空会话）
    const saved = loadJSON<PersistProjects>(LS_PROJECTS)
    if (saved?.lastSession?.id === sid) saveJSON(LS_PROJECTS, { ...saved, lastSession: undefined, lastSessionAt: undefined })
    // R0：销毁该会话的内嵌预览视图。视图池是进程级单例，删会话不会自动回收 →
    // 不清理就是内存泄漏 + 僵尸渲染进程（每个视图一个渲染进程）。
    void (async () => {
      const d = window.desktop
      if (!d?.embedStatus || !d?.embedCloseTarget) return
      try {
        const s2 = (await d.embedStatus()) as { targets?: { id: string; sid?: string }[] } | undefined
        for (const t of s2?.targets ?? []) if (t.sid === sid) await d.embedCloseTarget(t.id)
      } catch {
        /* 清理失败不影响会话删除本身 */
      }
    })()
  },

  // 删除工作区：桥命令（停全部会话 + 删 {ws}/.go-code 元数据，保留用户文件）+ 本地移除
  //（项目 + 该工作区全部标题覆盖/视图）。活跃工作区被删 → 切到剩余第一个项目；无 → idle。
  deleteWorkspace: (path) => {
    send({ type: 'delete_workspace', payload: { workspace: path } })
    const s = get()
    const overrides = { ...s.titleOverrides }
    for (const k of Object.keys(overrides)) if (k.startsWith(titleKey(path, ''))) delete overrides[k]
    saveJSON(LS_TITLES, overrides)
    const wsSids = s.projects.find((p) => p.path === path)?.sessions.map((x) => x.id) ?? []
    const views = { ...s.views }
    for (const sid of wsSids) delete views[sid]
    // 标签集同工作区会话一并回收（含「无会话工作区」用工作区路径当 scope 的那一份，D7）
    const tabsByScope = { ...s.tabsByScope }
    for (const sid of wsSids) delete tabsByScope[sid]
    delete tabsByScope[path]
    // 顶部快捷栏（recentSessions）同工作区会话一并移除
    const recentSessions = s.recentSessions.filter((r) => r.path !== path)
    const projects = s.projects.filter((p) => p.path !== path)
    if (s.workspace === path) {
      if (projects.length) {
        set({ projects, titleOverrides: overrides, views, tabsByScope, recentSessions })
        get().setActiveWorkspace(projects[0].path)
      } else {
        sessionPending = false
        firstAsks.length = 0 // 工作区清空：作废自动建会话缓冲
        set({ projects, titleOverrides: overrides, views, tabsByScope, recentSessions, ...resetView(), workspace: undefined, activeSessionId: undefined, sessions: [], wsGit: undefined, wsGitLoading: false, wsGitError: undefined, wsGitFiles: [], wsGitHistory: [], wsGitHasMore: false, wsGitAt: undefined, wsFileDiffs: {}, wsCommitDiffLoading: undefined, gitOpBusy: undefined, gitOpError: undefined, gitBranches: undefined })
      }
    } else {
      set({ projects, titleOverrides: overrides, views, tabsByScope, recentSessions })
    }
    persistProjects(get())
    // 清理持久化 lastSession：若指向被删工作区 → 清掉（连同 lastSessionAt）
    const saved = loadJSON<PersistProjects>(LS_PROJECTS)
    if (saved?.lastSession?.path === path) saveJSON(LS_PROJECTS, { ...saved, lastSession: undefined, lastSessionAt: undefined })
    // R0：销毁该工作区全部会话的内嵌预览视图。视图池是进程级单例，删工作区不会自动回收 →
    // 不清理就是内存泄漏 + 僵尸渲染进程（每个视图一个渲染进程）。
    void (async () => {
      const d = window.desktop
      if (!d?.embedStatus || !d?.embedCloseTarget) return
      try {
        const s2 = (await d.embedStatus()) as { targets?: { id: string; sid?: string }[] } | undefined
        for (const t of s2?.targets ?? []) if (t.sid && wsSids.includes(t.sid)) await d.embedCloseTarget(t.id)
      } catch {
        /* 清理失败不影响工作区删除本身 */
      }
    })()
  },

  // 输入队列（Q1）：空闲消息立即以 ask_batch 启动一轮；运行中连发只进本地暂存，
  // 在当前轮 llm_end 时整队一次性 ask_batch，避免逐条 IPC/逐条唤醒。
  // 斜杠命令（/技能名 /summary /clear）：不建用户块、不进暂存队列——发 command 命令由 bridge 解析为 SDK 命令执行。
  ask: (text, contents) => {
    const trimmed = text.trim()
    // 普通文本消息：单 text 块；携带附件（contents 里可含 image 等非 text 块）→ 用其作为消息内容块。
    // 允许「空文本 + 附件」的组合；纯空且无附件 → 忽略。
    const msgContents = contents && contents.length
      ? contents
      : trimmed
        ? [{ type: 'text' as const, content: trimmed }]
        : []
    if (!msgContents.length) return
    const sid = get().activeSessionId
    if (!sid) {
      // 发送时创建：无活跃会话（新工作区 / 切工作区未选会话）→ 自动建会话，
      // 消息入缓冲（2026-08-22：携带完整内容块，含图片 —— 不再静默丢弃附件），
      // new_session 响应激活后统一 flush。send() 注入当前 workspace。
      // 无工作区（BootScreen 态）时 Composer 不可达，此处防御性 return。
      if (!get().workspace) return
      if (!trimmed && !(contents && contents.length)) return
      firstAsks.push({ text: trimmed, contents: contents && contents.length ? contents : undefined })
      // 启动水合未完成：只缓冲，等 list 恢复（恢复会话激活 / 空列表回退自动建会话）——
      // 否则重载后水合前打字会新建空会话、丢要恢复的历史上下文（P3 竞态）
      if (hydrating) return
      if (!sessionPending) {
        sessionPending = true
        send({ type: 'new_session' }, true)
      }
      return
    }
    if (trimmed.startsWith('/')) {
      // 斜杠命令判定（问题十一）：仅「内置命令 / 已启用技能」的完整首词走 command 通道；
      // 未识别的 /xxx 一律按普通文本消息发送（回退约定）——此前任何 / 开头文本都被劫持进
      // 命令通道，Composer 的文本回退被架空，服务端 unknown command 直接报错死局。
      const firstTok = trimmed.split(/\s/)[0].toLowerCase()
      const known = BUILTIN_SLASH.some((c) => c.toLowerCase() === firstTok)
        || get().skills.some((x) => x.enabled && `/${x.name}`.toLowerCase() === firstTok)
      if (known) {
        send({ type: 'command', payload: { session_id: sid, line: trimmed } })
        return
      }
      // 未识别 → 落到下方普通文本消息路径（ask_batch / 暂存队列），模型看到原文自行处理
    }
    if ((get().views[sid]?.pendingQueue.length ?? 0) >= QUEUE_CAP) return // 暂存队列已满：拒发（UI 已禁用，双保险）
    // 归一化：先入本地暂存队列。运行中不逐条推送，由 llm_end 统一 flush；空闲时没有
    // 未来 llm_end，立即以单次 ask_batch 启动新一轮。
    set((s) => {
      const v = s.views[sid] ?? emptyView()
      return setView(s, sid, { ...v, pendingQueue: [...v.pendingQueue, { id: blockSeq++, contents: msgContents, pushed: false }] })
    })
    if (!sessionBusy(get().views[sid])) get().flushPending(sid)
  },

  // 编辑暂存队列某条（推送前，逐条独立编辑）：只改文本内容。纯文本消息 → 整条替换为单个
  // text 块；带附件（图片等）的消息 → 原地更新首个 text 块、保留附件；已推送项不可编辑。
  updatePending: (id, text) => {
    const trimmed = text.trim()
    if (!trimmed) return
    const sid = get().activeSessionId
    if (!sid) return
    set((s) => {
      const v = s.views[sid] ?? emptyView()
      return setView(s, sid, { ...v, pendingQueue: v.pendingQueue.map((q) => {
        if (q.id !== id || q.pushed) return q
        if (!queuedHasAttach(q)) return { ...q, contents: [{ type: 'text' as const, content: trimmed }] }
        let replaced = false
        const contents = q.contents.map((c) => {
          if (c.type === 'text' && !replaced) { replaced = true; return { ...c, content: trimmed } }
          return c
        })
        if (!replaced) contents.unshift({ type: 'text' as const, content: trimmed })
        return { ...q, contents }
      }) })
    })
  },

  // 从暂存队列移除某条（仅未推送项；已推送项已进服务端 inbox，无法撤回）。
  removePending: (id) => {
    const sid = get().activeSessionId
    if (!sid) return
    set((s) => {
      const v = s.views[sid] ?? emptyView()
      return setView(s, sid, { ...v, pendingQueue: v.pendingQueue.filter((q) => !(q.id === id && !q.pushed)) })
    })
  },

  // 暂存队列整队一次性推送（ask_batch）：仅推送未推送项并打 pushed 标记（幂等，重复调用
  // 不重复推送）。服务端消费后经 UserInputsConsumed 事件把对应消息移入对话页。
  flushPending: (sid) => {
    const s = sid ?? get().activeSessionId
    if (!s) return
    const v = get().views[s]
    if (!v) return
    const toSend = v.pendingQueue.filter((q) => !q.pushed)
    if (!toSend.length) return
    // 推送前体积预检（P0）：整批消息加成一行 JSON 经 stdin 传输，受 bridge
    // maxCommandLineBytes（4MiB）与单条图片预算（maxImageBlockBytes=3MiB）约束。
    // 采集入口已压缩（lib/imageAttach），此处是最后一道闸：撞线前**在客户端**给出
    // 可行动提示（哪条、多大、怎么改），而不是把整批推过去换回一句
    // 「command line too long」（用户无法据其行动）。
    const oversize = toSend.findIndex((q) => messageImageBytes(q.contents) > MAX_MESSAGE_IMAGE_BYTES)
    if (oversize >= 0) {
      const q = toSend[oversize]
      const bytes = messageImageBytes(q.contents)
      set((st) => {
        const nv = st.views[s] ?? emptyView()
        return setView(st, s, {
          ...nv,
          pendingQueueError: i18nT('composer.pq.tooLarge')
            .replace('{i}', String(oversize + 1))
            .replace('{size}', fmtBytes(bytes))
            .replace('{max}', fmtBytes(MAX_MESSAGE_IMAGE_BYTES)),
        })
      })
      return
    }
    // 整队一次性推送：每条消息 = 内容块数组（text/image，带 mime_type），对齐服务端 Message.Content。
    // workspace 显式带会话归属（问题十四）：flush 由后台会话的 llm_end 触发，用户此时可能
    // 已切到其他工作区/会话——send() 兜底注入的「当前全局 workspace」会把命令路由错位。
    const wsOfSession = sessionWorkspaceOf(get(), s)
    send({
      type: 'ask_batch',
      payload: {
        session_id: s,
        ...(wsOfSession ? { workspace: wsOfSession } : {}),
        messages: toSend.map((q) => ({
          content: q.contents.map((c) => ({
            type: c.type,
            content: c.content,
            ...(c.mimeType ? { mime_type: c.mimeType } : {}),
          })),
        })),
      },
    })
    const ids = new Set(toSend.map((q) => q.id))
    set((st) => {
      const nv = st.views[s] ?? emptyView()
      return setView(st, s, { ...nv, pendingQueue: nv.pendingQueue.map((q) => (ids.has(q.id) ? { ...q, pushed: true } : q)) })
    })
  },

  approve: (id, approve, editedArgs) => {
    const sid = get().activeSessionId
    // 幂等守卫：队列中已无该 id（已被另一入口决策/超时自动拒绝）→ 不再发命令。
    // Modal 与右栏任务 Tab 可能同时挂载同一队首（各带一个 60s 倒计时），
    // 无守卫时超时路径会重复 send，后端拒绝产生无谓命令与日志。
    const stillPending = sid ? (get().views[sid]?.approvals.some((a) => a.id === id) ?? false) : false
    if (!stillPending) return
    send({ type: 'approve', payload: { session_id: sid, id, approve, arguments: editedArgs } })
    // 乐观关闭审批弹窗：点批准/拒绝后立即移除该审批（不等工具执行完的 tool_run_end 兜底清除）
    // —— 此前批准后弹窗一直显示到工具跑完，体验割裂（2026-08-17）。
    if (sid) {
      set((s) => {
        const v = s.views[sid] ?? emptyView()
        return setView(s, sid, { ...v, approvals: v.approvals.filter((a) => a.id !== id) })
      })
    }
  },

  // 沙箱拦截放行决策：乐观清除 modal + send sandbox_answer（放行=该命令绕过沙箱重跑一次；
  // bash 工具结束兜底清除）
  releaseSandbox: (approve) => {
    const sid = get().activeSessionId
    if (!sid) return
    set((s) => {
      const v = s.views[sid] ?? emptyView()
      return setView(s, sid, { ...v, release: undefined }, { release: undefined })
    })
    send({ type: 'sandbox_answer', payload: { session_id: sid, approve } })
  },

  // 回答 ask_user 提问批次：乐观清除 modal + send answer_question（一次命令注入整批回答；
  // bridge 注入解阻工具；工具结束兜底清除）
  answerQuestion: (answers) => {
    const q = get().question
    const sid = get().activeSessionId
    if (!q || !sid || !answers.length) return
    set((s) => {
      const v = s.views[sid] ?? emptyView()
      return setView(s, sid, { ...v, question: undefined }, { question: undefined })
    })
    send({ type: 'answer_question', payload: { session_id: sid, batch_id: q.id, answers } })
  },

  // 把运行中的长工具调用摘离为后台任务（决策 #5：用户主动 promote 恒可用）：
  // bridge promote_task → Session.PromoteTask → 引擎批释放（主 loop 拿占位继续）；
  // task_promoted 事件把该工具行切为后台态；后台完成后 task_result_delivered 自动回传。
  // 2026-08-30（问题 1）：点击即乐观置位（按钮立即变「停止」后台态，不等事件往返），
  // 命令失败时按 id 回滚为未 promote + 行内错误提示（此前失败只有全局 bridgeError 一闪，
  // 用户观感 = 「点了没反应」）。在途登记必须在 send 之前：mock 命令响应是同步发出的，
  // send 之后才登记会漏掉同步到达的失败响应（成功路径的正常清登记同理依赖先登记）。
  promoteTask: (taskId) => {
    const sid = get().activeSessionId
    if (!sid || !taskId) return
    set((s) => {
      const v = s.views[sid] ?? emptyView()
      return setView(s, sid, {
        ...v,
        blocks: v.blocks.map((b) =>
          b.kind === 'tool' && b.taskId === taskId && b.status === 'running' && !b.promoted
            ? { ...b, promoted: true, taskStatus: 'running' as const }
            : b,
        ),
        runNodes: v.runNodes.map((n) => (n.kind === 'tool' && n.taskId === taskId ? { ...n, promoted: true } : n)),
      })
    })
    const id = cmdSeq + 1 // send() 内 ++cmdSeq 的确定值（同步代码，无并发插入）
    promotePending.set(id, { sid, taskId })
    send({ type: 'promote_task', payload: { session_id: sid, task_id: taskId } })
  },

  // 按 taskId 精确停止异步任务（agent / async_task / promoted 工具统一）：
  // bridge interrupt_task → Session.InterruptTask 精确路由（task-* → 子 agent 注册表；
  // tooltask-* → 工具批控制器）。只影响该任务自身，不中断主会话、不影响其他任务。
  // 发送前乐观置 interrupting（按钮立即反馈「中断中」并禁用重复停止）；命令失败
  //（未知/已完成任务）按 id 回滚为 running；最终由 task_result_delivered 收敛终态。
  interruptTask: (taskId) => {
    const sid = get().activeSessionId
    if (!sid || !taskId) return
    set((s) => {
      const v = s.views[sid] ?? emptyView()
      const blocks = v.blocks.map((b) => {
        if (!(b.kind === 'tool' || b.kind === 'agent' || b.kind === 'async_task') || b.taskId !== taskId) return b
        if (b.kind === 'tool') {
          return b.status === 'running' || b.interrupting
            ? { ...b, interrupting: true, taskStatus: 'interrupting' as const }
            : b
        }
        if (b.kind === 'agent' || b.kind === 'async_task') {
          return b.status === 'running' || b.status === 'interrupting'
            ? { ...b, status: 'interrupting' as const }
            : b
        }
        return b
      })
      return setView(s, sid, { ...v, blocks })
    })
    const id = send({ type: 'interrupt_task', payload: { session_id: sid, task_id: taskId } })
    interruptPending.set(id, { sid, taskId })
  },

  interrupt: () => {
    const sid = get().activeSessionId
    // 乐观复位流式态 + 立即置位 aborted：点击「停止」的瞬间客户端就不再渲染任何
    // 后续 content/reasoning chunk（防御见 content_chunk/reasoning_chunk 分支），
    // 输入框立即解除阻塞 —— 不等 agent_end(abort) 往返，杜绝「点了停止，LLM 还在
    // 流式输出到结束」的体感（线上实证：中断命令在途/路由失配时会有一段时间仍在
    // 输出，旧实现只复位 streaming、未置 aborted，回调的 chunk 会一路渲染完）。
    // 后端 abort 仍即时生效（bridge → Session.Interrupt → ac.Abort 切断流），
    // agent_end(abort) 到达后幂等覆盖（追加「（已中断）」提示）。
    if (sid) {
      set((s) => {
        const v = s.views[sid] ?? emptyView()
        return setView(s, sid, {
          ...v,
          streaming: false,
          aborted: true, // 立即置位：过滤后续残留 chunk（含后端切断前已在途的缓冲）
          llmStartTs: undefined,
          turnStartTs: undefined,
          // 冻结进行中的助手块：本地立即停止追加（块级 streaming=false），并结算
          // 进行中的思考计时（thinkStart 已打点但未 llm_end 结算 → 置 thinkMs=已耗时，
          // 「思考 ·N…」实时跳动立即停住，显示中断时刻的思考时长）
          blocks: v.blocks.map((b) => {
            if (b.kind !== 'assistant' || !b.streaming) return b
            const nb = { ...b, streaming: false }
            if (nb.thinkStart != null && nb.thinkMs == null) nb.thinkMs = Date.now() - nb.thinkStart
            return nb
          }),
        })
      })
    }
    send({ type: 'interrupt', payload: { session_id: sid } })
  },
  compact: () => {
    const sid = get().activeSessionId
    if (!sid) return
    // Command failures can happen before the SDK publishes compress_end; clear
    // the optimistic/loading state immediately in that case.
    send({ type: 'compact', payload: { session_id: sid } })
  },

  switchMode: (mode) => {
    set({ mode })
    send({ type: 'switch_mode', payload: { session_id: get().activeSessionId, mode } })
    persistPrefs(get()) // 全局默认 + 会话级记录（问题 2，2026-08-17）
  },
  switchEffort: (effort) => {
    set({ effort })
    send({ type: 'switch_effort', payload: { session_id: get().activeSessionId, effort } })
    persistPrefs(get())
  },

  // Persona 切换：乐观设置 + 当前会话下一轮生效（bridge 重建 loop）
  switchPersona: (persona) => {
    set({ persona })
    // Work 布局预设：叙述居中、右栏折叠（办公场景聚焦内容）；Code 保持现状
    if (persona === 'work') get().setRightExpanded(false)
    send({ type: 'switch_persona', payload: { session_id: get().activeSessionId, persona } })
    persistPrefs(get())
  },
  // 切换模型：provider 给定且 ≠ active → 持久化活跃 Provider 选择 + 乐观更新 UI，
  // 然后切模型（switch_model 校验基于 active provider；两命令同步先后入 bridge stdin，顺序保证）。
  // 关键：这里【不发 set_provider】—— set_provider 会让 bridge rebuildAll 重建
  // 全部会话 loop，其它会话的模型在目标 Provider 缺失时会被回退为默认模型
  // （模型串 Session 的根因之一）。模型切换由 switch_model 按 session_id 逐会话
  // 解析（bridge 内部按需 setActive + 只重建目标会话 loop）。
  // 跨 Provider 选择（Composer 多 Provider 分组下拉）：选中其他 Provider 的模型即连带切 Provider。
  switchModel: (model, provider) => {
    const s = get()
    if (provider && provider !== s.settings.provider.active) {
      // 仅持久化活跃 Provider 选择（provider:save 落 settings.json，重启后 boot 恢复；
      // 不触发 bridge 全局重建——那会把其它会话的模型/Provider 一并改写）。
      void transport.providerSave({ provider, active: provider })
      // 乐观更新活跃 Provider（bridge switch_model 也会自动切，双保险；UI 标签即时反映）
      set({ settings: { ...s.settings, provider: { ...s.settings.provider, active: provider } } })
    }
    set({ model })
    // 显式携带 provider：bridge 按它解析模型（无需依赖 set_provider 时序），
    // 模型属于哪个 Provider 就以它为准。
    send({ type: 'switch_model', payload: { session_id: s.activeSessionId, model, provider } })
    persistPrefs(get())
  },
  refreshGit: () => {
    get().loadGitSnapshot()
    get().loadGitHistory()
  },
  // 工作区级 git 探测：不带 session_id，send() 注入当前 workspace，bridge 直接返回该目录
  // 的仓库状态（is_repo/branch/变更文件…）。响应由 command_response(git_snapshot) 的
  // 「无 session」分支写入 wsGit。添加/切换工作区、启动水合时自动调用 —— 无需先建会话。
  loadWsGit: () => {
    if (!get().workspace) return
    set({ wsGitLoading: true, wsGitError: undefined })
    send({ type: 'git_snapshot', payload: {} })
  },
  // 工作区级提交历史（无会话也可浏览）；appendMore=「加载更多」追加下一页（skip=现有条数）
  loadWsGitHistory: (appendMore = false) => {
    if (!get().workspace) return
    wsHistAppend = appendMore
    set({ wsGitHistLoading: true })
    send({ type: 'git_history', payload: { limit: 30, skip: appendMore ? get().wsGitHistory.length : 0 } })
  },
  loadWsFileDiff: (path, staged = false) => {
    if (!get().workspace || !path || path.length > 4096) return
    send({ type: 'git_file_diff', payload: { path, staged } })
  },
  loadWsCommitDiff: (hash) => {
    if (!get().workspace || !/^[0-9a-fA-F]{7,64}$/.test(hash)) return
    set({ wsCommitDiffLoading: hash })
    send({ type: 'git_commit_diff', payload: { hash } })
  },
  // —— Git 写操作（P1）：统一入口置 gitOpBusy 防并发；成功/失败由 command_response 路由处理 ——
  gitStage: (paths) => gitWriteOp('git_stage', { paths }),
  gitUnstage: (paths) => gitWriteOp('git_unstage', { paths }),
  gitDiscard: (paths, untracked = false) => gitWriteOp('git_discard', { paths, untracked }),
  gitCommit: (message, stageAll = false) => gitWriteOp('git_commit', { message, stage_all: stageAll }),
  clearGitOpError: () => set({ gitOpError: undefined }),
  // —— 分支与仓库初始化（P2）——
  loadGitBranches: () => {
    if (!get().workspace) return
    send({ type: 'git_branch_list', payload: {} })
  },
  gitCheckout: (branch) => gitWriteOp('git_checkout', { branch }),
  gitInitRepo: () => gitWriteOp('git_init', {}),
  loadGitHistory: (appendMore = false) => {
    const sid = get().activeSessionId
    if (!sid) return
    sessHistAppend = appendMore
    set((s) => { const v = s.views[sid] ?? emptyView(); return setView(s, sid, { ...v, gitHistLoading: true }) })
    const skip = appendMore ? (get().views[sid]?.gitHistory.length ?? 0) : 0
    send({ type: 'git_history', payload: { session_id: sid, limit: 30, skip } })
  },
  loadCommitDiff: (hash) => { const sid=get().activeSessionId; if(sid && /^[0-9a-fA-F]{7,64}$/.test(hash)) send({type:'git_commit_diff',payload:{session_id:sid,hash}}) },
  loadGitSnapshot: () => {
    const sid = get().activeSessionId
    if (!sid || !get().workspace) return
    set((s) => { const v=s.views[sid]??emptyView(); return setView(s,sid,{...v,gitLoading:true,gitError:undefined}) })
    send({ type: 'git_snapshot', payload: { session_id: sid } })
  },
  loadGitDiff: (path, staged = false) => {
    const sid = get().activeSessionId
    if (!sid || !get().workspace || path.length > 4096) return
    send({ type: 'git_file_diff', payload: { session_id: sid, path, staged } })
  },
  loadGitFileDiff: (path, staged = false) => { get().loadGitDiff(path, staged) },
  // 定位到文件：记录精确定位请求（fileFocus）+ 请求内容，**不切 tab、不展开右栏**。
  // 为什么单列（R3）：Agent 自动打开只该「把内容准备好」——切 tab 会换掉用户正在看的那一页
  //（ADR-2 的失败模式）。用户点击路径仍走 openFile（= 切文件栏 + 展开右栏 + 定位）。
  // 与 openFile 共用**同一处** fileFocus 构造：fileFocus 是「无路径打开文件标签」的初值
  //（focusSpec），两处各写一份迟早会漏字段。
  // 「打开即见内容」（R4）的自动预览挂在 openFile（用户点击入口）里，见其注释；
  // locateFile 保持「只定位、不切 tab、不展开」的语义（Agent 后台产出走这里 = ADR-2）。
  //
  // 阶段 2 变更：**不再清空内容**。阶段 1 这里写 `filePreview: undefined`（「打开即清空旧内容，
  // 等响应回填」），在单例模型下是必要的（否则打开 B 会先看到 A 的内容）；现在内容按归一化
  // 路径索引，清空只会把**同一文件另一个标签**（diff / source 并存）打成「正在读取文件…」。
  // 内容刷新靠下面这条请求的响应覆盖同一键（见 command_response 的 file_preview 分支）。
  locateFile: (path, toolId, mode, startLine) => {
    set((s) => ({
      fileFocus: {
        path,
        ...(mode === 'source' ? {} : { toolId }),
        ...(startLine != null ? { startLine } : {}),
        seq: (s.fileFocus?.seq ?? 0) + 1,
      },
    }))
    requestFilePreview(path)
  },
  // 打开文件预览：切到文件 tab + 展开右栏 + 定位（locateFile）。
  // 可视类文件是例外：**不**切文件 tab（不建 file 标签），只展开右栏 + 写 fileFocus + 开预览
  //（「一个文件一个标签」——见函数体内注释）。
  // mode='source'（统一 open-file 能力）：完整内容视图（「最终文件」）——
  // 不携带 toolId，FilePreviewTab 不会强制落到 operation/diff 模式；
  // 支持工作区外绝对路径（bridge file_preview 已放宽）。
  // startLine：read_file 已读区间的起始行 → 「最终文件」渲染后定位滚动到该行。
  openFile: (path, toolId, mode, startLine) => {
    // 可视类文件**不建 file 标签**（2026-09 用户口径「一个文件一个标签」）：这类内容只有内嵌
    // 浏览器能渲染，file 标签里只剩一行状态（「内容已在右侧预览中打开」），与下面自动开的预览
    // 标签**同名** —— 同一个文件占两个标签，用户看到的就是重复。判据与自动预览用**同一事实源**
    // （visualKindOf），两处各判一次迟早漂移：那会出现「有 file 标签但没内容」或反之。
    // 文本类**行为完全不变**（仍建 file 标签，diff / source 两种 mode）。
    const kind = visualKindOf(path)
    if (!kind) {
      // 标签 payload 在**打开时**写入，而不是等 locateFile 之后的 fileFocus：
      //   ① 标签条要显示文件名（阶段 1），而 fileFocus 是随后的动作才写的；
      //   ② 阶段 2 要按标签隔离内容，payload 必须从打开那一刻就跟着标签走。
      // mode 口径与 locateFile 一致：显式 source，或缺 toolId → 完整预览；否则 diff/操作模式。
      get().openTab({
        kind: 'file',
        path,
        mode: mode === 'source' || !toolId ? 'source' : 'diff',
        ...(mode === 'source' || !toolId ? {} : { toolId }),
        ...(startLine != null ? { startLine } : {}),
      })
    }
    get().setRightExpanded(true)
    // 可视类**照样**写 fileFocus：文件面板的上下文仍要正确（用户之后手动开「文件」标签时
    // 看到的就是这个文件），只是当下不进文件面板。
    get().locateFile(path, toolId, mode, startLine)
    // —— 「打开即见内容」（R4，2026-09-18 用户口径）——
    //
    // 可视类文件（pdf/图片/html/svg/xlsx/docx/pptx）的内容**只有内嵌浏览器能渲染**，文件栏本体
    // 没有承载面。此前用户点开后停在「这是二进制文件……」+「预览 / 用默认程序打开」两个按钮的
    // 中间态；用户的原话是「完全没必要，我期望的就是打开就能看到文件内容」。故在**用户点击**的
    // 入口处直接替他按下那次预览（activate:true = 展开右栏 + 切到预览）。
    //
    // 为什么放在 openFile（动作）而不是文件栏组件里（第一版就是那样，被自己的 e2e 抓出缺陷）：
    // 组件在切走时**卸载**，组件内的去重 ref 随之清空 → 用户每次切回文件栏都会被再"自动打开"
    // 一次并弹回预览（实测 4ms 内把 rightTab 从 file 顶回 web，形成拉锯）。动作层天然只发生一次。
    //
    // 为什么判据是扩展名而不是 bridge 的 binary：这与产出后自动打开（autoOpenVisualFile）同一
    // 事实源（lib/visualFile.visualKindOf），文件栏随时能给出答案，不必等一次 IPC 往返；
    // 后台路径（locateFile）**不**走这里 —— ADR-2 的「不抢视线」保持不变。
    //（kind 在函数开头已经算好：同一判据还要决定建不建 file 标签，见上方注释。）
    if (!kind) return
    if (kind === 'xlsx' || kind === 'docx' || kind === 'pptx') void get().previewOfficeFile(path, kind)
    else void get().openInBrowserTab(path, { activate: true })
  },
  // ensureFilePreview：内容缓存缺失时按需补拉（阶段 2）。
  // 调用方是文件面板：激活一个 file 标签时若该路径没有缓存（切工作区清空过 / 被
  // FILE_PREVIEWS_MAX 淘汰 / 标签从「最近关闭」重开）就补一次 —— 否则用户会停在
  // 「正在读取文件…」且**没有任何东西会再来拉**（阶段 1 靠「打开即清空 + 立即请求」，
  // 阶段 2 内容可以跨标签复用，就必须补上这条兜底）。
  // 去重：同一键在途（FILE_PREVIEW_RETRY_MS 内）或已有缓存 → 不发。
  ensureFilePreview: (path) => {
    if (!path) return
    const s = get()
    const key = fileKeyOf(path, s.workspace)
    if (s.filePreviews[key]) return
    const at = filePreviewInFlight.get(key)
    if (at !== undefined && Date.now() - at < FILE_PREVIEW_RETRY_MS) return
    requestFilePreview(path)
  },

  // closeFile：「完整文件视图」头部那个 ✕（标题 rp.file.closeView = 关闭**视图**，不是关标签；
  // 关标签走标签条上的 ✕）。
  // 阶段 2 起内容与视图都归标签所有，故这里改为**清激活 file 标签的视图 payload**（保留 path
  // —— 标签标题不变，用户看到的是「回到了文件活动列表」），语义与阶段 1 的「清两个全局单例」
  // 等价（面板都退回活动列表）。mode 清掉后该标签的身份键回落 'source'，故再次打开同一文件
  // 仍命中**这个**标签（不会因为「键变了」而多开一个）。
  closeFile: () => {
    const s = get()
    const scope = scopeKeyOf(s)
    const set0 = syncTabsToRightTab(s)
    const i = set0.tabs.findIndex((t) => t.id === set0.activeId && t.kind === 'file')
    const tabs = i < 0 ? set0.tabs : set0.tabs.map((t, k) => (
      k === i ? ({ id: t.id, kind: 'file', path: (t as FileTabInstance).path } as TabInstance) : t
    ))
    set({ ...tabPatch(s, scope, { ...set0, tabs }), fileFocus: undefined })
  },

  // 拉取使用统计报告（全局用量；metrics 命令忽略注入的 workspace，返回全项目）。
  // from/to = 窗口（YYYY-MM-DD，只裁剪序列；Stats 恒为全历史）——热力图窗口取近一年，
  // 7/30 天切换在前端切同一份报告，避免为每个视图各扫一遍全量 JSONL。
  fetchMetrics: (opts) => {
    metricsQueryFrom = opts?.from ?? ''
    send({ type: 'metrics', payload: { from: metricsQueryFrom, to: opts?.to ?? '', project: opts?.project ?? '' } })
  },

  // 按需拉取：报告缺失或超过 METRICS_TTL_MS 才发命令。空会话图表每次新建会话都会挂载，
  // 不带这层就去重跑全量扫描（百万行 JSONL 数百毫秒）。
  ensureMetrics: () => {
    const st = get()
    if (st.metrics && st.metricsAt && Date.now() - st.metricsAt < METRICS_TTL_MS) return
    st.fetchMetrics({ from: metricsWindowFrom() })
  },

  // 技能清单：进 SkillsPage / 开 /skill 面板时拉取
  fetchSkills: () => send({ type: 'skills', payload: {} }),

  // 上下文构成分区：Composer ctx 环 hover 时拉取（一次往返，毫秒级；响应见 command_response
  // 分发的 context_breakdown 分支）。无活跃会话（未建会话/恢复中）→ 不发命令（面板显示空态）。
  fetchCtxBreakdown: () => {
    const sid = get().activeSessionId
    if (!sid) return
    send({ type: 'context_breakdown', payload: { session_id: sid } })
  },
  refreshExternalSkillsStatus: async () => {
    set({ externalSkillsLoading: true, externalSkillsError: undefined })
    try {
      const status = await transport.externalSkillsStatus()
      set({ externalSkillsStatus: status, externalSkillsLoading: false })
    } catch (e) {
      set({ externalSkillsLoading: false, externalSkillsError: e instanceof Error ? e.message : String(e) })
    }
  },
  importExternalSkills: async () => {
    set({ externalSkillsImporting: true, externalSkillsError: undefined, externalSkillsImported: undefined })
    try {
      const result = await transport.importExternalSkills()
      if (!result.ok || !result.result) {
        set({ externalSkillsImporting: false, externalSkillsError: result.error ?? '导入外部 Skills 失败' })
        return false
      }
      set({
        externalSkillsImporting: false,
        externalSkillsStatus: result.result.status,
        externalSkillsImported: result.result.imported?.length ?? 0,
      })
      // 全局 Skills 已在 bridge 中对所有 workspace 重建；逐工作区拉取清单。
      // 当前 workspace 的响应由 dispatchEvent 门禁写入 skills，后台 workspace 响应不串台。
      const paths = get().projects.map((p) => p.path)
      for (const workspace of paths) send({ type: 'skills', payload: { workspace } })
      return true
    } catch (e) {
      set({ externalSkillsImporting: false, externalSkillsError: e instanceof Error ? e.message : String(e) })
      return false
    }
  },
  // 启用/禁用：乐观更新 + 回拉确认（bridge 持久化到 {ws}/.go-code/skills.json（启用状态，路径不变））
  toggleSkill: (name, enabled) => {
    set((s) => ({ skills: s.skills.map((x) => (x.name === name ? { ...x, enabled } : x)) }))
    send({ type: 'skill_toggle', payload: { name, enabled } })
    send({ type: 'skills', payload: {} })
  },
  getSkill: (name) => send({ type: 'skill_get', payload: { name } }),
  closeSkill: () => set({ skillDetail: undefined }),
  // 远程技能下载（exec git，全局/工作区作用域）：装完回拉清单；name 可选 = 市场浏览后精确安装单个
  installSkill: (url, scope, name) => {
    const payload: Record<string, unknown> = { url, scope }
    if (name) payload.name = name
    send({ type: 'skill_install', payload })
    send({ type: 'skills', payload: {} })
  },
  // 浏览远程市场（skill_browse，只读）：清旧结果 → 请求；响应经 dispatchEvent 落 marketSkills
  browseMarket: (url) => {
    set({ marketSkills: undefined }) // 浏览中清旧列表（避免残留上次浏览结果）
    send({ type: 'skill_browse', payload: { url } })
  },
  // 刷新当前市场（浏览结果区「刷新」/ 市场安装/更新后）：重拉实时列表并对比本地（同 browseMarket）
  refreshMarket: (url) => {
    set({ marketSkills: undefined })
    send({ type: 'skill_browse', payload: { url } })
  },
  updateSkill: (name) => {
    send({ type: 'skill_update', payload: { name } })
    send({ type: 'skills', payload: {} })
  },
  uninstallSkill: (name) => {
    send({ type: 'skill_uninstall', payload: { name } })
    send({ type: 'skills', payload: {} })
  },
  // —— 市场管理（SkillsPage）：保存市场免手填 + 按市场更新已装技能 ——
  fetchMarkets: () => send({ type: 'market_list', payload: {} }),
  addMarket: (url) => {
    send({ type: 'market_add', payload: { url } })
    send({ type: 'market_list', payload: {} })
  },
  removeMarket: (url) => {
    send({ type: 'market_remove', payload: { url } })
    send({ type: 'market_list', payload: {} })
  },
  updateMarketSkills: (url) => {
    send({ type: 'skill_update_market', payload: { url } })
    send({ type: 'skills', payload: {} })
    send({ type: 'market_list', payload: {} })
  },

  // MCP（SettingsModal MCP tab）：清单/保存（持久化 + 热应用）/刷新
  fetchMCPServers: () => send({ type: 'mcp_list', payload: {} }),
  saveMCP: async (servers: Record<string, MCPServerCfg>) => {
    set({ mcpSaving: true })
    const r = await transport.mcpSave(servers) // Electron 写 settings.json mcpServers 段
    if (!r.ok) {
      set({ mcpSaving: false })
      return r
    }
    // 刷新 store settings（mcpServers 是 MCP tab 的配置源：移除/启用在保存后立即生效）
    const s = await transport.settingsGet()
    set({ settings: s })
    // 热应用：bridge mcp_set 重建会话工具集 + mcp_list 回拉状态
    send({ type: 'mcp_set', payload: { servers } })
    send({ type: 'mcp_list', payload: {} })
    return r
  },
  refreshMCP: (name: string) => {
    send({ type: 'mcp_refresh', payload: { name } })
    send({ type: 'mcp_list', payload: {} })
  },
  // 项目级 MCP：汇总（只读，不创建运行态）/ 保存（写 {ws}/.go-code/settings.json + 热应用）
  fetchMCPProjects: (workspaces: string[]) => {
    set({ mcpProjectsLoading: true })
    send({ type: 'mcp_projects', payload: { workspaces } })
  },
  saveMCPProject: (workspace, servers) => {
    set({ mcpProjectSaving: true })
    return new Promise<{ ok: boolean; error?: string }>((resolve) => {
      send({ type: 'mcp_project_set', payload: { workspace, servers } }, false, (r) => {
        // 无论成败都回拉：失败时也要把面板拉回磁盘真实状态（不让 UI 停在想象态）
        set({ mcpProjectSaving: false })
        send({ type: 'mcp_projects', payload: { workspaces: get().projects.map((p) => p.path) } })
        resolve(r.ok ? { ok: true } : { ok: false, error: r.error ?? '保存失败' })
      })
    })
  },

  // hooks（钩子页）：来源汇总 / 保存某来源（写盘 + 热应用）/ 信任项目命令
  fetchHooks: (workspaces: string[]) => {
    set({ hooksLoading: true })
    send({ type: 'hooks_list', payload: { workspaces } })
  },
  saveHooks: (scope, workspace, body) => {
    set({ hooksSaving: true })
    return new Promise<{ ok: boolean; error?: string }>((resolve) => {
      send({ type: 'hooks_set', payload: { scope, ...(workspace ? { workspace } : {}), body } }, false, (r) => {
        set({ hooksSaving: false })
        if (r.ok) {
          // 保存成功后回拉（以磁盘为准：bridge 会做归一化/严格校验）
          send({ type: 'hooks_list', payload: { workspaces: get().projects.map((p) => p.path) } })
          resolve({ ok: true })
        } else {
          resolve({ ok: false, error: r.error ?? '保存失败' })
        }
      })
    })
  },
  trustHooks: (commands: string[], scope: string) => {
    return new Promise<{ ok: boolean; error?: string }>((resolve) => {
      send({ type: 'hooks_trust', payload: { commands, scope: scope || 'project', reason: '用户在设置面板确认' } }, false, (r) => {
        send({ type: 'hooks_list', payload: { workspaces: get().projects.map((p) => p.path) } })
        resolve(r.ok ? { ok: true } : { ok: false, error: r.error ?? '信任失败' })
      })
    })
  },

  // 插件（SettingsModal 插件 tab）：清单/安装/卸载/启用/禁用（Browser Use 首个实例）
  fetchPlugins: () => send({ type: 'plugin_list', payload: {} }),
  installPlugin: (id: string) => {
    // 乐观 installing：plugin_install 异步（npm 30-60s），立即标记避免「点了没反应」；
    // 不做即时 plugin_list（会与异步 goroutine 竞态、把乐观态冲回 not-installed），
    // 完成后 bridge 发 plugin_status 刷新（installing → ready/error）。
    set((s) => ({ plugins: s.plugins.map((p) => (p.id === id ? { ...p, status: 'installing' } : p)) }))
    send({ type: 'plugin_install', payload: { id } })
  },
  uninstallPlugin: (id: string) => {
    send({ type: 'plugin_uninstall', payload: { id } })
    send({ type: 'plugin_list', payload: {} })
  },
  enablePlugin: (id: string) => {
    // browser 插件且 presentation=embedded：先起内嵌端点（后台、不可见），Enable 才连得上。
    // 必须用 send()（自动带 workspace）；window.desktop.command 不带 workspace 会被 bridge 拒绝。
    // 注意：不创建/显示视图——内嵌视图在真正使用 Browser Use 时才由 web tab 创建并 attach。
    const s = useAppStore.getState()
    const embedded = s.browserConfig?.presentation === 'embedded' || !s.browserConfig // 默认 embedded
    if (id === 'browser' && embedded) {
      send({ type: 'browser_embed_start', payload: { target_id: 'go-code-embed' } })
      send({ type: 'plugin_enable', payload: { id } })
      send({ type: 'plugin_list', payload: {} })
      return
    }
    send({ type: 'plugin_enable', payload: { id } })
    send({ type: 'plugin_list', payload: {} })
  },
  disablePlugin: (id: string) => {
    send({ type: 'plugin_disable', payload: { id } })
    send({ type: 'plugin_list', payload: {} })
  },

  // —— 浏览器引擎切换（mcp|ego，2026-09）——
  fetchBrowserEngine: () => {
    send({ type: 'browser_engine_get', payload: {} })
  },
  // 强制重新检测 ego lite（安装完成后调）：refresh_ego 让 bridge 忽略检测缓存，
  // 立即反映磁盘现状（已安装 → 落 ~/.go-code/ego.json 持久化记录），前端随即更新
  // installed/app_running 分层状态 → 安装按钮消失、改显示「启动 ego lite」。
  refreshEgoStatus: () => {
    send({ type: 'browser_engine_get', payload: { refresh_ego: true } })
  },
  setBrowserEngine: (engine: 'mcp' | 'ego') => {
    // 1. 落盘 settings.json（Electron settings:set 顶层 patch）；2. bridge 热应用
    // （browser_engine_set：切 ego 编排停用 browser 插件 / 切 mcp 恢复）。bridge 只读
    // settings.json，故顺序必须先落盘再发命令（命令本身带 engine payload 兜底时序）。
    const id = send({ type: 'browser_engine_set', payload: { engine } })
    return new Promise<{ ok: boolean; error?: string }>((resolve) => {
      onCommandResponseOnce(id, (r) => {
        if (r.ok) {
          set((s) => ({ browserEngine: engine, settings: { ...s.settings, browser_engine: engine } }))
          get().fetchBrowserEngine() // 拉 ego 状态
          get().fetchPlugins() // browser 插件状态可能变（ego 时禁用）
        }
        resolve(r.ok ? { ok: true } : { ok: false, error: r.error })
      })
    })
  },

  // 浏览器插件配置（插件详情设置）：browser_config_get 读有效配置；browser_config_set
  // 保存并热应用（校验失败 bridge 拒绝，错误透出）。保存成功刷新配置与插件状态。
  // IM 集成配置（im_config_get/im_schema_list/im_status；SettingsModal IM tab）
  fetchIMConfig: () => {
    send({ type: 'im_config_get', payload: {} })
    send({ type: 'im_schema_list', payload: {} })
    send({ type: 'im_status', payload: {} })
  },
  saveIMConfig: (cfg) => {
    const id = send({ type: 'im_config_set', payload: { config: cfg } })
    return new Promise((resolve) => {
      onCommandResponseOnce(id, (r) => {
        if (r.ok) get().fetchIMConfig() // 保存成功刷新（拿最新状态）
        resolve(r)
      })
    })
  },
  // IM 绑定确认（弹窗用户决策）：approve=true 写路由（chat→workspace），false 拒绝
  imBindConfirm: (chat, approve, workspace) => {
    send({ type: 'im_bind_confirm', payload: { chat, approve, workspace } })
    set({ imBindPending: undefined })
  },
  fetchIMChats: (gatewayId) => {
    send({ type: 'im_chat_list', payload: { gateway_id: gatewayId } })
  },
  fetchBrowserConfig: () => send({ type: 'browser_config_get', payload: {} }),
  // M2/M3：起内嵌端点（enable 时）；attach 内嵌视图（真正使用时）。均走 send() 带 workspace。
  startEmbedEndpoint: () => send({ type: 'browser_embed_start', payload: { target_id: 'go-code-embed' } }),
  attachEmbedView: (debuggerAddr: string) => send({ type: 'browser_embed_attach', payload: { debugger_addr: debuggerAddr } }),
  // R4：把某个预览标签标记为「已读」（未读点消失）。清除时机见 onEmbedNav（激活 + 面板可见）
  // 与用户的显式点击（点标签即激活 → 同一处清除）。
  markPreviewSeen: (id) => set((s) => (s.unreadViews.includes(id) ? { unreadViews: s.unreadViews.filter((x) => x !== id) } : {})),
  saveBrowserConfig: (cfg) => {
    const id = ++cmdSeq
    const ws = useAppStore.getState().workspace
    const payload = { ...cfg, user_data_dir: cfg.user_data_dir ?? '' } as unknown as Record<string, unknown>
    return new Promise<{ ok: boolean; error?: string }>((resolve) => {
      let timer: ReturnType<typeof setTimeout> | undefined
      const off = transport.onEvent((line) => {
        try {
          const ev = JSON.parse(line) as { event_type?: string; id?: number; ok?: boolean; error?: string }
          if (ev.event_type !== 'command_response' || ev.id !== id) return
          off()
          if (timer) clearTimeout(timer)
          if (ev.ok) {
            useAppStore.setState({ browserConfig: { ...cfg } })
            send({ type: 'plugin_list', payload: {} })
            resolve({ ok: true })
          } else {
            resolve({ ok: false, error: ev.error ?? '保存失败' })
          }
        } catch {
          /* 非 JSON 事件忽略 */
        }
      })
      timer = setTimeout(() => {
        off()
        resolve({ ok: false, error: '保存超时' })
      }, 15000)
      void transport.send({ type: 'browser_config_set', id, payload: ws ? { ...payload, workspace: ws } : payload })
    })
  },

  // 浏览器（右栏 web tab）：页列表 + 截图（browser_* 命令，走插件私有连接）
  fetchBrowserPages: () => {
    armBrowserBusyWatchdog()
    const gen = browserGeneration
    const id = send({ type: 'browser_pages', payload: {} })
    browserInFlight.set(id, { gen, kind: 'pages' })
    set({ browserBusy: true })
  },
  fetchBrowserShot: (pageId?: number) => {
    armBrowserBusyWatchdog()
    const gen = browserGeneration
    const id = send({ type: 'browser_screenshot', payload: pageId != null ? { page_id: pageId } : {} })
    browserInFlight.set(id, { gen, kind: 'screenshot', pageId })
    set({ browserBusy: true })
  },
  // 关闭指定网页（web tab 页列表 ✕）：bridge 关闭成功后用最新页列表兜底（响应即刷），
  // 这里只负责在失败时也回落 busy；成功响应的 browser_close 分支会更新页列表并清 busy。
  closeBrowserPage: (pageId: number) => {
    armBrowserBusyWatchdog()
    const gen = browserGeneration
    const id = send({ type: 'browser_close', payload: { page_id: pageId } })
    browserInFlight.set(id, { gen, kind: 'close', pageId })
    set({ browserBusy: true })
  },

  shutdown: () => send({ type: 'shutdown', payload: { session_id: get().activeSessionId } }),

  toggleLeft: () => set((s) => ({ leftExpanded: !s.leftExpanded })),
  // 右侧栏开关/tab 是会话级视图状态：写入活跃会话的 view（per-session 恢复）
  toggleRight: () => {
    const s = get()
    const sid = s.activeSessionId
    const next = !s.rightExpanded
    if (!sid) return set({ rightExpanded: next })
    const v = s.views[sid] ?? emptyView()
    set(setView(s, sid, { ...v, rightExpanded: next }))
  },
  setLeftExpanded: (b) => set({ leftExpanded: b }),
  setRightExpanded: (b) => {
    const s = get()
    const sid = s.activeSessionId
    if (!sid) return set({ rightExpanded: b })
    const v = s.views[sid] ?? emptyView()
    set(setView(s, sid, { ...v, rightExpanded: b }))
  },
  togglePin: () => set((s) => ({ leftPinned: !s.leftPinned })),
  setRightTab: (t) => {
    // 兼容入口（e2e 与既有调用点仍按 kind 切标签）：语义 = **打开或激活**该 kind 的标签。
    // 为什么不让它直接写 rightTab：那会绕过标签集，出现「切过去了、标签条上却没有这个标签」。
    get().openTab({ kind: t } as TabSpec, { activate: true })
  },
  // openTab：打开或激活一个标签。幂等键（阶段 2，见 tabKeyOf）：
  //   · file → kind + path + mode：**多实例**。同一文件可以同时有 diff 与 source 两个标签
  //     （D5），同一 (path, mode) 命中已有实例（更新 payload + openSeq，不新增）。
  //   · 其余 kind → kind 本身（每类每 scope 一个实例，阶段 1 口径不变）。
  openTab: (spec, opts) => {
    const s = get()
    const scope = scopeKeyOf(s)
    const set0 = syncTabsToRightTab(s)
    const activate = opts?.activate !== false
    // 「无路径的 file 打开」（rail 的「文件」按钮 / setRightTab('file') / e2e 复位）：payload
    // 从 fileFocus 取 —— 语义是「打开用户最后一次定位的那个文件」。这就是 T1 之后「可视类文件
    // 不建 file 标签、但用户手动点「文件」标签仍能看到它」的那条路（fileFocus 留的定位锚）。
    const full: TabSpec = spec.kind === 'file' && !spec.path ? { ...focusSpec(s), ...spec } : spec
    const key = tabKeyOf(full, s.workspace)
    let i: number
    if (key !== undefined) {
      i = set0.tabs.findIndex((t) => tabKeyOf(t, s.workspace) === key)
    } else if (full.kind === 'file') {
      // fileFocus 也没有（没定位过任何文件）→ 退化为「激活已有的 file 标签」：优先当前激活的
      // 那个，其次最近打开的那个；一个都没有才新建空标签（不猜路径）。
      const hit = set0.tabs.find((t) => t.id === set0.activeId && t.kind === 'file')
        ?? [...set0.tabs].reverse().find((t) => t.kind === 'file')
      i = hit ? set0.tabs.indexOf(hit) : -1
    } else {
      i = set0.tabs.findIndex((t) => t.kind === full.kind)
    }
    let tabs = set0.tabs
    let id: string
    if (i >= 0) {
      const prev = set0.tabs[i]
      const merged: TabInstance = full.kind === 'file'
        ? (key !== undefined
          // 命中同一 (path, mode)：payload **整体替换**（不是合并）——上一次的 toolId/startLine
          // 不能留到这一次（否则「普通点开」会沿用上次 read_file 的定位行）。openSeq 自增 =
          // 「每次打开都回默认视图」的触发器（阶段 1 是全局 fileFocus.seq，阶段 2 迁到标签上：
          // 全局 seq 会让「重开 A」把 B 的视图一起复位）。
          ? ({ ...full, id: prev.id, openSeq: ((prev as FileTabInstance).openSeq ?? 0) + 1 } as TabInstance)
          // 无路径打开命中已有 file 标签：只激活，**不动 payload**（不能把空 spec 合进去，
          // 那会把 path/mode 抹掉 → 标签变空标签）。
          : prev)
        // 其它 kind：沿用阶段 1 的「合并 payload」（web 换视图等）
        : ({ ...prev, ...full } as TabInstance)
      id = merged.id
      tabs = set0.tabs.map((t, k) => (k === i ? merged : t))
    } else {
      // file 且 spec 自带 path：payload **只取 spec** —— 不能像其它 kind 那样先铺一层
      // makeTab 的初值（那层来自 fileFocus，可能是**另一个**文件的 toolId/startLine，
      // 合并后就成了「A 的路径 + B 的定位」）。无 path 的 file 才用 makeTab 的初值。
      const t = (full.kind === 'file' && full.path
        ? { ...full, id: newTabId(), openSeq: 1 }
        : { ...makeTab(full.kind, s), ...full }) as TabInstance
      id = t.id
      tabs = [...tabs, t]
    }
    const next: TabSet = { tabs, activeId: activate ? id : (set0.activeId ?? id), closed: set0.closed }
    // 文件标签的 60% 宽规则（原 setRightTab 内联）：只在「首次进入 / 从折叠态打开」时给，
    // 已在文件标签内手动调宽后不反复重置（L1 的 G1-G4 钉住这三条口径）。
    const shouldSizeFile = spec.kind === 'file' && (s.rightTab !== 'file' || !s.rightExpanded)
    const sizePatch = shouldSizeFile && typeof window !== 'undefined'
      ? { rightSize: Math.round(window.innerWidth * 0.6) }
      : {}
    set({ ...tabPatch(s, scope, next), ...sizePatch })
    return id
  },
  activateTab: (id) => {
    const s = get()
    const scope = scopeKeyOf(s)
    const set0 = syncTabsToRightTab(s)
    if (set0.activeId === id) return
    const tab = set0.tabs.find((t) => t.id === id)
    if (!tab) return
    set(tabPatch(s, scope, { ...set0, activeId: id }))
    // web 标签 → 同步底层页面的激活态。内层标签条退役后，这里是**唯一**的切页入口。
    if (tab.kind === 'web') {
      if (tab.viewId) {
        get().markPreviewSeen(tab.viewId) // 用户点开 = 看到了（未读点立即消失，不等 nav 事件）
        // 阶段 3：休眠标签的恢复在主进程里做（重建 + 加载，**沿用原 id**）。恢复失败必须让
        // 用户看见 —— 否则就是「点了没反应」的静默失败（与 openInBrowserTab 的失败口径一致：
        // 响亮报错只属于用户显式点击，而点标签正是用户显式动作）。
        const r = window.desktop?.embedActivate?.(tab.viewId)
        void r?.then((res) => {
          if (res && res.ok === false) {
            const tp = get().embedNav?.targets?.find((x) => x.id === tab.viewId)
            const name = tp?.title || tp?.url || tab.viewId || ''
            get().showToast(i18nT('browser.restoreFail').replace('{name}', name).replace('{err}', res.error ?? ''), 'error')
          }
        })
      } else if (isPendingWebTab(tab)) {
        // 阶段 5：**重启恢复**出来的 web 标签没有视图（恢复时刻意不建，见 loadPersistedTabs
        // 「视图按需重建」）。重建**不在这里**做：重建点唯一收口在 RightPanel 的 effect
        //（「这个待重建标签成了当前正在显示的内容」才建视图）—— 那里同时覆盖「点标签」与
        // 「恢复时它就是激活标签、用户只是把面板展开」两条路径，而在这里再做一次会双开视图。
      }
      // 回退（MCP 页列表）模式：页选择由 BrowserTab 从**激活标签**派生（activePageId prop），
      // 这里无需额外动作 —— 组件按该 pageId 拉截图。
    }
  },
  closeTab: (id) => {
    const s = get()
    const scope = scopeKeyOf(s)
    const set0 = syncTabsToRightTab(s)
    const i = set0.tabs.findIndex((t) => t.id === id)
    if (i < 0) return
    const gone = set0.tabs[i]
    const tabs = set0.tabs.filter((t) => t.id !== id)
    // 关掉激活标签 → 顺延到**右邻居**，没有则左邻居（Chrome 口径）；全关光 → activeId=null（空态）
    const activeId = set0.activeId === id ? ((tabs[i] ?? tabs[i - 1])?.id ?? null) : set0.activeId
    const closed = [gone, ...set0.closed].slice(0, TAB_CLOSED_MAX)
    set(tabPatch(s, scope, { tabs, activeId, closed }))
    // web 标签 → 真关掉底层页面（视图池是进程级单例，不关就是僵尸渲染进程）。
    // 注意：关页面会触发 embed-nav → syncWebTabs 会看到该 key 消失，此时标签已不在集合里（幂等）。
    if (gone.kind === 'web') {
      if (gone.viewId) {
        get().markPreviewSeen(gone.viewId)
        void window.desktop?.embedCloseTarget?.(gone.viewId)
      } else if (gone.pageId != null) {
        get().closeBrowserPage(gone.pageId)
      }
    }
  },
  reopenTab: (id) => {
    const s = get()
    const scope = scopeKeyOf(s)
    const set0 = syncTabsToRightTab(s)
    const i = id ? set0.closed.findIndex((t) => t.id === id) : 0
    if (i < 0 || !set0.closed[i]) return
    const tab = set0.closed[i]
    const rest = set0.closed.filter((_, k) => k !== i)
    // 重开 = 追加到末尾并激活（阶段 1 不恢复原位置）
    const tabs = set0.tabs.some((t) => t.id === tab.id)
      ? set0.tabs.map((t) => (t.id === tab.id ? tab : t))
      : [...set0.tabs, tab]
    set(tabPatch(s, scope, { tabs, activeId: tab.id, closed: rest }))
  },
  // 组件 effect 调用：把标签集收敛到 rightTab。已一致时**不 set**（返回同一引用）——
  // 否则 effect → set → effect 会自激。
  syncTabsToRightTab: () => {
    const s = get()
    const scope = scopeKeyOf(s)
    const next = syncTabsToRightTab(s)
    if (next === s.tabsByScope[scope]) return
    set(tabPatch(s, scope, next))
  },
  flushSessionTabs: () => flushPersistTabs(),
  // 把「内嵌浏览器的页面」同步成面板级 web 标签（一页一标签）。
  // 为什么是**同步**而不是「打开时加一个」：页面的生命周期在 Electron 侧（视图池），
  // 用户/AI 都可能绕过 UI 直接关页（closeEmbedTarget、AI 的 close_page），
  // 声明式收敛一次覆盖全部路径；与旧内层标签条的数据源口径完全一致。
  syncWebTabs: (source, items, activeKey) => {
    const s = get()
    const scope = scopeKeyOf(s)
    const set0 = syncTabsToRightTab(s)
    const live = new Set(items.map((x) => x.key))
    // ① 该数据源里已消失的页面 → 删标签；**另一数据源**的 web 标签：本数据源有数据时一并清掉
    //（与组件「embedLive ? navTargets : browserPages」的权威口径一致，否则两套 id 会各留一排标签）
    const other = source === 'native' ? 'mcp' : 'native'
    const kept: TabInstance[] = set0.tabs.filter((t) => {
      if (t.kind !== 'web') return true
      // 阶段 5：重启恢复的**待重建**标签（有 url 快照、没有池身份）本来就不该出现在视图池里，
      // 直到用户点它 → 不能拿「池里没有它」当成「页面被关了」把它删掉（那正好把恢复的 web
      // 标签全吃掉：重启后主进程推来的第一帧 targets 是空的）。
      if (isPendingWebTab(t)) return true
      const k = webTabKey(t, source)
      if (k !== undefined) return live.has(k)
      if (webTabKey(t, other) !== undefined) return items.length === 0
      return true // 无 id 的「空」web 标签（用户点 + 浏览器 开的）：交给下面统一处置
    })
    // 显式标注类型：TS 5.5+ 会为这类回调**推断出类型谓词**，`!isPendingWebTab(t)` 在谓词里
    // 变成 `Exclude<TabInstance, WebTabInstance>`，与 `t.kind === 'web'` 求交后得到 never。
    const pending: WebTabInstance[] = kept.filter(isPendingWebTab)
    const generics: TabInstance[] = kept.filter((t) => t.kind === 'web' && !isPendingWebTab(t) && webTabKey(t, source) === undefined && webTabKey(t, other) === undefined)
    // 用 id 集合而不是数组 includes（引用比较）：标签 id 在集合内唯一（newTabId / 恢复时的重发），
    // 且省掉 O(n²)。
    const genericIds = new Set(generics.map((t) => t.id))
    const pendingIds = new Set(pending.map((t) => t.id))
    const solid = kept.filter((t) => !genericIds.has(t.id) && !pendingIds.has(t.id))
    // ② 复用已有同 key 的实例（保 id 稳定 → 不闪、不影响 activeId 指向）
    const byKey = new Map<string, TabInstance>()
    for (const t of solid) { const k = webTabKey(t, source); if (k) byKey.set(k, t) }
    // ②b 阶段 5：**待重建的恢复标签优先认领**这个页面（按 srcPath / url 命中）—— 用户点恢复
    //     出来的标签 → rebuildWebTab 建出视图 → 这里把视图接回**原来那个标签实例**。
    //     不认领的话：恢复的标签永远停在待重建态（点一次多一个重复标签，那个原标签再也点不开）。
    const claim = (x: { key: string; url: string; srcPath?: string }): TabInstance | undefined => {
      const i = pending.findIndex((t) => samePreviewTarget(t, x, s.workspace))
      if (i < 0) return undefined
      // 快照用完即弃（此后以视图池为真相源，与活标签同一形态）。
      const { url: _snapshot, ...live } = pending.splice(i, 1)[0]
      return { ...live, viewId: x.key }
    }
    let webTabs = items.map((x) => byKey.get(x.key) ?? claim(x) ?? makeWebTab(source, x.key))
    // ③ 收养：用户点「+ 浏览器」开的空标签认领第一个页面（否则它永远空着）
    if (generics.length && webTabs.length) {
      const adopted = generics[0]
      webTabs = webTabs.map((t, i) => (i === 0 ? ({ ...t, id: adopted.id } as TabInstance) : t))
    }
    // ④ 组装：web 标签插回「原来第一个 web 标签」的位置，其余标签相对顺序不变。
    //    注意 nonWeb 必须**只含非 web**：solid 里还留着复用下来的 web 标签，混进来就会重复。
    const nonWeb = solid.filter((t) => t.kind !== 'web')
    const firstWebIdx = set0.tabs.findIndex((t) => t.kind === 'web')
    const insertAt = firstWebIdx < 0
      ? nonWeb.length
      : set0.tabs.slice(0, firstWebIdx).filter((t) => t.kind !== 'web').length
    // 剩下的「无身份」标签（没被认领的空标签 / 还没重建的恢复标签）插在 web 标签之后。
    const leftovers = [...(webTabs.length ? generics.slice(1) : generics), ...pending]
    const tabs = [...nonWeb.slice(0, insertAt), ...webTabs, ...leftovers, ...nonWeb.slice(insertAt)]
    // ⑤ 激活项：原激活标签还在 → 跟随视图池的激活页（AI 切页时标签条跟着走，与旧内层条同口径）；
    //    被删掉了（页面被别处关）→ 顺延到池里的激活页，再退到第一个
    let activeId = set0.activeId
    const activeTab = tabs.find((t) => t.id === activeId)
    const follow = activeKey ? tabs.find((t) => webTabKey(t, source) === activeKey) : undefined
    if (!activeTab) activeId = follow?.id ?? tabs[0]?.id ?? null
    else if (activeTab.kind === 'web' && follow) activeId = follow.id
    if (sameTabList(tabs, set0.tabs) && activeId === set0.activeId) return // 无变化：不 set
    set(tabPatch(s, scope, { tabs, activeId, closed: set0.closed }))
  },
  setRightSize: (n) => set({ rightSize: n }),
  // M3：使用 Browser Use 时聚焦右侧 web tab —— 收起左栏 + 展开右栏 + 切到网页 tab
  focusBrowser: () => {
    get().setRightExpanded(true)
    get().setRightTab('web')
    set({ leftExpanded: false }) // 收起左侧：把主视界交给右侧 web tab
  },
  // 预览专用（只切右侧，不动左栏）—— 为什么单列：收起左栏是「Browser Use 接管视界」
  // 的语义，用在「预览一个本地文件」上是误伤（用户没要求收起文件树）。
  // 与 focusBrowser 的唯一差别就是**不碰 leftExpanded**：openHtmlInBrowser 被
  // 产出卡/文件栏/自动打开/```html 代码块共用，这些场景没有一个是「AI 接管浏览器」。
  focusBrowserForPreview: () => {
    get().setRightExpanded(true)
    get().setRightTab('web')
  },
  // §6.9 右栏自动展开语义（产品要求）：
  //   1. 执行 Browser Use 时自动打开右侧 web 栏；
  //   2. 已打开（右栏展开且 tab 是 web）时不重复打开；
  //   3. 用户收起右栏后，下一次 Browser Use 动作再次打开。
  // 与「连接 MCP ≠ 打开侧栏」边界：只在真实 Browser Use 动作（工具/AI 操作/首次页面）触发。
  //
  // 2026-09-20 会话收口（用户口径「别在别的会话里掀开我正看的面板」）：只有**工具所属会话 ==
  // 当前会话**才展开。旧实现不按会话过滤 → 后台会话里 Agent 一开浏览器，用户正在看的那个会话
  // 面板也被掀开、左栏还被收走（正是用户反馈的那类「很别扭」）。别的会话的可见性兜底是
  // embedAiActive 呼吸徽标 + 该标签的未读点（见 onEmbedAiAction / syncWebTabs），不动视线。
  // fromSid 省略/为空（旧版事件、无归属）→ 回落「当前会话」，不制造回归。
  focusBrowserForUse: (fromSid?: string) => {
    const st = get()
    if (fromSid && fromSid !== st.activeSessionId) return // 别的会话：不展开、不收左栏
    if (st.rightExpanded && st.rightTab === 'web') return // 要求 2：已打开不重复
    st.focusBrowser() // 要求 1+3：打开或收起后重开
  },
  // §3.1（R2）预览统一入口：所有「预览本地文件」的动作都走这里 → 内嵌浏览器**新建标签**。
  //
  // 为什么必须新建（缺陷 A）：修复前预览走 embedShow + embedNavigate —— 那是「复用当前
  // 激活视图」的语义（embed.ts 的 ensureEmbed「池非空即 return」），于是「预览 A → 预览 B」
  // = B 顶掉 A：没有标签、无法返回、A 的页面状态丢失。新建后 A/B 并存于视图池，用户可在页
  // 列表里切回（R0 的按会话标签清单 + 本迁移合起来才构成「多标签」）。
  //
  // 标签语义（2026-09，用户口径「打开过的文件：刷新已存在的标签；没开过：新开标签」）：命中
  // 已有标签时不再只是切过去 —— 还要把它的内容更新到最新（见下面 refresh 的三条分派）。
  // 为什么必须刷新：Agent 反复写同一个产出文件（改一次 → 再改一次）时，「打开过的那个标签」
  // 若停在旧内容上，用户看到的就是**过期结果**；而 office 预览更甚：每次转换都是新的临时
  // html_path，旧实现每预览一次就多一个重复标签（本仓「未决 8」）——故去重身份取**源文件路径**。
  //
  // 为什么必须带 sid：视图归属会话（R0 机制）。旧预览路径落在**自举视图**（sid=''，AI 的
  // attach/页面枚举载体）上，而 sid='' 按规格「放行 + 不展示」→ 会话 A 的预览会出现在会话 B。
  // 本 action 一律用 activeSessionId 归属，正是那条跨会话泄漏的收口点（R0 只交付了机制）。
  //
  // activate 语义（默认 false）：
  //   · true  = 用户显式点击（产出卡/文件栏/消息里的「在浏览器打开」）→ 展开右栏 + 切 web tab
  //   · false = Agent 自动打开（R3）→ **不动任何视图/面板状态**（不展开、不切 tab、不激活）
  // 与 Browser Use 的分界：这里绝不调 focusBrowser（它收起左栏，是「AI 接管视界」的语义）。
  //
  // 失败口径（R3 ④）：**响亮报错只属于用户显式点击**。activate:false 撞上限
  // 是背景行为 —— 用户没要求过这次打开，弹「已达上限」纯是噪音，静默降级即可。
  // 阶段 3 起上限语义变了：超限先按 LRU **休眠**最久没看的「用户看过」的标签（不再抛错），
  // 只有保护集里没有可驱逐对象时才会失败（那种形态说明 12 个视图全在使用中）。
  openInBrowserTab: async (path, opts) => {
    const st = get()
    const activate = opts?.activate === true
    // quiet：调用方是**背景行为**（Agent 产出的自动打开，见 autoOpenVisualFile）→ 失败不弹。
    // 「激活」与「报错口径」是两件事（阶段 4 后自动打开已改走 activate:false，即**不**激活）：
    // 阶段 3 起超限先按 LRU 休眠（不再抛错），失败只剩「保护集里无可驱逐对象」这一种形态 ——
    // 用户没要求过第 N 次打开，弹错是噪音。响亮报错只属于用户显式点击（原 R3 ④ 口径不变）。
    const quiet = opts?.quiet === true
    // 绝对化：调用方传的通常是工作区相对路径（out/report.pdf）或已是绝对路径
    const abs = path.startsWith('/') ? path : st.workspace ? `${st.workspace}/${path}` : path
    const fileUrl = 'file://' + abs.split('/').map((seg) => encodeURIComponent(seg)).join('/')
    // 去重身份 = **源文件路径**（不是 URL）：office 预览每次转换产出新的临时 html_path →
    // URL 认不出「这是同一个文件」，只有源文件能。没显式给 srcPath 的（pdf/html/图片）源就是
    // 它自己。统一送绝对路径：主进程按字符串比对，口径必须单一（相对/绝对混用会各开一个标签）。
    const srcPath = (opts?.srcPath ?? abs).replaceAll('\\', '/')
    // 面板切换必须**同步**发生（在任何 await 之前），与修复前的实现同序：调用方（含
    // e2e/verify_auto_open_visual.mjs 的同步状态读取、以及用户点击后的即时反馈）依赖
    // 「调完这个 action，rightTab 已经是 web」；放到 await 之后会让它晚一次 IPC 往返生效
    // （实测：e2e:auto 的「停在预览 tab」由绿转红）。activate:false 时**一律不动**面板。
    if (activate) get().focusBrowserForPreview()
    try {
      const d = window.desktop
      if (!d) {
        if (activate && !quiet) st.showToast(i18nT('common.browserUnsupported'), 'error')
        return
      }
      // 去重：同一文件已在视图池里 → **刷新那个标签**（不是新建）。查主进程真实池（embedStatus）
      // 而非前端 embedNav 快照：快照由导航事件推送，可能落后于「刚建好、还没导航完」的视图。
      const status = (await d.embedStatus()) as { targets?: { id: string; url?: string; sid?: string; srcPath?: string }[] } | undefined
      const poolIds = new Set((status?.targets ?? []).map((t) => t.id)) // activate:false 定位新视图用（差集）
      // 去重**优先本会话**：同一文件可能已被开在**别的会话**的标签里（多会话并存是 R0 之后的常态）。
      // 只按 URL 取首个匹配会在这种情况下永远命中别人的标签 → 激活被跨会话规则拒绝（见下）→
      // 每次都走「新建」，于是本会话内**每点一次预览就多一个重复标签**，直到撞上 8 个上限。
      // 实测（两会话探针）：会话 SID 开过该文件后，在另一会话连点两次 → 同 URL 视图 1→2→3。
      // 取不到本会话的才退到任意会话（由下面「激活没真生效就新建」兜住跨会话那一半）。
      //
      // R3 ③：**activate:false 只在当前会话里找**，找不到就在**本会话新建**。为什么不能也退到
      // hits[0]：后台路径命中别的会话的标签时「什么都不做」= 静默无反应（用户看不到任何东西，
      // 也没有标签留给用户点）——与「Agent 产出把内容准备好」直接矛盾。activate:true 保留退到
      // hits[0]（用户点击的那次激活若被跨会话规则拒绝，由下面「激活没真生效就新建」兜住）。
      const mine = get().activeSessionId ?? ''
      const ws = get().workspace
      const sameSrc = (t: { srcPath?: string }) => !!t.srcPath && relIfInside(t.srcPath, ws) === relIfInside(srcPath, ws)
      const byUrl = (t: { url?: string }) => t.url === fileUrl
      // 口径与主进程 embed:open-target 一致：先比 srcPath（**两侧都有值**时），再比 URL
      const pick = (list: { id: string; url?: string; sid?: string; srcPath?: string }[]) =>
        list.find(sameSrc) ?? list.find(byUrl)
      const all = status?.targets ?? []
      const hit = pick(all.filter((t) => (t.sid ?? '') === mine)) ?? (activate ? pick(all.filter((t) => (t.sid ?? '') !== mine)) : undefined)
      if (hit) {
        // 命中已有标签时把内容更新到最新（「打开过的文件：刷新已存在的标签」）：
        //   · URL 不同（office 产物换了临时目录）→ 让**该**标签导航到新产物；
        //   · 同一 file:// 文件 → reload 该标签（重读磁盘，看到最新一次产出）；
        //   · http(s) → 什么都不做（只激活）：刷外部站点不是预览语义，还会打断用户正填的表单。
        // 两条都作用**指定**标签、都不碰 activeId/面板 → activate:false 也能安全刷新后台标签。
        // 可选调用（?.）：桩环境（e2e 的 mock window.desktop）没有这两个 API 时退化成「只激活」，
        // 不让一次缺失的 API 把整个预览动作变成抛错。
        const refresh = async (): Promise<void> => {
          if (hit.url !== fileUrl) {
            const r = await d.embedNavigateTarget?.(hit.id, fileUrl)
            if (r && !r.ok) throw new Error(r.error || i18nT('common.browserOpenFailed'))
          } else if (fileUrl.startsWith('file://')) {
            const r = await d.embedReloadTarget?.(hit.id)
            if (r && !r.ok) throw new Error(r.error || i18nT('common.browserOpenFailed'))
          }
        }
        if (activate) {
          const r = await d.embedActivate(hit.id) as { ok?: boolean; error?: string; state?: { activeId?: string | null } } | undefined
          if (!r?.ok) throw new Error(r?.error || i18nT('common.browserOpenFailed'))
          // 命中且**真的**切过去了才算完成。为什么看结果而不是直接信命中：R0 的跨会话激活是
          // **被拒绝**的（activateEmbedTarget 对 sid 不匹配的视图原样返回状态），而同一个文件
          // 完全可能已开在**别的会话**的标签里 —— 只报成功就成了「点了没反应」的静默失败，
          // 比多开一个标签糟得多。视图在查完到切之间被关掉的竞态同理（id 已不在池里）。
          if (r.state?.activeId !== hit.id) {
            const created = await d.embedOpenTarget(fileUrl, { sid: get().activeSessionId ?? '', activate: true, srcPath })
            if (!created?.ok) throw new Error(created?.error || i18nT('common.browserOpenFailed'))
            rememberPreviewView(created, activate, poolIds)
          } else {
            await refresh()
          }
        } else {
          // activate:false 命中：只刷新，不新建、不激活、不碰面板（规格 ⑥）。
          await refresh()
        }
      } else {
        const r = await d.embedOpenTarget(fileUrl, { sid: get().activeSessionId ?? '', activate, srcPath })
        if (!r?.ok) throw new Error(r?.error || i18nT('common.browserOpenFailed'))
        rememberPreviewView(r, activate, poolIds)
      }
    } catch (e) {
      // 静默条件见上面「失败口径」：背景产出失败不弹（quiet，用户没要求过这次打开），
      // 用户点击失败必弹（否则是「点了没反应」的静默失败，比多开一个标签糟得多）。
      if (activate && !quiet) st.showToast(i18nT('common.browserOpenFail').replace('{path}', path).replace('{err}', e instanceof Error ? e.message : String(e)), 'error')
    }
  },
  // write_file 落盘的 .html → 右侧内嵌浏览器 file:// 打开（2026-09 决策：HTML
  // 产物由 LLM 决定落盘位置；代码块本身不猜路径，只有工具卡知道真实 path）。
  // 文件缺失/加载失败 → toast 明确提示（不静默）。
  //
  // §3.1 委托化：保留导出名（6 处调用方零改动），内部改走 openInBrowserTab（新建标签）。
  // 6 处**全部**是用户显式点击（产出卡主按钮 html/svg/pdf、文件栏 renderBinaryActions、
  // 消息里的「在浏览器打开」、子 agent 产物块），故一律 activate:true（响亮失败也归它）。
  // 2026-09 三次修正后**没有** activate:true 的自动调用方了（Agent 自动打开改走 activate:false +
  // quiet:true，见 §6.10）；xlsx/docx/pptx 也**不**再经这里（previewOfficeFile 现在直接调
  // openInBrowserTab 以透传 activate）。
  openHtmlInBrowser: async (path) => {
    await get().openInBrowserTab(path, { activate: true })
  },
  // 纯展示 ```html 代码块（未落盘文件）→ 右侧内嵌浏览器以 data URL 展示同一内容。
  // 与 write_file 的 file:// 打开共用同一右侧 web tab 位置（「同一位置」语义）。
  //
  // §3.1：同样改走**新建标签**（embedOpenTarget）。为什么不去重：data URL 由内容生成，
  // 每次内容不同 → URL 必然不同，去重表里永远命中不了，硬查只是白跑一次 embedStatus。
  // activate:true —— 调用方是用户点击（markdown.tsx 的「在浏览器打开」），与 6 处调用点同类。
  openHtmlContentInBrowser: async (rawHtml) => {
    const st = get()
    // 面板切换同步生效（同 openInBrowserTab 的说明）：调用方依赖「调完即已切到 web tab」。
    st.focusBrowserForPreview()
    try {
      const d = window.desktop
      if (!d) {
        st.showToast(i18nT('common.browserUnsupported'), 'error')
        return
      }
      // data:text/html —— 避免临时文件管理；内容里 # / % 等经 encodeURIComponent 安全化。
      // 注意：data URL 里 script 执行受 Chromium 同源策略限制（视为 opaque origin），
      // 与 sandbox iframe 一致 —— 交互脚本可跑、访问不到主应用。
      const dataUrl = 'data:text/html;charset=utf-8,' + encodeURIComponent(rawHtml)
      const r = await d.embedOpenTarget(dataUrl, { sid: get().activeSessionId ?? '', activate: true })
      if (!r?.ok) throw new Error(r?.error || i18nT('common.browserOpenFailed'))
    } catch (e) {
      st.showToast(i18nT('common.htmlOpenFail').replace('{err}', e instanceof Error ? e.message : String(e)), 'error')
    }
  },
  // 经 bridge 转换的文件预览（xlsx/docx/pptx 共用一个实现；产出卡与右栏文件栏共用）：
  //   ① bridge 侧用受管 Python 把文件转成**自带样式的 HTML**
  //      （xlsx_preview / docx_preview / pptx_preview；路径口径与 file_preview 完全一致）；
  //   ② 拿到产物路径后走 openInBrowserTab —— 与 PDF／html 产物**同一条**内嵌浏览器
  //      通路（file:// + Chromium 渲染），不为这三种格式另起一套渲染机制。
  //      activate 由 opts 透传：默认 true（用户点击产出卡/文件栏）；Agent 自动产出传 false
  //      （阶段 4：后台加标签 + 未读点，不激活不展开，见 §6.10 三次修正），只额外多带 quiet:true。
  //      **不能**直接调 openHtmlInBrowser —— 那个的语义被固定成 activate:true（6 处用户点击
  //      调用方），后台路径会因此抢视线。
  //
  // 为什么三种格式合并成一个 helper（而不是三份几乎一样的 async 函数）：
  // 它们的前端逻辑**逐字相同** —— 都是「校验能力 → 置 busy → 等 bridge → 导航 → 复位」，
  // 唯一差别是调用哪个 IPC 与哪个文案 key。写三份的话，「同路径去重」「先复位再导航」
  // 「失败必须 toast 真实原因」这些细节会各自演化（修一处漏两处）。
  //
  // 为什么不把 HTML 内容拉回前端自己渲染：产物可达数百 KB～数 MB，且 bridge stdin 单条
  // 命令上限 4 MiB（把内容塞进响应等于给预览加一个会静默失效的天花板）；走文件路径
  // 还能让浏览器的滚动/选中/查找等原生能力直接可用。
  //
  // 转换中要有可见反馈：首次调用可能先 bootstrap 受管运行时（建 venv + 装约 42MB），
  // 分钟级。用 officeBusy 驱动按钮态（禁用 + 「转换中…」），失败则 toast 出真实原因 ——
  // 用户点了没反应是最糟的形态。
  previewOfficeFile: async (path, kind, opts) => {
    const st = get()
    const ws = st.workspace ?? ''
    const activate = opts?.activate !== false // 默认 true：三个 previewXxx 与文件栏都是用户点击
    const quiet = opts?.quiet === true // 自动产出（产出即展示）：环境不支持时不弹；转换失败仍弹（见末尾 catch）
    const d = window.desktop
    // 能力面按 kind 取（三种格式各有一个 IPC；undefined = 该环境不支持，如浏览器 mock）
    const call = kind === 'xlsx' ? d?.watchXlsx : kind === 'docx' ? d?.watchDocx : d?.watchPptx
    if (!call) {
      if (activate && !quiet) st.showToast(i18nT('common.browserUnsupported'), 'error')
      return
    }
    // 同一个文件已在转换：忽略重复点击（避免排队等同一个文件）。**不同**文件必须放行 ——
    // 用户完全可能在等 A 的时候去点 B；一并挡掉会让 B 的按钮看着没反应（静默）。
    // 并发安全由后端保证（每次转换独立临时目录，按年龄回收），前端只挡同路径。
    // 注意 busy 是**单个**路径槽：与「不同文件放行」并不矛盾 —— 置位会被后来的覆盖，
    // 复位判定用 `=== path` 保证先完成的那个不会把后来者的 busy 清掉（见下方两处）。
    const busy = get().officeBusy
    if (busy === path) return
    set({ officeBusy: path })
    try {
      // 传相对路径/绝对路径都给 bridge（bridge 用与 file_preview 同一函数解析）；
      // workspace 供相对路径锚定（工作区外绝对路径不依赖它）。
      const r = await call({ path, workspace: ws })
      if (!r?.ok || !r.html_path) throw new Error(r?.error || i18nT('common.officeConvertFailed'))
      // 产物是临时文件的**绝对路径** → 直接进内嵌浏览器（openInBrowserTab 对绝对路径原样使用）。
      // srcPath 传**源文件**（office 文件本身，绝对化）：转换产物每次换临时目录，只有源文件
      // 能认出「这是同一个文件」→ 同一份工作簿重复预览 = 同一个标签导航到新产物，不再堆标签。
      // 先落 busy=false 再导航：导航本身会立刻 focusBrowserForPreview + 展示，转换态该结束了。
      if (get().officeBusy === path) set({ officeBusy: undefined })
      await get().openInBrowserTab(r.html_path, { activate, srcPath: path.startsWith('/') ? path : ws ? `${ws}/${path}` : path })
    } catch (e) {
      if (get().officeBusy === path) set({ officeBusy: undefined })
      st.showToast(i18nT('common.officeConvertFail').replace('{path}', path).replace('{err}', e instanceof Error ? e.message : String(e)), 'error')
    }
  },
  // 三个对外动作（保留各自名字，调用方按格式选；都只是转发到上面的 helper）。
  // 为什么不让调用方直接调 previewOfficeFile(path, 'xlsx')：产出卡/文件栏的两处分派
  // 已经在按扩展名分支了，动作名能把「这条分支走的是哪条通路」写进代码可读性里，
  // 也让将来某一种格式要加专属行为（如 pptx 的「已知近似」提示）时有落点。
  // ⚠️ 这三个是**用户点击**入口（产出卡主按钮 / 文件栏 renderBinaryActions）→ 不带 opts，
  // activate 必须保持 true；Agent 自动产出现走 activate:false（只多带 quiet:true，见 §6.10）。
  previewXlsx: (path) => get().previewOfficeFile(path, 'xlsx'),
  previewDocx: (path) => get().previewOfficeFile(path, 'docx'),
  previewPptx: (path) => get().previewOfficeFile(path, 'pptx'),
  openSkills: () => set({ inSkills: true }),
  closeSkills: () => set({ inSkills: false }),
  openNewFolder: () => set({ showNewWsModal: true }),
  closeNewFolder: () => set({ showNewWsModal: false }),

  // 全局设置水合（settings:get → ~/.go-code/settings.json；mock=localStorage）
  loadSettings: async () => {
    try {
      const s = await transport.settingsGet()
      set((st) => {
        // 多 Provider 水合：当前模型不在活跃 Provider 的模型列表 → 默认到其默认模型（对齐 bridge）
        const activeModels = activeProviderCfg(s)?.models ?? {}
        const next = typeof activeModels[st.model] === 'number' ? st.model : defaultModelFor(s)
        // 浏览器引擎也要水合：settings.json 的 browser_engine（顶层字段）是权威值，
        // 但 renderer 的 browserEngine 此前只在「打开设置插件 tab / 切换引擎」时才拉——
        // 于是启动后 RightPanel 读到的恒为 undefined，ego 用户看到的一直是「未启用」
        // 的老占位（file:// 预览路径也据此被关掉）。这里随设置一起水合。
        const engine = s.browser_engine === 'ego' ? 'ego' as const : s.browser_engine === 'mcp' ? 'mcp' as const : st.browserEngine
        return { settings: s, model: next, browserEngine: engine }
      })
    } catch {
      /* 保持默认 */
    }
  },
  openSettings: () => set({ inSettings: true }),
  closeSettings: () => set({ inSettings: false }),
  // 全局 Toast：轻提示（保存成功/失败）。覆盖旧 Toast；2.5s 自动消失。
  // 用于 Provider 保存、Mesh 配置保存等「操作完成」反馈，替代内联文本提示。
  showToast: (msg, kind) => {
    set({ toast: { msg, kind: kind ?? 'ok', id: Date.now() } })
  },
  // toast 自动消失由 ToastView 组件负责（监听 toast.id 变化起定时器）——见 App.tsx。
  // —— 定时任务（CronPage + CronRunsPage + 通用设置开关）——
  openCron: () => {
    set({ inCron: true, cronRunsJob: null, cronDetailOpen: false, activeCronRun: undefined })
    get().fetchCron()
  },
  closeCron: () => set({ inCron: false, cronRunsJob: null, cronDetailOpen: false, activeCronRun: undefined }),
  // 点击某条任务 → 进入全新的「执行历史」页（独立于任务列表；展示该任务每次执行的 sessions 历史）
  openCronRuns: (job) => {
    set({ cronRunsJob: job, cronDetailOpen: false, activeCronRun: undefined })
    get().fetchCronRuns(job.id)
  },
  closeCronRuns: () => set({ cronRunsJob: null, cronDetailOpen: false, activeCronRun: undefined }),
  fetchCron: () => {
    set({ cronLoading: true })
    send({ type: 'cron_list', payload: {} })
  },
  fetchCronRuns: (jobId) => {
    send({ type: 'cron_runs', payload: jobId ? { job_id: jobId } : {} })
  },
  fetchCronRunDetail: (jobId, runId) => {
    set({ cronDetailLoading: true })
    send({ type: 'cron_runs_detail', payload: { job_id: jobId, run_id: runId } })
  },
  clearCronRunDetail: () => set({ activeCronRun: undefined, cronDetailLoading: false }),

  // —— 链路 tab：trace ——
  // 拉取历史 trace 摘要列表（bridge 读事件日志按需投影；零新增存储）。
  fetchTraces: (sid, limit) => {
    const s = get()
    const target = sid || s.activeSessionId || ''
    if (!target) return
    const v = s.views[target] ?? emptyView()
    set(setView(s, target, { ...v, traceListLoading: true }))
    // latest=1：顺带回「最新一条 trace」的详情 —— 刷新/切回会话后 traceEvents 为空
    //（checkpoint 不含它），用它兜底回显，而不是误报「尚无运行」。
    // limit：**必须显式传**才能「加载更多」—— 后端 limit<=0 会回落到默认上限，
    // 不传等于每次都要同一批（长会话 112 条 trace 时按钮点了没反应）。
    const payload: Record<string, unknown> = { session_id: target, latest: 1 }
    if (limit && limit > 0) payload.limit = limit
    send({ type: 'trace', payload })
  },
  // 选中某条 trace：undefined = 跟随最新（用实时投影）；否则从 bridge 拉该条历史详情。
  selectTrace: (id, sid) => {
    const s = get()
    const target = sid || s.activeSessionId || ''
    if (!target) return
    const v = s.views[target] ?? emptyView()
    // 切换即清旧详情（避免短暂显示上一条 trace 的条）
    set(setView(s, target, { ...v, activeTraceId: id, traceDetail: undefined }))
    if (id) send({ type: 'trace', payload: { session_id: target, run_id: id } })
  },
  getTraceEvents: (sid) => {
    const s = get()
    const target = sid || s.activeSessionId || ''
    return target ? (s.views[target]?.traceEvents ?? []) : []
  },
  setCronDetail: (open) => set({ cronDetailOpen: open }),
  deleteCron: (id) => {
    send({ type: 'cron_delete', payload: { id } })
    get().fetchCron() // 乐观刷新（cron_changed 事件也会兜底刷新）
  },
  updateCron: (id, patch) => {
    send({ type: 'cron_update', payload: { id, ...patch } })
    get().fetchCron()
  },
  // 定时任务开关：乐观更新 + 落盘 + bridge 重载（重建 loop：加载/卸载 cron 工具）
  setCronEnabled: (on) => {
    const cur = get().settings.cron ?? { enabled: true, auto_clean: true }
    set((s) => ({ settings: { ...s.settings, cron: { ...cur, enabled: on } }, cronEnabled: on }))
    void transport.settingsSet({ cron: { ...cur, enabled: on } }).then((r) => {
      if (!r.ok) return // 写盘失败：不通知 bridge（避免"未持久化却热生效"的假成功）
      send({ type: 'reload_settings', payload: {} })
    })
  },
  setCronAutoClean: (on) => {
    const cur = get().settings.cron ?? { enabled: true, auto_clean: true }
    set((s) => ({ settings: { ...s.settings, cron: { ...cur, auto_clean: on } }, cronAutoClean: on }))
    void transport.settingsSet({ cron: { ...cur, auto_clean: on } })
  },
  // Session Mesh 开关（C1 本机互聊）：乐观更新 + 落盘 + bridge 热重载（加载/卸载 session_* 工具）
  setMeshSessionEnabled: (on) => {
    const cur = get().settings.mesh ?? { session_enabled: false, remote_enabled: false }
    const next = { ...cur, session_enabled: on, remote_enabled: on ? cur.remote_enabled : false }
    set((s) => ({ settings: { ...s.settings, mesh: next } }))
    void transport.settingsSet({ mesh: next }).then((r) => {
      if (!r.ok) return // 写盘失败：不通知 bridge（避免"未持久化却热生效"的假成功）
      send({ type: 'reload_settings', payload: {} })
    })
  },
  // Session Mesh 远程开关（C2 多节点）：依赖 C1；只开远程而关本机 = 视为关闭（bridge 侧同样约束）
  setMeshRemoteEnabled: (on) => {
    const cur = get().settings.mesh ?? { session_enabled: false, remote_enabled: false }
    const next = { ...cur, remote_enabled: on && cur.session_enabled !== false }
    set((s) => ({ settings: { ...s.settings, mesh: next } }))
    void transport.settingsSet({ mesh: next }).then((r) => {
      if (!r.ok) return
      send({ type: 'reload_settings', payload: {} })
    })
  },
  // Session Mesh 远程配置（listen/peers/token）：保存后热重载（网关重启）
  setMeshRemoteCfg: (remote) => {
    const cur = get().settings.mesh ?? { session_enabled: false, remote_enabled: false }
    const next = { ...cur, remote }
    set((s) => ({ settings: { ...s.settings, mesh: next } }))
    void transport.settingsSet({ mesh: next }).then((r) => {
      if (!r.ok) return
      send({ type: 'reload_settings', payload: {} })
    })
  },
  // 查询 Session Mesh 运行状态（mesh_status）：何时给「别人怎么连我」/ 排障
  fetchMeshStatus: () => {
    const id = send({ type: 'mesh_status', payload: {} })
    onCommandResponseData(id, (r) => {
      if (!r.ok) return
      set({ meshStatus: (r.data ?? null) as MeshStatusInfo | null })
    })
  },
  // 主动 ping 一个节点（连接检测）：mesh_ping → { ok, rtt_ms }；失败给 error 文案。
  pingMeshNode: (nodeId) => {
    const id = send({ type: 'mesh_ping', payload: { node_id: nodeId } })
    return new Promise((resolve) => {
      onCommandResponseData(id, (r) => {
        const data = (r.data ?? {}) as { rtt_ms?: number; ok?: boolean }
        if (r.ok && data.ok) resolve({ ok: true, rttMs: data.rtt_ms })
        else resolve({ ok: false, error: r.error || 'ping 失败' })
      })
    })
  },
  // 主「停止」是否连后台任务一起停（S3-B，缺省 false = 只停主会话）：乐观更新 + 落盘
  // settings.json runtime 段；bridge interrupt 分支逐次读取生效值（无需 reload_settings）。
  setStopBackgroundOnInterrupt: (on) => {
    const cur = get().settings.runtime ?? { stop_background_on_interrupt: false }
    set((s) => ({ settings: { ...s.settings, runtime: { ...cur, stop_background_on_interrupt: on } } }))
    void transport.settingsSet({ runtime: { ...cur, stop_background_on_interrupt: on } })
  },
  setGoalAlignmentRounds: (n) => {
    const value = Math.max(0, Math.min(100, Math.round(n)))
    const agent = { ...(get().settings.agent ?? { reminder_rounds: 30 }), reminder_rounds: value }
    set((s) => ({ settings: { ...s.settings, agent } }))
    void transport.settingsSet({ agent }).then((r) => {
      if (!r.ok) return // 写盘失败：不推送阈值（配置未持久化，运行中改无意义）
      // 只更新运行中的 AgentLoop 阈值；不重建会话、不丢失运行上下文。
      send({ type: 'set_goal_alignment_rounds', payload: { reminder_rounds: value } })
    })
  },
  // 子 agent 并发槽上限（主池 agent_spawn / 辅池 subagent_explore）：乐观更新 + 落盘
  // settings.json agent 段，再通知 bridge 热更新（只改容量：在途任务继续跑，新派发生效）。
  // 下限 1（0/负数会让 bridge 回退默认值）；上限 10000 防手抖写爆。
  setSubagentConcurrency: (patch) => {
    const cur = get().settings.agent ?? { reminder_rounds: 30 }
    const clamp = (v: number, fallback: number) => {
      const n = Math.round(v)
      if (!Number.isFinite(n) || n < 1) return fallback
      return Math.min(n, 10000)
    }
    const agent: AgentSettings = {
      ...cur,
      max_subagents: clamp(patch.max_subagents ?? cur.max_subagents ?? SUBAGENT_CONCURRENCY_DEFAULT, SUBAGENT_CONCURRENCY_DEFAULT),
      max_explore_subagents: clamp(patch.max_explore_subagents ?? cur.max_explore_subagents ?? SUBAGENT_CONCURRENCY_DEFAULT, SUBAGENT_CONCURRENCY_DEFAULT),
    }
    set((s) => ({ settings: { ...s.settings, agent } }))
    void transport.settingsSet({ agent }).then((r) => {
      if (!r.ok) return // 写盘失败：不推送（配置未持久化，运行中改无意义）
      send({ type: 'set_max_subagents', payload: { max_subagents: agent.max_subagents, max_explore_subagents: agent.max_explore_subagents } })
    })
  },
  // 语音输入（STT）配置：乐观更新 + 落盘（settings.json stt 段；Composer 话筒据此显隐）
  setSTTSettings: (patch: Partial<STTSettings>) => {
    const cur = get().settings.stt ?? {}
    const next = { ...cur, ...patch }
    set((s) => ({ settings: { ...s.settings, stt: next } }))
    void transport.settingsSet({ stt: next })
  },
  setTheme: (theme) => {
    set((s) => ({ settings: { ...s.settings, theme } }))
    void transport.settingsSet({ theme })
    // 实际视觉应用（data-theme + 窗口底色）由 App.tsx 的 effect 统一处理
  },
  setNarrativeSkin: (skin) => {
    set((s) => ({ settings: { ...s.settings, narrative_skin: skin } }))
    void transport.settingsSet({ narrative_skin: skin })
    // 实际视觉应用（data-skin）由 App.tsx 的 effect 统一处理
  },
  setLang: (lang) => {
    set((s) => ({ settings: { ...s.settings, lang } }))
    void transport.settingsSet({ lang }).then((r) => {
      if (!r.ok) return
      // 轻量热通知 bridge（仅刷新 Hub 的 mesh 文案语言，不重建 loop）
      send({ type: 'set_ui_lang', payload: { lang } })
    })
  },
  // 命令沙箱模式（none/seatbelt）：乐观更新 + 落盘 + 通知 bridge 重载（loop 重建热切换）
  setSandboxMode: (mode) => {
    const cur = get().settings.sandbox ?? {}
    set((s) => ({ settings: { ...s.settings, sandbox: { ...cur, mode } } }))
    void transport.settingsSet({ sandbox: { ...cur, mode } }).then((r) => {
      if (!r.ok) return // 写盘失败：不重载（沙箱模式未持久化）
      send({ type: 'reload_settings', payload: {} }) // bridge 重载 settings + rebuildAll（沙箱生效）
    })
  },
  // Computer Use 已授权 App（设置 → 插件 → Computer Use）：乐观更新 + 落盘 + 热生效。
  // 授权 = 永久记录；未授权 App 的 computer_open_app/activate 拒绝（统一授权门禁）。
  // settings:set 对 permissions 整段替换——permissions 段只含 computer_approved_apps。
  computerApprovedSave: (apps) => {
    set((s) => ({ settings: { ...s.settings, permissions: { computer_approved_apps: apps } } }))
    void transport.settingsSet({ permissions: { computer_approved_apps: apps } }).then((r) => {
      if (!r.ok) return // 写盘失败：不重载（授权未持久化）
      send({ type: 'reload_settings', payload: {} })
    })
  },
  // 系统「选择应用程序」对话框（osascript choose application，主进程弹）→ 显示名。
  // 从系统选保证显示名拼写正确（`open -a <name>` 匹配），无需手输。
  pickAppFromSystem: async () => {
    const picked = await transport.pickApp()
    const name = picked?.name?.trim()
    return name ?? null
  },
  providerSave: async (p) => {
    const r = await transport.providerSave(p)
    if (r.ok) {
      const s = await transport.settingsGet()
      set({ settings: s })
    }
    return r.ok
  },
  // 各 provider 是否已设 key（仅布尔指示；Provider 面板「已设置 ✓」；含自定义 provider）
  refreshProviderKeys: () => {
    void (async () => {
      const provs = Object.keys(get().settings.provider).filter((k) => k !== 'active')
      const ks: Record<string, boolean> = {}
      for (const p of provs) ks[p] = await transport.providerKeyStatus(p)
      set({ providerKeys: ks })
    })()
  },
  // 新增自定义 provider：providerSave 建 settings 条目 + 存 key，再 set_provider 通知 bridge
  //（base_url/protocol/key 随 payload，不依赖落盘时序）。不切 active——用户后续点「设为当前」。
  // 内置/preset provider 重新添加（deleted 标记清除）时 openai_compat 传 nil —— bridge 预设
  // 初始化自动补官方 compat；只有自定义 provider 才用 minimal 保守默认（避免内置 provider
  // 被 minimal 降级：不发 reasoning_effort/max_completion_tokens → 思考模型请求能力丢失）。
  addProvider: async (p) => {
    const isPreset = typeof p.openai_compat !== 'undefined' || BUILTIN_PROVIDER_IDS.includes(p.provider)
    const compat = p.openai_compat ?? (isPreset ? undefined : { minimal: true, base_path: '/v1' })
    const r = await transport.providerSave({ provider: p.provider, base_url: p.base_url, protocol: p.protocol ?? 'chat_completions', openai_compat: compat, api_key: p.api_key })
    if (r.ok) {
      const s = await transport.settingsGet()
      set({ settings: s, providerKeys: { ...get().providerKeys, [p.provider]: Boolean(r.hasKey) } })
      const stored = (get().settings.provider[p.provider] as ProviderCfg | undefined)?.openai_compat
      send({ type: 'set_provider', payload: { provider: p.provider, base_url: p.base_url, protocol: p.protocol ?? 'chat_completions', openai_compat: stored ?? compat, api_key: p.api_key } })
    }
    return r
  },
  // 自定义 provider 改名：把整卡配置迁移到新 id（providerSave 建新条目 + 复制 api_key，
  // providerRemove 物理删旧条目；内置 5 家不可改名——UI 已挡）。改的是活跃 provider 时
  // electron provider:remove 会自动回退 active 到内置，故改名流程：先建新 id 再删旧 id，
  // 最后把 active 指回新 id（若原先是活跃）。session 侧归属（bs.provider）不迁移——
  // 改名的自定义 provider 通常不是活跃/会话绑定方；如需要可在设置里重新「设为当前」。
  renameProvider: async (oldName, newName) => {
    const name = newName.trim()
    if (!name) return { ok: false, error: '名称不能为空' }
    if (name === oldName) return { ok: true }
    if (!/^[A-Za-z][A-Za-z0-9_-]*$/.test(name) || name === 'active') return { ok: false, error: '名称仅限字母开头，可含字母/数字/短横/下划线' }
    const s = get().settings
    const cfg = s.provider[oldName]
    if (!cfg || typeof cfg !== 'object') return { ok: false, error: 'provider 不存在' }
    if (Object.prototype.hasOwnProperty.call(s.provider, name)) return { ok: false, error: '已存在同名 Provider' }
    const wasActive = s.provider.active === oldName
    // 1) 新 id 落盘：全量配置迁移（models/max_tokens/prices/input_types/openai_compat/protocol/key）
    const r = await transport.providerSave({
      provider: name,
      base_url: cfg.base_url,
      protocol: cfg.protocol,
      openai_compat: cfg.openai_compat,
      models: cfg.models,
      max_tokens: cfg.max_tokens,
      prices: cfg.prices,
      input_types: cfg.input_types,
      auth_type: cfg.auth_type,
      usage_input_includes_cache: cfg.usage_input_includes_cache ?? null, // usage 口径标注随卡迁移
      cache_ttl_1h: cfg.cache_ttl_1h ?? null, // 缓存 TTL 标注随卡迁移
      api_key: cfg.api_key,
      ...(wasActive ? { active: name } : {}),
    })
    if (!r.ok) return r
    // 2) 删旧 id（物理删除；不触发 removeProvider 的 set_provider 通知——bridge 内存
    //    由下面合并后的 set_provider 一次性重建，避免中间态把 active 切走）
    const rm = await transport.providerRemove(oldName)
    if (!rm.ok) {
      // 回滚：删掉刚建的新条目，保持原状
      await transport.providerRemove(name)
      return rm
    }
    const s2 = await transport.settingsGet()
    set({ settings: s2, providerKeys: { ...get().providerKeys, [name]: Boolean(r.hasKey), [oldName]: false } })
    send({ type: 'set_provider', payload: { provider: s2.provider.active, active: s2.provider.active } })
    return { ok: true }
  },
  // 删除自定义 provider：providerRemove 清 settings + key；删的是活跃 → electron 已回退内置；
  // 通知 bridge 重建（loadSettings 清理该 provider 的内存残留）。
  removeProvider: async (provider) => {
    const r = await transport.providerRemove(provider)
    if (r.ok) {
      const s = await transport.settingsGet()
      set({ settings: s, providerKeys: { ...get().providerKeys, [provider]: false } })
      send({ type: 'set_provider', payload: { provider: s.provider.active, active: s.provider.active } })
    }
    return r
  },
  // 拉取 provider 模型列表（list_models；响应经 dispatchEvent 落 fetchedModels）
  fetchModels: (provider) => {
    send({ type: 'list_models', payload: { provider } })
  },
  // 从实时价源补价（refresh_prices → models.dev / openrouter）。
  //
  // 为什么必须在这里落盘：桥侧刻意**只改内存**（userPriceTable + 注册表 override，即刻影响
  // FillUsageCost / 会话成本 / cost_usd），settings.json 由 Electron 独占写（桥不抢写），
  // 响应里 `persisted:false` + note 就是让调用方走 providerSave —— 少了这一步，价格只活在
  // 当前进程里，**重启即丢**（用户实测的「重启后没生效」）。
  //
  // 覆盖语义（2026-09-21 用户决策「直接覆盖就行」）：默认 overwrite=true —— 价源知道的所有
  // 模型都按实时价重写（含手填价与内置价）。代价要知情：写进 userPriceTable 后用户价优先级
  // 最高，会挡住**未来内置价更新**（价源每次重拉会自然刷新，故只影响「同一价源里改价」之外的
  // 场景）；代价换的是「拉一次列表价格就是最新且落盘的」，不需要逐模型手改。
  // overwrite:false 保留保守路径（只补无价模型，不动手填价/内置价）。
  refreshPrices: async (p) => {
    const overwrite = p.overwrite !== false // 默认覆盖
    // 在途登记（onEvent）必须在 send 之前：mock 命令响应是同步发出的，send 之后才挂监听
    // 会漏掉同步到达的响应（promoteTask 同款注释）；id 取 send() 内 ++cmdSeq 的确定值。
    const id = cmdSeq + 1
    const resp = await new Promise<{ ok: boolean; error?: string; data?: unknown }>((resolve) => {
      let timer: ReturnType<typeof setTimeout> | undefined
      const off = transport.onEvent((line) => {
        try {
          const ev = JSON.parse(line) as { event_type?: string; id?: number; ok?: boolean; error?: string; data?: unknown }
          if (ev.event_type !== 'command_response' || ev.id !== id) return
          off()
          if (timer) clearTimeout(timer)
          resolve({ ok: ev.ok === true, error: ev.error, data: ev.data })
        } catch {
          /* 非 JSON 事件忽略 */
        }
      })
      // 超时比通用命令长：价目文件 ~4.7MB（models.dev 全量），慢网也要留余量。
      timer = setTimeout(() => { off(); resolve({ ok: false, error: 'bridge 响应超时' }) }, 30_000)
      send({ type: 'refresh_prices', payload: { provider: p.provider, source: p.source, overwrite } })
    })
    if (!resp.ok || !resp.data || typeof resp.data !== 'object') return undefined
    const d = resp.data as {
      source?: string; filled?: string[]; filled_count?: number; skipped_count?: number; total?: number
      prices?: Record<string, ModelPrices>
    }
    // 桥回传的是**整表**（既有用户价 + 本次补入/覆盖），整表写回与桥/Electron 的替换语义
    // 一致（prices 里没有的模型 = 无覆盖），因此不会丢已有条目。
    const prices = d.prices ?? {}
    if (Object.keys(prices).length > 0) {
      await get().saveProvider({ provider: p.provider, prices })
    }
    return {
      source: String(d.source ?? ''),
      filled: Array.isArray(d.filled) ? d.filled : [],
      filledCount: Number(d.filled_count ?? (Array.isArray(d.filled) ? d.filled.length : 0)),
      skippedCount: Number(d.skipped_count ?? 0),
      total: Number(d.total ?? 0),
    }
  },
  // 拉取内置 Provider 预设（list_provider_presets ← provider/providers.json 单一事实源）。
  // 响应经 dispatchEvent 落 providerPresets；SettingsModal 下拉据此渲染（不再硬编码）。
  fetchProviderPresets: () => {
    send({ type: 'list_provider_presets', payload: {} })
  },
  // 测试连接（test_connection；GET /models 探活，带 10s 超时）。瞬态——把待保存的
  // base_url + api_key 直接发给 bridge，不落盘、不改配置。结果经 Promise 消费
  //（dispatchEvent 对 test_connection 直接 return，失败不外溢到 bridgeError）。
  testProvider: (base_url, api_key) => {
    const id = ++cmdSeq
    pending[id] = { type: 'test_connection', navigationGeneration }
    return new Promise<{ ok: boolean; error?: string; count?: number }>((resolve) => {
      let timer: ReturnType<typeof setTimeout> | undefined
      const off = transport.onEvent((line) => {
        try {
          const ev = JSON.parse(line) as { event_type?: string; id?: number; ok?: boolean; error?: string; data?: { count?: number } }
          if (ev.event_type !== 'command_response' || ev.id !== id) return
          off()
          if (timer) clearTimeout(timer)
          if (ev.ok) resolve({ ok: true, count: ev.data?.count })
          else resolve({ ok: false, error: ev.error ?? '连接失败' })
        } catch {
          /* 非 JSON 事件忽略 */
        }
      })
      timer = setTimeout(() => {
        off()
        resolve({ ok: false, error: '连接超时' })
      }, 15000)
      void transport.send({ type: 'test_connection', id, payload: { base_url, api_key } })
    })
  },
  // 保存 provider 配置：持久化（providerSave）+ 刷新 settings + 热切换（set_provider 命令，不重启 bridge）
  saveProvider: (p): Promise<boolean> => {
    return (async () => {
      const r = await transport.providerSave(p)
      if (r.ok) {
        // 持久化成功 → 重新读取 settings 刷新 store（否则 base_url/models 仍是旧值，重开弹窗回退）
        const s = await transport.settingsGet()
        set({ settings: s, providerKeys: { ...get().providerKeys, [p.provider]: Boolean(r.hasKey) } })
      }
      // 热切换：base_url/key/protocol 变更下一轮生效（bridge set_provider 重建全部会话 loop）。
      // 问题六（主因）：命令与 providerSave 异步落盘存在竞态——bridge 先执行 set_provider 时
      // loadSettings 可能读到旧 settings.json。payload 必须自带 models/max_tokens 兜底
      // （与 toggleModelWindow / list_models 路径同语义），否则窗口热切换不生效。
      // 2026-09 修复：必须在 providerSave + settingsGet 刷新 store 之后再发送 —— 否则 payload
      // 里的 models/max_tokens 来自旧 store，会覆盖落盘的新值（bridge loadSettings 先于
      // setModels(payload) 执行，payload 旧值把新窗口打回）。
      const cmd: ProviderSaveInput = { provider: p.provider }
      if (p.active) cmd.active = p.active
      if (p.base_url) cmd.base_url = p.base_url
      if (p.protocol) cmd.protocol = p.protocol
      if (p.openai_compat) cmd.openai_compat = p.openai_compat
      if (p.api_key) cmd.api_key = p.api_key
      if (p.auth_type) cmd.auth_type = p.auth_type // OAuth 鉴权方式（bridge set_provider 消费）
      // usage 口径：true/false 显式标注；显式 null = 清除回自动（缺键 = 其它保存路径不动它，
      // bridge 侧按「键存在才动」处理，避免改价格/窗口时误清用户标注）。
      if (p.usage_input_includes_cache === true || p.usage_input_includes_cache === false) {
        cmd.usage_input_includes_cache = p.usage_input_includes_cache
      } else if (p.usage_input_includes_cache === null) {
        cmd.usage_input_includes_cache = null
      }
      // 缓存 TTL 标注：同三态（true=1h / false=5m / null=清除回自动）。
      if (p.cache_ttl_1h === true || p.cache_ttl_1h === false) {
        cmd.cache_ttl_1h = p.cache_ttl_1h
      } else if (p.cache_ttl_1h === null) {
        cmd.cache_ttl_1h = null
      }
      if (p.input_types) cmd.input_types = p.input_types // 输入类型门控（bridge 消费：模态覆盖注册表）
      if (p.prices) cmd.prices = p.prices // 每模型价表覆盖（bridge 消费：价表覆盖注册表）
      {
        const provKey = p.provider as 'deepseek' | 'openai' | 'opencode' | 'kimi' | 'zhipu'
        const cur = get().settings.provider[provKey]
        if (cur && typeof cur === 'object') {
          const cfg = cur as { models?: Record<string, number>; max_tokens?: Record<string, number>; prices?: Record<string, { input: number; cache_read: number; cache_write: number; output: number }> }
          if (cfg.models && Object.keys(cfg.models).length > 0) (cmd as unknown as Record<string, unknown>).models = cfg.models
          if (cfg.max_tokens && Object.keys(cfg.max_tokens).length > 0) (cmd as unknown as Record<string, unknown>).max_tokens = cfg.max_tokens
          if (cfg.prices && Object.keys(cfg.prices).length > 0) (cmd as unknown as Record<string, unknown>).prices = cfg.prices
        }
      }
      send({ type: 'set_provider', payload: cmd as unknown as Record<string, unknown> })
      return r.ok
    })()
  },
  // —— OAuth 订阅登录（2026-08 P2）——
  // 发起 OAuth 登录（bridge oauth_login；auth_url/device_code 事件经 onEvent 通道回前端展示）。
  // 返回 Promise：登录流程结束后 resolve（成功/失败），调用方据此复位 UI 状态。
  oauthLogin: (provider) => {
    return (async () => {
      const r = await transport.oauthLogin(provider)
      if (r.ok) {
        const s = await transport.settingsGet()
        set({ settings: s })
        await get().refreshOAuthStatus(provider)
      }
      return r
    })()
  },
  // OAuth 登出：删除凭证 + 回退 api_key + 通知 bridge 重建。
  oauthLogout: async (provider) => {
    const r = await transport.oauthLogout(provider)
    if (r.ok) {
      const s = await transport.settingsGet()
      set({ settings: s })
      send({ type: 'set_provider', payload: { provider: s.provider.active, active: s.provider.active } })
      await get().refreshOAuthStatus(provider)
    }
    return r
  },
  // 刷新某 provider 的 OAuth 登录状态（面板展示：登录中/已登录/订阅）。
  refreshOAuthStatus: async (provider) => {
    try {
      const st = await transport.oauthStatus(provider)
      set((s) => ({ oauthStatus: { ...s.oauthStatus, [provider]: st } }))
    } catch {
      /* 状态查询失败不阻断面板 */
    }
  },
  refreshAllOAuthStatus: () => {
    const provs = Object.keys(get().settings.provider).filter((k) => k !== 'active')
    for (const p of provs) void get().refreshOAuthStatus(p)
  },
  // 用户在前端登录卡片输入授权码/重定向 URL → 回填 bridge 等待中的 Prompt。
  oauthPromptAnswer: (provider, value, requestId) => transport.oauthPromptAnswer(provider, value, requestId),
  // 标 1M → 模型窗口 1000000；否则回 128000。持久化 + set_provider（携带 models）让
  // bridge 压缩窗口联动（payload 兜底 settings.json 落盘时序）。
  toggleModelWindow: (provider, model, oneM) => {
    const s = get().settings
    const prov = s.provider[provider as 'deepseek' | 'openai' | 'opencode' | 'kimi' | 'zhipu']
    const models = { ...prov.models, [model]: oneM ? MODEL_WINDOW_1M : MODEL_WINDOW }
    set({ settings: { ...s, provider: { ...s.provider, [provider as 'deepseek' | 'openai' | 'opencode' | 'kimi' | 'zhipu']: { ...prov, models } } } })
    void get().providerSave({ provider, models })
    send({ type: 'set_provider', payload: { provider, base_url: prov.base_url, models } })
  },
  // 每模型上下文窗口（contextWindow，数值）：整表补齐（已配置保持 + 未知补已知默认 + 本模型新值）。
  // 持久化 + set_provider（携带 models）让 bridge 的 WithContextWindow 联动；bridge switch_model
  // 切模型时按此窗口重解析，下一轮 LLM 生效。
  setModelWindow: (provider, model, window) => {
    const s = get().settings
    const prov = s.provider[provider as 'deepseek' | 'openai' | 'opencode' | 'kimi' | 'zhipu']
    const known = KNOWN_MODEL_WINDOWS[provider] ?? {}
    const models: Record<string, number> = {}
    for (const name of Object.keys(prov.models)) {
      models[name] = prov.models[name] ?? known[name] ?? MODEL_WINDOW
    }
    models[model] = window
    set({ settings: { ...s, provider: { ...s.provider, [provider as 'deepseek' | 'openai' | 'opencode' | 'kimi' | 'zhipu']: { ...prov, models } } } })
    void get().providerSave({ provider, models })
    send({ type: 'set_provider', payload: { provider, base_url: prov.base_url, models } })
  },
  // 每模型单次输出上限（max_tokens）：整表补齐（已配置保持 + 未知补已知默认 + 本模型新值），
  // 持久化 + set_provider 携带 max_tokens（bridge 立即生效，不依赖 settings.json 落盘时序）。
  setModelMaxTokens: (provider, model, tokens) => {
    // 上限校验（2026-09）：已知模型默认上限即厂商硬上限（如 GLM anthropic 端点
    // max_tokens ≤131072；配 256000 会导致每次请求 400 1210「max_tokens参数非法」）。
    // 提交值超已知上限 → clamp 到已知上限（用户配置只允许 ≤ 厂商上限）。
    const known = KNOWN_MODEL_MAX_TOKENS[provider] ?? {}
    const cap = known[model]
    if (typeof cap === 'number' && tokens > cap) tokens = cap
    const s = get().settings
    const prov = s.provider[provider as 'deepseek' | 'openai' | 'opencode' | 'kimi' | 'zhipu']
    const cur = prov.max_tokens ?? {}
    const max_tokens: Record<string, number> = {}
    for (const name of Object.keys(prov.models)) {
      max_tokens[name] = cur[name] ?? known[name] ?? MODEL_MAX_TOKENS_DEFAULT
    }
    max_tokens[model] = tokens
    set({ settings: { ...s, provider: { ...s.provider, [provider as 'deepseek' | 'openai' | 'opencode' | 'kimi' | 'zhipu']: { ...prov, max_tokens } } } })
    void get().providerSave({ provider, max_tokens })
    send({ type: 'set_provider', payload: { provider, base_url: prov.base_url, models: prov.models, max_tokens } })
  },
  // 每模型输入类型（input_types；默认 ["text"]，勾选图片 → ["text","image"]）：多模态门控
  // 数据源 = settings.provider[].input_types[model]（Electron settings.json 持久化；UI 据此
  // 决定是否显示图片上传按钮）。持久化 + set_provider 同步 payload。
  // 未显式配置时以已知多模态默认（KNOWN_MODEL_INPUT_TYPES）为基底，避免把注册表
  // 真实支持 image 的模型误写成纯文本（用户显式取消勾选才写 ["text"]）。
  toggleModelImage: (provider, model, on) => {
    const s = get().settings
    const prov = s.provider[provider as 'deepseek' | 'openai' | 'opencode' | 'kimi' | 'zhipu']
    const known = KNOWN_MODEL_INPUT_TYPES[provider]?.[model] ?? ['text']
    const cur = prov.input_types?.[model] ?? known
    const base = cur.length ? cur.filter((t) => t !== 'image') : ['text']
    const input_types = { ...(prov.input_types ?? {}), [model]: on ? [...base, 'image'] : base }
    set({ settings: { ...s, provider: { ...s.provider, [provider as 'deepseek' | 'openai' | 'opencode' | 'kimi' | 'zhipu']: { ...prov, input_types } } } })
    void get().providerSave({ provider, input_types })
    send({ type: 'set_provider', payload: { provider, base_url: prov.base_url, models: prov.models, max_tokens: prov.max_tokens, input_types } })
  },
  // —— 模型清单编辑（2026-09）：手动添加/改名/删除 ——
  // 改名/删除要把 models/max_tokens/input_types/prices 四个 model-keyed 表同 key 迁移/清除，
  // 否则残留旧 key 会让模型下拉多出幽灵项、且 bridge 侧注册表同步不到（overrideRegistryLocked
  // 遍历 models ∪ maxTokens ∪ inputTypes —— 任一张表残留旧 key 都会让旧模型复活）。
  // 保存走 providerSave（整表覆盖）+ set_provider（payload 全量带模型表，bridge 热同步）。
  // Provider 是任意 id（不限于内置 5 家），此处不做 key 收窄（与既有 toggle* 的强制类型转换
  // 等价但更宽松——settings.provider 索引签名本就允许任意 id）。
  addModel: async (provider, model, window, maxTokens) => {
    const name = model.trim()
    if (!name || !/^[^\s]+$/.test(name)) return { ok: false, error: '模型名不能为空/含空白' }
    const s = get().settings
    const prov = s.provider[provider] as ProviderCfg | undefined
    if (!prov || typeof prov !== 'object') return { ok: false, error: 'provider 不存在' }
    if (prov.models[name] != null) return { ok: false, error: '已存在同名模型' }
    const known = KNOWN_MODEL_WINDOWS[provider] ?? {}
    const knownMax = KNOWN_MODEL_MAX_TOKENS[provider] ?? {}
    const models = { ...prov.models, [name]: window ?? known[name] ?? MODEL_WINDOW }
    const max_tokens = { ...(prov.max_tokens ?? {}), [name]: maxTokens ?? knownMax[name] ?? MODEL_MAX_TOKENS_DEFAULT }
    set({ settings: { ...s, provider: { ...s.provider, [provider]: { ...prov, models, max_tokens } } } })
    const ok = await get().providerSave({ provider, models, max_tokens })
    send({ type: 'set_provider', payload: { provider, base_url: prov.base_url, models, max_tokens } })
    return { ok, error: ok ? undefined : '保存失败' }
  },
  renameModel: async (provider, oldName, newName) => {
    const name = newName.trim()
    if (!name || !/^[^\s]+$/.test(name)) return { ok: false, error: '模型名不能为空/含空白' }
    if (oldName === name) return { ok: true }
    const s = get().settings
    const prov = s.provider[provider] as ProviderCfg | undefined
    if (!prov || typeof prov !== 'object') return { ok: false, error: 'provider 不存在' }
    if (prov.models[oldName] == null) return { ok: false, error: '原模型不存在' }
    if (prov.models[name] != null) return { ok: false, error: '已存在同名模型' }
    const mv = <T>(m: Record<string, T> | undefined): Record<string, T> | undefined => {
      if (!m || m[oldName] == null) return m
      const n = { ...m }
      delete n[oldName]
      n[name] = m[oldName]
      return n
    }
    const models = mv(prov.models)!
    const max_tokens = mv(prov.max_tokens)
    const input_types = mv(prov.input_types)
    const prices = mv(prov.prices)
    set({ settings: { ...s, provider: { ...s.provider, [provider]: { ...prov, models, max_tokens, input_types, prices } as ProviderCfg } } })
    const ok = await get().providerSave({ provider, models, max_tokens, input_types, prices })
    // 全量带模型表（改名后旧 key 必须从 bridge 内存消失——setModels 整表覆盖）
    send({ type: 'set_provider', payload: { provider, base_url: prov.base_url, models, max_tokens, input_types, prices } })
    // 当前 UI/会话正在用被改名的模型（且 provider 匹配）→ 乐观切到新名并通知 bridge
    //（旧 loop 的 model 引用已失效：set_provider rebuildAll 会把缺失模型回退默认；
    // switch_model 立即指回新名，避免 UI 显示旧名 / 会话请求 404）。
    const st = get()
    if (provider === st.settings.provider.active && st.model === oldName) {
      set({ model: name })
      if (st.activeSessionId) {
        send({ type: 'switch_model', payload: { session_id: st.activeSessionId, model: name, provider } })
      }
    }
    return { ok, error: ok ? undefined : '保存失败' }
  },
  removeModel: (provider, model) => {
    const s = get().settings
    const prov = s.provider[provider] as ProviderCfg | undefined
    if (!prov || typeof prov !== 'object' || prov.models[model] == null) return
    const drop = <T>(m: Record<string, T> | undefined): Record<string, T> | undefined => {
      if (!m || m[model] == null) return m
      const n = { ...m }
      delete n[model]
      return n
    }
    const models = drop(prov.models)!
    const max_tokens = drop(prov.max_tokens)
    const input_types = drop(prov.input_types)
    const prices = drop(prov.prices)
    set({ settings: { ...s, provider: { ...s.provider, [provider]: { ...prov, models, max_tokens, input_types, prices } as ProviderCfg } } })
    void get().providerSave({ provider, models, max_tokens, input_types, prices })
    send({ type: 'set_provider', payload: { provider, base_url: prov.base_url, models, max_tokens, input_types, prices } })
    // 当前 UI/会话正在用被删的模型（且 provider 匹配）→ 回退默认（rebuildAll 也会兜底，
    // 这里乐观同步 UI + 逐会话指回默认模型，避免 UI 残留不可用模型名）
    const st = get()
    if (provider === st.settings.provider.active && st.model === model) {
      const fb = defaultModelFor(st.settings)
      set({ model: fb })
      if (st.activeSessionId) {
        send({ type: 'switch_model', payload: { session_id: st.activeSessionId, model: fb, provider } })
      }
    }
  },
  // 每模型价表覆盖（USD/1M tokens；仅存显式设置项——prices 全 0 = 免费模型）。
  // 清空入口（四个字段全空 → 视为回退默认）由调用方在提交前完成：删 prices[model] key。
  setModelPrices: (provider, model, prices) => {
    const s = get().settings
    const prov = s.provider[provider] as ProviderCfg | undefined
    if (!prov || typeof prov !== 'object' || prov.models[model] == null) return
    const pricesMap = { ...(prov.prices ?? {}), [model]: prices }
    set({ settings: { ...s, provider: { ...s.provider, [provider]: { ...prov, prices: pricesMap } } } })
    void get().providerSave({ provider, prices: pricesMap })
    send({ type: 'set_provider', payload: { provider, base_url: prov.base_url, models: prov.models, max_tokens: prov.max_tokens, prices: pricesMap } })
  },
  // 清除某模型价表覆盖（回退内置注册表价）：删除 prices[model] 条目再整体提交。
  clearModelPrices: (provider, model) => {
    const s = get().settings
    const prov = s.provider[provider] as ProviderCfg | undefined
    if (!prov || typeof prov !== 'object') return
    if (!prov.prices || prov.prices[model] == null) return
    const prices = { ...prov.prices }
    delete prices[model]
    const patch: ProviderSaveInput = { provider, prices }
    set({ settings: { ...s, provider: { ...s.provider, [provider]: { ...prov, prices } } } })
    void get().providerSave(patch)
    send({ type: 'set_provider', payload: { provider, base_url: prov.base_url, models: prov.models, max_tokens: prov.max_tokens, prices } })
  },
}))

// M6：内嵌浏览器全局事件订阅（store 创建后启动一次；见 subscribeEmbedEvents 语义说明）。
// 防御性包裹：初始化失败只降级 embed 能力，绝不允许拖垮整个应用模块加载。
try {
  subscribeEmbedEvents()
} catch (e) {
  console.error('[embed] 订阅初始化失败（已降级）:', e)
}

// 阶段 5：标签集持久化的写入收口（唯一）。挂在订阅上而不是逐个 action 插桩，理由见
// schedulePersistTabs 的注释。只认**引用变化**（tabsByScope 的更新一律整体替换，见 tabPatch
// 与 deleteSession），所以这里等价于「标签集真的变了」。防抖在 schedulePersistTabs 里。
useAppStore.subscribe((s, prev) => {
  if (s.tabsByScope !== prev.tabsByScope) schedulePersistTabs()
})
// 关窗前把防抖窗口里的**待写**落盘（localStorage 是同步的，但定时器可能还没到）。
// beforeunload 在 Electron 里对「窗口关闭」会触发；取不到事件 API 的环境（L1 的假 window）跳过。
if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
  window.addEventListener('beforeunload', () => flushPendingPersistTabs())
}
