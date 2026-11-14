// 虚拟 Project 分组 / 回执归一化 / 合并的单测（node:test，不启动 Electron/浏览器）。
//
// 这些函数是左栏分组与 `plugin_projects_list` 水合的唯一判据 —— 它们错了，插件的会话
// 要么从真实项目选择器里冒出来（把插件当工作区），要么整组消失（用户以为插件没了）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { Project } from '../store/useAppStore'
import {
  isVirtualProject,
  mergePluginProjects,
  pluginProjectFromWire,
  pluginProjectLabel,
  pluginProjectsFromResponse,
  realProjects,
  splitPluginProjects,
} from './pluginProjects'

const P = (path: string, extra: Partial<Project> = {}): Project => ({ path, sessions: [], ...extra })
const PLUGIN_A = '/Users/u/.go-code/plugin-sessions/alpha/home'
const PLUGIN_B = '/Users/u/.go-code/plugin-sessions/beta/home'

test('isVirtualProject：virtual 标记与路径特征都要认', () => {
  assert.equal(isVirtualProject(P(PLUGIN_A)), true, '只有路径也要认（回执还没到）')
  assert.equal(isVirtualProject(P('/x', { virtual: true })), true)
  assert.equal(isVirtualProject(P('/Users/u/work/repo')), false)
  assert.equal(isVirtualProject(undefined), false)
})

test('realProjects：真实项目顺序不变，虚拟剔除', () => {
  const list = [P(PLUGIN_A, { virtual: true }), P('/b'), P('/a'), P(PLUGIN_B, { virtual: true })]
  assert.deepEqual(realProjects(list).map((p) => p.path), ['/b', '/a'])
})

test('splitPluginProjects：安装中 / 已卸载，未标 installed 视为已安装', () => {
  const list = [
    P('/real'),
    P(PLUGIN_A, { virtual: true, pluginId: 'alpha' }),
    P(PLUGIN_B, { virtual: true, pluginId: 'beta', installed: false }),
  ]
  const { installed, uninstalled } = splitPluginProjects(list)
  assert.deepEqual(installed.map((p) => p.pluginId), ['alpha'])
  assert.deepEqual(uninstalled.map((p) => p.pluginId), ['beta'])
})

test('pluginProjectLabel：name → pluginId → 路径尾段（跳过 home）', () => {
  assert.equal(pluginProjectLabel(P(PLUGIN_A, { name: 'PDF 阅读器' })), 'PDF 阅读器')
  assert.equal(pluginProjectLabel(P(PLUGIN_A, { pluginId: 'alpha' })), 'alpha')
  assert.equal(pluginProjectLabel(P(PLUGIN_A)), 'alpha')
  assert.equal(pluginProjectLabel(P('')), '')
})

test('pluginProjectFromWire：字段缺省与别名', () => {
  assert.deepEqual(pluginProjectFromWire({ path: PLUGIN_A, plugin_id: 'alpha', icon: '📕' }), {
    path: PLUGIN_A,
    virtual: true,
    pluginId: 'alpha',
    name: 'alpha',
    icon: '📕',
    installed: true,
    enabled: true,
  })
  // home / pluginId 别名；name 缺失时退回 pluginId
  assert.equal(pluginProjectFromWire({ home: PLUGIN_B, pluginId: 'beta', installed: false })?.name, 'beta')
  assert.equal(pluginProjectFromWire({ home: PLUGIN_B, pluginId: 'beta', installed: false })?.installed, false)
  // 没路径 → undefined（不猜、不造空项目）
  assert.equal(pluginProjectFromWire({ name: 'x' }), undefined)
  assert.equal(pluginProjectFromWire(null), undefined)
})

test('pluginProjectsFromResponse：data.projects / data.data.projects / 顶层 projects', () => {
  const one = [{ path: PLUGIN_A }]
  assert.deepEqual(pluginProjectsFromResponse({ data: { projects: one } }), one)
  assert.deepEqual(pluginProjectsFromResponse({ data: { data: { projects: one } } }), one)
  assert.deepEqual(pluginProjectsFromResponse({ data: {}, raw: { projects: one } }), one)
  assert.deepEqual(pluginProjectsFromResponse({ data: { projects: [] } }), [], '空数组是有效回执（插件全没了）')
  // 形状不认识 → undefined（≠ 空数组）：调用方据此保留已加载的虚拟项目
  assert.equal(pluginProjectsFromResponse({ data: {} }), undefined)
  assert.equal(pluginProjectsFromResponse({}), undefined)
})

test('mergePluginProjects：保留本地会话、真实项目位置不变、新路径追加尾部', () => {
  const prev = [
    P('/real-1', { pinned: true }),
    P(PLUGIN_A, { virtual: true, pluginId: 'alpha', sessions: [{ id: 's1', title: 't1' }] }),
    P('/real-2'),
  ]
  const { projects, paths } = mergePluginProjects(prev, [
    { path: PLUGIN_A, plugin_id: 'alpha', name: 'Alpha', icon: 'A' },
    { path: PLUGIN_B, plugin_id: 'beta', name: 'Beta' },
  ])
  assert.deepEqual(projects.map((p) => p.path), ['/real-1', PLUGIN_A, '/real-2', PLUGIN_B])
  assert.equal(projects[0].pinned, true, '真实项目的置顶/位置不被插件合并影响')
  const a = projects.find((p) => p.path === PLUGIN_A)!
  assert.deepEqual(a.sessions.map((s) => s.id), ['s1'], '回执不覆盖本地会话列表')
  assert.equal(a.name, 'Alpha')
  assert.equal(a.icon, 'A')
  assert.deepEqual(paths, [PLUGIN_A, PLUGIN_B], '返回的 paths 用于逐个 list 水合')
})

test('mergePluginProjects：回执里没有的虚拟项目 —— 有会话转已卸载，空壳摘掉', () => {
  const prev = [
    P(PLUGIN_A, { virtual: true, pluginId: 'alpha', sessions: [{ id: 's1', title: 't1' }] }),
    P(PLUGIN_B, { virtual: true, pluginId: 'beta' }),
  ]
  const { projects } = mergePluginProjects(prev, [])
  assert.deepEqual(projects.map((p) => p.path), [PLUGIN_A])
  assert.equal(projects[0].installed, false)
})

test('mergePluginProjects：已卸载插件重新出现在回执里 → 恢复为已安装', () => {
  const prev = [P(PLUGIN_A, { virtual: true, pluginId: 'alpha', installed: false, sessions: [{ id: 's1', title: 't1' }] })]
  const { projects } = mergePluginProjects(prev, [{ path: PLUGIN_A, plugin_id: 'alpha' }])
  assert.equal(projects[0].installed, true)
  assert.equal(projects[0].sessions.length, 1)
})
