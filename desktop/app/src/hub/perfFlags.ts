// P4-01 的性能 memo 运行期回滚开关。
//
// 验收要求「性能数据与回滚开关」：支持/用户不用重新发版，在 DevTools 里执行
//   localStorage.setItem('hub.perfMemo', '0')
// 然后重载应用（或重进 Hub），GroupView 就退回 P4-01 之前的「每次渲染重算整条
// 已结算时间线」。删掉该键 / 设为其他值 = 默认开启（当前优化行为）。
//
// 为什么默认开启且读取失败也开启：这是**性能**回滚，不是功能开关；storage 在
// 隐私模式/沙箱里可能连访问都抛 SecurityError，此时退回优化路径仍是正确渲染。
// 值与历史 localStorage 读取保持一致：只认精确的 '0'。

export const HUB_PERF_MEMO_KEY = 'hub.perfMemo'

/** localStorage 的最小接口（测试注入内存实现；隐私模式下可以是 null）。 */
export interface HubPerfStorage {
  getItem(key: string): string | null
}

/**
 * 解析性能 memo 开关。默认 true；`hub.perfMemo === '0'` 时 false；
 * storage 不可用或 getItem 抛错时 fail-open 返回 true（保持当前优化行为）。
 */
export function hubPerfMemoEnabled(storage?: HubPerfStorage | null): boolean {
  try {
    const store = storage === undefined
      ? (typeof localStorage === 'undefined' ? null : localStorage)
      : storage
    if (!store) return true
    return store.getItem(HUB_PERF_MEMO_KEY) !== '0'
  } catch {
    return true
  }
}
