import { create } from 'zustand'
import { getTransport } from '../transport'
import { hubCommand } from './api'
import type { HubAgent, HubDMInfo, HubHistoryPage, HubMessage, HubPage } from './types'

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
  handleEvent(line: string): void
}

let optimisticSeq = 0 // 负数临时 seq，避免与真实 seq（正数）混淆

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

  openHub() {
    set({ open: true })
    void get().refresh()
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
    set((s) => ({
      activeId: agentID,
      messages: [],
      hasMore: false,
      live: '',
      rootRunId: null,
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

  handleEvent(line) {
    let ev: {
      event_type?: string
      agent_id?: string
      session_id?: string
      run_id?: string
      parent_run_id?: string
      depth?: number
      seq?: number
      message?: HubMessage
      client_msg_id?: string
      content?: string
      text?: string
      status?: string
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
        const agentID = ev.agent_id || ''
        const msg = ev.message
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

/** HubSurface 挂载时订阅事件；返回撤销函数。 */
export function attachHubEvents(): () => void {
  const transport = getTransport()
  return transport.onEvent((line) => useHubStore.getState().handleEvent(line))
}
