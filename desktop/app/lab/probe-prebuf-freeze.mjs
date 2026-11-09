// 复现「会话视图冻结 / UI 看似卡死」——snapshotPending 泄漏 → preBuf 永不落地。
//
// store 自己的注释（useAppStore.ts:7610）已经写明这个失效形态：
//   「snapshotPending 不清理 → 该 sid 后续事件全部积压 preBuf 永不落地（视图冻结，UI 看似"卡死"）」
//
// 本探针验证三件事：
//   ① new_session 响应「丢失」时，看门狗是否兜底（应：15s 后事件恢复落地）
//   ② 响应**迟到**（超过看门狗窗口）时，事件是否永久积压 → 视图冻结
//   ③ 用户看到的形态：后端事件持续到达，但前端 blocks 停在原地（与现场一致）
//
// 用法：node lab/probe-prebuf-freeze.mjs
import { build } from 'esbuild'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = dirname(fileURLToPath(import.meta.url))
const require = createRequire(import.meta.url)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const dir = mkdtempSync(join(tmpdir(), 'prebuf-'))
const outfile = join(dir, 'store.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile, logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})

// —— 假 transport：事件由我们手动注入（模拟 bridge stdout）——
// 注意：getTransport() 在模块加载时缓存 transport —— window.desktop 必须在 require 之前就位，
// 否则会拿到 mock transport（mock 会自己造会话，掩盖真实行为）。
let emit = () => {}
const mem = new Map()
global.localStorage = { getItem: (k) => mem.get(k) ?? null, setItem: (k, v) => mem.set(k, String(v)), removeItem: (k) => mem.delete(k) }
global.window = {
  localStorage: global.localStorage,
  matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {}, removeEventListener() {},
  desktop: {
    startBridge: async () => ({ ok: true }),
    command: async (cmd) => { console.log('   [cmd →]', cmd.type, JSON.stringify(cmd.payload ?? {}).slice(0, 140)); return { ok: true } },
    onEvent: (cb) => { emit = cb; return () => { emit = () => {} } },
    onExit: () => () => {},
  },
}
const { useAppStore, dispatchEvent } = require(outfile)
const st = () => useAppStore.getState()
const view = (sid) => st().views[sid]

const SID = 's-freeze'
useAppStore.setState({
  workspace: '/ws', activeSessionId: SID, bridgeStatus: 'idle', views: {},
  sessions: [{ id: SID, title: 'F' }], projects: [{ path: '/ws', sessions: [{ id: SID, title: 'F' }] }],
  blocks: [], runNodes: [], approvals: [], diffs: [], todos: [], turns: [], usage: {}, costUsd: 0,
  streaming: false, pendingQueue: [], toolStartTs: {}, thinkStartTs: {}, turnThinkMs: {},
  recentSessions: [], titleOverrides: {}, settings: { provider: { active: 'p' } }, model: 'm', provider: 'p',
})

// 注入一个「命令已发出」但响应永远不来的 new_session —— 复现 bridge 崩溃/响应丢失
console.log('=== 场景：new_session 响应迟到（超过 15s 看门狗窗口）===')
// 先 wire（store.start() 里调 wire()；探针里手动调，保证 transport.onEvent 已接上）
await useAppStore.getState().start('/ws')
await sleep(300)
useAppStore.getState().setActiveSession(SID)
await sleep(300)
console.log('发 new_session 后，view.blocks =', view(SID)?.blocks.length ?? 0)

// 模拟「响应丢了，但事件继续到达」——bridge 侧会话其实活着
const now = () => new Date().toISOString()
for (let i = 0; i < 5; i++) {
  emit(JSON.stringify({ event_type: 'agent_start', session_id: SID, run_id: 'r1', timestamp: now() }))
  emit(JSON.stringify({ event_type: 'content_chunk', session_id: SID, run_id: 'r1', request_id: 'q1', content: `实时内容 ${i}`, timestamp: now() }))
  await sleep(50)
}
await sleep(500)
const v1 = view(SID)
console.log('事件到达后 0.5s：blocks =', v1?.blocks.length ?? 0,
  '｜正文 =', JSON.stringify((v1?.blocks ?? []).map((b) => b.text ?? '').join('')))
console.log('  → 结论：%s', (v1?.blocks.length ?? 0) === 0
  ? '复现：事件已到达但 blocks 仍为 0 —— 视图冻结（preBuf 积压未落地）'
  : '未复现：事件已落地')

console.log('')
console.log('等待 15s 看门狗…')
await sleep(16000)
const v2 = view(SID)
console.log('看门狗后：blocks =', v2?.blocks.length ?? 0,
  '｜正文 =', JSON.stringify((v2?.blocks ?? []).map((b) => b.text ?? '').join('')))
console.log('  → 结论：%s', (v2?.blocks.length ?? 0) > 0
  ? '看门狗兜底成功：事件在 15s 后落地（用户看到「卡 15 秒后突然恢复」）'
  : '看门狗未兜底：事件永久积压 —— UI 永久冻结')

// 看门狗是否真的被 armed？（它要求 snapshotPending.get(sid) === requestId）
console.log('')
console.log('=== 诊断：看门狗为什么没兜底 ===')
console.log('store 是否暴露内部 map：', typeof useAppStore.__debugSnapshotPending)
console.log('（snapshotPending / preBuf 是模块级私有变量，探针看不到 → 用行为推断）')
// 行为推断：再发一次 new_session（响应仍不来），看新请求是否会重新挂起 preBuf
emit(JSON.stringify({ event_type: 'content_chunk', session_id: SID, run_id: 'r1', request_id: 'q1', content: '再来一条', timestamp: now() }))
await sleep(300)
console.log('看门狗后再发事件：blocks =', view(SID)?.blocks.length ?? 0,
  '  （仍为 0 = 看门狗没清 preBuf，事件继续积压）')

console.log('')
console.log('=== 场景：看门狗之后又来事件（持续冻结 or 恢复？）===')
emit(JSON.stringify({ event_type: 'content_chunk', session_id: SID, run_id: 'r1', request_id: 'q1', content: '又一条', timestamp: now() }))
await sleep(500)
const v3 = view(SID)
console.log('新事件后：blocks =', v3?.blocks.length ?? 0,
  '｜正文 =', JSON.stringify((v3?.blocks ?? []).map((b) => b.text ?? '').join('')))
console.log('  → 结论：%s', (v3?.blocks.length ?? 0) > (v2?.blocks.length ?? 0)
  ? '恢复：看门狗后事件正常落地'
  : '仍冻结：新事件也积压（preBuf 又被重新挂起）')

// —— 诊断：事件到底去哪了？直接 dispatch 对比 ——
console.log('')
console.log('=== 诊断：绕过 wire 直接 dispatchEvent 会落地吗？===')
dispatchEvent({ event_type: 'content_chunk', session_id: SID, run_id: 'r1', request_id: 'q1', content: '直投', timestamp: now() })
await sleep(200)
const v4 = view(SID)
console.log('直投后：blocks =', v4?.blocks.length ?? 0)
console.log('  → 结论：%s', (v4?.blocks.length ?? 0) > 0
  ? 'store 本身正常 —— 冻结在 wire 的 preBuf 缓冲（事件被挂起未落地）'
  : 'store 也不落地 —— 问题在 dispatchEvent 内部')
