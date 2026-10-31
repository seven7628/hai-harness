// 技能行/命令行的「它管什么」短语：把一整段 SKILL.md description 收成一行。
//
// 为什么要这一步：设计里 `/` 菜单的技能行是 `/anysearch` + `.m`「联网 · 并行检索」——
// 一个 2~4 词的标签（design/agent-runtime/v2/index.html:1021）。我们手里只有技能注册表
// 的 description，那是**整段**说明文字；直接塞进 `.m` 就会在 28px 高的一行里折成十几行、
// 压到相邻行和面板外面（用户 2026-10 截图：`/` 菜单里字挤成一团）。
//
// 取法有据：取第一句（描述的第一句本来就是「这个技能管什么」），再按需截断；
// 拉丁文本不切词，CJK 按字切。调用方把完整 description 放进 title，鼠标停一下就有全文。

/** 句读边界：句号/分号/感叹/问号/竖线/破折号/英文句点后空格。 */
const SENTENCE_END = /[。；;！!？?｜|]|\.\s|—{1,2}/

/**
 * shortReason 把描述收成一行短语。
 * @param desc 完整描述（可能多行、可能很长）
 * @param max  最多保留的字符数（不含省略号）；默认 30
 */
export function shortReason(desc: string, max = 30): string {
  const s = desc.replace(/\s+/g, ' ').trim()
  if (!s) return ''
  const cut = s.search(SENTENCE_END)
  const first = cut > 0 ? s.slice(0, cut) : s
  if (first.length <= max) return first.replace(/[，,、。；;：:\s]+$/, '')
  const slice = first.slice(0, max)
  // 拉丁词边界：不要把一个词劈成两半（`comp` + …）；空间太靠前就按字切
  const sp = slice.lastIndexOf(' ')
  const body = sp > max * 0.6 ? slice.slice(0, sp) : slice
  return body.replace(/[，,、\s]+$/, '') + '…'
}
