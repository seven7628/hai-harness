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
  api_key?: string // API key 明文（settings.json；bridge loadSettings 读取）
  models: Record<string, number> // model → context window（tokens；1M=1000000）
  max_tokens: Record<string, number> // model → 单次输出上限（tokens；DeepSeek v4-flash 官方 384k）
  // 每模型用户价表覆盖（USD/1M tokens；缺省/全 0 = 用内置注册表价表）。
  // 维度 model → 单价：同模型名在不同 provider 下费率不同（deepseek-v4-flash 缓存价
  // DeepSeek 直连 $0.014/M vs OpenCode Go $0.0028/M），故价表挂 provider 段。
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
// 单台 MCP 服务器配置（与 bridge mcp.ServerConfig 同构；env 明文存 settings.json——v1 决策）
export interface MCPServerCfg {
  type?: 'stdio' | 'http' | 'sse'
  command?: string
  args?: string[]
  env?: Record<string, string>
  url?: string
  enabled?: boolean
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
}
export interface RuntimeSettings {
  stop_background_on_interrupt?: boolean // 主停止是否连后台任务一起停（缺省 false：只停主会话）
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
  api_key?: string // API key（明文存 settings.json；与 provider 同策略）
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
  // 会话页样式（可选的纯样式层开关）：current（默认，现状）| v3（组件样式）。
  // 缺省 = current，老 settings.json 没有该字段时行为不变。
  narrative_skin?: 'current' | 'v3'
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
  mesh?: MeshSettings // Session Mesh 会话间通信（缺省全关：开启后才注册 session_* 工具）
  agent?: AgentSettings // Agent 运行时设置（提醒阈值 + 子 agent 并发槽上限）
  runtime?: RuntimeSettings // 运行期行为开关（S3-B 主停止后台任务语义）
  stt?: STTSettings // 语音输入（话筒）配置；配置齐全才显示话筒按钮
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
  api_key?: string
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
  externalSkillsStatus(): Promise<ExternalSkillsStatus>
  importExternalSkills(): Promise<{ ok: boolean; result?: ExternalSkillsImportResult; error?: string }>
  pickApp(): Promise<{ name: string; bundleId?: string } | null> // 系统「选择应用程序」（osascript choose application）；取消 → null
  providerSave(p: ProviderSaveInput): Promise<{ ok: boolean; hasKey?: boolean; error?: string }>
  providerRemove(provider: string): Promise<{ ok: boolean; error?: string }> // 删除自定义 provider（含 key）
  providerKeyStatus(provider: string): Promise<boolean>
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
  watchXlsx(req: { path: string; workspace: string }): Promise<{ ok: boolean; html_path?: string; error?: string }>
  /**
   * docx 文档 / pptx 演示文稿预览：与 watchXlsx 是**同一条通路**（同形态入参、同样返回
   * HTML 产物路径、同样送内嵌浏览器 file://），桥接层只换命令名（docx_preview /
   * pptx_preview）。拆成三个方法而不是一个带 kind 的方法，是为了与 store 的三个动作
   * （previewXlsx/previewDocx/previewPptx）一一对应；浏览器 mock 三个都返回失败。
   */
  watchDocx(req: { path: string; workspace: string }): Promise<{ ok: boolean; html_path?: string; error?: string }>
  watchPptx(req: { path: string; workspace: string }): Promise<{ ok: boolean; html_path?: string; error?: string }>
  // 语音转写：把录音音频（wav 字节）发到 OpenAI 兼容 /audio/transcriptions，返回文本。
  // 由主进程转发（net.fetch）——避开渲染层 CORS 限制、API key 不进网页层。
  sttTranscribe(audio: ArrayBuffer, mimeType: string): Promise<{ ok: boolean; text?: string; error?: string }>
  send(cmd: BridgeCommand): Promise<{ ok: boolean; error?: string }>
  onEvent(cb: (line: string) => void): () => void
  onExit(cb: (info: BridgeExitInfo) => void): () => void
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
  // 渠道级：路由（Chat → workspace）与授权名单
  routes?: IMRouteCfg[]
  allow_users?: string[]
}

export interface IMRouteCfg {
  chat: IMChat
  workspace: string
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
