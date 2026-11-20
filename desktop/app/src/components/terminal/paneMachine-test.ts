// [recovered] from /private/tmp/term-pane.test.cjs（模块标记 `// src/components/terminal/paneMachine-test.ts`）
// [implemented] 依据 docs/TERMINAL_E2E_CONTRACTS.md「paneMachine 13 条状态转移」：
//   切线把 paneMachine.ts 内联进来了（并且是同一次构建里**带 initialPaneState** 的那份，
//   正是本模块恢复该导出的依据）。本文件改为 `import` 真模块，断言逐条保留（不删不改）。
import test from 'node:test'
import assert from 'node:assert/strict'

import { initialPaneState, next, type PaneEvent, type PaneState } from './paneMachine'

const idle: PaneState = { s: 'idle' }
const creating: PaneState = { s: 'creating' }
const connecting: PaneState = { s: 'connecting', ptyId: 'p1' }
const replaying: PaneState = { s: 'replaying', ptyId: 'p1' }
const live: PaneState = { s: 'live', ptyId: 'p1' }
const exited: PaneState = { s: 'exited', ptyId: 'p1', code: 0 }
const failed: PaneState = { s: 'failed', error: 'boom' }

test('initialPaneState 是 idle：挂载即可渲染，不会凭空建 PTY', () => {
  assert.deepEqual(initialPaneState, { s: 'idle' })
  assert.deepEqual(next(initialPaneState, { t: 'open' }).fx, [{ k: 'create-pty' }])
})

test('idle + open → creating，并请求建 PTY', () => {
  const r = next(idle, { t: 'open' })
  assert.deepEqual(r.state, { s: 'creating' })
  assert.deepEqual(r.fx, [{ k: 'create-pty' }])
})

test('creating + created → connecting（带 ptyId），并请求票据', () => {
  const r = next(creating, { t: 'created', ptyId: 'p1' })
  assert.deepEqual(r.state, { s: 'connecting', ptyId: 'p1' })
  assert.deepEqual(r.fx, [{ k: 'issue-ticket' }])
})

test('creating + timeout(create) → failed，只 toast（还没有 socket 可关）', () => {
  const r = next(creating, { t: 'timeout', phase: 'create' })
  assert.equal(r.state.s, 'failed')
  assert.deepEqual(r.fx, [{ k: 'toast', error: '创建终端超时' }])
})

test('connecting + ticket → connecting 带上票据，并开 socket', () => {
  const r = next(connecting, { t: 'ticket', ticket: 'tk' })
  assert.deepEqual(r.state, { s: 'connecting', ptyId: 'p1', ticket: 'tk' })
  assert.deepEqual(r.fx, [{ k: 'open-socket' }])
})

test('connecting + ws-open / hello → 状态与 fx 都不动', () => {
  for (const ev of [{ t: 'ws-open' }, { t: 'hello' }] as PaneEvent[]) {
    const r = next(connecting, ev)
    assert.equal(r.state, connecting, `握手事件不该换状态：${ev.t}`)
    assert.deepEqual(r.fx, [])
  }
})

test('connecting + reset → replaying，并重置终端', () => {
  const r = next({ s: 'connecting', ptyId: 'p1', ticket: 'tk' }, { t: 'reset' })
  assert.deepEqual(r.state, { s: 'replaying', ptyId: 'p1' })
  assert.deepEqual(r.fx, [{ k: 'reset-term' }])
})

test('replaying + output → live，且 show 只出现一次', () => {
  const r = next(replaying, { t: 'output' })
  assert.deepEqual(r.state, { s: 'live', ptyId: 'p1' })
  assert.deepEqual(r.fx, [{ k: 'show' }])
  const again = next(r.state, { t: 'output' })
  assert.equal(again.state, r.state)
  assert.deepEqual(again.fx, [])
})

test('connecting + timeout(connect) → failed，关 socket 并 toast', () => {
  const r = next(connecting, { t: 'timeout', phase: 'connect' })
  assert.equal(r.state.s, 'failed')
  assert.deepEqual(r.fx, [{ k: 'close-socket' }, { k: 'toast', error: '连接终端超时' }])
})

test('任意状态 + error → failed，关 socket 并把原文递给 toast', () => {
  for (const s of [idle, creating, connecting, replaying, live, exited, failed]) {
    const r = next(s, { t: 'error', error: 'ws 断了' })
    assert.deepEqual(r.state, { s: 'failed', error: 'ws 断了' }, `从 ${s.s} 进来也要落到 failed`)
    assert.deepEqual(r.fx, [{ k: 'close-socket' }, { k: 'toast', error: 'ws 断了' }])
  }
})

test('live + exit → exited（带退出码），并关 socket', () => {
  assert.deepEqual(next(live, { t: 'exit', code: 7 }), {
    state: { s: 'exited', ptyId: 'p1', code: 7 },
    fx: [{ k: 'close-socket' }],
  })
  assert.deepEqual(next(live, { t: 'exit' }), {
    state: { s: 'exited', ptyId: 'p1', code: undefined },
    fx: [{ k: 'close-socket' }],
  })
})

test('close：有 ptyId 时连 PTY 一起收', () => {
  for (const s of [connecting, replaying, live, exited]) {
    assert.deepEqual(
      next(s, { t: 'close' }),
      { state: { s: 'idle' }, fx: [{ k: 'close-socket' }, { k: 'close-pty' }] },
      `从 ${s.s} 关标签要连进程一起收`,
    )
  }
})

test('close：没有 ptyId 时只关 socket', () => {
  for (const s of [idle, creating, failed]) {
    assert.deepEqual(next(s, { t: 'close' }), { state: { s: 'idle' }, fx: [{ k: 'close-socket' }] })
  }
})

test('未列出的组合原样返回，fx 为空', () => {
  const cases: [PaneState, PaneEvent][] = [
    [idle, { t: 'output' }],
    [idle, { t: 'ticket', ticket: 'tk' }],
    [idle, { t: 'exit', code: 0 }],
    [creating, { t: 'open' }],
    [creating, { t: 'reset' }],
    [connecting, { t: 'output' }],
    [connecting, { t: 'exit', code: 0 }],
    [live, { t: 'reset' }],
    [live, { t: 'open' }],
    [live, { t: 'created', ptyId: 'p2' }],
    [replaying, { t: 'hello' }],
    [exited, { t: 'output' }],
    [failed, { t: 'ws-open' }],
  ]
  for (const [s, ev] of cases) {
    const r = next(s, ev)
    assert.equal(r.state, s, `${s.s} + ${ev.t} 应当原样返回同一个对象`)
    assert.deepEqual(r.fx, [])
  }
})

test('超时落在错误的阶段时不算数（迟到的 timeout 不该毁掉一个已连上的终端）', () => {
  assert.deepEqual(next(live, { t: 'timeout', phase: 'connect' }), { state: live, fx: [] })
  assert.deepEqual(next(connecting, { t: 'timeout', phase: 'create' }), { state: connecting, fx: [] })
})

test('纯函数：任何转移都不改入参', () => {
  const snapshot = structuredClone(connecting)
  next(connecting, { t: 'ticket', ticket: 'tk' })
  next(connecting, { t: 'error', error: 'x' })
  next(connecting, { t: 'close' })
  assert.deepEqual(connecting, snapshot)
})

test('replaying + exit → exited（建连时才发现会话已退出）', () => {
  const r = next(replaying, { t: 'exit', code: 3 })
  assert.deepEqual(r.state, { s: 'exited', ptyId: 'p1', code: 3 })
  assert.deepEqual(r.fx, [{ k: 'close-socket' }])
})
