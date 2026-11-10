import { useEffect, useMemo, useRef, useState } from 'react'
import { Markdown } from '../components/markdown'
import { Bot, Plus, SendIcon } from '../components/icons'
import { useT } from '../i18n'
import { useHubStore } from './useHubStore'
import { useAppStore } from '../store/useAppStore'
import { alertSeverity } from './alerts'
import GroupView from './GroupView'
import AgentSigil from './AgentSigil'
import { agentSigilLabel } from './avatarLabels'
import { useAgentState } from './useAgentState'
import { isImeComposing } from './mentions'
import { canAutoFollow, highlightClass, scrollToSeq, seqDataAttr, seqJumpPlan } from './seqJump'
import type { HubMessage, HubRunEntry } from './types'
import './hub.css'

// Hub 中栏（P4-UI-1）：可嵌入 App 三栏布局的会话视图（不再自带 rail/inspector）。
//
// 左栏列表在 HubSidebar，右栏详情在 HubInspector；这里只管「当前 DM/群的对话」。
// 工作区 CenterPanel 在 App 里保持挂载（display:none），草稿与本地状态不因进出 Hub 丢失。
export default function HubSurface() {
  const t = useT()
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
  const agents = useHubStore((s) => s.agents)
  const select = useHubStore((s) => s.select)
  const loadOlder = useHubStore((s) => s.loadOlder)
  const send = useHubStore((s) => s.send)
  const refresh = useHubStore((s) => s.refresh)
  const clearError = useHubStore((s) => s.clearError)
  const drafts = useHubStore((s) => s.drafts)
  const setDraft = useHubStore((s) => s.setDraft)
  const failedText = useHubStore((s) => s.failedText)
  const retry = useHubStore((s) => s.retry)
  const notice = useHubStore((s) => s.notice)
  const clearNotice = useHubStore((s) => s.clearNotice)
  const jump = useHubStore((s) => s.jump)
  const copyDeepLink = useHubStore((s) => s.copyDeepLink)
  const openEditor = useHubStore((s) => s.openEditor)
  const closeHub = useHubStore((s) => s.closeHub)
  const inspector = useHubStore((s) => s.inspector)
  const setInspector = useHubStore((s) => s.setInspector)
  const setRightExpanded = useAppStore((s) => s.setRightExpanded)
  const rightExpanded = useAppStore((s) => s.rightExpanded)
  // 右栏折叠时，中栏的 Context / composer「+」也必须把右栏展开 —— 只改 inspector
  // 会让内容落在 44px 图标条后面（用户看不到任何变化）。
  // 折叠态下 Context 是「打开」而不是「关掉已选中的 Context」。
  const toggleContext = (): void => {
    setInspector(rightExpanded && inspector === 'context' ? null : 'context')
    setRightExpanded(true)
  }
  const showContext = (): void => {
    setInspector('context')
    setRightExpanded(true)
  }
  const draft = activeId ? (drafts[activeId] ?? '') : ''
  const view = useHubStore((s) => s.view)
  const setView = useHubStore((s) => s.setView)
  const runLog = useHubStore((s) => s.runLog)
  const actions = useHubStore((s) => s.actions)
  const tasks = useHubStore((s) => s.tasks)
  const alerts = useHubStore((s) => s.alerts)
  const dismissAlert = useHubStore((s) => s.dismissAlert)
  const retryTask = useHubStore((s) => s.retryTask)
  const groups = useHubStore((s) => s.groups)
  const activeGroupId = useHubStore((s) => s.activeGroupId)
  const activeGroup = groups.find((g) => g.id === activeGroupId) ?? null

  const listRef = useRef<HTMLDivElement>(null)
  const stickBottom = useRef(true)
  const [highlightSeq, setHighlightSeq] = useState<number | null>(null)
  const highlightTimer = useRef<number | null>(null)
  // 每个实例只消费每个 nonce 一次（同 GroupView：store 的 jump 留 TTL 给 StrictMode 第二实例）。
  const handledJump = useRef(0)

  useEffect(() => {
    void refresh()
    // 中途离开再回来：重开当前 target —— 回复可能是在没有订阅时落定的（B3），
    // 只 refresh() 会留下半截流式气泡与常亮运行点；群还要重新水合 conversation/
    // 已读游标（B7）。Hub 关闭期间 activeGroupId 仍保留，但事件按「未打开」计未读。
    // 深链刚打开 Hub 时这次重开会与深链自己的 select 换代（复审 B1）：
    // 是否重开由 store 的 deepLinkOwnedOpen 判定，组件不再自己看 activeGroupId/activeId。
    useHubStore.getState().resumeOpenTarget()
    // 只在挂载时跑一次（refresh 自身的引用变化不需要重开）。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 滚到底：只有用户本来就在底部时才自动跟随（翻历史时别把视图拽走）。
  useEffect(() => {
    const el = listRef.current
    if (!el || !stickBottom.current) return
    // 待执行的深链跳转优先：先贴底再跳会闪一下（跳转 effect 会接管滚动）。
    if (!canAutoFollow(jump, activeId)) return
    el.scrollTop = el.scrollHeight
  }, [messages.length, live, jump, activeId])

  // 深链 / Context 面板的触发 seq（P4-02）：只消费落在当前 DM 且消息已加载的请求。
  // 群走 GroupView 的同一条路径（targetId 不匹配时这里直接跳过）。
  useEffect(() => {
    const plan = seqJumpPlan(jump, { targetId: activeId, messages })
    if (!plan || handledJump.current === plan.nonce) return
    stickBottom.current = false // 别让「跟随最新」把定位顶回底部
    if (!scrollToSeq(listRef.current, plan.seq)) return
    handledJump.current = plan.nonce
    setHighlightSeq(plan.seq)
    if (highlightTimer.current != null) window.clearTimeout(highlightTimer.current)
    highlightTimer.current = window.setTimeout(() => setHighlightSeq(null), 1800)
    // 不在消费后立刻 clearJump：见 GroupView 同处注释（StrictMode 双跑会丢高亮）。
  }, [jump, activeId, messages])

  useEffect(() => () => {
    if (highlightTimer.current != null) window.clearTimeout(highlightTimer.current)
  }, [])

  const activeAgent = useMemo(
    () => agents.find((a) => a.id === activeId) ?? profile ?? null,
    [agents, activeId, profile],
  )
  const isRunning = activeId ? running[activeId] === true : false
  const activeState = useAgentState(activeId, activeAgent?.deleted)
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
    <div className="hub-center">
      {alerts.length > 0 ? (
        <div className="hub-alert-stack hub-center-alerts" role="region" aria-label="Hub 告警">
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
      {error ? (
        <div className="hub-error hub-center-banner" role="alert">
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
        <div className="hub-notice hub-center-banner" role="status">
          <span>{notice}</span>
          <button className="hub-link" onClick={clearNotice} aria-label={t('hub.dismiss')}>×</button>
        </div>
      ) : null}

      {activeGroup ? (
        <GroupView key={activeGroup.id} group={activeGroup} />
      ) : (
        <section className="hub-dm">
          {!activeAgent ? (
            <div className="hub-empty hub-empty-pick">
              <Bot size={30} />
              <p>{activeId ? t('hub.loading') : t('hub.pickAgent')}</p>
              {!activeId ? (
                <span className="hub-empty-actions">
                  <button className="hub-link" onClick={() => openEditor(null)}>
                    <Plus size={12} /> {t('hub.newAgent')}
                  </button>
                  <button className="hub-link" onClick={closeHub}>{t('hub.backToWorkspaces')}</button>
                </span>
              ) : null}
            </div>
          ) : (
            <>
              <header className="hub-dm-head">
                <AgentSigil
                  agentId={activeAgent.id}
                  responsibility={activeAgent.role}
                  state={activeState}
                  size={32}
                  label={agentSigilLabel(t, activeAgent.name, activeState)}
                />
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
                <button
                  className="hub-link"
                  onClick={() => void copyDeepLink('dm', activeAgent.id, latestSeq)}
                  title="复制这个会话的 hub:// 深链"
                >
                  链接
                </button>
                <button
                  className={`hub-link ${inspector === 'context' ? 'on' : ''}`}
                  onClick={toggleContext}
                  title="上下文（只读）：触发上下文与提示词分层"
                >
                  Context
                </button>
                <button className="hub-link" onClick={() => openEditor(activeAgent)} title={t('hub.editAgent')}>
                  {t('hub.edit')}
                </button>
                <span className={`hub-dot ${isRunning ? 'on' : ''}`} title={isRunning ? t('hub.running') : t('hub.idle')} />
              </header>

              <div className={`hub-list dm ${view === 'run' ? 'run' : ''}`} ref={listRef} onScroll={onScroll}>
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

              {view === 'chat' ? <div className="hub-composer hub-composer-pill">
                <button
                  className="hub-composer-plus"
                  onClick={showContext}
                  title="上下文与提示词（暂不支持附件）"
                  aria-label="打开上下文面板"
                >
                  <Plus size={14} />
                </button>
                <textarea
                  value={draft}
                  placeholder={`Message ${activeAgent.name}`}
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
    </div>
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
        <div className={`hub-system-note hub-system-action ${action}${hl}`} role="status" data-seq={seq}>
          <span className="hub-system-kind">{actionLabel(action)}</span>
          <span>{msg.text}</span>
        </div>
      )
    }
    return <div className={`hub-system-note${hl}`} data-seq={seq} role="status">{msg.text}</div>
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
