import { useEffect, useRef, useState } from 'react'
import { Markdown } from '../components/markdown'
import CsvPreview from '../components/CsvPreview'
import { useT } from '../i18n'
import { copyText } from '../lib/clipboard'
import { downloadText, safeFilename } from './download'
import { useHubStore } from './useHubStore'

// App 级任务产物预览弹窗（P4-UI-12）。
//
// 只消费 hub_task_artifact_preview 的结果：不做「外部打开 / 在访达显示」——
// Electron 的 shell:open-path 对 ~/.go-code 会拒绝，文件内容只能在这里看。

const IMAGE_EXT = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'svg', 'bmp'])
const MD_EXT = new Set(['md', 'mdx'])
const CSV_EXT = new Set(['csv', 'tsv'])

export default function ArtifactPreview() {
  const state = useHubStore((s) => s.artifactPreview)
  const close = useHubStore((s) => s.closeArtifactPreview)
  const open = useHubStore((s) => s.openArtifactPreview)
  const t = useT()
  const dialogRef = useRef<HTMLDivElement>(null)
  const triggerRef = useRef<HTMLElement | null>(null)
  const [copied, setCopied] = useState(false)
  const isOpen = Boolean(state)
  const key = state?.key

  // 打开/关闭只跑一次：loading→data 不重挂弹窗，也不把焦点抢回来。
  // 关闭时焦点回到触发按钮（键盘用户不能“掉”到 body）。
  useEffect(() => {
    if (!isOpen) return
    triggerRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    const timer = window.setTimeout(() => dialogRef.current?.focus(), 0)
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopPropagation()
        close()
      }
    }
    window.addEventListener('keydown', onKey, true)
    return () => {
      window.clearTimeout(timer)
      window.removeEventListener('keydown', onKey, true)
      const el = triggerRef.current
      if (el && document.documentElement.contains(el)) el.focus()
    }
  }, [isOpen, close])

  useEffect(() => {
    setCopied(false)
  }, [key])

  if (!state) return null

  const data = state.data
  const displayName = data?.name || state.name || data?.path || state.path || '文件'
  const extSource = data?.name || state.name || data?.path || state.path || ''
  const ext = (extSource.split('.').pop() ?? '').toLowerCase().replace(/[^a-z0-9]+$/g, '')
  const isImage = Boolean(data?.data_url) && IMAGE_EXT.has(ext)
  const content = data?.content
  const canDownload = Boolean(content) || isImage
  const retry = () => void open(state.taskId, { path: state.path, name: state.name })

  const onDownload = () => {
    if (!data) return
    if (content) {
      downloadText(displayName, content, 'text/plain;charset=utf-8')
      return
    }
    if (isImage && data.data_url) {
      try {
        const a = document.createElement('a')
        a.href = data.data_url
        a.download = safeFilename(displayName, 'artifact')
        a.rel = 'noopener'
        a.click()
      } catch {
        /* 下载失败时保留弹窗；浏览器会给出自己的提示 */
      }
    }
  }

  return (
    <div
      className="hub-modal-scrim hub-artifact-scrim"
      role="presentation"
      onClick={(e) => {
        if (e.target === e.currentTarget) close()
      }}
    >
      <div
        ref={dialogRef}
        className="hub-modal hub-artifact-modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="hub-artifact-title"
        tabIndex={-1}
        onClick={(e) => e.stopPropagation()}
      >
        <header className="hub-modal-head hub-artifact-head">
          <div className="hub-artifact-titles">
            <span id="hub-artifact-title" className="hub-modal-title">{displayName}</span>
            <span className="hub-artifact-sub">
              {data?.path && data.path !== displayName ? <span className="hub-artifact-path">{data.path}</span> : null}
              {data ? <span>{bytesText(data.size)}</span> : null}
              {data?.lines ? <span>{t('artifacts.lines').replace('{n}', String(data.lines))}</span> : null}
            </span>
          </div>
          <button type="button" className="hub-link hub-artifact-close" onClick={close}>
            {t('hub.close')}
          </button>
        </header>

        <div className="hub-artifact-body">
          {state.loading ? (
            <div className="hub-artifact-note" role="status">{t('hub.artifact.loading')}</div>
          ) : state.error ? (
            <div className="hub-artifact-error" role="alert">
              <p>{state.error}</p>
              <button type="button" className="hub-btn" onClick={retry}>{t('hub.artifact.retry')}</button>
            </div>
          ) : data ? (
            <>
              {data.truncated ? <div className="hub-artifact-note">{t('hub.artifact.truncated')}</div> : null}
              {isImage ? (
                <div className="hub-artifact-image">
                  <img src={data.data_url} alt={displayName} />
                </div>
              ) : content !== undefined ? (
                MD_EXT.has(ext) ? (
                  // 不传 baseDir：任务产物没有“工作区相对资源”的语义，绝对 baseDir 会泄漏。
                  <div className="hub-artifact-doc"><Markdown text={content} /></div>
                ) : CSV_EXT.has(ext) ? (
                  <div className="hub-artifact-doc"><CsvPreview content={content} path={displayName} /></div>
                ) : (
                  <pre className="hub-artifact-pre">{content}</pre>
                )
              ) : data.binary ? (
                <div className="hub-artifact-note">
                  {t('hub.artifact.binary').replace('{name}', displayName)}
                  {data.mime ? <span className="hub-artifact-mime">{data.mime}</span> : null}
                </div>
              ) : (
                <div className="hub-artifact-note">{t('hub.artifact.empty')}</div>
              )}
            </>
          ) : null}
        </div>

        <footer className="hub-artifact-foot">
          <span className="hub-artifact-copied" role="status">{copied ? t('hub.artifact.copied') : ''}</span>
          {content !== undefined ? (
            <button
              type="button"
              className="hub-btn"
              onClick={() => {
                void copyText(content).then((ok) => setCopied(ok))
              }}
            >
              {t('hub.artifact.copy')}
            </button>
          ) : null}
          {canDownload ? (
            <button type="button" className="hub-btn" onClick={onDownload}>{t('hub.artifact.download')}</button>
          ) : null}
        </footer>
      </div>
    </div>
  )
}

function bytesText(bytes: number): string {
  if (!bytes || bytes <= 0) return ''
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`
}
