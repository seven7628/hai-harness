import { create } from 'zustand'
import { getTransport } from '../transport'
import { hubCommand } from './api'
import type { HubAgent, HubDMInfo, HubHistoryPage, HubMessage, HubPage } from './types'

// Agent Hub 的前端状态（§8.2 Hub Rail + §8.4 DM 视图）。
//
// 独立于 useAppStore：Hub 是**另一块 surface**（智能体列表 + DM），没有工作区会话的
// 三层视图/审阅面/trace；混进主 store 会让「当前会话」两套语义打架（hub 的
// session_id 是 hub.<agent>.dm，但导航、右栏、恢复逻辑都按工作区会话走）。
// 代价是事件订阅要单独挂一份：HubSurface 挂载时 attach，卸载时撤销。

interface HubState {
  open: boolean
  agents: HubAgent[]
  dm: Record<string, HubDMInfo>
  activeId: string | null
  profile: HubAgent | null
  sid: string | null
  messages: HubMessage[]
  hasMore: boolean
  unread: number
  loading: boolean
  loadingOlder: boolean
  sending: boolean
  error: string | null
  /** 当前 DM 的流式正文（content_chunk 累积；hub_message 落定后清空）。 */
  live: string
  /** agent_id → 是否在跑（左栏状态点）。 */
  running: Record<string, boolean>

  openHub(): void
  closeHub(): void
  refresh(): Promise<void>
  select(agentID: string): Promise<void>
  loadOlder(): Promise<void>
  send(text: string): Promise<void>
  clearError(): void
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
  unread: 0,
  loading: false,
  loadingOlder: false,
  sending: false,
  error: null,
  live: '',
  running: {},

  openHub() {
    set({ open: true })
    void get().refresh()
  },

  closeHub() {
    set({ open: false })
  },

  async refresh() {
    set({ loading: true, error: null })
    const r = await hubCommand<{ agents?: HubAgent[]; dm?: Record<string, HubDMInfo> }>('hub_agent_list', {})
    if (!r.ok) {
      set({ loading: false, error: r.error || '读取智能体列表失败' })
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
    set({ agents: sorted, dm, loading: false })
  },

  async select(agentID) {
    if (!agentID) return
    set({ activeId: agentID, messages: [], hasMore: false, unread: 0, live: '', error: null, loading: true })
    const r = await hubCommand<HubPage>('hub_open', {
      target: { kind: 'dm', agent_id: agentID },
      limit: 50,
    })
    if (!r.ok) {
      set({ loading: false, error: r.error || '打开对话失败' })
      return
    }
    const msgs = r.data?.messages ?? []
    set({
      loading: false,
      profile: r.data?.agent ?? null,
      sid: r.data?.session_id ?? null,
      messages: msgs,
      hasMore: r.data?.has_more === true,
      unread: 0,
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

  async loadOlder() {
    const { activeId, messages, hasMore, loadingOlder } = get()
    if (!activeId || !hasMore || loadingOlder || messages.length === 0) return
    set({ loadingOlder: true })
    const r = await hubCommand<HubHistoryPage>('hub_history', {
      agent_id: activeId,
      before_seq: messages[0].seq,
      limit: 50,
    })
    if (!r.ok) {
      set({ loadingOlder: false, error: r.error || '加载更早消息失败' })
      return
    }
    const older = r.data?.messages ?? []
    set((s) => ({
      loadingOlder: false,
      hasMore: r.data?.has_more === true,
      messages: [...older, ...s.messages.filter((m) => m.seq > (older[older.length - 1]?.seq ?? 0))],
    }))
  },

  async send(text) {
    const { activeId, sending } = get()
    const body = text.trim()
    if (!activeId || !body || sending) return
    const clientMsgId = `ui-${Date.now()}-${++optimisticSeq}`
    const optimistic: HubMessage = {
      seq: -optimisticSeq,
      sender: { kind: 'human' },
      text: body,
      pending: true,
      clientMsgId,
    }
    set((s) => ({ messages: [...s.messages, optimistic], sending: true, error: null }))
    const r = await hubCommand<{ seq?: number; deduplicated?: boolean }>('hub_send', {
      target: { kind: 'dm', agent_id: activeId },
      text: body,
      client_msg_id: clientMsgId,
    })
    if (!r.ok) {
      // 失败：撤下乐观消息并把原文还给输入框（调用方从事件里拿不到内容）。
      set((s) => ({
        sending: false,
        error: r.error || '发送失败',
        messages: s.messages.filter((m) => m.clientMsgId !== clientMsgId),
      }))
      return
    }
    const seq = Number(r.data?.seq ?? 0)
    if (seq > 0) {
      // hub_message 事件通常已经先到（同一条命令里 emit 在 resp 之前）；
      // 这里兜底把乐观消息就地落成真实 seq，事件重复到达时按 seq 去重。
      set((s) => ({
        messages: s.messages.map((m) =>
          m.clientMsgId === clientMsgId ? { ...m, seq, pending: false } : m,
        ),
      }))
    }
    set({ sending: false })
    void get().refresh()
  },

  clearError() {
    set({ error: null })
  },

  handleEvent(line) {
    let ev: {
      event_type?: string
      agent_id?: string
      session_id?: string
      seq?: number
      message?: HubMessage
      client_msg_id?: string
      text?: string
      content?: string
      status?: string
    }
    try {
      ev = JSON.parse(line) as typeof ev
    } catch {
      return
    }
    const { activeId, sid } = get()
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
          return {
            messages: [...withoutPending.filter((m) => m.seq !== msg.seq), msg].sort((a, b) => a.seq - b.seq),
            live: msg.sender?.kind === 'agent' ? '' : s.live,
          }
        })
        return
      }
      case 'content_chunk':
      case 'reasoning_chunk': {
        // DM 的流式：与工作区会话同一条 content_chunk（群组才用 hub_message_delta）。
        if (ev.event_type !== 'content_chunk') return
        if (!sid || (ev.session_id && ev.session_id !== sid)) return
        const piece = ev.text ?? ev.content ?? ''
        if (piece) set((s) => ({ live: s.live + piece }))
        return
      }
      case 'agent_start': {
        if (!activeId) return
        if (ev.session_id && sid && ev.session_id !== sid) return
        set((s) => ({ running: { ...s.running, [activeId]: true }, live: '' }))
        return
      }
      case 'agent_end': {
        if (!activeId) return
        if (ev.session_id && sid && ev.session_id !== sid) return
        set((s) => ({ running: { ...s.running, [activeId]: false } }))
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
