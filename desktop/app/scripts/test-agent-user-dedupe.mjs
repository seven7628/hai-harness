// 子会话「同一个气泡出现 N 次」回归（2026-11 用户报障：切到子智能体会话看到 3 个一样的消息）。
//
// 三条独立来源，任何一条单独成立都会多出气泡：
//   1. `agent_user_message` 是纯 append，没有主键 —— checkpoint hydrate 与 preBuf 补应用
//      在恢复边界上会把同一条事件应用两次（重放不变量）。
//   2. `appendOlderItems`（journal 分页前插）只按 toolId/正文去重，**user 条目一律放行**；
//      而分页 offset 的单位是「卡片 items 条数」，与 journal 派生条目数不等 —— 窗口必然与
//      卡片已有内容重叠，于是每个重叠页都把同一条 user 消息再加一遍。
//   3. `loadAgentHistory` 在目标卡已 loading 时仍会用初始值 offset=0 发请求（mark 变成
//      空操作，但 sendIt 没拦住）；子会话的 loading 又读错了卡（读最新卡、请求挂最早卡）
//      → 滚动预取完全没节流，一次上滑连发多页 → 每个响应各加一份 user 条目。
//
// 用法：node scripts/test-agent-user-dedupe.mjs（npm run test:agent-user-dedupe）
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}

// —— 假 journal：157 条条目，初始 Prompt @0、resume 指令 @123 ——
const RESUME = '应用重启过，你的 worktree 完好，请从中断处继续：'
const JOURNAL = []
for (let i = 0; i < 157; i++) {
  if (i === 0) JOURNAL.push({ kind: 'user', text: 'ORIGINAL PROMPT', index: i })
  else if (i === 123) JOURNAL.push({ kind: 'user', text: RESUME, index: i })
  else if (i % 3 === 0) JOURNAL.push({ kind: 'tool', name: 'bash', tool_id: `t${i}`, index: i })
  else JOURNAL.push({ kind: 'assistant', text: `entry ${i}`, index: i })
}
function pageFor(offset, limit = 50) {
  const total = JOURNAL.length
  const end = Math.max(0, total - offset)
  const start = Math.max(0, end - limit)
  return { entries: JOURNAL.slice(start, end), total, offset, limit, has_more: start > 0 }
}

const F = { cb: null, requests: [], delay: 5 }
const desktop = {
  startBridge: async () => ({ ok: true }), pickWorkspace: async () => null, homeDir: async () => '/tmp',
  createWorkspaceFolder: async () => ({ ok: false }), settingsGet: async () => null, settingsSet: async () => null,
  externalSkillsStatus: async () => null, importExternalSkills: async () => ({ ok: true }),
  pickApp: async () => null, providerSave: async () => ({ ok: true }), providerRemove: async () => ({ ok: true }),
  providerKeyStatus: async () => false, mcpSave: async () => ({ ok: true }),
  fsList: async () => ({ ok: true, entries: [] }), fsReadRef: async () => ({ ok: true, content: '' }),
  pickFsFile: async () => null, pickFsDir: async () => null,
  command: async (cmd) => {
    const respond = (data) => F.cb?.(JSON.stringify({ event_type: 'command_response', id: cmd.id, ok: true, data, workspace: '/ws' }))
    if (cmd.type === 'agent_history') {
      F.requests.push(cmd.payload)
      // 真实 bridge 是一次异步往返：同步回执会把「在途节流」测没。
      setTimeout(() => respond(pageFor(Number(cmd.payload?.offset ?? 0))), F.delay)
    } else {
      setTimeout(() => respond({}), 0)
    }
    return { ok: true }
  },
  onEvent: (cb) => { F.cb = cb; return () => { F.cb = null } },
  onExit: () => () => {},
}
global.window = {
  localStorage: global.localStorage, matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {}, removeEventListener() {}, desktop,
}
global.document = { documentElement: { classList: { add() {}, remove() {} } } }

const dir = mkdtempSync(join(tmpdir(), 'agent-user-dedupe-'))
const outfile = join(dir, 'store.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile, logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})
const { useAppStore, dispatchEvent } = require(outfile)
await useAppStore.getState().start()
await sleep(30)

const RUN1 = 'run-1'
const RUN2 = 'run-2'
const TASK = 'task-xyz'
const emptyView = {
  blocks: [], approvals: [], runNodes: [], diffs: [], compressing: false, todos: [], turns: [],
  toolStartTs: {}, reqBlock: {}, thinkStartTs: {}, turnThinkMs: {}, traceEvents: [], gitFiles: [], gitHistory: [],
  pendingQueue: [], usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 },
  git: { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true },
  costUsd: 0, streaming: false, aborted: false, runningAgentCount: 0,
  gitHasMore: false, gitHistLoading: false, gitLoading: false, rightExpanded: false, rightTab: 'activity',
}
const card = (id, runId, items, spawnedAt) => ({
  kind: 'agent', id, taskId: TASK, runId, label: 'fix-path-select', status: 'running', spawnedAt, items,
})
function setup(items2 = []) {
  F.requests.length = 0
  useAppStore.setState({
    activeSessionId: 's1',
    views: { s1: { ...emptyView, blocks: [
      card(10, RUN1, [{ kind: 'text', text: 'run1 work', runId: RUN1 }], 1),
      card(20, RUN2, items2, 2),
    ] } },
  })
}
const blockOf = (id) => useAppStore.getState().views.s1.blocks.find((b) => b.id === id)
const users = (b) => b.items.filter((it) => it.kind === 'user').map((it) => it.text)

let pass = 0
const ok = (label) => { pass++; console.log(`  ✅ ${label}`) }
const userEvent = (ts) => ({
  event_type: 'agent_user_message', session_id: 's1', run_id: RUN2, task_id: TASK,
  text: RESUME, source: 'prompt',
  timestamp: new Date(1794000000000 + (ts ?? 0) * 1000).toISOString(),
})

// ============ T1：同一条 agent_user_message 重放 3 次 → 只有 1 个气泡 ============
console.log('T1 agent_user_message 重放幂等（恢复边界补应用）')
{
  setup()
  for (let i = 0; i < 3; i++) dispatchEvent(userEvent())
  assert.deepStrictEqual(users(blockOf(20)), [RESUME], `重放后应只有 1 条，got ${JSON.stringify(users(blockOf(20)))}`)
  ok('同一 (runId, text, ts) 应用 3 次 = 1 条')
  // 反例：真实连发两次（不同时间戳）不能被吞掉
  dispatchEvent(userEvent(1))
  assert.strictEqual(users(blockOf(20)).length, 2, '不同时间戳的同文消息是两次真实投递，必须都保留')
  ok('不同时间戳的同文消息仍保留两条（不误伤 agent_send 连发）')
}

// ============ T2：journal 分页窗口与卡片已有 user 重叠 → 不重复 ============
console.log('T2 分页（appendOlderItems）user 去重')
{
  setup([{ kind: 'user', text: RESUME, runId: RUN2, startedAt: 100 }])
  await useAppStore.getState().loadAgentHistory(20, TASK)
  await sleep(30)
  assert.strictEqual(F.requests.length, 1, '应只发一个分页请求')
  // offset = items.length = 1 → 窗口 [106,156)，包含 user@123
  assert.strictEqual(users(blockOf(20)).length, 1, `分页窗口与已有 user 重叠不应复制，got ${JSON.stringify(users(blockOf(20)))}`)
  ok('第 1 页（与首屏 user 重叠）= 仍然 1 条')
  await useAppStore.getState().loadAgentHistory(20, TASK)
  await sleep(30)
  assert.strictEqual(users(blockOf(20)).length, 1, '连续翻页不得累加 user 条目')
  ok('继续翻页 = 仍然 1 条')
}

// ============ T3：在途重复触发只发一个请求（不退回 offset=0） ============
console.log('T3 在途节流：并发触发不叠加请求')
{
  setup([{ kind: 'user', text: RESUME, runId: RUN2, startedAt: 100 }])
  await Promise.all([
    useAppStore.getState().loadAgentHistory(20, TASK),
    useAppStore.getState().loadAgentHistory(20, TASK),
    useAppStore.getState().loadAgentHistory(20, TASK),
  ])
  await sleep(30)
  assert.strictEqual(F.requests.length, 1, `在途期间只应有一个请求，got ${JSON.stringify(F.requests)}`)
  assert.notStrictEqual(F.requests[0].offset, 0, '重复触发不得用 offset=0 再拉一次「最新一页」')
  assert.strictEqual(users(blockOf(20)).length, 1, '并发触发不得复制 user 条目')
  ok('3 次并发触发 = 1 个请求、1 条 user')
}

// ============ T4：journal 里两条同文 user（不同 index）都要保留 ============
console.log('T4 同行不同条：同文但不同 historyIndex 不被误杀')
{
  setup()
  // 人造一页：两条正文相同、序号不同（真实场景：agent_send 连发同一句「继续」）
  const page = {
    entries: [
      { kind: 'user', text: '继续', index: 200 },
      { kind: 'assistant', text: 'ok', index: 201 },
      { kind: 'user', text: '继续', index: 202 },
    ],
    total: 3, offset: 0, limit: 50, has_more: false,
  }
  const realCommand = desktop.command
  desktop.command = async (cmd) => {
    if (cmd.type === 'agent_history') {
      F.requests.push(cmd.payload)
      setTimeout(() => F.cb?.(JSON.stringify({ event_type: 'command_response', id: cmd.id, ok: true, data: page, workspace: '/ws' })), 5)
    } else {
      setTimeout(() => F.cb?.(JSON.stringify({ event_type: 'command_response', id: cmd.id, ok: true, data: {}, workspace: '/ws' })), 0)
    }
    return { ok: true }
  }
  await useAppStore.getState().loadAgentHistory(20, TASK)
  await sleep(30)
  desktop.command = realCommand
  assert.deepStrictEqual(users(blockOf(20)), ['继续', '继续'], `同文不同序号应保留两条，got ${JSON.stringify(users(blockOf(20)))}`)
  ok('两条「继续」都保留（去重按序号，不按正文）')
}

console.log(`\n${pass} 项通过`)
