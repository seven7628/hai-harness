// mediaStage-test.ts — 媒体元素池的**生命周期**单测（R08c 评审裁决 3）。
//
// 为什么值得单测：这个池子决定「切标签要不要重下字节」与「关标签有没有释放解码器」，
// 而这两件事在真实 Electron 里各只有一条断言（e2e 的 [4]），中间的分支（两个标签共用
// 一个 URL、关一个还剩另一个、池满淘汰）在 UI 里几乎撞不出来。评审抓到的两个真缺陷
// 正好都在这里：
//   · dispose() 是死代码 → 关标签不释放解码器；
//   · 用一个共享 boolean 表示「挂着」→ 同一 URL 开两个标签时，A detach 会把 B 正在
//     显示的元素从活 DOM 里拔掉。
// 所以这里用最小假 DOM 把引用计数与回收时机钉死（不是 DOM 解析器，只是 addChild/remove）。
//
// 运行：esbuild bundle 后 node --test（见 package.json test:media-stage）。
import { test, beforeEach } from 'node:test'
import assert from 'node:assert/strict'
import {
  acquireMediaElement,
  disposeMediaElementsForPath,
  mediaStageSnapshot,
  resetMediaStage,
} from './mediaStage.ts'

// —— 最小假 DOM ——
class FakeNode {
  parentElement: FakeNode | null = null
  children: FakeNode[] = []
  dataset: Record<string, string> = {}
  attrs: Record<string, string> = {}
  style: Record<string, string> = {}
  className = ''
  src = ''
  controls = false
  preload = ''
  paused = true
  currentTime = 0
  readyState = 0
  duration = 0
  constructor(public tag: string) {}
  appendChild(c: FakeNode): FakeNode {
    if (c.parentElement) c.parentElement.removeChild(c)
    c.parentElement = this
    this.children.push(c)
    return c
  }
  removeChild(c: FakeNode): FakeNode {
    const i = this.children.indexOf(c)
    if (i >= 0) this.children.splice(i, 1)
    c.parentElement = null
    return c
  }
  remove(): void {
    if (this.parentElement) this.parentElement.removeChild(this)
  }
  setAttribute(k: string, v: string): void { this.attrs[k] = v }
  removeAttribute(k: string): void { delete this.attrs[k] }
  pause(): void { this.paused = true }
  load(): void { /* 真实实现会释放解码器；这里只需要能被调用 */ }
}

const globalWithDocument = globalThis as unknown as { document: { createElement: (tag: string) => FakeNode } }
globalWithDocument.document = { createElement: (tag: string) => new FakeNode(tag) }

const hostEl = (): FakeNode => new FakeNode('div')
const asHost = (n: FakeNode): HTMLElement => n as unknown as HTMLElement

beforeEach(() => resetMediaStage())

test('attach 后元素在宿主里；detach 只摘下不销毁（切标签不重下）', () => {
  const host = hostEl()
  const h = acquireMediaElement('hai-media://local/a.mp4', 'video', 'hai-media://local/a.mp4')
  h.attach(asHost(host))
  assert.equal(host.children.length, 1)
  h.el.currentTime = 42
  h.detach()
  assert.equal(host.children.length, 0)
  const snap = mediaStageSnapshot()
  assert.equal(snap.length, 1, '元素还留在池里')
  assert.equal(snap[0].attachedCount, 0)
  // 再挂回来：还是同一份元素（缓冲区/currentTime 都在）
  const again = acquireMediaElement('hai-media://local/a.mp4', 'video', 'hai-media://local/a.mp4')
  assert.equal(again.el.currentTime, 42)
  assert.equal(again.el, h.el)
})

test('同一 URL 两个标签：A detach 不得拔掉 B 正在显示的元素（引用计数）', () => {
  const hostA = hostEl()
  const hostB = hostEl()
  const a = acquireMediaElement('hai-media://local/shared.mp4', 'video', 'hai-media://local/shared.mp4', { srcPath: 'shared.mp4' })
  a.attach(asHost(hostA))
  const b = acquireMediaElement('hai-media://local/shared.mp4', 'video', 'hai-media://local/shared.mp4', { srcPath: 'shared.mp4' })
  b.attach(asHost(hostB))
  // 元素跟着后来者走（一个元素只能在一个宿主里）
  assert.equal(hostB.children.length, 1)
  assert.equal(hostA.children.length, 0)
  a.detach() // 前一个标签切走
  assert.equal(hostB.children.length, 1, 'B 正在显示的元素不能被 A 的 detach 拔掉')
  assert.equal(mediaStageSnapshot()[0].attachedCount, 1, '计数必须还剩 1（B 还挂着）')
  b.detach()
  assert.equal(mediaStageSnapshot()[0].attachedCount, 0)
})

test('dispose() 真的销毁并移除（关标签释放解码器，不是死代码）', () => {
  const host = hostEl()
  const h = acquireMediaElement('hai-media://local/b.mp4', 'video', 'hai-media://local/b.mp4')
  h.attach(asHost(host))
  h.dispose()
  assert.equal(host.children.length, 0)
  assert.equal(mediaStageSnapshot().length, 0, '池子里也不该留下这个元素')
})

test('disposeMediaElementsForPath：没人挂着 → 当场销毁；还有人挂着 → 等最后一个 detach', () => {
  const host = hostEl()
  const h = acquireMediaElement('hai-media://local/c.mp4', 'video', 'hai-media://local/c.mp4', { srcPath: 'c.mp4' })
  // 没挂载时关标签：立即销毁
  disposeMediaElementsForPath('c.mp4')
  assert.equal(mediaStageSnapshot().length, 0)

  // 挂着时关标签（另一个标签还在显示同一个文件）：先标记，等最后一个 detach
  const h2 = acquireMediaElement('hai-media://local/d.mp4', 'video', 'hai-media://local/d.mp4', { srcPath: 'd.mp4' })
  h2.attach(asHost(host))
  disposeMediaElementsForPath('d.mp4')
  assert.equal(mediaStageSnapshot().length, 1, '还挂着 → 不能当场销毁（会把活 DOM 里的元素拔掉）')
  assert.equal(mediaStageSnapshot()[0].pendingDispose, true)
  h2.detach()
  assert.equal(mediaStageSnapshot().length, 0, '最后一个 handle 走了 → 这时才销毁')
  void h
})

test('按路径回收不误伤别的文件（srcPath 精确匹配）', () => {
  const h1 = acquireMediaElement('hai-media://local/x.mp4', 'video', 'hai-media://local/x.mp4', { srcPath: 'x.mp4' })
  const h2 = acquireMediaElement('hai-media://local/y.mp4', 'video', 'hai-media://local/y.mp4', { srcPath: 'y.mp4' })
  disposeMediaElementsForPath('x.mp4')
  const snap = mediaStageSnapshot()
  assert.deepEqual(snap.map((s) => s.srcPath), ['y.mp4'])
  void h1
  void h2
})

test('池上限 4：淘汰的只能是**没挂着**的，活着的元素不被淘汰', () => {
  const host = hostEl()
  const live = acquireMediaElement('hai-media://local/live.mp4', 'video', 'hai-media://local/live.mp4', { srcPath: 'live.mp4' })
  live.attach(asHost(host))
  for (let i = 0; i < 6; i++) {
    acquireMediaElement(`hai-media://local/tmp-${i}.mp4`, 'video', `hai-media://local/tmp-${i}.mp4`)
  }
  const keys = mediaStageSnapshot().map((s) => s.key)
  assert.ok(keys.includes('hai-media://local/live.mp4'), '正在显示的元素不能被淘汰')
  assert.ok(keys.length <= 4, `池子不该超过 4（实得 ${keys.length}）`)
})
