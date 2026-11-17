// 「产出可视文件 → 自动打开右侧栏」行为验证（2026-09-17；阶段 4 语义反转后重写断言）。
//
// 背景：该逻辑（store.autoOpenVisualFile，§6.10）由一次被打断的后台任务实现——
// 代码完整（tsc 通过、artifacts 86 项基线未回退）但**没有任何测试**：
// 它留了 `autoOpenVisualState()` 作为「e2e 断言用」的探针，却无消费方。
// 本脚本补上这层验证，否则「自动打开」是本次改动里唯一未经检验的行为。
//
// 覆盖（对应用户拍板的三档规则 + 实现自带的两个保护）：
//   A1 只对可视类自动打开（文本类**不**打开）
//   A2 后台产出 → **只加/刷新标签 + 未读点**，不切标签、不展开、不收左栏（阶段 4 反转）
//   A3 后台标签加载出真实页面（真跑 onEmbedNav）→ 仍不动面板
//   A4 **用户显式**预览（activate:true）→ 展开 + 切 web（这条必须保持；T1 语义）
//   A5 定位到该文件（fileFocus 指向它）
//   D1 去重：同一文件不重复打开
//   S1 后台会话（非激活会话）的产出不抢当前视图
//   S2/S3 别的会话的页面 / AI 动作 → 当前面板三态**全部不变**（阶段 4 的主战场）
//
// 语义演变（三段历史，别把中间的当结论）：
//   2026-09-17 一改（ADR-2「不抢视线」）→ 2026-09 二改（「产出即展示」）→ 2026-09-20 三改
//  （阶段 4「自动激活收口」，用户明确口径）：
//   · 二改推翻一改的理由见 store §6.10：一改的实现里**没有任何一步**会把内容显示出来 ——
//     面板展开后显示的是当前激活视图（自举占位页 about:blank），用户必须手点标签才看得到
//     （2026-09 用户实测报告）。二改于是改成「展开 + 切 web + 激活那个标签」。
//   · 三改推翻二改的理由：那是**进程级**口径 —— 不管用户在哪个会话、正在看什么，一次产出就
//     把界面切走；用户原话是「切会话/切工作区/新建会话也会自动打开右侧浏览器 Tab 很别扭」。
//     二改要解决的**真问题**（「没有任何一步会把它显示出来」）由**未读点**解决：标签带未读点、
//     折叠态 rail 有角标（A2 断言 railBadge），用户随时看得见「有东西在等我」，但看不看、
//     什么时候看由用户决定。左栏一如既往**不**收（收左栏是 Browser Use「接管视界」的语义）。
// 三件事现在的归属：
//   · **定位**：fileFocus 指向该文件（与面板展不展开无关）
//   · **准备**：后台标签 + 未读点（activate:false）—— 内容随时可看，但不抢视线
//   · **展开/切标签**：只由**用户显式动作**触发（openFile / openInBrowserTab activate:true）
// 「真视图池里新标签确实是 activeId」在 embed 层由 `e2e:container` 钉住 —— 本脚本跑在普通
// Chrome 里、用**假**视图池（见 index.html 的内联注入），只能验到 store 层的
// 激活标签/`rightExpanded`/`leftExpanded`/标签集/未读点。
//
// 用例编号说明：本文件的 A1/A2/… 是**用例**编号，与 ADR 的 R0~R5 轮次无关（旧编号 R1/R2/R3
// 曾与之混淆，2026-09-17 改名）。
//
// 用法：node e2e/verify_auto_open_visual.mjs
import { spawn } from 'child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = 5199

// 端口就绪 + 归属自检（e2e/_serve.mjs 的 waitProbePage）：固定端口 5199 上可能挂着别人
// 的服务（F46 的实测就是这台的 5199）—— 撞上时必须报「前置不满足」并退出 2，不许连上去
// 把 waitForFunction 超时读成「代码坏了」。判据取本脚本探针页自己的 <title>。
const PROBE_URL = `http://localhost:${PORT}/e2e/_autoprobe_run/index.html`
const PROBE_MARKER = '<title>auto-open probe</title>'

// 探针页：真实 store（含 start() 建链）+ 真实 RightPanel（验证「切到文件栏」是真的切了）。
//
// 阶段 4 起探针还注入一个**假的内嵌视图池**（fake host api 的 embed*）与一个可驱动的 nav 事件
// 回调 —— 本阶段的三档规则里有两条（AI 操作浏览器 / 后台产出）只有在真跑 onEmbedNav 的
// 收口逻辑时才验得到。注入点必须在 **store 模块加载之前**（subscribeEmbedEvents 只在模块
// 初始化时订阅一次；模块加载时没有宿主桥 → 永不订阅，nav 断言会静默空转），
// 故放在 index.html 的**内联 script**（经典脚本，先于 module 脚本执行）。
const PROBE_DIR = path.join(__dirname, '_autoprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>auto-open probe</title>'
  + `<script>
// —— 假内嵌视图池（只替掉最外层的 IPC 边界，store 的逻辑全真跑）——
// 为什么要池而不是「返回固定结果」：autoOpenVisualFile 走 activate:false → 未读点由
// rememberPreviewView 用**打开前后的池快照差集**算出（不依赖实现把 id 放在响应哪个字段）。
// 池不记账的话差集恒空 → 未读点断言永远绿 = 空转。
//
// 为什么整个 fake 用 Proxy 兜底：注入 fake（= L0 之前注入 window.desktop）会让 store 选**真实
// electron transport**（src/transport/index.ts 按宿主桥是否存在分流），
// 而真实 transport 会调几十个 preload API —— 缺一个就是 "d.xxx is not a function"
// 把断言打成 TypeError（实测）。Proxy 只替最外层：**被测的 embed 通路逐个显式实现**
//（逻辑全真跑），其余 API 一律返回 { ok: true } 的惰性桩（probe 不验那些）。
window.__pool = []
window.__navCb = null
window.__seq = 0
// 唤醒记账：休眠标签点开后必须用**原 id** 调 embedActivate（主进程按 id 重建 webContents，
// URL 取休眠快照）。不记账就只能断言「点了没报错」，验不到「唤醒的是同一页」。
window.__activateCalls = []
const impl = {
  // —— 视图池：被测通路，必须记账 ——
  embedStatus: async () => ({ ok: true, targets: window.__pool.map((t) => ({ ...t })) }),
  embedOpenTarget: async (url, o) => {
    // id 序号**永不重置**（reset 只清池）：与主进程 't'+自增序号同语义 —— id 不复用，
    // 否则「每个视图只自动展开一次」的记账（embedAutoOpenedViews）会把复用 id 误判成
    // 「已展开过」→ 假红（实测踩过：reset 后新视图又叫 t1）。
    const id = 't' + (++window.__seq)
    window.__pool.push({ id, url, title: '', sid: (o && o.sid) || '', srcPath: (o && o.srcPath) || '' })
    return { ok: true, state: { activeId: (o && o.activate) ? id : null, targets: window.__pool.map((t) => ({ ...t })) } }
  },
  embedActivate: async (id) => { window.__activateCalls.push(id); return { ok: true, state: { activeId: id, targets: window.__pool.map((t) => ({ ...t })) } } },
  embedCloseTarget: async (id) => { window.__pool = window.__pool.filter((t) => t.id !== id); return { ok: true } },
  embedReloadTarget: async () => ({ ok: true }),
  embedNavigateTarget: async () => ({ ok: true }),
  // —— 事件订阅：把回调存起来，probe 用它们驱动真实的 onEmbedNav / onEmbedAiAction ——
  onEmbedNav: (cb) => { window.__navCb = cb; return () => {} },
  onEmbedAiAction: (cb) => { window.__aiCb = cb; return () => {} },
  // 面板组件会调的（缺了 effect 抛错会把整棵 React 树卸载，实测过）
  embedHide: async () => ({ ok: true }),
  embedShow: async () => ({ ok: true }),
  embedResize: async () => ({ ok: true }),
  embedCommand: async () => ({ ok: true }),
  embedDebuggerProxy: async () => ({ ok: false }), // 不起自举循环（probe 不需要 attach）
}
window.desktop = new Proxy(impl, {
  get(t, k) {
    if (k in t) return t[k]
    if (typeof k !== 'string') return undefined // Symbol.toPrimitive 等：别造函数
    if (k.startsWith('on')) return () => () => {} // 事件订阅类：返回退订函数
    return async () => ({ ok: true }) // 其余 transport API：惰性桩
  },
})
</script></head>`
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import RightPanel from '../../src/components/RightPanel'
import { useAppStore } from '../../src/store/useAppStore'
// ⚠️ 副作用注册，必须与 src/main.tsx 一致。R06b 把 web 的 title/icon 委派给 provider 之后，
// 少了这一行 getSurface('web') 恒为 undefined —— 探针证明的会是 RightPanel 的 \`??\` 兜底
// 分支，而不是 provider（实测：标签名退化成 kindLabel 的「网页」，session-tabs 探针因此假红）。
import '../../src/surfaces/builtin'
import { getSurface } from '../../src/surfaces/registry'
import '../../src/index.css'

// —— 仪器自检：provider 的 render 到底有没有被执行 ——
// 「用探针证明没变」的验收必须先证明探针走的是被测那条路（04-review F45）。这里给注册表里
// 那个实例的 render 包一层计数：面板真的走委派 → 计数增长；若 RightPanel 走的是 \`??\` 回退，
// 计数恒为 0，断言会当场变红而不是静默通过。
const webProvider = getSurface('web')
;(window as unknown as { __webRenderCalls: number }).__webRenderCalls = 0
if (webProvider) {
  const orig = webProvider.render
  webProvider.render = (ctx) => {
    ;(window as unknown as { __webRenderCalls: number }).__webRenderCalls++
    return orig(ctx)
  }
}

// 固定工作区，使 relIfInside 的相对化路径可预测（probe 断言依赖它）。
useAppStore.setState({ workspace: '/w' })
;(window as unknown as { __store: unknown }).__store = useAppStore
;(window as unknown as { __webSurface: unknown }).__webSurface = webProvider ?? null
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

let code = 0
try {
  // 端口就绪 + 归属自检：waitPort 只能证明「有人在听」，证明不了「听的是本脚本刚起的那个」。
  // 2026-11 实测踩到过：一个残留在 5199 的 vite（**另一棵 worktree** 的服务）让本脚本连上了
  // 别人的模块图 → 探针页 404 → 一直等不到 __store，报出来的却是 waitForFunction 超时。
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:auto', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1100, height: 900 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!window.__store, null, { timeout: 20000 })
  await page.waitForTimeout(800)

  // 每个用例前复位视图状态，避免用例间互相污染（面板/rightExpanded 是会话态）。
  // leftExpanded 显式置回 true：它是 M4 缺陷的观测点（修复前会被 focusBrowser 静默收起），
  // 每个用例都从「左栏正常展开」出发，才能断言它**没有**被动过。
  // tabsByScope / unreadViews / 假视图池也一并复位 —— 阶段 4 的断言要看「标签集与未读点」。
  //
  // R11b：面板 = 激活标签，所以「复位到链路面板」= seed 一个激活的 activity 标签（而不是写
  // 状态里的面板字段）。直接 seed 而不是等收敛 effect 建默认集，是为了让断言读到的时刻确定。
  const reset = () => page.evaluate(() => {
    window.__pool.length = 0
    window.__activateCalls.length = 0
    const s = window.__store.getState()
    const scope = s.activeSessionId || s.workspace || ''
    window.__store.setState({
      rightExpanded: false, leftExpanded: true, unreadViews: [],
      tabsByScope: { [scope]: { tabs: [{ id: 'act0', kind: 'activity' }], activeId: 'act0', closed: [] } },
    })
  })
  // 快照：面板三态 + 定位锚 + 未读点 + 本 scope 的标签集 + 折叠态 rail 角标。
  const snap = () => page.evaluate(() => {
    const s = window.__store.getState()
    const scope = s.activeSessionId || s.workspace || ''
    const set0 = s.tabsByScope[scope] || {}
    const tabs = set0.tabs || []
    const unreadViews = s.unreadViews || []
    // 按 data-kind 找，不按文案：rail 按钮的 aria-label 就是 tab 的 i18n 文案，
    // 2026-11 把 web 的显示名从「网页」改成「浏览器」后，`aria-label*="网页"` 永不匹配。
    const railBtn = document.querySelector('.right-rail .rbtn[data-kind="web"]')
    return {
      // 面板 = 激活标签的 kind（R11b：状态里那个写穿副本已删）
      tab: ((tabs.find((x) => x.id === set0.activeId) || {}).kind) || '',
      expanded: s.rightExpanded, left: s.leftExpanded,
      focus: (s.fileFocus && s.fileFocus.path) || '',
      unread: unreadViews.length,
      unreadTabs: tabs.filter((x) => x.kind === 'web' && unreadViews.includes(x.viewId)).length,
      tabKinds: tabs.map((x) => x.kind).join(','),
      webIds: tabs.filter((x) => x.kind === 'web').map((x) => x.viewId).join(','),
      activeView: (s.embedNav && s.embedNav.activeId) || '',
      pool: window.__pool.length,
      railBadge: railBtn ? (railBtn.querySelector('.unread-badge') ? railBtn.querySelector('.unread-badge').textContent : '') : 'NO_BTN',
    }
  })
  // 直接调被测函数（它就是两个触发源 tool_run_end / agent_end 最终调用的那个）。
  // 阶段 4 起它是 async（未读点在 IPC 响应后才写进 unreadViews）→ 这里 await 它。
  const fire = (p, sid = 'S1') => page.evaluate(async ([pp, ss]) => {
    const opened = await window.__AUTO(pp, ss)
    const s = window.__store.getState()
    const scope = s.activeSessionId || s.workspace || ''
    const set0 = s.tabsByScope[scope] || {}
    const tabs = set0.tabs || []
    return {
      opened,
      tab: ((tabs.find((x) => x.id === set0.activeId) || {}).kind) || '',
      expanded: s.rightExpanded, left: s.leftExpanded,
      focus: (s.fileFocus && s.fileFocus.path) || '', unread: (s.unreadViews || []).length,
    }
  }, [p, sid])
  // 驱动真实的 onEmbedNav 处理（假池 → 真 store 逻辑）：activeId 指向池里指定视图，
  // 其余页面一并进 targets —— 这正是主进程推过来的形态。
  const nav = (activeId) => page.evaluate(([aid]) => {
    const s = window.__store.getState()
    window.__navCb({
      id: aid, url: (window.__pool.find((t) => t.id === aid) || {}).url || '', title: '', loading: false,
      sid: s.activeSessionId || '', visible: true, srcPath: '',
      canGoBack: false, canGoForward: false, activeId: aid, targets: window.__pool.map((t) => ({ ...t })),
    })
  }, [activeId])

  // 暴露被测函数 + 固定 activeSessionId（后台会话保护断言需要）
  await page.evaluate(() => {
    window.__store.setState({ activeSessionId: 'S1' })
  })
  await page.evaluate(async () => {
    const mod = await import('/src/store/useAppStore.ts')
    window.__AUTO = mod.autoOpenVisualFile
  })

  console.log('A1 只对可视类自动打开（文本类不打开）')
  await reset()
  const txt = await fire('src/main.go')
  check('文本类不自动打开（opened=false）', txt.opened === false, JSON.stringify(txt))
  check('文本类不改视图状态（仍 activity + 未展开）',
    txt.tab === 'activity' && txt.expanded === false, JSON.stringify(txt))
  const md = await fire('README.md')
  check('.md 不自动打开（避免改代码刷屏）', md.opened === false, JSON.stringify(md))

  // —— A2：阶段 4 的**语义反转**（用户 2026-09-20 口径）——
  // 旧断言（§6.10 二次修正「产出即展示」）钉的是「切到 web tab + 展开右栏 + 不标未读」。
  // 现在钉的是**更强**的形态：标签必须存在、必须带未读点，而面板**一动都不能动**
  //（不切标签 / 不展开 / 不收左栏）—— 断言从「有反应」变成「有反应且不打扰」。
  console.log('A2 可视类：只加/刷新后台标签 + 未读点，**不切标签、不展开、不收左栏**')
  await reset()
  const pdf = await fire('out/report.pdf')
  check('pdf 触发自动打开（opened=true）', pdf.opened === true, JSON.stringify(pdf))
  // 先钉「建的是**后台**视图」：此刻还没收到任何 nav 事件，embedNav.activeId 应为空
  //（activate:false 的语义 = 主进程不设 activeId）。随后再驱动 nav 建标签。
  const beforeNav = await snap()
  check('视图池里是后台标签（activeId 未指向它）', beforeNav.activeView === '' && beforeNav.pool === 1, JSON.stringify(beforeNav))
  const bgView = await page.evaluate(() => window.__pool[0].id) // id 序号永不重置 → 动态取
  // 标签是 onEmbedNav 的 syncWebTabs 建的（视图池是真相源）→ 驱动一次 nav（真链路里由主进程推），
  // 再断言标签集 —— 顺序反了会得到「标签还没建」的假红。
  await nav(bgView)
  const pdfS = await snap()
  check('标签存在（本 scope 标签集里多了一个 web 标签）', pdfS.webIds === bgView, JSON.stringify(pdfS))
  check('标签带**未读点**（unreadViews 命中该视图）',
    pdfS.unread === 1 && pdfS.unreadTabs === 1, JSON.stringify(pdfS))
  check('**没有**抢激活（激活标签仍是 activity）', pdfS.tab === 'activity', JSON.stringify(pdfS))
  check('**没有**展开右栏（rightExpanded=false）', pdfS.expanded === false, JSON.stringify(pdfS))
  check('右栏收起态也看得见未读点（rail 角标）', pdfS.railBadge === '1', JSON.stringify(pdfS))
  check('定位到该文件（fileFocus=out/report.pdf）', pdf.focus === 'out/report.pdf', JSON.stringify(pdf))
  // M4 核心断言：预览本地文件**不得**收起左栏（重置为 true，须仍为 true）。
  check('左栏未被收起（leftExpanded 保持 true）', pdf.left === true, JSON.stringify(pdf))

  console.log('A3 后台标签加载出真实页面后：仍不动面板（真跑 onEmbedNav 的收口逻辑）')
  await nav(bgView) // 第二次 nav 模拟后续导航/标题变化
  const afterNav = await snap()
  check('nav 事件后仍停在 activity（不抢激活）', afterNav.tab === 'activity', JSON.stringify(afterNav))
  check('nav 事件后右栏仍收起（不自动展开）', afterNav.expanded === false, JSON.stringify(afterNav))
  check('nav 事件后左栏仍展开（不收左栏）', afterNav.left === true, JSON.stringify(afterNav))
  check('该后台视图仍带未读点（用户还没看过）', afterNav.unreadTabs === 1, JSON.stringify(afterNav))

  console.log('A4 用户显式预览（activate:true）→ 展开 + 切 web（这条**必须**保持，T1 语义）')
  await reset()
  await page.evaluate(() => window.__store.getState().openInBrowserTab('out/user-open.pdf', { activate: true }))
  await page.waitForTimeout(50)
  const userView = await page.evaluate(() => window.__pool[0].id) // 永不重置的 id 序号 → 动态取
  const userOpen = await snap()
  check('用户点击 → 切到内容标签（激活标签=web）', userOpen.tab === 'web', JSON.stringify(userOpen))
  check('用户点击 → 展开右栏', userOpen.expanded === true, JSON.stringify(userOpen))
  check('用户点击 → 左栏未被收起', userOpen.left === true, JSON.stringify(userOpen))
  check('用户点击 → 不标未读（内容已经显示出来了）', userOpen.unread === 0, JSON.stringify(userOpen))
  await nav(userView) // 该视图加载出真实页面（用户显式预览 + 激活 → 走 focusWebTabIfClosed 兜底）
  const userNav = await snap()
  check('显式预览的 nav 事件后仍在内容标签且展开', userNav.tab === 'web' && userNav.expanded === true, JSON.stringify(userNav))
  check('显式预览的 nav 事件后左栏仍展开（不被 focusBrowser 收起）', userNav.left === true, JSON.stringify(userNav))

  // —— A5：休眠标签点开唤醒（R06c 新增）——
  //
  // 为什么这条不能省：A2/A3 验的是「后台标签 + 未读点」，A4 验的是「用户显式预览」，而**休眠
  // 标签的唤醒**是第三条路 —— 渲染层的 `.rtab[data-sleeping]` 被点 → store 的 activateTab →
  // IPC embedActivate(**原 id**)。container 的 T4 类断言直接调 IPC，绕过了标签渲染与
  // activateTab 的判断；而 R06c 恰好把面板从 RightPanel 搬进了 WebTab（provider 渲染），
  // 「点标签 → 面板跟着换」这一跳正是平移后最容易断的地方。所以这里**从 DOM 点**，
  // 断言落在 IPC 入参、标签身份与标签名上。
  //
  // 口径（03 §2.3 不变量 3）：sleep = 销毁 webContents 但**不广播 destroyed**、保留 id 与
  // lastUsed；唤醒用**原 id** 重建、URL 取休眠快照。于是「唤醒后 URL 不丢」在本探针里的
  // 可观测形式 = 标签身份（data-view）不变 + 标签名（由池里的 url 推出的文件名）不变。
  console.log('A5 休眠标签点开 → 走渲染层的 activateTab 唤醒：用**原 id**，标题/身份不变')
  await reset()
  await page.evaluate(() => window.__store.getState().openInBrowserTab('out/sleep-a.pdf', { activate: true }))
  await page.waitForTimeout(50)
  await page.evaluate(() => window.__store.getState().openInBrowserTab('out/sleep-b.pdf', { activate: true }))
  await page.waitForTimeout(50)
  // 让 A 休眠：主进程的休眠口径 = 条目仍在 targets 里、带 sleeping 标记（id/url 都保留）。
  // 顺手推一帧 nav（真链路里由主进程推），标签条才会把它渲染成 data-sleeping。
  const sleptId = await page.evaluate(() => {
    const a = window.__pool[0]
    const active = window.__pool[1]
    if (!a || !active) return ''
    a.sleeping = true
    const s = window.__store.getState()
    window.__navCb({
      id: active.id, url: active.url, title: '', loading: false, sid: s.activeSessionId || '', visible: true, srcPath: '',
      canGoBack: false, canGoForward: false, activeId: active.id, targets: window.__pool.map((t) => ({ ...t })),
    })
    return a.id
  })
  const sleepTab = () => page.evaluate(() => {
    const el = document.querySelector('.rtab[data-sleeping="1"]')
    if (!el) return null
    const label = el.querySelector('.rtab-t')
    return {
      view: el.dataset.view || '',
      title: label ? label.textContent : '',
      on: el.classList.contains('on'),
      tabs: document.querySelectorAll('.rtab').length,
    }
  })
  const preSleep = await sleepTab()
  const renderCalls = await page.evaluate(() => window.__webRenderCalls)
  check('仪器：休眠帧让标签带上 data-sleeping（否则下面点的是普通标签）', !!preSleep, JSON.stringify(preSleep))
  check('仪器：provider 的 render 真的执行过（不是 `??` 兜底分支）', renderCalls > 0, `renderCalls=${renderCalls}`)
  check('前置：休眠标签在池里是**原 id**', !!preSleep && preSleep.view === sleptId, `${JSON.stringify(preSleep)} 期望 ${sleptId}`)
  check('前置：休眠标签不是激活标签（点它才会走激活分支）', !!preSleep && preSleep.on === false, JSON.stringify(preSleep))
  check('休眠标签名由池里 url 推出（provider.title，不是 kindLabel 兜底）',
    !!preSleep && preSleep.title === 'sleep-a.pdf', JSON.stringify(preSleep))

  await page.click('.rtab[data-sleeping="1"]')
  await page.waitForTimeout(50)
  const activations = await page.evaluate(() => window.__activateCalls.slice())
  const postSleep = await sleepTab()
  check('点休眠标签 → 用**原 id** 调 embedActivate（唤醒同一页，不是新建）',
    activations.includes(sleptId), JSON.stringify(activations))
  check('唤醒后台标签名不变（URL 快照没丢）',
    !!postSleep && postSleep.title === preSleep.title, `${JSON.stringify(preSleep)} → ${JSON.stringify(postSleep)}`)
  check('唤醒后标签身份不变（data-view 仍是原 id）',
    !!postSleep && postSleep.view === preSleep.view, `${JSON.stringify(preSleep)} → ${JSON.stringify(postSleep)}`)
  check('唤醒后标签数不变（没有多开一个重复标签）',
    !!postSleep && postSleep.tabs === preSleep.tabs, `${JSON.stringify(preSleep)} → ${JSON.stringify(postSleep)}`)
  check('唤醒后该标签成为激活标签（点开即切过去）',
    !!postSleep && postSleep.on === true, JSON.stringify(postSleep))

  console.log('D1 去重：同一文件不重复打开')
  await reset()
  const x1 = await fire('out/sales.xlsx')
  const x2 = await fire('out/sales.xlsx')
  check('首次打开（opened=true）', x1.opened === true, JSON.stringify(x1))
  check('第二次同文件不再打开（opened=false，不把用户拽回）', x2.opened === false, JSON.stringify(x2))
  const x3 = await fire('out/sales.xlsx') // 复位后同文件仍应被去重挡住
  check('复位视图后同文件仍去重（去重锚是「文件」不是「视图」）', x3.opened === false, JSON.stringify(x3))

  console.log('S1 后台会话（非激活会话）的产出不抢当前视图')
  await reset()
  const bg = await fire('out/report.pdf', 'S-OTHER')
  check('非激活会话不自动打开（opened=false）', bg.opened === false, JSON.stringify(bg))
  check('后台会话不改当前视图', bg.tab === 'activity' && bg.expanded === false, JSON.stringify(bg))

  // —— S2/S3：阶段 4 的主战场（用户原话：「切会话/切工作区/新建会话也会自动打开右侧浏览器 Tab 很别扭」）——
  // 旧实现是**进程级**判据：任何内嵌视图出现真实页面 + 单值 `embedAutoOpenedForSid` 只记得最后一个
  // 会话 → 切到新会话后下一次任何 nav 事件都又展开一次。这里把两种来源都钉住。
  console.log('S2 别的会话的页面出现 → 当前面板三态**全部不变**')
  await reset()
  await page.evaluate(() => {
    window.__store.setState({
      activeSessionId: 'S1', rightExpanded: true, leftExpanded: true,
      // 面板停在「变更」（激活标签 = git）
      tabsByScope: { S1: { tabs: [{ id: 'g1', kind: 'git' }], activeId: 'g1', closed: [] } },
    })
  })
  await page.evaluate(() => { window.__pool.push({ id: 't9', url: 'https://other.example', title: 'Other', sid: 'S-OTHER', srcPath: '' }) })
  const beforeS2 = await snap()
  await nav('t9')
  const afterS2 = await snap()
  check('别的会话的页面不进本 scope 的标签集（syncWebTabs 按 scope 过滤）',
    afterS2.webIds === '', JSON.stringify(afterS2))
  check('当前面板的 激活标签/rightExpanded/leftExpanded 全部不变',
    afterS2.tab === beforeS2.tab && afterS2.expanded === beforeS2.expanded && afterS2.left === beforeS2.left,
    `${JSON.stringify(beforeS2)} → ${JSON.stringify(afterS2)}`)

  console.log('S3 AI 在别的会话操作浏览器（onEmbedAiAction）→ 不展开、不切标签、不收左栏')
  await reset()
  await page.evaluate(() => {
    window.__store.setState({
      activeSessionId: 'S1', rightExpanded: false, leftExpanded: true,
      tabsByScope: { S1: { tabs: [{ id: 'act1', kind: 'activity' }], activeId: 'act1', closed: [] } },
    })
  })
  const beforeS3 = await snap()
  await page.evaluate(() => window.__aiCb({ kind: 'down', x: 10, y: 20 }))
  const afterS3 = await snap()
  check('AI 动作后 激活标签/rightExpanded/leftExpanded 全部不变（旧实现走 focusBrowser：展开+切 web+收左栏）',
    afterS3.tab === beforeS3.tab && afterS3.expanded === beforeS3.expanded && afterS3.left === beforeS3.left,
    `${JSON.stringify(beforeS3)} → ${JSON.stringify(afterS3)}`)
  const aiActive = await page.evaluate(() => window.__store.getState().embedAiActive)
  check('AI 动作的可见反馈仍在（embedAiActive 呼吸徽标）', aiActive === true, String(aiActive))

  console.log('边界：绝对路径在工作区内 → 相对化后定位')
  await reset()
  const abs = await fire('/w/out/chart.svg')
  check('工作区内绝对路径被相对化（fileFocus=out/chart.svg）', abs.focus === 'out/chart.svg', JSON.stringify(abs))

  check('无页面错误', errs.length === 0, errs.join('; '))
  await browser.close()
} catch (e) {
  // 前置不满足 ≠ 断言失败：退出码 2（00 §4.6：红必须落到具体机制，不许含糊成「测试挂了」）。
  if (!isPrecondition(e)) throw e
  console.error(`\n⛔ 前置不满足（退出码 2，不是测试失败）：${e.message}`)
  code = 2
} finally {
  cleanup()
}

if (code === 0) {
  console.log(`\n${fails.length === 0 ? 'ALL PASS' : `FAILED (${fails.length}): ${fails.join('; ')}`}`)
  code = fails.length === 0 ? 0 : 1
}
process.exit(code)
