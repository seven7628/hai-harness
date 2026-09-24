// checkpoint.ts —— bridge ViewReducer 产出的 ViewCheckpoint 的 renderer 侧类型。
//
// 与 desktop/bridge/view_reducer.go 的 Go/JSON schema 一一对应（字段名 = JSON tag）。
// 这是版本化后端投影，不是 zustand 对象；hydrate 时映射为 SessionView。
// 瞬时交互态（approvals/question/release/pendingQueue/streaming 句柄）不在此 schema 中。

export interface CheckpointUsage {
  input: number
  output: number
  // 桥侧 wire 形态是蛇形 cache_read（bridge/view_reducer.go Usage 的 json tag）——
  // 这里曾误写成 camelCase cacheRead，导致 hydrate 读不到缓存命中量（重启/恢复后
  // 会话累计 cacheRead 归零 → 状态栏「缓存」掉到个位数百分比）。保留 camelCase
  // 可选字段，兼容旧数据/手写 payload。
  cache_read?: number
  cacheRead?: number
  // 写入缓存量（Anthropic cache_creation_input_tokens；bridge Usage.cache_write）。
  // 与命中量并列：命中率回答「命中了多少」，写入量回答「这轮是不是在整段重写缓存」。
  cache_write?: number
  cacheWrite?: number
  // 1h 档写入量（⊆ cache_write，Anthropic extended TTL 专用；bridge Usage.cache_write_1h）。
  // 状态栏缓存 tooltip 据此区分 5m/1h 两种缓存规模（2026-09-23 补齐维度）。
  cache_write_1h?: number
  cacheWrite1h?: number
  reasoning: number
  // cost_usd 卡片级成本（USD）：仅任务卡（agent / async_task / promoted tool）填充，
  // 由 bridge view_reducer 在 TaskEnd / task_result_delivered 时按任务全量写入。
  // 会话级总成本是顶层 cost_usd 字段（两处独立记账，不叠加）。
  cost_usd?: number
  costUsd?: number
}

export interface CheckpointTodo {
  id: string
  title: string
  done: boolean
  blockedBy?: string[]
}

export interface CheckpointDiff {
  path: string
  added: number
  removed: number
  unified?: string
  truncated?: boolean
}

export interface CheckpointContent {
  type: string // text | image
  content: string
  mime_type?: string
}

export interface CheckpointToolError {
  code?: string
  changed?: string
  retryable?: boolean
  nextAction?: string
  recovery?: string
}

export interface CheckpointAgentItem {
  kind: 'text' | 'thinking' | 'tool' | 'compression'
  text?: string
  is_error?: boolean
  think_start?: number
  tool_id?: string
  name?: string
  args?: string
  status?: string // tool item: running|done|error
  result?: string
  error_info?: CheckpointToolError
  dur_ms?: number
  started_at?: number
  task_id?: string
  diff?: CheckpointDiff
  images?: CheckpointContent[] // tool item: 结果图片
  before?: number
  after?: number
  ctx_tokens?: number
  reason?: string
  model?: string
  summary?: string
  usage?: CheckpointUsage
  error?: string
  aborted?: boolean
  active?: boolean
  analysis?: string // 压缩器 <analysis> 块留档（仅观测；compression item）
}

// CheckpointBlock 与 Go Block 平铺对应（可选字段按 kind 解释）。
export interface CheckpointBlock {
  kind: string
  id: number
  divider_reason?: string // divider: shutdown|abort|clear（Go Block.DividerReason）
  text?: string
  thinking?: string
  contents?: CheckpointContent[]
  images?: CheckpointContent[] // tool 结果图片（read_file 读图 / 截图）
  model?: string
  run_id?: string
  request_id?: string
  ts?: number
  tone?: string
  // error 块的重试进度（llm_error，2026-09-18）：刷新/重放后 RetryProgress 仍能算
  // 倒计时与「本次尝试已进行 Xs」（下一次尝试起点 = ts + retry_delay_ms）。
  retrying?: boolean
  retry_attempt?: number
  retry_max_attempts?: number
  retry_delay_ms?: number
  retry_started_at?: number // 本轮重试首个失败时刻（累计「已等待时长」的锚点）
  retry_cost_usd?: number // 本轮重试已花费（USD，2026-09-23）：失败尝试的已产生用量折算，跨次累加
  think_ms?: number
  ttft_ms?: number
  dur_ms?: number
  agent_dur_ms?: number
  // 输出速度（tokens/s）的分子：本轮输出 token（含 reasoning）与其中 reasoning 部分。
  // 落库保证刷新/重放后消息底部的 tokens/s 不消失（分母由 genMsOf 纯函数重算）。
  output_tokens?: number
  reasoning_tokens?: number
  // thinking_summarized：本轮思考是摘要形态（Anthropic display=summarized）——tokens/s
  // 的分母口径必须在刷新后保持一致（genMsOf 纯函数按它重算）。
  thinking_summarized?: boolean
  tool_id?: string
  name?: string
  args?: string
  result?: string
  is_error?: boolean
  error_info?: CheckpointToolError
  status?: string
  diff?: CheckpointDiff
  task_id?: string
  promoted?: boolean
  task_status?: string
  todos?: CheckpointTodo[]
  started_at?: number
  label?: string
  spawned_at?: number
  ended_at?: number
  usage?: CheckpointUsage
  items?: CheckpointAgentItem[]
  task_result?: string
  task_error?: string
  delivered_to_main?: boolean
  before?: number
  after?: number
  reason?: string
  summary?: string
  ctx_tokens?: number
  error?: string
  aborted?: boolean
  warnings?: string[]
  analysis?: string // 压缩器 <analysis> 块留档（仅观测；compression block）
  // artifacts 块（AgentEnd.Artifacts 投影）：本轮产出文件清单。
  // 对齐 Go view_reducer.go Block.Artifacts / ArtifactRef。
  artifacts?: CheckpointArtifact[]
  artifacts_truncated?: boolean
}

// CheckpointArtifact 单个产出文件（对齐 Go ArtifactRef / core.Artifact）。
export interface CheckpointArtifact {
  name: string // 文件名（basename）
  path: string // 展示路径：工作区内相对 / 区外绝对
  kind: string // created | modified
  added?: number
  removed?: number
  size?: number
  lines?: number
  sha256?: string // 内容指纹（前 16 字节 hex）；用于同内容去重
}

export interface CheckpointRunNode {
  id: string
  parent_id?: string
  label: string
  status: string // running|done|error|interrupted
  depth: number
  kind: string // run|tool
  name?: string
  args?: string
  result?: string
  is_error?: boolean
  started_at?: number
  dur_ms?: number
  task_id?: string
  promoted?: boolean
}

export interface CheckpointTurn {
  model: string
  input: number
  output: number
  cache_read: number
  cache_write?: number
  reasoning: number
  dur_ms?: number
  think_ms?: number
  ttft_ms?: number
  cost_usd: number
  run_id?: string
  request_id?: string
  finish_reason?: string
}

export interface CheckpointLastRun {
  run_id?: string
  status: string // done|error|interrupted
  finish_reason?: string
  task_id?: string
  ended_at?: number
}

export interface CheckpointLastError {
  message: string
  kind?: string
  at?: number
}

export interface CheckpointDiagnostic {
  event_type: string
  count: number
  first_seq?: number
  last_seq?: number
}

// CheckpointView = ViewReducer.Snapshot() 的 JSON 投影（schema_version=1）。
export interface CheckpointView {
  schema_version: number
  blocks: CheckpointBlock[]
  run_nodes: CheckpointRunNode[]
  turns: CheckpointTurn[]
  usage: CheckpointUsage
  cost_usd: number
  lifetime_usage?: CheckpointUsage
  lifetime_cost_usd?: number
  todos?: CheckpointTodo[]
  ctx_tokens?: number
  // ctx_tokens_estimated：ctx_tokens 的口径标记（true = 压后字符估算，非 provider 真实
  // usage）。旧记录缺该字段 → undefined，按真实口径渲染（bridge CtxTokensEstimated）。
  ctx_tokens_estimated?: boolean
  context_window?: number
  diffs?: CheckpointDiff[]
  last_run?: CheckpointLastRun
  last_error?: CheckpointLastError
  degraded: boolean
  diagnostics?: CheckpointDiagnostic[]
}

// CheckpointResponseData 是 new_session 响应中 restored="checkpoint" 分支的 data 字段。
export interface CheckpointResponseData {
  restored: 'checkpoint'
  checkpoint_version: number
  revision?: number
  reducer_seq?: number
  clear_generation?: number
  degraded: boolean
  view_checkpoint: CheckpointView
}