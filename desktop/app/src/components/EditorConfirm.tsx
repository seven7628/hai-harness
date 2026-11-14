// 脏内容确认框（PR-05b 条款 6/7）：关 dirty 标签与「退出应用」**共用同一个框**。
//
// 为什么放在组件里而不是 store 的 action 里：三选里「保存」要先发 file_save 并等结果，
// 成功才关标签 —— 这条链路跨了命令通道（editorDocs）与标签集（store）。放在 UI 层既
// 看得见顺序，也不用让 store 反向依赖编辑器会话（那会绕出 import 环）。
import { useAppStore } from '../store/useAppStore'
import type { FileTabInstance } from '../store/useAppStore'
import { useT } from '../i18n'
import { editorDocs } from '../lib/editorRuntime'
import { bridge } from '../lib/bridge'

export function EditorConfirm() {
  const t = useT()
  const confirm = useAppStore((s) => s.closeConfirm)
  const closeNext = useAppStore((s) => s.closeConfirmNext)
  // 标签集按作用域存（tabsByScope）；确认框要按 id 找到那个标签的路径显示文件名。
  const tabsByScope = useAppStore((s) => s.tabsByScope)
  const scope = useAppStore((s) => s.activeSessionId || s.workspace || '')
  const cancel = useAppStore((s) => s.cancelCloseConfirm)
  const activateTab = useAppStore((s) => s.activateTab)
  if (!confirm) return null

  // 队列：批量关闭时一次问一个（关其它/关右侧撞到多个脏标签 —— 不能只问最后一个）
  const queue = confirm.reason === 'close' ? confirm.tabIds : []
  const tabId = queue[0]
  const tab = (tabsByScope[scope]?.tabs ?? []).find((x) => x.id === tabId) as FileTabInstance | undefined
  const name = (tab?.path ?? '').split('/').pop() || ''
  const quit = confirm.reason === 'quit'

  // 保存失败（冲突/失效/删除）：留在原地没有意义 —— 用户需要看见那条黄条/红条。
  // 把标签切到前台，黄条就在那儿，三条动作都在。
  const revealAndClose = (id: string) => {
    activateTab(id)
    cancel()
  }

  const onSave = async () => {
    if (quit) {
      // 退出：把所有脏文档各存一遍；任何一个失败就停下并把它切到前台（不静默丢内容）
      const dirty = editorDocs.snapshot().filter((e) => e.doc.dirty)
      for (const e of dirty) {
        const r = await editorDocs.save(e.tabId)
        if (!r.ok) return revealAndClose(e.tabId)
      }
      bridge?.quitAnyway?.()
      cancel()
      return
    }
    if (!tabId) return cancel()
    const r = await editorDocs.save(tabId)
    if (!r.ok) return revealAndClose(tabId)
    closeNext(tabId)
  }

  const onDiscard = () => {
    if (quit) {
      bridge?.quitAnyway?.()
      cancel()
      return
    }
    if (tabId) closeNext(tabId)
  }

  return (
    <div className="editor-confirm-backdrop" onClick={cancel}>
      <div
        className="editor-confirm"
        role="dialog"
        aria-modal="true"
        aria-label={t(quit ? 'editor.quit.title' : 'editor.close.title')}
        onClick={(e) => e.stopPropagation()}
        onKeyDown={(e) => { if (e.key === 'Escape') { e.stopPropagation(); cancel() } }}
      >
        <div className="editor-confirm-title">{t(quit ? 'editor.quit.title' : 'editor.close.title')}</div>
        {!quit && name && <div className="editor-confirm-file mono">{name}</div>}
        {!quit && queue.length > 1 && <div className="editor-confirm-more">{t('editor.close.more').replace('{n}', String(queue.length - 1))}</div>}
        <div className="editor-confirm-acts">
          <button className="editor-btn" onClick={cancel} autoFocus>{t('editor.close.cancel')}</button>
          <button className="editor-btn danger" onClick={onDiscard}>{t('editor.close.discard')}</button>
          <button className="editor-btn primary" onClick={() => void onSave()}>{t('editor.close.save')}</button>
        </div>
      </div>
    </div>
  )
}
