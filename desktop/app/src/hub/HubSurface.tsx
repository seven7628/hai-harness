import { useEffect, useMemo, useRef, useState } from 'react'
import { Markdown } from '../components/markdown'
import { Bot, SendIcon } from '../components/icons'
import { useT } from '../i18n'
import { attachHubEvents, useHubStore } from './useHubStore'
import AgentEditor from './AgentEditor'
import type { HubAgent, HubMessage } from './types'
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

  const [draft, setDraft] = useState('')
  // null = 关闭；'new' = 新建；HubAgent = 编辑该智能体。
  const [editing, setEditing] = useState<HubAgent | 'new' | null>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const stickBottom = useRef(true)

  useEffect(() => attachHubEvents(), [])
  useEffect(() => {
    void refresh()
  }, [refresh])

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

  function submit(): void {
    if (!draft.trim() || sending) return
    void send(draft)
    setDraft('')
    stickBottom.current = true
  }

  return (
    <div className="hub">
      <aside className="hub-rail" aria-label={t('hub.agents')}>
        <div className="hub-rail-head">
          <span className="hub-brand"><Bot size={16} /> {t('hub.title')}</span>
          <span className="hub-rail-actions">
            <button className="hub-link" onClick={() => setEditing('new')} title={t('hub.newAgent')}>＋</button>
            <button className="hub-link" onClick={closeHub} title={t('hub.backToWorkspaces')}>
              {t('hub.back')}
            </button>
          </span>
        </div>
        {agents.length === 0 && !loading ? (
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
          <div className="hub-error" role="alert" onClick={clearError} title={t('hub.dismiss')}>
            {error}
          </div>
        ) : null}
      </aside>

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
              <button className="hub-link" onClick={() => setEditing(activeAgent)} title={t('hub.editAgent')}>
                {t('hub.edit')}
              </button>
              <span className={`hub-dot ${isRunning ? 'on' : ''}`} title={isRunning ? t('hub.running') : t('hub.idle')} />
            </header>

            <div className="hub-list" ref={listRef} onScroll={onScroll}>
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
                {live ? (
                  <div className="hub-msg agent">
                    <div className="hub-body">
                      <Markdown text={live} streaming />
                    </div>
                  </div>
                ) : null}
                {isRunning && !live ? <div className="hub-muted">{t('hub.thinking')}</div> : null}
              </div>
            </div>

            <div className="hub-composer">
              <textarea
                value={draft}
                placeholder={t('hub.placeholder')}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault()
                    submit()
                  }
                }}
                rows={2}
              />
              <button className="hub-send" onClick={submit} disabled={sending || !draft.trim()} title={t('hub.send')}>
                <SendIcon />
              </button>
            </div>
          </>
        )}
      </section>

      {editing ? (
        <AgentEditor
          agent={editing === 'new' ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={(id) => {
            void refresh()
            if (id) void select(id)
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
