import { test } from 'node:test'
import assert from 'node:assert/strict'
import { batchRunningLine, batchStatus, failedCallLine } from './batch'

const call = (o: Partial<{ status: 'running' | 'done'; isError: boolean; durMs: number; name: string; result: string }> = {}) => ({
  kind: 'tool' as const, id: 1, toolId: 't', name: o.name ?? 'read_file', status: o.status ?? 'done',
  isError: o.isError, durMs: o.durMs, result: o.result ?? '',
})

test('计数按真实状态：完成 / 运行中 / 失败 / 耗时（只加已结算的）', () => {
  const s = batchStatus([
    call({ status: 'done', durMs: 1200 }),
    call({ status: 'running' }),
    call({ status: 'done', isError: true, durMs: 300 }),
  ] as never)
  // done 只数成功的：失败是单独一档（「1 completed · 1 failed」比「2 completed」诚实）
  assert.deepEqual(s, { total: 3, done: 1, running: 1, failed: 1, durMs: 1500, settled: false })
})

test('全部结束才算 settled（自动收缩的门）', () => {
  assert.equal(batchStatus([call({ status: 'done', durMs: 10 })] as never).settled, true)
  assert.equal(batchStatus([call({ status: 'running' })] as never).settled, false)
  assert.equal(batchStatus([] as never).settled, true)
})

test('运行中的组头：总数 + 完成 + 运行中 + 失败 + 耗时', () => {
  const calls = [
    call({ status: 'done', durMs: 12000 }),
    call({ status: 'running' }),
    call({ status: 'running' }),
  ] as never
  const line = batchRunningLine(calls)
  assert.match(line, /^3 tool calls/)
  assert.match(line, /1 completed/)
  assert.match(line, /2 running/)
  assert.match(line, /12 s|12s/)
})

test('单数说 tool call，不留个复数', () => {
  assert.match(batchRunningLine([call({ status: 'running' })] as never), /^1 tool call ·/)
})

test('失败行摘要：名字 + 结果首行，超长截断', () => {
  assert.equal(failedCallLine(call({ name: 'bash', result: '\n\nboom: exit 1\nmore' }) as never), 'bash · boom: exit 1')
  const long = failedCallLine(call({ name: 'bash', result: 'x'.repeat(400) }) as never)
  assert.ok(long.length <= 130 && long.endsWith('…'))
})
