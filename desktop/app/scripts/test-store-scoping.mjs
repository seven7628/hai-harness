#!/usr/bin/env node
// R0 验收（store 侧，Node 直接跑，无需浏览器/Electron）：删除会话 / 删除工作区 → 清理内嵌预览视图。
//
// 为什么这组断言有意义（防的回归）：
//   内嵌视图池是**进程级单例**（electron/embed.ts 的 views Map），每个视图 = 一个独立渲染进程。
//   UI 删掉会话后若不清池：僵尸渲染进程 + 内存泄漏，而且 Go 侧 Enumerate() 仍能看到「已被用户
//   删掉的会话」的页面（用户视角：删了还在）。
//   清理必须**按 sid 精确匹配**：
//     · 误伤自举视图（sid === ''，attach / 页面枚举载体）→ 浏览器工具整条链路失效；
//     · 误伤别的会话的视图 → 直接砸掉别人正在看的页面（跨会话泄漏）。
//   deleteSession / deleteWorkspace 都是**同步**函数，清理是 `void (async () => …)()` 的即发即忘 →
//   断言前必须「等一拍」。这里用**轮询**而不是固定 sleep：实现同步完成时不白等，慢一拍也不误判；
//   负向断言（「不该关的没被关」）额外给一小段宽限，保证同一批清理都已入账。
//
// 用法：cd desktop/app && npm run test:store:scoping
// 退出码：全通过 0；任一断言失败非 0（TDD「先红」阶段靠这个非 0 码证明脚本有效）。
import { build } from 'esbuild'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

// 1) shim localStorage（store 模块加载时读取项目树/标题覆盖/偏好）
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}

// 2) mock window.desktop（**必须在 require bundle 之前**：store 模块加载时按
//    `typeof window !== 'undefined' && window.desktop` 选 transport —— 有 desktop 就走
//    electronTransport，send() 会调 d.command()。所以 command 必须给桩，否则 deleteSession 里的
//    send(...) 会同步抛错，根本走不到被测的清理分支）。
//    embedStatus / embedCloseTarget 是**被测对象**：清理逻辑必须按 targets[].sid 过滤。
const closed = []
// mockTargets 可变：场景 3 会把它换成「R0 之前的 Electron 形态」（targets 无 sid），
// 验证清理不会因为「匹配不到 sid」而退化成「全关」。
let mockTargets = [
  { id: 't1', sid: 's1', url: 'file:///a.html' },
  { id: 't2', sid: 's2', url: 'file:///b.html' },
  { id: 't3', sid: '', url: 'about:blank' }, // 自举视图：不属于任何会话，绝不能被关
]
global.window = {
  desktop: {
    command: async () => ({ ok: true }),
    embedStatus: async () => ({
      ready: true, visible: true, attached: true, url: '', title: '', activeId: 't1',
      targets: mockTargets,
    }),
    embedCloseTarget: async (id) => { closed.push(id); return { ok: true } },
    // 与清理无关的相邻 API 给无副作用桩：实现若顺带调用（例如顺带切会话）也不该炸；
    // 它们不是断言对象，只保证被测分支能真实跑到底。
    embedSetSession: async () => ({ ok: true }),
    embedOpenTarget: async () => ({ ok: true }),
    embedActivate: async () => ({ ok: true }),
  },
  // 浏览器同款时间函数：store 内部若用 window.setTimeout 做即发即忘调度也能正常工作
  setTimeout: (...a) => setTimeout(...a),
  clearTimeout: (...a) => clearTimeout(...a),
}

// L0（D6）：bridge.ts 在模块求值时向 preload 交接一次（宿主私有）。node 里没有 preload，
// 这里用一次 claim 供货 —— 每个 require 副本 = 一次冷启动，都该拿到当时那份 fake。
global.window.__haiClaimHostApi = () => global.window.desktop ?? null

// 3) esbuild 打包真实 store 到临时 CJS 后 require（与 test-store-queue.mjs 同策略）
const dir = mkdtempSync(join(tmpdir(), 'store-scoping-'))
const outfile = join(dir, 'store-bundle.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' }, // mock.ts 的 ?raw/?url 资源
})
const { useAppStore } = require(outfile)

const WS = '/mock/ws'
const OTHER = '/mock/other'

// setup：铺好「工作区 WS 含会话 s1/s2」的前置状态。
// workspace 故意**不是** WS：deleteWorkspace 走「非活跃工作区」分支，断言不被
// setActiveWorkspace 的额外动作干扰（那属于别的测试的关注点）。
function setup() {
  useAppStore.setState({
    workspace: OTHER,
    activeSessionId: undefined,
    projects: [
      { path: WS, sessions: [{ id: 's1', title: 'A' }, { id: 's2', title: 'B' }] },
      { path: OTHER, sessions: [] },
    ],
    views: {},
    recentSessions: [],
    titleOverrides: {},
  })
}

// 即发即忘 → 轮询等待清理入账，再给一拍宽限（负向断言必须等整批跑完，否则空转也能「通过」）
async function waitCleanup(pred, timeout = 1500) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    if (pred()) break
    await sleep(25)
  }
  await sleep(80)
}

async function main() {
  // ===== 前置硬检查（不用 skip：缺失正是本脚本要暴露的事实）=====
  assert(typeof global.window.desktop.embedStatus === 'function',
    `mock window.desktop.embedStatus 必须存在（实际 typeof = ${typeof global.window.desktop.embedStatus}）`)
  assert(typeof global.window.desktop.embedCloseTarget === 'function',
    `mock window.desktop.embedCloseTarget 必须存在（实际 typeof = ${typeof global.window.desktop.embedCloseTarget}）`)
  assert(typeof useAppStore.getState().deleteSession === 'function',
    `store 必须有 deleteSession（实际 typeof = ${typeof useAppStore.getState().deleteSession}）`)
  assert(typeof useAppStore.getState().deleteWorkspace === 'function',
    `store 必须有 deleteWorkspace（实际 typeof = ${typeof useAppStore.getState().deleteWorkspace}）`)
  console.log('PASS 前置：embedStatus / embedCloseTarget / deleteSession / deleteWorkspace 均已接线')

  // ===== 场景 1：deleteSession(WS,'s1') → 只关 s1 的视图（t1），不碰 t2/t3 =====
  setup()
  closed.length = 0
  useAppStore.getState().deleteSession(WS, 's1')
  await waitCleanup(() => closed.includes('t1'))
  assert(closed.includes('t1'),
    `deleteSession 应即发即忘关闭该会话的内嵌视图：sid='s1' → 应关 t1；实际 closed=${JSON.stringify(closed)}（等待 1.5s 后仍未出现 → 清理未实现）`)
  assert(!closed.includes('t2'),
    `不得误伤其它会话的视图：t2 属于 s2，删 s1 不该动它（跨会话泄漏）；实际 closed=${JSON.stringify(closed)}`)
  assert(!closed.includes('t3'),
    `不得关闭自举视图：t3 的 sid='' 不属于任何会话，它是 attach / 页面枚举载体，误关会让浏览器工具整条链路失效；实际 closed=${JSON.stringify(closed)}`)
  console.log('PASS 场景1：deleteSession 只关本会话（sid 精确匹配）的内嵌视图')

  // ===== 场景 2：deleteWorkspace(WS) → 该工作区**全部会话**（s1/s2）的视图都被清理 =====
  setup()
  closed.length = 0
  useAppStore.getState().deleteWorkspace(WS)
  await waitCleanup(() => closed.includes('t2'))
  assert(closed.includes('t2'),
    `deleteWorkspace 应清理该工作区全部会话的视图：WS 含 s1/s2 → 应关 t2（sid='s2'）；实际 closed=${JSON.stringify(closed)}（等待 1.5s 后仍未出现 → 工作区级清理未实现）`)
  assert(closed.includes('t1'),
    `deleteWorkspace 应清理该工作区全部会话的视图：WS 含 s1/s2 → 也应关 t1（sid='s1'）；实际 closed=${JSON.stringify(closed)}`)
  assert(!closed.includes('t3'),
    `deleteWorkspace 同样不得关闭自举视图（sid='' 不属于任何工作区会话）；实际 closed=${JSON.stringify(closed)}`)
  console.log('PASS 场景2：deleteWorkspace 清理该工作区全部会话的内嵌视图，且不动自举视图')

  // ===== 场景 3（防御）：targets 缺 sid（R0 之前的 Electron 形态）→ 宁可漏关，不得错关 =====
  // 为什么有意义：视图池是进程级单例，「匹配不到 sid」时的省事写法（`if (!t.sid) close()` /
  // 不过滤直接全关）会把**别的会话**正在看的页面一起砸掉 —— 用户视角是「删一个会话，
  // 另一个会话的预览突然消失」。会话归属未知时必须不动作。
  setup()
  closed.length = 0
  mockTargets = [{ id: 't1', url: 'file:///a.html' }, { id: 't2', url: 'file:///b.html' }]
  useAppStore.getState().deleteSession(WS, 's1')
  await waitCleanup(() => closed.length > 0)
  assert(closed.length === 0,
    `targets 缺 sid 时不得退化成「全关」：归属未知的视图宁可漏关也不能错关；实际 closed=${JSON.stringify(closed)}`)
  useAppStore.getState().deleteWorkspace(WS)
  await waitCleanup(() => closed.length > 0)
  assert(closed.length === 0,
    `deleteWorkspace 同样不得关掉 sid 未知的视图；实际 closed=${JSON.stringify(closed)}`)
  console.log('PASS 场景3：targets 缺 sid 时清理不动作（不误伤未知归属的视图）')
  mockTargets = [
    { id: 't1', sid: 's1', url: 'file:///a.html' },
    { id: 't2', sid: 's2', url: 'file:///b.html' },
    { id: 't3', sid: '', url: 'about:blank' },
  ]
}

main().then(() => {
  console.log(`\n✅ store 会话视图清理：${pass} 项全部通过`)
  process.exit(0)
}).catch((e) => {
  console.error(`\n❌ ${e?.message ?? e}`)
  process.exit(1)
})
