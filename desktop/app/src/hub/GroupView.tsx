import { useCallback, useEffect, useMemo, useRef, useState, type DependencyList } from 'react'
import { useLang, useT } from '../i18n'
import { GitBranch, ImageIcon, MoreIcon, PaperclipIcon, SendIcon, TerminalIcon } from '../components/icons'
import { useResolvedTheme } from '../lib/useTheme'
import { useHubStore } from './useHubStore'
import { useHostPanels } from '../host/panels'
import { hubCommand } from './api'
import { GroupBubble } from './MessageBubble'
import { DayDivider } from './ConversationMessage'
import { dayKey, dayLabel } from './conversationDay'
import { RoundGroup } from './RoundGroup'
import ConversationBar from './ConversationBar'
import { RoundActivityDigest } from './RoundActivityDigest'
import { StreamingMessage } from './StreamingMessage'
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
import { asGroupMode, GROUP_MODE_HINT, GROUP_MODES, type GroupMode } from './groupMode'
import { hubPerfMemoEnabled } from './perfFlags'
import { useScrollbarWidth } from './useScrollbarWidth'
import { useHeaderMenu } from './useHeaderMenu'
import { useComposerAutoGrow } from './useComposerAutoGrow'
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
import { useClearSentAttachments, useHubAttachments } from './useHubAttachments'
import { withFileRefs } from './attachments'
import AttachmentStrip from './AttachmentStrip'
import HubInlineNeeds from './HubInlineNeeds'
import HubInlineTodos from './HubInlineTodos'

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
  const resetConversation = useHubStore((s) => s.resetGroupConversation)
  const select = useHubStore((s) => s.select)
  const agents = useHubStore((s) => s.agents)
  const jump = useHubStore((s) => s.jump)
  const copyDeepLink = useHubStore((s) => s.copyDeepLink)
  const inspector = useHubStore((s) => s.inspector)
  const setInspector = useHubStore((s) => s.setInspector)
  const { setRightExpanded, rightExpanded, capabilities } = useHostPanels()
  // P4-01 性能 memo 的启动期快照：localStorage['hub.perfMemo']='0' 后重载即关闭。
  // 只读一次（不在每次 render 访问 storage）；支持回滚不需要发版，见 perfFlags.ts。
  const [perfMemo] = useState(() => hubPerfMemoEnabled())
  // 群 composer 草稿放 store（按群 id）：横幅/气泡重试成功后要能清掉输入框 —— 重试
  // 不经过 submit，本地 state 收不到「成功」这件事（复审 B2）；顺带切群再回来草稿还在。
  const draft = useHubStore((s) => s.groupDraftText[group.id] ?? '')
  const setGroupDraft = useHubStore((s) => s.setGroupDraft)
  const setDraft = useCallback((text: string): void => setGroupDraft(group.id, text), [setGroupDraft, group.id])
  const [composerMode, setComposerMode] = useState<'chat' | 'assign'>('chat')
  const [view, setView] = useState<'chat' | 'run'>('chat')
  const [displayMode, setDisplayMode] = useState<'grouped' | 'flat'>(readGroupDisplay)
  // 头部「更多操作」：外点/Esc 关闭 + 键盘导航（P6-16，与 HubSurface 共用同一份行为）。
  const headerMenu = useHeaderMenu()
  // 群模式切换在飞：菜单项要能立刻反映「保存中」，失败由 store.error/HubErrorBanner 透出。
  const [modeSaving, setModeSaving] = useState<GroupMode | null>(null)
  // 「重置本群限制」在飞：状态条上的按钮要能立刻反映，失败由 store.error 透出。
  const [resetBusy, setResetBusy] = useState(false)
  const theme = useResolvedTheme()
  const openContext = (): void => {
    setInspector(rightExpanded && inspector === 'context' ? null : 'context')
    setRightExpanded(true)
  }
  // @ 选择器：焦点始终在 textarea，选择器只做 listbox/aria-activedescendant。
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  // composer 单行垂直居中 + 最多 4 行自动增高（DM/群同一套）。
  useComposerAutoGrow(textareaRef, draft)
  // 群 composer 的附件（图片 + 文件引用）：与 DM 同一套 hook；换群时清空。
  const attach = useHubAttachments()
  const attachScope = useRef<string | null>(null)
  useEffect(() => {
    if (attachScope.current === group.id) return
    attachScope.current = group.id
    attach.reset()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [group.id])
  // 失败气泡的行内重试成功后，条带里那张图也该消失（否则再点发送会重复发图）。
  useClearSentAttachments(attach, 'group', group.id)
  // 隐藏的图片选择器：按钮只触发 click，<input> 留在可访问树里（e2e 可 setInputFiles）。
  const imageInputRef = useRef<HTMLInputElement>(null)
  const assignTextareaRef = useRef<HTMLTextAreaElement>(null)
  const [caret, setCaret] = useState(0)
  const [mentionIndex, setMentionIndex] = useState(0)
  const [dismissedMentionStart, setDismissedMentionStart] = useState(-1)
  // 派任务表单
  const [assignTarget, setAssignTarget] = useState('')
  const [assignTitle, setAssignTitle] = useState('')
  const [assignInstruction, setAssignInstruction] = useState('')
  // 派任务指令也是多行草稿：同样的自动增高，避免被 chat textarea 的样式误伤。
  useComposerAutoGrow(assignTextareaRef, assignInstruction)
  // 时间线滚动 + 触发消息高亮（B1/B6）
  const listRef = useRef<HTMLDivElement>(null)
  // 会话视图根节点：--hub-scrollbar-w 写在这里，composer 外层继承后补右内边距。
  const rootRef = useRef<HTMLElement | null>(null)
  useScrollbarWidth(listRef, rootRef)
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
  // 贴底 effect 也要盯「草稿正文长度」：草稿是就地增长的，length 不变时正文已经长了
  // 好几屏；只依赖 length 只能等 ResizeObserver 兜底（晚一帧，且长正文时更明显）。
  const groupDraftChars = groupDrafts.reduce((n, d) => n + d.text.length, 0)
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
  // 状态条（本轮/本链/队列/pair/投递）由 ConversationBar 自己算（P6-10，纯函数在
  // conversation.ts）：群头只留身份（成员/模式/owner/标题），不再重复一份对话计数。
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
    groupDraftChars,
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
    // 纯图片且没有正文也允许发（后端存「[图片]」）；群路由要不要 @ 由用户自己决定。
    if (sending || (!draft.trim() && attach.total === 0)) return
    stickBottom.current = true
    // 结构化 mentions：正文里仍保留 @Name（后端也解析），但显式带上 agent_id，
    // 让「点选过谁」不依赖后端对名字的二次解析（§8.3.3）。@ 只从用户输入的正文解析
    // （拼进去的 @路径 不是唤醒目标）。
    const mentions = mentionIds(draft, roster)
    const mentionAll = hasMentionAll(draft)
    // 文件引用拼进正文（一行一个 @path）；图片走 attachments。
    const text = withFileRefs(draft, attach.files)
    const atts = attach.all
    // 第 6 个参数是拼 @path 之前的草稿：失败重试成功后据此清输入框（见 store.clearsDraft）。
    const ok = await send(text, mentions, mentionAll, undefined, atts.length > 0 ? atts : undefined, draft)
    // 正文由 store 在发送那一刻乐观清空（失败按需放回），这里不再 setDraft('') / setCaret(0)：
    // 响应回来时用户可能已经在输入新内容，无条件清空会把它抹掉（光标也一并归零）。
    // 附件与焦点仍只在成功分支收尾（失败时留着附件，重试还能再发一次）。
    if (ok) {
      attach.reset()
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

  // 改群协作模式：走 hub_group_update 的字段级 patch（未传字段保留原值），
  // bridge 侧会把新的 agents.Mode 推到已装配的成员会话（P6-06）。刷新列表让
  // 头部文案、inspector 与真实策略同源 —— 不做「只改本地显示」的假开关。
  async function setGroupMode(next: GroupMode): Promise<void> {
    if (modeSaving || asGroupMode(group.policy?.mode) === next) return
    setModeSaving(next)
    const r = await hubCommand('hub_group_update', {
      group_id: group.id,
      patch: { policy: { mode: next } },
    })
    setModeSaving(null)
    if (r.ok) await useHubStore.getState().loadGroups()
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
    <section ref={rootRef} className="main hub-group-view hub-demo" data-theme={theme === 'paper' ? 'light' : 'dark'}>
      <header className="chat-header">
        <AgentStack
          members={stackMembers}
          size={28}
          groupRunning={groupRunning}
          label={stackAriaLabel(t, stackMembers, { groupRunning, name: group.name })}
        />
        <div className="chat-title-wrap">
          <div className="chat-title">
            <span className="name">{group.name}</span>
            {group.deleted ? <span className="agent-tag">已删除</span> : null}
          </div>
          <div
            className="chat-sub"
            title={[
              t('hub.group.members').replace('{n}', String(members.length)),
              t('hub.group.mode').replace('{mode}', group.policy?.mode ?? 'ask'),
              owner ? t('hub.group.owner').replace('{name}', agentNames[owner.agent_id] ?? owner.agent_id) : '',
              title,
            ].filter(Boolean).join(' · ')}
          >
            <span>{t('hub.group.members').replace('{n}', String(members.length))}</span>
            <span className="sep">·</span>
            <span>{t('hub.group.mode').replace('{mode}', group.policy?.mode ?? 'ask')}</span>
            {owner ? <><span className="sep">·</span><span>{t('hub.group.owner').replace('{name}', agentNames[owner.agent_id] ?? owner.agent_id)}</span></> : null}
            {title ? <><span className="sep">·</span><span>{title}</span></> : null}
          </div>
        </div>
        <div className="header-actions">
          <button
            type="button"
            className={`icon-btn${inspector === 'context' ? ' active' : ''}`}
            onClick={openContext}
            title="上下文（只读）：触发上下文与提示词分层"
            aria-label="上下文"
          >
            <GitBranch size={15} />
          </button>
          <button
            type="button"
            className="icon-btn"
            ref={headerMenu.triggerRef}
            onClick={headerMenu.toggle}
            aria-label="更多操作"
            aria-haspopup="menu"
            aria-expanded={headerMenu.open}
          >
            <MoreIcon size={15} />
          </button>
          {headerMenu.open ? (
            <div className="header-menu" role="menu" ref={headerMenu.menuRef}>
              <button type="button" role="menuitemradio" aria-checked={view === 'chat'} onClick={() => { setView('chat'); headerMenu.close() }}>Chat</button>
              <button type="button" role="menuitemradio" aria-checked={view === 'run'} onClick={() => { setView('run'); headerMenu.close() }}>Run</button>
              <span className="header-menu-sep" />
              <button type="button" role="menuitemradio" aria-checked={displayMode === 'grouped'} onClick={() => { setDisplay('grouped'); headerMenu.close() }}>{t('hub.header.grouped')}</button>
              <button type="button" role="menuitemradio" aria-checked={displayMode === 'flat'} onClick={() => { setDisplay('flat'); headerMenu.close() }}>{t('hub.header.flat')}</button>
              <span className="header-menu-sep" />
              <button type="button" role="menuitem" onClick={() => { headerMenu.close(); copyLink() }}>{t('hub.header.link')}</button>
              <button type="button" role="menuitem" onClick={() => { headerMenu.close(); exportMarkdown() }}>{t('hub.header.export')}</button>
              {/* 协作模式 + 日预算都是 hub_group_update 的字段级 patch。手机不渲染：
                  该命令不在 mobile_server 的 allowlist 内，点下去只会 403（见 HostCapabilities）。 */}
              {capabilities.groupAdmin ? (
                <>
                  <span className="header-menu-sep" />
                  {/* 协作模式：三选一（radio）。文案与 GroupEditor 同源（groupMode.ts），
                      点哪一项就真的把群策略改成哪一项 —— 不是只改显示的假开关。 */}
                  <span className="header-menu-label">协作模式</span>
                  {GROUP_MODES.map((m) => (
                    <button
                      key={m}
                      type="button"
                      role="menuitemradio"
                      aria-checked={asGroupMode(group.policy?.mode) === m}
                      disabled={modeSaving !== null}
                      title={GROUP_MODE_HINT[m]}
                      onClick={() => void setGroupMode(m)}
                    >
                      {modeSaving === m ? `${m} · 保存中…` : m}
                    </button>
                  ))}
                  <span className="header-menu-sep" />
                  <button type="button" role="menuitem" onClick={() => { headerMenu.close(); void editBudget() }}>
                    {t('hub.header.budget').replace('{amount}', (group.policy?.daily_budget_usd ?? 0).toFixed(0))}
                  </button>
                </>
              ) : null}
            </div>
          ) : null}
        </div>
      </header>

      {/* 状态条（P6-10）：本轮/本链/队列/pair 倒计时/投递账本 + 重置/停止门禁。
          放在 header 与消息区之间：状态是当前事实，滚动历史时不能跟着滚走。 */}
      <ConversationBar
        conversation={conversation}
        pairNames={agentNames}
        active={convActive}
        stopping={convStopping}
        resetting={resetBusy}
        onStop={() => void stopConversation(group.id)}
        // 重置本群限制 = hub_conversation_reset，不在 mobile_server 的 allowlist 内：
        // 手机不传 onReset（ConversationBar 自己就不渲染那个按钮），不是渲染后静默失败。
        onReset={capabilities.groupAdmin ? () => {
          setResetBusy(true)
          void resetConversation(group.id).finally(() => setResetBusy(false))
        } : undefined}
      />

      <div className="messages" ref={listRef} onScroll={onScroll}>
        <div className="messages-inner">
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
        <TypingIndicator names={typingNames} />
        </div>
      </div>

      <div className="composer-wrap">
        <div className="composer-inner">
        <HubInlineNeeds scope={{ kind: 'group', groupId: group.id }} />
        {/* 场景2：本群派出的 Project 任务待办（只读；群作用域 = origin.group_id）。 */}
        <HubInlineTodos scope={{ kind: 'group', groupId: group.id }} />
        <AttachmentStrip
          images={attach.images}
          files={attach.files}
          notice={attach.notice}
          busy={attach.busy}
          onRemove={attach.remove}
        />
        <div className="composer">
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
              <input
                ref={imageInputRef}
                type="file"
                accept="image/*"
                multiple
                className="hub-attach-input"
                data-testid="hub-group-image-input"
                aria-label="选择图片"
                onChange={(e) => {
                  const picked = Array.from(e.target.files ?? [])
                  e.target.value = ''
                  if (picked.length > 0) void attach.addImageFiles(picked)
                }}
              />
              <button
                type="button"
                className="icon-btn"
                onClick={() => void attach.pickFiles()}
                title="引用文件（发送时写成 @路径，不读文件内容）"
                aria-label="引用文件"
              >
                <PaperclipIcon size={15} />
              </button>
              <button
                type="button"
                className="icon-btn"
                onClick={() => imageInputRef.current?.click()}
                title="添加图片"
                aria-label="添加图片"
              >
                <ImageIcon size={15} />
              </button>
              <button
                type="button"
                className="icon-btn"
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
                onPaste={attach.onPaste}
                onDrop={attach.onDrop}
                onDragOver={(e) => {
                  // 不拦 dragover 浏览器会当成「打开文件」而不是 drop。
                  if (e.dataTransfer?.types?.includes('Files')) e.preventDefault()
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
                rows={1}
                aria-autocomplete="list"
                aria-controls={pickerOpen && candidates.length > 0 ? MENTION_LISTBOX_ID : undefined}
                aria-expanded={pickerOpen && candidates.length > 0}
                aria-activedescendant={
                  pickerOpen && candidates.length > 0 ? `${MENTION_LISTBOX_ID}-opt-${activeMentionIndex}` : undefined
                }
              />
              <button
                className="send-btn"
                onClick={() => void submit()}
                disabled={sending || attach.busy || (!draft.trim() && attach.total === 0)}
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
                    {members.length === 0 ? '群还没有成员，无法派任务。' : '没有可接派任务的成员。'}
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
                    <AgentSigil agentId={agent.id} name={agent.name} responsibility={agent.role} status={false} size={24} />
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
                className="send-btn hub-assign-send"
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
        <div className="composer-meta">
          <span className="composer-hint">
            <kbd>Enter</kbd> {t('hub.composer.enter')} <span className="composer-sep">·</span> <kbd>Shift</kbd>+<kbd>Enter</kbd> {t('hub.composer.newline')}
          </span>
          <span className="meta-links">
            <span className="composer-mode-switch" role="tablist" aria-label="输入模式">
              <button type="button" role="tab" aria-selected={composerMode === 'chat'} className={composerMode === 'chat' ? 'on' : ''} onClick={() => setComposerMode('chat')}>对话</button>
              <button type="button" role="tab" aria-selected={composerMode === 'assign'} className={composerMode === 'assign' ? 'on' : ''} onClick={() => setComposerMode('assign')}>派任务</button>
            </span>
            {composerMode === 'chat' ? (
              <span className="composer-note">
                {members.length === 0
                  ? '群还没有成员，消息只会记录。'
                  : draft.trim() && mentionIds(draft, roster).length === 0 && !hasMentionAll(draft)
                    ? '仅记录，不触发回复（@成员 才会唤醒）'
                    : t('hub.composer.mentionHint')}
              </span>
            ) : (
              <span className="composer-note">派任务会创建账本任务，结果以任务卡回群。</span>
            )}
          </span>
        </div>
        {composerMode === 'assign' && assignNote ? (
          <div className="hub-assign-note" role="status">{assignNote}</div>
        ) : null}
        </div>
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
        <div key={`round-${r.id}`} className="route-line" role="status">
          <span className="routing-pill">路由</span>
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
