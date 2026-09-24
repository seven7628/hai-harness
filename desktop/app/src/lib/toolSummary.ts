import { t as defaultT } from '../i18n'

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
