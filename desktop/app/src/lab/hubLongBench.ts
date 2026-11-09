// P4-01 长会话基准的浏览器侧夹具（lab 专用，不进生产包）。
//
// 只在 `lab/hub-long.html` 里被加载：
//   - 用真实 GroupView + 真实 store/handleEvent（不是另写一个假视图）；
//   - 220 条消息 + 3 条并行流式草稿，固定的 PRNG，无网络/无 bridge；
//   - 暴露 window.__hubBench 给 Playwright：挂载、delta 突发、键入、滚动、
//     DOM/内存快照、reduced-motion 探针、弱网不变量检查。
//
// 生产构建不会引用本文件；bench 脚本用 Vite 单页构建到临时目录再跑，React 是
// production 版（dev 版会把每次渲染放大约 2~3 倍，数字不可比）。
import { createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import '../index.css'
import '../hub/hub.css'
import GroupView from '../hub/GroupView'
import { useHubStore } from '../hub/useHubStore'
import { buildHubLongFixture, HUB_LONG_WORKERS } from './hubLongFixture'

interface BenchWindow extends Window {
  __hubBench?: HubLongBenchApi
  gc?: () => void
}

interface Stats {
  n: number
  p50: number
  p95: number
  max: number
  mean: number
}

interface DomSnapshot {
  threadHtmlHash: string
  totalElements: number
  threadElements: number
  messages: number
  streaming: number
  roundGroups: number
  routingRows: number
  typingText: string
  threadScrollHeight: number
  listScrollHeight: number
  listClientHeight: number
  scrollTop: number
}

interface HubLongBenchApi {
  fixtureInfo(): { messages: number; rounds: number; drafts: number; textChars: number; group: string }
  mount(): Promise<{ mountMs: number; dom: DomSnapshot; scrollHeight: number }>
  deltaBurst(opts?: { count?: number; gapMs?: number }): Promise<{
    count: number
    committed: number
    wallMs: number
    commit: Stats
    layout: Stats
    dom: DomSnapshot
  }>
  resetInput(): void
  inputStats(): { samples: Stats; frameSamples: Stats; raw: number[]; frameRaw: number[] }
  startBackgroundStream(opts?: { intervalMs?: number }): { intervalMs: number }
  stopBackgroundStream(): number
  scrollStream(opts?: { durationMs?: number }): Promise<{
    frames: number
    raf: Stats
    over50: number
    longTasks: { n: number; max: number; total: number }
    bottomGapMaxPx: number
    scrollTopUpEnd: number
    scrollTopTailEnd: number
    driftPx: number
    scrollHeight: number
  }>
  snapshot(opts?: { gc?: boolean }): {
    dom: DomSnapshot
    memory: { usedJSHeapSize: number | null; totalJSHeapSize: number | null; jsHeapSizeLimit: number | null }
  }
  motionProbe(): {
    reduceMatches: boolean
    caret: string | null
    typingDot: string | null
    highlight: string | null
    convDot: string | null
  }
  uiProbe(): Promise<{ pass: boolean; contextChips: number; highlighted: number; cleared: boolean; detail?: string }>
  weakNetwork(): Promise<{
    pass: boolean
    checks: { name: string; pass: boolean; detail?: unknown }[]
    finalDrafts: number
    streamingBubbles: number
    messages: number
  }>
}

const fixture = buildHubLongFixture()
const benched = window as BenchWindow

let root: Root | null = null
let inputSamples: number[] = []
let inputFrameSamples: number[] = []
let keyDownAt = 0
let probeInstalled = false
let streamTimer: number | null = null
let streamTick = 0
let streamIndexes: number[] = []
let backgroundDeltas = 0

function r2(x: number): number {
  return Math.round(x * 100) / 100
}

function stats(values: number[]): Stats {
  if (values.length === 0) return { n: 0, p50: 0, p95: 0, max: 0, mean: 0 }
  const sorted = [...values].sort((a, b) => a - b)
  const pick = (q: number): number => sorted[Math.min(sorted.length - 1, Math.floor((sorted.length - 1) * q))]
  let sum = 0
  for (const v of sorted) sum += v
  return { n: sorted.length, p50: r2(pick(0.5)), p95: r2(pick(0.95)), max: r2(sorted[sorted.length - 1]), mean: r2(sum / sorted.length) }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, ms))
}

function raf2(): Promise<void> {
  return new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve())))
}

function fnv1a(text: string): string {
  let h = 0x811c9dc5
  for (let i = 0; i < text.length; i++) {
    h ^= text.charCodeAt(i)
    h = Math.imul(h, 0x01000193)
  }
  return (h >>> 0).toString(16)
}

function domSnapshot(): DomSnapshot {
  const thread = document.querySelector('.hub-thread')
  const list = document.querySelector('.hub-list')
  return {
    threadHtmlHash: thread ? fnv1a(thread.innerHTML) : '0',
    totalElements: document.getElementsByTagName('*').length,
    threadElements: thread ? thread.querySelectorAll('*').length : 0,
    messages: document.querySelectorAll('.hub-msg').length,
    streaming: document.querySelectorAll('.hub-msg.streaming').length,
    roundGroups: document.querySelectorAll('.hub-round-group').length,
    routingRows: document.querySelectorAll('.hub-action.routing').length,
    typingText: document.querySelector('.hub-typing')?.textContent ?? '',
    threadScrollHeight: Math.round(thread?.scrollHeight ?? 0),
    listScrollHeight: Math.round(list?.scrollHeight ?? 0),
    listClientHeight: Math.round(list?.clientHeight ?? 0),
    scrollTop: Math.round(list?.scrollTop ?? 0),
  }
}

function seedStore(): void {
  useHubStore.setState({
    open: true,
    agents: fixture.agents,
    groups: [fixture.group],
    activeGroupId: fixture.group.id,
    activeId: null,
    profile: null,
    sid: null,
    groupMessages: fixture.messages.map((m) => ({ ...m })),
    groupRounds: fixture.rounds.map((r) => ({ ...r })),
    groupActivity: fixture.activity.map((a) => ({ ...a })),
    groupDrafts: fixture.initialDrafts.map((d) => ({ ...d })),
    groupSending: false,
    groupAssigning: false,
    groupAssignNote: null,
    groupConvStopping: false,
    groupConversations: {},
    groupInfo: {},
    groupReadSeq: {},
    finalizedMessageIds: [],
    running: {},
    tasks: [],
    loading: false,
    error: null,
  })
}

function installStyle(): void {
  const style = document.createElement('style')
  style.textContent =
    '#root{height:100%}' +
    '.lab-hub-shell{height:100%;display:flex;min-height:0;min-width:0}' +
    '.lab-hub-shell>.hub-dm{flex:1;height:100%;min-width:0}'
  document.head.appendChild(style)
}

function installInputProbe(): void {
  if (probeInstalled) return
  probeInstalled = true
  window.addEventListener('keydown', (e) => {
    const t = e.target
    if (t instanceof HTMLTextAreaElement) keyDownAt = performance.now()
  }, true)
  // 冒泡阶段（React 在 #root 上的监听之后）：
  //   microtask = keydown → 本次输入事件触发的 React 提交完成（处理成本）；
  //   下一帧   = 用户看到的反馈延迟（含 vsync 对齐，120Hz 下 0~8.3ms）。
  window.addEventListener('input', (e) => {
    const t = e.target
    if (!(t instanceof HTMLTextAreaElement)) return
    const t0 = keyDownAt || performance.now()
    keyDownAt = 0
    queueMicrotask(() => inputSamples.push(performance.now() - t0))
    requestAnimationFrame(() => inputFrameSamples.push(performance.now() - t0))
  }, false)
}

function waitThreadMutation(timeoutMs = 400): Promise<boolean> {
  return new Promise((resolve) => {
    const thread = document.querySelector('.hub-thread')
    if (!thread) {
      resolve(false)
      return
    }
    let done = false
    const finish = (ok: boolean): void => {
      if (done) return
      done = true
      obs.disconnect()
      clearTimeout(timer)
      resolve(ok)
    }
    const obs = new MutationObserver(() => finish(true))
    obs.observe(thread, { subtree: true, childList: true, characterData: true })
    const timer = window.setTimeout(() => finish(false), timeoutMs)
  })
}

function pushDelta(messageId: string, agentId: string, index: number, delta: string, roundId = 'rnd_0'): void {
  useHubStore.getState().handleEvent(JSON.stringify({
    event_type: 'hub_message_delta',
    group_id: fixture.group.id,
    agent_id: agentId,
    round_id: roundId,
    message_id: messageId,
    index,
    delta,
  }))
}

function currentDraftIndexes(): number[] {
  const drafts = useHubStore.getState().groupDrafts
  const out: number[] = []
  for (let i = 0; i < HUB_LONG_WORKERS; i++) out.push(drafts[i]?.index ?? 0)
  return out
}

function deltaChunk(n: number): string {
  return `第${n}段：弱网下 delta 只丢草稿，final 到达后整条覆盖。`
}

async function mount(): Promise<{ mountMs: number; dom: DomSnapshot; scrollHeight: number }> {
  if (root) throw new Error('bench already mounted')
  seedStore()
  const host = document.getElementById('root')
  if (!host) throw new Error('#root missing')
  host.textContent = ''
  await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()))
  const t0 = performance.now()
  root = createRoot(host)
  root.render(createElement('div', { className: 'lab-hub-shell' }, createElement(GroupView, { group: fixture.group })))
  await raf2()
  const mountMs = performance.now() - t0
  installInputProbe()
  const dom = domSnapshot()
  return { mountMs: r2(mountMs), dom, scrollHeight: dom.listScrollHeight }
}

async function deltaBurst(opts: { count?: number; gapMs?: number } = {}): Promise<{
  count: number
  committed: number
  wallMs: number
  commit: Stats
  layout: Stats
  dom: DomSnapshot
}> {
  const count = opts.count ?? 120
  const gapMs = opts.gapMs ?? 8
  const idx = currentDraftIndexes()
  const commitSamples: number[] = []
  const layoutSamples: number[] = []
  let committed = 0
  const thread = document.querySelector('.hub-thread')
  const t0 = performance.now()
  for (let n = 0; n < count; n++) {
    const w = n % HUB_LONG_WORKERS
    const draft = useHubStore.getState().groupDrafts[w]
    if (!draft) break
    idx[w] += 1
    const start = performance.now()
    const mutation = waitThreadMutation()
    pushDelta(draft.messageId, draft.agentId, idx[w], deltaChunk(n), draft.roundId)
    const mutated = await mutation
    const committedAt = performance.now()
    if (mutated) committed++
    commitSamples.push(committedAt - start)
    // 提交后强制一次样式/布局，把这条 delta 摊到的 reflow 单独量出来。
    if (thread) {
      const layoutStart = performance.now()
      void thread.scrollHeight
      layoutSamples.push(performance.now() - layoutStart)
    }
    if (gapMs > 0) await sleep(gapMs)
  }
  return {
    count,
    committed,
    wallMs: r2(performance.now() - t0),
    commit: stats(commitSamples),
    layout: stats(layoutSamples),
    dom: domSnapshot(),
  }
}

function resetInput(): void {
  inputSamples = []
  inputFrameSamples = []
  keyDownAt = 0
}

function inputStats(): { samples: Stats; frameSamples: Stats; raw: number[]; frameRaw: number[] } {
  return {
    samples: stats(inputSamples),
    frameSamples: stats(inputFrameSamples),
    raw: inputSamples.map(r2),
    frameRaw: inputFrameSamples.map(r2),
  }
}

function startBackgroundStream(opts: { intervalMs?: number } = {}): { intervalMs: number } {
  stopBackgroundStream()
  streamIndexes = currentDraftIndexes()
  streamTick = 0
  backgroundDeltas = 0
  const intervalMs = opts.intervalMs ?? 30
  streamTimer = window.setInterval(() => {
    const drafts = useHubStore.getState().groupDrafts
    if (drafts.length === 0) return
    const w = streamTick++ % drafts.length
    const draft = drafts[w]
    streamIndexes[w] = Math.max(streamIndexes[w] ?? 0, draft.index) + 1
    pushDelta(draft.messageId, draft.agentId, streamIndexes[w], '后台并发到达的流式片段。', draft.roundId)
    backgroundDeltas++
  }, intervalMs)
  return { intervalMs }
}

function stopBackgroundStream(): number {
  if (streamTimer !== null) {
    window.clearInterval(streamTimer)
    streamTimer = null
  }
  return backgroundDeltas
}

async function scrollStream(opts: { durationMs?: number } = {}): Promise<{
  frames: number
  raf: Stats
  over50: number
  longTasks: { n: number; max: number; total: number }
  bottomGapMaxPx: number
  scrollTopUpEnd: number
  scrollTopTailEnd: number
  driftPx: number
  scrollHeight: number
}> {
  const budget = opts.durationMs ?? 2400
  const bottomMs = Math.round(budget * 0.33)
  const upEndMs = budget - 400
  const list = document.querySelector('.hub-list')
  if (!(list instanceof HTMLElement)) throw new Error('.hub-list missing')
  list.scrollTop = list.scrollHeight
  await sleep(80)

  const idx = currentDraftIndexes()
  let tick = 0
  const timer = window.setInterval(() => {
    const drafts = useHubStore.getState().groupDrafts
    if (drafts.length === 0) return
    const w = tick++ % drafts.length
    const draft = drafts[w]
    idx[w] += 1
    pushDelta(draft.messageId, draft.agentId, idx[w], '滚动期间到达的流式片段。', draft.roundId)
  }, 24)

  const longTasks: number[] = []
  let po: PerformanceObserver | null = null
  try {
    po = new PerformanceObserver((entries) => {
      for (const e of entries.getEntries()) longTasks.push(e.duration)
    })
    po.observe({ entryTypes: ['longtask'] })
  } catch {
    // 某些环境没有 longtask（如旧版 WebKit）：保留空数组即可。
  }

  const rafSamples: number[] = []
  let bottomGapMax = 0
  let scrollTopUpEnd = 0
  let scrollTopTailEnd = 0
  const t0 = performance.now()
  await new Promise<void>((resolve) => {
    let prev = 0
    let upStartTop = 0
    let upStartAt = 0
    const frame = (now: number): void => {
      if (prev > 0) rafSamples.push(now - prev)
      prev = now
      const elapsed = now - t0
      if (elapsed < bottomMs) {
        const gap = Math.abs(list.scrollHeight - list.clientHeight - list.scrollTop)
        bottomGapMax = Math.max(bottomGapMax, gap)
      } else {
        if (upStartAt === 0) {
          upStartAt = now
          upStartTop = list.scrollTop
        }
        if (elapsed < upEndMs) {
          const p = Math.min(1, (now - upStartAt) / Math.max(1, upEndMs - bottomMs))
          list.scrollTop = Math.max(0, upStartTop * (1 - p))
        } else {
          if (scrollTopUpEnd === 0) scrollTopUpEnd = list.scrollTop
          scrollTopTailEnd = list.scrollTop
        }
      }
      if (elapsed < budget) requestAnimationFrame(frame)
      else resolve()
    }
    requestAnimationFrame(frame)
  })
  window.clearInterval(timer)
  po?.disconnect()

  let longMax = 0
  let longTotal = 0
  for (const d of longTasks) {
    longMax = Math.max(longMax, d)
    longTotal += d
  }
  return {
    frames: rafSamples.length,
    raf: stats(rafSamples),
    over50: rafSamples.filter((d) => d > 50).length,
    longTasks: { n: longTasks.length, max: r2(longMax), total: r2(longTotal) },
    bottomGapMaxPx: r2(bottomGapMax),
    scrollTopUpEnd: Math.round(scrollTopUpEnd),
    scrollTopTailEnd: Math.round(scrollTopTailEnd),
    driftPx: Math.abs(Math.round(scrollTopTailEnd - scrollTopUpEnd)),
    scrollHeight: Math.round(list.scrollHeight),
  }
}

function snapshot(opts: { gc?: boolean } = {}): {
  dom: DomSnapshot
  memory: { usedJSHeapSize: number | null; totalJSHeapSize: number | null; jsHeapSizeLimit: number | null }
} {
  if (opts.gc) benched.gc?.()
  const mem = (performance as Performance & {
    memory?: { usedJSHeapSize: number; totalJSHeapSize: number; jsHeapSizeLimit: number }
  }).memory
  return {
    dom: domSnapshot(),
    memory: {
      usedJSHeapSize: mem ? Math.round(mem.usedJSHeapSize / 1024 / 1024) : null,
      totalJSHeapSize: mem ? Math.round(mem.totalJSHeapSize / 1024 / 1024) : null,
      jsHeapSizeLimit: mem ? Math.round(mem.jsHeapSizeLimit / 1024 / 1024) : null,
    },
  }
}

function motionProbe(): {
  reduceMatches: boolean
  caret: string | null
  typingDot: string | null
  highlight: string | null
  convDot: string | null
} {
  let hl = document.getElementById('bench-motion-hl')
  if (!hl) {
    hl = document.createElement('div')
    hl.id = 'bench-motion-hl'
    hl.className = 'hub-msg-highlight'
    hl.textContent = 'probe'
    hl.style.cssText = 'position:fixed;left:-9999px;top:0'
    document.body.appendChild(hl)
  }
  let conv = document.getElementById('bench-motion-conv')
  if (!conv) {
    conv = document.createElement('div')
    conv.id = 'bench-motion-conv'
    conv.className = 'hub-conv-bar running'
    conv.style.cssText = 'position:fixed;left:-9999px;top:0'
    const dot = document.createElement('span')
    dot.className = 'hub-conv-dot'
    conv.appendChild(dot)
    document.body.appendChild(conv)
  }
  const anim = (el: Element | null): string | null => (el ? getComputedStyle(el).animationName : null)
  return {
    reduceMatches: typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches,
    caret: anim(document.querySelector('.hub-stream-caret')),
    typingDot: anim(document.querySelector('.hub-typing-dots i')),
    highlight: anim(hl),
    convDot: anim(conv.querySelector('.hub-conv-dot')),
  }
}

async function uiProbe(): Promise<{ pass: boolean; contextChips: number; highlighted: number; cleared: boolean; detail?: string }> {
  const chips = Array.from(document.querySelectorAll('.hub-context-chip'))
  const button = chips.find((el) => el.tagName === 'BUTTON') as HTMLButtonElement | undefined
  if (!button) {
    return { pass: false, contextChips: chips.length, highlighted: 0, cleared: false, detail: 'no context chip button' }
  }
  button.click()
  await raf2()
  // 只数真实消息气泡：#bench-motion-hl 是 reduced-motion 探针，不能算进来。
  const highlighted = document.querySelectorAll('.hub-msg.hub-msg-highlight').length
  await sleep(2000)
  const cleared = document.querySelectorAll('.hub-msg.hub-msg-highlight').length === 0
  return { pass: highlighted === 1 && cleared, contextChips: chips.length, highlighted, cleared }
}

async function sendDelta(messageId: string, agentId: string, index: number, delta: string): Promise<void> {
  pushDelta(messageId, agentId, index, delta)
  await raf2()
}

async function sendFinal(id: string, seq: number, text: string, agentId: string): Promise<void> {
  useHubStore.getState().handleEvent(JSON.stringify({
    event_type: 'hub_message',
    group_id: fixture.group.id,
    message: {
      id,
      seq,
      group_id: fixture.group.id,
      kind: 'text',
      sender: { kind: 'agent', agent_id: agentId },
      text,
      created_at: new Date().toISOString(),
    },
  }))
  await raf2()
}

async function weakNetwork(): Promise<{
  pass: boolean
  checks: { name: string; pass: boolean; detail?: unknown }[]
  finalDrafts: number
  streamingBubbles: number
  messages: number
}> {
  const checks: { name: string; pass: boolean; detail?: unknown }[] = []
  const check = (name: string, pass: boolean, detail?: unknown): void => {
    checks.push({ name, pass, detail })
  }
  const state = (): ReturnType<typeof useHubStore.getState> => useHubStore.getState()
  const typingText = (): string => document.querySelector('.hub-typing')?.textContent ?? ''
  const streamText = (): string =>
    Array.from(document.querySelectorAll('.hub-stream-text')).map((e) => e.textContent ?? '').join('\n')

  check('initial: 3 drafts + 3 bubbles', state().groupDrafts.length === 3 && document.querySelectorAll('.hub-msg.streaming').length === 3)
  // TypingIndicator 对 >2 人只显示「前两位 +N」：断言可见文案与草稿集合一致。
  check('initial: typing names match drafts',
    typingText().includes('Alice') && typingText().includes('Bob') && typingText().includes('+1') && !typingText().includes('Carol'))

  // 先把 Alice 的草稿推进到 index ≥ 2，再测乱序/重复/空洞。
  let alice = state().groupDrafts[0]
  await sendDelta(alice.messageId, alice.agentId, alice.index + 1, '预热一。')
  alice = state().groupDrafts[0]
  await sendDelta(alice.messageId, alice.agentId, alice.index + 1, '预热二。')

  const beforeDup = state().groupDrafts[0]
  await sendDelta(beforeDup.messageId, beforeDup.agentId, beforeDup.index, '重复索引不得写入。')
  const afterDup = state().groupDrafts[0]
  check('duplicate index dropped', afterDup.text === beforeDup.text && afterDup.index === beforeDup.index)

  await sendDelta(afterDup.messageId, afterDup.agentId, Math.max(0, afterDup.index - 2), '迟到的旧索引不得写入。')
  const afterOld = state().groupDrafts[0]
  check('out-of-order delta dropped', afterOld.text === afterDup.text && afterOld.index === afterDup.index)

  await sendDelta(afterOld.messageId, afterOld.agentId, afterOld.index + 2, '跳过一块的空洞。')
  const afterGap = state().groupDrafts[0]
  check('gap tolerated on draft (best effort)',
    afterGap.text.endsWith('跳过一块的空洞。') && afterGap.index === afterOld.index + 2,
    { index: afterGap.index })
  check('no duplicate draft bubble after gap', document.querySelectorAll('.hub-msg.streaming').length === 3)

  // final 先到、delta 后到：finalized 守卫必须挡住凭空重建草稿。
  const finalId = 'm_final_before'
  await sendFinal(finalId, 900, 'FINAL 优先：最终消息先到，迟到 delta 不得造草稿。', 'ag_bob')
  check('final-before-delta: final rendered once', document.querySelectorAll('[data-seq="900"]').length === 1)
  await sendDelta(finalId, 'ag_bob', 0, 'GHOST-迟到 delta')
  check('final-before-delta: no ghost draft',
    !state().groupDrafts.some((d) => d.messageId === finalId) && document.querySelectorAll('.hub-msg.streaming').length === 3)

  await sendFinal(finalId, 900, 'FINAL duplicate', 'ag_bob')
  check('duplicate final does not duplicate message',
    document.querySelectorAll('[data-seq="900"]').length === 1 &&
      state().groupMessages.filter((m) => m.id === finalId).length === 1)

  // agent_end 兜底：Carol 的草稿没等到 final 也要清掉，typing 同步收窄。
  useHubStore.getState().handleEvent(JSON.stringify({
    event_type: 'agent_end',
    session_id: 'hub.ag_carol.g.grp_long',
  }))
  await raf2()
  check('agent_end clears its draft',
    state().groupDrafts.length === 2 && !state().groupDrafts.some((d) => d.agentId === 'ag_carol'))
  check('typing indicator tracks agent_end',
    typingText().includes('Alice') && typingText().includes('Bob') && !typingText().includes('Carol') &&
      document.querySelectorAll('.hub-msg.streaming').length === 2)

  // final 覆盖草稿：草稿正文整条消失，不留第二颗气泡。
  const aliceDraft = state().groupDrafts.find((d) => d.agentId === 'ag_alice')
  if (!aliceDraft) throw new Error('alice draft missing')
  await sendFinal(aliceDraft.messageId, 901, 'Alice 的最终完整回复：草稿到此为止。', 'ag_alice')
  check('final replaces draft (single bubble)',
    state().groupDrafts.length === 1 &&
      document.querySelectorAll('.hub-msg.streaming').length === 1 &&
      document.querySelectorAll('[data-seq="901"]').length === 1)
  check('final text wins over draft',
    (document.querySelector('[data-seq="901"]')?.textContent ?? '').includes('最终完整回复') &&
      !streamText().includes('预热一') && !streamText().includes('空洞'))

  await sendDelta(aliceDraft.messageId, 'ag_alice', 99, 'FINAL 之后的迟到 delta。')
  check('post-final delta dropped', state().groupDrafts.length === 1 &&
    document.querySelectorAll('.hub-msg.streaming').length === 1)
  check('typing indicator matches remaining draft',
    typingText().includes('Bob') && !typingText().includes('Alice') && !typingText().includes('Carol'))
  check('one draft bubble per draft', document.querySelectorAll('.hub-msg.streaming').length === state().groupDrafts.length)
  check('no duplicate rendered messages',
    document.querySelectorAll('.hub-msg:not(.streaming)').length === state().groupMessages.length)

  return {
    pass: checks.every((c) => c.pass),
    checks,
    finalDrafts: state().groupDrafts.length,
    streamingBubbles: document.querySelectorAll('.hub-msg.streaming').length,
    messages: state().groupMessages.length,
  }
}

const api: HubLongBenchApi = {
  fixtureInfo: () => ({
    messages: fixture.messages.length,
    rounds: fixture.rounds.length,
    drafts: fixture.initialDrafts.length,
    textChars: fixture.messages.reduce((n, m) => n + (m.text?.length ?? 0), 0),
    group: fixture.group.id,
  }),
  mount,
  deltaBurst,
  resetInput,
  inputStats,
  startBackgroundStream,
  stopBackgroundStream,
  scrollStream,
  snapshot,
  motionProbe,
  uiProbe,
  weakNetwork,
}

installStyle()
benched.__hubBench = api
