// media-protocol.test.ts — `hai-media://` 协议单测：node:test，纯 Node **不启动 Electron**
// （协议的判断链是纯函数，见 media-protocol.ts 顶部注释）。
//
// 为什么这一份必须存在（00 §4.7）：这条通路是「渲染进程拿 URL → 主进程把本地文件送过去」，
// 也就是本 PR 唯一一个能把任意路径内容变成可读字节的入口。它的拒绝分支在 UI 里几乎不可
// 复现（要造软链逃逸、编码后的 ..、Windows 反斜杠……），而一旦写宽，任何 `<video src>` 都能
// 变成任意文件读取。所以每条逃逸手段都在这里钉死到状态码。
//
// 覆盖三块：
//   ① Range 解析（0-1023 / 1024- / -512 / 越界 / 多段 / 语法错 / 空文件）；
//   ② 生产入口 handleMediaRequest 的响应（200 / 206 / 416 / 405 / HEAD 无体 + 头齐全）；
//   ③ 路径校验的 8 种逃逸（.. / %2e%2e / %2E%2E / %2f / 绝对路径 / 软链 / NUL / 尾斜杠），
//      外加大小写与「合法文件名里的空格/中文/#」不过度拒绝。
//
// 运行：esbuild bundle 后 node --test（见 package.json test:media-protocol）。
import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import {
  MEDIA_MIMES,
  allowMediaRoot,
  handleMediaRequest,
  isMediaExt,
  mediaStats,
  mediaUrlFor,
  mediaUrlForFile,
  mimeOfMediaPath,
  planRange,
  resetMediaProtocolState,
  resolveMediaPath,
  type MediaResponse,
} from './media-protocol.ts'
import { MEDIA_VIDEO_EXTS } from '../src/lib/mediaFile.ts'
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

before(() => {
  process.env.HOME = fakeHome
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
  // 假 HOME 下的敏感目录（当成工作区打开也不该成为媒体根）
  fs.mkdirSync(path.join(fakeHome, '.ssh'), { recursive: true })
  fs.writeFileSync(path.join(fakeHome, '.ssh', 'key.mp4'), 'PRIVATE-KEY')
  allowMediaRoot(root)
})

after(() => {
  process.env.HOME = oldHome
  for (const p of [root, outside, fakeHome]) fs.rmSync(p, { recursive: true, force: true })
})

const url = (p: string) => mediaUrlFor(p)
/** 收完整个响应体（200/206 是文件流，HEAD/416 无体）。 */
async function bodyOf(r: MediaResponse): Promise<Buffer> {
  if (!r.body) return Buffer.alloc(0)
  if (r.body instanceof Uint8Array) return Buffer.from(r.body)
  const chunks: Buffer[] = []
  for await (const c of r.body) chunks.push(Buffer.from(c as Buffer))
  return Buffer.concat(chunks)
}
const get = (p: string, range?: string, method = 'GET') =>
  handleMediaRequest({ method, url: p.startsWith('hai-media://') ? p : url(p), range })

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

test('只读：除 GET/HEAD 一律 405 + Allow（协议不能变成写通道）', async () => {
  for (const m of ['POST', 'PUT', 'DELETE', 'PATCH', 'OPTIONS']) {
    const r = get(path.join(root, 'clip.mp4'), undefined, m)
    assert.equal(r.status, 405, `${m} 必须 405`)
    assert.equal(r.headers.allow, 'GET, HEAD')
  }
})

test('计数：主进程统计能区分「首字节」与「中段」（e2e 的不重下判据）', async () => {
  resetMediaProtocolState()
  allowMediaRoot(root)
  await bodyOf(get(path.join(root, 'clip.mp4'), 'bytes=0-1023'))
  await bodyOf(get(path.join(root, 'clip.mp4'), 'bytes=2048-3071'))
  const s = mediaStats()
  assert.equal(s.requests, 2)
  assert.equal(s.rangeRequests, 2)
  assert.equal(s.firstByteHits, 1, '只有 start=0 的那次算首字节')
  assert.equal(s.bytesServed, 2048)
})

// ——————————————————————— ③ 路径校验：8 种逃逸 ———————————————————————

// 只看状态码时必须把响应体销毁：200/206 的体是**文件流**，不消费/不关掉会一直挂着
// 文件句柄（临时目录被删之后表现为一个迟到的 ENOENT，把整个测试进程带崩）。
const statusOf = (p: string) => {
  const r = get(p.startsWith('hai-media://') ? p : url(p))
  if (r.body && !(r.body instanceof Uint8Array)) (r.body as { destroy: () => void }).destroy()
  return r.status
}
const rawStatus = (pathname: string, range?: string) => {
  const r = handleMediaRequest({ method: 'GET', url: `hai-media://local${pathname}`, range })
  if (r.body && !(r.body instanceof Uint8Array)) (r.body as { destroy: () => void }).destroy()
  return r.status
}

test('逃逸 1/8：字面量 ..（解析后落在工作区外）→ 403', () => {
  assert.equal(rawStatus(`/${root.slice(1)}/../${path.basename(outside)}/secret.mp4`), 403)
})

test('逃逸 2/8：编码后的 ..（%2e%2e）→ 403，而不是被 URL 解析器悄悄规范化后放行', () => {
  assert.equal(rawStatus(`/${root.slice(1)}/%2e%2e/${path.basename(outside)}/secret.mp4`), 403)
  // 编码的斜杠 + 编码的点：解一次之后才是真分隔符，必须仍然挡住
  assert.equal(rawStatus(`/${root.slice(1)}/sub%2f..%2f..%2f${path.basename(outside)}%2fsecret.mp4`), 403)
})

test('逃逸 3/8：大写编码（%2E%2E）与反斜杠（Windows 分隔符）→ 403', () => {
  assert.equal(rawStatus(`/${root.slice(1)}/%2E%2E/${path.basename(outside)}/secret.mp4`), 403)
  assert.equal(rawStatus(`/${root.slice(1)}/..%5C${path.basename(outside)}%5Csecret.mp4`), 403)
})

test('逃逸 4/8：绝对路径直接指向工作区外 → 403（不是 404：先按白名单+包含拒绝）', () => {
  assert.equal(statusOf(path.join(outside, 'secret.mp4')), 403)
})

test('逃逸 5/8：软链指向工作区外 → 403（realpath 解链之后才比包含）', () => {
  assert.equal(statusOf(path.join(root, 'link.mp4')), 403)
  // 反向对照：指向工作区内的软链必须能看 —— 否则是「把所有软链当攻击」的过度防御
  assert.equal(statusOf(path.join(root, 'inside-link.mp4')), 200)
})

test('逃逸 6/8：NUL（%00 以及字面量）→ 403', () => {
  assert.equal(rawStatus(`/${root.slice(1)}/clip.mp4%00.png`), 403)
  assert.equal(rawStatus(`/${root.slice(1)}/clip.mp4\0`), 403)
})

test('逃逸 7/8：大小写 —— 扩展名大小写不敏感（.MP3 能看），路径大小写不归一（fail-closed）', () => {
  assert.equal(statusOf(path.join(root, 'song.MP3')), 200, '扩展名必须大小写不敏感，否则大写的相机文件打不开')
  // 路径大小写：realpath 在 macOS 的**大小写不敏感**卷上并不归一大小写（实测：同一目录
  // 大写形态 realpath 回来仍是大写），所以换个大小写会拿不到包含判断 → 403；在
  // 大小写敏感卷上文件根本不存在 → 404。两种都不是 200 —— 协议 URL 只由 mediaUrlForFile
  // 用 realpath 生成，正常播放不经过这条分支，所以 fail-closed 不伤正常路径。
  const st = rawStatus('/' + root.slice(1).toUpperCase() + '/clip.mp4')
  assert.ok(st === 403 || st === 404, `大小写变体不该被放行，got ${st}`)
})

test('逃逸 8/8：尾斜杠 → 403（movie.mp4/ 会被误以为是那个文件）', () => {
  assert.equal(rawStatus(`/${root.slice(1)}/clip.mp4/`), 403)
})

test('二次编码（%252e%252e）不会被解成 .. —— 只解一次，落到 404', () => {
  const st = rawStatus(`/${root.slice(1)}/%252e%252e/secret.mp4`)
  assert.equal(st, 404, '解出的是字面量 %2e%2e（不存在的文件名），不是上跳')
})

test('非媒体扩展名一律 403 —— 协议不是任意文件读取通道（存在也不给）', async () => {
  const r = get(path.join(root, 'notes.txt'))
  assert.equal(r.status, 403)
  const body = await bodyOf(r)
  assert.ok(!body.toString().includes('TOP-SECRET-TEXT'), '拒绝响应里不能夹带文件内容')
})

test('敏感目录即使在工作区内/被当工作区打开也不放行', () => {
  assert.equal(allowMediaRoot(path.join(fakeHome, '.ssh')).ok, false)
  // 把 HOME 当工作区打开（.ssh 就在里面）：协议仍要挡住 .ssh 下的媒体文件
  allowMediaRoot(fakeHome)
  assert.equal(statusOf(path.join(fakeHome, '.ssh', 'key.mp4')), 403)
  resetMediaProtocolState()
  allowMediaRoot(root)
})

test('合法文件名不被过度拒绝：空格 / 中文 / # / % 都要能过（URL 编码往返）', async () => {
  for (const name of ['带 空格#井号.mp4']) {
    const p = path.join(root, name)
    const r = get(p)
    assert.equal(r.status, 200, `${name} 应可读`)
    assert.equal((await bodyOf(r)).length, 64)
    // 直接交给 resolveMediaPath 的也是编码后的 pathname（模拟 Chromium 发来的样子）
    assert.equal(resolveMediaPath(encodeURI(p)).ok, true)
  }
})

test('未登记工作区时任何路径都 403（fail-closed）', () => {
  resetMediaProtocolState()
  assert.equal(statusOf(path.join(root, 'clip.mp4')), 403)
  allowMediaRoot(root)
  assert.equal(statusOf(path.join(root, 'clip.mp4')), 200)
})

test('scheme/host 不合法 → 400/403（不允许别的 host 借道）', () => {
  assert.equal(handleMediaRequest({ method: 'GET', url: 'file:///etc/passwd.mp4' }).status, 400)
  assert.equal(handleMediaRequest({ method: 'GET', url: 'hai-media://evil/etc/x.mp4' }).status, 403)
  assert.equal(handleMediaRequest({ method: 'GET', url: 'not a url' }).status, 400)
})

// ——————————————————————— ④ 与前端渲染器清单不漂移 ———————————————————————

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
