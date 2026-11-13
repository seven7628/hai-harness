// 内联「任务等待你」卡的提问解析与作答拼装（HubInlineNeeds 的纯逻辑）。
//
// 卡片只展示、只把答案交给 hub_need_resolve；答案口径必须与 AskUserQuestionPanel
// 一致 —— 同一件事在两处有不同口径，用户就会答出模型读不懂的东西：
//   · 多选 = 勾选项用「；」拼接，自由输入追加在末尾（AskUserQuestionPanel.finalAnswer）；
//   · 单选 / 自由输入 = 原值。
// need.body 是 events.AskUserQuestion.Questions 的 JSON：
//   [{id, question, options: [{label, preview}], multi}]
// 纯函数，Node 直测（scripts/test-hub-inline-needs.mjs）。

export interface InlineNeedOption {
  label: string
  /** 选项预览（「必须看见才能选」的备选才带；仅单选，见 tools/question 的校验）。 */
  preview: string
}

export interface InlineNeedQuestion {
  id: string
  text: string
  /** true = 可多选：勾选多个，提交时用「；」拼成一条 answer。 */
  multi: boolean
  options: InlineNeedOption[]
}

/** 选项兼容两种写法：纯字符串（等价 {label}）与对象 {label|value, preview}。 */
export function optionList(raw: unknown): InlineNeedOption[] {
  if (!Array.isArray(raw)) return []
  const out: InlineNeedOption[] = []
  for (const o of raw) {
    if (typeof o === 'string') {
      const label = o.trim()
      if (label) out.push({ label, preview: '' })
      continue
    }
    if (!o || typeof o !== 'object') continue
    const it = o as { label?: unknown; value?: unknown; preview?: unknown }
    const raw2 = typeof it.label === 'string' && it.label.trim()
      ? it.label
      : (typeof it.value === 'string' ? it.value : '')
    const label = raw2.trim()
    if (!label) continue
    out.push({ label, preview: typeof it.preview === 'string' ? it.preview : '' })
  }
  return out
}

/**
 * need.body → 题目列表。纯文本 / 老格式（不是 JSON 数组）返回空数组，
 * 调用方据此退回展示原始 body（截图/日志里能看出模型到底问了什么）。
 */
export function parseInlineQuestions(body: string | null | undefined): InlineNeedQuestion[] {
  const text = String(body ?? '').trim()
  if (!text) return []
  let raw: unknown
  try {
    raw = JSON.parse(text)
  } catch {
    return []
  }
  if (!Array.isArray(raw)) return []
  const out: InlineNeedQuestion[] = []
  raw.forEach((v, i) => {
    if (!v || typeof v !== 'object') return
    const q = v as { id?: unknown; question?: unknown; text?: unknown; options?: unknown; multi?: unknown }
    const id = typeof q.id === 'string' && q.id.trim() ? q.id.trim() : String(i)
    const title = typeof q.question === 'string' && q.question.trim()
      ? q.question
      : (typeof q.text === 'string' && q.text.trim() ? q.text : `问题 ${i + 1}`)
    out.push({ id, text: title, multi: q.multi === true, options: optionList(q.options) })
  })
  return out
}

/**
 * 单题最终答案：多选 = 勾选项（按勾选顺序）用「；」拼接 + 自由输入；单选/自由输入 = 原值。
 * 多选**不能**被写成单选：点第二个选项是「再加一个」，不是「换一个」。
 */
export function composeAnswer(
  q: InlineNeedQuestion,
  picked: string[] | undefined,
  text: string | undefined,
): string {
  const typed = String(text ?? '').trim()
  if (!q.multi) return typed
  const parts = [...new Set((picked ?? []).filter(Boolean))]
  if (typed) parts.push(typed)
  return parts.join('；')
}

/**
 * 整张卡的答案：{question_id: answer}。每题都带上（没答的写空串）——
 * Go 侧 formatAnswers 会把空答案明确写成「用户没答」，与「没提交」区分开。
 */
export function composeAnswers(
  questions: InlineNeedQuestion[],
  picks: Record<string, string[]> | undefined,
  texts: Record<string, string> | undefined,
): Record<string, string> {
  const out: Record<string, string> = {}
  for (const q of questions) out[q.id] = composeAnswer(q, picks?.[q.id], texts?.[q.id])
  return out
}

/**
 * 预览：当前选中项的 preview（多选取第一个勾选且带预览的）；选中项没有 preview、
 * 或还没选时取第一个带 preview 的选项 —— 与 AskUserQuestionPanel.previewFor 同规则，
 * 卡片一出现就看得见备选长什么样，而不是空白等着点。
 * selected 由调用方按题型给：多选 = 勾选集合，单选/自由输入 = 那个值。
 */
export function previewOf(
  q: InlineNeedQuestion,
  selected: string[] | undefined,
): { label: string; text: string } | null {
  const sel = selected ?? []
  const chosen = q.options.find((o) => sel.includes(o.label) && o.preview)
  if (chosen) return { label: chosen.label, text: chosen.preview }
  const first = q.options.find((o) => o.preview)
  return first ? { label: first.label, text: first.preview } : null
}
