import { memo, useMemo } from 'react'
import { agentMarkHTML, type AgentMarkState } from './agent-marks'
import '../../runtime-tokens.css'
import './agent-mark.css'

/* Ten subagents, one family: one body, one pair of eyes, one tool each. The
   state is the subagent's own (running / stopped / finished) — a child's face
   is never borrowed from its parent's. */
export const AgentMark = memo(function AgentMark({
  kind,
  state = 'run',
  size,
  className,
}: {
  kind: string
  state?: AgentMarkState
  size?: number
  className?: string
}) {
  const html = useMemo(() => agentMarkHTML(kind, state, size ? { size } : undefined), [kind, state, size])
  return (
    <span
      className={'rt-agent-mark' + (className ? ' ' + className : '')}
      aria-hidden="true"
      dangerouslySetInnerHTML={{ __html: html }}
    />
  )
})
