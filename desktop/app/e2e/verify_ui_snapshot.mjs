#!/usr/bin/env node
// `ui` 通道 + 渲染层快照（PR-12）的验证。
//
// 来源：docs/creator-mode-v0.1/02-pr-plan.md PR-12、03-code-plan.md §3.1–3.2、
//       00-conventions §4.7（测试必须走生产入口）。
//
// §4.7 的三个「容易测成别的东西」的点，这个脚本按口径钉住：
//   · **真的从 IPC 边界发**：探针只替 `window.desktop`（command/onEvent/settingsGet），
//     store 的 start() → startUISnapshot → transport.send 全是真的；断言读的是
//     `window.desktop.command` 收到的信封，不是测试里另造的一条发送函数。
//   · **真的注册表**：探针 page 与 src/main.tsx 一样 `import '../../src/surfaces/builtin'`，
//     T0 用 `listSurfaces()` 当场戳穿「注册没生效、测的是兜底」（04 F45）。
//   · **真的 store 动作**：开标签/切标签/禁用面板全部走 store 与 health 的公开函数；
//     禁用状态读的是真 localStorage + 真通知通道（health → notifySurfacesChanged）。
//
// 覆盖（每条都能被实现打死）：
//   T0 注册表自检：git/activity/file/web 四个内置 provider 都在。
//   T1 挂载后 ≤800ms 发出第一条 `{type:'ui',op:'snapshot'}`，字段齐全（layout/surfaces/
//      tabs/editors/theme/viewport/at），且**没有任何长字符串**（无文件内容/HTML）。
//   T2 连开 3 个标签（间隔 50ms）→ 500ms 窗口内只合并成 1 条（允许 2 条，**绝不是 3 条**），
//      且合并后的快照里 3 个标签都在（证明是「合并」不是「丢事件」）。
//   T3 切换激活标签 → 快照的 tabs.activeId 与逐条 active 跟着变。
//   T4 disableSurface('git') → surfaces 里 git 的 disabled:true（且 listSurfaces 里消失）。
//   T5 bridge → 渲染层：注入 surfaces_changed → uiBus 收到；未知 op / 非 JSON 行不产生
//      pageerror（页面上任何未捕获异常都算失败）。
//   T6 窗口 resize 走同一条 debounce → 快照的 viewport 更新。
//
// 用法：cd desktop/app && node e2e/verify_ui_snapshot.mjs
// 退出码：全通过 0；任一断言失败 1；前置不满足（端口被占 / 探针服务没起来）2。
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { exitCodeOf, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')

// 端口按口径固定 5227（PR-12）；归属自检走 _serve.mjs 的 waitProbePage，
// 撞上一个还没退干净的别人的 vite 时报「前置不满足」并退出 2（04 F46）。
const PORT = 5227
const PROBE_DIR = path.join(__dirname, '_uisnapprobe_run')
const PROBE_MARKER = '<title>ui-snapshot probe</title>'
const PROBE_URL = `http://localhost:${PORT}/e2e/_uisnapprobe_run/index.html`

const TITLE_MAX = 200
const SNAPSHOT_MAX_BYTES = 256 * 1024

// —— 探针页 ——
// 真 store + 真 RightPanel。`window.desktop` 只提供 IPC 边界（command/onEvent/settingsGet），
// 其余方法用 Proxy 兜成异步空操作：真组件（RightPanel 的原生视图 effect）会调用
// embed* 等方法，缺一个就会在 effect 里抛错并把整棵 React 树卸载（PR-10 实测过）。
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8">'
  + `<title>ui-snapshot probe</title></head>`
  + '<body><div id="root" style="height:100vh"></div>'
  + '<script type="module" src="./main.tsx"></script></body></html>')

fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
// 探针页：真 store + 真 RightPanel + 真注册表。
//
// ⚠️ 这里**全部**用 await import 而不是顶部 static import，顺序是断言的前提：
// store 在模块求值时就 \`const transport = getTransport()\` 把实现定死了，而生产里
// window.desktop 由 preload **先于** bundle 注入。static import 会被 hoist 到赋值之前，
// 探针就会静默跑在 mock transport 上 —— 症状是 window.desktop.command 一条都没收到，
// 看起来像「快照没实现」（PR-12 实测踩过，这条注释是为下一个踩的人留的）。
import '../../src/index.css'

const sent: Array<Record<string, unknown>> = []
const uiEvents: Array<{ op: string; data: unknown }> = []
const feeds: Array<(line: string) => void> = []
let bootSettings: unknown = null
const mountAt = performance.now()

// IPC 边界替身：**只**替 window.desktop（生产 electronTransport 走 d.command / d.onEvent）。
// 其余方法用 Proxy 兜成异步空操作：RightPanel 的原生视图 effect 会调用 embed*，
// 缺一个就会在 effect 里抛错并把整棵 React 树卸载（PR-10 实测过）。
const stub: Record<string, unknown> = {
  startBridge: async () => ({ ok: true }),
  command: async (cmd: unknown) => {
    sent.push({ ...(cmd as Record<string, unknown>), __at: performance.now() })
    return { ok: true }
  },
  // 多监听者（真 Electron 的 onEvent 就是逐个 addListener）：store 与其它订阅方
  // 各注册一份，只记最后一个会让注入的行投错人（PR-12 实测踩过）。
  onEvent: (cb: (line: string) => void) => {
    feeds.push(cb)
    return () => {
      const i = feeds.indexOf(cb)
      if (i >= 0) feeds.splice(i, 1)
    }
  },
  onExit: () => () => {},
  settingsGet: async () => bootSettings ?? {},
  settingsSet: async () => ({ ok: true }),
  startupMark: () => {},
}
;(window as unknown as { desktop: unknown }).desktop = new Proxy(stub, {
  get(t, k) {
    if (k in t) return t[k as string]
    return () => Promise.resolve({ ok: true })
  },
})

const { createRoot } = await import('react-dom/client')
const [{ default: RightPanel }] = await Promise.all([import('../../src/components/RightPanel')])
await import('../../src/surfaces/builtin') // 副作用注册（F45：缺了它 T0 会当场红）
const { getSurface, listSurfaces } = await import('../../src/surfaces/registry')
const { disableSurface, enableSurface, listDisabled } = await import('../../src/surfaces/health')
const { subscribeUI } = await import('../../src/lib/uiBus')
const { useAppStore } = await import('../../src/store/useAppStore')

const WS = '/mock/proj-ui-snapshot'
useAppStore.setState({ workspace: WS })
bootSettings = useAppStore.getState().settings

const w = window as unknown as Record<string, unknown>
w.__probe = {
  mountAt,
  sent,
  uiEvents,
  store: useAppStore,
  // 注入一条 bridge→renderer 的行（走真 transport.onEvent → 真 store 解析分支）。
  inject: (line: string) => feeds.forEach((f) => f(line)),
  subscribe: (op: string) => subscribeUI(op, (data) => uiEvents.push({ op, data })),
  feedCount: () => feeds.length,
  surfaces: {
    list: () => listSurfaces().map((p) => p.kind),
    has: (k: string) => !!getSurface(k as never),
    disable: (k: string) => disableSurface(k as never),
    enable: (k: string) => enableSurface(k as never),
    disabled: () => listDisabled(),
  },
  // 快照类的读：**只**从 sent 里挑 ui 信封（断言不接触内部实现）。
  snapshots: () =>
    sent
      .filter((c) => c.type === 'ui' && (c.payload as Record<string, unknown> | undefined)?.op === 'snapshot')
      .map((c) => ({ ...((c.payload as Record<string, unknown>).data as Record<string, unknown>), __at: c.__at as number })),
  open: (kind: string, extra: Record<string, unknown>) =>
    useAppStore.getState().openTab({ kind, ...extra } as never),
  activate: (id: string) => useAppStore.getState().activateTab(id),
  tabIds: () => {
    const s = useAppStore.getState()
    const scope = s.activeSessionId || s.workspace || ''
    return (s.tabsByScope[scope]?.tabs ?? []).map((t) => t.id)
  },
}

void useAppStore.getState().start(WS)
createRoot(document.getElementById('root')!).render(<RightPanel />)
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
server.stderr.on('data', (d) => { const s = String(d); if (!/ready in|Local:/.test(s)) process.stderr.write(s) })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

const fails = []
function check(label, cond, detail = '') {
  if (cond) console.log(`  ✅ ${label}`)
  else { fails.push(label); console.log(`  ❌ ${label}${detail ? ' —— ' + detail : ''}`) }
}

// 递归找最长的字符串 + 键名（T1 的「不许塞内容」判据）。
function longestString(v, key = '') {
  if (typeof v === 'string') return { len: v.length, key, sample: v.slice(0, 40) }
  if (Array.isArray(v)) {
    let best = { len: 0, key, sample: '' }
    for (const x of v) {
      const r = longestString(x, key)
      if (r.len > best.len) best = r
    }
    return best
  }
  if (v && typeof v === 'object') {
    let best = { len: 0, key, sample: '' }
    for (const [k, x] of Object.entries(v)) {
      const r = longestString(x, k)
      if (r.len > best.len) best = r
    }
    return best
  }
  return { len: 0, key, sample: '' }
}

let code = 0
try {
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:ui-snapshot', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  page.setDefaultTimeout(5000)
  await page.addInitScript(() => {
    try { localStorage.clear() } catch { /* 忽略 */ }
  })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))

  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!window.__probe, null, { timeout: 30000 })
  // 有界等待：**不**用一个无限超时把「一条都没发」变成 TimeoutError —— 那会把
  // 「实现没接上」说成「探针异常」，丢掉真正该看的那条断言（PR-12 变异证据踩过）。
  await page.waitForFunction(() => window.__probe.snapshots().length > 0, null, { timeout: 3000 }).catch(() => {})

  const snapshots = () => page.evaluate(() => window.__probe.snapshots())

  // ---- T0 注册表自检（F45：先证明探针走的是被测那条路）----
  console.log('\nT0 内置 provider 真的注册了（否则下面测的是兜底分支）')
  const kinds = await page.evaluate(() => window.__probe.surfaces.list())
  for (const k of ['git', 'activity', 'file', 'web']) {
    check(`listSurfaces() 含 ${k}`, kinds.includes(k), JSON.stringify(kinds))
  }

  // ---- T1 挂载即发 + 字段齐全 + 没有内容 ----
  console.log('\nT1 挂载后 ≤800ms 发出第一条快照，字段齐全且只有元数据')
  const first = (await snapshots())[0]
  const delta = first ? first.__at - (await page.evaluate(() => window.__probe.mountAt)) : NaN
  check(`第一条快照在挂载后 800ms 内（实测 ${Number.isFinite(delta) ? delta.toFixed(0) + 'ms' : '无快照'}）`,
    Number.isFinite(delta) && delta >= 0 && delta <= 800)
  if (!first) {
    console.log('  ❌ 一条 ui 快照都没发出 —— 后续断言按空数据继续（它们会各自红）')
    fails.push('没有任何 ui 快照')
  }
  for (const k of ['layout', 'surfaces', 'tabs', 'editors', 'theme', 'viewport', 'at']) {
    check(`快照含 ${k}`, first[k] !== undefined)
  }
  check('layout 有左右栏折叠态', typeof first?.layout?.leftExpanded === 'boolean' && typeof first?.layout?.right?.open === 'boolean')
  check('at 是 epoch ms', typeof first?.at === 'number' && first.at > 1_600_000_000_000)
  check('surfaces 是数组且每项有 kind/group/order/disabled',
    Array.isArray(first?.surfaces) && first.surfaces.length > 0
    && first.surfaces.every((s) => typeof s.kind === 'string' && typeof s.group === 'string'
      && typeof s.order === 'number' && typeof s.disabled === 'boolean'))
  check('editors 有 open/dirty 两个计数',
    typeof first?.editors?.open === 'number' && typeof first?.editors?.dirty === 'number')
  const longest = longestString(first)
  // 这条是 PR-12 的硬约束：快照里不许出现「文件内容样式」的长字符串。
  // 判据取标题上限 200：任何超长字符串都只能是从内容里抄进来的。
  check(`没有任何长字符串（最长 ${longest.len} 字符，key=${longest.key}）`, longest.len <= TITLE_MAX,
    `样例：${JSON.stringify(longest.sample)}`)
  const bytes = Buffer.byteLength(JSON.stringify(first), 'utf8')
  check(`序列化后 ${bytes} 字节 < 256 KiB`, bytes < SNAPSHOT_MAX_BYTES)

  // ---- T2 debounce 合并（3 次变化 → 不是 3 条）----
  console.log('\nT2 连开 3 个标签（间隔 50ms）→ 合并成 1 条（允许 2 条）')
  const before = (await snapshots()).length
  for (let i = 0; i < 3; i += 1) {
    await page.evaluate((n) => window.__probe.open('file', { path: `/mock/proj-ui-snapshot/f${n}.txt`, mode: 'edit' }), i)
    await page.waitForTimeout(50)
  }
  await page.waitForTimeout(700) // > debounce(500)
  const after = (await snapshots()).length
  const merged = after - before
  check(`500ms 窗口内新增 ${merged} 条快照（3 次打开被合并）`, merged >= 1 && merged <= 2 && merged !== 3,
    `before=${before} after=${after}`)
  const lastTabs = (await snapshots()).at(-1)?.tabs?.tabs ?? []
  check(`合并后的快照含全部 3 个新标签（${lastTabs.length} 个）`, lastTabs.length >= 3)

  // ---- T3 切换激活标签 → active 跟着变 ----
  console.log('\nT3 切换激活标签 → 快照的 active 跟着变')
  const ids = await page.evaluate(() => window.__probe.tabIds())
  check('至少 3 个标签可取 id', ids.length >= 3, JSON.stringify(ids))
  await page.evaluate((id) => window.__probe.activate(id), ids[0])
  await page.waitForTimeout(700)
  const afterActivate = (await snapshots()).at(-1)
  check(`activeId = ${ids[0]}`, afterActivate?.tabs?.activeId === ids[0], JSON.stringify(afterActivate?.tabs?.activeId))
  const activeEntries = (afterActivate?.tabs?.tabs ?? []).filter((t) => t.active)
  check('恰好一个 active 且是刚切的那个', activeEntries.length === 1 && activeEntries[0].id === ids[0],
    JSON.stringify(activeEntries.map((t) => t.id)))

  // ---- T4 面板禁用 → surfaces.disabled ----
  console.log('\nT4 disableSurface(git) → 快照里 git.disabled = true')
  await page.evaluate(() => window.__probe.surfaces.disable('git'))
  await page.waitForTimeout(700)
  const afterDisable = (await snapshots()).at(-1)
  const gitEntry = (afterDisable?.surfaces ?? []).find((s) => s.kind === 'git')
  check('surfaces 里 git 仍在，且 disabled:true', !!gitEntry && gitEntry.disabled === true, JSON.stringify(gitEntry))
  const listed = await page.evaluate(() => window.__probe.surfaces.list())
  check('对照：listSurfaces() 里 git 已消失（禁用真的生效）', !listed.includes('git'), JSON.stringify(listed))
  await page.evaluate(() => window.__probe.surfaces.enable('git'))

  // ---- T5 bridge → 渲染层：surfaces_changed 通道 + 未知 op 静默 ----
  console.log('\nT5 注入 surfaces_changed / 未知 op / 非 JSON 行')
  await page.evaluate(() => window.__probe.subscribe('surfaces_changed'))
  await page.evaluate(() => window.__probe.inject(JSON.stringify({ type: 'ui', op: 'surfaces_changed', data: { at: 42 } })))
  const gotChanged = await page.evaluate(() => window.__probe.uiEvents.filter((e) => e.op === 'surfaces_changed').length)

  check('uiBus 收到 surfaces_changed', gotChanged === 1, String(gotChanged))
  await page.evaluate(() => window.__probe.inject(JSON.stringify({ type: 'ui', op: 'no_such_op', data: {} })))
  await page.evaluate(() => window.__probe.inject('{ not json'))
  await page.waitForTimeout(100)
  check('未知 op / 非 JSON 行不产生页面错误', errs.length === 0, errs.join(' | '))

  // ---- T6 resize 走同一条 debounce ----
  console.log('\nT6 窗口 resize → 快照 viewport 更新')
  const beforeResize = (await snapshots()).length
  await page.setViewportSize({ width: 1000, height: 700 })
  await page.waitForTimeout(700)
  const resized = (await snapshots()).at(-1)
  const afterResize = (await snapshots()).length
  check('resize 产生了新快照', afterResize > beforeResize, `before=${beforeResize} after=${afterResize}`)
  check(`viewport = 1000×700（实得 ${resized.viewport?.w}×${resized.viewport?.h}）`,
    resized.viewport?.w === 1000 && resized.viewport?.h === 700)

  // ---- 收尾：整场零 pageerror ----
  console.log('\nT7 全程零 pageerror')
  check('pageerror = 0', errs.length === 0, errs.join(' | '))

  await browser.close()
} catch (e) {
  if (isPrecondition(e)) {
    console.error(`\n[前置不满足] ${e.message}`)
    code = exitCodeOf(e)
  } else {
    console.error('\n[探针异常]', e)
    code = 1
  }
  cleanup()
  process.exit(code)
}
cleanup()
if (fails.length) {
  console.log(`\n❌ ${fails.length} 条断言失败：\n  - ${fails.join('\n  - ')}`)
  process.exit(1)
}
console.log('\n✅ ui 通道 + 快照推送全部通过')
process.exit(0)
