// 「重启后标签集与激活标签恢复 + 视图按需重建」验证（阶段 5：持久化 go-code.sessionTabs）。
//
// 方案原文（docs/RIGHT_PANEL_TABS_PLAN.md §6 阶段 5）：验收 = 「重启后标签集与激活标签恢复；
// 视图按需重建」。本脚本把这两句翻成可执行断言。
//
// 为什么用**探针页 + page.reload()**（而不是真 Electron 二次启动）：
//   · 阶段 5 的全部逻辑都在渲染层（store 读写 localStorage + RightPanel 的收敛 effect），
//     主进程只被**调用**（embed:open-target）。reload 会重新加载整个渲染层模块图 ——
//     与「应用重启」在**被测范围内**等价，且能精确控制假视图池（真 Electron 里视图池由
//     Chromium 决定，反而验不到「恢复时一个视图都没建」这条反向断言）。
//   · 真 Electron 二次启动（_electron.launch ×2 + 同 HAI_DATA_DIR）多花 ~40s 且要动共享
//     dist/，收益只在「主进程侧也真的重启了」这一点上 —— 那一点由本脚本的**假池从零开始**
//     （reload 后 window.__pool 为空、id 序号从 t1 重来）同样覆盖。
//   若后续要更强的端到端口径，可另加一条真重启用例；本阶段的回归门禁以本脚本为准。
//
// 覆盖：
//   P1 写入：标签集/激活标签/url 快照落进 go-code.sessionTabs（且不写运行时字段）
//   P2 重启（reload）：标签集、顺序、类型、diff/source 模式、激活标签全部恢复；
//      **不展开右栏、不收左栏**（恢复 ≠ 激活）
//   P3 视图按需重建（**反向断言**）：恢复后视图池为空；重启后主进程推来的空 targets
//      不得把恢复的 web 标签当「已关闭」删掉
//   P4 用户展开面板 / 点标签 → 这一刻才建视图（1 个，url = 快照），且**接回原标签**
//      （不新增重复标签）
//   P5 http(s) 网页（无 srcPath）走 url 快照重建
//   P6 坏数据不炸（截断 JSON → 静默丢弃、正常启动）
//   P7 跨 scope 不串（A 工作区/会话的标签不出现在 B）
//
// 用法：cd desktop/app && node e2e/verify_session_tabs_persist.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { spawn } from 'child_process'
import fs from 'node:fs'
import net from 'node:net'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = 5208
const LS_TABS = 'go-code.sessionTabs'

// 照抄 verify_auto_open_visual.mjs 的 waitPort（host:'localhost' 让 Node 按系统解析顺序
// 试 IPv4/IPv6 —— 直连 127.0.0.1 会在 vite 只绑 ::1 时永远连不上）。
async function waitPort(port, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { clearTimeout(timer); s.destroy(); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 探针页：真实 store + 真实 RightPanel + **假内嵌视图池**（window.desktop 的 embed 通路）。
// 注入点必须在 store 模块加载之前（subscribeEmbedEvents 只在模块初始化时订阅一次）→ 内联
// script（经典脚本先于 module 脚本执行），与 verify_auto_open_visual.mjs 同一写法。
// 关键：假池的 id 序号在每次 reload 后**从 t1 重来**（与主进程 't'+自增序号同语义）——
// 「恢复的标签不带池身份」这条设计正是为了避免撞上重启后重号的另一个页面。
const PROBE_DIR = path.join(__dirname, '_tabsprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>session-tabs probe</title>'
  + `<script>
window.__pool = []
window.__navCb = null
window.__seq = 0
window.__activateCalls = []
const impl = {
  embedStatus: async () => ({ ok: true, targets: window.__pool.map((t) => ({ ...t })) }),
  embedOpenTarget: async (url, o) => {
    const id = 't' + (++window.__seq) // 序号永不重置（reset 只清池）：与主进程一致，id 不复用
    window.__pool.push({ id, url, title: '', sid: (o && o.sid) || '', srcPath: (o && o.srcPath) || '' })
    return { ok: true, state: { activeId: (o && o.activate) ? id : null, targets: window.__pool.map((t) => ({ ...t })) } }
  },
  embedActivate: async (id) => { window.__activateCalls.push(id); return { ok: true, state: { activeId: id, targets: window.__pool.map((t) => ({ ...t })) } } },
  embedCloseTarget: async (id) => { window.__pool = window.__pool.filter((t) => t.id !== id); return { ok: true } },
  embedReloadTarget: async () => ({ ok: true }),
  embedNavigateTarget: async () => ({ ok: true }),
  onEmbedNav: (cb) => { window.__navCb = cb; return () => {} },
  onEmbedAiAction: (cb) => { window.__aiCb = cb; return () => {} },
  embedHide: async () => ({ ok: true }),
  embedShow: async () => ({ ok: true }),
  embedResize: async () => ({ ok: true }),
  embedCommand: async () => ({ ok: true }),
  embedDebuggerProxy: async () => ({ ok: false }), // 不起自举循环（探针不需要 attach）
}
window.desktop = new Proxy(impl, {
  get(t, k) {
    if (k in t) return t[k]
    if (typeof k !== 'string') return undefined
    if (k.startsWith('on')) return () => () => {}
    return async () => ({ ok: true })
  },
})
</script></head>`
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import RightPanel from '../../src/components/RightPanel'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

// 启动顺序与真实应用一致：先 start()（它会重置视图字段），会话恢复（new_session 响应 →
// activeSessionId）**在其后**发生 —— 顺序反了会把恢复的激活标签又打回默认。
useAppStore.setState({ workspace: '/w' })
;(window as unknown as { __store: unknown }).__store = useAppStore
void useAppStore.getState().start('/w')

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
  else { fails.push(label); console.log(`  ❌ ${label} ${detail}`) }
}

try {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1100, height: 900 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))

  const URL = `http://localhost:${PORT}/e2e/_tabsprobe_run/index.html`
  // 加载并「恢复会话」：真实链路上 activeSessionId 由 new_session 响应写入，探针直接置位
  // （与 verify_auto_open_visual.mjs 同口径）。等一小会儿让 store/面板 effect 跑完。
  // seed：预置 localStorage（模拟「上一次运行留下的存储」）。写之前先等一个防抖窗口 ——
  // 关窗前 store 会把**在途**的待写补上（beforeunload），不等就会被它覆盖掉 seed。
  const load = async (seed) => {
    if (seed !== undefined) {
      await page.waitForTimeout(400)
      await page.evaluate(([k, v]) => { if (v === null) localStorage.removeItem(k); else localStorage.setItem(k, v) }, [LS_TABS, seed])
    }
    await page.reload({ waitUntil: 'domcontentloaded' })
    await page.waitForFunction(() => !!window.__store, null, { timeout: 20000 })
    await page.waitForTimeout(600)
    await page.evaluate(() => { window.__store.setState({ workspace: '/w', activeSessionId: 'S1' }) })
    await page.waitForTimeout(250)
  }
  await page.goto(URL, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!window.__store, null, { timeout: 20000 })
  await page.waitForTimeout(600)
  // 每个用例前清掉上一次的持久化（探针用的是真 localStorage：不清就会串到下一个用例）
  await page.evaluate(([k]) => localStorage.removeItem(k), [LS_TABS])

  // 快照：面板三态 + 本 scope 的标签集 + 视图池 + 标签条的 DOM 事实
  const snap = () => page.evaluate(() => {
    const s = window.__store.getState()
    const scope = s.activeSessionId || s.workspace || ''
    const set = s.tabsByScope[scope] || {}
    const tabs = set.tabs || []
    const rows = [...document.querySelectorAll('.rtabs .rtab')].map((r) => ({
      kind: r.getAttribute('data-kind'),
      view: r.getAttribute('data-view') || '',
      pending: r.getAttribute('data-pending') === '1',
      sleeping: r.getAttribute('data-sleeping') === '1',
      on: r.getAttribute('aria-selected') === 'true',
      title: (r.querySelector('.rtab-t')?.textContent ?? '').trim(),
    }))
    return {
      tab: s.rightTab, expanded: s.rightExpanded, left: s.leftExpanded,
      kinds: tabs.map((x) => x.kind).join(','),
      modes: tabs.filter((x) => x.kind === 'file').map((x) => `${(x.path || '').split('/').pop()}:${x.mode ?? 'source'}`).join(','),
      webView: tabs.filter((x) => x.kind === 'web').map((x) => x.viewId ?? '(pending)').join(','),
      activeKind: (tabs.find((x) => x.id === set.activeId) || {}).kind || '',
      activeId: set.activeId || '',
      tabIds: tabs.map((x) => x.id).join(','),
      pool: window.__pool.map((t) => ({ id: t.id, url: t.url, srcPath: t.srcPath })),
      activateCalls: window.__activateCalls.slice(),
      rows,
      barVisible: !!document.querySelector('.rtabs'),
      railVisible: !!document.querySelector('.right-rail .rbtn'),
    }
  })
  const stored = () => page.evaluate(([k]) => localStorage.getItem(k), [LS_TABS])
  // 驱动真实的 onEmbedNav（假池 → 真 store 逻辑）：与主进程推过来的形态一致
  const nav = (activeId) => page.evaluate(([aid]) => {
    const s = window.__store.getState()
    window.__navCb({
      id: aid, url: (window.__pool.find((t) => t.id === aid) || {}).url || '', title: '', loading: false,
      sid: s.activeSessionId || '', visible: true, srcPath: (window.__pool.find((t) => t.id === aid) || {}).srcPath || '',
      canGoBack: false, canGoForward: false, activeId: aid, targets: window.__pool.map((t) => ({ ...t })),
    })
  }, [activeId])

  // —— P1：写入（打开几个标签 → 落盘形态）——
  console.log('P1 标签集落进 localStorage（go-code.sessionTabs）')
  await load(null)
  await page.evaluate(async () => {
    const s = window.__store.getState()
    s.openFile('/w/src/a.ts') // source 标签
    s.openFile('/w/src/a.ts', 'tool-1') // 同一文件的 diff 标签（并存）
    await s.openInBrowserTab('out/report.pdf', { activate: true }) // 预览 → 视图池出现一个页面
  })
  const firstPool = await page.evaluate(() => window.__pool.map((t) => t.id))
  check('预览建出一个视图（前置条件，否则下面的 web 标签断言是空转）', firstPool.length === 1, JSON.stringify(firstPool))
  await nav(firstPool[0]) // 主进程推导航事件 → syncWebTabs 建出 web 标签
  await page.evaluate(() => window.__store.getState().flushSessionTabs())
  const raw = await stored()
  const payload = JSON.parse(raw)
  const scope = payload.scopes.S1
  check('写入键为 go-code.sessionTabs 且带版本号', payload.v === 1 && !!scope, raw)
  check('标签集与顺序落盘（activity,file,file,web）',
    scope.tabs.map((t) => t.kind).join(',') === 'activity,file,file,web', JSON.stringify(scope.tabs))
  check('file 标签的 diff / source 模式各自落盘',
    scope.tabs.filter((t) => t.kind === 'file').map((t) => t.mode).sort().join(',') === 'diff,source',
    JSON.stringify(scope.tabs.filter((t) => t.kind === 'file')))
  check('web 标签存 url 快照（而不是运行时 viewId）',
    scope.tabs.some((t) => t.kind === 'web' && t.url === 'file:///w/out/report.pdf' && !('viewId' in t)),
    JSON.stringify(scope.tabs.find((t) => t.kind === 'web')))
  check('激活标签一并落盘（此刻是 web 标签）',
    scope.tabs.find((t) => t.id === scope.activeId)?.kind === 'web', JSON.stringify({ activeId: scope.activeId }))

  // —— P2/P3：重启（reload）——
  console.log('P2 重启后：标签集与激活标签恢复，且**不展开右栏、不收左栏**')
  await load() // 不带 seed：保留 P1 写入的内容，模拟「关掉应用再打开」
  const after = await snap()
  check('标签集与顺序恢复（activity,file,file,web）', after.kinds === 'activity,file,file,web', JSON.stringify(after.kinds))
  check('file 标签的 diff/source 模式恢复', after.modes === 'a.ts:source,a.ts:diff', JSON.stringify(after.modes))
  check('激活标签恢复（web，不是回落 activity）', after.activeKind === 'web', JSON.stringify({ activeKind: after.activeKind, tab: after.tab }))
  check('rightTab 跟随恢复的激活标签（内容区与标签条一致）', after.tab === 'web', JSON.stringify(after.tab))
  check('**没有**展开右栏（恢复 ≠ 激活）', after.expanded === false, JSON.stringify(after.expanded))
  check('**没有**收起左栏', after.left === true, JSON.stringify(after.left))
  check('重启后视图池为空（恢复不 eager 创建 WebContentsView）', after.pool.length === 0, JSON.stringify(after.pool))
  check('恢复的 web 标签是**待重建**态（没有池身份）', after.webView === '(pending)', JSON.stringify(after.webView))
  console.log('P3 重启后主进程推来的空 targets 不得删掉恢复的 web 标签')
  await page.evaluate(() => window.__navCb({ id: '', url: '', title: '', loading: false, sid: 'S1', visible: false, srcPath: '', canGoBack: false, canGoForward: false, activeId: null, targets: [] }))
  const afterNav = await snap()
  check('空 targets 后 web 标签仍在', afterNav.kinds === 'activity,file,file,web', JSON.stringify(afterNav.kinds))
  check('且仍是待重建态（没被当成「页面已关闭」）', afterNav.webView === '(pending)', JSON.stringify(afterNav.webView))
  check('仍未创建任何视图', afterNav.pool.length === 0, JSON.stringify(afterNav.pool))

  // —— P4：用户要看它了 → 这一刻才建视图，且接回原标签 ——
  console.log('P4 展开面板（用户要看恢复的网页标签）→ 按需重建视图')
  const idsBefore = (await snap()).tabIds
  await page.locator('.right.collapsed .rbtn[aria-label]').first().click() // 展开钮
  await page.waitForTimeout(400)
  const created = await page.evaluate(() => window.__pool.map((t) => ({ id: t.id, url: t.url, srcPath: t.srcPath })))
  check('用户展开面板后建出**恰好一个**视图', created.length === 1, JSON.stringify(created))
  check('视图 url = 存储里的快照（不是空的 about:blank）', created[0]?.url === 'file:///w/out/report.pdf', JSON.stringify(created))
  check('视图带源文件路径（office 预览重建的依据）', created[0]?.srcPath === '/w/out/report.pdf', JSON.stringify(created))
  await nav(created[0].id) // 主进程推导航事件（真实链路：页面加载完 → embed-nav）
  await page.waitForTimeout(200)
  const claimed = await snap()
  check('标签**接回原来那个实例**（不新增重复标签）', claimed.tabIds === idsBefore, `${idsBefore} → ${claimed.tabIds}`)
  check('标签变成活标签（data-view = 新视图 id，不再是待重建态）',
    claimed.rows.filter((r) => r.kind === 'web').map((r) => `${r.view}:${r.pending ? 'pending' : 'live'}`).join(',') === `${created[0].id}:live`,
    JSON.stringify(claimed.rows.filter((r) => r.kind === 'web')))
  check('标签名用快照 url 还原（report.pdf）',
    claimed.rows.filter((r) => r.kind === 'web').map((r) => r.title).join(',') === 'report.pdf',
    JSON.stringify(claimed.rows.filter((r) => r.kind === 'web')))
  check('web 标签数仍为 1（重建没有多开）', claimed.kinds.split(',').filter((k) => k === 'web').length === 1, JSON.stringify(claimed.kinds))
  // 切走再切回（活标签的正常路径）→ 走 embedActivate，不再新建视图
  await page.locator('.rtabs .rtab[data-kind="file"]').first().click()
  await page.waitForTimeout(150)
  await page.locator('.rtabs .rtab[data-kind="web"]').first().click()
  await page.waitForTimeout(200)
  const again = await snap()
  check('活标签来回切 → 只 embedActivate，不新建视图',
    again.pool.length === 1 && again.activateCalls.includes(created[0].id), JSON.stringify({ pool: again.pool.length, calls: again.activateCalls }))

  // —— P5：http(s) 网页（没有 srcPath）走 url 快照重建 ——
  console.log('P5 http(s) 网页（无源文件）按 url 快照重建')
  await load(JSON.stringify({ v: 1, scopes: { S1: { tabs: [{ id: 'tab1', kind: 'web', url: 'https://example.com/a' }], activeId: 'tab1' } } }))
  const httpBefore = await snap()
  check('http 标签恢复为待重建态且视图池为空', httpBefore.webView === '(pending)' && httpBefore.pool.length === 0, JSON.stringify(httpBefore))
  await page.evaluate(() => window.__store.setState({ rightExpanded: true })) // 用户展开面板
  await page.waitForTimeout(400)
  const httpPool = await page.evaluate(() => window.__pool.map((t) => ({ id: t.id, url: t.url })))
  check('按 url 快照建出视图', httpPool.length === 1 && httpPool[0].url === 'https://example.com/a', JSON.stringify(httpPool))
  await nav(httpPool[0].id)
  const httpAfter = await snap()
  check('接回原标签（活）', httpAfter.webView === httpPool[0].id && httpAfter.kinds === 'web', JSON.stringify({ webView: httpAfter.webView, kinds: httpAfter.kinds }))

  // —— P6：坏数据不炸 ——
  console.log('P6 坏数据（截断 JSON）→ 静默丢弃、正常启动')
  errs.length = 0
  await load('{"v":1,"scopes":{"S1":{"tabs":[{"id":"a","ki')
  const dirty = await snap()
  check('截断的 JSON → 不炸（无页面错误）', errs.length === 0, errs.join('; '))
  check('脏数据被静默丢弃 → 回落默认标签集（面板可用）', dirty.kinds === 'activity', JSON.stringify(dirty.kinds))
  check('折叠态 rail 正常渲染（面板没坏）', dirty.railVisible === true, JSON.stringify(dirty.railVisible))

  // —— P7：跨 scope 不串 ——
  console.log('P7 标签集按 scope 隔离（A 的标签不出现在 B）')
  await load(JSON.stringify({ v: 1, scopes: {
    S1: { tabs: [{ id: 'a1', kind: 'git' }], activeId: 'a1' },
    S2: { tabs: [{ id: 'b1', kind: 'web', url: 'https://b.example' }], activeId: 'b1' },
  } }))
  const s1 = await snap()
  check('S1 只拿到自己的标签集（git）', s1.kinds === 'git' && s1.tab === 'git', JSON.stringify({ kinds: s1.kinds, tab: s1.tab }))
  // 切会话走**真实动作**（setActiveSession：未水合会话会 resetView 回落默认 rightTab ——
  // 恢复出来的标签集恰恰常常没有 activity 标签，正是「切走再切回会不会被默认值顶掉」的考场）
  await page.evaluate(() => window.__store.getState().setActiveSession('S2', '/w'))
  await page.waitForTimeout(300)
  const s2 = await snap()
  check('切到 S2 → 拿到 S2 自己的标签集（web）', s2.kinds === 'web' && s2.tab === 'web', JSON.stringify({ kinds: s2.kinds, tab: s2.tab }))
  await page.evaluate(() => window.__store.getState().setActiveSession('S1', '/w'))
  await page.waitForTimeout(300)
  const back = await snap()
  check('切回 S1 → 仍是 S1 自己的（两套互不串，且没被默认 activity 顶掉）', back.kinds === 'git' && back.tab === 'git', JSON.stringify({ kinds: back.kinds, tab: back.tab }))

  check('全程无页面错误', errs.length === 0, errs.join('; '))
  await browser.close()
} finally {
  cleanup()
}

console.log(`\n${fails.length === 0 ? 'ALL PASS' : `FAILED (${fails.length}): ${fails.join('; ')}`}`)
// 成功路径也要显式收尾：spawn 的 vite 子进程会让 Node 事件循环永不空 → 脚本「全绿却永不退出」
//（终端卡住 + 留下僵尸 vite 占端口，见提交 315fdd7）。这里 kill 掉子进程后事件循环自然排空，
// 上面那行汇总已经打印完，不截断。
process.exit(fails.length === 0 ? 0 : 1)
