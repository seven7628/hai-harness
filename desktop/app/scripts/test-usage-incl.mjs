// usage 口径 / 缓存 TTL 标注（usage_input_includes_cache、cache_ttl_1h）store 回归：真实 store + mock transport，Node 直接跑。
//
// 覆盖（与 electron provider:save / bridge set_provider 三处同形）：
//   ① saveProvider(true/false) → settings.provider[pid].usage_input_includes_cache 落盘
//   ② saveProvider(null) → 字段被删除（回「自动」判定；「不存在」与「false」语义不同）
//   ③ 未带该键的保存路径（改 base_url 等）不得清掉已存标注
//   ④ cache_ttl_1h（Anthropic 缓存 TTL 标注）三态同上：true=1h / false=5m / null=清除
//
// 用法：node scripts/test-usage-incl.mjs
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

const dir = mkdtempSync(join(tmpdir(), 'usage-incl-'))
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

const prov = (pid) => {
  const s = useAppStore.getState().settings
  const cfg = s.provider[pid]
  return cfg && typeof cfg === 'object' ? cfg : undefined
}

await useAppStore.getState().start('/mock/usage-incl')

// ① 显式标注 true / false → 落盘
assert.equal(await useAppStore.getState().saveProvider({ provider: 'aggregator', base_url: 'https://gateway.example.com', protocol: 'anthropic', usage_input_includes_cache: true }), true, 'saveProvider(true) 成功')
assert.equal(prov('aggregator')?.usage_input_includes_cache, true, 'true 落盘（input_tokens 已含缓存）')
await useAppStore.getState().saveProvider({ provider: 'aggregator', usage_input_includes_cache: false })
assert.equal(prov('aggregator')?.usage_input_includes_cache, false, 'false 落盘（Anthropic 语义：未含缓存）')

// ③ 其它保存路径（不带该键）不得清掉标注
await useAppStore.getState().saveProvider({ provider: 'aggregator', base_url: 'https://gateway.example.com/v2' })
assert.equal(prov('aggregator')?.usage_input_includes_cache, false, '改 base_url 不动 usage 口径标注')

// ② 显式 null → 删除字段（回自动）
await useAppStore.getState().saveProvider({ provider: 'aggregator', usage_input_includes_cache: null })
assert.equal(Object.prototype.hasOwnProperty.call(prov('aggregator') ?? {}, 'usage_input_includes_cache'), false, 'null 清除标注（字段必须消失，不能留 false）')

// ④ 缓存 TTL 标注（cache_ttl_1h）：同三态；语义 = Anthropic 断点 TTL（1h / 5m / 端点默认）
await useAppStore.getState().saveProvider({ provider: 'aggregator', cache_ttl_1h: true })
assert.equal(prov('aggregator')?.cache_ttl_1h, true, 'cache_ttl_1h=true 落盘（1h extended TTL）')
await useAppStore.getState().saveProvider({ provider: 'aggregator', cache_ttl_1h: false })
assert.equal(prov('aggregator')?.cache_ttl_1h, false, 'cache_ttl_1h=false 落盘（5m）')
await useAppStore.getState().saveProvider({ provider: 'aggregator', base_url: 'https://gateway.example.com/v3' })
assert.equal(prov('aggregator')?.cache_ttl_1h, false, '改 base_url 不动 TTL 标注')
await useAppStore.getState().saveProvider({ provider: 'aggregator', cache_ttl_1h: null })
assert.equal(Object.prototype.hasOwnProperty.call(prov('aggregator') ?? {}, 'cache_ttl_1h'), false, 'null 清除 TTL 标注（回端点默认）')
assert.equal(prov('aggregator')?.usage_input_includes_cache, undefined, 'TTL 与 usage 口径两个标注互不影响')

console.log('USAGE_INCL_TEST_PASS')
