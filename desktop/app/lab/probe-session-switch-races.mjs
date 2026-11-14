// 会话切换竞态复现（2026-11-09）：
//   A. 后台会话只收到「实时尾部事件」→ views[sid].blocks>0，切过去时被 isHydrated 当成
//      「已恢复」→ 不发 new_session → 历史只剩进程启动后的尾部。
//   B. 子智能体视图焦点（focusedAgent）存在会话 view 里 → 切走再切回，主会话继续显示
//      子智能体转录（刷新才正常）。
//
// 用法：node lab/probe-session-switch-races.mjs
import { build } from 'esbuild'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)
const dir = mkdtempSync(join(tmpdir(), 'switch-race-'))
const outfile = join(dir, 'store.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile, logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})

const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}
const cmds = []
global.window = {
  localStorage: global.localStorage,
  matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {}, removeEventListener() {},
  desktop: {
    startBridge: async () => ({ ok: true }), pickWorkspace: async () => null, homeDir: async () => '/tmp',
    createWorkspaceFolder: async () => ({ ok: false }), settingsGet: async () => null, settingsSet: async () => null,
    externalSkillsStatus: async () => null, importExternalSkills: async () => ({ ok: true }),
    pickApp: async () => null, providerSave: async () => ({ ok: true }), providerRemove: async () => ({ ok: true }),
    providerKeyStatus: async () => false, mcpSave: async () => ({ ok: true }),
    fsList: async () => ({ ok: true, entries: [] }), fsReadRef: async () => ({ ok: true, content: '' }),
    pickFsFile: async () => null, pickFsDir: async () => null,
    command: async (cmd) => { cmds.push(cmd); return { ok: true } },
    onEvent: () => () => {}, onExit: () => () => {},
  },
}
const { useAppStore, hydrateCheckpoint, dispatchEvent } = require(outfile)
const st = () => useAppStore.getState()
const view = (sid) => st().views[sid]

const cp = (over = {}) => ({
  schema_version: 1,
  revision: 1,
  blocks: [
    { kind: 'user', id: 1, text: '第一轮', ts: 1000 },
    { kind: 'assistant', id: 2, text: '第一轮回答', model: 'm', run_id: 'r0', ts: 1100 },
    { kind: 'user', id: 3, text: '第二轮', ts: 2000 },
    { kind: 'assistant', id: 4, text: '第二轮回答', model: 'm', run_id: 'r0', ts: 2100 },
  ],
  run_nodes: [], turns: [{ run_id: 'r0', dur_ms: 10 }], usage: { input: 10, output: 5 },
  ...over,
})

function boot() {
  const empty = { blocks: [], approvals: [], runNodes: [], diffs: [], compressing: false, todos: [], turns: [],
    toolStartTs: {}, reqBlock: {}, thinkStartTs: {}, turnThinkMs: {}, traceEvents: [], gitFiles: [], gitHistory: [],
    pendingQueue: [], usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 },
    git: { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true },
    costUsd: 0, streaming: false, aborted: false, runningAgentCount: 0, gitHasMore: false, gitHistLoading: false,
    gitLoading: false, rightExpanded: false }
  useAppStore.setState({
    workspace: '/ws', activeSessionId: 'sA', bridgeStatus: 'running', views: {},
    sessions: [{ id: 'sA', title: 'A' }, { id: 'sB', title: 'B' }],
    projects: [{ path: '/ws', sessions: [{ id: 'sA', title: 'A' }, { id: 'sB', title: 'B' }] }],
    blocks: [], runNodes: [], approvals: [], diffs: [], todos: [], turns: [], usage: empty.usage,
    costUsd: 0, streaming: false, pendingQueue: [], toolStartTs: {}, thinkStartTs: {}, turnThinkMs: {},
    recentSessions: [], titleOverrides: {}, settings: { provider: { active: 'p' } }, model: 'm', provider: 'p',
  })
  useAppStore.setState({ views: { sA: { ...empty } } })
  hydrateCheckpoint('sA', cp(), 1, { revision: 1, live: false })
}

// ===== A：后台尾部事件把会话「伪装成已 hydrate」 =====
boot()
const before = cmds.filter((c) => c.type === 'new_session').length
// 模拟用户在 A，B 在后台跑（或刚跑完），事件按 session_id 落进 views.sB
dispatchEvent({ event_type: 'agent_start', session_id: 'sB', run_id: 'rB', timestamp: 3000 })
dispatchEvent({ event_type: 'llm_start', session_id: 'sB', run_id: 'rB', request_id: 'q1', model: 'm', timestamp: 3001 })
dispatchEvent({ event_type: 'llm_end', session_id: 'sB', run_id: 'rB', request_id: 'q1', model: 'm', finish_reason: 'stop', content: '后台最后一句', usage: { Input: 1, Output: 1 }, timestamp: 3002 })
dispatchEvent({ event_type: 'agent_end', session_id: 'sB', run_id: 'rB', finish_reason: 'stop', duration_ms: 10, timestamp: 3003 })
console.log('B 的 view 在切过去之前：blocks=%d revision=%s turns=%d',
  view('sB')?.blocks.length, view('sB')?.checkpointRevision, view('sB')?.turns.length)
useAppStore.getState().setActiveSession('sB')
const after = cmds.filter((c) => c.type === 'new_session').length
console.log('切到 B 后：发了 new_session=%s  blocks=%d revision=%s turns=%d',
  after > before, view('sB')?.blocks.length, view('sB')?.checkpointRevision, view('sB')?.turns.length)
console.log('  → 结论：%s', after > before
  ? '已修：hydrated=false → 切过去会重新请求历史（修复前不发 new_session，只剩后台尾部）'
  : '仍未修：切过去没有请求历史')

// ===== B：子智能体焦点跨会话存活 =====
boot()
dispatchEvent({ event_type: 'agent_start', session_id: 'sA', run_id: 'subA', parent_run_id: 'mainA', depth: 1, name: '子任务', task_id: 'taskA', timestamp: 4000 })
dispatchEvent({ event_type: 'tool_approval_requested', session_id: 'sA', id: 'ap1', run_id: 'subA', name: 'bash', arguments: '{}', timestamp: 4001 })
console.log('\n在 A 里收到子智能体审批后 focusedAgent=%s', JSON.stringify(st().focusedAgent))
// B 也准备一个已恢复的 view（模拟之前打开过 B）
useAppStore.setState((s) => ({ views: { ...s.views, sB: { ...s.views.sA, focusedAgent: undefined, blocks: [], checkpointRevision: 1 } } }))
useAppStore.getState().setActiveSession('sB')
console.log('切到 B 后 focusedAgent=%s（应为空）', JSON.stringify(st().focusedAgent))
useAppStore.getState().setActiveSession('sA')
console.log('切回 A 后 focusedAgent=%s', JSON.stringify(st().focusedAgent))
console.log('  → 结论：%s', st().focusedAgent?.runId === 'subA'
  ? '按设计保留：子智能体是视图模式，切回恢复焦点（2026-11 决策：只修 hydrated，不清 focusedAgent）'
  : '焦点已被清掉（与本轮决策不符）')

// ===== C：新建会话点击后没有任何本地反馈（响应慢 = 界面停在旧会话）=====
boot()
cmds.length = 0
const beforeSid = st().activeSessionId
await useAppStore.getState().newSession()
await new Promise((r) => setTimeout(r, 1000))
console.log('\n点击新建会话（bridge 不响应）1s 后：activeSessionId=%s（undefined=loading shell）skeleton=%s',
  st().activeSessionId, st().skeleton?.phase)
console.log('  → 结论：%s', st().activeSessionId === undefined && (st().skeleton?.phase === 'pending' || st().skeleton?.phase === 'visible')
  ? '已修：点击即进入 loading shell（activeSessionId 清空 + 骨架屏），不再停在旧会话'
  : '仍未修：点击后界面没有立即反馈')

// ===== D：发送失败（bridge 未运行）被静默吞掉 =====
boot()
// L0（D6）：bridge 在模块求值时消费 window.desktop；node 里没有 preload，用 claim 供货
// （否则真正的 desktop 被消费掉后，下面这行读到的就是 undefined）。
global.window.__haiClaimHostApi = () => global.window.desktop ?? null
const realCommand = global.window.desktop.command
global.window.desktop.command = async () => ({ ok: false, error: 'bridge 未运行' })
const beforeErr = st().bridgeError
useAppStore.getState().setActiveSession('sB')
await new Promise((r) => setTimeout(r, 200))
console.log('\nbridge 未运行时切换会话：activeSessionId=%s bridgeError=%s blocks=%d',
  st().activeSessionId, JSON.stringify(st().bridgeError ?? null), st().blocks.length)
console.log('  → 结论：%s', Boolean(st().bridgeError) || st().toast?.kind === 'error'
  ? '已修：transport.send 的 ok:false 被合成为 command_response（bridgeError/toast 可见）'
  : '仍未修：命令没发出去且没有任何错误提示')
global.window.desktop.command = realCommand
process.exit(0)
