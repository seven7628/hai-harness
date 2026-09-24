// trace.ts —— 链路 tab 的 trace 数据模型与**实时投影**。
//
// 两条数据来源，同一套 span 模型（docs/TRACE_CHAIN_REDESIGN.md）：
//   ① 历史 trace：bridge `trace` 命令回的历史投影（读事件日志，见 desktop/bridge/trace.go）
//   ② 当前 trace：**前端实时投影** —— 本进程收到的事件流已在内存里（store 的 runNodes/turns
//      是同源事件加工出来的），直接由事件流投影成 span，无需再等命令往返。
//
// 为什么当前 trace 不用命令拉：当前 run 的事件在流式到达，逐次往返会有延迟与竞态；
// 而 store 已经按事件增量维护状态，复用同一批事件做投影最简单、也天然实时。
//
// span 模型与 Go 侧 traceSpan 字段一一对应（kind/id/run_id/parent_id/start_at/dur_ms/...），
// 保证「实时投影」与「历史投影」在 UI 上完全同形，切换 trace 不跳变。

import type { AnyEvent } from './events'

export type TraceKind = 'run' | 'llm' | 'tool' | 'compress' | 'wait'
export type TraceStatus = 'running' | 'done' | 'error' | 'interrupted'

/** 一个 span（瀑布图一行）。字段与 Go traceSpan 对齐（JSON 名同形）。 */
export interface TraceSpan {
  kind: TraceKind
  id?: string // tool: 调用 id（正文回查锚点）；llm: request_id；wait: 批次 id
  run_id?: string // 所属 run（挂树用）
  parent_id?: string // run: 父 run（根为空）
  name?: string
  label?: string
  status: TraceStatus
  depth?: number
  start_at?: number // 相对 trace 起点的 ms 偏移
  dur_ms?: number

  // llm
  model?: string
  in_tok?: number
  out_tok?: number
  cache_tok?: number
  cache_write_tok?: number
  reason_tok?: number
  cost_usd?: number

  // compress
  before?: number
  after?: number
  ctx_tokens?: number
  reason?: string

  // 其他
  wait_type?: string
  questions?: number
  task_id?: string
  is_error?: boolean
  promoted?: boolean
  retries?: number

  /** 绝对开始时刻（ms；前端排序/跟随用，不进 wire 契约语义） */
  startAbs?: number
}

/** trace 列表项（顶部选择器）。 */
export interface TraceSummary {
  id: string
  label: string
  status: TraceStatus
  start_at: number
  wall_ms: number
  spans: number
  tools: number
  turns: number
  subs: number
  errors: number
  in_tok: number
  out_tok: number
  cost_usd: number
  meta_bytes: number
}

export interface TraceDetail {
  summary: TraceSummary
  spans: TraceSpan[]
}

/** 参与 trace 投影的事件类型（白名单；与 Go traceSpanTypes 一致）。 */
export const TRACE_SPAN_TYPES = new Set([
  'agent_start', 'agent_end',
  'llm_start', 'llm_end', 'llm_error',
  'tool_run_start', 'tool_run_end',
  'compress_start', 'compress_end',
  'ask_user_question',
])

/** 事件时间戳 → 毫秒（兼容数字与 RFC3339 字符串）。 */
export function traceTs(raw: AnyEvent): number | undefined {
  const v = (raw as { timestamp?: unknown }).timestamp
  if (typeof v === 'number') return v
  if (typeof v === 'string') {
    const t = Date.parse(v)
    return Number.isFinite(t) ? t : undefined
  }
  return undefined
}

interface UsageLike {
  Input?: number
  Output?: number
  CacheRead?: number
  CacheWrite?: number
  Reasoning?: number
}

/** 展示标签：优先 name，其次 content 首行（Go 侧 traceLabel 同规则）。 */
export function traceLabel(name: unknown, content: unknown): string {
  const n = typeof name === 'string' ? name.trim() : ''
  if (n) return n
  let s = typeof content === 'string' ? content.trim() : ''
  if (!s) return 'run'
  const nl = s.search(/[\r\n]/)
  if (nl >= 0) s = s.slice(0, nl)
  const chars = Array.from(s)
  if (chars.length > 60) s = chars.slice(0, 60).join('') + '…'
  return s || 'run'
}

/** 可变累加器（投影过程中按 span 身份索引）。 */
interface Acc {
  span: TraceSpan
  startAbs: number
  endAbs: number
  hasStart: boolean
  hasEnd: boolean
}

/**
 * projectTrace —— 把**一个 run 子树**的事件流投影成 trace（纯函数，可单测）。
 *
 * 传入的 events 应已按时间顺序、且已过滤为 `TRACE_SPAN_TYPES`（调用方负责）。
 * 与 Go buildTraceDetail 的折叠规则一致：start/end 配对成一个 span、相对时间以最早 start 为 0。
 *
 * `liveRootId`：**当前仍在运行**的 root run（无则空）。语义关键 —— 「没有 end」有两种含义：
 *   - 该 run 正在跑（agent_start 已发、agent_end 还没到）→ status='running'
 *   - 该 run 已中断/崩溃残留（会话已不在运行）→ status='interrupted'
 * 若不区分，运行中的链路会被显示成「未完成」，与「运行中」的实际状态矛盾。
 */
export function projectTrace(rootRunId: string, events: AnyEvent[], liveRootId?: string): TraceDetail | null {
  if (events.length === 0) return null
  const isLiveTrace = !!liveRootId && liveRootId === rootRunId

  const runs = new Map<string, Acc>()
  const llms = new Map<string, Acc>()
  const tools = new Map<string, Acc>()
  const compress = new Map<string, Acc>()
  const order: Acc[] = []
  const push = (a: Acc) => { order.push(a) }
  const pendingRetry = new Map<string, number>()

  const str = (v: unknown): string => (typeof v === 'string' ? v : '')
  const num = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? v : 0)

  for (const e of events) {
    const t = e.event_type
    const runId = str(e.run_id)
    const ts = traceTs(e) ?? 0
    switch (t) {
      case 'agent_start': {
        if (runs.has(runId)) break // 同 run 重复 start：幂等
        const a: Acc = {
          span: {
            kind: 'run', run_id: runId, parent_id: str(e.parent_run_id) || undefined,
            label: traceLabel(e.name, e.content), name: str(e.name) || undefined,
            status: 'running', depth: num(e.depth), model: str(e.model) || undefined,
          },
          startAbs: ts, endAbs: 0, hasStart: true, hasEnd: false,
        }
        runs.set(runId, a); push(a)
        break
      }
      case 'agent_end': {
        const a = runs.get(runId); if (!a) break
        const finish = str(e.finish_reason)
        const err = str(e.error)
        const dur = num(e.duration_ms)
        a.hasEnd = true; a.endAbs = ts
        if (dur > 0) a.span.dur_ms = dur
        a.span.status = finish === 'error' || err ? 'error'
          : finish === 'abort' || finish === 'max_iterations' ? 'interrupted' : 'done'
        if (str(e.task_id)) a.span.task_id = str(e.task_id)
        break
      }
      case 'llm_start': {
        const key = str(e.request_id) || `${runId}\u0000${ts}`
        if (llms.has(key)) break
        const a: Acc = {
          span: {
            kind: 'llm', id: str(e.request_id) || undefined, run_id: runId,
            model: str(e.model) || undefined, label: 'llm', status: 'running',
          },
          startAbs: ts, endAbs: 0, hasStart: true, hasEnd: false,
        }
        llms.set(key, a); push(a)
        break
      }
      case 'llm_end': {
        const key = str(e.request_id) || `${runId}\u0000${ts}`
        let a = llms.get(key)
        if (!a) {
          // 无 start 的 end（旧格式/截断）：补一个 done 态 span
          a = {
            span: {
              kind: 'llm', id: str(e.request_id) || undefined, run_id: runId,
              model: str(e.model) || undefined, label: 'llm', status: 'done',
            },
            startAbs: ts, endAbs: 0, hasStart: true, hasEnd: false,
          }
          llms.set(key, a); push(a)
        }
        a.hasEnd = true; a.endAbs = ts; a.span.status = 'done'
        if (str(e.model)) { a.span.model = str(e.model); a.span.name = str(e.model) }
        const u = e.usage as UsageLike | undefined
        if (u) {
          a.span.in_tok = num(u.Input) || undefined
          a.span.out_tok = num(u.Output) || undefined
          a.span.cache_tok = num(u.CacheRead) || undefined
          a.span.cache_write_tok = num(u.CacheWrite) || undefined
          a.span.reason_tok = num(u.Reasoning) || undefined
        }
        const cost = num(e.cost_usd)
        if (cost) a.span.cost_usd = cost
        const n = pendingRetry.get(runId)
        if (n) { a.span.retries = n; pendingRetry.delete(runId) }
        break
      }
      case 'llm_error': {
        pendingRetry.set(runId, (pendingRetry.get(runId) ?? 0) + 1)
        break
      }
      case 'tool_run_start': {
        const key = str(e.id) || `${str(e.name)}\u0000${ts}`
        if (tools.has(key)) break
        const a: Acc = {
          span: {
            kind: 'tool', id: str(e.id) || undefined, run_id: runId,
            name: str(e.name) || undefined, label: str(e.name) || 'tool',
            status: 'running', task_id: str(e.task_id) || undefined,
          },
          startAbs: ts, endAbs: 0, hasStart: true, hasEnd: false,
        }
        tools.set(key, a); push(a)
        break
      }
      case 'tool_run_end': {
        const key = str(e.id) || `${str(e.name)}\u0000${ts}`
        let a = tools.get(key)
        if (!a) {
          a = {
            span: {
              kind: 'tool', id: str(e.id) || undefined, run_id: runId,
              name: str(e.name) || undefined, label: str(e.name) || 'tool', status: 'done',
            },
            startAbs: ts, endAbs: 0, hasStart: true, hasEnd: false,
          }
          tools.set(key, a); push(a)
        }
        a.hasEnd = true; a.endAbs = ts
        const isErr = e.is_error === true
        a.span.is_error = isErr || undefined
        a.span.status = isErr ? 'error' : 'done'
        if (str(e.task_id)) a.span.task_id = str(e.task_id)
        if (e.promoted === true) a.span.promoted = true
        break
      }
      case 'compress_start': {
        const a: Acc = {
          span: {
            kind: 'compress', run_id: runId, label: 'compress', status: 'running',
            before: num(e.before) || undefined, reason: str(e.reason) || undefined,
          },
          startAbs: ts, endAbs: 0, hasStart: true, hasEnd: false,
        }
        compress.set(runId, a); push(a)
        break
      }
      case 'compress_end': {
        let a = compress.get(runId)
        if (!a) {
          a = {
            span: { kind: 'compress', run_id: runId, label: 'compress', status: 'done' },
            startAbs: ts, endAbs: 0, hasStart: true, hasEnd: false,
          }
          push(a)
        }
        a.hasEnd = true; a.endAbs = ts
        a.span.status = str(e.error) ? 'error' : 'done'
        a.span.before = num(e.before) || undefined
        a.span.after = num(e.after) || undefined
        a.span.ctx_tokens = num(e.ctx_tokens) || undefined
        if (str(e.model)) a.span.model = str(e.model)
        compress.delete(runId)
        break
      }
      case 'ask_user_question': {
        const qs = Array.isArray(e.questions) ? e.questions.length : 0
        push({
          span: {
            kind: 'wait', id: str(e.id) || undefined, run_id: runId,
            label: 'ask_user', status: 'done', wait_type: 'ask_user',
            questions: qs || undefined,
          },
          startAbs: ts, endAbs: 0, hasStart: true, hasEnd: false,
        })
        break
      }
    }
  }
  if (order.length === 0) return null

  // 相对时间基准 = 最早 start
  let zero = 0
  for (const a of order) {
    if (a.hasStart && a.startAbs > 0 && (zero === 0 || a.startAbs < zero)) zero = a.startAbs
  }

  const spans: TraceSpan[] = []
  let endMax = 0
  for (const a of order) {
    const s = a.span
    s.startAbs = a.startAbs || undefined
    s.start_at = a.hasStart && a.startAbs > 0 && zero > 0 ? a.startAbs - zero : 0
    if (s.dur_ms == null) {
      if (a.hasEnd && a.endAbs > 0 && a.startAbs > 0) s.dur_ms = a.endAbs - a.startAbs
      else if (a.hasStart && !a.hasEnd) {
        // 只有「成对事件」的 span 才可能未闭合；wait（ask_user_question）是**单事件**，
        // 没有 end 属正常（用户已答也照样只有一条），不能标成未完成
        // —— 与 Go trace.go 的白名单保持一致（交叉比对抓出的差异）。
        if (s.kind === 'run' || s.kind === 'llm' || s.kind === 'tool' || s.kind === 'compress') {
          // 区分「正在跑」与「中断残留」：只有当前活跃 root run 才算 running（见函数注释）。
          s.status = isLiveTrace ? 'running' : 'interrupted'
        }
      }
    }
    if (s.status === 'running' && a.hasEnd) s.status = 'done'
    endMax = Math.max(endMax, s.start_at + (s.dur_ms ?? 0))
    spans.push(s)
  }

  // 摘要（与 Go 侧同口径：Errors 只数可归因的失败 span；run 自身失败且无子失败记 1）
  const rootAcc = runs.get(rootRunId)
  let childErrors = 0
  const sum: TraceSummary = {
    id: rootRunId,
    label: rootAcc?.span.label || rootRunId,
    status: (rootAcc?.span.status ?? 'done') as TraceStatus,
    start_at: rootAcc?.startAbs ?? 0,
    wall_ms: endMax,
    spans: spans.length,
    tools: 0,
    turns: 0,
    subs: 0,
    errors: 0,
    in_tok: 0,
    out_tok: 0,
    cost_usd: 0,
    meta_bytes: 0,
  }
  for (const s of spans) {
    if (s.kind === 'run') {
      if (s.run_id !== rootRunId) sum.subs++
    } else if (s.kind === 'llm') {
      sum.turns++
      sum.in_tok += s.in_tok ?? 0
      sum.out_tok += s.out_tok ?? 0
      sum.cost_usd += s.cost_usd ?? 0
      if (s.status === 'error') childErrors++
    } else if (s.kind === 'tool') {
      sum.tools++
      if (s.status === 'error') childErrors++
    } else if (s.status === 'error') childErrors++
  }
  sum.errors = childErrors || (sum.status === 'error' ? 1 : 0)
  sum.meta_bytes = JSON.stringify(spans).length
  return { summary: sum, spans }
}

/** 由事件流解析出全部 root run（保序去重），并给出每个 run 的父节点。 */
export function traceRoots(events: AnyEvent[]): { roots: string[]; parentOf: Map<string, string> } {
  const parentOf = new Map<string, string>()
  const roots: string[] = []
  for (const e of events) {
    if (e.event_type !== 'agent_start') continue
    const rid = typeof e.run_id === 'string' ? e.run_id : ''
    if (!rid || parentOf.has(rid)) continue
    const pid = typeof e.parent_run_id === 'string' ? e.parent_run_id : ''
    parentOf.set(rid, pid)
    if (!pid) roots.push(rid)
  }
  return { roots, parentOf }
}

/**
 * projectAllTraces —— 把事件流按 root run 切成多条 trace（最近在上）。
 * 与 Go buildTraces 同构：root 收敛后代 run 的全部事件。
 */
export function projectAllTraces(events: AnyEvent[], liveRootId?: string): TraceDetail[] {
  const { roots, parentOf } = traceRoots(events)
  if (roots.length === 0) return []
  const kids = new Map<string, string[]>()
  for (const [rid, pid] of parentOf) {
    const arr = kids.get(pid) ?? []
    arr.push(rid)
    kids.set(pid, arr)
  }
  const out: TraceDetail[] = []
  for (const root of roots) {
    const subtree = new Set<string>()
    const stack = [root]
    while (stack.length) {
      const x = stack.pop()!
      if (subtree.has(x)) continue
      subtree.add(x)
      stack.push(...(kids.get(x) ?? []))
    }
    const sub = events.filter((e) => typeof e.run_id === 'string' && subtree.has(e.run_id))
    const d = projectTrace(root, sub, liveRootId)
    if (d) out.push(d)
  }
  out.sort((a, b) => b.summary.start_at - a.summary.start_at)
  return out
}

/** span 的嵌套深度（沿 parent_id 计算；供缩进渲染）。 */export function traceSpanDepth(spans: TraceSpan[], s: TraceSpan): number {
  if (s.kind === 'tool') {
    // 工具挂在所属 run 下（树里比 run 深一层）
    return 1
  }
  if (s.kind === 'run') {
    const byRun = new Map<string, TraceSpan>()
    for (const x of spans) if (x.kind === 'run' && x.run_id) byRun.set(x.run_id, x)
    let d = 0
    let cur = s.parent_id
    const seen = new Set<string>()
    while (cur && !seen.has(cur)) {
      seen.add(cur)
      d++
      cur = byRun.get(cur)?.parent_id
    }
    return d
  }
  return 1
}

/**
 * liveRootRunId —— 取「当前仍在运行」的顶层 run id（无则 undefined）。
 *
 * 数据源是 store 的 runNodes（不是 traceEvents）：runNodes 既由实时事件维护，
 * 也被 checkpoint 恢复归一化过（finalizeReplayView 把残留 running 收敛为 done），
 * 因此它能同时正确回答「刷新后还在跑吗」与「崩溃残留该标 interrupted 吗」。
 *
 * 用途：projectTrace 据此把「无 end 的 root run」判为 running（正在跑）而不是 interrupted。
 */
export function liveRootRunId(runNodes: { id: string; kind: string; status: string; parentId?: string }[]): string | undefined {
  const n = runNodes.find((x) => x.kind === 'run' && x.status === 'running' && !x.parentId)
  return n?.id
}
