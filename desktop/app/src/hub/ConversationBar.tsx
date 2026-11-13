import { useEffect, useState } from 'react'
import type { HubConversation } from './types'
import { conversationBarModel } from './conversation'

// 自动对话状态条（P3b-07 → P6-10）：一条横条说清楚本轮 / 本链 / 队列 / pair（含
// 自动恢复倒计时）/ 投递账本（已入队·已回复·未回复·失败·跳过）/ 终态，并给「重置
// 本群限制」「停止自动对话」两个门禁。
//
// 分工：所有文案与门禁在 conversation.ts 的 conversationBarModel 里（纯函数，Node
// 直测），这里只负责画 + 一个「倒计时要能自己走」的时钟。
// 位置：header 与 messages 之间的独立一档 —— 状态是**当前事实**，滚动历史时不能消失。

/** pair 恢复倒计时的心跳：只在真的到限时挂定时器（其余靠 hub_conversation 事件驱动）。 */
const PAIR_TICK_MS = 30_000

export default function ConversationBar({
  conversation,
  pairNames,
  active = false,
  stopping = false,
  resetting = false,
  onStop,
  onReset,
}: {
  conversation: HubConversation | null | undefined
  pairNames?: Record<string, string>
  /** 当前群有 run 在跑/有流式草稿（停止按钮的「active」条件）。 */
  active?: boolean
  stopping?: boolean
  resetting?: boolean
  onStop?: () => void
  onReset?: () => void
}) {
  // 「约 8 分钟后自动恢复」要有东西驱动重渲，否则那条字会一直停在挂载时的值。
  // 只在 pair_reset_at 存在时挂：其他情况 hub_conversation 事件本来就够频繁。
  const resetAt = conversation?.pair_reset_at ?? ''
  const [, setTick] = useState(0)
  useEffect(() => {
    if (!resetAt) return
    const timer = setInterval(() => setTick((n) => n + 1), PAIR_TICK_MS)
    return () => clearInterval(timer)
  }, [resetAt])

  const model = conversationBarModel({ conversation, pairNames, active })
  if (!model.visible) return null
  return (
    <div className={`hub-conv-bar ${model.tone}`} role="group" aria-label="自动对话状态">
      <span className="hub-conv-status" role="status" aria-live="polite" title={model.statusTitle || undefined}>
        <span className="hub-conv-dot" aria-hidden="true" />
        {model.status}
      </span>
      {model.segments.length > 0 ? (
        <span className="hub-conv-facts">
          {model.segments.map((s, i) => (
            <span key={s.key} className={`hub-conv-seg hub-conv-${s.key}${s.tone ? ` ${s.tone}` : ''}`} title={s.title}>
              {i > 0 ? <span className="hub-conv-sep" aria-hidden="true">·</span> : null}
              {s.text}
            </span>
          ))}
        </span>
      ) : null}
      <span className="hub-conv-actions">
        {onReset && model.canReset ? (
          <button
            type="button"
            className="hub-link hub-conv-reset"
            onClick={onReset}
            disabled={resetting}
            title={model.resetTitle}
          >
            {resetting ? '重置中…' : '重置本群限制'}
          </button>
        ) : null}
        {onStop ? (
          <button
            type="button"
            className="hub-link hub-conv-stop"
            onClick={onStop}
            disabled={!model.canStop || stopping}
            title={model.canStop ? '停止自动对话（清空队列并中断当前 run）' : '当前没有可停止的自动对话'}
          >
            {stopping ? '停止中…' : '停止自动对话'}
          </button>
        ) : null}
      </span>
    </div>
  )
}
