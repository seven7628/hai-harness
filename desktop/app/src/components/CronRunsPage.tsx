import { useEffect, useMemo, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import type { CronJob, CronRun } from '../transport/types'
import { Back, Clock } from './icons'

function fmtTime(v?: string): string {
  if (!v) return '—'
  const d = new Date(v)
  return isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

const PAGE_SIZE = 10

export default function CronRunsPage() {
  const t = useT()
  const job = useAppStore((s) => s.cronRunsJob)
  const runs = useAppStore((s) => s.cronRuns)
  const closeCronRuns = useAppStore((s) => s.closeCronRuns)
  const fetchCronRuns = useAppStore((s) => s.fetchCronRuns)
  const fetchCronRunDetail = useAppStore((s) => s.fetchCronRunDetail)
  const setCronDetail = useAppStore((s) => s.setCronDetail)

  const [page, setPage] = useState(1)

  useEffect(() => {
    if (job) fetchCronRuns(job.id)
  }, [job, fetchCronRuns])

  // 数据刷新（触发新执行）时若当前页越界则回退
  const totalPages = Math.max(1, Math.ceil(runs.length / PAGE_SIZE))
  const cur = Math.min(page, totalPages)
  const pageRuns = useMemo(
    () => runs.slice((cur - 1) * PAGE_SIZE, cur * PAGE_SIZE),
    [runs, cur],
  )

  if (!job) return null

  // 点击某次执行 → 全新详情页（独立记录、本地化存储的会话历史）
  const openRun = (run: CronRun) => {
    setCronDetail(true)
    fetchCronRunDetail(job.id, run.run_id)
  }

  const statusText = (s?: string): string => {
    const map: Record<string, string> = {
      pending: t('cron.status.pending'),
      running: t('cron.status.running'),
      success: t('cron.status.success'),
      error: t('cron.status.error'),
      skipped: t('cron.status.skipped'),
      timeout: t('cron.status.timeout'),
    }
    return (s && map[s]) || s || '—'
  }

  return (
    <div className="cron-page cron-runs-page">
      <header className="cron-head">
        <button className="cron-head-back" onClick={closeCronRuns}><Back /> {t('cron.backList')}</button>
        <div className="cron-head-title">
          <h1><Clock size={16} /> {t('cron.runsTitle')}</h1>
          <span className="cron-head-sub">{job.id} · {job.cron} · {job.recurring ? t('cron.recurring') : t('cron.once')}{job.paused ? ` · ${t('cron.paused')}` : ''}</span>
        </div>
        <span className="cron-head-grow" />
        <div className="cron-head-right">
          <span className="cron-head-count">{t('cron.runsCount').replace('{n}', String(runs.length))}</span>
          <button className="cron-head-refresh" onClick={() => fetchCronRuns(job.id)}>{t('cron.refresh')}</button>
        </div>
      </header>

      <div className="sp-body">
        <div className="sp-content">
          <div className="cron-card">
            <div className="cron-runs-jobinfo">
              <span className="cron-runs-job-prompt">{job.prompt}</span>
              <span className="mono">{job.cron}</span>
              <span>{job.recurring ? t('cron.recurring') : t('cron.once')}</span>
              {job.paused && <span className="cron-status paused">{t('cron.paused')}</span>}
            </div>

            {runs.length === 0 ? (
              <div className="empty">{t('cron.emptyRuns')}</div>
            ) : (
              <>
                <table className="cron-table">
                  <thead>
                    <tr>
                      <th>{t('cron.th.runTime')}</th><th>{t('cron.th.status')}</th><th>{t('cron.th.duration')}</th><th>{t('cron.th.session')}</th><th>{t('cron.th.summary')}</th><th>{t('cron.th.actions')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {pageRuns.map((r) => (
                      <tr key={r.run_id} className="cron-run-row" onClick={() => openRun(r)}>
                        <td>{fmtTime(r.fired_at)}</td>
                        <td><span className={`cron-status ${r.status}`}>{statusText(r.status)}</span></td>
                        <td>{r.duration_ms ? `${(r.duration_ms / 1000).toFixed(1)}s` : '—'}</td>
                        <td className="mono">{r.session_id || '—'}</td>
                        <td className="cron-prompt">{r.error || r.final_result || '—'}</td>
                        <td className="cron-actions">
                          <button className="btn" onClick={(e) => { e.stopPropagation(); openRun(r) }}>{t('cron.viewSession')}</button>
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
    </div>
  )
}
