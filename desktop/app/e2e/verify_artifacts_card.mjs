#!/usr/bin/env node
// Artifacts 产出卡片回归（2026-09-15）
//
// 起因：AgentEnd 新增 Artifacts（本轮产出文件汇总），客户端渲染为「本次产出」卡片
// （样式选型 C：hero 大图标磁贴 + 紧凑列表 + 折叠 + 点击复制路径）。
//
// 本脚本锁住的关键行为（每条都对应一个「实现错了就会静默退化」的点）：
//   ① hero 渲染首个产出（文件名/徽标/± 统计/size·lines/路径），动作**常显**
//   ② 紧凑列表：其余产出渲染；动作**默认隐藏、悬停浮现**（对齐 htmlblock 语言）
//   ③ 键盘可达：聚焦行内按钮 → :focus-within 让动作浮现（不牺牲可达性）
//   ④ 折叠：默认显示 4 条，点「展开剩余」→ 全显，计数同步（4/N → N/N）；可收回
//   ⑤ 点击路径 = 复制完整路径（绝对化：相对路径 → 工作区 + 路径）+ 视觉反馈
//   ⑥ 按扩展名分派主按钮：.html → 在浏览器打开；文本 → 预览；其他 → 用默认程序打开
//   ⑦ 新建/修改徽标区分；size 为 0（append 增量语义）时不显示体积
//   ⑧ 同内容去重标注（同 sha256 的第二个产出标「与 X 内容相同」）
//   ⑨ 截断提示（artifacts_truncated）
//   ⑩ 空清单不渲染任何内容（不产生空卡片）
//   ⑩b xlsx/xls：主按钮「预览表格」走 bridge 转 HTML → 内嵌浏览器（**不**走文本预览），
//       且次按钮「用默认程序打开」保留（内嵌预览看不了图表；转换失败也靠它兜底）——
//       与 PDF 那条同形（⑥b）
//
// 用法：cd desktop/app && node e2e/verify_artifacts_card.mjs
// 退出码：全通过 0；任一断言失败 1；前置不满足（端口被占 / 探针服务没起来）2。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import fs from 'fs'
import { exitCodeOf, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4188)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

// 就绪判据 = waitProbePage（e2e/_serve.mjs）：**这张探针页真的回了 200 且带本脚本的标记**，
// 而不是「端口上有人在听」。固定端口 4188 一旦挂着别人（F46：残留在 5199 的那台 vite 就是这样），
// 旧写法会把「连上了别人的服务器」读成测试失败 —— 现在是前置不满足，退出码 2。
const PROBE_MARKER = '<title>artifacts probe</title>'

// 临时探针页：挂真实 ArtifactCard 组件 + 真实 index.css（跑完删除）。
// store 需要的最小状态用 setState 直接注入（workspace 用于相对路径绝对化）。
const PROBE_DIR = path.join(__dirname, '_artprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>artifacts probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import { ArtifactCard } from '../../src/components/ArtifactCard'
import RightPanel from '../../src/components/RightPanel'
import { useAppStore } from '../../src/store/useAppStore'
// ⚠️ 副作用注册，必须与 src/main.tsx 一致（04-review F45）：RightPanel 里 file/git/web 三个面板
// 都是 getSurface(kind)?.render(...) ?? 兜底，少了这一行，「点预览 → 文件真的渲染出来」证明的
// 是 \`??\` 兜底分支，不是 provider。下面的 __surfaces 自检让这件事当场可断言。
import '../../src/surfaces/builtin'
import { getSurface } from '../../src/surfaces/registry'
import '../../src/index.css'

// 仪器自检：把「注册表里到底有没有这几个 kind」暴露给 e2e（断言见脚本里的「仪器」一节）。
;(window as unknown as { __surfaces: Record<string, boolean> }).__surfaces =
  Object.fromEntries(['file', 'git', 'web'].map((k) => [k, !!getSurface(k)]))

// workspace 用于相对路径绝对化（点击复制/预览的目标路径）
useAppStore.setState({ workspace: '/Users/dev/ws' })
// 暴露给 e2e 驱动（验证「点预览」后的真实渲染，而非仅按钮文案）
;(window as unknown as { __store: unknown }).__store = useAppStore
// 启动 store（建立 transport 连接）：不调用它 → send() 发出去的命令响应无人接收，
// 「点预览」永远等不到 file_preview 响应（e2e 必须复刻真实启动路径）。
void useAppStore.getState().start('/Users/dev/ws')

const CASES = (window as unknown as { __CASES: Record<string, { items: unknown[]; truncated?: boolean }> }).__CASES

// 左：产出卡片样本；右：真实 RightPanel（预览目标）。缺 RightPanel 就无法验证
// 「点预览 → 文件真的渲染出来」（此前 e2e 只断言按钮文案，漏掉了真实缺陷）。
createRoot(document.getElementById('root')!).render(
  <div style={{ display: 'flex', height: '100vh' }}>
    <div className="narrative" style={{ flex: '1 1 auto', overflowY: 'auto', padding: 16, minWidth: 0 }}>
      {Object.entries(CASES).map(([k, v]) => (
        <div key={k} data-case={k} className="msg artifacts">
          <ArtifactCard items={v.items as never} {...(v.truncated ? { truncated: true } : {})} />
        </div>
      ))}
    </div>
    <div style={{ flex: '0 0 460px', borderLeft: '1px solid var(--border)', minWidth: 0 }}>
      <RightPanel />
    </div>
  </div>,
)
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_artprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
server.stderr.on('data', (d) => { const s = String(d); if (!/ready in|Local:/.test(s)) process.stderr.write(s) })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

// —— 样本 ——
const SINGLE = {
  items: [{ name: 'flappy-bird.html', path: 'out/flappy-bird.html', kind: 'created', added: 120, removed: 0, size: 15249, lines: 412, sha256: 'h1' }],
}
const MULTI = {
  items: [
    { name: 'flappy-bird.html', path: 'out/flappy-bird.html', kind: 'created', added: 120, size: 15249, lines: 412, sha256: 'h1' },
    { name: 'README.md', path: 'README.md', kind: 'modified', added: 42, removed: 3, size: 2100, lines: 88, sha256: 'h2' },
    { name: 'sales.xlsx', path: 'out/sales.xlsx', kind: 'created', size: 38000 },
    { name: 'api.ts', path: 'src/api.ts', kind: 'modified', added: 18, removed: 7, size: 4600, lines: 152, sha256: 'h3' },
    { name: 'chart.svg', path: 'out/chart.svg', kind: 'created', size: 12000 },
    { name: 'notes.md', path: 'docs/notes.md', kind: 'created', size: 1200, lines: 34, sha256: 'h4' },
    { name: 'styles.css', path: 'src/styles.css', kind: 'modified', added: 9, removed: 4, size: 3100, lines: 110, sha256: 'h5' },
  ],
}
// size=0（append 增量语义）→ 不显示体积维度；abs path 在 workspace 外
const APPEND_DELTA = {
  items: [
    { name: 'log.txt', path: 'log.txt', kind: 'modified', added: 3, sha256: 'h9' },
    { name: 'abs.txt', path: '/tmp/elsewhere/abs.txt', kind: 'created', size: 12, lines: 1, sha256: 'hA' },
  ],
}
// 同内容不同路径 → 第二项标注「与 X 内容相同」
const DUP = {
  items: [
    { name: 'a.html', path: 'a.html', kind: 'created', size: 100, lines: 5, sha256: 'same' },
    { name: 'copy.html', path: 'copy.html', kind: 'created', size: 100, lines: 5, sha256: 'same' },
  ],
}
// PDF：主按钮 = 预览 PDF（内嵌浏览器渲染），次按钮必须保留「用默认程序打开」。
// 单独一个 case（不塞进 MULTI）：MULTI 的条数/折叠计数已被多条断言锁死，加一项会连带
// 改动它们的期望值，把一次功能回归混进一堆无关数字里。
const PDF = {
  items: [
    { name: 'report.pdf', path: 'out/report.pdf', kind: 'created', size: 240000, lines: 12, sha256: 'hP' },
    { name: 'brief.pdf', path: 'out/brief.pdf', kind: 'modified', size: 90000, sha256: 'hQ' },
  ],
}
// xlsx：主按钮 = 预览表格（bridge 转 HTML → 内嵌浏览器 file://），次按钮必须保留
// 「用默认程序打开」（内嵌预览看不了图表/条件格式；.xls 转换失败也靠它兜底）。
// 与 PDF case 同形、单独一个（理由同上：不混进 MULTI 的计数断言）。
const XLSX = {
  items: [
    { name: 'sales.xlsx', path: 'out/sales.xlsx', kind: 'created', size: 38000, lines: 12, sha256: 'hX1' },
    { name: 'legacy.xls', path: 'out/legacy.xls', kind: 'created', size: 21000, sha256: 'hX2' },
  ],
}
const TRUNC = {
  items: [{ name: 'x.txt', path: 'x.txt', kind: 'created', size: 10, lines: 1, sha256: 'hX' }],
  truncated: true,
}
const EMPTY = { items: [] }

async function main() {
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:artifacts', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 900, height: 1000 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => {
    const t = m.text()
    if (m.type() === 'error' && !/ERR_NAME_NOT_RESOLVED|Failed to load resource/.test(t)) errs.push(t.slice(0, 200))
  })
  await page.addInitScript((c) => { window.__CASES = c }, {
    single: SINGLE, multi: MULTI, appendDelta: APPEND_DELTA, dup: DUP, trunc: TRUNC, empty: EMPTY, pdf: PDF, xlsx: XLSX,
  })
  await page.goto(PROBE_URL)
  await page.waitForSelector('[data-case="single"] .artifacts-card')

  // ── 仪器自检：探针走的是被测那条路（04 F45）──
  // 少了 surfaces/builtin → getSurface() 全 undefined → RightPanel 静默走 `??` 兜底，
  // 这时「预览渲染出内容」验的是兜底组件，不是 provider。坏掉的仪器必须当场变红。
  const surfaces = (await page.evaluate(() => window.__surfaces)) ?? {}
  assert(surfaces.file && surfaces.git && surfaces.web,
    `surface provider 已注册（file/git/web = ${JSON.stringify(surfaces)}）`)

  const q = (sel, caseName = 'single') => page.locator(`[data-case="${caseName}"] ${sel}`)
  // 折叠可见行数：.art-more 包裹层 display:none 的行不计
  const visibleRows = () => page.locator('[data-case="multi"] .art-rest > div').evaluateAll(
    (els) => els.filter((e) => (e.classList.contains('art-more') ? getComputedStyle(e).display !== 'none' : true))
      .filter((e) => e.querySelector('.art')).length,
  )

  // ── ① hero 渲染 + 动作常显 ──
  {
    const heroName = await q('.art-hero-name').textContent()
    assert(heroName === 'flappy-bird.html', `hero 显示首个产出：${heroName}`)

    const badge = (await q('.art-hero .badge').textContent())?.trim()
    assert(badge === '新建', `created → 「新建」徽标：${badge}`)

    const stat = (await q('.art-hero .art-stat').textContent()) ?? ''
    assert(stat.includes('+120'), `hero 显示 +行数：${stat}`)

    const meta = (await q('.art-hero .hero-meta').textContent()) ?? ''
    assert(/14\.9 KB/.test(meta) && /412/.test(meta), `hero 元信息含体积与行数：${meta}`)

    const tileExt = (await q('.art-tile .ext').textContent())?.trim()
    assert(tileExt === 'html', `磁贴显示扩展名：${tileExt}`)

    // hero 动作常显（opacity=1，非悬停浮出）
    const heroActsOpacity = await q('.art-hero .art-acts').evaluate((el) => getComputedStyle(el).opacity)
    assert(heroActsOpacity === '1', `hero 动作常显（opacity=${heroActsOpacity}）`)

    // .html → 主按钮为「在浏览器打开」
    const primary = (await q('.art-hero .btn.primary').textContent())?.trim()
    assert(primary === '在浏览器打开', `.html 主按钮 = 在浏览器打开：${primary}`)
  }

  // ── ② 紧凑列表：默认折叠 + 悬停浮现 ──
  {
    const rows = page.locator('[data-case="multi"] .art-rest .art')
    const total = await rows.count()
    assert(total === 6, `紧凑列表渲染其余 6 项：${total}`)

    const visible = await visibleRows()
    assert(visible === 4, `默认显示 4 条（折叠）：${visible}`)

    const hud = (await q('.art-rest-hd', 'multi').textContent()) ?? ''
    assert(/另外 6 个/.test(hud) && /显示 4 \/ 6/.test(hud), `列表头显示计数：${hud}`)

    // 动作默认隐藏 → 悬停浮现
    const firstRowActs = page.locator('[data-case="multi"] .art-rest .art').first().locator('.art-acts')
    const before = await firstRowActs.evaluate((el) => getComputedStyle(el).opacity)
    assert(before === '0', `紧凑行动作默认隐藏（opacity=${before}）`)

    await page.locator('[data-case="multi"] .art-rest .art').first().hover()
    await page.waitForTimeout(250)
    const after = await firstRowActs.evaluate((el) => getComputedStyle(el).opacity)
    assert(after === '1', `悬停后动作浮现（opacity=${after}）`)
  }

  // ── ③ 键盘可达：聚焦行内按钮 → :focus-within 浮现 ──
  {
    await page.locator('[data-case="multi"] .art-rest .art').first().locator('.btn').first().focus()
    await page.waitForTimeout(200)
    const state = await page.evaluate(() => {
      const art = document.querySelector('[data-case="multi"] .art-rest .art')
      const acts = art ? art.querySelector('.art-acts') : null
      return {
        focusWithin: art ? art.matches(':focus-within') : false,
        opacity: acts ? getComputedStyle(acts).opacity : 'n/a',
        focusedTag: document.activeElement ? document.activeElement.tagName : '',
      }
    })
    assert(state.focusWithin, ':focus-within 命中行容器')
    assert(state.opacity === '1', `键盘聚焦时动作浮现（opacity=${state.opacity}）`)
    assert(state.focusedTag === 'BUTTON', `焦点在按钮上：${state.focusedTag}`)
  }

  // ── ④ 折叠展开 / 收回 + 计数同步 ──
  {
    const btn = page.locator('[data-case="multi"] .art-ft button', { hasText: '展开剩余' }).first()
    assert(await btn.count() === 1, '存在「展开剩余 N 个」按钮')
    await btn.click()
    await page.waitForTimeout(250)

    const visibleAfter = await visibleRows()
    assert(visibleAfter === 6, `展开后全部可见：${visibleAfter}`)

    const hud = (await q('.art-rest-hd', 'multi').textContent()) ?? ''
    assert(/显示 6 \/ 6/.test(hud), `展开后计数同步：${hud}`)

    await page.locator('[data-case="multi"] .art-ft button', { hasText: '收起' }).first().click()
    await page.waitForTimeout(250)
    const back = await visibleRows()
    assert(back === 4, `收回后恢复 4 条：${back}`)
  }

  // ── ⑤ 点击路径 = 复制完整路径（相对 → 绝对化） ──
  {
    await page.context().grantPermissions(['clipboard-read', 'clipboard-write'])
    await page.locator('[data-case="single"] .art-path').click()
    await page.waitForTimeout(300)

    const clip = await page.evaluate(() => navigator.clipboard.readText())
    assert(clip === '/Users/dev/ws/out/flappy-bird.html', `复制绝对路径：${clip}`)

    const copied = await q('.art-path').evaluate((el) => el.classList.contains('copied'))
    assert(copied, '复制后有视觉反馈（.copied）')
    const cpText = (await q('.art-path .cp').textContent())?.trim()
    assert(cpText === '已复制', `反馈文案为「已复制」：${cpText}`)
  }

  // ── ⑥ 按扩展名分派主按钮 ──
  {
    // 文本类（README.md）→ 预览
    const mdPrimary = (await page.locator('[data-case="multi"] .art-rest .art', { hasText: 'README.md' }).locator('.btn').first().textContent())?.trim()
    assert(mdPrimary === '预览', `.md 主按钮 = 预览：${mdPrimary}`)
    // xlsx 已单独成类（表格预览）→ 本节只钉「它不再落到 default 分支」，
    // 主/次按钮与点击链路的完整断言见 ⑩b（那里有专门的 XLSX case）。
    const xlsPrimary = (await page.locator('[data-case="multi"] .art-rest .art', { hasText: 'sales.xlsx' }).locator('.btn').first().textContent())?.trim()
    assert(xlsPrimary === '预览表格', `.xlsx 主按钮 = 预览表格（不再走「用默认程序打开」）：${xlsPrimary}`)
  }

  // ── ⑥b PDF 分派：主按钮=预览 PDF，次按钮保留「用默认程序打开」 ──
  //
  // 为什么单独锁：PDF 若并进 BROWSER_EXTS，主按钮会变成「在浏览器打开」——文案与语义
  // 都不对（那是「打开网页」，不是「预览文档」）。同时「用默认程序打开」不得因为主按钮
  // 改成预览而**消失**（内嵌 viewer 看不了批注/导出，外部应用是必要出口）。
  {
    const hero = page.locator('[data-case="pdf"] .art-hero')
    // 排除 .btn.icon（在 Finder 中显示那个图标按钮，所有类型都有）：
    // 文本按钮 = 主按钮 + PDF 次按钮 = 2
    const textBtns = hero.locator('.art-acts .btn:not(.icon)')
    assert(await textBtns.count() === 2, `PDF 文本按钮 = 主按钮 + 次按钮（count=${await textBtns.count()}）`)

    // 主按钮：文案 + primary 样式（hero 常显主按钮）
    const mainBtn = textBtns.first()
    assert((await mainBtn.textContent())?.trim() === '预览 PDF', `PDF 主按钮 = 预览 PDF：${(await mainBtn.textContent())?.trim()}`)
    assert((await mainBtn.getAttribute('class'))?.includes('primary') === true, 'PDF 主按钮带 primary 样式')
    const mainTitle = await mainBtn.getAttribute('title')
    assert(mainTitle === '预览 PDF', `PDF 主按钮 title 用 art.previewPdf：${mainTitle}`)

    // 次按钮：必须仍是「用默认程序打开」（不能消失），且**不是** primary
    const secondBtn = textBtns.nth(1)
    const secondText = (await secondBtn.textContent())?.trim()
    assert(secondText === '用默认程序打开', `PDF 次按钮 = 用默认程序打开：${secondText}`)
    assert((await secondBtn.getAttribute('class'))?.includes('primary') === false, 'PDF 次按钮不带 primary 样式')

    // 图标：PDF 分支是折角文档（与浏览器窗口图标的矩形+标题栏区分开）
    const glyphPaths = await hero.locator('.art-tile .glyph svg path').count()
    assert(glyphPaths === 3, `PDF 图标走折角文档分支（path 数=${glyphPaths}）`)
    // ext 文案仍显示 pdf（磁贴下方）
    assert((await hero.locator('.art-tile .ext').textContent())?.trim() === 'pdf', 'PDF 磁贴显示扩展名 pdf')

    // 紧凑行同样分派（不只是 hero）
    const compactBtns = page.locator('[data-case="pdf"] .art-rest .art', { hasText: 'brief.pdf' }).locator('.art-acts .btn:not(.icon)')
    assert(await compactBtns.count() === 2, `PDF 紧凑行也是 2 个按钮（count=${await compactBtns.count()}）`)
    assert((await compactBtns.first().textContent())?.trim() === '预览 PDF', 'PDF 紧凑行主按钮 = 预览 PDF')

    // 点击主按钮 → 走内嵌浏览器通道（真实 store action），**不是** openFile 文本预览。
    // 探针跑在普通 Chrome 里（window.desktop 不存在），故注入一个假 embed API 来驱动
    // 真实 store 代码路径：只替掉最外层的 IPC 边界，openHtmlInBrowser 的逻辑全真跑。
    await page.evaluate(() => {
      window.__calls = []
      // R2 之后预览走 store.openInBrowserTab：先 embedStatus 查池去重，再 embedOpenTarget **新建标签**。
      // 旧路径（embedShow + embedNavigate）仍保留桩：其它代码路径还会调，缺了会 TypeError。
      window.desktop = {
        embedStatus: async () => ({ ok: true, targets: [] }), // 空池 → 无去重命中 → 新建
        // 面板（RightPanel）现在**总是**尝试挂载原生内嵌视图（本地预览不依赖浏览器插件），
        // 于是它会调用 embedHide/embedResize/embedDebuggerProxy/embedCommand —— 部分桩会让
        // effect 抛 "is not a function" 并把整棵 React 树卸载（实测过）。真实 Electron 里这些
        // API 恒在；探针渲染的是真组件，桩就得够真实。
        embedHide: async () => ({ ok: true }),
        embedResize: async () => ({ ok: true }),
        embedDebuggerProxy: async () => ({ ok: true, endpoint: '127.0.0.1:1' }),
        embedCommand: async () => ({ ok: true }),
        embedOpenTarget: async (u) => { window.__calls.push('embedOpenTarget:' + u); return { ok: true, state: {} } },
        embedActivate: async () => ({ ok: true, state: {} }),
        embedShow: async () => ({ ok: true }),
        embedNavigate: async (u) => { window.__calls.push('embedNavigate:' + u); return { ok: true } },
      }
      const st = window.__store
      const origOpen = st.getState().openFile
      st.setState({ openFile: (...a) => { window.__calls.push('openFile'); return origOpen(...a) } })
    })
    await mainBtn.click()
    await page.waitForTimeout(400)
    const calls = await page.evaluate(() => window.__calls)
    assert(calls.some((c) => c.startsWith('embedOpenTarget:file://')), `PDF 预览走内嵌浏览器 file://（calls=${JSON.stringify(calls)}）`)
    assert(!calls.includes('openFile'), `PDF 预览不走文本预览通道（calls=${JSON.stringify(calls)}）`)
    // R2 语义：预览必须**新建标签**，不得退回「复用当前视图」（embedShow + embedNavigate）——
    // 那正是缺陷 A（预览 A → 预览 B = B 顶掉 A、无法返回）。这条防止后续改动把 R2 悄悄退回旧路径。
    assert(!calls.some((c) => c.startsWith('embedNavigate:')), `PDF 预览不得退回复用单视图的旧路径（calls=${JSON.stringify(calls)}）`)
    // file:// URL 必须是绝对路径（相对 out/report.pdf → workspace + 路径）
    const navUrl = calls.find((c) => c.startsWith('embedOpenTarget:'))?.slice('embedOpenTarget:'.length)
    assert(navUrl === 'file:///Users/dev/ws/out/report.pdf', `PDF 预览 URL 已绝对化：${navUrl}`)

    // 文件不存在：embedOpenTarget 报错 → 必须有**用户可见**的错误提示（不静默白面板）。
    // 这是 embed.ts 侧改动（错误上抛）配套的渲染层断言：只上抛、不 toast 等于没修。
    await page.evaluate(() => {
      window.__calls = []
      window.desktop.embedOpenTarget = async (u) => { window.__calls.push('embedOpenTarget:' + u); return { ok: false, error: 'ERR_FILE_NOT_FOUND (-6)' } }
      window.__store.setState({ toast: null })
    })
    await mainBtn.click()
    await page.waitForTimeout(400)
    const failed = await page.evaluate(() => ({ toast: window.__store.getState().toast, calls: window.__calls }))
    assert(failed.calls.some((c) => c.startsWith('embedOpenTarget:')), `无效 PDF 路径仍发起打开（calls=${JSON.stringify(failed.calls)}）`)
    assert(failed.toast !== null && failed.toast !== undefined, 'PDF 打开失败时弹出 toast（不静默）')
    assert(/report\.pdf/.test(String(failed.toast?.msg ?? '')), `toast 指明是哪个文件失败：${failed.toast?.msg}`)
    assert((failed.toast?.kind ?? '') === 'error', `失败 toast 为 error 类型：${failed.toast?.kind}`)
  }

  // ── ⑦ 空值维度不展示（append 增量语义）+ 工作区外绝对路径 ──
  {
    // log.txt 是 appendDelta 的 hero（首个产出）
    const logRow = page.locator('[data-case="appendDelta"] .art-hero')
    const meta = (await logRow.locator('.hero-meta').textContent())?.trim() ?? ''
    assert(meta === '', `size/lines 为空时不显示该维度（meta=${JSON.stringify(meta)}）`)
    const stat = (await logRow.locator('.art-stat').textContent()) ?? ''
    assert(stat.includes('+3'), `但 ± 统计仍展示：${stat}`)

    // 工作区外绝对路径：复制得到原样绝对路径
    await page.locator('[data-case="appendDelta"] .art-rest .art', { hasText: 'abs.txt' }).locator('.art-path').click()
    await page.waitForTimeout(300)
    const clip = await page.evaluate(() => navigator.clipboard.readText())
    assert(clip === '/tmp/elsewhere/abs.txt', `工作区外保留绝对路径：${clip}`)
  }

  // ── ⑧ 同内容去重标注 ──
  {
    const dup = page.locator('[data-case="dup"] .art-dup')
    assert(await dup.count() === 1, `仅第二个同内容项带标注（count=${await dup.count()}）`)
    const txt = (await dup.textContent())?.trim() ?? ''
    assert(txt.includes('a.html'), `标注指向首个同内容项：${txt}`)
  }

  // ── ⑨ 截断提示 ──
  {
    const warn = page.locator('[data-case="trunc"] .art-ft .warn')
    assert(await warn.count() === 1, '截断时显示警告标记')
    const txt = (await warn.textContent()) ?? ''
    assert(/未列出/.test(txt), `截断提示文案：${txt}`)
  }

  // ── ⑩ 空清单不渲染 ──
  {
    const card = page.locator('[data-case="empty"] .artifacts-card')
    assert(await card.count() === 0, '空清单不渲染卡片')
  }

  // ── ⑩b xlsx/xls 分派：主按钮=预览表格（转 HTML → 内嵌浏览器），次按钮保留 ──
  //
  // 为什么单独锁：xlsx 是二进制容器，**不能**走 openFile 的文本预览（会显示乱码），
  // 必须走 store.previewXlsx（bridge 受管 Python 转 HTML → openHtmlInBrowser）。
  // 与 PDF 同理，「用默认程序打开」不得因为主按钮变成预览而消失。
  {
    const hero = page.locator('[data-case="xlsx"] .art-hero')
    const textBtns = hero.locator('.art-acts .btn:not(.icon)')
    assert(await textBtns.count() === 2, `xlsx 文本按钮 = 主按钮 + 次按钮（count=${await textBtns.count()}）`)

    const mainBtn = textBtns.first()
    assert((await mainBtn.textContent())?.trim() === '预览表格', `xlsx 主按钮 = 预览表格：${(await mainBtn.textContent())?.trim()}`)
    assert((await mainBtn.getAttribute('class'))?.includes('primary') === true, 'xlsx 主按钮带 primary 样式')
    assert(await mainBtn.getAttribute('title') === '预览表格', `xlsx 主按钮 title 用 art.previewXlsx：${await mainBtn.getAttribute('title')}`)

    const secondBtn = textBtns.nth(1)
    assert((await secondBtn.textContent())?.trim() === '用默认程序打开', `xlsx 次按钮 = 用默认程序打开：${(await secondBtn.textContent())?.trim()}`)
    assert((await secondBtn.getAttribute('class'))?.includes('primary') === false, 'xlsx 次按钮不带 primary 样式')

    // .xls（OLE2 老格式，openpyxl 读不了）也走同一支：文案如实指向预览，
    // 失败时由 bridge 给出明确原因（比「用默认程序打开」更符合点击预期）。
    const legacyBtns = page.locator('[data-case="xlsx"] .art-rest .art', { hasText: 'legacy.xls' }).locator('.art-acts .btn:not(.icon)')
    assert(await legacyBtns.count() === 2, `xls 也是 2 个按钮（count=${await legacyBtns.count()}）`)
    assert((await legacyBtns.first().textContent())?.trim() === '预览表格', 'xls 主按钮 = 预览表格')

    // 点击主按钮 → 走 bridge 转换 + 内嵌浏览器通道，**不是** openFile 文本预览。
    // 探针跑在普通 Chrome（无 window.desktop）：注入假 watchXlsx + 假 embed API，
    // 只替掉最外层 IPC 边界，store.previewXlsx 的逻辑全真跑。
    await page.evaluate(() => {
      window.__calls = []
      window.desktop = {
        watchXlsx: async (req) => {
          window.__calls.push('watchXlsx:' + JSON.stringify(req))
          // bridge 侧产物是临时文件的**绝对路径**（受管运行时 tmp 下）
          return { ok: true, html_path: '/tmp/xlsx-preview-abc/preview.html' }
        },
        // R2 之后预览走 openInBrowserTab（embedStatus 去重 + embedOpenTarget 新建标签）；
        // 旧路径保留桩，见上方 PDF 用例的同款说明。
        embedStatus: async () => ({ ok: true, targets: [] }),
        // 面板（RightPanel）现在**总是**尝试挂载原生内嵌视图（本地预览不依赖浏览器插件），
        // 于是它会调用 embedHide/embedResize/embedDebuggerProxy/embedCommand —— 部分桩会让
        // effect 抛 "is not a function" 并把整棵 React 树卸载（实测过）。真实 Electron 里这些
        // API 恒在；探针渲染的是真组件，桩就得够真实。
        embedHide: async () => ({ ok: true }),
        embedResize: async () => ({ ok: true }),
        embedDebuggerProxy: async () => ({ ok: true, endpoint: '127.0.0.1:1' }),
        embedCommand: async () => ({ ok: true }),
        embedOpenTarget: async (u) => { window.__calls.push('embedOpenTarget:' + u); return { ok: true, state: {} } },
        embedActivate: async () => ({ ok: true, state: {} }),
        embedShow: async () => ({ ok: true }),
        embedNavigate: async (u) => { window.__calls.push('embedNavigate:' + u); return { ok: true } },
      }
      const st = window.__store
      const origOpen = st.getState().openFile
      st.setState({ openFile: (...a) => { window.__calls.push('openFile'); return origOpen(...a) } })
    })
    await mainBtn.click()
    await page.waitForTimeout(500)
    const calls = await page.evaluate(() => window.__calls)
    assert(
      calls.some((c) => c.startsWith('watchXlsx:') && c.includes('"path":"out/sales.xlsx"')),
      `xlsx 预览调用 bridge 转换并传相对路径（calls=${JSON.stringify(calls)}）`,
    )
    assert(calls.some((c) => c.startsWith('embedOpenTarget:file://') && c.includes('xlsx-preview-abc/preview.html')), `xlsx 预览走内嵌浏览器（calls=${JSON.stringify(calls)}）`)
    const navUrl = calls.find((c) => c.startsWith('embedOpenTarget:'))?.slice('embedOpenTarget:'.length)
    assert(navUrl === 'file:///tmp/xlsx-preview-abc/preview.html', `产物按 file:// 绝对路径打开：${navUrl}`)
    assert(!calls.includes('openFile'), `xlsx 预览不走文本预览通道（calls=${JSON.stringify(calls)}）`)
    // 转换在途态已复位（按钮文案回到「预览表格」，不是停在「转换中…」）
    assert((await mainBtn.textContent())?.trim() === '预览表格', '转换结束后按钮回到「预览表格」')

    // 失败：bridge 报错 → 必须 toast 出原因（不静默）。
    await page.evaluate(() => {
      window.__calls = []
      window.desktop.watchXlsx = async () => ({ ok: false, error: 'managed Python runtime unavailable: pip 断网' })
      window.__store.setState({ toast: null })
    })
    await mainBtn.click()
    await page.waitForTimeout(400)
    const failed = await page.evaluate(() => ({ toast: window.__store.getState().toast, busy: window.__store.getState().xlsxBusy ?? null }))
    assert(failed.toast !== null && failed.toast !== undefined, 'xlsx 转换失败时弹出 toast（不静默）')
    assert(/sales\.xlsx/.test(String(failed.toast?.msg ?? '')), `toast 指明是哪个文件失败：${failed.toast?.msg}`)
    assert(/pip 断网/.test(String(failed.toast?.msg ?? '')), `toast 带上 bridge 的真实原因：${failed.toast?.msg}`)
    assert((failed.toast?.kind ?? '') === 'error', '失败 toast 为 error 类型')
    assert(failed.busy === null, '失败后转换态复位（按钮不永久禁用）')

    // 并发：A 的表还在转（bridge 未回），点 B 的按钮**不得**被静默吞掉。
    // 这条对应一次设计返工：最初写成「有在途转换就 return」，用户在等 A 时点 B
    // 表现为完全没反应（按钮文案也不变）—— 只有同路径才该挡。
    await page.evaluate(() => {
      window.__calls = []
      window.__pending = []
      window.__store.setState({ toast: null, xlsxBusy: undefined })
      window.desktop.watchXlsx = (req) => new Promise((resolve) => {
        window.__calls.push('watchXlsx:' + req.path)
        window.__pending.push({ req, resolve })
      })
      window.desktop.embedOpenTarget = async (u) => { window.__calls.push('embedOpenTarget:' + u); return { ok: true, state: {} } }
    })
    await mainBtn.click() // A（out/sales.xlsx）挂起
    await page.waitForTimeout(150)
    const secondCard = page.locator('[data-case="xlsx"] .art-rest .art', { hasText: 'sales.xlsx' }).locator('.art-acts .btn').first()
    assert(await mainBtn.count() === 1, 'hero 主按钮仍在')
    // 用紧凑列表里另一张（legacy.xls）表按钮：点它必须也发起请求
    const legacyMain = page.locator('[data-case="xlsx"] .art-rest .art', { hasText: 'legacy.xls' }).locator('.art-acts .btn').first()
    await legacyMain.click()
    await page.waitForTimeout(200)
    const inflight = await page.evaluate(() => window.__calls)
    assert(
      inflight.filter((c) => c.startsWith('watchXlsx:')).length === 2,
      `A 在途时点 B 也必须发起转换（不被静默吞掉）：${JSON.stringify(inflight)}`,
    )
    // 放行两个请求：都必须真的导航到各自的产物（不互相踩）
    await page.evaluate(() => {
      const pend = window.__pending.splice(0)
      pend.forEach(({ req, resolve }, i) => resolve({ ok: true, html_path: `/tmp/xlsx-preview-p${i}/preview.html` }))
      window.__store.setState({ xlsxBusy: undefined })
    })
    await page.waitForTimeout(500)
    const done = await page.evaluate(() => window.__calls.filter((c) => c.startsWith('embedOpenTarget:')))
    assert(done.length === 2, `两次转换都完成打开（不互相取消）：${JSON.stringify(done)}`)
    assert(new Set(done).size === 2, `两个产物各自打开自己的文件：${JSON.stringify(done)}`)
  }

  // ── ⑪ 点「预览」→ 右侧栏真的渲染文件内容（不是停在「正在读取文件…」） ──
  //
  // 这是最关键的一条：此前 e2e 只断言「预览」按钮的**文案**，没验证点击后的真实行为，
  // 于是漏掉了真实缺陷——产出卡片传绝对路径，而文件活动列表用相对路径，
  // RightPanel 的字面比较失配 → 右侧栏永远停在「正在读取文件…」。
  {
    const readingSel = '.file-activity-empty'
    // 逐个类型点预览，确认都能渲染出内容（覆盖 .json 与常见文本类型）
    for (const [ext, name] of [['json', 'data.json'], ['md', 'README2.md'], ['ts', 'api2.ts'], ['csv', 'rows.csv'], ['txt', 'notes2.txt']]) {
      // 用单条样本卡片，点其预览按钮
      await page.evaluate(({ n }) => {
        const st = window.__store
        st.setState({
          rightExpanded: true, rightTab: 'file',
          blocks: [{ kind: 'tool', id: 900, toolId: 't9', name: 'write_file', args: JSON.stringify({ path: n }), result: 'written', status: 'done' }],
          fileFocus: undefined, filePreviews: {},
        })
      }, { n: `${name}` })
      await page.waitForTimeout(150)

      // 构造一个只含该文件的产出卡片并点它的预览按钮
      await page.evaluate(({ n }) => {
        const holder = document.createElement('div')
        holder.id = '__preview_probe'
        document.body.appendChild(holder)
      }, { n: name })
      // 直接用组件点击路径：改为调用 store 的 openFile（等价于点「预览」按钮的行为）
      await page.evaluate(({ n }) => {
        window.__store.getState().openFile(n, undefined, 'source')
      }, { n: name })
      await page.waitForTimeout(400)

      const st = await page.evaluate(() => ({
        reading: document.querySelector('.file-activity-empty')?.textContent ?? null,
        hasSource: !!document.querySelector('.file-source'),
        // 渲染容器有三条（「最终文件」按文件类型分派）：
        //   .md-preview-doc（Markdown 预览）/ .csv-preview（CSV 表格，csv.ts + CsvPreview）
        //   / .fp-code（其余文本的高亮源码）——任一即算「渲染出内容」。
        codeText: (document.querySelector('.file-source .md-preview-doc') ?? document.querySelector('.csv-preview') ?? document.querySelector('.fp-code'))?.textContent?.slice(0, 30) ?? null,
        previewPath: Object.values(window.__store.getState().filePreviews).map((v) => v.path).join(',') || null,
        focusPath: window.__store.getState().fileFocus?.path ?? null,
      }))
      if (st.reading !== null) {
        const diag = await page.evaluate(() => ({
          activities: window.__store.getState().blocks.filter((b) => b.kind === 'tool').map((b) => b.name),
          focus: window.__store.getState().fileFocus,
          previews: window.__store.getState().filePreviews,
          // 组件内部 state 拿不到，但从 DOM 反推
          inspectorPath: document.querySelector('.file-inspector-path')?.textContent ?? null,
          tabsOn: [...document.querySelectorAll('.file-detail-tabs button')].map((b) => b.textContent + (b.className.includes('on') ? '*' : '')).join('|'),
        }))
        console.log(`  ⚠ ${ext} 诊断:`, JSON.stringify(diag))
      }
      assert(st.reading === null, `${ext}: 不得停在「正在读取文件…」（reading=${JSON.stringify(st.reading)}）`)
      assert(st.codeText !== null && st.codeText.length > 0, `${ext}: 渲染出文件内容（${JSON.stringify(st.codeText)}）`)
    }
  }

  // ── ⑫ 产出卡片预览按钮走同一条链路（传相对路径，与活动列表一致） ──
  //
  // 回归守卫：ArtifactCard 必须传 a.path（相对），不能传绝对路径——
  // 否则 RightPanel 的 path 匹配失配，右侧永远「正在读取文件…」。
  {
    const src = await page.evaluate(() => {
      // 通过真实点击产出卡片的「预览」按钮触发（而非直接调 store）
      const btn = document.querySelector('[data-case="multi"] .art-rest .art button')
      return btn ? btn.textContent?.trim() : null
    })
    // 找到标记为「预览」的按钮并点击（.md 那条）
    const previewBtn = page.locator('[data-case="multi"] .art-rest .art', { hasText: 'README.md' }).locator('button', { hasText: '预览' }).first()
    await previewBtn.click()
    await page.waitForTimeout(500)
    const after = await page.evaluate(() => ({
      reading: document.querySelector('.file-activity-empty')?.textContent ?? null,
      focusPath: window.__store.getState().fileFocus?.path ?? null,
      // .md 默认预览容器；.csv 默认表格容器（见「最终文件」的渲染分派）——任一即视为渲染出内容
      codeText: (document.querySelector('.file-source .md-preview-doc') ?? document.querySelector('.csv-preview') ?? document.querySelector('.fp-code'))?.textContent?.slice(0, 24) ?? null,
    }))
    assert(after.reading === null, `产出卡片预览：不得停在读取中（reading=${JSON.stringify(after.reading)}）`)
    // 关键：传的是相对路径（与活动列表一致），不是工作区绝对路径
    assert(after.focusPath === 'README.md', `产出卡片预览传相对路径（focusPath=${after.focusPath}）`)
    assert(after.codeText !== null, `产出卡片预览渲染出内容（${JSON.stringify(after.codeText)}）`)
    void src
  }

  // ── ⑬ 文案无未替换的占位符（i18n 缺 replace 会静默漏到用户面前） ──
  {
    const texts = await page.locator('.artifacts-card').allTextContents()
    const bad = texts.filter((t) => /\{\w+\}/.test(t))
    assert(bad.length === 0, `无未替换占位符（${bad.length ? bad.map((b) => b.match(/\{\w+\}/g)).join(',') : 'clean'}）`)
  }

  // ── 页面无脚本错误 ──
  assert(errs.length === 0, `无页面错误（${errs.length ? errs.join(' | ') : 'clean'}）`)

  await browser.close()
  console.log(`\n✅ artifacts 产出卡片：${pass} 项全部通过`)
}

main().then(() => {
  // 成功也必须**显式退出**：vite 子进程（stdio:'pipe'）持有事件循环 → 不 exit 就永远不返回
  // （表现为「打印 ✅ 全部通过 之后挂住」，且拿不到退出码 —— 脚本化的验收/CI 会一直等）。
  // `process.on('exit', cleanup)` 救不了这里：它只在进程真要退出时才触发，鸡生蛋。
  // 与 e2e/verify_preview_container.mjs、verify_preview_scoping.mjs 同款写法。
  cleanup()
  process.exit(0)
}).catch((e) => {
  const pre = isPrecondition(e)
  console.error(`\n${pre ? '⛔ 前置不满足（退出码 2，不是测试失败）' : '❌'} ` + (e?.message ?? e))
  cleanup()
  process.exit(exitCodeOf(e))
})
