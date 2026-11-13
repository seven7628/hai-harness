#!/usr/bin/env node
// Hub 会话事件的**主 store 闸门**回归（真实 store + esbuild 打包，Node 直跑，无需浏览器/Electron）。
//
// 为什么要有这个测试（防的回归）：
//   appStore 的全局事件入口只挡了 `event_type.startsWith('hub_')`。但 Hub 会话的流式/运行事件
//   **没有** hub_ 前缀 —— content_chunk / reasoning_chunk / agent_start / agent_end / llm_end /
//   tool_run_* 全是普通事件名，只有 `session_id`（bridge 给每条事件注入）能认出它属于 Hub：
//   `hub.<agent>.dm`（DM）/ `hub.<agent>.g.<gid>`（群会话，见 agenthub/group.go 的 SessionID）。
//
//   漏进主 store 的代价是**两份**：
//     · 内存：setView 建出永不清理的 views['hub.*']（Hub 自己还有一份，同一段正文画两遍）；
//     · 唤醒：每次 chunk 都 setState → 全部 useAppStore 订阅点重算（Project 侧一个都不用）。
//
//   所以闸门有两层（同一判据，两个位置）：
//     1. wire() 的 transport.onEvent —— 传输层最前面，连 chunk 合并缓冲与快照 preBuf 都不进；
//     2. dispatchEvent() —— 事件进视图层的**唯一入口**（快照补应用 / preBuf 补应用 / 重放都走
//        这里），在这里再挡一次 ⇒ 任何调用路径都建不出 hub.* 视图。
//   两层都必须**不误伤**工作区会话（s_…）与空 sid，也必须让 command_response 照常结算
//   （否则发消息会等 10s 超时）。下面 P1/P2 分别钉这两层，P3 钉不误伤。
//
// 用法：npm run test:store:hub-gate
// 退出码：全通过 0；任一断言失败 1（TDD「先红」阶段靠这个非 0 码证明脚本有效）。
import { build } from 'esbuild'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// 断言器：**收集**失败而不是第一条就退出 —— 一次跑完能同时看到「闸门漏了」和「闸门误伤」。
let pass = 0
const fails = []
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => {
  if (cond) ok(m)
  else { fails.push(m); console.log(`  ❌ ${m}`) }
}
const eq = (actual, want, m) => assert(Object.is(actual, want), `${m}（实际 ${JSON.stringify(actual)}）`)

// 1) shim localStorage（store 模块加载时读项目树/标题覆盖/偏好）
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}

// 2) fake window.desktop —— **必须在 require bundle 之前**：store 模块加载时按
//    `typeof window !== 'undefined' && window.desktop` 选 transport（有 desktop 就走
//    electronTransport）。P2 要的就是这条真实路径：wire() 在 start() 里注册到 transport.onEvent，
//    我们往这个订阅者里灌事件行 —— 与线上 bridge 推事件走的是**同一条**线。
//    onEvent 必须是**多订阅者**（真 transport 就是 Set）：store 会为一次性 command_response
//    再注册临时订阅者，单槽会把 wire() 的回调顶掉，于是 P2 静默退化成「什么都没测」。
const F = { cbs: new Set(), sessions: new Map(), ws: '/ws-hub-gate' }
const push = (ev) => {
  const line = JSON.stringify({ ...ev, workspace: F.ws })
  for (const cb of [...F.cbs]) cb(line)
}
const respond = (id, data) => push({ event_type: 'command_response', id, ok: true, data })

function makeDesktop() {
  return {
    startBridge: async () => ({ ok: true }),
    pickWorkspace: async () => null,
    homeDir: async () => '/tmp',
    createWorkspaceFolder: async () => ({ ok: false }),
    settingsGet: async () => null,
    settingsSet: async () => null,
    externalSkillsStatus: async () => null,
    importExternalSkills: async () => ({ ok: true }),
    pickApp: async () => null,
    providerSave: async () => ({ ok: true }),
    providerRemove: async () => ({ ok: true }),
    providerKeyStatus: async () => false,
    mcpSave: async () => ({ ok: true }),
    fsList: async () => ({ ok: true, entries: [] }),
    fsReadRef: async () => ({ ok: true, content: '' }),
    pickFsFile: async () => null,
    pickFsDir: async () => null,
    // bridge 命令桩：list 回项目树、new_session 回空快照（其余一律 {}）。
    // 只够把 start() 的恢复链走通 —— 断言对象是闸门，不是恢复。
    command: async (cmd) => {
      if (cmd.type === 'list') respond(cmd.id, { sessions: [...F.sessions.values()] })
      else if (cmd.type === 'new_session') {
        const want = String(cmd.payload?.id ?? '')
        respond(cmd.id, {
          session_id: want,
          sessions: [...F.sessions.values()],
          todos: [],
          live: false,
          restored: 'snapshot',
          snapshot: { events: [] },
        })
      } else respond(cmd.id, {})
      return { ok: true }
    },
    onEvent: (cb) => {
      F.cbs.add(cb)
      return () => F.cbs.delete(cb)
    },
    onExit: () => () => undefined,
  }
}

global.window = {
  localStorage: global.localStorage,
  matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {},
  removeEventListener() {},
  desktop: makeDesktop(),
}
global.document = { documentElement: { classList: { add() {}, remove() {} } } }

// 3) esbuild 打包真实 store 到临时 CJS（mock.ts 的 ?raw/?url 资源要 stub 掉，否则打包失败）
const dir = mkdtempSync(join(tmpdir(), 'store-hub-gate-'))
const outfile = join(dir, 'store-bundle.cjs')
const assetPlugin = {
  name: 'asset-suffix',
  setup(b) {
    b.onResolve({ filter: /\?(raw|url|no-inline)$/ }, () => ({ path: 'asset', namespace: 'asset-stub' }))
    b.onLoad({ filter: /.*/, namespace: 'asset-stub' }, () => ({ contents: 'module.exports = ""', loader: 'js' }))
  },
}
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  plugins: [assetPlugin],
})
const { useAppStore, dispatchEvent, isHubSessionId } = require(outfile)

const views = () => useAppStore.getState().views
const hubViewKeys = () => Object.keys(views()).filter((k) => k.startsWith('hub.'))
const textOf = (v) => (v?.blocks ?? []).map((b) => (typeof b.text === 'string' ? b.text : '')).join('')
const reset = () => useAppStore.setState({
  views: {},
  activeSessionId: undefined,
  blocks: [],
  runNodes: [],
  busySids: new Set(),
})

// ============ P0 纯函数边界（两层闸门共用的判据） ============
console.log('P0 isHubSessionId 边界')
eq(isHubSessionId('hub.ag_x.dm'), true, 'DM 会话 id 是 Hub')
eq(isHubSessionId('hub.ag_x.g.grp_1'), true, '群会话 id 是 Hub')
eq(isHubSessionId('s_normal'), false, '工作区会话（s_…）不是 Hub')
eq(isHubSessionId('s_'), false, '空壳工作区 id 不是 Hub')
eq(isHubSessionId(''), false, '空 sid 不是 Hub（无归属，不能误伤）')
eq(isHubSessionId(undefined), false, '缺 session_id 不是 Hub')
eq(isHubSessionId(null), false, 'null 不是 Hub')
eq(isHubSessionId(42), false, '非字符串不是 Hub')
eq(isHubSessionId('hubby'), false, '没有点分隔的 hub 前缀不是 Hub 会话 id')

// ============ P1 dispatchEvent 闸门（视图层唯一入口） ============
// 即使某个调用方绕过传输层（快照补应用 / preBuf 补应用 / 日志重放 / 测试），也不许建 hub.* 视图。
console.log('P1 dispatchEvent 闸门')
reset()
dispatchEvent({ event_type: 'content_chunk', session_id: 'hub.ag_x.dm', run_id: 'r1', content: 'hi' })
dispatchEvent({ event_type: 'agent_start', session_id: 'hub.ag_x.dm', run_id: 'r1' })
// chunk 有 32ms 合并窗口：等它 flush 完再断言，否则「还没建」等于没测
await sleep(60)
eq(views()['hub.ag_x.dm'], undefined, 'dispatchEvent：hub 会话的 chunk/agent_start 不建视图')
eq(hubViewKeys().length, 0, 'dispatchEvent：views 里没有 hub.* 键')
eq(useAppStore.getState().busySids.has('hub.ag_x.dm'), false, 'dispatchEvent：hub 会话不进 busySids（左栏运行点/embed 保护集）')

// ============ P2 传输层闸门（wire() 的 transport.onEvent，线上真实路径） ============
console.log('P2 wire() 传输层闸门（经 fake bridge 灌事件行）')
F.sessions.set('s_normal', { id: 's_normal', title: 'SN', updated_at: Date.now() })
await useAppStore.getState().start(F.ws)
await sleep(120) // list → new_session → 快照落地
assert(F.cbs.size > 0, 'start() 后 transport.onEvent 有订阅者（wire() 已接线）')
assert(Boolean(views().s_normal), '夹具就绪：工作区会话 s_normal 已恢复出视图')

// 正控制：先证明这条线**真的**能把事件送到 dispatchEvent —— 否则下面的「没建视图」毫无意义。
push({ event_type: 'agent_start', session_id: 's_probe', run_id: 'rp' })
await sleep(30)
assert(Boolean(views().s_probe), '正控制：非 Hub 会话的 agent_start 会建视图（事件线通）')

const before = Object.keys(views()).length
const hubEvents = [
  { event_type: 'agent_start', session_id: 'hub.ag_x.dm', run_id: 'rh' },
  { event_type: 'content_chunk', session_id: 'hub.ag_x.dm', run_id: 'rh', content: '漏进来了？' },
  { event_type: 'reasoning_chunk', session_id: 'hub.ag_x.dm', run_id: 'rh', content: '思考…' },
  { event_type: 'tool_run_start', session_id: 'hub.ag_x.dm', run_id: 'rh', id: 'call_1', name: 'bash' },
  { event_type: 'tool_run_end', session_id: 'hub.ag_x.dm', run_id: 'rh', id: 'call_1', name: 'bash', result: 'ok' },
  { event_type: 'llm_end', session_id: 'hub.ag_x.dm', run_id: 'rh', request_id: 'req_1', model: 'm' },
  { event_type: 'agent_end', session_id: 'hub.ag_x.dm', run_id: 'rh' },
  { event_type: 'agent_start', session_id: 'hub.ag_x.g.grp_1', run_id: 'rg' },
  { event_type: 'content_chunk', session_id: 'hub.ag_x.g.grp_1', run_id: 'rg', content: '群里漏？' },
]
for (const ev of hubEvents) push(ev)
await sleep(80) // 覆盖 chunk 合并窗口
eq(hubViewKeys().length, 0, 'wire()：Hub 会话的流式/运行事件不建 hub.* 视图')
eq(Object.keys(views()).length, before, 'wire()：视图数量不变（没有任何一条 Hub 事件建了视图）')
eq(useAppStore.getState().busySids.has('hub.ag_x.dm'), false, 'wire()：Hub 会话不进 busySids')
eq(useAppStore.getState().busySids.has('hub.ag_x.g.grp_1'), false, 'wire()：Hub 群会话不进 busySids')

// 原有 hub_ 前缀闸门回归：hub_message 是 Hub 自己的事件名，两个形状（hub sid / 工作区 sid）都不该进主 store
const keysBefore = Object.keys(views()).sort().join(',')
const blocksBefore = (views().s_normal?.blocks ?? []).length
push({ event_type: 'hub_message', session_id: 'hub.ag_x.dm', group_id: '', message: { id: 'msg_1' } })
push({ event_type: 'hub_message', session_id: 's_normal', group_id: '', message: { id: 'msg_2' } })
push({ event_type: 'hub_conversation', session_id: 's_normal', group_id: 'grp_1' })
await sleep(40)
eq(Object.keys(views()).sort().join(','), keysBefore, 'wire()：hub_ 事件一个视图都不建（键集合不变）')
eq(hubViewKeys().length, 0, 'wire()：hub_message 仍被 hub_ 闸门挡掉（不建 hub.* 视图）')
eq((views().s_normal?.blocks ?? []).length, blocksBefore, 'wire()：hub_ 事件不动任何工作区视图的正文')

// ============ P3 不误伤（闸门只管 Hub，工作区/空 sid 照旧） ============
console.log('P3 不误伤工作区会话')
// ① 工作区会话的流式事件照常进它自己的视图（经过传输层）
push({ event_type: 'content_chunk', session_id: 's_normal', run_id: 'rw', content: 'wire-ok' })
await sleep(80)
assert(textOf(views().s_normal).includes('wire-ok'), 'wire()：工作区会话的 content_chunk 照常落进 views[s_normal]')
eq(hubViewKeys().length, 0, 'wire()：工作区事件不会顺带建出 hub.* 视图')
// chunk 合并缓冲是**全局**一个队列（queueChunk），Hub 的 chunk 若进得来就有机会被合并/串到
// 工作区视图里 —— 闸门在缓冲之前就 return，所以 Hub 正文绝不该出现在 Project 视图里。
eq(textOf(views().s_normal).includes('漏进来了？'), false, 'wire()：Hub 的 chunk 正文不会串进工作区视图')

// ② 工作区会话的 tool_run_end(todos) 仍写会话待办（Hub 侧 Project 待办依赖同一条 Project 路径）
push({ event_type: 'tool_run_start', session_id: 's_normal', run_id: 'rw', id: 'call_todo', name: 'todo_write' })
push({
  event_type: 'tool_run_end', session_id: 's_normal', run_id: 'rw', id: 'call_todo', name: 'todo_write',
  result: 'ok', todos: [{ content: '写报告', status: 'pending' }],
})
await sleep(40)
eq((views().s_normal?.todos ?? []).length, 1, 'wire()：工作区会话的 tool_run_end(todos) 仍写 views[sid].todos')

// ③ 空 sid（无归属事件）不能被当成 Hub：回退活跃会话，且不建 '' 视图
useAppStore.setState({ activeSessionId: 's_normal' })
push({ event_type: 'agent_start', session_id: '', run_id: 'rz' })
await sleep(40)
eq(views()[''], undefined, 'wire()：空 sid 不建 "" 视图（空 sid 不是 Hub，走既有回退）')
eq(hubViewKeys().length, 0, 'wire()：空 sid 事件不建 hub.* 视图')

// ④ command_response 是闸门的**例外**：它不建视图，而 pending / snapshot 的结算必须走完
//    （挡掉 = 发消息等 10s 超时）。上面的 s_normal 视图就是 list / new_session 两条
//    command_response 走通的结果；这里再补一条**带 hub 形状 session_id** 的响应，确认这一支
//    既不抛错也不建视图（真 bridge 的 command_response 没有顶层 session_id，见 main.go 的
//    respond —— 这条只是把「万一有」的边界钉住）。
eq(useAppStore.getState().bridgeStatus, 'running', 'wire()：list/new_session 的 command_response 已结算（bridge running、视图已恢复）')
dispatchEvent({ event_type: 'command_response', id: 987654, ok: true, session_id: 'hub.ag_x.dm', data: {} })
eq(hubViewKeys().length, 0, 'wire()：带 hub 形状 session_id 的 command_response 也不建视图（例外支生效）')

// ============ 汇总 ============
console.log('')
if (fails.length > 0) {
  console.log(`❌ ${fails.length} 条失败 / ${pass} 条通过`)
  for (const f of fails) console.log(`   · ${f}`)
  process.exit(1)
}
console.log(`✅ 全部通过（${pass} 条）`)
