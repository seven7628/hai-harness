import type { CSSProperties } from 'react'
import {
  SHAPES,
  SIGIL_VIEWBOX,
  TRAITS,
  deriveSigil,
  isAvatarState,
  type AvatarState,
  type SigilSize,
  type TraitMark,
} from './avatar'

// 智能体头像 sigil v2 的渲染层（方案 03 §3.3.3–3.3.7）。
//
// 只消费 avatar.ts 的 view model：形状/色相/内侧标记全部来自确定性推导，
// 状态层只画环/弧/点（stroke-dashoffset 与 opacity，不触发布局）。
// 主题靠 [data-theme='ink'|'paper'] 的 CSS 变量，不在 TS 里判断深浅色。
//
// 无障碍：与名字同一行的头像传 label=null → aria-hidden；独立出现时传 label
// （含名字/状态词）→ role="img" + aria-label。

function traitNode(mark: TraitMark, i: number) {
  if (mark.type === 'circle') {
    return <circle key={i} className="trait" cx={mark.cx} cy={mark.cy} r={mark.r} />
  }
  if (mark.type === 'rect') {
    return <rect key={i} className="trait" x={mark.x} y={mark.y} width={mark.width} height={mark.height} rx={mark.rx} />
  }
  return <path key={i} className="trait line" style={{ strokeWidth: mark.width }} d={mark.d} />
}

function statusLayer(state: AvatarState) {
  if (state === 'idle') {
    return <circle className="ring" cx="24" cy="24" r="22" opacity=".55" />
  }
  if (state === 'working') {
    // 270° 弧：dasharray/dashoffset 在 CSS 里（旋转只改 dashoffset，不依赖 transform-origin）。
    return <circle className="ring arc" cx="24" cy="24" r="22" strokeWidth="2" />
  }
  if (state === 'needs_attention') {
    // 加粗环 + 右上实心点：与 idle 的区别是「点 + 环更重」，与 working 的区别是没有缺口。
    return (
      <>
        <circle className="ring thick" cx="24" cy="24" r="22" />
        <circle className="dot" cx="38" cy="10" r="5" />
      </>
    )
  }
  // offline：只有 2.5 单位轮廓，无环无点。
  return null
}

export default function AgentSigil({
  agentId,
  responsibility,
  state = 'idle',
  size = 32,
  status = true,
  label,
  className = '',
  style,
}: {
  /** 身份键：同一 id + 职责在任何位置渲染同一图形。 */
  agentId?: string | null
  /** 职责短句（HubAgent.role）；只影响内侧标记。 */
  responsibility?: string | null
  state?: AvatarState
  size?: SigilSize
  /** false：只画身份，不画状态层（Group 叠放的其余成员）。 */
  status?: boolean
  /** 有值 → role="img" + aria-label；null/undefined → aria-hidden（名字已在同一行）。 */
  label?: string | null
  className?: string
  style?: CSSProperties
}) {
  const identity = deriveSigil(agentId, responsibility)
  const shown = isAvatarState(state) ? state : 'idle'
  const sigilState = status ? shown : 'none'
  return (
    <span
      className={`sigil s${size}${className ? ` ${className}` : ''}`}
      data-agent-id={agentId || undefined}
      data-shape={identity.shapeId}
      data-hue={identity.hueId}
      data-trait={identity.traitId}
      data-state={sigilState}
      data-size={size}
      role={label ? 'img' : undefined}
      aria-label={label || undefined}
      aria-hidden={label ? undefined : true}
      style={style}
    >
      <svg viewBox={`0 0 ${SIGIL_VIEWBOX} ${SIGIL_VIEWBOX}`} focusable="false" aria-hidden="true">
        <path className="shape" d={SHAPES[identity.shape].d} />
        <g className="traits">{TRAITS[identity.trait].marks.map(traitNode)}</g>
        {status ? <g className="status">{statusLayer(shown)}</g> : null}
      </svg>
    </span>
  )
}
