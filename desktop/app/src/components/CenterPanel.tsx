import { memo } from 'react'
import Narrative from './Narrative'
import Composer from './Composer'
import ApprovalPanel from './ApprovalPanel'
import AskUserQuestionPanel from './AskUserQuestionPanel'
import { AgentSessionView } from './AgentSessionView'
import { useAppStore, titleKey, skeletonFinishExit } from '../store/useAppStore'
import { useT } from '../i18n'

// 顶部「最近会话」横条（类似 VS Code 最近打开文件）：最近点开的 session 标签，
// 点击切换；⌘/Ctrl+1..5 快捷键切换（见 App.tsx 全局快捷键）。上限 5（与视图缓存一致）。
function RecentBar() {
  const t = useT()
  const recentSessions = useAppStore((s) => s.recentSessions)
  const activeSessionId = useAppStore((s) => s.activeSessionId)
  const workspace = useAppStore((s) => s.workspace)
  const projects = useAppStore((s) => s.projects)
  const titleOverrides = useAppStore((s) => s.titleOverrides)
  const setActiveSession = useAppStore((s) => s.setActiveSession)
  const removeRecentSession = useAppStore((s) => s.removeRecentSession)
  if (recentSessions.length === 0) return null
  // 标题：优先 titleOverrides，否则 project.sessions 里的 title，否则会话 id 短尾
  const titleOf = (path: string, id: string): string => {
    const key = titleKey(path, id)
    const ov = titleOverrides[key]
    if (ov) return ov
    const proj = projects.find((p) => p.path === path)
    const meta = proj?.sessions.find((x) => x.id === id)
    if (meta?.title) return meta.title
    return id.length > 12 ? `…${id.slice(-10)}` : id
  }
  return (
    <div className="recent-bar" role="tablist" aria-label={t('recent.sessions')}>
      {recentSessions.map((r, i) => {
        const active = r.id === activeSessionId && r.path === workspace
        const hotkey = i === 9 ? '⌘0' : `⌘${i + 1}` // 第 10 个用 ⌘0
        return (
          <button
            key={`${r.path}:${r.id}`}
            className={`recent-tab ${active ? 'active' : ''}`}
            role="tab"
            aria-selected={active}
            title={`${titleOf(r.path, r.id)} (${t('recent.switch')} ${hotkey})`}
            onClick={() => setActiveSession(r.id, r.path)}
          >
            <span className="recent-idx mono">{i === 9 ? '0' : i + 1}</span>
            <span className="recent-title">{titleOf(r.path, r.id)}</span>
            <span
              className="recent-close"
              role="button"
              tabIndex={0}
              title={t('recent.close')}
              aria-label={t('recent.close')}
              onClick={(e) => { e.stopPropagation(); removeRecentSession(r.path, r.id) }}
              onKeyDown={(e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault(); e.stopPropagation()
                  removeRecentSession(r.path, r.id)
                }
              }}
            >
              ✕
            </span>
          </button>
        )
      })}
    </div>
  )
}

// SessionSkeleton 切换会话时的骨架屏（替代原先的纯模糊遮罩）。
//
// 为什么改成骨架屏：原先只有一层 backdrop-blur + 右上角 spinner，用户看不出
// 「在等什么」，也未缓存会话最长要等 3s（见 setActiveSession 的兜底护栏）。
// 骨架屏给出「马上会出现一屏对话」的形状预期，等待感受更短。
//
// 为什么是「假」骨架：这里没有真实尺寸信息（会话还没加载），所以骨架按常见
// 对话形态铺：几条左右交替的气泡 + 工具行。刻意不做逐条脉动动画 —— 那是
// 20 个元素同时跑动画，长会话切换时白耗帧率；改为对整块做一次淡入 +
// 一条缓慢移动的高光带（compositor 合成层，几乎无成本）。
const SessionSkeleton = memo(function SessionSkeleton({
  label,
  leaving,
}: {
  label: string
  // leaving：数据已到位，正在淡出。此时下方真实内容已同时挂载并开始渐入，
  // 两者交叉淡化 —— 不给用户看「骨架消失 → 空白 → 内容出现」的三段跳。
  leaving?: boolean
}) {
  // 骨架的形状（2026-10-21 设计）。
  //
  // 设计约束，按重要性排列：
  //  1. **必须像对话**，不能像表格：左边一小段（用户提问）、右边长段落（回答）、
  //     偶尔一条细高条（工具调用）。用户一眼就知道「马上是一屏聊天记录」。
  //  2. **不能闪**：整块只有一条高光带在动 + 每行错峰显现，不做逐条脉动。
  //     脉动（skeleton pulse）在 20+ 元素上同时跑动画，长会话切换时白耗帧率，
  //     而且那种「呼吸感」在深色主题上显得廉价。
  //  3. **要有「历史正在铺开」的感觉**：行不是同时出现的，而是自上而下
  //     逐行显影（每行 40ms 错峰）—— 这正是用户说的「最近的对话历史缓慢展示」，
  //     只不过先展示的是「形状」，等数据到了再由真实内容接棒。
  //  4. 靠 @media (prefers-reduced-motion) 优雅降级为静态。
  const rows: { side: 'l' | 'r'; bars: (number | 'tool')[] }[] = [
    { side: 'l', bars: [46] },
    { side: 'r', bars: [92, 88, 64] },
    { side: 'r', bars: ['tool'] },
    { side: 'r', bars: [90, 94, 52] },
    { side: 'l', bars: [58] },
    { side: 'r', bars: ['tool', 76] },
    { side: 'r', bars: [88, 70] },
  ]
  return (
    <div
      className={`session-skeleton${leaving ? ' is-leaving' : ''}`}
      role="status"
      aria-label={label}
      aria-busy={!leaving}
      // 退场动画播完 → 通知 store 卸载。只在 leaving 时响应：进场动画
      // （ssFadeIn）与高光带的无限动画也会冒泡到 animationend，必须过滤。
      onAnimationEnd={(e) => {
        if (leaving && e.target === e.currentTarget) skeletonFinishExit()
      }}
    >
      <div className="ss-sheen" aria-hidden="true" />
      <div className="ss-rows" aria-hidden="true">
        {rows.map((r, i) => (
          <div
            key={i}
            className={`ss-row ss-${r.side === 'l' ? 'left' : 'right'}`}
            style={{ animationDelay: `${i * 40}ms` }}
          >
            {r.bars.map((w, j) =>
              w === 'tool' ? (
                <span key={j} className="ss-bar ss-bar-tool" style={{ width: '72%' }} />
              ) : (
                <span key={j} className="ss-bar" style={{ width: `${w}%` }} />
              ),
            )}
          </div>
        ))}
      </div>
    </div>
  )
})

export default function CenterPanel() {
  const t = useT()
  // 子智能体「子会话」：左栏树选中某个子 agent 时，叙述区换成它自己的转录/用量/TODO。
  // 这是**视图切换**而不是新会话（见 AgentSessionView 的注释）—— 顶部会话身份不变。
  const focused = useAppStore((s) => s.focusedAgent)
  // issue 4：切换会话时的短暂加载态 → 叙述区模糊遮罩（有缓存约一帧、无缓存等激活，超时兜底）
  // 骨架屏状态机：pending 时不渲染（还没到显示阈值），visible/exit 才渲染。
  //
  // pending 阶段刻意什么都不显示：那一刻内容还没到，但加载也很快（<120ms），
  // 用户视线里保持上一帧/空白，比闪一下骨架屏更平顺。
  const skeletonPhase = useAppStore((s) => s.skeleton.phase)
  const showSkeleton = skeletonPhase === 'visible' || skeletonPhase === 'exit'
  return (
    <section className="center">
      <RecentBar />
      <div className="narrative-wrap">
        {focused ? <AgentSessionView focus={focused} /> : <Narrative />}
        {showSkeleton && <SessionSkeleton label={t('common.loading')} leaving={skeletonPhase === 'exit'} />}
      </div>
      {/* 决策停靠区（HITL，非模态）：审批 + ask_user 提问都停靠在输入框上方 —— 待决策时叙述区
          照常滚动/选中/切会话，用户可以先翻对话历史、看清 diff 再拍板（用户 2026-09-20 报障）。 */}
      <ApprovalPanel />
      <AskUserQuestionPanel />
      <Composer />
    </section>
  )
}
