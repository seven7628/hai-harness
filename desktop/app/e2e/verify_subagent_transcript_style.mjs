#!/usr/bin/env node
// 子 agent 转录样式一致性回归（2026-09-23 用户报障）
//
// 现象：右侧栏「任务」tab 里异步子 agent 卡片展开后的内容，与主会话长得不一样。
// 根因不是「漏了一条样式」，而是转录自带了另一套精简渲染 —— 同一份 AgentItem[]
// 在主对话卡里走 Markdown + .tool-group/.tool-row，在转录里却是：
//   正文 = 裸文本 .at-text（11.5px、pre-wrap、不走 Markdown：列表/行内码都不渲染）
//   工具行 = .at-tool（11px、恒 accent 色、无 data-family、无行首图标、每行独立边框）
// 更糟的是转录的字体/颜色靠**继承宿主**（.agent-card 是 mono + text-2，右栏卡当时是
// 是 sans + text）→ 同一份数据在两处连字号字体色都不一样；V3 皮肤又只覆盖主对话那套。
//
// 本脚本用真实 CenterPanel + 真实两份 CSS，把两处转录的类名结构与
// 计算样式逐项对比（current / v3 两套皮肤），并留档截图。
//
// **2026-10 宿主变更**：第二处宿主从「右栏任务 tab」改成「对话区的子会话视图」
// （AgentSessionView —— 左栏树点选某个子智能体后在叙述区渲染它）。右栏任务 tab 已退役为
// 审阅台（不再列子 agent 卡）。因此本脚本的 host=panel 现在是**子会话视图**，
// 断言内容（同一份 DOM / 同一套计算样式）不变 —— 这正是本次迁移要保住的性质：
// 子 agent 的转录无论出现在哪个宿主，都必须与主对话同构。
//
// 用法：cd desktop/app && node e2e/verify_subagent_transcript_style.mjs
//      REPORT=1 node e2e/verify_subagent_transcript_style.mjs   # 只取证不判（复现用）
// 退出码：全通过 0；任一断言失败非 0。
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
const SHOTS = path.join(__dirname, '_subagent_shots')
const REPORT = process.env.REPORT === '1' // 只量不判（复现/取证用）

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

// 临时探针页：真实 CenterPanel（宿主 ① 主对话）或 RightPanel（宿主 ② 右栏任务 tab），跑完删除。
// 数据不是手搓 blocks，而是**重放真实事件链**（agent_start → llm_start/思考/正文 → 工具
// → agent_end，字段形状照 mock/bridge 原样）—— 两处渲染的是生产同一条数据链路。
const PROBE_DIR = path.join(__dirname, '_subagentprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh"><head><meta charset="utf-8"><title>subagent transcript probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import CenterPanel from '../../src/components/CenterPanel'
import { useAppStore, dispatchEvent } from '../../src/store/useAppStore'
import '../../src/index.css'
import '../../src/narrative-skin.css'

const q = new URLSearchParams(location.search)
document.documentElement.setAttribute('data-skin', q.get('skin') === 'v3' ? 'v3' : 'current')
document.documentElement.setAttribute('data-theme', q.get('theme') || 'ink')
const HOST = q.get('host') === 'panel' ? 'panel' : 'main'

const SID = 'probe'
// 密度固定「详细」：简洁档（默认）把思考/工具折进一行「Worked · N calls」，
// 这一段量的正是**逐行**的转录渲染（.thinking/.tool-row/.md-text），折叠态下量不到。
useAppStore.setState((s) => ({ settings: { ...s.settings, narrative_density: 'detailed' } }))
useAppStore.setState({
  workspace: '/mock/proj-auth',
  activeSessionId: SID,
  bridgeStatus: 'running',
  sessions: [{ id: SID, title: '子 agent 转录' }],
  projects: [{ path: '/mock/proj-auth', sessions: [{ id: SID, title: '子 agent 转录' }] }],
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

// 一个主 run 派一个子 agent：转录含 思考 → 正文（Markdown 列表/行内码）→ 两次工具 → 正文
// 事件时间戳可指定偏移（毫秒）：一轮的「TTFT / LLM 总耗时 / tokens/s」都由事件时刻
// 差分算出来，全塞在同一个 Date.now() 里这些数字就恒为 0，元信息行看不出对不对。
const T0 = Date.now() - 120_000
const ev = (o, at = 0) => dispatchEvent({ session_id: SID, timestamp: T0 + at, ...o })
ev({ event_type: 'user_inputs_consumed', user_texts: ['修复 auth 过期 token 被吞成 500'] })
ev({ event_type: 'agent_start', run_id: 'main-1', index: 0, content: '处理用户输入', model: 'deepseek-v4' })
ev({ event_type: 'agent_start', run_id: 'sub-1', parent_run_id: 'main-1', depth: 1, task_id: 'a1', content: 'subagent · 审查 auth 改动' })
ev({ event_type: 'llm_start', run_id: 'sub-1', request_id: 'r-sub-1', model: 'deepseek-v4' }, 5_000)
ev({ event_type: 'reasoning_chunk', run_id: 'sub-1', request_id: 'r-sub-1', content: '先读 auth.go 确认错误分支，再看 auth_test.go 现有覆盖。' }, 5_400)
ev({ event_type: 'content_chunk', run_id: 'sub-1', request_id: 'r-sub-1', content: '结论先给：\\n\\n1. \`errors.Is(err, ErrUnauthorized)\` 漏掉 JWT 过期分支\\n2. 过期 token 落入通用 500，应单独映射 401\\n' }, 6_400)
ev({ event_type: 'llm_end', run_id: 'sub-1', request_id: 'r-sub-1', model: 'deepseek-v4', finish_reason: 'tool_call', usage: { Input: 1120, Output: 74, CacheRead: 1000, TotalTokens: 1194 } }, 7_400)
ev({ event_type: 'tool_run_start', run_id: 'sub-1', id: 'call_1', name: 'bash', arguments: '{"command":"cd /path/to/workspace && grep -rn ErrTokenExpired ."}' })
ev({ event_type: 'tool_run_end', run_id: 'sub-1', id: 'call_1', name: 'bash', result: 'auth/login.go:42: jwt.ErrTokenExpired,', is_error: false })
ev({ event_type: 'tool_run_start', run_id: 'sub-1', id: 'call_2', name: 'read_file', arguments: '{"path":"auth.go","start_line":1,"end_line":200}' })
ev({ event_type: 'tool_run_end', run_id: 'sub-1', id: 'call_2', name: 'read_file', result: 'package auth', is_error: false })
ev({ event_type: 'content_chunk', run_id: 'sub-1', request_id: 'r-sub-1', content: '审查通过：401/500 边界正确。' })
ev({ event_type: 'agent_end', run_id: 'sub-1', parent_run_id: 'main-1', depth: 1, content: 'subagent · 审查 auth 改动', finish_reason: 'end', model: 'deepseek-v4' })
// 任务结果（自动回传）：**Markdown** 结果 —— 两处宿主必须同一形态（此前右栏是 mono <pre>）
ev({ event_type: 'task_end', task_id: 'a1', run_id: 'main-1', result: '**审查通过**：401/500 边界正确\\n\\n- 过期分支单独映射 401\\n- 其余仍回 500', usage: { Input: 940, Output: 70, CacheRead: 860, TotalTokens: 1010 } })
ev({ event_type: 'task_result_delivered', task_id: 'a1', result: '**审查通过**：401/500 边界正确\\n\\n- 过期分支单独映射 401\\n- 其余仍回 500', usage: { Input: 940, Output: 70, CacheRead: 860, TotalTokens: 1010 } })
// 后台工具任务（promote）：子会话视图里的工具任务展开体应与主对话工具行同一套
ev({ event_type: 'tool_run_start', run_id: 'main-1', id: 'call_9', name: 'bash', arguments: '{"command":"sleep 30"}', task_id: 'tooltask-1' })
ev({ event_type: 'task_promoted', task_id: 'tooltask-1', kind: 'tool', name: 'bash' })
ev({ event_type: 'tool_run_end', run_id: 'main-1', id: 'call_9', name: 'bash', result: 'sleep done (30s)', is_error: false })

// 事件会把 view 的镜像字段同步回顶层 → 视图状态最后写（setView 的同步链会拿 view 里的值覆盖顶层）
{
  const st = useAppStore.getState()
  const v = st.views[SID]!
  useAppStore.setState({
    views: { ...st.views, [SID]: { ...v, rightExpanded: true, rightTab: 'activity' } },
    rightExpanded: true,
    rightTab: 'activity',
    rightSize: 460,
  })
}

// host=panel（第二处宿主）：聚焦到子 agent → 叙述区渲染 AgentSessionView。
// 2026-10 起右栏「任务」tab 已删除 —— 子会话视图就是子智能体转录的**唯一**宿主，
// 「两处宿主同构」这条性质改为「主对话 vs 子会话视图」。
// 必须在 setState 之后同步设置，保证首帧就是子会话视图。
if (HOST === 'panel') {
  window.__store = useAppStore
  useAppStore.getState().focusAgent({ nodeId: 'a1', runId: 'sub-1', taskId: 'a1' })
}
createRoot(document.getElementById('root')!).render(<CenterPanel />)

`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_subagentprobe_run/index.html`
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

// 量测：取计算样式（「看起来一样」的可判定化）
const css = (locator, props) => locator.evaluate((el, ps) => {
  const s = getComputedStyle(el)
  const out = {}
  for (const p of ps) out[p] = s.getPropertyValue(p)
  return out
}, props)
const TEXT = ['font-size', 'line-height', 'color', 'font-family']
const ROW = ['font-size', 'font-family', 'color']
const ARGS = ['font-size', 'color']

// 打开某个宿主页 → 展开子 agent 卡 → 返回转录内的结构与计算样式
async function probe(page, skin, host) {
  await page.goto(`${PROBE_URL}?skin=${skin}&host=${host}`, { waitUntil: 'domcontentloaded' })
  if (host === 'panel') {
    // 子会话视图 = **主会话的 Narrative 本身**（2026-10 用户拍板「复用主会话页面」），
    // 没有独立的转录外壳 —— 锚点是它注入的头部行 .as-hd。
    await page.waitForSelector('.narrative .as-hd', { timeout: 20000 })
  } else {
    await page.waitForSelector('.narrative .agent-card', { timeout: 20000 })
    await page.locator('.narrative .agent-card .ac-main').first().evaluate((el) => el.click())
  }
  const tr = host === 'panel' ? '.narrative' : '.agent-card .ac-body'
  await page.waitForSelector(tr, { timeout: 5000 })
  await page.waitForTimeout(300)

  const loc = page.locator(tr).first()
  const first = async (sel, ps) => (await loc.locator(sel).count()) ? css(loc.locator(sel).first(), ps) : null
  const survey = {
    atText: await loc.locator('.at-text').count(),
    atTool: await loc.locator('.at-tool').count(),
    mdText: await loc.locator('.md-text').count(),
    mdList: await loc.locator('.md-text ol li, .md-text ul li').count(),
    toolRow: await loc.locator('.tool-row').count(),
    toolGroup: await loc.locator('.tool-group').count(),
    family: await loc.locator('.tool-row[data-family]').count(),
    icon: await loc.locator('.tool-row .ic').count(),
    thinking: await loc.locator('.thinking').count(),
    shell: await page.locator('.agent-card').count(),          // 任务卡外壳（两处同一份实现）
    legacyShell: await page.locator('.agent-view, .av-hd, .av-body, .av-sec, .av-st').count(), // 旧卡壳退役
    // 交付结果（2026-10 起的口径）：**不再是一张页脚卡**（.task-result-inline 已退役，
    // 结果本身就是转录最后一条 assistant 正文）。故这里量两件事：
    //   legacyResultCard = 旧页脚卡是否复活（必须 0）；
    //   resultMd = 交付正文是否走 Markdown（.md-text 里能读到那两行列表）。
    legacyResultCard: await page.locator('.task-result-inline').count(),
    resultMd: await loc.locator('.md-text ul li, .md-text ol li').count(),
    taskCards: await page.locator('.agent-card:has(.task-promoted)').count(),
    // 一条消息的元信息行（2026-10 用户报障「子智能体没有模型信息、消息时刻、
    // 也没有 LLM 总耗时 · Agent 总耗时 · tokens/s」）—— 子会话必须与主会话一样有三段：
    // .who 里的模型名、.who 里的时刻、.body 里的 .assistant-turn-meta。
    msgModel: await loc.locator('.msg.assistant .who .mono').count(),
    msgTs: await loc.locator('.msg.assistant .who .msg-ts').count(),
    turnMeta: await loc.locator('.msg.assistant .assistant-turn-meta').count(),
    turnMetaText: await loc.locator('.msg.assistant .assistant-turn-meta').first().textContent().catch(() => null),
  }
  const shellCard = page.locator('.narrative .agent-card').first()
  const styles = {
    text: await first('.at-text, .md-text', TEXT),
    // 卡壳只在主对话存在（panel = 子会话视图，没有 .ac-main/.ag-sec）
    header: (await shellCard.locator('.ac-main').count()) ? await css(shellCard.locator('.ac-main').first(), ['font-size', 'font-family', 'padding', 'gap', 'color']) : null,
    sec: (await shellCard.locator('.ag-sec').count()) ? await css(shellCard.locator('.ag-sec').first(), ['font-size', 'color', 'background-color', 'padding']) : null,
    row: await first('.at-tool, .tool-row', ROW),
    args: await first('.at-tool .args, .tool-row .args', ARGS),
    name: await first('.at-tool .t, .tool-row .t', ROW),
    family: await loc.locator('.at-tool, .tool-row').first().getAttribute('data-family', { timeout: 3000 }).catch(() => null),
  }
  // 后台工具任务的展开体（原右栏工具任务卡）：现在只在它的子会话视图里出现 ——
  // 由 e2e/verify_agent_tree.mjs 覆盖（那里断言 .tr-sec/.json-box/pre.tr-code）。
  const taskCard = null
  const shot = path.join(SHOTS, `${skin}-${host}.png`)
  await page.screenshot({ path: shot })
  return { survey, styles, shot, taskCard }
}

async function main() {
  fs.rmSync(SHOTS, { recursive: true, force: true })
  fs.mkdirSync(SHOTS, { recursive: true })
  await waitPort(PORT)
  // 端口可连只说明**有东西**在监听；再确认这张探针页真能开（子进程退出即失败）
  await waitHttp(PROBE_URL, { child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  page.on('pageerror', (e) => console.error('[pageerror]', e.message))

  const got = {}
  for (const skin of ['current', 'v3']) {
    console.log(`\n── 皮肤 ${skin} ──`)
    for (const host of ['main', 'panel']) {
      const r = await probe(page, skin, host)
      got[`${skin}.${host}`] = r
      console.log(`  ${host.padEnd(5)} 结构 ${JSON.stringify(r.survey)}`)
      console.log(`  ${' '.repeat(5)} 正文 ${JSON.stringify(r.styles.text)}`)
      console.log(`  ${' '.repeat(5)} 工具行 ${JSON.stringify(r.styles.row)}`)
      console.log(`  ${' '.repeat(5)} 工具参数 ${JSON.stringify(r.styles.args)}`)
    }
  }
  console.log(`\n  📸 截图：${path.relative(APP, SHOTS)}/`)

  if (!REPORT) {
    for (const skin of ['current', 'v3']) {
      console.log(`\n── 断言（皮肤 ${skin}）──`)
      const main = got[`${skin}.main`]
      const panel = got[`${skin}.panel`]
      assert(main.survey.atText === 0 && panel.survey.atText === 0 && main.survey.atTool === 0 && panel.survey.atTool === 0,
        '两处转录都不再有 .at-* 旧渲染（同一份 DOM）')
      assert(main.survey.mdText > 0 && panel.survey.mdText > 0, '两处转录的正文都是 .md-text（走 Markdown）')
      assert(main.survey.mdList > 0 && panel.survey.mdList > 0, `正文里的有序列表渲染成 <ol><li>（不是裸文本，${panel.survey.mdList} 项）`)
      assert(main.survey.toolRow === panel.survey.toolRow && main.survey.toolRow >= 2, `两处工具行数量一致（${panel.survey.toolRow} 行）`)
      assert(main.survey.toolGroup > 0 && panel.survey.toolGroup > 0, '连续工具调用在两处都归组在 .tool-group 里')
      assert(main.survey.family === main.survey.toolRow && panel.survey.family === panel.survey.toolRow, '每行都带 data-family（族色同源）')
      assert(main.survey.thinking > 0 && panel.survey.thinking > 0, '两处思考块都用 .thinking')
      assert(main.survey.icon === panel.survey.icon, `行首图标 .ic 数量一致（V3 皮肤下渲染，本皮肤 ${main.survey.icon} 个）`)
      assert(panel.styles.family === 'cmd', `bash 行按族着色（data-family=${panel.styles.family}）`)
      // 交付结果不再是一张页脚卡：它已经就是转录最后一条正文（见 AgentSessionView 注释）。
      // 两处都不能复活 .task-result-inline，且交付正文必须是 Markdown 列表（不是 mono <pre>）。
      assert(main.survey.legacyResultCard === 0 && panel.survey.legacyResultCard === 0,
        '任务结果页脚卡已退役（两处 .task-result-inline 均为 0）')
      assert(main.survey.resultMd > 0 && panel.survey.resultMd > 0,
        `交付结果两处都渲染成 Markdown 列表（不再是 mono <pre>，各 ${panel.survey.resultMd} 项）`)
      // 2026-10：第二处宿主是子会话视图（无卡壳）。旧 .agent-view/.av-* 已全仓退役。
      assert(main.survey.legacyShell === 0 && panel.survey.legacyShell === 0,
        '旧 .agent-view/.av-* 卡壳全仓退役')
      assert(main.survey.shell > 0, '主对话仍有子 agent 卡（.agent-card）')
      assert(panel.survey.shell === 0, '子会话视图不再套一层卡壳（转录直接铺在视图里）')
      // 每条消息的元信息行：模型名 / 时刻 / 「TTFT · LLM 总耗时 · Agent 总耗时 · tokens/s」。
      // 只断言子会话这一侧 —— 参考侧的「主会话消息」在本探针里没有（main 宿主渲染的是
      // 子 agent 卡片的转录，主 run 只发了 agent_start、没有 LLM 轮），而那套渲染原语
      // 就是 Narrative 自己（同一个组件），不是另一个实现。
      assert(panel.survey.msgModel > 0,
        `子会话的消息显示模型名（${panel.survey.msgModel} 条）`)
      assert(panel.survey.msgTs > 0,
        `子会话的消息显示时刻（${panel.survey.msgTs} 条）`)
      assert(panel.survey.turnMeta > 0,
        `子会话有本轮指标行（${panel.survey.turnMeta} 条）`)
      assert(/tokens\/s|tok\/s/.test(panel.survey.turnMetaText ?? ''),
        `子会话的指标行含 tokens/s（实际「${panel.survey.turnMetaText}」）`)
      assert(/TTFT/.test(panel.survey.turnMetaText ?? '') && /总耗时/.test(panel.survey.turnMetaText ?? ''),
        `子会话的指标行含 TTFT 与总耗时（实际「${panel.survey.turnMetaText}」）`)
      for (const [label, key] of [['正文', 'text'], ['工具行', 'row'], ['工具参数', 'args'], ['工具名', 'name']]) {
        assert(JSON.stringify(main.styles[key]) === JSON.stringify(panel.styles[key]),
          `${label}计算样式一致：${JSON.stringify(panel.styles[key])}`)
      }
    }
  }

  await browser.close()
  cleanup()
  console.log(REPORT
    ? '\n[e2e] 取证完成（REPORT=1，未断言）'
    : `\n[e2e] 子 agent 转录样式一致性全部通过（${pass} 项断言）`)
}

main().catch((e) => { console.error('  ❌ E2E 失败', e); cleanup(); process.exit(1) })
