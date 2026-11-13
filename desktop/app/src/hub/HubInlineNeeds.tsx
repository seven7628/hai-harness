import { useMemo, useState } from 'react'
import { useHubStore } from './useHubStore'
import { useAppStore } from '../store/useAppStore'
import type { HubNeed } from './types'

// Hub 单聊/群内的「任务被卡住」内联卡：审批 / 提问 / 待办等。
//
// 与 Needs you 抽屉共用同一份 need 数据与 resolve 路由，但**只展示、绝不写入
// 任何 hub 消息或智能体上下文**。Project 会话的审批正文/问题选项属于人看的信息，
// 不能因为「在群里出现」就被群快照带进其他智能体的上下文。

type Scope = { kind: 'dm'; agentId: string | null } | { kind: 'group'; groupId: string }

function needScopeMatch(need: HubNeed, scope: Scope): boolean {
  if (scope.kind === 'group') return need.group_id === scope.groupId
  // DM：只收没有群归属、且属于这个智能体的 need。项目会话的 need 在 bridge 里
  // 已把 agent_id 填成发起者，因此 DM 派出的 Project 审批也会命中。
  if (need.group_id) return false
  const owner = need.origin_agent_id || need.agent_id
  return !!scope.agentId && owner === scope.agentId
}

function attr(need: HubNeed): string {
  const who = need.origin_agent_name || need.origin_agent_id || need.agent_id || ''
  const ws = need.target_workspace || need.workspace || ''
  const sid = need.target_session_id || need.session_id || ''
  const project = ws ? `${ws.split('/').filter(Boolean).pop() ?? ws}${sid ? ` / ${sid}` : ''}` : sid
  const sub = need.run_name ? `子智能体 ${need.run_name}` : ''
  return [who, project, sub].filter(Boolean).join(' · ')
}

interface Question {
  id?: string
  question?: string
  text?: string
  options?: Array<string | { label?: string; value?: string; description?: string }>
}

function questionsOf(need: HubNeed): Question[] {
  if (need.kind !== 'question' || !need.body) return []
  try {
    const raw = JSON.parse(need.body) as unknown
    if (Array.isArray(raw)) return raw.filter((v): v is Question => !!v && typeof v === 'object')
  } catch {
    // 老格式/纯文本：退回原始 body
  }
  return []
}

export default function HubInlineNeeds({ scope }: { scope: Scope }) {
  const needs = useHubStore((s) => s.needs)
  const resolve = useHubStore((s) => s.resolveNeed)
  const setInspector = useHubStore((s) => s.setInspector)
  const setRightExpanded = useAppStore((s) => s.setRightExpanded)
  const [busy, setBusy] = useState<string | null>(null)
  const [answers, setAnswers] = useState<Record<string, Record<string, string>>>({})

  const mine = useMemo(
    () => needs.filter((n) => n.status !== 'resolved' && needScopeMatch(n, scope)).slice(0, 3),
    [needs, scope],
  )

  if (mine.length === 0) return null

  async function run(need: HubNeed, resolution: string): Promise<void> {
    setBusy(need.id)
    await resolve(need.id, resolution, answers[need.id] ?? {})
    setBusy(null)
  }

  function openAll(): void {
    setInspector('needs')
    setRightExpanded(true)
  }

  return (
    <section className="hub-inline-needs" aria-label="任务等待你处理">
      {mine.map((need) => {
        const qs = questionsOf(need)
        const answer = answers[need.id] ?? {}
        const kind = need.kind ?? 'other'
        return (
          <article className={`hub-inline-need hub-inline-need--${kind}`} key={need.id}>
            <header>
              <span className={`hub-chip ${kind}`}>{kindLabel(kind)}</span>
              <strong>{need.title || kindLabel(kind)}</strong>
              <span className="hub-inline-need-attr" title={attr(need)}>{attr(need)}</span>
            </header>
            {need.body && qs.length === 0 ? (
              <pre className="hub-inline-need-body">{need.body.length > 240 ? `${need.body.slice(0, 240)}…` : need.body}</pre>
            ) : null}
            {kind === 'question' && qs.length > 0 ? (
              <div className="hub-inline-need-questions">
                {qs.map((q, i) => {
                  const qid = q.id || String(i)
                  const opts = (q.options ?? []).map((o) => (typeof o === 'string' ? o : (o.label || o.value || '')))
                  return (
                    <label key={qid}>
                      <span>{q.question || q.text || `问题 ${i + 1}`}</span>
                      <input
                        value={answer[qid] ?? ''}
                        onChange={(e) => setAnswers((prev) => ({ ...prev, [need.id]: { ...(prev[need.id] ?? {}), [qid]: e.target.value } }))}
                        placeholder="输入回答"
                      />
                      {opts.length > 0 ? (
                        <span className="hub-inline-need-options">
                          {opts.map((o) => (
                            <button
                              type="button"
                              key={o}
                              className={answer[qid] === o ? 'on' : ''}
                              onClick={() => setAnswers((prev) => ({ ...prev, [need.id]: { ...(prev[need.id] ?? {}), [qid]: o } }))}
                            >{o}</button>
                          ))}
                        </span>
                      ) : null}
                    </label>
                  )
                })}
              </div>
            ) : null}
            <footer>
              {kind === 'approval' ? (
                <>
                  <button className="hub-btn primary" disabled={busy === need.id} onClick={() => void run(need, 'approve')}>批准</button>
                  <button className="hub-btn" disabled={busy === need.id} onClick={() => void run(need, 'deny')}>拒绝</button>
                </>
              ) : kind === 'question' ? (
                <button className="hub-btn primary" disabled={busy === need.id} onClick={() => void run(need, 'answered')}>提交回答</button>
              ) : (
                <button className="hub-btn" disabled={busy === need.id} onClick={() => void run(need, 'acknowledged')}>标记已读</button>
              )}
              <button className="hub-link" onClick={openAll}>查看全部</button>
              {need.task_id ? <span className="hub-muted">{need.task_id}</span> : null}
            </footer>
          </article>
        )
      })}
    </section>
  )
}

function kindLabel(kind: string): string {
  return ({ approval: '审批', question: '提问', handoff: '交接', failure: '失败', budget: '预算' } as Record<string, string>)[kind] ?? '事项'
}
