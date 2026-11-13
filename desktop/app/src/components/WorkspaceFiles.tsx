import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { getTransport } from '../transport'
import type { FsEntry, FsOpReply } from '../transport/types'
import { fileKeyOf, useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import { copyText } from '../lib/clipboard'
import { isEditorEnabled, isTextLikePath } from '../lib/editorState'
import ConfirmModal from './ConfirmModal'
import {
  Back,
  ChevronDown,
  ChevronRight,
  CopyIcon,
  EyeIcon,
  FileIcon,
  FilePlusIcon,
  FolderIcon,
  FolderOpenIcon,
  FolderPlus,
  PasteIcon,
  PencilIcon,
  Plus,
  ScissorsIcon,
  SearchIcon,
  TrashIcon,
} from './icons'

// 工作区文件浏览（左栏「查看文件」模式）：目录懒展开 + 点击文件在右侧文件栏展示。
//
// 为什么用 fsList（Electron IPC）而不是新加 bridge 命令：文件浏览是**宿主侧**能力
//（读磁盘、工作区内外都能读），bridge 的 file_preview 才是「内容 + 截断/二进制判定」的口径；
// 二者分工已有先例（@ 引用面板 FileRefPanel 同样走 fsList）。这样左栏与右侧栏各用其长：
// 目录树只列名字，内容按需由 file_preview 取（1 MiB 截断、二进制标记）。
//
// 2026-10 起这一栏也是**可写的**（对齐 Mac 访达的文件夹管理）：
//   · 重命名（双击 / F2 / 右键）、新建文件夹/文件、复制副本、删到废纸篓（⌘⌫，弹确认）
//   · 拖动移动：拖到目录行（悬停 600ms 自动展开）或拖到空白处（= 工作区根目录）
//   · 多选批量：⌘ 单击增减、⇧ 单击选区间、⌘A 全选；整组拖动 / 删除 / 拷贝剪切
//   · 从访达拖入：把外部文件拷进目标目录（来源路径由 preload getPathForFile 还原）
//   · 剪贴板：⌘C / ⌘X / ⌘V（与拖动同一套 fs 操作，见 electron/fs-ops.ts）
//   · 键盘导航：↑↓ 选择（⇧ 扩选）、←→ 收起/展开、Enter 打开、右键菜单也支持 ↑↓/Enter
//
// 四条交互口径值得记住（改之前先读这里）：
//   ① 写操作**不走乐观更新**：等主进程回话后重载受影响的目录。理由是磁盘是唯一事实源 ——
//      乐观地先搬再回滚，会在失败时闪一下「东西搬过去了又弹回来」。重命名/新建是毫秒级，
//      复制大目录会明显等一会儿：等待期用行级 pending（淡出）+ 目录行 pending 表示「在干活」，
//      而不是先假装成功。
//   ② 同名一律**拒绝**并 toast（产品口径，不弹覆盖询问、不自动改名）。唯一的例外是「复制副本」：
//      那个名字是我们生成的（a copy、a copy 2），冲突时自动递增，否则用户没法连按两次 ⌘D。
//   ③ 拖动到**文件**行 = 拖进它所在的目录（访达口径）；拖到自身/子孙/同一个父目录都不算
//      有效目标（无高亮、光标是禁止符），避免「拖了半天什么都没发生」的困惑。
//   ④ 批量操作**逐项**执行、**逐项**报告：拖 5 个进去其中 1 个同名，另外 4 个必须照样到位，
//      然后一条 toast 说清「成功 4 / 失败 1（原因）」。整批回滚在文件系统上做不到，也不该假装。
function sortEntries(list: FsEntry[]): FsEntry[] {
  // 目录在前、同类按名（大小写不敏感）；保持稳定顺序，避免每次展开跳动。
  return [...list].sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
    return a.name.localeCompare(b.name, undefined, { sensitivity: 'base' })
  })
}

// 绝对路径的父目录（POSIX；与仓库其它地方一致，路径分隔符按 '/' 处理）。
function parentOf(p: string): string {
  const i = p.lastIndexOf('/')
  return i > 0 ? p.slice(0, i) : '/'
}

// 拖到目录行后多久自动展开：600ms 是「停留一下」与「路过」的分界（访达弹簧文件夹的体感）。
const SPRING_OPEN_MS = 600

type RowEntry = { kind: 'entry'; entry: FsEntry; depth: number; parent: string }
type RowCreate = { kind: 'new'; depth: number; parent: string }
type Row = RowEntry | RowCreate

type DirState = FsEntry[] | 'loading' | { error: string }

export default function WorkspaceFiles({ onBack }: { onBack: () => void }) {
  const t = useT()
  const workspace = useAppStore((s) => s.workspace)
  const openFile = useAppStore((s) => s.openFile)
  // PR-05b 入口②：文件树的「编辑」。开关关掉时这一项**不渲染**（回滚 = 界面上没这条）。
  const editorOn = useAppStore((s) => isEditorEnabled(s.settings))
  const openFileForEdit = useAppStore((s) => s.openFileForEdit)
  const openPreviewTab = useAppStore((s) => s.openPreviewTab)
  // 选中行 = 「文件面板当前定位的文件」。阶段 2 起内容按标签隔离（filePreviews），
  // 但「最后定位的文件」仍是 fileFocus 的语义（原 filePreview.path 与它同值：都由同一次
  // locateFile 写入）→ 换成 fileFocus.path，行为不变。
  const activePath = useAppStore((s) => s.fileFocus?.path)
  // 高亮判据必须归一化：fileFocus.path 有「工作区内绝对」与「工作区相对」两种写法
  //（文件树点开传绝对，对话里点开传相对），而条目 path 恒为绝对 —— 直接 === 会在
  // 文件被移动/重命名后（store 会改写这条路径）让「正在展示」的高亮凭空消失。
  const activeKey = activePath ? fileKeyOf(activePath, workspace) : ''
  const projects = useAppStore((s) => s.projects)
  const setActiveWorkspace = useAppStore((s) => s.setActiveWorkspace)
  const addWorkspace = useAppStore((s) => s.addWorkspace)
  const openNewFolder = useAppStore((s) => s.openNewFolder)
  const showToast = useAppStore((s) => s.showToast)
  // 写操作改的是磁盘 → 右栏（内容缓存/file 标签/选中锚）必须跟着收敛，见 store 的
  // filePathMoved / filePathRemoved。
  const filePathMoved = useAppStore((s) => s.filePathMoved)
  const filePathRemoved = useAppStore((s) => s.filePathRemoved)
  const [wsOpen, setWsOpen] = useState(false)
  const wsMenuRef = useRef<HTMLDivElement>(null)
  // dirPath → 条目 / 'loading' / 错误串。惰性：只有展开过的目录才在表里。
  const [dirs, setDirs] = useState<Record<string, DirState>>({})
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  const [q, setQ] = useState('')
  const [nonce, setNonce] = useState(0)
  const loadSeq = useRef(0)
  // —— 写入态（2026-10 文件夹管理）——
  // 选中集合（键盘/剪贴板/批量操作的作用对象）。用数组而不是 Set 是为了稳定顺序：
  // 批量操作的执行顺序要跟**视觉顺序**一致（用户看到的从上到下），否则失败报告会跳来跳去。
  const [sel, setSel] = useState<string[]>([])
  // ⇧ 区间的锚点（访达口径：连续 ⇧ 点击始终从同一个锚点扩张，而不是从上一次点击扩张）
  const [anchor, setAnchor] = useState<string | null>(null)
  // 「当前行」= 用户最后触碰的那一行（⌘/⇧/↑↓ 都会动它）。它和 anchor 是两件事：
  // anchor 决定 ⇧ 区间从哪开始（保持不变），cursor 决定描边画在谁身上（跟着手走）。
  const [cursor, setCursor] = useState<string | null>(null)
  const selSet = useMemo(() => new Set(sel), [sel])
  const [renaming, setRenaming] = useState<{ path: string; isDir: boolean; name: string; orig: string } | null>(null)
  const [creating, setCreating] = useState<{ dir: string; kind: 'dir' | 'file'; name: string } | null>(null)
  const [menu, setMenu] = useState<{ x: number; y: number; entry: FsEntry | null; parent: string } | null>(null)
  const [drag, setDrag] = useState<{ entries: FsEntry[] } | null>(null)
  const [dropTarget, setDropTarget] = useState<string | null>(null) // 悬停中的目录行
  const [dropExternal, setDropExternal] = useState(false) // 悬停中的是「从访达拖进来的文件」
  const [hoverRoot, setHoverRoot] = useState(false) // 悬停在空白区（= 工作区根）
  const [pending, setPending] = useState<Record<string, boolean>>({}) // 操作进行中的条目（淡出）
  const [confirm, setConfirm] = useState<{ entries: FsEntry[] } | null>(null) // 删到废纸篓的确认
  // 应用内剪贴板（拷贝/剪切，支持多选）。不用系统剪贴板：我们要的是「文件对象」而不是文本，
  // 而 Electron 的 file 剪贴板格式要走 startDrag 那套原生约定（本轮不做拖出，见说明）。
  const [clip, setClip] = useState<{ mode: 'copy' | 'cut'; entries: FsEntry[] } | null>(null)

  const treeRef = useRef<HTMLDivElement>(null)
  const rowRefs = useRef<Map<string, HTMLDivElement>>(new Map())
  const inputRef = useRef<HTMLInputElement>(null)
  // 一次编辑只提交一次：Enter 提交后组件卸载，某些浏览器仍会补一个 blur。
  const editSettled = useRef(false)
  // 弹簧展开的待定计时器（同一时刻只有一个目标）
  const springRef = useRef<{ path: string; timer: number } | null>(null)

  const loadDir = useCallback(async (dir: string) => {
    const seq = ++loadSeq.current
    // 只对**还没加载过**的目录显示 loading：已经有内容的目录在重载时保留旧内容直到新结果
    // 到达，否则每次刷新/写操作后整棵树都会先塌成「加载中…」再长回来（视觉抖动）。
    setDirs((d) => (Array.isArray(d[dir]) ? d : { ...d, [dir]: 'loading' }))
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

  // 右键菜单：点外部 / Esc 关闭；↑↓/Home/End 在菜单项之间移动焦点（访达口径），
  // Enter/Space 走按钮自身的默认行为（这里不拦）。关闭时把焦点还给树，
  // 否则用户按完 Esc 再按 ↑↓ 会发现「树没反应」（焦点掉在 body 上）。
  useEffect(() => {
    if (!menu) return
    const items = () => Array.from(document.querySelectorAll<HTMLButtonElement>('.lf-menu .ctx-item:not(:disabled)'))
    const onDown = (e: MouseEvent) => {
      if (!(e.target as HTMLElement | null)?.closest?.('.ctx-menu')) setMenu(null)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setMenu(null)
        treeRef.current?.focus()
        return
      }
      if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(e.key)) return
      const list = items()
      if (!list.length) return
      e.preventDefault()
      const idx = list.findIndex((el) => el === document.activeElement)
      if (e.key === 'ArrowDown') list[(idx + 1) % list.length].focus()
      else if (e.key === 'ArrowUp') list[idx <= 0 ? list.length - 1 : idx - 1].focus()
      else if (e.key === 'Home') list[0].focus()
      else list[list.length - 1].focus()
    }
    document.addEventListener('mousedown', onDown)
    window.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      window.removeEventListener('keydown', onKey)
    }
  }, [menu])

  // 工作区切换 / 刷新 → 重置缓存并展开根目录。
  // 写入态（选中/编辑/剪贴板）也一并清空：剪贴板里存的是**别的树**里的绝对路径，
  // 换根后粘贴会撞上「只能操作当前工作区内的文件」，不如直接忘掉。
  useEffect(() => {
    setDirs({})
    setExpanded({})
    setQ('')
    setSel([])
    setAnchor(null)
    setCursor(null)
    setRenaming(null)
    setCreating(null)
    setMenu(null)
    setDrag(null)
    setClip(null)
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

  const rows = useMemo<Row[]>(() => {
    const out: Row[] = []
    const walk = (dir: string, depth: number) => {
      // 新建输入框排在目标目录的**第一个子项**位置（访达口径：新东西出现在文件夹最上面，
      // 而不是弹一个居中对话框抢焦点）。
      if (creating?.dir === dir) out.push({ kind: 'new', depth, parent: dir })
      const children = dirs[dir]
      if (!Array.isArray(children)) return
      for (const e of children) {
        const show = matches(e.name) || (e.isDir && dirHasMatch(e.path, new Set()))
        if (!show) continue
        out.push({ kind: 'entry', entry: e, depth, parent: dir })
        if (e.isDir && expanded[e.path]) walk(e.path, depth + 1)
      }
    }
    if (workspace) walk(workspace, 0)
    return out
  }, [dirs, expanded, workspace, dirHasMatch, matches, creating])

  const entryRows = useMemo(() => rows.filter((r): r is RowEntry => r.kind === 'entry'), [rows])
  // 选中的条目（按**视觉顺序**）：批量操作的执行顺序与失败报告顺序都依赖它。
  // 只认「当前可见」的行：折叠目录里的旧选中不该被批量删除（用户看不见的东西不能被删）。
  const selectedEntries = useMemo(
    () => entryRows.filter((r) => selSet.has(r.entry.path)).map((r) => r.entry),
    [entryRows, selSet],
  )

  const rootState = workspace ? dirs[workspace] : undefined
  const rootError = rootState && !Array.isArray(rootState) && rootState !== 'loading' ? rootState.error : ''

  // —— 选中 ——

  const selectOnly = (path: string) => {
    setSel([path])
    setAnchor(path)
    setCursor(path)
  }
  const toggleSel = (path: string) => {
    setSel((s) => (s.includes(path) ? s.filter((x) => x !== path) : [...s, path]))
    setAnchor(path)
    setCursor(path)
  }
  // ⇧ 区间：以 anchor 为起点、当前点击为终点，取**可见行**的连续区间。
  const selectRange = (path: string) => {
    const list = entryRows.map((r) => r.entry.path)
    const a = anchor ? list.indexOf(anchor) : -1
    const b = list.indexOf(path)
    if (a < 0 || b < 0) {
      selectOnly(path)
      return
    }
    const [lo, hi] = a <= b ? [a, b] : [b, a]
    setSel(list.slice(lo, hi + 1)) // anchor 不动：连续 ⇧ 点击始终从同一锚点扩张
    setCursor(path) // 但描边跟着点击走
  }

  // —— 目录缓存的一致性维护 ——

  // rekeyTree：移动/重命名目录后，把展开态与缓存键搬到新路径。
  // 缓存内容**整体作废**（条目里的 path 已经指向旧位置），只保留「哪些目录是展开的」——
  // 用户看到的是「东西搬过去了，原来展开的层级还是展开的」。
  const rekeyTree = (from: string, to: string) => {
    const under = (k: string) => k === from || k.startsWith(from + '/')
    setExpanded((e) => {
      const out: Record<string, boolean> = {}
      for (const [k, v] of Object.entries(e)) {
        if (under(k)) out[to + k.slice(from.length)] = v
        else out[k] = v
      }
      return out
    })
    setDirs((d) => {
      const out: Record<string, DirState> = {}
      for (const [k, v] of Object.entries(d)) {
        if (!under(k)) out[k] = v
      }
      return out
    })
  }

  // forgetTree：删除后把整棵子树的展开态与缓存清掉。
  const forgetTree = (path: string, isDir: boolean) => {
    const under = (k: string) => k === path || (isDir && k.startsWith(path + '/'))
    setExpanded((e) => Object.fromEntries(Object.entries(e).filter(([k]) => !under(k))))
    setDirs((d) => Object.fromEntries(Object.entries(d).filter(([k]) => !under(k))))
  }

  const reload = useCallback(async (...list: string[]) => {
    const uniq = [...new Set(list.filter(Boolean))]
    await Promise.all(uniq.map((d) => loadDir(d)))
  }, [loadDir])

  const setBusy = (path: string, on: boolean) => {
    setPending((p) => {
      if (on) return { ...p, [path]: true }
      const n = { ...p }
      delete n[path]
      return n
    })
  }

  // opFail 失败反馈：主进程已给出可读原因（含 code 分流），这里只负责显示。
  const opFail = (r: FsOpReply) => showToast(r.error || t('files.opFail'), 'error')

  // canDropInto：目标是「一个和现在不同的目录」，且不是被拖目录自己/它的子孙。
  // 拖到自己原来的父目录也算无效 —— 否则一次拖动会走一遍 IPC 却什么都不变。
  const canDropInto = (entry: FsEntry, destDir: string): boolean => {
    if (!destDir || destDir === entry.path) return false
    if (parentOf(entry.path) === destDir) return false
    if (entry.isDir && destDir.startsWith(entry.path + '/')) return false
    return true
  }

  // 批量失败汇总：成功 N 项 + 失败明细（最多列 2 条，其余折成计数）。
  // 为什么不做整批回滚：文件系统上没有事务，回滚本身就会产生第二批失败（见文件头 ④）。
  const batchToast = (okCount: number, failed: { name: string; error: string }[], okMsg: string) => {
    if (!failed.length) {
      if (okCount) showToast(okMsg, 'ok')
      return
    }
    const shown = failed.slice(0, 2).map((f) => `${f.name || '—'}（${f.error}）`).join('、')
    const detail = failed.length > 2 ? `${shown} 等 ${failed.length} 项` : shown
    if (okCount === 0) {
      showToast(detail, 'error')
      return
    }
    showToast(
      t('files.batchPartial')
        .replace('{ok}', String(okCount))
        .replace('{fail}', String(failed.length))
        .replace('{detail}', detail),
      'error',
    )
  }

  // —— 操作：移动 / 重命名 / 新建 / 复制副本 / 拷贝粘贴 / 删除 / 导入 ——

  const moveMany = async (entries: FsEntry[], destDir: string) => {
    if (!workspace) return
    const targets = entries.filter((e) => canDropInto(e, destDir))
    if (!targets.length) return
    const dirsToReload = [destDir]
    const moved: string[] = []
    const failed: { name: string; error: string }[] = []
    for (const e of targets) {
      const wasExpanded = !!expanded[e.path]
      setBusy(e.path, true)
      let r: FsOpReply
      try {
        r = await getTransport().fsMove({ workspace, src: e.path, destDir })
      } catch (err) {
        setBusy(e.path, false)
        failed.push({ name: e.name, error: (err as Error).message })
        continue
      }
      setBusy(e.path, false)
      if (!r.ok || !r.path) {
        failed.push({ name: e.name, error: r.error || t('files.opFail') })
        continue
      }
      if (e.isDir) rekeyTree(e.path, r.path)
      filePathMoved(e.path, r.path, { isDir: e.isDir })
      moved.push(r.path)
      dirsToReload.push(parentOf(e.path))
      // 搬运的是目录且它原本是展开的 → 新路径也要重载（子项 path 全变了）
      if (e.isDir && wasExpanded) dirsToReload.push(r.path)
    }
    // 目标目录展开（折叠着的话用户看不到结果）
    setExpanded((x) => ({ ...x, [destDir]: true }))
    setSel(moved)
    setAnchor(moved[moved.length - 1] ?? null)
    setCursor(moved[moved.length - 1] ?? null)
    await reload(...dirsToReload)
    batchToast(
      moved.length,
      failed,
      moved.length === 1
        ? t('files.movedTo').replace('{name}', targets[0].name)
        : t('files.movedMany').replace('{n}', String(moved.length)),
    )
  }

  const startRename = (entry: FsEntry) => {
    setMenu(null)
    editSettled.current = false
    setRenaming({ path: entry.path, isDir: entry.isDir, name: entry.name, orig: entry.name })
  }

  // 文件默认只选中扩展名之前的部分（访达口径：改 main.go 时不想连带改掉 .go）。
  useEffect(() => {
    if (!renaming) return
    const el = inputRef.current
    if (!el) return
    el.focus()
    const dot = renaming.isDir ? -1 : renaming.name.lastIndexOf('.')
    const end = dot > 0 ? dot : renaming.name.length
    el.setSelectionRange(0, end)
  }, [renaming])

  const commitRename = async () => {
    const r = renaming
    if (!r || editSettled.current) return
    editSettled.current = true
    setRenaming(null)
    const name = r.name.trim()
    if (!workspace || !name || name === r.orig) return
    setBusy(r.path, true)
    let res: FsOpReply
    try {
      res = await getTransport().fsRename({ workspace, path: r.path, name })
    } catch (e) {
      setBusy(r.path, false)
      showToast((e as Error).message, 'error')
      return
    }
    setBusy(r.path, false)
    if (!res.ok || !res.path) return opFail(res)
    if (r.isDir) rekeyTree(r.path, res.path)
    filePathMoved(r.path, res.path, { isDir: r.isDir })
    selectOnly(res.path)
    await reload(parentOf(r.path), r.isDir ? res.path : '')
  }

  const startCreate = (dir: string, kind: 'dir' | 'file') => {
    if (!workspace) return
    setMenu(null)
    editSettled.current = false
    setCreating({ dir, kind, name: kind === 'dir' ? t('files.newFolderName') : t('files.newFileName') })
    setExpanded((e) => ({ ...e, [dir]: true }))
    if (dirs[dir] == null) void loadDir(dir)
  }

  useEffect(() => {
    if (!creating) return
    const el = inputRef.current
    if (!el) return
    el.focus()
    el.select()
  }, [creating])

  const commitCreate = async () => {
    const c = creating
    if (!c || editSettled.current) return
    editSettled.current = true
    setCreating(null)
    const name = c.name.trim()
    if (!workspace || !name) {
      // 名字留空 = 取消（不建无名条目，也不弹错误：用户只是改主意了）
      void reload(c.dir)
      return
    }
    const tr = getTransport()
    let res: FsOpReply
    try {
      res = c.kind === 'dir'
        ? await tr.fsMkdir({ workspace, dir: c.dir, name })
        : await tr.fsCreateFile({ workspace, dir: c.dir, name })
    } catch (e) {
      showToast((e as Error).message, 'error')
      return
    }
    if (!res.ok || !res.path) {
      opFail(res)
      void reload(c.dir)
      return
    }
    selectOnly(res.path)
    await reload(c.dir)
  }

  const doDuplicate = async (entries: FsEntry[]) => {
    if (!workspace || !entries.length) return
    setMenu(null)
    const made: string[] = []
    const failed: { name: string; error: string }[] = []
    for (const e of entries) {
      setBusy(e.path, true)
      let r: FsOpReply
      try {
        r = await getTransport().fsDuplicate({ workspace, path: e.path })
      } catch (err) {
        setBusy(e.path, false)
        failed.push({ name: e.name, error: (err as Error).message })
        continue
      }
      setBusy(e.path, false)
      if (!r.ok || !r.path) {
        failed.push({ name: e.name, error: r.error || t('files.opFail') })
        continue
      }
      made.push(r.path)
    }
    setSel(made)
    setAnchor(made[made.length - 1] ?? null)
    setCursor(made[made.length - 1] ?? null)
    await reload(...entries.map((e) => parentOf(e.path)))
    batchToast(
      made.length,
      failed,
      made.length === 1
        ? t('files.duplicatedTo').replace('{name}', made[0].slice(made[0].lastIndexOf('/') + 1))
        : t('files.duplicatedMany').replace('{n}', String(made.length)),
    )
  }

  const doCopy = (entries: FsEntry[], mode: 'copy' | 'cut') => {
    if (!entries.length) return
    setMenu(null)
    setClip({ mode, entries })
    const key = entries.length === 1
      ? (mode === 'copy' ? 'files.copiedHint' : 'files.cutHint')
      : (mode === 'copy' ? 'files.copiedHintMany' : 'files.cutHintMany')
    showToast(t(key).replace('{name}', entries[0].name).replace('{n}', String(entries.length)))
  }

  const doPaste = async (dir: string) => {
    if (!workspace || !clip) return
    setMenu(null)
    const { mode, entries } = clip
    const tr = getTransport()
    const made: string[] = []
    const failed: { name: string; error: string }[] = []
    const stuck: FsEntry[] = [] // 没搬成的（剪切时剪贴板只留这些）
    const dirsToReload = [dir]
    setPending((p) => ({ ...p, [dir]: true }))
    for (const e of entries) {
      let r: FsOpReply
      try {
        r = mode === 'copy'
          ? await tr.fsCopy({ workspace, src: e.path, destDir: dir })
          : await tr.fsMove({ workspace, src: e.path, destDir: dir })
      } catch (err) {
        failed.push({ name: e.name, error: (err as Error).message })
        stuck.push(e)
        continue
      }
      if (!r.ok || !r.path) {
        failed.push({ name: e.name, error: r.error || t('files.opFail') })
        stuck.push(e)
        continue
      }
      made.push(r.path)
      if (mode === 'cut') {
        if (e.isDir) rekeyTree(e.path, r.path)
        filePathMoved(e.path, r.path, { isDir: e.isDir })
        dirsToReload.push(parentOf(e.path))
      }
    }
    setPending((p) => {
      const n = { ...p }
      delete n[dir]
      return n
    })
    // 剪切 = 移动：源目录也要重载，搬成功的从剪贴板里摘掉（全成功即清空，与访达一致）；
    // 拷贝则保留剪贴板（同一批可以往多个目录各粘一份）。
    if (mode === 'cut') setClip(stuck.length ? { mode: 'cut', entries: stuck } : null)
    setExpanded((e) => ({ ...e, [dir]: true }))
    setSel(made)
    setAnchor(made[made.length - 1] ?? null)
    setCursor(made[made.length - 1] ?? null)
    await reload(...dirsToReload)
    batchToast(
      made.length,
      failed,
      made.length === 1
        ? t('files.pasted').replace('{name}', made[0].slice(made[0].lastIndexOf('/') + 1))
        : t('files.pastedMany').replace('{n}', String(made.length)),
    )
  }

  const doDelete = async (entries: FsEntry[]) => {
    setConfirm(null)
    setMenu(null)
    if (!workspace || !entries.length) return
    const gone: string[] = []
    const failed: { name: string; error: string }[] = []
    const dirsToReload: string[] = []
    for (const e of entries) {
      setBusy(e.path, true)
      let r: FsOpReply
      try {
        r = await getTransport().fsTrash({ workspace, path: e.path })
      } catch (err) {
        setBusy(e.path, false)
        failed.push({ name: e.name, error: (err as Error).message })
        continue
      }
      setBusy(e.path, false)
      if (!r.ok) {
        failed.push({ name: e.name, error: r.error || t('files.opFail') })
        continue
      }
      forgetTree(e.path, e.isDir)
      filePathRemoved(e.path, { isDir: e.isDir })
      gone.push(e.path)
      dirsToReload.push(parentOf(e.path))
    }
    setSel((s) => s.filter((p) => !gone.includes(p)))
    setCursor((c) => (c && gone.includes(c) ? null : c))
    if (clip) {
      const left = clip.entries.filter((e) => !gone.includes(e.path))
      setClip(left.length ? { mode: clip.mode, entries: left } : null)
    }
    await reload(...dirsToReload)
    batchToast(
      gone.length,
      failed,
      entries.length === 1
        ? t('files.trashed').replace('{name}', entries[0].name)
        : t('files.trashedMany').replace('{n}', String(gone.length)),
    )
  }

  // 从访达拖入：路径由 preload 的 getPathForFile 还原（Electron 32+ 的官方方式）。
  const externalPaths = (e: React.DragEvent): string[] => {
    const files = Array.from(e.dataTransfer?.files ?? [])
    const out: string[] = []
    for (const f of files) {
      const p = window.desktop?.getPathForFile?.(f)
      // 浏览器 mock 没有 preload：退化成与 mock transport 同口径的假路径，
      // 这样左栏的导入交互在浏览器里也能完整走通（真实磁盘路径只存在于桌面端）。
      if (p) out.push(p)
      else if (f.name) out.push(`/mock/${f.name}`)
    }
    return out
  }
  const hasExternalFiles = (e: React.DragEvent): boolean =>
    Array.from(e.dataTransfer?.types ?? []).includes('Files')

  const doImport = async (dir: string, sources: string[]) => {
    if (!workspace || !sources.length) return
    setPending((p) => ({ ...p, [dir]: true }))
    let r: { ok: boolean; imported: string[]; failed: { name: string; error: string }[] }
    try {
      r = await getTransport().fsImport({ workspace, destDir: dir, sources })
    } catch (e) {
      setPending((p) => {
        const n = { ...p }
        delete n[dir]
        return n
      })
      showToast((e as Error).message, 'error')
      return
    }
    setPending((p) => {
      const n = { ...p }
      delete n[dir]
      return n
    })
    setExpanded((e) => ({ ...e, [dir]: true }))
    setSel(r.imported)
    setAnchor(r.imported[r.imported.length - 1] ?? null)
    setCursor(r.imported[r.imported.length - 1] ?? null)
    await reload(dir)
    batchToast(
      r.imported.length,
      r.failed,
      r.imported.length === 1
        ? t('files.importedOne').replace('{name}', r.imported[0].slice(r.imported[0].lastIndexOf('/') + 1))
        : t('files.importedMany').replace('{n}', String(r.imported.length)),
    )
  }

  // —— 拖动 ——

  const clearSpring = () => {
    if (springRef.current) {
      window.clearTimeout(springRef.current.timer)
      springRef.current = null
    }
  }
  useEffect(() => clearSpring, [])

  const endDrag = () => {
    setDrag(null)
    setDropTarget(null)
    setDropExternal(false)
    setHoverRoot(false)
    clearSpring()
  }

  const onRowDragOver = (e: React.DragEvent, row: RowEntry) => {
    // 先拦住冒泡：否则「无效目标」会落到树容器的空白区处理器上，把文件搬去工作区根目录
    //（第一版就是这么错的，e2e 的「拖到自己的父目录不该有变化」当场抓到）。
    e.stopPropagation()
    // 从访达拖进来的文件：目标行接受 = 往这个目录里导入
    if (hasExternalFiles(e)) {
      e.preventDefault()
      e.dataTransfer.dropEffect = 'copy'
      setDropTarget(row.entry.path)
      setDropExternal(true)
      setHoverRoot(false)
      return
    }
    if (!drag) return
    // 拖到文件行 = 拖进它所在的目录（访达口径）；拖到目录行 = 拖进该目录。
    const destDir = row.entry.isDir ? row.entry.path : row.parent
    const valid = drag.entries.filter((x) => canDropInto(x, destDir))
    if (!valid.length) return // 不 preventDefault → 光标显示禁止符
    e.preventDefault()
    e.dataTransfer.dropEffect = 'move'
    setDropTarget(row.entry.path)
    setDropExternal(false)
    setHoverRoot(false)
    // 弹簧展开：悬停在**折叠的目录行**上 600ms → 展开，让用户能一次拖进深层目录。
    if (row.entry.isDir && !expanded[row.entry.path] && springRef.current?.path !== row.entry.path) {
      clearSpring()
      const timer = window.setTimeout(() => {
        springRef.current = null
        setExpanded((x) => ({ ...x, [row.entry.path]: true }))
        if (dirs[row.entry.path] == null) void loadDir(row.entry.path)
      }, SPRING_OPEN_MS)
      springRef.current = { path: row.entry.path, timer }
    }
  }

  const onRowDrop = (e: React.DragEvent, row: RowEntry) => {
    // 同 onRowDragOver：无效目标也必须拦住冒泡，否则会「意外地」落到根目录处理器上。
    e.stopPropagation()
    const destDir = row.entry.isDir ? row.entry.path : row.parent
    if (hasExternalFiles(e)) {
      e.preventDefault()
      const paths = externalPaths(e)
      endDrag()
      void doImport(destDir, paths)
      return
    }
    if (!drag) return
    if (!drag.entries.some((x) => canDropInto(x, destDir))) return
    e.preventDefault()
    const moved = drag.entries
    endDrag()
    void moveMany(moved, destDir)
  }

  // 空白区 = 工作区根目录。这里只在「没有落在任何行上」时生效（行内 dragOver 已 stopPropagation）。
  const onRootDragOver = (e: React.DragEvent) => {
    if (!workspace) return
    if (hasExternalFiles(e)) {
      e.preventDefault()
      e.dataTransfer.dropEffect = 'copy'
      setHoverRoot(true)
      setDropTarget(null)
      setDropExternal(true)
      clearSpring()
      return
    }
    if (!drag) return
    if (!drag.entries.some((x) => canDropInto(x, workspace))) return
    e.preventDefault()
    e.dataTransfer.dropEffect = 'move'
    setHoverRoot(true)
    setDropTarget(null)
    setDropExternal(false)
    clearSpring()
  }

  const onRootDrop = (e: React.DragEvent) => {
    if (!workspace) return
    if (hasExternalFiles(e)) {
      e.preventDefault()
      const paths = externalPaths(e)
      endDrag()
      void doImport(workspace, paths)
      return
    }
    if (!drag) return
    if (!drag.entries.some((x) => canDropInto(x, workspace))) return
    e.preventDefault()
    const moved = drag.entries
    endDrag()
    void moveMany(moved, workspace)
  }

  // —— 右键菜单 ——

  const openMenu = (e: React.MouseEvent, entry: FsEntry | null, parent: string) => {
    if (!workspace) return
    e.preventDefault()
    e.stopPropagation()
    // 右键一个「已在多选里」的条目 → 保持整组选中（访达口径）；否则改成只选它。
    if (entry && !selSet.has(entry.path)) selectOnly(entry.path)
    // 贴边时把菜单拉回视口内（条目数决定高度，这里按上限估）
    const w = 208
    const h = entry ? 300 : 168
    setMenu({
      x: Math.max(8, Math.min(e.clientX, window.innerWidth - w)),
      y: Math.max(8, Math.min(e.clientY, window.innerHeight - h)),
      entry,
      parent,
    })
  }

  // 菜单「粘贴 / 新建」的目标目录：条目是目录 → 里面；是文件 → 它所在目录；空白 → 根。
  const menuTargetDir = ((): string => {
    if (!menu) return workspace ?? ''
    if (!menu.entry) return menu.parent
    return menu.entry.isDir ? menu.entry.path : menu.parent
  })()

  // 菜单作用对象：右键的条目在多选里 → 整组；否则只有它自己。
  const menuEntries: FsEntry[] = useMemo(() => {
    if (!menu?.entry) return []
    return selSet.has(menu.entry.path) && selectedEntries.length > 1 ? selectedEntries : [menu.entry]
  }, [menu, selSet, selectedEntries])
  const menuMulti = menuEntries.length > 1
  // 批量动作的文案带上数量（访达在菜单里也这么写），否则用户不知道会删掉几个。
  const withCount = (label: string) => (menuMulti ? `${label}（${menuEntries.length}）` : label)

  // —— 键盘 ——

  const scrollIntoView = (path: string) => {
    window.requestAnimationFrame(() => rowRefs.current.get(path)?.scrollIntoView({ block: 'nearest' }))
  }

  const onTreeKeyDown = (e: React.KeyboardEvent) => {
    if (renaming || creating) return // 输入框自己处理按键
    // 菜单打开时 ↑↓ 属于菜单（焦点在菜单项上，同一事件仍会冒泡到 window 监听器）。
    // 少这一条的表现很隐蔽：按 ↑ 在菜单里移动，树的选中却同时被塌成单选 ——
    // 菜单上的「（4 项）」标签当场消失（e2e 抓到的就是这个）。
    if (menu) return
    const mod = e.metaKey || e.ctrlKey
    // 方向键从「当前行」出发（cursor），不是从选中集的第一项 —— 多选后按 ↓ 应该落到
    // 你最后触碰的那一行的下一行，而不是跳回整块选区的顶部。
    const curPath = cursor ?? sel[0] ?? null
    const cur = entryRows.findIndex((r) => r.entry.path === curPath)
    const row = cur >= 0 ? entryRows[cur] : null

    // ⌘A 全选可见条目（与访达一致：全选 = 当前视图里的全部）
    if (mod && e.key.toLowerCase() === 'a' && entryRows.length) {
      e.preventDefault()
      setSel(entryRows.map((r) => r.entry.path))
      if (!anchor) setAnchor(entryRows[0].entry.path)
      setCursor(entryRows[0].entry.path)
      return
    }
    if (mod && (e.key === 'c' || e.key === 'x') && selectedEntries.length) {
      e.preventDefault()
      doCopy(selectedEntries, e.key === 'c' ? 'copy' : 'cut')
      return
    }
    if (mod && e.key === 'v' && clip) {
      e.preventDefault()
      // 粘贴目标：选中项是目录 → 里面；是文件 → 它所在目录；无选中 → 工作区根
      const dest = row ? (row.entry.isDir ? row.entry.path : row.parent) : (workspace ?? '')
      void doPaste(dest)
      return
    }
    if (mod && e.key.toLowerCase() === 'd' && selectedEntries.length) {
      e.preventDefault()
      void doDuplicate(selectedEntries)
      return
    }
    if (mod && e.key === 'Backspace' && selectedEntries.length) {
      e.preventDefault()
      setConfirm({ entries: selectedEntries })
      return
    }
    if (e.key === 'F2' && row && selectedEntries.length === 1) {
      e.preventDefault()
      startRename(row.entry)
      return
    }
    if (e.key === 'Enter' && row) {
      e.preventDefault()
      if (row.entry.isDir) toggleDir(row.entry.path)
      else openFile(row.entry.path, undefined, 'source')
      return
    }
    if (e.key === 'Escape') {
      setSel([])
      setAnchor(null)
      setCursor(null)
      return
    }
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      if (!entryRows.length) return
      const next = e.key === 'ArrowDown'
        ? (cur < 0 ? 0 : Math.min(entryRows.length - 1, cur + 1))
        : (cur < 0 ? entryRows.length - 1 : Math.max(0, cur - 1))
      const p = entryRows[next].entry.path
      // ⇧ + ↑↓ = 从锚点扩选（锚点不动），否则单选
      if (e.shiftKey) selectRange(p)
      else selectOnly(p)
      scrollIntoView(p)
      return
    }
    if (!row) return
    if (e.key === 'ArrowRight') {
      e.preventDefault()
      if (!row.entry.isDir) return
      if (!expanded[row.entry.path]) toggleDir(row.entry.path)
      else {
        const child = entryRows[cur + 1]
        if (child && child.entry.path.startsWith(row.entry.path + '/')) {
          selectOnly(child.entry.path)
          scrollIntoView(child.entry.path)
        }
      }
      return
    }
    if (e.key === 'ArrowLeft') {
      e.preventDefault()
      if (row.entry.isDir && expanded[row.entry.path]) {
        toggleDir(row.entry.path)
        return
      }
      const parent = entryRows.find((r) => r.entry.path === row.parent)
      if (parent) {
        selectOnly(parent.entry.path)
        scrollIntoView(parent.entry.path)
      }
    }
  }

  // —— 渲染 ——

  const reveal = async (p: string) => {
    setMenu(null)
    if (!window.desktop?.showItemInFolder) {
      showToast(t('files.revealUnsupported'), 'error')
      return
    }
    await window.desktop.showItemInFolder(p)
  }

  const copyPath = async (p: string) => {
    setMenu(null)
    const ok = await copyText(p)
    showToast(t(ok ? 'files.pathCopied' : 'files.pathCopyFail'), ok ? 'ok' : 'error')
  }

  const rowClass = (entry: FsEntry, parent: string) => {
    const dest = entry.isDir ? entry.path : parent
    return [
      'lf-row',
      entry.isDir ? 'dir' : 'file',
      !entry.isDir && activeKey && fileKeyOf(entry.path, workspace) === activeKey ? 'active' : '',
      selSet.has(entry.path) ? 'sel' : '',
      // 多选时只有「当前行」带描边（最后点/最后用键盘走到的那一行）：每行都描边会看成一摞卡片
      selSet.has(entry.path) && cursor === entry.path ? 'sel-primary' : '',
      entry.name.startsWith('.') ? 'dot' : '',
      pending[entry.path] ? 'pending' : '',
      drag?.entries.some((x) => x.path === entry.path) ? 'dragging' : '',
      // 高亮判据要和 drop 的落点一致：目录行拖进自己，文件行拖进它所在的目录
      dropTarget === entry.path && (dropExternal || (drag && drag.entries.some((x) => canDropInto(x, dest)))) ? 'dz' : '',
    ].filter(Boolean).join(' ')
  }

  const renderEntry = (row: RowEntry) => {
    const { entry, depth, parent } = row
    const isOpen = entry.isDir && expanded[entry.path]
    const state = dirs[entry.path]
    const err = state && !Array.isArray(state) && state !== 'loading' ? state.error : ''
    const dest = entry.isDir ? entry.path : parent
    const canDropHere = !!drag && drag.entries.some((x) => canDropInto(x, dest))
    return (
      <div
        key={entry.path}
        ref={(el) => {
          if (el) rowRefs.current.set(entry.path, el)
          else rowRefs.current.delete(entry.path)
        }}
        className={rowClass(entry, parent)}
        style={{ paddingLeft: 6 + depth * 14 }}
        role="treeitem"
        aria-expanded={entry.isDir ? !!isOpen : undefined}
        aria-selected={selSet.has(entry.path)}
        aria-level={depth + 1}
        title={entry.path}
        draggable={renaming?.path !== entry.path}
        onClick={(e) => {
          e.stopPropagation()
          // ⌘ 增减、⇧ 选区间 —— 这两种手势**不**触发打开（否则一次框选会打开一堆文件）
          if (e.shiftKey) {
            selectRange(entry.path)
            return
          }
          if (e.metaKey || e.ctrlKey) {
            toggleSel(entry.path)
            return
          }
          selectOnly(entry.path)
          if (renaming) return
          if (entry.isDir) toggleDir(entry.path)
          else openFile(entry.path, undefined, 'source')
        }}
        onDoubleClick={(e) => {
          e.stopPropagation()
          startRename(entry)
        }}
        onContextMenu={(e) => openMenu(e, entry, parent)}
        onDragStart={(e) => {
          e.stopPropagation()
          e.dataTransfer.effectAllowed = 'move'
          // 至少放一个值：部分平台不设 data 就不触发 drop 事件（也不显示拖拽光标）。
          e.dataTransfer.setData('text/plain', entry.path)
          // 拖已在多选里的行 = 拖整组；拖未选中的行 = 先单选它再拖它自己。
          const group = selSet.has(entry.path) && selectedEntries.length > 1 ? selectedEntries : [entry]
          if (!selSet.has(entry.path)) selectOnly(entry.path)
          setDrag({ entries: group })
        }}
        onDragEnd={endDrag}
        onDragOver={(e) => onRowDragOver(e, row)}
        onDragLeave={(e) => {
          // 只在真的离开这一行时清高亮（行内有多个 span，relatedTarget 仍在本行内时不清）
          if ((e.currentTarget as HTMLElement).contains(e.relatedTarget as Node | null)) return
          setDropTarget((d) => (d === entry.path ? null : d))
          setDropExternal(false)
          if (springRef.current?.path === entry.path) clearSpring()
        }}
        onDrop={(e) => onRowDrop(e, row)}
      >
        <span className="lf-chev">
          {entry.isDir ? (isOpen ? <ChevronDown size={11} /> : <ChevronRight size={11} />) : null}
        </span>
        <span className="lf-ic">{entry.isDir ? (isOpen ? <FolderOpenIcon size={13} /> : <FolderIcon size={13} />) : <FileIcon size={13} />}</span>
        {renaming?.path === entry.path ? (
          <input
            ref={inputRef}
            className="lf-input"
            value={renaming.name}
            aria-label={t('files.rename')}
            spellCheck={false}
            onChange={(e) => setRenaming((r) => (r ? { ...r, name: e.target.value } : r))}
            onClick={(e) => e.stopPropagation()}
            onDoubleClick={(e) => e.stopPropagation()}
            onKeyDown={(e) => {
              e.stopPropagation()
              if (e.key === 'Enter') void commitRename()
              else if (e.key === 'Escape') {
                editSettled.current = true
                setRenaming(null)
              }
            }}
            onBlur={() => void commitRename()}
          />
        ) : (
          <span className="lf-nm">{entry.name}</span>
        )}
        {dropTarget === entry.path && (dropExternal || canDropHere) && (
          <span className="lf-dz-badge">{t(dropExternal ? 'files.dropImport' : 'files.dropInto')}</span>
        )}
        {state === 'loading' && <span className="lf-busy">…</span>}
        {err && <span className="lf-err" title={err}>!</span>}
      </div>
    )
  }

  const renderCreate = (row: RowCreate) => (
    <div className="lf-row new" style={{ paddingLeft: 6 + row.depth * 14 }} key="__new__">
      <span className="lf-chev" />
      <span className="lf-ic">{creating?.kind === 'dir' ? <FolderIcon size={13} /> : <FileIcon size={13} />}</span>
      <input
        ref={inputRef}
        className="lf-input"
        value={creating?.name ?? ''}
        aria-label={creating?.kind === 'dir' ? t('files.newFolder') : t('files.newFile')}
        spellCheck={false}
        onChange={(e) => setCreating((c) => (c ? { ...c, name: e.target.value } : c))}
        onKeyDown={(e) => {
          e.stopPropagation()
          if (e.key === 'Enter') void commitCreate()
          else if (e.key === 'Escape') {
            editSettled.current = true
            setCreating(null)
            void reload(row.parent)
          }
        }}
        onBlur={() => void commitCreate()}
      />
    </div>
  )

  return (
    <div className="lf" aria-label={t('files.aria')}>
      <div className="lf-head">
        <button className="lf-back" onClick={onBack} title={t('files.backHint')}>
          <Back size={13} /> {t('files.back')}
        </button>
        <span className="grow" />
        {/* 新建文件夹：作用在「选中项所在的目录」（与右键菜单同一口径），一眼可发现。 */}
        <button
          className="lf-refresh lf-newdir"
          onClick={() => {
            const single = selectedEntries.length === 1 ? entryRows.find((r) => r.entry.path === sel[0]) : null
            const dir = single ? (single.entry.isDir ? single.entry.path : single.parent) : (workspace ?? '')
            if (dir) startCreate(dir, 'dir')
          }}
          disabled={!workspace}
          title={t('files.newFolder')}
          aria-label={t('files.newFolder')}
        >
          <FolderPlus size={14} />
        </button>
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
      <div
        className={`lf-tree ${hoverRoot && (drag || dropExternal) ? 'dz-root' : ''}`}
        role="tree"
        aria-label={t('files.aria')}
        aria-multiselectable
        tabIndex={0}
        ref={treeRef}
        onKeyDown={onTreeKeyDown}
        onMouseDown={(e) => {
          // 点空白处 = 取消选中（点行时行自己 stopPropagation）
          if (e.target === e.currentTarget) {
            setSel([])
            setAnchor(null)
            setCursor(null)
          }
        }}
        onContextMenu={(e) => openMenu(e, null, workspace ?? '')}
        onDragOver={onRootDragOver}
        onDragLeave={(e) => {
          if ((e.currentTarget as HTMLElement).contains(e.relatedTarget as Node | null)) return
          setHoverRoot(false)
          setDropExternal(false)
        }}
        onDrop={onRootDrop}
      >
        {!workspace && <div className="lf-hint">{t('files.noWorkspace')}</div>}
        {workspace && rootState === 'loading' && <div className="lf-hint">{t('files.loading')}</div>}
        {rootError && <div className="lf-hint err">{rootError}</div>}
        {workspace && Array.isArray(rootState) && rows.length === 0 && (
          <div className="lf-hint">{q ? t('files.noMatch') : t('files.empty')}</div>
        )}
        {rows.map((row) => (row.kind === 'new' ? renderCreate(row) : renderEntry(row)))}
        {hoverRoot && (drag || dropExternal) && (
          <div className="lf-drop-hint">{t(dropExternal ? 'files.dropImportRoot' : 'files.dropRoot')}</div>
        )}
      </div>
      {menu && (
        <div className="ctx-menu lf-menu" style={{ left: menu.x, top: menu.y }} role="menu">
          {menu.entry && (
            <>
              <button
                className="ctx-item"
                role="menuitem"
                onClick={() => {
                  const entry = menu.entry!
                  setMenu(null)
                  if (entry.isDir) toggleDir(entry.path)
                  else openFile(entry.path, undefined, 'source')
                }}
              >
                {menu.entry.isDir ? <FolderOpenIcon size={13} /> : <FileIcon size={13} />}
                {menu.entry.isDir
                  ? t(expanded[menu.entry.path] ? 'files.collapse' : 'files.expand')
                  : t('files.open')}
              </button>
              <div className="ctx-sep" />
            </>
          )}
          <button className="ctx-item" role="menuitem" onClick={() => startCreate(menuTargetDir, 'dir')}>
            <FolderPlus size={13} /> {t('files.newFolder')}
          </button>
          <button className="ctx-item" role="menuitem" onClick={() => startCreate(menuTargetDir, 'file')}>
            <FilePlusIcon size={13} /> {t('files.newFile')}
          </button>
          <div className="ctx-sep" />
          {menu.entry && (
            <>
              <button className="ctx-item" role="menuitem" onClick={() => doCopy(menuEntries, 'copy')}>
                <CopyIcon size={13} /> {withCount(t('files.copy'))}
              </button>
              <button className="ctx-item" role="menuitem" onClick={() => doCopy(menuEntries, 'cut')}>
                <ScissorsIcon size={13} /> {withCount(t('files.cut'))}
              </button>
            </>
          )}
          <button className="ctx-item" role="menuitem" disabled={!clip} onClick={() => void doPaste(menuTargetDir)}>
            <PasteIcon size={13} /> {t('files.paste')}
            {clip && (
              <span className="ctx-hint">
                {clip.entries.length > 1 ? `${clip.entries.length} 项` : clip.entries[0].name}
              </span>
            )}
          </button>
          {menu.entry && (
            <>
              <button className="ctx-item" role="menuitem" onClick={() => void doDuplicate(menuEntries)}>
                <CopyIcon size={13} /> {withCount(t('files.duplicate'))}
              </button>
              <button
                className="ctx-item"
                role="menuitem"
                disabled={menuMulti}
                onClick={() => startRename(menu.entry!)}
              >
                <PencilIcon size={13} /> {t('files.rename')}
              </button>
              {/* 「编辑」（PR-05b 入口②）：只对**文本类单文件**出现 —— 目录/二进制/图片/
                  工作簿编辑不了（binary 的最终判定在 file_open，这里只是别给一个必然失败的动作）。
                  位置刻意在「重命名」之后：菜单前两项（打开 / 新建文件夹）的顺序被
                  e2e/verify_left_files_batch.mjs 的键盘用例钉住，插在前面会改变 Tab/↑↓ 顺序。 */}
              {/* 「预览」（R08b）：**显式**进预览标签 —— 同一个文件在注册表那套渲染器里长什么样，
                  与「打开」的默认通道（可视类落内嵌浏览器 web 标签，D-I 口径不变）是两回事。
                  位置在「编辑」之后：菜单前两项（打开 / 新建文件夹）的顺序被
                  e2e/verify_left_files_batch.mjs 的键盘用例钉住，插在前面会改 ↑↓ 顺序。
                  所有非目录文件都给这一项（含 .zip 这类没有渲染器的）：落点会写明「没有能渲染
                  .zip 的渲染器」并给路径，比在菜单里按扩展名猜「能不能预览」更诚实。 */}
              {!menu.entry.isDir && (
                <button
                  className="ctx-item"
                  role="menuitem"
                  data-preview="1"
                  onClick={() => {
                    const entry = menu.entry!
                    setMenu(null)
                    openPreviewTab(entry.path)
                  }}
                >
                  <EyeIcon size={13} /> {t('files.preview')}
                </button>
              )}
              {editorOn && !menu.entry.isDir && isTextLikePath(menu.entry.path) && (
                <button
                  className="ctx-item"
                  role="menuitem"
                  data-edit="1"
                  onClick={() => {
                    const entry = menu.entry!
                    setMenu(null)
                    openFileForEdit(entry.path)
                  }}
                >
                  <PencilIcon size={13} /> {t('editor.edit')}
                </button>
              )}
              <div className="ctx-sep" />
              <button className="ctx-item" role="menuitem" disabled={menuMulti} onClick={() => void reveal(menu.entry!.path)}>
                <FolderOpenIcon size={13} /> {t('files.reveal')}
              </button>
              <button className="ctx-item" role="menuitem" disabled={menuMulti} onClick={() => void copyPath(menu.entry!.path)}>
                <CopyIcon size={13} /> {t('files.copyPath')}
              </button>
              <div className="ctx-sep" />
              <button
                className="ctx-item danger"
                role="menuitem"
                onClick={() => {
                  const entries = menuEntries
                  setMenu(null)
                  setConfirm({ entries })
                }}
              >
                <TrashIcon size={13} /> {withCount(t('files.delete'))}
              </button>
            </>
          )}
        </div>
      )}
      {confirm && (
        <ConfirmModal
          title={t('files.deleteTitle')}
          message={
            confirm.entries.length === 1
              ? (confirm.entries[0].isDir ? t('files.deleteDirMsg') : t('files.deleteMsg')).replace('{name}', confirm.entries[0].name)
              : t('files.deleteMsgMany')
                .replace('{name}', confirm.entries[0].name)
                .replace('{n}', String(confirm.entries.length))
          }
          confirmLabel={t('files.delete')}
          cancelLabel={t('files.cancel')}
          danger
          onConfirm={() => void doDelete(confirm.entries)}
          onCancel={() => setConfirm(null)}
        />
      )}
    </div>
  )
}
