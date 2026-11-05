import { useMemo, useState } from 'react'
import { Markdown } from '../components/markdown'
import { SendIcon } from '../components/icons'
import { useHubStore } from './useHubStore'
import type { HubGroup, HubGroupMember, HubMessage } from './types'

// 群组视图（P3a-08）：群头（成员叠图）/ 时间线 / 路由回执 / Chat|Run / 发言。
//
// 本轮智能体回复由 P3b 的群会话接入；这里先保证人发消息、@ 路由回执、
// 审批占位与活动流是真实数据（都来自 bridge 的群日志/活动命令）。
export default function GroupView({ group }: { group: HubGroup }) {
  const t = useT()
  const messages = useHubStore((s) => s.groupMessages)
  const activity = useHubStore((s) => s.groupActivity)
  const round = useHubStore((s) => s.groupRound)
  const sending = useHubStore((s) => s.groupSending)
  const send = useHubStore((s) => s.sendGroup)
  const agents = useHubStore((s) => s.agents)
  const [draft, setDraft] = useState('')
  const [view, setView] = useState<'chat' | 'run'>('chat')

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

  async function submit(): Promise<void> {
    if (!draft.trim() || sending) return
    const ok = await send(draft)
    if (ok) setDraft('')
  }

  return (
    <section className="hub-dm hub-group-view">
      <header className="hub-dm-head">
        <MemberStack members={members} names={agentNames} />
        <div className="hub-dm-title">
          <strong>{group.name}</strong>
          <span className="hub-sub">{members.length} 名成员{title ? ` · ${title}` : ''}</span>
        </div>
        <span className={`hub-chip group-mode-${group.policy?.mode ?? 'ask'}`}>{group.policy?.mode ?? 'ask'}</span>
        <span className="hub-view-toggle" role="tablist" aria-label="Chat or Run">
          <button role="tab" aria-selected={view === 'chat'} className={view === 'chat' ? 'on' : ''} onClick={() => setView('chat')}>Chat</button>
          <button role="tab" aria-selected={view === 'run'} className={view === 'run' ? 'on' : ''} onClick={() => setView('run')}>Run</button>
        </span>
      </header>

      <div className="hub-list">
        {view === 'run' ? (
          <div className="hub-run">
            {activity.length === 0 ? (
              <div className="hub-empty"><p>还没有群活动</p><span className="hub-muted">@ 路由与工具活动会出现在这里</span></div>
            ) : activity.map((a, i) => (
              <div key={`${a.after_seq}-${i}`} className={`hub-run-row ${a.status ?? ''}`}>
                <span className="hub-chip">{a.kind === 'routing' ? '路由' : '工具'}</span>
                <span className="hub-run-name">{a.name || a.kind}</span>
                <span className="hub-muted">{a.summary}</span>
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
            {messages.map((m) => (
              <GroupBubble key={`${m.seq}-${m.clientMsgId ?? ''}`} msg={m} names={agentNames} />
            ))}
            {round ? (
              <div className="hub-action routing" role="status">
                <span className="hub-action-kind">路由</span>
                <span>
                  {round.mention_all ? '@all ' : ''}
                  {(round.targets ?? []).length === 0 ? '无人被唤醒' : `唤醒 ${(round.targets ?? []).map((id) => agentNames[id] ?? id).join('、')}`}
                  {round.dropped ? `（丢弃 ${round.dropped} 个）` : ''}
                </span>
              </div>
            ) : null}
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

function GroupBubble({ msg, names }: { msg: HubMessage; names: Record<string, string> }) {
  if (msg.kind === 'approval_stub') {
    return (
      <div className="hub-msg system hub-approval-stub" role="status">
        <span className="hub-chip">审批占位</span>
        <span>{msg.text || '有人发起审批，详情在 Needs you'}</span>
      </div>
    )
  }
  if (msg.kind === 'routing_receipt' || msg.sender?.kind === 'system') {
    return <div className="hub-msg system">{msg.text}</div>
  }
  const kind = msg.sender?.kind ?? 'system'
  const who = kind === 'human' ? '你' : (names[msg.sender?.agent_id ?? ''] ?? msg.sender?.agent_id ?? '智能体')
  return (
    <div className={`hub-msg ${kind === 'human' ? 'human' : 'agent'}`}>
      <div className="hub-group-author">{who}</div>
      <div className="hub-body">
        <Markdown text={msg.text ?? ''} />
      </div>
    </div>
  )
}

function MemberStack({ members, names }: { members: HubGroupMember[]; names: Record<string, string> }) {
  return (
    <span className="hub-member-stack" aria-label={`${members.length} 名成员`}>
      {members.slice(0, 4).map((m, i) => (
        <span key={m.agent_id} className="hub-member-dot" style={{ zIndex: 4 - i }} title={names[m.agent_id] ?? m.agent_id}>
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
