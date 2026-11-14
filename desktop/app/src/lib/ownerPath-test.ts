// ownerPath（send 归属取值）与 sessionOwnerPath 注册表的单测（node:test，不启动 Electron/浏览器）。
//
// 钉住两件事：
//   1. send() 的归属优先级 payload.workspace → sessionOwner[sid] → activeWorkspace
//      （插件会话一旦有归属，命令必须回到那里，不能跟着 UI 当前项目走）；
//   2. 归属表本身的生命周期（删除会话/删除项目要清键，否则同 id 复用会继承旧路径）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  clearSessionOwnerPaths,
  forgetSessionOwnerPath,
  forgetSessionOwnerPathsOf,
  getSessionOwnerPath,
  isPluginProjectPath,
  resolveSendWorkspace,
  sessionOwnerPathEntries,
  setSessionOwnerPath,
} from './ownerPath'

const PLUGIN_A = '/Users/u/.go-code/plugin-sessions/alpha/home'
const PLUGIN_B = '/Users/u/.go-code/plugin-sessions/beta/home'
const REAL = '/Users/u/work/repo'

test('resolveSendWorkspace：显式 payload 优先于一切', () => {
  const ownerOf = (sid: string) => (sid === 's1' ? PLUGIN_A : undefined)
  // 显式 workspace = 真实项目，而会话归属是插件 —— 命令仍按调用方指定的走
  assert.equal(
    resolveSendWorkspace({ payloadWorkspace: REAL, sessionId: 's1', activeWorkspace: '/other', ownerOf }),
    REAL,
  )
  // 空白 payload 不算「显式」（不能被空格劫持成空 workspace）
  assert.equal(
    resolveSendWorkspace({ payloadWorkspace: '   ', sessionId: 's1', activeWorkspace: '/other', ownerOf }),
    PLUGIN_A,
  )
})

test('resolveSendWorkspace：无显式 workspace 时按会话归属', () => {
  const ownerOf = (sid: string) => (sid === 's1' ? PLUGIN_A : undefined)
  assert.equal(resolveSendWorkspace({ sessionId: 's1', activeWorkspace: REAL, ownerOf }), PLUGIN_A)
  // 归属查不到（真实项目的会话不登记）→ 回落活跃工作区
  assert.equal(resolveSendWorkspace({ sessionId: 's2', activeWorkspace: REAL, ownerOf }), REAL)
})

test('resolveSendWorkspace：空 session / 无 ownerOf 时只看活跃工作区', () => {
  assert.equal(resolveSendWorkspace({ sessionId: '', activeWorkspace: REAL }), REAL)
  assert.equal(resolveSendWorkspace({ activeWorkspace: REAL }), REAL)
  // 空 session 不查表（否则「裸命令」会被错误归属）
  let asked = 0
  const ownerOf = () => { asked++; return PLUGIN_A }
  assert.equal(resolveSendWorkspace({ sessionId: '', activeWorkspace: REAL, ownerOf }), REAL)
  assert.equal(asked, 0)
  // 全都缺失 → 空串（调用方不带 workspace 字段）
  assert.equal(resolveSendWorkspace({}), '')
  assert.equal(resolveSendWorkspace({ payloadWorkspace: null, sessionId: null, activeWorkspace: null }), '')
})

test('resolveSendWorkspace：虚拟与真实混合 —— 归属不串到别的会话/项目', () => {
  clearSessionOwnerPaths()
  setSessionOwnerPath('plug-1', PLUGIN_A)
  setSessionOwnerPath('plug-2', PLUGIN_B)
  const ownerOf = getSessionOwnerPath
  assert.equal(resolveSendWorkspace({ sessionId: 'plug-1', activeWorkspace: REAL, ownerOf }), PLUGIN_A)
  assert.equal(resolveSendWorkspace({ sessionId: 'plug-2', activeWorkspace: REAL, ownerOf }), PLUGIN_B)
  // 真实会话（未登记）不被插件归属污染
  assert.equal(resolveSendWorkspace({ sessionId: 'real-1', activeWorkspace: REAL, ownerOf }), REAL)
  // 停在插件 A 的会话上，给真实会话发带显式 workspace 的命令 → 仍去真实项目
  assert.equal(
    resolveSendWorkspace({ payloadWorkspace: REAL, sessionId: 'plug-1', activeWorkspace: PLUGIN_A, ownerOf }),
    REAL,
  )
})

test('sessionOwnerPath：写 / 读 / 删会话 / 删路径', () => {
  clearSessionOwnerPaths()
  setSessionOwnerPath('s1', PLUGIN_A)
  setSessionOwnerPath('s2', PLUGIN_A)
  setSessionOwnerPath('s3', PLUGIN_B)
  // 空值忽略：不留半个键
  setSessionOwnerPath('', PLUGIN_A)
  setSessionOwnerPath('s4', '')
  assert.equal(getSessionOwnerPath(''), undefined)
  assert.equal(getSessionOwnerPath('s4'), undefined)
  assert.equal(getSessionOwnerPath('s1'), PLUGIN_A)
  assert.equal(sessionOwnerPathEntries().length, 3)

  forgetSessionOwnerPath('s1')
  assert.equal(getSessionOwnerPath('s1'), undefined)
  assert.equal(getSessionOwnerPath('s2'), PLUGIN_A, '删一个会话不影响同项目的其它会话')

  forgetSessionOwnerPathsOf(PLUGIN_A)
  assert.equal(getSessionOwnerPath('s2'), undefined)
  assert.equal(getSessionOwnerPath('s3'), PLUGIN_B, '只清该路径下的会话')
  clearSessionOwnerPaths()
  assert.deepEqual(sessionOwnerPathEntries(), [])
})

test('isPluginProjectPath：只认插件会话区，真实项目/相邻目录不算', () => {
  assert.equal(isPluginProjectPath(PLUGIN_A), true)
  assert.equal(isPluginProjectPath('~/.go-code/plugin-sessions/alpha/home'), true)
  assert.equal(isPluginProjectPath('C:\\Users\\u\\.go-code\\plugin-sessions\\alpha\\home'), true)
  assert.equal(isPluginProjectPath(REAL), false)
  // 相邻名字不算：`plugin-sessions-archive` 是用户的普通目录
  assert.equal(isPluginProjectPath('/Users/u/work/plugin-sessions-archive'), false)
  assert.equal(isPluginProjectPath(''), false)
  assert.equal(isPluginProjectPath(undefined), false)
})
