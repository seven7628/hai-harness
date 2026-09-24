// 模型提问（ask_user）的快捷选项：既能当「点选文本」，也能带一份**预览**。
//
// 预览的用途（后端工具描述里对模型讲的那条边界，这里是同一件事的 UI 侧）：
// 「必须看见才能选」的备选才有意义 —— ASCII 版面 / 代码或 diff 片段 / 配置或 schema
// 变体 / 小表格。所以预览是**每个选项各带一份**（不是问题级共享一块），UI 并排展示
// 「选项 ↔ 所选选项的预览」。
//
// 边界（后端 tools/question 校验层兜底）：仅单选可用 —— 多选的勾选项无法并排预览，
// multi:true + preview 会被工具直接拒绝（而不是静默丢掉预览：丢掉就等于「模型以为
// 用户看见了，而用户没看见」）。
export interface QuestionOption {
  label: string // 点选后**原样**成为回答文本（与 Go 侧 events.QuestionOption.Label 同源）
  preview?: string // markdown 预览；缺省 = 该选项无预览（走旧的横排按钮版面）
}

// parseQuestionOptions 把事件里的 options 归一化成 QuestionOption[]，兼容三种形态：
//   · {"label":"A","preview":"…"} —— 当前形态（预览能力上线后）；
//   · "A" —— 纯字符串：历史会话落盘的 JSONL 事件、前端 mock、以及模型的简写写法
//     （Go 侧 UnmarshalJSON 同样吃这两种，见 events/ask.go）；
//   · 其它垃圾值（null / 数字 / 嵌套对象）→ 丢弃：脏数据不该把提问面板渲染崩。
//
// 空 label（含纯空白）丢弃 —— 空按钮点不下去，也没有回答文本；preview 空串等同缺省
// （不落 preview 键，保持对象形状稳定，便于断言与快照比较）。
export function parseQuestionOptions(raw: unknown): QuestionOption[] {
  if (!Array.isArray(raw)) return []
  const out: QuestionOption[] = []
  for (const it of raw) {
    if (typeof it === 'string') {
      const label = it.trim()
      if (label) out.push({ label })
      continue
    }
    if (!it || typeof it !== 'object') continue
    const o = it as { label?: unknown; preview?: unknown }
    const label = typeof o.label === 'string' ? o.label.trim() : ''
    if (!label) continue
    const preview = typeof o.preview === 'string' ? o.preview : ''
    out.push(preview ? { label, preview } : { label })
  }
  return out
}

// hasPreview 该题是否需要「并排预览」版面（任一选项带非空 preview）。
export function hasPreview(options: QuestionOption[]): boolean {
  return options.some((o) => !!o.preview)
}
