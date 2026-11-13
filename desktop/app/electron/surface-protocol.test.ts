// surface-protocol.test.ts —— `hai-surface://` 协议 + 索引读取单测（PR-14 A/B）。
//
// 纯 Node，**不启动 Electron**（与 media-protocol.test.ts 同一个理由：判定链是纯函数）。
//
// 为什么每条拒绝都要钉住：这条通道把 `<id>` 目录里的文件变成渲染进程可 `import()` 的
// 模块 —— 那正是插件代码执行的位置。写宽了有三种后果，每一种都不会在 UI 里报错：
//   ① `..` / 绝对路径 / 软链逃逸 → 任意文件读取（panel.js 能读到 ~/.ssh 里的东西）；
//   ② 扩展名不判 → `.json`/`.db` 也能取，白名单形同虚设；
//   ③ 不判 enabled → 「禁用插件」只是 UI 上灰了，字节照给（权限变成装饰）。
//
// 覆盖四块：
//   ① 纯函数 resolveSurfacePath：.. / 编码后的 %2e%2e / 绝对路径 / NUL / 反斜杠 /
//      尾斜杠 / 扩展名 / 非法 id / 未启用 / 不存在 / 软链逃逸 / 软链指向非白名单；
//   ② 生产入口 handleSurfaceRequest：200 的头（content-type / no-store / CORS）、
//      405、404、HEAD 无体；
//   ③ readSurfaceIndex：坏 manifest 带 error、非法 id 整条跳过、entry/icon/commands/data；
//   ④ registerSurfaceProtocol 那一层翻译（Request → Response）也真跑一次 —— 只测
//      handleSurfaceRequest 会绕过主进程里真正注册给 Chromium 的那个 handler。
//
// 运行：esbuild bundle 后 node --test（见 package.json test:surface-protocol）。
import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import {
  SURFACE_SCHEME,
  SURFACE_MIMES,
  extOfSurfacePath,
  handleSurfaceRequest,
  registerSurfaceProtocol,
  resolveSurfacePath,
  type SurfaceProtocolLike,
  type SurfaceRequestLike,
} from './surface-protocol.ts'
import { SURFACE_ID_RE, configureSurfaceRoot, readSurfaceIndex, surfaceRoot } from './surface-index.ts'

const root = fs.mkdtempSync(path.join(os.tmpdir(), 'surface-proto-'))
const outside = fs.mkdtempSync(path.join(os.tmpdir(), 'surface-outside-'))
const oldHome = process.env.HOME

// 两份「不该被送出去」的内容：每条逃逸都要断言响应体里没有它们。
const SECRETS = ['OUTSIDE-SECRET-PANEL', 'PRIVATE-KEY-MATERIAL']

function writeSurface(id: string, files: Record<string, string>, manifest: Record<string, unknown> | string = {}): void {
  const dir = path.join(root, id)
  fs.mkdirSync(dir, { recursive: true })
  const mf = typeof manifest === 'string' ? manifest : {
    id,
    name: { zh: id },
    version: '1.0.0',
    kind: `plugin:${id}`,
    entry: 'panel.js',
    commands: ['workspace_list'],
    data: ['surfaces_changed'],
    ...manifest,
  }
  fs.writeFileSync(path.join(dir, 'surface.json'), typeof mf === 'string' ? mf : JSON.stringify(mf))
  for (const [name, body] of Object.entries(files)) {
    const p = path.join(dir, name)
    fs.mkdirSync(path.dirname(p), { recursive: true })
    fs.writeFileSync(p, body)
  }
}

function writeIndex(ids: string[]): void {
  fs.writeFileSync(path.join(root, 'index.json'), JSON.stringify({
    version: 1,
    items: ids.map((id) => ({
      id, version: '1.0.0', hash: id.padEnd(64, 'a').slice(0, 64), source: '/tmp/src-' + id, enabled: true, installedAt: '2026-11-12T00:00:00Z',
    })),
  }))
}

before(() => {
  process.env.HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'surface-proto-home-'))
  configureSurfaceRoot(root)
  writeSurface('demo', {
    'panel.js': 'export const surface = { kind: "plugin:demo", apply() {} }\n',
    'icon.svg': '<svg xmlns="http://www.w3.org/2000/svg"></svg>\n',
    'styles.css': 'body{}\n',
    'data.json': '{"ok":true}\n',
    'pic.png': 'not-really-a-png',
    'notes.txt': 'TEXT-NOT-ALLOWED',
  })
  writeSurface('nested', { 'dist/panel.js': 'export const surface = { kind: "plugin:nested" }\n' }, { entry: 'dist/panel.js' })
  writeSurface('off', { 'panel.js': 'export const surface = {}\n' })
  writeSurface('broken', { 'panel.js': '' }, '{"id": "broken",')
  // 软链逃逸：panel-evil.js -> 外面的机密文件
  const secret = path.join(outside, 'secret.js')
  fs.writeFileSync(secret, 'OUTSIDE-SECRET-PANEL')
  fs.symlinkSync(secret, path.join(root, 'demo', 'evil.js'))
  // 软链指向非白名单类型（过 abs 的白名单，realpath 成 .txt）
  const txt = path.join(outside, 'secret.txt')
  fs.writeFileSync(txt, 'PRIVATE-KEY-MATERIAL')
  fs.symlinkSync(txt, path.join(root, 'demo', 'sneaky.js'))
  writeIndex(['demo', 'nested', 'off', 'broken', 'Bad_ID', '../escape'])
  // 手动把 off 改成 enabled:false（writeIndex 一律 true）
  const idx = JSON.parse(fs.readFileSync(path.join(root, 'index.json'), 'utf8')) as { items: Array<{ id: string; enabled: boolean }> }
  for (const it of idx.items) if (it.id === 'off') it.enabled = false
  fs.writeFileSync(path.join(root, 'index.json'), JSON.stringify(idx))
})

after(() => {
  if (oldHome === undefined) delete process.env.HOME
  else process.env.HOME = oldHome
  fs.rmSync(root, { recursive: true, force: true })
  fs.rmSync(outside, { recursive: true, force: true })
})

const index = () => readSurfaceIndex()

function assertRejected(id: string, pathname: string, status: number, where: string, reasonHas = ''): void {
  const r = resolveSurfacePath(id, pathname, index())
  assert.equal(r.ok, false, `${where}: 被接受了`)
  if (!r.ok) {
    assert.equal(r.status, status, `${where}: 状态码 ${r.status}，want ${status}（${r.reason}）`)
    // reason 而不是只看状态码：`..` 与「逃逸」都回 403，但**判定顺序**是设计的一部分
    // （先拒形状，再算路径）。只看状态码时，把 `..` 那一行删掉测试照样绿 —— 而那时
    // 「白名单在 realpath 之前」「不泄露存不存在」这些性质已经悄悄换了机制。
    if (reasonHas) assert.ok(r.reason.includes(reasonHas), `${where}: reason ${JSON.stringify(r.reason)} 里没有 ${JSON.stringify(reasonHas)}`)
  }
}

// ---- ① 纯函数：路径校验的每一种逃逸 ----

test('纯函数：正常资产解析成目录内的绝对路径', () => {
  const r = resolveSurfacePath('demo', '/panel.js', index())
  assert.equal(r.ok, true)
  if (r.ok) {
    assert.equal(r.real, fs.realpathSync(path.join(root, 'demo', 'panel.js')))
    assert.equal(r.mime, 'text/javascript')
  }
})

test('纯函数：嵌套 entry（dist/panel.js）也能解析', () => {
  const r = resolveSurfacePath('nested', '/dist/panel.js', index())
  assert.equal(r.ok, true)
})

test('纯函数：.. 一律 403（明写与编码后各一条）', () => {
  assertRejected('demo', '/../off/panel.js', 403, '明写 ..', '上跳')
  assertRejected('demo', '/%2e%2e/off/panel.js', 403, '编码后的 ..', '上跳')
  assertRejected('demo', '/a/../../b.js', 403, '中段 ..', '上跳')
})

test('纯函数：绝对路径 / NUL / 反斜杠 / 尾斜杠 一律拒', () => {
  assertRejected('demo', 'panel.js', 403, '不以 / 开头')
  assertRejected('demo', '/panel\0.js', 403, 'NUL', 'NUL')
  assertRejected('demo', '/a\\b.js', 403, '反斜杠', '反斜杠')
  assertRejected('demo', '/panel.js/', 403, '尾斜杠', '尾斜杠')
  assertRejected('demo', '', 400, '空路径')
  assertRejected('demo', '/a%ZZ', 400, '非法编码')
})

test('纯函数：扩展名白名单之外一律 403（连存不存在都不泄露）', () => {
  assertRejected('demo', '/notes.txt', 403, 'txt')
  assertRejected('demo', '/no-such-file.wasm', 403, '不在白名单的扩展名（文件也不存在）')
  for (const ext of Object.keys(SURFACE_MIMES)) {
    assert.ok(extOfSurfacePath(`/x.${ext}`) === ext)
  }
})

test('纯函数：id 必须匹配 ^[a-z][a-z0-9-]{1,40}$', () => {
  for (const bad of ['Demo', 'a', '../etc', 'demo_x', '-demo', '', 'd'.repeat(42)]) {
    assert.ok(!SURFACE_ID_RE.test(bad), `${bad} 不该匹配`)
    assertRejected(bad, '/panel.js', 403, `非法 id ${bad}`)
  }
  assert.ok(SURFACE_ID_RE.test('demo-2'))
})

test('纯函数：未启用 / index 里没有 → 404（禁用 = 能力消失）', () => {
  assertRejected('off', '/panel.js', 404, 'enabled=false')
  assertRejected('ghost', '/panel.js', 404, 'index 里没有')
})

test('纯函数：目录存在但文件不存在 → 404', () => {
  assertRejected('demo', '/missing.js', 404, '文件不存在')
})

test('纯函数：软链逃出 <id> 目录 → 拒绝（且不带出任何字节）', () => {
  const r = resolveSurfacePath('demo', '/evil.js', index())
  assert.equal(r.ok, false, '软链逃逸被接受了')
  if (!r.ok) assert.equal(r.status, 404)
})

test('纯函数：软链指向非白名单类型 → 403（realpath 后再判一次扩展名）', () => {
  assertRejected('demo', '/sneaky.js', 403, '软链到 .txt')
})

// ---- ② 生产入口：handleSurfaceRequest ----

const req = (method: string, pathname: string, id = 'demo'): SurfaceRequestLike => ({
  method,
  url: `${SURFACE_SCHEME}://${id}${pathname}`,
})

test('入口：GET panel.js → 200 + 正确 Content-Type + no-store + CORS', () => {
  const r = handleSurfaceRequest(req('GET', '/panel.js'), index())
  assert.equal(r.status, 200)
  assert.equal(r.headers['content-type'], 'text/javascript')
  assert.equal(r.headers['cache-control'], 'no-store')
  assert.equal(r.headers['access-control-allow-origin'], '*')
  assert.equal(r.headers['x-content-type-options'], 'nosniff')
  assert.ok(r.body && new TextDecoder().decode(r.body).includes('plugin:demo'))
})

test('入口：各扩展名的 Content-Type', () => {
  const want: Record<string, string> = {
    '/icon.svg': 'image/svg+xml',
    '/styles.css': 'text/css',
    '/data.json': 'application/json',
    '/pic.png': 'image/png',
  }
  for (const [p, mime] of Object.entries(want)) {
    const r = handleSurfaceRequest(req('GET', p), index())
    assert.equal(r.status, 200, `${p} 没通`)
    assert.equal(r.headers['content-type'], mime, `${p} 的 MIME 不对`)
  }
})

test('入口：HEAD 有头无体', () => {
  const r = handleSurfaceRequest(req('HEAD', '/panel.js'), index())
  assert.equal(r.status, 200)
  assert.equal(r.body, undefined)
})

test('入口：非 GET/HEAD → 405（这条通道不能变成写通道）', () => {
  for (const m of ['POST', 'PUT', 'DELETE', 'PATCH']) {
    assert.equal(handleSurfaceRequest(req(m, '/panel.js'), index()).status, 405, `${m} 没被拒`)
  }
})

test('入口：禁用插件 → 404，且响应体里没有任何插件内容', () => {
  const r = handleSurfaceRequest(req('GET', '/panel.js', 'off'), index())
  assert.equal(r.status, 404)
  if (r.body) {
    const text = new TextDecoder().decode(r.body)
    for (const s of SECRETS) assert.ok(!text.includes(s), `404 的响应体里出现了 ${s}`)
  }
})

test('入口：bad scheme / bad url / NUL', () => {
  assert.equal(handleSurfaceRequest({ method: 'GET', url: 'hai-media://local/x.mp4' }, index()).status, 400)
  assert.equal(handleSurfaceRequest({ method: 'GET', url: 'not a url' }, index()).status, 400)
  assert.equal(handleSurfaceRequest({ method: 'GET', url: `${SURFACE_SCHEME}://demo/panel.js\0` }, index()).status, 403)
})

test('入口：逃逸请求的响应体里没有机密内容（拒绝必须是拒绝，不能先送字节）', () => {
  for (const p of ['/../off/panel.js', '/evil.js', '/sneaky.js', '/notes.txt']) {
    const r = handleSurfaceRequest(req('GET', p), index())
    assert.notEqual(r.status, 200, `${p} 竟然成功了`)
    const text = r.body ? new TextDecoder().decode(r.body) : ''
    for (const s of SECRETS) assert.ok(!text.includes(s), `${p} 的响应体里出现了 ${s}`)
  }
})

// ---- ③ readSurfaceIndex ----

test('索引：空目录 → 空列表（不是 null）', () => {
  const empty = readSurfaceIndex(fs.mkdtempSync(path.join(os.tmpdir(), 'surface-empty-')))
  assert.deepEqual(empty, { version: 1, items: [] })
})

test('索引：非法 id 整条跳过，坏 manifest 带 error 照发', () => {
  const idx = readSurfaceIndex()
  const ids = idx.items.map((i) => i.id)
  assert.ok(!ids.includes('Bad_ID'), '非法 id 没被跳过')
  assert.ok(!ids.includes('../escape'), '上跳 id 没被跳过')
  assert.deepEqual(ids, ['demo', 'nested', 'off', 'broken'])
  const broken = idx.items.find((i) => i.id === 'broken')
  assert.ok(broken?.error, '坏 manifest 没带 error')
  assert.equal(broken?.enabled, true) // enabled 来自 index，不因 manifest 坏而失真
})

test('索引：manifest 的 kind/commands/data/icon/entry 都透出（loader 全靠它）', () => {
  const idx = readSurfaceIndex()
  const demo = idx.items.find((i) => i.id === 'demo')
  assert.ok(demo)
  assert.equal(demo?.kind, 'plugin:demo')
  assert.deepEqual(demo?.commands, ['workspace_list'])
  assert.deepEqual(demo?.data, ['surfaces_changed'])
  assert.equal(demo?.icon, 'icon.svg')
  assert.equal(demo?.entry, 'panel.js')
  const nested = idx.items.find((i) => i.id === 'nested')
  assert.equal(nested?.entry, 'dist/panel.js')
})

test('索引：manifest 的 entry 是绝对路径 / 上跳时不收（回缺省 panel.js）', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'surface-entry-'))
  fs.mkdirSync(path.join(dir, 'xx'), { recursive: true })
  fs.writeFileSync(path.join(dir, 'index.json'), JSON.stringify({ version: 1, items: [{ id: 'xx', enabled: true }] }))
  fs.writeFileSync(path.join(dir, 'xx', 'surface.json'), JSON.stringify({ id: 'xx', kind: 'plugin:xx', entry: '/etc/passwd', icon: '../x.svg' }))
  const item = readSurfaceIndex(dir).items[0]
  assert.equal(item.entry, 'panel.js')
  assert.equal(item.icon, undefined)
})

test('索引：版本不符 / 坏 JSON → 空列表（不让一份坏文件带黑协议与 IPC）', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'surface-badidx-'))
  fs.writeFileSync(path.join(dir, 'index.json'), '{"version":99,"items":[{"id":"demo"}]}')
  assert.deepEqual(readSurfaceIndex(dir).items, [])
  fs.writeFileSync(path.join(dir, 'index.json'), '{"items":')
  assert.deepEqual(readSurfaceIndex(dir).items, [])
})

test('surfaceRoot：configureSurfaceRoot 之后是它（生产由 main.ts 指到 <cfgDir>/surfaces）', () => {
  assert.equal(surfaceRoot(), root)
})

// ---- ④ protocol.handle 那一层：翻译层也真跑 ----

test('registerSurfaceProtocol：捕获真实 handler 并走 Request→Response 翻译', async () => {
  let handler: ((request: SurfaceRequestLike) => Response | Promise<Response>) | undefined
  let scheme = ''
  const fake: SurfaceProtocolLike = {
    handle: (s, h) => {
      scheme = s
      handler = h
    },
  }
  registerSurfaceProtocol(fake)
  assert.equal(scheme, SURFACE_SCHEME)
  assert.ok(handler, 'protocol.handle 没被调用')
  const res = await handler!({ method: 'GET', url: `${SURFACE_SCHEME}://demo/panel.js` })
  assert.equal(res.status, 200)
  assert.equal(res.headers.get('content-type'), 'text/javascript')
  const body = await res.text()
  assert.ok(body.includes('export const surface'))
  const denied = await handler!({ method: 'GET', url: `${SURFACE_SCHEME}://off/panel.js` })
  assert.equal(denied.status, 404)
})
