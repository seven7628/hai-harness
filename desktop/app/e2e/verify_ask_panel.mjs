#!/usr/bin/env node
// ask_user 提问面板「不阻断对话」回归（2026-09-20）
//
// 起因（用户报障）：ask_user 弹窗是全屏 `.modal-hint` scrim —— 待答期间叙述区被压暗盖住、
// 滚不动、左右栏也点不了，可用户往往要**先翻一遍对话历史再决定怎么答**。
// 同一问题早在 docs/DESKTOP_HANDOFF.md §5 #12 记录过（「待答/待审批时点左栏切换会话被挡住」）。
//
// 修法：提问面板从全屏 modal 改为**停靠面板**（`components/AskUserQuestionPanel.tsx`，
// 挂在 `CenterPanel` 的叙述区与 Composer 之间）——叙述区只被压矮，照常滚动/选中/命中。
//
// 本脚本用真实 CenterPanel（真叙述区 + 真 Composer + 真提问面板）＋真实 index.css 断言：
//   ① 面板在流内（position: static），不遮叙述区（两者矩形不相交）
//   ② 全屏伪元素缺席：没有任何覆盖 ≥95% viewport 的 fixed 层（老 scrim 的形态）
//   ③ 叙述区仍可命中（elementFromPoint 落在 .narrative 内部）——老 scrim 下这里命中遮罩
//   ④ **待答时滚轮真的能翻历史**（用户报障的原话）：滚轮上滚 → scrollTop 变小
//   ⑤ 面板停在输入框上方（面板底 ≈ Composer 顶）；点标题条可收起 → 叙述区拿回整屏（≥70%）
//   ⑥ 作答链路不变：未答禁用 → 点选项启用 → 提交后面板消失
//   ⑦ 决策倒计时：标题条显示「剩余 M:SS」且每秒在走；到点切「已超时」态并禁用提交
//   ⑧ 选项预览（模型的预览能力）：带 preview 的题「左列选项 + 右侧预览」并排（几何实锤：
//      预览框在选项列表右侧）、预览跟随所选项切换、预览走正文同一条 Markdown 管线
//      （<strong>）；没带预览的题版面不变（仍是横排按钮）
//
// 用法：cd desktop/app && node e2e/verify_ask_panel.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4192)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

async function waitPort(port, timeoutMs = 30000) {
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

// 临时探针页：真实 CenterPanel（叙述区 + 提问面板 + 输入框）+ 一份足够长的历史（跑完删除）
const PROBE_DIR = path.join(__dirname, '_askprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh"><head><meta charset="utf-8"><title>ask probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import CenterPanel from '../../src/components/CenterPanel'
import { useAppStore, dispatchEvent } from '../../src/store/useAppStore'
import '../../src/index.css'
// index.css 末尾的 @import 在 dev 下不下发，探针显式 import 皮肤两层（与 verify_user_blocks 同）
import '../../src/runtime-palette.css'
import '../../src/runtime-skin.css'

const q = new URLSearchParams(location.search)
// 与 index.html 一致：皮肤是静态属性（会话页只有 runtime 一套）
document.documentElement.setAttribute('data-skin', 'runtime')
document.documentElement.setAttribute('data-theme', q.get('theme') || 'ink')

await useAppStore.getState().start('/tmp/ask-probe-ws') // 建立 transport（mock），send 才有响应
const timeoutMs = Number(q.get('timeoutMs') || 30 * 60 * 1000) // 默认与后端一致（30 分钟）

// 长历史：叙述区必须溢出，才谈得上「翻历史」。
// **必须走事件**（不是手搓 blocks）：2026-10 起叙述区按「轮」渲染，一轮由
// agent_start/llm_start/content_chunk/llm_end/agent_end 这一段事件构成 —— 只有裸
// assistant 块时 selectTurns 拼不出轮，叙述区是空的（量出来 scrollHeight === clientHeight）。
const T0 = Date.now() - 120_000
const ev = (o, at = 0) => dispatchEvent({ session_id: 'probe', timestamp: T0 + at, ...o })
ev({ event_type: 'user_inputs_consumed', user_texts: ['帮我收敛一下 auth 的过期语义'] })
for (let i = 0; i < 20; i++) {
  const rid = 'h' + i
  const base = i * 4000
  ev({ event_type: 'agent_start', run_id: rid, index: 0, content: '历史轮', model: 'deepseek-v4' }, base)
  ev({ event_type: 'llm_start', run_id: rid, request_id: 'req-' + i, model: 'deepseek-v4' }, base + 200)
  ev({ event_type: 'content_chunk', run_id: rid, request_id: 'req-' + i, content: '第 ' + (i + 1) + ' 段历史：' + '这段正文用来把叙述区撑高，验证「待答时仍可滚轮翻历史」。'.repeat(3) }, base + 400)
  ev({ event_type: 'llm_end', run_id: rid, request_id: 'req-' + i, model: 'deepseek-v4', finish_reason: 'end', usage: { Input: 900, Output: 40, CacheRead: 700, TotalTokens: 940 } }, base + 600)
  ev({ event_type: 'agent_end', run_id: rid, index: 0, content: '', finish_reason: 'end', model: 'deepseek-v4' }, base + 800)
}

useAppStore.setState({
  persona: 'code',
  activeSessionId: 'probe',
  // 提问在 store 里是**列表**（多批并存，按作用域过滤 —— 2026-10 起；旧的单槽 question 已删）
  questions: [{
    id: 'q-batch-1', runId: 'run-7', timeoutMs, ts: Date.now() - 1000,
    items: [
      // 第一条：选项带 preview（「必须看见才能选」的备选）→ 面板走并排预览版面；
      // 第二条：纯字符串选项（历史事件/回放形态）→ 走原来的横排按钮版面
      {
        id: 'q-1-1', question: '过期 token 的对外行为应如何处理？',
        options: [
          { label: '401 + token_expired 错误码', preview: '**401** Unauthorized — {"code": "token_expired"}' },
          { label: '静默自动续期', preview: 'Set-Cookie: session=<new>（调用方无感）' },
          { label: '记录日志继续放行', preview: '(log) token expired → pass-through' },
        ],
      },
      { id: 'q-1-2', question: '改动范围？', options: ['仅 auth 包', '连同其余 500 兜底一起收敛'] },
    ],
  }],
})

createRoot(document.getElementById('root')!).render(
  <div style={{ height: '100vh', display: 'flex' }}><CenterPanel /></div>,
)
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_askprobe_run/index.html`
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

// 覆盖 ≥95% viewport 的 fixed/absolute 层 = 全屏遮罩（老 .modal-hint scrim 的形态）
const overlayProbe = () => {
  const vw = window.innerWidth
  const vh = window.innerHeight
  const big = []
  for (const el of document.querySelectorAll('body *')) {
    const cs = getComputedStyle(el)
    if (cs.display === 'none' || cs.visibility === 'hidden' || cs.opacity === '0') continue
    if (cs.position !== 'fixed' && cs.position !== 'absolute') continue
    const r = el.getBoundingClientRect()
    if (r.width >= vw * 0.95 && r.height >= vh * 0.95) big.push(`${el.className || el.tagName}(${cs.position})`)
  }
  return big
}

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true })
  const page = await browser.newPage({ viewport: { width: 1100, height: 820 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => { if (m.type() === 'error') errs.push(m.text().slice(0, 200)) })

  await page.goto(`${PROBE_URL}?theme=ink`)
  await page.waitForSelector('.dock')
  await page.waitForTimeout(200) // 等叙述区置底校准

  // ① 面板在流内 + 不遮叙述区
  const geo = await page.evaluate(() => {
    const R = (s) => { const el = document.querySelector(s); const r = el.getBoundingClientRect(); return { top: Math.round(r.top), bottom: Math.round(r.bottom), left: Math.round(r.left), right: Math.round(r.right), h: Math.round(r.height) } }
    const nar = document.querySelector('.narrative')
    const cs = getComputedStyle(document.querySelector('.dock'))
    return {
      dockPos: cs.position,
      dockRole: document.querySelector('.dock').getAttribute('role'),
      ariaModal: document.querySelector('.dock').getAttribute('aria-modal'),
      dock: R('.dock'),
      narrative: R('.narrative'),
      composer: R('.composer'),
      narScroll: { top: nar.scrollTop, h: nar.scrollHeight, client: nar.clientHeight },
      vh: window.innerHeight,
      dockQ: document.querySelectorAll('.dock').length,
      hiddenInputs: document.querySelector('.dock input')?.name,
    }
  })
  console.log('\n[geo]', JSON.stringify(geo))
  assert(geo.dockPos === 'static', `提问面板在文档流内（position: ${geo.dockPos}）—— 不再是全屏浮层`)
  assert(geo.dockRole === 'region' && geo.ariaModal === null, '提问面板是非模态（role=region，无 aria-modal）')
  assert(geo.dock.top >= geo.narrative.bottom - 1, `面板不覆盖叙述区（叙述区底 ${geo.narrative.bottom} / 面板顶 ${geo.dock.top}）`)
  assert(Math.abs(geo.dock.top - geo.narrative.bottom) <= 1, `面板紧贴在叙述区下方（面板顶 ${geo.dock.top} / 叙述区底 ${geo.narrative.bottom}）`)
  assert(Math.abs(geo.dock.bottom - geo.composer.top) <= 10, `面板贴着输入框上沿（面板底 ${geo.dock.bottom} / 输入框顶 ${geo.composer.top}）`)
  assert(geo.narrative.h >= geo.vh * 0.3, `待答时叙述区仍有一屏可观的高度（${geo.narrative.h}px ≥ 视口 30% = ${Math.round(geo.vh * 0.3)}px）`)
  assert(geo.narScroll.h > geo.narScroll.client, `叙述区内容确实溢出、可滚动（${geo.narScroll.h} > ${geo.narScroll.client}）`)

  // ② 全屏遮罩缺席
  const overlays = await page.evaluate(overlayProbe)
  assert(overlays.length === 0, `没有任何全屏遮罩层（实得 ${overlays.length}：${overlays.join(', ')}）`)

  // ③ 叙述区仍可命中：中心点落在叙述区内部（老 scrim 下这里命中的是遮罩）
  const hit = await page.evaluate(() => {
    const r = document.querySelector('.narrative').getBoundingClientRect()
    const el = document.elementFromPoint(Math.round(r.left + r.width / 2), Math.round(r.top + r.height / 2))
    return { tag: el?.tagName, cls: el?.className && String(el.className).slice(0, 40), inNarrative: Boolean(el?.closest('.narrative')) }
  })
  assert(hit.inNarrative, `叙述区正中仍是叙述区（命中 <${hit.tag} class="${hit.cls}">）`)

  // ④ 用户报障的原话：待答时滚轮能翻历史
  const box = await page.locator('.narrative').boundingBox()
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
  const before = await page.evaluate(() => document.querySelector('.narrative').scrollTop)
  await page.mouse.wheel(0, -600)
  await page.waitForTimeout(250)
  const after = await page.evaluate(() => document.querySelector('.narrative').scrollTop)
  console.log(`  [wheel] scrollTop ${before} → ${after}`)
  assert(after < before, `待答时滚轮上滚真的翻动了历史（scrollTop ${before} → ${after}）`)
  const dockStill = await page.locator('.dock').isVisible()
  assert(dockStill, '翻历史期间提问面板始终可见（不会被滚动带跑）')

  // ⑤ 收起面板 → 叙述区拿回整屏（想通读历史时的开关）；再展开，作答状态不丢
  const hBefore = (await page.locator('.narrative').boundingBox()).height
  await page.locator('.dock .dock-hd').click()
  await page.waitForTimeout(200)
  const folded = (await page.locator('.narrative').boundingBox()).height
  const foldText = await page.locator('.dock .dock-fold-hint').innerText()
  console.log(`  [fold] narrative ${Math.round(hBefore)} → ${Math.round(folded)}px · "${foldText}"`)
  assert(folded >= geo.vh * 0.7, `收起后叙述区拿回整屏（${Math.round(folded)}px ≥ 视口 70%）`)
  assert(/0\/2/.test(foldText), `收起态标题条仍显示作答进度（"${foldText}"）`)
  await page.locator('.dock .dock-hd').click()
  await page.waitForTimeout(150)
  // runtime 的提问面板是**分页**的（一次一题，.ask-step 报进度）：收起再展开仍停在原来那一题
  assert(await page.locator('.dock .ask').count() === 1, '再点标题条展开：当前那一题还在（收起不丢状态）')
  assert(/1/.test(await page.locator('.dock .ask-step').innerText()),
    `展开后仍停在第一题（.ask-step = "${(await page.locator('.dock .ask-step').innerText()).replace(/\n/g, ' ')}"）`)

  // ⑧ 选项预览（模型给选项配 preview）：runtime 的 .ask-body 本身就是两列
  //    （选项 max-content + 预览 1fr），预览是**排版好的纯文本**（mono + pre，保缩进）——
  //    不过 Markdown 渲染器（ASCII 图换行就不是图了，见 parts/ask.css:238-247）。
  const pv = await page.evaluate(() => {
    const R = (el) => { const r = el.getBoundingClientRect(); return { left: Math.round(r.left), right: Math.round(r.right) } }
    const card = document.querySelector('.dock .ask')
    const list = card.querySelector('.ask-list')
    const box = card.querySelector('.ask-pv')
    const pre = card.querySelector('.ask-pv-pre')
    return {
      listCount: card.querySelectorAll('.ask-list').length,
      optionCount: card.querySelectorAll('.ask-opt').length,
      // 逐字比对按钮文字：只数个数会漏掉「按钮渲染成空 label」这种静默坏掉
      labels: [...card.querySelectorAll('.ask-opt .ask-lab')].map((b) => b.textContent),
      listBox: R(list),
      previewBox: box ? R(box) : null,
      hd: card.querySelector('.ask-pv-head')?.textContent || '',
      which: card.querySelector('.ask-pv-which')?.textContent || '',
      preText: pre?.textContent || '',
      form: card.getAttribute('data-form'),
      aria: box?.getAttribute('aria-label') || '',
      role: box?.getAttribute('role') || '',
      // 预览不过 Markdown：正文里的 **401** 必须原样留在纯文本里
      strong: Boolean(box?.querySelector('strong')),
    }
  })
  console.log('  [preview]', JSON.stringify(pv))
  assert(pv.form === 'preview' && pv.listCount === 1, `带预览的题走 data-form=preview（实得 ${pv.form}）`)
  assert(pv.optionCount === 3, `选项逐条渲染（${pv.optionCount} 条）`)
  assert(JSON.stringify(pv.labels) === JSON.stringify(['401 + token_expired 错误码', '静默自动续期', '记录日志继续放行']),
    `选项文字逐字渲染（实得 ${JSON.stringify(pv.labels)}）`)
  assert(pv.previewBox !== null && pv.previewBox.left >= pv.listBox.right - 1,
    `预览真的在选项右侧（列表右 ${pv.listBox.right} / 预览左 ${pv.previewBox?.left}）`)
  assert(pv.role === 'figure' && /401/.test(pv.aria), `预览是可读的图注区域（role=${pv.role} aria-label="${pv.aria}"）`)
  assert(/preview/.test(pv.hd) && /401 \+ token_expired/.test(pv.which),
    `预览头标出这是哪个选项的预览（"${pv.hd.replace(/\n/g, ' ').trim()}"）`)
  assert(/401\*\* Unauthorized/.test(pv.preText), `默认先显示第一个带预览选项的预览（"${pv.preText.slice(0, 40)}"）`)
  assert(!pv.strong && /\*\*401\*\*/.test(pv.preText),
    '预览是纯文本 <pre>（**401** 原样保留，不过 Markdown —— 与设计一致）')
  // 点第二个选项 → 预览切到它的内容（单选语义未变：点选 = 作答）
  await page.locator('.dock .ask').first().locator('.ask-opt').nth(1).click()
  await page.waitForTimeout(150)
  const pv2 = await page.evaluate(() => {
    const card = document.querySelector('.dock .ask')
    return { which: card.querySelector('.ask-pv-which')?.textContent || '', pre: card.querySelector('.ask-pv-pre')?.textContent || '' }
  })
  console.log('  [preview-switch]', JSON.stringify(pv2))
  assert(/静默自动续期/.test(pv2.which) && /Set-Cookie/.test(pv2.pre), '点另一个选项后预览切到该选项的内容')
  await page.screenshot({ path: '/tmp/ask-panel-preview.png' })
  // 预览切换本身已构成作答（单选：点选 = 作答）→ 重新载入探针页，让 ⑥ 从干净未答态断言
  await page.goto(`${PROBE_URL}?theme=ink`)
  await page.waitForSelector('.dock')
  await page.waitForTimeout(300)

  // ⑥ 作答链路不变：未答禁用 → 点选项启用 → 下一题 → 全部答完提交 → 面板消失
  //    （runtime 一次只渲染一题：最后一题是「完成」，前面几题是「下一步」）
  const goBtn = page.locator('.dock .ask-btn.go')
  assert(await goBtn.isDisabled(), '未答完时前进/提交按钮禁用（必须全部作答的语义未变）')
  await page.locator('.dock .ask-opt').first().click()
  assert(!(await goBtn.isDisabled()), '第一题选好后按钮启用（1/2）')
  await goBtn.click() // 下一步 → 第二题
  await page.waitForTimeout(250)
  const step2 = await page.locator('.dock .ask-step').innerText()
  assert(/2/.test(step2), `点「下一步」进入第二题（.ask-step = "${step2.replace(/\n/g, ' ')}"）`)
  // 第二题是纯字符串选项（历史事件 / 回放形态）：逐字渲染、无预览列
  const q2 = await page.evaluate(() => {
    const card = document.querySelector('.dock .ask')
    return {
      labels: [...card.querySelectorAll('.ask-opt .ask-lab')].map((b) => b.textContent),
      form: card.getAttribute('data-form'),
      hasPv: Boolean(card.querySelector('.ask-pv')),
    }
  })
  assert(JSON.stringify(q2.labels) === JSON.stringify(['仅 auth 包', '连同其余 500 兜底一起收敛']),
    `纯字符串选项在面板上逐字渲染（实得 ${JSON.stringify(q2.labels)}）`)
  assert(q2.form === 'plain' && !q2.hasPv, `无预览的题走 plain 版面、不带预览列（form=${q2.form}）`)
  assert(await goBtn.isDisabled(), '第二题未答 → 按钮仍禁用')
  await page.locator('.dock .ask-opt').first().click()
  assert(!(await goBtn.isDisabled()), '两题都答好 → 提交键启用（2/2）')
  await goBtn.click()
  await page.waitForTimeout(300)
  assert(await page.locator('.dock').count() === 0, '提交后面板消失（乐观清除，等待模型续跑）')
  const narAfter = await page.evaluate(() => document.querySelector('.narrative').getBoundingClientRect().height)
  assert(narAfter > geo.narrative.h, `面板收起后叙述区回高（${geo.narrative.h} → ${Math.round(narAfter)}px）`)

  assert(errs.length === 0, `控制台 0 报错（实得 ${errs.length}：${errs.slice(0, 3).join(' | ')}）`)

  // ⑦ 决策倒计时：显示剩余时间、每秒在走；到点切「已超时」态并禁用提交
  await page.goto(`${PROBE_URL}?theme=ink`)
  await page.waitForSelector('.dock .dock-countdown')
  const cd1 = await page.locator('.dock .dock-countdown').innerText()
  await page.waitForTimeout(2200)
  const cd2 = await page.locator('.dock .dock-countdown').innerText()
  console.log(`  [countdown] "${cd1}" → "${cd2}"`)
  assert(/剩余 \d+:\d\d/.test(cd1) && cd1 !== cd2, `倒计时显示剩余时间且在走（"${cd1}" → "${cd2}"）`)
  assert(cd1.startsWith('剩余 29:') || cd1.startsWith('剩余 30:'), `倒计时口径 = 后端等待窗口（30 分钟），实得 "${cd1}"`)
  await page.screenshot({ path: '/tmp/ask-panel-countdown.png' })

  await page.goto(`${PROBE_URL}?theme=ink&timeoutMs=2000`) // 2s 窗口：直接看超时态
  await page.waitForSelector('.dock')
  await page.waitForTimeout(2600)
  const over = await page.locator('.dock .dock-countdown').innerText()
  // 超时态：runtime 的面板把提示写在 .ask-foot 的 .ask-note 里（旧皮肤是 .timeout）
  const overHint = await page.locator('.dock .ask-foot .ask-note').innerText()
  console.log(`  [timeout] "${over}" · "${overHint}"`)
  assert(/已超时/.test(over), `窗口用尽后显示已超时（"${over}"）`)
  assert(await page.locator('.dock .ask-btn.go').isDisabled(), '已超时后提交按钮禁用（后端已放行，不再假装能提交）')
  assert(/未作答/.test(overHint), `已超时提示说明「未作答 → 模型按最佳判断继续」（"${overHint}"）`)
  await page.screenshot({ path: process.env.SHOT || '/tmp/ask-panel-timedout.png' })

  await browser.close()
  console.log(`\n全部通过（${pass} 项断言）`)
}

main().then(() => process.exit(0)).catch((e) => { console.error(`\n❌ ${e.message}`); process.exit(1) })
