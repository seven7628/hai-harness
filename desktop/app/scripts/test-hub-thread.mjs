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
assert(bubbleSrc.includes('TurnContextLine') && bubbleSrc.includes('compact'), '触发上下文收进消息头，不再单独占一行')
const humanStart = bubbleSrc.indexOf("if (kind === 'human') {")
const humanEnd = bubbleSrc.indexOf('<AgentMessage', humanStart)
assert(humanStart >= 0 && humanEnd > humanStart, '找到 human 分支边界（断言不靠固定窗口）')
const humanBranch = bubbleSrc.slice(humanStart, humanEnd)
assert(humanBranch.includes("msg.visibility === 'owner'"), '人类 owner-only 消息也保留仅你可看标记（human 分支）')
assert(taskSrc.includes('TaskArtifacts') && taskSrc.includes('context?: ReactNode'), '任务卡使用统一产物组件并保留上下文入口')
assert(artifactsSrc.includes("t('artifacts.files')"), '产物标签走 i18n')
assert(cssSrc.includes('container-type: inline-size') && cssSrc.includes('@container'), '产物布局靠容器查询，不跟视口断点走')
assert(mockSrc.includes("visibility: 'owner'") && mockSrc.includes("sender: { kind: 'system' }"), 'mock 覆盖 owner-only 与系统指令两条路径')


// 7) 头像 sigil v2：所有渲染路径都不再用 emoji/首字母；群叠放 +N；i18n 状态 key。
const avatarSrc = readFileSync(join(hubDir, 'avatar.ts'), 'utf8')
const sigilSrc = readFileSync(join(hubDir, 'AgentSigil.tsx'), 'utf8')
const stackSrc = readFileSync(join(hubDir, 'AgentStack.tsx'), 'utf8')
const badgeSrc = readFileSync(join(hubDir, 'AgentBadge.tsx'), 'utf8')
const mentionSrc = readFileSync(join(hubDir, 'AgentMentionChip.tsx'), 'utf8')
const editorSrc = readFileSync(join(hubDir, 'AgentEditor.tsx'), 'utf8')
const sidebarSrc = readFileSync(join(hubDir, 'HubSidebar.tsx'), 'utf8')
const inspectorSrc = readFileSync(join(hubDir, 'HubInspector.tsx'), 'utf8')
const groupSrc = readFileSync(join(hubDir, 'GroupView.tsx'), 'utf8')
const i18nSrc = readFileSync(join(hubDir, '..', 'i18n.ts'), 'utf8')

const count = (haystack, needle) => haystack.split(needle).length - 1
assert(badgeSrc.includes('AgentSigil') && !badgeSrc.includes('avatar.value') &&
  !badgeSrc.includes('name.slice(0, 1)'), 'AgentBadge 用 sigil，不再渲染 emoji/首字母')
assert(mentionSrc.includes('AgentSigil') && !mentionSrc.includes('hub-mention-avatar') &&
  !mentionSrc.includes('hub-mention-dot'), '@ 徽章用 sigil，旧的 emoji 头像/圆点已移除')
assert(editorSrc.includes('AgentSigil') && !editorSrc.includes('setEmoji') &&
  !editorSrc.includes("className=\"hub-avatar\""), '编辑器预览用 sigil，emoji 输入已移除（表单字段保留兼容）')
assert(groupSrc.includes('AgentStack') && !groupSrc.includes('hub-member-dot'),
  '群头成员叠图走 AgentStack，首字母圆点已移除')
assert(sidebarSrc.includes('AgentStack') && !sidebarSrc.includes('hub-member-dot'),
  '侧栏群行走 AgentStack')
assert(inspectorSrc.includes('AgentStack') && inspectorSrc.includes('AgentSigil'),
  '详情页群概览用叠放、成员/智能体用 sigil')
assert(!sidebarSrc.includes('HubAvatar') && !inspectorSrc.includes('HubAvatar') &&
  !groupSrc.includes('MemberStack'), '不再有两套头像实现')
assert(sigilSrc.includes('data-state') && sigilSrc.includes('statusLayer'),
  '状态层由 AgentSigil 统一渲染（data-state 供 CSS 动画/主题）')
assert(avatarSrc.includes('fnv1a(`avatar1|${id}`)') &&
  avatarSrc.includes('fnv1a(`avatar1|${id}|${resp}`)'), '身份散列公式与 demo 逐字一致（avatar1|id / avatar1|id|resp）')
assert(avatarSrc.includes("if (input.pending) return 'needs_attention'") &&
  avatarSrc.includes("if (input.deleted) return 'offline'") &&
  avatarSrc.includes("if (input.running) return 'working'"),
  '状态映射 needs_attention > offline > working > idle')
assert(avatarSrc.includes('input.length >= 5 ? input.slice(0, 3) : input.slice()'),
  'Group 叠放：2–4 全显示、≥5 前 3 +N')
assert(stackSrc.includes('planStack') && stackSrc.includes('agent-stack-more') &&
  stackSrc.includes('plan.more > 0'), 'AgentStack 用 planStack 渲染 +N 徽标')
assert(cssSrc.includes("[data-theme='ink'] .sigil") && cssSrc.includes("[data-theme='paper'] .sigil"),
  'ink/paper 各自一套 sigil 主题值')
assert(cssSrc.includes('prefers-reduced-motion') && cssSrc.includes('animation: none !important'),
  'reduced-motion 下状态动画全部关闭')
assert(!cssSrc.includes('.hub-avatar {') && !cssSrc.includes('.hub-avatar.ring') &&
  !cssSrc.includes('.hub-agent-avatar') && !cssSrc.includes('.hub-member-dot'),
  '旧 .hub-avatar/.hub-agent-avatar/.hub-member-dot 死样式已清理')
assert(count(i18nSrc, "'hub.avatar.state.needs_attention'") === 2 &&
  count(i18nSrc, "'hub.avatar.stack.label'") === 2 &&
  count(i18nSrc, "'hub.avatar.stack.summary.working'") === 2,
  '头像状态词/独立 label/叠放 label 的中英 key 齐备')

console.log(`\n✅ hub 线程纯逻辑：${pass} 项全部通过`)
