import { useEffect, useRef, useState } from 'react'
import { useAppStore, type PendingQuestionItem } from '../store/useAppStore'
import { useClock } from '../lib/useNow'
import { parseQuestionOptions } from '../lib/questionOptions'
import { Markdown } from './markdown'
import { useT } from '../i18n'

// 决策倒计时（2026-09-20 用户要求：让用户知道还剩多长时间做决策）：
// 剩 = timeoutMs - (提问以来的毫秒)。时钟复用应用唯一的 1s tick（useClock，
// 只有本小组件订阅 → 每秒 tick 不重渲正文/工具体），不另挂 setInterval。
// 返回 null = 无时限（后端 timeout_ms 缺省；如宿主用 NewQuestionWaiter(-1)）→ 不显示。
function useCountdown(q?: { timeoutMs?: number; ts?: number }): { leftMs: number; label: string } | null {
  const active = Boolean(q?.timeoutMs && q.timeoutMs > 0)
  const elapsed = useClock(q?.ts, active) // 提问至今的毫秒数（每秒走）
  if (!active || !q?.timeoutMs) return null
  const leftMs = Math.max(0, q.timeoutMs - (elapsed ?? 0))
  const total = Math.round(leftMs / 1000)
  const mm = Math.floor(total / 60)
  const ss = total % 60
  return { leftMs, label: `${mm}:${String(ss).padStart(2, '0')}` }
}

// 模型→用户提问面板（HITL 反方向）：ask_user 工具批次事件一次性展示全部问题，
// 每题快捷选项 + 自由输入，**必须全部作答后才能提交**（button 禁用直到全答）——
// 不可跳过、不可部分提交；不可取消（工具在等待回答 = 阻塞 agent 循环，提交是唯一出路）。
//
// 2026-09-20（用户报障「弹窗挡住对话历史，想先翻历史再决定」）：**从全屏 modal 改成局部停靠面板**。
// 此前是 `.modal-hint.open` 全屏 scrim（铺满 viewport + 半透明压暗）：叙述区被盖住、滚不动、
// 左栏也点不了（同一问题早在 docs/DESKTOP_HANDOFF.md §5 #12 记录过）。现在面板停靠在
// **叙述区与输入框之间**（见 components/CenterPanel.tsx）：叙述区只被压矮、照常滚动与选中，
// 用户可以边翻历史边作答；多会话下也顺手能切走再切回（question 按会话存 view，回来还在）。
// 批次大/想腾出整屏读历史时，点标题条即可**收起**面板（只留一行进度，作答状态不丢）。
// 等待窗口（默认 30 分钟，见 events/ask.go）以倒计时显示；到点工具结果会明确告诉模型
// 「用户未作答」，模型按最佳判断继续。
//
// 预览（模型的预览能力，见 lib/questionOptions）：选项带 preview 时左列竖排选项、右侧并排
// 显示**所选项**的预览（还没选就显示第一个带预览的选项，面板一展开就看得见备选长什么样）。
// 仅单选题走这个版面 —— 多选带预览会被后端 tools/question 直接拒绝。
export default function AskUserQuestionPanel() {
  const t = useT()
  const question = useAppStore((s) => s.question)
  const answerQuestion = useAppStore((s) => s.answerQuestion)
  // 每题作答：'' = 未答；点选选项或自由输入即答
  const [answers, setAnswers] = useState<Record<string, string>>({})
  // 多选题的勾选状态（questionId → 已选选项集合）；单选/自由输入仍走 answers
  const [multiSel, setMultiSel] = useState<Record<string, string[]>>({})
  // 面板收起（只留标题条）——把版面让给叙述区；作答状态都在本组件里，收起不丢
  const [folded, setFolded] = useState(false)
  const firstRef = useRef<HTMLInputElement>(null)
  const cd = useCountdown(question)

  // 每次新批次：重置作答 + 聚焦第一个输入（键盘直达；有选项的可先点选）。
  // preventScroll：面板恒在视野内，别让聚焦把叙述区的阅读位置带跑（用户可能正在翻历史）。
  useEffect(() => {
    setAnswers({})
    setMultiSel({})
    if (question) requestAnimationFrame(() => firstRef.current?.focus({ preventScroll: true }))
  }, [question?.id])

  if (!question) return null
  // 消费侧再过一次选项归一化（幂等）：上游塞进旧的纯字符串形态时，这里也不会渲染成空按钮、
  // 更不会让提交按钮永远禁用（那会把 agent 卡到超时）。形态知识只留在 lib/questionOptions。
  const items = question.items.map((it) => ({ ...it, options: parseQuestionOptions(it.options) }))
  // 一题已答：单选/自由输入非空；多选题 = 至少勾了一个选项 或 自由输入非空
  const isAnswered = (it: PendingQuestionItem) =>
    it.multi
      ? (multiSel[it.id]?.length ?? 0) > 0 || (answers[it.id] ?? '').trim() !== ''
      : (answers[it.id] ?? '').trim() !== ''
  const answered = items.filter(isAnswered).length
  const allAnswered = items.length > 0 && answered === items.length

  // 切换多选题的某个选项勾选状态
  const toggle = (id: string, o: string) =>
    setMultiSel((m) => {
      const cur = m[id] ?? []
      const next = cur.includes(o) ? cur.filter((x) => x !== o) : [...cur, o]
      return { ...m, [id]: next }
    })
  // 自由输入 / 单选选项的作答值
  const setAnswer = (id: string, v: string) => setAnswers((m) => ({ ...m, [id]: v }))

  // 组装每题最终答案：多选 = 勾选选项用「；」拼接（+ 自由输入追加）；单选/自由输入 = 原值
  const finalAnswer = (it: PendingQuestionItem): string => {
    if (it.multi) {
      const parts = [...(multiSel[it.id] ?? [])]
      const text = (answers[it.id] ?? '').trim()
      if (text) parts.push(text)
      return parts.join('；')
    }
    return (answers[it.id] ?? '').trim()
  }

  const submitAll = () => {
    if (!allAnswered) return // 有未答 → 不提交（按钮禁用兜底）
    answerQuestion(items.map((it) => ({ question_id: it.id, answer: finalAnswer(it) })))
  }

  // 预览锚：显示**用户当前选中**选项的预览；还没选时显示第一个带预览的选项
  //（面板一展开就看得见备选长什么样，而不是空白等着点）。点另一个选项即切到它的预览，
  // 单选语义不变（点选 = 作答）。
  const previewFor = (it: PendingQuestionItem): { label: string; preview: string } | null => {
    const withPv = it.options.filter((o) => o.preview)
    if (withPv.length === 0) return null
    const pick = withPv.find((o) => o.label === answers[it.id]) ?? withPv[0]
    return { label: pick.label, preview: pick.preview as string }
  }

  // 选项组：带预览时左列竖排、右侧并排预览（预览只在单选出现——多选带 preview 会被
  // 后端 tools/question 拒绝，这里再兜一层）；无预览沿用原来的横排换行按钮。
  const renderOptions = (it: PendingQuestionItem, i: number) => {
    const pv = it.multi ? null : previewFor(it)
    const list = (
      <div
        className={`q-options${pv ? ' col' : ''}`}
        role="group"
        aria-label={t('ask.optionsAria').replace('{n}', String(i + 1))}
      >
        {it.multi && <span className="q-multi-hint">{t('ask.multi')}</span>}
        {it.options.map((o) => {
          const sel = it.multi ? (multiSel[it.id]?.includes(o.label) ?? false) : answers[it.id] === o.label
          return (
            <button
              key={o.label}
              className={`q-option${sel ? ' sel' : ''}`}
              aria-pressed={sel}
              onClick={() => (it.multi ? toggle(it.id, o.label) : setAnswer(it.id, o.label))}
            >
              {o.label}
            </button>
          )
        })}
      </div>
    )
    if (!pv) return list
    return (
      <div className="q-split">
        {list}
        {/* 预览走正文同一个 Markdown 渲染器：模型写的代码块/表格/公式在预览里也成立 */}
        <div className="q-preview" role="figure" aria-label={t('ask.previewAria').replace('{label}', pv.label)}>
          <div className="q-preview-hd mono">{t('ask.previewFor').replace('{label}', pv.label)}</div>
          <div className="q-preview-body">
            <Markdown text={pv.preview} />
          </div>
        </div>
      </div>
    )
  }

  return (
    // 非模态：role="region"（不是 dialog/aria-modal —— 叙述区与左右栏都还能用）
    <div className={`dock${folded ? ' folded' : ''}`} role="region" aria-label={t('ask.title')}>
      <div className="dock-box">
        <h3
          className="dock-hd"
          role="button"
          tabIndex={0}
          aria-expanded={!folded}
          title={folded ? t('ask.expand') : t('ask.fold')}
          onClick={() => setFolded((v) => !v)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault()
              setFolded((v) => !v)
            }
          }}
        >
          <span className="chev" aria-hidden="true">{folded ? '▸' : '▾'}</span>
          <span className="q-mark">?</span> {t('ask.title')}{items.length > 1 ? t('ask.count').replace('{n}', String(items.length)) : ''}
          {question.runId && <span className="mono q-run">run {question.runId}</span>}
          {cd && (
            <span
              className={`mono dock-countdown${cd.leftMs === 0 ? ' over' : cd.leftMs <= 60_000 ? ' soon' : ''}`}
              title={t('ask.countdownTitle').replace('{total}', String(Math.round((question.timeoutMs ?? 0) / 60_000)))}
            >
              {cd.leftMs === 0 ? t('ask.timedOut') : t('ask.timeLeft').replace('{t}', cd.label)}
            </span>
          )}
          {folded && (
            <span className="dock-fold-hint">
              {t('ask.folded').replace('{answered}', String(answered)).replace('{total}', String(items.length))}
            </span>
          )}
        </h3>
        {!folded && (
          <>
            <div className="q-list">
              {items.map((it, i) => (
                <div className="q-item" key={it.id}>
                  <div className="q-title">
                    <span className="q-no">{i + 1}</span>
                    <span className="q-text">{it.question}</span>
                  </div>
                  {it.options.length > 0 && renderOptions(it, i)}
                  <input
                    ref={i === 0 ? firstRef : undefined}
                    name={`question-answer-${i}`}
                    value={answers[it.id] ?? ''}
                    onChange={(e) => setAnswer(it.id, e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key !== 'Enter') return
                      // 输入法组合态（中文等用 Enter 选字/上屏）不得触发提交；
                      // isComposing + keyCode 229 双保险兼容各平台 IME（同 Composer 主输入框）。
                      if (e.nativeEvent.isComposing || e.keyCode === 229) return
                      submitAll()
                    }}
                    placeholder={it.multi ? t('ask.placeholderExtra') : it.options.length ? t('ask.placeholderOptions') : t('ask.placeholder')}
                    aria-label={t('ask.inputAria').replace('{n}', String(i + 1))}
                    spellCheck={false}
                  />
                </div>
              ))}
            </div>
            <div className="acts">
              <span className="timeout">
                {cd && cd.leftMs === 0 ? t('ask.timedOutHint') : t('ask.allRequired')}
              </span>
              <button className="btn primary" disabled={!allAnswered || (cd?.leftMs === 0)} onClick={submitAll}>
                {t('ask.submitAll').replace('{answered}', String(answered)).replace('{total}', String(items.length))}
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  )
}
