// 导航丢弃判定 + 诊断 trace（FE-03）。
//
// 背景：前端用单调 `navigationGeneration` 丢弃「上一次导航的迟到响应」——这是正确且必须的
// （否则用户切走之后，旧工作区的会话会把当前界面覆盖掉）。问题是**丢弃是静默的**：
// 只在「恢复重试」那一条路径上有一条 warn，其余情况用户看到的是「历史不出来 / 骨架屏超时
// 后空白」，代码里也查不到「这一条响应为什么没落到视图」。同一段判断还把三类完全不同的
// 情况混成一个布尔：
//
//   stale（过期）：这条响应属于已被新导航取代的那一次请求 —— **正常**，用户点快一点就会发生，
//                  不需要告警，只需要能查到「它被丢了、被哪一代丢的」；
//   inconsistent（不一致）：代际对得上，但响应的 workspace / session_id 与发起时不符 ——
//                  这是**真正的异常**（串台、身份错配、bridge 回错目标），必须能和 stale 区分开，
//                  否则真 bug 永远埋在「反正都是丢弃」里。
//
// 本模块只做两件事：判定 + 有界记录（环形缓冲）。**不改变任何行为**：丢弃仍然是丢弃，
// 不弹 toast、不设置用户可见错误（除非调用方本来就要重试/告警，见 scheduleRestoreRetry）。
// 记录里带发起/当前代际与两端 workspace/session，足以从一份用户日志反推现场。

/** 丢弃类别：过期（正常）vs 不一致（异常）。 */
export type NavDiscardKind = 'stale' | 'inconsistent'

/** 判定输入（全部来自请求登记 + 响应回显 + 当前 store，无隐式读全局）。 */
export interface NavDiscardInput {
  /** 响应回显 / 登记的目标 workspace。 */
  responseWorkspace?: string
  /** 当前活跃 workspace（未打开项目时为空）。 */
  workspace?: string
  /** 发起命令时登记的导航代际；undefined = 这条响应没有对应的在途登记（查不到来源）。 */
  metaGeneration?: number
  /** 当前单调导航代际。 */
  generation: number
  /** 发起时登记的目标会话 id（恢复请求会显式带）。 */
  expectedSession?: string
  /** 响应里的 session_id。 */
  sessionId: string
  /** 恢复 attempt 已被更新请求取代（响应回显的 attempt 小于在途值）。 */
  staleAttempt?: boolean
}

/** 判定结果：current=true 才允许改写视图/激活会话。 */
export type NavVerdict = { current: true } | { current: false; kind: NavDiscardKind; reason: string }

/**
 * classifyNavDiscard 判定一条响应是不是当前导航的。
 *
 * 判定顺序（先判「过期」再判「不一致」）：代际/attempt 是最强的取代证据 —— 一旦被更新的
 * 导航取代，同一条响应即使携带的 workspace 也对不上，本质仍是「过期」而不是身份错配。
 * 顺序同时决定 trace 里的措辞，改动顺序会让历史日志的语义漂移。
 */
export function classifyNavDiscard(p: NavDiscardInput): NavVerdict {
  if (p.staleAttempt) {
    return { current: false, kind: 'stale', reason: '过期(stale)：恢复 attempt 已被更新的请求取代' }
  }
  if (p.metaGeneration == null) {
    // 没有登记在途信息 → 无从证明它属于当前导航。这类响应绝不能被应用（可能来自
    // 刷新前的旧窗口 / 重复响应），但也不该被说成「过期」：它是身份对不上。
    return { current: false, kind: 'inconsistent', reason: '不一致(inconsistent)：响应没有对应的在途登记' }
  }
  if (p.metaGeneration !== p.generation) {
    return {
      current: false,
      kind: 'stale',
      reason: `过期(stale)：导航代际 ${p.metaGeneration} → ${p.generation}`,
    }
  }
  if (p.responseWorkspace && p.responseWorkspace !== p.workspace) {
    return {
      current: false,
      kind: 'inconsistent',
      reason: `不一致(inconsistent)：响应 workspace=${p.responseWorkspace} 与当前 ${p.workspace} 不符`,
    }
  }
  if (p.expectedSession && p.expectedSession !== p.sessionId) {
    return {
      current: false,
      kind: 'inconsistent',
      reason: `不一致(inconsistent)：响应 session_id=${p.sessionId} 与请求目标 ${p.expectedSession} 不符`,
    }
  }
  return { current: true }
}

/** 一条丢弃记录（字段刻意冗余：现场日志要能自证，不能要求读者回查 store）。 */
export interface NavDiscardRecord {
  at: number
  kind: NavDiscardKind
  /** 触发丢弃的命令类型（new_session / …）。 */
  type: string
  reason: string
  /** 响应里的会话 id。 */
  sid: string
  /** 响应回显 / 登记的 workspace。 */
  responseWorkspace?: string
  /** 当前活跃 workspace（未打开项目时为空）。 */
  workspace?: string
  metaGeneration?: number
  generation: number
  expectedSession?: string
}

/** 环形缓冲上限：只留最近若干条（诊断用，不参与任何行为，不占渲染内存）。 */
export const NAV_DISCARD_LIMIT = 50

const discards: NavDiscardRecord[] = []

/**
 * recordNavDiscard 记一条丢弃。**永不抛**：诊断代码不能反过来影响导航主流程。
 *
 * console.debug 而非 warn/log：stale 是高频正常现象（用户连点导航就会产生），用 warn 会
 * 淹没真正的 inconsistent。devtools 的 Verbose 级别或下面的有界缓冲才是它的查看入口。
 */
export function recordNavDiscard(r: NavDiscardRecord): void {
  try {
    discards.push(r)
    if (discards.length > NAV_DISCARD_LIMIT) discards.splice(0, discards.length - NAV_DISCARD_LIMIT)
    console.debug(
      `[nav-trace] 丢弃${r.type}（${r.kind}）：${r.reason}` +
        ` sid=${r.sid || '-'} ws=${r.responseWorkspace || r.workspace || '-'}` +
        ` gen=${r.metaGeneration ?? '-'}→${r.generation}`,
    )
  } catch {
    /* 诊断通道自身的问题不影响导航 */
  }
}

/** navDiscards 返回最近丢弃记录的副本（测试/未来诊断面板用；外部改不到内部状态）。 */
export function navDiscards(): NavDiscardRecord[] {
  return discards.slice()
}

/** clearNavDiscards 清空记录（测试隔离用；生产不会调用）。 */
export function clearNavDiscards(): void {
  discards.length = 0
}
