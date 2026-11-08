// 「A、B 正在输入…」（P3b-07，方案 §7.5.3 / §8.3.5）。
// 数据来自群内流式草稿（hub_message_delta），由 GroupView 去重后传入。
// aria-live="polite" 只报一次人名；动画在 prefers-reduced-motion 下关闭。

export default function TypingIndicator({ names }: { names: string[] }) {
  const unique = Array.from(new Set(names.filter(Boolean)))
  const shown = unique.slice(0, 2)
  const more = unique.length - shown.length
  // live region 必须**先存在再更新**：names 为空时也保留容器（CSS 折叠为 0 高），
  // 否则读屏在节点插入时不会播报第一条「正在输入」（N8）。
  return (
    <div className={`hub-typing ${unique.length === 0 ? 'empty' : ''}`} aria-live="polite" aria-atomic="true">
      {unique.length > 0 ? (
        <>
          <span className="hub-typing-dots" aria-hidden="true">
            <i /><i /><i />
          </span>
          <span>
            {shown.join('、')} 正在输入…
            {more > 0 ? ` +${more}` : ''}
          </span>
        </>
      ) : null}
    </div>
  )
}
