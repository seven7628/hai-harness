// 编辑器文档会话（PR-05b）：把「哪个标签开着哪个文档、它的 baseHash 是多少、这次保存
// 要不要发」收在一处，组件只负责画。
//
// 依赖注入 send 而不是直接 import store：① 单测能给出「第二次保存不会发出去」这种
// 需要数调用的断言，而不用起 React/Electron；② 命令通道由宿主决定（真 bridge 走
// transport，mock 走 mock 的虚拟文件），这里不关心。
//
// 生命周期：文档按**标签 id** 存（不是路径）—— 同一个文件可以同时有 preview 与 edit
// 两个标签，它们的 baseHash 必须各记各的；关标签时 file_close 退订一次。
import { emptyDoc, externalEffect, reduceDoc, saveBlock, savePayload } from './editorState'
import type { EditorDoc, SaveBlock } from './editorState'

export interface EditorReply {
  ok: boolean
  error?: string
  code?: string
  data?: Record<string, unknown>
}

export interface EditorDocsDeps {
  /** 发一条 file_* 命令；永不在失败时 reject（返回 ok:false + code） */
  send: (type: 'file_open' | 'file_save' | 'file_close', payload: Record<string, unknown>) => Promise<EditorReply>
  /** 某个标签的脏状态变了（宿主据此点亮标签圆点 / 原生红点） */
  onDirty?: (tabId: string, dirty: boolean) => void
  /** 任何文档变了（组件重绘用；也在 open/save 的中间态触发） */
  onDoc?: (tabId: string, doc: EditorDoc) => void
}

export interface SaveOutcome {
  ok: boolean
  /** 非空 = 压根没发请求（只读/失效/已删除/正在保存） */
  blocked?: SaveBlock
  code?: string
  error?: string
}

type FileOpenData = { path?: unknown; content?: unknown; hash?: unknown; truncated?: unknown; binary?: unknown }

interface Entry {
  tabId: string
  doc: EditorDoc
  /** 在途请求代号：迟到的 file_open 响应不能覆盖更新的那一次 */
  token: number
}

export function createEditorDocs(deps: EditorDocsDeps) {
  const docs = new Map<string, Entry>()
  let token = 0

  const listeners = new Set<() => void>()
  const emit = (tabId: string): void => {
    const doc = docs.get(tabId)?.doc
    if (doc) deps.onDoc?.(tabId, doc)
    for (const l of listeners) l()
  }
  const apply = (tabId: string, action: Parameters<typeof reduceDoc>[1]): EditorDoc | undefined => {
    const e = docs.get(tabId)
    if (!e) return undefined
    const was = e.doc.dirty
    e.doc = reduceDoc(e.doc, action)
    if (e.doc.dirty !== was) deps.onDirty?.(tabId, e.doc.dirty)
    emit(tabId)
    return e.doc
  }

  /** file_open 一条（首次打开 / 重新加载 / 干净态的外部变更，共用）。 */
  async function load(tabId: string, path: string, reset: boolean): Promise<EditorDoc | undefined> {
    const prev = docs.get(tabId)
    const wasDirty = prev?.doc.dirty === true
    const base = reset ? (prev?.doc ?? emptyDoc(path)) : emptyDoc(path)
    docs.set(tabId, { tabId, doc: reduceDoc(base, { type: 'loading', path }), token: ++token })
    // loading 一定把 dirty 归零（内容还没回来）：宿主的镜像必须同步清掉，否则会出现
    // 「重新加载后标签圆点还在」——圆点读的是 store 的 dirtyTabs，不跟着 reducer 走就会说谎。
    if (wasDirty) deps.onDirty?.(tabId, false)
    emit(tabId)
    const mine = docs.get(tabId)!.token
    const r = await deps.send('file_open', { path })
    if (docs.get(tabId)?.token !== mine) return undefined // 迟到的响应（已重载/已关标签）
    if (!r.ok) {
      apply(tabId, { type: 'open-failed', code: r.code, message: r.error })
      return docs.get(tabId)?.doc
    }
    const d = (r.data ?? {}) as FileOpenData
    const doc = apply(tabId, {
      type: 'opened',
      path: typeof d.path === 'string' && d.path ? d.path : path,
      content: typeof d.content === 'string' ? d.content : '',
      hash: typeof d.hash === 'string' ? d.hash : '',
      truncated: d.truncated === true,
      binary: d.binary === true,
    })
    return doc
  }

  return {
    get(tabId: string): EditorDoc | undefined {
      return docs.get(tabId)?.doc
    },

    /** 组件订阅（useSyncExternalStore）：任何文档的任何变化都会通知。 */
    subscribe(fn: () => void): () => void {
      listeners.add(fn)
      return () => { listeners.delete(fn) }
    },

    /** 进入编辑（首次）：读盘 + 记 baseHash/baseContent。 */
    open(tabId: string, path: string): Promise<EditorDoc | undefined> {
      return load(tabId, path, false)
    },

    /** 重新加载（丢弃本地改动）：清条 + 重读。冲突条的「重新加载」与「重新打开」都走它。 */
    reload(tabId: string): Promise<EditorDoc | undefined> {
      const cur = docs.get(tabId)
      if (!cur) return Promise.resolve(undefined)
      return load(tabId, cur.doc.path, true)
    },

    change(tabId: string, content: string): void {
      apply(tabId, { type: 'change', content })
    },

    /**
     * 保存。overwrite=true 只由冲突条的「覆盖保存」传入（baseHash 清空 = 无条件写）；
     * 「删除后仍要新建」也走 overwrite —— 那是用户显式确认过的。
     */
    async save(tabId: string, opts: { overwrite?: boolean } = {}): Promise<SaveOutcome> {
      const e = docs.get(tabId)
      const blocked = saveBlock(e?.doc, opts.overwrite)
      // 关键：blocked 时**一条命令都不发**。denied 之后再按 ⌘S 无限重发是本 PR 最容易
      // 写出的一种死循环（每次都被拒，用户只看到红条闪）。
      if (blocked) return { ok: false, blocked }
      const doc = e!.doc
      // payload.content 是**写盘形态**（按文件原本的 eol 还原过）；reducer 要的是编辑器空间的
      // 那一段（LF），否则 `saved` 里 baseContent 会变成 CRLF 而 content 是 LF → 立刻又脏。
      const payload = { ...savePayload(doc, opts.overwrite), ...(opts.overwrite && doc.deleted ? { allowNew: true } : {}) }
      apply(tabId, { type: 'save-start' })
      const r = await deps.send('file_save', payload)
      if (!r.ok) {
        const d = (r.data ?? {}) as { currentHash?: unknown }
        apply(tabId, {
          type: 'save-failed',
          code: r.code,
          currentHash: typeof d.currentHash === 'string' ? d.currentHash : undefined,
          message: r.error,
        })
        return { ok: false, code: r.code, error: r.error }
      }
      const d = (r.data ?? {}) as { hash?: unknown }
      const hash = typeof d.hash === 'string' ? d.hash : ''
      apply(tabId, { type: 'saved', content: doc.content, hash })
      return { ok: true }
    },

    /** 关顶部条（Esc）。只关条，内容与 baseContent 都不动。 */
    dismiss(tabId: string): void {
      apply(tabId, { type: 'banner-close' })
    },

    /** 外部变更（file_changed，by:"external"）：按**绝对路径**命中所有开着它的标签。 */
    external(path: string, ev: { hash?: string; deleted?: boolean }): void {
      for (const [tabId, e] of docs) {
        if (e.doc.path !== path) continue
        // 干净 → 静默重载（光标行由组件自己记住再对回来）；脏 → 只挂条，绝不动内容。
        if ('banner' in externalEffect(e.doc, ev)) {
          apply(tabId, { type: 'external', ...ev })
        } else {
          void load(tabId, e.doc.path, true)
        }
      }
    },

    /** 关标签：退订 + 丢本地状态（dirty 内容随标签一起消失，确认框在调用方之前）。 */
    close(tabId: string): void {
      const e = docs.get(tabId)
      docs.delete(tabId)
      if (e?.doc.dirty) deps.onDirty?.(tabId, false)
      if (e && e.doc.path) void deps.send('file_close', { path: e.doc.path })
    },

    anyDirty(): boolean {
      for (const e of docs.values()) if (e.doc.dirty) return true
      return false
    },

    /** 测试/调试用：当前全部文档（不暴露 Map，避免外部改写内部状态）。 */
    snapshot(): Array<{ tabId: string; doc: EditorDoc }> {
      return [...docs.values()].map((e) => ({ tabId: e.tabId, doc: e.doc }))
    },
  }
}

export type EditorDocs = ReturnType<typeof createEditorDocs>
