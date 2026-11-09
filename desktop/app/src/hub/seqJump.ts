import type { HubMessage } from './types'

// 「跳到某条消息并高亮」的唯一判定 + DOM 落点（P4-02）。
//
// 为什么单独抽出来：Context 面板（Inspector）、群时间线（GroupView）、DM（HubSurface）
// 三处都要跳。滚动逻辑复制三份，迟早有一份忘记 stickBottom/不查目标是否存在。
// 这里只做两件纯事：
//   ① seqJumpPlan：这次请求该不该由**当前视图**执行（目标一致 + 消息已加载）；
//   ② highlightClass / seqDataAttr：高亮 class 与 data-seq 的同一个口径。
// 真正的 scrollIntoView 只有 scrollToSeq 一处（DOM，可在 Node 里用假 root 测）。

/** store 里的待执行跳转：nonce 用来让消费方只清掉自己接的那一次。 */
export interface HubJumpRequest {
  seq: number
  /** 群 id 或 agent id：只有当前视图匹配才执行（防止切走后跳到别人的时间线）。 */
  targetId: string
  nonce: number
}

/**
 * 请求是否落在当前视图、且目标消息已在内存里。
 * 返回 null = 本次不执行（视图不匹配 / 消息还没加载 / seq 非法）。
 * 条件里「消息必须已加载」很关键：历史没回来就 scrollIntoView 只会滚个空。
 */
export function seqJumpPlan(
  request: HubJumpRequest | null | undefined,
  current: {
    targetId: string | null | undefined
    messages?: Array<Pick<HubMessage, 'seq'>> | null
  },
): { seq: number; targetId: string; nonce: number } | null {
  if (!request) return null
  const seq = Math.trunc(Number(request.seq))
  if (!(seq > 0) || !Number.isSafeInteger(seq)) return null
  if (!request.targetId || request.targetId !== current.targetId) return null
  const loaded = (current.messages ?? []).some((m) => Number(m?.seq) === seq)
  if (!loaded) return null
  return { seq, targetId: request.targetId, nonce: request.nonce }
}

/** 消息是否处于跳转高亮态（气泡/系统行共用）。 */
export function highlightClass(seq: number | null | undefined, highlightSeq: number | null | undefined): string {
  const s = Math.trunc(Number(seq))
  const h = Math.trunc(Number(highlightSeq))
  return h > 0 && s > 0 && s === h ? ' hub-msg-highlight' : ''
}

/** data-seq 只给真实消息（乐观气泡 seq<0 不该被深链/跳转命中）。 */
export function seqDataAttr(seq: number | null | undefined): string | undefined {
  const s = Math.trunc(Number(seq))
  return s > 0 ? String(s) : undefined
}

/**
 * 滚到 data-seq 并居中。返回是否真的找到元素（调用方据此决定是否清 pending）。
 * reduced-motion 时用 auto：滚动本身是功能，不该只剩动画。
 */
export function scrollToSeq(root: ParentNode | null | undefined, seq: number): boolean {
  const s = Math.trunc(Number(seq))
  if (!root || !(s > 0)) return false
  const el = root.querySelector(`[data-seq="${s}"]`) as HTMLElement | null
  if (!el || typeof el.scrollIntoView !== 'function') return false
  const reduced =
    typeof window !== 'undefined' &&
    typeof window.matchMedia === 'function' &&
    window.matchMedia('(prefers-reduced-motion: reduce)').matches
  el.scrollIntoView({ block: 'center', behavior: reduced ? 'auto' : 'smooth' })
  return true
}
