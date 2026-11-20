import type { BridgeCommand } from '../store/events'

export interface BridgeExitInfo {
  code: number | null
  err: string
}

// 全局设置（对齐 electron/main.ts 的 AppSettings；API key 明文持久化在 settings.json
// provider 段——桌面端 Provider 设置直接写盘，2026-08-16 用户决策）
export interface OpenAICompatCapabilities {
  minimal?: boolean
  base_path?: string
  reasoning_effort?: boolean
  max_completion_tokens?: boolean
  stream_usage?: boolean
  tools?: boolean
}

export interface ModelPrices {
  input: number // USD / 1M tokens（输入未命中价）
  cache_read: number // USD / 1M tokens（缓存命中价）
  cache_write: number // USD / 1M tokens（缓存写入价；多数厂商 0）
  output: number // USD / 1M tokens
}

export interface ProviderCfg {
  base_url: string
  // label 显示名（2026-11）：只影响 UI。id（settings.provider 的键）保持不可变，因为
  // metrics 记账 / 用户价表 / billing 请求 / GOCODE_KEY_<NAME> 兜底全部按 id 索引 ——
  // 改 id 会让历史统计断成两个 provider。空/缺省 = 用内置名或 id。
  label?: string
  // 瞬态输入，不持久化到 settings（BR-08）：key 由主进程落 keys.json（safeStorage + 0600），
  // settings.json 里**不再**出现 provider key；读侧也被主进程抹掉了。
  api_key?: string
  models: Record<string, number> // model → context window（tokens；1M=1000000）
  max_tokens: Record<string, number> // model → 单次输出上限（tokens；DeepSeek v4-flash 官方 384k）
  // 每模型用户价表覆盖（USD/1M tokens；缺省/全 0 = 用内置注册表价表）。
  // 维度 model → 单价：价表挂 provider 段（2026-10 起 deepseek-v4-flash 直连与
  // OpenCode Go 同价 0.15/0.003/0.6 —— 仍按 provider 分别记账，勿跨 provider 套价）。
  prices?: Record<string, ModelPrices>
  protocol?: 'chat_completions' | 'responses' | 'anthropic' // OpenAI 兼容接入协议；responses/anthropic 暂未支持（默认 chat_completions）
  // 自定义 OpenAI-compatible 端点的请求能力；未配置保持完整 OpenAI 请求。
  openai_compat?: OpenAICompatCapabilities
  input_types?: Record<string, string[]> // model → 输入类型（默认 ["text"]；支持多模态 = ["text","image"] —— UI 据此门控图片上传）
  auth_type?: 'api_key' | 'oauth' // 鉴权方式（OAuth 订阅登录 2026-08；缺省 api_key；oauth 时 token 存 oauth.json 不入 settings）
  // usage 口径显式标注（2026-09-21）：input_tokens 是否**已含**缓存命中/写入。
  // 缺省（字段不存在）= 自动：anthropic 协议按官方语义（未含 → 相加），OpenAI 形状网关靠
  // 响应自报字段嗅探。端点违背自己声明的协议时（如 anthropic 协议 + OpenAI 计数）必须显式标注，
  // 否则上下文占用/命中率/成本会整体偏一倍。见 provider.Registry.UsageInputIncludesCache。
  usage_input_includes_cache?: boolean
  // Anthropic 缓存断点 TTL 标注（2026-09-21）：true = 1h extended TTL（写入价 2× 基础输入价），
  // false = 5m（写入价 1.25×）。缺省 = 自动：官方 api.anthropic.com 用 1h，兼容端点用 5m
  //（它们只实现 Messages 基础协议、ttl 是 Claude 扩展字段）。
  cache_ttl_1h?: boolean
  deleted?: boolean // 用户已删除（内置 5 家删除标记；bridge 重启不复活）
}

// ProviderPreset 内置 Provider 预设（数据源：bridge list_provider_presets ← provider/providers.json
// 单一事实源；前端不再硬编码预设表）。
export interface ProviderPreset {
  name: string
  id: string
  base_url: string
  protocol: 'chat_completions' | 'responses' | 'anthropic'
  models: string[] // 预设模型清单（注册表内置）
  oauth?: {
    name?: string
    flow?: string
    is_subscription?: boolean
    login_label?: string
    key_instead?: boolean
  }
}
// 单台 MCP 服务器配置（与 bridge mcp.ServerConfig 同构；env/headers 明文存 settings.json——v1 决策）
export interface MCPServerCfg {
  // type：stdio（默认）| http | streamable_http | sse。streamable_http / streamable-http
  // 都是 http 的别名（同一个 MCP StreamableHTTP 传输，bridge 侧归一）。
  type?: 'stdio' | 'http' | 'streamable_http' | 'streamable-http' | 'sse'
  command?: string
  args?: string[]
  env?: Record<string, string>
  url?: string
  // headers 远端传输（http/sse）每次请求附带的 HTTP 头（如 Authorization: Bearer …）。
  // 为什么必须有：多数托管 MCP 端点用 Authorization 把关，没有自定义头就连不上
  // （原缺口：「http 模式不支持 Header 设置，导致链接无法成功」）。
  // 明文存 settings.json 与面板原样回显（用户自己填的凭证——2026-10 决策，不做 ${VAR} 展开）。
  // 仅远端有意义；stdio 是子进程，没有 HTTP 头这一层（写了不报错但无效果）。
  headers?: Record<string, string>
  enabled?: boolean
  // exposure 暴露档位（config 层词汇，折法与默认见 mcp.Manager.DefaultExposure）：
  //   direct   直接声明给模型（工具多时会撑爆工具表）
  //   codemode 可被 codemode 脚本调用、不写进 codemode 描述
  //   deferred 同上，由 tool_search 现查（引擎侧出厂默认）
  //   hidden   撤下（不声明、不进描述、脚本内也不可达）
  // 不设 = 未声明：有效档位由宿主装配决策解析（codemode 开 = deferred 按需可搜；关 = direct 直接可见）。
  exposure?: 'direct' | 'codemode' | 'deferred' | 'hidden'
}
// 命令沙箱设置（macOS Seatbelt / sandbox-exec）。sensitive_paths 追加进策略的
// 强制 deny-read 路径（绝对路径或相对 home；默认已含 ~/.ssh/.aws/Keychains 等）。
export interface SandboxSettings {
  mode: 'none' | 'seatbelt'
  sensitive_paths?: string[]
}
// permissions 设置段：仅存 Computer Use 已授权 App（统一授权门禁；设置 → 插件 →
// Computer Use 维护）。旧 allowed_apps（open 白名单）已随「权限」设置页移除（2026-09）。
// 显示名匹配（大小写不敏感子串）；空 = 全部拒绝（安全默认）。
export interface PermissionsSettings {
  computer_approved_apps: string[]
}
// 定时任务设置（CronCreate/CronList/CronUpdate/CronDelete 工具 + 触发调度）。
// enabled: 是否开启（开启才在 AgentHarness 注册 cron 工具并触发；关闭=不触发但任务保留）。
// auto_clean: 是否自动清理 N 天前派生的会话文件（默认开；任务定义与运行账本保留）。
export interface CronSettings {
  enabled: boolean
  auto_clean: boolean
}
// settings.json codemode 段（脚本编排工具，Wave 3）。字段与默认值见 bridge 的
// loadCodemodeSettings（desktop/bridge/codemode.go）：缺字段/整段缺失一律走默认，
// 非法值退化到默认并在 bridge stderr 留警告。
export interface CodemodeSettings {
  // 是否注册 codemode 工具（默认 false = 完全不注册）。
  enabled: boolean
  // 模式："on"（默认，已声明工具保持声明 + 描述追加「也可从脚本调用」）| "off"（不激活）。
  // "only"（已声明工具对模型隐藏）在 Wave 2 只做了一半，bridge 会**拒绝**它并按关闭处理。
  mode?: 'on' | 'off'
  // 目录描述的估算 token 上限：>0 覆盖，<0 = 不限，0/缺省 = 工具内建默认（3000）。
  inline_budget?: number
  // 脚本未在首行 // @options 声明 timeout_ms 时的默认墙钟上限（秒，>0，≤ 3600）。
  // 缺省 = 30 秒（工具内建默认）；脚本自己的声明永远优先。
  budget_seconds?: number
}
// settings.json agent 段：子 agent 运行时调参（提醒阈值 + 并发槽上限）。
export interface AgentSettings {
  reminder_rounds: number
  // 子 agent 并发槽上限（缺省 = bridge 的 subagent.DefaultConcurrency = 1000）。
  // max_subagents = 主池（agent_spawn，写型子 agent）；max_explore_subagents =
  // 辅池（subagent_explore，只读）。两池独立，互不饿死。
  // 注意：槽满时工具调用是**同步阻塞**的（主 Agent 整批挂住，任何超时都解不开），
  // 所以这两个值只是「失控护栏」，别调小到接近日常并发。
  max_subagents?: number
  max_explore_subagents?: number
  // 自动压缩触发阈值（占模型上下文窗口比例，缺省 0.8 = 80%）。调小 = 更早压缩
  // （上下文更短、后续请求更便宜，但压缩更频繁）；调大 = 更晚压缩（单请求更长、
  // 更容易撞窗口）。范围 (0, 1]，缺失/非法按 0.8。
  compress_threshold?: number
  // 首字预算（秒，缺省 90）：单次 LLM 尝试从「请求发出」到「收到模型首个流事件」的上限，
  // 首个事件一到即解除，流开始后**不限制总时长**（长 reasoning / 大文档生成不受切）。
  // 调大 = 更能容忍弱网与长首字模型（GLM / Claude 长思考 / 跨境链路 / 企业代理）；
  // 调小 = 更快判定「上游无响应」并重试。范围 (0, 900]，缺失/非法按 90。
  //
  // 为什么要暴露：这个预算**每次尝试独立计时**，而重试不会让同一个慢模型变快 ——
  // 30s 时弱网模型每一轮都被切成「上游故障」，重试 10 次全废，用户侧恒表现为
  // 「总是异常」后耗尽成供应商侧故障。默认值已调宽到 90s，此处留给按 provider 特性再调。
  first_chunk_timeout?: number
  // 进程级 provider 并发上限（P6-03，缺省/0 = 不限，硬上限 64）。群聊并行投递时
  // 11 个目标会同时打 provider，这一层是「别把上游打到 429」的总闸；调小 = 更保守。
  max_concurrent_provider_calls?: number
}
// 群聊对话引擎的全局护栏（P6-04/D8，settings.json 顶层 `hub.conversation`）。
// **不是群策略**：调的是「对话引擎怎么防无限接龙」，所有群共用一套，0/缺省回落默认值。
export interface HubConversationSettings {
  max_depth?: number // 每条链的深度上限（P6-05，默认 1000）
  pair_limit?: number // 无序 pair 在窗口内的成功投递上限（默认 10）
  pair_window_ms?: number // pair 计数的滚动窗口（默认 600000 = 10 分钟）
  idle_reset_ms?: number // 群空闲多久回 idle 并清零（默认 1800000 = 30 分钟）
  cooldown_ms?: number // 两次自动唤醒之间的最小间隔（默认 0）
  run_timeout_ms?: number // 单轮 run 上限（默认 600000；P6-08 才真正中断）
  max_pending_wakes?: number // 待投递队列上限（默认 200）
}
// Session Mesh 运行状态（mesh_status 命令响应；设置页「别人怎么连我」数据源）。
export interface MeshStatusInfo {
  session_enabled: boolean
  remote_enabled: boolean
  node_id?: string
  listen_addr?: string
  tls?: boolean // true = wss；false/undefined = ws
  connect_urls?: string[] // 别人可连接本机的地址（监听 + 网卡探测的公网/LAN 候选）
  links?: { node_id: string; inbound: boolean; connected_at?: string; last_seen?: string }[]
  peers?: {
    // 出站节点（peer）拨号状态：connected 是否已连、last_error 最近失败原因（排障）
    node_id: string
    addr?: string
    connected?: boolean
    last_error?: string
    last_attempt?: string
    last_success?: string
    rtt_ms?: number // -1 = 未 ping
  }[]
  remote_sessions?: {
    // 对端节点在线会话摘要（session_list 的远程部分；含会话标题 name）
    node: string
    id: string
    name?: string
    status?: string
    last_seen?: string
  }[]
  reason?: string // 网关未运行原因（remote disabled / start failed）
}

// 语音输入（STT）模型配置：OpenAI 兼容 /audio/transcriptions 端点。
// enabled + base_url + model + api_key 全部齐全时，Composer 才显示话筒按钮。
export interface STTSettings {
  enabled?: boolean // 语音输入总开关（缺省 false）
  base_url?: string // 端点根（如 https://api.openai.com/v1）；请求发往 {base_url}/audio/transcriptions
  model?: string // 转写模型（如 whisper-1 / gpt-4o-transcribe）
  api_key?: string // API key（STT 历史：仍存 settings.json；provider key 已迁 keys.json，见 BR-08）
  vendor?: string // 当前语音服务商预设 id（openai/groq/…/custom）；仅 UI 回显（切换厂商恢复手改过的 URL/模型列表）
  base_urls?: Record<string, string> // 各服务商手改过的端点（按厂商 id 分组；UI 切回该厂商时回显，转写仍用上面的 base_url）
  custom_models?: Record<string, string[]> // 用户自定义模型名（按厂商 id 分组；下拉列表 = 预设模型 + 此处）
}
// Session Mesh 会话间通信（docs/SESSION_MESH_COLLABORATION.md）。
// 缺省全关（安全默认）：开启后才注册 session_send/session_list 工具。
export interface MeshSettings {
  session_enabled?: boolean // C1：本机多 Session 互聊工具
  remote_enabled?: boolean // C2：远程多节点互通（依赖 session_enabled）
  remote?: {
    listen_addr?: string // 本节点监听地址（缺省 127.0.0.1:17890）
    tls_cert?: string // TLS 证书路径（非空 → wss）
    tls_key?: string // TLS 私钥路径
    auth_token?: string // 握手凭据（两端一致；空 = 拒绝连接）
    node_id?: string // 本机节点标识（auto = UUID 持久化）
    peers?: { id: string; addr: string; token: string }[] // 出站节点列表
  }
}
export interface AppSettings {
  theme: 'dark' | 'light' | 'system'
  lang: 'zh' | 'en'
  // 浏览器引擎（2026-09）：mcp（默认，Browser Use）| ego（ego lite + ego-browser skill）。
  browser_engine?: 'mcp' | 'ego'
  // 注：会话页皮肤只有 runtime 一套（2026-11 移除 current/v3 选择项），故无 narrative_skin
  // 字段；旧 settings.json / 老 bridge 若还带着它，按未知字段忽略（不报错、不回写）。
  narrative_density?: 'concise' | 'detailed'
  provider: {
    active: string // 当前活跃 provider id
    deepseek: ProviderCfg
    openai: ProviderCfg
    opencode: ProviderCfg
    kimi: ProviderCfg
    zhipu: ProviderCfg
    [id: string]: ProviderCfg | string // 用户自定义 OpenAI provider（key = id）；active 是字符串保留键
  }
  mcpServers: Record<string, MCPServerCfg>
  sandbox?: SandboxSettings // 命令沙箱（缺省 seatbelt；桌面端默认开启 2026-08-16 决策）
  permissions?: PermissionsSettings // open 白名单（空 = 禁用 open）
  cron?: CronSettings // 定时任务（缺省全开 2026-08-19 决策）
  // codemode（脚本编排工具，2026-10 Wave 3）：缺省**关闭**（对齐 pi defaultActive:false）。
  // 类型在这里声明是为了往返安全：settings.json 是自由 JSON，bridge 读它决定「是否注册
  // codemode 工具 + MCP 未声明档位落 direct 还是 deferred」；前端不做类型化重写内容的
  // 裁剪（readSettings 展开原始键），声明类型让面板/往返都不丢用户手写的这一节。
  codemode?: CodemodeSettings
  mesh?: MeshSettings // Session Mesh 会话间通信（缺省全关：开启后才注册 session_* 工具）
  agent?: AgentSettings // Agent 运行时设置（提醒阈值 + 子 agent 并发槽上限）
  hub?: { conversation?: HubConversationSettings } // 群聊对话引擎全局护栏（P6-04/D8）
  stt?: STTSettings // 语音输入（话筒）配置；配置齐全才显示话筒按钮
  // 内嵌编辑器回滚开关（PR-05b）：false → 文件树/预览工具栏的「编辑」入口**不出现**。
  // 缺省（字段不存在）视为开启 —— 老 settings.json 不该因为少一个键就没了这条能力。
  editor?: boolean
  // 功能开关（BR：缺省 = 关）。当前只有 surfaceTools：AI 创建/安装面板插件所需的
  // ui_inspect + surface_install 等工具。开关读不到/写坏一律按 false（fail-closed），
  // 与 desktop/bridge/surface_settings.go 同口径；设置页 →「面板插件」有可见开关。
  features?: { surfaceTools?: boolean }
}

// 一条定时任务（cron_list 返回；CronPage 数据源）
export interface CronJob {
  id: string
  cron: string
  prompt: string
  recurring: boolean
  paused?: boolean // 暂停：到点不触发、不计数；任务与历史保留
  workspace: string
  created_at: string
  next_run?: string
  last_run?: string
  last_status?: 'pending' | 'running' | 'success' | 'error' | 'skipped' | 'timeout'
  run_count: number
  expire_at?: string
  im_chat?: { gateway: string; chat_id: string } | null // IM 推送目标（cron 结果推送到群）
}
// 一次运行记录（cron_runs 返回；CronPage「运行历史」）
export interface CronRun {
  run_id: string
  job_id: string
  fired_at: string
  session_id?: string
  status: 'pending' | 'running' | 'success' | 'error' | 'skipped' | 'timeout'
  error?: string
  duration_ms?: number
  final_result?: string
}
// 某次运行的本地化历史详情（cron_runs_detail 返回；CronRunDetailPage 数据源）
// snapshot：事件日志聚合（与 new_session 恢复同构）→ 前端 materializeSnapshot 重建视图。
export interface CronRunDetail {
  job_id: string
  run_id: string
  session_id?: string
  found: boolean // 事件日志是否存在（旧数据/运行中 → 仅元数据）
  status?: 'pending' | 'running' | 'success' | 'error' | 'skipped' | 'timeout'
  fired_at?: string
  duration_ms?: number
  final_result?: string
  error?: string
  snapshot?: { events: unknown[] } // 事件日志聚合（详情页视图重建数据源）
  viewSid?: string // 前端 materialize 后的专用 view sid（渲染用，非传输字段）
}
export interface ProviderSaveInput {
  provider: string
  active?: string
  base_url?: string
  label?: string // 显示名（空串 = 清除回默认显示名）
  protocol?: string // OpenAI 接入协议（chat_completions/responses；responses 暂未支持）
  openai_compat?: OpenAICompatCapabilities
  models?: Record<string, number>
  max_tokens?: Record<string, number>
  prices?: Record<string, ModelPrices> // 每模型价表覆盖（USD/1M tokens）
  input_types?: Record<string, string[]> // model → 输入类型门控（图片上传）
  auth_type?: 'api_key' | 'oauth' // 鉴权方式（OAuth 订阅登录）
  // usage 口径：true/false = 显式标注，null = 清除标注回「自动判定」
  usage_input_includes_cache?: boolean | null
  cache_ttl_1h?: boolean | null // 缓存 TTL：true=1h / false=5m / null=清除回自动
  // 瞬态输入，不持久化到 settings：主进程把它写进 keys.json（safeStorage）。
  // null = 显式删除该 provider 的 key；undefined/'' = 不修改。
  api_key?: string | null
}

// ── Provider 余额 / 配额（provider_billing 命令响应；数据面见 provider/billing）──
// 单位与语义都来自上游：金额带 Currency（DeepSeek 实测 CNY），窗口百分比已归一 0..100。
// percent **可选**：上游报了窗口但没给读数时必须缺字段，而不是 0（0% 是「没用过」的确定结论）。
export interface BillingMoney {
  amount: number
  currency: string
}
export interface BillingWindow {
  key: string
  label?: string
  percent?: number
  status?: string
  resets_at?: string
}
export interface BillingSnapshot {
  provider: string
  fetched_at: string
  kind: 'balance' | 'quota' | 'report' | 'none'
  source?: string
  currency?: string
  balance?: BillingMoney
  limit?: BillingMoney
  used?: BillingMoney
  windows?: BillingWindow[]
  plan_label?: string
  not_connected?: boolean // 没配 key / 没登录 —— 不是错误
  experimental?: boolean   // 端点未实测（presets.json verified_by != live）
  error_code?: string
  error_status?: number
  // 本次刷新失败但字段里是上一次成功的读数（UI：旧值变灰 + 错误一行）
  stale?: boolean
}

// OAuth 登录状态（oauth_status 命令响应；不含 token）
export interface OAuthStatus {
  provider: string
  logged_in: boolean
  supports_oauth: boolean
  name?: string
  subscription?: boolean
  source?: 'oauth'
  expires_at?: number
}

// —— 文件系统（@ 引用浏览/读取、工作区内外文件选择）：renderer ↔ Electron 主进程 fs ——
export interface FsEntry {
  name: string
  path: string // 绝对路径
  isDir: boolean
  size?: number
}

export interface ExternalSkillSource {
  name: string
  root: string
  skills_dir: string
  root_exists: boolean
  skills_dir_exists: boolean
  skill_count: number
}

export interface ExternalSkillsStatus {
  enabled: boolean
  sources: ExternalSkillSource[]
}

// —— 办公组件（LibreOffice kit，R08d）——
// 与 bridge 的 office_kit_status 响应同构（desktop/bridge/office.go 的 officekit.Status）。
// state：ok=可用 / missing=没装 / invalid=装了但校验不过（sha256/版本/平台）。
export interface OfficeKitPlatform {
  id: string
  os: string
  arch: string
  supply: 'kit' | 'system' // system = 复用系统 soffice（Linux），不随 app 分发
  sizeBytes: number
  license: string
  note?: string
}

export interface OfficeKitStatus {
  available: boolean
  state: 'ok' | 'missing' | 'invalid'
  source?: 'system' | 'kit'
  bin?: string
  root?: string
  version?: string
  platform: string
  sizeBytes: number
  license?: string
  licenseUrl?: string
  sourceUrl?: string
  reason?: string
  errors?: string[]
  platforms: OfficeKitPlatform[]
  pinnedVersion: string
}

/**
 * 办公预览请求（xlsx / docx / pptx 三条命令同形）。
 *
 * approximate=true：**只要近似 HTML**，跳过 LibreOffice kit。右侧栏的面板渲染器
 *（store 的 loadOfficePreview）必须传它 —— 它把产物当文本读回来塞进 iframe，读不了 PDF；
 * 不传则 kit 可用时 html_path 是 PDF（给内嵌浏览器看的，见 previewOfficeFile）。
 */
export interface OfficePreviewRequest {
  path: string
  workspace: string
  approximate?: boolean
}

// ─── 右栏终端（PTY，docs/TERMINAL_DESIGN.md §5.1 / docs/TERMINAL_BRIDGE_SPEC.md §1.3–1.5）───
//
// 控制面三条命令（pty_create / pty_ticket / pty_close）走既有 NDJSON 命令通道；
// 数据面是一条独立 WS（`ws://127.0.0.1:<随机端口>/pty?ticket=…`），由 pty_ticket 发一次性票据。
//
// 这里的方法**不抛**：形状不对 / 业务失败对面板来说是同一件事 —— 显示一句错误
// （term.failed），别把整块右栏炸掉（见 transport/electron.ts 的 ptyInfoOf 注释）。

/** pty_create 成功后的进程描述（字段名由 Go 桥决定，见 desktop/bridge/pty.go）。 */
export interface PtyInfo {
  pty_id: string
  cwd: string
  shell: string
  pid: number
  cols: number
  rows: number
}

export interface PtyCreateInput {
  /** 终端的工作目录（= 会话所属工作区；空则连 PTY 都起不来，见 store 的 termEnsure）。 */
  workspace: string
  /** 归属会话（Go 侧只做记账/事件归因，不影响 shell 本身）。 */
  session_id?: string
  cols: number
  rows: number
}

export type PtyCreateResult = { ok: true; pty: PtyInfo } | { ok: false; error: string }

export type PtyTicketResult =
  | { ok: true; url: string; ticket: string; expiresIn: number }
  | { ok: false; error: string }

export type PtyCloseResult = { ok: boolean; error?: string }

export interface ExternalSkillsStatusResult {
  ok: boolean
  status?: ExternalSkillsStatus
  error?: string
}

export interface ExternalSkillImport {
  source: string
  name: string
  target: string
}

export interface ExternalSkillsImportResult {
  status: ExternalSkillsStatus
  imported: ExternalSkillImport[]
}

/**
 * 文件树写操作的统一回复（与 electron/fs-ops.ts 的 FsOpResult 同构）。
 * code 供渲染层分流：EXISTS（同名冲突）/ DENIED（越界或受保护）/ INVALID_NAME / NOT_FOUND / IS_ROOT / CYCLE / IO。
 */
/**
 * 批量导入的回复：imported = 落地后的绝对路径；failed 带原始 name（源文件名）与原因。
 * 与 FsOpReply 分开是因为「部分成功」是这条通路的正常结果，不是错误。
 */
export interface FsImportReply {
  ok: boolean
  imported: string[]
  failed: { name: string; error: string }[]
}

export interface FsOpReply {
  ok: boolean
  path?: string
  error?: string
  code?: string
}

// —— 输入框历史（本地文件持久化）——
// 与 electron/input-history.ts、src/lib/inputHistory.ts 同构；图片只存指纹。
export interface InputHistoryRef { path: string; name: string; size: number }
export interface InputHistoryImage { mime: string; len: number; hash: string }
export interface InputHistoryItem {
  text: string
  at?: number
  refs?: InputHistoryRef[]
  images?: InputHistoryImage[]
}
export interface InputHistoryReply { ok: boolean; items?: InputHistoryItem[]; error?: string }

export interface FsListResult {
  ok: boolean
  path: string // 请求浏览的目录（绝对路径）
  entries: FsEntry[]
  error?: string
}
// @ 引用展开结果：文件 → FilePath 块（含内容）；目录 → Dir 块（含递归文件清单）。
// content 为可直接拼进用户消息的文本。
export interface FsRefResult {
  ok: boolean
  content?: string
  error?: string
}
// 本地图片 → data URL：Markdown 里的 <img> 直接吃这个字符串（dataUrl 缺省即失败，见 error）。
export interface FsImageResult {
  ok: boolean
  dataUrl?: string
  mime?: string
  error?: string
}
export interface Transport {
  start(workspace: string): Promise<{ ok: boolean; error?: string }>
  pickWorkspace(): Promise<string | null>
  homeDir(): Promise<string> // 主目录（「新建文件夹…」modal 默认父目录）
  createWorkspaceFolder(parent: string, name: string): Promise<{ ok: boolean; path?: string; error?: string }>
  settingsGet(): Promise<AppSettings>
  settingsSet(patch: Partial<AppSettings>): Promise<{ ok: boolean; error?: string; settings?: AppSettings }>
  // 输入历史：主进程本地文件（~/.go-code/input-history.json）。浏览器 mock 返回空/成功，
  // 渲染层仍以 localStorage 镜像为准；Electron 下启动/切会话时用文件结果对齐本地镜像。
  inputHistoryGet(scope: string): Promise<InputHistoryReply>
  inputHistorySet(scope: string, items: InputHistoryItem[]): Promise<InputHistoryReply>
  inputHistoryForget(prefix: string, subtree?: boolean): Promise<{ ok: boolean; error?: string }>
  externalSkillsStatus(): Promise<ExternalSkillsStatus>
  // 办公组件状态（R08d）。refresh=true = 用户点了「重新探测」（安装/卸载发生在本进程之外）。
  officeKitStatus(refresh?: boolean): Promise<OfficeKitStatus>
  importExternalSkills(): Promise<{ ok: boolean; result?: ExternalSkillsImportResult; error?: string }>
  pickApp(): Promise<{ name: string; bundleId?: string } | null> // 系统「选择应用程序」（osascript choose application）；取消 → null
  providerSave(p: ProviderSaveInput): Promise<{ ok: boolean; hasKey?: boolean; error?: string }>
  providerRemove(provider: string): Promise<{ ok: boolean; error?: string }> // 删除自定义 provider（含 key）
  // 「已设置」布尔指示（key 住 keys.json；迁移未完成时主进程回退遗留明文）
  providerKeyStatus(provider: string): Promise<boolean>
  // 明文 key，仅供 renderer 本地「测试连接」/改名复制用（不预填、不落 settings、不写日志）。
  // 无 key / 解密失败 / 旧主进程 → 空串。
  providerKey(provider: string): Promise<string>
  // provider 改名：把 keys.json 条目从 from 复制到 to（新条目建好后、删旧条目之前调用）。
  // 旧主进程没有该方法 → { ok:false, error:'unsupported' }。
  providerCopyKey(from: string, to: string): Promise<{ ok: boolean; copied?: boolean; error?: string }>
  // OAuth 订阅登录（2026-08 P2）
  oauthLogin(provider: string): Promise<{ ok: boolean; error?: string }>
  oauthLogout(provider: string): Promise<{ ok: boolean; error?: string }>
  oauthStatus(provider: string): Promise<OAuthStatus>
  oauthPromptAnswer(provider: string, value: string, requestId?: string): Promise<{ ok: boolean; error?: string }>
  mcpSave(servers: Record<string, MCPServerCfg>): Promise<{ ok: boolean; error?: string }>
  // @ 引用文件系统（工作区内/外）：浏览目录、读取引用内容、原生对话框选外部文件/目录
  fsList(dir: string): Promise<FsListResult>
  fsReadRef(req: { path: string; workspace: string }): Promise<FsRefResult>
  /** 本地图片 → data URL（Markdown 预览用；绝对路径不受 workspace 约束，见 electron/read-image.ts）。 */
  fsReadImage(req: { path: string; workspace: string }): Promise<FsImageResult>
  /** 单路径元信息（引用附件条展示文件名 + 大小；不读内容）。 */
  fsStat(p: string): Promise<{ ok: boolean; size?: number; isFile?: boolean; error?: string }>
  // —— 文件树写操作（左栏「工作区文件」的文件夹管理）——
  // 作用域：源与目标都必须落在 workspace 内（主进程 realpath 包含判断，见 electron/fs-ops.ts）。
  // 同名冲突一律**拒绝**（code=EXISTS），不自动改名、不弹覆盖询问 —— 产品口径。
  // 全部返回结果绝对路径（path），供调用方选中/重载目录。
  fsRename(req: { workspace: string; path: string; name: string }): Promise<FsOpReply>
  fsMove(req: { workspace: string; src: string; destDir: string }): Promise<FsOpReply>
  fsMkdir(req: { workspace: string; dir: string; name: string }): Promise<FsOpReply>
  fsCreateFile(req: { workspace: string; dir: string; name: string }): Promise<FsOpReply>
  /** 复制副本：a.go → a copy.go（副本名由我们生成，冲突自动递增）。 */
  fsDuplicate(req: { workspace: string; path: string }): Promise<FsOpReply>
  /** 拷贝到另一个目录（⌘C/⌘V）：同名 → EXISTS（与移动同一口径，不自动改名）。 */
  fsCopy(req: { workspace: string; src: string; destDir: string }): Promise<FsOpReply>
  /**
   * 从访达拖入：把工作区外的文件/目录拷进 destDir。
   * 逐个报告失败（同名只挡那一个，不整批中断）；敏感目录与配置目录的源一律拒绝。
   */
  fsImport(req: { workspace: string; destDir: string; sources: string[] }): Promise<FsImportReply>
  /** 删除 = 移进系统废纸篓（可恢复）。二次确认由调用方弹。 */
  fsTrash(req: { workspace: string; path: string }): Promise<FsOpReply>
  /**
   * 原生「选择文件」单选（工作区外引用）：取消 → null。既有调用方（FileRefPanel）语义不变。
   */
  pickFsFile(): Promise<string | null>
  /**
   * 原生「选择文件」多选（Composer「添加文件」一次引多个）：取消 → []。
   * 与 pickFsFile 走同一条 fs:pick-file IPC（主进程按 opts.multi 分流），
   * 单独一个方法是为了让「单选拿到 string」与「多选拿到 string[]」在类型上各自精确，
   * 调用方无需 union 收窄。
   */
  pickFsFiles(opts?: { filters?: { name: string; extensions: string[] }[] }): Promise<string[]>
  pickFsDir(): Promise<string | null> // 原生「选择文件夹」（工作区外引用）
  /**
   * File（拖拽 / <input> 选到的）→ 磁盘路径。Electron 32+ 官方方式（webUtils.getPathForFile），
   * 取代已废弃的 File.path 属性。JS 构造的 File（非磁盘文件）返回空串，调用方须跳过并提示。
   * 浏览器 mock 返回 '/mock/...' 假路径（无真实磁盘）。
   */
  getPathForFile(file: File): string
  // xlsx 工作簿预览：bridge 用受管 Python（openpyxl）转 HTML → 返回产物路径 →
  // 前端送内嵌浏览器 file://（与 PDF 预览同一条通路；见 store 的 previewXlsx）。
  watchXlsx(req: OfficePreviewRequest): Promise<{ ok: boolean; html_path?: string; error?: string; renderer?: 'libreoffice' | 'approximate' }>
  /**
   * docx 文档 / pptx 演示文稿预览：与 watchXlsx 是**同一条通路**（同形态入参、同样返回
   * HTML 产物路径、同样送内嵌浏览器 file://），桥接层只换命令名（docx_preview /
   * pptx_preview）。拆成三个方法而不是一个带 kind 的方法，是为了与 store 的三个动作
   * （previewXlsx/previewDocx/previewPptx）一一对应；浏览器 mock 三个都返回失败。
   */
  watchDocx(req: OfficePreviewRequest): Promise<{ ok: boolean; html_path?: string; error?: string; renderer?: 'libreoffice' | 'approximate' }>
  watchPptx(req: OfficePreviewRequest): Promise<{ ok: boolean; html_path?: string; error?: string; renderer?: 'libreoffice' | 'approximate' }>
  // 语音转写：把录音音频（wav 字节）发到 OpenAI 兼容 /audio/transcriptions，返回文本。
  // 由主进程转发（net.fetch）——避开渲染层 CORS 限制、API key 不进网页层。
  sttTranscribe(audio: ArrayBuffer, mimeType: string): Promise<{ ok: boolean; text?: string; error?: string }>
  // —— 右栏终端（docs/TERMINAL_DESIGN.md §5.1）：控制面三条 ——
  // 数据面（WS）由组件自己开：票据在这里换，帧协议见 components/terminal/frame.ts。
  ptyCreate(input: PtyCreateInput): Promise<PtyCreateResult>
  ptyTicket(ptyId: string): Promise<PtyTicketResult>
  ptyClose(ptyId: string): Promise<PtyCloseResult>
  send(cmd: BridgeCommand): Promise<{ ok: boolean; error?: string }>
  onEvent(cb: (line: string) => void): () => void
  onExit(cb: (info: BridgeExitInfo) => void): () => void
  /**
   * hub:// 深链（P4-02）：主进程收到 open-url / second-instance argv 后转发。
   * 返回取消订阅函数。浏览器 mock 是空实现（没有 OS 协议处理器）。
   */
  onHubDeepLink(cb: (url: string) => void): () => void
  /** 取冷启动/刷新期间排队的深链；取完主进程转为直发（先订阅再 take 才不漏）。 */
  takeHubDeepLinks(): Promise<string[]>
}

// ─── IM 集成（外部 IM Gateway，docs/IM_INTEGRATION.md）───

export interface IMFieldOption {
  value: string
  label: string
}

export interface IMField {
  key: string
  label: string
  type: 'string' | 'password' | 'select' | 'bool' | 'textarea' | 'number' | 'json'
  required?: boolean
  placeholder?: string
  help?: string
  options?: IMFieldOption[]
  default?: unknown
  secret?: boolean
  advanced?: boolean
}

export interface IMFieldGroup {
  title: string
  fields: IMField[]
}

export interface IMGatewaySchema {
  type: string
  name: string
  description?: string
  groups?: IMFieldGroup[]
  required_fields?: string[]
}

export interface IMChat {
  gateway: string
  chat_id: string
  thread_id?: string
}

export interface IMGatewayCfg {
  id: string
  type: string
  enabled: boolean
  config: Record<string, unknown>
  // 渠道级：路由（Chat → workspace/agent/group）与授权名单
  routes?: IMRouteCfg[]
  allow_users?: string[]
}

// IMRouteCfg 路由：Chat → 投递目标，**三选一**（与 bridge im.RouteConfig 同构）。
//
//	workspace 非空          → workspace:<path>，驱动该项目的会话（原有语义）
//	agent 非空（group 空）  → agent:<id>，驱动该智能体的 DM
//	group + agent 非空      → group:<id>，驱动该群会话（agent 是该群的执行者）
//
// owner 一般留空（由目标派生）；显式填写时必须与目标一致（bridge Validate 校验）。
// 前端**不得**把 agent/group 字段丢掉再写回——那会把消息静默改投到项目会话（B2）。
export interface IMRouteCfg {
  chat: IMChat
  workspace?: string
  agent?: string
  group?: string
  owner?: string
}

export interface IMMirrorCfg {
  session_id: string
  chat: IMChat
  mode: 'none' | 'push' | 'bidirectional'
}

export interface IMSecurityCfg {
  require_bind_confirm?: boolean
}

export interface IMConfig {
  enabled: boolean
  gateways: IMGatewayCfg[]
  mirrors: IMMirrorCfg[]
  security: IMSecurityCfg
}

export interface IMGatewayStatus {
  id: string
  type: string
  enabled: boolean
  status: 'disconnected' | 'connecting' | 'connected' | 'error' | 'not_started'
}

// IM 绑定确认请求（im_bind_confirm_requested 事件 → 桌面端弹窗）
export interface IMBindRequest {
  chat: IMChat
  userId: string
  text: string
}

// IM 群信息（im_chat_list；路由绑定下拉）
export interface IMChatInfo {
  chat_id: string
  name: string
}

// —— Surface 插件索引（PR-14 B）——
//
// 与主进程 electron/surface-index.ts 的 SurfaceIndexItem 同构（两处都得写：preload
// 不能 import 渲染层代码，而渲染层不能 import 主进程模块）。字段名即磁盘协议
// （bridge 的 index.json + 每个 <id>/surface.json），**不是**驼峰改写 —— 少一层翻译，
// 少一处会写错的地方。
/**
 * manifest.entries[] 的一项（D-2：入口由 manifest 声明，宿主渲染）。
 *
 * 与主进程 electron/surface-index.ts 同构。渲染层把它翻成菜单项/设置页的「打开」，
 * 插件**不能**自己注入菜单（那正是 D-2 否掉的做法）。
 */
/**
 * entries[].title 的两种写法（R16b）：单字符串（老）或 `{ zh, en }` 对象。
 *
 * 与主进程 electron/surface-index.ts 的同名类型同构（两处都得写：preload 不能 import
 * 渲染层代码，渲染层不能 import 主进程模块）。**原样**传到这里，选语言在渲染点
 * （surfaces/plugin/entries.ts 的 entryTitleText + useLang）。
 */
export type SurfaceEntryTitle = string | { zh?: string; en?: string }

export interface SurfaceEntryDecl {
  id: string
  title: SurfaceEntryTitle
  /** `<id>` 目录内的相对路径（缺省 = 没有图标，用 kind 图标） */
  icon?: string
  /** 排序权重（缺省 100，升序） */
  order?: number
  /** URL 型插件的页面路径（相对 `web.url`）；ESM 面板不读它 */
  path?: string
}

/** manifest.web.start：宿主替插件拉起前端服务（与 electron/surface-index.ts 同构）。 */
export interface SurfaceWebStartDecl {
  command: string
  args?: string[]
  cwd?: string
  env?: Record<string, string>
  health?: string
  timeoutMs?: number
}

export interface SurfaceIndexItem {
  id: string
  version: string
  /** sha256(入口文件)；loader 用它拼 `?v=<hash>` 做缓存键 */
  hash: string
  enabled: boolean
  /** 必须等于 `plugin:<id>`（loader 校验；不符即拒绝注册） */
  kind?: string
  /** manifest.name 的显示名（zh → en → 任意非空）；零入口时默认入口的标题取它 */
  name?: string
  /** manifest.entries[]（空数组 = 没声明，宿主给一个默认入口） */
  entries: SurfaceEntryDecl[]
  /** 声明的宿主命令（白名单的一半；另一半在 bridge） */
  commands: string[]
  /** 声明的事件 type（host.onEvent 的闸门；含 agent.* 与本地 surfaces_changed） */
  data: string[]
  /** `<id>` 目录内的相对路径（缺省 panel.js；可改名，如 dist/panel.js） */
  entry: string
  /** `<id>` 目录内的相对路径（如 icon.svg） */
  icon?: string
  /**
   * URL 型插件（2026-11）：manifest.web 存在时没有 panel.js，宿主用原生视图打开
   * `web.url + entries[].path`；页面通过 preload 注入的 `window.haiHost` 使用宿主能力。
   */
  web?: { url: string; start?: SurfaceWebStartDecl; permissions?: string[] }
  /** manifest 读不出来时的原因（loader 跳过这一条，其余照常） */
  error?: string
}

export interface SurfaceIndex {
  version: number
  items: SurfaceIndexItem[]
}

// —— URL 型插件的原生视图桥（2026-11）——
//
// 页面在独立 WebContentsView 里，拿不到宿主 window；这三条类型是「页面 ↔ 主进程 ↔
// 渲染层 host」的 wire 形状。字段名即协议，改之前先读 plugin-app.ts / plugin-app-preload.ts。

/** 一条原生视图的加载状态（宿主据此画 loading/error，而不是让 Chromium 错误页裸奔）。 */
export interface SurfaceAppStatus {
  /** `${pluginId}:${tabId}`（与 tab 一一对应；关闭 tab 即关视图） */
  key: string
  status: 'loading' | 'starting' | 'ready' | 'error'
  url?: string
  error?: string
  /** Chromium 的 did-fail-load errorCode（-3 = ABORTED，不算错误） */
  code?: number
  /** 服务启动失败时的最近日志（宿主错误态直接显示，省得用户去翻控制台） */
  logs?: string
}

/** 渲染层 → 主进程：为某个 tab 建/复用一条原生视图。 */
export interface SurfaceAppOpenInput {
  key: string
  pluginId: string
  url: string
  /** manifest.web.url 的根（服务探活/启动用）；缺省 = url（老调用） */
  webUrl?: string
  /** manifest.data：页面允许订阅的事件（preload 第一道闸门） */
  events: string[]
  /** manifest.commands：页面允许调用的命令（main / 渲染层各再挡一次） */
  commands: string[]
  /** manifest.web.start：有它则先确保服务就绪再 loadURL（缺省 = 假设服务已在跑） */
  start?: SurfaceWebStartDecl
  /** manifest.web.permissions：目前只支持 media（麦克风/摄像头） */
  permissions?: string[]
}

/** 主进程 → 渲染层：页面调用 host.command，等渲染层用 pluginAppHostReply 回。 */
export interface SurfaceHostRequest {
  /** 单调递增的请求 id（主进程分配；渲染层原样带回） */
  id: number
  pluginId: string
  name: string
  payload?: unknown
}

/** host.command 的返回（**永不抛**：拒绝是一个值，见 surfaces/plugin/loader.ts） */
export interface SurfaceCommandReply {
  ok: boolean
  /** 固定集合：denied / unknown_id / timeout / error（渲染层预检也是 denied） */
  reason?: string
  data?: unknown
}

/**
 * 插件事件信封（09-agent-bridge §5.2）：`agent.*` / `tool.call` / 本地 ui op 共用。
 *
 * `seq` 每个 run 单调递增、终态（agent.done / agent.error）之后停投；run_id / session_id
 * 只在 agent.* 事件上出现（本地 op 没有 run 归属）。
 */
export interface SurfaceEvent {
  type: string
  seq?: number
  run_id?: string
  session_id?: string
  data?: unknown
}

/**
 * 插件 Agent 事件的信封（P1b2，§6.6）：main 把 bridge 的 `surface_agent_event.data`
 * **原样**转发到 `surface:agent-event`，所以形状就是这两个键。
 */
export interface SurfaceAgentEventPayload {
  surface_id: string
  event: SurfaceEvent
}

/**
 * 插件页（PR-14b）三条写/读操作的回执。
 *
 * reason 是封闭集合（unknown_id / bad_request / error，外加不可信来源的 denied）：
 * 页面把它映射成一句人话，**不解析桥的中文 error**——文案改了不该让 UI 跟着漂。
 * status 透传 store 的回执状态（applied / cleaned），页面用它区分「卸掉了」与「本来就干净」。
 */
export interface SurfaceActionResult {
  ok: boolean
  reason?: string
  id?: string
  enabled?: boolean
  status?: string
}

/** audit.jsonl 的一行（与 bridge 的 auditLine 同构：字段名即磁盘协议） */
export interface SurfaceAuditLine {
  id: string
  /** install / remove / enable / disable / command */
  action: string
  /** action=command 时被调的命令名 */
  cmd?: string
  ok: boolean
  /** RFC3339Nano（UTC） */
  ts: string
  /** 拒绝原因（成功行没有这个字段） */
  reason?: string
}

/** surface_audit 的回执：lines **恒出现**（空态是 []，不是字段消失） */
export interface SurfaceAuditReply {
  ok: boolean
  reason?: string
  lines: SurfaceAuditLine[]
}

/**
 * hub_group_edit（P7-01/I3）的 wire payload：群名/模式/群主是**字段级 patch**（不传 =
 * 不改），成员增删一次提交。bridge 先全量校验再单次落盘，任一失败群逐字段不变 ——
 * 前端不要退化成多条命令的序列（那正是 I3 修掉的半改状态）。
 */
export interface HubGroupEditPayload {
  group_id: string
  name?: string
  mode?: string
  /** 新群主 agent_id（必须出现在最终成员表里；不能与 remove_members 重叠） */
  owner?: string
  add_members?: string[]
  remove_members?: string[]
}
