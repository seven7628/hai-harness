import { Fragment, memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { cloneElement } from 'react'
import type { ReactElement, ReactNode } from 'react'
import { useAppStore, genMsOf, mainRunEnded } from '../store/useAppStore'
import { resolveRunState, turnLive } from '../lib/turnSettle'
import { signalText } from '../lib/signalFacts'
import type { RevealWindow } from '../lib/scrollFollow'
import { decideFollow, isExternalMoveUp, isUserScrollUp, revealWindow, shouldPinBottom, shouldResumeFollow, stepReveal, wheelConsumedByScroller } from '../lib/scrollFollow'
import type { MsgBlock } from '../store/useAppStore'
import type { TurnStats, TurnUsage } from './runtime/turn/selectTurns'
import { Markdown } from './markdown'
import { FOLDED_KINDS, answerIndex, answerOf, defersToTurnEnd, foldedBody, hasWork, mergeArtifacts, noticeBlocks, outsideOf, rememberSettleMark, runningBody, runWorkBody, segmentUsage, selectTurns, settleIndexOf, settleMarkValid, statsOfItems, turnHead, turnRuns, turnStats, turnUsage, workBody } from './runtime/turn/selectTurns'
import { roundsOf } from './runtime/turn/rounds'
import { batchLine, isDispatchTool } from './runtime/turn/batch'
import { BatchGroup } from './runtime/turn/BatchGroup'
import { Ledger } from './runtime/turn/Ledger'
import { AsyncNote } from './runtime/blocks/AsyncNote'
import { UserBlock } from './runtime/blocks/UserBlock'
import { Lane } from './runtime/lane/Lane'
import { nestCards } from '../lib/nestedCards'
import { LivePanel } from './runtime/turn/LivePanel'
import { WorkBox } from './runtime/turn/WorkBox'
import { FilesLine } from './runtime/turn/FilesLine'
import { Signal } from './runtime/signals/Signal'
import { headsOf } from './runtime/blocks/heads'
import type { CompactHead } from './runtime/blocks/Compact'
import './runtime/turn/turn.css'
// 过程块的渲染原语（思考 / 工具行 / 计划卡 / 压缩块 / 行内码）与子 agent 转录共用一份
// —— 见 NarrativeBlocks.tsx 的文件头（两处宿主、一套 DOM、一套皮肤）。
import { CompressionBlock, InlineCode, Thinking, ToolGroup } from './NarrativeBlocks'
// 任务卡外壳（子 agent 卡）：与右栏「任务」tab 的同类卡共用同一份实现，见 TaskCard.tsx
import { AgentTaskCard } from './TaskCard'
import HaiLogo from './HaiLogo'
import { CheckCircleIcon, PaperclipIcon } from './icons'
import { fmtDuration, fmtRun, fmtClock, fmtUsd } from './ui'
import { useClock } from '../lib/useNow'
import { useSkin } from '../lib/useSkin'
import { ArtifactCard, absArtifactPath } from './ArtifactCard'
import { ArtifactsBlock } from './runtime/artifacts/ArtifactsBlock'
import { UsageActivityMini } from './UsageStats'
import { useLang, useT } from '../i18n'


/* 后台任务落地：两种块（回执 / 完整结果）用同一个 note 渲染 —— 它们说的是同一句
   话，差别只是展开后是纯输出还是 Markdown（见 runtime/blocks/AsyncNote.tsx）。 */
const TaskDeliveryBlock = memo(function TaskDeliveryBlock({ b }: { b: Extract<MsgBlock, { kind: 'task_delivery' }> }) {
  return <AsyncNote taskId={b.taskId} status={b.status} result={b.result} error={b.error} from={b.parentTaskId} />
})

const TaskResultBlock = memo(function TaskResultBlock({ b }: { b: Extract<MsgBlock, { kind: 'task_result' }> }) {
  return <AsyncNote taskId={b.taskId} status={b.error ? 'failed' : 'completed'} result={b.result} error={b.error} from={b.parentTaskId} rich />
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
/* 盒子外面（时间线）的判据不在这里，也不只看种类：error / divider / artifacts 始终在
   外面，而记录类（signal / task_delivery）看**到达时刻** —— 运行中到达的属于这次运行
   的过程（盒内、按事件顺序），AgentEnd 之后到达的才留在时间线上。实现与理由同在
   runtime/turn/selectTurns.ts 的 foldedBody / workBody（那条规则要有测试才算数）。 */
// 工作过程的种类：简洁档收起时它们全部进工作盒子（外面只剩最后一轮正文、产出卡、
// 交卷后到达的记录）。运行中到达的 signal / task_delivery 同样进盒子，判据见
// selectTurns.workBody（含中间轮正文与思考 —— 历史轮的 reasoning 也要能展开读到）。
// 这里保留这个集合给「有没有工作」用。
const FOLDED = FOLDED_KINDS

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

/**
 * NarrativeSource 数据源覆盖：**子智能体子会话**用它把「自己的转录」喂给同一个 Narrative。
 *
 * 为什么用 props 覆盖而不是再造一个页面组件：用户 2026-10 的要求是「子会话完全可以复用
 * 主会话的页面样式与功能；不需要有独立的了」。滚动/贴底跟随/历史渐入/加载更早/空态
 * 都住在这个组件里 —— 另写一份的结果是「样式看着像、行为全没有」（下方空白不能贴底、
 * 没有加载更早）。所以子会话 = 同一个 Narrative + 另一个数据源。
 */
export interface NarrativeSource {
  blocks: MsgBlock[]
  /** 分页三态（子 agent 的分页在子任务 journal 里，不走 blocks_page） */
  total?: number
  hasMore?: boolean
  loading?: boolean
  error?: boolean
  onLoadOlder?: () => void
  /** 空态覆盖（子会话没有消息时不该显示「开始新对话」引导） */
  empty?: ReactNode
}

/* 文件行只印**第一个**被改的文件；增删是整轮的合计（设计 ui.js fileHTML） */
function firstFileOf(turn: { items: MsgBlock[] }): string {
  for (const b of turn.items) {
    if (b.kind === 'tool' && b.diff?.path) return b.diff.path
  }
  return ''
}

export default function Narrative({ viewSid, source, header, footer, density: densityProp, runState, sourceKey }: {
  viewSid?: string
  source?: NarrativeSource
  /** 首行插槽（子会话的「返回主会话 + 身份/用量」）。放在容器**内部**：
   *  这样它与正文同宽同内边距，视觉上就是叙述区的第一行，而不是一条通栏的卡片头。 */
  header?: ReactNode
  /** 末行插槽（宿主追加额外行：子会话的任务级错误）。结果不再用它重复渲染 ——
   *  结果就是转录最后一条 assistant 正文。 */
  footer?: ReactNode
  /** 详细 / 简洁。详细 = 旧观感（工作块全展开、无折叠行）；简洁 = 一轮的工作
   *  收进一行，跑着的时候敞开、跑完收起，点开读全过程。缺省读设置（默认简洁 ——
   *  v2 画布的默认档就是 concise，移植过来的东西不该以另一副样子出现）。 */
  density?: 'concise' | 'detailed'
  /** 这段转录**自己的**运行态（子会话页 / 任务卡内的子转录）。缺省 = 读主会话视图的
   *  AgentEnd 结算位与顶层 run 节点 —— 子 agent 在跑不能拿主 agent 的结算位代表。 */
  runState?: { live: boolean; startedAt?: number; runId?: string }
  /** 这段转录的**身份**（子会话：该子 agent 的 run_id / 后台任务 task_id）。
   *
   *  它是滚动容器的 key —— 与主会话的 session id 同一个语义：换身份 = 换一段转录 =
   *  重建节点、丢掉旧滚动位置、重新贴底。此前子会话页没有它，key 退化成**主会话** id，
   *  于是「左栏从子 agent A 切到 B」容器不重建：转录换了，滚动位置还停在 A 的某处，
   *  A 的上滑意图也会带给 B。 */
  sourceKey?: string
} = {}) {
  const t = useT()
  // 语言作为**值**参与元素复用的依赖（useT 每次渲染返回新闭包，身份不能当依赖）
  const lang = useLang()
  // 指定 viewSid（如 cron 详情页专用 view）→ 渲染该 view 的 blocks；缺省 = 活跃会话。
  // source 优先（子会话）：hooks 必须无条件调用，所以先取 store 再覆盖。
  const storeBlocks = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocks ?? [] : s.blocks))
  const rawBlocks = source ? source.blocks : storeBlocks
  /* 本层视图：**孙任务卡不在这里画**。事件流是会话级的（子智能体派孙任务时，它的
     task_started / agent_start / llm_error 仍经父 handler 上浮到同一条流），store 只能把
     它插成一张平铺的卡；卡上的 parentTaskId 说明它属于派发它的那张卡 —— 所以它在
     那张卡的子会话页里画（AgentSessionView 用同一个 nestCards），主转录只留本层。
     块本身留在 blocks 里不动：左栏树 / 停止按钮 / run→卡 查找全靠它们。 */
  const blocks = useMemo(() => nestCards(rawBlocks).shown, [rawBlocks])
  const persona = useAppStore((s) => s.persona)
  const focusAgent = useAppStore((s) => s.focusAgent)
  /* 车道行的「打开」入口：子智能体有自己的一页（同一套渲染，只是换数据源）。
     孙任务只出现在派发它的那一页上（见 lib/nestedCards），所以这个入口得跟着车道走 ——
     否则想看「它自己干了什么」只能先去左栏树里找它。nodeId/runId/taskId 三个 id 的
     分工见 store 的 AgentFocus 注释（树节点身份 / 找卡片 / 停止与分页）。 */
  const openAgent = useCallback((b: Extract<MsgBlock, { kind: 'agent' }>) => {
    focusAgent({ nodeId: b.taskId || b.runId, runId: b.runId, ...(b.taskId ? { taskId: b.taskId } : {}) })
  }, [focusAgent])
  const turnMetrics = useAppStore((s) => (viewSid ? s.views[viewSid]?.turns : s.turns))
  const storedDensity = useAppStore((s) => s.settings.narrative_density ?? 'concise')
  // 会话页样式（current/v3/runtime）：用户块与轮头按设计重塑只在 runtime 皮肤下生效，
  // 另两档保留原结构 —— 那是它们已经被验证过的样子。读 data-skin 属性而非 store：
  // 属性是「实际渲染成什么」的唯一事实，store 是意图（见 lib/useSkin.ts）。
  const skin = useSkin()
  const density = densityProp ?? storedDensity
  // run 级「正在跑」：块上的 running 在两次 LLM 调用之间是假的（见下方 live）
  // 主 run 是否已结算（AgentEnd）：折叠行与轮尾账的**唯一**放行信号。
  // 不用 run 节点 status 推断 —— 那一路上「llm_end 与下一次 llm_start 之间」会被判成已结束
  // （用户 2026-10 报障：Worked for 与 tokens 早于 AgentEnd 出现），见 lib/turnSettle.ts。
  const mainEnded = useAppStore((s) => (s.activeSessionId ? mainRunEnded(s.views[s.activeSessionId]) : true))
  // 本次运行（AgentStart）的时刻：Working 的累计时长从它起算，而不是从「用户点发送」起算
  // —— 排队等前置任务、压缩这些时间不属于这次运行。
  const mainStartedAt = useAppStore((s) => {
    const nodes = (s.views[s.activeSessionId ?? ''] ?? { runNodes: s.runNodes }).runNodes ?? []
    const root = nodes.find((n) => !n.parentId)
    return root?.startedAt
  })
  /* 正在跑的是**哪一次**运行：顶层 run 节点的 id。runEnded 是视图级的一位，单看它
     挡不住「上一轮的残留 running 块被新 run 点亮」—— 见 lib/turnSettle.ts 的长注释。 */
  const mainRunId = useAppStore((s) => {
    const nodes = (s.views[s.activeSessionId ?? ''] ?? { runNodes: s.runNodes }).runNodes ?? []
    return nodes.find((n) => n.kind === 'run' && !n.parentId)?.id
  })
  /* 子会话页（AgentSessionView / 任务卡内的子转录）渲染的是**子 agent 的**转录，运行态
     必须看它自己：主 agent 交卷了，后台子 agent 还在跑（用户 2026-10 报障：点进子会话
     第一屏就是 Worked for，顶部却写着执行中）。判据收在 lib/turnSettle.resolveRunState。 */
  const { ended: runEnded, startedAt: runStartedAt, runId: activeRunId } = resolveRunState({
    own: runState,
    mainEnded,
    mainStartedAt,
    mainRunId,
  })
  // 等待用户：审批 / 提问。两者都是「运行暂停、等外部回应」，不是 AgentEnd。
  const waiting = useAppStore((s) => ((s.approvals?.length ?? 0) > 0 ? 'approval' : (s.questions?.length ?? 0) > 0 ? 'input' : null))
  // 产出卡的动作：预览 = 用户在文件栏点它的同一条路；Finder = workspace + 相对路径
  const openFile = useAppStore((s) => s.openFile)
  const workspace = useAppStore((s) => s.workspace)
  // 简洁档下点开过的轮：展开是这一轮自己的状态，不是全局设置
  const [openTurns, setOpenTurns] = useState<Record<string, boolean>>({})
  /* 已结算的轮：**元素复用**缓存。
     流式时每个 chunk 都会重渲染整条转录，而历史轮的输入一个都没变 —— 重算的还是同一棵树。
     实测（lab/bench-concurrent.mjs 的 C 段）：423 块时每 chunk 9.5ms 里有 7.4ms 花在
     「与流式无关的已有内容」上；把非末轮整体跳过（临时实验）后斜率直接归零。
     判据必须是**引用相等**（块对象不可变，内容一变就是新对象），加上本组件里所有
     store 选择器的值与本地状态；少一项就会出现「点开不展开 / 账不更新」这类陈旧 UI。 */
  const turnCacheRef = useRef(new Map<string, { items: readonly unknown[]; globals: readonly unknown[]; state: readonly unknown[]; els: (ReactElement | null)[] }>())
  // settledAtRef：这一轮**上一次结算时**有多少块（turn.items.length）。
  //
  // 用途只有一个：块上没有主 run id 时（冷恢复 / 旧快照 / 事件缺 run_id），turnRuns
  // 一刀也切不出来，整轮就只有一个段，`live` 一翻就把已经结算的那一段重新展开成
  // Working（答案从轮尾挪进盒子）。有了这个「观测量」，结算之后新到的东西就能切出
  // 自己的段，上半段保持折叠 —— 用户 2026-11 的「上下抖 2~4s」正是缺这条边界。
  // 记的是「结算时有多少块 + 那一段最后一块的 id」——id 用来在轮的内容被整体替换
  //（同一轮 key 被复用：分页/重放/测试夹具重灌块）时把过期的边界作废，否则会把新内容
  // 从中间切开（实测 test:transcript 的场景 C：插话被切到第一段、盒子只剩后半段）。
  const settledAtRef = useRef<Map<string, { at: number; lastId?: number }>>(new Map())
  const activeSessionId = useAppStore((s) => s.activeSessionId)
  /* 这段转录的身份：滚动容器（滚动位置 / 贴底意图 / 手势监听）都挂在它上面。
     viewSid = 独立 view（cron / 恢复页）；sourceKey = 子会话页（子 agent 的 run_id）；
     都没有 = 当前主会话。三者同义：「这是哪一段转录」。 */
  const transcriptKey = viewSid ?? sourceKey ?? activeSessionId ?? 'none'
  const ref = useRef<HTMLDivElement | null>(null)
  // nodeGen：滚动容器的「节点世代」。
  //
  // ## 为什么必须有（2026-10-21 修 bug）
  //
  // 根节点的 key 锁了会话 id（切会话时重建 DOM 以丢弃滚动位置）。React 换 key
  // 会**销毁旧节点、创建新节点**，而挂监听器的 effect 依赖是 []，它不会重跑 ——
  // 于是 wheel / touch / scroll / keydown 全部挂在**已脱离文档的旧节点**上，
  // 从此再也收不到任何用户手势。
  //
  // 后果：手势判据（上滑停跟随）彻底失效，而程序性钉底仍然工作（tick 每次
  // 重新读 ref.current，用的是新节点）→ 「上滑永远被拽回底部」。
  // 用 callback ref 记录节点身份变化，让监听器跟着节点走。
  const [nodeGen, setNodeGen] = useState(0)
  const setNodeRef = useCallback((n: HTMLDivElement | null) => {
    if (ref.current === n) return
    ref.current = n
    setNodeGen((g) => g + 1)
  }, [])
  const anchorRef = useRef<HTMLDivElement>(null)
  // —— 主对话分页（2026-10-20）——
  // 后端 BlocksPage 的游标：最早已加载块的 id 与「还有更早」标记。
  // 注意这两个值来自 store（由 hydrateCheckpoint 写入），不是本地 state ——
  // 会话切换时随视图一起重置，避免把上一个会话的游标带到新会话。
  const storeTotal = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocksTotal ?? 0 : s.blocksTotal))
  const storeMoreCursor = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocksCursor ?? 0 : s.blocksCursor))
  const storeHasMore = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocksHasMore ?? false : s.blocksHasMore))
  // 游标是**主对话 blocks** 的概念。子会话（source 模式）的分页在子任务 journal 里，
  // 与主会话的 blocksCursor 无关 —— 旧实现读 store 的游标，于是「主会话还有更早的块」
  // 就变成了子会话滚动预取的放行信号（子会话里一上滑就连发分页请求）。
  // source 模式只看它自己的 hasMore。
  const moreCursor = source ? (source.hasMore ? 1 : 0) : storeMoreCursor
  const total = source ? (source.total ?? 0) : storeTotal
  const hasMore = source ? Boolean(source.hasMore) : storeHasMore
  // 按钮用的 loading/error 走 store（不是本地 state）：响应由 store 的
  // blocks_page 处理器写回，本地 state 无法感知「已经加载完了」，
  // 会出现按钮一直转圈/错误态不消。store 是唯一事实源。
  const storeOlderLoading = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocksOlderLoading ?? false : s.blocksOlderLoading))
  const storeOlderError = useAppStore((s) => (viewSid ? s.views[viewSid]?.blocksOlderError ?? false : s.blocksOlderError))
  const olderLoading = source ? Boolean(source.loading) : storeOlderLoading
  const olderError = source ? Boolean(source.error) : storeOlderError
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

  // —— 渐入动画的重播控制（不重挂载组件）——
  //
  // 需求：每次会话进入都重播一次「历史渐入」，但**绝不能重新挂载容器**
  //（重挂载 = DOM 重建 = scrollTop 归零 = 用户被拽回底部）。
  //
  // 做法：把 animation 挂在子元素上，用「类名 + 属性」的切换来重启动画：
  //   1. 先移除类（animOff），一帧后再加回来（animOn）—— 强制浏览器重算样式，
  //      动画从头播；
  //   2. 只动动画类，不动 DOM 结构，滚动位置完全不受影响。
  //
  // 为什么需要两帧：同一帧内 remove + add 同一个类，浏览器会合并成「无变化」，
  // 动画不会重播。必须让浏览器先看到「没有这个类」的状态。
  const [animOn, setAnimOn] = useState(false)
  useEffect(() => {
    if (!enterAnimAt) return
    setAnimOn(false)
    let raf1 = 0
    let timer: ReturnType<typeof setTimeout> | undefined
    raf1 = requestAnimationFrame(() => {
      setAnimOn(true)
      // 动画播完就摘掉类（2026-10-21）。
      //
      // 为什么必须摘：这个类会给所有子元素挂 animation。留着不放的话，
      // 任何布局变化都可能让动画重新求值/重排，在流式输出期间表现为
      // 「内容高度反复变化 → 浏览器钳制 scrollTop → 用户被顶回底部」。
      // 动画本来就只需要播这一次。
      //
      // 620ms = 最长一条的延迟 288ms + 时长 320ms + 少量余量。
      timer = setTimeout(() => setAnimOn(false), 620)
    })
    return () => {
      cancelAnimationFrame(raf1)
      if (timer) clearTimeout(timer)
    }
  }, [enterAnimAt])

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
  // 常量与决策规则都在 lib/scrollFollow（纯函数，不变量由单测钉住）。
  // revealStart：已铺开的**起始下标**（不是「个数」）。
  //
  // ## 为什么必须用绝对下标（2026-10-21 修 bug）
  //
  // 上一版存的是「铺开多少个」revealCount，切片时算 blocks.length - revealCount。
  // 流式输出让 blocks.length 变大，而 revealCount 不变 → 起点跟着变大 →
  // **前面已渲染的块被丢弃** → DOM 高度骤减 → 浏览器把 scrollTop 夹到底部。
  // 这正是「一有流式输出就被顶回贴底」的主因之一。
  //
  // 改用绝对下标后，铺开只做一件事：把 start 每帧减一批，
  // 起点单调减小（朝向 0），**永远不会因为尾部增长而后移**。
  // 0 表示「全部显示」，与起点语义统一。
  //
  // ## 起始窗口在**渲染期**决定，不走 effect（2026-11 修切换时的上下窜动）
  //
  // 上一版用 effect 决定起始窗口：先按上一段转录的 revealStart 把**全部**块挂上
  // DOM，等 enterAnimAt 的 effect 跑起来再砍到尾部 25 条。这一砍把 scrollHeight
  // 从「整段高度」缩到一屏 —— 浏览器立刻把 scrollTop 钳到 0，随后铺开循环每帧
  // 补 25 条、每帧重新钉底。长会话（200 条 / 几万像素）里，用户看到的就是切换
  // 瞬间「先窜到上面、再滑回底部」，而 content-visibility 的估算高度让落点一次
  // 比一次偏（「最新位置定位没有更新」）。渲染期就决定窗口，就**没有**那次
  // 「先全挂、再砍掉」的中间态。
  //
  // 另外：revealStart 以前是组件级 state，**跨转录存活**。上一段铺到一半切走
  //（start=100）、新会话只有 30 条 → blocks.slice(100) 为空 → 白屏一帧。
  // 现在 key 一变就重新决定，不会串。
  //
  // 身份用**转录**（transcriptKey），不用 enterAnimAt：enterAnimAt 是全局的
  // 进入序号，后台会话的 new_session 响应也会把它 ++（store 的 markEnterAnim）。
  // 把它算进身份的话，这类与本段转录无关的进入会让铺开重新开始 —— 相当于
  // 在用户眼前把整段内容砍到尾部 25 条再长回来（一次几万像素的高度塌缩），
  // 表现就是「有子智能体/后台任务在跑的时候，主会话页面自己上下抖」。同一次
  // 进入只铺一遍；换会话（key 变）才重新决定。
  const enterKey = transcriptKey
  const [reveal, setReveal] = useState<RevealWindow>({ key: '', start: 0 })
  // 渲染期派生（同组件内 setState：React 会立刻重渲染，中间那一帧不会提交）：
  const decided = revealWindow(reveal, enterKey, blocks.length, Boolean(enterAnimAt))
  if (decided !== reveal) setReveal(decided)
  // 只渲染 blocks.slice(revealStart)；0 = 全部显示。铺开进度单调朝 0 走，
  // 流式追加新块不会让它后移（绝对下标语义，见上）。
  const revealStart = reveal.start > 0 ? reveal.start : 0
  const revealStartRef = useRef(revealStart)
  revealStartRef.current = revealStart
  const revealing = reveal.start > 0
  useEffect(() => {
    if (!revealing) return
    let raf = 0
    const step = () => {
      const next = stepReveal(revealStartRef.current)
      revealStartRef.current = next
      setReveal((r) => (r.key === reveal.key ? { ...r, start: next } : r))
      if (next > 0) raf = requestAnimationFrame(step)
    }
    raf = requestAnimationFrame(step)
    return () => {
      if (raf) cancelAnimationFrame(raf)
    }
  }, [reveal.key, revealing])
  const storeLoadOlder = useAppStore((s) => s.loadOlderBlocks)
  const sourceLoadOlder = source?.onLoadOlder
  const onLoadOlder = useCallback(() => {
    if (olderLoading) return
    if (sourceLoadOlder) sourceLoadOlder()
    else storeLoadOlder(viewSid)
  }, [olderLoading, sourceLoadOlder, storeLoadOlder, viewSid])

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
    // ── 跟随态：**绘制前**钉到底部 ──────────────────────────────────────
    //
    // ## 为什么必须在 useLayoutEffect 里做（2026-10-21 修「刷新/进入定位不准」）
    //
    // 进入会话（刷新页面、切长会话）时只渲染尾部的 REVEAL_PER_FRAME 个块，
    // 其余靠铺开逐帧补到上方。而铺开期间 rAF tick 是**关掉的**（见下方
    // `if (revealStart > 0) return`），容器的 scrollTop 又是 0 —— 于是首帧
    // 显示的是「尾部窗口的顶部」，比最新消息早二十来条，几百毫秒后才跳到底部。
    // 块数 ≤ 25 时 revealStart 恒为 0，所以短会话看不出来，长会话才明显。
    //
    // 放在 layout effect 里而不是等 tick：layout effect 在 DOM 变更后、浏览器
    // 绘制**前**执行，所以这个错误位置一帧都不会被画出来。
    if (stickRef.current) {
      const max = el.scrollHeight - el.clientHeight
      if (max > 0 && el.scrollTop !== max) {
        el.scrollTop = max
        // 单写口径：tick 的「外部改动」判据依赖它知道自己写过什么
        expectedTopRef.current = el.scrollTop
        lastTopRef.current = el.scrollTop
      }
      anchorRef2.current = { height: el.scrollHeight, top: el.scrollTop, firstID }
      return
    }
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
    if (grew > 0) {
      el.scrollTop = prev.top + grew
      // tick 的「外部改动」判据只认「tick 自己写过什么」。这里是补偿写入，
      // 不同步基准会让下一帧把它误判成用户上滑而关掉跟随。
      expectedTopRef.current = el.scrollTop
      lastTopRef.current = el.scrollTop
    }
    // revealStart：铺开每推进一批都会改变可见范围，必须在绘制前重新钉底
  }, [blocks, revealStart])

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
  }, [hasMore, olderLoading, olderError, moreCursor, onLoadOlder, blocks.length, nodeGen])
  // —— 智能置底（2026-08-22）：流式输出期间用户上滑浏览历史时，不再被新块拽回底部 ——
  // stickRef：是否贴底。纯 ref（滚动是高频事件，绝不逐 tick setState）；
  // 只有「跨过」底部边界时才 setState（away/fresh 指示器显隐，一次手势最多一次重渲）。
  // 离开底部 → 停自动滚动（也不再做任何强制布局）；新块只悄悄标记 fresh（按钮加圆点）；
  // 回到底部附近 / 点击按钮 / 切换会话 → 恢复贴底。
  const stickRef = useRef(true)
  // autoscrollRef：是否应该「让新内容把视图往上顶」。
  //
  // 这是本轮重写的核心（2026-10-21，用户点出矛盾本质）：
  //   没上划 → true  → 新内容到达时跟随到底部（底部往上顶）
  //   上划过 → false → 新内容只在下面增长，视图一动不动
  //
  // 与 stickRef 的区别：stickRef 由 scroll 事件里的「距底距离」推算，
  // 会被 content-visibility 造成的高度抖动干扰；autoscrollRef 只由
  // **用户亲手滚动的那一刻**决定一次，之后 tick 不再改它。
  // 两者暂时并存（stickRef 供 away/fresh 按钮显示），后续可合并。
  const autoscrollRef = useRef(true)
  // expectedTopRef：tick 上一次**自己**把 scrollTop 写成的值（跨 effect 重跑存活）。
  //
  // ## 为什么必须是 ref 而不是 effect 内的 let（2026-10-21，日志实证后定案）
  //
  // 判据是「我上次写到多少」，但 tick 所在的 effect 依赖 [blocks, revealStart]，
  // 而流式输出**每帧都生成新的 blocks 数组** → effect 每帧取消重跑。
  // 若把 expectedTop 放在闭包里，每次重跑都会被重置成「用户此刻刚滚到的位置」，
  // 下一帧自比对必然相等 → 判据永远不触发（实测 [TICK-EXTERNAL] 一次未出现）。
  // ref 跨重跑存活，才真正表达「我上次写到多少」。
  const expectedTopRef = useRef(-1)
  // 同上：lastMax 也必须跨重跑存活。放在闭包里会让 grew 每帧都为 true，
  // 无论高度是否真的变化都无条件钉底。
  const lastMaxRef = useRef(-1)
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
  // 上一次观察到的 scrollTop：用于「变小 ⇒ 用户在往上看」这个与事件源无关的判据
  const lastTopRef = useRef(0)
  // 触摸滑动上一次的 Y（判断方向）
  const touchYRef = useRef(0)
  // intentRef：本次手势的方向（'up' = 看更早的内容）。
  //
  // 不能用「滚完之后离底部多远」代替：tick 会把位置钉回底部，用户在 threshold
  // 内时 settle 读到的永远是「在底部」。方向只有用户的手势知道。
  const intentRef = useRef<'up' | 'down' | null>(null)
  // scroll 监听里观察到的 max（scrollHeight - clientHeight）。
  // 用于区分「用户上滑」与「内容塌缩导致的 scrollTop 钳制」：切会话时
  // blocks 被清空 → 容器塌缩 → 浏览器把 scrollTop 从上一会话的底部钳到 0，
  // 这个「变小」不是用户意图，不能据此关掉跟随。
  const scrollMaxRef = useRef(-1)
  const [away, setAway] = useState(false) // 用户已离开底部（显示「回到底部」按钮）
  const [fresh, setFresh] = useState(false) // 离开期间有新内容到达（按钮带圆点）
  // away 是**为什么**被置位的：true = 用户自己往上翻了（意图，必须尊重）；
  // false = tick 的「外部位移」启发式（内容塌缩会骗它，允许用几何撤销）。
  // 不分开的话，自动折叠造成的一次误判会让按钮永久挂住（用户 2026-10 报障）。
  const awayHardRef = useRef(false)
  // 贴底/方向判定统一走 lib/scrollFollow（纯函数，不变量由 scrollFollow-test 钉住）
  const BOTTOM_THRESHOLD = 120 // px：视为「贴底」的容差（content-visibility 高度为估算值）
  // —— 换转录：**渲染期**把「用户意图」清零（2026-11 修切换后回不到最新位置）——
  //
  // 为什么必须在渲染期、而不是 effect：
  //
  //  ① 新节点是 key 变化新建的，scrollTop 从 0 起。绘制前那次钉底（下方
  //     useLayoutEffect）判的是 stickRef —— 用户在上一段上滑过（stickRef=false）
  //     的话这一帧就不钉，第一帧画在「尾部窗口的顶部」，要等下一个 effect 才
  //     回到底部。渲染期置回 true，layout effect 同一帧就能钉到底。
  //  ② 手势标记是**上一段转录的**。实测（npm run probe:switch-scroll）：在 A 里
  //     上滑 → 点 B；B 的第一次 scroll（切换自己钉底产生的那一次）被当成用户
  //     手势，settle(dir='up') → stopFollow(hard) → awayHard 置位。此后视图明明
  //     贴在底部，「↓ 回到最新」却挂住 1.6s 以上、跟随一直是死的 —— 因为
  //     shouldResumeFollow 对「用户亲手翻上去」的正确做法是不自动翻案（见
  //     scrollFollow.ts 的注释）。错的是那个手势的来源，不是那条规则。
  //  ③ 基准（expectedTop / lastMax / lastTop / scrollMax）全按上一段内容的高度
  //     建立，不换掉的话第一帧会把「换了一段内容」判成外部位移或用户上滑。
  //
  // nodeFreshRef：新节点的「第一次 scroll」只校准基准，不判方向。
  //
  // 换 key 时浏览器的 scroll 事件是**异步**派发的：它可能晚于渲染、早于
  // transcriptKey 那个 effect。那一次事件的 prevTop / prevMax 还是上一段转录的
  // 坐标系，判出来必然是「用户上滑」→ hard stop → 按钮挂一下。第一次只对齐
  // 基准，把它当成「我的位置变了」，不当成「用户动了」。
  const nodeFreshRef = useRef(false)
  // 用 ref 记住「上一次渲染的转录身份」：同一段转录的后续渲染什么都不做。
  const switchRef = useRef(transcriptKey)
  if (switchRef.current !== transcriptKey) {
    switchRef.current = transcriptKey
    stickRef.current = true
    autoscrollRef.current = true
    awayHardRef.current = false
    expectedTopRef.current = -1
    lastMaxRef.current = -1
    lastTopRef.current = 0
    scrollMaxRef.current = -1
    userGestureRef.current = false
    intentRef.current = null
    lastGestureAtRef.current = 0
    touchYRef.current = 0
    nodeFreshRef.current = true
    settledAtRef.current.clear()
    setAway(false)
    setFresh(false)
  }
  // 程序滚动的**唯一入口**。
  //
  // ## 单写口径（2026-10-21）
  //
  // tick 的「外部改动」判据依赖「tick 知道每一次自己写入了多少」。任何绕过
  // 这里的 el.scrollTop 赋值都会让基准失真，判据随即失效（上一版把基准放在
  // effect 闭包里就栽在这上面）。所以写入后立刻同步两个基准：
  //   · expectedTopRef：我刚写到多少 → 下一帧据此识别外部改动
  //   · lastTopRef：让这次写入产生的 scroll 事件不被误判成「用户往上滚」
  const setScrollTop = (el: HTMLElement, v: number) => {
    if (el.scrollTop === v) return
    el.scrollTop = v
    expectedTopRef.current = el.scrollTop
    lastTopRef.current = el.scrollTop
  }
  // 用户滚动 → 更新「是否该跟随」的意图（autoscrollRef）。
  //
  // ## 设计原则（用户点出的矛盾本质）
  //
  //   没上划 → 希望新内容把视图往上顶
  //   上划后 → 绝不希望被顶回去
  //
  // 所以这不是「滚动位置的推算」，而是**用户意图的状态机**。
  // 唯一的输入是用户亲手滚动（wheel / touch / 键盘 / 拖滚动条），
  // 唯一的输出是 autoscrollRef 的 true/false。tick 只读不写。
  //
  // ## 为什么需要多种事件源
  //
  // 单靠 wheel 不够：拖滚动条、键盘翻页、触控板惯性都不产生 wheel。
  // 所以额外用「scrollTop 变小」这个**方向信号**兜底 —— 程序性钉底只会让
  // scrollTop 变大（趋向 max），绝不会变小，因此「变小」必然源于用户。
  useEffect(() => {
    const el = ref.current
    if (!el) return
    lastTopRef.current = el.scrollTop
    scrollMaxRef.current = el.scrollHeight - el.clientHeight
    // 停止跟随：唯一的状态转移，所有手势/外部改动都收敛到这里。
    // `hard` = 这次停止是不是**用户亲手**翻上去的（见 awayHardRef 的注释）。
    const stopFollow = (hard = false) => {
      // 已脱离文档的旧节点不在讨论范围：切会话换 key 之后，浏览器仍可能把
      // 旧节点上排队的 scroll 事件派发过来 —— 那种事件读到的是上一段的高度与
      // 位置，据此停跟随只会让新会话的「↓ 回到最新」闪一下（实测 1 帧）。
      if (!el.isConnected) return
      autoscrollRef.current = false
      if (hard) awayHardRef.current = true
      if (stickRef.current) {
        stickRef.current = false
        setAway(true)
      }
    }
    // 恢复跟随（仅在**用户自己**把视图带回底部时）。
    const startFollow = () => {
      autoscrollRef.current = true
      awayHardRef.current = false
      // 基准必须同步成当前位置：否则 tick 的下一帧会把「距旧基准的落差」
      // 当成外部改动，刚恢复的跟随又被自己关掉。
      expectedTopRef.current = el.scrollTop
      lastTopRef.current = el.scrollTop
      if (!stickRef.current) {
        stickRef.current = true
        setAway(false)
      }
      setFresh(false)
    }
    // 手势结束后重新评估。
    //
    // ## 为什么必须传方向（2026-10-21 修 bug）
    //
    // 上一版这里是 `const nb = isNearBottom(); autoscrollRef.current = nb`。
    // 而 tick 会把位置钉回底部，用户在阈值内时 isNearBottom() 永远为 true ——
    // 于是 onScroll 里刚由「top 变小」置的 false，在同一函数末尾被 settle
    // 又改回 true。表现：上滑瞬间 away 按钮闪一下就没，视图继续被拽回。
    // 方向只有用户手势知道，position 不可信。
    const settle = (dir: 'up' | 'down' | null) => {
      const follow = decideFollow({
        dir,
        top: el.scrollTop,
        max: el.scrollHeight - el.clientHeight,
        threshold: BOTTOM_THRESHOLD
      })
      if (follow) startFollow()
      else stopFollow(dir === 'up') // 手势方向是 up → 用户意图（硬停止）；否则只是位置判断
    }
    const mark = (dir: 'up' | 'down' | null) => {
      userGestureRef.current = true
      lastGestureAtRef.current = performance.now()
      if (dir) intentRef.current = dir
    }
    // target → el 之间最近的**可垂直滚动**祖先（工具输出 <pre>、diff 体、思考折叠体…）。
    // wheel 会冒泡到 .narrative，若不排除，用户只是在输出里往回翻两行，整个对话就被
    // 判定成「他要离开底部」（用户 2026-10：自动折叠后按钮行为异常，一半原因在这）。
    const nestedScrollerOf = (target: EventTarget | null): HTMLElement | null => {
      let node = target instanceof HTMLElement ? target : null
      while (node && node !== el) {
        const oy = getComputedStyle(node).overflowY
        if ((oy === 'auto' || oy === 'scroll') && node.scrollHeight > node.clientHeight + 1) return node
        node = node.parentElement
      }
      return null
    }
    const onWheel = (e: WheelEvent) => {
      const inner = nestedScrollerOf(e.target)
      /* 运行盒子（Working / Worked，两种浓度里只有简洁档有）本身**就是这一轮的
         转录**：在它里面往回翻 = 用户在看历史。它占掉大半屏，用户的手几乎总是落在
         它上面 —— 若照「内层可滚就吞掉」处理，外层一直被钉在底部、按钮也不出现，
         表现就是「运行中完全上不去看历史」(用户 2026-10)。这里只截住它自己那层；
         盒子**里面**的工具输出/思考折叠体仍是内层滚动，不该打断跟随（下方判据）。 */
      if (inner && inner.classList.contains('rt-work-body') && e.deltaY < 0) {
        mark('up')
        stopFollow(true)
        return
      }
      if (inner && wheelConsumedByScroller({
        deltaY: e.deltaY,
        scrollTop: inner.scrollTop,
        scrollHeight: inner.scrollHeight,
        clientHeight: inner.clientHeight,
      })) return
      const dir = e.deltaY < 0 ? 'up' : 'down'
      mark(dir)
      // 向上滚 → 立刻停止跟随（不等 scroll 事件，避免被 tick 抢跑）。
      if (dir === 'up') stopFollow(true)
    }
    const onScroll = () => {
      if (!el.isConnected) return
      if (nodeFreshRef.current) {
        // 新节点的第一次 scroll：只对齐基准（见 nodeFreshRef 的注释）
        nodeFreshRef.current = false
        lastTopRef.current = el.scrollTop
        scrollMaxRef.current = el.scrollHeight - el.clientHeight
        userGestureRef.current = false
        intentRef.current = null
        return
      }
      const top = el.scrollTop
      const prevTop = lastTopRef.current
      lastTopRef.current = top
      const max = el.scrollHeight - el.clientHeight
      const prevMax = scrollMaxRef.current
      scrollMaxRef.current = max
      // 方向兜底：scrollTop 变小 = 用户往上看更早的内容。
      //
      // 必须排除两种**非用户**的变小（2026-10-21，端到端实测踩到）：
      //  ① 内容塌缩导致钳制：切会话/清空 blocks 时容器变矮，浏览器把
      //     scrollTop 从上一会话底部钳到 0 —— 实测这一步会把跟随永久关掉，
      //     表现为新会话再也不自动贴底（gap 一路涨到 850px）。
      //  ② 钳到当前底部：top 已经贴着 max，再变小只是高度变化的结果。
      const movedUp = isUserScrollUp({ top, prevTop, max, prevMax })
      if (movedUp) stopFollow(true)
      // 用户手势引发的 scroll 才重新评估「是否回到底部」。
      if (!userGestureRef.current) return
      userGestureRef.current = false
      const dir = intentRef.current ?? (movedUp ? 'up' : 'down')
      intentRef.current = null
      settle(dir)
    }
    const onKey = (e: KeyboardEvent) => {
      if (!['ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End', ' '].includes(e.key)) return
      const up = e.key === 'ArrowUp' || e.key === 'PageUp' || e.key === 'Home'
      mark(up ? 'up' : 'down')
      if (up) stopFollow(true)
    }
    const onTouchStart = (e: TouchEvent) => {
      mark(null)
      touchYRef.current = e.touches[0]?.clientY ?? 0
    }
    const onTouchMove = (e: TouchEvent) => {
      const y = e.touches[0]?.clientY ?? 0
      const dy = y - touchYRef.current
      touchYRef.current = y
      if (Math.abs(dy) < 1) return
      // 手指向下拖 = 内容往上走 = 看历史
      const dir = dy > 0 ? 'up' : 'down'
      mark(dir)
      if (dir === 'up') stopFollow(true)
    }
    el.addEventListener('wheel', onWheel, { passive: true })
    el.addEventListener('touchstart', onTouchStart, { passive: true })
    el.addEventListener('touchmove', onTouchMove, { passive: true })
    el.addEventListener('keydown', onKey, { passive: true })
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => {
      el.removeEventListener('wheel', onWheel)
      el.removeEventListener('touchstart', onTouchStart)
      el.removeEventListener('touchmove', onTouchMove)
      el.removeEventListener('keydown', onKey)
      el.removeEventListener('scroll', onScroll)
    }
    // nodeGen：切会话换 key 会替换 DOM 节点，监听器必须重新挂到新节点
  }, [nodeGen])
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
    // ## 校准循环**始终**运行（2026-10-21 修「刷新后停在某个位置不动」）
    //
    // 之前这里有两处早退：`!stickRef.current` 直接 return、`revealStart > 0`
    // 直接 return。它们让 tick「根本没启动」，而启动与否依赖 blocks/revealStart
    // 的变化时机 —— 一旦某个时序下 effect 跑在 follow=true 之前、之后又不再变，
    // 循环就永远不启动：内容继续长高，视图却停在原地不动。
    // 这正是用户报的「刷新后不在最底部，在某个位置停住了」（能跑就会跟到底）。
    //
    // 现在改为「循环常驻、每帧自己判断」：
    //   · follow=true  → 高度变化就钉底；
    //   · follow=false → 一帧不写，只做「外部改动」检测（用户上滑的兜底判据）。
    // 代价是离开底部时每帧一次 scrollTop 读取，可忽略；换来的是不再有
    // 「循环没启动」这个状态分支。
    if (!stickRef.current && blocks.length) setFresh(true)
    let raf = 0
    // expectedTop：tick 上一次**自己**把 scrollTop 写成的值。
    //
    // ## 这是唯一不会与用户抢跑的判据（2026-10-21，日志实证后定案）
    //
    // 日志显示：用户上滑后 autoscroll 恒为 true、[SETTLE] 一次都没出现，
    // 而 tick 每帧都在把 top 钉回 max。原因是 tick 跑得比所有事件都快：
    // 用户的滚动刚发生、scrollTop 还没被事件读到，tick 就已经把它改回底部了。
    // 于是「top 变小」「wheel deltaY<0」「finger down」这些判据全部失效 ——
    // 它们观察到的世界永远是「贴在底部」。
    //
    // 破局点：tick 完全知道自己把 top 写成了多少。下一帧如果发现
    // `el.scrollTop !== expectedTop`，那**一定是外部改的**（用户滚动/滚动条/
    // 键盘/浏览器钳制），因为 tick 自己只写这一个值。
    // 这一帧就不能再钉底，并且要把 autoscroll 关掉。
    //
    // 它不依赖任何事件时序，也不依赖高度是否稳定 —— 只依赖「我写过什么」。
    const tick = () => {
      raf = 0
      const max = el.scrollHeight - el.clientHeight
      const prevMax = lastMaxRef.current
      lastMaxRef.current = max
      // 外部改过 scrollTop → 用户移动了视图 → 立即停止跟随。
      //
      // 只判「变小」：tick 自己只会把 top 往 max 推（单调增大），所以 top 变小
      // 必然是外部改的（用户上滑/拖滚动条/键盘）。用 abs 差值会在浏览器
      // scroll-anchoring 把 top 自动加大时误判成「用户动了」。
      // 容差 2px：过滤亚像素/取整误差。
      if (
        isExternalMoveUp({
          top: el.scrollTop,
          expectedTop: expectedTopRef.current,
          max,
          prevMax
        })
      ) {
        if (autoscrollRef.current) {
          autoscrollRef.current = false
          stickRef.current = false
          awayHardRef.current = false // 启发式停止，不是用户亲口说的
          setAway(true)
        }
      }
      // 几何兜底（用户 2026-10：自动折叠让「回到最新」按钮挂住）：
      // 非跟随态下 tick 不写 scrollTop，位置可信 —— 既然已经贴底，按钮就不该在。
      // 用户亲手翻上去的（awayHard）不在此列：那一路必须由用户自己滚回来才恢复，
      // 否则「在阈值内上滑 50px」会被立刻翻案（就是历史上的 bug ①）。
      else if (
        shouldResumeFollow({
          follow: autoscrollRef.current,
          top: el.scrollTop,
          max,
          hardAway: awayHardRef.current,
        })
      ) {
        autoscrollRef.current = true
        stickRef.current = true
        awayHardRef.current = false
        expectedTopRef.current = el.scrollTop
        setAway(false)
        setFresh(false)
      }
      // ## 只要「跟随 + 不在底部」就钉，不再要求高度刚变过（2026-10-21）
      //
      // 上一版是 `if (grew)` —— 只有 scrollHeight 变化才钉。于是「高度已稳定、
      // 但位置是错的」这种状态永远修不回来：刷新后停在半途就一直停着，内容
      // 长高前它不会再动一下。用户报的「不在最底部，在某个位置停住了」正是它。
      //
      // 去掉 grew 门槛为什么安全：跟随态下钉底是不变式（本来就该在底部），
      // 而用户一旦上滑，外部改动判据会先把 autoscroll 置 false —— 不会抢跑。
      if (shouldPinBottom({ follow: autoscrollRef.current, top: el.scrollTop, max })) {
        setScrollTop(el, max)
      }
      // 无论有没有钉底，都记下「本帧结束时 top 应该是多少」，供下一帧比对。
      // 钉底分支里 top === max；未钉底时就是当前值（我们没动它）。
      expectedTopRef.current = el.scrollTop
      raf = requestAnimationFrame(tick)
    }
    // 首次进入（或某条路径清空过）才同步；**不得**每次 effect 重跑都覆盖，
    // 否则用户上滑的位移会被当成基准吃掉（正是上一版失效的原因）。
    if (expectedTopRef.current < 0) expectedTopRef.current = el.scrollTop
    raf = requestAnimationFrame(tick)
    return () => { if (raf) cancelAnimationFrame(raf) }
    // nodeGen：切会话换 key 会替换 DOM 节点，tick 必须钉到新节点上
  }, [blocks, revealStart, nodeGen])
  // 会话切换：重置贴底状态。内容到达后的持续校准由 blocks effect 统一负责
  //（覆盖 content-visibility 慢布局：rAF 循环直到内容高度稳定）。
  // 这里只重置 + 若新会话内容已在（缓存切换），先滚一次减少等待。
  useEffect(() => {
    stickRef.current = true
    autoscrollRef.current = true // 新会话默认跟随（用户没表达过「看历史」意图）
    awayHardRef.current = false
    setAway(false)
    setFresh(false)
    const el = ref.current
    // 内容整体换掉 → 旧基准无意义（否则首帧会把切会话的自动滚动误判成用户上滑）
    expectedTopRef.current = -1
    lastMaxRef.current = -1
    scrollMaxRef.current = -1
    if (el) {
      setScrollTop(el, el.scrollHeight - el.clientHeight)
      lastTopRef.current = el.scrollTop
      expectedTopRef.current = el.scrollTop
      // scrollMaxRef 也必须归位：onScroll 用它区分「用户上滑」与「内容塌缩造成的
      // 钳制」，留着上一段的 max 会让新会话的第一次滚动直接判成用户上滑。
      scrollMaxRef.current = el.scrollHeight - el.clientHeight
    }
    // 依赖是「哪一段转录」而不是「哪个会话」：子会话页没有自己的 session id，
    // 用 sourceKey（子 agent 的 run_id）—— 否则 A→B 切换不重置贴底与滚动位置。
  }, [transcriptKey])
  const jumpToBottom = () => {
    stickRef.current = true
    autoscrollRef.current = true // 点「回到底部」= 明确要恢复跟随
    awayHardRef.current = false
    /* 手势意图也要一起清掉（2026-11，元素复用提速后暴露出来的真 bug）：
       `settle()` 判断是否停止跟随时**先看意图**（decideFollow：dir==='up' 一律不跟），
       而用户上滑留下的 intentRef='up' 不会自己过期。于是一个排队中的 scroll 事件
       （上滑那一下的、或本函数自己写 scrollTop 产生的）在点了「回到底部」之后才被处理，
       就会拿旧意图把刚刚恢复的跟随再关掉 —— 表现：已经贴在底部了，按钮还在。
       之前没暴露只是因为渲染慢、事件恰好先到；提速 40% 之后顺序反过来了。 */
    intentRef.current = null
    userGestureRef.current = false
    lastGestureAtRef.current = 0
    setAway(false)
    setFresh(false)
    expectedTopRef.current = -1
    const el = ref.current
    if (!el) return
    // ## 为什么不再用 scrollIntoView（2026-10-21）
    //
    // 它把「末尾锚点元素的底边」对齐到容器底边，但锚点后面还有「回到最新」
    // 按钮、且下方内容可能被异步撑高（shiki/mermaid）—— 落点会差几十~几百像素，
    // 表现为「点了回底，但没到底」（端到端实测偶发 377px）。
    // 直接钉 max 才是「回到底部」的语义；再补一次下一帧的 rAF，覆盖
    // 同一帧内后续的撑高（异步渲染）。
    el.scrollTop = el.scrollHeight - el.clientHeight
    expectedTopRef.current = el.scrollTop
    lastTopRef.current = el.scrollTop
    requestAnimationFrame(() => {
      const cur = ref.current
      if (!cur || !stickRef.current) return
      const max = cur.scrollHeight - cur.clientHeight
      if (max > 0 && cur.scrollTop !== max) {
        cur.scrollTop = max
        expectedTopRef.current = cur.scrollTop
        lastTopRef.current = cur.scrollTop
      }
    })
  }

  // 渲染单个非工具块（工具已在上面归组处理）
  /* textOnly：正在写的那一段回复在简洁档运行中露在 Working 外面，但**只露正文** ——
     它自己的思考行属于「工作过程」，必须留在 Working 里（否则每一轮都会有一条
     「思考 · xx ms」跑到折叠块外面）。 */
  const renderBlock = (
    b: Exclude<MsgBlock, { kind: 'tool' }>,
    heads?: CompactHead[],
    opts?: { textOnly?: boolean },
  ): ReactElement | null => {
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
    // 任务结果/回执：运行时皮肤下是一行信号（设计「Small signals」），另两档保留卡片
    if (b.kind === 'task_result' || b.kind === 'task_delivery') {
      if (skin !== 'runtime') {
        return b.kind === 'task_result' ? <TaskResultBlock key={b.id} b={b} /> : <TaskDeliveryBlock key={b.id} b={b} />
      }
      const st = b.kind === 'task_delivery' ? b.status : b.error ? 'failed' : 'completed'
      const first = ((b.kind === 'task_delivery' ? b.result || b.error : b.error || b.result) || '').split('\n')[0].trim()
      return (
        <div className="sig-list" key={b.id}>
          <Signal kind="promote" title={`Task ${b.taskId} · ${st}`} detail={first.slice(0, 160)} type={b.kind} />
        </div>
      )
    }
    if (b.kind === 'compression') return <CompressionBlock key={b.id} b={b} />
    // 一行信号（设计 v2 §「Small signals」）：store 已经把它做成 'signal' 块，文案在
    // lib/signalFacts.ts 里由**事实**构造（右侧那一列是原始 event_type，不翻译）。
    if (b.kind === 'signal') {
      const txt = signalText(b.fact, t)
      return (
        <div className="sig-list" key={b.id}>
          <Signal
            kind={b.fact.sig}
            title={txt.title}
            {...(txt.detail ? { detail: txt.detail } : {})}
            type={b.fact.etype}
          />
        </div>
      )
    }
    // 老快照里没有 'signal' 块（checkpoint 以前写的是 system 块）：仍按信号行渲染，
    // 但文案只能取那一段文本本身 —— 不为新形态丢掉历史转录。
    if (b.kind === 'system' && skin === 'runtime') {
      const [first, ...rest] = b.text.split('\n').map((l) => l.trim()).filter(Boolean)
      const kind = b.tone === 'error' ? 'persist' : b.tone === 'success' ? 'closed' : 'silent'
      return (
        <div className="sig-list" key={b.id}>
          <Signal kind={kind as never} title={(first || b.text).slice(0, 120)} detail={rest.join(' ').slice(0, 160)} type="system" />
        </div>
      )
    }
    if (b.kind === 'system') {
      return (
        <div className={`msg system ${b.tone || 'info'}`} key={b.id} role="status">
          <span className="sys-dot" aria-hidden="true" />
          <span className="body"><InlineCode text={b.text} /></span>
        </div>
      )
    }
    if (b.kind === 'user' && skin === 'runtime') return <UserBlock key={b.id} b={b} />
    /* 用户自己说的那句话：current/v3 走这条，runtime 走上面那条 UserBlock。
       **这条分支不能少** —— 函数末尾是 `if (b.kind !== 'assistant') return null`，
       少一条分支就等于默认皮肤（narrative_skin 未设 → current）下用户消息整段不渲染
       （F43：6d28d60 把这块连同 system/task_result 的卡片一起删了，但那几样是「事件」，
       这一样是**对话本身**）。它的样式一直在：index.css 的 .msg.user .who b / 
       .msg.user .body p（pre-wrap —— 用户按的 ⏎ 必须原样回来）/ .user-imgs / .user-refs。 */
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
          {/* 运行时皮肤：产出卡照 parts/artifacts.js 的形态（主卡 + 另外 N 个 + 展开剩余）。
              其他皮肤仍走原 ArtifactCard（两条路径都在，切换皮肤不丢功能）。 */}
          {skin === 'runtime' ? (
            <ArtifactsBlock
              items={b.items}
              {...(b.truncated ? { truncated: true } : {})}
              onPreview={(a) => openFile(a.path)}
              onReveal={(a) => {
                const d = window.desktop
                if (!d?.showItemInFolder) return
                void d.showItemInFolder(absArtifactPath(a.path, workspace))
              }}
            />
          ) : (
            <ArtifactCard items={b.items} {...(b.truncated ? { truncated: true } : {})} />
          )}
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
          {b.thinking && !opts?.textOnly && (
            <Thinking text={b.thinking} live={b.streaming} thinkMs={b.thinkMs} thinkStart={b.thinkStart} />
          )}
          {b.text || b.streaming ? (
            <div className="md-text">
              {/* streaming 透传：流式中 HTML 围栏不挂 iframe（半截 <script> 会被执行、
                  且 srcDoc 每 token 重载 = 白板闪烁，见 HtmlPreviewBlock 注释）。 */}
              {b.text ? <Markdown text={b.text} streaming={b.streaming} /> : null}
              {/* 运行时皮肤不画这一行：设计的轮尾只有一条账（calls · tokens · cache ·
                  s turn，TTFT/LLM/tok-s 在悬停里展开）。这行的 TTFT/LLM/Agent/速度与那条账
                  是同一份事实，两行并排等于把同一个数报两遍 —— 见 rt-turn-foot 的 Ledger。 */}
              {!b.streaming && skin !== 'runtime' && (
                <AssistantTurnMeta b={b} />
              )}
              {/* 流式光标：设计是 6×14 的 accent 方块，闪在 steps(1) 上（`.rt-caret`，
                  1.05s）—— 一个字符做不到方块的高宽比，也闪不出阶梯而不是渐变 */}
              {b.streaming ? <span className="rt-caret" aria-hidden="true" /> : null}
            </div>
          ) : null}
        </div>
      </div>
    )
  }

  // 铺开进度：只渲染尾部 revealCount 个（见上方「历史分批铺开」）。
  // 从尾部开始，是因为阅读锚点在底部（最新消息）。
  // revealStart 是绝对下标：0 = 全部显示。铺开过程中它单调减小、永不后移，
  // 因此流式追加新块不会挤掉前面已渲染的内容（见上方注释）。
  const visibleBlocks = revealStart <= 0 ? blocks : blocks.slice(revealStart)

  // 连续工具块归组（并行工具上下排列），其余原样。抽出来是因为它现在要按「轮」
  // 跑一遍，而不是对整条转录跑一遍。
  const groupBlocks = (list: MsgBlock[], textOnlyId?: number): (ReactElement | null)[] => {
    const out: (ReactElement | null)[] = []
    for (let i = 0; i < list.length; i++) {
      const b = list[i]
      // 连续的子 agent 合成一条「车道」：设计里它们是一块（组标题 + 每行四个事实），
      // 不是一张张并列的卡片。另两档皮肤保留卡片（那是它们验证过的样子）。
      if (b.kind === 'agent' && skin === 'runtime') {
        const group: Extract<MsgBlock, { kind: 'agent' }>[] = [b]
        while (i + 1 < list.length && list[i + 1].kind === 'agent') {
          group.push(list[i + 1] as Extract<MsgBlock, { kind: 'agent' }>)
          i++
        }
        out.push(<Lane key={'la' + group[0].id} agents={group} onOpen={openAgent} />)
        continue
      }
      if (b.kind === 'tool') {
        const group: Extract<MsgBlock, { kind: 'tool' }>[] = [b]
        while (i + 1 < list.length && list[i + 1].kind === 'tool') {
          group.push(list[i + 1] as Extract<MsgBlock, { kind: 'tool' }>)
          i++
        }
        out.push(<ToolGroup key={group[0].id} tools={group} />)
        continue
      }
      if (b.kind === 'compression' && skin === 'runtime') {
        // 运行中的压缩要显示「正在读哪几条」：它们就是压缩前的那些块，一行一条
        out.push(renderBlock(b, headsOf(list.slice(0, i))))
        continue
      } else {
        out.push(renderBlock(b, undefined, textOnlyId != null && b.id === textOnlyId ? { textOnly: true } : undefined))
      }
    }
    // 逐块错峰：设计用 `animation-delay: calc(var(--i) * 240ms + 80ms)`，序号由
    // 渲染处给出（设计页也是 style="--i:n"）。这里把序号挂到块自己身上，
    // 不再用 nth-child 硬写一条阶梯 —— 那条阶梯既不是 240ms，也无法超过 13 个。
    return out.map((el, i) => (el ? cloneElement(el, { style: { ...(el.props.style || {}), '--i': i } }) : el))
  }

  /* 运行时皮肤下，一轮内部先分「round」再铺开：一条 assistant（它的 reasoning 与文字）
     开一个 round，其后的工具/子 agent 归它；没有前置 assistant 的连续工具自成一段。
     设计用这个边界表达 6px（段内）与 12px（段间）—— 没有分组，那两个数就没有意义。 */
  const groupRounds = (list: MsgBlock[], textOnlyId?: number): (ReactElement | null)[] => {
    const rs = roundsOf(list)
    return rs.map((r, ri) => {
      // 派发调用（agent_spawn/…）不进转录：子 agent 由车道那一行讲完，再画一遍就是
      // 同一个子 agent 出现两次（见 batch.ts 的 isDispatchTool）
      const rItems = r.items.filter((b) => !(b.kind === 'tool' && isDispatchTool(b.name)))
      const tools = rItems.filter((b): b is Extract<MsgBlock, { kind: 'tool' }> => b.kind === 'tool')
      const others = rItems.filter((b) => b.kind !== 'tool')
      const line = tools.length ? batchLine(tools) : ''
      return (
        <div className="rt-grp" key={r.key + ri}>
          {/* 这一轮的旁白/记录照原顺序留在外面 */}
          {r.say ? groupBlocks([r.say], textOnlyId) : null}
          {others.length ? groupBlocks(others, textOnlyId) : null}
          {/* 一组调用折成一行，行本身是折角按钮（设计 .cy-act）：动词 + 对象 + 它数的量
              + 耗时，点开/收起的是**这一组逐条的行**，不牵动旁白与记录 */}
          {/* 没有批次语法可拼的调用（codemode / MCP 之类不在 BATCH_VERB 表里）**必须**逐条画：
              之前只在 line 非空时渲染 BatchGroup，于是这些工具整段消失 ——
              运行时皮肤里 codemode 行根本看不见（用户 2026-10 报障的根源）。 */}
          {line ? (
            <BatchGroup line={line} calls={tools}>{groupBlocks(tools, textOnlyId)}</BatchGroup>
          ) : (
            tools.length ? groupBlocks(tools, textOnlyId) : null
          )}
        </div>
      )
    })
  }

  /* 一轮 = 一条 user 消息到下一条之间。store 里没有这个对象（见 PORT-PLAN §5），
     所以这里派生：轮尾账要 usage、轮首要有折叠行，两者都靠 selectTurns 的归属。

     折叠的**只有工作过程**，不含回答 —— v2 里折叠行包的是 groups（工具/子智能体），
     答案在它外面。所以简洁档收起时渲染的是「非工作块」，展开才回到原顺序。 */
  const turns = selectTurns(visibleBlocks)
  const items: (ReactElement | null)[] = []
  /* 复用判据里的「全局输入」：本组件读到的每个 store 值与本地状态（storeBlocks 除外 ——
     它每个 chunk 都会变，逐轮比块引用就够了）。 */
  const turnGlobals: readonly unknown[] = [
    lang, skin, density, runEnded, waiting, persona, workspace, openFile,
    turnMetrics, openTurns, animOn, runStartedAt, mainStartedAt, mainRunId, mainEnded, transcriptKey,
  ]
  const sameList = (a: readonly unknown[], b: readonly unknown[]) => a.length === b.length && a.every((v, i) => v === b[i])
  const turnCache = turnCacheRef.current
  for (const [ti, turn] of turns.entries()) {
    const stats = turnStats(turn)
    const usage = turnUsage(turn, turnMetrics)
    const head = turnHead(turn)
    const turnStart = head.t0
    const concise = density === 'concise'
    /* 这一轮还在不在跑 = **主 run 交卷了没有**（AgentEnd 是唯一折叠时刻）。
       AgentEnd 之后一律折叠：块上残留的 running（后台子 agent / 被 promote 的工具）
       属于任务自己的生命周期，不得把整轮重新拉回 Working。见 lib/turnSettle.ts。 */
    /* sameRun：这一轮的 run 与正在跑的那次运行是不是同一个。没有 runId 的旧数据
       （块上缺 run_id）按「同一轮」处理 —— 宁可沿用老行为，也不要凭空把在跑的轮判死。 */
    const sameRun = !turn.run || !activeRunId ? true : turn.run === activeRunId
    /* 结算边界（观测量）：这一轮上一次「真的静下来」时有多少块。边界只有在
       「下标还在、且那一段最后一块仍是同一块」时才算数 —— 轮的内容被整体替换
       （分页 / 重放 / 测试夹具重灌块，同一个 turn key 被复用）就作废。 */
    const settleRec = settledAtRef.current.get(turn.key)
    const settleAt = settleMarkValid(settleRec, turn.items) ? settleRec.at : undefined
    if (settleRec && settleAt == null) settledAtRef.current.delete(turn.key)
    const rawLive = turnLive({ isLast: ti === turns.length - 1, turnRunning: sameRun && turn.running, runEnded, sameRun })
    /* 结算过、且**没有新块**时，`live` 再被点亮也不认（渲染层面）。
       专治「主 run 已 AgentEnd、后台 run 起跑（没有用户输入、块上也没有 run id）」把已经
       收起的一段重新展开：探针 lab/probe-settled-fold-relight 里同一批内容会从「Worked
       行 + 答案在时间线上」变成「一个 Working 盒子（答案被挪进去）」—— 用户 2026-11
       看到的上下抖就是这个。有新块时**不锁**：它由 turnRuns 的结算边界切出自己的段
       （见下方 runs），上半段仍然保持折叠 —— 既有口径「同一个 turn 里第二个 run 起新
       盒子」不受影响。 */
    const live = rawLive && !(settleAt != null && settleAt === turn.items.length)
    // 正在跑的这一轮默认敞开：折叠行的脸能说明它在动，但看不见它在做什么，
    // 用户就没法判断它卡在哪。跑完再按用户的选择收起。
    const open = !concise || (openTurns[turn.key] ?? live)
    /* 盒子装什么、要不要画盒子：同一个事实源（workBody 非空 = 有东西可装）。
       之前「有没有工具」决定画不画盒子 —— async_task-only / system-only 的轮次
       workBody 有行却没有盒子，那些行就整轮消失。 */
    // 「只做了压缩的那次运行」（AgentEnd 之后手动 /compact 会新开一次 run）整段是
    // 通知，不是过程：压缩块与 compress_end 信号走时间线，不画 Working/Worked 盒子。
    // 口径见 selectTurns.noticeBlocks；数值口径（workedMs / wallMs / usage）不受影响。
    const notice = noticeBlocks(turn)
    const workBlocks = workBody(turn, notice)
    const work = hasWork(turn, notice)
    /* 运行中的盒子内容 = 工作行 + 正在写的那一段正文（turn.answer）。
       正文在交卷前属于「工作过程」（外面只剩记录），但它不在 workBody 里（那里排除了
       answer）—— 少了这一步，工具轮的流式正文会既不在盒子里也不在外面。
       记录（信号/回执）不在这个集合里：它们是时间线，外面已经画过。 */
    const workSet = new Set(workBlocks)
    const liveBox = turn.items.filter((b) => workSet.has(b) || b === turn.answer)
    const isWork = (b: MsgBlock) => FOLDED.has(b.kind)
    /* 简洁档：收起时正文只剩**结论**（这一轮最后一个有正文的 assistant），中间那些
       「我看看 X」的旁白、思考、工具全在工作过程里。展开才按原顺序读全过程。
       详细档：原顺序整段展开，不折叠（v2 里详细档根本没有折叠行）。 */
    const workItems = concise
      ? (skin === 'runtime'
        ? groupRounds(workBlocks)
        : groupBlocks(turn.items.filter((b) => !notice.has(b) && (isWork(b) || (b.kind === 'assistant' && b !== turn.answer) || b.kind === 'compression'))))
      : []
    /* 简洁档的外面 = **时间线**：记录（signal / 产出 / 错误 / 分隔线 / 后台任务回执），
       加上结论正文。工作内容（思考、旁白、工具、子 agent、压缩）无论运行中还是跑完、
       无论展开还是收起，都只存在于工作盒子里 —— 展开的是盒子，不是把行摊进流里。
       运行中：盒子存在时正文还在盒子里长（到 AgentEnd 交出来）；**没有盒子时正文就是
       时间线**（纯对话轮没有工作过程可装，把它关进不存在的盒子等于整轮不显示）。
       （旧写法 `open ? items : foldedBody` 会在展开时把整轮再画一遍到外面，
       实测盒子 3 行、外面 20 行 —— 用户在截图里看到的正是这个。） */
    const bodySource = concise && live ? runningBody(turn, !work, notice) : concise ? foldedBody(turn, notice) : turn.items
    /* 一轮可能跨**多次运行**（后台任务回推后续跑）：盒子按 run 切段（见 selectTurns.turnRuns）。
       声明提到这里，是因为下面的「结论 / 账 / 卡」闸门都要按**段**判，而不只按整轮判。 */
    // 记结算边界：只有在「真的静下来了」才记 —— 视图说这次运行已结算（AgentEnd），
    // 且这一轮里没有还在流式 / 在跑的块。下标取当前块数：之后再到的块由 turnRuns
    // 切出自己的段，上半段保持折叠。
    //
    // 为什么不用「runEnded 由假变真」：事件可能成批到达（一次渲染里 start+end 都在），
    // 那次转换根本观测不到。为什么不用 `!live && answer`：块比 agent_start 先到或根本
    // 缺 agent_start 时，`live` 会在运行中途读成 false，把「跑了一半」记成「结算过」，
    // 随后插话 / 续跑的块被切到第二段、整轮凭空多一个盒子（实测 test:transcript 场景 C）。
    if (runEnded && ti === turns.length - 1 && turn.answer && !turn.answer.streaming && !turn.running) {
      // 旧边界仍有效就不要往后挪：AgentEnd 之后又来的工具/记录不能把多段缩回一段，
      // 否则 Worked 盒的 state key 从 turn#1 切回 turn，刚点开的盒子会弹回收起。
      settledAtRef.current.set(turn.key, rememberSettleMark(settleRec, turn.items))
    }
    const runs = concise && skin === 'runtime' ? turnRuns(turn, settleAt) : []
    const multiRun = runs.length > 1
    /* 输入全都没变 → 直接交回上一次的那几个元素（React 看到同一个元素对象、props 同一个，
       整棵子树 bailout，连 beginWork 都不进）。 */
    const turnState: readonly unknown[] = [open, live, multiRun, ti, turns.length]
    const hit = turnCache.get(turn.key)
    if (hit && sameList(hit.items, turn.items) && sameList(hit.globals, turnGlobals) && sameList(hit.state, turnState)) {
      items.push(...hit.els)
      continue
    }
    const startIdx = items.length
    const remember = () => {
      turnCache.set(turn.key, { items: turn.items, globals: turnGlobals, state: turnState, els: items.slice(startIdx) })
    }
    if (turn.user) items.push(renderBlock(turn.user))
    /* 用户 2026-10：轮尾账排在产出卡**上方** —— 先结算这一轮，再看这一轮的产出。
       产出卡仍是轮尾最后一块（它是「记录」，不属于工作过程）。只在运行时皮肤、且账
       真的会渲染时把产出卡拆出来；运行中/无用量归属的轮保持原序（账本来就不出现）。 */
    const ledgerShown = usage.matched && !live
    /* 交卷**之后**到达的记录（signal / 后台任务回执）不属于这一轮的结论，也不许排在账上面：
       用户 2026-11 的口径 —— 这类通知只有两个合法位置，要么在 Working 盒子里（运行中到达），
       要么在 tokens/artifacts **之后**（AgentEnd 之后到达）。 */
    const outside = outsideOf(turn, notice)
    const postRecord = (b: MsgBlock, i: number) =>
      // 通知段（见 noticeBlocks）里的记录不算「交卷后到达」：它们跟着自己的压缩走。
      // 一旦被当成 postRecord 就会延后到轮尾账/产出卡之后 —— 读起来是「先结算、再压缩」，
      // 与事实相反（用户 2026-11：两个压缩通知要被拆到结算两侧）。
      !notice.has(b) && (b.kind === 'signal' || b.kind === 'task_delivery') && outside(b, i)
    /* 一轮可能跨多次运行（后台任务回推后续跑），每 run 一个产出块 —— 卡片只画一张，
       按 path 合并（见 mergeArtifacts）。store 里仍是每 run 一块。
       闸门是 `!live`（AgentEnd 已到）而**不是** ledgerShown：产出是 AgentEnd 的事实，
       与「这一轮有没有拿到 usage 指标」无关（没有 llm_end 的轮次也该看到产出清单）。 */
    const artRaw = skin === 'runtime' && !live ? bodySource.filter((b) => b.kind === 'artifacts') : []
    const artBlocks = artRaw.length > 1 ? [mergeArtifacts(artRaw)] : artRaw
    const noArt = artBlocks.length ? bodySource.filter((b) => b.kind !== 'artifacts') : bodySource
    /* 交卷之后到达的记录（见 postRecord）一律排在**结算之后**：账与产出卡是这一轮的交付，
       通知不属于交付序列（用户 2026-11 口径）。但「结算」是**分段**的：一轮可能跨多次运行
       （后台任务回推 → 续跑），只有**最后一段**的记录才该延后到轮尾（合并且只有一份的账/卡）；
       前几段的记录必须跟它自己那段的盒子走，否则四条 `Task … completed` 会全部堆在最后一个
       Worked 后面（用户 2026-11 截图）。还在跑的段由 plan 的分段结算负责（那里也保证记录
       落在账/卡之后）。 */
    const postBlocks = skin === 'runtime' && !live
      ? turn.items.filter((b, i) => postRecord(b, i) && defersToTurnEnd(runs, i))
      : []
    const bodyBlocks = postBlocks.length ? noArt.filter((b) => !postBlocks.includes(b)) : noArt
    const body = skin === 'runtime' ? groupRounds(bodyBlocks) : groupBlocks(bodyBlocks)
    /* —— 情况二（一轮多次运行）：盒子按 run 分开，记录留在段与段之间的时间线上 ——
       用户 2026-11 的两个形态：run1 AgentEnd → 异步任务结果通知（顶层、盒外）→ run2 新盒子。
       盒子是按 run 算的，所以这里按 turn.items 的**顺序**交错渲染：
         [盒子A] … [盒间记录] … [盒子B] … [整轮的结论正文]
       单 run（情况一）继续走上面那套老路径，一行不变。 */
    const segAnswerAt = answerIndex(turn)
    /* 一段运行的**结算**（结论正文 / 轮尾账 / 产出卡）归它自己，不归「整轮」：
       下一段运行一起跑，整轮的 `live` 又变真、`turn.answer` 又移走 —— 拿这两个当闸门，
       上一段已经交付的东西就整块消失（用户 2026-11 截图：`Worked for 41m10s` 之后来了
       `Task … completed`，上一段的 llm_content 被收回盒子、产出卡与 tokens 都不见了）。
       所以：**已交卷的段**在盒外自己结算（正文对自己的最后一块、账按自己的 run、卡按自己的
       AgentEnd），**还在跑的段**照旧正文留在盒子里长。整轮都交卷时仍走老路径（合并成一张
       卡 + 一行账，见下面的 artBlocks/ledgerShown）。 */
    const segs = multiRun
      ? runs.map((r, i) => {
          const segLive = i === runs.length - 1 && live
          /* 结论按**这一段自己**的最后一段正文（整轮的 turn.answer 只等于「最后那段」的结论，
             拿它当每一段的结论，前几段交付的正文就会被收回各自的 Worked 盒子 ——
             用户 2026-11 两连问：先问「正文进了盒子」，再问「刷新后上方的 Worked 为什么
             没有 llm_content 在外侧」。两处是同一件事：整轮交卷了也一样按段判）。 */
          const segAnswer = segLive ? undefined : answerOf(r.items)
          const its = runWorkBody(r.items, {
            answer: segAnswer,
            answerAt: segAnswerAt,
            offset: r.start,
            notice,
            // 邻居兜底按整轮看（段首记录的前一次运行在上一段里），见 selectTurns.recordAfterRun
            all: turn.items,
          })
          const segUsage = segmentUsage(turn, r, turnMetrics)
          return { run: r, items: its, stats: statsOfItems(r.items), live: segLive, answer: segAnswer, usage: segUsage }
        })
      : []
    /* 时间线上的块只可能是记录/结构块（工具行永远在工作过程里）—— 类型上收窄一次，
       下面的 renderBlock 才接得住。 */
    type PlanBlock = Exclude<MsgBlock, { kind: 'tool' }>
    type ArtBlock = Extract<MsgBlock, { kind: 'artifacts' }>
    /* 已交卷那一段的**结算**插在它自己最后一块之后：盒子 → 正文 → 账 → 产出卡 → 回时间线。
       只在下半段还在跑（`live`）时这样画：整轮都交卷后由轮尾统一的账与合并卡负责，
       两套不能同时开（会画两遍）。 */
    const settles = new Map<number, number>() // turn.items 下标 → segs 下标
    /* 每一段自己的结论：**永远**在盒外（「上方的 Worked 没有 llm_content 在外侧」= 它被折进盒子了） */
    const segAnswerSet = new Set<MsgBlock>()
    segs.forEach((s) => { if (s.answer) segAnswerSet.add(s.answer) })
    /* 分段结算（账 / 卡跟随它自己那一段）只在「下半段还在跑」时用：整轮交卷后由轮尾统一的
       一行账 + 合并一张卡负责，两套不能同时开（会画两遍）。 */
    if (multiRun && live) {
      segs.forEach((s, i) => {
        if (s.live) return
        const hasArt = s.run.items.some((b) => b.kind === 'artifacts')
        if (s.usage.matched || hasArt) settles.set(settleIndexOf(turn, s.run, s.answer), i)
      })
    }
    const plan: ({ box: number } | { b: PlanBlock } | { settle: number })[] = []
    if (multiRun) {
      const boxAt = new Map<number, number>()
      segs.forEach((s, i) => { if (s.items.length) boxAt.set(s.run.start, i) })
      for (let i = 0; i < turn.items.length; i++) {
        // 盒子只在段首出现一次；**不跳过**段内其余下标 —— 段内可能夹着「交卷后到达」
        // 的记录（上一段运行刚结束、下一段还没开始），它们要在盒子之后按顺序落出来。
        const bi = boxAt.get(i)
        if (bi != null) plan.push({ box: bi })
        const b = turn.items[i]
        // 工具行永远在工作过程里；产出卡不进时间线（轮尾合并渲染，或跟随已交卷段的结算）
        if (b.kind !== 'tool' && b.kind !== 'artifacts') {
          // 交卷之后到达的记录：只有**最后一段**的统一延后到轮尾结算后面（见 postBlocks），
          // 这里跳过；前几段的记录与自己那段的盒子一起落在两个盒子之间。
          const deferred = postBlocks.includes(b)
          // 已交卷那一段的结论：即使整轮还在跑，它也已经交出去了，必须留在时间线上
          if (deferred) { /* 见 postBlocks */ }
          else if (segAnswerSet.has(b)) plan.push({ b })
          // 结论正文只在**交卷之后**才上线：运行中它在最后一段的盒子里长
          else if (b === turn.answer) { if (!live) plan.push({ b }) }
          else if (outside(b, i)) plan.push({ b })
        }
        const si = settles.get(i)
        if (si != null) plan.push({ settle: si })
      }
    }
    /* 轮尾账的 markup 只写一份：轮尾（整轮交卷）与分段结算（上一段交了卷、下一段还在跑）
       是同一块东西，两处必须一模一样。 */
    const ledgerEl = (st: TurnStats, u: TurnUsage, key?: string) => (
      <div className="rt-turn-foot" key={key}>
        <Ledger
          calls={st.calls}
          tokens={u.total}
          cacheRead={u.cacheRead}
          input={u.input}
          wallMs={st.wallMs ?? st.workedMs}
          ttftMs={u.ttftMs}
          llmMs={u.llmMs}
          outputTokens={u.output}
        />
      </div>
    )
    const hasArtifacts = turn.items.some((b) => b.kind === 'artifacts')
    if (!body.length && !artBlocks.length && !workItems.length && !work) { remember(); continue }
    items.push(
      <div
        className="rt-turn"
        data-density={density}
        data-open={open ? '1' : undefined}
        /* data-live：这一轮**正在跑**（区别于「展开了」）。运行时皮肤用它把
           content-visibility 只关在真正长高的那一段上 —— 关到所有 data-open 上等于
           把整条转录的离屏跳过全关掉（detailed 密度下每个轮都是 open，实测 420 块
           挂载：layout/style 160ms vs 旧皮肤 28ms）。 */
        data-live={live ? '1' : undefined}
        data-foot={usage.matched ? '1' : undefined}
        key={turn.key}
      >
        {skin === 'runtime' && (head.model || head.t0 != null) ? (
          <div className="rt-head">
            {head.model ? <span>{head.model}</span> : null}
            {/* 轮头 = 「哪个模型 · 什么时候」。用户 2026-10：只要时刻读不出这是哪天 ——
                隔天回来翻记录时，07:57:27 到底是今天还是上周的完全无法判断。
                日期用 fmtClock 的全量写法（YYYY-MM-DD HH:MM:SS，与消息时间戳同一函数）。 */}
            {head.t0 != null ? <span className="rt-t" title={fmtClock(head.t0)}>{fmtClock(head.t0)}</span> : null}
          </div>
        ) : null}
        {/* 一轮多次运行（情况二）：按 run 分开的盒子 + 段间记录，按 turn.items 顺序交错。
            单 run（情况一）走下面那两条老分支，判据完全不变。 */}
        {multiRun
          ? plan.map((p, pi) => {
              if ('b' in p) {
                /* 盒与盒之间的时间线（异步任务结果通知、会话级提示…）：顶层，不缩进，
                   与 working 盒同级。结论正文保留「交代」的 16px 节奏，其余按记录自己走。 */
                const el = renderBlock(p.b)
                if (!el) return null
                return p.b === turn.answer || segAnswerSet.has(p.b)
                  ? <div className="rt-turn-ans" key={`pl-${pi}`}>{el}</div>
                  : <div className="rt-turn-line" key={`pl-${pi}`}>{el}</div>
              }
              if ('settle' in p) {
                /* 已交卷那一段的结算：账（按**那一段自己的 run** 结算）+ 它自己的产出卡。
                   下半段还在跑时，这两块不许消失（用户 2026-11：`Task … completed` 一到，
                   上一段的 tokens 与产出卡整块不见了）。 */
                const s = segs[p.settle]
                if (!s) return null
                const arts = s.run.items.filter((b): b is ArtBlock => b.kind === 'artifacts')
                const card = arts.length > 1 ? [mergeArtifacts(arts)] : arts
                if (!s.usage.matched && !card.length) return null
                return (
                  <Fragment key={`st-${pi}`}>
                    {s.usage.matched ? ledgerEl(s.stats, s.usage) : null}
                    {card.length ? <div className="rt-turn-artifacts">{groupRounds(card)}</div> : null}
                  </Fragment>
                )
              }
              const seg = segs[p.box]
              if (!seg) return null
              const segLive = seg.live
              const rows = groupRounds(seg.items)
              if (segLive) {
                // 这一段自己的起点（首块的时间戳）—— 不是整轮的起点，否则第二个盒子的钟从 0 起涨
                const first = seg.run.items[0]
                const startedAt = first && 'startedAt' in first && typeof first.startedAt === 'number'
                  ? first.startedAt
                  : first && 'ts' in first && typeof first.ts === 'number' ? first.ts : turnStart
                return (
                  <LivePanel key={seg.run.key} items={seg.items} startedAt={startedAt ?? runStartedAt} waiting={waiting}>
                    <div className="rt-turn-in">{rows}</div>
                  </LivePanel>
                )
              }
              const segKey = `${turn.key}#${p.box}`
              const segOpen = openTurns[segKey] ?? false
              return (
                <WorkBox
                  key={seg.run.key}
                  stats={seg.stats}
                  running={false}
                  open={segOpen}
                  onToggle={() => setOpenTurns((m) => ({ ...m, [segKey]: !segOpen }))}
                >
                  {rows.length ? rows : null}
                </WorkBox>
              )
            })
          : null}
        {/* 「Worked for 8.2 s」是**结算**，不是进度：运行中这一行只该是「正在做什么」，
            汇总的时长与批次要到 AgentEnd 才成立 —— 与轮尾账同一时刻出现。 */}
        {!multiRun && concise && work && live ? (
          /* Working = 整次运行（AgentStart → AgentEnd）：标题行 + 这一轮的全部内容
             （思考、中间的旁白、工具行、子 agent），可滚动、有高度上限。
             内容取 liveBox（工作行 + 正在写的正文），不是 turn.items —— 记录（信号/回执）
             属于时间线，已经在外面渲染过，再塞进面板就是同一行画两遍。 */
          <LivePanel items={liveBox} startedAt={runStartedAt ?? turnStart} waiting={waiting}>
            <div className="rt-turn-in">{groupRounds(liveBox)}</div>
          </LivePanel>
        ) : null}
        {/* 结算后：收起是一行 Worked for，展开还是**同一个盒子**（头 + 内容，46vh 封顶）。
            以前展开是把这些行摊进流里，于是「跑完的一轮」看起来仍在跑：答案被推到
            一堵工具行下面，而本该兜住它们的盒子没有底。外面从头到尾只有最后一轮正文。 */}
        {!multiRun && concise && work && !live ? (
          <WorkBox
            stats={stats}
            /* 结算盒永远不是“在跑”：`!live` 已经是 AgentEnd 之后的唯一分支
               （turnSettle: runEnded 压过 turnRunning）。这里传 turn.running 会让
               「主 run 已交卷、后台任务还在跑」的盒子显示成 Working · …（时钟还在涨），
               而它本该是 Worked for <时长>。 */
            running={false}
            open={open}
            onToggle={() => setOpenTurns((m) => ({ ...m, [turn.key]: !open }))}
          >
            {workItems.length ? workItems : null}
          </WorkBox>
        ) : null}
        {/* 多 run 时时间线上的块已经按顺序进了 plan（含结论正文），不再整体再画一遍 */}
        {!multiRun && body.length ? <div className="rt-turn-ans">{body}</div> : null}
        {/* 这一轮改了哪个文件：记录（带框卡），点它展开做出改动的工作行 */}
        {/* 简洁档收起时不再单独画文件行：这条信息已经在 Worked for 那一行里
            （「Edited 1 file +22 −17」），再画一遍是同一个数字说两次。
            有产出卡的轮次也不画：卡里已经逐个列了这次产出的文件，这张卡是同一份
            事实的第三份拷贝（用户 2026-10 截图里「很突兀」的那一行）。 */}
        {skin === 'runtime' && !live && !hasArtifacts && stats.files > 0 && (!concise || open) ? (
          <FilesLine
            path={firstFileOf(turn)}
            added={stats.added}
            removed={stats.removed}
            {...(concise ? { onOpen: () => setOpenTurns((m) => ({ ...m, [turn.key]: !open })) } : {})}
          />
        ) : null}
        {/* 轮尾账是**结算**，不是进度：AgentEnd 之前这一轮的用量还在长，
            把「11 calls · 16k tokens」边跑边刷出来就是在报一个还没定的数。 */}
        {ledgerShown ? ledgerEl(stats, usage) : null}
        {/* 产出卡排在轮尾账之后（用户 2026-10 的轮尾顺序）：账说「这一轮花了多少」，
            卡说「这一轮产出了什么」；卡仍是最后一块，轮间距由它承担（见 turn.css）。 */}
        {artBlocks.length ? (
          <div className="rt-turn-artifacts">{groupRounds(artBlocks)}</div>
        ) : null}
        {/* 结算之后到达的记录（见 postRecord）：账和卡是这一轮的交付，通知排在它们**后面** */}
        {postBlocks.map((b, pi) => {
          if (b.kind === 'tool') return null
          const el = renderBlock(b)
          return el ? <div className="rt-turn-line" key={`post-${pi}`}>{el}</div> : null
        })}
      </div>,
    )
    remember()
  }

  return (
    <div
      className={`narrative${animOn ? ' narrative-enter' : ''}${crossFade ? ' is-crossfade' : ''}`}
      // ## 为什么这里**没有** key（2026-10-21 修 bug）
      //
      // 上一版把 enterAnimAt 放进 key 来「重播渐入动画」：
      //     key={`n-${enteredAt}-${enterAnimAt}`}
      // 后果是每次 enterAnimAt 变化 → 组件重新挂载 → DOM 重建 → **scrollTop 归零**
      // → 贴底校准循环发现 scrollTop !== max → 把视图拉回底部。
      // 用户感受就是「上滑看历史总被拽回底部」。
      //
      // 重播动画不需要重挂载：用 data-enter 属性变化 + CSS animation 就够
      //（属性的变化会重启同名的 animation，前提是 animation-name 相同但
      // 属性选择器不同 —— 见 index.css 的 .narrative-enter[data-enter]）。
      // 这里用 key 只锁「会话 id」，切会话才重建（那时本来就该丢滚动位置）。
      key={transcriptKey}
      ref={setNodeRef}
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
      {header}
      {blocks.length === 0 && (source ? source.empty ?? null : <EmptyChat persona={persona} />)}
      {items}
      {footer}
      <div className="narrative-anchor" ref={anchorRef} aria-hidden="true" />
      {/* 「↓ 回到最新」不能占**流内高度**（2026-11 修「底部内容原地上下颤」）。
          这个按钮原来直接放在流里（position: sticky 并不脱离文档流）：它出现/消失会把
          scrollHeight 改掉 30px（实测 13056 → 13086）。贴底态下 tick 每帧把 scrollTop
          钉到 max —— 于是「跟随」一旦在某一帧被误判成 false、下一帧又被恢复，整段内容
          就会跟着 ±30px 上下弹，而滚动位置一直没变（正是用户描述的那个形态）。
          外层是零高 sticky 容器（视觉落点与原来一致），按钮 translateY(-100%) 抬到
          容器上方：出现与否都不再影响内容高度。 */}
      {(away || fresh) && (
        <div className="narrative-jump-wrap">
          <button
            className="narrative-jump"
            onClick={jumpToBottom}
            aria-label={t('narrative.backToLatest')}
            title={fresh ? t('narrative.newArrived') : t('narrative.backToLatest')}
          >
            {fresh && <span className="narrative-jump-dot" aria-hidden="true" />}
            ↓ {fresh ? t('narrative.newOutput') : t('narrative.jumpLatest')}
          </button>
        </div>
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
