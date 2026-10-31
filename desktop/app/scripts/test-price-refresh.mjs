// 「从价源补价」端到端 store 回归：真实 store + mock transport，Node 直接跑。
//
// 覆盖（用户实测「拉取模型列表后补价，重启没生效」的整条链路 + 2026-09-21 决策「直接覆盖」）：
//   ① refreshPrices → 桥回传整表 → **store 经 providerSave 落盘**（settings.provider[x].prices）
//      —— 少了这步价格只活在进程里，重启即丢（桥刻意只改内存，settings.json 是 Electron 独占写者）
//   ② 默认**覆盖**语义（用户决策「直接覆盖就行」）：手填价也会被实时价重写；
//      overwrite:false 才走保守路径（只补无价模型，手填价不动）
//   ③ 拉取模型列表（list_models）后自动补价且**覆盖**（用户决策「拉取列表的时候自动覆盖」）
//   ④ 落盘结果在回读 settings 与 localStorage blob 里都可见（= 重启水合路径）
//
// 用法：node scripts/test-price-refresh.mjs
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)

const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}

const dir = mkdtempSync(join(tmpdir(), 'price-refresh-'))
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

const cfg = (pid) => {
  const s = useAppStore.getState().settings
  const c = s.provider[pid]
  return c && typeof c === 'object' ? c : undefined
}
const waitFor = async (fn, ms = 3000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < ms) {
    if (fn()) return true
    await new Promise((r) => setTimeout(r, 25))
  }
  return false
}

await useAppStore.getState().start('/mock/price-refresh')

// 建一个「聚合网关」provider：两个模型，其中 m1 已有手填价（补价不得动它）
assert.equal(await useAppStore.getState().saveProvider({
  provider: 'gw',
  base_url: 'https://gw.example.com/v1',
  protocol: 'chat_completions',
  models: { m1: 128000, m2: 128000 },
  prices: { m1: { input: 9.99, cache_read: 0.999, cache_write: 0, output: 19.99 } },
}), true, 'saveProvider 建 provider 成功')
assert.deepEqual(Object.keys(cfg('gw')?.prices ?? {}), ['m1'], '初始只有 m1 有手填价')

// ① + ② 默认补价 = 覆盖：m1 的手填价被实时价重写、m2 补上；结果落盘
const r1 = await useAppStore.getState().refreshPrices({ provider: 'gw' })
assert.ok(r1, 'refreshPrices 返回摘要（非 undefined）')
assert.deepEqual(r1.filled, ['m1', 'm2'], `默认覆盖：全部模型都被更新（实得 ${JSON.stringify(r1.filled)}）`)
assert.equal(r1.filledCount, 2, 'filled_count = 2')
assert.equal(r1.skippedCount, 0, 'skipped_count = 0（价源都收录了）')
assert.equal(r1.total, 2, 'total = 2')
assert.ok(r1.source.length > 0, `source 非空（实得 ${r1.source}）`)

const prices = cfg('gw')?.prices ?? {}
assert.ok(prices.m1 && prices.m1.input !== 9.99, `默认覆盖：手填价被实时价重写（实得 ${JSON.stringify(prices.m1)}）`)
assert.ok(prices.m2 && prices.m2.input > 0, `m2 补上了价（实得 ${JSON.stringify(prices.m2)}）`)

// ④ 落盘 → 重启等价路径：
//   - store.settings 是 saveProvider 内 transport.settingsGet() 回读的结果（= 重启水合路径）；
//   - localStorage 的 settings blob 是 mock 的「settings.json」，含价才算真持久化。
const persisted = useAppStore.getState().settings.provider.gw?.prices
assert.ok((persisted?.m2?.input ?? 0) > 0, '回读 settings 里有 m2 价（重启水合路径可见）')
const raw = JSON.parse(localStorage.getItem('go-code.settings') ?? '{}')
assert.ok(raw.provider?.gw?.prices?.m2, '落盘 blob 里含 m2 价（持久化生效，而非只改内存）')

// ② 的保守分支仍在：overwrite:false → 只补无价模型，手填价保留
await useAppStore.getState().saveProvider({ provider: 'gw', prices: { m1: { input: 9.99, cache_read: 0.999, cache_write: 0, output: 19.99 } } })
const r2 = await useAppStore.getState().refreshPrices({ provider: 'gw', overwrite: false })
assert.ok(r2, 'overwrite:false 有摘要')
assert.deepEqual(r2.filled, ['m2'], `保守路径只补无价模型（实得 ${JSON.stringify(r2.filled)}）`)
assert.deepEqual(cfg('gw')?.prices?.m1, { input: 9.99, cache_read: 0.999, cache_write: 0, output: 19.99 }, '保守路径保留手填价')

// ③ 拉取模型列表 → 自动补价且覆盖（用户决策：拉列表时自动覆盖）
await useAppStore.getState().saveProvider({ provider: 'gw', prices: { m1: { input: 9.99, cache_read: 0.999, cache_write: 0, output: 19.99 } } })
useAppStore.getState().fetchModels('gw')
const chained = await waitFor(() => (cfg('gw')?.prices?.m1?.input ?? 9.99) !== 9.99)
assert.ok(chained, '拉取模型列表后自动补价且覆盖了手填价')

console.log('PRICE_REFRESH_TEST_PASS')
