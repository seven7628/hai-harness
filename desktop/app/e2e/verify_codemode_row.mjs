#!/usr/bin/env node
// codemode 工具行渲染回归（档位徽标 + 结果优先折叠）
//
// 起因（用户实测）：
//   ① 头部只有裸 `codemode`，分不出这一次是「检索」还是「执行」；
//   ② 折叠态前 5 行**全是样板**（Script completed / Wall time / Output:），真正的返回值
//      只露了个 `{` 就被折了 —— 因为表头占掉了折叠预览那 5 行预算里的 3 行。
//
// 2026-10 改版（「脚本卡」）：档位从行尾药丸改成名字后的标签，两档各有竖脊与骨架 ——
// 检索档的产出是工具名 chip，执行档多一条「珠链」，执行中显示预算进度条与脚本调用点。
//
// 本脚本锁住的关键行为（每条都对应一个「实现错了就会静默退化」的点）：
//   ① 检索档：host_ops 非零 + nested_calls 为空 → 档位标签「检索」+ 摘要「找回 N 个工具」
//   ② 执行档：有 nested_calls → 档位标签「执行」（i18n，不再是硬编码英文）+ 摘要「N 次调用」
//   ③ 表头剥离：折叠预览的**第一行就是返回值**（不再是 `Script completed`）
//   ④ 结局 chip：completed → ok 样式；failed → err 样式（且与 is_error 一致）
//   ⑤ 墙钟 chip：显示 `0.1s`（表头那行不再是纯文本占位）
//   ⑥ 已折叠行数 + spill 路径 + 「查看脚本」三者在同一行（缺失的不渲染）
//   ⑦ 「查看脚本」默认收起源码；点击后展开真实 JS 源码
//   ⑧ 普通工具行**不出现**档位徽标（零污染）
//   ⑨ 纯检索脚本展开后没有嵌套清单（保留操作不是工具调用 —— 这正是需要 host_ops 的原因）
//
// 用法：cd desktop/app && node e2e/verify_codemode_row.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4191)

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
      s.on('error', () => { s.destroy(); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 临时探针页：挂**真实 store + 真实工具行**（走真实事件路径，不手搓 props）。
// 为什么用 store 而不是直接渲染 ToolRow：档位判据的输入（host_ops）是 store 从事件里
// 解析并落到工具块上的 —— 手搓 props 就绕过了那条链，测不到真正的接线。
const PROBE_DIR = path.join(__dirname, '_cmprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>codemode probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import { useEffect, useMemo } from 'react'
import { useAppStore, dispatchEvent } from '../../src/store/useAppStore'
import { ToolGroup } from '../../src/components/NarrativeBlocks'
import '../../src/index.css'

const SID = 'cm-probe'
useAppStore.setState({ workspace: '/Users/dev/ws', activeSessionId: SID })
;(window as unknown as { __store: unknown }).__store = useAppStore
;(window as unknown as { __dispatch: unknown }).__dispatch = dispatchEvent

// 从 store 里取出工具块（与真实对话同一份数据）。
// 注意：选择器必须返回**稳定引用** —— 若在 useAppStore 的选择器里 filter，每次渲染都得到
// 新数组，zustand 判定「变了」→ 无限重渲（实测 Maximum update depth exceeded）。
function App() {
  const blocks = useAppStore((s) => s.views[SID]?.blocks)
  const tools = useMemo(() => (blocks ?? []).filter((b) => b.kind === 'tool'), [blocks])
  useEffect(() => { ;(window as unknown as { __tools: unknown }).__tools = tools }, [tools])
  return (
    <div className="narrative" style={{ padding: 16 }}>
      <ToolGroup tools={tools as never} />
    </div>
  )
}
createRoot(document.getElementById('root')!).render(<App />)
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_cmprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
server.stderr.on('data', (d) => { const s = String(d); if (!/ready in|Local:/.test(s)) process.stderr.write(s) })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

// —— 样本：与 desktop/app/src/transport/mock.ts 的 codemodeScript 同形态（真实表头）——
const SEARCH_SCRIPT = 'const hits = await searchTools("ai-test", 50);\nreturn { count: 28, names: hits.tools.map(t => t.name) };'
const SEARCH_RESULT = 'Script completed\nWall time 0.1 seconds\nOutput:\n'
  + JSON.stringify({ count: 28, names: ['a', 'b', 'c', 'd', 'e'] }, null, 1)
const EXEC_SCRIPT = 'const out = await tools.bash({ command: "go test ./auth" });\nreturn { ran: out.exit_code };'
const EXEC_RESULT = 'Script completed\nWall time 1.4 seconds\nOutput:\n'
  + JSON.stringify({ ran: 0 }, null, 1)
  + '\n' + Array.from({ length: 26 }, (_, i) => `ok  \\tgithub.com/example/auth\\t0.0${i % 10}s`).join('\n')
  + '\n[Full output: /tmp/hai-harness-codemode-spill-1234/out.txt (read with offset/limit)]'
const NESTED = [
  { id: 'n1', name: 'bash', args: '{"command":"go test ./auth"}', result: 'ok 0.04s', is_error: false, duration: 4200000 },
  { id: 'n2', name: 'read_file', args: '{"path":"auth/session.go"}', result: 'package auth', is_error: false, duration: 300000 },
  { id: 'n3', name: 'grep', args: '{"pattern":"TODO"}', result: '', is_error: true, error: 'no match', duration: 120000 },
]
// 执行中的样本：声明了 60s 预算 + 几处调用点（静态），用来锁「脚本控制台条」
const RUN_SCRIPT = '// @options: {"timeout_ms": 60000}\nconst a = await tools.bash({ command: "ls" })\nconst b = await tools.read_file({ path: "x" })\nawait searchTools("scenario")\nreturn a'

// 等某个按钮的 boundingBox 连续两次采样一致（布局稳定）。
async function settleButton(page, rowIndex, sel, tries = 30) {
  let prev = null
  for (let i = 0; i < tries; i++) {
    const box = await page.evaluate(([ri, s]) => {
      const el = document.querySelectorAll('.tool-row')[ri]?.querySelector(s)
      if (!el) return null
      const r = el.getBoundingClientRect()
      return { x: r.x, y: r.y, w: r.width, h: r.height }
    }, [rowIndex, sel])
    if (!box) throw new Error(`找不到第 ${rowIndex} 行的 ${sel}`)
    if (prev && prev.x === box.x && prev.y === box.y) return box
    prev = box
    await page.waitForTimeout(100)
  }
  return prev
}

await waitPort(PORT)
const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 900, height: 900 } })
const errors = []
page.on('pageerror', (e) => errors.push(String(e)))
await page.goto(PROBE_URL, { waitUntil: 'networkidle' })

// 走真实事件路径投递三次调用：检索 / 执行 / 普通工具
await page.evaluate(({ searchScript, searchResult, execScript, execResult, nested, runScript }) => {
  const d = (e) => window.__dispatch(e)
  const base = { session_id: 'cm-probe', run_id: 'main-1' }
  // ① 检索档
  d({ event_type: 'tool_run_start', ...base, id: 'cm-1', name: 'codemode', arguments: JSON.stringify({ code: searchScript }) })
  d({
    event_type: 'tool_run_end', ...base, id: 'cm-1', name: 'codemode', result: searchResult, is_error: false,
    host_ops: { search: 1, describe: 0 },
  })
  // ② 执行档（有嵌套调用 + spill）
  d({ event_type: 'tool_run_start', ...base, id: 'cm-2', name: 'codemode', arguments: JSON.stringify({ code: execScript }) })
  d({
    event_type: 'tool_run_end', ...base, id: 'cm-2', name: 'codemode', result: execResult, is_error: false,
    nested_calls: nested, host_ops: { search: 1, describe: 0 },
  })
  // ③ 普通工具（零污染对照）
  d({ event_type: 'tool_run_start', ...base, id: 'r-1', name: 'read_file', arguments: '{"path":"auth.go"}' })
  d({ event_type: 'tool_run_end', ...base, id: 'r-1', name: 'read_file', result: 'package auth', is_error: false })
  // ④ 执行中的 codemode（只有 start，没有 end —— 锁「脚本控制台条」）
  d({ event_type: 'tool_run_start', ...base, id: 'cm-run', name: 'codemode', arguments: JSON.stringify({ code: runScript }) })
}, { searchScript: SEARCH_SCRIPT, searchResult: SEARCH_RESULT, execScript: EXEC_SCRIPT, execResult: EXEC_RESULT, nested: NESTED, runScript: RUN_SCRIPT })

await page.waitForSelector('.tool-row', { timeout: 5000 })
await page.waitForTimeout(400) // 等 ResizeObserver 量宽

const rows = await page.$$('.tool-row')
assert(rows.length === 4, `工具行数 = 4（检索/执行/普通/执行中），实际 ${rows.length}`)

const dump = await page.evaluate(() => {
  const out = []
  for (const row of document.querySelectorAll('.tool-row')) {
    const q = (sel) => row.querySelector(sel)
    const txt = (sel) => q(sel)?.textContent?.trim() ?? null
    out.push({
      name: txt('.t'),
      tier: txt('.cm-mode-word'),
      tierSearch: q('.cm-mode')?.classList.contains('search') ?? false,
      cmAttr: row.getAttribute('data-cm'),
      beads: [...row.querySelectorAll('.cm-bead')].map((b) => b.textContent.trim()),
      beadErrs: [...row.querySelectorAll('.cm-bead.err')].length,
      flowArrows: row.querySelectorAll('.cm-flow-arrow').length,
      foundChips: [...row.querySelectorAll('.cm-found .cm-tool')].map((c) => c.textContent.trim()),
      discoverChip: txt('.tn-discover'),
      hasRun: Boolean(q('.tool-run')),
      runClock: txt('.run-clock'),
      runSites: [...row.querySelectorAll('.run-site')].map((x) => x.textContent.trim()),
      hasRunBar: Boolean(q('.run-bar')),
      hasBareDots: Boolean(q('.tool-row-hd > .run')),
      summary: txt('.tn-summary'),
      innerFailed: txt('.cm-inner-failed'),
      innerFailedWarn: q('.cm-inner-failed') !== null,
      outcome: txt('.tn-outcome'),
      outcomeErr: q('.tn-outcome')?.classList.contains('err') ?? false,
      firstPreviewLine: (txt('.tn-pre') ?? '').split('\n')[0],
      preview: txt('.tn-pre'),
      hidden: txt('.tn-hidden'),
      spill: txt('.tn-spill'),
      scriptBtn: txt('.tn-script'),
      hasNested: Boolean(q('.tool-nested')),
      hasScriptCode: Boolean(q('.tn-script-code')),
      hasSummaryRow: Boolean(q('.tn-summary')),
    })
  }
  return out
})

const [search, exec, plain] = dump

// —— ① 检索档 ——
assert(search.tier === '检索', `检索行徽标 = "${search.tier}"，want 检索`)
assert(search.tierSearch, '检索徽标带 .search 类（强调色）')
assert(search.summary?.includes('找回 5 个工具'), `检索行摘要 = "${search.summary}"，want 含「找回 5 个工具」`)
assert(search.outcome === '完成' && !search.outcomeErr, `检索行结局 chip = "${search.outcome}"（非错误样式）`)
assert(search.summary?.includes('0.1s'), `检索行摘要含墙钟 0.1s，实际 "${search.summary}"`)
assert(!search.hasNested, '纯检索脚本不该有嵌套调用清单（保留操作不是工具调用）')
assert(!search.innerFailedWarn, '没有嵌套失败的行不该出现「内层失败」角标')

// —— ③ 表头剥离 + 检索档的产出渲染成 chip ——
assert(search.cmAttr === 'search', `检索行 data-cm = "${search.cmAttr}"，want search（竖脊分档）`)
assert(search.foundChips.length === 5, `检索档应把 5 个工具名渲染成 chip，实际 ${JSON.stringify(search.foundChips)}`)
assert(search.foundChips[0] === 'a' && search.foundChips[4] === 'e', `chip 顺序 = ${JSON.stringify(search.foundChips)}`)
assert(search.preview === null, '检索档命中 chip 后不再渲染 JSON 预览（不重复同一份信息）')
assert(!(search.summary ?? '').includes('Script completed'), '摘要里不该再有 Script completed 样板')

// —— ② 执行档 ——
assert(exec.tier === '执行', `执行行档位标签 = "${exec.tier}"，want 执行（走 i18n，不再是硬编码 Execute）`)
assert(!exec.tierSearch, '执行标签不带 .search 类')
assert(exec.cmAttr === 'execute', `执行行 data-cm = "${exec.cmAttr}"，want execute`)
assert(exec.summary?.includes('3 次调用'), `执行行摘要 = "${exec.summary}"，want 含「3 次调用」`)
// ⑩ 外层成功 + 内层有失败：折叠行头部必须有「内层失败 1」角标（脚本 catch 掉错误时，
//    行尾结局仍是 ✓「完成」—— 这个矛盾正是角标存在的理由）。
assert(exec.innerFailed?.includes('内层失败 1'), `执行行应有「内层失败 1」角标，实际 "${exec.innerFailed}"`)
assert(exec.outcome === '完成' && !exec.outcomeErr, `执行行结局仍是完成（非错误样式），实际 "${exec.outcome}"`)
assert(exec.hasNested, '执行行应有嵌套调用清单')
// 珠链：按顺序三颗（bash → read_file → grep）+ 两个箭头；失败的那颗带 .err
assert(exec.beads.length === 3, `珠链应有 3 颗珠子，实际 ${JSON.stringify(exec.beads)}`)
assert(exec.beads[0] === 'bash' && exec.beads[2] === 'grep', `珠链顺序 = ${JSON.stringify(exec.beads)}`)
assert(exec.flowArrows === 2, `珠链箭头数 = ${exec.flowArrows}，want 2`)
assert(exec.beadErrs === 1, `失败调用那颗珠子应带 .err，实际 ${exec.beadErrs}`)
// 保留操作不在明细里：只靠「检索 1」这一个 chip 才看得见
assert((exec.discoverChip ?? '').includes('1'), `执行档应显示「检索 1」（保留操作不进明细），实际 "${exec.discoverChip}"`)
assert(exec.firstPreviewLine === '{', `执行行预览首行 = "${exec.firstPreviewLine}"，want "{"`)

// —— ⑥ 已折叠行数 + spill 路径 + 查看脚本三者在场 ——
assert(!!exec.hidden, `长输出应显示「已折叠 N 行」，实际 ${exec.hidden}`)
assert(exec.spill?.includes('hai-harness-codemode-spill-1234'), `应点名 spill 路径，实际 "${exec.spill}"`)
assert(exec.scriptBtn === '查看脚本', `源码按钮 = "${exec.scriptBtn}"，want 查看脚本`)

// —— ⑦ 默认收起源码，点击后展开 ——
assert(!exec.hasScriptCode, '源码默认收起（结果优先）')
const execRow = rows[1]
// 点击前必须等布局**完全稳定**：折叠预览的宽度是 ResizeObserver 量出来的，量完那一刻
// 预览会重排（行数变化 → 高度变化 → 按钮位移）。Playwright 先算坐标再点，坐标一旦被
// 这次重排作废，点击就落到别的元素上（实测落到 .tn-pre）——不是产品缺陷，是测试时序。
const btnBox = await settleButton(page, 1, '.tn-script')
// 用**稳定后的坐标**点击（不能用 locator.click：它会自己重新算一次坐标，而这次重排
// 就发生在那两步之间 —— 实测点击落到 .tn-pre 上）。
await page.mouse.click(btnBox.x + btnBox.w / 2, btnBox.y + btnBox.h / 2)
await page.waitForTimeout(200)
const afterToggle = await execRow.evaluate((row) => ({
  hasCode: Boolean(row.querySelector('.tn-script-code')),
  code: row.querySelector('.tn-script-code')?.textContent ?? '',
  btn: row.querySelector('.tn-script')?.textContent?.trim(),
}))
assert(afterToggle.hasCode, '点「查看脚本」后源码应展开')
assert(afterToggle.code.includes('tools.bash'), `展开的是真实源码，实际 "${afterToggle.code.slice(0, 40)}"`)
assert(afterToggle.btn === '收起脚本', `展开后按钮文案 = "${afterToggle.btn}"，want 收起脚本`)

// —— ⑧ 普通工具零污染 ——
assert(plain.name === 'read_file', `第三行应是 read_file，实际 "${plain.name}"`)
assert(plain.tier === null, `普通工具不该有档位标签，实际 "${plain.tier}"`)
assert(plain.cmAttr === null, `普通工具不该有 data-cm（竖脊只属于 codemode），实际 "${plain.cmAttr}"`)
assert(!plain.hasSummaryRow, '普通工具不该有 codemode 的结局摘要行')
assert(!plain.hasRun, '普通工具不该有脚本控制台条')

// —— ⑩ 执行中：脚本控制台条（预算进度 + 静态调用点），且不再显示裸「…」与重复计时 ——
const running = dump[3]
assert(running.hasRun, '执行中的 codemode 应有 .tool-run 控制台条')
assert(running.hasRunBar, '控制台条应有预算进度条 .run-bar')
assert((running.runClock ?? '').endsWith('/ 1m 0s'), `预算读数 = "${running.runClock}"，want 以「/ 1m 0s」结尾（脚本声明了 timeout_ms 60000）`)
assert(running.runSites.length === 3, `调用点 chip 应有 3 个（bash/read_file/searchTools），实际 ${JSON.stringify(running.runSites)}`)
assert(running.runSites[2].includes('searchTools'), `第 3 个调用点应是 searchTools，实际 ${JSON.stringify(running.runSites)}`)
assert(!running.hasBareDots, '执行中的 codemode 不该再显示头部那个裸「…」（已有控制台条）')
assert(running.beads.length === 0, '还没结果时不该有珠链（珠链来自完成后的 nested_calls）')
// 执行中不许按源码猜档位（源码里有 searchTools 会把一个正要干活的脚本谎报成「检索」）
assert(running.tier === '执行', `执行中档位标签 = "${running.tier}"，want 执行（不看源码猜）`)

// —— ⑪ 执行中条延迟出现（RUN_STRIP_DELAY_MS）：短调用不该闪一下 ——
// 实测 p50 = 0.17s：没有这道闸，进度条会在十几帧里出现又消失（观感是抖了一下）。
await page.evaluate(() => {
  window.__dispatch({
    event_type: 'tool_run_start', session_id: 'cm-probe', run_id: 'main-1', id: 'cm-short', name: 'codemode',
    arguments: JSON.stringify({ code: 'await tools.bash({ command: "true" })' }),
  })
})
await page.waitForTimeout(150)
const early = await page.evaluate(() => [...document.querySelectorAll('.tool-row')].some((r) => r.querySelector('.run-clock')?.textContent?.endsWith('/ 30s')))
assert(!early, '挂载 150ms 时不该画出控制台条（短调用不闪）')
await page.waitForTimeout(450)
const late = await page.evaluate(() => [...document.querySelectorAll('.tool-row')].some((r) => r.querySelector('.run-clock')?.textContent?.endsWith('/ 30s')))
assert(late, '挂载 600ms 后应画出控制台条（长调用看得见）')
// 收尾：让它结束，别影响后面的行序断言
await page.evaluate(() => {
  window.__dispatch({
    event_type: 'tool_run_end', session_id: 'cm-probe', run_id: 'main-1', id: 'cm-short', name: 'codemode',
    result: 'Script completed\nWall time 0.6 seconds\nOutput:\n{}', is_error: false,
  })
})
await page.waitForTimeout(200)

// —— ⑫ 运行中的输出尾巴（tail -f）：只给人看，工具结束即消失 ——
await page.evaluate(() => {
  const d = (e) => window.__dispatch(e)
  const base = { session_id: 'cm-probe', run_id: 'main-1' }
  d({ event_type: 'tool_run_start', ...base, id: 'r-tail', name: 'bash', arguments: '{"command":"npm run build"}' })
  d({ event_type: 'tool_output', ...base, id: 'r-tail', seq: 1, tail: 'building...\n', total_bytes: 12 })
  d({ event_type: 'tool_output', ...base, id: 'r-tail', seq: 1, tail: 'STALE\n', total_bytes: 1 })
  d({ event_type: 'tool_output', ...base, id: 'r-tail', seq: 2, tail: 'building...\ncompiled 42 modules\n', total_bytes: 33 })
})
await page.waitForTimeout(200)
const tail = await page.evaluate(() => {
  const row = [...document.querySelectorAll('.tool-row')].find((r) => r.querySelector('.t')?.textContent === 'bash' && r.querySelector('.tool-tail'))
  if (!row) return null
  return {
    text: row.querySelector('.tt-pre')?.textContent ?? '',
    bytes: row.querySelector('.tt-bytes')?.textContent ?? '',
    live: row.querySelector('.tt-label')?.textContent ?? '',
    hasResult: Boolean(row.querySelector('.tn-pre')),
  }
})
assert(tail !== null, '运行中的 bash 行应显示输出尾巴 .tool-tail')
assert(tail.text.includes('compiled 42 modules'), `尾巴应是**最新一帧**（替换语义），实际 ${JSON.stringify(tail.text)}`)
assert(!tail.text.includes('STALE'), '重复/更旧的帧必须被丢弃')
assert(tail.bytes.includes('33B'), `字节数应显示累计真值，实际 "${tail.bytes}"`)
assert(tail.live.includes('运行中输出'), `尾巴表头文案 = "${tail.live}"`)
await page.evaluate(() => {
  window.__dispatch({ event_type: 'tool_run_end', session_id: 'cm-probe', run_id: 'main-1', id: 'r-tail', name: 'bash', result: 'done', is_error: false })
})
await page.waitForTimeout(200)
const tailGone = await page.evaluate(() => [...document.querySelectorAll('.tool-row')].some((r) => r.querySelector('.tool-tail')))
assert(!tailGone, '工具结束后尾巴必须消失（最终结果才是权威）')

// —— ④ 失败档（补一条：结局 chip 走 err 样式）——
await page.evaluate(() => {
  const d = (e) => window.__dispatch(e)
  const base = { session_id: 'cm-probe', run_id: 'main-1' }
  d({ event_type: 'tool_run_start', ...base, id: 'cm-9', name: 'codemode', arguments: JSON.stringify({ code: 'throw new Error("boom")' }) })
  d({
    event_type: 'tool_run_end', ...base, id: 'cm-9', name: 'codemode', is_error: true,
    result: 'Script failed\nWall time 0.3 seconds\nOutput:\n[script error] Error: boom',
    // 外层失败**且**内层也失败：这时不叠「内层失败」角标（行尾 ✗ 已经说明一切，
    // 再叠一个只是噪声）。判据见 lib/codemodeNested.ts:innerFailureHint。
    nested_calls: [{ id: 'f1', name: 'bash', args: '{}', result: '', is_error: true, error: 'boom', duration: 1000 }],
  })
})
await page.waitForTimeout(300)
const failed = await page.evaluate(() => {
  const rows = [...document.querySelectorAll('.tool-row')]
  const row = rows[rows.length - 1]
  const o = row.querySelector('.tn-outcome')
  return {
    outcome: o?.textContent?.trim() ?? null,
    err: o?.classList.contains('err') ?? false,
    first: (row.querySelector('.tn-pre')?.textContent ?? '').split('\n')[0],
    innerFailed: row.querySelector('.cm-inner-failed')?.textContent?.trim() ?? null,
  }
})
assert(failed.outcome === '失败', `失败档结局 chip = "${failed.outcome}"，want 失败`)
assert(failed.innerFailed === null, `外层已失败时不该再叠「内层失败」角标，实际 "${failed.innerFailed}"`)
assert(failed.err, '失败档结局 chip 带 .err 类（错误样式）')
assert(failed.first.includes('[script error]'), `失败档预览首行应是错误信息，实际 "${failed.first}"`)

assert(errors.length === 0, `页面无 JS 报错，实际 ${errors.slice(0, 2).join(' | ')}`)

await browser.close()
console.log(`\n✅ codemode 工具行回归：${pass} 项断言全过`)
