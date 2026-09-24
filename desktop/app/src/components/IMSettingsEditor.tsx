import { useEffect, useMemo, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import type {
  IMConfig,
  IMGatewayCfg,
  IMGatewaySchema,
  IMField,
} from '../transport/types'
import Sel from './Sel'

// IM 集成设置编辑器（设置弹窗「IM 集成」tab）。
// 设计（docs/IM_INTEGRATION.md §9）：每个 Gateway 自带配置 Schema（im_schema_list 返回），
// UI 按 Schema 动态渲染表单——不同渠道配置不同（飞书 4+ 项 / Telegram 1 项），
// 新增渠道 = 后端注册 Schema，前端零改动。
//
// 交互：
//   - 主开关（enabled）：总开关
//   - Gateway 列表：每个实例一行（类型/名称/状态点/启用开关/编辑/删除）
//   - 添加 Gateway：选类型 → 动态渲染 Schema 表单 → 保存
//   - 路由表（Chat → workspace）：增删
//   - 授权名单：per-gateway user id 列表
//   - Secret 字段：保存后不回显（显示"已设置"），可覆盖
//
// 数据源：store（im_config_get/im_schema_list/im_status 经 command_response 事件分发写入）。

export function IMSettingsEditor() {
  const t = useT()
  const imConfig = useAppStore((s) => s.imConfig)
  const imSchemas = useAppStore((s) => s.imSchemas)
  const imStatuses = useAppStore((s) => s.imStatuses)
  const fetchIMConfig = useAppStore((s) => s.fetchIMConfig)
  const saveIMConfig = useAppStore((s) => s.saveIMConfig)

  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [loadFailed, setLoadFailed] = useState(false)
  const [attempt, setAttempt] = useState(0)

  // 添加/编辑 Gateway 弹窗状态
  const [adding, setAdding] = useState(false)
  const [addType, setAddType] = useState('')
  const [addDraft, setAddDraft] = useState<Record<string, unknown>>({})
  const [addErr, setAddErr] = useState<string | null>(null)
  const [editingId, setEditingId] = useState<string | null>(null) // 编辑中的 gateway id
  // 行内添加路由/授权：gateway id → 'route' | 'allow' | null（正在添加什么）
  const [addingTo, setAddingTo] = useState<Record<string, 'route' | 'allow' | undefined>>({})

  // 打开时拉取配置（未取到则触发）；拉取失败 8s 后显示重试
  useEffect(() => {
    fetchIMConfig()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [attempt])

  useEffect(() => {
    if (imConfig) return
    const timer = window.setTimeout(() => setLoadFailed(true), 8000)
    return () => window.clearTimeout(timer)
  }, [imConfig])

  const cfg: IMConfig = {
    enabled: imConfig?.enabled ?? false,
    // 字段级兜底：Go 序列化空配置可能省略字段（omitempty）或历史数据为 null
    gateways: (imConfig?.gateways ?? []).map((g) => ({
      ...g,
      routes: g.routes ?? [],
      allow_users: g.allow_users ?? [],
    })),
    mirrors: imConfig?.mirrors ?? [],
    security: {
      require_bind_confirm: imConfig?.security?.require_bind_confirm ?? true,
    },
  }

  const save = async (next: IMConfig) => {
    setErr(null)
    setBusy(true)
    const r = await saveIMConfig(next)
    setBusy(false)
    if (!r.ok) {
      setErr(r.error ?? t('settings.im.errSave'))
      return false
    }
    return true
  }

  // 注意：所有 hooks 必须在条件 return 之前（React hooks 规则）
  const schemaByType = useMemo(() => {
    const m: Record<string, IMGatewaySchema> = {}
    for (const s of imSchemas) m[s.type] = s
    return m
  }, [imSchemas])

  if (!imConfig && loadFailed) {
    return (
      <div className="mcp-form">
        <p className="set-placeholder">{t('settings.im.loadFailed')}</p>
        <button className="btn" onClick={() => { setLoadFailed(false); setAttempt((n) => n + 1) }}>
          {t('common.retry')}
        </button>
      </div>
    )
  }
  if (!imConfig) {
    return <p className="set-placeholder">{t('settings.im.loading')}</p>
  }

  const allFields = (s: IMGatewaySchema) => (s.groups ?? []).flatMap((g) => g.fields)

  // 渲染单个字段（按 type）
  const renderField = (
    f: IMField,
    value: unknown,
    set: (v: unknown) => void,
    secretSet: boolean, // 是否已设置（secret 显示"已设置"）
  ) => {
    const inputCls = 'im-inp'
    switch (f.type) {
      case 'password':
        return (
          <div className="im-field">
            <label className="im-label">
              {f.label}
              {f.required && <span className="im-req">*</span>}
            </label>
            <div className="im-inp-row">
              <input
                className={inputCls}
                type="password"
                placeholder={secretSet ? '••••••（已设置）' : f.placeholder ?? ''}
                value={(value as string) ?? ''}
                onChange={(e) => set(e.target.value)}
              />
              {secretSet && <span className="im-set-badge">✓ 已设置</span>}
            </div>
            {f.help && <div className="im-help">{f.help}</div>}
          </div>
        )
      case 'select':
        return (
          <div className="im-field">
            <label className="im-label">
              {f.label}
              {f.required && <span className="im-req">*</span>}
            </label>
            <Sel
              value={(value as string) ?? (f.default as string) ?? ''}
              options={(f.options ?? []).map((o) => ({ value: o.value, label: o.label }))}
              onChange={set}
            />
            {f.help && <div className="im-help">{f.help}</div>}
          </div>
        )
      case 'bool':
        return (
          <div className="im-field im-field-row">
            <label className="im-label">{f.label}</label>
            <button
              className={`switch ${value ? 'on' : ''}`}
              role="switch"
              aria-checked={!!value}
              onClick={() => set(!value)}
            />
            {f.help && <div className="im-help">{f.help}</div>}
          </div>
        )
      default:
        return (
          <div className="im-field">
            <label className="im-label">
              {f.label}
              {f.required && <span className="im-req">*</span>}
            </label>
            <input
              className={inputCls}
              type="text"
              placeholder={f.placeholder ?? ''}
              value={(value as string) ?? ''}
              onChange={(e) => set(e.target.value)}
            />
            {f.help && <div className="im-help">{f.help}</div>}
          </div>
        )
    }
  }

  // 初始化 draft：填充 schema 字段的默认值（select/bool/string 的 default）
  const initDraftWithDefaults = (schema: IMGatewaySchema | undefined) => {
    if (!schema) return {}
    const d: Record<string, unknown> = {}
    for (const f of allFields(schema)) {
      if (f.default !== undefined) d[f.key] = f.default
    }
    return d
  }

  // 添加/编辑 Gateway 表单
  const renderAddForm = () => {
    const schema = schemaByType[addType]
    if (!schema) return null
    const fields = allFields(schema)
    const base = fields.filter((f) => !f.advanced)
    const adv = fields.filter((f) => f.advanced)

    return (
      <div className="im-add-form">
        <div className="im-add-head">
          <span className="im-add-title">
            {editingId ? t('settings.im.editGateway') : t('settings.im.addGateway')}
            ：{schema.name}
          </span>
          <button
            className="btn gho"
            onClick={() => { setAdding(false); setEditingId(null); setAddDraft({}) }}
          >
            ✕
          </button>
        </div>
        {base.map((f) =>
          renderField(f, addDraft[f.key], (v) => setAddDraft((d) => ({ ...d, [f.key]: v })), false),
        )}
        {adv.length > 0 && (
          <details className="im-adv">
            <summary>{t('settings.im.advanced')}</summary>
            <div className="im-adv-body">
              {adv.map((f) =>
                renderField(f, addDraft[f.key], (v) => setAddDraft((d) => ({ ...d, [f.key]: v })), false),
              )}
            </div>
          </details>
        )}
        {addErr && <p className="im-err">{addErr}</p>}
        <div className="btnrow" style={{ marginTop: 10 }}>
          <button
            className="btn pri"
            disabled={busy}
            onClick={async () => {
              setAddErr(null)
              // 必填校验
              const missing = fields.filter((f) => f.required && !addDraft[f.key])
              if (missing.length > 0) {
                setAddErr(`${t('settings.im.missingRequired')}: ${missing.map((f) => f.label).join(', ')}`)
                return
              }
              const next = { ...cfg }
              if (editingId) {
                const idx = next.gateways.findIndex((g) => g.id === editingId)
                if (idx >= 0) {
                  next.gateways[idx] = {
                    ...next.gateways[idx],
                    type: addType,
                    config: { ...addDraft },
                  }
                }
              } else {
                next.gateways.push({
                  id: addType + '-' + Date.now().toString(36),
                  type: addType,
                  enabled: true,
                  config: { ...addDraft },
                })
              }
              if (await save(next)) {
                setAdding(false)
                setEditingId(null)
                setAddDraft({})
              }
            }}
          >
            {t('mcp.save')}
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="im-editor">
      {/* 主开关 */}
      <div className="im-master">
        <div>
          <div className="im-master-title">{t('settings.im.title')}</div>
          <div className="im-master-desc">{t('settings.im.desc')}</div>
        </div>
        <button
          className={`switch ${cfg.enabled ? 'on' : ''}`}
          role="switch"
          aria-checked={cfg.enabled}
          disabled={busy}
          onClick={async () => {
            const next = { ...cfg, enabled: !cfg.enabled }
            await save(next)
          }}
        />
      </div>

      {/* Gateway 列表 */}
      <div className="im-sec">
        <div className="im-sec-head">
          <span className="im-sec-title">{t('settings.im.gateways')}</span>
          <button
            className="btn out"
            onClick={() => {
              setAdding(true)
              setEditingId(null)
              setAddDraft(initDraftWithDefaults(schemaByType[imSchemas[0]?.type ?? '']))
              setAddType(imSchemas[0]?.type ?? '')
            }}
          >
            ＋ {t('settings.im.addGateway')}
          </button>
        </div>
        {cfg.gateways.length === 0 ? (
          <p className="set-placeholder">{t('settings.im.noGateways')}</p>
        ) : (
          cfg.gateways.map((g: IMGatewayCfg) => {
            const s = schemaByType[g.type]
            const st = imStatuses[g.id] ?? 'not_started'
            const routes = g.routes ?? []
            const allowUsers = g.allow_users ?? []
            return (
              <div className="im-gw-card" key={g.id}>
                {/* 头部：状态 + 名称 + 操作 */}
                <div className="im-gw">
                  <span className={`im-dot im-dot-${st}`} />
                  <span className="im-gw-name">{s?.name ?? g.type}</span>
                  <span className="im-gw-id">{g.id}</span>
                  <span className="im-gw-status">{t(`settings.im.st.${st}`)}</span>
                  <button
                    className={`switch ${g.enabled ? 'on' : ''}`}
                    role="switch"
                    aria-checked={g.enabled}
                    disabled={busy}
                    onClick={async () => {
                      const next = { ...cfg }
                      const idx = next.gateways.findIndex((x) => x.id === g.id)
                      if (idx >= 0) next.gateways[idx] = { ...g, enabled: !g.enabled }
                      await save(next)
                    }}
                  />
                  <button
                    className="btn gho im-gw-edit"
                    onClick={() => {
                      setAdding(true)
                      setEditingId(g.id)
                      setAddType(g.type)
                      setAddDraft({ ...initDraftWithDefaults(schemaByType[g.type]), ...g.config })
                    }}
                  >
                    {t('settings.im.edit')}
                  </button>
                  <button
                    className="btn dan im-gw-del"
                    onClick={async () => {
                      const next = { ...cfg, gateways: cfg.gateways.filter((x) => x.id !== g.id) }
                      await save(next)
                    }}
                  >
                    {t('settings.im.del')}
                  </button>
                </div>
                {/* 渠道级路由（始终显示：空状态 + 添加入口） */}
                <div className="im-gw-sub">
                  <div className="im-gw-sub-head">
                    <span className="im-gw-sub-title">{t('settings.im.routes')}</span>
                    <button
                      className="btn gho im-gw-sub-add"
                      onClick={() => setAddingTo((m) => ({ ...m, [g.id]: addingTo[g.id] === 'route' ? undefined : 'route' }))}
                    >
                      ＋ {t('settings.im.addRoute')}
                    </button>
                  </div>
                  {routes.length === 0 && !addingTo[g.id] && (
                    <p className="im-gw-empty">{t('settings.im.noRoutes')}</p>
                  )}
                  {routes.map((r, i) => (
                    <div className="im-route" key={i}>
                      <span className="im-route-chat">
                        {r.chat.gateway}:{r.chat.chat_id}
                        {r.chat.thread_id ? `:${r.chat.thread_id}` : ''}
                      </span>
                      <span className="im-route-arrow">→</span>
                      <span className="im-route-ws">{r.workspace}</span>
                      <button
                        className="btn dan im-gw-del"
                        onClick={async () => {
                          const next = { ...cfg }
                          const gi = next.gateways.findIndex((x) => x.id === g.id)
                          if (gi >= 0) {
                            next.gateways[gi] = {
                              ...next.gateways[gi],
                              routes: (next.gateways[gi].routes ?? []).filter((_, j) => j !== i),
                            }
                          }
                          await save(next)
                        }}
                      >
                        {t('settings.im.del')}
                      </button>
                    </div>
                  ))}
                  {addingTo[g.id] === 'route' && (
                    <RouteAddRow
                      gatewayId={g.id}
                      gatewayType={g.type}
                      cfg={cfg}
                      save={save}
                      onDone={() => setAddingTo((m) => ({ ...m, [g.id]: undefined }))}
                    />
                  )}
                </div>
                {/* 渠道级授权名单（始终显示：空状态 + 添加入口） */}
                <div className="im-gw-sub">
                  <div className="im-gw-sub-head">
                    <span className="im-gw-sub-title">{t('settings.im.allowUsers')}</span>
                    <button
                      className="btn gho im-gw-sub-add"
                      onClick={() => setAddingTo((m) => ({ ...m, [g.id]: addingTo[g.id] === 'allow' ? undefined : 'allow' }))}
                    >
                      ＋ {t('settings.im.addUser')}
                    </button>
                  </div>
                  {allowUsers.length === 0 && !addingTo[g.id] && (
                    <p className="im-gw-empty">{t('settings.im.noUsers')}</p>
                  )}
                  {allowUsers.map((u, i) => (
                    <div className="im-route" key={u}>
                      <span className="im-route-chat">{u}</span>
                      <button
                        className="btn dan im-gw-del"
                        onClick={async () => {
                          const next = { ...cfg }
                          const gi = next.gateways.findIndex((x) => x.id === g.id)
                          if (gi >= 0) {
                            next.gateways[gi] = {
                              ...next.gateways[gi],
                              allow_users: (next.gateways[gi].allow_users ?? []).filter((_, j) => j !== i),
                            }
                          }
                          await save(next)
                        }}
                      >
                        {t('settings.im.del')}
                      </button>
                    </div>
                  ))}
                  {addingTo[g.id] === 'allow' && (
                    <AllowAddRow
                      gatewayId={g.id}
                      cfg={cfg}
                      save={save}
                      onDone={() => setAddingTo((m) => ({ ...m, [g.id]: undefined }))}
                    />
                  )}
                </div>
              </div>
            )
          })
        )}
      </div>

      {/* 添加/编辑表单 */}
      {adding && (
        <>
          {!editingId && (
            <div className="im-field">
              <label className="im-label">{t('settings.im.gatewayType')}</label>
              <Sel
                value={addType}
                options={imSchemas.map((s) => ({ value: s.type, label: s.name }))}
                onChange={(v) => { setAddType(v); setAddDraft(initDraftWithDefaults(schemaByType[v])) }}
              />
            </div>
          )}
          {renderAddForm()}
        </>
      )}

      {err && <p className="im-err">{err}</p>}
      {busy && <p className="im-help">{t('settings.im.saving')}</p>}
    </div>
  )
}

// RouteAddRow 行内添加路由（Chat → workspace）。
interface RouteAddRowProps {
  gatewayId: string
  gatewayType: string
  cfg: IMConfig
  save: (next: IMConfig) => Promise<boolean>
  onDone: () => void
}

function RouteAddRow({ gatewayId, gatewayType, cfg, save, onDone }: RouteAddRowProps) {
  const t = useT()
  const workspace = useAppStore((s) => s.workspace)
  const projects = useAppStore((s) => s.projects)
  const imChats = useAppStore((s) => s.imChats)
  const fetchIMChats = useAppStore((s) => s.fetchIMChats)
  const [chatId, setChatId] = useState('')
  const [ws, setWs] = useState('')
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const candidates = Array.from(new Set([
    ...(workspace ? [workspace] : []),
    ...projects.map((p) => p.path),
  ].filter(Boolean)))

  // 群列表：打开时拉取（无缓存或缓存空）
  const chats = imChats[gatewayId]
  useEffect(() => {
    if (!chats) fetchIMChats(gatewayId)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gatewayId, chats])

  const submit = async () => {
    const cid = chatId.trim()
    if (!cid) { setErr(t('settings.im.errChatId')); return }
    if (!ws) { setErr(t('settings.im.errSelectWs')); return }
    setErr(null)
    setBusy(true)
    const next = { ...cfg }
    const gi = next.gateways.findIndex((x) => x.id === gatewayId)
    if (gi < 0) { setBusy(false); return }
    const routes = next.gateways[gi].routes ?? []
    // 同 chat 已存在 → 替换
    const existing = routes.findIndex((r) => r.chat.gateway === gatewayType && r.chat.chat_id === cid)
    const newRoute = { chat: { gateway: gatewayType, chat_id: cid }, workspace: ws }
    if (existing >= 0) routes[existing] = newRoute
    else routes.push(newRoute)
    next.gateways[gi] = { ...next.gateways[gi], routes }
    if (await save(next)) {
      setChatId('')
      setWs('')
      onDone()
    }
    setBusy(false)
  }

  return (
    <div className="im-route-add">
      {chats && chats.length > 0 ? (
        <Sel
          value={chatId}
          options={[
            { value: '', label: t('settings.im.selectChat') },
            ...chats.map((c) => ({ value: c.chat_id, label: c.name || c.chat_id, hint: c.name ? c.chat_id : undefined })),
          ]}
          onChange={setChatId}
          style={{ flex: 1 }}
        />
      ) : (
        <input
          className="im-inp"
          placeholder={chats ? t('settings.im.noChats') : t('settings.im.loadingChats')}
          value={chatId}
          onChange={(e) => setChatId(e.target.value)}
          style={{ flex: 1 }}
        />
      )}
      <Sel
        value={ws}
        options={[
          { value: '', label: t('settings.im.bindSelectWorkspace') },
          ...candidates.map((p) => ({ value: p, label: p })),
        ]}
        onChange={setWs}
        style={{ flex: 1 }}
      />
      {err && <span className="im-err" style={{ marginLeft: 6 }}>{err}</span>}
      <button className="btn pri" disabled={busy} onClick={submit}>✓</button>
      <button className="btn gho" onClick={onDone}>✕</button>
    </div>
  )
}

// AllowAddRow 行内添加授权用户。
interface AllowAddRowProps {
  gatewayId: string
  cfg: IMConfig
  save: (next: IMConfig) => Promise<boolean>
  onDone: () => void
}

function AllowAddRow({ gatewayId, cfg, save, onDone }: AllowAddRowProps) {
  const t = useT()
  const [uid, setUid] = useState('')
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const submit = async () => {
    const u = uid.trim()
    if (!u) { setErr(t('settings.im.errUserId')); return }
    setErr(null)
    setBusy(true)
    const next = { ...cfg }
    const gi = next.gateways.findIndex((x) => x.id === gatewayId)
    if (gi < 0) { setBusy(false); return }
    const users = next.gateways[gi].allow_users ?? []
    if (!users.includes(u)) users.push(u)
    next.gateways[gi] = { ...next.gateways[gi], allow_users: users }
    if (await save(next)) {
      setUid('')
      onDone()
    }
    setBusy(false)
  }

  return (
    <div className="im-route-add">
      <input
        className="im-inp"
        placeholder={t('settings.im.allowUserIdPlaceholder')}
        value={uid}
        onChange={(e) => setUid(e.target.value)}
        style={{ flex: 1 }}
      />
      {err && <span className="im-err" style={{ marginLeft: 6 }}>{err}</span>}
      <button className="btn pri" disabled={busy} onClick={submit}>✓</button>
      <button className="btn gho" onClick={onDone}>✕</button>
    </div>
  )
}
