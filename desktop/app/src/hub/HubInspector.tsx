import { useMemo, useState } from 'react'
import { AlertIcon, Bot, CheckList, PanelRight, PencilIcon } from '../components/icons'
import { useT } from '../i18n'
import { useAppStore } from '../store/useAppStore'
import HubAvatar from './HubAvatar'
import { accentOfGroup } from './agentAccent'
import { useHubStore } from './useHubStore'
import NeedsYouPanel from './NeedsYouPanel'
import ContextPanel from './ContextPanel'
import type { HubTask } from './types'
import { taskMarkdown } from './exportMarkdown'
import { downloadText, safeFilename, timestampSlug } from './download'

// Hub 右栏：概览（默认）/ 任务板 / Needs you / 会话信息 / Context（只读）。
//
// P4-UI-1 起右栏常驻：没有选中页签时显示概览（头像/名字/角色/模型/运行态/任务/Needs），
// 不再是空白。编辑按钮打开 App 级 AgentEditor（store.editor）。
export default function HubInspector() {
  const t = useT()
  const panel = useHubStore((s) => s.inspector)
  const setPanel = useHubStore((s) => s.setInspector)
  const tasks = useHubStore((s) => s.tasks)
  const needs = useHubStore((s) => s.needs)
  const rightExpanded = useAppStore((s) => s.rightExpanded)
  const setRightExpanded = useAppStore((s) => s.setRightExpanded)
  const activeTasks = tasks.filter((task) => !isTerminal(task.status)).length

  // 折叠态：右栏只剩 44px 图标条。Hub 打开时 RightPanel 被隐藏，
  // 展开/切页签必须由这里自己提供（否则用户把右栏收起后进不去详情）。
  if (!rightExpanded) {
    return (
      <aside className="right collapsed hub-detail-rail" aria-label="Hub 详情">
        <div className="rail right-rail" style={{ flex: 1 }}>
          <button
            className="rbtn"
            data-tip={t('topbar.toggleRight')}
            aria-label={t('topbar.toggleRight')}
            onClick={() => setRightExpanded(true)}
          >
            <PanelRight size={15} />
          </button>
          <button
            className={`rbtn ${panel == null ? 'on' : ''}`}
            data-tip="概览"
            aria-label="概览"
            onClick={() => setPanel(null)}
          >
            <Bot size={15} />
          </button>
          <button
            className={`rbtn ${panel === 'tasks' ? 'on' : ''}`}
            data-tip="任务"
            aria-label="任务"
            onClick={() => { setPanel('tasks'); setRightExpanded(true) }}
          >
            <CheckList size={15} />
            {activeTasks > 0 ? <span className="todo-badge mono">{activeTasks}</span> : null}
          </button>
          <button
            className={`rbtn ${panel === 'needs' ? 'on' : ''}`}
            data-tip="Needs you"
            aria-label="Needs you"
            onClick={() => { setPanel('needs'); setRightExpanded(true) }}
          >
            <AlertIcon size={15} />
            {needs.length > 0 ? <span className="todo-badge mono">{needs.length}</span> : null}
          </button>
        </div>
      </aside>
    )
  }

  const current = panel ?? 'overview'
  return (
    <aside className="right expanded hub-inspector hub-detail" aria-label="Hub 详情">
      <nav className="hub-inspector-tabs hub-detail-tabs" role="tablist" aria-label="Hub 详情页签">
        <button role="tab" aria-selected={current === 'overview'} className={current === 'overview' ? 'on' : ''} onClick={() => setPanel(null)}>概览</button>
        <button role="tab" aria-selected={current === 'tasks'} className={current === 'tasks' ? 'on' : ''} onClick={() => setPanel('tasks')}>
          任务{activeTasks > 0 ? <span className="hub-badge">{activeTasks}</span> : null}
        </button>
        <button role="tab" aria-selected={current === 'needs'} className={current === 'needs' ? 'on' : ''} onClick={() => setPanel('needs')}>
          Needs{needs.length > 0 ? <span className="hub-badge alert">{needs.length}</span> : null}
        </button>
        <button role="tab" aria-selected={current === 'session'} className={current === 'session' ? 'on' : ''} onClick={() => setPanel('session')}>会话</button>
        <button role="tab" aria-selected={current === 'context'} className={current === 'context' ? 'on' : ''} onClick={() => setPanel('context')}>Context</button>
        <button
          className="hub-link hub-inspector-close"
          onClick={() => setRightExpanded(false)}
          title="收起右栏"
          aria-label="收起右栏"
        >
          <PanelRight size={13} />
        </button>
      </nav>
      {current === 'overview' ? <OverviewPanel /> : null}
      {current === 'tasks' ? <TaskBoard /> : null}
      {current === 'needs' ? <NeedsYouPanel /> : null}
      {current === 'session' ? <SessionPanel /> : null}
      {current === 'context' ? <ContextPanel /> : null}
    </aside>
  )
}

function isTerminal(status?: string): boolean {
  return ['completed', 'failed', 'interrupted', 'delivered', 'canceled', 'blocked_budget', 'declined'].includes(status ?? '')
}

function OverviewPanel() {
  const t = useT()
  const activeId = useHubStore((s) => s.activeId)
  const profile = useHubStore((s) => s.profile)
  const agents = useHubStore((s) => s.agents)
  const running = useHubStore((s) => s.running)
  const groups = useHubStore((s) => s.groups)
  const activeGroupId = useHubStore((s) => s.activeGroupId)
  const groupConversations = useHubStore((s) => s.groupConversations)
  const groupInfo = useHubStore((s) => s.groupInfo)
  const sid = useHubStore((s) => s.sid)
  const messages = useHubStore((s) => s.messages)
  const tasks = useHubStore((s) => s.tasks)
  const needs = useHubStore((s) => s.needs)
  const openEditor = useHubStore((s) => s.openEditor)
  const copyDeepLink = useHubStore((s) => s.copyDeepLink)

  const group = groups.find((g) => g.id === activeGroupId) ?? null
  const agent = agents.find((a) => a.id === activeId) ?? profile
  const activeTasks = tasks.filter((task) => !isTerminal(task.status)).length

  if (group) {
    const members = group.members ?? []
    const conv = groupConversations[group.id]
    const groupRunning = conv?.status === 'running'
    const unread = groupInfo[group.id]?.unread ?? 0
    const groupAccent = accentOfGroup(group, agents)
    return (
      <section className="hub-panel hub-overview" aria-label="群组概览">
        <div className="hub-ov-hero">
          <HubAvatar agent={{ name: group.name }} accent={groupAccent} size={44} ring />
          <div className="hub-ov-id">
            <strong>{group.name}</strong>
            <span>{members.length} 名成员 · {group.policy?.mode ?? 'ask'}</span>
          </div>
          <span className={`hub-ov-status ${groupRunning ? 'on' : ''}`}>{groupRunning ? t('hub.running') : t('hub.idle')}</span>
        </div>
        <p className="hub-ov-desc">
          {group.deleted ? '这个群已删除（历史仍可查看）。' : '自动对话与 @ 路由按群策略执行；成员由群主管理。'}
        </p>
        <dl className="hub-session-info">
          <dt>成员</dt><dd>{members.length}</dd>
          <dt>群主</dt><dd>{group.owner_agent_id ? (agents.find((a) => a.id === group.owner_agent_id)?.name ?? group.owner_agent_id) : '—'}</dd>
          <dt>日预算</dt><dd>${(group.policy?.daily_budget_usd ?? 0).toFixed(0)}{(group.policy?.daily_budget_usd ?? 0) > 0 ? '' : '（不限）'}</dd>
          <dt>未读</dt><dd>{unread > 0 ? unread : '无'}</dd>
          <dt>进行中任务</dt><dd>{activeTasks}</dd>
          <dt>Needs</dt><dd>{needs.length}</dd>
        </dl>
        <div className="hub-ov-members">
          <span className="hub-ov-label">成员</span>
          {members.map((m) => {
            const member = agents.find((a) => a.id === m.agent_id)
            return (
              <div className="hub-ov-member" key={m.agent_id}>
                <HubAvatar agent={member ?? { id: m.agent_id, name: m.agent_id }} size={24} ring={running[m.agent_id] === true} />
                <span className="hub-ov-member-name">{member?.name ?? m.agent_id}</span>
                <span className="hub-sub">{m.role || member?.role || ''}</span>
                {running[m.agent_id] ? <span className="hub-ov-run" title={t('hub.running')} /> : null}
              </div>
            )
          })}
        </div>
        <footer className="hub-ov-actions">
          <button className="hub-btn" onClick={() => void copyDeepLink('group', group.id)}>复制链接</button>
        </footer>
      </section>
    )
  }

  if (!agent) {
    return (
      <section className="hub-panel hub-overview" aria-label="Hub 概览">
        <div className="hub-empty hub-pad">
          <Bot size={26} />
          <p>还没有选中会话</p>
          <span className="hub-muted">从左栏选一个智能体或群组，这里会显示详情。</span>
        </div>
      </section>
    )
  }

  const isRunning = activeId ? running[activeId] === true : false
  return (
    <section className="hub-panel hub-overview" aria-label="智能体概览">
      <div className="hub-ov-hero">
        <HubAvatar agent={agent} size={44} ring />
        <div className="hub-ov-id">
          <strong>{agent.name}</strong>
          <span>{agent.role || '智能体'}</span>
        </div>
        <span className={`hub-ov-status ${isRunning ? 'on' : ''}`}>{isRunning ? t('hub.running') : t('hub.idle')}</span>
      </div>
      <div className="hub-ov-model" title="Provider · 模型 · 推理档位">
        {[agent.provider, agent.model, agent.effort || '默认档位'].filter(Boolean).join(' · ')}
      </div>
      <p className="hub-ov-desc">{agent.description || agent.role || '这个智能体还没有写简介。'}</p>
      <footer className="hub-ov-actions">
        <button className="hub-btn" onClick={() => void copyDeepLink('dm', agent.id)}>复制链接</button>
      </footer>
      <dl className="hub-session-info">
        <dt>会话</dt><dd className="mono">{sid ?? '未打开'}</dd>
        <dt>消息</dt><dd>{messages.length}</dd>
        <dt>版本</dt><dd>v{agent.version ?? 1}</dd>
        <dt>压缩阈值</dt><dd>{Math.round((agent.compress_threshold ?? 0.8) * 100)}%</dd>
        <dt>日预算</dt><dd>{(agent.policy?.daily_budget_usd ?? 0) > 0 ? `$${(agent.policy?.daily_budget_usd ?? 0).toFixed(0)}` : '不限'}</dd>
        <dt>进行中任务</dt><dd>{activeTasks}</dd>
        <dt>Needs</dt><dd>{needs.length}</dd>
      </dl>
      <p className="hub-muted hub-ov-note">人格、工具、记忆与模型参数在「编辑」里；本页只读。</p>
      <footer className="hub-ov-actions hub-ov-foot">
        <button className="hub-btn primary" onClick={() => openEditor(agent)}>
          <PencilIcon size={12} /> {t('hub.editAgent')}
        </button>
      </footer>
    </section>
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
  const terminal = isTerminal(task.status)
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
