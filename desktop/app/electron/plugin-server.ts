// plugin-server.ts —— web.start 的进程生命周期（MCP server 同一心智：配置命令，连上去）。
//
// 为什么在 Electron main 而不是 Go bridge：
//   · 服务是**页面视图**的前置条件（就绪后才 loadURL），生命周期与 plugin-app 的视图池同层；
//   · 健康检查/日志/退出要直接驱动视图状态（starting / error + logs），绕桥只会多一次往返；
//   · manifest 已由 bridge 安装期校验，main 只做形状/路径的二次防御（不信渲染层）。
//
// 三条边界：
//   1. **惰性启动**：第一次打开该插件的标签才启动；多个标签共享一个进程；插件卸载/应用退出
//      才停。关闭单个标签不停服务（浏览器标签的心智：回来还是热的）。
//   2. **不抢已有服务**：先探活；web.url 已经能响应就复用（owned=false），退出时也不杀。
//   3. **最小环境**：只给 PATH/HOME 等启动必需项 + manifest.start.env，不把宿主进程的
//      provider key / bridge token 交给插件服务。命令/参数用 shell:false 直接 spawn。
import { spawn, type ChildProcess } from 'node:child_process'
import http from 'node:http'
import https from 'node:https'
import path from 'node:path'
import { surfaceRoot } from './surface-index'
import type { SurfaceWebDecl, SurfaceWebStartDecl } from './surface-index'
import { baseServerEnv, cleanLogLine, joinHealthUrl, resolveServerCwd, serverTimeoutMs } from './plugin-server-common'

export interface PluginServerResult {
  ok: boolean
  error?: string
  /** 失败时的最近日志（宿主错误态直接显示） */
  logs?: string
  /** true = 本次由宿主 spawn；false = 复用了已在跑的服务 */
  owned?: boolean
}

interface ServerState {
  pluginId: string
  url: string
  sig: string
  start: SurfaceWebStartDecl
  child?: ChildProcess
  owned: boolean
  external: boolean
  ready: boolean
  stopping: boolean
  logs: string[]
  starting?: Promise<PluginServerResult>
}

const MAX_LOGS = 80
const TAIL_LINES = 20
const servers = new Map<string, ServerState>()
type ExitHandler = (pluginId: string, error: string, logs: string) => void
const exitHandlers = new Set<ExitHandler>()

/** onPluginServerExit 服务在**就绪后**退出（崩溃/用户 kill）时通知视图层。 */
export function onPluginServerExit(cb: ExitHandler): () => void {
  exitHandlers.add(cb)
  return () => exitHandlers.delete(cb)
}

function sigOf(web: SurfaceWebDecl): string {
  return JSON.stringify({ url: web.url, start: web.start ?? null })
}

function pushLog(st: ServerState, line: string): void {
  const clean = cleanLogLine(line)
  st.logs.push(clean)
  if (st.logs.length > MAX_LOGS) st.logs.splice(0, st.logs.length - MAX_LOGS)
}

function tail(st: ServerState): string {
  const text = st.logs.slice(-TAIL_LINES).join('\n')
  return text.length > 4000 ? text.slice(-4000) : text
}

function wireLogs(st: ServerState, child: ChildProcess): void {
  for (const stream of [child.stdout, child.stderr]) {
    if (!stream) continue
    stream.setEncoding('utf8')
    let buf = ''
    stream.on('data', (chunk: string) => {
      buf += chunk
      const lines = buf.split(/\r?\n/)
      buf = lines.pop() ?? ''
      for (const line of lines) pushLog(st, line)
    })
    stream.on('end', () => {
      if (buf) pushLog(st, buf)
      buf = ''
    })
  }
}

/** 探活：任何 <500 的 HTTP 响应都算服务已起（401/404 也是「端口在听」）。 */
function probe(url: string, timeoutMs = 700): Promise<boolean> {
  return new Promise((resolve) => {
    let done = false
    const finish = (v: boolean): void => {
      if (done) return
      done = true
      resolve(v)
    }
    try {
      const u = new URL(url)
      const mod = u.protocol === 'https:' ? https : http
      const req = mod.get(u, { timeout: timeoutMs }, (res) => {
        const code = res.statusCode ?? 0
        res.resume()
        finish(code > 0 && code < 500)
      })
      req.on('error', () => finish(false))
      req.on('timeout', () => {
        req.destroy()
        finish(false)
      })
    } catch {
      finish(false)
    }
  })
}

/** 杀掉 spawn 的子进程。POSIX 用进程组（npm 会再拉 node）；Windows 只能杀直接子进程。 */
function killChild(child: ChildProcess): void {
  const pid = child.pid
  if (!pid) return
  const send = (sig: NodeJS.Signals): void => {
    try {
      if (process.platform !== 'win32') process.kill(-pid, sig)
      else child.kill(sig)
    } catch {
      try {
        child.kill(sig)
      } catch {
        /* 已经退出 */
      }
    }
  }
  send('SIGTERM')
  // 2s 还没退就 SIGKILL；unref 让这个计时器不拖住应用退出。
  setTimeout(() => send('SIGKILL'), 2000).unref?.()
}

function fail(st: ServerState, message: string): PluginServerResult {
  pushLog(st, message)
  if (st.child) killChild(st.child)
  st.child = undefined
  st.ready = false
  st.owned = false
  return { ok: false, error: message, logs: tail(st) }
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms))
}

async function startServer(st: ServerState): Promise<PluginServerResult> {
  const { start, url } = st
  const healthUrl = joinHealthUrl(url, start.health)
  // 已经有服务在监听（用户手动 npm run dev，或上一轮外部服务）→ 复用，不 spawn。
  if (await probe(healthUrl, 800)) {
    st.ready = true
    st.owned = false
    st.external = true
    pushLog(st, `复用已运行的服务：${healthUrl}`)
    return { ok: true, owned: false }
  }
  if (st.stopping) return fail(st, '服务启动已取消')

  let child: ChildProcess
  try {
    const pluginDir = path.join(surfaceRoot(), st.pluginId)
    const cwd = resolveServerCwd(pluginDir, start.cwd)
    const env = { ...baseServerEnv(), ...(start.env ?? {}), PWD: cwd }
    child = spawn(start.command, start.args ?? [], {
      cwd,
      env,
      shell: false,
      detached: process.platform !== 'win32',
      windowsHide: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    pushLog(st, `启动服务：${[start.command, ...(start.args ?? [])].join(' ')}（cwd=${cwd}）`)
  } catch (e) {
    return fail(st, `服务启动失败：${e instanceof Error ? e.message : String(e)}`)
  }

  st.child = child
  st.owned = true
  st.external = false
  wireLogs(st, child)

  let exitError = ''
  child.on('error', (e) => {
    exitError = `服务启动失败：${e.message}`
    pushLog(st, exitError)
  })
  child.on('exit', (code, signal) => {
    const wasReady = st.ready
    st.ready = false
    if (st.child === child) st.child = undefined
    const msg = `服务进程退出（code=${code ?? 'null'}${signal ? `，signal=${signal}` : ''}）`
    pushLog(st, msg)
    if (!wasReady) {
      exitError = msg
      return
    }
    if (!st.stopping) {
      const logs = tail(st)
      for (const cb of exitHandlers) {
        try {
          cb(st.pluginId, msg, logs)
        } catch {
          /* 一个视图层回调抛错不影响其余 */
        }
      }
    }
  })

  const timeoutMs = serverTimeoutMs(start.timeoutMs)
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (st.stopping) return fail(st, '服务启动已取消')
    if (exitError) return fail(st, exitError)
    if (await probe(healthUrl, 700)) {
      st.ready = true
      st.external = false
      pushLog(st, `服务就绪：${healthUrl}`)
      return { ok: true, owned: true }
    }
    await sleep(200)
  }
  return fail(st, `服务在 ${timeoutMs}ms 内未就绪（health=${healthUrl}）`)
}

/**
 * ensurePluginServer 确保某个插件的服务已就绪（惰性、幂等、多标签共享）。
 *
 * 没有 web.start → 立即 ok（旧行为：服务由用户自己跑）。有 start → 先探活复用，再 spawn
 * 并轮询 health；失败返回最近日志，由视图层画宿主错误态。
 */
export function ensurePluginServer(pluginId: string, web: SurfaceWebDecl): Promise<PluginServerResult> {
  if (!web.start) return Promise.resolve({ ok: true })
  const sig = sigOf(web)
  let cur = servers.get(pluginId)
  if (cur && (cur.url !== web.url || cur.sig !== sig)) {
    stopPluginServer(pluginId)
    cur = undefined
  }
  if (cur?.starting) return cur.starting
  if (cur?.ready) {
    if (cur.owned && cur.child) return Promise.resolve({ ok: true, owned: true })
    // 外部服务：可能已经被用户停掉；重探一次再决定是否 spawn。
    return probe(joinHealthUrl(web.url, web.start.health), 700).then((up) => {
      if (up) return { ok: true, owned: false }
      servers.delete(pluginId)
      return ensurePluginServer(pluginId, web)
    })
  }
  const st: ServerState = cur ?? {
    pluginId,
    url: web.url,
    sig,
    start: web.start,
    owned: false,
    external: false,
    ready: false,
    stopping: false,
    logs: [],
  }
  st.url = web.url
  st.sig = sig
  st.start = web.start
  servers.set(pluginId, st)
  const p = startServer(st)
  st.starting = p
  void p.finally(() => {
    if (st.starting === p) st.starting = undefined
  })
  return p
}

/** stopPluginServer 停掉**由宿主启动**的服务；外部服务只清状态，不杀。 */
export function stopPluginServer(pluginId: string): void {
  const st = servers.get(pluginId)
  if (!st) return
  servers.delete(pluginId)
  st.stopping = true
  st.ready = false
  if (st.child && st.owned) killChild(st.child)
  st.child = undefined
}

/** stopAllPluginServers 应用退出/窗口关闭时调用（幂等）。 */
export function stopAllPluginServers(): void {
  for (const id of [...servers.keys()]) stopPluginServer(id)
}

/** 诊断用：当前服务状态（不含 child 对象）。 */
export function pluginServerSnapshot(): Array<{ pluginId: string; ready: boolean; owned: boolean; url: string; logs: string }> {
  return [...servers.values()].map((s) => ({ pluginId: s.pluginId, ready: s.ready, owned: s.owned, url: s.url, logs: tail(s) }))
}
