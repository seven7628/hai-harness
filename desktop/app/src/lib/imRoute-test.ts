// IM 路由目标（workspace / agent / group）纯逻辑单测（node:test，不启浏览器）。
//
// 这组断言防的回归（审计 B2）：编辑器只有 {chat, workspace} 时，
//   · 列表把 agent/群路由显示成空目标（用户看不出这条消息会被送到哪）；
//   · 「同一 chat 再加一条」按 chat 覆盖 → agent/group 字段被静默抹掉，
//     消息从「驱动智能体」变成「驱动项目会话」，全程没有任何提示。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  derivedRouteOwner,
  makeRoute,
  mergeRouteTarget,
  routeOwner,
  routeTargetComplete,
  routeTargetKind,
  routeTargetLabel,
  routeTargetValue,
  sameChat,
  upsertRoute,
} from './imRoute'
import type { IMRouteCfg } from '../transport/types'

const chat = (id: string) => ({ gateway: 'feishu', chat_id: id })

test('目标三态的读取与展示', () => {
  const ws: IMRouteCfg = { chat: chat('c1'), workspace: '/proj/x' }
  const dm: IMRouteCfg = { chat: chat('c2'), agent: 'ag1' }
  const grp: IMRouteCfg = { chat: chat('c3'), group: 'g1', agent: 'ag1' }

  assert.equal(routeTargetKind(ws), 'workspace')
  assert.equal(routeTargetKind(dm), 'agent')
  assert.equal(routeTargetKind(grp), 'group')

  assert.equal(routeTargetValue(ws), '/proj/x')
  assert.equal(routeTargetValue(dm), 'ag1')
  assert.equal(routeTargetValue(grp), 'g1')

  assert.equal(routeTargetLabel(ws), 'workspace:/proj/x')
  assert.equal(routeTargetLabel(dm), 'agent:ag1')
  assert.equal(routeTargetLabel(grp), 'group:g1')

  assert.equal(routeTargetLabel({ chat: chat('c4') }), '') // 无目标：列表显示「只可删除」
})

test('归属派生：群 > 智能体 > 工作区；显式 owner 优先', () => {
  assert.equal(derivedRouteOwner({ chat: chat('c'), group: 'g1', agent: 'ag1' }), 'group:g1')
  assert.equal(derivedRouteOwner({ chat: chat('c'), agent: 'ag1' }), 'agent:ag1')
  assert.equal(derivedRouteOwner({ chat: chat('c'), workspace: '/p' }), 'workspace:/p')
  assert.equal(routeOwner({ chat: chat('c'), agent: 'ag1' }), 'agent:ag1')
  // 显式且合法 → 用它（服务端要求与目标一致，这里只做展示）
  assert.equal(routeOwner({ chat: chat('c'), agent: 'ag1', owner: 'agent:ag1' }), 'agent:ag1')
  // 非法显式值 → 回落派生，不产生假归属
  assert.equal(routeOwner({ chat: chat('c'), agent: 'ag1', owner: 'global' }), 'agent:ag1')
})

test('目标完整性（群路由必须有执行者）', () => {
  assert.equal(routeTargetComplete({ chat: chat('c'), workspace: '/p' }), true)
  assert.equal(routeTargetComplete({ chat: chat('c'), agent: 'a' }), true)
  assert.equal(routeTargetComplete({ chat: chat('c'), group: 'g' }), false)
  assert.equal(routeTargetComplete({ chat: chat('c'), group: 'g', agent: 'a' }), true)
  assert.equal(routeTargetComplete({ chat: chat('c') }), false)
})

test('编辑同一维度：只改值，其它目标字段保留', () => {
  const grp: IMRouteCfg = { chat: chat('c3'), group: 'g1', agent: 'ag1' }
  const edited = mergeRouteTarget(grp, 'group', 'g2')
  assert.deepEqual(edited, { chat: chat('c3'), group: 'g2', agent: 'ag1' }) // 执行者不丢

  const ws: IMRouteCfg = { chat: chat('c1'), workspace: '/old' }
  assert.deepEqual(mergeRouteTarget(ws, 'workspace', '/new'), { chat: chat('c1'), workspace: '/new' })
})

test('换目标维度：旧维度字段清掉（避免归属派生歧义）', () => {
  const ws: IMRouteCfg = { chat: chat('c1'), workspace: '/p' }
  const toAgent = mergeRouteTarget(ws, 'agent', 'ag9')
  assert.deepEqual(toAgent, { chat: chat('c1'), agent: 'ag9' })
  assert.equal(derivedRouteOwner(toAgent), 'agent:ag9')

  const grp: IMRouteCfg = { chat: chat('c3'), group: 'g1', agent: 'ag1' }
  const toWs = mergeRouteTarget(grp, 'workspace', '/p')
  assert.deepEqual(toWs, { chat: chat('c3'), workspace: '/p' })
})

test('显式 owner 只在仍与目标一致时保留', () => {
  const r: IMRouteCfg = { chat: chat('c2'), agent: 'ag1', owner: 'agent:ag1' }
  // 同维度改值 → 显式 owner 与目标不再一致 → 丢弃（否则服务端 Validate 拒绝整份配置）
  assert.deepEqual(mergeRouteTarget(r, 'agent', 'ag2'), { chat: chat('c2'), agent: 'ag2' })
  // 值没变 → 保留
  assert.deepEqual(mergeRouteTarget(r, 'agent', 'ag1'), { chat: chat('c2'), agent: 'ag1', owner: 'agent:ag1' })
})

test('upsertRoute：按 chat 合并、不产生重复行、不改入参', () => {
  const routes: IMRouteCfg[] = [{ chat: chat('c1'), workspace: '/p' }]
  const next = upsertRoute(routes, makeRoute(chat('c1'), 'agent', 'ag1'))
  assert.equal(next.length, 1)
  assert.equal(derivedRouteOwner(next[0]), 'agent:ag1')
  assert.equal(derivedRouteOwner(routes[0]), 'workspace:/p') // 入参未被改

  const added = upsertRoute(routes, makeRoute(chat('c2'), 'group', 'g1'))
  assert.equal(added.length, 2)
  assert.equal(added[1].group, 'g1')
})

test('sameChat：thread_id 参与比较，空串与缺省等价', () => {
  assert.equal(sameChat(chat('a'), { gateway: 'feishu', chat_id: 'a', thread_id: '' }), true)
  assert.equal(sameChat({ gateway: 'feishu', chat_id: 'a', thread_id: 't1' }, chat('a')), false)
})
