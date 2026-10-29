import { useEffect, useRef, useState } from 'react'
import { useAppStore, type PendingQuestionItem } from '../store/useAppStore'
import { useClock } from '../lib/useNow'
import { parseQuestionOptions } from '../lib/questionOptions'
import { Markdown } from './markdown'
import { useT } from '../i18n'
import { useScopedQuestion } from './sessionScope'
import { useSkin } from '../lib/useSkin'
import './ask-runtime.css'
import './ask-runtime.css'

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
  // 运行时皮肤：同一份作答状态渲染两套类名。通用 .q-* / .btn 的规则和设计的
  // .ask-* 同权重、又写在后面（index.css 先加载但选择器更具体），共存就会出现
  // 「设计的间距对了、底色却是旧的」——类名互斥比堆 !important 干净。
  const rt = useSkin() === 'runtime'
  // 作用域感知（2026-10，与 ApprovalPanel 同一条判定）：看主会话 → 主 run 的提问；
  // 看子会话 → 那个子 agent 的提问。提问通道在后端同样是 Session 级单槽，
  // 这里只做展示过滤（别人的提问不该在你眼前挡住面板）。
  const question = useScopedQuestion()
  const answerQuestion = useAppStore((s) => s.answerQuestion)
  // 每题作答：'' = 未答；点选选项或自由输入即答
  const [answers, setAnswers] = useState<Record<string, string>>({})
  // 多选题的勾选状态（questionId → 已选选项集合）；单选/自由输入仍走 answers
  const [multiSel, setMultiSel] = useState<Record<string, string[]>>({})
  // 面板收起（只留标题条）——把版面让给叙述区；作答状态都在本组件里，收起不丢
  const [folded, setFolded] = useState(false)
  // 分页：设计一次只问一道（.ask-step 那行「question 1 of 2」+ 圆点），
  // 不是把两三个问题上下摞成两三张卡 —— 摞起来读的人不知道自己在第几问。
  const [step, setStep] = useState(0)
  const [cursor, setCursor] = useState(-1) // ↑↓ 的落点（设计 .ask-opt[data-cursor]）
  // 预览跟着**指针**走（设计 parts/ask.js:132-137 的 shownIdx：hover > cursor > 已选 > 第一条）：
  // 只在点选之后才换预览，等于要用户先做决定再看备选的差别
  const [hoverIdx, setHoverIdx] = useState(-1)
  const firstRef = useRef<HTMLInputElement>(null)
  // 键盘的入口是**卡片本身**，不是自由输入框：设计 parts/ask.js:454-460 写明
  // 「点卡片任意空白处就聚焦它，否则数字键要先 Tab 进去才生效」。焦点在输入框里时
  // 数字键属于正文（下面的 inInput 守卫），所以默认焦点给卡片。
  const cardRef = useRef<HTMLDivElement>(null)
  const cd = useCountdown(question)

  // 每次新批次：重置作答 + 聚焦第一个输入（键盘直达；有选项的可先点选）。
  // preventScroll：面板恒在视野内，别让聚焦把叙述区的阅读位置带跑（用户可能正在翻历史）。
  useEffect(() => {
    setAnswers({})
    setMultiSel({})
    setStep(0)
    setCursor(-1)
    setHoverIdx(-1)
    if (question) requestAnimationFrame(() => cardRef.current?.focus({ preventScroll: true }))
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

  // allowEmpty=false：整批都要有答案（继续/完成按钮的语义）；
  // allowEmpty=true：跳过 —— 未答的题按空答案交上去，Go 侧 formatAnswers 会明确写成
  // 「(no answer from the user — no option selected, no text typed)」，模型分得清
  // 「用户跳过了」和「用户答了但答的是空」。设计里 Skip 就是这个语义（parts/ask.js:445-448）。
  const submitAll = (allowEmpty = false) => {
    if (!allowEmpty && !allAnswered) return // 有未答 → 不提交（按钮禁用兜底）
    // 带上面板正在显示的**这批**的 batch id：主/子可能各有一批在等，
    // 由面板（而非 store 里"最后到达那条"）决定回答发去哪一批。
    answerQuestion(question.id, items.map((it) => ({ question_id: it.id, answer: finalAnswer(it) })))
  }

  // 预览锚：显示**用户当前选中**选项的预览；还没选时显示第一个带预览的选项
  //（面板一展开就看得见备选长什么样，而不是空白等着点）。点另一个选项即切到它的预览，
  // 单选语义不变（点选 = 作答）。
  const previewFor = (it: PendingQuestionItem): { label: string; preview: string } | null => {
    const withPv = it.options.filter((o) => o.preview)
    if (withPv.length === 0) return null
    // 顺序照设计：指针 > 键盘光标 > 已选 > 第一条带预览的。指针压过键盘，
    // 因为指针的位置是用户刚放下的，键盘光标可能还停在上一次的方向键上。
    const cand = [
      hoverIdx >= 0 ? it.options[hoverIdx] : undefined,
      cursor >= 0 ? it.options[cursor] : undefined,
      withPv.find((o) => o.label === answers[it.id]),
      withPv[0],
    ].find((o) => o && o.preview)
    return cand ? { label: cand.label, preview: cand.preview as string } : null
  }

  // 选项组：带预览时左列竖排、右侧并排预览（预览只在单选出现——多选带 preview 会被
  // 后端 tools/question 拒绝，这里再兜一层）；无预览沿用原来的横排换行按钮。
  const renderOptions = (it: PendingQuestionItem, i: number, cur = -1) => {
    const pv = it.multi ? null : previewFor(it)
    const list = (
      <div
        className={`${rt ? 'ask-list' : `q-options${pv ? ' col' : ''}`}`}
        role="group"
        aria-label={t('ask.optionsAria').replace('{n}', String(i + 1))}
        onPointerLeave={() => setHoverIdx(-1)}
      >
        {it.multi && <span className="q-multi-hint">{t('ask.multi')}</span>}
        {it.options.map((o) => {
          const sel = it.multi ? (multiSel[it.id]?.includes(o.label) ?? false) : answers[it.id] === o.label
          return (
            <button
              key={o.label}
              className={rt ? 'ask-opt' : `q-option${sel ? ' sel' : ''}`}
              {...(rt && it.options.indexOf(o) === cur ? { 'data-cursor': '1' } : {})}
              role={it.multi ? 'checkbox' : 'radio'}
              aria-checked={sel}
              // 指针落在一行上时键盘光标要让位（设计 ask.js:461-472 的 sync）：
              // 两个高亮同时亮着，用户会以为选了两个；预览也跟着指针换（shownIdx）
              {...(rt ? {
                onPointerEnter: () => {
                  setCursor(-1)
                  setHoverIdx(it.options.indexOf(o))
                },
              } : {})}
              onClick={() => (it.multi ? toggle(it.id, o.label) : setAnswer(it.id, o.label))}
            >
              {/* 设计的三段结构：标记（圆点/勾）· 文案 · 序号键位（各占一列，
                  否则每个标签后跟着数字会参差，参差读起来像失误） */}
              <span className="ask-mark" aria-hidden="true" />
              <span className="ask-lab">{o.label}</span>
              {it.options.indexOf(o) < 9 ? <kbd aria-hidden="true">{it.options.indexOf(o) + 1}</kbd> : null}
            </button>
          )
        })}
      </div>
    )
    if (!pv) return list
    // 运行时皮肤：设计的 .ask-body 本身就是两列（选项 max-content + 预览 1fr），
    // 再套一层 .q-split 会让它变成「一格」，实测把选项列撑到 687px、预览顶出卡外。
    const pvEl = (
      <>
        {list}
        {/* 预览走正文同一个 Markdown 渲染器：模型写的代码块/表格/公式在预览里也成立 */}
        <div className={rt ? 'ask-pv' : 'q-preview'} role="figure" aria-label={t('ask.previewAria').replace('{label}', pv.label)}>
          <div className={rt ? 'ask-pv-head mono' : 'q-preview-hd mono'}>
            {/* 设计只写一个词（preview），右侧才是选项名 —— 两处都印名字会把
                同一句话在一行里说两遍（原来那版就是这样） */}
            <span>{rt ? 'preview' : t('ask.previewFor').replace('{label}', pv.label)}</span>
            <span className="ask-pv-which">{pv.label}</span>
          </div>
          {rt ? (
            /* 预览是**排版好的纯文本**（parts/ask.css:238-247：mono + pre + 保留
               缩进，ASCII 图换行就不是图了）。所以这里不过 markdown 渲染器。 */
            <pre key={pv.label} className="ask-pv-pre ask-swap" data-empty={pv.preview ? undefined : '1'}>
              {pv.preview || ''}
            </pre>
          ) : (
            <div className="q-preview-body">
              <Markdown text={pv.preview} />
            </div>
          )}
        </div>
      </>
    )
    return rt ? pvEl : <div className="q-split">{pvEl}</div>
  }


  if (rt) {
    const at = Math.min(step, Math.max(0, items.length - 1))
    const it = items[at]
    const last = at + 1 >= items.length
    const here = isAnswered(it)
    const goNext = () => {
      if (!here || cd?.leftMs === 0) return
      if (last) submitAll()
      else {
        setStep(at + 1)
        setCursor(-1)
        requestAnimationFrame(() => firstRef.current?.focus({ preventScroll: true }))
      }
    }
    return (
      <div className={`dock ask-scope${folded ? ' folded' : ''}`} role="region" aria-label={t('ask.title')}>
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
            <span className="q-mark">?</span> {t('ask.title')}
            {items.length > 1 ? t('ask.count').replace('{n}', String(items.length)) : ''}
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
                <div
                  className="ask"
                  ref={cardRef}
                  tabIndex={0}
                  data-form={it.multi ? 'multi' : previewFor(it) ? 'preview' : 'plain'}
                  {...(items.length > 1 ? { 'data-step': '' } : {})}
                  role="group"
                  aria-labelledby={`ask-q-${it.id}`}
                  // 点卡片空白处 = 把键盘拿回来（设计 parts/ask.js:454-460）；落在按钮上
                  // 时不抢焦点，否则按钮自己的 ⏎ 会被这一下吃掉
                  onPointerDown={(e) => {
                    if ((e.target as HTMLElement).closest('button')) return
                    cardRef.current?.focus({ preventScroll: true })
                  }}
                  onKeyDown={(e) => {
                    if (e.metaKey || e.ctrlKey || e.altKey) return
                    const tgt = e.target as HTMLElement
                    const inInput = tgt.tagName === 'INPUT' || tgt.tagName === 'TEXTAREA'
                    // 页脚按钮保留自己的键：⏎ 在「上一步」上就该是上一步（设计 ask.js:489）
                    if (tgt.closest('.ask-btn, .ask-all')) return
                    if (!inInput && (e.key === 'ArrowDown' || e.key === 'ArrowUp'
                      || e.key === 'ArrowRight' || e.key === 'ArrowLeft')) {
                      e.preventDefault()
                      const n = it.options.length
                      if (!n) return
                      // 设计里 ←↑ 同义、↓→ 同义（parts/ask.js:497-498）：网格里两个轴都该走
                      const fwd = e.key === 'ArrowDown' || e.key === 'ArrowRight'
                      const next = cursor < 0 ? 0 : (cursor + (fwd ? 1 : -1) + n) % n
                      setCursor(next)
                      return
                    }
                    if (!inInput) {
                      const k = Number(e.key)
                      if (Number.isInteger(k) && k >= 1 && k <= Math.min(9, it.options.length)) {
                        e.preventDefault()
                        const o = it.options[k - 1]
                        setCursor(k - 1)
                        if (it.multi) toggle(it.id, o.label)
                        else setAnswer(it.id, o.label)
                        return
                      }
                    }
                    if (e.key === 'Escape') {
                      // Esc = 跳过这批（设计 ask.js:508）：未答的题按空答案交，不是关掉面板
                      e.preventDefault()
                      submitAll(true)
                      return
                    }
                    if (e.key === 'Enter') {
                      // ⏎：光标处还没选就先选它，已选就下一问/提交
                      if (!inInput && cursor >= 0 && !here) {
                        const o = it.options[cursor]
                        if (o) {
                          e.preventDefault()
                          if (it.multi) toggle(it.id, o.label)
                          else setAnswer(it.id, o.label)
                          return
                        }
                      }
                      if (!inInput) {
                        e.preventDefault()
                        goNext()
                      }
                    }
                  }}
                >
                  {items.length > 1 && (
                    <div className="ask-step">
                      <span>{t('ask.stepOf').replace('{i}', String(at + 1)).replace('{n}', String(items.length))}</span>
                      <span className="ask-dots" aria-hidden="true">
                        {items.map((_, k) => (
                          <i key={k} data-on={k < at ? 'past' : k === at ? 'now' : 'next'} />
                        ))}
                      </span>
                    </div>
                  )}
                  <div className="ask-head">
                    <span className="ask-ic" aria-hidden="true">
                      <svg viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.5">
                        <circle cx="6" cy="6" r="4.5" />
                        <path d="M4.85 4.7a1.2 1.2 0 1 1 1.55 1.14c-.3.12-.4.36-.4.66" />
                        <path d="M6 8.3h.01" />
                      </svg>
                    </span>
                    <div className="ask-q" id={`ask-q-${it.id}`}>{it.question}</div>
                  </div>
                  <div className="ask-body">
                    {it.options.length > 0 && renderOptions(it, at, cursor)}
                  </div>
                  <input
                    ref={firstRef}
                    name={`question-answer-${at}`}
                    value={answers[it.id] ?? ''}
                    onChange={(e) => setAnswer(it.id, e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key !== 'Enter') return
                      if (e.nativeEvent.isComposing || e.keyCode === 229) return
                      e.preventDefault()
                      goNext()
                    }}
                    placeholder={it.multi ? t('ask.placeholderExtra') : it.options.length ? t('ask.placeholderOptions') : t('ask.placeholder')}
                    aria-label={t('ask.inputAria').replace('{n}', String(at + 1))}
                    spellCheck={false}
                  />
                  <div className="ask-foot">
                    <span className="ask-note">
                      {cd && cd.leftMs === 0
                        ? t('ask.timedOutHint')
                        : t('ask.stepNote').replace('{done}', String(answered)).replace('{total}', String(items.length))}
                    </span>
                    <div className="ask-acts">
                      {/* 单选卡（非分步）按设计给三样：多选的全选/清空、跳过、继续。
                          分步批次只给「上一步 / 下一问」，因为中途跳过会让后面几问失去
                          上下文（设计 parts/ask.js:190-208 也是这么分的）。 */}
                      {items.length === 1 && it.multi && it.options.length > 1 && (() => {
                        const picked = multiSel[it.id] ?? []
                        const allPicked = it.options.length > 0 && it.options.every((o) => picked.includes(o.label))
                        return (
                          <button
                            type="button"
                            className="ask-all"
                            onClick={() => {
                              // 全选 ⇄ 清空：一个按钮两种文案（设计 ask.js:428-435）
                              it.options.forEach((o) => {
                                const has = picked.includes(o.label)
                                if (allPicked ? has : !has) toggle(it.id, o.label)
                              })
                              setCursor(-1)
                            }}
                          >
                            {allPicked ? t('ask.clearAll') : t('ask.selectAll')}
                          </button>
                        )
                      })()}
                      {items.length === 1 ? (
                        <button type="button" className="ask-btn ghost" onClick={() => submitAll(true)}>
                          {t('ask.skip')}
                        </button>
                      ) : (
                        <button
                          type="button"
                          className="ask-btn ghost"
                          disabled={at === 0}
                          onClick={() => {
                            setStep(at - 1)
                            setCursor(-1)
                          }}
                        >
                          {t('ask.back')}
                        </button>
                      )}
                      <button type="button" className="ask-btn go" disabled={!here || cd?.leftMs === 0} onClick={goNext}>
                        {last ? t('ask.done') : t('ask.next')}
                      </button>
                    </div>
                  </div>
                </div>
              </div>
            </>
          )}
        </div>
      </div>
    )
  }

  return (
    // 非模态：role="region"（不是 dialog/aria-modal —— 叙述区与左右栏都还能用）
    <div className={`dock${rt ? ' ask-scope' : ''}${folded ? ' folded' : ''}`} role="region" aria-label={t('ask.title')}>
      {/* 壳不再是卡片：这档皮肤下一张卡 = 一个问题（.q-item.ask），
          批次里问三条就是三张卡，壳只负责折叠与倒计时 */}
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
                <div
                  className={rt ? 'ask' : 'q-item'}
                  key={it.id}
                  data-form={it.multi ? 'multi' : previewFor(it) ? 'preview' : 'plain'}
                  onKeyDown={(e) => {
                    // 数字键直选（kbd 列给出的承诺）：只认 1-9、不带修饰键、
                    // 且焦点不在自由输入框里（那里数字是正文）。
                    if (e.metaKey || e.ctrlKey || e.altKey) return
                    if ((e.target as HTMLElement).tagName === 'INPUT') return
                    const n = Number(e.key)
                    if (!Number.isInteger(n) || n < 1 || n > Math.min(9, it.options.length)) return
                    const opt = it.options[n - 1]
                    e.preventDefault()
                    if (it.multi) toggle(it.id, opt.label)
                    else setAnswer(it.id, opt.label)
                  }}
                >
                  <div className={rt ? 'ask-head' : 'q-title'}>
                    {/* 设计的问题是「一个图标 + 一句话」两列（--ask-lead 26px），
                        不是带序号的徽标；批次里的序号由 kbd 列承担 */}
                    <span className={rt ? 'ask-ic' : 'q-no'} aria-hidden="true">
                      {rt ? (
                        <svg viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.5">
                          <circle cx="6" cy="6" r="4.5" />
                          <path d="M4.85 4.7a1.2 1.2 0 1 1 1.55 1.14c-.3.12-.4.36-.4.66" />
                          <path d="M6 8.3h.01" />
                        </svg>
                      ) : (
                        i + 1
                      )}
                    </span>
                    <span className={rt ? 'ask-q' : 'q-text'}>{it.question}</span>
                  </div>
                  {/* 设计的卡分三段：head / body / foot。body 自带 12px 6px 内距，
                      没有它选项会贴到卡的边框上 */}
                  <div className={rt ? 'ask-body' : undefined}>
                  {it.options.length > 0 && renderOptions(it, i)}
                  </div>
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
            <div className={rt ? 'ask-foot' : 'acts'}>
              <span className="timeout">
                {cd && cd.leftMs === 0 ? t('ask.timedOutHint') : t('ask.allRequired')}
              </span>
              <button
                className={rt ? 'ask-btn go' : 'btn primary'}
                disabled={!allAnswered || (cd?.leftMs === 0)}
                onClick={() => submitAll()}
              >
                {t('ask.submitAll').replace('{answered}', String(answered)).replace('{total}', String(items.length))}
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  )
}
