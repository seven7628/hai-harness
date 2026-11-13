#!/usr/bin/env node
// 场景2 浏览器验收：Project 待办在 Hub **真页面**里长什么样（DM / 群内联卡）。
//
// 与 scripts/test-hub-todos.mjs 的分工：那个跑 store/纯函数（投递链路 + 显示口径），
// 这个跑 DOM —— 真 CSS（index.css / demo.css / hub.css / HubInlineTodos.css）、真 React、
// 真布局。两边都过才算「卡看得见，且长得对」。
//
// 用法（默认自起 dev server；5224 端口，不占 5173/5180）：
//   node scripts/check-hub-inline-todos.mjs
//   node scripts/check-hub-inline-todos.mjs --url http://localhost:5180/   # 复用已起的
//
// 夹具：scripts/hub-todos-harness.mjs（复用 hub-harness 的打包/挂载，只多包一层会话骨架
// 与组件自己的 CSS）。状态由本脚本直接塞进 store（tasks / todoSnapshots / runNames），
// 不依赖 mock.ts —— 那张热点文件不该为一条 UI 断言再分叉一套 fixture。
//
// 判据（任一失败 → exit 1）：
//   1. DM 主 run 卡：归属「A · proj / s_proj · 主会话」、`ToDo · 0/2`、2 条；
//   2. 子 run 卡：「子智能体 <name>」；
//   3. 未知 run：`run_` + 前 8 位，**不得**出现「主会话」；
//   4. 作用域隔离：群 scope 的任务不出现在 DM、DM 任务不进群；
//   5. 终态收卡；折叠按钮 aria-expanded 来回切换；
//   6. 只读：`.msg-row` 条数不变、transport 一条 hub_send/hub_group_send 都没发；
//   7. 上限：最多 3 张卡、每卡最多 3 条，多出来的折成 `+N`；
//   8. 390×844 无横向溢出，且 `.messages` 仍有高度（卡没把消息区压成 0）；
//   9. console error / pageerror 0。
import { chromium } from 'playwright'
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import { mountInlineTodos, injectHubState, setScope, emitHubEvent, sentHubCommands } from './hub-todos-harness.mjs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const PORT = Number(arg('port', process.env.HUB_INLINE_TODOS_PORT || '5224'))
const URL_ARG = arg('url', '')
const BASE = URL_ARG || `http://localhost:${PORT}/`
const NO_SPAWN = argv.includes('--no-spawn')
const SHOT = (name) => `/tmp/hub-inline-todos-${name}.png`

const failed = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }

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

// —— 夹具数据（线上事件的形状：session + workspace + run + todos）——
const T0 = Date.parse('2026-05-01T10:00:00Z')
const iso = (s) => new Date(T0 + s * 1000).toISOString()
const WS_MAIN = '/Users/me/work/proj'
const WS_GRP = '/Users/me/work/grp'
const S = { main: 's_proj', sub: 's_proj_sub', ghost: 's_proj_ghost', grp: 's_grp' }
const runKey = (sessionId, runId) => `${sessionId}\u0000${runId}`
const item = (id, title, done = false) => ({ id, title, done })

const task = (id, opts = {}) => ({
  id,
  title: `任务 ${id}`,
  status: opts.status ?? 'running',
  origin: opts.origin ?? { kind: 'dm', agent_id: 'ag_a', agent_name: 'A' },
  target: { kind: 'workspace', workspace: opts.workspace ?? WS_MAIN, session_id: opts.session },
  updated_at: opts.at ?? iso(0),
})
const snap = (sessionId, runId, workspace, todos, at) => ({
  key: runKey(sessionId, runId), sessionId, runId, workspace, todos, ts: '', seq: 0, at,
})

// 阶段 A：三张 DM 卡（主 run / 子 run / 未知 run）+ 一条群任务（不该出现在 DM）。
const PHASE_A = {
  tasks: [
    task('task_dm_main', { session: S.main, at: iso(1) }),
    task('task_dm_sub', { session: S.sub, at: iso(2) }),
    task('task_dm_ghost', { session: S.ghost, at: iso(3) }),
    task('task_group', {
      origin: { kind: 'group', group_id: 'g1', agent_id: 'ag_a', agent_name: 'A' },
      workspace: WS_GRP, session: S.grp, at: iso(4),
    }),
  ],
  todoSnapshots: Object.fromEntries([
    [runKey(S.main, 'main'), snap(S.main, 'main', WS_MAIN, [item('m1', '勘察三家定价页'), item('m2', '整理对比表')], 1)],
    [runKey(S.sub, 'sub1'), snap(S.sub, 'sub1', WS_MAIN, [item('u1', '拉公开情报')], 2)],
    [runKey(S.ghost, 'run_ghost1234abcd'), snap(S.ghost, 'run_ghost1234abcd', WS_MAIN, [item('g1', '没人认领的活')], 3)],
    [runKey(S.grp, 'gmain'), snap(S.grp, 'gmain', WS_GRP, [item('w1', '群里的活')], 4)],
  ]),
  runNames: {
    // agent_start 的两种已知形态 + 一条未知（runNames 里没有 = 只有 run id）。
    [runKey(S.main, 'main')]: { root: true, name: '' },
    [runKey(S.sub, 'sub1')]: { root: false, name: 'Scout' },
    [runKey(S.grp, 'gmain')]: { root: true, name: '' },
  },
}

// 阶段 B：4 条在跑的任务（上限 3 张卡）+ 最新那张 5 条待办（上限 3 条 + `+N`）。
const capTasks = ['task_cap1', 'task_cap2', 'task_cap3', 'task_cap4'].map((id, i) => task(id, {
  session: `s_cap${i + 1}`, at: iso(10 + i),
}))
const capSnaps = [1, 2, 3, 4].map((n) => snap(
  `s_cap${n}`, `cap${n}`, WS_MAIN,
  n === 4
    ? [item('c1', '第一条', true), item('c2', '第二条', true), item('c3', '第三条'), item('c4', '第四条'), item('c5', '第五条')]
    : [item(`k${n}`, `第 ${n} 条的活`)],
  10 + n - 1,
))
const PHASE_CAP = {
  tasks: capTasks,
  todoSnapshots: Object.fromEntries(capSnaps.map((s) => [s.key, s])),
  runNames: {},
}

const browser = await chromium.launch()
const page = await mountInlineTodos(browser, { baseURL: BASE })

// 卡片快照：文案 + 条目 + `+N` + 折叠态，全部取自 DOM（不读 store，否则测的是 store）。
const cards = () => page.evaluate(() => Array.from(document.querySelectorAll('.hub-inline-todo')).map((el) => {
  const text = (sel) => el.querySelector(sel)?.textContent?.trim() ?? ''
  return {
    title: text('.hub-inline-todo-title'),
    attr: text('.hub-inline-todo-attr'),
    attrTitle: el.querySelector('.hub-inline-todo-attr')?.getAttribute('title') ?? '',
    items: Array.from(el.querySelectorAll('.hub-inline-todo-item')).map((li) => ({
      text: li.querySelector('.hub-inline-todo-text')?.textContent?.trim() ?? '',
      done: li.classList.contains('done'),
    })),
    more: text('.hub-inline-todo-more'),
    folded: el.querySelector('.hub-inline-todo-fold')?.getAttribute('aria-expanded') ?? null,
    barNow: el.querySelector('.hub-inline-todo-bar')?.getAttribute('aria-valuenow') ?? null,
  }
}))
const msgRows = () => page.locator('.msg-row').count()
const sent = async () => sentHubCommands(page)

try {
  // ———————————— 阶段 A：DM ————————————
  await injectHubState(page, PHASE_A)
  const rowsBefore = await msgRows()
  const a = await cards()
  check(a.length === 3, `DM 出 3 张卡（主 run / 子 run / 未知 run；got=${a.length}）`)

  // 1) 主 run 卡
  const main = a.find((c) => c.attr.endsWith('主会话'))
  check(!!main, `主 run 卡的归属写明「主会话」（got=${JSON.stringify(a.map((c) => c.attr))}）`)
  if (main) {
    check(main.attr === 'A · proj / s_proj · 主会话', `主 run 归属 = A · proj / s_proj · 主会话（got=${main.attr}）`)
    check(main.title === 'ToDo · 0/2', `主 run 卡标题 = ToDo · 0/2（got=${main.title}）`)
    check(main.items.length === 2, `主 run 卡列出 2 条（got=${main.items.length}）`)
    check(main.items[0]?.text === '勘察三家定价页', `第一条待办文案（got=${main.items[0]?.text}）`)
    check(main.items.every((it) => it.done === false), '0/2 时没有条目被标成完成')
    check(main.barNow === '0', `进度条 aria-valuenow=0（got=${main.barNow}）`)
  }

  // 2) 子 run 卡
  const sub = a.find((c) => c.attr.includes('子智能体'))
  check(!!sub, `子 run 卡的归属写明「子智能体 <name>」（got=${JSON.stringify(a.map((c) => c.attr))}）`)
  if (sub) {
    check(sub.attr === 'A · proj / s_proj_sub · 子智能体 Scout', `子 run 归属 = …子智能体 Scout（got=${sub.attr}）`)
    check(sub.items.length === 1, `子 run 卡列出 1 条（got=${sub.items.length}）`)
  }

  // 3) 未知 run：run_ + 前 8 位，不冒充主会话、也不冒充子智能体
  const ghost = a.find((c) => c.attr.includes('run_ghost123'))
  check(!!ghost, `未知 run 显示 run_ + 前 8 位（got=${JSON.stringify(a.map((c) => c.attr))}）`)
  if (ghost) {
    check(ghost.attr === 'A · proj / s_proj_ghost · run_ghost123', `未知 run 归属 = …run_ghost123（got=${ghost.attr}）`)
    check(!ghost.attr.includes('主会话'), `未知 run **不得**写「主会话」（got=${ghost.attr}）`)
    check(!ghost.attr.includes('子智能体'), `未知 run 也不冒充子智能体（got=${ghost.attr}）`)
    check(ghost.attrTitle === ghost.attr, `完整归属留在 title 里（got=${ghost.attrTitle}）`)
  }
  await page.screenshot({ path: SHOT('dm') })

  // 4a) 群任务不进 DM
  check(!a.some((c) => /grp|s_grp/.test(c.attr)), `群 scope 的任务不出现在 DM（got=${JSON.stringify(a.map((c) => c.attr))}）`)
  check(await page.locator('.hub-inline-todos').count() === 1, 'DM 只有一处待办区（不是每卡一个区）')

  // 4b) DM 任务不进群：同一张页面切 scope
  await setScope(page, { kind: 'group', groupId: 'g1' })
  const g = await cards()
  check(g.length === 1, `群 scope 只出本群那 1 张卡（got=${g.length}）`)
  check(g[0]?.attr === 'A · grp / s_grp · 主会话', `群卡归属 = A · grp / s_grp · 主会话（got=${g[0]?.attr}）`)
  check(!!g[0]?.items.some((it) => it.text === '群里的活'), '群卡列的是群里那条待办')
  check(!g.some((c) => /s_proj/.test(c.attr)), `DM 任务不进群（got=${JSON.stringify(g.map((c) => c.attr))}）`)
  await page.screenshot({ path: SHOT('group') })

  // 切回 DM：3 张卡回来（scope 是可逆的投影，不是一次性过滤）
  await setScope(page, { kind: 'dm', agentId: 'ag_a' })
  check(await page.locator('.hub-inline-todo').count() === 3, '切回 DM 仍是 3 张卡')

  // 5a) 折叠：aria-expanded 来回切换，列表跟着收起/展开
  const mainCard = page.locator('.hub-inline-todo').filter({ hasText: '主会话' }).first()
  const fold = mainCard.locator('.hub-inline-todo-fold')
  check(await fold.getAttribute('aria-expanded') === 'true', '默认展开（aria-expanded=true）')
  check(await mainCard.locator('.hub-inline-todo-item').count() === 2, '展开时 2 条都在 DOM 里')
  await fold.click()
  await page.waitForTimeout(120)
  check(await fold.getAttribute('aria-expanded') === 'false', '点一下 → aria-expanded=false')
  check(await mainCard.locator('.hub-inline-todo-item').count() === 0, '折叠后条目从 DOM 里移除（不是只是看不见）')
  check((await fold.textContent())?.includes('展开'), '折叠后按钮文案变「展开」')
  await page.screenshot({ path: SHOT('folded') })
  await fold.click()
  await page.waitForTimeout(120)
  check(await fold.getAttribute('aria-expanded') === 'true', '再点一下 → 切回 aria-expanded=true')
  check(await mainCard.locator('.hub-inline-todo-item').count() === 2, '展开后 2 条回来')

  // 5b) 终态收卡：hub_task_update completed（与 bridge 同形：带整条 task）→ 卡片立刻消失
  const mainDone = { ...PHASE_A.tasks[0], status: 'completed', updated_at: iso(5) }
  await emitHubEvent(page, {
    event_type: 'hub_task_update', task_id: 'task_dm_main', status: 'completed', task: mainDone,
  })
  const afterDone = await cards()
  check(afterDone.length === 2, `终态任务卡消失（got=${afterDone.length} 张）`)
  check(!afterDone.some((c) => c.attr.endsWith('主会话')), `终态任务的卡不再出现（got=${JSON.stringify(afterDone.map((c) => c.attr))}）`)

  // 6) 只读：消息条数不变 + 一条 send 都没发
  check(await msgRows() === rowsBefore, `内联卡不写 hub 消息（.msg-row ${rowsBefore} → ${await msgRows()}）`)
  const sentA = await sent()
  check(!sentA.includes('hub_send'), `没有发 hub_send（got=${JSON.stringify(sentA)}）`)
  check(!sentA.includes('hub_group_send'), `没有发 hub_group_send（got=${JSON.stringify(sentA)}）`)

  // ———————————— 阶段 B：上限（3 张卡 / 3 条 + N）————————————
  await injectHubState(page, PHASE_CAP)
  const b = await cards()
  check(b.length === 3, `最多 3 张卡（4 条在跑任务只出 3 张；got=${b.length}）`)
  const top = b[0]
  check(top?.title === 'ToDo · 2/5', `最新 run 在第一位且计数按全量算 = ToDo · 2/5（got=${top?.title}）`)
  check(top?.items.length === 3, `每卡最多列 3 条（got=${top?.items.length}）`)
  check(top?.more === '+2', `超出部分折成 +2（got=${top?.more}）`)
  check(b.every((c) => c.items.length <= 3), '没有任何卡超过 3 条')
  check(top?.items.filter((it) => it.done).length === 2, '前 3 条里 2 条是完成态')
  await page.screenshot({ path: SHOT('cap') })

  // ———————————— 阶段 C：390×844 ————————————
  await page.setViewportSize({ width: 390, height: 844 })
  await page.waitForTimeout(250)
  const layout = await page.evaluate(() => {
    const doc = document.documentElement
    const messages = document.querySelector('.messages')
    const cards = Array.from(document.querySelectorAll('.hub-inline-todo'))
    const todos = document.querySelector('.hub-inline-todos')
    const css = todos ? getComputedStyle(todos) : null
    return {
      docW: doc.scrollWidth,
      viewportW: window.innerWidth,
      messagesH: messages ? messages.getBoundingClientRect().height : -1,
      cardRight: Math.max(0, ...cards.map((c) => c.getBoundingClientRect().right)),
      cardCount: cards.length,
      todosMaxH: todos ? todos.getBoundingClientRect().height : -1,
      // 组件自己的 CSS 有没有真的加载：display/max-height 只可能来自 HubInlineTodos.css。
      todosDisplay: css?.display ?? '',
      todosMaxHeight: css?.maxHeight ?? '',
      todosOverflow: css?.overflowY ?? '',
    }
  })
  check(layout.cardCount === 3, `390 下 3 张卡仍渲染（got=${layout.cardCount}）`)
  check(layout.todosDisplay === 'flex', `HubInlineTodos.css 真的生效（display=${layout.todosDisplay}）`)
  check(layout.todosMaxHeight !== 'none', `待办区有 max-height（got=${layout.todosMaxHeight}）`)
  check(layout.todosOverflow === 'auto', `待办区自己滚动（overflow-y=${layout.todosOverflow}）`)
  check(layout.docW <= 390, `390 无横向溢出（doc=${layout.docW}）`)
  check(layout.cardRight <= 390.5, `卡片右边缘不出屏（right=${layout.cardRight.toFixed(1)}）`)
  check(layout.messagesH > 0, `390 下 .messages 仍有高度（got=${layout.messagesH}）`)
  check(layout.todosMaxH > 0 && layout.todosMaxH <= 141, `待办区被限高（got=${layout.todosMaxH.toFixed(1)}px ≤ 140+边框）`)
  await page.screenshot({ path: SHOT('390') })

  // 小屏也点得开：折叠按钮在 390 下仍可点（触达目标在卡内、不溢出）。
  const fold390 = page.locator('.hub-inline-todo-fold').first()
  const box = await fold390.boundingBox()
  check(!!box && box.width > 0 && box.height > 0, `390 下折叠按钮可点（box=${JSON.stringify(box)}）`)
  await fold390.click()
  await page.waitForTimeout(120)
  check(await fold390.getAttribute('aria-expanded') === 'false', '390 下折叠交互有效')

  // 9) 控制台
  const hubErrors = page.__hubErrors ?? []
  check(hubErrors.length === 0, `console error / pageerror 0（got=${hubErrors.slice(0, 3).join(' | ')}）`)
} finally {
  await page.close().catch(() => {})
  await browser.close().catch(() => {})
  stopServer()
}

if (failed.length) {
  console.error(`\n❌ Hub 内联待办卡：${failed.length} 项不达标`)
  for (const f of failed) console.error(`   · ${f}`)
  process.exit(1)
}
console.log('✅ Hub 内联待办卡：DM 归属 / 子 run / 未知 run / 作用域隔离 / 折叠 / 终态收卡 / 只读 / 上限 / 390 全部通过')
console.log(`   截图：${SHOT('dm')} ${SHOT('group')} ${SHOT('folded')} ${SHOT('cap')} ${SHOT('390')}`)
