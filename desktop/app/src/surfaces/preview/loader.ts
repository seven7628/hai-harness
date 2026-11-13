// 渲染器组件的懒加载缓存（把 `load: () => Promise<ComponentType>` 变成可复用的一次加载）
//
// 为什么单独一个模块而不是塞进 PreviewHost：react-refresh 对「同文件既导出组件又导出函数」
// 会告警（基线 48 warnings 是钉住的，不给自己加噪音）；况且这张缓存与 React 无关。
//
// 三条纪律：
//   · 同一 id 只调一次 load（inflight 去重）；成功后进 componentCache，之后**同步**可命中 ——
//     文件面板的「定位到读过的行」依赖 .fp-code 与 html 同一次提交落地，多等一帧就会丢锚；
//   · 失败**不缓存**（与转换服务「失败不缓存」同一口径：一次失败不该永久失败）；
//   · load 解出来不是函数 = 模块没 default 导出 → 当成错误上报（否则 Host 会静默渲染空白）。
import type { ComponentType } from 'react'
import { getRenderer } from './registry'
import type { PreviewProps } from './types'

const componentCache = new Map<string, ComponentType<PreviewProps>>()
const inflight = new Map<string, Promise<ComponentType<PreviewProps>>>()

/** 已加载好的组件（同步路径；未加载 → undefined）。 */
export function cachedRenderer(id: string): ComponentType<PreviewProps> | undefined {
  return componentCache.get(id)
}

export function loadRenderer(id: string): Promise<ComponentType<PreviewProps>> {
  const hit = componentCache.get(id)
  if (hit) return Promise.resolve(hit)
  const running = inflight.get(id)
  if (running) return running
  const r = getRenderer(id)
  if (!r) return Promise.reject(new Error(`渲染器未注册：${id}`))
  const p = r.load().then(
    (C) => {
      inflight.delete(id)
      if (typeof C !== 'function') throw new Error(`渲染器模块没有 default 组件：${id}`)
      componentCache.set(id, C)
      return C
    },
    (e: unknown) => {
      inflight.delete(id)
      throw e
    },
  )
  inflight.set(id, p)
  return p
}

/** 预热（不等结果）。文件面板挂载时就把 text/csv 拉起来，见 FilePreview 的注释。 */
export function preloadRenderers(ids: readonly string[]): void {
  for (const id of ids) void loadRenderer(id).catch(() => { /* 失败留给真正渲染时报（unavailable 占位） */ })
}

/** 仅供测试/探针：清空组件缓存（注册表清空变异后，重新注册也能重新加载）。 */
export function resetRendererCacheForTest(): void {
  componentCache.clear()
  inflight.clear()
}
