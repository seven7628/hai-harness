import { memo } from 'react'

/* 一行统计里有两类数字值得上色，而它们是**字符串**的一部分
   （「Edited 1 file +96 −0」「Ran npm test · 277 ms」）：
     · 变更量：+96 绿 / −0 红 —— 与工具行、右侧文件页签的 add/del 同一对令牌；
     · 计时：`26 s` / `306 ms` / `17 ms` —— 用 --tone-time，让「多久」在这一屏里
       有一致的身份色（用户 2026-10：给所有计时信息一个与主题搭配的颜色）。
   只做显示，不改变文本内容（复制出去还是原句）。 */
const TOKEN = /([+−]\d[\d,]*)|(\d+(?:\.\d+)?\s?(?:ms|s|m)\b)/g

export const RichStats = memo(function RichStats({ text }: { text: string }) {
  if (!text) return null
  const out: (string | JSX.Element)[] = []
  let last = 0
  let i = 0
  for (const m of text.matchAll(TOKEN)) {
    const at = m.index ?? 0
    if (at > last) out.push(text.slice(last, at))
    const isDiff = m[1] != null
    const cls = isDiff ? (m[1]!.startsWith('+') ? 'rt-add' : 'rt-del') : 'rt-time'
    out.push(<span className={cls} key={i++}>{m[0]}</span>)
    last = at + m[0].length
  }
  if (last < text.length) out.push(text.slice(last))
  return <>{out}</>
})
