// chrome-import.ts — 一键导入用户常用 Chrome 登录状态（Electron main）。
//
// 原理（见 docs/BROWSER_COOKIE_IMPORT.md §"一键导入"）：
//   用户 Chrome 与 go-code 受管 Chrome 是同一 macOS 用户、同一 Keychain 密钥
//   （'Chrome Safe Storage'）加密 cookie（v10 格式）→ 无需解密，直接把用户 Chrome
//   的登录相关文件复制进受管 profile，Chrome 启动时用同一把密钥自行解密。
//
// 安全模型：
//   - 值不透明：cookie 值只在 Chrome 内核内解密，本模块不做任何解密、不读取值、
//     不输出值；LLM/日志永远看不到。
//   - 用户主动授权：一键按钮 = 显式授权；导入前检测 Chrome 运行状态并提示退出。
//   - 可撤销：与 cookie:clear 共用清除逻辑（清受管 profile 的登录文件）。
//   - 只复制登录相关文件，不复制扩展/历史/缓存等无关数据。
//
// 边界：
//   - Chrome 运行中复制会拿到磁盘旧数据（内存态未落盘）+ 文件被锁 → 必须先退出。
//   - Chrome 版本升级若改加密格式（v10 已验证当前可用）→ 复制仍无害（同密钥体系）。
//   - 本模块不负责启动/重启受管 Chrome（由插件层 serverArgs / 现有 enable 流程处理）。

import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import { execFileSync } from 'node:child_process'
import { readChromeCookies, type ChromeCookie } from './chrome-cookies-read'

/** 受管 profile 根目录（与 plugin/browser 默认一致；window 模式用）。 */
export function managedProfileDir(): string {
  return path.join(os.homedir(), '.go-code', 'browser-profile')
}

/** 用户常用 Chrome profile 根目录（macOS 默认路径）。 */
export function userChromeDir(): string {
  return path.join(os.homedir(), 'Library', 'Application Support', 'Google', 'Chrome')
}

/** 用户 Chrome 的 Default profile 目录。 */
export function userDefaultDir(): string {
  return path.join(userChromeDir(), 'Default')
}

// —— 安全护栏：任何指向用户 Chrome 目录的写操作一律禁止 ——
// 导入流程对用户 Chrome 只读（sqlite3 -readonly 解密）。此断言防止未来改动误把
// 写操作指向用户 Chrome 数据（曾出现过把目标误当源复制的风险）。
const USER_CHROME_MARKER = path.join('Library', 'Application Support', 'Google', 'Chrome')

export function assertNotUserChromeWrite(p: string): void {
  const resolved = path.resolve(p)
  if (resolved.includes(USER_CHROME_MARKER)) {
    throw new Error(`安全拦截：禁止写入用户 Chrome 目录（${resolved}）。导入只读用户 Chrome，写入目标是 go-code 自己的存储。`)
  }
}

/** 检测 Chrome 是否在运行（macOS：pgrep 主进程；返回 true = 在运行）。 */
export function isChromeRunning(): boolean {
  try {
    execFileSync('/usr/bin/pgrep', ['-x', 'Google Chrome'], { stdio: 'ignore' })
    return true
  } catch {
    return false
  }
}

/** 检测受管 Chrome 是否在运行（SingletonLock 存在 = 运行中）。 */
export function isManagedChromeRunning(): boolean {
  return fs.existsSync(path.join(managedProfileDir(), 'SingletonLock'))
}

export interface ChromeImportResult {
  ok: boolean
  /** 复制的登录文件数（不含目录）。 */
  copied: number
  /** 是否检测到用户 Chrome 在运行（提示用）。 */
  chromeRunning: boolean
  /** 是否已自动退出并重开 Chrome（autoQuit 路径）。 */
  autoQuit: boolean
  error?: string
}

/** 优雅退出 Chrome（osascript quit → 触发落盘），并等待其完全退出。返回是否成功。 */
export function quitChrome(waitMs = 15000): boolean {
  // 紧急制动：CI/测试环境绝不允许退出用户真实 Chrome（历史事故：npm test 里的
  // chrome-import 测试走 autoQuit 路径，真实执行 osascript 把开发者正开着的 Chrome 退掉、
  // 登录态中断）。测试用 GO_CODE_NO_QUIT_CHROME=1 或注入 quitFn 替身。
  if (process.env.GO_CODE_NO_QUIT_CHROME === '1') {
    return false
  }
  try {
    execFileSync('/usr/bin/osascript', ['-e', 'tell application "Google Chrome" to quit'], { stdio: 'ignore', timeout: 10000 })
  } catch {
    /* osascript 失败（可能已被退出）继续走等待 */
  }
  const deadline = Date.now() + waitMs
  while (Date.now() < deadline) {
    if (!isChromeRunning()) return true
    // 等 300ms 再探测
    const sleepUntil = Date.now() + 300
    while (Date.now() < sleepUntil) {
      /* busy-wait 300ms */
    }
  }
  return !isChromeRunning()
}

/** 重新打开用户 Chrome（open -a）。返回是否成功发起。 */
export function relaunchChrome(): boolean {
  try {
    execFileSync('/usr/bin/open', ['-a', 'Google Chrome'], { stdio: 'ignore', timeout: 5000 })
    return true
  } catch {
    return false
  }
}

/**
 * 一键导入：把用户 Chrome 的登录 cookie **写入 Electron partition**（embedded 模式 Browser Use 实际使用的存储）。
 * 流程：检测/退出 Chrome → 读+解密 Chrome cookie（v10，同 Keychain）→ 经注入的写入函数落 partition。
 * 值不透明：value 只在 main 内存与 partition store 间流转，不返回渲染器、不落日志。
 *
 * @param writeFn 写入函数（main.ts 注入 Electron session.cookies.set；测试注入 mock）
 * @param opts chromeRunningOverride 测试注入（undefined=真实检测）；autoQuit=true 且 Chrome 运行时自动退出→导入→重开
 *   quitFn/relaunchFn 测试注入：**测试禁止触碰真实 Chrome**（否则跑 npm test 会把用户正开着的
 *   Chrome 退掉、登录态中断——历史事故）。默认取真实实现，生产行为不变。
 */
export async function importCookiesToPartition(
  writeFn: (cookies: ChromeCookie[]) => Promise<number>,
  opts?: {
    chromeRunningOverride?: boolean
    autoQuit?: boolean
    quitFn?: () => boolean
    relaunchFn?: () => boolean
    readFn?: () => ChromeCookie[]
  },
): Promise<ChromeImportResult> {
  const quit = opts?.quitFn ?? quitChrome
  const relaunch = opts?.relaunchFn ?? relaunchChrome
  // 前置：用户 Chrome 需退出（保证 WAL 落盘）；autoQuit=true 时自动优雅退出
  const running = opts?.chromeRunningOverride ?? isChromeRunning()
  if (running && !opts?.autoQuit) {
    return { ok: false, copied: 0, chromeRunning: true, autoQuit: false, error: '请先退出 Chrome 再导入（运行中会锁库且数据未落盘）' }
  }
  if (isManagedChromeRunning()) {
    return { ok: false, copied: 0, chromeRunning: true, autoQuit: false, error: '请先关闭应用内的浏览器会话再导入' }
  }
  let autoQuit = false
  if (running && opts?.autoQuit) {
    if (!quit()) {
      return { ok: false, copied: 0, chromeRunning: true, autoQuit: false, error: '自动退出 Chrome 失败，请手动退出后重试' }
    }
    autoQuit = true
  }

  try {
    const cookies = (opts?.readFn ?? readChromeCookies)()
    const written = await writeFn(cookies)
    // autoQuit 路径：导入完重新打开用户 Chrome（无感）
    if (autoQuit) {
      relaunch()
    }
    return { ok: true, copied: written, chromeRunning: false, autoQuit }
  } catch (e) {
    // autoQuit 路径失败：不返回 chromeRunning 提示（用户已授权自动退出）
    return { ok: false, copied: 0, chromeRunning: false, autoQuit: false, error: (e as Error).message }
  }
}

/**
 * 清除已导入的登录 cookie（可撤销）。clearFn 由 main.ts 注入（Electron cookies 逐条 remove）。
 * 返回清除条数。
 */
export async function clearImportedLogin(clearFn: (domains: string[]) => Promise<number>, domains: string[]): Promise<number> {
  return clearFn(domains)
}
