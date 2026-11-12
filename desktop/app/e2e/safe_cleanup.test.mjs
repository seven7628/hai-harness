// safe_cleanup.test.mjs — E2E 清理安全护栏的回归测试（node:test）。
//
// 防的是**已发生过的真实事故**：E2E 用 `pkill -9 -f <通用名>` 清孤儿进程，把用户正开着的
// Chrome（`--user-data-dir=~/.go-code/browser-profile`）和浏览器会话 MCP
// （`.../chrome-devtools-mcp/...`）一起 SIGKILL，登录态中断。
//
// 本测试锁死不变量：清理器**只能命中带本 E2E 隔离标记的进程**，对模拟的用户真实进程零命中。
// 用法：cd desktop/app && node --test e2e/safe_cleanup.test.mjs

import { test } from 'node:test'
import assert from 'node:assert/strict'
import os from 'node:os'
import path from 'node:path'
import { createCleaner, assertNoForeignKill, listProcesses } from './safe_cleanup.mjs'

const REAL_HOME = os.homedir()
const E2E_HOME = path.join(REAL_HOME, '.go-code-e2e-home')
const E2E_WS = '/tmp/gocode-client-e2e-ws'

// —— 模拟进程表（命令行取自真实事故现场）——
const FAKE_PROCS = [
  // 用户正在使用的 Chrome（默认 profile）——必须零命中
  { pid: 1612, command: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome' },
  // 用户真实受管 Chrome ——必须零命中
  {
    pid: 1700,
    command: `/Applications/Google Chrome.app/Contents/MacOS/Google Chrome --user-data-dir=${REAL_HOME}/.go-code/browser-profile`,
  },
  // 用户真实浏览器会话 MCP ——必须零命中
  {
    pid: 79116,
    command: `node ${REAL_HOME}/.go-code/plugins/browser/node_modules/chrome-devtools-mcp/build/src/index.js`,
  },
  // 用户真实桌面应用 ——必须零命中
  {
    pid: 8000,
    command: `/Applications/go-code.app/Contents/MacOS/go-code --user-data-dir=${REAL_HOME}/Library/Application Support/hai-desktop`,
  },
  // E2E 自己起的 Chrome（隔离 profile）——应当命中
  {
    pid: 9001,
    command: `/Applications/Google Chrome.app/Contents/MacOS/Google Chrome --user-data-dir=${E2E_HOME}/.go-code/browser-profile --headless`,
  },
  // E2E 自己起的 MCP（隔离 HOME）——应当命中
  {
    pid: 9002,
    command: `node ${E2E_HOME}/.go-code/plugins/browser/node_modules/chrome-devtools-mcp/build/src/index.js`,
  },
]

/** 用假进程表测匹配逻辑（不触碰真实 kill）。 */
function pick(markers, procs = FAKE_PROCS) {
  return procs.filter((p) => markers.some((m) => p.command.includes(m)))
}

test('清理器只命中带 E2E 隔离标记的进程（用户真实进程零命中）', () => {
  const hits = pick([E2E_HOME, E2E_WS])
  const pids = hits.map((h) => h.pid).sort()
  assert.deepEqual(pids, [9001, 9002], `应只命中 E2E 自己的进程，实际=${pids}`)
})

test('旧版泛匹配（chrome-devtools-mcp / browser-profile）确认会误伤 — 回归证据', () => {
  // 这两条断言记录“为什么必须改”：泛字符串确实同时命中用户真实进程。
  assert.equal(pick(['chrome-devtools-mcp']).some((p) => p.pid === 79116), true,
    '旧写法 -f chrome-devtools-mcp 会命中用户真实 MCP')
  assert.equal(pick(['browser-profile']).some((p) => p.pid === 1700), true,
    '旧写法 -f browser-profile 会命中用户真实受管 Chrome')
})

test('createCleaner 拒绝过于宽泛的标记（防退化回泛匹配）', () => {
  assert.throws(() => createCleaner({ markers: [] }), /不能为空/)
  assert.throws(() => createCleaner({ markers: ['chrome'] }), /过于宽泛/)
  assert.throws(() => createCleaner({ markers: ['browser-profile'] }), /过于宽泛/)
  // 合法：具体隔离路径
  assert.doesNotThrow(() => createCleaner({ markers: [E2E_HOME] }))
})

test('assertNoForeignKill 拦截指向用户真实环境的清理目标', () => {
  const realChrome = [{ pid: 1, command: `Chrome --user-data-dir=${REAL_HOME}/.go-code/browser-profile` }]
  assert.throws(() => assertNoForeignKill(realChrome, { realHome: REAL_HOME }), /安全中止/)

  const userData = [{ pid: 2, command: `go-code --user-data-dir=${REAL_HOME}/Library/Application Support/hai-desktop` }]
  assert.throws(() => assertNoForeignKill(userData, { realHome: REAL_HOME }), /安全中止/)

  // E2E 自己的隔离进程不应被拦（隔离 HOME 是 realHome 的子路径，但具体路径不同）
  const e2eProc = [{ pid: 3, command: `Chrome --user-data-dir=${E2E_HOME}/.go-code/browser-profile` }]
  assert.doesNotThrow(() => assertNoForeignKill(e2eProc, { realHome: REAL_HOME }))
})

test('真实环境探测：本机清理器不会命中用户真实 Chrome/MCP', () => {
  const cleaner = createCleaner({ markers: [E2E_HOME, E2E_WS] })
  const victims = cleaner.previewVictims()
  const cmds = victims.map((v) => v.command).join('\n')
  assert.doesNotMatch(cmds, /Application Support\/Google\/Chrome/, '不得命中用户日常 Chrome')
  assert.doesNotMatch(cmds, new RegExp(`${REAL_HOME}/\\.go-code/plugins`), '不得命中用户真实 MCP')
  assert.doesNotMatch(cmds, new RegExp(`${REAL_HOME}/\\.go-code/browser-profile`), '不得命中用户真实受管 profile')
  // 自检本身不应抛错（真实环境里这些目标都应合法）
  assert.doesNotThrow(() => assertNoForeignKill(victims, { realHome: REAL_HOME }))
})

test('listProcesses 能读到真实进程表（非空且含 ps 自身）', () => {
  const procs = listProcesses()
  assert.ok(procs.length > 0, '进程表不应为空')
  assert.ok(procs.every((p) => Number.isInteger(p.pid) && typeof p.command === 'string'))
})
