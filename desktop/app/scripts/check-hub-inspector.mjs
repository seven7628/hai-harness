#!/usr/bin/env node
// Hub inspector 宽度回归：默认收起 + 展开只占小部分（用户报「智能体模式对话时右侧占比太大，
// 明明应该默认收起来，而且展开应该只占小部分」）。
//
// 前置（默认复用 5180，不占 5173；另开终端起 dev server）：
//   cd desktop/app && npx vite --port 5180
// 运行：
//   node scripts/check-hub-inspector.mjs
//   node scripts/check-hub-inspector.mjs --url http://localhost:5197/
//   node scripts/check-hub-inspector.mjs --no-shot        # 不写截图（默认落 /tmp）
//
// 判据（任一失败 → 打印实测宽高并 exit 1；dev server 没起 → exit 2）：
//   1280×900：进 Hub 选第一个智能体 →
//             .right.collapsed 在、.right.expanded 不在、.hub-demo ≥ 700（会话列拿到宽度）；
//             点 .hub-detail-rail 的第一个 .rbtn → 300ms 后 .right.expanded 在、宽 340–420；
//             点 inspector 头部的「收起」按钮 → 回到 collapsed（图标条 44px）；
//   1280×620：同样默认收起（短窗更不该被 inspector 吃掉高度/宽度）；
//   390×844 ：打开 Hub 默认收起（窄窗下 inspector 根本撑不开，只能是图标条）；
//   1280×900 追加：从「任务」图标展开也落在 340–420（不是只有第一个 rbtn 走这条路）；
//                  拖右侧分隔条最多到 420（Hub 期间的 maxSize 上限，1280 下 = 32.8%）；
//                  退出 Hub：右栏恢复进入前的展开态，且 maxSize 回到 80%（能拖过 420）。
import { chromium } from 'playwright'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const URL = arg('url', process.env.HUB_INSPECTOR_URL || 'http://localhost:5180/')
const SHOT = !argv.includes('--no-shot')
// Hub 内展开的目标宽度 360、上限 420（见 App.tsx 的 HUB_INSPECTOR_W / HUB_INSPECTOR_MAX_W）。
const MIN_W = 340
const MAX_W = 420
const CAP = 422 // 库按百分比换算回来的 px 会有小数，留 2px 容差

// —— dev server preflight：没起 → exit 2（与「布局不达标」的 exit 1 区分开）——
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

const down = await preflight(URL)
if (down) {
  console.error(`❌ dev server 不可用：${URL}（${down}）`)
  console.error('   先另开一个终端起 dev server（不要占 5173）：')
  console.error('     cd desktop/app && npx vite --port 5180')
  console.error('   或换端口后重跑：')
  console.error('     node scripts/check-hub-inspector.mjs --url http://localhost:5181/')
  process.exit(2)
}

const failures = []
const report = []
const errors = []
const check = (cond, msg) => { if (!cond) failures.push(msg) }

// 一次量齐：右栏两态的元素与宽度、会话列宽度、窗口尺寸（失败时要打印实测值）。
const measure = (page) => page.evaluate(() => {
  const w = (sel) => {
    const el = document.querySelector(sel)
    return el ? Math.round(el.getBoundingClientRect().width * 10) / 10 : null
  }
  return {
    winW: window.innerWidth,
    winH: window.innerHeight,
    expanded: document.querySelectorAll('.right.expanded').length,
    collapsed: document.querySelectorAll('.right.collapsed').length,
    rail: w('.hub-detail-rail'),
    insp: w('.hub-inspector'),
    demo: w('.hub-demo'),
    left: w('.left'),
  }
})

// 进 Hub 并选中第一个智能体。左栏展开态点第一行（顺带选中）；折叠态（窄窗）点 rail 的
// 「智能体」入口 —— 两条路都是真实入口，跟用户操作一致。
async function openHub(page) {
  await page.goto(URL, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1800)
  if (!(await page.locator('.hub-demo').count())) {
    const row = page.locator('.hub-side-row').first()
    if (await row.count()) {
      await row.click({ timeout: 8000 })
    } else {
      await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 })
    }
    await page.waitForTimeout(900)
  }
  if (!(await page.locator('.hub-demo').count())) throw new Error('Hub 没打开（.hub-demo 没出现）')
}

// 左栏展开态才有 Hub 段与「回到工作区」：折叠时先点展开钮（与 check-hub-left-layout 同款）。
async function ensureLeftOpen(page) {
  if (!(await page.locator('.left.collapsed').count())) return
  await page.locator('button[aria-label="左栏收缩 / ⌘B"]').first().click({ timeout: 5000 }).catch(() => {})
  await page.waitForTimeout(350)
}

const fmt = (m) =>
  `${m.winW}×${m.winH} left=${m.left} demo=${m.demo} rail=${m.rail} insp=${m.insp} ` +
  `collapsed=${m.collapsed} expanded=${m.expanded}`

const browser = await chromium.launch()
try {
  // —— 1) 1280×900：默认收起 → 点 rail 展开 360 → 收起回图标条 ——
  {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
    page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))
    page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
    const tag = '1280×900'
    await openHub(page)
    const a = await measure(page)
    report.push(`${tag} 默认 ${fmt(a)}`)
    check(a.collapsed >= 1, `${tag} 进 Hub 后没有 .right.collapsed（右栏没收起）`)
    check(a.expanded === 0, `${tag} 进 Hub 后 .right.expanded=${a.expanded}（应默认收起）`)
    check(a.demo !== null && a.demo >= 700, `${tag} 会话列 .hub-demo=${a.demo} < 700（宽度没让给会话）`)
    if (SHOT) await page.screenshot({ path: '/tmp/hub-inspector-1280-collapsed.png' }).catch(() => {})

    await page.locator('.hub-detail-rail .rbtn').first().click({ timeout: 8000 })
    await page.waitForTimeout(300)
    const b = await measure(page)
    report.push(`${tag} 展开 ${fmt(b)}`)
    check(b.expanded >= 1, `${tag} 点 rail 图标后 .right.expanded 没出现（展开失败）`)
    check(b.insp !== null && b.insp >= MIN_W && b.insp <= MAX_W, `${tag} 展开宽度 ${b.insp} 不在 ${MIN_W}–${MAX_W}`)
    if (SHOT) await page.screenshot({ path: '/tmp/hub-inspector-1280-expanded.png' }).catch(() => {})

    const close = page.locator('.hub-inspector-head button[aria-label*="收起"], .hub-inspector-head button[title*="收起"]').first()
    check((await close.count()) > 0, `${tag} inspector 头部找不到「收起」按钮`)
    if (await close.count()) {
      await close.click({ timeout: 8000 })
      await page.waitForTimeout(350)
      const c = await measure(page)
      report.push(`${tag} 再收起 ${fmt(c)}`)
      check(c.expanded === 0, `${tag} 点「收起」后 .right.expanded=${c.expanded}（没回到图标条）`)
      check(c.rail !== null && c.rail <= 60, `${tag} 收起后图标条宽度 ${c.rail} > 60`)
    }
    await page.close()
  }

  // —— 2) 1280×620：短窗同样默认收起 ——
  {
    const page = await browser.newPage({ viewport: { width: 1280, height: 620 } })
    page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))
    const tag = '1280×620'
    await openHub(page)
    const m = await measure(page)
    report.push(`${tag} 默认 ${fmt(m)}`)
    check(m.collapsed >= 1, `${tag} 进 Hub 后没有 .right.collapsed`)
    check(m.expanded === 0, `${tag} 进 Hub 后 .right.expanded=${m.expanded}（应默认收起）`)
    if (SHOT) await page.screenshot({ path: '/tmp/hub-inspector-1280x620-collapsed.png' }).catch(() => {})
    await page.close()
  }

  // —— 3) 390×844：窄窗打开 Hub 也默认收起 ——
  {
    const page = await browser.newPage({ viewport: { width: 390, height: 844 } })
    page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))
    const tag = '390×844'
    await openHub(page)
    const m = await measure(page)
    report.push(`${tag} 默认 ${fmt(m)}`)
    check(m.collapsed >= 1, `${tag} 打开 Hub 后没有 .right.collapsed`)
    check(m.expanded === 0, `${tag} 打开 Hub 后 .right.expanded=${m.expanded}（应默认收起）`)
    if (SHOT) await page.screenshot({ path: '/tmp/hub-inspector-390-collapsed.png' }).catch(() => {})
    await page.close()
  }

  // —— 4) 1280×900：其它页签图标也落 360；拖拽被 420 卡住；退出 Hub 恢复 ——
  {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
    page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))
    const tag = '1280×900 追加'
    await page.goto(URL, { waitUntil: 'domcontentloaded' })
    await page.waitForTimeout(1800)
    await ensureLeftOpen(page)
    // 工作区里右栏是展开的 → 进 Hub 必须收起，退出 Hub 必须恢复
    await page.evaluate(() => window.__store?.getState?.()?.setRightExpanded?.(true))
    await page.waitForTimeout(400)
    const pre = await measure(page)
    check(pre.expanded >= 1, `${tag} 进 Hub 前右栏没展开（前置失败，宽度恢复这条测不了）`)

    await page.locator('.hub-side-row').first().click({ timeout: 8000 })
    await page.waitForTimeout(900)
    const inHub = await measure(page)
    report.push(`${tag} 进 Hub ${fmt(inHub)}`)
    check(inHub.expanded === 0, `${tag} 进 Hub 后右栏仍展开（expanded=${inHub.expanded}）`)

    // 「任务」图标（第三个 rbtn）展开：与第一个 rbtn 走同一条 effect，也必须落 360
    await page.locator('.hub-detail-rail .rbtn').nth(2).click({ timeout: 8000 })
    await page.waitForTimeout(300)
    const tabOpen = await measure(page)
    report.push(`${tag} 任务图标 ${fmt(tabOpen)}`)
    check(
      tabOpen.insp !== null && tabOpen.insp >= MIN_W && tabOpen.insp <= MAX_W,
      `${tag} 从任务图标展开宽度 ${tabOpen.insp} 不在 ${MIN_W}–${MAX_W}`,
    )

    // 拖右侧分隔条（第二个 .resize-handle）往左 300px：Hub 期间最多 420
    const sep = page.locator('.resize-handle').last()
    const box = await sep.boundingBox()
    if (!box) {
      failures.push(`${tag} 找不到右侧分隔条（.resize-handle）`)
    } else {
      await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
      await page.mouse.down()
      await page.mouse.move(box.x - 300, box.y + box.height / 2, { steps: 25 })
      await page.mouse.up()
      await page.waitForTimeout(400)
      const dragged = await measure(page)
      report.push(`${tag} 拖到上限 ${fmt(dragged)}`)
      check(
        dragged.insp !== null && dragged.insp > MIN_W && dragged.insp <= CAP,
        `${tag} Hub 内拖宽后 inspector=${dragged.insp}（上限 ${CAP}）`,
      )
    }

    // 退出 Hub：右栏恢复展开态，maxSize 回到 80%（1280 下能拖过 420）
    const back = page.locator('.hub-side-back').first()
    check((await back.count()) > 0, `${tag} 找不到「回到工作区」（.hub-side-back）`)
    if (await back.count()) {
      await back.click({ timeout: 8000 })
      await page.waitForTimeout(900)
      const out = await page.evaluate(() => {
        const el = document.querySelector('.right.expanded')
        return {
          hubOpen: !!document.querySelector('.hub-demo'),
          rightW: el ? Math.round(el.getBoundingClientRect().width * 10) / 10 : null,
          expanded: document.querySelectorAll('.right.expanded').length,
        }
      })
      report.push(`${tag} 退出 Hub rightW=${out.rightW} expanded=${out.expanded} hub=${out.hubOpen}`)
      check(!out.hubOpen, `${tag} 退出后 .hub-demo 还在（Hub 没关）`)
      check(out.expanded >= 1, `${tag} 退出 Hub 后右栏没恢复展开（expanded=${out.expanded}）`)

      // 工作区里再拖一次：不再受 420 限制（这条锁「别把非 Hub 页面的拖拽范围改小」）
      const sep2 = page.locator('.resize-handle').last()
      const box2 = await sep2.boundingBox()
      if (box2) {
        await page.mouse.move(box2.x + box2.width / 2, box2.y + box2.height / 2)
        await page.mouse.down()
        await page.mouse.move(box2.x - 200, box2.y + box2.height / 2, { steps: 25 })
        await page.mouse.up()
        await page.waitForTimeout(400)
        const outDrag = await page.evaluate(() => {
          const el = document.querySelector('.right.expanded')
          return el ? Math.round(el.getBoundingClientRect().width * 10) / 10 : null
        })
        report.push(`${tag} 退出后拖宽 rightW=${outDrag}`)
        check(outDrag !== null && outDrag > CAP, `${tag} 退出 Hub 后右栏拖不过 ${CAP}（maxSize 没恢复：${outDrag}）`)
      }
    }
    if (SHOT) await page.screenshot({ path: '/tmp/hub-inspector-1280-exit.png' }).catch(() => {})
    await page.close()
  }
} catch (e) {
  failures.push(`异常：${e?.message || e}`)
} finally {
  await browser.close()
}

if (errors.length) {
  console.error(`⚠️  控制台错误 ${errors.length} 条（前 3 条）：`)
  for (const e of errors.slice(0, 3)) console.error(`   · ${e}`)
  failures.push(`控制台/页面错误 ${errors.length} 条`)
}

if (failures.length) {
  console.error(`❌ Hub inspector 宽度不符合预期（${URL}）`)
  for (const line of report) console.error(`   实测 ${line}`)
  for (const f of failures) console.error(`   · ${f}`)
  process.exit(1)
}

console.log(`✅ Hub inspector 默认收起、展开 ${MIN_W}–${MAX_W}px、Hub 内上限 ${CAP}（${URL}）`)
for (const line of report) console.log(`   ${line}`)
if (SHOT) console.log('   截图 /tmp/hub-inspector-*.png')
