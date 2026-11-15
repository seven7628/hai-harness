#!/usr/bin/env node
// composer.quote 端到端（09-agent-bridge §6.2 / D7）—— 真 Composer + 真 loader + 真插件。
//
// 这条门禁只钉一件事，但每一格都是「实现错了会静默」的那种：
//   ① 引用的**唯一**出口是 `host.command('composer.quote', {text, source})`：不经过 bridge
//      （渲染层本地命令），也不经过 surface_command —— 通道错了在产品里表现为「偶发丢引用」；
//   ② 文本进的是**当前草稿**（追加到末尾、不覆盖用户已经打的字），且带一枚可删的来源 chip；
//   ③ **绝不自动发送**（D7）：插完草稿之后 ask_batch 一次都不许发出去；
//   ④ 同一动作重复投递（连点 / 面板重挂载）只插一次：文本一次、chip 一枚；
//   ⑤ 上限与拒绝：空文本 / 超 8 KiB → invalid_input；没有活动会话 → no_session。
//
// 端口由内核分配（e2e/_serve.mjs 的 freePort）—— 不占 5231/5241/5262/5264/5265 那几条
// 固定给别的探针的端口，也不和固定端口链抢。
//
// 用法：cd desktop/app && npm run e2e:composer-quote
// 退出码：全通过 0；任一断言失败 1；前置不满足（端口/编译）2。
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { exitCodeOf, freePort, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = await freePort()
const PROBE_DIR = path.join(__dirname, `_composerquote_run_${process.pid}`)
const PROBE_MARKER = '<title>composer quote probe</title>'
const PROBE_URL = `http://localhost:${PORT}/e2e/${path.basename(PROBE_DIR)}/index.html`
const WS = '/mock/composer-quote'
const WS2 = '/mock/composer-quote-2'

fs.mkdirSync(path.join(PROBE_DIR, 'plugins', 'quoter'), { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8">'
  + '<title>composer quote probe</title></head>'
  + '<body><div id="root" style="height:100vh"></div>'
  + '<script type="module" src="./main.tsx"></script></body></html>')

// 探针插件：只做一件事 —— 把真 loader 给的 host 挂到 globalThis，测试经它调 composer.quote。
// 这就是插件作者会写的那一行（host.command('composer.quote', …)），没有别的入口。
fs.writeFileSync(path.join(PROBE_DIR, 'plugins/quoter/panel.js'), `
export const surface = {
  kind: 'plugin:quoter',
  apply({ host, React, surfaces }) {
    globalThis.__hostQuote = host
    surfaces.register({
      kind: 'plugin:quoter', group: 'plugin', order: 10,
      title: () => 'Quoter', render: () => React.createElement('div', { 'data-plugin': 'quoter' }, 'quoter'),
    })
  },
}
`)

fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
// 探针页：**真** Composer + **真** loader + **真** store（mock transport）。
// ⚠️ 注入必须在任何 src 模块求值之前（L0：桥在模块求值时一次性消费 window.desktop），
// 所以先摆 window.desktop，再 await import —— static import 会被 hoist 到赋值之前。
import '../../src/index.css'

const sent: Array<Record<string, unknown>> = []
const feeds: Array<(line: string) => void> = []
const agentFeeds: Array<(p: Record<string, unknown>) => void> = []
const pluginIndex = {
  version: 1,
  items: [{
    id: 'quoter',
    version: '1.0.0',
    hash: 'h'.padEnd(64, '0'),
    enabled: true,
    kind: 'plugin:quoter',
    name: 'Quoter',
    entries: [],
    commands: ['composer.quote', 'agent.run', 'ui.toast'],
    data: ['agent.delta', 'surfaces_changed'],
    entry: 'panel.js',
  }],
}

// 假 bridge 的**回执**要真的回一条 command_response：真 bridge 的响应走事件流（不是 invoke
// 的返回值），少这一条 new_session 就永远激活不了会话 —— no_session 那条会变成假绿。
const respond = (cmd: Record<string, unknown>, data: Record<string, unknown>) => {
  setTimeout(() => {
    const line = JSON.stringify({ event_type: 'command_response', id: cmd.id, ok: true, workspace: '${WS}', data })
    feeds.forEach((f) => f(line))
  }, 0)
}
let sessionSeq = 0
const stub: Record<string, unknown> = {
  startBridge: async () => ({ ok: true }),
  command: async (cmd: unknown) => {
    const c = cmd as Record<string, unknown>
    sent.push({ ...c, __at: performance.now() })
    if (c?.type === 'new_session') {
      const sid = 'probe-s' + (++sessionSeq)
      respond(c, { session_id: sid, sessions: [{ id: sid, title: 'probe ' + sessionSeq }], todos: [], live: false })
    }
    return { ok: true }
  },
  onEvent: (cb: (line: string) => void) => {
    feeds.push(cb)
    return () => {
      const i = feeds.indexOf(cb)
      if (i >= 0) feeds.splice(i, 1)
    }
  },
  onExit: () => () => {},
  settingsGet: async () => ({}),
  settingsSet: async () => ({ ok: true }),
  startupMark: () => {},
  surfaceIndex: async () => pluginIndex,
  surfaceCommand: async () => ({ ok: true, data: {} }),
  surfaceAgentToken: async (id: string) => ({ ok: true, token: 'probe-token-' + id }),
  surfaceAgent: async () => ({ ok: true, data: {} }),
  onSurfaceAgentEvent: (cb: (p: Record<string, unknown>) => void) => {
    agentFeeds.push(cb)
    return () => {
      const i = agentFeeds.indexOf(cb)
      if (i >= 0) agentFeeds.splice(i, 1)
    }
  },
}
;(window as unknown as { desktop: unknown }).desktop = new Proxy(stub, {
  get(t, k) {
    if (k in t) return t[k as string]
    return () => Promise.resolve({ ok: true })
  },
})

const { createRoot } = await import('react-dom/client')
const [{ default: Composer }] = await Promise.all([import('../../src/components/Composer')])
const { setPluginBaseUrlForTest } = await import('../../src/surfaces/plugin/loader')
setPluginBaseUrlForTest((id: string) => \`/e2e/${path.basename(PROBE_DIR)}/plugins/\${id}\`)
const { useAppStore } = await import('../../src/store/useAppStore')

createRoot(document.getElementById('root')!).render(
  <div style={{ display: 'flex', flexDirection: 'column', minHeight: '100vh', background: 'var(--bg)' }}>
    <div style={{ flex: '1 1 auto' }} />
    <Composer />
  </div>,
)

const w = window as unknown as Record<string, unknown>
w.__probe = {
  store: useAppStore,
  // 插件世界里那次调用（host 是 loader 给的，token 在 loader 闭包里）
  command: (name: string, payload?: unknown) =>
    (globalThis as Record<string, unknown>).__hostQuote
      ? (globalThis as any).__hostQuote.command(name, payload)
      : Promise.resolve({ ok: false, reason: 'no_host' }),
  hostShape: () => {
    const host = (globalThis as Record<string, unknown>).__hostQuote as Record<string, unknown> | undefined
    return host ? { keys: Object.keys(host).sort(), events: typeof host.events } : null
  },
  // 草稿与 chip 只看 DOM 事实（不读内部 state）
  draft: () => (document.querySelector('textarea.input') as HTMLTextAreaElement | null)?.value ?? '',
  chips: () => [...document.querySelectorAll('[data-quote-chip]')].map((n) => n.getAttribute('data-quote-chip')),
  removeChip: () => {
    const btn = document.querySelector('[data-quote-chip] .attach-x') as HTMLElement | null
    btn?.click()
    return !!btn
  },
  sent: () => sent,
  sentTypes: () => sent.map((c) => c.type),
  sessionId: () => useAppStore.getState().activeSessionId ?? '',
  turns: () => useAppStore.getState().turns.length,
  newSession: () => useAppStore.getState().newSession(),
}

// 真启动路径（mock transport）；会话在测试里显式建（这样 no_session 那条也能测到）。
void useAppStore.getState().start(${JSON.stringify(WS)})
`)

fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react(), tailwindcss()], server: { fs: { allow: [APP] } } })
`)

const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
server.stderr.on('data', (d) => { const s = String(d); if (!/ready in|Local:/.test(s)) process.stderr.write(s) })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

const fails = []
function check(label, cond, detail = '') {
  if (cond) console.log(`  ✅ ${label}`)
  else { fails.push(label); console.log(`  ❌ ${label}${detail ? ' —— ' + detail : ''}`) }
}

try {
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:composer-quote', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1100, height: 800 } })
  page.setDefaultTimeout(5000)
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!window.__probe, null, { timeout: 30000 })
  // 等真 loader 把探针插件装上（host 由 apply 交出来）
  await page.waitForFunction(() => !!globalThis.__hostQuote, null, { timeout: 15000 })

  const call = (name, payload) => page.evaluate(([n, p]) => window.__probe.command(n, p), [name, payload])
  const draft = () => page.evaluate(() => window.__probe.draft())
  const chips = () => page.evaluate(() => window.__probe.chips())

  console.log('\nT1 host 的形状：command + onEvent，没有 events')
  const shape = await page.evaluate(() => window.__probe.hostShape())
  check('host.onEvent 在', Array.isArray(shape?.keys) && shape.keys.includes('onEvent'), JSON.stringify(shape))
  check('host.events 不在', shape?.events === 'undefined', JSON.stringify(shape))

  console.log('\nT2 没有活动会话 → no_session（不静默写进另一份草稿）')
  const noSession = await call('composer.quote', { text: '没有会话时', source: { id: 'sel-0', title: '讲义' } })
  check('回执 no_session', noSession?.ok === false && noSession?.reason === 'no_session', JSON.stringify(noSession))
  check('草稿还是空的', (await draft()) === '', JSON.stringify(await draft()))
  check('没有 chip', (await chips()).length === 0, JSON.stringify(await chips()))

  console.log('\nT3 建会话 → 引用插入草稿（末尾追加）+ 来源 chip')
  await page.evaluate(() => window.__probe.newSession())
  await page.waitForFunction(() => !!window.__probe.sessionId(), null, { timeout: 8000 })
  // 用户先打了一半的字：引用必须**追加**，不能覆盖
  await page.click('textarea.input')
  await page.keyboard.type('我的问题：')
  const okQuote = await call('composer.quote', { text: '  选中了一段讲义  ', source: { id: 'sel-1', title: '讲义 p.12' } })
  check('回执 ok', okQuote?.ok === true, JSON.stringify(okQuote))
  const d1 = await draft()
  check('文本进了草稿（trim 过）', d1.includes('选中了一段讲义'), JSON.stringify(d1))
  check('用户已经打的字还在（追加，不覆盖）', d1.startsWith('我的问题：'), JSON.stringify(d1))
  const c1 = await chips()
  check('chip 出现且文案 = source.title', c1.length === 1 && c1[0] === '讲义 p.12', JSON.stringify(c1))

  console.log('\nT4 绝不自动发送（D7）')
  check('没有任何 ask_batch 发出去', !(await page.evaluate(() => window.__probe.sentTypes())).includes('ask_batch'),
    JSON.stringify(await page.evaluate(() => window.__probe.sentTypes())))
  check('会话里没有多出轮次', (await page.evaluate(() => window.__probe.turns())) === 0, String(await page.evaluate(() => window.__probe.turns())))
  check('输入框里还是有内容（不是被发送清空）', (await draft()).includes('选中了一段讲义'))

  console.log('\nT5 同一动作重复投递 → 只插一次（文本一次、chip 一枚）')
  const again = await call('composer.quote', { text: '选中了一段讲义', source: { id: 'sel-1', title: '讲义 p.12' } })
  check('重复投递仍回 ok（幂等，不是错误）', again?.ok === true, JSON.stringify(again))
  const d2 = await draft()
  check('文本只出现一次', d2.split('选中了一段讲义').length - 1 === 1, JSON.stringify(d2))
  check('chip 仍是一枚', (await chips()).length === 1, JSON.stringify(await chips()))
  // 另一个来源的同一段文本：允许（不同 chip，文本已存在所以不再追加）
  const other = await call('composer.quote', { text: '选中了一段讲义', source: { id: 'sel-2', title: '另一份讲义' } })
  check('换来源仍 ok', other?.ok === true, JSON.stringify(other))
  check('文本依旧只有一份', (await draft()).split('选中了一段讲义').length - 1 === 1, JSON.stringify(await draft()))
  check('chip 变成两枚（来源不同）', (await chips()).length === 2, JSON.stringify(await chips()))

  console.log('\nT6 chip 可删，且删 chip 不动正文')
  const removed = await page.evaluate(() => window.__probe.removeChip())
  check('点到了 ×', removed === true)
  check('少了一枚 chip', (await chips()).length === 1, JSON.stringify(await chips()))
  check('正文一个字都没少', (await draft()).includes('选中了一段讲义'), JSON.stringify(await draft()))

  console.log('\nT7 拒绝：空文本 / 超 8 KiB / 非字符串')
  check('空文本 → invalid_input', (await call('composer.quote', { text: '   ' }))?.reason === 'invalid_input')
  check('缺 text → invalid_input', (await call('composer.quote', {}))?.reason === 'invalid_input')
  check('数字 text → invalid_input', (await call('composer.quote', { text: 42 }))?.reason === 'invalid_input')
  check('8 KiB+1 → invalid_input', (await call('composer.quote', { text: 'x'.repeat(8193) }))?.reason === 'invalid_input')
  const draftAfterRejects = await draft()
  check('拒绝没有改动草稿', draftAfterRejects.includes('选中了一段讲义') && draftAfterRejects.length < 8193, String(draftAfterRejects.length))

  console.log('\nT8 换会话：引用进的是**当前**草稿（不串台，切回来原样还在）')
  const firstId = await page.evaluate(() => window.__probe.sessionId())
  const firstDraft = await draft()
  await page.evaluate(() => window.__probe.newSession())
  await page.waitForFunction((prev) => !!window.__probe.sessionId() && window.__probe.sessionId() !== prev, firstId, { timeout: 8000 }).catch(() => {})
  const second = await call('composer.quote', { text: '第二个会话的引用', source: { id: 'sel-9', title: '另一课' } })
  check('第二个会话回执 ok', second?.ok === true, JSON.stringify(second))
  const d3 = await draft()
  check('新草稿只有新引用', d3.includes('第二个会话的引用') && !d3.includes('选中了一段讲义'), JSON.stringify(d3))
  check('新会话的 chip 只有一枚', (await chips()).length === 1, JSON.stringify(await chips()))
  // 切回第一个会话 → 那份草稿与它的 chip 原样还在（引用属于那份草稿）
  await page.evaluate((id) => window.__probe.store.getState().setActiveSession(id), firstId)
  await page.waitForFunction((id) => window.__probe.sessionId() === id, firstId, { timeout: 5000 }).catch(() => {})
  const back = await draft()
  check('切回来的草稿 = 第一个会话那份', back === firstDraft, JSON.stringify(back))
  check('切回来的 chip 也回来了', (await chips()).length === 1, JSON.stringify(await chips()))

  console.log('\nT9 全程零 pageerror')
  check('pageerror = 0', errs.length === 0, errs.slice(0, 3).join(' | '))

  await browser.close()
} catch (e) {
  cleanup()
  console.error(`\n❌ ${e?.message ?? e}`)
  process.exit(exitCodeOf(e))
}

cleanup()
if (fails.length) {
  console.error(`\n❌ ${fails.length} 条断言失败：\n  - ${fails.join('\n  - ')}`)
  process.exit(1)
}
console.log('\n✅ composer.quote 全部通过')
