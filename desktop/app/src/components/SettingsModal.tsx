import { useEffect, useRef, useState } from 'react'
import type { ReactElement } from 'react'
import { useAppStore, SUBAGENT_CONCURRENCY_DEFAULT, COMPRESS_THRESHOLD_DEFAULT, COMPRESS_THRESHOLD_MIN, COMPRESS_THRESHOLD_MAX, FIRST_CHUNK_TIMEOUT_DEFAULT, FIRST_CHUNK_TIMEOUT_MIN, FIRST_CHUNK_TIMEOUT_MAX } from '../store/useAppStore'
import { useT } from '../i18n'
import { meshUUID, meshToken } from '../lib/meshRand'
import { copyText } from '../lib/clipboard'
import { AccessIcon, BarChart, ChevronDown, EyeIcon, EyeOffIcon, Gear, Globe, HookIcon, LayersIcon, PaletteIcon, PinIcon, SendIcon, SessionIcon, Smartphone, Sparkles, TerminalIcon } from './icons'
import { BrowserSettingsEditor } from './BrowserSettingsEditor'
import { ComputerSettingsEditor } from './ComputerSettingsEditor'
import { IMSettingsEditor } from './IMSettingsEditor'
import AppearanceTab from './AppearanceTab'
import { MCPTab } from './MCPTab'
import { HooksTab } from './HooksTab'
import SurfacesPage from './SurfacesPage'
import MobileTab from './MobileTab'
import UsageStats from './UsageStats'
import ProviderPage from './ProviderPage'
import Sel, { type SelOption } from './Sel'
import type { MeshSettings, AppSettings, HubConversationSettings } from '../transport/types'
import type { DreamSettingsInput } from '../store/useAppStore'
import { bridge } from '../lib/bridge'

// 左侧导航分组（对齐设计稿：基础项 → 模型 → Agent 能力 → 数据与统计）。
// 表驱动而非七段硬编码：新增模块只加一行，分组标题由 label 决定（null = 不分组，顶部基础项）。
type SettingsTab = 'general' | 'appearance' | 'shortcuts' | 'provider' | 'mcp' | 'hooks' | 'plugin' | 'surfaces' | 'im' | 'mesh' | 'mobile' | 'memory' | 'usage'
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
      // 面板插件（PR-14b）：与上面那条 `plugin` 是**两套**插件系统（那套是 MCP/浏览器引擎/
      // ego 之类的宿主插件，这套是 `~/.go-code/surfaces/` 里的 Surface 面板）。
      { key: 'surfaces', label: 'settings.surfaces', icon: <LayersIcon size={14} /> },
      { key: 'im', label: 'settings.im', icon: <SessionIcon size={14} /> },
      { key: 'mesh', label: 'settings.mesh', icon: <SendIcon size={14} /> },
      { key: 'mobile', label: 'settings.mobile', icon: <Smartphone size={14} /> },
      { key: 'memory', label: 'settings.memory', icon: <PinIcon size={14} /> },
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
          {tab === 'provider' && <ProviderPage />}
          {tab === 'mcp' && <MCPTab />}
          {tab === 'hooks' && <HooksTab />}
          {tab === 'plugin' && <PluginTab />}
          {tab === 'surfaces' && <SurfacesPage />}
          {tab === 'im' && <IMTab />}
          {tab === 'mesh' && <MeshTab />}
          {tab === 'mobile' && <MobileTab />}
          {tab === 'memory' && <MemoryTab />}
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
  const setCodemodeEnabled = useAppStore((s) => s.setCodemodeEnabled)
  const setCronAutoClean = useAppStore((s) => s.setCronAutoClean)
  const setCompressThreshold = useAppStore((s) => s.setCompressThreshold)
  // 首字预算（秒）：与 store 的钳制口径一致地兜底（缺配置 → 默认 90）。
  const setFirstChunkTimeout = useAppStore((s) => s.setFirstChunkTimeout)
  const firstChunkTimeoutRaw = settings.agent?.first_chunk_timeout ?? FIRST_CHUNK_TIMEOUT_DEFAULT
  const firstChunkTimeout =
    Number.isFinite(firstChunkTimeoutRaw) && firstChunkTimeoutRaw > 0
      ? Math.min(FIRST_CHUNK_TIMEOUT_MAX, Math.max(FIRST_CHUNK_TIMEOUT_MIN, Math.round(firstChunkTimeoutRaw)))
      : FIRST_CHUNK_TIMEOUT_DEFAULT
  // 压缩阈值（占窗口比例）：与 store 的钳制口径一致地兜底（缺配置 → 默认 0.8）。
  // 滑杆的 value 必须是整数百分比，浮点比例 ×100 后用 Math.round 归一。
  const compressThresholdRaw = settings.agent?.compress_threshold ?? COMPRESS_THRESHOLD_DEFAULT
  const compressThreshold =
    Number.isFinite(compressThresholdRaw) && compressThresholdRaw > 0
      ? Math.min(COMPRESS_THRESHOLD_MAX, Math.max(COMPRESS_THRESHOLD_MIN, compressThresholdRaw))
      : COMPRESS_THRESHOLD_DEFAULT
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
      {/* 上下文压缩阈值（占模型上下文窗口比例）：用小步进滑杆 + 数字输入双控件。
           调小 = 更早压缩（单次请求更短、更便宜，但压缩更频繁）；调大 = 更晚压缩。 */}
      <div className="set-row">
        <div><div className="set-label">{t('settings.compressThreshold')}</div><div className="set-desc">{t('settings.compressThreshold.desc')}</div></div>
        <div className="set-inline">
          <input
            className="range-input"
            type="range" min={5} max={95} step={1}
            aria-label={t('settings.compressThreshold')}
            value={Math.round(compressThreshold * 100)}
            onChange={(e) => setCompressThreshold(Number(e.target.value) / 100)}
          />
          <input
            className="text-input"
            type="number" min={5} max={95} step={5}
            value={Math.round(compressThreshold * 100)}
            onChange={(e) => setCompressThreshold(Number(e.target.value) / 100)}
          />
          <span className="set-unit">%</span>
        </div>
      </div>
      {/* 首字预算（秒）：单次尝试「请求发出 → 首个流事件」的上限，首字一到即解除、
           流开始后不限总时长。调大 = 更容忍弱网/长首字模型；调小 = 更快判定上游无响应。 */}
      <div className="set-row">
        <div><div className="set-label">{t('settings.firstChunkTimeout')}</div><div className="set-desc">{t('settings.firstChunkTimeout.desc')}</div></div>
        <div className="set-inline">
          <input className="text-input" type="number" min={FIRST_CHUNK_TIMEOUT_MIN} max={FIRST_CHUNK_TIMEOUT_MAX} step={10} value={firstChunkTimeout} onChange={(e) => setFirstChunkTimeout(Number(e.target.value))} />
          <span className="set-unit">s</span>
        </div>
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
      {/* codemode（脚本编排工具）：默认关；开启后模型多一个「写脚本串工具」的入口，
          且 MCP 的「未声明」档位默认落点翻成 deferred（脚本里 searchTools 现查）。
          切换后走 reload_settings 重建 loop —— 工具注册与 MCP 档位都在装配期定型。 */}
      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.codemode')}</div>
          <div className="set-desc">{t('settings.codemode.desc')}</div>
        </div>
        <div className="seg" role="radiogroup" aria-label={t('settings.codemode')}>
          <button className={(settings.codemode?.enabled ?? false) ? 'cur' : ''} aria-pressed={settings.codemode?.enabled ?? false} onClick={() => setCodemodeEnabled(true)}>{t('settings.on')}</button>
          <button className={!(settings.codemode?.enabled ?? false) ? 'cur' : ''} aria-pressed={!(settings.codemode?.enabled ?? false)} onClick={() => setCodemodeEnabled(false)}>{t('settings.off')}</button>
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
      {/* 办公组件（LibreOffice kit，R08d）：版本/体积/许可/路径/状态 + 重新探测。
          只展示与说明，**不自动下载** —— 分发是 PR-13。 */}
      {/* 群聊对话引擎护栏（P6-04/D8）：`hub.conversation` 全局旋钮 + provider 并发上限 */}
      <ConversationSection />
      <OfficeKitSection />
      {/* 语音输入（STT）：整段折进可折叠卡片（默认收起）——OpenAI 兼容 /audio/transcriptions 端点 */}
      <STTSection />
    </>
  )
}

// —— 群聊对话引擎护栏（P6-04/D8）：settings.json 顶层 `hub.conversation` ——
// 这些是**所有群共用**的引擎护栏（不是群策略）：调的是「怎么防无限接龙」。
// 毫秒类窗口在界面上按分钟展示（没人想算 600000），写回时 ×60000；
// 每项改动立即落盘 + reload_settings —— 但**已启动的群 loop 读的是启动时的限额**，
// 所以描述里明说「改完需重启 Hub 才影响正在跑的群」。
const CONV_DEFAULTS = {
  maxDepth: 1000,
  pairLimit: 10,
  pairWindowMs: 600_000,
  idleResetMs: 1_800_000,
  cooldownMs: 0,
  runTimeoutMs: 600_000,
  maxPendingWakes: 200,
} as const
const MIN_MS = 60_000

function ConvRow({
  label,
  desc,
  value,
  min,
  max,
  step,
  onChange,
}: {
  label: string
  desc: string
  value: number
  min: number
  max?: number
  step?: number
  onChange: (n: number) => void
}) {
  return (
    <div className="set-row">
      <div>
        <div className="set-label">{label}</div>
        <div className="set-desc">{desc}</div>
      </div>
      <input
        className="text-input"
        type="number"
        min={min}
        max={max}
        step={step ?? 1}
        value={value}
        onChange={(e) => {
          const n = Number(e.target.value)
          if (Number.isFinite(n) && n >= min && (max === undefined || n <= max)) onChange(Math.round(n))
        }}
      />
    </div>
  )
}

function ConversationSection() {
  const t = useT()
  const conv: HubConversationSettings = useAppStore((s) => s.settings.hub?.conversation) ?? {}
  const providerCalls = useAppStore((s) => s.settings.agent?.max_concurrent_provider_calls) ?? 0
  const setConv = useAppStore((s) => s.setHubConversation)
  const toMin = (ms: number | undefined, fallback: number) =>
    Math.max(1, Math.round((ms && ms > 0 ? ms : fallback) / MIN_MS))
  return (
    <>
      <div className="settings-sec-title">{t('settings.conversation')}</div>
      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.conversation')}</div>
          <div className="set-desc">{t('settings.conversation.desc')}</div>
        </div>
      </div>
      <ConvRow
        label={t('settings.conversation.maxDepth')}
        desc={t('settings.conversation.maxDepth.desc')}
        value={conv.max_depth ?? CONV_DEFAULTS.maxDepth}
        min={1}
        max={100_000}
        onChange={(n) => setConv({ max_depth: n })}
      />
      <ConvRow
        label={t('settings.conversation.pairLimit')}
        desc={t('settings.conversation.pairLimit.desc')}
        value={conv.pair_limit ?? CONV_DEFAULTS.pairLimit}
        min={1}
        max={1000}
        onChange={(n) => setConv({ pair_limit: n })}
      />
      <ConvRow
        label={t('settings.conversation.pairWindow')}
        desc={t('settings.conversation.pairWindow.desc')}
        value={toMin(conv.pair_window_ms, CONV_DEFAULTS.pairWindowMs)}
        min={1}
        max={1440}
        onChange={(n) => setConv({ pair_window_ms: n * MIN_MS })}
      />
      <ConvRow
        label={t('settings.conversation.idleReset')}
        desc={t('settings.conversation.idleReset.desc')}
        value={toMin(conv.idle_reset_ms, CONV_DEFAULTS.idleResetMs)}
        min={1}
        max={1440}
        onChange={(n) => setConv({ idle_reset_ms: n * MIN_MS })}
      />
      <ConvRow
        label={t('settings.conversation.cooldown')}
        desc={t('settings.conversation.cooldown.desc')}
        value={conv.cooldown_ms ?? CONV_DEFAULTS.cooldownMs}
        min={0}
        max={600_000}
        step={100}
        onChange={(n) => setConv({ cooldown_ms: n })}
      />
      <ConvRow
        label={t('settings.conversation.runTimeout')}
        desc={t('settings.conversation.runTimeout.desc')}
        value={toMin(conv.run_timeout_ms, CONV_DEFAULTS.runTimeoutMs)}
        min={1}
        max={1440}
        onChange={(n) => setConv({ run_timeout_ms: n * MIN_MS })}
      />
      <ConvRow
        label={t('settings.conversation.maxPending')}
        desc={t('settings.conversation.maxPending.desc')}
        value={conv.max_pending_wakes ?? CONV_DEFAULTS.maxPendingWakes}
        min={1}
        max={10_000}
        onChange={(n) => setConv({ max_pending_wakes: n })}
      />
      {/* provider 并发上限属于 agent 节（进程级），和上面几项一起放：都是「群聊并行投递」的闸。 */}
      <ConvRow
        label={t('settings.conversation.providerCalls')}
        desc={t('settings.conversation.providerCalls.desc')}
        value={providerCalls}
        min={0}
        max={64}
        onChange={(n) => useAppStore.getState().setProviderConcurrency(n)}
      />
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
      if (bridge?.settingsSet) {
        const r = await bridge.settingsSet({ browser_engine: next } as Partial<AppSettings>)
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
      const r = await bridge?.egoInstall()
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
      const r = await bridge?.egoLaunch()
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
              {/* 归属（owner）：IM 的启用/路由按归属隔离（连接共享）—— 面板必须显示
                  「这个开关是给谁的」，否则 A 群启用看起来像 B 群也启用了。 */}
              {p.owner !== undefined && (
                <div className="mcp-s-cmd">
                  {t('plugin.owner.scope')}：<span className="mono">{p.owner || '—'}</span>
                  {p.ownerLabel ? ` · ${p.ownerLabel}` : ''}
                  {typeof p.sharedConnected === 'number' && p.sharedConnected > 0
                    ? ` · ${t('plugin.owner.shared').replace('{n}', String(p.sharedConnected))}`
                    : ''}
                </div>
              )}
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

// —— Project Memory 开关（设置面板 · Agent 组）——
//
// 生效时机 = **下一个会话**（或 switch_model / switch_persona / set_provider 重建
// loop 之后）。运行中的会话不变，这不是缺陷：记忆注入是**系统提示词的一部分**，
// 会话中途删掉会让同一会话前后两轮「你记得什么」不一致，而记忆的价值恰恰建立在
// 跨轮一致上。所以文案必须写明这一点，否则用户会以为开关失灵。
//
// 另两处诚实展示，不粉饰：
//   · env_override —— 环境变量压过面板选择时明确告知，否则用户在这里改了却没生效
//   · explicit=false —— 「默认开着」不是「你的决定」，文案不冒认
function MemoryTab() {
  const t = useT()
  const ms = useAppStore((s) => s.memorySettings)
  const listing = useAppStore((s) => s.memoryListing)
  const browseAvailable = useAppStore((s) => s.memoryBrowseAvailable)
  const browseLoading = useAppStore((s) => s.memoryBrowseLoading)
  const browseError = useAppStore((s) => s.memoryBrowseError)
  const fetchMemorySettings = useAppStore((s) => s.fetchMemorySettings)
  const fetchMemoryBrowsing = useAppStore((s) => s.fetchMemoryBrowsing)
  const setMemoryEnabled = useAppStore((s) => s.setMemoryEnabled)
  const showToast = useAppStore((s) => s.showToast)

  useEffect(() => {
    void fetchMemorySettings()
    void fetchMemoryBrowsing()
  }, [fetchMemorySettings, fetchMemoryBrowsing])

  const on = ms?.enabled ?? true
  // 关掉记忆时不该显示"0 个文件"——那会被读成"记忆被清空了"。
  // 实际语义是"没有记忆目录"，两者的处置完全不同。
  const showListing = on && browseAvailable

  return (
    <>
      <div className="settings-sec-title">{t('settings.memory')}</div>
      <p className="set-desc">{t('settings.memory.desc')}</p>

      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.memory.enable')}</div>
          <div className="set-desc">{t('settings.memory.enable.desc')}</div>
        </div>
        <div className="seg" role="radiogroup" aria-label={t('settings.memory.enable')}>
          <button className={on ? 'cur' : ''} aria-pressed={on} onClick={() => setMemoryEnabled(true)}>{t('settings.on')}</button>
          <button className={!on ? 'cur' : ''} aria-pressed={!on} onClick={() => setMemoryEnabled(false)}>{t('settings.off')}</button>
        </div>
      </div>

      <p className="set-desc" style={{ marginTop: 8 }}>{t('settings.memory.nextSession')}</p>

      {ms?.env_override && (
        <p className="set-desc" style={{ marginTop: 8, color: 'var(--warn, #d08700)' }}>
          {t('settings.memory.envOverride')}
        </p>
      )}
      {!ms?.explicit && (
        <p className="set-desc" style={{ marginTop: 8 }}>{t('settings.memory.defaultOn')}</p>
      )}

      {ms?.memory_dir && (
        <div className="mesh-conn-url" style={{ marginTop: 12 }}>
          <code className="mesh-code">{ms.memory_dir}</code>
          {/* 复制反馈用「已复制」就地替换文案（与 MeshTab 的连接地址同一范式），
              而不是 toast：这段面板已经在讲路径，就地反馈更贴近被复制的对象。 */}
          <button
            className="btn"
            onClick={() => {
              void copyText(ms.memory_dir || '').then((ok) => {
                if (ok) showToast(t('settings.memory.dirCopied'))
              })
            }}
          >
            {t('settings.memory.dirCopy')}
          </button>
        </div>
      )}
      {ms?.memory_dir_error && (
        <p className="set-desc" style={{ marginTop: 8, color: 'var(--danger, #c0392b)' }}>
          {ms.memory_dir_error}
        </p>
      )}

      {/* —— 记忆浏览（官方 §2.7 的 `/memory`：browse and open memory files）——
          纯读，且**刻意不提供删除按钮**：官方承诺的是「随时可以自己编辑或删除」，
          也就是把这件事交给用户直接在文件上做。宿主再画一个删除入口，等于要给
          删除配一套校验与确认，而官方没有这套机制——那是自创。 */}
      <div className="set-row" style={{ marginTop: 16 }}>
        <div>
          <div className="set-label">{t('settings.memory.browse')}</div>
          <div className="set-desc">{t('settings.memory.browse.desc')}</div>
        </div>
        <button className="btn" onClick={() => void fetchMemoryBrowsing()}>
          {t('settings.memory.browse.refresh')}
        </button>
      </div>

      {browseError && (
        // 命令失败 ≠ 记忆未启用：说「未启用」会把用户引去检查开关，而问题在别处。
        <p className="set-desc" style={{ marginTop: 8, color: 'var(--danger, #c0392b)' }}>
          {t('settings.memory.browse.failed')}: {browseError}
        </p>
      )}
      {!on && !browseError && (
        <p className="set-desc" style={{ marginTop: 8 }}>{t('settings.memory.browse.off')}</p>
      )}
      {on && browseLoading && !listing && (
        <p className="set-desc" style={{ marginTop: 8 }}>{t('settings.memory.browse.loading')}</p>
      )}
      {showListing && !listing?.index_present && (
        <p className="set-desc" style={{ marginTop: 8 }}>{t('settings.memory.browse.noIndex')}</p>
      )}
      {showListing && listing && listing.files.length === 0 && (
        <p className="set-desc" style={{ marginTop: 8 }}>{t('settings.memory.browse.empty')}</p>
      )}
      {showListing && listing && listing.files.length > 0 && (
        <table className="mem-table" style={{ marginTop: 8 }}>
          <thead>
            <tr>
              <th>{t('settings.memory.browse')}</th>
              <th>type</th>
              <th>modified</th>
              <th style={{ textAlign: 'right' }}>{t('settings.memory.browse.size')}</th>
            </tr>
          </thead>
          <tbody>
            {listing.files.map((f) => (
              <tr key={f.name} className={f.missing ? 'gone' : undefined}>
                <td>
                  <code>{f.name}</code>
                  {f.missing && <span className="tag-warn"> {t('settings.memory.browse.missing')}</span>}
                  {/* 未登记是正常中间态（模型刚写 topic 还没补索引），标出来而不是
                      藏起来：藏起来会让人以为这条记忆不存在。 */}
                  {!f.indexed && !f.missing && (
                    <span className="tag-dim"> {t('settings.memory.browse.notIndexed')}</span>
                  )}
                </td>
                <td>{f.type || '—'}</td>
                <td>{f.modified || '—'}</td>
                <td style={{ textAlign: 'right' }}>{f.size}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <DreamSection />
    </>
  )
}

// —— Dream（记忆生命周期巡检）——
//
// 这一块回答三个问题：现在开着吗（开关 + 每天几点）、用哪个模型跑（provider/model）、
// 之前几轮干了什么（运行记录）。**巡检本身不由前端驱动**：到点由 bridge 起一个派生
// 会话跑（与 cron 同一条链路），面板只负责读写设置与展示记录。
//
// 交互决策：逐项保存（每次选择直接 dream_set），不设「保存」按钮 —— 这里的每一项都是
// 离散选择而不是自由文本，多一个按钮只会多一个"忘了点"的状态。失败时 bridge 的错误
// 文案落在这里，且草稿回退到磁盘上的值（面板显示的永远是真实生效的设置）。
const pad2 = (n: number) => String(n).padStart(2, '0')

function fmtDreamRunTime(v?: string): string {
  if (!v) return '—'
  const d = new Date(v)
  return isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

function DreamSection() {
  const t = useT()
  const dream = useAppStore((s) => s.dreamSettings)
  const runs = useAppStore((s) => s.dreamRuns)
  const loading = useAppStore((s) => s.dreamLoading)
  const saving = useAppStore((s) => s.dreamSaving)
  const running = useAppStore((s) => s.dreamRunning)
  const err = useAppStore((s) => s.dreamError)
  const fetchDream = useAppStore((s) => s.fetchDream)
  const saveDream = useAppStore((s) => s.saveDream)
  const runDreamNow = useAppStore((s) => s.runDreamNow)
  const appSettings = useAppStore((s) => s.settings)
  // 记忆总开关：关着时巡检无事可做（bridge 的 fireDream 也会拒绝）。
  const memoryOff = useAppStore((s) => s.memorySettings?.enabled) === false
  const presets = useAppStore((s) => s.providerPresets)
  const fetchProviderPresets = useAppStore((s) => s.fetchProviderPresets)

  useEffect(() => { void fetchDream() }, [fetchDream])
  // Provider 显示名来自预设表（同一个 providers.json 事实源）；没拉到就先发一次。
  useEffect(() => { if (presets.length === 0) fetchProviderPresets() }, [presets, fetchProviderPresets])

  // 草稿：面板上当前的选择。bridge 回写（dream_get / dream_set 的响应）之后覆盖 ——
  // 保存失败时这次覆盖就是"回滚"，用户看到的仍是磁盘上真正生效的值。
  const [draft, setDraft] = useState<DreamSettingsInput>({ enabled: false, provider: '', model: '', at: '03:00' })
  useEffect(() => {
    if (!dream || saving) return
    setDraft({ enabled: dream.enabled, provider: dream.provider, model: dream.model, at: dream.at || '03:00' })
  }, [dream, saving])

  // Provider 选项与模型表都取 settings.provider（Composer 模型菜单同一数据源）：
  // 面板能选到的，一定是运行时真能用的那个 provider。
  const labelOf = (id: string) => presets.find((p) => p.id === id)?.name || id
  const modelsOf = (prov: string): string[] => {
    if (!prov) return []
    const cfg = (appSettings.provider as Record<string, unknown>)[prov]
    const own = cfg && typeof cfg === 'object'
      ? Object.keys((cfg as { models?: Record<string, number> }).models || {})
      : []
    if (own.length) return own.sort()
    // 还没拉过模型表的 provider：用预设清单兜底（bridge 侧对空表 fail-open）
    return (presets.find((p) => p.id === prov)?.models ?? []).slice()
  }
  const providerOptions: SelOption[] = [
    { value: '', label: t('settings.dream.providerDefault') },
    ...Object.entries(appSettings.provider)
      .filter(([k, v]) => k !== 'active' && !!v && typeof v === 'object')
      .map(([k]) => ({ value: k, label: labelOf(k) })),
  ]
  const modelOptions: SelOption[] = modelsOf(draft.provider).map((m) => ({ value: m, label: m }))
  const hourOptions: SelOption[] = Array.from({ length: 24 }, (_, i) => ({ value: pad2(i), label: pad2(i) }))
  const minuteOptions: SelOption[] = Array.from({ length: 60 }, (_, i) => ({ value: pad2(i), label: pad2(i) }))
  const [hh, mm] = (draft.at || '03:00').split(':')

  const commit = (patch: Partial<DreamSettingsInput>) => {
    const next: DreamSettingsInput = { ...draft, ...patch }
    setDraft(next)
    saveDream(next)
  }
  // 换 provider 必须连带换 model：bridge 要求两者成对，而模型名带上别的 provider
  // 的模型就是一次注定失败的夜间运行。默认选第一个可用模型。
  const onProvider = (prov: string) => {
    if (!prov) { commit({ provider: '', model: '' }); return }
    const models = modelsOf(prov)
    commit({ provider: prov, model: models[0] ?? '' })
  }

  const now = new Date()
  const today = `${now.getFullYear()}-${pad2(now.getMonth() + 1)}-${pad2(now.getDate())}`
  const ranToday = dream?.last_run_date === today
  const disabled = running || saving

  return (
    <>
      <div className="settings-sec-title" style={{ marginTop: 24 }}>
        {t('settings.dream')}
        {running && <span className="tag-warn"> {t('settings.dream.running')}</span>}
      </div>
      <p className="set-desc">{t('settings.dream.desc')}</p>

      {/* 记忆关着时巡检没有对象（bridge 侧也会拒绝触发）：提前说清楚，
          比让用户点了「立即运行」再收到一句错误更有用。 */}
      {memoryOff && <p className="set-desc" style={{ marginTop: 8, color: 'var(--warn, #d08700)' }}>{t('settings.dream.memoryOff')}</p>}

      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.dream.enable')}</div>
          <div className="set-desc">
            {dream?.enabled
              ? ranToday
                ? t('settings.dream.state.ranToday').replace('{at}', dream.at || '03:00')
                : t('settings.dream.state.willRun').replace('{at}', dream.at || '03:00')
              : t('settings.dream.state.off')}
          </div>
        </div>
        <div className="seg" role="radiogroup" aria-label={t('settings.dream.enable')}>
          <button className={draft.enabled ? 'cur' : ''} aria-pressed={draft.enabled} disabled={disabled} onClick={() => commit({ enabled: true })}>{t('settings.on')}</button>
          <button className={!draft.enabled ? 'cur' : ''} aria-pressed={!draft.enabled} disabled={disabled} onClick={() => commit({ enabled: false })}>{t('settings.off')}</button>
        </div>
      </div>

      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.dream.provider')}</div>
          <div className="set-desc">{t('settings.dream.provider.desc')}</div>
        </div>
        <Sel
          value={draft.provider}
          options={providerOptions}
          onChange={onProvider}
          disabled={disabled}
          style={{ minWidth: 180 }}
        />
      </div>

      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.dream.model')}</div>
          <div className="set-desc">
            {!draft.provider
              ? t('settings.dream.model.followDefault')
              : modelOptions.length === 0
                ? t('settings.dream.model.none')
                : ''}
          </div>
        </div>
        <Sel
          value={draft.model}
          options={modelOptions}
          onChange={(m) => commit({ model: m })}
          disabled={disabled || !draft.provider || modelOptions.length === 0}
          placeholder="—"
          style={{ minWidth: 220 }}
        />
      </div>

      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.dream.at')}</div>
          <div className="set-desc">{t('settings.dream.at.desc')}</div>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
          <Sel
            value={hh}
            options={hourOptions}
            onChange={(h) => commit({ at: `${h}:${mm}` })}
            disabled={disabled}
            style={{ width: 78 }}
          />
          <span className="set-desc">:</span>
          <Sel
            value={mm}
            options={minuteOptions}
            onChange={(m) => commit({ at: `${hh}:${m}` })}
            disabled={disabled}
            style={{ width: 78 }}
          />
        </div>
      </div>

      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.dream.runNow')}</div>
          <div className="set-desc">{t('settings.dream.runNow.desc')}</div>
        </div>
        <button className="btn" disabled={disabled} onClick={() => runDreamNow()}>
          {running ? t('settings.dream.running') : t('settings.dream.runNow.btn')}
        </button>
      </div>

      {err && (
        <p className="set-desc" style={{ marginTop: 8, color: 'var(--danger, #c0392b)' }}>{err}</p>
      )}
      {saving && <p className="set-desc" style={{ marginTop: 8 }}>{t('settings.dream.saving')}</p>}

      <div className="set-row" style={{ marginTop: 16 }}>
        <div>
          <div className="set-label">{t('settings.dream.runs')}</div>
          <div className="set-desc">{t('settings.dream.runs.desc')}</div>
        </div>
        <button className="btn" disabled={loading} onClick={() => void fetchDream()}>
          {loading ? t('settings.memory.browse.loading') : t('settings.memory.browse.refresh')}
        </button>
      </div>

      {runs.length === 0 ? (
        <p className="set-desc" style={{ marginTop: 8 }}>{t('settings.dream.runs.empty')}</p>
      ) : (
        <table className="mem-table" style={{ marginTop: 8 }}>
          <thead>
            <tr>
              <th>{t('settings.dream.runs.time')}</th>
              <th>{t('settings.dream.provider')}</th>
              <th>{t('settings.dream.runs.result')}</th>
            </tr>
          </thead>
          <tbody>
            {runs.map((r) => (
              <tr key={r.session_id || r.started_at}>
                <td>{fmtDreamRunTime(r.started_at)}</td>
                <td>
                  <code>{r.provider || '—'}</code>
                  {r.model ? <span className="tag-dim"> {r.model}</span> : null}
                </td>
                <td>
                  {r.error
                    ? <span className="tag-warn">{r.error}</span>
                    : r.finished_at
                      ? (r.summary || t('settings.dream.runs.noSummary'))
                      : t('settings.dream.runs.unfinished')}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  )
}

// —— 办公组件（LibreOffice kit，R08d）——
//
// 这一块是「用户能不能看懂自己有没有、为什么没有」的唯一落点：状态 + 版本 + 体积 +
// 许可 + 路径 + 一个「重新探测」。安装动作**不在这里**（不自动下载，分发是 PR-13），
// 只给手动的说明与目标目录 —— 让用户能把 kit 放对地方，而不是看着「未安装」没辙。
const OFFICE_PLATFORM_LABEL: Record<string, string> = {
  'darwin-arm64': 'macOS · Apple Silicon',
  'darwin-x64': 'macOS · Intel',
  'windows-x64': 'Windows · x64',
  'linux-x64': 'Linux · x64',
}

function fmtKitSize(n: number): string {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GB`
  if (n >= 1 << 20) return `${Math.round(n / (1 << 20))} MB`
  return `${n} B`
}

function OfficeKitSection() {
  const t = useT()
  const kit = useAppStore((s) => s.officeKit)
  const loading = useAppStore((s) => s.officeKitLoading)
  const fetchOfficeKit = useAppStore((s) => s.fetchOfficeKit)
  const [open, setOpen] = useState(false)
  // 打开设置页时如果还没查过就查一次（App 启动已经查过；这里覆盖「启动时 bridge 还没起」）。
  useEffect(() => {
    if (!kit && !loading) void fetchOfficeKit()
  }, [kit, loading, fetchOfficeKit])
  const stateLabel = !kit
    ? '—'
    : kit.available
      ? t('office.kit.state.ok')
      : kit.state === 'invalid'
        ? t('office.kit.state.invalid')
        : t('office.kit.state.missing')
  const sourceLabel = kit?.source === 'system' ? t('office.kit.source.system') : kit?.source === 'kit' ? t('office.kit.source.kit') : ''
  return (
    <>
      <div className="settings-sec-title">
        {t('office.kit.title')}
        <span className={`okb-badge ${kit?.available ? 'ok' : kit ? 'bad' : ''}`} data-office-kit-state={kit?.state ?? 'unknown'}>{stateLabel}</span>
      </div>
      <div className="set-row">
        <div>
          <div className="set-label">{t('office.kit.subtitle')}</div>
          <div className="set-desc">{t('office.kit.desc')}</div>
          {/* 工具注册是启动期快照（desktop/bridge/office_tools.go）：状态可以热刷新，
              问 agent 要 office_convert/office_render 必须重启 —— 写在这里，别让
              「重新探测」暗示工具也热生效。 */}
          <div className="set-desc">{t('office.kit.restartHint')}</div>
          {kit && (
            <div className="okb-meta">
              <div><span className="okb-k">{t('office.kit.version')}</span><span className="okb-v mono">{kit.version || '—'}{sourceLabel ? `（${sourceLabel}）` : ''}</span></div>
              <div><span className="okb-k">{t('office.kit.size')}</span><span className="okb-v mono">{kit.sizeBytes > 0 ? fmtKitSize(kit.sizeBytes) : '—'}</span></div>
              <div><span className="okb-k">{t('office.kit.license')}</span><span className="okb-v">{kit.license || '—'}</span></div>
              <div><span className="okb-k">{t('office.kit.path')}</span><span className="okb-v mono okb-path" title={kit.bin || kit.root || ''}>{kit.bin || kit.root || '—'}</span></div>
            </div>
          )}
        </div>
        <button className="btn" disabled={loading} onClick={() => void fetchOfficeKit(true)}>
          {loading ? t('office.kit.probing') : t('office.kit.reprobe')}
        </button>
      </div>
      {kit?.reason && <div className="external-skills-feedback error">{kit.reason}</div>}
      {kit?.errors && kit.errors.length > 0 && <div className="external-skills-feedback error okb-errors mono">{kit.errors.join(' · ')}</div>}
      <div className="set-row">
        <div>
          <div className="set-label">{t('office.kit.howto')}</div>
          <div className="set-desc">{t('office.kit.install.manual')}</div>
          {open && kit && (
            <div className="okb-platforms">
              {kit.platforms.map((p) => (
                <div className="okb-platform" key={p.id}>
                  <span className="okb-p-name">{OFFICE_PLATFORM_LABEL[p.id] ?? p.id}</span>
                  <span className="okb-p-supply">{p.supply === 'system' ? t('office.kit.supply.system') : t('office.kit.supply.kit')}</span>
                  <span className="okb-p-size mono">{p.sizeBytes > 0 ? fmtKitSize(p.sizeBytes) : '—'}</span>
                  <span className="okb-p-note">{p.note ?? ''}</span>
                </div>
              ))}
            </div>
          )}
        </div>
        <button className="btn link" onClick={() => setOpen((v) => !v)}>
          {open ? t('office.kit.howto.hide') : t('office.kit.howto.show')}
        </button>
      </div>
    </>
  )
}
