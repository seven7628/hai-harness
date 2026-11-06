import { create } from 'zustand'
import { getTransport } from '../transport'
import { hubCommand } from './api'
import type { HubAgent, HubAlert, HubDMInfo, HubGroup, HubHistoryPage, HubMessage, HubGroupActivity, HubNeed, HubPage, HubRound, HubRunEntry, HubTask } from './types'
import { alertKey } from './alerts'

// Agent Hub 的前端状态（§8.2 Hub Rail + §8.4 DM 视图）。
//
// 独立于 useAppStore：Hub 的 session_id（hub.<agent>.dm）与工作区会话是两套语义；
// 混进主 store 会让导航/右栏/恢复逻辑打架（主 store 也已在 wire() 里过滤 hub_* 事件，
// 避免 views/busySids 里留下永不清理的 hub 正文）。
//
// 这里的每条不变量都对应 P1-08 复审实测到的一个缺陷：
//   - select 用 token 防「先点 A 后点 B，A 的迟到响应覆盖 B」（B4）
//   - 只认根 run 的 chunk/agent_start/agent_end（B5：子 agent 会串进流式、提前熄运行点）
//   - 发送失败保留原文和失败气泡（B2：之前气泡与输入框一起清空）
//   - live 在根 run 结束时清空、重进会话时重置（B3：半截气泡残留）

interface HubState {
  open: boolean
  agents: HubAgent[]
  dm: Record<string, HubDMInfo>
  activeId: string | null
  profile: HubAgent | null
  sid: string | null
  messages: HubMessage[]
  hasMore: boolean
  loading: boolean
  loadingOlder: boolean
  sending: boolean
  error: string | null
  /** 一次性提示（回滚成功等），与 error 分开显示。 */
  notice: string | null
  /** 当前 DM 的流式正文（只累计根 run 的 content_chunk）。 */
  live: string
  /** 当前根 run 的 run_id（子 agent chunk 的 run_id 不同，据此过滤）。 */
  rootRunId: string | null
  /** agent_id → 是否在跑（左栏状态环）。 */
  running: Record<string, boolean>
  /** 每个 agent 未发送的草稿（切 surface 不丢）。 */
  drafts: Record<string, string>
  /** 上一次发送失败的原文（供重试；失败气泡同时保留在列表里）。 */
  failedText: string | null
  /** 任务板/Needs you/会话信息抽屉。 */
  inspector: 'tasks' | 'needs' | 'session' | null
  tasks: HubTask[]
  tasksLoading: boolean
  needs: HubNeed[]
  needsLoading: boolean
  /** Hub 全局告警（P2.5-05）：恢复失败/预算/推送失败等，按 envelope 去重。 */
  alerts: HubAlert[]
  /** DM 右栏视图：Chat = 对话，Run = 工具/压缩运行日志。 */
  view: 'chat' | 'run'
  /** 当前 DM 的 Run 日志（只收该 session 的根/子工具事件）。 */
  runLog: HubRunEntry[]
  /** 系统动作行（压缩回执/清空等），与用户气泡同列显示。 */
  actions: HubMessage[]
  /** 群组（P3a-08）：列表/当前群/时间线/最近一轮路由。 */
  groups: HubGroup[]
  groupsLoading: boolean
  activeGroupId: string | null
  groupMessages: HubMessage[]
  groupRounds: HubRound[]
  groupActivity: HubGroupActivity[]
  groupSending: boolean

  openHub(): void
  closeHub(): void
  refresh(): Promise<void>
  select(agentID: string): Promise<void>
  clearSelection(): void
  loadOlder(): Promise<void>
  send(text: string): Promise<boolean>
  retry(): Promise<boolean>
  setDraft(agentID: string, text: string): void
  clearError(): void
  clearNotice(): void
  dismissAlert(id: string): void
  setInspector(panel: 'tasks' | 'needs' | 'session' | null): void
  setView(view: 'chat' | 'run'): void
  loadGroups(): Promise<void>
  selectGroup(groupID: string): Promise<void>
  loadGroupActivity(groupID?: string): Promise<void>
  clearGroup(): void
  sendGroup(text: string): Promise<boolean>
  loadTasks(): Promise<void>
  loadNeeds(): Promise<void>
  cancelTask(taskID: string): Promise<void>
  retryTask(taskID: string): Promise<void>
  resolveNeed(needID: string, resolution: string, answers?: Record<string, string>): Promise<void>
  clearContext(): Promise<void>
  handleEvent(line: string): void
}

let optimisticSeq = 0 // 负数临时 seq，避免与真实 seq（正数）混淆
let groupSendGen = 0 // 群发送代际：切群/新一轮发送后，旧响应不得改状态
let groupSelectGen = 0 // 选群代际：A→B→A 时旧响应不得覆盖新状态

function roundHasAction(r: HubRound | null | undefined): boolean {
  return !!r && (!!r.mention_all || (r.targets ?? []).length > 0 || (r.dropped ?? 0) > 0)
}

function upsertRound(list: HubRound[], round: HubRound | null | undefined): HubRound[] {
  if (!roundHasAction(round) || !round) return list
  const next = list.filter((r) => r.id !== round.id)
  next.push(round)
  next.sort((a, b) => (a.trigger_seq ?? 0) - (b.trigger_seq ?? 0))
  return next
}

export const useHubStore = create<HubState>((set, get) => ({
  open: false,
  agents: [],
  dm: {},
  activeId: null,
  profile: null,
  sid: null,
  messages: [],
  hasMore: false,
  loading: false,
  loadingOlder: false,
  sending: false,
  error: null,
  notice: null,
  live: '',
  rootRunId: null,
  running: {},
  drafts: {},
  failedText: null,
  inspector: null,
  tasks: [],
  tasksLoading: false,
  needs: [],
  needsLoading: false,
  alerts: [],
  view: 'chat',
  runLog: [],
  actions: [],
  groups: [],
  groupsLoading: false,
  activeGroupId: null,
  groupMessages: [],
  groupRounds: [],
  groupActivity: [],
  groupSending: false,

  openHub() {
    set({ open: true })
    void get().refresh()
    void get().loadTasks()
    void get().loadNeeds()
    void get().loadGroups()
  },

  closeHub() {
    set({ open: false })
  },

  async refresh() {
    // 不碰 loading：每次发送完都刷新列表，共用 loading 会让消息区闪「加载中」。
    const r = await hubCommand<{ agents?: HubAgent[]; dm?: Record<string, HubDMInfo> }>('hub_agent_list', {})
    if (!r.ok) {
      set({ error: r.error || '读取智能体列表失败' })
      return
    }
    const agents = r.data?.agents ?? []
    const dm = r.data?.dm ?? {}
    // 排序：有未读优先 → 最近有消息 → 名字（左栏是「谁在等我」，不是通讯录）。
    const rank = (a: HubAgent): [number, number, string] => {
      const info = dm[a.id] ?? {}
      return [info.unread && info.unread > 0 ? 0 : 1, -(Date.parse(String(info.last_at ?? '')) || 0), a.name]
    }
    const sorted = [...agents].sort((a, b) => {
      const ra = rank(a)
      const rb = rank(b)
      return ra[0] - rb[0] || ra[1] - rb[1] || ra[2].localeCompare(rb[2])
    })
    set({ agents: sorted, dm })
  },

  async select(agentID) {
    if (!agentID) return
    groupSendGen++
    groupSelectGen++
    set((s) => ({
      activeId: agentID,
      activeGroupId: null,
      groupMessages: [],
      groupActivity: [],
      groupRounds: [],
      groupSending: false,
      messages: [],
      hasMore: false,
      live: '',
      rootRunId: null,
      runLog: [],
      actions: [],
      error: null,
      loading: true,
      // 离开期间的事件没人接：重开会话先假设空闲，运行中的话下面的 chunk 会把它点亮。
      running: { ...s.running, [agentID]: false },
    }))
    const r = await hubCommand<HubPage>('hub_open', {
      target: { kind: 'dm', agent_id: agentID },
      limit: 50,
    })
    // 竞态：A 的响应可能晚于 B。只要用户已经切走，这一份就丢掉。
    if (get().activeId !== agentID) return
    if (!r.ok) {
      set({ loading: false, error: r.error || '打开对话失败' })
      return
    }
    set({
      loading: false,
      profile: r.data?.agent ?? null,
      sid: r.data?.session_id ?? null,
      messages: r.data?.messages ?? [],
      hasMore: r.data?.has_more === true,
    })
    // 打开过 = 已读：左栏未读点立即消掉（bridge 已推进游标，这里同步本地投影）。
    set((s) => {
      const dm = { ...s.dm }
      const info = { ...(dm[agentID] ?? {}) }
      info.unread = 0
      dm[agentID] = info
      return { dm }
    })
  },

  clearSelection() {
    set({ activeId: null, profile: null, sid: null, messages: [], hasMore: false, live: '', rootRunId: null, failedText: null })
  },

  async loadOlder() {
    const { activeId, messages, hasMore, loadingOlder } = get()
    if (!activeId || !hasMore || loadingOlder || messages.length === 0) return
    const before = messages[0].seq
    set({ loadingOlder: true })
    const r = await hubCommand<HubHistoryPage>('hub_history', {
      agent_id: activeId,
      before_seq: before,
      limit: 50,
    })
    if (get().activeId !== activeId) return
    if (!r.ok) {
      set({ loadingOlder: false, error: r.error || '加载更早消息失败' })
      return
    }
    const older = r.data?.messages ?? []
    set((s) => {
      const pageSeqs = new Set(older.map((m) => m.seq))
      // 保留：本页之前的真实消息 + 所有乐观/失败气泡（seq<=0，不能被翻页滤掉）。
      const keep = s.messages.filter((m) => m.seq > 0 && !pageSeqs.has(m.seq))
      return {
        loadingOlder: false,
        hasMore: r.data?.has_more === true,
        messages: [...older, ...keep].sort((a, b) => a.seq - b.seq),
      }
    })
  },

  async send(text) {
    const { activeId, sending } = get()
    const body = text.trim()
    if (!activeId || !body || sending) return false
    const clientMsgId = `ui-${Date.now()}-${++optimisticSeq}`
    const optimistic: HubMessage = {
      seq: -optimisticSeq,
      sender: { kind: 'human' },
      text: body,
      pending: true,
      clientMsgId,
    }
    set((s) => ({ messages: [...s.messages, optimistic], sending: true, error: null, notice: null }))
    const r = await hubCommand<{ seq?: number; deduplicated?: boolean }>('hub_send', {
      target: { kind: 'dm', agent_id: activeId },
      text: body,
      client_msg_id: clientMsgId,
    })
    if (!r.ok) {
      // 失败：气泡标 failed（可重试），原文留在 failedText 供输入框回填 —— 不能两头都清。
      set((s) => ({
        sending: false,
        error: r.error || '发送失败',
        failedText: body,
        messages: s.messages.map((m) => (m.clientMsgId === clientMsgId ? { ...m, pending: false, failed: true } : m)),
      }))
      return false
    }
    const seq = Number(r.data?.seq ?? 0)
    if (seq > 0) {
      // hub_message 事件通常已经先到（emit 在 resp 之前）；这里兜底把乐观消息就地落成真实 seq。
      set((s) => ({
        messages: s.messages.map((m) => (m.clientMsgId === clientMsgId ? { ...m, seq, pending: false, failed: false } : m)),
      }))
    }
    set({ sending: false, failedText: null })
    void get().refresh()
    return true
  },

  async retry() {
    const text = get().failedText
    if (!text) return false
    set({ failedText: null, messages: get().messages.filter((m) => !m.failed) })
    return get().send(text)
  },

  setDraft(agentID, text) {
    if (!agentID) return
    set((s) => ({ drafts: { ...s.drafts, [agentID]: text } }))
  },

  clearError() {
    set({ error: null })
  },

  clearNotice() {
    set({ notice: null })
  },

  dismissAlert(id) {
    set((s) => ({ alerts: s.alerts.filter((a) => a.id !== id) }))
  },

  setInspector(panel) {
    set({ inspector: panel })
    if (panel === 'tasks') void get().loadTasks()
    if (panel === 'needs') void get().loadNeeds()
  },

  setView(view) {
    set({ view })
  },

  async loadTasks() {
    set({ tasksLoading: true })
    const r = await hubCommand<{ tasks?: HubTask[] }>('hub_task_list', { limit: 100 })
    if (!r.ok) {
      set({ tasksLoading: false, error: r.error || '读取任务失败' })
      return
    }
    set({ tasksLoading: false, tasks: r.data?.tasks ?? [] })
  },

  async loadNeeds() {
    set({ needsLoading: true })
    const r = await hubCommand<{ needs?: HubNeed[] }>('hub_need_list', { status: 'pending' })
    if (!r.ok) {
      set({ needsLoading: false, error: r.error || '读取 Needs you 失败' })
      return
    }
    set({ needsLoading: false, needs: r.data?.needs ?? [] })
  },

  async cancelTask(taskID) {
    const r = await hubCommand<{ task?: HubTask }>('hub_task_cancel', { task_id: taskID })
    if (!r.ok) {
      set({ error: r.error || '取消失败' })
      return
    }
    if (r.data?.task) {
      set((s) => ({ tasks: s.tasks.map((t) => (t.id === taskID ? (r.data!.task as HubTask) : t)) }))
    }
    void get().loadTasks()
    set({ notice: '已请求取消任务' })
  },

  async retryTask(taskID) {
    const r = await hubCommand<{ task?: HubTask }>('hub_task_retry', { task_id: taskID })
    if (!r.ok) {
      set({ error: r.error || '重试失败' })
      return
    }
    set({ notice: '已创建重试任务' })
    void get().loadTasks()
  },

  async resolveNeed(needID, resolution, answers) {
    const r = await hubCommand<{ need?: HubNeed }>('hub_need_resolve', { need_id: needID, resolution, answers })
    if (!r.ok) {
      set({ error: r.error || '处理失败' })
      return
    }
    if (r.data?.need) {
      const resolved = r.data.need
      set((s) => ({ needs: s.needs.filter((n) => n.id !== needID), tasks: s.tasks }))
      if (resolved.status === 'resolved') set({ notice: '已处理' })
    }
    void get().loadNeeds()
  },

  async clearContext() {
    const id = get().activeId
    if (!id) return
    const r = await hubCommand<{ session_id?: string }>('hub_clear', { target: { kind: 'dm', agent_id: id } })
    if (!r.ok) {
      set({ error: r.error || '清空上下文失败' })
      return
    }
    set({ live: '', rootRunId: null, runLog: [], actions: [], notice: '上下文已清空' })
    if (id) void get().select(id)
  },

  async loadGroups() {
    set({ groupsLoading: true })
    const r = await hubCommand<{ groups?: HubGroup[] }>('hub_group_list', {})
    if (!r.ok) {
      set({ groupsLoading: false, error: r.error || '读取群组失败' })
      return
    }
    set({ groupsLoading: false, groups: r.data?.groups ?? [] })
  },

  async selectGroup(groupID) {
    if (!groupID) return
    groupSendGen++
    const gen = ++groupSelectGen
    set({
      activeGroupId: groupID, activeId: null, profile: null, sid: null,
      groupMessages: [], groupActivity: [], groupRounds: [], groupSending: false,
      messages: [], live: '', rootRunId: null,
      error: null, loading: true,
    })
    const r = await hubCommand<{ group?: HubGroup }>('hub_group_get', { group_id: groupID })
    if (get().activeGroupId !== groupID || groupSelectGen !== gen) return
    if (!r.ok) {
      set({ loading: false, error: r.error || '打开群组失败' })
      return
    }
    const h = await hubCommand<{ messages?: HubMessage[] }>('hub_group_history', { group_id: groupID, limit: 50 })
    if (get().activeGroupId !== groupID || groupSelectGen !== gen) return
    const a = await hubCommand<{ activity?: HubGroupActivity[] }>('hub_group_activity', { group_id: groupID })
    if (get().activeGroupId !== groupID || groupSelectGen !== gen) return
    set({
      loading: false,
      groupMessages: h.ok ? (h.data?.messages ?? []) : [],
      groupActivity: a.ok ? (a.data?.activity ?? []) : [],
      groups: get().groups.map((g) => (g.id === groupID ? (r.data?.group ?? g) : g)),
    })
  },

  async loadGroupActivity(groupID) {
    const gid = groupID ?? get().activeGroupId
    if (!gid) return
    const gen = groupSelectGen
    const a = await hubCommand<{ activity?: HubGroupActivity[] }>('hub_group_activity', { group_id: gid })
    // 同一群 A→B→A 也要用代际拒绝旧包，否则新读会被旧快照覆盖。
    if (get().activeGroupId !== gid || groupSelectGen !== gen) return
    if (a.ok) set({ groupActivity: a.data?.activity ?? [] })
  },

  clearGroup() {
    groupSendGen++
    set({ activeGroupId: null, groupMessages: [], groupActivity: [], groupRounds: [], groupSending: false })
  },

  async sendGroup(text) {
    const { activeGroupId, groupSending } = get()
    const body = text.trim()
    if (!activeGroupId || !body || groupSending) return false
    const gen = ++groupSendGen
    const clientMsgId = `g-${Date.now()}-${++optimisticSeq}`
    set((s) => ({
      groupSending: true,
      groupMessages: [...s.groupMessages, { seq: -optimisticSeq, sender: { kind: 'human' }, text: body, pending: true, clientMsgId }],
    }))
    const r = await hubCommand<{ seq?: number; round?: HubRound }>('hub_group_send', {
      group_id: activeGroupId, text: body, client_msg_id: clientMsgId,
    })
    // 切群/下一轮发送已发生：旧响应只负责自己的乐观气泡，不得复位新一轮的 groupSending。
    if (groupSendGen !== gen || get().activeGroupId !== activeGroupId) return false
    if (!r.ok) {
      set((s) => ({
        groupSending: false, error: r.error || '发送失败',
        groupMessages: s.groupMessages.map((m) => (m.clientMsgId === clientMsgId ? { ...m, pending: false, failed: true } : m)),
      }))
      return false
    }
    set((s) => ({
      groupSending: false,
      groupRounds: upsertRound(s.groupRounds, r.data?.round),
      groupMessages: s.groupMessages.map((m) => (m.clientMsgId === clientMsgId && r.data?.seq ? { ...m, seq: r.data.seq, pending: false } : m)),
    }))
    void get().loadGroupActivity(activeGroupId)
    return true
  },

  handleEvent(line) {
    let ev: {
      event_type?: string
      agent_id?: string
      session_id?: string
      run_id?: string
      parent_run_id?: string
      depth?: number
      seq?: number
      message?: HubMessage | string
      client_msg_id?: string
      content?: string
      text?: string
      status?: string
      task?: HubTask
      need?: HubNeed
      id?: string
      name?: string
      is_error?: boolean
      arguments?: string
      timestamp?: string
      ms?: number
      before?: number
      after?: number
      reason?: string
      group_id?: string
      scope?: string
      code?: string
      kind?: string
      severity?: string
      task_id?: string
      at?: string
      round?: HubRound
    }
    try {
      ev = JSON.parse(line) as typeof ev
    } catch {
      return
    }
    const { activeId, sid } = get()
    const isRoot = (ev.parent_run_id ?? '') === '' && (ev.depth ?? 0) === 0
    switch (ev.event_type) {
      case 'hub_message': {
        if (ev.group_id) {
          if (ev.group_id !== get().activeGroupId) return
          const msg = typeof ev.message === 'string' ? undefined : ev.message
          if (!msg) return
          set((s) => {
            const withoutPending = ev.client_msg_id
              ? s.groupMessages.filter((m) => m.clientMsgId !== ev.client_msg_id)
              : s.groupMessages
            if (withoutPending.some((m) => m.seq === msg.seq)) return {}
            return { groupMessages: [...withoutPending, msg].sort((a, b) => a.seq - b.seq) }
          })
          return
        }
        const agentID = ev.agent_id || ''
        const msg = typeof ev.message === 'string' ? undefined : ev.message
        if (!msg) return
        // 左栏预览/未读同步（非当前 DM 也更新，正在看的那条不算未读）。
        set((s) => {
          const dm = { ...s.dm }
          const info = { ...(dm[agentID] ?? {}) }
          info.last_seq = msg.seq
          info.last_text = (msg.text ?? '').replace(/\s+/g, ' ').slice(0, 80)
          info.last_sender = msg.sender?.kind
          info.last_at = msg.created_at ?? new Date().toISOString()
          if (agentID !== activeId && msg.sender?.kind === 'agent') {
            info.unread = (info.unread ?? 0) + 1
          }
          dm[agentID] = info
          return { dm }
        })
        if (agentID !== activeId) return
        set((s) => {
          // 乐观消息 → 真实消息：按 client_msg_id（人发的）或 seq（智能体回复）去重。
          const withoutPending = ev.client_msg_id
            ? s.messages.filter((m) => m.clientMsgId !== ev.client_msg_id)
            : s.messages
          if (withoutPending.some((m) => m.seq === msg.seq)) return { live: '' }
          const next = [...withoutPending.filter((m) => m.seq !== msg.seq), msg]
          return { messages: next.sort((a, b) => a.seq - b.seq), live: msg.sender?.kind === 'agent' ? '' : s.live }
        })
        return
      }
      case 'content_chunk': {
        // 只收：① 当前会话 ② 根 run（子 agent 的 chunk 也带同一 session_id，run_id 不同）。
        if (!sid || (ev.session_id && ev.session_id !== sid)) return
        if (!isRoot) return
        const root = get().rootRunId
        if (root && ev.run_id && ev.run_id !== root) return
        const piece = ev.content ?? ev.text ?? ''
        if (piece) {
          set((s) => ({
            live: s.live + piece,
            running: activeId ? { ...s.running, [activeId]: true } : s.running,
          }))
        }
        return
      }
      case 'agent_start': {
        if (!isRoot) return
        if (!activeId || !sid || (ev.session_id && ev.session_id !== sid)) return
        set((s) => ({
          running: { ...s.running, [activeId]: true },
          live: '',
          rootRunId: ev.run_id ?? null,
        }))
        return
      }
      case 'agent_end': {
        if (!isRoot) return // 子 agent 结束不能熄根 run 的运行点
        if (!activeId || !sid || (ev.session_id && ev.session_id !== sid)) return
        set((s) => ({
          running: { ...s.running, [activeId]: false },
          live: '', // 纯工具轮/中断没有最终回复：半截流式气泡不能留着
          rootRunId: null,
        }))
        return
      }
      case 'hub_alert': {
        const code = ev.code || ev.kind || 'hub_alert'
        const message = typeof ev.message === 'string' ? ev.message : undefined
        const id = alertKey(code, ev.task_id, message)
        const alert: HubAlert = {
          id, scope: ev.scope, code, kind: ev.kind, severity: ev.severity,
          task_id: ev.task_id, agent_id: ev.agent_id, group_id: ev.group_id,
          message, at: ev.at,
        }
        set((s) => {
          if (s.alerts.some((a) => a.id === id)) return {}
          return { alerts: [alert, ...s.alerts].slice(0, 20) }
        })
        return
      }
      case 'hub_task_update': {
        const task = ev.task
        if (!task?.id) return
        set((s) => {
          const rest = s.tasks.filter((t) => t.id !== task.id)
          return { tasks: [task, ...rest] }
        })
        return
      }
      case 'hub_need': {
        const need = ev.need
        if (!need?.id) return
        set((s) => {
          const rest = s.needs.filter((n) => n.id !== need.id)
          // 已处理/过期的不留在 pending 列表里（但保留事件通知能力由上层决定）。
          if (need.status && need.status !== 'pending') return { needs: rest }
          return { needs: [need, ...rest] }
        })
        return
      }
      case 'tool_run_start':
      case 'tool_run_end': {
        if (!sid || (ev.session_id && ev.session_id !== sid)) return
        const key = ev.id || `${ev.name ?? 'tool'}-${ev.timestamp ?? Date.now()}`
        const started = ev.event_type === 'tool_run_start'
        set((s) => {
          if (started) {
            if (s.runLog.some((e) => e.id === key)) return {}
            return {
              runLog: [...s.runLog, { id: key, kind: 'tool' as const, name: ev.name, status: 'running', at: Date.now() }],
            }
          }
          const next = s.runLog.map((e) =>
            e.id === key ? { ...e, status: ev.is_error ? 'error' : 'done', ms: e.at ? Date.now() - e.at : undefined } : e,
          )
          if (!next.some((e) => e.id === key)) {
            next.push({ id: key, kind: 'tool', name: ev.name, status: ev.is_error ? 'error' : 'done', at: Date.now() })
          }
          return { runLog: next }
        })
        return
      }
      case 'compress_start': {
        if (!sid || (ev.session_id && ev.session_id !== sid)) return
        set((s) => ({
          runLog: [...s.runLog, { id: `compress-${Date.now()}`, kind: 'compress' as const, name: 'compress', status: 'running', at: Date.now() }],
        }))
        return
      }
      case 'compress_end': {
        if (!sid || (ev.session_id && ev.session_id !== sid)) return
        set((s) => ({
          runLog: [...s.runLog, {
            id: `compress-${Date.now()}`, kind: 'compress' as const, name: 'compress', status: 'done',
            summary: `${ev.before ?? '?'} → ${ev.after ?? '?'}`, at: Date.now(),
          }],
          actions: [...s.actions, {
            seq: -(s.actions.length + 1), sender: { kind: 'system' },
            text: `上下文已压缩：${ev.before ?? '?'} → ${ev.after ?? '?'} 条`,
            refs: { action: 'compress' }, created_at: new Date().toISOString(),
          }],
        }))
        return
      }
      case 'hub_round': {
        if (!ev.group_id || ev.group_id !== get().activeGroupId) return
        set((s) => ({ groupRounds: upsertRound(s.groupRounds, ev.round) }))
        void get().loadGroupActivity(ev.group_id)
        return
      }
      case 'hub_agent_status': {
        const agentID = ev.agent_id || ''
        if (!agentID) return
        const running = ev.status !== 'idle' && ev.status !== 'deleted'
        set((s) => ({ running: { ...s.running, [agentID]: running } }))
        return
      }
      default:
        return
    }
  },
}))

/** App 启动时订阅一次（离开 Hub 页也要收告警）；返回撤销函数。 */
export function attachHubEvents(): () => void {
  const transport = getTransport()
  return transport.onEvent((line) => useHubStore.getState().handleEvent(line))
}
