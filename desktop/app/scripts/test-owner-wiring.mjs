#!/usr/bin/env node
// 归属注入的**接线**验收（Node 直跑，不启 Electron/浏览器）。
//
// 防的回归（独立 Review B1）：
//   useAppStore.send() 只读 activeSessionId → Hub DM/群 下打开设置时，hooks_trust /
//   plugin_enable / im_route_* 被写成 workspace:<工作区>，而派发按 agent:/group: 查
//   → 「明明确认过了却不生效」。这组断言走真实 store（不是纯函数），确认 send() 在
//   三种上下文下注入的 owner 与后端派生一致；另外用源码守卫钉住 IMSettingsEditor /
//   HooksTab 必须是同一派生（不能再各算一份）。
//
// 用法：cd desktop/app && npm run test:owner-wiring
// 退出码：全过 0；任一断言失败非 0。
import { build } from 'esbuild'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = dirname(fileURLToPath(import.meta.url))
const src = (...p) => join(__dirname, '..', 'src', ...p)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

// 1) shim 浏览器环境（store 模块加载时读 localStorage / 选 transport）
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}
const sent = []
const listeners = new Set()
global.window = {
  desktop: {
    command: async (cmd) => { sent.push(cmd); return { ok: true, data: {} } },
    // send() 带响应回调时走 onCommandResponseOnce → 需要事件通道（桩：只收集不投递）
    onEvent: (cb) => { listeners.add(cb); return () => listeners.delete(cb) },
  },
}

// 2) 打包真实 store（含依赖），再在 Node 里调用其 action
// 必须**同一个 bundle**里拿 store 与 activeOwner：分开打包会得到两份模块实例，
// setHubOwnerContext 写的那份与 send() 读的那份不是同一个变量（测试会假失败）。
const r = await build({
  stdin: {
    contents: `export { useAppStore } from './src/store/useAppStore'
export { setHubOwnerContext, resetHubOwnerContext, getHubOwnerContext } from './src/lib/activeOwner'`,
    resolveDir: join(__dirname, '..'),
    sourcefile: 'owner-wiring-entry.ts',
    loader: 'ts',
  },
  bundle: true,
  platform: 'node',
  format: 'cjs',
  write: false,
  logLevel: 'silent',
  external: ['electron'],
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})
const mod = { exports: {} }
new Function('module', 'exports', 'require', r.outputFiles[0].text)(mod, mod.exports, (m) => {
  throw new Error(`unexpected require(${m})`)
})
const { useAppStore, setHubOwnerContext, resetHubOwnerContext } = mod.exports
assert(typeof useAppStore?.getState === 'function', 'store 可加载（useAppStore.getState 存在）')

const lastCmd = (type) => [...sent].reverse().find((c) => c.type === type)

// 3) 上下文 → 注入的 owner
const WS = '/proj/x'
useAppStore.setState({ workspace: WS, activeSessionId: 's-project' })

resetHubOwnerContext()
sent.length = 0
useAppStore.getState().fetchHooks([WS])
let cmd = lastCmd('hooks_list')
assert(cmd?.payload?.owner === `workspace:${WS}`, `Hub 关闭：hooks_list 注入 workspace:<工作区>（实际 ${cmd?.payload?.owner}）`)

setHubOwnerContext({ open: true, sid: 'hub.ag1.dm', groupId: null })
sent.length = 0
useAppStore.getState().fetchHooks([WS])
cmd = lastCmd('hooks_list')
assert(cmd?.payload?.owner === 'agent:ag1', `Hub DM：hooks_list 注入 agent:ag1（实际 ${cmd?.payload?.owner}）`)

setHubOwnerContext({ open: true, sid: null, groupId: 'grp1' })
sent.length = 0
useAppStore.getState().fetchHooks([WS])
cmd = lastCmd('hooks_list')
assert(cmd?.payload?.owner === 'group:grp1', `Hub 群：hooks_list 注入 group:grp1（实际 ${cmd?.payload?.owner}）`)

// 启停（plugin_enable/disable）同样按 owner —— 插件 tab 的开关不能落到项目归属
sent.length = 0
useAppStore.getState().enablePlugin('im')
cmd = lastCmd('plugin_enable')
assert(cmd?.payload?.owner === 'group:grp1', `plugin_enable 注入 group:grp1（实际 ${cmd?.payload?.owner}）`)

// Hub 打开但没选中目标：不注入（服务端明确拒绝），绝不能猜成 workspace
setHubOwnerContext({ open: true, sid: null, groupId: null })
sent.length = 0
useAppStore.getState().fetchHooks([WS])
cmd = lastCmd('hooks_list')
assert(cmd && cmd.payload?.owner === undefined, `Hub 未选中：不注入 owner（实际 ${cmd?.payload?.owner}）`)

// 显式 owner 不被覆盖（项目来源的确认要写给那个项目）
setHubOwnerContext({ open: true, sid: 'hub.ag1.dm', groupId: null })
sent.length = 0
useAppStore.getState().trustHooks(['echo hi'], 'project', `workspace:${WS}`)
cmd = lastCmd('hooks_trust')
assert(cmd?.payload?.owner === `workspace:${WS}`, `显式 owner 优先（实际 ${cmd?.payload?.owner}）`)
resetHubOwnerContext()

// 4) 源码守卫：设置页组件与 send() 必须用同一派生（不能各算一份）
const guard = (file, patterns, banned = []) => {
  const text = readFileSync(src(...file), 'utf8')
  for (const p of patterns) {
    assert(text.includes(p), `${file.join('/')} 使用 ${p}`)
  }
  for (const b of banned) {
    assert(!text.includes(b), `${file.join('/')} 不再直接使用 ${b}`)
  }
}
guard(['store', 'useAppStore.ts'], ['resolveActiveOwner'], ['sessionOwner('])
guard(['components', 'IMSettingsEditor.tsx'], ['ownerForContext'], ['sessionOwner('])
guard(['components', 'HooksTab.tsx'], ['ownerValid'])
guard(['App.tsx'], ['setHubOwnerContext'])

console.log(`\n${pass} 项断言全部通过（owner 接线）`)
