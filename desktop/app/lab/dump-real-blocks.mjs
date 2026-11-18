// 把真实会话的 live view blocks 导出成 JSON，供渲染探针使用。
// 用法：node lab/dump-real-blocks.mjs <事件文件> <输出文件>
import { build } from 'esbuild'
import { createReadStream, mkdtempSync, readFileSync, writeFileSync } from 'node:fs'
import * as readline from 'node:readline'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = dirname(fileURLToPath(import.meta.url))
const require = createRequire(import.meta.url)
const EVENTS = process.argv[2] ??
  '/Users/admin/.go-code/events/Users_admin_workspace_go-code-855c9368/s-1793856293837663000-16.jsonl'
const OUT = process.argv[3] ?? '/tmp/real-blocks.json'
const SID = 's-real'

const dir = mkdtempSync(join(tmpdir(), 'dump-blocks-'))
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
useAppStore.setState({
  workspace: '/ws', activeSessionId: SID, bridgeStatus: 'running', views: {},
  sessions: [{ id: SID, title: 'P' }], projects: [{ path: '/ws', sessions: [{ id: SID, title: 'P' }] }],
  blocks: [], runNodes: [], approvals: [], diffs: [], todos: [], turns: [], usage: {}, costUsd: 0,
  streaming: false, pendingQueue: [], toolStartTs: {}, thinkStartTs: {}, turnThinkMs: {},
  recentSessions: [], titleOverrides: {}, settings: { provider: { active: 'p' } }, model: 'm', provider: 'p',
})

const all = []
// 流式读：真实会话的事件文件动辄几百 MB（Node 的 readFileSync 上限 512MB 字符），
// 而且整读进来会在这台 16G 机器上再吃一份峰值内存 ⇒ 逐行读。
{
  const rl = readline.createInterface({ input: createReadStream(EVENTS), crlfDelay: Infinity })
  for await (const line of rl) {
    const s = line.trim()
    if (!s) continue
    try { const o = JSON.parse(s); if (o && o.event_type) all.push(o) } catch { /* 半行 */ }
  }
}
for (const e of all) dispatchEvent({ ...e, session_id: SID })

const v = useAppStore.getState().views[SID]
const out = { blocks: v.blocks, runNodes: v.runNodes, turns: v.turns }
writeFileSync(OUT, JSON.stringify(out))
console.log('已导出 %d blocks → %s (%.1f MB)', v.blocks.length, OUT, JSON.stringify(out).length / 1048576)
