// 「子 agent 跑完后主 Agent 总结那轮没有 LLM 耗时 / TTFT」根因复现（真实 store，Node 直跑）。
//
// 背景（2026-09-29 报障）：派了子 agent 之后，主 Agent 每次做总结/轮询的那一轮，
// 底部指标行经常缺 LLM 总耗时，TTFT 也偶发缺失；刷新一次又好了。
//
// 假设：live 路径的 turnStartTs 是 **view 级标量**（useAppStore.ts:683 `turnStartTs?: number`），
// 而不是按 runId/run 分槽。写入方只有主 run 的 llm_start（:4269），而**任何 run 的 llm_end**
// 都会读它算 durMs（:4375）并把它清成 undefined（子分支 :4420 / 主分支 :4478）。
// spawn_agent 是 Background 工具（subagent/agent_tools.go:58 `Background: true`），
// 子 agent 与主 Agent 的轮次并发交错 —— 子 agent 的 llm_end 落进主 Agent 某轮的
// llm_start..llm_end 窗口，就把主 Agent 该轮的起点吃掉：durMs/ttftMs 双双变 undefined。
//
// 对照：Go 侧 view_reducer 用的是 **按 key 分槽** 的 reqState/turnStart
// （view_reducer.go:432/439，key = request_id / runId），所以刷新后从 checkpoint 重建的
// 指标是对的 —— 「刷新前缺、刷新后有」正是这个 bug 的指纹。
//
// 覆盖：
//   T1 无子 agent 的主 run 总结轮 → durMs / ttftMs / agentDurMs 齐全（基线）
//   T2 子 agent 的 llm_end 落在主 run 总结轮窗口内 → durMs 丢失
//   T3 子 agent 的 llm_end 落在主 run 轮询轮窗口内 → 同样丢失
//   T4 agentDurMs 是否受影响（独立字段，应仍可用）
//
// 用法：node scripts/test-turnstart-clobber.mjs
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync, copyFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const LS = new Map()
global.localStorage = {
  getItem: (k) => (LS.has(k) ? LS.get(k) : null),
  setItem: (k, v) => LS.set(k, String(v)),
  removeItem: (k) => LS.delete(k),
}

const F = { sessions: new Map(), events: new Map(), ws: '/ws-clobber', cb: null }
function emitResp(id, data) {
  if (F.cb) F.cb(JSON.stringify({ event_type: 'command_response', id, ok: true, data, workspace: F.ws }))
}
function makeDesktop() {
  return {
    startBridge: async () => ({ ok: true }),
    pickWorkspace: async () => null, homeDir: async () => '/tmp', createWorkspaceFolder: async () => ({ ok: false }),
    settingsGet: async () => null, settingsSet: async () => null,
    externalSkillsStatus: async () => null, importExternalSkills: async () => ({ ok: true }),
    pickApp: async () => null, providerSave: async () => ({ ok: true }), providerRemove: async () => ({ ok: true }),
    providerKeyStatus: async () => false, mcpSave: async () => ({ ok: true }),
    fsList: async () => ({ ok: true, entries: [] }), fsReadRef: async () => ({ ok: true, content: '' }),
    pickFsFile: async () => null, pickFsDir: async () => null,
    command: async (cmd) => {
      if (cmd.type === 'list') emitResp(cmd.id, { sessions: [...F.sessions.values()] })
      else if (cmd.type === 'new_session') {
        const want = String(cmd.payload?.id ?? '')
        if (F.sessions.has(want)) {
          const snapEvents = (F.events.get(want) || []).map(({ session_id, ...rest }) => rest)
          emitResp(cmd.id, { session_id: want, sessions: [...F.sessions.values()], todos: [], live: false, restored: 'snapshot', snapshot: { events: snapEvents } })
        } else emitResp(cmd.id, { session_id: want, sessions: [...F.sessions.values()], todos: [], live: false })
      } else emitResp(cmd.id, {})
      return { ok: true }
    },
    onEvent: (cb) => { F.cb = cb; return () => { F.cb = null } },
    onExit: () => () => {},
  }
}

// mock.ts 带 ?raw / ?url 资源导入（README.md、brand/*.svg|png），补 loader 才能打包。
const dir = mkdtempSync(join(tmpdir(), 'turnstart-clobber-'))
const outfile = join(dir, 'store.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile, logLevel: 'silent', loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})
const mod = require(outfile)

global.window = {
  localStorage: global.localStorage,
  matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {}, removeEventListener() {},
  desktop: makeDesktop(),
}
global.document = { documentElement: { classList: { add() {}, remove() {} } } }

let pass = 0
function ok(label) { pass++; console.log(`  ✅ ${label}`) }
function bad(label) { console.log(`  ❌ ${label}`); process.exitCode = 1 }

const T0 = Date.parse('2026-09-29T10:00:00.000+08:00')

// 造事件流。at(ms) 相对 T0。
function seed(kind) {
  const evs = []
  const push = (at, o) => evs.push({ session_id: 'sA', timestamp: new Date(T0 + at).toISOString(), ...o })
  const usage = { Input: 100, Output: 50, CacheRead: 0, CacheWrite: 0, Reasoning: 0, TotalTokens: 150 }

  // —— 主 run r1 开局：派子 agent ——
  push(0, { event_type: 'agent_start', run_id: 'r1' })
  push(0, { event_type: 'llm_start', run_id: 'r1', request_id: 'req1', model: 'm' })
  push(50, { event_type: 'content_chunk', run_id: 'r1', request_id: 'req1', content: '我来派个子 agent' })
  push(100, { event_type: 'llm_end', run_id: 'r1', request_id: 'req1', model: 'm', finish_reason: 'tool_call', usage })
  push(150, { event_type: 'task_started', task_id: 't1', name: 'researcher', tool_name: 'spawn_agent' })

  if (kind === 'baseline') {
    // 无子 agent：主 run 直接总结
    push(1000, { event_type: 'llm_start', run_id: 'r1', request_id: 'req9', model: 'm' })
    push(1200, { event_type: 'content_chunk', run_id: 'r1', request_id: 'req9', content: '总结如下' })
    push(2200, { event_type: 'llm_end', run_id: 'r1', request_id: 'req9', model: 'm', finish_reason: 'stop', content: '总结如下', usage })
    push(2250, { event_type: 'agent_end', run_id: 'r1', model: 'm', finish_reason: 'stop', duration_ms: 2250, usage })
    return evs
  }

  // —— 子 run r2（parent r1，Background 工具 → 与主 run 轮次并发）——
  push(200, { event_type: 'agent_start', run_id: 'r2', parent_run_id: 'r1', task_id: 't1', name: 'researcher' })
  push(250, { event_type: 'llm_start', run_id: 'r2', request_id: 'sreq1', model: 'm' })
  push(300, { event_type: 'content_chunk', run_id: 'r2', request_id: 'sreq1', content: '子 agent 干活中' })

  if (kind === 'clobber-summary') {
    // 主 run 的总结轮先开（turnStartTs = 1000），子 run 的 llm_end **插进窗口内**（1500）
    push(1000, { event_type: 'llm_start', run_id: 'r1', request_id: 'req9', model: 'm' })
    push(1500, { event_type: 'llm_end', run_id: 'r2', request_id: 'sreq1', model: 'm', finish_reason: 'stop', content: '子 agent 干活中', usage })
    push(1600, { event_type: 'content_chunk', run_id: 'r1', request_id: 'req9', content: '总结如下' })
    push(2200, { event_type: 'llm_end', run_id: 'r1', request_id: 'req9', model: 'm', finish_reason: 'stop', content: '总结如下', usage })
    push(2250, { event_type: 'agent_end', run_id: 'r1', model: 'm', finish_reason: 'stop', duration_ms: 2250, usage })
  } else if (kind === 'clobber-poll') {
    // 典型轮询形态：主 run 轮询轮开着，子 run 的 llm_end 落进来
    push(1000, { event_type: 'llm_start', run_id: 'r1', request_id: 'req9', model: 'm' })
    push(1200, { event_type: 'content_chunk', run_id: 'r1', request_id: 'req9', content: '还在跑，我等等' })
    push(1500, { event_type: 'llm_end', run_id: 'r2', request_id: 'sreq1', model: 'm', finish_reason: 'stop', content: '子 agent 干活中', usage })
    push(1600, { event_type: 'llm_end', run_id: 'r1', request_id: 'req9', model: 'm', finish_reason: 'tool_call', usage })
    // 子 run 收尾 + 主 run 总结轮（此时子 run 已静默，总结轮不受影响）
    push(2000, { event_type: 'agent_end', run_id: 'r2', parent_run_id: 'r1', task_id: 't1', model: 'm', finish_reason: 'stop', duration_ms: 1800, usage })
    push(2100, { event_type: 'task_end', task_id: 't1', status: 'completed', usage })
    push(2200, { event_type: 'llm_start', run_id: 'r1', request_id: 'req10', model: 'm' })
    push(2400, { event_type: 'content_chunk', run_id: 'r1', request_id: 'req10', content: '总结如下' })
    push(3400, { event_type: 'llm_end', run_id: 'r1', request_id: 'req10', model: 'm', finish_reason: 'stop', content: '总结如下', usage })
    push(3450, { event_type: 'agent_end', run_id: 'r1', model: 'm', finish_reason: 'stop', duration_ms: 3450, usage })
  } else if (kind === 'stream-stuck') {
    // run 级失败：只有 session_run_error，没有 llm_end、没有 agent_end
    push(1000, { event_type: 'llm_start', run_id: 'r1', request_id: 'req9', model: 'm' })
    push(1200, { event_type: 'content_chunk', run_id: 'r1', request_id: 'req9', content: '总结如下' })
    push(1500, { event_type: 'session_run_error', run_id: 'r1', error: 'boom', error_kind: 'upstream_unavailable' })
  } else if (kind === 'truncated') {
    // 达轮数上限截断：有正文但没有「最终正文轮」
    push(1000, { event_type: 'llm_start', run_id: 'r1', request_id: 'req9', model: 'm' })
    push(1200, { event_type: 'content_chunk', run_id: 'r1', request_id: 'req9', content: '再查一下' })
    push(1300, { event_type: 'llm_end', run_id: 'r1', request_id: 'req9', model: 'm', finish_reason: 'tool_call', usage })
    push(1400, { event_type: 'agent_end', run_id: 'r1', model: 'm', finish_reason: 'max_iterations', duration_ms: 1400, usage })
  }
  return evs
}

async function runTurn(kind) {
  F.sessions.clear(); F.events.clear()
  F.sessions.set('sA', { id: 'sA', title: 'SA', updated_at: Date.now() })
  F.events.set('sA', seed(kind))
  LS.set('go-code.projects', JSON.stringify({ paths: [F.ws], active: F.ws, lastSession: { path: F.ws, id: 'sA' } }))
  const copy = join(dir, `store-${Math.random().toString(36).slice(2)}.cjs`)
  copyFileSync(outfile, copy)
  const s = require(copy).useAppStore
  await s.getState().start()
  await sleep(40)
  const v = s.getState().views['sA']
  assert(v, 'sA 视图应已恢复')
  return v
}

const lastTextBlock = (v, text) =>
  [...v.blocks].reverse().find((b) => b.kind === 'assistant' && b.text === text)

// ============ T1 基线：没有子 agent 干扰 ============
console.log('T1 基线（无子 agent 交错）')
{
  const v = await runTurn('baseline')
  const b = lastTextBlock(v, '总结如下')
  assert(b, '应存在总结块')
  console.log(`     durMs=${b.durMs} ttftMs=${b.ttftMs} agentDurMs=${b.agentDurMs}`)
  if (b.durMs === 1200 && b.ttftMs === 200 && b.agentDurMs === 2250) ok('总结轮 durMs=1200 / ttftMs=200 / agentDurMs=2250 齐全')
  else bad(`基线就不对：durMs=${b.durMs} ttftMs=${b.ttftMs} agentDurMs=${b.agentDurMs}`)
}

// ============ T2 子 agent 的 llm_end 插进主 run 总结轮窗口 ============
console.log('T2 子 agent 的 llm_end 落进主 run 总结轮窗口（报障场景）')
{
  const v = await runTurn('clobber-summary')
  const b = lastTextBlock(v, '总结如下')
  assert(b, '应存在总结块')
  console.log(`     durMs=${b.durMs} ttftMs=${b.ttftMs} agentDurMs=${b.agentDurMs}`)
  // 修复前：durMs=undefined（子 run 的 llm_end 把主 run 的 turnStartTs 清成 undefined）
  if (b.durMs === 1200) ok('durMs=1200：主 run 该轮起点未被子 run 抢走')
  else bad(`durMs=${b.durMs}，want 1200`)
  if (b.ttftMs === 600) ok('TTFT=600（llmStartTs 本就独立，未受影响）')
  else bad(`TTFT 不对：${b.ttftMs}`)
  if (b.agentDurMs === 2250) ok('agentDurMs=2250')
  else bad(`agentDurMs=${b.agentDurMs}，want 2250`)
}

// ============ T3 轮询轮同样中招 ============
console.log('T3 子 agent 的 llm_end 落进主 run 轮询轮窗口')
{
  const v = await runTurn('clobber-poll')
  const poll = lastTextBlock(v, '还在跑，我等等')
  const summary = lastTextBlock(v, '总结如下')
  assert(poll && summary, '应存在轮询块与总结块')
  console.log(`     轮询轮 durMs=${poll.durMs} ttftMs=${poll.ttftMs}`)
  console.log(`     总结轮 durMs=${summary.durMs} agentDurMs=${summary.agentDurMs}`)
  if (poll.durMs === 600) ok('轮询轮 durMs=600（1000→1600）：并发窗口内不再被清')
  else bad(`轮询轮 durMs=${poll.durMs}，want 600`)
  if (summary.durMs === 1200) ok('总结轮 durMs=1200 不受影响')
  else bad(`总结轮 durMs=${summary.durMs}，want 1200`)
}

// ============ T7 子 run 的槽位终态清理（防无界增长）============
// agent_end 是 run 的提交边界。子 run 末轮若无 llm_end（中断/失败），那一格没人消费；
// 不在 agent_end 删掉的话，每个子 agent 结束都留一格。
console.log('T7 子 run 槽位在 agent_end 被清理')
{
  const v = await runTurn('clobber-poll')
  const keys = Object.keys(v.turnStartTs ?? {})
  console.log(`     残留槽位 = ${JSON.stringify(keys)}`)
  if (keys.length === 0) ok('无残留：主 run 与子 run 的槽位都在各自 agent_end 清掉')
  else bad(`残留 ${keys.length} 格：${keys.join(',')}`)
}

// ============ T4 run 级失败：整行指标一起消失 ============
// session_run_error 复位了 view 级 streaming，块也被 llm_start 之外的路径收尾，
// 但**没有 llm_end（无 durMs/usage）也没有 agent_end（无 agentDurMs）** →
// AssistantTurnMeta 的 head 与 speed 双双为 null（:145 `if (!head && !speed) return null`）
// → 整行不渲染。用户读作「Agent 总耗时和 LLM 耗时都没了」。
console.log('T4 run 级失败（session_run_error，无 agent_end）')
{
  const v = await runTurn('stream-stuck')
  const b = lastTextBlock(v, '总结如下')
  assert(b, '应存在总结块')
  console.log(`     streaming=${b.streaming} durMs=${b.durMs} ttftMs=${b.ttftMs} agentDurMs=${b.agentDurMs} outputTokens=${b.outputTokens}`)
  if (b.durMs == null && b.agentDurMs == null && b.outputTokens == null) {
    ok('复现：head 与 speed 均为 null → 整行不渲染（LLM 耗时 + Agent 总耗时一起消失）')
  } else {
    bad(`未复现，durMs=${b.durMs} agentDurMs=${b.agentDurMs}`)
  }
}

// ============ T5 轮数上限截断 ============
console.log('T5 达轮数上限截断（max_iterations）')
{
  const v = await runTurn('truncated')
  const b = lastTextBlock(v, '再查一下')
  assert(b, '应存在正文块')
  console.log(`     durMs=${b.durMs} ttftMs=${b.ttftMs} agentDurMs=${b.agentDurMs}`)
  const hasAny = b.durMs != null || b.ttftMs != null || b.agentDurMs != null
  if (hasAny) ok('截断轮至少有一段指标（AgentEnd 末轮兜底生效）')
  else bad('截断轮三段全空')
}

console.log(`\n通过 ${pass} 项`)
// ============ T6 附带影响：子 run 的每轮耗时被主 run 起点污染 ============
// llm_end 在判 isSub **之前**就算 durMs = now - v.turnStartTs（:4375），子 run 的
// TurnMetric.durMs 用的也是这个值。并发窗口内子 run 拿到的是主 run 的轮起点。
console.log('T6 子 run 的每轮耗时（TurnMetric）归属')
{
  F.sessions.clear(); F.events.clear()
  F.sessions.set('sA', { id: 'sA', title: 'SA', updated_at: Date.now() })
  F.events.set('sA', seed('clobber-summary'))
  LS.set('go-code.projects', JSON.stringify({ paths: [F.ws], active: F.ws, lastSession: { path: F.ws, id: 'sA' } }))
  const copy = join(dir, `store-${Math.random().toString(36).slice(2)}.cjs`)
  copyFileSync(outfile, copy)
  const s = require(copy).useAppStore
  await s.getState().start()
  await sleep(40)
  const turns = s.getState().views['sA'].turns
  for (const t of turns) console.log(`     run=${t.runId} req=${t.requestId} durMs=${t.durMs}`)
  const sub = turns.find((t) => t.runId === 'r2')
  // 子 run 自己的 llm_start=250 → llm_end=1500 = 1250ms
  // 修复前记成 500（1500−1000，用了主 run 的轮起点）
  if (sub && sub.durMs === 1250) ok('子 run durMs=1250 = 自己的 250→1500（修复前误记 500）')
  else bad(`子 run durMs=${sub && sub.durMs}，want 1250`)
}
