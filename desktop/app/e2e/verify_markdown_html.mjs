#!/usr/bin/env node
// Markdown 渲染增强回归（2026-09）：内联 HTML / 标题锚点 / alerts / emoji / frontmatter /
// 本地相对图片。
//
// 起因：用户反馈「Markdown 预览模式支持得不够全面」——截图里 README 的居中标题、<picture>
// 双主题 logo、徽标全是**转义后的 HTML 源码**。根因：整条管线只有 rehype-katex，没有
// rehype-raw 那一路（内联 HTML 被当文本显示）。
//
// 本脚本锁住的关键行为（每条都对应「实现错了就会静默退化」的点）：
//   ① 内联 HTML 渲染成真实元素（align=center 生效、<b>/<br>/行内 <code> 保留）
//   ② 白名单 sanitize：script/iframe/style/表单/事件属性/javascript: href/data:text/html
//      全部进不了 DOM，且 window.__pwned 始终为空（真攻击面，不是字符串比对）
//   ③ <picture> 按**应用主题**挑图（不是系统 prefers-color-scheme），退化成单个 <img>
//   ④ 本地相对图片：锚到工作区/文件目录 → 经 fs:read-image 拿 data URL → 真的加载出来
//      （naturalWidth 有值）；远程图原样；读不到时给「路径 + 原因」而不是一张破图
//   ⑤ 原始 HTML 的 <pre> 保住自己的预排版（不能被围栏那条解包逻辑吃掉）；围栏仍高亮
//   ⑥ 标题 id（中文锚点 = GitHub slug 口径）+ 目录链接 href 同参命中 + 重名去重
//   ⑦ GitHub alerts：> [!NOTE] 变成带类型标题的提示块，普通引用块不受影响
//   ⑧ emoji 短代码、frontmatter 卡片（不再被当成分隔线 + setext 标题），解析不出来时退回代码块
//
// 用法：cd desktop/app && node e2e/verify_markdown_html.mjs
// （真实 Markdown 组件 + 真实 index.css + mock transport；跑完自动删探针页）
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4200)

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

const PROBE_DIR = path.join(__dirname, '_mdhtmlprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>md html probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')

// 探针页：真实 Markdown 组件 + 真实 index.css + 真实 store（mock transport）。
// store 暴露成 __store，用来验证「相对链接点开 = 走既有文件面板」这类**接线**，而不只是
// 「href 看着对」。
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { Markdown } from '../../src/components/markdown'
import { useAppStore } from '../../src/store/useAppStore'
import { getTransport } from '../../src/transport'
import { HostPanelsProvider } from '../../src/host/panels-desktop'
import '../../src/index.css'

useAppStore.setState({ workspace: '/mock/proj-auth' })
;(window as unknown as { __store: unknown }).__store = useAppStore
const CASES = (window as unknown as { __CASES: Record<string, string> }).__CASES

// 「图片通道不可用」探针（用户实测过的那条失败态）：应用没重启时主进程/preload 里没有
// fs:read-image，渲染进程拿到的是 TypeError。这里在**挂载之后**再打断通道，让一个独立 case
// 现取现失败（其余 case 早已成功并进了缓存，不受影响）。
function Probe() {
  const [broken, setBroken] = useState(false)
  useEffect(() => {
    ;(window as unknown as { __breakChannel: () => void }).__breakChannel = () => {
      getTransport().fsReadImage = () => Promise.reject(new TypeError('window.desktop.fsReadImage is not a function'))
      setBroken(true)
    }
  }, [])
  return (
    <div className="narrative" style={{ width: 620, margin: '0 auto' }}>
      {Object.entries(CASES).map(([k, v]) => (
        <div key={k} data-case={k} className="md-text"><Markdown text={v} /></div>
      ))}
      {broken && <div data-case="brokenChannel" className="md-text"><Markdown text={'![logo](img/broken-channel.png)'} /></div>}
    </div>
  )
}
// 与真应用同一条 host seam（P8-01b）：Markdown 的工作区根/文件打开都从
// HostPanelsProvider 下发，探针不包一层就拿不到 workspace（相对图片解析不出来）。
createRoot(document.getElementById('root')!).render(<HostPanelsProvider><Probe /></HostPanelsProvider>)
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_mdhtmlprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

// 敌意样本：每一条都是「能进 DOM 就等于破防」的写法
const ATTACK = [
  '<script>window.__pwned = 1</script>',
  '<img src="nope.png" onerror="window.__pwned = 2">',
  '<a href="javascript:window.__pwned = 3">点我</a>',
  '<iframe src="https://example.com"></iframe>',
  '<style>body{display:none}</style>',
  '<div onclick="window.__pwned = 4">div</div>',
  '<svg><script>window.__pwned = 5</script><rect width="10" height="10" fill="#f00"/></svg>',
  '<form action="/x"><input type="text" name="a"><button>go</button></form>',
  '<img src="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==">',
  '<meta http-equiv="refresh" content="0;url=https://example.com">',
  '<b>保留的粗体</b>',
].join('\n\n')

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 900, height: 1200 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => {
    const t = m.text()
    // 样本里的外部图片/网络噪音不算失败
    if (m.type() === 'error' && !/ERR_NAME_NOT_RESOLVED|Failed to load resource|ERR_FAILED/.test(t)) errs.push(t.slice(0, 200))
  })
  // 远程资源（README 的 shields.io 徽标等）一律掐掉：本用例只验「远程 src 不被本地通道接管」，
  // 不该因为内网/离线环境变得不确定，也不该拖慢用例。
  await page.route(/^https?:\/\/(?!localhost)/, (r) => r.abort())

  const CASES = {
    rawCenter: '<p align="center">\n<b>事件驱动</b><br>\n<i>只负责驱动 <code>Session</code></i>\n</p>',
    rawPicture: '<picture><source media="(prefers-color-scheme: dark)" srcset="brand/hai-logo-dark.svg"><img src="brand/hai-logo-light.svg" alt="HAI Harness" width="200"></picture>',
    rawNoPicture: '<picture><source media="(min-width: 100px)" srcset="brand/hai-wide.svg"><img src="brand/hai-fallback.svg" alt="fallback"></picture>',
    rawPre: '<pre>列1   列2\n  缩进 保持</pre>',
    fenceStillBlock: '```go\npackage main\n```',
    rawInline: '按 <kbd>⌘K</kbd> 打开，<mark>高亮</mark>，<sub>下</sub><sup>上</sup>。',
    rawDetails: '<details><summary>点我展开</summary>\n\n折叠里的正文\n\n</details>',
    attack: ATTACK,
    toc: '## 架构\n\n正文一。\n\n## 执行链路（事件与 Trace）\n\n正文二。\n\n[跳到架构](#架构) · [跳到执行链路](#执行链路事件与-trace)\n\n## 架构\n\n重名标题。',
    alertNote: '> [!NOTE]\n> 这是一条提示。',
    alertWarn: '> [!WARNING]\n> 这是一条警告。',
    quotePlain: '> 普通引用，不该变成 alert。',
    emoji: ':rocket: 快速开始，:bug: 修一个 bug。',
    frontmatter: '---\ntitle: HAI Harness\ntags:\n  - go\n  - agent\ncount: 3\n---\n\n正文开始。',
    fmBroken: '---\ntitle: x\n这行不是键值对\n---\n\n正文。',
    localImg: '![logo](brand/hai-logo-light.svg)',
    localImgRel: '![图标](img/icon.png)',
    remoteImg: '![远程](https://example.com/a.png)',
    notImage: '![文档](docs/guide.md)',
    relLink: '[文档](docs/guide.md)',
    // 真·现实样本：本仓库自己的 README（用户截图里那份）。它的目录锚点是**在 GitHub 上生成的**，
    // 正好当 slug 口径的基准；头部的 <picture>/徽标/居中块也都是真实写法。
    readme: fs.readFileSync(path.resolve(APP, '..', '..', 'README.zh-CN.md'), 'utf8'),
  }
  await page.addInitScript((c) => { window.__CASES = c }, CASES)
  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('[data-case="toc"] .md h2', { timeout: 25000 })
  await page.waitForTimeout(1200) // 等本地图 IPC（mock）与 shiki 落地

  // ① 内联 HTML 渲染
  const raw = await page.evaluate(() => {
    const C = (n) => document.querySelector(`[data-case="${n}"]`)
    const md = (n) => C(n).querySelector('.md')
    const centerEl = md('rawCenter').querySelector('[align="center"]')
    const tocIds = [...md('toc').querySelectorAll('h2')].map((h) => h.id)
    const tocLinks = [...md('toc').querySelectorAll('a')].map((a) => a.getAttribute('href'))
    const fm = md('frontmatter')
    const alert = md('alertNote')
    const xss = md('attack')
    const local = md('localImg')
    const notImg = md('notImage')
    const rel = md('relLink')
    return {
      // —— raw HTML ——
      center: {
        exists: !!centerEl,
        textAlign: centerEl ? getComputedStyle(centerEl).textAlign : null,
        hasBold: !!centerEl?.querySelector('b'),
        hasBr: !!centerEl?.querySelector('br'),
        hasInlineCode: !!centerEl?.querySelector('code'),
        escaped: /&lt;p align/.test(md('rawCenter').textContent || ''),
      },
      picture: {
        anyPicture: !!md('rawPicture').querySelector('picture'),
        src: md('rawPicture').querySelector('img')?.getAttribute('src') ?? null,
        localPath: md('rawPicture').querySelector('img')?.getAttribute('data-local-path') ?? null,
        width: md('rawPicture').querySelector('img')?.getAttribute('width') ?? null,
        alt: md('rawPicture').querySelector('img')?.getAttribute('alt') ?? null,
        natW: md('rawPicture').querySelector('img')?.naturalWidth ?? 0,
      },
      pictureFallback: {
        src: md('rawNoPicture').querySelector('img')?.getAttribute('src') ?? null,
        localPath: md('rawNoPicture').querySelector('img')?.getAttribute('data-local-path') ?? null,
      },
      pre: {
        keptPre: md('rawPre').querySelectorAll('pre').length,
        hasCodeblock: !!md('rawPre').querySelector('.codeblock'),
        whiteSpace: (() => { const p = md('rawPre').querySelector('pre'); return p ? getComputedStyle(p).whiteSpace : null })(),
        text: md('rawPre').querySelector('pre')?.textContent ?? null,
      },
      fence: { hasCodeblock: !!md('fenceStillBlock').querySelector('.codeblock'), nestedPre: !!md('fenceStillBlock').querySelector('.codeblock')?.closest('pre') },
      inline: {
        kbdInP: !!md('rawInline').querySelector('p > kbd'),
        kbdBorder: (() => { const k = md('rawInline').querySelector('kbd'); return k ? getComputedStyle(k).borderTopWidth : null })(),
        mark: !!md('rawInline').querySelector('mark'),
        sub: !!md('rawInline').querySelector('sub'),
        sup: !!md('rawInline').querySelector('sup'),
      },
      details: {
        exists: !!md('rawDetails').querySelector('details'),
        summary: md('rawDetails').querySelector('summary')?.textContent ?? null,
        bodyText: (md('rawDetails').textContent || '').includes('折叠里的正文'),
      },
      // —— 攻击面 ——
      xss: {
        pwned: window.__pwned ?? null,
        script: xss.querySelectorAll('script').length,
        iframe: xss.querySelectorAll('iframe').length,
        style: xss.querySelectorAll('style').length,
        form: xss.querySelectorAll('form').length,
        textInput: xss.querySelectorAll('input[type="text"]').length,
        onAttrs: xss.querySelectorAll('[onclick],[onerror],[onload]').length,
        jsHref: [...xss.querySelectorAll('a')].some((a) => /^\s*javascript:/i.test(a.getAttribute('href') || '')),
        dataHtmlImg: [...xss.querySelectorAll('img')].some((i) => /^data:text\/html/i.test(i.getAttribute('src') || '')),
        meta: xss.querySelectorAll('meta').length,
        keptBold: !!xss.querySelector('b'),
      },
      // —— 锚点 ——
      toc: { ids: tocIds, links: tocLinks.map((h) => (() => { try { return decodeURIComponent(h ?? '') } catch { return h } })()), hashTarget: !!document.getElementById('user-content-架构') },
      // —— alerts ——
      alert: {
        cls: alert.querySelector('.md-alert')?.className ?? null,
        head: alert.querySelector('.md-alert-hd')?.textContent ?? null,
        body: alert.querySelector('.md-alert')?.textContent ?? '',
        raw: (alert.textContent || '').includes('[!NOTE]'),
        warnCls: md('alertWarn').querySelector('.md-alert')?.className ?? null,
        warnHead: md('alertWarn').querySelector('.md-alert-hd')?.textContent ?? null,
        plain: md('quotePlain').querySelectorAll('.md-alert').length,
        plainIsBlockquote: !!md('quotePlain').querySelector('blockquote'),
      },
      // —— emoji / frontmatter ——
      emoji: { text: md('emoji').textContent, hasShortcode: (md('emoji').textContent || '').includes(':rocket:') },
      fm: {
        card: !!fm.querySelector('.md-fm'),
        head: fm.querySelector('.md-fm-hd')?.textContent ?? null,
        keys: [...fm.querySelectorAll('.md-fm-k')].map((e) => e.textContent),
        values: [...fm.querySelectorAll('.md-fm-v')].map((e) => e.textContent),
        headings: fm.querySelectorAll('h1,h2,h3').length,
        hr: fm.querySelectorAll('hr').length,
        brokenCard: !!md('fmBroken').querySelector('.md-fm'),
        brokenCode: !!md('fmBroken').querySelector('.md-fm .codeblock, .md-fm pre'),
      },
      // —— 本地图 ——
      local: {        src: local.querySelector('img')?.getAttribute('src') ?? null,
        localPath: local.querySelector('img')?.getAttribute('data-local-path') ?? null,
        natW: local.querySelector('img')?.naturalWidth ?? 0,
        alt: local.querySelector('img')?.getAttribute('alt') ?? null,
        relLocalPath: md('localImgRel').querySelector('img')?.getAttribute('data-local-path') ?? null,
        missingPlaceholder: notImg.querySelectorAll('.md-img-missing').length,
        missingTitle: notImg.querySelector('.md-img-missing')?.getAttribute('title') ?? null,
        remoteSrc: md('remoteImg').querySelector('img')?.getAttribute('src') ?? null,
        remoteLocalPath: md('remoteImg').querySelector('img')?.getAttribute('data-local-path') ?? null,
      },
      relLink: { href: rel.querySelector('a')?.getAttribute('href') ?? null, title: rel.querySelector('a')?.getAttribute('title') ?? null },
      appTheme: document.documentElement.getAttribute('data-theme'),
    }
  })

  // ① 内联 HTML 变真实元素
  assert(raw.center.exists, '内联 HTML：<p align="center"> 渲染成真实元素（不再是一屏转义源码）')
  assert(raw.center.textAlign === 'center', `内联 HTML：align="center" 真的居中（text-align=${raw.center.textAlign}）`)
  assert(raw.center.hasBold && raw.center.hasBr && raw.center.hasInlineCode, '内联 HTML：<b>/<br>/行内 <code> 都保留为元素')
  assert(!raw.center.escaped, '内联 HTML：不再出现 &lt;p align 这种转义文本')

  // ③ <picture> 按应用主题挑图（两段式：显式把主题掰到 ink / paper 各验一次 —— 探针环境
  // 里 store 初始化会按系统偏好写 data-theme，不能假设它是 ink）
  assert(!raw.picture.anyPicture, '<picture> 被定型成单个 <img>（不留媒体查询）')
  assert(raw.picture.width === '200' && raw.picture.alt === 'HAI Harness', '<picture> 的 width/alt 透传给了新 <img>')
  assert(['ink', 'paper'].includes(raw.appTheme), `应用主题已解析（data-theme=${raw.appTheme}）`)
  const themeSrc = async (theme) => page.evaluate(async (th) => {
    document.documentElement.setAttribute('data-theme', th)
    await new Promise((r) => setTimeout(r, 600))
    const img = document.querySelector('[data-case="rawPicture"] .md img')
    return {
      // 主题属性可能被 store 再写回 —— 把它读回来，断言才是在「当时的真实主题」下成立的
      actual: document.documentElement.getAttribute('data-theme'),
      localPath: img?.getAttribute('data-local-path') ?? null,
      natW: img?.naturalWidth ?? 0,
    }
  }, theme)
  const darkPick = await themeSrc('ink')
  assert(darkPick.localPath === '/mock/proj-auth/brand/hai-logo-dark.svg', `深色主题下挑 dark 源（实际 ${darkPick.localPath}，data-theme=${darkPick.actual}）`)
  assert(darkPick.natW === 120, `<picture> 挑出的图经本地通道真的加载出来了（naturalWidth=${darkPick.natW}）`)
  const lightPick = await themeSrc('paper')
  assert(lightPick.localPath === '/mock/proj-auth/brand/hai-logo-light.svg', `切亮色主题后换成 light 源（实际 ${lightPick.localPath}，data-theme=${lightPick.actual}）`)
  assert(raw.pictureFallback.localPath === '/mock/proj-auth/brand/hai-fallback.svg', '非主题类媒体查询（min-width）不表态 → 回退 <img> 的 src')
  await themeSrc('ink') // 后续断言（含 CSS 边框/居中）回到深色主题下

  // ⑤ 原始 HTML 的 <pre> 保住预排版
  assert(raw.pre.keptPre === 1 && !raw.pre.hasCodeblock, '原始 HTML 的 <pre> 保留自己那层（没有被围栏解包逻辑吃掉）')
  assert(raw.pre.whiteSpace === 'pre' && /列1   列2/.test(raw.pre.text), `原始 <pre> 保住了等宽预排版（white-space=${raw.pre.whiteSpace}）`)
  assert(raw.fence.hasCodeblock && !raw.fence.nestedPre, '回归：markdown 围栏仍走高亮代码块（且不被 <pre> 再包一层）')

  // 行内语义标签
  assert(raw.inline.kbdInP && raw.inline.kbdBorder === '1px', `行内 <kbd> 不被顶成块且拿到边框（border=${raw.inline.kbdBorder}）`)
  assert(raw.inline.mark && raw.inline.sub && raw.inline.sup, '<mark>/<sub>/<sup> 都渲染出来')
  assert(raw.details.exists && raw.details.summary === '点我展开' && raw.details.bodyText, '<details>/<summary> 渲染且内容可达')

  // ② sanitize（真攻击面）
  assert(raw.xss.pwned === null, 'sanitize：11 种敌意样本一个都没能执行（window.__pwned 始终为空）')
  assert(raw.xss.script === 0 && raw.xss.style === 0 && raw.xss.iframe === 0 && raw.xss.meta === 0, 'sanitize：script/style/iframe/meta 全部连内容一起剥掉')
  assert(raw.xss.form === 0 && raw.xss.textInput === 0, 'sanitize：表单与文本输入进不来')
  assert(raw.xss.onAttrs === 0, 'sanitize：事件属性（onclick/onerror/onload）一个不剩')
  assert(!raw.xss.jsHref, 'sanitize：javascript: href 被摘掉')
  assert(!raw.xss.dataHtmlImg, 'sanitize：data:text/html 被摘掉（只放行 data:image/*）')
  assert(raw.xss.keptBold, 'sanitize 不是一刀切：同一块里的 <b> 照常渲染')

  // ⑥ 标题锚点
  assert(raw.toc.ids[0] === 'user-content-架构' && raw.toc.ids[2] === 'user-content-架构-1', `标题 id 带 user-content- 前缀且重名去重（${raw.toc.ids.join(' , ')}）`)
  assert(raw.toc.ids[1] === 'user-content-执行链路事件与-trace', `中文 + 括号标题的 id 与 GitHub slug 口径一致（${raw.toc.ids[1]}）`)
  assert(raw.toc.links[0] === '#user-content-架构' && raw.toc.links[1] === '#user-content-执行链路事件与-trace', `目录链接 href 与标题 id 同参（点了能命中）（实际 ${JSON.stringify(raw.toc.links)}）`)
  assert(raw.toc.hashTarget, '锚点落地元素存在（getElementById 命中 h2）')
  // 真点一次：目录必须是**能跳**的，不只是 href 长得对（remark 会把非 ASCII 目标百分号编码，
  // 断言因此要按解码后的值比 —— 浏览器做片段匹配时也是解码后比的）
  const jumped = await page.evaluate(() => {
    const a = document.querySelector('[data-case="toc"] .md a')
    a.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
    const frag = location.hash.slice(1)
    // location.hash 是编码态（remark 会把非 ASCII 目标百分号编码）——浏览器做片段匹配时按解码后
    // 的值找元素，断言也要同参，否则会得到「跳不过去」的假警报。
    let decoded = frag
    try { decoded = decodeURIComponent(frag) } catch { /* 保持原样 */ }
    const el = decoded ? document.getElementById(decoded) : null
    return { hash: location.hash, hit: !!el, tag: el?.tagName ?? null, text: el?.textContent ?? null }
  })
  assert(jumped.hit && jumped.tag === 'H2' && jumped.text === '架构', `点目录真的跳到该标题（hash=${jumped.hash} → ${jumped.tag}「${jumped.text}」）`)

  // ⑦ alerts
  assert(/md-alert-note/.test(raw.alert.cls || ''), '> [!NOTE] 渲染成 md-alert-note')
  assert(raw.alert.head === '注意', `alert 标题走 i18n 文案（实际「${raw.alert.head}」）`)
  assert(/这是一条提示/.test(raw.alert.body || '') && !raw.alert.raw, 'alert 正文保留且标记行被吃掉（不残留 [!NOTE]）')
  assert(/md-alert-warning/.test(raw.alert.warnCls || '') && raw.alert.warnHead === '警告', '> [!WARNING] 同理（警告）')
  assert(raw.alert.plain === 0 && raw.alert.plainIsBlockquote, '普通引用块不受影响（仍是 blockquote）')

  // ⑧ emoji / frontmatter
  assert(!raw.emoji.hasShortcode && /🚀/.test(raw.emoji.text || ''), `:rocket: 变成 emoji（${(raw.emoji.text || '').trim()}）`)
  assert(raw.fm.card && raw.fm.keys.join(',') === 'title,tags,count', `frontmatter 渲染成卡片且键正确（${raw.fm.keys.join(',')}）`)
  assert(raw.fm.values[0] === 'HAI Harness' && /- go\n- agent/.test(raw.fm.values[1] || ''), 'frontmatter 值保留原样（嵌套块整块保留）')
  assert(raw.fm.headings === 0 && raw.fm.hr === 0, 'frontmatter 不再被当成分隔线 + setext 标题')
  assert(raw.fm.brokenCard && raw.fm.brokenCode, 'frontmatter 解析不出键值时退回卡片内的代码块（不猜）')

  // ④ 本地相对图片
  assert((raw.local.src || '').startsWith('data:image/svg+xml;base64,') && raw.local.natW === 120, `本地相对图经 fs:read-image 拿 data URL 并真的加载（naturalWidth=${raw.local.natW}）`)
  assert(raw.local.localPath === '/mock/proj-auth/brand/hai-logo-light.svg', `相对路径锚到工作区根（实际 ${raw.local.localPath}）`)
  assert(raw.local.relLocalPath === '/mock/proj-auth/img/icon.png', `子目录相对路径同样锚到工作区（实际 ${raw.local.relLocalPath}）`)
  assert(raw.local.alt === 'logo', '本地图的 alt 保留')
  assert(raw.local.missingPlaceholder === 1 && /不是图片文件/.test(raw.local.missingTitle || ''), `非图片扩展名给「路径 + 原因」而不是破图（title=${JSON.stringify(raw.local.missingTitle)}）`)
  // 失败态的**可见原因**也要对：只写「无法显示」，用户分不清「文件不存在」与「通道没起来」
  //（后者正是用户实测踩到的：改了主进程没重启，界面上与前者长得一模一样）
  const missingHint = await page.evaluate(() => document.querySelector('[data-case="notImage"] .md-img-missing-hint')?.textContent ?? null)
  assert(missingHint === '不是图片文件', `失败态直接写明原因（实际「${missingHint}」）`)
  await page.evaluate(() => (window).__breakChannel())
  await page.waitForSelector('[data-case="brokenChannel"] .md-img-missing-hint', { timeout: 8000 })
  const brokenHint = await page.evaluate(() => document.querySelector('[data-case="brokenChannel"] .md-img-missing-hint')?.textContent ?? null)
  assert(brokenHint === '图片通道不可用（重启应用后生效）', `通道不可用（如主进程未重启）时给出可行动的提示（实际「${brokenHint}」）`)
  assert(raw.local.remoteSrc === 'https://example.com/a.png' && !raw.local.remoteLocalPath, '远程图原样走网络（不被本地通道接管）')
  assert(raw.relLink.href === '/mock/proj-auth/docs/guide.md', `相对链接解析成工作区绝对路径（实际 ${raw.relLink.href}）`)

  // 相对链接点开 = 走既有文件面板（验证接线，不只是 href 看着对）
  await page.evaluate(() => {
    const a = document.querySelector('[data-case="relLink"] .md a')
    a.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
  })
  await page.waitForTimeout(300)
  const opened = await page.evaluate(() => {
    const st = window.__store.getState()
    // 标签按「会话/工作区」分组存放（tabsByScope），探针里没有会话 → 退化按 workspace 分组
    const scope = st.activeSessionId ? `s:${st.activeSessionId}` : `w:${st.workspace}`
    const bag = st.tabsByScope?.[scope] ?? Object.values(st.tabsByScope ?? {})[0] ?? { tabs: [] }
    return { scope, tabs: (bag.tabs ?? []).map((t) => t.path ?? t.kind ?? ''), expanded: st.rightExpanded ?? null }
  })
  assert(opened.tabs.some((p) => String(p).includes('docs/guide.md')), `相对链接点开进文件面板（scope=${opened.scope} tabs=${JSON.stringify(opened.tabs)}）`)

  assert(errs.length === 0, `无 pageerror / 无异常 console（${errs.length ? errs.join(' | ') : '干净'}）`)

  // ============ 真·现实样本：本仓库 README.zh-CN.md ============
  // 上面全是构造样本，这一段拿用户截图里那份文件原样跑一遍：它是「预览不够全面」的原始现场。
  await page.waitForTimeout(1500)
  const real = await page.evaluate(() => {
    const md = document.querySelector('[data-case="readme"] .md')
    const txt = md?.textContent ?? ''
    const logo = [...md?.querySelectorAll('img') ?? []].find((i) => i.getAttribute('alt') === 'HAI Harness')
    const h1 = md?.querySelector('h1')
    // 头部那行目录是**原始 HTML 链接**：逐个拿 id 找落点（找不到就是 slug 口径与 GitHub 不一致）
    const tocHrefs = [...md?.querySelectorAll('a[href^="#"]') ?? []]
      .map((a) => a.getAttribute('href') ?? '')
      .filter((h) => h.length > 1)
    const unresolved = tocHrefs.filter((h) => {
      let frag = h.slice(1)
      try { frag = decodeURIComponent(frag) } catch { /* 保持原样 */ }
      return !document.getElementById(frag)
    })
    return {
      escaped: /&lt;p align|<p align="center">\s*<picture>/.test(txt),
      hasPicture: !!md?.querySelector('picture'),
      scripts: md?.querySelectorAll('script').length ?? -1,
      h1Align: h1 ? getComputedStyle(h1).textAlign : null,
      logoAlt: logo?.getAttribute('alt') ?? null,
      logoPath: logo?.getAttribute('data-local-path') ?? null,
      logoNatW: logo?.naturalWidth ?? 0,
      tocCount: tocHrefs.length,
      unresolved,
      headings: md?.querySelectorAll('h1,h2,h3,h4,h5,h6').length ?? 0,
    }
  })
  assert(!real.escaped, '真实 README：不再出现一屏转义 HTML（截图 1 的那个症状消失了）')
  assert(!real.hasPicture && real.scripts === 0, '真实 README：<picture> 已定型、没有 script')
  assert(real.h1Align === 'center', `真实 README：<h1 align="center"> 居中（text-align=${real.h1Align}）`)
  assert(real.logoAlt === 'HAI Harness' && /brand\/hai-logo-dark\.svg$/.test(real.logoPath ?? '') && real.logoNatW === 120, `真实 README：深色主题 logo 经本地通道加载（${real.logoPath}，naturalWidth=${real.logoNatW}）`)
  assert(real.headings >= 15, `真实 README：各级标题都渲染出来了（${real.headings} 个，与源文件 17 个标题同量级）`)
  assert(real.tocCount >= 6 && real.unresolved.length === 0, `真实 README：头部目录 ${real.tocCount} 项全部有落点（对不上：${JSON.stringify(real.unresolved)}）`)

  // 留一张截图（e2e/_* 已 gitignore）：渲染类改动靠肉眼复核，比断言更早发现「不难看但不对」
  const shotDir = path.join(__dirname, '_mdhtml_shots')
  fs.mkdirSync(shotDir, { recursive: true })
  await page.locator('[data-case="readme"]').screenshot({ path: path.join(shotDir, 'readme-zh-cn.png') })
  console.log(`  📸 ${path.relative(APP, path.join(shotDir, 'readme-zh-cn.png'))}`)

  await browser.close()
  console.log(`\n全通过：${pass} 项断言`)
}

main().then(() => { cleanup(); process.exit(0) }).catch((e) => { console.error('\n' + (e?.stack || e)); cleanup(); process.exit(1) })
