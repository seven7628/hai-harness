import { useMemo, useState } from 'react'
import { useHubStore } from './useHubStore'
import { useAppStore } from '../store/useAppStore'
import {
  composeAnswers,
  parseInlineQuestions,
  previewOf,
  type InlineNeedQuestion,
} from './inlineNeedAnswers'
import type { HubNeed } from './types'

// Hub 单聊/群内的「任务被卡住」内联卡：审批 / 提问 / 待办等。
//
// 与 Needs you 抽屉共用同一份 need 数据与 resolve 路由，但**只展示、绝不写入
// 任何 hub 消息或智能体上下文**。Project 会话的审批正文/问题选项属于人看的信息，
// 不能因为「在群里出现」就被群快照带进其他智能体的上下文。
//
// 提问卡的口径与 AskUserQuestionPanel 一致（N3）：多选可同时勾选多个，提交时用
// 「；」拼接；选项自带 preview 时显示在选项下方 —— 内联卡空间小，但「必须看见
// 才能选」的备选不能因为地方窄就只剩标签。

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

export default function HubInlineNeeds({ scope }: { scope: Scope }) {
  const needs = useHubStore((s) => s.needs)
  const resolve = useHubStore((s) => s.resolveNeed)
  const setInspector = useHubStore((s) => s.setInspector)
  const setRightExpanded = useAppStore((s) => s.setRightExpanded)
  const [busy, setBusy] = useState<string | null>(null)
  // 自由输入/单选值（按 need → question 两层）；多选另外记一份勾选集合，两者在提交时
  // 由 composeAnswers 合成 —— 勾选不能覆盖用户已经打过字的自由输入。
  const [answers, setAnswers] = useState<Record<string, Record<string, string>>>({})
  const [picks, setPicks] = useState<Record<string, Record<string, string[]>>>({})

  const mine = useMemo(
    () => needs.filter((n) => n.status !== 'resolved' && needScopeMatch(n, scope)).slice(0, 3),
    [needs, scope],
  )

  if (mine.length === 0) return null

  function setText(needId: string, qid: string, value: string): void {
    setAnswers((prev) => ({ ...prev, [needId]: { ...(prev[needId] ?? {}), [qid]: value } }))
  }

  // 多选：点一下切换勾选（不是覆盖上一个选项）。
  function toggle(needId: string, qid: string, label: string): void {
    setPicks((prev) => {
      const cur = prev[needId]?.[qid] ?? []
      const next = cur.includes(label) ? cur.filter((x) => x !== label) : [...cur, label]
      return { ...prev, [needId]: { ...(prev[needId] ?? {}), [qid]: next } }
    })
  }

  async function run(need: HubNeed, qs: InlineNeedQuestion[], resolution: string): Promise<void> {
    setBusy(need.id)
    await resolve(need.id, resolution, composeAnswers(qs, picks[need.id], answers[need.id]))
    setBusy(null)
  }

  function openAll(): void {
    setInspector('needs')
    setRightExpanded(true)
  }

  return (
    <section className="hub-inline-needs" aria-label="任务等待你处理">
      {mine.map((need) => {
        const qs = parseInlineQuestions(need.body)
        const typed = answers[need.id] ?? {}
        const kind = need.kind ?? 'other'
        const target = need.target_session_id || need.session_id || ''
        const submitTitle = target ? `提交到 ${target}` : undefined
        return (
          <article className={`hub-inline-need hub-inline-need--${kind}`} key={need.id}>
            <header>
              <span className={`hub-chip ${kind}`}>{kindLabel(kind)}</span>
              <strong title={need.title || kindLabel(kind)}>{need.title || kindLabel(kind)}</strong>
              <span className="hub-inline-need-attr" title={attr(need)}>{attr(need)}</span>
            </header>
            {need.body && qs.length === 0 ? (
              <pre className="hub-inline-need-body">{need.body.length > 240 ? `${need.body.slice(0, 240)}…` : need.body}</pre>
            ) : null}
            {kind === 'question' && qs.length > 0 ? (
              <div className="hub-inline-need-questions">
                {qs.map((q) => {
                  const picked = picks[need.id]?.[q.id] ?? []
                  const value = typed[q.id] ?? ''
                  // 单选/自由输入的「选中项」就是那个值本身；多选才是勾选集合。
                  const chosen = q.multi ? picked : (value ? [value] : [])
                  const preview = previewOf(q, chosen)
                  return (
                    <div className="hub-inline-need-question" key={q.id}>
                      <span className="hub-inline-need-q">
                        {q.text}
                        {q.multi ? <span className="hub-inline-need-multi">可多选</span> : null}
                      </span>
                      <input
                        value={value}
                        aria-label={q.text}
                        onChange={(e) => setText(need.id, q.id, e.target.value)}
                        placeholder={q.multi ? '补充说明（可留空）' : '输入回答'}
                      />
                      {q.options.length > 0 ? (
                        <span className="hub-inline-need-options">
                          {q.options.map((o) => {
                            const on = q.multi ? picked.includes(o.label) : value === o.label
                            return (
                              <button
                                type="button"
                                key={o.label}
                                className={on ? 'on' : ''}
                                aria-pressed={on}
                                title={o.preview ? `预览：${o.label}` : o.label}
                                onClick={() => {
                                  if (q.multi) toggle(need.id, q.id, o.label)
                                  else setText(need.id, q.id, o.label)
                                }}
                              >{o.label}</button>
                            )
                          })}
                        </span>
                      ) : null}
                      {preview ? (
                        <pre
                          className="hub-inline-need-preview"
                          aria-label={`选项「${preview.label}」的预览`}
                        >{preview.text}</pre>
                      ) : null}
                    </div>
                  )
                })}
              </div>
            ) : null}
            <footer>
              {kind === 'approval' ? (
                <>
                  <button className="hub-btn primary" title={submitTitle} disabled={busy === need.id} onClick={() => void run(need, qs, 'approve')}>批准</button>
                  <button className="hub-btn" title={submitTitle} disabled={busy === need.id} onClick={() => void run(need, qs, 'deny')}>拒绝</button>
                </>
              ) : kind === 'question' ? (
                <button className="hub-btn primary" title={submitTitle} disabled={busy === need.id} onClick={() => void run(need, qs, 'answered')}>提交回答</button>
              ) : (
                <button className="hub-btn" title={submitTitle} disabled={busy === need.id} onClick={() => void run(need, qs, 'acknowledged')}>标记已读</button>
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
