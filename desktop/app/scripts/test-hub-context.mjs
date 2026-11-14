#!/usr/bin/env node
// P4-02 前端验收（Node 直跑，无需浏览器）：
//   deeplink：canonical 格式/解析、非法 scheme/host/空 id/负 seq 一律拒绝、argv 提取；
//   contextView：消息级触发上下文选择、trigger seq 定位落点、提示词分层归一化；
//   seqJump：深链/触发 chip 的跳转判定（目标 + 已加载）与高亮 class、DOM 落点；
//   exportMarkdown：HTML/元字符转义、群时间线、任务 Markdown（结果/产物/用量）；
//   store：深链打开/选中/定位、重复深链只加载一次、无效链报错、提示词预览只读缓存；
//   promptScope：提示词预览的缓存键/作用域判定（DM ≠ 群、群 A ≠ 群 B、迟到响应）；
//   ContextPanel 接线：两处 loadPromptPreview 都必须把群 id 传下去（源码守卫）。
// 这些是「Context 面板 / hub:// 深链 / 导出」在 UI 侧的唯一分叉点，值得独立回归。
import { build } from 'esbuild'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const hubDir = join(__dirname, '..', 'src', 'hub')
const result = await build({
  stdin: {
    contents: [
      "export * as deeplink from './deeplink.ts'",
      "export * as contextView from './contextView.ts'",
      "export * as seqJump from './seqJump.ts'",
      "export * as promptScope from './promptScope.ts'",
      "export * as exportMarkdown from './exportMarkdown.ts'",
      "export * as store from './useHubStore.ts'",
      "export * as transport from '../transport/index.ts'",
      "export * as electronDeepLink from '../../electron/deeplink.ts'",
    ].join('\n'),
    resolveDir: hubDir,
    sourcefile: 'hub-context-entry.ts',
    loader: 'ts',
  },
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
  // useHubStore 会把 mock transport 一起拉进来；Vite 的 ?raw/?url 在 esbuild 里没有
  // 对应 loader，给它们一个空模块（本测试不渲染品牌图）。
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
const { deeplink, contextView, seqJump, promptScope, exportMarkdown, store, transport, electronDeepLink } = mod

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const eq = (got, want, m) => assert(got === want, `${m}（got=${JSON.stringify(got)} want=${JSON.stringify(want)}）`)

// —— 1) deeplink：canonical 格式 ——
{
  eq(deeplink.formatHubDeepLink('group', 'grp_1', 42), 'hub://group/grp_1?seq=42', '群深链 canonical 形态')
  eq(deeplink.formatHubDeepLink('dm', 'ag 1'), 'hub://dm/ag%201', 'id 按 URI 组件转义')
  eq(deeplink.formatHubDeepLink('dm', 'ag_1', 0), 'hub://dm/ag_1', 'seq=0 不写查询参数')
  eq(deeplink.formatHubDeepLink('dm', 'ag_1', -3), 'hub://dm/ag_1', '负 seq 不写进链接')
  eq(deeplink.formatHubDeepLink('dm', '', 3), '', '空 id 生成空串（按钮据此禁用）')
  eq(deeplink.formatHubDeepLink('channel', 'x', 1), '', '未知 host 不生成链接')
}

// —— 2) deeplink：解析 / 拒绝 ——
{
  const p = deeplink.parseHubDeepLink('hub://dm/ag%201?seq=7')
  eq(p.kind, 'dm', '解析 host=dm')
  eq(p.id, 'ag 1', 'id 解码')
  eq(p.seq, 7, '解析 seq')
  const round = deeplink.parseHubDeepLink(deeplink.formatHubDeepLink('group', 'g-1', 3))
  eq(`${round.kind}:${round.id}:${round.seq}`, 'group:g-1:3', 'format → parse 往返一致')
  eq(deeplink.parseHubDeepLink('hub://group/g1').seq, 0, '缺省 seq = 0（只打开不定位）')
  eq(deeplink.parseHubDeepLink('hub://group/g1?seq=0').seq, 0, 'seq=0 合法')
  eq(deeplink.parseHubDeepLink('HUB://GROUP/G1?seq=2').id, 'G1', 'scheme/host 大小写宽容（id 保留原样）')

  const bad = [
    ['空串', ''],
    ['别的 scheme', 'https://group/g1'],
    ['未知 host', 'hub://channel/g1'],
    ['缺 id', 'hub://group'],
    ['空 id', 'hub://group/'],
    ['编码出来的斜杠', 'hub://dm/%2F'],
    ['负 seq', 'hub://group/g1?seq=-1'],
    ['非数字 seq', 'hub://group/g1?seq=abc'],
    ['小数 seq', 'hub://group/g1?seq=1.5'],
    ['科学计数 seq', 'hub://group/g1?seq=1e3'],
    ['带账号信息', 'hub://user@group/g1'],
    ['多段路径', 'hub://group/g1/extra'],
    ['非 canonical 形态', 'hub:group/g1'],
    ['非法百分号转义', 'hub://group/%E0%A4%A'],
  ]
  for (const [label, raw] of bad) {
    eq(deeplink.parseHubDeepLink(raw), null, `拒绝深链：${label}`)
  }
  eq(deeplink.hubDeepLinkKey({ kind: 'dm', id: 'a', seq: 1 }), 'dm:a:1', '去重键包含位置')
  eq(deeplink.hubDeepLinkKey(null), '', '空链接去重键为空')
}

// —— 3) Electron argv：只认 hub:// 参数 ——
{
  const { extractHubDeepLinkArg } = electronDeepLink
  eq(extractHubDeepLinkArg(['/Applications/Hai.app/Contents/MacOS/Hai']), null, '普通启动 argv 没有深链')
  eq(extractHubDeepLinkArg(['/app', '--flag', 'hub://dm/ag_1?seq=3']), 'hub://dm/ag_1?seq=3', '从 argv 提取深链')
  eq(extractHubDeepLinkArg(['"hub://group/g1?seq=2"']), 'hub://group/g1?seq=2', 'Windows 引号包裹去引号')
  eq(extractHubDeepLinkArg(null), null, 'argv 缺失返回 null')
}

// —— 4) contextView：消息级触发上下文 ——
{
  const msg = {
    seq: 9,
    sender: { kind: 'agent', agent_id: 'ag_b' },
    round_id: ' rnd_9 ',
    context: {
      trigger_seqs: [3, 0, 3, -2, 7],
      senders: [
        { agent_id: 'ag_a', name: '' },
        { agent_id: 'ag_a', name: 'Alice' },
        { name: '外部' },
        {},
      ],
      reason: '  被 @ 提及  ',
    },
  }
  const view = contextView.messageContextView(msg, { ag_a: 'Alice' })
  eq(view.seq, 9, '上下文视图带消息 seq')
  eq(view.roundId, 'rnd_9', 'round_id 去空白')
  eq(view.reason, '被 @ 提及', 'reason 去空白')
  eq(view.triggerSeqs.join(','), '3,7', 'trigger_seqs 过滤非正数并去重')
  eq(view.senders.length, 2, 'senders 去重并丢幽灵项')
  eq(view.senders[0].name, 'Alice', 'sender 名字缺省时用名册兜底')
  eq(view.senders[1].name, '外部', '只有名字的 sender 保留')
  assert(view.hasContext, '有触发上下文')
  const empty = contextView.messageContextView({ seq: 1 })
  assert(!empty.hasContext, '普通消息没有触发上下文')
  eq(empty.triggerSeqs.length, 0, '空 trigger_seqs')
  const repr = contextView.messageContextView({ seq: 2, context: { reason: 'R' } })
  assert(repr.hasContext, '只有 reason 也算有上下文')

  const msgs = [
    { seq: 1, sender: { kind: 'human' }, text: 'hi', context: { trigger_seqs: [0] } },
    { seq: 2, sender: { kind: 'agent', agent_id: 'a' }, text: 'x', context: { reason: 'r' } },
    { seq: 3, sender: { kind: 'agent', agent_id: 'a' }, text: 'y' },
    { seq: -1, sender: { kind: 'agent' }, text: 'pending', pending: true, context: { reason: 'r' } },
    { seq: 4, sender: { kind: 'system' }, text: 'z', context: { reason: 'sys' } },
  ]
  const options = contextView.contextMessageOptions(msgs)
  eq(options.map((m) => m.seq).join(','), '4,2', '候选 = 非人类且有上下文，最新在前')
  eq(contextView.contextMessageOptions(msgs, 1).map((m) => m.seq).join(','), '4', 'limit 只留最近的 N 条')
  eq(contextView.pickContextMessage(msgs, 2).seq, 2, '尊重选中的消息')
  eq(contextView.pickContextMessage(msgs, 99).seq, 4, '选中项失效时回退最新一条')
  eq(contextView.pickContextMessage([], 3), null, '没有候选返回 null')

  eq(contextView.resolveTriggerJump([5, 6], [{ seq: 6 }]), 6, '优先跳已加载的触发 seq')
  eq(contextView.resolveTriggerJump([5, 6], [{ seq: 1 }]), 5, '都未加载时退回第一个合法 seq')
  eq(contextView.resolveTriggerJump([0, -1], [{ seq: 1 }]), 0, '没有合法 seq 返回 0')
  eq(contextView.resolveTriggerJump(undefined, null), 0, 'undefined 输入返回 0')
}

// —— 5) contextView：提示词分层形状 ——
{
  const layers = contextView.promptLayerViews([
    { key: 'protocol', title: 'L1 · Hub 协议', text: 'abc' },
    { key: 'roster', title: 'L2 · 群名册', text: '' },
    { key: 'memory', title: '记忆', text: 'm' },
    { key: 'environment', title: '', text: 'env' },
    { key: 'empty', title: '', text: '' },
  ])
  eq(layers.length, 4, '空层被丢掉')
  eq(layers.map((l) => l.label).join(','), 'L1,L2,L4,L3', '层号从标题提取，缺标题按 key 兜底')
  eq(layers[0].title, 'L1 · Hub 协议', '标题原样保留')
  eq(layers[0].chars, 3, '字符数按正文长度')
  eq(contextView.promptLayerLabel('identity', 'L2 · 身份与职责'), 'L2', '标题里的 L 号优先')
  eq(contextView.promptLayerViews(null).length, 0, 'null layers → 空数组')
  assert(contextView.promptRuntimeNote('note').includes('note'), '后端 note 保留')
  assert(contextView.promptRuntimeNote('note').includes('L5'), '永远说明 L5 不在预览内')
  eq(contextView.promptRuntimeNote('L5 由内核装配'), 'L5 由内核装配', '已说明 L5 的 note 不重复追加')
  assert(contextView.promptRuntimeNote('').includes('L5'), '空 note 也给出 L5 说明')
  eq(contextView.promptPreviewBytes(123, layers), 123, '优先后端 total_bytes')
  eq(contextView.promptPreviewBytes(0, [{ chars: 2 }, { chars: 3 }]), 5, '缺 total_bytes 时按正文估算')
}

// —— 6) seqJump：跳转判定 + 高亮 + DOM 落点 ——
{
  const req = { seq: 12, targetId: 'grp_1', nonce: 7 }
  const plan = seqJump.seqJumpPlan(req, { targetId: 'grp_1', messages: [{ seq: 12 }] })
  eq(plan.seq, 12, '目标一致且消息已加载 → 执行跳转')
  eq(plan.nonce, 7, 'plan 保留 nonce（消费方只清自己接的那次）')
  eq(seqJump.seqJumpPlan(req, { targetId: 'dm_1', messages: [{ seq: 12 }] }), null, '目标不一致 → 不跳')
  eq(seqJump.seqJumpPlan(req, { targetId: 'grp_1', messages: [{ seq: 11 }] }), null, '消息未加载 → 不跳')
  eq(seqJump.seqJumpPlan({ ...req, seq: 0 }, { targetId: 'grp_1', messages: [{ seq: 0 }] }), null, 'seq<=0 → 不跳')
  eq(seqJump.seqJumpPlan(null, { targetId: 'grp_1', messages: [] }), null, '没有请求 → 不跳')

  eq(seqJump.highlightClass(12, 12), ' hub-msg-highlight', '命中 seq 才高亮')
  eq(seqJump.highlightClass(12, 11), '', '别的 seq 不高亮')
  eq(seqJump.highlightClass(0, 0), '', '乐观气泡（seq<=0）不高亮')
  eq(seqJump.highlightClass(undefined, 12), '', '缺 seq 不高亮')
  eq(seqJump.seqDataAttr(12), '12', '正 seq 生成 data-seq')
  eq(seqJump.seqDataAttr(0), undefined, '乐观气泡没有 data-seq')
  eq(seqJump.seqDataAttr(-3), undefined, '负数 seq 没有 data-seq')

  const calls = []
  const root = {
    querySelector(sel) {
      calls.push(sel)
      return { scrollIntoView(opts) { calls.push(opts) } }
    },
  }
  assert(seqJump.scrollToSeq(root, 12), '找到元素返回 true')
  eq(calls[0], '[data-seq="12"]', '按 data-seq 定位')
  eq(calls[1].block, 'center', '滚动居中')
  assert(!seqJump.scrollToSeq({ querySelector: () => null }, 12), '找不到元素返回 false')
  assert(!seqJump.scrollToSeq(root, 0), 'seq<=0 不查询 DOM')
  eq(calls.length, 2, '无效 seq 没有产生额外查询')
}

// —— 7) exportMarkdown：转义 ——
{
  const escaped = exportMarkdown.escapeHtmlText('<script>alert("x")&</script>')
  assert(!escaped.includes('<script>'), 'HTML 标签被转义')
  assert(escaped.includes('&lt;script&gt;'), '< 与 > 变成实体')
  assert(escaped.includes('&amp;'), '& 变成实体')
  const inline = exportMarkdown.escapeInline('a*b_[c]|<d>#e`f~g', 80)
  assert(inline.includes('\\*') && inline.includes('\\_') && inline.includes('\\['), 'Markdown 元字符被转义')
  assert(inline.includes('&lt;d&gt;'), '单行字段里的 HTML 也被转义')
  const clipped = exportMarkdown.escapeInline('价格表'.repeat(10), 6)
  eq(Array.from(clipped).length, 6, '按码点截断（CJK 不被切半）')
  assert(clipped.endsWith('…'), '截断带省略号')
  eq(exportMarkdown.escapeInline(' a\n b '), 'a b', '单行字段折叠换行')
  eq(exportMarkdown.quoteMarkdown(''), '> （空）', '空正文仍给引用块')
  const quoted = exportMarkdown.quoteMarkdown('第一行\n\n<script>')
  eq(quoted, '> 第一行\n>\n> &lt;script&gt;', '多行正文逐行加 > 并转义')
}

// —— 8) exportMarkdown：群时间线 ——
{
  const agents = [
    { id: 'ag_a', name: 'Alice' },
    { id: 'ag_b', name: 'Bob' },
  ]
  const group = {
    id: 'grp_1',
    name: 'Price <Team>',
    owner_agent_id: 'ag_a',
    members: [{ agent_id: 'ag_a', role: 'owner' }, { agent_id: 'ag_b' }],
    policy: { mode: 'ask', daily_budget_usd: 5 },
  }
  const messages = [
    {
      seq: 2, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_b' },
      text: '第二行\n<script>alert(1)</script>', created_at: '2026-03-10T12:01:00Z',
      round_id: 'rnd_1', context: { trigger_seqs: [1], senders: [{ agent_id: 'ag_a' }] },
    },
    { seq: 1, kind: 'text', sender: { kind: 'human' }, text: '@Alice 看下', created_at: '2026-03-10T12:00:00Z' },
    {
      seq: 3, kind: 'task_result', sender: { kind: 'agent', agent_id: 'ag_b' },
      text: '搞定了', created_at: '2026-03-10T12:02:00Z',
      refs: { task_id: 't_1', status: 'completed', target_agent_id: 'ag_b' },
    },
  ]
  const tasks = [{
    id: 't_1', status: 'completed',
    result: { summary: '三家价格表', artifacts: [{ path: 'out/pricing.md', kind: 'created', size: 345 }] },
  }]
  const md = exportMarkdown.groupTimelineMarkdown({
    group, agents, messages, tasks, exportedAt: '2026-03-10T13:00:00Z',
  })
  assert(md.startsWith('# 群：Price &lt;Team&gt;'), '群名在标题里且转义')
  assert(md.includes('- 群组：`grp_1`'), '群 id')
  assert(md.includes('- 成员：Alice（owner）、Bob'), '成员与角色')
  assert(md.includes('- 策略：mode=ask · 日预算 $5'), '群策略（并发不再是策略旋钮）')
  assert(md.includes('- 消息：3 条 · seq 1–3'), '消息数量与 seq 范围')
  assert(md.includes('- 导出：2026-03-10T13:00:00Z'), '导出时间可固定')
  assert(md.indexOf('### #1') < md.indexOf('### #2'), '消息按 seq 升序')
  assert(md.indexOf('### #2') < md.indexOf('### #3'), '任务卡在最后')
  assert(md.includes('### #1 · 你 · 2026-03-10T12:00:00Z'), '人类消息的发送者/时间')
  assert(md.includes('> &lt;script&gt;alert(1)&lt;/script&gt;'), '正文 HTML 被转义')
  assert(!md.includes('<script>'), '导出不含可执行 HTML')
  assert(md.includes('- 上下文：触发：#1 · 来自 Alice · 轮次 `rnd_1`'), '触发上下文摘要')
  assert(md.includes('**任务卡** `t_1` · completed · 执行 Bob'), '任务卡摘要')
  assert(md.includes('- 产物：`out/pricing.md`（created）'), '任务产物')
  const empty = exportMarkdown.groupTimelineMarkdown({ group: { id: 'g2', name: '空群' }, messages: [], exportedAt: 'X' })
  assert(empty.includes('（还没有消息）'), '空群时间线有说明')
  assert(empty.includes('- 消息：0 条'), '空群消息数为 0')
}

// —— 8b) exportMarkdown：上下文发送者名字转义（复审 B2）——
{
  const evilName = '<img src=x onerror=alert(1)>'
  const md = exportMarkdown.groupTimelineMarkdown({
    group: { id: 'grp_x', name: 'X' },
    messages: [{
      seq: 7, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_a' }, text: '回复',
      created_at: 'T', context: { trigger_seqs: [6], senders: [{ name: evilName }] },
    }],
    exportedAt: 'T',
  })
  assert(md.includes('&lt;img src=x onerror=alert\\(1\\)&gt;'), '上下文发送者名字被 HTML + Markdown 转义')
  assert(!md.includes('<img'), '群时间线不含原始 <img 标签')
  const longName = 'N'.repeat(80)
  const capped = exportMarkdown.groupTimelineMarkdown({
    group: { id: 'grp_x', name: 'X' },
    messages: [{
      seq: 8, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_a' }, text: '回复',
      created_at: 'T', context: { senders: [{ name: longName }] },
    }],
    exportedAt: 'T',
  })
  assert(!capped.includes(longName), '过长的发送者名字被截断')
  assert(capped.includes('N'.repeat(20) + '…'), '截断与 escapeInline 同一 cap 语义（60）')
}

// —— 9) exportMarkdown：任务 ——
{
  const task = {
    id: 'task <1>',
    title: '整理 <b>价格</b> *表*',
    status: 'completed',
    attempt: 2,
    origin: { kind: 'group', group_id: 'grp_1', agent_id: 'ag_a', agent_name: 'Alice', message_seq: 7 },
    target: { kind: 'agent', agent_id: 'ag_b', group_id: 'grp_1', workspace: '/w/a b', session_id: 'hub.ag_b.g.grp_1' },
    instruction: '第一行\n第二行 <script>',
    error: 'boom <i>',
    created_at: '2026-03-10T12:00:00Z',
    updated_at: '2026-03-10T12:05:00Z',
    result: {
      summary: '完成 <ok>',
      artifacts: [{ path: 'out/a.md', kind: 'created', lines: 12, size: 345 }],
      usage: { input: 1200, output: 300, cache_read: 50, cost_priced: true, cost_usd: 0.0123 },
    },
  }
  const md = exportMarkdown.taskMarkdown(task, { agentNames: { ag_b: 'Bob' }, exportedAt: '2026-03-10T13:00:00Z' })
  assert(md.startsWith('# 任务：整理 &lt;b&gt;价格&lt;/b&gt; \\*表\\*'), '标题转义且作为任务标题')
  assert(md.includes('- ID：`task &lt;1&gt;`'), '任务 id')
  assert(md.includes('- 状态：completed（已完成）'), '状态原文 + 中文标签')
  assert(md.includes('- 来源：kind=group · 群 `grp_1` · 发起者 Alice · 消息 #7'), '来源摘要')
  assert(md.includes('- 目标：kind=agent · 执行者 Bob · 群 `grp_1` · 工作区 `/w/a b` · 会话 `hub.ag_b.g.grp_1`'), '目标摘要')
  assert(md.includes('- 时间：创建 2026-03-10T12:00:00Z · 更新 2026-03-10T12:05:00Z · 第 2 次尝试'), '时间与尝试次数')
  assert(md.includes('## 指令') && md.includes('> 第二行 &lt;script&gt;'), '指令引用块 + 转义')
  assert(md.includes('## 结果') && md.includes('> 完成 &lt;ok&gt;'), '结果引用块 + 转义')
  assert(md.includes('## 产物') && md.includes('- `out/a.md`（created · 12 行 · 345 B）'), '产物明细')
  assert(md.includes('## 错误') && md.includes('> boom &lt;i&gt;'), '错误引用块 + 转义')
  assert(md.includes('- Tokens：输入 1200 · 输出 300 · 缓存读 50'), '用量 token')
  assert(md.includes('- 成本：$0.0123'), '已计价成本')
  assert(!md.includes('<script>') && !md.includes('<b>'), '任务导出不含可执行 HTML')
  const unpriced = exportMarkdown.taskMarkdown({ id: 't2', status: 'running', result: { usage: { input: 1 } } })
  assert(unpriced.includes('- 成本：未计价'), '未计价明确写「未计价」')
  const bare = exportMarkdown.taskMarkdown({ id: 't3' })
  assert(bare.includes('- 状态：—'), '缺状态时给破折号')
  assert(bare.includes('## 用量') && bare.includes('- 未记录'), '缺用量时说明')
}

// —— 9b) promptScope：提示词预览的作用域键（DM ≠ 群，群 A ≠ 群 B）——
// 钉的是「群作用域永远显示 DM 变体」的成因：缓存键/竞态判定只比 agentId。
{
  const { normalizePromptScope, promptScopeKey, promptScopeMatches } = promptScope
  eq(JSON.stringify(normalizePromptScope(' ag_1 ', undefined)), '{"agentId":"ag_1","groupId":""}', '归一化：agent 去空白，缺省 group = DM 空串')
  eq(normalizePromptScope('ag_1', '   ').groupId, '', '空白 group_id 也是 DM（与后端 TrimSpace 同口径）')
  eq(normalizePromptScope('ag_1', ' grp_1 ').groupId, 'grp_1', '群 id 去空白')

  const dm = { agentId: 'ag_1', groupId: '' }
  const ga = { agentId: 'ag_1', groupId: 'grp_a' }
  const gb = { agentId: 'ag_1', groupId: 'grp_b' }
  assert(promptScopeKey(dm) !== promptScopeKey(ga), '同 agent 的 DM 与群不共用缓存键')
  assert(promptScopeKey(ga) !== promptScopeKey(gb), '群 A 与群 B 不共用缓存键')
  eq(promptScopeKey(ga), promptScopeKey({ agentId: ' ag_1 ', groupId: ' grp_a ' }), '键按归一化后比较（同作用域稳定）')
  eq(promptScopeKey(null), '', '没有 agent 时键为空（不命中任何 state）')
  eq(promptScopeKey({ groupId: 'grp_a' }), '', '只有群没有 agent 也是空键')
  // 用 NUL 分隔而不是 ':'/'/'：群 id 允许自定义，':' 会拼出歧义键。
  assert(
    promptScopeKey({ agentId: 'a:b', groupId: 'c' }) !== promptScopeKey({ agentId: 'a', groupId: 'b:c' }),
    'id 里带分隔符也不会撞键',
  )

  assert(promptScopeMatches(ga, ga), '同 agent 同群命中')
  eq(promptScopeMatches(ga, dm), false, '群预览不命中 DM 作用域')
  eq(promptScopeMatches(dm, ga), false, 'DM 预览不命中群作用域')
  eq(promptScopeMatches(ga, gb), false, '群 A 预览不命中群 B')
  eq(promptScopeMatches({ agentId: 'ag_1' }, dm), true, '老 state 没有 groupId 按 DM 处理（与后端缺省同义）')
  eq(promptScopeMatches({ agentId: 'ag_1' }, ga), false, '老 state 不冒充群预览')
  eq(promptScopeMatches(null, dm), false, '没有 state 不命中')
  eq(promptScopeMatches(ga, null), false, '没有请求作用域不命中')
}

// —— 9c) ContextPanel 接线：群作用域必须把群 id 传下去（源码守卫）——
// 纯函数再准，接线漏一处就回到「群作用域显示 DM 变体」：面板里 loadPromptPreview
// 有两个调用点（进面板的 effect、↻ 重新读取），两处都要带第三个实参 promptGroupId。
// 用源码 grep 钉住（与 test-mobile-hub 的静态约束同一套路，不需要浏览器）。
{
  const src = readFileSync(join(hubDir, 'ContextPanel.tsx'), 'utf8')
  const calls = [...src.matchAll(/loadPrompt\(([^)]*)\)/g)].map((m) => m[1])
  eq(calls.length, 2, 'ContextPanel 有且只有两处 loadPromptPreview 调用（effect + ↻）')
  for (const [i, args] of calls.entries()) {
    const parts = args.split(',').map((s) => s.trim()).filter(Boolean)
    eq(parts.length, 3, `第 ${i + 1} 处调用传三个实参（agent, force, groupID）`)
    eq(parts[0], 'promptAgentId', `第 ${i + 1} 处调用的目标是当前成员`)
    eq(parts[2], 'promptGroupId', `第 ${i + 1} 处调用的第三个实参是 promptGroupId`)
  }
  assert(/const promptGroupId = group\?\.id \?\? ''/.test(src), '群作用域取 group.id，DM 取空串')
  assert(/promptScopeMatches\(prompt, \{ agentId: promptAgentId, groupId: promptGroupId \}\)/.test(src), '只渲染同作用域的预览')
  // P8-01b：依赖数组多了 host 能力位（手机不渲染提示词分层、也不发请求）；
  // 切群/切成员仍必须重新请求 —— 这两个 id 不能从依赖里掉。
  assert(/\}, \[canPromptPreview, promptAgentId, promptGroupId, loadPrompt\]\)/.test(src), '切群（同 agent）也会重新请求')
}

// —— 10) store：深链 / 跳转 / 提示词只读缓存 ——
{
  transport.resetTransport()
  const t = transport.getTransport()
  const counts = new Map()
  const sent = []
  const rawSend = t.send.bind(t)
  // 竞态用例需要**真实乱序**：mock 的 respond 是同步 queueMicrotask，按发送顺序回，
  // 「A 先发先回」压不到迟到守卫（断言会假阳性）。所以把 grp_race_a 的预览请求压后
  // 45ms —— 先发 A、后发 B，A 的响应后到，与「切群后旧响应迟到」同形。
  const RACE_DELAY_MS = 45
  const isLateRaceA = (cmd) => cmd.type === 'hub_agent_prompt_preview' && cmd.payload?.group_id === 'grp_race_a'
  t.send = (cmd) => {
    counts.set(cmd.type, (counts.get(cmd.type) ?? 0) + 1)
    sent.push({ type: cmd.type, payload: cmd.payload ?? {} })
    if (!isLateRaceA(cmd)) return rawSend(cmd)
    return new Promise((resolve) => setTimeout(() => resolve(rawSend(cmd)), RACE_DELAY_MS))
  }
  // 最近一次提示词预览请求的 payload（断言 group_id 有没有带、带的是哪个群）。
  const lastPromptPayload = () => [...sent].reverse().find((c) => c.type === 'hub_agent_prompt_preview')?.payload ?? {}
  const H = store.useHubStore
  H.setState({
    open: false, activeId: null, activeGroupId: null, agents: [], groups: [],
    messages: [], groupMessages: [], hasMore: false, dm: {}, tasks: [], needs: [],
    error: null, notice: null, jump: null, prompt: null, loading: false,
  })

  const bad = await H.getState().openDeepLink('https://group/g1?seq=1')
  eq(bad, false, '无效深链返回 false')
  assert((H.getState().error ?? '').includes('无效'), '无效深链给可见错误')
  assert(!H.getState().open, '无效深链不打开 Hub')

  const okGroup = await H.getState().openDeepLink('hub://group/grp_mock1?seq=3')
  eq(okGroup, true, '群深链打开成功')
  assert(H.getState().open, '深链会打开 Hub')
  eq(H.getState().activeGroupId, 'grp_mock1', '选中群目标')
  assert(H.getState().groupMessages.length >= 5, '群历史已加载')
  const jump = H.getState().jump
  eq(jump?.seq ?? 0, 3, '深链 seq 登记为待跳转')
  eq(jump?.targetId ?? '', 'grp_mock1', '跳转目标限定在该群')

  counts.clear()
  const [a, b] = await Promise.all([
    H.getState().openDeepLink('hub://group/grp_mock1?seq=3'),
    H.getState().openDeepLink('hub://group/grp_mock1?seq=3'),
  ])
  assert(a === true && b === true, '并发重复深链都成功')
  eq(counts.get('hub_group_get') ?? 0, 1, '并发重复深链只打开一次（不重复加载）')

  const missing = await H.getState().openDeepLink('hub://dm/ag_nope?seq=1')
  eq(missing, false, '目标不存在返回 false')
  assert((H.getState().error ?? '').includes('不存在'), '目标不存在给可见错误')

  const okDM = await H.getState().openDeepLink('hub://dm/ag_mockres1?seq=2')
  eq(okDM, true, 'DM 深链打开成功')
  eq(H.getState().activeId, 'ag_mockres1', '选中 DM 目标')
  eq(H.getState().jump?.seq ?? 0, 2, 'DM 深链把已加载的 seq 登记为待跳转')
  eq(H.getState().jump?.targetId ?? '', 'ag_mockres1', 'DM 跳转目标限定在该智能体')
  const okFar = await H.getState().openDeepLink('hub://dm/ag_mockres1?seq=99')
  eq(okFar, true, '目标 seq 超出已加载范围仍打开会话')
  assert((H.getState().notice ?? '').includes('#99'), 'seq 不在已加载历史时如实提示')
  eq(H.getState().jump, null, '没加载到目标 seq 不登记跳转')

  H.getState().requestJump(2)
  const req = H.getState().jump
  eq(req?.targetId ?? '', 'ag_mockres1', 'requestJump 落在当前目标')
  eq(req?.seq ?? 0, 2, 'requestJump 记录已加载的 seq')
  H.getState().clearJump((req?.nonce ?? 0) + 1)
  assert(H.getState().jump, '别的 nonce 不能清掉当前请求')
  H.getState().clearJump(req?.nonce ?? 0)
  eq(H.getState().jump, null, '同一个 nonce 清掉请求')

  const copied = await H.getState().copyDeepLink('dm', 'ag_mockres1', 4)
  eq(typeof copied, 'boolean', '复制深链返回布尔（Node 无剪贴板也不抛）')
  const feedback = `${H.getState().notice ?? ''}${H.getState().error ?? ''}`
  assert(feedback.includes('hub://dm/ag_mockres1?seq=4'), '复制结果把链接给到用户可见处')

  counts.clear()
  await H.getState().loadPromptPreview('ag_mockres1')
  const prompt = H.getState().prompt
  eq(prompt?.layers.length ?? 0, 2, '提示词预览取到 layers')
  assert(prompt?.loaded === true, '提示词预览标记已加载')
  assert((prompt?.note ?? '').includes('L5'), '预览 note 说明 L5 不在预览内')
  eq(counts.get('hub_agent_prompt_preview') ?? 0, 1, '只读预览只请求一次')
  const views = contextView.promptLayerViews(prompt?.layers)
  eq(views.map((l) => l.label).join(','), 'L1,L2', 'mock 预览层带 L 号（real 响应同形：protocol=L1、identity=L2）')
  await H.getState().loadPromptPreview('ag_mockres1')
  eq(counts.get('hub_agent_prompt_preview') ?? 0, 1, '同 agent 复用缓存（切页签不重复请求）')
  await H.getState().loadPromptPreview('ag_mockres1', true)
  eq(counts.get('hub_agent_prompt_preview') ?? 0, 2, 'force 重新读取')

  // 修「群作用域永远显示 DM 变体」：缓存键必须含作用域，群作用域请求必须带 group_id。
  // 前端只认**本次请求**的作用域，不读响应里的 scope/group_id（旧 bridge 不回显也
  // 不影响）—— 下面几条同时钉住「响应缺字段也不崩」。
  counts.clear()
  sent.length = 0
  await H.getState().loadPromptPreview('ag_mockres1', true) // force：确保这次 DM 真的发出去
  eq(counts.get('hub_agent_prompt_preview') ?? 0, 1, 'DM 作用域发一次请求')
  eq(lastPromptPayload().group_id, undefined, 'DM 作用域不带 group_id（旧调用语义不变）')
  eq(H.getState().prompt?.groupId ?? 'x', '', 'DM 作用域的 state 记空 groupId')
  await H.getState().loadPromptPreview('ag_mockres1', false, 'grp_mock1')
  eq(counts.get('hub_agent_prompt_preview') ?? 0, 2, '同 agent 从 DM 切到群必须重新请求（缓存键含作用域）')
  eq(lastPromptPayload().group_id, 'grp_mock1', '群作用域请求带 group_id')
  eq(H.getState().prompt?.groupId ?? '', 'grp_mock1', '群作用域的 state 记群 id')
  eq(H.getState().prompt?.layers.length ?? 0, 2, '群作用域仍取到 layers')
  const groupIdentity = (H.getState().prompt?.layers ?? []).find((l) => l.key === 'identity')?.text ?? ''
  assert(groupIdentity.includes('Writer'), '群作用域的 L2 是群名册（mock 与真 bridge 同形）')
  await H.getState().loadPromptPreview('ag_mockres1', false, 'grp_mock1')
  eq(counts.get('hub_agent_prompt_preview') ?? 0, 2, '同群复用缓存（切页签不重复请求）')
  await H.getState().loadPromptPreview('ag_mockres1', false, 'grp_mock2')
  eq(counts.get('hub_agent_prompt_preview') ?? 0, 3, '群 A → 群 B 重新请求')
  eq(lastPromptPayload().group_id, 'grp_mock2', '切群后请求带新群 id')
  await H.getState().loadPromptPreview('ag_mockres1')
  eq(counts.get('hub_agent_prompt_preview') ?? 0, 4, '群 → DM 重新请求（不拿群的层冒充 DM）')
  eq(lastPromptPayload().group_id, undefined, '切回 DM 的请求不带 group_id')
  const dmIdentity = (H.getState().prompt?.layers ?? []).find((l) => l.key === 'identity')?.text ?? ''
  assert(dmIdentity.includes('Researcher') && !dmIdentity.includes('名册'), '切回 DM 拿回 DM 变体（不是群名册）')

  // 竞态：群 A 的迟到响应不许写进群 B 的面板（判定要比作用域，不是只比 agentId）。
  counts.clear()
  const order = []
  await Promise.all([
    H.getState().loadPromptPreview('ag_mockres1', false, 'grp_race_a').then(() => order.push('a')),
    H.getState().loadPromptPreview('ag_mockres1', false, 'grp_race_b').then(() => order.push('b')),
  ])
  // 夹具自检：A 确实后到。否则「A 先回」时迟到守卫根本没被触发，下面那条是假阳性。
  eq(order.join(','), 'b,a', '夹具自检：群 A 的响应真的后到（不是同步按序回）')
  eq(H.getState().prompt?.groupId ?? '', 'grp_race_b', '迟到的群 A 响应不覆盖群 B')
  eq(counts.get('hub_agent_prompt_preview') ?? 0, 2, '两个作用域各发一次（在飞请求不复用）')
  counts.clear()
  await H.getState().loadPromptPreview('ag_mockres1', false, 'grp_race_a')
  eq(counts.get('hub_agent_prompt_preview') ?? 0, 1, '单槽缓存：切回群 A 重新请求而不是显示 B 的层')
}

// —— 11) 复审行为：深链所有权（B1）与缺失 seq（B3）——
{
  transport.resetTransport()
  const t = transport.getTransport()
  const counts = new Map()
  const rawSend = t.send.bind(t)
  t.send = (cmd) => {
    counts.set(cmd.type, (counts.get(cmd.type) ?? 0) + 1)
    return rawSend(cmd)
  }
  const H = store.useHubStore
  const reset = (patch = {}) => H.setState({
    open: false, activeId: null, activeGroupId: null, agents: [], groups: [],
    messages: [], groupMessages: [], hasMore: false, dm: {}, tasks: [], needs: [],
    error: null, notice: null, jump: null, prompt: null, loading: false,
    deepLinkOwnedOpen: false, ...patch,
  })
  const waitFor = async (cond, ms = 1500) => {
    const started = Date.now()
    while (Date.now() - started < ms) {
      if (cond()) return true
      await new Promise((r) => setTimeout(r, 5))
    }
    return cond()
  }

  // B1-①：Hub 曾开过（群已缓存）→ 关闭 → 深链打开；mount effect 与深链同 tick 竞争。
  reset()
  await H.getState().loadGroups()
  H.setState({
    open: false, activeGroupId: 'grp_mock1', groupMessages: [{ seq: 5, text: '陈旧快照' }],
    jump: null, notice: null, deepLinkOwnedOpen: false,
  })
  counts.clear()
  const pending = H.getState().openDeepLink('hub://group/grp_mock1?seq=3')
  // HubSurface 挂载时调用的就是这一个 store 函数（不是测试自造的旁路）。
  H.getState().resumeOpenTarget()
  const ok = await pending
  eq(ok, true, '缓存后关闭的群深链打开成功')
  eq(H.getState().activeGroupId, 'grp_mock1', '深链选中群目标')
  assert(H.getState().jump && H.getState().jump.seq === 3, '深链跳转没有被 mount 重开作废（B1）')
  eq(H.getState().jump?.targetId ?? '', 'grp_mock1', '跳转归属该群')
  eq(H.getState().notice, null, '不再误报「不在已加载历史」')
  assert(H.getState().groupMessages.length >= 5, '群历史已加载')
  eq(counts.get('hub_group_get') ?? 0, 1, '只打开一次群（mount 没有重复 select）')
  eq(counts.get('hub_group_history') ?? 0, 1, '只拉一次群历史（没有重复请求/循环）')
  eq(H.getState().deepLinkOwnedOpen, true, '深链打开期间持有本次打开的所有权')
  // StrictMode 会双跑 mount effect：第二次 resume 仍然必须跳过（一次性 claim 会在这里翻车）。
  H.getState().resumeOpenTarget()
  await new Promise((r) => setTimeout(r, 30))
  eq(counts.get('hub_group_get') ?? 0, 1, 'StrictMode 双跑 mount 也只有深链一次打开')
  eq(counts.get('hub_group_history') ?? 0, 1, 'StrictMode 双跑 mount 不重复拉历史')
  H.getState().closeHub()
  eq(H.getState().deepLinkOwnedOpen, false, '关闭后所有权释放')

  // B1-②：mount effect 晚于深链完成（claim 必须留到消费，不能靠「在飞」窗口）。
  reset()
  await H.getState().loadGroups()
  H.setState({ open: false, activeGroupId: 'grp_mock1', groupMessages: [], jump: null, notice: null })
  counts.clear()
  await H.getState().openDeepLink('hub://group/grp_mock1?seq=3')
  assert(H.getState().jump && H.getState().jump.seq === 3, '深链完成后跳转在 store 里')
  H.getState().resumeOpenTarget()
  await new Promise((r) => setTimeout(r, 30))
  eq(counts.get('hub_group_get') ?? 0, 1, '迟到的 mount 被 claim 挡住（不重复打开）')
  eq(counts.get('hub_group_history') ?? 0, 1, '迟到的 mount 不重复拉历史')
  assert(H.getState().jump && H.getState().jump.seq === 3, '迟到的 mount 不清掉深链跳转')

  // B1-③：普通重新打开（非深链）仍要 mount 水合 —— claim 不能误伤。
  reset()
  await H.getState().loadGroups()
  H.setState({ open: false, activeGroupId: 'grp_mock1', groupMessages: [], jump: null })
  counts.clear()
  H.getState().openHub()
  H.getState().resumeOpenTarget()
  assert(await waitFor(() => H.getState().groupMessages.length >= 5), '普通打开时 mount 仍重新水合群历史')
  eq(counts.get('hub_group_history') ?? 0, 1, '普通打开 mount 只拉一次历史')

  // B3：缺失 seq 不能留下一条永不消费的 jump。
  reset({ open: true, activeGroupId: 'grp_mock1' })
  H.setState({
    groupMessages: [
      { seq: 1, kind: 'text', sender: { kind: 'human' }, text: 'hi', created_at: 'T' },
      { seq: 2, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_mockres1' }, text: 'yo', created_at: 'T' },
    ],
    jump: null, notice: null,
  })
  H.getState().requestJump(99)
  eq(H.getState().jump, null, '缺失 seq 不登记 jump（B3）')
  assert((H.getState().notice ?? '').includes('#99'), '缺失 seq 给可见提示')
  const view = { stickBottom: true, scrollTop: 0, scrollHeight: 120, clientHeight: 40 }
  const follow = () => {
    if (view.stickBottom && seqJump.canAutoFollow(H.getState().jump, 'grp_mock1')) view.scrollTop = view.scrollHeight
  }
  H.getState().handleEvent(JSON.stringify({
    event_type: 'hub_message', group_id: 'grp_mock1', seq: 3,
    message: { seq: 3, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_mockres1' }, text: '新消息', created_at: 'T' },
  }))
  follow()
  eq(view.scrollTop, view.scrollHeight, '缺失跳转之后新消息仍然贴底（B3）')
  H.getState().requestJump(1)
  eq(H.getState().jump?.seq ?? 0, 1, '后续合法跳转仍然生效')
  eq(seqJump.canAutoFollow(H.getState().jump, 'grp_mock1'), false, '合法跳转让贴底先让位')
  H.getState().clearJump(H.getState().jump.nonce)
  eq(seqJump.canAutoFollow(H.getState().jump, 'grp_mock1'), true, '跳转消费后贴底恢复')
  // 消费方不再立刻 clear（StrictMode 双跑），必须靠 TTL 释放，否则仍会永久挡贴底。
  H.getState().requestJump(2)
  assert(H.getState().jump && H.getState().jump.seq === 2, '再次登记合法跳转')
  await new Promise((r) => setTimeout(r, store.HUB_JUMP_TTL_MS + 150))
  eq(H.getState().jump, null, '跳转 TTL 到期自动释放（B3 的兜底清路）')
  eq(seqJump.canAutoFollow(H.getState().jump, 'grp_mock1'), true, 'TTL 过期后贴底恢复')
  await H.getState().openDeepLink('hub://group/grp_mock1?seq=4')
  eq(H.getState().jump?.seq ?? 0, 4, '缺失跳转之后深链跳转仍能登记')
}

console.log(`\n✅ hub Context/深链/导出：${pass} 项全部通过`)
