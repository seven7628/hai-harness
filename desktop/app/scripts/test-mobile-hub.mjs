#!/usr/bin/env node
// 手机 Agent Hub 纯逻辑验收（Node 直跑，无需浏览器/构建）：
//   hub_model.js：seq 合并/去重、乐观气泡替换、delta 追加与迟到丢弃、
//                 @ 分词（code/URL/email 跳过）、候选过滤/光标插入/结构化 mentions、
//                 任务卡状态映射、事件分流（聊天 vs Hub）、resync 只回 Hub 快照。
// 同时做静态约束：页面不得使用 innerHTML/insertAdjacentHTML 渲染消息；
// hub resync 分支不得调用会话 openSession。
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const __dirname = dirname(fileURLToPath(import.meta.url))
const modelPath = join(__dirname, '..', '..', 'bridge', 'mobile_web', 'hub_model.js')
const appPath = join(__dirname, '..', '..', 'bridge', 'mobile_web', 'app.js')
const htmlPath = join(__dirname, '..', '..', 'bridge', 'mobile_web', 'index.html')

const require = createRequire(import.meta.url)
const H = require(modelPath)
const appSrc = readFileSync(appPath, 'utf8')
const modelSrc = readFileSync(modelPath, 'utf8')
const htmlSrc = readFileSync(htmlPath, 'utf8')

let pass = 0
const ok = (m) => {
  pass++
  console.log(`  ✅ ${m}`)
}
const assert = (cond, m) => {
  if (!cond) throw new Error(`断言失败: ${m}`)
  ok(m)
}
const eq = (got, want, m) => assert(got === want, `${m}（got=${JSON.stringify(got)} want=${JSON.stringify(want)}）`)

const roster = [
  { id: 'ag_alice', name: 'Alice', role: '研究', groupRole: 'owner' },
  { id: 'ag_alicechen', name: 'Alice Chen', role: '长文写作', groupRole: 'member' },
  { id: 'ag_bob', name: 'Bob', role: '数据', groupRole: 'member' },
  { id: 'ag_obs', name: 'Observer', role: '旁听', groupRole: 'observer' },
]

// —— 1) seq 合并：按 seq 升序、按 id/seq 去重 ——
{
  const a = { id: 'm2', seq: 2, kind: 'text', text: '二', sender: { kind: 'agent', agent_id: 'a' } }
  const b = { id: 'm1', seq: 1, kind: 'text', text: '一', sender: { kind: 'human' } }
  const out = H.mergeMessages([a], [b, { ...a }, { id: 'm3', seq: 3, kind: 'text', text: '三' }])
  eq(out.map((m) => m.seq).join(','), '1,2,3', '消息按 seq 升序')
  eq(out.length, 3, '同 id/seq 的重复事件被去掉')
  eq(out[1].text, '二', '重复帧不覆盖原正文')
  const noId = H.mergeMessages(
    [{ seq: 4, kind: 'text', text: '旧' }],
    [{ seq: 4, kind: 'text', text: '新' }],
  )
  eq(noId.length, 1, '没有 id 的消息按 seq 去重')
  eq(noId[0].text, '新', '同 seq 的迟到快照替换旧正文')
}

// —— 2) 乐观气泡：client_msg_id 命中后替换为服务端消息 ——
{
  const opt = H.optimisticMessage({ text: '你好', clientMsgId: 'c-1', senderKind: 'human', groupId: 'g1' })
  const pending = H.mergeMessages([], [opt])
  eq(pending.length, 1, '乐观消息先入列')
  eq(pending[0].pending, true, '乐观消息带 pending 标记')
  const server = {
    id: 'srv-1', seq: 7, kind: 'text', text: '你好',
    sender: { kind: 'human' }, refs: { client_msg_id: 'c-1' },
  }
  const out = H.mergeMessages(pending, [server])
  eq(out.length, 1, 'client_msg_id 命中后不新增第二条')
  eq(out[0].id, 'srv-1', '服务端 id 覆盖本地 id')
  eq(out[0].seq, 7, '服务端 seq 覆盖本地 0')
  eq(out[0].pending, undefined, '落盘后清掉 pending')
}

// —— 3) delta：index 追加、重复/迟到丢弃、final 后不再追加 ——
{
  const e0 = { message_id: 'draft-1', group_id: 'g1', agent_id: 'ag_bob', index: 0, delta: '你' }
  const e1 = { ...e0, index: 1, delta: '好' }
  let r = H.applyDelta([], e0, {})
  assert(r.applied, 'index=0 的 delta 追加为草稿')
  r = H.applyDelta(r.messages, e1, r.indexByID)
  assert(r.applied, 'index=1 继续追加')
  eq(r.messages[0].text, '你好', '草稿正文按 delta 顺序拼起来')
  const dup = H.applyDelta(r.messages, { ...e1, delta: '重复' }, r.indexByID)
  eq(dup.applied, false, '同一 index 的重复 delta 丢弃')
  const stale = H.applyDelta(r.messages, { ...e0, delta: '迟到' }, r.indexByID)
  eq(stale.applied, false, '小于已应用 index 的迟到 delta 丢弃')
  eq(stale.messages[0].text, '你好', '迟到 delta 不污染正文')

  const final = H.messageFromEvent({
    event_type: 'hub_message', group_id: 'g1', seq: 9, message_id: 'draft-1',
    message: { id: 'draft-1', seq: 9, kind: 'text', text: '你好！', sender: { kind: 'agent', agent_id: 'ag_bob' } },
  })
  const merged = H.mergeMessages(stale.messages, [final])
  eq(merged.length, 1, 'final 按 id 替换草稿，不重复')
  eq(merged[0].seq, 9, 'final 是权威 seq')
  assert(!merged[0].draft, 'final 后不再带 draft 标记')
  const late = H.applyDelta(merged, { ...e1, index: 2, delta: '？' }, dup.indexByID)
  eq(late.applied, false, 'final 之后到达的 delta 一律丢弃')
  eq(late.messages[0].text, '你好！', 'final 正文不被迟到 delta 改写')
}

// —— 4) @ 分词：最长名字优先；code/URL/email 不误判 ——
{
  const text = [
    '@Alice Chen 和 @Alice 看这里。',
    '行内 `@Alice` 不算。',
    '```',
    '@Alice 在围栏里也不算。',
    '```',
    '链接 https://example.com/@Alice 不算。',
    '邮箱 alice@example.com 也不算。',
    '最后 @Bob。',
  ].join('\n')
  const ids = H.mentionIds(text, roster)
  eq(ids.join(','), 'ag_alicechen,ag_alice,ag_bob', '只命中正文里真正点名的成员（最长名优先、去重）')
  const twice = H.mentionIds('@Alice @Alice', roster)
  eq(twice.join(','), 'ag_alice', '同一成员多次 @ 只发送一次')
  assert(!H.hasMentionAll('`@all`'), '代码里的 @all 不算 mention_all')
  assert(H.hasMentionAll('@所有人 开会'), '@所有人 产生 mention_all')
}

// —— 5) 全屏 @ 选择器：过滤、光标插入、结构化 payload ——
{
  const q = H.filterMentionCandidates('ali', roster).map((m) => m.name)
  assert(q.includes('Alice') && q.includes('Alice Chen'), '按名字过滤候选')
  assert(!q.includes('Observer'), 'query 不匹配的成员不出现')
  const all = H.filterMentionCandidates('all', roster, { includeAll: true })
  eq(all[0].id, H.ALL_MENTION_ID, 'includeAll 时 @所有人 排在首位')
  const deleted = H.filterMentionCandidates('', [{ id: 'x', name: 'X', deleted: true }])
  eq(deleted.length, 0, '已删除成员不出现在候选里')
  const gone = [{ id: 'ag_gone', name: 'Gone', deleted: true }]
  eq(H.mentionIds('@Gone 看下', gone).length, 0, '已删除成员不会被结构化 @ 发送')

  const applied = H.applyMention('请 @Al', 5, 'Alice')
  eq(applied.text, '请 @Alice ', '选中后把半截 @Al 替换成 @Alice + 空格')
  eq(applied.caret, 9, '光标落在空格之后')
  const onEmpty = H.applyMention('', 0, 'Bob')
  eq(onEmpty.text, '@Bob ', '无半截 token 时在光标处插入')
  eq(H.activeMentionQuery('请 @Al', 5).query, 'Al', '光标前识别 @ 查询')
  eq(H.activeMentionQuery('a@b', 3), null, 'email 形态不触发选择器')

  const p = H.groupSendPayload('g1', '@Alice Chen 和 @Alice 看下', roster, 'c-9')
  eq((p.mentions || []).join(','), 'ag_alicechen,ag_alice', '发送时带结构化 mentions:[agent_id]')
  eq(p.client_msg_id, 'c-9', '发送带 client_msg_id 幂等键')
  const allP = H.groupSendPayload('g1', '@所有人 开会', roster)
  eq(allP.mention_all, true, '@所有人 走 mention_all 字段')
  eq(allP.mentions, undefined, '@all 不混进结构化 mentions')
  const dm = H.dmSendPayload('ag_alice', '在吗')
  eq(dm.target.agent_id, 'ag_alice', 'DM payload 带 target.agent_id')
  const assign = H.assignPayload('g1', 'ag_bob', '做价格表', '标题')
  eq(assign.agent_id, 'ag_bob', '派任务 payload 带执行成员')
  eq(assign.instruction, '做价格表', '派任务 payload 带 instruction')
}

// —— 6) 任务结果卡：状态/耗时/用量/产物/错误映射 ——
{
  const msg = {
    kind: 'task_result', text: '价格表已生成',
    refs: {
      task_id: 'task-1', status: 'failed', title: '竞品价格', error: 'timeout',
      agent_id: 'ag_bob', origin_kind: 'agent', origin_agent_id: 'ag_alice', origin_agent_name: 'Alice',
      duration_ms: 15400,
      usage: { input: 1234, output: 567, cost_usd: 0.0123, cost_priced: true },
      artifacts: [{ name: 'prices.md', path: 'out/prices.md', kind: 'created', size: 2048 }],
    },
  }
  const view = H.taskCardView(msg)
  eq(view.statusLabel, '失败', 'failed → 失败')
  eq(view.tone, 'error', 'failed → error 色调')
  eq(view.terminal, true, 'failed 是终态')
  eq(view.durationLabel, '15.4s', 'duration_ms → 可读耗时')
  assert(view.usageLabel.includes('1.2k → 567 tok'), '用量按 token 展示')
  assert(view.usageLabel.includes('$0.01'), '计价后展示成本')
  eq(view.artifacts[0].name, 'prices.md', '产物名映射')
  eq(view.error, 'timeout', '错误文本映射')
  eq(view.initiatorName, 'Alice', '派发者取账本 origin，不从 sender 猜')

  const running = H.taskStatusView('running')
  eq(running.terminal, false, 'running 不是终态')
  eq(H.taskStatusView('completed').tone, 'success', 'completed → success 色调')
  eq(H.taskStatusView('interrupted').tone, 'warn', 'interrupted → warn 色调')

  const patched = H.applyTaskUpdate([msg], { id: 'task-1', status: 'completed', error: '' })
  eq(patched[0].refs.status, 'completed', 'hub_task_update 刷新卡状态')
  const noop = H.applyTaskUpdate([msg], { id: 'other', status: 'completed' })
  assert(noop === [msg] || noop[0] === msg, '非本任务的事件不改卡')

  // 模型正文/错误可能带 HTML：模型层原样保留，页面用 textContent 渲染，不转义也不拼标签。
  const xssMsg = {
    kind: 'task_result', text: '<img src=x onerror=alert(1)>',
    refs: { task_id: 't', status: 'completed', summary: '<script>alert(1)</script>' },
  }
  const xss = H.taskCardView(xssMsg)
  eq(xss.summary, '<script>alert(1)</script>', '不可信文本原样返回，交给 textContent')
}

// —— 7) 事件分流：Hub 帧不进聊天处理，聊天帧不进 Hub 处理 ——
{
  const hubMsg = { event_type: 'hub_message', group_id: 'g1' }
  const hubDelta = { event_type: 'hub_message_delta', group_id: 'g1' }
  eq(H.eventRoute(hubMsg, 'chat'), 'ignore', 'hub_message 不进入聊天处理器')
  eq(H.eventRoute(hubDelta, 'chat'), 'ignore', 'hub_message_delta 不进入聊天处理器')
  eq(H.eventRoute(hubMsg, 'hub'), 'hub', 'hub_message 由 Hub 处理器消费')
  eq(H.eventRoute({ event_type: 'content_chunk' }, 'hub'), 'ignore', '会话 chunk 不进入 Hub 处理器')
  eq(H.eventRoute({ type: 'resync' }, 'hub'), 'hub', 'Hub resync 服务端帧由 Hub 处理器消费')
  eq(H.eventRoute({ type: 'hello', scope: 'hub' }, 'hub'), 'hub', 'Hub hello 由 Hub 处理器消费')

  const groupPlan = H.hubResyncTargets('hub-group', { groupId: 'g1', agentId: '' })
  eq(groupPlan.home, true, 'resync 总是重拉 Hub 首页快照')
  eq(groupPlan.groupId, 'g1', '群时间线 resync 只回群 id')
  eq(groupPlan.agentId, '', '群时间线 resync 不带 DM 目标')
  assert(!JSON.stringify(groupPlan).includes('session'), 'resync 计划不包含会话 sid，避免误走 openSession')
  const dmPlan = H.hubResyncTargets('hub-dm', { agentId: 'ag_alice' })
  eq(dmPlan.agentId, 'ag_alice', 'DM resync 回 agent_id')
  eq(H.hubResyncTargets('hub', {}).groupId, '', '首页 resync 不打开时间线')
}

// —— 8) 系统行/Needs 问题解析 ——
{
  const names = { ag_bob: 'Bob', ag_alice: 'Alice' }
  eq(H.routingSummary('targets=ag_bob,ag_alice', names), '唤醒 Bob、Alice', 'routing_receipt 还原成人话')
  eq(H.routingSummary('@all targets=ag_bob dropped=1', names), '@all 唤醒 Bob（丢弃 1 个）', 'routing_receipt 处理 @all/丢弃数')
  const need = {
    kind: 'question', body: JSON.stringify([{ id: 'q1', question: '选哪个' }, { text: '补充' }]),
  }
  const qs = H.parseNeedQuestions(need)
  eq(qs.length, 2, '解析 question Need 的问题列表')
  eq(qs[0].question, '选哪个', '问题正文保留')
  eq(H.hubPreview({ last_outcome: '任务完成', last_text: '旧预览' }), '任务完成', '左栏预览优先任务结论')
}

// —— 9) 静态约束：不拼 HTML；resync 不碰会话；聊天/Hub 分流都在 ——
{
  for (const [name, src] of [['hub_model.js', modelSrc], ['app.js', appSrc]]) {
    assert(!/\.innerHTML\b/.test(src), `${name} 不使用 innerHTML`)
    assert(!/insertAdjacentHTML|outerHTML|document\.write\s*\(/.test(src), `${name} 不使用其它 HTML 注入 API`)
  }
  assert(/<script src="hub_model.js"><\/script>/.test(htmlSrc), 'index.html 先加载 hub_model.js')
  assert(/<script src="app.js"><\/script>/.test(htmlSrc), 'index.html 加载 app.js')
  assert(/scope=hub/.test(appSrc), 'app.js 打开 scope=hub 的独立 WebSocket')
  assert(/H\.hubResyncTargets\(/.test(appSrc), 'app.js 的 resync 走 Hub 快照计划')
  assert(/H\.groupSendPayload\(/.test(appSrc), '群发送走带 mentions 的 payload 构造')
  assert(/H\.dmSendPayload\(/.test(appSrc), 'DM 发送走 hub_send payload 构造')
  for (const cmd of [
    'hub_agent_list', 'hub_group_list', 'hub_group_get', 'hub_group_history',
    'hub_open', 'hub_history', 'hub_send', 'hub_group_send', 'hub_group_assign',
    'hub_conversation_stop', 'hub_task_list', 'hub_need_list', 'hub_need_resolve',
    'hub_mark_read', 'hub_task_cancel',
  ]) {
    assert(appSrc.includes(`'${cmd}'`), `app.js 使用 ${cmd}`)
  }
  const resyncStart = appSrc.indexOf('async function hubResync')
  const resyncEnd = appSrc.indexOf('function onHubEvent', resyncStart)
  assert(resyncStart > 0 && resyncEnd > resyncStart, '找到 Hub resync 实现')
  const resyncSrc = appSrc.slice(resyncStart, resyncEnd)
  assert(!resyncSrc.includes('openSession'), 'Hub resync 分支不调用 openSession(state.sid)')
  const chatStart = appSrc.indexOf('function onEvent(ev)')
  const chatSrc = appSrc.slice(chatStart, chatStart + 600)
  assert(chatSrc.includes("H.eventRoute(ev, 'chat')"), '聊天处理器先过滤 Hub 事件')
  assert((appSrc.match(/eventRoute\(/g) || []).length >= 2, '聊天与 Hub 两条路径都经过 eventRoute')
}

console.log(`\n手机 Hub 纯逻辑验收通过：${pass} 项`)
