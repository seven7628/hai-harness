import { memo, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import type { RunNode } from '../store/useAppStore'
import { agentTreeFromView, flattenVisible, collectTreeKeys, pathOfNode, pathKeyOf, subtreeSize, pageRoots, DEFAULT_ROOT_PAGE } from '../lib/agentTree'
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
  // 窗口外的卡片（首屏只回最近 200 块，跑半天的会话里 agent 卡片早就在窗口之外 ——
  // 不并进来，刷新一次树就空一次。见 store 的 SessionView.agentDetached）
  const detached = useAppStore((s) => s.agentDetached)
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
    () => [
      ...blocks.filter((b) => b.kind === 'agent' || b.kind === 'async_task' || (b.kind === 'tool' && b.promoted)),
      ...detached,
    ],
    [blocks, detached],
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
  // —— 选中链的连线：一条从**主会话行**一路画到选中节点的折线 ——
  //
  // 为什么不用「逐行点亮 guide」（旧做法）：guide 每层一列、每行只覆盖自己那一格，
  // 于是同一列会由多行拼出来 —— 视觉上是并排的几条竖线（用户 2026-10：「这个关系效果
  // 也不太美观」），而且它从**根节点**开始画，永远接不到上面的会话行。
  //
  // 现在按真实几何画一条路径：起点 = 会话行下沿（"从主 session 一直连过来"），
  // 沿祖先各层的连线列向下，到祖先节点行心右折，最后进选中节点。选中的越靠下，
  // 线就越长（用户明确要的效果）。坐标全部相对 .agent-tree 量一次，随折叠/分页/宽度变化
  // 重算 —— 不用估算行高，行高本来就会随字号与皮肤变。
  const treeRef = useRef<HTMLDivElement | null>(null)
  const rowRefs = useRef(new Map<string, HTMLDivElement>())
  const [pathD, setPathD] = useState<string | null>(null)
  useLayoutEffect(() => {
    const el = treeRef.current
    const target = focused ? pathOfNode(roots, focused.nodeId) : []
    if (!el || target.length === 0) {
      setPathD((prev) => (prev === null ? prev : null))
      return
    }
    const box = el.getBoundingClientRect()
    const step = parseFloat(getComputedStyle(el).getPropertyValue('--atree-step')) || 14
    // 起点：紧邻的**会话行**下沿（tree 是会话行的兄弟节点，见 LeftPanel）。
    // 量不到就贴住自身顶边 —— 宁可短一点，也不要悬空画到别处去。
    const prev = el.previousElementSibling as HTMLElement | null
    const startY = prev ? prev.getBoundingClientRect().bottom - box.top + 1 : -4
    const pts: Array<{ x: number; y: number }> = []
    for (const n of target) {
      const row = rowRefs.current.get(n.id)
      if (!row) {
        // 有祖先被折叠/分页挡住 → 没有可达的几何：撤掉旧线，别留一条接不上的残线
        setPathD((prevD) => (prevD === null ? prevD : null))
        return
      }
      const r = row.getBoundingClientRect()
      // 该层连线列的**中心**：第 d 层前面有 d-1 个 guide 列，elbow 列宽 step-2
      pts.push({ x: (n.depth - 1) * step + step / 2, y: r.top - box.top + r.height / 2 })
    }
    const last = pts[pts.length - 1]
    let d = `M ${startY >= 0 ? pts[0].x : pts[0].x} ${startY} L ${pts[0].x} ${pts[0].y}`
    const r = Math.min(4, step / 3)
    for (let i = 1; i < pts.length; i++) {
      const a = pts[i - 1], b = pts[i]
      // 拐角用圆弧过渡（直角在 1.5px 细线上很生硬）
      d += ` L ${a.x + (b.x > a.x ? -r : r)} ${a.y} Q ${a.x} ${a.y} ${a.x} ${a.y + r}`
        + ` L ${a.x} ${b.y - r} Q ${a.x} ${b.y} ${a.x + (b.x > a.x ? r : -r)} ${b.y}`
    }
    // 收尾：在选中行心向右伸一小段，指进图标（与行内 elbow 的横线同一长度语义）
    d += ` L ${last.x + step / 2 + 1} ${last.y}`
    setPathD((prevD) => (prevD === d ? prevD : d))
  })

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
    <div className={`agent-tree at-${variant}`} role="tree" aria-label={t('left.agents')} ref={treeRef}>
      {/* 选中链：一条从会话行连到选中节点的折线（几何见上方 useLayoutEffect） */}
      {pathD && (
        <svg className="atree-path" aria-hidden="true">
          <path className="atree-path-line" d={pathD} />
        </svg>
      )}
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
            ref={(el) => {
              if (el) rowRefs.current.set(n.id, el)
              else rowRefs.current.delete(n.id)
            }}
            onClick={() => open(n)}
            title={n.label}
          >
            {/* 连线列：每层一个 guide（祖先还有后续兄弟则画竖线），最后一层画 ├ 或 └。
                缩进量与行高都交给 CSS → 行高变化/拖宽都不需要重算几何。 */}
            {n.guides.map((on, i) => (
              <span key={i} className={`atree-guide${on ? ' on' : ''}`} aria-hidden="true" />
            ))}
            <span className={`atree-elbow${n.last ? ' last' : ''}`} aria-hidden="true" />
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
