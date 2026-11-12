// keys-store.ts —— provider API key 的**唯一持久化容器**（~/.go-code/keys.json，0600）。
//
// BR-08（§6.1 第 2 条后半）：key 不再写进 settings.json（那是明文，且会被整份并集写回）。
// 现在的分工：
//   · settings.json = 非密钥配置（provider 的 base_url/models/protocol/…）；
//   · keys.json    = 每个 provider 一把 key，`{provider: {enc, data}}`：
//       - safeStorage 可用 → `enc:true` + encryptString（macOS 钥匙串 / Win DPAPI）；
//       - 不可用（无钥匙串的 Linux 容器等）→ `enc:false` + base64。
//         **这不是加密**，只是「无钥匙串环境的降级」，只为兼容历史 keys.json 形状；
//         任何安全声明都不得把它算成加密。
//   · bridge 不落盘 key：key 只经 spawn env（启动时当前活跃 provider 一把）与命令 payload
//     （切换/保存时按需注入）进入 bridge 内存。
//
// 本模块不 import electron、不直接碰 fs（io/crypto/时间全注入），因此能在 node:test 里
// 直接构造「坏文件 / 写失败 / 换机器解密失败」等分支；main.ts 只做 Electron 适配。
//
// 数据安全的三条硬约束（宁可重复不可丢）：
//   ① 坏 keys.json（JSON 坏 / 非对象）→ 备份 `.corrupt-<ts>` 并**拒绝写**，绝不覆盖丢 key；
//   ② 迁移先写 keys.json，**成功后才**从 settings.json 删明文；keys 写失败 → settings 一个字都不动；
//   ③ settings 写失败 → 明文留在 settings（key 已进 keys.json，重复但绝不丢）。
import { atomicWriteText, isValidProviderName, type JsonObject, type SettingsFsOps } from './settings-store'

/** keys.json 的一个条目：enc=true 时 data 是 safeStorage 密文（base64），否则是明文 base64。 */
export interface KeyEntry {
  enc: boolean
  data: string
}

/** keys.json 整体（按 provider 索引；未知/历史条目原样保留）。 */
export type KeyStoreFile = Record<string, unknown>

/** 迁移到 keys.json 后，settings 备份里替换明文的占位符。 */
export const MOVED_PLACEHOLDER = '<moved-to-keys.json>'

/** keys.json 读写用的文件适配器：settings 那套（原子写）再加 readdir（找 .corrupt-* 备份）。 */
export interface KeyStoreFsOps extends SettingsFsOps {
  /** 列目录（脱敏备份要枚举 `settings.json.corrupt-*`）；读不到目录应抛错。 */
  readdir(dir: string): string[]
}

/** 加密能力（main.ts 用 safeStorage 适配；测试里用假实现）。 */
export interface KeyCryptoOps {
  /** 钥匙串/DPAPI 是否可用（false → base64 降级，见文件头）。 */
  isEncryptionAvailable(): boolean
  /** 明文 → base64 密文。仅在 isEncryptionAvailable() 为 true 时调用。 */
  encryptString(plain: string): string
  /** base64 密文 → 明文。换机器/钥匙串丢失时会抛错。 */
  decryptString(data: string): string
}

export interface KeyStoreArgs {
  /** keys.json 绝对路径。 */
  file: string
  io: KeyStoreFsOps
  crypto: KeyCryptoOps
  /** 备份名时间戳（测试可固定）。 */
  now?: () => Date
  log?: (msg: string) => void
}

function isPlainObject(v: unknown): v is JsonObject {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

function cloneJson<T>(v: T): T {
  return JSON.parse(JSON.stringify(v)) as T
}

function dirOf(file: string): string {
  const i = Math.max(file.lastIndexOf('/'), file.lastIndexOf('\\'))
  return i <= 0 ? '.' : file.slice(0, i)
}

function baseOf(file: string): string {
  const i = Math.max(file.lastIndexOf('/'), file.lastIndexOf('\\'))
  return i < 0 ? file : file.slice(i + 1)
}

function joinPath(dir: string, name: string): string {
  return dir.endsWith('/') ? `${dir}${name}` : `${dir}/${name}`
}

function errCode(e: unknown): string {
  return typeof e === 'object' && e !== null && 'code' in e ? String((e as { code: unknown }).code) : ''
}

function errMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

function isKeyEntry(v: unknown): v is KeyEntry {
  return isPlainObject(v) && typeof v.enc === 'boolean' && typeof v.data === 'string'
}

/** 备份时间戳（与 main.ts quarantineCorruptSettings 的命名一致）。 */
function stamp(now?: () => Date): string {
  return (now?.() ?? new Date()).toISOString().replace(/[:.]/g, '-')
}

// ─────────────────────────────── 读 / 写 ───────────────────────────────

export type KeyStoreRead =
  | { ok: true; keys: KeyStoreFile; missing?: boolean }
  | { ok: false; error: string; backup?: string }

/**
 * readKeyStoreForWrite：**写路径专用**的读盘（任何 set/remove/copy/迁移都先过它）。
 *
 *   · 文件不存在 → `{}`（首次运行/用户删过：干净的起点，可以写）；
 *   · 坏 JSON / 非对象 → 备份 `keys.json.corrupt-<ts>`（0600，原文本逐字节）后返回 `ok:false`，
 *     调用方必须**拒绝这次写**。keys.json 是 key 的唯一副本，覆盖 = 永久丢失；
 *   · 其它读失败（权限/半截写入）→ 同样拒绝写（不备份，因为读不出内容）。
 */
export function readKeyStoreForWrite(args: KeyStoreArgs): KeyStoreRead {
  let text: string
  try {
    text = args.io.readFile(args.file)
  } catch (e) {
    if (errCode(e) === 'ENOENT') return { ok: true, keys: {}, missing: true }
    return { ok: false, error: `读取 ${args.file} 失败（已拒绝这次写入、原文件保持不变）：${errMessage(e)}` }
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch (e) {
    const backup = backupCorrupt(args, text)
    return {
      ok: false,
      backup,
      error: `${args.file} 不是有效 JSON（${errMessage(e)}）${backup ? `，已备份到 ${backup}` : ''}；`
        + '已拒绝这次写入（不覆盖、不丢 key）',
    }
  }
  if (!isPlainObject(parsed)) {
    const backup = backupCorrupt(args, text)
    return {
      ok: false,
      backup,
      error: `${args.file} 不是 JSON 对象${backup ? `（已备份到 ${backup}）` : ''}；已拒绝这次写入（不覆盖、不丢 key）`,
    }
  }
  return { ok: true, keys: parsed as KeyStoreFile }
}

/** 坏文件备份：copy 而非 rename —— 原文件留在原处，人工可恢复，写路径继续拒绝。 */
function backupCorrupt(args: KeyStoreArgs, text: string): string | undefined {
  const backup = `${args.file}.corrupt-${stamp(args.now)}`
  try {
    atomicWriteText(args.io, backup, text, 0o600)
    return backup
  } catch (e) {
    args.log?.(`[keys] 备份坏文件失败（${backup}）：${errMessage(e)}`)
    return undefined
  }
}

/** 宽松读（getKey/hasKey 用）：读不出 / 坏文件一律当空，绝不抛。 */
function readKeyStoreLoose(args: KeyStoreArgs): KeyStoreFile {
  try {
    const parsed: unknown = JSON.parse(args.io.readFile(args.file))
    return isPlainObject(parsed) ? (parsed as KeyStoreFile) : {}
  } catch {
    return {}
  }
}

/** 原子写整份 keys.json（tmp + rename，0600；未知 provider 条目原样保留由调用方保证）。 */
function writeKeyStore(args: KeyStoreArgs, keys: KeyStoreFile): void {
  atomicWriteText(args.io, args.file, `${JSON.stringify(keys, null, 2)}\n`, 0o600)
}

export type KeyStoreWrite = { ok: true; changed: boolean } | { ok: false; error: string }

/**
 * encodeKey：明文 → keys.json 条目。
 * safeStorage 可用时加密；不可用（或加密抛错）时**降级**为 base64（enc:false）——
 * 明确不是加密：只是让无钥匙串环境仍能保存 key，并在同一台机器上读回。
 */
function encodeKey(args: KeyStoreArgs, plaintext: string): KeyEntry {
  if (args.crypto.isEncryptionAvailable()) {
    try {
      return { enc: true, data: args.crypto.encryptString(plaintext) }
    } catch (e) {
      args.log?.(`[keys] safeStorage 加密失败，降级为 base64（enc:false，非加密）：${errMessage(e)}`)
    }
  }
  return { enc: false, data: Buffer.from(plaintext, 'utf8').toString('base64') }
}

/** setKey：写入某 provider 的 key（明文输入，落盘按 enc 编码）。空串等价于 removeKey。 */
export function setKey(args: KeyStoreArgs, provider: string, plaintext: string): KeyStoreWrite {
  if (!isValidProviderName(provider)) return { ok: false, error: `无效 provider 名称：${provider}` }
  const plain = plaintext ?? ''
  if (!plain) return removeKey(args, provider)
  const read = readKeyStoreForWrite(args)
  if (!read.ok) return { ok: false, error: read.error }
  const keys = read.keys
  const prev = keys[provider]
  const next = encodeKey(args, plain)
  // 内容相同就不重写（mtime/权限都不动；避免每次 provider:save 都碰钥匙串）
  if (isKeyEntry(prev) && prev.enc === next.enc && prev.data === next.data) return { ok: true, changed: false }
  keys[provider] = next
  try {
    writeKeyStore(args, keys)
  } catch (e) {
    return { ok: false, error: `写 ${args.file} 失败（原文件保持不变）：${errMessage(e)}` }
  }
  return { ok: true, changed: true }
}

/** removeKey：删除某 provider 的 key（不存在 → ok, changed:false；坏文件 → 拒绝写）。 */
export function removeKey(args: KeyStoreArgs, provider: string): KeyStoreWrite {
  if (!isValidProviderName(provider)) return { ok: false, error: `无效 provider 名称：${provider}` }
  const read = readKeyStoreForWrite(args)
  if (!read.ok) return { ok: false, error: read.error }
  if (!(provider in read.keys)) return { ok: true, changed: false }
  delete read.keys[provider]
  try {
    writeKeyStore(args, read.keys)
  } catch (e) {
    return { ok: false, error: `写 ${args.file} 失败（原文件保持不变）：${errMessage(e)}` }
  }
  return { ok: true, changed: true }
}

export type KeyCopyResult = { ok: true; copied: boolean } | { ok: false; error: string }

/**
 * copyKey：把 from 的条目原样复制到 to（provider 改名用）。
 * 不解密再加密：密文与机器绑定，原样搬条目既省一次钥匙串调用，也避免「解不开就丢 key」。
 * from 没有条目 → ok,copied:false（没有 key 的 provider 也能改名，不算失败）。
 */
export function copyKey(args: KeyStoreArgs, from: string, to: string): KeyCopyResult {
  if (!isValidProviderName(from) || !isValidProviderName(to)) return { ok: false, error: '无效 provider 名称' }
  const read = readKeyStoreForWrite(args)
  if (!read.ok) return { ok: false, error: read.error }
  const entry = read.keys[from]
  if (!isKeyEntry(entry)) return { ok: true, copied: false }
  read.keys[to] = { enc: entry.enc, data: entry.data }
  try {
    writeKeyStore(args, read.keys)
  } catch (e) {
    return { ok: false, error: `写 ${args.file} 失败（原文件保持不变）：${errMessage(e)}` }
  }
  return { ok: true, copied: true }
}

/**
 * getKey：取某 provider 的明文 key。解密失败（换机器 / 钥匙串丢失 / 落后格式）→ 空串，
 * 绝不抛错（renderer 侧「测试连接」按「未设置」提示，用户重新填一次即可）。
 */
export function getKey(args: KeyStoreArgs, provider: string): string {
  if (!isValidProviderName(provider)) return ''
  const entry = readKeyStoreLoose(args)[provider]
  if (!isKeyEntry(entry)) return ''
  try {
    return entry.enc ? args.crypto.decryptString(entry.data) : Buffer.from(entry.data, 'base64').toString('utf8')
  } catch (e) {
    args.log?.(`[keys] ${provider} 的 key 解密失败（换机器/钥匙串丢失？按未设置处理）：${errMessage(e)}`)
    return ''
  }
}

/** hasKey：条目存在且解得出非空明文（解不开的条目不算「已设置」——它用不了）。 */
export function hasKey(args: KeyStoreArgs, provider: string): boolean {
  return Boolean(getKey(args, provider).trim())
}

// ─────────────────────── settings 明文 → keys.json 迁移 ───────────────────────

export interface PlaintextMigrationArgs extends KeyStoreArgs {
  /** settings.json 的原始对象（写侧基线，**不含**默认值并集）。 */
  settingsRaw: JsonObject
  settingsFile: string
  /** 迁移时写的备份（与 settings-store 同一个 settings.json.bak）。 */
  settingsBackupFile: string
}

export interface PlaintextMigration {
  ok: boolean
  /** 已搬进 keys.json（且已从 settings 删掉明文）的 provider。 */
  moved: string[]
  /** 明文仍留在 settings 的 provider（keys 已写但 settings 写失败 → 下次启动重试）。 */
  kept: string[]
  /** settings.json 是否被改写（删掉了 api_key）。 */
  settingsChanged: boolean
  /** 本次被脱敏的备份文件。 */
  sanitized?: string[]
  error?: string
}

/**
 * migratePlaintextProviderKeys：存量 `provider.<name>.api_key` 明文 → keys.json。
 *
 * 顺序（每一步都为了「绝不丢 key」）：
 *   ① 先把 key 写进 keys.json —— settings 是当前最新来源，同名旧条目以明文为准覆盖；
 *   ② keys 写成功后才从 settings 原始对象删 `api_key` 并原子写回；
 *   ③ settings 写失败 → 明文**保留在 settings**（key 已在 keys.json，重复但绝不丢）；
 *   ④ 两者都成功后，把 settings 的备份（`.bak` / `.corrupt-*`）里的明文替换成占位符。
 *
 * 幂等：settings 没有明文 key 时什么都不写（不碰 mtime）；迁移失败的下次启动会自动重试。
 * 只处理合法 provider 名（bridge 侧同样按 validProviderName 过滤，非法名的 key 无法被使用，
 * 原样留在 settings 等人工清理）。
 */
export function migratePlaintextProviderKeys(args: PlaintextMigrationArgs): PlaintextMigration {
  const raw = args.settingsRaw
  const providers = isPlainObject(raw.provider) ? (raw.provider as JsonObject) : null
  const pending: Array<{ name: string; plain: string }> = []
  if (providers) {
    for (const [name, v] of Object.entries(providers)) {
      if (name === 'active' || !isPlainObject(v)) continue
      const k = v.api_key
      if (typeof k !== 'string' || !k.trim()) continue
      if (!isValidProviderName(name)) {
        args.log?.(`[keys] provider 名非法（${name}），其 api_key 留在 settings.json 等人工清理`)
        continue
      }
      pending.push({ name, plain: k })
    }
  }
  if (pending.length === 0) return { ok: true, moved: [], kept: [], settingsChanged: false }

  // ① 先写 keys.json（一次原子写：要么全进，要么一个都不进）
  const read = readKeyStoreForWrite(args)
  if (!read.ok) return { ok: false, moved: [], kept: pending.map((p) => p.name), settingsChanged: false, error: read.error }
  const keys = read.keys
  for (const p of pending) keys[p.name] = encodeKey(args, p.plain)
  try {
    writeKeyStore(args, keys)
  } catch (e) {
    // keys 没写成 → settings 一个字都不动（明文仍在，下次启动重试）
    return {
      ok: false,
      moved: [],
      kept: pending.map((p) => p.name),
      settingsChanged: false,
      error: `写 ${args.file} 失败（settings.json 未改动，明文保留）：${errMessage(e)}`,
    }
  }

  // ② 再删 settings 里的明文
  const next = cloneJson(raw)
  const nextProviders = next.provider as JsonObject
  for (const p of pending) {
    const cfg = nextProviders[p.name]
    if (isPlainObject(cfg)) delete cfg.api_key
  }
  try {
    atomicWriteText(args.io, args.settingsFile, `${JSON.stringify(next, null, 2)}\n`, 0o600)
  } catch (e) {
    args.log?.(`[keys] settings 写回失败（明文保留，key 已在 keys.json）：${errMessage(e)}`)
    return {
      ok: false,
      moved: [],
      kept: pending.map((p) => p.name),
      settingsChanged: false,
      error: `写 ${args.settingsFile} 失败（key 已存 ${args.file}，settings.json 明文保留）：${errMessage(e)}`,
    }
  }

  // ③ 脱敏备份（key 与 settings 都安全了才做；失败只是明文多留一份，不影响可用性）
  const sanitized = sanitizeSettingsBackups(args)
  // 迁移完成时把用户可见的明文从内存对象里摘掉，调用方可直接复用
  if (isPlainObject(raw.provider)) {
    const live = raw.provider as JsonObject
    for (const p of pending) {
      const cfg = live[p.name]
      if (isPlainObject(cfg)) delete cfg.api_key
    }
  }
  args.log?.(`[keys] 迁移明文 key：${pending.map((p) => p.name).join(', ')} → ${args.file}`
    + (sanitized.length ? `（已脱敏备份：${sanitized.map(baseOf).join(', ')}）` : ''))
  return { ok: true, moved: pending.map((p) => p.name), kept: [], settingsChanged: true, sanitized }
}

/** 备份脱敏：settings.json.bak + 同目录 settings.json.corrupt-*，0600 不变（原子写覆盖）。 */
function sanitizeSettingsBackups(args: PlaintextMigrationArgs): string[] {
  const dir = dirOf(args.settingsFile)
  const prefix = `${baseOf(args.settingsFile)}.corrupt-`
  const targets = new Set<string>([args.settingsBackupFile])
  try {
    for (const name of args.io.readdir(dir)) if (name.startsWith(prefix)) targets.add(joinPath(dir, name))
  } catch {
    /* 目录读不到：至少处理已知的 .bak */
  }
  const done: string[] = []
  for (const file of targets) {
    let text: string
    try {
      text = args.io.readFile(file)
    } catch (e) {
      if (errCode(e) !== 'ENOENT') args.log?.(`[keys] 读备份 ${file} 失败（明文可能仍在）：${errMessage(e)}`)
      continue
    }
    const next = redactSettingsText(text)
    if (next === text) continue
    try {
      atomicWriteText(args.io, file, next, 0o600)
      done.push(file)
    } catch (e) {
      args.log?.(`[keys] 脱敏备份 ${file} 失败（明文可能仍在）：${errMessage(e)}`)
    }
  }
  return done
}

/**
 * redactSettingsText：把一份 settings 文本里的 provider 明文 key 换成占位符。
 * 合法 JSON 走 parse（精确、不误伤其它字段）；坏文件用保守正则只替换 `"api_key": "..."`。
 */
export function redactSettingsText(text: string): string {
  try {
    const parsed: unknown = JSON.parse(text)
    if (isPlainObject(parsed)) {
      let changed = false
      const providers = isPlainObject(parsed.provider) ? (parsed.provider as JsonObject) : null
      if (providers) {
        for (const [name, v] of Object.entries(providers)) {
          if (name === 'active' || !isPlainObject(v)) continue
          if (typeof v.api_key === 'string' && v.api_key !== '' && v.api_key !== MOVED_PLACEHOLDER) {
            v.api_key = MOVED_PLACEHOLDER
            changed = true
          }
        }
      }
      // 没有明文 key 就**不动**原文本（不用重新序列化改坏别人的格式）
      return changed ? `${JSON.stringify(parsed, null, 2)}\n` : text
    }
  } catch {
    /* 坏 JSON：走下面的正则 */
  }
  return text.replace(/"api_key"\s*:\s*"[^"]*"/g, `"api_key": "${MOVED_PLACEHOLDER}"`)
}

// ─────────────────────── spawn env / 桥命令的 key 注入 ───────────────────────

/**
 * 内置五卡的固定 env 名；自定义 provider 用通用 GOCODE_KEY_<UPPER(id)>。
 * （bridge 的 keyLocked 也按这两个名字兜底读 env。）
 */
export function envNameForProvider(provider: string): string | null {
  const fixed: Record<string, string> = {
    deepseek: 'DEEPSEEK_API_TOKEN',
    openai: 'OPENAI_API_KEY',
    opencode: 'OPENCODE_API_KEY',
    kimi: 'KIMI_API_KEY',
    zhipu: 'ZHIPUAI_API_KEY',
  }
  if (fixed[provider]) return fixed[provider]
  if (isValidProviderName(provider)) return `GOCODE_KEY_${provider.toUpperCase()}`
  return null
}

/**
 * activeProviderKeyEnv：spawn env 里**只放当前活跃 provider 的一把 key**（BR-08）。
 *
 * 旧实现把所有已存 key 都塞进 env：一旦 spawn，bridge 的所有 MCP/插件子进程都继承这些
 * 明文 key（最小权限原则失效），而且旧条目会在 bridge 内存为空时代替新 key（401 根因）。
 * 现在 env 只作「活跃 provider 的启动兜底」，非活跃 provider 的 key 只在切换时随命令注入。
 */
export function activeProviderKeyEnv(active: string, lookup: (provider: string) => string): Record<string, string> {
  if (!isValidProviderName(active)) return {}
  const name = envNameForProvider(active)
  if (!name) return {}
  const key = (lookup(active) ?? '').trim()
  return key ? { [name]: key } : {}
}

/**
 * 需要「按 provider 带 key」的桥命令（白名单，不是「带 provider 就注入」）：
 *   · set_provider    —— 保存/切换 provider（provider:save 之后 renderer 发的热切换）；
 *   · switch_model    —— 切模型；前端不带 key，跨 provider 切模型靠它拿到新 key；
 *   · list_models     —— 设置页对任意一家「拉取模型列表」；
 *   · provider_billing —— 设置页余额/配额（单家查询）。
 *
 * 后两条不是可选项：env 里只有**活跃** provider 的 key，非活跃 provider 若不带 key，
 * bridge 的 keyLocked 会返回空 → 「未配置 API key」（功能回归）。白名单而不是通配，
 * 是为了不让 key 跟着无关命令流转（少一条泄漏/落日志的路径）。
 */
const PROVIDER_KEY_COMMANDS = new Set(['set_provider', 'switch_model', 'list_models', 'provider_billing'])

/**
 * injectProviderKey：bridge 命令的唯一出口（main.sendCommand）在转发前按需补 key。
 *
 *   · 命令在 PROVIDER_KEY_COMMANDS 里、payload 带合法 provider、且 payload **没有** api_key
 *     → 按该 provider 从 keys.json 取 key 注入；
 *   · payload 已带 `api_key`（含显式空串 = 清除）→ 原样返回，**绝不覆盖**；
 *   · 无可用 key / 非白名单命令 / 非法 provider → 原样返回。
 *
 * 返回值是新对象（不改入参），且**不产生任何日志**——key 不进日志/错误消息。
 */
export function injectProviderKey(cmd: unknown, lookup: (provider: string) => string): unknown {
  if (!isPlainObject(cmd) || !isPlainObject(cmd.payload)) return cmd
  if (typeof cmd.type !== 'string' || !PROVIDER_KEY_COMMANDS.has(cmd.type)) return cmd
  const payload = cmd.payload
  if ('api_key' in payload) return cmd
  const provider = payload.provider
  if (typeof provider !== 'string' || !isValidProviderName(provider)) return cmd
  const key = (lookup(provider) ?? '').trim()
  if (!key) return cmd
  return { ...cmd, payload: { ...payload, api_key: key } }
}
