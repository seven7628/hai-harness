// 思考正文的段落切分（Narrative 的 Thinking 组件用）。
//
// 背景：思考正文是纯文本 + white-space: pre-wrap，段落之间靠**空行**分隔。pre-wrap 下空行会
// 渲染成一整个行盒（V3 21px / 默认皮肤 21px），段落间隔 ≈ 2 倍行高 —— 比正文段落（.msg .body p
// 的 4px）松一个数量级，V3 里尤其明显（2026-09-19 用户报障）。
// 修法：把思考正文按空行切成 `<p class="tk-p">`，间距交给 CSS 按皮肤控制
// （默认皮肤 = 原空行高度，观感不变；V3 = 4px，与正文同节奏）。
//
// 关键边界：围栏代码块（```）**内部**的空行属于代码，不是段落分隔 —— 无脑 split(/\n{2,}/)
// 会把代码块拆成两段，且 ``` 的开合配对被打断（inlineCode 按反引号奇偶配对，配对错位后
// 代码会渲染成普通文本）。所以这里按行扫描 + 围栏状态跟踪。
//
// 纯函数（无 React/store 依赖）→ 可被 node 直接断言（scripts/test-thinking-text.mjs）。
export function splitThinkingParagraphs(text: string): string[] {
  const lines = text.split('\n').map((l) => l.replace(/\r$/, '')) // CRLF：逐行去掉尾随 \r
  const paras: string[] = []
  let cur: string[] = []
  let inFence = false
  for (const line of lines) {
    // 围栏行本身始终归属当前段落（开/合围栏都是内容的一部分）
    if (/^\s*```/.test(line)) inFence = !inFence
    if (!inFence && line.trim() === '') {
      if (cur.length > 0) {
        paras.push(cur.join('\n'))
        cur = []
      }
      continue // 段落之间的空行（含连续多行）不产生空段
    }
    cur.push(line)
  }
  if (cur.length > 0) paras.push(cur.join('\n'))
  return paras
}
