// docx / pptx / xlsx 渲染器：复用**现有转换产物**，把 HTML 摆进面板（R08b / 01-design §4.7）
//
// 与 previewOfficeFile 的关系（不是重复实现，是分工）：
//   · previewOfficeFile（既有）= 「转换 → 产物路径 → 内嵌浏览器 file:// 标签」，给**浏览器**看；
//   · 本组件 = 同一个 bridge 命令（xlsx_preview / docx_preview / pptx_preview）拿到同一个产物
//     路径，但把内容**读回前端**渲染在预览标签里 —— 因为这里要的是「文件面板内的一块内容」，
//     而不是再开一条原生视图。
//
// 为什么读回而不是把产物路径塞进 iframe：dev 模式 renderer 是 http://localhost，产物在
// 临时目录（file://），跨 scheme 的 iframe 一律被 Chromium 拦；那条路只有原生视图能走。
// 代价是 file_preview 的 1 MiB 上限 —— 超了必须**如实说**并给完整版出口（截断的 HTML 会丢尾）。
//
// 保真度如实交代（延续 pptx_preview.go 的「诚实比假装像更重要」）：顶部固定一条能力提示。
// docx 走 mammoth（语义 HTML，版式近似）、pptx 走 python-pptx（近似版式）、xlsx 走 openpyxl
// （值 + 基本样式）。都不是 LibreOffice 级保真 —— 那要 kit（PR-08d）。
//
// 依赖缺失（浏览器预览模式 / 旧桌面端没有 conversion IPC）由 registry 的 requires 在挂载前
// 拦下；**转换失败/运行时没装好**在这里经 onUnavailable 上报真实原因（bridge 的 error 原样）。
import { useEffect, useRef, useState } from 'react'
import { useT } from '../../../i18n'
import { displayPath } from '../../../lib/localAsset'
import { DOCX_EXTS, PPTX_EXTS, XLSX_EXTS } from '../../../lib/visualFile'
import { useAppStore } from '../../../store/useAppStore'
import type { PreviewProps } from '../types'

type OfficeKind = 'docx' | 'pptx' | 'xlsx'

/**
 * 扩展名 → 转换通道。老格式（.doc/.ppt/.xls）也归到同一支：由 bridge 给出明确失败原因。
 * 刻意不 export：本文件是组件模块（react-refresh 只接受「只导出组件」），而这里也确实只有
 * 渲染器自己用得到 —— 注册表按扩展名集合 match，不经过这个函数。
 */
function officeKindOf(ext: string): OfficeKind | undefined {
  if (XLSX_EXTS.has(ext)) return 'xlsx'
  if (DOCX_EXTS.has(ext)) return 'docx'
  if (PPTX_EXTS.has(ext)) return 'pptx'
  return undefined
}

type Phase =
  | { kind: 'converting' }
  | { kind: 'ready'; html: string; truncated: boolean; htmlPath: string }
  | { kind: 'failed'; reason: string }

export default function OfficePreview({ file, onUnavailable }: PreviewProps) {
  const t = useT()
  const workspace = useAppStore((s) => s.workspace) ?? ''
  const loadOfficePreview = useAppStore((s) => s.loadOfficePreview)
  const previewOfficeFile = useAppStore((s) => s.previewOfficeFile)
  const kind = officeKindOf(file.ext)
  const [phase, setPhase] = useState<Phase>({ kind: 'converting' })

  // 失败上报走 ref：effect 只依赖 (path, kind)，回调换身份不该重转一次
  const reportRef = useRef(onUnavailable)
  reportRef.current = onUnavailable

  useEffect(() => {
    if (!kind) return
    let alive = true
    setPhase({ kind: 'converting' })
    loadOfficePreview(file.path, kind).then(
      (r) => {
        if (!alive) return
        if (!r.ok) {
          setPhase({ kind: 'failed', reason: r.error })
          reportRef.current?.(r.error)
          return
        }
        setPhase({ kind: 'ready', html: r.html, truncated: r.truncated, htmlPath: r.htmlPath })
      },
      (e: unknown) => {
        if (!alive) return
        const reason = e instanceof Error ? e.message : String(e)
        setPhase({ kind: 'failed', reason })
        reportRef.current?.(reason)
      },
    )
    return () => { alive = false }
  }, [file.path, kind, loadOfficePreview])

  // 没有对应通道（注册表不该把这种文件交给本组件）→ 说清楚，别静默空白
  if (!kind) {
    return <div className="pv-office-hint" data-office-state="failed">{t('rp.preview.officeNoKind')}</div>
  }

  if (phase.kind === 'converting') {
    return (
      <div className="pv-office" data-office-kind={kind} data-office-state="converting" role="status">
        <div className="pv-office-bar"><span className="pv-office-spin" aria-hidden="true">◌</span> {t('rp.preview.officeConverting')}</div>
        <div className="pv-office-hint">{displayPath(file.path, workspace)}</div>
        <div className="pv-office-hint mono">{t('rp.preview.officeRuntime')}</div>
      </div>
    )
  }

  if (phase.kind === 'failed') {
    // 正常情况下 onUnavailable 已经把整块换成 unavailable 占位；这一段是兜底
    //（调用方没接上报时也绝不白屏）
    return (
      <div className="pv-office" data-office-kind={kind} data-office-state="failed" role="status">
        <div className="pv-office-bar">⚠ {t('rp.preview.officeFailed')}</div>
        <div className="pv-office-hint pv-error">{phase.reason}</div>
      </div>
    )
  }

  return (
    <div className="pv-office" data-office-kind={kind} data-office-state="ready" data-office-truncated={phase.truncated ? '1' : '0'}>
      <div className="pv-office-bar">
        <span>{t(`rp.preview.officeApprox.${kind}`)}</span>
        {phase.truncated && (
          <>
            <span className="pv-office-warn">{t('rp.preview.officeTruncated')}</span>
            <button className="pv-office-more" onClick={() => void previewOfficeFile(file.path, kind)}>
              {t('rp.preview.openFull')}
            </button>
          </>
        )}
      </div>
      {/* sandbox 只开 allow-same-origin：产物的样式能生效、探针能读 contentDocument，
          但**不执行**任何脚本（转换产物是数据，不是可执行内容）。 */}
      <iframe
        className="pv-office-frame"
        sandbox="allow-same-origin"
        title={`${kind} preview: ${file.path}`}
        data-office-html-length={phase.html.length}
        srcDoc={phase.html}
      />
    </div>
  )
}
