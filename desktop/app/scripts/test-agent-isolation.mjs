// 主/子 Agent 提示隔离 + Session 统一 FIFO 审批 的 store 行为回归（真实 store + esbuild，
// Node 直接跑，无需浏览器/vitest）。模式同 scripts/test-store-queue.mjs。
//
// 覆盖：SubAgent 审批自动**聚焦它的子会话**（主 Agent 不聚焦）、统一 FIFO 顺序与去重、
// 压缩双通道（主会话块 + 卡片）、SubAgent 压缩仅进卡片、watchdog 键隔离语义、SubAgent
// retry 累计与终态清理、llm_end 不清主错误、agent_end abort 边界、快照重放归一化、
// 旧事件缺 run_id 回退主 Agent、跨会话隔离。
//
// 2026-10：右栏「任务」tab 已退役 → 原来「SubAgent 审批自动展开右栏并切任务 tab」的
// 口径改为「自动聚焦到该子会话」（focusedAgent），审批卡就停靠在它的输入区上方。
//
// 用法：npm run test:agents
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)

// 1) shim localStorage（store 模块加载时读取项目树/偏好）
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}

// 2) esbuild 打包 store（含真实 mock transport）到临时 CJS
const dir = mkdtempSync(join(tmpdir(), 'agent-isolation-'))
const outfile = join(dir, 'store-bundle.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  // store 间接依赖 src/transport/mock.ts（import README/brand 资源，带 ?raw/?url 后缀）：
  // node 侧没有 vite 的资源插件链 → 补 loader，否则打包直接失败。
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})
const {
  useAppStore,
  dispatchEvent,
  materializeSnapshot,
  isSubAgentRun,
  approvalOwnerIsSub,
  runningTaskCount,
  asyncTaskBlocks,
} = require(outfile)

function setup() {
  useAppStore.setState({
    workspace: '/mock/w',
    activeSessionId: 's1',
    bridgeStatus: 'running',
    sessions: [{ id: 's1', title: 't' }],
    projects: [{ path: '/mock/w', sessions: [{ id: 's1', title: 't' }] }],
    views: {},
    streaming: false,
    pendingQueue: [],
    blocks: [],
    runNodes: [],
    approvals: [],
    diffs: [],
    todos: [],
    usage: { input: 0, output: 0, cacheRead: 0, reasoning: 0 },
    turns: [],
    costUsd: 0,
    toolStartTs: {},
    rightExpanded: false,
    rightTab: 'activity',
  })
}
const EV = (t, p = {}) => dispatchEvent({ event_type: t, session_id: 's1', ...p })
const view = () => useAppStore.getState().views.s1
const sub = (runId, name = 'sub-review') => EV('agent_start', { run_id: runId, parent_run_id: 'root', name })

// ===== 场景 1：SubAgent 审批自动聚焦它的子会话；主 Agent 审批不抢视线 =====
setup()
EV('tool_approval_requested', { id: 'a-main', name: 'write_file', arguments: '{"path":"/x"}' })
let st = useAppStore.getState()
assert.equal(st.approvals.length, 1, '主审批应入统一队列')
assert.equal(st.rightExpanded, false, '主 Agent 审批不得自动展开右栏')
assert.equal(st.focusedAgent, undefined, '主 Agent 审批不得切换视图（仍是主会话）')
sub('sub-1')
EV('tool_approval_requested', { id: 'a-sub', name: 'write_file', arguments: '{"path":"/y"}', run_id: 'sub-1' })
st = useAppStore.getState()
assert.equal(st.approvals.length, 2, 'SubAgent 审批应追加到同一队列（不拆分）')
assert.equal(st.approvals[0].id, 'a-main', '队首必须是先到的主审批')
assert.deepStrictEqual(st.focusedAgent, { nodeId: 'sub-1', runId: 'sub-1' },
  'SubAgent 审批应聚焦到它的子会话（nodeId 回退到 runId：这张卡还没有 taskId）')
assert.equal(st.rightTab, 'activity', 'SubAgent 审批不再切换右栏标签（任务 tab 已退役）')
console.log('PASS 场景1：SubAgent 审批聚焦子会话；主审批不抢视线；统一队列保序')

// ===== 场景 2：统一 FIFO 顺序 = 事件顺序；重复 id 去重；approvals[0] 才是展示队首 =====
setup()
sub('s-a')
EV('tool_approval_requested', { id: 'qa', name: 'bash', arguments: '{}', run_id: 's-a' })
sub('s-b')
EV('tool_approval_requested', { id: 'qb', name: 'bash', arguments: '{}', run_id: 's-b' })
assert.deepStrictEqual(view().approvals.map((a) => a.id), ['qa', 'qb'], 'FIFO 顺序 = 事件顺序')
EV('tool_approval_requested', { id: 'qa', name: 'bash', arguments: '{}', run_id: 's-a' })
assert.equal(view().approvals.length, 2, '重复 id 必须去重')
EV('tool_approval_requested', { id: 'qc', name: 'bash', arguments: '{}', run_id: 's-a' })
assert.equal(view().approvals.length, 3, '新 id 仍追加')
assert.equal(view().approvals[0].id, 'qa', '队首不变')
// 归属判定：卡片存在且 RunNode 兜底均可识别
assert.ok(isSubAgentRun(view(), 's-a'), '卡片归属应识别为 SubAgent')
assert.ok(approvalOwnerIsSub(view(), 's-b'), 'RunNode 兜底应识别为 SubAgent')
assert.ok(!approvalOwnerIsSub(view(), undefined), '无 run_id 按主 Agent')
console.log('PASS 场景2：FIFO 保序 + 去重 + 归属判定')

// ===== 场景 3：压缩双通道 —— 主压缩活动字段 + 结束追加主会话块 + 顶层镜像 =====
setup()
EV('agent_start', { run_id: 'root' })
EV('compress_start', { before: 12, reason: 'automatic' })
let v = view()
assert.equal(v.compressing, true, '主压缩应置 compressing')
assert.equal(v.compressBefore, 12, '主压缩应保存 compressBefore')
// 顶层镜像（活跃会话）同步
st = useAppStore.getState()
assert.equal(st.compressing, true, '顶层镜像 compressing 必须同步')
assert.equal(st.compressBefore, 12, '顶层镜像 compressBefore 必须同步')
assert.equal(v.blocks.filter((b) => b.kind === 'compression').length, 0, '压缩结束前不追加块')
EV('compress_end', { before: 12, after: 5, reason: 'automatic', ctx_tokens: 8000 })
v = view()
assert.equal(v.compressing, false, '压缩结束应清活动态')
assert.equal(v.compressBefore, undefined, '压缩结束应清 compressBefore')
const mainBlock = v.blocks.find((b) => b.kind === 'compression')
assert.ok(mainBlock, '主压缩结束应追加 compression 块')
assert.equal(mainBlock.before, 12, '主压缩块 before 正确')
assert.equal(mainBlock.after, 5, '主压缩块 after 正确')
assert.equal(useAppStore.getState().compressing, false, '顶层镜像结束态同步')
console.log('PASS 场景3：主压缩双通道（活动字段 + 会话块 + 顶层镜像）')

// ===== 场景 4：SubAgent 压缩只进对应卡片，不污染主压缩活动态 =====
setup()
EV('agent_start', { run_id: 'root' })
sub('sub-a')
EV('compress_start', { run_id: 'sub-a', before: 3 })
v = view()
assert.equal(v.compressing, false, 'SubAgent 压缩不得置主 compressing')
assert.equal(v.compressBefore, undefined, 'SubAgent 压缩不得写主 compressBefore')
const cardA = v.blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-a')
assert.ok(cardA?.kind === 'agent' && cardA.items.some((it) => it.kind === 'compression' && it.active), '卡片应有 active 压缩项')
assert.equal(cardA?.compressing, true, '卡片应标记压缩中')
EV('compress_end', { run_id: 'sub-a', before: 3, after: 1, ctx_tokens: 4000 })
v = view()
const cardA2 = v.blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-a')
const cItem = cardA2?.kind === 'agent' ? cardA2.items.find((it) => it.kind === 'compression') : undefined
assert.ok(cItem?.kind === 'compression' && cItem.active === false && cItem.after === 1, '压缩项应收敛')
assert.equal(v.blocks.filter((b) => b.kind === 'compression').length, 0, '主会话不得出现 SubAgent 压缩块')
assert.equal(v.compressBefore, undefined, '主压缩活动字段不受影响')
console.log('PASS 场景4：SubAgent 压缩仅进卡片、不影响主压缩态')

// ===== 场景 5：watchdog 键隔离 —— 并发的压缩（主 + SubAgent）活动态互不干扰 =====
setup()
EV('agent_start', { run_id: 'root' })
sub('sub-a')
sub('sub-b')
EV('compress_start', { before: 20 })            // 主
EV('compress_start', { run_id: 'sub-a', before: 2 })
EV('compress_start', { run_id: 'sub-b', before: 4 })
v = view()
assert.equal(v.compressing, true, '主压缩活动态保持')
assert.equal(v.blocks.filter((b) => b.kind === 'agent').length, 2, '两张 SubAgent 卡片')
assert.ok(v.blocks.every((b) => b.kind !== 'agent' || b.compressing), '两张卡片都应压缩中')
// 只结束 sub-a：主与 sub-b 的活动态不得被波及
EV('compress_end', { run_id: 'sub-a', before: 2, after: 1 })
v = view()
assert.equal(v.compressing, true, 'SubAgent 压缩结束不得清主压缩')
const subB = v.blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-b')
assert.equal(subB?.compressing, true, '其他 SubAgent 压缩不受影响')
const subA = v.blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-a')
assert.equal(subA?.compressing, false, '本卡片压缩结束收敛')
console.log('PASS 场景5：并发压缩活动态按 run 键隔离')

// ===== 场景 6：SubAgent retry 累计、首次 llm_start 无 retryMessage、成功轮清除 =====
setup()
sub('sub-r')
EV('llm_start', { run_id: 'sub-r', request_id: 'rq0', model: 'gpt-test' })
let card = view().blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-r')
assert.equal(card?.retryMessage, undefined, '首次 llm_start 不应产生 retryMessage')
assert.notEqual(card?.retrying, true, '首次 llm_start 不应标记重试中')
EV('llm_error', { run_id: 'sub-r', attempt: 1, will_retry: true, message: 'boom1' })
card = view().blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-r')
assert.equal(card?.retryCount, 1, 'retryCount 应记为 1')
assert.equal(card?.retrying, true, '重试中标记')
assert.ok(card?.retryMessage?.includes('重试'), 'retryMessage 应含重试文案')
assert.equal(card?.items.filter((it) => it.kind === 'text' && it.error).length, 1, '错误文本应原位更新为一条')
EV('llm_error', { run_id: 'sub-r', attempt: 2, will_retry: true, message: 'boom2' })
card = view().blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-r')
assert.equal(card?.retryCount, 2, 'retryCount 应累计到 2')
assert.equal(card?.items.filter((it) => it.kind === 'text' && it.error).length, 1, '多次重试仍只有一条错误文本')
// 重试成功：llm_end 清 retry 态（历史失败文本保留在 items）
EV('llm_end', { run_id: 'sub-r', request_id: 'rq0', model: 'gpt-test', usage: {} })
card = view().blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-r')
assert.equal(card?.retrying, false, '成功轮应清 retrying')
assert.equal(card?.retryMessage, undefined, '成功轮应清 retryMessage')
assert.ok(!view().blocks.some((b) => b.kind === 'error'), 'SubAgent 错误不得进入主叙述 error 块')
console.log('PASS 场景6：SubAgent retry 累计/清理 + 卡片内单条错误')

// ===== 场景 7：SubAgent llm_end 不得清主 error 块；主 llm_end 清 =====
setup()
EV('agent_start', { run_id: 'root' })
EV('llm_error', { run_id: 'root', attempt: 1, will_retry: true, message: 'main down' })
assert.ok(view().blocks.some((b) => b.kind === 'error'), '主 llm_error 产生主 error 块')
sub('sub-x')
EV('llm_end', { run_id: 'sub-x', request_id: 'rq-sub', model: 'gpt-test', usage: {} })
assert.ok(view().blocks.some((b) => b.kind === 'error'), 'SubAgent llm_end 不得清主错误块')
EV('llm_end', { run_id: 'root', request_id: 'rq-main', model: 'gpt-test', usage: {} })
assert.ok(!view().blocks.some((b) => b.kind === 'error'), '主 llm_end 应清主错误块')
console.log('PASS 场景7：llm_end 错误块清理只归属主 run')

// ===== 场景 8：agent_end abort 边界 —— SubAgent abort 不清等待态；主 abort 清 =====
setup()
EV('agent_start', { run_id: 'root' })
sub('sub-y')
EV('tool_approval_requested', { id: 'ap-y', name: 'write_file', arguments: '{}', run_id: 'sub-y' })
EV('ask_user_question', { id: 'q-y', run_id: 'sub-y', questions: [{ id: 'q1', question: '继续?' }] })
EV('sandbox_blocked', { id: 'sb-y', hint: 'rm -rf' })
assert.equal(view().approvals.length, 1, '审批入队')
assert.equal(view().questions.length, 1, '提问挂起')
assert.ok(view().release, '放行挂起')
// SubAgent 中断：卡片收敛，等待态保留
EV('agent_end', { run_id: 'sub-y', finish_reason: 'abort' })
v = view()
assert.equal(v.approvals.length, 1, 'SubAgent abort 不得清审批队列')
// 2026-10（按 run 归属后）：子 run abort → 清**它自己的**提问（它的 ask_user 工具已中止，
// 那条提问不可能再有人答），但绝不碰主 run 的那批。旧口径是「一条都不许清」——
// 那是单槽时代的自保（清掉就会误清主会话的），不是语义本身。
assert.equal(v.questions.length, 0, 'SubAgent abort 清它自己的提问（工具已中止，那条不可能再被回答）')
assert.ok(v.release, 'SubAgent abort 不得清放行')
const subY = v.blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-y')
assert.equal(subY?.status, 'interrupted', 'SubAgent 卡片终态 interrupted')
assert.equal(subY?.retrying, false, 'SubAgent 终态清 retrying')
assert.equal(v.aborted, false, 'SubAgent abort 不得置主中断标记')
// 主 run abort：清等待态 + 置 aborted
EV('agent_end', { run_id: 'root', finish_reason: 'abort' })
v = view()
assert.equal(v.approvals.length, 0, '主 abort 应清审批队列')
assert.equal(v.questions.length, 0, '主 abort 应清提问（含子 run 已收敛的那些）')
assert.equal(v.release, undefined, '主 abort 应清放行')
assert.equal(v.aborted, true, '主 abort 应置中断标记')
// 实现 2026-08 变更：abort 不再补「（已中断）」文本，改追加 divider 作上下文隔离
assert.ok(v.blocks.some((b) => b.kind === 'divider' && b.reason === 'abort'), '主 abort 应追加 divider 中断隔离')
console.log('PASS 场景8：abort 清理边界（各清各的：Sub 清自己的、主清自己的）')

// ===== 场景 8b：主/子两批 ask_user 并存 —— 互不覆盖，各清各的 =====
// 2026-10 真机验证发现的缺陷：question 曾是**单个会话槽**，后到的提问直接覆盖先到的 →
// 「主会话正在等答的那批凭空消失」。现在按 run 归属存多条。
setup()
EV('agent_start', { run_id: 'root' })
EV('ask_user_question', { id: 'q-main', run_id: 'root', questions: [{ id: 'q-m1', question: '主问？' }] })
sub('sub-z')
EV('ask_user_question', { id: 'q-z', run_id: 'sub-z', questions: [{ id: 'q-z1', question: '子问？' }] })
v = view()
assert.deepStrictEqual(v.questions.map((x) => x.id), ['q-main', 'q-z'], '两批并存（子提问不覆盖主提问）')
assert.equal(v.questions[0].runId, 'root', '主 run 的提问仍在，归属不变')
// 子 run 的 ask_user 工具结束（超时/回答）→ 只清它那批
EV('tool_run_end', { run_id: 'sub-z', id: 'call-z', name: 'ask_user', result: 'ok' })
v = view()
assert.deepStrictEqual(v.questions.map((x) => x.id), ['q-main'], '子那批结束只清子那批，主会话仍在等')
// 主 run 的 ask_user 结束 → 清掉最后一批
EV('tool_run_end', { run_id: 'root', id: 'call-m', name: 'ask_user', result: 'ok' })
assert.equal(view().questions.length, 0, '主那批结束 → 清空')
// 回答按 batch id 精确投递（不再取"最后到达那条"）
EV('ask_user_question', { id: 'q-x', run_id: 'root', questions: [{ id: 'q-x1', question: '再问？' }] })
sub('sub-w')
EV('ask_user_question', { id: 'q-w', run_id: 'sub-w', questions: [{ id: 'q-w1', question: '又问？' }] })
useAppStore.getState().answerQuestion('q-x', [{ question_id: 'q-x1', answer: 'A' }])
v = view()
assert.deepStrictEqual(v.questions.map((x) => x.id), ['q-w'], '回答 q-x → 只清 q-x，q-w 仍在等（未被"最后到达那条"顶替）')
console.log('PASS 场景8b：主/子提问并存；结束与回答都按归属/batch id 精确处理')

// ===== 场景 9：快照重放 —— 残留压缩活动态/审批清空、运行卡片 abandon =====
setup()
EV('agent_start', { run_id: 'root' })
materializeSnapshot('s1', [
  { event_type: 'agent_start', run_id: 'root', parent_run_id: '' },
  { event_type: 'agent_start', run_id: 'sub-z', parent_run_id: 'root', name: 'ghost-sub' },
  { event_type: 'compress_start', before: 9 }, // 无配套 end
  { event_type: 'tool_approval_requested', id: 'ghost-ap', name: 'bash', arguments: '{}', run_id: 'sub-z' },
])
v = view()
assert.equal(v.compressing, false, '快照恢复后不得残留压缩活动态')
assert.equal(v.compressBefore, undefined, '快照恢复后不得残留 compressBefore')
assert.equal(v.approvals.length, 0, '快照恢复后审批清空')
const ghost = v.blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-z')
assert.equal(ghost?.status, 'abandoned', '运行中子 agent 快照恢复后应收敛 abandoned')
assert.equal(v.streaming, false, '快照恢复后流式态复位')
console.log('PASS 场景9：快照重放归一化（压缩/审批/卡片/流式）')

// ===== 场景 10：旧事件缺 run_id 按主 Agent 回退 =====
setup()
EV('compress_start', { before: 5 }) // 无 run_id → 主
assert.equal(view().compressing, true, '无 run_id 压缩按主 Agent')
EV('compress_end', { before: 5, after: 2 })
EV('tool_approval_requested', { id: 'legacy-ap', name: 'bash', arguments: '{}' }) // 无 run_id → 主
assert.equal(view().approvals[0].runId, undefined, '旧审批事件 runId 缺省')
assert.equal(useAppStore.getState().rightExpanded, false, '无 run_id 审批不得自动展开右栏')
EV('llm_error', { attempt: 1, will_retry: true, message: 'legacy fail' })
assert.ok(view().blocks.some((b) => b.kind === 'error'), '无 run_id 错误按主叙述 error 块')
console.log('PASS 场景10：旧事件缺 run_id 回退主 Agent')

// ===== 场景 11：跨会话隔离 —— s2 的压缩/审批/错误只进 s2 view，不污染 s1 镜像 =====
setup()
EV('agent_start', { run_id: 'root' })
dispatchEvent({ event_type: 'agent_start', session_id: 's2', run_id: 'root2' })
dispatchEvent({ event_type: 'agent_start', session_id: 's2', run_id: 'sub-2', parent_run_id: 'root2', name: 'other-sub' })
dispatchEvent({ event_type: 'compress_start', session_id: 's2', run_id: 'sub-2', before: 7 })
dispatchEvent({ event_type: 'tool_approval_requested', session_id: 's2', id: 'ap-s2', name: 'bash', arguments: '{}', run_id: 'sub-2' })
dispatchEvent({ event_type: 'llm_error', session_id: 's2', run_id: 'sub-2', attempt: 1, will_retry: true, message: 's2 boom' })
v = view()
const v2 = useAppStore.getState().views.s2
assert.equal(v.compressing, false, 's2 压缩不得影响 s1 主压缩态')
assert.equal(v.approvals.length, 0, 's2 审批不得进入 s1 队列')
assert.ok(!v.blocks.some((b) => b.kind === 'error'), 's2 错误不得进入 s1 叙述')
assert.equal(v2.compressing, false, 's2 SubAgent 压缩只进卡片')
assert.ok(v2.blocks.some((b) => b.kind === 'agent' && b.runId === 'sub-2' && b.compressing), 's2 卡片压缩中')
assert.equal(v2.approvals.length, 1, 's2 审批进 s2 队列')
assert.equal(useAppStore.getState().approvals.length, 0, 's1 顶层镜像审批不受 s2 影响')
console.log('PASS 场景11：跨会话压缩/审批/错误隔离')

// ===== 场景 12：approve 幂等守卫（重复决策只发一次命令）与乐观移除 =====
setup()
sub('sub-p')
EV('tool_approval_requested', { id: 'ap-p', name: 'bash', arguments: '{}', run_id: 'sub-p' })
assert.equal(view().approvals.length, 1, '审批入队')
useAppStore.getState().approve('ap-p', true)
assert.equal(view().approvals.length, 0, '乐观移除')
// 已被移除的 id 再次决策：守卫拦截（不 send、不崩溃）
useAppStore.getState().approve('ap-p', true)
console.log('PASS 场景12：approve 幂等守卫 + 乐观移除')

// ===== 场景 13：主压缩 watchdog 超时收敛（经 事件触发 + 计时器先行清理路径验证） =====
// compressTimers 为模块私有，无法注入时长；这里验证 session_closed 清理路径不崩溃、
// 且 agent_end(SubAgent abort) 会收敛卡片内 active 压缩项。
setup()
EV('agent_start', { run_id: 'root' })
sub('sub-c')
EV('compress_start', { run_id: 'sub-c', before: 3 })
assert.ok(view().blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-c')?.compressing, '压缩中')
EV('agent_end', { run_id: 'sub-c', finish_reason: 'abort' }) // compress_end 不达
const cardC = view().blocks.find((b) => b.kind === 'agent' && b.runId === 'sub-c')
const cActive = cardC?.kind === 'agent' ? cardC.items.find((it) => it.kind === 'compression' && it.active) : undefined
assert.equal(cActive, undefined, 'SubAgent abort 收敛 active 压缩项')
assert.equal(cardC?.compressing, false, 'SubAgent abort 清卡片压缩中')
EV('session_closed')
console.log('PASS 场景13：SubAgent abort 压缩收敛 + session_closed 清理不崩溃')

console.log('AGENT_ISOLATION_TEST_PASS')
process.exit(0)
