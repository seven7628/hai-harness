import { useMemo, useState } from 'react'
import { SendIcon } from '../components/icons'
import { useHubStore } from './useHubStore'
import { GroupBubble, parseRoutingText } from './MessageBubble'
import { RoundGroup, roundKey } from './RoundGroup'
import { RoundActivityDigest } from './RoundActivityDigest'
import type { HubGroup, HubGroupMember, HubMessage, HubRound } from './types'

// 群组视图（P3a-08）：群头（成员叠图）/ 时间线 / 路由动作 / Chat|Run / 发言。
// 同轮回复按 round_id（或同一组 trigger_seqs）合并；「分开」保留平铺。
type ThreadItem =
  | { kind: 'message'; msg: HubMessage }
  | { kind: 'round'; round: HubRound }

export default function GroupView({ group }: { group: HubGroup }) {
  const t = useT()
  const messages = useHubStore((s) => s.groupMessages)
  const activity = useHubStore((s) => s.groupActivity)
  const rounds = useHubStore((s) => s.groupRounds)
  const sending = useHubStore((s) => s.groupSending)
  const send = useHubStore((s) => s.sendGroup)
  const agents = useHubStore((s) => s.agents)
  const [draft, setDraft] = useState('')
  const [view, setView] = useState<'chat' | 'run'>('chat')
  const [displayMode, setDisplayMode] = useState<'grouped' | 'flat'>(() =>
    typeof localStorage !== 'undefined' && localStorage.getItem('hub.groupDisplay') === 'flat' ? 'flat' : 'grouped',
  )

  const members = group.members ?? []
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

  async function submit(): Promise<void> {
    if (!draft.trim() || sending) return
    const ok = await send(draft)
    if (ok) setDraft('')
  }

  function setDisplay(next: 'grouped' | 'flat'): void {
    setDisplayMode(next)
    if (typeof localStorage !== 'undefined') localStorage.setItem('hub.groupDisplay', next)
  }

  const items = useMemo(() => buildThreadItems(messages, rounds), [messages, rounds])

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
            {messages.length === 0 ? (
              <div className="hub-intro">
                <p>{t('hub.groupIntro')}</p>
                <span className="hub-muted">{t('hub.groupHint')}</span>
              </div>
            ) : null}
            {renderThread(items, agentNames, displayMode === 'grouped')}
          </div>
        )}
      </div>

      <div className="hub-composer">
        <textarea
          value={draft}
          placeholder={t('hub.groupPlaceholder')}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault()
              void submit()
            }
          }}
          rows={2}
        />
        <button className="hub-send" onClick={() => void submit()} disabled={sending || !draft.trim()} title={t('hub.send')} aria-label={t('hub.send')}>
          <SendIcon />
        </button>
      </div>
    </section>
  )
}

function buildThreadItems(messages: HubMessage[], rounds: HubRound[]): ThreadItem[] {
  const items: ThreadItem[] = []
  const bySeq = new Map<number, HubRound[]>()
  for (const r of rounds) {
    const seq = r.trigger_seq ?? 0
    if (!seq) continue
    bySeq.set(seq, [...(bySeq.get(seq) ?? []), r])
  }
  for (const msg of messages) {
    items.push({ kind: 'message', msg })
    for (const r of bySeq.get(msg.seq) ?? []) items.push({ kind: 'round', round: r })
  }
  return items
}

function renderThread(items: ThreadItem[], names: Record<string, string>, grouped: boolean) {
  const nodes: React.ReactNode[] = []
  let batch: HubMessage[] = []
  let batchKey = ''
  const flush = (): void => {
    if (batch.length === 0) return
    nodes.push(
      <RoundGroup key={`g-${batch[0].seq}-${batch[batch.length - 1].seq}`} messages={batch} names={names} grouped={grouped && !!batchKey} />,
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
      nodes.push(<GroupBubble key={`m-${item.msg.seq}`} msg={item.msg} names={names} />)
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

function routingSummary(text: string, names: Record<string, string>): string {
  const parsed = parseRoutingText(text)
  if (!parsed) return text
  const targets = parsed.targets.map((id) => names[id] ?? id)
  return `${parsed.all ? '@all ' : ''}${targets.length ? `唤醒 ${targets.join('、')}` : '无人被唤醒'}${parsed.dropped ? `（丢弃 ${parsed.dropped} 个）` : ''}`
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
