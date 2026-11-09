// inputHistory.ts —— 输入框「↑/↓ 回溯历史输入」的模型 + 持久化。
//
// 为什么独立成 lib：两件事都必须能单测（node:test，无 DOM）——
//   ① 键盘判定：什么时候 ↑ 才算「取历史」而不是移动光标（多行输入里很容易判错）；
//   ② 导航与容量：越界、草稿恢复契约（草稿由调用方保存）、重复输入、每个 Session 100 条上限。
//
// 存储 = 两层：
//   · localStorage（键 go-code.inputHistory）：同步镜像 —— ↑ 按下时必须立刻有结果，
//     不能等 IPC；
//   · Electron 主进程的 ~/.go-code/input-history.json：可备份的本地文件，跨重启/清站点
//     数据/换安装都还在（见 electron/input-history.ts；Composer 启动/切会话时用文件对齐镜像）。
// 作用域按「会话」隔离（正在看子智能体时按子智能体再分一份），切会话不串台。
//
// 历史条目不是纯文本，而是**发送时的输入快照**（用户 2026-11 要求覆盖 Command/文件/图片）：
//   · text  = 用户键入的原文（含 /命令 与手打的 @引用）；
//   · refs  = 文档引用 chips 的元数据（path/name/size）——回填后发送时仍走 @引用；
//   · images= 图片附件的**指纹**（mime/长度/采样哈希），不是图片本身。
// 图片字节不进 localStorage：单张可达 3MiB、100 条就是几百 MB，配额必炸。运行期把
// 原始内容放在 Composer 的内存缓存里（有字节预算），重启后按指纹回查会话转录里的
// user 块（bridge 已持久化完整 contents，含图片）。因此历史条目仍然很小，但常见情况下
// ↑ 能把图片原样找回。
//
// 容量口径（用户要求：每个 Session 最多 100 条）：
//   · 每个作用域保留最近 100 条；重复输入**移到最新**，不占额外槽位（否则连按几次
//     「继续」就把真正的历史挤没了）；
//   · 最多保留 40 个作用域（LRU，防 localStorage 无界增长，与 sessionTabs 同口径）；
//   · 单作用域 200k 字符 / 全局 1.5M 字符软预算：超长粘贴时丢最旧的，而不是让
//     localStorage 配额静默失败。
const LS_KEY = 'go-code.inputHistory'
const VERSION = 2
const LEGACY_VERSION = 1 // v1 = 纯文本数组（本功能首版）；读到时迁移成 { text }

export const INPUT_HISTORY_MAX = 100
export const INPUT_HISTORY_MAX_SCOPES = 40
export const INPUT_HISTORY_SCOPE_CHARS = 200_000
export const INPUT_HISTORY_TOTAL_CHARS = 1_500_000
/** 单条上限：超大粘贴直接不入历史（而不是把已有历史全部挤掉后再自己消失）。 */
export const INPUT_HISTORY_ENTRY_CHARS = 100_000

/** localStorage 的最小接口（单测注入内存实现；隐私模式下可以是 null）。 */
export interface InputHistoryStorage {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
}

/** 文档引用快照（与 Composer 的 DocRef 同形；size < 0 = stat 不可用）。 */
export interface InputHistoryRef { path: string; name: string; size: number }

/**
 * 图片指纹：只用于把历史条目和「真正的图片字节」对上。
 * hash 取首尾各 2KB 的 FNV-1a（大图不必全量哈希；len+mime 已排除绝大多数误配）。
 */
export interface HistoryImageFingerprint { mime: string; len: number; hash: string }

export interface InputHistoryItem {
  text: string
  /** 发送时刻（unix ms）。用于「本地镜像 + 文件持久化」两侧合并排序；旧数据缺省 = 0。 */
  at?: number
  refs?: InputHistoryRef[]
  images?: HistoryImageFingerprint[]
}

interface ScopeHistory { t: number; items: InputHistoryItem[] }
export interface InputHistoryState { v: number; scopes: Record<string, ScopeHistory> }

export function emptyInputHistory(): InputHistoryState {
  return { v: VERSION, scopes: {} }
}

/**
 * 作用域 key：工作区 + 会话（+ 子智能体）用 NUL 分隔（路径/sid 不可能含 NUL）。
 * 无会话时落在 `${workspace}\0`，会话建立后由 adoptInputHistory 并进会话作用域。
 * 子智能体用 **task_id** 而不是 run_id：resume 会换 run_id 但保持 task_id，
 * 用 run_id 会让「续跑后的同一个子会话」丢掉之前的历史。
 */
export function inputHistoryScope(workspace: string, sessionId?: string, agentTaskId?: string): string {
  const base = sessionId ? `${workspace}\u0000${sessionId}` : `${workspace}\u0000`
  return agentTaskId ? `${base}\u0000agent:${agentTaskId}` : base
}

function hashSample(s: string): string {
  // 4 段各 1KB：大图不必全量哈希（↑ 连按要快），但同长度不同图必须能区分。
  const CHUNK = 1024
  const starts = [0, Math.floor(s.length / 3), Math.floor((s.length * 2) / 3), Math.max(0, s.length - CHUNK)]
  let h = 2166136261
  for (const start of starts) {
    const end = Math.min(s.length, start + CHUNK)
    for (let i = start; i < end; i++) {
      h ^= s.charCodeAt(i)
      h = Math.imul(h, 16777619)
    }
  }
  return (h >>> 0).toString(36)
}

export function imageFingerprint(content: string, mimeType?: string): HistoryImageFingerprint {
  return { mime: mimeType ?? '', len: content.length, hash: hashSample(content) }
}

/** 从内容块里只取图片（text 块不参与匹配，正文另有 text 字段）。 */
export function imageFingerprints(contents: readonly { type: string; content: string; mimeType?: string }[]): HistoryImageFingerprint[] {
  return contents
    .filter((c) => c.type === 'image')
    .map((c) => imageFingerprint(c.content, c.mimeType))
}

export function imageKey(images?: readonly HistoryImageFingerprint[]): string {
  return (images ?? []).map((i) => `${i.mime}:${i.len}:${i.hash}`).join('\u0001')
}

/** 通用内容块（与 Composer/store 的 QueuedContent 结构对齐；lib 不依赖 store 类型）。 */
export interface FingerprintableContent { type: string; content: string; mimeType?: string }

/**
 * 按图片指纹从候选内容块里找回原图（历史条目只存指纹，原图在会话转录/暂存队列里）。
 * candidates 建议按「新 → 旧」排列；返回第一个指纹完全一致的图片块数组。
 */
export function findHistoryImages<T extends FingerprintableContent>(
  images: readonly HistoryImageFingerprint[],
  candidates: readonly (readonly T[])[],
): T[] | undefined {
  if (!images.length) return undefined
  const key = imageKey(images)
  for (const contents of candidates) {
    const found = contents.filter((c) => c.type === 'image') as T[]
    if (found.length && imageKey(imageFingerprints(found)) === key) return found
  }
  return undefined
}

/** 条目身份：文本 + 引用路径 + 图片指纹。用于去重与「同文不同附件」区分。 */
export function itemKey(item: InputHistoryItem): string {
  return JSON.stringify([item.text, item.refs ?? [], item.images ?? []])
}

function validRef(x: unknown): x is InputHistoryRef {
  if (!x || typeof x !== 'object') return false
  const r = x as { path?: unknown; name?: unknown; size?: unknown }
  return typeof r.path === 'string' && typeof r.name === 'string' && typeof r.size === 'number'
}

function validImage(x: unknown): x is HistoryImageFingerprint {
  if (!x || typeof x !== 'object') return false
  const i = x as { mime?: unknown; len?: unknown; hash?: unknown }
  return typeof i.mime === 'string' && typeof i.len === 'number' && typeof i.hash === 'string'
}

/** 规范化：trim 正文；丢弃非法 refs/images；空条目 → undefined。 */
export function normalizeHistoryItem(item: InputHistoryItem): InputHistoryItem | undefined {
  const text = typeof item.text === 'string' ? item.text.trim() : ''
  const refs = (item.refs ?? []).filter(validRef).map((r) => ({ path: r.path, name: r.name, size: r.size }))
  const images = (item.images ?? []).filter(validImage).map((i) => ({ mime: i.mime, len: i.len, hash: i.hash }))
  if (!text && !refs.length && !images.length) return undefined
  const at = typeof item.at === 'number' && Number.isFinite(item.at) ? item.at : undefined
  const clean: InputHistoryItem = {
    text,
    ...(at != null ? { at } : {}),
    ...(refs.length ? { refs } : {}),
    ...(images.length ? { images } : {}),
  }
  // 单条上限：拒绝超大条目，别让它先驱逐旧历史、再被 prune 掉（等于白发一次）
  if (itemKey(clean).length > INPUT_HISTORY_ENTRY_CHARS) return undefined
  return clean
}

/** 反序列化：坏数据 / 未知版本 / 非法条目一律静默丢弃（历史不是关键数据）。v1 自动迁移。 */
export function parseInputHistory(raw: string | null): InputHistoryState {
  if (!raw) return emptyInputHistory()
  let obj: unknown
  try {
    obj = JSON.parse(raw)
  } catch {
    return emptyInputHistory()
  }
  const rec = obj as { v?: unknown; scopes?: unknown } | null
  if (!rec || (rec.v !== VERSION && rec.v !== LEGACY_VERSION) || !rec.scopes || typeof rec.scopes !== 'object') {
    return emptyInputHistory()
  }
  const scopes: Record<string, ScopeHistory> = {}
  for (const [key, value] of Object.entries(rec.scopes as Record<string, unknown>)) {
    if (!key || !value || typeof value !== 'object') continue
    const entry = value as { t?: unknown; items?: unknown }
    if (!Array.isArray(entry.items)) continue
    const items: InputHistoryItem[] = []
    for (const x of entry.items) {
      // v1：items 是 string[]；v2：items 是对象。两种都过 normalize，迁移与校验共用一条路。
      const item = normalizeHistoryItem(typeof x === 'string' ? { text: x } : (x as InputHistoryItem))
      if (item) items.push(item)
    }
    const capped = items.slice(-INPUT_HISTORY_MAX)
    if (!capped.length) continue
    scopes[key] = {
      t: typeof entry.t === 'number' && Number.isFinite(entry.t) ? entry.t : 0,
      items: capped,
    }
  }
  // 读到的旧数据也过一遍容量裁剪（手工改过 localStorage / 旧版本超限时不让它进内存无界膨胀）
  return pruneInputHistory({ v: VERSION, scopes })
}

/** 追加一条：去重（同文同附件移到最后）+ 截到 100 条。 */
export function appendInputHistory(items: readonly InputHistoryItem[], item: InputHistoryItem): InputHistoryItem[] {
  const entry = normalizeHistoryItem(item)
  if (!entry) return [...items]
  const key = itemKey(entry)
  return [...items.filter((x) => itemKey(x) !== key), entry].slice(-INPUT_HISTORY_MAX)
}

function charsOf(items: readonly InputHistoryItem[]): number {
  let n = 0
  // 图片只存指纹，itemKey 足以代表落盘体积（正文 + refs + 指纹）。
  for (const item of items) n += itemKey(item).length
  return n
}

/** 容量裁剪：单作用域条数/字符预算 → 作用域数量 LRU → 全局字符预算。 */
export function pruneInputHistory(state: InputHistoryState, maxScopes = INPUT_HISTORY_MAX_SCOPES): InputHistoryState {
  const scopes: Record<string, ScopeHistory> = {}
  for (const [key, value] of Object.entries(state.scopes)) {
    let items = value.items.slice(-INPUT_HISTORY_MAX)
    let chars = charsOf(items)
    let drop = 0
    while (chars > INPUT_HISTORY_SCOPE_CHARS && drop < items.length) {
      chars -= itemKey(items[drop]).length
      drop++
    }
    items = items.slice(drop)
    if (items.length) scopes[key] = { t: value.t, items }
  }
  const keys = Object.keys(scopes)
  if (keys.length > maxScopes) {
    keys.sort((a, b) => scopes[a].t - scopes[b].t)
    for (const key of keys.slice(0, keys.length - maxScopes)) delete scopes[key]
  }
  let total = 0
  for (const v of Object.values(scopes)) total += charsOf(v.items)
  while (total > INPUT_HISTORY_TOTAL_CHARS) {
    const oldest = Object.keys(scopes).sort((a, b) => scopes[a].t - scopes[b].t)[0]
    if (!oldest) break
    total -= charsOf(scopes[oldest].items)
    delete scopes[oldest]
  }
  return { v: VERSION, scopes }
}

/**
 * 历史导航状态（纯数据）：items 从旧到新，index 指向当前选中项。
 * 草稿（进入历史前的正文/引用/图片）由调用方保存 —— 本模块只负责「选中哪一条」。
 */
export interface HistoryNavigation { items: InputHistoryItem[]; index: number }

/** ↑：进入历史（最新一条）或向更旧一条移动；没有历史 → null；已到最旧 → 原样返回。 */
export function recallOlder(nav: HistoryNavigation | null, history: readonly InputHistoryItem[]): HistoryNavigation | null {
  if (!history.length) return nav
  if (!nav) return { items: [...history], index: history.length - 1 }
  if (nav.index <= 0) return nav
  return { ...nav, index: nav.index - 1 }
}

/** ↓：向更新一条移动；越过最新 → null（调用方据此恢复草稿并退出历史模式）。 */
export function recallNewer(nav: HistoryNavigation | null): HistoryNavigation | null {
  if (!nav) return null
  if (nav.index >= nav.items.length - 1) return null
  return { ...nav, index: nav.index + 1 }
}

export function historyItem(nav: HistoryNavigation): InputHistoryItem {
  return nav.items[nav.index] ?? { text: '' }
}

export type HistoryDirection = 'older' | 'newer'
export interface KeyModifiers {
  shiftKey?: boolean
  metaKey?: boolean
  ctrlKey?: boolean
  altKey?: boolean
}

/**
 * 键盘判定：返回 'older' / 'newer' 表示这个 ↑/↓ 该用来选历史；null = 保留原生光标行为。
 *
 * 规则：
 *   · 组合键（Shift/Cmd/Ctrl/Alt）一律不碰 —— Shift+↑ 是选区、Alt+↑ 是行移动；
 *   · 输入法组合态不碰（中文候选框里的 ↑↓ 是选字）；
 *   · 未进入历史模式时只有**光标在开头**的 ↑ 才取历史（多行正文里 ↑ 必须能移动光标）；
 *   · 已进入历史模式后 ↑↓ 就是历史选择，↓ 越过最新由调用方恢复草稿。
 */
export function historyDirection(
  key: string,
  mods: KeyModifiers,
  composing: boolean,
  selectionStart: number,
  selectionEnd: number,
  browsing: boolean,
): HistoryDirection | null {
  if (composing) return null
  if (mods.shiftKey || mods.metaKey || mods.ctrlKey || mods.altKey) return null
  if (key === 'ArrowUp') {
    if (browsing) return 'older'
    return selectionStart === 0 && selectionEnd === 0 ? 'older' : null
  }
  if (key === 'ArrowDown') return browsing ? 'newer' : null
  return null
}

/**
 * 合并「文件里的（remote）」与「本地镜像（local）」：按内容去重，取 at 较新的那份；
 * 再按 at 升序（旧→新），截到 100。缺 at 的旧数据视为 0（最旧），不会压掉新记录。
 */
export function mergeInputHistories(
  remote: readonly InputHistoryItem[],
  local: readonly InputHistoryItem[],
): InputHistoryItem[] {
  const byKey = new Map<string, InputHistoryItem>()
  for (const item of remote) {
    const clean = normalizeHistoryItem(item)
    if (clean) byKey.set(itemKey(clean), clean)
  }
  for (const item of local) {
    const clean = normalizeHistoryItem(item)
    if (!clean) continue
    const key = itemKey(clean)
    const prev = byKey.get(key)
    if (!prev || (clean.at ?? 0) > (prev.at ?? 0)) byKey.set(key, clean)
  }
  return [...byKey.values()]
    .sort((a, b) => (a.at ?? 0) - (b.at ?? 0))
    .slice(-INPUT_HISTORY_MAX)
}

function copyItem(item: InputHistoryItem): InputHistoryItem {
  return {
    text: item.text,
    ...(item.at != null ? { at: item.at } : {}),
    ...(item.refs ? { refs: item.refs.map((r) => ({ ...r })) } : {}),
    ...(item.images ? { images: item.images.map((i) => ({ ...i })) } : {}),
  }
}

/**
 * 可注入存储的历史仓库（单测用内存 KV；运行时用 localStorage）。
 * 读盘做了 raw 字符串缓存：↑ 连按时不会反复 parse 整份 JSON。
 */
export class InputHistoryStore {
  private cacheRaw: string | null | undefined
  private cache: InputHistoryState = emptyInputHistory()

  constructor(private readonly kv: InputHistoryStorage | null) {}

  private load(): InputHistoryState {
    if (!this.kv) return emptyInputHistory()
    let raw: string | null
    try {
      raw = this.kv.getItem(LS_KEY)
    } catch {
      return emptyInputHistory()
    }
    if (this.cacheRaw !== undefined && raw === this.cacheRaw) return this.cache
    this.cacheRaw = raw
    this.cache = parseInputHistory(raw)
    return this.cache
  }

  private save(state: InputHistoryState): void {
    if (!this.kv) return
    try {
      const raw = JSON.stringify(state)
      this.kv.setItem(LS_KEY, raw)
      this.cacheRaw = raw
      this.cache = state
    } catch {
      // 配额/隐私模式：写入失败不致命（这条历史只在本次落盘时丢失）。
      // 下次读盘会回到旧值，因此这里不保留内存副本，避免「看起来记下了、其实没有」。
      this.cacheRaw = undefined
      this.cache = emptyInputHistory()
    }
  }

  get(scope: string): InputHistoryItem[] {
    return (this.load().scopes[scope]?.items ?? []).map(copyItem)
  }

  add(scope: string, item: InputHistoryItem, now = Date.now()): InputHistoryItem[] {
    // 新发送的条目标上时间：远端文件与本地镜像合并时靠它排序 / 判新旧。
    const entry = normalizeHistoryItem({ ...item, at: now })
    if (!entry) return this.get(scope)
    const state = this.load()
    const scopes = { ...state.scopes }
    scopes[scope] = {
      t: now,
      items: appendInputHistory(scopes[scope]?.items ?? [], entry),
    }
    const next = pruneInputHistory({ v: VERSION, scopes })
    this.save(next)
    return (next.scopes[scope]?.items ?? []).map(copyItem)
  }

  /** 用远端（文件）结果替换本地镜像；用于启动/切会话时对齐。 */
  replace(scope: string, items: readonly InputHistoryItem[], now = Date.now()): InputHistoryItem[] {
    const state = this.load()
    const scopes = { ...state.scopes }
    const normalized = items
      .map((x) => normalizeHistoryItem(x))
      .filter((x): x is InputHistoryItem => Boolean(x))
      .slice(-INPUT_HISTORY_MAX)
    if (normalized.length) scopes[scope] = { t: now, items: normalized }
    else delete scopes[scope]
    const next = pruneInputHistory({ v: VERSION, scopes })
    this.save(next)
    return (next.scopes[scope]?.items ?? []).map(copyItem)
  }

  /** 把无会话时的 workspace 历史并进刚建立的会话（第一条消息也能 ↑ 回来）。 */
  adopt(from: string, to: string, now = Date.now()): void {
    if (from === to) return
    const state = this.load()
    const source = state.scopes[from]
    if (!source?.items.length) return
    const scopes = { ...state.scopes }
    let items = [...(scopes[to]?.items ?? [])]
    for (const item of source.items) items = appendInputHistory(items, item)
    scopes[to] = { t: now, items }
    delete scopes[from]
    this.save(pruneInputHistory({ v: VERSION, scopes }))
  }

  /**
   * 删除会话时回收：prefix 本身或其 NUL 子作用域（子智能体）全部清掉。
   * NUL 边界保证 s1 不会误删 s10 的历史。
   */
  forget(prefix: string): void {
    const state = this.load()
    const scopes = { ...state.scopes }
    let changed = false
    for (const key of Object.keys(scopes)) {
      if (key === prefix || key.startsWith(`${prefix}\u0000`)) {
        delete scopes[key]
        changed = true
      }
    }
    if (changed) this.save({ v: VERSION, scopes })
  }

  /** 删除工作区时回收该工作区下**全部**会话 / 子智能体 / 无会话历史。 */
  forgetWorkspace(workspace: string): void {
    const prefix = `${workspace}\u0000`
    const state = this.load()
    const scopes = { ...state.scopes }
    let changed = false
    for (const key of Object.keys(scopes)) {
      if (key.startsWith(prefix)) {
        delete scopes[key]
        changed = true
      }
    }
    if (changed) this.save({ v: VERSION, scopes })
  }
}

function defaultStorage(): InputHistoryStorage | null {
  try {
    return (globalThis as { localStorage?: InputHistoryStorage }).localStorage ?? null
  } catch {
    return null // 隐私模式/禁用 storage
  }
}

/** 应用单例（模块加载时解析一次 localStorage；渲染进程里始终可用）。 */
export const inputHistoryStore = new InputHistoryStore(defaultStorage())

export function readInputHistory(scope: string): InputHistoryItem[] {
  return inputHistoryStore.get(scope)
}

export function recordInputHistory(scope: string, item: InputHistoryItem): InputHistoryItem[] {
  return inputHistoryStore.add(scope, item)
}

export function replaceInputHistory(scope: string, items: readonly InputHistoryItem[]): InputHistoryItem[] {
  return inputHistoryStore.replace(scope, items)
}

export function adoptInputHistory(from: string, to: string): void {
  inputHistoryStore.adopt(from, to)
}

export function forgetInputHistory(prefix: string): void {
  inputHistoryStore.forget(prefix)
}

export function forgetInputHistoryWorkspace(workspace: string): void {
  inputHistoryStore.forgetWorkspace(workspace)
}
