import { useEffect, useRef, useState } from 'react'
import { getTransport } from '../transport'
import type { FsEntry, FsListResult } from '../transport/types'
import { useT } from '../i18n'
import { FolderIcon, FolderOpenIcon, FileIcon } from './icons'
import './runtime/pop/pop.css'

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
  // 行模型：目录导航用 .pop-row.dir，动作（引用此目录/浏览…）也进同一份行列表 ——
  // 弹层里没有「按钮条」这种东西，按钮条会把它变回对话框。
  const [active, setActive] = useState(0)
  const rows: Array<
    | { kind: 'up'; label: string }
    | { kind: 'entry'; e: FsEntry }
    | { kind: 'act'; label: string; run: () => void }
  > = [
    ...(parent != null ? [{ kind: 'up' as const, label: '..' }] : []),
    ...shown.map((e) => ({ kind: 'entry' as const, e })),
    { kind: 'act' as const, label: t('composer.ref.useDir'), run: () => onPick(dir, true) },
    { kind: 'act' as const, label: t('composer.ref.browseFile'), run: () => void getTransport().pickFsFile().then((pp) => pp && onPick(pp, false)) },
    { kind: 'act' as const, label: t('composer.ref.browseDir'), run: () => void getTransport().pickFsDir().then((pp) => pp && onPick(pp, true)) },
  ]
  const runRow = (i: number) => {
    const r = rows[i]
    if (!r) return
    if (r.kind === 'up') up()
    else if (r.kind === 'entry') openEntry(r.e)
    else r.run()
  }

  const shownRef = useRef<FsEntry[]>(shown)
  shownRef.current = shown
  // Tab 补全监听只注册一份（此前两个完全相同的 capture 监听重复注册；首个的
  // stopImmediatePropagation 保证实际单触发，2026-08-30 去重清理）。


  return (
    <div
      className="pop"
      data-pop="src"
      role="listbox"
      aria-label={t('composer.refAria')}
      onClick={(e) => e.stopPropagation()}
      onKeyDown={(e) => {
        onKey(e as unknown as React.KeyboardEvent)
        if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
          e.preventDefault()
          setActive((i) => (i + (e.key === 'ArrowDown' ? 1 : -1) + rows.length) % rows.length)
        } else if (e.key === 'Enter') {
          e.preventDefault()
          runRow(active)
        }
      }}
    >
      <div className="pop-search">
        <FolderOpenIcon size={12} />
        <input
          value={pathDraft}
          placeholder={t('composer.ref.pathPh')}
          spellCheck={false}
          aria-label={t('composer.ref.pathPh')}
          onChange={(e) => setPathDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.nativeEvent.isComposing || e.keyCode === 229) return
            if (e.key === 'Enter') {
              e.preventDefault()
              goPath()
            }
          }}
        />
      </div>
      {/* 面包屑放在标签行：弹层里「你在哪」是位置信息，不是按钮 */}
      <div className="pop-label" title={dir}>
        <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{dir}</span>
        <b>{shown.length}</b>
      </div>
      {loading ? <div className="pop-row"><span className="k">{t('composer.ref.loading')}</span></div> : null}
      {error ? <div className="pop-row"><span className="k">{error}</span></div> : null}
      {!loading && !error && shown.length === 0 ? (
        <div className="pop-row"><span className="k">{t('composer.ref.empty')}</span></div>
      ) : null}
      {!loading && !error
        ? rows.map((r, i) => {
            const on = i === active
            const cls = `pop-row${r.kind === 'entry' && r.e.isDir ? ' dir' : ''}${r.kind === 'up' ? ' dir' : ''}${on ? ' is-active' : ''}`
            return (
              <button
                type="button"
                key={r.kind === 'entry' ? r.e.path : `act:${i}`}
                className={cls}
                role="option"
                aria-selected={on}
                title={r.kind === 'entry' ? r.e.path : undefined}
                onMouseEnter={() => setActive(i)}
                onClick={() => runRow(i)}
                onMouseDown={(ev) => ev.preventDefault()}
              >
                {r.kind === 'up' ? <FolderOpenIcon size={12} /> : null}
                {r.kind === 'entry' ? (r.e.isDir ? <FolderIcon size={12} /> : <FileIcon size={12} />) : null}
                <span className="k">
                  {r.kind === 'entry' ? r.e.name : r.label}
                </span>
                {r.kind === 'entry' && !r.e.isDir ? <span className="m">{fmtSize(r.e.size)}</span> : null}
              </button>
            )
          })
        : null}
      <div className="pop-foot">{t('composer.ref.popFoot')}</div>
    </div>
  )
}