// 编辑器会话的**宿主接线**（PR-05b）：把 lib/editorDocs 的纯会话接到 store / transport /
// 主进程上。放这里而不是塞进 Editor.tsx，是因为它有三条与组件生命周期无关的订阅：
//
//   · file_changed 事件 → 按路径路由到开着它的标签（标签没激活/组件没挂载时也要收）
//   · 标签集变化 → 真关掉的标签要 file_close 退订（否则 bridge 的打开集合与目录 watch
//     只增不减，64 条 LRU 迟早把别的文件挤掉）
//   · 主进程的退出请求 → 复用同一个脏确认框
//
// 副作用订阅装在模块加载时（与 store 的标签持久化订阅同款），幂等。
import { useAppStore } from '../store/useAppStore'
import type { AppState } from '../store/useAppStore'
import { createEditorDocs } from './editorDocs'

export const editorDocs = createEditorDocs({
  send: (type, payload) => useAppStore.getState().editCommand(type, payload),
  onDirty: (tabId, dirty) => useAppStore.getState().setTabDirty(tabId, dirty),
})

// 所有作用域里还活着的标签 id（关标签判定用 `tabsByScope` 而不是当前会话的 tabs：
// 切会话只是「看不见」，标签还在，缓冲区与 file_close 都必须保持）。
function liveTabIds(s: AppState): Set<string> {
  const ids = new Set<string>()
  for (const set of Object.values(s.tabsByScope ?? {})) {
    for (const t of set?.tabs ?? []) ids.add(t.id)
  }
  return ids
}

// bindQuitRequest：把主进程的退出询问接到确认框上。**幂等且可重试** —— 模块加载时
// window.desktop 可能还没就位（L1 测试环境根本没有；探针/setup 顺序也可能晚一步），
// 所以 Editor 挂载时会再调一次。脏文档只可能由编辑器产生 → 那时一定已绑定成功。
let quitBound = false
export function bindQuitRequest(): void {
  if (quitBound || typeof window === 'undefined') return
  const d = window.desktop
  if (!d?.onQuitRequest) return
  quitBound = true
  d.onQuitRequest(() => useAppStore.setState({ closeConfirm: { reason: 'quit' } }))
}

let installed = false
export function installEditorRuntime(): void {
  if (installed) return
  installed = true
  let lastSeq = 0
  useAppStore.subscribe((s, prev) => {
    if (s.fileChange && s.fileChange.seq !== lastSeq) {
      lastSeq = s.fileChange.seq
      editorDocs.external(s.fileChange.path, { hash: s.fileChange.hash, deleted: s.fileChange.deleted })
    }
    if (s.tabsByScope !== prev.tabsByScope) {
      const live = liveTabIds(s)
      for (const { tabId } of editorDocs.snapshot()) {
        if (!live.has(tabId)) editorDocs.close(tabId)
      }
    }
  })
  // 主进程 before-quit 拦住一次 → 渲染层用同一个确认框问用户（PR-05b 条款 7）。
  bindQuitRequest()
}

installEditorRuntime()
