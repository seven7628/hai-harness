#!/usr/bin/env node
// store 层「待办归属」回归（真实 store，无浏览器）。
//
// 与 e2e/verify_agent_tree.mjs 的分工：
//   · 那个验**渲染结果**（树画在哪、子会话显示谁的清单）；
//   · 这个验**状态机**（事件进来后 store 里到底存了什么）——渲染对了但状态错了
//     会表现为「刷新一下就变回去」，只看截图抓不到。
//
// 钉住三条（用户 2026-10 报障「子智能体里显示主会话的 TODO」的直接根因）：
//   1. 主 run 的 todo_* → 会话清单 views[sid].todos 与顶层镜像同步；
//   2. 子 run 的 todo_* → **只**写它自己的 agent 卡片（块级 + 工具项定格），会话清单不动；
//   3. 没有清单的子 agent（如孙 agent）不继承、不伪造、不回退主会话清单。
//
// 用法：cd desktop/app && node e2e/verify_todos_scope.mjs
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve, dirname } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = dirname(fileURLToPath(import.meta.url))
const APP = resolve(__dirname, '..')
const require = createRequire(import.meta.url)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }

// localStorage / window shim：store 模块加载时读项目树/偏好并选 transport
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}
global.window = { innerWidth: 1600, desktop: { command: async () => ({ ok: true }) }, setTimeout, clearTimeout }

const dir = mkdtempSync(join(tmpdir(), 'todos-scope-'))
const outfile = join(dir, 'bundle.cjs')
await build({
  entryPoints: [join(APP, 'src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  // mock transport 引用 README/brand 资源：给它们占位 loader（本测试不关心内容）
  loader: { '.md': 'text', '.svg': 'dataurl', '.png': 'dataurl' },
})
const { useAppStore, dispatchEvent } = require(outfile)

const SID = 's1'
useAppStore.setState({ activeSessionId: SID, workspace: '/ws', views: {}, blocks: [], runNodes: [], todos: [] })
const ev = (o) => dispatchEvent({ session_id: SID, timestamp: Date.now(), ...o })

// 主 run：两条待办
ev({ event_type: 'agent_start', run_id: 'main-1', content: 'main' })
ev({ event_type: 'tool_run_start', run_id: 'main-1', id: 'm1', name: 'todo_add', arguments: '{}' })
ev({
  event_type: 'tool_run_end', run_id: 'main-1', id: 'm1', name: 'todo_add', result: 'ok',
  todos: [{ id: 't1', title: '主计划', done: false }],
})
assert.deepEqual(useAppStore.getState().todos.map((t) => t.title), ['主计划'])
ok('主 run 的 todo_* 写会话清单（顶层镜像同步）')

// 子 agent：它自己的清单
ev({ event_type: 'agent_start', run_id: 'sub-1', parent_run_id: 'main-1', task_id: 'task-1', name: 'sub', content: 'sub' })
ev({ event_type: 'tool_run_start', run_id: 'sub-1', id: 's1', name: 'todo_add', arguments: '{}' })
ev({
  event_type: 'tool_run_end', run_id: 'sub-1', id: 's1', name: 'todo_add', result: 'ok',
  todos: [{ id: 't1', title: '子计划', done: false }],
})
let st = useAppStore.getState()
assert.deepEqual(st.todos.map((t) => t.title), ['主计划'])
ok('子 run 的 todo_* **不改写**会话清单（核心回归）')

const card = st.blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-1')
assert.ok(card)
assert.deepEqual(card.todos.map((t) => t.title), ['子计划'])
ok('子 run 的清单落在自己的 agent 卡片上')

const item = card.items.find((it) => it.kind === 'tool' && it.toolId === 's1')
assert.deepEqual(item.todos.map((t) => t.title), ['子计划'])
ok('工具项也定格了子 agent 的清单（历史计划卡显示当时状态）')

// 孙 agent：没有清单 → 不继承、不伪造
ev({ event_type: 'agent_start', run_id: 'sub-2', parent_run_id: 'sub-1', task_id: 'task-2', name: 'grand', content: 'grand' })
st = useAppStore.getState()
const g = st.blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-2')
assert.equal(g.todos, undefined)
ok('孙 agent 没有清单时不留空数组、不继承父/会话清单')
assert.deepEqual(st.todos.map((t) => t.title), ['主计划'])
ok('嵌套两层的子 agent 活动之后，会话清单依然只有主 agent 的那条')

console.log(`\n✅ ${pass} 条断言全通过`)
