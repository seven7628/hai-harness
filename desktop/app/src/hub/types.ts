// Agent Hub 前端类型（与 bridge 的 hub_* 命令响应一一对应）。
//
// 只描述「UI 用到的那部分」：profile 在 Go 侧字段很多（工具策略/权限/预算），
// 资料页用到的字段单独写，避免前端复制一份领域模型后与后端漂移。

export interface HubAvatar {
  kind?: string // emoji | image
  value?: string // emoji
  bg?: string
  path?: string
}

export interface HubAgent {
  id: string
  name: string
  avatar?: HubAvatar
  accent?: string
  role?: string
  description?: string
  instructions?: string
  persona?: string
  provider?: string
  model?: string
  effort?: string
  compress_threshold?: number
  policy?: { daily_budget_usd?: number; max_task_cost_usd?: number }
  memory?: { enabled?: boolean; max_index_lines?: number }
  tools?: { preset?: string; disable?: string[]; allow?: string[]; bash_roots?: string[]; disable_network?: boolean }
  version?: number
  deleted?: boolean
}

/** hub_agent_list 的 dm 投影：预览/未读（不是 profile 的一部分）。 */
export interface HubDMInfo {
  unread?: number
  last_seq?: number
  last_text?: string
  last_sender?: string
  last_at?: string
}

export interface HubSender {
  kind?: string // human | agent | system
  agent_id?: string
}

/**
 * 一条消息附件（Hub DM / 群共用；与 bridge 的 HubAttachment 一一对应）。
 *
 * 图片只走 data URL（前端压缩后），**不把任意文件内容读进来**；文件只做 `@path`
 * 引用 —— 发送时由前端把 `@<path>` 拼进正文，后端不读文件。
 */
export interface HubAttachment {
  kind: 'image' | 'file'
  name?: string
  mime_type?: string
  bytes?: number
  /** 仅 kind=image：压缩后的 data URL（后端直接喂视觉模型）。 */
  data_url?: string
  /** 仅 kind=file：引用路径（绝对路径优先）。 */
  path?: string
}

export interface HubMessage {
  seq: number
  id?: string
  kind?: string
  sender?: HubSender
  text?: string
  /** 旧消息没有这个字段（undefined 兼容）；只有人类发送时才带上。 */
  attachments?: HubAttachment[]
  created_at?: string
  round_id?: string
  context?: { trigger_seqs?: number[]; senders?: Array<{ agent_id?: string; name?: string }>; reason?: string }
  visibility?: string
  refs?: Record<string, unknown>
  /** 前端乐观消息：等待 hub_message 回执（含 client_msg_id）后替换/移除。 */
  pending?: boolean
  /** 发送失败（可重试）：气泡保留在列表里，原文同时进 store.failedText。 */
  failed?: boolean
  clientMsgId?: string
}

/** hub_agent_prompt_preview 的一层系统提示词（L1–L4；L5 由内核在请求期装配）。 */
export interface HubPromptLayer {
  key?: string
  title?: string
  text?: string
}

/** hub_agent_prompt_preview 响应（Go：hubPromptPreview）。 */
export interface HubPromptPreviewData {
  agent_id?: string
  /** 'dm' | 'group'：后端按是否收到 group_id 决定。前端不据此建缓存（见 promptScope.ts）。 */
  scope?: string
  /** 回显的群 id；DM 时为空串。旧 mock/旧 bridge 可能没有这两个字段。 */
  group_id?: string
  layers?: HubPromptLayer[]
  total_bytes?: number
  note?: string
}

export interface HubPage {
  agent?: HubAgent
  session_id?: string
  messages?: HubMessage[]
  has_more?: boolean
  cursor?: number
  exists?: boolean
}

export interface HubHistoryPage {
  messages?: HubMessage[]
  has_more?: boolean
}

/** hub_search 的 scope（P4-04b-1）。 */
export type HubSearchScope = 'all' | 'group' | 'dm'

/**
 * 翻页游标。
 *
 * `conversations` 是 scope=all 的**逐会话续页游标**（key `g:<gid>` / `d:<agent_id>`
 * → 下一次该会话的 before_seq）；必须原样回传，丢掉它就会退回第一页（B1）。
 * `created_at`/`id`/`seq` 只用于展示与 scope=group/dm 的快速路径。
 */
export interface HubSearchCursor {
  created_at?: string
  id?: string
  seq?: number
  conversations?: Record<string, number>
}

/** 一条搜索命中（hub_search.hits[i]；字段与 bridge 的 hubSearchHit 对齐）。 */
export interface HubSearchHit {
  scope: 'group' | 'dm'
  group_id?: string
  agent_id?: string
  name?: string
  seq: number
  message_id?: string
  /** bridge 侧按 rune 截断、以命中词为中心的片段。 */
  snippet?: string
  /** 已还原的发送者显示名（我 / 智能体名 / 系统）。 */
  sender?: string
  sender_kind?: string
  sender_agent_id?: string
  kind?: string
  created_at?: string
  /** 仅群主可见：UI 必须打标，不能无声混在普通结果里。 */
  owner_only?: boolean
}

/** hub_search 响应。 */
export interface HubSearchPage {
  query?: string
  scope?: string
  limit?: number
  hits?: HubSearchHit[]
  count?: number
  has_more?: boolean
  next_before?: HubSearchCursor | null
  /** 本次 fan-out 扫了多少个会话（1 = 单会话 scope）。 */
  scanned?: number
}

/** 任务账本条目（hub_task_list/get；字段与 agenthub.Task 对齐，只列 UI 用的）。 */
export interface HubTaskUsage {
  input?: number
  output?: number
  cache_read?: number
  cache_write?: number
  reasoning?: number
  cost_priced?: boolean
  cost_usd?: number
}

export interface HubTaskArtifact {
  name?: string
  path?: string
  kind?: string
  size?: number
  lines?: number
}

export interface HubTaskResult {
  summary?: string
  error?: string
  finish_reason?: string
  usage?: HubTaskUsage
  artifacts?: HubTaskArtifact[]
}

/** hub_task_artifact_preview 的响应数据（path 是展示用相对路径，不回绝对路径）。 */
export interface HubArtifactPreviewData {
  path: string
  name: string
  size: number
  lines: number
  truncated: boolean
  binary: boolean
  content?: string
  data_url?: string
  mime?: string
}

export interface HubAlert {
  id: string
  scope?: string // hub | group | agent | task
  code?: string
  kind?: string
  severity?: string // info | warning | error
  task_id?: string
  agent_id?: string
  group_id?: string
  message?: string
  at?: string
}

export interface HubTask {
  id: string
  root_task_id?: string
  parent_task_id?: string
  title?: string
  instruction?: string
  status?: string
  attempt?: number
  error?: string
  origin?: { kind?: string; group_id?: string; agent_id?: string; agent_name?: string; message_seq?: number }
  target?: {
    kind?: string // workspace | agent
    workspace?: string
    session_id?: string
    /** target.kind=agent 时的执行者（人工派任务靠它定位任务卡）。 */
    agent_id?: string
    group_id?: string
  }
  /** 派发时间线（账本 Dispatch）：run_id = 这条任务在目标会话里跑起来的**主 run**。 */
  dispatch?: {
    run_id?: string
    dispatched_at?: string
    accepted_at?: string
    agent_start_at?: string
    agent_end_at?: string
  }
  result?: HubTaskResult
  created_at?: string
  updated_at?: string
}

/** hub_task_update 的扁平字段（§7.2 / §7.3.3）：agent_id = 发起者，target_agent_id = 执行者。 */
export interface HubTaskUpdate {
  task_id?: string
  status?: string
  target_kind?: string
  group_id?: string
  agent_id?: string
  target_agent_id?: string
  title?: string
  error?: string
  task?: HubTask
}

/** Needs you（审批/提问/失败/交接/预算）。 */
export interface HubNeed {
  id: string
  kind?: string
  status?: string
  agent_id?: string
  group_id?: string
  task_id?: string
  session_id?: string
  workspace?: string
  origin_agent_id?: string
  origin_agent_name?: string
  origin_kind?: string
  target_kind?: string
  target_workspace?: string
  target_session_id?: string
  run_id?: string
  run_name?: string
  depth?: number
  anchor?: string
  title?: string
  body?: string
  created_at?: string
  resolved_at?: string
}

/** Run 视图的一行（工具起止 / 压缩 / 状态）；来自 agent_* 与会话事件。 */
export interface HubRunEntry {
  id: string
  kind: 'tool' | 'status' | 'compress'
  name?: string
  status?: string
  summary?: string
  ms?: number
  at: number
}

/** 群组成员/策略/群头（hub_group_list/get）。 */
export interface HubGroupMember {
  agent_id: string
  role?: string
  joined_at?: string
}

export interface HubGroupPolicy {
  mode?: string
  max_handoff_depth?: number
  max_turns_per_human?: number
  max_mentions_per_message?: number
  daily_budget_usd?: number
}

export interface HubGroup {
  id: string
  name: string
  owner_agent_id?: string
  members?: HubGroupMember[]
  policy?: HubGroupPolicy
  pinned?: boolean
  deleted?: boolean
  created_at?: string
  updated_at?: string
}

export interface HubRound {
  id: string
  group_id?: string
  trigger_seq?: number
  targets?: string[]
  mention_all?: boolean
  dropped?: number
  unknown?: string[]
}

export interface HubGroupActivity {
  group_id: string
  agent_id?: string
  run_id?: string
  after_seq: number
  kind: string
  name?: string
  status?: string
  summary?: string
  ms?: number
  at?: string
}

/**
 * hub_group_list 的 progress[group_id]（Go agenthub.GroupPerception 的 JSON 投影）。
 * 左栏预览优先 last_outcome（任务完成/失败…），否则 last_text。
 */
export interface HubGroupProgress {
  unread?: number
  last_seq?: number
  last_sender?: string
  last_at?: string
  last_text?: string
  last_outcome?: string
  last_task_id?: string
  last_task_status?: string
  last_task_summary?: string
  /**
   * 前端投影：任务卡所在消息的 seq。0 = 最新可用消息不是任务卡；
   * 缺省 = 尚未从事件里标记（刷新得到的 progress 只有 last_outcome 可判）。
   * 用来挡住迟到的 hub_task_update 把旧结论写回 last_outcome（N2）。
   */
  last_task_seq?: number
}

/** hub_conversation 事件/回执里的对话视图（Go agenthub.ConversationView）。 */
export interface HubConversation {
  id?: string
  group_id?: string
  /** idle | running | waiting_human | stopped | exhausted */
  status?: string
  human_seq?: number
  turns_used?: number
  turn_budget?: number
  depth?: number
  max_depth?: number
  queue_len?: number
  pair_limit?: number
  /** 键是**无序** `a↔b`（Go 的 ConversationPairKey，P6-04）。 */
  pair_count?: Record<string, number>
  /** pair 计数的滚动窗口（毫秒，默认 10 分钟）。 */
  pair_window_ms?: number
  /** 最热那一对的最早过期时刻（「约 N 分钟后自动恢复」的数据源）。 */
  pair_reset_at?: string
  /** 空闲重置阈值（毫秒，默认 30 分钟）。 */
  idle_reset_ms?: number
  /** 单轮 run 上限（毫秒，默认 10 分钟）。 */
  run_timeout_ms?: number
  /** 待投递队列上限（默认 200）。 */
  max_pending_wakes?: number
  cooldown_ms?: number
  last_wake_at?: string
  stop_reason?: string
  started_at?: string
  updated_at?: string
  /** 最近一轮的投递账本（P6-01）：每个 target 一条，见 HubDelivery。 */
  deliveries?: HubDelivery[]
}

/**
 * 投递账本的单目标状态（P6-01，Go 的 agenthub.Delivery）。
 * queued：排队等目标空闲；running：已交给目标会话；done：这一轮收尾；
 * skipped：护栏/停止/换代有意不跑（带 reason）；failed：投递出错（带 reason）。
 */
export interface HubDelivery {
  wake_id?: string
  conversation_id?: string
  group_id?: string
  target?: string
  sender?: string
  human?: boolean
  depth?: number
  turn?: number
  trigger_seq?: number
  round_id?: string
  status?: 'queued' | 'running' | 'done' | 'skipped' | 'failed' | string
  /** 原因码：target_busy / rate_limit / pair_limit / depth_limit / budget / queue_full / stale_conversation / ... */
  reason?: string
  /** 同一件事的人话说明（含「跟谁、几次、窗口多长」）。 */
  note?: string
  attempts?: number
  ready_at?: string
  enqueued_at?: string
  updated_at?: string
}
