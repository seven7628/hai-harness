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

// 引擎把每颗头的运行态挂在 svg 上（vendor：svg.__lunaFace = face）。
// 只用到 visible：false 时 step()/paintFace() 会整颗跳过。
type LunaFaceRef = { visible: boolean }

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
  live = true,
}: {
  agentId?: string | null
  /** 参与职责规则匹配（demo 的 avatarColor 同样吃 id+name+role）。
   *  所有调用点都必须传真实名字，否则同一智能体可能在不同入口落到两种颜色。 */
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
  /** false：会话行头像。引擎不再随机换风格（data-live="off"），CSS 也不跑工作呼吸动画。
   *  静态 prop：调用点传常量，不进 effect 依赖。 */
  live?: boolean
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

  // 离屏停画：长会话里几十上百颗 52px 头像，引擎 30fps 无条件遍历 scene.faces 并写
  // transform + d。引擎已尊重 face.visible（step / paintFace 都直接跳过），所以这里
  // 只按可见性翻这一个开关，vendor 一行不改。
  //
  // 依赖 []：observer 只建一次。`st` 变化（工作/待审批）不影响可见性，重建反而会在
  // 每次状态翻转时丢掉当前可见性、让离屏头像白画一帧。
  useEffect(() => {
    if (typeof IntersectionObserver === 'undefined') return
    const host = ref.current
    if (!host) return
    const io = new IntersectionObserver((entries) => {
      const entry = entries[entries.length - 1]
      if (!entry) return
      const svg = host.querySelector('svg[data-luna-face]') as (SVGElement & { __lunaFace?: LunaFaceRef }) | null
      const face = svg?.__lunaFace
      if (face) face.visible = entry.isIntersecting
    }, {
      // 滚动容器就是会话列表；头像在别处（header/侧栏/叠放）时退回视口。
      root: host.closest('.messages'),
      // 提前一屏量开始画，滚到时不会看到空白脸。
      rootMargin: '200px',
    })
    io.observe(host)
    return () => io.disconnect()
  }, [])

  return (
    <span
      ref={ref}
      className={`avatar luna-avatar${SIZE_CLASS[px] ? ` ${SIZE_CLASS[px]}` : ''}${className ? ` ${className}` : ''}`}
      data-agent-id={agentId || undefined}
      data-slug={agentId || undefined}
      data-state={st}
      data-status={status ? 'on' : 'off'}
      data-size={px}
      data-live={live ? undefined : 'off'}
      role={label ? 'img' : undefined}
      aria-label={label || undefined}
      aria-hidden={label ? undefined : true}
      style={{ width: px, height: px, ...style }}
    >
      <svg
        data-luna-face
        data-live={live ? undefined : 'off'}
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
