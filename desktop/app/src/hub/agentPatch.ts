import type { HubAgent } from './types'

// 资料页的「表单 → 命令 payload」纯函数。
//
// 抽出来的原因（P1-08/09 复核 B1）：保存时用整份 profile 当 patch，会把界面上没
// 展示的字段写零值 —— 最要命的是 tools.disable（撤下的工具被静默重新启用）。
// 这里只产出「真正改动过的字段」，纯函数便于 node 回归（scripts/test-agent-patch.mjs）。

export interface AgentForm {
  name: string
  emoji: string
  accent: string
  role: string
  description: string
  instructions: string
  provider: string
  model: string
  effort: string
  threshold: number
  dailyBudget: number
  taskBudget: number
  memoryOn: boolean
  preset: string
  disable: string
}

/**
 * 编辑器「日预算」初始值：新建默认 20；已有智能体缺 policy = 未设置（0 = 不限）。
 * 不能沿用 20：agentPatch 会把它和 `policy?.daily_budget_usd ?? 0` 比较，
 * 于是只改名字也会写出 policy，把「不限」悄悄变成 $20（复审 task-7rscybyy）。
 */
export function initialDailyBudget(agent?: HubAgent | null): number {
  return agent ? (agent.policy?.daily_budget_usd ?? 0) : 20
}

/** 撤下工具：空格/逗号分隔，去空、去重。 */
export function normalizeDisable(text: string): string[] {
  const seen = new Set<string>()
  for (const raw of text.split(/[\s,]+/)) {
    const v = raw.trim()
    if (v) seen.add(v)
  }
  return [...seen]
}

function same(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b)
}

/** 新建时的完整 profile（服务端会分配 id/version）。 */
export function buildAgentProfile(form: AgentForm): Record<string, unknown> {
  return {
    name: form.name.trim(),
    avatar: { kind: 'emoji', value: form.emoji },
    accent: form.accent,
    role: form.role.trim(),
    description: form.description.trim(),
    instructions: form.instructions,
    provider: form.provider.trim(),
    model: form.model.trim(),
    effort: form.effort,
    compress_threshold: form.threshold,
    policy: { daily_budget_usd: form.dailyBudget, max_task_cost_usd: form.taskBudget },
    memory: { enabled: form.memoryOn },
    tools: { preset: form.preset, disable: normalizeDisable(form.disable) },
  }
}

/**
 * 编辑时的 patch：只包含与 agent 当前值不同的字段。
 * agent 为空（理论上不会发生）时退化为整份 profile。
 */
export function buildAgentPatch(form: AgentForm, agent?: HubAgent | null): Record<string, unknown> {
  if (!agent) return buildAgentProfile(form)
  const patch: Record<string, unknown> = {}
  const setIfChanged = (key: string, next: unknown, prev: unknown): void => {
    if (!same(next, prev)) patch[key] = next
  }
  setIfChanged('name', form.name.trim(), agent.name ?? '')
  setIfChanged('role', form.role.trim(), agent.role ?? '')
  setIfChanged('description', form.description.trim(), agent.description ?? '')
  setIfChanged('instructions', form.instructions, agent.instructions ?? '')
  setIfChanged('provider', form.provider.trim(), agent.provider ?? '')
  setIfChanged('model', form.model.trim(), agent.model ?? '')
  setIfChanged('effort', form.effort, agent.effort ?? '')
  setIfChanged('compress_threshold', form.threshold, agent.compress_threshold ?? 0.8)
  // policy 是深合并：只发改动字段，max_auto_turns/workspace_roots 原样保留。
  const prevPolicy = agent.policy ?? {}
  const nextPolicy = {
    ...prevPolicy,
    daily_budget_usd: form.dailyBudget,
    max_task_cost_usd: form.taskBudget,
  }
  if (form.dailyBudget !== (prevPolicy.daily_budget_usd ?? 0) ||
      form.taskBudget !== (prevPolicy.max_task_cost_usd ?? 0)) {
    patch.policy = nextPolicy
  }
  setIfChanged('memory', { enabled: form.memoryOn }, { enabled: agent.memory?.enabled !== false })
  // 图片头像没有上传 UI：不碰 kind=image 的智能体，避免保存把头像降级成 emoji（N7）。
  if (agent.avatar?.kind !== 'image') {
    setIfChanged(
      'avatar',
      { kind: 'emoji', value: form.emoji },
      { kind: agent.avatar?.kind ?? 'emoji', value: agent.avatar?.value ?? '' },
    )
  }
  setIfChanged('accent', form.accent, agent.accent ?? '')
  const disable = normalizeDisable(form.disable)
  if (form.preset !== (agent.tools?.preset ?? 'all') || !same(disable, agent.tools?.disable ?? [])) {
    // tools 是深合并：带上原对象，未展示的 allow/mcp/bash_roots 原样保留。
    patch.tools = { ...(agent.tools ?? {}), preset: form.preset, disable }
  }
  return patch
}
