import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import { Clock } from './icons'
import Narrative from './Narrative'
import { fmtDuration } from '../lib/duration'
import { fmtStamp } from './ui'
import PageHead from './PageHead'
import EmptyState from './EmptyState'

function fmtDur(ms?: number): string {
  if (!ms) return '—'
  return fmtDuration(ms)
}

// 某次执行的详情页：元信息条（人话字段）+ 叙事区（与主对话页同构）。
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
      <PageHead
        className="cron-head"
        // i18n: 建议 key cron.backRun —— 文案基线：返回记录
        backLabel="返回记录"
        onBack={back}
        icon={<Clock size={15} />}
        title={t('cron.detailTitle')}
        subtitle={activeCronRun ? fmtStamp(activeCronRun.fired_at) : ''}
        right={
          activeCronRun ? (
            <span className={`cron-status ${activeCronRun.status ?? ''}`}>{statusText(activeCronRun.status)}</span>
          ) : null
        }
      />

      {cronDetailLoading && !activeCronRun ? (
        <div className="sp-body"><div className="sp-content"><div className="cron-card"><EmptyState compact icon={<Clock size={18} />} title={t('cron.detailLoading')} /></div></div></div>
      ) : !activeCronRun ? (
        <div className="sp-body">
          <div className="sp-content">
            <div className="cron-card">
              <EmptyState
                icon={<Clock size={18} />}
                title="未找到该次执行记录"
                hint={t('cron.detailNotFound')}
                actions={<button className="btn" onClick={back}>返回执行记录</button>}
              />
            </div>
          </div>
        </div>
      ) : (
        <>
          {/* 执行元信息条：轻量、不占叙事区空间 */}
          <div className="cron-detail-meta">
            <span className="cron-meta-item">
              <span className="cron-meta-label">{t('cron.meta.triggerAt')}</span>
              <b className="mono">{fmtStamp(activeCronRun.fired_at)}</b>
            </span>
            <span className="cron-meta-item">
              <span className="cron-meta-label">{t('cron.meta.duration')}</span>
              <b className="mono">{fmtDur(activeCronRun.duration_ms)}</b>
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
            <div className="sp-body">
              <div className="sp-content">
                <div className="cron-card">
                  <EmptyState icon={<Clock size={18} />} title="该次执行的历史尚未归档" hint="执行仍在进行中，或数据已缺失。" />
                </div>
              </div>
            </div>
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
