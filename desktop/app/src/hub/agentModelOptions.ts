// 资料页 Provider / Model / Effort 的选项推导（纯函数）。
//
// 为什么单独一层：表单只关心「哪些值可选、当前值还在不在列表里」。把判定从 TSX
// 拿出来，一是未知 provider/model 不会被静默改写（用户仍能看到当前值），二是
// scripts/test-agent-model-options.mjs 能直接锁住过滤/并集/排序/默认值规则。

/** settings.provider 是自由 JSON：条目缺失或畸形都不能让下拉炸掉。 */
export type ProviderSettingsLike = object | null | undefined
export type FetchedModelsLike = object | null | undefined

export interface ProviderOption {
  id: string
  label: string
}

export interface AgentModelDefaults {
  provider: string
  model: string
}

interface ProviderCfgLike {
  label?: unknown
  models?: unknown
  deleted?: unknown
}

function asRecord(v: unknown): Record<string, unknown> | null {
  return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null
}

function cfgOf(settings: ProviderSettingsLike, id: string): ProviderCfgLike | null {
  const rec = asRecord(settings)
  if (!rec) return null
  const cfg = asRecord(rec[id])
  return cfg ? (cfg as ProviderCfgLike) : null
}

/**
 * Provider 下拉选项：排除 `active` 保留键、`deleted: true` 与非对象条目。
 * 顺序沿用 settings（用户配置顺序），label 缺省回落到 provider id。
 */
export function providerOptions(settings: ProviderSettingsLike): ProviderOption[] {
  const rec = asRecord(settings)
  if (!rec) return []
  const out: ProviderOption[] = []
  for (const id of Object.keys(rec)) {
    if (id === 'active') continue
    const cfg = cfgOf(settings, id)
    if (!cfg || cfg.deleted === true) continue
    const label = typeof cfg.label === 'string' && cfg.label.trim() ? cfg.label : id
    out.push({ id, label })
  }
  return out
}

/**
 * 某 Provider 的可选模型 = `cfg.models` 的键 ∪ `fetchedModels[provider]`。
 * 去重后按名字排序；models 缺失/非对象（字符串、数组等畸形值）当作空列表。
 */
export function modelsForProvider(
  settings: ProviderSettingsLike,
  fetchedModels: FetchedModelsLike,
  provider: string,
): string[] {
  const set = new Set<string>()
  if (provider) {
    const models = cfgOf(settings, provider)?.models
    if (models && typeof models === 'object' && !Array.isArray(models)) {
      for (const name of Object.keys(models)) if (name.trim()) set.add(name)
    }
    const pulled = asRecord(fetchedModels)?.[provider]
    if (Array.isArray(pulled)) {
      for (const name of pulled) if (typeof name === 'string' && name.trim()) set.add(name)
    }
  }
  return [...set].sort()
}

/**
 * 切换 Provider 后的模型：当前模型仍在新列表就保留，否则选第一个可用模型；
 * 新 Provider 没有模型 → 清空（不要让旧模型挂在新 Provider 名下）。
 */
export function chooseModelOnProviderSwitch(currentModel: string, models: readonly string[]): string {
  if (currentModel && models.includes(currentModel)) return currentModel
  return models[0] ?? ''
}

/**
 * 当前值不在候选列表时原样返回（UI 据此渲染一条 disabled「当前值」选项）；
 * 空值或已在列表里返回 null。
 */
export function unlistedValue(value: string, options: readonly string[]): string | null {
  return value && !options.includes(value) ? value : null
}

/** 新智能体的默认 Provider/Model：active → settings 第一个可用 Provider。 */
export function newAgentDefaults(
  settings: ProviderSettingsLike,
  fetchedModels: FetchedModelsLike,
  globalModel: string,
): AgentModelDefaults {
  const options = providerOptions(settings)
  const active = asRecord(settings)?.active
  const provider = typeof active === 'string' && options.some((o) => o.id === active)
    ? active
    : (options[0]?.id ?? '')
  if (!provider) return { provider: '', model: '' }
  const models = modelsForProvider(settings, fetchedModels, provider)
  return { provider, model: models.includes(globalModel) ? globalModel : (models[0] ?? '') }
}

// 产品口径的 8 个档位：空值（跟随模型默认）+ 7 个真实 reasoning effort。
// 值必须保持后端既有小写，标签只是 UI 的 Title Case。
export const EFFORT_VALUES = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'] as const
export type EffortValue = (typeof EFFORT_VALUES)[number]

const EFFORT_LABELS: Record<EffortValue, string> = {
  none: 'None',
  minimal: 'Minimal',
  low: 'Low',
  medium: 'Medium',
  high: 'High',
  xhigh: 'XHigh',
  max: 'Max',
}

export interface EffortChoice {
  value: string
  label: string
}

/** 下拉选项：第一项是 `''`（标签由调用方按当前语言给），随后 7 个档位。 */
export function effortChoices(): EffortChoice[] {
  return [
    { value: '', label: '' },
    ...EFFORT_VALUES.map((value) => ({ value, label: EFFORT_LABELS[value] })),
  ]
}

/** 展示标签：'' → ''（调用方补 i18n），已知档位 → Title Case，未知值原样保留。 */
export function effortLabel(value: string | null | undefined): string {
  const raw = value ?? ''
  if (!raw) return ''
  const lowered = raw.toLowerCase()
  return (EFFORT_VALUES as readonly string[]).includes(lowered)
    ? EFFORT_LABELS[lowered as EffortValue]
    : raw
}
