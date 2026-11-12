// keys-store-test.ts —— provider key 存储与注入语义的回归（node:test，不启动 Electron）。
//
// 钉住 §6.1 第 2 条后半（BR-08）：
//   ① keys.json 形状与编码：safeStorage 可用 → enc:true；不可用 → enc:false + base64
//      （**降级不是加密**）；解密失败按未设置；原子写 0600、未知 provider 条目原样保留；
//   ② 坏文件（坏 JSON / 非对象）→ 备份 `.corrupt-<ts>` + **拒绝写**（不覆盖丢 key）；
//   ③ 迁移：先写 keys.json 再删 settings 明文；keys 写失败 → settings 一个字不动；
//      settings 写失败 → 明文保留（宁可重复不可丢）；幂等；备份（.bak / .corrupt-*）脱敏；
//   ④ 注入面：spawn env 只放**当前活跃** provider 一把 key；set_provider / switch_model
//      缺 api_key 时按 provider 注入，已带的 key 绝不覆盖；
//   ⑤ main.ts 接线守卫（只作补充：行为断言在 ①–④，防止「逻辑对了但没接上」）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, renameSync, rmSync, statSync, unlinkSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  MOVED_PLACEHOLDER,
  activeProviderKeyEnv,
  copyKey,
  envNameForProvider,
  getKey,
  hasKey,
  injectProviderKey,
  migratePlaintextProviderKeys,
  readKeyStoreForWrite,
  redactSettingsText,
  removeKey,
  setKey,
  type KeyCryptoOps,
  type KeyStoreFsOps,
  type KeyStoreFile,
} from './keys-store'
import type { JsonObject } from './settings-store'

// ─────────────────────────────── 夹具 ───────────────────────────────

const KEYS = '/cfg/keys.json'
const SETTINGS = '/cfg/settings.json'
const BAK = '/cfg/settings.json.bak'

/** 内存 fs：可注入失败点，记录每次写/rename（含 mode），验证 0600 与原子写。 */
function fakeIo(files: Record<string, string> = {}) {
  const store = new Map<string, { text: string; mode: number }>()
  for (const [k, v] of Object.entries(files)) store.set(k, { text: v, mode: 0o600 })
  const calls: string[] = []
  const fail = { write: new Set<string>(), rename: new Set<string>(), read: new Set<string>() }
  const io: KeyStoreFsOps = {
    mkdir(dir, mode) {
      calls.push(`mkdir ${dir} ${mode.toString(8)}`)
    },
    readFile(f) {
      if (fail.read.has(f)) throw new Error(`EACCES: ${f}`)
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
      calls.push(`rename ${from} → ${to}`)
      store.set(to, e)
      store.delete(from)
    },
    unlink(f) {
      calls.push(`unlink ${f}`)
      store.delete(f)
    },
    readdir(dir) {
      const prefix = `${dir}/`
      return [...store.keys()]
        .filter((p) => p.startsWith(prefix))
        .map((p) => p.slice(prefix.length))
        .filter((n) => !n.includes('/'))
    },
  }
  const text = (f: string) => store.get(f)?.text
  const mode = (f: string) => store.get(f)?.mode
  return { io, store, calls, fail, text, mode }
}

/** 假钥匙串：enc 模式 = 「加密」成 base64('enc:'+plain)；plain 模式 = 加密不可用。 */
function fakeCrypto(opts: { available?: boolean; decryptFails?: boolean } = {}): KeyCryptoOps {
  const available = opts.available ?? true
  return {
    isEncryptionAvailable: () => available,
    encryptString: (plain) => Buffer.from(`enc:${plain}`, 'utf8').toString('base64'),
    decryptString: (data) => {
      if (opts.decryptFails) throw new Error('keychain unavailable')
      const s = Buffer.from(data, 'base64').toString('utf8')
      if (!s.startsWith('enc:')) throw new Error('not encrypted by fake crypto')
      return s.slice(4)
    },
  }
}

const NOW = () => new Date('2026-09-30T12:00:00.000Z')
const STAMP = '2026-09-30T12-00-00-000Z'

function args(io: ReturnType<typeof fakeIo>['io'], crypto: KeyCryptoOps = fakeCrypto()) {
  return { file: KEYS, io, crypto, now: NOW }
}

/** 迁移夹具：settings 原始对象（v2，provider.*.api_key 明文仍在）+ keys.json 旧条目。 */
function rawSettings(extra: JsonObject = {}): JsonObject {
  return {
    schema_version: 2,
    provider: {
      active: 'deepseek',
      deepseek: { base_url: 'https://api.deepseek.com/v1', api_key: 'sk-deepseek' },
      openai: { base_url: 'https://api.openai.com/v1', api_key: 'sk-openai', label: '我的 OpenAI' },
      myproxy: { base_url: 'https://proxy.example/v1', api_key: '' }, // 空串 = 未设置，不迁移
      zhipu: { deleted: true }, // 无 key：原样
    },
    hooks: { pre_tool: [{ name: 'audit' }] },
    ...extra,
  }
}

function migrationArgs(io: ReturnType<typeof fakeIo>['io'], raw: JsonObject, crypto: KeyCryptoOps = fakeCrypto()) {
  return { ...args(io, crypto), settingsRaw: raw, settingsFile: SETTINGS, settingsBackupFile: BAK }
}

// ───────────────────────── ① 编码 / 读写 / 原子写 ─────────────────────────

test('setKey + getKey：safeStorage 可用 → enc:true（密文）；文件 0600 且原子写（tmp+rename）', () => {
  const f = fakeIo()
  const a = args(f.io)
  const r = setKey(a, 'deepseek', 'sk-plain-1')
  assert.equal(r.ok, true)
  assert.equal(r.ok ? r.changed : false, true)

  const keys = JSON.parse(f.text(KEYS)!) as KeyStoreFile
  const entry = keys.deepseek as { enc: boolean; data: string }
  assert.equal(entry.enc, true, '有钥匙串时必须标记加密')
  assert.equal(Buffer.from(entry.data, 'base64').toString('utf8'), 'enc:sk-plain-1')
  assert.equal(entry.data.includes('sk-plain-1'), false, '落盘不得出现明文')
  assert.equal(f.mode(KEYS), 0o600, 'keys.json 必须 0600')
  assert.ok(f.calls.some((c) => c === `write ${KEYS}.tmp 600`), '先写同目录 tmp')
  assert.ok(f.calls.some((c) => c === `rename ${KEYS}.tmp → ${KEYS}`), 'tmp → rename 原子替换')
  assert.equal(getKey(a, 'deepseek'), 'sk-plain-1')
  assert.equal(hasKey(a, 'deepseek'), true)
})

test('无钥匙串 → enc:false + base64（降级不是加密，但能读回）；解密失败按未设置', () => {
  const f = fakeIo()
  const plainCrypto = fakeCrypto({ available: false })
  const a = args(f.io, plainCrypto)
  assert.equal(setKey(a, 'kimi', 'sk-kimi-1').ok, true)
  const entry = (JSON.parse(f.text(KEYS)!) as KeyStoreFile).kimi as { enc: boolean; data: string }
  assert.equal(entry.enc, false)
  assert.equal(Buffer.from(entry.data, 'base64').toString('utf8'), 'sk-kimi-1')
  assert.equal(getKey(a, 'kimi'), 'sk-kimi-1')

  // 换机器 / 钥匙串丢失：加密条目解密抛错 → 空串（绝不把半截/错误数据当 key 用）
  const broken = args(f.io, fakeCrypto({ decryptFails: true }))
  assert.equal(getKey(broken, 'kimi'), 'sk-kimi-1', 'enc:false（base64 降级）跨机器仍可读')
  assert.equal(setKey(broken, 'openai', 'sk-openai-1').ok, true)
  assert.equal(getKey(broken, 'openai'), '')
  assert.equal(hasKey(broken, 'openai'), false, '解不开的条目不算「已设置」')
})

test('setKey/removeKey/copyKey：未知 provider 条目原样保留；内容相同不重写', () => {
  const legacy = { legacy: { enc: false, data: Buffer.from('sk-legacy').toString('base64') }, future: { note: 'keep-me' } }
  const f = fakeIo({ [KEYS]: JSON.stringify(legacy) })
  const a = args(f.io)
  assert.equal(setKey(a, 'zhipu', 'sk-zhipu-1').ok, true)
  let keys = JSON.parse(f.text(KEYS)!) as KeyStoreFile
  assert.deepEqual(keys.legacy, legacy.legacy, '未知/历史条目必须原样保留')
  assert.deepEqual(keys.future, legacy.future, '认不出的条目也不能删')
  assert.equal(getKey(a, 'legacy'), 'sk-legacy', '旧 enc:false 条目仍可读（格式兼容）')

  // 幂等：同一明文再写一次不碰文件
  const writes = () => f.calls.filter((c) => c.startsWith('write') || c.startsWith('rename')).length
  const before = writes()
  assert.equal(setKey(a, 'zhipu', 'sk-zhipu-1').ok, true)
  assert.equal(writes(), before, '内容没变就不重写')

  // copyKey：原样搬条目（不解密）
  const cp = copyKey(a, 'zhipu', 'zhipu2')
  assert.deepEqual(cp, { ok: true, copied: true })
  keys = JSON.parse(f.text(KEYS)!) as KeyStoreFile
  assert.deepEqual(keys.zhipu2, keys.zhipu)
  assert.equal(getKey(a, 'zhipu2'), 'sk-zhipu-1')
  // from 没有条目 → ok,copied:false（没有 key 的 provider 也能改名）
  assert.deepEqual(copyKey(a, 'nosuch', 'other'), { ok: true, copied: false })

  // removeKey：只删目标 provider，其它条目不动
  assert.deepEqual(removeKey(a, 'zhipu'), { ok: true, changed: true })
  keys = JSON.parse(f.text(KEYS)!) as KeyStoreFile
  assert.equal('zhipu' in keys, false)
  assert.equal(getKey(a, 'zhipu2'), 'sk-zhipu-1')
  assert.deepEqual(removeKey(a, 'zhipu'), { ok: true, changed: false }, '删不存在的条目 = no-op')
})

// ───────────────────────── ② 坏文件：备份 + 拒绝写 ─────────────────────────

test('坏 JSON → 备份 .corrupt-<ts>（0600）并拒绝写；原文件一个字节都不改', () => {
  const broken = '{"deepseek": {"enc": true, "data": "abc"' // 半截 JSON
  const f = fakeIo({ [KEYS]: broken })
  const a = args(f.io)

  const read = readKeyStoreForWrite(a)
  assert.equal(read.ok, false)
  assert.equal(read.ok ? '' : read.backup, `${KEYS}.corrupt-${STAMP}`)
  assert.equal(f.text(`${KEYS}.corrupt-${STAMP}`), broken, '备份必须逐字节保留原内容')
  assert.equal(f.mode(`${KEYS}.corrupt-${STAMP}`), 0o600)

  const w = setKey(a, 'deepseek', 'sk-new')
  assert.equal(w.ok, false)
  assert.match(w.ok ? '' : w.error, /已拒绝这次写入/)
  assert.equal(f.text(KEYS), broken, '坏文件绝不能被覆盖（覆盖 = 丢 key）')

  // remove / copy / 迁移同样拒绝
  assert.equal(removeKey(a, 'deepseek').ok, false)
  assert.equal(copyKey(a, 'deepseek', 'x').ok, false)
  const m = migratePlaintextProviderKeys(migrationArgs(f.io, rawSettings()))
  assert.equal(m.ok, false)
  assert.deepEqual(m.kept, ['deepseek', 'openai'])
  assert.equal(JSON.stringify(m).includes('sk-deepseek'), false, '错误消息不得带明文 key')
})

test('非对象 JSON（数组/字符串）同样拒绝写；读失败（非 ENOENT）也拒绝写', () => {
  const f = fakeIo({ [KEYS]: '[]' })
  const r = readKeyStoreForWrite(args(f.io))
  assert.equal(r.ok, false)
  assert.equal(f.text(`${KEYS}.corrupt-${STAMP}`), '[]')

  const f2 = fakeIo({ [KEYS]: '{"a":{"enc":false,"data":""}}' })
  f2.fail.read.add(KEYS)
  const r2 = readKeyStoreForWrite(args(f2.io))
  assert.equal(r2.ok, false)
  assert.match(r2.ok ? '' : r2.error, /读取/)
  assert.equal(f2.text(KEYS), '{"a":{"enc":false,"data":""}}')
})

test('文件不存在 → 空起点（可以写）；空串 setKey = 删除', () => {
  const f = fakeIo()
  const read = readKeyStoreForWrite(args(f.io))
  assert.equal(read.ok, true)
  assert.deepEqual(read.ok ? read.keys : null, {})
  assert.equal(read.ok ? read.missing : false, true)

  const a = args(f.io)
  assert.equal(setKey(a, 'deepseek', 'sk-1').ok, true)
  assert.deepEqual(setKey(a, 'deepseek', ''), { ok: true, changed: true })
  assert.deepEqual(JSON.parse(f.text(KEYS)!), {})
})

// ───────────────────────── ③ 迁移：明文 → keys.json ─────────────────────────

test('迁移成功：先写 keys.json 再删 settings 明文；其它字段/键逐字节保留', () => {
  const settingsText = JSON.stringify(rawSettings(), null, 2)
  const f = fakeIo({ [SETTINGS]: settingsText })
  const raw = rawSettings()
  const m = migratePlaintextProviderKeys(migrationArgs(f.io, raw))

  assert.equal(m.ok, true)
  assert.deepEqual(m.moved, ['deepseek', 'openai'])
  assert.equal(m.settingsChanged, true)

  // keys.json：两把 key 都在，且是加密条目
  const keys = JSON.parse(f.text(KEYS)!) as KeyStoreFile
  assert.equal((keys.deepseek as { enc: boolean }).enc, true)
  const a = args(f.io)
  assert.equal(getKey(a, 'deepseek'), 'sk-deepseek')
  assert.equal(getKey(a, 'openai'), 'sk-openai')

  // settings：api_key 消失，其它字段原样（label/base_url/hooks/schema_version 不动）
  const out = JSON.parse(f.text(SETTINGS)!) as JsonObject
  const prov = out.provider as JsonObject
  assert.deepEqual(prov.deepseek, { base_url: 'https://api.deepseek.com/v1' })
  assert.deepEqual(prov.openai, { base_url: 'https://api.openai.com/v1', label: '我的 OpenAI' })
  assert.deepEqual(prov.myproxy, { base_url: 'https://proxy.example/v1', api_key: '' }, '空串不是 key：不迁移、原样保留')
  assert.deepEqual(prov.zhipu, { deleted: true })
  assert.deepEqual(out.hooks, { pre_tool: [{ name: 'audit' }] })
  assert.equal(out.schema_version, 2)
  assert.equal(f.text(SETTINGS)!.includes('sk-deepseek'), false, 'settings 不得再有明文 key')
  assert.equal(f.text(SETTINGS)!.includes('sk-openai'), false)
  assert.equal(f.mode(SETTINGS), 0o600)

  // 迁移后内存对象同步摘掉明文（调用方可直接复用）
  assert.equal(JSON.stringify(raw).includes('sk-deepseek'), false)

  // 写顺序：keys.json 的 rename 早于 settings.json 的 rename（先 key 后删明文）
  const keysAt = f.calls.findIndex((c) => c === `rename ${KEYS}.tmp → ${KEYS}`)
  const settingsAt = f.calls.findIndex((c) => c === `rename ${SETTINGS}.tmp → ${SETTINGS}`)
  assert.ok(keysAt >= 0 && settingsAt > keysAt, `必须先写 keys.json（${keysAt} < ${settingsAt}）`)
})

test('迁移：同名旧条目以 settings 明文为准覆盖', () => {
  const old = { deepseek: { enc: false, data: Buffer.from('sk-old').toString('base64') }, keep: { enc: false, data: 'x' } }
  const f = fakeIo({ [KEYS]: JSON.stringify(old) })
  const raw = rawSettings()
  const m = migratePlaintextProviderKeys(migrationArgs(f.io, raw))
  assert.equal(m.ok, true)
  assert.equal(getKey(args(f.io), 'deepseek'), 'sk-deepseek', 'settings 是当前最新来源')
  const keys = JSON.parse(f.text(KEYS)!) as KeyStoreFile
  assert.deepEqual(keys.keep, old.keep, '无关旧条目保留')
})

test('迁移：keys.json 写失败 → settings 一个字都不改（明文保留，下次重试）', () => {
  const settingsText = JSON.stringify(rawSettings(), null, 2)
  const f = fakeIo({ [SETTINGS]: settingsText })
  f.fail.write.add(`${KEYS}.tmp`)
  const m = migratePlaintextProviderKeys(migrationArgs(f.io, rawSettings()))
  assert.equal(m.ok, false)
  assert.deepEqual(m.moved, [])
  assert.deepEqual(m.kept, ['deepseek', 'openai'])
  assert.equal(m.settingsChanged, false)
  assert.equal(f.text(SETTINGS), settingsText, 'keys 没写成 → settings 不得动')
  assert.equal(f.store.has(KEYS), false)
  assert.equal(m.error?.includes('sk-deepseek'), false, '错误消息不得带明文 key')
})

test('迁移：settings 写失败 → 明文保留在 settings（宁可重复不可丢），key 已在 keys.json', () => {
  const settingsText = JSON.stringify(rawSettings(), null, 2)
  const f = fakeIo({ [SETTINGS]: settingsText })
  f.fail.write.add(`${SETTINGS}.tmp`)
  const raw = rawSettings()
  const m = migratePlaintextProviderKeys(migrationArgs(f.io, raw))
  assert.equal(m.ok, false)
  assert.deepEqual(m.moved, [])
  assert.deepEqual(m.kept, ['deepseek', 'openai'])
  assert.equal(m.settingsChanged, false)
  assert.equal(f.text(SETTINGS), settingsText, 'settings 写失败 → 原文（含明文）保留')
  assert.equal(getKey(args(f.io), 'deepseek'), 'sk-deepseek', 'key 已进 keys.json（重复但不丢）')
  assert.equal(JSON.stringify(raw).includes('sk-deepseek'), true, '内存对象也未摘明文（与磁盘一致）')

  // 修好磁盘后重试（幂等）：成功且只删一次
  f.fail.write.clear()
  const m2 = migratePlaintextProviderKeys(migrationArgs(f.io, rawSettings()))
  assert.equal(m2.ok, true)
  assert.equal(f.text(SETTINGS)!.includes('sk-deepseek'), false)
})

test('迁移幂等：settings 已无明文 → 不写任何文件（不碰 mtime）', () => {
  const f = fakeIo()
  const raw: JsonObject = { schema_version: 2, provider: { active: 'deepseek', deepseek: { base_url: 'https://d/v1' } } }
  f.store.set(SETTINGS, { text: JSON.stringify(raw, null, 2), mode: 0o600 })
  const before = f.calls.length
  const m = migratePlaintextProviderKeys(migrationArgs(f.io, raw))
  assert.equal(m.ok, true)
  assert.deepEqual(m.moved, [])
  assert.equal(m.settingsChanged, false)
  assert.equal(f.calls.length, before, '没有明文 key 时一个文件都不写')
  assert.equal(f.text(KEYS), undefined)
})

test('迁移：备份脱敏（.bak 合法 JSON / .corrupt-* 坏 JSON 走保守正则），0600 不变', () => {
  const good = JSON.stringify(rawSettings(), null, 2)
  const brokenBak = '{"provider":{"deepseek":{"api_key":"sk-broken-key","base_url":"https://d/v1"}' // 坏 JSON
  const f = fakeIo({
    [SETTINGS]: good,
    [BAK]: good,
    [`${SETTINGS}.corrupt-2026-09-01T00-00-00-000Z`]: brokenBak,
  })
  const m = migratePlaintextProviderKeys(migrationArgs(f.io, rawSettings()))
  assert.equal(m.ok, true)
  assert.deepEqual((m.sanitized ?? []).map((p) => p.replace('/cfg/', '')).sort(), [
    'settings.json.bak',
    'settings.json.corrupt-2026-09-01T00-00-00-000Z',
  ])

  const bak = f.text(BAK)!
  assert.equal(bak.includes('sk-deepseek'), false, '.bak 不得再有明文 key')
  const bakJson = JSON.parse(bak) as JsonObject
  assert.equal(((bakJson.provider as JsonObject).deepseek as JsonObject).api_key, MOVED_PLACEHOLDER)
  // 其余字段不变（脱敏不是重写配置）
  assert.deepEqual(((bakJson.provider as JsonObject).openai as JsonObject).label, '我的 OpenAI')
  assert.equal(f.mode(BAK), 0o600)

  const corrupt = f.text(`${SETTINGS}.corrupt-2026-09-01T00-00-00-000Z`)!
  assert.equal(corrupt.includes('sk-broken-key'), false, '坏备份也要脱敏（正则路径）')
  assert.equal(corrupt.includes(MOVED_PLACEHOLDER), true)
  assert.equal(f.mode(`${SETTINGS}.corrupt-2026-09-01T00-00-00-000Z`), 0o600)
})

test('redactSettingsText：无明文 key 的文本原样返回（保持用户格式/字节）', () => {
  const noKey = '{"provider":{"deepseek":{"base_url":"https://d/v1"}},"theme":"dark"}'
  assert.equal(redactSettingsText(noKey), noKey)
  const already = `{"provider":{"deepseek":{"api_key":"${MOVED_PLACEHOLDER}"}}}`
  assert.equal(redactSettingsText(already), already)
  // 坏 JSON：只替换 api_key 的值，其它字符（含坏结构）原样
  const bad = '{"provider":{"a":{"api_key": "sk-1", "x": }}}'
  assert.equal(redactSettingsText(bad), `{"provider":{"a":{"api_key": "${MOVED_PLACEHOLDER}", "x": }}}`)
})

test('真实 fs（tmp 目录）：迁移落盘可读回、keys.json/settings.json/备份均 0600', () => {
  const dir = mkdtempSync(join(tmpdir(), 'keys-store-'))
  try {
    const keysFile = join(dir, 'keys.json')
    const settingsFile = join(dir, 'settings.json')
    const bakFile = `${settingsFile}.bak`
    const raw = rawSettings()
    const text = JSON.stringify(raw, null, 2)
    writeFileSync(settingsFile, text, { mode: 0o644 }) // 旧实现可能是 0644：迁移后必须收紧到 0600
    writeFileSync(bakFile, text, { mode: 0o644 })
    const io: KeyStoreFsOps = {
      mkdir: (d, mode) => mkdirSync(d, { recursive: true, mode }),
      readFile: (f) => readFileSync(f, 'utf8'),
      writeFile: (f, t, mode) => writeFileSync(f, t, { mode }),
      rename: (a, b) => renameSync(a, b),
      unlink: (f) => unlinkSync(f),
      readdir: (d) => readdirSync(d),
    }
    const a = { file: keysFile, io, crypto: fakeCrypto(), now: NOW }
    const m = migratePlaintextProviderKeys({ ...a, settingsRaw: raw, settingsFile, settingsBackupFile: bakFile })
    assert.equal(m.ok, true)
    assert.deepEqual(m.moved, ['deepseek', 'openai'])

    assert.equal(statSync(keysFile).mode & 0o777, 0o600, 'keys.json 0600')
    assert.equal(statSync(settingsFile).mode & 0o777, 0o600, 'settings.json 0600')
    assert.equal(statSync(bakFile).mode & 0o777, 0o600, '.bak 0600')
    assert.equal(getKey(a, 'deepseek'), 'sk-deepseek')
    assert.equal(readFileSync(settingsFile, 'utf8').includes('sk-deepseek'), false)
    assert.equal(readFileSync(bakFile, 'utf8').includes('sk-deepseek'), false)
    // 目录里没有残留 tmp
    assert.equal(readdirSync(dir).some((n) => n.endsWith('.tmp')), false, '原子写的 tmp 必须清干净')
    assert.equal(statSync(keysFile).size > 0, true)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// ───────────────────────── ④ spawn env / 命令注入 ─────────────────────────

test('activeProviderKeyEnv：只注入当前活跃 provider 的一把 key（无 key → 空）', () => {
  const looked: string[] = []
  const store: Record<string, string> = { deepseek: 'sk-d', openai: 'sk-o', myproxy: 'sk-m' }
  const lookup = (p: string) => {
    looked.push(p)
    return store[p] ?? ''
  }
  assert.deepEqual(activeProviderKeyEnv('openai', lookup), { OPENAI_API_KEY: 'sk-o' })
  assert.deepEqual(looked, ['openai'], '只查活跃 provider，绝不遍历全部已存 key')
  assert.deepEqual(activeProviderKeyEnv('myproxy', lookup), { GOCODE_KEY_MYPROXY: 'sk-m' })
  assert.deepEqual(activeProviderKeyEnv('nosuch', lookup), {}, '没有 key → 不注入空值')
  assert.deepEqual(activeProviderKeyEnv('active', lookup), {}, '保留键不是 provider')
  assert.equal(envNameForProvider('zhipu'), 'ZHIPUAI_API_KEY')
  assert.equal(envNameForProvider('kimi'), 'KIMI_API_KEY')
  assert.equal(envNameForProvider('bad name'), null)
})

test('injectProviderKey：set_provider/switch_model/list_models/provider_billing 缺 key 时按 provider 注入，已带 key 不覆盖', () => {
  const store: Record<string, string> = { deepseek: 'sk-d', zhipu: 'sk-z' }
  const lookup = (p: string) => store[p] ?? ''

  // set_provider：缺 api_key → 注入
  assert.deepEqual(
    injectProviderKey({ id: 1, type: 'set_provider', payload: { provider: 'deepseek', base_url: 'https://d/v1' } }, lookup),
    { id: 1, type: 'set_provider', payload: { provider: 'deepseek', base_url: 'https://d/v1', api_key: 'sk-d' } },
  )
  // 已带 key（含显式空串 = 清除）→ 原样
  const explicit = { type: 'set_provider', payload: { provider: 'deepseek', api_key: 'sk-user' } }
  assert.equal(injectProviderKey(explicit, lookup), explicit)
  const clear = { type: 'set_provider', payload: { provider: 'deepseek', api_key: '' } }
  assert.equal(injectProviderKey(clear, lookup), clear, '显式空串是「清 key」，不得补回')

  // switch_model（前端 switchModel 不带 key）：按 payload.provider 注入
  assert.deepEqual(
    injectProviderKey({ id: 2, type: 'switch_model', payload: { session_id: 's1', model: 'glm-4.6', provider: 'zhipu' } }, lookup),
    { id: 2, type: 'switch_model', payload: { session_id: 's1', model: 'glm-4.6', provider: 'zhipu', api_key: 'sk-z' } },
  )
  // switch_model 不带 provider / 该 provider 无 key → 原样
  const noProv = { type: 'switch_model', payload: { session_id: 's1', model: 'deepseek-chat' } }
  assert.equal(injectProviderKey(noProv, lookup), noProv)
  const noKey = { type: 'switch_model', payload: { provider: 'nosuch', model: 'x' } }
  assert.equal(injectProviderKey(noKey, lookup), noKey)

  // list_models / provider_billing：非活跃 provider 也要能用（env 只有活跃一把 key）
  for (const type of ['list_models', 'provider_billing']) {
    assert.deepEqual(
      injectProviderKey({ type, payload: { provider: 'zhipu' } }, lookup),
      { type, payload: { provider: 'zhipu', api_key: 'sk-z' } },
      `${type} 必须带上该 provider 的 key`,
    )
    assert.equal(injectProviderKey({ type, payload: { provider: 'zhipu', api_key: '' } }, lookup).payload.api_key, '', `${type} 已带空 key 不覆盖`)
  }

  // 其它命令 / 非法 payload → 原样（不改入参；白名单外一律不注入 key）
  const other = { type: 'ask_batch', payload: { provider: 'deepseek' } }
  assert.equal(injectProviderKey(other, lookup), other)
  assert.equal(JSON.stringify(other).includes('api_key'), false, '不得改动原命令对象')
  assert.equal(injectProviderKey(null, lookup), null)
  assert.equal(JSON.stringify(injectProviderKey({ type: 'set_provider' }, lookup)), '{"type":"set_provider"}')
})

// ───────────────────────── ⑤ main.ts 接线守卫（补充） ─────────────────────────

const MAIN = join(process.cwd(), 'electron', 'main.ts')

/** 去注释 + 去空白（跨行写法与注释都不该骗过守卫）。 */
function flatten(src: string): string {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/(^|\n)\s*\/\/.*/g, '$1')
    .replace(/\s+/g, '')
}

test('main.ts：settings 明文迁移在 spawn 前跑；provider:save 不写 api_key；env/命令走 keys-store', () => {
  assert.equal(existsSync(MAIN), true, `找不到 ${MAIN}（应在 desktop/app 下运行本测试）`)
  const src = readFileSync(MAIN, 'utf8')
  const flat = flatten(src)

  // 启动顺序：settings 迁移 → key 迁移 → （renderer 触发的）spawn
  const ready = src.indexOf('app.whenReady().then(')
  assert.ok(ready >= 0, '找不到 app.whenReady')
  const block = src.slice(ready, src.indexOf('})', ready) + 2)
  const v2At = block.indexOf('migrateSettingsOnStartup()')
  const keyAt = block.indexOf('migrateProviderKeysOnStartup()')
  assert.ok(v2At >= 0 && keyAt > v2At, 'key 迁移必须排在 settings v2 迁移之后')
  assert.ok(block.indexOf('createWindow()') > keyAt, 'key 迁移必须先于 createWindow')
  // 启动路径仍不直接 spawn bridge（只由 renderer bridge:start 触发）
  assert.equal(/app\.whenReady[\s\S]{0,900}?spawnBridge\(/.test(src), false, 'whenReady 里不得直接 spawnBridge')

  // settings 写路径：provider:save 一律抹掉 api_key 再交给 settings-store
  assert.ok(flat.includes('api_key:undefined'), 'provider:save 必须把 api_key 从 settings 写侧剔除')
  // 旧「删 keys.json 旧条目 + settings 当主源」的注释/逻辑不得复活
  assert.equal(flat.includes('settings是新主源'), false)

  // env：只经 activeProviderKeyEnv（单 key）；不再有「遍历 keys.json 全部条目」的注入
  const envFn = src.slice(src.indexOf('function loadKeysEnv'), src.indexOf('\n}', src.indexOf('function loadKeysEnv')))
  assert.ok(envFn.includes('activeProviderKeyEnv('), 'loadKeysEnv 必须走 activeProviderKeyEnv（只注入活跃 provider 一把）')
  assert.equal(/for\s*\(/.test(envFn), false, 'loadKeysEnv 不得遍历全部已存 key')

  // 命令出口：sendCommand 转发前注入（唯一出口）
  const sendFn = src.slice(src.indexOf('function sendCommand'), src.indexOf('\n}', src.indexOf('function sendCommand')))
  assert.ok(sendFn.includes('injectProviderKey('), 'sendCommand 必须调 injectProviderKey（bridge 命令唯一出口）')
  assert.ok(sendFn.includes('JSON.stringify('), '注入结果才是真正写进 bridge stdin 的命令')

  // key 只经 keys-store：主进程不得自己解析 keys.json
  assert.equal(flat.includes('JSON.parse(fs.readFileSync(KEYS_FILE()'), false, 'keys.json 的解析只在 keys-store')
  assert.ok(flat.includes("from'./keys-store'"))
})
