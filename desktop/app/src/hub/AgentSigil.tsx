import { useEffect, useRef } from 'react'
import type { CSSProperties } from 'react'
import type { AvatarState } from './avatar'
import {
  lunaColor,
  lunaDamp,
  lunaExpression,
  lunaPhase,
  lunaSamples,
  releaseLunaScene,
  retainLunaScene,
} from './lunaAvatar'

// 头像 = demo 的 Luna 球面头像（mascot-bots.js + luna-face.js），不重画。
//
// 组件只声明“谁 / 什么职责 / 什么状态 / 多大 / 要不要读屏标签”，
// 几何、眼形、眨眼、视线、点头全部交给 vendor 里的同一套引擎。
// 外层类名同时保留 `avatar`（demo CSS 的动效/状态点）和 `luna-avatar`（侧栏等
// 在 .hub-demo 作用域外也能拿到基础尺寸）。

const SIZE_CLASS: Record<number, string> = { 28: 'xs', 52: 'lg' }

export default function AgentSigil({
  agentId,
  name,
  responsibility,
  state = 'idle',
  size = 44,
  status = true,
  label,
  className = '',
  style,
}: {
  agentId?: string | null
  /** 颜色散列的辅助输入；不传时只按 id。 */
  name?: string | null
  /** 职责短句（HubAgent.role）；决定固定身份色。 */
  responsibility?: string | null
  state?: AvatarState
  size?: number
  /** false：只画身份，不画状态点（头像叠放里的非 lead 成员）。 */
  status?: boolean
  /** 有值 → role="img" + aria-label；null/undefined → aria-hidden（名字已在同一行）。 */
  label?: string | null
  className?: string
  style?: CSSProperties
}) {
  const ref = useRef<HTMLSpanElement>(null)
  const id = agentId || name || 'agent'
  const px = size
  // 叠放里的 peer 也要保留工作/待审批表情；状态点由 .avatar-stack 的 CSS 隐藏。
  const st = state
  const expr = lunaExpression(st as 'idle' | 'working' | 'needs_attention' | 'offline')
  const fill = lunaColor(id, name ?? '', responsibility ?? '')

  useEffect(() => {
    const host = ref.current
    const sc = retainLunaScene()
    if (!host || !sc) return
    sc.sync(host.parentElement ?? document)
    const svg = host.querySelector('svg[data-luna-face]') as SVGElement | null
    if (!svg) return
    const want = lunaExpression(st as 'idle' | 'working' | 'needs_attention' | 'offline')
    if (host.dataset.baseExpr !== want) {
      host.dataset.baseExpr = want
      sc.setBase(svg, want)
      sc.setExpr(svg, want)
    }
    return () => releaseLunaScene()
  }, [st, id, fill])

  return (
    <span
      ref={ref}
      className={`avatar luna-avatar${SIZE_CLASS[px] ? ` ${SIZE_CLASS[px]}` : ''}${className ? ` ${className}` : ''}`}
      data-agent-id={agentId || undefined}
      data-slug={agentId || undefined}
      data-state={st}
      data-status={status ? 'on' : 'off'}
      data-size={px}
      role={label ? 'img' : undefined}
      aria-label={label || undefined}
      aria-hidden={label ? undefined : true}
      style={{ width: px, height: px, ...style }}
    >
      <svg
        data-luna-face
        data-expr={expr}
        data-fill={fill}
        data-eye="var(--eye,#0b0b0a)"
        data-phase={lunaPhase(id)}
        data-samples={lunaSamples(px)}
        data-damp={lunaDamp(px)}
        viewBox="0 0 240 240"
        width={px}
        height={px}
        aria-hidden="true"
        focusable="false"
      />
    </span>
  )
}
