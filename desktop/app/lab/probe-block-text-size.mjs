// 量 live view 里每个 block 的文本规模（2026-11-09）。
//
// 背景：用户报「会话一直卡住」。实测 renderer 主线程热点在 **文字排版**
// （font/text shaping），且 Chromium 持续刷 `box_fragment_painter.cc NOTREACH`
// （paint 失败）。→ 怀疑某个 block 里塞了**超长文本**，Chromium 排版/paint 崩掉。
//
// 用法：node lab/probe-block-text-size.mjs [事件文件]
import { build } from 'esbuild'
import { mkdtempSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = dirname(fileURLToPath(import.meta.url))
const require = createRequire(import.meta.url)
const EVENTS = process.argv[2] ??
  '/Users/admin/.go-code/events/Users_admin_workspace_go-code-855c9368/s-1793856293837663000-16.jsonl'
const SID = 's-size'

const dir = mkdtempSync(join(tmpdir(), 'block-size-'))
const outfile = join(dir, 'store.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile, logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})
const mem = new Map()
global.localStorage = { getItem: (k) => mem.get(k) ?? null, setItem: (k, v) => mem.set(k, String(v)), removeItem: (k) => mem.delete(k) }
global.window = {
  localStorage: global.localStorage,
  matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {}, removeEventListener() {},
  desktop: { startBridge: async () => ({ ok: true }), command: async () => ({ ok: true }), onEvent: () => () => {}, onExit: () => () => {} },
}
const { useAppStore, dispatchEvent } = require(outfile)
const st = () => useAppStore.getState()
useAppStore.setState({
  workspace: '/ws', activeSessionId: SID, bridgeStatus: 'running', views: {},
  sessions: [{ id: SID, title: 'P' }], projects: [{ path: '/ws', sessions: [{ id: SID, title: 'P' }] }],
  blocks: [], runNodes: [], approvals: [], diffs: [], todos: [], turns: [], usage: {}, costUsd: 0,
  streaming: false, pendingQueue: [], toolStartTs: {}, thinkStartTs: {}, turnThinkMs: {},
  recentSessions: [], titleOverrides: {}, settings: { provider: { active: 'p' } }, model: 'm', provider: 'p',
})

const all = []
for (const line of readFileSync(EVENTS, 'utf8').split('\n')) {
  const s = line.trim()
  if (!s) continue
  try { const o = JSON.parse(s); if (o && o.event_type) all.push(o) } catch { /* 半行 */ }
}
for (const e of all) dispatchEvent({ ...e, session_id: SID })

const v = st().views[SID]
const blocks = v?.blocks ?? []
console.log('事件文件：%s', EVENTS)
console.log('blocks=%d', blocks.length)

// 量每个 block 的「渲染文本量」
function textLen(b) {
  switch (b.kind) {
    case 'user': return (b.text ?? '').length
    case 'assistant': return (b.text ?? '').length + (b.thinking ?? '').length
    case 'tool': return (b.args ?? '').length + (b.result ?? '').length + (b.liveTail ?? '').length
    case 'agent': return b.items.reduce((a, it) => a + (it.text ?? '').length + (it.result ?? '').length, 0)
    default: return 0
  }
}
const sizes = blocks.map((b) => ({ kind: b.kind, id: b.id, len: textLen(b), name: b.name ?? b.label ?? '' }))
sizes.sort((a, b) => b.len - a.len)
console.log('')
console.log('文本量最大的 15 个 block：')
console.log('kind        id          len        name')
for (const s of sizes.slice(0, 15)) console.log('%-10s %-11s %10d  %s', s.kind, s.id, s.len, String(s.name).slice(0, 40))
const total = sizes.reduce((a, x) => a + x.len, 0)
console.log('')
console.log('全部 block 文本合计：%d 字符（%.1f MB）', total, total / 1048576)
const over = sizes.filter((s) => s.len > 100000)
console.log('超过 100k 字符的 block：%d 个', over.length)
for (const s of over.slice(0, 10)) console.log('   %s id=%s len=%d name=%s', s.kind, s.id, s.len, String(s.name).slice(0, 30))
