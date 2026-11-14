// viewBudget-test.ts —— 视图内存预算的单测（node:test，无 DOM、无 store 运行时）—— P1（2026-11-12）。
//
// 为什么钉这些：渲染进程 footprint 实测 2.08 GB（峰值 3.13 GB），而 V8 的堆上限
// 是**崩溃线**（到顶 → last resort GC → OOM 停止执行 ⇒ Electron 里 Aw, Snap）。
// 预算机制写错的几种表现**全是静默的**：
//   ① 降级时没置 hydrated=false → 切回会话渲染一个空壳 ⇒ 用户看到「历史凭空消失」；
//   ② 降级把 C 类（待审批的 diff / 用户没发出去的输入）一起丢了 ⇒ 丢用户数据；
//   ③ 淘汰了运行中的会话 ⇒ 它的实时 UI 直接坏掉；
//   ④ 裁块尾时游标没跟着走 ⇒ 「加载更早」再也取不回被裁掉的那段。
import test from 'node:test'
import assert from 'node:assert/strict'
import {
  VIEW_BUDGET_BYTES,
  VIEW_BUDGET_PRESSURE_BYTES,
  VIEW_KEEP_AGENT_CARDS,
  VIEW_KEEP_BLOCKS,
  currentBudgetBytes,
  degradeView,
  degradedBytes,
  memPressureActive,
  planBudget,
  roughSize,
  setMemPressure,
  viewBytes,
  viewHasPendingInteraction,
} from './viewBudget'
import type { MsgBlock, SessionView } from './useAppStore'

// —— 夹具 ——

const textBlock = (id: number, text: string): MsgBlock =>
  ({ kind: 'assistant', id, text, streaming: false, thinking: '' })
const agentCard = (id: number, label: string): MsgBlock =>
  ({ kind: 'agent', id, runId: `r${id}`, label, status: 'done', spawnedAt: 0, usage: {}, items: [] } as never)

const view = (blocks: MsgBlock[], extra: Partial<SessionView> = {}): SessionView =>
  ({
    blocks,
    agentDetached: [],
    approvals: [],
    runNodes: [],
    diffs: [],
    todos: [],
    usage: {},
    git: {},
    gitFiles: [],
    gitHistory: [],
    turns: [],
    traceEvents: [],
    streaming: false,
    runEnded: false,
    aborted: false,
    reqBlock: {},
    toolStartTs: {},
    runningAgentCount: 0,
    thinkStartTs: {},
    questions: [],
    pendingQueue: [],
    rightExpanded: false,
    rightLayout: { ratio: 0.32 },
    compressing: false,
    gitLoading: false,
    hydrated: true,
    ...extra,
  }) as unknown as SessionView

// —— 一、roughSize ——

test('roughSize：字符串按 UTF-16 算（2 字节/字符）', () => {
  assert.equal(roughSize('abcd'), 8)
  assert.equal(roughSize(''), 0)
  assert.equal(roughSize(null), 0)
  assert.equal(roughSize(undefined), 0)
  assert.equal(roughSize(42), 8)
})

test('roughSize：嵌套对象按属性累加；深度封顶不爆栈', () => {
  const a = roughSize({ x: 'ab', y: { z: 'c' } })
  const b = roughSize({ x: 'ab', y: { z: 'd' } })
  assert.equal(a, b) // 同量级
  assert.ok(a > 0)
  // 深度封顶：极深嵌套返回有限值而不是栈溢出。
  let deep: unknown = 'x'
  for (let i = 0; i < 50; i++) deep = { d: deep }
  assert.ok(Number.isFinite(roughSize(deep)))
})

// —— 二、viewBytes ——

test('viewBytes：块与窗口外卡片都计入', () => {
  const v = view([textBlock(1, 'x'.repeat(1000))], { agentDetached: [agentCard(2, 'a')] })
  const one = view([textBlock(1, 'x'.repeat(1000))])
  assert.ok(viewBytes(v) > viewBytes(one))
})

// —— 三、degradeView：分级降级 ———

test('degradeView：blocks 裁到尾部，游标跟着新的最早块走', () => {
  const blocks = Array.from({ length: VIEW_KEEP_BLOCKS + 50 }, (_, i) => textBlock(i + 1, `b${i}`))
  const v = view(blocks, { blocksCursor: 1, blocksHasMore: true })
  const d = degradeView(v)
  assert.equal(d.blocks.length, VIEW_KEEP_BLOCKS)
  // 保留的是**尾部**（最近的内容），不是头部。
  assert.equal(d.blocks[0].id, 51)
  assert.equal(d.blocks[d.blocks.length - 1].id, VIEW_KEEP_BLOCKS + 50)
  // 游标 = 新的最早块 id ⇒ 「加载更早」仍能取回被裁掉的那段。
  assert.equal(d.blocksCursor, 51)
  assert.equal(d.blocksHasMore, true)
  // 关键：不置 hydrated=false 的话，切回去会渲染一个空壳（历史凭空消失）。
  assert.equal(d.hydrated, false)
})

test('degradeView：A 类清空 / C 类保留', () => {
  const v = view([textBlock(1, 'x')], {
    traceEvents: [{ event_type: 'x' } as never],
    diffs: [{ path: 'a' } as never],
    turns: [{ n: 1 } as never],
    gitFiles: [{ path: 'b' } as never],
    gitHistory: [{ sha: 'c' } as never],
    // C 类：用户正在交互的东西
    approvals: [{ id: 'ap' } as never],
    questions: [{ id: 'q' } as never],
    pendingQueue: [{ id: 'pq' } as never],
    release: { id: 'rel' } as never,
    todos: [{ id: 'td' } as never],
    usage: { input: 10 } as never,
    costUsd: 0.5,
  })
  const d = degradeView(v)
  // A 类：可重取 → 丢掉
  assert.equal(d.traceEvents.length, 0)
  assert.equal(d.diffs.length, 0)
  assert.equal(d.gitFiles.length, 0)
  assert.equal(d.gitHistory.length, 0)
  // turns 保留（裁到尾部）：状态栏的「回合 = N」要读它，且它很小。
  assert.equal(d.turns.length, 1)
  // C 类：一个都不能丢
  assert.equal(d.approvals.length, 1)
  assert.equal(d.questions.length, 1)
  assert.equal(d.pendingQueue.length, 1)
  assert.ok(d.release)
  assert.equal(d.todos.length, 1)
  assert.equal(d.costUsd, 0.5)
})

test('degradeView：窗口外卡片裁到尾部（渲染进程内存的大头）', () => {
  const cards = Array.from({ length: VIEW_KEEP_AGENT_CARDS + 30 }, (_, i) => agentCard(i + 1, `c${i}`))
  const d = degradeView(view([], { agentDetached: cards }))
  assert.equal(d.agentDetached?.length, VIEW_KEEP_AGENT_CARDS)
  assert.equal(d.agentDetached?.[d.agentDetached.length - 1].id, VIEW_KEEP_AGENT_CARDS + 30)
})

test('degradeView：降级后确实更省', () => {
  const v = view(Array.from({ length: 1000 }, (_, i) => textBlock(i + 1, 'x'.repeat(500))))
  assert.ok(degradedBytes(v) < viewBytes(v))
})

// —— 四、C 类保护 ——

test('viewHasPendingInteraction：四类待办交互都算「不可丢」', () => {
  assert.equal(viewHasPendingInteraction(view([])), false)
  assert.equal(viewHasPendingInteraction(view([], { approvals: [{ id: 'a' } as never] })), true)
  assert.equal(viewHasPendingInteraction(view([], { questions: [{ id: 'q' } as never] })), true)
  assert.equal(viewHasPendingInteraction(view([], { pendingQueue: [{ id: 'p' } as never] })), true)
  assert.equal(viewHasPendingInteraction(view([], { release: { id: 'r' } as never })), true)
})

// —— 五、planBudget：分级处置 ——

test('planBudget：没超预算 → 什么都不做', () => {
  const views = { a: view([textBlock(1, 'x')]) }
  const plan = planBudget({ views, order: ['a'], budget: VIEW_BUDGET_BYTES })
  assert.deepEqual(plan.degrade, [])
  assert.deepEqual(plan.evict, [])
})

test('planBudget：超预算 → 先降级 LRU 最久没看的', () => {
  const big = view(Array.from({ length: 500 }, (_, i) => textBlock(i + 1, 'x'.repeat(2000))))
  const views = { a: big, b: big }
  const plan = planBudget({ views, order: ['a', 'b'], budget: 1 })
  assert.ok(plan.degrade.length > 0)
  // LRU 顺序：order 里靠前 = 最久没看 → 先降级它。
  assert.equal(plan.degrade[0], 'a')
})

test('planBudget：运行中的会话不可淘汰（protect 回调）', () => {
  const big = view(Array.from({ length: 500 }, (_, i) => textBlock(i + 1, 'x'.repeat(4000))))
  const views = { busy: big, idle: big }
  const plan = planBudget({
    views,
    order: ['busy', 'idle'],
    budget: 1,
    protect: (sid) => sid === 'busy',
  })
  assert.ok(!plan.evict.includes('busy'))
})

test('planBudget：有待办交互的会话既不降级也不淘汰', () => {
  const big = view(Array.from({ length: 500 }, (_, i) => textBlock(i + 1, 'x'.repeat(4000))))
  const views = {
    pending: view(Array.from({ length: 500 }, (_, i) => textBlock(i + 1, 'x'.repeat(4000))), {
      approvals: [{ id: 'ap' } as never],
    }),
    other: big,
  }
  const plan = planBudget({ views, order: ['pending', 'other'], budget: 1 })
  assert.ok(!plan.degrade.includes('pending'))
  assert.ok(!plan.evict.includes('pending'))
})

test('planBudget：active 受保护（不被淘汰）', () => {
  const big = view(Array.from({ length: 500 }, (_, i) => textBlock(i + 1, 'x'.repeat(4000))))
  const views = { act: big, other: big }
  const plan = planBudget({ views, order: ['act', 'other'], active: 'act', budget: 1 })
  assert.ok(!plan.evict.includes('act'))
})

// —— 六、内存压力信号 ——

test('memPressureActive：swap 用掉一半以上**且**物理内存空闲不足 15% 才算颠簸', () => {
  // swap 只用了 40% → 不算颠簸（哪怕物理内存也不多）。
  setMemPressure({ free: 100, total: 1000, swapFree: 600, swapTotal: 1000, at: 0 })
  assert.equal(memPressureActive(), false)
  // swap 用了 90% + 物理内存只剩 10% → 颠簸。
  setMemPressure({ free: 100, total: 1000, swapFree: 100, swapTotal: 1000, at: 0 })
  assert.equal(memPressureActive(), true)
  // swap 满但物理内存还够 → 不算颠簸（这时降级属于过度反应）。
  setMemPressure({ free: 800, total: 1000, swapFree: 100, swapTotal: 1000, at: 0 })
  assert.equal(memPressureActive(), false)
})

test('currentBudgetBytes：颠簸时预算收紧', () => {
  setMemPressure({ free: 800, total: 1000, swapFree: 600, swapTotal: 1000, at: 0 })
  assert.equal(currentBudgetBytes(), VIEW_BUDGET_BYTES)
  setMemPressure({ free: 100, total: 1000, swapFree: 100, swapTotal: 1000, at: 0 })
  assert.equal(currentBudgetBytes(), VIEW_BUDGET_PRESSURE_BYTES)
})
