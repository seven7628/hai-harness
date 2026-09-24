import Narrative from './Narrative'
import Composer from './Composer'
import ApprovalPanel from './ApprovalPanel'
import AskUserQuestionPanel from './AskUserQuestionPanel'
import { useAppStore, titleKey } from '../store/useAppStore'
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

export default function CenterPanel() {
  const t = useT()
  // issue 4：切换会话时的短暂加载态 → 叙述区模糊遮罩（有缓存约一帧、无缓存等激活，超时兜底）
  const sessionLoading = useAppStore((s) => s.sessionLoading)
  return (
    <section className="center">
      <RecentBar />
      <div className="narrative-wrap">
        <Narrative />
        {sessionLoading && <div className="session-loading" role="status" aria-label={t('common.loading')} />}
      </div>
      {/* 决策停靠区（HITL，非模态）：审批 + ask_user 提问都停靠在输入框上方 —— 待决策时叙述区
          照常滚动/选中/切会话，用户可以先翻对话历史、看清 diff 再拍板（用户 2026-09-20 报障）。 */}
      <ApprovalPanel />
      <AskUserQuestionPanel />
      <Composer />
    </section>
  )
}
