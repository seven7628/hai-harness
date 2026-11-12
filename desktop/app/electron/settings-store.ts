// settings-store.ts —— settings.json 的**单写者**语义（BR-01 / BR-29 / BR-02 的写侧）。
//
// 为什么需要这一层：读侧 `readSettingsResult()` 返回「DEFAULT_SETTINGS ∪ 磁盘值」供显示，
// 但**写侧绝不能拿这份并集当基线** —— 任何一次无关保存（改主题）都会把整份默认值固化进
// 用户文件（BR-01），新版本改默认对老用户永不生效。历史上的四条写路径（settings:set /
// provider:save / provider:remove / mcp:save）都是「读整份并集 → 拼自己那段 → 整文件重写」，
// 于是还叠加了 BR-29（拼错字段名被永久写入）与 BR-02（bridge 的 provider 快照覆盖兄弟键）。
//
// 本模块只做纯逻辑（不 import electron / 不直接碰 fs），三件事：
//   ① v1→v2 迁移：以发布时 DEFAULT_SETTINGS 为基线，删除与默认值**完全相同**的已知键；
//   ② 写侧白名单 patch：只允许已知顶层/嵌套键，只改 patch 涉及的键，未知键报错；
//      并把路径记进顶层 `explicit`（v2 起「用户显式选过、即使等于默认」的唯一证据）；
//   ③ 段级写（provider / mcpServers）：只改自己的段，兄弟 provider 与顶层未知键原样保留。
//
// 值的取舍（§9.3）：v1 无法区分「被固化的默认」与「用户显式选了默认值」——统一按删除处理，
// 未来跟随新默认；v2 起写侧落 `explicit`，后续迁移不再丢失用户意图。
//
// 文件 IO 抽成可注入参数（SettingsFsOps），迁移的缺文件/坏文件/备份路径/写失败回滚都能在
// node:test 里直接构造，不必启动 Electron。

/** 普通 JSON 对象（settings.json 的根与各段）。值保持 unknown：合法性由字段 schema 判定。 */
export type JsonObject = Record<string, unknown>

/** settings.json 的 schema 版本（本模块写出的一直是它）。 */
const SETTINGS_SCHEMA_VERSION = 2

/** provider 段「即使等于默认也保留」的字段（§9.3；本任务不迁移 api_key，见 BR-08）。 */
const PROVIDER_KEEP_FIELDS: readonly string[] = ['api_key', 'label', 'prices', 'usage_input_includes_cache', 'deleted']

function isPlainObject(v: unknown): v is JsonObject {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

function cloneJson<T>(v: T): T {
  return JSON.parse(JSON.stringify(v)) as T
}

export function deepEqual(a: unknown, b: unknown): boolean {
  if (a === b) return true
  if (Array.isArray(a) || Array.isArray(b)) {
    if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) return false
    for (let i = 0; i < a.length; i++) if (!deepEqual(a[i], b[i])) return false
    return true
  }
  if (!isPlainObject(a) || !isPlainObject(b)) return false
  const ak = Object.keys(a)
  const bk = Object.keys(b)
  if (ak.length !== bk.length) return false
  for (const k of ak) {
    if (!Object.prototype.hasOwnProperty.call(b, k)) return false
    if (!deepEqual(a[k], b[k])) return false
  }
  return true
}

/** provider 命名校验（与 bridge validProviderName 同规则：字母开头，字母/数字/短横/下划线）。 */
export function isValidProviderName(name: unknown): name is string {
  return typeof name === 'string' && name !== '' && name !== 'active' && /^[A-Za-z][A-Za-z0-9_-]*$/.test(name)
}

function isV2Settings(raw: JsonObject): boolean {
  const v = raw.schema_version
  return typeof v === 'number' && v >= SETTINGS_SCHEMA_VERSION
}

// ─────────────────────────── explicit（用户显式设置过的路径） ───────────────────────────

/** 读顶层 explicit（非数组/含非字符串 → 过滤）。 */
export function explicitPathsOf(raw: JsonObject): string[] {
  const v = raw.explicit
  if (!Array.isArray(v)) return []
  return v.filter((x): x is string => typeof x === 'string' && x.trim() !== '')
}

/** 某路径（点路径）在 raw 里是否真的存在。 */
function hasPath(raw: JsonObject, path: string): boolean {
  const segs = path.split('.')
  let cur: unknown = raw
  for (const seg of segs) {
    if (!isPlainObject(cur) || !Object.prototype.hasOwnProperty.call(cur, seg)) return false
    cur = cur[seg]
  }
  return true
}

function writeExplicit(raw: JsonObject, paths: string[]): void {
  const uniq = Array.from(new Set(paths)).sort()
  if (uniq.length === 0) {
    delete raw.explicit
    return
  }
  raw.explicit = uniq
}

export function addExplicitPath(raw: JsonObject, path: string): void {
  const cur = explicitPathsOf(raw).filter((p) => p !== path)
  cur.push(path)
  writeExplicit(raw, cur)
}

/** 删除路径及其所有子路径（例：删 provider.x 时同时清掉 provider.x.api_key）。 */
export function deleteExplicitPath(raw: JsonObject, path: string): void {
  const keep = explicitPathsOf(raw).filter((p) => p !== path && !p.startsWith(`${path}.`))
  writeExplicit(raw, keep)
}

/** 清掉已不存在的路径、去重排序、空则删键（迁移后调用；**不新增**任何路径）。 */
export function sanitizeExplicit(raw: JsonObject): void {
  writeExplicit(
    raw,
    explicitPathsOf(raw).filter((p) => hasPath(raw, p)),
  )
}

// ─────────────────────────────── v1 → v2 迁移（§9.3） ───────────────────────────────

export interface SettingsMigration {
  raw: JsonObject
  changed: boolean
  /** 被删除的路径（诊断/日志用；不参与语义）。 */
  removed: string[]
}

/**
 * migrateSettingsV2：v1 固化默认值 → v2。
 *
 * 规则（§9.3）：
 *   · 以 defaults（发布时 DEFAULT_SETTINGS）为基线，逐键删除与默认值**完全相同**的已知顶层键；
 *   · 未知顶层键原样保留；已知段（provider/mcpServers/agent/sandbox/permissions/stt…）
 *     递归到已知字段（`models` 这类动态 map 只在「整段等于默认」时删）；
 *   · provider 段保留 `active`，以及 api_key/label/prices/usage_input_includes_cache/deleted
 *     （即使等于默认也保留 —— 本任务不迁移 key）；内置 provider 的 base_url/models/
 *     max_tokens/protocol/input_types 等于默认则删；自定义 provider 没有内置默认，整段保留；
 *   · 幂等：schema_version >= 2 的文件直接原样返回（不重复删、不动 explicit）；
 *   · 完成后写 schema_version: 2；v1 没有 explicit 就**不凭空添加**路径。
 */
export function migrateSettingsV2(rawIn: JsonObject, defaults: JsonObject): SettingsMigration {
  if (isV2Settings(rawIn)) return { raw: cloneJson(rawIn), changed: false, removed: [] }
  const raw = cloneJson(rawIn)
  const removed: string[] = []

  pruneSection(raw, defaults, '', removed)

  raw.schema_version = SETTINGS_SCHEMA_VERSION
  sanitizeExplicit(raw)
  return { raw, changed: !deepEqual(rawIn, raw), removed }
}

/** 递归删除「与 defaults 完全相同」的已知字段；未知键一律保留。 */
function pruneSection(target: JsonObject, defaults: JsonObject, base: string, removed: string[]): void {
  for (const key of Object.keys(target)) {
    const path = base ? `${base}.${key}` : key
    if (key === 'schema_version' || key === 'explicit') continue // 内部键由本模块维护
    if (!Object.prototype.hasOwnProperty.call(defaults, key)) continue // 未知键：保留
    const def = defaults[key]
    const val = target[key]

    if (key === 'provider' && isPlainObject(val) && isPlainObject(def)) {
      pruneProviderSection(val, def, removed)
      if (Object.keys(val).length === 0 && Object.keys(def).length > 0) {
        delete target[key]
        removed.push(path)
      }
      continue
    }
    // 已知段递归：嵌套的默认字段同样删；删空的段整段删掉（读侧会补回默认）。
    if (isPlainObject(val) && isPlainObject(def) && Object.keys(def).length > 0) {
      pruneSection(val, def, path, removed)
      if (Object.keys(val).length === 0) {
        delete target[key]
        removed.push(path)
      }
      continue
    }
    if (deepEqual(val, def)) {
      delete target[key]
      removed.push(path)
    }
  }
}

function pruneProviderSection(providers: JsonObject, defaults: JsonObject, removed: string[]): void {
  for (const [id, cfg] of Object.entries(providers)) {
    if (id === 'active') continue // 运行必需：即使等于默认也保留
    if (!isPlainObject(cfg)) continue // 形状不认识（如字符串）：原样保留
    const def = defaults[id]
    if (!isPlainObject(def)) continue // 自定义 provider：缺内置默认 → 非空字段全部保留
    for (const [field, val] of Object.entries(cfg)) {
      if (PROVIDER_KEEP_FIELDS.includes(field)) continue // key/label/prices/用量口径/删除标记：保留
      if (!Object.prototype.hasOwnProperty.call(def, field)) continue
      if (deepEqual(val, def[field])) {
        delete cfg[field]
        removed.push(`provider.${id}.${field}`)
      }
    }
    if (Object.keys(cfg).length === 0) {
      delete providers[id]
      removed.push(`provider.${id}`)
    }
  }
}

// ─────────────────────────────── 写侧字段 schema（白名单） ───────────────────────────────

export type FieldKind =
  | { t: 'enum'; values: readonly string[] }
  | { t: 'string'; maxLen?: number; trim?: boolean; emptyDeletes?: boolean }
  | { t: 'int'; min?: number; max?: number }
  | { t: 'number'; min?: number; max?: number; exclusiveMin?: boolean }
  | { t: 'bool' }
  | { t: 'boolOrNull' }
  | { t: 'stringArray' }
  | { t: 'stringRecord' }
  | { t: 'stringArrayRecord' }
  | { t: 'numberRecord' }
  | { t: 'prices' }
  | { t: 'jsonObject' }
  | { t: 'peers' }
  | { t: 'object'; fields: Record<string, FieldKind> }
  | { t: 'providerMap' }
  | { t: 'mcpServers' }
  | { t: 'forbidden' }

/** provider 段的字段白名单（settings:set 的 provider 分支与 provider:save 共用）。 */
const PROVIDER_FIELDS: Record<string, FieldKind> = {
  base_url: { t: 'string' },
  label: { t: 'string', trim: true, maxLen: 64, emptyDeletes: true },
  models: { t: 'numberRecord' },
  max_tokens: { t: 'numberRecord' },
  prices: { t: 'prices' },
  protocol: { t: 'enum', values: ['chat_completions', 'responses', 'anthropic'] },
  openai_compat: { t: 'jsonObject' },
  input_types: { t: 'stringArrayRecord' },
  auth_type: { t: 'enum', values: ['api_key', 'oauth'] },
  usage_input_includes_cache: { t: 'boolOrNull' },
  cache_ttl_1h: { t: 'boolOrNull' },
  api_key: { t: 'string', emptyDeletes: false },
  deleted: { t: 'bool' },
}

/** MCP 服务器条目字段白名单（与 mcp.ServerConfig 的 JSON 键对齐）。 */
const MCP_FIELDS: Record<string, FieldKind> = {
  type: { t: 'string' },
  command: { t: 'string' },
  args: { t: 'stringArray' },
  env: { t: 'stringRecord' },
  url: { t: 'string' },
  headers: { t: 'stringRecord' },
  enabled: { t: 'bool' },
  exposure: { t: 'string' },
}

/** 顶层 settings schema：白名单 + 逐字段合法化。未知键 → `unknown settings key <path>`。 */
export const SETTINGS_SCHEMA: Record<string, FieldKind> = {
  theme: { t: 'enum', values: ['dark', 'light', 'system'] },
  lang: { t: 'enum', values: ['zh', 'en'] },
  browser_engine: { t: 'enum', values: ['mcp', 'ego'] },
  narrative_skin: { t: 'enum', values: ['current', 'v3', 'runtime'] },
  narrative_density: { t: 'enum', values: ['concise', 'detailed'] },
  provider: { t: 'providerMap' },
  mcpServers: { t: 'mcpServers' },
  agent: {
    t: 'object',
    fields: {
      reminder_rounds: { t: 'int', min: 0 },
      compress_threshold: { t: 'number', min: 0, max: 1, exclusiveMin: true },
      max_subagents: { t: 'int', min: 1 },
      max_explore_subagents: { t: 'int', min: 1 },
      first_chunk_timeout: { t: 'int', min: 1, max: 900 },
      // bridge 侧已有、UI 未暴露的字段：放行以免「手写过它 → 设置面板改 reminder_rounds
      // 时整条 patch 被拒」。0 = 关闭自动摘离。
      async_promote_after_seconds: { t: 'int', min: 0 },
    },
  },
  sandbox: {
    t: 'object',
    fields: {
      mode: { t: 'enum', values: ['none', 'seatbelt'] },
      sensitive_paths: { t: 'stringArray' },
      write_home: { t: 'bool' },
    },
  },
  permissions: {
    t: 'object',
    fields: {
      computer_approved_apps: { t: 'stringArray' },
    },
  },
  cron: {
    t: 'object',
    fields: {
      enabled: { t: 'bool' },
      auto_clean: { t: 'bool' },
    },
  },
  codemode: {
    t: 'object',
    fields: {
      enabled: { t: 'bool' },
      mode: { t: 'enum', values: ['on', 'off'] },
      inline_budget: { t: 'int' },
      budget_seconds: { t: 'number', min: 0, max: 3600, exclusiveMin: true },
    },
  },
  mesh: {
    t: 'object',
    fields: {
      session_enabled: { t: 'bool' },
      remote_enabled: { t: 'bool' },
      remote: {
        t: 'object',
        fields: {
          listen_addr: { t: 'string' },
          tls_cert: { t: 'string' },
          tls_key: { t: 'string' },
          auth_token: { t: 'string' },
          node_id: { t: 'string' },
          peers: { t: 'peers' },
          hide_session_names: { t: 'bool' },
          sensitive_rules: { t: 'stringArray' },
        },
      },
    },
  },
  checkpoint: {
    t: 'object',
    fields: {
      background_compact: { t: 'boolOrNull' },
    },
  },
  stt: {
    t: 'object',
    fields: {
      enabled: { t: 'bool' },
      base_url: { t: 'string' },
      model: { t: 'string' },
      api_key: { t: 'string' },
      vendor: { t: 'string' },
      base_urls: { t: 'stringRecord' },
      custom_models: { t: 'stringArrayRecord' },
    },
  },
  schema_version: { t: 'forbidden' },
  explicit: { t: 'forbidden' },
}

// ─────────────────────────────── patch 应用（白名单 + 合并） ───────────────────────────────

export type SettingsApplyResult =
  | { ok: true; raw: JsonObject; changed: boolean; touched: string[] }
  | { ok: false; error: string }

interface Ctx {
  touched: string[]
}

function unknownKey(path: string): string {
  return `unknown settings key ${path}`
}

function invalidValue(path: string, why: string): string {
  return `invalid settings value ${path}: ${why}`
}

function isStringArray(v: unknown): v is string[] {
  return Array.isArray(v) && v.every((x) => typeof x === 'string')
}

function isStringRecord(v: unknown): v is Record<string, string> {
  return isPlainObject(v) && Object.values(v).every((x) => typeof x === 'string')
}

function isStringArrayRecord(v: unknown): v is Record<string, string[]> {
  return isPlainObject(v) && Object.values(v).every(isStringArray)
}

function isNumberRecord(v: unknown): v is Record<string, number> {
  return isPlainObject(v) && Object.values(v).every((x) => typeof x === 'number' && Number.isFinite(x))
}

function isPrices(v: unknown): v is Record<string, unknown> {
  if (!isPlainObject(v)) return false
  return Object.values(v).every((p) => {
    if (!isPlainObject(p)) return false
    return (['input', 'cache_read', 'cache_write', 'output'] as const).every(
      (k) => typeof p[k] === 'number' && Number.isFinite(p[k] as number),
    )
  })
}

function isPeers(v: unknown): boolean {
  if (!Array.isArray(v)) return false
  return v.every((p) => isPlainObject(p) && typeof p.id === 'string' && typeof p.addr === 'string' && typeof p.token === 'string')
}

/** 写入/删除一个叶子值；null 表示删除（三态字段的「回默认」）。 */
function putValue(target: JsonObject, key: string, value: unknown, path: string, ctx: Ctx): void {
  if (value === null || value === undefined) delete target[key]
  else target[key] = value
  ctx.touched.push(path)
}

function applyKind(target: JsonObject, key: string, kind: FieldKind, value: unknown, path: string, ctx: Ctx): string | undefined {
  // 统一的三态口径：null = 删除该键（读侧回默认）。避免「不存在」与「显式设成默认值」
  // 被混成一件事（provider usage_input_includes_cache / cache_ttl_1h 就靠这个）。
  // 例外：provider / mcpServers 是两个大 map，整段 null 不是「回默认」而是数据事故——
  // 会把所有 provider（含明文 api_key）或所有 MCP env/headers 一起抹掉，一律拒绝；
  // 想清空请逐个 provider / 逐个 server 删（mcpServers: {} 仍是合法的「清空」）。
  if (value === null && kind.t !== 'forbidden') {
    if (kind.t === 'providerMap' || kind.t === 'mcpServers') {
      return invalidValue(path, '不允许整段置空（会连带清掉 api_key / env，请逐项删除）')
    }
    putValue(target, key, null, path, ctx)
    return
  }
  switch (kind.t) {
    case 'forbidden':
      return `${path} 是内部键，不允许通过 settings:set 写入`
    case 'enum': {
      if (typeof value !== 'string' || !kind.values.includes(value)) {
        return invalidValue(path, `应为 ${kind.values.join(' / ')}`)
      }
      putValue(target, key, value, path, ctx)
      return
    }
    case 'string': {
      if (typeof value !== 'string') return invalidValue(path, '应为字符串')
      let s = kind.trim ? value.trim() : value
      if (kind.maxLen !== undefined) s = s.slice(0, kind.maxLen)
      if (s === '' && kind.emptyDeletes) {
        putValue(target, key, null, path, ctx)
        return
      }
      putValue(target, key, s, path, ctx)
      return
    }
    case 'int':
    case 'number': {
      if (typeof value === 'string' && value.trim() === '') return invalidValue(path, '应为数字')
      const n = typeof value === 'number' ? value : Number(value)
      if (typeof value === 'boolean' || value === null || typeof value === 'object' || !Number.isFinite(n)) {
        return invalidValue(path, '应为数字')
      }
      const num = kind.t === 'int' ? Math.round(n) : n
      if (kind.t === 'int' && num !== n && Math.abs(num - n) > 1e-9) return invalidValue(path, '应为整数')
      const exclusiveMin = kind.t === 'number' && kind.exclusiveMin === true
      if (kind.min !== undefined && (exclusiveMin ? num <= kind.min : num < kind.min)) {
        return invalidValue(path, `应${exclusiveMin ? '>' : '≥'} ${kind.min}`)
      }
      if (kind.max !== undefined && num > kind.max) return invalidValue(path, `应 ≤ ${kind.max}`)
      putValue(target, key, num, path, ctx)
      return
    }
    case 'bool': {
      if (typeof value !== 'boolean') return invalidValue(path, '应为布尔值')
      putValue(target, key, value, path, ctx)
      return
    }
    case 'boolOrNull': {
      if (value === null) {
        putValue(target, key, null, path, ctx)
        return
      }
      if (typeof value !== 'boolean') return invalidValue(path, '应为布尔值或 null')
      putValue(target, key, value, path, ctx)
      return
    }
    case 'stringArray': {
      if (!isStringArray(value)) return invalidValue(path, '应为字符串数组')
      putValue(target, key, cloneJson(value), path, ctx)
      return
    }
    case 'stringRecord': {
      if (!isStringRecord(value)) return invalidValue(path, '应为「字符串 → 字符串」映射')
      putValue(target, key, cloneJson(value), path, ctx)
      return
    }
    case 'stringArrayRecord': {
      if (!isStringArrayRecord(value)) return invalidValue(path, '应为「字符串 → 字符串数组」映射')
      putValue(target, key, cloneJson(value), path, ctx)
      return
    }
    case 'numberRecord': {
      if (!isNumberRecord(value)) return invalidValue(path, '应为「模型 → 数字」映射')
      putValue(target, key, cloneJson(value), path, ctx)
      return
    }
    case 'prices': {
      if (!isPrices(value)) return invalidValue(path, '应为「模型 → {input,cache_read,cache_write,output}」')
      putValue(target, key, cloneJson(value), path, ctx)
      return
    }
    case 'jsonObject': {
      if (!isPlainObject(value)) return invalidValue(path, '应为对象')
      putValue(target, key, cloneJson(value), path, ctx)
      return
    }
    case 'peers': {
      if (!isPeers(value)) return invalidValue(path, '应为 [{id,addr,token}]')
      putValue(target, key, cloneJson(value), path, ctx)
      return
    }
    case 'object': {
      if (!isPlainObject(value)) return invalidValue(path, '应为对象')
      const sub = isPlainObject(target[key]) ? (target[key] as JsonObject) : {}
      const err = applyObjectPatch(sub, kind.fields, value, path, ctx)
      if (err) return err
      if (Object.keys(sub).length === 0) delete target[key]
      else target[key] = sub
      return
    }
    case 'providerMap':
      return applyProviderMapIn(target, key, value, path, ctx)
    case 'mcpServers':
      return applyMcpServersIn(target, key, value, path, ctx)
    default: {
      const never: never = kind
      return invalidValue(path, `未知字段类型 ${JSON.stringify(never)}`)
    }
  }
}

/** 对 object 型 schema 做**局部** patch：只动 patch 里出现的键，其余保持原样。 */
function applyObjectPatch(target: JsonObject, fields: Record<string, FieldKind>, patch: unknown, base: string, ctx: Ctx): string | undefined {
  if (!isPlainObject(patch)) return invalidValue(base, '应为对象')
  for (const [k, v] of Object.entries(patch)) {
    if (v === undefined) continue // undefined = 未提供（renderer 的 {...cur} 会带出 undefined 字段）
    const kind = fields[k]
    const path = base ? `${base}.${k}` : k
    if (!kind) return unknownKey(path)
    const err = applyKind(target, k, kind, v, path, ctx)
    if (err) return err
  }
  return
}

function applyProviderMapIn(target: JsonObject, key: string, value: unknown, path: string, ctx: Ctx): string | undefined {
  if (!isPlainObject(value)) return invalidValue(path, '应为对象')
  const providers = isPlainObject(target[key]) ? (target[key] as JsonObject) : {}
  for (const [id, entry] of Object.entries(value)) {
    if (entry === undefined) continue
    if (id === 'active') {
      if (!isValidProviderName(entry)) return invalidValue(`${path}.active`, '应为合法 provider 名称')
      providers.active = entry
      ctx.touched.push(`${path}.active`)
      continue
    }
    if (!isValidProviderName(id)) return unknownKey(`${path}.${id}`)
    if (!isPlainObject(entry)) return invalidValue(`${path}.${id}`, '应为对象')
    const current = isPlainObject(providers[id]) ? (providers[id] as JsonObject) : {}
    const err = applyObjectPatch(current, PROVIDER_FIELDS, entry, `${path}.${id}`, ctx)
    if (err) return err
    if (Object.keys(current).length === 0) delete providers[id]
    else providers[id] = current
  }
  if (Object.keys(providers).length === 0) delete target[key]
  else target[key] = providers
  return
}

function applyMcpServersIn(target: JsonObject, key: string, value: unknown, path: string, ctx: Ctx): string | undefined {
  const r = normalizeMcpServers(value, path)
  if ('error' in r) return r.error
  // 段级替换；旧条目的 explicit 由 settleExplicit 的 sanitize 自动淘汰（路径已不存在）。
  if (Object.keys(r.servers).length === 0) delete target[key]
  else target[key] = r.servers
  for (const name of Object.keys(r.servers)) ctx.touched.push(`${path}.${name}`)
  return
}

/** MCP 段全量替换的校验与归一（mcp:save 与 settings:set 的 mcpServers 分支共用）。 */
function normalizeMcpServers(value: unknown, base: string): { servers: JsonObject } | { error: string } {
  if (!isPlainObject(value)) return { error: invalidValue(base, '应为对象') }
  const servers: JsonObject = {}
  for (const [name, raw] of Object.entries(value)) {
    if (raw === undefined) continue
    if (!name.trim()) return { error: '服务器名不能为空' }
    const path = `${base}.${name}`
    if (!isPlainObject(raw)) return { error: invalidValue(path, '应为对象') }
    const c = raw as { type?: unknown; command?: unknown; url?: unknown }
    // 类型归一与 bridge 的 mcp.ServerConfig.Validate 同口径（streamable_http/streamable-http 都是 http 别名）。
    const type = (typeof c.type === 'string' ? c.type : 'stdio').trim().toLowerCase()
    const stdio = type === '' || type === 'stdio'
    const remote = ['http', 'streamable_http', 'streamable-http', 'sse'].includes(type)
    if (!stdio && !remote) return { error: `未知 type ${String(c.type)}` }
    if (stdio && !(typeof c.command === 'string' && c.command.trim())) return { error: `${name}: stdio 需 command` }
    if (remote && !(typeof c.url === 'string' && c.url.trim())) return { error: `${name}: 需 url` }
    const out: JsonObject = {}
    for (const [field, v] of Object.entries(raw)) {
      // undefined = 「这一项没填」，不是「值是 undefined」：MCPTab.buildCfg 对无参数的 stdio
      // 总是给出 `args: undefined`，经 Electron IPC（V8 结构化克隆）后**键仍然存在**。
      // 不跳过就会被 applyKind 当成坏值（`应为字符串数组`）→ 整个保存失败（P5-08 复审 B1）。
      // 口径与 applyObjectPatch 一致：先跳过，再做未知键检查。
      if (v === undefined) continue
      if (!MCP_FIELDS[field]) return { error: unknownKey(`${path}.${field}`) }
      const err = applyKind(out, field, MCP_FIELDS[field], v, `${path}.${field}`, { touched: [] })
      if (typeof err === 'string') return { error: err }
    }
    servers[name] = out
  }
  return { servers }
}

// ─────────────────────── 写前置条件：只接受已迁移（v2）的对象 ───────────────────────

/**
 * 未迁移输入的拒绝理由（四条 apply* 的统一前置条件）。
 *
 * 为什么必须在 apply* 里挡住：这里曾经无脑 `stampSchemaVersion(raw)`，于是「启动迁移失败 +
 * 用户改一次主题」就把未迁移的 v1 文件写成 `schema_version: 2` —— 下次启动 already-v2 直接
 * 跳过迁移，固化在旧文件里的默认值再也删不掉（BR-01 的安全属性被永久废掉，P5-08 复审 B2）。
 * 现在 schema_version:2 **只可能**由「成功的迁移写盘」产生（readSettingsForWrite），
 * apply* 拿到 v1 一律拒绝，宁可不保存也不盖章。
 */
function unmigratedInputError(rawIn: JsonObject): string {
  return `settings.json 尚未迁移到 v2（schema_version=${JSON.stringify(rawIn.schema_version ?? null)}）：已拒绝这次写入，避免把未迁移的默认值盖章成 v2`
}

/** 写侧入口的公共闸门：输入必须是 v2（迁移由读盘侧负责，见 readSettingsForWrite）。 */
function isWritable(rawIn: JsonObject): boolean {
  return isV2Settings(rawIn)
}

/**
 * 结算 explicit：patch 涉及的路径登记为「用户显式设置过」；值被删/段被替换后已不存在的
 * 路径由 sanitizeExplicit 统一淘汰（不手写第二套删除逻辑）。
 */
function settleExplicit(raw: JsonObject, ctx: Ctx): void {
  for (const p of ctx.touched) if (hasPath(raw, p)) addExplicitPath(raw, p)
  sanitizeExplicit(raw)
}

/**
 * applySettingsPatch：写侧白名单 + 合法化（BR-29 + BR-01 的写侧）。
 *
 * · 未知顶层/嵌套键 → `{ ok:false, error:'unknown settings key <path>' }`，一个键都不写；
 * · 只改 patch 涉及的键（局部合并），未出现的键/段原样保留；
 * · provider 分支与 provider:save 同规则（字段白名单 + 合并不补默认）；
 * · 路径记进 explicit；输入必须是已迁移的 v2（见 unmigratedInputError）。
 */
export function applySettingsPatch(rawIn: JsonObject, patch: unknown, schema: Record<string, FieldKind> = SETTINGS_SCHEMA): SettingsApplyResult {
  if (!isWritable(rawIn)) return { ok: false, error: unmigratedInputError(rawIn) }
  const raw = cloneJson(rawIn)
  if (!isPlainObject(patch)) return { ok: false, error: '无效设置 patch（应为对象）' }
  if (Object.keys(patch).length === 0) return { ok: true, raw, changed: false, touched: [] }
  const ctx: Ctx = { touched: [] }
  const err = applyObjectPatch(raw, schema, patch, '', ctx)
  if (err) return { ok: false, error: err }
  settleExplicit(raw, ctx)
  return { ok: true, raw, changed: !deepEqual(rawIn, raw), touched: ctx.touched }
}

// ─────────────────────────────── 段级写：provider / mcp ───────────────────────────────

/**
 * applyProviderSave：provider:save 的写侧语义（BR-02 的 Electron 一半）。
 *
 * 只改 `provider.<name>` 里输入出现的字段：
 *   · 兄弟 provider、provider 段的未知字段、顶层未知键**一律不动**（不是 `{...cur}` 全量重写）；
 *   · 不把 DEFAULT_SETTINGS 的 base_url/models/max_tokens/protocol/input_types 补进 raw；
 *   · 重新保存 = 清除 deleted 标记（内置删除后重新启用）；
 *   · api_key 空串视为「不修改」（与既有行为一致）。BR-08 起 main.ts 的 provider:save 会先把
 *     `api_key` 剥掉（写 keys.json），所以这里只剩「历史遗留 api_key 原样保留」这一种情况；
 *     真正的迁移在 keys-store.migratePlaintextProviderKeys（本模块不认识 keys.json）。
 */
export function applyProviderSave(rawIn: JsonObject, input: unknown): SettingsApplyResult {
  if (!isWritable(rawIn)) return { ok: false, error: unmigratedInputError(rawIn) }
  const raw = cloneJson(rawIn)
  if (!isPlainObject(input)) return { ok: false, error: '无效 provider 配置' }
  const name = input.provider
  if (!isValidProviderName(name)) return { ok: false, error: '无效 provider 名称' }
  const active = input.active
  if (active !== undefined && active !== '' && !isValidProviderName(active)) return { ok: false, error: '无效 provider 名称' }

  const fields: JsonObject = {}
  for (const [k, v] of Object.entries(input)) {
    if (k === 'provider' || k === 'active' || v === undefined) continue
    if (!PROVIDER_FIELDS[k]) return { ok: false, error: unknownKey(`provider.${name}.${k}`) }
    fields[k] = v
  }
  // 空串 = 「这一项没填」而不是「清空配置」（与既有 provider:save 行为一致）
  if (fields.api_key === '') delete fields.api_key
  if (fields.base_url === '') delete fields.base_url
  if (Object.keys(fields).length === 0 && active === undefined) return { ok: true, raw, changed: false, touched: [] }

  const ctx: Ctx = { touched: [] }
  const providers = isPlainObject(raw.provider) ? (raw.provider as JsonObject) : {}
  const current = isPlainObject(providers[name]) ? (providers[name] as JsonObject) : {}
  // 用户重新保存配置 = 重新启用（清除删除标记；否则 bridge 重启仍视为已删除）。
  delete current.deleted
  const err = applyObjectPatch(current, PROVIDER_FIELDS, fields, `provider.${name}`, ctx)
  if (err) return { ok: false, error: err }
  if (active !== undefined && active !== '') {
    providers.active = active
    ctx.touched.push('provider.active')
  }
  if (Object.keys(current).length === 0) delete providers[name]
  else providers[name] = current
  raw.provider = providers
  settleExplicit(raw, ctx)
  return { ok: true, raw, changed: !deepEqual(rawIn, raw), touched: ctx.touched }
}

export interface ProviderRemoveOptions {
  /** 内置 provider 不物理删除（bridge 会预置复活），用 deleted 标记。 */
  builtins: readonly string[]
  /** 删除活跃 provider 时的回退顺序（缺省 = builtins）。 */
  fallbackOrder?: readonly string[]
}

/** applyProviderRemove：只改 provider.<name> 与 provider.active，其余段/兄弟原样保留。 */
export function applyProviderRemove(rawIn: JsonObject, provider: unknown, opts: ProviderRemoveOptions): SettingsApplyResult {
  if (!isWritable(rawIn)) return { ok: false, error: unmigratedInputError(rawIn) }
  if (!isValidProviderName(provider)) return { ok: false, error: '无效 provider 名称' }
  const raw = cloneJson(rawIn)
  const providers = isPlainObject(raw.provider) ? (raw.provider as JsonObject) : {}
  const entry = providers[provider]
  if (opts.builtins.includes(provider)) {
    // 不重建对象、不补默认值：只加标记，已有字段（含用户 api_key/label/prices）原样带过。
    const cfg = isPlainObject(entry) ? (entry as JsonObject) : {}
    cfg.deleted = true
    providers[provider] = cfg
  } else {
    delete providers[provider]
  }
  const touched = [`provider.${provider}`]
  if (providers.active === provider) {
    const order = opts.fallbackOrder ?? opts.builtins
    const next = order.find((b) => b !== provider && !(isPlainObject(providers[b]) && (providers[b] as JsonObject).deleted === true))
    providers.active = next ?? 'deepseek'
    touched.push('provider.active')
  }
  raw.provider = providers
  settleExplicit(raw, { touched })
  return { ok: true, raw, changed: !deepEqual(rawIn, raw), touched }
}

/** applyMcpServers：mcpServers 段全量替换（只改这一段，顶层其它键原样保留）。 */
export function applyMcpServers(rawIn: JsonObject, servers: unknown): SettingsApplyResult {
  if (!isWritable(rawIn)) return { ok: false, error: unmigratedInputError(rawIn) }
  const raw = cloneJson(rawIn)
  const ctx: Ctx = { touched: [] }
  const target: JsonObject = {}
  const err = applyMcpServersIn(target, 'mcpServers', servers, 'mcpServers', ctx)
  if (err) return { ok: false, error: err }
  if (isPlainObject(target.mcpServers)) raw.mcpServers = target.mcpServers
  else delete raw.mcpServers
  settleExplicit(raw, ctx)
  return { ok: true, raw, changed: !deepEqual(rawIn, raw), touched: ctx.touched }
}

// ─────────────────────────────── 文件 IO（可注入）与迁移执行 ───────────────────────────────

export interface SettingsFsOps {
  mkdir(dir: string, mode: number): void
  readFile(file: string): string
  writeFile(file: string, text: string, mode: number): void
  rename(from: string, to: string): void
  unlink(file: string): void
}

function dirOf(file: string): string {
  const i = Math.max(file.lastIndexOf('/'), file.lastIndexOf('\\'))
  return i <= 0 ? '.' : file.slice(0, i)
}

/**
 * atomicWriteText：目录 0700 + 同目录临时文件（0600）+ rename 原子替换。
 * 失败时清掉临时文件并抛出 —— 目标文件保持原样（要么旧、要么新，绝无半截）。
 */
export function atomicWriteText(io: SettingsFsOps, file: string, text: string, mode = 0o600): void {
  io.mkdir(dirOf(file), 0o700)
  const tmp = `${file}.tmp`
  try {
    io.writeFile(tmp, text, mode)
    io.rename(tmp, file)
  } catch (e) {
    try {
      io.unlink(tmp)
    } catch {
      /* 无残留 / 删不掉：忽略 */
    }
    throw e
  }
}

export interface SettingsMigrationRun {
  ok: boolean
  skipped?: 'missing' | 'already-v2' | 'corrupt'
  changed?: boolean
  backup?: string
  removed?: string[]
  error?: string
}

function errCode(e: unknown): string {
  return typeof e === 'object' && e !== null && 'code' in e ? String((e as { code: unknown }).code) : ''
}

function errMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

/**
 * readSettingsForWrite：**写路径专用的读盘**（BR-01 的安全属性，P5-08 复审 B2 的修法）。
 *
 * 「写即迁移」：读原文件 → 已是 v2 就返回原始对象 → 未迁移（v1/更早）则**先**写
 * `<file>.bak`（0600，原文本逐字节）再原子写迁移结果，然后返回**迁移后**的原始对象。
 *
 * 三条硬约束：
 *   · 迁移没成功 → 返回 `ok:false`，调用方必须**拒绝这次写**（原文件保持不变）。绝不把
 *     未迁移对象交给 apply*：那会盖 `schema_version: 2`，下次启动 already-v2 跳过迁移，
 *     固化默认值再也删不掉。
 *   · 只认**原始对象**；缺文件 → `{ schema_version: 2 }`（首次运行的干净起点，不写盘）。
 *   · 坏文件（读失败 / JSON 坏 / 非对象）→ `skipped:'corrupt'` + `ok:false`，不碰文件。
 *
 * 幂等：v2 文件只读不写；.bak 不会被第二次覆盖。
 */
export interface SettingsWriteRead {
  ok: boolean
  /** ok 时的原始对象（**一定是 v2**：未迁移文件已在返回前迁移落盘）。 */
  raw?: JsonObject
  /** 没迁移时的原因（诊断用：missing / already-v2 / corrupt）。 */
  skipped?: 'missing' | 'already-v2' | 'corrupt'
  changed?: boolean
  backup?: string
  removed?: string[]
  error?: string
}

export function readSettingsForWrite(args: SettingsMigrationArgs): SettingsWriteRead {
  const { file, backupFile, defaults, io, log } = args
  let text: string
  try {
    text = io.readFile(file)
  } catch (e) {
    if (errCode(e) === 'ENOENT') {
      // 首次运行 / 用户删过：空文档就是最干净的 v2 起点（没有任何默认值可固化）。
      return { ok: true, raw: { schema_version: SETTINGS_SCHEMA_VERSION }, skipped: 'missing' }
    }
    return { ok: false, skipped: 'corrupt', error: `读取 ${file} 失败（已拒绝这次写入、原文件保持不变）: ${errMessage(e)}` }
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch (e) {
    return { ok: false, skipped: 'corrupt', error: `${file} 不是有效 JSON（${errMessage(e)}），已拒绝这次写入、原文件保持不变` }
  }
  if (!isPlainObject(parsed)) {
    return { ok: false, skipped: 'corrupt', error: `${file} 不是 JSON 对象，已拒绝这次写入、原文件保持不变` }
  }
  if (isV2Settings(parsed)) return { ok: true, raw: parsed, skipped: 'already-v2' }

  const m = migrateSettingsV2(parsed, defaults)
  try {
    atomicWriteText(io, backupFile, text, 0o600)
  } catch (e) {
    return { ok: false, error: `备份 ${backupFile} 失败（未改动 ${file}，已拒绝这次写入）：${errMessage(e)}` }
  }
  try {
    atomicWriteText(io, file, `${JSON.stringify(m.raw, null, 2)}\n`, 0o600)
  } catch (e) {
    // 原子写失败：目标文件未被 rename 覆盖，仍是迁移前那份；备份也还在。
    return { ok: false, error: `写回 ${file} 失败（原文件保持不变，备份 ${backupFile}，已拒绝这次写入）：${errMessage(e)}` }
  }
  log?.(`[settings] v1→v2 迁移：删除 ${m.removed.length} 个默认固化键，备份 ${backupFile}`)
  return { ok: true, raw: m.raw, changed: m.changed, backup: backupFile, removed: m.removed }
}

export interface SettingsMigrationArgs {
  file: string
  backupFile: string
  defaults: JsonObject
  io: SettingsFsOps
  log?: (msg: string) => void
}

/**
 * runSettingsMigration：启动时执行的 v1→v2 迁移（在 bridge spawn 之前调用）。
 *
 * 与写路径共用 readSettingsForWrite（同一套顺序与错误），只是把结果压成「跑了一次迁移」的
 * 报告形状。任一步失败都返回 `ok:false` 且**不留半截文件**；调用方只记日志、继续用旧文件
 * （启动迁移失败不阻止启动，后续写会重试迁移而不是盖章 —— 见 readSettingsForWrite）。
 */
export function runSettingsMigration(args: SettingsMigrationArgs): SettingsMigrationRun {
  const r = readSettingsForWrite(args)
  if (!r.ok) return r.skipped === 'corrupt' ? { ok: false, skipped: 'corrupt', error: r.error } : { ok: false, error: r.error }
  if (r.skipped) return { ok: true, skipped: r.skipped }
  return { ok: true, changed: r.changed, backup: r.backup, removed: r.removed }
}

export type SettingsCommitResult =
  | { ok: true; raw: JsonObject; changed: boolean }
  | { ok: false; error: string; corrupt?: boolean }

/**
 * commitSettingsWrite：四条写路径的**唯一出口**（main.ts 只调它）。
 *
 * 读 raw（未迁移先迁移落盘）→ `mutate`（applySettingsPatch / applyProviderSave /
 * applyProviderRemove / applyMcpServers）→ changed 才原子写。
 * 迁移失败 / 坏文件 → `ok:false`，**一个字节都不写**（B2：绝不给未迁移文件盖章）。
 *
 * 注意顺序：**先迁移再改**，所以 patch 本身非法时 v1 文件也已经迁移好了（幂等、带 .bak），
 * 只是这次修改被拒。这正是想要的：迁移不依赖 patch 是否合法，越早完成越安全。
 */
export function commitSettingsWrite(args: SettingsMigrationArgs & { mutate: (raw: JsonObject) => SettingsApplyResult }): SettingsCommitResult {
  const { mutate, ...readArgs } = args
  const read = readSettingsForWrite(readArgs)
  if (!read.ok || !read.raw) {
    return { ok: false, error: read.error ?? '读取 settings.json 失败', corrupt: read.skipped === 'corrupt' }
  }
  const applied = mutate(read.raw)
  if (!applied.ok) return { ok: false, error: applied.error }
  if (!applied.changed) return { ok: true, raw: applied.raw, changed: false }
  try {
    atomicWriteText(readArgs.io, readArgs.file, `${JSON.stringify(applied.raw, null, 2)}\n`, 0o600)
  } catch (e) {
    return { ok: false, error: `配置写盘失败（原文件保持不变）: ${errMessage(e)}` }
  }
  return { ok: true, raw: applied.raw, changed: true }
}
