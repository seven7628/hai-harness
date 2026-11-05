import React, { useEffect, useMemo, useRef, useState } from 'react'
import { Markdown } from '../components/markdown'
import { Bot, SendIcon } from '../components/icons'
import { useT } from '../i18n'
import { attachHubEvents, useHubStore } from './useHubStore'
import AgentEditor from './AgentEditor'
import HubInspector from './HubInspector'
import GroupView from './GroupView'
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
  const draft = activeId ? (drafts[activeId] ?? '') : ''
  const inspector = useHubStore((s) => s.inspector)
  const setInspector = useHubStore((s) => s.setInspector)
  const view = useHubStore((s) => s.view)
  const setView = useHubStore((s) => s.setView)
  const runLog = useHubStore((s) => s.runLog)
  const actions = useHubStore((s) => s.actions)
  const tasks = useHubStore((s) => s.tasks)
  const needs = useHubStore((s) => s.needs)
  const groups = useHubStore((s) => s.groups)
  const groupsLoading = useHubStore((s) => s.groupsLoading)
  const activeGroupId = useHubStore((s) => s.activeGroupId)
  const selectGroup = useHubStore((s) => s.selectGroup)
  const activeGroup = groups.find((g) => g.id === activeGroupId) ?? null
  const [railTab, setRailTab] = React.useState<'agents' | 'groups'>('agents')
  const activeTasks = tasks.filter((t) => !['completed', 'failed', 'interrupted', 'delivered', 'canceled', 'blocked_budget', 'declined'].includes(t.status ?? '')).length

  // null = 关闭；'new' = 新建；HubAgent = 编辑该智能体。
  const [editing, setEditing] = useState<HubAgent | 'new' | null>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const stickBottom = useRef(true)

  useEffect(() => attachHubEvents(), [])
  useEffect(() => {
    void refresh()
    // 中途离开再回来：重开当前 DM —— 回复可能是在没有订阅时落定的（B3），
    // 只 refresh() 会留下半截流式气泡与常亮运行点。
    const id = useHubStore.getState().activeId
    if (id) void select(id)
    // 只在挂载时跑一次（select/refresh 自身的引用变化不需要重开）。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 滚到底：只有用户本来就在底部时才自动跟随（翻历史时别把视图拽走）。
  useEffect(() => {
    const el = listRef.current
    if (!el || !stickBottom.current) return
    el.scrollTop = el.scrollHeight
  }, [messages.length, live])

  const activeAgent = useMemo(
    () => agents.find((a) => a.id === activeId) ?? profile ?? null,
    [agents, activeId, profile],
  )
  const isRunning = activeId ? running[activeId] === true : false

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
        <div className="hub-rail-tabs" role="tablist">
          <button role="tab" aria-selected={railTab === 'agents'} className={railTab === 'agents' ? 'on' : ''} onClick={() => setRailTab('agents')}>智能体</button>
          <button role="tab" aria-selected={railTab === 'groups'} className={railTab === 'groups' ? 'on' : ''} onClick={() => setRailTab('groups')}>群组</button>
        </div>
        {railTab === 'groups' ? (
          groupsLoading ? <div className="hub-rail-empty">加载中…</div> :
          groups.length === 0 ? <div className="hub-rail-empty">还没有群组</div> : (
            <div className="hub-rail-list">
              {groups.map((g) => (
                <button key={g.id} className={`hub-row ${g.id === activeGroupId ? 'on' : ''}`} onClick={() => void selectGroup(g.id)}>
                  <span className="hub-member-stack">
                    {(g.members ?? []).slice(0, 3).map((m, i) => (
                      <span key={m.agent_id} className="hub-member-dot" style={{ zIndex: 3 - i }}>
                        {(agents.find((a) => a.id === m.agent_id)?.name ?? m.agent_id).slice(0, 1)}
                      </span>
                    ))}
                  </span>
                  <span className="hub-row-text">
                    <span className="hub-row-top"><span className="hub-name">{g.name}</span></span>
                    <span className="hub-preview">{(g.members ?? []).length} 名成员 · {g.policy?.mode ?? 'ask'}</span>
                  </span>
                </button>
              ))}
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
        <GroupView group={activeGroup} />
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
                  <HubBubble key={`${m.seq}-${m.clientMsgId ?? ''}`} msg={m} />
                ))}
                {actions.map((m) => (
                  <HubBubble key={`action-${m.seq}-${m.text ?? ''}`} msg={m} />
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

      {activeGroup ? null : <HubInspector />}

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

function HubBubble({ msg }: { msg: HubMessage }) {
  const kind = msg.sender?.kind ?? 'system'
  if (kind === 'system') {
    // 系统动作行：refs.action 存在时按「动作 × 文本」渲染，与普通系统提示区分开。
    const action = typeof msg.refs?.action === 'string' ? msg.refs.action : ''
    if (action) {
      return (
        <div className={`hub-action ${action}`} role="status">
          <span className="hub-action-kind">{actionLabel(action)}</span>
          <span>{msg.text}</span>
        </div>
      )
    }
    return <div className="hub-msg system">{msg.text}</div>
  }
  return (
    <div className={`hub-msg ${kind === 'human' ? 'human' : 'agent'}`}>
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
