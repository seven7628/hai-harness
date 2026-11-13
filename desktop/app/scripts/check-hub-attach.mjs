#!/usr/bin/env node
// Hub 图片 / 文件附件的真浏览器回归（P4-UI-13）。
//
// 前置：默认自起 dev server（5226 端口，不占 5173/5180）；也可复用已起的：
//   node scripts/check-hub-attach.mjs [--url http://localhost:5180/] [--out /tmp/hub-attach-shots]
//   node scripts/check-hub-attach.mjs --no-spawn        # 只复用，不起新 server
//
// 断言：
//   · DM：setInputFiles 注入一张 8×8 PNG → composer 上方出现缩略图 chip → 发送 →
//     用户气泡里出现 <img>（data URL）→ 附件条带清空
//   · 灯箱：点缩略图打开 → 「复制图片」不抛 pageerror 且给出可读状态 → Esc 关闭 →
//     再开 → 点遮罩关闭
//   · 历史：?hubFixture=attach 的 hub_history 消息带图片 + 文件 chip（title=绝对路径），
//     点文件 chip 复制路径不抛；历史消息的灯箱没有「删除」
//   · 群：同样的注入 → 群用户气泡里出现 <img>（纯图片、正文为空也能发）
//   · 390×844：DM / 群会话带附件时 documentElement.scrollWidth <= 390（0 横向溢出）
//   · 扇形叠牌：data-n / --i / --mid、单图 128×88（390 下 108×74）、hover 的牌 z-index=200、
//     每张牌右上角的 × 都是最上层命中元素（叠放顺序不遮挡）、第 2/3 张的 × 能移除自己、
//     700px + 10 张不横向溢出且抽出的牌不被裁
//   · 全程无 console error / pageerror
//
// 不造假数据：图片是真实 PNG（Buffer 注入），附件回显来自 mock 的 hub_message，
// 历史来自 mock 的 hub_history（?hubFixture=attach）。
//
// 390 的取法：窄窗下左栏是图标条、Hub 侧栏不渲染（单栏布局），没法再点列表进会话，
// 所以先在宽窗打开会话，再把视口缩到 390 —— 会话保持挂载，量的正是带附件的
// 消息与 composer 条带是否撑破文档。
import { chromium } from 'playwright'
import { mkdirSync } from 'node:fs'
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const APP_DIR = fileURLToPath(new URL('..', import.meta.url))

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
// 注意：别把常量命名成 URL —— 会遮住全局 URL 构造函数（history fixture 要用）。
const URL_ARG = arg('url', process.env.HUB_ATTACH_URL || '')
const PORT = Number(arg('port', process.env.HUB_ATTACH_PORT || '5226'))
const BASE_URL = URL_ARG || `http://localhost:${PORT}/`
const NO_SPAWN = argv.includes('--no-spawn')
const OUT = arg('out', '/tmp/hub-attach-shots')
const FIXTURE_URL = new URL(BASE_URL)
FIXTURE_URL.searchParams.set('hubFixture', 'attach')

// —— dev server：默认自起 / 退出时带走（与 check-hub-inline-todos.mjs 同一套）——
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
if (await preflight(BASE_URL)) {
  if (NO_SPAWN) {
    console.error(`❌ dev server 不可用：${BASE_URL}`)
    console.error(`   先起：cd desktop/app && npx vite --port ${PORT} --strictPort`)
    process.exit(2)
  }
  // 直接跑 node_modules/.bin/vite：经 npx 会多一层 npm exec 进程，SIGTERM 只杀到壳。
  server = spawn(join(APP_DIR, 'node_modules', '.bin', 'vite'), ['--port', String(PORT), '--strictPort'], {
    cwd: APP_DIR, stdio: 'ignore',
  })
  let ready = false
  for (let i = 0; i < 120; i++) {
    await new Promise((r) => setTimeout(r, 500))
    if (!(await preflight(BASE_URL))) { ready = true; break }
  }
  if (!ready) {
    console.error(`❌ 自起 dev server 超时（${BASE_URL}）`)
    process.exit(2)
  }
}
const stopServer = () => { try { server?.kill('SIGTERM') } catch { /* 已退出 */ } }
process.on('exit', stopServer)
process.on('SIGINT', () => { stopServer(); process.exit(130) })
process.on('SIGTERM', () => { stopServer(); process.exit(143) })

// 8×8 PNG（橙底 + 深色对角）：真实可解码的图片，和 mock fixture 是同一张。
const PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAYAAADED76LAAAALklEQVR42mPQ0DH5f7MqDSsGyTHAGNgkQTQDugA6mwFdF7ppxCvAawVeRxLyJgDMMZrx26+tKgAAAABJRU5ErkJggg==',
  'base64',
)
const filePayload = { name: 'hub-shot.png', mimeType: 'image/png', buffer: PNG }

const failed = []
const errors = []
let assertions = 0
const check = (cond, msg) => { assertions++; if (!cond) failed.push(msg) }

mkdirSync(OUT, { recursive: true })

const browser = await chromium.launch()
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
// 复制按钮走 ClipboardItem：给权限，让「复制图片」真的能写进去（没权限的失败路径也断言不抛）。
await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: new URL(BASE_URL).origin }).catch(() => {})
const page = await context.newPage()
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))

async function openHub(url = BASE_URL) {
  await page.goto(url, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1400)
  if (!(await page.locator('.hub-side-row').count())) {
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(700)
  }
  await page.locator('.hub-side-row').first().waitFor({ timeout: 10000 })
}

async function openDM(url = BASE_URL) {
  await openHub(url)
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '智能体' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(200)
  await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  await page.waitForTimeout(700)
  await page.locator('.hub-demo .composer textarea').first().waitFor({ timeout: 8000 })
}

async function openGroup(url = BASE_URL) {
  await openHub(url)
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(250)
  await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  await page.waitForTimeout(900)
  await page.locator('.hub-demo .composer textarea').first().waitFor({ timeout: 8000 })
}

async function sendWithImage(testid, text) {
  await page.setInputFiles(`[data-testid="${testid}"]`, filePayload)
  // 压缩是异步的：缩略图出现才说明 data URL 已经进 state。
  await page.locator('.hub-attach-strip .hub-attach-chip--img img').first().waitFor({ timeout: 10000 })
  check(await page.locator('.hub-attach-strip .hub-attach-chip--img img').count() === 1,
    `${testid}: 附件条带应有 1 张缩略图`)
  if (text) await page.locator('.hub-demo .composer textarea').first().fill(text)
  const send = page.locator('.hub-demo .composer .send-btn').first()
  check(await send.isEnabled(), `${testid}: 有图时发送按钮可用`)
  await send.click()
  return page.locator('.hub-demo .msg-row.user .hub-msg-img img').first()
}

// —— 1) DM：注入 → 缩略图 → 发送 → 用户气泡里的图片 ——
{
  await openDM()

  // 纸夹 → pickFsFiles（mock 返回 2 个路径）→ 文件引用 chip（名字 + 大小 + ×）。
  await page.locator('.hub-demo .composer [aria-label="引用文件"]').first().click()
  await page.locator('.hub-attach-strip .hub-attach-chip--file').first().waitFor({ timeout: 6000 })
  check(await page.locator('.hub-attach-strip .hub-attach-chip--file').count() === 2, '纸夹一次引用 2 个文件（pickFsFiles 多选）')
  const chipName = (await page.locator('.hub-attach-chip--file .hub-attach-file-name').first().textContent()) ?? ''
  check(chipName.trim().length > 0, `文件 chip 显示文件名（${chipName.trim()}）`)
  const chipSize = (await page.locator('.hub-attach-chip--file .hub-attach-file-size').first().textContent()) ?? ''
  check(/\d/.test(chipSize), `文件 chip 显示体积（${chipSize.trim()}）`)
  await page.screenshot({ path: `${OUT}/dm-strip-files.png` })
  // 移除两个文件 chip（× 只在条带里出现），回到「只发图片」的用例。
  for (let i = 0; i < 2; i++) await page.locator('.hub-attach-strip .hub-attach-chip--file .hub-attach-x').first().click()
  await page.waitForTimeout(200)
  check(await page.locator('.hub-attach-chip--file').count() === 0, '文件 chip 可以被移除')

  // 拖拽：图片走压缩，普通文件走 getPathForFile（mock 给 /mock/<name>）—— 都不能静默丢弃。
  await page.evaluate((b64) => {
    const bin = atob(b64)
    const bytes = new Uint8Array(bin.length)
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
    const dt = new DataTransfer()
    dt.items.add(new File([bytes], 'drop-shot.png', { type: 'image/png' }))
    dt.items.add(new File([new Uint8Array([1, 2, 3])], 'notes.txt', { type: 'text/plain' }))
    const ta = document.querySelector('.hub-demo .composer textarea')
    ta.dispatchEvent(new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: dt }))
  }, PNG.toString('base64'))
  await page.locator('.hub-attach-strip .hub-attach-chip--img img').first().waitFor({ timeout: 10000 })
  check(await page.locator('.hub-attach-chip--img').count() === 1, '拖拽图片 → 附件条带出现 1 张缩略图')
  const dropped = page.locator('.hub-attach-strip .hub-attach-chip--file').first()
  await dropped.waitFor({ timeout: 6000 })
  check(String(await dropped.getAttribute('title')).includes('/mock/notes.txt'),
    '拖拽普通文件 → 变成 @路径 引用（getPathForFile），不是被丢掉')

  // 粘贴：DataTransfer 里只有一张图 → 走 addImageFiles（纯图片无文本，preventDefault）。
  await page.evaluate((b64) => {
    const bin = atob(b64)
    const bytes = new Uint8Array(bin.length)
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
    const dt = new DataTransfer()
    dt.items.add(new File([bytes], 'paste-shot.png', { type: 'image/png' }))
    const ta = document.querySelector('.hub-demo .composer textarea')
    ta.dispatchEvent(new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: dt }))
  }, PNG.toString('base64'))
  await page.waitForFunction(() => document.querySelectorAll('.hub-attach-chip--img').length === 2, null, { timeout: 10000 })
  check(await page.locator('.hub-attach-chip--img').count() === 2, '粘贴图片 → 又多一张缩略图（不静默丢弃）')
  await page.screenshot({ path: `${OUT}/dm-strip-drop-paste.png` })
  // 清空这三条（2 张图 + 1 个文件），回到「只发一张图」的用例。
  // 图片是扇形叠牌：hover 会把牌抽正（× 随牌一起移动），所以先 hover 再点 ×，
  // 与真人的操作路径一致；否则点击点会落在牌自己的图上。
  for (let i = await page.locator('.hub-attach-chip--img').count(); i > 0; i--) {
    const card = page.locator('.hub-attach-strip .hub-attach-chip--img').first()
    await card.hover()
    await card.locator('.hub-attach-x').click()
  }
  await page.locator('.hub-attach-strip .hub-attach-chip--file .hub-attach-x').first().click()
  await page.waitForTimeout(200)
  check(await page.locator('.hub-attach-chip').count() === 0, '拖拽/粘贴来的附件都能移除')

  await page.setInputFiles('[data-testid="hub-image-input"]', filePayload)
  await page.locator('.hub-attach-strip .hub-attach-chip--img img').first().waitFor({ timeout: 10000 })
  await page.screenshot({ path: `${OUT}/dm-strip-image.png` })
  // 缩略图 → 待发灯箱（有「删除」）→ 删除后条带清空 → 再注入一次（走正常发送流程）。
  // 扇形牌 hover 会抽正/上浮；先 hover 再点，避免 Playwright 在「移动鼠标→元素位移」
  // 之间反复判定 not stable（真实用户也是先看到牌再点）。
  const stripThumb = page.locator('.hub-attach-strip .hub-attach-thumb').first()
  await stripThumb.hover()
  await stripThumb.click()
  await page.locator('.hub-attach-lb').waitFor({ timeout: 6000 })
  check(await page.locator('.hub-attach-lb-btn.danger').count() === 1, '待发灯箱里有「删除」')
  await page.locator('.hub-attach-lb-btn.danger').click()
  await page.locator('.hub-attach-lb').waitFor({ state: 'detached', timeout: 4000 })
  check(await page.locator('.hub-attach-chip--img').count() === 0, '灯箱里删除后条带清空')

  const bubble = await sendWithImage('hub-image-input', '看这张图')
  await bubble.waitFor({ timeout: 10000 })
  const src = await bubble.getAttribute('src')
  check(String(src).startsWith('data:image/'), 'DM 用户气泡里的 <img> 是 data URL（图片走 attachments）')
  check(await page.locator('.hub-attach-strip .hub-attach-chip--img').count() === 0, '发送成功后附件条带清空（成功才清）')
  await page.screenshot({ path: `${OUT}/dm-sent.png` })

  // 灯箱：点缩略图 → 复制（不抛）→ Esc 关闭 → 再开 → 点遮罩关闭
  const thumb = page.locator('.hub-demo .msg-row.user .hub-msg-img').first()
  await thumb.hover()
  await thumb.click()
  await page.locator('.hub-attach-lb').waitFor({ timeout: 6000 })
  await page.screenshot({ path: `${OUT}/dm-lightbox.png` })
  const errorsBeforeCopy = errors.length
  await page.locator('.hub-attach-lb-btn', { hasText: '复制图片' }).click()
  await page.waitForTimeout(600)
  const status = await page.locator('.hub-attach-lb-status').first().textContent().catch(() => '')
  check(Boolean(status && status.trim()), '灯箱复制后给出可读状态（成功或失败，不 alert）')
  check(errors.length === errorsBeforeCopy, `复制图片不应抛错：${errors.slice(0, 3).join(' | ')}`)
  await page.keyboard.press('Escape')
  await page.locator('.hub-attach-lb').waitFor({ state: 'detached', timeout: 4000 })
  await thumb.hover()
  await thumb.click()
  await page.locator('.hub-attach-lb').waitFor({ timeout: 6000 })
  await page.mouse.click(4, 4)
  await page.locator('.hub-attach-lb').waitFor({ state: 'detached', timeout: 4000 })
  check(true, '灯箱：Esc 与遮罩都能关闭')
}

// —— 2) 历史 / 刷新：?hubFixture=attach 的 hub_history 里有附件 ——
{
  await openDM(FIXTURE_URL.toString())
  await page.locator('.hub-demo .msg-row.user .hub-msg-img img').first().waitFor({ timeout: 8000 })
  check(true, '刷新后历史消息里的图片仍在（hub_history 回显 attachments）')
  const fileChip = page.locator('.hub-demo .msg-row.user .hub-msg-file').first()
  check(await fileChip.count() === 1, '历史消息里的文件引用 chip 存在')
  const title = await fileChip.getAttribute('title')
  check(String(title).includes('/mock/workspace/pricing-v3.md'), '文件 chip 的 title 是绝对路径（不假预览）')
  const errorsBeforeFile = errors.length
  await fileChip.click()
  await page.waitForTimeout(300)
  check(errors.length === errorsBeforeFile, `点击文件 chip（复制路径）不应报错：${errors.slice(0, 3).join(' | ')}`)
  // 图片灯箱里的「删除」只在待发条带里出现；历史消息的灯箱不该有删除按钮。
  const historyImg = page.locator('.hub-demo .msg-row.user .hub-msg-img').first()
  await historyImg.hover()
  await historyImg.click()
  await page.locator('.hub-attach-lb').waitFor({ timeout: 6000 })
  check(await page.locator('.hub-attach-lb-btn.danger').count() === 0, '历史消息的灯箱没有「删除」（不删别人的消息）')
  await page.keyboard.press('Escape')
  await page.locator('.hub-attach-lb').waitFor({ state: 'detached', timeout: 4000 })
  await page.screenshot({ path: `${OUT}/dm-history.png` })
}

// —— 3) 群：纯图片（正文为空）也能发 ——
{
  await openGroup()
  const bubble = await sendWithImage('hub-group-image-input', '')
  await bubble.waitFor({ timeout: 10000 })
  const src = await bubble.getAttribute('src')
  check(String(src).startsWith('data:image/'), '群用户气泡里出现图片（正文为空也发得出去）')
  check(await page.locator('.hub-attach-strip .hub-attach-chip--img').count() === 0, '群发送成功后附件条带清空')
  await page.screenshot({ path: `${OUT}/group-sent.png` })
}

// —— 3a) 两批各 4 张：已有图片不能被重复扣一次（张数上限 10，8 张必须全进）——
{
  await openDM()
  const batch = (n) => Array.from({ length: n }, (_, i) => ({ ...filePayload, name: `batch-${i + 1}.png` }))
  await page.setInputFiles('[data-testid="hub-image-input"]', batch(4))
  await page.waitForTimeout(900)
  check(await page.locator('.hub-attach-strip .hub-attach-chip--img').count() === 4, '第一批 4 张进入条带')
  await page.setInputFiles('[data-testid="hub-image-input"]', batch(4))
  await page.waitForTimeout(1200)
  check(await page.locator('.hub-attach-strip .hub-attach-chip--img').count() === 8, '第二批 4 张也进入条带（current 不被重复扣）')
}

// —— 3b) 6 张图：气泡只铺 4 张，+2 打开第 5 张，方向键/按钮能到第 6 张 ——
{
  await openDM()
  const six = Array.from({ length: 6 }, (_, i) => ({ ...filePayload, name: `hub-shot-${i + 1}.png` }))
  await page.setInputFiles('[data-testid="hub-image-input"]', six)
  await page.waitForTimeout(700)
  check(await page.locator('.hub-attach-strip .hub-attach-chip--img').count() === 6, '6 张图都进了待发条带')
  await page.locator('.hub-demo .composer textarea').first().fill('六张图')
  await page.locator('.hub-demo .composer .send-btn').first().click()
  const row = page.locator('.hub-demo .msg-row.user').last()
  await row.waitFor({ timeout: 10000 })
  await row.locator('.hub-msg-img').first().waitFor({ timeout: 8000 })
  check(await row.locator('.hub-msg-img').count() === 4, '气泡只铺 4 张缩略图（不把整屏撑爆）')
  const more = row.locator('.hub-msg-img-more')
  check(await more.count() === 1 && (await more.textContent())?.trim() === '+2', '+2 是可点的「还有 2 张」入口')
  // 打标记：关闭后要断言焦点真的回到这颗触发按钮（不是 body，也不是背景里的发送框）。
  await more.evaluate((el) => el.setAttribute('data-focus-trigger', '1'))
  await more.click()
  await page.locator('.hub-attach-lb').waitFor({ timeout: 6000 })
  let cap = await page.locator('.hub-attach-lb figcaption').textContent()
  check(String(cap).includes('5/6'), `+2 打开的是第 5 张（got=${cap}）`)
  await page.keyboard.press('ArrowRight')
  await page.waitForTimeout(150)
  cap = await page.locator('.hub-attach-lb figcaption').textContent()
  check(String(cap).includes('6/6'), `→ 能到第 6 张（got=${cap}）`)
  await page.locator('.hub-attach-lb-btn.nav[aria-label="上一张"]').click()
  await page.waitForTimeout(150)
  cap = await page.locator('.hub-attach-lb figcaption').textContent()
  check(String(cap).includes('5/6'), `‹ 能回到第 5 张（got=${cap}）`)

  // 灯箱的 Tab 陷阱：aria-modal 拦不住浏览器的 Tab，焦点必须自己圈在对话框里。
  const focusInfo = () => page.evaluate(() => {
    const el = document.activeElement
    const lb = document.querySelector('.hub-attach-lb')
    const label = el
      ? (el.getAttribute('aria-label') || (el.textContent ?? '').trim().slice(0, 10) || el.tagName)
      : ''
    return { tag: el ? el.tagName : '', label, inside: !!(el && lb && lb.contains(el)) }
  })
  // 可聚焦控件：上一张 / 下一张 / 复制图片 / 关闭（历史消息的灯箱没有「删除」）。
  const total = await page.locator('.hub-attach-lb button:not([disabled])').count()
  check(total >= 3, `灯箱里有 ${total} 个可聚焦按钮（Tab 循环的落点）`)
  const start = await focusInfo()
  check(start.inside, `打开灯箱后焦点在灯箱内（got=${start.tag} ${start.label}）`)
  // Tab 次数 > 按钮数：必须自己绕回来，不能停在背景的发送框/侧栏上。
  const seen = []
  for (let i = 0; i < total + 2; i++) {
    await page.keyboard.press('Tab')
    const info = await focusInfo()
    check(info.inside, `第 ${i + 1} 次 Tab 仍停在 .hub-attach-lb 内（got=${info.tag} ${info.label}）`)
    seen.push(info.label)
  }
  check(seen.includes(start.label), `Tab 循环回到起点（${seen.join(' → ')}）`)
  for (let i = 0; i < total + 2; i++) {
    await page.keyboard.press('Shift+Tab')
    const info = await focusInfo()
    check(info.inside, `第 ${i + 1} 次 Shift+Tab 仍停在 .hub-attach-lb 内（got=${info.tag} ${info.label}）`)
  }
  // 方向键导航不受焦点陷阱影响（焦点停在按钮上也要能翻页）。
  const capBefore = await page.locator('.hub-attach-lb figcaption').textContent()
  await page.keyboard.press('ArrowRight')
  await page.waitForTimeout(150)
  const capAfter = await page.locator('.hub-attach-lb figcaption').textContent()
  check(String(capBefore) !== String(capAfter), `Tab 之后方向键仍能翻页（${capBefore} → ${capAfter}）`)

  await page.keyboard.press('Escape')
  await page.locator('.hub-attach-lb').waitFor({ state: 'detached', timeout: 4000 })
  // Esc 之后焦点回到「+2」那颗触发按钮（不是 body，也不是背景里的输入框）。
  const back = await page.evaluate(() => document.activeElement?.getAttribute('data-focus-trigger') === '1')
  check(back, '关闭灯箱后焦点回到触发元素（+N 缩略图按钮）')
  await page.screenshot({ path: `${OUT}/dm-lightbox-focus.png` })
}

// —— 3c) 扇形叠牌：几何 / 命中区 / 单图尺寸 / 窄列不溢出 ——
{
  await openDM()
  const cards = page.locator('.hub-attach-strip .hub-attach-chip--img')
  const three = Array.from({ length: 3 }, (_, i) => ({ ...filePayload, name: `fan-${i + 1}.png` }))
  await page.setInputFiles('[data-testid="hub-image-input"]', three)
  await page.waitForTimeout(900)
  await page.locator('.hub-attach-strip').scrollIntoViewIfNeeded()
  check(await page.locator('.hub-attach-imgs').getAttribute('data-n') === '3', '三张图 → 扇形容器 data-n=3')
  const geom = await page.evaluate(() => {
    const el = document.querySelector('.hub-attach-imgs')
    const card = el.querySelector('.hub-attach-chip--img')
    return {
      i: card.style.getPropertyValue('--i').trim(),
      mid: card.style.getPropertyValue('--mid').trim(),
      lastMid: el.querySelectorAll('.hub-attach-chip--img')[2].style.getPropertyValue('--mid').trim(),
    }
  })
  check(geom.i === '0' && geom.mid === '1' && geom.lastMid === '1',
    `扇形几何走 --i/--mid 行内变量（got i=${geom.i} mid=${geom.mid}/${geom.lastMid}）`)
  // 每张牌右上角的 × 必须是命中的最上层元素：`z-index: 100 - i` 的叠放顺序就是为这个。
  const hits = await page.evaluate(() => Array.from(document.querySelectorAll('.hub-attach-strip .hub-attach-chip--img')).map((card) => {
    const x = card.querySelector('.hub-attach-x')
    const r = x.getBoundingClientRect()
    const top = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2)
    return top === x
  }))
  check(hits.length === 3 && hits.every(Boolean), `每张牌的 × 都在最上层（命中：${hits.join(',')}）`)
  // hover 的牌抽到整叠之上：z-index 必须是 200（否则会被右边那张牌盖住一半）。
  await cards.nth(1).hover()
  await page.waitForTimeout(300)
  const zHover = await cards.nth(1).evaluate((el) => getComputedStyle(el).zIndex)
  check(zHover === '200', `hover 的牌 z-index=200（抽出来压住整叠，got=${zHover}）`)
  // 第 2 张自己的 ×：hover 后点它自己（不是 .first()），条带少一张。
  await cards.nth(1).locator('.hub-attach-x').click()
  await page.waitForTimeout(400)
  check(await cards.count() === 2, '第 2 张的 × 能移除自己（3 → 2）')
  // 原来的第 3 张现在在第 2 位：它的 × 同样能移除（不是只有第一张可点）。
  await cards.nth(1).hover()
  await cards.nth(1).locator('.hub-attach-x').click()
  await page.waitForTimeout(400)
  check(await cards.count() === 1, '第 3 张的 × 也能移除自己（2 → 1）')

  // 一张图不是一叠牌：128×88 大缩略图（删掉 data-n='1' 规则就退回 64×45）。
  const size = await cards.first().evaluate((el) => `${el.offsetWidth}x${el.offsetHeight}`)
  check(size === '128x88', `单图缩略图 128x88（got=${size}）`)
  await page.setViewportSize({ width: 390, height: 844 })
  await page.waitForTimeout(900)
  const sizeNarrow = await cards.first().evaluate((el) => `${el.offsetWidth}x${el.offsetHeight}`)
  check(sizeNarrow === '108x74', `390 下单图缩略图 108x74（got=${sizeNarrow}）`)

  // 窄列（641–900）10 张牌：条带内部横向滚动，文档 0 横向溢出，抽出的牌不被裁。
  await page.setViewportSize({ width: 700, height: 900 })
  await page.waitForTimeout(500)
  const ten = Array.from({ length: 10 }, (_, i) => ({ ...filePayload, name: `wide-${i + 1}.png` }))
  await page.setInputFiles('[data-testid="hub-image-input"]', ten)
  await page.waitForTimeout(1200)
  check(await cards.count() === 10, '700px 下 10 张图都进条带')
  const doc700 = await page.evaluate(() => document.documentElement.scrollWidth)
  check(doc700 <= 700, `700px + 10 张：文档 0 横向溢出（scrollWidth=${doc700}）`)
  const stripBox = await page.evaluate(() => {
    const el = document.querySelector('.hub-attach-imgs')
    const composer = document.querySelector('.hub-demo .composer')
    return {
      // 内容比列宽时必须是「内部滚动」而不是「溢出到列外」：
      // overflow-x 仍是 visible 的话，最后几张牌会画到 composer 右边（页面不一定滚，但已经越界）。
      overflowX: getComputedStyle(el).overflowX,
      overflows: el.scrollWidth > el.clientWidth,
      right: Math.round(el.getBoundingClientRect().right),
      composerRight: Math.round(composer.getBoundingClientRect().right),
    }
  })
  check(stripBox.overflowX === 'auto' && stripBox.overflows,
    `700px + 10 张：牌比列宽时走条带内部滚动（overflowX=${stripBox.overflowX}，${stripBox.overflows ? '内容溢出' : '内容放得下'}）`)
  check(stripBox.right <= stripBox.composerRight + 1,
    `700px 下条带右缘不越界（${stripBox.right} <= ${stripBox.composerRight}）`)
  // 抽出来的牌不能被 overflow 裁掉：hover 第一张，牌顶要落在容器的可视区内。
  await cards.first().hover()
  await page.waitForTimeout(300)
  const lift = await page.evaluate(() => {
    const el = document.querySelector('.hub-attach-imgs')
    const card = el.querySelector('.hub-attach-chip--img')
    return { cardTop: Math.round(card.getBoundingClientRect().top), clipTop: Math.round(el.getBoundingClientRect().top) }
  })
  check(lift.cardTop >= lift.clipTop, `700px 下抽出的牌不被裁（牌顶 ${lift.cardTop} >= 容器顶 ${lift.clipTop}）`)
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.waitForTimeout(500)
}

// —— 4) 失败 → 重试：附件跟着失败走，重试成功后条带才清 ——
{
  await openDM()
  await page.setInputFiles('[data-testid="hub-image-input"]', filePayload)
  await page.locator('.hub-attach-strip .hub-attach-chip--img img').first().waitFor({ timeout: 10000 })
  // __lostack__ = mock 注入「已落账但响应丢失」：前端表现为失败，重试命中 cid 幂等。
  await page.locator('.hub-demo .composer textarea').first().fill('__lostack__')
  await page.locator('.hub-demo .composer .send-btn').first().click()
  const failedRow = page.locator('.hub-demo .msg-row.user.failed').first()
  await failedRow.waitFor({ timeout: 10000 })
  check(await failedRow.locator('.hub-msg-img img').count() === 1, '失败气泡仍带图片（不静默丢图）')
  check(await page.locator('.hub-attach-strip .hub-attach-chip--img').count() === 1, '失败后 composer 保留图片（供重试）')
  await page.screenshot({ path: `${OUT}/dm-failed.png` })

  await failedRow.locator('.user-retry').click()
  await page.locator('.hub-demo .msg-row.user.failed').first().waitFor({ state: 'detached', timeout: 10000 })
  await page.waitForTimeout(500)
  check(await page.locator('.hub-attach-strip .hub-attach-chip--img').count() === 0, '重试成功后条带清空（不会重复发图）')
  // 注意别用正文定位：Markdown 会把 __lostack__ 渲染成 <strong>。
  check(await page.locator('.hub-demo .msg-row.user .hub-msg-img img').count() === 1, '重试成功后的气泡仍显示图片')
  await page.screenshot({ path: `${OUT}/dm-retried.png` })
}

// —— 5) 390×844：有附件时 0 横向溢出 ——
{
  await openDM(FIXTURE_URL.toString())
  await page.locator('.hub-demo .msg-row.user .hub-msg-img img').first().waitFor({ timeout: 8000 })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.waitForTimeout(900)
  const width = await page.evaluate(() => document.documentElement.scrollWidth)
  check(width <= 390, `390 下 DM（有附件）文档横向溢出：scrollWidth=${width}`)
  const attach = await page.evaluate(() => {
    const el = document.querySelector('.hub-msg-attach')
    return el ? Math.round(el.getBoundingClientRect().right) : null
  })
  check(attach != null && attach <= 390, `390 下 DM 附件块右缘越界：${attach}`)
  const composer = await page.evaluate(() => {
    const el = document.querySelector('.hub-demo .composer')
    return el ? Math.round(el.getBoundingClientRect().right) : -1
  })
  check(composer > 0 && composer <= 390, `390 下 composer 右缘越界：${composer}`)
  await page.screenshot({ path: `${OUT}/dm-390.png` })

  // 群：回宽窗切到群会话，再缩回 390。
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.waitForTimeout(400)
  await openGroup(FIXTURE_URL.toString())
  await page.locator('.hub-demo .msg-row.user .hub-msg-img img').first().waitFor({ timeout: 8000 })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.waitForTimeout(900)
  const gwidth = await page.evaluate(() => document.documentElement.scrollWidth)
  check(gwidth <= 390, `390 下群（有附件）文档横向溢出：scrollWidth=${gwidth}`)
  const gattach = await page.evaluate(() => {
    const el = document.querySelector('.hub-msg-attach')
    return el ? Math.round(el.getBoundingClientRect().right) : null
  })
  check(gattach != null && gattach <= 390, `390 下群附件块右缘越界：${gattach}`)
  await page.screenshot({ path: `${OUT}/group-390.png` })
  await page.setViewportSize({ width: 1280, height: 900 })
}

check(errors.length === 0, `存在 console error / pageerror：${errors.slice(0, 4).join(' | ')}`)

await browser.close()

if (errors.length > 0) {
  console.error('\nconsole/page errors:')
  for (const e of errors.slice(0, 10)) console.error(`  · ${e}`)
}
if (failed.length > 0) {
  console.error('\n失败：')
  for (const f of failed) console.error(`  ✗ ${f}`)
  console.error(`\n截图在 ${OUT}`)
  process.exit(1)
}
console.log(`✅ Hub 附件浏览器回归通过（${assertions} 项断言：DM/群 注入·发送·回显·灯箱·Tab 焦点陷阱·历史·390·扇形叠牌命中区）`)
console.log(`   截图：${OUT}/dm-strip-files.png, dm-strip-drop-paste.png, dm-strip-image.png, dm-sent.png, dm-lightbox.png, dm-history.png, group-sent.png, dm-failed.png, dm-retried.png, dm-390.png, group-390.png`)
