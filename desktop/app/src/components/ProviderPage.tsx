import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { useAppStore, KNOWN_MODEL_MAX_TOKENS, MODEL_MAX_TOKENS_DEFAULT, inputTypesForProvider } from '../store/useAppStore'
import { useT } from '../i18n'
import { getTransport } from '../transport'
import type { ProviderCfg, ProviderPreset } from '../transport/types'
import { ChevronDown, ChevronLeft, EyeIcon, EyeOffIcon, ImageIcon, Plus, SearchIcon } from './icons'
import { ProviderGlyph } from './ProviderGlyph'
import { fmtMoney, localSpendOf, meterOf, resetIn, sortedWindows, type MeterTone } from '../lib/billingView'
import { CATEGORY_ORDER, assertCatalogCoverage, availableEntries, catalogByID, categoryCounts, hostOfURL, searchCatalog, sortConfigured, type CatalogEntry, type CatalogCategory } from '../lib/providerCatalog'

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
    opendesign: 'OpenDesign',
  }
  return fixed[key] ?? key
}
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

// isOAuthPreset 预设是否为 OAuth 订阅厂商（添加表单据此隐藏 API Key、提示登录方式）。
function isOAuthPreset(presets: ProviderPreset[], name: string): boolean {
  return presets.some((p) => p.id === name && Boolean(p.oauth))
}

// 列表页「可用」区预览条数：2 条 ≈ 116px —— 让「已添加」区始终在首屏内（body 高 672px），
// 完整目录在添加页（搜索 + 分类）。
const AVAILABLE_PREVIEW = 2

export default function ProviderPage() {
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
  const renameProviderDisplay = useAppStore((s) => s.renameProviderDisplay)
  // 余额 / 配额（provider_billing；数据面见 provider/billing）
  const billing = useAppStore((s) => s.billing)
  const fetchBilling = useAppStore((s) => s.fetchBilling)
  // 本地记账（今日/本月花费）：从已缓存的 metrics 报告派生，不在 bridge 重扫 JSONL
  const metrics = useAppStore((s) => s.metrics)
  const ensureMetrics = useAppStore((s) => s.ensureMetrics)
  const [labelDrafts, setLabelDrafts] = useState<Record<string, string>>({})
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
  // 三级视图（docs/PROVIDER_MANAGEMENT_REDESIGN.md）：列表 → 添加目录 → 详情。
  // 不用路由：设置弹窗已处理遮罩/Esc/焦点，嵌套一层 history 的收益不抵代价。
  const [view, setView] = useState<'list' | 'add' | 'detail'>('list')
  const [detailID, setDetailID] = useState<string | null>(null)
  const [addEntry, setAddEntry] = useState<CatalogEntry | null>(null)
  const [catalogQ, setCatalogQ] = useState('')
  const [catalogCat, setCatalogCat] = useState<CatalogCategory | 'all'>('all')
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
    // 依赖整个 settings：增删 provider / 改名也触发回填。cfgOf/providerIds 是每帧新建的闭包，
    // 放进依赖会变成「每次渲染都回填」，把用户正在输入的草稿冲掉。
  }, [settings]) // eslint-disable-line react-hooks/exhaustive-deps

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
    setExpanded((m) => ({ ...m, [name]: true }))
    // 添加成功回列表：留在目录页会让人以为「还没加成功」（卡片只是变成灰色「已添加」）。
    setView('list')
    setAddEntry(null)
    fetchBilling(name, false) // 新 provider 立刻拉一次计量（无能力/无 key 会直接返回，不发请求）
    showToast(t('provider.saved'))
  }
  const del = async (pid: string) => {
    await removeProvider(pid)
    setErr(null)
  }
  // 提交显示名：与已存值相同则跳过；失败只提示不打断。空串 = 清除回内置名。
  const commitLabel = async (pid: string) => {
    const draft = labelDrafts[pid]
    if (draft === undefined) return
    const next = draft.trim()
    if (next === ((cfgOf(pid)?.label ?? '').trim())) return
    const r = await renameProviderDisplay(pid, next)
    if (!r.ok) { setErr(r.error ?? t('provider.page.renameErr')); return }
    setLabelDrafts((m) => { const n = { ...m }; delete n[pid]; return n })
    setErr(null)
    showToast(t('provider.saved'))
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

  // ── 目录 / 列表派生（三级视图共用）───────────────────────────────
  const labelOf = (pid: string) => (cfgOf(pid)?.label ?? '').trim() || providerDisplay(t, pid)
  const entryOf = (pid: string): CatalogEntry => {
    const e = catalogByID(pid)
    if (e) return e
    // 用户自建 id（dep/渠道别名如 opencode2）→ 派生一个字标：取首字母 + id 哈希色相
    const letters = pid.replace(/[^A-Za-z0-9]/g, '').slice(0, 2).toUpperCase() || '?'
    let h = 0
    for (let i = 0; i < pid.length; i++) h = (h * 31 + pid.charCodeAt(i)) % 360
    return { id: pid, name: pid, host: '', category: 'custom', aliases: [], glyph: letters, hue: h }
  }
  const counts = categoryCounts()
  // 自定义模板只在目录里提供（列表页的「可用」是发现入口，两个「Custom」行没有意义）
  const available = availableEntries(providerIds).filter((e) => !e.custom)
  const rows = sortConfigured(providerIds.map((pid) => ({
    id: pid,
    active: settings.provider.active === pid,
    hasKey: Boolean(providerKeys[pid]) || (cfgOf(pid)?.auth_type === 'oauth' && Boolean(oauthStatus[pid]?.logged_in)),
    hasMeter: false, // billing 接入后由快照填充（docs/PROVIDER_USAGE_BILLING.md）
    label: labelOf(pid),
  })))
  const presetNames: Record<string, string> = {}
  for (const p of providerPresets) presetNames[p.id] = providerDisplay(t, p.id)

  const openDetail = (pid: string) => {
    setDetailID(pid); setView('detail'); setErr(null); setTestNew(null)
    ensureMetrics() // 本地花费来自缓存的 metrics（60s），不额外扫 JSONL
    if (!billing[pid]) fetchBilling(pid, false)
  }
  const openAdd = (entry: CatalogEntry) => {
    setAddEntry(entry)
    setErr(null)
    setTestNew(null)
    if (entry.custom) {
      startAdd()
      setNewProtocol(entry.custom.protocol)
    } else {
      onPreset(entry.id)
      setAdding(true)
    }
  }
  const leaveAdd = () => { setAdding(false); setAddEntry(null); setErr(null) }
  // 开发态一致性：后端预设里出现、目录里没有 → console.warn（用户仍能用 id 兜底显示）。
  // 只在 DEV 跑：生产环境没有任何补救动作，告警只会变成噪声。
  useEffect(() => {
    if (import.meta.env.DEV && providerPresets.length > 0) assertCatalogCoverage(providerPresets.map((p) => p.id))
  }, [providerPresets])

  // 进 Provider 页拉一次计量（refresh=false → bridge 侧 60s 缓存；切 tab 不重复请求）。
  useEffect(() => { fetchBilling(undefined, false) }, [fetchBilling])

  // Esc 分层：添加页（表单 → 目录 → 列表）/ 详情页 → 列表。
  // 捕获阶段 + stopPropagation：SettingsModal 也在 window 上监听 Esc 关弹窗，
  // 不拦住的话「返回」会直接把整个设置关掉。
  useEffect(() => {
    if (view === 'list') return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.preventDefault()
      e.stopPropagation()
      if (view === 'add' && adding) {
        setAdding(false)
        setAddEntry(null)
        setErr(null)
        return
      }
      setView('list')
      setDetailID(null)
      setErr(null)
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [view, adding])

  const renderRow = (pid: string) => {
    const e = entryOf(pid)
    const cfg = cfgOf(pid)
    const isActive = settings.provider.active === pid
    const hasCred = Boolean(providerKeys[pid]) || (cfg?.auth_type === 'oauth' && Boolean(oauthStatus[pid]?.logged_in))
    const modelCount = Object.keys(cfg?.models ?? {}).length
    return (
      <div
        key={pid}
        className={`pvp-row ${isActive ? 'cur' : ''}`}
        role="button"
        tabIndex={0}
        aria-label={labelOf(pid)}
        onClick={() => openDetail(pid)}
        onKeyDown={(ev) => { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); openDetail(pid) } }}
      >
        <ProviderGlyph entry={e} size={30} />
        <div className="pvp-row-body">
          <div className="pvp-row-1">
            <span className="pvp-row-name">{labelOf(pid)}</span>
            {isActive && <span className="pvp-cur">{t('provider.page.current')}</span>}
            {!hasCred && <span className="pvp-warn">{t('provider.page.noKey')}</span>}
            <span className="pvp-grow" />
            {(() => {
              const m = meterOf(billing[pid])
              // 不把 m.hint（内部标记/错误码）放进 title：那是排障用的，不该出现在悬停提示里
              return (
                <span className={`pvp-meter ${m.text ? 'has' : ''}`}>
                  {typeof m.percent === 'number' && (
                    <span className={`pvp-bar ${m.tone}`}><i style={{ width: `${m.percent > 0 ? Math.max(2, Math.min(100, m.percent)) : 0}%` }} /></span>
                  )}
                  {m.text && <span className={`pvp-meter-txt ${m.tone}`}>{m.text}</span>}
                </span>
              )
            })()}
            {!isActive && (
              <button
                className="btn pvp-mini"
                onClick={(ev) => { ev.stopPropagation(); void saveProvider({ provider: pid, active: pid }) }}
              >
                {t('provider.page.setActive')}
              </button>
            )}
          </div>
          <div className="pvp-row-2">
            <span className="pvp-host">{hostOfURL(cfg?.base_url ?? '') || (cfg?.base_url ?? '—')}</span>
            <span className="pvp-sep">·</span>
            <span>{t('provider.modelCount').replace('{n}', String(modelCount))}</span>
            <span className="pvp-sep">·</span>
            <span>{t(`provider.page.cat.${e.category}`)}</span>
          </div>
        </div>
      </div>
    )
  }

  const renderAvailable = (e: CatalogEntry) => (
    <div className="pvp-row pvp-row-avail" key={e.id}>
      <ProviderGlyph entry={e} size={30} />
      <div className="pvp-row-body">
        <div className="pvp-row-1">
          <span className="pvp-row-name">{presetNames[e.id] ?? e.name}</span>
          {e.oauth && <span className="pvp-chip-sub">{t('provider.page.sub')}</span>}
          <span className="pvp-grow" />
          <button className="btn pvp-mini" onClick={() => openAdd(e)}>
            <Plus size={13} /> {t('provider.page.addOne')}
          </button>
        </div>
        <div className="pvp-row-2">
          <span className="pvp-host">{e.host}</span>
          <span className="pvp-sep">·</span>
          <span>{t(`provider.page.cat.${e.category}`)}</span>
        </div>
      </div>
    </div>
  )

  // renderBilling 详情页的「用量与余额」块。
  // 三类空态在这里必须**分开说**（用同一句话就分不清该做什么）：
  //   未连接（去配 key / 登录） / 不提供查询（换 provider 或忍了） / 出错（重试）。
  const renderBilling = (pid: string) => {
    const snap = billing[pid]
    const local = localSpendOf(metrics?.ByDayModel as { Day: string; Model: string; CostUsd: number }[] | undefined, pid)
    const errText = (code?: string) => {
      switch (code) {
        case 'billing_unauthorized': return t('provider.meter.errUnauthorized')
        case 'billing_rate_limited': return t('provider.meter.errRateLimited')
        case 'billing_endpoint_changed': return t('provider.meter.errChanged')
        case 'billing_network': return t('provider.meter.errNetwork')
        case 'billing_bad_json': return t('provider.meter.errBadJson')
        default: return t('provider.meter.errGeneric')
      }
    }
    const ago = (() => {
      const ts = snap?.fetched_at ? new Date(snap.fetched_at).getTime() : NaN
      if (!Number.isFinite(ts)) return ''
      const min = Math.max(0, Math.round((Date.now() - ts) / 60000))
      if (min < 1) return t('provider.meter.justNow')
      if (min < 60) return t('provider.meter.minutesAgo').replace('{n}', String(min))
      return t('provider.meter.hoursAgo').replace('{n}', String(Math.floor(min / 60)))
    })()

    let body: ReactNode
    if (!snap) {
      body = <div className="pvp-bill-empty">{t('provider.meter.loading')}</div>
    } else if (snap.not_connected || snap.error_code === 'billing_no_key') {
      body = <div className="pvp-bill-empty">{t('provider.meter.notConnected')}</div>
    } else if (snap.error_code === 'billing_unsupported') {
      body = <div className="pvp-bill-empty">{t('provider.meter.unsupported')}</div>
    } else if (snap.error_code && !snap.stale) {
      body = (
        <div className="pvp-bill-err">
          <span>{errText(snap.error_code)}</span>
          <button className="btn pvp-mini" onClick={() => fetchBilling(pid, true)}>{t('provider.meter.retry')}</button>
        </div>
      )
    } else if (snap.kind === 'quota') {
      body = (
        <div className="pvp-bill-rows">
          {sortedWindows(snap).map((w) => {
            const pct = typeof w.percent === 'number' ? Math.max(0, Math.min(100, w.percent)) : null
            const tone: MeterTone = pct === null ? 'muted' : meterOf({ ...snap, windows: [w] }).tone
            const reset = resetIn(w.resets_at)
            return (
              <div className="pvp-bill-row" key={w.key}>
                <span className="pvp-bill-k">{w.label ?? w.key}</span>
                <span className={`pvp-bar wide ${tone}`}>{pct !== null && <i style={{ width: `${pct > 0 ? Math.max(2, pct) : 0}%` }} />}</span>
                <span className={`pvp-bill-v ${tone}`}>{pct !== null ? `${Math.round(pct)}%` : t('provider.meter.noReading')}</span>
                <span className="pvp-bill-reset">{reset ? t('provider.meter.resetsIn').replace('{t}', reset) : ''}</span>
              </div>
            )
          })}
        </div>
      )
    } else if (snap.kind === 'balance') {
      body = (
        <div className="pvp-bill-rows">
          {snap.balance && (
            <div className="pvp-bill-row">
              <span className="pvp-bill-k">{t('provider.meter.balance')}</span>
              <span className={`pvp-bill-money ${snap.balance.amount <= 0 ? 'danger' : ''}`}>{fmtMoney(snap.balance)}</span>
            </div>
          )}
          {snap.used && (
            <div className="pvp-bill-row">
              <span className="pvp-bill-k">{t('provider.meter.used')}</span>
              <span className="pvp-bill-money">{fmtMoney(snap.used)}</span>
            </div>
          )}
          {snap.limit && (
            <div className="pvp-bill-row">
              <span className="pvp-bill-k">{t('provider.meter.limit')}</span>
              <span className="pvp-bill-money">{fmtMoney(snap.limit)}</span>
            </div>
          )}
        </div>
      )
    } else {
      body = <div className="pvp-bill-empty">{t('provider.meter.unsupported')}</div>
    }

    return (
      <div className="pvp-bill">
        <div className="pvp-bill-hd">
          <span className="pvp-bill-title">{t('provider.meter.title')}</span>
          {snap?.experimental && <span className="pvp-chip-sub">{t('provider.meter.experimental')}</span>}
          <span className="pvp-grow" />
          {local.monthUsd > 0 && (
            <span className="pvp-bill-local" title={t('provider.meter.localHint')}>
              {t('provider.meter.month')} <span className="pvp-bill-est">≈</span>{fmtMoney({ amount: local.monthUsd, currency: 'USD' })}
            </span>
          )}
          {ago && <span className="pvp-bill-at">{ago}</span>}
          <button className="btn pvp-mini" onClick={() => fetchBilling(pid, true)}>
            {t('provider.meter.refresh')}
          </button>
        </div>
        {body}
        {snap?.stale && snap.error_code && (
          <div className="pvp-bill-err">
            <span>{errText(snap.error_code)} · {t('provider.meter.stale')}</span>
            <button className="btn pvp-mini" onClick={() => fetchBilling(pid, true)}>{t('provider.meter.retry')}</button>
          </div>
        )}
        {snap?.source && <div className="pvp-bill-src">{t('provider.meter.source')} · {snap.source}</div>}
      </div>
    )
  }

  // ── ① 列表页 ──────────────────────────────────────────────
  if (view === 'list') {
    return (
      <div className="pvp">
        <div className="pvp-head">
          <div>
            <div className="settings-sec-title" style={{ marginBottom: 2 }}>{t('settings.provider')}</div>
            <p className="set-desc" style={{ margin: 0 }}>{t('provider.windowHint')}</p>
          </div>
          <button className="btn primary pvp-add-btn" onClick={() => { setView('add'); setAdding(false); setAddEntry(null); setErr(null) }}>
            <Plus size={14} /> {t('provider.page.add')}
          </button>
        </div>
        {err && <p className="nwf-err" role="alert">{err}</p>}
        <div className="pvp-sec">{t('provider.page.added').replace('{n}', String(rows.length))}</div>
        {rows.length === 0 && <div className="pvp-empty">{t('provider.page.emptyAdded')}</div>}
        {rows.map((r) => renderRow(r.id))}
        {available.length > 0 && (
          <>
            <div className="pvp-sec">{t('provider.page.available').replace('{n}', String(available.length))}</div>
            {/* 可用区只预览 2 条（AVAILABLE_PREVIEW）：全列出来会把「已添加」挤出首屏。
                完整列表在添加页（搜索 + 分类），这里只做「发现入口」。 */}
            {available.slice(0, AVAILABLE_PREVIEW).map(renderAvailable)}
            {available.length > AVAILABLE_PREVIEW && (
              <button className="pvp-browse" onClick={() => { setView('add'); setAdding(false); setAddEntry(null); setErr(null) }}>
                {t('provider.page.browseAll').replace('{n}', String(available.length))}
              </button>
            )}
          </>
        )}
      </div>
    )
  }

  // ── ② 添加页（目录 / 表单）───────────────────────────────────
  if (view === 'add') {
    // 自定义模板置顶：用户最常做的是「加一个中转/兼容端点」，不该滚到 25 条之后。
    const list = [...searchCatalog(catalogQ, { category: catalogCat, displayNames: presetNames })]
      .sort((a, b) => Number(Boolean(b.custom)) - Number(Boolean(a.custom)))
    return (
      <div className="pvp">
        <div className="pvp-nav">
          <button className="pvp-back" onClick={() => (adding ? leaveAdd() : setView('list'))}>
            <ChevronLeft size={14} /> {adding ? t('provider.page.catalog') : t('provider.page.add')}
          </button>
          {adding && addEntry && addEntry.id && (
            <span className="pvp-nav-sub">{addEntry.custom ? t('provider.page.custom') : (presetNames[addEntry.id] ?? addEntry.name)}</span>
          )}
        </div>
        {adding ? (
          <>
            {err && <p className="nwf-err" role="alert">{err}</p>}
          <div className="prov-card">
            <div className="prov-add-hd">
              {/* 标题跟随来源：从目录卡片进来 = 该厂商名；从「自定义」进来 = 自定义模板名。
                  标题写死「新增自定义 Provider」在选预设时会自相矛盾（用户明明选的是 DeepSeek）。 */}
              <span className="prov-name">
                {addEntry?.custom
                  ? t(addEntry.custom.protocol === 'anthropic' ? 'provider.page.customAnthropic' : 'provider.page.customOpenAI')
                  : (addEntry ? (presetNames[addEntry.id] ?? addEntry.name) : t('provider.addTitle'))}
              </span>
              <button className="btn" onClick={() => setAdding(false)}>{t('provider.page.backToCatalog')}</button>
            </div>
            <div className="prov-body">
              {/* 厂商选择已经在目录页完成（搜索/分类比 25 项下拉更可发现）。
                  这里只回显来源 + 一个「换一个」——两套选择器并存会让「我在加哪家」变模糊。 */}
              <div className="set-row">
                <div className="set-label">{t('provider.page.vendor')}</div>
                <div className="prov-keyrow">
                  <span className="pvp-vendor">
                    {addEntry?.custom
                      ? t(addEntry.custom.protocol === 'anthropic' ? 'provider.page.customAnthropic' : 'provider.page.customOpenAI')
                      : addEntry ? (presetNames[addEntry.id] ?? addEntry.name) : t('provider.presetCustom')}
                  </span>
                  <button className="btn" onClick={leaveAdd}>{t('provider.page.changeVendor')}</button>
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
          </>
        ) : (
          <>
            <div className="pvp-search">
              <SearchIcon size={14} />
              <input
                autoFocus
                value={catalogQ}
                onChange={(e) => setCatalogQ(e.target.value)}
                placeholder={t('provider.page.searchPh')}
                aria-label={t('provider.page.searchPh')}
                spellCheck={false}
              />
              <span className="pvp-count" aria-live="polite">{list.length}</span>
            </div>
            <div className="pvp-catalog">
              <div className="pvp-cats" role="tablist" aria-label={t('provider.page.add')}>
                {CATEGORY_ORDER.map((c) => (
                  <button
                    key={c.key}
                    role="tab"
                    aria-selected={catalogCat === c.key}
                    className={`pvp-cat ${catalogCat === c.key ? 'cur' : ''}`}
                    onClick={() => setCatalogCat(c.key)}
                  >
                    <span>{t(c.labelKey)}</span>
                    <span className="pvp-cat-n">{counts[c.key] ?? 0}</span>
                  </button>
                ))}
              </div>
              <div className="pvp-grid">
                {list.map((e) => {
                  const added = providerIds.includes(e.id)
                  return (
                    <button
                      key={e.id}
                      className={`pvp-card ${added ? 'is-added' : ''}`}
                      disabled={added}
                      onClick={() => openAdd(e)}
                    >
                      <ProviderGlyph entry={e} size={32} />
                      <span className="pvp-card-main">
                        <span className="pvp-card-name">{presetNames[e.id] ?? e.name}</span>
                        <span className="pvp-card-sub">{e.host}</span>
                      </span>
                      {added
                        ? <span className="pvp-card-added">{t('provider.page.alreadyAdded')}</span>
                        : e.oauth
                          ? <span className="pvp-chip-sub">{t('provider.page.sub')}</span>
                          : <span className="pvp-card-plus"><Plus size={14} /></span>}
                    </button>
                  )
                })}
                {list.length === 0 && <div className="pvp-empty pvp-empty-grid">{t('provider.page.noResult')}</div>}
              </div>
            </div>
          </>
        )}
      </div>
    )
  }

  // ── ③ 详情页（复用既有编辑器：只渲染选中那一条，强制展开）────────
  return (
    <div className="pvp pvp-detail">
      <div className="pvp-nav">
        <button className="pvp-back" onClick={() => { setView('list'); setDetailID(null); setErr(null) }}>
          <ChevronLeft size={14} /> {t('settings.provider')}
        </button>
        {detailID && (
          <span className="pvp-nav-right">
            {settings.provider.active !== detailID && (
              <button className="btn pvp-mini" onClick={() => void saveProvider({ provider: detailID, active: detailID })}>
                {t('provider.page.setActive')}
              </button>
            )}
            <button className="btn pvp-mini pvp-del" onClick={() => { void del(detailID); setView('list'); setDetailID(null) }}>
              {t('provider.remove')}
            </button>
          </span>
        )}
      </div>
      {err && <p className="nwf-err" role="alert">{err}</p>}
      {detailID && (
        <div className="pvp-detail-title">
          <ProviderGlyph entry={entryOf(detailID)} size={30} />
          <div className="pvp-detail-id">
            <div className="pvp-row-name">{labelOf(detailID)}</div>
            <div className="pvp-row-2">
              <span className="pvp-host">{hostOfURL(cfgOf(detailID)?.base_url ?? '') || '—'}</span>
              <span className="pvp-sep">·</span>
              <span className="pvp-id">{detailID}</span>
            </div>
          </div>
        </div>
      )}
      {detailID && renderBilling(detailID)}

      {/* 只渲染选中的那一条：详情页是「一个 provider 的编辑器」，不是整表展开。 */}
      {providerIds.filter((pid) => pid === detailID).map((pid) => {
        const pc = cfgOf(pid)
        const isActive = settings.provider.active === pid
        const hasKey = providerKeys[pid]
        const modelNames = Object.keys(pc?.models ?? {})
        const open = true // 详情页只渲染选中那一条，恒展开（折叠交互在列表页）
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
                {/* 显示名（2026-11）：只写 label，不动 settings 的键（id 不可变）——
                    历史 metrics / 价表 / billing 全按 id 记账，改 id 会把历史断成两个 provider。 */}
                <div className="set-row">
                  <div className="set-label">{t('provider.page.displayName')}</div>
                  <div className="prov-keyrow">
                    <input
                      className="prov-input"
                      value={labelDrafts[pid] ?? (pc?.label ?? '')}
                      placeholder={providerDisplay(t, pid)}
                      onChange={(e) => setLabelDrafts((m) => ({ ...m, [pid]: e.target.value }))}
                      onBlur={() => void commitLabel(pid)}
                      onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); void commitLabel(pid) } }}
                      aria-label={t('provider.page.displayName')}
                      spellCheck={false}
                      maxLength={64}
                    />
                    <span className="pvp-id-hint">{t('provider.page.idHint')} · {pid}</span>
                  </div>
                </div>
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
    </div>
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

