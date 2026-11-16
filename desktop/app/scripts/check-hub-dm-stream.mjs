#!/usr/bin/env node
// 场景2b 浏览器验收：DM 按 LLM 轮次流式成多条气泡（不再是「一条 live 从头拼到尾」）。
//
// 背景：bridge 现在每轮 LLM 正文既发 hub_message_delta（group_id=""、message_id=msg_xxx）
// 又**仍然**发 content_chunk（旧 live 路径），轮末一条 hub_message（message.id = 该轮 delta
// 的 message_id）。前端若把两条路径都画出来，用户会看到「同一段正文两条气泡」。
//
// 用法（默认自起 dev server；5220 端口，不占 5173）：
//   node scripts/check-hub-dm-stream.mjs
//   node scripts/check-hub-dm-stream.mjs --url http://localhost:5180/   # 复用已起的
//   node scripts/check-hub-dm-stream.mjs --out /tmp/hub-dm-stream --keep   # 保留截图目录
//
// mock 夹具：?hubFixture=dmstream（transport/mock.ts 的 mockDMStreamReply）——
// 两轮回复，每轮 content_chunk + hub_message_delta 并发，轮末各一条 hub_message。
//
// 判据（任一失败 → exit 1）：
//   · 流式期间任何时刻只有 1 条草稿气泡（live 与草稿不能同时画出来）；
//   · 两轮各自成一条气泡（不同轮次的正文不拼进同一气泡）→ 至少 2 种流式正文；
//   · 草稿带 hub.streaming 标签，且草稿在时没有额外的「思考中」行；
//   · final 之后草稿清零、正好 2 条 agent 消息、每段正文只出现一次（不重复）；
//   · 无 console error / pageerror。
import { chromium } from 'playwright'
import { spawn } from 'node:child_process'
import { mkdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const PORT = Number(arg('port', process.env.HUB_DM_STREAM_PORT || '5220'))
const URL_ARG = arg('url', '')
const BASE = URL_ARG || `http://localhost:${PORT}/`
const OUT = arg('out', '/tmp/hub-dm-stream')
const NO_SPAWN = argv.includes('--no-spawn')
const ROUND1 = '先说结论'
const ROUND2 = '再补证据'

const failed = []
const errors = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }

mkdirSync(OUT, { recursive: true })

// —— dev server ——
async function preflight(url) {
  try {
    const res = await fetch(url, { signal: AbortSignal.timeout(4000) })
    if (!res.ok) return `HTTP ${res.status}`
    const html = await res.text()
    if (!html.includes('id="root"')) return '响应里没有 #root（不是 vite dev 页面？）'
    return null
  } catch (e) {
    return e?.cause?.code || e?.message || String(e)
  }
}

let server = null
async function ensureServer() {
  if (!(await preflight(BASE))) return
  if (NO_SPAWN) {
    console.error(`❌ dev server 不可用：${BASE}`)
    console.error('   先起：cd desktop/app && npx vite --port 5220 --strictPort')
    process.exit(2)
  }
  const appDir = fileURLToPath(new URL('..', import.meta.url))
  // 直接跑 node_modules/.bin/vite：经 npx 会多一层 npm exec 进程，SIGTERM 只杀到壳，
  // 真 vite 继续占着端口（下次 --strictPort 起不来）。
  server = spawn(join(appDir, 'node_modules', '.bin', 'vite'), ['--port', String(PORT), '--strictPort'], {
    cwd: appDir, stdio: 'ignore',
  })
  for (let i = 0; i < 120; i++) {
    await new Promise((r) => setTimeout(r, 500))
    if (!(await preflight(BASE))) return
  }
  console.error(`❌ 自起 dev server 超时（${BASE}）`)
  process.exit(2)
}

await ensureServer()
// 异常/中断路径也要带走自起的 dev server（否则下次 --strictPort 起不来）。
const stopServer = () => { try { server?.kill('SIGTERM') } catch { /* 已退出 */ } }
process.on('exit', stopServer)
process.on('SIGINT', () => { stopServer(); process.exit(130) })

const browser = await chromium.launch()
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))

// 打开页面：冷启动的 vite 可能先返回 504（依赖重优化），重试几次直到 React 挂载。
async function openApp() {
  for (let i = 0; i < 4; i++) {
    await page.goto(`${BASE}?hubFixture=dmstream`, { waitUntil: 'domcontentloaded' })
    await page.waitForTimeout(1500)
    if (await page.locator('.hub-side-row').count()) return
    if (!(await page.locator('button[aria-label="智能体"]').count())) continue
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(700)
    if (await page.locator('.hub-side-row').count()) return
  }
  throw new Error('页面没挂载出 Hub 左栏（dev server 未就绪 / 504 Outdated Optimize Dep）')
}

// —— 打开 DM（与其它 hub 脚本同一套：左栏「智能体」页签 → 第一行）——
await openApp()
await page.locator('.hub-side-row').first().waitFor({ timeout: 10000 })
const tab = page.locator('.hub-side-tabs button').filter({ hasText: '智能体' }).first()
if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
await page.waitForTimeout(250)
await page.locator('.hub-side-row').first().click({ timeout: 8000 })
await page.waitForTimeout(800)
const composer = page.locator('.hub-demo .composer textarea').first()
await composer.waitFor({ timeout: 8000 })

// 采样器：流式气泡的数量 / 正文 / 标签 / 是否有「思考中」行。
const sample = () => page.evaluate(() => {
  const rows = Array.from(document.querySelectorAll('.hub-dm .msg-row.agent.streaming'))
  const dm = document.querySelector('.hub-dm')
  return {
    // draft=true 是场景2b 的草稿气泡（每轮一条）；false 是 content_chunk 的 live 兜底。
    // 两条路径同时出现 = 同一段正文画了两遍，正是本脚本要抓的回归。
    // carets：DM 的光标是 CSS 伪元素（.hub-demo .streaming.hub-live .agent-text::after），
    // 只有活的那段该有 —— 多段草稿并存时老段跟着闪就是用户报障的「多个光标」。
    streaming: rows.map((r) => {
      const after = getComputedStyle(r.querySelector('.agent-text') ?? r, '::after')
      const jsCaret = !!r.querySelector('.hub-stream-caret')
      const cssCaret = after.content !== 'none' && after.content !== 'normal'
      return {
        text: (r.querySelector('.hub-stream-text, .markdown-body, .md-body')?.textContent ?? r.textContent ?? '').trim(),
        tag: r.querySelector('.agent-tag')?.textContent?.trim() ?? '',
        draft: r.classList.contains('dm-draft'),
        live: r.classList.contains('hub-live'),
        carets: (jsCaret ? 1 : 0) + (cssCaret ? 1 : 0),
      }
    }),
    systemLines: Array.from(dm?.querySelectorAll('.system-line') ?? []).map((e) => (e.textContent ?? '').trim()),
    agentRows: Array.from(document.querySelectorAll('.hub-dm .msg-row.agent:not(.streaming)'))
      .map((r) => (r.textContent ?? '').replace(/\s+/g, ' ').trim()),
  }
})

await composer.fill('跑一下价格对比')
await composer.press('Enter')

// —— 采样直到流式结束（草稿清空且两条 final 落地）——
const seen = []        // 草稿气泡里见过的正文（= 每轮一条）
const seenLive = []    // live 兜底气泡里见过的正文（有 delta 时不该出现）
let maxPerAgentDrafts = 0    // 同一时刻的草稿峰值（多段并存是正常的）
let maxPerAgentCarets = 0    // 同一时刻画出来的光标峰值（必须 ≤ 1）
let maxPerAgentTags = 0      // 同一时刻「正在输入」标签峰值（必须 ≤ 1）
let doubleCaret = null       // 一条气泡上叠了两根光标
let caretsOnStale = null     // 老段（没有「正在输入」标签）还在闪光标
let tagOnLiveMissing = null  // 活的（有光标）却没有「正在输入」标签
let dupWhileStreaming = null
let tagMissing = null
let thinkingWhileDraft = null
let shotR1 = false
let shotR2 = false
const deadline = Date.now() + 20000
let done = false
while (Date.now() < deadline) {
  const s = await sample()
  const draftsNow = s.streaming.filter((x) => x.draft)
  maxPerAgentDrafts = Math.max(maxPerAgentDrafts, draftsNow.length)
  maxPerAgentCarets = Math.max(maxPerAgentCarets, s.streaming.filter((x) => x.carets > 0).length)
  maxPerAgentTags = Math.max(maxPerAgentTags, s.streaming.filter((x) => /正在输入|Typing/.test(x.tag)).length)
  for (const d of s.streaming) {
    const live = /正在输入|Typing/.test(d.tag)
    if (d.carets > 1) doubleCaret = doubleCaret ?? d.text.slice(0, 24)
    if (!live && d.carets > 0) caretsOnStale = caretsOnStale ?? d.text.slice(0, 24)
    if (live && d.carets === 0) tagOnLiveMissing = tagOnLiveMissing ?? d.text.slice(0, 24)
  }
  const liveRows = s.streaming.filter((x) => !x.draft)
  if (liveRows.length > 0 && draftsNow.length > 0) {
    // live 兜底与草稿同时渲染 = 同一段正文画两遍（本脚本要防的回归）。
    // 多段草稿并存本身是正常的（final 等 AgentEnd），不算重复。
    dupWhileStreaming = s.streaming.map((x) => `${x.draft ? 'draft' : 'live'}:${x.text.slice(0, 24)}`)
  }
  for (const d of s.streaming) {
    const bucket = d.draft ? seen : seenLive
    if (!bucket.includes(d.text)) bucket.push(d.text)
    // 老段（已收口、还没等到 final）不再挂标签，这是对的；只有挂着但文案不对才算异常。
    if (d.tag && !/正在输入|Typing/.test(d.tag)) tagMissing = d.tag
  }
  if (s.streaming.length > 0 && s.systemLines.some((x) => /正在思考|Thinking/.test(x))) {
    thinkingWhileDraft = s.systemLines.join(' | ')
  }
  if (s.streaming.length > 0 && s.streaming[0].text.includes(ROUND1) && !shotR1) {
    shotR1 = true
    await page.screenshot({ path: join(OUT, 'dm-stream-round1.png') })
  }
  if (s.streaming.length > 0 && s.streaming[0].text.includes(ROUND2) && !shotR2) {
    shotR2 = true
    await page.screenshot({ path: join(OUT, 'dm-stream-round2.png') })
  }
  const settled = s.streaming.length === 0
    && s.agentRows.filter((t) => t.includes(ROUND1) || t.includes(ROUND2)).length >= 2
  if (settled) {
    // 再确认一次（避免刚 final 一帧就判完成）。
    await page.waitForTimeout(400)
    const again = await sample()
    if (again.streaming.length === 0) { done = true; break }
  }
  await page.waitForTimeout(50)
}

check(done, 'DM 两轮流式在 20s 内结束（草稿清零 + 两条消息落地）')
// 夹具照真 bridge 的 final 时机（AgentEnd 一起到）：两段草稿必须真的并存过，
// 否则下面的光标断言等于没测。
check(maxPerAgentDrafts >= 2, `流式期间两段草稿并存（峰值 ${maxPerAgentDrafts}）`)
check(maxPerAgentCarets <= 1, `同一时刻只有 1 根光标（峰值 ${maxPerAgentCarets}）`)
check(doubleCaret === null, `一条气泡上不叠两根光标（got=${doubleCaret ?? '无'}）`)
check(caretsOnStale === null, `已收口的老段不再闪光标（got=${caretsOnStale ?? '无'}）`)
check(tagOnLiveMissing === null, `在流的那段一定有光标（got=${tagOnLiveMissing ?? '无'}）`)
check(maxPerAgentTags <= 1, `同一时刻只有 1 个「正在输入」标签（峰值 ${maxPerAgentTags}）`)
check(dupWhileStreaming === null, `live 与草稿不同时渲染（同时出现：${JSON.stringify(dupWhileStreaming)}）`)
check(seen.length >= 2, `两轮各自成一条草稿气泡（观测到 ${seen.length} 种正文：${JSON.stringify(seen.map((t) => t.slice(0, 12)))}）`)
// 关键分叉：草稿路径活着时 live 兜底一条都不该出现。把 applyHubDelta 的 DM 分支去掉
// （回退成「只靠 content_chunk 的一条 live」）这条就会挂 —— 否则本脚本对旧实现也「通过」。
check(seenLive.length === 0, `有 delta 时不走 live 兜底气泡（观测到：${JSON.stringify(seenLive.map((t) => t.slice(0, 12)))}）`)
check(seen.some((t) => t.includes(ROUND1)), '第一轮正文在草稿里出现过')
check(seen.some((t) => t.includes(ROUND2)), '第二轮正文在草稿里出现过')
check(!seen.some((t) => t.includes(ROUND1) && t.includes(ROUND2)), '两轮正文不拼进同一条气泡')
check(tagMissing === null, `草稿上的标签文案是 hub.streaming（异常标签：${tagMissing ?? '无'}）`)
check(thinkingWhileDraft === null, `草稿在时不额外显示「思考中」（got=${thinkingWhileDraft ?? '无'}）`)

// —— final 之后：不重复 ——
const final = await sample()
await page.screenshot({ path: join(OUT, 'dm-stream-final.png') })
check(final.streaming.length === 0, `final 后没有残留草稿（got=${final.streaming.length}）`)
const r1Rows = final.agentRows.filter((t) => t.includes(ROUND1))
const r2Rows = final.agentRows.filter((t) => t.includes(ROUND2))
// 注：这段 DM 在 mock 里本来就有一条种子历史（「三家公开价…」），所以只数本轮两段。
check(r1Rows.length === 1, `第一轮正文落成恰好 1 条消息（got=${r1Rows.length}）`)
check(r2Rows.length === 1, `第二轮正文落成恰好 1 条消息（got=${r2Rows.length}）`)
check(r1Rows.length + r2Rows.length === 2, `两轮各一条，没有重复气泡（got=${r1Rows.length + r2Rows.length}）`)
check(!final.agentRows.some((t) => t.includes(ROUND1) && t.includes(ROUND2)), 'final 消息没有把两轮拼成一条')
check(errors.length === 0, `无 console error / pageerror：${errors.slice(0, 3).join(' | ')}`)

await browser.close()
stopServer()

if (failed.length) {
  console.error(`\n❌ DM 多气泡流式：${failed.length} 项不达标`)
  for (const f of failed) console.error(`   · ${f}`)
  process.exit(1)
}
console.log('✅ DM 多气泡流式：全部通过（截图见 ' + OUT + '）')
