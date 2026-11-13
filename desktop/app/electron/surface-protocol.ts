// surface-protocol.ts —— `hai-surface://<id>/<path>`：插件 panel.js / icon.svg / css 的字节通道（PR-14 A）。
//
// 为什么需要它（而不是让渲染层用 file:// 或把 panel.js 读成字符串再 eval）：
//   · `file://` 在 dist 打包后不可用（渲染进程是 http://localhost），且 `import()` 一个
//     file URL 在标准 scheme 下会被 Chromium 直接拒（不安全来源）；
//   · 字符串 + `new Function` 就绕开了 ESM 的边界（没有静态 import 解析、没有 URL 身份、
//     也没有「hash 变了才重新拉」的缓存键）—— 而 §5.1 的硬约束恰恰是「只接受同源可信 ESM」。
// 自定义协议同时解决三件事：可 import、有稳定 URL（`?v=<hash>`）、**字节仍然只来自
// `~/.go-code/surfaces/<id>/`**。
//
// 与 `hai-media://`（media-protocol.ts）同一条纪律，逐条对应：
//   1. 只 GET/HEAD（405 其它）：这条通道不能变成写通道，也不能变成探测通道；
//   2. 路径只解一次码，然后拒绝 NUL / 反斜杠 / 非绝对（pathname 必须以 / 开头）/
//      `..` 段 / 尾斜杠；
//   3. 扩展名白名单在 stat **之前**（非白名单连「存不存在」都不泄露）；
//   4. realpath 解软链后再比一次扩展名，再比**包含**（`<id>` 目录内）；
//   5. 必须是普通文件；
//   6. `<id>` 必须匹配 ^[a-z][a-z0-9-]{1,40}$，且**必须在 index 里且 enabled**——
//      禁用/卸载后协议立刻 404（能力随 enabled 一起消失，不是「灰掉但还能拿字节」）。
//
// 校验函数 `resolveSurfacePath(id, pathname, index)` 是**纯函数**（输入 id/路径/索引 →
// 绝对路径或错误），所以拒绝分支全部能在 node 单测里跑，不需要启动 Electron。
import * as fs from 'node:fs'
import * as path from 'node:path'
import { readSurfaceIndex, SURFACE_ID_RE, surfaceRoot, type SurfaceIndex } from './surface-index'

export const SURFACE_SCHEME = 'hai-surface'

/** 扩展名 → Content-Type。**这就是协议的扩展名白名单**（不在这张表里的一律 403）。 */
export const SURFACE_MIMES: Record<string, string> = {
  js: 'text/javascript',
  mjs: 'text/javascript',
  css: 'text/css',
  json: 'application/json',
  svg: 'image/svg+xml',
  png: 'image/png',
}

// registerSchemesAsPrivileged 的入参必须在 app ready **之前**交给 Electron（与
// `hai-media` 同一时序要求，见 main.ts 顶部）。
//   · standard —— 有 host/path（`<id>` 在 host 段），且是独立 origin；
//   · secure  —— 视作安全上下文（否则 `import()` 会被当作不安全来源拒绝）；
//   · supportFetchAPI —— 允许渲染进程 fetch（loader 的 import 走的就是它）；
//   · corsEnabled —— 上面的 fetch 需要 CORS 头（我们在响应里统一放开）。
export const SURFACE_SCHEME_PRIVILEGES = {
  scheme: SURFACE_SCHEME,
  privileges: { standard: true, secure: true, supportFetchAPI: true, corsEnabled: true },
} as const

export type SurfacePathResult =
  | { ok: true; real: string; size: number; mtimeMs: number; mime: string }
  | { ok: false; status: number; reason: string }

/** 扩展名（小写、不含点）。`foo` / `.hidden` / `a/b.js/` 都要能给出正确答案。 */
export function extOfSurfacePath(p: string): string {
  const base = p.slice(p.lastIndexOf('/') + 1).slice(p.lastIndexOf('\\') + 1)
  const i = base.lastIndexOf('.')
  return i > 0 ? base.slice(i + 1).toLowerCase() : ''
}

export function mimeOfSurfacePath(p: string): string {
  return SURFACE_MIMES[extOfSurfacePath(p)] ?? 'application/octet-stream'
}

function isSurfaceExt(p: string): boolean {
  return Object.prototype.hasOwnProperty.call(SURFACE_MIMES, extOfSurfacePath(p))
}

/**
 * resolveSurfacePath 协议的唯一判定（纯函数；单测直接调它）。
 *
 * 返回 403 = 「不许」、404 = 「没有」。两者的分界是刻意的：
 *   · 403 用于**形状非法**（.. / 绝对 / NUL / 扩展名 / id 非法）——与「文件在不在」无关；
 *   · 404 用于**合法但不存在**（插件没装 / 被禁用 / 文件没了 / 软链逃逸）。
 * 软链逃逸回 404 而不是 403：那与「目标不存在」对插件是同一件事（它没有权限知道
 * `<id>` 目录外面有什么）。
 */
export function resolveSurfacePath(
  rawID: string,
  rawPathname: string,
  index: SurfaceIndex,
  root: string = surfaceRoot(),
): SurfacePathResult {
  const id = String(rawID ?? '')
  if (!SURFACE_ID_RE.test(id)) return { ok: false, status: 403, reason: 'id 非法' }
  if (typeof rawPathname !== 'string' || rawPathname === '') return { ok: false, status: 400, reason: '空路径' }
  if (rawPathname.includes('\0')) return { ok: false, status: 403, reason: '路径非法（NUL）' }
  let decoded: string
  try {
    decoded = decodeURIComponent(rawPathname) // 只解一次，绝不再解
  } catch {
    return { ok: false, status: 400, reason: '路径编码非法' }
  }
  if (decoded.includes('\0')) return { ok: false, status: 403, reason: '路径非法（NUL）' }
  if (decoded.includes('\\')) return { ok: false, status: 403, reason: '路径非法（反斜杠）' }
  if (!decoded.startsWith('/')) return { ok: false, status: 403, reason: '路径必须是绝对路径' }
  const segs = decoded.split('/')
  if (segs.includes('..')) return { ok: false, status: 403, reason: '路径非法（上跳）' }
  if (decoded.endsWith('/')) return { ok: false, status: 403, reason: '路径非法（尾斜杠）' }
  // 扩展名在 stat 之前：非白名单文件连「存不存在」都不该被探测出来。
  if (!isSurfaceExt(decoded)) return { ok: false, status: 403, reason: '不是插件资产（扩展名不在白名单）' }

  // 只有 enabled 的插件有字节。index 里没有 / enabled=false 一律 404：禁用是「能力消失」，
  // 而不是「面板看不见但文件还能取」——后者会让「禁用」变成一句空话。
  const item = index.items.find((it) => it.id === id)
  if (!item || !item.enabled) return { ok: false, status: 404, reason: '插件不存在或未启用' }

  const base = path.join(root, id)
  const abs = path.resolve(base, '.' + decoded) // 去掉开头那个 '/' 再相对解析
  if (abs !== base && !abs.startsWith(base + path.sep)) {
    return { ok: false, status: 403, reason: '路径逃出了插件目录' }
  }
  let realBase: string
  let real: string
  try {
    realBase = fs.realpathSync(base)
    real = fs.realpathSync(abs) // 软链在这里被解开
  } catch {
    return { ok: false, status: 404, reason: '文件不存在' }
  }
  // realpath 之后再判一次扩展名：上面判的是 abs（软链名），读字节用的是 real。
  // `x/panel.js -> ~/secret.json` 会过 abs 的白名单、realpath 成 .json —— 不重判就等于
  // 「白名单只管文件名不管内容」。
  if (!isSurfaceExt(real)) return { ok: false, status: 403, reason: '目标不是插件资产（软链指向其它类型）' }
  if (real !== realBase && !real.startsWith(realBase + path.sep)) {
    return { ok: false, status: 404, reason: '文件不存在（软链逃出了插件目录）' }
  }
  let st: fs.Stats
  try {
    st = fs.statSync(real)
  } catch {
    return { ok: false, status: 404, reason: '文件不存在' }
  }
  if (!st.isFile()) return { ok: false, status: 403, reason: '不是普通文件' }
  return { ok: true, real, size: st.size, mtimeMs: st.mtimeMs, mime: mimeOfSurfacePath(real) }
}

// —— 生产入口：Request → Response ——

export interface SurfaceRequestLike {
  method: string
  url: string
}

export interface SurfaceResponse {
  status: number
  headers: Record<string, string>
  body?: Uint8Array
}

function plain(status: number, reason: string): SurfaceResponse {
  // 不回 body 的细节（原因只留在主进程日志与测试里）：协议的错误文案是给浏览器看的，
  // 插件不该从中读出「这个 id 存不存在」的额外线索 —— 状态码已经表达了全部必要信息。
  return {
    status,
    headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
    body: new TextEncoder().encode(`${status} ${reason}`),
  }
}

/**
 * handleSurfaceRequest 生产入口（主进程的 protocol.handle 只做 Request→这里→Response 的翻译）。
 *
 * 缓存策略：`no-store`。URL 里已经带 `?v=<hash>`（loader 拼的），所以「内容变了要重新拉」
 * 由 URL 身份回答；给 `Cache-Control` 任期只会让「卸载后仍然拿到旧字节」多一种可能。
 */
export function handleSurfaceRequest(req: SurfaceRequestLike, index?: SurfaceIndex): SurfaceResponse {
  const method = String(req?.method ?? '').toUpperCase()
  if (method !== 'GET' && method !== 'HEAD') return plain(405, 'method not allowed')
  const rawUrl = String(req?.url ?? '')
  if (rawUrl.includes('\0')) return plain(403, '路径非法（NUL）')
  let u: URL
  try {
    u = new URL(rawUrl)
  } catch {
    return plain(400, 'bad url')
  }
  if (u.protocol !== `${SURFACE_SCHEME}:`) return plain(400, 'bad scheme')
  // host 段就是插件 id（standard scheme 会把它小写化；id 规则本身要求小写，所以无歧义）。
  const target = resolveSurfacePath(u.hostname || u.host, u.pathname, index ?? readSurfaceIndex())
  if (!target.ok) return plain(target.status, target.reason)

  let bytes: Uint8Array
  try {
    bytes = fs.readFileSync(target.real)
  } catch {
    return plain(404, '文件不存在')
  }
  return {
    status: 200,
    headers: {
      'content-type': target.mime,
      'content-length': String(bytes.length),
      // 读的是 update 那之后的字节，不是 stat 时那一版：两者不同就说明有人正在替换它，
      // 这一份响应照样完整（readFileSync 一次读），只是可能与下一次不同 —— 由 ?v= 回答。
      'cache-control': 'no-store',
      // 同一条 URL 要能被渲染进程 fetch/import（跨 origin）：只读插件资产，不涉凭据。
      'access-control-allow-origin': '*',
      'x-content-type-options': 'nosniff',
    },
    body: method === 'HEAD' ? undefined : bytes,
  }
}

/** 协议对象的最小形状（单测用假对象；生产传 Electron 的 protocol）。 */
export interface SurfaceProtocolLike {
  handle: (scheme: string, handler: (request: SurfaceRequestLike) => Response | Promise<Response>) => void
}

/**
 * registerSurfaceProtocol 在 app ready 之后调用一次。
 *
 * 测试走的是 handleSurfaceRequest（纯函数）—— 这里只做翻译，与 media 的
 * registerMediaProtocol 同一取舍，那条「翻译层也要被真跑」的教训（media 的评审裁决 3）
 * 在这里由 e2e 兜：verify_plugin_loader.mjs 的那个 panel.js 是真的从 URL import 的。
 */
export function registerSurfaceProtocol(proto?: SurfaceProtocolLike): void {
  // eslint-disable-next-line @typescript-eslint/no-var-requires
  const protocol = proto ?? (require('electron') as typeof import('electron')).protocol
  protocol.handle(SURFACE_SCHEME, (request) => {
    const r = handleSurfaceRequest({ method: request.method, url: request.url })
    return new Response(r.body ?? null, { status: r.status, headers: r.headers })
  })
}
