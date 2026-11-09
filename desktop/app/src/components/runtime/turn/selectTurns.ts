import type { ArtifactItem, MsgBlock, TurnMetric } from '../../../store/useAppStore'
import { fmtDuration } from '../../../lib/duration'

/* The transcript is a flat list of blocks — there is no "turn" object anywhere
   in the store (see PORT-PLAN §5: `turns[]` holds metrics only, the content
   lives in `blocks[]`). A turn is therefore *derived*: it opens on a user
   message and runs until the next one. Everything in between — thinking, tools,
   subagents, compressions, artifacts — belongs to it. */
type M<K extends MsgBlock['kind']> = Extract<MsgBlock, { kind: K }>

export interface Turn {
  key: string
  index: number
  user?: M<'user'>
  /* 这一轮属于哪一次运行（AgentStart 的 run_id，来自块上的 runId）。
     轮是**派生**的（user 块到下一个 user 块），运行却是显式的 —— 两者不是一一对应：
     后台子 agent / promoted 工具会把一个 run 的余温带进下一次运行。判「这一轮还在不在
     跑」必须同时看 run（见 lib/turnSettle.ts 的 sameRun 与用户 2026-10 的报障：
     上一轮 AgentEnd 之后又发一条消息，上一轮的 Working 被新 run 重新点亮）。
     agent 块的 runId 是**子** run，不能拿来当这一轮的 run → 只看正文与工具块。 */
  run?: string
  items: MsgBlock[]
  answer?: M<'assistant'>
  tools: M<'tool'>[]
  agents: M<'agent'>[]
  compressions: M<'compression'>[]
  artifacts?: M<'artifacts'>
  running: boolean
}

/* A turn's cost is not on any block: usage lives in `SessionView.turns[]`, one
   entry per LLM call, tagged with the `runId` it belongs to. So the join key is
   the runId the blocks already carry — no timestamp arithmetic, no guessing. */
export interface TurnUsage {
  input: number
  output: number
  cacheRead: number
  reasoning: number
  total: number
  llmMs: number
  ttftMs?: number
  matched: boolean
}

export interface TurnStats {
  calls: number
  /** measured segments only: Σ assistant.durMs + Σ tool.durMs. Not wall time —
      the gaps between them are not attributable to anything we can name.
      （并行工具会重复计入；纯工具调用轮曾因「无正文不写 durMs」整段漏计 —— 见
      store 的 llm_end：现在无论有没有正文都把 durMs 落到块上。） */
  workedMs: number
  /** 真墙钟 = 本段内每个 run 的 AgentEnd.DurationMS 之和（同 run 只取一次）。
      结算前（还在跑）没有这个值 → 调用方回落到 workedMs（滚动计时）。
      为什么要它：`turn` 这个标签承诺的是墙钟，而 workedMs 是「可归因片段之和」——
      2026-11-04 实名报障 Worked 26m28s < LLM 31m48s，就是这两把尺子混用。 */
  wallMs?: number
  files: number
  added: number
  removed: number
  agents: number
  compressions: number
}

// 文本判据必须容忍**缺字段**：Go 那边 Block.Text 是 `json:"text,omitempty"`，
// 一个「只有思考、没有正文」的 assistant 块序列化后根本没有 text 字段。历史分页
// （blocks_page）把这种块原样交给 UI，之前的 `b.text.trim()` 直接把整个转录打成
// 空白页 —— 用户只是往上滚了一屏（2026-10 报障：滚一下就 Cannot read 'trim'）。
// 一条畸形块不该让整页崩：这里按空串处理，缺什么当没有。
const textOf = (b: MsgBlock): string => (typeof (b as { text?: string }).text === 'string' ? (b as { text: string }).text : '')
const isText = (b: MsgBlock) => b.kind === 'assistant' && textOf(b).trim().length > 0

function emptyTurn(index: number, key: string): Turn {
  return { key, index, items: [], tools: [], agents: [], compressions: [], running: false }
}

function push(t: Turn, b: MsgBlock) {
  t.items.push(b)
  // 取**最后一个**带 runId 的正文/工具块：agent_resume 会在同一轮里续跑第二次 run，
  // 那一轮属于**新** run（取第一个会把续跑判成「上一轮」而永远不亮 Working）。
  if (b.kind === 'assistant' || b.kind === 'tool') {
    const r = (b as { runId?: string }).runId
    if (typeof r === 'string' && r) t.run = r
  }
  switch (b.kind) {
    case 'assistant':
      if (isText(b)) t.answer = t.answer ? (textOf(b).trim() ? b : t.answer) : b
      if (b.streaming) t.running = true
      break
    case 'tool':
      t.tools.push(b)
      if (b.status === 'running') t.running = true
      break
    case 'agent':
      t.agents.push(b)
      if (b.status === 'running') t.running = true
      break
    case 'compression':
      t.compressions.push(b)
      break
    case 'artifacts':
      t.artifacts = b
      break
    default:
      break
  }
}

export function selectTurns(blocks: MsgBlock[]): Turn[] {
  const turns: Turn[] = []
  let cur: Turn | null = null
  blocks.forEach((b) => {
    if (b.kind === 'user' && b.injected && cur) {
      /* 运行中插话（agent_loop 的插话 Poll 消费进来的一条用户消息）：它进的是**同一个
         run**，所以属于当前这一轮，按事件顺序留在 items 里 —— 不另起一轮。
         用户 2026-11 报障的形态是：Working 正在跑时再说一句，转录里冒出**第二个
         Working 盒子**（同一个 run 被切成两轮，两轮都还 live）。一次运行只有一个盒子，
         盒子内部按时间顺序说话；AgentEnd 之后整盒折叠（与工具行同样的归属）。
         没有当前轮（会话从中间恢复）时退回普通路径：它需要一个能落脚的家。 */
      push(cur, b)
      return
    }
    if (b.kind === 'user') {
      cur = emptyTurn(turns.length, 't' + b.id)
      cur.user = b
      turns.push(cur)
      return
    }
    if (!cur) {
      // a session can start mid-run (restored transcript): the first blocks have
      // no user message of their own, and they still need a home
      // key 用块 id 而不是下标：分页「加载更早」会改变可见切片，下标一变轮盒
      // 就要重挂，已展开的代码块/工具详情会丢状态。
      cur = emptyTurn(turns.length, 't0-' + b.id)
      turns.push(cur)
    }
    push(cur, b)
  })
  return turns
}

/* 一段运行 / 一轮的统计。抽成 items 版本是为了**按 run 分别结算**：简洁档一个 run
   一个盒子，每个盒子的「Worked for 2m · 3 calls」必须是**这一段自己的**数，而不是整轮
   合起来的（用户 2026-11 的两形态：一轮里可以有两次运行，两个 AgentEnd）。 */
export function statsOfItems(items: MsgBlock[]): TurnStats {
  const files = new Map<string, { add: number; del: number }>()
  let workedMs = 0
  items.forEach((b) => {
    if (b.kind === 'assistant' && b.durMs) workedMs += b.durMs
    if (b.kind === 'tool' && b.durMs) workedMs += b.durMs
    if (b.kind === 'tool' && b.diff && b.diff.path) {
      const row = files.get(b.diff.path) || { add: 0, del: 0 }
      row.add += b.diff.added ?? 0
      row.del += b.diff.removed ?? 0
      files.set(b.diff.path, row)
    }
  })
  let added = 0
  let removed = 0
  files.forEach((f) => { added += f.add; removed += f.del })
  // 墙钟：AgentEnd.DurationMS 只挂在该 run 的末条 assistant 块上（agentDurMs）→
  // 同 run 取一次（max），一轮内多次运行（续跑/分段结算）串行 → 相加。
  // 没有 runId 的旧数据只能按块累加（退化，但仍优于直接用 workedMs）。
  const wallByRun = new Map<string, number>()
  let wallAnon = 0
  items.forEach((b) => {
    const d = (b as { agentDurMs?: number }).agentDurMs
    if (!d || d <= 0) return
    const r = (b as { runId?: string }).runId
    if (r) wallByRun.set(r, Math.max(wallByRun.get(r) ?? 0, d))
    else wallAnon += d
  })
  let wallMs = wallAnon
  wallByRun.forEach((d) => { wallMs += d })
  return {
    calls: items.filter((b) => b.kind === 'tool').length,
    workedMs,
    ...(wallMs > 0 ? { wallMs } : {}),
    files: files.size,
    added,
    removed,
    agents: items.filter((b) => b.kind === 'agent').length,
    compressions: items.filter((b) => b.kind === 'compression').length,
  }
}

export function turnStats(turn: Turn): TurnStats {
  return statsOfItems(turn.items)
}

/* The turn head is one dim line: which model answered it, and when it opened.
   The model is taken from the answer when there is one — a turn can switch
   models halfway, and the head should name the one that produced the text, not
   whichever call happened to come first. */
export interface TurnHead {
  model?: string
  t0?: number
}

export function turnHead(turn: Turn): TurnHead {
  const fromAnswer = turn.answer?.model
  const fromItems = turn.items.find((b) => b.kind === 'assistant' && b.model)
  const model = fromAnswer || (fromItems && fromItems.kind === 'assistant' ? fromItems.model : undefined)
  // 轮的开口时刻 = 用户那句话的时刻；没有 user 块（会话截断）才退到首块
  const first = turn.user ?? turn.items.find((b) => 'ts' in b && b.ts)
  const t0 = first && 'ts' in first ? first.ts : undefined
  return { model, t0 }
}

export function turnUsage(
  turn: Turn,
  metrics: TurnMetric[] | undefined,
  /* 只结算这几段 run（一轮多次运行时的**分段结算**：上一段已交卷、下一段还在跑，
     轮尾账只能报已经定下来的数，见 Narrative 的 settles / settleIndexOf）。省略 = 整轮。 */
  only?: ReadonlySet<string>,
): TurnUsage {
  /* 只算**这一轮自己的 run**（正文块上的 runId）—— 子 agent 的 runId 不算。
     子 agent 的用量会持续到主 run 交卷之后（后台子 agent / promoted 工具），
     而它那张卡一直留在这一轮的 items 里；把子 run 也算进来，已经结算的轮尾账
     会在**下一次运行开始后继续变大**（用户 2026-11 报障：AgentEnd 时是 7k，
     发下一条消息变成 17k，LLM 秒数也跟着涨）。子 agent 的花费在它自己的卡片上
     （card.usage，TaskEnd 结算），不在这里重复记一遍。
     一次轮里可能有多个主 run（agent_resume 原地续跑），所以是集合并集。 */
  const runs = new Set<string>()
  turn.items.forEach((b) => {
    if (b.kind === 'assistant' && b.runId && (!only || only.has(b.runId))) runs.add(b.runId)
  })
  const u: TurnUsage = { input: 0, output: 0, cacheRead: 0, reasoning: 0, total: 0, llmMs: 0, matched: false }
  if (!metrics || !runs.size) return u
  metrics.forEach((m) => {
    if (!m.runId || !runs.has(m.runId)) return
    u.matched = true
    u.input += m.input || 0
    u.output += m.output || 0
    u.cacheRead += m.cacheRead || 0
    u.reasoning += m.reasoning || 0
    u.llmMs += m.durMs || 0
  })
  u.total = u.input + u.output
  if (!u.ttftMs) {
    const a = turn.items.find(
      (b) => b.kind === 'assistant' && b.ttftMs && (!only || (b.runId != null && only.has(b.runId))),
    )
    if (a && a.kind === 'assistant') u.ttftMs = a.ttftMs
  }
  return u
}

/** 一段运行自己的账。只认**这一段里出现过**的 run：缺 runId 的段以前回落到「整轮」，
    于是「0 calls 的一段」被写成整轮的 11.00M tokens（用户 2026-11 截图里两个 0 calls
    的 Worked 底下的那一行）。现在没有自己 run 的一段就是没账。 */
export function segmentUsage(turn: Turn, run: TurnRun, metrics: TurnMetric[] | undefined): TurnUsage {
  const only = new Set<string>()
  if (run.runId) only.add(run.runId)
  run.items.forEach((b) => {
    if (b.kind === 'assistant' && b.runId) only.add(b.runId)
  })
  return turnUsage(turn, metrics, only)
}

/** 轮尾 / 折叠行的时长：口径同全仓 fmtDuration（1h30m / 1m25s / 1s666ms / 880ms）。 */
export function durText(ms: number): string {
  if (!ms) return ''
  return fmtDuration(ms)
}

/* What a folded turn still shows. Folding is about the working phase, never
   about the record: the answer, the error that explains why there is no
   answer, the context divider and the produced files stay outside; everything
   else is what it took to get here.

   Records (signal / task_delivery) are the interesting case — their place
   depends on **when they arrived**, not on what kind they are (2026-10，
   用户口径：简洁档 = 时间线 + 一个 Working 块):
   · 运行中到达 → 属于那次运行的过程，进盒子、按事件顺序夹在工作行之间。
     九类 Small signals（memory_activity / tool_hook_warning / run_silent_end /
     goal_alignment_reminder / task_promoted / user_inputs_consumed /
     session_budget_exceeded / session_persist_error / session_closed）与
     后台任务回执 task_delivery 同一判据：它们是「正在发生的事」的一行说明。
   · AgentEnd 之后到达 → 盒子已经折叠，折进去等于「跑完之后什么都没发生」。
     它们留在时间线上（store 按所属轮落位，见 insertAtTurnEnd）。
   判据见 recordAfterRun：块上的 afterRun 位（store 在落块时按 runEnded 盖戳），
   旧快照没有这一位时退回位置 —— 排在轮内最后一段正文之后即交卷后到达。 */
export const ALWAYS_SHOWN = new Set<MsgBlock['kind']>([
  // 失败/重试进度：这一轮为什么没有答案。折进盒子 = 让失败看起来像「什么都没发生」。
  'error',
  // 上下文隔离横线（shutdown / abort / clear）：结构边界，不是这一轮的内容。
  'divider',
  // 本轮产出（AgentEnd.Artifacts）：这一轮的**结果**，轮尾固定落位（见 Narrative）。
  'artifacts',
])

/** 记录类块：一行信号 / 后台任务回执。它们的位置按到达时刻决定，见上方注释。 */
const isRecord = (b: MsgBlock): b is M<'signal'> | M<'task_delivery'> =>
  b.kind === 'signal' || b.kind === 'task_delivery'

const owningRunId = (b: MsgBlock): string | undefined => {
  switch (b.kind) {
    case 'assistant': return b.runId
    case 'compression': return b.runId
    case 'tool': return b.parentRunId ?? b.runId
    case 'agent': return b.parentRunId
    case 'async_task': return b.parentRunId
    default: return undefined
  }
}

/* 只做了压缩的那一次运行 —— 它的块属于**通知**，不属于「过程」。
 *
 * 典型形态（2026-11 用户报障）：AgentEnd 之后手动 `/compact`。命令经 inbox 注入，
 * 空闲会话为了消费它会**新开一次 run**（AgentStart → CompressStart/End → AgentEnd），
 * 这次 run 里没有正文、没有工具，唯一的内容就是压缩块 + 那行 `compress_end` 信号。
 * 按 run 切段时它于是独占一段，画出来是一个没有时长的 `Worked · 1 compress` 盒子 ——
 * 读起来像「又干了一段活」，而事实是「交卷之后做了一次上下文维护」。
 *
 * 判据只用块自己带的 run id：同一 run 里有没有**过程**（正文 / 工具 / 子 agent /
 * 运行中插话）。没有 run id 的旧数据不算通知（保持老行为：压缩属于过程）。
 * 记录（signal / task_delivery）没有 run id，按 turnRuns 的同一规则跟着**当前段**走。
 */
export function noticeBlocks(turn: Turn): Set<MsgBlock> {
  const worked = new Set<string>()
  const runs = new Set<string>()
  for (const b of turn.items) {
    const r = owningRunId(b)
    if (!r) continue
    runs.add(r)
    if (
      b.kind === 'assistant' || b.kind === 'tool' || b.kind === 'agent' ||
      b.kind === 'async_task' || (b.kind === 'user' && b.injected)
    ) worked.add(r)
  }
  const notice = new Set<MsgBlock>()
  if (runs.size === 0) return notice
  let cur: string | undefined
  for (const b of turn.items) {
    const r = owningRunId(b)
    if (r) cur = r
    if (cur && runs.has(cur) && !worked.has(cur)) notice.add(b)
  }
  return notice
}

/* 这条记录是不是 AgentEnd **之后**才到的。afterRun 位优先（bridge / store 在落块那一刻
   按「主 run 还在不在跑」盖章 —— 位置本身不是证据，checkpoint 恢复后位置只说明它插在哪）。
   旧快照没有这一位（`after_run` 当年是 omitempty，false 会被吃掉）时按**邻居**判：

   · 前后最近的、带主 run id 的块是**同一次运行** → 记录夹在这次运行中间 → 运行中到达；
   · 前面那次运行之后再没有它的块（下一步是另一次运行 / 用户 / 结束）→ 交卷后到达。

   为什么不能只看「排在轮内最后一段正文之后」（旧判据）：一次运行中间本来就会有正文
   （narration），后面还接着十几分钟的工具行 —— 那些记录明明是在跑的时候到的，却被判成
   交卷后，于是一律甩到盒子外面（用户 2026-11 截图：Task completed 永远排在 Working 下方，
   盒子里只剩思考与 toolcall）。 */
const recordAfterRun = (b: MsgBlock, i: number, items: MsgBlock[], answerAt: number): boolean => {
  if (!isRecord(b)) return false
  if (b.afterRun != null) return b.afterRun
  const prev = runIdAround(items, i, -1)
  // 前面没有任何运行块（窗口从运行中间开始 / 纯记录轮）：只能退回位置判据
  if (!prev) return answerAt >= 0 && i > answerAt
  // 后面还有同一次运行的块 → 它落在这次运行中间，不是在交卷之后
  return runIdAround(items, i, +1) !== prev
}

/* 从 i 开始向 dir 方向找最近的「带主 run id」的块。记录 / 插话 / 用户块没有 run id，跳过。 */
const runIdAround = (items: MsgBlock[], i: number, dir: 1 | -1): string | undefined => {
  for (let j = i + dir; j >= 0 && j < items.length; j += dir) {
    const rid = owningRunId(items[j])
    if (rid) return rid
  }
  return undefined
}

/* 位置兜底的分界点 = 这一轮的结论（最后一段正文）。正在流式输出的那一段还不是结论：
   此刻到达的记录仍属于这次运行，否则「边跑边来的信号」会在打字期间被甩到盒子外。 */
export const answerIndex = (turn: Turn): number => {
  const a = turn.answer
  return a && !a.streaming ? turn.items.indexOf(a) : -1
}

/** 盒子外面（时间线上）的块：结构性/结论性块 + 交卷后到达的记录 + **只做了压缩的
    那次运行**的全部块（见 noticeBlocks：AgentEnd 之后的手动压缩是通知，不是过程）。 */
export function outsideOf(turn: Turn, notice?: ReadonlySet<MsgBlock>): (b: MsgBlock, i: number) => boolean {
  const answerAt = answerIndex(turn)
  const n = notice ?? noticeBlocks(turn)
  return (b, i) => ALWAYS_SHOWN.has(b.kind) || n.has(b) || recordAfterRun(b, i, turn.items, answerAt)
}

export function foldedBody(turn: Turn, notice?: ReadonlySet<MsgBlock>): MsgBlock[] {
  const outside = outsideOf(turn, notice)
  return turn.items.filter((b, i) => b === turn.answer || outside(b, i))
}

/* Running, folded density: while the box exists the box holds the run — work
   rows *and* the records that arrived while it was live (in the order they
   happened). Outside stays only what is not the run's progress: the answer,
   the failure that explains its absence, the divider, the produced files, and
   the records that arrive after AgentEnd (the box is already folded then —
   folding those in would read as "nothing happened once it finished").

   includeAnswer 覆盖「这一轮根本没有 Working 盒子」的形态（纯对话轮：没有工具、
   没有子 agent，workBody 为空 ⇒ 不画盒子）。此时正文若仍被排除在外面，它就既不在
   盒子里（没有盒子）也不在外面 —— 整轮在 AgentEnd 之前一个字都渲染不出来。 */
export function runningBody(turn: Turn, includeAnswer = false, notice?: ReadonlySet<MsgBlock>): MsgBlock[] {
  const outside = outsideOf(turn, notice)
  return turn.items.filter((b, i) => outside(b, i) || (includeAnswer && b === turn.answer))
}

/* What the working box holds (concise). Everything that is *work*: tools, subagents,
   background tasks, the run's own rows — plus the mid-run narration and thoughts.

   Why assistant blocks belong here too (2026-10, from the spec «历史轮次的 Reasoning
   默认收缩…中间轮次的 Content 保留在 Working 内»): a run with five LLM rounds used to
   drop rounds 1–4 entirely — they were neither outside (only the last round's reply is)
   nor inside the box (this list had no `assistant`). The reader could expand the box
   and still not find what the model said in round 2. They belong inside, and the
   Thinking component already folds itself when it is not live, so they arrive
   collapsed — which is exactly the "history rounds stay shut" rule.

   The one assistant left out is the turn's answer: it is handed over to the Worked
   area at AgentEnd and must not appear twice. Compression blocks come along for the
   same reason — they are part of what happened during the run. */
export function workBody(turn: Turn, notice?: ReadonlySet<MsgBlock>): MsgBlock[] {
  const answerAt = answerIndex(turn)
  const n = notice ?? noticeBlocks(turn)
  return turn.items.filter(
    (b, i) =>
      // 只做了压缩的那次运行（交卷后的手动 /compact）不进盒子：它是时间线上的通知。
      !n.has(b) && (
      FOLDED_KINDS.has(b.kind)
      // 运行中插话：属于这次运行的过程（它不在外面 —— 外面是时间线，盒子已经收起了）。
      || (b.kind === 'user' && b.injected)
      // 运行中到达的记录：这一次运行的过程说明，按事件顺序夹在工作行之间。
      || (isRecord(b) && !recordAfterRun(b, i, turn.items, answerAt))
      || ((b.kind === 'assistant' || b.kind === 'compression') && b !== turn.answer)),
  )
}

/** 一段运行自己的结论 = 这一段里**最后一个有正文**的 assistant。
    别拿整轮的 `turn.answer` 当结论：下一段运行一开始吐字，它就跟着移走了 —— 上一段
    已经交付的正文会被判成过程行收回 Worked 盒子（用户 2026-11：Worked for 41m10s 之后
    来了 Task completed，上一段的 llm_content 就「进了盒子」、时间线上看不见了）。 */
export function answerOf(items: MsgBlock[]): MsgBlock | undefined {
  for (let i = items.length - 1; i >= 0; i--) {
    const b = items[i]
    if (b.kind === 'assistant' && textOf(b).trim().length > 0) return b
  }
  return undefined
}

/* 一轮里可能有**多次运行**（后台任务结果回推后主 run 自动续跑 / agent_resume 原地续跑），
   而产出清单是**每次运行**的 AgentEnd 各带一份 —— 同一轮于是会有多个 artifacts 块
   （store 与 Go 的 view_reducer 都按 runId 去重，所以「一个 AgentEnd 一块」是对的）。
   卡片问的是「这一轮产出了什么」：两张卡都写「本轮产出 N 个文件」，是同一份事实报两遍
   （用户 2026-11 截图：连续两张「本轮产出 1 个文件」）。

   合并口径（纯展示层，store 里的块保持每 run 一份，续跑/对账都还要它）：
   · 按 path 去重，**后出现的覆盖先出现的**（同一文件在两个 run 里都改过 → 取最终那次）；
   · 顺序按首次出现（读起来还是时间顺序）；
   · truncated 取或（任一 run 的清单被截断，卡片就要说）。
   id 用第一块的（React key 稳定，合并前后不会重挂）。 */
export function mergeArtifacts(blocks: Extract<MsgBlock, { kind: 'artifacts' }>[]): Extract<MsgBlock, { kind: 'artifacts' }> {
  const first = blocks[0]
  if (blocks.length <= 1) return first
  const byPath = new Map<string, ArtifactItem>()
  for (const b of blocks) for (const it of b.items) byPath.set(it.path, it)
  return { ...first, items: [...byPath.values()], ...(blocks.some((b) => b.truncated) ? { truncated: true } : {}) }
}

/* —— 一轮里的**多次运行**（用户 2026-11 的口径，两个形态）——
   情况一：一次运行 → 一个盒子，运行中到达的记录在盒内按时间夹在工作行之间；
   情况二：run1 正常 AgentEnd → 异步任务结果通知到达（**顶层，盒外**）→ run2 起一个新盒子。
   判据落在「盒子 = 一次运行」上，所以先把轮切成段。

   段键取块上的**主 run id**：assistant 看 runId、tool 看 parentRunId（工具块记的是父 run）、
   compression 看 runId。子 agent 卡片 / 异步任务卡看 **parentRunId** —— 卡片自己那个 runId
   是子 run，拿它切段会把父 run 切成两截。没有主 run id 的块（记录、插话）跟着当前段走。 */
export interface TurnRun {
  key: string
  runId?: string
  /** 在 turn.items 里的下标区间（[start, end)）—— 渲染按 turn.items 的顺序交错走。 */
  start: number
  end: number
  items: MsgBlock[]
}

/* 一段运行值不值得**单开一个盒子**：它交付过东西 —— 正文结论、记录（回执/信号）、产出。
   交付过的段必须切出来：它的结论与记录要落在两个盒子之间的时间线上（见 defersToTurnEnd）。
   没交付的连续运行（只跑了几轮思考/工具就被回执切开的续跑）合成一个盒子 —— 否则
   「一次用户输入」会变成一排只剩时长的 Worked：用户 2026-11 的截图里一轮 4 个盒子，
   其中三个 0 calls、没有任何正文（全是 run 边界，不是三次交付）。 */
const delivers = (b: MsgBlock): boolean => isRecord(b) || b.kind === 'artifacts' || isText(b)

export function turnRuns(turn: Turn, settleAt?: number): TurnRun[] {
  const runs: TurnRun[] = []
  let curId: string | undefined
  let start = 0
  const seal = (end: number) => {
    runs.push({
      key: `${turn.key}#${runs.length}`,
      ...(curId ? { runId: curId } : {}),
      start, end, items: turn.items.slice(start, end),
    })
    start = end
  }
  turn.items.forEach((b, i) => {
    const rid = owningRunId(b)
    // 已经在一段里，又遇到**另一个**主 run 的块 → 上一段到此为止（这就是情况二的边界）。
    // 只有上一段**交付过**才真的切：两个连着的 run 之间没有正文、没有记录、没有产出时，
    // 它们对读者是同一段活，切开会留下一个只有「Worked for 4s · 0 calls」的空盒子。
    if (rid && curId && rid !== curId && turn.items.slice(start, i).some(delivers)) seal(i)
    if (rid) curId = rid
  })
  seal(turn.items.length)
  const kept = runs.filter((r) => r.end > r.start)
  /* 块上没有主 run id 时（冷恢复 / 旧快照 / 事件缺 run_id），上面一刀也切不出来 ——
     整轮于是只有一个段，`live` 一翻就把**已经结算的那一段**重新展开成 Working（答案
     从轮尾被挪进盒子里，几百像素一进一出）。用户 2026-11 的「主 run 已 AgentEnd、后台
     还在动时页面上下抖 2~4s」就是它。
     所以再给一条**观测量**的边界：这一轮上一次结算时它有多少块（settleAt）。只在
     「按 run id 切不出段」时使用它 —— 有 run id 的情况由上面那条更权威的判据负责，
     不掺进来（否则同一个边界会被切两次，多出一个空盒子）。 */
  if (
    kept.length === 1 &&
    settleAt != null &&
    settleAt > kept[0].start &&
    settleAt < kept[0].end
  ) {
    const [only] = kept
    return [
      { ...only, key: `${turn.key}#0`, end: settleAt, items: turn.items.slice(only.start, settleAt) },
      { ...only, key: `${turn.key}#1`, start: settleAt, items: turn.items.slice(settleAt, only.end) },
    ]
  }
  return kept
}

/**
 * 交卷之后到达的记录（signal / task_delivery）该不该延后到**轮尾结算之后**。
 *
 * 一轮可以跨多次运行（后台任务回推 → 续跑），而账与产出卡只在整轮交卷时合并成一份
 * 画在轮尾。所以「结算之后」是**分段**的：只有最后一段的记录该跟着轮尾走；前几段的
 * 记录跟它自己那段的盒子走（两个盒子之间）。把整轮的记录一律延后，几条
 * `Task … completed` 就会全堆在最后一个 Worked 后面（用户 2026-11 截图）。
 *
 * 记录块自己没有 runId —— 它在 turn.items 里的位置（插入时落在刚交卷的那段之后）
 * 就是归属。段没切出来（单 run / 详细档不按段渲染 / 旧快照）时保持老行为：全部延后。
 */
export function defersToTurnEnd(runs: TurnRun[], i: number): boolean {
  if (runs.length < 2) return true
  return i >= runs[runs.length - 1].start
}

/** 一次运行的**结算**（账 / 产出卡）该落在哪个下标之后：它自己最后一块要跳过
    「结算之后才到达」的记录（signal / task_delivery 的 afterRun 位）与产出卡本身 ——
    否则「Task … completed」会排到账和卡上面，读起来像回执先于结算。
    旧快照没有 afterRun 位时按邻居兜底（见 recordAfterRun）：夹在这次运行中间的记录不算。 */
export function settleIndexOf(turn: Turn, run: TurnRun, answer: MsgBlock | undefined): number {
  const ansAt = answer ? turn.items.indexOf(answer) : -1
  let k = run.end - 1
  while (k > run.start) {
    const b = turn.items[k]
    const stamped = recordAfterRun(b, k, turn.items, ansAt)
    if (b.kind !== 'artifacts' && !stamped) break
    k--
  }
  return k
}

/* 一段运行装进盒子的行：与 workBody 同一条判据，只是作用域是一段。
   answer 传**整轮**的结论（中间那次运行的正文属于过程，跟着自己的盒子走，
   否则两次运行各吐一段正文，时间线上会凭空多出两段"结论"）。
   offset = 这一段在 turn.items 里的起点：位置兜底（旧快照没有 afterRun 位）用的还是
   全局下标，否则每一段都会把自己的记录判成"交卷后到达"。
   all = 整轮的 items（缺省 = 这一段自己）：邻居判据的单位是**整轮**，段首那条记录的前一次
   运行在上一段里 —— 只看这一段会把它当成「前面没有运行块」。 */
export function runWorkBody(
  items: MsgBlock[],
  opts: { answer?: MsgBlock; answerAt: number; offset: number; all?: MsgBlock[]; notice?: ReadonlySet<MsgBlock> },
): MsgBlock[] {
  const all = opts.all ?? items
  return items.filter(
    (b, i) =>
      // 只做了压缩的那次运行：段内块走时间线，盒子不画（见 noticeBlocks）。
      !opts.notice?.has(b) && (
      FOLDED_KINDS.has(b.kind)
      || (b.kind === 'user' && b.injected)
      || (isRecord(b) && !recordAfterRun(b, i + opts.offset, all, opts.answerAt))
      || ((b.kind === 'assistant' || b.kind === 'compression') && b !== opts.answer)),
  )
}

/* 这一轮有没有东西要装进 Working 盒子？盒子存在当且仅当 workBody 非空 ——
   不是「有没有工具」。按工具数判会把 async_task-only / system-only 的轮次整轮丢掉：
   workBody 里明明有行，却没有任何地方渲染它（盒子是按工具数决定画不画的）。 */
export function hasWork(turn: Turn, notice?: ReadonlySet<MsgBlock>): boolean {
  return workBody(turn, notice).length > 0
}

/** Blocks that belong to the working phase even though they are not "a tool". */
export const FOLDED_KINDS: ReadonlySet<MsgBlock['kind']> = new Set<MsgBlock['kind']>([
  'tool', 'agent', 'async_task', 'system', 'task_result',
])
