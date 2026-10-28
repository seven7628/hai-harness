import { memo, useMemo } from 'react'
import { faceHTML, type MarkSystem, type MarkState } from './faces'
import '../../runtime-tokens.css'
import './faces.css'

/* A face, not a spinner. The drawing is a body painted through a mask with the
   eyes cut out of it, so at 14px you read a solid little head with the page
   showing through its eyes. Four states, one system per page (data-mark on
   <html>) — a thought and a running phase can never be drawn by two hands. */
export const Face = memo(function Face({
  system = 'pip',
  state = 'done',
  size,
  className,
  title,
}: {
  system?: MarkSystem
  state?: MarkState
  size?: number
  className?: string
  title?: string
}) {
  const html = useMemo(() => faceHTML(system, state), [system, state])
  return (
    <span
      className={'rt-face' + (className ? ' ' + className : '')}
      style={size ? { ['--mk-size' as string]: size + 'px' } : undefined}
      title={title}
      aria-hidden={title ? undefined : true}
      dangerouslySetInnerHTML={{ __html: html }}
    />
  )
})
