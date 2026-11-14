// PDF 预览端到端验证 + 右栏预览标签条（R4）行为回归。
//
// 第一版（2026-09-18 上午）只钉住了「PDF 能进右侧栏并渲染」；当天下午按用户口径改了两件事，
// 本脚本随之扩展：
//   ① **打开即见内容**：可视类文件（pdf/xlsx/docx/pptx/图片/html/svg）从文件栏/对话里点开时，
//      不再停在「这是二进制文件」+「预览/用默认程序打开」两个按钮的中间态，而是**直接打开预览**。
//      （旧的中间态断言已按新口径**翻转**：见 T1 —— 没有按钮，且右栏已经切到预览。）
//   ② **标签条按原型重做**（docs/demo/right-panel-preview.html 的 .pv-strip/.pv-tab）：
//      标签只写文件名（file:// 的 title 常为空，此前退化成整条百分号编码 URL）、
//      未读点（后台打开、用户还没看过）替代类型图标、hover 才露 ✕、超 5 个收窄 + 「+N」、
//      role=tablist/tab + aria-selected、右键菜单（关闭/关闭其他/关闭右侧/复制路径/在访达中显示/
//      用默认程序打开）。
//
// 覆盖（全部走真链路：真 Electron + 真 preload/IPC + 真 WebContentsView + 真 Chromium）：
//   T1 打开即见内容：openFile(pdf) → **没有**中间态（无提示段落、无「预览 PDF」按钮），且右栏已切预览
//   T2 视图池新增该 PDF（URL 编码正确、归属当前会话）
//   T3 视图几何 = 右栏内容容器（.browser-shot）的矩形（±2px）且贴窗口右缘
//   T4 真渲染：CDP 读到 contentType=application/pdf + 全尺寸 <embed>；capturePage 像素里
//      「纸张」占比 > 5%（暗色主题下「非白像素」会被背景色骗过 —— 第一版就踩过这个坑）
//   T5 标签条（新 UI）：文件名标题、aria-selected、URL 在 title 里
//   T6 面板语义：切到内容标签 + 展开右栏 + 不收左栏（M4 回归点，基线显式置为展开）
//   T7 未读点：后台打开（activate:false）→ 标签出现未读点；点击该标签 → 未读点消失
//   T8 右键菜单：复制文件路径（回读真剪贴板）+ 关闭右侧所有标签（批量关闭真的生效）
//   T9 两标签并存（缺陷 A 回归：预览 B 不顶掉 A）
//   T10 点 ✕ → 视图真销毁，另一个不受影响
//   T14 面板级下拉菜单不被原生视图盖住（菜单打开期间 hide）+ 菜单锚点
//   T15「＋ → 网页」在池里没有真实页面时也要真的开出标签（自举 about:blank 载体不当身份）
//   T16 收起右栏后再切回 → 显示中页面的排版视口不得被钉成 80×80（预览只剩左上角一小块的根因）
//
// 用法：cd desktop/app && npm run e2e:pdf
//   PDF_E2E_SRC=...      指定 PDF（默认取仓库 desktop/ 下那份真实报告）
//   PDF_E2E_SHOT_DIR=... 截图输出目录（脚本会打印路径供人眼复核）
//   GO_CODE_E2E_FORCE_BUILD=1 → 强制 npm run build（改了 electron/ 或 src/ 后必须）
// 退出码：全通过 0；断言失败 1；**前置不满足 2**（夹具不在 / 端口被占 / 服务没起来）；
//         缺 embed API（老产物）skip 0。
// 夹具：默认取仓库 desktop/ 下那份真实报告；它**不在版本库里**（见看板 R-17），
//       所以默认环境一定要用 PDF_E2E_SRC 指一份真实 PDF（中文名保留）。
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
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'pdf-e2e-home-'))
const WS = fs.mkdtempSync(path.join(os.tmpdir(), 'pdf-e2e-ws-'))
const SHOTS = process.env.PDF_E2E_SHOT_DIR || fs.mkdtempSync(path.join(os.tmpdir(), 'pdf-e2e-shot-'))
fs.mkdirSync(SHOTS, { recursive: true })
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
      s.on('error', () => { clearTimeout(timer); s.destroy(); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error(`vite dev server 未就绪（port ${port}）`)
    await sleep(250)
  }
}

async function waitFor(fn, { timeout = 60000, interval = 250, label = '条件' } = {}) {
  const end = Date.now() + timeout
  let last
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v; last = v } catch (e) { last = e?.message }
    await sleep(interval)
  }
  throw new Error(`等待超时: ${label}（最后: ${JSON.stringify(last)}）`)
}

// —— 构建产物（主进程跑 dist-electron/main.js；renderer 由 dev server 提供源码）——
const index = path.join(APP, 'dist', 'index.html')
const mainJs = path.join(APP, 'dist-electron', 'main.js')
if (process.env.GO_CODE_E2E_FORCE_BUILD === '1' || !fs.existsSync(index) || !fs.existsSync(mainJs)) {
  const b = spawnSync('npm', ['run', 'build'], { cwd: APP, encoding: 'utf8' })
  if (b.status !== 0) throw new Error(`npm run build 失败:\n${b.stdout}\n${b.stderr}`)
}
// bridge：真会话（new_session）与文件预览（file_preview）都要经 Go bridge → 现构建。
const build = spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], { cwd: REPO, encoding: 'utf8' })
if (build.status !== 0) throw new Error(`go build 失败:\n${build.stderr}`)

// —— 夹具：把**真实**那份 PDF 拷进隔离工作区（保留中文名 → 顺带钉住 file:// 的 URI 编码）——
const PDF_SRC = process.env.PDF_E2E_SRC || path.join(REPO, 'desktop', '百工2026年上半年经营分析.pdf')
if (!fs.existsSync(PDF_SRC)) {
  // 夹具缺失 = 前置不满足，**不是**断言失败（00 §1.9 退出码三档 / 04 F47）：
  // 退 2 并在消息里给出下一步，避免被读成「PDF 预览坏了」。
  console.error(`\n⛔ 前置不满足（退出码 2，不是测试失败）：PDF 夹具不存在 —— ${PDF_SRC}`)
  console.error('   用 PDF_E2E_SRC=/path/to/真实.pdf 指一份（保留中文名，顺带钉住 file:// 的 URI 编码）')
  process.exit(2)
}
const PDF_NAME = path.basename(PDF_SRC)
const pdfSize = fs.statSync(PDF_SRC).size
fs.copyFileSync(PDF_SRC, path.join(WS, PDF_NAME))
// T9 用的第二个可视文件（内容可区分）
const HTML_NAME = 'preview-second.html'
fs.writeFileSync(path.join(WS, HTML_NAME), '<html><body><h1 id="marker">SECOND-MARKER</h1></body></html>')
const toFileUrl = (p) => 'file://' + p.split('/').map(encodeURIComponent).join('/')
const pdfUrl = toFileUrl(path.join(WS, PDF_NAME))
const htmlUrl = toFileUrl(path.join(WS, HTML_NAME))

// —— vite dev server（renderer 走源码：e2e 才能 import 到与真应用**同一个** store 实例）——
const DEV_PORT = Number(process.env.PDF_E2E_PORT || 5393)
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
    // HAI_DATA_DIR：多实例模式（跳过单实例锁 + userData/配置根随 HOME 隔离）——
    // 本脚本绝不碰用户正在跑的那个实例的状态。
    env: { ...process.env, HOME, HAI_DATA_DIR: HOME, VITE_DEV_SERVER_URL: `http://localhost:${DEV_PORT}/` },
  })
  const win = await app.firstWindow()
  win.on('console', (m) => { if (m.type() === 'error' && !/favicon/.test(m.text())) console.log('[renderer-err]', m.text()) })
  await win.waitForLoadState('domcontentloaded')
  // 注入隔离工作区（userData 已随 HAI_DATA_DIR 隔离）→ reload 跳过选目录
  await win.evaluate(([ws]) => { localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws })) }, [WS])
  await win.reload()

  // —— 就绪判定用 bridge 状态（语言无关；比找「设置」按钮稳）——
  const store = async (expr) => win.evaluate(`(async () => {
    const m = await import('/src/store/useAppStore.ts')
    const s = m.useAppStore.getState()
    return ${expr}
  })()`)
  await waitFor(() => store('s.bridgeStatus === "running"'), { label: 'bridge 就绪（bridgeStatus=running）', timeout: 90000 })
  console.log(`  夹具: ${PDF_NAME}（${(pdfSize / 1024).toFixed(0)} KB）→ ${WS}`)

  const api = await win.evaluate(() => {
    const d = window.desktop
    return { status: !!d?.embedStatus, open: !!d?.embedOpenTarget, activate: !!d?.embedActivate, close: !!d?.embedCloseTarget }
  })
  if (!api.status || !api.open || !api.activate || !api.close) {
    console.log(`  ⚠ 缺少 embed API，跳过（${JSON.stringify(api)}）`)
    console.log('\nSKIPPED (embed api unavailable)')
    process.exit(0)
  }

  // 会话：空工作区按规格不自动建会话 → 用真应用的 newSession（左栏「新建会话」同一 action）。
  if (!(await store('s.activeSessionId || ""'))) {
    await store('(async () => { await s.newSession(); return true })()')
  }
  const SID = await waitFor(() => store('s.activeSessionId || ""'), { label: '会话创建（activeSessionId 回填）', timeout: 60000 })
  check('真会话就绪（预览带会话归属）', !!SID, `sid=${JSON.stringify(SID)}`)

  // M4 回归点需要一个**有效基线**：左栏先显式展开（否则「预览没收起左栏」这类断言在已收起的
  // 基线上恒真，等于空转 —— 第一版就这么空转过一次）。
  await win.evaluate(`(async () => {
    const m = await import('/src/store/useAppStore.ts')
    m.useAppStore.setState({ leftExpanded: true })
    return true
  })()`)
  // 清掉自举的 about:blank 载体之外的残留（保证「池增长」断言不被遗留视图满足）
  await win.evaluate(async () => {
    const st = await window.desktop.embedStatus()
    for (const t of st?.targets ?? []) {
      if (t.url && t.url !== 'about:blank') await window.desktop.embedCloseTarget(t.id)
    }
  })
  await sleep(400)

  // —— T1 打开即见内容（本轮用户口径；旧行为「提示 + 预览按钮」已翻转）——
  console.log('T1 打开即见内容：openFile(pdf) → 无中间态 + 右栏已切预览')
  await store(`(async () => { s.openFile(${JSON.stringify(PDF_NAME)}, undefined, 'source'); return true })()`)
  const t1 = await waitFor(async () => {
    const st = await store(`({
      // 阶段 2：内容按归一化路径索引（filePreviews）。PDF_NAME 是裸文件名（已相对）→ 键即它本身。
      binary: s.filePreviews?.[${JSON.stringify(PDF_NAME)}]?.binary === true,
      tab: m.selectRightTab(s),
      expanded: s.rightExpanded,
      buttons: document.querySelectorAll('.file-binary-actions .btn').length,
      hint: document.querySelectorAll('.file-binary-hint').length,
      status: document.querySelector('.file-binary-status')?.textContent?.trim() ?? '',
    })`)
    return st.tab === 'web' ? st : null
  }, { label: '打开后右栏切到预览', timeout: 30000 })
  check('bridge 判定为二进制（filePreviews[path].binary=true，内容不当文本喂 shiki）', t1.binary === true, JSON.stringify(t1))
  check('**没有**中间态按钮（「预览 PDF」/「用默认程序打开」都不再出现）', t1.buttons === 0, JSON.stringify(t1))
  check('**没有**提示段落（.file-binary-hint）', t1.hint === 0, JSON.stringify(t1))
  check('右栏已自动切到预览（激活标签=web + 已展开）', t1.tab === 'web' && t1.expanded === true, JSON.stringify(t1))
  await win.screenshot({ path: path.join(SHOTS, '01-file-tab-binary.png') })

  // —— T2 视图池新增该 PDF ——
  console.log('T2 视图池新增该 PDF（编码 + 会话归属）')
  const t2 = await waitFor(async () => {
    const st = await win.evaluate(() => window.desktop.embedStatus())
    const t = (st?.targets ?? []).find((x) => String(x.url || '').endsWith('.pdf'))
    return t ? { target: t, st } : null
  }, { label: 'PDF 内嵌视图出现', timeout: 30000 })
  const nViews = (t2.st?.targets ?? []).filter((t) => t.url && t.url !== 'about:blank').length
  check('视图池新增 PDF 视图（不是复用 about:blank 载体）', nViews === 1, JSON.stringify(t2.st?.targets))
  check('视图 URL = file://…（中文名按 URI 编码，可直接被 Chromium 加载）', t2.target.url === pdfUrl, `${t2.target.url} vs ${pdfUrl}`)
  check('视图归属当前会话（sid 对得上，不会跨会话泄漏）', (t2.target.sid ?? '') === SID, JSON.stringify(t2.target))

  // —— T3 视图几何 = 右栏内容容器 ——
  //
  // **先等布局落定，再量**（R06d）。以前这里是「量一次、差 ≤2px」，但被量的东西在面板入场
  // 动画期间是**会动**的：`.right-body` 的 slideRight（translateX 18px → 0）把 .browser-shot
  // 从 x+18 平移到 x；而原生视图只在两种时机拿到新矩形 —— ResizeObserver（只报尺寸、不报
  // 位移）和 animationend 补的那一枪。于是「动画中途量一次」必然让视图落后于 DOM，差多少
  // 取决于采样落在动画的哪一相。
  //
  // 实测（R06d，同码连跑 8 次）：E2E 窗口在后台，180ms 的入场动画被 Chromium 节流到 ~3.4s；
  // 失败的跑次恰好采样在「最后一次动画中途同步」之后 2ms、animationend 纠正之前 → 视图 x
  // 差 3–6px（y/宽/高全等，因为 slideRight 只平移）。所以这不是产品缺陷（产品在 animationend
  // 会补到正确位置），是**探针的采样竞态** —— 与 R06c 修 T16 同款处置：判据一字不改（±2px），
  // 先把被测对象等到落定。永久性偏移（真回归）照样红，因为量的是落定后的几何。
  const settle = await win.evaluate(async () => {
    const host = document.querySelector('.browser-shot')
    const running = () => document.getAnimations().filter((a) => {
      if (a.playState !== 'running') return false
      const t = a.effect && a.effect.target
      return !!t && t instanceof Element && (t === host || t.contains(host))
    })
    const names = []
    const t0 = Date.now()
    for (;;) {
      const rs = running()
      if (rs.length === 0) return { ok: true, waited: Date.now() - t0, names }
      for (const a of rs) { if (a.animationName && !names.includes(a.animationName)) names.push(a.animationName) }
      if (Date.now() - t0 > 20000) return { ok: false, waited: Date.now() - t0, names }
      await new Promise((r) => setTimeout(r, 16))
    }
  })
  console.log(`T3 布局落定：等 ${settle.waited}ms（运行中的动画：${settle.names.join(',') || '无'}）`)
  check('右栏容器布局已落定（入场动画已结束，不是采样在动画中途）', settle.ok === true, JSON.stringify(settle))
  // animationend → 渲染层 applyRect → IPC → 主进程 setBounds：给一条 IPC + 一帧的余量
  await sleep(150)
  const domRect = await win.evaluate(() => {
    const el = document.querySelector('.browser-shot')
    if (!el) return null
    const r = el.getBoundingClientRect()
    return { x: Math.round(r.x), y: Math.round(r.y), width: Math.round(r.width), height: Math.round(r.height), winW: window.innerWidth }
  })
  const geo = await app.evaluate(async ({ BrowserWindow }, arg) => {
    const win = BrowserWindow.getAllWindows()[0]
    const [ww, wh] = win.getContentSize()
    const kids = (win.contentView?.children ?? []).filter((c) => c.webContents && typeof c.webContents.getURL === 'function')
    const v = kids.find((c) => c.webContents.getURL() === arg.url)
    if (!v) return { found: false, ww, wh, urls: kids.map((c) => c.webContents.getURL()) }
    return { found: true, ww, wh, bounds: v.getBounds() }
  }, { url: pdfUrl })
  console.log(`T3 视图几何：window ${geo.ww}×${geo.wh}，DOM 容器 ${JSON.stringify(domRect)}，视图 ${JSON.stringify(geo.bounds)}`)
  check('主进程里能按 URL 找到该 WebContentsView', geo.found === true, JSON.stringify(geo))
  check('视图可见（embed:status 的 visible=true）', t2.target.visible === true, JSON.stringify(t2.target))
  const aligned = domRect && geo.bounds
    && Math.abs(geo.bounds.x - domRect.x) <= 2 && Math.abs(geo.bounds.y - domRect.y) <= 2
    && Math.abs(geo.bounds.width - domRect.width) <= 2 && Math.abs(geo.bounds.height - domRect.height) <= 2
  check('视图矩形 = 右栏内容容器 .browser-shot 的矩形（±2px ⇒ 视图就长在右栏里）', !!aligned, JSON.stringify({ domRect, bounds: geo.bounds }))
  check('容器贴窗口右缘（≥ winW-20 ⇒ 是右侧栏，不是中栏）', !!domRect && domRect.x + domRect.width >= domRect.winW - 20, JSON.stringify(domRect))

  // —— T4 真渲染（CDP contentType/embed + capturePage 纸张占比）——
  console.log('T4 真渲染：CDP 读 contentType/embed + capturePage「纸张」占比')
  const probe = async (expr) => {
    const res = await win.evaluate((e) => window.desktop.embedCommand({ method: 'Runtime.evaluate', params: { expression: e, returnByValue: true } }), expr)
    return res?.result?.result?.value ?? res?.result?.value ?? res?.value ?? null
  }
  const contentType = await probe('document.contentType')
  const embedBox = await probe('(() => { const e = document.querySelector("embed[type=\'application/pdf\']"); return e ? Math.round(e.getBoundingClientRect().width) + "x" + Math.round(e.getBoundingClientRect().height) : "" })()')
  check('页面 contentType = application/pdf（Chromium 把它当 PDF 打开，不是当文本/下载）', contentType === 'application/pdf', JSON.stringify(contentType))
  check('PDF 插件元素占满视图（<embed type=application/pdf> 全尺寸）', /\d+x\d+/.test(String(embedBox)), JSON.stringify(embedBox))
  // 纸张占比：暗色主题下「非白像素」会被背景色骗过（第一版实测 99.9%）→ 改数**亮像素**，
  // 它才是「PDF 页面真的画出来了」的判据（白纸占视图约 60%）。
  let shot = null
  const t0 = Date.now()
  await waitFor(async () => {
    shot = await app.evaluate(async ({ BrowserWindow }, arg) => {
      const win = BrowserWindow.getAllWindows()[0]
      const kids = (win.contentView?.children ?? []).filter((c) => c.webContents && typeof c.webContents.getURL === 'function')
      const v = kids.find((c) => c.webContents.getURL() === arg.url)
      if (!v) return null
      const img = await v.webContents.capturePage()
      const bmp = img.toBitmap()
      let paper = 0
      const px = bmp.length / 4
      for (let i = 0; i < bmp.length; i += 4) if (bmp[i] > 200 && bmp[i + 1] > 200 && bmp[i + 2] > 200) paper++
      return { size: img.getSize(), paper: px ? paper / px : 0, png: img.toPNG().toString('base64') }
    }, { url: pdfUrl })
    return shot && shot.paper > 0.05
  }, { label: 'PDF 页面画出（纸张占比 > 5%）', timeout: 20000, interval: 500 })
  if (shot) {
    const pngPath = path.join(SHOTS, '02-embed-pdf-view.png')
    fs.writeFileSync(pngPath, Buffer.from(shot.png, 'base64'))
    console.log(`    视图截图: ${pngPath}（${shot.size.width}×${shot.size.height}，纸张占比 ${(shot.paper * 100).toFixed(1)}%，首次达标耗时 ${Date.now() - t0}ms）`)
  }
  check('capturePage 拿到视图像素（没有渲染进程崩溃/黑屏）', !!shot && shot.size.width > 0 && shot.size.height > 0, JSON.stringify(shot?.size))
  check('页面真的画出来了（纸张占比 > 5% ⇒ 不是空白/灰底）', (shot?.paper ?? 0) > 0.05, `paper=${shot?.paper}`)

  // —— T4b 可视类文件不建 file 标签（「一个文件一个标签」）+ 切过去不被弹回 ——
  //
  // 2026-09 口径变更：openFile 对**可视类**文件（pdf/图片/html/office）不再建 file 标签 ——
  // 这类内容只有内嵌浏览器能渲染，file 标签里只剩一行状态（「内容已在右侧预览中打开」），
  // 与下面自动开的预览标签**同名** → 同一个文件占两个标签，用户看到的就是重复。
  // 故本段分两半：
  //   ① openFile 之后：标签集/标签条里**没有** file 标签，该文件只占预览这一个标签；
  //      fileFocus 仍指向它（文件面板的上下文不能丢 —— 用户之后手动点「文件」标签看的就是它）；
  //   ② 用户**显式**点开「文件」标签 → 文件栏照旧给那行状态，且切过去不被弹回预览。
  //
  // 为什么②要放在渲染之后：内嵌视图**首个真实页面加载完成**时，store 会按既有语义把面板切回
  // 内容标签（focusWebTabIfClosed，M6「有页面就展开预览」）。渲染完成前切到文件 tab 会被它
  // 顶回来（实测 diag.tab 一直是 web）。渲染完成 = 那个一次性事件已经发生 → 此时的切换才稳。
  console.log('T4b 可视类不建 file 标签 + 显式切过去不被弹回')
  const t4bTabs = await store(`({
    kinds: (s.tabsByScope[s.activeSessionId]?.tabs ?? []).map((t) => t.kind),
    focus: s.fileFocus?.path ?? '',
    tab: m.selectRightTab(s),
  })`)
  check('openFile(pdf) 之后**没有** file 标签（一个文件一个标签）', !t4bTabs.kinds.includes('file'), JSON.stringify(t4bTabs))
  check('fileFocus 仍指向该 PDF（手动开「文件」标签时能看到它）', t4bTabs.focus === PDF_NAME, JSON.stringify(t4bTabs))
  const t4bBar = {
    fileTabs: await win.locator('.rtab[data-kind="file"]').count(),
    pdfTabs: await win.locator('.rtab[data-kind="web"] .rtab-t', { hasText: PDF_NAME }).count(),
  }
  check('标签条上没有 file 标签（同一个文件不占两个标签）', t4bBar.fileTabs === 0, JSON.stringify(t4bBar))
  check('标签条上该 PDF 只有一个标签', t4bBar.pdfTabs === 1, JSON.stringify(t4bBar))
  // 诊断订阅：记录激活标签 kind 的每次变化（失败时能看出是谁把它切走的）。
  // R11b：没有可比较的镜像字段了，两侧都现算（选择器是纯函数，订阅里调它零成本）。
  await win.evaluate(`(async () => {
    const m = await import('/src/store/useAppStore.ts')
    window.__tabLog = []
    m.useAppStore.subscribe((s, prev) => {
      const from = m.selectRightTab(prev)
      const to = m.selectRightTab(s)
      if (from !== to) window.__tabLog.push({ t: Date.now(), from, to, left: s.leftExpanded })
    })
    return true
  })()`)
  await store('(async () => { s.openTab({ kind: "file" }); return true })()')
  let t4bDiag = null
  const t4b = await waitFor(async () => {
    const st = await store(`({
      status: document.querySelector('.file-binary-status')?.textContent?.trim() ?? '',
      buttons: document.querySelectorAll('.file-binary-actions .btn').length,
      hints: document.querySelectorAll('.file-binary-hint').length,
      tab: m.selectRightTab(s),
      focus: s.fileFocus?.path ?? '',
      binary: s.filePreviews?.[${JSON.stringify(PDF_NAME)}]?.binary ?? null,
      standalone: !!document.querySelector('.file-side-detail'),
      tabLog: (window.__tabLog ?? []).slice(-6),
    })`)
    t4bDiag = st
    return st.status ? st : null
  }, { label: '文件栏状态行出现', timeout: 15000 })
    .catch((e) => { throw new Error(`${e.message} | diag=${JSON.stringify(t4bDiag)}`) })
  // 显式点开「文件」标签时走的是 standalone 全文分支（fileFocus 无 toolId）—— 状态行由此渲染，
  // 文案口径与「打开即见内容」一致：内容在预览里，这里不再给多余的中间态按钮。
  check('文件栏留一行状态（而不是空白或中间态）', t4b.status.includes('已在右侧预览中打开'), JSON.stringify(t4b))
  check('状态行旁没有按钮、没有提示段落（文件动作收进了标签右键菜单）', t4b.buttons === 0 && t4b.hints === 0, JSON.stringify(t4b))
  await win.screenshot({ path: path.join(SHOTS, '01b-file-tab-status.png') })
  // 拉锯回归：切到文件 tab 后不得被弹回预览（第一版把自动打开放在组件里，实测 4ms 被顶回 web）
  await sleep(700)
  const t4c = await store('({ tab: m.selectRightTab(s), log: (window.__tabLog ?? []).slice(-4) })')
  check('切到文件栏后停在文件栏（不被自动打开弹回预览）', t4c.tab === 'file', JSON.stringify(t4c))
  await store('(async () => { s.openTab({ kind: "web" }); return true })()')

  // —— T5 标签条（新 UI）——
  console.log('T5 标签条：文件名标题 + aria-selected + URL 在 title 里')
  const tabInfo = async () => {
    const rows = await win.locator('.rtab[data-kind="web"]').all()
    const out = []
    for (const r of rows) {
      out.push({
        title: (await r.locator('.rtab-t').textContent())?.trim() ?? '',
        titleAttr: (await r.getAttribute('title'))?.trim() ?? '',
        role: (await r.getAttribute('role')) ?? '',
        selected: (await r.getAttribute('aria-selected')) ?? '',
        unread: (await r.locator('.browser-page-dot').count()) > 0,
        // 注意：点开 ✕ 会真关标签 → 这里只读属性，不点击
        cls: (await r.getAttribute('class')) ?? '',
      })
    }
    return out
  }
  const t5 = await waitFor(async () => {
    const rows = await tabInfo()
    return rows.length ? rows : null
  }, { label: '标签条出现', timeout: 20000 })
  const pdfTab = t5.find((r) => r.title.endsWith('.pdf'))
  check('标签标题 = 文件名（不再是整条百分号编码 URL）', pdfTab?.title === PDF_NAME, JSON.stringify(t5))
  check('完整 URL 保留在 title 属性里（悬停可见）', pdfTab?.titleAttr === pdfUrl, JSON.stringify(pdfTab))
  check('ARIA：role=tab + aria-selected=true（激活页）', pdfTab?.role === 'tab' && pdfTab?.selected === 'true', JSON.stringify(pdfTab))
  check('前台打开的标签**没有**未读点', pdfTab?.unread === false, JSON.stringify(pdfTab))

  // —— T6 面板语义（切内容标签 + 不收左栏）——
  const t6 = await store('({ tab: m.selectRightTab(s), expanded: s.rightExpanded, left: s.leftExpanded })')
  check('面板切到内容标签（激活标签=web）且已展开', t6.tab === 'web' && t6.expanded === true, JSON.stringify(t6))
  check('左栏**未被收起**（M4 回归点：预览不该有「AI 接管视界」的副作用）', t6.left === true, JSON.stringify(t6))
  await win.screenshot({ path: path.join(SHOTS, '03-app-window.png') })

  // —— T7 未读点（后台打开 → 有未读点；点开 → 消失）——
  console.log('T7 未读点：后台打开（activate:false）→ 有未读点；点击 → 消失')
  const t7 = await waitFor(async () => {
    await store(`(async () => { await s.openInBrowserTab(${JSON.stringify(HTML_NAME)}, { activate: false }); return true })()`)
    const rows = await tabInfo()
    const tab = rows.find((r) => r.title === HTML_NAME)
    return tab?.unread ? { rows, tab } : null
  }, { label: '后台标签出现未读点', timeout: 20000, interval: 500 })
  check('后台打开的标签带未读点（没有它，后台打开等于「看不见」）', t7.tab.unread === true, JSON.stringify(t7.tab))
  // 点它 → 未读点消失（pickPage 立即清 + nav 事件兜底）
  const bgRow = win.locator('.rtab[data-kind="web"]', { hasText: HTML_NAME }).first()
  await bgRow.click()
  const t7b = await waitFor(async () => {
    const rows = await tabInfo()
    const tab = rows.find((r) => r.title === HTML_NAME)
    return tab && !tab.unread && tab.selected === 'true' ? tab : null
  }, { label: '未读点消失且成为激活页', timeout: 20000 })
  check('点开后未读点消失且成为激活页', t7b.unread === false && t7b.selected === 'true', JSON.stringify(t7b))

  // —— T8 右键菜单（复制路径回读剪贴板 + 关闭右侧所有标签）——
  console.log('T8 右键菜单：复制文件路径 / 关闭右侧所有标签')
  const pdfRow = win.locator('.rtab[data-kind="web"]', { hasText: PDF_NAME }).first()
  await pdfRow.click({ button: 'right' })
  await win.locator('.ctx-menu').waitFor({ state: 'visible', timeout: 5000 })
  const menuItems = await win.locator('.ctx-menu .ctx-item').allTextContents()
  console.log(`    菜单项: ${JSON.stringify(menuItems.map((s) => s.trim()))}`)
  check('菜单含文件动作三项（复制路径/在访达中显示/用默认程序打开）',
    menuItems.some((s) => s.includes('复制文件路径')) && menuItems.some((s) => s.includes('在访达中显示')) && menuItems.some((s) => s.includes('用默认程序打开')),
    JSON.stringify(menuItems))
  await win.locator('.ctx-menu .ctx-item', { hasText: '复制文件路径' }).click()
  const clip = await app.evaluate(({ clipboard }) => clipboard.readText())
  check('「复制文件路径」把**解码后的绝对路径**写进真剪贴板', clip === path.join(WS, PDF_NAME), JSON.stringify(clip))
  // 关闭右侧所有标签：先确保 PDF 左侧、HTML 在右侧 —— 当前顺序 = 池插入顺序（PDF 先）
  await pdfRow.click({ button: 'right' })
  await win.locator('.ctx-menu .ctx-item', { hasText: '关闭右侧所有标签' }).click()
  const t8 = await waitFor(async () => {
    const st = await win.evaluate(() => window.desktop.embedStatus())
    const urls = (st?.targets ?? []).map((t) => t.url)
    return !urls.includes(htmlUrl) ? urls : null
  }, { label: '右侧标签被批量关闭', timeout: 20000 })
  check('「关闭右侧所有标签」真的关掉了后面的标签（HTML 视图已销毁）', !t8.includes(htmlUrl), JSON.stringify(t8))
  check('被右键的那个标签自己还在（关的是「右侧」不是「全部」）', t8.includes(pdfUrl), JSON.stringify(t8))

  // —— T9 两标签并存（缺陷 A 回归）——
  console.log('T9 再开一个可视文件 → 两标签并存（不顶掉）')
  await store(`(async () => { await s.openHtmlInBrowser(${JSON.stringify(HTML_NAME)}); return true })()`)
  const t9 = await waitFor(async () => {
    const st = await win.evaluate(() => window.desktop.embedStatus())
    const urls = (st?.targets ?? []).map((t) => t.url)
    return urls.includes(pdfUrl) && urls.includes(htmlUrl) ? urls : null
  }, { label: 'PDF 与第二个文件并存', timeout: 30000 })
  check('两个 file:// 视图同时在池中（PDF 没被顶掉）', t9.includes(pdfUrl) && t9.includes(htmlUrl), JSON.stringify(t9))

  // —— T10 关标签 = 真销毁 ——
  console.log('T10 点 PDF 标签的 ✕ → 视图真销毁，另一个仍在')
  let closeRow = null
  for (const r of await win.locator('.rtab[data-kind="web"]').all()) {
    if (((await r.locator('.rtab-t').textContent()) ?? '').trim() === PDF_NAME) closeRow = r
  }
  if (!closeRow) throw new Error('标签条里找不到 PDF 标签')
  await closeRow.locator('.rtab-close').click()
  const t10 = await waitFor(async () => {
    const st = await win.evaluate(() => window.desktop.embedStatus())
    const urls = (st?.targets ?? []).map((t) => t.url)
    return !urls.includes(pdfUrl) ? urls : null
  }, { label: 'PDF 视图销毁', timeout: 20000 })
  check('PDF 视图已从池中移除（真销毁，不是只藏起来）', !t10.includes(pdfUrl), JSON.stringify(t10))
  check('另一个文件的视图不受影响（关一个不连坐）', t10.includes(htmlUrl), JSON.stringify(t10))

  // —— T14 面板级下拉菜单不得被原生视图盖住 ——
  // 原生 WebContentsView 恒合成在窗口 DOM 之上（原生层级 > DOM，CSS z-index 压不住）→
  // 菜单打开期间必须把原生视图 hide 掉，否则菜单被预览画面挡掉（2026-09-20 用户实测报障）。
  console.log('T14 打开「＋」菜单 → 原生视图必须隐藏（否则菜单被画面盖住）')
  await store('(async () => { s.openTab({ kind: "web" }); return true })()')
  await waitFor(async () => {
    const st = await win.evaluate(() => window.desktop.embedStatus())
    return st?.visible === true ? st : null
  }, { label: '预览可见（前置）', timeout: 20000 })
  check('T14 前置：预览可见（visible=true）', true)
  await win.locator('.rtabs-add').click()
  const t14 = await waitFor(async () => {
    const st = await win.evaluate(() => window.desktop.embedStatus())
    return st?.visible === false ? st : null
  }, { label: '菜单打开 → 视图隐藏', timeout: 10000 })
  check('T14 菜单打开期间原生视图已隐藏（菜单不会被盖住）', t14.visible === false, JSON.stringify(t14))
  // 菜单锚点（T3）：新建菜单必须贴在「＋」按钮**正下方、贴其右缘** —— 而不是标签条左侧
  //（基础样式是 left:8px，那样会跑到 ⌄ 底下）。量真实矩形而不是看类名：CSS 改回左侧时
  // 类名还在，只有矩形能证明它确实挂在按钮上（截图复核见 04-add-menu.png）。
  const t14Anchor = await win.evaluate(() => {
    const add = document.querySelector('.rtabs-add')?.getBoundingClientRect()
    const menu = document.querySelector('.rtabs-menu')?.getBoundingClientRect()
    const more = document.querySelector('.rtabs-more')?.getBoundingClientRect()
    if (!add || !menu) return null
    return {
      addLeft: Math.round(add.left), addRight: Math.round(add.right), addBottom: Math.round(add.bottom),
      menuLeft: Math.round(menu.left), menuRight: Math.round(menu.right), menuTop: Math.round(menu.top),
      moreLeft: Math.round(more?.left ?? -1),
      rightVariant: !!document.querySelector('.rtabs-menu-right'),
      menus: document.querySelectorAll('.rtabs-menu').length,
    }
  })
  check('＋ 菜单锚在按钮正下方（右缘对齐按钮右缘、顶边贴按钮底边，±2px）',
    !!t14Anchor && Math.abs(t14Anchor.menuRight - t14Anchor.addRight) <= 2 && Math.abs(t14Anchor.menuTop - t14Anchor.addBottom) <= 2,
    JSON.stringify(t14Anchor))
  check('＋ 菜单不再锚在标签条左侧（左缘远离 ⌄ 一侧；且只有一个菜单在 DOM 里）',
    !!t14Anchor && t14Anchor.menuLeft > t14Anchor.moreLeft + 100 && t14Anchor.rightVariant && t14Anchor.menus === 1,
    JSON.stringify(t14Anchor))
  await win.screenshot({ path: path.join(SHOTS, '04-add-menu.png') }).catch(() => {})
  await win.locator('.rtabs-backdrop').click({ force: true })
  const t14b = await waitFor(async () => {
    const st = await win.evaluate(() => window.desktop.embedStatus())
    return st?.visible === true ? st : null
  }, { label: '菜单关闭 → 视图恢复', timeout: 10000 })
  check('T14 菜单关闭后视图恢复显示', t14b.visible === true, JSON.stringify(t14b))

  // —— T15「＋ → 网页」在**池里没有真实页面**时必须真的开出一个标签（2026-09-21 用户报障）——
  // 用户原话：「右侧侧边栏点击加号、选择网页无效」。真机 trace：点击确实建了 web 标签，但它认下了
  // 池里那个 **about:blank 自举载体**的 id（`makeTab` 取 embedNav.activeId），而 onEmbedNav 喂给
  // syncWebTabs 的 items 会过滤掉 about:blank → 标签在 4ms 后被当「页面已关闭」删掉、面板被
  // 顶回 activity —— 用户看到的就是「点了没反应」。修法见 store 的 activePageIdOf。
  // 本用例走**真实点击路径**（＋ → 菜单项「网页」），且刻意把池清成「只剩自举载体」= 用户当时的现场。
  console.log('T15 无真实页面时「＋ → 网页」也必须开出 web 标签（自举载体不当身份）')
  await store('(async () => { s.openTab({ kind: "web" }); return true })()')
  await sleep(300)
  // 清掉所有真实页面（只留 about:blank 自举载体）—— 与用户现场一致
  await win.evaluate(async () => {
    const st = await window.desktop.embedStatus()
    for (const t of st?.targets ?? []) {
      if (t.url && t.url !== 'about:blank') await window.desktop.embedCloseTarget(t.id)
    }
  })
  await sleep(800)
  // 关掉现存 web 标签（页面都关光了，标签会随 nav 事件收敛掉；兜底手动收敛一次）
  await win.evaluate(`(async () => {
    const m = await import('/src/store/useAppStore.ts')
    const s = m.useAppStore.getState()
    for (const t of [...(s.tabsByScope[s.activeSessionId]?.tabs ?? [])]) if (t.kind === 'web') s.closeTab(t.id)
    return true
  })()`)
  await sleep(800)
  const t15Pre = await win.evaluate(async () => {
    const st = await window.desktop.embedStatus()
    return { pool: (st?.targets ?? []).map((t) => t.url) }
  })
  check('T15 前置：视图池只剩 about:blank 自举载体（＝用户报障时的现场）',
    t15Pre.pool.length === 1 && t15Pre.pool[0] === 'about:blank', JSON.stringify(t15Pre))
  await win.locator('.rtabs-add').click()
  await win.locator('.rtabs-menu-right .rtabs-mi', { hasText: '网页' }).click()
  // 断言取在**nav 事件之后**（事件在点击后 ~4ms 内到达；等 1s 远超那个窗口）——
  // 修复前此刻标签已经被吃掉了，这条会红（见提交记录里的先红后绿）。
  await sleep(1000)
  const t15 = await store(`({
    tab: m.selectRightTab(s),
    web: (s.tabsByScope[s.activeSessionId]?.tabs ?? []).filter((x) => x.kind === 'web').map((x) => ({ id: x.id, viewId: x.viewId ?? null })),
    active: s.tabsByScope[s.activeSessionId]?.activeId ?? null,
    chips: [...document.querySelectorAll('.rtab[data-kind="web"]')].length,
    body: (document.querySelector('.right-body')?.textContent ?? '').slice(0, 40),
  })`)
  check('T15 「＋ → 网页」真的开出了 web 标签（没被自举载体连累删掉）',
    t15.web.length === 1, JSON.stringify(t15))
  check('T15 且它**不认** about:blank 自举载体当身份（viewId 为空 = 等真实页面收养）',
    t15.web[0]?.viewId === null, JSON.stringify(t15.web))
  check('T15 面板切到网页标签（用户看得见变化 —— 报障的正是「没反应」）',
    t15.tab === 'web' && t15.active === t15.web[0]?.id, JSON.stringify({ tab: t15.tab, active: t15.active, web: t15.web }))
  check('T15 标签条上真的多出一个 web 标签', t15.chips === 1, JSON.stringify(t15.chips))
  await win.screenshot({ path: path.join(SHOTS, '05-add-web-no-page.png') }).catch(() => {})

  // —— T16 收起右栏不得把页面的**排版视口**钉死（2026-09-21 用户报障：预览只剩左上角一小块）——
  // 用户原话：「图片、网页等无法正常加载」+ 截图里整块面板只剩左上角一小块内容（页面底色铺满）。
  // 根因（真机 trace 实测，两段各修一处）：
  //   ① 制造：收起右栏时 BrowserTab 卸载，ResizeObserver 对**已脱离文档**的元素还补一次「尺寸 0」
  //      的通知，渲染层把那次「量不到」钳成了 80×80 下发 → `embed:resize` 把**所有**原生视图缩到
  //      80×80，`Emulation.setDeviceMetricsOverride` 更把**当前显示的那一页**的排版视口钉在 80×80
  //      （页面按 80px 宽排版 = 只剩左上角一小块）。收口见 RightPanel 的 embedRectOf。
  //   ② 不自愈：视口对齐只在**尺寸变化**时下发、且只打给主进程当前的 activeId → 同尺寸下切换标签
  //      （不重挂载、ResizeObserver 不触发）会把错视口一直留着（用户点标签、刷新页面都回不来）。
  //      收口见 electron/embed.ts 的 alignEntryViewport（激活/显示路径各对齐一次）。
  console.log('T16 收起右栏不得缩视图 + 切回标签必须自愈（排版视口 = 容器）')
  const viewportOf = async (urlHint) => {
    const pool = await win.evaluate(() => window.desktop.embedStatus())
    const vis = urlHint
      ? (pool?.targets ?? []).find((x) => x.url === urlHint)
      : (pool?.targets ?? []).find((x) => x.visible && x.url && x.url !== 'about:blank')
    if (!vis) return { vis: null }
    const m = await app.evaluate(async ({ BrowserWindow }, arg) => {
      const w = BrowserWindow.getAllWindows()[0]
      const kids = (w.contentView?.children ?? []).filter((c) => c.webContents && typeof c.webContents.getURL === 'function')
      const v = kids.find((c) => c.webContents.getURL() === arg.url)
      if (!v) return null
      let inner = null
      try { inner = await v.webContents.executeJavaScript('[window.innerWidth, window.innerHeight]', true) } catch { /* 未就绪 */ }
      return { bounds: v.getBounds(), zoom: w.webContents.getZoomFactor(), inner }
    }, { url: vis.url })
    return { vis, ...m }
  }
  const allBounds = async () => app.evaluate(({ BrowserWindow }) => {
    const w = BrowserWindow.getAllWindows()[0]
    return (w.contentView?.children ?? [])
      .filter((c) => c.webContents && typeof c.webContents.getURL === 'function')
      .map((c) => ({ url: c.webContents.getURL(), bounds: c.getBounds() }))
      .filter((x) => x.url && x.url !== 'about:blank')
  })
  await store(`(async () => { await s.openHtmlInBrowser(${JSON.stringify(HTML_NAME)}); return true })()`)
  const pre16 = await waitFor(async () => {
    const v = await viewportOf()
    return v?.inner ? v : null
  }, { label: '第二个预览显示中', timeout: 30000 })
  const expW0 = Math.round(pre16.bounds.width / pre16.zoom)
  check('T16 前置：有一页显示中，且排版视口 = 容器（基线本来就该成立）',
    Math.abs(pre16.inner[0] - expW0) <= 4, JSON.stringify({ inner: pre16.inner, bounds: pre16.bounds, zoom: pre16.zoom }))

  // —— T16a：收起右栏**不得**把原生视图缩成 80×80（缺陷的源头）——
  await store('(async () => { s.setRightExpanded(false); return true })()')
  await sleep(1500)
  const collapsed = await allBounds()
  const tiny = collapsed.filter((x) => x.bounds.width <= 100 || x.bounds.height <= 100)
  check('T16a 收起右栏后原生视图**没有**被缩成 80×80（缺陷源头：量不到就下发假矩形）',
    collapsed.length > 0 && tiny.length === 0, JSON.stringify(collapsed))
  const collapsedViewport = await viewportOf(pre16.vis.url)
  check('T16a 收起期间页面的排版视口也没被钉死（旧实现此刻被写成 80×80）',
    !!collapsedViewport.inner && collapsedViewport.inner[0] > 200, JSON.stringify(collapsedViewport.inner ?? null))

  // —— T16b：任何来源留下的错视口，切回这个标签时必须自愈（**同尺寸切换**标签这条路）——
  // 为什么必须钉这条：视口对齐只在「尺寸变化」时下发，而同尺寸切换标签既不会重挂载 BrowserTab、
  // 也不会触发 ResizeObserver → 旧实现下这页会一直按 80px 排版（用户点标签、刷新都不回来）。
  await store(`(async () => { s.openFile(${JSON.stringify(PDF_NAME)}, undefined, 'source'); return true })()`)
  const pdfShown = await waitFor(async () => {
    const pool = await win.evaluate(() => window.desktop.embedStatus())
    return (pool?.targets ?? []).some((x) => String(x.url).endsWith('.pdf')) ? pool : null
  }, { label: '第二个 web 标签（PDF）就绪', timeout: 30000 })
  void pdfShown
  const htmlChip = win.locator('.rtab[data-kind="web"]', { hasText: HTML_NAME }).first()
  await htmlChip.click()
  await sleep(1200)
  // 把「显示中那一页」（html 预览）的排版视口设成 80×80 —— 用户现场就是它被这么留下的
  await win.evaluate(() => window.desktop.embedCommand({
    method: 'Emulation.setDeviceMetricsOverride',
    params: { width: 80, height: 80, deviceScaleFactor: 1, mobile: false },
  })).catch(() => {})
  await sleep(600)
  const botched = await viewportOf()
  check('T16b 前置：已把显示中页面的视口弄坏（80×80，模拟缺陷现场）',
    !!botched.inner && botched.inner[0] <= 120, JSON.stringify(botched.inner ?? null))
  // 同尺寸切换：html → PDF → html（面板尺寸不变、组件不重挂载）
  await win.locator('.rtab[data-kind="web"]', { hasText: '.pdf' }).first().click()
  await sleep(1200)
  await htmlChip.click()
  const healed = await waitFor(async () => {
    const v = await viewportOf()
    return v?.inner && v.inner[0] > 120 ? v : null
  }, { label: '同尺寸切回后排版视口自愈', timeout: 15000 }).catch(() => null)
  const after16 = healed ?? await viewportOf()
  const expW = after16.bounds ? Math.round(after16.bounds.width / after16.zoom) : 0
  console.log(`    bounds=${after16?.bounds?.width}×${after16?.bounds?.height} zoom=${after16?.zoom} → 期望视口 ${expW}，实际 ${JSON.stringify(after16?.inner ?? null)}`)
  check('T16b 同尺寸切回这个标签后排版视口自愈为容器尺寸（不再留在 80×80）',
    !!after16.inner && after16.inner[0] > 200 && Math.abs(after16.inner[0] - expW) <= 4,
    JSON.stringify({ inner: after16.inner, expected: expW, bounds: after16.bounds }))
  check('T16b 且高度也对齐（不是只修了宽度）',
    !!after16.inner && Math.abs(after16.inner[1] - Math.round(after16.bounds.height / after16.zoom)) <= 4,
    JSON.stringify({ inner: after16.inner, bounds: after16.bounds }))
  await win.screenshot({ path: path.join(SHOTS, '06-after-collapse-expand.png') }).catch(() => {})
} catch (e) {
  fails.push(`异常: ${e?.message ?? e}`)
  console.error(`\n❌ 异常: ${e?.stack ?? e}`)
  if (viteErr.trim()) console.error(`vite stderr:\n${viteErr}`)
} finally {
  try { await app?.close() } catch { /* 已退出 */ }
  cleanup()
}

console.log(`\n证据截图目录: ${SHOTS}`)
console.log(fails.length === 0 ? '\nALL PASS' : `\nFAILED (${fails.length}):\n - ${fails.join('\n - ')}`)
process.exit(fails.length === 0 ? 0 : 1)
