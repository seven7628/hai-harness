#!/usr/bin/env node
// P3b-07/08 前端 hub 纯逻辑验收（Node 直跑，无需浏览器）：
//   mentions：最长名字优先、fenced/inline code/URL/email 跳过、光标插入、选择器过滤；
//   conversation：轮次/深度/队列文案、终态文案、停止按钮门禁、左栏预览回退、相对时间；
//   taskCard：task_result 卡的 completed/failed/中断/拒绝文案、产物/耗时/成本、左栏结论行；
//   groupProjection：未读/预览/任务更新/已读/竞态守卫（群 + DM 两套口径）；
//   store：Hub 关闭时未读、打开时已读、hub_read seq<=0、迟到任务更新不复活结论、
//          hub_agent_list 在飞时到达的 DM 消息不被旧快照覆盖（P3b-08）；
//   mock：停止结算在飞任务（interrupted），二次 assign 后停止按钮不被僵尸任务点亮。
// 这些是「@ 路由 / 自动对话 / 任务卡 / 左栏未读」在 UI 侧的唯一分叉点，值得独立回归。
import { build } from 'esbuild'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const hubDir = join(__dirname, '..', 'src', 'hub')
// 一个虚拟入口同时导出五个模块：esbuild 只跑一次，导入一个 data URL 即可。
const result = await build({
  stdin: {
    contents: [
      "export * as mentions from './mentions.ts'",
      "export * as conversation from './conversation.ts'",
      "export * as taskCard from './taskCard.ts'",
      "export * as groupProjection from './groupProjection.ts'",
      "export * as perfFlags from './perfFlags.ts'",
      "export * as store from './useHubStore.ts'",
      "export * as transport from '../transport/index.ts'",
    ].join('\n'),
    resolveDir: hubDir,
    sourcefile: 'hub-p3b-entry.ts',
    loader: 'ts',
  },
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
  // useHubStore 会把 mock transport 一起拉进来；Vite 的 ?raw/?url/?no-inline 在
  // esbuild 里没有对应 loader，给它们一个空模块（本测试不渲染品牌图）。
  plugins: [{
    name: 'stub-vite-assets',
    setup(build) {
      build.onResolve({ filter: /\?(raw|url|no-inline)$/ }, (args) => ({ path: args.path, namespace: 'vite-asset' }))
      build.onLoad({ filter: /.*/, namespace: 'vite-asset' }, () => ({ contents: 'export default ""', loader: 'js' }))
    },
  }],
})
const source = result.outputFiles[0].text
const mod = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)
const { mentions, conversation, taskCard, groupProjection, store, transport } = mod

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const eq = (got, want, m) => assert(got === want, `${m}（got=${JSON.stringify(got)} want=${JSON.stringify(want)}）`)

const roster = [
  { id: 'ag_alice', name: 'Alice', role: '研究', groupRole: 'owner', accent: '#7bc199' },
  { id: 'ag_alicechen', name: 'Alice Chen', role: '长文写作', groupRole: 'member', accent: '#9daed2' },
  { id: 'ag_critic', name: 'Critic', role: '反例审查', groupRole: 'member' },
]

// —— 1) mentions：最长名字优先 ——
{
  const ids = mentions.mentionIds('@Alice Chen 先写，@Alice 补数据，@alice 收尾', roster)
  eq(ids.join(','), 'ag_alicechen,ag_alice', '最长成员名优先（Alice Chen 不被拆成 Alice + Chen）')
  const link = mentions.linkifyMentions('@Alice Chen 先写', roster)
  eq(link, '[@Alice Chen](#mention=ag_alicechen) 先写', 'linkify 生成 #mention=<id> 链接')
}

// —— 2) mentions：code / URL / email 不参与 ——
{
  const text = [
    '正常 @Alice 要点名。',
    '行内 `@Alice` 不是点名。',
    '```',
    '@Alice 在围栏里也不是。',
    '```',
    '链接 https://example.com/@Alice 不算。',
    '邮箱 alice@example.com 也不算。',
  ].join('\n')
  const linked = mentions.linkifyMentions(text, roster)
  const codeSpan = linked.slice(linked.indexOf('行内'), linked.indexOf('在围栏里'))
  assert(!codeSpan.includes('#mention='), '行内 code 里的 @ 不被 linkify')
  const fenced = linked.slice(linked.indexOf('```'), linked.lastIndexOf('```'))
  assert(!fenced.includes('#mention='), 'fenced code 里的 @ 不被 linkify')
  const urlPart = linked.slice(linked.indexOf('链接'), linked.indexOf('邮箱'))
  assert(!urlPart.includes('#mention='), 'URL 里的 @ 不被 linkify')
  const emailPart = linked.slice(linked.indexOf('邮箱'))
  assert(!emailPart.includes('#mention='), 'email 里的 @ 不被 linkify')
  eq((linked.match(/#mention=/g) ?? []).length, 1, '只有正文里那一个 @ 被 linkify')
  eq(mentions.mentionIds('`@Alice` https://x/@Alice a@b.com', roster).length, 0, 'mentionIds 同样跳过 code/URL/email')
}

// —— 3) mentions：光标插入 / 查询 / 过滤 ——
{
  const text = '请 @Ali 看一下'
  const r = mentions.applyMention(text, 6, 'Alice')
  eq(r.text, '请 @Alice 看一下', '插入时替换半截 @Ali 且不重复空格')
  eq(r.caret, 9, 'caret 落在名字后的空格之后')
  eq(r.text.slice(0, r.caret), '请 @Alice ', 'caret 前缀可继续打字')
  const atEnd = mentions.applyMention('请 ', 2, 'Writer')
  eq(atEnd.text, '请 @Writer ', '光标不在 @ 后时按插入处理')
  eq(mentions.activeMentionQuery('请 @Wri', 6)?.query, 'Wri', '识别正在输入的 @ 查询')
  assert(mentions.activeMentionQuery('a@b', 3) === null, 'email 形态不触发选择器')
  assert(mentions.activeMentionQuery('请 @Alice 看', 10) === null, '@ 后已有空格时选择器关闭')

  const byName = mentions.filterMentionCandidates('alice c', roster).map((a) => a.id)
  eq(byName.join(','), 'ag_alicechen', '按名字过滤（alice c → Alice Chen）')
  const byRole = mentions.filterMentionCandidates('长文', roster).map((a) => a.id)
  eq(byRole.join(','), 'ag_alicechen', '按职责过滤（长文 → 长文写作）')
  const byID = mentions.filterMentionCandidates('ag_cri', roster).map((a) => a.id)
  eq(byID.join(','), 'ag_critic', '按 agent id 过滤')
  const byGroupRole = mentions.filterMentionCandidates('member', roster).map((a) => a.id)
  eq(byGroupRole.join(','), 'ag_alicechen,ag_critic', '按群角色过滤（member）')
  eq(mentions.filterMentionCandidates('', roster).length, 3, '空查询返回全部候选')
  // P6-12：observer 角色已删除 —— 派任务对象不再按角色过滤（只排除已删除成员）。
  const assignable = mentions.assignableMembers(roster).map((a) => a.id)
  eq(assignable.join(','), 'ag_alice,ag_alicechen,ag_critic', '所有未删除成员都可派任务')
}

// —— 3b) mentions：IME / 空候选 Enter / 句读关闭 / Markdown 链接标签（B2/B3/N4）——
{
  assert(mentions.isImeComposing({ isComposing: true }, 0), 'IME 合成中（isComposing）')
  assert(mentions.isImeComposing(null, 229), 'IME 合成中（keyCode 229）')
  assert(!mentions.isImeComposing({ isComposing: false }, 13), '普通 Enter 不是 IME')
  eq(mentions.mentionKeyAction('Enter', false), 'none', '空候选 Enter 不拦截（走发送）')
  eq(mentions.mentionKeyAction('Tab', false), 'none', '空候选 Tab 不拦截（走焦点移动）')
  eq(mentions.mentionKeyAction('Enter', true), 'pick', '有候选 Enter 确认')
  eq(mentions.mentionKeyAction('Escape', false), 'dismiss', '空候选 Esc 仍关闭选择器')

  assert(mentions.activeMentionQuery('@Writer，', 8) === null, '中文逗号结束 @ 查询（@Writer，）')
  assert(mentions.activeMentionQuery('@Writer。', 8) === null, '中文句号结束 @ 查询')
  assert(mentions.activeMentionQuery('@Writer,', 8) === null, 'ASCII 逗号结束 @ 查询')

  const label = '[问 @Writer](https://example.com/x)'
  eq(mentions.linkifyMentions(label, roster), label, 'Markdown 链接标签里的 @ 不改写（N4）')
  eq(mentions.mentionIds(label, roster).length, 0, '链接标签里的 @ 不算结构化 mention')
  const mixed = `${label} 和 @Alice`
  const linkedMixed = mentions.linkifyMentions(mixed, roster)
  eq((linkedMixed.match(/#mention=/g) ?? []).length, 1, '链接外的那一个 @ 仍 linkify')
  eq(mentions.linkifyMentions('![@Writer](a.png)', roster), '![@Writer](a.png)', '图片 alt 里的 @ 也不改写')
}

// —— 4) mentions：@all 与群名册 ——
{
  assert(mentions.hasMentionAll('@all 一起来'), '@all 识别')
  assert(mentions.hasMentionAll('@所有人 一起来'), '@所有人 识别')
  assert(!mentions.hasMentionAll('`@all` 不算'), 'code 里的 @all 不算')
  eq(mentions.linkifyMentions('@all 一起来', roster), '[@all](#mention=__all__) 一起来', '@all 也渲染成 #mention=__all__ 链接')
  eq(mentions.linkifyMentions('@所有人 一起来', roster), '[@所有人](#mention=__all__) 一起来', '@所有人 同样链接')
  eq(mentions.mentionIds('@all 和 @Alice 一起来', roster).join(','), 'ag_alice', '结构化 mentions 不含 @all')
  const group = {
    id: 'g1', name: '群',
    members: [
      { agent_id: 'ag_alice', role: 'owner' },
      { agent_id: 'ag_critic', role: 'member' },
      { agent_id: 'ag_gone', role: 'member' },
    ],
  }
  const agents = [
    { id: 'ag_alice', name: 'Alice', role: '研究' },
    { id: 'ag_critic', name: 'Critic', role: '反例审查' },
    { id: 'ag_gone', name: 'Gone', deleted: true },
  ]
  const built = mentions.groupRoster(group, agents)
  eq(built.length, 3, '群名册按成员展开')
  eq(built.find((a) => a.id === 'ag_alice')?.groupRole, 'owner', '名册保留群角色')
  assert(mentions.mentionIds('@Gone 你好', built).length === 0, '已删除成员不进结构化 mentions')
}

// —— 5) conversation：轮次/深度/队列 ——
{
  const conv = {
    group_id: 'g1', status: 'running', turns_used: 3, turn_budget: 8,
    depth: 2, max_depth: 4, queue_len: 1, pair_count: { 'ag_alice→ag_alicechen': 2 },
  }
  eq(conversation.conversationTurnText(conv), '第 3/8 轮 · 深度 2/4 · 队列 1', '有限预算仍显示第 N/上限 轮')
  eq(
    conversation.conversationTurnText({ ...conv, turn_budget: -1 }),
    '第 3 轮 · 深度 2/4 · 队列 1',
    '无限预算（-1）不显示 /8',
  )
  eq(
    conversation.conversationTurnText({ status: 'idle' }),
    '第 0 轮 · 深度 0/4 · 队列 0',
    '缺省限额 = 无上限（生产默认），不能又回落到 8',
  )
  eq(conversation.conversationStatusLabel('running'), '自动对话中', 'running 文案')
  eq(conversation.conversationStatusLabel('waiting_human'), '等你继续', 'waiting_human 文案')
  eq(conversation.conversationPairText(conv, { ag_alice: 'Alice', ag_alicechen: 'Alice Chen' }), 'Alice → Alice Chen 2/2', 'pair 计数按名还原')
}

// —— 6) conversation：终态文案 + 停止门禁 ——
{
  eq(conversation.conversationTerminalText({ status: 'stopped' }), '你停止了自动对话', 'stopped 默认文案')
  eq(
    conversation.conversationTerminalText({ status: 'stopped', stop_reason: '人类停止了自动对话，等你继续' }),
    '你停止了自动对话',
    '默认 stop_reason 不重复',
  )
  eq(
    conversation.conversationTerminalText({ status: 'exhausted' }),
    '自动对话已达上限，等你继续',
    'exhausted 兜底文案不提轮数',
  )
  eq(
    conversation.conversationTerminalText({ status: 'exhausted', stop_reason: 'A→B 踢皮球 2/2' }),
    '自动对话已达上限：A→B 踢皮球 2/2',
    'exhausted 带原因文案',
  )
  eq(conversation.conversationTerminalText({ status: 'running' }), '', 'running 没有终态文案')

  assert(conversation.conversationCanStop({ status: 'running', queue_len: 1 }, false) === true, 'running 且有队列可停')
  assert(conversation.conversationCanStop({ status: 'running', queue_len: 0 }, true) === true, 'running 且有 run 在跑可停')
  assert(conversation.conversationCanStop({ status: 'running', queue_len: 0 }, false) === false, 'running 但无事可做时禁用停止')
  assert(conversation.conversationCanStop({ status: 'exhausted' }, true) === true, 'exhausted 且 run 在跑可停')
  assert(conversation.conversationCanStop({ status: 'exhausted' }, false) === false, 'exhausted 无 run 时禁用停止')
  assert(conversation.conversationCanStop({ status: 'stopped', queue_len: 5 }, true) === false, 'stopped 永远禁用停止')
  // B7：人派的长任务可能没有对话视图/队列，只有 active（任务 dispatched/running）。
  assert(conversation.conversationCanStop({ status: 'idle', queue_len: 5 }, true) === true, 'idle 但任务/run 在跑时可停')
  assert(conversation.conversationCanStop({ status: 'idle', queue_len: 5 }, false) === false, 'idle 且无事可做时禁用停止')
  assert(conversation.conversationCanStop(null, true) === true, '没有对话但任务在跑时可停')
  assert(conversation.conversationCanStop(null, false) === false, '没有对话也没有 run 时禁用停止')
  assert(conversation.conversationCanStop({ status: 'waiting_human' }, true) === true, '队列排空但任务仍在跑时可停')
}

// —— 7) conversation：左栏预览回退 + 相对时间 + 99+ ——
{
  eq(conversation.groupPreview({ last_outcome: '任务完成：简报好了', last_text: '好的' }), '任务完成：简报好了', 'last_outcome 优先')
  eq(conversation.groupPreview({ last_text: '好的\n  收到 ' }), '好的 收到', 'fallback last_text 且折叠空白')
  eq(conversation.groupPreview({}), '', '空投影返回空串')
  const now = Date.parse('2026-03-10T12:00:00Z')
  eq(conversation.formatRelativeTime('2026-03-10T11:59:30Z', now), '刚刚', '相对时间：刚刚')
  eq(conversation.formatRelativeTime('2026-03-10T11:40:00Z', now), '20 分钟前', '相对时间：分钟')
  eq(conversation.formatRelativeTime('2026-03-10T09:00:00Z', now), '3 小时前', '相对时间：小时')
  eq(conversation.formatRelativeTime('2026-03-09T12:00:00Z', now), '昨天', '相对时间：昨天')
  eq(conversation.formatRelativeTime('2026-03-01T12:00:00Z', now), '3 月 1 日', '相对时间：月日')
  eq(conversation.formatRelativeTime('not-a-date', now), '', '非法时间返回空串')
  eq(conversation.unreadBadge(120), '99+', '未读角标 99+')
  eq(conversation.unreadBadge(7), '7', '未读角标正常值')
}

// —— 8) taskCard：completed 卡解析 ——
const card = (refs, text, sender = { kind: 'system' }) => ({
  seq: 42, kind: 'task_result', sender, text, refs, created_at: '2026-03-10T12:00:00Z',
})
{
  const msg = card({
    task_id: 'task_31', status: 'completed', agent_id: 'ag_writer', title: '把三家价格表写成两页简报',
    duration_ms: 18_400,
    usage: { input: 12_000, output: 3_000, cost_priced: true, cost_usd: 0.03 },
    artifacts: [
      { name: 'pricing-v3.md', path: 'out/pricing-v3.md', kind: 'created', size: 2140 },
      { name: '', path: '' },
    ],
    error: '',
  }, '三家价格表整理好了，含结论与风险。', { kind: 'agent', agent_id: 'ag_writer' })
  // 真实形状：结果卡 sender 就是执行者；派发者只在账本 origin 里（B5）。
  const task = {
    id: 'task_31', title: '把三家价格表写成两页简报', status: 'completed',
    origin: { kind: 'group', group_id: 'g1', agent_id: 'ag_researcher', agent_name: 'Researcher' },
    target: { kind: 'agent', agent_id: 'ag_writer', group_id: 'g1' },
    result: { summary: '三家价格表整理好了，含结论与风险。' },
  }
  const view = taskCard.parseTaskResult(msg, task)
  assert(view, 'task_result 解析出视图模型')
  eq(view.taskId, 'task_31', 'task_id')
  eq(view.status, 'completed', 'status')
  eq(view.statusLabel, '已完成', 'completed 状态文案')
  eq(view.tone, 'success', 'completed tone = success')
  assert(view.terminal === true, 'completed 是终态')
  eq(view.title, '把三家价格表写成两页简报', 'title 取 refs')
  eq(view.summary, '三家价格表整理好了，含结论与风险。', 'summary 取卡片正文')
  eq(view.error, '', 'completed 无错误')
  eq(view.agentId, 'ag_writer', '执行者取 agent_id（sender）')
  eq(view.originKind, 'group', '派发者取账本 origin.kind')
  eq(view.initiatorAgentId, 'ag_researcher', '派发者取 origin.agent_id，不取 sender')
  eq(view.initiatorName, 'Researcher', '派发者名取 origin.agent_name')
  eq(view.artifacts.length, 1, '空产物被丢弃')
  eq(view.artifacts[0].name, 'pricing-v3.md', '产物名字')
  eq(taskCard.taskDurationText(view.durationMs), '18.4s', '耗时文案')
  eq(taskCard.taskUsageText(view.usage), '12k → 3.0k tok · $0.03', '成本/用量文案')
  eq(taskCard.taskArtifactText(view.artifacts[0]), 'created · out/pricing-v3.md · 2.1KB', '产物一行')
  eq(taskCard.taskOutcomeLine(view.status, view.summary), '任务完成：三家价格表整理好了，含结论与风险。', '左栏结论行')
}

// —— 9) taskCard：failed / 中断 / 取消 / 拒绝 / 预算 + 状态映射 ——
{
  const failed = taskCard.parseTaskResult(card(
    { task_id: 'task_32', status: 'failed', agent_id: 'ag_writer', title: '抓取价格', error: '工具超时' },
    '工具超时',
  ))
  eq(failed.statusLabel, '失败', 'failed 文案')
  eq(failed.tone, 'error', 'failed tone = error')
  eq(failed.summary, '', '正文等于错误时不重复成 summary')
  eq(failed.error, '工具超时', 'error 单独保留')
  eq(taskCard.taskOutcomeLine('failed', '', '工具超时'), '任务失败：工具超时', 'failed 左栏结论优先原因')

  eq(taskCard.taskStatusView('interrupted').label, '被中断（人/重启/超时）', 'interrupted 文案')
  eq(taskCard.taskStatusView('interrupted').tone, 'warn', 'interrupted tone = warn')
  eq(taskCard.taskStatusView('canceled').label, '已取消', 'canceled 文案')
  eq(taskCard.taskStatusView('declined').label, '已被拒绝', 'declined 文案')
  eq(taskCard.taskStatusView('blocked_budget').label, '预算/护栏阻断', 'blocked 文案')
  eq(taskCard.taskStatusView('queued').tone, 'muted', 'queued tone = muted')
  eq(taskCard.taskStatusView('dispatched').tone, 'info', 'dispatched tone = info')
  eq(taskCard.taskStatusView('running').label, '进行中', 'running 文案')
  assert(taskCard.isTerminalTaskStatus('completed') && !taskCard.isTerminalTaskStatus('running'), '终态集合')
  eq(taskCard.parseTaskResult({ seq: 1, kind: 'text', text: '@Alice 你好' }), null, '非 task_result 返回 null')
  eq(taskCard.taskOutcomeLine('running', '半截'), '', '非终态不编结论')
  eq(taskCard.taskOutcomeLine('completed', ''), '任务完成', '空摘要只留标题')
}

// —— 9b) taskCard：origin human / refs 快照 / 缺状态（B5/N10）——
{
  const humanMsg = card(
    { task_id: 'task_h', status: 'completed', agent_id: 'ag_writer', title: '人派的任务' },
    '好了', { kind: 'agent', agent_id: 'ag_writer' },
  )
  const human = taskCard.parseTaskResult(humanMsg, {
    id: 'task_h', origin: { kind: 'human', group_id: 'g1' }, target: { kind: 'agent', agent_id: 'ag_writer' },
  })
  eq(human.originKind, 'human', '人派任务 origin.kind=human')
  eq(human.initiatorAgentId, '', '人派任务没有发起 agent')
  eq(human.initiatorName, '', '人派任务没有发起 agent 名')

  // 任务未加载（重启/历史）：结果卡 refs 自带 origin 快照，仍能画「Researcher → Writer」。
  const snapshotMsg = card(
    {
      task_id: 'task_s', status: 'completed', agent_id: 'ag_writer',
      origin_kind: 'group', origin_agent_id: 'ag_researcher', origin_agent_name: 'Researcher',
    },
    '好了', { kind: 'agent', agent_id: 'ag_writer' },
  )
  const snapshot = taskCard.parseTaskResult(snapshotMsg)
  eq(snapshot.originKind, 'group', '任务未加载时 origin_kind 取 refs 快照')
  eq(snapshot.initiatorAgentId, 'ag_researcher', '任务未加载时 origin_agent_id 取 refs 快照')
  eq(snapshot.initiatorName, 'Researcher', '任务未加载时 origin_agent_name 取 refs 快照')
  eq(snapshot.agentId, 'ag_writer', '快照场景执行者仍是 sender')

  const noOrigin = taskCard.parseTaskResult(card(
    { task_id: 'task_n', status: 'completed', agent_id: 'ag_writer' },
    '好了', { kind: 'agent', agent_id: 'ag_writer' },
  ))
  eq(noOrigin.initiatorAgentId, '', '账本与 refs 都没有 origin 时退回只显示执行者')
  eq(noOrigin.originKind, '', '缺 origin 时不猜 human')

  const unknown = taskCard.parseTaskResult(card(
    { task_id: 'task_u', agent_id: 'ag_writer' }, '结果不明',
  ))
  eq(unknown.status, 'unknown', '缺 status → unknown（不能默认 completed）')
  eq(unknown.statusLabel, '未知状态', 'unknown 文案')
  eq(unknown.tone, 'muted', 'unknown 不是成功绿')
  assert(unknown.terminal === false, 'unknown 不是终态')
}

// —— 9c) taskCard：workspace 目标（B1，群结果卡丢 Project 归属 / 误标「派发」）——
{
  // refs 形状逐字对齐 bridge（hub_task_result.go taskResultCardRefs + workspaceTaskCardText）：
  // workspace 目标没有 agent_id，署名是**发起者**，归属写在 target_* 与正文抬头里。
  const ws = '/ws/agent-hub'
  const sid = 's_proj_1'
  const head = `〔Project ${ws} / ${sid}〕`
  const wsRefs = {
    task_id: 'task_ws1', status: 'completed', title: '做一份三家对比表', duration_ms: 42_000,
    target_kind: 'workspace', target_workspace: ws, target_session_id: sid,
    origin_kind: 'group', origin_agent_id: 'ag_dispatch', origin_agent_name: '群派发A',
    usage: { input: 9000, output: 1200, cost_priced: true, cost_usd: 0.02 },
    artifacts: [], error: '',
  }
  const wsText = `${head}\n三家对比表已完成，含结论与风险。`
  const wsMsg = card(wsRefs, wsText, { kind: 'agent', agent_id: 'ag_dispatch' })
  // 账本已加载的**正常路径**：result.summary 是纯摘要，不含抬头。
  const wsTask = {
    id: 'task_ws1', title: '做一份三家对比表', status: 'completed',
    origin: { kind: 'group', group_id: 'g1', agent_id: 'ag_dispatch', agent_name: '群派发A' },
    target: { kind: 'workspace', workspace: ws, session_id: sid, group_id: 'g1' },
    result: { summary: '三家对比表已完成，含结论与风险。' },
  }
  const view = taskCard.parseTaskResult(wsMsg, wsTask)
  eq(view.targetKind, 'workspace', 'target_kind 暴露到视图（refs 快照）')
  eq(view.targetWorkspace, ws, 'target_workspace 暴露到视图')
  eq(view.targetSessionId, sid, 'target_session_id 暴露到视图')
  // B1 变异点：去掉「workspace 先取正文」就会退到 task.result.summary，抬头消失。
  assert(view.summary.includes(ws) && view.summary.includes(sid),
    `workspace 卡摘要必须带着 Project 归属（got=${JSON.stringify(view.summary)}）`)
  eq(taskCard.stripProjectHeader(view.summary), '三家对比表已完成，含结论与风险。',
    '抬头只由 meta 行画一次，正文渲染前剥掉')
  eq(view.agentId, '', 'workspace 目标没有执行智能体（否则会画成「群派发A 派发」）')
  // 防御：旧卡/脏 refs 把发起者写进 agent_id 时也不能画执行者 —— workspace 目标里没有
  // 智能体，画出来就是「群派发A → 群派发A」这种假执行者。
  eq(taskCard.parseTaskResult(card({ ...wsRefs, agent_id: 'ag_dispatch', target_agent_id: 'ag_dispatch' }, wsText)).agentId,
    '', 'workspace 卡即使 refs 带了 agent_id 也不画执行者')
  eq(view.originName, '群派发A', '发起者名（workspace 卡的 sender 就是发起者）')
  eq(view.initiatorAgentId, 'ag_dispatch', '发起者 id 仍按 origin 解析（打开 DM 用得到）')

  // 任务未加载（重启/历史）：归属仍在正文抬头里，不靠账本。
  const history = taskCard.parseTaskResult(card(wsRefs, wsText, { kind: 'agent', agent_id: 'ag_dispatch' }))
  assert(history.summary.includes(ws) && history.summary.includes(sid), '任务未加载时归属仍从正文读出')
  eq(history.originName, '群派发A', '任务未加载时发起者名取 refs 快照')

  // 抬头不能被当成标题：refs/账本都没有 title 时，标题取去掉抬头的正文首行。
  const noTitle = taskCard.parseTaskResult(card(
    { ...wsRefs, title: '', task_id: 'task_ws2' }, wsText,
  ))
  eq(noTitle.title, '三家对比表已完成，含结论与风险。', '标题兜底用去掉抬头的正文首行')
  eq(taskCard.taskOutcomeLine(noTitle.status, noTitle.summary), '任务完成：三家对比表已完成，含结论与风险。',
    '左栏结论行不把〔Project …〕抬头读进摘要')

  // 失败卡：正文 = 抬头 + 错误原文，摘要不能把错误重复一遍。
  const failedWs = taskCard.parseTaskResult(card(
    { ...wsRefs, status: 'failed', error: '工具超时' }, `${head}\n工具超时`,
  ))
  eq(failedWs.summary, '', 'workspace 失败卡正文等于错误时不重复成 summary')
  eq(failedWs.error, '工具超时', 'workspace 失败卡错误单独保留')
  eq(failedWs.targetSessionId, sid, '失败卡仍保留 Project 归属')

  // 人发起的 workspace 卡（理论上不写卡）：没有发起智能体时不能编一个。
  const humanWs = taskCard.parseTaskResult(card(
    { ...wsRefs, origin_kind: 'human', origin_agent_id: '', origin_agent_name: '' }, wsText,
    { kind: 'human' },
  ))
  eq(humanWs.originName, '', '人发起时没有发起者名（卡片写「你 发起」）')

  // agent 目标不受影响：执行者、派发者、摘要优先级都保持原样。
  const agentView = taskCard.parseTaskResult(card(
    { task_id: 'task_a', status: 'completed', agent_id: 'ag_writer', title: '写简报', target_kind: 'agent' },
    '简报写好了', { kind: 'agent', agent_id: 'ag_writer' },
  ), {
    id: 'task_a', origin: { kind: 'group', agent_id: 'ag_researcher', agent_name: 'Researcher' },
    target: { kind: 'agent', agent_id: 'ag_writer' }, result: { summary: '简报写好了（账本）' },
  })
  eq(agentView.agentId, 'ag_writer', 'agent 目标的执行者不变')
  eq(agentView.summary, '简报写好了（账本）', 'agent 目标仍按 refs → 账本 → 正文的优先级')
  eq(agentView.targetWorkspace, '', 'agent 目标没有 workspace 归属')
}

// —— 10) taskCard：target_agent_id 优先 + 未计价 + 缺字段兜底 ——
{
  const view = taskCard.parseTaskResult(card(
    {
      task_id: 'task_33', status: 'interrupted', agent_id: '', target_agent_id: 'ag_critic',
      title: '', duration_ms: 0, usage: { input: 800, output: 0, cost_priced: false, cost_usd: 0 },
      artifacts: [], error: '进程重启',
    },
    '进程重启',
  ))
  eq(view.agentId, 'ag_critic', 'target_agent_id 优先于 agent_id')
  eq(view.title, '进程重启', '没有 title 时用正文首行')
  eq(taskCard.taskUsageText(view.usage), '800 → 0 tok · 未计价', '未计价明确标注')
  eq(taskCard.taskDurationText(0), '', '0ms 不显示耗时')
  eq(taskCard.taskDurationText(125_000), '2 分 05 秒', '分钟级耗时')
  assert(view.tone === 'warn' && view.terminal, 'interrupted 是 warn 终态')
}

// —— 11) groupProjection：正在看 / 可用消息 / 预览 / 未读 / 任务更新 / 已读 / 竞态 ——
{
  assert(groupProjection.isHubTargetActive(false, 'g1', 'g1') === false, 'Hub 关闭时 activeGroupId 仍留着也不算正在看')
  assert(groupProjection.isHubTargetActive(true, 'g1', 'g1') === true, 'Hub 打开且当前群才算正在看')
  assert(groupProjection.isHubTargetActive(true, 'g1', 'g2') === false, '别的群不算正在看')
  assert(groupProjection.isUsableGroupMessage({ kind: 'routing_receipt', text: 'targets=ag_a' }) === false, '路由回执不进预览/未读')
  assert(groupProjection.isUsableGroupMessage({ kind: 'round_summary', text: '总结' }) === false, '轮次总结不进预览/未读')
  assert(groupProjection.isUsableGroupMessage({ kind: 'approval_stub', text: '审批' }) === false, '审批占位不进预览/未读')
  assert(groupProjection.isUsableGroupMessage({ kind: 'text', text: '   ' }) === false, '空正文不算')
  assert(groupProjection.isUsableGroupMessage({ kind: 'text', text: '好' }) === true, '普通正文算')

  const base = {
    unread: 2, last_seq: 4, last_text: '旧', last_task_id: 'task_0',
    last_task_status: 'completed', last_task_summary: '旧任务', last_task_seq: 4, last_outcome: '任务完成：旧任务',
  }
  const m = (seq, sender, text, extra = {}) => ({
    seq, kind: 'text', sender, text, created_at: '2026-03-10T12:00:00Z', ...extra,
  })
  const agent = { kind: 'agent', agent_id: 'ag_a' }
  const human = { kind: 'human' }
  let info = groupProjection.projectGroupMessage(base, m(5, agent, '新消息'), false)
  eq(info.last_text, '新消息', '预览取最后一条可用消息')
  eq(info.unread, 3, '关闭时 agent 消息累加未读')
  info = groupProjection.projectGroupMessage(info, m(6, human, '人自己说的'), false)
  eq(info.unread, 3, '人自己发的消息不累加未读')
  const uiOnly = groupProjection.projectGroupMessage(info, m(7, { kind: 'system' }, 'targets=ag_a', { kind: 'routing_receipt' }), false)
  assert(uiOnly === info, 'UI-only 消息不改投影（引用相等）')
  const seen = groupProjection.projectGroupMessage(info, m(8, agent, '正在看'), true)
  eq(seen.unread, 0, '正在看时未读保持 0')

  const cardMsg = m(9, agent, '三家价格表好了', {
    kind: 'task_result',
    refs: { task_id: 'task_9', status: 'completed', agent_id: 'ag_a', title: '简报' },
  })
  const withCard = groupProjection.projectGroupMessage(base, cardMsg, false)
  eq(withCard.last_outcome, '任务完成：三家价格表好了', '任务卡给出左栏结论')
  eq(withCard.last_task_summary, '三家价格表好了', '左栏摘要取正文')
  // workspace 卡（B1）：正文带〔Project …〕抬头，左栏的口径必须与 Go 的
  // groupPreviewText 一致 —— 摘要是摘要，抬头不跟着进左栏。
  const wsCard = groupProjection.projectGroupMessage(base, m(11, agent,
    `〔Project /ws/agent-hub / s_proj_1〕\n三家对比表已完成`, {
      kind: 'task_result',
      refs: { task_id: 'task_11', status: 'completed', target_kind: 'workspace', target_workspace: '/ws/agent-hub' },
    }), false)
  eq(wsCard.last_outcome, '任务完成：三家对比表已完成', 'workspace 卡左栏结论不带抬头')
  eq(wsCard.last_task_summary, '三家对比表已完成', 'workspace 卡左栏摘要不带抬头')
  eq(withCard.last_task_seq, 9, '记住任务卡 seq')
  eq(withCard.last_task_id, 'task_9', '记住任务 id')
  const after = groupProjection.projectGroupMessage(withCard, m(10, agent, '后续普通消息'), false)
  eq(after.last_outcome, '', '后续普通消息清掉结论')
  eq(after.last_task_seq, 0, '任务卡不再是最新可用消息')
  eq(after.last_task_id, 'task_9', 'last_task_* 仍跟随最近一张卡')

  // N2：迟到更新不能把已经被普通消息清掉的结论复活。
  const late = groupProjection.projectTaskUpdate(after, {
    task_id: 'task_9', status: 'completed', task: { id: 'task_9', result: { summary: '迟到的总结' } },
  })
  eq(late.last_outcome, '', '迟到的 hub_task_update 不复活 last_outcome')
  const latest = groupProjection.projectTaskUpdate(
    { ...withCard, last_seq: 9 },
    { task_id: 'task_9', status: 'completed', task: { id: 'task_9', result: { summary: '更新的总结' } } },
  )
  eq(latest.last_outcome, '任务完成：更新的总结', '任务卡仍是最新时更新结论')
  eq(groupProjection.projectTaskUpdate(withCard, { task_id: 'other', status: 'failed' }), withCard, '别的任务更新不动本群投影')

  assert(groupProjection.projectGroupRead({ ...withCard, unread: 3 }, 0).unread === 3, 'hub_read seq=0 不清未读')
  assert(groupProjection.projectGroupRead({ ...withCard, unread: 3, last_seq: 9 }, 8).unread === 3, '读到旧 seq 不清更新的未读')
  assert(groupProjection.projectGroupRead({ ...withCard, unread: 3, last_seq: 9 }, 9).unread === 0, '读到最新 seq 清未读')

  assert(groupProjection.shouldApplyGroupProgress(undefined, { last_seq: 1 }, 0, 0) === true, '无本地投影时采纳快照')
  assert(groupProjection.shouldApplyGroupProgress({ last_seq: 5 }, { last_seq: 6 }, 0, 100) === true, '快照 seq 更新时采纳')
  assert(groupProjection.shouldApplyGroupProgress({ last_seq: 5 }, { last_seq: 4 }, 0, 100) === false, '快照 seq 更旧时丢弃')
  assert(groupProjection.shouldApplyGroupProgress({ last_seq: 5 }, { last_seq: 5 }, 200, 100) === false, '同 seq 但请求开始后本地更新过 → 丢弃')
  assert(groupProjection.shouldApplyGroupProgress({ last_seq: 5 }, { last_seq: 5 }, 100, 200) === true, '同 seq 且本地没再更新 → 采纳')

  const ctxMsgs = [
    m(1, human, '@A @B 看一下'),
    m(2, { kind: 'agent', agent_id: 'ag_a' }, 'A 的背景', { context: { trigger_seqs: [1, 2] } }),
    m(3, { kind: 'agent', agent_id: 'ag_b' }, 'B 的背景'),
    m(4, { kind: 'agent', agent_id: 'ag_a' }, 'A 补充', { context: { trigger_seqs: [2, 3] } }),
  ]
  const map = groupProjection.triggerSeqBySender(ctxMsgs)
  eq(map.ag_a, 2, 'A 的触发消息 seq 精确到 2')
  eq(map.ag_b, 3, 'B 的触发消息 seq 精确到 3')
  const senders = groupProjection.uniqueContextSenders([
    { agent_id: 'ag_a', name: 'A' }, { agent_id: 'ag_a', name: 'A' }, { name: 'B' }, {},
  ])
  eq(senders.length, 2, 'context senders 去重并丢掉幽灵项')
  eq(groupProjection.uniqueContextSenders([{ agent_id: 'ag_a' }]).length, 1, '单发送者由组件隐藏')
}

// —— 12) store：Hub 关闭未读 / 打开已读 / hub_read seq<=0 / 迟到任务更新（B4/N2/N10）——
{
  const H = store.useHubStore
  const reset = (patch = {}) => H.setState({
    open: false, activeId: null, activeGroupId: null, groupInfo: {}, groupMessages: [],
    groupDrafts: [], groupReadSeq: {}, dm: {}, messages: [], tasks: [], groupConversations: {},
    ...patch,
  })
  const ev = (x) => H.getState().handleEvent(JSON.stringify(x))
  const groupMsg = (seq, text, sender = { kind: 'agent', agent_id: 'ag_a' }) => ev({
    event_type: 'hub_message', group_id: 'g1', seq,
    message: { seq, kind: 'text', sender, text, created_at: '2026-03-10T12:00:00Z' },
  })

  reset({ activeGroupId: 'g1', groupInfo: { g1: { unread: 0, last_seq: 10, last_text: '旧' } } })
  groupMsg(11, '关闭期间来的')
  const closed = H.getState().groupInfo.g1
  eq(closed.unread, 1, 'Hub 关闭时新消息累加未读（activeGroupId 还在）')
  eq(closed.last_seq, 11, '关闭时预览仍前移')
  eq(H.getState().groupMessages.length, 0, '关闭时不把消息塞进时间线')

  H.setState({ open: true })
  groupMsg(12, '打开后到的')
  const opened = H.getState().groupInfo.g1
  eq(opened.unread, 0, 'Hub 打开且当前群时新消息直接已读')
  eq(H.getState().groupMessages.length, 1, '打开时消息进时间线')

  reset({ open: true, activeGroupId: 'g1', groupInfo: { g1: { unread: 4, last_seq: 12 } } })
  ev({ event_type: 'hub_read', group_id: 'g1', seq: 0 })
  eq(H.getState().groupInfo.g1.unread, 4, 'hub_read seq=0 不清未读')
  ev({ event_type: 'hub_read', group_id: 'g1', seq: 12 })
  eq(H.getState().groupInfo.g1.unread, 0, 'hub_read 读到最新 seq 清未读')

  reset({
    activeId: 'ag_a', dm: { ag_a: { unread: 0, last_seq: 0 } },
  })
  ev({
    event_type: 'hub_message', agent_id: 'ag_a', seq: 1,
    message: { seq: 1, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_a' }, text: 'DM 关闭时来的' },
  })
  eq(H.getState().dm.ag_a.unread, 1, 'DM 在 Hub 关闭时累加未读')
  eq(H.getState().messages.length, 0, 'DM 关闭时不进时间线')
  H.setState({ open: true })
  ev({
    event_type: 'hub_message', agent_id: 'ag_a', seq: 2,
    message: { seq: 2, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_a' }, text: 'DM 打开后' },
  })
  eq(H.getState().dm.ag_a.unread, 0, 'DM 打开后已读')
  eq(H.getState().messages.length, 1, 'DM 打开后进时间线')

  reset({
    activeGroupId: 'g1',
    groupInfo: {
      g1: {
        unread: 0, last_seq: 10, last_text: '后续普通消息', last_task_id: 'task_9',
        last_task_seq: 0, last_outcome: '', last_task_status: 'completed',
      },
    },
  })
  ev({
    event_type: 'hub_task_update', group_id: 'g1', task_id: 'task_9', status: 'completed',
    task: { id: 'task_9', target: { group_id: 'g1' }, result: { summary: '迟到的总结' } },
  })
  eq(H.getState().groupInfo.g1.last_outcome, '', 'store 层：迟到任务更新不复活结论')
}

// —— 12b) P4-01 弱网：delta 只丢草稿、final 恒赢、对账清幽灵（store 事件层）——
{
  const H = store.useHubStore
  const ev = (x) => H.getState().handleEvent(JSON.stringify(x))
  const delta = (id, index, text, agent = 'ag_a') => ev({
    event_type: 'hub_message_delta', group_id: 'g1', agent_id: agent, round_id: 'r1',
    message_id: id, index, delta: text,
  })
  const final = (id, seq, text) => ev({
    event_type: 'hub_message', group_id: 'g1',
    message: { id, seq, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_a' }, text, created_at: '2026-03-10T12:00:00Z' },
  })
  H.setState({
    open: true, activeGroupId: 'g1', activeId: null, groupMessages: [], groupDrafts: [],
    finalizedMessageIds: [], groupInfo: {}, tasks: [],
  })

  delta('m1', 0, '你')
  delta('m1', 1, '好')
  eq(H.getState().groupDrafts[0]?.text, '你好', '事件层：顺序 delta 合并成一条草稿')
  delta('m1', 1, '重复')
  eq(H.getState().groupDrafts[0]?.text, '你好', '事件层：重复 index 丢弃')
  delta('m1', 0, '迟到')
  eq(H.getState().groupDrafts[0]?.text, '你好', '事件层：乱序 delta 丢弃')
  delta('m1', 3, '！')
  eq(H.getState().groupDrafts[0]?.text, '你好！', '事件层：index 空洞按到达内容拼接')

  final('m1', 10, '你好！最终版')
  eq(H.getState().groupDrafts.length, 0, '事件层：final 清掉草稿')
  eq(H.getState().groupMessages.filter((m) => m.id === 'm1').length, 1, '事件层：final 只落一条消息')
  assert(H.getState().finalizedMessageIds.includes('m1'), '事件层：final 记入 finalized')
  delta('m1', 0, '幽灵')
  eq(H.getState().groupDrafts.length, 0, '事件层：final 后迟到 delta 不复活草稿')
  final('m1', 10, '重复 final')
  eq(H.getState().groupMessages.filter((m) => m.id === 'm1').length, 1, '事件层：重复 final 不重复消息')

  // final 先到（WS 乱序/重连补包）：之后 index 0 的 delta 不能造草稿。
  final('m2', 11, '先到的 final')
  delta('m2', 0, '后到的 delta')
  eq(H.getState().groupDrafts.length, 0, '事件层：final-before-delta 不造草稿')

  // agent_end 兜底：没有 final 的草稿按 agent 清掉，别的 agent 不受影响。
  delta('b1', 0, 'B 的半截', 'ag_b')
  delta('a2', 0, 'A 的另一条', 'ag_a')
  eq(H.getState().groupDrafts.length, 2, '事件层：两个 agent 各一条草稿')
  ev({ event_type: 'agent_end', session_id: 'hub.ag_b.g.g1' })
  eq(H.getState().groupDrafts.length, 1, '事件层：agent_end 清掉该 agent 的草稿')
  eq(H.getState().groupDrafts[0].agentId, 'ag_a', '事件层：只清目标 agent')

  // 重连/重进群对账：历史加载期间漏掉 final 的草稿，历史里已有该 message_id →
  // 必须删掉并记入 finalized（迟到 delta 不能再复活）。
  H.setState({
    open: true, activeGroupId: null, groupMessages: [], groupDrafts: [],
    finalizedMessageIds: [], groupInfo: {}, tasks: [],
  })
  const t = transport.getTransport()
  const realSend = t.send.bind(t)
  let historyCmd = null
  t.send = (cmd) => {
    if (cmd.type === 'hub_group_get') {
      setTimeout(() => t.emit({
        event_type: 'command_response', id: cmd.id, ok: true,
        data: { group: { id: 'g1', name: 'G' }, cursor: { human_read_seq: 0 } },
      }), 0)
      return Promise.resolve({ ok: true })
    }
    if (cmd.type === 'hub_group_history') {
      historyCmd = cmd
      return Promise.resolve({ ok: true })
    }
    if (cmd.type === 'hub_group_activity') {
      setTimeout(() => t.emit({ event_type: 'command_response', id: cmd.id, ok: true, data: { activity: [] } }), 0)
      return Promise.resolve({ ok: true })
    }
    return realSend(cmd)
  }
  try {
    const inflight = H.getState().selectGroup('g1')
    // 等 hub_group_history 发出（selectGroup 先 await hub_group_get）。
    for (let i = 0; i < 50 && !historyCmd; i++) await new Promise((r) => setTimeout(r, 5))
    assert(!!historyCmd, '对账：发出了 hub_group_history')
    // 历史响应还没回来时，一条流式 delta 先到 → 草稿存在。
    delta('m_sync', 0, '断线期间只剩草稿')
    eq(H.getState().groupDrafts.length, 1, '对账：历史在飞时 delta 仍进草稿')
    t.emit({
      event_type: 'command_response', id: historyCmd.id, ok: true,
      data: { messages: [{ id: 'm_sync', seq: 20, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_a' }, text: '历史里已有 final' }] },
    })
    await inflight
  } finally {
    t.send = realSend
  }
  eq(H.getState().groupDrafts.length, 0, '对账：历史里已有 final 的草稿被清掉')
  assert(H.getState().finalizedMessageIds.includes('m_sync'), '对账：已落地的 id 记入 finalized')
  delta('m_sync', 0, '幽灵')
  eq(H.getState().groupDrafts.length, 0, '对账后迟到 delta 不复活草稿')
  eq(H.getState().groupMessages.filter((m) => m.id === 'm_sync').length, 1, '对账：历史消息只落一条')
}

// —— 12c) P4-01 性能 memo 回滚开关（localStorage: hub.perfMemo=0）——
{
  const flags = mod.perfFlags
  eq(flags.HUB_PERF_MEMO_KEY, 'hub.perfMemo', '回滚开关 key 固定（支持文档/脚本可复制）')
  let seenKey = null
  const read = (value) => flags.hubPerfMemoEnabled({ getItem: (key) => { seenKey = key; return value } })
  eq(read(null), true, '默认开启（键不存在）')
  eq(seenKey, 'hub.perfMemo', 'helper 读的就是 hub.perfMemo')
  eq(read('0'), false, "'0' 关闭 memo")
  eq(read('1'), true, '非 0 值保持开启')
  eq(read('false'), true, '只认精确 0（避免误读其他写法）')
  eq(flags.hubPerfMemoEnabled({ getItem: () => { throw new Error('SecurityError') } }), true, 'storage 读取抛错 fail-open（保持优化）')
  eq(flags.hubPerfMemoEnabled(null), true, 'storage 不可用默认开启')

  // 静态接线守卫：localStorage 关闭必须真的走 useMemo 的 undefined 分支，
  // 否则「有开关但没效果」也能骗过 helper 单测。
  const viewSrc = readFileSync(join(hubDir, 'GroupView.tsx'), 'utf8')
  assert(viewSrc.includes("from './perfFlags'"), 'GroupView 引用 perfFlags helper')
  assert(viewSrc.includes('useState(() => hubPerfMemoEnabled())'), 'GroupView 启动时读一次开关')
  const callAt = viewSrc.indexOf('const threadNodes = usePerfMemo(')
  assert(callAt >= 0, 'GroupView 通过 usePerfMemo 调用时间线 memo')
  const call = viewSrc.slice(callAt, callAt + 800)
  assert(call.includes('perfMemo ? ['), '开启时走依赖数组分支')
  assert(call.includes('] : undefined'), '关闭时 deps 传 undefined（每次渲染重算）')
  assert(viewSrc.includes('function usePerfMemo'), '存在带 undefined 语义的 usePerfMemo 包装')
}

// —— 12d) 场景2b：DM 多气泡流式（每轮 LLM 一条草稿，靠 message_id 分桶）——
{
  const H = store.useHubStore
  const ev = (x) => H.getState().handleEvent(JSON.stringify(x))
  // DM delta：group_id 是空串（不是缺字段），round_id 也是空串。
  const dmDelta = (id, index, text, agent = 'ag_a') => ev({
    event_type: 'hub_message_delta', group_id: '', agent_id: agent, round_id: '',
    message_id: id, index, delta: text,
  })
  const dmFinal = (id, seq, text, agent = 'ag_a') => ev({
    event_type: 'hub_message', agent_id: agent, seq,
    message: { id, seq, kind: 'text', sender: { kind: 'agent', agent_id: agent }, text, created_at: '2026-03-10T12:00:00Z' },
  })
  const grpDelta = (id, index, text, agent = 'ag_a') => ev({
    event_type: 'hub_message_delta', group_id: 'g1', agent_id: agent, round_id: 'r1',
    message_id: id, index, delta: text,
  })
  const fresh = (patch = {}) => H.setState({
    open: true, view: 'dm', activeId: 'ag_a', sid: 'hub.ag_a.dm', activeGroupId: null,
    messages: [], live: '', groupMessages: [], groupDrafts: [], finalizedMessageIds: [],
    dm: {}, groupInfo: {}, tasks: [],
    ...patch,
  })

  fresh()
  // 两轮 LLM = 两个 message_id = 两条草稿（场景2b 的核心：不能再挤成一条）。
  dmDelta('m_r1', 0, '第一轮')
  dmDelta('m_r2', 0, '第二轮')
  eq(H.getState().groupDrafts.length, 2, 'DM：两个 message_id 各一条草稿')
  assert(H.getState().groupDrafts.every((d) => d.groupId === '' && d.agentId === 'ag_a'), 'DM：草稿 groupId 为空、agentId 正确')
  dmDelta('m_r1', 1, '+1')
  dmDelta('m_r2', 1, '+2')
  eq(H.getState().groupDrafts.map((d) => d.text).join('|'), '第一轮+1|第二轮+2', 'DM：并行草稿各自增长')

  // final 各清各的：清掉 r1 后 r2 还在。
  dmFinal('m_r1', 10, '第一轮+1（终）')
  eq(H.getState().groupDrafts.length, 1, 'DM：final 只清自己那条草稿')
  eq(H.getState().groupDrafts[0].messageId, 'm_r2', 'DM：另一轮的草稿仍在流式')
  eq(H.getState().messages.filter((m) => m.id === 'm_r1').length, 1, 'DM：final 落一条消息')
  dmFinal('m_r2', 11, '第二轮+2（终）')
  eq(H.getState().groupDrafts.length, 0, 'DM：第二轮 final 后草稿清零')
  eq(H.getState().messages.length, 2, 'DM：两条气泡 = 两条消息（不合并、不重复）')
  assert(H.getState().finalizedMessageIds.includes('m_r1') && H.getState().finalizedMessageIds.includes('m_r2'), 'DM：final 记入 finalized')
  dmDelta('m_r1', 2, '幽灵')
  eq(H.getState().groupDrafts.length, 0, 'DM：final 后迟到 delta 不复活草稿')

  // 只收当前 DM：别的 agent 的 delta 一律不进草稿。
  dmDelta('m_x', 0, '别人的', 'ag_b')
  eq(H.getState().groupDrafts.length, 0, 'DM：非当前 agent 的 delta 丢弃')
  H.setState({ open: false })
  dmDelta('m_y', 0, 'Hub 关着')
  eq(H.getState().groupDrafts.length, 0, 'DM：Hub 关闭时不收草稿')

  // agent_end：清掉 ag_a 的 DM 草稿，但一条群草稿都不能动。
  fresh({ activeGroupId: 'g1' })
  grpDelta('g_keep', 0, '群的半截')
  H.setState({ activeId: 'ag_a', activeGroupId: null })
  dmDelta('m_a1', 0, 'A 的这一轮')
  dmDelta('m_a2', 0, 'A 的下一轮', 'ag_a')
  eq(H.getState().groupDrafts.length, 3, 'agent_end 前：群 1 + DM 2 条草稿')
  ev({ event_type: 'agent_end', session_id: 'hub.ag_a.dm' })
  eq(H.getState().groupDrafts.length, 1, 'agent_end：DM 草稿全清（多轮一起清）')
  eq(H.getState().groupDrafts[0].messageId, 'g_keep', 'agent_end：群草稿不受 DM agent_end 影响')
  // 反过来：群 agent_end 也不能清 DM 草稿。
  fresh()
  grpDelta('g_a', 0, '群的')
  dmDelta('m_keep', 0, '单聊的')
  ev({ event_type: 'agent_end', session_id: 'hub.ag_a.g.g1' })
  eq(H.getState().groupDrafts.length, 1, '群 agent_end 只清本群草稿')
  eq(H.getState().groupDrafts[0].messageId, 'm_keep', '群 agent_end 不动 DM 草稿')

  // content_chunk 仍然累加 live（非 Hub/项目会话要靠它），DM 的渲染只靠草稿去重。
  fresh()
  ev({ event_type: 'content_chunk', session_id: 'hub.ag_a.dm', run_id: 'run1', content: '旧链路' })
  eq(H.getState().live, '旧链路', 'content_chunk：live 兜底仍在（草稿为空时渲染）')
  dmDelta('m_live', 0, '新链路')
  eq(H.getState().groupDrafts.length, 1, 'delta 到后草稿接管')
  eq(H.getState().live, '旧链路', 'content_chunk 不因草稿清空 live（由 final 负责清）')
  dmFinal('m_live', 12, '新链路（终）')
  eq(H.getState().live, '', 'DM final 后 live 清空（不会和草稿叠成双气泡）')

  // 群行为未被改坏：群 delta 仍只认当前群，final 仍走群消息桶。
  fresh({ activeId: null, activeGroupId: 'g1', view: 'chat' })
  grpDelta('g1x', 0, '群的')
  eq(H.getState().groupDrafts.length, 1, '群：本群 delta 进草稿')
  ev({ event_type: 'hub_message_delta', group_id: 'g2', agent_id: 'ag_a', round_id: 'r1', message_id: 'g2x', index: 0, delta: '别的群' })
  eq(H.getState().groupDrafts.length, 1, '群：别的群的 delta 丢弃')
  ev({
    event_type: 'hub_message', group_id: 'g1',
    message: { id: 'g1x', seq: 20, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_a' }, text: '群的（终）', created_at: '2026-03-10T12:00:00Z' },
  })
  eq(H.getState().groupDrafts.length, 0, '群：final 清草稿')
  eq(H.getState().groupMessages.filter((m) => m.id === 'g1x').length, 1, '群：final 落一条群消息')
  eq(H.getState().messages.length, 0, '群：群消息不进 DM 消息桶')

  // 重连/重开 DM 的对账：hub_open 在飞时收到的流式草稿，历史里已有该 message_id →
  // 删草稿并记入 finalized（迟到 delta 不能再复活它）。
  fresh({ activeId: null })
  const t = transport.getTransport()
  const realSend = t.send.bind(t)
  let openCmd = null
  t.send = (cmd) => {
    if (cmd.type === 'hub_open') { openCmd = cmd; return Promise.resolve({ ok: true }) }
    return realSend(cmd)
  }
  try {
    const inflight = H.getState().select('ag_a')
    for (let i = 0; i < 50 && !openCmd; i++) await new Promise((r) => setTimeout(r, 5))
    assert(!!openCmd, 'DM 对账：发出了 hub_open')
    dmDelta('m_sync', 0, '断线期间只剩草稿')
    eq(H.getState().groupDrafts.length, 1, 'DM 对账：历史在飞时 delta 仍进草稿')
    t.emit({
      event_type: 'command_response', id: openCmd.id, ok: true,
      data: {
        agent: roster[0], session_id: 'hub.ag_a.dm', has_more: false,
        messages: [{ id: 'm_sync', seq: 21, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_a' }, text: '历史里已有 final' }],
      },
    })
    await inflight
  } finally {
    t.send = realSend
  }
  eq(H.getState().groupDrafts.length, 0, 'DM 对账：历史里已有 final 的草稿被清掉')
  assert(H.getState().finalizedMessageIds.includes('m_sync'), 'DM 对账：已落地的 id 记入 finalized')
  dmDelta('m_sync', 0, '幽灵')
  eq(H.getState().groupDrafts.length, 0, 'DM 对账后迟到 delta 不复活草稿')
  eq(H.getState().messages.filter((m) => m.id === 'm_sync').length, 1, 'DM 对账：历史消息只落一条')
}

// —— 13) DM 未读刷新竞态（P3b-08）：请求在飞时到达的消息不被旧快照覆盖 ——
{
  const proj = groupProjection
  // 纯函数口径（与 shouldApplyGroupProgress 同一把尺）
  assert(proj.shouldApplyDMInfo(undefined, { last_seq: 1 }, 0, 0), 'DM 投影：没有本地值直接采纳')
  assert(!proj.shouldApplyDMInfo({ last_seq: 5 }, { last_seq: 4 }, 0, 1), 'DM 投影：更旧的 last_seq 丢弃')
  assert(proj.shouldApplyDMInfo({ last_seq: 5 }, { last_seq: 6 }, 0, 1), 'DM 投影：更新的 last_seq 采纳')
  assert(!proj.shouldApplyDMInfo({ last_seq: 5 }, { last_seq: 5 }, 100, 50), 'DM 投影：同 seq 但请求开始后本地更新过 → 丢弃')
  assert(proj.shouldApplyDMInfo({ last_seq: 5 }, { last_seq: 5 }, 40, 50), 'DM 投影：同 seq 且本地没再动 → 采纳')
  assert(!proj.shouldApplyDMInfo({ last_seq: 5, unread: 0 }, { last_seq: 5, unread: 3 }, 100, 50), 'DM 投影：同 seq 的旧未读数不覆盖本地已读')

  // store 层：refresh 发出后、响应落地前到来的 hub_message 必须留住
  const H = store.useHubStore
  const t = transport.getTransport()
  const realSend = t.send.bind(t)
  let pending = null
  t.send = (cmd) => {
    if (cmd.type === 'hub_agent_list') { pending = cmd; return Promise.resolve({ ok: true }) }
    return realSend(cmd)
  }
  H.setState({
    open: false, activeId: null, agents: [], messages: [],
    dm: { ag_stale: { unread: 0, last_seq: 1, last_text: '旧' } },
  })
  try {
    const inflight = H.getState().refresh()
    H.getState().handleEvent(JSON.stringify({
      event_type: 'hub_message', agent_id: 'ag_stale', seq: 5,
      message: {
        seq: 5, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_stale' },
        text: '刷新期间来的', created_at: '2026-03-10T12:00:00Z',
      },
    }))
    assert(!!pending, 'refresh 发出了 hub_agent_list')
    // 手写 command_response：emit 是 transport 的私有方法，运行时照常可调（只为控制落地顺序）。
    t.emit({
      event_type: 'command_response', id: pending.id, ok: true, data: {
        agents: [{ id: 'ag_stale', name: 'Stale' }],
        dm: { ag_stale: { unread: 0, last_seq: 1, last_text: '旧' } },
      },
    })
    await inflight
  } finally {
    t.send = realSend // 断言抛错也要还原，后面的 mock 停止用例还要用真 send
  }
  eq(H.getState().dm.ag_stale.last_seq, 5, '旧响应不覆盖刷新期间的新消息（last_seq）')
  eq(H.getState().dm.ag_stale.unread, 1, '旧响应不覆盖刷新期间的未读')
  eq(H.getState().dm.ag_stale.last_text, '刷新期间来的', '旧响应不覆盖刷新期间的预览')
  eq(H.getState().agents.length, 1, 'agents 列表本身仍按响应更新')
  eq(H.getState().agents[0].id, 'ag_stale', 'agents 列表内容来自响应')
}

// —— 14) mock stop 结算在飞任务（P3b-08）：二次 assign 后停止按钮不被僵尸任务点亮 ——
{
  const t = transport.getTransport()
  let cmdSeq = 90_000
  const call = (type, payload) => new Promise((resolve) => {
    const id = ++cmdSeq
    const off = t.onEvent((line) => {
      let ev
      try { ev = JSON.parse(line) } catch { return }
      if (ev.event_type !== 'command_response' || ev.id !== id) return
      off()
      resolve(ev)
    })
    void t.send({ id, type, payload })
  })
  const events = []
  const offAll = t.onEvent((line) => {
    try { events.push(JSON.parse(line)) } catch { /* 非 JSON 行忽略 */ }
  })
  const waitFor = async (pred, label, timeoutMs = 8000) => {
    const deadline = Date.now() + timeoutMs
    while (Date.now() < deadline) {
      if (pred()) return
      await new Promise((r) => setTimeout(r, 25))
    }
    throw new Error(`等待超时：${label}`)
  }
  const updatesOf = (id) => events.filter((e) => e.event_type === 'hub_task_update' && e.task_id === id)
  const taskByID = (list, id) => list.find((x) => x.id === id)
  const activeTasks = (list) => list.filter((x) => x.status === 'dispatched' || x.status === 'running').length

  const a1 = await call('hub_group_assign', {
    group_id: 'grp_mock1', agent_id: 'ag_mockwri2', instruction: '先整理一页价格对比', client_msg_id: 'cm_p3b_stop_1',
  })
  assert(a1.ok === true, 'mock 派任务成功')
  const firstID = String(a1.data.task_id)
  // 停止必须打在真的「在飞」上（queued 不算 active）：等 dispatched 事件。
  await waitFor(() => updatesOf(firstID).some((e) => e.status === 'dispatched'), '第一次任务 dispatched')

  const stop = await call('hub_conversation_stop', { group_id: 'grp_mock1' })
  assert(stop.ok === true, 'mock 停止成功')
  eq(stop.data.conversation?.status, 'stopped', '停止后对话终态 stopped')
  assert(Number(stop.data.interrupted) >= 1, `停止结算了在飞任务（interrupted=${stop.data.interrupted}）`)
  const terminal = updatesOf(firstID).filter((e) => e.status === 'interrupted')
  eq(terminal.length, 1, '停止为在飞任务发了一条 interrupted task_update')
  assert(String(terminal[0]?.error ?? '').includes('停止'), '中断事件带人话原因')

  let list = (await call('hub_task_list')).data.tasks
  eq(taskByID(list, firstID)?.status, 'interrupted', '账本里第一次任务已结算为 interrupted')
  eq(activeTasks(list), 0, '停止后没有 dispatched/running 的任务')

  // 停止之后再来一次派发：旧任务不得复活，停止按钮只在真的有事可停时可点。
  const a2 = await call('hub_group_assign', {
    group_id: 'grp_mock1', agent_id: 'ag_mockwri2', instruction: '再来一页', client_msg_id: 'cm_p3b_stop_2',
  })
  assert(a2.ok === true, '停止后第二次派任务成功')
  const secondID = String(a2.data.task_id)
  assert(secondID !== firstID, '第二次派发是新任务，不是复用僵尸任务')
  list = (await call('hub_task_list')).data.tasks
  eq(taskByID(list, firstID)?.status, 'interrupted', '二次派发不改写已中断的第一次任务')
  await waitFor(() => updatesOf(secondID).some((e) => e.status === 'dispatched'), '第二次任务 dispatched')
  list = (await call('hub_task_list')).data.tasks
  eq(activeTasks(list), 1, '第二次派发期间确实有在飞任务（只有新任务）')
  eq(taskByID(list, firstID)?.status, 'interrupted', '第二次任务在飞时第一次任务仍是 interrupted')
  // 停止后正在跑：可停（active）。
  let convEvents = events.filter((e) => e.event_type === 'hub_conversation' && e.group_id === 'grp_mock1')
  assert(conversation.conversationCanStop(convEvents[convEvents.length - 1]?.conversation, true), '派发期间可停止')

  await waitFor(() => updatesOf(secondID).some((e) => e.status === 'completed'), '第二次任务 completed', 10_000)
  list = (await call('hub_task_list')).data.tasks
  eq(taskByID(list, secondID)?.status, 'completed', '第二次任务完成')
  eq(activeTasks(list), 0, '完成后没有在飞任务（僵尸任务未复活）')
  convEvents = events.filter((e) => e.event_type === 'hub_conversation' && e.group_id === 'grp_mock1')
  const tail = convEvents[convEvents.length - 1]?.conversation
  eq(conversation.conversationCanStop(tail, activeTasks(list) > 0), false, '二次 assign 完成后停止按钮保持禁用')
  offAll()
}

console.log(`\n✅ hub P3b 纯逻辑：${pass} 项全部通过`)
