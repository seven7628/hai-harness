// surfaces/PluginAssetIcon.tsx —— 「manifest 里的图标路径」→ 真图片（左栏插件行）。
//
// 为什么需要一个组件而不是一行 `<img src={p.icon}>`：`Project.icon` 是**两种东西**
// 共用的一个字段 —— 老数据/回执里可能是 emoji（`📕`），新 manifest 里是目录内相对路径
// （`icon.svg`）。前者当文本画、后者必须走 `hai-surface://<id>/icon.svg`。直接拼 URL 会把
// 「📕」拼成 `hai-surface://x/📕`（404 后空白 13px），直接当文本画则是用户报障的
// 「插件图标位置显示 icon.svg 这几个字」。
//
// 失败一律退回 `fallback`（调用方的 PluginIcon）：一个坏图标不该让整行消失，也不该刷红控制台。
import { useState } from 'react'
import type { ReactNode } from 'react'
import { pluginAssetUrl } from './plugin/loader'
import { canLoadPluginAsset, isPluginAssetPath } from './pluginAsset'

export default function PluginAssetIcon({
  pluginId,
  icon,
  size = 13,
  className = 'plugin-asset-ico',
  fallback,
}: {
  pluginId: string | undefined
  icon: string | undefined | null
  size?: number
  className?: string
  /** 没有图标 / 读不到时画什么（通常是 PluginIcon） */
  fallback: ReactNode
}) {
  // 每行/每张卡一份状态：一个坏图标只影响自己，不影响同组的其它插件。
  const [broken, setBroken] = useState(false)
  const src = pluginId && isPluginAssetPath(icon) ? pluginAssetUrl(pluginId, String(icon).trim()) : ''
  if (!canLoadPluginAsset(src) || broken) return <>{fallback}</>
  return (
    <img
      className={className}
      src={src}
      width={size}
      height={size}
      alt=""
      draggable={false}
      data-plugin-icon={pluginId}
      // 协议 404（插件被停用/目录没了）→ 退回 kind 图标，而不是留一个空白框。
      onError={() => setBroken(true)}
    />
  )
}
