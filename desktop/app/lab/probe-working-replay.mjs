// 探针：长时运行会话「切回来时 Working 盒回放 / 卡顿」的复现与量化。
//
// 复现的形态（用户 2026-11 报障）：
//   · 同时有若干跑了 30~60min 的会话（它们的最后一个 run 一直 live）
//   · 在这些会话之间切换（包括子智能体 Session）
//   · 每次切换，Working 盒里的 message block 会从头「铺」一遍：先只有尾部 25 块，
//     然后每帧补 25 块；盒内滚动在「顶部 ↔ 底部」之间来回跳
//
// 本探针不需要真 bridge：直接 __hydrate 一份「一次运行、N 块」的 checkpoint（live=true），
// 再用 setActiveSession 走缓存命中路径（与真实切会话同一段代码）。
//
// 用法（先起一个独立的 dev server，别占用正在用的那个）：
//   cd desktop/app && VITE_PORT=5288 npx vite
//   npm run probe:working-replay          # 主会话：默认 vs 无 reveal 对照
//   MODE=sub npm run probe:working-replay # 子智能体 Session：A/B 切换
//   N=1600 npm run probe:working-replay   # 加大历史
//
// 观察点：
//   · 「回放结束」= Working 盒内的 .msg 数不再增长（静默 1.2s）
//   · longtask / >50ms 帧 = 主线程卡顿
//   · top 跳动 = 盒内滚动位置在回放期间反复从 0 跳到 max
//   · 无 reveal（enterAnimAt=0）= 不铺开的对照；修复后它和默认都应该只有 1 次重挂
//
// 预期（M-series Mac，Chrome headless，1440×900）：
//   HEAD（未修）N=800 合成 ≈ 9~13s / 7~12s longtask；真实 record ≈ 4.8s / 3.1s longtask
//   修完（turn 对齐 + cap 120 + 盒内不逐行入场）≈ 1.3~1.4s（含下面固定的 1.2s 静默等待）
//   / 1 个 longtask ≈ 150ms / 0 个 >50ms 帧 / 1 次重挂
//   注意：cap 生效时 DOM 里只有尾部 120 块；隐藏块由盒内「本次运行更早的记录」按页补。
//   FP=1 的指纹只覆盖当前渲染窗口（跑到 hidden=0 才是全量）。
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const MODE = process.env.MODE || 'compare'
const N = Number(process.env.N || 800)
const ITEMS = Number(process.env.ITEMS || 600)
// CP_PATH：用真实会话的 view_checkpoint（由 session 侧车解析出的 JSON）代替合成夹具。
const CP_PATH = process.env.CP_PATH || ''
const wait = (ms) => new Promise((r) => setTimeout(r, ms))

const b = await Browser.launch({ width: 1440, height: 900 })
const page = await b.newPage({ width: 1440, height: 900 })
await page.goto(URL, { settleMs: 5000 })
page.send('Performance.enable').catch(() => {})

// page.eval 的 30s 上限不够：N=800 的回放本身可到 13s，加上长任务会顶到超时。
const ev = async (fn, arg, timeout = 180000) => {
  const expression = '(' + fn.toString() + ')(' + JSON.stringify(arg) + ')'
  const r = await page.send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true }, timeout)
  if (r.exceptionDetails) throw new Error(r.exceptionDetails.text || 'eval error')
  return r.result?.value
}
const waitUntil = async (expr, timeout = 30000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval(new Function('return (' + expr + ')'))) return true
    await wait(100)
  }
  return false
}
await waitUntil('!!window.__store && !!window.__hydrate')
await page.eval(() => {
  document.documentElement.dataset.skin = 'runtime'
  window.__store.getState().setNarrativeDensity('concise')
})
await wait(300)

// —— 夹具：一次运行（run-live）从第一条 user 消息一路跑到第 N 块 ——
// 交替 assistant / tool，全部同一个 run_id；末尾没有交卷（run_nodes.status=running）。
const oneRunCp = (sid, n, salt = 0) => {
  const t0 = Date.now() - 3600_000
  const blocks = [{ kind: 'user', id: 1, text: `[${sid}] 把这个仓库的 auth 模块从登录到刷新令牌全部过一遍。`, ts: t0 }]
  let id = 2, ts = t0 + 1000, k = 0
  while (blocks.length < n) {
    if (blocks.length % 3 === 0) {
      blocks.push({
        kind: 'assistant', id: id++, run_id: 'run-live', ts,
        text: `第 ${k} 段进展：确认第 ${k} 处的错误分支，接着看下一个调用点。`,
        thinking: `先确认第 ${k} 个分支的 sentinel error 与调用方。`,
        think_ms: 900, ttft_ms: 300, dur_ms: 1400,
      })
    } else {
      blocks.push({
        kind: 'tool', id: id++, run_id: 'run-live', tool_id: `t-${k}`, name: k % 2 ? 'read_file' : 'bash',
        args: JSON.stringify({ path: 'auth.go', n: k }), status: 'done',
        result: `第 ${k} 行输出：ok`, dur_ms: 120, started_at: ts,
      })
    }
    ts += 800
    k++
  }
  for (let i = 10; i < blocks.length - 5; i += 37) {
    blocks.splice(i, 0, { kind: 'task_delivery', id: 900000 + salt * 10000 + i, task_id: `bg-${i}`, status: 'completed', after_run: false, ts: t0 + i * 800 })
  }
  return {
    schema_version: 1, blocks,
    run_nodes: [{ id: 'run-live', label: 'main', status: 'running', depth: 0, kind: 'run', started_at: t0 }],
    turns: [], usage: { input: 0, output: 0, reasoning: 0 }, cost_usd: 0, degraded: false,
  }
}

const hydrate = async (sid, cp, live) => {
  await page.eval((a) => window.__hydrate(a.sid, a.cp, undefined, { live: a.live, blocksTotal: a.cp.blocks.length, blocksHasMore: false }), { sid, cp, live })
  await page.eval((a) => {
    const st = window.__store.getState()
    const ws = st.workspace || '/mock/workspace'
    const sessions = [...(st.sessions || []).filter((s) => s.id !== a.sid), { id: a.sid, title: a.sid }]
    window.__store.setState({ workspace: ws, projects: [{ path: ws, name: 'mock', sessions }], sessions })
  }, { sid })
  await wait(60)
}

const perf = async () => {
  try {
    const { metrics } = await page.send('Performance.getMetrics')
    const m = Object.fromEntries(metrics.map((x) => [x.name, x.value]))
    return { layout: m.LayoutDuration || 0, style: m.RecalcStyleDuration || 0, script: m.ScriptDuration || 0, task: m.TaskDuration || 0 }
  } catch { return { layout: 0, style: 0, script: 0, task: 0 } }
}

// —— 采样：从 setActiveSession 的同一帧开始，到 Working 盒内容不再增长 ——
const sampleSwitch = async (doSwitch, label, opts = {}) => {
  const maxMs = opts.maxMs ?? 40000
  const viaFocus = opts.viaFocus === true
  const before = await perf()
  const out = await ev((arg) => new Promise((res) => {
    const long = []
    let po = null
    try {
      po = new PerformanceObserver((l) => { for (const e of l.getEntries()) long.push(Math.round(e.duration)) })
      po.observe({ entryTypes: ['longtask'] })
    } catch { /* 老 Chromium 没有 longtask */ }
    const t0 = performance.now()
    const st = window.__store.getState()
    if (arg.viaFocus) st.focusAgent(arg.focus)
    else st.setActiveSession(arg.to, st.workspace)
    // 对照组：enterAnimAt=0 → revealWindow 的 animate=false → 一次渲染全部。
    // 同一 tick 内写，React 只会提交一次（与真实切换同一帧）。
    if (arg.killReveal) window.__store.setState({ enterAnimAt: 0 })
    const rows = []
    let lastMsg = -1
    let lastChange = 0
    const tick = () => {
      const live = document.querySelector('.cy-live')
      const body = live ? live.querySelector('.rt-work-body') : null
      const msg = body ? body.querySelectorAll('.msg').length : 0
      const t = Math.round(performance.now() - t0)
      if (msg !== lastMsg) { lastMsg = msg; lastChange = t }
      rows.push({
        t, msg,
        top: body ? Math.round(body.scrollTop) : 0,
        max: body ? Math.round(body.scrollHeight - body.clientHeight) : 0,
        anims: document.getAnimations ? document.getAnimations().length : -1,
        // 每次换 key 都会重建这个节点：用它数「回放期间 Working 盒被重挂了几次」。
        live: live ? (live.__probeMark || (live.__probeMark = Math.random().toString(36).slice(2, 6))) : null,
      })
      const settled = t > 500 && t - lastChange > 1200
      if ((t < arg.maxMs && !settled) || rows.length < 8) requestAnimationFrame(tick)
      else { if (po) po.disconnect(); res({ rows, long }) }
    }
    requestAnimationFrame(tick)
  }), { to: opts.to, focus: opts.focus, viaFocus, killReveal: opts.killReveal === true, maxMs })
  const after = await perf()

  const rows = out.rows
  const last = rows[rows.length - 1]
  const gaps = rows.slice(1).map((x, i) => x.t - rows[i].t)
  const jumps = rows.slice(1).map((x, i) => (x.max > 0 ? Math.abs(x.top - rows[i].top) : 0)).filter((d) => d > 50)
  const remounts = new Set(rows.map((r) => r.live).filter(Boolean)).size
  console.log(`\n[${label}] 回放结束 ${last.t}ms  frames=${rows.length}  max frame=${Math.max(...gaps)}ms  >50ms=${gaps.filter((g) => g > 50).length}`)
  console.log(`  Working 盒: .msg ${rows[0].msg} → ${last.msg}   盒高 ${last.max + (last.max ? 0 : 0)}px(内容)  top 跳动>50px=${jumps.length} 次（max ${Math.max(0, ...jumps)}px）`)
  console.log(`  重挂次数（.cy-live 节点身份数）=${remounts}   动画数首帧=${rows[0].anims}`)
  console.log(`  longtask ${out.long.length} 个 / ${out.long.reduce((a, c) => a + c, 0)}ms（max ${Math.max(0, ...out.long)}ms）`)
  console.log(`  Δ layout=${Math.round((after.layout - before.layout) * 1000)}ms style=${Math.round((after.style - before.style) * 1000)}ms script=${Math.round((after.script - before.script) * 1000)}ms task=${Math.round((after.task - before.task) * 1000)}ms`)
  console.log(`  进度: ${rows.filter((_, i) => i % 5 === 0).slice(0, 12).map((x) => `${x.t}:${x.msg}m/${x.top}top`).join(' ')}`)
}

if (MODE === 'sub') {
  // 子智能体 Session：两张 agent 卡各带 ITEMS 条活动记录；点进 A、再切到 B。
  // 子会话页与主会话页是同一个 Narrative，身份键是 sourceKey（子 agent 的 run_id），
  // 所以切换同样会重新决定 reveal 窗口 → 同样回放。
  const mkItems = (rid) => {
    const a = []
    const t0 = Date.now() - 1800000
    let k = 0
    while (a.length < ITEMS) {
      if (a.length % 3 === 0) a.push({ kind: 'thinking', text: `确认第 ${k} 个分支。`, runId: rid, ts: t0 + k * 800, thinkMs: 900 })
      else if (a.length % 3 === 1) a.push({ kind: 'text', text: `第 ${k} 段进展：确认错误分支，接着看下一个调用点。`, runId: rid, ts: t0 + k * 800, durMs: 1400 })
      else a.push({ kind: 'tool', toolId: `t-${k}`, name: k % 2 ? 'read_file' : 'bash', args: '{}', status: 'running', result: `ok ${k}`, durMs: 120, startedAt: t0 + k * 800, runId: rid })
      k++
    }
    return a
  }
  const cp = {
    schema_version: 1,
    blocks: [
      { kind: 'user', id: 1, text: '跑一个长任务', ts: Date.now() - 3600000 },
      { kind: 'agent', id: 2, run_id: 'run-A', label: 'Explore A', status: 'running', spawned_at: Date.now() - 1800000, task_id: 'task-A', items: mkItems('run-A') },
      { kind: 'agent', id: 3, run_id: 'run-B', label: 'Explore B', status: 'running', spawned_at: Date.now() - 1700000, task_id: 'task-B', items: mkItems('run-B') },
    ],
    run_nodes: [{ id: 'main-run', label: 'main', status: 'running', depth: 0, kind: 'run' }],
    turns: [], usage: { input: 0, output: 0, reasoning: 0 }, cost_usd: 0, degraded: false,
  }
  await hydrate('sub-1', cp, true)
  await page.eval(() => window.__store.getState().setActiveSession('sub-1', window.__store.getState().workspace))
  await wait(1200)
  const focus = (w) => (w === 'A'
    ? { nodeId: 'task-A', runId: 'run-A', taskId: 'task-A' }
    : { nodeId: 'task-B', runId: 'run-B', taskId: 'task-B' })
  await page.eval((f) => window.__store.getState().focusAgent(f), focus('A'))
  await wait(8000)
  await sampleSwitch(null, `子会话 A → B（${ITEMS} items）`, { viaFocus: true, focus: focus('B') })
  await wait(800)
  await sampleSwitch(null, `子会话 B → A（${ITEMS} items）`, { viaFocus: true, focus: focus('A') })
} else {
  const a = 'replay-a'
  const bSid = 'replay-b'
  // 真实会话：CP_PATH 指向一份 view_checkpoint JSON（blocks 为数组、root run 为 running）。
  // 切到尾部 N 块以内时，切片会落在 live turn 中间 —— 就是用户报障的形态。
  const real = CP_PATH ? JSON.parse(readFileSync(CP_PATH, 'utf8')) : null
  if (real) {
    const blocks = real.blocks || []
    const start = N > 0 && N < blocks.length ? blocks.length - N : 0
    const cp = { ...real, blocks: blocks.slice(start), schema_version: 1 }
    console.log(`real checkpoint: ${CP_PATH} total=${blocks.length} 取尾部 ${cp.blocks.length} 块`)
    await hydrate(a, cp, true)
    await hydrate(bSid, cp, true)
  } else {
    await hydrate(a, oneRunCp(a, N, 1), true)
    await hydrate(bSid, oneRunCp(bSid, N, 2), true)
  }
  await page.eval((sid) => window.__store.getState().setActiveSession(sid, window.__store.getState().workspace), a)
  await wait(10000) // 让 A 自己铺完（否则测的是「铺到一半切走」）
  await sampleSwitch(null, `N=${N} live：默认（reveal + 渐入）`, { to: bSid })
  await wait(1200)
  await sampleSwitch(null, `N=${N} live：enterAnimAt=0（无 reveal 对照）`, { to: a, killReveal: true })
  await wait(1200)
  await sampleSwitch(null, `N=${N} live：再回默认`, { to: bSid })
}

// FP=1：切换稳定后给整段转录做 DOM 指纹（条数 + 文本哈希）。
// 用来证明「铺开优化」没有丢块/换序 —— 修复前后应逐字节一致（盒子折叠态除外）。
if (process.env.FP === '1') {
  await wait(2500)
  const fp = await page.eval(() => {
    const els = [...document.querySelectorAll('.narrative .msg')]
    const text = els.map((e) => e.innerText).join('\u0000')
    let h = 0
    for (let i = 0; i < text.length; i++) h = (Math.imul(h, 31) + text.charCodeAt(i)) | 0
    return { n: els.length, h, len: text.length }
  })
  console.log('DOM fingerprint:', JSON.stringify(fp))
}

await b.close?.()
