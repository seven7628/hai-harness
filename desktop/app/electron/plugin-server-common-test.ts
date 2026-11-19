// plugin-server-common-test.ts —— web.start 纯函数的边界（cwd / env / 探活 URL）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { baseServerEnv, joinHealthUrl, resolveServerCwd, serverTimeoutMs, stripAnsi } from './plugin-server-common.ts'

function mkPlugin(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'plugin-server-cwd-'))
  fs.mkdirSync(path.join(dir, 'app'), { recursive: true })
  return dir
}

test('cwd 缺省 = 插件目录；相对子目录在目录内', () => {
  const dir = mkPlugin()
  assert.equal(resolveServerCwd(dir), fs.realpathSync(dir))
  assert.equal(resolveServerCwd(dir, '.'), fs.realpathSync(dir))
  assert.equal(resolveServerCwd(dir, 'app'), path.join(fs.realpathSync(dir), 'app'))
})

test('cwd 上跳 / 软链逃逸 / 不存在都拒', () => {
  const dir = mkPlugin()
  assert.throws(() => resolveServerCwd(dir, '..'), /上跳/)
  fs.symlinkSync(os.tmpdir(), path.join(dir, 'escape'))
  assert.throws(() => resolveServerCwd(dir, 'escape'), /逃出/)
  assert.throws(() => resolveServerCwd(dir, 'missing'), /ENOENT|no such/i)
})

test('绝对 cwd 是显式逃生门（不要求位于插件目录内）', () => {
  const dir = mkPlugin()
  const outside = fs.mkdtempSync(path.join(os.tmpdir(), 'plugin-server-outside-'))
  assert.equal(resolveServerCwd(dir, outside), fs.realpathSync(outside))
})

test('最小环境不含宿主的 SECRET / ELECTRON_*，manifest env 可覆盖', () => {
  const env = baseServerEnv({ PATH: '/bin', HOME: '/home/u', SECRET_TOKEN: 'x', ELECTRON_RUN_AS_NODE: '1' } as NodeJS.ProcessEnv)
  assert.equal(env.SECRET_TOKEN, undefined)
  assert.equal(env.ELECTRON_RUN_AS_NODE, undefined)
  assert.equal(env.PATH, '/bin')
  assert.equal(env.HOME, '/home/u')
})

test('health URL 只拼字符串（协议相对也留在原 origin）', () => {
  assert.equal(joinHealthUrl('http://127.0.0.1:5235', '/health'), 'http://127.0.0.1:5235/health')
  assert.equal(joinHealthUrl('http://127.0.0.1:5235/', 'health'), 'http://127.0.0.1:5235/health')
  assert.equal(joinHealthUrl('http://127.0.0.1:5235/app', '?x=1'), 'http://127.0.0.1:5235/app/?x=1')
  assert.equal(joinHealthUrl('http://127.0.0.1:5235', '//evil.com'), 'http://127.0.0.1:5235/evil.com')
})

test('timeout 缺省/夹取；ANSI 清理', () => {
  assert.equal(serverTimeoutMs(undefined), 20000)
  assert.equal(serverTimeoutMs(0), 20000)
  assert.equal(serverTimeoutMs(1500), 1500)
  assert.equal(serverTimeoutMs(999999), 60000)
  assert.equal(stripAnsi('\u001b[31mred\u001b[0m'), 'red')
})
