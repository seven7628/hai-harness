#!/usr/bin/env node
// LLM 重试进度显示回归（2026-09-18）
//
// 起因：用户实测「一直显示重试 1」——Attempt 语义被读反（它是「已失败的那一次」）+ 下一次
// 尝试可能长时间无首字（实测一次挂 187s），界面全程只有静态文案，看起来像卡死/重试失效。
// 现在文案改「第 N/M 次失败 → 重试第 N+1/M 次」，并在错误块下方实时显示
// 「退避倒计时 → 本次尝试已进行 Xs · 已等待 Xs」。
//
// 本脚本锁住的关键行为（每条都对应一个「实现错了就会静默退化」的点）：
//   ① 文案含进度总量：第 N/M 次失败 → 重试第 N+1/M 次
//   ② 退避窗口内是**倒计时**，且每秒真的在走（3s → 2s）
//   ③ 退避结束后切「本次尝试已进行 Xs」，且每秒真的在走（5s → 6s）
//   ④ 本轮重试确实重过（retryStartedAt 早于本次失败）→ 追加累计「已等待 Xs」
//   ⑤ 首次失败（retryStartedAt = 本次失败时刻）不显示累计行（无信息量，避免噪音）
//   ⑥ 重试耗尽（retrying=false）→ 进度行不渲染（否则计时器会一直走，误导「还在重试」）
//   ⑦ 重试成本（2026-09-23 决策「失败尝试也要记账」）：失败尝试的已产生用量折算成金额 →
//      重试中显示在计时行尾；重试结束后**保留**（真花掉的钱不随重试结束消失）；无成本不占位
//
// 用法：cd desktop/app && node e2e/verify_retry_progress.mjs
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
      s.on('error', () => { clearTimeout(timer); s.destroy(); resolve(false) }) // 未监听：ECONNREFUSED 不是致命错误
    })
    if (up) return true
    if (Date.now() > deadline) throw new Error(`vite 未在 ${timeoutMs}ms 内监听 ${port}`)
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 临时探针页：挂真实 Narrative + 真实 index.css，blocks 直接注入 store（跑完删除）。
const PROBE_DIR = path.join(__dirname, '_retryprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>retry progress probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import Narrative from '../../src/components/Narrative'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

;(window as unknown as { __store: unknown }).__store = useAppStore
void useAppStore.getState().start('/Users/dev/ws')

// 只渲染叙述通道（错误块 + RetryProgress 都在这里）
createRoot(document.getElementById('root')!).render(
  <div className="narrative" style={{ height: '100vh', overflowY: 'auto', padding: 16 }}>
    <Narrative />
  </div>,
)
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_retryprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
server.stderr.on('data', (d) => { const s = String(d); if (!/ready in|Local:/.test(s)) process.stderr.write(s) })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

// 注入一个错误块（真实 store 结构：与 useAppStore 的 llm_error 分支产出一致）
const setBlock = (page, over) => page.evaluate((o) => {
  const st = window.__store
  st.setState({ blocks: [{ kind: 'error', id: 1, text: '（LLM 请求失败：第 1/11 次尝试失败 → 重试第 2/11 次）read: connection reset by peer', ...o }] })
}, over)

// textContent（不是 innerText）：探针页的叙述容器不参与真实布局，innerText 可能返回空串
const retryLine = (page) => page.locator('.msg.error .err-retry').textContent()

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 900, height: 800 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  await page.goto(PROBE_URL)
  await page.waitForSelector('.narrative')

  // ① 文案进度：第 N/M 次失败 → 重试第 N+1/M 次（Attempt 语义纠正）
  await setBlock(page, { ts: Date.now(), retrying: true, retryAttempt: 1, retryMax: 11, retryDelayMs: 3000, retryStartedAt: Date.now() })
  const text = await page.locator('.msg.error .err-text').textContent()
  assert(text.includes('第 1/11 次尝试失败'), `文案应含「第 1/11 次尝试失败」: ${text}`)
  assert(text.includes('重试第 2/11 次'), `文案应含「重试第 2/11 次」: ${text}`)

  // ② 退避窗口 = 倒计时，且每秒在走
  const c1 = await retryLine(page)
  assert(/3s 后开始重试/.test(c1), `退避窗口应显示倒计时: ${c1}`)
  await page.waitForTimeout(1300)
  const c2 = await retryLine(page)
  assert(/2s 后开始重试/.test(c2), `倒计时应在走（3s → 2s）: ${c2}`)

  // ③ 退避结束 → 「本次尝试已进行 Xs」，且每秒在走
  // 秒数取整向下（fmtRun 用 floor），故用区间断言 + 「两次数值递增 1」而不是写死字面量。
  const secs = (s, re) => { const m = s.match(re); return m ? Number(m[1]) : null }
  const t0 = Date.now()
  await setBlock(page, { ts: t0 - 5000, retrying: true, retryAttempt: 2, retryMax: 11, retryDelayMs: 0, retryStartedAt: t0 - 5000 })
  const e1 = await retryLine(page)
  const e1s = secs(e1, /本次尝试已进行 (\d+)s/)
  assert(e1s != null && e1s >= 4 && e1s <= 5, `尝试进行中应显示已进行时长（≈5s）: ${e1}`)
  assert(!/已等待/.test(e1), `首次失败不显示累计等待（无信息量）: ${e1}`)
  await page.waitForTimeout(1300)
  const e2 = await retryLine(page)
  const e2s = secs(e2, /本次尝试已进行 (\d+)s/)
  assert(e2s === e1s + 1, `已进行时长应在走（每秒 +1）: ${e1} → ${e2}`)

  // ④ 本轮确实重过（起点更早）→ 追加累计「已等待」，同样每秒在走
  const t1 = Date.now()
  await setBlock(page, { ts: t1 - 5000, retrying: true, retryAttempt: 3, retryMax: 11, retryDelayMs: 0, retryStartedAt: t1 - 20000 })
  const w1 = await retryLine(page)
  const w1s = secs(w1, /已等待 (\d+)s/)
  assert(secs(w1, /本次尝试已进行 (\d+)s/) != null && w1s != null && w1s >= 19 && w1s <= 20, `应显示累计已等待时长（≈20s）: ${w1}`)
  await page.waitForTimeout(1300)
  const w2 = await retryLine(page)
  assert(secs(w2, /已等待 (\d+)s/) === w1s + 1, `累计等待应在走（每秒 +1）: ${w1} → ${w2}`)

  // ⑤ 重试耗尽：整行不渲染（不留「还在重试」的假象；无成本时不留任何行）
  await setBlock(page, { ts: Date.now(), retrying: false, retryAttempt: 11, retryMax: 11 })
  assert(await page.locator('.msg.error .err-retry').count() === 0, '重试耗尽不应渲染进度行')

  // ⑥ 重试成本：重试中显示在计时行尾（金额与 fmtUsd 同口径：< $0.0001 用 6 位小数）
  await setBlock(page, { ts: Date.now() - 3000, retrying: true, retryAttempt: 2, retryMax: 11, retryDelayMs: 0, retryCostUsd: 0.000037 })
  const rc1 = await retryLine(page)
  assert(/重试已花费 \$0\.000037/.test(rc1), `重试中应显示已花费金额: ${rc1}`)
  assert(/本次尝试已进行/.test(rc1), `金额应与计时同行（不额外占行）: ${rc1}`)

  // ⑦ 重试结束（耗尽/成功）→ 计时行消失，但「重试已花费」保留（那是真花掉的钱）
  await setBlock(page, { ts: Date.now(), retrying: false, retryAttempt: 11, retryMax: 11, retryCostUsd: 0.000037 })
  const rc2 = await retryLine(page)
  assert(/重试已花费 \$0\.000037/.test(rc2), `重试结束后应保留已花费金额: ${rc2}`)
  assert(!/本次尝试已进行/.test(rc2), `重试结束不应再显示计时: ${rc2}`)

  if (errs.length) throw new Error('页面错误: ' + errs.join(' | '))
  await browser.close()
  console.log(`\nRETRY_PROGRESS_TEST_PASS（${pass} 项断言）`)
}

main().then(() => {
  // 成功也必须显式退出：vite 子进程（stdio:'pipe'）持有事件循环，不 exit 就永远不返回
  //（与 verify_artifacts_card.mjs / verify_preview_container.mjs 同款写法）。
  cleanup()
  process.exit(0)
}).catch((e) => { console.error('\n❌ ' + (e?.message || e)); cleanup(); process.exit(1) })
