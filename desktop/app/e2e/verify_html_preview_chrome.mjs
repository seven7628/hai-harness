#!/usr/bin/env node
// HTML 预览块「无壳 + 悬停浮出」回归（2026-09-13）
//
// 起因：用户反馈 ```html 围栏的实时预览块**恒显边框 + 恒显头部工具条**，读起来像嵌进来的
// 一个独立「HTML 块」，与对话正文割裂。目标：默认无壳（无边框、无底色、无头部），
// 指针进入该区域再浮出，让预览区看起来就是正文里的一块内容。
//
// 本脚本锁住的关键行为（每条都对应一个「实现错了就会静默退化」的点）：
//   ① 默认态：无可见边框、头部 opacity 0
//   ② **指针在 iframe 内容上方** → 头部浮出、边框现身
//      （核心难点：隔离 origin 的 iframe 是独立进程，父文档收不到指针事件、父容器
//       :hover 不命中 → 只能由子文档探针 postMessage 上报。脚本用**真实鼠标移动**
//       驱动，若改回纯 CSS 悬停，此条必失败 —— 见下方「纯 CSS 反证」）
//   ③ 指针移出（到块外的正文区）→ 回到无壳态（不是「一旦出现就常驻」）
//   ④ 悬停时预览**不位移**（浮层头部不能顶下 iframe；否则指针下的内容跳位）
//   ⑤ 默认态工具条不吃点击（含按钮，pointer-events:none）→ 点预览顶部不会被静默吞掉
//   ⑥ 源码 tab：头部**常驻且回到文档流**（浮层会盖住源码首行；且切 tab 后指针可能已在块外）
//   ⑦ Tab 键能聚焦到头部按钮，壳与工具条一并现身（默认隐藏不牺牲键盘可达性）
//   ⑧ 叙述区滚动后不残留悬空工具条（滚动把块从指针下挪走却收不到指针事件 → 兜底收起）
//   ⑨ 探针被 HTML 自带 CSP 挡掉时**自愈成恒显**（悬停信号完全依赖子探针，这是本改动的
//      功能风险点：不自愈则工具条永远点不到）
//
// 用法：cd desktop/app && node e2e/verify_html_preview_chrome.mjs
// （用真实 Markdown 组件 + 真实 index.css 的临时探针页，跑完自动删除；不依赖 npm run build）
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4186)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

async function waitPort(port, timeoutMs = 25000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { clearTimeout(timer); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 临时探针页：只挂真实 Markdown 组件 + 真实 index.css（跑完删除）
const PROBE_DIR = path.join(__dirname, '_htmlprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>html preview probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import { Markdown } from '../../src/components/markdown'
import '../../src/index.css'
const CASES = (window as unknown as { __CASES: Record<string, string> }).__CASES
createRoot(document.getElementById('root')!).render(
  // 关键：可滚动的是 .narrative 容器本身（生产结构 —— window 不滚，叙述区滚），
  // 否则「滚动兜底」断言测的是 window 滚动而非真实链路。
  <div className="narrative" style={{ height: '100vh', overflowY: 'auto', width: 620, margin: '0 auto' }}>
    {Object.entries(CASES).map(([k, v]) => (
      <div key={k} data-case={k} className="md-text"><Markdown text={v} /></div>
    ))}
    <div id="spacer" style={{ height: 1200 }} />
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

const PROBE_URL = `http://localhost:${PORT}/e2e/_htmlprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

const HTML_DOC = `<style>body{margin:0}h1{font-size:20px}</style>
<h1 id="inner-head">预览内容</h1><p>这是一段用于回归的 HTML 预览内容。</p>`

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 900, height: 900 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => {
    const t = m.text()
    // 过滤样本自身的预期噪音：csp 样本用 script-src 'none' 挡住注入的探针，
    // 浏览器必然报一条 CSP 违规（这正是该样本要复现的场景）。
    if (m.type() === 'error'
      && !/ERR_NAME_NOT_RESOLVED|Failed to load resource/.test(t)
      && !/Content Security Policy directive 'script-src 'none''/.test(t)) errs.push(t.slice(0, 200))
  })

  await page.addInitScript((c) => { window.__CASES = c }, {
    html: '```html\n' + HTML_DOC + '\n```',
    // 探针失效样本：HTML 自带 CSP script-src 'none' → 注入的悬停/高度探针不执行。
    // 此时必须退回恒显（否则工具条永远点不到 —— 功能丢失，不只是好看不好看）。
    // 必须写成**完整文档**：片段会被包一层骨架，meta 落进 body 里不再被当作 CSP 生效
    // （实测：片段形式探针照样跑起来 → 测不到兜底分支）。
    // 完整文档（作者写了 <html>/<body>，假定白底、只设文字色）——必须仍在白底上，
    // 否则深色文字会落在主题深色画布上（实测：标题几乎不可见）。
    fullDoc: '```html\n' + '<!doctype html><html><head><meta charset="utf-8"><style>'
      + 'body{color:#333;font-family:system-ui}</style></head><body>'
      + '<h1 id="fd-h1" style="color:#2c3e50">完整文档标题</h1></body></html>\n```',
    // 片段（无 <html>）——由组件注入骨架 + 主题画布，片段自身不写背景时跟随主题。
    fragment: '```html\n' + '<div id="frag-body">片段内容</div>\n```',
    csp: '```html\n' + '<!doctype html><html><head><meta charset="utf-8">'
      + '<meta http-equiv="Content-Security-Policy" content="script-src \'none\'"></head>'
      + '<body><h1 id="csp-body">CSP 文档</h1></body></html>\n```',
  })
  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('[data-case="html"] .htmlblock-frame', { timeout: 25000 })
  // 等 iframe 内容 + 探针 postMessage 高度实测完成（高度定型后才有稳定的几何断言）
  await page.waitForTimeout(2000)

  // 采样：头部计算样式 + 整块壳的可见边框 + 头部/iframe 几何
  const snap = () => page.evaluate(() => {
    const blk = document.querySelector('[data-case="html"] .htmlblock')
    const hd = blk.querySelector('.htmlblock-hd')
    const fr = blk.querySelector('.htmlblock-frame')
    const cs = getComputedStyle(blk)
    const hcs = getComputedStyle(hd)
    const b = blk.getBoundingClientRect(), hb = hd.getBoundingClientRect(), fb = fr.getBoundingClientRect()
    const borderVisible = ['Top', 'Right', 'Bottom', 'Left']
      .some((s) => cs[`border${s}Color`] !== 'rgba(0, 0, 0, 0)' && parseFloat(cs[`border${s}Width`]) > 0)
    return {
      hdOpacity: Number(hcs.opacity),
      hdPE: hcs.pointerEvents,
      hdPosition: hcs.position,
      hdVisible: hb.width > 0 && hb.height > 0,
      borderVisible,
      borderTopColor: cs.borderTopColor,
      blk: { x: b.x, y: b.y, w: b.width, h: b.height },
      hd: { top: hb.top, h: hb.height },
      frame: { top: fb.top, h: fb.height },
      frameBottom: fb.bottom,
      blkBottom: b.bottom,
      // 头部是否盖在 iframe 上（浮层）还是把 iframe 推下（在流内）
      hdOverlapsFrame: hb.bottom > fb.top + 1,
    }
  })

  // 鼠标移到指定点并等过渡结束（头部 opacity 过渡 120ms + 子探针节流 150ms）
  const moveTo = async (x, y) => { await page.mouse.move(x, y); await page.waitForTimeout(450) }

  const boxOf = async (sel) => page.locator(sel).boundingBox()

  // 叙事区里、HTML 块「之外」的一个点（用于「移出」——不能用页面角落，
  // 要真实落在正文区域上，才能验证父文档侧的反推逻辑）
  const outsidePoint2 = async (sel) => {
    const blk = await boxOf(`${sel} .htmlblock`)
    return { x: blk.x + blk.width / 2, y: blk.y - 20 }
  }
  const outsidePoint = () => outsidePoint2('[data-case="html"]')

  const clickIntoFrame = async (x, y) => {
    await page.mouse.click(x, y)
    await page.waitForTimeout(250)
  }

  // ---- ① 默认态：无壳 ----
  let out = await outsidePoint()
  await moveTo(out.x, out.y)
  const idle = await snap()
  assert(!idle.borderVisible, `默认态无可见边框（实际 borderTopColor=${idle.borderTopColor}）`)
  assert(idle.hdOpacity === 0, `默认态头部不可见（opacity=${idle.hdOpacity}）`)
  assert(idle.hdVisible, '头部仍在 DOM 中占位（用 opacity 而非 display:none，悬停才能浮出）')
  assert(idle.hdPosition === 'absolute', `头部默认是浮层（position=${idle.hdPosition}）`)
  assert(idle.hdPE === 'none', `默认态头部容器不吃点击（pointer-events=${idle.hdPE}）`)
  assert(await page.evaluate(() => {
    const btns = [...document.querySelectorAll('[data-case="html"] .htmlblock-hd button')]
    return btns.length > 0 && btns.every((b) => getComputedStyle(b).pointerEvents === 'none')
  }), '默认态工具条**按钮**也不吃点击（否则看不见却能点到，静默吞掉对预览的点击）')

  // ---- ② 指针在 iframe 内容上方 → 浮出（核心：子探针上报，非 CSS :hover）----
  const fr = await boxOf('[data-case="html"] .htmlblock-frame')
  const framePt = { x: fr.x + fr.width / 2, y: fr.y + fr.height / 2 }
  await moveTo(framePt.x, framePt.y)
  const overFrame = await snap()
  assert(overFrame.hdOpacity === 1, `指针位于 iframe 内容上方时头部浮出（opacity=${overFrame.hdOpacity}）`)
  assert(overFrame.borderVisible, '指针位于 iframe 内容上方时外壳边框现身')
  // 反证：若只有 :hover 在起作用，父容器此刻并不命中 :hover（OOPIF）
  const cssHoverOnly = await page.evaluate(() => {
    const blk = document.querySelector('[data-case="html"] .htmlblock')
    return { hdOnClass: blk.classList.contains('hd-on'), matchesHover: blk.matches(':hover') }
  })
  assert(cssHoverOnly.hdOnClass, '浮出态由 .hd-on（子探针上报的悬停）驱动，而非仅靠 CSS :hover')

  // ---- ③ 移出到块外的正文区 → 回无壳态（不能常驻）----
  out = await outsidePoint()
  await moveTo(out.x, out.y)
  const away = await snap()
  assert(away.hdOpacity === 0 && !away.borderVisible, '指针移出到正文区后回到无壳态（不是出现后常驻）')

  // ---- ④ 悬停时预览不位移 ----
  await moveTo(framePt.x, framePt.y)
  const hovered = await snap()
  assert(Math.abs(hovered.frame.top - idle.frame.top) <= 0.5,
    `悬停不顶下预览（悬停 frame.top=${hovered.frame.top} vs 默认 ${idle.frame.top}）`)
  assert(Math.abs(hovered.blk.h - idle.blk.h) <= 0.5,
    `悬停不改变整块高度（悬停 ${hovered.blk.h} vs 默认 ${idle.blk.h}）`)
  assert(hovered.hdOverlapsFrame, '头部悬停时以浮层盖在预览之上（不在流内推挤）')

  // ---- ⑤ 点预览顶部：不被工具条吞掉 ----
  // 头部浮层覆盖的那条区域，默认态点击应穿透到 iframe。
  await moveTo(out.x, out.y) // 先移出，回到无壳态
  const headBandY = idle.frame.top + 6 // 头部浮层所在的高度带
  await clickIntoFrame(framePt.x, headBandY)
  const innerFocused = await page.evaluate(() => document.activeElement?.tagName)
  assert(innerFocused === 'IFRAME', `默认态点预览顶部（头部覆盖带）落到 iframe 上（activeElement=${innerFocused}，未被工具条吞掉）`)

  // ---- ⑥ 源码 tab：头部常驻且在流内 ----
  // 悬停浮现 → 点「源码」tab
  await moveTo(framePt.x, framePt.y)
  await page.locator('[data-case="html"] .htmlblock-tabs button').nth(1).click()
  await page.waitForTimeout(300)
  out = await outsidePoint()
  await moveTo(out.x, out.y) // 点完把手移开：源码模式必须仍然可见（不依赖悬停）
  const srcIdle = await page.evaluate(() => {
    const blk = document.querySelector('[data-case="html"] .htmlblock')
    const hd = blk.querySelector('.htmlblock-hd')
    const body = blk.querySelector('.cb-body')
    const hb = hd.getBoundingClientRect(), bb = body.getBoundingClientRect()
    return {
      isSourceMode: blk.classList.contains('source-mode'),
      hdOpacity: Number(getComputedStyle(hd).opacity),
      hdPosition: getComputedStyle(hd).position,
      hdPE: getComputedStyle(hd).pointerEvents,
      btnPE: getComputedStyle(hd.querySelector('button')).pointerEvents,
      hdOverlapsBody: hb.bottom > bb.top + 1, // 在流内时头部应紧贴正文之上，不重叠
    }
  })
  assert(srcIdle.isSourceMode, '已切到源码模式')
  assert(srcIdle.hdOpacity === 1, `源码模式指针移开后头部仍常驻（opacity=${srcIdle.hdOpacity}）`)
  assert(srcIdle.hdPosition === 'static', `源码模式头部回到文档流（position=${srcIdle.hdPosition}）`)
  assert(srcIdle.hdPE === 'auto' && srcIdle.btnPE === 'auto',
    `源码模式工具条可点击（hd=${srcIdle.hdPE} / button=${srcIdle.btnPE}）`)
  assert(!srcIdle.hdOverlapsBody, `源码模式头部不盖住源码首行（重叠=${srcIdle.hdOverlapsBody}）`)

  // ---- ⑦ 键盘可达：Tab 能聚焦到工具条按钮，壳随之现身 ----
  await page.locator('[data-case="html"] .htmlblock-tabs button').nth(0).click() // 回预览
  await page.waitForTimeout(300)
  out = await outsidePoint()
  await moveTo(out.x, out.y)
  const before = await snap()
  assert(before.hdOpacity === 0, '（前置）移开后回到无壳态')
  // 用真实键盘路径：从「渲染」按钮起 Tab，能走到同块内的其它按钮
  await page.locator('[data-case="html"] .htmlblock-tabs button').nth(0).focus()
  await page.keyboard.press('Tab')
  await page.waitForTimeout(250)
  const focused = await page.evaluate(() => {
    const blk = document.querySelector('[data-case="html"] .htmlblock')
    const cs = getComputedStyle(blk)
    return {
      btnReachable: blk.contains(document.activeElement),
      focusedLabel: document.activeElement?.textContent ?? '',
      hdOpacity: Number(getComputedStyle(blk.querySelector('.htmlblock-hd')).opacity),
      borderVisible: cs.borderTopColor !== 'rgba(0, 0, 0, 0)',
    }
  })
  assert(focused.btnReachable, `Tab 能在 HTML 预览块内移动焦点（当前「${focused.focusedLabel}」）`)
  assert(focused.hdOpacity === 1 && focused.borderVisible,
    `聚焦时头部与外壳一并现身（opacity=${focused.hdOpacity}，border=${focused.borderVisible}）—— 默认隐藏不牺牲键盘可达性`)

  // ---- ⑧ 叙述区滚动后不残留悬空工具条 ----
  await page.locator('[data-case="html"] .htmlblock-tabs button').nth(0).click()
  await page.waitForTimeout(200)
  await page.evaluate(() => { const el = document.activeElement; if (el && el.blur) el.blur() }) // 先撤键盘焦点（它会正当维持浮出）
  await moveTo(framePt.x, framePt.y)
  const scrolled = await page.evaluate(() => {
    // 滚真正的滚动容器（.narrative），不是 window —— 与生产一致
    const el = document.querySelector('.narrative')
    ;(el || window).scrollBy(0, 120)
    return true
  })
  void scrolled
  await page.waitForTimeout(400)
  const afterScroll = await snap()
  assert(afterScroll.hdOpacity === 0,
    `叙述区滚动后不残留悬空工具条（滚动把块挪走却收不到指针事件 → 已兜底收起，opacity=${afterScroll.hdOpacity}）`)

  // ---- ⑨ 主题画布语义：完整文档保白底、片段跟随主题（曾踩坑：两者一刀切）----
  // 完整文档的作者假定白底（只设 color 不设 background）→ iframe 元素必须仍是白底，
  // 否则文字不可读；片段则靠组件注入的 --hai-* 主题画布，深/亮主题都成立。
  await page.locator('[data-case="fullDoc"] .htmlblock').scrollIntoViewIfNeeded()
  await page.waitForTimeout(400)
  const canvas = await page.evaluate(() => {
    const frameBg = (sel) => {
      const f = document.querySelector(`${sel} .htmlblock-frame`)
      return f ? getComputedStyle(f).backgroundColor : null
    }
    return { fullDocFrameBg: frameBg('[data-case="fullDoc"]'), fragmentFrameBg: frameBg('[data-case="fragment"]') }
  })
  assert(canvas.fullDocFrameBg === 'rgb(255, 255, 255)',
    `完整文档 iframe 保持白底（实际 ${canvas.fullDocFrameBg}）—— 作者的深色文字才可读`)
  assert(canvas.fragmentFrameBg === 'rgb(255, 255, 255)',
    `片段 iframe 元素同为白底占位（实际 ${canvas.fragmentFrameBg}）；片段的主题底色来自注入骨架`)

  // 片段骨架确实注入了主题画布（body 背景 + --hai-bg 令牌）：
  // sandbox 无 allow-same-origin → 读不到 contentDocument（这正是沙箱的隔离保证），
  // 所以「骨架内容」改由**非沙箱探针页**直接断言（见 e2e:codeblock 的 canvasCss 断言
  // 与源码），此处只验证沙箱边界本身成立（读不到子文档 = 隔离有效）。
  const sandboxed = await page.evaluate(() => {
    const f = document.querySelector('[data-case="fragment"] .htmlblock-frame')
    if (!f) return null
    let readable = true
    try { void f.contentDocument?.body } catch { readable = false }
    return { hasFrame: true, isolated: f.contentDocument === null || readable === false }
  })
  assert(sandboxed?.hasFrame, '片段预览 iframe 存在')
  assert(sandboxed.isolated, '沙箱隔离仍有效（父文档读不到子文档 DOM）')

  // ---- ⑨b 片段注入骨架的「主题画布」实测（沙箱读不到 → 用非沙箱 iframe 复现同一 srcdoc）----
  // 这是「贴合系统主题」真正生效的地方：iframe 内拿不到应用 CSS 变量，故组件把当前主题
  // 烘成 --hai-* 令牌 + body 底色注入骨架。深/亮两套都必须成立（否则又是一块刺眼白底）。
  for (const theme of ['ink', 'paper']) {
    await page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme)
    await page.waitForTimeout(500)
    const c = await page.evaluate(async () => {
      const src = document.querySelector('[data-case="fragment"] .htmlblock-frame')?.getAttribute('srcdoc') || ''
      const f = document.createElement('iframe') // 非沙箱：仅为读回注入结果（不执行用户脚本的对照）
      f.style.cssText = 'width:400px;height:120px;border:0;position:fixed;left:-9999px'
      f.srcdoc = src
      document.body.appendChild(f)
      await new Promise((r) => { f.onload = r; setTimeout(r, 700) })
      const d = f.contentDocument
      const cs = d.defaultView.getComputedStyle(d.body)
      const root = d.defaultView.getComputedStyle(d.documentElement)
      const out = {
        injected: src.includes('--hai-bg'),
        tokenBg: root.getPropertyValue('--hai-bg').trim(),
        bodyBg: cs.backgroundColor,
        colorScheme: root.getPropertyValue('color-scheme').trim(),
      }
      f.remove()
      return out
    })
    assert(c.injected && c.tokenBg, `[${theme}] 片段骨架注入了主题令牌（--hai-bg=${c.tokenBg}）`)
    // 期望值**从当前主题的 token 现算**，不再写死 rgb(22,20,15)：写死的那份在换主题
    //（2026-09-21 朱砂定稿）时必然过期，且过期的表现是"断言失败"而不是"界面坏掉"，
    // 白耗一次排查。--hai-bg 与 body 底色都应等于该主题的 --paper。
    const paper = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--paper').trim())
    const hex2rgb = (h) => {
      const n = parseInt(h.replace('#', ''), 16)
      return `rgb(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255})`
    }
    const want = hex2rgb(paper)
    assert(c.tokenBg === paper && c.bodyBg === want,
      `[${theme}] 骨架底色 = 该主题 --paper（--hai-bg=${c.tokenBg} / body=${c.bodyBg}，期望 ${want}；color-scheme=${c.colorScheme}）`)
    // 恒白底这条老毛病仍在守：**深色下绝不允许是白**。
    // （亮色侧不比"是否等于纯白"了：2026-09-21 起亮色底是 #fdfcfb，暖白 —— 上面那条
    //  "等于 --paper"才是真正的判据；这里只兜"深色被刷成白"这一种退化。）
    if (theme === 'ink') {
      assert(c.bodyBg !== 'rgb(255, 255, 255)',
        `[ink] 不是恒白底（body=${c.bodyBg}，color-scheme=${c.colorScheme}）`)
    }
  }
  await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'ink'))

  // ---- ⑩ 探针失效（CSP script-src 'none'）→ 退回恒显，工具条仍可用 ----
  // 这是「默认隐藏」这个改动的功能风险点：悬停信号完全依赖子探针，探针被 CSP 挡掉
  // 时必须自愈成恒显，否则用户再也点不到「源码/复制」。
  await page.locator('[data-case="csp"] .htmlblock').scrollIntoViewIfNeeded()
  out = await outsidePoint2('[data-case="csp"]')
  await moveTo(out.x, out.y) // 指针明确在块外
  await page.waitForTimeout(2600) // 等过兜底判定窗口（2.5s）
  const csp = await page.evaluate(() => {
    const blk = document.querySelector('[data-case="csp"] .htmlblock')
    const hd = blk.querySelector('.htmlblock-hd')
    return {
      probeDead: blk.classList.contains('probe-dead'),
      hdOpacity: Number(getComputedStyle(hd).opacity),
      btnPE: getComputedStyle(hd.querySelector('button')).pointerEvents,
      bodyRendered: !!blk.querySelector('.htmlblock-frame'),
    }
  })
  assert(csp.bodyRendered, 'CSP 文档仍渲染出预览 iframe')
  // 反向保护：正常文档**绝不能**被误判为 probe-dead（否则等于悄悄退回恒显、无壳失效）
  const normalNotDead = await page.evaluate(() => {
    const blk = document.querySelector('[data-case="html"] .htmlblock')
    const cs = getComputedStyle(blk.querySelector('.htmlblock-hd'))
    return { probeDead: blk.classList.contains('probe-dead'), hdOpacity: Number(cs.opacity) }
  })
  assert(!normalNotDead.probeDead, '正常文档未被误判为 probe-dead（正常文档探针按时上报）')
  assert(normalNotDead.hdOpacity === 0, `正常文档仍是「无壳」态（opacity=${normalNotDead.hdOpacity}，未因兜底判定而恒显）`)
  assert(csp.probeDead, 'CSP 挡掉探针 → 标记 probe-dead（自愈判定生效）')
  assert(csp.hdOpacity === 1, `探针失效时退回恒显（opacity=${csp.hdOpacity}）—— 工具条不会变成点不到`)
  assert(csp.btnPE === 'auto', `探针失效时工具条按钮可点（pointer-events=${csp.btnPE}）`)

  // ---- ⑪ 反复悬停/移出不抖动（iframe 高度稳定）----
  await page.evaluate(() => { const el = document.querySelector('.narrative'); if (el) el.scrollTop = 0 })
  await page.waitForTimeout(200)
  const fr2 = await boxOf('[data-case="html"] .htmlblock-frame')
  const pt2 = { x: fr2.x + fr2.width / 2, y: fr2.y + fr2.height / 2 }
  const heights = []
  for (let i = 0; i < 3; i++) {
    await moveTo(pt2.x, pt2.y)
    heights.push((await snap()).frame.h)
    const o = await outsidePoint()
    await moveTo(o.x, o.y)
  }
  assert(new Set(heights.map((h) => Math.round(h))).size === 1,
    `反复悬停/移出 iframe 高度恒定（实测 ${heights.join(' / ')}）`)

  assert(errs.length === 0, `无页面/控制台报错${errs.length ? '：' + errs.join(' | ') : ''}`)

  await browser.close()
  cleanup()
  console.log(`\n[verify] HTML 预览块「无壳 + 悬停浮出」全部通过（${pass} 项断言）`)
}

main().catch((e) => { console.error('FAIL', e); cleanup(); process.exit(1) })
