import { useEffect, useRef, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { getTransport } from '../transport'
import { useT } from '../i18n'

// 「新建文件夹…」modal：位置（默认主目录，可浏览换）+ 文件夹名称 → 创建目录并注册为新工作区。
// 与 ApprovalModal 同构的 scrim/modal；Esc / 取消关闭（非阻塞，可关）。
export default function NewWorkspaceModal() {
  const t = useT()
  const show = useAppStore((s) => s.showNewWsModal)
  const closeNewFolder = useAppStore((s) => s.closeNewFolder)
  const setActiveWorkspace = useAppStore((s) => s.setActiveWorkspace)
  const [parent, setParent] = useState('')
  const [name, setName] = useState('')
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)

  // 每次打开：重置表单 + 默认父目录为主目录 + 聚焦名称输入
  useEffect(() => {
    if (!show) return
    setParent('')
    setName('')
    setErr(null)
    setBusy(false)
    void getTransport().homeDir().then(setParent)
    requestAnimationFrame(() => inputRef.current?.focus())
  }, [show])

  // Esc 关闭
  useEffect(() => {
    if (!show) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') closeNewFolder()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [show, closeNewFolder])

  if (!show) return null

  const browse = async () => {
    const p = await getTransport().pickWorkspace()
    if (p) setParent(p)
  }

  const submit = async () => {
    const clean = name.trim()
    if (!clean) {
      setErr(t('nwf.errName'))
      return
    }
    if (!parent) {
      setErr(t('nwf.errParent'))
      return
    }
    setBusy(true)
    setErr(null)
    const r = await getTransport().createWorkspaceFolder(parent, clean)
    if (!r.ok || !r.path) {
      setBusy(false)
      setErr(r.error || t('nwf.errFail'))
      return
    }
    setActiveWorkspace(r.path)
    closeNewFolder()
  }

  return (
    <div className="modal-hint open" role="dialog" aria-modal="true" aria-label={t('nwf.title')}>
      <div className="card nwf-card">
        <h3>{t('nwf.title')}</h3>
        <div className="row">
          <span className="param">{t('nwf.location')}</span>
          <input readOnly name="nwf-parent" value={parent} placeholder={t('nwf.parentPh')} aria-label={t('nwf.location')} spellCheck={false} />
          <button className="nwf-browse" onClick={() => void browse()} disabled={busy}>
            {t('nwf.browse')}
          </button>
        </div>
        <div className="row">
          <span className="param">{t('nwf.name')}</span>
          <input
            ref={inputRef}
            name="nwf-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => {
              // 输入法组合态（中文等用 Enter 选字/上屏）不得触发提交
              if (e.nativeEvent.isComposing || e.keyCode === 229) return
              if (e.key === 'Enter' && !busy && name.trim()) void submit()
            }}
            placeholder={t('nwf.namePh')}
            aria-label={t('nwf.name')}
            spellCheck={false}
          />
        </div>
        {err && (
          <p className="nwf-err" role="alert">
            {err}
          </p>
        )}
        <div className="acts">
          <button className="btn" onClick={closeNewFolder} disabled={busy}>
            {t('nwf.cancel')}
          </button>
          <button className="btn primary" onClick={() => void submit()} disabled={busy || !name.trim()}>
            {busy ? t('nwf.creating') : t('nwf.create')}
          </button>
        </div>
      </div>
    </div>
  )
}
