#!/usr/bin/env node
// 审批面板「不阻断对话」回归（2026-09-20）
//
// 起因（用户拍板）：「审批（ApprovalModal）也不要全屏，和 ask_user 类似」——审批与提问是同一类
// HITL 决策，此前是 `.modal-hint` 全屏 scrim：叙述区被压暗盖住、滚不动、左栏也点不了，
// 而用户往往要先翻对话历史 / 看清 diff 再拍板。
//
// 修法：`ApprovalCard` 保留（右栏任务 Tab 复用），外面那层全屏 modal 换成**停靠面板**
// `components/ApprovalPanel.tsx`（CenterPanel 的叙述区与 Composer 之间，与提问面板同一套
// `.dock` 语言）；等待窗口由后端 30 分钟（`session.MaxApprovalTimeout`）驱动，倒计时读数
// 来自桥接层注入的 `timeout_ms`（单一事实来源，前端不写死 60s）。
//
// 本脚本用真实 CenterPanel ＋真实 ApprovalCard ＋真实 index.css 断言：
//   ① 面板在流内（position: static）；没有任何覆盖 ≥95% viewport 的 fixed 层（老 scrim 形态）
//   ② 面板停在输入框上方、不遮叙述区；叙述区仍可命中
//   ③ **待审批时滚轮真的能翻历史**（用户报障的原话）
//   ④ 倒计时来自 timeout_ms（30 分钟 → 29:xx），且每秒在走；折叠态也看得到
//   ⑤ 收起面板 → 叙述区拿回整屏；再展开 diff 还在
//   ⑥ 窗口用尽 → 自动拒绝（队列清空、面板消失）——倒计时不是装饰
//   ⑦ 决策链路不变：批准 → 队列清空、面板消失
//
// 用法：cd desktop/app && node e2e/verify_approval_panel.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4193)

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
      s.on('error', () => { clearTimeout(timer); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 临时探针页：真实 CenterPanel + 一张待审批的 write_file（带 diff），跑完删除
const PROBE_DIR = path.join(__dirname, '_apprprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh"><head><meta charset="utf-8"><title>approval probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import CenterPanel from '../../src/components/CenterPanel'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'
import '../../src/narrative-skin.css'

const q = new URLSearchParams(location.search)
document.documentElement.setAttribute('data-skin', q.get('skin') === 'v3' ? 'v3' : 'current')
document.documentElement.setAttribute('data-theme', q.get('theme') || 'ink')

const HISTORY = Array.from({ length: 20 }, (_, i) => ({
  kind: 'assistant', id: 100 + i, streaming: false, thinking: '', model: 'deepseek-v4',
  text: '第 ' + (i + 1) + ' 段历史：' + '这段正文用来把叙述区撑高，验证「待审批时仍可滚轮翻历史」。'.repeat(3),
}))

// 与桥接层注入同形：timeout_ms（等待窗口）+ diff（文件变更预览）
const timeoutMs = Number(q.get('timeoutMs') || 30 * 60 * 1000)
const unified = [
  '--- a/auth/login.go',
  '+++ b/auth/login.go',
  '@@ -88,7 +88,8 @@',
  ' 88  if err := verify(req); err != nil {',
  '-89      return 500, "internal", err',
  '+89      if errors.Is(err, jwt.ErrTokenExpired) {',
  '+90          return 401, "token_expired", nil',
  '+91      }',
  ' 92      return 500, "internal", err',
].join('\\n')

await useAppStore.getState().start('/tmp/appr-probe-ws') // 建立 transport（mock），approve 才有响应
const BLOCKS = [{ kind: 'user', id: 1, text: '改一下 auth 的过期语义' }, ...HISTORY]
const APPROVAL = {
  id: 'call-ap-1', name: 'write_file', index: 0, runId: 'run-7',
  arguments: JSON.stringify({ path: 'auth/login.go', content: 'package auth\\n' }, null, 2),
  diff: { path: 'auth/login.go', added: 3, removed: 1, unified },
  timeoutMs, arrivedAt: Date.now() - 1000,
}
// 会话视图与顶层镜像都要塞：approve 的幂等守卫读的是 views[sid].approvals
//（顶层 approvals 只负责渲染）。字段抄自 store 的 emptyView()。
const VIEW = {
  blocks: BLOCKS, approvals: [APPROVAL], runNodes: [], diffs: [], compressing: false, todos: [],
  usage: { input: 0, output: 0, cacheRead: 0, reasoning: 0 },
  git: { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true },
  gitFiles: [], gitHistory: [], gitHasMore: false, gitHistLoading: false, gitLoading: false,
  turns: [], costUsd: 0, toolStartTs: {}, streaming: false, aborted: false, reqBlock: {},
  runningAgentCount: 0, thinkStartTs: {}, turnThinkMs: {}, pendingQueue: [],
  rightExpanded: false, traceEvents: [],
}
useAppStore.setState({
  persona: 'code',
  activeSessionId: 'probe',
  blocks: BLOCKS,
  approvals: [APPROVAL],
  views: { probe: VIEW },
})

; (window as unknown as { __store: unknown }).__store = useAppStore

createRoot(document.getElementById('root')!).render(
  <div style={{ height: '100vh', display: 'flex' }}><CenterPanel /></div>,
)
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_apprprobe_run/index.html`
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

// 覆盖 ≥95% viewport 的 fixed/absolute 层 = 全屏遮罩（老 .modal-hint scrim 的形态）
const overlayProbe = () => {
  const vw = window.innerWidth
  const vh = window.innerHeight
  const big = []
  for (const el of document.querySelectorAll('body *')) {
    const cs = getComputedStyle(el)
    if (cs.display === 'none' || cs.visibility === 'hidden' || cs.opacity === '0') continue
    if (cs.position !== 'fixed' && cs.position !== 'absolute') continue
    const r = el.getBoundingClientRect()
    if (r.width >= vw * 0.95 && r.height >= vh * 0.95) big.push(`${el.className || el.tagName}(${cs.position})`)
  }
  return big
}

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true })
  const page = await browser.newPage({ viewport: { width: 1100, height: 820 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => { if (m.type() === 'error') errs.push(m.text().slice(0, 200)) })

  await page.goto(`${PROBE_URL}?skin=current&theme=ink`)
  await page.waitForSelector('.dock .card')
  await page.waitForTimeout(250)

  // ① 面板在流内 + 无全屏遮罩
  const geo = await page.evaluate(() => {
    const R = (s) => { const r = document.querySelector(s).getBoundingClientRect(); return { top: Math.round(r.top), bottom: Math.round(r.bottom), h: Math.round(r.height) } }
    const dock = document.querySelector('.dock')
    return {
      pos: getComputedStyle(dock).position,
      role: dock.getAttribute('role'),
      ariaModal: dock.getAttribute('aria-modal'),
      dock: R('.dock'),
      narrative: R('.narrative'),
      composer: R('.composer'),
      vh: window.innerHeight,
      scrims: document.querySelectorAll('.modal-hint.open').length,
    }
  })
  console.log('\n[geo]', JSON.stringify(geo))
  assert(geo.pos === 'static', `审批面板在文档流内（position: ${geo.pos}）—— 不再是全屏浮层`)
  assert(geo.role === 'region' && geo.ariaModal === null, '审批面板是非模态（role=region，无 aria-modal）')
  assert(geo.scrims === 0, '没有 `.modal-hint.open` 全屏 scrim')
  const overlays = await page.evaluate(overlayProbe)
  assert(overlays.length === 0, `没有任何全屏遮罩层（实得 ${overlays.length}：${overlays.join(', ')}）`)
  assert(geo.dock.top >= geo.narrative.bottom - 1, `面板不覆盖叙述区（叙述区底 ${geo.narrative.bottom} / 面板顶 ${geo.dock.top}）`)
  assert(Math.abs(geo.dock.bottom - geo.composer.top) <= 10, `面板贴着输入框上沿（面板底 ${geo.dock.bottom} / 输入框顶 ${geo.composer.top}）`)
  assert(geo.narrative.h >= geo.vh * 0.25, `待审批时叙述区仍可读（${geo.narrative.h}px ≥ 视口 25%）`)

  // ② 叙述区仍可命中
  const hit = await page.evaluate(() => {
    const r = document.querySelector('.narrative').getBoundingClientRect()
    const el = document.elementFromPoint(Math.round(r.left + r.width / 2), Math.round(r.top + r.height / 2))
    return { inNarrative: Boolean(el?.closest('.narrative')), tag: el?.tagName }
  })
  assert(hit.inNarrative, `叙述区正中仍是叙述区（命中 <${hit.tag}>）`)

  // ③ 待审批时滚轮真的能翻历史
  const box = await page.locator('.narrative').boundingBox()
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
  const before = await page.evaluate(() => document.querySelector('.narrative').scrollTop)
  await page.mouse.wheel(0, -600)
  await page.waitForTimeout(250)
  const after = await page.evaluate(() => document.querySelector('.narrative').scrollTop)
  console.log(`  [wheel] scrollTop ${before} → ${after}`)
  assert(after < before, `待审批时滚轮上滚真的翻动了历史（scrollTop ${before} → ${after}）`)
  assert(await page.locator('.dock').isVisible(), '翻历史期间审批面板始终可见')

  // ④ 倒计时来自 timeout_ms（不是写死的 60s）且在走
  const cd1 = await page.locator('.dock .acts .timeout').innerText()
  await page.waitForTimeout(2200)
  const cd2 = await page.locator('.dock .acts .timeout').innerText()
  console.log(`  [countdown] "${cd1}" → "${cd2}"`)
  assert(/29:\d\d|30:00/.test(cd1), `倒计时读数 = 后端等待窗口（30 分钟），实得 "${cd1}"`)
  assert(cd1 !== cd2, `倒计时每秒在走（"${cd1}" → "${cd2}"）`)
  await page.screenshot({ path: '/tmp/approval-panel-docked.png' }) // 证据：停靠形态（含 diff + 倒计时）

  // ⑤ 收起面板 → 叙述区拿回整屏；展开后 diff 还在
  const hBefore = (await page.locator('.narrative').boundingBox()).height
  await page.locator('.dock .appr-hd').click()
  await page.waitForTimeout(220)
  const foldedH = (await page.locator('.narrative').boundingBox()).height
  const foldText = await page.locator('.dock .appr-fold-hint').innerText()
  console.log(`  [fold] narrative ${Math.round(hBefore)} → ${Math.round(foldedH)}px · "${foldText}"`)
  assert(foldedH >= geo.vh * 0.65 && foldedH >= hBefore * 1.4,
    `收起后叙述区明显变大（${Math.round(hBefore)} → ${Math.round(foldedH)}px，≥ 视口 65% 且 ≥ 1.4 倍）`)
  assert(/剩余|自动拒绝/.test(foldText), `收起态标题条仍显示自动拒绝倒计时（"${foldText}"）`)
  assert(await page.locator('.dock .appr-diff').count() === 0, '收起态不渲染 diff 正文（只留标题条）')
  await page.locator('.dock .appr-hd').click()
  await page.waitForTimeout(200)
  assert(await page.locator('.dock .appr-diff').count() === 1, '再点标题条展开：diff 预览回来了')

  // ⑥ 窗口用尽 → 自动拒绝（队列清空、面板消失）
  await page.goto(`${PROBE_URL}?skin=current&theme=ink&timeoutMs=2500`)
  await page.waitForSelector('.dock .card')
  await page.waitForTimeout(3600)
  const left = await page.evaluate(() => window.__store.getState().approvals.length)
  const gone = await page.locator('.dock').count() === 0
  console.log(`  [auto-reject] approvals=${left} dock=${gone ? 'gone' : 'still there'}`)
  assert(gone && left === 0, `等待窗口用尽 → 自动拒绝（面板消失、审批队列清空，实得 approvals=${left}）`)

  assert(errs.length === 0, `控制台 0 报错（实得 ${errs.length}：${errs.slice(0, 3).join(' | ')}）`)

  // ⑦ 决策链路：批准 → 面板消失
  await page.goto(`${PROBE_URL}?skin=current&theme=ink`)
  await page.waitForSelector('.dock .card')
  await page.locator('.dock button.btn.primary').click()
  await page.waitForTimeout(250)
  assert(await page.locator('.dock').count() === 0, '点「批准」后面板消失（乐观清队列，等待工具执行）')

  await browser.close()
  console.log(`\n全部通过（${pass} 项断言）`)
}

main().then(() => process.exit(0)).catch((e) => { console.error(`\n❌ ${e.message}`); process.exit(1) })
