import React, { useEffect, useMemo, useRef, useState } from 'react'
import { Markdown } from '../components/markdown'
import { Bot, SendIcon } from '../components/icons'
import { useT } from '../i18n'
import { useHubStore } from './useHubStore'
import AgentEditor from './AgentEditor'
import { alertSeverity } from './alerts'
import HubInspector from './HubInspector'
import GroupView from './GroupView'
import { isImeComposing } from './mentions'
import { formatRelativeTime, groupPreview, unreadBadge } from './conversation'
import { highlightClass, scrollToSeq, seqDataAttr, seqJumpPlan } from './seqJump'
import type { HubAgent, HubMessage, HubRunEntry } from './types'
import './hub.css'

// Hub surface：左栏 = 智能体 rail（§8.2），右栏 = DM 视图（§8.4）。
//
// 刻意不做三栏（工作区左栏/会话中栏/审阅右栏）：Hub 的 DM 是「和这个智能体的一条长期
// 对话」，不是运行日志；Run 视图（同一 session 的 Narrative）留到资料页/后续任务。
export default function HubSurface() {
  const t = useT()
  const agents = useHubStore((s) => s.agents)
  const dm = useHubStore((s) => s.dm)
  const activeId = useHubStore((s) => s.activeId)
  const profile = useHubStore((s) => s.profile)
  const messages = useHubStore((s) => s.messages)
  const hasMore = useHubStore((s) => s.hasMore)
  const loading = useHubStore((s) => s.loading)
  const loadingOlder = useHubStore((s) => s.loadingOlder)
  const sending = useHubStore((s) => s.sending)
  const error = useHubStore((s) => s.error)
  const live = useHubStore((s) => s.live)
  const running = useHubStore((s) => s.running)
  const select = useHubStore((s) => s.select)
  const loadOlder = useHubStore((s) => s.loadOlder)
  const send = useHubStore((s) => s.send)
  const refresh = useHubStore((s) => s.refresh)
  const closeHub = useHubStore((s) => s.closeHub)
  const clearError = useHubStore((s) => s.clearError)
  const drafts = useHubStore((s) => s.drafts)
  const setDraft = useHubStore((s) => s.setDraft)
  const failedText = useHubStore((s) => s.failedText)
  const retry = useHubStore((s) => s.retry)
  const clearSelection = useHubStore((s) => s.clearSelection)
  const notice = useHubStore((s) => s.notice)
  const clearNotice = useHubStore((s) => s.clearNotice)
  const jump = useHubStore((s) => s.jump)
  const clearJump = useHubStore((s) => s.clearJump)
  const copyDeepLink = useHubStore((s) => s.copyDeepLink)
  const draft = activeId ? (drafts[activeId] ?? '') : ''
  const inspector = useHubStore((s) => s.inspector)
  const setInspector = useHubStore((s) => s.setInspector)
  const view = useHubStore((s) => s.view)
  const setView = useHubStore((s) => s.setView)
  const runLog = useHubStore((s) => s.runLog)
  const actions = useHubStore((s) => s.actions)
  const tasks = useHubStore((s) => s.tasks)
  const needs = useHubStore((s) => s.needs)
  const alerts = useHubStore((s) => s.alerts)
  const dismissAlert = useHubStore((s) => s.dismissAlert)
  const retryTask = useHubStore((s) => s.retryTask)
  const groups = useHubStore((s) => s.groups)
  const groupsLoading = useHubStore((s) => s.groupsLoading)
  const groupInfo = useHubStore((s) => s.groupInfo)
  const activeGroupId = useHubStore((s) => s.activeGroupId)
  const selectGroup = useHubStore((s) => s.selectGroup)
  const activeGroup = groups.find((g) => g.id === activeGroupId) ?? null
  const [railTab, setRailTab] = React.useState<'agents' | 'groups'>('agents')
  const activeTasks = tasks.filter((t) => !['completed', 'failed', 'interrupted', 'delivered', 'canceled', 'blocked_budget', 'declined'].includes(t.status ?? '')).length

  // null = 关闭；'new' = 新建；HubAgent = 编辑该智能体。
  const [editing, setEditing] = useState<HubAgent | 'new' | null>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const stickBottom = useRef(true)
  const [highlightSeq, setHighlightSeq] = useState<number | null>(null)
  const highlightTimer = useRef<number | null>(null)

  useEffect(() => {
    void refresh()
    // 中途离开再回来：重开当前 target —— 回复可能是在没有订阅时落定的（B3），
    // 只 refresh() 会留下半截流式气泡与常亮运行点；群还要重新水合 conversation/
    // 已读游标（B7）。Hub 关闭期间 activeGroupId 仍保留，但事件按「未打开」计未读。
    const state = useHubStore.getState()
    if (state.activeGroupId) void selectGroup(state.activeGroupId)
    else if (state.activeId) void select(state.activeId)
    // 只在挂载时跑一次（select/selectGroup/refresh 自身的引用变化不需要重开）。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 滚到底：只有用户本来就在底部时才自动跟随（翻历史时别把视图拽走）。
  useEffect(() => {
    const el = listRef.current
    if (!el || !stickBottom.current) return
    // 待执行的深链跳转优先：先贴底再跳会闪一下（跳转 effect 会接管滚动）。
    if (jump && jump.targetId === activeId) return
    el.scrollTop = el.scrollHeight
  }, [messages.length, live, jump, activeId])

  // 深链 / Context 面板的触发 seq（P4-02）：只消费落在当前 DM 且消息已加载的请求。
  // 群走 GroupView 的同一条路径（targetId 不匹配时这里直接跳过）。
  useEffect(() => {
    const plan = seqJumpPlan(jump, { targetId: activeId, messages })
    if (!plan) return
    stickBottom.current = false // 别让「跟随最新」把定位顶回底部
    if (!scrollToSeq(listRef.current, plan.seq)) return
    setHighlightSeq(plan.seq)
    clearJump(plan.nonce)
    if (highlightTimer.current != null) window.clearTimeout(highlightTimer.current)
    highlightTimer.current = window.setTimeout(() => setHighlightSeq(null), 1800)
  }, [jump, activeId, messages, clearJump])

  useEffect(() => () => {
    if (highlightTimer.current != null) window.clearTimeout(highlightTimer.current)
  }, [])

  const activeAgent = useMemo(
    () => agents.find((a) => a.id === activeId) ?? profile ?? null,
    [agents, activeId, profile],
  )
  const isRunning = activeId ? running[activeId] === true : false
  // 深链指向「最新一条」：seq 最大的已落盘消息（跳转高亮由深链的 seq 决定）。
  const latestSeq = useMemo(
    () => messages.reduce((max, m) => (Number(m.seq) > max ? Number(m.seq) : max), 0),
    [messages],
  )

  function onScroll(): void {
    const el = listRef.current
    if (!el) return
    stickBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80
    if (el.scrollTop < 60 && hasMore && !loadingOlder) void loadOlder()
  }

  async function submit(): Promise<void> {
    if (!draft.trim() || sending || !activeId) return
    stickBottom.current = true
    const ok = await send(draft)
    // 成功才清草稿；失败保留原文（store 同时把它放进 failedText，气泡标 failed）。
    if (ok) setDraft(activeId, '')
  }

  return (
    <div className="hub">
      <aside className="hub-rail" aria-label={t('hub.agents')}>
        <div className="hub-rail-head">
          <span className="hub-brand"><Bot size={16} /> {t('hub.title')}</span>
          <span className="hub-rail-actions">
            <button
              className={`hub-link ${inspector === 'tasks' ? 'on' : ''}`}
              onClick={() => setInspector(inspector === 'tasks' ? null : 'tasks')}
              title="任务板"
            >
              任务{activeTasks > 0 ? <span className="hub-badge">{activeTasks}</span> : null}
            </button>
            <button
              className={`hub-link ${inspector === 'needs' ? 'on' : ''}`}
              onClick={() => setInspector(inspector === 'needs' ? null : 'needs')}
              title="Needs you"
            >
              Needs{needs.length > 0 ? <span className="hub-badge alert">{needs.length}</span> : null}
            </button>
            <button className="hub-link" onClick={() => setEditing('new')} title={t('hub.newAgent')}>＋</button>
            <button className="hub-link" onClick={closeHub} title={t('hub.backToWorkspaces')}>
              {t('hub.back')}
            </button>
          </span>
        </div>
        {alerts.length > 0 ? (
          <div className="hub-alert-stack" role="region" aria-label="Hub 告警">
            {alerts.map((a) => {
              const task = tasks.find((t) => t.id === a.task_id)
              // 全局恢复失败没有 task/agent：退回当前 DM 或任务板上任一发起者，
              // 让 hub_open 再触发一次恢复（不是空动作，P2.5-05 复审 B3）。
              const agentID = a.agent_id || task?.origin?.agent_id || activeId || tasks[0]?.origin?.agent_id
              return (
                <div key={a.id} className={`hub-alert-row ${alertSeverity(a.severity)}`}>
                  <span className="hub-alert-dot" title={a.severity ?? 'warning'} />
                  <span className="hub-alert-text" title={a.message}>{a.message || a.code}</span>
                  {a.task_id ? <span className="hub-sub">{a.task_id}</span> : null}
                  {a.code === 'recovery_failed' ? (
                    a.task_id ? (
                      <button className="hub-link" onClick={() => void retryTask(a.task_id!)}>重试</button>
                    ) : agentID ? (
                      <button className="hub-link" onClick={() => void select(agentID)}>重试</button>
                    ) : null
                  ) : null}
                  <button className="hub-link" onClick={() => dismissAlert(a.id)} aria-label="忽略告警">×</button>
                </div>
              )
            })}
          </div>
        ) : null}
        <div className="hub-rail-tabs" role="tablist">
          <button role="tab" aria-selected={railTab === 'agents'} className={railTab === 'agents' ? 'on' : ''} onClick={() => setRailTab('agents')}>智能体</button>
          <button role="tab" aria-selected={railTab === 'groups'} className={railTab === 'groups' ? 'on' : ''} onClick={() => setRailTab('groups')}>群组</button>
        </div>
        {railTab === 'groups' ? (
          groupsLoading ? <div className="hub-rail-empty">加载中…</div> :
          groups.length === 0 ? <div className="hub-rail-empty">还没有群组</div> : (
            <div className="hub-rail-list">
              {groups.map((g) => {
                const info = groupInfo[g.id] ?? {}
                const unread = info.unread ?? 0
                const memberCount = (g.members ?? []).length
                // 预览优先任务结论（「做完了什么」），否则最后一条消息；都没有才退回成员/策略。
                const preview = groupPreview(info) || `${memberCount} 名成员 · ${g.policy?.mode ?? 'ask'}`
                const time = formatRelativeTime(info.last_at)
                return (
                  <button
                    key={g.id}
                    className={`hub-row hub-group-row ${g.id === activeGroupId ? 'on' : ''}`}
                    onClick={() => void selectGroup(g.id)}
                    aria-label={`${g.name}，${memberCount} 名成员${unread > 0 ? `，${unreadBadge(unread)} 条未读` : '，没有未读'}，${preview}`}
                  >
                    <span className="hub-member-stack" aria-hidden="true">
                      {(g.members ?? []).slice(0, 3).map((m, i) => (
                        <span key={m.agent_id} className="hub-member-dot" style={{ zIndex: 3 - i }}>
                          {(agents.find((a) => a.id === m.agent_id)?.name ?? m.agent_id).slice(0, 1)}
                        </span>
                      ))}
                    </span>
                    <span className="hub-row-text">
                      <span className="hub-row-top">
                        <span className="hub-name">{g.name}</span>
                        {unread > 0 ? <span className="hub-unread">{unreadBadge(unread)}</span> : null}
                      </span>
                      <span className="hub-preview" title={preview}>{preview}</span>
                    </span>
                    {time ? <span className="hub-row-time">{time}</span> : null}
                  </button>
                )
              })}
            </div>
          )
        ) : agents.length === 0 && !loading ? (
          <div className="hub-rail-empty">{t('hub.noAgents')}</div>
        ) : (
          <div className="hub-rail-list">
            {agents.map((a) => (
              <HubRow
                key={a.id}
                agent={a}
                preview={dm[a.id]?.last_text}
                unread={dm[a.id]?.unread ?? 0}
                running={running[a.id] === true}
                active={a.id === activeId}
                onClick={() => void select(a.id)}
              />
            ))}
          </div>
        )}
        {error ? (
          <div className="hub-error" role="alert">
            <span>{error}</span>
            {failedText ? (
              <button className="hub-link" onClick={() => void retry()} disabled={sending}>
                {t('hub.retry')}
              </button>
            ) : null}
            <button className="hub-link" onClick={clearError} aria-label={t('hub.dismiss')}>×</button>
          </div>
        ) : null}
        {notice ? (
          <div className="hub-notice" role="status">
            <span>{notice}</span>
            <button className="hub-link" onClick={clearNotice} aria-label={t('hub.dismiss')}>×</button>
          </div>
        ) : null}
      </aside>

      {activeGroup ? (
        <GroupView key={activeGroup.id} group={activeGroup} />
      ) : (
      <section className="hub-dm">
        {!activeAgent ? (
          <div className="hub-empty">
            <Bot size={28} />
            <p>{t('hub.pickAgent')}</p>
          </div>
        ) : (
          <>
            <header className="hub-dm-head">
              <HubAvatar agent={activeAgent} size={26} />
              <div className="hub-dm-title">
                <strong>{activeAgent.name}</strong>
                <span className="hub-sub">
                  {[activeAgent.role, [activeAgent.provider, activeAgent.model].filter(Boolean).join(' · ')]
                    .filter(Boolean)
                    .join(' — ')}
                </span>
              </div>
              <span className="hub-view-toggle" role="tablist" aria-label="Chat or Run">
                <button role="tab" aria-selected={view === 'chat'} className={view === 'chat' ? 'on' : ''} onClick={() => setView('chat')}>Chat</button>
                <button role="tab" aria-selected={view === 'run'} className={view === 'run' ? 'on' : ''} onClick={() => setView('run')}>Run</button>
              </span>
              <button className="hub-link" onClick={() => setInspector(inspector === 'session' ? null : 'session')} title="会话信息">会话</button>
              <button
                className={`hub-link ${inspector === 'context' ? 'on' : ''}`}
                onClick={() => setInspector(inspector === 'context' ? null : 'context')}
                title="上下文（只读）：触发上下文与提示词分层"
              >
                Context
              </button>
              <button
                className="hub-link"
                onClick={() => void copyDeepLink('dm', activeAgent.id, latestSeq)}
                title="复制这个会话的 hub:// 深链"
              >
                链接
              </button>
              <button className="hub-link" onClick={() => setEditing(activeAgent)} title={t('hub.editAgent')}>
                {t('hub.edit')}
              </button>
              <span className={`hub-dot ${isRunning ? 'on' : ''}`} title={isRunning ? t('hub.running') : t('hub.idle')} />
            </header>

            <div className={`hub-list ${view === 'run' ? 'run' : ''}`} ref={listRef} onScroll={onScroll}>
              {view === 'run' ? (
                <RunView entries={runLog} running={isRunning} />
              ) : (
              <>
              {loading ? <div className="hub-muted">{t('hub.loading')}</div> : null}
              {!loading && messages.length === 0 && !live ? (
                <div className="hub-intro">
                  <p>
                    {activeAgent.description || activeAgent.role || t('hub.introFallback')}
                  </p>
                  <span className="hub-muted">{t('hub.introHint')}</span>
                </div>
              ) : null}
              <div className="hub-thread">
                {messages.map((m) => (
                  <HubBubble key={`${m.seq}-${m.clientMsgId ?? ''}`} msg={m} highlightSeq={highlightSeq} />
                ))}
                {actions.map((m) => (
                  <HubBubble key={`action-${m.seq}-${m.text ?? ''}`} msg={m} highlightSeq={highlightSeq} />
                ))}
                {live ? (
                  <div className="hub-msg agent">
                    <div className="hub-body">
                      <Markdown text={live} streaming />
                    </div>
                  </div>
                ) : null}
                {isRunning && !live ? <div className="hub-muted">{t('hub.thinking')}</div> : null}
              </div>
              </>
              )}
            </div>

            {view === 'chat' ? <div className="hub-composer">
              <textarea
                value={draft}
                placeholder={t('hub.placeholder')}
                onChange={(e) => activeId && setDraft(activeId, e.target.value)}
                onKeyDown={(e) => {
                  // DM 与群 composer 同口径：IME 合成中的 Enter 属于输入法（B2）。
                  if (isImeComposing(e.nativeEvent, e.keyCode)) return
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault()
                    submit()
                  }
                }}
                rows={2}
              />
              <button
                className="hub-send"
                onClick={() => void submit()}
                disabled={sending || !draft.trim()}
                title={t('hub.send')}
                aria-label={t('hub.send')}
              >
                <SendIcon />
              </button>
            </div> : null}
          </>
        )}
      </section>
      )}

      <HubInspector />

      {editing ? (
        <AgentEditor
          agent={editing === 'new' ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={(id) => {
            void refresh()
            if (id) void select(id)
            else clearSelection()
          }}
        />
      ) : null}
    </div>
  )
}

function HubRow({
  agent,
  preview,
  unread,
  running,
  active,
  onClick,
}: {
  agent: HubAgent
  preview?: string
  unread: number
  running: boolean
  active: boolean
  onClick: () => void
}) {
  return (
    <button className={`hub-row ${active ? 'on' : ''}`} onClick={onClick}>
      <HubAvatar agent={agent} size={30} ring={running} />
      <span className="hub-row-text">
        <span className="hub-row-top">
          <span className="hub-name">{agent.name}</span>
          {unread > 0 ? <span className="hub-unread">{unread > 99 ? '99+' : unread}</span> : null}
        </span>
        <span className="hub-preview">{preview || agent.role || '—'}</span>
      </span>
    </button>
  )
}

function HubAvatar({ agent, size, ring }: { agent: HubAgent; size: number; ring?: boolean }) {
  const accent = agent.accent || '#b8492f'
  const style = { width: size, height: size, background: `${accent}22`, borderColor: accent, color: accent }
  const emoji = agent.avatar?.kind === 'emoji' ? agent.avatar.value : ''
  return (
    <span className={`hub-avatar ${ring ? 'ring' : ''}`} style={{ ...style, fontSize: Math.round(size * 0.5) }}>
      {emoji || agent.name.slice(0, 1)}
    </span>
  )
}

function HubBubble({ msg, highlightSeq }: { msg: HubMessage; highlightSeq?: number | null }) {
  const kind = msg.sender?.kind ?? 'system'
  const seq = seqDataAttr(msg.seq)
  const hl = highlightClass(msg.seq, highlightSeq)
  if (kind === 'system') {
    // 系统动作行：refs.action 存在时按「动作 × 文本」渲染，与普通系统提示区分开。
    const action = typeof msg.refs?.action === 'string' ? msg.refs.action : ''
    if (action) {
      return (
        <div className={`hub-action ${action}${hl}`} role="status" data-seq={seq}>
          <span className="hub-action-kind">{actionLabel(action)}</span>
          <span>{msg.text}</span>
        </div>
      )
    }
    return <div className={`hub-msg system${hl}`} data-seq={seq}>{msg.text}</div>
  }
  return (
    <div className={`hub-msg ${kind === 'human' ? 'human' : 'agent'}${hl}`} data-seq={seq}>
      <div className="hub-body">
        <Markdown text={msg.text ?? ''} />
      </div>
    </div>
  )
}


/** Run 视图：同一会话的工具/压缩运行日志（Chat 之外的运行态落地）。 */
function RunView({ entries, running }: { entries: HubRunEntry[]; running: boolean }) {
  if (entries.length === 0) {
    return (
      <div className="hub-empty">
        <p>{running ? '正在运行…' : '还没有运行记录'}</p>
        <span className="hub-muted">工具调用与上下文压缩会显示在这里</span>
      </div>
    )
  }
  return (
    <div className="hub-run">
      {entries.map((e) => (
        <div key={e.id} className={`hub-run-row ${e.status ?? ''}`}>
          <span className="hub-chip">{e.kind === 'compress' ? '压缩' : '工具'}</span>
          <span className="hub-run-name">{e.name || e.kind}</span>
          {e.summary ? <span className="hub-muted">{e.summary}</span> : null}
          <span className="hub-muted">{e.status === 'running' ? '运行中' : e.status === 'error' ? '失败' : e.ms ? `${(e.ms / 1000).toFixed(1)}s` : '完成'}</span>
        </div>
      ))}
    </div>
  )
}

function actionLabel(action: string): string {
  const map: Record<string, string> = {
    compress: '压缩', clear: '清空', retry: '重试', interrupt: '中断', task: '任务',
  }
  return map[action] ?? action
}
