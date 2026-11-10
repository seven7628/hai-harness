// owner 归属编码的单测（node:test，不启动 Electron/浏览器）。
//
// 钉住与 Go 侧 owner 包**逐字一致**的规则：DM→agent、群→group、其余→workspace，
// 且 hub 前缀形状不对时返回空（不回落成 workspace，避免把智能体 home 当项目归属）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  LEGACY_OWNER,
  agentOwner,
  groupOwner,
  ownerId,
  ownerKind,
  ownerLabel,
  ownerValid,
  sessionOwner,
  workspaceOwner,
} from './owner'

test('sessionOwner：Hub DM / 群 / 项目会话', () => {
  assert.equal(sessionOwner('hub.abc.dm', '/home/x/.go-code/hub/homes/abc'), 'agent:abc')
  assert.equal(sessionOwner('hub.abc.g.g1', '/home/x/.go-code/hub/homes/abc'), 'group:g1')
  assert.equal(sessionOwner('hub.abc.g.team.eu', ''), 'group:team.eu')
  assert.equal(sessionOwner('s-123', '/proj/x'), 'workspace:/proj/x')
  assert.equal(sessionOwner('cron-1', '/proj/x'), 'workspace:/proj/x')
})

test('sessionOwner：hub 前缀但形状不对 → 空（不回落 workspace）', () => {
  assert.equal(sessionOwner('hub.abc', '/proj/x'), '')
  assert.equal(sessionOwner('hub.abc.g.', ''), '')
})

test('kind / valid / id', () => {
  assert.equal(ownerKind('workspace:/p'), 'workspace')
  assert.equal(ownerKind('agent:a'), 'agent')
  assert.equal(ownerKind('group:g'), 'group')
  assert.equal(ownerKind(LEGACY_OWNER), '')
  assert.equal(ownerKind('workspace:'), '')
  assert.equal(ownerKind('global'), '')
  assert.equal(ownerValid(LEGACY_OWNER), false)
  assert.equal(ownerValid('agent:a'), true)
  assert.equal(ownerId('workspace:/a/b'), '/a/b')
  assert.equal(ownerId('group:g1'), 'g1')
  assert.equal(ownerId('nope'), '')
})

test('DM 与群不共享归属（同一 agent 两个 owner）', () => {
  const dm = sessionOwner('hub.a.dm', '')
  const group = sessionOwner('hub.a.g.g1', '')
  assert.notEqual(dm, group)
  assert.equal(dm, agentOwner('a'))
  assert.equal(group, groupOwner('g1'))
})

test('空值不产生归属', () => {
  assert.equal(workspaceOwner(''), '')
  assert.equal(workspaceOwner('   '), '')
  assert.equal(agentOwner(''), '')
  assert.equal(groupOwner('  '), '')
})

test('ownerLabel 展示（不参与判定）', () => {
  assert.match(ownerLabel('agent:a'), /智能体 a/)
  assert.match(ownerLabel('group:g'), /群聊 g/)
  assert.match(ownerLabel('workspace:/p'), /项目会话/)
  assert.equal(ownerLabel(''), '未绑定归属')
  assert.equal(ownerLabel(LEGACY_OWNER), '迁移前的全局信任')
})
