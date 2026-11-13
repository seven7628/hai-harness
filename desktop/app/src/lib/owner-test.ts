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
import {
  getHubOwnerContext,
  ownerForContext,
  resetHubOwnerContext,
  resolveActiveOwner,
  setHubOwnerContext,
} from './activeOwner'

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

// ── 当前上下文的归属（B1：Hub DM/群 与 项目会话） ────────────────────────────
//
// 改造前 send() 只读 useAppStore.activeSessionId —— Hub 打开时它是项目会话（或空），
// 于是 Hub 里发出的 hooks_trust / plugin_enable 被写成 workspace:<工作区>，而派发按
// agent:/group: 查 → 确认了却不生效。这组断言钉住「上下文 → 归属」的唯一派生。

const HUB_CLOSED = { open: false, sid: null, groupId: null }

test('ownerForContext：Hub DM → agent，Hub 群 → group', () => {
  assert.equal(
    ownerForContext({ open: true, sid: 'hub.ag1.dm', groupId: null }, 's-project', '/proj/x'),
    'agent:ag1',
  )
  assert.equal(
    ownerForContext({ open: true, sid: null, groupId: 'grp1' }, 's-project', '/proj/x'),
    'group:grp1',
  )
  // 群的 sid（hub.<agent>.g.<group>）若也拿来算，结果必须仍是 group（不是 agent）
  assert.equal(
    ownerForContext({ open: true, sid: 'hub.ag1.g.grp1', groupId: 'grp1' }, '', ''),
    'group:grp1',
  )
})

test('ownerForContext：Hub 关闭 → 工作区会话归属', () => {
  assert.equal(ownerForContext(HUB_CLOSED, 's-1', '/proj/x'), 'workspace:/proj/x')
  assert.equal(ownerForContext(HUB_CLOSED, 'hub.ag9.dm', ''), 'agent:ag9') // 残留 hub 会话 id 仍按 hub 规则
})

test('ownerForContext：Hub 打开但没选中 → 空（不猜成 workspace）', () => {
  assert.equal(ownerForContext({ open: true, sid: null, groupId: null }, 's-1', '/proj/x'), '')
  // 非 hub 形状的 sid：未绑定，而不是回落项目归属
  assert.equal(ownerForContext({ open: true, sid: 's-project', groupId: null }, 's-project', '/proj/x'), '')
  // 形状不对的 hub id（缺 .dm / 空群）→ 空
  assert.equal(ownerForContext({ open: true, sid: 'hub.ag1', groupId: null }, '', ''), '')
  assert.equal(ownerForContext({ open: true, sid: 'hub.ag1.g.', groupId: null }, '', ''), '')
})

test('setHubOwnerContext / resolveActiveOwner：非 React 路径读同一份上下文', () => {
  resetHubOwnerContext()
  assert.equal(resolveActiveOwner('s-1', '/proj/x'), 'workspace:/proj/x')
  setHubOwnerContext({ open: true, sid: 'hub.ag1.dm', groupId: null })
  assert.equal(resolveActiveOwner('s-1', '/proj/x'), 'agent:ag1')
  setHubOwnerContext({ open: true, sid: null, groupId: 'grp1' })
  assert.equal(resolveActiveOwner('s-1', '/proj/x'), 'group:grp1')
  setHubOwnerContext({ open: false, sid: null, groupId: null })
  assert.equal(resolveActiveOwner('s-1', '/proj/x'), 'workspace:/proj/x')
  resetHubOwnerContext()
  assert.deepEqual(getHubOwnerContext(), HUB_CLOSED)
})
