#!/usr/bin/env node
// P3a-08 前端 hub 纯逻辑验收（Node 直跑，无需浏览器）：
//   upsertRound 去重/排序/空 round；buildThreadItems 把动作行插到 trigger_seq 之后；
//   roundKey 的 round_id/trigger_seqs 优先级；routingSummary 按名册还原且不泄漏 targets=ag_。
// 这些函数是直播视图与历史回执一致性的唯一分叉点，值得有独立回归。
import { build } from 'esbuild'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const entry = join(__dirname, '..', 'src', 'hub', 'groupThread.ts')
const result = await build({
  entryPoints: [entry],
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
})
const source = result.outputFiles[0].text
const mod = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

const round = (id, seq, targets = ['ag_a'], extra = {}) => ({ id, trigger_seq: seq, targets, ...extra })
const msg = (seq, extra = {}) => ({ seq, sender: { kind: 'agent', agent_id: 'ag_a' }, text: `m${seq}`, ...extra })

// 1) upsertRound：空 round 跳过、同 id 覆盖、按 trigger_seq 排序。
let rounds = []
rounds = mod.upsertRound(rounds, round('r2', 20))
rounds = mod.upsertRound(rounds, round('r1', 10))
rounds = mod.upsertRound(rounds, round('empty', 30, []))
assert(rounds.map((r) => r.id).join(',') === 'r1,r2', '空 round 不入列，按 trigger_seq 排序')
rounds = mod.upsertRound(rounds, round('r1', 10, ['ag_a', 'ag_b']))
assert(rounds.length === 2 && rounds[0].targets.length === 2, '同 id upsert 覆盖旧值而不是追加')

// 2) buildThreadItems：动作行紧跟 trigger_seq 消息；无 @ 的 round 不插。
const items = mod.buildThreadItems(
  [msg(10), msg(11), msg(20)],
  [round('r1', 10), round('r2', 20), round('empty', 11, [])],
)
assert(items.map((i) => i.kind).join(',') === 'message,round,message,message,round', '动作行插到各自 trigger_seq 之后')
assert(items.filter((i) => i.kind === 'round').length === 2, '空 round 不产生动作行')

// 3) N3：同一轮有路由动作行时，紧跟的 routing_receipt 不重复渲染。
const deduped = mod.buildThreadItems(
  [msg(10), { ...msg(11), kind: 'routing_receipt', text: 'targets=ag_a' }, msg(12)],
  [round('r1', 10)],
)
assert(deduped.filter((i) => i.kind === 'round').length === 1, '路由动作行只出现一次')
assert(deduped.filter((i) => i.kind === 'message' && i.msg.kind === 'routing_receipt').length === 0, '对应轮次的 receipt 不再渲染')
const standalone = mod.buildThreadItems(
  [{ ...msg(20), kind: 'routing_receipt', text: 'targets=ag_a' }],
  [],
)
assert(standalone.length === 1 && standalone[0].msg.kind === 'routing_receipt', '没有 round 时 receipt 仍保留')

// 4) roundKey：round_id 优先于 context.trigger_seqs；都没有为空。
assert(mod.roundKey(msg(1, { round_id: 'r9', context: { trigger_seqs: [3] } })) === 'r:r9', 'round_id 优先')
assert(mod.roundKey(msg(1, { context: { trigger_seqs: [3, 4] } })) === 't:3,4', '退化为 trigger_seqs')
assert(mod.roundKey(msg(1)) === '', '普通消息无轮次键')

// 5) routingSummary：按名册还原，不出现 ag_/targets=。
const names = { ag_a: 'Alice', ag_b: 'Bob' }
assert(mod.routingSummary('targets=ag_a,ag_b', names) === '唤醒 Alice、Bob', '路由回执按名还原')
assert(mod.routingSummary('@all targets=ag_a dropped=2', names) === '@all 唤醒 Alice（丢弃 2 个）', '@all + dropped')
assert(mod.routingSummary('targets=', names) === '无人被唤醒', '空目标可读')
assert(mod.routingSummary('自由文本', names) === '自由文本', '非回执文本原样返回')

// 6) 群消息版式约束：同轮只收紧间距，每条消息独立；作者标识与产物组件不能被拆回去。
const hubDir = join(__dirname, '..', 'src', 'hub')
const roundSrc = readFileSync(join(hubDir, 'RoundGroup.tsx'), 'utf8')
const bubbleSrc = readFileSync(join(hubDir, 'MessageBubble.tsx'), 'utf8')
const taskSrc = readFileSync(join(hubDir, 'TaskResultCard.tsx'), 'utf8')
const artifactsSrc = readFileSync(join(hubDir, 'TaskArtifacts.tsx'), 'utf8')
const mockSrc = readFileSync(join(hubDir, '..', 'transport', 'mock.ts'), 'utf8')
const cssSrc = readFileSync(join(hubDir, 'hub.css'), 'utf8')
assert(!roundSrc.includes('hub-round-group') && roundSrc.includes('hub-round-stack'), '同轮不再画外框/表头，只保留无边框间距栈')
assert(bubbleSrc.includes('hub-group-msg') && bubbleSrc.includes('AgentMessage') && bubbleSrc.includes('UserMessage'), '每条群消息独立版式并带作者头（agent 无气泡 / human 右侧气泡）')
// 长正文折叠壳必须把 streaming 透传给 Markdown：吞掉它 = 流式正文按整段重解析、
// 半截 HTML 直接挂 iframe（2026-11 回归）。浏览器侧的同类断言见 check-hub-collapse.mjs。
const collapseSrc = readFileSync(join(hubDir, 'CollapsibleMarkdown.tsx'), 'utf8')
const collapseCode = collapseSrc.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '')
assert(collapseCode.includes('<Markdown text={text} streaming={streaming}'), '折叠壳把 streaming 透传给 Markdown')
assert(collapseCode.split('streaming={streaming}').length - 1 === 1, '只有折叠壳那一处传 streaming（弹窗里不传）')
assert(collapseCode.includes('> COLLAPSE_HEIGHT') && !collapseCode.includes('COLLAPSE_HEIGHT + 1'), '折叠阈值是 > COLLAPSE_HEIGHT（419 不折 / 421 折）')
assert(bubbleSrc.includes('TurnContextLine') && bubbleSrc.includes('compact'), '触发上下文收进消息头，不再单独占一行')
const humanStart = bubbleSrc.indexOf("if (kind === 'human') {")
const humanEnd = bubbleSrc.indexOf('<AgentMessage', humanStart)
assert(humanStart >= 0 && humanEnd > humanStart, '找到 human 分支边界（断言不靠固定窗口）')
const humanBranch = bubbleSrc.slice(humanStart, humanEnd)
assert(humanBranch.includes("msg.visibility === 'owner'"), '人类 owner-only 消息也保留仅你可看标记（human 分支）')
assert(taskSrc.includes('TaskArtifacts') && taskSrc.includes('context?: ReactNode'), '任务卡使用统一产物组件并保留上下文入口')
assert(artifactsSrc.includes("t('artifacts.aria')") && artifactsSrc.includes("artifacts.created"), '产物标签走 i18n')
assert(cssSrc.includes('.event-artifacts') && cssSrc.includes('.artifact-line'), '产物走 demo 的 artifact-line 解剖')
assert(mockSrc.includes("visibility: 'owner'") && mockSrc.includes("sender: { kind: 'system' }"), 'mock 覆盖 owner-only 与系统指令两条路径')


// 7) 头像：demo 的 Luna 引擎（mascot-bots.js + luna-face.js），不再有 sigil v2 / emoji。
const lunaSrc = readFileSync(join(hubDir, 'lunaAvatar.ts'), 'utf8')
const sigilSrc = readFileSync(join(hubDir, 'AgentSigil.tsx'), 'utf8')
const stackSrc = readFileSync(join(hubDir, 'AgentStack.tsx'), 'utf8')
const badgeSrc = readFileSync(join(hubDir, 'AgentBadge.tsx'), 'utf8')
const mentionSrc = readFileSync(join(hubDir, 'AgentMentionChip.tsx'), 'utf8')
const editorSrc = readFileSync(join(hubDir, 'AgentEditor.tsx'), 'utf8')
const sidebarSrc = readFileSync(join(hubDir, 'HubSidebar.tsx'), 'utf8')
const hubSurfaceSrc = readFileSync(join(hubDir, 'HubSurface.tsx'), 'utf8')
const inspectorSrc = readFileSync(join(hubDir, 'HubInspector.tsx'), 'utf8')
const groupSrc = readFileSync(join(hubDir, 'GroupView.tsx'), 'utf8')
const convoSrc = readFileSync(join(hubDir, 'ConversationMessage.tsx'), 'utf8')
const i18nSrc = readFileSync(join(hubDir, '..', 'i18n.ts'), 'utf8')
const demoCss = readFileSync(join(hubDir, 'demo.css'), 'utf8')

const count = (haystack, needle) => haystack.split(needle).length - 1
assert(sigilSrc.includes('data-luna-face') && sigilSrc.includes('retainLunaScene') && sigilSrc.includes('releaseLunaScene') &&
  lunaSrc.includes("import './vendor/mascot-bots.js'") && lunaSrc.includes("import './vendor/luna-face.js'"),
  '头像直接复用 demo 的 Luna 引擎（vendor 脚本 + data-luna-face）')
assert(lunaSrc.includes('ROLE_RULES') && lunaSrc.includes('inspecting') &&
  lunaSrc.includes('surprised') && lunaSrc.includes('sleepy'),
  'demo 的职责配色与状态表情映射原样保留')
assert(!sigilSrc.includes('statusLayer') && !sigilSrc.includes('SHAPES['),
  'AgentSigil 不再自绘几何 sigil')
assert(stackSrc.includes('planStack') && stackSrc.includes('avatar-more') &&
  stackSrc.includes('plan.more > 0'), 'AgentStack 用 planStack 渲染 +N 徽标')
assert(cssSrc.includes('.luna-avatar') && demoCss.includes('.hub-demo .avatar'),
  'demo 头像样式已纳入 hub.css + demo.css')
assert(demoCss.includes('.hub-demo .avatar') && demoCss.includes('.hub-demo .msg-row') &&
  demoCss.includes('.hub-demo .composer'), 'demo DOM/CSS 已按 .hub-demo 作用域原样移植')
assert(badgeSrc.includes('AgentSigil') && !badgeSrc.includes('name.slice(0, 1)'),
  'AgentBadge 用 Luna 头像，不再渲染 emoji/首字母')
assert(mentionSrc.includes('AgentSigil') && !mentionSrc.includes('hub-mention-dot'),
  '@ 徽章用 Luna 头像，旧圆点已移除')
assert(editorSrc.includes('AgentSigil') && !editorSrc.includes('setEmoji'),
  '编辑器预览用 Luna 头像，emoji 输入已移除（表单字段保留兼容）')
assert(groupSrc.includes('AgentStack') && !groupSrc.includes('hub-member-dot'),
  '群头成员叠图走 AgentStack，首字母圆点已移除')
assert(sidebarSrc.includes('AgentStack') && sidebarSrc.includes('size={44}'),
  '侧栏单人头像按 demo 用 44px')
assert(inspectorSrc.includes('AgentStack') && inspectorSrc.includes('AgentSigil'),
  '详情页群概览用叠放、成员/智能体用 Luna 头像')
assert(!sidebarSrc.includes('HubAvatar') && !inspectorSrc.includes('HubAvatar') &&
  !groupSrc.includes('MemberStack'), '不再有两套头像实现')
assert(convoSrc.includes('size={52}') && convoSrc.includes('agent-head') &&
  convoSrc.includes('agent-text') && convoSrc.includes('user-bubble'),
  '消息区按 demo 的 52px 头像 + agent-head/agent-text/user-bubble 结构')
assert(hubSurfaceSrc.includes('size={52}') && groupSrc.includes('<AgentStack') &&
  groupSrc.includes('size={28}'), 'DM 头 52 / 群头叠放 28（demo 尺寸）')
assert(cssSrc.includes('.hub-demo .messages') && cssSrc.includes('.hub-demo .chat-header'),
  '会话区采用 demo 的 chat-header/messages/composer 布局')
assert(cssSrc.includes('prefers-reduced-motion') && cssSrc.includes('animation: none !important'),
  'reduced-motion 下状态动画全部关闭')
assert(!cssSrc.includes('.hub-avatar {') && !cssSrc.includes('.hub-agent-avatar') &&
  !cssSrc.includes('.hub-member-dot'), '旧 .hub-avatar/.hub-agent-avatar/.hub-member-dot 死样式已清理')
assert(hubSurfaceSrc.includes('agentSigilLabel(t, activeAgent.name, activeState)'),
  'DM ChatHeader 独立头像带名字+状态 aria-label')
assert(inspectorSrc.includes('agentSigilLabel(t, agent.name, agentState)'),
  '详情 hero 独立头像同样带名字+状态 aria-label')
assert(groupSrc.includes('name: group.name') && inspectorSrc.includes('name: group.name'),
  '群叠放 aria-label 含群名（GroupView / 详情 hero）')
assert(count(i18nSrc, "'hub.avatar.state.needs_attention'") === 2 &&
  count(i18nSrc, "'hub.avatar.stack.label'") === 2 &&
  count(i18nSrc, "'hub.avatar.stack.summary.working'") === 2 &&
  count(i18nSrc, "'hub.avatar.stack.labelNamed'") === 2,
  '头像状态词/独立 label/叠放 label 的中英 key 齐备')

console.log(`\n✅ hub 线程纯逻辑：${pass} 项全部通过`)
