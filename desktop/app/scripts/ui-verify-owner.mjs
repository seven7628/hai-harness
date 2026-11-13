// 真实 UI 验收（Review 要求）：Hub DM/群 设置页的归属显示与写入、编辑 agent/群路由不丢目标。
//
// 跑法（浏览器真实渲染，mock 数据源；不进 npm test，需要起 dev server）：
//   cd desktop/app && npx vite --port 5617 &
//   node scripts/ui-verify-owner.mjs
// 断言基于**可见文本**与 DOM value，不 mock 组件；截图落在 /tmp/ui-*.png。
//
// 覆盖（Review 要求的两条）：
//   · Hub DM / 群的设置页归属显示 = agent:<id> / group:<g>（不是 workspace:）；
//   · 编辑 agent / 群路由只改被编辑的维度，目标不被静默改写成 workspace。
import { chromium } from 'playwright'

const URL = 'http://localhost:5617/'
let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const fail = (m) => { throw new Error(`断言失败: ${m}`) }
const assert = (c, m) => (c ? ok(m) : fail(m))

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1440, height: 980 } })
page.on('pageerror', (e) => console.log('[pageerror]', e.message.slice(0, 300)))
await page.goto(URL, { waitUntil: 'networkidle' })
await page.waitForTimeout(1500)
const openSettings = async () => {
  await page.getByText('设置', { exact: true }).first().click()
  await page.waitForTimeout(500)
}
const closeSettings = async () => {
  await page.locator('button.settings-close').click()
  await page.waitForTimeout(300)
}
const settingsTab = async (name) => {
  await page.locator('.settings-nav button, .set-nav button').getByText(name, { exact: true }).first().click()
  await page.waitForTimeout(600)
}
const bodyText = () => page.locator('body').innerText()

// ── 1. Hub DM：设置页归属显示 agent:ag_mockres1 ──────────────────────────────
await page.locator('.hub-side-row', { hasText: 'Researcher' }).first().click()
await page.waitForTimeout(800)
await openSettings()
await settingsTab('IM 集成')
let txt = await bodyText()
assert(txt.includes('agent:ag_mockres1'), 'Hub DM → IM 设置页显示 agent:ag_mockres1')
assert(!txt.includes('workspace:~/.go-code/hub'), 'Hub DM → 不显示智能体 home 当作项目归属')
await page.screenshot({ path: '/tmp/ui-10-dm-im-owner.png' })

await settingsTab('钩子')
txt = await bodyText()
assert(txt.includes('agent:ag_mockres1'), 'Hub DM → 钩子页显示同一归属')

// ── 2. Hub 群：设置页归属显示 group:grp_mock1 ────────────────────────────────
await closeSettings()
await page.locator('.hub-side-tabs button, .hub-side-tabs [role="button"]').filter({ hasText: '群组' }).first().click()
await page.waitForTimeout(400)
await page.locator('.hub-group-row', { hasText: '竞品研究' }).first().click()
await page.waitForTimeout(900)
await openSettings()
await settingsTab('IM 集成')
txt = await bodyText()
assert(txt.includes('group:grp_mock1'), 'Hub 群 → IM 设置页显示 group:grp_mock1')
await page.screenshot({ path: '/tmp/ui-11-group-im-owner.png' })

// ── 3. 路由列表显示三种目标 ────────────────────────────────────────────────
assert(txt.includes('workspace:~/workspace/go-code'), '路由列表显示 workspace 目标')
assert(txt.includes('agent:ag_mockres1'), '路由列表显示 agent 目标')
assert(txt.includes('group:grp_mock1'), '路由列表显示 group 目标')

// ── 4. 编辑 agent 路由：换成另一个 agent，目标维度不丢 ──────────────────────
const agentRow = page.locator('.im-route', { hasText: 'oc_mock_dm' }).first()
await agentRow.getByRole('button', { name: '编辑' }).first().click()
await page.waitForTimeout(300)
await page.screenshot({ path: '/tmp/ui-12-edit-agent-route.png' })
// 编辑行：chat 只读文本 + 目标类型 Sel（应已选中「智能体 DM」）+ 值 Sel（已选 ag_mockres1）
const editRow = page.locator('.im-route-add').first()
assert((await editRow.innerText()).includes('oc_mock_dm'), '编辑行固定在该 chat（不可切 chat）')
const kindBtn = editRow.locator('.sel-btn').nth(0)
assert((await kindBtn.innerText()).includes('智能体 DM'), '编辑 agent 路由时目标类型预选 agent（不被改写成 workspace）')
await page.screenshot({ path: '/tmp/ui-12-edit-agent-route.png' })
// 换成 Writer（另一个 agent）：只改这一维，目标类型仍是 agent
await editRow.locator('.sel-btn').nth(1).click()
await page.getByRole('option').filter({ hasText: 'Writer' }).first().click()
await page.waitForTimeout(200)
await editRow.locator('button.btn.pri').click() // ✓
await page.waitForTimeout(900)
txt = await bodyText()
assert(txt.includes('agent:ag_mockwri2'), '编辑后路由仍是 agent 目标（换成 ag_mockwri2）')
assert(!/oc_mock_dm[^]{0,120}workspace:/.test(txt), '编辑 agent 路由没有把它改写成 workspace 目标')
await page.screenshot({ path: '/tmp/ui-13-after-edit-agent.png' })

// ── 5. 编辑群路由：只改执行者，群目标不被改写 ──────────────────────────────
const teamRow = page.locator('.im-route', { hasText: 'oc_mock_team' }).first()
assert((await teamRow.innerText()).includes('group:grp_mock1'), '群路由初始显示 group:grp_mock1')
await teamRow.getByRole('button', { name: '编辑' }).first().click()
await page.waitForTimeout(300)
const groupEdit = page.locator('.im-route-add').first()
assert((await groupEdit.locator('.sel-btn').nth(0).innerText()).includes('群聊'), '群路由编辑预选 group')
assert((await groupEdit.locator('input.im-inp').last().inputValue()) === 'ag_mockres1', '群路由编辑保留执行者 agent（不丢）')
await page.screenshot({ path: '/tmp/ui-14-edit-group-route.png' })
const actor = groupEdit.locator('input.im-inp').last()
await actor.fill('ag_mockwri2')
await groupEdit.locator('button.btn.pri').click()
await page.waitForTimeout(900)
txt = await bodyText()
assert(txt.includes('group:grp_mock1'), '改群执行者后群目标仍是 group:grp_mock1（目标未被改写）')
assert(!/oc_mock_team[^]{0,120}workspace:/.test(txt), '群路由没有被改写成 workspace 目标')
await page.screenshot({ path: '/tmp/ui-15-group-route-kept.png' })

console.log(`\n${pass} 项 UI 断言全部通过`)
await browser.close()
