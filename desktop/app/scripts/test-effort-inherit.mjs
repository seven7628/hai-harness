// effort 生效性回归（真实 store + 真实 mock transport + esbuild 打包，Node 直接跑）。
//
// 回答两个问题：
//   1) 客户端切 effort 是否真的把命令发给 bridge（7 档原样发，不在客户端做归一化 ——
//      档位适配由 provider 按 model + provider 完成）。
//   2) 新建/恢复会话「继承历史配置的 effort」是否真的落到 bridge（会话级记录 > 全局默认），
//      而不是停留在 UI 显示。
//
// 观测方式：包住 transport.send，记录实际发出的命令（bridge 侧收到的就是这些）。
//
// 用法：npm run test:effort
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)

// 1) localStorage shim：预置「用户上次的选择」（全局偏好）与会话级记录
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}
const seedPrefs = (globalEffort, sessionEfforts = {}) => {
  mem.set('go-code.prefs', JSON.stringify({ mode: 'auto', model: 'deepseek-v4-pro', provider: 'opencode', effort: globalEffort, persona: 'code' }))
  mem.set('go-code.sessionPrefs', JSON.stringify(Object.fromEntries(
    Object.entries(sessionEfforts).map(([sid, eff]) => [`/mock/w\u0000${sid}`, { mode: 'hitl', model: 'deepseek-chat', provider: 'deepseek', effort: eff, persona: 'code' }]),
  )))
}
seedPrefs('high')

// 2) esbuild 打包 store + transport（同一模块实例：store 里的 transport 单例与测试拿到的是同一个对象）
const dir = mkdtempSync(join(tmpdir(), 'effort-inherit-'))
const outfile = join(dir, 'bundle.cjs')
await build({
  stdin: {
    contents: "export * from './src/store/useAppStore'\nexport { getTransport } from './src/transport'\n",
    resolveDir: join(__dirname, '..'),
    loader: 'ts',
  },
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' }, // mock.ts 的 ?raw/?url 资源

})
const { useAppStore, applySessionPrefs, getTransport } = require(outfile)

// 3) 命令记录：包住 transport.send（store 模块级 const transport = getTransport() 同一对象）
const transport = getTransport()
const sent = []
const origSend = transport.send.bind(transport)
transport.send = (cmd) => {
  sent.push(cmd)
  return origSend(cmd)
}
const sentEfforts = () => sent.filter((c) => c.type === 'switch_effort').map((c) => c.payload.effort)

// 会话/Provider 配置形状（与场景 7 同构：模型必须在 Provider 的 models 表里）
const settings = useAppStore.getState().settings
const baseState = {
  workspace: '/mock/w',
  bridgeStatus: 'running',
  settings: {
    ...settings,
    provider: {
      ...settings.provider,
      active: 'deepseek',
      deepseek: { ...settings.provider.deepseek, models: { 'deepseek-chat': 128000 } },
      opencode: { ...settings.provider.opencode, models: { 'deepseek-v4-pro': 1000000 } },
    },
  },
  sessions: [{ id: 's1', title: 'A' }, { id: 's2', title: 'B' }],
  projects: [{ path: '/mock/w', sessions: [{ id: 's1', title: 'A' }, { id: 's2', title: 'B' }] }],
}

// ===== 场景 1：客户端切 effort → 命令原样发出（7 档，客户端不做归一化） =====
// 注意：switchEffort 会 persistPrefs 落盘（全局 + 会话级），所以每个场景前重新 seed。
seedPrefs('high')
sent.length = 0
useAppStore.setState({ ...baseState, activeSessionId: 's1' })
for (const key of ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max']) {
  useAppStore.getState().switchEffort(key)
}
assert.deepStrictEqual(
  sentEfforts(),
  ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'],
  'switchEffort 必须把 7 档原样发给 bridge（档位适配在 provider 侧）',
)
assert.equal(useAppStore.getState().effort, 'max', 'UI 状态应为最后一次选择')
console.log('PASS 场景1：客户端切 effort 原样下发 7 档（含 none/xhigh）')

// ===== 场景 2：新建会话继承「全局默认 effort」（无会话级记录）→ 真的发给 bridge =====
seedPrefs('high') // 全局偏好 high，无 s2 会话级记录
sent.length = 0
// bridge 新建会话的初始档位是 low（create 的 effort="" → low）；随后前端按偏好补发 switch_effort
useAppStore.setState({ ...baseState, activeSessionId: 's2', effort: 'low' })
applySessionPrefs('/mock/w', 's2')
assert.deepStrictEqual(sentEfforts(), ['high'], '新会话应继承全局偏好 effort=high 并同步给 bridge')
assert.equal(useAppStore.getState().effort, 'high', 'UI 应同步为继承到的档位')
console.log('PASS 场景2：新建会话继承全局偏好 effort 并下发 bridge')

// ===== 场景 3：已有会话按「会话级记录」恢复（优先于全局默认） =====
seedPrefs('high', { s1: 'max' }) // 全局 high + s1 会话级 max
sent.length = 0
useAppStore.setState({ ...baseState, activeSessionId: 's1', effort: 'low' })
applySessionPrefs('/mock/w', 's1')
assert.deepStrictEqual(sentEfforts(), ['max'], '会话级记录 effort=max 应优先于全局 high')
assert.equal(useAppStore.getState().effort, 'max', 'UI 应恢复为会话级档位')
console.log('PASS 场景3：会话级记录优先于全局默认，且下发 bridge')

// ===== 场景 4：无任何记录（全新环境）→ 保持 bridge 默认 low，不发多余命令 =====
sent.length = 0
mem.delete('go-code.sessionPrefs')
mem.delete('go-code.prefs')
useAppStore.setState({ ...baseState, activeSessionId: 's2', effort: 'low' })
applySessionPrefs('/mock/w', 's2')
assert.deepStrictEqual(sentEfforts(), ['low'], '无记录时按当前值（= bridge 默认 low）下发，保持会话自洽')
console.log('PASS 场景4：无记录时维持默认档位')

console.log('EFFORT_INHERIT_TEST_PASS')
