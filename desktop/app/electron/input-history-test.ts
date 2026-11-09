// input-history.test.ts —— 本地文件持久化的单测（node:test，不启动 Electron）。
//
// 钉住：
//   ① 跨实例读回（= 重启后仍在）、原子写不留 temp；
//   ② 每 Session 100 条、单条超限拒收、作用域 LRU；
//   ③ 删会话只清该会话（含子智能体），删工作区清整棵；
//   ④ 坏文件不炸主进程，下一次写会覆盖成合法文件。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  INPUT_HISTORY_ENTRY_CHARS,
  INPUT_HISTORY_MAX,
  emptyInputHistoryFile,
  forgetInputHistory,
  getInputHistory,
  inputHistoryFilePath,
  ipcInputHistoryForget,
  ipcInputHistoryGet,
  ipcInputHistorySet,
  parseInputHistoryFile,
  readInputHistoryFile,
  setInputHistory,
  writeInputHistoryFile,
} from './input-history'

function tempDir(): string {
  return mkdtempSync(join(tmpdir(), 'hai-input-history-'))
}

const textItem = (text: string, at?: number) => ({ text, ...(at != null ? { at } : {}) })

test('文件往返：跨实例读回、原子写不留 temp', () => {
  const dir = tempDir()
  try {
    const file = inputHistoryFilePath(dir)
    const next = setInputHistory(emptyInputHistoryFile(), 'ws\u0000s1', [
      { text: '第一条', at: 1000 },
      { text: '第二条', at: 1001, refs: [{ path: '/w/a.md', name: 'a.md', size: 12 }] },
    ])
    writeInputHistoryFile(file, next)
    assert.deepEqual(readInputHistoryFile(file), next, '读回与写入一致')
    assert.equal(readdirSync(dir).filter((n) => n.endsWith('.tmp')).length, 0, '没有残留 temp')
    const raw = readFileSync(file, 'utf8')
    assert.ok(raw.includes('第一条'))
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('容量：每会话最多 100 条；同文去重保留最长列表尾部', () => {
  const dir = tempDir()
  try {
    const file = inputHistoryFilePath(dir)
    const items = Array.from({ length: INPUT_HISTORY_MAX + 5 }, (_, i) => textItem(`msg-${i + 1}`, i))
    const next = setInputHistory(emptyInputHistoryFile(), 's', items)
    const got = getInputHistory(next, 's')
    assert.equal(got.length, INPUT_HISTORY_MAX)
    assert.equal(got[0].text, 'msg-6', '超限丢最旧的')
    assert.equal(got.at(-1)?.text, `msg-${INPUT_HISTORY_MAX + 5}`)
    writeInputHistoryFile(file, next)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('超大条目：拒收，不驱逐已有条目', () => {
  const huge = 'x'.repeat(INPUT_HISTORY_ENTRY_CHARS + 1)
  const next = setInputHistory(emptyInputHistoryFile(), 's', [textItem('保留', 1), textItem(huge, 2)])
  assert.deepEqual(getInputHistory(next, 's'), [{ text: '保留', at: 1 }])
})

test('作用域 LRU：超过 40 个按 t 淘汰最久未用', () => {
  let file = emptyInputHistoryFile()
  for (let i = 0; i < 45; i++) file = setInputHistory(file, `s${i}`, [textItem(`m${i}`, i)], i)
  assert.equal(Object.keys(file.scopes).length, 40)
  assert.equal(getInputHistory(file, 's0').length, 0, '最久未用被淘汰')
  assert.equal(getInputHistory(file, 's44').length, 1, '最新保留')
})

test('删除：会话只清自己 + 子智能体；工作区清整棵', () => {
  let file = emptyInputHistoryFile()
  file = setInputHistory(file, 'ws\u0000s1', [textItem('a')])
  file = setInputHistory(file, 'ws\u0000s1\u0000agent:t1', [textItem('a-sub')])
  file = setInputHistory(file, 'ws\u0000s2', [textItem('b')])
  file = setInputHistory(file, 'ws\u0000', [textItem('bare')])

  const afterSession = forgetInputHistory(file, 'ws\u0000s1')
  assert.equal(getInputHistory(afterSession, 'ws\u0000s1').length, 0)
  assert.equal(getInputHistory(afterSession, 'ws\u0000s1\u0000agent:t1').length, 0)
  assert.deepEqual(getInputHistory(afterSession, 'ws\u0000s2'), [{ text: 'b' }], '兄弟会话不受影响')
  assert.deepEqual(getInputHistory(afterSession, 'ws\u0000'), [{ text: 'bare' }], '工作区作用域不受影响')

  const afterWs = forgetInputHistory(file, 'ws\u0000', true)
  assert.equal(Object.keys(afterWs.scopes).length, 0, '工作区整棵清空')
})

test('坏文件：解析为空、不抛；IPC set 覆盖成合法文件', () => {
  const dir = tempDir()
  try {
    const file = inputHistoryFilePath(dir)
    writeFileSync(file, '{oops')
    assert.deepEqual(readInputHistoryFile(file), emptyInputHistoryFile())
    const r = ipcInputHistorySet(file, 's', [{ text: '恢复' }])
    assert.equal(r.ok, true)
    assert.deepEqual(r.items, [{ text: '恢复' }])
    const got = ipcInputHistoryGet(file, 's')
    assert.equal(got.ok, true)
    assert.deepEqual(got.items, [{ text: '恢复' }])
    assert.equal(ipcInputHistoryForget(file, 's').ok, true)
    assert.deepEqual(ipcInputHistoryGet(file, 's').items, [])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('解析：v1 结构与非法条目宽容处理', () => {
  const raw = JSON.stringify({
    v: 1,
    scopes: {
      s: { t: 5, items: [{ text: ' 保留 ', at: 123 }, { text: '   ' }, 42] },
      empty: { t: 1, items: [] },
      bad: 'nope',
    },
  })
  const file = parseInputHistoryFile(raw)
  assert.deepEqual(file.scopes.s.items, [{ text: '保留', at: 123 }])
  assert.equal(file.scopes.empty, undefined)
  assert.equal(file.scopes.bad, undefined)
})
