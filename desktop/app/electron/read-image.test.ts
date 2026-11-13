// read-image.test.ts — Markdown 本地图片通道（read-image.ts）单测：node:test，纯 Node
// 无 Electron 依赖，毫秒级。
//
// 为什么值得单测：这条通路是「渲染进程交一个路径 → 主进程读盘」的最小面 —— 也正是最容易
// 被写宽的一处。相对路径的 .. 逃逸、敏感目录、非图片文件、超大文件都必须挡在 readFileSync
// **之前**，而这些分支在真实 UI 里极难逐条复现（要造 .ssh 下的图、造 8MiB 的 png）。
// 于是用临时目录把每条拒绝理由钉死；同时把「**绝对路径在工作区外必须放行**」也钉住 ——
// 那是产品行为（工作区外文档旁边的图要跟着显示），不是漏洞，很容易被后来者当成风险堵掉。
//
// 运行：esbuild bundle 后 node --test（见 package.json test:read-image）。
import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import { MAX_IMAGE_BYTES, imageMimeFor, readImageAsDataUrl } from './read-image.ts'

// 工作区用临时目录；「工作区外」用**另一个**临时目录（不是工作区里的兄弟路径）—— 后者即使
// 被误挡也可能刚好通过，只有真的另一个目录才证明绝对路径不受 workspace 约束。
const root = fs.mkdtempSync(path.join(os.tmpdir(), 'read-image-ws-'))
const outside = fs.mkdtempSync(path.join(os.tmpdir(), 'read-image-out-'))
after(() => {
  fs.rmSync(root, { recursive: true, force: true })
  fs.rmSync(outside, { recursive: true, force: true })
})

function write(p: string, data: string | Buffer): string {
  fs.mkdirSync(path.dirname(p), { recursive: true })
  fs.writeFileSync(p, data)
  return p
}

// 内容随便写：read-image.ts 按扩展名判 mime，不嗅探文件头（见该文件顶部注释）。
const SVG = '<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>'
const PNG = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0x03])
const okSvg = write(path.join(root, 'ok.svg'), SVG)
const okPng = write(path.join(root, 'ok.png'), PNG)
const bigPng = write(path.join(root, 'big.png'), Buffer.alloc(64, 7)) // 超限用例（≥ 32 字节）
const txt = write(path.join(root, 'a.txt'), 'not an image')
const go = write(path.join(root, 'a.go'), 'package main')
const outsidePng = write(path.join(outside, 'out.png'), PNG)
fs.mkdirSync(path.join(root, 'dir'))
fs.mkdirSync(path.join(root, 'dir.png')) // 目录 + 图片扩展名：目录判定必须先于 mime 判定

type ReadResult =
  | { ok: true; dataUrl: string; mime: string; bytes: number }
  | { ok: false; error: string }

// 拒绝断言只比关键字不比全文：文案会微调，而「为什么拒绝」这条口径不能变。
function assertRejected(r: ReadResult, keyword: string, why: string): void {
  assert.equal(r.ok, false, `${why}：本应拒绝，却放行了`)
  assert.ok(!r.ok && r.error.includes(keyword), `${why}：error「${!r.ok ? r.error : ''}」不含「${keyword}」`)
}

test('① 空 path → 拒绝（渲染进程漏传路径时，不该拿工作区去 stat）', () => {
  assertRejected(readImageAsDataUrl({ path: '', workspace: root }), 'path 为空', '空字符串路径')
  assertRejected(readImageAsDataUrl({ path: '' }), 'path 为空', '空字符串路径 + 无工作区')
})

test('② 相对路径缺 workspace → 拒绝（没有锚点就无从判断指向哪里）', () => {
  assertRejected(readImageAsDataUrl({ path: 'ok.png' }), 'workspace 为空', '未传 workspace')
  assertRejected(readImageAsDataUrl({ path: 'ok.png', workspace: '' }), 'workspace 为空', 'workspace 为空串')
  assertRejected(readImageAsDataUrl({ path: 'ok.png', workspace: 'ws' }), 'workspace 为空', 'workspace 不是绝对路径')
})

test('③ 相对路径 .. 逃逸 → 拒绝；同一分支内的工作区相对路径必须放行', () => {
  assertRejected(readImageAsDataUrl({ path: '../x.png', workspace: root }), '工作区外', '相对路径 .. 逃逸')
  assertRejected(readImageAsDataUrl({ path: path.join('sub', '..', '..', 'x.png'), workspace: root }), '工作区外', '先拼再归一化后的逃逸')
  // 放行侧配对断言：若把整个「相对路径」分支一刀切成拒绝，上面两条仍会通过 —— 这条不会。
  const inside = readImageAsDataUrl({ path: 'ok.png', workspace: root })
  assert.equal(inside.ok, true, inside.ok ? '' : inside.error)
})

test('④ 绝对路径在工作区外 → **放行**（产品行为：工作区外文档旁边的图要能显示）', () => {
  const r = readImageAsDataUrl({ path: outsidePng, workspace: root })
  assert.equal(r.ok, true, r.ok ? '' : r.error)
  assert.ok(r.ok && r.dataUrl.startsWith('data:image/png;base64,'))
  // 不传 workspace 也必须能读（工作区外的预览根本不知道工作区是谁）
  const noWs = readImageAsDataUrl({ path: outsidePng })
  assert.equal(noWs.ok, true, noWs.ok ? '' : noWs.error)
})

test('⑤ 敏感目录 → 拒绝（且先于存在性判定：受保护路径连「在不在」都不回答）', () => {
  // ~/.ssh/x.png 在测试机上通常并不存在 —— 这里要的就是「受保护」而不是「不存在」。
  assertRejected(readImageAsDataUrl({ path: path.join(os.homedir(), '.ssh', 'x.png') }), '受保护', '~/.ssh 下的图')
  assertRejected(readImageAsDataUrl({ path: path.join(os.homedir(), '.aws', 'creds.png') }), '受保护', '~/.aws 下的图')
  assertRejected(readImageAsDataUrl({ path: path.join(os.homedir(), '.ssh', 'x.png'), workspace: root }), '受保护', '~/.ssh 下的图 + 工作区参数')
})

test('⑥ 非图片扩展名（.txt/.go）→ 拒绝', () => {
  assertRejected(readImageAsDataUrl({ path: txt, workspace: root }), '不是图片文件', '.txt 文件')
  assertRejected(readImageAsDataUrl({ path: go, workspace: root }), '不是图片文件', '.go 文件')
})

test('⑦ 目录 → 拒绝（目录名带图片扩展名时也必须走目录分支）', () => {
  assertRejected(readImageAsDataUrl({ path: path.join(root, 'dir'), workspace: root }), '目录', '普通目录')
  assertRejected(readImageAsDataUrl({ path: path.join(root, 'dir.png'), workspace: root }), '目录', '名为 dir.png 的目录')
})

test('⑧ 不存在的文件 → 拒绝（相对路径与绝对路径两条入参都算）', () => {
  assertRejected(readImageAsDataUrl({ path: path.join(root, 'nope.png'), workspace: root }), '不存在', '绝对路径不存在')
  assertRejected(readImageAsDataUrl({ path: 'nope.png', workspace: root }), '不存在', '工作区内相对路径不存在')
})

test('⑨ 正常文件（svg / png）→ data URL 前缀、mime、字节数都对', () => {
  const svg = readImageAsDataUrl({ path: okSvg, workspace: root })
  assert.equal(svg.ok, true, svg.ok ? '' : svg.error)
  assert.ok(svg.ok && svg.dataUrl.startsWith('data:image/svg+xml;base64,'), 'svg 前缀不对')
  assert.equal(svg.ok && svg.dataUrl, 'data:image/svg+xml;base64,' + Buffer.from(SVG).toString('base64'), '内容必须整段带上（预览靠它渲染）')
  assert.equal(svg.ok && svg.mime, 'image/svg+xml')
  assert.equal(svg.ok && svg.bytes, Buffer.byteLength(SVG))

  const png = readImageAsDataUrl({ path: okPng, workspace: root })
  assert.equal(png.ok, true, png.ok ? '' : png.error)
  assert.ok(png.ok && png.dataUrl.startsWith('data:image/png;base64,'), 'png 前缀不对')
  assert.equal(png.ok && png.dataUrl, 'data:image/png;base64,' + PNG.toString('base64'))
  assert.equal(png.ok && png.mime, 'image/png')
  assert.equal(png.ok && png.bytes, PNG.length)
})

test('⑩ 超过 maxBytes → 拒绝（默认上限 8MiB，调用方可以收紧）', () => {
  assert.equal(MAX_IMAGE_BYTES, 8 * 1024 * 1024, '默认上限是「一次 IPC 能舒服带多大的图」的产品口径，改动要显式')
  assert.equal(readImageAsDataUrl({ path: bigPng, workspace: root }).ok, true, '默认上限下 64 字节当然能读')
  assertRejected(readImageAsDataUrl({ path: bigPng, workspace: root }, { maxBytes: 16 }), '过大', 'maxBytes 16 + 64 字节文件')
  // 边界取「刚好等于上限」= 放行（> 才拒），避免把等于上限的文件误挡。
  assert.equal(readImageAsDataUrl({ path: bigPng, workspace: root }, { maxBytes: 64 }).ok, true, '恰好等于上限应放行')
  assertRejected(readImageAsDataUrl({ path: bigPng, workspace: root }, { maxBytes: 63 }), '过大', 'maxBytes 63 + 64 字节文件')
})

test('⑪ imageMimeFor：大小写不敏感；未知/无扩展名 → null', () => {
  assert.equal(imageMimeFor('/a/b.PNG'), 'image/png')
  assert.equal(imageMimeFor('/a/b.JpG'), 'image/jpeg')
  assert.equal(imageMimeFor('/a/b.SVG'), 'image/svg+xml')
  assert.equal(imageMimeFor('/a/b.png'), 'image/png')
  for (const p of ['/a/b.txt', '/a/b.go', '/a/b', '/a/b.gitignore', '', '/a/b.png.txt']) {
    assert.equal(imageMimeFor(p), null, `${p} 不该被当成图片`)
  }
})

test('敏感目录先于存在性：判定顺序一旦反了，「~/.ssh/x.png 存不存在」就泄露给渲染进程', () => {
  const r = readImageAsDataUrl({ path: path.join(os.homedir(), '.ssh', 'x.png') })
  assertRejected(r, '受保护', '不存在的敏感路径')
  assert.ok(!r.ok && !r.error.includes('不存在'), '不能回落成「不存在」—— 那等于回答了「这个路径在不在」')
})
