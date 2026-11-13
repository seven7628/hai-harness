// 渲染器宿主：选渲染器 → 懒加载 → 包一个可观测的容器（data-renderer）
//
// 三个落点（探针与看板都按 data-renderer 断言）：
//   · 命中且依赖可用 → `data-renderer="<id>"` + data-state=loading|ready
//   · 命中但依赖缺失 / 模块加载失败 → `data-renderer="unavailable"` + data-reason
//   · 无人命中（含二进制无渲染器）→ `data-renderer="unavailable"` + data-reason
// 「不许白屏」是硬要求：每一条失败路径都必须有**写明原因**的占位（00 §4.3 空态有引导）。
import { useCallback, useEffect, useState } from 'react'
import type { ComponentType } from 'react'
import { useT } from '../../i18n'
import { cachedRenderer, loadRenderer } from './loader'
import { missingDependencyReason, pickRenderer } from './registry'
import type { FileRef, PreviewProps } from './types'
// 副作用注册：凡用到 Host 的地方都保证三个渲染器已在册（见 renderers/index.ts 顶部注释）
import './renderers'

export interface PreviewHostProps {
  file: FileRef
  content?: string
  /** 现成的高亮 HTML（文件面板为「定位到读过的行」本来就算了一份，见 types.PreviewProps）。 */
  html?: string
  /** 收窄候选渲染器（文件面板的「表格 / 纯文本」开关用）。 */
  only?: readonly string[]
}

type Loaded = { id: string; C?: ComponentType<PreviewProps>; error?: string }
/** 渲染器自己上报的「接不了」（R08b，见 types.PreviewProps.onUnavailable）。 */
type Reported = { id: string; reason: string }

export function PreviewHost({ file, content = '', html, only }: PreviewHostProps) {
  const t = useT()
  // 每次渲染现算：候选表只有几条，且 pickRenderer 返回的是**注册表里的那个对象**
  // （身份稳定），所以下面 effect 的依赖用 id 就够，不必 memo 出什么。
  const picked = pickRenderer(file, only ? { only } : undefined)
  const id = picked?.id
  // 初始 state 直接吃 loader 的同步缓存（预热过的渲染器第一帧就能挂载）。
  // 这一帧很要紧：文件面板靠 .fp-code 是否存在来恢复滚动位置/定位行，
  // 白白晚一帧就可能落在「元素还没挂载」的窗口里（见 FilePreview 的滚动恢复 effect）。
  const [loaded, setLoaded] = useState<Loaded | undefined>(() => {
    const C = id ? cachedRenderer(id) : undefined
    return id && C ? { id, C } : undefined
  })
  // 渲染器上报的失败（转换失败/内嵌视图建不起来）。key 带 id：换渲染器时旧上报自动失效，
  // 不必在 effect 里清（清了反而会多一次渲染）。
  const [reported, setReported] = useState<Reported | undefined>(undefined)
  const onUnavailable = useCallback((reason: string) => {
    setReported((prev) => (id && prev?.id === id && prev.reason === reason ? prev : { id: id ?? '', reason }))
  }, [id])

  useEffect(() => {
    if (!id) return
    const cached = cachedRenderer(id)
    if (cached) { setLoaded({ id, C: cached }); return }
    // 换 id 时先清掉上一个的组件（否则会拿 A 的组件渲染 B 的容器一帧）
    setLoaded((prev) => (prev?.id === id ? prev : undefined))
    let alive = true
    loadRenderer(id).then(
      (C) => { if (alive) setLoaded({ id, C }) },
      (e: unknown) => { if (alive) setLoaded({ id, error: (e as Error)?.message ?? String(e) }) },
    )
    return () => { alive = false }
  }, [id])

  if (!picked) {
    return (
      <Unavailable reason={t('rp.preview.noRenderer').replace('{ext}', file.ext ? `.${file.ext}` : file.path)} />
    )
  }
  const depReason = missingDependencyReason(picked)
  if (depReason) return <Unavailable id={picked.id} reason={depReason} />
  if (loaded?.id === picked.id && loaded.error) return <Unavailable id={picked.id} reason={loaded.error} />
  // 渲染器上报的失败：整块换成 unavailable（不是把占位塞进渲染器自己的容器里）——
  // data-renderer 只有宿主这一个写者，同屏两个标识会让探针与看板都得猜哪个算数。
  if (reported?.id === picked.id) return <Unavailable id={picked.id} reason={reported.reason} />

  const C = loaded?.id === picked.id ? loaded.C : undefined
  return (
    <div className="preview-host" data-renderer={picked.id} data-state={C ? 'ready' : 'loading'}>
      {C ? <C file={file} content={content} html={html} onUnavailable={onUnavailable} /> : null}
    </div>
  )
}

function Unavailable({ id, reason }: { id?: string; reason: string }) {
  const t = useT()
  return (
    <div
      className="preview-host preview-unavailable"
      data-renderer="unavailable"
      data-renderer-id={id}
      data-reason={reason}
      role="status"
    >
      <div className="preview-unavailable-title">⚠ {t('rp.preview.unavailable')}</div>
      <div className="preview-unavailable-reason">{reason}</div>
    </div>
  )
}
