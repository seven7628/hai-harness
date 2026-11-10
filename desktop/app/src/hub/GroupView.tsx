import { useCallback, useEffect, useMemo, useRef, useState, type DependencyList } from 'react'
import { useLang, useT } from '../i18n'
import { GitBranch, SendIcon, TerminalIcon } from '../components/icons'
import { useHubStore } from './useHubStore'
import { useAppStore } from '../store/useAppStore'
import { hubCommand } from './api'
import { GroupBubble } from './MessageBubble'
import { DayDivider } from './ConversationMessage'
import { dayKey, dayLabel } from './conversationDay'
import { RoundGroup } from './RoundGroup'
import { RoundActivityDigest } from './RoundActivityDigest'
import { StreamingMessage } from './StreamingMessage'
import ConversationBar from './ConversationBar'
import MentionPicker from './MentionPicker'
import AgentStack from './AgentStack'
import AgentSigil from './AgentSigil'
import { stateResolver } from './avatar'
import { stackAriaLabel } from './avatarLabels'
import { usePendingAgents } from './useAgentState'
import TypingIndicator from './TypingIndicator'
import { buildThreadItems, routingSummary, roundKey, type ThreadItem } from './groupThread'
import { triggerSeqBySender } from './groupProjection'
import { canAutoFollow, scrollToSeq, seqJumpPlan } from './seqJump'
import { groupTimelineMarkdown } from './exportMarkdown'
import { hubPerfMemoEnabled } from './perfFlags'
import { downloadText, safeFilename, timestampSlug } from './download'
import {
  ALL_MENTION_ID,
  activeMentionQuery,
  applyMention,
  assignableMembers,
  filterMentionCandidates,
  groupRoster,
  hasMentionAll,
  isImeComposing,
  mentionIds,
  mentionKeyAction,
  type MentionAgent,
} from './mentions'
import type { HubGroup, HubMessage } from './types'

// 群组视图（P3a-08 → P3b-07）：群头（成员叠图）/ 自动对话状态条 / 时间线 /
// 路由动作 / Chat|Run / 发言（@ 选择器）与派任务表单。
//
// composer 两态：
//   对话：@ 弹成员选择器（结构化 mentions 随 hub_group_send 一起发）；
//   派任务：选成员 + 指令 + 可选标题 → hub_group_assign（plan §7.1.3 的 agent_id 键）。
// 同轮回复按 round_id（或同一组 trigger_seqs）收紧间距；「分开」恢复常规间距。
//
// 滚动（B1）：与 DM 同一条规则「用户本来在底部才跟随」；发送/派任务/打开群时
// 强制贴底。任务卡是异步长出来的，消息长度/派任务说明/对话状态/输入中都会触发。
// 上下文 chip（B6）：跳到触发消息并高亮 data-seq，不打开 DM。

const MENTION_LISTBOX_ID = 'hub-mention-listbox'

export default function GroupView({ group }: { group: HubGroup }) {
  const t = useT()
  const lang = useLang()
  const messages = useHubStore((s) => s.groupMessages)
  const activity = useHubStore((s) => s.groupActivity)
  const rounds = useHubStore((s) => s.groupRounds)
  const drafts = useHubStore((s) => s.groupDrafts)
  const sending = useHubStore((s) => s.groupSending)
  const assigning = useHubStore((s) => s.groupAssigning)
  const assignNote = useHubStore((s) => s.groupAssignNote)
  const convStopping = useHubStore((s) => s.groupConvStopping)
  const conversation = useHubStore((s) => s.groupConversations[group.id] ?? null)
  const running = useHubStore((s) => s.running)
  const pendingIds = usePendingAgents()
  const stateOf = stateResolver({ running, pending: pendingIds })
  const tasks = useHubStore((s) => s.tasks)
  const send = useHubStore((s) => s.sendGroup)
  const sendAssign = useHubStore((s) => s.sendGroupAssign)
  const stopConversation = useHubStore((s) => s.stopGroupConversation)
  const select = useHubStore((s) => s.select)
  const agents = useHubStore((s) => s.agents)
  const jump = useHubStore((s) => s.jump)
  const copyDeepLink = useHubStore((s) => s.copyDeepLink)
  const inspector = useHubStore((s) => s.inspector)
  const setInspector = useHubStore((s) => s.setInspector)
  const setRightExpanded = useAppStore((s) => s.setRightExpanded)
  const rightExpanded = useAppStore((s) => s.rightExpanded)
  // P4-01 性能 memo 的启动期快照：localStorage['hub.perfMemo']='0' 后重载即关闭。
  // 只读一次（不在每次 render 访问 storage）；支持回滚不需要发版，见 perfFlags.ts。
  const [perfMemo] = useState(() => hubPerfMemoEnabled())
  const [draft, setDraft] = useState('')
  const [composerMode, setComposerMode] = useState<'chat' | 'assign'>('chat')
  const [view, setView] = useState<'chat' | 'run'>('chat')
  const [displayMode, setDisplayMode] = useState<'grouped' | 'flat'>(readGroupDisplay)
  // @ 选择器：焦点始终在 textarea，选择器只做 listbox/aria-activedescendant。
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const assignTextareaRef = useRef<HTMLTextAreaElement>(null)
  const [caret, setCaret] = useState(0)
  const [mentionIndex, setMentionIndex] = useState(0)
  const [dismissedMentionStart, setDismissedMentionStart] = useState(-1)
  // 派任务表单
  const [assignTarget, setAssignTarget] = useState('')
  const [assignTitle, setAssignTitle] = useState('')
  const [assignInstruction, setAssignInstruction] = useState('')
  // 时间线滚动 + 触发消息高亮（B1/B6）
  const listRef = useRef<HTMLDivElement>(null)
  const threadRef = useRef<HTMLDivElement>(null)
  const stickBottom = useRef(true)
  const [highlightSeq, setHighlightSeq] = useState<number | null>(null)
  const highlightTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  // 每个实例只消费每个 nonce 一次：store 里的 jump 会保留 TTL，重复 effect
  //（StrictMode 双跑 / 流式消息）不能重复滚动。
  const handledJump = useRef(0)

  const members = group.members ?? []
  const roster = useMemo(() => groupRoster(group, agents), [group, agents])
  const assignable = useMemo(() => assignableMembers(roster), [roster])
  const agentNames = useMemo(() => {
    const map: Record<string, string> = {}
    for (const a of agents) map[a.id] = a.name
    return map
  }, [agents])
  const title = members
    .map((m) => agentNames[m.agent_id] ?? m.agent_id)
    .filter(Boolean)
    .join('、')
  const owner = members.find((m) => m.agent_id === group.owner_agent_id)
  // 群头叠放：成员状态逐个算（组状态取最高优先级，只在 lead 画一次）。
  const stackMembers = members.map((m) => {
    const member = agents.find((a) => a.id === m.agent_id)
    return {
      id: m.agent_id,
      name: agentNames[m.agent_id] ?? m.agent_id,
      responsibility: member?.role,
      state: stateOf(m.agent_id, member?.deleted),
    }
  })

  const mentionQuery = composerMode === 'chat' ? activeMentionQuery(draft, caret) : null
  const mentionQueryText = mentionQuery?.query ?? ''
  const allCandidate: MentionAgent = useMemo(
    () => ({ id: ALL_MENTION_ID, name: '所有人', role: '', groupRole: 'all' }),
    [],
  )
  const showAllCandidate =
    !!mentionQuery &&
    (!mentionQueryText || 'all'.includes(mentionQueryText.toLowerCase()) || '所有人'.includes(mentionQueryText))
  const hasMentionQuery = !!mentionQuery
  const candidates = useMemo(
    () => (hasMentionQuery
      ? [...(showAllCandidate ? [allCandidate] : []), ...filterMentionCandidates(mentionQueryText, roster)]
      : []),
    [hasMentionQuery, mentionQueryText, showAllCandidate, roster, allCandidate],
  )
  const pickerOpen = !!mentionQuery && mentionQuery.start !== dismissedMentionStart
  const activeMentionIndex = candidates.length === 0 ? 0 : Math.min(Math.max(0, mentionIndex), candidates.length - 1)

  // 查询变了从头选；Esc 关闭后同一 @ 不再弹（再敲字会重置 dismissed）。
  useEffect(() => {
    setMentionIndex(0)
  }, [mentionQueryText])

  // 可派发成员变化（加/删成员）时把默认目标收敛到第一个可用的人。
  useEffect(() => {
    if (assignTarget && assignable.some((a) => a.id === assignTarget)) return
    setAssignTarget(assignable[0]?.id ?? '')
  }, [assignable, assignTarget])

  const items = useMemo(() => buildThreadItems(messages, rounds), [messages, rounds])
  // 草稿按当前群过滤一次：空态判断和渲染必须用同一份，否则别的群的草稿会让
  // 本群「还没有消息」的引导语消失，却渲染出零个气泡（空面板）。
  const groupDrafts = useMemo(() => drafts.filter((d) => d.groupId === group.id), [drafts, group.id])
  const typingNames = useMemo(
    () => groupDrafts.map((d) => agentNames[d.agentId] ?? d.agentId),
    [groupDrafts, agentNames],
  )
  // 停止按钮的 active 条件：流式草稿 / 群成员在跑 / 群任务已派发或在跑。
  // 工具型长任务可能没有草稿也没有流式正文，任务状态是唯一可靠信号（B7）。
  const activeGroupTasks = tasks.filter((task) => {
    const gid = task.target?.group_id || task.origin?.group_id
    return gid === group.id && (task.status === 'dispatched' || task.status === 'running')
  }).length
  const convActive = groupDrafts.length > 0 || members.some((m) => running[m.agent_id] === true) || activeGroupTasks > 0
  // 头像的组状态口径：会话在跑（或草稿/成员/任务在跑）。convActive 本身不含 status，
  // 只看它会让「自动对话中但没有成员正在跑」的群头显示成待命。
  const groupRunning = conversation?.status === 'running' || convActive
  // 身份必须稳定：它经 GroupBubble → Markdown(renderMention) 传下去，每次渲染换新函数
  // 会让 memo 失效 —— 输入框每敲一个字都重 parse 整条时间线。
  const openAgent = useCallback((id: string): void => {
    void select(id)
  }, [select])

  // 触发消息 seq → 发送者（纯函数在 groupProjection.ts，Node 直测）。
  const triggerSeqBySenderMap = useMemo(() => triggerSeqBySender(messages), [messages])

  // 贴底跟随：只有用户本来就在底部时才自动滚动（翻历史/跳转后不抢视图）。
  useEffect(() => {
    if (view !== 'chat' || !stickBottom.current) return
    // 有待执行的深链/触发跳转时先别贴底：跳转自己会滚动，否则一帧内先到底再回中。
    if (!canAutoFollow(jump, group.id)) return
    const el = listRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [
    messages.length,
    groupDrafts.length,
    composerMode,
    assignNote,
    conversation?.status,
    typingNames.length,
    activeGroupTasks,
    view,
    jump,
    group.id,
  ])

  // 打开/切群：新时间线永远从底部开始（B1）。
  useEffect(() => {
    stickBottom.current = true
    const el = listRef.current
    if (el && view === 'chat') el.scrollTop = el.scrollHeight
  }, [group.id, view])

  // 任务卡/流式气泡/输入区高度都是异步长出来的：内容或可视区尺寸一变，
  // 只要用户仍在底部就跟到底（B1；审查实测任务卡从 0% 可见变 100%）。
  useEffect(() => {
    const el = listRef.current
    if (view !== 'chat' || !el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(() => {
      if (stickBottom.current) el.scrollTop = el.scrollHeight
    })
    ro.observe(el)
    if (threadRef.current) ro.observe(threadRef.current)
    return () => ro.disconnect()
  }, [view])

  useEffect(() => () => {
    if (highlightTimer.current) clearTimeout(highlightTimer.current)
  }, [])

  async function submit(): Promise<void> {
    if (!draft.trim() || sending) return
    stickBottom.current = true
    // 结构化 mentions：正文里仍保留 @Name（后端也解析），但显式带上 agent_id，
    // 让「点选过谁」不依赖后端对名字的二次解析（§8.3.3）。
    const ok = await send(draft, mentionIds(draft, roster), hasMentionAll(draft))
    if (ok) {
      setDraft('')
      setCaret(0)
      requestAnimationFrame(() => textareaRef.current?.focus())
    }
  }

  async function submitAssign(): Promise<void> {
    if (!assignTarget || !assignInstruction.trim() || assigning) return
    stickBottom.current = true
    const ok = await sendAssign(group.id, {
      agent_id: assignTarget,
      instruction: assignInstruction,
      title: assignTitle.trim() || undefined,
    })
    if (ok) {
      setAssignInstruction('')
      setAssignTitle('')
      requestAnimationFrame(() => assignTextareaRef.current?.focus())
    }
  }

  function pickMention(agent: MentionAgent): void {
    const applied = applyMention(draft, caret, agent.name)
    setDraft(applied.text)
    setCaret(applied.caret)
    setMentionIndex(0)
    setDismissedMentionStart(-1)
    // 选择后把焦点和光标送回 textarea（mousedown 已 preventDefault，这里兜底键盘路径）。
    setTimeout(() => {
      const el = textareaRef.current
      if (!el) return
      el.focus()
      el.setSelectionRange(applied.caret, applied.caret)
    }, 0)
  }

  function mentionKey(e: React.KeyboardEvent<HTMLTextAreaElement>): boolean {
    if (!pickerOpen || !mentionQuery) return false
    const action = mentionKeyAction(e.key, candidates.length > 0)
    if (action === 'up' || action === 'down') {
      e.preventDefault()
      const step = action === 'down' ? 1 : -1
      setMentionIndex((activeMentionIndex + step + candidates.length) % candidates.length)
      return true
    }
    if (action === 'pick') {
      e.preventDefault()
      pickMention(candidates[activeMentionIndex])
      return true
    }
    if (action === 'dismiss') {
      e.preventDefault()
      setDismissedMentionStart(mentionQuery.start)
      return true
    }
    // 空候选：Enter/Tab 不拦截 —— Enter 发送，Tab 正常移焦（B3）。
    return false
  }

  function setDisplay(next: 'grouped' | 'flat'): void {
    setDisplayMode(next)
    writeGroupDisplay(next)
  }

  function onScroll(): void {
    const el = listRef.current
    if (!el) return
    stickBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80
  }

  // 上下文 chip：滚到触发消息并短暂高亮；不打开 DM（B6）。
  // 滚动落点走 seqJump.scrollToSeq（与 DM/深链同一条实现），返回是否真的找到元素。
  const jumpToSeq = useCallback((seq: number): boolean => {
    if (!(seq > 0)) return false
    stickBottom.current = false
    if (!scrollToSeq(listRef.current, seq)) return false
    setHighlightSeq(seq)
    if (highlightTimer.current) clearTimeout(highlightTimer.current)
    highlightTimer.current = setTimeout(() => setHighlightSeq(null), 1800)
    return true
  }, [])

  // 深链 / Context 面板的触发 seq（P4-02）：目标群且消息已加载才由本视图执行。
  useEffect(() => {
    const plan = seqJumpPlan(jump, { targetId: group.id, messages })
    if (!plan || handledJump.current === plan.nonce) return
    if (!jumpToSeq(plan.seq)) return
    // 有意不在消费后立刻 clearJump：StrictMode 会双跑 effect，第一个实例清掉后
    // 第二个实例就看不到跳转（高亮丢失）。jump 由 store 的 TTL（2s）回收。
    handledJump.current = plan.nonce
  }, [jump, group.id, messages, jumpToSeq])

  // 已结算时间线的元素树按依赖记忆（P4-01）：草稿 delta / 输入框每敲一个字都会
  // 重渲染 GroupView，但 messages/rounds 没变；React 按同一 element 引用跳过这
  // 223 个 slot，只更新草稿气泡。当前 bench:hub-long（production，220 条消息 +
  // 3 条草稿）3-run median after-run：delta 提交 p50 0.3ms、强制布局 p50 0.6ms；A/B review
  // 记录 delta 0.9→0.3ms、键入 1.2→0.6ms。依赖里不能漏 highlightSeq：B6 跳转
  // 高亮必须能重渲染对应消息。
  //
  // 回滚：hub.perfMemo=0 → deps 传 undefined，useMemo 每次渲染都重算，即 P4-01
  // 之前的路径（不新增分支组件、不改变任何输出）。默认开启。
  const threadNodes = usePerfMemo(
    () => renderThread(items, agentNames, displayMode === 'grouped', roster, openAgent, triggerSeqBySenderMap, jumpToSeq, highlightSeq, lang),
    perfMemo ? [items, agentNames, displayMode, roster, openAgent, triggerSeqBySenderMap, jumpToSeq, highlightSeq, lang] : undefined,
  )

  function copyLink(): void {
    const seq = messages.reduce((max, m) => (Number(m.seq) > max ? Number(m.seq) : max), 0)
    void copyDeepLink('group', group.id, seq)
  }

  // @ 按钮：在光标处插入一个 @ 并聚焦，随后由既有的 mention picker 接管（真实可用，
  // 不做 demo 里的附件/语音按钮 —— 那些没有实现）。
  function insertMentionToken(): void {
    const el = textareaRef.current
    const pos = el?.selectionStart ?? draft.length
    const next = `${draft.slice(0, pos)}@${draft.slice(pos)}`
    setDraft(next)
    setCaret(pos + 1)
    setDismissedMentionStart(-1)
    requestAnimationFrame(() => {
      const node = textareaRef.current
      if (!node) return
      node.focus()
      node.setSelectionRange(pos + 1, pos + 1)
    })
  }

  function exportMarkdown(): void {
    const md = groupTimelineMarkdown({ group, agents, messages, tasks, exportedAt: new Date().toISOString() })
    downloadText(`hub-group-${safeFilename(group.name || group.id, 'group')}-${timestampSlug()}.md`, md)
  }

  async function editBudget(): Promise<void> {
    const current = group.policy?.daily_budget_usd ?? 0
    const raw = window.prompt('群日预算 USD（0 = 不限额）', String(current))
    if (raw === null) return
    const next = Number(raw)
    if (!Number.isFinite(next) || next < 0) return
    const r = await hubCommand('hub_group_update', {
      group_id: group.id,
      patch: { policy: { daily_budget_usd: next } },
    })
    if (r.ok) await useHubStore.getState().loadGroups()
  }

  return (
    <section className="hub-dm hub-group-view">
      <header className="hub-conv-head hub-group-head">
        <div className="hub-conv-id">
          <AgentStack
            members={stackMembers}
            size={32}
            groupRunning={groupRunning}
            label={stackAriaLabel(t, stackMembers, { groupRunning })}
          />
          <div className="hub-conv-title">
            <span className="hub-conv-line">
              <strong>{group.name}</strong>
              {group.deleted ? <span className="hub-chip danger">已删除</span> : null}
            </span>
            <span className="hub-conv-sub">
              {t('hub.group.members').replace('{n}', String(members.length))}
              {' · '}
              {t('hub.group.mode').replace('{mode}', group.policy?.mode ?? 'ask')}
              {owner ? ` · ${t('hub.group.owner').replace('{name}', agentNames[owner.agent_id] ?? owner.agent_id)}` : ''}
            </span>
            {title ? <span className="hub-conv-sub hub-conv-names" title={title}>{title}</span> : null}
          </div>
        </div>
        <div className="hub-conv-actions">
          <button
            className="hub-pill hub-budget-chip"
            onClick={() => void editBudget()}
            title="点击修改群日预算"
            aria-label={`群日预算 $${(group.policy?.daily_budget_usd ?? 0).toFixed(0)}，点击修改`}
          >
            {t('hub.header.budget').replace('{amount}', (group.policy?.daily_budget_usd ?? 0).toFixed(0))}
          </button>
          <span className="hub-seg" role="tablist" aria-label="Chat or Run">
            <button role="tab" aria-selected={view === 'chat'} className={view === 'chat' ? 'on' : ''} onClick={() => setView('chat')}>{t('hub.header.chat')}</button>
            <button role="tab" aria-selected={view === 'run'} className={view === 'run' ? 'on' : ''} onClick={() => setView('run')}>{t('hub.header.run')}</button>
          </span>
          {view === 'chat' ? (
            <span className="hub-seg" role="tablist" aria-label="紧凑或分开显示">
              <button role="tab" aria-selected={displayMode === 'grouped'} className={displayMode === 'grouped' ? 'on' : ''} onClick={() => setDisplay('grouped')}>{t('hub.header.grouped')}</button>
              <button role="tab" aria-selected={displayMode === 'flat'} className={displayMode === 'flat' ? 'on' : ''} onClick={() => setDisplay('flat')}>{t('hub.header.flat')}</button>
            </span>
          ) : null}
          <button
            className={`hub-action-btn ${inspector === 'context' ? 'on' : ''}`}
            onClick={() => {
              setInspector(rightExpanded && inspector === 'context' ? null : 'context')
              setRightExpanded(true)
            }}
            title="上下文（只读）：触发上下文与提示词分层"
          >
            {t('hub.header.context')}
          </button>
          <button className="hub-action-btn" onClick={copyLink} title="复制这个群的 hub:// 深链">{t('hub.header.link')}</button>
          <button className="hub-action-btn" onClick={exportMarkdown} title="导出群时间线 Markdown">{t('hub.header.export')}</button>
        </div>
      </header>

      <ConversationBar
        conversation={conversation}
        pairNames={agentNames}
        active={convActive}
        stopping={convStopping}
        onStop={() => void stopConversation(group.id)}
      />

      <div className="hub-list" ref={listRef} onScroll={onScroll}>
        {view === 'run' ? (
          <div className="hub-run">
            <RoundActivityDigest activity={activity} names={agentNames} />
            {activity.length === 0 ? (
              <div className="hub-empty"><p>还没有群活动</p><span className="hub-muted">@ 路由与工具活动会出现在这里</span></div>
            ) : activity.map((a, i) => (
              <div key={`${a.after_seq}-${i}`} className={`hub-run-row ${a.status ?? ''}`}>
                <span className="hub-run-icon" aria-hidden="true">
                  {a.kind === 'routing' ? <GitBranch size={12} /> : <TerminalIcon size={12} />}
                </span>
                <span className="hub-run-copy">
                  <span className="hub-run-kicker">{a.kind === 'routing' ? 'Route' : 'Tool'}</span>
                  <span className="hub-run-line">
                    <span className="hub-run-name">{a.name || a.kind}</span>
                    <span className="hub-run-detail">{a.kind === 'routing' ? routingSummary(a.summary ?? '', agentNames) : a.summary}</span>
                  </span>
                </span>
                <span className="hub-run-status">{a.status === 'error' ? '失败' : a.status === 'running' ? '运行中' : `#${a.after_seq}`}</span>
              </div>
            ))}
          </div>
        ) : (
          <div className={`hub-thread hub-thread-${displayMode}`} ref={threadRef}>
            {messages.length === 0 && groupDrafts.length === 0 ? (
              <div className="hub-intro">
                <p>{t('hub.groupIntro')}</p>
                <span className="hub-muted">{t('hub.groupHint')}</span>
              </div>
            ) : null}
            {threadNodes}
            {groupDrafts.map((d) => (
              <StreamingMessage
                key={d.messageId}
                draft={d}
                name={agentNames[d.agentId] ?? d.agentId}
                agent={roster.find((a) => a.id === d.agentId)}
              />
            ))}
          </div>
        )}
      </div>

      <div className="hub-composer-wrap">
        {/* 常驻 live region：内容出现前容器已在 DOM 里，读屏才会播报（N8）。 */}
        <TypingIndicator names={typingNames} />
        <div className="hub-composer">
          {pickerOpen ? (
            <MentionPicker
              id={MENTION_LISTBOX_ID}
              candidates={candidates}
              activeIndex={activeMentionIndex}
              onActiveIndex={setMentionIndex}
              onPick={pickMention}
              onClose={() => setDismissedMentionStart(mentionQuery?.start ?? -1)}
            />
          ) : null}
          {composerMode === 'chat' ? (
            <>
              <button
                type="button"
                className="hub-composer-tool"
                onClick={insertMentionToken}
                title={t('hub.composer.mention')}
                aria-label={t('hub.composer.mention')}
              >
                @
              </button>
              <textarea
                ref={textareaRef}
                value={draft}
                placeholder={t('hub.groupPlaceholder')}
                onChange={(e) => {
                  setDraft(e.target.value)
                  setCaret(e.target.selectionStart)
                  setDismissedMentionStart(-1)
                }}
                onSelect={(e) => setCaret(e.currentTarget.selectionStart)}
                onClick={(e) => setCaret(e.currentTarget.selectionStart)}
                onKeyDown={(e) => {
                  // IME 合成中的 Enter/Esc 属于输入法，不能发送/关闭（B2）。
                  if (isImeComposing(e.nativeEvent, e.keyCode)) return
                  if (mentionKey(e)) return
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault()
                    void submit()
                  }
                }}
                rows={2}
                aria-autocomplete="list"
                aria-controls={pickerOpen && candidates.length > 0 ? MENTION_LISTBOX_ID : undefined}
                aria-expanded={pickerOpen && candidates.length > 0}
                aria-activedescendant={
                  pickerOpen && candidates.length > 0 ? `${MENTION_LISTBOX_ID}-opt-${activeMentionIndex}` : undefined
                }
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
            </>
          ) : (
            <div className="hub-assign-form" role="group" aria-label="派任务">
              <div className="hub-assign-members" role="radiogroup" aria-label="选择执行成员">
                {assignable.length === 0 ? (
                  <span className="hub-muted">
                    {members.length === 0 ? '群还没有成员，无法派任务。' : '没有可接派任务的成员（observer 只旁听）。'}
                  </span>
                ) : assignable.map((agent) => (
                  <button
                    key={agent.id}
                    type="button"
                    role="radio"
                    aria-checked={assignTarget === agent.id}
                    className={`hub-assign-member ${assignTarget === agent.id ? 'on' : ''}`}
                    onClick={() => setAssignTarget(agent.id)}
                  >
                    <AgentSigil agentId={agent.id} responsibility={agent.role} status={false} size={24} />
                    <span>{agent.name}</span>
                    {agent.groupRole && agent.groupRole !== 'all' ? <span className="hub-muted">{agent.groupRole}</span> : null}
                  </button>
                ))}
              </div>
              <input
                className="hub-assign-title"
                value={assignTitle}
                onChange={(e) => setAssignTitle(e.target.value)}
                placeholder="标题（可选，默认取指令首行）"
                aria-label="任务标题（可选）"
              />
              <textarea
                ref={assignTextareaRef}
                value={assignInstruction}
                onChange={(e) => setAssignInstruction(e.target.value)}
                placeholder="要对方交付什么？（Enter 派发，Shift+Enter 换行）"
                aria-label="任务指令"
                rows={2}
                onKeyDown={(e) => {
                  if (isImeComposing(e.nativeEvent, e.keyCode)) return
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault()
                    void submitAssign()
                  }
                }}
              />
              <button
                type="button"
                className="hub-send hub-assign-send"
                onClick={() => void submitAssign()}
                disabled={assigning || !assignTarget || !assignInstruction.trim()}
                title="派发任务"
                aria-label="派发任务"
              >
                <SendIcon />
              </button>
            </div>
          )}
        </div>
        <div className="hub-composer-meta">
          <span className="hub-seg hub-seg-quiet" role="tablist" aria-label="输入模式">
            <button role="tab" aria-selected={composerMode === 'chat'} className={composerMode === 'chat' ? 'on' : ''} onClick={() => setComposerMode('chat')}>对话</button>
            <button role="tab" aria-selected={composerMode === 'assign'} className={composerMode === 'assign' ? 'on' : ''} onClick={() => setComposerMode('assign')}>派任务</button>
          </span>
          {composerMode === 'chat' ? (
            <span className="hub-composer-hint">
              {members.length === 0
                ? '群还没有成员，消息只会记录。'
                : draft.trim() && mentionIds(draft, roster).length === 0 && !hasMentionAll(draft)
                  ? '仅记录，不触发回复（@成员 才会唤醒）'
                  : t('hub.composer.mentionHint')}
            </span>
          ) : (
            <span className="hub-composer-hint">派任务会创建账本任务，结果以任务卡回群。</span>
          )}
          {composerMode === 'chat' ? (
            <span className="hub-composer-keys">
              <kbd>Enter</kbd> {t('hub.composer.enter')}
              <span className="hub-composer-sep">·</span>
              <kbd>Shift</kbd>+<kbd>Enter</kbd> {t('hub.composer.newline')}
            </span>
          ) : null}
        </div>
        {composerMode === 'assign' && assignNote ? (
          <div className="hub-assign-note" role="status">{assignNote}</div>
        ) : null}
      </div>
    </section>
  )
}


/**
 * 带「显式关闭」的 useMemo：deps 传 undefined 时 React 语义就是每次渲染重算，
 * 正好是 P4-01 之前的路径（不需要两套渲染代码，也不会改变输出）。
 * @types/react 把 deps 标成必填，运行时支持 undefined（官方文档：省略依赖 = 每次重算），
 * 这里用一个注释清楚的窄 cast 保留该语义；调用方不得传 undefined 以外的非数组值。
 */
function usePerfMemo<T>(factory: () => T, deps: DependencyList | undefined): T {
  // eslint-disable-next-line react-hooks/exhaustive-deps -- deps 由调用方提供；undefined = 每次重算
  return useMemo(factory, deps as DependencyList)
}

function renderThread(
  items: ThreadItem[],
  names: Record<string, string>,
  grouped: boolean,
  roster: MentionAgent[],
  onSelectAgent: (id: string) => void,
  triggerSeqBySender: Record<string, number>,
  onJumpToSeq: (seq: number) => void,
  highlightSeq: number | null,
  lang: 'zh' | 'en',
) {
  const nodes: React.ReactNode[] = []
  let batch: HubMessage[] = []
  let batchKey = ''
  let lastDay = ''
  const flush = (): void => {
    if (batch.length === 0) return
    nodes.push(
      <RoundGroup
        key={`g-${batch[0].seq}-${batch[batch.length - 1].seq}`}
        messages={batch}
        names={names}
        grouped={grouped && !!batchKey}
        roster={roster}
        onSelectAgent={onSelectAgent}
        triggerSeqBySender={triggerSeqBySender}
        onJumpToSeq={onJumpToSeq}
        highlightSeq={highlightSeq}
      />,
    )
    batch = []
    batchKey = ''
  }
  // 日期分隔线：每条消息（含交接卡/回执）按本地自然日切段；同轮消息跨天时先收批，
  // 分隔线不能被包进 round-stack 的紧凑间距里。
  const pushDay = (msg: HubMessage): void => {
    const key = dayKey(msg.created_at)
    if (!key || key === lastDay) return
    if (batch.length > 0) flush()
    lastDay = key
    nodes.push(<DayDivider key={`day-${key}`} label={dayLabel(msg.created_at, lang)} />)
  }
  for (const item of items) {
    if (item.kind === 'round') {
      flush()
      const r = item.round
      const targets = (r.targets ?? []).map((id) => names[id] ?? id)
      nodes.push(
        <div key={`round-${r.id}`} className="hub-system-note hub-system-routing" role="status">
          <span className="hub-system-kind">路由</span>
          <span>
            {r.mention_all ? '@all ' : ''}
            {targets.length === 0 ? '无人被唤醒' : `唤醒 ${targets.join('、')}`}
            {r.dropped ? `（丢弃 ${r.dropped} 个）` : ''}
          </span>
        </div>,
      )
      continue
    }
    if (item.msg.kind === 'routing_receipt') {
      flush()
      pushDay(item.msg)
      nodes.push(
        <GroupBubble
          key={`m-${item.msg.seq}`}
          msg={item.msg}
          names={names}
          roster={roster}
          onSelectAgent={onSelectAgent}
          triggerSeqBySender={triggerSeqBySender}
          onJumpToSeq={onJumpToSeq}
          highlightSeq={highlightSeq}
        />,
      )
      continue
    }
    const key = roundKey(item.msg)
    pushDay(item.msg)
    if (batch.length > 0 && (key === '' || key !== batchKey)) flush()
    if (batch.length === 0) batchKey = key
    batch.push(item.msg)
  }
  flush()
  return nodes
}


// localStorage 在沙箱/隐私模式下可能连访问都抛 SecurityError：读写都必须兜底（N10）。
function readGroupDisplay(): 'grouped' | 'flat' {
  try {
    if (typeof localStorage === 'undefined') return 'grouped'
    return localStorage.getItem('hub.groupDisplay') === 'flat' ? 'flat' : 'grouped'
  } catch {
    return 'grouped'
  }
}

function writeGroupDisplay(next: 'grouped' | 'flat'): void {
  try {
    if (typeof localStorage !== 'undefined') localStorage.setItem('hub.groupDisplay', next)
  } catch {
    // 隐私模式/配额满：只是记不住偏好，不影响当前视图。
  }
}
