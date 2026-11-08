// 「A、B 正在输入…」（P3b-07，方案 §7.5.3 / §8.3.5）。
// 数据来自群内流式草稿（hub_message_delta），由 GroupView 去重后传入。
// aria-live="polite" 只报一次人名；动画在 prefers-reduced-motion 下关闭。

export default function TypingIndicator({ names }: { names: string[] }) {
  const unique = Array.from(new Set(names.filter(Boolean)))
  if (unique.length === 0) return null
  const shown = unique.slice(0, 2)
  const more = unique.length - shown.length
  return (
    <div className="hub-typing" aria-live="polite" aria-atomic="true">
      <span className="hub-typing-dots" aria-hidden="true">
        <i /><i /><i />
      </span>
      <span>
        {shown.join('、')} 正在输入…
        {more > 0 ? ` +${more}` : ''}
      </span>
    </div>
  )
}
