import type { HubMessage, HubTask, HubTaskArtifact, HubTaskUsage } from './types'

// 群内任务结果卡（P3b-07，方案 §7.5.5 / §8.3.5）。
//
// bridge 写 task_result 群消息时把结果**反规范化**进 refs（hub_task_result.go）：
//   {client_msg_id, task_id, status, agent_id, title, duration_ms, usage, artifacts, error}
//   + 归属：target_kind（agent|workspace）、target_workspace/target_session_id、
//     origin_kind/origin_agent_id/origin_agent_name
// 前端只认这份快照，不去查账本 —— 历史消息在任务被重试/账本轮转后仍能原样渲染。
// 纯函数，Node 直测。

export type TaskTone = 'muted' | 'info' | 'success' | 'warn' | 'error'

export interface TaskStatusView {
  label: string
  tone: TaskTone
  terminal: boolean
}

export interface TaskCardView {
  taskId: string
  status: string
  title: string
  summary: string
  error: string
  /** agent | workspace | ''（refs 快照优先，账本兜底）。workspace = 项目会话任务。 */
  targetKind: string
  /** workspace 目标的 Project 工作区与/或会话（agent 目标为空）。 */
  targetWorkspace: string
  targetSessionId: string
  /** 执行者（target_agent_id 优先；task_result 的 agent_id 就是执行者）。 */
  agentId: string
  /**
   * 派发者：来自账本 `task.origin`（缺失时回退 refs 快照），**不是**
   * task_result 的 sender（bridge 写的 sender 就是执行者本人）。
   */
  initiatorAgentId: string
  /** 派发者显示名（账本 origin.agent_name 或 refs 快照）；任务未加载时可为空。 */
  initiatorName: string
  /**
   * 发起者显示名（结果卡「<发起者> 发起」这一行）：账本/refs 的 origin 名，缺快照时
   * 退到 sender —— bridge 约定 workspace 卡的 sender 就是发起者（hub_task_result.go）。
   * 人发起为空串（卡片写「你 发起」）。
   */
  originName: string
  /** human | group | dm | agent | ''（未知/账本未加载）。 */
  originKind: string
  durationMs: number
  usage: HubTaskUsage | null
  artifacts: HubTaskArtifact[]
  statusLabel: string
  tone: TaskTone
  terminal: boolean
}

const TERMINAL = new Set([
  'completed', 'failed', 'interrupted', 'canceled', 'declined', 'blocked_budget', 'delivered',
])

const STATUS_VIEW: Record<string, TaskStatusView> = {
  queued: { label: '排队中（目标忙）', tone: 'muted', terminal: false },
  dispatched: { label: '已派发，等待开始', tone: 'info', terminal: false },
  running: { label: '进行中', tone: 'info', terminal: false },
  completed: { label: '已完成', tone: 'success', terminal: true },
  delivered: { label: '已回推', tone: 'success', terminal: true },
  failed: { label: '失败', tone: 'error', terminal: true },
  interrupted: { label: '被中断（人/重启/超时）', tone: 'warn', terminal: true },
  canceled: { label: '已取消', tone: 'muted', terminal: true },
  declined: { label: '已被拒绝', tone: 'warn', terminal: true },
  blocked_budget: { label: '预算/护栏阻断', tone: 'warn', terminal: true },
  unknown: { label: '未知状态', tone: 'muted', terminal: false },
}

export function taskStatusView(status?: string): TaskStatusView {
  const key = (status ?? '').trim()
  const known = STATUS_VIEW[key]
  if (known) return known
  return { label: key || '未知状态', tone: 'muted', terminal: false }
}

export function isTerminalTaskStatus(status?: string): boolean {
  return TERMINAL.has((status ?? '').trim())
}

function str(v: unknown): string {
  return typeof v === 'string' ? v.trim() : typeof v === 'number' ? String(v) : ''
}

function num(v: unknown): number {
  const n = typeof v === 'number' ? v : Number(v)
  return Number.isFinite(n) ? n : 0
}

function asUsage(v: unknown): HubTaskUsage | null {
  if (!v || typeof v !== 'object') return null
  const raw = v as Record<string, unknown>
  const usage: HubTaskUsage = {
    input: num(raw.input) || undefined,
    output: num(raw.output) || undefined,
    cache_read: num(raw.cache_read) || undefined,
    cache_write: num(raw.cache_write) || undefined,
    reasoning: num(raw.reasoning) || undefined,
    cost_priced: raw.cost_priced === true,
    cost_usd: Number.isFinite(Number(raw.cost_usd)) ? Number(raw.cost_usd) : undefined,
  }
  const empty = !usage.input && !usage.output && !usage.cache_read && !usage.cache_write && !usage.reasoning && usage.cost_usd === undefined
  return empty ? null : usage
}

function asArtifacts(v: unknown): HubTaskArtifact[] {
  if (!Array.isArray(v)) return []
  const out: HubTaskArtifact[] = []
  for (const item of v) {
    if (!item || typeof item !== 'object') continue
    const raw = item as Record<string, unknown>
    const name = str(raw.name)
    const path = str(raw.path)
    if (!name && !path) continue
    out.push({
      name: name || path.split('/').pop(),
      path,
      kind: str(raw.kind) || undefined,
      size: num(raw.size) || undefined,
      lines: num(raw.lines) || undefined,
    })
  }
  return out
}

function firstLine(text: string): string {
  const line = text.trim().split('\n')[0] ?? ''
  return truncate(line, 80)
}

/**
 * kind=task_result → 稳定视图模型；其他消息返回 null（系统/普通气泡走原路径）。
 * 卡片正文是执行者摘要（没有摘要的终态是原因兜底），refs 里才是权威字段。
 *
 * task 是账本条目（hub_task_update/task_list）：派发者只看 origin，绝不从
 * msg.sender 推 —— bridge 写结果卡时 sender = 执行者。任务未加载时回退到
 * refs 里的 origin 快照（bridge 写卡时一并落账）；两者都没有才只显示执行者。
 */
export function parseTaskResult(msg: HubMessage, task?: HubTask | null): TaskCardView | null {
  if (!msg || msg.kind !== 'task_result') return null
  const refs = (msg.refs ?? {}) as Record<string, unknown>
  const text = (msg.text ?? '').trim()
  // 归属：workspace 目标 = group 来源的 Project 任务（sender 是发起者，不是执行者）。
  const targetKind = str(refs.target_kind) || str(task?.target?.kind)
  const targetWorkspace = str(refs.target_workspace) || str(task?.target?.workspace)
  const targetSessionId = str(refs.target_session_id) || str(task?.target?.session_id)
  const workspace = targetKind === 'workspace'
  // 正文抬头以下的真实摘要：正文 = 「〔Project <ws> / <sid>〕\n<摘要>」（workspaceTaskCardText）。
  const body = workspace ? stripProjectHeader(text) : text
  // 缺状态 = 未知，不能默认 completed（否则失败卡会被画成绿色成功）。
  const status = str(refs.status) || 'unknown'
  const statusView = taskStatusView(status)
  const error = str(refs.error) || str(task?.error) || str(task?.result?.error)
  const repeated = !!error && body === error // 摘要与错误同一句：只在错误行画一次
  // workspace 目标的摘要在账本里**不带**抬头（task.result.summary 是纯摘要），按普通
  // 优先级取就会把「这段结果跑在哪个 Project 会话」丢掉（B1）。所以 workspace 卡
  // 一律先取正文，再退到 refs/账本；卡片用 targetWorkspace/targetSessionId 画同一件事。
  const summary = workspace
    ? (repeated ? '' : firstNonEmpty(text, str(refs.summary), str(task?.result?.summary)))
    : str(refs.summary) || str(task?.result?.summary) || (repeated ? '' : body)
  // 标题兜底取**去掉抬头**的正文首行：summary 里带着〔Project …〕时不能把它当标题。
  const title = str(refs.title) || str(task?.title) || firstLine(body || summary) || '任务结果'
  // workspace 目标没有执行智能体（只跑在一个 Project 会话里）：硬画会把发起者当成执行者，
  // 出现「群派发A 派发」这种不存在的执行者（B1）。
  const agentId = workspace
    ? ''
    : str(refs.target_agent_id) || str(refs.agent_id) || str(task?.target?.agent_id)
  // 账本 origin.kind 是 human/group/dm（不是 sender kind）；结果卡 refs 自带一份
  // origin 快照，任务未加载（历史/重启后还没拉 task_list）也能画出「谁 → 执行者」。
  const originKind = str(task?.origin?.kind) || str(refs.origin_kind)
  const originAgentId = str(task?.origin?.agent_id) || str(refs.origin_agent_id)
  const originAgentName = str(task?.origin?.agent_name) || str(refs.origin_agent_name)
  const initiatorAgentId = originKind && originKind !== 'human' ? originAgentId : ''
  const initiatorName = originKind && originKind !== 'human' ? originAgentName : ''
  const senderAgentId = msg.sender?.kind === 'agent' ? str(msg.sender.agent_id) : ''
  // workspace 卡的 sender 就是发起者（bridge 约定）：快照缺名字时用它兜底，
  // 宁可显示 id 也不能让「谁派的」变成空白。
  const originName = initiatorName || (workspace && originKind !== 'human' ? senderAgentId : '')
  return {
    taskId: str(refs.task_id) || str(task?.id),
    status,
    title,
    summary,
    error,
    targetKind,
    targetWorkspace,
    targetSessionId,
    agentId,
    initiatorAgentId,
    initiatorName,
    originName,
    originKind,
    durationMs: Math.max(0, num(refs.duration_ms)),
    usage: asUsage(refs.usage),
    artifacts: asArtifacts(refs.artifacts),
    statusLabel: statusView.label,
    tone: statusView.tone,
    terminal: statusView.terminal,
  }
}

/** 正文抬头的字面形态（hub_task_result.go workspaceTaskCardText）：`〔Project <ws> / <sid>〕`。 */
const PROJECT_HEAD = /^〔Project [^〕\n]*〕[ \t]*\n?/

/**
 * 去掉正文开头的〔Project <ws> / <sid>〕抬头。
 * 卡片用 refs 的 target_workspace/target_session_id 画同一件事，正文里再来一遍就是重复；
 * 数据层（summary）保留抬头，因为它是不依赖账本的归属原文。
 */
export function stripProjectHeader(text: string): string {
  return text.replace(PROJECT_HEAD, '').trim()
}

function truncate(text: string, max: number): string {
  const runes = Array.from(text)
  if (runes.length <= max) return text
  return runes.slice(0, Math.max(0, max - 1)).join('') + '…'
}

function firstNonEmpty(...values: string[]): string {
  for (const v of values) {
    if (v.trim()) return v.trim()
  }
  return ''
}

/**
 * 左栏任务结论行，逐字对齐 Go agenthub.groupTaskOutcome（任务完成：… / 任务失败：…），
 * 让事件驱动出来的预览与刷新后的 `progress.last_outcome` 是同一句话。
 */
export function taskOutcomeLine(status: string, summary?: string, reason?: string, max = 80): string {
  const head = (() => {
    switch (status) {
      case 'completed':
      case 'delivered': return '任务完成：'
      case 'failed': return '任务失败：'
      case 'interrupted': return '任务已中断：'
      case 'canceled': return '任务已取消：'
      case 'declined': return '任务已被拒绝：'
      case 'blocked_budget': return '任务预算阻断：'
      default: return ''
    }
  })()
  if (!head) return ''
  // workspace 结果卡的摘要带〔Project …〕抬头（抬头本身是归属，卡片 meta 行已经画过）：
  // 左栏结论行只要摘要这一句，否则左栏会变成「任务完成：〔Project …〕…」。
  const clean = stripProjectHeader(summary ?? '')
  const preferReason = ['failed', 'interrupted', 'canceled', 'declined', 'blocked_budget'].includes(status)
  const detail = preferReason
    ? firstNonEmpty(reason ?? '', clean)
    : firstNonEmpty(clean, reason ?? '')
  if (!detail) return head.replace(/：$/, '')
  return head + truncate(detail.replace(/\s+/g, ' '), max)
}

/** 耗时：820ms / 18.4s / 3 分 05 秒。 */
export function taskDurationText(ms?: number): string {
  const v = Math.max(0, Math.round(ms ?? 0))
  if (!v) return ''
  if (v < 1000) return `${v}ms`
  if (v < 60_000) return `${(v / 1000).toFixed(1)}s`
  const min = Math.floor(v / 60_000)
  const sec = Math.round((v % 60_000) / 1000)
  return `${min} 分 ${String(sec).padStart(2, '0')} 秒`
}

function shortTokens(n?: number): string {
  const v = Math.max(0, Math.round(n ?? 0))
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(1)}M`
  if (v >= 1000) return `${(v / 1000).toFixed(v >= 10_000 ? 0 : 1)}k`
  return String(v)
}

/** 成本/用量：`12k → 3k tok · $0.03`；未计价明确写「未计价」而不是 $0。 */
export function taskUsageText(usage?: HubTaskUsage | null): string {
  if (!usage) return ''
  const parts: string[] = []
  if (usage.input || usage.output) parts.push(`${shortTokens(usage.input)} → ${shortTokens(usage.output)} tok`)
  if (usage.cost_usd !== undefined && usage.cost_priced) {
    const usd = usage.cost_usd
    parts.push(`$${usd < 0.01 ? usd.toFixed(4) : usd.toFixed(2)}`)
  } else if (usage.cost_usd !== undefined) {
    parts.push('未计价')
  }
  return parts.join(' · ')
}

/** 产物一行：`created · out/pricing-v3.md · 2.1KB`。 */
export function taskArtifactText(artifact: HubTaskArtifact): string {
  const kind = artifact.kind || 'artifact'
  const path = artifact.path || artifact.name || ''
  const size = artifact.size
    ? artifact.size >= 1024
      ? `${(artifact.size / 1024).toFixed(1)}KB`
      : `${artifact.size}B`
    : ''
  return [kind, path, size].filter(Boolean).join(' · ')
}
