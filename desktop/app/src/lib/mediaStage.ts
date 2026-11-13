// mediaStage.ts — 媒体元素池：同一个 URL 的 <video>/<audio> **只创建一次**，切标签时
// 从 DOM 上摘下来但不销毁（R08c）。
//
// 为什么必须这么做（不是过度设计）：右栏是「一次只渲染激活标签」（RightPanel 的
// `activeTab` 只有一个，file 标签还带 key 强制重挂）。如果媒体元素跟着组件一起销毁，
// 切走再切回就等于**重新 new 一个 <video>**：浏览器要重新发 Range 请求、重新解容器、
// 重新缓冲 —— 1 小时视频上这是肉眼可见的停顿，也是本轮验收里「切走 5 秒再切回不重新
// 请求首字节」那条要挡的行为。元素活着，缓冲区就活着。
//
// 生命周期（复评裁决 2 之后的最终形态）：
//   · 挂载 → acquire(tabId)：把**标签 id** 记进 owners（Set，天然幂等：切走再切回
//     不会因为「又挂了一次」把计数加爆）；
//   · 切标签 → detach()：暂停 + 从 DOM 摘下，**不参与销毁决策**（元素与缓冲区留着，
//     这正是「不重下」的来源）；
//   · 关标签 → 由 store 调 disposeMediaElementsForPath(path, tabId)：从 owners 里删掉
//     这个标签；**owners 空了才销毁**；
//   · 池满 4 个 → 淘汰最久未用的、且没有任何 owner 的条目。
//
// 为什么销毁判据是「标签 owner」而不是「当前挂着几个 handle」（复评裁决 2）：
// 关 A 之后 B 还开着，但 B 可能正切到别的标签（detach）。按挂载数算，B 一切走就会把
// 元素销毁 → B 切回来发现要重新解容器/重下字节 —— 正撞本 PR 的验收点。owner 只在
// 「标签真的关了」时才减少，所以 A 关闭不影响 B，B 关闭才会回收。
import type { MediaKind } from './mediaFile'

/** 媒体种类与「这是不是媒体文件」同源（mediaFile.ts 的 MediaKind），不另立一份枚举。 */
export type MediaElementKind = MediaKind

export interface MediaStageOptions {
  /** 文件的 mtime（毫秒）。变了就重建元素（内容已经换了）。 */
  modifiedMs?: number
  /** 标签里的原始路径（相对/绝对都行）：关闭标签时按它回收元素，见 disposeMediaElementsForPath。 */
  srcPath?: string
  /** 标签 id：元素的 owner。关标签时按它把 owner 摘掉，owners 空了才销毁。 */
  tabId?: string
}


interface PoolEntry {
  el: HTMLMediaElement
  kind: MediaElementKind
  src: string
  srcPath: string
  modifiedMs: number
  /** 递增的使用序号：淘汰时挑最小的（最近没用过的）。 */
  lastUsed: number
  /** 当前挂在 DOM 上的 handle 数（只用于快照/诊断，**不参与销毁决策**）。 */
  attachedCount: number
  /** 还开着这个文件的标签 id（owner）。空 = 可以销毁。 */
  owners: Set<string>
}

const MAX_POOL = 4
const pool = new Map<string, PoolEntry>()
let useSeq = 0

function createElement(kind: MediaElementKind, src: string): HTMLMediaElement {
  const el = document.createElement(kind === 'audio' ? 'audio' : 'video') as HTMLMediaElement
  el.controls = true // 原生控件：不引任何播放器依赖（硬约束）
  el.preload = 'metadata'
  // 播放器自身不该被拖出面板；画中画/远程播放保持默认（不做额外限制）
  el.setAttribute('playsinline', '')
  el.className = 'pv-media-el'
  el.src = src
  return el
}

function destroyEntry(e: PoolEntry): void {
  try {
    e.el.pause()
    e.el.removeAttribute('src')
    e.el.load() // 释放解码器与缓冲（不调它，元素会一直占着 demuxer）
  } catch {
    /* 元素已被回收/环境不支持：忽略 */
  }
  e.el.remove()
}

function evictIfNeeded(): void {
  if (pool.size <= MAX_POOL) return
  const victims = [...pool.entries()]
    .filter(([, e]) => e.attachedCount === 0 && e.owners.size === 0)
    .sort((a, b) => a[1].lastUsed - b[1].lastUsed)
  for (const [key, e] of victims) {
    if (pool.size <= MAX_POOL) break
    destroyEntry(e)
    pool.delete(key)
  }
}

export interface MediaStageHandle {
  el: HTMLMediaElement
  /** 挂到宿主里（同一个元素可以被不同标签轮流挂）。 */
  attach(host: HTMLElement): void
  /** 从 DOM 摘下 + 暂停；元素与缓冲区留在池子里。 */
  detach(): void
  /** 本 handle 彻底结束（不再需要这个元素时）。还有别的 handle 挂着则只减计数。 */
  dispose(): void
}

/**
 * acquireMediaElement：取（或建）一个 URL 对应的媒体元素。
 * key 用绝对 URL：同一文件的两个标签共用同一个元素；不同文件互不相干。
 */
export function acquireMediaElement(
  key: string,
  kind: MediaElementKind,
  src: string,
  opts: MediaStageOptions = {},
): MediaStageHandle {
  const modifiedMs = opts.modifiedMs ?? 0
  let entry = pool.get(key)
  if (entry && (entry.kind !== kind || entry.src !== src || entry.modifiedMs !== modifiedMs)) {
    // 同一路径但内容变了 / 类型变了：旧的不能再用（浏览器会把旧缓冲当新文件）
    destroyEntry(entry)
    pool.delete(key)
    entry = undefined
  }
  if (!entry) {
    entry = {
      el: createElement(kind, src),
      kind,
      src,
      srcPath: opts.srcPath ?? '',
      modifiedMs,
      lastUsed: 0,
      attachedCount: 0,
      owners: new Set<string>(),
    }
    entry.el.dataset.mediaKey = key
    pool.set(key, entry)
  }
  const e = entry
  if (opts.srcPath) e.srcPath = opts.srcPath // 后挂的标签可能用另一种写法（相对/绝对）
  // owner 是 Set 的 add：同一个标签切走再切回**不会**把计数加爆（区别于「每次挂载 +1」）。
  // 没给 tabId（老调用方/单测）→ 没有 owner，元素按「挂载数」被淘汰逻辑兜底回收。
  if (opts.tabId) e.owners.add(opts.tabId)
  e.lastUsed = ++useSeq
  evictIfNeeded()

  let host: HTMLElement | null = null
  let counted = false
  const uncount = (): void => {
    if (!counted) return
    counted = false
    e.attachedCount = Math.max(0, e.attachedCount - 1)
    // **不在这里销毁**（复评裁决 2）：detach 只说明「这个标签暂时不在看」，不说明标签关了。
  }
  return {
    el: e.el,
    attach(next: HTMLElement) {
      if (host === next) return
      // 只有元素**还在本 handle 的宿主里**时才摘；被别的标签挪走了就只交接，不抢
      if (host && e.el.parentElement === host) host.removeChild(e.el)
      next.appendChild(e.el)
      host = next
      if (!counted) {
        counted = true
        e.attachedCount += 1
      }
      e.lastUsed = ++useSeq
    },
    detach() {
      // 暂停在前：从 DOM 摘下不会自动暂停（元素不在文档里也能放声）
      try {
        e.el.pause()
      } catch {
        /* ignore */
      }
      if (host && e.el.parentElement === host) host.removeChild(e.el)
      host = null
      uncount()
    },
    dispose() {
      if (host && e.el.parentElement === host) host.removeChild(e.el)
      host = null
      uncount()
      if (opts.tabId) e.owners.delete(opts.tabId)
      if (e.owners.size === 0) {
        destroyEntry(e)
        if (pool.get(key) === e) pool.delete(key)
      }
    },
  }
}

/**
 * disposeMediaElementsForPath 关标签时按**标签里的路径**回收元素（store 调）。
 * 之所以按 path 而不是 URL：store 不知道 URL（那是 media.tsx 从 IPC 拿的），
 * 而标签对象里有 path —— 元素池在 acquire 时记下同一个字符串。
 *
 * `tabId` 给了 = 只摘掉这个标签的 owner（别的标签还开着 → 元素留着）；
 * 不给 = 这个 path 上的标签全没了（工作区删除等）→ 清空 owners 并销毁。
 */
export function disposeMediaElementsForPath(path: string, tabId?: string): void {
  if (!path) return
  for (const [key, e] of [...pool.entries()]) {
    if (e.srcPath !== path) continue
    if (tabId) e.owners.delete(tabId)
    else e.owners.clear()
    if (e.owners.size > 0) continue // 还有标签开着这个文件 → 元素必须留着
    destroyEntry(e)
    pool.delete(key)
  }
}

/** 仅供单测/探针：当前池子里的条目（键 = 媒体 URL）。 */
export function mediaStageSnapshot(): {
  key: string
  kind: MediaElementKind
  srcPath: string
  attachedCount: number
  owners: string[]
  currentTime: number
  inDom: boolean
}[] {
  return [...pool.entries()].map(([key, e]) => ({
    key,
    kind: e.kind,
    srcPath: e.srcPath,
    attachedCount: e.attachedCount,
    owners: [...e.owners],
    currentTime: e.el.currentTime,
    inDom: !!e.el.parentElement,
  }))
}

export function resetMediaStage(): void {
  for (const e of pool.values()) destroyEntry(e)
  pool.clear()
  useSeq = 0
}
