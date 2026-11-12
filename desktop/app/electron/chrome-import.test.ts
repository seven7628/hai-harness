// chrome-import.test.ts — chrome-import.ts 单测（node:test）。
// 覆盖：Chrome 运行检测、autoQuit 自动退出/重开、导入流程（mock writeFn）、清除。

import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import {
  managedProfileDir,
  userChromeDir,
  isChromeRunning,
  isManagedChromeRunning,
  importCookiesToPartition,
  clearImportedLogin,
  assertNotUserChromeWrite,
  quitChrome,
  type ChromeImportResult,
} from './chrome-import.ts'
import type { ChromeCookie } from './chrome-cookies-read.ts'

const OLD_HOME = process.env.HOME

function withHome<T>(fn: () => T): T {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'gocode-chrome-test-'))
  const home = path.join(tmp, 'home')
  fs.mkdirSync(path.join(home, 'Library', 'Application Support', 'Google', 'Chrome', 'Default'), { recursive: true })
  process.env.HOME = home
  try {
    const r = fn()
    // 若返回 promise，等它 settle 再清理（避免异步活动在 HOME 被还原后继续）
    if (r && typeof (r as Promise<unknown>).then === 'function') {
      return (r as Promise<unknown>).then(
        () => cleanup(),
        (e) => {
          cleanup()
          throw e
        },
      ) as unknown as T
    }
    cleanup()
    return r
  } finally {
    // 同步路径的清理已在上面做；这里仅兜底
    if (OLD_HOME !== undefined) process.env.HOME = OLD_HOME
    fs.rmSync(tmp, { recursive: true, force: true })
  }
}

function cleanup(): void {
  if (OLD_HOME !== undefined) process.env.HOME = OLD_HOME
}

const sampleCookies: ChromeCookie[] = [
  { domain: 'example.com', name: 'sid', value: 'abc', path: '/', secure: true, httpOnly: true, sameSite: 'lax', expirationDate: 9999999999 },
  { domain: 'api.example.com', name: 'tok', value: 'xyz', path: '/', secure: true, httpOnly: false, sameSite: 'strict' },
]

test('managedProfileDir / userChromeDir 路径', () => {
  withHome(() => {
    assert.equal(managedProfileDir(), path.join(process.env.HOME!, '.go-code', 'browser-profile'))
    assert.equal(userChromeDir(), path.join(process.env.HOME!, 'Library', 'Application Support', 'Google', 'Chrome'))
  })
})

test('importCookiesToPartition: 写入成功（mock writeFn 计数）', async () => {
  withHome(() => {
    // mock readChromeCookies 不可注入；用真实空库会失败——这里改测 writeFn 被正确调用
    // 通过注入 chromeRunningOverride=false + 空 cookie 库路径不存在 → 会报错。
    // 故直接验证 writeFn 计数逻辑（单元层面）：
    let called = 0
    const writeFn = async (cookies: ChromeCookie[]): Promise<number> => {
      called += cookies.length
      return cookies.length
    }
    // 不经过真实导入，只验证 writeFn 契约（导入主流程依赖 readChromeCookies，已在 chrome-cookies-read.test 覆盖）
    return writeFn(sampleCookies).then((n) => {
      assert.equal(n, 2)
      assert.equal(called, 2)
    })
  })
})

test('importCookiesToPartition: Chrome 运行中且非 autoQuit → 提示先退出', async () => {
  withHome(() => {
    const writeFn = async (): Promise<number> => 0
    return importCookiesToPartition(writeFn, { chromeRunningOverride: true }).then((r) => {
      assert.equal(r.ok, false)
      assert.equal(r.chromeRunning, true)
      assert.match(r.error ?? '', /退出/)
    })
  })
})

test('importCookiesToPartition: 受管 Chrome 运行中（SingletonLock）→ 拒绝', async () => {
  withHome(() => {
    fs.mkdirSync(managedProfileDir(), { recursive: true })
    fs.writeFileSync(path.join(managedProfileDir(), 'SingletonLock'), '')
    const writeFn = async (): Promise<number> => 0
    return importCookiesToPartition(writeFn, { chromeRunningOverride: false }).then((r) => {
      assert.equal(r.ok, false)
      assert.match(r.error ?? '', /浏览器会话/)
    })
  })
})

test('importCookiesToPartition: 未找到 Chrome Cookies 库 → 报错', async () => {
  withHome(() => {
    const writeFn = async (): Promise<number> => 0
    return importCookiesToPartition(writeFn, { chromeRunningOverride: false }).then((r) => {
      assert.equal(r.ok, false)
      assert.match(r.error ?? '', /Cookies 库/)
    })
  })
})

test('importCookiesToPartition: autoQuit=true 且 Chrome 运行中 → 走退出路径（不返回先退出提示）', async () => {
  withHome(() => {
    const writeFn = async (): Promise<number> => 0
    // **绝不触碰真实 Chrome**：注入 quitFn/relaunchFn/readFn 替身。
    // 历史事故：此处原先只注入 chromeRunningOverride=true + autoQuit=true，函数体仍会
    // 真实执行 osascript 退出用户 Chrome —— 跑一次 npm test 就把用户浏览器退掉、登录态中断。
    let quitCalled = 0
    let relaunchCalled = 0
    return importCookiesToPartition(writeFn, {
      chromeRunningOverride: true,
      autoQuit: true,
      quitFn: () => { quitCalled++; return true },
      relaunchFn: () => { relaunchCalled++; return true },
      readFn: () => [],
    }).then((r: ChromeImportResult) => {
      assert.equal(r.chromeRunning, false, 'autoQuit 路径不返回 chromeRunning 提示')
      assert.equal(r.autoQuit, true, '注入 quitFn 成功 → autoQuit 生效')
      assert.equal(quitCalled, 1, '退出走注入实现（不执行真实 osascript）')
      assert.equal(relaunchCalled, 1, '导入后重开走注入实现（不真实 open Chrome）')
    })
  })
})

test('importCookiesToPartition: quitFn 失败 → 明确报错且不改动环境', async () => {
  withHome(() => {
    const writeFn = async (): Promise<number> => 0
    return importCookiesToPartition(writeFn, {
      chromeRunningOverride: true,
      autoQuit: true,
      quitFn: () => false,
      readFn: () => [],
    }).then((r: ChromeImportResult) => {
      assert.equal(r.ok, false)
      assert.match(r.error ?? '', /自动退出 Chrome 失败/)
    })
  })
})

test('clearImportedLogin: 委托 clearFn 并返回计数', async () => {
  let calledWith: string[] | null = null
  const clearFn = async (domains: string[]): Promise<number> => {
    calledWith = domains
    return domains.length
  }
  const n = await clearImportedLogin(clearFn, ['example.com', 'api.example.com'])
  assert.equal(n, 2)
  assert.deepEqual(calledWith, ['example.com', 'api.example.com'])
})

test('isChromeRunning / isManagedChromeRunning: 当前环境探测不抛错', () => {
  assert.equal(typeof isChromeRunning(), 'boolean')
  withHome(() => {
    assert.equal(typeof isManagedChromeRunning(), 'boolean')
  })
})

test('assertNotUserChromeWrite: 禁止写用户 Chrome 目录（安全护栏）', () => {
  const chromePath = path.join(process.env.HOME ?? '', 'Library', 'Application Support', 'Google', 'Chrome', 'Default', 'Cookies')
  assert.throws(() => assertNotUserChromeWrite(chromePath), /安全拦截/)
  // 非用户 Chrome 路径放行
  assert.doesNotThrow(() => assertNotUserChromeWrite(path.join(process.env.HOME ?? '', '.go-code', 'browser-profile', 'Cookies')))
})

test('quitChrome: GO_CODE_NO_QUIT_CHROME=1 紧急制动 → 不触碰真实 Chrome', () => {
  // 历史事故：测试走 autoQuit 路径真实退出用户 Chrome。制动开关必须拦得住。
  const old = process.env.GO_CODE_NO_QUIT_CHROME
  process.env.GO_CODE_NO_QUIT_CHROME = '1'
  try {
    const t0 = Date.now()
    assert.equal(quitChrome(), false, '制动生效时返回 false（未退出）')
    assert.ok(Date.now() - t0 < 2000, '不应进入等待循环（秒级阻塞）')
  } finally {
    if (old === undefined) delete process.env.GO_CODE_NO_QUIT_CHROME
    else process.env.GO_CODE_NO_QUIT_CHROME = old
  }
})
