// [recovered] from /private/tmp/builtin-surfaces.test.cjs（模块标记 `// src/components/terminal/paneMachine.ts`）
// [implemented] 依据 docs/TERMINAL_E2E_CONTRACTS.md「paneMachine 13 条状态转移」：
//   切片里机器叫 `next4`、类型全丢；`initialPaneState` 被 esbuild 当**未使用导出**删掉了
//   （TerminalPane 从 `{s:'connecting',ptyId}` 起跑），但它出现在还原的 -test 切线里
//   （`// src/components/terminal/paneMachine.ts` 同一处），故按测试恢复。
//   本文件补导出名（TerminalPane.tsx 的 import：next）与类型标注，**逻辑逐字未改**；
//   移除 @ts-nocheck。
//
// 纯函数状态机（不碰 DOM、不碰 socket）：输入「当前状态 + 事件」→ 输出「新状态 + 动作」。
// 组件只负责执行动作（建 PTY / 开 socket / reset / toast），于是「哪些事件在哪个状态算数」
// 可以单测钉死 —— 见 paneMachine-test.ts 的 13 条。

export type PaneState =
  | { s: 'idle' }
  | { s: 'creating' }
  | { s: 'connecting'; ptyId: string; ticket?: string }
  | { s: 'replaying'; ptyId: string }
  | { s: 'live'; ptyId: string }
  | { s: 'exited'; ptyId: string; code?: number }
  | { s: 'failed'; error: string }

export type PaneFx =
  | { k: 'create-pty' }
  | { k: 'issue-ticket' }
  | { k: 'open-socket' }
  | { k: 'close-socket' }
  | { k: 'close-pty' }
  | { k: 'reset-term' }
  | { k: 'show' }
  | { k: 'toast'; error: string }

export type PaneEvent =
  | { t: 'open' }
  | { t: 'created'; ptyId: string }
  | { t: 'ticket'; ticket: string }
  | { t: 'ws-open' }
  | { t: 'hello' }
  | { t: 'reset' }
  | { t: 'output' }
  | { t: 'exit'; code?: number }
  | { t: 'timeout'; phase: 'create' | 'connect' }
  | { t: 'error'; error: string }
  | { t: 'close' }

const TIMEOUT = {
  create: '创建终端超时',
  connect: '连接终端超时',
} as const

/**
 * 挂载即可渲染的空态：**不凭空建 PTY**（PTY 由 store 的 termEnsure 建，组件只连）。
 * 与 TerminalPane 的 `{s:'connecting', ptyId}` 起跑并存：这条导出给「等待外部给 ptyId」
 * 的用法（以及单测的起点）。
 */
export const initialPaneState: PaneState = { s: 'idle' }

function ptyIdOf(state: PaneState): string | undefined {
  if (state.s === 'connecting' || state.s === 'replaying' || state.s === 'live' || state.s === 'exited') {
    return state.ptyId
  }
  return undefined
}

export function next(state: PaneState, ev: PaneEvent): { state: PaneState; fx: PaneFx[] } {
  // 两条「任意状态」事件：close 是用户关标签，error 是链路/桥给的失败。
  if (ev.t === 'close') {
    return {
      state: { s: 'idle' },
      fx: ptyIdOf(state) ? [{ k: 'close-socket' }, { k: 'close-pty' }] : [{ k: 'close-socket' }],
    }
  }
  if (ev.t === 'error') {
    return { state: { s: 'failed', error: ev.error }, fx: [{ k: 'close-socket' }, { k: 'toast', error: ev.error }] }
  }

  switch (ev.t) {
    case 'open':
      if (state.s === 'idle') return { state: { s: 'creating' }, fx: [{ k: 'create-pty' }] }
      break
    case 'created':
      if (state.s === 'creating') return { state: { s: 'connecting', ptyId: ev.ptyId }, fx: [{ k: 'issue-ticket' }] }
      break
    case 'ticket':
      if (state.s === 'connecting') {
        return { state: { s: 'connecting', ptyId: state.ptyId, ticket: ev.ticket }, fx: [{ k: 'open-socket' }] }
      }
      break
    // 握手帧（hello）与 ws open 都不换状态：真正的「已连上」证据是第一条输出（replaying→live）。
    case 'ws-open':
    case 'hello':
      if (state.s === 'connecting') return { state, fx: [] }
      break
    case 'reset':
      if (state.s === 'connecting') return { state: { s: 'replaying', ptyId: state.ptyId }, fx: [{ k: 'reset-term' }] }
      break
    case 'output':
      if (state.s === 'replaying') return { state: { s: 'live', ptyId: state.ptyId }, fx: [{ k: 'show' }] }
      break
    case 'exit':
      if (state.s === 'live') {
        return { state: { s: 'exited', ptyId: state.ptyId, code: ev.code }, fx: [{ k: 'close-socket' }] }
      }
      break
    case 'timeout':
      // 迟到/错阶段的 timeout 不算数：已连上的终端不该被一条过期的计时器毁掉。
      if (ev.phase === 'create' && state.s === 'creating') {
        const error = TIMEOUT.create
        return { state: { s: 'failed', error }, fx: [{ k: 'toast', error }] }
      }
      if (ev.phase === 'connect' && state.s === 'connecting') {
        const error = TIMEOUT.connect
        return { state: { s: 'failed', error }, fx: [{ k: 'close-socket' }, { k: 'toast', error }] }
      }
      break
  }
  // 未列出的组合：原样返回**同一个对象**（调用方可以靠引用相等判「没变」），无动作。
  return { state, fx: [] }
}
