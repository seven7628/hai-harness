// plugin-server-common.ts —— web.start 的纯函数（不 import electron，可在 Node 里单测）。
//
// 这里放三件容易写错、且错了只在真机才暴露的事：
//   1. cwd 的 realpath 边界（相对路径不能借软链逃出插件目录；绝对路径是显式逃生门）；
//   2. 子进程的最小环境（不能把应用进程的 API key / electron 变量整套交给插件服务）；
//   3. 探活 URL 的字符串拼接（不能把 health 拼成另一个 origin）。
import fs from 'node:fs'
import path from 'node:path'

/** spawn 到就绪的缺省上限（与 bridge 的 surfaceWebStartDefaultTimeoutMS 同值）。 */
export const DEFAULT_SERVER_TIMEOUT_MS = 20000

const POSIX_ENV_KEYS = ['PATH', 'HOME', 'USER', 'LOGNAME', 'TMPDIR', 'TMP', 'TEMP', 'LANG', 'LC_ALL', 'LC_CTYPE', 'SHELL']
const WIN_ENV_KEYS = [
  'PATH', 'PATHEXT', 'SystemRoot', 'COMSPEC', 'TEMP', 'TMP',
  'USERPROFILE', 'APPDATA', 'LOCALAPPDATA', 'HOMEDRIVE', 'HOMEPATH', 'NUMBER_OF_PROCESSORS',
]

/**
 * baseServerEnv 给插件服务的最小环境。
 *
 * 为什么不直接 `{...process.env}`：Electron/宿主进程里可能有 provider key、bridge token、
 * 调试开关；一个 manifest 的 start 命令就能把它们读走。MCP server 的 env 也是「配置什么
 * 给什么」，这里沿用同一口径：PATH/HOME 等启动必需项 + manifest.start.env 显式覆盖。
 */
export function baseServerEnv(src: NodeJS.ProcessEnv = process.env): Record<string, string> {
  const keys = process.platform === 'win32' ? WIN_ENV_KEYS : POSIX_ENV_KEYS
  const out: Record<string, string> = {}
  for (const k of keys) {
    const v = src[k]
    if (typeof v === 'string' && v) out[k] = v
  }
  return out
}

/**
 * resolveServerCwd 把 manifest.start.cwd 解析成真实目录。
 *
 * 相对路径：必须在插件目录内（realpath 后再比一次，挡软链逃逸），且任何 `..` 段直接拒
 * （与安装期同一口径，运行期不放宽）。绝对路径：显式允许（React 源码常在另一个仓库），
 * 但仍然要求存在且是目录。
 */
export function resolveServerCwd(pluginDir: string, cwd?: string): string {
  const raw = String(cwd ?? '').trim()
  const realPlugin = fs.realpathSync(pluginDir)
  if (!raw) return realPlugin
  if (!path.isAbsolute(raw)) {
    if (raw.replace(/\\/g, '/').split('/').includes('..')) throw new Error(`cwd 不许上跳（..）：${raw}`)
  }
  const dir = path.isAbsolute(raw) ? path.resolve(raw) : path.resolve(realPlugin, raw)
  const real = fs.realpathSync(dir)
  if (!fs.statSync(real).isDirectory()) throw new Error(`cwd 不是目录：${dir}`)
  if (!path.isAbsolute(raw)) {
    const rel = path.relative(realPlugin, real)
    if (rel === '..' || rel.startsWith('..' + path.sep) || path.isAbsolute(rel)) {
      throw new Error(`cwd 经软链逃出了插件目录：${raw}`)
    }
  }
  return real
}

/** joinHealthUrl 探活地址：与渲染层 appUrlOf 同一套字符串拼接，不解析 path。 */
export function joinHealthUrl(base: string, health?: string): string {
  const b = String(base ?? '').trim().replace(/\/+$/, '')
  const h = String(health ?? '').trim().replace(/^\/+/, '')
  return h ? `${b}/${h}` : `${b}/`
}

/** 终端/框架输出里的 ANSI 颜色不进入宿主错误态（否则日志是一屏转义码）。 */
export function stripAnsi(s: string): string {
  // eslint-disable-next-line no-control-regex
  return String(s ?? '').replace(/\u001b\[[0-9;]*m/g, '')
}

/** timeout_ms 的缺省/夹取（安装期已判死非法值；运行期只做 0 → 默认）。 */
export function serverTimeoutMs(ms?: number): number {
  return typeof ms === 'number' && Number.isFinite(ms) && ms > 0 ? Math.min(ms, 60000) : DEFAULT_SERVER_TIMEOUT_MS
}

/** 一行日志的清洗：去 ANSI、去尾空白；空行保留（堆栈可读）。 */
export function cleanLogLine(line: string): string {
  return stripAnsi(line).replace(/\r$/, '').trimEnd()
}
