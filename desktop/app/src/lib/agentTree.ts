// 子智能体 / 异步任务的树形纯逻辑（左栏「会话 → 子智能体 → 孙智能体 …」）。
//
// 为什么单独成模块：递归结构 + 连线几何是**纯函数**，能在 node 里直接单测
// （见 agentTree-test.ts），不需要挂 DOM 或 React。渲染层（AgentTree.tsx）只负责
// 把这里算出来的行画出来 —— 树的正确性（孤儿归属、深度、连线列）不依赖渲染。
//
// 两类节点同住一棵树（用户视角就是「这次会话派生出去的东西」）：
//   · agent = 子智能体运行：id = run_id，parentId = agent_start.parent_run_id
//   · task  = 后台任务（异步 bash / 被摘离的工具调用）：id = task_id，
//             parentId = task_started.parent_run_id（谁派的就挂谁下面）
// 只按 run/task 的父子锚点连边，**不按时间排序猜**：并发派发时时间序等于没有信息。
import type { AsyncTaskStatus } from '../store/useAppStore'

export type AgentNodeKind = 'agent' | 'task'

/** 树的节点状态（run 的终态与任务的六态共用一个窄类型） */
export type AgentNodeStatus = AsyncTaskStatus | 'done' | 'error' | 'interrupted' | 'abandoned'

/** 树节点的最小输入（来自 runNodes + agent 卡片 + task 卡片，字段都可缺） */
export interface AgentTreeInput {
  /** 树节点身份：taskId 优先（同一后台子智能体的三条来源合成一个节点），退化 runId */
  id: string
  /** agent 的真实运行 id（agent_start.run_id）。与 id 不同 —— 后者可能被 taskId 取代，
   *  而 run_id 才是「这条转录/这轮事件」的锚点（找卡片、找事件都用它）。 */
  runId?: string
  parentId?: string
  kind?: AgentNodeKind
  label?: string
  /** agent 节点：它对应的后台任务 id（停止/续跑用）；task 节点：同 id */
  taskId?: string
  /** task 节点：发起它的工具名（图标/文案区分 bash 与子 agent） */
  toolName?: string
  status?: AgentNodeStatus
  startedAt?: number
}

export interface AgentTreeNode {
  /** 树节点身份（taskId 优先，退化 runId）—— 折叠键/选中键都用它 */
  id: string
  /** agent 的真实 run id（task 节点为空）；找卡片/订阅事件用它 */
  runId?: string
  parentId?: string
  kind: AgentNodeKind
  label: string
  taskId?: string
  toolName?: string
  /** 相对根的深度：1 = 直接挂在当前会话下 */
  depth: number
  status?: AgentNodeStatus
  startedAt?: number
  children: AgentTreeNode[]
  /** 在父的 children 中的序号（画连线用） */
  index: number
  /** 同层最后一个 → 父的竖线在此收头（└ 而不是 ├） */
  last: boolean
  /** 祖先是否还有后续兄弟节点（决定每层竖线画不画）：长度 = depth - 1 */
  guides: boolean[]
}

/**
 * buildAgentTree 把扁平的 run/task 列表构建成森林（多个根的数组）。
 *
 * 容错（真实事件流里都会遇到）：
 *   1. 父不在列表里（父 run 已收敛 / checkpoint 只留了子卡）→ 提为根，不丢节点；
 *   2. 环（异常日志 / 重放的错序事件）→ 打断成根，绝不死循环；
 *   3. 同 id 重复（agent_start 重复投递 / task 与 agent 同 taskId）→ 只补字段，不新增。
 *
 * 排序：同层按 startedAt 升序（派发顺序），缺失时按输入顺序。
 */
export function buildAgentTree(input: AgentTreeInput[]): AgentTreeNode[] {
  const nodes = new Map<string, AgentTreeNode>()
  const order: string[] = []
  for (const it of input) {
    const id = String(it.id || '')
    if (!id) continue
    const prev = nodes.get(id)
    if (prev) {
      // 重复项：补空字段，不新增（父关系以先到的为准，晚到的只在原本缺失时生效）——
      // 典型：task_started 先建 async_task 占位，agent_start 到达后同一个 task 变成 agent 卡
      if (!prev.parentId && it.parentId) prev.parentId = it.parentId
      if (!prev.runId && it.runId) prev.runId = it.runId
      // 名字：空字符串也算「还没有」——占位卡的泛称必须能被 agent 真名覆盖（见下）
      if (!prev.label && it.label) prev.label = it.label
      if (!prev.taskId && it.taskId) prev.taskId = it.taskId
      if (!prev.toolName && it.toolName) prev.toolName = it.toolName
      if (!prev.startedAt && it.startedAt) prev.startedAt = it.startedAt
      if (!prev.status && it.status) prev.status = it.status
      // 占位（task）升级成 agent：agent 是更具体的身份，它的转写才有内容，
      // 名字也以 agent 的为准（占位贴的是「后台任务」这种泛称）。
      if (it.kind === 'agent' && prev.kind === 'task') {
        prev.kind = 'agent'
        if (it.label) prev.label = it.label
      }
      continue
    }
    nodes.set(id, {
      id,
      runId: it.runId || undefined,
      parentId: it.parentId || undefined,
      kind: it.kind ?? 'agent',
      // 名字**先留空**、建图完成后统一兜底为 id（见函数尾）：占位卡与运行卡合并时
      // 要能被 agent 的真名覆盖，这里先塞 id 会把「空名字」变成「有名字」而挡住覆盖。
      label: it.label || '',
      taskId: it.taskId || undefined,
      toolName: it.toolName || undefined,
      depth: 1, // 占位，下面按父链重算
      status: it.status,
      startedAt: it.startedAt,
      children: [],
      index: 0,
      last: false,
      guides: [],
    })
    order.push(id)
  }

  // 链到父上；无父 / 父不存在 / 会成环 → 根
  const roots: AgentTreeNode[] = []
  for (const id of order) {
    const n = nodes.get(id)!
    const p = n.parentId ? nodes.get(n.parentId) : undefined
    if (p && p !== n && !createsCycle(nodes, n, p)) p.children.push(n)
    else {
      n.parentId = undefined
      roots.push(n)
    }
  }

  // 同层排序（派发顺序）+ 深度/连线列
  const sortChildren = (list: AgentTreeNode[]) => {
    list.sort((a, b) => {
      const sa = a.startedAt ?? Number.MAX_SAFE_INTEGER
      const sb = b.startedAt ?? Number.MAX_SAFE_INTEGER
      if (sa !== sb) return sa - sb
      return order.indexOf(a.id) - order.indexOf(b.id)
    })
    for (let i = 0; i < list.length; i++) {
      const n = list[i]
      n.index = i
      n.last = i === list.length - 1
    }
  }
  // 名字兜底：始终没有名字的节点用 id（可读性最低的选项，但不留空行）
  for (const n of nodes.values()) if (!n.label) n.label = n.id

  sortChildren(roots)
  const walk = (n: AgentTreeNode, depth: number, ancestorHasNext: boolean[]) => {
    n.depth = depth
    // guides[i] = 第 i 层祖先「后面还有兄弟」→ 该列要画一条贯穿竖线
    n.guides = ancestorHasNext.slice()
    sortChildren(n.children)
    for (const c of n.children) walk(c, depth + 1, [...ancestorHasNext, !n.last])
  }
  for (const r of roots) walk(r, 1, [])

  return roots
}

/** createsCycle：把 child 挂到 parent 下是否会形成环（沿 parent 的祖先链找 child）。 */
function createsCycle(map: Map<string, AgentTreeNode>, child: AgentTreeNode, parent: AgentTreeNode): boolean {
  const seen = new Set<string>()
  let cur: AgentTreeNode | undefined = parent
  while (cur) {
    if (cur === child) return true
    if (!cur.parentId || seen.has(cur.id)) return false
    seen.add(cur.id)
    cur = map.get(cur.parentId)
  }
  return false
}

/** flattenAgentTree 深度优先展开成渲染行（父一定在子之前）。 */
export function flattenAgentTree(roots: AgentTreeNode[]): AgentTreeNode[] {
  const out: AgentTreeNode[] = []
  const walk = (n: AgentTreeNode) => {
    out.push(n)
    for (const c of n.children) walk(c)
  }
  for (const r of roots) walk(r)
  return out
}

/**
 * flattenVisible 按「折叠集合」展开（被折叠节点的子孙不进结果）。
 * collapsed 的键是 pathKey（见 pathKeyOf）——不用 id：重放/恢复后 id 会变，
 * 而用户想记住的是「这个位置的分支是收起的」。
 */
export function flattenVisible(roots: AgentTreeNode[], collapsed: ReadonlySet<string>): AgentTreeNode[] {
  const out: AgentTreeNode[] = []
  const walk = (n: AgentTreeNode, parentKey: string) => {
    out.push(n)
    const key = pathKeyOf(n, parentKey)
    if (collapsed.has(key)) return
    for (const c of n.children) walk(c, key)
  }
  for (const r of roots) walk(r, '')
  return out
}

/** countRunningNodes 树内「在跑」节点数（角标用；interrupting 也算在跑）。 */
export function countRunningNodes(roots: AgentTreeNode[]): number {
  let n = 0
  const walk = (x: AgentTreeNode) => {
    if (x.status === 'running' || x.status === 'interrupting') n++
    for (const c of x.children) walk(c)
  }
  for (const r of roots) walk(r)
  return n
}

/** subtreeSize 该节点子树里的节点数（含自己）——「还有 N 个」提示用。 */
export function subtreeSize(node: AgentTreeNode): number {
  let n = 1
  for (const c of node.children) n += subtreeSize(c)
  return n
}

/**
 * pathKeyOf 从根到该节点的 id 路径（"r1/r2/r3"）。
 *
 * 用途：**折叠状态的身份键**。只用 id 不够 —— 重放/恢复后 id 会变，而路径形状
 * 不变；只用 depth 也不够 —— 同一层的两个分支会串。路径键两者兼顾。
 */
export function pathKeyOf(node: AgentTreeNode, parentKey = ''): string {
  return parentKey ? `${parentKey}/${node.id}` : node.id
}

/**
 * collectTreeKeys 全部节点的 pathKey（「全部展开/折叠」用）。
 * 返回 Map<id, pathKey>：渲染层按键即可（id 唯一）。
 */
export function collectTreeKeys(roots: AgentTreeNode[]): Map<string, string> {
  const out = new Map<string, string>()
  const walk = (n: AgentTreeNode, parentKey: string) => {
    const key = pathKeyOf(n, parentKey)
    out.set(n.id, key)
    for (const c of n.children) walk(c, key)
  }
  for (const r of roots) walk(r, '')
  return out
}

// ---------------------------------------------------------------------------
// view → 树（把 store 的 runNodes / blocks 投影成上面的输入）
// ---------------------------------------------------------------------------

/** store.RunNode 的最小子集（结构类型：不 import store，避免 lib → store 循环） */
export interface ViewRunNode {
  id: string
  parentId?: string
  label?: string
  status?: string
  kind: string // 'run' | 'tool'
  taskId?: string
  startedAt?: number
}

/** store.MsgBlock 里与树相关的两种块的最小子集 */
export interface ViewAgentBlock {
  kind: 'agent'
  runId: string
  parentRunId?: string
  taskId?: string
  label?: string
  status?: string
  spawnedAt?: number
}
export interface ViewTaskBlock {
  kind: 'async_task' | 'tool'
  taskId?: string
  parentRunId?: string
  label?: string
  name?: string
  toolName?: string
  status?: string
  startedAt?: number
  promoted?: boolean
}

/**
 * agentTreeFromView 把「一个会话视图」投影成子智能体森林。
 *
 * **节点身份 = taskId 优先，退化 runId**：同一个后台子智能体同时有
 *   · agent_start → runNode（runId 身份）
 *   · task_started → async_task 占位（taskId 身份）
 *   · agent_start 到达后 → agent 卡片（带 taskId）
 * 三者是**同一个东西**，用 taskId 作键才能合成一个节点（占位升级成 agent 卡）。
 * 同步子智能体没有 taskId → 退化为 runId（天然唯一）。
 *
 * 只收「派生出去的东西」：主 run（无 parentId 的 runNode）不在这棵树里 —— 会话行本身就是它。
 */
export function agentTreeFromView(
  runNodes: ViewRunNode[],
  blocks: readonly (ViewAgentBlock | ViewTaskBlock)[],
): AgentTreeNode[] {
  // runId → 节点键（父链查找用；必须先于建图算好，否则「父的 taskId」还没确定）
  const keyOfRun = new Map<string, string>()
  for (const b of blocks) {
    if (b.kind === 'agent' && b.runId) keyOfRun.set(b.runId, b.taskId || b.runId)
  }
  for (const n of runNodes) {
    if (n.kind !== 'run') continue
    if (!keyOfRun.has(n.id)) keyOfRun.set(n.id, n.taskId || n.id)
  }

  const input: AgentTreeInput[] = []
  for (const n of runNodes) {
    // 主 run = 会话本身，不进树；工具节点（tool-xxx）不属于这条线（它们在执行链路 tab）
    if (n.kind !== 'run' || !n.parentId) continue
    input.push({
      id: keyOfRun.get(n.id) || n.id,
      runId: n.id,
      parentId: keyOfRun.get(n.parentId) || n.parentId,
      kind: 'agent',
      label: n.label,
      taskId: n.taskId,
      status: runStatusToNode(n.status),
      startedAt: n.startedAt,
    })
  }
  for (const b of blocks) {
    if (b.kind === 'agent') {
      const key = b.taskId || b.runId
      input.push({
        id: key,
        runId: b.runId,
        parentId: b.parentRunId ? (keyOfRun.get(b.parentRunId) || b.parentRunId) : undefined,
        kind: 'agent',
        label: b.label,
        taskId: b.taskId,
        status: b.status as AgentNodeStatus | undefined,
        startedAt: b.spawnedAt,
      })
      continue
    }
    // 后台任务：async_task 占位 / 被摘离（promoted）的工具调用。两者都是 taskId 身份。
    if (!b.taskId) continue
    if (b.kind === 'tool' && !b.promoted) continue
    input.push({
      id: b.taskId,
      parentId: b.parentRunId ? (keyOfRun.get(b.parentRunId) || b.parentRunId) : undefined,
      kind: 'task',
      label: b.kind === 'async_task' ? b.label : b.name,
      taskId: b.taskId,
      toolName: b.kind === 'async_task' ? b.toolName : b.name,
      status: b.status as AgentNodeStatus | undefined,
      startedAt: b.startedAt,
    })
  }
  return buildAgentTree(input)
}

/** run 节点的四态 → 树的节点状态（done 也当「已完成」显示）。 */
function runStatusToNode(s: string | undefined): AgentNodeStatus | undefined {
  switch (s) {
    case 'running': return 'running'
    case 'done': return 'completed'
    case 'error': return 'failed'
    case 'interrupted': return 'interrupted'
    default: return undefined
  }
}

/**
 * focusedPathKeys 选中节点的「根 → 自己」路径键集合。
 *
 * 用途：**选中时点亮整条连线**（参考产品里选中子会话会画出连接线）。只点亮被选中那一行
 * 不够 —— 递归场景下用户真正想知道的是「它是从哪条链上下来的」，那正是祖先链。
 */
export function focusedPathKeys(roots: AgentTreeNode[], id: string): Set<string> {
  const out = new Set<string>()
  const walk = (n: AgentTreeNode, parentKey: string, acc: string[]): boolean => {
    const key = pathKeyOf(n, parentKey)
    const next = [...acc, key]
    if (n.id === id) {
      for (const k of next) out.add(k)
      return true
    }
    for (const c of n.children) if (walk(c, key, next)) return true
    return false
  }
  for (const r of roots) if (walk(r, '', [])) break
  return out
}

/** pathOfNode 从树里找某节点的祖先链（面包屑 / 连线高亮用）。 */
export function pathOfNode(roots: AgentTreeNode[], id: string): AgentTreeNode[] {
  const out: AgentTreeNode[] = []
  const walk = (n: AgentTreeNode, acc: AgentTreeNode[]): boolean => {
    const next = [...acc, n]
    if (n.id === id) {
      out.push(...next)
      return true
    }
    for (const c of n.children) if (walk(c, next)) return true
    return false
  }
  for (const r of roots) if (walk(r, [])) break
  return out
}
