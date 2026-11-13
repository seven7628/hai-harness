// hub:// 深链（P4-02）：唯一 canonical 形态
//   hub://group/<group_id>?seq=42
//   hub://dm/<agent_id>?seq=42
//
// 纯函数（不碰 store/DOM），Node 直测（scripts/test-hub-context.mjs）。
//
// 解析刻意严格：协议必须是 hub:，宿主只能是 group|dm，id 必须有，seq 可省略
//（0 = 只打开会话不定位），出现时必须是正整数。深链来自**外部进程**（macOS
// open-url / Win-Linux argv），猜一个「最接近的目标」会让窗口跳到错的会话，
// 所以任何不合格形态一律 null，由调用方报错。

export const HUB_DEEP_LINK_SCHEME = 'hub'

export type HubDeepLinkKind = 'group' | 'dm'

/** 深链目标：seq=0 表示只打开会话，不滚动定位。 */
export interface HubDeepLink {
  kind: HubDeepLinkKind
  id: string
  seq: number
}

export const HUB_DEEP_LINK_HOSTS: readonly HubDeepLinkKind[] = ['group', 'dm']

export const HUB_DEEP_LINK_INVALID_ERROR =
  '无效的 Hub 深链（应为 hub://group/<id> 或 hub://dm/<id>?seq=N）'

/** 规范化输出：id 按 URI 组件转义，seq<=0 不写。id 为空返回空串（调用方据此禁用按钮）。 */
export function formatHubDeepLink(
  kind: HubDeepLinkKind,
  id: string | null | undefined,
  seq?: number | null,
): string {
  const clean = String(id ?? '').trim()
  if (!clean) return ''
  if (!HUB_DEEP_LINK_HOSTS.includes(kind)) return ''
  const n = Math.trunc(Number(seq ?? 0))
  const query = Number.isSafeInteger(n) && n > 0 ? `?seq=${n}` : ''
  return `${HUB_DEEP_LINK_SCHEME}://${kind}/${encodeURIComponent(clean)}${query}`
}

/** 解析失败一律 null（不抛）；合法但没写 seq → seq=0。 */
export function parseHubDeepLink(raw: string | null | undefined): HubDeepLink | null {
  const text = String(raw ?? '').trim()
  if (!text) return null
  // 先卡 canonical 前缀：不给 new URL 机会把 `hub:group/x` 解释成 pathname。
  if (!/^hub:\/\//i.test(text)) return null
  let url: URL
  try {
    url = new URL(text)
  } catch {
    return null
  }
  if (url.protocol !== `${HUB_DEEP_LINK_SCHEME}:`) return null
  // 带账号信息的 URL（hub://user@group/x）不是我们的形态：一律拒绝。
  if (url.username || url.password) return null
  const host = url.hostname.toLowerCase()
  if (!HUB_DEEP_LINK_HOSTS.includes(host as HubDeepLinkKind)) return null
  const rawPath = url.pathname.replace(/^\/+/, '')
  let id: string
  try {
    id = decodeURIComponent(rawPath).trim()
  } catch {
    return null // 非法百分号转义
  }
  // id 是单段：多段路径（a/b）与解码出来的 `/` 都拒绝。
  if (!id || id.includes('/')) return null
  let seq = 0
  const seqRaw = url.searchParams.get('seq')
  if (seqRaw != null && seqRaw !== '') {
    if (!/^\d+$/.test(seqRaw)) return null // 负数/小数/非数字
    seq = Number(seqRaw)
    if (!Number.isSafeInteger(seq)) return null
  }
  return { kind: host as HubDeepLinkKind, id, seq }
}

/** 去重键：同一目标的同一位置重复触发只跑一次（openDeepLink 的在飞表用它）。 */
export function hubDeepLinkKey(link: HubDeepLink | null | undefined): string {
  if (!link) return ''
  return `${link.kind}:${link.id}:${link.seq}`
}
