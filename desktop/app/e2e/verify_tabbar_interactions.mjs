#!/usr/bin/env node
// 面板级标签条的**交互**验证（PR-09：+N 溢出 / 右键 关闭全部·重开 / ⌃⇥ / 空态能力面板 / 中键关闭）。
//
// 来源：docs/creator-mode-v0.1/03-code-plan.md §2.8「多标签只需三块」（溢出、右键五项、⌃⇥），
// 以及 00-conventions §4.7「测试必须走生产入口」——本脚本的所有断言都读**真组件渲染出来的 DOM**
// 与**真 store 的 action 结果**：
//   · 探针页挂真 RightPanel + 真 store（不是复刻的 JSX，也不在测试里重算槽位/回绕/菜单项）；
//   · 点击走真 DOM（page.click / page.mouse.click），键盘走真 KeyboardEvent（page.keyboard）；
//   · 中键、右键、✕ 都是浏览器层面派发的事件，测试不直接调 store 动作去「代表」它们。
//
// 覆盖（每条都是「实现错了就红」的判据）：
//   A  +N 溢出：≤5 全渲染且无 +N；>5 只渲染 5 槽 + +N（N=隐藏数）；激活项在窗口外时挤进最后一槽
//      （且 tabs 数组顺序不变）；点隐藏项 = activateTab 且关闭；Escape / 点别处关闭；
//      键盘可达（Tab 聚焦 → Enter/Space 打开 → ↑↓ 移焦点 → Enter 激活 → Escape 归还焦点）；
//      20 标签只渲染 5 个槽（结构性「不卡」，不做时间断言）。
//   B  右键菜单补 `关闭全部标签` / `重开`：前者对 dirty 走确认框（不静默丢内容），后者栈空时
//      disabled、非空时真重开并激活；两项都遵守「点完关闭菜单」。
//   C  ⌃⇥ / ⌃⇧⇥ 在 tabs 数组里前后移动 activeId 并**回绕**；preventDefault；⌃⌘⇥ 不接管；
//      输入框聚焦时同样生效（全局切换，不是文本编辑）。
//   D  空态能力面板：保留原标题/提示，列出 ADDABLE_KINDS 每一项，点击 = openTab。
//   E  中键关闭：走与 ✕ 相同的路径（dirty 有守卫），且**不会**把被点的标签变成激活标签。
//
// 用法：cd desktop/app && node e2e/verify_tabbar_interactions.mjs
// 退出码：全通过 0；任一断言失败 1；前置不满足（端口被占 / 探针服务没起来）2。
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { exitCodeOf, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')

// 端口按口径固定 5225（PR-09），归属自检走 _serve.mjs 的 waitProbePage：撞上一个还没退干净的
// 别人的 vite 时必须报「前置不满足」并退出 2 —— 不许连上去把超时读成「代码坏了」（04 F46）。
const PORT = 5225
const PROBE_DIR = path.join(__dirname, '_tabbarprobe_run')
const PROBE_MARKER = '<title>tabbar-interactions probe</title>'
const PROBE_URL = `http://localhost:${PORT}/e2e/_tabbarprobe_run/index.html`

// 探针页：真 store + 真 RightPanel + **mock transport**（src/transport/mock.ts：无 window.desktop
// 时 getTransport() 自动选它）。这里刻意不造假的 file_preview 响应：文件标签的内容由生产代码
// 自己的 mock 通道给，探针只负责把标签集摆到要测的形态。
// `#probe-input` 是**测试脚手架**（不是被测能力）：用来验证「输入框聚焦时 ⌃⇥ 仍然生效」。
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8">'
  + `<title>tabbar-interactions probe</title></head>`
  + '<body><div id="root" style="height:100vh"></div>'
  + '<input id="probe-input" aria-label="probe input" style="position:fixed;left:8px;bottom:8px;width:120px" />'
  + '<script type="module" src="./main.tsx"></script></body></html>')

fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import RightPanel from '../../src/components/RightPanel'
// ⚠️ 副作用注册，必须与 src/main.tsx 一致（04-review F45）：少了这行 registry 是空的，
// RightPanel 的 getSurface(kind)?.render(...) 恒走 ?? 兜底 —— 断言证明的就不是 provider 了。
// 下面 N0 自检会用 __surfaces 当场戳穿这件事。
import '../../src/surfaces/builtin'
import { getSurface } from '../../src/surfaces/registry'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

useAppStore.setState({ workspace: '/mock/proj-auth' })
;(window as unknown as { __store: unknown }).__store = useAppStore
;(window as unknown as { __surfaces: unknown }).__surfaces = {
  file: !!getSurface('file'), git: !!getSurface('git'), web: !!getSurface('web'), activity: !!getSurface('activity'),
}
// 启动 store（mock transport）：文件标签的 file_preview 由它应答，面板才有真实内容可画。
void useAppStore.getState().start('/mock/proj-auth')

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

let code = 0
try {
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:tabbar-interactions', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1100, height: 900 } })
  // 断言失败要立刻看得见（默认 30s 会把一次红拖成两分钟，且失败点被埋在执行中断里）。
  page.setDefaultTimeout(5000)
  // 真 localStorage：上一次运行留下的标签集会在 store 模块加载时被恢复 —— 在脚本之前清掉。
  await page.addInitScript(() => { try { localStorage.clear() } catch { /* file:// 等 */ } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))

  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!window.__store, null, { timeout: 20000 })
  // 等 mock 的 start() 落定（会话/设置水合），再开始 seed —— 否则它可能把我们的
  // activeSessionId 改回 mock 会话，断言就会打在另一个 scope 上。
  await page.waitForFunction(() => {
    const s = window.__store.getState()
    return s.bridgeStatus === 'running' || (s.sessions ?? []).length > 0
  }, null, { timeout: 20000 }).catch(() => { /* mock 不置 running 也不影响：下面都显式 seed */ })
  await page.waitForTimeout(500)

  // —— 夹具：标签实例就是生产里的 TabInstance 形状（测试只摆状态，不写算法）——
  const tab = {
    activity: (id) => ({ id, kind: 'activity' }),
    git: (id) => ({ id, kind: 'git' }),
    web: (id, url) => ({ id, kind: 'web', url }),
    file: (id, name, mode = 'source') => ({ id, kind: 'file', path: `/mock/proj-auth/${name}`, mode }),
  }
  // 夹具只摆「标签集」这一层；作用域与面板展开态走**真实动作**（setActiveSession +
  // setRightExpanded）—— 直接写 rightExpanded 会被视图镜像（rightTabPatch → setView）当场
  // 冲掉，那样测到的是「折叠态下没有标签条」，不是标签条行为。
  const seed = (tabs, activeId, extra = {}) => page.evaluate(([tabs, activeId, extra]) => {
    window.__store.getState().setActiveSession('S1', '/mock/proj-auth')
    window.__store.getState().setRightExpanded(true)
    window.__store.setState({
      tabsByScope: { S1: { tabs, activeId, closed: extra.closed || [] } },
      dirtyTabs: extra.dirty || {},
      closeConfirm: undefined,
    })
  }, [tabs, activeId, extra])
  const settle = () => page.waitForTimeout(160)

  // 标签条的 DOM 事实（全部从真实渲染结果读）
  const bar = () => page.evaluate(() => {
    const q = (s) => document.querySelector(s)
    const qa = (s) => [...document.querySelectorAll(s)]
    const ov = q('.rtabs-overflow')
    return {
      rtabCount: qa('.rtabs .rtab').length,
      rows: qa('.rtabs .rtab').map((r) => ({
        kind: r.getAttribute('data-kind'),
        mode: r.getAttribute('data-mode'),
        view: r.getAttribute('data-view'),
        sleeping: r.getAttribute('data-sleeping'),
        pending: r.getAttribute('data-pending'),
        restoring: r.getAttribute('data-restoring'),
        on: r.getAttribute('aria-selected') === 'true',
        title: (r.querySelector('.rtab-t')?.textContent ?? '').trim(),
      })),
      hasOverflow: !!ov,
      overflowTag: ov ? ov.tagName : '',
      overflowN: (q('.rtabs-overflow-n')?.textContent ?? '').trim(),
      overflowExpanded: ov ? ov.getAttribute('aria-expanded') : null,
      overflowMenu: !!q('.rtabs-overflow-menu'),
      // 锚点几何：菜单必须贴在自己按钮的**下方右缘**（.rtabs-overflow-wrap 是定位基准）。
      // 光有「菜单出现了」不够 —— 把 .rtabs-overflow-menu 的定位规则放到 .rtabs-menu 之前，
      // 菜单照样出现，但会从按钮右侧 8px 处开始（被 .rtabs-menu 的 left:8px/top:40px 覆盖）。
      overflowRect: (() => {
        const menu = q('.rtabs-overflow-menu')
        if (!ov || !menu) return null
        const a = ov.getBoundingClientRect()
        const b = menu.getBoundingClientRect()
        return { btnRight: a.right, btnBottom: a.bottom, menuRight: b.right, menuTop: b.top }
      })(),
      overflowMenuRole: q('.rtabs-overflow-menu')?.getAttribute('role') ?? '',
      overflowItems: qa('.rtabs-overflow-item').map((x) => (x.textContent ?? '').trim()),
      overflowItemsRole: qa('.rtabs-overflow-item').map((x) => x.getAttribute('role')),
      emptyTitle: (q('.rtabs-empty .big')?.textContent ?? '').trim(),
      emptyText: (q('.rtabs-empty')?.textContent ?? '').replace(/\s+/g, ' ').trim(),
      emptyAdds: qa('.rtabs-empty-add').map((b) => ({
        kind: b.getAttribute('data-kind'),
        label: (b.textContent ?? '').trim(),
        icon: !!b.querySelector('svg'),
      })),
      ctx: qa('.ctx-menu .ctx-item').map((b) => ({ text: (b.textContent ?? '').trim(), disabled: !!b.disabled })),
      dialog: !!q('.editor-confirm'),
      dialogFile: (q('.editor-confirm .editor-confirm-file')?.textContent ?? '').trim(),
      activeClass: (document.activeElement?.className ?? '').toString(),
      activeTag: document.activeElement?.tagName ?? '',
      activeText: (document.activeElement?.textContent ?? '').trim(),
      activeId: (() => {
        const s = window.__store.getState()
        const sc = s.activeSessionId || s.workspace || ''
        return (s.tabsByScope[sc] || {}).activeId ?? null
      })(),
    }
  })
  const state = () => page.evaluate(() => {
    const s = window.__store.getState()
    const sc = s.activeSessionId || s.workspace || ''
    const set = s.tabsByScope[sc] || { tabs: [], activeId: null, closed: [] }
    return {
      scope: sc,
      ids: set.tabs.map((t) => t.id),
      titles: set.tabs.map((t) => (t.kind === 'file' ? (t.path || '').split('/').pop() : t.kind)),
      kinds: set.tabs.map((t) => t.kind),
      activeId: set.activeId,
      closed: (set.closed || []).map((t) => t.id),
      dirty: { ...s.dirtyTabs },
      closeConfirm: s.closeConfirm ? { reason: s.closeConfirm.reason, ids: s.closeConfirm.tabIds } : null,
    }
  })
  // 真点击（找不到元素不抛：红阶段要能把「缺元素」记成一条失败，而不是中断整个脚本）
  const click = async (sel, label = sel) => {
    const loc = page.locator(sel).first()
    if ((await loc.count()) === 0) { check(`[缺元素] ${label}`, false, sel); return false }
    await loc.click()
    return true
  }
  const clickCtxItem = async (label) => {
    const hit = await page.evaluate((l) => {
      const b = [...document.querySelectorAll('.ctx-menu .ctx-item')].find((x) => (x.textContent ?? '').trim() === l)
      if (!b) return false
      b.click()
      return true
    }, label)
    if (!hit) check(`[缺菜单项] ${label}`, false, (await bar()).ctx.map((x) => x.text).join('|'))
    return hit
  }
  const ctxItem = (b, label) => b.ctx.find((x) => x.text === label)
  // 从当前焦点出发按 Tab，直到落到 +N 按钮（证明它真的在 Tab 序里，而不是只能 script 聚焦）
  const focusOverflowByTab = async (max = 60) => {
    for (let i = 0; i < max; i++) {
      await page.keyboard.press('Tab')
      const hit = await page.evaluate(() => document.activeElement?.classList?.contains('rtabs-overflow') ?? false)
      if (hit) return true
    }
    return false
  }
  const section = async (name, fn) => {
    console.log(name)
    try { await fn() } catch (e) { check(`${name.split(' ')[0]} 执行中断`, false, String(e?.message ?? e)) }
  }

  const seven = [
    tab.activity('a1'), tab.git('a2'), tab.web('a3', 'https://a.example/p'),
    tab.file('a4', 'f4.ts'), tab.file('a5', 'f5.ts'), tab.file('a6', 'f6.ts'), tab.file('a7', 'f7.ts'),
  ]

  check('N0 探针的 surfaces 注册表非空（否则下面验的是 ?? 兜底分支）',
    await page.evaluate(() => {
      const s = window.__surfaces || {}
      return !!(s.file && s.git && s.web && s.activity)
    }), JSON.stringify(await page.evaluate(() => window.__surfaces)))

  await section('A1 5 个标签：全部渲染，没有 +N（TABS_MAX_VISIBLE 是上限而非触发阈值）', async () => {
    await seed([tab.activity('a1'), tab.git('a2'), tab.web('a3', 'https://a.example/p'), tab.file('a4', 'f4.ts'), tab.file('a5', 'f5.ts')], 'a1')
    await settle()
    const b = await bar()
    check('A1 5 个标签 → 渲染 5 个 .rtab', b.rtabCount === 5, `实得 ${b.rtabCount}`)
    check('A1 5 个标签 → 没有 +N 按钮', !b.hasOverflow, `实得 overflow=${b.hasOverflow}`)
    check('A1 可见标签保留 data-kind（activity/git/web/file 齐了）',
      b.rows.map((r) => r.kind).join(',') === 'activity,git,web,file,file', b.rows.map((r) => r.kind).join(','))
    check('A1 可见标签保留 data-mode（file=source）/ data-view（非 web 为空串）',
      b.rows[3].mode === 'source' && b.rows[3].view === '' && b.rows[0].mode === null, JSON.stringify(b.rows[3]))
    check('A1 待重建的 web 标签仍带 data-pending / data-sleeping',
      b.rows[2].pending === '1' && b.rows[2].sleeping === '1', JSON.stringify(b.rows[2]))
  })

  await section('A2 7 个标签：5 槽 + +N=2，点隐藏项 = activateTab 并关闭', async () => {
    await seed(seven, 'a1')
    await settle()
    let b = await bar()
    check('A2 7 个标签 → DOM 里只有 5 个 .rtab', b.rtabCount === 5, `实得 ${b.rtabCount}`)
    check('A2 7 个标签 → .rtabs-overflow-n 文本 = 2', b.hasOverflow && b.overflowN === '2', JSON.stringify({ has: b.hasOverflow, n: b.overflowN }))
    check('A2 +N 是 <button>', b.overflowTag === 'BUTTON', b.overflowTag)
    // 标题走生产口径：activity/git 是 kindLabel，file 是文件名，web 是 webSurface 给的页面标题
    //（这里是 url 的 host）。这条断言锁的是「窗口 = 前 5 个」，不是标题文案本身。
    check('A2 可见窗口 = 前 5 个（a1..a5）', b.rows.map((r) => r.title).join(',') === '链路,变更,a.example,f4.ts,f5.ts', b.rows.map((r) => r.title).join(','))
    await click('.rtabs-overflow', '+N 按钮')
    b = await bar()
    check('A2 点 +N → 弹出下拉，role=menu', b.overflowMenu && b.overflowMenuRole === 'menu', JSON.stringify({ menu: b.overflowMenu, role: b.overflowMenuRole }))
    check('A2 下拉锚在按钮正下方（右缘对齐 ±2px、顶边贴按钮底边 ±2px）',
      !!b.overflowRect
      && Math.abs(b.overflowRect.menuRight - b.overflowRect.btnRight) <= 2
      && Math.abs(b.overflowRect.menuTop - b.overflowRect.btnBottom) <= 2,
      JSON.stringify(b.overflowRect))

    check('A2 下拉列出被隐藏的 2 个标签（f6.ts, f7.ts）', b.overflowItems.join(',') === 'f6.ts,f7.ts', b.overflowItems.join(','))
    check('A2 下拉项是 role=menuitem', b.overflowItemsRole.length === 2 && b.overflowItemsRole.every((r) => r === 'menuitem'), b.overflowItemsRole.join(','))
    await click('.rtabs-overflow-item', '下拉第一项')
    b = await bar()
    const s = await state()
    check('A2 点隐藏项 → activateTab(该项)', s.activeId === 'a6', `activeId=${s.activeId}`)
    check('A2 点隐藏项 → 下拉关闭', !b.overflowMenu && b.overflowExpanded === 'false', JSON.stringify({ menu: b.overflowMenu, expanded: b.overflowExpanded }))
    // 点别处关闭（复用 menu 的 backdrop）：先证明真的开着，再证明被关掉
    await click('.rtabs-overflow', '+N 按钮')
    check('A2 重新点 +N → 下拉打开', (await bar()).overflowMenu)
    await page.mouse.click(6, 6)
    b = await bar()
    check('A2 点别处 → 下拉关闭', !b.overflowMenu, JSON.stringify({ menu: b.overflowMenu }))
    // Escape 关闭
    await click('.rtabs-overflow', '+N 按钮')
    check('A2 再点 +N → 下拉打开', (await bar()).overflowMenu)
    await page.keyboard.press('Escape')
    b = await bar()
    check('A2 Escape → 下拉关闭', !b.overflowMenu, JSON.stringify({ menu: b.overflowMenu }))
  })

  await section('A3 激活项落在窗口外：挤进最后一槽，且 tabs 顺序不变', async () => {
    await seed(seven, 'a7')
    await settle()
    let b = await bar()
    let s = await state()
    check('A3 7 个标签仍只渲染 5 个槽位', b.rtabCount === 5, `实得 ${b.rtabCount}`)
    check('A3 激活项（第 7 个）在第 5 槽且 aria-selected=true', b.rows[4].on && b.rows[4].title === 'f7.ts', JSON.stringify(b.rows.map((r) => `${r.title}:${r.on}`)))
    check('A3 只有一个槽 aria-selected', b.rows.filter((r) => r.on).length === 1)
    check('A3 tabs 数组顺序未被改写（还是 a1..a7）', s.ids.join(',') === 'a1,a2,a3,a4,a5,a6,a7', s.ids.join(','))
    check('A3 +N 仍为 2（隐藏数按 tabs 总数算，不因换槽漂移）', b.overflowN === '2', b.overflowN)
    await click('.rtabs-overflow', '+N 按钮')
    b = await bar()
    check('A3 下拉列出被挤出去的 f5.ts / f6.ts', b.overflowItems.join(',') === 'f5.ts,f6.ts', b.overflowItems.join(','))
    await page.keyboard.press('Escape')
  })

  await section('A4 键盘可达：Tab 聚焦 → Enter/Space 打开 → ↑↓ 移动 → Enter 激活 → Escape 归还焦点', async () => {
    await seed(seven, 'a7')
    await settle()
    check('A4 +N 能被 Tab 聚焦', await focusOverflowByTab())
    check('A4 焦点的确是 .rtabs-overflow 且是 button', (await bar()).activeTag === 'BUTTON')
    await page.keyboard.press('Enter')
    let b = await bar()
    check('A4 Enter 打开下拉', b.overflowMenu && b.overflowItems.length === 2, JSON.stringify({ menu: b.overflowMenu, items: b.overflowItems }))
    check('A4 打开后焦点进入菜单项', b.activeClass.includes('rtabs-overflow-item'), b.activeClass + ' / ' + b.activeText)
    await page.keyboard.press('ArrowDown')
    b = await bar()
    check('A4 ↓ 把焦点从第 1 项移到第 2 项（f6.ts）', b.activeClass.includes('rtabs-overflow-item') && b.activeText === 'f6.ts', b.activeClass + ' / ' + b.activeText)
    await page.keyboard.press('ArrowUp')
    b = await bar()
    check('A4 ↑ 回到第 1 项（f5.ts）', b.activeText === 'f5.ts' && b.activeClass.includes('rtabs-overflow-item'), b.activeText)
    await page.keyboard.press('Enter')
    b = await bar()
    const s = await state()
    check('A4 Enter 激活当前项（a5）', s.activeId === 'a5', `activeId=${s.activeId}`)
    check('A4 Enter 后下拉消失', !b.overflowMenu, JSON.stringify({ menu: b.overflowMenu }))
    // Space 打开 → Escape 关闭并归还焦点
    check('A4 关掉菜单后能再次 Tab 到 +N', await focusOverflowByTab())
    await page.keyboard.press('Space')
    b = await bar()
    check('A4 Space 也能打开下拉', b.overflowMenu && b.overflowItems.length === 2, JSON.stringify({ menu: b.overflowMenu, items: b.overflowItems }))
    await page.keyboard.press('Escape')
    b = await bar()
    check('A4 Escape 关闭下拉', !b.overflowMenu, JSON.stringify({ menu: b.overflowMenu }))
    check('A4 Escape 后焦点回到 .rtabs-overflow', b.activeClass.includes('rtabs-overflow'), b.activeClass)
  })

  await section('A5 20 个标签：结构性「不卡」（只渲染 5 槽，隐藏 15）', async () => {
    const twenty = Array.from({ length: 20 }, (_, i) => tab.file(`t${i + 1}`, `f${i + 1}.ts`))
    const t0 = Date.now()
    await seed(twenty, 't1')
    await page.waitForFunction(() => document.querySelectorAll('.rtabs .rtab').length > 0, null, { timeout: 5000 })
    // 只打点、不断言（时间断言在 CI 上必抖）
    console.log(`      · 20 标签首次渲染（seed → 5 槽可见）：${Date.now() - t0}ms`)
    let b = await bar()
    check('A5 20 个标签 → DOM 里只有 5 个 .rtab', b.rtabCount === 5, `实得 ${b.rtabCount}`)
    check('A5 20 个标签 → +N = 15', b.overflowN === '15', b.overflowN)
    await click('.rtabs-overflow', '+N 按钮')
    b = await bar()
    check('A5 下拉里正好 15 项', b.overflowItems.length === 15, `实得 ${b.overflowItems.length}`)
    check('A5 下拉项 = 第 6..20 个标签', b.overflowItems[0] === 'f6.ts' && b.overflowItems[14] === 'f20.ts', `${b.overflowItems[0]} .. ${b.overflowItems[14]}`)
    await page.keyboard.press('Escape')
    await seed(twenty, 't20')
    await settle()
    b = await bar()
    check('A5 激活第 20 个 → 仍只渲染 5 个槽位', b.rtabCount === 5, `实得 ${b.rtabCount}`)
    const on = b.rows.filter((r) => r.on)
    check('A5 激活第 20 个 → 它就在可见槽位里（aria-selected=true）', on.length === 1 && on[0].title === 'f20.ts', JSON.stringify(b.rows.map((r) => `${r.title}:${r.on}`)))
    check('A5 激活第 20 个 → 隐藏数仍是 15', b.overflowN === '15', b.overflowN)
    await click('.rtabs-overflow', '+N 按钮')
    b = await bar()
    check('A5 激活第 20 个 → 下拉仍 15 项（含被挤出的 f5.ts）', b.overflowItems.length === 15 && b.overflowItems.includes('f5.ts'), b.overflowItems.join(','))
    await page.keyboard.press('Escape')
  })

  await section('B1 右键菜单五项（关闭/其他/右侧/全部/重开）', async () => {
    await seed([tab.activity('b1'), tab.file('b2', 'b2.ts'), tab.git('b3')], 'b3')
    await settle()
    await page.locator('.rtabs .rtab').first().click({ button: 'right' })
    const b = await bar()
    check('B1 右键菜单五项按序：关闭 → 关闭其他 → 关闭右侧 → 关闭全部 → 重开',
      b.ctx.map((x) => x.text).join('|') === '关闭|关闭其他标签|关闭右侧所有标签|关闭全部标签|重开', b.ctx.map((x) => x.text).join('|'))
    check('B1 重开栈为空 → 重开 disabled', ctxItem(b, '重开')?.disabled === true, JSON.stringify(ctxItem(b, '重开')))
    check('B1 关闭全部标签 可用', ctxItem(b, '关闭全部标签')?.disabled === false, JSON.stringify(ctxItem(b, '关闭全部标签')))
  })

  await section('B2 关闭全部：干净的直接关，dirty 的进确认框（不静默丢弃）', async () => {
    await seed([tab.activity('c1'), tab.file('c2', 'c2.ts'), tab.git('c3')], 'c3', { dirty: { c2: true } })
    await settle()
    await page.locator('.rtabs .rtab').nth(2).click({ button: 'right' })
    await clickCtxItem('关闭全部标签')
    await settle()
    let b = await bar()
    let s = await state()
    check('B2 干净的两个（c1/c3）立刻关闭', s.ids.join(',') === 'c2', s.ids.join(','))
    check('B2 dirty 的 c2 没被静默丢弃（标签仍在）', s.ids.includes('c2'), s.ids.join(','))
    check('B2 dirty 的 c2 触发既有确认框', b.dialog && s.closeConfirm?.ids.join(',') === 'c2', JSON.stringify({ dialog: b.dialog, confirm: s.closeConfirm }))
    check('B2 确认框显示的文件名 = c2.ts', b.dialogFile === 'c2.ts', b.dialogFile)
    check('B2 点菜单项后菜单关闭', b.ctx.length === 0, b.ctx.map((x) => x.text).join('|'))
    // 取消 → 标签与内容都留着
    await click('.editor-confirm .editor-btn', '取消')
    await settle()
    s = await state()
    check('B2 取消 → 标签仍在、dirty 状态不变', s.ids.join(',') === 'c2' && s.dirty.c2 === true, JSON.stringify({ ids: s.ids, dirty: s.dirty }))
    check('B2 取消 → 确认框关闭', !(await bar()).dialog)
    // 再关一次 → 丢弃 → 真的关掉
    await page.locator('.rtabs .rtab').first().click({ button: 'right' })
    await clickCtxItem('关闭全部标签')
    await settle()
    await page.locator('.editor-confirm .editor-btn').filter({ hasText: '丢弃' }).click()
    await settle()
    s = await state()
    b = await bar()
    check('B2 用户选「丢弃」后才关掉（进重开栈）', s.ids.length === 0 && s.closed.includes('c2'), JSON.stringify({ ids: s.ids, closed: s.closed }))
    check('B2 关光后进入空态', b.rtabCount === 0 && b.emptyTitle === '没有打开的标签', JSON.stringify({ n: b.rtabCount, title: b.emptyTitle }))
  })

  await section('B3 重开：栈空 disabled，栈非空时调用 reopenTab 并激活', async () => {
    await seed([tab.activity('d1'), tab.file('d2', 'd2.ts')], 'd1')
    await settle()
    await page.locator('.rtabs .rtab').first().click({ button: 'right' })
    let b = await bar()
    check('B3 未关过标签 → 重开 disabled', ctxItem(b, '重开')?.disabled === true, JSON.stringify(ctxItem(b, '重开')))
    await page.keyboard.press('Escape')
    // 用 ✕ 关掉第二个标签（真实路径，不是 store 调用）
    await page.locator('.rtabs .rtab').nth(1).locator('.rtab-close').click()
    await settle()
    let s = await state()
    check('B3 ✕ 关掉 d2 → 进重开栈', s.closed.includes('d2') && s.ids.join(',') === 'd1', JSON.stringify({ ids: s.ids, closed: s.closed }))
    await page.locator('.rtabs .rtab').first().click({ button: 'right' })
    b = await bar()
    check('B3 重开栈非空 → 重开可用', ctxItem(b, '重开')?.disabled === false, JSON.stringify(ctxItem(b, '重开')))
    await clickCtxItem('重开')
    await settle()
    s = await state()
    b = await bar()
    check('B3 点重开 → 标签回来了且成为激活标签', s.ids.join(',') === 'd1,d2' && s.activeId === 'd2', JSON.stringify({ ids: s.ids, active: s.activeId }))
    check('B3 重开后标签条渲染 2 个', b.rtabCount === 2, `实得 ${b.rtabCount}`)
    check('B3 点菜单项后菜单关闭', b.ctx.length === 0, b.ctx.map((x) => x.text).join('|'))
  })

  await section('C ⌃⇥ / ⌃⇧⇥：数组内前后移动并回绕', async () => {
    const three = [tab.activity('e1'), tab.file('e2', 'e2.ts'), tab.git('e3')]
    await seed(three, 'e3')
    await settle()
    check('C1 起始 active = 第 3 个（e3）', (await state()).activeId === 'e3')
    await page.keyboard.press('Control+Tab')
    await settle()
    let s = await state()
    let b = await bar()
    check('C1 最后一个 ⌃⇥ → 回绕到第一个（e1）', s.activeId === 'e1', `activeId=${s.activeId}`)
    check('C1 DOM 的激活槽也跟着换（第 1 槽 aria-selected）', b.rows[0].on && b.rows.filter((r) => r.on).length === 1, JSON.stringify(b.rows.map((r) => r.on)))
    await page.keyboard.press('Control+Shift+Tab')
    await settle()
    s = await state()
    check('C2 第一个 ⌃⇧⇥ → 回绕到最后一个（e3）', s.activeId === 'e3', `activeId=${s.activeId}`)
    // 方向：中间的标签前后各一步
    await seed(three, 'e2')
    await settle()
    await page.keyboard.press('Control+Tab')
    await settle()
    s = await state()
    check('C2 中间 ⌃⇥ → 下一个（e3，不是回绕）', s.activeId === 'e3', `activeId=${s.activeId}`)
    await page.keyboard.press('Control+Shift+Tab')
    await settle()
    s = await state()
    check('C2 中间 ⌃⇧⇥ → 上一个（e2）', s.activeId === 'e2', `activeId=${s.activeId}`)
    // 输入框聚焦时也生效
    await page.focus('#probe-input')
    const focused = await page.evaluate(() => document.activeElement?.id ?? '')
    await page.keyboard.press('Control+Tab')
    await settle()
    s = await state()
    check('C3 输入框聚焦时 ⌃⇥ 仍生效（全局切换，不是文本编辑）', focused === 'probe-input' && s.activeId === 'e3', JSON.stringify({ focused, active: s.activeId }))
    // preventDefault（dispatchEvent 返回 false = 有监听器 preventDefault 了）
    const prevented = await page.evaluate(() => window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', ctrlKey: true, bubbles: true, cancelable: true })))
    check('C4 ⌃⇥ 调用了 preventDefault（dispatchEvent 返回 false）', prevented === false, `dispatchEvent=${prevented}`)
    const before = (await state()).activeId
    const notPrevented = await page.evaluate(() => window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', ctrlKey: true, metaKey: true, bubbles: true, cancelable: true })))
    const after = (await state()).activeId
    check('C4 ⌃⌘⇥ 不被接管（!metaKey）：不 preventDefault、也不切标签', notPrevented === true && after === before, JSON.stringify({ notPrevented, before, after }))
  })

  await section('D 空态能力面板', async () => {
    await seed([], null)
    await settle()
    let b = await bar()
    check('D1 空集 → 保留原空态标题', b.emptyTitle === '没有打开的标签', b.emptyTitle)
    check('D1 空集 → 保留原提示文案', b.emptyText.includes('点 ＋ 打开一个标签，或从左侧文件树打开文件。'), b.emptyText)
    check('D2 空态列出 ADDABLE_KINDS 的每一项（3 个按钮）', b.emptyAdds.length === 3, `实得 ${b.emptyAdds.length}`)
    check('D2 三项 = activity / git / web（与 ＋ 菜单同一份 ADDABLE_KINDS）', b.emptyAdds.map((x) => x.kind).join(',') === 'activity,git,web', b.emptyAdds.map((x) => x.kind).join(','))
    check('D2 每项都带图标 + kindLabel 文案', b.emptyAdds.every((x) => x.icon) && b.emptyAdds.map((x) => x.label).join(',') === '链路,变更,网页', JSON.stringify(b.emptyAdds))
    await click('.rtabs-empty-add', '空态第一个按钮')
    await settle()
    b = await bar()
    const s = await state()
    check('D3 点第一项 → openTab({kind})：tabs.length = 1 且已激活', s.ids.length === 1 && s.kinds[0] === 'activity' && s.activeId === s.ids[0], JSON.stringify({ ids: s.ids, kinds: s.kinds, active: s.activeId }))
    check('D3 点后空态消失、标签条渲染 1 个标签', b.rtabCount === 1 && b.emptyAdds.length === 0, JSON.stringify({ tabs: b.rtabCount, adds: b.emptyAdds.length }))
  })

  await section('E 中键关闭（真 page.mouse.click {button:middle}）', async () => {
    await seed([tab.activity('g1'), tab.file('g2', 'g2.ts'), tab.git('g3')], 'g1')
    await settle()
    const box = await page.locator('.rtabs .rtab').nth(1).boundingBox()
    check('E1 能取到第 2 个标签的坐标', !!box, JSON.stringify(box))
    if (box) await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2, { button: 'middle' })
    await settle()
    let b = await bar()
    let s = await state()
    check('E1 中键关闭该标签（集合里没了）', s.ids.join(',') === 'g1,g3', s.ids.join(','))
    check('E1 标签条也不再渲染它', b.rtabCount === 2 && !b.rows.some((r) => r.title === 'g2.ts'), JSON.stringify(b.rows.map((r) => r.title)))
    check('E1 它没有变成激活标签（activeId 仍是 g1）', s.activeId === 'g1', `activeId=${s.activeId}`)
    check('E1 干净标签直接关，不弹确认框', !b.dialog)
    // dirty 守卫：中键与 ✕ 同一条路
    await seed([tab.activity('h1'), tab.file('h2', 'h2.ts'), tab.git('h3')], 'h1', { dirty: { h2: true } })
    await settle()
    const box2 = await page.locator('.rtabs .rtab').nth(1).boundingBox()
    if (box2) await page.mouse.click(box2.x + box2.width / 2, box2.y + box2.height / 2, { button: 'middle' })
    await settle()
    b = await bar()
    s = await state()
    check('E2 中键关 dirty 标签 → 不静默丢弃（标签仍在 + 确认框）', s.ids.includes('h2') && b.dialog, JSON.stringify({ ids: s.ids, dialog: b.dialog }))
    check('E2 确认框指向 h2.ts', b.dialogFile === 'h2.ts', b.dialogFile)
    await click('.editor-confirm .editor-btn', '取消')
  })

  check('全程无页面错误', errs.length === 0, errs.join('; '))
  await browser.close()
} catch (e) {
  // 前置不满足 ≠ 断言失败：退出码 2，且必须把「为什么」打出来（00 §4.6 红归因）。
  if (!isPrecondition(e)) throw e
  console.error(`\n⛔ 前置不满足（退出码 2，不是测试失败）：${e.message}`)
  code = 2
} finally {
  cleanup()
}

if (code === 0) {
  console.log(`\n${fails.length === 0 ? 'ALL PASS' : `FAILED (${fails.length}): ${fails.join('; ')}`}`)
  code = fails.length === 0 ? 0 : exitCodeOf(new Error('assert'))
}
process.exit(code)
