import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
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

// 助手块底部的本轮指标行：TTFT / LLM 总耗时 / Agent 总耗时 / 输出速度。
// 2026-09-29：四个指标**各自独立**渲染。此前整行挂在 `ttftMs != null && durMs != null`
// 一个门上 —— 中断、run 级失败、达轮数上限截断这三种收尾都没有 llm_end（durMs 的唯一
// 来源），于是 durMs 缺失把整行藏掉，连已经挂在块上的 Agent 总耗时、已经拿到的 usage
// 一起消失，用户读作「AgentEnd 时前端没展示耗时信息」。现在缺哪段省哪段，取不到就不显示。
const AssistantTurnMeta = memo(function AssistantTurnMeta({ b }: { b: Extract<MsgBlock, { kind: 'assistant' }> }) {
  const t = useT()
  // 输出速度 = outputTokens / 生成窗口。分母口径见 store 的 genMsOf：
  // 分子含 reasoning token，故思考未流式时必须用整轮 durMs——否则 reasoning 模型会
  // 算出几十倍虚高（DeepSeek 把 reasoning 塞在末次 usage、流里没有 reasoning chunk 时，
  // durMs - ttftMs 只剩正文的零点几秒）。刷新后 genMs 未持久化，用同一纯函数重算，数字一致。
  let speed: string | null = null
  const out = b.outputTokens
  if (out != null && out > 0) {
    const genMs = b.genMs ?? genMsOf(b)
    if (genMs != null && genMs > 0) speed = t('narrative.tokensPerSec').replace('{speed}', (out / (genMs / 1000)).toFixed(2))
  }
  const head = b.ttftMs != null && b.durMs != null
    ? t('narrative.ttft').replace('{ttft}', fmtDuration(b.ttftMs)).replace('{dur}', fmtDuration(b.durMs))
    : b.ttftMs != null
      ? t('narrative.ttftOnly').replace('{ttft}', fmtDuration(b.ttftMs))
      : b.durMs != null
        ? t('narrative.llmDurOnly').replace('{dur}', fmtDuration(b.durMs))
        : null
  // 一段都拿不到（既无耗时也无速度）→ 整行不渲染，不留空壳。
  if (!head && !speed) return null
  return (
    <div className="assistant-turn-meta mono">
      {head ?? ''}
      {b.agentDurMs != null ? `${head || speed ? ' · ' : ''}${t('narrative.agentDur').replace('{dur}', fmtDuration(b.agentDurMs))}` : ''}
      {speed ?? ''}
    </div>
  )
})

export default function Narrative({ viewSid }: { viewSid?: string } = {}) {
  const t = useT()
  // 指定 viewSid（如 cron 详情页专用 view）→ 渲染该 view 的 blocks；缺省 = 活跃会话
  const blocks = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocks ?? [] : s.blocks))
  const persona = useAppStore((s) => s.persona)
  const activeSessionId = useAppStore((s) => s.activeSessionId)
  const ref = useRef<HTMLDivElement>(null)
  const anchorRef = useRef<HTMLDivElement>(null)
  // —— 主对话分页（2026-10-20）——
  // 后端 BlocksPage 的游标：最早已加载块的 id 与「还有更早」标记。
  // 注意这两个值来自 store（由 hydrateCheckpoint 写入），不是本地 state ——
  // 会话切换时随视图一起重置，避免把上一个会话的游标带到新会话。
  const total = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocksTotal ?? 0 : s.blocksTotal))
  const moreCursor = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocksCursor ?? 0 : s.blocksCursor))
  const hasMore = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocksHasMore ?? false : s.blocksHasMore))
  // 按钮用的 loading/error 走 store（不是本地 state）：响应由 store 的
  // blocks_page 处理器写回，本地 state 无法感知「已经加载完了」，
  // 会出现按钮一直转圈/错误态不消。store 是唯一事实源。
  const olderLoading = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocksOlderLoading ?? false : s.blocksOlderLoading))
  const olderError = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocksOlderError ?? false : s.blocksOlderError))
  // 会话进入序号：每次 checkpoint hydrate 变化 → 重播历史渐入（见 store 的 enteredAt 注释）
  const enteredAt = useAppStore((s) =>
    viewSid ? s.views[viewSid]?.enteredAt ?? 0 : (s.activeSessionId ? s.views[s.activeSessionId]?.enteredAt ?? 0 : 0),
  )
  // enterAnimAt：数据到达那一刻（与骨架屏卸载同一帧写入）。
  // 用它决定「什么时候播渐入」——而不是自己起一个定时器猜。
  const enterAnimAt = useAppStore((s) => s.enterAnimAt)
  // 骨架屏是否参与过这次切换：只有它出现过，内容渐入才需要延后 90ms 让位
  //（避免两层半透明叠加）。纯快速切换（骨架屏从未出现）则不加延迟。
  const skeletonPhase = useAppStore((s) => s.skeleton.phase)
  const crossFade = skeletonPhase === 'exit' || skeletonPhase === 'visible'

  // —— 历史分批铺开（真正意义上的「缓慢展示」）——
  //
  // 长会话首屏有 200 个块，一次性全挂 DOM 会让主线程卡住：用户看到的是
  // 骨架屏消失后**整块内容突然凝固**几百毫秒，然后才能滚动。
  // 这里改成按帧递增可见块数：「先出最近一屏，再往上续」。
  //
  // 为什么从**最新往旧**铺：会话的阅读锚点在底部（最新消息）。用户切过来
  // 第一眼要看的是最后发生了什么，不是最早的。所以先渲染尾部。
  //
  // 节奏：每帧 REVEAL_PER_FRAME 个块（约 60fps → 25ms/批）。
  // 200 块 ≈ 8 批 ≈ 200ms 铺完；而一屏可见约 8~12 块，首批就把屏幕填满了，
  // 后续补的是上方离屏内容（被 content-visibility 跳过布局，几乎无成本）。
  // 这样「铺开动画」与「可交互」几乎同时发生，不会让人觉得在等。
  const REVEAL_PER_FRAME = 25
  const [revealCount, setRevealCount] = useState<number>(Infinity)
  useEffect(() => {
    // 只在「会话刚进入」时铺开：正常追加（流式/新消息）不限制。
    //
    // 触发源只用 enterAnimAt，**不能**用 crossFade —— 后者是
    // skeleton.phase === 'exit' | 'visible'，骨架屏退场完成后 phase 变 idle，
    // crossFade 随之变 false → effect 重跑 → revealCount 被重置为 Infinity
    // → 铺开中途突然全部显示（正是要避免的）。enterAnimAt 是单调的时间戳，
    // 一次会话进入只变一次，天然稳定。
    if (!enterAnimAt) {
      setRevealCount(Infinity)
      return
    }
    const total = blocks.length
    if (total <= REVEAL_PER_FRAME) {
      setRevealCount(Infinity)
      return
    }
    let shown = 0
    let raf = 0
    const step = () => {
      shown += REVEAL_PER_FRAME
      if (shown >= total) {
        setRevealCount(Infinity)
        return
      }
      setRevealCount(shown)
      raf = requestAnimationFrame(step)
    }
    setRevealCount(REVEAL_PER_FRAME)
    raf = requestAnimationFrame(step)
    return () => {
      if (raf) cancelAnimationFrame(raf)
    }
    // 依赖 enterAnimAt：每次会话进入重新铺一遍；blocks.length 变化不重置
    // （否则流式新块会把铺开进度打回起点 —— 那是明显 bug）。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enterAnimAt])
  const loadOlder = useAppStore((s) => s.loadOlderBlocks)
  const onLoadOlder = useCallback(() => {
    if (olderLoading) return
    loadOlder(viewSid)
  }, [olderLoading, loadOlder, viewSid])

  // —— 滚动锚定补偿（仅在「前插历史」时保持视觉位置）——
  //
  // 从顶部前插 N 个块会让 scrollHeight 变大、原有内容整体下移，用户正在读的
  // 那一行会跳走。补偿方式：按高度差把 scrollTop 推回去。
  //
  // ## 为什么必须区分「前插」与「追加」（2026-10-21 修 bug）
  //
  // 上一版只要 blocks.length 变大就补偿，结果**流式输出把用户往下拽**：
  // 用户上滑读历史（stickRef=false），新内容追加在**底部**，scrollHeight 变大，
  // 补偿逻辑执行 scrollTop += grew → 视图被推回底部。
  // 用户感受就是「上划总被拉回去」，实测 0.5s 内被拽 305px。
  //
  // 正确判据是**增长发生在哪一端**：
  //   · 前插（历史）：新块 id 小于原首块 id → 增长在顶部 → 需要补偿；
  //   · 追加（流式/新消息）：新块 id 大于原尾块 id → 增长在底部 → **不能补偿**
  //     （用户正在上方阅读，底部增长不该影响他的位置）。
  //
  // 用 id 而不是 length 判断：id 单调递增，且分页返回的历史块 id 必然更小。
  const anchorRef2 = useRef<{ height: number; top: number; firstID: number } | null>(null)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const prev = anchorRef2.current
    const firstID = blocks.length ? blocks[0].id : 0
    if (!prev) {
      anchorRef2.current = { height: el.scrollHeight, top: el.scrollTop, firstID }
      return
    }
    // 前插的判据：首块 id 变小了（有更早的块插到了前面）。
    // 只是追加（首块 id 不变）→ 什么都不做。
    const prepended = firstID > 0 && prev.firstID > 0 && firstID < prev.firstID
    if (!prepended) {
      anchorRef2.current = { height: el.scrollHeight, top: el.scrollTop, firstID }
      return
    }
    const grew = el.scrollHeight - prev.height
    anchorRef2.current = { height: el.scrollHeight, top: el.scrollTop, firstID }
    // 贴底时由校准循环接管（前插后仍应贴底）；用户在上方阅读时才补偿。
    if (stickRef.current) return
    if (grew > 0) el.scrollTop = prev.top + grew
  }, [blocks])

  // —— 滚动到顶部自动加载（你要求的「滚动加载」）——
  //
  // 与按钮并存：按钮是「知道有更多、主动点」；滚动加载是「一直往上翻，
  // 到底了就自动续上」，长会话翻历史时不必反复找按钮。
  //
  // 触发条件（全部满足）：
  //   · 有更早的块、没在加载中、没有未处理的错误（失败后不自动重试，
  //     否则后端持续失败会变成无限请求循环 —— 交给按钮手动重试）；
  //   · 容器滚到距顶 < 阈值，且最近 400ms 内有用户手势（见下方 onScroll）。
  //     不用纯 scrollTop 判定：切会话/前插补偿/贴底校准都会产生 scroll 事件，
  //     不加手势判据会把它们当成「用户翻到了顶部」而无限拉取历史。
  //
  // 节流：一次加载到响应之间可能连续触发多帧，靠 store 的 blocksOlderLoading
  // 天然节流（在途时 loadOlderBlocks 直接返回）。
  const TOP_THRESHOLD = 320 // px：距顶多远开始预取（提前一屏，滚动更顺）
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const maybeLoad = () => {
      if (!hasMore || olderLoading || olderError) return
      if (!moreCursor) return
      if (el.scrollTop > TOP_THRESHOLD) return
      // 不需要在这里手工记锚点：补偿 effect 自己在每次 blocks 变化时维护
      // anchorRef2（它用首块 id 判断是否前插）。这里再写一次反而会写入
      // 「还没前插」的状态，导致补偿时 grew 算错。
      onLoadOlder()
    }
    const onScroll = () => {
      // 只认用户手势产生的滚动（时间窗内最近有过 wheel/touch/键盘操作）。
      // 程序滚动（贴底校准、锚定补偿）没有手势，不触发预取 —— 否则贴底 tick
      // 的每一帧都可能把历史当成「用户翻到了顶部」而无限拉取。
      //
      // 窗口 400ms：滚动惯性期间的事件都算用户意图；静默超过这个时长的
      // scroll 变化一定是程序触发的。
      if (performance.now() - lastGestureAtRef.current > 400) return
      maybeLoad()
    }
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => el.removeEventListener('scroll', onScroll)
  }, [hasMore, olderLoading, olderError, moreCursor, onLoadOlder, blocks.length])
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
  // 最近一次用户手势的时刻。
  //
  // 为什么不能复用 userGestureRef 做「用户滚动了吗」的判据：那是**一次性标记**，
  // 第一个 scroll 监听器消费后即清空（见下方 onScroll）。分页预取需要的是
  // 「这帧 scroll 是否源于用户」，用一个带时限的时间戳更稳，也避免两个监听器
  // 抢同一个标记导致贴底逻辑失灵。
  const lastGestureAtRef = useRef(0)
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
    const mark = () => {
      userGestureRef.current = true
      lastGestureAtRef.current = performance.now()
    }
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
    // 历史铺开期间**不启动**校准循环（2026-10-21）。
    //
    // 铺开是从尾部向上补块：每批都会让 scrollHeight 增长、且新块插在上方。
    // 校准循环会把 scrollTop 钉到底部，与铺开+锚定补偿争夺同一个属性，
    // 表现为每补一批就抖一下。铺开结束后 effect 因 revealCount 变化重跑，
    // 校准循环自然接管（此时内容已完整，一次到底即可）。
    if (revealCount !== Infinity) return
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
  }, [blocks, revealCount])
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
              {/* streaming 透传：流式中 HTML 围栏不挂 iframe（半截 <script> 会被执行、
                  且 srcDoc 每 token 重载 = 白板闪烁，见 HtmlPreviewBlock 注释）。 */}
              {b.text ? <Markdown text={b.text} streaming={b.streaming} /> : null}
              {!b.streaming && (
                <AssistantTurnMeta b={b} />
              )}
              {b.streaming && <span className="mono" style={{ color: 'var(--running)' }}>▍</span>}
            </div>
          ) : null}
        </div>
      </div>
    )
  }

  // 铺开进度：只渲染尾部 revealCount 个（见上方「历史分批铺开」）。
  // 从尾部开始，是因为阅读锚点在底部（最新消息）。
  const visibleBlocks = revealCount >= blocks.length ? blocks : blocks.slice(blocks.length - revealCount)

  // 连续工具块归组（并行工具上下排列），其余原样
  const items: (ReactElement | null)[] = []
  for (let i = 0; i < visibleBlocks.length; i++) {
    const b = visibleBlocks[i]
    if (b.kind === 'tool') {
      const group: Extract<MsgBlock, { kind: 'tool' }>[] = [b]
      while (i + 1 < visibleBlocks.length && visibleBlocks[i + 1].kind === 'tool') {
        group.push(visibleBlocks[i + 1] as Extract<MsgBlock, { kind: 'tool' }>)
        i++
      }
      items.push(<ToolGroup key={group[0].id} tools={group} liveTodoId={liveTodoId} />)
    } else {
      items.push(renderBlock(b))
    }
  }

  return (
    <div
      className={`narrative narrative-enter${crossFade ? ' is-crossfade' : ''}`}
      // key 里带 enterAnimAt：数据到达 → key 变 → React 重新挂载 → 动画重新从头播。
      // 这正是「历史缓慢展示」的触发点，由数据驱动而非计时器。
      key={`n-${enteredAt}-${enterAnimAt}`}
      ref={ref}
      aria-live="polite"
      data-entered={enteredAt}
    >
      {/* 「加载更早」放最顶部（历史往上长）。只要 hasMore 就显示 ——
          用户必须能看见「这里还有更早的内容」，绝不能因为分页而让截断后的
          首屏看起来就是全部历史（那是静默丢数据，用户红线）。 */}
      {hasMore && (
        <div className="narrative-more">
          {olderError ? (
            <button type="button" className="narrative-more-btn is-err" onClick={onLoadOlder} disabled={olderLoading}>
              {t('agent.loadOlderFailed')}
            </button>
          ) : olderLoading ? (
            <span className="narrative-more-busy">{t('agent.loading')}</span>
          ) : (
            <button
              type="button"
              className="narrative-more-btn"
              onClick={onLoadOlder}
              disabled={!moreCursor}
            >
              {t('agent.loadOlderN').replace('{n}', String(Math.max(0, total - blocks.length)))}
            </button>
          )}
        </div>
      )}
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
