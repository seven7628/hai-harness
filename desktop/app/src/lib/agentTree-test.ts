// agentTree-test.ts —— 子智能体树纯逻辑单测（node:test，无 DOM/Electron）。
//
// 为什么这些断言值得有：树的两条性质一旦坏，用户**看不见**坏在哪 ——
//   · 递归：孙智能体必须挂在子智能体下面，掉到根上看起来「也像一棵树」（只是层级错了）；
//   · 容错：父 run 先收敛（checkpoint 只剩子卡）时，子卡**必须**还能显示（提为根），
//     否则用户会以为「子智能体凭空消失了」——这类回归在 UI 上无从判读。
// 另外连线几何（guides/last）是纯计算，值得钉死：它决定「谁接在谁下面」的视觉可信度。
// 后台任务与子智能体同住一棵树：task_started 先建占位、agent_start 到达后升级成 agent，
// 必须是**同一个节点**（两条来源同一个 task_id），否则用户会看到两层一样的卡。
//
// 运行：esbuild bundle 后 node --test（见 package.json 的 test:agent-tree）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  buildAgentTree,
  collectTreeKeys,
  focusedPathKeys,
  countRunningNodes,
  flattenAgentTree,
  flattenVisible,
  pathKeyOf,
  subtreeSize,
} from './agentTree'

test('嵌套：子智能体下的子智能体挂在子节点下（不是平级）', () => {
  const roots = buildAgentTree([
    { id: 'a1', label: 'explore' },
    { id: 'a2', parentId: 'a1', label: 'grand' },
  ])
  assert.equal(roots.length, 1)
  assert.equal(roots[0].id, 'a1')
  assert.equal(roots[0].children.length, 1)
  assert.equal(roots[0].children[0].id, 'a2')
  assert.equal(roots[0].children[0].depth, 2)
})

test('三层递归：深度按父链递增，guides 列数 = depth-1', () => {
  const roots = buildAgentTree([
    { id: 'a1' },
    { id: 'a2', parentId: 'a1' },
    { id: 'a3', parentId: 'a2' },
    { id: 'a4', parentId: 'a3' },
  ])
  const flat = flattenAgentTree(roots)
  assert.deepEqual(flat.map((n) => n.depth), [1, 2, 3, 4])
  assert.deepEqual(flat.map((n) => n.guides.length), [0, 1, 2, 3])
  // 单链：每层都是「最后一个」，所以祖先层都不需要画贯穿竖线
  assert.deepEqual(flat.map((n) => n.last), [true, true, true, true])
  assert.deepEqual(flat[3].guides, [false, false, false])
})

test('兄弟：非末位节点让下层补一根贯穿竖线', () => {
  const roots = buildAgentTree([
    { id: 'a1', startedAt: 1 },
    { id: 'a2', startedAt: 2 },
    { id: 'b1', parentId: 'a1', startedAt: 3 },
    { id: 'b2', parentId: 'a1', startedAt: 4 },
  ])
  assert.equal(roots.length, 2)
  assert.equal(roots[0].last, false) // a1 后面还有 a2
  assert.equal(roots[1].last, true)
  const kids = roots[0].children
  assert.equal(kids.length, 2)
  assert.equal(kids[0].last, false)
  assert.equal(kids[1].last, true)
  // a1 有后续兄弟 → 它孩子的第一列必须补竖线
  assert.deepEqual(kids[0].guides, [true])
})

test('容错：父 run 不在列表里 → 提为根（不丢节点）', () => {
  const roots = buildAgentTree([{ id: 'a1', parentId: 'gone', label: 'orphan' }])
  assert.equal(roots.length, 1)
  assert.equal(roots[0].id, 'a1')
  assert.equal(roots[0].parentId, undefined, '父指向已清除，避免渲染层再去找')
  assert.equal(roots[0].label, 'orphan')
})

test('容错：环（a→b→a）被打断成根，不死循环', () => {
  const roots = buildAgentTree([
    { id: 'a', parentId: 'b' },
    { id: 'b', parentId: 'a' },
  ])
  const flat = flattenAgentTree(roots)
  assert.equal(flat.length, 2, '两个节点都要在树里')
  assert.ok(roots.length >= 1)
})

test('去重：同一 id 重复投递只建一个节点，空字段被后续补齐', () => {
  const roots = buildAgentTree([
    { id: 'a1', parentId: 'r1' },
    { id: 'a1', label: 'late label', taskId: 'task-1' },
  ])
  assert.equal(roots.length, 1)
  assert.equal(roots[0].label, 'late label')
  assert.equal(roots[0].taskId, 'task-1')
})

test('task→agent 升级：同一 id 的占位卡与运行卡合成一个节点（kind 取 agent）', () => {
  const roots = buildAgentTree([
    { id: 'task-9', kind: 'task', label: '后台任务', toolName: 'bash', status: 'running' },
    { id: 'task-9', kind: 'agent', label: 'review', status: 'running' },
  ])
  assert.equal(roots.length, 1)
  assert.equal(roots[0].kind, 'agent', 'agent 是更具体的身份（有转录）')
  assert.equal(roots[0].label, 'review')
  assert.equal(roots[0].toolName, 'bash', '工具名不能因升级丢掉（图标来源）')
})

test('排序：同层按 startedAt 升序；缺失时间戳的排后面', () => {
  const roots = buildAgentTree([
    { id: 'a1', startedAt: 30 },
    { id: 'a2', startedAt: 10 },
    { id: 'a3' },
  ])
  assert.deepEqual(roots.map((n) => n.id), ['a2', 'a1', 'a3'])
  assert.deepEqual(roots.map((n) => n.index), [0, 1, 2])
})

test('统计：running/interrupting 都算在跑（含孙节点）', () => {
  const roots = buildAgentTree([
    { id: 'a1', status: 'running' },
    { id: 'a2', parentId: 'a1', status: 'completed' },
    { id: 'a3', parentId: 'a2', status: 'interrupting' },
    { id: 'b1', status: 'failed' },
  ])
  assert.equal(countRunningNodes(roots), 2)
})

test('pathKey：路径键随父链拼接，同名不同分支不串', () => {
  const roots = buildAgentTree([
    { id: 'a1' },
    { id: 'a2', parentId: 'a1' },
    { id: 'b1' },
    { id: 'b2', parentId: 'b1' },
  ])
  const keys = collectTreeKeys(roots)
  assert.equal(keys.get('a2'), 'a1/a2')
  assert.equal(keys.get('b2'), 'b1/b2')
  assert.notEqual(keys.get('a2'), keys.get('b2'))
  assert.equal(pathKeyOf(roots[0], 'x'), 'x/a1')
})

test('折叠：flattenVisible 跳过被折叠节点的整棵子树', () => {
  const roots = buildAgentTree([
    { id: 'a1' },
    { id: 'a2', parentId: 'a1' },
    { id: 'a3', parentId: 'a2' },
    { id: 'b1' },
  ])
  assert.equal(flattenVisible(roots, new Set()).length, 4)
  const collapsed = new Set(['a1'])
  const vis = flattenVisible(roots, collapsed)
  assert.deepEqual(vis.map((n) => n.id), ['a1', 'b1'], '折叠 a1 后它的整棵子树都不显示')
})

test('subtreeSize：含自己在内的子孙节点数（「还有 N 个」提示）', () => {
  const roots = buildAgentTree([
    { id: 'a1' },
    { id: 'a2', parentId: 'a1' },
    { id: 'a3', parentId: 'a2' },
  ])
  assert.equal(subtreeSize(roots[0]), 3)
  assert.equal(subtreeSize(roots[0].children[0]), 2)
})

test('选中连线：点亮「根 → 自己」整条路径（祖先链）', () => {
  const roots = buildAgentTree([
    { id: 'a1' },
    { id: 'a2', parentId: 'a1' },
    { id: 'a3', parentId: 'a2' },
    { id: 'b1' },
  ])
  assert.deepEqual([...focusedPathKeys(roots, 'a3')].sort(), ['a1', 'a1/a2', 'a1/a2/a3'].sort())
  assert.deepEqual([...focusedPathKeys(roots, 'b1')], ['b1'], '另一分支不点亮')
  assert.equal(focusedPathKeys(roots, 'nope').size, 0, '找不到 → 空集（不抛错）')
})

test('空输入：空森林（不抛错）', () => {
  assert.deepEqual(buildAgentTree([]), [])
  assert.deepEqual(flattenAgentTree([]), [])
  assert.deepEqual(flattenVisible([], new Set()), [])
  assert.equal(countRunningNodes([]), 0)
})
