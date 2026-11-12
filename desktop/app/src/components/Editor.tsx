// 内嵌编辑器（PR-05b / F32：零依赖受控 textarea + 行号栏）。
//
// 为什么不用 CodeMirror（04-review F32 已裁决）：V0.1 要的是「能改能存能撞冲突」，
// 语法高亮只是锦上添花，而 CM6 会把三条主链路（受控值、滚动同步、⌘S 作用域）都变成
// 「库的语义 vs 我们的语义」的对账。受控 textarea 的语义只有一条：value 就是真相。
//
// 这个文件只负责**画**与**焦点/光标**；状态机在 lib/editorState.ts，命令与会话在
// lib/editorDocs.ts（两者都有单测，见 scripts/test-editor.mjs）。
import { useCallback, useEffect, useLayoutEffect, useRef, useSyncExternalStore } from 'react'
import { fileKeyOf, useAppStore } from '../store/useAppStore'
import type { FileTabInstance } from '../store/useAppStore'
import { useT } from '../i18n'
import { bindQuitRequest, editorDocs } from '../lib/editorRuntime'
import { isEditorEnabled, saveBlock } from '../lib/editorState'
import { FilePreviewTab } from './FilePreview'

/** 光标所在行号（1 起）：行号栏高亮与「外部变更后保留光标行」共用。 */
function lineOf(text: string, offset: number): number {
  let line = 1
  const end = Math.min(Math.max(offset, 0), text.length)
  for (let i = 0; i < end; i++) if (text.charCodeAt(i) === 10) line++
  return line
}
/** 行首偏移：把「第 n 行」换回 selectionStart。 */
function offsetOfLine(text: string, line: number): number {
  if (line <= 1) return 0
  let cur = 1
  for (let i = 0; i < text.length; i++) {
    if (text.charCodeAt(i) !== 10) continue
    if (++cur === line) return i + 1
  }
  return text.length
}

export function Editor({ tab }: { tab: FileTabInstance }) {
  const t = useT()
  const enabled = useAppStore((s) => isEditorEnabled(s.settings))
  // 标签的 path 可能是工作区相对；文档的 path 用 file_open 回显的绝对路径
  // （file_changed 事件也带绝对路径 —— 两者必须同源，否则外部变更永远匹配不上）。
  const path = tab.path ?? ''
  const doc = useSyncExternalStore(
    editorDocs.subscribe,
    () => editorDocs.get(tab.id),
    () => editorDocs.get(tab.id),
  )

  const ws = useAppStore((s) => s.workspace)
  const areaRef = useRef<HTMLTextAreaElement>(null)
  const gutterRef = useRef<HTMLDivElement>(null)
  const caretLine = useRef(1)
  const prevStatus = useRef(doc?.status)

  // 退出询问的绑定在这里补一次（幂等）：脏文档只能由编辑器产生，所以「有东西要保护」
  // 的时刻一定已经绑定；模块加载时 window.desktop 未就位的环境（L1/探针）也能接上。
  useEffect(() => { bindQuitRequest() }, [])

  // 进入编辑 = 发一次 file_open。**判据是「这个标签是否已经有同路径的文档」而不是挂载次数**：
  // 切标签会把本组件整体卸载重挂（RightPanel 的 key={tab.id}），若挂载即重读，切走再切回就
  // 会静默丢掉后台保留的 dirty 内容（PR-05b 条款 6 明确允许后台保留）。
  useEffect(() => {
    // !enabled：开关关掉后这个标签退回预览（见下方早返回），不该再去 bridge 登记打开集合。
    if (!path || !enabled) return
    const cur = editorDocs.get(tab.id)
    if (cur && fileKeyOf(cur.path, ws) === fileKeyOf(path, ws)) return
    void editorDocs.open(tab.id, path)
  }, [tab.id, path, ws, enabled])

  // 重新加载（status: loading → ready）后把光标对回**原来的行**（列允许丢：长度变了，
  // 列没有意义 —— PR-05b 的验收口径就是「保留光标所在行」）。
  useLayoutEffect(() => {
    const was = prevStatus.current
    prevStatus.current = doc?.status
    const el = areaRef.current
    if (!el || was !== 'loading' || doc?.status !== 'ready') return
    const off = offsetOfLine(doc.content, caretLine.current)
    el.setSelectionRange(off, off)
    el.scrollTop = Math.max(0, (caretLine.current - 5) * 18) // 行高见 CSS（.editor-area）
  }, [doc?.status, doc?.content])

  const syncScroll = useCallback(() => {
    if (gutterRef.current && areaRef.current) gutterRef.current.scrollTop = areaRef.current.scrollTop
  }, [])

  // 在行号栏上滚轮也要滚正文（「双向同步」的另一半：只跟不领，等于行号大了不跟随）。
  const onGutterWheel = useCallback((e: React.WheelEvent) => {
    if (!areaRef.current) return
    areaRef.current.scrollTop += e.deltaY
    syncScroll()
  }, [syncScroll])

  const remember = useCallback(() => {
    const el = areaRef.current
    if (el && doc) caretLine.current = lineOf(el.value, el.selectionStart)
  }, [doc])

  const save = useCallback(async (overwrite = false) => {
    const blocked = saveBlock(editorDocs.get(tab.id), overwrite)
    if (blocked) return // 只读/失效/已删除：界面上的说明已经写清楚了，静默拦住即可
    await editorDocs.save(tab.id, { overwrite })
  }, [tab.id])

  if (!enabled) return <FilePreviewTab tab={{ ...tab, mode: 'source' }} />
  if (!path) return <div className="editor-empty">{t('editor.noPath')}</div>

  const content = doc?.content ?? ''
  const lineCount = content.length ? content.split('\n').length : 1
  const readonly = !!(doc && doc.readonly)
  const banner = doc?.banner
  const blocked = saveBlock(doc)
  const closeBanner = () => editorDocs.dismiss(tab.id)
  const reload = () => { void editorDocs.reload(tab.id) }
  const conflictBar = (kind: 'conflict' | 'external' | 'deleted') => (
    <div className="editor-banner warn" role="alert" data-kind={kind}>
      <span className="editor-banner-t">
        {kind === 'conflict' ? t('editor.conflict.title')
          : kind === 'external' ? t('editor.external.title')
            : t('editor.deleted.title')}
      </span>
      {kind === 'conflict' && banner?.currentHash && <span className="editor-banner-mono mono">{t('editor.conflict.disk')} {String(banner.currentHash).slice(0, 8)}</span>}
      <span className="editor-banner-acts">
        <button className="editor-btn" onClick={reload}>{t('editor.conflict.reload')}</button>
        <button className="editor-btn primary" onClick={() => void save(true)}>{kind === 'deleted' ? t('editor.deleted.recreate') : t('editor.conflict.overwrite')}</button>
        <button className="editor-btn" onClick={closeBanner}>{t('editor.conflict.cancel')}</button>
      </span>
    </div>
  )

  return (
    <div className="editor-root" onKeyDown={(e) => { if (e.key === 'Escape' && banner) { e.stopPropagation(); closeBanner() } }}>
      <div className="editor-head">
        <span className="editor-path mono" title={path}>{path}</span>
        <span className="editor-stat mono">{t('editor.lines').replace('{n}', String(lineCount))}</span>
        {doc?.dirty && <span className="editor-dirty" aria-label={t('editor.dirty')} title={t('editor.dirty')} />}
        <button
          className="editor-btn primary"
          disabled={blocked !== ''}
          title={t('editor.saveHint')}
          onClick={() => void save()}
        >{doc?.saving ? t('editor.saving') : t('editor.save')}</button>
      </div>

      {banner && (banner.kind === 'conflict' || banner.kind === 'external' || banner.kind === 'deleted') && conflictBar(banner.kind)}
      {banner && banner.kind === 'invalid' && (
        <div className="editor-banner danger" role="alert">
          <span className="editor-banner-t">{t('editor.invalid.title')}</span>
          <span className="editor-banner-acts">
            <button className="editor-btn primary" onClick={reload}>{t('editor.invalid.reopen')}</button>
            <button className="editor-btn" onClick={closeBanner}>{t('editor.conflict.cancel')}</button>
          </span>
        </div>
      )}
      {banner && banner.kind === 'error' && (
        <div className="editor-banner danger" role="alert">
          <span className="editor-banner-t">{t('editor.error.title')}</span>
          {banner.message && <span className="editor-banner-msg mono">{banner.message}</span>}
          <span className="editor-banner-acts">
            <button className="editor-btn" onClick={closeBanner}>{t('editor.conflict.cancel')}</button>
          </span>
        </div>
      )}
      {(readonly || doc?.status === 'loading') && (
        <div className={'editor-note' + (readonly ? ' ro' : '')} role="note">
          {doc?.status === 'loading'
            ? t('editor.loading')
            : doc?.readonly === 'binary' ? t('editor.ro.binary') : t('editor.ro.tooLarge')}
        </div>
      )}

      <div className="editor-body">
        <div className="editor-gutter mono" ref={gutterRef} aria-hidden="true" onWheel={onGutterWheel}>
          {Array.from({ length: lineCount }, (_, i) => <div key={i}>{i + 1}</div>)}
        </div>
        <textarea
          ref={areaRef}
          className="editor-area mono"
          value={content}
          readOnly={readonly || doc?.status !== 'ready'}
          spellCheck={false}
          autoComplete="off"
          autoCorrect="off"
          autoCapitalize="off"
          aria-label={t('editor.aria')}
          onChange={(e) => { editorDocs.change(tab.id, e.target.value); remember() }}
          onScroll={syncScroll}
          onSelect={remember}
          onClick={remember}
          onKeyUp={remember}
          onKeyDown={(e) => {
            // ⌘S / Ctrl+S **只在这里**拦截（D-C：不绑全局）。焦点在别处时浏览器/系统
            // 的默认行为保持不变 —— 全局绑定会在用户敲对话输入框时误存文件。
            if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 's') {
              e.preventDefault()
              void save()
            }
          }}
        />
      </div>
    </div>
  )
}
