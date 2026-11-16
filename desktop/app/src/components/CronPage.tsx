import { useEffect, useMemo, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import type { CronJob } from '../transport/types'
import { Clock } from './icons'
import ConfirmModal from './ConfirmModal'
import PageHead from './PageHead'
import PageModal from './PageModal'
import EmptyState from './EmptyState'
import Sel from './Sel'
import { fmtStamp } from './ui'

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

const PAGE_SIZE = 10

// 定时任务列表页。
// 页头/空态/编辑弹窗与执行记录、详情、技能页共用同一套组件（PageHead / EmptyState / PageModal），
// 避免「每页各写一套头」的时代再次出现。
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
  const loading = useAppStore((s) => s.cronLoading)
  const error = useAppStore((s) => s.cronError)

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

  // 进入定时任务页时拉 IM 配置（渠道下拉数据源；设置页可能未打开过）
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

  const empty = jobs.length === 0

  return (
    <div className="cron-page cron-list-page">
      <PageHead
        className="cron-head"
        backLabel={t('cron.back')}
        onBack={closeCron}
        icon={<Clock size={15} />}
        title={t('cron.title')}
        // i18n: 建议 key cron.subPlain —— 现有 cron.sub 把工具名（CronCreate 等）当副标题
        subtitle="按计划自动执行；每次执行的会话历史都能回看"
        right={
          <>
            <span className="page-head-count cron-head-count">{t('cron.count').replace('{n}', String(jobs.length))}</span>
            <button className="btn page-head-refresh cron-head-refresh" onClick={fetchCron} disabled={loading}>
              {t('cron.refresh')}
            </button>
          </>
        }
      />

      {!enabled && (
        <div className="cron-warn" role="status">
          <span className="cron-warn-dot" aria-hidden />
          <span>
            {t('cron.enabledOff')}{' '}
            <button className="cron-warn-link" onClick={openSettings}>{t('cron.settingsGeneral')}</button>{' '}
            {t('cron.enabledOff2')}
          </span>
        </div>
      )}

      <div className="sp-body">
        <div className="sp-content">
          <div className="cron-card">
            {loading && empty ? (
              <EmptyState compact icon={<Clock size={18} />} title="正在读取定时任务…" />
            ) : error && empty ? (
              <EmptyState
                icon={<Clock size={18} />}
                title="读取定时任务失败"
                hint={error}
                actions={<button className="btn" onClick={fetchCron}>重试</button>}
              />
            ) : empty ? (
              <EmptyState
                icon={<Clock size={18} />}
                title="暂无定时任务"
                hint={enabled ? '在会话里描述「什么时候做什么」，到点后这里会列出每次执行。' : '功能开启后，新建的任务会出现在这里。'}
                actions={enabled ? <button className="btn ghost" onClick={fetchCron}>刷新</button> : null}
              />
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
                        <td className="cell-title" data-label={t('cron.th.name')}>
                          <div className="cron-prompt">{j.prompt}</div>
                          <div className="mono" style={{ fontSize: 10.5, color: 'var(--text-3)' }}>{j.id}</div>
                        </td>
                        <td data-label={t('cron.th.schedule')} title={j.cron}>
                          {describeCron(j.cron, t)}
                          <div className="mono" style={{ fontSize: 10.5, color: 'var(--text-3)' }}>{j.recurring ? t('cron.recurring') : t('cron.once')}</div>
                        </td>
                        <td data-label={t('cron.th.status')}>
                          {j.paused ? (
                            <span className="cron-status paused">{t('cron.paused')}</span>
                          ) : (
                            <span className={`cron-status ${j.last_status ?? ''}`}>{statusText(t, j.last_status)}</span>
                          )}
                        </td>
                        <td className="cell-time" data-label={t('cron.th.lastRun')}>{fmtStamp(j.last_run)}</td>
                        <td className="cell-num" data-label={t('cron.th.runCount')}>{j.run_count}</td>
                        <td className="cell-time" data-label={t('cron.th.created')}>{fmtStamp(j.created_at)}</td>
                        <td className="cron-actions" data-label={t('cron.th.actions')} onClick={(e) => e.stopPropagation()}>
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
        <PageModal
          title="编辑任务"
          // i18n: 建议 key cron.editSub —— 内部 id 降级为弱色小字，不再进标题
          subtitle={<>任务 {editing.id} · {describeCron(draft.cron, t)}</>}
          icon={<span aria-hidden>✎</span>}
          onClose={() => setEditing(null)}
          width={560}
          cardClassName="cron-edit-modal"
          closeLabel={t('cron.cancel')}
          footer={
            <>
              <button className="btn" onClick={() => setEditing(null)}>{t('cron.cancel')}</button>
              <button className="btn primary" onClick={saveEdit}>{t('cron.save')}</button>
            </>
          }
        >
          <div className="cron-edit">
            <label className="page-modal-field">
              {/* i18n: 建议 key cron.promptLabel —— 原硬编码英文 Prompt */}
              <span className="page-modal-label">任务内容</span>
              <textarea rows={3} value={draft.prompt} onChange={(e) => setDraft({ ...draft, prompt: e.target.value })} />
            </label>
            <label className="page-modal-field">
              {/* i18n: 建议 key cron.planLabel */}
              <span className="page-modal-label">
                执行计划
                <span className="page-modal-hint">5 段表达式，如 */5 * * * *</span>
              </span>
              <input className="mono" value={draft.cron} onChange={(e) => setDraft({ ...draft, cron: e.target.value })} placeholder={t('cron.exprPh')} />
            </label>
            <label className="page-modal-check">
              <input type="checkbox" checked={draft.recurring} onChange={(e) => setDraft({ ...draft, recurring: e.target.checked })} />
              {t('cron.recurringLabel')}
            </label>
            {/* IM 推送目标：先选渠道（已启用 Gateway），再选群（im_chat_list） */}
            <div className="page-modal-field">
              <span className="page-modal-label">{t('cron.imPush')}</span>
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
            </div>
            {draft.imChat?.gateway && (
              <div className="page-modal-field">
                <span className="page-modal-label">{t('cron.imPushGroup')}</span>
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
              </div>
            )}
          </div>
        </PageModal>
      )}

      {confirm && (
        <ConfirmModal
          // i18n: 建议 key cron.deleteTitlePlain —— 原 cron.deleteTitle 把内部 id 放进了标题
          title="删除任务"
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
