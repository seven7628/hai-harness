import { useEffect, useMemo, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import type { CronJob } from '../transport/types'
import { Back, Clock } from './icons'
import ConfirmModal from './ConfirmModal'
import Sel from './Sel'

// 把 5 字段 cron 转成可读描述（桌面端展示用；解析失败回显原文）
function describeCron(expr: string, t: (k: string) => string): string {
  const p = expr.trim().split(/\s+/)
  if (p.length !== 5) return expr
  const [mi, h, dom, mon, dow] = p
  const parts: string[] = []
  if (mi !== '*') parts.push(t('cron.desc.minute').replace('{mi}', mi))
  if (h !== '*') parts.push(t('cron.desc.hour').replace('{h}', h))
  if (dom !== '*') parts.push(t('cron.desc.dom').replace('{dom}', dom))
  if (mon !== '*') parts.push(t('cron.desc.month').replace('{mon}', mon))
  if (dow !== '*') parts.push(t('cron.desc.dow').replace('{dow}', dow))
  return parts.length ? parts.join(' · ') : t('cron.desc.everyMinute')
}

function statusText(t: (k: string) => string, s?: string): string {
  const map: Record<string, string> = {
    pending: t('cron.status.pending'),
    running: t('cron.status.running'),
    success: t('cron.status.success'),
    error: t('cron.status.error'),
    skipped: t('cron.status.skipped'),
    timeout: t('cron.status.timeout'),
  }
  return (s && map[s]) || '—'
}

function fmtTime(v?: string): string {
  if (!v) return '—'
  const d = new Date(v)
  return isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

const PAGE_SIZE = 10

export default function CronPage() {
  const t = useT()
  const closeCron = useAppStore((s) => s.closeCron)
  const fetchCron = useAppStore((s) => s.fetchCron)
  const openCronRuns = useAppStore((s) => s.openCronRuns)
  const deleteCron = useAppStore((s) => s.deleteCron)
  const updateCron = useAppStore((s) => s.updateCron)
  const openSettings = useAppStore((s) => s.openSettings)
  const jobs = useAppStore((s) => s.cronJobs)
  const enabled = useAppStore((s) => s.cronEnabled)

  const [confirm, setConfirm] = useState<CronJob | null>(null)
  const [editing, setEditing] = useState<CronJob | null>(null)
  const [draft, setDraft] = useState<{ prompt: string; cron: string; recurring: boolean; imChat: { gateway: string; gatewayId: string; chat_id: string } | null }>({
    prompt: '', cron: '', recurring: true, imChat: null,
  })
  const [page, setPage] = useState(1)
  // IM 推送目标选择：渠道（enabled gateway）→ 群（im_chat_list）
  const imConfig = useAppStore((s) => s.imConfig)
  const imChats = useAppStore((s) => s.imChats)
  const fetchIMChats = useAppStore((s) => s.fetchIMChats)
  const fetchIMConfig = useAppStore((s) => s.fetchIMConfig)
  const imGateways = useMemo(() => (imConfig?.gateways ?? []).filter((g) => g.enabled), [imConfig])

  // 进入 Cron 页时拉 IM 配置（渠道下拉数据源；设置页可能未打开过）
  useEffect(() => {
    fetchIMConfig()
  }, [fetchIMConfig])

  const totalPages = Math.max(1, Math.ceil(jobs.length / PAGE_SIZE))
  const cur = Math.min(page, totalPages)
  const pageJobs = useMemo(
    () => jobs.slice((cur - 1) * PAGE_SIZE, cur * PAGE_SIZE),
    [jobs, cur],
  )

  const startEdit = (j: CronJob) => {
    setEditing(j)
    setDraft({
      prompt: j.prompt, cron: j.cron, recurring: j.recurring,
      imChat: j.im_chat
        ? { gateway: j.im_chat.gateway, gatewayId: imGateways.find((g) => g.type === j.im_chat!.gateway)?.id ?? '', chat_id: j.im_chat.chat_id }
        : null,
    })
  }
  const saveEdit = () => {
    if (editing) {
      const patch: Record<string, unknown> = { prompt: draft.prompt, cron: draft.cron, recurring: draft.recurring }
      // im_chat：显式传（含 null=清除）；未选渠道也传 null 清除旧绑定
      patch.im_chat = draft.imChat ? { gateway: draft.imChat.gateway, chat_id: draft.imChat.chat_id } : null
      updateCron(editing.id, patch)
    }
    setEditing(null)
  }

  return (
    <div className="cron-page cron-list-page">
      <header className="cron-head">
        <button className="cron-head-back" onClick={closeCron}><Back /> {t('cron.back')}</button>
        <div className="cron-head-title">
          <h1><Clock size={16} /> {t('cron.title')}</h1>
          <span className="cron-head-sub">{t('cron.sub')}</span>
        </div>
        <span className="cron-head-grow" />
        <div className="cron-head-right">
          <span className="cron-head-count">{t('cron.count').replace('{n}', String(jobs.length))}</span>
          <button className="cron-head-refresh" onClick={fetchCron}>{t('cron.refresh')}</button>
        </div>
      </header>

      {!enabled && (
        <div className="cron-warn" style={{ padding: '10px 20px', background: 'var(--surface-2)', color: 'var(--warn)' }}>
          {t('cron.enabledOff')}
          <button className="btn link" onClick={openSettings}> {t('cron.settingsGeneral')} </button>
          {t('cron.enabledOff2')}
        </div>
      )}

      <div className="sp-body">
        <div className="sp-content">
          <div className="cron-card">
            {jobs.length === 0 ? (
              <div className="empty">{t('cron.empty')}</div>
            ) : (
              <>
                <table className="cron-table">
                  <thead>
                    <tr>
                      <th>{t('cron.th.name')}</th><th>{t('cron.th.schedule')}</th><th>{t('cron.th.status')}</th><th>{t('cron.th.lastRun')}</th>
                      <th>{t('cron.th.runCount')}</th><th>{t('cron.th.created')}</th><th>{t('cron.th.actions')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {pageJobs.map((j) => (
                      <tr
                        key={j.id}
                        className="cron-job-row"
                        title={t('cron.viewRuns')}
                        onClick={() => openCronRuns(j)}
                      >
                        <td>
                          <div className="cron-prompt">{j.prompt}</div>
                          <div className="mono" style={{ fontSize: 10.5, color: 'var(--text-3)' }}>{j.id}</div>
                        </td>
                        <td title={j.cron}>{describeCron(j.cron, t)}<div className="mono" style={{ fontSize: 10.5, color: 'var(--text-3)' }}>{j.recurring ? t('cron.recurring') : t('cron.once')}</div></td>
                        <td>
                          {j.paused ? (
                            <span className="cron-status paused">{t('cron.paused')}</span>
                          ) : (
                            <span className={`cron-status ${j.last_status ?? ''}`}>{statusText(t, j.last_status)}</span>
                          )}
                        </td>
                        <td>{fmtTime(j.last_run)}</td>
                        <td>{j.run_count}</td>
                        <td>{fmtTime(j.created_at)}</td>
                        <td className="cron-actions" onClick={(e) => e.stopPropagation()}>
                          <button className="btn" onClick={() => startEdit(j)}>{t('cron.edit')}</button>
                          <button className="btn" onClick={() => updateCron(j.id, { paused: !j.paused })}>{j.paused ? t('cron.resume') : t('cron.pause')}</button>
                          <button className="btn danger" onClick={() => setConfirm(j)}>{t('cron.delete')}</button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                {totalPages > 1 && (
                  <div className="cron-pager">
                    <button className="btn" disabled={cur <= 1} onClick={() => setPage(cur - 1)}>{t('cron.prev')}</button>
                    <span className="mono">{cur} / {totalPages}</span>
                    <button className="btn" disabled={cur >= totalPages} onClick={() => setPage(cur + 1)}>{t('cron.next')}</button>
                  </div>
                )}
              </>
            )}
          </div>
        </div>
      </div>

      {editing && (
        <div className="modal-hint open" role="dialog" aria-modal="true" aria-label={t('cron.title')}>
          <div className="card confirm-card" onClick={(e) => e.stopPropagation()}>
            <h3><span style={{ color: 'var(--accent)' }}>✎</span> {t('cron.editTitle').replace('{id}', editing.id)}</h3>
            <div className="cron-edit">
              <label>
                <span>Prompt</span>
                <textarea value={draft.prompt} rows={3} onChange={(e) => setDraft({ ...draft, prompt: e.target.value })} />
              </label>
              <label>
                <span>{t('cron.expr')}</span>
                <input value={draft.cron} onChange={(e) => setDraft({ ...draft, cron: e.target.value })} placeholder={t('cron.exprPh')} />
              </label>
              <label className="row">
                <input type="checkbox" checked={draft.recurring} onChange={(e) => setDraft({ ...draft, recurring: e.target.checked })} />
                {t('cron.recurringLabel')}
              </label>
              {/* IM 推送目标：先选渠道（已启用 Gateway），再选群（im_chat_list） */}
              <label>
                <span>{t('cron.imPush')}</span>
                <Sel
                  value={draft.imChat?.gateway ?? ''}
                  options={[
                    { value: '', label: t('cron.imPushNone') },
                    ...imGateways.map((g) => ({ value: g.type, label: `${g.type} (${g.id})` })),
                  ]}
                  onChange={(v) => {
                    const g = imGateways.find((x) => x.type === v)
                    if (g) fetchIMChats(g.id)
                    setDraft({ ...draft, imChat: g ? { gateway: g.type, gatewayId: g.id, chat_id: '' } : null })
                  }}
                />
              </label>
              {draft.imChat?.gateway && (
                <label>
                  <span>{t('cron.imPushGroup')}</span>
                  <Sel
                    value={draft.imChat.chat_id}
                    options={[
                      { value: '', label: t('cron.imPushSelectGroup') },
                      ...(draft.imChat.gatewayId ? (imChats[draft.imChat.gatewayId] ?? []) : []).map((c) => ({
                        value: c.chat_id, label: c.name || c.chat_id,
                      })),
                    ]}
                    onChange={(v) => setDraft({ ...draft, imChat: { ...draft.imChat!, chat_id: v } })}
                  />
                </label>
              )}
            </div>
            <div className="acts">
              <button className="btn" onClick={() => setEditing(null)}>{t('cron.cancel')}</button>
              <button className="btn primary" onClick={saveEdit}>{t('cron.save')}</button>
            </div>
          </div>
        </div>
      )}

      {confirm && (
        <ConfirmModal
          title={t('cron.deleteTitle').replace('{id}', confirm.id)}
          message={t('cron.deleteMsg').replace('{prompt}', confirm.prompt)}
          confirmLabel={t('cron.delete')}
          cancelLabel={t('cron.cancel')}
          danger
          onConfirm={() => { deleteCron(confirm.id); setConfirm(null) }}
          onCancel={() => setConfirm(null)}
        />
      )}
    </div>
  )
}
