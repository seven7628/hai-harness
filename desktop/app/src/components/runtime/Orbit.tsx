import { memo } from 'react'
import '../../runtime-tokens.css'
import './orbit.css'

/* Two rings, two dots going round in opposite directions. The same object at
   two sizes (`--s`): the start of a run and a running compression are the same
   sentence about time, so they are the same drawing. */
export const Orbit = memo(function Orbit({ size = 26, className }: { size?: number; className?: string }) {
  return (
    <span className={'rt-orbit' + (className ? ' ' + className : '')} style={{ ['--s' as string]: size + 'px' }} aria-hidden="true">
      <span className="ring" />
      <span className="ring" />
      <span className="spin" />
      <span className="spin" />
    </span>
  )
})
