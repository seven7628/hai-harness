import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { getTransport } from '../transport'
import type { FsEntry } from '../transport/types'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import { ChevronDown, ChevronRight, FolderIcon, FolderOpenIcon, FileIcon, FolderPlus, Plus, SearchIcon, Back } from './icons'

// 工作区文件浏览（左栏「查看文件」模式）：目录懒展开 + 点击文件在右侧文件栏展示。
//
// 为什么用 fsList（Electron IPC）而不是新加 bridge 命令：文件浏览是**宿主侧**能力
//（读磁盘、工作区内外都能读），bridge 的 file_preview 才是「内容 + 截断/二进制判定」的口径；
// 二者分工已有先例（@ 引用面板 FileRefPanel 同样走 fsList）。这样左栏与右侧栏各用其长：
// 目录树只列名字，内容按需由 file_preview 取（1 MiB 截断、二进制标记）。
//
// 交互对齐参考实现（2026-09-20 用户口径）：顶部「返回任务」+ 搜索框，下面是工作区目录树；
// 单击文件 = 右侧展示（openFile：切文件栏 + 展开右栏 + 定位），单击目录 = 展开/收起。
function sortEntries(list: FsEntry[]): FsEntry[] {
  // 目录在前、同类按名（大小写不敏感）；保持稳定顺序，避免每次展开跳动。
  return [...list].sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
    return a.name.localeCompare(b.name, undefined, { sensitivity: 'base' })
  })
}

export default function WorkspaceFiles({ onBack }: { onBack: () => void }) {
  const t = useT()
  const workspace = useAppStore((s) => s.workspace)
  const openFile = useAppStore((s) => s.openFile)
  // 选中行 = 「文件面板当前定位的文件」。阶段 2 起内容按标签隔离（filePreviews），
  // 但「最后定位的文件」仍是 fileFocus 的语义（原 filePreview.path 与它同值：都由同一次
  // locateFile 写入）→ 换成 fileFocus.path，行为不变。
  const activePath = useAppStore((s) => s.fileFocus?.path)
  const projects = useAppStore((s) => s.projects)
  const setActiveWorkspace = useAppStore((s) => s.setActiveWorkspace)
  const addWorkspace = useAppStore((s) => s.addWorkspace)
  const openNewFolder = useAppStore((s) => s.openNewFolder)
  const [wsOpen, setWsOpen] = useState(false)
  const wsMenuRef = useRef<HTMLDivElement>(null)
  // dirPath → 条目 / 'loading' / 错误串。惰性：只有展开过的目录才在表里。
  const [dirs, setDirs] = useState<Record<string, FsEntry[] | 'loading' | { error: string }>>({})
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  const [q, setQ] = useState('')
  const [nonce, setNonce] = useState(0)
  const loadSeq = useRef(0)

  const loadDir = useCallback(async (dir: string) => {
    const seq = ++loadSeq.current
    setDirs((d) => ({ ...d, [dir]: 'loading' }))
    let entries: FsEntry[] | null = null
    let error = ''
    try {
      const r = await getTransport().fsList(dir)
      if (r.ok) entries = sortEntries(r.entries)
      else error = r.error ?? t('files.loadError')
    } catch (e) {
      error = (e as Error)?.message ?? String(e)
    }
    if (seq !== loadSeq.current && entries == null) return // 过期响应不覆盖新结果（仅错误分支短路）
    setDirs((d) => ({ ...d, [dir]: entries ?? { error: error || t('files.loadError') } }))
  }, [t])

  // 切换菜单：点外部 / Esc 关闭（与左栏「添加工作区」菜单同款交互）。
  useEffect(() => {
    if (!wsOpen) return
    const onDown = (e: MouseEvent) => {
      if (!wsMenuRef.current?.contains(e.target as Node)) setWsOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setWsOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    window.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      window.removeEventListener('keydown', onKey)
    }
  }, [wsOpen])

  // 工作区切换 / 刷新 → 重置缓存并展开根目录。
  useEffect(() => {
    setDirs({})
    setExpanded({})
    setQ('')
    if (workspace) {
      setExpanded({ [workspace]: true })
      void loadDir(workspace)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workspace, nonce])

  const toggleDir = (path: string) => {
    const next = !expanded[path]
    setExpanded((e) => ({ ...e, [path]: next }))
    if (next && dirs[path] == null) void loadDir(path) // 首次展开才拉取（懒加载）
  }

  const matches = useCallback(
    (name: string) => !q.trim() || name.toLowerCase().includes(q.trim().toLowerCase()),
    [q],
  )

  // 目录是否应显示：自身命中，或**已加载**的后代里有命中（搜索时保留通路）。
  const dirHasMatch = useCallback((path: string, seen: Set<string>): boolean => {
    if (seen.has(path)) return false
    seen.add(path)
    const children = dirs[path]
    if (!Array.isArray(children)) return false
    return children.some((e) => matches(e.name) || (e.isDir && dirHasMatch(e.path, seen)))
  }, [dirs, matches])

  const rows = useMemo(() => {
    const out: { entry: FsEntry; depth: number }[] = []
    const walk = (dir: string, depth: number) => {
      const children = dirs[dir]
      if (!Array.isArray(children)) return
      for (const e of children) {
        const show = matches(e.name) || (e.isDir && dirHasMatch(e.path, new Set()))
        if (!show) continue
        out.push({ entry: e, depth })
        if (e.isDir && expanded[e.path]) walk(e.path, depth + 1)
      }
    }
    if (workspace) walk(workspace, 0)
    return out
  }, [dirs, expanded, workspace, dirHasMatch, matches])

  const rootState = workspace ? dirs[workspace] : undefined
  const rootError = rootState && !Array.isArray(rootState) && rootState !== 'loading' ? rootState.error : ''

  return (
    <div className="lf" aria-label={t('files.aria')}>
      <div className="lf-head">
        <button className="lf-back" onClick={onBack} title={t('files.backHint')}>
          <Back size={13} /> {t('files.back')}
        </button>
        <span className="grow" />
        <button className="lf-refresh" onClick={() => setNonce((n) => n + 1)} title={t('files.refresh')} aria-label={t('files.refresh')}>
          ↻
        </button>
      </div>
      <div className="lf-search">
        <SearchIcon size={12} />
        <input
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder={t('files.search')}
          aria-label={t('files.search')}
          spellCheck={false}
        />
        {q && <button className="lf-clear" onClick={() => setQ('')} aria-label={t('files.clear')}>✕</button>}
      </div>
      {/* 工作区根目录切换：文件树以**当前活跃工作区**为根，这里换根（含添加工作区入口，
          与任务视图的「添加工作区」同一组 store 动作）——否则在文件模式里没法换目录。 */}
      <div className="lf-ws-wrap" ref={wsMenuRef}>
        <button
          className={`lf-ws ${wsOpen ? 'on' : ''}`}
          onClick={() => setWsOpen((v) => !v)}
          aria-expanded={wsOpen}
          aria-haspopup="menu"
          aria-label={t('files.switch')}
          title={workspace ? `${t('files.switchHint')}\n${workspace}` : t('files.noWorkspace')}
        >
          <FolderOpenIcon size={13} />
          <span className="lf-ws-name">{workspace ? (workspace.split('/').pop() || workspace) : t('files.noWorkspace')}</span>
          <ChevronDown size={11} className="lf-ws-caret" />
        </button>
        {wsOpen && (
          <div className="add-ws-menu lf-ws-menu" role="menu">
            <div className="lf-ws-menu-title">{t('files.switch')}</div>
            {projects.map((p) => {
              const name = p.path.split('/').pop() || p.path
              const cur = p.path === workspace
              return (
                <button
                  key={p.path}
                  role="menuitem"
                  className={cur ? 'cur' : ''}
                  title={p.path}
                  onClick={() => {
                    setWsOpen(false)
                    if (!cur) setActiveWorkspace(p.path) // 换根：树随 workspace 变化重载
                  }}
                >
                  <FolderIcon size={13} /> {name}
                  {cur && <span className="lf-ws-cur">{t('files.current')}</span>}
                </button>
              )
            })}
            <div className="lf-ws-sep" />
            <button role="menuitem" onClick={() => { setWsOpen(false); void addWorkspace() }}>
              <FolderPlus size={13} /> {t('left.selectFolder')}
            </button>
            <button role="menuitem" onClick={() => { setWsOpen(false); openNewFolder() }}>
              <Plus size={13} /> {t('left.newFolder')}
            </button>
          </div>
        )}
      </div>
      <div className="lf-tree" role="tree">
        {!workspace && <div className="lf-hint">{t('files.noWorkspace')}</div>}
        {workspace && rootState === 'loading' && <div className="lf-hint">{t('files.loading')}</div>}
        {rootError && <div className="lf-hint err">{rootError}</div>}
        {workspace && Array.isArray(rootState) && rows.length === 0 && (
          <div className="lf-hint">{q ? t('files.noMatch') : t('files.empty')}</div>
        )}
        {rows.map(({ entry, depth }) => {
          const isOpen = entry.isDir && expanded[entry.path]
          const state = dirs[entry.path]
          const err = state && !Array.isArray(state) && state !== 'loading' ? state.error : ''
          return (
            <div
              key={entry.path}
              className={`lf-row ${entry.isDir ? 'dir' : 'file'} ${!entry.isDir && activePath === entry.path ? 'active' : ''} ${entry.name.startsWith('.') ? 'dot' : ''}`}
              style={{ paddingLeft: 6 + depth * 14 }}
              role="treeitem"
              aria-expanded={entry.isDir ? !!isOpen : undefined}
              title={entry.path}
              onClick={() => (entry.isDir ? toggleDir(entry.path) : openFile(entry.path, undefined, 'source'))}
            >
              <span className="lf-chev">
                {entry.isDir ? (isOpen ? <ChevronDown size={11} /> : <ChevronRight size={11} />) : null}
              </span>
              <span className="lf-ic">{entry.isDir ? (isOpen ? <FolderOpenIcon size={13} /> : <FolderIcon size={13} />) : <FileIcon size={13} />}</span>
              <span className="lf-nm">{entry.name}</span>
              {state === 'loading' && <span className="lf-busy">…</span>}
              {err && <span className="lf-err" title={err}>!</span>}
            </div>
          )
        })}
      </div>
    </div>
  )
}
