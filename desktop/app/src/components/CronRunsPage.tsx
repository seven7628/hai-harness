import { useEffect, useMemo, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import type { CronRun } from '../transport/types'
import { Clock } from './icons'
import { fmtDuration } from '../lib/duration'
import { fmtStamp } from './ui'
import PageHead from './PageHead'
import EmptyState from './EmptyState'

const PAGE_SIZE = 10

// 执行历史（某个任务的全部运行记录）。
// 表格在 ≤900px 由 CSS 变成卡片（thead 隐藏、td 用 data-label 显示字段名），
// 这样 800px 宽时不会把「执行中」竖排、也不会被 .cron-card 的 overflow 裁掉。
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
      <PageHead
        className="cron-head"
        backLabel={t('cron.backList')}
        onBack={closeCronRuns}
        icon={<Clock size={15} />}
        title={t('cron.runsTitle')}
        // 任务内容放副标题（人话优先）；cron 表达式与内部 id 降级到下面的信息条
        subtitle={job.prompt}
        right={
          <>
            <span className="page-head-count cron-head-count">{t('cron.runsCount').replace('{n}', String(runs.length))}</span>
            <button className="btn page-head-refresh cron-head-refresh" onClick={() => fetchCronRuns(job.id)}>{t('cron.refresh')}</button>
          </>
        }
      />

      <div className="sp-body">
        <div className="sp-content">
          <div className="cron-card">
            <div className="cron-runs-jobinfo">
              <span className="mono" title={job.cron}>{job.cron}</span>
              <span>{job.recurring ? t('cron.recurring') : t('cron.once')}</span>
              {job.paused && <span className="cron-status paused">{t('cron.paused')}</span>}
              <span className="mono cron-runs-job-id" title={job.id}>{job.id}</span>
            </div>

            {runs.length === 0 ? (
              <EmptyState
                icon={<Clock size={18} />}
                title="暂无执行记录"
                hint="到点触发后，每次执行的会话历史都会出现在这里。"
              />
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
                        <td className="cell-time" data-label={t('cron.th.runTime')}>{fmtStamp(r.fired_at)}</td>
                        <td data-label={t('cron.th.status')}><span className={`cron-status ${r.status}`}>{statusText(r.status)}</span></td>
                        <td className="cell-time" data-label={t('cron.th.duration')}>{r.duration_ms ? fmtDuration(r.duration_ms) : '—'}</td>
                        <td className="mono" data-label={t('cron.th.session')}>{r.session_id || '—'}</td>
                        <td className="cron-prompt cell-summary" data-label={t('cron.th.summary')} title={r.error || r.final_result || ''}>{r.error || r.final_result || '—'}</td>
                        <td className="cron-actions" data-label={t('cron.th.actions')} onClick={(e) => e.stopPropagation()}>
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
