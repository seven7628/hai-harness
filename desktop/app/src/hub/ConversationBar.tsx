import type { HubConversation } from './types'
import {
  conversationCanStop,
  conversationPairText,
  conversationStatusLabel,
  conversationTerminalText,
  conversationTone,
  conversationTurnText,
} from './conversation'

// 自动对话状态条（P3b-07，方案 §7.5.4 / §8.3.5）。
// 事件驱动：hub_conversation 每次轮次/队列/终态变化都会来一条；这里只做展示。
// 停止按钮的门禁在 conversationCanStop：有 run/队列/任务在跑才可点（active 优先）。

export default function ConversationBar({
  conversation,
  pairNames,
  active = false,
  stopping = false,
  onStop,
}: {
  conversation: HubConversation | null | undefined
  pairNames?: Record<string, string>
  /** 当前群有 run 在跑/有流式草稿（停止按钮的「active」条件）。 */
  active?: boolean
  stopping?: boolean
  onStop?: () => void
}) {
  // 人派的长任务可能没有自动对话视图：只要还有 run/任务在跑，也要给停止入口（B7）。
  const status = conversation?.status ?? ''
  const synthetic = !status || status === 'idle'
  if (synthetic && !active) return null
  const tone = synthetic ? 'running' : conversationTone(status)
  const canStop = conversationCanStop(conversation, active)
  const pair = synthetic ? '' : conversationPairText(conversation, pairNames)
  const terminal = synthetic ? '' : conversationTerminalText(conversation)
  return (
    <div className={`hub-conv-bar ${tone}`} role="group" aria-label="自动对话状态">
      <span className="hub-conv-status">
        <span className="hub-conv-dot" aria-hidden="true" />
        {synthetic ? '任务运行中' : conversationStatusLabel(status)}
      </span>
      <span className="hub-conv-turns" role="status" aria-live="polite">
        {synthetic ? '当前有 run 在跑，可随时停止' : conversationTurnText(conversation)}
      </span>
      {pair ? <span className="hub-conv-pair" title="同一对智能体的往返次数（防踢皮球）">{pair}</span> : null}
      {terminal ? <span className="hub-conv-terminal">{terminal}</span> : null}
      {onStop ? (
        <button
          type="button"
          className="hub-link hub-conv-stop"
          onClick={onStop}
          disabled={!canStop || stopping}
          title={canStop ? '停止自动对话（清空队列并中断当前 run）' : '当前没有可停止的自动对话'}
        >
          {stopping ? '停止中…' : '停止自动对话'}
        </button>
      ) : null}
    </div>
  )
}
