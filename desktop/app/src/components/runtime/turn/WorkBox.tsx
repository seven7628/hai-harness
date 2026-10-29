import type { ReactNode } from 'react'
import { WorkFold } from './WorkFold'
import type { TurnStats } from './selectTurns'

/* The working phase after it stopped, in the same shape it had while running.

   Concise density has exactly two states, and both are boxes:

     running  → LivePanel (head: Working · clock · calls, body: the whole run)
     ended    → this: the fold row as the head, the same content as the body,
                capped, or the head alone when it is shut

   The alternative — spilling the rows into the transcript when the user expands
   — is what made a finished turn read as if the run were still going: the
   answer ended up *below* a wall of tool rows, and the box that was supposed to
   hold them had no bottom. Outside this box sits only the last round's reply. */
export function WorkBox({
  open,
  onToggle,
  stats,
  running,
  children,
}: {
  open: boolean
  onToggle: () => void
  stats: TurnStats
  running: boolean
  children?: ReactNode
}) {
  const head = <WorkFold stats={stats} running={running} open={open} onToggle={onToggle} />
  // Shut: the fold row *is* the block, with nothing around it (a border around a
  // single dim line would read as one more card in a one-line transcript).
  if (!open) return head
  return (
    <div className="rt-workbox">
      {head}
      {children ? <div className="rt-work-body rt-turn-in">{children}</div> : null}
    </div>
  )
}
