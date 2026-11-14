// 手机会话清单的数据层（P8-02）：工作区 + 会话，只做「取 + 归一 + 排序 + 文案」。
//
// 为什么手机不能直接用 store 的 projects：那份项目树来自桌面 localStorage
// （useAppStore 模块初始化时读 hai.projects）。手机首次配对时它**是空的** ——
// 用户会看到一个没有任何工作区的列表，直到有人先在桌面上开过项目。真相源在
// bridge：workspace_list（~/.go-code/workspaces.json）与 list（某工作区的会话）。
// 见 bridge/main.go 的 CmdWorkspaceList / CmdList 与 workspace_registry.go。
//
// 为什么在这里直连 transport 而不走 store 的 send()：
//   · store 的 send() 是模块私有；公开动作里只有 setActiveWorkspace 会发 list，但它
//     顺带发 skills + git_snapshot + git_history —— 这三条都**不在**手机 bridge 的
//     allowlist 里（desktop/bridge/mobile_server.go 的 mobileAllowedCommands），发出去
//     只会拿 403 并把 bridgeError 染红。
//   · 这里只要两条只读命令，所以显式发、显式收（按命令 id 对账），失败原样返回给
//     列表屏渲染成一条可读横幅。
//
// 与 store 的唯一交点仍然是 setActiveSession(sid, ws)（在会话屏里调用）：激活、恢复、
// 事件订阅全部走桌面同一套路径 —— 手机不另建一份会话状态。
import type { BridgeCommand } from '../store/events'
import type { Transport } from '../transport/types'

/** bridge workspace_registry.go 的 workspaceEntry。 */
export interface MobileWorkspace {
  path: string
  /** 用户起的别名；空串 = 没起，用路径尾段显示。 */
  title: string
  pinned: boolean
  /** 最近打开时刻（unix ms）；0/undefined = 未知。 */
  lastOpened?: number
}

/** bridge listSessions 的一行（定时任务派生会话不出现在这里）。 */
export interface MobileSessionRow {
  id: string
  title: string
  /** 最近活动时刻（unix ms）。 */
  updatedAt?: number
  pinned: boolean
  /** 进程内正在跑（bridge 的 bs.running || hubDispatchPending）。 */
  running: boolean
}

/** 上次选中的工作区（旧手机页同款 key：升级不丢选择）。 */
export const MOBILE_WS_KEY = 'hai.mobile.ws'

// 直连 transport 的命令 id 从 20 亿起：store 的 cmdSeq 从 0 递增（要跑 20 亿条命令才撞），
// bridge 自己的手机侧 nextID 是 10 亿 —— 三段互不重叠，合成响应不会串到别人的 pending 上。
const REQ_ID_BASE = 2_000_000_000
let reqSeq = 0

/** 只取需要的那两个方法：测试可以直接塞一个假 transport。 */
type Sender = Pick<Transport, 'send' | 'onEvent'>

export interface FetchResult<T> {
  ok: boolean
  error?: string
  items: T[]
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : ''
}

function num(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined
}

/** workspace_list → 工作区行（保持 bridge 的排序：置顶 → 最近打开 → 路径）。 */
export function parseWorkspaces(data: unknown): MobileWorkspace[] {
  const list = (data as { workspaces?: unknown } | undefined)?.workspaces
  if (!Array.isArray(list)) return []
  const out: MobileWorkspace[] = []
  for (const raw of list) {
    const o = (raw ?? {}) as Record<string, unknown>
    const path = str(o.path).trim()
    if (!path) continue
    out.push({
      path,
      title: str(o.title).trim(),
      pinned: o.pinned === true,
      lastOpened: num(o.last_opened) ?? num(o.lastOpened),
    })
  }
  return out
}

/** list → 会话行（bridge 已排好序，这里只归一 + 兜底排序）。 */
export function parseSessions(data: unknown): MobileSessionRow[] {
  const list = (data as { sessions?: unknown } | undefined)?.sessions
  if (!Array.isArray(list)) return []
  const out: MobileSessionRow[] = []
  for (const raw of list) {
    const o = (raw ?? {}) as Record<string, unknown>
    const id = str(o.id).trim()
    if (!id) continue
    out.push({
      id,
      title: str(o.title).trim() || id,
      updatedAt: num(o.updated_at) ?? num(o.updatedAt),
      pinned: o.pinned === true,
      running: o.running === true,
    })
  }
  return out
}

/**
 * 会话排序：置顶在前，然后按最近活动降序；都没有时间戳时按 id 降序
 * （bridge 的 genID 单调递增 ≈ 创建逆序）。与 store 的 sortSessions 同口径，
 * 但多一条 pinned 优先 —— 手机列表没有分组标题，置顶必须真的顶到最上面。
 */
export function sortSessionRows(list: MobileSessionRow[]): MobileSessionRow[] {
  return [...list].sort((a, b) => {
    if (a.pinned !== b.pinned) return a.pinned ? -1 : 1
    const ta = a.updatedAt ?? 0
    const tb = b.updatedAt ?? 0
    if (ta !== tb) return tb - ta
    return a.id === b.id ? 0 : a.id < b.id ? 1 : -1
  })
}

/**
 * 默认工作区：优先上次选中的（还在清单里才算），否则第一条，空清单 → 空串。
 * 不做「猜测最近打开」—— bridge 已经把清单排好序了，再猜一次就是两套规则。
 */
export function pickWorkspace(list: MobileWorkspace[], saved?: string): string {
  if (saved && list.some((w) => w.path === saved)) return saved
  return list[0]?.path ?? ''
}

/** 路径尾段：手机上完整路径太长，尾段才认得出是哪个仓库。 */
export function pathTail(path: string): string {
  return path.replace(/[\\/]+$/, '').split(/[\\/]/).pop() || ''
}

/** 工作区显示名：别名优先，否则路径尾段。 */
export function workspaceLabel(w: MobileWorkspace, lang: 'zh' | 'en' = 'zh'): string {
  if (w.title) return w.title
  return pathTail(w.path) || (lang === 'en' ? 'Untitled workspace' : '未命名工作区')
}

/** 会话显示名：bridge 一般已给标题；空标题在 parseSessions 里已经回落成 id。 */
export function sessionLabel(s: MobileSessionRow, lang: 'zh' | 'en' = 'zh'): string {
  return s.title || (lang === 'en' ? 'Untitled session' : '未命名会话')
}

/**
 * 相对时间（列表行右侧那格）。手机列表只关心「刚刚/几分钟前/几小时前/几天前」，
 * 超过一周就给日期 —— 精确到秒在这里没有意义，反而更难看。
 */
export function fmtSessionTime(ms: number | undefined, now: number = Date.now(), lang: 'zh' | 'en' = 'zh'): string {
  if (!ms || !Number.isFinite(ms) || ms <= 0) return ''
  const diff = now - ms
  if (diff < 60_000) return lang === 'en' ? 'just now' : '刚刚'
  const min = Math.floor(diff / 60_000)
  if (min < 60) return lang === 'en' ? `${min}m ago` : `${min} 分钟前`
  const hour = Math.floor(min / 60)
  if (hour < 24) return lang === 'en' ? `${hour}h ago` : `${hour} 小时前`
  const day = Math.floor(hour / 24)
  if (day < 7) return lang === 'en' ? `${day}d ago` : `${day} 天前`
  const d = new Date(ms)
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  return lang === 'en' ? `${d.getFullYear()}-${mm}-${dd}` : `${mm}-${dd}`
}

export function readSavedWorkspace(): string {
  try {
    return (localStorage.getItem(MOBILE_WS_KEY) ?? '').trim()
  } catch {
    /* 隐私模式：当作没选过 */
    return ''
  }
}

export function writeSavedWorkspace(path: string): void {
  try {
    localStorage.setItem(MOBILE_WS_KEY, path)
  } catch {
    /* 写不进去只影响下次打开 */
  }
}

/**
 * 发一条只读命令并按 id 收自己的响应。
 *
 * 为什么要自己订阅：手机 transport 的 send() 只回 {ok,error} —— 数据走的是合成的
 * command_response 事件（见 transport/mobile.ts 的 send）。同一帧 store 也会收到
 * （onEvent 是多订阅），但那条响应对 store 是「未知命令」（pending 里没有这个 id），
 * 只有 ok:false 才会落一行 bridgeError —— 所以失败时这里自己给列表屏一个可读横幅。
 */
export async function request<T = unknown>(
  t: Sender,
  type: string,
  payload: Record<string, unknown> = {},
  timeoutMs = 15_000,
): Promise<{ ok: boolean; error?: string; data?: T }> {
  const id = REQ_ID_BASE + ++reqSeq
  const cmd: BridgeCommand = { id, type, payload }
  return new Promise((resolve) => {
    let done = false
    let off: (() => void) | undefined
    const finish = (r: { ok: boolean; error?: string; data?: T }): void => {
      if (done) return
      done = true
      if (off) off()
      clearTimeout(timer)
      resolve(r)
    }
    const timer = setTimeout(() => finish({ ok: false, error: '请求超时（服务端没有响应）' }), timeoutMs)
    off = t.onEvent((line) => {
      let ev: { event_type?: string; id?: unknown; ok?: unknown; error?: unknown; data?: unknown }
      try {
        ev = JSON.parse(line) as typeof ev
      } catch {
        return // 非 JSON 行（bridge 日志）
      }
      if (ev.event_type !== 'command_response' || Number(ev.id) !== id) return
      if (ev.ok === false) finish({ ok: false, error: str(ev.error) || '命令失败' })
      else finish({ ok: true, data: ev.data as T })
    })
    void t
      .send(cmd)
      .then((res) => {
        // transport 层失败（未配对 / 网络断）不会再合成事件 → 这里收口，不让列表屏干等。
        if (res && res.ok === false) finish({ ok: false, error: res.error || '发送失败' })
      })
      .catch((e: unknown) => finish({ ok: false, error: e instanceof Error ? e.message : String(e) }))
  })
}

/** 工作区清单（bridge 的 workspaces.json 是真相源；本地 localStorage 只是上次选择）。 */
export async function fetchWorkspaces(t: Sender): Promise<FetchResult<MobileWorkspace>> {
  const r = await request(t, 'workspace_list')
  if (!r.ok) return { ok: false, error: r.error, items: [] }
  return { ok: true, items: parseWorkspaces(r.data) }
}

/** 某工作区的会话清单。 */
export async function fetchSessions(t: Sender, ws: string): Promise<FetchResult<MobileSessionRow>> {
  if (!ws) return { ok: true, items: [] }
  const r = await request(t, 'list', { workspace: ws })
  if (!r.ok) return { ok: false, error: r.error, items: [] }
  return { ok: true, items: sortSessionRows(parseSessions(r.data)) }
}
