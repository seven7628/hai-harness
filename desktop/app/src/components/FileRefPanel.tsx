import { useEffect, useRef, useState } from 'react'
import { getTransport } from '../transport'
import type { FsEntry, FsListResult } from '../transport/types'
import { useT } from '../i18n'
import { FolderIcon, FolderOpenIcon, FileIcon } from './icons'

// 简易文件浏览器（输入框 @ 引用）：浏览工作区 / 任意绝对路径，选择文件或目录。
// 文件单击 = 立即引用；目录单击 = 进入；「引用此文件夹」引用当前目录；
// 「选择外部文件/文件夹…」走系统原生对话框（工作区外引用）。
// 真实环境经 Electron 主进程 fs（工作区内外都能读，不受 bridge 沙箱约束）；浏览器走 mock 虚拟树。
function fmtSize(n?: number): string {
  if (n == null) return ''
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

// 父目录（'/' 的父为 null）；renderer 无 path 模块，纯字符串处理（POSIX 路径，桌面端 macOS）
function parentDir(dir: string): string | null {
  if (!dir || dir === '/') return null
  const i = dir.lastIndexOf('/')
  if (i <= 0) return '/'
  return dir.slice(0, i)
}

export default function FileRefPanel({
  workspace,
  filter,
  onPick,
  onClose,
  onTabComplete,
}: {
  workspace: string
  filter: string
  onPick: (path: string, isDir: boolean) => void
  onClose: () => void
  onTabComplete?: () => boolean
}) {
  const t = useT()
  const [dir, setDir] = useState<string>(workspace || '/')
  const [entries, setEntries] = useState<FsEntry[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [pathDraft, setPathDraft] = useState('')

  const loadSeq = useRef(0)
  const load = async (target: string) => {
    const seq = ++loadSeq.current
    setLoading(true)
    setError(null)
    let r: FsListResult
    try {
      r = await getTransport().fsList(target)
    } catch (e) {
      // IPC/transport 异常（旧版主进程/preload 未注册 fs:list 等）也要结束 loading 并提示，
      // 不能永远「加载中…」
      setLoading(false)
      setError(`${t('composer.ref.loadError')}: ${(e as Error)?.message ?? String(e)}`)
      return
    }
    setLoading(false)
    if (seq !== loadSeq.current) return
    if (r.ok) {
      setDir(r.path)
      setEntries(r.entries)
      setPathDraft(r.path)
    } else {
      setError(r.error ?? t('composer.ref.loadError'))
    }
  }

  // 挂载 + workspace 变化 → 从工作区根目录开始浏览。
  // 注意不 autofocus 路径输入框：焦点保持在 Composer 输入区，@ 后的键入用于过滤列表。
  useEffect(() => {
    void load(workspace || '/')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workspace])

  // @ 后输入目录路径时进入对应目录；例如 docs/ 在 workspace/docs 中继续过滤。
  useEffect(() => {
    const raw = filter.trim()
    if (!raw.includes('/')) return
    const parts = raw.split('/')
    const dirParts = parts.slice(0, -1)
    const target = `${workspace.replace(/\/+$/, '')}/${dirParts.filter(Boolean).join('/')}`
    if (!dirParts.length || target === dir) return
    void load(target)
    // 目录切换只依赖路径前缀，避免每次输入文件名重复请求。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filter, workspace])

  useEffect(() => {
    // 输入框在面板外部，必须在 capture 阶段拦截 Tab；仅监听 Tab，
    // 不能影响 @ 输入和其他键盘操作。
    const onTab = (e: KeyboardEvent) => {
      if (e.key !== 'Tab') return
      const first = shownRef.current[0]
      if (!first) return
      e.preventDefault()
      e.stopImmediatePropagation()
      onPick(first.path, first.isDir)
    }
    document.addEventListener('keydown', onTab, true)
    return () => document.removeEventListener('keydown', onTab, true)
  }, [onPick])

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === 'Tab') {
      const first = shown[0]
      if (first) {
        e.preventDefault()
        e.stopPropagation()
        onPick(first.path, first.isDir)
        return
      }
    }
    if (e.key === 'Escape') {
      e.stopPropagation()
      onClose()
    }
  }

  const up = () => {
    const p = parentDir(dir)
    if (p != null) void load(p)
  }

  const goPath = () => {
    const raw = pathDraft.trim()
    if (!raw) return
    // 已输入绝对路径原样跳转；相对路径（如 ./sub / sub）以工作区为基准补全
    const abs = raw.startsWith('/') || /^[A-Za-z]:[\\/]/.test(raw) ? raw : `${workspace.replace(/\/$/, '')}/${raw.replace(/^\.\//, '')}`
    void load(abs)
  }

  // 双击行 = 文件引用（单击已在行上处理；目录单击即进入）
  const openEntry = (e: FsEntry) => {
    if (e.isDir) void load(e.path)
    else onPick(e.path, false)
  }

  const shown = (() => {
    const raw = filter.trim()
    // 路径前缀用于定位目录，只有最后一段用于过滤当前目录内容。
    // 例如 @docs/readme → 在 workspace/docs 中匹配 readme。
    const leaf = raw.includes('/') ? (raw.split('/').pop() ?? '') : raw
    return leaf
      ? entries.filter((e) => e.name.toLowerCase().includes(leaf.toLowerCase()))
      : entries
  })()
  const parent = parentDir(dir)

  const shownRef = useRef<FsEntry[]>(shown)
  shownRef.current = shown
  // Tab 补全监听只注册一份（此前两个完全相同的 capture 监听重复注册；首个的
  // stopImmediatePropagation 保证实际单触发，2026-08-30 去重清理）。

  return (
    <div className="ref-menu" role="listbox" aria-label={t('composer.refAria')} onClick={(e) => e.stopPropagation()} onKeyDown={onKey}>
      <div className="ref-head">
        <input
          className="ref-path"
          value={pathDraft}
          placeholder={t('composer.ref.pathPh')}
          spellCheck={false}
          onChange={(e) => setPathDraft(e.target.value)}
          onKeyDown={(e) => {
            // 输入法组合态（中文等用 Enter 选字/上屏）不得触发跳转
            if (e.nativeEvent.isComposing || e.keyCode === 229) return
            if (e.key === 'Enter') {
              e.preventDefault()
              goPath()
            }
          }}
        />
        <button className="btn" onClick={() => void load(workspace || '/')}>{t('composer.ref.workspace')}</button>
        <button className="btn" onClick={() => void getTransport().pickFsFile().then((p) => p && onPick(p, false))}>{t('composer.ref.browseFile')}</button>
        <button className="btn" onClick={() => void getTransport().pickFsDir().then((p) => p && onPick(p, true))}>{t('composer.ref.browseDir')}</button>
      </div>
      <div className="ref-body">
        {loading && <div className="ref-hint">{t('composer.ref.loading')}</div>}
        {error && <div className="ref-hint err">{error}</div>}
        {!loading && !error && (
          <>
            {parent != null && (
              <div className="ref-row dir up" role="option" onClick={up} onMouseDown={(e) => e.preventDefault()} title={parent}>
                <span className="ref-ic"><FolderOpenIcon size={14} /></span>
                <span className="ref-nm">..</span>
              </div>
            )}
            {shown.length === 0 && <div className="ref-hint">{t('composer.ref.empty')}</div>}
            {shown.map((e) => (
              <div
                className={`ref-row ${e.isDir ? 'dir' : 'file'}`}
                role="option"
                key={e.path}
                title={e.path}
                onClick={() => openEntry(e)}
                onDoubleClick={(ev) => {
                  // 双击目录：已在单击进入；双击文件与单击一致（防 double-click 二次触发）
                  ev.preventDefault()
                }}
                onMouseDown={(ev) => ev.preventDefault()}
              >
                <span className="ref-ic">{e.isDir ? <FolderIcon size={14} /> : <FileIcon size={13} />}</span>
                <span className="ref-nm">{e.name}</span>
                {!e.isDir && <span className="ref-sz">{fmtSize(e.size)}</span>}
              </div>
            ))}
          </>
        )}
      </div>
      <div className="ref-foot">
        <span className="ref-cur" title={dir}>{dir}</span>
        <button className="btn primary" onClick={() => onPick(dir, true)}>{t('composer.ref.useDir')}</button>
      </div>
    </div>
  )
}