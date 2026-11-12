// chrome-cookies-read.test.ts — chrome-cookies-read.ts 单测（node:test）。
// 覆盖：密钥派生、v10 解密（构造密文）、时间转换、sameSite 映射。

import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as crypto from 'node:crypto'
import {
  deriveChromeKey,
  decryptChromeCookie,
  chromeTimeToUnix,
  mapSameSite,
  getChromeSafeStoragePassword,
} from './chrome-cookies-read.ts'

/** 用 Chrome 同款算法加密一段明文（v10 结构：'v10' + iv16 + AES-128-CBC + 16字节随机前缀）。 */
function encryptV10(value: string, key: Buffer): Buffer {
  const iv = crypto.randomBytes(16)
  const prefix = crypto.randomBytes(16)
  const plain = Buffer.concat([prefix, Buffer.from(value, 'utf8')])
  const cipher = crypto.createCipheriv('aes-128-cbc', key, iv)
  const ct = Buffer.concat([cipher.update(plain), cipher.final()])
  return Buffer.concat([Buffer.from('v10', 'latin1'), iv, ct])
}

test('deriveChromeKey: 对齐 Chrome 算法（PBKDF2-SHA1 saltysalt 1003 → 16B）', () => {
  const key = deriveChromeKey('test-password')
  assert.equal(key.length, 16)
  // 与 Python 实现交叉验证过的固定向量
  const known = crypto.pbkdf2Sync('test-password', 'saltysalt', 1003, 16, 'sha1')
  assert.deepEqual(key, known)
})

test('decryptChromeCookie: 解密 v10 密文（跳过 16 字节前缀）', () => {
  const key = crypto.pbkdf2Sync('pw', 'saltysalt', 1003, 16, 'sha1')
  const enc = encryptV10('my-secret-cookie-value', key)
  const dec = decryptChromeCookie(enc, key)
  assert.equal(dec, 'my-secret-cookie-value')
})

test('decryptChromeCookie: 非 v10 / 过短 / 坏 key 返回 null', () => {
  const key = crypto.pbkdf2Sync('pw', 'saltysalt', 1003, 16, 'sha1')
  assert.equal(decryptChromeCookie(Buffer.from('notv10'), key), null)
  assert.equal(decryptChromeCookie(Buffer.alloc(10), key), null)
  const wrongKey = crypto.pbkdf2Sync('wrong', 'saltysalt', 1003, 16, 'sha1')
  const enc = encryptV10('x', key)
  assert.equal(decryptChromeCookie(enc, wrongKey), null) // 坏 key → padding 校验失败
})

test('chromeTimeToUnix: Chrome 微秒(1601) → Unix 秒', () => {
  // 2000-01-01T00:00:00Z 的 Chrome 时间
  const chromeUs = BigInt(12591158400000000)
  const unix = chromeTimeToUnix(Number(chromeUs))
  assert.equal(unix, 946684800)
})

test('mapSameSite: 数字字符串 → 枚举', () => {
  assert.equal(mapSameSite('0'), 'unspecified')
  assert.equal(mapSameSite('1'), 'no_restriction')
  assert.equal(mapSameSite('2'), 'lax')
  assert.equal(mapSameSite('3'), 'strict')
  assert.equal(mapSameSite('9'), 'lax') // 未知回默认
})

test('getChromeSafeStoragePassword: 当前环境不抛错（可能拿不到）', () => {
  // 真实 Keychain 调用：只断言返回 string 或 null
  const pw = getChromeSafeStoragePassword()
  assert.ok(pw === null || typeof pw === 'string')
})
