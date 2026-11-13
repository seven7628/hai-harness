// P4-01 长会话基准的确定性数据（lab 专用，不进生产包）。
//
// 目的：200+ 条群消息 + 2~3 条并行流式草稿，形状接近真实长会话（同轮回复折叠、
// 路由动作行、@提及、列表/表格/引用都有），让 bench 数字可重复、可前后对比。
// 不使用代码块：shiki 高亮是异步的，会把「首屏/每次 delta」的测量掺进 chunk 加载。
import type { HubStreamDraft } from '../hub/groupStream'
import type { HubAgent, HubGroup, HubGroupActivity, HubMessage, HubRound } from '../hub/types'

export const HUB_LONG_MESSAGE_TARGET = 220
export const HUB_LONG_WORKERS = 3

const GROUP_ID = 'grp_long'
const BASE_AT = Date.UTC(2026, 0, 2, 9, 0, 0)

const AGENTS: HubAgent[] = [
  { id: 'ag_alice', name: 'Alice', role: '研究员', provider: 'anthropic', model: 'claude-opus', accent: '#c25b3a' },
  { id: 'ag_bob', name: 'Bob', role: '实现', provider: 'anthropic', model: 'claude-sonnet', accent: '#3a6fc2' },
  { id: 'ag_carol', name: 'Carol', role: '审查', provider: 'openai', model: 'gpt-5', accent: '#3a9c74' },
]

const TOPICS = [
  '登录链路的 JWT 过期分支',
  '结算状态机幂等键',
  '搜索索引重建的窗口期',
  '通知重试的退避曲线',
  '权限中间件去重',
  '订单导出的并发上限',
  '群消息路由的迟到回执',
  '长会话压缩阈值',
  '工具行折叠后的可读性',
  '弱网下草稿与 final 的合并',
]

const DRAFT_SEEDS = [
  '先对一下登录链路：过期分支、刷新竞态、以及回退路径，我按顺序核。',
  '我这边在跑结算状态机的幂等用例，先给结论：重试键要绑定 client_msg_id。',
  '审查视角：先看弱网顺序，再看长会话里有没有重复挂载。',
]

function lcg(seed: number): () => number {
  let s = seed >>> 0
  return () => {
    s = (Math.imul(s, 1664525) + 1013904223) >>> 0
    return s / 0x1_0000_0000
  }
}

function at(seq: number): string {
  return new Date(BASE_AT + seq * 61_000).toISOString()
}

function utf8Bytes(text: string): number {
  return new TextEncoder().encode(text).length
}

function agentText(round: number, k: number, rand: () => number): string {
  const topic = TOPICS[(round + k) % TOPICS.length]
  const variant = (round + k) % 5
  if (variant === 0) {
    return [
      `**${topic}** 的结论：先修可观测性，再动合并逻辑。`,
      '',
      `第一段：把 \`hub_group_get\` 的返回按 seq 合并，别拿快照覆盖已到的消息；` +
        `实测漏一条 final 就会留下半截草稿，${Math.round(rand() * 40 + 20)}ms 后才被下一轮顶掉。`,
      '',
      `第二段：路由回执和动作行只保留一条，否则群里会出现两条「唤醒 ${AGENTS[(round + 1) % AGENTS.length].name}」。`,
    ].join('\n')
  }
  if (variant === 1) {
    return [
      `### ${topic} 复盘`,
      '',
      `- 触发：@${AGENTS[(round + 2) % AGENTS.length].name} 的路由在 ${Math.round(rand() * 300 + 80)}ms 内完成`,
      '- 现象：同轮回复没有折叠，时间线多出 3 个气泡',
      '- 结论：round_id 缺失时按 trigger_seqs 归并，单条保持平铺',
      `- 下一步：补一条弱网顺序用例（final 先到、delta 后到）`,
    ].join('\n')
  }
  if (variant === 2) {
    return [
      `> ${topic}：先量再改，别凭手感加虚拟列表。`,
      '',
      '1. 记录首屏、每次 delta、键入延迟、滚动掉帧、DOM 节点数；',
      '2. 只修测出来的瓶颈，改动保持可回滚；',
      `3. @${AGENTS[(round + 1) % AGENTS.length].name} 复核弱网不变量：final 恒赢、迟到 delta 丢弃。`,
      '',
      `补充：输入框每敲一个字都会重渲染整页，${Math.round(rand() * 20 + 8)} 条消息时肉眼可见。`,
    ].join('\n')
  }
  if (variant === 3) {
    return [
      `| 指标 | 实测 | 预算 |`,
      `| --- | --- | --- |`,
      `| 首屏挂载 | ${Math.round(rand() * 180 + 180)}ms | < 400ms |`,
      `| 每次 delta | ${Math.round(rand() * 9 + 4)}ms | < 8ms |`,
      `| 键入延迟 | ${Math.round(rand() * 40 + 20)}ms | < 50ms |`,
      '',
      `${topic} 的表格先放这里；数字来自 mock，不代表真机。`,
    ].join('\n')
  }
  return `短回复（${topic}）：收到，${Math.round(rand() * 30 + 5)} 分钟内给结果。@${AGENTS[(round + 2) % AGENTS.length].name} 先别动合并逻辑。`
}

function humanText(round: number, rand: () => number): string {
  const topic = TOPICS[round % TOPICS.length]
  if (round % 4 === 0) {
    return `这轮先看 ${topic}。@${AGENTS[round % AGENTS.length].name} 给结论，@${AGENTS[(round + 1) % AGENTS.length].name} 补数据。`
  }
  if (round % 4 === 1) {
    return `继续：把 ${topic} 的失败路径也列出来，别只写 happy path。`
  }
  if (round % 4 === 2) {
    return `我贴一下现场：弱网重连后草稿还在，final 没到；先确认是不是只丢了 delta。`
  }
  return `预算还剩 ${Math.round(rand() * 10 + 2)} 美元，今天把这轮收掉。`
}

export interface HubLongFixture {
  group: HubGroup
  agents: HubAgent[]
  messages: HubMessage[]
  rounds: HubRound[]
  activity: HubGroupActivity[]
  initialDrafts: HubStreamDraft[]
}

export function buildHubLongFixture(): HubLongFixture {
  const rand = lcg(0x5eed_1234)
  const group: HubGroup = {
    id: GROUP_ID,
    name: '长会话性能群',
    owner_agent_id: 'ag_alice',
    members: AGENTS.map((a) => ({ agent_id: a.id, role: a.id === 'ag_alice' ? 'owner' : 'member' })),
    policy: { mode: 'auto', daily_budget_usd: 30, max_mentions_per_message: 3 },
    created_at: at(0),
  }

  const messages: HubMessage[] = []
  const rounds: HubRound[] = []
  let seq = 0
  let round = 0
  while (messages.length < HUB_LONG_MESSAGE_TARGET) {
    seq++
    const triggerSeq = seq
    messages.push({
      seq,
      id: `m${seq}`,
      kind: 'text',
      sender: { kind: 'human' },
      text: humanText(round, rand),
      created_at: at(seq),
    })
    const replies = 2 + (round % 2)
    for (let k = 0; k < replies && messages.length < HUB_LONG_MESSAGE_TARGET; k++) {
      seq++
      const agent = AGENTS[(round + k) % AGENTS.length]
      messages.push({
        seq,
        id: `m${seq}`,
        kind: 'text',
        sender: { kind: 'agent', agent_id: agent.id },
        text: agentText(round, k, rand),
        round_id: `rnd_${round}`,
        context: {
          trigger_seqs: [triggerSeq],
          // 每 5 轮造一条「多人触发」：TurnContextLine 只在 ≥2 个不同 sender 时出现，
          // 基准里要能验证上下文 chip 跳转/高亮没有被时间线记忆化弄坏。
          senders: round % 5 === 0 && k === 0
            ? [
                { agent_id: AGENTS[0].id, name: AGENTS[0].name },
                { agent_id: AGENTS[1].id, name: AGENTS[1].name },
              ]
            : [{ agent_id: agent.id, name: agent.name }],
        },
        created_at: at(seq),
      })
    }
    if (round % 3 === 0) {
      rounds.push({
        id: `route_${round}`,
        group_id: GROUP_ID,
        trigger_seq: triggerSeq,
        targets: [AGENTS[(round + 1) % AGENTS.length].id, AGENTS[(round + 2) % AGENTS.length].id],
        dropped: round % 9 === 0 ? 1 : 0,
      })
    }
    round++
  }

  const activity: HubGroupActivity[] = []
  for (let i = 0; i < 40; i++) {
    const msg = messages[Math.min(messages.length - 1, i * 5 + 2)]
    activity.push({
      group_id: GROUP_ID,
      agent_id: AGENTS[i % AGENTS.length].id,
      after_seq: msg?.seq ?? i,
      kind: i % 4 === 0 ? 'routing' : 'tool',
      name: i % 4 === 0 ? 'route' : 'bash',
      status: i % 7 === 0 ? 'error' : 'ok',
      summary: i % 4 === 0 ? `targets=${AGENTS[i % AGENTS.length].id}` : `go test ./... (${i + 1})`,
      ms: 120 + i * 17,
      at: at(i),
    })
  }

  const initialDrafts: HubStreamDraft[] = AGENTS.map((agent, i) => {
    const text = DRAFT_SEEDS[i] ?? ''
    return {
      messageId: `stream_${agent.id}`,
      groupId: GROUP_ID,
      agentId: agent.id,
      roundId: `rnd_${Math.max(0, round - 1)}`,
      text,
      index: 0,
      bytes: utf8Bytes(text),
      updatedAt: 0,
    }
  })

  return { group, agents: AGENTS, messages, rounds, activity, initialDrafts }
}
