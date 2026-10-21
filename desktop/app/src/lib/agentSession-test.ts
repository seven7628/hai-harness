// agentSession-test.ts —— 子智能体转录 → 主会话会话块的映射单测（node:test，无 DOM）。
//
// 为什么钉这个：这个映射是「子会话 = 主会话页」的**唯一**适配点。它错了不会崩，只会
// 「看着怪」——思考块丢了、两条记录挤成一条、React key 撞了导致展开一个工具另一个跟着
// 展开。这些都不是类型能挡住的，只能靠断言。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { agentItemsToBlocks } from './agentSession'
import type { AgentItem } from '../store/useAppStore'

const tool = (over: Partial<Extract<AgentItem, { kind: 'tool' }>> = {}): AgentItem =>
  ({ kind: 'tool', toolId: 't1', name: 'bash', status: 'done', ...over })

test('thinking + 紧随的 text → 合并成一个 assistant（主会话里思考就是 assistant 的字段）', () => {
  const out = agentItemsToBlocks([
    { kind: 'thinking', text: '先看看仓库结构' },
    { kind: 'text', text: '看完了，接下来改 auth' },
  ])
  assert.equal(out.length, 1, '不是两块 —— 分成两块会多出一行「谁在说话」的作者行')
  assert.equal(out[0].kind, 'assistant')
  assert.equal((out[0] as any).thinking, '先看看仓库结构')
  assert.equal((out[0] as any).text, '看完了，接下来改 auth')
})

test('单独的 thinking（后面是工具）→ 只有 thinking 的 assistant 块', () => {
  const out = agentItemsToBlocks([
    { kind: 'thinking', text: '要跑测试' },
    tool(),
  ])
  assert.equal(out.length, 2)
  assert.equal(out[0].kind, 'assistant')
  assert.equal((out[0] as any).text, '')
  assert.equal((out[0] as any).thinking, '要跑测试')
  assert.equal(out[1].kind, 'tool')
})

test('两段「思考+正文」保持时间序，不会被合并成一段', () => {
  const out = agentItemsToBlocks([
    { kind: 'thinking', text: '想 A' }, { kind: 'text', text: '说 A' },
    tool(),
    { kind: 'thinking', text: '想 B' }, { kind: 'text', text: '说 B' },
  ])
  assert.deepEqual(out.map((b) => b.kind), ['assistant', 'tool', 'assistant'])
  assert.equal((out[0] as any).thinking, '想 A')
  assert.equal((out[2] as any).thinking, '想 B')
})

test('单独 text → assistant 块', () => {
  const out = agentItemsToBlocks([{ kind: 'text', text: '纯正文' }])
  assert.equal(out.length, 1)
  assert.equal(out[0].kind, 'assistant')
  assert.equal((out[0] as any).text, '纯正文')
  assert.equal((out[0] as any).thinking, '')
})

test('失败提示（error=true）→ system + tone:error，不能悄悄变成普通正文', () => {
  // assistant 没有错误位；映射成普通段落的后果是「这轮失败了」被显示成正常输出。
  const out = agentItemsToBlocks([{ kind: 'text', text: 'LLM 调用失败：超时', error: true }])
  assert.equal(out[0].kind, 'system')
  assert.equal((out[0] as any).tone, 'error')
  assert.equal((out[0] as any).text, 'LLM 调用失败：超时')
})

test('tool → tool 块：三态映射到主会话的两态 + isError', () => {
  const out = agentItemsToBlocks([
    tool({ toolId: 'a', status: 'running' }),
    tool({ toolId: 'b', status: 'done' }),
    tool({ toolId: 'c', status: 'error', result: 'boom' }),
  ])
  assert.deepEqual(out.map((b) => (b as any).status), ['running', 'done', 'done'],
    'tool 块只有 running/done；错误态由 isError 表达（不是第三种 status）')
  assert.deepEqual(out.map((b) => (b as any).isError), [false, false, true])
})

test('tool 的可选字段原样透传（缺失的不写成 undefined 键）', () => {
  const out = agentItemsToBlocks([tool({
    args: '{"command":"ls"}', result: 'ok', durMs: 12, startedAt: 1000, taskId: 'task-1',
    diff: { path: 'a.go', added: 1, removed: 0 } as any,
  })])
  const t = out[0] as any
  assert.equal(t.args, '{"command":"ls"}')
  assert.equal(t.result, 'ok')
  assert.equal(t.durMs, 12)
  assert.equal(t.startedAt, 1000)
  assert.equal(t.taskId, 'task-1')
  assert.ok(t.diff)
  const bare = agentItemsToBlocks([tool()])[0] as any
  assert.ok(!('args' in bare), '没有 args 就不该出现这个键（否则渲染层会渲染空参数块）')
  assert.ok(!('diff' in bare))
})

test('compression → compression 块（子 agent 自己的上下文压缩）', () => {
  const out = agentItemsToBlocks([{ kind: 'compression', before: 100, after: 20, reason: 'auto', durMs: 900 }])
  assert.equal(out[0].kind, 'compression')
  const c = out[0] as any
  assert.equal(c.before, 100)
  assert.equal(c.after, 20)
  assert.equal(c.reason, 'auto')
  assert.equal(c.durMs, 900)
})

test('id 全部为负且互不相同（合成块不能与会话里真实的块撞 key）', () => {
  const out = agentItemsToBlocks([
    { kind: 'text', text: 'a' }, tool(), tool(), { kind: 'thinking', text: 't' },
    { kind: 'compression', before: 1, after: 0 },
  ])
  const ids = out.map((b) => (b as any).id as number)
  assert.equal(new Set(ids).size, ids.length, 'id 唯一（撞了会让 React 复用错节点）')
  assert.ok(ids.every((i) => i < 0), '一律负数：别与会话 blockSeq 的正数 id 撞上')
})

test('空列表 → 空数组（子会话无转录时交给 Narrative 的空态）', () => {
  assert.deepEqual(agentItemsToBlocks([]), [])
})
