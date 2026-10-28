import { Face } from '../Face'
import { FoldIcon } from '../FoldIcon'
import { durText, type TurnStats } from './selectTurns'
import './work-fold.css'

/* One dim line that answers "did it do anything, and how much" without opening
   the working phase. The face carries the state: running while the turn is
   live, engaged when the row is open, done when it is folded shut. */
export function WorkFold({
  stats,
  running,
  open,
  onToggle,
  label = 'Worked for',
}: {
  stats: TurnStats
  running: boolean
  open: boolean
  onToggle: () => void
  label?: string
}) {
  const dur = running ? '…' : durText(stats.workedMs)
  const parts: string[] = []
  if (stats.calls) parts.push(stats.calls + (stats.calls === 1 ? ' call' : ' calls'))
  if (stats.agents) parts.push(stats.agents + (stats.agents === 1 ? ' subagent' : ' subagents'))
  if (stats.compressions) parts.push(stats.compressions + (stats.compressions === 1 ? ' compress' : ' compresses'))
  if (stats.files) {
    parts.push(
      (stats.files === 1 ? 'Edited 1 file' : 'Edited ' + stats.files + ' files') +
        ' +' + stats.added + ' \u2212' + stats.removed,
    )
  }
  return (
    <button
      className="rt-act"
      type="button"
      aria-expanded={open}
      onClick={onToggle}
      data-running={running ? '1' : undefined}
    >
      <Face state={running ? 'run' : open ? 'engaged' : 'done'} />
      <span className="rt-act-t">
        {dur ? <><span className="rt-act-k">{label}</span> <b>{dur}</b></> : <span className="rt-act-k">{running ? 'Working' : 'Worked'}</span>}
        {parts.length ? <span className="rt-act-rest">{' \u00b7 '}{parts.join(' \u00b7 ')}</span> : null}
      </span>
      <FoldIcon open={open} />
    </button>
  )
}
