import { memo } from 'react'
import { fmtDuration, fmtRun } from '../ui'
import { useClock } from '../../lib/useNow'
import { batchLineOf, type BatchProgress } from '../../lib/codemodeBatch'
import type { CodemodeTier } from '../../lib/codemodeNested'
import './tool-batch-progress.css'

/* 批量执行的进度条 + 一行统计（规格 §3）。
   两档，且**只有一档是百分比**：
     · 结束后（有 nested_calls）：真实比例 = 已完成/总数，红色段 = 失败份额；
     · 运行中（后端还没有任何子任务数据）：不确定态跑马灯 + 已用时长，不画百分比 —— 
       这是规格要求的诚实档，也是当前协议能支持的极限（见 lib/codemodeBatch.ts 顶部）。 */
export const ToolBatchProgress = memo(function ToolBatchProgress({
  tier,
  progress,
  running,
  startedAt,
  wall,
}: {
  tier: CodemodeTier
  progress?: BatchProgress
  running: boolean
  startedAt?: number
  wall?: string
}) {
  const now = useClock(startedAt, running)
  if (running) {
    const elapsed = startedAt != null && now != null ? Math.max(0, now - startedAt) : 0
    return (
      <div className="cm-bp" data-indeterminate="1" data-tier={tier}>
        <div className="cm-bp-bar" role="progressbar" aria-valuetext="running">
          <i className="cm-bp-fill" />
        </div>
        <div className="cm-bp-line mono">
          {tier === 'search' ? 'Search' : 'Execute'} · running · {fmtRun(elapsed)}
        </div>
      </div>
    )
  }
  if (!progress) return null
  const pct = Math.round((progress.pct ?? 0) * 100)
  const failPct = progress.total ? Math.round((progress.failed / progress.total) * 100) : 0
  return (
    <div className="cm-bp" data-tier={tier}>
      <div
        className="cm-bp-bar"
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={pct}
        data-failed={progress.failed > 0 ? '1' : undefined}
      >
        <i className="cm-bp-fill" style={{ width: `${pct}%` }} />
        {/* 红色失败段从绿段末尾开始，占据剩下的那段进度：pct 只算**成功**的份额
            （lib/codemodeBatch.ts 的 done/total），所以两段合起来正好是「跑完的部分」。
            特例：全部子调用都失败时 pct=0、失败段满格 —— 条上整条红，这是正确表达。 */}
        {failPct > 0 ? <i className="cm-bp-fail" style={{ width: `${failPct}%`, left: `${pct}%` }} /> : null}
      </div>
      <div className="cm-bp-line mono">
        {batchLineOf(progress, tier, wall ?? (progress.durMs ? fmtDuration(progress.durMs) : undefined))}
      </div>
    </div>
  )
})
