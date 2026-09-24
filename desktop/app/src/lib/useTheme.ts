import { useEffect, useState } from 'react'

// 解析后的主题（ink=深 / paper=亮），随 <html data-theme> 属性变化实时更新。
// applyTheme()（lib/theme.ts）把主题偏好写进 data-theme；这里用 MutationObserver 订阅，
// 任何来源（设置/系统跟随）改动都跟随。shiki 高亮等需要随主题重渲的场景用它驱动。
export type ResolvedTheme = 'ink' | 'paper'

export function useResolvedTheme(): ResolvedTheme {
  const [t, setT] = useState<ResolvedTheme>(() =>
    (document.documentElement.getAttribute('data-theme') as ResolvedTheme) || 'ink',
  )
  useEffect(() => {
    const el = document.documentElement
    const update = () => setT((el.getAttribute('data-theme') as ResolvedTheme) || 'ink')
    const mo = new MutationObserver(update)
    mo.observe(el, { attributes: true, attributeFilter: ['data-theme'] })
    update()
    return () => mo.disconnect()
  }, [])
  return t
}
