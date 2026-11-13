import type { HubAgent, HubAvatar, HubGroup } from './types'

// 群内 @ 的前端纯逻辑（P3b-07，方案 §6.5.6 / §8.3.3 / §7.5.2）。
//
// 与 Go 侧 agenthub/mention.go 同一口径，前端只做**展示与输入**，路由以后端为准：
//   - 最长成员名优先（「Alice Chen」不能被拆成「Alice」+「Chen」）；
//   - 跳过 fenced/inline code、URL、email（写代码的 @ 不是点名）；
//   - @ 前后按 ASCII 字母数字/连字符/下划线判边界（中文「请@Alice」照常生效）。
//
// 这里不读 store/时钟，Node 直测（scripts/test-hub-p3b.mjs）。

export interface MentionAgent {
  id: string
  name: string
  /** 职责（HubAgent.role，例如「竞品研究员」）。 */
  role?: string
  /** 群内角色：owner | member | observer（@all 的合成候选用 all）。 */
  groupRole?: string
  accent?: string
  avatar?: HubAvatar
  deleted?: boolean
}

export type MentionToken =
  | { kind: 'text'; text: string }
  | { kind: 'mention'; text: string; agentId: string; name: string }

/** @all 的合成候选 id（不是真实 agent id，发送时不进结构化 mentions）。 */
export const ALL_MENTION_ID = '__all__'

const FOLD_ASCII = /[A-Za-z0-9]/
// 触发词结束符：@Wri，/@Wri. 之后就当普通正文，选择器必须立刻收起。
const MENTION_QUERY_STOP = /[\s，。！？、；：,.!?;:]/

function isAsciiAlnum(r: string): boolean {
  return FOLD_ASCII.test(r)
}

function runLength(runes: string[], start: number, ch: string): number {
  let n = 0
  while (start + n < runes.length && runes[start + n] === ch) n++
  return n
}

function findFenceEnd(runes: string[], start: number, ch: string, want: number): number {
  for (let i = start; i < runes.length;) {
    if (runes[i] !== ch) {
      i++
      continue
    }
    const n = runLength(runes, i, ch)
    if (n >= want) return i
    i += n
  }
  return -1
}

function hasPrefix(runes: string[], at: number, prefix: string): boolean {
  const p = Array.from(prefix)
  if (at + p.length > runes.length) return false
  for (let i = 0; i < p.length; i++) {
    if (runes[at + i].toLowerCase() !== p[i].toLowerCase()) return false
  }
  return true
}

function isURLTerminator(ch: string): boolean {
  if (/\s/.test(ch)) return true
  return '<>"\')]}'.includes(ch)
}

function isEmailToken(token: string[]): boolean {
  const at = token.indexOf('@')
  if (at <= 0 || at >= token.length - 1) return false
  const last = token[at - 1]
  if (!(/[A-Za-z0-9._%+-]/.test(last))) return false
  return token.slice(at + 1).includes('.')
}

/**
 * 标出 Markdown 链接/图片整段（`[label](dest)`、`![alt](src)`、`[label][ref]`）：
 * @ 不能改写链接标签，否则会生成嵌套链接（`[@Writer](#mention=…)` 套在已有
 * `[问 @Writer](url)` 里），渲染器会把整段链接拆坏（N4）。
 * 只做结构扫描，不解析 Markdown 语义；扫描失败（未闭合）就不遮，交给原有逻辑。
 */
function maskMarkdownLinks(runes: string[], masked: boolean[]): void {
  for (let i = 0; i < runes.length; i++) {
    if (masked[i]) continue
    const image = runes[i] === '!' && runes[i + 1] === '['
    const bracket = image ? i + 1 : i
    if (runes[bracket] !== '[') continue
    // label：支持一层嵌套 []（常见的 [a [b]] 也够用）；转义括号跳过。
    let depth = 1
    let end = bracket + 1
    for (; end < runes.length; end++) {
      if (runes[end] === '\\') {
        end++
        continue
      }
      if (runes[end] === '[') depth++
      else if (runes[end] === ']') {
        depth--
        if (depth === 0) break
      }
    }
    if (depth !== 0) continue
    const tail = end + 1
    let destEnd = -1
    if (runes[tail] === '(') {
      let pd = 1
      for (let k = tail + 1; k < runes.length; k++) {
        if (runes[k] === '\\') {
          k++
          continue
        }
        if (runes[k] === '(') pd++
        else if (runes[k] === ')') {
          pd--
          if (pd === 0) {
            destEnd = k
            break
          }
        }
      }
    } else if (runes[tail] === '[') {
      for (let k = tail + 1; k < runes.length; k++) {
        if (runes[k] === ']') {
          destEnd = k
          break
        }
      }
    }
    if (destEnd < 0) continue
    for (let k = i; k <= destEnd; k++) masked[k] = true
    i = destEnd
  }
}

/** 标出不参与 mention 解析的区间：fenced/inline code、URL、email（与 Go maskMentionSkipped 同口径）。 */
export function maskMentionSkipped(text: string): boolean[] {
  const runes = Array.from(text)
  const masked = new Array<boolean>(runes.length).fill(false)
  for (let i = 0; i < runes.length;) {
    const ch = runes[i]
    if (ch === '`') {
      const n = runLength(runes, i, '`')
      let end = findFenceEnd(runes, i + n, '`', n)
      if (end < 0) {
        // 未闭合的单/双反引号不是代码；三反引号未闭合按 fenced code 到 EOF。
        if (n >= 3) end = runes.length
        else {
          i += n
          continue
        }
      } else {
        end += n
      }
      for (let k = i; k < end && k < runes.length; k++) masked[k] = true
      i = end
      continue
    }
    if (ch === '~') {
      const n = runLength(runes, i, '~')
      if (n >= 3) {
        let end = findFenceEnd(runes, i + n, '~', n)
        end = end < 0 ? runes.length : end + n
        for (let k = i; k < end && k < runes.length; k++) masked[k] = true
        i = end
        continue
      }
    }
    if (hasPrefix(runes, i, 'http://') || hasPrefix(runes, i, 'https://') || hasPrefix(runes, i, 'www.')) {
      let end = i
      while (end < runes.length && !isURLTerminator(runes[end])) end++
      for (let k = i; k < end; k++) masked[k] = true
      i = end
      continue
    }
    i++
  }
  maskMarkdownLinks(runes, masked)
  // email：按空白切 token，只遮住形如 a@b.c 的整段（避免把「(@Writer)」当邮箱）。
  let i = 0
  while (i < runes.length) {
    while (i < runes.length && /\s/.test(runes[i])) i++
    const start = i
    while (i < runes.length && !/\s/.test(runes[i])) i++
    if (isEmailToken(runes.slice(start, i))) {
      for (let k = start; k < i; k++) masked[k] = true
    }
  }
  return masked
}

function boundaryBefore(runes: string[], at: number): boolean {
  if (at === 0) return true
  const r = runes[at - 1]
  return !isAsciiAlnum(r) && r !== '\\' && r !== '/' && r !== '@'
}

function boundaryAfter(runes: string[], at: number): boolean {
  if (at >= runes.length) return true
  const r = runes[at]
  return !isAsciiAlnum(r) && r !== '-' && r !== '_'
}

interface Candidate {
  agent: MentionAgent
  name: string
  runes: string[]
  fold: string
}

function sortCandidates(agents: MentionAgent[]): Candidate[] {
  const out: Candidate[] = []
  const seen = new Set<string>()
  for (const agent of agents) {
    if (!agent || !agent.id) continue
    const name = (agent.name || '').trim()
    if (!name) continue
    if (seen.has(agent.id)) continue
    seen.add(agent.id)
    out.push({ agent, name, runes: Array.from(name), fold: name.toLowerCase() })
  }
  // 最长名字优先；同长按 fold 稳定排序，保证同输入同输出（测试可断言）。
  out.sort((a, b) => b.runes.length - a.runes.length || a.fold.localeCompare(b.fold))
  return out
}

/** 解析用的候选（含已删除成员，用于历史消息灰显；发送时 mentionIds 会过滤掉）。 */
export function mentionCandidates(members: MentionAgent[]): MentionAgent[] {
  return sortCandidates(members).map((c) => c.agent)
}

/**
 * 把正文切成 text / mention 片段。未命中的 @ 保留在 text 里（不吞字）。
 * 返回的 mention.text 是正文里的原始写法（保留大小写）。
 */
export function tokenizeMentions(text: string, members: MentionAgent[]): MentionToken[] {
  const runes = Array.from(text)
  const candidates = sortCandidates(members)
  if (runes.length === 0 || candidates.length === 0) return text ? [{ kind: 'text', text }] : []
  const masked = maskMentionSkipped(text)
  const tokens: MentionToken[] = []
  let buf: string[] = []
  const flush = (): void => {
    if (buf.length === 0) return
    tokens.push({ kind: 'text', text: buf.join('') })
    buf = []
  }
  for (let i = 0; i < runes.length;) {
    const ch = runes[i]
    if (masked[i] || (ch !== '@' && ch !== '＠') || !boundaryBefore(runes, i)) {
      buf.push(ch)
      i++
      continue
    }
    const start = i + 1
    // @all / @所有人：展示层同样做成徽章（§7.5.2）；权限由后端判，前端不在这里降级。
    const allLen = runes.slice(start, start + 3).join('') === '所有人'
      ? 3
      : hasPrefix(runes, start, 'all') ? 3 : 0
    if (allLen > 0 && boundaryAfter(runes, start + allLen)) {
      flush()
      tokens.push({
        kind: 'mention',
        text: runes.slice(i, start + allLen).join(''),
        agentId: ALL_MENTION_ID,
        name: '所有人',
      })
      i = start + allLen
      continue
    }
    let matched: Candidate | null = null
    for (const c of candidates) {
      const end = start + c.runes.length
      if (end > runes.length) continue
      const fold = runes.slice(start, end).join('').toLowerCase()
      if (fold !== c.fold) continue
      if (!boundaryAfter(runes, end)) continue
      matched = c
      break
    }
    if (!matched) {
      buf.push(ch)
      i++
      continue
    }
    flush()
    const end = start + matched.runes.length
    tokens.push({
      kind: 'mention',
      text: runes.slice(i, end).join(''),
      agentId: matched.agent.id,
      name: matched.agent.name,
    })
    i = end
  }
  flush()
  return tokens
}

function escapeLinkLabel(text: string): string {
  return text.replace(/([\\[\]])/g, '\\$1')
}

/**
 * 把正文里的 @Name 变成 Markdown 链接 `[原写法](#mention=<id>)`。
 * 渲染侧给 Markdown 传 renderMention 换成 AgentMentionChip；不传则退化成普通站内链接。
 */
export function linkifyMentions(text: string, members: MentionAgent[]): string {
  if (!text) return ''
  const tokens = tokenizeMentions(text, members)
  if (!tokens.some((t) => t.kind === 'mention')) return text
  return tokens
    .map((t) =>
      t.kind === 'mention'
        ? `[${escapeLinkLabel(t.text)}](#mention=${encodeURIComponent(t.agentId)})`
        : t.text,
    )
    .join('')
}

/** 从 renderMention 的 href 里取 agent id（非 mention 链接返回 null）。 */
export function mentionIdFromHref(href: string): string | null {
  const m = /^#mention=(.+)$/.exec(href)
  if (!m) return null
  try {
    return decodeURIComponent(m[1])
  } catch {
    return null
  }
}

/** 选择器过滤：按名字 / id / 职责 / 群角色做大小写不敏感子串匹配。 */
export function filterMentionCandidates(query: string, members: MentionAgent[]): MentionAgent[] {
  const q = query.trim().toLowerCase()
  const list = sortCandidates(members).map((c) => c.agent)
  if (!q) return list
  return list.filter((a) =>
    [a.name, a.id, a.role, a.groupRole].some((v) => (v ?? '').toLowerCase().includes(q)),
  )
}

function isAsciiAlnumChar(ch: string | undefined): boolean {
  return !!ch && /[A-Za-z0-9]/.test(ch)
}

/**
 * 光标前正在输入的 @ 查询（选择器据此打开）。
 * 返回 { query, start }；start 是 '@' 的字符串下标（Esc 关闭时按它记忆）。
 */
export function activeMentionQuery(text: string, caret: number): { query: string; start: number } | null {
  const at = Math.max(0, Math.min(caret, text.length))
  let i = at - 1
  while (i >= 0) {
    const ch = text[i]
    if (ch === '@' || ch === '＠') {
      if (i > 0 && (isAsciiAlnumChar(text[i - 1]) || ['\\', '/', '@'].includes(text[i - 1]))) return null
      const query = text.slice(i + 1, at)
      // 句读/空白出现 = 这个词已经结束，选择器必须关闭（@Writer， 不能再弹）。
      if (MENTION_QUERY_STOP.test(query)) return null
      return { query, start: i }
    }
    if (/\s/.test(ch)) return null
    i--
  }
  return null
}

/**
 * 选中成员后插入 `@Name `。若光标前是 `@Ali` 这样的半截 token 就整体替换；
 * 否则在光标处插入。caret 是插入后的新光标位置（在空格之后）。
 */
export function applyMention(text: string, caret: number, name: string): { text: string; caret: number } {
  const at = Math.max(0, Math.min(caret, text.length))
  const q = activeMentionQuery(text, at)
  const start = q ? q.start : at
  // 后面已经有一个空白（用户打完词再点选）就别再补一个空格；否则补上，
  // 保证插入后能继续往下打字。两种情况的 caret 都在「名字 + 一个空白」之后。
  const next = text[at]
  const suffix = next && /\s/.test(next) ? '' : ' '
  const insert = `@${name}${suffix}`
  return {
    text: text.slice(0, start) + insert + text.slice(at),
    caret: start + 1 + name.length + 1,
  }
}

/**
 * IME 合成中：Enter/Esc 属于输入法（确认候选/取消），绝不能当发送/关闭处理。
 * 兼容 React 的 nativeEvent.isComposing 与旧浏览器的 keyCode 229（Safari/WebKit）。
 */
export function isImeComposing(
  nativeEvent: { isComposing?: boolean } | null | undefined,
  keyCode?: number,
): boolean {
  return nativeEvent?.isComposing === true || keyCode === 229
}

export type MentionKeyAction = 'up' | 'down' | 'pick' | 'dismiss' | 'none'

/**
 * 选择器键盘语义（textarea 与 listbox 共用）：
 * 空候选时 Enter/Tab **不拦截**（Enter 走发送，Tab 走焦点移动）；Esc 永远关闭。
 */
export function mentionKeyAction(key: string, hasCandidates: boolean): MentionKeyAction {
  if (key === 'ArrowUp') return hasCandidates ? 'up' : 'none'
  if (key === 'ArrowDown') return hasCandidates ? 'down' : 'none'
  if (key === 'Enter' || key === 'Tab') return hasCandidates ? 'pick' : 'none'
  if (key === 'Escape') return 'dismiss'
  return 'none'
}

/** 正文里命中的成员 id（去重、保持出现顺序；已删除成员不发送）。 */
export function mentionIds(text: string, members: MentionAgent[]): string[] {
  const byID = new Map(members.map((m) => [m.id, m]))
  const out: string[] = []
  for (const t of tokenizeMentions(text, members)) {
    if (t.kind !== 'mention') continue
    // @all 不是结构化 mention（后端用 mention_all 字段），不塞进 mentions 数组。
    if (t.agentId === ALL_MENTION_ID) continue
    if (byID.get(t.agentId)?.deleted) continue
    if (!out.includes(t.agentId)) out.push(t.agentId)
  }
  return out
}

/** @all 是否出现在正文里（跳过 code/URL/email；后端仍会再鉴权一次）。 */
export function hasMentionAll(text: string): boolean {
  const runes = Array.from(text)
  const masked = maskMentionSkipped(text)
  for (let i = 0; i < runes.length; i++) {
    if (masked[i] || (runes[i] !== '@' && runes[i] !== '＠') || !boundaryBefore(runes, i)) continue
    if (hasPrefix(runes, i + 1, 'all') && boundaryAfter(runes, i + 4)) return true
    if (runes.slice(i + 1, i + 4).join('') === '所有人' && boundaryAfter(runes, i + 4)) return true
  }
  return false
}

/** 群成员 + profile → @ 名册（历史的已删除成员保留，用于灰显）。 */
export function groupRoster(group: HubGroup | null | undefined, agents: HubAgent[]): MentionAgent[] {
  if (!group) return []
  const byID = new Map(agents.map((a) => [a.id, a]))
  return (group.members ?? []).map((m) => {
    const a = byID.get(m.agent_id)
    return {
      id: m.agent_id,
      name: a?.name || m.agent_id,
      role: a?.role,
      groupRole: m.role,
      accent: a?.accent,
      avatar: a?.avatar,
      deleted: a?.deleted === true,
    }
  })
}

/** 可接派任务的成员：observer 只旁听（方案 §8.3.3），已删除的不列。 */
export function assignableMembers(members: MentionAgent[]): MentionAgent[] {
  return members.filter((m) => !m.deleted && m.groupRole !== 'observer')
}
