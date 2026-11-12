// settings-store-test.ts —— settings.json 单写者语义的回归（node:test，不启动 Electron）。
//
// 钉住 §6.1 第 2 条前半（BR-01 / BR-29 / BR-02 的写侧）：
//   ① v1 默认值固化 → v2 迁移：删默认、留未知键/自定义 provider/api_key/deleted/active，
//      写 schema_version:2，幂等；不把默认写进 raw；
//   ② 写侧白名单：未知键（顶层/嵌套）拒绝；只改 patch 涉及的键；explicit 记录显式路径；
//   ③ provider:save / provider:remove / mcp:save 只改自己的段（BR-02 的 Electron 一半）：
//      不补兄弟 provider 默认、不丢未知字段、不丢其它顶层键；
//   ④ 迁移文件 IO：缺文件/坏文件/非对象/备份路径可注入/写失败保留原文件；
//   ⑤ 写即迁移（复审 B2）：写路径先迁移再改，迁移失败**拒绝这次写**且不盖章 schema_version:2；
//   ⑥ main.ts 接线守卫（只作补充：行为断言在 ①–⑤，不再拿源码文本当唯一证据）。
//
// 注意：本任务的写侧**不**碰 provider.*.api_key —— 下面显式断言「未被 patch 的 key 原样保留」。
// key 从 settings 搬到 keys.json 是 BR-08（keys-store.migratePlaintextProviderKeys / main.ts
// provider:save），由 keys-store-test.ts 覆盖；本文件的 `.bak` 逐字节断言因此不受其影响。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, renameSync, rmSync, statSync, unlinkSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  addExplicitPath,
  applyMcpServers,
  applyProviderRemove,
  applyProviderSave,
  applySettingsPatch,
  atomicWriteText,
  commitSettingsWrite,
  deepEqual,
  explicitPathsOf,
  deleteExplicitPath,
  migrateSettingsV2,
  runSettingsMigration,
  sanitizeExplicit,
  type JsonObject,
  type SettingsFsOps,
} from './settings-store'

// ─────────────────────────────── 夹具 ───────────────────────────────

// 与 main.ts 的 DEFAULT_SETTINGS 同形状的裁剪版：迁移规则只依赖「默认值 + 键名」，
// 值本身与真实默认保持一致的语义（provider 内置段 / 动态 map / 空段默认）。
const DEFAULTS: JsonObject = {
  theme: 'dark',
  lang: 'zh',
  provider: {
    active: 'deepseek',
    deepseek: {
      base_url: 'https://api.deepseek.com/v1',
      models: { 'deepseek-chat': 64000 },
      max_tokens: { 'deepseek-chat': 8192 },
      protocol: 'chat_completions',
      input_types: {},
    },
    openai: {
      base_url: 'https://api.openai.com/v1',
      models: { 'gpt-4o': 128000 },
      max_tokens: { 'gpt-4o': 16384 },
      protocol: 'chat_completions',
      input_types: {},
    },
    zhipu: {
      base_url: 'https://open.bigmodel.cn/api/paas/v4',
      models: { 'glm-4.6': 204800 },
      max_tokens: { 'glm-4.6': 131072 },
      protocol: 'chat_completions',
      input_types: {},
    },
  },
  mcpServers: {},
  agent: { reminder_rounds: 30 },
  sandbox: { mode: 'seatbelt', sensitive_paths: [] },
  permissions: { computer_approved_apps: [] },
  stt: { enabled: false },
}

const DEF_PROVIDERS = DEFAULTS.provider as JsonObject
/** 某个内置 provider 的默认段（+ 用户改动）。 */
function builtin(id: string, extra: JsonObject = {}): JsonObject {
  return { ...(DEF_PROVIDERS[id] as JsonObject), ...extra }
}
const MYPROXY: JsonObject = {
  base_url: 'https://proxy.example/v1',
  models: { 'm-1': 32768 },
  max_tokens: { 'm-1': 4096 },
  protocol: 'chat_completions',
  api_key: 'sk-proxy',
  label: 'Proxy',
  future_field: { nested: [1, 2] },
}
const OPENAI_PRICES = { 'gpt-4o': { input: 2.5, cache_read: 1.25, cache_write: 2.5, output: 10 } }

/** v1 真实现场形状：一次无关保存把整份 DEFAULT_SETTINGS 并集写了进去。 */
function v1(rawOverrides: JsonObject = {}): JsonObject {
  return {
    theme: 'dark',
    lang: 'zh',
    provider: {
      active: 'deepseek',
      deepseek: builtin('deepseek', { api_key: 'sk-deepseek' }),
      openai: builtin('openai', {
        api_key: 'sk-openai',
        label: '我的 OpenAI',
        prices: OPENAI_PRICES,
        usage_input_includes_cache: true,
      }),
      // 自定义 provider：不在 defaults 里，没有「内置默认」可比 → 全部保留
      myproxy: MYPROXY,
      // 用户删除过的内置 provider：deleted 标记必须保留（否则 bridge 重启复活）
      zhipu: builtin('zhipu', { deleted: true }),
    },
    mcpServers: {},
    agent: { reminder_rounds: 30 },
    sandbox: { mode: 'seatbelt', sensitive_paths: [] },
    permissions: { computer_approved_apps: [] },
    stt: { enabled: false },
    // 未知顶层键：bridge 段 / 历史残留 / 第三方写入 → 原样保留
    hooks: { pre_tool: [{ name: 'audit' }] },
    runtime: { stop_background_on_interrupt: true },
    ...rawOverrides,
  }
}

/** 迁移后的干净 v2（主题浅色 + deepseek 有 key，用来测写侧 patch 的 diff 面）。 */
function v2(): JsonObject {
  return {
    schema_version: 2,
    theme: 'light',
    provider: { active: 'deepseek', deepseek: { api_key: 'sk-deepseek' } },
    mcpServers: { fs: { command: 'mcp-fs', args: ['--root', '/tmp'] } },
    hooks: { pre_tool: [{ name: 'audit' }] },
  }
}

// ─────────────────────────────── 小工具 ───────────────────────────────

/** 逐路径 diff：对象递归，数组/标量整体比较。返回 {path, before, after} 列表（有序）。 */
function rawDiff(before: unknown, after: unknown, base = ''): Array<{ path: string; before?: unknown; after?: unknown }> {
  const isObj = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v)
  if (isObj(before) && isObj(after)) {
    const keys = Array.from(new Set([...Object.keys(before), ...Object.keys(after)])).sort()
    return keys.flatMap((k) => rawDiff(before[k], after[k], base ? `${base}.${k}` : k))
  }
  if (deepEqual(before, after)) return []
  return [{ path: base, before, after }]
}

function fmt(v: unknown): string {
  if (v === undefined) return '(无)'
  const s = JSON.stringify(v)
  return s.length > 120 ? `${s.slice(0, 117)}…` : s
}

/** 打印一次 raw diff（交付证据：一次 theme patch 到底写了几行）。 */
function printDiff(title: string, before: JsonObject, after: JsonObject): void {
  const d = rawDiff(before, after)
  const lines = d.map((x) => `  ${x.before === undefined ? '+' : x.after === undefined ? '-' : '~'} ${x.path}: ${fmt(x.before)} → ${fmt(x.after)}`)
  console.log(`[evidence] ${title}（${d.length} 处变化）\n${lines.join('\n')}`)
}

/** 内存 fs：可注入失败点，记录每次调用（含 mode），用于验证备份路径/0600/回滚。 */
function fakeIo(files: Record<string, string> = {}) {
  const store = new Map<string, { text: string; mode: number }>()
  for (const [k, v] of Object.entries(files)) store.set(k, { text: v, mode: 0o600 })
  const calls: string[] = []
  const fail = { write: new Set<string>(), rename: new Set<string>() }
  const io: SettingsFsOps = {
    mkdir(dir, mode) {
      calls.push(`mkdir ${dir} ${mode.toString(8)}`)
    },
    readFile(f) {
      const e = store.get(f)
      if (!e) {
        const err = new Error(`ENOENT: ${f}`) as NodeJS.ErrnoException
        err.code = 'ENOENT'
        throw err
      }
      calls.push(`read ${f}`)
      return e.text
    },
    writeFile(f, text, mode) {
      if (fail.write.has(f)) throw new Error(`disk full: ${f}`)
      calls.push(`write ${f} ${mode.toString(8)}`)
      store.set(f, { text, mode })
    },
    rename(from, to) {
      if (fail.rename.has(to)) throw new Error(`rename failed: ${from} → ${to}`)
      const e = store.get(from)
      if (!e) throw new Error(`missing tmp: ${from}`)
      calls.push(`rename ${from} ${to}`)
      store.set(to, e)
      store.delete(from)
    },
    unlink(f) {
      calls.push(`unlink ${f}`)
      store.delete(f)
    },
  }
  return { io, store, calls, fail }
}

// ───────────────────────── ① v1→v2 迁移（§9.3 规则） ─────────────────────────

test('v1 默认值固化 → v2：删默认、留 key/label/prices/deleted/active/未知键，写 schema_version:2', () => {
  const before = v1()
  const { raw: out, changed, removed } = migrateSettingsV2(before, DEFAULTS)

  assert.equal(changed, true)
  assert.deepEqual(before, v1(), '迁移不得改动入参（纯函数）')

  // 与默认完全相同的顶层键被删（删除优先）
  for (const k of ['theme', 'lang', 'mcpServers', 'agent', 'sandbox', 'permissions', 'stt']) {
    assert.equal(k in out, false, `${k} 等于默认值，应删除`)
  }
  // 未知顶层键原样保留
  assert.deepEqual(out.hooks, { pre_tool: [{ name: 'audit' }] })
  assert.deepEqual(out.runtime, { stop_background_on_interrupt: true })

  const provider = out.provider as JsonObject
  // active 运行必需：即使等于默认也保留
  assert.equal(provider.active, 'deepseek')
  // 内置 provider：默认字段删干净，api_key 保留（本任务不迁移 key）
  assert.deepEqual(provider.deepseek, { api_key: 'sk-deepseek' })
  // 保留清单：api_key / label / prices / usage_input_includes_cache 即使存在也保留
  assert.deepEqual(provider.openai, {
    api_key: 'sk-openai',
    label: '我的 OpenAI',
    prices: OPENAI_PRICES,
    usage_input_includes_cache: true,
  })
  // 自定义 provider：没有内置默认 → 整段保留（含未来字段）
  assert.deepEqual(provider.myproxy, MYPROXY)
  // deleted 标记保留，默认字段删掉
  assert.deepEqual(provider.zhipu, { deleted: true })

  // 完成后是 v2；v1 没有 explicit → 不凭空添加
  assert.equal(out.schema_version, 2)
  assert.equal('explicit' in out, false)

  // 默认值没有被写进 raw（不只是「删了 theme」——嵌套默认也不能留）
  assert.equal(JSON.stringify(out).includes('deepseek-chat'), false, '内置 models 默认不得留在 raw')
  // 内置 provider 的每个默认字段都删了（自定义 provider 的 protocol 是用户值，会保留）
  for (const id of ['deepseek', 'openai', 'zhipu']) {
    const cfg = (out.provider as JsonObject)[id] as JsonObject
    for (const field of ['base_url', 'models', 'max_tokens', 'protocol', 'input_types']) {
      assert.equal(field in cfg, false, `内置 ${id}.${field} 等于默认值，应删除`)
    }
  }

  // 诊断：删除路径里有顶层与嵌套
  assert.ok(removed.includes('theme'))
  assert.ok(removed.includes('provider.deepseek.base_url'))
  assert.ok(removed.includes('provider.openai.models'))
})

test('迁移只删「与默认完全相同」的值：用户改过的值原样保留', () => {
  const before = v1({
    theme: 'light',
    agent: { reminder_rounds: 0, compress_threshold: 0.6 },
    sandbox: { mode: 'seatbelt', sensitive_paths: ['~/.ssh'], write_home: false },
    permissions: { computer_approved_apps: ['Safari'] },
    stt: { enabled: true, base_url: 'https://api.openai.com/v1', model: 'whisper-1' },
  })
  const { raw: out } = migrateSettingsV2(before, DEFAULTS)
  assert.equal(out.theme, 'light')
  // 字段级：等于默认的 mode 删掉，用户加的两项保留
  assert.deepEqual(out.agent, { reminder_rounds: 0, compress_threshold: 0.6 })
  assert.deepEqual(out.sandbox, { sensitive_paths: ['~/.ssh'], write_home: false })
  assert.deepEqual(out.permissions, { computer_approved_apps: ['Safari'] })
  assert.deepEqual(out.stt, { enabled: true, base_url: 'https://api.openai.com/v1', model: 'whisper-1' })
})

test('迁移幂等：v2 文件再迁移是 no-op（changed:false，内容逐字节等价）', () => {
  const once = migrateSettingsV2(v1(), DEFAULTS).raw
  const twice = migrateSettingsV2(once, DEFAULTS)
  assert.equal(twice.changed, false)
  assert.deepEqual(twice.raw, once)
  assert.deepEqual(twice.removed, [])
})

test('迁移不因空文件/只有未知键而崩：只补 schema_version', () => {
  const { raw: out } = migrateSettingsV2({ future: { a: 1 } }, DEFAULTS)
  assert.deepEqual(out, { future: { a: 1 }, schema_version: 2 })
})

test('explicit：加路径去重排序 / 删前缀 / 值不存在自动淘汰 / 空则删键', () => {
  const raw: JsonObject = {
    theme: 'dark',
    provider: { active: 'deepseek', deepseek: { api_key: 'sk', auth_type: 'oauth' } },
  }
  addExplicitPath(raw, 'theme')
  addExplicitPath(raw, 'provider.deepseek.api_key')
  addExplicitPath(raw, 'provider.deepseek.api_key') // 去重
  assert.deepEqual(explicitPathsOf(raw), ['provider.deepseek.api_key', 'theme'])
  deleteExplicitPath(raw, 'provider.deepseek') // 删前缀 = 连子路径一起删
  assert.deepEqual(explicitPathsOf(raw), ['theme'])
  deleteExplicitPath(raw, 'theme')
  assert.equal('explicit' in raw, false)
  // sanitize：值已不存在的路径被淘汰，且**不新增**路径
  raw.explicit = ['theme', 'agent.reminder_rounds']
  sanitizeExplicit(raw)
  assert.deepEqual(explicitPathsOf(raw), ['theme'])
})

// ───────────────────────── ② 写侧白名单 patch ─────────────────────────

test('settings:set 未知键（顶层/嵌套/provider 字段）一律拒绝，一个键都不写', () => {
  const raw = v2()
  for (const [patch, expect] of [
    [{ theme2: 'dark' }, 'unknown settings key theme2'],
    [{ agent: { reminder_roundz: 3 } }, 'unknown settings key agent.reminder_roundz'],
    [{ provider: { deepseek: { base_urll: 'x' } } }, 'unknown settings key provider.deepseek.base_urll'],
    [{ provider: { 'bad.dot': { api_key: 'x' } } }, 'unknown settings key provider.bad.dot'],
  ] as const) {
    const r = applySettingsPatch(raw, patch)
    assert.equal(r.ok, false)
    if (!r.ok) assert.match(r.error, new RegExp(expect.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')))
  }
  // 合法化：拼错的枚举值也拒绝（BR-29 的另一半：不留 'darkk' 这种永久脏值）
  const bad = applySettingsPatch(raw, { theme: 'darkk' })
  assert.equal(bad.ok, false)
  if (!bad.ok) assert.match(bad.error, /invalid settings value theme/)
  // 内部键不允许从 patch 写
  for (const p of [{ schema_version: 3 }, { explicit: ['theme'] }]) {
    const r = applySettingsPatch(raw, p)
    assert.equal(r.ok, false)
  }
})

test('一次 theme patch 的 diff 只有 theme + explicit（已迁移 v2）；未迁移 v1 由写路径先迁移', () => {
  // v2 已迁移文件：schema_version 已在 → diff = theme + explicit
  const v2Before = v2()
  const r2 = applySettingsPatch(v2Before, { theme: 'dark' })
  assert.ok(r2.ok)
  if (r2.ok) {
    printDiff('已迁移 v2 + settings:set({theme:"dark"})', v2Before, r2.raw)
    assert.deepEqual(
      rawDiff(v2Before, r2.raw).map((x) => x.path),
      ['explicit', 'theme'],
    )
    assert.deepEqual(explicitPathsOf(r2.raw), ['theme'])
    // provider / mcpServers / 未知顶层键原样保留
    assert.deepEqual(r2.raw.provider, v2Before.provider)
    assert.deepEqual(r2.raw.mcpServers, v2Before.mcpServers)
    assert.deepEqual(r2.raw.hooks, v2Before.hooks)
  }
  // 未迁移的 v1 文件：apply* **不再**顺手盖章 schema_version（那会让「启动迁移失败 + 改一次
  // 主题」把固化默认值永久钉死，复审 B2）。写路径必须先迁移：见 ⑤「写即迁移」的用例。
  const v1Before: JsonObject = { theme: 'dark', provider: { active: 'deepseek' }, unknown_top: 1 }
  const r1 = applySettingsPatch(v1Before, { theme: 'light' })
  assert.equal(r1.ok, false)
  if (!r1.ok) assert.match(r1.error, /尚未迁移到 v2/)
  assert.equal('schema_version' in v1Before, false, '拒绝时不得改动入参（更不能盖章）')
})

test('四条 apply* 对未迁移输入（schema_version<2）一律拒绝：宁可不保存，也不盖章', () => {
  // 迁移没跑成功时，写路径必须**拒绝**这次写：盖章 schema_version:2 会让下次启动
  // already-v2 跳过迁移，v1 里的固化默认值再也删不掉（BR-01 的安全属性，复审 B2）。
  const v1Raw: JsonObject = { theme: 'dark', provider: { active: 'deepseek' }, mcpServers: {} }
  const snapshot = JSON.stringify(v1Raw)
  const cases: Array<[string, () => { ok: boolean; error?: string }]> = [
    ['applySettingsPatch', () => applySettingsPatch(v1Raw, { theme: 'light' })],
    ['applyProviderSave', () => applyProviderSave(v1Raw, { provider: 'deepseek', label: 'x' })],
    ['applyProviderRemove', () => applyProviderRemove(v1Raw, 'deepseek', { builtins: ['deepseek'] })],
    ['applyMcpServers', () => applyMcpServers(v1Raw, { fs: { command: 'mcp-fs' } })],
  ]
  for (const [name, call] of cases) {
    const r = call()
    assert.equal(r.ok, false, `${name} 必须拒绝未迁移输入`)
    assert.match(r.error ?? '', /尚未迁移到 v2/, `${name} 的错误要说清原因`)
  }
  assert.equal(JSON.stringify(v1Raw), snapshot, '拒绝时不得改动入参')
  // 未来版本（schema_version > 2）照常可写：读侧不降级，写侧也不挡
  const future = applySettingsPatch({ schema_version: 3, theme: 'dark' }, { theme: 'light' })
  assert.equal(future.ok, true)
})

test('已知段局部合并：只改 patch 提到的叶子，同段其它字段保留；null 回默认', () => {
  const raw: JsonObject = {
    schema_version: 2,
    agent: { reminder_rounds: 12, compress_threshold: 0.7, future: 1 },
  }
  const r = applySettingsPatch(raw, { agent: { compress_threshold: null, max_subagents: 8 } })
  assert.ok(r.ok)
  if (r.ok) {
    assert.deepEqual(r.raw.agent, { reminder_rounds: 12, max_subagents: 8, future: 1 })
    assert.deepEqual(explicitPathsOf(r.raw), ['agent.max_subagents'])
  }
})

test('renderer 的 {...cur} 形状（undefined 字段 / bridge 专有字段）不会被误拒', () => {
  // 设置面板的 patch 普遍是 `{...cur, 目标键}`：cur 来自 settingsGet（读侧并集），
  // 可能带 undefined 字段与桥侧专有字段。它们必须被当作「未提供」，而不是未知键/坏值。
  const raw: JsonObject = { schema_version: 2, agent: { reminder_rounds: 30, async_promote_after_seconds: 0 } }
  const r = applySettingsPatch(raw, {
    agent: {
      reminder_rounds: 12,
      compress_threshold: undefined,
      max_subagents: undefined,
      async_promote_after_seconds: 0,
    },
    theme: undefined,
    sandbox: { mode: 'none', sensitive_paths: [], write_home: undefined },
  })
  assert.ok(r.ok)
  if (!r.ok) return
  assert.deepEqual(r.raw.agent, { reminder_rounds: 12, async_promote_after_seconds: 0 })
  assert.deepEqual(r.raw.sandbox, { mode: 'none', sensitive_paths: [] })
  assert.equal('theme' in r.raw, false)
  assert.deepEqual(explicitPathsOf(r.raw), [
    'agent.async_promote_after_seconds',
    'agent.reminder_rounds',
    'sandbox.mode',
    'sandbox.sensitive_paths',
  ])
})

test('settings:set 收到 provider 时走同一套白名单/合并规则（不补默认、不丢兄弟）', () => {
  const before = v2()
  const r = applySettingsPatch(before, { provider: { active: 'openai', openai: { label: 'X' } } })
  assert.ok(r.ok)
  if (!r.ok) return
  assert.deepEqual(r.raw.provider, {
    active: 'openai',
    deepseek: { api_key: 'sk-deepseek' },
    openai: { label: 'X' },
  })
  assert.deepEqual(r.raw.mcpServers, before.mcpServers)
  assert.deepEqual(r.raw.hooks, before.hooks)
  assert.deepEqual(explicitPathsOf(r.raw), ['provider.active', 'provider.openai.label'])
})

test('settings:set 禁止整段 provider / mcpServers 置 null（会连带清掉 api_key / env）', () => {
  const raw: JsonObject = {
    schema_version: 2,
    theme: 'light',
    agent: { reminder_rounds: 12 },
    provider: { active: 'deepseek', deepseek: { api_key: 'sk-d' } },
    mcpServers: { fs: { command: 'mcp-fs', env: { TOKEN: 't' } } },
  }
  for (const patch of [{ provider: null }, { mcpServers: null }]) {
    const r = applySettingsPatch(raw, patch)
    assert.equal(r.ok, false)
    assert.match(r.error ?? '', /不允许整段置空/)
  }
  // 其它段保持「null = 回默认（删键）」
  const r = applySettingsPatch(raw, { agent: null, theme: null })
  assert.ok(r.ok)
  if (!r.ok) return
  assert.equal('agent' in r.raw, false)
  assert.equal('theme' in r.raw, false)
  assert.deepEqual(r.raw.provider, raw.provider, 'provider 段不动')
  assert.deepEqual(r.raw.mcpServers, raw.mcpServers, 'mcpServers 段不动')
  // mcpServers: {} 仍是合法的「清空」（mcp:save 的既有路径）
  const empty = applyMcpServers(raw, {})
  assert.ok(empty.ok)
  if (empty.ok) assert.equal('mcpServers' in empty.raw, false)
})

test('settings:set 只 patch 已有 provider 的 label：base_url/models/auth_type/未知字段原样保留', () => {
  // provider 分支不能每次从 {} 重建条目（复审 M5 的存活变异）：那会把 base_url / models /
  // auth_type 以及本版本不认识的字段一起抹掉，等于把 provider 打回默认。局部合并才是对的。
  const before: JsonObject = {
    schema_version: 2,
    provider: {
      active: 'myproxy',
      myproxy: {
        base_url: 'https://p/v1',
        models: { 'm-1': 32768 },
        max_tokens: { 'm-1': 4096 },
        protocol: 'chat_completions',
        auth_type: 'oauth',
        label: '旧名',
        future_field: { nested: [1, 2] },
      },
      deepseek: { api_key: 'sk-d' },
    },
  }
  const r = applySettingsPatch(before, { provider: { myproxy: { label: '新名' } } })
  assert.ok(r.ok)
  if (!r.ok) return
  assert.deepEqual((r.raw.provider as JsonObject).myproxy, {
    base_url: 'https://p/v1',
    models: { 'm-1': 32768 },
    max_tokens: { 'm-1': 4096 },
    protocol: 'chat_completions',
    auth_type: 'oauth',
    label: '新名',
    future_field: { nested: [1, 2] },
  })
  assert.deepEqual((r.raw.provider as JsonObject).deepseek, { api_key: 'sk-d' }, '兄弟 provider 不动')
  assert.equal((r.raw.provider as JsonObject).active, 'myproxy')
  assert.deepEqual(explicitPathsOf(r.raw), ['provider.myproxy.label'])
})

// ───────────────────────── ③ 段级写（BR-02 的 Electron 一半） ─────────────────────────

test('provider:save（oauth 风格 api_key→auth_type patch）不动兄弟 provider / 顶层键 / 未知字段', () => {
  const before = v2()
  const r = applyProviderSave(before, { provider: 'deepseek', auth_type: 'oauth' })
  assert.ok(r.ok)
  if (!r.ok) return
  printDiff('provider:save({provider:"deepseek",auth_type:"oauth"})', before, r.raw)
  // 命中 provider 只加 auth_type，不把 DEFAULT_SETTINGS 的 base_url/models/... 补进来
  assert.deepEqual(r.raw.provider, {
    active: 'deepseek',
    deepseek: { api_key: 'sk-deepseek', auth_type: 'oauth' },
  })
  // 兄弟段与未知顶层键逐字节不变
  assert.deepEqual(r.raw.mcpServers, before.mcpServers)
  assert.deepEqual(r.raw.hooks, before.hooks)
  assert.equal(r.raw.theme, 'light')
  assert.deepEqual(explicitPathsOf(r.raw), ['provider.deepseek.auth_type'])
  // 入参不被修改（写路径 = 读 raw → 纯函数 → 写 raw）
  assert.deepEqual(before, v2())
})

test('provider:save 对自定义 provider 的未知字段原样保留（快照覆盖不会丢）', () => {
  const before: JsonObject = {
    schema_version: 2,
    provider: {
      active: 'myproxy',
      myproxy: { base_url: 'https://p/v1', api_key: 'sk-1', future_field: { a: [1] }, auth_type: 'api_key' },
      other: { base_url: 'https://o/v1', api_key: 'sk-2' },
    },
    unknown_top: { keep: true },
  }
  const r = applyProviderSave(before, { provider: 'myproxy', label: 'Proxy' })
  assert.ok(r.ok)
  if (!r.ok) return
  assert.deepEqual((r.raw.provider as JsonObject).myproxy, {
    base_url: 'https://p/v1',
    api_key: 'sk-1',
    future_field: { a: [1] },
    auth_type: 'api_key',
    label: 'Proxy',
  })
  assert.deepEqual((r.raw.provider as JsonObject).other, { base_url: 'https://o/v1', api_key: 'sk-2' })
  assert.deepEqual(r.raw.unknown_top, { keep: true })
})

test('provider:save 的 {...cur} 形状（undefined 字段）照常工作、空串不清空配置', () => {
  const before = v2()
  const r = applyProviderSave(before, {
    provider: 'deepseek',
    models: { 'deepseek-chat': 64000 },
    openai_compat: undefined,
    label: undefined,
    api_key: undefined,
    base_url: '',
  })
  assert.ok(r.ok)
  if (!r.ok) return
  assert.deepEqual((r.raw.provider as JsonObject).deepseek, {
    api_key: 'sk-deepseek',
    models: { 'deepseek-chat': 64000 },
  })
})

test('provider:save 不写 api_key 空串、保存即清除 deleted、未知字段名拒绝', () => {
  const before: JsonObject = {
    schema_version: 2,
    provider: { active: 'deepseek', deepseek: { api_key: 'sk-keep', deleted: true } },
  }
  const r = applyProviderSave(before, { provider: 'deepseek', api_key: '', base_url: 'https://d/v1' })
  assert.ok(r.ok)
  if (!r.ok) return
  assert.deepEqual((r.raw.provider as JsonObject).deepseek, { api_key: 'sk-keep', base_url: 'https://d/v1' })
  const bad = applyProviderSave(before, { provider: 'deepseek', base_urll: 'https://d/v1' })
  assert.equal(bad.ok, false)
  if (!bad.ok) assert.match(bad.error, /unknown settings key provider\.deepseek\.base_urll/)
})

test('provider:remove 内置 → 只加 deleted 标记（不重建对象/不补默认），其它段与兄弟 provider 保留', () => {
  const before = v2()
  const r = applyProviderRemove(before, 'deepseek', { builtins: ['deepseek', 'openai'] })
  assert.ok(r.ok)
  if (!r.ok) return
  assert.deepEqual((r.raw.provider as JsonObject).deepseek, { api_key: 'sk-deepseek', deleted: true })
  assert.equal((r.raw.provider as JsonObject).active, 'openai', '删除活跃 provider → 回退到下一个未删除的内置')
  assert.deepEqual(r.raw.mcpServers, before.mcpServers)
  assert.deepEqual(r.raw.hooks, before.hooks)
  // raw 里原本没有 openai 段 → 不得凭空补默认
  assert.equal('openai' in (r.raw.provider as JsonObject), false)
})

test('provider:remove 自定义 → 物理删除；active 回退；顶层未知键不动', () => {
  const before: JsonObject = {
    schema_version: 2,
    theme: 'light',
    provider: { active: 'myproxy', myproxy: { base_url: 'https://p/v1', api_key: 'sk' }, deepseek: { api_key: 'sk-d', deleted: true }, openai: {} },
    hooks: { x: 1 },
  }
  const r = applyProviderRemove(before, 'myproxy', { builtins: ['deepseek', 'openai'] })
  assert.ok(r.ok)
  if (!r.ok) return
  assert.deepEqual(r.raw.provider, { active: 'openai', deepseek: { api_key: 'sk-d', deleted: true }, openai: {} })
  assert.equal(r.raw.theme, 'light')
  assert.deepEqual(r.raw.hooks, { x: 1 })
})

test('mcp:save 只替换 mcpServers 段：provider / theme / 未知键全部保留', () => {
  const before = v2()
  const r = applyMcpServers(before, { gh: { type: 'stdio', command: 'mcp-gh', env: { TOKEN: 'x' } } })
  assert.ok(r.ok)
  if (!r.ok) return
  assert.deepEqual(r.raw.mcpServers, { gh: { type: 'stdio', command: 'mcp-gh', env: { TOKEN: 'x' } } })
  assert.deepEqual(r.raw.provider, before.provider)
  assert.equal(r.raw.theme, 'light')
  assert.deepEqual(r.raw.hooks, before.hooks)
  assert.deepEqual(explicitPathsOf(r.raw), ['mcpServers.gh'])
  // 清空 = 删段（读侧补回默认 {}），其它段仍在
  const empty = applyMcpServers(r.raw, {})
  assert.ok(empty.ok)
  if (empty.ok) {
    assert.equal('mcpServers' in empty.raw, false)
    assert.deepEqual(empty.raw.provider, before.provider)
  }
})

test('mcp:save 校验与 bridge 同口径：未知字段/缺 command/缺 url 拒绝', () => {
  const raw = v2()
  for (const [servers, pattern] of [
    [{ fs: { command: 'x', bogus: 1 } }, /unknown settings key mcpServers\.fs\.bogus/],
    [{ fs: { type: 'stdio' } }, /fs: stdio 需 command/],
    [{ web: { type: 'http' } }, /web: 需 url/],
    [{ web: { type: 'http', url: 'https://x' } }, null],
    [{ web: { type: 'streamable-http', url: 'https://x' } }, null],
  ] as const) {
    const r = applyMcpServers(raw, servers)
    if (pattern === null) assert.equal(r.ok, true)
    else {
      assert.equal(r.ok, false)
      if (!r.ok) assert.match(r.error, pattern)
    }
  }
})

test('mcp:save / settings:set{mcpServers} 带 args: undefined（无参数 stdio）成功且不写 args 键', () => {
  // MCPTab.buildCfg 对无参数的 stdio server 总是给出 `args: <string[]|undefined>`，经 Electron IPC
  // （V8 结构化克隆）后**键还在**。写侧把它当成「这一项没填」跳过，而不是坏值拒绝
  //（复审 B1：旧实现会让「添加/编辑无 args 的 stdio server」整条保存失败）。
  const before = v2()
  const cfg = { type: 'stdio', command: 'npx', args: undefined }
  const cases: Array<[string, () => ReturnType<typeof applyMcpServers>]> = [
    ['mcp:save', () => applyMcpServers(before, { npx: cfg })],
    ['settings:set{mcpServers}', () => applySettingsPatch(before, { mcpServers: { npx: cfg } })],
  ]
  for (const [name, call] of cases) {
    const r = call()
    assert.equal(r.ok, true, `${name} 不应拒绝 args: undefined`)
    if (!r.ok) continue
    const entry = (r.raw.mcpServers as JsonObject).npx as JsonObject
    assert.deepEqual(entry, { type: 'stdio', command: 'npx' }, `${name}: args: undefined 不得落成键`)
    assert.equal('args' in entry, false, `${name}: 无参数时配置里不该出现 args`)
    assert.deepEqual(r.raw.provider, before.provider, `${name}: 其它段不动`)
  }
  // undefined = 未提供（与 applySettingsPatch 的 {...cur} 同口径）：未知键也会被跳过；
  // 但**有值**的未知键仍然拒绝（不放松校验）。
  assert.equal(applyMcpServers(before, { fs: { command: 'mcp-fs', bogus: undefined } }).ok, true)
  const bad = applyMcpServers(before, { fs: { command: 'mcp-fs', bogus: 1 } })
  assert.equal(bad.ok, false)
  if (!bad.ok) assert.match(bad.error, /unknown settings key mcpServers\.fs\.bogus/)
})

test('mcp:save 编辑已有条目：草稿原样回填保留 args/command/env；清空 args 只删该键', () => {
  const before: JsonObject = {
    schema_version: 2,
    mcpServers: { fs: { type: 'stdio', command: 'mcp-fs', args: ['--root', '/tmp'], env: { A: 'b' } } },
    theme: 'light',
  }
  // 编辑（MCPTab 用 {...cur} 重建整份 cfg）：未改动的 args/command 原样保留
  const edited = applyMcpServers(before, { fs: { type: 'stdio', command: 'mcp-fs', args: ['--root', '/tmp'], env: { A: 'b', B: 'c' } } })
  assert.ok(edited.ok)
  if (edited.ok) {
    const entry = (edited.raw.mcpServers as JsonObject).fs as JsonObject
    assert.equal(entry.command, 'mcp-fs')
    assert.deepEqual(entry.args, ['--root', '/tmp'])
    assert.deepEqual(entry.env, { A: 'b', B: 'c' })
    assert.equal(edited.raw.theme, 'light')
  }
  // 用户把参数清空：args: [] 保留空数组（写侧不擅自改用户意图），args: undefined 则不留键
  const emptyArgs = applyMcpServers(before, { fs: { type: 'stdio', command: 'mcp-fs', args: [], env: { A: 'b' } } })
  assert.ok(emptyArgs.ok)
  if (emptyArgs.ok) {
    const entry = (emptyArgs.raw.mcpServers as JsonObject).fs as JsonObject
    assert.equal(entry.command, 'mcp-fs')
    assert.deepEqual(entry.args, [])
  }
  const noArgs = applyMcpServers(before, { fs: { type: 'stdio', command: 'mcp-fs', args: undefined, env: { A: 'b' } } })
  assert.ok(noArgs.ok)
  if (noArgs.ok) {
    const entry = (noArgs.raw.mcpServers as JsonObject).fs as JsonObject
    assert.equal(entry.command, 'mcp-fs')
    assert.equal('args' in entry, false)
  }
})

// ───────────────────────── ④ 迁移文件 IO（可注入） ─────────────────────────

const FILE = '/cfg/settings.json'
const BAK = '/cfg/settings.json.bak'

test('迁移 IO：缺文件 → 跳过（不创建文件、不写备份）', () => {
  const { io, store, calls } = fakeIo()
  const r = runSettingsMigration({ file: FILE, backupFile: BAK, defaults: DEFAULTS, io })
  assert.deepEqual(r, { ok: true, skipped: 'missing' })
  assert.equal(store.size, 0)
  assert.equal(calls.some((c) => c.startsWith('write') || c.startsWith('rename')), false)
})

test('迁移 IO：坏文件 / 非对象 → 拒绝迁移且原文件逐字节不变（不写备份）', () => {
  for (const text of ['{ not json', '[]', 'null', '"dark"']) {
    const { io, store } = fakeIo({ [FILE]: text })
    const r = runSettingsMigration({ file: FILE, backupFile: BAK, defaults: DEFAULTS, io })
    assert.equal(r.ok, false)
    assert.equal(r.skipped, 'corrupt')
    assert.equal(store.get(FILE)?.text, text)
    assert.equal(store.has(BAK), false)
  }
})

test('迁移 IO：先写注入路径的 .bak（0600 + 原文本），再原子写 v2；再来一次跳过', () => {
  const original = JSON.stringify(v1(), null, 2)
  const { io, store, calls } = fakeIo({ [FILE]: original })
  const backup = '/elsewhere/custom-settings.bak' // 路径可注入
  const r = runSettingsMigration({ file: FILE, backupFile: backup, defaults: DEFAULTS, io })
  assert.equal(r.ok, true)
  assert.equal(r.changed, true)
  assert.equal(r.backup, backup)
  assert.ok((r.removed ?? []).length > 0)
  // 备份 = 迁移前逐字节原文，0600
  assert.equal(store.get(backup)?.text, original)
  assert.equal(store.get(backup)?.mode, 0o600)
  // 目标文件 = 迁移后的 v2（同样 0600），且写的是「干净」内容
  const out = JSON.parse(store.get(FILE)!.text) as JsonObject
  assert.equal(out.schema_version, 2)
  assert.equal(out.theme, undefined)
  assert.equal(store.get(FILE)?.mode, 0o600)
  // 顺序：备份写在目标写之前
  assert.ok(calls.indexOf(`write ${backup} 600`) < calls.indexOf(`rename ${FILE}.tmp ${FILE}`))
  // 幂等：第二次只读一次判定 already-v2，不写任何东西（不覆盖备份、不重写文件）
  const writes = () => calls.filter((c) => c.startsWith('write') || c.startsWith('rename') || c.startsWith('unlink'))
  const writesBefore = writes().length
  const again = runSettingsMigration({ file: FILE, backupFile: backup, defaults: DEFAULTS, io })
  assert.deepEqual(again, { ok: true, skipped: 'already-v2' })
  assert.equal(writes().length, writesBefore)
})

test('迁移 IO：备份失败 → 不碰目标文件（原文件保持 v1）', () => {
  const original = JSON.stringify(v1())
  const { io, store, fail } = fakeIo({ [FILE]: original })
  fail.rename.add(BAK)
  const r = runSettingsMigration({ file: FILE, backupFile: BAK, defaults: DEFAULTS, io })
  assert.equal(r.ok, false)
  assert.match(r.error ?? '', /备份 .* 失败（未改动/)
  assert.equal(store.get(FILE)?.text, original)
  assert.equal(store.has(BAK), false)
  assert.equal(store.has(`${BAK}.tmp`), false, '备份失败残留的临时文件要清掉')
})

test('迁移 IO：写回失败（rename 失败）→ 原文件保持不变、残留 tmp 清掉、备份仍在', () => {
  const original = JSON.stringify(v1())
  const { io, store, fail, calls } = fakeIo({ [FILE]: original })
  fail.rename.add(FILE)
  const r = runSettingsMigration({ file: FILE, backupFile: BAK, defaults: DEFAULTS, io })
  assert.equal(r.ok, false)
  assert.match(r.error ?? '', /原文件保持不变/)
  assert.equal(store.get(FILE)?.text, original)
  assert.equal(store.get(BAK)?.text, original)
  assert.equal(store.has(`${FILE}.tmp`), false, '残留临时文件（含明文 key）必须清掉')
  assert.ok(calls.includes(`unlink ${FILE}.tmp`))
})

test('迁移 IO：真实 fs（tmp 目录）—— 备份 0600、v2 落盘、重跑无变化', () => {
  const dir = mkdtempSync(join(tmpdir(), 'settings-store-'))
  try {
    const file = join(dir, 'settings.json')
    const bak = join(dir, 'settings.json.bak')
    const original = `${JSON.stringify(v1(), null, 2)}\n`
    writeFileSync(file, original, { mode: 0o600 })
    const realIo: SettingsFsOps = {
      mkdir: (d, mode) => mkdirSync(d, { recursive: true, mode }),
      readFile: (f) => readFileSync(f, 'utf8'),
      writeFile: (f, text, mode) => writeFileSync(f, text, { mode }),
      rename: (a, b) => renameSync(a, b),
      unlink: (f) => unlinkSync(f),
    }
    const r = runSettingsMigration({ file, backupFile: bak, defaults: DEFAULTS, io: realIo })
    assert.equal(r.ok, true)
    assert.equal(readFileSync(bak, 'utf8'), original)
    assert.equal(statSync(bak).mode & 0o777, 0o600)
    assert.equal(statSync(file).mode & 0o777, 0o600)
    const out = JSON.parse(readFileSync(file, 'utf8')) as JsonObject
    assert.equal(out.schema_version, 2)
    assert.deepEqual((out.provider as JsonObject).deepseek, { api_key: 'sk-deepseek' })
    const textAfter = readFileSync(file, 'utf8')
    assert.deepEqual(runSettingsMigration({ file, backupFile: bak, defaults: DEFAULTS, io: realIo }), {
      ok: true,
      skipped: 'already-v2',
    })
    assert.equal(readFileSync(file, 'utf8'), textAfter)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('atomicWriteText：写失败不留 tmp、不改目标', () => {
  const { io, store, fail, calls } = fakeIo({ '/cfg/a.json': 'old' })
  fail.rename.add('/cfg/a.json')
  assert.throws(() => atomicWriteText(io, '/cfg/a.json', 'new'))
  assert.equal(store.get('/cfg/a.json')?.text, 'old')
  assert.equal(store.has('/cfg/a.json.tmp'), false)
  assert.ok(calls.includes('unlink /cfg/a.json.tmp'))
})

// ─────────────────── ⑤ 写即迁移：写路径绝不越过迁移盖章（复审 B2） ───────────────────
//
// 场景（复审给的真机路径）：启动迁移失败（备份/写回失败）→ 用户改一次主题。旧实现在 apply*
// 里盖章 schema_version:2，于是 v1 文件被写成 v2 → 下次启动 already-v2 跳过迁移 → v1 里
// 固化的默认值永久生效、再也删不掉。以下用例全部在**行为层**断言（注入 fs，不看源码文本）。

test('写即迁移：v1 文件 + 一次 theme patch → 先迁移再 patch，产物没有任何固化默认键', () => {
  const original = `${JSON.stringify(v1(), null, 2)}\n`
  const { io, store, calls } = fakeIo({ [FILE]: original })
  const r = commitSettingsWrite({
    file: FILE,
    backupFile: BAK,
    defaults: DEFAULTS,
    io,
    mutate: (raw) => applySettingsPatch(raw, { theme: 'light' }),
  })
  assert.equal(r.ok, true)
  if (!r.ok) return
  // 备份 = 迁移前逐字节原文（可回滚）
  assert.equal(store.get(BAK)?.text, original)
  const out = JSON.parse(store.get(FILE)!.text) as JsonObject
  assert.equal(out.schema_version, 2)
  assert.equal(out.theme, 'light')
  // 关键断言：一次主题修改**不得**把 DEFAULT_SETTINGS 的任何默认值带进用户文件
  for (const k of ['lang', 'mcpServers', 'agent', 'sandbox', 'permissions', 'stt']) {
    assert.equal(k in out, false, `${k} 等于默认值，不得因一次 theme patch 被固化`)
  }
  assert.equal(JSON.stringify(out).includes('deepseek-chat'), false, '内置 models 默认不得落盘')
  const deepseek = (out.provider as JsonObject).deepseek as JsonObject
  for (const field of ['base_url', 'models', 'max_tokens', 'protocol', 'input_types']) {
    assert.equal(field in deepseek, false, `provider.deepseek.${field} 是默认值，不得落盘`)
  }
  // 用户值原样保留
  assert.deepEqual(deepseek, { api_key: 'sk-deepseek' })
  assert.deepEqual((out.provider as JsonObject).myproxy, MYPROXY)
  assert.deepEqual((out.provider as JsonObject).zhipu, { deleted: true })
  assert.deepEqual(out.hooks, { pre_tool: [{ name: 'audit' }] })
  assert.deepEqual(out.runtime, { stop_background_on_interrupt: true })
  assert.deepEqual(explicitPathsOf(out), ['theme'])
  // 两次原子写：迁移 → patch（都走 tmp+rename）
  assert.equal(calls.filter((c) => c === `rename ${FILE}.tmp ${FILE}`).length, 2)
})

test('写路径：迁移失败（备份失败）→ 拒绝这次写、原文件逐字节不变、绝不出现 schema_version:2', () => {
  const original = `${JSON.stringify(v1(), null, 2)}\n`
  const { io, store, fail } = fakeIo({ [FILE]: original })
  fail.rename.add(BAK)
  const r = commitSettingsWrite({
    file: FILE,
    backupFile: BAK,
    defaults: DEFAULTS,
    io,
    mutate: (raw) => applySettingsPatch(raw, { theme: 'light' }),
  })
  assert.equal(r.ok, false)
  assert.match(r.ok ? '' : r.error, /备份 .* 失败/)
  assert.equal(r.ok ? true : r.corrupt, false, '不是坏文件 → main.ts 不该 quarantine')
  assert.equal(store.get(FILE)?.text, original, '原文件必须逐字节不变')
  assert.equal((JSON.parse(store.get(FILE)!.text) as JsonObject).schema_version, undefined, '绝不能盖章')
  assert.equal(store.has(BAK), false)
  assert.equal(store.has(`${FILE}.tmp`), false, '失败不留 tmp')

  // 磁盘恢复后重试 → 迁移 + patch 成功（每次写都重试迁移，而不是「跳过迁移直接盖章」）
  fail.rename.clear()
  const retry = commitSettingsWrite({
    file: FILE,
    backupFile: BAK,
    defaults: DEFAULTS,
    io,
    mutate: (raw) => applySettingsPatch(raw, { theme: 'light' }),
  })
  assert.equal(retry.ok, true)
  const out = JSON.parse(store.get(FILE)!.text) as JsonObject
  assert.equal(out.schema_version, 2)
  assert.equal(out.theme, 'light')
  assert.equal('lang' in out, false, '重试也是「先迁移再 patch」，不带默认值')
})

test('写路径：迁移失败（写回失败）→ 拒绝这次写、原文件逐字节不变、备份仍在', () => {
  const original = `${JSON.stringify(v1(), null, 2)}\n`
  const { io, store, fail } = fakeIo({ [FILE]: original })
  fail.rename.add(FILE)
  const r = commitSettingsWrite({
    file: FILE,
    backupFile: BAK,
    defaults: DEFAULTS,
    io,
    mutate: () => {
      throw new Error('迁移失败时不得调用 mutate（会把未迁移对象交给 apply*）')
    },
  })
  assert.equal(r.ok, false)
  assert.match(r.ok ? '' : r.error, /写回 .* 失败（原文件保持不变/)
  assert.equal(store.get(FILE)?.text, original)
  assert.equal((JSON.parse(store.get(FILE)!.text) as JsonObject).schema_version, undefined)
  assert.equal(store.get(BAK)?.text, original, '备份已写（迁移前原文），目标文件没被覆盖')
  assert.equal(store.has(`${FILE}.tmp`), false)
})

test('写路径：坏文件 → corrupt 标记 + 拒绝写（main.ts 据此 quarantine），且不调用 mutate', () => {
  for (const text of ['{ not json', '[]', 'null']) {
    const { io, store } = fakeIo({ [FILE]: text })
    const r = commitSettingsWrite({
      file: FILE,
      backupFile: BAK,
      defaults: DEFAULTS,
      io,
      mutate: () => {
        throw new Error('坏文件不得进入 mutate（默认值绝不能当写基线）')
      },
    })
    assert.equal(r.ok, false)
    assert.equal(r.ok ? false : r.corrupt, true)
    assert.equal(store.get(FILE)?.text, text)
    assert.equal(store.has(BAK), false)
  }
})

test('写路径：缺文件（首次运行）→ 只写用户显式值 + schema_version，不先铺一遍默认值', () => {
  const { io, store } = fakeIo()
  const r = commitSettingsWrite({
    file: FILE,
    backupFile: BAK,
    defaults: DEFAULTS,
    io,
    mutate: (raw) => applySettingsPatch(raw, { theme: 'light' }),
  })
  assert.equal(r.ok, true)
  const out = JSON.parse(store.get(FILE)!.text) as JsonObject
  assert.deepEqual(out, { schema_version: 2, theme: 'light', explicit: ['theme'] })
  assert.equal(store.has(BAK), false, '没有旧文件可备份 → 不写 .bak')
})

test('写路径：已迁移 v2 + 幂等 patch → changed=false 不写盘；有变化才写一次', () => {
  // 幂等 = 值相同**且** explicit 已记过这条路径（缺 explicit 时补齐本身就是一次真实变化）
  const settled: JsonObject = { ...v2(), explicit: ['theme'] }
  const text = `${JSON.stringify(settled, null, 2)}\n`
  const { io, store, calls } = fakeIo({ [FILE]: text })
  const same = commitSettingsWrite({
    file: FILE,
    backupFile: BAK,
    defaults: DEFAULTS,
    io,
    mutate: (raw) => applySettingsPatch(raw, { theme: 'light' }), // settled 已是 light + explicit
  })
  assert.equal(same.ok, true)
  assert.equal(same.ok ? same.changed : true, false)
  assert.equal(store.get(FILE)?.text, text, '没变化就不重写（也没必要动 mtime）')
  assert.equal(calls.some((c) => c.startsWith('write')), false)

  const changed = commitSettingsWrite({
    file: FILE,
    backupFile: BAK,
    defaults: DEFAULTS,
    io,
    mutate: (raw) => applySettingsPatch(raw, { theme: 'dark' }),
  })
  assert.equal(changed.ok, true)
  assert.equal(changed.ok ? changed.changed : false, true)
  const out = JSON.parse(store.get(FILE)!.text) as JsonObject
  assert.equal(out.theme, 'dark')
  assert.deepEqual(rawDiff(settled, out).map((x) => x.path), ['theme'])
})

// ───────────────────────── ⑥ main.ts 接线守卫（不启动 Electron） ─────────────────────────

function sourcePath(file: string): string {
  const p = join(process.cwd(), 'electron', file)
  if (!existsSync(p)) throw new Error(`源码守卫找不到 ${p}（应在 desktop/app 下运行本测试）`)
  return p
}

/** 去注释 + 去空白的源码（跨行写法与注释说明都不该骗过守卫）。 */
function flatten(src: string): string {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/(^|\n)\s*\/\/.*/g, '$1')
    .replace(/\s+/g, '')
}

test('main.ts：写路径全部走 settings-store（不再有并集基线 / 段内 {...cur} 全量重写）', () => {
  const src = readFileSync(sourcePath('main.ts'), 'utf8')
  const flat = flatten(src)
  // 旧的「读并集 → 整文件重写」入口必须消失
  assert.equal(flat.includes('settingsForWrite'), false, 'settingsForWrite（并集写基线）必须删除')
  assert.equal(flat.includes('writeSettings('), false, 'writeSettings(AppSettings) 必须换成 commitSettings(原始对象)')
  assert.equal(flat.includes('writeRawSettings('), false, 'writeRawSettings 已并入 settings-store.commitSettingsWrite')
  assert.equal(flat.includes('readRawSettingsForWrite('), false, '读原始对象 + 迁移统一在 settings-store.readSettingsForWrite')
  // 四条写路径共用同一个「迁移 + 原子写」出口（B2：不存在绕过迁移的写）
  assert.equal(flat.split('commitSettings(').length - 1, 5, '四条写路径 + 定义各一次 commitSettings(')
  assert.equal(flat.split('commitSettingsWrite(').length - 1, 1, 'commitSettings 是 commitSettingsWrite( 的唯一调用点')
  // 四条写路径的调用点
  for (const fn of ['applySettingsPatch(', 'applyProviderSave(', 'applyProviderRemove(', 'applyMcpServers(']) {
    assert.ok(flat.includes(fn), `main.ts 应调用 ${fn}`)
  }
  // settings:set 不再 `{ ...cur, ...patch }`
  assert.equal(flat.includes('{...cur,...patch}'), false, 'settings:set 不得再把并集展开写回')
  // 读侧仍是「默认值 ∪ 磁盘值」（显示用），且内部键不外泄
  assert.ok(flat.includes('JSON.parse(JSON.stringify(DEFAULT_SETTINGS))'), 'readSettingsResult 仍要合并默认值供显示')
  assert.ok(flat.includes("deletebody.explicit") && flat.includes('deletebody.schema_version'), '内部键不得出现在 renderer 看到的 settings 里')
})

test('main.ts：迁移在 app 启动、bridge spawn 之前调用，失败只记日志不阻止启动', () => {
  const src = readFileSync(sourcePath('main.ts'), 'utf8')
  const ready = src.indexOf('app.whenReady().then(')
  assert.ok(ready >= 0, '找不到 app.whenReady')
  const readyBlock = src.slice(ready, src.indexOf('})', ready) + 2)
  const migrateAt = readyBlock.indexOf('migrateSettingsOnStartup()')
  assert.ok(migrateAt >= 0, 'whenReady 里必须调用 migrateSettingsOnStartup()')
  // 迁移必须排在 registerIpc（注册 bridge:start 处理器）与 createWindow 之前
  assert.ok(readyBlock.indexOf('registerIpc()') > migrateAt, '迁移必须先于 registerIpc（bridge:start 注册）')
  assert.ok(readyBlock.indexOf('createWindow()') > migrateAt, '迁移必须先于 createWindow')
  // 启动路径自身不 spawn bridge（只由 renderer 的 bridge:start 触发）
  assert.equal(/app\.whenReady[\s\S]{0,600}?spawnBridge\(/.test(src), false, 'whenReady 里不得直接 spawnBridge')
  // 迁移失败路径不能 throw 出 whenReady（否则应用起不来）
  const fn = src.indexOf('function migrateSettingsOnStartup')
  assert.ok(fn >= 0)
  const body = src.slice(fn, src.indexOf('\n}', fn))
  assert.match(body, /catch/)
  assert.match(body, /console\.error/)
  assert.ok(body.includes('runSettingsMigration('))
})
