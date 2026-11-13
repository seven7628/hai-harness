#!/usr/bin/env node
// 面板错误边界 / 连续失败自动禁用 / 墓碑（PR-10）的验证。
//
// 来源：docs/creator-mode-v0.1/03-code-plan.md §2.9「Boundary 与墓碑」、
//       02-pr-plan.md PR-10（SurfaceBoundary / 3 次自动禁用 / unavailable 墓碑）、
//       00-conventions §4.7「测试必须走生产入口」。
//
// 这一轮特别容易假绿的三处，脚本按 §4.7 的口径钉住：
//   · **注册走生产入口**：探针页用 `registerSurface()` 在运行时注册 `plugin:boom` / `plugin:ok`，
//     不是把 provider 直接塞进 RightPanel —— 那样测的是「测试自己造的边界」，不是注册表那条路。
//     N0 自检当场戳穿「注册没生效、测的是兜底分支」（F45 的形态）。
//   · **状态走真 store 动作**：标签集用真 `openTab` 打开（N1），后续夹具只摆 tabsByScope
//     这一层（与 verify_tabbar_interactions 同口径：测试摆状态，不写算法）。
//   · **断言读真 DOM / 真 localStorage**：`.surface-crash` / `[data-unavailable="1"]` /
//     墓碑按钮 / `localStorage['surface.disabled']`。计数算法一条都不在测试里复刻。
//
// 覆盖（每条都能被实现打死）：
//   T1 第 1 次崩溃 → CrashCard；**标签条/收起钮仍在**（边界只包内容区）；切到别的标签正常（隔离）。
//   T2 崩溃后切走再切回 → 重新渲染（boundary 靠 tabId 变化复位错误态，不是永久错误页）。
//   T2b 切标签**不重挂 children**（R06c 原地复用）：provider 组件实例号在 a→b→a 后不变。
//   T3 连续 3 次崩溃 → 该标签变墓碑（data-unavailable="1"）/ getSurface 仍在 / listSurfaces 不含 /
//      localStorage 里有它（kind·count·lastError·at·disabled 五字段）。
//   T3c 禁用内置 kind → ＋ 菜单与空态能力面板都不再列出它；**注销** provider 时菜单仍然列出它
//       （菜单只按「禁用」过滤，不按「是否注册」—— provider 漏注册有 panelFallback 兜底）。
//   T4 连续性：崩 2 次 → 成功渲染一次（同 kind 的另一个标签）→ 再崩 1 次 **不能** 被禁用
//      （证明 recordMountSuccess 真的把 count 归零）。
//   T5 刷新页面（page.reload）后墓碑仍在（读的是 localStorage，不是内存）。
//   T6 点「重新启用」→ provider 回到 listSurfaces、标签恢复正常渲染、localStorage 的 disabled 消失。
//   T7a 干净标签点「移除标签」→ 立刻消失、不弹确认框。
//   T7b dirty 标签点「移除标签」→ 标签仍在 + 弹确认框 + 取消后脏标记还在（closeTabs 语义，不许静默丢内容）。
//
// 用法：cd desktop/app && node e2e/verify_surface_boundary.mjs
// 退出码：全通过 0；任一断言失败 1；前置不满足（端口被占 / 探针服务没起来）2。
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { exitCodeOf, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')

// 端口按口径固定 5226（PR-10），归属自检走 _serve.mjs 的 waitProbePage：
// 撞上一个还没退干净的别人的 vite 时报「前置不满足」并退出 2（04 F46），不许把超时读成代码坏了。
const PORT = 5226
const PROBE_DIR = path.join(__dirname, '_surfaceboundaryprobe_run')
const PROBE_MARKER = '<title>surface-boundary probe</title>'
const PROBE_URL = `http://localhost:${PORT}/e2e/_surfaceboundaryprobe_run/index.html`

const STORAGE_KEY = 'surface.disabled'

// —— 探针页 ——
// 真 store + 真 RightPanel + 真注册表。`plugin:boom` 的 render 按**夹具闸门**决定是否抛错：
//   · tab.params.boom === true 且闸门开 → 每次渲染都抛（崩溃/连续失败用例）
//   · 闸门关（__probe.setBoom(false)）→ 正常渲染（「切走再切回」「重新启用」用例要的就是这个）
// 为什么不是「只抛第 N 次」的计数器：React 18 在 dev 下会把出错的那次 render 再跑一遍
// （收集组件栈），「只抛第一次」会在第二遍成功 —— 于是崩溃根本不发生（实测踩过）。
// 闸门开关是**夹具的输入**，被验证的计数算法一条都不在测试里。
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8">'
  + `<title>surface-boundary probe</title></head>`
  + '<body><div id="root" style="height:100vh"></div>'
  + '<script type="module" src="./main.tsx"></script></body></html>')

fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { useState } from 'react'
import { createRoot } from 'react-dom/client'
import RightPanel from '../../src/components/RightPanel'
// ⚠️ 副作用注册，必须与 src/main.tsx 一致（04-review F45）：少了这行 registry 是空的，
// RightPanel 的 getSurface(kind)?.render(...) 恒走 ?? 兜底 —— N0 会用 __surfaces 当场戳穿。
import '../../src/surfaces/builtin'
import { getSurface, listSurfaces, registerSurface, unregisterSurface } from '../../src/surfaces/registry'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

// health 用**动态** import：红阶段它还不存在（模块 404 → import() 拒绝 → health 保持 null），
// 探针页照样能起来，断言落在「DOM 里没有 .surface-crash / 没有 data-unavailable」这些
// 真实缺口的判据上，而不是「一个模块找不到，整张页面白掉」。
// 用变量拼 specifier + @vite-ignore：写成字面量会被 vite 在 transform 阶段静态解析，
// 缺文件时**整页 500**（红阶段就变成「探针页起不来」，看不到任何一条真断言）。
const HEALTH_URL = '../../src/surfaces/' + 'health.ts'
let health: typeof import('../../src/surfaces/health') | null = null
try { health = await import(/* @vite-ignore */ HEALTH_URL) } catch { health = null }

// 夹具闸门：true = 带 {boom:true} 的标签真的崩（默认）；false = 放行到正常渲染。
//
// 为什么不是「抛 N 次」的计数器：**React 18 在 dev 下会把出错的那次 render 再跑一遍**
// （收集组件栈用），于是「只抛第一次」的写法第二遍就成功了 —— 崩溃根本不会发生，
// 探针会看到一张正常的 .probe-ok（PR-10 实测踩过）。要「这次崩、下次不崩」只能靠显式闸门。
let boomEnabled = true

// 故意在 render 里抛错的插件 provider（运行时注册，走生产入口 registerSurface）。
registerSurface({
  kind: 'plugin:boom',
  namespace: 'plugin',
  group: 'plugin',
  order: 10,
  title: () => 'Boom 面板',
  render: (ctx) => {
    const p = (ctx.tab.params ?? {}) as { boom?: boolean }
    if (boomEnabled && p.boom) throw new Error('boom render 崩溃（' + ctx.tab.id + '）')
    return <div className="probe-ok" data-tab={ctx.tab.id} data-kind="plugin:boom">boom ok</div>
  },
})

// 原地复用探针（R06c）：每**挂载**一次领一个自增实例号 —— useState 的惰性初值只在 mount
// 时执行，所以「切标签后实例号不变」= children 没有被重挂。切 web 标签必须原地复用
//（原生视图与页面状态才保得住），崩溃边界不能靠 key 换隔离把它们一起打死。
function StableProbe({ tab }: { tab: { id: string } }) {
  const [inst] = useState(() => {
    const g = window as unknown as { __inst?: number }
    g.__inst = (g.__inst ?? 0) + 1
    return g.__inst
  })
  return <div className="probe-stable" data-inst={inst} data-tab={tab.id}>stable</div>
}

// 对照组：同一次会话里一个**不崩**的面板（隔离 / 切换 / 连续性都要它）。
registerSurface({
  kind: 'plugin:ok',
  namespace: 'plugin',
  group: 'plugin',
  order: 20,
  title: () => '健康面板',
  render: (ctx) => <div className="probe-ok" data-tab={ctx.tab.id} data-kind="plugin:ok">ok</div>,
})

registerSurface({
  kind: 'plugin:stable',
  namespace: 'plugin',
  group: 'plugin',
  order: 30,
  title: () => '复用面板',
  render: (ctx) => <StableProbe tab={ctx.tab} />,
})

useAppStore.setState({ workspace: '/mock/proj-auth' })
const w = window as unknown as Record<string, unknown>
w.__store = useAppStore
// 只转发生产函数：断言读的是 listSurfaces()/getSurface()/isSurfaceDisabled() 本身。
w.__surfaces = {
  hasBoom: () => !!getSurface('plugin:boom'),
  hasGit: () => !!getSurface('git'),
  has: (k: string) => !!getSurface(k as never),
  list: () => listSurfaces().map((p) => p.kind),
  // 注销也是生产函数：T3c 要用它证明「菜单不因 provider 没注册而变空」。
  unregister: (k: string) => unregisterSurface(k as never),
}
w.__health = health ? {
  isDisabled: (k: string) => health!.isSurfaceDisabled(k as never),
  list: () => health!.listDisabled(),
  enable: (k: string) => health!.enableSurface(k as never),
  disable: (k: string) => health!.disableSurface(k as never),
} : null
w.__probe = {
  setBoom: (v: boolean) => { boomEnabled = v },
  resetInst: () => { (window as unknown as { __inst?: number }).__inst = 0 },
  open: (kind: string, params?: Record<string, unknown>) =>
    useAppStore.getState().openTab({ kind, params } as never),
}
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
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:surface-boundary', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1100, height: 900 } })
  page.setDefaultTimeout(5000)
  // 真 localStorage：上一次运行留下的禁用集/标签集会在模块加载时被读到 —— 但在脚本之前清。
  // **只清一次**（sessionStorage 打标）：T5 要 page.reload() 之后仍然看得见禁用集，
  // 用 addInitScript 每次导航都清会把「持久化」这条证据当场抹掉。
  await page.addInitScript(() => {
    try {
      if (!sessionStorage.getItem('probe:cleared')) {
        localStorage.clear()
        sessionStorage.setItem('probe:cleared', '1')
      }
    } catch { /* file:// 等 */ }
  })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))

  const bootStore = async () => {
    await page.waitForFunction(() => !!window.__store, null, { timeout: 20000 })
    await page.waitForFunction(() => {
      const s = window.__store.getState()
      return s.bridgeStatus === 'running' || (s.sessions ?? []).length > 0
    }, null, { timeout: 20000 }).catch(() => { /* mock 不置 running 也不影响：下面都显式 seed */ })
    await page.waitForTimeout(400)
  }

  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await bootStore()
  const settle = () => page.waitForTimeout(160)

  // 标签夹具：只摆「标签集」这一层（与 verify_tabbar_interactions 同口径）；作用域与展开态走
  // 真实动作（setActiveSession + setRightExpanded），否则会被视图镜像当场冲掉。
  const plugin = (id, kind, params) => ({ id, kind, ...(params ? { params } : {}) })
  const seed = (tabs, activeId, extra = {}) => page.evaluate(([tabs, activeId, extra]) => {
    window.__store.getState().setActiveSession('S1', '/mock/proj-auth')
    window.__store.getState().setRightExpanded(true)
    window.__store.setState({
      tabsByScope: { S1: { tabs, activeId, closed: extra.closed || [] } },
      dirtyTabs: extra.dirty || {},
      closeConfirm: undefined,
    })
  }, [tabs, activeId, extra])
  // 每个用例开局把计数/禁用集清干净（走生产函数 enableSurface，不手写 localStorage 结构）。
  const resetHealth = async () => {
    await page.evaluate(() => {
      if (!window.__health) return // 红阶段：模块还不存在（见探针页里的动态 import）
      window.__health.enable('plugin:boom')
      window.__health.enable('plugin:ok')
      window.__health.enable('git')
    })
    await settle()
  }
  // 真实 DOM + 真实 localStorage 的快照（不在测试里推算任何一条判定）
  const view = () => page.evaluate((key) => {
    const q = (s) => document.querySelector(s)
    const qa = (s) => [...document.querySelectorAll(s)]
    const crash = q('.surface-crash')
    const tomb = q('.surface-tombstone')
    let storage = null
    try { storage = localStorage.getItem(key) } catch { /* 忽略 */ }
    const s = window.__store.getState()
    const sc = s.activeSessionId || s.workspace || ''
    const set = s.tabsByScope[sc] || { tabs: [], activeId: null }
    return {
      crash: crash ? {
        title: (crash.querySelector('.surface-crash-title')?.textContent ?? '').trim(),
        msg: (crash.querySelector('.surface-crash-msg')?.textContent ?? '').trim(),
        hasStack: !!crash.querySelector('details pre'),
        buttons: qa('.surface-crash button').map((b) => (b.className || '').toString()),
        texts: {
          retry: (q('.surface-crash-retry')?.textContent ?? '').trim(),
          disable: (q('.surface-crash-disable')?.textContent ?? '').trim(),
          details: (q('.surface-crash-details > summary')?.textContent ?? '').trim(),
        },
      } : null,
      tomb: tomb ? {
        reason: tomb.getAttribute('data-reason'),
        title: (tomb.querySelector('.surface-tombstone-title')?.textContent ?? '').trim(),
        why: (tomb.querySelector('.surface-tombstone-why')?.textContent ?? '').trim(),
        buttons: qa('.surface-tombstone button').map((b) => (b.className || '').toString()),
        texts: {
          enable: (q('.surface-tombstone-enable')?.textContent ?? '').trim(),
          remove: (q('.surface-tombstone-remove')?.textContent ?? '').trim(),
        },
        text: (tomb.textContent ?? '').replace(/\s+/g, ' ').trim(),
      } : null,
      rtabs: qa('.rtabs .rtab').map((r) => ({
        kind: r.getAttribute('data-kind'),
        on: r.getAttribute('aria-selected') === 'true',
        unavailable: r.getAttribute('data-unavailable'),
        reason: r.getAttribute('data-unavailable-reason'),
        cls: (r.className || '').toString(),
        title: r.getAttribute('title'),
        text: (r.querySelector('.rtab-t')?.textContent ?? '').trim(),
      })),
      hasTabbar: !!q('.rtabs'),
      hasCollapse: !!q('.tab-collapse'),
      addMenuKinds: qa('.rtabs-menu-right .rtabs-mi').map((b) => b.getAttribute('data-kind')),
      emptyAdds: qa('.rtabs-empty-add').map((b) => b.getAttribute('data-kind')),
      dialog: !!q('.editor-confirm'),
      probeOk: qa('.probe-ok').map((x) => x.getAttribute('data-tab')),
      // 原地复用：当前渲染出来的 provider 组件实例号（data-inst）与它服务的标签
      stable: qa('.probe-stable').map((x) => ({ tab: x.getAttribute('data-tab'), inst: Number(x.getAttribute('data-inst')) })),
      dirty: Object.keys(window.__store.getState().dirtyTabs || {}),
      storage,
      disabledList: window.__health ? window.__health.list() : null,
      isDisabled: window.__health ? window.__health.isDisabled('plugin:boom') : null,
      gitDisabled: window.__health ? window.__health.isDisabled('git') : null,
      listKinds: window.__surfaces ? window.__surfaces.list() : null,
      hasBoomProvider: window.__surfaces.hasBoom(),
      hasGitProvider: window.__surfaces.hasGit(),
      ids: set.tabs.map((t) => t.id),
      activeId: set.activeId,
      activeKind: set.tabs.find((t) => t.id === set.activeId)?.kind ?? null,
    }
  }, STORAGE_KEY)
  const click = async (sel, label = sel) => {
    const loc = page.locator(sel).first()
    if ((await loc.count()) === 0) { check(`[缺元素] ${label}`, false, sel); return false }
    await loc.click()
    await settle()
    return true
  }
  // 点标签条上的某个标签（真点击，不调 store 动作）
  const clickTab = async (kind, label) => click(`.rtabs .rtab[data-kind="${kind}"]`, label ?? `标签 ${kind}`)
  // 同 kind 的多个标签只能按下标点（标题/kind 都一样）
  const clickTabAt = async (i, label) => {
    const loc = page.locator('.rtabs .rtab').nth(i)
    if ((await loc.count()) === 0) { check(`[缺元素] ${label}`, false, `index ${i}`); return false }
    await loc.click()
    await settle()
    return true
  }
  const section = async (name, fn) => {
    console.log(name)
    try { await fn() } catch (e) { check(`${name.split(' ')[0]} 执行中断`, false, String(e?.message ?? e)) }
  }
  const parseStorage = (raw) => {
    if (!raw) return null
    try { return JSON.parse(raw) } catch { return null }
  }
  const boom0 = (id, extra = {}) => plugin(id, 'plugin:boom', { boom: true, ...extra })
  const ok = (id) => plugin(id, 'plugin:ok')
  const stable = (id) => plugin(id, 'plugin:stable')

  // —— N0 / N1：先证明探针走的是被测那条路（F45） ——
  console.log('N 探针自检')
  check('N0 两个 plugin provider 真注册上了（否则下面测的是 ??.兜底，不是 provider）',
    await page.evaluate(() => !!window.__surfaces.hasBoom()), 'getSurface("plugin:boom") 为空')
  check('N0 内置 provider 也在（副作用 import 生效）',
    await page.evaluate(() => window.__surfaces.hasGit()), 'getSurface("git") 为空')
  await section('N1 生产动作 openTab 能把 plugin 标签打开', async () => {
    await resetHealth()
    await page.evaluate(() => window.__probe.open('plugin:boom', { boom: false }))
    await settle()
    const v = await view()
    check('N1 openTab({kind:"plugin:boom"}) → 激活标签的 kind 就是 plugin:boom',
      v.activeKind === 'plugin:boom', JSON.stringify({ ids: v.ids, activeId: v.activeId, activeKind: v.activeKind }))
    check('N1 它真的渲染出 provider 的内容（.probe-ok）', v.probeOk.length === 1, JSON.stringify(v.probeOk))
    await seed([], null)
  })

  // —— T1：第 1 次崩溃 → CrashCard；标签条仍在；切走隔离 ——
  await section('T1 第 1 次崩溃 → CrashCard 出现、标签条仍在、切到别的标签正常（隔离）', async () => {
    await resetHealth()
    await page.evaluate(() => window.__probe.setBoom(true))
    await seed([boom0('b1'), ok('o1')], 'b1')
    await settle()
    const v = await view()
    check('T1 render 抛错 → 内容区出现 .surface-crash', !!v.crash, JSON.stringify(v.crash))
    check('T1 CrashCard 的面板名与标签条同源（provider.title = Boom 面板）',
      v.crash?.title === 'Boom 面板', JSON.stringify(v.crash?.title))
    check('T1 CrashCard 带 err.message（不是空白的「出错了」）',
      !!v.crash?.msg.includes('boom render 崩溃'), JSON.stringify(v.crash?.msg))
    check('T1 CrashCard 有可折叠的 stack（details pre）', v.crash?.hasStack === true, JSON.stringify(v.crash?.hasStack))
    check('T1 CrashCard 有两个按钮：重试 / 禁用此面板',
      !!v.crash?.buttons.includes('surface-crash-retry') && !!v.crash?.buttons.includes('surface-crash-disable'),
      JSON.stringify(v.crash?.buttons))
    check('T1 **标签条仍在**（边界只包内容区）', v.hasTabbar && v.rtabs.length === 2, JSON.stringify(v.rtabs.map((r) => r.kind)))
    check('T1 收起按钮仍在（崩一个面板不能让人没法切走/收起）', v.hasCollapse, String(v.hasCollapse))
    await clickTab('plugin:ok')
    const after = await view()
    check('T1 切到一个不崩的标签 → 不再有 CrashCard（错误态按标签隔离）',
      after.crash === null, JSON.stringify(after.crash))
    check('T1 另一个标签自己的内容正常渲染（.probe-ok[data-tab=o1]）',
      after.probeOk.includes('o1'), JSON.stringify(after.probeOk))
  })

  // —— T2：切走再切回 → 重新渲染（boundary 按 tab.id 重置，不是永久错误页） ——
  await section('T2 崩溃后切到另一个标签再切回来 → 重新渲染（不是永久错误页）', async () => {
    await resetHealth()
    await page.evaluate(() => window.__probe.setBoom(true))
    await seed([boom0('b2'), ok('o2')], 'b2')
    await settle()
    check('T2 第一次挂载崩 → CrashCard', !!(await view()).crash)
    await clickTab('plugin:ok')
    // 切走之后把闸门关掉：切回来时「能渲染」是这次切换带来的，不是计时/重试的巧合。
    await page.evaluate(() => window.__probe.setBoom(false))
    await clickTab('plugin:boom')
    const v = await view()
    check('T2 切回该标签 → 重新渲染成功（.probe-ok[data-tab=b2]）',
      v.probeOk.includes('b2'), JSON.stringify({ crash: v.crash, probeOk: v.probeOk }))
    check('T2 切回后没有残留的 CrashCard（错误态跟着标签，不跟着 RightPanel）',
      v.crash === null, JSON.stringify(v.crash))
  })

  // —— T2b：切标签**不重挂 children**（R06c 原地复用）——
  // 判据是**实例号**：StableProbe 的 useState 惰性初值只在 mount 时跑一次，切标签若把
  // children 重挂（例如 boundary 挂 key={tab.id}），实例号会一路变大。
  await section('T2b 切标签原地复用：provider 组件实例不重挂（R06c）', async () => {
    await resetHealth()
    await page.evaluate(() => window.__probe.resetInst())
    await seed([stable('s1'), stable('s2')], 's1')
    await settle()
    const a1 = (await view()).stable[0]
    check('T2b 第一个标签渲染出 provider（实例号 = 1）', a1?.tab === 's1' && a1?.inst === 1, JSON.stringify(a1))
    await clickTabAt(1, '第 2 个标签 s2')
    const b = (await view()).stable[0]
    check('T2b 切到同 kind 的另一个标签 → 内容跟着换（data-tab=s2）', b?.tab === 's2', JSON.stringify(b))
    await clickTabAt(0, '第 1 个标签 s1（切回）')
    const a2 = (await view()).stable[0]
    // 每次运行都把实例号打出来：这条断言的价值全在「数值没变」，只写 ✅ 不写数等于没证据。
    console.log(`  ℹ T2b 实例号：切前 s1=${a1?.inst} → 切到 s2=${b?.inst} → 切回 s1=${a2?.inst}`)
    check('T2b **切走再切回，实例号不变**（children 原地复用，没有重挂）',
      a2?.tab === 's1' && a2?.inst === a1?.inst, JSON.stringify({ 切前: a1, 切到s2: b, 切回: a2 }))
    check('T2b 全程只有一个 provider 组件实例（不是每个标签各一个）',
      b?.inst === a1?.inst, JSON.stringify({ 切前: a1, 切到s2: b }))
  })

  // —— T4（先做：会清空计数，不影响 T3 的「恰好 3 次」）——
  await section('T4 连续性：崩 2 次 → 成功渲染一次 → 再崩 1 次，不能被禁用', async () => {
    await resetHealth()
    await page.evaluate(() => window.__probe.setBoom(true))
    // 同一个 kind 的两个标签：A 每次崩、B 正常 —— 成功渲染 B 必须把 A 连续失败数归零。
    await seed([boom0('b4a'), plugin('b4b', 'plugin:boom', { boom: false }), ok('o4')], 'b4a')
    await settle()
    check('T4 第 1 次崩 → CrashCard', !!(await view()).crash)
    await click('.surface-crash-retry', '重试')
    check('T4 第 2 次崩 → 仍是 CrashCard', !!(await view()).crash)
    // 同一 kind 有两个标签：下标 1 是 B（点击走真 DOM，不调 store 动作）
    await page.evaluate(() => {
      const tabs = [...document.querySelectorAll('.rtabs .rtab')]
      tabs[1]?.click()
    })
    await settle()
    const mid = await view()
    check('T4 切到同 kind 的另一个标签 → 它正常渲染（成功一次）',
      mid.probeOk.includes('b4b') && mid.crash === null, JSON.stringify({ crash: mid.crash, probeOk: mid.probeOk }))
    check('T4 成功渲染把连续失败数归零（localStorage 里 count=0 或没有该记录）',
      (() => { const e = parseStorage(mid.storage)?.['plugin:boom']; return !e || e.count === 0 })(),
      String(mid.storage))
    await page.evaluate(() => {
      const tabs = [...document.querySelectorAll('.rtabs .rtab')]
      tabs[0]?.click()
    })
    await settle()
    const v = await view()
    check('T4 再崩 1 次 → CrashCard（不是墓碑）', !!v.crash, JSON.stringify({ crash: v.crash, tomb: v.tomb }))
    check('T4 **没有被禁用**（证明 recordMountSuccess 真的归零，不是累加）',
      v.isDisabled === false && v.tomb === null, JSON.stringify({ isDisabled: v.isDisabled, tomb: v.tomb }))
    check('T4 标签条上没有 data-unavailable', v.rtabs.every((r) => r.unavailable === null),
      JSON.stringify(v.rtabs.map((r) => r.unavailable)))
  })

  // —— T3：连续 3 次崩溃 → 自动禁用 + 墓碑 ——
  await section('T3 连续 3 次崩溃 → 墓碑（data-unavailable=1 / getSurface 仍在 / listSurfaces 不含 / localStorage）', async () => {
    await resetHealth()
    await page.evaluate(() => window.__probe.setBoom(true))
    await seed([boom0('b3'), ok('o3')], 'b3')
    await settle()
    check('T3 第 1 次崩 → CrashCard', !!(await view()).crash)
    await click('.surface-crash-retry', '重试')
    check('T3 第 2 次崩 → CrashCard', !!(await view()).crash)
    check('T3 崩 2 次还没被禁用', (await view()).isDisabled === false)
    await click('.surface-crash-retry', '重试')
    const v = await view()
    check('T3 第 3 次崩 → 内容区变成墓碑 .surface-tombstone', !!v.tomb, JSON.stringify({ crash: v.crash, tomb: v.tomb }))
    check('T3 墓碑原因为 disabled', v.tomb?.reason === 'disabled', String(v.tomb?.reason))
    check('T3 墓碑写着「连续 3 次崩溃后已自动禁用」',
      !!v.tomb?.why.includes('连续 3 次崩溃后已自动禁用'), String(v.tomb?.why))
    check('T3 墓碑面板名与标签条同源（Boom 面板）', v.tomb?.title === 'Boom 面板', String(v.tomb?.title))
    check('T3 墓碑有两个动作：重新启用 / 移除标签',
      !!v.tomb?.buttons.includes('surface-tombstone-enable') && !!v.tomb?.buttons.includes('surface-tombstone-remove'),
      JSON.stringify(v.tomb?.buttons))
    const tab = v.rtabs.find((r) => r.kind === 'plugin:boom')
    check('T3 标签条上该标签 data-unavailable="1"', tab?.unavailable === '1', JSON.stringify(tab))
    check('T3 该标签是灰态（复用 .rtab.sleeping 那套视觉：unavailable 类）',
      !!(tab?.cls || '').includes('unavailable'), String(tab?.cls))
    check('T3 该标签 title 写明「已禁用」', !!tab?.title && !/Boom 面板/.test(tab.title), String(tab?.title))
    check('T3 getSurface() 仍能拿到 provider（墓碑要靠它拿面板名）', v.hasBoomProvider === true, String(v.hasBoomProvider))
    check('T3 listSurfaces() 不再列出被禁用的 provider',
      !(v.listKinds || []).includes('plugin:boom'), JSON.stringify(v.listKinds))
    check('T3 listDisabled() 里有它', (v.disabledList || []).includes('plugin:boom'), JSON.stringify(v.disabledList))
    const e = parseStorage(v.storage)?.['plugin:boom']
    check('T3 localStorage[surface.disabled] 记录了 kind/count/lastError/at/disabled',
      !!e && e.kind === 'plugin:boom' && e.count === 3 && e.disabled === true
      && typeof e.at === 'number' && e.at > 0 && String(e.lastError).includes('boom'),
      String(v.storage))
    check('T3 标签条仍在（墓碑态也能切走）', v.rtabs.length === 2, JSON.stringify(v.rtabs.length))
    await clickTab('plugin:ok')
    const other = await view()
    check('T3 从墓碑标签切到别的标签仍正常', other.probeOk.includes('o3') && other.tomb === null,
      JSON.stringify({ probeOk: other.probeOk, tomb: other.tomb }))
    await clickTab('plugin:boom')
  })

  // —— T3c：＋ 菜单与空态能力面板要过滤掉被禁用的 kind（内置 kind 也一样） ——
  await section('T3c 禁用内置 git → ＋ 菜单与空态能力面板都不再列出它', async () => {
    await page.evaluate(() => window.__health.disable('git'))
    await settle()
    const v = await view()
    check('T3c isSurfaceDisabled("git") === true（探针与面板走同一份 health）',
      v.gitDisabled === true, String(v.gitDisabled))
    check('T3c listSurfaces() 剔除 git', !(v.listKinds || []).includes('git'), JSON.stringify(v.listKinds))
    check('T3c getSurface("git") 仍在（禁用 ≠ 卸载）', v.hasGitProvider === true, String(v.hasGitProvider))
    await seed([], null) // 空态：没有标签
    await settle()
    const empty = await view()
    // 严格相等（不是「不含 git」）：红阶段这些按钮上还没有被过滤，用 includes 会得到一个
    // 假的绿 —— 判据要能同时证明「顺序仍是 ADDABLE_KINDS」。
    check('T3c 空态能力面板 = activity,web（git 被剔除）',
      empty.emptyAdds.join(',') === 'activity,web', JSON.stringify(empty.emptyAdds))
    await click('.rtabs-add', '＋ 新建标签')
    const menu = await view()
    check('T3c ＋ 菜单 = activity,web（git 被剔除，且菜单项带 data-kind）',
      menu.addMenuKinds.join(',') === 'activity,web', JSON.stringify(menu.addMenuKinds))
    await page.keyboard.press('Escape')
    await settle()
    await page.evaluate(() => window.__health.enable('git'))
    await settle()
    await click('.rtabs-add', '＋ 新建标签（重新启用后）')
    const back = await view()
    check('T3c 重新启用 git → ＋ 菜单 = activity,git,web',
      back.addMenuKinds.join(',') === 'activity,git,web', JSON.stringify(back.addMenuKinds))
    await page.keyboard.press('Escape')
    await settle()
    check('T3c 重新启用后 listSurfaces() 里又有它', ((await view()).listKinds || []).includes('git'))

    // 反向：**注销** provider 不等于「禁用」—— 菜单是静态能力清单，不该因此变空
    //（provider 漏注册时 panelFallback 兜得住，F45；菜单静默少一项反而更难查）。
    await page.evaluate(() => window.__surfaces.unregister('git'))
    await settle()
    const gone = await view()
    check('T3c 注销 git → getSurface("git") 为空（注册表里真的没了）', gone.hasGitProvider === false,
      String(gone.hasGitProvider))
    check('T3c 注销 git → listSurfaces() 不含它', !(gone.listKinds || []).includes('git'), JSON.stringify(gone.listKinds))
    await click('.rtabs-add', '＋ 新建标签（注销后）')
    const afterUnreg = await view()
    check('T3c **注销** git 之后 ＋ 菜单里仍然有它（菜单不按注册表过滤）',
      afterUnreg.addMenuKinds.join(',') === 'activity,git,web', JSON.stringify(afterUnreg.addMenuKinds))
    await page.keyboard.press('Escape')
    await settle()
  })

  // —— T5：刷新后墓碑仍在（读 localStorage，不是内存） ——
  await section('T5 刷新页面后墓碑仍在（localStorage 持久化）', async () => {
    // 上一节结束时 plugin:boom 仍是禁用态（T3 自动禁用的）—— 先确认这个前提。
    const before = await view()
    check('T5 刷新前 plugin:boom 处于禁用态', before.isDisabled === true, String(before.storage))
    await page.reload({ waitUntil: 'domcontentloaded' })
    await bootStore()
    // 刷新后内存里的一切都是新的：唯一还活着的证据就是 localStorage。
    const persisted = await page.evaluate((key) => ({
      raw: localStorage.getItem(key),
      disabled: window.__health.isDisabled('plugin:boom'),
    }), STORAGE_KEY)
    check('T5 刷新后 localStorage 里仍有禁用记录', !!parseStorage(persisted.raw)?.['plugin:boom'], String(persisted.raw))
    check('T5 刷新后 isSurfaceDisabled() 仍为 true（从 localStorage 读回来）', persisted.disabled === true, String(persisted.disabled))
    await seed([boom0('b5'), ok('o5')], 'b5')
    await settle()
    const v = await view()
    check('T5 刷新后打开同一个标签 → 直接是墓碑（没有再崩一次才禁）', !!v.tomb && v.crash === null,
      JSON.stringify({ tomb: v.tomb, crash: v.crash }))
    check('T5 刷新后标签条上仍有 data-unavailable="1"',
      v.rtabs.find((r) => r.kind === 'plugin:boom')?.unavailable === '1',
      JSON.stringify(v.rtabs.map((r) => [r.kind, r.unavailable])))
  })

  // —— T6：重新启用 ——
  await section('T6 点「重新启用」→ provider 回到 listSurfaces、标签恢复正常渲染、localStorage 清掉', async () => {
    // 夹具闸门关掉：probe 要证明「重新启用后 provider 真的能渲染」，不要让夹具再崩一次。
    await page.evaluate(() => window.__probe.setBoom(false))
    await click('.surface-tombstone-enable', '重新启用')
    const v = await view()
    check('T6 墓碑消失，provider 正常渲染（.probe-ok[data-tab=b5]）',
      v.tomb === null && v.probeOk.includes('b5'), JSON.stringify({ tomb: v.tomb, probeOk: v.probeOk }))
    check('T6 provider 回到 listSurfaces()', (v.listKinds || []).includes('plugin:boom'), JSON.stringify(v.listKinds))
    check('T6 listDisabled() 里没有它了', !(v.disabledList || []).includes('plugin:boom'), JSON.stringify(v.disabledList))
    const e = parseStorage(v.storage)?.['plugin:boom']
    check('T6 localStorage 里 disabled 标记消失', !e || e.disabled !== true, String(v.storage))
    check('T6 标签条上的 data-unavailable 消失',
      (v.rtabs.find((r) => r.kind === 'plugin:boom')?.unavailable ?? null) === null,
      JSON.stringify(v.rtabs.map((r) => [r.kind, r.unavailable])))
  })

  // —— T7：移除标签（墓碑不是编辑器：不走 dirty 确认框） ——
  // —— T7a：干净标签 → 点了就消失、不弹确认框（交互洁癖可以满足的那一半）——
  await section('T7a 干净标签：点「移除标签」→ 立刻消失、不弹确认框', async () => {
    // 手动禁用（不是崩溃路径）→ 墓碑的原因文案应当是「已禁用」，不带「连续 3 次」。
    await page.evaluate(() => window.__health.disable('plugin:boom'))
    await seed([boom0('b7'), ok('o7')], 'b7', {})
    await settle()
    const v = await view()
    check('T7a 手动禁用 → 墓碑原因为 disabled', v.tomb?.reason === 'disabled', String(v.tomb?.reason))
    check('T7a 手动禁用的文案不说「连续 3 次」（与自动禁用区分）',
      !!v.tomb && !v.tomb.why.includes('连续 3 次'), String(v.tomb?.why))
    check('T7a 移除前标签集里有 b7', v.ids.includes('b7'), JSON.stringify(v.ids))
    await click('.surface-tombstone-remove', '移除标签')
    const after = await view()
    check('T7a 点「移除标签」→ 该标签从标签条与标签集里消失',
      !after.ids.includes('b7') && !after.rtabs.some((r) => r.kind === 'plugin:boom'),
      JSON.stringify({ ids: after.ids, rtabs: after.rtabs.map((r) => r.kind) }))
    check('T7a 干净标签**没有弹确认框**', after.dialog === false, String(after.dialog))
    check('T7a 顺延到右邻居（剩下的标签仍正常渲染）',
      after.activeId === 'o7' && after.probeOk.includes('o7'),
      JSON.stringify({ activeId: after.activeId, probeOk: after.probeOk }))
  })

  // —— T7b：dirty 标签 → 数据安全优先：标签仍在 + 确认框出现 + 取消后脏标记还在 ——
  // 墓碑的「移除标签」走 store.closeTabs([id])：干净的立刻关，脏的进确认队列。
  // 用 closeTabNow 会**静默丢掉未保存内容** —— 这条断言就是拦它的。
  await section('T7b dirty 标签：点「移除标签」→ 标签仍在 + 弹确认框（不静默丢内容）', async () => {
    await page.evaluate(() => window.__health.disable('plugin:boom'))
    await seed([boom0('b7d'), ok('o7d')], 'b7d', { dirty: { b7d: true } })
    await settle()
    check('T7b 移除前是 dirty 标签', (await view()).dirty.includes('b7d'), 'dirtyTabs 里没有 b7d')
    await click('.surface-tombstone-remove', '移除标签（dirty）')
    const v = await view()
    check('T7b dirty 标签**没有被静默关掉**', v.ids.includes('b7d'), JSON.stringify(v.ids))
    check('T7b 弹出了确认框', v.dialog === true, String(v.dialog))
    // 确认框第一个按钮是「取消」（EditorConfirm 的 autoFocus）
    await click('.editor-confirm-acts .editor-btn', '取消（确认框第一个按钮）')
    const after = await view()
    check('T7b 点取消后标签仍在、也没被降级成干净的', after.ids.includes('b7d') && after.dirty.includes('b7d'),
      JSON.stringify({ ids: after.ids, dirty: after.dirty }))
    check('T7b 取消后确认框关掉', after.dialog === false, String(after.dialog))
    check('T7b 取消后墓碑仍在（面板仍然是禁用态）', after.tomb?.reason === 'disabled', String(after.tomb?.reason))
  })

  // —— T8：i18n —— 崩溃卡/墓碑在 en 下必须全英文（缺 key 会静默回退 zh） ——
  await section('T8 en 下崩溃卡 / 墓碑不蹦中文', async () => {
    await resetHealth()
    await page.evaluate(() => window.__probe.setBoom(true))
    await seed([boom0('b8'), ok('o8')], 'b8')
    await settle()
    await page.evaluate(() => window.__store.setState((s) => ({ settings: { ...s.settings, lang: 'en' } })))
    await settle()
    const en = await view()
    check('T8 en 下崩溃卡按钮是英文（Retry / Disable this panel）',
      en.crash?.texts.retry === 'Retry' && en.crash?.texts.disable === 'Disable this panel',
      JSON.stringify(en.crash?.texts))
    // 只看**组件自己产生**的文案（按钮 + details 的 summary）：err.message 是夹具写的、
    // 面板名来自 provider.title，那两处本来就不该被 i18n 管。
    check('T8 en 下崩溃卡自带的文案没有中文（CJK 一律不许出现）',
      !/[\u4e00-\u9fff]/.test(`${en.crash?.texts.retry ?? ''}${en.crash?.texts.disable ?? ''}${en.crash?.texts.details ?? ''}`),
      JSON.stringify(en.crash?.texts))
    check('T8 en 下「错误详情」是英文', en.crash?.texts.details === 'Error details', JSON.stringify(en.crash?.texts.details))
    await click('.surface-crash-disable', '禁用此面板（en）')
    const tomb = await view()
    // 同样：title 是 provider 给的面板名，不归 i18n 管；只看原因文案。
    check('T8 en 下墓碑原因是英文且没有中文',
      !!tomb.tomb && !/[\u4e00-\u9fff]/.test(tomb.tomb.why) && /disabled/i.test(tomb.tomb.why),
      JSON.stringify(tomb.tomb?.why))
    check('T8 en 下墓碑按钮是英文（Re-enable / Remove tab）',
      tomb.tomb?.texts.enable === 'Re-enable' && tomb.tomb?.texts.remove === 'Remove tab',
      JSON.stringify(tomb.tomb?.texts))
    // 复位：后面没有用例了，但别把语言留给下一个读页面的人
    await page.evaluate(() => window.__store.setState((s) => ({ settings: { ...s.settings, lang: 'zh' } })))
    await settle()
    await page.evaluate(() => window.__health.enable('plugin:boom'))
    await settle()
  })

  // 意外页面错误：崩溃是被 ErrorBoundary 接住的，不该冒泡成 pageerror（夹具的违规崩不算）
  const unexpected = errs.filter((m) => !/boom render 崩溃/.test(m))
  check('E5 全程没有意外的页面错误（边界接住的错误不冒泡）', unexpected.length === 0, unexpected.slice(0, 3).join(' | '))

  const passed = fails.length === 0
  console.log(`\n${passed ? 'SURFACE_BOUNDARY_PROBE_PASS' : 'SURFACE_BOUNDARY_PROBE_FAIL'}（失败 ${fails.length} 项）`)
  code = passed ? 0 : 1
} catch (e) {
  code = exitCodeOf(e)
  console.error(`\n${isPrecondition(e) ? '前置不满足' : '执行中断'}：${e instanceof Error ? e.message : String(e)}`)
}
process.exit(code)
