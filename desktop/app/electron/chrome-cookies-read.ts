// chrome-cookies-read.ts — 读取并解密用户 Chrome 的 cookie（Electron main）。
//
// 背景（docs/BROWSER_COOKIE_IMPORT.md 修订5）：
//   Browser Use 默认 embedded 模式使用 Electron partition（persist:gocode-browser）
//   存 cookie，而"复制文件"只对 window 模式（~/.go-code/browser-profile）生效。
//   因此方案改为：**解密用户 Chrome cookie → 经 Electron cookies.set() 写入 partition**。
//
// 解密算法（macOS Chrome v10）：
//   password = Keychain generic password（service='Chrome Safe Storage', account='Chrome'）
//   key = PBKDF2-HMAC-SHA1(password, salt='saltysalt', iter=1003, dkLen=16)
//   结构: 'v10' || iv(16) || AES-128-CBC 密文 || 明文前 16 字节为随机前缀（忽略）
//   （实测验证：本机可解密，明文尾部即 cookie 值）
//
// 安全：值只在 main 进程内存流转，写入 Electron partition（明文存储）后即弃；
//   不落日志、不返回渲染器。

import { execFileSync, execSync } from 'node:child_process'
import * as crypto from 'node:crypto'
import * as os from 'node:os'
import * as path from 'node:path'

/** 用户 Chrome 的 Cookies SQLite 路径。 */
export function userCookiesDb(): string {
  return path.join(os.homedir(), 'Library', 'Application Support', 'Google', 'Chrome', 'Default', 'Cookies')
}

/** 从 Keychain 取 Chrome Safe Storage 口令（只读，不新建）。失败返回 null。 */
export function getChromeSafeStoragePassword(): string | null {
  try {
    const out = execFileSync('/usr/bin/security', ['find-generic-password', '-w', '-s', 'Chrome Safe Storage', '-a', 'Chrome'], {
      encoding: 'utf8',
      timeout: 10000,
      stdio: ['ignore', 'pipe', 'ignore'],
    })
    return out.trim() || null
  } catch {
    return null
  }
}

/** 由口令派生 16 字节 AES 密钥（对齐 Chromium os_crypt）。 */
export function deriveChromeKey(password: string): Buffer {
  return crypto.pbkdf2Sync(password, 'saltysalt', 1003, 16, 'sha1')
}

/** 解密一条 v10 cookie 值；非 v10 / 失败返回 null。 */
export function decryptChromeCookie(encrypted: Buffer, key: Buffer): string | null {
  if (!encrypted || encrypted.length < 35 || encrypted.subarray(0, 3).toString('latin1') !== 'v10') {
    return null
  }
  const iv = encrypted.subarray(3, 19)
  const ct = encrypted.subarray(19)
  try {
    const decipher = crypto.createDecipheriv('aes-128-cbc', key, iv)
    // Node 的 final() 已自动去除 PKCS7 padding
    const pt = Buffer.concat([decipher.update(ct), decipher.final()])
    // 前 16 字节是 Chrome 附加随机前缀（v10 明文头），跳过
    return pt.length > 16 ? pt.subarray(16).toString('utf8') : null
  } catch {
    return null
  }
}

export interface ChromeCookie {
  domain: string
  name: string
  value: string
  path: string
  secure: boolean
  httpOnly: boolean
  expirationDate?: number
  sameSite: 'unspecified' | 'no_restriction' | 'lax' | 'strict'
}

/** 读用户 Chrome Cookies 库并解密全部 cookie（只读；需 Chrome 已退出保证落盘）。 */
export function readChromeCookies(): ChromeCookie[] {
  const db = userCookiesDb()
  if (!require('node:fs').existsSync(db)) {
    throw new Error(`未找到用户 Chrome Cookies 库（${db}）`)
  }
  const password = getChromeSafeStoragePassword()
  if (!password) {
    throw new Error('无法读取 Chrome 安全存储口令（Keychain 未授权）')
  }
  const key = deriveChromeKey(password)
  const out: ChromeCookie[] = []
  // 用 sqlite3 CLI 只读导出 encrypted_value 的 hex（避免引入原生依赖）。
  // **安全约束：-readonly 保证任何 SQL 都无法写库**（含 PRAGMA journal_mode 等）。
  // 列名随 Chrome 版本变化（151 用 is_secure/is_httponly；旧版 secure/httponly）——用 PRAGMA 探测
  const schema = execSync(`sqlite3 -readonly "${db}" "PRAGMA table_info(cookies)"`, { encoding: 'utf8', timeout: 10000 })
  const hasIsSecure = /is_secure/.test(schema)
  const secureCol = hasIsSecure ? 'is_secure' : 'secure'
  const httpOnlyCol = hasIsSecure ? 'is_httponly' : 'httponly'
  const sql = `SELECT host_key, name, path, ${secureCol}, ${httpOnlyCol}, expires_utc, samesite, hex(encrypted_value) FROM cookies`
  const result = execSync(`sqlite3 -readonly "${db}" "${sql}"`, { encoding: 'utf8', timeout: 20000, maxBuffer: 64 * 1024 * 1024 })
  // sqlite3 CLI 默认输出：列间 | 分隔，行间 \n；NULL 为空串
  for (const line of result.split('\n')) {
    if (!line.trim()) continue
    const cols = line.split('|')
    if (cols.length < 8) continue
    const [host, name, p, secure, httpOnly, expiresUtc, sameSite, encHex] = cols
    if (!encHex) continue
    const enc = Buffer.from(encHex, 'hex')
    const value = decryptChromeCookie(enc, key)
    if (value === null) continue
    out.push({
      domain: host,
      name,
      value,
      path: p || '/',
      secure: secure === '1',
      httpOnly: httpOnly === '1',
      expirationDate: expiresUtc ? chromeTimeToUnix(Number(expiresUtc)) : undefined,
      sameSite: mapSameSite(sameSite),
    })
  }
  return out
}

/** Chrome 的 expires_utc 是微秒（1601 epoch），转 Unix 秒。 */
export function chromeTimeToUnix(us: number): number {
  if (!us || us <= 0) return undefined as unknown as number
  return Math.round(us / 1_000_000 - 11644473600)
}

export function mapSameSite(v: string): ChromeCookie['sameSite'] {
  switch (v) {
    case '0': return 'unspecified'
    case '1': return 'no_restriction'
    case '2': return 'lax'
    case '3': return 'strict'
    default: return 'lax'
  }
}
