// media-protocol.test.ts — `hai-media://` 协议单测：node:test，纯 Node **不启动 Electron**
// （协议的判断链是纯函数，见 media-protocol.ts 顶部注释）。
//
// 为什么这一份必须存在（00 §4.7）：这条通路是「渲染进程拿 URL → 主进程把本地文件送过去」，
// 也就是本 PR 唯一一个能把任意路径内容变成可读字节的入口。它的拒绝分支在 UI 里几乎不可
// 复现（要造软链逃逸、编码后的 ..、Windows 反斜杠……），而一旦写宽，任何 `<video src>` 都能
// 变成任意文件读取。所以每条逃逸不仅断言**状态码**，还断言**响应体里没有那几份机密内容**
// ——「403 了但字节已经发出去了」是评审裁决 3 指出的最危险的一种假绿。
//
// 覆盖四块：
//   ① Range 解析（0-1023 / 1024- / -512 / 越界 / 多段 / 语法错 / 空文件）；
//   ② 生产入口 handleMediaRequest 的响应（200/206/416/304/405/HEAD + 头齐全 + 字节内容）；
//   ③ 路径校验的 8 种逃逸（.. / %2e%2e / %2E%2E / %2f / 绝对路径 / 软链 / NUL / 尾斜杠）
//      + 软链到非媒体 + 未登记工作区；
//   ④ protocol.handle 那一层：用假 protocol 捕获真实 handler，证明 Range/If-* 头被透传
//      （评审裁决 3 的最后一条：只测 handleMediaRequest 会绕过生产里的翻译层）。
//
// 运行：esbuild bundle 后 node --test（见 package.json test:media-protocol）。
import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import {
  MEDIA_MIMES,
  handleMediaRequest,
  isMediaExt,
  mediaStats,
  mediaUrlFor,
  mediaUrlForFile,
  mimeOfMediaPath,
  planRange,
  registerMediaProtocol,
  resetMediaProtocolState,
  resolveMediaPath,
  type MediaRequestLike,
  type MediaResponse,
} from './media-protocol.ts'
import { MEDIA_VIDEO_EXTS } from '../src/lib/mediaFile.ts'
// 生产的信任谓词（不是 ()=>true 桩，复评 4）：单测也走 known-workspaces 那份真清单
import { configureKnownWorkspaces, isKnownWorkspace, rememberWorkspace } from './known-workspaces.ts'
// Go 侧的容器矩阵以**文本**在打包时内联（esbuild --loader:.go=text）：它和协议白名单
// 必须是同一批扩展名，否则「矩阵判得出、协议不给字节」或反过来。读源码文本而不是
// 再抄一份清单 —— 抄一份就等于加了三份要同步的表。
// @ts-expect-error 非 JS 模块，由 esbuild 的 text loader 提供
import matrixGoSource from '../../bridge/media_matrix.go'

// 工作区 / 工作区外 / 假 HOME 都是**真的另一个临时目录**：只有真的另一个目录才证明包含判断
// 生效（兄弟路径即使被误判也可能刚好通过）。
const root = fs.mkdtempSync(path.join(os.tmpdir(), 'media-proto-ws-'))
const outside = fs.mkdtempSync(path.join(os.tmpdir(), 'media-proto-out-'))
const fakeHome = fs.mkdtempSync(path.join(os.tmpdir(), 'media-proto-home-'))
const oldHome = process.env.HOME

const bytes = (n: number): Buffer => Buffer.from(Array.from({ length: n }, (_, i) => i % 251))
// 这三份字符串是「不该被送出去」的判据：每条逃逸用例都要断言响应体里没有它们。
const SECRETS = ['OUTSIDE-SECRET', 'PRIVATE-KEY', 'TOP-SECRET-TEXT']
const assertClean = (body: Buffer, where: string) => {
  const text = body.toString('latin1')
  for (const s of SECRETS) assert.ok(!text.includes(s), `${where} 的响应体里出现了 ${s}（拒绝必须是拒绝，不能先送字节）`)
}

// 登记工作区的根走**生产入口**（allowMediaRoot 已降为模块私有，评审裁决 1）：
// mediaUrlForFile 是唯一会给 URL 的函数，它顺带登记根。
// 第三个参数用**真的** isKnownWorkspace（复评 4）：之前是 ()=>true 桩，等于单测只验了
// 「谓词返回 true 会怎样」，生产里那条接线只有跑不动的 e2e 覆盖。现在先把目录记进
// known-workspaces（= 用户亲手选过），再登记根。
const knownFile = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'media-known-')), 'workspaces.json')
function registerWorkspace(dir: string, trusted = true): { ok: boolean; error?: string } {
  if (trusted) rememberWorkspace(dir)
  const sample = fs.existsSync(path.join(dir, 'clip.mp4')) ? 'clip.mp4' : path.join(dir, 'song.MP3')
  return mediaUrlForFile(sample, dir, isKnownWorkspace)
}

before(() => {
  process.env.HOME = fakeHome
  configureKnownWorkspaces(knownFile)
  fs.writeFileSync(path.join(root, 'clip.mp4'), bytes(5000))
  fs.writeFileSync(path.join(root, 'song.MP3'), bytes(1000))
  fs.writeFileSync(path.join(root, 'notes.txt'), 'TOP-SECRET-TEXT')
  fs.writeFileSync(path.join(root, '带 空格#井号.mp4'), bytes(64))
  fs.mkdirSync(path.join(root, 'sub'))
  fs.writeFileSync(path.join(root, 'sub', 'b.webm'), bytes(128))
  // 工作区外的真媒体文件（软链与绝对路径两种逃逸都指向它）
  fs.writeFileSync(path.join(outside, 'secret.mp4'), 'OUTSIDE-SECRET')
  // 软链：工作区里的 .mp4 指向工作区外
  fs.symlinkSync(path.join(outside, 'secret.mp4'), path.join(root, 'link.mp4'))
  // 软链：工作区里指向工作区内的另一个文件（应当放行 —— 拒绝所有软链是过度防御）
  fs.symlinkSync(path.join(root, 'clip.mp4'), path.join(root, 'inside-link.mp4'))
  // 软链：**名字是媒体、目标是文本**（评审裁决 2：realpath 之后必须重判扩展名）
  fs.symlinkSync(path.join(root, 'notes.txt'), path.join(root, 'hidden.mp4'))
  // 假 HOME 下的敏感目录（当成工作区打开也不该成为媒体根）
  fs.mkdirSync(path.join(fakeHome, '.ssh'), { recursive: true })
  fs.writeFileSync(path.join(fakeHome, '.ssh', 'key.mp4'), 'PRIVATE-KEY')
  // 假 HOME 自己也要有一个正常媒体文件：登记根走的是生产入口 mediaUrlForFile，
  // 它要求「工作区里至少有一份能给出 URL 的媒体」（否则这条用例测的是另一条失败分支）
  fs.writeFileSync(path.join(fakeHome, 'clip.mp4'), bytes(8))
  const reg = registerWorkspace(root)
  if (!reg.ok) throw new Error(`登记工作区失败: ${reg.error}`)
})

after(() => {
  process.env.HOME = oldHome
  for (const p of [root, outside, fakeHome]) fs.rmSync(p, { recursive: true, force: true })
})

const url = (p: string) => mediaUrlFor(p)
/** 收完整个响应体（200/206 是文件流，HEAD/416/304 无体）。 */
async function bodyOf(r: MediaResponse): Promise<Buffer> {
  if (!r.body) return Buffer.alloc(0)
  if (r.body instanceof Uint8Array) return Buffer.from(r.body)
  const chunks: Buffer[] = []
  for await (const c of r.body) chunks.push(Buffer.from(c as Buffer))
  return Buffer.concat(chunks)
}
const reqOf = (p: string, extra: Partial<MediaRequestLike> = {}, method = 'GET'): MediaRequestLike => ({
  method,
  url: p.startsWith('hai-media://') ? p : url(p),
  ...extra,
})
const get = (p: string, range?: string, method = 'GET') => handleMediaRequest(reqOf(p, { range }, method))
/** 状态码 + body（逃逸用例两者都要断言）。 */
async function call(p: string, extra: Partial<MediaRequestLike> = {}) {
  const r = handleMediaRequest(reqOf(p, extra))
  return { status: r.status, headers: r.headers, body: await bodyOf(r) }
}
const rawCall = (pathname: string, extra: Partial<MediaRequestLike> = {}) =>
  call(`hai-media://local${pathname}`, extra)

// ——————————————————————— ① Range 解析 ———————————————————————

test('planRange：无 Range / 未知单位 / 多段 / 语法错 → 200 整文件', () => {
  for (const h of [null, undefined, '', 'items=0-1', 'bytes=abc', 'bytes=100-50', 'bytes=-', 'bytes=0-1,5-6']) {
    assert.deepEqual(planRange(h, 5000), { kind: 'entire' }, `header=${String(h)} 应当忽略 Range`)
  }
})

test('planRange：bytes=0-1023 → 206 [0,1023]', () => {
  assert.deepEqual(planRange('bytes=0-1023', 5000), { kind: 'partial', start: 0, end: 1023 })
})

test('planRange：bytes=1024- → 206 到文件尾', () => {
  assert.deepEqual(planRange('bytes=1024-', 5000), { kind: 'partial', start: 1024, end: 4999 })
})

test('planRange：bytes=-512（后缀）→ 最后 512 字节', () => {
  assert.deepEqual(planRange('bytes=-512', 5000), { kind: 'partial', start: 4488, end: 4999 })
  // 后缀比文件还长 → 整个文件（仍按 206 回，区间就是 [0,size-1]）
  assert.deepEqual(planRange('bytes=-99999', 5000), { kind: 'partial', start: 0, end: 4999 })
})

test('planRange：end 越界要 clamp，start 越界要 416（这是两条不同的分支）', () => {
  assert.deepEqual(planRange('bytes=4990-99999', 5000), { kind: 'partial', start: 4990, end: 4999 })
  assert.deepEqual(planRange('bytes=5000-5001', 5000), { kind: 'unsatisfiable' }, 'start == size 已越界')
  assert.deepEqual(planRange('bytes=99999-', 5000), { kind: 'unsatisfiable' })
  assert.deepEqual(planRange('bytes=-0', 5000), { kind: 'unsatisfiable' }, '后缀 0 不可满足')
})

test('planRange：空文件上任何 Range 都不满足（416，不是 200）', () => {
  assert.deepEqual(planRange('bytes=0-', 0), { kind: 'unsatisfiable' })
  assert.deepEqual(planRange(null, 0), { kind: 'entire' }, '无 Range 的空文件仍是 200')
})

test('planRange：单位大小写不敏感（Bytes=0-1）', () => {
  assert.deepEqual(planRange('Bytes=0-1', 100), { kind: 'partial', start: 0, end: 1 })
})

test('planRange：不接受非整数/科学计数法（Number("1e3") 会变成 1000 —— 必须挡住）', () => {
  assert.deepEqual(planRange('bytes=1e3-', 5000), { kind: 'entire' })
  assert.deepEqual(planRange('bytes=0x10-', 5000), { kind: 'entire' })
})

// ——————————————————————— ② 生产入口的响应 ———————————————————————

test('无 Range → 200 + accept-ranges + content-length + content-type', async () => {
  const r = get(path.join(root, 'clip.mp4'))
  assert.equal(r.status, 200)
  assert.equal(r.headers['accept-ranges'], 'bytes')
  assert.equal(r.headers['content-length'], '5000')
  assert.equal(r.headers['content-type'], 'video/mp4')
  const body = await bodyOf(r)
  assert.equal(body.length, 5000)
  assert.deepEqual(body.subarray(0, 8), bytes(5000).subarray(0, 8))
})

test('Range: bytes=0-1023 → 206 + Content-Range + 1024 字节（字节内容要对）', async () => {
  const r = get(path.join(root, 'clip.mp4'), 'bytes=0-1023')
  assert.equal(r.status, 206)
  assert.equal(r.headers['content-range'], 'bytes 0-1023/5000')
  assert.equal(r.headers['content-length'], '1024')
  assert.equal(r.headers['accept-ranges'], 'bytes')
  const body = await bodyOf(r)
  assert.equal(body.length, 1024)
  assert.deepEqual(body, bytes(5000).subarray(0, 1024), '偏移必须真的落到文件里（不是回放头部）')
})

test('Range: bytes=1024- → 206，从 1024 起（证明 start 生效）', async () => {
  const r = get(path.join(root, 'clip.mp4'), 'bytes=1024-')
  assert.equal(r.status, 206)
  assert.equal(r.headers['content-range'], 'bytes 1024-4999/5000')
  const body = await bodyOf(r)
  assert.deepEqual(body, bytes(5000).subarray(1024))
})

test('Range: bytes=-512 → 206，内容确实是尾部 512 字节', async () => {
  const r = get(path.join(root, 'clip.mp4'), 'bytes=-512')
  assert.equal(r.status, 206)
  assert.equal(r.headers['content-range'], 'bytes 4488-4999/5000')
  assert.deepEqual(await bodyOf(r), bytes(5000).subarray(4488))
})

test('416 边界：start == size → 416 + Content-Range: bytes */size + 空体', async () => {
  const r = get(path.join(root, 'clip.mp4'), 'bytes=5000-6000')
  assert.equal(r.status, 416)
  assert.equal(r.headers['content-range'], 'bytes */5000')
  assert.equal((await bodyOf(r)).length, 0)
})

test('多段 Range → 200 整文件（明确口径：不做 multipart/byteranges）', async () => {
  const r = get(path.join(root, 'clip.mp4'), 'bytes=0-1,5-6')
  assert.equal(r.status, 200)
  assert.equal(r.headers['content-length'], '5000')
  assert.equal(r.headers['content-range'], undefined)
})

test('HEAD：同头、无体（播放器/探针会先发它）', async () => {
  const r = get(path.join(root, 'clip.mp4'), 'bytes=0-99', 'HEAD')
  assert.equal(r.status, 206)
  assert.equal(r.headers['content-range'], 'bytes 0-99/5000')
  assert.equal(r.headers['content-length'], '100')
  assert.equal(r.body, undefined)
})

test('If-None-Match 命中 etag → 304 无体（etag 不能白给）', async () => {
  const first = get(path.join(root, 'clip.mp4'), 'bytes=0-99')
  const etag = first.headers.etag
  assert.ok(etag && !etag.startsWith('W/'), 'etag 必须是强验证器（If-Range 只认强的）')
  await bodyOf(first)
  const second = await call(path.join(root, 'clip.mp4'), { ifNoneMatch: etag })
  assert.equal(second.status, 304)
  assert.equal(second.body.length, 0)
  assert.equal(second.headers.etag, etag)
  // 不匹配 → 照常 200（不能一律 304）
  const third = await call(path.join(root, 'clip.mp4'), { ifNoneMatch: '"nope"' })
  assert.equal(third.status, 200)
})

test('If-Range 不命中（文件换过了）→ 忽略 Range 回 200 整文件', async () => {
  const stale = await call(path.join(root, 'clip.mp4'), { range: 'bytes=0-1023', ifRange: '"stale-etag"' })
  assert.equal(stale.status, 200, 'If-Range 不命中时必须回整文件，否则客户端会把新字节拼进旧缓冲')
  assert.equal(stale.headers['content-range'], undefined)
  assert.equal(stale.body.length, 5000)
  // 命中 → 正常 206
  const first = get(path.join(root, 'clip.mp4'))
  const etag = first.headers.etag
  await bodyOf(first)
  const fresh = await call(path.join(root, 'clip.mp4'), { range: 'bytes=0-1023', ifRange: etag })
  assert.equal(fresh.status, 206)
  assert.equal(fresh.body.length, 1024)
})

test('只读：除 GET/HEAD 一律 405 + Allow（协议不能变成写通道）', async () => {
  for (const m of ['POST', 'PUT', 'DELETE', 'PATCH', 'OPTIONS']) {
    const r = get(path.join(root, 'clip.mp4'), undefined, m)
    assert.equal(r.status, 405, `${m} 必须 405`)
    assert.equal(r.headers.allow, 'GET, HEAD')
  }
})

test('计数：主进程统计能区分「首字节」与「中段」，且只按真的送出的字节记', async () => {
  resetMediaProtocolState()
  registerWorkspace(root)
  await bodyOf(get(path.join(root, 'clip.mp4'), 'bytes=0-1023'))
  await bodyOf(get(path.join(root, 'clip.mp4'), 'bytes=2048-3071'))
  const s = mediaStats()
  assert.equal(s.requests, 2)
  assert.equal(s.rangeRequests, 2)
  assert.equal(s.firstByteHits, 1, '只有 start=0 的那次算首字节')
  assert.equal(s.bytesServed, 2048, '按流里真的过去的字节记（不是按计划长度）')

  // 客户端中途放弃（seek/关标签时 Chromium 就是这么干的）：请求建出来了、一个字节都没读，
  // 计数就不该动。按「计划」记数的话这里会多出 1024 字节 + 一次首字节 —— 而 e2e 正是拿
  // 这两个数当「切走切回不重下」的判据（评审裁决 3 的第二条）。
  const abandoned = get(path.join(root, 'clip.mp4'), 'bytes=0-1023')
  ;(abandoned.body as unknown as { destroy: () => void }).destroy()
  await new Promise((r) => setTimeout(r, 20))
  const s2 = mediaStats()
  assert.equal(s2.bytesServed, 2048, `没读过的字节不能记（bytesServed=${s2.bytesServed}）`)
  assert.equal(s2.firstByteHits, 1, '没读过的首字节不能记')
})

// ——————————————————————— ③ 路径校验：8 种逃逸（状态 + 内容） ———————————————————————

test('逃逸 1/8：字面量 ..（解析后落在工作区外）→ 403 且没读到内容', async () => {
  const r = await rawCall(`/${root.slice(1)}/../${path.basename(outside)}/secret.mp4`)
  assert.equal(r.status, 403)
  assertClean(r.body, '.. 逃逸')
})

test('逃逸 2/8：编码后的 ..（%2e%2e）→ 403，而不是被 URL 解析器悄悄规范化后放行', async () => {
  const a = await rawCall(`/${root.slice(1)}/%2e%2e/${path.basename(outside)}/secret.mp4`)
  assert.equal(a.status, 403)
  assertClean(a.body, '%2e%2e 逃逸')
  // 编码的斜杠 + 编码的点：解一次之后才是真分隔符，必须仍然挡住
  const b = await rawCall(`/${root.slice(1)}/sub%2f..%2f..%2f${path.basename(outside)}%2fsecret.mp4`)
  assert.equal(b.status, 403)
  assertClean(b.body, '%2f.. 逃逸')
})

test('逃逸 3/8：大写编码（%2E%2E）与反斜杠（Windows 分隔符）→ 403', async () => {
  const a = await rawCall(`/${root.slice(1)}/%2E%2E/${path.basename(outside)}/secret.mp4`)
  assert.equal(a.status, 403)
  assertClean(a.body, '%2E%2E 逃逸')
  const b = await rawCall(`/${root.slice(1)}/..%5C${path.basename(outside)}%5Csecret.mp4`)
  assert.equal(b.status, 403)
  assertClean(b.body, '反斜杠逃逸')
})

test('逃逸 4/8：绝对路径直接指向工作区外 → 403（不是 404：先按包含拒绝）', async () => {
  const r = await call(path.join(outside, 'secret.mp4'))
  assert.equal(r.status, 403)
  assertClean(r.body, '绝对路径逃逸')
})

test('逃逸 5/8：软链指向工作区外 → 403（realpath 解链之后才比包含）', async () => {
  const r = await call(path.join(root, 'link.mp4'))
  assert.equal(r.status, 403)
  assertClean(r.body, '软链逃逸')
  // 反向对照：指向工作区内的软链必须能看 —— 否则是「把所有软链当攻击」的过度防御
  const ok = await call(path.join(root, 'inside-link.mp4'))
  assert.equal(ok.status, 200)
  assert.equal(ok.body.length, 5000)
})

test('逃逸 5b：软链名字是 .mp4、目标是文本 → 403（realpath 之后重判扩展名）', async () => {
  const r = await call(path.join(root, 'hidden.mp4'))
  assert.equal(r.status, 403, '白名单只管文件名不管内容就等于没有白名单')
  assertClean(r.body, '软链到非媒体')
  // 拿 URL 的那条路也要拒（否则面板拿到一个必然 403 的 URL）
  const u = mediaUrlForFile(path.join(root, 'hidden.mp4'), root, isKnownWorkspace)
  assert.equal(u.ok, false)
})

test('逃逸 6/8：NUL（%00 以及字面量）→ 403 且没读到内容', async () => {
  const a = await rawCall(`/${root.slice(1)}/clip.mp4%00.png`)
  assert.equal(a.status, 403)
  assertClean(a.body, 'NUL(%00)')
  const b = await rawCall(`/${root.slice(1)}/clip.mp4\0`)
  assert.equal(b.status, 403)
  assertClean(b.body, 'NUL(字面量)')
})

test('逃逸 7/8：大小写 —— 扩展名大小写不敏感（.MP3 能看），路径大小写不归一（fail-closed）', async () => {
  const mp3 = await call(path.join(root, 'song.MP3'))
  assert.equal(mp3.status, 200, '扩展名必须大小写不敏感，否则大写的相机文件打不开')
  // 路径大小写：realpath 在 macOS 的**大小写不敏感**卷上并不归一大小写（实测：同一目录
  // 大写形态 realpath 回来仍是大写），所以换个大小写会拿不到包含判断 → 403；在
  // 大小写敏感卷上文件根本不存在 → 404。两种都不是 200 —— 协议 URL 只由 mediaUrlForFile
  // 用 realpath 生成，正常播放不经过这条分支，所以 fail-closed 不伤正常路径。
  const r = await rawCall('/' + root.slice(1).toUpperCase() + '/clip.mp4')
  assert.ok(r.status === 403 || r.status === 404, `大小写变体不该被放行，got ${r.status}`)
  assertClean(r.body, '大小写变体')
})

test('逃逸 8/8：尾斜杠 → 403（movie.mp4/ 会被误以为是那个文件）', async () => {
  const r = await rawCall(`/${root.slice(1)}/clip.mp4/`)
  assert.equal(r.status, 403)
  assertClean(r.body, '尾斜杠')
})

test('二次编码（%252e%252e）不会被解成 .. —— 只解一次，落到 404', async () => {
  const r = await rawCall(`/${root.slice(1)}/%252e%252e/secret.mp4`)
  assert.equal(r.status, 404, '解出的是字面量 %2e%2e（不存在的文件名），不是上跳')
  assertClean(r.body, '二次编码')
})

test('非媒体扩展名一律 403 —— 协议不是任意文件读取通道（存在也不给）', async () => {
  const r = await call(path.join(root, 'notes.txt'))
  assert.equal(r.status, 403)
  assertClean(r.body, '非媒体扩展名')
})

test('敏感目录即使在工作区内/被当工作区打开也不放行', async () => {
  const reg = registerWorkspace(fakeHome)
  assert.equal(reg.ok, true, '假 HOME 本身不是敏感目录，应当能登记（否则这条测的是别的东西）')
  const r = await call(path.join(fakeHome, '.ssh', 'key.mp4'))
  assert.equal(r.status, 403)
  assertClean(r.body, '敏感目录')
  resetMediaProtocolState()
  registerWorkspace(root)
})

test('合法文件名不被过度拒绝：空格 / 中文 / # / % 都要能过（URL 编码往返）', async () => {
  for (const name of ['带 空格#井号.mp4']) {
    const p = path.join(root, name)
    const r = await call(p)
    assert.equal(r.status, 200, `${name} 应可读`)
    assert.equal(r.body.length, 64)
    // 直接交给 resolveMediaPath 的也是编码后的 pathname（模拟 Chromium 发来的样子）
    assert.equal(resolveMediaPath(encodeURI(p)).ok, true)
  }
})

test('未登记工作区时任何路径都 403（fail-closed）', async () => {
  resetMediaProtocolState()
  const r = await call(path.join(root, 'clip.mp4'))
  assert.equal(r.status, 403)
  assertClean(r.body, '未登记工作区')
  registerWorkspace(root)
  assert.equal((await call(path.join(root, 'clip.mp4'))).status, 200)
})

test('未登记的工作区拿不到 URL（渲染进程传 workspace=/Users 也换不到根）', async () => {
  resetMediaProtocolState()
  // 渲染进程能编的就是这两个入参；信任谓词说「没在清单里」→ 连根都登记不上
  const denied = mediaUrlForFile(path.join(outside, 'secret.mp4'), outside, isKnownWorkspace)
  assert.equal(denied.ok, false)
  assert.match(denied.error ?? '', /未登记/)
  // 根没登记 → 直接构造 URL 也 403（协议侧不认「刚才谁尝试过」）
  const r = await call(path.join(outside, 'secret.mp4'))
  assert.equal(r.status, 403)
  assertClean(r.body, '未登记工作区的 URL')
  // 反向：清单里有 → 正常给 URL 并登记根
  const allowed = mediaUrlForFile(path.join(root, 'clip.mp4'), root, isKnownWorkspace)
  assert.equal(allowed.ok, true)
  assert.match(allowed.url ?? '', /^hai-media:\/\/local\//)
  resetMediaProtocolState()
  registerWorkspace(root)
})

test('登记第 9/10 个根之后，最早那个根仍然能 200（没有 FIFO 淘汰）', async () => {
  // 复评裁决 1：这里曾经有 MAX_ROOTS=8 + slice(-8)。开 9 个工作区的用户一登记新根，
  // 最先那个根立刻 403 —— 正在播的视频当场断流。这条用例就是那次回归的钉子。
  const base = fs.mkdtempSync(path.join(os.tmpdir(), 'media-many-roots-'))
  const dirs: string[] = []
  const bodies = new Map<string, Buffer>()
  for (let i = 0; i < 10; i++) {
    const d = path.join(base, `ws-${i}`)
    fs.mkdirSync(d)
    const b = Buffer.from(`ROOT-${i}-MEDIA`)
    fs.writeFileSync(path.join(d, 'clip.mp4'), b)
    dirs.push(d)
    bodies.set(d, b)
    const reg = registerWorkspace(d)
    assert.equal(reg.ok, true, `第 ${i + 1} 个根应当登记成功: ${reg.error}`)
    assert.equal(isKnownWorkspace(d), true)
  }
  // 最早的那个根（ws-0）必须仍然可用，且字节正确
  for (const d of [dirs[0], dirs[1], dirs[9]]) {
    const r = await call(path.join(d, 'clip.mp4'))
    assert.equal(r.status, 200, `${d} 应当仍然能读（实得 ${r.status}）`)
    assert.deepEqual(r.body, bodies.get(d))
  }
  fs.rmSync(base, { recursive: true, force: true })
})

test('scheme/host 不合法 → 400/403（不允许别的 host 借道）', () => {
  assert.equal(handleMediaRequest({ method: 'GET', url: 'file:///etc/passwd.mp4' }).status, 400)
  assert.equal(handleMediaRequest({ method: 'GET', url: 'hai-media://evil/etc/x.mp4' }).status, 403)
  assert.equal(handleMediaRequest({ method: 'GET', url: 'not a url' }).status, 400)
})

// ——————————————————————— ④ protocol.handle 那一层（真入口） ———————————————————————

test('registerMediaProtocol：Range / If-None-Match 头经 protocol.handle 透传到响应', async () => {
  type Captured = { scheme: string; handler: (r: { method: string; url: string; headers: { get: (n: string) => string | null } }) => Response | Promise<Response> }
  let captured: Captured | undefined
  registerMediaProtocol({
    handle(scheme, handler) {
      captured = { scheme, handler }
    },
  })
  assert.ok(captured, 'protocol.handle 必须被调用（否则协议根本没接上）')
  assert.equal((captured as Captured).scheme, 'hai-media')

  const target = mediaUrlFor(path.join(root, 'clip.mp4'))
  // 真的 Request 对象：headers.get 的大小写不敏感行为与被测代码一致
  const r206 = await (captured as Captured).handler(new Request(target, { headers: { range: 'bytes=0-1023' } }))
  assert.equal(r206.status, 206)
  assert.equal(r206.headers.get('content-range'), 'bytes 0-1023/5000')
  assert.equal(r206.headers.get('accept-ranges'), 'bytes')
  const body = Buffer.from(await r206.arrayBuffer())
  assert.equal(body.length, 1024)
  assert.deepEqual(body, bytes(5000).subarray(0, 1024))

  // 304 也必须能从这一层透出去（If-None-Match → 条件请求）
  const etag = r206.headers.get('etag') as string
  const r304 = await (captured as Captured).handler(new Request(target, { headers: { range: 'bytes=0-1023', 'if-none-match': etag } }))
  assert.equal(r304.status, 304)

  // 工作区外的真文件在这一层也是 403（不是 200 空体）
  const r403 = await (captured as Captured).handler(new Request(mediaUrlFor(path.join(outside, 'secret.mp4'))))
  assert.equal(r403.status, 403)
  assert.ok(!Buffer.from(await r403.arrayBuffer()).toString('latin1').includes('OUTSIDE-SECRET'))
})

// ——————————————————————— ⑤ 与前端/Go 清单不漂移 ———————————————————————

test('协议白名单 ⇄ 前端媒体扩展名清单：两边必须是同一批（否则「能渲染但 403」或「能播但没渲染器」）', () => {
  const protoVideo = Object.keys(MEDIA_MIMES).filter((e) => MEDIA_MIMES[e].startsWith('video/')).sort()
  const protoAll = Object.keys(MEDIA_MIMES).sort()
  assert.deepEqual([...MEDIA_VIDEO_EXTS].sort(), protoVideo, '视频扩展名两边不一致')
  for (const ext of protoAll) assert.equal(isMediaExt(`x.${ext}`), true)
  assert.equal(mimeOfMediaPath('a.MP4'), 'video/mp4')
})

test('协议白名单 ⇄ Go 的编解码矩阵：扩展名集合必须逐字相同（跨进程的单一事实源不能漂）', () => {
  // 从 Go 的规则表文本里抠出 Exts: []string{...} 的每个字面量。表一旦被改写成本文件
  // 抓不到的形态，这条会红 —— 那正是要求「矩阵必须是数据」的原因：数据才能被核对。
  const goExts = new Set<string>()
  for (const m of String(matrixGoSource).matchAll(/Exts:\s*\[\]string\{([^}]*)\}/g)) {
    for (const q of m[1].matchAll(/"([^"]+)"/g)) goExts.add(q[1])
  }
  assert.ok(goExts.size >= 15, `没从 media_matrix.go 抠出扩展名（抓取规则与表形态不一致）：${[...goExts]}`)
  assert.deepEqual([...goExts].sort(), Object.keys(MEDIA_MIMES).sort(), 'Go 矩阵与协议白名单的扩展名集合不一致')
})
