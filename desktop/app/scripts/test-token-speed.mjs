// 输出速度（tokens/s）口径回归测试（真实 store + esbuild 打包，Node 直接跑，无需浏览器）。
//
// 背景（2026-09-10 报障）：对话记录底部显示「LLM 总耗时 11.9s · 7316.90 tokens/s」，
// 而实际生成速度只有 100~200 tok/s。根因是分子分母口径不一致：
//   - 分子 usage.Output 统计的是「思考 + 正文」的全部输出 token（reasoning 模型下思考
//     往往占绝大部分，如 deepseek-v4-flash 实测 2236 out 里 2028 是 reasoning）；
//   - 分母却用 durMs - ttftMs。当上游把 reasoning 只放在末次 usage、流里没有
//     reasoning_chunk 时，ttftMs 实际落在首个 **content** chunk 上（思考早已结束），
//     分母只剩正文流式那零点几秒 → 分子含思考、分母不含 → 速度虚高几十倍。
// 修复：genMsOf 纯函数统一口径 —— 思考未流式但有 reasoning token 时用整轮 durMs 作分母。
//
// 覆盖：
//   T1 reasoning 未流式（报障场景）→ 分母 = 整轮 durMs，速度回到 100~200 量级
//   T2 reasoning 已流式（有 thinking）→ 分母 = durMs - ttftMs（保持原语义）
//   T3 非 reasoning 模型 → 行为不变（durMs - ttftMs）
//   T4 llm_end 落地的块上 outputTokens/reasoningTokens 就位（供 UI 与 checkpoint 用）
//   T5 纯函数幂等/边界：durMs 缺失 → undefined，不为 NaN/负值
//
// 用法：npm run test:tok-speed
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// —— 共享 localStorage ——
const LS = new Map()
global.localStorage = {
  getItem: (k) => (LS.has(k) ? LS.get(k) : null),
  setItem: (k, v) => LS.set(k, String(v)),
  removeItem: (k) => LS.delete(k),
}

// —— 假 bridge（list/new_session/事件流）——
const F = { sessions: new Map(), events: new Map(), ws: '/ws-tok', cb: null }
function emitResp(id, data) {
  if (F.cb) F.cb(JSON.stringify({ event_type: 'command_response', id, ok: true, data, workspace: F.ws }))
}
// 造一轮：llm_start →（可选 reasoning chunk）→ content chunk → llm_end
// meta 控制 thinking 是否流式、usage 里的 reasoning token 数。
function seedTurn(sid, { streamedThinking, output, reasoning, content }) {
  const evs = []
  const T0 = Date.parse('2026-09-10T10:13:13.096+08:00')
  const push = (offsetMs, o) => evs.push({ session_id: sid, timestamp: new Date(T0 + offsetMs).toISOString(), ...o })
  push(0, { event_type: 'agent_start', run_id: 'r1' })
  push(0, { event_type: 'llm_start', run_id: 'r1', model: 'deepseek/deepseek-v4-flash', request_id: 'req1' })
  if (streamedThinking) {
    // 思考流式到达：首个 reasoning_chunk 早（TTFT 落在它上面）
    push(500, { event_type: 'reasoning_chunk', run_id: 'r1', request_id: 'req1', content: '让我想想…' })
  }
  // 正文 chunk：位置决定 TTFT（未流式思考时 TTFT 落在这里 = 11.6s 后）
  push(streamedThinking ? 9000 : 11600, { event_type: 'content_chunk', run_id: 'r1', request_id: 'req1', content })
  push(11900, {
    event_type: 'llm_end', run_id: 'r1', request_id: 'req1', model: 'deepseek/deepseek-v4-flash',
    finish_reason: 'stop', content,
    usage: { Input: 86845, Output: output, CacheRead: 86656, CacheWrite: 0, Reasoning: reasoning, TotalTokens: 88923 },
  })
  return evs
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

const dir = mkdtempSync(join(tmpdir(), 'tok-speed-'))
const outfile = join(dir, 'store.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile, logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' }, // mock.ts 的 ?raw/?url 资源

})
const mod = require(outfile)
const { useAppStore, genMsOf } = mod

global.window = {
  localStorage: global.localStorage,
  matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {}, removeEventListener() {},
  desktop: makeDesktop(),
}
global.document = { documentElement: { classList: { add() {}, remove() {} } } }

let pass = 0
function ok(label) { pass++; console.log(`  ✅ ${label}`) }

// —— 造一个已挂载 sA 的 store，并把事件推进去 ——
async function runTurn(seed) {
  F.sessions.clear(); F.events.clear()
  F.sessions.set('sA', { id: 'sA', title: 'SA', updated_at: Date.now() })
  F.events.set('sA', seed)
  LS.set('go-code.projects', JSON.stringify({ paths: [F.ws], active: F.ws, lastSession: { path: F.ws, id: 'sA' } }))
  // 每轮用独立模块副本：require 缓存会让 start() 直接跳过（模拟真实刷新重置）
  const copy = join(dir, `store-${Math.random().toString(36).slice(2)}.cjs`)
  const s = await import('node:fs').then((fs) => { fs.copyFileSync(outfile, copy); return require(copy).useAppStore })
  await s.getState().start()
  await sleep(40)
  const v = s.getState().views['sA']
  assert(v, 'sA 视图应已恢复')
  const blk = [...v.blocks].reverse().find((b) => b.kind === 'assistant' && b.text)
  assert(blk, '应存在带正文的 assistant 块')
  return blk
}

// ============ T1：reasoning 未流式（报障场景）============
console.log('T1 reasoning 未流式（用户在 2026-09-10 报障的那一轮）')
{
  // 实测数据：Output 2236（含 Reasoning 2028），总耗时 11.9s，TTFT 11.6s
  const blk = await runTurn(seedTurn('sA', { streamedThinking: false, output: 2236, reasoning: 2028, content: 'OK' }))
  assert.strictEqual(blk.outputTokens, 2236, 'outputTokens 应为 usage.Output')
  assert.strictEqual(blk.reasoningTokens, 2028, 'reasoningTokens 应为 usage.Reasoning')
  const genMs = blk.genMs ?? genMsOf(blk)
  const speed = blk.outputTokens / (genMs / 1000)
  assert(speed < 300, `修复后速度应回到 100~300 量级，实际 ${speed.toFixed(1)} tok/s`)
  assert(speed > 100, `速度不应过低，实际 ${speed.toFixed(1)} tok/s`)
  ok(`分母含思考：genMs=${genMs}ms → ${speed.toFixed(1)} tok/s（修复前 7453.33）`)
}

// ============ T2：思考已流式 ============
console.log('T2 reasoning 已流式（thinking 有内容）→ 分母照常减去 TTFT')
{
  const blk = await runTurn(seedTurn('sA', { streamedThinking: true, output: 2236, reasoning: 2028, content: 'OK' }))
  const genMs = blk.genMs ?? genMsOf(blk)
  // durMs 11900 - ttft 500（首个 reasoning chunk）= 11400
  assert(genMs < 11900, `已流式思考应减去 TTFT，实际 genMs=${genMs}`)
  assert(genMs >= 11000, `genMs 应在 11.4s 附近，实际 ${genMs}`)
  ok(`思考已流式：genMs=${genMs}ms（= 总耗时 - 首个 reasoning chunk）`)
}

// ============ T3：非 reasoning 模型行为不变 ============
console.log('T3 非 reasoning 模型（reasoning=0）→ 行为不变')
{
  const blk = await runTurn(seedTurn('sA', { streamedThinking: false, output: 300, reasoning: 0, content: 'OK' }))
  const genMs = blk.genMs ?? genMsOf(blk)
  // durMs 11900 - ttft 11600 = 300
  assert.strictEqual(genMs, 300, `非 reasoning 应保持 durMs - ttftMs，实际 ${genMs}`)
  ok(`非 reasoning：genMs=${genMs}ms（= 总耗时 - TTFT，语义未变）`)
}

// ============ T4：纯函数边界 ============
console.log('T4 genMsOf 边界（纯函数，UI 与 checkpoint 重放共用）')
{
  assert.strictEqual(genMsOf({ durMs: undefined, ttftMs: 100 }), undefined, 'durMs 缺失 → undefined')
  assert.strictEqual(genMsOf({ durMs: 100, ttftMs: 500 }), 0, 'ttft > durMs → 钳到 0（不为负）')
  assert.strictEqual(genMsOf({ durMs: 11900, ttftMs: 11600, reasoningTokens: 2028 }), 11900, '未流式思考 → 整轮')
  assert.strictEqual(genMsOf({ durMs: 11900, ttftMs: 500, reasoningTokens: 2028, thinking: 'x' }), 11400, '已流式 → 减 TTFT')
  // Anthropic display=summarized（2026-09-20）：思考文本是摘要（thinking 非空），但 token
  // 数是完整思考量、生成时间在首字之前 → 必须用整轮 durMs。反例（旧行为）：TTFT 7.3s /
  // 总 11.2s / out 866 → 222 tok/s，真值 866/11.2 ≈ 77 tok/s。
  assert.strictEqual(
    genMsOf({ durMs: 11200, ttftMs: 7300, reasoningTokens: 612, thinking: '摘要', thinkingSummarized: true }),
    11200, '摘要化思考（Anthropic）→ 整轮 durMs')
  assert.strictEqual(
    genMsOf({ durMs: 11200, ttftMs: 7300, reasoningTokens: 612, thinking: '摘要' }),
    3900, '非摘要化（未标记）→ 仍按已流式扣 TTFT（口径不被误改）')
  // 重放一致性：同一块字段两次调用结果相同（无隐藏状态）
  const b = { durMs: 11900, ttftMs: 11600, reasoningTokens: 2028 }
  assert.strictEqual(genMsOf(b), genMsOf(b), '幂等')
  ok('durMs 缺失/负值钳制/口径分派/摘要化思考/幂等 全部通过')
}

// ============ T5：刷新（冷恢复 checkpoint → hydrate）后 Tokens/s 不丢 ============
// 报障二：页面上数字正常，一刷新就没了。刷新走 bridge 的 view_checkpoint 冷恢复
// （hydrateCheckpoint → checkpointToView），若该映射漏了 output_tokens/reasoning_tokens，
// 刷新后 UI 的 `out == null` 直接 return null → 整行数字消失。
console.log('T5 刷新（checkpoint hydrate）后数字复原')
{
  const { hydrateCheckpoint } = mod
  assert(typeof hydrateCheckpoint === 'function', 'hydrateCheckpoint 应被导出（冷恢复入口）')
  const sid = 'sRefresh'
  // 模拟 bridge 回来的 view_checkpoint：块上带 output_tokens / reasoning_tokens
  hydrateCheckpoint(sid, {
    schema_version: 1,
    blocks: [
      { kind: 'user', id: 1, text: 'hi' },
      { kind: 'assistant', id: 2, text: 'OK', thinking: '', streaming: false,
        model: 'deepseek/deepseek-v4-flash', dur_ms: 11900, ttft_ms: 11600,
        output_tokens: 2236, reasoning_tokens: 2028 },
    ],
    run_nodes: [], turns: [], usage: {}, cost_usd: 0,
  })
  const v = useAppStore.getState().views[sid]
  assert(v, 'hydrate 后视图应存在')
  const blk = v.blocks.find((b) => b.kind === 'assistant' && b.text)
  assert(blk, 'hydrate 后应有 assistant 块')
  assert.strictEqual(blk.outputTokens, 2236, '刷新后 outputTokens 必须复原')
  assert.strictEqual(blk.reasoningTokens, 2028, '刷新后 reasoningTokens 必须复原')
  // 数字可算（与 T1 同一口径 → 同一结果）
  const genMs = blk.genMs ?? genMsOf(blk)
  const speed = blk.outputTokens / (genMs / 1000)
  assert(Math.abs(speed - 187.9) < 0.5, `刷新后速度应与实时一致（~187.9），实际 ${speed.toFixed(1)}`)
  ok(`刷新后复原：genMs=${genMs}ms → ${speed.toFixed(1)} tok/s（与实时一致）`)
}

console.log(`\n全部通过（${pass} 项）`)
