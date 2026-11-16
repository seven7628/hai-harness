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

// 10) 场景2b：DM delta 的 group_id 是空串（不是缺字段），必须有 agent_id。
const dmDelta = (id, index, text, extra = {}) => ({
  group_id: '', agent_id: 'ag_a', round_id: '', message_id: id, index, delta: text, ...extra,
})
let dm = mod.applyHubDelta([], dmDelta('dm1', 0, '单聊'), [])
assert(dm.length === 1 && dm[0].groupId === '' && dm[0].agentId === 'ag_a', 'DM 空 groupId 收下（草稿 groupId 为空串）')
dm = mod.applyHubDelta(dm, dmDelta('dm1', 1, '第二块'), [])
assert(dm[0].text === '单聊第二块' && dm[0].index === 1, 'DM 同 id 顺序合并')
// 空 agentId：不知道草稿归谁，群和 DM 的过滤都靠它 → 必须拒绝（空 groupId + 空 agentId
// 既不是群也不是单聊，收下只会在 UI 里长出一条无处可归的气泡）。
assert(mod.applyHubDelta([], dmDelta('dm2', 0, '无主', { agent_id: '' }), []).length === 0, 'DM 空 agentId 拒绝')
assert(mod.applyHubDelta([], { ...delta('dm3', 0, '无主'), agent_id: '' }, []).length === 0, '群 delta 空 agentId 也拒绝')
// 群 delta 语义不变：group_id 必须原样带下来（GroupView 按 groupId 过滤），
// 群的 delta 与 DM 的草稿在同一个数组里互不合并。
let mixed = mod.applyHubDelta([], delta('g1x', 0, '群的'), [])
mixed = mod.applyHubDelta(mixed, dmDelta('d1x', 0, '单聊的'), [])
assert(mixed.length === 2, '群与 DM 草稿并存不合并')
assert(mixed.find((d) => d.messageId === 'g1x').groupId === 'grp_1', '群草稿保留 groupId')
assert(mixed.find((d) => d.messageId === 'd1x').groupId === '', 'DM 草稿 groupId 为空串')
assert(mod.applyHubDelta([], { message_id: 'noscope', index: 0, delta: 'x' }, []).length === 0, '无 groupId 也无 agentId 丢弃')

// 11) DM 两个 message_id 并行（两轮 LLM 各一条气泡），各自 index 单调、各自 final。
let two2 = mod.applyHubDelta([], dmDelta('r1', 0, '第一轮'), [])
two2 = mod.applyHubDelta(two2, dmDelta('r2', 0, '第二轮'), [])
two2 = mod.applyHubDelta(two2, dmDelta('r1', 1, '+1'), [])
two2 = mod.applyHubDelta(two2, dmDelta('r2', 1, '+2'), [])
assert(two2.length === 2, '两个 DM message_id 并行 = 两条草稿')
assert(two2.map((d) => d.text).join('|') === '第一轮+1|第二轮+2', '并行草稿各自拼接，不交叉')
// 迟到的 r1 块不能写进 r2（按 message_id 定位，不是按 agent）。
assert(mod.applyHubDelta(two2, dmDelta('r1', 1, 'LATE'), [])[0].text === '第一轮+1', 'DM 迟到块按 id 丢弃')
const dmFin = mod.finalizeHubDraft(two2, [], 'r1')
assert(dmFin.drafts.length === 1 && dmFin.drafts[0].messageId === 'r2', 'DM final 只删自己那条')
assert(mod.applyHubDelta(dmFin.drafts, dmDelta('r1', 2, 'ghost'), dmFin.finalized).length === 1, 'DM final 后迟到 delta 不复活')
// DM 首块必须 index=0（与群同口径）：中途打开会话时不造半截气泡。
assert(mod.applyHubDelta([], dmDelta('mid', 3, '半截'), []).length === 0, 'DM 首块非 0 丢弃')

// 12) agent_end 的作用域：DM 只清 DM 草稿，群只清本群草稿，同一个 agent 互不误伤。
let scoped = mod.applyHubDelta([], delta('gg', 0, '群的'), [])
scoped = mod.applyHubDelta(scoped, dmDelta('dd', 0, '单聊的'), [])
const afterDM = mod.dropDraftsForAgent(scoped, 'ag_a', '')
assert(afterDM.length === 1 && afterDM[0].groupId === 'grp_1', 'DM agent_end 只清 DM 草稿')
const afterGRP = mod.dropDraftsForAgent(scoped, 'ag_a', 'grp_1')
assert(afterGRP.length === 1 && afterGRP[0].groupId === '', '群 agent_end 只清本群草稿')
assert(mod.dropDraftsForAgent(scoped, 'ag_a').length === 0, '不传作用域 = 清该 agent 全部（旧行为）')
assert(mod.dropDraftsForAgent(scoped, 'ag_c', '') === scoped, '没有匹配时返回原数组')

// 13) dmScopeFromSession 只认 hub.<agent>.dm。
assert(mod.dmScopeFromSession('hub.ag_a.dm')?.agentId === 'ag_a', '解析 DM agent')
assert(mod.dmScopeFromSession('hub.ag_a.g.grp_1') === null, '群会话不是 DM')
assert(mod.dmScopeFromSession('proj.ag_a.dm') === null, '非 hub 会话不是 DM')
assert(mod.dmScopeFromSession('') === null && mod.dmScopeFromSession(undefined) === null, '空 session_id')

// 14) 谁在真的流（liveDraftIds）：一轮 AgentEnd 才逐条发 final，所以一次输入的持续回复
//     会同时挂好几条草稿（每个 LLM 轮次一段）。光标/「正在输入」只给每个 agent 最后
//     那条 —— 否则用户看到「一句话没说完，三根光标一起闪」。
let seg = mod.applyHubDelta([], delta('s1', 0, '我先看看'), [])
seg = mod.applyHubDelta(seg, delta('s2', 0, '找到了'), [])
seg = mod.applyHubDelta(seg, delta('s3', 0, '建好了'), [])
let live = mod.liveDraftIds(seg)
assert(seg.length === 3 && live.size === 1 && live.has('s3'), '同一 agent 三段草稿：只有最后一段活着')
// 最后一段再涨块，仍然只有它活着（判据是段序，不是「谁的 index 最大」）。
seg = mod.applyHubDelta(seg, delta('s3', 1, '，发过去'), [])
assert(mod.liveDraftIds(seg).size === 1 && mod.liveDraftIds(seg).has('s3'), '活草稿涨块不改变归属')
// 并行 agent：各留各的活草稿（群里两个 agent 同时回 = 两根光标，这是对的）。
const para = mod.applyHubDelta(seg, delta('b1', 0, '我也在写', { agent_id: 'ag_b' }), [])
live = mod.liveDraftIds(para)
assert(live.size === 2 && live.has('s3') && live.has('b1'), '并行 agent 各自一条活草稿')
// 中间那条 final 落地（删草稿）不抢走活草稿的归属。
const midFin = mod.finalizeHubDraft(para, [], 's2')
live = mod.liveDraftIds(midFin.drafts)
assert(live.size === 2 && live.has('s3') && live.has('b1'), '删掉中间草稿后归属不变')
// 群与 DM 用同一个 agentId：作用域分开，DM 的草稿不能把群的活草稿顶掉。
let scopeLive = mod.applyHubDelta([], delta('gl1', 0, '群的'), [])
scopeLive = mod.applyHubDelta(scopeLive, dmDelta('dl1', 0, '单聊的'), [])
live = mod.liveDraftIds(scopeLive)
assert(live.size === 2 && live.has('gl1') && live.has('dl1'), '群与 DM 各自一条活草稿')
// 空列表：不画光标（返回空集，调用方 has() 恒 false）。
assert(mod.liveDraftIds([]).size === 0, '没有草稿时没有活草稿')

console.log(`\n✅ hub 流式草稿：${pass} 项全部通过`)
