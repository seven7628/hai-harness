#!/usr/bin/env node
// 子智能体树 + 子会话视图 e2e（用户 2026-10 需求）。
//
// 要验的四件事，都是"看着对但其实错"那一类，必须真渲染才能判：
//   1. **递归层级**：孙智能体画在子智能体**下面**（缩进 + 连线的 guide 列数 = 深度-1），
//      而不是被拉平成同级；
//   2. **连线**：非末位节点有贯穿竖线（guide.on）、末位收头（elbow.last）、
//      选中某个子会话时，一条折线从会话行连到它（.atree-path-line）——回答"从哪条链上下来的"；
//   3. **右栏退役**：右栏连「任务」这个 tab 本身都不再存在（不是「tab 里清空卡片」）——
//      子智能体 = 另一个会话，它的待审批/压缩/待办与主会话同一套面板，只是按当前
//      查看的会话分域渲染。因此这里断言的是「rail 里没有这个入口」+「面板里没有任务卡」；
//   4. **TODO 隔离**：主会话清单与子 agent 清单互不影响 —— 子 agent 的 todo_* 事件
//      不改会话清单，子会话视图显示的是它自己的清单（没有就空，绝不回退）。
//
// 数据不是手搓 blocks，而是**重放真实事件链**（agent_start → todo_* 工具 → agent_end），
// 与生产同一条链路（同 e2e/verify_subagent_transcript_style.mjs 的做法）。
//
// 用法：cd desktop/app && node e2e/verify_agent_tree.mjs
//      REPORT=1 ... # 只取证（截图）不判定
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
const SHOTS = path.join(__dirname, '_agenttree_shots')
const REPORT = process.env.REPORT === '1'

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
      s.on('error', () => { clearTimeout(timer); s.destroy(); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

const PROBE_DIR = path.join(__dirname, '_agenttreeprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh"><head><meta charset="utf-8"><title>agent tree probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')

fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import LeftPanel from '../../src/components/LeftPanel'
import CenterPanel from '../../src/components/CenterPanel'
import RightPanel from '../../src/components/RightPanel'
// ⚠️ 副作用注册，必须与 src/main.tsx 一致（04-review F45）：RightPanel 三个内容面板都是
// getSurface(kind)?.render(...) ?? 兜底。少了这一行，本探针右栏验的一直是兜底分支。
import '../../src/surfaces/builtin'
import { getSurface } from '../../src/surfaces/registry'
import { useAppStore, dispatchEvent, hydrateCheckpoint } from '../../src/store/useAppStore'
import { aggregateFileActivity } from '../../src/lib/fileActivity'
import { withAgentToolBlocks } from '../../src/lib/agentSession'
import '../../src/index.css'
import '../../src/narrative-skin.css'

// —— R07 仪器自检：activity provider 的 render 到底有没有被执行 ——
// 与 R06c 给 web 做的那层包法一致（e2e/verify_auto_open_visual.mjs 的 __webRenderCalls）：
// 计数为 0 说明右栏走的是 RightPanel 的 ?? 兜底分支 —— 那时「链路面板看起来一样」证明的是
// 兜底组件，不是 provider（04-review F45：探针必须先证明自己走的是被测那条路）。
const actProvider = getSurface('activity')
;(window as unknown as { __actRenderCalls: number }).__actRenderCalls = 0
;(window as unknown as { __actProvider: unknown }).__actProvider = actProvider ?? null
if (actProvider) {
  const origActRender = actProvider.render
  actProvider.render = (ctx) => {
    ;(window as unknown as { __actRenderCalls: number }).__actRenderCalls++
    return origActRender(ctx)
  }
}

const q = new URLSearchParams(location.search)
document.documentElement.setAttribute('data-theme', 'ink')
const HOST = q.get('host') || 'left'

const SID = 'probe'
// 探针把 store 挂到 window（生产没有这层；e2e 直接驱动 store 比点 DOM 更稳）
window.__store = useAppStore
window.__dispatch = dispatchEvent
// 文件活动聚合（会话级）：子智能体的工具调用必须能被它看见 —— 右侧文件面板按 toolId
// 定位「本次变更」，缺了它们子会话里点开文件就没有变更视图（用户 2026-10 报障）。
window.__fileActivity = (blocks) => aggregateFileActivity(withAgentToolBlocks(blocks), [])
useAppStore.setState({
  workspace: '/mock/proj-auth',
  activeSessionId: SID,
  bridgeStatus: 'running',
  leftExpanded: true,
  projects: [{ path: '/mock/proj-auth', sessions: [{ id: SID, title: '主会话' }] }],
  sessions: [{ id: SID, title: '主会话' }],
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

const ev = (o) => dispatchEvent({ session_id: SID, timestamp: Date.now(), ...o })

// 主 run
ev({ event_type: 'user_inputs_consumed', user_texts: ['把 auth 重构拆给子智能体'] })
ev({ event_type: 'agent_start', run_id: 'main-1', index: 0, content: '主任务', model: 'deepseek-v4' })
// 主会话自己的 TODO（两条）
ev({ event_type: 'tool_run_start', run_id: 'main-1', id: 'm-todo-1', name: 'todo_add', arguments: '{"title":"主计划甲"}' })
ev({ event_type: 'tool_run_end', run_id: 'main-1', id: 'm-todo-1', name: 'todo_add', result: 'ok',
     todos: [{ id: 't1', title: '主计划甲', done: false }, { id: 't2', title: '主计划乙', done: false }] })

// 子 agent（后台，task-1）
ev({ event_type: 'agent_start', run_id: 'sub-1', parent_run_id: 'main-1', depth: 1, task_id: 'task-1', name: 'auth 审查', content: 'auth 审查' })
// 子 agent 自己的 TODO（与主会话不同）
ev({ event_type: 'tool_run_start', run_id: 'sub-1', id: 's-todo-1', name: 'todo_add', arguments: '{"title":"子计划"}' })
ev({ event_type: 'tool_run_end', run_id: 'sub-1', id: 's-todo-1', name: 'todo_add', result: 'ok',
     todos: [{ id: 't1', title: '子计划', done: false }] })

// 孙 agent（递归：sub-1 派 sub-2）
ev({ event_type: 'agent_start', run_id: 'sub-2', parent_run_id: 'sub-1', depth: 2, task_id: 'task-2', name: '跑测试', content: '跑测试' })
ev({ event_type: 'content_chunk', run_id: 'sub-2', request_id: 'q-sub-2', content: '正在跑 go test …' })

// 另一个子 agent（同级，验证"非末位 → 贯穿竖线"）
ev({ event_type: 'agent_start', run_id: 'sub-3', parent_run_id: 'main-1', depth: 1, task_id: 'task-3', name: '文档校对', content: '文档校对' })
// 后台工具任务（promoted bash）：进树，但**不**进右栏
ev({ event_type: 'tool_run_start', run_id: 'main-1', id: 'call-9', name: 'bash', arguments: '{"command":"sleep 30"}', task_id: 'tooltask-1' })
ev({ event_type: 'task_started', task_id: 'tooltask-1', run_id: 'main-1', tool_name: 'bash', name: 'sleep 30', async: true })
ev({ event_type: 'task_promoted', task_id: 'tooltask-1', kind: 'tool', name: 'bash' })
// 工具跑完（后台任务回执）：结果正文要能在它的子会话里看到
ev({ event_type: 'tool_run_end', run_id: 'main-1', id: 'call-9', name: 'bash', result: 'sleep done (30s)', is_error: false })

// 视图收尾：右栏展开到「链路」面板（验任务 tab 已退役）。
// R11b：面板 = 激活标签，所以「展开到链路」= 该 scope 的标签集里有一个激活的 activity 标签
// （旧实现写的是 state 里的面板字段，那个字段已删）。
{
  const st = useAppStore.getState()
  const v = st.views[SID]
  useAppStore.setState({
    views: { ...st.views, [SID]: { ...v, rightExpanded: true } },
    rightExpanded: true,
    rightSize: 420,
    tabsByScope: { ...st.tabsByScope, [SID]: { tabs: [{ id: 'act1', kind: 'activity' }], activeId: 'act1', closed: [] } },
  })
}

const El = HOST === 'left' ? LeftPanel : HOST === 'center' ? CenterPanel : RightPanel

// ---- 刷新模式（?refresh=1）：模拟「刷新后打开一个长会话」----
//
// 生产里打开长会话只回最近 200 块（blocks_limit），跑半天的会话里 agent 卡片早就
// 在窗口之外 —— 用户报障「刷新后左侧子智能体全没了」就是这条。bridge 现在把窗口外的
// 卡片单独放在响应的 agents 字段里，这里按同一形态喂给 hydrateCheckpoint。
if (q.get('refresh')) {
  window.__historyCalls = []
  // 探针里没有 bridge：把「补拉首屏」换成记录调用（真正要验的是**有没有发起**、
  // 用的 taskId 对不对 —— 分页本身由 store 脚本与 bridge 测试覆盖）。
  useAppStore.setState({
    loadAgentHistory: (blockId, taskId) => { window.__historyCalls.push({ blockId, taskId }) },
  })
  hydrateCheckpoint(SID, {
    blocks: [
      { kind: 'user', id: 500, text: '继续' },
      { kind: 'assistant', id: 501, text: '好' },
    ],
    run_nodes: [{ id: 'main-1', label: '主任务', status: 'done', depth: 0, kind: 'run' }],
    turns: [], todos: [], diffs: [],
  }, undefined, {
    blocksTotal: 520, blocksCursor: 500, blocksHasMore: true,
    agents: [
      { kind: 'agent', id: 40, run_id: 'sub-hist-1', parent_run_id: 'main-1', label: '历史子智能体',
        status: 'completed', spawned_at: 100, task_id: 'task-hist-1', items_trimmed: 37 },
      { kind: 'agent', id: 41, run_id: 'sub-hist-2', parent_run_id: 'sub-hist-1', label: '历史孙智能体',
        status: 'completed', spawned_at: 200, task_id: 'task-hist-2', items_trimmed: 12 },
    ],
  })
}

createRoot(document.getElementById('root')!).render(<El />)
`)

fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const URL_BASE = `http://localhost:${PORT}/e2e/_agenttreeprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'ignore' })
const cleanup = () => {
  if (!process.env.KEEP_PROBE) try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  if (!process.env.KEEP_PROBE) try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)
const watchdog = setTimeout(() => {
  console.error('\n❌ 超时（120s）')
  cleanup()
  process.exit(1)
}, 120_000)
watchdog.unref()

async function main() {
  fs.rmSync(SHOTS, { recursive: true, force: true })
  fs.mkdirSync(SHOTS, { recursive: true })
  await waitPort(PORT)
  // 端口可连只说明**有东西**在监听；再确认这张探针页真能开（子进程退出即失败）
  await waitHttp(URL_BASE, { child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1360, height: 860 } })
  page.on('pageerror', (e) => console.error('[pageerror]', e.message))

  // ---- ① 左栏树：递归层级 + 连线 ----
  await page.goto(`${URL_BASE}?host=left`, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('.agent-tree .atree-row', { timeout: 20000 })
  await page.waitForTimeout(200)

  const rows = await page.locator('.agent-tree .atree-row').evaluateAll((els) =>
    els.map((el) => ({
      label: el.querySelector('.atree-label')?.textContent ?? '',
      level: Number(el.getAttribute('aria-level') ?? 0),
      guides: el.querySelectorAll('.atree-guide').length,
      guidesOn: el.querySelectorAll('.atree-guide.on').length,
      last: !!el.querySelector('.atree-elbow.last'),
      run: el.getAttribute('data-run') ?? '',
      task: el.getAttribute('data-task') ?? '',
      // 缩进量要量**标签**的左边（行的左边缘对所有行都一样：guide 列在行内），
      // 这正是用户看到的「谁在谁右边」。
      indent: Math.round(el.querySelector('.atree-label')?.getBoundingClientRect().left ?? 0),
    })))
  const byLabel = Object.fromEntries(rows.map((r) => [r.label, r]))
  const labels = rows.map((r) => r.label)
  if (REPORT) console.log(JSON.stringify(rows, null, 2))

  assert(rows.length === 4, `树里 4 个节点（2 子 + 1 孙 + 1 后台任务），实际 ${rows.length}：${labels.join(' / ')}`)
  assert(byLabel['auth 审查']?.level === 1, '子智能体 depth=1')
  assert(byLabel['跑测试']?.level === 2, '孙智能体 depth=2（递归，不是被拉平）')
  assert(byLabel['跑测试']?.guides === 1, '孙节点有 1 列祖先 guide（深度-1 列）')
  assert(byLabel['auth 审查']?.guides === 0, '根级子节点没有祖先 guide')
  assert(
    rows.findIndex((r) => r.label === '跑测试') === rows.findIndex((r) => r.label === 'auth 审查') + 1,
    '孙节点紧跟在其父节点之后（深度优先，父在子前）',
  )
  assert(
    byLabel['auth 审查']?.indent < byLabel['跑测试']?.indent,
    '孙节点比子节点缩进更深（视觉层级）',
  )
  // 根级有 3 个同级（bash / 文档校对 / auth 审查）—— 2026-10 起**倒序**（最新在前），
  // 所以末位是最早派发的 auth 审查，而不是最后派发的 bash。
  assert(labels[0] === 'bash', `根层倒序：最后派生/开始的任务在最上（实际首位 ${labels[0]}）`)
  assert(
    labels.indexOf('文档校对') < labels.indexOf('auth 审查'),
    `更晚派发的在前（文档校对 早于 auth 审查 之外的位置关系不对：${labels.join(' / ')}）`,
  )
  assert(byLabel['bash']?.last === false, '「bash」后面还有同级 → 非末位（画 ├）')
  assert(byLabel['文档校对']?.last === false, '「文档校对」也不是末位（auth 审查 还在后面）')
  assert(byLabel['auth 审查']?.last === true, '根级最后一个（最早派发）→ 末位（画 └）')
  assert(byLabel['跑测试']?.last === true, '孙节点是它父的唯一孩子 → 自己也收头（└）')
  assert(byLabel['文档校对']?.guidesOn === 0, '根级兄弟之上没有祖先竖线（它的 guide 列数为 0）')

  await page.screenshot({ path: path.join(SHOTS, 'left-tree.png') })
  ok('左栏树：层级 / 缩进 / 连线字符几何都对')

  // ---- ①a 树的**版式规则真的生效**（不只是 DOM 结构对）----
  //
  // 2026-10 用户报障「这个左侧样式不是没有，而是被影响了」：多写的一个 `*/` 让后面几行
  // 变成游离的 CSS 记号，整条 `.agent-tree` 规则被当作无效选择器丢弃 ——
  // `--atree-step` 于是未定义，连线列塌成 0 宽，行全平在会话行同一层。
  // DOM 断言全绿、看着却完全不对，只有量**计算样式**才抓得住。
  const treeCss = await page.locator('.agent-tree').first().evaluate((el) => {
    const cs = getComputedStyle(el)
    return {
      display: cs.display,
      marginLeft: cs.marginLeft,
      step: cs.getPropertyValue('--atree-step').trim(),
    }
  })
  if (REPORT) console.log(JSON.stringify(treeCss, null, 2))
  assert(treeCss.display === 'flex', `.agent-tree 规则生效（display:flex，实际 ${treeCss.display}）`)
  // 缩进与左边距都是「窄栏里别把标签挤走」的判据（用户 2026-10 两次反馈空隙太大）
  assert(treeCss.step === '12px', `--atree-step 自定义属性生效（实际「${treeCss.step}」）`)
  // 层级判据（语义化的，钉住"子节点不能伸到父节点左边"）：
  //   树干落在会话行圆点的正下方；子智能体的标签在会话标题右侧。
  // 比"某个 px 值"耐用 —— 改配色/字号/缩进时它仍然守得住这条关系。
  const hierarchy = await page.evaluate(() => {
    const cx = (el) => {
      const r = el.getBoundingClientRect()
      return r.left + r.width / 2
    }
    const item = document.querySelector('.session-item.active') || document.querySelector('.session-item')
    const dot = item.querySelector('.dot')
    const title = item.querySelector('.session-title')
    const row = document.querySelector('.agent-tree .atree-row')
    return {
      dotX: Math.round(cx(dot)),
      titleX: Math.round(title.getBoundingClientRect().left),
      nodeX: Math.round(cx(row.querySelector('.atree-node'))),
      labelX: Math.round(row.querySelector('.atree-label').getBoundingClientRect().left),
    }
  })
  if (REPORT) console.log(JSON.stringify(hierarchy, null, 2))
  assert(hierarchy.nodeX >= hierarchy.dotX - 1,
    `树干不越过会话行的圆点（树干 ${hierarchy.nodeX}px / 圆点 ${hierarchy.dotX}px）`)
  assert(hierarchy.nodeX <= hierarchy.dotX + 2,
    `树干就落在圆点正下方（树干 ${hierarchy.nodeX}px / 圆点 ${hierarchy.dotX}px）`)
  assert(hierarchy.labelX > hierarchy.titleX,
    `子智能体标签在会话标题右侧（层级看得出：${hierarchy.labelX} > ${hierarchy.titleX}px）`)
  // 行距：一行到底占多高。>20px 会让 5 个子智能体吃掉半屏（用户实测反馈）。
  const pitch = await page.locator('.agent-tree .atree-row').evaluateAll((els) => {
    const ys = els.map((el) => el.getBoundingClientRect().top)
    const gaps = []
    for (let i = 1; i < ys.length; i++) gaps.push(Math.round(ys[i] - ys[i - 1]))
    return { gaps, h: Math.round(els[0]?.getBoundingClientRect().height ?? 0) }
  })
  assert(pitch.h <= 24, `行高 ≤24px（实际 ${pitch.h}px）`)
  assert(pitch.gaps.every((g) => g <= 24), `相邻行间距 ≤24px（实际 ${JSON.stringify(pitch.gaps)}）`)
  const cols = await page.locator('.agent-tree .atree-row').first().evaluate((el) => {
    const elbow = el.querySelector('.atree-elbow')
    return { elbow: Math.round(elbow?.getBoundingClientRect().width ?? 0) }
  })
  assert(cols.elbow > 8, `连线列有实际宽度（.atree-elbow ≈12px，实际 ${cols.elbow}px）`)

  // ---- ② 点选子会话：祖先链点亮 ----
  await page.locator('.agent-tree .atree-row', { hasText: '跑测试' }).first().click()
  await page.waitForTimeout(250)
  const lit = await page.locator('.agent-tree .atree-row.on').evaluateAll((els) =>
    els.map((el) => el.querySelector('.atree-label')?.textContent ?? ''))
  assert(lit.includes('跑测试'), '被点的节点进入选中态')
  // 选中链是一条**从会话行连到选中节点**的折线（用户 2026-10：「我期望的是从主 session
  // 一直连接到对应的子智能体」「选中最底部的那么连线就会更长」）。
  // 旧实现是逐行点亮 guide —— 并排几条竖线、且接不到会话行，已退役。
  const measurePath = () => page.evaluate(() => {
    const tree = document.querySelector('.agent-tree')
    const p = document.querySelector('.atree-path-line')
    const row = document.querySelector('.agent-tree .atree-row.on')
    if (!tree || !p || !row) return null
    const tb = tree.getBoundingClientRect()
    const pb = p.getBoundingClientRect()
    const rb = row.getBoundingClientRect()
    return {
      len: Math.round(p.getTotalLength()),
      topRel: Math.round(pb.top - tb.top),
      bottomRel: Math.round(pb.bottom - tb.top),
      rowCenterRel: Math.round(rb.top - tb.top + rb.height / 2),
      leftRel: Math.round(pb.left - tb.left),
    }
  })
  const nested = await measurePath()
  if (REPORT) console.log(JSON.stringify(nested, null, 2))
  assert(Boolean(nested), '选中有子会话时，连线必须被画出来（.atree-path-line）')
  assert(nested.len > 20, `连线有实际长度（实际 ${nested.len}px）`)
  assert(nested.topRel <= 4,
    `连线起点接在**会话行**下沿（相对树顶 ${nested.topRel}px；旧实现从根节点才开始画）`)
  assert(Math.abs(nested.bottomRel - nested.rowCenterRel) <= 2,
    `连线终点落在选中行的心线（end=${nested.bottomRel} rowCenter=${nested.rowCenterRel}）`)
  assert(nested.leftRel >= 0 && nested.leftRel < 12, `连线在第一层连线列上（x=${nested.leftRel}px）`)

  // 悬停：整行一层淡底色（参考设计里行是"药丸"，不是分隔线）—— 静止 vs hover 必须有差
  const rowBg = async () => page.locator('.agent-tree .atree-row', { hasText: '文档校对' }).first()
    .evaluate((el) => getComputedStyle(el).backgroundColor)
  const bgIdle = await rowBg()
  await page.locator('.agent-tree .atree-row', { hasText: '文档校对' }).first().hover()
  await page.waitForTimeout(160)
  const bgHover = await rowBg()
  assert(bgIdle !== bgHover, `hover 有底色反馈（静止 ${bgIdle} → 悬停 ${bgHover}）`)
  // 节点标记（连线上的珠子）也要在 hover 时提亮，否则鼠标划过整行只有一片底色
  const nodeRing = await page.locator('.agent-tree .atree-row', { hasText: '文档校对' }).first()
    .locator('.atree-node').evaluate((el) => getComputedStyle(el).boxShadow)
  assert(nodeRing !== 'none', `连线上的节点标记有描边/填充（实际 ${nodeRing.slice(0, 40)}）`)

  // 选中最上面的 vs 最下面的根级节点 → 后者线更长（用户明确要的效果）
  const lenOf = async (label) => {
    await page.locator('.agent-tree .atree-row', { hasText: label }).first().click()
    await page.waitForTimeout(220)
    const m = await measurePath()
    return m ? m.len : -1
  }
  const lenTop = await lenOf('bash')          // 根层最上
  const lenBottom = await lenOf('auth 审查')  // 根层最下（最早派发）
  assert(lenTop > 0 && lenBottom > lenTop + 10,
    `选中的越靠下，连线越长（最上 ${lenTop}px / 最下 ${lenBottom}px）`)
  await page.locator('.agent-tree .atree-row', { hasText: '跑测试' }).first().click()
  await page.waitForTimeout(200)
  await page.screenshot({ path: path.join(SHOTS, 'left-tree-selected.png') })

  // ---- ②a 左栏点「主会话」会话行 → 退出子会话视图（用户 2026-10 报障：点了没反应）----
  await page.evaluate(() => window.__store.getState().focusAgent({ nodeId: 'task-1', runId: 'sub-1', taskId: 'task-1' }))
  await page.waitForTimeout(200)
  assert(await page.evaluate(() => !!window.__store.getState().focusedAgent), '先进入子会话视图')
  await page.locator('.session-item.active').first().click()
  await page.waitForTimeout(250)
  assert(await page.evaluate(() => !window.__store.getState().focusedAgent),
    '左栏点当前会话行 → 退出子会话、回到主会话（same id 此前是空操作）')

  // ---- ②b 分页：根层默认只显示最近 5 个，支持加载更多（用户 2026-10 要求） ----
  //
  // 先在**无聚焦**状态下验「默认 5 个」：聚焦节点会把它所在的那棵根补进可见列表
  // （pageRoots 的规则），那是另一条性质，下面单独验。
  await page.evaluate(() => window.__store.getState().focusAgent(undefined))
  await page.waitForTimeout(200)
  const rootsBefore = await page.locator('.agent-tree .atree-row[aria-level="1"]').count()
  // 补到 9 个根级子智能体（原 3 个 + 6 个），时间递增 → 倒序时新加的在最上
  await page.evaluate(() => {
    const base = Date.now()
    for (let i = 1; i <= 6; i++) {
      window.__dispatch({
        event_type: 'agent_start', session_id: 'probe', run_id: `sub-x${i}`,
        parent_run_id: 'main-1', depth: 1, name: `批量 ${i}`, content: `批量 ${i}`,
        timestamp: base + i * 1000,
      })
    }
  })
  await page.waitForTimeout(400)
  const paged = {
    rootsBefore,
    roots: await page.locator('.agent-tree .atree-row[aria-level="1"]').count(),
    all: await page.locator('.agent-tree .atree-row').count(),
    moreText: await page.locator('.atree-more-btn').innerText().catch(() => ''),
    firstLabels: await page.locator('.agent-tree .atree-row[aria-level="1"] .atree-label').allInnerTexts(),
    allLabels: await page.locator('.agent-tree .atree-label').allInnerTexts(),
  }
  if (REPORT) console.log(JSON.stringify(paged, null, 2))
  assert(paged.roots === 5, `根层默认只显示最近 5 个（实际 ${paged.roots}；分页前 ${paged.rootsBefore} 个）`)
  assert(paged.firstLabels[0] === '批量 6', `倒序：最新派发的在最上（实际首位 ${paged.firstLabels[0]}）`)
  assert(/还有 4 个/.test(paged.moreText), `「加载更多」标明还剩几个（实际「${paged.moreText}」）`)
  assert(!paged.allLabels.includes('auth 审查'),
    `最早派发的 auth 审查 被分页挡掉 —— DOM 里连它的行都没有（可见标签：${paged.allLabels.join(' / ')}）`)
  await page.screenshot({ path: path.join(SHOTS, 'left-tree-paged.png') })

  // 聚焦到**被分页挡掉**的那个（最早派发的 auth 审查）→ 它那棵根必须补进来，
  // 否则「审批自动聚焦」之后左栏看不到任何高亮（用户会以为自动聚焦没生效）。
  await page.evaluate(() => window.__store.getState().focusAgent({ nodeId: 'task-1', runId: 'sub-1', taskId: 'task-1' }))
  await page.waitForTimeout(250)
  const withFocus = {
    roots: await page.locator('.agent-tree .atree-row[aria-level="1"]').count(),
    on: await page.locator('.agent-tree .atree-row.on .atree-label').allInnerTexts(),
  }
  assert(withFocus.roots === 6, `聚焦被挡掉的节点 → 它那棵补进来（5+1，实际 ${withFocus.roots}）`)
  assert(withFocus.on.includes('auth 审查'), '补进来的那棵就是被选中的（高亮可见）')
  await page.screenshot({ path: path.join(SHOTS, 'left-tree-paged-focus.png') })

  // 点「加载更多」→ 全部显示
  await page.evaluate(() => window.__store.getState().focusAgent(undefined))
  await page.waitForTimeout(150)
  await page.locator('.atree-more-btn').first().click()
  await page.waitForTimeout(300)
  const expanded = {
    roots: await page.locator('.agent-tree .atree-row[aria-level="1"]').count(),
    less: await page.locator('.atree-more-btn.less').count(),
  }
  assert(expanded.roots === 9, `加载更多后根层全显示（实际 ${expanded.roots}）`)
  assert(expanded.less === 1, '展开后给「收起更早的」回程入口')
  await page.screenshot({ path: path.join(SHOTS, 'left-tree-paged-all.png') })

  // ---- ③ 子会话视图（对话区）：版式 = 主会话页 + 一个返回 ----
  await page.goto(`${URL_BASE}?host=center`, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('.narrative, .agent-tree', { timeout: 20000 })
  // 先量主会话叙述区的版式（用户 2026-10 要求「子会话页复用主智能体页面样式」）
  const mainLayout = await page.locator('.narrative').first().evaluate((el) => {
    const cs = getComputedStyle(el)
    return { maxWidth: cs.maxWidth, padding: cs.padding, fontSize: cs.fontSize, overflowAnchor: cs.overflowAnchor }
  })
  // 直接经 store 聚焦到孙 agent（等价于左栏点选；这里验证视图本身）
  await page.evaluate(() => {
    window.__store.getState().focusAgent({ nodeId: 'task-2', runId: 'sub-2', taskId: 'task-2' })
  })
  await page.waitForSelector('.narrative .as-hd', { timeout: 10000 })
  await page.waitForTimeout(200)
  const sess = {
    crumb: await page.locator('.narrative .as-crumb').innerText(),
    title: await page.locator('.narrative .as-crumb-cur').innerText(),
    // 转录现在就是主会话的渲染（不再有独立的 .agent-tr 外壳）
    transcript: await page.locator('.narrative .msg.assistant').count(),
    // 待办只有右栏一份（跟随作用域）：子会话视图里**刻意没有**第二份
    todoInSession: await page.locator('.narrative .todo-panel').count(),
    todoText: await page.locator('.narrative').innerText().catch(() => ''),
    backBtn: await page.locator('.narrative .as-back').count(),
  }
  if (REPORT) console.log(JSON.stringify(sess, null, 2))
  assert(sess.title.includes('跑测试'), `子会话标题 = 该子智能体（实际 ${sess.title}）`)
  assert(sess.crumb.includes('auth 审查'), '面包屑显示祖先链（递归场景能看出从哪来的）')
  assert(sess.transcript === 1, '子会话渲染它自己的转录')
  // 版式同等：子会话内容容器必须与主会话叙述区**同宽同内边距**（复用同一个 .narrative
  // class，而不是抄一遍数值 —— 抄的值会随主会话那边调整而漂移，且只有肉眼能发现）。
  const subLayout = await page.locator('.narrative:has(.as-hd)').evaluate((el) => {
    const cs = getComputedStyle(el)
    const hd = el.querySelector('.as-hd')
    return {
      hasNarrative: el.classList.contains('narrative'),
      maxWidth: cs.maxWidth, padding: cs.padding, fontSize: cs.fontSize, overflowAnchor: cs.overflowAnchor,
      hdBorder: hd ? getComputedStyle(hd).borderBottomWidth : null,
    }
  })
  if (REPORT) console.log(JSON.stringify({ mainLayout, subLayout }, null, 2))
  assert(subLayout.hasNarrative, '子会话内容容器直接复用主会话的 .narrative（同一个 class，不是另抄一套值）')
  assert(subLayout.maxWidth === mainLayout.maxWidth, `同宽（主 ${mainLayout.maxWidth} / 子 ${subLayout.maxWidth}）`)
  assert(subLayout.padding === mainLayout.padding, `同内边距（主 ${mainLayout.padding} / 子 ${subLayout.padding}）`)
  assert(subLayout.fontSize === mainLayout.fontSize, '同字号')
  assert(subLayout.overflowAnchor === mainLayout.overflowAnchor, '同滚动锚定策略（否则流式时一处抖一处不抖）')
  assert(subLayout.hdBorder === '0px', `头部不是「卡片头」（无分隔线，实际 ${subLayout.hdBorder}）—— 它就是叙述区第一行`)
  await page.screenshot({ path: path.join(SHOTS, 'center-subsession-layout.png') })

  assert(sess.todoInSession === 0, '子会话视图里没有第二份待办面板（一份清单只在一处显示）')
  assert(!sess.todoText.includes('主计划甲'), '子会话**不显示**主会话的待办（核心回归）')
  assert(sess.backBtn === 1, '有「返回主会话」出口')
  await page.screenshot({ path: path.join(SHOTS, 'center-subsession.png') })

  // ---- ⑤ 后台工具任务（异步 bash）：进树，且能在子会话里看到参数/结果 ----
  await page.evaluate(() => {
    window.__store.getState().focusAgent({ nodeId: 'tooltask-1', runId: 'main-1', taskId: 'tooltask-1' })
  })
  await page.waitForSelector('.narrative .as-hd', { timeout: 10000 })
  await page.waitForTimeout(200)
  // 2026-11 运行时皮肤：默认（简洁档）的工具行是**车道行** `.rt-turn > .rt-act`，
  // `.tool-row` / `.tool-group` 只在展开全量视图里出现 —— 主对话与子会话两处都走同一套，
  // 故锚点必须是 .rt-act（旧的 .tool-row 断言在这个皮肤下恒为 0，属脚本 drift）。
  const laneCount = await page.locator('.narrative .rt-act').count()
  const turnCount = await page.locator('.narrative .rt-turn').count()
  // 简洁档默认把这一个调用折成「Worked · 1 call」，参数/结果要点开才看得到（与主对话同构）。
  if (laneCount > 0) {
    await page.locator('.narrative .rt-act').first().click()
    await page.waitForTimeout(300)
  }
  const toolSess = {
    title: await page.locator('.narrative .as-crumb-cur').innerText(),
    lanes: laneCount,
    turns: turnCount,
    text: await page.locator('.narrative').innerText(),
    // 旧右栏那套展开体外壳（.agent-view / .av-*）不得作为子会话外壳回归
    legacyShell: await page.locator('.narrative .agent-view, .narrative .av-hd, .narrative .av-body').count(),
    backBtn: await page.locator('.narrative .as-back').count(),
  }
  if (REPORT) console.log(JSON.stringify(toolSess, null, 2))
  assert(toolSess.turns >= 1 && toolSess.lanes >= 1,
    `后台工具任务在子会话里用主对话同一套工具行渲染（.rt-turn ${toolSess.turns} / .rt-act ${toolSess.lanes}）`)
  assert(toolSess.text.includes('sleep 30'), `展开后看得到工具参数（实际「${toolSess.text.replace(/\n/g, ' / ')}」）`)
  assert(toolSess.legacyShell === 0, '子会话没有回退到旧右栏展开体外壳（.agent-view/.av-*）')
  assert(toolSess.backBtn === 1, '工具任务的子会话也有「返回主会话」')
  await page.screenshot({ path: path.join(SHOTS, 'center-tool-task.png') })

  // ---- ④b 子会话的输入框 = 发给子智能体；并提供明确的回程 ----
  //
  // 用户 2026-10 要求三件事：
  //   1. 子会话窗口里打字发送 → 发给子智能体（此前一律发给主会话）
  //   2. 子会话窗口要有快速返回主会话的入口
  //   3. 左栏点主会话行 → 切回主会话（此前是空操作，因为 same id 时 setActiveSession 什么都不做）
  await page.evaluate(() => window.__store.getState().focusAgent({ nodeId: 'task-1', runId: 'sub-1', taskId: 'task-1' }))
  await page.waitForSelector('.narrative .as-hd', { timeout: 10000 })
  await page.waitForTimeout(200)
  // 间谍：拦 sendToAgent，验证「发出去了、发给了谁」
  await page.evaluate(() => {
    window.__sends = []
    const st = window.__store.getState()
    const orig = st.sendToAgent
    window.__store.setState({
      sendToAgent: (taskId, text) => { window.__sends.push({ taskId, text }); return orig(taskId, text) },
    })
  })
  const hint = {
    text: await page.locator('.composer-target-hint').first().innerText(),
    back: await page.locator('.composer-target-hint .cth-back').count(),
    headBack: await page.locator('.as-back').count(),
  }
  assert(/发给「auth 审查」/.test(hint.text), `提示条说明发给谁（实际「${hint.text.replace(/\n/g, ' ')}」）`)
  assert(hint.back === 1, '输入框上方有「返回主会话」（注意力在这里，回程要够近）')
  assert(hint.headBack === 1, '头部也保留「返回主会话」')

  await page.locator('.composer textarea.input').fill('请把返回 401 的分支也改掉')
  await page.locator('.composer textarea.input').press('Enter')
  await page.waitForTimeout(300)
  const sends = await page.evaluate(() => window.__sends)
  assert(sends.length === 1, `发送走了 sendToAgent（子智能体通道），实际 ${sends.length} 次`)
  assert(sends[0].taskId === 'task-1', `发给了正在看的那个子智能体（task-1，实际 ${sends[0].taskId}）`)
  assert(sends[0].text.includes('401'), '正文原样投递')
  const cleared = await page.locator('.composer textarea.input').inputValue()
  assert(cleared === '', '发完即清空输入框')
  await page.screenshot({ path: path.join(SHOTS, 'center-subsession-composer.png') })

  // 发出去的话必须出现在**它的转录里**（用户 2026-10：「子智能体发送消息不能等同于主
  // 智能体吗？理论上就是不同的 session 管理」）。事件在消息被投入上下文时产生
  // （subagent 的 Poll，所有投递源的唯一汇合点），reducer 追加一条 user 项 →
  // 子会话按主会话的 user 块渲染。
  await page.evaluate(() => window.__dispatch({
    event_type: 'agent_user_message', session_id: 'probe', task_id: 'task-1',
    text: '把返回 401 的分支也改掉', source: 'user', timestamp: Date.now(),
  }))
  await page.waitForTimeout(350)
  /* skin：user 块（`.msg.user`）在**三档皮肤**下都渲染 —— current/v3 走 Narrative.renderBlock
     的老 DOM（`.msg.user > .who/.body`，F43 恢复；此前 6d28d60 把这条分支连同事件卡一起删了，
     默认皮肤下用户消息整段消失），runtime 走 UserBlock（`.msg.user.rt-you`）。
     这一段仍显式切到 runtime 再量：它同时钉住「子会话宿主走的是 UserBlock 那条路」，
     与主会话的三档断言（e2e/verify_user_blocks.mjs）互补，不重复。
     皮肤只改显示层（MutationObserver → 重渲染），store 里的 user 项不受影响。 */
  await page.evaluate(() => document.documentElement.setAttribute('data-skin', 'runtime'))
  await page.waitForTimeout(300)
  const userInTranscript = {
    blocks: await page.locator('.narrative .msg.user').count(),
    text: await page.locator('.narrative .msg.user').first().innerText().catch(() => ''),
    items: await page.evaluate(() => {
      const b = window.__store.getState().blocks.find((x) => x.kind === 'agent' && x.taskId === 'task-1')
      return b ? b.items.filter((it) => it.kind === 'user').length : -1
    }),
  }
  if (REPORT) console.log(JSON.stringify(userInTranscript, null, 2))
  assert(userInTranscript.items === 1, `store：卡片 items 里多了一条 user 项（实际 ${userInTranscript.items}）`)
  assert(userInTranscript.blocks === 1, `子会话转录里渲染成主会话同款的 user 块（实际 ${userInTranscript.blocks} 块）`)
  assert(userInTranscript.text.includes('401'), `正文可见（实际「${userInTranscript.text.replace(/\n/g, ' ')}」）`)
  await page.screenshot({ path: path.join(SHOTS, 'center-subsession-user-msg.png') })

  // 点输入框上的返回 → 回到主会话
  await page.locator('.composer-target-hint .cth-back').click()
  await page.waitForTimeout(300)
  assert(await page.evaluate(() => !window.__store.getState().focusedAgent), '点「返回主会话」→ 退出子会话视图')
  assert(await page.locator('.narrative .as-hd').count() === 0, '子会话头部消失（真的回到主会话）')
  // 那句话**只属于子运行的上下文**：主会话叙述区不该出现它（否则等于把子会话的对话
  // 混进主会话历史 —— 正是「TODO 串了」那类 bug 的同构版本）
  const inMain = await page.locator('.narrative .msg.user').allInnerTexts()
  assert(!inMain.some((x) => x.includes('401')), `主会话叙述区不出现子运行的对话（实际 ${JSON.stringify(inMain.map((x) => x.slice(0, 20)))}）`)
  // 量完把皮肤切回探针默认（后面几段按原皮肤量版式/滚动，不受这一段的临时切换影响）
  await page.evaluate(() => document.documentElement.setAttribute('data-skin', 'current'))
  await page.waitForTimeout(200)

  // ---- ⑤b 功能复用：子会话的滚动/贴底来自 Narrative（不是「看着像」而是「就是它」）----
  //
  // 用户 2026-10 的原话是「复用主会话的页面样式**与功能**」。样式能靠同宽同 padding 验，
  // 功能（贴底跟随、能滚动、能翻更早）只能靠真的滚一次才算数 —— 这正是上一版独立实现里
  // 缺失的部分（当时下方一片空白且不能跟随）。
  /* 密度：简洁档（默认）把一次运行的工作过程收成一行「Worked · N calls」——
     40 条工具调用也只占一行，页面永远不超过一屏，这段「真的能滚」就量不到任何东西。
     故这一段按**详细档**（用户可选设置）量：同样的块以逐行形态铺开，才能验到滚动/贴底。
     量完恢复默认，后面几段不受影响。 */
  await page.evaluate(() => {
    const st = window.__store.getState()
    window.__store.setState({ settings: { ...st.settings, narrative_density: 'detailed' } })
  })
  await page.waitForTimeout(200)
  await page.evaluate(() => {
    const st = window.__store.getState()
    st.focusAgent(undefined)
    // 造一个内容超过一屏的子智能体（40 条工具调用）
    window.__dispatch({ event_type: 'agent_start', session_id: 'probe', run_id: 'sub-long',
      parent_run_id: 'main-1', depth: 1, task_id: 'task-long', name: '长转录', content: '长转录' })
    for (let i = 0; i < 40; i++) {
      window.__dispatch({ event_type: 'tool_run_start', session_id: 'probe', run_id: 'sub-long',
        id: `lc${i}`, name: 'bash', arguments: JSON.stringify({ command: `echo line-${i}` }) })
      window.__dispatch({ event_type: 'tool_run_end', session_id: 'probe', run_id: 'sub-long',
        id: `lc${i}`, name: 'bash', result: `line-${i}`, is_error: false })
    }
    window.__dispatch({ event_type: 'agent_end', session_id: 'probe', run_id: 'sub-long',
      parent_run_id: 'main-1', depth: 1, content: '长转录结束' })
  })
  await page.evaluate(() => window.__store.getState().focusAgent({ nodeId: 'task-long', runId: 'sub-long', taskId: 'task-long' }))
  await page.waitForSelector('.narrative .as-hd', { timeout: 10000 })
  await page.waitForTimeout(600)
  const scrolled = await page.locator('.narrative').evaluate((el) => ({
    sh: el.scrollHeight, ch: el.clientHeight, st: Math.round(el.scrollTop),
    jump: !!el.querySelector('.narrative-jump'),
  }))
  if (REPORT) console.log(JSON.stringify(scrolled, null, 2))
  assert(scrolled.sh > scrolled.ch + 100, `子会话内容超过一屏 → 真的可滚动（实际 sh=${scrolled.sh} ch=${scrolled.ch}）`)
  assert(Math.abs(scrolled.st - (scrolled.sh - scrolled.ch)) <= 3,
    `进入子会话即贴底（停在最新一条；实际 ${scrolled.st} / 最大 ${scrolled.sh - scrolled.ch}）`)
  // 上滑 → 「回到底部」按钮出现（Narrative 的手势状态机，子会话同样生效）
  await page.locator('.narrative').evaluate((el) => { el.scrollTop = 0; el.dispatchEvent(new Event('scroll', { bubbles: true })) })
  await page.waitForTimeout(400)
  const awayBtn = await page.locator('.narrative .narrative-jump').count()
  assert(awayBtn === 1, '上滑离开底部 → 出现「回到底部」（与主会话同一套手势状态机）')
  await page.screenshot({ path: path.join(SHOTS, 'center-subsession-scrolled.png') })
  await page.evaluate(() => {
    const st = window.__store.getState()
    window.__store.setState({ settings: { ...st.settings, narrative_density: 'concise' } })
  })
  await page.waitForTimeout(200)

  // ---- ⑥ 右栏退役：任务 tab 不再列任何派生运行 ----
  await page.goto(`${URL_BASE}?host=right`, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('.right, .rtabs', { timeout: 20000 })
  await page.waitForTimeout(400)
  // R07：把右栏真正展开。store 初始化走的是「折叠」路径（实测 rightExpanded=false），折叠态
  // 根本不渲染 .right-body —— 那时下面 ⑥ 的「没有任务卡」是对的但没验到东西，R07 新增的
  // 「链路面板由 provider 渲染」也永远数不到。展开后才是有内容的那棵树。
  if ((await page.locator('.right-body').count()) === 0) {
    await page.evaluate(() => { window.__store.getState().setRightExpanded(true) })
    await page.waitForTimeout(500)
  }
  const rail = {
    cards: await page.locator('.right .agent-card').count(),
    mon: await page.locator('.right .agent-monitor, .right .am-list').count(),
    // 「入口本身没了」才是这次的口径：旧的 tab 按钮（aria-label=任务）必须一个都不剩，
    // 否则「tab 还在、只是空了」会让用户以为这里还有任务总览。
    taskEntries: await page.locator('.rail .rbtn[aria-label="任务"], .rtabs button:has-text("任务")').count(),
    tabs: await page.locator('.rtabs .rtab .rtab-t').allInnerTexts(),
  }
  if (REPORT) console.log(JSON.stringify(rail, null, 2))
  assert(rail.cards === 0, `右栏不再渲染任务卡（子 agent / async_task / 后台工具全在左栏树），实际 ${rail.cards}`)
  assert(rail.mon === 0, '「任务监视器」与「异步任务」列表头一并退役')
  assert(rail.taskEntries === 0, `右栏没有「任务」入口了（tab 已删除），实际 ${rail.taskEntries}`)
  assert(!rail.tabs.some((x) => x.trim() === '任务'), `标签条里不再出现「任务」标签，实际 ${JSON.stringify(rail.tabs)}`)
  await page.screenshot({ path: path.join(SHOTS, 'right-tasks.png') })

  // ---- ⑥b R07：链路面板真的由 provider 渲染（仪器 + 内容两条一起看）----
  //
  // 为什么单靠截图不够：provider 是**副作用注册**，探针页少了 builtin import 时
  // getSurface('activity') 恒 undefined，RightPanel 静默走 `??` 兜底 —— 兜底渲染的就是
  // 同一个 ActivityTab，像素一模一样（这正是 F45 说的「最坏的一种失败：断言全绿但没测到」）。
  // 所以这里断三件事：provider 在注册表里、它的 render 被执行过、面板里真有瀑布图。
  // 变异检验（R07 报告）：把 provider.render 改成返回 null 并保留兜底 → 计数归 0 而像素不变；
  // 改成返回别的元素 → 下面 tr-rail 断言立刻红。两条合起来才说明「渲染源是 provider」。
  const act = await page.evaluate(() => ({
    registered: !!window.__actProvider,
    calls: window.__actRenderCalls,
    rail: document.querySelectorAll('.right .tr-rail').length,
    wf: document.querySelectorAll('.right .wf').length,
  }))
  if (REPORT) console.log(JSON.stringify(act, null, 2))
  assert(act.registered, '仪器：activity provider 已注册（否则下面验的是 RightPanel 的 ?? 兜底）')
  assert(act.calls > 0, `仪器：activity provider 的 render 真的执行过（实际 ${act.calls} 次；0 = 走了兜底）`)
  assert(act.rail === 1, `前置：链路面板渲染出 trace 选择器（渲染源 = ActivityTab，实际 ${act.rail}）`)
  assert(act.wf === 1, `前置：瀑布图在（实际 ${act.wf}）`)

  // ---- ⑦ 待办面板跟随「你在看谁」（右栏是它的唯一宿主）----
  //
  // 三个作用域连成一个「同一块面板、三种输入」的对照：主会话 → 会话清单；子 agent →
  // 它自己的；孙 agent（没有清单）→ 整块不渲染。**第三条是关键**：回退主会话清单会让
  // 「它没有计划」显示成「它和主会话一样」，是用户 2026-10 报障的根因。
  const todoOf = async (focus) => {
    // 待办面板只在右栏**展开**形态里渲染（收起时右栏只剩一列图标）。右栏是它的唯一宿主，
    // 故先把右栏展开 —— 这是量面板的前提，不是被测行为。
    await page.evaluate((f) => {
      const st = window.__store.getState()
      st.setRightExpanded(true)
      st.focusAgent(f)
    }, focus)
    await page.waitForTimeout(250)
    return {
      count: await page.locator('.right .todo-panel').count(),
      text: await page.locator('.right .todo-panel').innerText().catch(() => ''),
    }
  }
  // 主会话：清单显示（默认折叠，计数可见）
  const todoMain = await todoOf(undefined)
  assert(todoMain.count === 1 && /0\/2|2\/2/.test(todoMain.text.replace(/\n/g, ' ')),
    `看主会话 → 显示会话清单（2 条），实际 ${todoMain.text.replace(/\n/g, ' ')}`)
  await page.screenshot({ path: path.join(SHOTS, 'right-todo-main.png') })
  // 子 agent：显示它自己的（展开列表读条目文本）
  await page.evaluate(() => window.__store.getState().focusAgent({ nodeId: 'task-1', runId: 'sub-1', taskId: 'task-1' }))
  await page.waitForTimeout(200)
  await page.locator('.right .todo-hd').first().click()
  await page.waitForTimeout(150)
  const subTodo = await page.locator('.right .todo-panel').innerText().catch(() => '')
  assert(subTodo.includes('子计划'), `看子 agent → 显示**它自己的**待办（实际：${subTodo.replace(/\n/g, ' ')}）`)
  assert(!subTodo.includes('主计划甲'), '子 agent 的清单里没有主会话的条目（互不串）')
  await page.screenshot({ path: path.join(SHOTS, 'right-todo-subagent.png') })
  // 孙 agent：没有清单 → 整块不渲染（绝不回退）
  const todoSun = await todoOf({ nodeId: 'task-2', runId: 'sub-2', taskId: 'task-2' })
  assert(todoSun.count === 0, `孙 agent 没有清单 → 面板整块不渲染（不回退主会话），实际 ${todoSun.count}`)

  // ---- ⑧ ask_user 面板：主会话的提问必须真的弹出来（2026-10 回归） ----
  //
  // 我第一版把「主会话」定义成 `runId 为空`，而主 run 的 ask_user 事件带真实 runId
  // → 主会话的提问被自己过滤掉，面板根本不出现。这里在**渲染层**钉住它。
  await page.goto(`${URL_BASE}?host=center`, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('.center, .narrative', { timeout: 20000 })
  await page.evaluate(() => window.__store.getState().focusAgent(undefined))
  await page.evaluate(() => window.__dispatch({
    event_type: 'ask_user_question', session_id: 'probe', id: 'q-main-ask', run_id: 'main-1',
    questions: [{ id: 'q-main-1', question: '主会话提问 —— 面板必须出现' }],
    timeout_ms: 600000,
  }))
  await page.waitForTimeout(300)
  const askMain = {
    panels: await page.locator('.dock').count(),
    text: await page.locator('.dock').innerText().catch(() => ''),
  }
  if (REPORT) console.log(JSON.stringify(askMain, null, 2))
  assert(askMain.panels >= 1, `看主会话 → ask_user 面板必须出现（实际问题数 ${askMain.panels}）`)
  assert(askMain.text.includes('主会话提问'), `面板显示的是主 run 的问题（实际 ${askMain.text.replace(/\n/g, ' ').slice(0, 80)}）`)
  await page.screenshot({ path: path.join(SHOTS, 'center-ask-main.png') })

  // 子 agent 也提问 → 两批并存；看主会话仍显示主的那批（不被子 agent 顶掉）
  await page.evaluate(() => window.__dispatch({
    event_type: 'ask_user_question', session_id: 'probe', id: 'q-sub-ask', run_id: 'sub-1',
    questions: [{ id: 'q-sub-1', question: '子 agent 提问' }],
  }))
  await page.waitForTimeout(250)
  const bothText = await page.locator('.dock').innerText().catch(() => '')
  assert(bothText.includes('主会话提问'), '子 agent 也提问后，主会话那批仍在（不覆盖）')
  assert(!bothText.includes('子 agent 提问'), '子 agent 的提问不显示在主会话面板里')
  // 切去看那个子会话 → 显示它自己的那批
  await page.evaluate(() => window.__store.getState().focusAgent({ nodeId: 'task-1', runId: 'sub-1', taskId: 'task-1' }))
  await page.waitForTimeout(300)
  const subAskText = await page.locator('.center').innerText().catch(() => '')
  assert(subAskText.includes('子 agent 提问'), '切到子会话 → 显示它自己的提问')
  assert(!subAskText.includes('主会话提问'), '子会话里不显示主会话的提问')
  await page.screenshot({ path: path.join(SHOTS, 'center-ask-sub.png') })
  // 收尾：清掉提问（避免影响后续断言）
  await page.evaluate(() => window.__store.setState((s) => ({ questions: [] })))

  // ---- ⑧c 子智能体的产出与「查看变更」（用户 2026-10 报障） ----
  //
  // 两条都属"主会话有、子会话没有"那一类：
  //   a. artifacts：AgentEnd.Artifacts 在 Go 侧本来就归**子运行的卡片**，但前端实时分支
  //      只落了 status/用量 —— 刷新后（checkpoint 带 artifacts）能看到，运行中看不到。
  //   b. 文件变更：右侧文件面板按 toolId 找 operation，而子运行的工具调用只在卡片 items
  //      上、不在 v.blocks 里 → 子会话里点 write/edit 的路径只剩「最终文件」。
  await page.goto(`${URL_BASE}?host=center`, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('.center, .narrative', { timeout: 20000 })
  await page.evaluate(() => window.__dispatch({
    event_type: 'agent_end', session_id: 'probe', run_id: 'sub-3', finish_reason: 'stop',
    duration_ms: 1500, usage: { input: 10, output: 20 },
    artifacts: [
      { name: 'view_reducer.go', path: 'desktop/bridge/view_reducer.go', kind: 'modified', added: 49, removed: 3, size: 130_000, lines: 3554 },
    ],
  }))
  await page.waitForTimeout(200)
  const cardArtifacts = await page.evaluate(() => {
    const b = window.__store.getState().blocks.find((x) => x.kind === 'agent' && x.runId === 'sub-3')
    return b && b.kind === 'agent' ? (b.artifacts ?? []).map((a) => a.path) : null
  })
  assert(Array.isArray(cardArtifacts) && cardArtifacts.includes('desktop/bridge/view_reducer.go'),
    `子运行的产出要挂在它的卡片上（否则子会话 footer 没有 artifacts），实际 ${JSON.stringify(cardArtifacts)}`)

  // 切到那个子会话：footer 里必须真的渲染出产出卡
  await page.evaluate(() => window.__store.getState().focusAgent({ nodeId: 'task-3', runId: 'sub-3', taskId: 'task-3' }))
  await page.waitForTimeout(350)
  // 必须是**主会话那张产出卡**（.msg.artifacts > .artifacts-card），不是卡片内的 .ac-artifacts
  // —— 后者带 border-top 与卡片内边距，放进叙述区会矮一截（用户一眼看出不一致）。
  const footerArt = await page.locator('.center .msg.artifacts .artifacts-card').count()
  const footerText = await page.locator('.center .msg.artifacts').innerText().catch(() => '')
  const cardInnerArt = await page.locator('.center .ac-artifacts').count()
  assert(footerArt >= 1, `子会话页要渲染产出卡（与主会话同一个 .msg.artifacts 块），实际 ${footerArt}`)
  assert(cardInnerArt === 0, `产出卡不该用卡片内那套外壳（.ac-artifacts 会矮一截），实际 ${cardInnerArt}`)
  assert(/view_reducer\.go/.test(footerText), `产出卡里是本轮那个文件（实际 ${footerText.replace(/\n/g, ' ').slice(0, 60)}）`)
  await page.screenshot({ path: path.join(SHOTS, 'center-sub-artifacts.png') })

  // 子智能体里的一次 write_file（带 diff）→ 文件活动聚合必须看得到它
  await page.evaluate(() => {
    window.__dispatch({
      event_type: 'tool_run_start', session_id: 'probe', run_id: 'sub-3', id: 'w-call-1',
      name: 'write_file', arguments: JSON.stringify({ path: 'src/auth.go', content: 'x' }),
    })
    window.__dispatch({
      event_type: 'tool_run_end', session_id: 'probe', run_id: 'sub-3', id: 'w-call-1',
      name: 'write_file', result: 'ok',
      diff: { path: 'src/auth.go', added: 3, removed: 1, unified: '@@ -1,2 +1,4 @@\n-old\n+new1\n+new2\n+new3' },
    })
  })
  await page.waitForTimeout(250)
  const agg = await page.evaluate(() => window.__fileActivity(window.__store.getState().blocks))
  const authEntry = agg.find((a) => a.path === 'src/auth.go')
  assert(Boolean(authEntry), `子智能体的写文件要进文件活动（否则右侧点开没有变更），实际 ${JSON.stringify(agg.map((a) => a.path))}`)
  assert((authEntry?.operations ?? []).some((op) => op.toolId === 'w-call-1'),
    `变更条目要带着那次调用的 toolId（右侧按它定位本次变更），实际 ${JSON.stringify(authEntry?.operations?.map((o) => o.toolId))}`)
  assert(authEntry?.changed === true, '写过的文件在文件面板里是「已修改」态（与主会话一致）')

  // ---- ⑨ 主会话清单没被子 agent 污染（store 层） ----
  const todosNow = await page.evaluate(() => window.__store.getState().todos)
  assert(
    todosNow.length === 2 && todosNow[0].title === '主计划甲' && todosNow[1].title === '主计划乙',
    `会话清单仍是主 agent 的两条（子 agent 的 todo_* 没覆盖它），实际 ${JSON.stringify(todosNow)}`,
  )

  // ---- ⑧b 刷新后左栏树不能空（用户 2026-10 报障） ----
  //
  // 「我的子智能体左侧侧边栏怎么看不到了？？？刷新后近期的也看不到了」。
  // 真因：树是从 blocks 投影出来的，而打开长会话只回最近 200 块 —— 卡片全在窗口之外。
  // 这里不手搓事件流，而是喂一份**和 bridge 一模一样**的刷新响应：首屏 2 块 +
  // blocks_total=520 + agents 里两张窗口外卡片。
  await page.goto(`${URL_BASE}?host=left&refresh=1`, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('.agent-tree .atree-row', { timeout: 20000 })
  await page.waitForTimeout(200)
  const afterRefresh = await page.locator('.agent-tree .atree-row').evaluateAll((els) =>
    els.map((el) => ({
      label: el.querySelector('.atree-label')?.textContent ?? '',
      level: Number(el.getAttribute('aria-level') ?? 0),
      task: el.getAttribute('data-task') ?? '',
    })))
  if (REPORT) console.log(JSON.stringify(afterRefresh, null, 2))
  assert(afterRefresh.length === 2,
    `刷新后窗口外的 2 张卡片仍要画进树（实际 ${afterRefresh.length} 行：${afterRefresh.map((r) => r.label).join(' / ') || '空'}）`)
  assert(afterRefresh.some((r) => r.label === '历史子智能体' && r.level === 1), '子智能体在根层')
  assert(afterRefresh.some((r) => r.label === '历史孙智能体' && r.level === 2),
    '孙智能体的父子关系也恢复（agents 里的 parent_run_id 串起来了）')
  assert(afterRefresh.every((r) => r.task.startsWith('task-hist-')),
    '行上带 task_id（点开时按它去 journal 取首屏）')
  await page.screenshot({ path: path.join(SHOTS, 'left-tree-after-refresh.png') })

  // 点开其中一张：子会话页要出现（而不是「该子智能体已不在当前会话中」），
  // 并且**自动补拉首屏**（骨架 items 为空 → 不补拉就是一页空白）。
  await page.locator('.agent-tree .atree-row', { hasText: '历史子智能体' }).first().click()
  await page.waitForTimeout(300)
  const focusNow = await page.evaluate(() => window.__store.getState().focusedAgent)
  assert(focusNow && focusNow.taskId === 'task-hist-1', `点开窗口外卡片 → 进入它的子会话（实际 ${JSON.stringify(focusNow)}）`)
  // 子会话视图本身（另一处宿主）：头部要有它自己的名字，不是「已不在会话中」，
  // 且打开时**自动补拉首屏**（骨架 items 为空 → 不补拉就是一页空白）。
  //
  // 补拉只在子会话视图挂载时发生（左栏点一下只是切 focus，视图在对话区渲染），
  // 所以这条必须在 center 宿主上验。
  await page.goto(`${URL_BASE}?host=center&refresh=1`, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('.center, .narrative', { timeout: 20000 })
  await page.evaluate(() => window.__store.getState().focusAgent({ nodeId: 'task-hist-1', runId: 'sub-hist-1', taskId: 'task-hist-1' }))
  await page.waitForTimeout(400)
  const calls = await page.evaluate(() => window.__historyCalls)
  assert(calls.length === 1 && calls[0].taskId === 'task-hist-1',
    `骨架卡片打开时自动补拉首屏（task_id 正确），实际 ${JSON.stringify(calls)}`)
  const subPage = await page.locator('.center').innerText().catch(() => '')
  assert(subPage.includes('历史子智能体'), `子会话页显示这个子智能体的名字（实际 ${subPage.replace(/\n/g, ' ').slice(0, 80)}）`)
  assert(!subPage.includes('已不在当前会话中'), '窗口外的卡片也要被认作「还在会话里」')
  // 反面：窗口外卡片**不得**混进主会话的对话流（它们不是「已加载的历史」）
  await page.evaluate(() => window.__store.getState().focusAgent(undefined))
  await page.waitForTimeout(250)
  const mainCards = await page.locator('.narrative .agent-card').count()
  assert(mainCards === 0, `窗口外卡片不进主对话流（否则会话开头凭空多几张旧卡），实际 ${mainCards} 张`)
  await page.screenshot({ path: path.join(SHOTS, 'center-after-refresh.png') })

  await browser.close()
  console.log(`\n✅ ${pass} 条断言全通过；截图在 ${SHOTS}`)
  // 显式收尾：vite 子进程 + watchdog 会让进程悬着（CI 里表现为「测试过了但超时」）
  cleanup()
  process.exit(0)
}

main().catch((e) => {
  console.error(`\n❌ ${e.message}`)
  cleanup()
  process.exit(1)
})
