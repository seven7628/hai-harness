#!/usr/bin/env node
// 会话区版式验收（Node 直跑，无需浏览器）。
//
// 纯函数：conversationDay.dayKey/dayLabel（日期分隔线）+ conversation.formatClockTime；
// 静态契约：DM/群共用 ConversationMessage 的两种版式（右侧暖气泡 / 左侧无气泡作者头）、
//           群头 AgentStack + 两行 subtitle、DM 头 52px Luna（独立 aria-label）+ 只读提示、composer meta 行、
//           事件卡解剖、--hub-* token 两套主题都定义（且没有照抄 demo 的硬编码色）。
//
// 为什么静态契约也值得测：这次改的是「版式归属」——如果以后有人在群聊里另起一套气泡，
// 或者在 DM 里把作者头删掉，纯逻辑测试是发现不了的；这里只钉住结构，不锁文案。
import { build } from 'esbuild'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const APP = join(__dirname, '..')

async function load(entry) {
  const result = await build({
    entryPoints: [join(APP, 'src', 'hub', entry)],
    bundle: true,
    platform: 'node',
    format: 'esm',
    write: false,
    logLevel: 'silent',
  })
  const source = result.outputFiles[0].text
  return import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)
}

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const eq = (got, want, m) => {
  if (got !== want) throw new Error(`断言失败: ${m}（got=${JSON.stringify(got)} want=${JSON.stringify(want)}）`)
  ok(m)
}

// 1) 日期分隔线：本地自然日键 + 今天/昨天/同年/跨年；非法时间返回空。
{
  const day = await load('conversationDay.ts')
  const now = new Date(2026, 9, 20, 15, 0, 0).getTime() // 2026-10-20 15:00 本地
  const at = (y, m, d) => new Date(y, m - 1, d, 9, 0, 0).toISOString()
  eq(day.dayKey(at(2026, 10, 20)), '2026-10-20', 'dayKey 取本地自然日')
  eq(day.dayKey('not-a-date'), '', '非法时间 → 空键（跳过分隔线）')
  eq(day.dayKey(''), '', '缺省时间 → 空键')
  eq(day.dayLabel(at(2026, 10, 20), 'zh', now), '今天', '今天')
  eq(day.dayLabel(at(2026, 10, 19), 'zh', now), '昨天', '昨天')
  assert(/^10\s*月\s*18\s*日$/.test(day.dayLabel(at(2026, 10, 18), 'zh', now)), '同年显示月日')
  eq(day.dayLabel(at(2025, 10, 18), 'zh', now), '2025年10月18日', '跨年带年份')
  eq(day.dayLabel(at(2026, 10, 20), 'en', now), 'Today', '英文今天')
  const enOlder = day.dayLabel(at(2026, 10, 18), 'en', now)
  assert(/Oct/.test(enOlder), `英文同年用 Intl（实际 ${enOlder}）`)
}

// 2) 消息时钟：HH:MM，本地时区；非法输入空串。
{
  const conv = await load('conversation.ts')
  const iso = new Date(2026, 9, 20, 9, 5, 0).toISOString()
  eq(conv.formatClockTime(iso), '09:05', '时钟补零')
  eq(conv.formatClockTime('nope'), '', '非法时间 → 空')
  eq(conv.formatRelativeTime(iso, new Date(2026, 9, 20, 9, 6, 0).getTime()), '1 分钟前', '相对时间口径未被改动')
}

// 3) 消息版式：demo DOM/CSS 原样移植，DM 与群共用同两个组件。
{
  const conv = readFileSync(join(APP, 'src', 'hub', 'ConversationMessage.tsx'), 'utf8')
  const demoCss = readFileSync(join(APP, 'src', 'hub', 'demo.css'), 'utf8')
  assert(conv.includes('msg-row agent') && conv.includes('agent-head') && conv.includes('agent-text') &&
    conv.includes('size={52}'), 'agent 行 = demo 的 msg-row/agent-head/agent-text + 52px 头像')
  assert(conv.includes('msg-row user') && conv.includes('user-bubble') && conv.includes('owner-avatar'),
    'user 行 = demo 的 msg-row/user-bubble/owner-avatar')
  assert(/\.hub-demo \.user-bubble\s*\{[^}]*border-radius:\s*18px/.test(demoCss), '用户气泡 18px 圆角')
  assert(/\.hub-demo \.user-bubble\s*\{[^}]*padding:\s*10px 14px/.test(demoCss), '用户气泡 10/14 内边距')
  assert(/\.hub-demo \.owner-avatar\s*\{[^}]*width:\s*24px/.test(demoCss), 'owner 头像 24px')
  assert(/\.hub-demo \.msg-row\.user \.msg-main\s*\{[^}]*max-width:\s*min\(78%,640px\)/.test(demoCss), '用户气泡给右缘 24px owner 头像 + 11px gap 留位')
  assert(/\.hub-demo \.messages\s*\{[^}]*padding:\s*30px 28px 10px/.test(demoCss), '会话区 padding 30/28')
  assert(/\.hub-demo \.event-card--handoff\s*\{/.test(demoCss) && /\.hub-demo \.event-card--approval\s*\{/.test(demoCss), '交接/审批卡有专属 token 规则')
  assert(/--user-bubble:\s*#ded7c8/.test(demoCss) && /\[data-theme="light"\]\{[^}]*--user-bubble/s.test(demoCss), 'demo 的暖色 token 两套主题都在')
  assert(/\.hub-demo \.messages-inner\s*\{[^}]*max-width:\s*760px/.test(demoCss), '会话列 760px')
  assert(/\.hub-demo \.composer\s*\{[^}]*border-radius:\s*19px/.test(demoCss), 'composer 19px 圆角')
  assert(/\.hub-demo \.event-icon\s*\{[^}]*width:\s*29px/.test(demoCss), '事件卡 29px 图标块')
  assert(/\.hub-demo \.event-body\s*\{[^}]*margin:\s*8px 0 0 38px/.test(demoCss) && /\.hub-demo \.event-actions\s*\{[^}]*margin:\s*9px 0 0 38px/.test(demoCss), '事件卡正文/操作缩进 38px（demo 原值）')
  assert(/\.hub-demo \.tool-card\s*\{/.test(demoCss) && /\.hub-demo \.tool-target\s*\{/.test(demoCss), '工具卡沿用 demo 的 tool-card/tool-target')
}

{
  const bubble = readFileSync(join(APP, 'src', 'hub', 'MessageBubble.tsx'), 'utf8')
  const bubbleCode = bubble.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '')
  assert(bubble.includes("from './ConversationMessage'") && bubble.includes('<AgentMessage') && bubble.includes('<UserMessage'), '群消息复用 ConversationMessage 的两种版式')
  // P6-12：observer 角色已删除 —— 代码里不再有「观察」标签（只读由群模式 readonly 表达）。
  // 注释可以提历史，渲染路径不许再引用 observer。
  assert(!bubbleCode.includes('observer') && !bubbleCode.includes('hub.observer'), '不再有 observer「观察」标签')
  assert(!bubbleCode.includes('回复 @'), '没有 reply_to 数据时不画「回复 @」')
  assert(bubble.includes('event-card--handoff') && bubble.includes('event-card--approval'), '交接/审批走 event-card 解剖')
  assert(bubble.includes('event-row') && bubble.includes('event-spacer') && !bubble.includes('className={`task-msg'), '事件卡包在 msg-row + 30px spacer 里（卡片左缘 41/38）')

  const stream = readFileSync(join(APP, 'src', 'hub', 'StreamingMessage.tsx'), 'utf8')
  assert(stream.includes('<AgentMessage') && !stream.includes('hub-msg-head'), '流式消息与最终消息同一版式')

  const dm = readFileSync(join(APP, 'src', 'hub', 'HubSurface.tsx'), 'utf8')
  assert(/size=\{52\}/.test(dm) && dm.includes('agentSigilLabel(') && dm.includes("t('hub.dm.locked')"),
    'DM 头 52px Luna 头像 + 独立 aria-label + 能力只读提示')
  assert(dm.includes('<AgentMessage') && dm.includes('<UserMessage'), 'DM 消息复用同一版式（作者头存在）')
  assert(dm.includes('composer-meta') && dm.includes('hub.composer.enter'), 'DM composer 有 meta 行')
  assert(!dm.includes('hub-composer-plus') && !dm.includes('hub-composer-pill'), '旧 composer 结构已移除')

  const group = readFileSync(join(APP, 'src', 'hub', 'GroupView.tsx'), 'utf8')
  assert(group.includes('<AgentStack') && /size=\{28\}/.test(group), '群头 AgentStack 28px（+N 由 planStack 决定）')
  assert(group.includes("t('hub.group.members')") && group.includes("t('hub.group.mode')") && group.includes("t('hub.group.owner')"), '群头 subtitle：成员数 / 模式 / owner')
  assert(group.includes('chat-title-wrap') && group.includes('chat-sub'), '群头用 demo 的 chat-title-wrap/chat-sub')
  assert(!group.includes('hub-conv-names'), '群头不再有第三行成员名')
  assert(group.includes('DayDivider') && group.includes('dayLabel('), '群时间线有日期分隔线')
  assert(group.includes('insertMentionToken') && group.includes("t('hub.composer.mention')"), '@ 按钮真实插入并聚焦')
  assert(group.includes('composer-meta'), '群 composer 有 meta 行')

  // P6-08：按 agent 停止的接线 —— 右栏成员行给入口、store 把 agent_id 带到命令里、
  // mock 认得同一个键（三处少一处，按钮就是装饰品）。
  const insp = readFileSync(join(APP, 'src', 'hub', 'HubInspector.tsx'), 'utf8')
  assert(insp.includes('stoppableAgentIds(') && insp.includes('hub-ov-member-stop') &&
    insp.includes('stopGroupConversation(group.id, m.agent_id)'), '右栏成员行有「只停这个成员」入口')
  const store = readFileSync(join(APP, 'src', 'hub', 'useHubStore.ts'), 'utf8')
  assert(/stopGroupConversation\(groupID, agentID\)/.test(store) && store.includes("aid ? { group_id: gid, agent_id: aid }"),
    'stopGroupConversation 带 agent_id 走 hub_conversation_stop')
  const mock = readFileSync(join(APP, 'src', 'transport', 'mock.ts'), 'utf8')
  assert(mock.includes('const stopAgent = String(cmd.payload?.agent_id ?? \'\').trim()'),
    'mock 的 hub_conversation_stop 认 agent_id（只停一个成员）')
}

// 4) 群头 +N 规则：≥5 人 = 前 4 + N（demo 口径）。
{
  const avatar = await load('avatar.ts')
  const members = Array.from({ length: 6 }, (_, i) => ({ id: `agt_${i}`, name: `A${i}`, state: 'idle' }))
  const plan = avatar.planStack(members)
  eq(plan.members.length, 4, '6 人群头画 4 个 Luna 头像')
  eq(plan.more, 2, '6 人群头显示 +2')
  const two = avatar.planStack(members.slice(0, 2))
  eq(two.members.length, 2, '2 人群头全部画出')
  eq(two.more, 0, '2 人群头没有 +N')
}

// 4b) 投递账本文案（P6-01）：每个 target 的状态 + 一轮摘要。
{
  const conv = await load('conversation.ts')
  const names = { agt_a: 'A', agt_b: 'B', agt_c: 'C' }
  eq(conv.deliveryLabel({ target: 'agt_b', status: 'queued', reason: 'target_busy' }, names),
    'B 排队中（目标忙，排队中）', 'queued 文案带原因')
  eq(conv.deliveryLabel({ target: 'agt_c', status: 'running' }, names), 'C 进行中', 'running 文案')
  eq(conv.deliveryLabel({ target: 'agt_c', status: 'done' }, names), 'C 已回复', 'done 文案')
  eq(conv.deliveryLabel({ target: 'agt_a', status: 'skipped', reason: 'pair_limit' }, names),
    'A 已跳过：互踢到上限，已跳过', 'skipped 文案带原因')
  eq(conv.deliveryLabel({ target: 'agt_a', status: 'failed', reason: 'boom' }, names),
    'A 失败：boom', '未知原因码原样显示')
  // P6-04：无序 pair + 滚动窗口的自动恢复文案。
  const now = new Date(2026, 9, 20, 12, 0, 0).getTime()
  eq(conv.conversationPairText({ pair_limit: 10, pair_count: { 'agt_a↔agt_b': 10 } }, names, now),
    'A ↔ B 10/10', 'pair 文案用 ↔（无序），没有恢复时间时只显示计数')
  const resetAt = new Date(now + 8 * 60 * 1000).toISOString()
  eq(conv.conversationPairText({ pair_limit: 10, pair_count: { 'agt_a↔agt_b': 10 }, pair_reset_at: resetAt }, names, now),
    'A ↔ B 10/10 · 约 8 分钟后自动恢复', '到限时显示自动恢复时间')
  eq(conv.pairRecoverText({ pair_limit: 10, pair_count: { 'agt_a↔agt_b': 3 }, pair_reset_at: resetAt }, now),
    '', '没到限不提示恢复时间')
  eq(conv.pairRecoverText({ pair_limit: 10, pair_count: { 'agt_a↔agt_b': 10 }, pair_reset_at: new Date(now + 30 * 60 * 1000).toISOString() }, now),
    '约 30 分钟后自动恢复', '整分钟向上取整')
  eq(conv.pairRecoverText({ pair_limit: 10, pair_count: { 'agt_a↔agt_b': 10 }, pair_reset_at: new Date(now - 1000).toISOString() }, now),
    '即将自动恢复', '已过期 → 即将恢复')
  eq(conv.pairRecoverText({ pair_limit: 10, pair_count: { 'agt_a↔agt_b': 10 }, pair_reset_at: new Date(now + 2 * 3600 * 1000).toISOString() }, now),
    '约 2 小时后自动恢复', '超过一小时用小时')
  // 旧数据（a→b 单向键）仍然能解析，不会因为升级丢计数。
  eq(conv.conversationPairText({ pair_limit: 10, pair_count: { 'agt_a→agt_b': 4 } }, names, now),
    'A ↔ B 4/10', '旧 → 键兼容解析')
  eq(conv.conversationPairText({ pair_count: {} }, names, now), '', '空 pair_count → 空')
  eq(conv.deliveryReasonText(''), '', '空原因 → 空文案')
  eq(conv.deliveryReasonText('budget'), '预算不足，未启动', '预算门的可见文案（P6-03）')
  eq(conv.deliveryReasonText('rate_limit'), '上游限流，退避重试', '429 退避的可见文案')
  // 一轮 @ 的投递汇总只有一处实现（deliveryTally + deliveryTallyText）：
  // 同一批数据分两套口径迟早会漂，P6-10 把旧的 deliverySummary 并了进去。
  assert(!('deliverySummary' in conv), '投递汇总只有 deliveryTally 一处实现（不再有第二套口径）')
}

// 4c) 状态条模型（P6-10）：本轮/本链/队列/pair 倒计时/投递账本 + 门禁 + 空态。
// 这一节是「一条状态条说清楚」的唯一判据：组件里不许再有第二套文案/门禁。
{
  const conv = await load('conversation.ts')
  const cid = 'conv_1'
  const names = { agt_a: 'A', agt_b: 'B' }
  const now = new Date(2026, 9, 20, 12, 0, 0).getTime()
  const at = (ms) => new Date(now + ms).toISOString()

  // —— 投递分档：已入队 = done+running+queued；未回复 = queued+running；失败/跳过分开 ——
  const tally = conv.deliveryTally({ deliveries: [
    { target: 'agt_z', status: 'running', turn: 1, conversation_id: 'conv_old' }, // 上一代，不计
    { target: 'agt_a', status: 'done', turn: 2, conversation_id: cid },
    { target: 'agt_b', status: 'running', turn: 2, conversation_id: cid },
    { target: 'agt_c', status: 'queued', reason: 'target_busy', turn: 2, conversation_id: cid },
    { target: 'agt_d', status: 'skipped', reason: 'pair_limit', turn: 2, conversation_id: cid },
    { target: 'agt_e', status: 'failed', reason: 'boom', turn: 2, conversation_id: cid },
  ] }, names)
  eq(tally.enqueued, 3, '已入队 = done+running+queued（不含跳过/失败/上一轮）')
  eq(tally.pending, 2, '未回复 = queued+running')
  eq(tally.done, 1, '已回复')
  eq(tally.skipped, 1, '跳过与失败分开计（前者是护栏有意不跑）')
  eq(tally.failed, 1, '失败')
  eq(tally.total, 5, '本批 5 条（上一轮不混进来）')
  eq(tally.failureReason, 'boom', '失败原因取第一条 failed')
  eq(tally.detail.split('\n').length, 5, 'hover 明细一条投递一行')
  eq(conv.deliveryTallyText(tally), '已入队 3 · 已回复 1 · 未回复 2 · 失败 1 · 跳过 1', '状态条投递文案')
  eq(
    conv.deliveryTallyText(conv.deliveryTally({ deliveries: [{ target: 'agt_a', status: 'skipped', reason: 'pair_limit' }] })),
    '跳过 1',
    '全部被跳过时不写「已入队 0」',
  )
  eq(conv.deliveryTallyText(conv.deliveryTally(null)), '', '没有账本 → 空文案（空态不占位）')

  // I2：同一批按 conversation_id + round_id 聚合。被护栏跳过的目标**从来没有 turn**
  // （恒 0），只按 turn 比对会把它挤出本批 —— 状态条上「A 在跑」却看不到「B 被跳过」。
  const mixedBatch = conv.deliveryTally({ deliveries: [
    { target: 'agt_c', status: 'running', turn: 1, conversation_id: cid, round_id: 'rnd_0' }, // 上一批（账本旧→新）
    { target: 'agt_a', status: 'running', turn: 1, conversation_id: cid, round_id: 'rnd_1' },
    { target: 'agt_b', status: 'skipped', reason: 'pair_limit', turn: 0, conversation_id: cid, round_id: 'rnd_1' },
  ] }, names)
  eq(mixedBatch.total, 2, 'I2：A running(turn=1) + B skipped(turn=0) 必须同批')
  eq(mixedBatch.running, 1, 'I2：本批 running 1 条')
  eq(mixedBatch.skipped, 1, 'I2：被跳过的目标也要显示（不能只剩 running）')
  eq(mixedBatch.pending, 1, 'I2：未回复只算 queued+running（跳过不算）')
  eq(mixedBatch.detail.split('\n').length, 2, 'I2：hover 明细包含被跳过的那条')
  // round_id 为空（旧后端/人类首轮）→ 回退到 turn 比对。
  const noRoundBatch = conv.deliveryTally({ deliveries: [
    { target: 'agt_a', status: 'running', turn: 2, conversation_id: cid }, // 上一轮
    { target: 'agt_b', status: 'skipped', reason: 'pair_limit', turn: 1, conversation_id: cid },
    { target: 'agt_c', status: 'running', turn: 1, conversation_id: cid },
  ] }, names)
  eq(noRoundBatch.total, 2, 'I2：round_id 为空时回退 turn（同 turn 才算一批）')
  eq(noRoundBatch.skipped, 1, 'I2：回退路径同样看得见被跳过的目标')
  // 不同 round 绝不能混进同一批。
  const crossRound = conv.deliveryTally({ deliveries: [
    { target: 'agt_b', status: 'running', turn: 1, conversation_id: cid, round_id: 'rnd_8' },
    { target: 'agt_a', status: 'done', turn: 3, conversation_id: cid, round_id: 'rnd_9' },
  ] }, names)
  eq(crossRound.total, 1, 'I2：不同 round 不同批（只有最后一条 round）')

  // —— 未知原因码：状态条上说明「这是未知码」；单条明细仍保留原始码给排障 ——
  eq(conv.deliveryReasonInline('boom'), '未知原因：boom', '未知原因码有可读文案')
  eq(conv.deliveryReasonInline('pair_limit'), '互踢到上限，已跳过', '已知码照常翻译')
  eq(conv.deliveryReasonInline(''), '', '空原因不产生空括号')
  eq(conv.deliveryReasonText('boom'), 'boom', '单条明细保留原始码（可 grep）')

  // —— 空态：没有可说的就整条不渲染 ——
  eq(conv.conversationBarModel({ conversation: { status: 'idle' } }).visible, false, '空闲且无 run → 状态条不渲染')
  eq(conv.conversationBarModel({ conversation: null }).visible, false, '没有对话视图 → 不渲染')
  eq(conv.conversationBarModel({ conversation: { status: 'idle' }, active: true }).visible, true, '空闲但有 run 在跑 → 渲染')

  // —— 运行中：本轮/本链/队列/pair/投递 五段都在，没有空段 ——
  const running = conv.conversationBarModel({
    conversation: {
      status: 'running', turns_used: 3, turn_budget: 8, depth: 2, max_depth: 4, queue_len: 1,
      pair_limit: 10, pair_count: { 'agt_a↔agt_b': 4 },
      deliveries: [{ target: 'agt_a', status: 'done', turn: 1, conversation_id: cid }],
    },
    pairNames: names, now,
  })
  eq(running.visible, true, '运行中 → 渲染')
  eq(running.tone, 'running', 'running tone')
  eq(running.status, '自动对话中', '状态标签')
  eq(running.segments.map((s) => s.key).join(','), 'round,chain,queue,pair,delivery', '状态条显示本轮/本链/队列/pair/投递')
  eq(running.segments[0].text, '第 3/8 轮', '本轮')
  eq(running.segments[1].text, '本链 2/4', '本链（深度）')
  eq(running.segments[2].text, '队列 1', '队列')
  eq(running.segments[3].text, 'A ↔ B 4/10', 'pair 计数')
  eq(running.segments[4].text, '已入队 1 · 已回复 1', '投递（@ 成功 = 已入队）')
  assert(running.segments.every((s) => s.text.trim().length > 0), '没有空文案段（不留悬空分隔符）')
  eq(running.canStop, true, '有队列 → 可停')
  eq(running.canReset, false, '没到限 → 不给重置')
  // N4：后端没给 max_depth 时，分母与 Go 的 DefaultConversationMaxDepth（1000）对齐，
  // 不能显示「深度 2/4」这种假上限。
  const noMaxDepth = conv.conversationBarModel({ conversation: { status: 'running', depth: 2 } })
  eq(noMaxDepth.segments.find((s) => s.key === 'chain').text, '本链 2/1000', 'N4：缺省深度分母 = Go 默认 1000')

  // —— 护栏命中：pair 到限 + 恢复倒计时 + 重置门禁 + 终态 ——
  const waiting = conv.conversationBarModel({
    conversation: {
      status: 'waiting_human', turns_used: 2, depth: 4, max_depth: 4, queue_len: 0,
      pair_limit: 2, pair_count: { 'agt_a↔agt_b': 2 }, pair_reset_at: at(8 * 60_000),
    },
    pairNames: names, now,
  })
  eq(waiting.status, '等你继续', 'waiting_human 状态标签')
  eq(waiting.canReset, true, '到限 → 可重置本群限制')
  eq(waiting.resetTitle, conv.CONVERSATION_RESET_TITLE, '重置按钮说明与纯函数同源')
  const pairSeg = waiting.segments.find((s) => s.key === 'pair')
  eq(pairSeg.text, 'A ↔ B 2/2 · 约 8 分钟后自动恢复', 'pair 段带自动恢复倒计时')
  eq(pairSeg.tone, 'warn', 'pair 到限标黄')
  eq(waiting.segments.find((s) => s.key === 'chain').tone, 'warn', '深度到限标黄')
  eq(waiting.segments.find((s) => s.key === 'queue').tone, '', '队列为空不标黄')
  assert(waiting.segments.some((s) => s.key === 'terminal' && s.text === '队列已排空，等你继续'), '终态段')

  // —— 失败投递：段标红 + 一句话原因（未知码也读得懂） ——
  const failed = conv.conversationBarModel({
    conversation: { status: 'running', queue_len: 1, deliveries: [{ target: 'agt_a', status: 'failed', reason: 'boom', turn: 1, conversation_id: cid }] },
    pairNames: names, now,
  })
  const dseg = failed.segments.find((s) => s.key === 'delivery')
  eq(dseg.text, '失败 1（未知原因：boom）', '失败段带一句话原因')
  eq(dseg.tone, 'danger', '失败段标红')

  // —— 未知状态码：不显示裸码 ——
  const weird = conv.conversationBarModel({ conversation: { status: 'hibernating' } })
  eq(weird.status, '未知状态（hibernating）', '未知状态码有可读文案')
  eq(weird.tone, 'unknown', 'tone 仍能区分未知')
  assert(weird.statusTitle.includes('hibernating'), 'hover 说明保留原始码')
  eq(weird.visible, true, '未知状态也显示（不能静默消失）')
  eq(weird.segments.length, 0, '未知状态没有可用计数 → 不留空段')
}

// 4d) 状态条的接线（静态契约）：组件只画 model；群头不重复一份对话计数。
{
  const bar = readFileSync(join(APP, 'src', 'hub', 'ConversationBar.tsx'), 'utf8')
  assert(bar.includes('conversationBarModel') && bar.includes('hub-conv-facts'), '状态条只渲染 model（文案/门禁不在组件里）')
  assert(bar.includes('hub-conv-reset') && bar.includes('onReset'), '「重置本群限制」接在状态条上')
  assert(bar.includes('setInterval') && bar.includes('pair_reset_at'), 'pair 倒计时有心跳（否则那条字永远不动）')
  const group = readFileSync(join(APP, 'src', 'hub', 'GroupView.tsx'), 'utf8')
  assert(group.includes('<ConversationBar'), '群视图渲染状态条')
  assert(!group.includes('hub-conv-pair'), '群头不再重复 pair 文案（一套信息只有一个来源）')
  assert(!group.includes('conversationTurnText'), '群头不再重复轮次文案')
  assert(!group.includes('重置限制'), '重置入口只有状态条一处')
}

// 4e) 会话区配色统一到 app 令牌（P6-14）：静态契约。
// 真正的判据在 check-hub-theme-palette.mjs（getComputedStyle 三套调色板）；这里钉住
// 「颜色来源」本身，避免以后有人把 demo 的暖色值写回去、或者顺手把内容色也一起冲掉。
{
  const css = readFileSync(join(APP, 'src', 'hub', 'hub.css'), 'utf8')
  // 取「.hub-demo, .hub-demo[data-theme='light'] { ... }」这一块。
  const block = (css.match(/\.hub-demo,\n\.hub-demo\[data-theme='light'\] \{[\s\S]*?\n\}/) ?? [''])[0]
  assert(block.length > 0, '会话区令牌映射块存在（同时覆盖深/浅两套 demo 主题）')
  for (const [token, appToken] of [
    ['--chat', 'var(--paper-2)'],       // 与右栏同一个 token
    ['--surface', 'var(--paper-3)'],    // 卡面/输入框比底亮一档
    ['--border', 'var(--ink-line)'],
    ['--border-strong', 'var(--ink-line-2)'],
    ['--text', 'var(--ink)'],
    ['--muted', 'var(--ink-2)'],
    ['--muted-2', 'var(--ink-3)'],
  ]) {
    assert(new RegExp(`\\${token}:\\s*${appToken.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')};`).test(block),
      `${token} 接到 app 令牌 ${appToken}`)
  }
  assert(/--header-bg:\s*color-mix\(in srgb, var\(--paper-2\)/.test(block), 'header 底是同色半透明（不是另一套颜色）')
  // 内容色不能被「统一」掉：强调/用户气泡/语义色仍归 demo（会话内容自己的调色板）。
  assert(!/--accent:/.test(block) && !/--user-bubble:/.test(block), '不重接 --accent / --user-bubble（内容色不混进交互主色）')
  assert(!/--danger:|--warn:|--success:/.test(block), '不重接语义色')
  // header 的分隔线：demo 写死 rgba(255,255,255,.055)，浅色下几乎不可见 → 换成 app 发丝线。
  assert(/\.hub-demo \.chat-header \{ border-bottom-color: var\(--ink-line\); \}/.test(css), 'header 分隔线 = --ink-line')
  // 右栏底色真的等于 --paper-2（.right 的 --bg 会压掉同特指度的声明）。
  assert(/\.right\.hub-inspector,\n\.right\.hub-detail-rail \{ background: var\(--paper-2\); \}/.test(css),
    '右栏（展开/收起两态）底色 = --paper-2（提到 (0,2,0) 跟加载顺序解耦）')
  // 会话区/header/composer 三处都吃 --chat（不是只改背景色）。
  const demoCss = readFileSync(join(APP, 'src', 'hub', 'demo.css'), 'utf8')
  assert(/\.hub-demo \.main\{[^}]*background:var\(--chat\)/.test(demoCss), '会话区底 = --chat')
  assert(/\.hub-demo \.composer-wrap\{[^}]*background:var\(--chat\)/.test(demoCss), 'composer 外层 = --chat')
  assert(/\.hub-demo \.chat-header\{[^}]*background:var\(--header-bg\)/.test(demoCss), 'header 底 = --header-bg（同一个 token 源）')
}

// 4c) 按 agent 停止（P6-08）：谁还能「只停这一个」+ 超时/中断的原因文案。
{
  const conv = await load('conversation.ts')
  eq(conv.deliveryReasonText('run_timeout'), '单轮超时，已中断', '超时的原因文案')
  eq(conv.deliveryReasonText('interrupted'), '运行被中断', '被中断的原因文案')
  // 只有 running/queued 的 target 才给「停止」入口，终态的不给；同 target 去重。
  const ids = conv.stoppableAgentIds({
    deliveries: [
      { target: 'agt_a', status: 'running' },
      { target: 'agt_a', status: 'queued' },
      { target: 'agt_b', status: 'queued' },
      { target: 'agt_c', status: 'done' },
      { target: 'agt_d', status: 'skipped', reason: 'stale_conversation' },
      { target: '', status: 'running' },
    ],
  })
  eq(JSON.stringify(ids), JSON.stringify(['agt_a', 'agt_b']), '可停成员 = 进行中/排队的 target（去重、跳过终态）')
  eq(conv.stoppableAgentIds(null).length, 0, '没有对话 → 没有可停成员')
}

console.log(`\nHUB_CONV_TEST_PASS (${pass})`)
