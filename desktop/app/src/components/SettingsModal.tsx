import { useEffect, useRef, useState } from 'react'
import type { ReactElement } from 'react'
import { useAppStore, KNOWN_MODEL_MAX_TOKENS, MODEL_MAX_TOKENS_DEFAULT, SUBAGENT_CONCURRENCY_DEFAULT, inputTypesForProvider } from '../store/useAppStore'
import { useT } from '../i18n'
import { meshUUID, meshToken } from '../lib/meshRand'
import { copyText } from '../lib/clipboard'
import { AccessIcon, BarChart, ChevronDown, EyeIcon, EyeOffIcon, Gear, Globe, HookIcon, ImageIcon, PaletteIcon, SendIcon, SessionIcon, Sparkles, TerminalIcon } from './icons'
import { BrowserSettingsEditor } from './BrowserSettingsEditor'
import { ComputerSettingsEditor } from './ComputerSettingsEditor'
import { IMSettingsEditor } from './IMSettingsEditor'
import AppearanceTab from './AppearanceTab'
import { MCPTab } from './MCPTab'
import { HooksTab } from './HooksTab'
import UsageStats from './UsageStats'
import type { ProviderCfg, ProviderPreset, MeshSettings, AppSettings } from '../transport/types'
import { getTransport } from '../transport'

// 左侧导航分组（对齐设计稿：基础项 → 模型 → Agent 能力 → 数据与统计）。
// 表驱动而非七段硬编码：新增模块只加一行，分组标题由 label 决定（null = 不分组，顶部基础项）。
type SettingsTab = 'general' | 'appearance' | 'shortcuts' | 'provider' | 'mcp' | 'hooks' | 'plugin' | 'im' | 'mesh' | 'usage'
const NAV_GROUPS: { id: string; label: string | null; items: { key: SettingsTab; label: string; icon: ReactElement }[] }[] = [
  {
    id: 'base',
    label: null,
    items: [
      { key: 'general', label: 'settings.general', icon: <Gear size={14} /> },
      { key: 'appearance', label: 'settings.appearance', icon: <PaletteIcon size={14} /> },
      { key: 'shortcuts', label: 'settings.shortcuts', icon: <TerminalIcon size={14} /> },
    ],
  },
  {
    id: 'model',
    label: 'settings.nav.model',
    items: [{ key: 'provider', label: 'settings.provider', icon: <Sparkles size={14} /> }],
  },
  {
    id: 'agent',
    label: 'settings.nav.agent',
    items: [
      { key: 'mcp', label: 'settings.mcp', icon: <Globe size={14} /> },
      { key: 'hooks', label: 'settings.hooks', icon: <HookIcon size={14} /> },
      { key: 'plugin', label: 'settings.plugin', icon: <AccessIcon size={14} /> },
      { key: 'im', label: 'settings.im', icon: <SessionIcon size={14} /> },
      { key: 'mesh', label: 'settings.mesh', icon: <SendIcon size={14} /> },
    ],
  },
  {
    id: 'data',
    label: 'settings.nav.data',
    items: [{ key: 'usage', label: 'settings.usage', icon: <BarChart size={14} /> }],
  },
]

// 设置弹窗：两栏（左窄导航 + 右宽内容），背景虚化（.modal-hint.blur backdrop-filter）。
// 分区：通用设置（主题/语言）、快捷键（只读）、Provider（OpenAI/DeepSeek）、MCP（Server 管理）。
// 持久化：settings.json（~/.go-code；mock=localStorage）；API key 明文落 settings.json provider 段
//（providerSave，2026-08-16 用户决策；key 输入框草稿保存后清空不回显，providerKeys 指示已设置）。
export default function SettingsModal() {
  const t = useT()
  const inSettings = useAppStore((s) => s.inSettings)
  const closeSettings = useAppStore((s) => s.closeSettings)
  const [tab, setTab] = useState<SettingsTab>('general')

  // 打开时重置到通用；Esc 关闭
  useEffect(() => {
    if (inSettings) setTab('general')
  }, [inSettings])
  useEffect(() => {
    if (!inSettings) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') closeSettings()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [inSettings, closeSettings])

  if (!inSettings) return null

  return (
    <>
      {/* 背景虚化遮罩：独立 fixed 元素；卡片是它的兄弟（非后代）→ 任何环境都不会被 backdrop-filter 糊到 */}
      <div className="settings-overlay" onClick={closeSettings} aria-hidden="true" />
      <div className="card settings-card settings-pop" role="dialog" aria-modal="true" aria-label={t('settings.title')}>
        <nav className="settings-nav" aria-label={t('settings.title')}>
          <div className="settings-title-row">
            <div className="settings-title">{t('settings.title')}</div>
            <button className="settings-close" onClick={closeSettings} aria-label={t('settings.close')} title={t('settings.close')}>
              ✕
            </button>
          </div>
          {NAV_GROUPS.map((group) => (
            <div className="settings-nav-group" key={group.id}>
              {group.label && <div className="settings-nav-sec">{t(group.label)}</div>}
              {group.items.map((item) => (
                <button
                  key={item.key}
                  className={`settings-nav-item ${tab === item.key ? 'cur' : ''}`}
                  onClick={() => setTab(item.key)}
                  aria-current={tab === item.key}
                >
                  {item.icon}
                  <span>{t(item.label)}</span>
                </button>
              ))}
            </div>
          ))}
        </nav>
        <div className="settings-body">
          {tab === 'general' && <GeneralTab />}
          {tab === 'appearance' && <AppearanceTab />}
          {tab === 'shortcuts' && <ShortcutsTab />}
          {tab === 'provider' && <ProviderTab />}
          {tab === 'mcp' && <MCPTab />}
          {tab === 'hooks' && <HooksTab />}
          {tab === 'plugin' && <PluginTab />}
          {tab === 'im' && <IMTab />}
          {tab === 'mesh' && <MeshTab />}
          {tab === 'usage' && <UsageStats />}
        </div>
      </div>
    </>
  )
}

// —— 语音输入（STT）厂商预设：OpenAI 兼容 /audio/transcriptions 端点 ——
// 选中厂商自动填 base_url + 默认模型；地址与模型均可继续手改（改完落盘，切走再切回保留）。
// custom_models：用户在该厂商下追加的模型名（持久化到 settings.stt.custom_models[vendorId]）。
const STT_PRESETS: { id: string; label: string; base_url: string; models: string[]; default_model: string }[] = [
  { id: 'openai', label: 'OpenAI', base_url: 'https://api.openai.com/v1', models: ['whisper-1', 'gpt-4o-transcribe', 'gpt-4o-mini-transcribe'], default_model: 'whisper-1' },
  { id: 'groq', label: 'Groq', base_url: 'https://api.groq.com/openai/v1', models: ['whisper-large-v3-turbo', 'whisper-large-v3', 'distil-whisper-large-v3-en'], default_model: 'whisper-large-v3-turbo' },
  { id: 'deepseek', label: 'DeepSeek', base_url: 'https://api.deepseek.com/v1', models: ['whisper-1'], default_model: 'whisper-1' },
  { id: 'zhipu', label: '智谱', base_url: 'https://open.bigmodel.cn/api/paas/v4', models: ['whisper-1'], default_model: 'whisper-1' },
  { id: 'kimi', label: 'Kimi', base_url: 'https://api.moonshot.cn/v1', models: ['whisper-1'], default_model: 'whisper-1' },
  { id: 'qwen', label: '阿里云', base_url: 'https://dashscope.aliyuncs.com/compatible-mode/v1', models: ['qwen2.5-omni', 'paraformer-realtime-v2'], default_model: 'qwen2.5-omni' },
  { id: 'custom', label: '自定义', base_url: '', models: [], default_model: '' },
]
const STT_CUSTOM_ID = 'custom'
const sttPresetOf = (id: string) => STT_PRESETS.find((p) => p.id === id)
// 归一化 base_url 便于与预设比对（去首尾空白 + 末尾斜杠）
const normBaseUrl = (u: string) => u.trim().replace(/\/+$/, '')

// —— 通用设置：主题（深色/亮色/随系统）+ 会话页样式（现状/V3）+ 语言（zh/en）——
function GeneralTab() {
  const t = useT()
  const settings = useAppStore((s) => s.settings)
  const setLang = useAppStore((s) => s.setLang)
  const setSandboxMode = useAppStore((s) => s.setSandboxMode)
  const setCronEnabled = useAppStore((s) => s.setCronEnabled)
  const setStopBackgroundOnInterrupt = useAppStore((s) => s.setStopBackgroundOnInterrupt)
  const setCronAutoClean = useAppStore((s) => s.setCronAutoClean)
  const externalSkillsStatus = useAppStore((s) => s.externalSkillsStatus)
  const externalSkillsLoading = useAppStore((s) => s.externalSkillsLoading)
  const externalSkillsImporting = useAppStore((s) => s.externalSkillsImporting)
  const externalSkillsError = useAppStore((s) => s.externalSkillsError)
  const externalSkillsImported = useAppStore((s) => s.externalSkillsImported)
  const refreshExternalSkillsStatus = useAppStore((s) => s.refreshExternalSkillsStatus)
  const importExternalSkills = useAppStore((s) => s.importExternalSkills)
  useEffect(() => {
    void refreshExternalSkillsStatus()
  }, [refreshExternalSkillsStatus])
  const sandboxOptions: { value: 'none' | 'seatbelt'; label: string }[] = [
    { value: 'none', label: t('settings.sandbox.none') },
    { value: 'seatbelt', label: t('settings.sandbox.seatbelt') },
  ]
  const langOptions: { value: 'zh' | 'en'; label: string }[] = [
    { value: 'zh', label: t('settings.lang.zh') },
    { value: 'en', label: t('settings.lang.en') },
  ]
  return (
    <>
      <div className="settings-sec-title">{t('settings.general')}</div>
      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.lang')}</div>
          <div className="set-desc">{t('settings.lang.desc')}</div>
        </div>
        <div className="seg" role="radiogroup" aria-label={t('settings.lang')}>
          {langOptions.map((o) => (
            <button
              key={o.value}
              className={settings.lang === o.value ? 'cur' : ''}
              aria-pressed={settings.lang === o.value}
              onClick={() => setLang(o.value)}
            >
              {o.label}
            </button>
          ))}
        </div>
      </div>
      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.sandbox')}</div>
          <div className="set-desc">{t('settings.sandbox.desc')}</div>
        </div>
        <div className="seg" role="radiogroup" aria-label={t('settings.sandbox')}>
          {sandboxOptions.map((o) => (
            <button
              key={o.value}
              className={(settings.sandbox?.mode ?? 'seatbelt') === o.value ? 'cur' : ''}
              aria-pressed={(settings.sandbox?.mode ?? 'seatbelt') === o.value}
              onClick={() => setSandboxMode(o.value)}
            >
              {o.label}
            </button>
          ))}
        </div>
      </div>
      <div className="set-row">
        <div><div className="set-label">{t('settings.goalAlign')}</div><div className="set-desc">{t('settings.goalAlign.desc')}</div></div>
        <input className="text-input" type="number" min={0} max={100} value={settings.agent?.reminder_rounds ?? 30} onChange={(e) => useAppStore.getState().setGoalAlignmentRounds(Number(e.target.value))} />
      </div>
      {/* 子 agent 并发槽上限（主池 = agent_spawn，辅池 = subagent_explore）：槽满会让工具调用
          同步阻塞（主 Agent 整批挂住），所以缺省 1000 只是失控护栏 —— 这两项是「调小」用的。 */}
      <div className="set-row">
        <div><div className="set-label">{t('settings.maxSubagents')}</div><div className="set-desc">{t('settings.maxSubagents.desc')}</div></div>
        <input className="text-input" type="number" min={1} max={10000} value={settings.agent?.max_subagents ?? SUBAGENT_CONCURRENCY_DEFAULT} onChange={(e) => useAppStore.getState().setSubagentConcurrency({ max_subagents: Number(e.target.value) })} />
      </div>
      <div className="set-row">
        <div><div className="set-label">{t('settings.maxExploreSubagents')}</div><div className="set-desc">{t('settings.maxExploreSubagents.desc')}</div></div>
        <input className="text-input" type="number" min={1} max={10000} value={settings.agent?.max_explore_subagents ?? SUBAGENT_CONCURRENCY_DEFAULT} onChange={(e) => useAppStore.getState().setSubagentConcurrency({ max_explore_subagents: Number(e.target.value) })} />
      </div>
      {/* 定时任务：开启才在 AgentHarness 注册 cron 工具并触发；关闭=不触发但任务保留 */}
      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.cron')}</div>
          <div className="set-desc">{t('settings.cron.desc')}</div>
        </div>
        <div className="seg" role="radiogroup" aria-label={t('settings.cron')}>
          <button className={(settings.cron?.enabled ?? true) ? 'cur' : ''} aria-pressed={settings.cron?.enabled ?? true} onClick={() => setCronEnabled(true)}>{t('settings.on')}</button>
          <button className={!(settings.cron?.enabled ?? true) ? 'cur' : ''} aria-pressed={!(settings.cron?.enabled ?? true)} onClick={() => setCronEnabled(false)}>{t('settings.off')}</button>
        </div>
      </div>
      {/* 主停止后台任务联动（S3-B，2026-08-30 默认翻转）：默认关 = 点 Composer 停止只停主会话，
          后台任务继续运行（单个任务经 interrupt_task 精确停止）；显式开 = 兼容旧级联语义 */}
      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.stopBg')}</div>
          <div className="set-desc">{t('settings.stopBg.desc')}</div>
        </div>
        <div className="seg" role="radiogroup" aria-label={t('settings.stopBgAria')}>
          <button className={(settings.runtime?.stop_background_on_interrupt ?? false) ? 'cur' : ''} aria-pressed={settings.runtime?.stop_background_on_interrupt ?? false} onClick={() => setStopBackgroundOnInterrupt(true)}>{t('settings.on')}</button>
          <button className={!(settings.runtime?.stop_background_on_interrupt ?? false) ? 'cur' : ''} aria-pressed={!(settings.runtime?.stop_background_on_interrupt ?? false)} onClick={() => setStopBackgroundOnInterrupt(false)}>{t('settings.off')}</button>
        </div>
      </div>
      {/* 外部 Skills：目录检测与导入由 Go bridge 完成，renderer 不直接访问用户目录 */}
      <div className="set-row external-skills-row">
        <div>
          <div className="set-label">{t('settings.skillsImport')}</div>
          <div className="set-desc">{t('settings.skillsImport.desc')}</div>
          <div className="external-skills-sources">
            {externalSkillsLoading && <span className="set-desc">{t('settings.skillsImport.loading')}</span>}
            {!externalSkillsLoading && externalSkillsStatus?.sources.map((source) => {
              const label = source.name === 'codex' ? t('settings.skillsImport.codex') : t('settings.skillsImport.claude')
              return (
                <div className="external-skills-source" key={source.name}>
                  <span className={`external-skills-dot ${source.root_exists ? 'on' : ''}`} />
                  <span className="external-skills-source-name">{label}</span>
                  <span className="external-skills-source-path" title={source.root}>{source.root}</span>
                  <span className={`external-skills-source-state ${source.root_exists ? 'ok' : ''}`}>
                    {source.root_exists ? `${t('settings.skillsImport.available')} · ${source.skill_count}` : t('settings.skillsImport.missing')}
                  </span>
                </div>
              )
            })}
          </div>
        </div>
        <button
          className="btn primary"
          disabled={externalSkillsLoading || externalSkillsImporting || !externalSkillsStatus?.enabled}
          onClick={() => void importExternalSkills()}
        >
          {externalSkillsImporting ? t('settings.skillsImport.importing') : t('settings.skillsImport.import')}
        </button>
      </div>
      {externalSkillsError && <div className="external-skills-feedback error">{t('settings.skillsImport.error').replace('{error}', externalSkillsError)}</div>}
      {!externalSkillsError && externalSkillsStatus && !externalSkillsStatus.enabled && <div className="external-skills-feedback">{t('settings.skillsImport.unavailable')}</div>}
      {!externalSkillsError && externalSkillsImported != null && (
        <div className="external-skills-feedback ok">
          {externalSkillsImported > 0 ? t('settings.skillsImport.done').replace('{n}', String(externalSkillsImported)) : t('settings.skillsImport.none')}
        </div>
      )}
      {/* 语音输入（STT）：整段折进可折叠卡片（默认收起）——OpenAI 兼容 /audio/transcriptions 端点 */}
      <STTSection />
    </>
  )
}

// —— 语音输入（STT）：整段收进可折叠卡片（默认收起；展开态记 localStorage）——
// 服务商切换自动带出该服务商的 API 地址（用户改过则优先用户值，按服务商分别记忆）+ 默认模型；
// 「自定义」服务商从空地址开始手填。模型列表 = 预设模型 + 用户自定义（按服务商分组，可增可删）。
// 服务商 / 地址 / 模型改动立即落盘（乐观更新 + settingsSet）；API Key 走「保存」（保存即启用）。
const STT_OPEN_KEY = 'go-code.stt.settingsOpen'
const loadSttOpen = (): boolean => {
  try { return localStorage.getItem(STT_OPEN_KEY) === '1' } catch { return false }
}
const saveSttOpen = (open: boolean) => {
  try { localStorage.setItem(STT_OPEN_KEY, open ? '1' : '0') } catch { /* 隐私模式/配额：忽略 */ }
}

function STTSection() {
  const t = useT()
  const settings = useAppStore((s) => s.settings)
  const setSTTSettings = useAppStore((s) => s.setSTTSettings)
  const stt = settings.stt ?? {}
  const baseURLs = stt.base_urls ?? {}
  const customModels = stt.custom_models ?? {}
  // 当前服务商：显式记录（stt.vendor）优先；旧配置无 vendor → 按 base_url 反查预设，
  // 查不到且没配过地址 = 全新安装 → 默认 OpenAI（与旧版行为一致：默认给 OpenAI 预设）；
  // 查不到但有地址 = 用户手填的地址 → 自定义。
  const explicitVendor = stt.vendor && sttPresetOf(stt.vendor) ? stt.vendor : undefined
  const matchedVendor = STT_PRESETS.find((p) => p.base_url && p.base_url === normBaseUrl(stt.base_url ?? ''))?.id
  const vendorId = explicitVendor ?? matchedVendor ?? (stt.base_url?.trim() ? STT_CUSTOM_ID : STT_PRESETS[0].id)
  const vendor = sttPresetOf(vendorId) ?? STT_PRESETS[STT_PRESETS.length - 1]
  const myModels = customModels[vendorId] ?? []
  // 可选模型 = 预设（自定义服务商无预设）+ 用户在该服务商下添加的
  const modelOptions = [...vendor.models, ...myModels.filter((m) => !vendor.models.includes(m))]
  const baseUrl = stt.base_url ?? ''
  const model = stt.model ?? ''
  const [open, setOpen] = useState(loadSttOpen)
  // 地址/key 走草稿（提交时机 = 失焦 / Enter / 保存 / 切服务商），避免每次按键写一次 settings.json。
  // 全新安装（地址与模型都空）时地址草稿预填服务商默认地址，打开卡片即可直接填 key 点保存。
  const firstConfig = !baseUrl.trim() && !model.trim()
  const [urlDraft, setUrlDraft] = useState(firstConfig ? vendor.base_url : baseUrl)
  // 显示/保存用的模型：已选则用它，全新安装则展示服务商默认（保存时一并写入）
  const modelShown = model || (firstConfig ? vendor.default_model : '')
  const [keyDraft, setKeyDraft] = useState(stt.api_key ?? '')
  const [showKey, setShowKey] = useState(false)
  const [err, setErr] = useState('')
  const [vendorOpen, setVendorOpen] = useState(false)
  const [modelOpen, setModelOpen] = useState(false)
  const [addingModel, setAddingModel] = useState(false)
  const [newModel, setNewModel] = useState('')
  const vendorRef = useRef<HTMLDivElement>(null)
  const modelRef = useRef<HTMLDivElement>(null)
  // 弹层方向：STT 卡在设置页靠下位置，下方空间不足时向上弹——否则弹层超出
  // .settings-body（overflow-y:auto 的滚动容器）被裁掉/顶出视口。
  const [popupUp, setPopupUp] = useState(false)
  const openPopup = (which: 'vendor' | 'model') => {
    const el = which === 'vendor' ? vendorRef.current : modelRef.current
    const body = el?.closest('.settings-body')
    const room = el && body ? body.getBoundingClientRect().bottom - el.getBoundingClientRect().bottom : Infinity
    setPopupUp(room < 300) // 弹层最大 280px + 余量
    setVendorOpen(which === 'vendor' ? !vendorOpen : false)
    setModelOpen(which === 'model' ? !modelOpen : false)
  }
  // settings 外部变化（首次水合 / 切服务商 / 保存）→ 回填草稿；key 只补空缺，不覆盖用户已输入。
  // 全新安装（settings 里地址为空）时回填服务商默认地址，避免水合把预填的草稿清成空。
  useEffect(() => {
    setUrlDraft(baseUrl.trim() ? baseUrl : vendor.base_url)
    setKeyDraft((d) => d || (stt.api_key ?? ''))
    // vendor.base_url 随服务商变：切服务商时 pickVendor 已显式 setUrlDraft，此处只兜水合/外部改动
  }, [baseUrl, stt.api_key, vendor.base_url])
  // 下拉：点击外部 / Esc 关闭（两弹层共用一套监听）
  useEffect(() => {
    if (!vendorOpen && !modelOpen) return
    const close = (e: MouseEvent) => {
      if (vendorRef.current?.contains(e.target as Node)) return
      if (modelRef.current?.contains(e.target as Node)) return
      setVendorOpen(false)
      setModelOpen(false)
    }
    const esc = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setVendorOpen(false)
        setModelOpen(false)
        setAddingModel(false)
      }
    }
    document.addEventListener('mousedown', close)
    document.addEventListener('keydown', esc)
    return () => {
      document.removeEventListener('mousedown', close)
      document.removeEventListener('keydown', esc)
    }
  }, [vendorOpen, modelOpen])

  const toggleOpen = () => setOpen((v) => { saveSttOpen(!v); return !v })
  const ready = Boolean(urlDraft.trim() && modelShown.trim() && (keyDraft.trim() || stt.api_key?.trim()))
  // 提交地址草稿（失焦 / Enter / 保存 / 切服务商时）：按服务商分别记忆，切走再切回仍是用户的值
  const commitUrl = () => {
    const v = urlDraft.trim()
    if (v === baseUrl) return
    setSTTSettings({ base_url: v, base_urls: { ...baseURLs, [vendorId]: v } })
  }
  // 保存：写下地址/模型（含服务商记忆）+ key，并启用（Composer 话筒据此显隐）
  const save = () => {
    const apiKey = keyDraft.trim() || (stt.api_key ?? '').trim()
    if (!urlDraft.trim() || !modelShown.trim() || !apiKey) {
      setErr(t('settings.stt.needConfig'))
      return
    }
    setErr('')
    setSTTSettings({
      base_url: urlDraft.trim(),
      model: modelShown.trim(),
      api_key: apiKey,
      vendor: vendorId,
      base_urls: { ...baseURLs, [vendorId]: urlDraft.trim() },
      enabled: true,
    })
  }
  const setEnabled = (on: boolean) => {
    if (!on) {
      setErr('')
      setSTTSettings({ enabled: false })
      return
    }
    if (!ready) {
      // 配置不全：给出明确提示而不是禁用按钮（用户看到「为什么不能开」）
      setErr(t('settings.stt.needConfig'))
      return
    }
    setErr('')
    setSTTSettings({
      enabled: true,
      ...(keyDraft.trim() ? { api_key: keyDraft.trim() } : {}),
    })
  }
  // 服务商切换：当前服务商的地址先落进记忆表，再带出新服务商的地址（用户改过的优先于预设）+ 模型
  const pickVendor = (id: string) => {
    const next = sttPresetOf(id)
    const nextBase = (baseURLs[id] ?? next?.base_url ?? '').trim()
    const nextPool = [...(next?.models ?? []), ...(customModels[id] ?? [])]
    // 同名模型在新服务商也存在（如 whisper-1）→ 保留当前模型；新服务商有清单 → 用其默认/首个；
    // 新服务商无任何模型（如「自定义」）→ 保留当前模型，避免用户手填的模型名被清空
    const nextModel = !nextPool.length
      ? model
      : (model && nextPool.includes(model) ? model : (next?.default_model || nextPool[0]))
    setVendorOpen(false)
    setErr('')
    setUrlDraft(nextBase)
    setSTTSettings({
      vendor: id,
      base_url: nextBase,
      base_urls: { ...baseURLs, [vendorId]: urlDraft.trim(), [id]: nextBase },
      model: nextModel,
    })
  }
  // 自定义模型：写入当前服务商的列表并直接选中（同服务商内去重；预设已有的名字不重复记）
  const addModel = () => {
    const name = newModel.trim()
    if (!name) return
    const list = myModels.includes(name) || vendor.models.includes(name) ? myModels : [...myModels, name]
    setSTTSettings({ custom_models: { ...customModels, [vendorId]: list }, model: name })
    setNewModel('')
    setAddingModel(false)
    setErr('')
  }
  const removeModel = (name: string) => {
    const list = myModels.filter((m) => m !== name)
    // 被删的正是当前模型 → 回落到该服务商的默认模型（没有则留空，由用户重选）
    const fallback = model === name ? (vendor.default_model || vendor.models[0] || '') : model
    setSTTSettings({ custom_models: { ...customModels, [vendorId]: list }, model: fallback })
  }
  // 折叠态摘要：一眼看出「开没开 + 当前服务商/模型」
  const summary = stt.enabled && ready
    ? `${vendorId === STT_CUSTOM_ID ? t('settings.stt.vendorCustom') : vendor.label} · ${modelShown}`
    : t('settings.stt.summaryOff')
  return (
    <div className={`stt-card ${open ? '' : 'collapsed'}`}>
      <div
        className="stt-head"
        role="button"
        tabIndex={0}
        aria-expanded={open}
        onClick={toggleOpen}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault()
            toggleOpen()
          }
        }}
      >
        <ChevronDown size={13} className="stt-chev" />
        <span className={`stt-dot ${stt.enabled && ready ? 'on' : ''}`} />
        <span className="stt-title">{t('settings.stt')}</span>
        <span className="stt-summary" title={summary}>{summary}</span>
      </div>
      {open && (
        <div className="stt-body">
          <div className="set-row">
            <div>
              <div className="set-label">{t('settings.stt.enabled')}</div>
              <div className="set-desc">{t('settings.stt.enabled.desc')}</div>
            </div>
            <div className="seg" role="radiogroup" aria-label={t('settings.stt.enabled')}>
              <button className={stt.enabled ? 'cur' : ''} aria-pressed={Boolean(stt.enabled)} onClick={() => setEnabled(true)}>
                {t('settings.on')}
              </button>
              <button className={!stt.enabled ? 'cur' : ''} aria-pressed={!stt.enabled} onClick={() => setEnabled(false)}>
                {t('settings.off')}
              </button>
            </div>
          </div>
          <div className="set-row">
            <div>
              <div className="set-label">{t('settings.stt.vendor')}</div>
              <div className="set-desc">{t('settings.stt.vendor.desc')}</div>
            </div>
            {/* 服务商下拉：选中即带出该服务商的 API 地址（用户改过的优先）+ 模型 */}
            <div className="prov-preset" ref={vendorRef}>
              <button
                type="button"
                className={`prov-input prov-select ${vendorOpen ? 'open' : ''}`}
                onClick={() => openPopup('vendor')}
                aria-haspopup="listbox"
                aria-expanded={vendorOpen}
              >
                <span className="prov-select-label">
                  {vendorId === STT_CUSTOM_ID ? t('settings.stt.vendorCustom') : vendor.label}
                </span>
                <ChevronDown size={12} className="prov-select-chev" />
              </button>
              {vendorOpen && (
                <div className={`prov-preset-popup ${popupUp ? 'up' : ''}`} role="listbox">
                  {STT_PRESETS.map((p) => (
                    <button
                      type="button"
                      key={p.id}
                      className={`prov-preset-opt ${vendorId === p.id ? 'cur' : ''}`}
                      onClick={() => pickVendor(p.id)}
                    >
                      <span className="prov-preset-name">{p.label}</span>
                      {p.models.length > 0 && <span className="prov-preset-badge">{p.default_model}</span>}
                    </button>
                  ))}
                </div>
              )}
            </div>
          </div>
          {/* API 地址：随服务商带出、可直接改（失焦/Enter 落盘）；自定义服务商留空手填 */}
          <div className="set-row">
            <div>
              <div className="set-label">{t('settings.stt.baseUrl')}</div>
              <div className="set-desc">{t('settings.stt.baseUrl.desc')}</div>
            </div>
            <input
              className="prov-input"
              value={urlDraft}
              onChange={(e) => { setUrlDraft(e.target.value); setErr('') }}
              onBlur={commitUrl}
              onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); commitUrl() } }}
              placeholder={t('settings.stt.baseUrl.ph')}
              aria-label={t('settings.stt.baseUrl')}
              spellCheck={false}
            />
          </div>
          <div className="set-row">
            <div>
              <div className="set-label">{t('settings.stt.model')}</div>
              <div className="set-desc">{t('settings.stt.model.desc')}</div>
            </div>
            {/* 模型下拉：当前服务商预设 + 自定义；行内可继续手输新模型名添加 */}
            <div className="prov-preset" ref={modelRef}>
              <button
                type="button"
                className={`prov-input prov-select ${modelOpen ? 'open' : ''}`}
                onClick={() => openPopup('model')}
                aria-haspopup="listbox"
                aria-expanded={modelOpen}
              >
                <span className="prov-select-label">{modelShown || t('settings.stt.modelPh')}</span>
                <ChevronDown size={12} className="prov-select-chev" />
              </button>
              {modelOpen && (
                <div className={`prov-preset-popup ${popupUp ? 'up' : ''}`} role="listbox">
                  {modelOptions.length === 0 && !addingModel && (
                    <div className="stt-model-empty">{t('settings.stt.modelEmpty')}</div>
                  )}
                  {modelOptions.map((m) => {
                    const mine = myModels.includes(m)
                    return (
                      <div key={m} className={`prov-preset-opt ${modelShown === m ? 'cur' : ''}`} role="option" aria-selected={modelShown === m}>
                        <button
                          type="button"
                          className="stt-model-pick"
                          onClick={() => { setSTTSettings({ model: m }); setModelOpen(false); setErr('') }}
                        >
                          <span className="prov-preset-name">{m}</span>
                        </button>
                        {/* 仅自定义模型可删（预设模型是内置清单） */}
                        {mine && (
                          <button
                            type="button"
                            className="stt-model-del"
                            title={t('settings.stt.modelRemove')}
                            aria-label={t('settings.stt.modelRemove')}
                            onClick={(e) => { e.stopPropagation(); removeModel(m) }}
                          >
                            ✕
                          </button>
                        )}
                      </div>
                    )
                  })}
                  {/* 添加自定义模型：随服务商记忆（settings.stt.custom_models[vendorId]） */}
                  {addingModel ? (
                    <div className="stt-model-add" role="form">
                      <input
                        className="prov-input mono"
                        placeholder={t('settings.stt.modelAddPh')}
                        value={newModel}
                        onChange={(e) => setNewModel(e.target.value)}
                        onKeyDown={(e) => {
                          if (e.key === 'Enter') { e.preventDefault(); addModel() }
                          if (e.key === 'Escape') { setAddingModel(false); setNewModel('') }
                        }}
                        aria-label={t('settings.stt.modelAdd')}
                        autoFocus
                        spellCheck={false}
                      />
                      <button className="btn primary" onClick={addModel} disabled={!newModel.trim()}>
                        {t('provider.addConfirm')}
                      </button>
                    </div>
                  ) : (
                    <button type="button" className="stt-model-addbtn" onClick={() => { setNewModel(''); setAddingModel(true) }}>
                      + {t('settings.stt.modelAdd')}
                    </button>
                  )}
                </div>
              )}
            </div>
          </div>
          <div className="set-row">
            <div>
              <div className="set-label">{t('settings.stt.apiKey')}</div>
              <div className="set-desc">{t('settings.stt.apiKey.desc')}</div>
            </div>
            <div className="prov-keyrow">
              <div className="prov-keyinput">
                <input
                  className="prov-input"
                  type={showKey ? 'text' : 'password'}
                  placeholder={t('settings.stt.apiKey.ph')}
                  value={keyDraft}
                  onChange={(e) => { setKeyDraft(e.target.value); setErr('') }}
                  autoComplete="off"
                  spellCheck={false}
                />
                <button
                  type="button"
                  className={`key-eye ${showKey ? 'on' : ''}`}
                  onClick={() => setShowKey((v) => !v)}
                  aria-label={showKey ? t('provider.keyHide') : t('provider.keyShow')}
                  title={showKey ? t('provider.keyHide') : t('provider.keyShow')}
                  tabIndex={-1}
                >
                  {showKey ? <EyeOffIcon size={14} /> : <EyeIcon size={14} />}
                </button>
              </div>
              <button className="btn primary" onClick={save}>{t('settings.stt.save')}</button>
            </div>
          </div>
          <div className="set-desc stt-status">
            {err ? (
              <span className="stt-err" role="alert">{err}</span>
            ) : stt.enabled && ready ? (
              t('settings.stt.ready').replace('{model}', modelShown)
            ) : (
              t('settings.stt.notReady')
            )}
          </div>
        </div>
      )}
    </div>
  )
}

// —— 快捷键：只读展示（键 + 动作 + 生效状态；本次 ⌘B/⌘\ 已接线，其余规范项标注未实现）——
function ShortcutsTab() {
  const t = useT()
  const items: { key: string; action: string; done: boolean }[] = [
    { key: '⌘B', action: t('sc.toggleLeft'), done: true },
    { key: '⌘\\', action: t('sc.toggleRight'), done: true },
    { key: 'Enter', action: t('sc.send'), done: true },
    { key: 'Shift+Enter', action: t('sc.newline'), done: true },
    { key: 'Esc', action: t('sc.close'), done: true },
    { key: '⌘K', action: t('sc.palette'), done: false },
    { key: 'Ctrl+O', action: t('sc.focusCycle'), done: false },
  ]
  return (
    <>
      <div className="settings-sec-title">{t('settings.shortcuts')}</div>
      <div className="sc-table" role="table" aria-label={t('settings.shortcuts')}>
        <div className="sc-row sc-head" role="row">
          <span className="sc-key">{t('sc.key')}</span>
          <span className="sc-action">{t('sc.action')}</span>
          <span className="sc-status">{t('sc.status')}</span>
        </div>
        {items.map((it) => (
          <div className="sc-row" role="row" key={it.key}>
            <span className="sc-key"><kbd>{it.key}</kbd></span>
            <span className="sc-action">{it.action}</span>
            <span className={`sc-status ${it.done ? 'ok' : 'no'}`}>{it.done ? t('sc.implemented') : t('sc.pending')}</span>
          </div>
        ))}
      </div>
      <p className="set-desc" style={{ marginTop: 10 }}>{t('sc.note')}</p>
    </>
  )
}

// —— Provider：内置快捷添加（DeepSeek/OpenAI/OpenCode Go/Kimi/智谱 + P1 可选 + P2 订阅）+ 用户自定义 ——
// 协议三选均已实现（chat-completions / responses / anthropic，2026-08）。
type ProtocolId = 'chat_completions' | 'responses' | 'anthropic'
// Provider 显示名：内置品牌名走 i18n（含中文品牌如智谱/阿里 Qwen），其余为固定英文名。
function providerDisplay(t: (k: string) => string, key: string): string {
  const i18nKeys: Record<string, string> = {
    deepseek: 'composer.providerDisplay.deepseek',
    openai: 'composer.providerDisplay.openai',
    opencode: 'composer.providerDisplay.opencode',
    kimi: 'composer.providerDisplay.kimi',
    zhipu: 'composer.providerDisplay.zhipu',
    qwen: 'composer.providerDisplay.qwen',
    xiaomi: 'composer.providerDisplay.xiaomi',
    together: 'composer.providerDisplay.together',
    fireworks: 'composer.providerDisplay.fireworks',
  }
  if (i18nKeys[key]) return t(i18nKeys[key])
  const fixed: Record<string, string> = {
    anthropic: 'Anthropic', groq: 'Groq', xai: 'xAI (Grok)', mistral: 'Mistral', minimax: 'MiniMax',
    cerebras: 'Cerebras', baseten: 'Baseten', nvidia: 'NVIDIA NIM', huggingface: 'Hugging Face', openrouter: 'OpenRouter',
    'openai-codex': 'OpenAI Codex', 'github-copilot': 'GitHub Copilot', 'kimi-coding': 'Kimi Coding',
  }
  return fixed[key] ?? key
}
// 已规划但未实现的厂商（对应 docs/PROVIDER_OPTIMIZATION_PLAN.md P2：google/bedrock/azure/cloudflare）
const PLANNED_PROVIDERS: { id: string; name: string }[] = [
  { id: 'google', name: 'Google Gemini' },
  { id: 'bedrock', name: 'Amazon Bedrock' },
  { id: 'azure', name: 'Azure OpenAI' },
  { id: 'cloudflare', name: 'Cloudflare' },
]
const PROTOCOL_OPTIONS: { value: ProtocolId; label: string }[] = [
  { value: 'chat_completions', label: 'Chat Completions' },
  { value: 'responses', label: 'Responses' },
  { value: 'anthropic', label: 'Anthropic' },
]

// usage 口径三项（2026-09-21）：自动 = 协议默认 + 响应形状嗅探；后两项 = 显式标注覆盖嗅探。
// 只影响 input_tokens 是否与 cache_read/cache_write 相加（见 bridge usage_input_includes_cache）。
const CACHE_TTL_OPTIONS: { value: 'auto' | '5m' | '1h'; label: string; desc: string }[] = [
  { value: 'auto', label: '自动', desc: '官方端点 1h（extended TTL），兼容端点 5m（推荐）' },
  { value: '1h', label: '1h', desc: '长缓存：写入价 2× 基础输入价，跨轮（工具批/后台任务/离开几分钟）不再过期' },
  { value: '5m', label: '5m', desc: '服务端默认：写入价 1.25× 基础输入价，但 5 分钟不活动即过期' },
]

const USAGE_INCL_OPTIONS: { value: 'auto' | 'inclusive' | 'exclusive'; label: string; desc: string }[] = [
  { value: 'auto', label: '自动', desc: '按协议默认 + 响应自报形状判定（推荐）' },
  { value: 'inclusive', label: '已含缓存', desc: 'input_tokens 已含缓存命中（OpenAI 形状网关：input_tokens = prompt_tokens）' },
  { value: 'exclusive', label: '未含缓存', desc: 'input_tokens 只含未命中部分（Anthropic 官方语义：总输入 = input + cache_read + cache_write）' },
]

// —— 预设数据源：bridge list_provider_presets ← provider/providers.json（单一事实源）。
// 以下辅助函数在 ProviderTab 内从 store.providerPresets 派生，不再硬编码预设表。

// presetById 从 presets 列表按 id 查预设；找不到返回 undefined。
function presetById(presets: ProviderPreset[], id: string): ProviderPreset | undefined {
  return presets.find((p) => p.id === id)
}

// presetBaseURL 预设默认 Base URL（providers.json base_url）。
function presetBaseURL(presets: ProviderPreset[], id: string): string {
  return presetById(presets, id)?.base_url ?? ''
}

// presetProtocol 预设默认协议（providers.json protocol）。
function presetProtocol(presets: ProviderPreset[], id: string): ProtocolId {
  const p = presetById(presets, id)
  return (p?.protocol as ProtocolId | undefined) ?? 'chat_completions'
}

// oauthProviderIDs OAuth 订阅预设 id 集（鉴权 = 订阅登录）。
function oauthProviderIDs(presets: ProviderPreset[]): string[] {
  return presets.filter((p) => p.oauth).map((p) => p.id)
}

// isOAuthPreset 预设是否为 OAuth 订阅厂商（添加表单据此隐藏 API Key、提示登录方式）。
function isOAuthPreset(presets: ProviderPreset[], name: string): boolean {
  return oauthProviderIDs(presets).includes(name)
}

// apiKeyProviderIDs API Key 预设 id 集（鉴权 = 填 key）。
function apiKeyProviderIDs(presets: ProviderPreset[]): string[] {
  return presets.filter((p) => !p.oauth).map((p) => p.id)
}

function ProviderTab() {
  const t = useT()
  const settings = useAppStore((s) => s.settings)
  const providerKeys = useAppStore((s) => s.providerKeys)
  const refreshProviderKeys = useAppStore((s) => s.refreshProviderKeys)
  const fetchModels = useAppStore((s) => s.fetchModels)
  const refreshPrices = useAppStore((s) => s.refreshPrices)
  const testProvider = useAppStore((s) => s.testProvider)
  const saveProvider = useAppStore((s) => s.saveProvider)
  const showToast = useAppStore((s) => s.showToast)
  const addProvider = useAppStore((s) => s.addProvider)
  const removeProvider = useAppStore((s) => s.removeProvider)
  const toggleModelWindow = useAppStore((s) => s.toggleModelWindow)
  const setModelWindow = useAppStore((s) => s.setModelWindow)
  const setModelMaxTokens = useAppStore((s) => s.setModelMaxTokens)
  const toggleModelImage = useAppStore((s) => s.toggleModelImage)
  // 模型清单编辑 + 每模型价表（2026-09）
  const addModel = useAppStore((s) => s.addModel)
  const renameModel = useAppStore((s) => s.renameModel)
  const removeModel = useAppStore((s) => s.removeModel)
  const renameProvider = useAppStore((s) => s.renameProvider)
  // OAuth 订阅登录（2026-08 P2）
  const oauthStatus = useAppStore((s) => s.oauthStatus)
  const refreshAllOAuthStatus = useAppStore((s) => s.refreshAllOAuthStatus)
  const oauthLogin = useAppStore((s) => s.oauthLogin)
  const oauthLogout = useAppStore((s) => s.oauthLogout)
  const oauthPromptAnswer = useAppStore((s) => s.oauthPromptAnswer)
  const [oauthLoggingIn, setOAuthLoggingIn] = useState<string | null>(null) // 正在登录的 provider
  const [oauthEvents, setOAuthEvents] = useState<Record<string, { type: string; url?: string; instructions?: string; user_code?: string; verification_uri?: string; message?: string }>>({}) // 登录事件
  const [oauthManual, setOAuthManual] = useState<string>('') // manual 输入草稿
  const [oauthRequestId, setOAuthRequestId] = useState<Record<string, string>>({}) // provider → 最近 prompt 的 request_id
  // 监听 oauth_event 推送：登录中显示进度；完成/失败后刷新状态
  useEffect(() => {
    refreshAllOAuthStatus()
    const off = getTransport().onEvent((line) => {
      try {
        const ev = JSON.parse(line) as {
          event_type?: string; provider?: string; request_id?: string
          event?: { type?: string; url?: string; user_code?: string; verification_uri?: string; message?: string; instructions?: string }
        }
        if (ev.event_type === 'oauth_event' && ev.provider) {
          setOAuthLoggingIn(ev.provider) // 事件到达 = 登录进行中
          if (ev.event) setOAuthEvents((m) => ({ ...m, [ev.provider!]: ev.event as never }))
        }
        if (ev.event_type === 'oauth_prompt_request' && ev.provider) {
          setOAuthLoggingIn(ev.provider)
          setOAuthManual('')
          if (ev.request_id) setOAuthRequestId((m) => ({ ...m, [ev.provider!]: ev.request_id! }))
          // 无事件时也显示 manual 输入框（占位 type=manual）
          setOAuthEvents((m) => (m[ev.provider!] ? m : { ...m, [ev.provider!]: { type: 'manual' } as never }))
        }
      } catch { /* 非 JSON 事件忽略 */ }
    })
    return off
  }, [refreshAllOAuthStatus])
  // 提交 manual 授权码/重定向 URL → 回填 bridge（带 request_id 精确路由）
  const submitOAuthManual = (pid: string) => {
    const v = oauthManual.trim()
    if (!v) return
    void oauthPromptAnswer(pid, v, oauthRequestId[pid])
    setOAuthManual('')
  }
  // 发起 OAuth 登录：等待 bridge 流程结束（成功/失败/超时）后复位登录中状态
  const startOAuthLogin = async (pid: string) => {
    setOAuthLoggingIn(pid)
    setOAuthEvents((m) => { const n = { ...m }; delete n[pid]; return n }) // 清旧事件
    const r = await oauthLogin(pid)
    setOAuthLoggingIn((cur) => (cur === pid ? null : cur))
    if (r && !r.ok && r.error) setErr(r.error)
  }
  const [busy, setBusy] = useState<string | null>(null) // 正在拉模型的 provider
  const [keys, setKeys] = useState<Record<string, string>>({}) // key 输入框草稿（打开时预填已存 key，掩码显示）
  const [showKey, setShowKey] = useState<Record<string, boolean>>({}) // 每 provider key 明文/掩码切换（默认掩码）
  const [baseURLs, setBaseURLs] = useState<Record<string, string>>({}) // baseURL 草稿
  const [protocols, setProtocols] = useState<Record<string, ProtocolId>>({}) // 协议草稿
  // usage 口径草稿（'auto' = 清除标注回自动判定；inclusive = input_tokens 已含缓存；
  // exclusive = 未含，按 Anthropic 语义相加）。见 provider.Registry.UsageInputIncludesCache。
  const [usageIncls, setUsageIncls] = useState<Record<string, 'auto' | 'inclusive' | 'exclusive'>>({})
  // 缓存 TTL 草稿（'auto' = 清除标注回端点默认：官方 1h / 兼容 5m）
  const [cacheTTLs, setCacheTTLs] = useState<Record<string, 'auto' | '5m' | '1h'>>({})
  const [err, setErr] = useState<string | null>(null)
  // 每张 Provider 卡可折叠（默认 active 展开、其余折叠——模型厂商模型多时省空间）
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  const [adding, setAdding] = useState(false) // 添加自定义 provider 表单展开
  const [newName, setNewName] = useState('')
  const [newBaseURL, setNewBaseURL] = useState('')
  const [newKey, setNewKey] = useState('')
  const [showNewKey, setShowNewKey] = useState(false) // 添加表单 key 明文/掩码（默认掩码）
  const [newProtocol, setNewProtocol] = useState<ProtocolId>('chat_completions')
  const [preset, setPreset] = useState<string>('') // 添加表单当前预设（'' = 自定义）
  const [presetOpen, setPresetOpen] = useState(false) // 自定义预设下拉弹层
  const presetRef = useRef<HTMLDivElement>(null) // 预设下拉容器（外部点击/Esc 关闭）
  const [testingNew, setTestingNew] = useState(false) // 添加表单「测试连接」进行中
  const [testNew, setTestNew] = useState<{ ok: boolean; msg: string } | null>(null) // 添加表单测试结果
  const [testing, setTesting] = useState<Record<string, boolean>>({}) // 每卡「测试连接」进行中
  const [tests, setTests] = useState<Record<string, { ok: boolean; msg: string }>>({}) // 每卡测试结果
  // 模型清单编辑（2026-09）：重命名行（pid|oldName → 输入框草稿）、删除确认态、手动添加行草稿
  const [renamingModel, setRenamingModel] = useState<Record<string, { provider: string; old: string }>>({}) // key = provider\0model → 正在改名
  const [modelDrafts, setModelDrafts] = useState<Record<string, string>>({}) // 改名输入草稿（同 key）
  const [addingModel, setAddingModel] = useState<string | null>(null) // 正在手动加模型的 provider（表单展开）
  const [addModelName, setAddModelName] = useState<Record<string, string>>({}) // 新增模型名草稿（per provider）
  const [priceEditOpen, setPriceEditOpen] = useState<Record<string, boolean>>({}) // key = pid\0model → 价格编辑展开
  const [renamingProvider, setRenamingProvider] = useState<string | null>(null) // 正在改名的自定义 provider
  const [providerDraft, setProviderDraft] = useState('') // provider 改名草稿
  const isOpen = (pid: string) => expanded[pid] ?? settings.provider.active === pid
  const toggle = (pid: string) => setExpanded((m) => ({ ...m, [pid]: !isOpen(pid) }))
  // 全部 provider id（active 是保留键）：内置三卡 + 用户自定义
  // 所有 provider 卡（active 除外）；已删除标记（deleted:true，内置 5 家删除后）不显示。
  const providerIds = Object.keys(settings.provider).filter((k) => k !== 'active' && !((settings.provider[k] as ProviderCfg | undefined)?.deleted))
  // 所有 provider 均可删除（含内置 5 家；内置删除 = 标记 deleted 隐藏，bridge 重启不复活）
  const isBuiltin = (pid: string) => ['deepseek', 'openai', 'opencode', 'kimi', 'zhipu'].includes(pid)
  const cfgOf = (pid: string): ProviderCfg | undefined => settings.provider[pid] as ProviderCfg | undefined

  // —— 预设数据源（providers.json 单一事实源；bridge list_provider_presets 下发）——
  const providerPresets = useAppStore((s) => s.providerPresets)
  const modelCosts = useAppStore((s) => s.modelCosts)
  const fetchProviderPresets = useAppStore((s) => s.fetchProviderPresets)
  const oauthSpecs: Record<string, { name?: string; subscription?: boolean; login_label?: string; key_instead?: boolean }> = {}
  for (const p of providerPresets) {
    if (p.oauth) {
      oauthSpecs[p.id] = {
        name: p.oauth.name,
        subscription: p.oauth.is_subscription,
        login_label: p.oauth.login_label,
        key_instead: p.oauth.key_instead,
      }
    }
  }
  const oauthProviderIds = oauthProviderIDs(providerPresets)
  const apiKeyProviderIds = apiKeyProviderIDs(providerPresets)
  // 打开设置时拉取预设 + 预设模型内置价（首次或 bridge 重启后兜底刷新）。
  // 注意：不能只看 providerPresets 判空 —— 重启后 store 全新（两者皆空），但若只缺
  // modelCosts（旧 store 数据/部分响应），也要补拉一次才能回显内置价。
  useEffect(() => {
    if (providerPresets.length === 0 || Object.keys(modelCosts).length === 0) fetchProviderPresets()
  }, [providerPresets.length, modelCosts, fetchProviderPresets])

  // 某（provider, model）的 max_tokens 有效值：设置里显式配置优先 → 已知默认 → 兜底 8192
  const maxTokensOf = (pid: string, name: string): number => {
    const stored = cfgOf(pid)?.max_tokens?.[name]
    if (typeof stored === 'number') return stored
    const known = KNOWN_MODEL_MAX_TOKENS[pid]?.[name]
    if (typeof known === 'number') return known
    return MODEL_MAX_TOKENS_DEFAULT
  }

  // 打开时刷新 key 状态；baseURL/protocol 草稿与 settings 同步（保存/加载/增删后回填）
  useEffect(() => {
    refreshProviderKeys()
  }, [refreshProviderKeys])
  // 预设下拉：外部点击 / Esc 关闭
  useEffect(() => {
    if (!presetOpen) return
    const close = (e: MouseEvent) => {
      if (presetRef.current && !presetRef.current.contains(e.target as Node)) setPresetOpen(false)
    }
    const esc = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setPresetOpen(false)
    }
    document.addEventListener('mousedown', close)
    document.addEventListener('keydown', esc)
    return () => {
      document.removeEventListener('mousedown', close)
      document.removeEventListener('keydown', esc)
    }
  }, [presetOpen])
  useEffect(() => {
    const b: Record<string, string> = {}
    const p: Record<string, ProtocolId> = {}
    const u: Record<string, 'auto' | 'inclusive' | 'exclusive'> = {}
    const t: Record<string, 'auto' | '5m' | '1h'> = {}
    for (const pid of providerIds) {
      b[pid] = cfgOf(pid)?.base_url ?? ''
      p[pid] = (cfgOf(pid)?.protocol as ProtocolId | undefined) ?? 'chat_completions'
      const ui = cfgOf(pid)?.usage_input_includes_cache
      u[pid] = ui === true ? 'inclusive' : ui === false ? 'exclusive' : 'auto'
      const ttl = cfgOf(pid)?.cache_ttl_1h
      t[pid] = ttl === true ? '1h' : ttl === false ? '5m' : 'auto'
    }
    setBaseURLs(b)
    setProtocols(p)
    setUsageIncls(u)
    setCacheTTLs(t)
    // key 草稿预填已存明文（默认掩码，眼睛切换明文）；只填空缺草稿，不覆盖用户已输入
    setKeys((m) => {
      let changed = false
      const next = { ...m }
      for (const pid of providerIds) {
        const stored = cfgOf(pid)?.api_key ?? ''
        if (!next[pid] && stored) {
          next[pid] = stored
          changed = true
        }
      }
      return changed ? next : m
    })
  }, [settings]) // 依赖整个 settings：增删 provider 也触发回填

  const save = async (pid: string) => {
    const api_key = (keys[pid] ?? '').trim() || undefined
    // usage 口径：'auto' 显式发 null（清除标注回自动），其余发 bool（显式标注）
    const uiDraft = usageIncls[pid] ?? 'auto'
    const ttlDraft = cacheTTLs[pid] ?? 'auto'
    const ok = await saveProvider({
      provider: pid,
      base_url: baseURLs[pid] ?? cfgOf(pid)?.base_url ?? '',
      protocol: protocols[pid] ?? 'chat_completions',
      openai_compat: cfgOf(pid)?.openai_compat,
      usage_input_includes_cache: uiDraft === 'auto' ? null : uiDraft === 'inclusive',
      cache_ttl_1h: ttlDraft === 'auto' ? null : ttlDraft === '1h',
      api_key,
    })
    // 不清空草稿：key 已预填明文（掩码），眼睛可切换查看；保存后 settings 刷新回填
    setErr(null)
    if (ok) {
      showToast(t('provider.saved')) // 成功 → 全局 Toast（符合系统样式）
    } else {
      showToast(t('provider.saveErr'), 'error')
    }
  }
  const toggleCompatMinimal = (pid: string, minimal: boolean) => {
    const pc = cfgOf(pid)
    if (!pc) return
    const openai_compat = minimal ? { ...(pc.openai_compat ?? {}), minimal: true, base_path: pc.openai_compat?.base_path || '/v1' } : { ...(pc.openai_compat ?? {}), minimal: false }
    saveProvider({ provider: pid, base_url: baseURLs[pid] ?? pc.base_url, protocol: protocols[pid] ?? 'chat_completions', openai_compat })
  }

  const pull = (pid: string) => {
    if (!providerKeys[pid]) {
      setErr(t('provider.errKeyFirst'))
      return
    }
    setErr(null)
    setBusy(pid)
    fetchModels(pid)
    // busy 结束信号：fetchedModels 变化后复位
    window.setTimeout(() => setBusy((b) => (b === pid ? null : b)), 6000)
  }
  // 从价源补价（refresh_prices → models.dev/openrouter）：桥只改内存，**落盘由 store 经
  // providerSave 完成**（否则重启即丢）。保守语义：只补「无价」的模型，不动手填价与内置价。
  const backfillPrices = async (pid: string) => {
    setErr(null)
    setBusy(pid)
    const r = await refreshPrices({ provider: pid })
    setBusy((b) => (b === pid ? null : b))
    if (!r) {
      showToast(t('provider.priceBackfillErr'), 'error')
      return
    }
    showToast(t('provider.priceBackfillDone')
      .replace('{n}', String(r.filledCount))
      .replace('{skip}', String(r.skippedCount))
      .replace('{src}', r.source || '—'))
  }
  const startAdd = () => {
    setNewName(''); setNewBaseURL(''); setNewKey(''); setNewProtocol('chat_completions')
    setPreset(''); setTestNew(null)
    setAdding(true); setErr(null)
  }
  const submitAdd = async () => {
    const name = newName.trim()
    if (!name) return setErr(t('provider.addErrName'))
    if (name === 'active' || !/^[A-Za-z][A-Za-z0-9_-]*$/.test(name)) return setErr(t('provider.addErrNameInvalid'))
    // 同名已存在且未被删除 → 拒绝；已删除（deleted:true，内置 provider 删除是标记非物理删除）
    // → 允许重新添加（重新保存即清除 deleted 标记，等同重新启用）。
    const existing = settings.provider[name] as ProviderCfg | undefined
    if (Object.prototype.hasOwnProperty.call(settings.provider, name) && !(existing && typeof existing === 'object' && existing.deleted)) {
      return setErr(t('provider.addErrExists'))
    }
    if (!newBaseURL.trim()) return setErr(t('provider.addErrURL'))
    const r = await addProvider({ provider: name, base_url: newBaseURL.trim(), protocol: newProtocol, openai_compat: { minimal: true, base_path: '/v1' }, api_key: newKey.trim() || undefined })
    if (!r.ok) return setErr(r.error ?? t('provider.addErr'))
    setAdding(false)
    setErr(null) // 清掉预设冲突等残留提示
    setExpanded((m) => ({ ...m, [name]: true })) // 新增后展开该卡
    showToast(t('provider.saved'))
  }
  const del = async (pid: string) => {
    await removeProvider(pid)
    setErr(null)
  }

  // 预设选择：自动填名称 + 对应默认 Base URL（模型列表留空，添加后用「拉取模型列表」导入）。
  // 同名已存在（如内置 deepseek 卡）→ 提示改名（与 submitAdd 同名校验口径一致）。
  const onPreset = (name: string) => {
    setPreset(name)
    setTestNew(null)
    if (!name) {
      setNewName('')
      setNewBaseURL('')
      setErr(null)
      return
    }
    setNewName(name)
    setNewBaseURL(presetBaseURL(providerPresets, name))
    setNewProtocol(presetProtocol(providerPresets, name)) // 预设协议（providers.json；minimax/fireworks→anthropic，xai→responses）
    // 同名已存在（如内置 deepseek 卡）→ 提示改名；但已删除（deleted:true）允许重新添加。
    const existing = settings.provider[name] as ProviderCfg | undefined
    if (Object.prototype.hasOwnProperty.call(settings.provider, name) && !(existing && typeof existing === 'object' && existing.deleted)) setErr(t('provider.addErrExists'))
    else setErr(null)
  }

  // 添加表单「测试连接」：瞬态发 test_connection（待保存的 base_url + key，不落盘不改配置）。
  const runNewTest = async () => {
    if (!newKey.trim()) {
      setErr(t('provider.testErrKey'))
      return
    }
    setErr(null)
    setTestingNew(true)
    setTestNew(null)
    const r = await testProvider(newBaseURL.trim(), newKey.trim())
    setTestingNew(false)
    setTestNew({ ok: r.ok, msg: r.ok ? t('provider.testOk').replace('{n}', String(r.count)) : (r.error ?? t('provider.testFailed')) })
  }

  // 卡片「测试连接」：用卡片草稿 baseURL + key（key 草稿空则回落已存 key）探活。
  const runCardTest = async (pid: string) => {
    const pc = cfgOf(pid)
    const key = (keys[pid] ?? '').trim() || pc?.api_key || ''
    if (!key) {
      setErr(t('provider.testErrKey'))
      return
    }
    setErr(null)
    setTesting((m) => ({ ...m, [pid]: true }))
    setTests((m) => ({ ...m, [pid]: { ok: false, msg: '' } })) // 清旧结果
    const r = await testProvider(baseURLs[pid] ?? pc?.base_url ?? '', key)
    setTesting((m) => ({ ...m, [pid]: false }))
    setTests((m) => ({ ...m, [pid]: { ok: r.ok, msg: r.ok ? t('provider.testOk').replace('{n}', String(r.count)) : (r.error ?? t('provider.testFailed')) } }))
  }

  // —— 模型清单编辑（2026-09）：手动添加 / 改名 / 删除 ——
  const modelKey = (pid: string, model: string) => `${pid}\u0000${model}`
  const submitAddModel = async (pid: string) => {
    const name = (addModelName[pid] ?? '').trim()
    if (!name) return
    const r = await addModel(pid, name)
    if (!r.ok) {
      setErr(r.error ?? t('provider.modelAddErr'))
      return
    }
    setAddModelName((m) => ({ ...m, [pid]: '' }))
    setAddingModel(null)
    setExpanded((m) => ({ ...m, [pid]: true }))
    setErr(null)
  }
  const submitRenameModel = async (pid: string, old: string) => {
    const key = modelKey(pid, old)
    const name = (modelDrafts[key] ?? old).trim()
    if (name === old) {
      setRenamingModel((m) => { const n = { ...m }; delete n[key]; return n })
      return
    }
    const r = await renameModel(pid, old, name)
    if (!r.ok) {
      setErr(r.error ?? t('provider.modelRenameErr'))
      return
    }
    setRenamingModel((m) => { const n = { ...m }; delete n[key]; return n })
    setModelDrafts((m) => { const n = { ...m }; delete n[key]; return n })
    setErr(null)
  }
  const submitRenameProvider = async () => {
    if (!renamingProvider) return
    const r = await renameProvider(renamingProvider, providerDraft)
    if (!r.ok) {
      setErr(r.error ?? t('provider.renameErr'))
      return
    }
    // 改名后 UI 状态跟随新 id
    const next = providerDraft.trim()
    if (next && next !== renamingProvider) {
      setBaseURLs((m) => ({ ...m, [next]: m[renamingProvider] ?? '' }))
      setKeys((m) => ({ ...m, [next]: m[renamingProvider] ?? '' }))
      setProtocols((m) => ({ ...m, [next]: m[renamingProvider] ?? 'chat_completions' }))
      setExpanded((m) => ({ ...m, [next]: true }))
    }
    setRenamingProvider(null)
    setProviderDraft('')
    setErr(null)
    showToast(t('provider.saved'))
  }

  // 协议选择器（卡片 + 添加表单共用）：三协议均已实现（chat-completions / responses / anthropic）
  const renderProtocol = (pid: string, current: ProtocolId, onChange: (v: ProtocolId) => void) => (
    <div className="seg" role="radiogroup" aria-label={t('provider.protocol')}>
      {PROTOCOL_OPTIONS.map((opt) => (
        <button
          key={opt.value}
          className={current === opt.value ? 'cur' : ''}
          aria-pressed={current === opt.value}
          onClick={() => onChange(opt.value)}
        >
          {opt.label}
        </button>
      ))}
    </div>
  )

  return (
    <>
      <div className="settings-sec-title">{t('settings.provider')}</div>
      <p className="set-desc" style={{ marginBottom: 6 }}>{t('provider.windowHint')}</p>
      {err && <p className="nwf-err" role="alert">{err}</p>}
      {/* 添加自定义 Provider（内置三卡是快捷添加预设；自定义 = 用户自己填 baseURL/key/协议） */}
      <div className="prov-addbar">
        {!adding ? (
          <button className="btn primary" onClick={startAdd}>{t('provider.add')}</button>
        ) : (
          <div className="prov-card">
            <div className="prov-add-hd">
              <span className="prov-name">{t('provider.addTitle')}</span>
              <button className="btn" onClick={() => setAdding(false)}>{t('provider.cancel')}</button>
            </div>
            <div className="prov-body">
              {/* 快捷预设：自定义下拉（内置厂商分组 + 自定义）；选中自动填名称 + 默认 Base URL + 协议。
                  不用原生 <select>：macOS 弹层是系统渲染（白底系统菜单），CSS 无法主题化；
                  自绘弹层与应用 popup/seg 同风格，闭合态与 prov-input 同体系。 */}
              <div className="set-row">
                <div className="set-label">{t('provider.preset')}</div>
                <div className="prov-preset" ref={presetRef}>
                  <button
                    type="button"
                    className={`prov-input prov-select ${presetOpen ? 'open' : ''}`}
                    onClick={() => setPresetOpen((v) => !v)}
                    aria-haspopup="listbox"
                    aria-expanded={presetOpen}
                  >
                    <span className="prov-select-label">{preset ? (providerDisplay(t, preset) ?? preset) : t('provider.presetCustom')}</span>
                    <ChevronDown size={12} className="prov-select-chev" />
                  </button>
                  {presetOpen && (
                    <div className="prov-preset-popup" role="listbox">
                      <button
                        type="button"
                        className={`prov-preset-opt ${preset === '' ? 'cur' : ''}`}
                        onClick={() => { onPreset(''); setPresetOpen(false) }}
                      >
                        {t('provider.presetCustom')}
                      </button>
                      {/* API Key 提供商（鉴权 = 填 key） */}
                      <div className="prov-preset-grp">{t('provider.presetApiKey')}</div>
                      {apiKeyProviderIds.map((n) => (
                        <button
                          type="button"
                          key={n}
                          className={`prov-preset-opt ${preset === n ? 'cur' : ''}`}
                          onClick={() => { onPreset(n); setPresetOpen(false) }}
                        >
                          <span className="prov-preset-name">{providerDisplay(t, n)}</span>
                          <span className="prov-preset-badge">{t('provider.badgeKey')}</span>
                        </button>
                      ))}
                      {/* OAuth 订阅提供商（鉴权 = 订阅登录） */}
                      <div className="prov-preset-grp">{t('provider.presetOAuth')}</div>
                      {oauthProviderIds.map((n) => (
                        <button
                          type="button"
                          key={n}
                          className={`prov-preset-opt ${preset === n ? 'cur' : ''}`}
                          onClick={() => { onPreset(n); setPresetOpen(false) }}
                        >
                          <span className="prov-preset-name">{providerDisplay(t, n)}</span>
                          <span className="prov-preset-badge prov-preset-badge-oauth">{t('provider.badgeOAuth')}</span>
                        </button>
                      ))}
                      {/* 规划中（原生协议未实现，置灰不可选） */}
                      <div className="prov-preset-grp">{t('provider.presetPlanned')}</div>
                      {PLANNED_PROVIDERS.map((p) => (
                        <button type="button" key={p.id} className="prov-preset-opt prov-preset-opt-disabled" disabled onClick={(e) => e.preventDefault()}>
                          <span className="prov-preset-name">{p.name}</span>
                          <span className="prov-preset-badge">{t('provider.badgePlanned')}</span>
                        </button>
                      ))}
                    </div>
                  )}
                </div>
              </div>
              <div className="set-row">
                <div className="set-label">{t('provider.addName')}</div>
                <input className="prov-input" value={newName} onChange={(e) => { setNewName(e.target.value); setErr(null) }} aria-label="provider name" placeholder="my-provider" spellCheck={false} />
              </div>
              <div className="set-row">
                <div className="set-label">{t('provider.baseURL')}</div>
                <input className="prov-input" value={newBaseURL} onChange={(e) => { setNewBaseURL(e.target.value); setErr(null) }} aria-label="provider base url" placeholder="https://api.example.com/v1" spellCheck={false} />
              </div>
              <div className="set-row">
                <div className="set-label">{t('provider.protocol')}</div>
                {renderProtocol('__new__', newProtocol, setNewProtocol)}
              </div>
              {/* OAuth 厂商：不需要 API Key —— 隐藏 key 输入与「测试连接」（key 为空测不了），
                  提示添加后展开卡片登录；「添加」按钮保留在独立行 */}
              {isOAuthPreset(providerPresets, preset) ? (
                <div className="set-row">
                  <div className="set-label" />
                  <div className="prov-oauth-hint" role="status">{t('provider.addOAuthHint')}</div>
                </div>
              ) : (
                <div className="set-row">
                  <div className="set-label">{t('provider.key')}</div>
                  <div className="prov-keyrow">
                    <div className="prov-keyinput">
                      <input className="prov-input" type={showNewKey ? 'text' : 'password'} placeholder={t('provider.keyPh')} value={newKey} onChange={(e) => { setNewKey(e.target.value); setErr(null) }} aria-label="provider api key" autoComplete="off" spellCheck={false} />
                      <button type="button" className={`key-eye ${showNewKey ? 'on' : ''}`} onClick={() => setShowNewKey((v) => !v)} aria-label={showNewKey ? t('provider.keyHide') : t('provider.keyShow')} title={showNewKey ? t('provider.keyHide') : t('provider.keyShow')} tabIndex={-1}>
                        {showNewKey ? <EyeOffIcon size={14} /> : <EyeIcon size={14} />}
                      </button>
                    </div>
                    <button className="btn" onClick={() => void runNewTest()} disabled={testingNew || !newKey.trim()}>
                      {testingNew ? t('provider.testing') : t('provider.test')}
                    </button>
                  </div>
                  {testNew && (
                    <div className={`prov-test ${testNew.ok ? 'ok' : 'bad'}`} role="status">
                      {testNew.ok ? '✓ ' : '✗ '}
                      {testNew.msg}
                    </div>
                  )}
                </div>
              )}
              <div className="set-row">
                <div className="set-label" />
                <div className="prov-keyrow">
                  <button className="btn primary" onClick={() => void submitAdd()}>{t('provider.addConfirm')}</button>
                </div>
              </div>
            </div>
          </div>
        )}
      </div>

      {providerIds.map((pid) => {
        const pc = cfgOf(pid)
        const isActive = settings.provider.active === pid
        const hasKey = providerKeys[pid]
        const modelNames = Object.keys(pc?.models ?? {})
        const open = isOpen(pid)
        return (
          <div className={`prov-card ${open ? '' : 'collapsed'}`} key={pid}>
            <div
              className="prov-head"
              role="button"
              tabIndex={0}
              aria-expanded={open}
              onClick={() => toggle(pid)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault()
                  toggle(pid)
                }
              }}
            >
              <ChevronDown size={13} className="prov-chev" />
              <span className={`prov-dot ${isActive ? 'on' : ''}`} />
              {renamingProvider === pid ? (
                <input
                  className="prov-input prov-rename-input"
                  value={providerDraft}
                  onChange={(e) => { setProviderDraft(e.target.value); setErr(null) }}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') { e.preventDefault(); void submitRenameProvider() }
                    if (e.key === 'Escape') { setRenamingProvider(null); setErr(null) }
                  }}
                  onBlur={() => void submitRenameProvider()}
                  onClick={(e) => e.stopPropagation()}
                  aria-label={t('provider.renamePh')}
                  autoFocus
                  spellCheck={false}
                />
              ) : (
                <span className="prov-name">{providerDisplay(t, pid) ?? pid}</span>
              )}
              {renamingProvider === pid && (
                <button className="btn primary" onClick={(e) => { e.stopPropagation(); void submitRenameProvider() }}>
                  {t('provider.save')}
                </button>
              )}
              <span className="prov-count">{t('provider.modelCount').replace('{n}', String(modelNames.length))}</span>
              {isActive ? (
                <span className="prov-cur">{t('provider.active')}</span>
              ) : (
                <button
                  className="btn"
                  onClick={(e) => {
                    e.stopPropagation() // 只切 active，不触发折叠切换
                    saveProvider({ provider: pid, active: pid })
                    setExpanded((m) => ({ ...m, [pid]: true })) // 设为当前 → 展开该卡
                  }}
                >
                  {t('provider.setActive')}
                </button>
              )}
              {!isBuiltin(pid) && !renamingProvider && (
                <button className="btn" title={t('provider.rename')} onClick={(e) => {
                  e.stopPropagation()
                  setRenamingProvider(pid)
                  setProviderDraft(pid)
                  setErr(null)
                }}>
                  {t('provider.rename')}
                </button>
              )}
              <button className="btn prov-del" title={t('provider.remove')} onClick={(e) => { e.stopPropagation(); void del(pid) }}>
                {t('provider.remove')}
              </button>
            </div>
            {open && (
              <div className="prov-body">
                <div className="set-row">
                  <div className="set-label">{t('provider.baseURL')}</div>
                  <input
                    className="prov-input"
                    value={baseURLs[pid] ?? pc?.base_url ?? ''}
                    onChange={(e) => setBaseURLs((m) => ({ ...m, [pid]: e.target.value }))}
                    aria-label={`${pid} base url`}
                    spellCheck={false}
                  />
                </div>
                <div className="set-row">
                  <div className="set-label">{t('provider.protocol')}</div>
                  {/* 接入协议：三协议均已实现（chat-completions / responses / anthropic），可自由切换 */}
                  {renderProtocol(pid, protocols[pid] ?? 'chat_completions', (v) => setProtocols((m) => ({ ...m, [pid]: v })))}
                </div>
                {/* usage 口径（2026-09-21）：input_tokens 是否已含缓存。端点违背所声明协议时
                    （anthropic 协议 + OpenAI 计数形状）必须显式标注，否则上下文占用/命中率/成本
                    整体偏一倍。默认「自动」= 协议默认 + 响应自报形状判定。 */}
                <div className="set-row">
                  <div>
                    <div className="set-label">{t('provider.usageIncl')}</div>
                    <div className="set-desc">{t('provider.usageInclDesc')}</div>
                  </div>
                  <div className="seg" role="radiogroup" aria-label={t('provider.usageIncl')}>
                    {USAGE_INCL_OPTIONS.map((opt) => (
                      <button
                        key={opt.value}
                        className={(usageIncls[pid] ?? 'auto') === opt.value ? 'cur' : ''}
                        aria-pressed={(usageIncls[pid] ?? 'auto') === opt.value}
                        onClick={() => setUsageIncls((m) => ({ ...m, [pid]: opt.value }))}
                        title={opt.desc}
                      >
                        {opt.label}
                      </button>
                    ))}
                  </div>
                </div>
                {/* 缓存 TTL（2026-09-21）：默认 5 分钟对本 Harness 的工具批/后台任务/用户思考太短，
                    跨轮缓存必过期。官方端点默认 1h（写入价 2× 基础输入价，代价换命中率）；
                    兼容端点默认 5m（不认识 ttl 扩展字段），需要时在此显式开 1h。 */}
                <div className="set-row">
                  <div>
                    <div className="set-label">{t('provider.cacheTTL')}</div>
                    <div className="set-desc">{t('provider.cacheTTLDesc')}</div>
                  </div>
                  <div className="seg" role="radiogroup" aria-label={t('provider.cacheTTL')}>
                    {CACHE_TTL_OPTIONS.map((opt) => (
                      <button
                        key={opt.value}
                        className={(cacheTTLs[pid] ?? 'auto') === opt.value ? 'cur' : ''}
                        aria-pressed={(cacheTTLs[pid] ?? 'auto') === opt.value}
                        onClick={() => setCacheTTLs((m) => ({ ...m, [pid]: opt.value }))}
                        title={opt.desc}
                      >
                        {opt.label}
                      </button>
                    ))}
                  </div>
                </div>
                {/* OAuth 订阅登录（2026-08 P2）：支持 OAuth 的 provider 显示登录入口；
                    登录后 auth_type=oauth（token 存 oauth.json），key 输入降级为「高级」 */}
                {oauthSpecs[pid] && (
                  <>
                  <div className="set-row">
                    <div>
                      <div className="set-label">{t('provider.oauth')}</div>
                      <div className="set-desc">
                        {oauthStatus[pid]?.logged_in
                          ? `${t('provider.oauthLoggedIn')}${oauthStatus[pid]?.subscription ? ` · ${t('provider.oauthSub')}` : ''}${oauthStatus[pid]?.expires_at ? ` · ${new Date(oauthStatus[pid].expires_at).toLocaleString()}` : ''}`
                          : t('provider.oauthNotLoggedIn')}
                      </div>
                    </div>
                    <div className="prov-keyrow">
                      {oauthStatus[pid]?.logged_in ? (
                        <button className="btn" onClick={() => void oauthLogout(pid)}>{t('provider.oauthLogout')}</button>
                      ) : (
                        <button className="btn primary" onClick={() => void startOAuthLogin(pid)} disabled={oauthLoggingIn === pid}>
                          {oauthLoggingIn === pid ? t('provider.oauthLoggingIn') : (oauthSpecs[pid]?.login_label ?? t('provider.oauthLogin'))}
                        </button>
                      )}
                    </div>
                  </div>
                  {/* 登录事件展示（auth_url / device_code）+ manual 授权码输入：
                      独立于 set-row 之外（set-row 是 flex 行，事件块 flex-basis:100% 会
                      在行内换行导致样式错位）——作为独立块展示登录进度 */}
                  {oauthLoggingIn === pid && oauthEvents[pid] && (
                      <div className="prov-oauth-flow" role="status">
                        {oauthEvents[pid].type === 'auth_url' && (
                          <>
                            <div className="set-desc">{oauthEvents[pid].instructions ?? t('provider.oauthOpenUrl')}</div>
                            <a className="btn" href={oauthEvents[pid].url} onClick={(e) => { e.preventDefault(); window.open(oauthEvents[pid].url, '_blank') }} target="_blank" rel="noreferrer">
                              {t('provider.oauthOpenBrowser')}
                            </a>
                          </>
                        )}
                        {oauthEvents[pid].type === 'device_code' && (
                          <>
                            <div className="set-desc">{t('provider.oauthDeviceCode')}</div>
                            <div className="prov-oauth-code">{oauthEvents[pid].user_code}</div>
                            <a className="btn" href={oauthEvents[pid].verification_uri} onClick={(e) => { e.preventDefault(); window.open(oauthEvents[pid].verification_uri, '_blank') }} target="_blank" rel="noreferrer">
                              {t('provider.oauthOpenBrowser')}
                            </a>
                          </>
                        )}
                        {oauthEvents[pid].type === 'progress' && oauthEvents[pid].message && (
                          <div className="set-desc">… {oauthEvents[pid].message}</div>
                        )}
                        {/* manual 输入：仅当 bridge 显式请求（oauth_prompt_request → type=manual）时显示，
                            避免 auth_url/device_code 正常流程被多余的 URL 输入框挤乱样式 */}
                        {oauthEvents[pid].type === 'manual' && (
                          <div className="prov-keyrow" style={{ marginTop: 6 }}>
                            <input
                              className="prov-input"
                              placeholder={t('provider.oauthManualPh')}
                              value={oauthManual}
                              onChange={(e) => setOAuthManual(e.target.value)}
                              aria-label="oauth manual code"
                              spellCheck={false}
                            />
                            <button className="btn" onClick={() => submitOAuthManual(pid)} disabled={!oauthManual.trim()}>
                              {t('provider.oauthSubmit')}
                            </button>
                          </div>
                        )}
                      </div>
                    )}
                  </>
                )}
                {!isBuiltin(pid) && (
                  <div className="set-row">
                    <div>
                      <div className="set-label">{t('settings.openaiCompat')}</div>
                      <div className="set-desc">{t('settings.openaiCompat.desc')}</div>
                    </div>
                    <label className="prov-1m">
                      <input
                        type="checkbox"
                        checked={Boolean(pc?.openai_compat?.minimal)}
                        onChange={(e) => toggleCompatMinimal(pid, e.target.checked)}
                      />
                      {t('settings.minCompat')}
                    </label>
                  </div>
                )}
                <div className="set-row">
                  <div>
                    <div className="set-label">{t('provider.key')}</div>
                    <div className="set-desc">{hasKey ? t('provider.keySet') : t('provider.keyNotSet')}</div>
                  </div>
                  <div className="prov-keyrow">
                    <div className="prov-keyinput">
                      <input
                        className="prov-input"
                        type={showKey[pid] ? 'text' : 'password'}
                        placeholder={t('provider.keyPh')}
                        value={keys[pid] ?? ''}
                        onChange={(e) => setKeys((m) => ({ ...m, [pid]: e.target.value }))}
                        aria-label={`${pid} api key`}
                        autoComplete="off"
                        spellCheck={false}
                      />
                      <button
                        type="button"
                        className={`key-eye ${showKey[pid] ? 'on' : ''}`}
                        onClick={() => setShowKey((m) => ({ ...m, [pid]: !m[pid] }))}
                        aria-label={showKey[pid] ? t('provider.keyHide') : t('provider.keyShow')}
                        title={showKey[pid] ? t('provider.keyHide') : t('provider.keyShow')}
                        tabIndex={-1}
                      >
                        {showKey[pid] ? <EyeOffIcon size={14} /> : <EyeIcon size={14} />}
                      </button>
                    </div>
                    <button className="btn" onClick={() => void runCardTest(pid)} disabled={testing[pid] || !((keys[pid] ?? '').trim() || pc?.api_key)}>
                      {testing[pid] ? t('provider.testing') : t('provider.test')}
                    </button>
                    <button className="btn primary" onClick={() => save(pid)}>
                      {t('provider.save')}
                    </button>
                  </div>
                  {tests[pid]?.msg && (
                    <div className={`prov-test ${tests[pid].ok ? 'ok' : 'bad'}`} role="status">
                      {tests[pid].ok ? '✓ ' : '✗ '}
                      {tests[pid].msg}
                    </div>
                  )}
                </div>
                <div className="prov-models">
                  <div className="prov-models-hd">
                    <span className="set-label">{t('provider.models')}</span>
                    <span className="prov-models-actions">
                      <button className="btn" onClick={() => { setAddingModel((cur) => (cur === pid ? null : pid)); setErr(null) }}>
                        {addingModel === pid ? t('provider.cancel') : `+ ${t('provider.modelAdd')}`}
                      </button>
                      <button className="btn" onClick={() => pull(pid)} disabled={busy === pid}>
                        {busy === pid ? t('provider.pulling') : t('provider.pull')}
                      </button>
                      {/* 从实时价源补「无价」模型（聚合网关的新模型常无内置价）：补价结果落
                          settings.json（重启保留）；拉取模型列表时也会自动补一次。 */}
                      <button className="btn" onClick={() => void backfillPrices(pid)} disabled={busy === pid} title={t('provider.priceBackfillHint')}>
                        {t('provider.priceBackfill')}
                      </button>
                    </span>
                  </div>
                  {/* 手动添加模型行（本地端点 / 未拉取列表时用；名字即请求 model 字段） */}
                  {addingModel === pid && (
                    <div className="prov-model-add" role="form">
                      <input
                        className="prov-input mono"
                        placeholder={t('provider.modelAddPh')}
                        value={addModelName[pid] ?? ''}
                        onChange={(e) => setAddModelName((m) => ({ ...m, [pid]: e.target.value }))}
                        onKeyDown={(e) => { if (e.key === 'Enter') void submitAddModel(pid) }}
                        aria-label="model name"
                        autoFocus
                        spellCheck={false}
                      />
                      <button className="btn primary" onClick={() => void submitAddModel(pid)} disabled={!(addModelName[pid] ?? '').trim()}>
                        {t('provider.addConfirm')}
                      </button>
                    </div>
                  )}
                  <div className="prov-legend">
                    <span className="prov-legend-name">{t('provider.legendName')}</span>
                    <span className="prov-legend-cap">{t('provider.ctxWindow')}</span>
                    <span className="prov-legend-tok">{t('provider.maxTokens')}</span>
                    <span className="prov-legend-1m">{t('provider.oneM')}</span>
                    <span className="prov-legend-img" title={t('provider.imgDesc')}>{t('provider.img')}</span>
                  </div>
                  {modelNames.length === 0 && <div className="set-placeholder">{t('provider.noModels')}</div>}
                  {modelNames.map((name) => {
                    const renKey = modelKey(pid, name)
                    const renaming = Boolean(renamingModel[renKey])
                    const renDraft = modelDrafts[renKey] ?? name
                    return (
                    <div className="prov-model" key={name}>
                      {renaming ? (
                        <input
                          className="mono prov-input prov-model-name"
                          value={renDraft}
                          onChange={(e) => setModelDrafts((m) => ({ ...m, [renKey]: e.target.value }))}
                          onKeyDown={(e) => {
                            if (e.key === 'Enter') { e.preventDefault(); void submitRenameModel(pid, name) }
                            if (e.key === 'Escape') { setRenamingModel((m) => { const n = { ...m }; delete n[renKey]; return n }); setErr(null) }
                          }}
                          onBlur={() => void submitRenameModel(pid, name)}
                          aria-label={t('provider.modelRenameAria').replace('{name}', name)}
                          autoFocus
                          spellCheck={false}
                        />
                      ) : (
                        <span className="mono prov-model-name" title={name}>{name}</span>
                      )}
                      <NumInput
                        className="prov-num"
                        label={t('provider.ctxWindow')}
                        value={pc?.models[name] ?? 0}
                        onCommit={(n) => setModelWindow(pid, name, n)}
                      />
                      <NumInput
                        className="prov-num"
                        label={t('provider.maxTokens')}
                        value={maxTokensOf(pid, name)}
                        onCommit={(n) => setModelMaxTokens(pid, name, n)}
                      />
                      <label className="prov-1m">
                        <input
                          type="checkbox"
                          checked={(pc?.models[name] ?? 0) >= 1000000}
                          onChange={(e) => toggleModelWindow(pid, name, e.target.checked)}
                        />
                        {t('provider.oneM')}
                      </label>
                      <label className="prov-img" title={t('provider.imgDesc')}>
                        <input
                          type="checkbox"
                          checked={inputTypesForProvider(settings, pid, name).includes('image')}
                          onChange={(e) => toggleModelImage(pid, name, e.target.checked)}
                          aria-label={t('settings.imgInputAria').replace('{name}', name)}
                        />
                        <span className="prov-img-ic"><ImageIcon size={15} /></span>
                      </label>
                      <button
                        className="prov-model-act"
                        title={t('provider.modelRename')}
                        onClick={() => { setRenamingModel((m) => ({ ...m, [renKey]: { provider: pid, old: name } })); setModelDrafts((m) => ({ ...m, [renKey]: name })); setErr(null) }}
                      >
                        ✎
                      </button>
                      <button
                        className="prov-model-act prov-model-del"
                        title={t('provider.modelRemove')}
                        onClick={() => { removeModel(pid, name); showToast(t('provider.saved')) }}
                      >
                        ✕
                      </button>
                      <button
                        className={`prov-model-act prov-model-price-toggle ${priceEditOpen[renKey] ? 'on' : ''}`}
                        title={t('provider.modelPrice')}
                        onClick={() => { setPriceEditOpen((m) => ({ ...m, [renKey]: !m[renKey] })); setErr(null) }}
                      >
                        $
                      </button>
                    </div>
                    )
                  })}
                  {/* 每模型价格编辑块（跟随各自模型行；name → prices[model] 全 0/缺省 = 内置价） */}
                  {modelNames.map((name) => {
                    const renKey = modelKey(pid, name)
                    if (!priceEditOpen[renKey]) return null
                    return <ModelPriceRow key={`price-${name}`} pid={pid} model={name} onSaved={() => showToast(t('provider.saved'))} />
                  })}
                </div>
              </div>
            )}
          </div>
        )
      })}
    </>
  )
}

// —— 每模型数值输入（contextWindow / max_tokens）：本地草稿 + blur/Enter 提交（避免每次击键持久化/IPC）——
function NumInput({ value, onCommit, label, className }: { value: number; onCommit: (n: number) => void; label: string; className?: string }) {
  const [draft, setDraft] = useState(String(value))
  useEffect(() => setDraft(String(value)), [value])
  const commit = () => {
    const n = Number(draft)
    if (Number.isFinite(n) && n > 0) {
      onCommit(Math.round(n))
    } else {
      setDraft(String(value)) // 非法/空 → 回退当前值
    }
  }
  return (
    <input
      className={className ?? 'prov-maxtok'}
      type="number"
      min={1}
      step={1000}
      value={draft}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => {
        if (e.key === 'Enter') commit()
      }}
      aria-label={label}
      title={label}
      spellCheck={false}
    />
  )
}

// —— 每模型价表编辑（USD/1M tokens；2026-09）——
// 输入草稿即时更新、Enter/blur 提交整行（不逐键持久化）；清空全部四格 → 「清除覆盖」
//（回退注册表内置价）；任意格填值 → 整行覆盖（空格按 0 = 免费段）。
function PriceInput({ value, onCommit, label, className }: { value: string; onCommit: (v: string) => void; label: string; className?: string }) {
  return (
    <input
      className={className ?? 'prov-price-num'}
      type="number"
      min={0}
      step={0.001}
      value={value}
      placeholder="默认"
      onChange={(e) => onCommit(e.target.value)}
      aria-label={label}
      title={label}
      spellCheck={false}
    />
  )
}

function ModelPriceRow({ pid, model, onSaved }: { pid: string; model: string; onSaved: () => void }) {
  const t = useT()
  const settings = useAppStore((s) => s.settings)
  const setModelPrices = useAppStore((s) => s.setModelPrices)
  const clearModelPrices = useAppStore((s) => s.clearModelPrices)
  const modelCosts = useAppStore((s) => s.modelCosts)
  const cfg = settings.provider[pid] as ProviderCfg | undefined
  const stored = cfg?.prices?.[model]
  // 注册表内置价（list_models capabilities.cost）：无用户覆盖时作为"当前生效价"回显
  // 占位 —— 此前编辑器只显示空串/「默认」，无法区分「无内置价」与「内置价未拉取」。
  const builtin = modelCosts[pid]?.[model]
  // 草稿初始化：有用户价预填；无 → 回显内置价（内置价全 0/无 → 空串 = 无价表）。
  // 注意：不做 settings→草稿的 effect 同步 —— 折叠再展开（重挂载）即取最新值；
  // 实时同步会在用户输入中途被外部 settings 刷新覆盖草稿。
  const effective = stored && (stored.input > 0 || stored.output > 0 || stored.cache_read > 0 || stored.cache_write > 0)
    ? stored
    : builtin && (builtin.input > 0 || builtin.output > 0 || builtin.cache_read > 0 || builtin.cache_write > 0) ? builtin : undefined
  const [input, setInput] = useState<{ i: string; cr: string; cw: string; o: string }>(() => ({
    i: effective ? String(effective.input) : '',
    cr: effective?.cache_read ? String(effective.cache_read) : '',
    cw: effective?.cache_write ? String(effective.cache_write) : '',
    o: effective ? String(effective.output) : '',
  }))
  const num = (v: string): number => {
    const n = Number(v)
    return Number.isFinite(n) && n >= 0 ? n : NaN
  }
  const commit = () => {
    const vals = { i: num(input.i), cr: num(input.cr), cw: num(input.cw), o: num(input.o) }
    const latest = cfg?.prices?.[model]
    // 非法输入 → 复位草稿不提交
    if ([vals.i, vals.cr, vals.cw, vals.o].some((n) => Number.isNaN(n))) {
      setInput({
        i: latest ? String(latest.input) : effective ? String(effective.input) : '',
        cr: latest?.cache_read ? String(latest.cache_read) : effective?.cache_read ? String(effective.cache_read) : '',
        cw: latest?.cache_write ? String(latest.cache_write) : effective?.cache_write ? String(effective.cache_write) : '',
        o: latest ? String(latest.output) : effective ? String(effective.output) : '',
      })
      return
    }
    const allEmpty = [vals.i, vals.cr, vals.cw, vals.o].every((n) => n === 0)
    if (allEmpty) {
      clearModelPrices(pid, model) // 全空 = 清除覆盖（回退内置价）
    } else {
      setModelPrices(pid, model, { input: vals.i, cache_read: vals.cr, cache_write: vals.cw, output: vals.o })
    }
    onSaved()
  }
  const resetToBuiltin = () => {
    clearModelPrices(pid, model)
    setInput({
      i: builtin ? String(builtin.input) : '',
      cr: builtin?.cache_read ? String(builtin.cache_read) : '',
      cw: builtin?.cache_write ? String(builtin.cache_write) : '',
      o: builtin ? String(builtin.output) : '',
    })
    onSaved()
  }
  return (
    <div className="prov-price-edit">
      <div className="prov-price-hint">{t('provider.priceHint')}</div>
      <div className="prov-price-grid">
        <label>{t('provider.priceInput')}<PriceInput value={input.i} onCommit={(v) => setInput((s) => ({ ...s, i: v }))} label={t('provider.priceInput')} /></label>
        <label>{t('provider.priceCacheRead')}<PriceInput value={input.cr} onCommit={(v) => setInput((s) => ({ ...s, cr: v }))} label={t('provider.priceCacheRead')} /></label>
        <label>{t('provider.priceCacheWrite')}<PriceInput value={input.cw} onCommit={(v) => setInput((s) => ({ ...s, cw: v }))} label={t('provider.priceCacheWrite')} /></label>
        <label>{t('provider.priceOutput')}<PriceInput value={input.o} onCommit={(v) => setInput((s) => ({ ...s, o: v }))} label={t('provider.priceOutput')} /></label>
        <button className="btn primary" onClick={commit}>{t('provider.save')}</button>
        <button className="btn" onClick={resetToBuiltin} title={t('provider.priceReset')}>{t('provider.priceReset')}</button>
      </div>
    </div>
  )
}

// IM 集成 tab：配置 + 网关 Schema 动态表单（IMSettingsEditor）。
function IMTab() {
  return <IMSettingsEditor />
}

// 插件 tab：内置能力包清单 + 状态（就绪/已启用/禁用/错误）。
// 2026-09：Browser Use 组件随应用内置（vendor 启动自动同步）——无「安装/升级/卸载」，
// 仅 启用/禁用/设置；error = 组件同步失败或 node 缺失（显示原因 + 重试）。
function PluginTab() {
  const t = useT()
  const plugins = useAppStore((s) => s.plugins)
  const browserEngine = useAppStore((s) => s.browserEngine)
  const egoStatus = useAppStore((s) => s.egoStatus)
  const fetchPlugins = useAppStore((s) => s.fetchPlugins)
  const fetchBrowserEngine = useAppStore((s) => s.fetchBrowserEngine)
  const refreshEgoStatus = useAppStore((s) => s.refreshEgoStatus)
  const setBrowserEngine = useAppStore((s) => s.setBrowserEngine)
  const installPlugin = useAppStore((s) => s.installPlugin) // 重试同步（错误恢复）
  const enablePlugin = useAppStore((s) => s.enablePlugin)
  const disablePlugin = useAppStore((s) => s.disablePlugin)
  const [configuring, setConfiguring] = useState<string | null>(null) // 内联展开配置编辑器的插件 id
  const [engSwitching, setEngSwitching] = useState(false)
  const [egoInstalling, setEgoInstalling] = useState(false)
  const [egoLaunching, setEgoLaunching] = useState(false)
  const [engineMsg, setEngineMsg] = useState('') // 切换结果反馈

  useEffect(() => {
    fetchPlugins()
    fetchBrowserEngine()
  }, [fetchPlugins, fetchBrowserEngine])

  const eng = browserEngine ?? 'mcp'
  const egoReady = !!egoStatus?.available
  // 已安装（磁盘检测 + 持久化记录，见 bridge ego_install.go）：装过一次就不再显示安装按钮
  // ——修复「本机已装 ego lite，重启后仍提示下载安装」。已装未运行时引导「启动」而非重装。
  const egoInstalled = !!egoStatus?.installed

  // 切换引擎：落盘 settings.json（Electron settings:set）+ bridge 热应用（browser_engine_set）
  const switchEngine = async (next: 'mcp' | 'ego') => {
    if (next === eng || engSwitching) return
    setEngSwitching(true)
    setEngineMsg('')
    try {
      // 先落盘 settings.json（bridge 启动/热读读它；命令带 engine payload 兜底时序）
      if (window.desktop?.settingsSet) {
        const r = await window.desktop.settingsSet({ browser_engine: next } as Partial<AppSettings>)
        if (!r.ok) {
          setEngineMsg(r.error ?? t('plugin.engine.errSave'))
          return
        }
      }
      const res = await setBrowserEngine(next)
      setEngineMsg(res.ok ? '' : res.error ?? '')
    } finally {
      setEngSwitching(false)
    }
  }

  // 下载安装 ego lite（跑随包 skill 的 install.sh；完成后用户需在 app 内 onboarding）
  const installEgo = async () => {
    setEgoInstalling(true)
    setEngineMsg('')
    try {
      const r = await window.desktop?.egoInstall()
      if (r?.ok) setEngineMsg(t('plugin.engine.egoInstalled'))
      else setEngineMsg(r?.error ?? t('plugin.engine.egoInstallFail'))
      // 安装完成 → 强制重新检测（refresh_ego 忽略 bridge 缓存）：已安装立即落持久化记录，
      // 状态切到「已安装」→ 安装按钮消失（不再重复提示安装）。
      refreshEgoStatus()
    } finally {
      setEgoInstalling(false)
    }
  }

  // 启动已安装的 ego lite（open -a；bridge 的 ego_activate_request 同路径，
  // 但这里是用户显式动作 —— 不需要引擎=ego 才可用）。
  const launchEgo = async () => {
    setEgoLaunching(true)
    setEngineMsg('')
    try {
      const r = await window.desktop?.egoLaunch()
      if (!r?.ok) setEngineMsg(r?.error ?? t('plugin.engine.egoLaunchFail'))
      else setEngineMsg('')
      // 启动后稍等再查（进程/CLI 注册需要一点时间），强制重新检测拿最新状态
      setTimeout(() => refreshEgoStatus(), 1200)
    } finally {
      setEgoLaunching(false)
    }
  }

  const statusLabel = (s: string): string => {
    switch (s) {
      case 'ready':
        return t('plugin.status.ready')
      case 'enabled':
        return t('plugin.status.enabled')
      case 'installing':
        return t('plugin.installing')
      case 'error':
        return t('plugin.status.error')
      default:
        return t('plugin.status.ready') // 内置组件：无 not-installed（启动自动同步）
    }
  }

  return (
    <>
      {/* 浏览器引擎（mcp | ego 互斥，2026-09） */}
      <div className="set-row" style={{ marginBottom: 6 }}>
        <div>
          <div className="set-label">{t('plugin.engine.title')}</div>
          <div className="set-desc">{t('plugin.engine.desc')}</div>
          {eng === 'ego' && (
            <div className="set-desc" style={{ color: 'var(--warn, #d29922)' }}>{t('plugin.engine.notice')}</div>
          )}
        </div>
        <div className="seg" role="radiogroup" aria-label={t('plugin.engine.title')}>
          <button
            className={eng === 'mcp' ? 'cur' : ''}
            aria-pressed={eng === 'mcp'}
            disabled={engSwitching}
            onClick={() => void switchEngine('mcp')}
          >
            {t('plugin.engine.mcp')}
          </button>
          <button
            className={eng === 'ego' ? 'cur' : ''}
            aria-pressed={eng === 'ego'}
            disabled={engSwitching}
            onClick={() => void switchEngine('ego')}
          >
            {t('plugin.engine.ego')}
          </button>
        </div>
      </div>
      {/* ego 状态分层（2026-09）：可用 / 已安装未可用 / 未安装 —— 已安装一律不显示安装按钮 */}
      {egoReady || egoInstalled || eng === 'ego' ? (
        <div className="set-row" style={{ marginBottom: 6 }}>
          <div>
            <div className="set-desc">
              {egoReady
                ? t('plugin.engine.egoReady').replace('{v}', egoStatus?.version ?? '')
                : egoInstalled
                  ? t('plugin.engine.egoInstalledLabel')
                  : t('plugin.engine.egoMissing')}
            </div>
            {!egoReady && (
              <div className="set-desc">
                {egoInstalled
                  // 已装未可用：区分「未运行」与「未完成引导（CLI 未注册）」——不提示重装
                  ? (egoStatus?.cli_found
                      ? t('plugin.engine.egoInstalledNotRunning')
                      : t('plugin.engine.egoInstalledNoCli'))
                  : t('plugin.engine.egoRunHint')}
              </div>
            )}
            {!egoReady && egoInstalled && (
              <div className="set-desc">{t('plugin.engine.egoInstalledHint')}</div>
            )}
          </div>
          {!egoReady && !egoInstalled && (
            <button className="btn" disabled={egoInstalling} onClick={() => void installEgo()}>
              {egoInstalling ? t('plugin.installing') : t('plugin.engine.egoInstall')}
            </button>
          )}
          {/* 已安装未运行 → 引导「启动」而不是重新下载（本机已装的 app 不该再让用户装一次） */}
          {!egoReady && egoInstalled && !egoStatus?.app_running && (
            <button className="btn" disabled={egoLaunching} onClick={() => void launchEgo()}>
              {egoLaunching ? t('plugin.engine.egoLaunching') : t('plugin.engine.egoLaunch')}
            </button>
          )}
        </div>
      ) : null}
      {engineMsg && <div className="mcp-s-err mono" style={{ marginBottom: 6 }}>{engineMsg}</div>}
      <p className="set-desc" style={{ marginBottom: 10 }}>{t('plugin.hint')}</p>
      {plugins.length === 0 ? (
        <p className="set-placeholder">{t('plugin.empty')}</p>
      ) : (
        plugins
          .filter((p) => !(p.id === 'browser' && eng === 'ego')) // 引擎=ego：Browser Use 工具不注册，卡片隐藏（引擎切换控件在上方，用户可切回 mcp 恢复）
          .map((p) => {
          const installing = p.status === 'installing'
          return (
            <div className="mcp-server" key={p.id}>
              <div className="mcp-s-h">
                <span
                  className={`mcp-state ${p.status === 'enabled' ? 'connected' : p.status === 'error' ? 'error' : 'idle'}`}
                  title={p.status}
                >
                  {statusLabel(p.status)}
                </span>
                <span className="mcp-name">{p.name}</span>
                <span className="grow" />
                {p.installed && (
                  <span className="mcp-type mono">
                    {t('plugin.version').replace('{v}', p.installed).replace('{p}', p.pinned ?? '')}
                  </span>
                )}
                {p.status === 'error' && !installing ? (
                  // 同步失败 / node 缺失：重试（重新触发 vendor 同步）
                  <button className="btn" onClick={() => installPlugin(p.id)}>{t('plugin.retry')}</button>
                ) : null}
                {p.enabled ? (
                  <button className="btn" onClick={() => disablePlugin(p.id)}>{t('plugin.disable')}</button>
                ) : p.status === 'ready' ? (
                  <button className="btn" onClick={() => enablePlugin(p.id)}>{t('plugin.enable')}</button>
                ) : null}
                {p.configurable && p.status !== 'error' && !installing && (
                  <button className="btn" onClick={() => setConfiguring(configuring === p.id ? null : p.id)}>
                    {t('plugin.config')}
                  </button>
                )}
              </div>
              <div className="mcp-s-cmd">{p.description}</div>
              {(p.error || (p.status === 'error' && p.reason)) && (
                <div className="mcp-s-err mono">
                  {p.error ?? (p.reason === 'NODE_MISSING' ? t('plugin.nodeMissing') : p.reason)}
                </div>
              )}
              {configuring === p.id && (p.id === 'computer' ? (
                <ComputerSettingsEditor onClose={() => setConfiguring(null)} />
              ) : (
                <BrowserSettingsEditor onClose={() => setConfiguring(null)} />
              ))}
            </div>
          )
        })
      )}
    </>
  )
}

// —— Session Mesh 会话间通信（基础设置栏）：C1 本机互聊 + C2 远程多节点 ——
// 缺省全关；开启后才注册 session_send/session_list 工具（bridge 热重载生效）。
function MeshTab() {
  const t = useT()
  const settings = useAppStore((s) => s.settings)
  const setMeshSessionEnabled = useAppStore((s) => s.setMeshSessionEnabled)
  const setMeshRemoteEnabled = useAppStore((s) => s.setMeshRemoteEnabled)
  const setMeshRemoteCfg = useAppStore((s) => s.setMeshRemoteCfg)
  const showToast = useAppStore((s) => s.showToast)
  const meshStatus = useAppStore((s) => s.meshStatus)
  const fetchMeshStatus = useAppStore((s) => s.fetchMeshStatus)
  const pingMeshNode = useAppStore((s) => s.pingMeshNode)
  const mesh = settings.mesh
  const sessionOn = mesh?.session_enabled ?? false
  const remoteOn = mesh?.remote_enabled ?? false

  // 打开 tab 即拉一次状态（「别人怎么连我」需最新监听/网卡信息）
  useEffect(() => {
    void fetchMeshStatus()
  }, [fetchMeshStatus, remoteOn])

  // 配置草稿。节点标识缺省直接用后端权威 UUID（不再 "auto" 占位）；
  // 连接凭据缺省自动生成唯一值（开箱即用）。
  const [listenAddr, setListenAddr] = useState(mesh?.remote?.listen_addr ?? '127.0.0.1:17890')
  const [authToken, setAuthToken] = useState(mesh?.remote?.auth_token || meshToken())
  const [showToken, setShowToken] = useState(false)
  const [nodeID, setNodeID] = useState(mesh?.remote?.node_id || meshUUID())
  const [tlsCert, setTlsCert] = useState(mesh?.remote?.tls_cert ?? '')
  const [tlsKey, setTlsKey] = useState(mesh?.remote?.tls_key ?? '')
  const [peerID, setPeerID] = useState('')
  const [peerAddr, setPeerAddr] = useState('')
  const [peerToken, setPeerToken] = useState('')
  const [copiedIdx, setCopiedIdx] = useState(-1)
  // 主动 ping 状态：正在 ping 的节点 id + 最近一次结果（rtt/错误）
  const [pingingNode, setPingingNode] = useState<string | null>(null)
  const [pingResult, setPingResult] = useState<{ node: string; ok: boolean; rttMs?: number; error?: string } | null>(null)

  // 主动检测与某节点的连接（mesh_ping）：成功展示 RTT，失败展示错误；随后刷新状态。
  const pingPeer = async (nodeId: string) => {
    setPingingNode(nodeId)
    setPingResult(null)
    const r = await pingMeshNode(nodeId)
    setPingingNode(null)
    setPingResult({ node: nodeId, ...r })
    fetchMeshStatus()
  }

  // 水合：mesh_status 返回的 node_id 是后端权威（~/.go-code/mesh_node_id 持久化），
  // 若设置里没显式配过（或还是 auto），用真实 UUID 回填展示，保证用户看到的就是
  // 对方可用的 node://<标识> 前缀。
  useEffect(() => {
    if (meshStatus?.node_id && (!mesh?.remote?.node_id || mesh?.remote?.node_id === 'auto')) {
      setNodeID(meshStatus.node_id)
    }
  }, [meshStatus?.node_id, mesh?.remote?.node_id])

  // 自动生成凭据兜底：settings 里没存 auth_token（首次用）→ 把自动生成的随机 token
  // 持久化（无论 remote 开关——token 是身份，需稳定；用户打开即可看到固定值）。
  // 只跑一次：落盘后 mesh.remote.auth_token 非空，条件不再满足。
  useEffect(() => {
    if (!mesh?.remote?.auth_token && authToken) {
      setMeshRemoteCfg({
        listen_addr: listenAddr.trim() || '127.0.0.1:17890',
        auth_token: authToken,
        node_id: nodeID.trim() || meshStatus?.node_id || meshUUID(),
        tls_cert: '',
        tls_key: '',
        peers: mesh?.remote?.peers ?? [],
      })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 统一提交远程配置（含 TLS/peers）；保存后热重载 + Toast 反馈
  const commitRemote = (patch: Partial<NonNullable<MeshSettings['remote']>>) => {
    const cur = mesh?.remote ?? {}
    setMeshRemoteCfg({
      listen_addr: listenAddr.trim() || '127.0.0.1:17890',
      auth_token: authToken || meshToken(),
      node_id: nodeID.trim() || meshStatus?.node_id || meshUUID(),
      tls_cert: tlsCert.trim(),
      tls_key: tlsKey.trim(),
      peers: cur.peers ?? [],
      ...patch,
    })
    showToast(t('settings.mesh.saved'))
  }
  const addPeer = () => {
    if (!peerID.trim() || !peerAddr.trim()) return
    const cur = mesh?.remote?.peers ?? []
    commitRemote({ peers: [...cur, { id: peerID.trim(), addr: peerAddr.trim(), token: peerToken.trim() }] })
    setPeerID('')
    setPeerAddr('')
    setPeerToken('')
  }
  const removePeer = (id: string) => {
    commitRemote({ peers: (mesh?.remote?.peers ?? []).filter((p) => p.id !== id) })
  }
  const copyTextToClipboard = async (text: string, idx: number) => {
    // copyText：浏览器 API + 主进程兜底（见 src/lib/clipboard.ts）
    const ok = await copyText(text)
    if (ok) {
      setCopiedIdx(idx)
      setTimeout(() => setCopiedIdx(-1), 1500)
    } else {
      showToast(t('common.copyFail'), 'error')
    }
  }

  return (
    <>
      <div className="settings-sec-title">{t('settings.mesh')}</div>
      <p className="set-desc">{t('settings.mesh.desc')}</p>

      {/* —— 能力开关 —— */}
      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.mesh.session')}</div>
          <div className="set-desc">{t('settings.mesh.session.desc')}</div>
        </div>
        <div className="seg" role="radiogroup" aria-label={t('settings.mesh.session')}>
          <button className={sessionOn ? 'cur' : ''} aria-pressed={sessionOn} onClick={() => setMeshSessionEnabled(true)}>{t('settings.on')}</button>
          <button className={!sessionOn ? 'cur' : ''} aria-pressed={!sessionOn} onClick={() => setMeshSessionEnabled(false)}>{t('settings.off')}</button>
        </div>
      </div>
      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.mesh.remote')}</div>
          <div className="set-desc">{t('settings.mesh.remote.desc')}</div>
        </div>
        <div className="seg" role="radiogroup" aria-label={t('settings.mesh.remote')}>
          <button className={remoteOn ? 'cur' : ''} aria-pressed={remoteOn} disabled={!sessionOn} onClick={() => setMeshRemoteEnabled(true)}>{t('settings.on')}</button>
          <button className={!remoteOn ? 'cur' : ''} aria-pressed={!remoteOn} onClick={() => setMeshRemoteEnabled(false)}>{t('settings.off')}</button>
        </div>
      </div>

      {remoteOn && (
        <>
          {/* —— 本机连接信息（别人怎么连我）—— */}
          <div className="mesh-card">
            <div className="set-label">{t('settings.mesh.connTitle')}</div>
            <p className="set-desc">{t('settings.mesh.connDesc')}</p>
            <div className="mesh-conn-node">
              <span className="set-label">{t('settings.mesh.connNode')}</span>
              <code className="mesh-code">{meshStatus?.node_id ?? mesh?.remote?.node_id ?? nodeID}</code>
              <span className="set-desc">{t('settings.mesh.connNodeDesc')}</span>
            </div>
            {(meshStatus?.connect_urls?.length ?? 0) === 0 ? (
              <p className="set-desc mesh-conn-empty">{t('settings.mesh.connEmpty')}</p>
            ) : (
              <div className="mesh-conn-urls">
                {(meshStatus?.connect_urls ?? []).map((u, i) => (
                  <div className="mesh-conn-url" key={u}>
                    <code className="mesh-code">{u}</code>
                    <button className="btn" onClick={() => copyTextToClipboard(u, i)}>
                      {copiedIdx === i ? t('settings.mesh.connCopied') : t('settings.mesh.connCopy')}
                    </button>
                  </div>
                ))}
              </div>
            )}
            <div className="mesh-conn-meta">
              <span className="set-desc">
                {t('settings.mesh.connTLS')}: {meshStatus?.tls ? t('settings.mesh.connTLSYes') : t('settings.mesh.connNoTLS')}
              </span>
              <button className="btn" onClick={() => fetchMeshStatus()}>{t('settings.mesh.refresh')}</button>
            </div>
            {/* 已连接节点 */}
            <div className="set-label mesh-links-label">{t('settings.mesh.links')}</div>
            {(meshStatus?.links?.length ?? 0) === 0 ? (
              <p className="set-desc">{t('settings.mesh.linksEmpty')}</p>
            ) : (
              <div className="mesh-links">
                {(meshStatus?.links ?? []).map((l) => (
                  <div className="mesh-link" key={l.node_id}>
                    <code className="mesh-code">{l.node_id}</code>
                    <span className="set-desc">{l.inbound ? '← 连入' : '→ 连出'}</span>
                  </div>
                ))}
              </div>
            )}
            {/* 对端在线会话（session_list 远程部分；含标题） */}
            <div className="set-label mesh-links-label">{t('settings.mesh.remoteSessions')}</div>
            {(meshStatus?.remote_sessions?.length ?? 0) === 0 ? (
              <p className="set-desc">{t('settings.mesh.remoteSessionsEmpty')}</p>
            ) : (
              <div className="mesh-links">
                {(meshStatus?.remote_sessions ?? []).map((s) => (
                  <div className="mesh-link" key={`${s.node}/${s.id}`}>
                    <code className="mesh-code">{s.node.slice(0, 8)}…/{s.id}</code>
                    <span className="set-desc">{s.name || s.id} · {s.status}</span>
                  </div>
                ))}
              </div>
            )}
          </div>

          {/* —— 本机监听配置 —— */}
          <div className="mesh-card">
            <div className="set-label">{t('settings.mesh.listen')}</div>
            <p className="set-desc">{t('settings.mesh.listen.desc')}</p>
            <div className="mesh-field">
              <input className="text-input" value={listenAddr} onChange={(e) => setListenAddr(e.target.value)} spellCheck={false} aria-label={t('settings.mesh.listen')} />
            </div>
            <div className="mesh-field">
              <span className="set-label">{t('settings.mesh.nodeID')}</span>
              <input className="text-input" value={nodeID} onChange={(e) => setNodeID(e.target.value)} spellCheck={false} aria-label={t('settings.mesh.nodeID')} />
              <span className="set-desc">{t('settings.mesh.nodeID.desc')}</span>
            </div>
            <div className="mesh-field">
              <span className="set-label">{t('settings.mesh.token')}</span>
              <div className="token-input-wrap">
                <input className="text-input" type={showToken ? 'text' : 'password'} value={authToken} onChange={(e) => setAuthToken(e.target.value)} spellCheck={false} aria-label={t('settings.mesh.token')} />
                <button className="btn token-eye" onClick={() => setShowToken((v) => !v)} aria-label={showToken ? t('settings.mesh.tokenHide') : t('settings.mesh.tokenShow')}>
                  {showToken ? <EyeOffIcon /> : <EyeIcon />}
                </button>
              </div>
              <span className="set-desc">{t('settings.mesh.token.desc')}</span>
            </div>
            {/* TLS（可选；不配 = ws 不报错） */}
            <div className="mesh-field">
              <span className="set-label">{t('settings.mesh.tls')}</span>
              <span className="set-desc">{t('settings.mesh.tls.desc')}</span>
              <input className="text-input" placeholder={t('settings.mesh.tlsCert')} value={tlsCert} onChange={(e) => setTlsCert(e.target.value)} spellCheck={false} aria-label={t('settings.mesh.tlsCert')} />
              <input className="text-input" placeholder={t('settings.mesh.tlsKey')} value={tlsKey} onChange={(e) => setTlsKey(e.target.value)} spellCheck={false} aria-label={t('settings.mesh.tlsKey')} />
            </div>
            <div className="mesh-save-row">
              <button className="btn primary" onClick={() => commitRemote({})}>{t('settings.save')}</button>
            </div>
          </div>

          {/* —— 出站节点（对端列表）—— */}
          <div className="mesh-card">
            <div className="set-label">{t('settings.mesh.peers')}</div>
            {(mesh?.remote?.peers ?? []).length === 0 && <p className="set-desc">{t('settings.mesh.peers.empty')}</p>}
            {(mesh?.remote?.peers ?? []).map((p) => {
              // 拨号状态（mesh_status.peers；保存后热重启即刷新）
              const ps = (meshStatus?.peers ?? []).find((x) => x.node_id === p.id)
              return (
                <div className="perm-item" key={p.id}>
                  <span className="perm-name" title={`${p.addr}${p.token ? ' (token)' : ''}`}>
                    {p.id} · {p.addr}
                    {/* 连接状态/最近错误（排障：为什么没连上） */}
                    <span className={`mesh-peer-state ${ps?.connected ? 'ok' : 'err'}`}>
                      {ps?.connected
                        ? t('settings.mesh.peerConnected')
                        : ps?.last_error
                          ? `${t('settings.mesh.peerFailed')}: ${ps.last_error}`
                          : t('settings.mesh.peerConnecting')}
                    </span>
                  </span>
                  <button
                    className="btn"
                    disabled={pingingNode === p.id}
                    onClick={() => void pingPeer(p.id)}
                  >
                    {pingingNode === p.id ? t('settings.mesh.peerPinging') : t('settings.mesh.peerPing')}
                  </button>
                  <button className="btn" onClick={() => removePeer(p.id)}>{t('permissions.remove')}</button>
                </div>
              )
            })}
            {/* ping 结果提示（连接检测反馈） */}
            {pingResult && (
              <p className={`set-desc mesh-ping-result ${pingResult.ok ? 'ok' : 'err'}`}>
                {pingResult.ok
                  ? `${t('settings.mesh.pingOk')} ${pingResult.node} · ${pingResult.rttMs ?? '-'} ms`
                  : `${t('settings.mesh.pingFail')} ${pingResult.node}: ${pingResult.error ?? ''}`}
              </p>
            )}
            <div className="mesh-peer-add">
              <input className="text-input" placeholder={t('settings.mesh.peerID')} value={peerID} onChange={(e) => setPeerID(e.target.value)} spellCheck={false} aria-label={t('settings.mesh.peerID')} />
              <input className="text-input" placeholder={t('settings.mesh.peerAddrPh')} value={peerAddr} onChange={(e) => setPeerAddr(e.target.value)} spellCheck={false} aria-label={t('settings.mesh.peerAddrPh')} />
              <input className="text-input" placeholder={t('settings.mesh.peerTokenPh')} type="password" value={peerToken} onChange={(e) => setPeerToken(e.target.value)} spellCheck={false} aria-label={t('settings.mesh.peerTokenPh')} />
              <button className="btn primary" onClick={addPeer} disabled={!peerID.trim() || !peerAddr.trim()}>{t('settings.mesh.peerAdd')}</button>
            </div>
          </div>
        </>
      )}
    </>
  )
}
