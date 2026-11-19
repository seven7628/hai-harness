// plugin-app.ts —— URL 型插件的原生视图池 + 页面 ↔ 宿主 host bridge（2026-11）。
//
// 与 embed.ts 的关系（不是复用，是刻意分开）：
//   · embed.ts 是**浏览器面板**：视图归属会话、接 CDP、参与 Browser Use 的页列表；
//   · 本文件是**插件应用**：视图归属 `pluginId`，不接 CDP、不进浏览器页列表，页面
//     通过 `plugin-app-preload.ts` 注入的 `window.haiHost` 使用宿主能力。
// 两者都要「原生层级浮在 DOM 上 + 按矩形 show/hide」，但生命周期与权限边界不同；
// 混进同一张池子会让「插件页面」出现在 Browser Use 的 targets 里（安全/语义都错）。
//
// 一条视图 = 一个标签（key = `${pluginId}:${tabId}`）。切标签/收右栏只 hide，不销毁：
// 页面状态留在渲染进程里，回来时还是那一页（浏览器标签的心智）。关标签才 close。
import { BrowserWindow, WebContentsView, ipcMain, session, shell } from 'electron'
import { normalizeHostReply } from './plugin-app-reply'
import { ensurePluginServer, onPluginServerExit, stopAllPluginServers, stopPluginServer } from './plugin-server'
import type { SurfaceWebStartDecl } from './surface-index'
import * as path from 'node:path'

/** 与渲染层 src/transport/types.ts 的 SurfaceAppStatus 同构（preload 不能 import 渲染层）。 */
export interface PluginAppStatus {
  key: string
  status: 'loading' | 'starting' | 'ready' | 'error'
  url?: string
  error?: string
  code?: number
  logs?: string
}

export interface PluginAppOpenInput {
  key: string
  pluginId: string
  url: string
  /** manifest.web.url 的根（服务探活/启动）；缺省 = url */
  webUrl?: string
  /** manifest.data：页面允许订阅的事件（preload 用它做第一道闸门） */
  events: string[]
  /** manifest.commands：页面允许调用的命令（main 与渲染层各校验一次） */
  commands: string[]
  /** manifest.web.start：有它则先确保服务就绪再 loadURL */
  start?: SurfaceWebStartDecl
  /** manifest.web.permissions：目前只支持 media（麦克风/摄像头） */
  permissions?: string[]
}

interface AppEntry {
  key: string
  pluginId: string
  url: string
  webUrl: string
  start?: SurfaceWebStartDecl
  origin: string
  events: Set<string>
  commands: Set<string>
  permissions: Set<string>
  view: WebContentsView
  wcId: number
  ready: boolean
  wantVisible: boolean
  lastRect?: { x: number; y: number; width: number; height: number }
  status: PluginAppStatus['status']
  lastError?: string
  lastCode?: number
  lastLogs?: string
}

const entries = new Map<string, AppEntry>()
let winRef: BrowserWindow | null = null
let activeKey = ''
let hostSeq = 0
const pendingHost = new Map<number, { resolve: (v: { ok: boolean; reason?: string; data?: unknown }) => void; timer: NodeJS.Timeout }>()

function originOf(raw: string): string {
  try {
    return new URL(raw).origin
  } catch {
    return ''
  }
}

/** CSS px → DIP（与 embed.ts 的 toDip 同一条理由：外观字体大小走原生 zoomFactor）。 */
function toDip(win: BrowserWindow | null, rect: { x: number; y: number; width: number; height: number }) {
  const z = win && !win.isDestroyed() ? win.webContents.getZoomFactor() : 1
  if (!z || z === 1) return rect
  return {
    x: Math.round(rect.x * z),
    y: Math.round(rect.y * z),
    width: Math.round(rect.width * z),
    height: Math.round(rect.height * z),
  }
}

function validRect(r: { x?: number; y?: number; width?: number; height?: number } | undefined): boolean {
  return !!r && typeof r.x === 'number' && typeof r.y === 'number' &&
    typeof r.width === 'number' && typeof r.height === 'number' &&
    r.width >= 80 && r.height >= 80
}

function sendStatus(e: AppEntry): void {
  if (!winRef || winRef.isDestroyed()) return
  let url = ''
  try {
    url = e.view.webContents.getURL()
  } catch {
    /* 视图正在销毁 */
  }
  const status: PluginAppStatus = {
    key: e.key,
    status: e.status,
    ...(url ? { url } : {}),
    ...(e.lastError ? { error: e.lastError } : {}),
    ...(e.lastCode != null ? { code: e.lastCode } : {}),
    ...(e.lastLogs ? { logs: e.lastLogs } : {}),
  }
  try {
    winRef.webContents.send('plugin-app:status', status)
  } catch {
    /* 窗口正在拆：状态丢了不影响 view 生命周期 */
  }
}

function setStatus(e: AppEntry, status: PluginAppStatus['status'], error?: string, code?: number, logs?: string): void {
  e.status = status
  e.lastError = error
  e.lastCode = code
  e.lastLogs = logs
  sendStatus(e)
}

/** 只允许视图停在声明 origin 内；外链交给系统浏览器（与主窗口的 setWindowOpenHandler 同口径）。 */
function sameOrigin(e: AppEntry, raw: string): boolean {
  const o = originOf(raw)
  return !!o && o === e.origin
}

function openExternal(raw: string): void {
  try {
    const u = new URL(raw)
    if (u.protocol === 'http:' || u.protocol === 'https:') void shell.openExternal(raw)
  } catch {
    /* 非 http(s)：不交给系统 */
  }
}

function createEntry(win: BrowserWindow, req: PluginAppOpenInput): AppEntry {
  const origin = originOf(req.url)
  if (!origin) throw new Error(`插件页面 URL 非法：${req.url}`)
  const partition = `persist:surface-${req.pluginId}`
  const ses = session.fromPartition(partition)
  // 权限只在 manifest.web.permissions 显式声明时放行。media 是麦克风/摄像头（学习类
  // 应用会用它做跟读）；默认全拒 —— 一个网页型插件不该默认拿到设备的耳朵。
  const allowedPerms = new Set(req.permissions ?? [])
  ses.setPermissionRequestHandler((wc, permission, callback) => {
    const e = [...entries.values()].find((x) => x.wcId === wc?.id)
    callback(!!e && permission === 'media' && allowedPerms.has('media'))
  })
  ses.setPermissionCheckHandler((wc, permission) => {
    const e = [...entries.values()].find((x) => x.wcId === wc?.id)
    return !!e && permission === 'media' && allowedPerms.has('media')
  })

  const view = new WebContentsView({
    webPreferences: {
      preload: path.join(__dirname, 'plugin-app-preload.js'),
      partition,
      sandbox: true,
      contextIsolation: true,
      nodeIntegration: false,
      webSecurity: true,
      spellcheck: false,
    },
  })
  view.setBackgroundColor('#0d1117')
  win.contentView.addChildView(view)
  view.setVisible(false)

  const entry: AppEntry = {
    key: req.key,
    pluginId: req.pluginId,
    url: req.url,
    webUrl: req.webUrl || req.url,
    ...(req.start ? { start: req.start } : {}),
    origin,
    events: new Set(req.events),
    commands: new Set(req.commands),
    permissions: allowedPerms,
    view,
    wcId: view.webContents.id,
    ready: false,
    wantVisible: false,
    status: 'loading',
  }
  entries.set(req.key, entry)

  view.webContents.setWindowOpenHandler(({ url }) => {
    openExternal(url)
    return { action: 'deny' }
  })
  view.webContents.on('will-navigate', (e, url) => {
    if (sameOrigin(entry, url)) return
    e.preventDefault()
    openExternal(url)
  })
  view.webContents.on('will-redirect', (e, url) => {
    if (sameOrigin(entry, url)) return
    e.preventDefault()
    openExternal(url)
  })
  view.webContents.on('did-start-loading', () => {
    // 首次加载期间不露 Chromium 的空白页：DOM 侧有宿主自己的 loading 态。
    if (!entry.ready) view.setVisible(false)
    setStatus(entry, 'loading')
  })
  view.webContents.on('did-finish-load', () => {
    entry.ready = true
    setStatus(entry, 'ready')
    if (entry.wantVisible) view.setVisible(true)
  })
  view.webContents.on('did-fail-load', (_e, code, desc, _url, isMainFrame) => {
    if (!isMainFrame || code === -3) return // -3 = ABORTED（被后续导航顶掉，正常）
    entry.ready = false
    view.setVisible(false)
    setStatus(entry, 'error', `${desc || '加载失败'}（${code}）`, code)
    // 失败页留在 DOM 层的宿主错误态里；不自动跳系统浏览器（那会把错误页变成外链）。
  })
  view.webContents.on('render-process-gone', (_e, details) => {
    entry.ready = false
    view.setVisible(false)
    setStatus(entry, 'error', `页面进程退出（${details.reason}）`)
  })
  view.webContents.on('destroyed', () => {
    entries.delete(entry.key)
  })

  return entry
}

/** loadEntry 真正加载页面（服务就绪后才调；与 createEntry 分开是为了先起 web.start）。 */
function loadEntry(e: AppEntry): void {
  e.ready = false
  setStatus(e, 'loading')
  void e.view.webContents.loadURL(e.url).catch((err: unknown) => {
    e.ready = false
    setStatus(e, 'error', err instanceof Error ? err.message : String(err))
  })
}

/** beginEntry 先确保 web.start 就绪，再加载页面；没有 start 就是直接加载。 */
async function beginEntry(e: AppEntry): Promise<{ ok: boolean; error?: string }> {
  if (e.start) {
    setStatus(e, 'starting')
    const r = await ensurePluginServer(e.pluginId, { url: e.webUrl, start: e.start })
    if (!r.ok) {
      const msg = r.error || '插件服务启动失败'
      setStatus(e, 'error', msg, undefined, r.logs)
      return { ok: false, error: msg }
    }
  }
  loadEntry(e)
  return { ok: true }
}

function destroyEntry(e: AppEntry): void {
  entries.delete(e.key)
  if (activeKey === e.key) activeKey = ''
  try {
    if (winRef && !winRef.isDestroyed()) winRef.contentView.removeChildView(e.view)
  } catch {
    /* 窗口已在拆 */
  }
  try {
    e.view.webContents.close()
  } catch {
    /* 已销毁 */
  }
}

// —— 公开 API（main.ts 的 IPC 处理器只做参数转发/来源校验）——

/** 主窗口就绪/关闭时更新（sendStatus/askRendererHost 需要它；不依赖 IPC 首次调用）。 */
export function setPluginAppWindow(win: BrowserWindow | null): void {
  if (!win || win.isDestroyed()) {
    winRef = null
    return
  }
  winRef = win
}

export async function openPluginApp(win: BrowserWindow, req: PluginAppOpenInput): Promise<{ ok: boolean; error?: string }> {
  winRef = win
  const cur = entries.get(req.key)
  if (cur) {
    // 同一 key 换 URL（manifest 升级/热加载）：重新加载，但保留视图与 partition。
    // 换 URL 意味着旧服务可能不再对应这个页面 → 先停掉宿主自己起的旧服务。
    const nextWebUrl = req.webUrl || req.url
    if (cur.url !== req.url || cur.webUrl !== nextWebUrl) {
      stopPluginServer(cur.pluginId)
      cur.url = req.url
      cur.webUrl = nextWebUrl
      cur.start = req.start
      cur.origin = originOf(req.url)
      cur.events = new Set(req.events)
      cur.commands = new Set(req.commands)
      cur.permissions = new Set(req.permissions ?? [])
      cur.ready = false
      cur.view.setVisible(false)
      return beginEntry(cur)
    }
    // 视图已存在（切标签回来）：把**当前**状态补发一次。不补的话，组件重新挂载时
    // 初始态是 loading，而 did-finish-load 早在上一轮就发过了 → 页面永远不 show。
    sendStatus(cur)
    return { ok: true }
  }
  try {
    const e = createEntry(win, req)
    return await beginEntry(e)
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) }
  }
}

export function showPluginApp(win: BrowserWindow, key: string, rect: { x: number; y: number; width: number; height: number }): { ok: boolean; error?: string } {
  winRef = win
  const e = entries.get(key)
  if (!e) return { ok: false, error: '视图不存在' }
  if (!validRect(rect)) return { ok: false, error: '矩形非法' }
  e.lastRect = rect
  // 同屏只显示一个插件视图：其他标签的视图保活但不可见（原生层级没有 DOM 的 z-index）。
  for (const other of entries.values()) {
    if (other.key !== key) {
      other.wantVisible = false
      other.view.setVisible(false)
    }
  }
  activeKey = key
  e.wantVisible = true
  e.view.setBounds(toDip(win, rect))
  if (e.ready) e.view.setVisible(true)
  return { ok: true }
}

export function hidePluginApp(key: string): void {
  const e = entries.get(key)
  if (!e) return
  e.wantVisible = false
  e.view.setVisible(false)
  if (activeKey === key) activeKey = ''
}

export function hideAllPluginApps(): void {
  for (const e of entries.values()) {
    e.wantVisible = false
    e.view.setVisible(false)
  }
  activeKey = ''
}

export function closePluginApp(key: string): void {
  const e = entries.get(key)
  if (e) destroyEntry(e)
}

export function closePluginAppsOfPlugin(pluginId: string): void {
  for (const e of [...entries.values()]) {
    if (e.pluginId === pluginId) destroyEntry(e)
  }
  // 插件卸载/禁用：连服务一起停（标签关闭不停，只有这里与退出才停）。
  stopPluginServer(pluginId)
}

export function closeAllPluginApps(): void {
  for (const e of [...entries.values()]) destroyEntry(e)
  stopAllPluginServers()
  for (const [, p] of pendingHost) {
    clearTimeout(p.timer)
    p.resolve({ ok: false, reason: 'denied' })
  }
  pendingHost.clear()
}

export function pluginAppCount(): number {
  return entries.size
}

// —— 页面 ↔ 渲染层 host 的桥 ——

function entryOfSender(wcId: number): AppEntry | undefined {
  for (const e of entries.values()) if (e.wcId === wcId) return e
  return undefined
}

/** 页面调 host.command：main 先校验 origin/声明，再交给渲染层的 createHost。 */
function askRendererHost(win: BrowserWindow, pluginId: string, name: string, payload?: unknown): Promise<{ ok: boolean; reason?: string; data?: unknown }> {
  if (!win || win.isDestroyed() || win.webContents.isDestroyed()) return Promise.resolve({ ok: false, reason: 'denied' })
  const id = ++hostSeq
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      pendingHost.delete(id)
      resolve({ ok: false, reason: 'timeout' })
    }, 60_000)
    pendingHost.set(id, { resolve, timer })
    win.webContents.send('plugin-app:host-command', { id, pluginId, name, payload })
  })
}

/**
 * registerPluginAppIpc 只注册一次（main.ts 在 app ready 后调用）。
 *
 * 权限边界：所有 handler 都先问「这个 sender 是哪条视图」——页面不能靠伪造 pluginId
 * 去用别人的声明；pluginId 只从 main 自己的 entries 表里取。
 */
let exitHooked = false

export function registerPluginAppIpc(): void {
  if (!exitHooked) {
    exitHooked = true
    onPluginServerExit((pluginId, error, logs) => {
      // 服务在页面已就绪后退出：把所有该插件的视图切到宿主错误态，用户点重试即可重拉。
      for (const e of entries.values()) {
        if (e.pluginId !== pluginId) continue
        e.ready = false
        try {
          e.view.setVisible(false)
        } catch {
          /* 视图已销毁 */
        }
        setStatus(e, 'error', error, undefined, logs)
      }
    })
  }
  ipcMain.handle('plugin-app:open', (e, p: PluginAppOpenInput) => {
    if (!winRef || winRef.isDestroyed()) return { ok: false, error: '窗口未就绪' }
    if (e.sender !== winRef.webContents) return { ok: false, error: '仅主窗口可开插件视图' }
    return openPluginApp(winRef, p)
  })
  ipcMain.handle('plugin-app:show', (e, p: { key: string; rect: { x: number; y: number; width: number; height: number } }) => {
    if (!winRef || winRef.isDestroyed()) return { ok: false, error: '窗口未就绪' }
    if (e.sender !== winRef.webContents) return { ok: false, error: '仅主窗口可显示插件视图' }
    return showPluginApp(winRef, String(p?.key ?? ''), p?.rect)
  })
  ipcMain.handle('plugin-app:hide', (e, key: string) => {
    if (e.sender !== winRef?.webContents) return { ok: false }
    hidePluginApp(String(key ?? ''))
    return { ok: true }
  })
  ipcMain.handle('plugin-app:close', (e, key: string) => {
    if (e.sender !== winRef?.webContents) return { ok: false }
    closePluginApp(String(key ?? ''))
    return { ok: true }
  })
  ipcMain.handle('plugin-app:close-plugin', (e, pluginId: string) => {
    if (e.sender !== winRef?.webContents) return { ok: false }
    closePluginAppsOfPlugin(String(pluginId ?? ''))
    return { ok: true }
  })
  // preload 同步取配置（sandbox preload 里 process.argv 不可靠；sendSync 是 Electron
  // 文档认可的最小面）。返回 null = 不是已登记的视图 → 不注入 window.haiHost。
  ipcMain.on('plugin-app:hello', (e) => {
    const entry = entryOfSender(e.sender.id)
    e.returnValue = entry
      ? { pluginId: entry.pluginId, origin: entry.origin, events: [...entry.events], commands: [...entry.commands] }
      : null
  })
  ipcMain.handle('plugin-app:host-command', async (e, p: { name?: unknown; payload?: unknown }) => {
    const entry = entryOfSender(e.sender.id)
    if (!entry) return { ok: false, reason: 'denied' }
    const frameUrl = (() => {
      try {
        return e.senderFrame?.url || e.sender.getURL()
      } catch {
        return ''
      }
    })()
    if (!sameOrigin(entry, frameUrl)) return { ok: false, reason: 'denied' }
    const name = typeof p?.name === 'string' ? p.name.trim() : ''
    if (!name || !entry.commands.has(name)) return { ok: false, reason: 'not_declared' }
    if (!winRef || winRef.isDestroyed()) return { ok: false, reason: 'denied' }
    return askRendererHost(winRef, entry.pluginId, name, p?.payload)
  })
  ipcMain.on('plugin-app:host-reply', (e, p: { id?: unknown; reply?: unknown }) => {
    if (e.sender !== winRef?.webContents) return
    const id = typeof p?.id === 'number' ? p.id : -1
    const pending = pendingHost.get(id)
    if (!pending) return
    pendingHost.delete(id)
    clearTimeout(pending.timer)
    // 顶层展开字段必须保留（见 plugin-app-reply.ts：页面按 `r.session_id` 读）。
    pending.resolve(normalizeHostReply(p?.reply))
  })
  // 渲染层 host 的事件 → 该插件的所有视图（通常一条；多开 tab 时按 session_id 自行过滤）。
  ipcMain.on('plugin-app:event', (e, p: { pluginId?: unknown; type?: unknown; event?: unknown }) => {
    if (e.sender !== winRef?.webContents) return
    const pluginId = typeof p?.pluginId === 'string' ? p.pluginId : ''
    const type = typeof p?.type === 'string' ? p.type : ''
    if (!pluginId || !type) return
    for (const entry of entries.values()) {
      if (entry.pluginId !== pluginId || !entry.events.has(type)) continue
      try {
        entry.view.webContents.send('plugin-app:event', { type, event: p?.event })
      } catch {
        /* 视图正在销毁 */
      }
    }
  })
}
