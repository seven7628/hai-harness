// 预览容器行为的真 Electron 验证（2026-09-17 立；2026-09-17 R2 按「新建标签」语义**翻转**）。
//
// 背景（本次设计探讨引出的代码事实）：同一个内嵌视图池有**两条不对称**的路径 ——
//   - 浏览器工具开页 → openEmbedTarget → createEntry **真新建**（视图并存，池累加）
//   - 本地文件预览   → 修复前 openHtmlInBrowser → embedShow + embedNavigate → 导航进**激活的那个**
// 后者在池非空时不会新建（embed.ts 的 ensureEmbed「views.size > 0 即 return」），于是
// 「预览 A → 预览 B」= B 顶掉 A：没有标签、无法返回，A 的页面状态丢失。
//
// R2（§3.1）把预览统一到 store.openInBrowserTab：**新建标签** + 带会话归属（sid）。本脚本随之
// **翻转**那两条被钉成缺陷的断言：
//   · 修复前：预览两次后视图池**未增长**（复用同一视图 → 无法并存）→ 现在必须**增长**
//   · 修复前：A 的页面**已不在视图池中**（被顶掉，不可返回）→ 现在 A 必须在池中，切回后 DOM 仍可达
// 驱动方式也必须随之改变（见下），否则翻转没有意义 —— 用旧的实现细节（embedShow/embedNavigate）
// 驱动，只能复现缺陷，证明不了修复。
//
// 驱动方式（关键）：断言由**真实调用路径**驱动 —— store 的 openHtmlInBrowser / openInBrowserTab
// （用户点产出卡/文件栏/消息里的「在浏览器打开」时走的就是它们），而不是底层 IPC。怎么在真
// Electron 里拿到真实 store 实例：以 dev 模式启动（VITE_DEV_SERVER_URL → vite dev server），
// 渲染层仍是**真应用**（真 UI、真 preload、真 IPC、真主进程视图池），e2e 用
// `import('/src/store/useAppStore.ts')` 拿到**同一个模块实例**（vite 的模块注册表按 URL 去重）
// → 调的就是用户点击时那个 action。T0 先钉住「拿到的确实是活实例」（bridgeStatus=running +
// 真会话 id），否则后面的断言全在空转。
//
// 覆盖：
//   T0 真实 store 实例 + 真会话就绪（驱动前提：不是活实例就立即红，不静默空转）
//   T1 openEmbedTarget 两次 → 视图池累加（对照组：并存是池本身具备的能力）
//   T2 预览路径（openHtmlInBrowser 两次）→ 池**增长**、A/B **并存**、各带会话 sid、activate 语义生效
//   T3 切回 A（去重命中，不新建）→ A 的 DOM **仍可达**；再切 B → B 的 DOM 也仍在（各自保活）
//   T3b activate:false（Agent 自动打开，R3 的语义）→ 新建但不激活、不动面板
//   T5 去重优先本会话 + activate:false 跨会话不静默
//   T4 视图池上限（MAX_EMBED_VIEWS=8）：到顶后再开**明确失败**，不静默驱逐
//   T7 撞上限时的失败口径：背景静默 / 用户点击响亮
//   T6 预览**不收起左栏**（真链路：nav 事件路径）
//   T11（2026-09 标签语义）同一 file:// 文件预览两次 → 池**不增长**、同一个标签、内容真的刷新
//   T12（同上）srcPath 去重：同一 srcPath 换 URL → 复用同一个标签并导航到新 URL
//   T13（同上）activate:false 下的刷新：不改 activeId、不展开/不切面板、不收左栏，但内容确实刷新
//   T16 收起右栏不得把原生视图缩成 80×80 / 把页面排版视口钉死（用户报障「只剩左上角一小块」）
//
// 用法：cd desktop/app && node e2e/verify_preview_container.mjs
//   GO_CODE_E2E_FORCE_BUILD=1 → 强制 npm run build（改了 electron/ 或 src/ 后必须，否则跑旧产物）
// 退出码：全通过 0；任一断言失败非 0。
import { _electron } from 'playwright'
import { spawn, spawnSync } from 'child_process'
import fs from 'node:fs'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const REPO = path.resolve(APP, '../..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'prev-e2e-home-'))
const WS = fs.mkdtempSync(path.join(os.tmpdir(), 'prev-e2e-ws-'))
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const fails = []

function check(label, cond, detail = '') {
  if (cond) console.log(`  ✅ ${label}`)
  else { fails.push(label); console.log(`  ❌ ${label} ${detail}`) }
}

async function waitPort(port, timeoutMs = 60000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { clearTimeout(timer); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error(`vite dev server 未就绪（port ${port}）`)
    await sleep(250)
  }
}

async function waitFor(fn, { timeout = 60000, interval = 250, label = '条件' } = {}) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v } catch { /* 继续 */ }
    await sleep(interval)
  }
  throw new Error(`等待超时: ${label}`)
}

// —— 构建产物 ——
// 主进程跑 dist-electron/main.js（renderer 由 dev server 提供源码）：改了 electron/** 必须重建，
// 故支持 GO_CODE_E2E_FORCE_BUILD=1 强制构建（策略同 e2e/verify_preview_scoping.mjs）。
const index = path.join(APP, 'dist', 'index.html')
const mainJs = path.join(APP, 'dist-electron', 'main.js')
const needsBuild = process.env.GO_CODE_E2E_FORCE_BUILD === '1' || !fs.existsSync(index) || !fs.existsSync(mainJs)
if (needsBuild) {
  const b = spawnSync('npm', ['run', 'build'], { cwd: APP, encoding: 'utf8' })
  if (b.status !== 0) throw new Error(`npm run build 失败:\n${b.stdout}\n${b.stderr}`)
}
// bridge：真会话（T0 的 new_session）要经 Go bridge，故必须现构建二进制。
const build = spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], { cwd: REPO, encoding: 'utf8' })
if (build.status !== 0) throw new Error(`go build 失败:\n${build.stderr}`)

// 三个待预览的本地文件（内容可区分，供 CDP 读回校验）
const fileA = path.join(WS, 'preview-a.html')
const fileB = path.join(WS, 'preview-b.html')
const fileC = path.join(WS, 'preview-c.html')
// T6/T7 专用：必须是**池里还没有的 URL**，否则去重命中 → 不发生导航/不触发新建 → 断言空转。
const fileT6 = path.join(WS, 'preview-t6.html')
const fileT7 = path.join(WS, 'preview-t7.html')
// T3c 专用：必须是**池里还没有的 URL**（去重命中会变成「刷新已有标签」，验不到「新标签即激活视图」）。
const fileT3C = path.join(WS, 'preview-t3c.html')
// T11/T13 专用：**同一个文件**先后预览两次（中间改写内容）→ 验证「打开过的文件：刷新已存在的标签」。
const fileT11 = path.join(WS, 'preview-t11.html')
// T12：srcPath 去重的两个**不同** URL（同一个 srcPath）。
const fileT12A = path.join(WS, 'preview-t12-a.html')
const fileT12B = path.join(WS, 'preview-t12-b.html')
// T15 专用：**唯一 URL**（避免命中去重 → 断言空转），用来提供一个「可见的原生视图」。
const fileT15 = path.join(WS, 'preview-t15.html')
const fileT16 = path.join(WS, 'preview-t16.html')
const markerHtml = (m) => `<html><body><h1 id="marker">${m}</h1></body></html>`
fs.writeFileSync(fileA, '<html><body><h1 id="marker">PREVIEW-A-MARKER</h1></body></html>')
fs.writeFileSync(fileB, '<html><body><h1 id="marker">PREVIEW-B-MARKER</h1></body></html>')
fs.writeFileSync(fileC, '<html><body><h1 id="marker">PREVIEW-C-MARKER</h1></body></html>')
fs.writeFileSync(fileT6, '<html><body><h1 id="marker">PREVIEW-T6-MARKER</h1></body></html>')
fs.writeFileSync(fileT7, '<html><body><h1 id="marker">PREVIEW-T7-MARKER</h1></body></html>')
fs.writeFileSync(fileT3C, markerHtml('PREVIEW-T3C-MARKER'))
fs.writeFileSync(fileT11, markerHtml('PREVIEW-T11-A'))
fs.writeFileSync(fileT12A, markerHtml('PREVIEW-T12-A'))
fs.writeFileSync(fileT12B, markerHtml('PREVIEW-T12-B'))
fs.writeFileSync(fileT15, markerHtml('PREVIEW-T15-MARKER'))
fs.writeFileSync(fileT16, markerHtml('PREVIEW-T16-MARKER'))
// T4 专用：**每个 URL 都不同**（同名同 URL 会命中 dedupe → 池不增长 → 上限断言永远到不了上限）。
// 阶段 3 起上限是「活渲染进程数 ≤ MAX_EMBED_VIEWS（12）」，超限**休眠**最久没看的标签，
// 故这里要开够数量才能触发驱逐（14 个 > 12；起始池还有若干 T1/T2/T3 留下的视图）。
const capFiles = []
for (let i = 0; i < 14; i++) {
  const p = path.join(WS, `cap-${i}.html`)
  fs.writeFileSync(p, markerHtml(`PREVIEW-CAP-${i}`))
  capFiles.push(p)
}
// T7 专用：构造「保护集不可驱逐」（后台路径连开，lastUsed 恒为 0 → 全在保护集里）用的
// **全新** URL —— 必须是池里没有的，否则去重命中会变成「恢复已有标签」而不是新建。
const guardFiles = []
for (let i = 0; i < 16; i++) {
  const p = path.join(WS, `guard-${i}.html`)
  fs.writeFileSync(p, markerHtml(`PREVIEW-GUARD-${i}`))
  guardFiles.push(p)
}
// T9 专用：保护集验证（刚被 AI 命令过的页 / 运行中会话的页 不得被驱逐）
const protFiles = []
for (let i = 0; i < 16; i++) {
  const p = path.join(WS, `prot-${i}.html`)
  fs.writeFileSync(p, markerHtml(`PREVIEW-PROT-${i}`))
  protFiles.push(p)
}
// T8 专用：关闭 → destroyed 广播的载体（唯一 URL，避免命中去重）
const fileT8 = path.join(WS, 'preview-t8.html')
fs.writeFileSync(fileT8, markerHtml('PREVIEW-T8-MARKER'))

const toFileUrl = (p) => 'file://' + p.split('/').map(encodeURIComponent).join('/')

// expectedMarkerOf：从 URL 推出该文件里 <h1 id="marker"> 的内容（T4 恢复后要验「内容真回来了」）。
// 不能写死 'PREVIEW-CAP'：被休眠的可能是**前序用例**留下的视图（preview-a.html 等），
// 写死会让断言在「恢复其实没发生、激活的还是别人」时也变绿（空转）。
function expectedMarkerOf(url) {
  const base = String(url).split('/').pop() ?? ''
  const p = /^preview-([a-z0-9]+)\.html$/.exec(base)
  if (p) return `PREVIEW-${p[1].toUpperCase()}-MARKER`
  const c = /^cap-(\d+)\.html$/.exec(base)
  if (c) return `PREVIEW-CAP-${c[1]}`
  return 'PREVIEW'
}

// 调试代理原始客户端（AI 的 CDP 通道就是它）：destroyed 是主进程广播给**所有** TCP 客户端的
// 单向推送（Go 侧据此 RemoveTarget），渲染层看不到；「命令打到休眠条目」也只有在协议层才验得
// 出来（embedCommand 只作用激活视图）。故 e2e 直连它。
async function connectDbgProxy(win) {
  const proxy = await win.evaluate(() => window.desktop.embedDebuggerProxy())
  const port = Number(String(proxy?.endpoint ?? '').split(':').pop())
  if (!proxy?.ok || !port) return null
  const sock = net.connect({ host: '127.0.0.1', port })
  const lines = []
  let buf = ''
  sock.on('data', (d) => {
    buf += String(d)
    let i
    while ((i = buf.indexOf('\n')) >= 0) {
      const raw = buf.slice(0, i)
      buf = buf.slice(i + 1)
      try { lines.push(JSON.parse(raw)) } catch { /* 半行/噪声忽略 */ }
    }
  })
  await new Promise((res, rej) => { sock.once('connect', res); sock.once('error', rej) })
  return {
    sock,
    lines,
    send: (obj) => sock.write(JSON.stringify(obj) + '\n'),
    // waitLine 在已收到的行里等一条满足条件的消息（from = 从第几行开始扫）
    async waitLine(pred, { timeout = 8000, from = 0 } = {}) {
      const end = Date.now() + timeout
      for (;;) {
        for (let i = from; i < lines.length; i++) if (pred(lines[i])) return lines[i]
        if (Date.now() > end) return null
        await sleep(120)
      }
    },
  }
}

// —— vite dev server（renderer 走源码：e2e 才能 import 到与真应用**同一个** store 实例）——
const DEV_PORT = Number(process.env.PREVIEW_E2E_PORT || 5391)
const server = spawn(
  'node',
  [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(DEV_PORT), '--strictPort'],
  { cwd: APP, env: { ...process.env, VITE_PORT: String(DEV_PORT) }, stdio: 'pipe' },
)
let viteErr = ''
server.stderr.on('data', (d) => { viteErr += String(d) })
server.stdout.on('data', () => { /* 保持管道不阻塞 */ })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  fs.rmSync(HOME, { recursive: true, force: true })
  fs.rmSync(WS, { recursive: true, force: true })
}

let app = null
try {
  await waitPort(DEV_PORT)
  app = await _electron.launch({
    executablePath: ELECTRON,
    args: ['.'],
    cwd: APP,
    // HAI_DATA_DIR：多实例模式（跳过单实例锁 + 配置根/userData 随 HOME 隔离）。
    // VITE_DEV_SERVER_URL：dev 模式 → 真应用 + 真 store（见文件头「驱动方式」）。
    env: { ...process.env, HOME, HAI_DATA_DIR: HOME, VITE_DEV_SERVER_URL: `http://localhost:${DEV_PORT}/` },
  })
  const win = await app.firstWindow()
  win.on('console', (m) => { if (m.type() === 'error' && !/favicon/.test(m.text())) console.log('[renderer-err]', m.text()) })
  await win.waitForLoadState('domcontentloaded')

  // 注入隔离工作区（userData 已随 HAI_DATA_DIR 隔离，不碰用户真实 localStorage）→ reload 跳过选目录
  await win.evaluate(([ws]) => { localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws })) }, [WS])
  await win.reload()
  await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: '主界面就绪', timeout: 60000 })

  const api = await win.evaluate(() => {
    const d = window.desktop
    return { status: !!d?.embedStatus, open: !!d?.embedOpenTarget, activate: !!d?.embedActivate, cmd: !!d?.embedCommand }
  })
  if (!api.status || !api.open || !api.activate || !api.cmd) {
    console.log(`  ⚠ 缺少 embed API，跳过（${JSON.stringify(api)}）`)
    console.log('\nSKIPPED (embed api unavailable)')
    process.exit(0)
  }

  // —— 页面内测试辅助：真实 store 实例 + 池快照 + DOM 读回 ——
  // store 实例按 URL 去重 → 与真应用用的是同一个（T0 会证明它确实是活的）。
  await win.evaluate(() => {
    window.__r2 = {
      async snap() {
        const st = await window.desktop.embedStatus()
        // sleeping/restoring：阶段 3 的休眠态（视图池 LRU 驱逐后条目仍在 targets 里，见
        // docs/RIGHT_PANEL_TABS_PLAN.md §11.4）。n = 条目总数（含休眠），live = 活渲染进程数
        //（上限只管 live —— 休眠不占渲染进程，这正是「上限可以放宽」的前提）。
        const targets = (st?.targets ?? []).map((t) => ({ id: t.id, url: t.url, sid: t.sid, visible: t.visible, srcPath: t.srcPath ?? '', loading: !!t.loading, sleeping: !!t.sleeping, restoring: !!t.restoring }))
        return { n: targets.length, live: targets.filter((t) => !t.sleeping).length, activeId: st?.activeId ?? null, targets }
      },
      // 读**激活视图**的 DOM marker（CDP Runtime.evaluate）。响应形状可能是
      // {result:{result:{value}}} / {result:{value}} / {value}，全试一遍（既有写法，勿改）。
      async marker() {
        for (let i = 0; i < 15; i++) {
          const res = await window.desktop.embedCommand({
            method: 'Runtime.evaluate',
            params: { expression: 'document.getElementById("marker")?.textContent ?? ""', returnByValue: true },
          })
          const v = res?.result?.result?.value ?? res?.result?.value ?? res?.value ?? ''
          if (typeof v === 'string' && v.includes('PREVIEW')) return v
          await new Promise((r) => setTimeout(r, 300))
        }
        return ''
      },
      // markerWait 等**指定** marker 出现（T11/T13 用来证明「刷新后真的读到新内容」）：
      // 上面的 marker() 读到含 'PREVIEW' 的字符串就返回 —— 拿它验刷新会把 **reload 前的旧内容**
      // 当成刷新成功（旧 marker 也含 PREVIEW），断言就永远是绿的（空转）。
      async markerWait(want, ms = 8000) {
        const end = Date.now() + ms
        let last = ''
        for (;;) {
          const res = await window.desktop.embedCommand({
            method: 'Runtime.evaluate',
            params: { expression: 'document.getElementById("marker")?.textContent ?? ""', returnByValue: true },
          })
          last = res?.result?.result?.value ?? res?.result?.value ?? res?.value ?? ''
          if (typeof last === 'string' && last.includes(want)) return last
          if (Date.now() > end) return last
          await new Promise((r) => setTimeout(r, 250))
        }
      },
      async panel() {
        const m = await import('/src/store/useAppStore.ts')
        const s = m.useAppStore.getState()
        return { tab: s.rightTab, expanded: s.rightExpanded, left: s.leftExpanded }
      },
      async resetPanel() {
        const m = await import('/src/store/useAppStore.ts')
        m.useAppStore.setState({ rightTab: 'activity', rightExpanded: false })
      },
      async call(name, args) {
        const m = await import('/src/store/useAppStore.ts')
        await m.useAppStore.getState()[name](...args)
        return true
      },
    }
  })

  // —— T0 驱动前提：拿到的是**活**的 store 实例（不是第二份模块）——
  // 判据：bridgeStatus=running 只有 start() 在**这个**实例上跑过才可能为真；activeSessionId
  // 是 bridge 的 new_session 响应回填的。两者都成立 ⇒ 后续 action 调的确实是真应用的 store。
  console.log('T0 真实 store 实例 + 真会话（驱动前提）')
  const t0a = await win.evaluate(async () => {
    const m = await import('/src/store/useAppStore.ts')
    const s = m.useAppStore.getState()
    return {
      typeofTab: typeof s.openInBrowserTab,
      typeofLegacy: typeof s.openHtmlInBrowser,
      bridge: s.bridgeStatus,
      sid: s.activeSessionId ?? null,
      workspace: s.workspace ?? null,
    }
  })
  check('store.openInBrowserTab 已实现（typeof function）', t0a.typeofTab === 'function', JSON.stringify(t0a))
  check('store.openHtmlInBrowser 仍在（6 处调用方零改动）', t0a.typeofLegacy === 'function', JSON.stringify(t0a))
  check('拿到的是**活** store 实例（bridgeStatus=running）', t0a.bridge === 'running', JSON.stringify(t0a))
  // 空工作区按规格不自动建会话（store:2670「不建空会话」）→ 用真应用的 newSession（左栏
  // 「新建会话」按钮调的同一个 action）建一个，再等 activeSessionId 回填。
  if (!t0a.sid) {
    await win.evaluate(() => window.__r2.call('newSession', []))
    await waitFor(() => win.evaluate(async () => {
      const m = await import('/src/store/useAppStore.ts')
      return m.useAppStore.getState().activeSessionId || ''
    }), { label: '会话创建（new_session 响应回填 activeSessionId）', timeout: 60000 })
  }
  const SID = await win.evaluate(async () => {
    const m = await import('/src/store/useAppStore.ts')
    return m.useAppStore.getState().activeSessionId ?? ''
  })
  check('真会话就绪（activeSessionId 非空，预览可带会话归属）', !!SID, `sid=${JSON.stringify(SID)}`)
  await sleep(1000)

  // —— T1 对照组：openEmbedTarget 两次 → 池累加 ——
  console.log('T1 openEmbedTarget 两次（对照组：池本身能并存）')
  const t1 = await win.evaluate(async ([a, b]) => {
    const d = window.desktop
    const n0 = (await window.__r2.snap()).n
    await d.embedOpenTarget(a)
    await new Promise((r) => setTimeout(r, 900))
    const n1 = (await window.__r2.snap()).n
    await d.embedOpenTarget(b)
    await new Promise((r) => setTimeout(r, 900))
    const st = await window.__r2.snap()
    return { n0, n1, n2: st.n, urls: st.targets.map((t) => t.url), activeId: st.activeId }
  }, [toFileUrl(fileA), toFileUrl(fileB)])
  check('openEmbedTarget 每次新建视图（池增长 n1>n0 且 n2>n1）',
    t1.n1 > t1.n0 && t1.n2 > t1.n1, JSON.stringify(t1))
  check('两个 file:// 同时存在于视图池（并存能力成立）',
    t1.urls.filter((u) => u && u.startsWith('file://')).length >= 2, JSON.stringify(t1.urls))

  // T1 的残留必须先清掉：它建出的正是 preview-a / preview-b —— 留着会让 T2「A 仍在池中」的
  // 断言被 T1 的遗留视图满足（假阳性，断言空转）。清空后还显式断言池里没有 file://（前提检查）。
  await win.evaluate(async () => {
    const st = await window.desktop.embedStatus()
    for (const t of st?.targets ?? []) {
      if (t.url && t.url !== 'about:blank') await window.desktop.embedCloseTarget(t.id)
    }
  })
  await sleep(500)
  const clean = await win.evaluate(() => window.__r2.snap())
  check('T2 前提：池中已无 file:// 视图（T1 残留清空，后续断言不靠遗留视图满足）',
    !clean.targets.some((t) => String(t.url || '').startsWith('file://')), JSON.stringify(clean.targets))

  // —— T2 预览路径（真实调用路径：store.openHtmlInBrowser = 6 处用户点击的入口）——
  // 修复前这条路径走 embedShow + embedNavigate（复用激活视图）→ 池不增长、A 被顶掉。
  console.log('T2 预览两次（store.openHtmlInBrowser，真实调用路径）')
  const t2 = await win.evaluate(async ([a, b]) => {
    await window.__r2.resetPanel()
    const n0 = (await window.__r2.snap()).n
    await window.__r2.call('openHtmlInBrowser', [a])
    await new Promise((r) => setTimeout(r, 1200))
    const s1 = await window.__r2.snap()
    const p1 = await window.__r2.panel()
    // 第二次预览前复位面板：让「展开 + 切 web tab」只可能来自本次 action。
    // （§6.9 的 onEmbedNav 自动展开每会话只触发一次，且已在第一次预览时用掉 → 这里不会被它干扰。）
    await window.__r2.resetPanel()
    await window.__r2.call('openHtmlInBrowser', [b])
    await new Promise((r) => setTimeout(r, 1200))
    const s2 = await window.__r2.snap()
    const p2 = await window.__r2.panel()
    return { n0, n1: s1.n, n2: s2.n, active1: s1.activeId, t1: s1.targets, t2: s2.targets, activeId: s2.activeId, p1, p2 }
  }, [fileA, fileB])
  check('预览每次**新建**标签（池增长：n1>n0 且 n2>n1）',
    t2.n1 > t2.n0 && t2.n2 > t2.n1, JSON.stringify({ n0: t2.n0, n1: t2.n1, n2: t2.n2 }))
  const aUrl = toFileUrl(fileA)
  const bUrl = toFileUrl(fileB)
  const aViews = t2.t2.filter((t) => t.url === aUrl)
  const bViews = t2.t2.filter((t) => t.url === bUrl)
  // 翻转点 ①：修复前这里是「预览两次后视图池未增长（复用同一视图 → 无法并存）」。
  check('两个预览**并存**于视图池（A、B 各占一个标签）',
    aViews.length === 1 && bViews.length === 1, JSON.stringify(t2.t2))
  // 翻转点 ②：修复前这里是「A 的页面已不在视图池中（被顶掉，不可返回）」。
  check('A 仍在视图池中（未被 B 顶掉，可返回）',
    aViews.length === 1, JSON.stringify(t2.t2))
  // 泄漏收口（R2 的第二个目标）：预览视图必须带**会话归属**，不能落在自举视图（sid=''）上
  // ——否则会话 A 的预览会出现在会话 B（旧预览路径正是落在 sid='' 上）。
  check('两个预览视图都带本会话 sid（不落在自举视图 sid=\'\' 上）',
    aViews.every((t) => t.sid === SID) && bViews.every((t) => t.sid === SID),
    JSON.stringify({ sid: SID, aViews, bViews }))
  check('首次预览后 A 的视图被激活（activeId 指向 A 的标签）',
    aViews.some((t) => t.id === t2.active1), JSON.stringify({ active1: t2.active1, aViews }))
  check('activate:true → 展开右栏 + 切 web tab（第二次预览，复位后仍成立）',
    t2.p2.tab === 'web' && t2.p2.expanded === true, JSON.stringify(t2.p2))

  // —— T3 切回 A：去重命中（不新建）+ A 的 DOM 仍可达 ——
  // 修复前 A 已被顶掉：切回无从谈起（池里没有 A 的视图），只能读到 B 的内容。
  console.log('T3 切回 A（去重命中不新建）→ 读 A 的 DOM')
  const t3 = await win.evaluate(async ([a, b]) => {
    const n0 = (await window.__r2.snap()).n
    await window.__r2.call('openInBrowserTab', [a, { activate: true }]) // 去重：命中 A → 只切不建
    await new Promise((r) => setTimeout(r, 800))
    const sA = await window.__r2.snap()
    const markerA = await window.__r2.marker()
    await window.__r2.call('openInBrowserTab', [b, { activate: true }]) // 再切回 B（同样只切不建）
    await new Promise((r) => setTimeout(r, 800))
    const sB = await window.__r2.snap()
    const markerB = await window.__r2.marker()
    return { n0, nA: sA.n, nB: sB.n, activeA: sA.activeId, activeB: sB.activeId, markerA, markerB, targets: sB.targets }
  }, [fileA, fileB])
  check('切回 A 不新建标签（去重命中：池大小不变）', t3.nA === t3.n0, JSON.stringify(t3))
  check('切回 B 也不新建标签（去重命中：池大小不变）', t3.nB === t3.n0, JSON.stringify(t3))
  check('切回 A 后 A 的 DOM **仍可达**（页面状态未丢，标记 = A）',
    t3.markerA.includes('PREVIEW-A'), `markerA=${JSON.stringify(t3.markerA)}`)
  check('再切回 B 后 B 的 DOM 仍在（两个标签各自保活，标记 = B）',
    t3.markerB.includes('PREVIEW-B'), `markerB=${JSON.stringify(t3.markerB)}`)

  // —— T3b activate:false → 新建但不激活、不动面板（**已无调用方**：钉的是开关本身的语义）——
  // 历史：一改（ADR-2「不抢视线」）时 Agent 自动打开走的就是这条路。2026-09 二改（「产出即展示」）
  // 后 Agent 产出改走 activate:true（见下面的 T3c），本用例保留为**开关语义**的钉子：将来若有人
  // 再以 activate:false 打开预览，行为必须仍是「不激活、不动面板」。
  console.log('T3b activate:false（开关语义）→ 新建但不激活、不动面板')
  const t3b = await win.evaluate(async ([c]) => {
    const before = await window.__r2.snap()
    const p0 = await window.__r2.panel()
    await window.__r2.call('openInBrowserTab', [c, { activate: false }])
    await new Promise((r) => setTimeout(r, 1000))
    const after = await window.__r2.snap()
    const p1 = await window.__r2.panel()
    return { n0: before.n, n1: after.n, activeBefore: before.activeId, activeAfter: after.activeId, p0, p1, targets: after.targets }
  }, [fileC])
  check('activate:false 仍**新建**标签（内容准备好）', t3b.n1 === t3b.n0 + 1, JSON.stringify({ n0: t3b.n0, n1: t3b.n1 }))
  check('activate:false **不激活**（activeId 不变）', t3b.activeAfter === t3b.activeBefore, JSON.stringify(t3b))
  check('activate:false **不动面板**（rightTab/rightExpanded 不变）',
    t3b.p1.tab === t3b.p0.tab && t3b.p1.expanded === t3b.p0.expanded, JSON.stringify({ p0: t3b.p0, p1: t3b.p1 }))

  // —— T3c 「产出即展示」= Agent 自动打开的**当前**语义（2026-09 二改，修的就是本文开头那个缺陷）——
  // 与 T3b 的分工：T3b 钉的是 activate:false 这个**开关**（已无调用方）；T3c 钉的是 Agent 产出
  // 实际走的那条路（activate:true + quiet:true，见 store §6.10）。
  // 为什么必须在真视图池这一层钉：这个缺陷在 store 层**看不出来** —— 内容确实准备好了、面板也确实
  // 展开了，唯独没有任何一步把它设为 activeId，于是面板显示的是自举占位页（about:blank）：
  // 地址栏没有路径（RightPanel 的 navUrl 跟随 activeId）、内容空白，用户必须手点标签才看得到。
  // 断言用 marker()（读**激活视图**的 DOM）而不只是比 activeId —— 直接证明「用户看到的就是刚产出
  // 的那个文件」。quiet:true 同时被覆盖：它不该让打开失败（也不该在这里弹 toast，T7 已单独覆盖）。
  console.log('T3c 产出即展示（activate:true + quiet:true）→ 新标签即激活视图，DOM 可读回')
  const t3c = await win.evaluate(async ([c]) => {
    const before = await window.__r2.snap()
    await window.__r2.call('openInBrowserTab', [c, { activate: true, quiet: true }])
    await new Promise((r) => setTimeout(r, 1200))
    const after = await window.__r2.snap()
    const m = await window.__r2.marker()
    const p1 = await window.__r2.panel()
    const newIds = (after.targets ?? []).map((t) => t.id)
      .filter((id) => !(before.targets ?? []).some((b) => b.id === id))
    return { n0: before.n, n1: after.n, newIds, activeAfter: after.activeId, marker: m, p1 }
  }, [fileT3C])
  check('T3c 新建标签（产出内容进场）', t3c.n1 === t3c.n0 + 1, JSON.stringify({ n0: t3c.n0, n1: t3c.n1 }))
  check('T3c 新标签**就是**激活视图（activeId = 新标签 —— 缺陷修在这里）',
    t3c.newIds.length === 1 && t3c.activeAfter === t3c.newIds[0], JSON.stringify(t3c))
  check('T3c 激活视图的 DOM 就是刚产出的文件（marker 读回）',
    String(t3c.marker).includes('PREVIEW-T3C'), `marker=${JSON.stringify(t3c.marker)}`)
  check('T3c 面板切到 web tab（地址栏与内容都被显示出来）', t3c.p1.tab === 'web', JSON.stringify(t3c.p1))
  // T3c 自清理：关掉本用例新建并激活的标签。T4 的「池上限」用例需要**可用池位** ——
  // 泄漏一个标签会让它整段空转（实测：池被填满 → 9 次开页全失败 → okRes 为空 → 假红）。
  const t3cClean = await win.evaluate(async ([id]) => {
    await window.desktop.embedCloseTarget(id)
    const s = await window.__r2.snap()
    return { n: s.n, activeId: s.activeId }
  }, [t3c.newIds[0]])
  check('T3c 自清理：关掉本用例新建的标签（不把池状态泄漏给后续用例）',
    t3cClean.n === t3c.n0, JSON.stringify(t3cClean))

  // —— T5 去重必须**优先本会话**：同一文件已开在别的会话时，本会话重复预览不得堆积重复标签 ——
  // 为什么必须有这条：T1/T2/T3 都只用了**一个**会话，而「同一文件已开在别的会话」是多会话下的
  // 常态。若去重只按 URL 取首个匹配，就会永远命中**别人**的标签 → 跨会话激活被拒 → 每次都走
  // 「新建」→ 本会话内每点一次预览就多一个重复标签（修复前实测同 URL 视图 1→2→3），最终撞上
  // T4 的 8 个上限。这条断言是那次修复的回归护栏。
  // 位置：必须在 T4 **之前**（T4 会把池填到 8，之后无法再新建）。
  console.log('T5 去重优先本会话（跨会话同 URL 不堆积重复标签）')
  const setSession = (sid) => win.evaluate(async ([s]) => {
    const m = await import('/src/store/useAppStore.ts')
    m.useAppStore.setState({ activeSessionId: s })
    await window.desktop.embedSetSession(s) // 真应用里由 App.tsx 的 effect 做同一件事
    return m.useAppStore.getState().activeSessionId
  }, [sid])
  const SID_OTHER = 'S-E2E-OTHER'
  const relT5 = 'out/t5-dup.pdf'
  fs.mkdirSync(path.join(WS, 'out'), { recursive: true })
  fs.writeFileSync(path.join(WS, relT5), '%PDF-1.4\n% t5\n')
  const urlT5 = toFileUrl(path.join(WS, relT5))
  const sameUrl = (s) => s.targets.filter((t) => t.url === urlT5)
  const openT5 = () => win.evaluate(([p]) => window.__r2.call('openInBrowserTab', [p, { activate: true }]), [relT5])

  await setSession(SID)
  await openT5()
  await sleep(800)
  const t5a = await win.evaluate(() => window.__r2.snap())

  await setSession(SID_OTHER)
  await openT5()
  await sleep(800)
  const t5b = await win.evaluate(() => window.__r2.snap())
  check('T5 另一会话首次预览同文件 → 新建一个（跨会话激活被拒，不静默）',
    sameUrl(t5b).length === sameUrl(t5a).length + 1, JSON.stringify(sameUrl(t5b)))

  await openT5()
  await sleep(800)
  const t5c = await win.evaluate(() => window.__r2.snap())
  check('T5 同会话内**再点一次**同文件 → 复用本会话标签，不堆积重复标签',
    sameUrl(t5c).length === sameUrl(t5b).length,
    `修复前此处会 +1（同 URL 视图数 ${sameUrl(t5b).length} → ${sameUrl(t5c).length}）：${JSON.stringify(sameUrl(t5c))}`)
  check('T5 复用的确实是**本会话**的标签（不是又命中了别的会话的）',
    sameUrl(t5c).some((t) => t.sid === SID_OTHER), JSON.stringify(sameUrl(t5c)))

  // ④ R3：activate:false（Agent 后台自动产出）时，去重**只在当前会话里找** —— 同文件已在
  //    **别的会话**时必须在本会话新建，而不是静默无反应。「Agent 产出了却没反应」是用户最不能
  //    接受的失败模式（他既看不到标签、也没有任何提示）。
  const SID_THIRD = 'S-E2E-T5C'
  await setSession(SID_THIRD)
  await win.evaluate(([p]) => window.__r2.call('openInBrowserTab', [p, { activate: false }]), [relT5])
  await sleep(800)
  const t5d = await win.evaluate(() => window.__r2.snap())
  check('T5 activate:false 且同文件只在别的会话 → 必须在本会话新建（不静默无反应）',
    sameUrl(t5d).length === sameUrl(t5c).length + 1 && sameUrl(t5d).some((t) => t.sid === SID_THIRD),
    `同 URL 视图数 ${sameUrl(t5c).length} → ${sameUrl(t5d).length}：${JSON.stringify(sameUrl(t5d))}`)

  // —— T4 视图池上限：阶段 3 起是 **LRU 休眠**（不再抛错）——
  // 用户口径（2026-09-20）：「上限超出就报错肯定不好，会阻塞 Agent 运行；我建议改为 LRU，
  // 只是影响展示不影响 Agent 运行时」。故旧断言（「超限抛错」+「第一个创建的视图未被驱逐」）
  // 按新语义**翻转**为：
  //   ① 超限**不**抛错（连开 14 个全部 ok —— 「不阻塞」就是本阶段的目的）；
  //   ② 活条目数（live，= 渲染进程数）**不突破上限 12**；
  //   ③ 确实发生了**休眠**驱逐（sleeping ≥ 1；非空转的前提）；
  //   ④ 被驱逐的是**最久没用过**的（LRU：最先打开的那批先休眠），且**刚打开的那个**绝不被驱逐；
  //   ⑤ **无静默丢失**：所有打开过的 id 都仍在 targets 里（活 or sleeping）—— 旧实现「拒绝新建」
  //      也是不丢，新实现靠「休眠条目保留原 id」达成；
  //   ⑥ 被休眠的标签**可点回重建**：embedActivate(它) → 同一个 id 回到 live、内容可读、
  //      live 数 +1（旧实现下它根本不存在，这条必红）。
  // ⚠️ 14 个调用必须用**14 个不同 URL**：embed:open-target 会去重（同会话同 URL 命中已有标签 →
  // 刷新它、不新建）—— 反复开同一个 URL 根本到不了上限，那组断言就成了空转。
  console.log('T4 视图池上限（MAX_EMBED_VIEWS=12）：超限 LRU 休眠，不抛错')
  // 用真实的 file://：不存在的路径会让视图停在错误页，干扰「池是否增长」的观测。
  const t4 = await win.evaluate(async ([urls]) => {
    const d = window.desktop
    const before = await window.__r2.snap() // 起始池快照（口径与起始基数无关）
    const results = [] // { ok, error, n, live, activeId }：每次调用后的池大小
    const seenIds = new Set(before.targets.map((t) => t.id))
    const created = [] // 本次新建的 id（池快照差集：不依赖实现把 id 放在响应哪个字段）
    // 每开一个都等它**首个文档提交**再开下一个：LRU 顺序才与创建顺序严格一致（否则「被休眠的是
    // 一段前缀」会受加载时序影响 → 假红）。主进程侧对「文档还没提交」的条目也**不驱逐**（见
    // embed.ts evictCandidateId），这条等待让两边的口径对齐。
    const waitLoaded = async (id) => {
      for (let k = 0; k < 40; k++) {
        const st = await window.__r2.snap()
        const t = st.targets.find((x) => x.id === id)
        if (t && t.loading === false && t.url && t.url !== 'about:blank') return
        await new Promise((r) => setTimeout(r, 100))
      }
    }
    for (let i = 0; i < urls.length; i++) {
      const r = await d.embedOpenTarget(urls[i]) // activate 默认 true = 用户显式打开（参与 LRU）
      const st = await window.__r2.snap()
      const fresh = st.targets.map((t) => t.id).filter((id) => !seenIds.has(id))
      for (const id of fresh) { seenIds.add(id); created.push(id) }
      if (fresh[0]) await waitLoaded(fresh[0])
      const st2 = await window.__r2.snap()
      results.push({ ok: !!r?.ok, error: r?.error ?? '', n: st2.n, live: st2.live, activeId: st2.activeId ?? '' })
    }
    const stEnd = await window.__r2.snap()
    return { results, ids: stEnd.targets.map((t) => t.id), beforeIds: before.targets.map((t) => t.id), liveEnd: stEnd.live, sleeping: stEnd.targets.filter((t) => t.sleeping).map((t) => t.id), sleepingInfo: stEnd.targets.filter((t) => t.sleeping).map((t) => ({ id: t.id, sid: t.sid, url: t.url })), activeId: stEnd.activeId, created, firstUrl: urls[0], lastUrl: urls[urls.length - 1] }
  }, [capFiles.map(toFileUrl)])
  const okRes = t4.results.filter((r) => r.ok)
  // ① 不再抛错（旧实现：第 9 次起必失败）
  check('T4 超限**不再抛错**（连开 14 个全部 ok —— 不阻塞 Agent/用户）',
    t4.results.every((r) => r.ok), JSON.stringify(t4.results.filter((r) => !r.ok)))
  check('T4 每次成功都真的建了视图（不是静默无反应）',
    okRes.length === t4.results.length && t4.created.length >= 12, JSON.stringify({ ok: okRes.length, created: t4.created.length }))
  // ② 活条目（渲染进程）不突破上限
  check('T4 活条目数从不突破上限 12（上限口径 = 活渲染进程数）',
    t4.results.every((r) => r.live <= 12) && t4.liveEnd <= 12, JSON.stringify(t4.results.map((r) => r.live)))
  // ③ 非空转前提：确实发生了休眠驱逐
  check('T4 确实发生了**休眠驱逐**（sleeping ≥ 1：否则下面的 LRU 断言是空转）',
    t4.sleeping.length >= 1, `sleeping=${JSON.stringify(t4.sleeping)} live=${t4.liveEnd} n=${t4.ids.length}`)
  // ④ LRU：被驱逐的必须是**最久没用过**的那批 —— 在本轮新建的视图里，被休眠的必须是一段
  // **前缀**（cap-0 比 cap-1 旧，故 cap-1 被休眠时 cap-0 不可能还活着）；而**刚打开的那个**
  // 绝不被驱逐。为什么用「前缀」而不是「第一个一定被休眠」：起始池里还有前序用例留下的更旧
  // 的视图，驱逐从它们开始 —— 断言「第一个 cap 必被休眠」会依赖起始基数（假红）。
  const lastId = t4.created[t4.created.length - 1] ?? ''
  const sleepSet = new Set(t4.sleeping)
  const firstLiveCap = t4.created.findIndex((id) => !sleepSet.has(id))
  const prefixOk = firstLiveCap < 0 || t4.created.slice(firstLiveCap).every((id) => !sleepSet.has(id))
  check('T4 驱逐严格按 LRU 顺序（新建集合里被休眠的是一段前缀：不跳着踢）',
    prefixOk, `created=${JSON.stringify(t4.created)} sleeping=${JSON.stringify(t4.sleeping)}`)
  check('T4 刚打开的视图**不**被驱逐（LRU 只踢最旧的）',
    !!lastId && !sleepSet.has(lastId) && t4.ids.includes(lastId), `lastId=${lastId} sleeping=${JSON.stringify(t4.sleeping)}`)
  // ⑤ 无静默丢失：打开过的 id 都还在（活 or 休眠）
  const lost = [...t4.beforeIds, ...t4.created].filter((id) => !t4.ids.includes(id))
  check('T4 无静默丢失（打开过的条目全部仍在 targets 里：活 or 休眠）',
    lost.length === 0, `lost=${JSON.stringify(lost)}`)
  // —— 内存基线（P09/P10；03 §2.3 / R-5 的硬要求：无基线不许合）——
  // 负载 = T4 循环刚把**活渲染进程**顶到上限 12 的那一刻（放在 ⑥「唤醒一个休眠条目」之前：
  // 唤醒那条路不驱逐，采样点若放在它之后 live 会变成 13，就不再是「12 视图」这个负载了）。
  // 默认不采，`MEM_BASELINE=1` 时才跑：常规门禁不该多一个测量步骤，但基线必须能在门禁脚本里
  // 就地复现（口径写死在负载旁边，别让人以后拿另一种负载去跟这组数字比）。
  if (process.env.MEM_BASELINE === '1') {
    const pool = await win.evaluate(() => window.__r2.snap())
    // 口径①：Electron 自己的 per-process 记账（主进程 app.getAppMetrics，RSS = workingSetSize）。
    const metrics = await app.evaluate(({ app }) => app.getAppMetrics())
    const tabs = metrics.filter((m) => m.type === 'Tab').map((m) => ({ pid: m.pid, mb: m.memory.workingSetSize / 1024 }))
    // 口径②：ps 读同一批 pid 的 RSS（交叉核对：两个数应当同量级，差太多说明采样时机或 pid 对不上）。
    const psOut = tabs.length ? spawnSync('ps', ['-o', 'rss=', '-p', tabs.map((t) => t.pid).join(',')], { encoding: 'utf8' }).stdout || '' : ''
    const psMB = psOut.split('\n').map((x) => parseInt(x.trim(), 10)).filter((n) => n > 0).map((kb) => kb / 1024)
    const r1 = (n) => Math.round(n * 10) / 10
    const sum = (a) => r1(a.reduce((x, y) => x + y, 0))
    const max = (a) => (a.length ? r1(Math.max(...a)) : 0)
    console.log('P09/P10 内存基线（负载 = T4 建满的视图池）')
    console.log(`  视图池：targets=${pool.n} live=${pool.live}（上限 12；sleeping=${pool.n - pool.live}）`)
    console.log(`  渲染进程数：${tabs.length}`)
    console.log(`  P09 单渲染进程最大 RSS：getAppMetrics=${max(tabs.map((t) => t.mb))}MB / ps=${max(psMB)}MB  —— 上限 150MB`)
    console.log(`  P10 全部渲染进程合计 RSS：getAppMetrics=${sum(tabs.map((t) => t.mb))}MB / ps=${sum(psMB)}MB  —— 上限 800MB`)
    console.log(`  per-pid(MB, getAppMetrics)：${tabs.map((t) => t.mb.toFixed(1)).join(', ')}`)
    console.log(`  per-pid(MB, ps)：${psMB.map((x) => x.toFixed(1)).join(', ')}`)
  }

  // ⑥ 被休眠的标签**可点回重建**（同一 id、内容可读、live +1）
  // 用户点标签的真实流程：他先切回**那个标签所属的会话**，再点标签。跨会话激活是被拒绝的
  //（口径与活条目一致，见 activateEmbedTarget），故这里必须先 embedSetSession(victim.sid)，
  // 否则断言测的是「跨会话拒绝」而不是「重建」。
  const victim = t4.sleepingInfo[0] ?? null
  const victimMarker = expectedMarkerOf(victim?.url ?? '')
  const t4b = await win.evaluate(async ([id, sid, want]) => {
    const before = await window.__r2.snap()
    if (sid) await window.desktop.embedSetSession(sid)
    const r = await window.desktop.embedActivate(id) // 用户点标签 = embedActivate
    await new Promise((res) => setTimeout(res, 1500))
    const after = await window.__r2.snap()
    const me = after.targets.find((t) => t.id === id) ?? null
    const marker = await window.__r2.markerWait(want, 8000)
    return { ok: !!r?.ok, error: r?.error ?? '', beforeLive: before.live, afterLive: after.live, me, marker, activeId: after.activeId, sameId: after.targets.filter((t) => t.id === id).length }
  }, [victim?.id ?? '', victim?.sid ?? '', victimMarker])
  check('T4 被休眠的标签可**点回重建**（embedActivate 成功、同一个 id 仍在 targets）',
    t4b.ok && t4b.sameId === 1 && !!t4b.me, JSON.stringify(t4b))
  check('T4 重建后它是**活的**（sleeping=false）且 live 数 +1',
    t4b.me?.sleeping === false && t4b.afterLive === t4b.beforeLive + 1, JSON.stringify({ me: t4b.me, before: t4b.beforeLive, after: t4b.afterLive }))
  check('T4 重建后**内容真的回来了**（读回该文件的 marker）',
    String(t4b.marker).includes(victimMarker), `want=${victimMarker} marker=${JSON.stringify(t4b.marker)}`)

  // —— T7 超限时的口径（阶段 3 翻转）：驱逐（静默）vs 保护集不可驱逐（用户点击响亮）——
  // 旧语义（R2/R3）：「背景静默 / 用户点击 toast」都是**失败**口径 —— 单轮 11~15 个可视产出
  // 必然撞上限，那时只能二选一地报错或静默丢。阶段 3 起上限由 LRU 休眠兜住：撞上限先**驱逐**
  // （背景静默、用户点击也照常成功），只有「保护集里一个可驱逐对象都没有」时才失败 ——
  // 那时用户显式点击**必须**响亮报错（静默失败是「点了没反应」，比多开一个标签糟得多），
  // 背景路径仍静默（用户没要求过这次打开，弹错是噪音）。
  console.log('T7 超限口径：LRU 驱逐（背景静默）vs 保护集不可驱逐（用户点击响亮）')
  const t7 = await win.evaluate(async ([a, b]) => {
    const m = await import('/src/store/useAppStore.ts')
    const snap = async () => {
      const st = await window.__r2.snap()
      return { n: st.n, live: st.live, sleeping: st.targets.filter((t) => t.sleeping).length }
    }
    const before = await snap()
    m.useAppStore.setState({ toast: null })
    await m.useAppStore.getState().openInBrowserTab(a, { activate: false }) // 背景（Agent 产出路径）
    const bgToast = m.useAppStore.getState().toast
    const afterBg = await snap()
    m.useAppStore.setState({ toast: null })
    await m.useAppStore.getState().openInBrowserTab(b, { activate: true }) // 用户点击
    const userToast = m.useAppStore.getState().toast
    const afterUser = await snap()
    return { bgToast: bgToast ?? null, userToast: userToast ?? null, before, afterBg, afterUser }
  }, [fileT7, fileT6])
  check('T7 背景（activate:false）超限 → **驱逐**而非失败（不弹 toast、活条目不突破上限）',
    t7.bgToast === null && t7.afterBg.live <= 12,
    JSON.stringify(t7))
  check('T7 背景超限确实驱逐了（休眠数增加 —— 不是「静默丢弃」）',
    t7.afterBg.sleeping > t7.before.sleeping, JSON.stringify(t7))
  check('T7 用户点击（activate:true）超限 → 仍成功（还有可驱逐对象就不该失败、不弹 toast）',
    t7.userToast === null && t7.afterUser.live <= 12, JSON.stringify(t7))

  // —— T7b 保护集不可驱逐 → 用户点击必须响亮报错（本阶段唯一保留的失败口径）——
  // 构造：清空真实标签 → 用**后台路径**（activate:false）连开，直到开不动。这些条目
  // lastUsed 恒为 0（AI 刚开/后台产出，用户从没看过）→ 全在保护集里 → 一个都驱逐不了。
  // 这正是「保护集内无可驱逐对象时仍然抛错」的形态（docs/RIGHT_PANEL_TABS_TASKS_PHASE3.md §2 步骤 3）。
  console.log('T7b 保护集不可驱逐：背景静默 / 用户点击响亮（唯一保留的失败口径）')
  const t7b = await win.evaluate(async ([urls]) => {
    const m = await import('/src/store/useAppStore.ts')
    const S = 'S-E2E-T7GUARD'
    m.useAppStore.setState({ activeSessionId: S })
    await window.desktop.embedSetSession(S)
    // 清空真实标签（**保留**自举 about:blank 视图：它是 Go 侧默认 target 的载体）。
    // 过滤条件是 `!== 'about:blank'`（而不是 `t.url && ...`）：URL 还没提交的活条目也要关掉 ——
    // 否则它会以「用户看过」的身份留在池里，成为下面唯一可驱逐的对象（断言就变噪声了）。
    for (const t of (await window.__r2.snap()).targets) {
      if (t.url !== 'about:blank') await window.desktop.embedCloseTarget(t.id)
    }
    const sleepBefore = (await window.__r2.snap()).targets.filter((t) => t.sleeping).length
    let seen = new Set((await window.__r2.snap()).targets.map((t) => t.id))
    const opened = []
    let failedAt = -1
    for (let i = 0; i < urls.length; i++) {
      const r = await window.desktop.embedOpenTarget(urls[i], { sid: S, activate: false }) // 后台：lastUsed 恒 0
      const st = await window.__r2.snap()
      const fresh = st.targets.map((t) => t.id).filter((id) => !seen.has(id))
      for (const id of fresh) seen.add(id)
      if (!r?.ok) { failedAt = i; break }
      opened.push(...fresh)
    }
    const stFail = await window.__r2.snap()
    const live = stFail.live
    const sleeping = stFail.targets.filter((t) => t.sleeping).length
    const leftovers = stFail.targets.filter((t) => t.id !== '' && !opened.includes(t.id)).map((t) => ({ id: t.id, url: t.url, sid: t.sid, sleeping: !!t.sleeping }))
    // 后台路径：静默（不弹 toast）
    m.useAppStore.setState({ toast: null })
    await m.useAppStore.getState().openInBrowserTab(urls[urls.length - 1], { activate: false })
    const bgToast = m.useAppStore.getState().toast
    // 用户点击路径：必须响亮
    m.useAppStore.setState({ toast: null })
    await m.useAppStore.getState().openInBrowserTab(urls[urls.length - 1], { activate: true })
    const userToast = m.useAppStore.getState().toast
    const after = await window.__r2.snap()
    return { failedAt, opened: opened.length, live, sleeping, sleepBefore, bgToast: bgToast ?? null, userToast: userToast ?? null, afterLive: after.live, afterSleeping: after.targets.filter((t) => t.sleeping).length, ids: opened, leftovers }
  }, [guardFiles.map(toFileUrl)])
  check('T7b 前提：后台连开确实撞上了「一个都驱逐不了」的墙（不是提前失败）',
    t7b.failedAt > 0 && t7b.opened === t7b.failedAt, JSON.stringify(t7b))
  check('T7b 保护集生效：这些条目一个都没被驱逐（sleeping 未增加、活条目停在上限）',
    t7b.sleeping === t7b.sleepBefore && t7b.live <= 12, JSON.stringify({ ...t7b, leftovers: t7b.leftovers }))
  check('T7b 背景（activate:false）撞「不可驱逐」→ 静默（不弹 toast）',
    t7b.bgToast === null, `toast=${JSON.stringify(t7b.bgToast)}`)
  check('T7b 用户点击（activate:true）撞「不可驱逐」→ 必须弹 toast 且含「上限」',
    !!t7b.userToast && /上限/.test(String(t7b.userToast?.msg ?? '')), `toast=${JSON.stringify(t7b.userToast)}`)
  check('T7b 失败是「明确失败」：池没被偷偷换掉（活条目与休眠数都不变）',
    t7b.afterLive === t7b.live && t7b.afterSleeping === t7b.sleeping, JSON.stringify(t7b))
  // 清理：关掉这批 guard 视图（否则后续用例也会撞上「不可驱逐」的墙 —— 测试卫生）
  const t7c = await win.evaluate(async ([ids]) => {
    for (const id of ids) await window.desktop.embedCloseTarget(id)
    const st = await window.__r2.snap()
    return { live: st.live, left: st.targets.filter((t) => t.id && ids.includes(t.id)).length }
  }, [t7b.ids])
  check('T7c 清理：guard 视图已关闭（后续用例可正常开新标签）',
    t7c.left === 0, JSON.stringify(t7c))

  // —— T8 关闭视图 → **通知 Go 侧**（阶段 3 的 P1：destroyed 单向推送）——
  // 缺口（docs/RIGHT_PANEL_TABS_PLAN.md §11.1）：`closeEmbedTarget` 只 views.delete + destroy +
  // pushEmbedNav（推渲染层），**不通知 Go 侧** → forwarder 页表留着幽灵页（list_pages 仍列它、
  // 命令路由报 target 不存在），而真关闭必须让 Go 侧 RemoveTarget（广播 targetDestroyed +
  // detachedFromTarget，后者是 Puppeteer 释放页面、browser_close_page 不再挂 300s 的唯一途径）。
  // 断言方式：直连调试代理（协议层），关掉一个视图后必须收到 {"type":"destroyed","targetId":id}；
  // 重复关闭**不得**重复广播（幂等 —— Go 侧 RemoveTarget 对未知 id 早退）。
  console.log('T8 关闭视图 → 通知 Go 侧（destroyed 单向推送）+ 幂等')
  const dbg = await connectDbgProxy(win)
  if (!dbg) {
    check('T8 调试代理可用（协议层断言的前提）', false, 'embedDebuggerProxy 不可用')
  } else {
    const t8 = await win.evaluate(async ([u]) => {
      const before = new Set((await window.__r2.snap()).targets.map((t) => t.id))
      const r = await window.desktop.embedOpenTarget(u)
      const st = await window.__r2.snap()
      const fresh = st.targets.map((t) => t.id).filter((id) => !before.has(id))
      return { ok: !!r?.ok, id: fresh[0] ?? '', url: u }
    }, [toFileUrl(fileT8)])
    check('T8 前提：新建了一个视图（拿得到它的 id）', t8.ok && !!t8.id, JSON.stringify(t8))
    // Go 侧页表（best-effort 的**前值**）：浏览器插件启用且 MCP 跟踪内嵌视图时才有值 ——
    // 有值才做「关闭后不再列出」的断言（否则是空转），没值就走日志（Go 侧链路由
    // desktop/bridge 的 TestEmbedAttachDestroyedRemovesTarget 与 plugin/browser/cdp 的单测覆盖）。
    const pagesBefore = await win.evaluate(async ([url]) => {
      const m = await import('/src/store/useAppStore.ts')
      await m.useAppStore.getState().fetchBrowserPages()
      await new Promise((r) => setTimeout(r, 1500))
      const pages = m.useAppStore.getState().browserPages ?? []
      return { n: pages.length, urls: pages.map((p) => p.url), has: pages.some((p) => p.url === url) }
    }, [t8.url])
    const from = dbg.lines.length
    await win.evaluate(async ([id]) => { await window.desktop.embedCloseTarget(id) }, [t8.id])
    const destroyed = await dbg.waitLine((l) => l.type === 'destroyed' && l.targetId === t8.id, { from })
    check('T8 关闭后主进程广播 destroyed（Go 侧据此 RemoveTarget：页表不再有幽灵页）',
      !!destroyed, `lines=${JSON.stringify(dbg.lines.slice(from).slice(0, 6))}`)
    const stGone = await win.evaluate(async ([id]) => {
      const st = await window.__r2.snap()
      return { stillListed: st.targets.some((t) => t.id === id), n: st.n }
    }, [t8.id])
    check('T8 关闭后条目也**从 targets 消失**（真关闭 ≠ 休眠：休眠仍在 targets 里）',
      stGone.stillListed === false, JSON.stringify(stGone))
    // 幂等：重复关闭同一条目不得再广播
    const from2 = dbg.lines.length
    await win.evaluate(async ([id]) => { await window.desktop.embedCloseTarget(id) }, [t8.id])
    await sleep(700)
    const dup = dbg.lines.slice(from2).filter((l) => l.type === 'destroyed' && l.targetId === t8.id)
    check('T8 幂等：重复关闭同一条目**不重复广播** destroyed（Go 侧不会被重复触发）',
      dup.length === 0, JSON.stringify(dup))
    if (pagesBefore.has) {
      const pagesAfter = await win.evaluate(async ([url]) => {
        const m = await import('/src/store/useAppStore.ts')
        await m.useAppStore.getState().fetchBrowserPages()
        await new Promise((r) => setTimeout(r, 1500))
        const pages = m.useAppStore.getState().browserPages ?? []
        return { n: pages.length, has: pages.some((p) => p.url === url) }
      }, [t8.url])
      check('T8 Go 侧页表（list_pages）关闭后不再列它',
        pagesAfter.has === false, JSON.stringify({ before: pagesBefore, after: pagesAfter }))
    } else {
      console.log(`  ⚠ T8 Go 侧页表断言跳过：本次运行 list_pages 未跟踪内嵌视图（页数 ${pagesBefore.n}）—— 由 go test 覆盖同一条链路`)
    }

    // —— T9 保护集：**刚被 AI 命令过的页**与**运行中会话的页**不得被驱逐 ——
    // 任务书步骤 2 的验收口径：「构造 9 个视图、其中 1 个刚被 AI 用过 → 被驱逐的**不是**它」。
    // 保护集（docs/RIGHT_PANEL_TABS_PLAN.md §11.3）：lastUsed===0 / activeId / 30s 内被命令过 /
    // 运行中会话的视图 / 当前会话最后一个 web 标签。这里用**受控池**把前三条与「运行中会话」
    // 都验一遍（驱逐顺序因此是确定的）：
    //   p0（最旧，但刚被 AI 命令过）→ 不得驱逐；p1（次旧，无保护）→ 必须第一个被驱逐；
    //   p3（属于「运行中」会话）→ 不得驱逐。
    console.log('T9 保护集：刚被 AI 命令过的页 / 运行中会话的页 不被驱逐（LRU 只踢最旧的）')
    const t9a = await win.evaluate(async ([u0, u1, u2, u3]) => {
      const m = await import('/src/store/useAppStore.ts')
      const S = 'S-E2E-T9'
      const SBUSY = 'S-E2E-T9BUSY'
      const open = async (url, sid) => {
        const before = new Set((await window.__r2.snap()).targets.map((t) => t.id))
        const r = await window.desktop.embedOpenTarget(url, { sid, activate: true })
        const st = await window.__r2.snap()
        return { ok: !!r?.ok, id: st.targets.map((t) => t.id).filter((id) => !before.has(id))[0] ?? '' }
      }
      // 清池（保留自举视图：Go 侧默认 target 的载体）。条件用 `!== 'about:blank'`：URL 未提交的
      // 活条目也要关（否则它会以「用户看过」的身份留下来，成为唯一可驱逐对象 → 噪声）。
      for (const t of (await window.__r2.snap()).targets) {
        if (t.url !== 'about:blank') await window.desktop.embedCloseTarget(t.id)
      }
      m.useAppStore.setState({ activeSessionId: S })
      await window.desktop.embedSetSession(S)
      const a = await open(u0, S) // 最旧
      const b = await open(u1, S) // 次旧
      const c = await open(u2, S) // 第三旧（当前激活）
      // p3 属于另一个会话，并把那个会话标记为**运行中**（口径 = store 的 busySids：
      // 有 running run 节点。按事件处理器的写法同时更新 views 与 busySids —— 订阅据此推送给主进程）
      const d = await open(u3, SBUSY)
      m.useAppStore.setState((s) => ({
        views: { ...s.views, [SBUSY]: { ...(s.views[SBUSY] ?? {}), runNodes: [{ id: 'r-e2e-t9', kind: 'run', status: 'running', depth: 0, label: 'e2e' }] } },
        busySids: new Set([SBUSY]),
      }))
      await new Promise((r) => setTimeout(r, 400)) // 等订阅把 busySids 推给主进程
      return { ids: { a: a.id, b: b.id, c: c.id, d: d.id }, ok: { a: a.ok, b: b.ok, c: c.ok, d: d.ok }, busy: [...m.useAppStore.getState().busySids] }
    }, [toFileUrl(protFiles[0]), toFileUrl(protFiles[1]), toFileUrl(protFiles[2]), toFileUrl(protFiles[3])])
    check('T9 前提：受控池建好（4 个视图）+ 运行中会话已推送给主进程',
      t9a.ok.a && t9a.ok.b && t9a.ok.c && t9a.ok.d && t9a.ids.a && t9a.busy.includes('S-E2E-T9BUSY'), JSON.stringify(t9a))
    // 让 **最旧的那个**（p0）刚被 AI 命令过（协议层真发一条命令 —— 这也是 lastCmdAt 的埋点路径）
    const cmdA = 8001
    const fromA = dbg.lines.length
    dbg.send({ type: 'cmd', id: cmdA, method: 'Runtime.evaluate', params: { expression: 'document.getElementById("marker")?.textContent ?? ""', returnByValue: true }, targetId: t9a.ids.a })
    const respA = await dbg.waitLine((l) => l.type === 'resp' && l.id === cmdA, { from: fromA, timeout: 12000 })
    check('T9 前提：对最旧的视图下了一条 AI 命令（lastCmdAt 埋点生效）',
      !!respA && !respA.error, JSON.stringify({ ids: t9a.ids, respA }))
    const t9b = await win.evaluate(async ([rest, ids]) => {
      const m = await import('/src/store/useAppStore.ts')
      const SBUSY = 'S-E2E-T9BUSY'
      const open = async (url) => {
        const before = new Set((await window.__r2.snap()).targets.map((t) => t.id))
        const r = await window.desktop.embedOpenTarget(url, { sid: SBUSY, activate: true })
        const st = await window.__r2.snap()
        return { ok: !!r?.ok, id: st.targets.map((t) => t.id).filter((id) => !before.has(id))[0] ?? '' }
      }
      const opened = []
      for (const u of rest) {
        const r = await open(u)
        if (!r.ok) break
        opened.push(r.id)
      }
      const st = await window.__r2.snap()
      const byId = (id) => st.targets.find((t) => t.id === id) ?? null
      return {
        sleeping: st.targets.filter((t) => t.sleeping).map((t) => t.id),
        live: st.live,
        aSleeping: !!byId(ids.a)?.sleeping, bSleeping: !!byId(ids.b)?.sleeping, dSleeping: !!byId(ids.d)?.sleeping,
        openedCount: opened.length,
        lastOpenedSleeping: opened.length ? !!byId(opened[opened.length - 1])?.sleeping : null,
        busy: [...m.useAppStore.getState().busySids],
      }
    }, [protFiles.slice(4).map(toFileUrl), t9a.ids])
    check('T9 前提：连开触发了驱逐（sleeping ≥ 1，否则下面的断言是空转）',
      t9b.sleeping.length >= 1, JSON.stringify({ ids: t9a.ids, t9b }))
    check('T9 保护集生效：**最旧**的那个（刚被 AI 命令过）**没有**被驱逐',
      t9b.aSleeping === false, JSON.stringify({ ids: t9a.ids, sleeping: t9b.sleeping }))
    check('T9 而次旧的那个被驱逐了（证明「没被驱逐」是保护集生效，不是驱逐没发生）',
      t9b.bSleeping === true, JSON.stringify({ ids: t9a.ids, sleeping: t9b.sleeping }))
    check('T9 运行中会话（busySids）的视图没被驱逐',
      t9b.dSleeping === false && t9b.busy.includes('S-E2E-T9BUSY'), JSON.stringify({ ids: t9a.ids, sleeping: t9b.sleeping, busy: t9b.busy }))
    check('T9 刚打开的视图仍活着（LRU 只踢最旧的）',
      t9b.lastOpenedSleeping === false, JSON.stringify({ ids: t9a.ids, t9b }))

    // T9c：**命令打到休眠条目 → 自动唤醒后执行**（不是「target 不存在」）。
    // 为什么必须有：休眠条目仍在 Go 的页表里（list_pages 列它），AI 随时可能命令它 ——
    // 直接回「target 不存在」等于让 Agent 的既有页表突然失效（用户口径：驱逐只该影响展示，
    // 不该影响 Agent 运行时）。故主进程命中休眠条目时先重建（沿用原 id）再重放命令。
    const evictedId = t9b.sleeping.find((id) => id && id !== t9a.ids.a && id !== t9a.ids.d) ?? ''
    const cmdId = 9001
    const fromCmd = dbg.lines.length
    dbg.send({ type: 'cmd', id: cmdId, method: 'Runtime.evaluate', params: { expression: 'document.getElementById("marker")?.textContent ?? ""', returnByValue: true }, targetId: evictedId })
    const resp = await dbg.waitLine((l) => l.type === 'resp' && l.id === cmdId, { from: fromCmd, timeout: 15000 })
    const value = resp?.result?.result?.value ?? resp?.result?.value ?? resp?.value ?? ''
    check('T9c AI 的命令打到**休眠**条目 → 自动唤醒并成功响应（不是 target 不存在）',
      !!resp && !resp.error && String(value).includes('PREVIEW-PROT'), JSON.stringify({ evictedId, resp, value }))
    const revived = await win.evaluate(async ([id]) => {
      const st = await window.__r2.snap()
      const me = st.targets.find((t) => t.id === id) ?? null
      return { sleeping: me?.sleeping, stillThere: !!me }
    }, [evictedId])
    check('T9c 唤醒后条目回到**活的**（sleeping=false）且 id 不变（标签身份不丢）',
      revived.stillThere === true && revived.sleeping === false, JSON.stringify({ evictedId, revived }))

    // T9d 清理：解除 busySids 保护 + 关掉本组造的视图（否则后面的用例会撞上「不可驱逐」的墙，
    // 断言会变成空转 —— 测试卫生，不是被测语义）。
    const t9d = await win.evaluate(async () => {
      const m = await import('/src/store/useAppStore.ts')
      m.useAppStore.setState((s) => {
        const views = { ...s.views }
        delete views['S-E2E-T9BUSY']
        return { views, busySids: new Set() }
      })
      await new Promise((r) => setTimeout(r, 300)) // 等订阅把「无运行中会话」推给主进程
      for (const t of (await window.__r2.snap()).targets) {
        if (t.url !== 'about:blank') await window.desktop.embedCloseTarget(t.id)
      }
      const st = await window.__r2.snap()
      return { live: st.live, busy: [...m.useAppStore.getState().busySids] }
    })
    check('T9d 清理：保护集解除 + 视图已关闭（留给后续用例干净的池）',
      t9d.live <= 1 && t9d.busy.length === 0, JSON.stringify(t9d))
  }
  if (dbg) dbg.sock.destroy()

  // —— T6 预览**不收起左栏**（真链路：nav 事件路径）——
  // 为什么必须在真链路验：store 层的 e2e:auto 只覆盖 autoOpenVisualFile 直接调用的效果；真链路里
  // 还有一条**异步**路径会动 leftExpanded —— 预览视图加载出首个真实页面时，onEmbedNav 会调
  // focusWebTabIfClosed()，它以前无条件走 focusBrowser()（含 set({leftExpanded:false})，那是
  // 「Browser Use 接管视界」的语义）→ 用户预览一次，左栏被静默收起。R3 按来源区分
  // （previewOwnedViews）：预览打开的视图 → focusBrowserForPreview()（不收左栏）。
  //
  // 两个「防空转」的要点（缺一个这条断言就永远是绿的）：
  //   ① 用 activate:false 触发 —— 面板不动（仍非 web tab），focusWebTabIfClosed 的早退守卫
  //      `rightExpanded && rightTab==='web'` 才不成立；用 activate:true 面板已切 web → 直接早退。
  //   ② 换到**全新会话** —— onEmbedNav 的「每会话只自动展开一次」守卫按会话 id 去重，复用旧会话
  //      会让 focusWebTabIfClosed 根本不被调用。
  console.log('T6 预览不收起左栏（真链路：nav 事件路径）')
  const t6 = await win.evaluate(async ([u]) => {
    const m = await import('/src/store/useAppStore.ts')
    // 腾出池位（T4 已把池填到上限 8）
    const ids = ((await window.desktop.embedStatus())?.targets ?? []).map((t) => t.id)
    for (const id of ids.slice(1, 3)) await window.desktop.embedCloseTarget(id)
    // 全新会话 + 复位到「面板收起、不在 web tab、左栏展开」→ 早退守卫不成立
    const S = 'S-E2E-T6'
    m.useAppStore.setState({ activeSessionId: S, rightTab: 'activity', rightExpanded: false, leftExpanded: true })
    await window.desktop.embedSetSession(S)
    const before = await window.__r2.panel()
    await window.__r2.call('openInBrowserTab', [u, { activate: false }])
    await new Promise((r) => setTimeout(r, 2000)) // 等 file:// 加载出真实 URL → 主进程推 embed-nav
    return { before, after: await window.__r2.panel() }
  }, [fileT6])
  check('T6 真链路：预览（后台标签）后左栏仍展开 —— 不被 focusBrowser 收起',
    t6.after.left === true,
    `left ${JSON.stringify(t6.before.left)} → ${JSON.stringify(t6.after.left)}；panel=${JSON.stringify(t6.after)}`)

  // —— T11 标签语义（2026-09 用户口径）：「打开过的文件：刷新已存在的标签；没开过：新开标签」——
  // 与 T2/T3 的差别：那两条只证明「第二次命中不新建」，**没有**证明「内容跟到了最新」——旧实现
  // 命中即只 embedActivate（切过去看的是**上一次**的内容），于是 Agent 反复写同一个产出文件时，
  // 用户点开的是过期结果。本组把「刷新」也钉住：
  //   ① 池**不增长**（同一个标签，不是每预览一次多一个）；
  //   ② 内容**真的**换成了新 marker（用 markerWait 等**新**值：marker() 读到含 PREVIEW 的旧内容
  //      就返回，用它验刷新永远绿 = 空转）。
  console.log('T11 同一 file:// 文件预览两次 → 池不增长 + 内容确实刷新（同一个标签）')
  // 先清池（只留自举视图）：让「池不增长」与「file:// 视图恰好 1 个」不被前面用例的残留满足。
  await win.evaluate(async () => {
    const st = await window.desktop.embedStatus()
    for (const t of st?.targets ?? []) {
      if (t.url && t.url !== 'about:blank') await window.desktop.embedCloseTarget(t.id)
    }
  })
  await sleep(500)
  const t11 = await win.evaluate(async ([f]) => {
    // 面板复位：让「展开 + 切 web」只可能来自本次 action（展开/切 tab 不是本组断言点，只为稳定）。
    await window.__r2.resetPanel()
    const n0 = (await window.__r2.snap()).n
    await window.__r2.call('openHtmlInBrowser', [f]) // 用户点击入口（activate:true）
    await new Promise((r) => setTimeout(r, 1200))
    const s1 = await window.__r2.snap()
    const markerA = await window.__r2.markerWait('PREVIEW-T11-A')
    return { n0, n1: s1.n, active1: s1.activeId, targets1: s1.targets, markerA }
  }, [fileT11])
  // 改写同一文件的内容 → 第二次预览必须让**那个**标签读到新内容（典型场景：Agent 又写了一遍产出）
  fs.writeFileSync(fileT11, markerHtml('PREVIEW-T11-B'))
  const t11b = await win.evaluate(async ([f, a1]) => {
    await window.__r2.resetPanel()
    await window.__r2.call('openInBrowserTab', [f, { activate: true }]) // 命中已有标签（去重）
    await new Promise((r) => setTimeout(r, 1200))
    const s2 = await window.__r2.snap()
    const markerB = await window.__r2.markerWait('PREVIEW-T11-B')
    return { n2: s2.n, active2: s2.activeId, targets2: s2.targets, markerB, sameId: s2.activeId === a1 }
  }, [fileT11, t11.active1])
  const t11aViews = t11.targets1.filter((t) => t.url === toFileUrl(fileT11))
  const t11bViews = t11b.targets2.filter((t) => t.url === toFileUrl(fileT11))
  check('T11 首次预览新建标签（池 +1）', t11.n1 === t11.n0 + 1, JSON.stringify({ n0: t11.n0, n1: t11.n1 }))
  check('T11 首次预览读到 marker A', t11.markerA.includes('PREVIEW-T11-A'), `markerA=${JSON.stringify(t11.markerA)}`)
  check('T11 第二次预览**不新建**标签（池大小不变）', t11b.n2 === t11.n1, JSON.stringify({ n1: t11.n1, n2: t11b.n2 }))
  check('T11 命中的是**同一个** target（activeId 未换）', t11b.sameId === true, JSON.stringify({ a1: t11.active1, a2: t11b.active2 }))
  check('T11 该文件在池中只有 1 个标签（没有堆重复标签）',
    t11aViews.length === 1 && t11bViews.length === 1, JSON.stringify({ t11aViews, t11bViews }))
  check('T11 内容**确实刷新**（同一标签读到改写后的 marker B）',
    t11b.markerB.includes('PREVIEW-T11-B'),
    `markerB=${JSON.stringify(t11b.markerB)}（仍是旧值 = 只激活没刷新）`)

  // —— T13 activate:false 的刷新：不抢视线，但内容要更新（且作用到**后台**标签）——
  // 为什么单列：Agent 产出走的是 activate:false（ADR-2「不抢视线」），而刷新动作本身也可能
  // 顺手切面板/改激活态。这里把被测标签**退到后台**（先在前台开另一个文件 —— Agent 产出的真实
  // 处境是「用户正看着别的东西」），然后验：
  //   ① 刷新确实发生（切回去读到 marker C —— 只有真 reload 过才会是 C；activate:false 下也就
  //      只能经 embed:reload-target 这条**按 id** 的通路）；
  //   ② activeId 不变（前台的还是那个别的文件）、面板不展开/不切 tab、左栏不收。
  console.log('T13 activate:false 下的刷新（后台标签）：不改 activeId、不展开/不切面板、不收左栏')
  fs.writeFileSync(fileT11, markerHtml('PREVIEW-T11-C'))
  const t13 = await win.evaluate(async ([f, other, fUrl]) => {
    const m = await import('/src/store/useAppStore.ts')
    // 复位到「面板收起 + 不在 web tab + 左栏展开」。必须走 store 的 action：rightTab/rightExpanded
    // 是**会话级**视图状态，raw setState 只改顶层镜像 —— App 的 onResize 用 setRightExpanded 镜像
    // 布局时会触发 setView，把顶层 rightTab 一起从会话态拉回旧值（实测：复位 10ms 后 tab 自己
    // 变成 'web'）→ p0 就成了噪声，断言变成「拿噪声比噪声」。
    const resetPanel = async () => {
      m.useAppStore.getState().setRightTab('activity')
      m.useAppStore.getState().setRightExpanded(false)
      m.useAppStore.setState({ leftExpanded: true })
      await new Promise((r) => setTimeout(r, 500)) // 等布局观察者镜像一次（可能再报一次 resize）
    }
    await resetPanel()
    // 先在前台开**另一个**文件 → 被测标签退到后台
    await window.__r2.call('openInBrowserTab', [other, { activate: true }])
    await new Promise((r) => setTimeout(r, 1500)) // 等预览的 nav 事件（含每会话一次的自动展开）落定
    await resetPanel() // 再复位一次：上一步 activate:true 会切 web/展开
    const before = await window.__r2.snap()
    const p0 = await window.__r2.panel()
    const bgId = before.targets.find((t) => t.url === fUrl)?.id ?? ''
    await window.__r2.call('openInBrowserTab', [f, { activate: false }]) // Agent 产出路径（后台刷新）
    await new Promise((r) => setTimeout(r, 1200))
    const after = await window.__r2.snap()
    const p1 = await window.__r2.panel()
    // 最后切回被测标签读内容（embedCommand 只作用激活视图）—— 刷新没发生的话这里读到的是 marker B
    await window.desktop.embedActivate(bgId)
    const marker = await window.__r2.markerWait('PREVIEW-T11-C')
    return {
      n0: before.n, n1: after.n, activeBefore: before.activeId, activeAfter: after.activeId,
      bgId, bgWasHidden: before.activeId !== bgId, p0, p1, marker,
      fileViews: after.targets.filter((t) => String(t.url || '').startsWith('file://')).length,
    }
  }, [fileT11, fileT7, toFileUrl(fileT11)])
  // 前提（防空转）：基线真的复位成功 + 被测标签真的在后台 + 确实找得到它的 id
  check('T13 前提：面板已复位（非 web tab + 收起 + 左栏展开）',
    t13.p0.tab === 'activity' && t13.p0.expanded === false && t13.p0.left === true, JSON.stringify(t13.p0))
  check('T13 前提：被测标签在**后台**（激活的是另一个标签）',
    t13.bgWasHidden === true && !!t13.bgId, JSON.stringify({ bgId: t13.bgId, active: t13.activeBefore }))
  check('T13 activate:false 刷新**不新建**标签（池大小不变）', t13.n1 === t13.n0, JSON.stringify({ n0: t13.n0, n1: t13.n1 }))
  check('T13 activate:false **不改激活态**（activeId 不变）',
    t13.activeAfter === t13.activeBefore, JSON.stringify({ before: t13.activeBefore, after: t13.activeAfter }))
  check('T13 activate:false **不展开也不切面板**（rightTab/rightExpanded 不变）',
    t13.p1.tab === t13.p0.tab && t13.p1.expanded === t13.p0.expanded, JSON.stringify({ p0: t13.p0, p1: t13.p1 }))
  check('T13 activate:false **不收左栏**（leftExpanded 保持 true）',
    t13.p1.left === true, JSON.stringify(t13.p1))
  check('T13 **后台**标签的内容仍**确实刷新**（切回去读到 marker C）',
    t13.marker.includes('PREVIEW-T11-C'),
    `marker=${JSON.stringify(t13.marker)}（旧值 = 后台标签没刷新）`)

  // —— T12 srcPath 去重（不依赖真转换）——
  // office 转换产物每次是**新的临时 html_path** → URL 认不出「同一个文件」，于是同一份 xlsx 每预览
  // 一次就多一个重复标签（本仓记录的「未决 8」）。故去重身份取**源文件路径**：同一个 srcPath 第二次
  // 打开 → 复用那个标签并**导航**到新产物（换个 URL 也不新建）。这里用两个普通 file:// 直接验
  // 主进程那条通路（不需要跑 Python 转换，断言的对象是「srcPath 去重 + 导航」本身）。
  console.log('T12 srcPath 去重：同一 srcPath 第二次打开 → 复用同一个标签并导航到新 URL')
  const t12 = await win.evaluate(async ([u1, u2]) => {
    const m = await import('/src/store/useAppStore.ts')
    const S = 'S-E2E-T12' // 独立会话：不影响前面用例的会话，也不被它们影响
    m.useAppStore.setState({ activeSessionId: S })
    await window.desktop.embedSetSession(S)
    const before = (await window.__r2.snap()).n
    const srcPath = 'e2e-src-shared'
    const r1 = await window.desktop.embedOpenTarget(u1, { sid: S, srcPath })
    await new Promise((r) => setTimeout(r, 900))
    const n1 = (await window.__r2.snap()).n
    const r2 = await window.desktop.embedOpenTarget(u2, { sid: S, srcPath })
    await new Promise((r) => setTimeout(r, 900))
    const st = await window.__r2.snap()
    return {
      before, n1, n2: st.n, ok1: !!r1?.ok, ok2: !!r2?.ok, err: r2?.error ?? '',
      shared: st.targets.filter((t) => t.srcPath === srcPath),
    }
  }, [toFileUrl(fileT12A), toFileUrl(fileT12B)])
  check('T12 第一次打开新建标签（池 +1）', t12.ok1 && t12.n1 === t12.before + 1, JSON.stringify({ before: t12.before, n1: t12.n1, ok1: t12.ok1 }))
  check('T12 第二次（同 srcPath、新 URL）**不新建**（池大小不变）',
    t12.n2 === t12.n1, JSON.stringify({ n1: t12.n1, n2: t12.n2, err: t12.err }))
  check('T12 该 srcPath 在池中只有 1 个条目', t12.shared.length === 1, JSON.stringify(t12.shared))
  check('T12 复用的是同一个标签且**已导航到新 URL**',
    t12.shared.length === 1 && t12.shared[0].url === toFileUrl(fileT12B), JSON.stringify(t12.shared))
  // —— T15 覆盖层弹窗打开 → 原生视图必须隐藏（用户报的真实缺陷）——
  // 机制：`.modal-hint` 是 `position:fixed; inset:0; z-index:100` + 居中 → 弹窗按**整个窗口**居中，
  // 卡片 360px（compact-card max 420px）必然压在右栏区域内；而原生 WebContentsView 是 Electron 的
  // 原生层，**恒合成在窗口 DOM 之上**（DOM 的 z-index 再高也压不住它）→ 弹窗右半被预览画面盖住
  //（用户截图：右侧文字被切、按钮区被压）。
  // 旧实现靠**手维护的状态名单**（inSettings/inSkills/inCron/showNewWsModal/question/release）判定，
  // Composer 的「压缩上下文？」确认弹窗不在名单里 → 打开它时视图根本没隐藏。
  // 本用例钉的是**通用机制**（按 DOM 判定「有覆盖层压在右栏上」），所以对以后新增的任何
  // .modal-hint 弹窗都成立 —— 不是只给 CompactConfirm 打补丁。
  console.log('T15 覆盖层弹窗（.modal-hint）打开 → 原生视图必须隐藏（否则弹窗被原生层盖住）')
  // 准备：一个可见的预览（唯一 URL，避免命中去重导致池不增长）
  await win.evaluate(async ([u]) => {
    const m = await import('/src/store/useAppStore.ts')
    m.useAppStore.setState({ rightExpanded: true })
    await window.__r2.call('openHtmlInBrowser', [u])
  }, [toFileUrl(fileT15)])
  await waitFor(async () => {
    const st = await win.evaluate(() => window.__r2.snap())
    return st.targets.some((t) => t.visible)
  }, { label: 'T15 前提：预览可见', timeout: 30000 })
  const t15a = await win.evaluate(() => window.__r2.snap())
  check('T15 前置：预览可见（visible=true）', t15a.targets.some((t) => t.visible), JSON.stringify(t15a.targets))

  // 走**真实点击路径**：点 Composer 的上下文占比 chip → 打开「压缩上下文？」确认弹窗
  const t15CtxCount = await win.locator('.ctx').count()
  check('T15 前置：找得到 Composer 的上下文 chip（.ctx）', t15CtxCount > 0, `count=${t15CtxCount}`)
  await win.locator('.ctx').first().click()
  await waitFor(async () => (await win.locator('.modal-hint.open').count()) > 0,
    { label: 'T15 弹窗已打开', timeout: 10000 })
  await sleep(400) // 隐藏要走 IPC + 主进程 setVisible，给一拍
  const t15b = await win.evaluate(() => window.__r2.snap())
  check('T15 弹窗打开期间原生视图已隐藏（弹窗不会被原生层盖住）',
    t15b.targets.length > 0 && !t15b.targets.some((t) => t.visible), JSON.stringify(t15b.targets))

  // 反向：关闭弹窗后视图必须**恢复显示** —— 否则「打开过一次弹窗」= 右栏永久黑掉
  await win.locator('.modal-hint.open .acts .btn:not(.primary)').first().click()
  await waitFor(async () => (await win.locator('.modal-hint.open').count()) === 0,
    { label: 'T15 弹窗已关闭', timeout: 10000 })
  await waitFor(async () => {
    const st = await win.evaluate(() => window.__r2.snap())
    return st.targets.some((t) => t.visible)
  }, { label: 'T15 弹窗关闭后视图恢复显示', timeout: 15000 })
  const t15c = await win.evaluate(() => window.__r2.snap())
  check('T15 弹窗关闭后原生视图恢复显示', t15c.targets.some((t) => t.visible), JSON.stringify(t15c.targets))
  // —— T16 收起右栏不得把原生视图缩成 80×80、把页面排版视口钉死 ——
  // 用户报障（2026-09-21）：「图片、网页等无法正常加载」，截图里整块面板只剩左上角一小块内容、
  // 页面底色铺满（页面按 80px 宽排版 = 只看得到左上角）。真机 trace 定位到源头在**收起**这一步：
  // BrowserTab 卸载，而 ResizeObserver 对**已脱离文档**的元素还会补发一次「尺寸 0」的通知 →
  // 旧实现把它钳成 `Math.max(80, …)` 下发：`embed:resize` 把**所有**原生视图缩到 80×80，
  // `Emulation.setDeviceMetricsOverride` 更把**当前显示那一页**的排版视口钉在 80×80。
  // 修法：渲染层「量不到就什么都**不**下发」（RightPanel 的 embedRectOf；隐藏交给 embedHide），
  // 并让主进程在「谁成为当前显示的页」时对齐一次视口（embed.ts 的 alignEntryViewport）。
  // 本用例钉住**可观测后果**：收起后视图几何不得被缩、页面的排版视口不得变成 80。
  console.log('T16 收起右栏不得缩视图 / 不得把页面排版视口钉成 80×80')
  await win.evaluate(async ([u]) => {
    const m = await import('/src/store/useAppStore.ts')
    m.useAppStore.setState({ rightExpanded: true })
    await window.__r2.call('openHtmlInBrowser', [u])
  }, [toFileUrl(fileT16)])
  const t16shown = await waitFor(async () => {
    const st = await win.evaluate(() => window.__r2.snap())
    return st.targets.some((t) => t.visible) ? st : null
  }, { label: 'T16 前提：预览可见', timeout: 30000 })
  const vis16 = t16shown.targets.find((t) => t.visible)
  // 量「显示中那一页」的排版视口（window.innerWidth/Height）与它的原生 bounds
  const t16Measure = async (url) => app.evaluate(async ({ BrowserWindow }, arg) => {
    const w = BrowserWindow.getAllWindows()[0]
    const kids = (w.contentView?.children ?? []).filter((c) => c.webContents && typeof c.webContents.getURL === 'function')
    const v = kids.find((c) => c.webContents.getURL() === arg)
    if (!v) return null
    let inner = null
    try { inner = await v.webContents.executeJavaScript('[window.innerWidth, window.innerHeight]', true) } catch { /* 未就绪 */ }
    return { bounds: v.getBounds(), zoom: w.webContents.getZoomFactor(), inner }
  }, url)
  const t16Before = await t16Measure(vis16.url)
  check('T16 前置：显示中页面的排版视口 = 容器（基线本来就该成立）',
    !!t16Before?.inner && Math.abs(t16Before.inner[0] - Math.round(t16Before.bounds.width / t16Before.zoom)) <= 4,
    JSON.stringify(t16Before))
  // 走**真实动作**收起右栏（用户点折叠按钮/⌘ 快捷键走的就是它）
  await win.evaluate(async () => {
    const m = await import('/src/store/useAppStore.ts')
    m.useAppStore.getState().setRightExpanded(false)
  })
  await sleep(1500)
  const t16All = await app.evaluate(({ BrowserWindow }) => {
    const w = BrowserWindow.getAllWindows()[0]
    return (w.contentView?.children ?? [])
      .filter((c) => c.webContents && typeof c.webContents.getURL === 'function')
      .map((c) => ({ url: c.webContents.getURL(), bounds: c.getBounds() }))
      .filter((x) => x.url && x.url !== 'about:blank')
  })
  const t16Tiny = t16All.filter((x) => x.bounds.width <= 100 || x.bounds.height <= 100)
  check('T16 收起右栏后原生视图**没有**被缩成 80×80（旧实现：量不到 → 下发假矩形）',
    t16All.length > 0 && t16Tiny.length === 0, JSON.stringify(t16All))
  const t16AfterCollapse = await t16Measure(vis16.url)
  check('T16 收起期间页面的排版视口也没被钉死（旧实现此刻被写成 80×80）',
    !!t16AfterCollapse?.inner && t16AfterCollapse.inner[0] > 200, JSON.stringify(t16AfterCollapse?.inner ?? null))
  // 反向：再展开 → 那一页的排版视口仍必须 = 容器（否则用户回来看到的就是「只剩一小块」）
  await win.evaluate(async () => {
    const m = await import('/src/store/useAppStore.ts')
    m.useAppStore.getState().setRightExpanded(true)
  })
  await sleep(1500)
  const t16Back = await t16Measure(vis16.url)
  const exp16 = t16Back ? Math.round(t16Back.bounds.width / t16Back.zoom) : 0
  check('T16 展开回来后排版视口 = 容器（用户看到的不再是「只剩左上角一小块」）',
    !!t16Back?.inner && t16Back.inner[0] > 200 && Math.abs(t16Back.inner[0] - exp16) <= 4,
    JSON.stringify({ inner: t16Back?.inner ?? null, expected: exp16, bounds: t16Back?.bounds ?? null }))
} catch (e) {
  fails.push(`异常：${e.message}`)
  console.log(`  ❌ 异常：${e.message}`)
  if (viteErr) console.log(`[vite stderr] ${viteErr.slice(-600)}`)
} finally {
  if (app) await app.close().catch(() => { /* 已退出 */ })
  cleanup()
}

console.log(`\n${fails.length === 0 ? 'ALL PASS' : `FAILED (${fails.length}): ${fails.join('; ')}`}`)
process.exit(fails.length === 0 ? 0 : 1)
