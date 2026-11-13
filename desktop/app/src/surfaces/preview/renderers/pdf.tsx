// pdf 渲染器：把**现有内嵌浏览器视图**摆到本面板的矩形上（R08b / 01-design §4.7「pdf 继续用
// Chromium viewer，不自研 pdf.js」）。
//
// 为什么不是 <embed type="application/pdf">：Chromium 的 PDF viewer 只在原生 WebContentsView
// 里可用 —— 主窗口没开 plugins，dev 模式的 renderer 是 http://localhost，file:// 的
// iframe/embed 一律被拦。仓库里唯一被 e2e 钉住过的 PDF 通道就是内嵌视图（e2e:pdf 的 T4：
// CDP 读到 contentType=application/pdf + 全尺寸 <embed> + 像素里「纸张」占比）。故本组件的
// 实质是「视图宿主」：建/复用视图 → 摆到自己的矩形 → 卸载时按归属收尾。
//
// 它自己**不解释 PDF**（页数/文字层都拿不到 —— 渲染进程没有字节通道，见
// docs/creator-mode-v0.1/04-review.md F50 的记账）：可见性、缩放、选中、查找全部是
// Chromium viewer 给的。面板这层只负责三件事：位置、可见性、失败时说清缺什么。
//
// 失败口径（不许白屏）：没有 embed 通道（浏览器预览模式 / 旧桌面端）由 registry 的
// requires:['embed'] 在**挂载前**拦下并写原因；视图建不起来/拿不到 id 走 onUnavailable。
import { useEffect, useRef, useState } from 'react'
import { useT } from '../../../i18n'
import { displayPath } from '../../../lib/localAsset'
import { embedRectOf } from '../../../lib/embedRect'
import {
  attachPreviewView, releasePreviewView, resizePreviewView, showPreviewView,
} from '../../../lib/previewEmbed'
import type { PreviewViewHandle } from '../../../lib/previewEmbed'
import { useAppStore } from '../../../store/useAppStore'
import { useDomOverlayOpen, useUiModalOpen } from '../../../lib/uiOverlay'
import type { PreviewProps } from '../types'

type Phase = 'attaching' | 'ready' | 'error'

export default function PdfPreview({ file, onUnavailable }: PreviewProps) {
  const t = useT()
  const workspace = useAppStore((s) => s.workspace) ?? ''
  const sid = useAppStore((s) => s.activeSessionId) ?? ''
  const setPreviewEmbedActive = useAppStore((s) => s.setPreviewEmbedActive)
  const menuOpen = useAppStore((s) => s.panelMenuOpen)
  const hostRef = useRef<HTMLDivElement>(null)
  const handleRef = useRef<PreviewViewHandle | null>(null)
  const [phase, setPhase] = useState<Phase>('attaching')
  const [reason, setReason] = useState('')
  // 观测点（探针/看板用）：视图池里那条视图的 id 与「是不是本面板建的」。
  // 没有它，探针只能看到「面板 ready」——分不清背后是真有一条 pdf 视图还是自举的 about:blank。
  const [view, setView] = useState<{ id: string; created: boolean } | null>(null)
  // 覆盖层闸门与 WebTab 同一份（原生视图恒在 DOM 之上，菜单/弹窗必须先把视图藏起来）
  const modalOpen = useUiModalOpen()
  const overlayOpen = useDomOverlayOpen(hostRef, phase === 'ready')
  const blocked = menuOpen || modalOpen || overlayOpen

  // 失败上报走 ref：effect 只依赖 (path, workspace, sid)，上报回调换身份不该重挂视图
  const reportRef = useRef(onUnavailable)
  reportRef.current = onUnavailable

  useEffect(() => {
    let alive = true
    let ro: ResizeObserver | null = null
    setPhase('attaching')
    setReason('')
    ;(async () => {
      const r = await attachPreviewView({ path: file.path, workspace, sid })
      if (!alive) {
        // 组件已经卸载（切标签/关标签）→ 建出来的视图不能留在池里当幽灵
        if (r.ok) void releasePreviewView(r.handle)
        return
      }
      if (!r.ok) {
        setPhase('error')
        setReason(r.error)
        reportRef.current?.(r.error)
        return
      }
      handleRef.current = r.handle
      setView({ id: r.handle.id, created: r.handle.created })
      const el = hostRef.current
      // 先让 RightPanel 的可见性闸门知道「这条 file 标签在显示原生视图」，再 show：
      // 顺序反过来时闸门会在面板显示完立刻 embedHide（父级 effect 晚于子级跑）。
      setPreviewEmbedActive(true)
      await showPreviewView(r.handle, embedRectOf(el))
      if (!alive) { void releasePreviewView(r.handle); return }
      setPhase('ready')
      if (el && 'ResizeObserver' in window) {
        ro = new ResizeObserver(() => { void resizePreviewView(embedRectOf(el)) })
        ro.observe(el)
      }
    })()
    return () => {
      alive = false
      ro?.disconnect()
      setPreviewEmbedActive(false)
      setView(null)
      const h = handleRef.current
      handleRef.current = null
      if (h) void releasePreviewView(h)
    }
  }, [file.path, workspace, sid, setPreviewEmbedActive])

  // 覆盖层/菜单打开 → 藏；关掉 → 重新摆回来。只在 ready 之后做（建连期间不抢显示）。
  useEffect(() => {
    if (phase !== 'ready') return
    const h = handleRef.current
    if (!h) return
    if (blocked) { setPreviewEmbedActive(false); void window.desktop?.embedHide?.(); return }
    setPreviewEmbedActive(true)
    void showPreviewView(h, embedRectOf(hostRef.current))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [blocked, phase])

  return (
    <div
      className="pv-pdf"
      data-pdf-state={phase}
      data-pdf-src={file.path}
      data-pdf-view={view?.id ?? ''}
      data-pdf-created={view ? (view.created ? '1' : '0') : ''}
      ref={hostRef}
    >
      {/* 原生视图浮在这块区域上；这里的文字只在「建连中 / 被覆盖层挡住」时露出来 */}
      <div className="pv-pdf-scrim">
        <div className="pv-pdf-badge mono">{t('rp.preview.pdfNative')}</div>
        <div className="pv-pdf-path mono">{displayPath(file.path, workspace)}</div>
        {phase === 'attaching' && <div className="pv-pdf-hint">{t('rp.preview.pdfAttaching')}</div>}
        {phase === 'error' && <div className="pv-pdf-hint pv-error">{reason}</div>}
      </div>
    </div>
  )
}
