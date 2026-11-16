#!/usr/bin/env node
// 场景2b 浏览器验收（群侧）：一轮里多条气泡、两轮各一条草稿，正文不拼接、不重复。
//
// 与 check-hub-dm-stream.mjs 同一口径，但**不做 DM 那套 scope 过滤**：群 delta 带
// group_id/round_id，草稿进 groupDrafts 后按 group.id 过滤渲染；群视图本来就不画
// content_chunk 的 live —— 本脚本要证明的是「虽然 bridge 同时发两条路径，群里也只见
// 每条消息一次」。
//
// 用法（默认自起 dev server；5225 端口，不占 5173/5180）：
//   node scripts/check-hub-group-stream.mjs
//   node scripts/check-hub-group-stream.mjs --url http://localhost:5180/   # 复用已起的
//
// mock 夹具：?hubFixture=groupstream（transport/mock.ts 的 mockGroupStreamReply）——
// 两轮回复，每轮 content_chunk 与 hub_message_delta 并发，**两轮 final 在 AgentEnd 一起到**
//（与真 bridge 的 hubDeltaSink 同形：LLMEnd 只收口、AgentEnd 才落账）；
// message_id 每轮不同，正文带唯一标记 GROUP-R1-A/B、GROUP-R2-A/B。
//
// 判据（任一失败 → exit 1）：
//   · 流式期间同一个智能体**多条草稿并存是正常的**（真 bridge 的 final 要等 AgentEnd，
//     每个 LLM 轮次一段草稿），但**同一时刻只有一根光标 + 一个「正在输入」标签**；
//   · final 之后草稿清零；
//   · 最终恰好两条群气泡，每轮标记在每个气泡里各出现一次（不拼接、不重复）；
//   · content_chunk 没有额外画出一条气泡（流式期间同一条正文不会同时出现在草稿与终态行）；
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
const PORT = Number(arg('port', process.env.HUB_GROUP_STREAM_PORT || '5225'))
const URL_ARG = arg('url', '')
const BASE = URL_ARG || `http://localhost:${PORT}/`
const OUT = arg('out', '/tmp/hub-group-stream')
const NO_SPAWN = argv.includes('--no-spawn')
// 每轮的 A 标记 = 正文开头，B 标记 = 正文结尾：两段正文「有没有被拼在一起」只看一行。
const R1 = 'GROUP-R1-A'
const R2 = 'GROUP-R2-A'

const failed = []
const errors = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }

mkdirSync(OUT, { recursive: true })

// —— dev server（与 check-hub-dm-stream.mjs 同一套：自起 / 带走）——
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
    console.error(`   先起：cd desktop/app && npx vite --port ${PORT} --strictPort`)
    process.exit(2)
  }
  const appDir = fileURLToPath(new URL('..', import.meta.url))
  // 直接跑 node_modules/.bin/vite：经 npx 会多一层 npm exec 进程，SIGTERM 只杀到壳。
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
const stopServer = () => { try { server?.kill('SIGTERM') } catch { /* 已退出 */ } }
process.on('exit', stopServer)
process.on('SIGINT', () => { stopServer(); process.exit(130) })

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1180, height: 900 } })
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))

/** 进 Hub → 群组页签 → 第一个群（grp_mock1「竞品研究」）。 */
async function openGroup() {
  for (let i = 0; i < 4; i++) {
    await page.goto(`${BASE}?hubFixture=groupstream`, { waitUntil: 'domcontentloaded' })
    await page.waitForTimeout(1500)
    if (await page.locator('.hub-side-row').count()) break
    // 冷启动的 vite 可能先 504（依赖重优化）：点一次 rail 的「智能体」再试。
    if (!(await page.locator('button[aria-label="智能体"]').count())) continue
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(700)
    if (await page.locator('.hub-side-row').count()) break
  }
  if (!(await page.locator('.hub-side-row').count())) {
    throw new Error('页面没挂载出 Hub 左栏（dev server 未就绪 / 504 Outdated Optimize Dep）')
  }
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(300)
  await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  await page.waitForTimeout(900)
}

/**
 * 采样器：草稿 / 终态 agent 行 / 页内标记计数。
 *
 * 草稿 = `.hub-group-msg.streaming`（StreamingMessage 的类名，与终态消息同一版式）；
 * 终态行 = 没有 streaming 的 `.msg-row.agent`。两者分开数，才看得出「同一段正文
 * 被两条路径各画一次」。
 */
const sample = () => page.evaluate(() => {
  const view = document.querySelector('.hub-group-view')
  const flat = (el) => (el?.textContent ?? '').replace(/\s+/g, ' ').trim()
  const agentOf = (r) => r.getAttribute('data-agent-id') ?? ''
  // 光标有两套来源，都要数：
  //   ① 群草稿的内联 6×14 方块（StreamingMessage 的 .hub-stream-caret）；
  //   ② demo 版伪元素（.hub-demo .streaming.hub-live .agent-text::after，DM 走这条）。
  // 少算一种，「一行两根光标」就会漏过去 —— 真机上两根同时存在过。
  const drafts = Array.from(view?.querySelectorAll('.msg-row.agent.streaming') ?? []).map((r) => {
    const after = getComputedStyle(r.querySelector('.agent-text') ?? r, '::after')
    const jsCaret = !!r.querySelector('.hub-stream-caret')
    const cssCaret = after.content !== 'none' && after.content !== 'normal'
    return {
      agentId: agentOf(r),
      text: (r.querySelector('.hub-stream-text')?.textContent ?? '').trim(),
      tag: r.querySelector('.agent-tag')?.textContent?.trim() ?? '',
      // 光标只属于「此刻还在收 delta」的那一段：多段草稿并存时不许一起闪，也不许叠两根。
      carets: (jsCaret ? 1 : 0) + (cssCaret ? 1 : 0),
    }
  })
  const settled = Array.from(view?.querySelectorAll('.msg-row.agent:not(.streaming)') ?? []).map((r) => ({
    agentId: agentOf(r), text: flat(r),
  }))
  const thread = view?.querySelector('.messages')
  const threadText = (thread?.textContent ?? '').replace(/\s+/g, ' ')
  const count = (needle) => threadText.split(needle).length - 1
  return {
    drafts,
    settled,
    settledText: settled.map((r) => r.text),
    textR1A: count('GROUP-R1-A'), textR1B: count('GROUP-R1-B'),
    textR2A: count('GROUP-R2-A'), textR2B: count('GROUP-R2-B'),
  }
})

try {
  await openGroup()
  const composer = page.locator('.hub-group-view .composer textarea').first()
  await composer.waitFor({ timeout: 8000 })
  await composer.fill('@Researcher 把两轮结论发一下')
  await page.waitForTimeout(120)
  await page.locator('.hub-group-view .composer .send-btn').first().click({ timeout: 5000 })

  // —— 采样到流式结束 ——
  const seenDrafts = []        // 草稿里见过的正文（= 每轮一条）
  const draftsPerAgent = {}    // agentId → 同一时刻的草稿峰值（多段并存是正常的）
  const caretsPerAgent = {}    // agentId → 同一时刻画出来的光标峰值（必须 ≤ 1）
  const tagsPerAgent = {}      // agentId → 同一时刻「正在输入」标签峰值（必须 ≤ 1）
  let doubleCaret = null       // 同一条气泡上叠了两根光标（内联 + 伪元素）
  let caretsOnStale = null     // 老段（没有「正在输入」标签）还在闪光标
  let tagOnLiveMissing = null  // 活的（有光标）却没有「正在输入」标签
  let tagMissing = null
  let dupWhileStreaming = null // 草稿 + 终态行同时出现同一条正文（content_chunk 多画了一条）
  let dupSettled = null        // 终态行里同一条正文出现两次
  let shotR1 = false
  let shotR2 = false
  const deadline = Date.now() + 25000
  let done = false
  while (Date.now() < deadline) {
    const s = await sample()
    // 真口径是**每个智能体**：草稿可以有多段（final 等 AgentEnd），但光标/标签只能是
    // 最后那一段的。多 agent 并行（群 @ 两人）时每人各一根光标才是对的，不能把总数钉成 1。
    const draftCount = {}
    const caretCount = {}
    const tagCount = {}
    for (const d of s.drafts) {
      const live = /正在输入|Typing/.test(d.tag)
      draftCount[d.agentId] = (draftCount[d.agentId] ?? 0) + 1
      if (d.carets > 0) caretCount[d.agentId] = (caretCount[d.agentId] ?? 0) + 1
      if (live) tagCount[d.agentId] = (tagCount[d.agentId] ?? 0) + 1
      // 同一时刻：有标签 ⇔ 有光标，而且都只有一根/一个。
      if (d.carets > 1) doubleCaret = doubleCaret ?? `${d.agentId}: ${d.text.slice(0, 24)}`
      if (!live && d.carets > 0) caretsOnStale = caretsOnStale ?? `${d.agentId}: ${d.text.slice(0, 24)}`
      if (live && d.carets === 0) tagOnLiveMissing = tagOnLiveMissing ?? `${d.agentId}: ${d.text.slice(0, 24)}`
    }
    for (const [id, n] of Object.entries(draftCount)) draftsPerAgent[id] = Math.max(draftsPerAgent[id] ?? 0, n)
    for (const [id, n] of Object.entries(caretCount)) caretsPerAgent[id] = Math.max(caretsPerAgent[id] ?? 0, n)
    for (const [id, n] of Object.entries(tagCount)) tagsPerAgent[id] = Math.max(tagsPerAgent[id] ?? 0, n)
    for (const d of s.drafts) {
      if (!seenDrafts.includes(d.text)) seenDrafts.push(d.text)
      if (d.tag && !/正在输入|Typing/.test(d.tag)) tagMissing = d.tag
      // 同一条正文（同一轮标记、同一个 agent）已经在终态行里 = 这条消息被画了两遍
      // （content_chunk 的 live 与 hub_message_delta 的草稿并存）。
      const marker = d.text.includes(R1) ? R1 : d.text.includes(R2) ? R2 : ''
      if (marker && s.settled.some((t) => t.agentId === d.agentId && t.text.includes(marker))) {
        dupWhileStreaming = dupWhileStreaming ?? [`${marker} 同时出现在草稿与终态行`]
      }
    }
    for (const m of [R1, R2]) {
      const hits = s.settled.filter((t) => t.text.includes(m)).length
      if (hits > 1) dupSettled = `${m} 在终态里出现 ${hits} 次`
    }
    if (!shotR1 && s.drafts.some((d) => /R1-/.test(d.text))) {
      shotR1 = true
      await page.screenshot({ path: join(OUT, 'round1-draft.png') })
    }
    if (!shotR2 && s.drafts.some((d) => /R2-/.test(d.text))) {
      shotR2 = true
      await page.screenshot({ path: join(OUT, 'round2-draft.png') })
    }
    const settledRows = s.settled.filter((t) => t.text.includes(R1) || t.text.includes(R2)).length
    if (s.drafts.length === 0 && settledRows >= 2) {
      // 再确认一帧（避免刚 final 就判完成）。
      await page.waitForTimeout(400)
      const again = await sample()
      if (again.drafts.length === 0) { done = true; break }
    }
    await page.waitForTimeout(50)
  }

  check(done, '群两轮流式在 25s 内结束（草稿清零 + 两条消息落地）')
  // 夹具照真 bridge 的 final 时机（AgentEnd 一起到），所以两段草稿必须真的并存过 ——
  // 否则下面的光标断言等于没测。
  check(Object.values(draftsPerAgent).some((n) => n >= 2),
    `流式期间两段草稿并存（每 agent 峰值 ${JSON.stringify(draftsPerAgent)}）`)
  check(Object.values(caretsPerAgent).every((n) => n <= 1),
    `同一智能体同时只有 1 根光标（峰值 ${JSON.stringify(caretsPerAgent)}）`)
  check(doubleCaret === null, `一条气泡上不叠两根光标（got=${doubleCaret ?? '无'}）`)
  check(caretsOnStale === null, `已收口的老段不再闪光标（got=${caretsOnStale ?? '无'}）`)
  check(tagOnLiveMissing === null, `在流的那段一定有光标（got=${tagOnLiveMissing ?? '无'}）`)
  check(Object.values(tagsPerAgent).every((n) => n <= 1),
    `同一智能体同时只有 1 个「正在输入」标签（峰值 ${JSON.stringify(tagsPerAgent)}）`)
  check(Object.keys(draftsPerAgent).length === 1 && Object.keys(draftsPerAgent)[0] !== '',
    `草稿气泡带 data-agent-id（got=${JSON.stringify(Object.keys(draftsPerAgent))}）`)
  check(dupWhileStreaming === null, `同一条正文不同时出现在草稿与终态行（got=${JSON.stringify(dupWhileStreaming)}）`)
  check(dupSettled === null, `终态行里没有重复正文（got=${dupSettled ?? '无'}）`)
  check(seenDrafts.length >= 2, `两轮各自流过一条草稿（观测到 ${seenDrafts.length} 种正文）`)
  check(seenDrafts.some((t) => t.includes(R1)), '第一轮正文在草稿里出现过')
  check(seenDrafts.some((t) => t.includes(R2)), '第二轮正文在草稿里出现过')
  check(!seenDrafts.some((t) => t.includes(R1) && t.includes(R2)), '两轮正文不拼进同一条草稿')
  check(tagMissing === null, `草稿上的标签文案是 hub.streaming（异常标签：${tagMissing ?? '无'}）`)

  // —— final：恰好两条群气泡，每段正文只出现一次、不拼接 ——
  const final = await sample()
  await page.screenshot({ path: join(OUT, 'final.png') })
  const r1Rows = final.settledText.filter((t) => t.includes(R1))
  const r2Rows = final.settledText.filter((t) => t.includes(R2))
  check(final.drafts.length === 0, `final 后草稿清零（got=${final.drafts.length}）`)
  check(r1Rows.length === 1, `第一轮落成恰好 1 条群气泡（got=${r1Rows.length}）`)
  check(r2Rows.length === 1, `第二轮落成恰好 1 条群气泡（got=${r2Rows.length}）`)
  check(r1Rows.length + r2Rows.length === 2, `最终恰好两条群气泡（got=${r1Rows.length + r2Rows.length}）`)
  check(!final.settledText.some((t) => t.includes(R1) && t.includes(R2)), '没有一条气泡把两轮拼在一起')
  // 每段正文（含结尾标记）在整个消息区里只出现一次 —— 拼接/重复都会让计数 > 1。
  check(final.textR1A === 1 && final.textR1B === 1, `第一轮正文只出现一次（A=${final.textR1A} B=${final.textR1B}）`)
  check(final.textR2A === 1 && final.textR2B === 1, `第二轮正文只出现一次（A=${final.textR2A} B=${final.textR2B}）`)
  check(r1Rows[0]?.indexOf('GROUP-R1-A') < r1Rows[0]?.indexOf('GROUP-R1-B'), '第一轮正文顺序没乱（A 在 B 前）')
  check(r2Rows[0]?.indexOf('GROUP-R2-A') < r2Rows[0]?.indexOf('GROUP-R2-B'), '第二轮正文顺序没乱（A 在 B 前）')

  // 窄屏也不出横向滚动（群的正文最长，容易把气泡撑宽）。
  await page.setViewportSize({ width: 390, height: 844 })
  await page.waitForTimeout(250)
  const narrow = await page.evaluate(() => ({
    docW: document.documentElement.scrollWidth,
    messagesH: document.querySelector('.hub-group-view .messages')?.getBoundingClientRect().height ?? -1,
  }))
  check(narrow.docW <= 390, `390 无横向溢出（doc=${narrow.docW}）`)
  check(narrow.messagesH > 0, `390 下群消息区仍有高度（got=${narrow.messagesH}）`)
  await page.screenshot({ path: join(OUT, 'final-390.png') })

  check(errors.length === 0, `无 console error / pageerror：${errors.slice(0, 3).join(' | ')}`)
} finally {
  await page.close().catch(() => {})
  await browser.close().catch(() => {})
  stopServer()
}

if (failed.length) {
  console.error(`\n❌ 群侧多气泡流式：${failed.length} 项不达标`)
  for (const f of failed) console.error(`   · ${f}`)
  process.exit(1)
}
console.log('✅ 群侧多气泡流式：单光标 / 两轮不拼接 / final 两条 / 不重复 / 390 全部通过')
console.log(`   截图：${OUT}/round1-draft.png ${OUT}/round2-draft.png ${OUT}/final.png ${OUT}/final-390.png`)
