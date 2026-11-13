// UI 信封总线（PR-12）：bridge → 渲染层的 `ui` 通道。
//
// 为什么**不**复用事件通道（store 的 dispatchEvent / events.Event）：`ui` 不是 agent
// 事件 —— 它没有 run/session 归属、不进 transcript、不落盘、不重放（00-conventions §4.4）。
// 塞进事件流会让每一条 UI 通知都被当成「会话事实」参与视图/checkpoint 构建，
// 那是语义污染，不是复用。代价是这张表要自己维护，所以它刻意只有三件事：订阅、
// 退订、投递。
//
// 没有消费者是**合法状态**（PR-12 只建通道：surfaces_changed 的真正订阅方是 PR-13
// 的插件页）。因此未知 op / 无订阅者一律静默 —— 不报错、不打日志。
type UIListener = (data: unknown) => void

const listeners = new Map<string, Set<UIListener>>()

/** 订阅某个 op；返回退订函数。op 为空/非字符串时不订阅（返回空操作）。 */
export function subscribeUI(op: string, fn: UIListener): () => void {
  if (!op || typeof op !== 'string') return () => {}
  let set = listeners.get(op)
  if (!set) {
    set = new Set()
    listeners.set(op, set)
  }
  set.add(fn)
  return () => {
    const cur = listeners.get(op)
    if (!cur) return
    cur.delete(fn)
    if (cur.size === 0) listeners.delete(op) // 不留下空 Set（否则表会随 op 数量无界增长）
  }
}

/**
 * 投递一条 ui 通知。
 *
 * 单个监听器抛错必须被隔离：这条通道与事件流共用同一条 stdout 解析入口，
 * 一个消费者的 bug 不该让后续事件（agent 输出、command_response）一起消失。
 */
export function emitUI(op: string, data: unknown): void {
  if (!op) return
  const set = listeners.get(op)
  if (!set) return // 未知 op：静默忽略
  for (const fn of [...set]) {
    try {
      fn(data)
    } catch {
      /* 消费者自己的问题，不影响通道 */
    }
  }
}

/** 仅供测试/探针：清空订阅表（生产无调用点）。 */
export function resetUIBusForTest(): void {
  listeners.clear()
}
