import { useMemo, useState } from 'react'
import { useHubStore } from './useHubStore'
import type { HubNeed } from './types'

// Needs you：审批 / 提问 / 失败 / 交接 / 预算的聚合收件箱（§6.10）。
//
// 只做「路由 + 决策」：审批调用 Session.Approve（锚 = 调用 id），提问调用
// Session.AnswerQuestions（锚 = 批次 id）；failure/budget 只标记已读。
export default function NeedsYouPanel() {
  const needs = useHubStore((s) => s.needs)
  const loading = useHubStore((s) => s.needsLoading)
  const resolve = useHubStore((s) => s.resolveNeed)
  const load = useHubStore((s) => s.loadNeeds)
  const [busy, setBusy] = useState<string | null>(null)

  async function run(need: HubNeed, resolution: string, answers?: Record<string, string>): Promise<void> {
    setBusy(need.id)
    await resolve(need.id, resolution, answers)
    setBusy(null)
  }

  return (
    <section className="hub-panel" aria-label="Needs you">
      <header className="hub-panel-head">
        <strong>Needs you</strong>
        <span className="hub-muted">{needs.length}</span>
        <button className="hub-link" onClick={() => void load()} title="刷新">↻</button>
      </header>
      {loading ? <div className="hub-muted hub-pad">加载中…</div> : null}
      {!loading && needs.length === 0 ? (
        <div className="hub-empty hub-pad">
          <p>没有待处理项</p>
          <span className="hub-muted">审批、提问、失败会出现在这里</span>
        </div>
      ) : null}
      <div className="hub-panel-list">
        {needs.map((need) => (
          <NeedRow
            key={need.id}
            need={need}
            busy={busy === need.id}
            onResolve={(resolution, answers) => void run(need, resolution, answers)}
          />
        ))}
      </div>
    </section>
  )
}

function NeedRow({
  need,
  busy,
  onResolve,
}: {
  need: HubNeed
  busy: boolean
  onResolve: (resolution: string, answers?: Record<string, string>) => void
}) {
  const questions = useMemo(() => parseQuestions(need), [need])
  const alerts = useHubStore((s) => s.alerts)
  const needAlerts = need.task_id ? alerts.filter((a) => a.task_id === need.task_id) : []
  const [answers, setAnswers] = useState<Record<string, string>>({})
  const kindLabel: Record<string, string> = {
    approval: '审批', question: '提问', failure: '失败', handoff: '交接', budget: '预算',
  }
  return (
    <article className={`hub-need hub-need-${need.kind ?? 'other'}`}>
      <header className="hub-need-head">
        <span className={`hub-chip ${need.kind ?? ''}`}>{kindLabel[need.kind ?? ''] ?? need.kind ?? '事项'}</span>
        <strong>{need.title || need.kind || '待处理'}</strong>
      </header>
      {/* 提问类：body 就是 questions JSON。能解析出问题时，输入框才是正文 ——
          再把原始 JSON 一并铺出来会占掉半屏，看起来像渲染了两遍（HUB-NEED-01）。
          解析不出来（老格式/纯文本）才退回原始 body，否则用户什么都看不到。 */}
      {need.body && !(need.kind === 'question' && questions.length > 0) ? <HubNeedBody body={need.body} /> : null}
      {needAlerts.length > 0 ? (
        <div className="hub-task-alerts">
          {needAlerts.slice(0, 3).map((a) => (
            <div key={a.id} className={`hub-alert-row ${a.severity ?? 'warning'}`}>
              <span className="hub-alert-dot" />
              <span className="hub-alert-text">{a.message || a.code}</span>
            </div>
          ))}
        </div>
      ) : null}
      <footer className="hub-need-actions">
        {need.kind === 'approval' ? (
          <>
            <button className="hub-btn primary" disabled={busy} onClick={() => onResolve('approve')}>批准</button>
            <button className="hub-btn" disabled={busy} onClick={() => onResolve('deny')}>拒绝</button>
          </>
        ) : null}
        {need.kind === 'question' ? (
          questions.length > 0 ? (
            <div className="hub-need-questions">
              {questions.map((q, i) => (
                <label key={q.id || String(i)}>
                  <span>{q.question || q.text || `问题 ${i + 1}`}</span>
                  <input
                    value={answers[q.id || String(i)] ?? ''}
                    onChange={(e) => setAnswers((prev) => ({ ...prev, [q.id || String(i)]: e.target.value }))}
                    placeholder="输入回答"
                  />
                </label>
              ))}
              <button
                className="hub-btn primary"
                disabled={busy}
                onClick={() => onResolve('answered', answers)}
              >
                提交回答
              </button>
            </div>
          ) : (
            <button className="hub-btn primary" disabled={busy} onClick={() => onResolve('answered', answers)}>
              已回答（在会话里处理）
            </button>
          )
        ) : null}
        {need.kind === 'handoff' ? (
          <>
            <button className="hub-btn primary" disabled={busy} onClick={() => onResolve('accept')}>接受</button>
            <button className="hub-btn" disabled={busy} onClick={() => onResolve('decline')}>拒绝</button>
          </>
        ) : need.kind !== 'approval' && need.kind !== 'question' ? (
          <button className="hub-btn" disabled={busy} onClick={() => onResolve('acknowledged')}>标记已读</button>
        ) : null}
        {need.task_id ? <span className="hub-muted">任务 {need.task_id}</span> : null}
      </footer>
    </article>
  )
}

function HubNeedBody({ body }: { body: string }) {
  // 审批的 arguments / 提问的 questions JSON：能解析就折叠展示，解析不了就纯文本。
  let pretty = body
  try {
    pretty = JSON.stringify(JSON.parse(body), null, 2)
  } catch {
    // 纯文本保持原样
  }
  return <pre className="hub-need-body">{pretty.length > 1200 ? `${pretty.slice(0, 1200)}…` : pretty}</pre>
}

function parseQuestions(need: HubNeed): Array<{ id?: string; question?: string; text?: string }> {
  if (need.kind !== 'question' || !need.body) return []
  try {
    const raw = JSON.parse(need.body) as unknown
    if (Array.isArray(raw)) {
      return raw.filter((v): v is { id?: string; question?: string; text?: string } => !!v && typeof v === 'object')
    }
  } catch {
    return []
  }
  return []
}
