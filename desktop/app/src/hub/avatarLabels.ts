import { planStack, type AvatarState, type StackInputMember, type StateCounts } from './avatar'

// 头像的无障碍文案（只在渲染层用，i18n key 见 src/i18n.ts）。
// 纯字符串拼装、不 import React：Node 测试可以单独覆盖，也避免组件里硬编码中文。

export type Translate = (key: string) => string

const STATE_KEY: Record<AvatarState, string> = {
  idle: 'hub.avatar.state.idle',
  working: 'hub.avatar.state.working',
  needs_attention: 'hub.avatar.state.needs_attention',
  offline: 'hub.avatar.state.offline',
}

export function avatarStateLabel(t: Translate, state: AvatarState): string {
  return t(STATE_KEY[state])
}

/** 独立头像的标签：「Researcher，工作中」。 */
export function agentSigilLabel(t: Translate, name: string, state: AvatarState): string {
  return t('hub.avatar.label')
    .replace('{name}', name)
    .replace('{state}', avatarStateLabel(t, state))
}

/**
 * 群叠放的状态摘要：「2 个工作中，1 个需要你」。
 * 只报非零项；全空闲时用「全部待命」，避免「0 个工作中，0 个需要你」的噪音。
 */
export function agentStackSummary(t: Translate, counts: StateCounts, state?: AvatarState): string {
  const parts: string[] = []
  if (counts.working > 0) parts.push(t('hub.avatar.stack.summary.working').replace('{n}', String(counts.working)))
  if (counts.needs_attention > 0) {
    parts.push(t('hub.avatar.stack.summary.needs_attention').replace('{n}', String(counts.needs_attention)))
  }
  if (counts.offline > 0) parts.push(t('hub.avatar.stack.summary.offline').replace('{n}', String(counts.offline)))
  if (parts.length > 0) return parts.join(t('hub.avatar.stack.sep'))
  // 成员都在待命、但群会话在跑：lead 显示 working，摘要也要跟着说真话。
  return state === 'working' ? t('hub.avatar.stack.summary.groupRunning') : t('hub.avatar.stack.summary.idle')
}

/** 群叠放的标签：「5 个智能体，2 个工作中，1 个需要你」。 */
export function agentStackLabel(t: Translate, total: number, counts: StateCounts, state?: AvatarState): string {
  return t('hub.avatar.stack.label')
    .replace('{n}', String(total))
    .replace('{summary}', agentStackSummary(t, counts, state))
}

/** 一步到位：叠放成员 → aria-label（与 AgentStack 内部的 planStack 同口径）。 */
export function stackAriaLabel(
  t: Translate,
  members: readonly StackInputMember[],
  opts?: { groupRunning?: boolean },
): string {
  const plan = planStack(members, opts)
  return agentStackLabel(t, plan.total, plan.counts, plan.state)
}
