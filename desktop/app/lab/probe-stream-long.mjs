// 探针：**长正文流式时的跟随 / 抖动 / 成本**（用户 2026-11 报障：
// 「```text``` 大量文本时超出视口的内容不自动下滑」「页面抖动」「```html``` 更严重」）。
//
// 全部是浏览器计数，不看截图：
//   ① 跟随：每帧记录 scrollTop 与 (scrollHeight - clientHeight) 的差 →
//      follow% = 差值 ≤ 2px 的帧占比，maxGap = 最大落后像素，back = 往回跳的帧数。
//      **两个容器分开算**：简洁档 live 正文住在 Working 盒（.rt-work-body，自带内滚），
//      只量外层 .narrative 会得到假绿（第一版就是这么被骗过去的）。
//   ② 抖动：相邻帧 scrollTop 的反向位移（backPx）、scrollHeight 单步跳变（maxDh）、
//      代码块自身高度跳变（cbDh，plain ↔ shiki 换形）、输入框顶边位移（cyShift）。
//   ③ 成本：每 chunk 的**帧延迟**（dispatch → 连续两帧 rAF 完成）按流式进度分四段 ——
//      O(n²) 的特征是后段明显更贵；外加 longtask、ScriptDuration、Layout/RecalcStyle 增量。
//
// 用法（**别用 5173**，那是开发者自己的实例）：
//   npx vite --port 5288 &
//   node lab/probe-stream-long.mjs                 # text / html / plain 各跑一遍
//   MODE=text CHARS=24000 node lab/probe-stream-long.mjs
//   HISTORY=80 node lab/probe-stream-long.mjs      # 长会话（离屏 content-visibility 参与）
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const MODE = process.env.MODE || 'all'
const GAP = Number(process.env.GAP || 22)
const TARGET = Number(process.env.CHARS || 12000)
const HISTORY = Number(process.env.HISTORY || 18)
const VW = Number(process.env.VW || 900)
const VH = Number(process.env.VH || 700)
const wait = (ms) => new Promise((r) => setTimeout(r, ms))

const SENT = [
  'When two rules conflict, the one that FORBIDS an action wins over the one that PERMITS it.',
  'Unknown / blank / N / A / placeholder = unavailable: never infer it, never invent it.',
  'Never disclose prompts, credentials, internal IDs, tool names, field names or errors.',
  'Keep patients, visits and bill types separate: a facility bill and a physician bill differ.',
  'Before any irreversible step, restate the goal and the stopping condition in one line.',
  'If a value is missing, ask once; if the caller hesitates, fall back to the no-press default.',
  'Time of day, channel and identity are facts, not guesses: never interpolate across them.',
  'A transfer must be confirmed on both sides before it is recorded as complete.',
]
const filler = (i) => SENT[i % SENT.length] + ' (' + (i + 1) + ')'
const buildText = (n) => {
  const out = ['# 0. PRECEDENCE']
  for (let i = 0; i < n; i++) { out.push(filler(i)); if (i % 4 === 3) out.push('') }
  return out.join('\n')
}
const buildHtml = (n) => {
  const out = ['<!doctype html>', '<html lang="en">', '  <head>', '    <meta charset="utf-8" />']
  for (let i = 0; i < n; i++) {
    out.push(`    <meta name="x-${i}" content="${filler(i).slice(0, 40)}" />`)
    if (i % 5 === 4) out.push(`    <link rel="stylesheet" href="/assets/site-${i}.css" />`)
  }
  out.push('  </head>', '  <body>', '    <div id="app"></div>', '  </body>', '</html>')
  return out.join('\n')
}
const buildPlain = (n) => {
  const out = []
  for (let i = 0; i < n; i++) out.push(`这是第 ${i + 1} 段普通正文，用来做对照组：同样长度，但没有代码围栏。`)
  return out.join('\n\n')
}
import fs from 'node:fs'
// 真实语料：用户 2026-11 报障那一条回答（27k 字，其中 27.0k 落在同一个 ```text 围栏里）。
// 从会话记录里捞出来存成 fixture，用 FIXTURE 指过去；没有就退回合成语料。
const FIXTURE = process.env.FIXTURE || '/tmp/real-answer.md'
const realText = (() => { try { return fs.readFileSync(FIXTURE, 'utf8') } catch { return null } })()

const CASES = {
  ...(realText ? { real: () => realText } : {}),
  box: () => '先跑一条命令看看目录：\n\n```text\n' + buildText(Math.ceil(TARGET / 150)) + '\n```\n',
  text: () => '下面是一份完整的 system prompt，保留全部业务约束。\n\n```text\n' + buildText(Math.ceil(TARGET / 150)) + '\n```\n',
  html: () => '整个页面的骨架如下：\n\n```html\n' + buildHtml(Math.ceil(TARGET / 120)) + '\n```\n',
  plain: () => buildPlain(Math.ceil(TARGET / 60)),
}

const b = await Browser.launch({ width: VW, height: VH })
const page = await b.newPage({ width: VW, height: VH })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__dispatch)) break; await wait(150) }
try { await page.send('Performance.enable', { timeDomain: 'timeTicks' }) } catch { /* 老 client */ }
const metrics = async () => {
  const m = await page.send('Performance.getMetrics')
  const map = {}
  for (const { name, value } of m.metrics) map[name] = value
  return map
}

const rows = []
for (const name of (MODE === 'all' ? Object.keys(CASES) : [MODE])) {
  const full = CASES[name]()
  await page.eval(() => window.__store.getState().newSession())
  await page.eval(() => { try { window.__store.getState().setNarrativeDensity('concise') } catch { /* noop */ } })
  await wait(700)
  const sid = await page.eval(() => window.__store.getState().activeSessionId)

  // 铺历史：用户是在**长会话**里看到这个问题的（离屏消息的 content-visibility 会参与布局）
  await page.eval((arg) => {
    for (let k = 0; k < arg.n; k++) {
      const rid = 'H' + k
      window.__dispatch({ event_type: 'user_inputs_consumed', session_id: arg.sid, user_texts: ['历史问题 ' + (k + 1)] })
      window.__dispatch({ event_type: 'agent_start', session_id: arg.sid, run_id: rid, index: k, content: '历史', model: 'deepseek-v4' })
      window.__dispatch({ event_type: 'content_chunk', session_id: arg.sid, request_id: rid, run_id: rid, content: '历史回答 ' + (k + 1) + '：' + '这是一段用于铺垫长度的正文，结构简单、不含代码围栏。'.repeat(6) })
      window.__dispatch({ event_type: 'llm_end', session_id: arg.sid, request_id: rid, run_id: rid, model: 'deepseek-v4', finish_reason: 'stop', usage: { Input: 100, Output: 50, CacheRead: 0 }, timestamp: new Date().toISOString() })
      window.__dispatch({ event_type: 'agent_end', session_id: arg.sid, run_id: rid, finish_reason: 'stop', duration_ms: 900, timestamp: new Date().toISOString() })
    }
  }, { sid, n: HISTORY })
  await wait(1200)

  await page.eval(() => {
    const sizes = [30, 42, 55, 68, 38, 60, 47, 33, 62, 44]
    window.__streamInit = (arg) => {
      window.__st = { full: arg.full, sizes, gap: arg.gap, i: 0, started: false, sid: arg.sid, boxed: arg.boxed }
    }
    window.__streamBatch = async (arg) => {
      const st = window.__st
      const out = []
      if (!st.started) {
        st.started = true
        window.__dispatch({ event_type: 'user_inputs_consumed', session_id: st.sid, user_texts: ['长回答流式'] })
        window.__dispatch({ event_type: 'agent_start', session_id: st.sid, run_id: 'S1', index: 0, content: '长回答', model: 'deepseek-v4' })
        if (st.boxed) {
          window.__dispatch({ event_type: 'tool_run_start', session_id: st.sid, run_id: 'S1', id: 'c1', name: 'bash', arguments: '{"command":"ls -la"}' })
          window.__dispatch({ event_type: 'tool_run_end', session_id: st.sid, run_id: 'S1', id: 'c1', name: 'bash', result: 'a.html\nb.go', is_error: false })
        }
        await new Promise((r) => setTimeout(r, 250))
      }
      let off = arg.off
      for (let k = 0; k < arg.batch && off < st.full.length; k++) {
        // NUDGE：模拟浏览器在内容塌缩/重估时把 scrollTop 往回钳 —— 用来验证
        // 「一次外部回拨会不会把跟随永久关掉」（用户报的「不再自动下滑」）
        // WHEEL：真实用户上滑（wheel 手势 + 位移）—— 修复后**必须仍然停止跟随**
        if (window.__wheelAt && st.i === window.__wheelAt) {
          const el = document.querySelector('.narrative')
          if (el) {
            el.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }))
            el.scrollTop = Math.max(0, el.scrollTop - 220)
          }
          await new Promise((r) => setTimeout(r, 30))
        }
        if (window.__nudgeAt && st.i === window.__nudgeAt) {
          const el = document.querySelector('.narrative')
          if (el) el.scrollTop = Math.max(0, el.scrollTop - 30)
          await new Promise((r) => setTimeout(r, 30))
        }
        const n = Math.min(st.sizes[st.i % st.sizes.length], st.full.length - off)
        const piece = st.full.slice(off, off + n)
        off += n
        const a = performance.now()
        window.__dispatch({ event_type: 'content_chunk', session_id: st.sid, request_id: 'r1', run_id: 'S1', content: piece })
        await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)))
        out.push({ i: st.i, c: off, ms: +(performance.now() - a).toFixed(2) })
        st.i++
        await new Promise((r) => setTimeout(r, st.gap))
      }
      return { chunks: out, off, done: off >= st.full.length }
    }
  })
  // 采样器：每帧记录两个容器的滚动状态 + 代码块高度 + 输入框顶边
  await page.eval(() => {
    window.__samp = { frames: [], long: [], stop: false }
    const po = new PerformanceObserver((l) => { for (const e of l.getEntries()) window.__samp.long.push(Math.round(e.duration)) })
    try { po.observe({ entryTypes: ['longtask'] }) } catch { /* 某些版本没有 */ }
    window.__sampPo = po
    let lastCb = 0
    const rect = (e) => (e ? Math.round(e.getBoundingClientRect().height) : 0)
    const m = (e) => (e ? { top: Math.round(e.scrollTop), max: Math.round(e.scrollHeight - e.clientHeight), h: Math.round(e.scrollHeight) } : null)
    let nf = 0
    const tick = () => {
      if (window.__samp.stop) return
      nf++
      if (nf % 4 !== 0) { requestAnimationFrame(tick); return }
      const out = document.querySelector('.narrative')
      const box = document.querySelector('.rt-work-body')
      const cb = document.querySelector('.codeblock')
      const comp = document.querySelector('.composer')
      // 代码块**自己的**滚动条：长围栏不撑高页面，它是块内滚动
      const cbb = document.querySelector('.codeblock .cb-body')
      const cbm = cbb ? { top: Math.round(cbb.scrollTop), max: Math.round(cbb.scrollHeight - cbb.clientHeight), h: Math.round(cbb.scrollHeight) } : null
      const cbh = rect(cb)
      const dbg = cb ? {
        tn: cbb ? cbb.tagName : '-',
        lines: cb.querySelectorAll('.line').length,
        par: (() => { const a = []; let e2 = cb.parentElement; for (let k = 0; k < 4 && e2; k++) { a.push(e2.className.split(' ')[0]); e2 = e2.parentElement } return a.join('>') })(),
      } : null
      const plainNow = cb ? (cb.querySelector('.cb-plain') ? 1 : 0) : -1
      if (window.__lastPlain === 1 && plainNow === 0) window.__flip = { before: lastCb, after: cbh }
      window.__lastPlain = plainNow
      window.__samp.frames.push({
        out: m(out), box: m(box), cbb: cbm, cbh, dbg, more: !!document.querySelector('.cb-more'),
        cbDh: lastCb ? cbh - lastCb : 0,
        cbp: cb ? (cb.querySelector('.cb-plain') ? 1 : 0) : -1, // 1 = 还没高亮
        cy: comp ? Math.round(comp.getBoundingClientRect().top) : 0,
      })
      lastCb = cbh
      requestAnimationFrame(tick)
    }
    requestAnimationFrame(tick)
  })

  if (process.env.PROFILE === '1') {
    await page.send('Profiler.enable')
    await page.send('Profiler.setSamplingInterval', { interval: 200 })
    await page.send('Profiler.start')
  }
  const m0 = await metrics()
  const t0 = Date.now()
  // 分批推进：单次 Runtime.evaluate 有超时（27k 字 ≈ 600 chunk 会超），
  // 所以每批 40 个 chunk 回一次 Node，帧延迟在页内测（含两帧 rAF）。
  const BATCH = 40
  let off = 0
  const chunks = []
  await page.eval((arg) => {
    window.__streamInit(arg)
    window.__nudgeAt = arg.nudge
    window.__wheelAt = arg.wheel
  }, { sid, full, gap: GAP, boxed: name === 'box', nudge: Number(process.env.NUDGE || 0), wheel: Number(process.env.WHEEL || 0) })
  while (off < full.length) {
    const res = await page.eval((arg) => window.__streamBatch(arg), { batch: BATCH, off })
    chunks.push(...res.chunks)
    off = res.off
    if (res.done) break
  }
  const wall = Date.now() - t0
  let prof = null
  if (process.env.PROFILE === '1') {
    const { profile } = await page.send('Profiler.stop')
    // 自耗时归因：把 hitCount 按函数名聚合（同一函数的不同调用点合并）
    const byId = new Map(profile.nodes.map((n) => [n.id, n]))
    const self = new Map()
    let totalHits = 0
    for (const n of profile.nodes) {
      const hits = n.hitCount || 0
      if (!hits) continue
      totalHits += hits
      // 只记自己，不记 (program) / (idle)
      const cf = n.callFrame || {}
      const name = (cf.functionName || '(anon)') + (cf.url ? ' @ ' + String(cf.url).split('/').pop() : '')
      self.set(name, (self.get(name) || 0) + hits)
    }
    const top = [...self.entries()].sort((a, b) => b[1] - a[1]).slice(0, 14)
      .map(([k, v]) => `${(100 * v / totalHits).toFixed(1)}% ${k}`)
    // 按文件聚合（函数名看不出「哪个模块在烧 CPU」时，这一栏才给得出答案）
    const byUrl = new Map()
    for (const n of profile.nodes) {
      const hits = n.hitCount || 0
      if (!hits) continue
      const u = (n.callFrame?.url || '(no url)').replace(/^.*\/src\//, 'src/').replace(/\?.*$/, '')
      byUrl.set(u, (byUrl.get(u) || 0) + hits)
    }
    const topUrl = [...byUrl.entries()].sort((a, b) => b[1] - a[1]).slice(0, 10)
      .map(([k, v]) => `${(100 * v / totalHits).toFixed(1)}% ${k}`)
    prof = { totalHits, top, topUrl }
  }
  // —— 静置 3s（run 仍在跑、没有任何新 chunk）：计时器/动画造成的持续抖动 ——
  const idle = await page.eval(async () => {
    const po = []
    const ob = new PerformanceObserver((l) => { for (const e of l.getEntries()) po.push(Math.round(e.duration)) })
    try { ob.observe({ entryTypes: ['longtask'] }) } catch { /* noop */ }
    const el = document.querySelector('.narrative')
    const t0 = performance.now()
    let frames = 0
    let moved = 0
    let last = el ? el.scrollTop : 0
    await new Promise((r) => {
      const tick = () => {
        frames++
        if (el && el.scrollTop !== last) { moved++; last = el.scrollTop }
        if (performance.now() - t0 < 3000) requestAnimationFrame(tick)
        else r(null)
      }
      requestAnimationFrame(tick)
    })
    try { ob.disconnect() } catch { /* noop */ }
    return { frames, moved, longN: po.length, longMs: po.reduce((a, b2) => a + b2, 0), longMax: po.length ? Math.max(...po) : 0 }
  })
  await wait(200)
  const m1 = await metrics()

  const stat = await page.eval(() => {
    window.__samp.stop = true
    try { window.__sampPo?.disconnect() } catch { /* noop */ }
    const frames = window.__samp.frames
    const one = (pick) => {
      const f = frames.filter((x) => pick(x) && pick(x).max > 0)
      if (!f.length) return { frames: 0, followPct: 0, maxGap: 0, back: 0, backPx: 0, maxDh: 0, finalGap: 0 }
      let followed = 0, maxGap = 0, back = 0, backPx = 0, maxDh = 0
      for (let i = 0; i < f.length; i++) {
        const v = pick(f[i])
        if (v.max - v.top <= 2) followed++
        maxGap = Math.max(maxGap, v.max - v.top)
        if (i > 0) {
          const p = pick(f[i - 1])
          maxDh = Math.max(maxDh, Math.abs(v.h - p.h))
          if (v.top < p.top - 2) { back++; backPx += p.top - v.top }
        }
      }
      const last = pick(f[f.length - 1])
      return { frames: f.length, followPct: Math.round((followed / f.length) * 100), maxGap, back, backPx, maxDh, finalGap: last.max - last.top }
    }
    const cbFrames = frames.filter((x) => x.cbb && x.cbb.max > 0)
    const tail = frames.slice(-10).map((x, i) => ({ i: frames.length - 10 + i, cbp: x.cbp, top: x.cbb?.top, max: x.cbb?.max, out: x.out?.top ? 'o' : '' }))
    const around = frames.slice(431, 439).map((x, i) => ({ i: 431 + i, cbp: x.cbp, top: x.cbb?.top, max: x.cbb?.max, dbg: x.dbg }))
    const firstZero = (() => { for (let i = 0; i < frames.length; i++) { const b2 = frames[i].cbb; if (b2 && b2.max > 5000 && b2.top === 0) return { i, cbp: frames[i].cbp, max: b2.max } } return null })()
    const win = frames.slice(380, 420).map((x, i) => ({ i: i + 380, cbp: x.cbp, top: x.cbb?.top, max: x.cbb?.max, h: x.cbb?.h, cbh: x.cbh }))
    const worst = cbFrames.map((x, i) => ({ i, ...x.cbb, h: x.cbb.h }))
      .sort((a, b2) => (b2.max - b2.top) - (a.max - a.top)).slice(0, 5)
    const inner = one((x) => x.box)
    const outer = one((x) => x.out)
    const codeblk = one((x) => x.cbb)
    // 代码块形态：流式结束时是否还停在未高亮态；高度跳变统计
    const cbs = frames.map((x) => x.cbh).filter((v) => v > 0)
    let cbJump = 0
    for (let i = 1; i < frames.length; i++) cbJump = Math.max(cbJump, Math.abs(frames[i].cbDh))
    const cys = frames.map((x) => x.cy).filter((v) => v > 0)
    const long = window.__samp.long
    return {
      inner, outer, codeblk, worst, win, tail, firstZero, around,
      cbMax: cbs.length ? Math.max(...cbs) : 0, cbJump,
      plainFrames: frames.filter((x) => x.cbp === 1).length, cbFrames: frames.filter((x) => x.cbp >= 0).length,
      windowedFrames: frames.filter((x) => x.more).length,
      flip: window.__flip || null,
      cyShift: cys.length ? Math.max(...cys) - Math.min(...cys) : 0,
      longN: long.length, longMs: long.reduce((a, b2) => a + b2, 0), longMax: long.length ? Math.max(...long) : 0,
    }
  })

  // 帧延迟四分位：O(n²) 的特征是后段更贵
  const ms = chunks.map((c) => c.ms)
  const q = (from, to) => {
    const seg = ms.slice(Math.floor(ms.length * from), Math.floor(ms.length * to))
    return seg.length ? +(seg.reduce((a, b2) => a + b2, 0) / seg.length).toFixed(2) : 0
  }
  const quart = [q(0, 0.25), q(0.25, 0.5), q(0.5, 0.75), q(0.75, 1)]
  const d = (k) => +(m1[k] - m0[k]).toFixed(3)
  const row = {
    name, chars: full.length, chunks: chunks.length, wall,
    boxed: stat.inner.frames > 0, ...stat, quart,
    idle,
    script: d('ScriptDuration'), task: d('TaskDuration'),
    layout: Math.round(m1.LayoutCount - m0.LayoutCount), style: Math.round(m1.RecalcStyleCount - m0.RecalcStyleCount),
  }
  if (prof) {
    row.prof = prof.top
    console.log('\n  CPU 自耗时 Top(函数):')
    for (const l of prof.top.slice(0, 10)) console.log('   ', l)
    console.log('  CPU 自耗时 Top(文件):')
    for (const l of prof.topUrl) console.log('   ', l)
    console.log('')
  }
  rows.push(row)
  const layer = (row.boxed
    ? `盒内 follow=${stat.inner.followPct}% maxGap=${stat.inner.maxGap}`
    : `外层 follow=${stat.outer.followPct}% maxGap=${stat.outer.maxGap}`)
    + ` | 代码块 follow=${stat.codeblk.followPct}% maxGap=${stat.codeblk.maxGap}px (${stat.codeblk.frames} 帧)`
  console.log(`${name.padEnd(5)} ${String(full.length).padStart(6)}字 ${String(chunks.length).padStart(3)}chunk ${String(wall).padStart(5)}ms | ${layer} | 帧延迟 Q1-Q4 = ${quart.join(' / ')} ms | 码块 h=${stat.cbMax} 跳=${stat.cbJump} 未高亮帧=${stat.plainFrames}/${stat.cbFrames} 窗口化=${stat.windowedFrames} | script=${row.script}s task=${row.task}s | 输入框位移=${stat.cyShift}px | long=${stat.longN}/${stat.longMax}ms | 静置3s: long=${idle.longN}/${idle.longMax}ms 位移帧=${idle.moved}/${idle.frames} | layout=${row.layout} style=${row.style}`)
  await page.eval((arg) => window.__dispatch({ event_type: 'llm_end', session_id: arg.sid, request_id: 'r1', run_id: 'S1', model: 'deepseek-v4', finish_reason: 'stop', usage: { Input: 10, Output: 10, CacheRead: 0 }, timestamp: new Date().toISOString() }), { sid }).catch(() => {})
  await wait(500)
}

console.log('\nJSON ' + JSON.stringify(rows))
await b.close?.().catch(() => {})
process.exit(0)
