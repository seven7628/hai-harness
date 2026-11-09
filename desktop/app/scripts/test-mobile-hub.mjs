#!/usr/bin/env node
// 手机 Agent Hub 纯逻辑验收（Node 直跑，无需浏览器/构建）：
//   hub_model.js：seq 合并/去重、乐观气泡替换、delta 追加与迟到丢弃、
//                 @ 分词（code/URL/email 跳过）、候选过滤/光标插入/结构化 mentions、
//                 任务卡状态映射、事件分流（聊天 vs Hub）、resync 只回 Hub 快照。
// 同时做静态约束：页面不得使用 innerHTML/insertAdjacentHTML 渲染消息；
// hub resync 分支不得调用会话 openSession。
// P4-03c 追加：键入 @、发送失败保草稿、hub_alert 去重、定稿 mdLite（安全 DOM）、
//              贴底+可见的已读守卫、主题 system/ink/paper、44px/reduced-motion。
// P4-03d 追加：失败草稿的「重试」必须复用首次 client_msg_id（幂等槽）：同一
//              (正文, 目标) 复用、成功清空、改草稿/换目标换新；已落盘的消息不再
//              插乐观气泡（否则 seq=0 会把回执盖回「发送中」）。
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { auditMobileWeb } from './mobile-web-audit.mjs'

const __dirname = dirname(fileURLToPath(import.meta.url))
const modelPath = join(__dirname, '..', '..', 'bridge', 'mobile_web', 'hub_model.js')
const appPath = join(__dirname, '..', '..', 'bridge', 'mobile_web', 'app.js')
const htmlPath = join(__dirname, '..', '..', 'bridge', 'mobile_web', 'index.html')
const cssPath = join(__dirname, '..', '..', 'bridge', 'mobile_web', 'style.css')
const tokensPath = join(__dirname, '..', '..', 'bridge', 'mobile_web', 'tokens.css')

const require = createRequire(import.meta.url)
const H = require(modelPath)
const appSrc = readFileSync(appPath, 'utf8')
const modelSrc = readFileSync(modelPath, 'utf8')
const htmlSrc = readFileSync(htmlPath, 'utf8')
const cssSrc = readFileSync(cssPath, 'utf8')
const tokensSrc = readFileSync(tokensPath, 'utf8')

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


// —— 10) N4：键入 @ 即时唤起（含 IME 组词安全） ——
{
  const typed = H.mentionTypingQuery('请 @Al', 5, { composing: false })
  eq(typed.query, 'Al', '键入 @ 后返回查询词')
  eq(typed.start, 2, '返回 @ 起点，插入时可整体替换半截 token')
  eq(H.mentionTypingQuery('请 @Al', 5, { composing: true }), null, 'IME 组词中不弹选择器')
  eq(H.activeMentionQuery('@', 1).query, '', '只敲 @ 也触发（空查询列出全部候选）')
  eq(H.mentionTypingQuery('邮箱 a@b', 6), null, 'email 形态不触发')
  const applied = H.applyMention('请 @Al 明天', 5, 'Alice')
  eq(applied.text, '请 @Alice 明天', '选中提及保留 @ 后面的正文')
  assert(/H\.mentionTypingQuery\(/.test(appSrc), 'app.js composer input 调用 mentionTypingQuery')
  assert(/compositionstart/.test(appSrc) && /compositionend/.test(appSrc), 'app.js 有 IME composition 安全')
  assert(/openMentionPicker\(q\.query\)/.test(appSrc), 'app.js 用键入的 query 预填选择器')
  assert(/function mentionFromComposer/.test(appSrc), 'app.js 把 input 事件接到选择器')
}

// —— 11) N5：发送失败不丢草稿（成功才清空） ——
{
  eq(H.draftAfterFailure('', '你好 @Bob'), '你好 @Bob', '失败且输入框为空时恢复整段草稿')
  eq(H.draftAfterFailure('   ', '你好 @Bob'), '你好 @Bob', '空白输入框也恢复草稿')
  eq(H.draftAfterFailure('发送期间新敲的', '旧草稿'), '发送期间新敲的', '失败不覆盖用户新输入')
  eq(H.draftAfterSend('你好 @Bob', '你好 @Bob'), '', '成功才清空已发送正文')
  eq(H.draftAfterSend('你好 @Bob，补充', '你好 @Bob'), '你好 @Bob，补充', '成功不清发送期间新输入')
  eq(H.draftAfterSend(' 你好 ', '你好'), '', '成功比较忽略首尾空白')
  assert(/H\.draftAfterSend\(/.test(appSrc), 'app.js 成功路径走 draftAfterSend')
  assert(/H\.draftAfterFailure\(/.test(appSrc), 'app.js 失败路径走 draftAfterFailure')
  assert(/appendHubSendError/.test(appSrc) && /重试/.test(appSrc), 'app.js 失败后有重试入口')
  const gStart = appSrc.indexOf('async function sendHubGroup')
  const gEnd = appSrc.indexOf('async function stopHubConversation', gStart)
  const gBody = gStart >= 0 && gEnd > gStart ? appSrc.slice(gStart, gEnd) : ''
  assert(gBody.length > 0, '找到 sendHubGroup 实现')
  assert(!/input\.value\s*=\s*''/.test(gBody), 'sendHubGroup 不在请求前清空输入框')
  const dStart = appSrc.indexOf('async function sendHubDM')
  const dEnd = appSrc.indexOf('// ---- Hub 事件流', dStart)
  const dBody = dStart >= 0 && dEnd > dStart ? appSrc.slice(dStart, dEnd) : ''
  assert(dBody.length > 0, '找到 sendHubDM 实现')
  assert(!/input\.value\s*=\s*''/.test(dBody), 'sendHubDM 不在请求前清空输入框')
}

// —— 12) N7：hub_alert 去重键与存储文本同源 ——
{
  eq(H.hubAlertText({}), 'Hub 告警', 'code/message 都为空时文本固定')
  eq(H.hubAlertText({ message: '', code: '' }), 'Hub 告警', '空字符串与 undefined 同口径')
  eq(H.hubAlertText({ code: 'settle_failed' }), 'settle_failed', '无 message 时用 code')
  eq(H.hubAlertText({ message: '磁盘满', code: 'settle_failed' }), '磁盘满', '有 message 时优先 message')
  let alerts = H.pushHubAlert([], {})
  alerts = H.pushHubAlert(alerts, {})
  eq(alerts.length, 1, '两条空告警只留一条')
  eq(alerts[0].text, 'Hub 告警', '存储文本就是去重键')
  alerts = H.pushHubAlert(alerts, {})
  eq(alerts.length, 1, 'code-less/message-less 连续告警仍只留一条')
  alerts = H.pushHubAlert(alerts, { message: '磁盘满', severity: 'error' })
  eq(alerts.length, 2, '不同告警各自保留')
  eq(alerts[0].severity, 'error', '告警保留 severity')
  alerts = H.pushHubAlert(alerts, { message: '磁盘满' })
  eq(alerts.length, 2, '同 message 重复告警只留一条')
  assert(/H\.pushHubAlert\(/.test(appSrc), 'app.js 走 pushHubAlert 统一去重')
  assert(!/case 'hub_group_update'/.test(appSrc), '死亡的 hub_group_update 分支已删除')
}

// —— 13) N9：定稿正文走安全 mdLite（DOM 计划，无 HTML 字符串） ——
{
  const plan = H.markdownPlan('**粗** 和 `code`\n```js\nconst x = 1\n```')
  eq(plan.map((p) => p.kind).join(','), 'bold,text,code,text,codeblock', 'markdown 计划覆盖粗体/行内代码/围栏')
  eq(plan.find((p) => p.kind === 'codeblock').text, 'const x = 1', '围栏丢掉语言标注')
  const html = '<img src=x onerror=alert(1)> **粗** @Alice'
  const parts = H.bodyParts(html, roster)
  assert(parts.every((p) => ['text', 'mention', 'bold', 'code', 'codeblock'].includes(p.kind)), '正文只产出安全节点描述')
  assert(parts.some((p) => p.kind === 'text' && p.text.includes('<img src=x onerror=alert(1)>')), 'HTML 原样作为文本节点')
  assert(!JSON.stringify(parts).includes('<b>'), '计划里不出现 HTML 标签')
  assert(parts.some((p) => p.kind === 'mention' && p.name === 'Alice'), '定稿正文仍高亮 @ 成员')
  const inCode = H.bodyParts('`@Alice`', roster)
  assert(!inCode.some((p) => p.kind === 'mention'), '行内代码里的 @ 不误标成员')
  const inFence = H.bodyParts('```\n@Alice\n```', roster)
  assert(!inFence.some((p) => p.kind === 'mention'), '围栏里的 @ 不误标成员')
  assert(/H\.bodyParts\(/.test(appSrc), 'app.js 用 bodyParts 渲染定稿正文')
  assert(/function appendHubBody\(/.test(appSrc), 'app.js 有 appendHubBody')
  assert(/appendHubBody\(body, m\.text/.test(appSrc), 'renderHubMessage 接 appendHubBody')
  const draftAt = appSrc.indexOf('function renderHubDraft')
  const draftEnd = appSrc.indexOf('function renderHubGroupStatus', draftAt)
  const draftSrc = draftAt >= 0 && draftEnd > draftAt ? appSrc.slice(draftAt, draftEnd) : ''
  assert(/body\.textContent = d\.text/.test(draftSrc), '流式草稿仍是 textContent 纯文本')
  assert(!/innerHTML|insertAdjacentHTML|document\.write/.test(draftSrc), '流式草稿不拼 HTML')
}

// —— 14) N8：已读游标只在下沉底部且页面可见时推进 ——
{
  const bottom = { scrollTop: 500, scrollHeight: 1000, clientHeight: 500 }
  assert(H.shouldMarkRead(bottom, true), '贴底且可见 → 推进已读')
  assert(H.shouldMarkRead({ scrollTop: 460, scrollHeight: 1000, clientHeight: 500 }, true), '距底 40px 内算贴底')
  assert(!H.shouldMarkRead({ scrollTop: 450, scrollHeight: 1000, clientHeight: 500 }, true), '距底 50px 不推进（正在读历史）')
  assert(!H.shouldMarkRead(bottom, false), '页面不可见 → 不推进')
  assert(!H.shouldMarkRead(null, true), '没有时间线 → 不推进')
  assert(!H.shouldMarkRead({ scrollTop: 0, scrollHeight: 0, clientHeight: 0 }, true), '空时间线 → 不推进')
  assert(/H\.shouldMarkRead\(/.test(appSrc), 'app.js 用 shouldMarkRead 守卫')
  assert(/if \(!atReadBottom\('hub-group-messages'\)\) return/.test(appSrc), '群已读在贴底/可见检查后才推进')
  assert(/if \(!atReadBottom\('hub-dm-messages'\)\) return/.test(appSrc), 'DM 已读在贴底/可见检查后才推进')
  assert(/document\.hidden/.test(appSrc), 'app.js 检查 document.hidden')
  assert(/visibilitychange/.test(appSrc), 'app.js 在 visibilitychange 重评估')
  assert(/addEventListener\('scroll', scheduleHubReadCheck/.test(appSrc), 'app.js 在时间线滚动时重评估')
  assert(/mark_read:\s*false/.test(appSrc), 'hub_open 打开时不无条件推进已读')
}

// —— 15) 主题：system / ink / paper 解析与持久化 ——
{
  eq(H.normalizeThemeMode(undefined), 'system', '缺省主题模式 = system')
  eq(H.normalizeThemeMode('PAPER'), 'paper', '主题模式大小写归一')
  eq(H.normalizeThemeMode('bogus'), 'system', '未知主题模式回落 system')
  eq(H.resolveTheme('system', false), 'ink', 'system + 深色系统 → 墨')
  eq(H.resolveTheme('system', true), 'paper', 'system + 浅色系统 → 宣纸')
  eq(H.resolveTheme('ink', true), 'ink', '手动墨不被浅色系统覆盖')
  eq(H.resolveTheme('paper', false), 'paper', '手动宣纸不被深色系统覆盖')
  eq(H.resolveTheme('bogus', true), 'paper', '未知模式按 system 解析')
  eq(H.themeColor('ink'), '#1a1614', '墨的 theme-color 来自桌面 token')
  eq(H.themeColor('paper'), '#fdfcfb', '宣纸的 theme-color 来自桌面 token')
  assert(tokensSrc.toLowerCase().includes('#1a1614') && tokensSrc.toLowerCase().includes('#fdfcfb'), 'token 里存在两种底色')
  assert(/hai\.theme/.test(appSrc), '主题持久化在 hai.theme')
  assert(/documentElement\.dataset\.theme/.test(appSrc), '主题通过 documentElement.dataset.theme 应用')
  assert(/prefers-color-scheme/.test(appSrc), '主题读取系统 prefers-color-scheme')
  assert(/themeMode === 'system'/.test(appSrc), '只有 system 模式响应系统外观变化')
  assert(/meta\[name="theme-color"\]/.test(appSrc), '主题切换更新 meta theme-color')
  assert(/<meta name="theme-color" content="#1a1614">/.test(htmlSrc), 'index.html 初始 theme-color 来自 token')
  for (const mode of ['system', 'ink', 'paper']) {
    assert(htmlSrc.includes(`data-theme-mode="${mode}"`), `index.html 有 ${mode} 选项`)
  }
  assert(/id="theme"/.test(htmlSrc) && /id="theme-sheet"/.test(htmlSrc), '主题开关与面板都在顶栏可直达')
}

// —— 16) 静态审计：44px / reduced-motion / XSS / 390 ——
{
  assert(/@media \(prefers-reduced-motion: reduce\)/.test(cssSrc), 'style.css 有 reduced-motion 媒体查询')
  assert(/\.hub-chip\s*\{[^}]*min-height:\s*44px/.test(cssSrc), 'hub-chip 触控高度 44px')
  assert(/\.hub-sheet-actions button\s*\{[^}]*min-height:\s*44px/.test(cssSrc), 'hub-sheet-actions 触控高度 44px')
  assert(/\.icon\s*\{[^}]*width:\s*44px[^}]*height:\s*44px/.test(cssSrc), '图标按钮 44x44')
  const audit = auditMobileWeb()
  if (audit.errors.length) for (const e of audit.errors) console.error(`  ❌ ${e}`)
  assert(audit.errors.length === 0, `静态审计通过（${audit.checks} 条 CSS 规则）`)
}

// —— 17) P4-03d：失败草稿重试复用 client_msg_id（发送幂等） ——
// 症状：手动「重试」如果换新 cid，而首次请求其实已经落盘（只是响应丢了），
// 服务端会把同一段正文写成两条。槽（slot）语义：同一 (正文, 目标) 复用，
// 成功清空，用户改草稿/换目标换新 —— 由 app.js 在执行层落实，模型层可单测。
{
  const gid = 'grp_1'
  let n = 0
  const gen = () => 'g-' + ++n
  const noNew = () => {
    throw new Error('复用路径不该生成新 cid')
  }

  const first = H.sendSlotID(null, '  你好 @Bob  ', gid, gen)
  eq(first.text, '你好 @Bob', '槽正文去首尾空白')
  eq(first.target, gid, '槽记录目标（群/DM 不串）')
  eq(first.cid, 'g-1', '首次发送生成 cid')

  const retry = H.sendSlotID(first, '你好 @Bob', gid, noNew)
  eq(retry.cid, 'g-1', '同一失败草稿重试复用首次 client_msg_id')
  eq(retry.text, first.text, '重试槽正文不变')
  eq(H.sendSlotID(first, ' 你好 @Bob ', gid, noNew).cid, 'g-1', '首尾空白差异不算新草稿')
  eq(H.sendSlotID(retry, '你好 @Bob', gid, noNew).cid, 'g-1', '重试再次失败后 cid 仍保留')

  eq(H.sendSlotAfterEdit(retry, '你好 @Bob 补充'), null, '用户改了草稿 → 槽失效（换新 cid）')
  eq(H.sendSlotAfterEdit(retry, ' 你好 @Bob '), retry, '草稿没变 → 槽保留')
  eq(H.sendSlotAfterEdit(null, '任意'), null, '没有槽时返回 null')
  const edited = H.sendSlotID(H.sendSlotAfterEdit(retry, '你好 @Bob 补充'), '你好 @Bob 补充', gid, gen)
  assert(edited.cid !== 'g-1', '编辑后发送生成新 cid')

  const otherGroup = H.sendSlotID(first, '你好 @Bob', 'grp_2', gen)
  assert(otherGroup.cid !== 'g-1', '换群不复用 cid（否则被当成另一条消息的重放）')
  const dmSlot = H.sendSlotID(null, '你好 @Bob', 'ag_alice', gen)
  assert(dmSlot.cid !== first.cid && dmSlot.cid !== otherGroup.cid, 'DM 槽独立于群槽')

  // 成功清空：调用方置 null，下一次发送必须是新 cid。
  const afterSuccess = H.sendSlotID(null, '你好 @Bob', gid, gen)
  assert(afterSuccess.cid !== 'g-1', '成功清空后下一次发送换新 cid')

  // 端到端模型：服务端按 cid 幂等 + 客户端乐观气泡按 cid 去重。
  const server = {
    log: new Map(),
    write(cid, text) {
      if (this.log.has(cid)) {
        const hit = this.log.get(cid)
        return { seq: hit.seq, message_id: hit.id, deduplicated: true }
      }
      const msg = {
        id: 'srv-' + (this.log.size + 1),
        seq: this.log.size + 1,
        client_msg_id: cid,
        group_id: gid,
        kind: 'text',
        sender: { kind: 'human' },
        text,
      }
      this.log.set(cid, msg)
      return { seq: msg.seq, message_id: msg.id }
    },
  }
  const sendOnce = (state, text, loseResponse) => {
    const slot = H.sendSlotID(state.slot, text, gid, gen)
    let messages = state.messages
    if (!H.hasLandedClientMsg(messages, slot.cid)) {
      messages = H.mergeMessages(
        messages,
        H.optimisticMessage({ text, clientMsgId: slot.cid, senderKind: 'human', groupId: gid }),
      )
    }
    if (loseResponse) {
      server.write(slot.cid, text) // 服务端写成功，响应在路上丢了
      return {
        slot,
        messages: messages.filter((m) => !(m && m.pending && H.clientMsgID(m) === slot.cid)),
      }
    }
    server.write(slot.cid, text)
    messages = H.mergeMessages(messages, Object.assign({}, server.log.get(slot.cid)))
    return { slot: null, messages }
  }

  let st = sendOnce({ slot: null, messages: [] }, '价格表结论', true)
  eq(server.log.size, 1, '首次请求其实已经落盘')
  eq(st.messages.length, 0, '失败时乐观气泡撤掉（草稿留在输入框）')
  const lostCid = st.slot && st.slot.cid
  assert(Boolean(lostCid), '失败后槽保留 cid（重试才有东西可复用）')

  st = sendOnce(st, '价格表结论', false)
  eq(st.slot, null, '成功之后槽清空')
  eq(server.log.size, 1, '重试复用 cid → 服务端只写一条')
  eq(st.messages.length, 1, '客户端时间线只有一条（乐观气泡按 cid 替换）')
  eq(st.messages[0].seq, 1, '回执把乐观气泡替换成服务端消息')
  assert(!st.messages[0].pending, '定稿后没有 pending 标记')
  eq(H.clientMsgID(st.messages[0]), lostCid, '落盘消息带着首次的 client_msg_id')

  st = sendOnce(st, '第二条', false)
  eq(server.log.size, 2, '成功之后再发是一条新消息')
  eq(st.messages.length, 2, '新消息正常追加')
  assert(!H.hasLandedClientMsg([{ client_msg_id: 'x', seq: 0 }], 'x'), '未落盘（seq=0）不算 landed')
  assert(H.hasLandedClientMsg([{ client_msg_id: 'x', seq: 3 }], 'x'), 'seq>0 视为已落盘')
  assert(H.hasLandedClientMsg([{ refs: { client_msg_id: 'x' }, seq: 3 }], 'x'), 'refs.client_msg_id 也认')
  assert(!H.hasLandedClientMsg([], 'x'), '空列表不 landed')

  // 编辑后重试：同一条失败草稿被改了正文 → 新 cid、新消息。
  const third = sendOnce({ slot: null, messages: st.messages }, '第三条', true)
  const edited3 = H.sendSlotAfterEdit(third.slot, '第三条 补充')
  eq(edited3, null, '编辑失败草稿后槽失效')
  const fourth = sendOnce({ slot: edited3, messages: third.messages }, '第三条 补充', false)
  eq(fourth.slot, null, '编辑后的发送成功后清空')
  eq(server.log.size, 4, '编辑后发送是一条新消息（不是重放）')

  // —— app.js 执行层接线（模型正确但如果没接上等于没修） ——
  const gStart = appSrc.indexOf('async function sendHubGroup')
  const gEnd = appSrc.indexOf('function appendHubGroupError', gStart)
  const gBody = gStart >= 0 && gEnd > gStart ? appSrc.slice(gStart, gEnd) : ''
  assert(gBody.length > 0, '找到 sendHubGroup 实现')
  assert(/const slot = H\.sendSlotID\(state\.hub\.pendingGroupSend/.test(gBody), '群发送用 pendingGroupSend 取 cid')
  assert(/state\.hub\.pendingGroupSend = slot/.test(gBody), '群发送把槽记回 state（失败后重试才能复用）')
  assert(/H\.hasLandedClientMsg\(state\.hub\.groupMessages, cid\)/.test(gBody), '群发送用 hasLandedClientMsg 防乐观气泡覆盖已落盘消息')
  const gCatchAt = gBody.indexOf('} catch (e) {')
  const gFinallyAt = gBody.indexOf('} finally {')
  const gCatch = gCatchAt >= 0 && gFinallyAt > gCatchAt ? gBody.slice(gCatchAt, gFinallyAt) : ''
  assert(gCatch.length > 0, '找到 sendHubGroup 的失败分支')
  assert(/pendingGroupSend = null/.test(gBody.slice(0, gCatchAt)), '群发送成功后清空幂等槽')
  assert(!/pendingGroupSend = null/.test(gCatch), '失败分支不清空幂等槽（重试复用）')

  const dStart = appSrc.indexOf('async function sendHubDM')
  const dEnd = appSrc.indexOf('// ---- Hub 事件流', dStart)
  const dBody = dStart >= 0 && dEnd > dStart ? appSrc.slice(dStart, dEnd) : ''
  assert(dBody.length > 0, '找到 sendHubDM 实现')
  assert(/const slot = H\.sendSlotID\(state\.hub\.pendingDMSend/.test(dBody), 'DM 发送用 pendingDMSend 取 cid')
  assert(/state\.hub\.pendingDMSend = slot/.test(dBody), 'DM 发送把槽记回 state')
  assert(/H\.hasLandedClientMsg\(state\.hub\.dmMessages, cid\)/.test(dBody), 'DM 发送用 hasLandedClientMsg')
  assert(/state\.hub\.pendingDMSend = null/.test(dBody), 'DM 成功后清空幂等槽')
  assert(
    /state\.hub\.pendingGroupSend = H\.sendSlotAfterEdit\(state\.hub\.pendingGroupSend, t\.value\)/.test(appSrc),
    '键入修改群草稿后清空幂等槽',
  )
  assert(
    /state\.hub\.pendingDMSend = H\.sendSlotAfterEdit\(state\.hub\.pendingDMSend, t\.value\)/.test(appSrc),
    '键入修改 DM 草稿后清空幂等槽',
  )
  assert(
    /state\.hub\.pendingGroupSend = H\.sendSlotAfterEdit\(state\.hub\.pendingGroupSend, input\.value\)/.test(appSrc),
    '插入 @ 改草稿后清空群幂等槽',
  )
}

console.log(`\n手机 Hub 纯逻辑验收通过：${pass} 项`)
