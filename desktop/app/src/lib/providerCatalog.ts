// Provider 目录（展示元数据）：分类 / 别名 / 搜索 / 显示名。
//
// 为什么单独一张表，而不是塞进 providers.json：
//   providers.json 是**模型元数据 + 预设**的单一事实源，由 scripts/gen_providers_json.py
//   生成（脚本不入库、--check 逐字节幂等）。而分类/主页/别名是**纯展示**信息，改一次分类
//   不该触发一次 JSON 重生成。两者变更频率与责任人都不同，故分开。
//   代价：新增 provider 要改两处 → 本文件末尾的 assertCatalogCoverage 在开发态兜住。
//
// 与后端的关系：catalog 只决定「添加页里有什么、怎么分组、怎么搜」；能不能用、默认 base_url
// 是多少，仍由 bridge list_provider_presets（providers.json）说了算。

export type CatalogCategory = 'sub' | 'vendor' | 'platform' | 'aggregator' | 'custom'

export interface CatalogEntry {
  id: string // 预设 id（与 providers.json 的 preset.id 一致）；自定义模板用 'custom:openai' / 'custom:anthropic'
  name: string // 兜底显示名（预设条目会被 i18n / providerDisplay 覆盖）
  host: string // 展示用 host（不含协议）
  category: CatalogCategory
  aliases: string[] // 搜索别名：glm→zhipu、grok→xai、moonshot→kimi …
  oauth?: boolean // OAuth 订阅登录（添加后走登录，不填 key）
  glyph: string // 字标（1~2 字符）
  hue: number // 字标色相（0~360）；同一厂商系用相近色相
  custom?: {
    // 自定义模板：点开后直接进表单，协议预置
    protocol: 'chat_completions' | 'responses' | 'anthropic'
  }
}

// 目录条目：24 条预设 + 2 条自定义模板。
// 分类口径（与 cc-switch 的 5 类对齐，但只保留我们真有的）：
//   sub        登录订阅（OAuth 订阅制：Claude Pro/Max、Codex、Copilot、Kimi Coding、Grok）
//   vendor     模型厂商官方 API
//   platform   第三方推理平台（按量）
//   aggregator 聚合/中转网关（一家多模型）
//   custom     自定义兼容端点
export const CATALOG: CatalogEntry[] = [
  // —— 登录订阅 ——
  { id: 'anthropic', name: 'Anthropic', host: 'api.anthropic.com', category: 'sub', oauth: true, aliases: ['claude', 'claude code', 'sonnet', 'opus'], glyph: 'A', hue: 24 },
  { id: 'openai-codex', name: 'OpenAI Codex', host: 'chatgpt.com', category: 'sub', oauth: true, aliases: ['codex', 'chatgpt', 'gpt'], glyph: 'C', hue: 150 },
  { id: 'github-copilot', name: 'GitHub Copilot', host: 'api.githubcopilot.com', category: 'sub', oauth: true, aliases: ['copilot', 'github'], glyph: 'GH', hue: 210 },
  { id: 'kimi-coding', name: 'Kimi Coding', host: 'api.kimi.com', category: 'sub', oauth: true, aliases: ['kimi', 'moonshot', 'k2'], glyph: 'K', hue: 250 },
  { id: 'xai', name: 'xAI (Grok)', host: 'api.x.ai', category: 'sub', oauth: true, aliases: ['grok', 'x'], glyph: 'X', hue: 0 },

  // —— 模型厂商 ——
  { id: 'deepseek', name: 'DeepSeek', host: 'api.deepseek.com', category: 'vendor', aliases: ['ds', '深度求索', 'v4'], glyph: 'DS', hue: 212 },
  { id: 'openai', name: 'OpenAI', host: 'api.openai.com', category: 'vendor', aliases: ['gpt', 'o3', 'chatgpt'], glyph: 'AI', hue: 160 },
  { id: 'kimi', name: 'Kimi (Moonshot)', host: 'api.moonshot.cn', category: 'vendor', aliases: ['moonshot', '月之暗面', 'k2'], glyph: 'K', hue: 250 },
  { id: 'zhipu', name: 'Zhipu GLM', host: 'open.bigmodel.cn', category: 'vendor', aliases: ['glm', '智谱', 'zai', 'bigmodel'], glyph: 'Z', hue: 196 },
  { id: 'qwen', name: 'Qwen', host: 'maas.aliyuncs.com', category: 'vendor', aliases: ['通义', '千问', 'dashscope', 'aliyun', '阿里'], glyph: 'Q', hue: 268 },
  { id: 'xiaomi', name: 'MiMo', host: 'api.xiaomimimo.com', category: 'vendor', aliases: ['小米', 'mimo'], glyph: 'MI', hue: 18 },
  { id: 'minimax', name: 'MiniMax', host: 'api.minimax.io', category: 'vendor', aliases: ['abab', '海螺'], glyph: 'MM', hue: 340 },
  { id: 'mistral', name: 'Mistral', host: 'api.mistral.ai', category: 'vendor', aliases: ['mixtral', 'codestral'], glyph: 'M', hue: 32 },

  // —— 第三方推理平台 ——
  { id: 'groq', name: 'Groq', host: 'api.groq.com', category: 'platform', aliases: ['lpU', 'fast'], glyph: 'G', hue: 12 },
  { id: 'together', name: 'Together', host: 'api.together.ai', category: 'platform', aliases: ['together ai'], glyph: 'T', hue: 220 },
  { id: 'fireworks', name: 'Fireworks', host: 'api.fireworks.ai', category: 'platform', aliases: ['fw'], glyph: 'F', hue: 300 },
  { id: 'cerebras', name: 'Cerebras', host: 'api.cerebras.ai', category: 'platform', aliases: ['wafer'], glyph: 'C', hue: 190 },
  { id: 'baseten', name: 'Baseten', host: 'inference.baseten.co', category: 'platform', aliases: ['bt'], glyph: 'B', hue: 96 },
  { id: 'nvidia', name: 'NVIDIA NIM', host: 'integrate.api.nvidia.com', category: 'platform', aliases: ['nim', '英伟达'], glyph: 'N', hue: 108 },
  { id: 'huggingface', name: 'Hugging Face', host: 'router.huggingface.co', category: 'platform', aliases: ['hf', 'router'], glyph: 'HF', hue: 44 },

  // —— 聚合 / 中转 ——
  { id: 'opencode', name: 'OpenCode Zen', host: 'opencode.ai', category: 'aggregator', aliases: ['zen', 'opencode go', 'oc'], glyph: 'OC', hue: 168 },
  { id: 'openrouter', name: 'OpenRouter', host: 'openrouter.ai', category: 'aggregator', oauth: true, aliases: ['or', 'router'], glyph: 'OR', hue: 264 },
  { id: 'opendesign', name: 'OpenDesign', host: 'amr-link.open-design.ai', category: 'aggregator', aliases: ['od', 'amr'], glyph: 'OD', hue: 286 },

  // —— 自定义（固定在「全部」顶部）——
  {
    id: 'custom:openai', name: 'Custom (OpenAI compatible)', host: '任意 /v1 端点', category: 'custom',
    aliases: ['custom', 'openai', 'compatible', '自定义', '中转', 'oneapi', 'newapi'],
    glyph: '+', hue: 168, custom: { protocol: 'chat_completions' },
  },
  {
    id: 'custom:anthropic', name: 'Custom (Anthropic compatible)', host: '任意 Anthropic 端点', category: 'custom',
    aliases: ['custom', 'anthropic', 'claude', 'compatible', '自定义'],
    glyph: '+', hue: 24, custom: { protocol: 'anthropic' },
  },
]

export const CATEGORY_ORDER: { key: CatalogCategory | 'all'; labelKey: string }[] = [
  { key: 'all', labelKey: 'provider.page.cat.all' },
  { key: 'sub', labelKey: 'provider.page.cat.sub' },
  { key: 'vendor', labelKey: 'provider.page.cat.vendor' },
  { key: 'platform', labelKey: 'provider.page.cat.platform' },
  { key: 'aggregator', labelKey: 'provider.page.cat.aggregator' },
  { key: 'custom', labelKey: 'provider.page.cat.custom' },
]

// catalogByID 按 id 查目录条目（预设 id 或自定义模板 id）。
export function catalogByID(id: string): CatalogEntry | undefined {
  return CATALOG.find((e) => e.id === id)
}

// normalize 搜索归一：小写 + 去空白（中文别名原样保留）。
function normalize(s: string): string {
  return s.trim().toLowerCase()
}

// searchCatalog 名称 / id / host / 别名 子串匹配（空查询 = 全部）。
// displayNames 允许调用方注入 i18n 显示名（如「智谱 Zhipu」），使中文界面下搜「智谱」也能命中。
export function searchCatalog(
  query: string,
  opts: { category?: CatalogCategory | 'all'; displayNames?: Record<string, string> } = {},
): CatalogEntry[] {
  const q = normalize(query)
  const cat = opts.category ?? 'all'
  return CATALOG.filter((e) => {
    if (cat !== 'all' && e.category !== cat) return false
    if (!q) return true
    const hay = [e.id, e.name, e.host, ...e.aliases, opts.displayNames?.[e.id] ?? ''].join(' ').toLowerCase()
    return hay.includes(q)
  })
}

// categoryCounts 每个分类的条目数（'all' = 全部）；用于分类栏的计数徽标。
export function categoryCounts(entries: CatalogEntry[] = CATALOG): Record<string, number> {
  const out: Record<string, number> = { all: entries.length }
  for (const e of entries) out[e.category] = (out[e.category] ?? 0) + 1
  return out
}

// availableEntries 目录中「尚未添加」的条目。
// 判定只按 id：用户把同一家加成两个不同 id（opencode2/opencode3）是合法用法，
// 不该因为「同端点已存在」而把目录项判成已添加。
export function availableEntries(configuredIDs: string[]): CatalogEntry[] {
  const set = new Set(configuredIDs)
  return CATALOG.filter((e) => !set.has(e.id))
}

// hostOfURL base_url → host（去协议/路径/端口小写）；解析失败返回 ''。
export function hostOfURL(raw: string): string {
  const s = (raw ?? '').trim()
  if (!s) return ''
  try {
    return new URL(s.includes('://') ? s : `https://${s}`).hostname.toLowerCase()
  } catch {
    return ''
  }
}

// assertCatalogCoverage 开发态一致性校验：目录里的预设 id 必须存在于后端预设表
// （providers.json）。缺失只告警不抛——后端可能先于前端升级，UI 用 id 兜底展示即可。
export function assertCatalogCoverage(presetIDs: string[]): string[] {
  if (presetIDs.length === 0) return []
  const set = new Set(presetIDs)
  const missing = CATALOG.filter((e) => !e.id.startsWith('custom:') && !set.has(e.id)).map((e) => e.id)
  if (missing.length > 0 && typeof console !== 'undefined') {
    console.warn('[providerCatalog] 目录条目在后端预设里不存在：', missing.join(', '))
  }
  return missing
}

// sortConfigured 已添加条目的默认排序：当前 → 有计量数据 → 有凭证 → 名称。
// 理由：用户最常看的两件事是「正在用哪个」和「哪个快没额度了」，都该在最上面。
export function sortConfigured<T extends { id: string; active: boolean; hasKey: boolean; hasMeter?: boolean; label: string }>(rows: T[]): T[] {
  return [...rows].sort((a, b) => {
    if (a.active !== b.active) return a.active ? -1 : 1
    if (Boolean(a.hasMeter) !== Boolean(b.hasMeter)) return a.hasMeter ? -1 : 1
    if (a.hasKey !== b.hasKey) return a.hasKey ? -1 : 1
    return a.label.localeCompare(b.label)
  })
}
