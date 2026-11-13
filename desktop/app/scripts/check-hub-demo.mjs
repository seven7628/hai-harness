#!/usr/bin/env node
// 场景演示群（grp_demo）浏览器验收：默认 mock 页就能看到本轮六项能力。
//
// 用法（默认自起 dev server；5227 端口，不占 5173/5180）：
//   node scripts/check-hub-demo.mjs
//   node scripts/check-hub-demo.mjs --url http://localhost:5180/   # 复用已起的
//   node scripts/check-hub-demo.mjs --no-spawn --port 5227
//
// 判据（任一失败 → exit 1）：
//   1. 左栏出现「场景演示 · 六项能力」，且它**不是**第一行（竞品研究仍是第一个群）；
//   2. 打开后：审批卡 + 提问卡（内联 need，带 workspace / s_demo_project / Researcher-sub）；
//   3. ToDo 卡：`ToDo · 2/3`，归属「子智能体 Researcher-sub」；
//   4. 人类消息同时带 1 张图 + 1 个文件 chip（合并发送）；
//   5. 任务卡（workspace 目标）+ 紧跟的「收到 Project 结果」总结；
//   6. 长 Markdown 渲染（表格 + 清单）且有 ```html 围栏的 visual-ui 块；
//   7. 发 @Researcher 继续 → 两轮独立气泡（GROUP-R1-A / GROUP-R2-A 各一条，不拼接）；
//   8. DM 隔离：Researcher 的 DM 有 DM-ONLY-RE 且没有 DM-ONLY-WR / 群内那句；
//   9. console error / pageerror 0。
import { chromium } from 'playwright'
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const PORT = Number(arg('port', process.env.HUB_DEMO_PORT || '5227'))
const URL_ARG = arg('url', '')
const BASE = URL_ARG || `http://localhost:${PORT}/`
const NO_SPAWN = argv.includes('--no-spawn')

const failed = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }

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

const errors = []
const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))

async function openHub() {
  for (let i = 0; i < 4; i++) {
    await page.goto(BASE, { waitUntil: 'domcontentloaded' })
    await page.waitForTimeout(1500)
    if (await page.locator('.hub-side-row').count()) return
    if (!(await page.locator('button[aria-label="智能体"]').count())) continue
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(700)
    if (await page.locator('.hub-side-row').count()) return
  }
  throw new Error('页面没挂载出 Hub 左栏（dev server 未就绪 / 504 Outdated Optimize Dep）')
}

async function openGroupTab() {
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(300)
}

await openHub()
await openGroupTab()

// —— 1) 左栏：新群存在，且竞品研究仍是第一个（既有脚本靠「第一行」选群） ——
const rows = page.locator('.hub-side-row')
const names = await rows.allInnerTexts()
const demoIndex = names.findIndex((t) => t.includes('场景演示'))
check(demoIndex >= 0, `左栏出现「场景演示 · 六项能力」（got=${JSON.stringify(names.map((n) => n.split('\n')[0]))}）`)
check(demoIndex > 0, `场景演示不是第一个群（got index=${demoIndex}）`)
check(names[0]?.includes('竞品研究'), `竞品研究仍是第一个群（got=${names[0]?.split('\n')[0]}）`)
check(names[demoIndex]?.includes('六项能力'), `群名带六项能力（got=${names[demoIndex]}）`)

// —— 2) 打开场景演示群 ——
await rows.nth(demoIndex).click({ timeout: 8000 })
await page.waitForTimeout(1200)
await page.locator('.hub-demo .composer textarea').first().waitFor({ timeout: 8000 })

const groupText = (await page.locator('.hub-group-view').innerText()).replace(/\s+/g, ' ')
check(groupText.includes('这句话只在本群，不要带去私聊'), '群内出现隔离哨兵人类消息')
check(groupText.includes('场景演示 · ①隔离'), '导览 system 消息可见')
check(groupText.includes('同一批：/compact + 正文 + 文件 + 图片'), '合并发送的正文可见')
check(groupText.includes('收到 Project 结果，我来总结'), '任务卡之后紧跟执行者总结（结果回推）')
check(groupText.includes('DM-ONLY') === false, '群聊里不出现 DM-ONLY 标记（私聊内容不进群）')

// 内联 need：审批 + 提问（waitFor 不能裸 await：超时只会抛 TimeoutError 栈，
// 失败清单里看不到「哪张卡没出现」，一次跑完也就只报一个错）。
const approval = page.locator('.hub-inline-need--approval').first()
const approvalShown = await approval.waitFor({ timeout: 8000 }).then(() => true).catch(() => false)
check(approvalShown, '审批（approval）内联卡出现')
const approvalText = approvalShown ? (await approval.innerText()).replace(/\s+/g, ' ') : ''
check(approvalText.includes('workspace'), `审批卡显示 Project workspace（got=${approvalText.slice(0, 120)}）`)
check(approvalText.includes('s_demo_project'), '审批卡显示 Project session')
check(approvalText.includes('Researcher-sub'), '审批卡显示子智能体名')
check(await page.locator('.hub-inline-need--question').count() >= 1, '提问（Ask）内联卡也在')

// 内联 ToDo：2/3 + 子智能体 Researcher-sub
const todo = page.locator('.hub-inline-todo').first()
const todoShown = await todo.waitFor({ timeout: 8000 }).then(() => true).catch(() => false)
check(todoShown, 'ToDo 内联卡出现')
const todoText = todoShown ? (await todo.innerText()).replace(/\s+/g, ' ') : ''
check(todoText.includes('ToDo · 2/3'), `待办卡计数 2/3（got=${todoText.slice(0, 80)}）`)
check(todoText.includes('子智能体 Researcher-sub'), `待办卡归属子智能体（got=${todoText.slice(0, 80)}）`)

// 附件：1 张图 + 1 个文件 chip
check(await page.locator('.hub-group-view .hub-msg-img img').count() >= 1, '人类消息里有图片附件')
check(await page.locator('.hub-group-view .hub-msg-file').count() >= 1, '人类消息里有文件引用 chip')
const fileTitle = await page.locator('.hub-group-view .hub-msg-file').first().getAttribute('title')
check(String(fileTitle ?? '').includes('/mock/workspace/pricing-v3.md'), `文件 chip 指向 /mock/workspace/pricing-v3.md（got=${fileTitle}）`)

// 任务卡：workspace 目标
const taskRow = page.locator('.hub-group-view .task-row').first()
const taskShown = await taskRow.waitFor({ timeout: 8000 }).then(() => true).catch(() => false)
check(taskShown, '任务结果卡出现')
const taskText = taskShown ? (await taskRow.innerText()).replace(/\s+/g, ' ') : ''
check(taskText.includes('/mock/workspace'), `任务卡显示 workspace（got=${taskText.slice(0, 140)}）`)
check(taskText.includes('s_demo_project'), '任务卡显示 Project session')
check(await taskRow.locator('.hub-task-open').count() === 1, '任务卡有「打开会话」入口')

// 回推总结的上下文行：TurnContextLine 在 senders <2 时整行 return null，所以这条同时
// 钉住「senders 补足 2 人」和「指向任务卡 #6」两件事。
const ctxJump = page.locator('.hub-group-view .hub-context-jump').first()
const ctxShown = await ctxJump.waitFor({ timeout: 6000 }).then(() => true).catch(() => false)
check(ctxShown, '回推总结带上下文行（senders ≥ 2）')
const ctxTitle = ctxShown ? await ctxJump.getAttribute('title') : ''
check(String(ctxTitle ?? '').includes('#6'), `上下文行指向任务卡 #6（got=${ctxTitle}）`)

// 长 Markdown + visual-ui（```html 围栏 → .htmlblock iframe）
check(await page.locator('.hub-group-view .md table').count() >= 1, '长消息里的表格渲染出来了')
check(await page.locator('.hub-group-view .md ul li').count() >= 5, '长消息里的清单渲染出来了')
const htmlblock = page.locator('.hub-group-view .htmlblock').first()
const htmlShown = await htmlblock.waitFor({ timeout: 8000 }).then(() => true).catch(() => false)
check(htmlShown, '```html 围栏渲染成 visual-ui 预览块')
const frameBox = htmlShown ? await htmlblock.locator('iframe.htmlblock-frame').boundingBox() : null
check(!!frameBox && frameBox.height > 20, `visual-ui 预览块有高度（got=${frameBox?.height}）`)

await page.screenshot({ path: '/tmp/hub-demo-group.png', fullPage: false })

// —— 3) 多气泡：发 @ 消息 → 两轮流式气泡 ——
const input = page.locator('.hub-demo .composer textarea').first()
await input.click()
await input.fill('@Researcher 继续')
await input.press('Enter')

const deadline = Date.now() + 20_000
let done = false
let maxConcurrent = 0
const seenDrafts = []
while (Date.now() < deadline) {
  const s = await page.evaluate(() => {
    const view = document.querySelector('.hub-group-view')
    const drafts = Array.from(view?.querySelectorAll('.msg-row.agent.streaming') ?? [])
      .map((r) => (r.querySelector('.hub-stream-text')?.textContent ?? '').trim())
    const settled = Array.from(view?.querySelectorAll('.msg-row.agent:not(.streaming)') ?? [])
      .map((r) => (r.textContent ?? '').replace(/\s+/g, ' ').trim())
    return { drafts, settled }
  })
  maxConcurrent = Math.max(maxConcurrent, s.drafts.length)
  for (const d of s.drafts) if (d && !seenDrafts.includes(d)) seenDrafts.push(d)
  if (s.drafts.length === 0
    && s.settled.filter((t) => t.includes('GROUP-R1-A')).length >= 1
    && s.settled.filter((t) => t.includes('GROUP-R2-A')).length >= 1) {
    await page.waitForTimeout(400)
    const again = await page.evaluate(() => document.querySelectorAll('.hub-group-view .msg-row.agent.streaming').length)
    if (again === 0) { done = true; break }
  }
  await page.waitForTimeout(50)
}
check(done, '发 @ 消息后两轮回复在 20s 内落地')
check(maxConcurrent <= 1, `流式期间同时最多 1 条草稿气泡（峰值 ${maxConcurrent}）`)
check(seenDrafts.some((t) => t.includes('GROUP-R1-A')), '第一轮草稿出现过')
check(seenDrafts.some((t) => t.includes('GROUP-R2-A')), '第二轮草稿出现过')
const finalRows = await page.evaluate(() => Array.from(
  document.querySelectorAll('.hub-group-view .msg-row.agent:not(.streaming)'),
).map((r) => (r.textContent ?? '').replace(/\s+/g, ' ').trim()))
check(finalRows.filter((t) => t.includes('GROUP-R1-A')).length === 1, '第一轮落成恰好 1 条气泡')
check(finalRows.filter((t) => t.includes('GROUP-R2-A')).length === 1, '第二轮落成恰好 1 条气泡')
check(!finalRows.some((t) => t.includes('GROUP-R1-A') && t.includes('GROUP-R2-A')), '两轮没有被拼进同一条气泡')
await page.screenshot({ path: '/tmp/hub-demo-stream.png' })

// —— 4) DM 隔离：Researcher 的私聊只有 DM-ONLY-RE ——
// 不能按「第一行」选：DM 列表按 last_at 倒序，mock 侧顺序是数据决定的（见 hubLog 的
// dmOnlyAt）。这里按名字选行，顺序怎么排都能落到 Researcher。
const agentTab = page.locator('.hub-side-tabs button').filter({ hasText: '智能体' }).first()
if ((await agentTab.getAttribute('aria-selected')) !== 'true') await agentTab.click({ timeout: 6000 })
await page.waitForTimeout(300)
const researcherRow = page.locator('.hub-side-row').filter({ hasText: 'Researcher' }).first()
const rowShown = await researcherRow.waitFor({ timeout: 8000 }).then(() => true).catch(() => false)
check(rowShown, '侧栏能找到 Researcher 的 DM 行（按名字选，不依赖第一行）')
if (rowShown) await researcherRow.click({ timeout: 8000 })
await page.waitForTimeout(900)
const dmText = (await page.locator('.hub-demo').first().innerText()).replace(/\s+/g, ' ')
check(dmText.includes('DM-ONLY-RE'), 'Researcher 的 DM 里有 DM-ONLY-RE')
check(!dmText.includes('DM-ONLY-WR'), 'Researcher 的 DM 里没有 Writer 的 DM-ONLY-WR')
check(!dmText.includes('这句话只在本群'), '群内那句没有漏进 DM')
check(dmText.includes('三家公开价'), '原 DM 种子（三家公开价）仍在')
await page.screenshot({ path: '/tmp/hub-demo-dm.png' })

check(errors.length === 0, `无 console error / pageerror：${errors.slice(0, 3).join(' | ')}`)

await browser.close()
stopServer()

if (failed.length) {
  console.error(`\n❌ 场景演示群：${failed.length} 项不达标`)
  for (const f of failed) console.error(`   · ${f}`)
  process.exit(1)
}
console.log('✅ 场景演示群：全部通过（截图 /tmp/hub-demo-group.png、hub-demo-stream.png、hub-demo-dm.png）')
