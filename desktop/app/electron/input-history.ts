// input-history.ts —— 输入框历史的**本地文件**持久化（Electron 主进程）。
//
// 为什么还要文件：localStorage 在 Electron 里确实能跨重启（userData/LevelDB），
// 但会被「清除站点数据 / dev 与打包版 origin 不同 / 换安装」抹掉。输入历史是用户
// 亲手发过的话，值得和 settings.json 一样落一份可备份、可检查的文件：
//
//   ~/.go-code/input-history.json
//   { "v": 1, "scopes": { "<workspace>\u0000<sid>": { "t": 179…, "items": [...] }, … } }
//
// scope 由渲染层拼（与 localStorage 同 key），主进程只当字符串键；与会话一一对应，
// 删会话/删工作区时按前缀回收。图片仍只存指纹，不存 data URL（见 lib/inputHistory.ts）。
import { mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

export const INPUT_HISTORY_FILE = 'input-history.json'
export const INPUT_HISTORY_MAX = 100
export const INPUT_HISTORY_MAX_SCOPES = 40
export const INPUT_HISTORY_ENTRY_CHARS = 100_000
export const INPUT_HISTORY_SCOPE_CHARS = 200_000
export const INPUT_HISTORY_TOTAL_CHARS = 1_500_000

export interface InputHistoryRef { path: string; name: string; size: number }
export interface InputHistoryImage { mime: string; len: number; hash: string }
export interface InputHistoryItem {
  text: string
  at?: number
  refs?: InputHistoryRef[]
  images?: InputHistoryImage[]
}
interface ScopeHistory { t: number; items: InputHistoryItem[] }
export interface InputHistoryFile { v: number; scopes: Record<string, ScopeHistory> }

export function emptyInputHistoryFile(): InputHistoryFile {
  return { v: 1, scopes: {} }
}

/** 历史文件路径（与 settings.json 同一配置根，便于用户备份/检查）。 */
export function inputHistoryFilePath(cfgDir: string): string {
  return join(cfgDir, INPUT_HISTORY_FILE)
}

function isRef(x: unknown): x is InputHistoryRef {
  if (!x || typeof x !== 'object') return false
  const r = x as { path?: unknown; name?: unknown; size?: unknown }
  return typeof r.path === 'string' && typeof r.name === 'string' && typeof r.size === 'number'
}

function isImage(x: unknown): x is InputHistoryImage {
  if (!x || typeof x !== 'object') return false
  const i = x as { mime?: unknown; len?: unknown; hash?: unknown }
  return typeof i.mime === 'string' && typeof i.len === 'number' && typeof i.hash === 'string'
}

/** 与渲染层同口径：trim 正文、丢弃非法 refs/images、空条目拒收、单条超限拒收。 */
export function normalizeInputHistoryItem(raw: unknown): InputHistoryItem | undefined {
  if (!raw || typeof raw !== 'object') return undefined
  const o = raw as { text?: unknown; at?: unknown; refs?: unknown; images?: unknown }
  const text = typeof o.text === 'string' ? o.text.trim() : ''
  const refs = Array.isArray(o.refs) ? o.refs.filter(isRef).map((r) => ({ path: r.path, name: r.name, size: r.size })) : []
  const images = Array.isArray(o.images) ? o.images.filter(isImage).map((i) => ({ mime: i.mime, len: i.len, hash: i.hash })) : []
  if (!text && !refs.length && !images.length) return undefined
  const at = typeof o.at === 'number' && Number.isFinite(o.at) ? o.at : undefined
  const item: InputHistoryItem = {
    text,
    ...(at != null ? { at } : {}),
    ...(refs.length ? { refs } : {}),
    ...(images.length ? { images } : {}),
  }
  if (JSON.stringify(item).length > INPUT_HISTORY_ENTRY_CHARS) return undefined
  return item
}

/** 去重（内容相同只留一条）+ 截到 100 条；保留调用方给的时间。 */
export function normalizeInputHistoryItems(raw: unknown): InputHistoryItem[] {
  if (!Array.isArray(raw)) return []
  const out: InputHistoryItem[] = []
  const seen = new Set<string>()
  for (const x of raw) {
    const item = normalizeInputHistoryItem(x)
    if (!item) continue
    const key = itemKey(item)
    if (seen.has(key)) continue
    seen.add(key)
    out.push(item)
  }
  return out.slice(-INPUT_HISTORY_MAX)
}

/** 条目身份 = 内容（不算 at；同一句话重发应移到最新，而不是并存两条）。 */
export function itemKey(item: InputHistoryItem): string {
  return JSON.stringify([item.text, item.refs ?? [], item.images ?? []])
}

function charsOf(items: readonly InputHistoryItem[]): number {
  let n = 0
  for (const item of items) n += itemKey(item).length
  return n
}

/** 容量裁剪：单条/单作用域/作用域数量/全局字符预算（与渲染层同口径）。 */
export function pruneInputHistoryFile(file: InputHistoryFile): InputHistoryFile {
  const scopes: Record<string, ScopeHistory> = {}
  for (const [key, value] of Object.entries(file.scopes)) {
    let items = normalizeInputHistoryItems(value.items)
    let chars = charsOf(items)
    let drop = 0
    while (chars > INPUT_HISTORY_SCOPE_CHARS && drop < items.length) {
      chars -= itemKey(items[drop]).length
      drop++
    }
    items = items.slice(drop)
    if (items.length) scopes[key] = { t: Number.isFinite(value.t) ? value.t : 0, items }
  }
  const keys = Object.keys(scopes)
  if (keys.length > INPUT_HISTORY_MAX_SCOPES) {
    keys.sort((a, b) => scopes[a].t - scopes[b].t)
    for (const key of keys.slice(0, keys.length - INPUT_HISTORY_MAX_SCOPES)) delete scopes[key]
  }
  let total = 0
  for (const v of Object.values(scopes)) total += charsOf(v.items)
  while (total > INPUT_HISTORY_TOTAL_CHARS) {
    const oldest = Object.keys(scopes).sort((a, b) => scopes[a].t - scopes[b].t)[0]
    if (!oldest) break
    total -= charsOf(scopes[oldest].items)
    delete scopes[oldest]
  }
  return { v: 1, scopes }
}

/** 宽松解析：坏 JSON / 未知版本 / 非法条目 → 空或丢弃（历史不是关键数据，不能炸主进程）。 */
export function parseInputHistoryFile(raw: string | null): InputHistoryFile {
  if (!raw) return emptyInputHistoryFile()
  let obj: unknown
  try {
    obj = JSON.parse(raw)
  } catch {
    return emptyInputHistoryFile()
  }
  const rec = obj as { v?: unknown; scopes?: unknown } | null
  if (!rec || rec.v !== 1 || !rec.scopes || typeof rec.scopes !== 'object') return emptyInputHistoryFile()
  const scopes: Record<string, ScopeHistory> = {}
  for (const [key, value] of Object.entries(rec.scopes as Record<string, unknown>)) {
    if (!key || !value || typeof value !== 'object') continue
    const entry = value as { t?: unknown; items?: unknown }
    const items = normalizeInputHistoryItems(entry.items)
    if (!items.length) continue
    scopes[key] = { t: typeof entry.t === 'number' && Number.isFinite(entry.t) ? entry.t : 0, items }
  }
  return pruneInputHistoryFile({ v: 1, scopes })
}

export function getInputHistory(file: InputHistoryFile, scope: string): InputHistoryItem[] {
  return [...(file.scopes[scope]?.items ?? [])]
}

/** 替换某作用域的完整列表（渲染层负责 append/去重；这里再兜一次容量）。 */
export function setInputHistory(file: InputHistoryFile, scope: string, items: unknown, now = Date.now()): InputHistoryFile {
  const scopes = { ...file.scopes }
  const normalized = normalizeInputHistoryItems(items)
  if (!normalized.length) delete scopes[scope]
  else scopes[scope] = { t: now, items: normalized }
  return pruneInputHistoryFile({ v: 1, scopes })
}

/**
 * 删除：默认精确匹配 scope 或其 NUL 子作用域（删会话时带它的子智能体）；
 * subtree=true 时按前缀清整棵（删工作区：prefix = `<workspace>\0`）。
 */
export function forgetInputHistory(file: InputHistoryFile, prefix: string, subtree = false): InputHistoryFile {
  if (!prefix) return file
  const scopes = { ...file.scopes }
  for (const key of Object.keys(scopes)) {
    if (key === prefix || key.startsWith(`${prefix}\u0000`) || (subtree && key.startsWith(prefix))) {
      delete scopes[key]
    }
  }
  return { v: 1, scopes }
}

export function readInputHistoryFile(filePath: string): InputHistoryFile {
  try {
    return parseInputHistoryFile(readFileSync(filePath, 'utf8'))
  } catch {
    return emptyInputHistoryFile()
  }
}

/** 原子写（temp + rename）；失败抛错，由 IPC 转成 ok:false，渲染层继续用 localStorage。 */
export function writeInputHistoryFile(filePath: string, file: InputHistoryFile): void {
  mkdirSync(dirname(filePath), { recursive: true, mode: 0o700 })
  const tmp = `${filePath}.${process.pid}.${Date.now()}.tmp`
  try {
    writeFileSync(tmp, JSON.stringify(file), { encoding: 'utf8', mode: 0o600 })
    renameSync(tmp, filePath)
  } catch (e) {
    try { rmSync(tmp, { force: true }) } catch { /* 清理失败不掩盖原错误 */ }
    throw e
  }
}

// —— IPC 用的薄封装（main.ts 每个 handler 一行） ——

export function ipcInputHistoryGet(filePath: string, scope: string): { ok: boolean; items?: InputHistoryItem[]; error?: string } {
  try {
    return { ok: true, items: getInputHistory(readInputHistoryFile(filePath), scope) }
  } catch (e) {
    return { ok: false, error: (e as Error).message }
  }
}

export function ipcInputHistorySet(filePath: string, scope: string, items: unknown): { ok: boolean; items?: InputHistoryItem[]; error?: string } {
  try {
    const next = setInputHistory(readInputHistoryFile(filePath), scope, items)
    writeInputHistoryFile(filePath, next)
    return { ok: true, items: getInputHistory(next, scope) }
  } catch (e) {
    return { ok: false, error: (e as Error).message }
  }
}

export function ipcInputHistoryForget(filePath: string, prefix: string, subtree = false): { ok: boolean; error?: string } {
  try {
    const before = readInputHistoryFile(filePath)
    const next = forgetInputHistory(before, prefix, subtree)
    if (Object.keys(next.scopes).length !== Object.keys(before.scopes).length) writeInputHistoryFile(filePath, next)
    return { ok: true }
  } catch (e) {
    return { ok: false, error: (e as Error).message }
  }
}
