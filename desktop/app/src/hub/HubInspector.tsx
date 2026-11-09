import { useMemo, useState } from 'react'
import { useHubStore } from './useHubStore'
import NeedsYouPanel from './NeedsYouPanel'
import ContextPanel from './ContextPanel'
import type { HubTask } from './types'
import { taskMarkdown } from './exportMarkdown'
import { downloadText, safeFilename, timestampSlug } from './download'

// Hub Inspector：任务板 / Needs you / 会话信息 / Context（只读）。
//
// 任务板读账本（hub_task_list/get），取消/重试走 Queue 语义；会话信息只展示
// 真实运行态与账本用量，导出是纯前端（JSON / 任务 Markdown），清空走 hub_clear。
// Context 页签只读（P4-02）：消息触发上下文 + 系统提示词分层，不提供任何写入。
export default function HubInspector() {
  const panel = useHubStore((s) => s.inspector)
  const setPanel = useHubStore((s) => s.setInspector)
  if (!panel) return null
  return (
    <aside className="hub-inspector" aria-label="Hub inspector">
      <nav className="hub-inspector-tabs" role="tablist">
        <button role="tab" aria-selected={panel === 'tasks'} className={panel === 'tasks' ? 'on' : ''} onClick={() => setPanel('tasks')}>任务</button>
        <button role="tab" aria-selected={panel === 'needs'} className={panel === 'needs' ? 'on' : ''} onClick={() => setPanel('needs')}>Needs you</button>
        <button role="tab" aria-selected={panel === 'session'} className={panel === 'session' ? 'on' : ''} onClick={() => setPanel('session')}>会话</button>
        <button role="tab" aria-selected={panel === 'context'} className={panel === 'context' ? 'on' : ''} onClick={() => setPanel('context')}>Context</button>
        <button className="hub-link hub-inspector-close" onClick={() => setPanel(null)} aria-label="关闭">×</button>
      </nav>
      {panel === 'tasks' ? <TaskBoard /> : null}
      {panel === 'needs' ? <NeedsYouPanel /> : null}
      {panel === 'session' ? <SessionPanel /> : null}
      {panel === 'context' ? <ContextPanel /> : null}
    </aside>
  )
}

function TaskBoard() {
  const tasks = useHubStore((s) => s.tasks)
  const loading = useHubStore((s) => s.tasksLoading)
  const load = useHubStore((s) => s.loadTasks)
  const cancel = useHubStore((s) => s.cancelTask)
  const retry = useHubStore((s) => s.retryTask)
  const [open, setOpen] = useState<string | null>(null)
  const [status, setStatus] = useState('')
  const shown = useMemo(() => (status ? tasks.filter((t) => t.status === status) : tasks), [tasks, status])
  return (
    <section className="hub-panel" aria-label="任务板">
      <header className="hub-panel-head">
        <strong>任务</strong>
        <span className="hub-muted">{shown.length}</span>
        <select value={status} onChange={(e) => setStatus(e.target.value)} aria-label="按状态筛选">
          <option value="">全部</option>
          <option value="queued">排队</option>
          <option value="dispatched">已派发</option>
          <option value="running">运行中</option>
          <option value="completed">已完成</option>
          <option value="failed">失败</option>
          <option value="interrupted">已中断</option>
          <option value="delivered">已回推</option>
          <option value="canceled">已取消</option>
        </select>
        <button className="hub-link" onClick={() => void load()} title="刷新">↻</button>
      </header>
      {loading ? <div className="hub-muted hub-pad">加载中…</div> : null}
      {!loading && shown.length === 0 ? (
        <div className="hub-empty hub-pad">
          <p>还没有任务</p>
          <span className="hub-muted">智能体用 session_send 派出的任务会出现在这里</span>
        </div>
      ) : null}
      <div className="hub-panel-list">
        {shown.map((task) => (
          <TaskRow
            key={task.id}
            task={task}
            open={open === task.id}
            onToggle={() => setOpen(open === task.id ? null : task.id)}
            onCancel={() => void cancel(task.id)}
            onRetry={() => void retry(task.id)}
          />
        ))}
      </div>
    </section>
  )
}

function TaskRow({
  task,
  open,
  onToggle,
  onCancel,
  onRetry,
}: {
  task: HubTask
  open: boolean
  onToggle: () => void
  onCancel: () => void
  onRetry: () => void
}) {
  const alerts = useHubStore((s) => s.alerts)
  const agents = useHubStore((s) => s.agents)
  const taskAlerts = alerts.filter((a) => a.task_id === task.id)
  const terminal = ['completed', 'failed', 'interrupted', 'delivered', 'canceled', 'blocked_budget', 'declined'].includes(task.status ?? '')
  const usage = task.result?.usage
  const cost = usage?.cost_priced ? `$${(usage.cost_usd ?? 0).toFixed(4)}` : '成本未知'
  return (
    <article className={`hub-task ${open ? 'open' : ''}`}>
      <button className="hub-task-head" onClick={onToggle} aria-expanded={open}>
        <span className={`hub-chip task-${task.status ?? 'unknown'}`}>{statusLabel(task.status)}</span>
        <strong>{task.title || task.instruction?.slice(0, 80) || task.id}</strong>
      </button>
      <div className="hub-task-meta">
        <span className="hub-muted">{task.target?.workspace ?? ''}{task.target?.session_id ? ` · ${task.target.session_id}` : ''}</span>
        {task.attempt && task.attempt > 1 ? <span className="hub-muted">第 {task.attempt} 次</span> : null}
      </div>
      {open ? (
        <div className="hub-task-detail">
          {task.instruction ? <pre className="hub-need-body">{task.instruction}</pre> : null}
          {task.result?.summary ? <div className="hub-task-summary"><MarkdownLite text={task.result.summary} /></div> : null}
          {task.error ? <p className="hub-error-text">{task.error}</p> : null}
          {taskAlerts.length > 0 ? (
            <div className="hub-task-alerts">
              {taskAlerts.slice(0, 3).map((a) => (
                <div key={a.id} className={`hub-alert-row ${a.severity ?? 'warning'}`}>
                  <span className="hub-alert-dot" />
                  <span className="hub-alert-text">{a.message || a.code}</span>
                </div>
              ))}
            </div>
          ) : null}
          {task.result?.artifacts?.length ? (
            <ul className="hub-artifacts">
              {task.result.artifacts.map((a, i) => (
                <li key={`${a.path ?? a.name ?? i}`}><span>{a.kind === 'created' ? '新增' : '修改'}</span> {a.path || a.name}</li>
              ))}
            </ul>
          ) : null}
          <div className="hub-task-meta">
            <span>{cost}</span>
            {usage ? <span className="hub-muted">↑{usage.input ?? 0} ↓{usage.output ?? 0}</span> : null}
            <span className="hub-muted">{task.id}</span>
          </div>
          <footer className="hub-need-actions">
            {!terminal ? <button className="hub-btn" onClick={onCancel}>取消</button> : null}
            {terminal ? <button className="hub-btn" onClick={onRetry}>重试</button> : null}
            <button
              className="hub-btn"
              onClick={() => {
                const names: Record<string, string> = {}
                for (const a of agents) names[a.id] = a.name || a.id
                const md = taskMarkdown(task, { agentNames: names, exportedAt: new Date().toISOString() })
                downloadText(`hub-task-${safeFilename(task.id, 'task')}-${timestampSlug()}.md`, md)
              }}
              title="导出这个任务的 Markdown（含结果/产物/用量）"
            >
              导出 MD
            </button>
          </footer>
        </div>
      ) : null}
    </article>
  )
}

function SessionPanel() {
  const agentId = useHubStore((s) => s.activeId)
  const agent = useHubStore((s) => s.agents.find((a) => a.id === s.activeId) ?? s.profile)
  const sid = useHubStore((s) => s.sid)
  const running = useHubStore((s) => (s.activeId ? s.running[s.activeId] === true : false))
  const tasks = useHubStore((s) => s.tasks)
  const messages = useHubStore((s) => s.messages)
  const clear = useHubStore((s) => s.clearContext)
  const usage = useMemo(() => tasks.reduce(
    (acc, t) => {
      const u = t.result?.usage
      if (!u) return acc
      acc.input += u.input ?? 0
      acc.output += u.output ?? 0
      if (u.cost_priced) acc.cost += u.cost_usd ?? 0
      else acc.unpriced += 1
      return acc
    },
    { input: 0, output: 0, cost: 0, unpriced: 0 },
  ), [tasks])
  function exportJSON(): void {
    const payload = { agent: agent?.name, agent_id: agentId, session_id: sid, exported_at: new Date().toISOString(), messages }
    // 与 Markdown 导出共用同一个下载入口（Blob + 临时 a download）。
    downloadText(
      `hub-${safeFilename(agentId ?? 'dm', 'dm')}-${timestampSlug()}.json`,
      JSON.stringify(payload, null, 2),
      'application/json',
    )
  }
  return (
    <section className="hub-panel" aria-label="会话信息">
      <header className="hub-panel-head"><strong>会话</strong><span className="hub-muted">{running ? '运行中' : '空闲'}</span></header>
      <dl className="hub-session-info">
        <dt>智能体</dt><dd>{agent?.name ?? agentId ?? '—'}</dd>
        <dt>会话</dt><dd>{sid ?? '未打开'}</dd>
        <dt>模型</dt><dd>{[agent?.provider, agent?.model].filter(Boolean).join(' · ') || '—'}</dd>
        <dt>本轮消息</dt><dd>{messages.length}</dd>
        <dt>任务用量</dt><dd>↑{usage.input} ↓{usage.output} · {usage.cost > 0 ? `$${usage.cost.toFixed(4)}` : '成本未知'}{usage.unpriced ? `（${usage.unpriced} 条无价）` : ''}</dd>
      </dl>
      <footer className="hub-need-actions">
        <button className="hub-btn" onClick={exportJSON} disabled={!messages.length}>导出记录</button>
        <button className="hub-btn" onClick={() => void clear()} disabled={running}>清空上下文</button>
      </footer>
      <p className="hub-muted hub-pad">清空保留 system 提示词；运行中不可清空。</p>
    </section>
  )
}

function statusLabel(status?: string): string {
  const map: Record<string, string> = {
    queued: '排队', dispatched: '已派发', running: '运行中', completed: '完成', delivered: '已回推',
    failed: '失败', interrupted: '中断', canceled: '取消', blocked_budget: '超预算', declined: '拒绝',
  }
  return map[status ?? ''] ?? status ?? '未知'
}

function MarkdownLite({ text }: { text: string }) {
  return <p className="hub-task-summary-text">{text}</p>
}
