#!/usr/bin/env node
// 群组编辑弹层（P6-17 / P7-01）：右栏入口 + 编辑模式 UI + 保存/移除/转让 + Esc。
//
// P7-01/I3：保存走**一条** hub_group_edit（name/mode/owner + 成员增删原子提交）。
// 第 3 段专门走「加人 + 转让群主」同一次保存 —— 那是旧实现最容易留下半改状态的路径。
//
// 前置（**不要用 5173/5180**）：
//   VITE_PORT=5222 npx vite
// 运行：
//   node scripts/check-hub-group-edit.mjs [--url http://localhost:5222/]
//
// 断言：
//   · 右栏 inspector 的「编辑群组」能打开弹层，且字段是当前群的值（名称/模式/成员）
//   · 群主那一行：勾选框锁定 + 「群主」chip（不可移除）
//   · 移除一个普通成员 → 保存 → 弹层关闭、右栏成员数减少（服务端状态刷新）
//   · 群名 + 协作模式一起保存 → 头部副标题显示新模式
//   · 加人 + 转让群主同一次保存 → 弹层关闭、右栏成员 +1、群主变成新人
//   · Esc 关闭弹层
//   · 640 宽：弹层不溢出视口
import { chromium } from 'playwright'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const BASE_URL = arg('url', process.env.HUB_GROUP_EDIT_URL || 'http://localhost:5222/')

const failed = []
const errors = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }
const log = (m) => console.log(`  ✅ ${m}`)

const browser = await chromium.launch()
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))

async function openGroup() {
  await page.goto(BASE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1400)
  if (!(await page.locator('.hub-demo').count())) {
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(700)
  }
  await page.locator('.hub-side-row').first().waitFor({ timeout: 10000 })
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(250)
  await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  await page.waitForTimeout(700)
  await page.locator('.hub-demo .chat-header').first().waitFor({ timeout: 8000 })
}

const modal = () => page.locator('.hub-group-modal').first()

async function openEditor() {
  // 右栏默认是折叠的 44px 图标条：先点「概览」把它展开，才能看到「编辑群组」。
  if (await page.locator('.right.collapsed.hub-detail-rail').count()) {
    await page.locator('.right.collapsed.hub-detail-rail button[aria-label="概览"]').first().click({ timeout: 6000 })
    await page.waitForTimeout(300)
  }
  // 右栏概览里的「编辑群组」（inspector 常驻，概览页签）。
  const btn = page.locator('.hub-ov-actions button', { hasText: '编辑群组' }).first()
  await btn.waitFor({ timeout: 8000 })
  await btn.click({ timeout: 6000 })
  await modal().waitFor({ timeout: 6000 })
  await page.waitForTimeout(200)
}

// —— 1) 打开：字段来自当前群 ——
await openGroup()
const headerSubBefore = await page.locator('.hub-demo .chat-sub').first().innerText().catch(() => '')
await openEditor()
{
  const title = await modal().locator('.hub-modal-title').innerText()
  check(title.includes('编辑群组'), `标题是编辑模式（got=${title}）`)
  const name = await modal().locator('input[type="text"], input:not([type])').first().inputValue()
  check(name.trim().length > 0, `群名已带入（got=${name}）`)
  const memberRows = await modal().locator('.hub-group-member').count()
  check(memberRows >= 2, `成员行渲染（${memberRows} 行）`)
  const ownerRow = modal().locator('.hub-group-member').filter({ hasText: '群主' }).first()
  check((await ownerRow.count()) === 1, '群主行有「群主」chip')
  const ownerDisabled = await ownerRow.locator('input[type="checkbox"]').isDisabled()
  check(ownerDisabled, '群主行的勾选框锁定（不可移除）')
  const removeBtns = await modal().locator('.hub-group-member-remove').count()
  check(removeBtns === memberRows - 1, `非群主成员都有移除按钮（${removeBtns}/${memberRows}）`)
  log(`编辑弹层：${memberRows} 名成员，群主锁定`)
}

// —— 2) 移除一个普通成员 + 改名 + 改模式 → 保存 ——
{
  const beforeCount = await page.locator('.hub-ov-member').count()
  await modal().locator('.hub-group-member-remove').first().click({ timeout: 4000 })
  await page.waitForTimeout(120)
  const afterRemove = await modal().locator('.hub-group-member.on').count()
  check(afterRemove === beforeCount - 1, `点移除后勾选数减少（${beforeCount} → ${afterRemove}）`)

  const nameInput = modal().locator('input[type="text"], input:not([type])').first()
  await nameInput.fill('竞品研究（浏览器编辑）')
  await modal().locator('select').last().selectOption('readonly')
  await modal().locator('.hub-modal-foot .hub-primary').click({ timeout: 4000 })
  await page.waitForTimeout(900)
  check((await page.locator('.hub-group-modal').count()) === 0, '保存后弹层关闭')

  const afterCount = await page.locator('.hub-ov-member').count()
  check(afterCount === beforeCount - 1, `右栏成员数已刷新（${beforeCount} → ${afterCount}）`)
  const name = await page.locator('.hub-demo .chat-title .name').first().innerText()
  check(name.includes('浏览器编辑'), `群名已刷新（got=${name}）`)
  const sub = await page.locator('.hub-demo .chat-sub').first().innerText()
  check(sub.includes('readonly'), `头部副标题显示新模式（before=${headerSubBefore} / after=${sub}）`)
  log(`保存：成员 ${beforeCount}→${afterCount}，名称/模式已刷新`)
}

// —— 3) 加人 + 转让群主：一次保存（一条 hub_group_edit，原子路径） ——
{
  const beforeCount = await page.locator('.hub-ov-member').count()
  await openEditor()
  const rows = modal().locator('.hub-group-member')
  const total = await rows.count()
  let addRow = null
  for (let i = 0; i < total; i++) {
    const row = rows.nth(i)
    if (!(await row.locator('input[type="checkbox"]').isChecked())) { addRow = row; break }
  }
  check(!!addRow, '有可添加的成员（roster 里还有没入群的人）')
  const newName = addRow ? (await addRow.locator('.hub-group-member-name').innerText()).trim() : ''
  if (addRow) await addRow.locator('input[type="checkbox"]').click({ timeout: 4000 })
  const ownerSelect = modal().locator('select').first()
  const opt = ownerSelect.locator('option').filter({ hasText: newName }).first()
  const value = await opt.getAttribute('value').catch(() => null)
  check(!!value, `群主下拉里有刚加入的成员（got=${value}）`)
  if (value) await ownerSelect.selectOption(value)
  await modal().locator('.hub-modal-foot .hub-primary').click({ timeout: 4000 })
  await page.waitForTimeout(900)
  check((await page.locator('.hub-group-modal').count()) === 0, '加人 + 转让保存后弹层关闭')
  const afterCount = await page.locator('.hub-ov-member').count()
  check(afterCount === beforeCount + 1, `右栏成员数 +1（${beforeCount} → ${afterCount}）`)
  const ownerName = (await page.locator('.hub-session-info dd').nth(1).innerText()).trim()
  check(ownerName === newName, `群主已转让（got=${ownerName} want=${newName}）`)
  log(`原子编辑：加人 + 转让（${newName} 成为群主）`)
}

// —— 4) Esc 关闭 ——
{
  await openEditor()
  await page.keyboard.press('Escape')
  await page.waitForTimeout(250)
  check((await page.locator('.hub-group-modal').count()) === 0, 'Esc 关闭编辑弹层')
}

// —— 5) 640 宽：弹层不溢出 ——
{
  await page.setViewportSize({ width: 640, height: 760 })
  await page.waitForTimeout(300)
  await openEditor()
  const box = await modal().boundingBox()
  const vp = page.viewportSize()
  check(!!box && box.width <= vp.width && box.x >= 0, `640：弹层宽度不溢出（w=${box?.width} vp=${vp?.width}）`)
  check(!!box && box.y >= 0 && box.y + box.height <= vp.height + 1, `640：弹层高度不溢出（y=${box?.y} h=${box?.height}）`)
  await page.keyboard.press('Escape')
  log('640：弹层不溢出')
}

await browser.close()

if (errors.length) failed.push(`console/page errors: ${errors.slice(0, 3).join(' | ')}`)
if (failed.length) {
  console.error(`\n❌ HUB_GROUP_EDIT_CHECK_FAIL (${failed.length})`)
  for (const f of failed) console.error(`   - ${f}`)
  process.exit(1)
}
console.log('\nHUB_GROUP_EDIT_CHECK_PASS')
