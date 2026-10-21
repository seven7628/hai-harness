import { memo, useMemo, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import type { RunNode } from '../store/useAppStore'
import { agentTreeFromView, flattenVisible, collectTreeKeys, focusedPathKeys, pathKeyOf, subtreeSize, pageRoots, DEFAULT_ROOT_PAGE } from '../lib/agentTree'
import type { AgentTreeNode } from '../lib/agentTree'
import { useT } from '../i18n'
import { Bot, ChevronDown, Clock } from './icons'

// 子智能体树：**一次会话派生出去的所有运行**（子智能体 / 孙智能体 / 后台任务）。
//
// 为什么是树而不是列表（用户 2026-10 需求）：子智能体可以再派子智能体
// （subagent.Tool 继承 ParentRunId/Depth），列表把所有运行拉平成同级，
// 「谁是谁派出来的」这个最关键的信息就没有了。树 + 连线（├─ └─）把它显式画出来。
//
// 连线为什么用字符/伪元素而不是 SVG：左栏可滚动 + 宽度可拖（140–360px），
// 一条竖线要跟着行高与缩进走；用 per-row 的 guide 列（见 lib/agentTree 的 guides）
// 交给 CSS 画，滚动/换行/异高都自然对齐，也不需要测量。
//
// 交互：点行 = 打开该 agent 的子会话；▸/▾ = 折叠整棵子树；
// 运行中的行带脉冲点，停止按钮走 interrupt_task（与任务卡同一个 action）。
//
// 排序与分页（用户 2026-10 要求「倒序排列；默认只加载最近 5 个，支持加载更多」）：
//   · 倒序在 lib/agentTree.buildAgentTree 里做（按 startedAt 降序，连线几何跟着视觉顺序走）；
//   · 分页**只在根层**做：一进来不该被几十个历史子智能体淹掉，而某个分支里主动展开的
//     子节点不该被二次截断。判定与「聚焦节点必须可见」的规则在 lib 的 pageRoots（可单测）。
export const AgentTree = memo(function AgentTree({
  variant = 'left',
}: {
  /** left = 左栏（紧凑，单行）；center = 对话区（带状态/耗时/停止按钮） */
  variant?: 'left' | 'center'
}) {
  const t = useT()
  const runNodes = useAppStore((s) => s.runNodes)
  const blocks = useAppStore((s) => s.blocks)
  const interruptTask = useAppStore((s) => s.interruptTask)
  const focusAgent = useAppStore((s) => s.focusAgent)
  const focused = useAppStore((s) => s.focusedAgent)
  // 折叠集合按**会话**记：切会话再回来，用户的折叠选择还在（纯 UI 偏好，不落盘）
  const sid = useAppStore((s) => s.activeSessionId) || ''
  const [collapsed, setCollapsed] = useState<Record<string, Set<string>>>({})
  // 「加载更多」按会话记：切走再回来仍是展开态（与 collapsed 同一种纯 UI 偏好，不落盘）
  const [expandedAll, setExpandedAll] = useState<Record<string, boolean>>({})
  const showAll = expandedAll[sid] ?? false
  const mine = collapsed[sid] ?? EMPTY_SET
  const toggle = (key: string) => {
    setCollapsed((prev) => {
      const cur = new Set(prev[sid] ?? [])
      if (cur.has(key)) cur.delete(key)
      else cur.add(key)
      return { ...prev, [sid]: cur }
    })
  }

  // 只把与树相关的两类块投影进去（user/assistant/… 与树无关，传全量会让
  // memo 在每次流式 chunk 后都重算 —— 这里只依赖「块的身份」而非内容）
  const treeBlocks = useMemo(
    () => blocks.filter((b) => b.kind === 'agent' || b.kind === 'async_task' || (b.kind === 'tool' && b.promoted)),
    [blocks],
  )
  const roots = useMemo(
    () => agentTreeFromView(runNodes as RunNode[], treeBlocks as Parameters<typeof agentTreeFromView>[1]),
    [runNodes, treeBlocks],
  )
  const keys = useMemo(() => collectTreeKeys(roots), [roots])
  // 根层分页：默认最近 DEFAULT_ROOT_PAGE 个；聚焦节点被挡掉时它那棵会被补进来（见 pageRoots）
  const paged = useMemo(
    () => pageRoots(roots, DEFAULT_ROOT_PAGE, { mustShowId: focused?.nodeId, showAll }),
    [roots, showAll, focused?.nodeId],
  )
  const rows = useMemo(() => flattenVisible(paged.roots, mine), [paged, mine])
  // 选中子会话时点亮整条祖先链的连线（见 lib/agentTree.focusedPathKeys 的注释）
  const litKeys = useMemo(() => (focused ? focusedPathKeys(roots, focused.nodeId) : EMPTY_SET), [roots, focused])
  if (rows.length === 0) return null

  // 点行 = 把叙述区切到这个子智能体（它有自己的转录 / 用量 / TODO，像一个子会话）。
  // 再点已选中的行 = 收回主会话（避免"点了没反应"的错觉）。
  const open = (n: AgentTreeNode) => {
    const same = focused?.nodeId === n.id
    focusAgent(
      same
        ? undefined
        : { nodeId: n.id, runId: n.runId ?? n.id, ...(n.taskId ? { taskId: n.taskId } : {}) },
    )
  }

  return (
    <div className={`agent-tree at-${variant}`} role="tree" aria-label={t('left.agents')}>
      {variant === 'center' && (
        <div className="atree-hd">
          <Bot size={13} />
          <span className="atree-title">{t('left.agents')}</span>
          <span className="atree-count mono">{rows.length}</span>
        </div>
      )}
      {rows.map((n) => {
        const key = keys.get(n.id) ?? pathKeyOf(n)
        const hasKids = n.children.length > 0
        const isCollapsed = mine.has(key)
        const onChain = litKeys.has(key)
        const running = n.status === 'running' || n.status === 'interrupting'
        const st = statusMeta(n.status, t)
        return (
          <div
            key={n.id}
            className={`atree-row st-${st.cls}${running ? ' running' : ''}${focused?.nodeId === n.id ? ' on' : ''}`}
            role="treeitem"
            aria-level={n.depth}
            aria-expanded={hasKids ? !isCollapsed : undefined}
            aria-selected={focused?.nodeId === n.id}
            data-run={n.kind === 'agent' ? n.id : ''}
            data-task={n.taskId ?? ''}
            onClick={() => open(n)}
            title={n.label}
          >
            {/* 连线列：每层一个 guide（祖先还有后续兄弟则画竖线），最后一层画 ├ 或 └。
                缩进量与行高都交给 CSS → 行高变化/拖宽都不需要重算几何。 */}
            {n.guides.map((on, i) => (
              // 该行在选中链上 → 它的每一列 guide 都指向链上的祖先（见 focusedPathKeys），一并点亮
              <span key={i} className={`atree-guide${on ? ' on' : ''}${on && onChain ? ' lit' : ''}`} aria-hidden="true" />
            ))}
            <span className={`atree-elbow${n.last ? ' last' : ''}${onChain ? ' lit' : ''}`} aria-hidden="true" />
            {hasKids ? (
              <button
                className={`atree-chev${isCollapsed ? '' : ' open'}`}
                aria-label={t(isCollapsed ? 'left.agentsExpand' : 'left.agentsCollapse')}
                onClick={(e) => {
                  e.stopPropagation()
                  toggle(key)
                }}
              >
                <ChevronDown size={11} />
              </button>
            ) : (
              <span className="atree-chev-sp" aria-hidden="true" />
            )}
            <span className={`atree-dot ${st.cls}${running ? ' running' : ''}`} aria-hidden="true" />
            {n.kind === 'task' ? <Clock size={11} /> : <Bot size={11} />}
            <span className="atree-label">{n.label}</span>
            {isCollapsed && hasKids && (
              <span className="atree-more mono" title={t('left.agentsHidden').replace('{n}', String(subtreeSize(n) - 1))}>
                +{subtreeSize(n) - 1}
              </span>
            )}
            {variant === 'center' && (
              <>
                <span className="atree-st mono">{st.label}</span>
                {running && n.taskId && (
                  <button
                    className="atree-stop"
                    title={t('rp.task.stopTask')}
                    aria-label={t('rp.task.stopTaskAria')}
                    onClick={(e) => {
                      e.stopPropagation()
                      interruptTask(n.taskId!)
                    }}
                  >
                    ⏹
                  </button>
                )}
              </>
            )}
          </div>
        )
      })}
      {/* 还有更早的子智能体没显示 → 给一个明确入口（否则用户会以为它们没了）。
          「还有 N 个」用的是 hidden（被分页挡掉的**根**数），与折叠提示的 +N 不是一个口径。 */}
      {paged.hidden > 0 && (
        <button className="atree-more-btn" onClick={() => setExpandedAll((prev) => ({ ...prev, [sid]: true }))}>
          {t('left.agentsMore').replace('{n}', String(paged.hidden))}
        </button>
      )}
      {showAll && roots.length > DEFAULT_ROOT_PAGE && (
        <button className="atree-more-btn less" onClick={() => setExpandedAll((prev) => ({ ...prev, [sid]: false }))}>
          {t('left.agentsLess')}
        </button>
      )}
    </div>
  )
})

const EMPTY_SET: ReadonlySet<string> = new Set()

/** 六态 → 展示元（与 TaskCard 的六态映射同语义，但这里是树需要的最小集） */
function statusMeta(s: AgentTreeNode['status'], t: (k: string) => string) {
  switch (s) {
    case 'running': return { cls: 'running', label: t('task.status.running') }
    case 'interrupting': return { cls: 'interrupting', label: t('task.status.interrupting') }
    case 'completed': return { cls: 'done', label: t('task.status.completed') }
    case 'interrupted': return { cls: 'interrupted', label: t('task.status.interrupted') }
    case 'failed': case 'error': return { cls: 'error', label: t('task.status.failed') }
    case 'abandoned': return { cls: 'abandoned', label: t('task.status.abandoned') }
    case 'done': return { cls: 'done', label: t('task.status.completed') }
    default: return { cls: 'idle', label: '' }
  }
}
