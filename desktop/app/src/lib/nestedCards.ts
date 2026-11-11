// 嵌套任务卡的视图规则（子智能体 → 孙任务）：谁画在哪一层。
//
// 为什么需要它：事件流只有**一条**。bridge 按**会话**注入 session_id（desktop/bridge/main.go
// 的 emit），子智能体派孙任务时，task_started / agent_start / llm_error 仍然经父 handler
// 上浮到同一条流（subagent/subagent.go 的 handler = tc.Handler），store 于是把它们插成
// 平铺的卡片（useAppStore 的 task_started / agent_start）。卡上带着 parentTaskId（派发方
// 的任务 id），但主转录不消费它 —— 结果就是孙任务的车道（连同它的重试错误）长在主会话的
// 时间线上，而派发它的那个子智能体的页面里反而什么都没有（用户 2026-11 报障）。
//
// 顺带的第二个症状：简洁/运行时皮肤按 run 切段，段的键对 agent 卡取的是 parentRunId
// （components/runtime/turn/selectTurns.ts 的 owningRunId）。孙卡的 parentRunId 是**子智能体
// 那次运行**，而那次运行的正文/工具都在它自己的卡里 —— 主转录里没有它们的顶层块，于是
// 多出一个只有孙卡的「Working … · 0 calls」空盒子。孙卡不在本层画，盒子自然消失。
//
// 这里只决定**视图去哪儿画**，不动 store：块必须留在 blocks 里 —— 左栏树、停止按钮、
// run→卡 的查找（llm_error 只带 run_id）、checkpoint 归一化全靠它们。
//
// 规则：
//   · 父卡在同一份列表里 → 这张卡不在本层画（改在父卡的子会话页里画）
//   · 父卡不在（历史窗口裁掉了 / 翻页还没到）→ 照旧画在本层：宁可多一条，也不让卡消失
//   · parentTaskId 成环（坏数据）→ 按「父不在」处理（不能两边都不画）
import type { MsgBlock } from '../store/useAppStore'

/** 任务卡（子智能体卡片 / 后台任务占位）—— 只有这两种带嵌套锚。 */
export type TaskCardBlock = Extract<MsgBlock, { kind: 'agent' | 'async_task' }>

const isCard = (b: MsgBlock): b is TaskCardBlock => b.kind === 'agent' || b.kind === 'async_task'

/** 派发方**任务** id（子→孙派发时非空；主 Agent 直派为空）。 */
export function parentAnchorOf(b: MsgBlock): string | undefined {
  if (!isCard(b)) return undefined
  return b.parentTaskId || undefined
}

/** 卡片身份：优先 taskId（占位与 agent 卡是同一个任务），退化 runId。 */
const keyOf = (b: TaskCardBlock): string =>
  b.taskId || (b.kind === 'agent' ? b.runId : '')

/** 派发时刻（排序用：agent 卡记 spawnedAt，占位卡记 startedAt）。 */
const startedAtOf = (b: TaskCardBlock): number =>
  b.kind === 'agent' ? b.spawnedAt ?? 0 : b.startedAt ?? 0

export interface NestedCards {
  /** 本层该画的块：可归属的孙任务卡已摘掉（父卡不在窗口里时保留）。 */
  shown: MsgBlock[]
  /** 每张卡直接派发的子卡（按派发时刻排序）：父卡的子会话页据此渲染。 */
  childrenByTask: Map<string, TaskCardBlock[]>
  /** 被摘掉的块 id（测试与调试用；渲染层不读）。 */
  hidden: Set<number>
}

/**
 * 判断一张卡是不是「属于本层之下的卡」：沿 parentTaskId 上溯，只有当链条终止在一张
 * **没有 parentTaskId 的卡**上（也就是本层的卡）时才算嵌套。父卡不在列表里 → false。
 */
function nestedUnderLocalCard(b: TaskCardBlock, byTask: ReadonlyMap<string, TaskCardBlock>): boolean {
  const seen = new Set<string>()
  let cur = b
  for (;;) {
    const parentId = parentAnchorOf(cur)
    if (!parentId) return cur !== b // 走到本层的根：自己是根就不算嵌套
    if (seen.has(parentId)) return false // 环：当本层处理，别把两边都藏了
    seen.add(parentId)
    const parent = byTask.get(parentId)
    if (!parent) return false // 父卡不在窗口里：留在本层（信息优先）
    cur = parent
  }
}

export function nestCards(blocks: readonly MsgBlock[]): NestedCards {
  // 先建 taskId → 卡 的索引：同一个任务可能同时有 task_started 占位与 agent_start 的卡，
  // 后到的那张（agent 卡）胜出 —— 与 store 的替换语义一致。
  const byTask = new Map<string, TaskCardBlock>()
  for (const b of blocks) {
    if (!isCard(b)) continue
    if (b.taskId) byTask.set(b.taskId, b)
  }
  const hidden = new Set<number>()
  const childrenByTask = new Map<string, TaskCardBlock[]>()
  // 同一个子任务的双来源（占位 + 卡）只归一次：身份用 taskId/runId，
  // 后到的那张替换先到的（占位被 agent 卡升级）。
  const childAt = new Map<string, number>() // 父:身份 → 在数组里的下标
  for (const b of blocks) {
    if (!isCard(b)) continue
    const parent = parentAnchorOf(b)
    if (!parent) continue
    if (!nestedUnderLocalCard(b, byTask)) continue
    hidden.add(b.id)
    const list = childrenByTask.get(parent) ?? []
    const slot = `${parent}\u0000${keyOf(b)}`
    const at = childAt.get(slot)
    if (at == null) {
      childAt.set(slot, list.length)
      list.push(b)
    } else {
      list[at] = b
    }
    childrenByTask.set(parent, list)
  }
  for (const list of childrenByTask.values()) {
    list.sort((a, b) => startedAtOf(a) - startedAtOf(b))
  }
  return {
    shown: hidden.size === 0 ? (blocks as MsgBlock[]) : blocks.filter((b) => !hidden.has(b.id)),
    childrenByTask,
    hidden,
  }
}

/**
 * 子会话页用：这一层（taskId）直接派发了哪些卡。给的是 `blocks` 里的原始块 ——
 * 调用方负责分配渲染 id（负数区间），因为原块的 id 是会话级的。
 */
export function childrenOf(blocks: readonly MsgBlock[], taskId: string | undefined): TaskCardBlock[] {
  if (!taskId) return []
  return nestCards(blocks).childrenByTask.get(taskId) ?? []
}

/** 块上的「发生时刻」（能当插入锚用的那几个）：没有时间的块（信号/错误行）跳过。 */
function blockTimeOf(b: MsgBlock): number | undefined {
  switch (b.kind) {
    case 'user': return b.ts
    case 'assistant': return b.ts
    case 'tool': return b.startedAt
    case 'compression': return b.startedAt
    default: return undefined
  }
}

/**
 * 孙任务卡在**父页**里的落点：派发它的那一刻，而不是页尾。
 *
 * 为什么要它：`parentTaskId` 只回答「是谁的孩子」，不回答「哪一刻派的」。追加到末尾的
 * 后果是「先派的活排在后干的活后面」——子 agent 干了半小时、派了三次子任务，三次全挤在
 * 页尾，读起来像收尾前一口气派的。
 *
 * 锚的优先级：
 *   1. 派发工具行本身（`tool.taskId === 子卡.taskId`）—— 精确，但只有部分路径给 taskId；
 *   2. 时间锚（`spawnedAt` vs 块的 ts/startedAt）—— 插在**最后一个不晚于它**的块之后；
 *   3. 都没有（旧 checkpoint：items 不带时间）→ 追加到末尾（旧行为，不猜）。
 *
 * 输入不被修改；子卡复制成新 id（`nextId` 由调用方给负数区间，避免与会话级 id 撞 key）。
 */
export function placeChildCards(
  blocks: readonly MsgBlock[],
  children: readonly TaskCardBlock[],
  nextId: () => number,
): MsgBlock[] {
  if (children.length === 0) return blocks as MsgBlock[]
  // 派发顺序：同锚的卡按时间先后落位
  const pending = [...children]
    .sort((a, b) => startedAtOf(a) - startedAtOf(b))
    .map((card) => ({ card, at: startedAtOf(card), taskId: card.taskId }))
  const anchors = pending.map(({ at, taskId }) => {
    // 1) 派发工具行：子卡的任务 id 挂在工具行上（promote/编排路径）
    if (taskId) {
      for (let i = blocks.length - 1; i >= 0; i--) {
        const b = blocks[i]
        if (b.kind === 'tool' && b.taskId === taskId) return i
      }
    }
    // 2) 时间锚：最后一个「不晚于它」的块
    if (at > 0) {
      for (let i = blocks.length - 1; i >= 0; i--) {
        const t = blockTimeOf(blocks[i])
        if (t != null && t > 0 && t <= at) return i
      }
    }
    return -1 // 3) 没有可用的锚
  })
  const after = new Map<number, MsgBlock[]>()
  const tail: MsgBlock[] = []
  pending.forEach(({ card }, i) => {
    const clone = { ...card, id: nextId() } as MsgBlock
    const at = anchors[i]
    if (at < 0) {
      tail.push(clone)
      return
    }
    const list = after.get(at) ?? []
    list.push(clone)
    after.set(at, list)
  })
  const out: MsgBlock[] = []
  blocks.forEach((b, i) => {
    out.push(b)
    const list = after.get(i)
    if (list) out.push(...list)
  })
  out.push(...tail)
  return out
}
