#!/usr/bin/env node
// P3a-09：群内流式草稿纯逻辑验收（Node 直跑）。
//   applyHubDelta 的 index 单调/并行草稿/256KB 上限；
//   finalizeHubDraft 的 final 覆盖与 finalized LRU；
//   agent_end 兜底清草稿、session_id 解析。
import { build } from 'esbuild'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const entry = join(__dirname, '..', 'src', 'hub', 'groupStream.ts')
const result = await build({
  entryPoints: [entry], bundle: true, platform: 'node', format: 'esm',
  write: false, logLevel: 'silent',
})
const source = result.outputFiles[0].text
const mod = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

const delta = (id, index, text, extra = {}) => ({
  group_id: 'grp_1', agent_id: 'ag_a', round_id: 'rnd_1', message_id: id, index, delta: text, ...extra,
})

// 1) 同 id 顺序合并。
let drafts = mod.applyHubDelta([], delta('m1', 0, '你'), [])
drafts = mod.applyHubDelta(drafts, delta('m1', 1, '好'), [])
drafts = mod.applyHubDelta(drafts, delta('m1', 2, '呀'), [])
assert(drafts.length === 1 && drafts[0].text === '你好呀' && drafts[0].index === 2, '同 id 顺序合并')

// 2) 迟到/乱序丢弃；首块 index 非 0 不造半截草稿。
drafts = mod.applyHubDelta(drafts, delta('m1', 1, 'LATE'), [])
assert(drafts[0].text === '你好呀', 'index <= 当前值丢弃')
assert(mod.applyHubDelta([], delta('m2', 3, '半截'), []).length === 0, '首块非 0 丢弃')

// 3) 并行 agent 各自草稿。
let two = mod.applyHubDelta([], delta('a1', 0, 'A'), [])
two = mod.applyHubDelta(two, delta('b1', 0, 'B', { agent_id: 'ag_b' }), [])
assert(two.length === 2 && two.map((d) => d.agentId).join(',') === 'ag_a,ag_b', '并行草稿不合并')

// 4) final 覆盖：删草稿 + finalized；之后迟到 delta 丢弃。
let fin = mod.finalizeHubDraft(two, [], 'a1')
assert(fin.drafts.length === 1 && fin.finalized[0] === 'a1', 'final 删除对应草稿')
assert(mod.applyHubDelta(fin.drafts, delta('a1', 9, '迟到的'), fin.finalized).length === 1, 'final 后迟到 delta 丢弃')

// 5) finalized LRU 200。
let finalized = []
for (let i = 0; i < 250; i++) finalized = mod.finalizeHubDraft([], finalized, `f${i}`).finalized
assert(finalized.length === 200 && finalized[0] === 'f249' && !finalized.includes('f0'), 'finalized 只留最近 200 个')

// 6) 256KB 上限：超出丢后续 delta。
let big = mod.applyHubDelta([], delta('big', 0, 'x'.repeat(mod.HUB_DRAFT_MAX_BYTES - 2)), [])
big = mod.applyHubDelta(big, delta('big', 1, 'toolong'), [])
assert(big[0].bytes <= mod.HUB_DRAFT_MAX_BYTES && big[0].text.length === mod.HUB_DRAFT_MAX_BYTES - 2, '草稿 256KB 上限')

// 7) agent_end 兜底清本轮草稿。
const dropped = mod.dropDraftsForAgent(two, 'ag_a')
assert(dropped.length === 1 && dropped[0].agentId === 'ag_b', '按 agent 清草稿')

// 8) 群 session id 解析（只认 hub.<agent>.g.<group>）。
assert(mod.groupScopeFromSession('hub.ag_a.g.grp_1')?.agentId === 'ag_a', '解析 agent')
assert(mod.groupScopeFromSession('hub.ag_a.g.grp_1')?.groupId === 'grp_1', '解析 group')
assert(mod.groupScopeFromSession('hub.ag_a.dm') === null, 'DM 不是群会话')
assert(mod.groupScopeFromSession(undefined) === null, '缺 session_id')

console.log(`\n✅ hub 流式草稿：${pass} 项全部通过`)
