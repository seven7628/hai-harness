import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import { Back, Clock } from './icons'
import Narrative from './Narrative'

function fmtTime(v?: string): string {
  if (!v) return '—'
  const d = new Date(v)
  return isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

function fmtDur(ms?: number): string {
  if (!ms) return '—'
  return `${(ms / 1000).toFixed(1)}s`
}

export default function CronRunDetailPage() {
  const t = useT()
  const activeCronRun = useAppStore((s) => s.activeCronRun)
  const cronDetailLoading = useAppStore((s) => s.cronDetailLoading)
  const clearCronRunDetail = useAppStore((s) => s.clearCronRunDetail)
  const setCronDetail = useAppStore((s) => s.setCronDetail)

  // 返回执行历史页（CronRunsPage）：清空详情状态，关闭详情子页
  const back = () => {
    clearCronRunDetail()
    setCronDetail(false)
  }

  const viewSid = activeCronRun?.viewSid
  const hasView = !!(viewSid && activeCronRun?.found)

  const statusText = (s?: string): string => {
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

  return (
    <div className="cron-page cron-detail-page">
      <header className="cron-head">
        <button className="cron-head-back" onClick={back}><Back /> {t('cron.backRuns')}</button>
        <div className="cron-head-title">
          <h1><Clock size={16} /> {t('cron.detailTitle')}</h1>
          <span className="cron-head-sub">{activeCronRun ? `${activeCronRun.run_id}${activeCronRun.session_id ? ' · ' + activeCronRun.session_id : ''}` : ''}</span>
        </div>
        <span className="cron-head-grow" />
        <div className="cron-head-right">
          {activeCronRun && (
            <span className={`cron-status ${activeCronRun.status ?? ''}`}>{statusText(activeCronRun.status)}</span>
          )}
        </div>
      </header>

      {cronDetailLoading && !activeCronRun ? (
        <div className="sp-body"><div className="sp-content"><div className="empty">{t('cron.detailLoading')}</div></div></div>
      ) : !activeCronRun ? (
        <div className="sp-body"><div className="sp-content"><div className="empty">{t('cron.detailNotFound')}</div></div></div>
      ) : (
        <>
          {/* 执行元信息条：轻量、不占叙事区空间 */}
          <div className="cron-detail-meta">
            <span className="cron-meta-item">
              <span className="cron-meta-label">{t('cron.meta.triggerAt')}</span>
              <b>{fmtTime(activeCronRun.fired_at)}</b>
            </span>
            <span className="cron-meta-item">
              <span className="cron-meta-label">{t('cron.meta.duration')}</span>
              <b>{fmtDur(activeCronRun.duration_ms)}</b>
            </span>
            {activeCronRun.session_id && (
              <span className="cron-meta-item">
                <span className="cron-meta-label">{t('cron.meta.session')}</span>
                <b className="mono">{activeCronRun.session_id}</b>
              </span>
            )}
            {activeCronRun.error && <span className="cron-meta-item cron-detail-err">{t('cron.meta.error')} {activeCronRun.error}</span>}
            {activeCronRun.final_result && !activeCronRun.error && (
              <span className="cron-meta-item cron-meta-result">
                <span className="cron-meta-label">{t('cron.meta.summary')}</span>
                <b className="cron-detail-summary">{activeCronRun.final_result}</b>
              </span>
            )}
          </div>

          {/* 叙事区：与主对话页同构（.narrative-wrap + .narrative，整页滚动/居中） */}
          {!hasView ? (
            <div className="sp-body"><div className="sp-content"><div className="empty">{t('cron.detailNotArchived')}</div></div></div>
          ) : (
            <div className="cron-detail-narrative">
              <Narrative viewSid={viewSid} />
            </div>
          )}
        </>
      )}
    </div>
  )
}
