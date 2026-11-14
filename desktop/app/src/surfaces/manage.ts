// manage.ts —— 面板插件**写路径**的两条渲染层小工具（PR-14b）。
//
// 为什么单独一个文件：这两件事有两个消费者 —— 设置页（SurfacesPage）与右栏的墓碑
// （SurfaceBoundary 的「重新启用」）。各写一份的第一天就会漂：一边把 unknown_id 说成
// 「参数不对」，另一边说成「网络问题」。
//
// 刻意**不做** IPC 包装层（没有 surfaceSetEnabled 的二次封装）：预加载已经把
// bridge.surfaceSetEnabled 暴露成同名 Promise 方法，再包一层只会让
// 「e2e 的 fake host api 要照哪一份实现」变成第二个问题。
import type { SurfaceActionResult } from '../transport/types'
import { bridge } from '../lib/bridge'

/**
 * surfaceReasonText 把 bridge 的封闭 reason 映射成一句人话。
 *
 * 集合是封闭的（unknown_id / bad_request / error，外加主进程自己的 denied）——
 * 兜底成「操作失败」而不是把 reason 原样贴给用户：英文小写下划线对用户没有信息量，
 * 而且那会让「多了一个 reason 分支但没人翻译」静默通过。
 */
export function surfaceReasonText(t: (key: string) => string, reason?: string): string {
  switch (reason) {
    case 'unknown_id':
      return t('surfaces.err.unknownId')
    case 'bad_request':
      return t('surfaces.err.badRequest')
    case 'denied':
      return t('surfaces.err.denied')
    default:
      return t('surfaces.err.error')
  }
}

/** pluginIDOf 从 `plugin:<id>` 里取 id（不是插件 kind = 空串）。 */
export function pluginIDOf(kind: string): string {
  const prefix = 'plugin:'
  return kind.startsWith(prefix) ? kind.slice(prefix.length) : ''
}

/**
 * setSurfaceEnabled 翻转一个面板插件的启用位（生产 = 宿主桥 → 主进程 → 桥）。
 *
 * 返回 `{ok, reason}`：没有宿主桥（浏览器 / mock 档）时不假装成功，而是
 * 回 `ok:false, reason:'error'` —— 「点了没反应」比「点了报错」更难查。
 */
export async function setSurfaceEnabled(id: string, enabled: boolean): Promise<SurfaceActionResult> {
  const d = bridge
  if (!d?.surfaceSetEnabled) return { ok: false, reason: 'error' }
  try {
    const r = await d.surfaceSetEnabled(id, enabled)
    return r && typeof r === 'object' ? r : { ok: false, reason: 'error' }
  } catch {
    return { ok: false, reason: 'error' }
  }
}
