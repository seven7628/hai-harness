#!/usr/bin/env node
// 验收（真实组件）：设置 →「外观」页 —— 代码主题 / 字体大小 / 背景颜色。
//
// 为什么每条断言都值得写（都是「实现错了会静默退化」的点）：
//   ① 导航里根本没有「外观」这一项 → 功能等于没做（用户找不到入口）；
//   ② 选了代码主题却只有**设置卡**变色、对话里的代码块不变 → 说明重渲依赖没接上
//      （markdown.tsx 的 effect 少 codeTheme 依赖时就是这个症状，肉眼很难发现）；
//   ③ 背景色覆盖挂在 <html> 的 inline 变量上，优先级高于 [data-theme='paper'] 规则 ——
//      切到深色主题时若不清掉，深色底会被刷成浅色（**这是本条最值钱的断言**）；
//   ④ 字体大小在 Electron 走原生缩放、浏览器退化为 html zoom：探针里能验的是
//      「偏好确实落盘 + 应用路径被调用」，原生缩放本身由 electron/ui:set-zoom 负责；
//   ⑤ 刷新后偏好要能自恢复（localStorage 水合 + 启动应用），否则「设置完重启就没了」。
//
// 用法：cd desktop/app && node e2e/verify_appearance_settings.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'node:child_process'
import net from 'node:net'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4194)
const SHOTS = path.join(__dirname, '_appearance_shots')
const PROBE_DIR = path.join(__dirname, '_appearanceprobe_run')
const URL_BASE = `http://localhost:${PORT}/e2e/_appearanceprobe_run/index.html`

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function waitPort(port, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => resolve(false))
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await sleep(250)
  }
}

// —— 探针页：真 SettingsModal + 真 Narrative（含 ts 代码块）+ 与 App.tsx 同序的主题/外观 effect ——
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.mkdirSync(SHOTS, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh"><head><meta charset="utf-8"><title>appearance probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { useEffect } from 'react'
import { createRoot } from 'react-dom/client'
import SettingsModal from '../../src/components/SettingsModal'
import Narrative from '../../src/components/Narrative'
import RightPanel from '../../src/components/RightPanel'
// ⚠️ 副作用注册，必须与 src/main.tsx 一致（04-review F45/F46）：少了这一行 registry 是空的，
// RightPanel 的 getSurface(kind)?.render(...) 恒走 ?? 兜底 —— 本探针验的一直是兜底组件
//（file 的兜底就是同一个 FilePreviewTab，像素一致、断言全绿）。下面 page 加载后的第一条
// 自检断言会当场戳穿这件事，补上这行才轮到真正的 provider 渲染。
import '../../src/surfaces/builtin'
import { getSurface } from '../../src/surfaces/registry'
import { DEFAULT_SETTINGS, useAppStore } from '../../src/store/useAppStore'
import { applyAppearance, useAppearance } from '../../src/lib/appearance'
import { applyTheme } from '../../src/lib/theme'
import '../../src/index.css'
import '../../src/narrative-skin.css'

// 与 App.tsx 同序：先 data-theme（applyTheme）再 applyAppearance（底色覆盖要看解析后的主题）。
function Shell() {
  const theme = useAppStore((s) => s.settings.theme)
  const ap = useAppearance()
  useEffect(() => {
    applyTheme(theme)
    applyAppearance(ap)
  }, [theme, ap])
  return null
}

// 右栏「最终文件」的预览：断言"切代码主题时它也要重渲"。
// 为什么不跟 Narrative 共用一份断言：两者是不同的渲染路径（.codeblock vs .fp-code），
// 2026-09-21 的缺陷正是"对话变了、右栏没变"——只测对话等于放过这个分支。
const DEMO_SRC = 'export const theme = {\\n  accent: "#1f1f23",\\n  surface: "paper",\\n};\\n'

useAppStore.setState({
  inSettings: true,
  settings: { ...DEFAULT_SETTINGS, theme: 'light' },
  activeSessionId: 'probe',
  workspace: '/tmp/probe-ws',
  rightExpanded: true,
  filePreviews: { 'demo.ts': { path: 'demo.ts', content: DEMO_SRC, lines: 4 } },
  tabsByScope: {
    probe: { tabs: [{ id: 'tp_file', kind: 'file', path: 'demo.ts', mode: 'source', openSeq: 1 }], activeId: 'tp_file', closed: [] },
  },
  blocks: [
    { kind: 'assistant', id: 1, streaming: false, thinking: '', text: '\\u4ee3\\u7801\\u5757\\uff1a\\n\\n\`\`\`ts\\nconst themePreview: ThemeConfig = {\\n  accent: "#2563eb",\\n};\\n\`\`\`' },
  ],
} as never)

;(window as unknown as { __surfaces: unknown }).__surfaces = {
  file: !!getSurface('file'), git: !!getSurface('git'), web: !!getSurface('web'), activity: !!getSurface('activity'),
}

createRoot(document.getElementById('root')!).render(
  <>
    <Shell />
    <div style={{ height: '100%', display: 'flex', flexDirection: 'column' }}><Narrative /></div>
    <div id="probe-right" style={{ position: 'fixed', right: 0, top: 0, height: '100%', width: 420, display: 'flex' }}>
      <RightPanel />
    </div>
    <SettingsModal />
  </>,
)
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'),
  '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')],
  { cwd: APP, stdio: 'ignore' })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)
const watchdog = setTimeout(() => {
  console.error('\n❌ 超时（180s）：vite / chromium 未按预期完成')
  process.exit(1)
}, 180_000)
watchdog.unref()

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1180, height: 860 }, deviceScaleFactor: 2 })
const errors = []
page.on('pageerror', (e) => errors.push(String(e)))
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })

// 对话区第一个代码块里某个 token 的颜色 —— 「主题真的变了」的可观测证据。
const tokenColor = () => page.evaluate(() => {
  const s = document.querySelector('.codeblock .shiki span[style*="color"]')
  return s ? getComputedStyle(s).color : ''
})

// 右栏「最终文件」预览里某个 token 的颜色（另一条渲染路径：.fp-code）。
// 与 .codeblock 分开取：2026-09-21 的缺陷是"对话重渲了、右栏停在旧主题"。
const rightPanelTokenColor = () => page.evaluate(() => {
  const s = document.querySelector('.fp-code .shiki span[style*="color"]')
  return s ? getComputedStyle(s).color : ''
})

// 从页面里调真实模块改偏好（用于模拟"外部改设置"的路径，例如残留态清理）
const setAppearanceFromPage = (page, patch) => page.evaluate(
  (p) => import('/src/lib/appearance.ts').then((m) => m.setAppearance(p)),
  patch,
)

const readLs = () => page.evaluate(() => {
  const raw = localStorage.getItem('go-code.appearance')
  return raw ? JSON.parse(raw) : null
})
const paperVar = () => page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--paper').trim())

// 计算两个 rgb(...) 的对比度 —— 用来断言"卡片边界真的看得见"，而不是"样式里写了个值"
// 计算颜色对比度。**要兼容两种计算值形态**：普通 token 是 `rgb(211, 211, 211)`，
// 而 color-mix() 的结果 Chromium 回的是 `color(srgb 0.826 0.826 0.826)`（0~1 浮点）——
// 只认前者时断言会因为"解析不到"而假失败（本轮就踩了一次）。
const contrastRgb = (a, b) => {
  const channels = (c) => {
    const str = c.trim()
    // 十六进制：**自定义属性**（--ink 这类）getComputedStyle 回的是原始 token 值，不是 rgb()
    const hex = /^#([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(str)
    if (hex) {
      const h = hex[1].length === 3 ? hex[1].split('').map((x) => x + x).join('') : hex[1]
      return [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16))
    }
    const head = (str.match(/-?\d+(\.\d+)?/g)?.map(Number) ?? []).slice(0, 3)
    return /^color\(/.test(str) ? head.map((v) => v * 255) : head
  }
  const lum = (c) => {
    const [r, g, bl] = channels(c).map((v) => {
      const x = v / 255
      return x <= 0.04045 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4
    })
    return 0.2126 * r + 0.7152 * g + 0.0722 * bl
  }
  const [x, y] = [lum(a), lum(b)]
  return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05)
}
// 主题各自的**声明底色**（src/index.css 的 [data-theme='paper'|'ink'] 块）。改主题时同步这两行 ——
// "覆盖被清掉"的判据是"回到主题默认"，不是"等于白色"：亮色默认 2026-09-21 起是 #fdfcfb（朱砂皮）。
const PAPER_DEFAULT = '#fdfcfb'
const INK_DEFAULT = '#1a1614'
// 真正的"覆盖已清"证据是 <html> 上没有 inline 变量残留（inline 优先级高于主题规则）
const inlinePaperVar = () => page.evaluate(() => document.documentElement.style.getPropertyValue('--paper').trim())

try {
  await waitPort(PORT)
  await page.goto(URL_BASE)
  await page.waitForSelector('.settings-nav-item', { timeout: 20000 })
  await page.waitForSelector('.codeblock .shiki', { timeout: 20000 })

  // —— 仪器自检（04-review F45/F46）——
  // RightPanel 的内容面板一律是 getSurface(kind)?.render(...) ?? 本地组件。本探针页只
  // import RightPanel 时注册表是空的 → 验的一直是 **兜底组件**，而兜底渲染的往往是同一个
  // 组件（file → FilePreviewTab），像素一致、断言全绿 —— 这是最坏的一种失败：看不出坏。
  // 所以先钉住「provider 真的在注册表里」：缺 import 时这里当场红，而不是让下面的面板断言
  // 继续在兜底分支上假绿。
  const surfaces = await page.evaluate(() => window.__surfaces ?? null)
  assert(!!surfaces?.file, `仪器：builtin provider 已注册（缺 builtin import 时验的是 ?? 兜底；实得 ${JSON.stringify(surfaces)}）`)

  // ① 入口在导航里
  const navLabels = await page.$$eval('.settings-nav-item', (els) => els.map((e) => e.textContent?.trim() ?? ''))
  assert(navLabels.includes('外观'), `设置导航里有「外观」入口（实得：${navLabels.join(' / ')}）`)
  await page.click('.settings-nav-item:has-text("外观")')
  await page.waitForSelector('.ct-grid', { timeout: 10000 })
  const gridCount = await page.locator('.ct-grid').count()
  const lightCards = await page.locator('.ct-grid').first().locator('.ct-card').count()
  assert(gridCount === 2, `亮/暗两组代码主题（实得 ${gridCount} 组）`)
  assert(lightCards === 6, `亮色侧就是 code-theme-compare 里那 6 套（实得 ${lightCards} 套）`)
  const darkCards = await page.locator('.ct-grid').nth(1).locator('.ct-card').count()
  assert(darkCards === 5, `深色侧 5 套（含默认 GitHub Dark，实得 ${darkCards} 套）`)

  const rowLabels = () => page.$$eval('.set-row .set-label', (els) => els.map((e) => e.textContent?.trim() ?? ''))
  const inAppearance = await rowLabels()
  assert(inAppearance.includes('主题') && inAppearance.includes('会话页样式'),
    `主题与会话页样式都在「外观」里（实得：${inAppearance.join(' / ')}）`)
  await page.click('.settings-nav-item:has-text("通用设置")')
  await page.waitForSelector('.set-row', { timeout: 10000 })
  await sleep(200)
  await page.screenshot({ path: path.join(SHOTS, 'general-tab.png'), fullPage: true })
  const inGeneral = await rowLabels()
  assert(!inGeneral.includes('主题') && !inGeneral.includes('会话页样式'),
    `「通用设置」里已移除主题/会话页样式（实得：${inGeneral.join(' / ')}）`)
  await page.click('.settings-nav-item:has-text("外观")')
  await page.waitForSelector('.ct-grid', { timeout: 10000 })

  // ② 选代码主题 → 对话里的代码块必须真的重渲（不是只有卡片自己变）
  await sleep(400)
  await page.screenshot({ path: path.join(SHOTS, 'appearance-tab.png'), fullPage: true })
  const before = await tokenColor()
  const beforeCard = await page.locator('.ct-card.cur .ct-card-name').first().textContent()
  assert(beforeCard?.trim() === 'Catppuccin Latte', `默认代码主题 = Catppuccin Latte（柔和化后的默认，实得 ${beforeCard?.trim()}）`)
  await page.waitForSelector('.fp-code .shiki', { timeout: 20000 })
  const beforeRight = await rightPanelTokenColor()
  assert(beforeRight !== '', `右栏「最终文件」预览已上高亮（token 颜色 ${beforeRight}）`)
  await page.locator('.ct-card', { hasText: 'Vitesse Light' }).first().click()
  await page.waitForFunction(
    (prev) => {
      const s = document.querySelector('.codeblock .shiki span[style*="color"]')
      return !!s && getComputedStyle(s).color !== prev
    },
    before,
    { timeout: 10000 },
  )
  const afterSwitch = await tokenColor()
  ok(`对话代码块随主题重渲（token 颜色 ${before} → ${afterSwitch}）`)
  // ②b 右栏预览是同一条偏好的另一条渲染路径 —— 漏接依赖时它**不会**跟着变（本轮修的缺陷）。
  await page.waitForFunction(
    (prev) => {
      const s = document.querySelector('.fp-code .shiki span[style*="color"]')
      return !!s && getComputedStyle(s).color !== prev
    },
    beforeRight,
    { timeout: 10000 },
  ).catch(() => { throw new Error(`断言失败: 右栏预览未跟随代码主题重渲（仍是 ${beforeRight}）`) })
  const afterRight = await rightPanelTokenColor()
  ok(`右栏预览随主题重渲（token 颜色 ${beforeRight} → ${afterRight}）`)
  assert((await readLs())?.codeThemeLight === 'vitesse-light', '代码主题落盘（localStorage.go-code.appearance）')
  await page.screenshot({ path: path.join(SHOTS, 'appearance-tab-vitesse.png'), fullPage: true })
  await page.evaluate(() => { const el = document.querySelector('.settings-body, .settings-content, .settings-main'); if (el) el.scrollTop = el.scrollHeight })
  await sleep(300)
  await page.screenshot({ path: path.join(SHOTS, 'appearance-tab-bottom.png'), fullPage: true })

  // ②c **每一张候选卡都真的可选**（2026-09-21 修掉的缺陷：亮色第一张 One Light 点不动）。
  // 为什么必须逐张点、并断言**落盘值**：这个缺陷不报错、界面也不变 —— 点下去被 normalize
  // 静默打回默认值，用户的体感是"卡点了没反应"。只断言"默认值对"或"某一张能切"都会放过它；
  // 而且名单从模块里读（不是硬编码），所以将来加/删候选也会被这条覆盖。
  {
    const lists = await page.evaluate(() => import('/src/lib/appearance.ts').then((m) => ({
      light: m.LIGHT_CODE_THEMES.map((x) => ({ id: x.id, label: x.label })),
      dark: m.DARK_CODE_THEMES.map((x) => ({ id: x.id, label: x.label })),
    })))
    const beforePick = await readLs()
    for (const [grid, key, items] of [[0, 'codeThemeLight', lists.light], [1, 'codeThemeDark', lists.dark]]) {
      for (let i = 0; i < items.length; i++) {
        const { id, label } = items[i]
        await page.locator('.ct-grid').nth(grid).locator('.ct-card').nth(i).click()
        // 等到落盘值 = 刚点的那一项；超时说明这一张是"死按钮"
        const landed = await page.waitForFunction(
          ([k, v]) => {
            try { return JSON.parse(localStorage.getItem('go-code.appearance') || '{}')[k] === v } catch { return false }
          },
          [key, id],
          { timeout: 8000 },
        ).then(() => true).catch(() => false)
        const now = (await readLs())?.[key]
        assert(landed, `${key === 'codeThemeLight' ? '亮色' : '深色'}候选「${label}」可选中（点后落盘 ${key}=${now}，期望 ${id}）`)
        // 界面也要跟上：选中态必须落在这张卡上（切换真的传到渲染层，不只是写了个值）
        assert(await page.locator('.ct-grid').nth(grid).locator('.ct-card').nth(i).getAttribute('aria-pressed') === 'true',
          `「${label}」选中态落在该卡上（aria-pressed=true）`)
      }
    }
    // 复位到进这一块之前的选择：后面的步骤（底色/缩放/刷新持久化）都依赖亮色侧是 vitesse-light
    await setAppearanceFromPage(page, { codeThemeLight: beforePick.codeThemeLight, codeThemeDark: beforePick.codeThemeDark })
    await sleep(300)
    assert((await readLs())?.codeThemeLight === beforePick.codeThemeLight, `候选项巡检后复位（codeThemeLight=${beforePick.codeThemeLight}）`)
  }

  // ②d 代码主题卡**预览的是"那一侧主题下的代码块"**，不随当前主题变（2026-09-21 用户报障：
  //     「这里应该是显示亮色的时候代码块是什么样式，而不是随主体变化而变化」）。
  //     判据不能只看"卡片有底色"：修之前亮色卡是 transparent（语法色落在**当前**卡面上），
  //     所以这里断言 ①代码面是**不透明的浅色**（= 亮色那张纸）②卡里的语法 token 在该面上够读。
  {
    await page.click('.set-row .seg button:text-is("深色")')
    await page.waitForFunction(() => document.documentElement.getAttribute('data-theme') === 'ink', null, { timeout: 5000 })
    await sleep(400) // 等亮色卡的 shiki 重新按主题渲染完
    const probe = await page.evaluate(() => {
      const grids = document.querySelectorAll('.ct-grid')
      const lightShiki = grids[0]?.querySelector('.ct-card:not(.dark) .ct-card-code .shiki')
      const darkShiki = grids[1]?.querySelector('.ct-card.dark .ct-card-code .shiki')
      const tokenOf = (root) => {
        const sp = root?.querySelector('span[style*="color"]')
        return sp ? getComputedStyle(sp).color : ''
      }
      return {
        lightSurface: lightShiki ? getComputedStyle(lightShiki).backgroundColor : '',
        lightToken: tokenOf(lightShiki),
        darkSurface: darkShiki ? getComputedStyle(darkShiki).backgroundColor : '',
        appTheme: document.documentElement.getAttribute('data-theme'),
      }
    })
    assert(probe.appTheme === 'ink', `探针处于深色主题（data-theme=${probe.appTheme}）`)
    assert(/^rgb/.test(probe.lightSurface) && !/rgba\(0, 0, 0, 0\)/.test(probe.lightSurface),
      `深色主题下亮色卡仍有自己的代码面（不是 transparent，实得 ${probe.lightSurface || '(空)'}）`)
    assert(contrastRgb(probe.lightSurface, 'rgb(0, 0, 0)') > 8,
      `该代码面是**亮色**那张纸（${probe.lightSurface} 与黑对比 ${contrastRgb(probe.lightSurface, 'rgb(0, 0, 0)').toFixed(1)}:1）`)
    const tokenRatio = contrastRgb(probe.lightToken, probe.lightSurface)
    assert(tokenRatio >= 4.5,
      `亮色卡的语法色在亮色代码面上够读（${probe.lightToken} on ${probe.lightSurface} = ${tokenRatio.toFixed(2)}:1）`)
    assert(contrastRgb(probe.darkSurface, 'rgb(255, 255, 255)') > 8,
      `深色卡的代码面仍是深色（${probe.darkSurface}）`)
    await page.click('.set-row .seg button:text-is("亮色")')
    await page.waitForFunction(() => document.documentElement.getAttribute('data-theme') === 'paper', null, { timeout: 5000 })
    await sleep(250)
  }

  // ③ 背景颜色：挂上 + 只作用于亮色主题（切深色必须清掉，否则深色底被刷白）
  await page.click('.bg-sw[title="#f5f5f5"]')
  await page.waitForFunction(() => getComputedStyle(document.documentElement).getPropertyValue('--paper').trim() === '#f5f5f5', null, { timeout: 5000 })
  assert(await paperVar() === '#f5f5f5', `背景色覆盖生效（--paper = ${await paperVar()}）`)
  const bodyBg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor)
  assert(bodyBg === 'rgb(245, 245, 245)', `页面底色跟着变（body background = ${bodyBg}）`)

  // ③b 自选底色必须**连带派生**卡面与发丝线（2026-09-21 用户报障的缺陷：
  //     「亮色下 artifacts 等边框看不出来」）。原因：只覆盖 --paper/--paper-2 时
  //     页面与卡面同色，而 --ink-line 还是主题自带的那支（为近白底 #FDFCFB 调的
  //     #EAE5E1）—— 底一换成灰，线与底几乎同亮度，整片糊在一起。
  //     所以这里断言的不是"变量被赋了值"，而是**渲染出来的边界真的看得见**。
  {
    const c = await page.evaluate(() => {
      const cs = getComputedStyle(document.documentElement)
      const card = document.querySelector('.ct-card') // 设置页里就有走 --border 的卡片
      const ccs = card ? getComputedStyle(card) : null
      return {
        paper: cs.getPropertyValue('--paper').trim(),
        paper2Inline: document.documentElement.style.getPropertyValue('--paper-2').trim(),
        cardBg: ccs?.backgroundColor ?? '',
        cardBorder: ccs?.borderTopColor ?? '',
        cardBorderWidth: ccs?.borderTopWidth ?? '',
      }
    })
    assert(/color-mix/.test(c.paper2Inline), `卡面由所选底色派生（inline --paper-2 = ${c.paper2Inline}）`)
    const ratio = contrastRgb(c.cardBorder, c.cardBg)
    assert(ratio >= 1.3, `卡片边界在自选底色上看得见（边框 ${c.cardBorder} vs 卡面 ${c.cardBg} = ${ratio.toFixed(2)}:1 ≥ 1.3）`)
    assert(c.cardBorderWidth === '1px', `边界仍是 1px 发丝线（${c.cardBorderWidth}）`)
    // 深色底方向也要跟着换（浅底上深线、深底上亮线）
    const derived = await page.evaluate(() => {
      const root = document.documentElement
      const before = root.style.getPropertyValue('--ink-line')
      return { before }
    })
    assert(/color-mix/.test(derived.before), `发丝线同样按底色派生（${derived.before}）`)

    // 深色方向：把底选成深色 → 文字换亮墨、线改为**提亮**方向、卡面比底更亮（分层不能丢）
    await setAppearanceFromPage(page, { lightBg: '#2b2b33' })
    await sleep(300)
    const dk = await page.evaluate(() => {
      const root = document.documentElement
      const card = document.querySelector('.ct-card')
      return {
        line: root.style.getPropertyValue('--ink-line'),
        ink: getComputedStyle(root).getPropertyValue('--ink').trim(),
        bodyBg: getComputedStyle(document.body).backgroundColor,
        cardBg: card ? getComputedStyle(card).backgroundColor : '',
      }
    })
    assert(/#fff|255, 255, 255/.test(dk.line), `深色自选底 → 发丝线改为提亮方向（${dk.line}）`)
    assert(contrastRgb(dk.ink, dk.bodyBg) >= 4.5, `深色自选底上文字够读（--ink ${dk.ink} vs 底 ${dk.bodyBg} = ${contrastRgb(dk.ink, dk.bodyBg).toFixed(2)}:1）`)
    assert(contrastRgb(dk.cardBg, dk.bodyBg) >= 1.05 && dk.cardBg !== dk.bodyBg,
      `深色自选底上卡面与底分得开（${dk.cardBg} vs ${dk.bodyBg} = ${contrastRgb(dk.cardBg, dk.bodyBg).toFixed(2)}:1）`)
    // 回浅色底，后面几步（缩放/刷新持久化）都用它
    await setAppearanceFromPage(page, { lightBg: '#f5f5f5' })
    await sleep(250)
  }
  await page.click('.set-row .seg button:text-is("深色")')
  await page.waitForFunction(() => document.documentElement.getAttribute('data-theme') === 'ink', null, { timeout: 5000 })
  assert(await paperVar() === INK_DEFAULT, `切深色后覆盖被清掉（--paper = ${await paperVar()}，不是残留的 #f5f5f5）`)
  assert((await readLs())?.lightBg === '#f5f5f5', '亮色底色偏好保留（切主题不清偏好）')
  await page.click('.set-row .seg button:text-is("亮色")')
  await page.waitForFunction(() => getComputedStyle(document.documentElement).getPropertyValue('--paper').trim() === '#f5f5f5', null, { timeout: 5000 })
  ok('切回亮色 → 覆盖自动重新挂上')

  // ④ 字体大小：偏好落盘 + 两条应用路径 + **三档缩放都不许破坏布局**
  //（教训：早先退化路径写成 documentElement.style.zoom，用户「字体调大后页面样式全乱」）。
  const measure = () => page.evaluate(() => {
    const hint = document.querySelector('.settings-overlay')
    const b = hint?.getBoundingClientRect()
    const root = document.getElementById('root').getBoundingClientRect()
    return {
      uiScale: getComputedStyle(document.documentElement).getPropertyValue('--ui-scale').trim(),
      legacyInlineZoom: document.documentElement.style.zoom || '',
      rootZoom: getComputedStyle(document.getElementById('root')).zoom,
      root: { w: Math.round(root.width), h: Math.round(root.height) },
      scrollH: document.documentElement.scrollHeight,
      scrollW: document.documentElement.scrollWidth,
      innerH: innerHeight,
      innerW: innerWidth,
      fixed: b ? { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height) } : null,
    }
  })
  const checkScale = async (label, value) => {
    await page.click(`.set-row .seg button:text-is("${label}")`)
    await page.waitForFunction(
      (v) => getComputedStyle(document.documentElement).getPropertyValue('--ui-scale').trim() === String(v),
      value, { timeout: 5000 },
    )
    const m = await measure()
    assert(m.rootZoom === String(value), `${label}：缩放挂在 #root（zoom=${m.rootZoom}）`)
    // 注意：#root 自身带 zoom，它的 rect 是「自己坐标空间」的尺寸 —— 必须乘回缩放比才是视觉尺寸
    const visW = Math.round(m.root.w * value)
    const visH = Math.round(m.root.h * value)
    assert(Math.abs(visW - m.innerW) <= 1 && Math.abs(visH - m.innerH) <= 1,
      `${label}：#root 视觉尺寸 = 视口（${visW}×${visH} vs ${m.innerW}×${m.innerH}，布局 ${m.root.w}×${m.root.h}）`)
    // 注意：html 的 scrollHeight/Width 记的是**布局 px**（#root 自己的 zoom 不计入），
    // 缩放后它天然大于视口，直接比大小会误报。真正要验的是"页面会不会滚/被裁"
    //（用户的症状正是「放大了之后内容被裁、要滚」）→ 直接试着滚一下，看有没有位移。
    const scrolled = await page.evaluate(() => {
      window.scrollTo(0, 9999)
      window.scrollTo(9999, 0)
      return { x: window.scrollX, y: window.scrollY }
    })
    assert(scrolled.x === 0 && scrolled.y === 0,
      `${label}：页面不产生滚动 / 不被裁（scrollTo 后位移 ${scrolled.x},${scrolled.y}；布局 scroll ${m.scrollW}×${m.scrollH}）`)
    assert(!!m.fixed && m.fixed.x === 0 && m.fixed.y === 0 && m.fixed.w === m.innerW && m.fixed.h === m.innerH,
      `${label}：fixed 浮层仍覆盖整屏（${JSON.stringify(m.fixed)}）`)
  }
  // 三档都过一遍：缩小档最容易被 height 计算漏掉，特大档最容易溢出
  await checkScale('小', 0.9)
  await checkScale('大', 1.1)
  await checkScale('特大', 1.25)
  assert((await readLs())?.scale === 1.25, '缩放偏好落盘（1.25）')

  // 遗留态：旧版本写在 <html> 上的 inline zoom 必须被清掉（HMR 不会替你清 inline style）
  const legacy = await page.evaluate(() => {
    document.documentElement.style.zoom = '1.3' // 模拟"上一次实现留下的残留"
    return getComputedStyle(document.documentElement).zoom
  })
  await setAppearanceFromPage(page, { scale: 1.1 })
  const after = await measure()
  assert(legacy === '1.3' && after.legacyInlineZoom === '' && after.rootZoom === '1.1',
    `遗留 inline zoom 被清除（清前 ${legacy} → 清后 "${after.legacyInlineZoom}"，缩放改由 #root 承担 ${after.rootZoom}）`)

  // 有宿主桥时（=真实 Electron）必须改走原生缩放，并关掉 CSS 退化，避免双重缩放。
  // L0（D6）之后页面上已经没有 window.desktop：宿主只经 preload 一次性交接，探针能用的
  // 注入点只剩 dev-only 的 __haiTestBridge（bridge.ts 在非生产构建挂的探针读取口）。
  // 注意注入发生在**页面加载之后**：src/lib/appearance.ts 的 hostApi() 是调用时解析，
  // bridge 常量此刻已是 null，只有现解析才看得见这个 fake（提成模块常量 = 永远走 CSS 分支，
  // 本条就卡在下面的 waitForFunction 上 —— main 上实测过的回归）。
  await page.evaluate(() => {
    window.__zooms = []
    window.__haiTestBridge = { setUiZoom: (z) => { window.__zooms.push(z); return Promise.resolve({ ok: true, scale: z }) } }
  })
  // 仪器自检：注入的是 dev-only 读取口，产品入口 window.desktop **必须仍然不存在**。
  // 没有这条，本条断言红了只要「把 window.desktop 放回来」也能绿 —— 那是靠回退 L0 换来的假绿。
  const desktopEntry = await page.evaluate(() => ({
    type: typeof window.desktop,
    own: Object.prototype.hasOwnProperty.call(window, 'desktop'),
  }))
  assert(desktopEntry.type === 'undefined' && !desktopEntry.own,
    `注入 __haiTestBridge 后 window.desktop 仍是 undefined（typeof=${desktopEntry.type}，own=${desktopEntry.own}）`)
  await page.click('.set-row .seg button:text-is("特大")')
  await page.waitForFunction(() => (window.__zooms || []).length > 0, null, { timeout: 5000 })
  assert(await page.evaluate(() => window.__zooms.at(-1)) === 1.25, '有宿主桥 → 走原生缩放 setUiZoom(1.25)')
  assert(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--ui-scale').trim()) === '1',
    '原生缩放时 CSS 退化被关掉（--ui-scale=1，不双重缩放）')
  await page.click('.set-row .seg button:text-is("大")') // 回到 1.1 供后面的刷新断言

  // ④b 主进程没注册 ui:set-zoom（旧产物 / 进程未重启）时：不许冒未捕获错误，且必须自动退回 CSS 路径
  //（用户 2026-09-21 报的 "No handler registered for 'ui:set-zoom'" 就是这种错配）
  await page.evaluate(() => {
    window.__haiTestBridge = {
      setUiZoom: () => Promise.reject(new Error("Error invoking remote method 'ui:set-zoom': Error: No handler registered for 'ui:set-zoom'")),
    }
  })
  await page.click('.set-row .seg button:text-is("特大")')
  await page.waitForFunction(
    () => getComputedStyle(document.documentElement).getPropertyValue('--ui-scale').trim() === '1.25',
    null, { timeout: 5000 },
  )
  ok('主进程缺 ui:set-zoom → 自动退回 CSS 路径（缩放仍可用）')
  await page.evaluate(() => { delete window.__haiTestBridge })
  await page.click('.set-row .seg button:text-is("大")') // 回到 1.1，供后面的刷新断言
  await page.waitForFunction(
    () => getComputedStyle(document.documentElement).getPropertyValue('--ui-scale').trim() === '1.1',
    null, { timeout: 5000 },
  )

  // ⑤ 刷新后自恢复
  await page.reload()
  await page.waitForSelector('.codeblock .shiki', { timeout: 20000 })
  assert(await paperVar() === '#f5f5f5', `刷新后底色自恢复（--paper = ${await paperVar()}）`)
  assert(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--ui-scale').trim()) === '1.1', '刷新后缩放自恢复')
  assert(await page.evaluate(() => document.documentElement.scrollHeight <= innerHeight + 1), '刷新后仍未溢出（退化路径不留坑）')
  const afterReload = await tokenColor()
  assert(afterReload === afterSwitch && afterReload !== before, `刷新后仍是所选代码主题（token 颜色 ${afterReload}，非默认的 ${before}）`)

  // ⑥ 「恢复默认外观」出口：把三项一起回默认（出问题时的逃生通道）
  //（⑤ 的 reload 会把设置页重置回「通用设置」tab，先切回外观）
  await page.click('.settings-nav-item:has-text("外观")')
  await page.waitForSelector('.ct-grid', { timeout: 10000 })
  await page.click('.set-row:has-text("恢复默认外观") button.btn')
  await page.waitForFunction(() => getComputedStyle(document.documentElement).getPropertyValue('--ui-scale').trim() === '1', null, { timeout: 5000 })
  const afterReset = await readLs()
  assert(afterReset.scale === 1 && afterReset.lightBg === '#ffffff' && afterReset.codeThemeLight === 'catppuccin-latte',
    `恢复默认后三项都回默认（${JSON.stringify(afterReset)}）`)
  assert(await paperVar() === PAPER_DEFAULT && (await inlinePaperVar()) === '',
    `恢复默认后背景覆盖已清（--paper = ${await paperVar()}，inline = "${await inlinePaperVar()}"）`)
  // 派生 token 也要一起清：清漏一个（比如 --ink-line）就会让主题自带的线永久失效 ——
  // 那正是"自选过底色之后，边框从此都怪怪的"这一类难查的残留
  const leftover = await page.evaluate(() => {
    const st = document.documentElement.style
    return ['--paper-2', '--paper-3', '--ink-line', '--ink-line-2', '--code-bg', '--ink', '--ink-2', '--ink-3']
      .filter((k) => st.getPropertyValue(k).trim() !== '')
  })
  assert(leftover.length === 0, `派生 token 全部清干净（残留：${leftover.join(', ') || '无'}）`)
  assert(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--paper-2').trim()) === '#ffffff',
    '恢复默认后卡面回到主题自带值（--paper-2 = #ffffff）')

  assert(errors.length === 0, `控制台 0 报错（实得 ${errors.length}${errors.length ? '：' + errors.slice(0, 3).join(' | ') : ''}）`)
  console.log(`\n全部通过（${pass} 项断言）`)
  console.log(`截图：${path.join(SHOTS, 'appearance-tab.png')}`)
} catch (e) {
  console.error('\n❌ ' + (e instanceof Error ? e.message : String(e)))
  try { await page.screenshot({ path: path.join(SHOTS, 'appearance-fail.png'), fullPage: true }) } catch { /* 忽略 */ }
  process.exitCode = 1
} finally {
  await browser.close().catch(() => {})
  cleanup()
}
