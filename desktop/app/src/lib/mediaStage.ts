// mediaStage.ts — 媒体元素池：同一个 URL 的 <video>/<audio> **只创建一次**，切标签时
// 从 DOM 上摘下来但不销毁（R08c）。
//
// 为什么必须这么做（不是过度设计）：右栏是「一次只渲染激活标签」（RightPanel 的
// `activeTab` 只有一个，file 标签还带 key 强制重挂）。如果媒体元素跟着组件一起销毁，
// 切走再切回就等于**重新 new 一个 <video>**：浏览器要重新发 Range 请求、重新解容器、
// 重新缓冲 —— 1 小时视频上这是肉眼可见的停顿，也是本轮验收里「切走 5 秒再切回不重新
// 请求首字节」那条要挡的行为。元素活着，缓冲区就活着。
//
// 生命周期（评审裁决 3 的第一条：原来 dispose() 是死代码，关标签不释放解码器）：
//   · 切标签 → detach()：暂停 + 摘下，池里留着（这正是「不重下」的来源）；
//   · 关标签 → 由 store 调 disposeMediaElementsForPath(path)：没人挂着就当场销毁，
//     还有别的标签共用一个 URL 时打 pendingDispose，等最后一个 detach 时再销毁；
//   · 池满 4 个 → 淘汰最久未用的**未挂载**条目。
//
// 为什么是引用计数而不是一个 boolean（评审裁决 3 的第二条）：同一个文件可以开两个
// 预览标签（file 标签 allowDuplicate）。后挂的那个会把元素挪到自己的宿主里，前一个
// unmount 时若按「共享标志」判断，会把这个**正在显示**的元素从活 DOM 里拔掉。
import type { MediaKind } from './mediaFile'

/** 媒体种类与「这是不是媒体文件」同源（mediaFile.ts 的 MediaKind），不另立一份枚举。 */
export type MediaElementKind = MediaKind

export interface MediaStageOptions {
  /** 文件的 mtime（毫秒）。变了就重建元素（内容已经换了）。 */
  modifiedMs?: number
  /** 标签里的原始路径（相对/绝对都行）：关闭标签时按它回收元素，见 disposeMediaElementsForPath。 */
  srcPath?: string
}

interface PoolEntry {
  el: HTMLMediaElement
  kind: MediaElementKind
  src: string
  srcPath: string
  modifiedMs: number
  /** 递增的使用序号：淘汰时挑最小的（最近没用过的）。 */
  lastUsed: number
  /** 有多少个 handle 正挂在这个元素上（引用计数，不是 boolean）。 */
  attachedCount: number
  /** 标签已经关了，但还有别的标签挂着 → 等最后一个 detach 再销毁。 */
  pendingDispose: boolean
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
    .filter(([, e]) => e.attachedCount === 0 && !e.pendingDispose)
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
      pendingDispose: false,
    }
    entry.el.dataset.mediaKey = key
    pool.set(key, entry)
  }
  const e = entry
  if (opts.srcPath) e.srcPath = opts.srcPath // 后挂的标签可能用另一种写法（相对/绝对）
  e.lastUsed = ++useSeq
  evictIfNeeded()

  let host: HTMLElement | null = null
  let counted = false
  const uncount = (): void => {
    if (!counted) return
    counted = false
    e.attachedCount = Math.max(0, e.attachedCount - 1)
    if (e.attachedCount === 0 && e.pendingDispose) {
      destroyEntry(e)
      if (pool.get(key) === e) pool.delete(key)
    }
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
      if (e.attachedCount === 0) {
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
 */
export function disposeMediaElementsForPath(path: string): void {
  if (!path) return
  for (const [key, e] of [...pool.entries()]) {
    if (e.srcPath !== path) continue
    if (e.attachedCount > 0) {
      // 还有别的标签显示着同一个文件：不能当场销毁（会把活 DOM 里的元素拔掉）
      e.pendingDispose = true
      continue
    }
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
  pendingDispose: boolean
  currentTime: number
  inDom: boolean
}[] {
  return [...pool.entries()].map(([key, e]) => ({
    key,
    kind: e.kind,
    srcPath: e.srcPath,
    attachedCount: e.attachedCount,
    pendingDispose: e.pendingDispose,
    currentTime: e.el.currentTime,
    inDom: !!e.el.parentElement,
  }))
}

export function resetMediaStage(): void {
  for (const e of pool.values()) destroyEntry(e)
  pool.clear()
  useSeq = 0
}
