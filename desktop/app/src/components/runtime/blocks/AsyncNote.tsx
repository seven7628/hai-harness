import { memo, useState } from 'react'
import type { TerminalTaskStatus } from '../../../store/useAppStore'
import { Markdown } from '../../markdown'
import { useT } from '../../../i18n'
import './async.css'

/* A background task landing while the run is still going.

   It is a record, not a toast: it is inserted into the stream and stays there —
   a toast steals the pointer and leaves with the answer, and this answer is
   something you may want to open a minute later. So it is a two-line note in
   flow with exactly one hairline frame more than the prose around it, because
   it arrived at a moment when the eye was somewhere else.

   No progress bar: the parent does not know how much of a background task is
   left. When it is still running the only certainly-true number is how long it
   has been running, so that is the number it shows.

   No dismiss: our blocks are the transcript, not a notification tray. Closing
   the note would delete history, and the app already has no way to put it back. */

const ICON = {
  ok: 'M2.5 6.3l2.3 2.3L9.5 3.7',
  fail: 'M6 2.6v5.1M6 9.6v.9',
  run: 'M6 2.7a3.3 3.3 0 1 1-3.3 3.3',
}

export const AsyncNote = memo(function AsyncNote({
  taskId,
  status,
  result,
  error,
  liveLabel,
  rich = false,
}: {
  taskId: string
  status: TerminalTaskStatus | 'running'
  result?: string
  error?: string
  /** what the row says while the task has not landed yet */
  liveLabel?: string
  /** the result is Markdown (a task_result block) rather than plain output */
  rich?: boolean
}) {
  const t = useT()
  const [open, setOpen] = useState(false)
  const failed = status === 'failed' || Boolean(error)
  const running = status === 'running'
  const kind = failed ? 'fail' : running ? 'run' : 'ok'
  const hasBody = Boolean(result || error)

  const verb = failed
    ? t('async.failed')
    : running
      ? t('async.running')
      : status === 'interrupted'
        ? t('async.killed')
        : t('async.finished')
  // while it runs there is no outcome, so the row shows the child's own last
  // line instead of inventing one
  const detail = running ? liveLabel || '' : (error || result || '').split('\n')[0] || ''

  return (
    <div className="rt-as" data-r={kind}>
      <span className="rt-as-ico" aria-hidden="true">
        <svg viewBox="0 0 12 12">
          <path
            d={ICON[kind]}
            fill="none"
            stroke="currentColor"
            strokeWidth="1.7"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      </span>
      <div className="rt-as-l1">
        {t('async.label')} <span className="rt-as-verb">{verb}</span>
        <span className="sep">{'\u00b7'}</span>
        <b>{taskId}</b>
      </div>
      {detail ? (
        <div className="rt-as-l2" title={detail}>
          <span className="rt-as-detail">{detail}</span>
        </div>
      ) : null}
      {hasBody ? (
        <button className="rt-as-act" type="button" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
          {open ? t('task.result.collapse') : t('task.result.expand')}
        </button>
      ) : null}
      {open && hasBody ? (
        <div className="rt-as-body">
          {error ? (
            <div className="rt-as-err">{error}</div>
          ) : rich ? (
            <Markdown text={result || ''} />
          ) : (
            <pre className="rt-as-pre">{result}</pre>
          )}
        </div>
      ) : null}
    </div>
  )
})
