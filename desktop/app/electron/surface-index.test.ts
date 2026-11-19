// surface-index.test.ts —— 读侧（`~/.go-code/surfaces/index.json` + 每个 `<id>/surface.json`）单测。
// 运行：esbuild bundle 后 node --test（见 package.json test:surface-index）。
//
// 为什么单测这一层：e2e 探针把 `surfaceIndex` 这个 IPC **整条替掉**（00-conventions §4.7），
// 所以「manifest.entries 怎么变成索引里的 entries[]」在链上没有任何覆盖 —— 而它正是
// R16 的入口在渲染层的唯一来源。这里覆盖三条：原样透传 / 防御性丢弃坏项 / 零入口仍是合法。
//
// 隔离：`readSurfaceIndex(root)` 显式收 root（生产由 configureSurfaceRoot 给），
// 每个用例一个临时目录，不碰真实的 ~/.go-code。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import { readSurfaceIndex, SURFACE_INDEX_VERSION } from './surface-index.ts'

/** mkRoot 造一个 surfaces 根目录（index.json + 若干 <id>/surface.json）。 */
function mkRoot(items: Array<Record<string, unknown>>): string {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'gocode-surface-index-'))
  fs.writeFileSync(path.join(root, 'index.json'), JSON.stringify({ version: SURFACE_INDEX_VERSION, items }), 'utf8')
  return root
}

function mkPlugin(root: string, id: string, manifest: Record<string, unknown>): void {
  fs.mkdirSync(path.join(root, id), { recursive: true })
  fs.writeFileSync(path.join(root, id, 'surface.json'), JSON.stringify(manifest), 'utf8')
}

const entry = (id: string, extra: Record<string, unknown> = {}) => ({ id, version: '1.0.0', hash: 'h'.repeat(64), enabled: true, ...extra })

test('entries / name 从 manifest 原样回来（index.json 的 schema 不动）', () => {
  const root = mkRoot([entry('course-manager')])
  mkPlugin(root, 'course-manager', {
    id: 'course-manager',
    kind: 'plugin:course-manager',
    name: { zh: '课程管理', en: 'Course manager' },
    entries: [
      { id: 'overview', title: '总览', order: 10 },
      { id: 'schedule', title: '课表', icon: 'calendar.svg', order: 20 },
      { id: 'resources', title: '资源' },
    ],
  })
  const idx = readSurfaceIndex(root)
  assert.equal(idx.items.length, 1)
  const it = idx.items[0]
  assert.equal(it.name, '课程管理', 'name 取 zh（兜底顺序 zh → en → 任意非空）')
  assert.deepEqual(it.entries, [
    { id: 'overview', title: '总览', order: 10 },
    { id: 'schedule', title: '课表', icon: 'calendar.svg', order: 20 },
    { id: 'resources', title: '资源' },
  ])
  // 铁律：index.json 的字段一个都没多（entries 是 manifest 字段，不是 store 字段）
  const onDisk = JSON.parse(fs.readFileSync(path.join(root, 'index.json'), 'utf8'))
  assert.equal(onDisk.items[0].entries, undefined, 'index.json 里不该出现 entries')
})

test('entries[].title 两种写法（R16b）：单字符串与 {zh,en} 都原样返回，空对象/非字符串丢掉', () => {
  const root = mkRoot([entry('mixed')])
  mkPlugin(root, 'mixed', {
    id: 'mixed',
    kind: 'plugin:mixed',
    entries: [
      { id: 'overview', title: '总览' },                            // 老写法：一个字都不用改
      { id: 'schedule', title: { zh: '课表', en: 'Schedule' } },     // 两种语言都声明
      { id: 'plan', title: { en: 'Plan' } },                        // 只声明 en 也合法
      { id: 'empty-obj', title: {} },                               // 一个非空都没有 → 丢
      { id: 'blank', title: { zh: '   ', en: '' } },                // 只有空白 → 丢
      { id: 'num', title: 7 },                                      // 不是字符串/对象 → 丢
    ],
  })
  const it = readSurfaceIndex(root).items[0]
  // 「原样」= 索引层不选语言、不归一化形状：前端按当前语言选（entries.ts 的 entryTitleText）。
  assert.deepEqual(it.entries, [
    { id: 'overview', title: '总览' },
    { id: 'schedule', title: { zh: '课表', en: 'Schedule' } },
    { id: 'plan', title: { en: 'Plan' } },
  ], JSON.stringify(it.entries))
})

test('坏 entry 的处置：id/title 坏丢这一项，icon 坏只丢图标（形状 / 重复 / 上限）', () => {
  const root = mkRoot([entry('messy')])
  mkPlugin(root, 'messy', {
    id: 'messy',
    kind: 'plugin:messy',
    name: { en: 'Messy' }, // 没有 zh → 退到 en
    entries: [
      { id: 'ok', title: '可以' },
      { id: 'Bad ID', title: '大写+空格' }, // id 非法 → 丢
      { id: 'ok', title: '重复' }, // 重复 id → 丢
      { id: 'empty-title', title: '   ' }, // title 空 → 丢
      { id: 'escapes', title: '逃逸', icon: '../x.svg' }, // icon 逃逸 → 丢（图标不安全）
      { id: 'abs-icon', title: '绝对', icon: '/etc/passwd' }, // 绝对路径 → 丢
      { id: 'long', title: '看'.repeat(65) }, // title 超 64 → 截断而不是丢（标题还能用）
      'not-an-object',
      null,
    ],
  })
  const it = readSurfaceIndex(root).items[0]
  assert.equal(it.name, 'Messy')
  // id/title 坏的丢**这一项**；icon 不安全的只丢**图标**、保留入口 ——
  // 入口的可见性比图标重要（D-2 的硬约束是「至少一个可见入口」；D-3 说图标坏了用 kind 图标兜底）。
  assert.deepEqual(it.entries.map((e) => e.id), ['ok', 'escapes', 'abs-icon', 'long'], JSON.stringify(it.entries))
  assert.equal(it.entries[1].icon, undefined, '../ 逃逸的 icon 不能进索引（渲染层会直接拼 URL）')
  assert.equal(it.entries[2].icon, undefined, '绝对路径同理')
  assert.equal(it.entries[3].title.length, 64, '超长 title 截到 64 字符（rune 不是字节）')
  // 插件本身照常可用：坏 manifest 才会带 error，坏 entry 不带
  assert.equal(it.error, undefined)
})

test('21 个 entry 只收前 20（与安装器同一条上限）', () => {
  const root = mkRoot([entry('many')])
  mkPlugin(root, 'many', {
    id: 'many',
    kind: 'plugin:many',
    entries: Array.from({ length: 21 }, (_, i) => ({ id: `page-${String(i).padStart(2, '0')}`, title: `页 ${i}` })),
  })
  const it = readSurfaceIndex(root).items[0]
  assert.equal(it.entries.length, 20)
  assert.equal(it.entries[19].id, 'page-19')
})

test('零 entries（缺省 / 空数组 / manifest 读不出来）都是合法的空列表', () => {
  const root = mkRoot([entry('alpha'), entry('beta'), entry('broken')])
  mkPlugin(root, 'alpha', { id: 'alpha', kind: 'plugin:alpha' })
  mkPlugin(root, 'beta', { id: 'beta', kind: 'plugin:beta', entries: [] })
  // broken：manifest 不是 JSON → 这条带 error 照发（坏一条不炸整份索引），entries 为空
  fs.mkdirSync(path.join(root, 'broken'), { recursive: true })
  fs.writeFileSync(path.join(root, 'broken', 'surface.json'), '{oops', 'utf8')
  const items = readSurfaceIndex(root).items
  const byId = Object.fromEntries(items.map((i) => [i.id, i]))
  assert.deepEqual(byId.alpha.entries, [])
  assert.equal(byId.alpha.name, undefined)
  assert.deepEqual(byId.beta.entries, [])
  assert.deepEqual(byId.broken.entries, [])
  assert.ok(byId.broken.error, '坏 manifest 仍要带上可读原因（这一条不因 entries 改变）')
})

// —— URL 型插件（2026-11）：没有 panel.js，内容在 web.url + entries[].path ——

test('web.url / permissions / entries[].path 原样进索引', () => {
  const root = mkRoot([entry('learning-world')])
  mkPlugin(root, 'learning-world', {
    id: 'learning-world',
    kind: 'plugin:learning-world',
    name: { zh: '学习世界' },
    web: { url: 'http://127.0.0.1:5173', permissions: ['media', 'geolocation'] },
    entries: [
      { id: 'learn', title: '学习世界', path: '/' },
      { id: 'admin', title: '课程后台', path: '/admin' },
      { id: 'hash', title: '哈希路由', path: '#/detail?id=1' },
    ],
  })
  const it = readSurfaceIndex(root).items[0]
  assert.deepEqual(it.web, { url: 'http://127.0.0.1:5173', permissions: ['media'] }, '只认识的权限进索引')
  assert.deepEqual(it.entries.map((e) => e.path), ['/', '/admin', '#/detail?id=1'])
  assert.equal(it.error, undefined)
})

test('web.start 原样进索引（命令/args/cwd/env/health/timeout_ms）', () => {
  const root = mkRoot([entry('server-app')])
  mkPlugin(root, 'server-app', {
    id: 'server-app',
    kind: 'plugin:server-app',
    web: {
      url: 'http://127.0.0.1:5235',
      start: {
        command: 'node',
        args: ['server.mjs'],
        cwd: '.',
        env: { PORT: '5235' },
        health: '/health',
        timeout_ms: 15000,
      },
    },
    entries: [{ id: 'home', title: '首页', path: '/' }],
  })
  const it = readSurfaceIndex(root).items[0]
  assert.deepEqual(it.web, {
    url: 'http://127.0.0.1:5235',
    start: { command: 'node', args: ['server.mjs'], cwd: '.', env: { PORT: '5235' }, health: '/health', timeoutMs: 15000 },
  })
  assert.equal(it.error, undefined)
})

test('坏 web.start（空命令 / 逃逸 health / 坏 timeout）→ 整条 web 带 error', () => {
  for (const [name, start] of [
    ['empty', { command: '' }],
    ['health', { command: 'node', health: '//evil.com' }],
    ['timeout', { command: 'node', timeout_ms: 10 }],
    ['env', { command: 'node', env: { '1BAD': 'x' } }],
    ['cwd', { command: 'node', cwd: '../escape' }],
  ] as const) {
    const root = mkRoot([entry(`bad-start-${name}`)])
    mkPlugin(root, `bad-start-${name}`, {
      id: `bad-start-${name}`,
      kind: `plugin:bad-start-${name}`,
      web: { url: 'http://127.0.0.1:5235', start },
    })
    const it = readSurfaceIndex(root).items[0]
    assert.equal(it.web, undefined, name)
    assert.match(it.error || '', /web/, name)
  }
})

test('坏 web.url 带 error（loader 会跳过它，而不是退回 import panel.js）', () => {
  const root = mkRoot([entry('bad-web')])
  mkPlugin(root, 'bad-web', {
    id: 'bad-web',
    kind: 'plugin:bad-web',
    web: { url: 'file:///etc/passwd' },
  })
  const it = readSurfaceIndex(root).items[0]
  assert.equal(it.web, undefined)
  assert.match(it.error || '', /web/)
})

test('entries[].path 坏（绝对 URL / 上跳）→ 丢这一项，不把 origin 交出去', () => {
  const root = mkRoot([entry('bad-path')])
  mkPlugin(root, 'bad-path', {
    id: 'bad-path',
    kind: 'plugin:bad-path',
    web: { url: 'http://127.0.0.1:5173' },
    entries: [
      { id: 'ok', title: '正常', path: '/admin' },
      { id: 'external', title: '外站', path: 'https://evil.com/x' },
      { id: 'proto-rel', title: '协议相对', path: '//evil.com/x' },
      { id: 'up', title: '上跳', path: '/a/../b' },
    ],
  })
  const it = readSurfaceIndex(root).items[0]
  assert.deepEqual(it.entries.map((e) => e.id), ['ok'])
})
