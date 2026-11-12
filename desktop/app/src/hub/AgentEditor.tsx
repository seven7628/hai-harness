import { useEffect, useMemo, useState } from 'react'
import { hubCommand } from './api'
import type { HubAgent } from './types'
import { useT } from '../i18n'
import { useAppStore } from '../store/useAppStore'
import AgentSigil from './AgentSigil'
import { useAgentState } from './useAgentState'
import { buildAgentPatch, buildAgentProfile, initialDailyBudget, type AgentForm } from './agentPatch'
import {
  EFFORT_VALUES, chooseModelOnProviderSwitch, effortChoices, modelsForProvider,
  newAgentDefaults, providerOptions, unlistedValue,
} from './agentModelOptions'

// 智能体资料页（§8.5）：新建/编辑，5 个页签。
//
// 页签与数据源：
//   基本       profile 标量（名字/头像/色/角色/职责/provider/model/档位/压缩阈值）
//   人格       instructions 与 persona（hub 会话的人格由 Hub 装配决定，persona 只展示）
//   工具与权限 tools.preset / disable / allow / disable_network / mcp.disable（前端只编辑
//              最常用的三项，其余保持原值 —— 全量编辑器留到工具面板）
//   记忆       hub_agent_memory（列表/读）+ hub_agent_memory_write（写前备份）
//   版本       hub_agent_versions / hub_agent_rollback / hub_agent_prompt_preview
//
// 保存语义：create 走 hub_agent_create（服务端分配 id）；update 走 hub_agent_update 的
// patch（只发改过的字段，避免把列表里没带的字段写成零值）。

type Tab = 'basic' | 'persona' | 'tools' | 'memory' | 'versions'

interface MemoryEntry {
  name: string
  size: number
  modified?: string
}

interface VersionRow {
  version: number
  saved_at?: string
  name?: string
  role?: string
  provider?: string
  model?: string
}

interface PromptLayer {
  key: string
  title: string
  text: string
}

export default function AgentEditor({
  agent,
  onClose,
  onSaved,
}: {
  agent?: HubAgent | null
  onClose: () => void
  onSaved: (agentID: string) => void
}) {
  const t = useT()
  const isNew = !agent
  // 下拉选项来自设置（Provider/Models 在设置页维护），不是手输自由文本。
  const providerSettings = useAppStore((s) => s.settings.provider)
  const fetchedModels = useAppStore((s) => s.fetchedModels)
  const globalModel = useAppStore((s) => s.model)
  const [tab, setTab] = useState<Tab>('basic')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // —— 基本 ——
  const [name, setName] = useState(agent?.name ?? '')
  // emoji 只作为旧数据兼容字段随 patch 原样带回；头像视觉由 id + role 自动生成。
  const [emoji] = useState(agent?.avatar?.value ?? '🟢')
  const [accent, setAccent] = useState(agent?.accent ?? '#7bc199')
  const [role, setRole] = useState(agent?.role ?? '')
  const [description, setDescription] = useState(agent?.description ?? '')
  // 新智能体默认 = 设置里的 active Provider + 当前全局模型（属于该 Provider 时）；
  // 编辑已有智能体原样保留，未知 Provider/Model 也不会被静默改写。
  const [defaults] = useState(() => {
    if (agent) return { provider: agent.provider ?? '', model: agent.model ?? '' }
    const st = useAppStore.getState()
    return newAgentDefaults(st.settings.provider, st.fetchedModels, st.model)
  })
  const [provider, setProvider] = useState(defaults.provider)
  const [model, setModel] = useState(defaults.model)
  const [effort, setEffort] = useState(agent?.effort ?? '')
  const [threshold, setThreshold] = useState(agent?.compress_threshold ?? 0.8)
  const [dailyBudget, setDailyBudget] = useState(initialDailyBudget(agent))
  const [taskBudget, setTaskBudget] = useState(agent?.policy?.max_task_cost_usd ?? 0)
  const [memoryOn, setMemoryOn] = useState(agent?.memory?.enabled !== false)

  // —— 人格 ——
  const [instructions, setInstructions] = useState(agent?.instructions ?? '')

  // —— 工具与权限 ——
  const [preset, setPreset] = useState(agent?.tools?.preset ?? 'all')
  const [disable, setDisable] = useState((agent?.tools?.disable ?? []).join(' '))

  // —— 记忆 ——
  const [entries, setEntries] = useState<MemoryEntry[]>([])
  const [memoryPath, setMemoryPath] = useState('MEMORY.md')
  const [memoryBody, setMemoryBody] = useState('')
  const [memoryExists, setMemoryExists] = useState(false)
  const [memoryNote, setMemoryNote] = useState('')
  const [note, setNote] = useState('')

  // —— 版本 ——
  const [versions, setVersions] = useState<VersionRow[]>([])
  const [currentVersion, setCurrentVersion] = useState(agent?.version ?? 0)
  const [layers, setLayers] = useState<PromptLayer[] | null>(null)
  const [previewNote, setPreviewNote] = useState('')

  const agentID = agent?.id ?? ''
  // 预览用真实状态：编辑既有智能体时，头像与列表/消息里保持一致。
  const avatarState = useAgentState(agentID || undefined, agent?.deleted)
  const tabs = useMemo<{ key: Tab; label: string }[]>(
    () => [
      { key: 'basic', label: t('hub.tab.basic') },
      { key: 'persona', label: t('hub.tab.persona') },
      { key: 'tools', label: t('hub.tab.tools') },
      { key: 'memory', label: t('hub.tab.memory') },
      { key: 'versions', label: t('hub.tab.versions') },
    ],
    [t],
  )

  // 模型/档位下拉的候选值：过滤/并集/排序都在 agentModelOptions 里（有 node 回归）。
  const providers = useMemo(() => providerOptions(providerSettings), [providerSettings])
  const providerIds = useMemo(() => providers.map((p) => p.id), [providers])
  const models = useMemo(
    () => modelsForProvider(providerSettings, fetchedModels, provider),
    [providerSettings, fetchedModels, provider],
  )
  const unknownProvider = unlistedValue(provider, providerIds)
  const unknownModel = unlistedValue(model, models)
  const unknownEffort = unlistedValue(effort, EFFORT_VALUES)
  const efforts = useMemo(() => effortChoices(), [])

  // 冷启动直接进 Hub 时设置可能晚于编辑器挂载：新智能体仍是空 Provider → 补一次默认值。
  useEffect(() => {
    if (!isNew || provider) return
    const d = newAgentDefaults(providerSettings, fetchedModels, globalModel)
    if (!d.provider) return
    setProvider(d.provider)
    setModel((cur) => cur || d.model)
  }, [isNew, provider, providerSettings, fetchedModels, globalModel])

  /** 切 Provider：模型仍在新列表就留着，否则落到第一个可用模型（没有就清空）。 */
  function changeProvider(next: string): void {
    setProvider(next)
    setModel((cur) => chooseModelOnProviderSwitch(
      cur,
      modelsForProvider(providerSettings, fetchedModels, next),
    ))
  }

  // Esc 关闭（遮罩点击已支持；键盘用户不该只能点鼠标）。
  useEffect(() => {
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape' && !busy) onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [busy, onClose])

  useEffect(() => {
    if (!agentID) return
    void loadMemory()
    void loadVersions()
    // 只在切智能体时拉一次；页签内的刷新是显式动作。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentID])

  async function loadMemory(): Promise<void> {
    if (!agentID) return
    const list = await hubCommand<{ entries?: MemoryEntry[] }>('hub_agent_memory', { agent_id: agentID })
    if (!list.ok) {
      setError(list.error ?? null)
      return
    }
    setEntries(list.data?.entries ?? [])
    await openMemoryFile(memoryPath)
  }

  async function openMemoryFile(path: string): Promise<void> {
    if (!agentID) return
    setMemoryPath(path)
    const r = await hubCommand<{ content?: string; exists?: boolean }>('hub_agent_memory', {
      agent_id: agentID,
      path,
    })
    if (!r.ok) {
      setError(r.error ?? null)
      return
    }
    setMemoryBody(r.data?.content ?? '')
    setMemoryExists(r.data?.exists === true)
    setMemoryNote('')
  }

  async function saveMemory(): Promise<void> {
    if (!agentID) return
    setBusy(true)
    const r = await hubCommand<{ backup?: string; effective?: string; warning?: string }>('hub_agent_memory_write', {
      agent_id: agentID,
      path: memoryPath,
      content: memoryBody,
    })
    setBusy(false)
    if (!r.ok) {
      setError(r.error ?? null)
      return
    }
    const backup = r.data?.backup ? t('hub.memory.backup').replace('{name}', r.data.backup) : t('hub.memory.newFile')
    const when = r.data?.effective === 'next_session' ? t('hub.effective.nextSession') : t('hub.effective.nextRun')
    // 先重载清单（重载会把 note 清空），再写回执 —— 否则用户永远看不到「已备份为 xxx」。
    await loadMemory()
    setMemoryNote(`${backup} · ${when}${r.data?.warning ? ` · ${r.data.warning}` : ''}`)
  }

  async function loadVersions(): Promise<void> {
    if (!agentID) return
    const r = await hubCommand<{ versions?: VersionRow[]; current?: number }>('hub_agent_versions', {
      agent_id: agentID,
      limit: 20,
    })
    if (!r.ok) {
      setError(r.error ?? null)
      return
    }
    setVersions(r.data?.versions ?? [])
    setCurrentVersion(r.data?.current ?? 0)
  }

  async function rollback(version: number): Promise<void> {
    if (!agentID) return
    if (!window.confirm(t('hub.versions.confirm').replace('{v}', String(version)))) return
    setBusy(true)
    const r = await hubCommand<{ agent?: HubAgent; warning?: string }>('hub_agent_rollback', {
      agent_id: agentID,
      version,
    })
    setBusy(false)
    if (!r.ok) {
      setError(r.error ?? null)
      return
    }
    setError(r.data?.warning ?? null)
    setNote(
      t('hub.versions.rolledBack')
        .replace('{from}', String(version))
        .replace('{to}', String(r.data?.agent?.version ?? 0)),
    )
    void loadVersions()
    onSaved(agentID)
  }

  async function previewPrompt(): Promise<void> {
    if (!agentID) return
    const r = await hubCommand<{ layers?: PromptLayer[]; note?: string }>('hub_agent_prompt_preview', {
      agent_id: agentID,
    })
    if (!r.ok) {
      setError(r.error ?? null)
      return
    }
    setLayers(r.data?.layers ?? [])
    setPreviewNote(r.data?.note ?? '')
  }

  async function save(): Promise<void> {
    if (!name.trim()) {
      setError(t('hub.err.nameRequired'))
      return
    }
    if (!provider.trim() || !model.trim()) {
      setError(t('hub.err.providerRequired'))
      return
    }
    setBusy(true)
    setError(null)
    // 表单 → payload 是纯函数（agentPatch.ts）：只发改动字段，tools.disable 不会被清空。
    const form: AgentForm = {
      name, emoji, accent, role, description, instructions, provider, model, effort,
      threshold, memoryOn, preset, disable,
      dailyBudget, taskBudget,
    }
    const patch = buildAgentPatch(form, agent)
    if (isNew) {
      const profile = buildAgentProfile(form)
      const r = await hubCommand<{ agent?: HubAgent }>('hub_agent_create', { profile })
      setBusy(false)
      if (!r.ok || !r.data?.agent) {
        setError(r.error ?? t('hub.err.create'))
        return
      }
      onSaved(r.data.agent.id)
      onClose()
      return
    }
    const r = await hubCommand<{ agent?: HubAgent; warning?: string; effective?: string }>('hub_agent_update', {
      agent_id: agentID,
      patch,
    })
    setBusy(false)
    if (!r.ok) {
      setError(r.error ?? t('hub.err.save'))
      return
    }
    onSaved(agentID)
    if (r.data?.warning) {
      setError(r.data.warning)
      void loadVersions() // 版本号变了：卡片「第 N 版」要跟着刷新
    } else {
      onClose()
    }
  }

  async function remove(): Promise<void> {
    if (!agentID) return
    if (!window.confirm(t('hub.confirmDelete'))) return
    setBusy(true)
    const r = await hubCommand('hub_agent_delete', { agent_id: agentID })
    setBusy(false)
    if (!r.ok) {
      setError(r.error ?? t('hub.err.delete'))
      return
    }
    onSaved('')
    onClose()
  }

  return (
    <div className="hub-modal-scrim" role="dialog" aria-modal="true" onClick={onClose}>
      <div className="hub-modal" onClick={(e) => e.stopPropagation()}>
        <header className="hub-modal-head">
          <span className="hub-modal-title">{isNew ? t('hub.newAgent') : t('hub.editAgent')}</span>
          <button className="hub-link" onClick={onClose}>{t('hub.close')}</button>
        </header>
        <div className="hub-modal-body">
          <aside className="hub-preview-card">
            <AgentSigil
              agentId={agentID || undefined}
              name={name}
              responsibility={role}
              state={avatarState}
              size={48}
            />
            <strong>{name || t('hub.unnamed')}</strong>
            <span className="hub-sub">{role || '—'}</span>
            <span className="hub-sub">{[provider, model].filter(Boolean).join(' · ') || '—'}</span>
            <span className="hub-sub">{t('hub.versionLabel').replace('{v}', String(currentVersion || 1))}</span>
          </aside>

          <section className="hub-tab-area">
            <nav className="hub-tabs" role="tablist">
              {tabs.map((x) => (
                <button
                  key={x.key}
                  role="tab"
                  aria-selected={tab === x.key}
                  className={tab === x.key ? 'on' : ''}
                  onClick={() => setTab(x.key)}
                >
                  {x.label}
                </button>
              ))}
            </nav>

            <div className="hub-tab-body">
              {tab === 'basic' ? (
                <div className="hub-form">
                  <label>{t('hub.field.name')}
                    <input value={name} onChange={(e) => setName(e.target.value)} placeholder="Researcher" />
                  </label>
                  <p className="hub-avatar-note">{t('hub.avatar.autoNote')}</p>
                  <label>{t('hub.field.accent')}
                    <input type="color" value={accent} onChange={(e) => setAccent(e.target.value)} />
                  </label>
                  <label>{t('hub.field.role')}
                    <input value={role} onChange={(e) => setRole(e.target.value)} placeholder={t('hub.field.role.ph')} />
                  </label>
                  <label className="wide">{t('hub.field.description')}
                    <textarea value={description} onChange={(e) => setDescription(e.target.value)} rows={2} />
                  </label>
                  <label>{t('hub.field.provider')}
                    <select value={provider} onChange={(e) => changeProvider(e.target.value)}>
                      {providers.length === 0 ? (
                        <option value="" disabled>{t('hub.provider.empty')}</option>
                      ) : provider === '' ? (
                        <option value="" disabled>{t('hub.provider.choose')}</option>
                      ) : null}
                      {/* 当前 Provider 不在设置里（改名/已删）：保留可见，绝不静默改写。 */}
                      {unknownProvider ? (
                        <option value={unknownProvider} disabled>
                          {unknownProvider} · {t('hub.option.unlisted')}
                        </option>
                      ) : null}
                      {providers.map((p) => (
                        <option key={p.id} value={p.id}>{p.label}</option>
                      ))}
                    </select>
                    {providers.length === 0 && unknownProvider ? (
                      <span className="hub-hint">{t('hub.provider.empty')}</span>
                    ) : null}
                  </label>
                  <label>{t('hub.field.model')}
                    <select value={model} onChange={(e) => setModel(e.target.value)}>
                      {provider === '' ? (
                        <option value="" disabled>{t('hub.provider.empty')}</option>
                      ) : models.length === 0 ? (
                        <option value="" disabled>{t('hub.model.empty')}</option>
                      ) : model === '' ? (
                        <option value="" disabled>{t('hub.model.choose')}</option>
                      ) : null}
                      {/* 当前 Model 不在该 Provider 的列表里：保留可见，避免保存时被顺手改掉。 */}
                      {unknownModel ? (
                        <option value={unknownModel} disabled>
                          {unknownModel} · {t('hub.option.unlisted')}
                        </option>
                      ) : null}
                      {models.map((m) => (
                        <option key={m} value={m}>{m}</option>
                      ))}
                    </select>
                    {provider !== '' && models.length === 0 && unknownModel ? (
                      <span className="hub-hint">{t('hub.model.empty')}</span>
                    ) : null}
                  </label>
                  <label>{t('hub.field.effort')}
                    <select value={effort} onChange={(e) => setEffort(e.target.value)}>
                      {efforts.map((choice) => (
                        <option key={choice.value || 'default'} value={choice.value}>
                          {choice.value ? choice.label : t('hub.effort.default')}
                        </option>
                      ))}
                      {/* 未知档位（旧数据/手写配置）原样保留，不转成任何档位。 */}
                      {unknownEffort ? (
                        <option value={unknownEffort} disabled>
                          {unknownEffort} · {t('hub.option.unlisted')}
                        </option>
                      ) : null}
                    </select>
                  </label>

                  {/* 预算/压缩/记忆收进折叠区：第一屏只留身份 + 模型。 */}
                  <details className="hub-advanced">
                    <summary>{t('hub.advanced')}</summary>
                    <div className="hub-advanced-body">
                      <div className="hub-form">
                        <label>{t('hub.field.threshold')}
                          <input
                            type="range"
                            min={0.5}
                            max={0.95}
                            step={0.01}
                            value={threshold}
                            onChange={(e) => setThreshold(Number(e.target.value))}
                          />
                          <span className="hub-sub">{threshold.toFixed(2)}</span>
                        </label>
                        <label>{t('hub.field.dailyBudget')}
                          <input
                            type="number"
                            min={0}
                            step={1}
                            value={dailyBudget}
                            onChange={(e) => setDailyBudget(Math.max(0, Number(e.target.value) || 0))}
                          />
                        </label>
                        <label>{t('hub.field.taskBudget')}
                          <input
                            type="number"
                            min={0}
                            step={0.5}
                            value={taskBudget}
                            onChange={(e) => setTaskBudget(Math.max(0, Number(e.target.value) || 0))}
                          />
                        </label>
                        <p className="hub-hint">{t('hub.budgetHint')}</p>
                        <label className="inline">
                          <input type="checkbox" checked={memoryOn} onChange={(e) => setMemoryOn(e.target.checked)} />
                          {t('hub.field.memory')}
                        </label>
                      </div>
                    </div>
                  </details>
                </div>
              ) : null}

              {tab === 'persona' ? (
                <div className="hub-form">
                  <label className="wide">{t('hub.field.instructions')}
                    <textarea
                      value={instructions}
                      onChange={(e) => setInstructions(e.target.value)}
                      rows={10}
                      placeholder={t('hub.field.instructions.ph')}
                    />
                  </label>
                  <p className="hub-muted">{t('hub.personaNote')}</p>
                </div>
              ) : null}

              {tab === 'tools' ? (
                <div className="hub-form">
                  <label>{t('hub.field.preset')}
                    <select value={preset} onChange={(e) => setPreset(e.target.value)}>
                      <option value="all">{t('hub.preset.all')}</option>
                      <option value="readonly">{t('hub.preset.readonly')}</option>
                      <option value="custom">{t('hub.preset.custom')}</option>
                    </select>
                  </label>
                  <label className="wide">{t('hub.field.disable')}
                    <textarea
                      value={disable}
                      onChange={(e) => setDisable(e.target.value)}
                      rows={3}
                      placeholder="browser_*  mcp__github__*"
                    />
                  </label>
                  <p className="hub-muted">{t('hub.toolsNote')}</p>
                </div>
              ) : null}

              {tab === 'memory' ? (
                <div className="hub-memory">
                  {isNew ? <p className="hub-muted">{t('hub.memory.needSave')}</p> : null}
                  {!isNew ? (
                    <>
                      <div className="hub-memory-list">
                        {entries.length === 0 ? <span className="hub-muted">{t('hub.memory.empty')}</span> : null}
                        {entries.map((e) => (
                          <button
                            key={e.name}
                            className={`hub-memory-item ${e.name === memoryPath ? 'on' : ''}`}
                            onClick={() => void openMemoryFile(e.name)}
                          >
                            {e.name}
                          </button>
                        ))}
                        {entries.some((e) => e.name === 'MEMORY.md') ? null : (
                          <button className="hub-memory-item" onClick={() => void openMemoryFile('MEMORY.md')}>
                            MEMORY.md
                          </button>
                        )}
                      </div>
                      <div className="hub-memory-editor">
                        <div className="hub-sub">
                          {memoryPath} {memoryExists ? '' : `· ${t('hub.memory.newTag')}`}
                        </div>
                        <textarea value={memoryBody} onChange={(e) => setMemoryBody(e.target.value)} rows={10} />
                        <div className="hub-row-actions">
                          <button className="hub-primary" onClick={() => void saveMemory()} disabled={busy}>
                            {t('hub.memory.save')}
                          </button>
                          {memoryNote ? <span className="hub-sub">{memoryNote}</span> : null}
                        </div>
                      </div>
                    </>
                  ) : null}
                </div>
              ) : null}

              {tab === 'versions' ? (
                <div className="hub-versions">
                  {isNew ? <p className="hub-muted">{t('hub.versions.needSave')}</p> : null}
                  {!isNew ? (
                    <>
                      <div className="hub-row-actions">
                        <button className="hub-primary" onClick={() => void previewPrompt()}>
                          {t('hub.versions.preview')}
                        </button>
                        <button className="hub-link" onClick={() => void loadVersions()}>{t('hub.refresh')}</button>
                      </div>
                      {layers ? (
                        <div className="hub-layers">
                          {layers.map((l) => (
                            <details key={l.key} open={l.key === 'protocol'}>
                              <summary>
                                {l.title} <span className="hub-sub">{l.text.length} chars</span>
                              </summary>
                              <pre>{l.text}</pre>
                            </details>
                          ))}
                          {previewNote ? <p className="hub-muted">{previewNote}</p> : null}
                        </div>
                      ) : null}
                      <div className="hub-version-list">
                        <div className="hub-sub">{t('hub.versions.current').replace('{v}', String(currentVersion))}</div>
                        {versions.length === 0 ? <span className="hub-muted">{t('hub.versions.empty')}</span> : null}
                        {versions.map((v) => (
                          <div className="hub-version-row" key={v.version}>
                            <span>v{v.version}</span>
                            <span className="hub-sub">{v.name ?? ''} {v.model ? `· ${v.model}` : ''}</span>
                            <button className="hub-link" disabled={busy} onClick={() => void rollback(v.version)}>
                              {t('hub.versions.rollback')}
                            </button>
                          </div>
                        ))}
                      </div>
                    </>
                  ) : null}
                </div>
              ) : null}
            </div>

            {error ? <div className="hub-error" onClick={() => setError(null)}>{error}</div> : null}
            {note ? (
              <div className="hub-notice" role="status" onClick={() => setNote('')}>
                {note}
              </div>
            ) : null}

            <footer className="hub-modal-foot">
              {/* 删除放在 footer 左侧次要位：原先贴在预览卡里紧挨内容，鼠标往上走就点到，
                  又是不可撤销操作（HUB-EDITOR-01）。这里与「取消 / 保存」隔开一整行宽。 */}
              {!isNew ? (
                <button className="hub-danger" onClick={() => void remove()} disabled={busy}>
                  {t('hub.delete')}
                </button>
              ) : null}
              <button className="hub-link" onClick={onClose}>{t('hub.cancel')}</button>
              <button className="hub-primary" onClick={() => void save()} disabled={busy}>
                {busy ? t('hub.saving') : t('hub.save')}
              </button>
            </footer>
          </section>
        </div>
      </div>
    </div>
  )
}
