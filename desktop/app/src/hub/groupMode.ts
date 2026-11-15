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

/** 各模式的行为说明（新建弹层的 hint + 头部菜单的 title）。
 *  存 i18n key 而不是中文：GroupEditor 与 GroupView 两条入口必须共用同一份文案。 */
export const GROUP_MODE_HINT_KEY: Record<GroupMode, string> = {
  ask: 'hub.group.mode.ask.hint',
  auto: 'hub.group.mode.auto.hint',
  readonly: 'hub.group.mode.readonly.hint',
}

/** 菜单/徽标用的短标签（同样是 i18n key）。 */
export const GROUP_MODE_LABEL_KEY: Record<GroupMode, string> = {
  ask: 'hub.group.mode.ask.label',
  auto: 'hub.group.mode.auto.label',
  readonly: 'hub.group.mode.readonly.label',
}
