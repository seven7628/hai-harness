// 资料页 Provider/Model/Effort 选项推导回归（UI-2）。
//
// 用法：npm run test:hub-model-options
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const out = join(mkdtempSync(join(tmpdir(), 'hub-model-options-')), 'agentModelOptions.mjs')
await build({
  entryPoints: [join(__dirname, '../src/hub/agentModelOptions.ts')],
  bundle: true,
  format: 'esm',
  outfile: out,
  logLevel: 'silent',
})
const {
  EFFORT_VALUES, chooseModelOnProviderSwitch, effortChoices, effortLabel,
  modelsForProvider, newAgentDefaults, providerOptions, unlistedValue,
} = await import(out)

// 1) Provider 过滤：排除 active / deleted / 非对象；label 缺省回落 id；保留配置顺序。
const settings = {
  active: 'deepseek',
  deepseek: { label: 'DeepSeek', models: { b: 1, a: 2 } },
  openai: { models: { 'gpt-4o': 128000 } },
  gone: { deleted: true, models: {} },
  broken: 'oops', // 纯字符串条目
  nullish: null,
  arr: [], // 数组也是 object，但畸形
}
assert.deepEqual(
  providerOptions(settings),
  [{ id: 'deepseek', label: 'DeepSeek' }, { id: 'openai', label: 'openai' }],
  'provider options must exclude active/deleted/malformed and fall back to id',
)
assert.deepEqual(providerOptions(null), [], 'missing settings → no providers')
assert.deepEqual(providerOptions({ active: 'x' }), [], 'only active → no providers')

// 2) 模型并集：cfg.models 的键 ∪ fetchedModels，去重 + 按名字排序；畸形 models 当空。
assert.deepEqual(
  modelsForProvider(settings, { deepseek: ['c', 'a', '', 7] }, 'deepseek'),
  ['a', 'b', 'c'],
  'model union must dedupe, drop junk entries and sort',
)
assert.deepEqual(
  modelsForProvider({ p: { models: 'oops' } }, { p: ['z'] }, 'p'),
  ['z'],
  'string models value must not throw; fetched models still used',
)
assert.deepEqual(modelsForProvider({ p: { models: ['m'] } }, null, 'p'), [], 'array models value is malformed')
assert.deepEqual(modelsForProvider(settings, { openai: ['gpt-4o', 'gpt-4o-mini'] }, 'openai'), ['gpt-4o', 'gpt-4o-mini'])
assert.deepEqual(modelsForProvider(settings, null, 'missing'), [], 'unknown provider without fetched → empty')

// 3) 未知当前值不能静默丢弃：UI 用 unlistedValue 渲染一条 disabled「当前值」。
const knownModels = ['deepseek-v4-pro']
assert.equal(unlistedValue('deepseek-v4-flash', knownModels), 'deepseek-v4-flash', 'unknown model must be preserved')
assert.equal(unlistedValue('deepseek-v4-pro', knownModels), null, 'known model needs no extra option')
assert.equal(unlistedValue('', knownModels), null, 'empty value needs no extra option')

// 4) 切换 Provider：当前模型在新列表就保留，否则第一个，列表空则清空。
assert.equal(chooseModelOnProviderSwitch('b', ['a', 'b']), 'b')
assert.equal(chooseModelOnProviderSwitch('c', ['a', 'b']), 'a')
assert.equal(chooseModelOnProviderSwitch('c', []), '')
assert.equal(chooseModelOnProviderSwitch('', ['a']), 'a')

// 5) 8 个档位：'' + None/Minimal/Low/Medium/High/XHigh/Max（value 小写）。
const choices = effortChoices()
assert.equal(choices.length, 8, 'effort select must offer 8 choices')
assert.deepEqual(
  choices.map((c) => c.value),
  ['', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'],
)
assert.deepEqual(
  choices.slice(1).map((c) => c.label),
  ['None', 'Minimal', 'Low', 'Medium', 'High', 'XHigh', 'Max'],
)
assert.deepEqual([...EFFORT_VALUES], ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'])
assert.equal(effortLabel('xhigh'), 'XHigh', 'inspector must show XHigh, not xhigh')
assert.equal(effortLabel('max'), 'Max')
assert.equal(effortLabel(''), '', 'default label is supplied by i18n')
assert.equal(effortLabel(undefined), '')
assert.equal(effortLabel('turbo'), 'turbo', 'unknown effort must not be rewritten')

// 6) 新智能体默认值：
//    active 有效 → 用它；全局模型属于该 Provider → 保留；否则第一个模型。
const defaultsSettings = {
  active: 'deepseek',
  deepseek: { models: { 'deepseek-v4-pro': 1, 'deepseek-v4-flash': 1 } },
  openai: { models: { 'gpt-4o': 1 } },
  gone: { deleted: true, models: { x: 1 } },
}
assert.deepEqual(newAgentDefaults(defaultsSettings, {}, 'deepseek-v4-pro'), {
  provider: 'deepseek', model: 'deepseek-v4-pro',
})
assert.deepEqual(newAgentDefaults(defaultsSettings, {}, 'gpt-4o'), {
  provider: 'deepseek', model: 'deepseek-v4-flash',
}, 'global model of another provider → first model of default provider')
assert.deepEqual(newAgentDefaults(defaultsSettings, { deepseek: ['aaa'] }, 'aaa'), {
  provider: 'deepseek', model: 'aaa',
}, 'fetched model counts as belonging to the provider')

// active 不在可选项（为空/已删除）→ 第一个可用 Provider。
assert.deepEqual(newAgentDefaults({ ...defaultsSettings, active: 'gone' }, {}, ''), {
  provider: 'deepseek', model: 'deepseek-v4-flash',
})
assert.deepEqual(newAgentDefaults({ ...defaultsSettings, active: '' }, {}, ''), {
  provider: 'deepseek', model: 'deepseek-v4-flash',
})
assert.deepEqual(newAgentDefaults({}, {}, 'deepseek-v4-pro'), { provider: '', model: '' }, 'nothing configured → empty')

// Provider 存在但没有模型 → model 留空（UI 显示「该 Provider 还没有模型」）。
const emptyProvider = { active: 'p', p: { models: {} } }
assert.deepEqual(newAgentDefaults(emptyProvider, {}, 'deepseek-v4-pro'), { provider: 'p', model: '' })

console.log('agent model options: 31/31 assertions passed')
