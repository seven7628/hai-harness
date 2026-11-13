// 群协作模式（policy.mode）的前端单一事实源。
//
// 三种模式在 bridge 侧（P6-06 hub_group_mode.go）各有真实行为，UI 文案必须与之一致，
// 否则就是「假开关」：
//   readonly → 非只读工具被拒（只读 + 群通信可用）
//   ask      → 非只读工具逐个弹审批（ToolApprovalRequested → Needs you）
//   auto     → 不加群级审批（仍受 profile/沙箱/全局权限约束）
//
// 新建弹层（GroupEditor）与群头部菜单（GroupView）共用这里的值域与文案；
// 只加/删一处会让两条入口给出不同的模式集合。
export type GroupMode = 'ask' | 'auto' | 'readonly'

/** 值域顺序 = UI 展示顺序（ask 默认且推荐）。 */
export const GROUP_MODES: GroupMode[] = ['ask', 'auto', 'readonly']

/** 归一化任意来源（旧 JSON / 事件 / 手输）的 mode 字符串。 */
export function asGroupMode(raw: unknown): GroupMode {
  return raw === 'auto' || raw === 'readonly' ? raw : 'ask'
}

/** 各模式的行为说明（新建弹层的 hint + 头部菜单的 title）。 */
export const GROUP_MODE_HINT: Record<GroupMode, string> = {
  ask: 'ask：有副作用的动作（bash/写文件/派任务/交接…）先征询你，只读与群内发言直接放行。',
  auto: 'auto：不加群级审批，工具调用直接执行；沙箱与智能体自身的工具策略照常生效。',
  readonly: 'readonly：成员只能读和发言，有副作用的工具会被拒绝（需要时再改成 ask/auto）。',
}

/** 菜单/徽标用的短标签。 */
export const GROUP_MODE_LABEL: Record<GroupMode, string> = {
  ask: 'ask',
  auto: 'auto',
  readonly: 'readonly',
}
