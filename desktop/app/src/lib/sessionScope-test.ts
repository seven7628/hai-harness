// sessionScope-test.ts —— 「当前看谁 → 该显示谁的 HITL」判定的单测（node:test，无 DOM）。
//
// 为什么钉这个（2026-10 真机报障）：我第一版把「主会话」定义成 `runId 为空`，
// 于是主 run 的 ask_user / 审批**全部被自己过滤掉** —— 面板看起来"没弹出来"。
// 而真实事件里主 run 是有 runId 的：
//   · tools/question/question.go: RunId = tc.RunId（当前正在执行的运行）
//   · tools/engine.go:            ToolApprovalRequested.RunId = tc.RunId
// 「是不是子 agent」只能由**这张卡片/runNode 有没有父**来判（isSubAgentRun），
// 不能由「run_id 有没有值」来判。这条错误判据看起来非常自然，所以必须钉住。
//
// 三条硬约束（每一条都对应一种真实故障）：
//   1. 看主会话时，**带 runId 的主 run** 审批/提问必须显示（否则面板整体消失）；
//   2. 看主会话时，子 agent 的审批/提问必须**不**显示（否则审批归属串了）；
//   3. 看子会话时，只有那个子 agent 的显示；同级的另一个不显示。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { approvalsInScope, questionInScope, looksSubRun } from './sessionScope'

// 最小 view 形状：只需 blocks/runNodes（isSubAgentRun 的两处判据）
const V = (blocks: any[] = [], runNodes: any[] = []) => ({ blocks, runNodes }) as any
const agentCard = (runId: string) => ({ kind: 'agent', id: 1, runId, label: runId, items: [] })
const subNode = (id: string, parentId: string) => ({ id, kind: 'run', parentId })
const mainNode = (id: string) => ({ id, kind: 'run' })

const q = (runId?: string, id = 'q1') => ({ id, runId, timeoutMs: 1000, ts: 0, items: [] }) as any
const ap = (id: string, runId?: string) => ({ id, name: 'write_file', arguments: '{}', index: 0, runId, arrivedAt: 0 }) as any

test('约束 1：看主会话 → 带 runId 的主 run 提问必须显示', () => {
  const v = V()
  // 主 run 有 runId，但不是子 agent
  assert.equal(looksSubRun(v, 'main-1'), false, '主 run 不带父 → 不是子 agent')
  assert.ok(questionInScope(v, [q('main-1')], undefined), '主会话应当看到自己的提问（BUG：此前被过滤成 undefined）')
  assert.equal(approvalsInScope(v, [ap('a1', 'main-1')], undefined).length, 1, '主会话应当看到自己的审批')
})

test('约束 1b：完全没有 runId 的旧事件（历史回放）也归主会话', () => {
  const v = V()
  assert.ok(questionInScope(v, [q(undefined)], undefined), '无 run_id 的提问归主会话')
  assert.equal(approvalsInScope(v, [ap('a1', undefined)], undefined).length, 1, '无 run_id 的审批归主会话')
})

test('约束 2：看主会话 → 子 agent 的提问/审批不显示', () => {
  const v = V([agentCard('sub-1')], [subNode('sub-1', 'main-1')])
  assert.equal(looksSubRun(v, 'sub-1'), true)
  assert.equal(questionInScope(v, [q('sub-1')], undefined), undefined, '子 agent 的提问不该挡住主会话')
  assert.equal(approvalsInScope(v, [ap('a1', 'sub-1')], undefined).length, 0, '子 agent 的审批不该出现在主会话')
})

test('约束 2b：判定兜底 —— 卡片还没建、只有 runNode 的父子关系时也算子 agent', () => {
  const v = V([], [subNode('sub-x', 'main-1')])
  assert.equal(looksSubRun(v, 'sub-x'), true, '事件重排/旧日志下卡片可能晚到，runNode 自带 parentId')
  assert.equal(approvalsInScope(v, [ap('a1', 'sub-x')], undefined).length, 0)
})

test('约束 3：看子会话 → 只有它自己的，同级的另一个不显示', () => {
  const v = V([agentCard('sub-1'), agentCard('sub-2')], [subNode('sub-1', 'main-1'), subNode('sub-2', 'main-1')])
  const list = [ap('a-main', 'main-1'), ap('a-1', 'sub-1'), ap('a-2', 'sub-2')]
  assert.deepEqual(approvalsInScope(v, list, 'sub-1').map((a) => a.id), ['a-1'], '看 sub-1 只看到它自己的')
  assert.ok(questionInScope(v, [q('sub-1')], 'sub-1'), '看 sub-1 看到它自己的提问')
  assert.equal(questionInScope(v, [q('sub-2')], 'sub-1'), undefined, '同级 sub-2 的提问不显示')
  assert.equal(questionInScope(v, [q('main-1')], 'sub-1'), undefined, '主会话的提问不显示在子会话里')
})

test('约束 3b：看子会话时主 run 的审批也不显示（不越界）', () => {
  const v = V([agentCard('sub-1')], [subNode('sub-1', 'main-1')])
  assert.equal(approvalsInScope(v, [ap('a-main', 'main-1')], 'sub-1').length, 0)
})

test('约束 4：多批提问并存 —— 子 agent 提问不覆盖主会话正在等的那批', () => {
  // 2026-10 真机验证发现的第二个缺陷：store 用单槽存 question 时，后到的直接覆盖先到的。
  // 表现是「主会话正在等答的那批凭空消失」，而两边的"看得见"完全由到达顺序决定。
  const v = V([agentCard('sub-1')], [subNode('sub-1', 'main-1')])
  const list = [q('main-1', 'q-main'), q('sub-1', 'q-sub')] // 主先、子后
  assert.equal(questionInScope(v, list, undefined)?.id, 'q-main', '看主会话 → 拿到主 run 的那批（没被子 agent 顶掉）')
  assert.equal(questionInScope(v, list, 'sub-1')?.id, 'q-sub', '看子会话 → 拿到它自己的那批')
  // 反过来也成立（子先、主后）：判定只看归属，不看到达顺序
  const rev = [q('sub-1', 'q-sub'), q('main-1', 'q-main')]
  assert.equal(questionInScope(v, rev, undefined)?.id, 'q-main', '到达顺序不影响归属判定')
})

test('约束 4b：同一作用域多批时取最早到达的那批（面板只显示一批，但要稳定）', () => {
  const v = V()
  const list = [q('main-1', 'q-a'), q('main-1', 'q-b')]
  assert.equal(questionInScope(v, list, undefined)?.id, 'q-a', '先到的那批先显示（不随重渲染抖动）')
})

test('审批队列顺序保持后端 FIFO（过滤不重排）', () => {
  const v = V([agentCard('s1'), agentCard('s2')], [])
  const list = [ap('a', 's2'), ap('b', 's1'), ap('c', 's2')]
  assert.deepEqual(approvalsInScope(v, list, 's2').map((a) => a.id), ['a', 'c'], '保持原相对顺序')
})
