import { Fragment, useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { CSSProperties } from 'react'
import { useAppStore, titleKey, MAX_WORKSPACE_PINS, MAX_SESSION_PINS } from '../store/useAppStore'
import ConfirmModal from './ConfirmModal'
import WorkspaceFiles from './WorkspaceFiles'
import HaiLogo from './HaiLogo'
import { ChevronDown, Plus, FolderPlus, FolderIcon, FolderOpenIcon, SessionPlus, PinIcon, Sparkles, Gear, SessionIcon, PanelLeft, Clock, MoreIcon, PluginIcon } from './icons'
// 虚拟 Project（插件）的判据/分组：与 store、插件分组渲染共用同一份纯函数
// （virtual 标记 + 插件 home 路径特征），左栏不再各写一次 p.virtual。
import { isVirtualProject, pluginProjectLabel, realProjects, splitPluginProjects } from '../lib/pluginProjects'
import { useT } from '../i18n'
import { AgentTree } from './AgentTree'
import { Bot } from './icons'
import { useHubStore } from '../hub/useHubStore'
import HubSidebar from '../hub/HubSidebar'
import { accentOfAgent, accentOfGroup } from '../hub/agentAccent'

// 工作区行内操作（查看文件/置顶/新建/删除）需要的最小面板宽度：低于它就收成一个「⋯」溢出菜单。
// 算法：行左右 margin 12 + padding 10 + 预留 81 + chevron 11 + 图标 15 + 两处 gap 14 + 计数 ~10
// + 名字下限 26 ≈ 179 —— 再留些余量到 240，否则名字只剩两三个字（默认宽度 236 即走窄形态）。
const ROW_ACTIONS_MIN_W = 240

// 左栏 = 工作区管理：显式「添加工作区」（选择已有 / 新建文件夹），每个工作区目录下挂多个会话。
// 两种视图（leftView）：任务列表（默认）| 工作区文件浏览（WorkspaceFiles）。
// 入口 = **每个工作区行**的文件夹按钮（悬停浮出，与 pin/新建/删除同级）：一次点击 = 切到该
// 工作区 + 以它为根进入文件视角；视角内「返回任务」切回。文件浏览是纯 UI 模式（局部 state，
// 不持久化），根目录始终跟随活跃工作区。
// 行内操作按面板宽度两态（ROW_ACTIONS_MIN_W）：窄（默认 236）→ 收成「⋯」溢出菜单（名字才有
// 空间显示）；宽（≥240，可拖）→ 铺开 4 个按钮。两态行为完全一致，菜单项即 4 个操作。
// 新建会话：已选工作区 → 直接在当前工作区建会话（不弹选择器）；未选 → 先选/建文件夹。
// 点击工作区头 → 切换为活跃并展开；活跃工作区再点 → 仅折叠/展开；悬停 + → 在该工作区直接建会话。
// 会话标题：优先取重命名覆盖（titleOverrides，localStorage），否则 bridge 自动标题（第一条 user 文本）。
// 双击会话标题 → 内联重命名（Enter 保存 / Esc 取消 / 清空恢复自动标题）。
export default function LeftPanel({ collapsed }: { collapsed?: boolean }) {
  const t = useT()
  const leftExpandedPref = useAppStore((s) => s.leftExpanded)
  // 渲染用的「展开/折叠」以面板实际状态为准（App 从 Panel.onResize 回写）：
  // store 只是用户意图，库在空间不足时会拒绝 expand —— 若仍按 store 画展开内容，
  // 就会出现 44px 容器里塞整栏（P4 审计 HUB-N-01）。独立使用（无 prop）时退回 store。
  const leftExpanded = collapsed == null ? leftExpandedPref : !collapsed
  const toggleLeft = useAppStore((s) => s.toggleLeft)
  const leftPinned = useAppStore((s) => s.leftPinned)
  const togglePin = useAppStore((s) => s.togglePin)
  const workspace = useAppStore((s) => s.workspace)
  const projects = useAppStore((s) => s.projects)
  // 真实工作区与虚拟 Project（插件）分组渲染（docs/creator-mode-v0.1/09-agent-bridge.md §9）：
  // 真实项目保持原顺序、原折叠状态；插件组排在它们之后，空则整组隐藏。
  const realList = realProjects(projects)
  const pluginGroups = splitPluginProjects(projects)
  const activeSessionId = useAppStore((s) => s.activeSessionId)
  const titleOverrides = useAppStore((s) => s.titleOverrides)
  const newSession = useAppStore((s) => s.newSession)
  const setActiveSession = useAppStore((s) => s.setActiveSession)
  const focusAgent = useAppStore((s) => s.focusAgent)
  const focusedAgent = useAppStore((s) => s.focusedAgent)
  const renameSession = useAppStore((s) => s.renameSession)
  const deleteSession = useAppStore((s) => s.deleteSession)
  const deleteWorkspace = useAppStore((s) => s.deleteWorkspace)
  const openSkills = useAppStore((s) => s.openSkills)
  const openHub = useHubStore((s) => s.openHub)
  const hubOpen = useHubStore((s) => s.open)
  const closeHub = useHubStore((s) => s.closeHub)
  // 折叠/展开态的左栏「智能体」入口用当前会话的身份色（群组取群主/成员 accent）。
  const hubAccent = useHubStore((s) => {
    const group = s.activeGroupId ? s.groups.find((g) => g.id === s.activeGroupId) : null
    if (group) return accentOfGroup(group, s.agents)
    const agent = s.activeId ? s.agents.find((a) => a.id === s.activeId) : null
    return agent ? accentOfAgent(agent) : ''
  })
  const hubAccentStyle = hubAccent ? ({ ['--agent-accent']: hubAccent } as CSSProperties) : undefined
  // 工作区/会话/文件视角都是「离开 Hub 回工作区」的入口：先退出 Hub，再执行原动作。
  const exitHub = (): void => {
    if (hubOpen) closeHub()
  }
  const openCron = useAppStore((s) => s.openCron)
  const openSettings = useAppStore((s) => s.openSettings)
  const setActiveWorkspace = useAppStore((s) => s.setActiveWorkspace)
  const toggleWorkspacePin = useAppStore((s) => s.toggleWorkspacePin)
  const addWorkspace = useAppStore((s) => s.addWorkspace)
  const openNewFolder = useAppStore((s) => s.openNewFolder)
  const createSessionIn = useAppStore((s) => s.createSessionIn)
  // 每会话是否在运行（是否忙）：store 事件级维护的 busy 会话 id 集合（agent/tool 起止时更新），
  // 左栏运行点据此点亮。busySids 只在这些转移点变更 → 流式 chunk 不重渲左栏。
  const busySids = useAppStore((s) => s.busySids)
  const pinnedWorkspaceCount = projects.filter((p) => p.pinned).length
  // 每个项目的展开状态（默认：活跃项目/有会话的项目展开）
  const [open, setOpen] = useState<Record<string, boolean>>({})
  // 每个工作区是否「展示更多」（默认收起：只显示 pinned + 最近补足共 5 条）
  const [more, setMore] = useState<Record<string, boolean>>({})
  const toggleSessionPin = useAppStore((s) => s.toggleSessionPin)
  // 「添加工作区」下拉展开
  const [addOpen, setAddOpen] = useState(false)
  // 折叠态「添加工作区」菜单容器（fixed 定位需读图标按钮 rect；点外部关闭）
  const addMenuRef = useRef<HTMLDivElement>(null)
  const addBtnRef = useRef<HTMLButtonElement>(null)
  // 折叠态菜单坐标（fixed；按图标按钮 rect 计算，垂直居中于按钮、贴其右侧）
  const [railMenuPos, setRailMenuPos] = useState<{ top: number; left: number } | null>(null)
  // 会话内联重命名（同时只有一个在编辑）
  const [editing, setEditing] = useState<{ path: string; sid: string } | null>(null)
  const [draft, setDraft] = useState('')
  const skipBlur = useRef(false) // Enter/Esc 后阻止 onBlur 重复提交
  // 左栏视图：任务列表（默认）| 工作区文件浏览（「查看文件」入口，见 WorkspaceFiles）。
  // 局部 state：纯 UI 模式，不跨会话持久（重开 app 回到任务列表，避免「上次停在文件树」的困惑）。
  const [leftView, setLeftView] = useState<'tasks' | 'files'>('tasks')
  // 面板宽度（可拖拽：react-resizable-panels，140–360）：窄了把行内 4 个操作收成「⋯」。
  const asideRef = useRef<HTMLElement>(null)
  const [narrow, setNarrow] = useState(false)
  // 「⋯」溢出菜单（fixed 定位：列表可滚动 + 面板 overflow:hidden，绝对定位会被裁/跟随滚动）
  const [moreMenu, setMoreMenu] = useState<{ path: string; top: number; right: number } | null>(null)
  const moreMenuRef = useRef<HTMLDivElement>(null)
  // 删除确认（居中设计弹窗，不用 alert/confirm）
  const [confirm, setConfirm] = useState<{ kind: 'session' | 'workspace'; path: string; sid?: string; name?: string } | null>(null)

  // 面板宽度观测（拖拽改宽即时切换形态；收起成图标条时也会报小宽度，但那时不渲染行）
  useEffect(() => {
    const el = asideRef.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const measure = () => setNarrow(el.getBoundingClientRect().width < ROW_ACTIONS_MIN_W)
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    measure()
    return () => ro.disconnect()
  }, [leftExpanded])

  // 「⋯」菜单：点外部 / Esc 关闭（与「添加工作区」菜单同款）
  useEffect(() => {
    if (!moreMenu) return
    const onDown = (e: MouseEvent) => {
      if (!moreMenuRef.current?.contains(e.target as Node)) setMoreMenu(null)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setMoreMenu(null)
    }
    document.addEventListener('mousedown', onDown)
    window.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      window.removeEventListener('keydown', onKey)
    }
  }, [moreMenu])

  const isOpen = (path: string) => open[path] ?? path === workspace
  // 折叠态菜单：点外部 / Esc 关闭（展开态菜单为内联块，随按钮外点自然收起）
  useEffect(() => {
    if (!addOpen) return
    const onDown = (e: MouseEvent) => {
      const el = e.target as Node
      if (addMenuRef.current?.contains(el) || addBtnRef.current?.contains(el)) return
      setAddOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setAddOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    window.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      window.removeEventListener('keydown', onKey)
    }
  }, [addOpen])
  // 折叠态菜单定位：fixed 不受图标条 overflow 裁剪，坐标按图标按钮 rect 算（垂直居中于按钮）。
  // 用 layout effect 在绘制前定位，避免首帧闪到左上角。
  useLayoutEffect(() => {
    if (!addOpen || leftExpanded) {
      setRailMenuPos(null)
      return
    }
    const btn = addBtnRef.current
    const menu = addMenuRef.current
    if (!btn) return
    const b = btn.getBoundingClientRect()
    const h = menu?.getBoundingClientRect().height ?? 0
    setRailMenuPos({
      top: Math.round(b.top + b.height / 2 - h / 2),
      left: Math.round(b.right + 8),
    })
  }, [addOpen, leftExpanded])
  // 「添加工作区」两个入口（选择已有 / 新建文件夹）：展开态内联菜单与折叠态浮动菜单共用。
  const addWorkspaceItems = (
    <>
      <button role="menuitem" onClick={() => { setAddOpen(false); void addWorkspace() }}>
        <FolderIcon size={13} /> {t('left.selectFolder')}
      </button>
      <button role="menuitem" onClick={() => { setAddOpen(false); openNewFolder() }}>
        <Plus size={13} /> {t('left.newFolder')}
      </button>
    </>
  )

  return (
    <>
      <aside ref={asideRef} className={`left ${leftExpanded ? 'expanded' : 'collapsed'}`} aria-label={t('left.aria')}>
      {leftExpanded ? (
        <div style={{ flex: 1, display: 'flex', flexDirection: 'column', minWidth: 0, minHeight: 0 }}>
          {/* 品牌行：HAI 共笔 logo 常驻左上角（Electron 拖拽区）；右侧 = 左栏收缩（归属左栏自身） */}
          <div className="left-brand">
            <span className="brand-logo" title="HAI"><HaiLogo height={36} /></span>
            <button
              className="brand-collapse"
              onClick={toggleLeft}
              title={t('topbar.toggleLeft')}
              aria-label={t('topbar.toggleLeft')}
            >
              <PanelLeft size={15} />
            </button>
          </div>
          {leftView === 'files' ? (
            <WorkspaceFiles onBack={() => setLeftView('tasks')} />
          ) : (
          <>
          {/* Hub 左侧段：智能体/群组列表 + 全历史搜索，始终位于「工作区」之上；
              限高 38vh 内部滚动，工作区树仍占据下方剩余空间。 */}
          <HubSidebar />
          <div className="left-head">
            <span className="left-title">{t('left.workspaces')}</span>
            <button
              className={`iconbtn ${leftPinned ? 'on' : ''}`}
              onClick={togglePin}
              title={t(leftPinned ? 'left.pinned' : 'left.pin')}
              aria-pressed={leftPinned}
              aria-label={t('left.pin')}
            >
              <PinIcon />
            </button>
          </div>

          <button
            className="new-session-btn"
            onClick={() => { exitHub(); void newSession() }}
            title={t(workspace ? 'left.newSessionHere' : 'left.newSessionFirst')}
          >
            <SessionPlus size={14} /> {t('left.newSession')}
          </button>

          <div className="add-ws">
            <button
              className="add-ws-btn"
              ref={addBtnRef}
              onClick={() => setAddOpen((v) => !v)}
              aria-expanded={addOpen}
              aria-haspopup="menu"
              aria-label={t('left.addWorkspace')}
            >
              <FolderPlus size={13} /> {t('left.addWorkspace')} <span className="add-caret">{addOpen ? '▾' : '▸'}</span>
            </button>
            {addOpen && (
              <div className="add-ws-menu" role="menu" ref={addMenuRef}>
                {addWorkspaceItems}
              </div>
            )}
          </div>

          <div className="proj-list">
            {projects.length === 0 && (
              <div className="session-item" style={{ color: 'var(--text-3)', cursor: 'default' }}>{t('left.noProjects')}</div>
            )}
            {realList.map((p) => {
              const pathOpen = isOpen(p.path)
              const activeProject = p.path === workspace
              return (
                <div key={p.path} className={`ws-group ${pathOpen ? 'open' : ''}`}>
                  <div
                    className={`ws-head ${activeProject ? 'active' : ''} ${p.pinned ? 'pinned' : ''} ${narrow ? 'narrow' : ''}`}
                    title={activeProject ? undefined : t('left.switchHint')}
                    onClick={() => {
                      exitHub()
                      // 展开/收起重置「展示更多」状态（每次收起再展开回到 5 条视图）
                      setMore((m) => {
                        if (p.path !== workspace) return m // 切换工作区不重置（保持用户偏好）
                        const n = { ...m }
                        delete n[p.path]
                        return n
                      })
                      if (p.path !== workspace) {
                        // 非活跃工作区：切换为活跃并展开（切换到空工作区的唯一入口）
                        setActiveWorkspace(p.path)
                        setOpen((o) => ({ ...o, [p.path]: true }))
                      } else {
                        setOpen((o) => ({ ...o, [p.path]: !pathOpen }))
                      }
                    }}
                  >
                    <ChevronDown className="chev" />
                    <span className="ws-ic"><FolderIcon /></span>
                    <span className="wname">{p.path.split('/').pop() || p.path}</span>
                    <span className="ws-count">{p.sessions.length}</span>
                    {narrow ? (
                      <button
                        className={`ws-more ${moreMenu?.path === p.path ? 'on' : ''}`}
                        title={t('left.more')}
                        aria-label={t('left.more')}
                        aria-expanded={moreMenu?.path === p.path}
                        aria-haspopup="menu"
                        onClick={(e) => {
                          e.stopPropagation()
                          const r = e.currentTarget.getBoundingClientRect()
                          setMoreMenu((m) =>
                            m?.path === p.path
                              ? null
                              : { path: p.path, top: Math.round(r.bottom + 4), right: Math.max(8, Math.round(window.innerWidth - r.right)) },
                          )
                        }}
                      >
                        <MoreIcon size={14} />
                      </button>
                    ) : (
                      <>
                    <button
                      className={`ws-pin ${p.pinned ? 'on' : ''}`}
                      title={t(p.pinned ? 'left.unpinWorkspace' : pinnedWorkspaceCount >= MAX_WORKSPACE_PINS ? 'left.pinWorkspaceLimit' : 'left.pinWorkspace')}
                      aria-label={t(p.pinned ? 'left.unpinWorkspace' : 'left.pinWorkspace')}
                      aria-pressed={p.pinned}
                      disabled={!p.pinned && pinnedWorkspaceCount >= MAX_WORKSPACE_PINS}
                      onClick={(e) => { e.stopPropagation(); toggleWorkspacePin(p.path) }}
                    >
                      <PinIcon size={13} />
                    </button>
                    <button
                      className="ws-files"
                      title={t('left.viewFilesIn')}
                      aria-label={t('left.viewFilesIn')}
                      onClick={(e) => {
                        e.stopPropagation()
                        exitHub()
                        // 文件视角以**该工作区**为根：非活跃先切为活跃（与「在该工作区新建会话」同款语义）
                        if (p.path !== workspace) setActiveWorkspace(p.path)
                        setLeftView('files')
                      }}
                    >
                      <FolderOpenIcon size={13} />
                    </button>
                    <button
                      className="ws-new"
                      title={t('left.newSessionIn')}
                      aria-label={t('left.newSessionIn')}
                      onClick={(e) => { e.stopPropagation(); exitHub(); createSessionIn(p.path) }}
                    >
                      <Plus size={12} />
                    </button>
                    <button
                      className="ws-del"
                      title={t('left.deleteWorkspace')}
                      aria-label={t('left.deleteWorkspace')}
                      onClick={(e) => {
                        e.stopPropagation()
                        setConfirm({ kind: 'workspace', path: p.path, name: p.path.split('/').pop() || p.path })
                      }}
                    >
                      ✕
                    </button>
                      </>
                    )}
                  </div>
                  {pathOpen && (
                    <div className="ws-body">
                      {p.sessions.length === 0 && (
                        <div className="session-item" style={{ color: 'var(--text-3)', cursor: 'default' }}>{t('left.noSessions')}</div>
                      )}
                      {(() => {
                        // 展示列表：pinned 优先（bridge 顺序：最新 pin 在前）+ 非 pinned 按
                        // updatedAt 补足，共 ≤5 条；「展示更多」展开后全量。
                        const all = p.sessions
                        const pinned = all.filter((x) => x.pinned)
                        const unpinned = all.filter((x) => !x.pinned)
                        const expanded = more[p.path] === true
                        const shown = expanded
                          ? all
                          : [...pinned, ...unpinned].slice(0, MAX_SESSION_PINS)
                        const hasMore = all.length > MAX_SESSION_PINS
                        return (
                          <>
                            {shown.map((s) => {
                              const key = titleKey(p.path, s.id)
                              const title = titleOverrides[key] ?? s.title
                              const isEditing = editing !== null && editing.path === p.path && editing.sid === s.id
                              const commitEdit = () => {
                                if (editing) renameSession(editing.path, editing.sid, draft)
                                setEditing(null)
                              }
                              const cancelEdit = () => setEditing(null)
                              return (
                                <Fragment key={s.id}>
                                <div
                                  className={`session-item ${s.id === activeSessionId ? 'active' : ''} ${s.pinned ? 'pinned' : ''}`}
                                  onClick={() => {
                                    if (isEditing) return
                                    exitHub()
                                    // 点会话行 = 回到它的**主会话**。
                                    // 正在看某个子智能体时，点当前会话原先什么都不做（same id →
                                    // setActiveSession 是空操作），于是「点主会话没反应」——
                                    // 用户 2026-10 报障。这里显式退出子会话视图。
                                    if (s.id === activeSessionId) {
                                      if (focusedAgent) focusAgent(undefined)
                                      return
                                    }
                                    setActiveSession(s.id, p.path)
                                  }}
                                  onDoubleClick={() => { setEditing({ path: p.path, sid: s.id }); setDraft(title) }}
                                  title={t('left.renameHint')}
                                >
                                  {/* 圆点：当前查看=accent 高亮；后台在跑=脉冲；空闲=绿 */}
                                  <span className={`dot ${s.id === activeSessionId ? 'active' : busySids.has(s.id) ? 'running' : 'done'}`} />
                                  {isEditing ? (
                                    <input
                                      className="rename-input"
                                      value={draft}
                                      autoFocus
                                      onChange={(e) => setDraft(e.target.value)}
                                      onKeyDown={(e) => {
                                        // 输入法组合态（中文等用 Enter 选字/上屏）不得提交/取消重命名
                                        if (e.nativeEvent.isComposing || e.keyCode === 229) return
                                        if (e.key === 'Enter') { skipBlur.current = true; commitEdit() }
                                        else if (e.key === 'Escape') { skipBlur.current = true; cancelEdit() }
                                        e.stopPropagation()
                                      }}
                                      onClick={(e) => e.stopPropagation()}
                                      onDoubleClick={(e) => e.stopPropagation()}
                                      onBlur={() => {
                                        if (skipBlur.current) { skipBlur.current = false; return }
                                        commitEdit()
                                      }}
                                    />
                                  ) : (
                                    <>
                                      <span className="session-title">{title}</span>
                                      <button
                                        className={`session-pin ${s.pinned ? 'on' : ''}`}
                                        title={t(s.pinned ? 'left.unpinSession' : 'left.pinSession')}
                                        aria-label={t(s.pinned ? 'left.unpinSession' : 'left.pinSession')}
                                        aria-pressed={s.pinned}
                                        disabled={!s.pinned && pinned.length >= MAX_SESSION_PINS}
                                        onClick={(e) => {
                                          e.stopPropagation()
                                          toggleSessionPin(p.path, s.id)
                                        }}
                                      >
                                        <PinIcon size={12} />
                                      </button>
                                      <button
                                        className="session-del"
                                        title={t('left.deleteSession')}
                                        aria-label={t('left.deleteSession')}
                                        onClick={(e) => {
                                          e.stopPropagation()
                                          setConfirm({ kind: 'session', path: p.path, sid: s.id, name: title })
                                        }}
                                      >
                                        ✕
                                      </button>
                                    </>
                                  )}
                                </div>
                                {/* 该会话派出去的子智能体树（递归：孙智能体在子节点下），紧跟
                                    在会话行下面 —— 视觉上「属于这个会话」。只对**正在查看**的会话
                                    渲染：store 只维护活跃会话的完整视图，别的会话画出来是一棵
                                    空树，比不画更误导。 */}
                                {s.id === activeSessionId && <AgentTree />}
                                </Fragment>
                              )
                            })}
                            {hasMore && (
                              <button
                                className="session-more"
                                onClick={() => setMore((m) => ({ ...m, [p.path]: !expanded }))}
                              >
                                {expanded ? t('left.showLess') : t('left.showMore')}
                                <span className="add-caret">{expanded ? '▾' : '▸'}</span>
                              </button>
                            )}
                          </>
                        )
                      })()}
                    </div>
                  )}
                </div>
              )
            })}

            {/* —— 插件分组（虚拟 Project）——
                插件 = 一个虚拟 Project：图标 + 「插件」徽标，会话行与真实项目完全同构。
                只提供「新建会话 / 删除会话」：pin/files/git/删除工作区对插件 home 都没有
                意义（§4.6 第 6 条）。已卸载（installed:false）单独一个子组，只读 —— 可打开、
                可删会话，不给新建。空则整组隐藏。 */}
            {pluginGroups.installed.length + pluginGroups.uninstalled.length > 0 && (
              <div className="plugin-projects">
                <div className="left-sub-title">{t('left.plugins')}</div>
                {([
                  { key: 'installed', title: pluginGroups.uninstalled.length ? t('left.pluginsInstalled') : '', list: pluginGroups.installed },
                  { key: 'uninstalled', title: pluginGroups.uninstalled.length ? t('left.pluginsUninstalled') : '', list: pluginGroups.uninstalled },
                ] as const).map((group) => group.list.length === 0 ? null : (
                  <div key={group.key} className="plugin-sub">
                    {group.title && <div className="left-sub-title sub">{group.title}</div>}
                    {group.list.map((p) => {
                      const pathOpen = isOpen(p.path)
                      const activeProject = p.path === workspace
                      const readonly = p.installed === false
                      const label = pluginProjectLabel(p)
                      const firstId = p.sessions[0]?.id
                      const openProject = () => {
                        exitHub()
                        setOpen((o) => ({ ...o, [p.path]: true }))
                        // 行点击 = 打开该插件的第一个会话；还没有会话 = 直接新建（已卸载只读，不建）。
                        if (firstId) setActiveSession(firstId, p.path)
                        else if (!readonly) createSessionIn(p.path)
                      }
                      return (
                        <div key={p.path} className={`ws-group plugin-group ${pathOpen ? 'open' : ''}`}>
                          <div
                            className={`ws-head plugin ${activeProject ? 'active' : ''} ${narrow ? 'narrow' : ''}`}
                            title={p.path}
                            onClick={openProject}
                          >
                            <button
                              className="ws-chev"
                              aria-label={pathOpen ? t('left.collapse') : t('left.expand')}
                              aria-expanded={pathOpen}
                              onClick={(e) => {
                                e.stopPropagation()
                                setOpen((o) => ({ ...o, [p.path]: !pathOpen }))
                              }}
                            >
                              <ChevronDown className="chev" />
                            </button>
                            <span className="ws-ic">
                              {p.icon ? <span className="plugin-icon" aria-hidden="true">{p.icon}</span> : <PluginIcon size={13} />}
                            </span>
                            <span className="wname">{label}</span>
                            <span className="plugin-badge">{t('left.pluginBadge')}</span>
                            <span className="ws-count">{p.sessions.length}</span>
                            {!readonly && (
                              <button
                                className="ws-new"
                                title={t('left.newSessionIn')}
                                aria-label={t('left.newSessionIn')}
                                onClick={(e) => { e.stopPropagation(); exitHub(); createSessionIn(p.path) }}
                              >
                                <Plus size={12} />
                              </button>
                            )}
                          </div>
                          {pathOpen && (
                            <div className="ws-body">
                              {p.sessions.length === 0 && (
                                <div className="session-item" style={{ color: 'var(--text-3)', cursor: 'default' }}>{t('left.noSessions')}</div>
                              )}
                              {p.sessions.map((s) => {
                                const title = titleOverrides[titleKey(p.path, s.id)] ?? s.title
                                return (
                                  <Fragment key={s.id}>
                                    <div
                                      className={`session-item ${s.id === activeSessionId ? 'active' : ''}`}
                                      title={title}
                                      onClick={() => {
                                        if (s.id === activeSessionId) {
                                          if (focusedAgent) focusAgent(undefined)
                                          return
                                        }
                                        setActiveSession(s.id, p.path)
                                      }}
                                    >
                                      <span className={`dot ${s.id === activeSessionId ? 'active' : busySids.has(s.id) ? 'running' : 'done'}`} />
                                      <span className="session-title">{title}</span>
                                      <button
                                        className="session-del"
                                        title={t('left.deleteSession')}
                                        aria-label={t('left.deleteSession')}
                                        onClick={(e) => {
                                          e.stopPropagation()
                                          setConfirm({ kind: 'session', path: p.path, sid: s.id, name: title })
                                        }}
                                      >
                                        ✕
                                      </button>
                                    </div>
                                    {s.id === activeSessionId && <AgentTree />}
                                  </Fragment>
                                )
                              })}
                            </div>
                          )}
                        </div>
                      )
                    })}
                  </div>
                ))}
              </div>
            )}
          </div>
          </>
          )}

          {/* 工作区行「⋯」溢出菜单（窄面板形态）：行内 4 个操作在此收齐，行为完全一致 */}
          {moreMenu && (() => {
            const p = projects.find((x) => x.path === moreMenu.path)
            // 虚拟 Project（插件）不出现在这个菜单里：它的行只给「新建会话 / 删除会话」，
            // pin/files/删除工作区对插件 home 都没有意义（§4.6 第 6 条）。
            if (!p || isVirtualProject(p)) return null
            const activeProject = p.path === workspace
            const name = p.path.split('/').pop() || p.path
            return (
              <div
                className="add-ws-menu ws-more-menu"
                role="menu"
                ref={moreMenuRef}
                style={{ top: moreMenu.top, right: moreMenu.right }}
                onClick={(e) => e.stopPropagation()}
              >
                <button
                  role="menuitem"
                  onClick={() => {
                    setMoreMenu(null)
                    exitHub()
                    if (!activeProject) setActiveWorkspace(p.path)
                    setLeftView('files')
                  }}
                >
                  <FolderOpenIcon size={13} /> {t('left.viewFilesIn')}
                </button>
                <button
                  role="menuitem"
                  disabled={!p.pinned && pinnedWorkspaceCount >= MAX_WORKSPACE_PINS}
                  onClick={() => { setMoreMenu(null); toggleWorkspacePin(p.path) }}
                >
                  <PinIcon size={13} /> {t(p.pinned ? 'left.unpinWorkspace' : 'left.pinWorkspace')}
                </button>
                <button role="menuitem" onClick={() => { setMoreMenu(null); exitHub(); createSessionIn(p.path) }}>
                  <Plus size={13} /> {t('left.newSessionIn')}
                </button>
                <div className="menu-sep" />
                <button
                  role="menuitem"
                  className="danger"
                  onClick={() => { setMoreMenu(null); setConfirm({ kind: 'workspace', path: p.path, name }) }}
                >
                  ✕ {t('left.deleteWorkspace')}
                </button>
              </div>
            )
          })()}

          <div className="left-spacer" />
          <div className="left-bottom">
            {/* Hub 入口只保留上方「智能体/群组」section；这里不再放第二个「智能体」按钮。
                files 视图本身有「返回任务」（即回到工作区）入口。 */}
            <button className="nav-item" onClick={openSkills}><Sparkles /> {t('left.skills')}</button>
            <button className="nav-item" onClick={openCron}><Clock /> {t('left.cron')}</button>
            <button className="nav-item" onClick={openSettings}><Gear /> {t('left.settings')}</button>
          </div>
        </div>
      ) : (
        <div className="rail left-rail">
          <div className="rail-logo" title="HAI"><HaiLogo height={26} /></div>
          {/* 常驻展开钮（与展开态品牌行的 PanelLeft 同款，收起后不再消失） */}
          <button className="rbtn" data-tip={t('topbar.toggleLeft')} aria-label={t('topbar.toggleLeft')} onClick={toggleLeft}><PanelLeft /></button>
          {/* 折叠态智能体入口（P4-UI-1）：点击 = 展开左栏并进入 Hub。
              放在顶部而不是底部，与展开态 Hub section 的位置对应。 */}
          <button
            className={`rbtn ${hubOpen ? 'on' : ''} ${hubOpen && hubAccent ? 'agent-active' : ''}`}
            style={hubAccentStyle}
            data-tip={t('hub.title')}
            aria-label={t('hub.title')}
            onClick={() => { toggleLeft(); openHub() }}
          >
            <Bot />
          </button>
          {/* 建会话 / 加工作区排在「工作区」图标之上：二者是入口动作（与展开态 .new-session-btn /
              .add-ws-btn 的上下顺序一致——展开时它们也在工作区列表之上），「工作区」图标只是
              展开当前列表的开关，放在动作之后。图标改为语义化同族（气泡+ / 文件夹+），
              不再用含义模糊的裸加号，且与展开态两个入口一一对应。 */}
          <button
            className="rbtn accent"
            data-tip={t(workspace ? 'left.newSessionHere' : 'left.newSessionFirst')}
            aria-label={t('left.newSession')}
            onClick={() => void newSession()}
          >
            <SessionPlus />
          </button>
          <div className="add-ws rail-add-ws">
            <button
              className="rbtn"
              data-tip={t('left.addWorkspace')}
              aria-label={t('left.addWorkspace')}
              aria-expanded={addOpen}
              aria-haspopup="menu"
              ref={addBtnRef}
              onClick={() => setAddOpen((v) => !v)}
            >
              <FolderPlus />
            </button>
            {/* 菜单 fixed 定位：图标条 .left 的 overflow:hidden 会裁掉绝对定位弹层 */}
            {addOpen && !leftExpanded && (
              <div
                className="add-ws-menu rail-menu"
                role="menu"
                ref={addMenuRef}
                style={railMenuPos ? { top: railMenuPos.top, left: railMenuPos.left } : { visibility: 'hidden' }}
              >
                {addWorkspaceItems}
              </div>
            )}
          </div>
          <button
            className={`rbtn ${hubOpen ? '' : 'on'}`}
            data-tip={t('left.workspaces')}
            aria-label={t('left.workspaces')}
            aria-pressed={!hubOpen}
            onClick={() => {
              // 折叠态点「工作区」= 回到工作区并展开左栏；Hub 开着时先退出 Hub，
              // 否则 Bot 与工作区两个入口会同时是高亮态，且中心仍停在 Hub。
              if (hubOpen) closeHub()
              toggleLeft()
            }}
          >
            <SessionIcon />
          </button>
          <div className="rail-spacer" />
          <div className="rail-bottom">
            <button className="rbtn" data-tip={t('left.skills')} onClick={() => { toggleLeft(); openSkills() }} aria-label={t('left.skills')}><Sparkles /></button>
            <button className="rbtn" data-tip={t('left.cron')} onClick={() => { toggleLeft(); openCron() }} aria-label={t('left.cron')}><Clock /></button>
            <button className="rbtn" data-tip={t('left.settings')} onClick={openSettings} aria-label={t('left.settings')}><Gear /></button>
          </div>
        </div>
      )}
      </aside>
      {confirm && (
        <ConfirmModal
          title={t(confirm.kind === 'session' ? 'left.deleteSession' : 'left.deleteWorkspace')}
          message={`「${confirm.name}」${t(confirm.kind === 'session' ? 'left.deleteSessionMsg' : 'left.deleteWorkspaceMsg')}`}
          confirmLabel={t('left.delete')}
          cancelLabel={t('left.cancel')}
          danger
          onConfirm={() => {
            if (confirm.kind === 'session') deleteSession(confirm.path, confirm.sid!)
            else deleteWorkspace(confirm.path)
            setConfirm(null)
          }}
          onCancel={() => setConfirm(null)}
        />
      )}
    </>
  )
}
