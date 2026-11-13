// media-protocol.ts — `hai-media://` 自定义协议：**唯一**一条把本地音视频字节喂给
// 渲染进程 `<video>/<audio>` 的通道（R08c / 01 §4.7、05 §4 D-L）。
//
// 为什么不能走 file:// 或 data URL：
//   · data URL 要把整段视频读进内存（1 小时 1080p ≈ 2-4 GB，直接把主进程撑爆）；
//   · file:// 在 dist 打包后不可用（渲染进程是 http://localhost），且**不支持 Range**
//     —— 没有 206 就没有拖动进度条（05 §4 D-L 的原话）。
// 于是：主进程注册自定义协议，按 `Range` 回 206 + `Content-Range`，用文件流而不是 Buffer。
//
// 这个文件同时是本 PR **最大的风险面**：一条能把任意路径的内容送进渲染进程的通道。
// 所以路径校验与 Range 解析都是**纯函数**、都在这里、都有单测（media-protocol.test.ts），
// 且刻意做成「先解码一次 → 拒绝 .. / NUL / 反斜杠 → 拒绝尾斜杠 → 扩展名白名单 →
// realpath 解软链 → 与已登记工作区的 realpath 比包含」的顺序：
//   · 解码只做**一次**：`%252e%252e` 解出 `%2e%2e`（字面量），不会被二次解码成 `..`；
//   · 先白名单后 realpath：非 media 扩展名连「文件在不在」都不该被探测出来（→ 403）；
//   · 软链用 realpath 解开后再比包含：`ws/link.mp4 -> /tmp/x.mp4` 必须 403；
//   · 只读：除 GET/HEAD 一律 405（协议不能变成写通道，也不能变成探测通道）。
//
// 生产入口是 handleMediaRequest()：主进程的 protocol.handle 只做「Request → 这里 → Response」
// 的翻译，单测直接调它（00 §4.7：测试必须走生产入口，不许在测试里另建等价物）。
import * as fs from 'node:fs'
import * as path from 'node:path'
import { PassThrough, type Readable } from 'node:stream'
import { isSensitiveFsPath } from './fs-guard'

// —— 方案常量 ——

export const MEDIA_SCHEME = 'hai-media'
// URL 的 host 段固定为 local（`hai-media://local/<绝对路径的每一段>`）。
// 固定 host 而不是「把绝对路径塞进 host」：host 在标准 scheme 里会被小写化，
// `C:\Users\Me` 之类的路径会悄悄变样；路径放 pathname 才能逐段 encode/decode。
export const MEDIA_HOST = 'local'

// registerSchemesAsPrivileged 的入参必须在 app ready **之前**交给 Electron，
// 所以它是一个导出的常量：main.ts 在模块顶层调用，这里只负责定义。
//   · standard  —— 按标准 URL 解析（有 host/path），且是独立 origin；
//   · secure    —— 视作安全上下文（否则 Chromium 拒绝在 <video> 里加载）；
//   · stream    —— 允许响应体是流（不缓冲整个文件 —— 这是 1 小时 mp4 能播的前提）；
//   · supportFetchAPI —— 允许渲染进程/会话 fetch 同一 URL（探针用它断言 206）；
//   · corsEnabled —— 上面那条 fetch 需要 CORS 头（见 handleMediaRequest）。
export const MEDIA_SCHEME_PRIVILEGES = {
  scheme: MEDIA_SCHEME,
  privileges: { standard: true, secure: true, stream: true, supportFetchAPI: true, corsEnabled: true },
} as const

// 扩展名 → MIME。**这就是协议的扩展名白名单**：不在表里的一律 403，与「文件是否存在于
// 工作区」无关 —— 协议只服务媒体，不能变成任意文件读取通道。
// 与 bridge 的编解码矩阵（desktop/bridge/media_matrix.go）保持同一批扩展名：
// 那边判「能不能直放」，这边判「给不给字节」，两边都只认媒体容器。
export const MEDIA_MIMES: Record<string, string> = {
  mp4: 'video/mp4',
  m4v: 'video/mp4',
  mov: 'video/quicktime',
  webm: 'video/webm',
  ogv: 'video/ogg',
  mkv: 'video/x-matroska',
  avi: 'video/x-msvideo',
  ts: 'video/mp2t',
  m2ts: 'video/mp2t',
  mts: 'video/mp2t',
  '3gp': 'video/3gpp',
  mp3: 'audio/mpeg',
  m4a: 'audio/mp4',
  aac: 'audio/aac',
  wav: 'audio/wav',
  ogg: 'audio/ogg',
  oga: 'audio/ogg',
  opus: 'audio/ogg',
  flac: 'audio/flac',
  wma: 'audio/x-ms-wma',
}

/** 扩展名（小写、不含点）。`foo` / `.gitignore` / `a/b.mp4/` 都要能给出正确答案。 */
export function extOfMediaPath(p: string): string {
  const base = p.slice(p.lastIndexOf('/') + 1).slice(p.lastIndexOf('\\') + 1)
  const i = base.lastIndexOf('.')
  return i > 0 ? base.slice(i + 1).toLowerCase() : ''
}

/** 是否媒体扩展名（协议白名单 + 渲染器注册表共用的判据）。 */
export function isMediaExt(p: string): boolean {
  return Object.prototype.hasOwnProperty.call(MEDIA_MIMES, extOfMediaPath(p))
}

export function mimeOfMediaPath(p: string): string {
  return MEDIA_MIMES[extOfMediaPath(p)] ?? 'application/octet-stream'
}

// —— Range 解析（纯函数）——

export type RangePlan =
  /** 200：没有 Range、Range 语法不认识、或**多段** Range（下面的注释解释为什么回整文件）。 */
  | { kind: 'entire' }
  /** 206：`bytes=start-end`（end 已 clamp 到 size-1，含两端）。 */
  | { kind: 'partial'; start: number; end: number }
  /** 416：起点越过文件尾 / 后缀长度为 0 / 空文件上的任何 Range。 */
  | { kind: 'unsatisfiable' }

// 多段 Range（`bytes=0-1,5-6`）**明确回 200 整文件**，而不是 206 + multipart/byteranges：
//   · RFC 9110 §14.2 允许服务端忽略 Range，回 200 是合法的「我不做分段」；
//   · 媒体播放器（Chromium/Firefox 的 demuxer）只发单段，多段是下载器的形态；
//   · multipart/byteranges 要自己拼 boundary + 每段头部，是这个 PR 里最不值当的复杂度，
//     而一旦拼错，播放器拿到的是坏字节 —— 不如整文件（正确但慢）而不是（快但可能坏）。
// 同理，语法非法（`bytes=abc`、`bytes=100-50`、`bytes=-`）一律按「忽略 Range」→ 200。
const RANGE_RE = /^bytes=(\d*)-(\d*)$/

export function planRange(header: string | null | undefined, size: number): RangePlan {
  if (typeof header !== 'string') return { kind: 'entire' }
  const raw = header.trim()
  if (!raw) return { kind: 'entire' }
  // 大小写不敏感的单位（HTTP 字段值里的 token 大小写不敏感；`Bytes=` 也是合法的）。
  const m = RANGE_RE.exec(raw.toLowerCase())
  if (!m) return { kind: 'entire' } // 含多段（有逗号）、未知单位、语法错误
  const [, rawStart, rawEnd] = m
  if (!rawStart && !rawEnd) return { kind: 'entire' } // `bytes=-` 无意义 → 忽略
  if (size <= 0) return { kind: 'unsatisfiable' } // 空文件上任何 Range 都不可满足
  if (!rawStart) {
    // 后缀形式 `bytes=-N`：最后 N 字节。N=0 不可满足（RFC 9110 §14.1.2）。
    const suffix = Number(rawEnd)
    if (!Number.isSafeInteger(suffix) || suffix <= 0) return { kind: 'unsatisfiable' }
    const start = Math.max(0, size - suffix)
    return { kind: 'partial', start, end: size - 1 }
  }
  const start = Number(rawStart)
  if (!Number.isSafeInteger(start)) return { kind: 'entire' }
  const end = rawEnd ? Number(rawEnd) : size - 1
  if (!Number.isSafeInteger(end)) return { kind: 'entire' }
  if (rawEnd && end < start) return { kind: 'entire' } // 语法非法（倒序）→ 忽略 Range
  if (start >= size) return { kind: 'unsatisfiable' } // 越界 → 416
  return { kind: 'partial', start, end: Math.min(end, size - 1) }
}

// —— 路径校验（纯函数 + 一份进程内的工作区白名单）——

// 允许的根目录（realpath）。唯一写入点是 mediaUrlForFile（生产）→ allowMediaRoot。
// 根 = 用户真的打开过的工作区目录，不是渲染进程随便给的任意字符串。
//
// **没有上限、没有淘汰**（复评裁决 1）：这里曾经有 `MAX_ROOTS = 8` + `slice(-8)`，后果是
// 开 9 个工作区的用户一登记新根，最先那个根立刻 403 —— 正在播的视频当场断流。根的生命周期
// 现在只有一条：工作区被移除时（IPC workspace:forget）→ 清单里删掉、下次不再登记。
// 顺序只用于「诊断可读性」：最近登记的在最前。
let roots: string[] = []

export function allowedMediaRoots(): readonly string[] {
  return roots
}

/**
 * 登记一个允许的根目录（realpath 规范化）。失败时不写入。
 *
 * **刻意不 export**（评审裁决 1）：根从哪来，是这条协议唯一的信任边界。以前它是导出的，
 * 谁都能登记任意目录；现在唯一的写入点是 mediaUrlForFile，而后者要求调用方给出
 * 「这个 workspace 是不是用户真的打开过」的谓词（生产里 = known-workspaces 那份清单）。
 */
function allowMediaRoot(dir: string): { ok: boolean; root?: string; error?: string } {
  if (typeof dir !== 'string' || !dir || !path.isAbsolute(dir)) return { ok: false, error: '工作区路径必须是绝对路径' }
  let real: string
  try {
    real = fs.realpathSync(dir)
  } catch {
    return { ok: false, error: `工作区不存在: ${dir}` }
  }
  try {
    if (!fs.statSync(real).isDirectory()) return { ok: false, error: `工作区不是目录: ${dir}` }
  } catch {
    return { ok: false, error: `工作区不可读: ${dir}` }
  }
  // 敏感目录（~/.ssh/.aws/...）即使被当成工作区打开也不做媒体根：与 fs-guard 同一份判据。
  if (isSensitiveFsPath(real)) return { ok: false, error: '该目录受保护' }
  // 去重 + 提到最前（不截断）：重复登记不该让已有根的位置/存在性发生变化
  roots = [real, ...roots.filter((r) => r !== real)]
  return { ok: true, root: real }
}

export function resetMediaProtocolState(): void {
  roots = []
  stats = { requests: 0, rangeRequests: 0, firstByteHits: 0, bytesServed: 0 }
}

export type MediaPathError = { ok: false; status: number; reason: string }
export type MediaPathTarget = { ok: true; real: string; size: number; mtimeMs: number; mime: string }
export type MediaPathResult = MediaPathTarget | MediaPathError

/**
 * URL pathname → 可服务的真实文件。**这是本 PR 最关键的一段**。
 *
 * 顺序是刻意的（每一条都对应一个逃逸手段，见 media-protocol.test.ts 的 8 条）：
 *   1. 解码**恰好一次**（`%252e` 不会变成 `.`）；解码失败 = 400（不是 500）。
 *   2. NUL/反斜杠/`..` 段 → 403。NUL 会截断 C 层路径；反斜杠在 Windows 上是分隔符，
 *      在 POSIX 上是合法文件名字符 —— 一律拒绝比「按平台分支」更难写错。
 *   3. 尾斜杠 → 403：`.../movie.mp4/` 在 POSIX 上不是那个文件（ENOTDIR），
 *      但**看起来**像，不明确拒绝就会变成「有时能看有时不能看」。
 *   4. 扩展名白名单 → 403：这一步在 stat 之前，所以非媒体文件连「存不存在」都不泄露。
 *   5. realpath（解软链）失败 = 404；然后比包含 —— 软链逃逸、工作区外一律 403。
 *   6. 必须是普通文件（目录/设备文件 403）。
 */
export function resolveMediaPath(rawPathname: string): MediaPathResult {
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
  const abs = path.resolve(decoded)
  if (!isMediaExt(abs)) return { ok: false, status: 403, reason: '不是媒体文件（扩展名不在白名单）' }
  let real: string
  try {
    real = fs.realpathSync(abs) // 软链在这里被解开；逃逸由下面的包含判断挡住
  } catch {
    return { ok: false, status: 404, reason: '文件不存在' }
  }
  // realpath 之后再判一次扩展名：上面判的是 abs（软链名），读字节用的是 real。
  // `ws/x.mp4 -> ~/foo.json` 会过 abs 的白名单、realpath 成 .json —— 不重判就等于
  // 「白名单只管文件名不管内容」。
  if (!isMediaExt(real)) return { ok: false, status: 403, reason: '目标不是媒体文件（软链指向其它类型）' }
  if (roots.length === 0) return { ok: false, status: 403, reason: '没有已登记的工作区' }
  const inside = roots.some((r) => real === r || real.startsWith(r + path.sep))
  if (!inside) return { ok: false, status: 403, reason: '文件在工作区外' }
  if (isSensitiveFsPath(real)) return { ok: false, status: 403, reason: '该路径受保护' }
  let st: fs.Stats
  try {
    st = fs.statSync(real)
  } catch {
    return { ok: false, status: 404, reason: '文件不存在' }
  }
  if (!st.isFile()) return { ok: false, status: 403, reason: '不是普通文件' }
  return { ok: true, real, size: st.size, mtimeMs: st.mtimeMs, mime: mimeOfMediaPath(real) }
}

// —— URL 构造（生产：mediaUrlForFile；测试：mediaUrlFor）——

/** 绝对路径 → `hai-media://local/...`（逐段 encode，`%` 会被编成 `%25`，所以文件名里的 `%2e` 不会变成 `..`）。 */
export function mediaUrlFor(abs: string): string {
  const segs = abs.split(/[\\/]+/).filter((s) => s !== '')
  return `${MEDIA_SCHEME}://${MEDIA_HOST}/` + segs.map((s) => encodeURIComponent(s)).join('/')
}

/**
 * mediaUrlForFile：渲染进程要 URL 的唯一入口（走 IPC `media:url`）。
 * 与 bridge 的 resolveWorkspacePath 同一条口径：相对路径锚定 workspace（`..` 逃逸拒绝），
 * 绝对路径原样采用 —— 但**两种都必须落在 workspace 内**，因为协议只服务工作区内。
 * 成功时顺带把 workspace 登记成允许的根。
 */
export function mediaUrlForFile(
  rawPath: string,
  workspace: string,
  isTrustedWorkspace: (realWorkspace: string) => boolean,
): { ok: boolean; url?: string; abs?: string; error?: string } {
  const raw = String(rawPath ?? '')
  const ws = String(workspace ?? '')
  if (!raw) return { ok: false, error: 'path 为空' }
  if (!ws || !path.isAbsolute(ws)) return { ok: false, error: 'workspace 为空' }
  // 信任判断在**登记根之前**：不是用户亲手打开过的工作区，连根都登记不上。
  let realWs: string
  try {
    realWs = fs.realpathSync(ws)
  } catch {
    return { ok: false, error: '工作区未登记（只能用系统选择器打开过的工作区预览媒体）' }
  }
  if (!isTrustedWorkspace(realWs)) {
    return { ok: false, error: '工作区未登记（只能用系统选择器打开过的工作区预览媒体）' }
  }
  const allowed = allowMediaRoot(realWs)
  if (!allowed.ok) return { ok: false, error: allowed.error }
  const abs = path.isAbsolute(raw) ? path.resolve(raw) : path.resolve(ws, raw)
  if (!isMediaExt(abs)) return { ok: false, error: '不是媒体文件（扩展名不在白名单）' }
  let real: string
  try {
    real = fs.realpathSync(abs)
  } catch {
    return { ok: false, error: `无法读取文件: ${abs}` }
  }
  // 与 resolveMediaPath 同一条：软链到非媒体文件时不给 URL（否则渲染层拿到的 URL 会 403，
  // 面板只能显示「放不了」而说不出为什么）。
  if (!isMediaExt(real)) return { ok: false, error: '目标不是媒体文件（软链指向其它类型）' }
  // 敏感目录（~/.ssh 等）即使在工作区内也不给 URL —— 与协议侧的判定保持一致，
  // 让面板在「拿不到 URL」时就说出真正的原因。
  if (isSensitiveFsPath(real)) return { ok: false, error: '该路径受保护（含敏感凭据）' }
  const root = allowed.root as string
  if (!(real === root || real.startsWith(root + path.sep))) return { ok: false, error: '媒体文件在工作区外（协议只服务工作区内的文件）' }
  return { ok: true, url: mediaUrlFor(real), abs: real }
}

// —— 生产入口：Request → Response ——

export interface MediaRequestLike {
  method: string
  url: string
  range?: string | null
  /** `If-None-Match`：命中就回 304（Chromium 的媒体缓存会拿它做重验证）。 */
  ifNoneMatch?: string | null
  /** `If-Range`：不命中 = 服务端这份文件已经换过了 → **忽略 Range 回 200 整文件**（RFC 9110 §13.1.5）。 */
  ifRange?: string | null
}

export interface MediaResponse {
  status: number
  headers: Record<string, string>
  /** HEAD / 416 / 405 无体；其余是文件流（不 Buffer 整个文件）。 */
  body?: Readable | Uint8Array
}

// 主进程计数（e2e 用它证明「切走再切回不重新请求首字节」——不是看像素，是看协议有没有被打）。
// 计数放在协议自己身上而不是 e2e 里另建一份：e2e 要观察的就是生产里真正处理请求的那段代码。
export interface MediaStats {
  requests: number
  rangeRequests: number
  firstByteHits: number
  bytesServed: number
}
let stats: MediaStats = { requests: 0, rangeRequests: 0, firstByteHits: 0, bytesServed: 0 }

// 计数**挂在流上**而不是挂在「计划」上（评审裁决 3 的第二条）：客户端 seek/abort 时
// 计划里那 N 字节并没有真的发出去，按计划记数就成了「自报数字」——而 e2e 正拿它当
// 「切走切回不重下」的判据。只有 'data' 真的流过去才算数。
//
// 返回的是 **PassThrough** 而不是源流本身（复评 4）：给源流挂 'data' 会把它切进 flowing
// 模式，下游（Response body）如果没在同一个 tick 接上，数据就那么在中间被消费掉了。
// 中间垫一个 PassThrough：pipe 自带背压、能缓冲，下游什么时候接上都行；计数挂在源流上
// （pipe 已接好，额外的 'data' 监听只是旁听，不影响投递）。
function countedStream(src: Readable, isFromFirstByte: boolean): Readable {
  const out = new PassThrough()
  src.pipe(out)
  let sawFirst = false
  src.on('data', (chunk: Buffer | string) => {
    stats.bytesServed += typeof chunk === 'string' ? Buffer.byteLength(chunk) : chunk.length
    if (!sawFirst) {
      sawFirst = true
      if (isFromFirstByte) stats.firstByteHits += 1
    }
  })
  return out
}

// etag 用**强验证器**（不带 W/）：If-Range 只认强验证器，弱 etag 永远不匹配（RFC 9110 §13.1.5）。
function etagOf(size: number, mtimeMs: number): string {
  return `"${size.toString(16)}-${Math.floor(mtimeMs).toString(16)}"`
}

function etagMatches(header: string, etag: string): boolean {
  const norm = (v: string): string => v.trim().replace(/^W\//, '')
  const target = norm(etag)
  return header.split(',').some((raw) => {
    const v = raw.trim()
    return v === '*' || norm(v) === target
  })
}

function ifRangeMatches(header: string, etag: string, lastModified: string): boolean {
  const v = header.trim()
  if (v.startsWith('"') || v.startsWith('W/')) return etagMatches(v, etag)
  return v === lastModified
}

export function mediaStats(): MediaStats {
  return { ...stats }
}

// fileBody 建文件流。为什么要单独一层：`stat` 与 `open` 之间文件可能被删/改权限
//（重新导出、用户在 Finder 里删掉正在播的临时文件），流会**异步** emit 'error'。
// 没人监听时 Node 会把它当 uncaughtException —— 主进程直接退出。这里只记一行日志：
// 播放器那一侧本来就会以「播放中断」的形式表现，不值得把整个应用带走。
function fileBody(real: string, opts?: { start?: number; end?: number }): Readable {
  const s = fs.createReadStream(real, opts)
  s.on('error', (e) => {
    // 已经被调用方 destroy 掉的流（客户端 seek/切标签时 Chromium 会中止请求）不该再报
    // 一次「读取失败」：那是一次正常的提前关闭，日志里冒出来的只是噪音。
    if (s.destroyed) return
    console.warn('[media] 读取失败', real, (e as Error).message)
  })
  return s
}

function plain(status: number, reason: string, extra: Record<string, string> = {}): MediaResponse {
  return {
    status,
    headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store', ...extra },
    body: new TextEncoder().encode(reason + '\n'),
  }
}

/**
 * handleMediaRequest：协议的**生产实现**（Electron 的 protocol.handle 只做 Response 翻译）。
 * 纯 Node 可测（不 import electron），所以单测跑的就是线上那条判断链。
 */
export function handleMediaRequest(req: MediaRequestLike): MediaResponse {
  const method = String(req?.method ?? '').toUpperCase()
  stats.requests += 1
  if (method !== 'GET' && method !== 'HEAD') {
    return plain(405, `method ${method || '(empty)'} not allowed`, { allow: 'GET, HEAD' })
  }
  const rawUrl = String(req?.url ?? '')
  // 字面量 NUL 在 URL 解析**之前**就要挡：URL 解析器会把它当非法字符抛错（→400），
  // 而这里要的是「路径非法」这条一致的 403。
  if (rawUrl.includes('\0')) return plain(403, '路径非法（NUL）')
  let u: URL
  try {
    u = new URL(rawUrl)
  } catch {
    return plain(400, 'bad url')
  }
  if (u.protocol !== `${MEDIA_SCHEME}:`) return plain(400, 'bad scheme')
  if (u.host !== MEDIA_HOST) return plain(403, 'bad host')
  const target = resolveMediaPath(u.pathname)
  if (!target.ok) return plain(target.status, target.reason)
  const etag = etagOf(target.size, target.mtimeMs)
  const lastModified = new Date(target.mtimeMs).toUTCString()
  const common = {
    'content-type': target.mime,
    'accept-ranges': 'bytes',
    etag,
    'last-modified': lastModified,
    // 媒体文件会被覆盖（重新导出/渲染），所以不许静默复用：no-cache = 每次用前先问。
    'cache-control': 'no-cache',
    // 同一条 URL 既给 <video>（no-cors）也给探针 fetch（cors），这里统一放开：
    // 工作区内的只读媒体，不涉及凭据。
    'access-control-allow-origin': '*',
  }
  // 条件请求：命中就 304（无体）。不做这一步的话，etag 白给 —— 浏览器每次重验证都要
  // 重新拉一遍字节，缓存形同虚设。
  if (req.ifNoneMatch && etagMatches(req.ifNoneMatch, etag)) {
    // 304 **不带 content-length**（RFC 9110 §15.4.5：它没有 body，而 content-length 描述的是
    // 「这个响应的 body 长度」，写 0 会让缓存/代理以为表示本身是空的）。
    const { 'content-type': _ct, ...cacheHeaders } = common
    return { status: 304, headers: cacheHeaders }
  }
  const header = req.range ?? null
  if (header != null) stats.rangeRequests += 1
  // If-Range 不命中 = 客户端手里那份已经过期 → 忽略 Range 回整文件（RFC 9110 §13.1.5），
  // 否则客户端会把新文件的字节拼进旧的缓冲里（音视频最常见的是「拖完变雪花/爆音」）。
  const plan = req.ifRange && !ifRangeMatches(req.ifRange, etag, lastModified)
    ? ({ kind: 'entire' } as const)
    : planRange(header, target.size)
  if (plan.kind === 'unsatisfiable') {
    return {
      status: 416,
      headers: { ...common, 'content-range': `bytes */${target.size}`, 'content-length': '0' },
    }
  }
  if (plan.kind === 'entire') {
    return {
      status: 200,
      headers: { ...common, 'content-length': String(target.size) },
      body: method === 'HEAD' ? undefined : countedStream(fileBody(target.real), true),
    }
  }
  const { start, end } = plan
  const len = end - start + 1
  return {
    status: 206,
    headers: {
      ...common,
      'content-range': `bytes ${start}-${end}/${target.size}`,
      'content-length': String(len),
    },
    body: method === 'HEAD' ? undefined : countedStream(fileBody(target.real, { start, end }), start === 0),
  }
}

// —— Electron 接线（生产侧只有这几行；Node 单测不 import 这一支）——

/**
 * registerMediaProtocol：在 app ready 之后调用一次。
 * 用 require 而不是顶层 import —— media-protocol.test.ts 在纯 Node 里跑，顶层 import
 * 'electron' 会让它在没有 Electron 的进程里直接崩（那个模块在 Node 里是字符串路径）。
 */
export interface MediaProtocolRequestLike {
  method: string
  url: string
  headers: { get(name: string): string | null }
}

/** Electron 的 `protocol` 只需要 handle 这一个方法（测试注入假实现，见 media-protocol.test.ts）。 */
export interface MediaProtocolLike {
  handle(scheme: string, handler: (request: MediaProtocolRequestLike) => Response | Promise<Response>): void
}

export function registerMediaProtocol(proto?: MediaProtocolLike): void {
  // eslint-disable-next-line @typescript-eslint/no-var-requires
  const protocol = proto ?? (require('electron') as typeof import('electron')).protocol
  // eslint-disable-next-line @typescript-eslint/no-var-requires
  const { Readable } = require('node:stream') as typeof import('node:stream')
  protocol.handle(MEDIA_SCHEME, (request) => {
    // 三个头都要透传：range 决定 206/416，if-none-match 决定 304，if-range 决定
    // 「Range 还算不算数」。这里少一个，下面的单测就直接调 handleMediaRequest 去了，
    // 而生产走的是这条翻译层（F52/F53 的同形状：测试验的必须就是生产跑的那一段）。
    const r = handleMediaRequest({
      method: request.method,
      url: request.url,
      range: request.headers.get('range'),
      ifNoneMatch: request.headers.get('if-none-match'),
      ifRange: request.headers.get('if-range'),
    })
    if (!r.body) return new Response(null, { status: r.status, headers: r.headers })
    const body = r.body instanceof Uint8Array ? r.body : (Readable.toWeb(r.body as Readable) as unknown as ReadableStream)
    return new Response(body, { status: r.status, headers: r.headers })
  })
}
