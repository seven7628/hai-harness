// failureLine-test.ts —— 「这次调用为什么失败」那一句话的单测（node:test，无 DOM）。
//
// 现场（用户 2026-10 截图）：收缩态只剩 `✗ .`。结果文本的第一行**不一定有信息量** ——
// bash 的 `---` 分隔线、脚本信封的 `Script failed`、只有一个点的输出，都是这种。
// 下面每条断言对应一条优先级规则（见 lib/failureLine.ts）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { failureLine } from './failureLine'

test('优先取引擎写的退出码标记（最短最准）', () => {
  const result = '---\n/Users/admin/Desktop/agent-server-g\n[exit: exit status 1]'
  assert.equal(failureLine(result), 'exit status 1')
})

test('分隔线与信封套话不算原因：跳到第一条真有内容的行', () => {
  assert.equal(failureLine('---\n---\nls: cannot access: No such file'), 'ls: cannot access: No such file')
  assert.equal(failureLine('Script failed\nWall time 0.0 seconds\nOutput:\nboom'), 'boom')
})

test('只有一个点/纯符号：不编原因，返回空串（调用方不渲染）', () => {
  assert.equal(failureLine('.'), '')
  assert.equal(failureLine('---'), '')
  assert.equal(failureLine(''), '')
  assert.equal(failureLine(undefined), '')
  assert.equal(failureLine('• • •'), '')
})

test('结果里没有可读内容时退到结构化错误码', () => {
  assert.equal(failureLine('.', { code: 'tool_timeout' }), 'tool_timeout')
  assert.equal(failureLine('', { nextAction: 'retry_with_smaller_range' }), 'retry_with_smaller_range')
})

test('长行截断保留开头（开头才是有辨识度的部分）', () => {
  const out = failureLine('x'.repeat(300))
  assert.equal(out.length, 120)
  assert.ok(out.endsWith('…'))
})

test('列表符号被剥掉，正文原样保留', () => {
  assert.equal(failureLine('- permission denied'), 'permission denied')
  assert.equal(failureLine('*   no such file or directory'), 'no such file or directory')
})
