import type { HubAgent, HubGroup, HubMessage, HubTask } from './types'
import { taskStatusView } from './taskCard'

// Markdown 导出（P4-02）：群时间线 / 单个任务。
//
// 纯函数（不碰 DOM），Node 直测（scripts/test-hub-context.mjs）；下载由 download.ts 负责。
//
// 安全口径（导出文件会进 Obsidian/编辑器/分享）：
//   ① 所有来自模型/用户的文本先做 HTML 转义（`<script>` 在 Markdown 预览里会被当 HTML 执行）；
//   ② 单行字段（标题/名字/路径/状态）额外转义 Markdown 元字符，并且折叠换行 ——
//      一个名字里的 `*` 或换行不能把整份文档结构改掉；
//   ③ 多行正文统一进引用块（每行 `> ` 前缀），代码围栏与空行都不会跑到文档顶层。

/** HTML 文本转义：`&` 先转，避免把后面生成的实体再转一遍。不做属性用，故不转引号。 */
export function escapeHtmlText(text: string | null | undefined): string {
  return String(text ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
}

/**
 * 单行字段：折叠空白 → 按码点截断（CJK 不会被切成半个）→ HTML 转义 → Markdown 元字符转义。
 * 先截断再转义，避免把 `&amp;` 截成 `&am`。
 */
export function escapeInline(text: string | null | undefined, max = 160): string {
  const flat = String(text ?? '').replace(/\s+/g, ' ').trim()
  const runes = Array.from(flat)
  const cap = Math.max(1, Math.trunc(max) || 160)
  const clipped = runes.length > cap ? runes.slice(0, cap - 1).join('') + '…' : flat
  return escapeHtmlText(clipped)
    .replace(/([\\`*_[\]()#|~])/g, '\\$1')
    .replace(/^[-+]/, (m) => `\\${m}`)
}

/**
 * 行内代码（id/路径/轮次）：内容按字面展示，所以**不**转义 Markdown 元字符
 *（`grp_1` 里的 `_` 被转义成 `\_` 会在代码里显示出来）。含反引号时用双反引号包住。
 */
export function inlineCode(text: string | null | undefined, max = 240): string {
  const flat = String(text ?? '').replace(/\s+/g, ' ').trim()
  const runes = Array.from(flat)
  const cap = Math.max(1, Math.trunc(max) || 240)
  const clipped = runes.length > cap ? runes.slice(0, cap - 1).join('') + '…' : flat
  const body = escapeHtmlText(clipped)
  const fence = body.includes('`') ? '``' : '`'
  const pad = body.includes('`') ? ' ' : ''
  return `${fence}${pad}${body}${pad}${fence}`
}

/** 多行正文 → 引用块（空行输出单独的 `>`，保留段落）。 */
export function quoteMarkdown(text: string | null | undefined): string {
  const normalized = String(text ?? '').replace(/\r\n?/g, '\n').replace(/^\n+|\n+$/g, '')
  if (!normalized.trim()) return '> （空）'
  return escapeHtmlText(normalized)
    .split('\n')
    .map((line) => (line.length > 0 ? `> ${line}` : '>'))
    .join('\n')
}

function agentNames(agents?: HubAgent[] | null): Record<string, string> {
  const map: Record<string, string> = {}
  for (const a of agents ?? []) {
    if (a?.id) map[a.id] = a.name || a.id
  }
  return map
}

function nameOf(id: string, names: Record<string, string>): string {
  if (!id) return '—'
  return escapeInline(names[id] || id, 60)
}

function senderLabel(msg: HubMessage, names: Record<string, string>): string {
  const kind = msg.sender?.kind ?? ''
  if (kind === 'human') return '你'
  if (kind === 'system') return '系统'
  return nameOf(String(msg.sender?.agent_id ?? ''), names)
}

function timeOf(value?: string): string {
  const text = String(value ?? '').trim()
  return text ? escapeInline(text, 40) : '—'
}

function messageContextLine(msg: HubMessage, names: Record<string, string>): string {
  const seqs = (msg.context?.trigger_seqs ?? []).filter((n) => Number(n) > 0)
  const senders = (msg.context?.senders ?? [])
    // 显式 name 同样来自消息上下文（后端快照/模型输入），必须按单行字段转义：
    // 原样拼进 Markdown 会变成可执行 HTML（复审 B2）。nameOf 内部已转义。
    .map((s) => {
      const explicit = String(s?.name ?? '').trim()
      return explicit ? escapeInline(explicit, 60) : nameOf(String(s?.agent_id ?? ''), names)
    })
    .filter((name) => name !== '—')
  const parts: string[] = []
  if (seqs.length > 0) parts.push(`触发：${seqs.map((n) => `#${n}`).join(', ')}`)
  if (senders.length > 0) parts.push(`来自 ${senders.join('、')}`)
  if (msg.round_id) parts.push(`轮次 ${inlineCode(msg.round_id, 60)}`)
  return parts.length > 0 ? `- 上下文：${parts.join(' · ')}` : ''
}

function taskBlock(msg: HubMessage, tasks: HubTask[] | undefined, names: Record<string, string>): string {
  if (msg.kind !== 'task_result') return ''
  const refs = msg.refs ?? {}
  const taskId = String(refs.task_id ?? '')
  const task = taskId ? (tasks ?? []).find((t) => t.id === taskId) : undefined
  const status = String(refs.status ?? task?.status ?? '')
  const exec = String(refs.target_agent_id ?? refs.agent_id ?? '')
  const head = [
    `**任务卡**${taskId ? ` ${inlineCode(taskId, 80)}` : ''}`,
    status ? escapeInline(status, 40) : '',
    exec ? `执行 ${nameOf(exec, names)}` : '',
  ].filter(Boolean).join(' · ')
  const lines = [head]
  const summary = String(task?.result?.summary ?? '').trim()
  if (summary && summary !== String(msg.text ?? '').trim()) lines.push(`- 摘要：${escapeInline(summary, 240)}`)
  const artifacts = task?.result?.artifacts ?? []
  if (artifacts.length > 0) {
    lines.push(`- 产物：${artifacts.map((a) => {
      const path = String(a.path || a.name || '').trim()
      const kind = String(a.kind ?? '').trim()
      return `${inlineCode(path, 200)}${kind ? `（${escapeInline(kind, 20)}）` : ''}`
    }).join('、')}`)
  }
  return lines.join('\n')
}

function memberLine(group: HubGroup, names: Record<string, string>): string {
  const members = group.members ?? []
  if (members.length === 0) return '—'
  return members
    .map((m) => {
      const role = String(m.role ?? '').trim()
      return `${nameOf(m.agent_id, names)}${role ? `（${escapeInline(role, 20)}）` : ''}`
    })
    .join('、')
}

function policyLine(group: HubGroup): string {
  const p = group.policy ?? {}
  const parts: string[] = []
  if (p.mode) parts.push(`mode=${escapeInline(p.mode, 20)}`)
  if (p.max_handoff_depth) parts.push(`交接深度 ${p.max_handoff_depth}`)
  if (p.daily_budget_usd) parts.push(`日预算 $${p.daily_budget_usd}`)
  return parts.join(' · ')
}

export interface GroupMarkdownInput {
  group: HubGroup
  agents?: HubAgent[] | null
  messages: HubMessage[]
  tasks?: HubTask[] | null
  /** 固定导出时间（测试用）；缺省 = 当前时间 ISO。 */
  exportedAt?: string
}

/** 群时间线 → Markdown：群头/成员/策略 + 每条消息的 seq/发送者/时间/正文 + 任务卡摘要。 */
export function groupTimelineMarkdown(input: GroupMarkdownInput): string {
  const { group } = input
  const names = agentNames(input.agents)
  const exportedAt = input.exportedAt ?? new Date().toISOString()
  const messages = [...(input.messages ?? [])]
    .filter((m) => Number(m?.seq) > 0)
    .sort((a, b) => Number(a.seq) - Number(b.seq))
  const first = messages[0]?.seq
  const last = messages[messages.length - 1]?.seq
  const groupName = escapeInline(group.name || group.id, 120)

  const lines: string[] = [`# 群：${groupName}`, '']
  lines.push(`- 群组：${inlineCode(group.id, 120)}`)
  lines.push(`- 成员：${memberLine(group, names)}`)
  const policy = policyLine(group)
  if (policy) lines.push(`- 策略：${policy}`)
  lines.push(
    messages.length > 0
      ? `- 消息：${messages.length} 条 · seq ${first}–${last}`
      : '- 消息：0 条',
  )
  lines.push(`- 导出：${escapeInline(exportedAt, 40)}`)
  lines.push('', '## 时间线', '')

  if (messages.length === 0) {
    lines.push('（还没有消息）')
  }
  for (const msg of messages) {
    lines.push(`### #${msg.seq} · ${senderLabel(msg, names)} · ${timeOf(msg.created_at)}`, '')
    lines.push(quoteMarkdown(msg.text ?? ''))
    const context = messageContextLine(msg, names)
    if (context) lines.push('', context)
    const task = taskBlock(msg, input.tasks ?? undefined, names)
    if (task) lines.push('', task)
    lines.push('')
  }
  return lines.join('\n').replace(/\n+$/, '\n')
}

export interface TaskMarkdownOptions {
  agentNames?: Record<string, string> | null
  exportedAt?: string
}

function firstLine(text?: string): string {
  return String(text ?? '').split('\n').map((line) => line.trim()).find(Boolean) ?? ''
}

function usageSection(task: HubTask): string[] {
  const usage = task.result?.usage
  const lines = ['## 用量', '']
  if (!usage) {
    lines.push('- 未记录')
    return lines
  }
  const tokens = [`输入 ${usage.input ?? 0}`, `输出 ${usage.output ?? 0}`]
  if (usage.cache_read) tokens.push(`缓存读 ${usage.cache_read}`)
  if (usage.cache_write) tokens.push(`缓存写 ${usage.cache_write}`)
  if (usage.reasoning) tokens.push(`推理 ${usage.reasoning}`)
  lines.push(`- Tokens：${tokens.join(' · ')}`)
  lines.push(`- 成本：${usage.cost_priced ? `$${(usage.cost_usd ?? 0).toFixed(4)}` : '未计价'}`)
  return lines
}

/** 任务 → Markdown：id/标题/状态/来源/目标/指令/结果/产物/错误/用量。 */
export function taskMarkdown(task: HubTask, options: TaskMarkdownOptions = {}): string {
  const names = options.agentNames ?? {}
  const title = escapeInline(task.title || firstLine(task.instruction) || task.id, 160)
  const lines: string[] = [`# 任务：${title}`, '']
  lines.push(`- ID：${inlineCode(task.id, 120)}`)
  const status = String(task.status ?? '').trim()
  lines.push(`- 状态：${status ? `${escapeInline(status, 40)}（${escapeInline(taskStatusView(status).label, 60)}）` : '—'}`)

  const origin = task.origin ?? {}
  const originParts: string[] = []
  if (origin.kind) originParts.push(`kind=${escapeInline(origin.kind, 20)}`)
  if (origin.group_id) originParts.push(`群 ${inlineCode(origin.group_id, 120)}`)
  if (origin.agent_id) originParts.push(`发起者 ${origin.agent_name ? escapeInline(origin.agent_name, 60) : nameOf(origin.agent_id, names)}`)
  if (origin.message_seq) originParts.push(`消息 #${origin.message_seq}`)
  lines.push(`- 来源：${originParts.join(' · ') || '—'}`)

  const target = task.target ?? {}
  const targetParts: string[] = []
  if (target.kind) targetParts.push(`kind=${escapeInline(target.kind, 20)}`)
  if (target.agent_id) targetParts.push(`执行者 ${nameOf(target.agent_id, names)}`)
  if (target.group_id) targetParts.push(`群 ${inlineCode(target.group_id, 120)}`)
  if (target.workspace) targetParts.push(`工作区 ${inlineCode(target.workspace, 240)}`)
  if (target.session_id) targetParts.push(`会话 ${inlineCode(target.session_id, 160)}`)
  lines.push(`- 目标：${targetParts.join(' · ') || '—'}`)

  const stamps: string[] = []
  if (task.created_at) stamps.push(`创建 ${timeOf(task.created_at)}`)
  if (task.updated_at) stamps.push(`更新 ${timeOf(task.updated_at)}`)
  if (task.attempt && task.attempt > 1) stamps.push(`第 ${task.attempt} 次尝试`)
  if (stamps.length > 0) lines.push(`- 时间：${stamps.join(' · ')}`)
  if (task.root_task_id) lines.push(`- 根任务：${inlineCode(task.root_task_id, 120)}`)
  if (task.parent_task_id) lines.push(`- 父任务：${inlineCode(task.parent_task_id, 120)}`)

  if (String(task.instruction ?? '').trim()) {
    lines.push('', '## 指令', '', quoteMarkdown(task.instruction))
  }
  if (String(task.result?.summary ?? '').trim()) {
    lines.push('', '## 结果', '', quoteMarkdown(task.result?.summary))
  }
  const artifacts = task.result?.artifacts ?? []
  if (artifacts.length > 0) {
    lines.push('', '## 产物', '')
    for (const artifact of artifacts) {
      const path = String(artifact.path || artifact.name || '').trim()
      const meta = [
        String(artifact.kind ?? '').trim(),
        artifact.lines ? `${artifact.lines} 行` : '',
        artifact.size ? `${artifact.size} B` : '',
      ].filter(Boolean).map((v) => escapeInline(v, 40)).join(' · ')
      lines.push(`- ${inlineCode(path, 240)}${meta ? `（${meta}）` : ''}`)
    }
  }
  if (String(task.error ?? '').trim()) {
    lines.push('', '## 错误', '', quoteMarkdown(task.error))
  }
  lines.push('', ...usageSection(task))
  const exportedAt = options.exportedAt
  if (exportedAt) lines.push('', `- 导出：${escapeInline(exportedAt, 40)}`)
  return lines.join('\n').replace(/\n+$/, '\n')
}
