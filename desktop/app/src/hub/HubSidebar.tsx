import { useEffect, useRef, useState } from 'react'
import type { CSSProperties } from 'react'
import { Bot, ChevronLeft, Plus, SearchIcon } from '../components/icons'
import { useT } from '../i18n'
import AgentSigil from './AgentSigil'
import AgentStack from './AgentStack'
import { planStack, stateResolver } from './avatar'
import { agentStackSummary } from './avatarLabels'
import { useAgentState, usePendingAgents } from './useAgentState'
import { accentOfAgent, accentOfGroup, contrastText } from './agentAccent'
import {
  HUB_SEARCH_DEBOUNCE_MS, normalizeSearchQuery, searchHitKey, searchScopeLabel,
} from './search'
import { formatRelativeTime, groupPreview, unreadBadge } from './conversation'
import { useHubStore } from './useHubStore'
import GroupEditor from './GroupEditor'
import type { HubAgent, HubSearchScope } from './types'
import './hub.css'

// 左栏「智能体」段（P4-UI-1）：常驻 App 左栏，位于 Persona 与「工作区」之间。
//
// 这里只负责「入口 + 全历史搜索 + 列表」：选中后由 store 打开 Hub，
// 中栏渲染会话、右栏渲染详情。列表自身限高（~38vh）内部滚动，工作区树仍在下方。
export default function HubSidebar() {
  const t = useT()
  const agents = useHubStore((s) => s.agents)
  const dm = useHubStore((s) => s.dm)
  const activeId = useHubStore((s) => s.activeId)
  const running = useHubStore((s) => s.running)
  const pendingIds = usePendingAgents()
  const stateOf = stateResolver({ running, pending: pendingIds })
  const groups = useHubStore((s) => s.groups)
  const groupsLoading = useHubStore((s) => s.groupsLoading)
  const groupInfo = useHubStore((s) => s.groupInfo)
  const groupConversations = useHubStore((s) => s.groupConversations)
  const activeGroupId = useHubStore((s) => s.activeGroupId)
  const selectionNonce = useHubStore((s) => s.selectionNonce)
  const hubOpen = useHubStore((s) => s.open)
  const openAgent = useHubStore((s) => s.openAgent)
  const openGroup = useHubStore((s) => s.openGroup)
  const closeHub = useHubStore((s) => s.closeHub)
  const openEditor = useHubStore((s) => s.openEditor)
  const refresh = useHubStore((s) => s.refresh)
  const loadGroups = useHubStore((s) => s.loadGroups)
  // 全历史搜索（P4-04b-1）：输入框是本地 state（debounce 只认用户输入），
  // 结果/游标在 store 里（切面板不丢，翻页要用）。
  const runSearch = useHubStore((s) => s.search)
  const searchMore = useHubStore((s) => s.searchMore)
  const clearSearch = useHubStore((s) => s.clearSearch)
  const openSearchHit = useHubStore((s) => s.openSearchHit)
  const searchScope = useHubStore((s) => s.searchScope)
  const setSearchScope = useHubStore((s) => s.setSearchScope)
  const searchHits = useHubStore((s) => s.searchHits)
  const searchHasMore = useHubStore((s) => s.searchHasMore)
  const searchLoading = useHubStore((s) => s.searchLoading)
  const searchError = useHubStore((s) => s.searchError)
  const searchQuery = useHubStore((s) => s.searchQuery)

  const [tab, setTab] = useState<'agents' | 'groups'>('agents')
  // 群组弹层（新建/编辑）：与 AgentEditor 分开（那个是 store 里的全局弹层）。
  // P6-17 起开关放 store：右栏 inspector 的「编辑群组」与左栏 ＋ 共用同一份状态。
  const groupEditor = useHubStore((s) => s.groupEditor)
  const openGroupEditor = useHubStore((s) => s.openGroupEditor)
  const closeGroupEditor = useHubStore((s) => s.closeGroupEditor)
  const [searchInput, setSearchInput] = useState('')
  const [searchPanel, setSearchPanel] = useState(false)
  // 搜索命中 / 深链打开目标后，自动把左栏切到对应 scope；用户随后手动切 tab
  // 不再被同一目标反复拉回（lastAutoTarget 只在「当前目标变了」时触发）。
  const lastAutoTarget = useRef('')
  const lastAutoNonce = useRef(-1)
  useEffect(() => {
    if (!hubOpen) {
      lastAutoTarget.current = ''
      lastAutoNonce.current = -1
      return
    }
    const key = activeGroupId ? `group:${activeGroupId}` : activeId ? `dm:${activeId}` : ''
    if (!key) return
    // 同一目标被深链再次打开时 nonce 会变：必须切回对应 scope，而不是被
    // 「target 没变」提前 return（用户在另一个 tab 上时尤其明显）。
    if (key === lastAutoTarget.current && selectionNonce === lastAutoNonce.current) return
    lastAutoTarget.current = key
    lastAutoNonce.current = selectionNonce
    setTab(activeGroupId ? 'groups' : 'agents')
  }, [hubOpen, activeGroupId, activeId, selectionNonce])

  // 左栏常驻：不打开 Hub 也要有智能体/群组可选（旧实现只在 openHub 时拉列表）。
  // 只拉列表，不打开 Hub、不改未读；点击行时才由 openAgent/openGroup 进入。
  useEffect(() => {
    void refresh()
    void loadGroups()
  }, [refresh, loadGroups])

  // 250ms debounce：只在用户停止输入后发一次 hub_search；旧响应由 store 的代际丢弃。
  useEffect(() => {
    const q = normalizeSearchQuery(searchInput)
    if (!q) {
      clearSearch()
      return
    }
    const timer = window.setTimeout(() => {
      setSearchPanel(true)
      void runSearch(q, searchScope)
    }, HUB_SEARCH_DEBOUNCE_MS)
    return () => window.clearTimeout(timer)
  }, [searchInput, searchScope, runSearch, clearSearch])

  const closeSearch = (): void => {
    setSearchInput('')
    setSearchPanel(false)
    clearSearch()
  }

  const showingSearch = searchPanel && !!searchInput.trim()

  return (
    <section className="hub-side" aria-label={t('hub.agents')}>
      <header className="hub-side-head">
        <span className="hub-side-title">
          <Bot size={13} /> {t('hub.title')}
        </span>
        <span className="hub-side-actions">
          {hubOpen ? (
            <button
              className="hub-side-back"
              onClick={closeHub}
              title={t('hub.backToWorkspaces')}
              aria-label={t('hub.backToWorkspaces')}
            >
              <ChevronLeft size={11} /> <span className="hub-side-back-label">{t('hub.back')}</span>
            </button>
          ) : null}
          <button
            className="hub-side-add"
            onClick={() => (tab === 'groups' ? openGroupEditor(null) : openEditor(null))}
            title={tab === 'groups' ? '新建群组' : t('hub.newAgent')}
            aria-label={tab === 'groups' ? '新建群组' : t('hub.newAgent')}
          >
            <Plus size={13} />
          </button>
        </span>
      </header>

      <div className="hub-side-search" role="search">
        <div className="hub-side-search-box">
          <SearchIcon size={12} />
          <input
            className="hub-side-search-input"
            type="search"
            value={searchInput}
            placeholder="搜索全部会话…"
            aria-label="搜索 Hub 全部历史消息"
            onChange={(e) => {
              setSearchInput(e.target.value)
              setSearchPanel(true)
            }}
            onFocus={() => {
              if (searchInput.trim()) setSearchPanel(true)
            }}
            onKeyDown={(e) => {
              if (e.key === 'Escape') {
                e.stopPropagation()
                closeSearch()
              }
            }}
          />
          {searchInput || searchQuery ? (
            <button className="hub-link hub-side-search-clear" onClick={closeSearch} aria-label="清空搜索">×</button>
          ) : null}
        </div>
        <div className="hub-side-scopes" role="tablist" aria-label="搜索范围">
          {(['all', 'group', 'dm'] as HubSearchScope[]).map((sc) => (
            <button
              key={sc}
              role="tab"
              aria-selected={searchScope === sc}
              className={searchScope === sc ? 'on' : ''}
              onClick={() => setSearchScope(sc)}
            >
              {searchScopeLabel(sc)}
            </button>
          ))}
        </div>
      </div>

      {showingSearch ? (
        <div className="hub-side-results" aria-label="搜索结果" aria-busy={searchLoading}>
          {searchError ? (
            <div className="hub-error" role="alert">
              <span>{searchError}</span>
              <button className="hub-link" onClick={() => void runSearch(searchInput, searchScope)}>重试</button>
            </div>
          ) : null}
          {searchLoading && searchHits.length === 0 ? <div className="hub-side-empty">搜索中…</div> : null}
          {!searchLoading && !searchError && searchHits.length === 0 ? (
            <div className="hub-side-empty">没有匹配的消息</div>
          ) : null}
          {searchHits.map((hit) => (
            <button
              key={searchHitKey(hit)}
              className={`hub-search-hit ${hit.owner_only ? 'owner-only' : ''}`}
              onClick={() => {
                // 先收起结果面板再打开会话：跳转后要高亮的是时间线，不是结果行。
                // 同时把左栏 tab 切到命中的 scope（群组命中不能还停在智能体列表）。
                setSearchPanel(false)
                setSearchInput('')
                setTab(hit.scope === 'group' ? 'groups' : 'agents')
                clearSearch()
                void openSearchHit(hit)
              }}
              title={hit.snippet}
            >
              <span className="hub-search-hit-top">
                <span className="hub-search-hit-name">{hit.name || hit.group_id || hit.agent_id}</span>
                <span className={`hub-chip scope-${hit.scope}`}>{hit.scope === 'group' ? '群' : '私聊'}</span>
                {hit.owner_only ? <span className="hub-chip owner-only">仅群主可见</span> : null}
                <span className="hub-search-hit-time">{formatRelativeTime(hit.created_at)}</span>
              </span>
              <span className="hub-search-hit-snippet">{hit.snippet}</span>
              <span className="hub-search-hit-meta">
                {hit.sender || ''}
                {hit.seq > 0 ? ` · #${hit.seq}` : ''}
              </span>
            </button>
          ))}
          {searchHasMore ? (
            <button className="hub-search-more" onClick={() => void searchMore()} disabled={searchLoading}>
              {searchLoading ? '加载中…' : '更早的结果'}
            </button>
          ) : null}
        </div>
      ) : (
        <>
          <div className="hub-side-tabs" role="tablist" aria-label="智能体或群组">
            <button
              role="tab"
              aria-selected={tab === 'agents'}
              className={tab === 'agents' ? 'on' : ''}
              onClick={() => setTab('agents')}
            >
              智能体 <span className="hub-side-count">{agents.length}</span>
            </button>
            <button
              role="tab"
              aria-selected={tab === 'groups'}
              className={tab === 'groups' ? 'on' : ''}
              onClick={() => setTab('groups')}
            >
              群组 <span className="hub-side-count">{groups.length}</span>
            </button>
          </div>

          {tab === 'groups' ? (
            groupsLoading ? <div className="hub-side-empty">加载中…</div> :
            groups.length === 0 ? (
              <div className="hub-side-empty">
                <p>还没有群组</p>
                <span>在 Hub 里把多个智能体拉进同一个群。</span>
                <button className="hub-side-empty-add" onClick={() => openGroupEditor(null)}>
                  <Plus size={12} /> 新建群组
                </button>
              </div>
            ) : (
              <div className="hub-side-list">
                {groups.map((g) => {
                  const info = groupInfo[g.id] ?? {}
                  const unread = info.unread ?? 0
                  const memberCount = (g.members ?? []).length
                  // 预览优先任务结论（「做完了什么」），否则最后一条消息；都没有才退回成员/策略。
                  const preview = groupPreview(info) || `${memberCount} 名成员 · ${g.policy?.mode ?? 'ask'}`
                  const time = formatRelativeTime(info.last_at)
                  const conv = groupConversations[g.id]
                  const groupRunning =
                    conv?.status === 'running' ||
                    ['queued', 'dispatched', 'running'].includes(info.last_task_status ?? '')
                  const groupAccent = accentOfGroup(g, agents)
                  // 叠放用全部成员：2–4 全显示、≥5 前 3 +N 由 planStack 决定。
                  const stackMembers = (g.members ?? []).map((m) => {
                    const member = agents.find((a) => a.id === m.agent_id)
                    return {
                      id: m.agent_id,
                      name: member?.name ?? m.agent_id,
                      responsibility: member?.role,
                      state: stateOf(m.agent_id, member?.deleted),
                    }
                  })
                  const stackPlan = planStack(stackMembers, { groupRunning })
                  const stackSummary = agentStackSummary(t, stackPlan.counts, stackPlan.state)
                  return (
                    <button
                      key={g.id}
                      className={`hub-side-row hub-group-row ${g.id === activeGroupId ? 'on' : ''}`}
                      style={rowAccentStyle(groupAccent)}
                      onClick={() => void openGroup(g.id)}
                      aria-current={g.id === activeGroupId ? 'true' : undefined}
                      aria-label={`${g.name}，${memberCount} 名成员，${stackSummary}${unread > 0 ? `，${unreadBadge(unread)} 条未读` : '，没有未读'}，${preview}`}
                    >
                      <AgentStack
                        members={stackMembers}
                        size={28}
                        groupRunning={groupRunning}
                        label={null}
                      />
                      <span className="hub-side-text">
                        <span className="hub-side-top">
                          <span className="hub-name">{g.name}</span>
                          {unread > 0 ? (
                            <span className="hub-unread" style={{ background: groupAccent, color: contrastText(groupAccent) }}>
                              {unreadBadge(unread)}
                            </span>
                          ) : null}
                        </span>
                        <span className="hub-side-meta">
                          <span className="hub-preview" title={preview}>{preview}</span>
                          {time ? <span className="hub-side-time">{time}</span> : null}
                        </span>
                      </span>
                    </button>
                  )
                })}
              </div>
            )
          ) : agents.length === 0 ? (
            <div className="hub-side-empty">
              <p>{t('hub.noAgents')}</p>
              <button className="hub-side-empty-add" onClick={() => openEditor(null)}>
                <Plus size={12} /> {t('hub.newAgent')}
              </button>
            </div>
          ) : (
            <div className="hub-side-list">
              {agents.map((a) => (
                <HubRow
                  key={a.id}
                  agent={a}
                  preview={dm[a.id]?.last_text}
                  time={formatRelativeTime(dm[a.id]?.last_at)}
                  unread={dm[a.id]?.unread ?? 0}
                  running={running[a.id] === true}
                  active={a.id === activeId}
                  onClick={() => void openAgent(a.id)}
                />
              ))}
            </div>
          )}
        </>
      )}

      {groupEditor.open ? (
        <GroupEditor
          group={groupEditor.groupId ? groups.find((g) => g.id === groupEditor.groupId) ?? null : null}
          onClose={closeGroupEditor}
        />
      ) : null}
    </section>
  )
}

function HubRow({
  agent,
  preview,
  time,
  unread,
  running,
  active,
  onClick,
}: {
  agent: HubAgent
  preview?: string
  time?: string
  unread: number
  running: boolean
  active: boolean
  onClick: () => void
}) {
  const accent = accentOfAgent(agent)
  const state = useAgentState(agent.id, agent.deleted)
  return (
    <button
      className={`hub-side-row ${active ? 'on' : ''} ${running ? 'running' : ''}`}
      style={rowAccentStyle(accent)}
      onClick={onClick}
      aria-current={active ? 'true' : undefined}
    >
      <AgentSigil
        agentId={agent.id}
        name={agent.name}
        responsibility={agent.role}
        state={state}
        size={44}
      />
      <span className="hub-side-text">
        <span className="hub-side-top">
          <span className="hub-name">{agent.name}</span>
          {unread > 0 ? (
            <span className="hub-unread" style={{ background: accent, color: contrastText(accent) }}>
              {unreadBadge(unread)}
            </span>
          ) : null}
        </span>
        <span className="hub-side-meta">
          <span className="hub-preview">{preview || agent.role || '—'}</span>
          {time ? <span className="hub-side-time">{time}</span> : null}
        </span>
      </span>
    </button>
  )
}

/** 行身份色通过 CSS 变量下发：hover/active/指示条共享同一支色，不逐个写内联。 */
function rowAccentStyle(accent: string): CSSProperties {
  return { ['--row-accent']: accent } as CSSProperties
}
