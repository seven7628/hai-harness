import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Markdown } from '../components/markdown'
import { AccessIcon, Bot, LayersIcon, MoreIcon, PaperclipIcon, Plus, SendIcon, TerminalIcon } from '../components/icons'
import { useLang, useT } from '../i18n'
import { useHubStore } from './useHubStore'
import { useAppStore } from '../store/useAppStore'
import { alertSeverity } from './alerts'
import GroupView from './GroupView'
import HubErrorBanner from './HubErrorBanner'
import AgentSigil from './AgentSigil'
import { AgentMessage, DayDivider, UserMessage } from './ConversationMessage'
import { dayKey, dayLabel } from './conversationDay'
import { formatClockTime } from './conversation'
import { agentSigilLabel } from './avatarLabels'
import { useAgentState } from './useAgentState'
import { isImeComposing, type MentionAgent } from './mentions'
import { canAutoFollow, highlightClass, scrollToSeq, seqDataAttr, seqJumpPlan } from './seqJump'
import type { HubMessage, HubRunEntry } from './types'
import { useResolvedTheme } from '../lib/useTheme'
import './hub.css'

// Hub 中栏（P4-UI-1）：可嵌入 App 三栏布局的会话视图（不再自带 rail/inspector）。
//
// 左栏列表在 HubSidebar，右栏详情在 HubInspector；这里只管「当前 DM/群的对话」。
// 工作区 CenterPanel 在 App 里保持挂载（display:none），草稿与本地状态不因进出 Hub 丢失。
export default function HubSurface() {
  const t = useT()
  const lang = useLang()
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
  const failed = useHubStore((s) => s.failed)
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
  const groupSending = useHubStore((s) => s.groupSending)
  const activeGroup = groups.find((g) => g.id === activeGroupId) ?? null
  // 重试只对「当前目标的那次失败」开放：失败记录跨会话留着会把消息发错人。
  const canRetryFailed = !!failed && (failed.kind === 'group'
    ? failed.groupId === activeGroupId
    : failed.agentId === activeId)

  const listRef = useRef<HTMLDivElement>(null)
  const stickBottom = useRef(true)
  const [highlightSeq, setHighlightSeq] = useState<number | null>(null)
  const [menuOpen, setMenuOpen] = useState(false)
  const theme = useResolvedTheme()
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
    // 成功才清草稿；失败保留原文（store 同时留 failed 记录，气泡标 failed 并带行内重试）。
    if (ok) setDraft(activeId, '')
  }

  // 时间线节点：日期分隔线按本地自然日切段（与群共用 conversationDay.ts 的口径）。
  // actions 是运行时动作回执；没有 created_at 时不分段，不造时间。
  const dmThread = useMemo(() => {
    if (!activeAgent) return []
    const nodes: ReactNode[] = []
    let lastDay = ''
    const pushDay = (m: HubMessage): void => {
      const key = dayKey(m.created_at)
      if (!key || key === lastDay) return
      lastDay = key
      nodes.push(<DayDivider key={`day-${key}`} label={dayLabel(m.created_at, lang)} />)
    }
    for (const m of messages) {
      pushDay(m)
      nodes.push(
        <HubBubble
          key={`${m.seq}-${m.clientMsgId ?? ''}`}
          msg={m}
          agent={activeAgent}
          highlightSeq={highlightSeq}
        />,
      )
    }
    for (const m of actions) {
      pushDay(m)
      nodes.push(<HubBubble key={`action-${m.seq}-${m.text ?? ''}`} msg={m} agent={activeAgent} highlightSeq={highlightSeq} />)
    }
    return nodes
  }, [messages, actions, activeAgent, highlightSeq, lang])

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
        <HubErrorBanner
          message={error}
          canRetry={canRetryFailed}
          busy={sending || groupSending}
          onRetry={() => void retry()}
          onDismiss={clearError}
        />
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
        <section className="main hub-dm hub-demo" data-theme={theme === 'paper' ? 'light' : 'dark'}>
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
              <header className="chat-header">
                <AgentSigil
                  agentId={activeAgent.id}
                  name={activeAgent.name}
                  responsibility={activeAgent.role}
                  state={activeState}
                  size={52}
                  label={agentSigilLabel(t, activeAgent.name, activeState)}
                />
                <div className="chat-title-wrap">
                  <div className="chat-title">
                    <span className="name">{activeAgent.name}</span>
                    {isRunning ? <span className="dot live" title={t('hub.running')} /> : null}
                  </div>
                  <div className="chat-sub">
                    {activeAgent.role ? <span>{activeAgent.role}</span> : null}
                    {activeAgent.role ? <span className="sep">·</span> : null}
                    <span>{[activeAgent.provider, activeAgent.model].filter(Boolean).join(' · ') || t('hub.idle')}</span>
                    <span className="sep">·</span>
                    <span className="lock" title={t('hub.dm.lockedHint')}>
                      <AccessIcon size={11} /> {t('hub.dm.locked')}
                    </span>
                  </div>
                </div>
                <div className="header-actions">
                  <button
                    type="button"
                    className={`icon-btn${inspector === 'context' ? ' active' : ''}`}
                    onClick={toggleContext}
                    title="上下文（只读）：触发上下文与提示词分层"
                    aria-label="上下文"
                  >
                    <LayersIcon size={15} />
                  </button>
                  <button
                    type="button"
                    className="icon-btn"
                    onClick={() => setMenuOpen((v) => !v)}
                    aria-label="更多操作"
                    aria-expanded={menuOpen}
                  >
                    <MoreIcon size={15} />
                  </button>
                  {menuOpen ? (
                    <div className="header-menu" role="menu">
                      <button type="button" role="menuitemradio" aria-checked={view === 'chat'} onClick={() => { setView('chat'); setMenuOpen(false) }}>{t('hub.header.chat')}</button>
                      <button type="button" role="menuitemradio" aria-checked={view === 'run'} onClick={() => { setView('run'); setMenuOpen(false) }}>{t('hub.header.run')}</button>
                      <span className="header-menu-sep" />
                      <button type="button" role="menuitem" onClick={() => { setMenuOpen(false); toggleContext() }}>{t('hub.header.context')}</button>
                      <button type="button" role="menuitem" onClick={() => { setMenuOpen(false); void copyDeepLink('dm', activeAgent.id, latestSeq) }}>{t('hub.header.link')}</button>
                      <button type="button" role="menuitem" onClick={() => { setMenuOpen(false); openEditor(activeAgent) }}>{t('hub.edit')}</button>
                    </div>
                  ) : null}
                </div>
              </header>

              <div className={`messages dm ${view === 'run' ? 'run' : ''}`} ref={listRef} onScroll={onScroll}>
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
                <div className="messages-inner">
                  {dmThread}
                  {live ? (
                    <AgentMessage
                      agent={activeAgent}
                      name={activeAgent.name}
                      role={activeAgent.role}
                      tags={<span className="agent-tag reply">{t('hub.streaming')}</span>}
                      className="streaming"
                    >
                      <Markdown text={live} streaming />
                    </AgentMessage>
                  ) : null}
                  {isRunning && !live ? <div className="system-line"><span>{t('hub.thinking')}</span></div> : null}
                </div>
                </>
                )}
              </div>

              {view === 'chat' ? (
                <div className="composer-wrap"><div className="composer-inner">
                  <div className="composer">
                    <button
                      className="icon-btn"
                      onClick={showContext}
                      title="上下文与提示词（暂不支持附件）"
                      aria-label="打开上下文面板"
                    >
                      <PaperclipIcon size={15} />
                    </button>
                    <textarea
                      value={draft}
                      placeholder={t('hub.sendTo').replace('{name}', activeAgent.name)}
                      onChange={(e) => activeId && setDraft(activeId, e.target.value)}
                      onKeyDown={(e) => {
                        // DM 与群 composer 同口径：IME 合成中的 Enter 属于输入法（B2）。
                        if (isImeComposing(e.nativeEvent, e.keyCode)) return
                        if (e.key === 'Enter' && !e.shiftKey) {
                          e.preventDefault()
                          submit()
                        }
                      }}
                      rows={1}
                    />
                    <button
                      className="send-btn"
                      onClick={() => void submit()}
                      disabled={sending || !draft.trim()}
                      title={t('hub.send')}
                      aria-label={t('hub.send')}
                    >
                      <SendIcon />
                    </button>
                  </div>
                  <div className="composer-meta">
                    <span className="composer-hint">
                      <kbd>Enter</kbd> {t('hub.composer.enter')} · <kbd>Shift</kbd>+<kbd>Enter</kbd> {t('hub.composer.newline')}
                    </span>
                    <span className="meta-links">
                      <button type="button" onClick={showContext}>它看到了什么</button>
                    </span>
                  </div>
                </div>
                </div>
              ) : null}
            </>
          )}
        </section>
      )}
    </div>
  )
}

function HubBubble({
  msg,
  agent,
  highlightSeq,
}: {
  msg: HubMessage
  agent?: MentionAgent | null
  highlightSeq?: number | null
}) {
  const t = useT()
  // 失败气泡的行内「重试」只认这条自己的 cid：两条失败时点 A 必须发 A，不能被
  // 「最新一条失败」顶掉（复审 B1）。按布尔订阅，别的消息不因失败表变化而重渲染。
  const canRetryThis = useHubStore((s) => !!msg.clientMsgId && !!s.failedByCid[msg.clientMsgId])
  const retry = useHubStore((s) => s.retry)
  // 正在发送时行内「重试」置灰：横幅同一时刻已是 busy，气泡按钮不能还显示可点
  // （P1 复审非阻塞）。store.retry 本身也会拒绝并发重试，这里只对齐视觉与指针。
  const sending = useHubStore((s) => s.sending)
  const kind = msg.sender?.kind ?? 'system'
  const seq = seqDataAttr(msg.seq)
  const hl = highlightClass(msg.seq, highlightSeq)
  if (kind === 'system') {
    // 系统动作行：refs.action 存在时按「动作 × 文本」渲染，与普通系统提示区分开。
    const action = typeof msg.refs?.action === 'string' ? msg.refs.action : ''
    if (action) {
      return (
        <div className={`system-line hub-system-action ${action}${hl}`} role="status" data-seq={seq}>
          <span className="hub-system-kind">{actionLabel(action)}</span>
          <span>{msg.text}</span>
        </div>
      )
    }
    return <div className={`system-line${hl}`} data-seq={seq} role="status"><span>{msg.text}</span></div>
  }
  const time = formatClockTime(msg.created_at)
  if (kind === 'human') {
    return (
      <UserMessage
        dataSeq={seq}
        className={hl}
        ownerOnly={msg.visibility === 'owner'}
        pending={msg.pending}
        failed={msg.failed}
        onRetry={msg.failed && canRetryThis ? () => void retry(msg.clientMsgId) : undefined}
        retryDisabled={sending}
        time={time}
        dateTime={msg.created_at}
      >
        <Markdown text={msg.text ?? ''} />
      </UserMessage>
    )
  }
  return (
    <AgentMessage
      agent={agent}
      name={agent?.name || '智能体'}
      role={agent?.role}
      tags={msg.visibility === 'owner' ? <span className="agent-tag owner">{t('hub.ownerOnly')}</span> : null}
      time={time}
      dateTime={msg.created_at}
      dataSeq={seq}
      className={hl}
    >
      <Markdown text={msg.text ?? ''} />
    </AgentMessage>
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
          <span className="hub-run-icon" aria-hidden="true">
            {e.kind === 'compress' ? <LayersIcon size={12} /> : <TerminalIcon size={12} />}
          </span>
          <span className="hub-run-copy">
            <span className="hub-run-kicker">{e.kind === 'compress' ? 'Compress' : 'Tool'}</span>
            <span className="hub-run-line">
              <span className="hub-run-name">{e.name || e.kind}</span>
              {e.summary ? <span className="hub-run-detail">{e.summary}</span> : null}
            </span>
          </span>
          <span className="hub-run-status">
            {e.status === 'running' ? '运行中' : e.status === 'error' ? '失败' : e.ms ? `${(e.ms / 1000).toFixed(1)}s` : '完成'}
          </span>
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
