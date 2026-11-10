#!/usr/bin/env node
// 会话区版式验收（Node 直跑，无需浏览器）。
//
// 纯函数：conversationDay.dayKey/dayLabel（日期分隔线）+ conversation.formatClockTime；
// 静态契约：DM/群共用 ConversationMessage 的两种版式（右侧暖气泡 / 左侧无气泡作者头）、
//           群头 AgentStack + 两行 subtitle、DM 头 48px sigil + 只读提示、composer meta 行、
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

// 3) 消息版式：DM 与群共用同两个组件。
{
  const conv = readFileSync(join(APP, 'src', 'hub', 'ConversationMessage.tsx'), 'utf8')
  const css = readFileSync(join(APP, 'src', 'hub', 'hub.css'), 'utf8')
  const cssCode = css.replace(/\/\*[\s\S]*?\*\//g, '')
  assert(/\.hub-msg\.agent\s*\{[^}]*grid-template-columns:\s*32px/.test(css), 'agent 行是 32px + 正文两列')
  assert(/\.hub-msg-avatar::after\s*\{[^}]*inset:\s*-6px/.test(css), '头像命中区扩到 44px（32 + 6×2）')
  assert(/\.hub-user-bubble\s*\{[^}]*border-radius:\s*18px/.test(css), '用户气泡 18px 圆角')
  assert(/\.hub-user-bubble\s*\{[^}]*padding:\s*10px 14px/.test(css), '用户气泡 10/14 内边距')
  assert(/--hub-user-bubble/.test(cssCode) && /\[data-theme='paper'\]\s*\{[^}]*--hub-user-bubble/s.test(cssCode) && /:root,\s*\n\[data-theme='ink'\]\s*\{[^}]*--hub-user-bubble/s.test(cssCode), '--hub-user-bubble 两套主题都定义')
  assert(!/#f0e8dc|#efe4d9|#f0e8dc/i.test(cssCode), '没有照抄 demo 的硬编码暖色')
  assert(/--hub-conv-max:\s*760px/.test(css), '会话列 760px')
  assert(/\.hub-composer\s*\{[^}]*border-radius:\s*19px/.test(css), 'composer 19px 圆角')
  assert(/\.hub-event-icon\s*\{[^}]*width:\s*29px/.test(css), '事件卡 29px 图标块')
  assert(/\.hub-run-icon\s*\{[^}]*width:\s*23px/.test(css), '工具行 23px 图标块 / 12px 圆角')
}

{
  const bubble = readFileSync(join(APP, 'src', 'hub', 'MessageBubble.tsx'), 'utf8')
  const bubbleCode = bubble.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '')
  assert(bubble.includes("from './ConversationMessage'") && bubble.includes('<AgentMessage') && bubble.includes('<UserMessage'), '群消息复用 ConversationMessage 的两种版式')
  assert(!/hub-msg agent[\s\S]{0,400}hub-body/.test(bubble), '群 agent 消息不再自带气泡容器')
  assert(bubble.includes('groupRole === \'observer\'') && bubble.includes("t('hub.observer')"), '观察标签只在 groupRole=observer 时出现')
  assert(!bubbleCode.includes('回复 @'), '没有 reply_to 数据时不画「回复 @」')
  assert(bubble.includes('hub-event--handoff') && bubble.includes('hub-event--approval'), '交接/审批走 event-card 解剖')

  const stream = readFileSync(join(APP, 'src', 'hub', 'StreamingMessage.tsx'), 'utf8')
  assert(stream.includes('<AgentMessage') && !stream.includes('hub-msg-head'), '流式消息与最终消息同一版式')

  const dm = readFileSync(join(APP, 'src', 'hub', 'HubSurface.tsx'), 'utf8')
  assert(/size=\{48\}/.test(dm) && dm.includes("t('hub.dm.locked')"), 'DM 头 48px sigil + 能力只读提示')
  assert(dm.includes('<AgentMessage') && dm.includes('<UserMessage'), 'DM 消息复用同一版式（作者头存在）')
  assert(dm.includes('hub-composer-meta') && dm.includes('hub.composer.enter'), 'DM composer 有 meta 行')
  assert(!dm.includes('hub-composer-plus') && !dm.includes('hub-composer-pill'), '旧 composer 结构已移除')

  const group = readFileSync(join(APP, 'src', 'hub', 'GroupView.tsx'), 'utf8')
  assert(group.includes('<AgentStack') && /size=\{32\}/.test(group), '群头 AgentStack（+N 由 planStack 决定）')
  assert(group.includes("t('hub.group.members')") && group.includes("t('hub.group.mode')") && group.includes("t('hub.group.owner')"), '群头 subtitle：成员数 / 模式 / owner')
  assert(group.includes('hub-conv-names'), '群头第二行：成员名')
  assert(group.includes('DayDivider') && group.includes('dayLabel('), '群时间线有日期分隔线')
  assert(group.includes('insertMentionToken') && group.includes("t('hub.composer.mention')"), '@ 按钮真实插入并聚焦')
  assert(group.includes('hub-composer-meta'), '群 composer 有 meta 行')
}

// 4) 群头 +N 规则（AgentStack 行为不改，只消费）：≥5 人 = 前 3 + N。
{
  const avatar = await load('avatar.ts')
  const members = Array.from({ length: 6 }, (_, i) => ({ id: `agt_${i}`, name: `A${i}`, state: 'idle' }))
  const plan = avatar.planStack(members)
  eq(plan.members.length, 3, '6 人群头只画 3 个 sigil')
  eq(plan.more, 3, '6 人群头显示 +3')
  const two = avatar.planStack(members.slice(0, 2))
  eq(two.members.length, 2, '2 人群头全部画出')
  eq(two.more, 0, '2 人群头没有 +N')
}

console.log(`\nHUB_CONV_TEST_PASS (${pass})`)
