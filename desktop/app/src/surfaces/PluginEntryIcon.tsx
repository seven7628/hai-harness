// surfaces/PluginEntryIcon.tsx —— 插件入口的图标（R16）。
//
// 三条：入口声明了 `icon` 就用它（`<id>` 目录内的相对路径，走 pluginAssetUrl 与入口模块
// 同一条 base）；读不到（404 / 坏图）**退回 kind 图标**，不阻塞、不报错 —— 一个坏图标
// 不该让整行菜单消失，也不该在控制台刷红（D-3：单个 entry 的 icon 缺失不阻塞安装，
// 运行时同理）。没声明图标同样是 kind 图标。
import { useState } from 'react'
import type { ReactNode } from 'react'
import { pluginAssetUrl } from './plugin/loader'
import { canLoadPluginAsset } from './pluginAsset'
import type { PluginEntryItem } from './plugin/entries'

export default function PluginEntryIcon({ entry, fallback, size = 12 }: {
  entry: PluginEntryItem
  /** kind 图标（由调用方从 registry 取，见 RightPanel 的 kindIcon）：读不到图时用它 */
  fallback: ReactNode
  /** 像素。菜单/标签 12（历史值），右侧 rail 的 34px 按钮用 16 */
  size?: number
}) {
  // 每项一份状态：一个坏图标只影响自己那一行（放在父级会让整组退回同一个图标）。
  const [broken, setBroken] = useState(false)
  const src = entry.icon ? pluginAssetUrl(entry.pluginId, entry.icon) : ''
  if (!canLoadPluginAsset(src) || broken) return <>{fallback}</>
  return (
    <img
      className="rtabs-entry-ico"
      src={src}
      width={size}
      height={size}
      style={{ width: size, height: size }}
      alt=""
      draggable={false}
      data-plugin-icon={entry.pluginId}
      onError={() => setBroken(true)}
    />
  )
}
