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
// 重复 index（同 index 不同正文）必须丢弃：弱网重发是常态，不能把重发内容再拼一遍。
drafts = mod.applyHubDelta(drafts, delta('m1', 2, '重复块'), [])
assert(drafts[0].text === '你好呀', '重复 index 丢弃（正文不变）')
assert(mod.applyHubDelta([], delta('m2', 3, '半截'), []).length === 0, '首块非 0 丢弃')
// 空洞（index 跳号）：草稿是 best effort，接收到的内容按到达顺序拼接，不凭空补内容。
let gap = mod.applyHubDelta([], delta('m3', 0, 'A'), [])
gap = mod.applyHubDelta(gap, delta('m3', 2, 'C'), [])
assert(gap[0].text === 'AC' && gap[0].index === 2, 'index 空洞：已到内容拼接，不阻塞')

// 3) 并行 agent 各自草稿。
let two = mod.applyHubDelta([], delta('a1', 0, 'A'), [])
two = mod.applyHubDelta(two, delta('b1', 0, 'B', { agent_id: 'ag_b' }), [])
assert(two.length === 2 && two.map((d) => d.agentId).join(',') === 'ag_a,ag_b', '并行草稿不合并')

// 4) final 覆盖：删草稿 + finalized；之后迟到 delta 丢弃。
let fin = mod.finalizeHubDraft(two, [], 'a1')
assert(fin.drafts.length === 1 && fin.finalized[0] === 'a1', 'final 删除对应草稿')
assert(mod.applyHubDelta(fin.drafts, delta('a1', 9, '迟到的'), fin.finalized).length === 1, 'final 后迟到 delta 丢弃')
// finalized 守卫必须自己拦住「index 0 打进空草稿列表」：这里没有 index 单调可挡，
// 删掉 applyHubDelta 里的 finalized 判断就会凭空重建一条草稿（UI 幽灵气泡）。
assert(mod.applyHubDelta([], delta('a1', 0, 'ghost'), fin.finalized).length === 0, 'final 后 index 0 迟到 delta 不重建草稿')
// final 先到、delta 后到（WS 乱序或重连补包）：没有草稿可挡，只能靠 finalized 守卫。
const finFirst = mod.finalizeHubDraft([], [], 'early-final')
assert(mod.applyHubDelta([], delta('early-final', 0, 'ghost'), finFirst.finalized).length === 0, 'final-before-delta：后到 delta 不造草稿')

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

// 8) 重连/重进群对账：历史里已经落地的 message_id 要删草稿并进 finalized，
//    历史缺页里没有的草稿不能误杀（仍可能在流式）；迟到 delta 不能再复活。
let resyncDrafts = mod.applyHubDelta([], delta('sync1', 0, '半截'), [])
resyncDrafts = mod.applyHubDelta(resyncDrafts, delta('sync2', 0, '还在流', { agent_id: 'ag_b' }), [])
const sync = mod.reconcileDraftsWithMessages(resyncDrafts, ['older'], ['sync1', 'unrelated-final'])
assert(sync.drafts.length === 1 && sync.drafts[0].messageId === 'sync2', '对账只删历史里已落地的草稿')
assert(sync.finalized[0] === 'sync1' && sync.finalized.includes('older'), '对账把已落地的 id 记入 finalized（保留旧 LRU）')
assert(mod.applyHubDelta(sync.drafts, delta('sync1', 0, 'ghost'), sync.finalized).length === 1, '对账后迟到 delta 不复活草稿')
const untouched = mod.reconcileDraftsWithMessages(resyncDrafts, [], ['other-page'])
assert(untouched.drafts === resyncDrafts, '无匹配时返回原数组（不触发多余渲染）')
assert(resyncDrafts.length === 2, '对账不修改入参')

// 9) 群 session id 解析（只认 hub.<agent>.g.<group>）。
assert(mod.groupScopeFromSession('hub.ag_a.g.grp_1')?.agentId === 'ag_a', '解析 agent')
assert(mod.groupScopeFromSession('hub.ag_a.g.grp_1')?.groupId === 'grp_1', '解析 group')
assert(mod.groupScopeFromSession('hub.ag_a.dm') === null, 'DM 不是群会话')
assert(mod.groupScopeFromSession(undefined) === null, '缺 session_id')

console.log(`\n✅ hub 流式草稿：${pass} 项全部通过`)
