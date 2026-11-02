import { t as defaultT } from '../i18n'
import { fmtDuration } from './duration'

// 工具调用参数摘要（链路树工具节点 / 消息工具 chip 共用）。
// 依据 SDK 工具参数结构（tools/builtin、todo、subagent、plan）：
//   read_file   path + start_line/end_line → "auth.go:12-40"
//   write/edit  path；list_dir path；grep pattern；bash command
//   todo_add title / todo_update id / agent_send|interrupt|TaskOutput task_id
// 无法解析或不认识 → 返回 ''（调用方回退纯工具名）。
export function toolArgsSummary(name: string, args: string, t: (k: string) => string = defaultT): string {
  let a: Record<string, unknown>
  try {
    a = JSON.parse(args)
  } catch {
    return ''
  }
  switch (name) {
    case 'read_file': {
      const path = String(a.path ?? '')
      if (!path) return ''
      const s = a.start_line
      const e = a.end_line
      if (typeof s === 'number' && typeof e === 'number') {
        // read_file 将反向区间归一化为升序，摘要也显示实际读取区间。
        return `${path}:${Math.min(s, e)}-${Math.max(s, e)}`
      }
      return path
    }
    case 'write_file':
    case 'edit_file':
    case 'list_dir':
      return String(a.path ?? '')
    case 'grep':
      return String(a.pattern ?? a.path ?? '')
    case 'bash':
      return String(a.command ?? '').slice(0, 44)
    case 'todo_add':
      return String(a.title ?? '').slice(0, 24)
    case 'todo_update':
      return a.id ? `#${a.id}` : ''
    case 'agent_spawn':
      return String(a.task ?? a.prompt ?? '').slice(0, 24)
    case 'agent_interrupt':
    case 'agent_send':
    case 'agent_resume':
    case 'TaskOutput':
      return String(a.task_id ?? '')
    case 'ask_user': {
      const qs = Array.isArray(a.questions) ? (a.questions as { question?: unknown }[]) : []
      const first = String(qs[0]?.question ?? '')
      const extra = qs.length > 1 ? t('toolSummary.askMore').replace('{n}', String(qs.length)) : ''
      return (first + extra).slice(0, 32)
    }
    default:
      return ''
  }
}

// 参数完整展示：JSON 美化排版；非 JSON（明文串）原样返回。
export function prettyArgs(args: string): string {
  if (!args) return ''
  try {
    return JSON.stringify(JSON.parse(args), null, 2)
  } catch {
    return args
  }
}

// 链路树工具节点短标签：名 + 参数摘要（如 "read_file auth.go:12-40"）
export function toolNodeLabel(name: string, args: string): string {
  const s = toolArgsSummary(name, args)
  return s ? `${name} ${s}` : name
}

/* ── 运行时皮肤的工具行：动词、对象、度量 ──────────────────────────────────────
   v2 的工具行是六列（折角 · 图标 · 动词 · 对象 · 度量 · 时长）。三个纯函数把
   「这次调用是什么、动了多少、花了多久」算出来；算不出就返回 null —— 行里那一格
   宁可为空，也不许填一个猜的数（设计与红线都要求这一点）。 */

// 动词：个体行用工具名的大写形式（设计 §11.6.3：批处理行才用过去式）
const VERBS: Record<string, string> = {
  read_file: 'Read',
  write_file: 'Write',
  edit_file: 'Edit',
  multi_edit: 'Edit',
  list_dir: 'List',
  glob: 'Glob',
  grep: 'Grep',
  bash: 'Bash',
  todo_add: 'Todo',
  todo_update: 'Todo',
  todo_write: 'Todo',
  todo_list: 'Todo',
  ask_user: 'Ask',
  agent_spawn: 'Agent',
  agent_send: 'Tell',
  agent_wait: 'Wait',
  agent_interrupt: 'Stop',
  SKILL: 'Skill',
  codemode: 'Codemode',
  plan_submit: 'Plan',
  web_fetch: 'Fetch',
  web_search: 'Search',
}

export function toolVerb(name: string): string {
  const v = VERBS[name]
  if (v) return v
  if (!name) return ''
  return name.charAt(0).toUpperCase() + name.slice(1).replace(/_/g, ' ')
}

export type ToolMetric =
  | { kind: 'diff'; added: number; removed: number }
  | { kind: 'text'; text: string }
  | null

function argNum(args: string, key: string): number | undefined {
  try {
    const v = (JSON.parse(args) as Record<string, unknown>)[key]
    return typeof v === 'number' ? v : undefined
  } catch {
    return undefined
  }
}

/* 度量口径（与设计一致）：
   diff → +a −b；读文件 → 行数（优先参数区间，其次结果行数）；检索 → 命中行数；
   列目录/glob → 文件数；bash → 结果字节数（KB，一位小数）。其余工具没有可数的东西。 */
export function toolMetric(
  name: string,
  args: string,
  result: string | undefined,
  diff?: { added?: number; removed?: number } | null,
): ToolMetric {
  if (diff && (diff.added != null || diff.removed != null)) {
    return { kind: 'diff', added: diff.added ?? 0, removed: diff.removed ?? 0 }
  }
  const res = result ?? ''
  const lines = res ? res.split('\n').filter((l) => l.trim().length > 0).length : 0
  if (name === 'read_file') {
    const s = argNum(args, 'start_line')
    const e = argNum(args, 'end_line')
    const n = s && e && e >= s ? e - s + 1 : lines
    return n > 0 ? { kind: 'text', text: `${n} lines` } : null
  }
  if (name === 'grep' || name === 'web_search') {
    return lines > 0 ? { kind: 'text', text: `${lines} ${lines === 1 ? 'hit' : 'hits'}` } : null
  }
  if (name === 'glob' || name === 'list_dir') {
    return lines > 0 ? { kind: 'text', text: `${lines} ${lines === 1 ? 'file' : 'files'}` } : null
  }
  if (name === 'bash') {
    if (!res) return null
    const kb = res.length / 1024
    return { kind: 'text', text: `${kb >= 1024 ? (kb / 1024).toFixed(1) + ' MB' : kb.toFixed(1) + ' KB'}` }
  }
  return null
}

/* 工具调用时长的既有入口：口径同 fmtDuration（全仓归一化写法），空值仍返回 ''。
   保留这个函数名是因为批量行 / 工具行 / 嵌套调用都从这里取。 */
export function fmtMs(ms?: number): string {
  if (ms == null || ms < 0) return ''
  return fmtDuration(ms)
}

/** 从工具参数 JSON 里取 bash 的**命令原文**（Live output 的 `$ …` 提示行）。
 *  解析不了（不是 JSON / 没有 command）就返回 undefined —— 提示行退化成
 *  「命令未记录」，绝不把半截 JSON 当命令显示。 */
export function bashCommandOf(args: string): string | undefined {
  if (!args) return undefined
  try {
    const o = JSON.parse(args) as { command?: unknown; cmd?: unknown }
    const c = typeof o.command === 'string' ? o.command : typeof o.cmd === 'string' ? o.cmd : ''
    return c.trim() || undefined
  } catch {
    return undefined
  }
}
