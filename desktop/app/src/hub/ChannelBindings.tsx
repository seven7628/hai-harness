import { useCallback, useEffect, useMemo, useState } from 'react'
import { useT } from '../i18n'
import { useAppStore } from '../store/useAppStore'
import Sel from '../components/Sel'
import { hubCommand } from './api'

// 智能体/群组的渠道绑定（复用现有 im-config.json 的 Gateways[].Routes）。
//
// 不新建 channel 表：一条绑定就是一条 im.RouteConfig。
//   agent → { chat, agent }                owner = agent:<id>
//   group → { chat, group, agent }         owner = group:<id>，agent 是群内执行者
//
// 读：im_config_get（gateway 列表）+ im_route_list（按 owner 过滤）
// 写：im_route_set / im_route_del；归属开关：plugin_enable / plugin_disable（owner 级）
// 群列表：im_chat_list（渠道不支持 ChatLister 时回落手填 chat_id）。

type OwnerKind = 'agent' | 'group'

interface GatewayRow {
  id: string
  type: string
  enabled?: boolean
}

interface RouteRow {
  gateway_id: string
  chat: { gateway: string; chat_id: string; thread_id?: string }
  agent?: string
  group?: string
  owner?: string
  ownerEnabled?: boolean
  target?: string
}

interface ChatRow {
  chat_id: string
  name?: string
}

interface MemberRow {
  id: string
  label: string
}

const ownerOf = (kind: OwnerKind, id: string): string => `${kind}:${id}`

export default function ChannelBindings({
  ownerKind,
  ownerId,
  executorId,
  members = [],
}: {
  ownerKind: OwnerKind
  ownerId: string
  /** 群绑定的默认执行者（通常是群主）；agent 绑定忽略。 */
  executorId?: string
  /** 群成员（执行者下拉）；agent 绑定忽略。 */
  members?: MemberRow[]
}) {
  const t = useT()
  const workspace = useAppStore((s) => s.workspace)
  const owner = ownerOf(ownerKind, ownerId)

  const [gateways, setGateways] = useState<GatewayRow[]>([])
  const [routes, setRoutes] = useState<RouteRow[]>([])
  const [ownerEnabled, setOwnerEnabled] = useState<boolean | null>(null)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [note, setNote] = useState('')

  const [adding, setAdding] = useState(false)
  const [gatewayId, setGatewayId] = useState('')
  const [chatId, setChatId] = useState('')
  const [threadId, setThreadId] = useState('')
  const [executor, setExecutor] = useState(executorId ?? '')
  const [chats, setChats] = useState<ChatRow[]>([])
  const [chatCache, setChatCache] = useState<Record<string, ChatRow[]>>({})
  const [chatsLoading, setChatsLoading] = useState(false)
  const [chatError, setChatError] = useState('')

  useEffect(() => { setExecutor(executorId ?? '') }, [executorId])

  const gateway = useMemo(() => gateways.find((g) => g.id === gatewayId), [gateways, gatewayId])
  const isGroup = ownerKind === 'group'
  const chatLabel = useCallback((r: RouteRow): string => {
    const known = (chatCache[r.gateway_id] ?? chats).find((c) => c.chat_id === r.chat.chat_id)?.name
    return known || r.chat.chat_id
  }, [chatCache, chats])

  const load = useCallback(async () => {
    if (!workspace) {
      setLoading(false)
      setError(t('hub.channel.noWorkspace'))
      return
    }
    setLoading(true)
    setError(null)
    const [cfg, list] = await Promise.all([
      hubCommand<{ config?: { gateways?: GatewayRow[] } }>('im_config_get', { workspace }),
      hubCommand<{ routes?: RouteRow[]; ownerEnabled?: boolean }>('im_route_list', { workspace, owner }),
    ])
    setLoading(false)
    if (!cfg.ok) {
      setError(cfg.error || t('hub.channel.loadFailed'))
      return
    }
    if (!list.ok) {
      setError(list.error || t('hub.channel.loadFailed'))
      return
    }
    const gs = cfg.data?.config?.gateways ?? []
    setGateways(gs)
    setRoutes(list.data?.routes ?? [])
    setOwnerEnabled(list.data?.ownerEnabled ?? null)
    setGatewayId((cur) => cur || (gs.find((g) => g.enabled !== false) ?? gs[0])?.id || '')
    // 路由行显示群名而不是裸 chat_id：后台拉，失败不阻塞绑定，回落 chat_id。
    void Promise.all(gs.map(async (g) => {
      const r = await hubCommand<{ chats?: ChatRow[] }>('im_chat_list', { workspace, gateway_id: g.id })
      return [g.id, r.ok ? (r.data?.chats ?? []) : []] as const
    })).then((entries) => setChatCache(Object.fromEntries(entries)))
  }, [workspace, owner]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { void load() }, [load])

  // 换 gateway / 打开添加表单时拉群列表；不支持 ChatLister 的渠道给出可读原因并允许手填。
  useEffect(() => {
    if (!adding || !gatewayId || !workspace) return
    const cached = chatCache[gatewayId]
    if (cached) {
      setChats(cached)
      setChatsLoading(false)
      setChatError('')
      return
    }
    let cancelled = false
    setChatsLoading(true)
    setChatError('')
    setChats([])
    setChatId('')
    void hubCommand<{ chats?: ChatRow[] }>('im_chat_list', { workspace, gateway_id: gatewayId }).then((r) => {
      if (cancelled) return
      setChatsLoading(false)
      if (!r.ok) {
        setChatError(r.error || t('hub.channel.chatListFailed'))
        return
      }
      setChats(r.data?.chats ?? [])
    })
    return () => { cancelled = true }
  }, [adding, gatewayId, workspace, chatCache]) // eslint-disable-line react-hooks/exhaustive-deps

  async function add(): Promise<void> {
    if (!gateway) { setError(t('hub.channel.needGateway')); return }
    const cid = chatId.trim()
    if (!cid) { setError(t('hub.channel.needChat')); return }
    const exec = isGroup ? (executor || executorId || '').trim() : ''
    if (isGroup && !exec) { setError(t('hub.channel.needExecutor')); return }
    // 同 Chat 已被别的归属绑定：im_route_set 会静默改绑，先让用户看见。
    const all = await hubCommand<{ routes?: RouteRow[] }>('im_route_list', { workspace })
    if (all.ok) {
      const tid = threadId.trim()
      const conflict = (all.data?.routes ?? []).find((r) => r.gateway_id === gateway.id
        && r.chat.chat_id === cid && (r.chat.thread_id ?? '') === tid && r.owner !== owner)
      if (conflict) {
        const who = conflict.owner || conflict.target || t('hub.channel.otherOwner')
        if (!window.confirm(t('hub.channel.rebindConfirm').replace('{owner}', who).replace('{kind}', t(isGroup ? 'hub.channel.kind.group' : 'hub.channel.kind.agent')))) return
      }
    }
    setBusy(true)
    setError(null)
    const route = {
      chat: {
        gateway: gateway.type,
        chat_id: cid,
        ...(threadId.trim() ? { thread_id: threadId.trim() } : {}),
      },
      ...(isGroup ? { group: ownerId, agent: exec } : { agent: ownerId }),
    }
    const r = await hubCommand('im_route_set', { workspace, gateway_id: gateway.id, route })
    setBusy(false)
    if (!r.ok) {
      setError(r.error || t('hub.channel.addFailed'))
      return
    }
    setAdding(false)
    setChatId('')
    setThreadId('')
    setNote(t('hub.channel.added'))
    await load()
  }

  async function remove(r: RouteRow): Promise<void> {
    if (!window.confirm(t('hub.channel.removeConfirm').replace('{chat}', chatLabel(r)))) return
    setBusy(true)
    setError(null)
    const res = await hubCommand('im_route_del', { workspace, gateway_id: r.gateway_id, chat: r.chat })
    setBusy(false)
    if (!res.ok) {
      setError(res.error || t('hub.channel.removeFailed'))
      return
    }
    setNote(t('hub.channel.removed'))
    await load()
  }

  async function toggleOwner(): Promise<void> {
    const next = !(ownerEnabled ?? true)
    setBusy(true)
    setError(null)
    const res = await hubCommand(next ? 'plugin_enable' : 'plugin_disable', {
      workspace,
      id: 'im',
      owner,
    })
    setBusy(false)
    if (!res.ok) {
      setError(res.error || t('hub.channel.toggleFailed'))
      return
    }
    setOwnerEnabled(next)
    setRoutes((rs) => rs.map((r) => ({ ...r, ownerEnabled: next })))
    setNote(next ? t('hub.channel.enabled') : t('hub.channel.disabled'))
  }

  if (loading) return <p className="hub-muted">{t('common.loading')}</p>

  return (
    <div className="hub-channels">
      <div className="hub-channels-head">
        <span className="hub-sub">
          {t('hub.channel.hint').replace('{kind}', t(isGroup ? 'hub.channel.kind.group' : 'hub.channel.kind.agent'))}
        </span>
        {routes.length > 0 ? (
          <button
            className={`hub-link${ownerEnabled === false ? ' hub-channel-off' : ''}`}
            disabled={busy}
            onClick={() => void toggleOwner()}
            title={t('hub.channel.toggleHint')}
          >
            {ownerEnabled === false ? t('hub.channel.enable') : t('hub.channel.disable')}
          </button>
        ) : null}
      </div>

      {routes.length === 0 ? (
        <p className="hub-muted">{t('hub.channel.empty')}</p>
      ) : (
        <div className="hub-channel-list">
          {routes.map((r) => {
            const gw = gateways.find((g) => g.id === r.gateway_id)
            return (
              <div className="hub-channel-row" key={`${r.gateway_id}:${r.chat.gateway}:${r.chat.chat_id}:${r.chat.thread_id ?? ''}`}>
                <span className="hub-channel-dot" data-on={r.ownerEnabled === false ? '0' : '1'} />
                <div className="hub-channel-main">
                  <span className="hub-channel-name">{chatLabel(r)}</span>
                  <span className="hub-channel-meta">
                    {gw?.type ?? r.chat.gateway} · {r.chat.chat_id}
                    {r.chat.thread_id ? `:${r.chat.thread_id}` : ''}
                    {isGroup && r.agent ? ` · ${members.find((m) => m.id === r.agent)?.label ?? r.agent}` : ''}
                  </span>
                </div>
                <button
                  className="hub-link"
                  disabled={busy}
                  onClick={() => void remove(r)}
                  aria-label={t('hub.channel.removeAria').replace('{chat}', chatLabel(r))}
                >
                  {t('hub.channel.remove')}
                </button>
              </div>
            )
          })}
        </div>
      )}

      {!adding ? (
        <button
          className="hub-primary hub-channel-add"
          disabled={busy || gateways.length === 0}
          onClick={() => {
            setAdding(true)
            setChatId('')
            setThreadId('')
            setExecutor(executorId ?? '')
          }}
        >
          {t('hub.channel.add')}
        </button>
      ) : (
        <div className="hub-channel-form">
          <label>{t('hub.channel.gateway')}
            <Sel
              value={gatewayId}
              onChange={setGatewayId}
              options={gateways.map((g) => ({
                value: g.id,
                label: `${g.type} · ${g.id}${g.enabled === false ? ` · ${t('hub.channel.gatewayOff')}` : ''}`,
                disabled: g.enabled === false,
              }))}
              placeholder={t('hub.channel.gatewayPick')}
            />
          </label>
          <label>{t('hub.channel.chat')}
            {chats.length > 0 ? (
              <Sel
                value={chatId}
                onChange={setChatId}
                options={chats.map((c) => ({ value: c.chat_id, label: c.name ? `${c.name} · ${c.chat_id}` : c.chat_id }))}
                placeholder={t('hub.channel.chatPick')}
              />
            ) : (
              <input
                value={chatId}
                onChange={(e) => setChatId(e.target.value)}
                placeholder={chatsLoading ? t('hub.channel.chatLoading') : t('hub.channel.chatPlaceholder')}
              />
            )}
            {chatError ? <span className="hub-hint">{chatError} · {t('hub.channel.chatManual')}</span> : null}
          </label>
          <label>{t('hub.channel.thread')}
            <input
              value={threadId}
              onChange={(e) => setThreadId(e.target.value)}
              placeholder={t('hub.channel.threadPlaceholder')}
            />
          </label>
          {isGroup ? (
            <label>{t('hub.channel.executor')}
              <Sel
                value={executor}
                onChange={setExecutor}
                options={members.map((m) => ({ value: m.id, label: m.label }))}
                placeholder={t('hub.channel.executorPick')}
              />
            </label>
          ) : null}
          <div className="hub-row-actions">
            <button className="hub-primary" disabled={busy} onClick={() => void add()}>
              {busy ? t('hub.channel.saving') : t('hub.channel.save')}
            </button>
            <button className="hub-link" disabled={busy} onClick={() => { setAdding(false); setError(null) }}>
              {t('hub.cancel')}
            </button>
          </div>
        </div>
      )}

      {gateways.length === 0 ? <p className="hub-hint">{t('hub.channel.noGateways')}</p> : null}
      {error ? <div className="hub-error" role="alert">{error}</div> : null}
      {note ? <div className="hub-notice" role="status">{note}</div> : null}
    </div>
  )
}
