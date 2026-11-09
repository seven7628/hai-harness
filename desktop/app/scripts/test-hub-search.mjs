#!/usr/bin/env node
// P4-04b-1 前端搜索纯逻辑验收（Node 直跑，无需浏览器）：
//   search.ts：查询归一/scope 白名单/hub_search payload/游标编码/命中映射/
//              翻页去重/点命中「加载 seq 附近一页」的计划与合并；
//   HubSurface/useHubStore 静态契约：250ms debounce、hub_search 真的被调用、
//              点命中真的走 searchAroundPlan（删掉 around-seq 加载 → 这里 RED）、
//              Escape 清空、owner-only 徽标存在。
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import { build } from 'esbuild'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const entry = join(__dirname, '..', 'src', 'hub', 'search.ts')
const result = await build({
  entryPoints: [entry],
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
})
const mod = await import(`data:text/javascript;base64,${Buffer.from(result.outputFiles[0].text).toString('base64')}`)

const storeSrc = readFileSync(join(__dirname, '..', 'src', 'hub', 'useHubStore.ts'), 'utf8')
const surfaceSrc = readFileSync(join(__dirname, '..', 'src', 'hub', 'HubSurface.tsx'), 'utf8')

let pass = 0
const ok = (m) => {
  pass++
  console.log(`  ✅ ${m}`)
}
const assert = (cond, m) => {
  if (!cond) throw new Error(`断言失败: ${m}`)
  ok(m)
}
const eq = (got, want, m) => {
  if (got !== want) throw new Error(`断言失败: ${m}（got=${JSON.stringify(got)} want=${JSON.stringify(want)}）`)
  ok(m)
}

// —— 1) 查询/范围归一 ——
assert(mod.HUB_SEARCH_DEBOUNCE_MS === 250, 'debounce 固定 250ms（设计定稿）')
eq(mod.normalizeSearchQuery('  alpha  '), 'alpha', '查询去首尾空白')
eq(mod.normalizeSearchQuery(undefined), '', '非字符串查询归一为空')
eq(mod.normalizeSearchScope('dm'), 'dm', '合法 scope 保留')
eq(mod.normalizeSearchScope('nonsense'), 'all', '非法 scope 回落到 all（不能让后端收到越界值）')
eq(mod.normalizeSearchScope(undefined), 'all', '缺省 scope = all')
eq(mod.searchScopeLabel('group'), '群聊', 'scope 文案：群聊')
eq(mod.searchScopeLabel('dm'), '私聊', 'scope 文案：私聊')
eq(mod.searchScopeLabel('all'), '全部', 'scope 文案：全部')

// —— 2) hub_search payload：limit 钳位 + 游标编码 ——
{
  const p1 = mod.searchPagePayload('  alpha ', 'all')
  eq(p1.query, 'alpha', 'payload 用归一后的查询')
  eq(p1.scope, 'all', 'payload scope')
  eq(p1.limit, 50, '默认 limit=50')
  assert(!('before' in p1), '第一页不带 before')
  eq(mod.searchPagePayload('x', 'all', { limit: 0 }).limit, 1, 'limit 下界 1')
  eq(mod.searchPagePayload('x', 'all', { limit: 999 }).limit, 200, 'limit 上界 200')
  const p2 = mod.searchPagePayload('x', 'group', {
    groupId: 'grp_1',
    cursor: { created_at: '2026-11-09T10:00:00Z', id: 'msg_9', seq: 9 },
  })
  eq(p2.group_id, 'grp_1', 'scope=group 带 group_id')
  eq(p2.before.id, 'msg_9', '游标 id')
  eq(p2.before.created_at, '2026-11-09T10:00:00Z', '游标 created_at')
  eq(p2.before.seq, 9, '游标 seq（单会话快速路径）')
  const p3 = mod.searchPagePayload('x', 'dm', { agentId: 'ag_1' })
  eq(p3.agent_id, 'ag_1', 'scope=dm 带 agent_id')
  assert(!('before' in mod.searchPagePayload('x', 'all', { cursor: { id: '', created_at: '' } })), '残缺游标不带 before')
}

// —— 3) 结果映射：坏条目丢弃、owner_only 保留、scope 决定目标 id ——
{
  const g = mod.mapSearchHit({
    scope: 'group', group_id: 'grp_1', name: '复盘群', seq: 12, message_id: 'msg_12',
    snippet: '…关键词…', sender: 'Alice', sender_kind: 'agent', kind: 'text',
    created_at: '2026-11-09T10:00:00Z', owner_only: true,
  })
  assert(!!g, '合法群命中映射成功')
  eq(g.scope, 'group', '命中 scope')
  eq(g.group_id, 'grp_1', '群命中带 group_id')
  eq(g.owner_only, true, 'owner-only 标记保留（UI 必须打标）')
  eq(g.sender, 'Alice', '发送者保留')
  eq(g.message_id, 'msg_12', 'message_id 保留')
  assert(mod.mapSearchHit({ scope: 'group', seq: 12 }) === null, '群命中缺 group_id → 丢弃')
  assert(mod.mapSearchHit({ scope: 'dm', seq: 12 }) === null, 'DM 命中缺 agent_id → 丢弃')
  assert(mod.mapSearchHit({ scope: 'group', group_id: 'g', seq: 0 }) === null, 'seq<=0 → 丢弃')
  assert(mod.mapSearchHit({ scope: 'group', group_id: 'g', seq: 3 }).owner_only === false, '缺省 owner_only=false')
  const dm = mod.mapSearchHit({ scope: 'dm', agent_id: 'ag_1', seq: 4 })
  eq(dm.agent_id, 'ag_1', 'DM 命中带 agent_id')
  eq(dm.message_id, 'msg_4', '没有 message_id 时按 seq 兜底')
}

// —— 4) 页面映射 + 翻页去重 + 游标 ——
{
  const page = mod.mapSearchPage({
    hits: [
      { scope: 'group', group_id: 'g1', seq: 5, message_id: 'msg_5' },
      { scope: 'group', group_id: 'g1', seq: 4, message_id: 'msg_4' },
      { scope: 'dm', seq: 9 }, // 坏条目：缺 agent_id
    ],
    has_more: true,
    next_before: { created_at: '2026-11-09T09:00:00Z', id: 'msg_4', seq: 4 },
  })
  eq(page.hits.length, 2, '坏条目被丢弃')
  eq(page.hasMore, true, 'has_more 保留')
  eq(page.next.id, 'msg_4', 'next_before 保留')
  const noCursor = mod.mapSearchPage({ hits: [{ scope: 'group', group_id: 'g', seq: 1 }], has_more: true })
  eq(noCursor.hasMore, false, 'has_more=true 但没有游标 → 视为没有下一页（否则会无限翻）')
  const empty = mod.mapSearchPage({ hits: [], has_more: true, next_before: { created_at: 'x', id: 'y' } })
  eq(empty.hasMore, false, '空页不宣称还有更多')
  const merged = mod.mergeSearchHits(page.hits, [
    { scope: 'group', group_id: 'g1', seq: 4, message_id: 'msg_4' },
    { scope: 'group', group_id: 'g1', seq: 3, message_id: 'msg_3' },
  ])
  eq(merged.map((h) => h.message_id).join(','), 'msg_5,msg_4,msg_3', '翻页去重且保持顺序')
  eq(mod.searchHitKey(page.hits[0]), 'group:g1:msg_5', '命中键含 scope + 会话 + message_id')
  assert(
    mod.searchHitKey({ scope: 'dm', agent_id: 'ag_1', seq: 2, message_id: 'msg_2' }) !==
      mod.searchHitKey({ scope: 'dm', agent_id: 'ag_2', seq: 2, message_id: 'msg_2' }),
    '不同 DM 的同 seq/message_id 不撞键',
  )
}

// —— 4b) 逐会话续页游标必须 opaque 回传（复审 B1：丢掉它就退回第一页）——
{
  const page = mod.mapSearchPage({
    hits: [
      { scope: 'group', group_id: 'g1', seq: 5, message_id: 'msg_5' },
      { scope: 'dm', agent_id: 'ag1', seq: 7, message_id: 'msg_7' },
    ],
    has_more: true,
    next_before: {
      created_at: '2026-11-09T10:00:00Z', id: 'msg_5', seq: 5,
      conversations: { 'g:g1': 4, 'd:ag1': 0 },
    },
  })
  assert(page.next && page.next.conversations, 'next_before.conversations 必须保留（不能只留 created_at/id）')
  eq(page.next.conversations['g:g1'], 4, '续页 seq 原样保留')
  eq(page.next.conversations['d:ag1'], 0, '0 值必须保留（该会话本轮没发出去，下页重取）')
  const payload = mod.searchPagePayload('q', 'all', { cursor: page.next })
  eq(
    JSON.stringify(payload.before.conversations),
    '{"g:g1":4,"d:ag1":0}',
    'mapSearchPage → searchPagePayload 往返不丢 conversations',
  )
  eq(payload.before.id, 'msg_5', '兼容字段 id 一起带上')

  // 只有 conversations（没有 created_at/id 的）游标也必须能翻页。
  const mapOnly = mod.mapSearchPage({
    hits: [{ scope: 'group', group_id: 'g1', seq: 2, message_id: 'msg_2' }],
    has_more: true,
    next_before: { conversations: { 'g:g1': 2 } },
  })
  assert(!!mapOnly.next, '只有 conversations 的游标也是可用游标')
  eq(mod.searchPagePayload('q', 'all', { cursor: mapOnly.next }).before.conversations['g:g1'], 2, '只有 map 也能翻页')

  // 非法 key / 负 value 丢弃；空 map 视为没有续页信息。
  eq(
    JSON.stringify(mod.readSearchConversations({ 'x:1': 2, 'g:ok': 3, 'g:neg': -1, 'd:bad': 'nope' })),
    '{"g:ok":3}',
    '非法 key/负值/非数字被丢弃',
  )
  eq(mod.readSearchConversations({}), undefined, '空 map → 没有续页信息')
  eq(mod.readSearchConversations({ 'x:1': 2 }), undefined, '只有非法 key → 没有续页信息')
  const broken = mod.mapSearchPage({
    hits: [{ scope: 'group', group_id: 'g1', seq: 1, message_id: 'msg_1' }],
    has_more: true,
    next_before: { conversations: { 'x:1': 2 } },
  })
  eq(broken.hasMore, false, '没有可用游标 → has_more=false（不能宣称还能翻）')

  // 旧格式游标（无 conversations）仍按 created_at/id 走（scope=group/dm 快速路径）。
  const legacy = mod.mapSearchPage({
    hits: [{ scope: 'group', group_id: 'g1', seq: 1, message_id: 'msg_1' }],
    has_more: true,
    next_before: { created_at: '2026-11-09T10:00:00Z', id: 'msg_1', seq: 1 },
  })
  assert(!!legacy.next && !legacy.next.conversations, '旧游标不凭空造 conversations')
  eq(mod.searchPagePayload('q', 'group', { cursor: legacy.next }).before.id, 'msg_1', '旧游标继续可用')
}

// —— 5) 点命中：加载「命中 seq 附近一页」的计划 ——
{
  const g = mod.searchAroundPlan('group', 'grp_1', 42)
  eq(g.command, 'hub_group_history', '群命中用 hub_group_history')
  eq(g.payload.group_id, 'grp_1', 'around payload group_id')
  eq(g.payload.before_seq, 43, 'before_seq = seq+1（把命中自己包含进来）')
  eq(g.payload.limit, 50, 'around 页大小 50')
  const dm = mod.searchAroundPlan('dm', 'ag_1', 7)
  eq(dm.command, 'hub_history', 'DM 命中用 hub_history')
  eq(dm.payload.agent_id, 'ag_1', 'around payload agent_id')
  eq(dm.payload.before_seq, 8, 'DM before_seq = seq+1')
  assert(mod.searchAroundPlan('group', '', 1) === null, '缺目标 → 不产生请求')
  assert(mod.searchAroundPlan('group', 'g', 0) === null, 'seq<=0 → 不产生请求')
}

// —— 6) 合并 around 页：去重/排序/保留乐观气泡 ——
{
  const loaded = [
    { seq: 100, id: 'msg_100', text: '最新' },
    { seq: -3, text: '乐观气泡', pending: true },
  ]
  const page = [
    { seq: 41, id: 'msg_41', text: '附近' },
    { seq: 42, id: 'msg_42', text: '命中' },
    { seq: 100, id: 'msg_100', text: '最新（页里也有）' },
  ]
  const merged = mod.mergeAroundPage(loaded, page)
  eq(merged.map((m) => m.seq).join(','), '-3,41,42,100', '合并后按 seq 升序、乐观气泡保留')
  eq(merged.filter((m) => m.seq === 100).length, 1, '同 seq 不重复')
  eq(mod.hasSeq(merged, 42), true, '命中 seq 已在时间线里')
  eq(mod.hasSeq(merged, 55), false, '未加载的 seq 判定为 false')
  eq(mod.hasSeq(merged, -3), false, '乐观气泡（seq<=0）不算命中')
  eq(mod.aroundHasMore(merged), true, '最小真实 seq=41 > 1 → 还有更早')
  eq(mod.aroundHasMore([{ seq: 1 }]), false, '从 seq=1 开始 → 没有更早')
  eq(mod.aroundHasMore([{ seq: -1 }]), false, '只有乐观气泡 → 没有更早')
}

// —— 7) 静态契约（这些是「界面上真的走通」的底线，不靠手点） ——
{
  assert(storeSrc.includes("'hub_search'"), 'useHubStore 真的调用 hub_search')
  assert(storeSrc.includes('searchAroundPlan('), '点命中必须走 searchAroundPlan 加载命中附近一页（删掉 → 本测试 RED）')
  assert(storeSrc.includes('requestJump(target.seq)'), '点命中后必须 requestJump(seq)（复用深链的跳转守卫）')
  assert(storeSrc.includes('mergeAroundPage('), 'around 页必须合并进当前时间线（不能整段替换）')
  assert(storeSrc.includes('searchGen'), '搜索/翻页必须有代际，迟到响应不得覆盖新结果')
  assert(surfaceSrc.includes('HUB_SEARCH_DEBOUNCE_MS'), '输入框使用共享的 250ms debounce 常量')
  assert(surfaceSrc.includes("e.key === 'Escape'"), 'Escape 必须清空/关闭搜索')
  assert(surfaceSrc.includes('仅群主可见'), 'owner-only 结果必须有徽标')
  assert(surfaceSrc.includes("'all', 'group', 'dm'"), '三种 scope chip 都在')
  assert(surfaceSrc.includes('searchHasMore'), '结果面板支持加载更早结果')
}

console.log(`\n✅ hub 搜索纯逻辑 + 契约：${pass} 项全部通过`)
