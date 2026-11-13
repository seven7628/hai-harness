// mediaStage.ts — 媒体元素池：同一个 URL 的 <video>/<audio> **只创建一次**，切标签时
// 从 DOM 上摘下来但不销毁（R08c）。
//
// 为什么必须这么做（不是过度设计）：右栏是「一次只渲染激活标签」（RightPanel 的
// `activeTab` 只有一个，file 标签还带 key 强制重挂）。如果媒体元素跟着组件一起销毁，
// 切走再切回就等于**重新 new 一个 <video>**：浏览器要重新发 Range 请求、重新解容器、
// 重新缓冲 —— 1 小时视频上这是肉眼可见的停顿，也是本轮验收里「切走 5 秒再切回不重新
// 请求首字节」那条要挡的行为。元素活着，缓冲区就活着。
//
// 与 PDF 的 view pool 同一条思路（原生视图也是「切标签不销毁」），但媒体不需要
// WebContentsView：一个 DOM 元素就够了。
//
// 三条纪律：
//   · detach 时**暂停**：切走的标签不该在后台继续出声；
//   · 池子上限 4 个：一个右栏同时不会播更多，超出按最近使用淘汰（淘汰 = 真的销毁，
//     下次进来重新拉字节，这是可接受的代价）；
//   · 文件换过（mtime 变了）就重建：否则用户看到的是旧内容，而且 currentTime 落在
//     新文件的非法区间里会直接报错。
export type MediaElementKind = 'video' | 'audio'

export interface MediaStageOptions {
  /** 文件的 mtime（毫秒）。变了就重建元素（内容已经换了）。 */
  modifiedMs?: number
}

interface PoolEntry {
  el: HTMLMediaElement
  kind: MediaElementKind
  src: string
  modifiedMs: number
  /** 递增的使用序号：淘汰时挑最小的（最近没用过的）。 */
  lastUsed: number
  /** 当前是否挂在某个宿主里（挂着的永不淘汰）。 */
  attached: boolean
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
    .filter(([, e]) => !e.attached)
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
  /** 从池子里彻底移除（关闭标签且不再需要时用；淘汰也会走到这条路）。 */
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
    entry = { el: createElement(kind, src), kind, src, modifiedMs, lastUsed: 0, attached: false }
    entry.el.dataset.mediaKey = key
    pool.set(key, entry)
  }
  const e = entry
  e.lastUsed = ++useSeq
  evictIfNeeded()

  let attachedTo: HTMLElement | null = null
  return {
    el: e.el,
    attach(host: HTMLElement) {
      if (attachedTo === host) return
      if (attachedTo) attachedTo.removeChild(e.el)
      host.appendChild(e.el)
      attachedTo = host
      e.attached = true
      e.lastUsed = ++useSeq
    },
    detach() {
      // 暂停在前：从 DOM 摘下不会自动暂停（元素不在文档里也能放声）
      try {
        e.el.pause()
      } catch {
        /* ignore */
      }
      if (e.el.parentElement) e.el.parentElement.removeChild(e.el)
      attachedTo = null
      e.attached = false
    },
    dispose() {
      destroyEntry(e)
      if (pool.get(key) === e) pool.delete(key)
    },
  }
}

/** 仅供单测/探针：当前池子里的条目（键 = 媒体 URL）。 */
export function mediaStageSnapshot(): { key: string; kind: MediaElementKind; attached: boolean; currentTime: number }[] {
  return [...pool.entries()].map(([key, e]) => ({ key, kind: e.kind, attached: e.attached, currentTime: e.el.currentTime }))
}

export function resetMediaStage(): void {
  for (const e of pool.values()) destroyEntry(e)
  pool.clear()
  useSeq = 0
}
