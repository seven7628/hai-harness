#!/usr/bin/env node
// 用户消息块回归（F43 / R05c，2026-11 用户报障）
//
// 现象：默认皮肤（设置里 `narrative_skin` 未设 → current）下，用户自己发出的那句话
// 在转录里**什么都不渲染**；切到 runtime 才看得见。也就是说默认安装打开就是坏的。
//
// 根因（git 考古，见下）：`6d28d60 runtime A2: events that used to be cards are now one
// line each` 在给 user 块加 runtime 分支的同时，把紧随其后的**老分支整段删掉**：
//     if (b.kind === 'user' && skin === 'runtime') return <UserBlock …/>   ← 保留
// -   if (b.kind === 'user') { …<div className="msg user">… }               ← 被删
// 于是 renderBlock 落到函数末尾的 `if (b.kind !== 'assistant') return null`，
// current/v3 两个皮肤的用户消息被静默吞掉（CSS .msg.user / .user-imgs / .user-refs
// 全部还在，只是没人再产出这些节点）。
//
// 本脚本锁住的行为（共 4 档：默认不设 / current / v3 / runtime，各一遍）：
//   ① 用户消息在转录里**真的存在**（.narrative .msg.user 计数）
//   ② 渲染出来的内容 = 发出去的内容（逐行出现在块文本里，换行/长行都不许丢）
//   ③ 它**看得见**：有盒子、有面积、display/visibility/opacity 正常，且块中心点的
//      实际命中元素就在块内（防「渲染了但被遮住/零高」）
//   ④ 助手回复仍在、且排在用户消息**之后**（顺序是转录的语义，不是样式）
//   ⑤ 图文消息的两个附属面：图片附件成真图、@引用列出已加载与缺失
//      （.user-imgs / .user-refs；runtime 是 .rt-imgs / .rt-ref）
//
// **这条断言存在的原因 —— F43，默认皮肤漏渲染用户消息**：
// 这个 P0 是靠人工截图发现的，没有任何测试会在「用户消息消失」时变红（.msg.user 计数
// 没人断言，转录里少一块也不会报错）。所以它挂在 `npm test` 链尾（e2e:subagent-style
// 之后）：删掉 Narrative.renderBlock 里任何一条 user 分支，这里立刻红。
//
// 皮肤怎么定：与 App.tsx 同一条表达式（`settings.narrative_skin ?? 'current'`），
// 经同一个 applyNarrativeSkin() 落到 <html data-skin>。`?skin=` 留空 = **什么都不设**，
// 这就是「默认设置」那一档（复现时必须用它，不能用 current 代打）。
//
// **探针主文件里的三份皮肤 CSS 必须显式 import（不是冗余）**：index.css 末尾那三行
// @import（narrative-skin / runtime-palette / runtime-skin）在 vite **dev** 下不下发 ——
// 实测只 import index.css 时 `.rt-head` 的 display 是 block 而不是 flex、runtime 下
// `.msg.assistant .who` 也没被隐藏，也就是「皮肤只生效一半」，量出来的 runtime 截图是
// 假象（`vite build` 的产物里这些规则是在的，所以只有探针会踩）。本脚本量的正是
// 「用户块在三档皮肤下长什么样」，缺了这三份就变成「在没皮肤的现实里长什么样」。
//
// 用法：cd desktop/app && node e2e/verify_user_blocks.mjs
//      REPORT=1 node e2e/verify_user_blocks.mjs   # 只取证不判（复现用）
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { freePort, waitHttp } from './_serve.mjs'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
// 端口由内核分配（见 e2e/_serve.mjs 的注释）：固定端口在链里会撞上一个还没退干净的服务器。
const PORT = Number(process.env.PORT) || await freePort()
const SHOTS = path.join(__dirname, '_user_blocks_shots')
const REPORT = process.env.REPORT === '1' // 只量不判（复现/取证用）

let pass = 0
const failures = []
// 逐项判、不中断：三档皮肤的病是**同一个**，一次跑完才看得见「到底哪几档坏、
// 坏在哪一条」，而不是第一档一红就收工（复现证据要的就是这个）。
const check = (cond, m) => {
  if (cond) { pass++; console.log(`  ✅ ${m}`); return }
  failures.push(m)
  console.log(`  ❌ ${m}`)
}

async function waitPort(port, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { s.destroy(); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 用户发出去的三行：换行必须在（老路径 issue 7 就是它）、长行不许被折掉、
// 中间一行是错误串（含引号与括号）。**不含 Markdown 记号** —— 这样「内容一致」
// 的判据对三档皮肤是同一把尺（runtime 走 Markdown，current 走 InlineCode）。
const USER_LINES = [
  '帮我看看这个报错，顺便确认下 /tmp/demo.log 还在不在：',
  "TypeError: Cannot read properties of undefined (reading 'map')",
  '第 3 行是我加的日志，别删。',
]
const USER_TEXT = USER_LINES.join('\n')
const REPLY = '看过日志了：那个 map 是 items 为空时调的，改成可选链就行。'
// 一条**图文**消息（截图 + 文字）+ 两个 @引用（一个已加载、一个缺失）：
// 老路径的 .user-imgs / .user-refs 与 runtime 的 .rt-imgs / .rt-ref 说的是同一件事，
// 断言只认「块里有图」「块里出现两条引用路径」，两套 DOM 都过得去。
const SHOT_SVG = '<svg xmlns="http://www.w3.org/2000/svg" width="120" height="80">'
  + '<rect width="120" height="80" fill="#232a33"/>'
  + '<rect x="0" y="0" width="120" height="18" fill="#2d353f"/>'
  + '<circle cx="9" cy="9" r="3" fill="#d97a66"/><circle cx="20" cy="9" r="3" fill="#d9b56a"/>'
  + '<rect x="8" y="28" width="104" height="3" fill="#4a5561"/>'
  + '<rect x="8" y="38" width="86" height="3" fill="#4a5561"/>'
  + '<rect x="8" y="48" width="96" height="3" fill="#d97a66"/><rect x="8" y="58" width="70" height="3" fill="#4a5561"/></svg>'
const IMG_URL = `data:image/svg+xml;base64,${Buffer.from(SHOT_SVG).toString('base64')}`
const REF_OK = 'docs/README.md'
const REF_MISSING = 'src/ghost.ts'

// 临时探针页：真实 CenterPanel（含真 Narrative）+ 生产同一份 CSS，跑完删除。
// **三档皮肤各自的那层 CSS 要显式 import**：index.css 末尾那三行 @import（narrative-skin /
// runtime-palette / runtime-skin）在 vite **dev** 下不下发（实测：探针里只 import
// index.css 时 .rt-head 的 display 是 block 而不是 flex、.msg.assistant 的 .who 也没被
// runtime 隐藏 —— 也就是「皮肤只生效了一半」，量出来的 runtime 截图是假象；
// `vite build` 的产物里这几条规则是在的）。这与其它探针的做法一致（verify_agent_tree /
// verify_plan_card_skin 都显式 import narrative-skin.css）。
const PROBE_DIR = path.join(__dirname, '_userprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh"><head><meta charset="utf-8"><title>user block probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import CenterPanel from '../../src/components/CenterPanel'
import { useAppStore, dispatchEvent } from '../../src/store/useAppStore'
import { applyNarrativeSkin } from '../../src/lib/narrativeSkin'
import '../../src/index.css'
import '../../src/narrative-skin.css'
import '../../src/runtime-palette.css'
import '../../src/runtime-skin.css'

const q = new URLSearchParams(location.search)
const WANT = q.get('skin') || '' // 空 = 不设 narrative_skin（默认设置那一档）

// 默认设置：与 App.tsx 同一条表达式 + 同一个 applyNarrativeSkin。
// 先按用户可能的样子把设置摆好，**包括不设**（delete 掉键），再由表达式推 data-skin。
useAppStore.setState((s) => {
  const settings = { ...s.settings }
  if (WANT) settings.narrative_skin = WANT
  else delete settings.narrative_skin
  return { settings }
})
const skin = useAppStore.getState().settings.narrative_skin ?? 'current'
applyNarrativeSkin(skin)
document.documentElement.setAttribute('data-theme', 'ink')

const SID = 'probe'
useAppStore.setState({
  workspace: '/mock/proj-demo',
  activeSessionId: SID,
  bridgeStatus: 'running',
  sessions: [{ id: SID, title: '用户消息' }],
  projects: [{ path: '/mock/proj-demo', sessions: [{ id: SID, title: '用户消息' }] }],
  views: {},
  streaming: false,
  pendingQueue: [],
  blocks: [],
  runNodes: [],
  approvals: [],
  diffs: [],
  todos: [],
  usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, reasoning: 0 },
  turns: [],
  costUsd: 0,
  toolStartTs: {},
})

const T0 = Date.now() - 60_000
const ev = (o, at = 0) => dispatchEvent({ session_id: SID, timestamp: T0 + at, ...o })
// 用户发言 → 服务端回执（user_inputs_consumed 是**唯一**把用户这句话落成块的入口，
// 实时与重放共用它；contents 是权威内容块，与生产同形）
ev({
  event_type: 'user_inputs_consumed',
  user_texts: [${JSON.stringify(USER_TEXT)}],
  user_contents: [[
    { type: 'text', content: ${JSON.stringify(USER_TEXT)} },
    { type: 'image', content: ${JSON.stringify(IMG_URL)}, mime_type: 'image/svg+xml' },
  ]],
})
// @引用展开结果：挂到「最近一条 user 块」下方（同一请求的判定在用户消息之后到达）
ev({
  event_type: 'refs_loaded',
  items: [
    { path: ${JSON.stringify(REF_OK)}, status: 'loaded', resolved: '/mock/proj-demo/docs/README.md', kind: 'file' },
    { path: ${JSON.stringify(REF_MISSING)}, status: 'missing' },
  ],
})
ev({ event_type: 'agent_start', run_id: 'main-1', index: 0, content: '处理用户输入', model: 'deepseek-v4' })
ev({ event_type: 'llm_start', run_id: 'main-1', request_id: 'r-1', model: 'deepseek-v4' }, 1_000)
ev({ event_type: 'content_chunk', run_id: 'main-1', request_id: 'r-1', content: ${JSON.stringify(REPLY)} }, 2_400)
ev({ event_type: 'llm_end', run_id: 'main-1', request_id: 'r-1', model: 'deepseek-v4', finish_reason: 'end', usage: { Input: 900, Output: 40, CacheRead: 700, TotalTokens: 940 } }, 3_000)
ev({ event_type: 'agent_end', run_id: 'main-1', index: 0, content: '处理用户输入', finish_reason: 'end', model: 'deepseek-v4' }, 3_200)

createRoot(document.getElementById('root')!).render(<CenterPanel />)

`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_userprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'ignore' })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)
const watchdog = setTimeout(() => {
  console.error('\n❌ 超时（120s）：vite / chromium 未按预期完成')
  cleanup()
  process.exit(1)
}, 120_000)
watchdog.unref()

// 一条探针：量 .msg.user 的存在 / 内容 / 可见性 / 与助手块的先后。
async function probe(page, want, label) {
  await page.goto(`${PROBE_URL}${want ? `?skin=${want}` : ''}`, { waitUntil: 'load' })
  await page.waitForSelector('.narrative', { timeout: 15000 })
  // 等助手回复真的落进来（转录铺开有错峰动画，等它静下来再量）
  await page.waitForFunction(() => document.querySelectorAll('.msg.assistant').length > 0, null, { timeout: 15000 })
  await page.waitForTimeout(600)

  const facts = await page.evaluate((refs) => {
    const narrative = document.querySelector('.narrative')
    const users = [...document.querySelectorAll('.narrative .msg.user')]
    const heads = [...document.querySelectorAll('.narrative .msg.assistant')]
    const box = (el) => {
      if (!el) return null
      const r = el.getBoundingClientRect()
      const s = getComputedStyle(el)
      return {
        w: Math.round(r.width), h: Math.round(r.height),
        top: Math.round(r.top), left: Math.round(r.left),
        display: s.display, visibility: s.visibility, opacity: s.opacity,
      }
    }
    // 块中心点真的命中它自己（或它的后代）吗 —— 「渲染了但看不见」的硬判据
    const hit = (el) => {
      if (!el) return null
      const r = el.getBoundingClientRect()
      if (!r.width || !r.height) return null
      const node = document.elementFromPoint(Math.round(r.left + r.width / 2), Math.round(r.top + r.height / 2))
      return { inside: !!node && (el === node || el.contains(node)), tag: node ? node.tagName.toLowerCase() : null }
    }
    const norm = (s) => (s || '').replace(/\\s+/g, ' ').trim()
    const u0 = users[0]
    const img = u0 ? u0.querySelector('img') : null
    const imgBox = img ? img.getBoundingClientRect() : null
    return {
      skinAttr: document.documentElement.getAttribute('data-skin'),
      densityAttr: document.documentElement.getAttribute('data-narrative-density'),
      narrative: !!narrative,
      userCount: users.length,
      userClass: users[0] ? users[0].className : null,
      // innerText 取**渲染后**的文字（受 white-space / display 影响），不是 DOM 原文
      userText: users[0] ? users[0].innerText : null,
      userNorm: users[0] ? norm(users[0].innerText) : null,
      userBox: box(users[0]),
      userHit: hit(users[0]),
      userIsRt: !!document.querySelector('.narrative .msg.user.rt-you'),
      // 图文消息的缩略图（老路径 .user-imgs > img / runtime .rt-imgs > button > img）
      imgCount: u0 ? u0.querySelectorAll('img').length : 0,
      imgNatural: img ? `${img.naturalWidth}×${img.naturalHeight}` : null,
      imgBox: imgBox ? { w: Math.round(imgBox.width), h: Math.round(imgBox.height) } : null,
      // @引用状态行（老路径 .user-refs-line / runtime .rt-ref）：两档都印路径原文
      refOk: u0 ? u0.innerText.includes(refs.ok) : false,
      refMissing: u0 ? u0.innerText.includes(refs.missing) : false,
      refClass: u0 ? (u0.querySelector('.user-refs') ? '.user-refs' : u0.querySelector('.rt-toks') ? '.rt-toks' : null) : null,
      assistantCount: heads.length,
      assistantNorm: heads[0] ? norm(heads[0].innerText) : null,
      assistantTop: heads[0] ? Math.round(heads[0].getBoundingClientRect().top) : null,
    }
  }, { ok: REF_OK, missing: REF_MISSING })

  const shot = path.join(SHOTS, `${label}.png`)
  await page.screenshot({ path: shot })
  return { ...facts, shot }
}

async function main() {
  fs.rmSync(SHOTS, { recursive: true, force: true })
  fs.mkdirSync(SHOTS, { recursive: true })
  await waitPort(PORT)
  // 端口可连只说明**有东西**在监听；再确认这张探针页真能开（子进程退出即失败）
  await waitHttp(PROBE_URL, { child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1180, height: 820 } })
  page.on('pageerror', (e) => console.error('[pageerror]', e.message))

  // 三档：'' = 默认（不设 narrative_skin），其余按设置里的值
  const CASES = [['', 'default'], ['current', 'current'], ['v3', 'v3'], ['runtime', 'runtime']]
  const got = {}
  for (const [want, label] of CASES) {
    console.log(`\n── 皮肤 ${label}${want ? '' : '（narrative_skin 未设置）'} ──`)
    const r = await probe(page, want, label)
    got[label] = r
    console.log(`  data-skin=${r.skinAttr}  .msg.user=${r.userCount}  .msg.assistant=${r.assistantCount}`
      + `  .rt-you=${r.userIsRt ? 'yes' : 'no'}`)
    console.log(`  用户块盒子 ${JSON.stringify(r.userBox)}  中心点命中 ${JSON.stringify(r.userHit)}`)
    console.log(`  用户块文本（innerText）= ${JSON.stringify(r.userText)}`)
  }
  console.log(`\n  📸 截图：${path.relative(APP, SHOTS)}/${CASES.map((c) => c[1]).join(', ')}`)

  if (!REPORT) {
    for (const [, label] of CASES) {
      const r = got[label]
      const norm = (s) => (s || '').replace(/\s+/g, ' ').trim()
      const text = norm(r.userNorm)
      console.log(`\n── 断言 ${label} ──`)
      // ① 存在
      check(r.narrative, `${label}：转录容器存在`)
      check(r.userCount === 1, `${label}：用户消息块渲染了（.msg.user 应为 1，实际 ${r.userCount}）`)
      // ② 内容一致：三行逐行出现（顺序即语义）
      if (!text) check(false, `${label}：用户块有文本可读（实际 ${JSON.stringify(r.userText)}）`)
      else {
        let at = -1
        for (const line of USER_LINES) {
          const idx = text.indexOf(norm(line), at + 1)
          check(idx > at, `${label}：用户块含原话「${line.slice(0, 24)}${line.length > 24 ? '…' : ''}」${idx > at ? '' : '（缺失或顺序错）'}`)
          at = idx > at ? idx : at
        }
      }
      // ③ 看得见
      const b = r.userBox
      check(!!b && b.w > 0 && b.h > 0, `${label}：用户块有面积（实际 ${b ? `${b.w}×${b.h}` : '无盒子'}）`)
      check(!!b && b.display !== 'none' && b.visibility !== 'hidden' && Number(b.opacity) > 0.05,
        `${label}：用户块未 display:none / visibility:hidden / opacity≈0（实际 ${b ? `${b.display}/${b.visibility}/${b.opacity}` : '无盒子'}）`)
      check(r.userHit?.inside === true,
        `${label}：用户块中心点的实际命中元素在块内（防被遮住/零高，实际命中 ${r.userHit?.tag ?? '无'})`)
      // ③b 图文消息的两个附属面（同一提交删掉的分支里的其余两条路）
      check(r.imgCount === 1 && r.imgNatural !== null && r.imgBox?.w > 0 && r.imgBox?.h > 0,
        `${label}：图片附件渲染成真图（${r.imgNatural ?? '无图'}，盒子 ${r.imgBox ? `${r.imgBox.w}×${r.imgBox.h}` : '无'}）`)
      check(r.refOk && r.refMissing,
        `${label}：@引用状态行列出已加载与缺失（${r.refClass ?? '无容器'}；现状 ok=${r.refOk} missing=${r.refMissing}）`)
      // ④ 助手回复照旧，且排在用户消息之后
      check(r.assistantCount === 1, `${label}：助手回复仍在（${r.assistantCount} 条）`)
      check(norm(r.assistantNorm).includes(REPLY.slice(0, 12)), `${label}：助手回复内容正确`)
      check(!!b && r.assistantTop != null && b.top < r.assistantTop,
        `${label}：用户消息排在助手回复之前（实际 ${b?.top ?? '无'} vs ${r.assistantTop ?? '无'}）`)
    }
    // runtime 皮肤必须仍然走 UserBlock（不许被这次修复改掉）
    console.log('\n── 断言 皮肤路由 ──')
    check(got.runtime.userIsRt === true, 'runtime：仍由 UserBlock（.rt-you）渲染，行为未变')
    check(got.current.userIsRt === false && got.v3.userIsRt === false,
      'current/v3：不走 runtime 的 UserBlock（各自皮肤的老 DOM）')
  }

  await browser.close()
  cleanup()
  if (REPORT) {
    console.log('\n[e2e] 取证完成（REPORT=1，未断言）')
    return
  }
  if (failures.length) {
    console.error(`\n[e2e] 用户消息块回归失败 ${failures.length} 项 / 通过 ${pass} 项`)
    for (const f of failures) console.error(`  · ${f}`)
    process.exitCode = 1
    return
  }
  console.log(`\n[e2e] 用户消息块回归全部通过（${pass} 项断言）`)
}

main().catch((e) => { console.error('  ❌ E2E 失败', e); cleanup(); process.exit(1) })
