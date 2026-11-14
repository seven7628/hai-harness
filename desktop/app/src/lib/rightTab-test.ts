// rightTab-test.ts —— `selectRightTab`（右栏「现在是哪个面板」的派生选择器）的单测
//（node:test，无 DOM、无 store 运行时）—— R11b。
//
// 为什么钉这些：R11b 删掉了 `AppState.rightTab`（PR-02 起的写穿副本），把全部读点换成这个
// 选择器。写错的表现**全是静默的**，因为「哪个面板在显示」没有独立的第二处可以对照：
//
//   ① 只认内置四类、遇 `plugin:<id>` 返回 undefined → 插件标签当选时折叠态 rail 一个都不亮、
//      `focusWebTabIfClosed` 之类的「已打开就不重复」判定会误判成「没开」；
//   ② 给悬空 activeId 加兜底（旧字段的语义是「保留最后的值」）→ 面板显示的是空态，而各处
//      按 kind 分支的判定仍以为在某面板上；
//   ③ 少看一层 scope（用 workspace 顶替 activeSessionId）→ 两个会话共用一套标签，
//      切会话看到别人的面板。
//
// 迁移类 PR 做「红先行」没有意义（旧字段本来就在派生），故用**变异检验**证明本文件有牙：
// 把选择器改成「只认内置四类」或「悬空返回 'web'」，对应用例必须变红（见提交说明）。
import test from 'node:test'
import assert from 'node:assert/strict'
import { activeTabOf, selectRightTab, tabScopeKey } from './rightTab'
import type { TabSet } from '../store/useAppStore'
import type { SurfaceKind } from '../surfaces/types'

// 一个标签集：tabs + activeId（closed 不影响选择器，填空数组）
const set = (tabs: { id: string; kind: SurfaceKind; [k: string]: unknown }[], activeId: string | null): TabSet =>
  ({ tabs, activeId, closed: [] }) as unknown as TabSet
const state = (tabsByScope: Record<string, TabSet>, activeSessionId?: string, workspace?: string) =>
  ({ tabsByScope, activeSessionId, workspace })

// —— 一、五种标签的派生结果 ——
// 逐 kind 钉**返回值本身**。file 的四种 mode 单列：kind 只回答「是哪个面板」，
// mode 回答「这个面板里渲染什么」—— 混进选择器就会让「编辑器标签」被当成另一个面板。
test('selectRightTab：内置四类各自派生自己的 kind', () => {
  const kinds: SurfaceKind[] = ['activity', 'git', 'web']
  for (const k of kinds) {
    assert.equal(selectRightTab(state({ s1: set([{ id: 't1', kind: k }], 't1') }, 's1')), k)
  }
})

test('selectRightTab：file 标签（无 mode）→ file', () => {
  assert.equal(selectRightTab(state({ s1: set([{ id: 't1', kind: 'file', path: 'a.md' }], 't1') }, 's1')), 'file')
})

test('selectRightTab：file 的四种 mode 都派生 file（mode 不是 kind，D-I/D5）', () => {
  for (const mode of ['diff', 'source', 'edit', 'preview']) {
    const st = state({ s1: set([{ id: 't1', kind: 'file', path: 'a.md', mode }], 't1') }, 's1')
    assert.equal(selectRightTab(st), 'file', `mode=${mode}`)
  }
  // 同一文件的 diff + source 两个实例：谁激活就还是 file —— 值不变，变的只是 activeId
  const two = set([
    { id: 't1', kind: 'file', path: 'a.md', mode: 'diff' },
    { id: 't2', kind: 'file', path: 'a.md', mode: 'source' },
  ], 't1')
  assert.equal(selectRightTab(state({ s1: two }, 's1')), 'file')
  two.activeId = 't2'
  assert.equal(activeTabOf(state({ s1: two }, 's1'))?.id, 't2')
  assert.equal(selectRightTab(state({ s1: two }, 's1')), 'file')
})

test('selectRightTab：plugin:<id> 原样返回（第五种标签，不是 undefined）', () => {
  const st = state({ s1: set([{ id: 't1', kind: 'plugin:demo', params: { a: 1 } }], 't1') }, 's1')
  assert.equal(selectRightTab(st), 'plugin:demo')
})

// —— 二、无值：三种都要是 undefined，且不许兜底 ——
test('selectRightTab：没有标签集 → undefined', () => {
  assert.equal(selectRightTab(state({}, 's1')), undefined)
})

test('selectRightTab：activeId===null（关光标签）→ undefined（不保留最后的值）', () => {
  const st = state({ s1: set([{ id: 't1', kind: 'git' }], null) }, 's1')
  assert.equal(selectRightTab(st), undefined, '旧字段在这里会留 git；选择器不许复刻')
})

test('selectRightTab：activeId 悬空（脏数据）→ undefined（旧兜底 web 已删）', () => {
  const st = state({ s1: set([{ id: 't1', kind: 'git' }], 'nope') }, 's1')
  assert.equal(selectRightTab(st), undefined)
  assert.equal(activeTabOf(st), undefined)
})

// —— 三、scope：会话 id 优先，无会话退化为工作区路径 ——
test('tabScopeKey：activeSessionId 优先，无会话退化为 workspace，都没有是空串', () => {
  assert.equal(tabScopeKey({ activeSessionId: 's1', workspace: '/ws' }), 's1')
  assert.equal(tabScopeKey({ workspace: '/ws' }), '/ws')
  assert.equal(tabScopeKey({}), '')
})

test('selectRightTab：只看当前 scope 的标签集（两个会话互不串）', () => {
  const st = state({
    s1: set([{ id: 'a', kind: 'git' }], 'a'),
    s2: set([{ id: 'b', kind: 'web' }], 'b'),
  }, 's2', '/ws')
  assert.equal(selectRightTab(st), 'web')
  assert.equal(selectRightTab({ ...st, activeSessionId: 's1' }), 'git')
  // 无会话时用工作区路径取键（D7 的窗口期）
  assert.equal(selectRightTab({ tabsByScope: { '/ws': set([{ id: 'f', kind: 'file' }], 'f') }, workspace: '/ws' }), 'file')
})
