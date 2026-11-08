import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { SendIcon } from '../components/icons'
import { useHubStore } from './useHubStore'
import { hubCommand } from './api'
import { GroupBubble } from './MessageBubble'
import { RoundGroup } from './RoundGroup'
import { RoundActivityDigest } from './RoundActivityDigest'
import { StreamingMessage } from './StreamingMessage'
import ConversationBar from './ConversationBar'
import MentionPicker from './MentionPicker'
import TypingIndicator from './TypingIndicator'
import { buildThreadItems, routingSummary, roundKey, type ThreadItem } from './groupThread'
import {
  ALL_MENTION_ID,
  activeMentionQuery,
  applyMention,
  assignableMembers,
  filterMentionCandidates,
  groupRoster,
  hasMentionAll,
  mentionIds,
  type MentionAgent,
} from './mentions'
import type { HubGroup, HubGroupMember, HubMessage } from './types'

// 群组视图（P3a-08 → P3b-07）：群头（成员叠图）/ 自动对话状态条 / 时间线 /
// 路由动作 / Chat|Run / 发言（@ 选择器）与派任务表单。
//
// composer 两态：
//   对话：@ 弹成员选择器（结构化 mentions 随 hub_group_send 一起发）；
//   派任务：选成员 + 指令 + 可选标题 → hub_group_assign（plan §7.1.3 的 agent_id 键）。
// 同轮回复按 round_id（或同一组 trigger_seqs）合并；「分开」保留平铺。

const MENTION_LISTBOX_ID = 'hub-mention-listbox'

export default function GroupView({ group }: { group: HubGroup }) {
  const t = useT()
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
  const send = useHubStore((s) => s.sendGroup)
  const sendAssign = useHubStore((s) => s.sendGroupAssign)
  const stopConversation = useHubStore((s) => s.stopGroupConversation)
  const select = useHubStore((s) => s.select)
  const agents = useHubStore((s) => s.agents)
  const [draft, setDraft] = useState('')
  const [composerMode, setComposerMode] = useState<'chat' | 'assign'>('chat')
  const [view, setView] = useState<'chat' | 'run'>('chat')
  const [displayMode, setDisplayMode] = useState<'grouped' | 'flat'>(() =>
    typeof localStorage !== 'undefined' && localStorage.getItem('hub.groupDisplay') === 'flat' ? 'flat' : 'grouped',
  )
  // @ 选择器：焦点始终在 textarea，选择器只做 listbox/aria-activedescendant。
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const [caret, setCaret] = useState(0)
  const [mentionIndex, setMentionIndex] = useState(0)
  const [dismissedMentionStart, setDismissedMentionStart] = useState(-1)
  // 派任务表单
  const [assignTarget, setAssignTarget] = useState('')
  const [assignTitle, setAssignTitle] = useState('')
  const [assignInstruction, setAssignInstruction] = useState('')

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

  async function submit(): Promise<void> {
    if (!draft.trim() || sending) return
    // 结构化 mentions：正文里仍保留 @Name（后端也解析），但显式带上 agent_id，
    // 让「点选过谁」不依赖后端对名字的二次解析（§8.3.3）。
    const ok = await send(draft, mentionIds(draft, roster), hasMentionAll(draft))
    if (ok) {
      setDraft('')
      setCaret(0)
    }
  }

  async function submitAssign(): Promise<void> {
    if (!assignTarget || !assignInstruction.trim() || assigning) return
    const ok = await sendAssign(group.id, {
      agent_id: assignTarget,
      instruction: assignInstruction,
      title: assignTitle.trim() || undefined,
    })
    if (ok) {
      setAssignInstruction('')
      setAssignTitle('')
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
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      if (candidates.length === 0) return true
      const step = e.key === 'ArrowDown' ? 1 : -1
      setMentionIndex((activeMentionIndex + step + candidates.length) % candidates.length)
      return true
    }
    if (e.key === 'Enter' || e.key === 'Tab') {
      e.preventDefault()
      if (candidates.length > 0) pickMention(candidates[activeMentionIndex])
      else setDismissedMentionStart(mentionQuery.start)
      return true
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      setDismissedMentionStart(mentionQuery.start)
      return true
    }
    return false
  }

  function setDisplay(next: 'grouped' | 'flat'): void {
    setDisplayMode(next)
    if (typeof localStorage !== 'undefined') localStorage.setItem('hub.groupDisplay', next)
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

  const items = useMemo(() => buildThreadItems(messages, rounds), [messages, rounds])
  // 草稿按当前群过滤一次：空态判断和渲染必须用同一份，否则别的群的草稿会让
  // 本群「还没有消息」的引导语消失，却渲染出零个气泡（空面板）。
  const groupDrafts = useMemo(() => drafts.filter((d) => d.groupId === group.id), [drafts, group.id])
  const typingNames = useMemo(
    () => groupDrafts.map((d) => agentNames[d.agentId] ?? d.agentId),
    [groupDrafts, agentNames],
  )
  // 停止按钮的 active 条件：有流式草稿，或群里有成员正在跑。
  const convActive = groupDrafts.length > 0 || members.some((m) => running[m.agent_id] === true)
  // 身份必须稳定：它经 GroupBubble → Markdown(renderMention) 传下去，每次渲染换新函数
  // 会让 memo 失效 —— 输入框每敲一个字都重 parse 整条时间线。
  const openAgent = useCallback((id: string): void => {
    void select(id)
  }, [select])

  return (
    <section className="hub-dm hub-group-view">
      <header className="hub-dm-head">
        <MemberStack members={members} names={agentNames} />
        <div className="hub-dm-title">
          <strong>{group.name}</strong>
          <span className="hub-sub">
            {members.length} 名成员{title ? ` · ${title}` : ''}
            {owner ? ` · 群主 ${agentNames[owner.agent_id] ?? owner.agent_id}` : ''}
          </span>
        </div>
        {group.deleted ? <span className="hub-chip danger">已删除</span> : null}
        <span className={`hub-chip group-mode-${group.policy?.mode ?? 'ask'}`}>{group.policy?.mode ?? 'ask'}</span>
        <button className="hub-chip hub-budget-chip" onClick={() => void editBudget()} title="点击修改群日预算">
          预算 ${(group.policy?.daily_budget_usd ?? 0).toFixed(0)}/日
        </button>
        <span className="hub-view-toggle" role="tablist" aria-label="Chat or Run">
          <button role="tab" aria-selected={view === 'chat'} className={view === 'chat' ? 'on' : ''} onClick={() => setView('chat')}>Chat</button>
          <button role="tab" aria-selected={view === 'run'} className={view === 'run' ? 'on' : ''} onClick={() => setView('run')}>Run</button>
        </span>
        {view === 'chat' ? (
          <span className="hub-view-toggle" role="tablist" aria-label="合并或分开显示">
            <button role="tab" aria-selected={displayMode === 'grouped'} className={displayMode === 'grouped' ? 'on' : ''} onClick={() => setDisplay('grouped')}>合并</button>
            <button role="tab" aria-selected={displayMode === 'flat'} className={displayMode === 'flat' ? 'on' : ''} onClick={() => setDisplay('flat')}>分开</button>
          </span>
        ) : null}
      </header>

      <ConversationBar
        conversation={conversation}
        pairNames={agentNames}
        active={convActive}
        stopping={convStopping}
        onStop={() => void stopConversation(group.id)}
      />

      <div className="hub-list">
        {view === 'run' ? (
          <div className="hub-run">
            <RoundActivityDigest activity={activity} names={agentNames} />
            {activity.length === 0 ? (
              <div className="hub-empty"><p>还没有群活动</p><span className="hub-muted">@ 路由与工具活动会出现在这里</span></div>
            ) : activity.map((a, i) => (
              <div key={`${a.after_seq}-${i}`} className={`hub-run-row ${a.status ?? ''}`}>
                <span className="hub-chip">{a.kind === 'routing' ? '路由' : '工具'}</span>
                <span className="hub-run-name">{a.name || a.kind}</span>
                <span className="hub-muted">{a.kind === 'routing' ? routingSummary(a.summary ?? '', agentNames) : a.summary}</span>
                <span className="hub-muted">#{a.after_seq}</span>
              </div>
            ))}
          </div>
        ) : (
          <div className="hub-thread">
            {messages.length === 0 && groupDrafts.length === 0 ? (
              <div className="hub-intro">
                <p>{t('hub.groupIntro')}</p>
                <span className="hub-muted">{t('hub.groupHint')}</span>
              </div>
            ) : null}
            {renderThread(items, agentNames, displayMode === 'grouped', roster, openAgent)}
            {groupDrafts.map((d) => (
              <StreamingMessage key={d.messageId} draft={d} name={agentNames[d.agentId] ?? d.agentId} />
            ))}
          </div>
        )}
      </div>

      <div className="hub-composer-wrap">
        {typingNames.length > 0 ? <TypingIndicator names={typingNames} /> : null}
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
                  if (mentionKey(e)) return
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault()
                    void submit()
                  }
                }}
                rows={2}
                aria-autocomplete="list"
                aria-controls={pickerOpen ? MENTION_LISTBOX_ID : undefined}
                aria-expanded={pickerOpen}
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
                    <span className="hub-mention-dot" style={{ background: agent.accent || 'var(--accent-solid)' }} aria-hidden="true" />
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
                value={assignInstruction}
                onChange={(e) => setAssignInstruction(e.target.value)}
                placeholder="要对方交付什么？（Enter 派发，Shift+Enter 换行）"
                aria-label="任务指令"
                rows={2}
                onKeyDown={(e) => {
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
        <div className="hub-composer-foot">
          <span className="hub-view-toggle" role="tablist" aria-label="输入模式">
            <button role="tab" aria-selected={composerMode === 'chat'} className={composerMode === 'chat' ? 'on' : ''} onClick={() => setComposerMode('chat')}>对话</button>
            <button role="tab" aria-selected={composerMode === 'assign'} className={composerMode === 'assign' ? 'on' : ''} onClick={() => setComposerMode('assign')}>派任务</button>
          </span>
          {composerMode === 'chat' ? (
            <span className="hub-muted hub-composer-hint">
              {members.length === 0
                ? '群还没有成员，消息只会记录。'
                : draft.trim() && mentionIds(draft, roster).length === 0 && !hasMentionAll(draft)
                  ? '仅记录，不触发回复（@成员 才会唤醒）'
                  : '输入 @ 提及成员 · Enter 发送，Shift+Enter 换行'}
            </span>
          ) : (
            <span className="hub-muted hub-composer-hint">派任务会创建账本任务，结果以任务卡回群。</span>
          )}
        </div>
        {composerMode === 'assign' && assignNote ? (
          <div className="hub-assign-note" role="status">{assignNote}</div>
        ) : null}
      </div>
    </section>
  )
}


function renderThread(
  items: ThreadItem[],
  names: Record<string, string>,
  grouped: boolean,
  roster: MentionAgent[],
  onSelectAgent: (id: string) => void,
) {
  const nodes: React.ReactNode[] = []
  let batch: HubMessage[] = []
  let batchKey = ''
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
      />,
    )
    batch = []
    batchKey = ''
  }
  for (const item of items) {
    if (item.kind === 'round') {
      flush()
      const r = item.round
      const targets = (r.targets ?? []).map((id) => names[id] ?? id)
      nodes.push(
        <div key={`round-${r.id}`} className="hub-action routing" role="status">
          <span className="hub-action-kind">路由</span>
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
      nodes.push(<GroupBubble key={`m-${item.msg.seq}`} msg={item.msg} names={names} roster={roster} onSelectAgent={onSelectAgent} />)
      continue
    }
    const key = roundKey(item.msg)
    if (batch.length > 0 && (key === '' || key !== batchKey)) flush()
    if (batch.length === 0) batchKey = key
    batch.push(item.msg)
  }
  flush()
  return nodes
}


function MemberStack({ members, names }: { members: HubGroupMember[]; names: Record<string, string> }) {
  return (
    <span className="hub-member-stack" aria-label={`${members.length} 名成员`}>
      {members.slice(0, 4).map((m, i) => (
        <span key={m.agent_id} className="hub-member-dot" style={{ zIndex: 4 - i }} title={`${names[m.agent_id] ?? m.agent_id}${m.role ? ` · ${m.role}` : ''}`}>
          {(names[m.agent_id] ?? m.agent_id).slice(0, 1)}
        </span>
      ))}
      {members.length > 4 ? <span className="hub-member-dot more">+{members.length - 4}</span> : null}
    </span>
  )
}

// 避免把 i18n hook 放到组件外：这里只做轻量包装（GroupView 内部已用 useHubStore）。
function useT() {
  return (key: string): string => {
    const dict: Record<string, string> = {
      'hub.groupIntro': '群内协作从这里开始。',
      'hub.groupHint': '用 @名字 唤醒成员；@all 仅群主/人类可用。',
      'hub.groupPlaceholder': '发消息，用 @名字 指定谁来做…',
      'hub.send': '发送',
    }
    return dict[key] ?? key
  }
}
