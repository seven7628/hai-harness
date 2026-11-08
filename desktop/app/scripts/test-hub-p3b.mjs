#!/usr/bin/env node
// P3b-07 前端 hub 纯逻辑验收（Node 直跑，无需浏览器）：
//   mentions：最长名字优先、fenced/inline code/URL/email 跳过、光标插入、选择器过滤；
//   conversation：轮次/深度/队列文案、终态文案、停止按钮门禁、左栏预览回退、相对时间；
//   taskCard：task_result 卡的 completed/failed/中断/拒绝文案、产物/耗时/成本、左栏结论行。
// 这三块是「@ 路由 / 自动对话 / 任务卡」在 UI 侧的唯一分叉点，值得独立回归。
import { build } from 'esbuild'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const hubDir = join(__dirname, '..', 'src', 'hub')
// 一个虚拟入口同时导出三个模块：esbuild 只跑一次，导入一个 data URL 即可。
const result = await build({
  stdin: {
    contents: [
      "export * as mentions from './mentions.ts'",
      "export * as conversation from './conversation.ts'",
      "export * as taskCard from './taskCard.ts'",
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
})
const source = result.outputFiles[0].text
const mod = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)
const { mentions, conversation, taskCard } = mod

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const eq = (got, want, m) => assert(got === want, `${m}（got=${JSON.stringify(got)} want=${JSON.stringify(want)}）`)

const roster = [
  { id: 'ag_alice', name: 'Alice', role: '研究', groupRole: 'owner', accent: '#7bc199' },
  { id: 'ag_alicechen', name: 'Alice Chen', role: '长文写作', groupRole: 'member', accent: '#9daed2' },
  { id: 'ag_critic', name: 'Critic', role: '反例审查', groupRole: 'observer' },
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
  const byGroupRole = mentions.filterMentionCandidates('observer', roster).map((a) => a.id)
  eq(byGroupRole.join(','), 'ag_critic', '按群角色过滤')
  eq(mentions.filterMentionCandidates('', roster).length, 3, '空查询返回全部候选')
  const assignable = mentions.assignableMembers(roster).map((a) => a.id)
  assert(!assignable.includes('ag_critic'), 'observer 不进派任务成员列表')
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
      { agent_id: 'ag_critic', role: 'observer' },
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
  eq(conversation.conversationTurnText(conv), '第 3/8 轮 · 深度 2/4 · 队列 1', '轮次/深度/队列一行')
  eq(conversation.conversationTurnText({ status: 'idle' }), '第 0/8 轮 · 深度 0/4 · 队列 0', '缺省限额补 Go 默认值')
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
    '自动对话已达上限（8 轮），等你继续',
    'exhausted 默认文案',
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
  assert(conversation.conversationCanStop({ status: 'idle', queue_len: 5 }, true) === false, 'idle 永远禁用停止')
  assert(conversation.conversationCanStop(null, true) === false, '没有对话时禁用停止')
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
  }, '三家价格表整理好了，含结论与风险。', { kind: 'agent', agent_id: 'ag_researcher' })
  const view = taskCard.parseTaskResult(msg)
  assert(view, 'task_result 解析出视图模型')
  eq(view.taskId, 'task_31', 'task_id')
  eq(view.status, 'completed', 'status')
  eq(view.statusLabel, '已完成', 'completed 状态文案')
  eq(view.tone, 'success', 'completed tone = success')
  assert(view.terminal === true, 'completed 是终态')
  eq(view.title, '把三家价格表写成两页简报', 'title 取 refs')
  eq(view.summary, '三家价格表整理好了，含结论与风险。', 'summary 取卡片正文')
  eq(view.error, '', 'completed 无错误')
  eq(view.agentId, 'ag_writer', '执行者取 agent_id')
  eq(view.initiatorAgentId, 'ag_researcher', '派发者取 sender')
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

console.log(`\n✅ hub P3b-07 纯逻辑：${pass} 项全部通过`)
