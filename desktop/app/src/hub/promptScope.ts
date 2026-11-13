// 提示词预览的作用域（DM / 群）：缓存键与竞态判定的唯一口径。
//
// 为什么单独抽出来：hub_agent_prompt_preview 的 L1 协议与 L2 名册**按作用域分叉**
// （群变体有 @ 与 assign_task，DM 变体没有；后端 payload 里可选带 group_id）。
// 前端若只按 agentId 缓存/比对，症状就是「在群里看到的是 DM 变体，而且切群、切
// DM 都不会重新请求」——错的是内容，成因在缓存键。所以键必须是 (agent, 群)。
//
// 约定：groupId 空串 = DM 作用域，与后端「没带 group_id 就是 DM 变体」同义；
// 调用方不必区分 undefined / null / 空白串。Node 直测：scripts/test-hub-context.mjs。

/** 一次提示词预览的作用域。 */
export interface PromptScope {
  agentId: string
  /** 群 id；空串 = DM（后端的 group_id 缺省语义）。 */
  groupId: string
}

/** 作用域归一化：id 去空白，groupID 缺省/空白一律收敛成 DM 的空串。 */
export function normalizePromptScope(
  agentID: string | null | undefined,
  groupID?: string | null,
): PromptScope {
  return {
    agentId: String(agentID ?? '').trim(),
    groupId: String(groupID ?? '').trim(),
  }
}

/**
 * 缓存键：`agentId\0groupId`。
 *
 * 用 NUL 分隔而不是 ':'/'/'：agent 与群 id 都可能出现那些分隔符（群 id 允许自定义），
 * 拼起来会撞键（`a:b` + `c` 与 `a` + `b:c`）；NUL 不会出现在 id 里。
 * 没有 agentId（未选目标）时返回空串 —— 空键永远不命中，不会拿别人的预览充数。
 */
export function promptScopeKey(scope: Partial<PromptScope> | null | undefined): string {
  const agentId = String(scope?.agentId ?? '').trim()
  if (!agentId) return ''
  return `${agentId}\u0000${String(scope?.groupId ?? '').trim()}`
}

/**
 * 当前 state 是不是这次请求的作用域（同 agent + 同群）。
 *
 * 两处用：① 复用缓存/在飞请求前判定；② 响应回来后比对（迟到的群 A 响应不许写进
 * 群 B 的面板）。state 里缺 groupId 的老形状按 DM 处理（与后端缺省语义一致）。
 */
export function promptScopeMatches(
  state: Partial<PromptScope> | null | undefined,
  scope: Partial<PromptScope> | null | undefined,
): boolean {
  const key = promptScopeKey(scope)
  return key !== '' && promptScopeKey(state) === key
}
