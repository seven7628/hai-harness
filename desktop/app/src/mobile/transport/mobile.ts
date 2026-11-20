// 手机端 Transport：没有 Electron 主进程，只有 bridge 的 HTTP/WS 面
// （desktop/bridge/mobile_server.go：/api/cmd、/api/events、/api/ping、/api/pair）。
//
// 三条契约（P8-01a，见 docs/agent-hub/MOBILE-UI-REUSE.md §1「四个真正阻塞」）：
//
//   1. `send()` → POST /api/cmd，**bridge 的响应只走这条 HTTP 应答，不会从 WS 回来**
//      （handleCmd 直接 writeMobileJSON(resp)）。而 hub/api.ts 的 hubCommand 是订阅
//      `command_response` 事件等结果的 —— 所以这里必须在应答后自己合成一帧
//      `{event_type:'command_response', id, ok, data, error}` 再分发给 onEvent 订阅者，
//      否则手机上的 Hub 每条命令都要等满 20s 超时。
//   2. `onEvent` 支持多订阅者；chat / hub 两条 WS（scope 见下）各自断线重连，
//      帧内容逐字节相同（3s 内）只投一次 —— Safari 息屏后旧连接半死不活时，
//      两条连接同时活着会把同一帧投两遍（旧页实测「add add a a」）。
//   3. Transport 有 60+ 方法，手机只实现核心几个，其余统一降级
//      `{ok:false, error:'unsupported: <method>'}`，**绝不抛未捕获异常**。
//
// WS 的 scope 口径（与任务书里写的 `scope=chat` 不同，以服务端为准）：
// mobile_server.go handleEvents 只接受 scope 为空（会话流）或 `hub`，
// 传 `scope=chat` 会直接 400。所以 chat 连接**不带 scope**、按 workspace+session_id 过滤，
// hub 连接带 `scope=hub` —— 与旧页 app.js 的 connect()/connectHub() 一致。
//
// 依赖全部可注入（fetch / WebSocket / storage / location / 计时器）：Node 单测
// （scripts/test-mobile-transport.mjs）直接喂 mock，不在模块顶层碰任何浏览器全局。
import type { Transport, AppSettings } from '../../transport/types'
import type { BridgeCommand } from '../../store/events'

// 凭据 key：P8 之前的手机页用 `hai.token` 存配对令牌，沿用它是为了
// 「升级前配对过的手机不用再扫一次码」；`hai.mobile.token` 是备用 key。
export const TOKEN_KEYS = ['hai.token', 'hai.mobile.token'] as const
export const SETTINGS_KEY = 'hai.mobile.settings'
// 旧手机页的主题偏好 key（app.js LS.theme：system/ink/paper）。新手机页沿用同一个
// key，旧页配对/设过主题的用户升级后不掉档；桌面端不用 localStorage 存主题
// （settings.json + lib/appearance.ts），所以这里只做手机侧的默认值推导。
export const THEME_KEY = 'hai.theme'

export type MobileLinkState = 'idle' | 'connecting' | 'open' | 'closed' | 'unauthorized'
export type MobileLink = 'chat' | 'hub'

export interface MobileStatus {
  chat: MobileLinkState
  hub: MobileLinkState
  /** 最近一次可读错误（401/网络错误/服务端 error）；连上后不清空，由调用方展示。 */
  lastError?: string
}

export interface MobileReply<T = unknown> {
  ok: boolean
  error?: string
  code?: string
  data?: T
  /** 服务端 401：凭据失效。**不清 token**，只置状态，由用户显式重新配对覆盖。 */
  unauthorized?: boolean
}

/** 依赖注入点（默认取浏览器全局；Node 单测注入 mock）。 */
export interface MobileDeps {
  fetch?: typeof fetch
  WebSocket?: MobileWebSocketCtor
  storage?: Pick<Storage, 'getItem' | 'setItem'>
  location?: { protocol: string; host: string }
  now?: () => number
  setTimeout?: (fn: () => void, ms: number) => unknown
  clearTimeout?: (handle: unknown) => void
  /** 重连指数退避上限（毫秒），默认 15s（与旧页一致）。 */
  maxRetryDelay?: number
}

/** 只用到 WebSocket 的这一小块（避免把 DOM 的 WebSocket 类型硬编码进依赖）。 */
export interface MobileWebSocket {
  onopen: (() => void) | null
  onmessage: ((ev: { data: unknown }) => void) | null
  onclose: (() => void) | null
  onerror: ((ev?: unknown) => void) | null
  close(): void
}
export type MobileWebSocketCtor = new (url: string) => MobileWebSocket

export interface MobileTransport extends Transport {
  /** 打开会话事件流（bridge 的会话流不带 scope，见文件头）。 */
  connectChat(workspace: string, sessionId: string): void
  /** 打开 Hub 事件流（scope=hub，只收 hub_*，不绑会话）。 */
  connectHub(): void
  disconnect(link?: MobileLink): void
  status(): MobileStatus
  onStatus(cb: (s: MobileStatus) => void): () => void
  /** 当前凭据（空串 = 未配对）。 */
  token(): string
  /** 配对成功后写入（显式动作；transport 自己绝不删 token）。 */
  setToken(token: string): void
  /** 凭据自检：GET /api/ping → {paired}。网络错误也返回可读 error，不抛。 */
  ping(): Promise<MobileReply<{ paired: boolean }>>
  /** 配对：POST /api/pair（6 位码 + 设备名）→ 写 token。 */
  pair(code: string, name: string): Promise<MobileReply<{ token: string }>>
}

const WS_FRAME_DEDUPE_MS = 3000
const WS_FRAME_MAP_MAX = 600

function errText(e: unknown): string {
  if (e instanceof Error) return e.message
  if (typeof e === 'string') return e
  return String(e ?? '未知错误')
}

function readTokenFrom(storage: MobileDeps['storage']): string {
  if (!storage) return ''
  for (const key of TOKEN_KEYS) {
    try {
      const v = (storage.getItem(key) ?? '').trim()
      if (v) return v
    } catch {
      /* 隐私模式/被禁用的 storage：当作没有，继续下一个 key */
    }
  }
  return ''
}

/** 主题默认值：沿用旧页 `hai.theme`（system/ink/paper）→ 桌面 ThemePref（dark/light/system）。 */
function themeFromLegacy(storage: MobileDeps['storage']): AppSettings['theme'] {
  let raw: string
  try {
    raw = (storage?.getItem(THEME_KEY) ?? '').trim()
  } catch {
    raw = ''
  }
  if (raw === 'paper' || raw === 'light') return 'light'
  if (raw === 'ink' || raw === 'dark') return 'dark'
  return 'system'
}

function defaultSettings(storage: MobileDeps['storage']): AppSettings {
  // 手机端只用到 theme/lang（P8-01a 的壳与 P8-01b 的 Hub store 都不读其余字段）。
  // 其余 60+ 字段由桌面 settings.json 持有，这里不复制一份默认表 —— 复制必然漂移。
  return { theme: themeFromLegacy(storage), lang: 'zh', provider: { active: '' } } as unknown as AppSettings
}

// 进程内单例：transport/index.ts 的 getTransport() 与手机壳必须拿到**同一个**实例，
// 否则两边各开一套 WS、各存一份订阅表（hubCommand 会等不到自己那条 command_response）。
// 惰性创建：模块求值时不碰任何浏览器全局（Node 单测 import 这个模块是安全的）。
let shared: MobileTransport | null = null

export function sharedMobileTransport(): MobileTransport {
  if (!shared) shared = mobileTransport()
  return shared
}

export function mobileTransport(deps: MobileDeps = {}): MobileTransport {
  const now = deps.now ?? (() => Date.now())
  const setTimer = deps.setTimeout ?? ((fn: () => void, ms: number) => setTimeout(fn, ms))
  const clearTimer = deps.clearTimeout ?? ((h: unknown) => clearTimeout(h as ReturnType<typeof setTimeout>))
  const maxRetryDelay = deps.maxRetryDelay ?? 15_000

  const storage = deps.storage ?? (typeof localStorage !== 'undefined' ? localStorage : undefined)
  const fetchImpl = deps.fetch ?? (typeof fetch !== 'undefined' ? fetch.bind(globalThis) : undefined)

  // 内存兜底：storage 不可写（隐私模式）时，至少本次会话内 token 还在。
  let memToken = ''

  const subs = new Set<(line: string) => void>()
  const statusSubs = new Set<(s: MobileStatus) => void>()
  const status: MobileStatus = { chat: 'idle', hub: 'idle' }

  function token(): string {
    const t = readTokenFrom(storage)
    return t || memToken
  }

  function setToken(t: string): void {
    const v = (t ?? '').trim()
    memToken = v
    try {
      storage?.setItem(TOKEN_KEYS[0], v)
    } catch {
      /* 写不进去就只留内存，不抛 */
    }
  }

  function publish(next: Partial<MobileStatus>): void {
    Object.assign(status, next)
    const snapshot = { ...status }
    for (const cb of statusSubs) {
      try {
        cb(snapshot)
      } catch {
        /* 订阅者自己的异常不能影响连接状态机 */
      }
    }
  }

  function emit(line: string): void {
    for (const cb of subs) {
      try {
        cb(line)
      } catch (e) {
        // 一个订阅者炸掉不能让其他订阅者（以及 hubCommand 的等待）丢帧。
        console.warn('[mobile transport] onEvent 订阅者异常：', errText(e))
      }
    }
  }

  // ---------- HTTP ----------

  // whole=true：服务端直接返回业务字段（/api/ping 的 {paired}、/api/pair 的 {token}），
  // 没有 `data` 信封 —— 这时把整个 body 当 data，而不是丢掉。
  async function httpJson<T>(path: string, init: RequestInit, opts: { whole?: boolean } = {}): Promise<MobileReply<T>> {
    if (!fetchImpl) return { ok: false, error: '当前环境没有 fetch，无法连接 bridge' }
    let res: Response
    try {
      res = await fetchImpl(path, init)
    } catch (e) {
      // 网络层失败（离线/服务重启/证书）：可读、可重试，不动 token。
      return { ok: false, error: `网络错误：${errText(e)}` }
    }
    let body: Record<string, unknown>
    try {
      body = (await res.json()) as Record<string, unknown>
    } catch {
      body = {}
    }
    const unauthorized = res.status === 401
    if (unauthorized) {
      // 凭据被撤销：不清 token（用户要能看到「已配对但失效」，再显式重新配对覆盖）。
      // Hub 事件流若还没连上/正在重连，直接停在 unauthorized，别再每 15s 去撞墙。
      const hub = links.hub
      if (hub.state !== 'open') {
        hub.state = 'unauthorized'
        publish({ hub: 'unauthorized', lastError: '设备凭据已失效（401），请重新配对' })
      } else {
        publish({ lastError: '设备凭据已失效（401），请重新配对' })
      }
      return { ok: false, unauthorized: true, error: '设备凭据已失效（可能被撤销），请重新配对（HTTP 401）' }
    }
    if (!res.ok) {
      const serverError = typeof body.error === 'string' ? body.error : ''
      return { ok: false, error: serverError || `HTTP ${res.status}`, code: typeof body.code === 'string' ? body.code : undefined }
    }
    if (body.ok === false) {
      return {
        ok: false,
        error: typeof body.error === 'string' ? body.error : '命令失败',
        code: typeof body.code === 'string' ? body.code : undefined,
      }
    }
    return {
      ok: true,
      data: (opts.whole ? (body as T) : (body.data as T | undefined)),
      code: typeof body.code === 'string' ? body.code : undefined,
    }
  }

  async function send(cmd: BridgeCommand): Promise<{ ok: boolean; error?: string }> {
    const id = Number(cmd?.id ?? 0)
    const type = String(cmd?.type ?? '')
    const payload = cmd?.payload ?? {}
    const t = token()
    if (!t) {
      const error = '未配对：缺少设备凭据，请先在配对页输入 6 位配对码'
      publish({ lastError: error })
      // 也要合成一帧：hubCommand 等的是事件，不是 send() 的返回值。
      emit(JSON.stringify({ event_type: 'command_response', id, ok: false, error }))
      return { ok: false, error }
    }
    const reply = await httpJson('/api/cmd', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${t}` },
      body: JSON.stringify({ cmd: type, payload }),
    })
    // ← 关键：HTTP 应答 → 合成 command_response（hub/api.ts 的 hubCommand 只认这个）。
    const envelope: Record<string, unknown> = { event_type: 'command_response', id, ok: reply.ok }
    if (reply.data !== undefined) envelope.data = reply.data
    if (reply.error) envelope.error = reply.error
    if (reply.code) envelope.code = reply.code
    emit(JSON.stringify(envelope))
    return reply.ok ? { ok: true } : { ok: false, error: reply.error }
  }

  // ---------- WebSocket ----------

  interface Link {
    state: MobileLinkState
    conn: MobileWebSocket | null
    gen: number
    retry: number
    timer: unknown
    url: string
    seen: Map<string, number>
  }

  function newLink(): Link {
    return { state: 'idle', conn: null, gen: 0, retry: 0, timer: null, url: '', seen: new Map() }
  }

  const links: Record<MobileLink, Link> = { chat: newLink(), hub: newLink() }

  function wsBase(): string {
    const loc = deps.location ?? (typeof location !== 'undefined' ? location : undefined)
    const proto = loc?.protocol === 'https:' ? 'wss' : 'ws'
    return `${proto}://${loc?.host ?? ''}`
  }

  // 同一帧投两遍的兜底：内容逐字节相同且 3s 内 → 丢弃（时间戳不同即视为新帧，
  // 所以正常的重复词「the the」不会被误删）。与旧页 app.js 的 isDuplicateFrame 同口径。
  function seenFrame(link: Link, raw: string): boolean {
    const t = now()
    const prev = link.seen.get(raw)
    if (prev !== undefined && t - prev < WS_FRAME_DEDUPE_MS) return true
    link.seen.set(raw, t)
    if (link.seen.size > WS_FRAME_MAP_MAX) {
      for (const [k, ts] of link.seen) if (t - ts > WS_FRAME_DEDUPE_MS) link.seen.delete(k)
    }
    return false
  }

  function closeLink(kind: MobileLink): void {
    const link = links[kind]
    link.gen++
    if (link.timer) {
      clearTimer(link.timer)
      link.timer = null
    }
    if (link.conn) {
      const c = link.conn
      link.conn = null
      try {
        c.close()
      } catch {
        /* 已经关掉的连接再 close 会抛，忽略 */
      }
    }
    link.state = 'idle'
  }

  function scheduleReconnect(kind: MobileLink, link: Link, gen: number): void {
    if (link.state === 'unauthorized') return
    const delay = Math.min(1000 * 2 ** link.retry, maxRetryDelay)
    link.retry++
    link.state = 'closed'
    publish({ [kind]: 'closed' } as Partial<MobileStatus>)
    if (link.timer) clearTimer(link.timer)
    link.timer = setTimer(() => {
      link.timer = null
      if (gen !== link.gen) return // 期间被 disconnect/重连换代：旧定时器作废
      open(kind)
    }, delay)
  }

  function open(kind: MobileLink): void {
    const link = links[kind]
    const Ctor = deps.WebSocket ?? (typeof WebSocket !== 'undefined' ? (WebSocket as unknown as MobileWebSocketCtor) : undefined)
    if (!Ctor || !link.url) {
      publish({ [kind]: 'idle' } as Partial<MobileStatus>)
      return
    }
    const gen = ++link.gen
    link.state = 'connecting'
    publish({ [kind]: 'connecting' } as Partial<MobileStatus>)
    let conn: MobileWebSocket
    try {
      conn = new Ctor(link.url)
    } catch (e) {
      publish({ [kind]: 'closed', lastError: `WebSocket 打不开：${errText(e)}` } as Partial<MobileStatus>)
      scheduleReconnect(kind, link, gen)
      return
    }
    link.conn = conn
    conn.onopen = () => {
      if (gen !== link.gen) return
      link.retry = 0
      publish({ [kind]: 'open' } as Partial<MobileStatus>)
    }
    conn.onmessage = (m) => {
      if (gen !== link.gen) return
      const raw = typeof m?.data === 'string' ? m.data : String(m?.data ?? '')
      if (!raw) return
      if (seenFrame(link, raw)) return
      // 事件原样分发（JSON 字符串）：hub store / 会话 store 都按字符串 JSON.parse。
      emit(raw)
    }
    conn.onerror = () => {
      if (gen !== link.gen) return
      // 浏览器不给 WS 错误的细节（握手 401 也只是 1006）；状态留给 onclose 重连。
      publish({ lastError: `${kind} 事件流连接异常` })
    }
    conn.onclose = () => {
      if (gen !== link.gen) return
      link.conn = null
      scheduleReconnect(kind, link, gen)
    }
  }

  function connectChat(ws: string, sid: string): void {
    const t = token()
    if (!t || !ws || !sid) return
    const link = links.chat
    closeLink('chat')
    const q = new URLSearchParams({ token: t, workspace: ws, session_id: sid })
    link.url = `${wsBase()}/api/events?${q.toString()}`
    open('chat')
  }

  function connectHub(): void {
    const t = token()
    if (!t) return
    const link = links.hub
    closeLink('hub')
    const q = new URLSearchParams({ scope: 'hub', token: t })
    link.url = `${wsBase()}/api/events?${q.toString()}`
    open('hub')
  }

  function disconnect(kind?: MobileLink): void {
    if (kind) {
      closeLink(kind)
      publish({ [kind]: 'idle' } as Partial<MobileStatus>)
      return
    }
    closeLink('chat')
    closeLink('hub')
    publish({ chat: 'idle', hub: 'idle' })
  }

  // ---------- Transport 实现 ----------

  const impl: Partial<Transport> & Record<string, unknown> = {
    start: async (_ws: string) => ({ ok: true }),
    send,
    onEvent: (cb: (line: string) => void) => {
      subs.add(cb)
      return () => {
        subs.delete(cb)
      }
    },
    onExit: (_cb: (info: { code: number | null; err: string }) => void) => () => {},
    onHubDeepLink: (_cb: (url: string) => void) => () => {},
    takeHubDeepLinks: async () => [],
    settingsGet: async (): Promise<AppSettings> => {
      let saved: Record<string, unknown> = {}
      try {
        const raw = storage?.getItem(SETTINGS_KEY)
        if (raw) saved = JSON.parse(raw) as Record<string, unknown>
      } catch {
        saved = {}
      }
      return { ...defaultSettings(storage), ...saved } as unknown as AppSettings
    },
    settingsSet: async (patch: Partial<AppSettings>) => {
      const current = await impl.settingsGet!()
      const next = { ...(current as unknown as Record<string, unknown>), ...(patch as Record<string, unknown>) }
      try {
        storage?.setItem(SETTINGS_KEY, JSON.stringify(next))
      } catch {
        /* 写不进去（隐私模式）也不抛：本次会话内 theme 由壳自己记 */
      }
      return { ok: true, settings: next as unknown as AppSettings }
    },
    // 输入历史/文件选择这些桌面专属能力：手机给「空但形状对」的结果，别让调用方等异常。
    inputHistoryGet: async () => ({ ok: true, items: [] }),
    inputHistorySet: async () => ({ ok: true, items: [] }),
    inputHistoryForget: async () => ({ ok: true }),
    // 右栏终端（docs/TERMINAL_DESIGN.md §5.1）：PTY 只活在桌面的 Go 桥里，手机连的是
    // /api/cmd 面（没有 pty_* 命令，也没有 /pty 的 WS 端点）。这里显式给失败原因，不靠
    // Proxy 兜成 `unsupported: ptyCreate` —— 手机面板要显示一句人话。
    // [recovered] 依据 docs/TERMINAL_MODULE_SWEEP.md §3.3
    ptyCreate: async () => ({ ok: false, error: '终端仅在桌面端可用' }),
    ptyTicket: async () => ({ ok: false, error: '终端仅在桌面端可用' }),
    ptyClose: async () => ({ ok: false, error: '终端仅在桌面端可用' }),
    pickWorkspace: async () => null,
    pickFsFile: async () => null,
    pickFsFiles: async () => [],
    pickFsDir: async () => null,
    pickApp: async () => null,
    getPathForFile: () => '',
    providerKeyStatus: async () => false,
    providerKey: async () => '',
  }

  // 未实现的方法统一降级：永远返回 Promise，永远不抛。
  // `then` 必须返回 undefined —— 否则这个对象会被当成 thenable（await 它会死循环）。
  const proxy = new Proxy(impl, {
    get(target, prop, receiver) {
      if (prop === 'then') return undefined
      if (Reflect.has(target, prop)) return Reflect.get(target, prop, receiver)
      if (typeof prop !== 'string') return undefined
      return () => Promise.resolve({ ok: false, error: `unsupported: ${prop}` })
    },
  })

  const api: MobileTransport = proxy as unknown as MobileTransport
  // 显式覆盖「不是 Transport 形状」的几个方法（Proxy 只兜底，不提供语义）。
  Object.assign(api, {
    connectChat,
    connectHub,
    disconnect,
    status: () => ({ ...status }),
    onStatus: (cb: (s: MobileStatus) => void) => {
      statusSubs.add(cb)
      return () => {
        statusSubs.delete(cb)
      }
    },
    token,
    setToken,
    ping: async () => {
      const t = token()
      const r = await httpJson<{ paired?: boolean }>(
        '/api/ping',
        { method: 'GET', headers: t ? { Authorization: `Bearer ${t}` } : {} },
        { whole: true },
      )
      if (!r.ok) return { ok: false, error: r.error, unauthorized: r.unauthorized }
      const paired = r.data?.paired === true
      return { ok: true, data: { paired } }
    },
    pair: async (code: string, name: string) => {
      const r = await httpJson<{ token?: string }>(
        '/api/pair',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ code, name }),
        },
        { whole: true },
      )
      const t = typeof r.data?.token === 'string' ? r.data.token : ''
      if (!r.ok || !t) return { ok: false, error: r.error || '配对失败：服务端没有返回 token' }
      setToken(t)
      publish({ lastError: undefined })
      return { ok: true, data: { token: t } }
    },
  } as Partial<MobileTransport>)

  return api
}
