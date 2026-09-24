import { memo, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactElement } from 'react'
import { useAppStore, genMsOf } from '../store/useAppStore'
import type { MsgBlock } from '../store/useAppStore'
import { Markdown } from './markdown'
// 过程块的渲染原语（思考 / 工具行 / 计划卡 / 压缩块 / 行内码）与子 agent 转录共用一份
// —— 见 NarrativeBlocks.tsx 的文件头（两处宿主、一套 DOM、一套皮肤）。
import { CompressionBlock, InlineCode, Thinking, ToolGroup } from './NarrativeBlocks'
// 任务卡外壳（子 agent 卡）：与右栏「任务」tab 的同类卡共用同一份实现，见 TaskCard.tsx
import { AgentTaskCard } from './TaskCard'
import HaiLogo from './HaiLogo'
import { CheckCircleIcon, PaperclipIcon } from './icons'
import { fmtDuration, fmtRun, fmtClock, fmtUsd } from './ui'
import { useClock } from '../lib/useNow'
import { ArtifactCard } from './ArtifactCard'
import { UsageActivityMini } from './UsageStats'
import { useT } from '../i18n'

function taskDeliveryLabel(status: Extract<MsgBlock, { kind: 'task_delivery' }>['status'], t: (k: string) => string) {
  switch (status) {
    case 'completed': return t('task.status.completed')
    case 'interrupted': return t('task.status.interrupted')
    case 'failed': return t('task.status.failed')
    case 'abandoned': return t('task.status.abandoned')
  }
}

// 异步任务完成通知（task_result_delivered 的轻量回执）：默认折叠，展开后查看
// 完整执行结果或错误（与工具调用卡片同构——先看状态行，需要时再展开全文）。
const TaskDeliveryBlock = memo(function TaskDeliveryBlock({ b }: { b: Extract<MsgBlock, { kind: 'task_delivery' }> }) {
  const t = useT()
  const [open, setOpen] = useState(false)
  const isErr = b.status === 'failed' || Boolean(b.error)
  const hasBody = Boolean(b.result || b.error)
  const expanded = open && hasBody
  return (
    <div className={`msg system ${isErr ? 'error' : b.status === 'completed' ? 'success' : 'info'}${expanded ? ' task-delivery-open' : ''}`} role="status">
      <span className="sys-dot" aria-hidden="true" />
      <button
        className="task-delivery-toggle"
        onClick={() => hasBody && setOpen((v) => !v)}
        aria-expanded={hasBody ? open : undefined}
        aria-label={open ? t('task.result.collapse') : t('task.result.expand')}
      >
        <span className="mono">{hasBody ? (open ? '▾' : '▸') : ''} <InlineCode text={`\`${b.taskId}\``} /> {taskDeliveryLabel(b.status, t)} · {t('task.result.notified')}</span>
      </button>
      {expanded && (
        <pre className={isErr ? 'task-result-err' : 'task-result-ok'}>{b.error || b.result}</pre>
      )}
    </div>
  )
})

// 后台任务结果（自动回传 task_result_delivered）：可折叠 Markdown 块（类似思考过程）——
// 默认折叠：`▸ 任务结果 · task-3 · ✓/✗`；展开渲染全文（复用 Markdown + shiki 管线）。
const TaskResultBlock = memo(function TaskResultBlock({ b }: { b: Extract<MsgBlock, { kind: 'task_result' }> }) {
  const [open, setOpen] = useState(false)
  const t = useT()
  const isErr = Boolean(b.error)
  return (
    <div className={`task-result ${isErr ? 'err' : 'ok'}`} key={b.id}>
      <button
        className="task-result-toggle"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-label={open ? t('task.result.collapse') : t('task.result.expand')}
      >
        <span className="mono">{open ? '▾' : '▸'} {t('task.result')} · {b.taskId} · {isErr ? '✗' : '✓'}</span>
      </button>
      {open && (
        <div className="task-result-body">
          {isErr ? <div className="task-result-err">{b.error}</div> : <Markdown text={b.result} />}
        </div>
      )}
    </div>
  )
})

// 压缩结果：和工具调用一样保留在对话流中，默认展示结果概要，展开后给出完整用量与上下文变化。
// 导出：右栏任务 Tab 的主压缩镜像复用同一组件（同源，不建第二套渲染）。

// RetryProgress 重试实时状态（2026-09-18）：退避窗口内显示「Ns 后开始重试」，下一次
// 尝试开始后显示「本次尝试已进行 Xs」。
//
// 为什么必须有它：llm_error 的 Attempt 语义是「已失败的那一次」，而两次失败之间可能很长
// ——实测一次尝试静默挂了 187s（无 llm_start、无报错、无任何事件），期间界面只有静态文案
// 「正在重试 1…」，用户读到的是「卡住/重试机制没了」。这一行是那段黑箱里唯一能证明
// 「在重试、且已经等了多久」的信息。
//
// 计算依据全部来自块上落库的两个静态值：ts（失败时刻）与 retryDelayMs（退避时长）；
// 下一次尝试起点 = ts + retryDelayMs。订阅 1s 全局时钟（与 ElapsedTimer 同款单时钟模式，
// 只有本小组件随秒重渲），重试结束（块被清除/替换）即卸载。
// costUsd：本轮重试**已花费**金额（USD，2026-09-23）—— 失败尝试的已产生用量（上游按已生成
// token 计费）折算而来。重试进行中显示在计时行尾；重试结束后（成功/耗尽/中止）计时行消失，
// 但「重试已花费」保留（那是真花掉的钱，不该随重试结束一起消失）。
function RetryProgress({ ts, delayMs, retrying, startedAt, costUsd }: { ts?: number; delayMs?: number; retrying?: boolean; startedAt?: number; costUsd?: number }) {
  const t = useT()
  const startAt = ts != null ? ts + (delayMs ?? 0) : undefined
  const live = useClock(startAt, Boolean(retrying) && startAt != null)
  const costLine = costUsd && costUsd > 0 ? t('narrative.retryCost').replace('{cost}', fmtUsd(costUsd)) : ''
  if (!retrying || startAt == null || live == null) {
    return costLine ? <span className="err-retry mono">{costLine}</span> : null
  }
  // 累计「已等待时长」= 本轮重试首个失败至今（用同一个时钟值换算，不额外订阅）。
  const waited = startedAt != null ? live + (startAt - startedAt) : undefined
  const main = live < 0
    ? t('narrative.retryCountdown').replace('{n}', String(Math.max(1, Math.ceil(-live / 1000))))
    : t('narrative.retryElapsed').replace('{t}', fmtRun(live))
  // 首轮失败时「已等待」与「本次尝试」几乎相同（无信息量）→ 只在本轮确实重过之后才追加
  const showWaited = waited != null && waited - Math.max(live, 0) > 1500
  return (
    <span className="err-retry mono">
      {main}
      {showWaited ? ` · ${t('narrative.retryWaited').replace('{t}', fmtRun(waited))}` : ''}
      {costLine ? ` · ${costLine}` : ''}
    </span>
  )
}

export default function Narrative({ viewSid }: { viewSid?: string } = {}) {
  const t = useT()
  // 指定 viewSid（如 cron 详情页专用 view）→ 渲染该 view 的 blocks；缺省 = 活跃会话
  const blocks = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocks ?? [] : s.blocks))
  const persona = useAppStore((s) => s.persona)
  const activeSessionId = useAppStore((s) => s.activeSessionId)
  const ref = useRef<HTMLDivElement>(null)
  const anchorRef = useRef<HTMLDivElement>(null)
  // 最新一张待办工具卡（todo_add / todo_update）：仅它展开并实时反映 todos 快照
  // （倒序扫第一个即最新；旧卡随 live 翻 false 自动折叠，见 TodoToolCard）
  const liveTodoId = useMemo(() => {
    for (let i = blocks.length - 1; i >= 0; i--) {
      const b = blocks[i]
      if (b.kind === 'tool' && (b.name === 'todo_add' || b.name === 'todo_update')) return b.toolId
    }
    return ''
  }, [blocks])

  // —— 智能置底（2026-08-22）：流式输出期间用户上滑浏览历史时，不再被新块拽回底部 ——
  // stickRef：是否贴底。纯 ref（滚动是高频事件，绝不逐 tick setState）；
  // 只有「跨过」底部边界时才 setState（away/fresh 指示器显隐，一次手势最多一次重渲）。
  // 离开底部 → 停自动滚动（也不再做任何强制布局）；新块只悄悄标记 fresh（按钮加圆点）；
  // 回到底部附近 / 点击按钮 / 切换会话 → 恢复贴底。
  const stickRef = useRef(true)
  // 用户手势标记：wheel / touchstart 置位（真实用户滚动输入）。程序滚动（校准
  // 循环赋值 scrollTop）不置位 —— 其 scroll 事件被 onScroll 忽略，stickRef 不受
  // 干扰。这解决了两个问题：
  // ① 流式中每帧程序钉底 → 若用「scroll 事件判定」，用户上滑事件总被程序滚动
  //    淹没/误判，永远无法离开底部（实测上滑被吞、按钮不出现）；
  // ② 切会话时内容暴涨触发 scrollTop 钳制（无用户手势）→ 不会误置 stickRef=false
  //    → 新会话内容到达后仍正常跟随。
  const userGestureRef = useRef(false)
  const [away, setAway] = useState(false) // 用户已离开底部（显示「回到底部」按钮）
  const [fresh, setFresh] = useState(false) // 离开期间有新内容到达（按钮带圆点）
  const BOTTOM_THRESHOLD = 120 // px：视为「贴底」的容差（content-visibility 高度为估算值）
  const isNearBottom = () => {
    const el = ref.current
    if (!el) return true
    return el.scrollHeight - el.scrollTop - el.clientHeight < BOTTOM_THRESHOLD
  }
  // 程序滚动辅助：直接赋值（不置任何标记 —— 其 scroll 事件因无用户手势被忽略）
  const setScrollTop = (el: HTMLElement, v: number) => {
    if (el.scrollTop === v) return
    el.scrollTop = v
  }
  // 用户手势监听：wheel（鼠标/触摸板滚动）+ touchstart（触屏）。置位后下一个
  // scroll 事件消费它并更新 stickRef；无手势的 scroll（程序/钳制）一律忽略。
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const mark = () => { userGestureRef.current = true }
    const onScroll = () => {
      if (!userGestureRef.current) return // 非用户手势的 scroll（程序滚动/钳制）→ 忽略
      userGestureRef.current = false
      const nb = isNearBottom()
      if (nb === stickRef.current) return
      stickRef.current = nb
      setAway(!nb)
      if (nb) setFresh(false)
    }
    const onKey = (e: KeyboardEvent) => {
      // 键盘滚动键（方向键/翻页/空格）→ 算用户滚动意图
      if (['ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End', ' '].includes(e.key)) {
        mark()
      }
    }
    el.addEventListener('wheel', mark, { passive: true })
    el.addEventListener('touchstart', mark, { passive: true })
    el.addEventListener('keydown', onKey, { passive: true })
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => {
      el.removeEventListener('wheel', mark)
      el.removeEventListener('touchstart', mark)
      el.removeEventListener('keydown', onKey)
      el.removeEventListener('scroll', onScroll)
    }
  }, [])
  // 内容变化（流式 / 异步渲染撑高 / 快照到达）：贴底状态下持续校准到最新底部。
  // 为什么不能「内容稳定就停」：mermaid SVG / 图片 / shiki 高亮等**异步渲染**
  // 在 blocks 稳定后仍会陆续撑高 scrollHeight（实测流式结束后 mermaid 2→9 个，
  // sh 4113→5301，gap 涨到 1766px 不再追）。所以校准循环只要贴底就常驻。
  // 关键：tick 只在「内容高度（scrollHeight）增长」时把 scrollTop 钉到新底部，
  // 用户主动上滑不改变 scrollHeight → tick 不会拽回（可自由浏览历史）；
  // 内容一旦继续增长 → 若用户已离开底部（stickRef=false）保持不打扰；
  // 若用户仍在底部 → 顺滑跟随最新内容。stickRef 由用户手势驱动的 scroll
  // 事件维护（wheel/touch 置位；程序滚动不置位，见上方 onScroll）。
  useEffect(() => {
    const el = ref.current
    if (!el) return
    if (!stickRef.current) {
      setFresh((f) => (blocks.length ? true : f)) // 离开底部期间有 blocks 更新 → fresh（按钮圆点）
      return
    }
    let raf = 0
    let lastMax = -1
    const tick = () => {
      raf = 0
      const max = el.scrollHeight - el.clientHeight
      const grew = max !== lastMax
      lastMax = max
      if (grew) {
        // 内容高度变化：用户仍在贴底状态 → 钉到最新底部（流式/异步渲染跟随）
        if (stickRef.current && max > 0 && el.scrollTop !== max) setScrollTop(el, max)
      }
      raf = requestAnimationFrame(tick)
    }
    raf = requestAnimationFrame(tick)
    return () => { if (raf) cancelAnimationFrame(raf) }
  }, [blocks])
  // 会话切换：重置贴底状态。内容到达后的持续校准由 blocks effect 统一负责
  //（覆盖 content-visibility 慢布局：rAF 循环直到内容高度稳定）。
  // 这里只重置 + 若新会话内容已在（缓存切换），先滚一次减少等待。
  useEffect(() => {
    stickRef.current = true
    setAway(false)
    setFresh(false)
    const el = ref.current
    if (el) setScrollTop(el, el.scrollHeight - el.clientHeight)
  }, [activeSessionId])
  const jumpToBottom = () => {
    stickRef.current = true
    setAway(false)
    setFresh(false)
    anchorRef.current?.scrollIntoView({ block: 'end', behavior: 'auto' })
  }

  // 渲染单个非工具块（工具已在上面归组处理）
  const renderBlock = (b: Exclude<MsgBlock, { kind: 'tool' }>): ReactElement | null => {
    if (b.kind === 'divider') {
      // 上下文隔离横线（shutdown/abort/clear）：纯分隔线，无文本；title 区分语义（内部）
      return (
        <div
          className={`msg divider ${b.reason ?? ''}`}
          key={b.id}
          aria-hidden="true"
          title={b.reason === 'shutdown' ? t('narrative.dividerShutdown') : b.reason === 'abort' ? t('narrative.dividerAbort') : b.reason === 'clear' ? t('narrative.dividerClear') : undefined}
        >
          <span className="divider-line" />
        </div>
      )
    }
    if (b.kind === 'task_result') return <TaskResultBlock key={b.id} b={b} />
    if (b.kind === 'task_delivery') return <TaskDeliveryBlock key={b.id} b={b} />
    if (b.kind === 'compression') return <CompressionBlock key={b.id} b={b} />
    if (b.kind === 'system') {
      return (
        <div className={`msg system ${b.tone || 'info'}`} key={b.id} role="status">
          <span className="sys-dot" aria-hidden="true" />
          <span className="body"><InlineCode text={b.text} /></span>
        </div>
      )
    }
    if (b.kind === 'user') {
      // 图文消息：文本 + 图片附件缩略图（data URL 直显）
      const imgs = b.contents?.filter((c) => c.type === 'image') ?? []
      // @引用展开状态行（refs_loaded 事件）：loaded → 勾 + 已加载；missing/blocked → 警示
      const refs = b.refs?.length
        ? (() => {
            const loaded = b.refs!.filter((r) => r.status === 'loaded')
            const missing = b.refs!.filter((r) => r.status === 'missing')
            const blocked = b.refs!.filter((r) => r.status === 'blocked')
            // 有缺失/受保护时逐条列出；全成功时合并计数
            const issues = [...missing.map((r) => ({ ...r, label: t('composer.refs.missing') })), ...blocked.map((r) => ({ ...r, label: t('composer.refs.blocked') }))]
            return { loaded, issues, allLoaded: issues.length === 0 }
          })()
        : undefined
      return (
        <div className="msg user" key={b.id}>
          <div className="who"><b>{t('narrative.user')}</b>{b.ts != null && <span className="msg-ts mono">{fmtClock(b.ts)}</span>}</div>
          <div className="body"><p><InlineCode text={b.text} /></p></div>
          {imgs.length > 0 && (
            <div className="user-imgs">
              {imgs.map((c, i) => (
                <img key={i} src={c.content} alt={t('narrative.attachAlt')} />
              ))}
            </div>
          )}
          {refs && (
            <div className="user-refs" role="status" aria-label={t('composer.refs.aria')}>
              {refs.loaded.length > 0 && (
                <span className={`user-refs-line ${refs.allLoaded ? 'ok' : ''}`}>
                  <CheckCircleIcon size={12} />
                  {refs.allLoaded
                    ? t('composer.refs.loaded').replace('{n}', String(refs.loaded.length))
                    : t('composer.refs.loadedList').replace('{list}', refs.loaded.map((r) => r.path).join('、'))}
                </span>
              )}
              {refs.issues.map((r, i) => (
                <span className="user-refs-line warn" key={i}>
                  <PaperclipIcon size={12} />
                  <code>{r.path}</code> {r.label}
                </span>
              ))}
            </div>
          )}
        </div>
      )
    }
    if (b.kind === 'agent') return <AgentTaskCard key={b.id} b={b} />
    // 本轮产出汇总（AgentEnd.Artifacts）：对话页尾部的「本次产出」卡片。
    // 位于本轮最终回复之后（AgentEnd 到达时追加），与截图里的 artifact 卡片同位置。
    if (b.kind === 'artifacts') {
      return (
        <div className="msg artifacts" key={b.id}>
          <ArtifactCard items={b.items} {...(b.truncated ? { truncated: true } : {})} />
        </div>
      )
    }
    // 运行失败反馈（llm_error / session_run_error）：醒目错误块（含 raw 错误串）。
    // 重试期间下方追加 RetryProgress（退避倒计时 → 本次尝试已进行 Xs）。
    if (b.kind === 'error') {
      return (
        <div className="msg error" key={b.id} role="alert">
          <div className="who"><b>{t('narrative.error')}</b></div>
          <div className="body">
            <p className="err-text"><InlineCode text={b.text} /></p>
            <RetryProgress ts={b.ts} delayMs={b.retryDelayMs} retrying={b.retrying} startedAt={b.retryStartedAt} costUsd={b.retryCostUsd} />
          </div>
        </div>
      )
    }
    if (b.kind !== 'assistant') return null
    // 空轮（纯工具调用、无文本无思考且未流式）不渲染空消息
    if (!b.text && !b.thinking && !b.streaming) return null
    return (
      <div className="msg assistant" key={b.id}>
        <div className="who">
          <HaiLogo className="who-hai" height={16} />
          {b.model && <span className="mono" style={{ fontSize: 11, color: 'var(--text-3)' }}>{b.model}</span>}
          {b.ts != null && <span className="msg-ts mono">{fmtClock(b.ts)}</span>}
        </div>
        <div className="body">
          {b.thinking && <Thinking text={b.thinking} live={b.streaming} thinkMs={b.thinkMs} thinkStart={b.thinkStart} />}
          {b.text || b.streaming ? (
            <div className="md-text">
              {b.text ? <Markdown text={b.text} /> : null}
              {!b.streaming && b.ttftMs != null && b.durMs != null && (
                <div className="assistant-turn-meta mono">
                  {t('narrative.ttft').replace('{ttft}', fmtDuration(b.ttftMs)).replace('{dur}', fmtDuration(b.durMs))}
                  {b.agentDurMs != null ? ` · ${t('narrative.agentDur').replace('{dur}', fmtDuration(b.agentDurMs))}` : ''}
                  {(() => {
                    // 输出速度 = outputTokens / 生成窗口。分母口径见 store 的 genMsOf：
                    // 分子含 reasoning token，故思考未流式时必须用整轮 durMs——否则
                    // reasoning 模型会算出几十倍虚高（DeepSeek 把 reasoning 塞在末次
                    // usage、流里没有 reasoning chunk 时，durMs - ttftMs 只剩正文的
                    // 零点几秒）。刷新后 genMs 未持久化，用同一纯函数重算，数字一致。
                    const out = b.outputTokens
                    if (out == null || out <= 0) return null
                    const genMs = b.genMs ?? genMsOf(b)
                    if (genMs == null || genMs <= 0) return null
                    return t('narrative.tokensPerSec').replace('{speed}', (out / (genMs / 1000)).toFixed(2))
                  })()}
                </div>
              )}
              {b.streaming && <span className="mono" style={{ color: 'var(--running)' }}>▍</span>}
            </div>
          ) : null}
        </div>
      </div>
    )
  }

  // 连续工具块归组（并行工具上下排列），其余原样
  const items: (ReactElement | null)[] = []
  for (let i = 0; i < blocks.length; i++) {
    const b = blocks[i]
    if (b.kind === 'tool') {
      const group: Extract<MsgBlock, { kind: 'tool' }>[] = [b]
      while (i + 1 < blocks.length && blocks[i + 1].kind === 'tool') {
        group.push(blocks[i + 1] as Extract<MsgBlock, { kind: 'tool' }>)
        i++
      }
      items.push(<ToolGroup key={group[0].id} tools={group} liveTodoId={liveTodoId} />)
    } else {
      items.push(renderBlock(b))
    }
  }

  return (
    <div className="narrative" ref={ref} aria-live="polite">
      {blocks.length === 0 && (
        <EmptyChat persona={persona} />
      )}
      {items}
      <div className="narrative-anchor" ref={anchorRef} aria-hidden="true" />
      {(away || fresh) && (
        <button
          className="narrative-jump"
          onClick={jumpToBottom}
          aria-label={t('narrative.backToLatest')}
          title={fresh ? t('narrative.newArrived') : t('narrative.backToLatest')}
        >
          {fresh && <span className="narrative-jump-dot" aria-hidden="true" />}
          ↓ {t('narrative.backToLatest')}
        </button>
      )}
    </div>
  )
}

// EmptyChat 空会话引导：按 Persona 区分提示（Code 工程师 / Work 办公助理）+ 近 90 天 Token 活动热力图。
// 图表是「这里能干活」的实证（有历史才有数据；无记录时 UsageActivityMini 自行不渲染）。
function EmptyChat({ persona }: { persona: 'code' | 'work' }) {
  const t = useT()
  const isWork = persona === 'work'
  return (
    <div className={`empty ${isWork ? 'work' : 'code'}`}>
      <div className="mark">{isWork ? '◇' : '⌘'}</div>
      <div className="big">{isWork ? t('narrative.emptyWorkTitle') : t('narrative.emptyCodeTitle')}</div>
      <div className="hint">
        {isWork
          ? t('narrative.emptyWorkDesc')
          : t('narrative.emptyCodeDesc')}
      </div>
      <div className="examples">
        {(isWork
          ? [t('narrative.suggestWork1'), t('narrative.suggestWork2'), t('narrative.suggestWork3')]
          : [t('narrative.suggestCode1'), t('narrative.suggestCode2'), t('narrative.suggestCode3')]
        ).map((e) => <span key={e}>{e}</span>)}
      </div>
      <UsageActivityMini />
    </div>
  )
}
