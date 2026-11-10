import type { CSSProperties } from 'react'
import type { HubAgent, HubAvatar as HubAvatarData } from './types'
import { accentOfAgent } from './agentAccent'

/** 智能体头像：emoji 优先，否则名字首字；身份色决定描边/底色/运行环。 */
export default function HubAvatar({
  agent,
  size,
  ring,
  accent: accentOverride,
}: {
  agent: { id?: string; name: string; accent?: string; avatar?: HubAvatarData }
  size: number
  ring?: boolean
  /** 群组等没有 profile 的场景显式传入身份色。 */
  accent?: string
}) {
  const accent = accentOverride || accentOfAgent(agent)
  const style = {
    width: size,
    height: size,
    background: `color-mix(in srgb, ${accent} 14%, transparent)`,
    borderColor: accent,
    color: accent,
    fontSize: Math.round(size * 0.5),
    // CSS 里 .hub-avatar.ring 用这支色做 halo：所有头像保持各自颜色。
    ['--agent-accent']: accent,
  } as CSSProperties
  const emoji = agent.avatar?.kind === 'emoji' ? agent.avatar.value : ''
  return (
    <span className={`hub-avatar ${ring ? 'ring' : ''}`} style={style}>
      {emoji || (agent.name || '?').slice(0, 1)}
    </span>
  )
}
