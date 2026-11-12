// 预览渲染器注册表单测（R08a 验收核心之一）：node --test + esbuild，见 00-conventions §1.9
//
// 这一份**只测注册表本身**（不 import 任何渲染器/React/DOM）——「注册表能脱离浏览器单测」
// 是设计约束，也是它作为唯一选择规则来源的前提。三个真渲染器的接线由 renderers-test.ts 覆盖。
import test from 'node:test'
import assert from 'node:assert/strict'
import {
  dependencyStatus,
  fileRefOf,
  getRenderer,
  listRenderers,
  missingDependencyReason,
  pickRenderer,
  registerRenderer,
  resetRenderersForTest,
  setDependencyStatus,
  unregisterRenderer,
} from './registry'
import type { FileRef, PreviewProps, PreviewRenderer } from './types'
import type { ComponentType } from 'react'

const noop = async () => (() => null) as unknown as ComponentType<PreviewProps>

function make(over: Partial<PreviewRenderer> = {}): PreviewRenderer {
  return { id: 'r', match: () => 1, weight: 10, load: noop, ...over }
}

const fx = (path: string, binary = false): FileRef => fileRefOf(path, binary ? 0 : 12, binary)

test.beforeEach(() => resetRenderersForTest())

test('注册后可按 id 取回；未注册的 id 返回 undefined', () => {
  const r = make()
  registerRenderer(r)
  assert.equal(getRenderer('r'), r)
  assert.equal(getRenderer('nope'), undefined)
  assert.equal(getRenderer(undefined), undefined)
  assert.deepEqual(listRenderers().map((x) => x.id), ['r'])
})

test('重复 id 注册即抛，且不覆盖已有实例（不静默覆盖）', () => {
  const first = make()
  registerRenderer(first)
  assert.throws(() => registerRenderer(make({ weight: 99 })), /重复注册/)
  assert.equal(getRenderer('r'), first)
  assert.equal(listRenderers().length, 1)
})

test('字段缺失/非法：id / match / load / weight 一律注册即抛', () => {
  assert.throws(() => registerRenderer(make({ id: '' })), /缺少 id/)
  assert.throws(() => registerRenderer(make({ match: undefined as never })), /缺少 match/)
  assert.throws(() => registerRenderer(make({ load: undefined as never })), /缺少 load/)
  assert.throws(() => registerRenderer(make({ weight: Number.NaN })), /weight/)
  assert.throws(() => registerRenderer(make({ weight: 'heavy' as never })), /weight/)
  assert.equal(listRenderers().length, 0, '校验失败不能留下半注册的条目')
})

test('依赖名册：已知依赖可注册，拼错的依赖名注册即抛', () => {
  registerRenderer(make({ id: 'docx', requires: ['conversion'] }))
  assert.deepEqual(getRenderer('docx')?.requires, ['conversion'])
  assert.throws(() => registerRenderer(make({ id: 'docx2', requires: ['conversoin' as never] })), /未知依赖：conversoin/)
  assert.throws(() => registerRenderer(make({ id: 'x', requires: 'conversion' as never })), /必须是数组/)
})

test('选择规则：weight 降序取第一个，match=0 不参与', () => {
  registerRenderer(make({ id: 'text', weight: 10, match: () => 1 }))
  registerRenderer(make({ id: 'csv', weight: 60, match: (f) => (f.ext === 'csv' ? 1 : 0) }))
  registerRenderer(make({ id: 'image', weight: 80, match: (f) => (f.ext === 'png' ? 1 : 0) }))
  assert.equal(pickRenderer(fx('a.csv'))?.id, 'csv', 'csv 命中且 weight 高于 text')
  assert.equal(pickRenderer(fx('a.png'))?.id, 'image')
  assert.equal(pickRenderer(fx('a.go'))?.id, 'text', '只有兜底时落到 text')
  assert.equal(pickRenderer(fx('a.csv', true))?.id, 'csv', 'csv 的 match 与 binary 无关')
})

test('选择规则：同 weight **按注册顺序**（确定，不靠 Map 迭代顺序）', () => {
  registerRenderer(make({ id: 'first', weight: 50, match: () => 1 }))
  registerRenderer(make({ id: 'second', weight: 50, match: () => 1 }))
  assert.equal(pickRenderer(fx('a.txt'))?.id, 'first')
  // 反向注册 → 反向胜出，证明判据就是注册顺序而不是 id 字典序
  resetRenderersForTest()
  registerRenderer(make({ id: 'second', weight: 50, match: () => 1 }))
  registerRenderer(make({ id: 'first', weight: 50, match: () => 1 }))
  assert.equal(pickRenderer(fx('a.txt'))?.id, 'second')
})

test('选择规则：match 的数字大小不参与排序（只判接不接），weight 说了算', () => {
  registerRenderer(make({ id: 'low-match-high-weight', weight: 90, match: () => 1 }))
  registerRenderer(make({ id: 'high-match-low-weight', weight: 20, match: () => 1000 }))
  assert.equal(pickRenderer(fx('a.txt'))?.id, 'low-match-high-weight')
})

test('only 收窄候选（文件面板的「表格/纯文本」开关靠它，不把 UI 模式塞进 FileRef）', () => {
  registerRenderer(make({ id: 'text', weight: 10, match: () => 1 }))
  registerRenderer(make({ id: 'csv', weight: 60, match: (f) => (f.ext === 'csv' ? 1 : 0) }))
  assert.equal(pickRenderer(fx('a.csv'), { only: ['text'] })?.id, 'text')
  assert.equal(pickRenderer(fx('a.csv'), { only: ['csv'] })?.id, 'csv')
  assert.equal(pickRenderer(fx('a.csv'), { only: ['image'] }), undefined)
})

test('无人接 → undefined（调用方的 unavailable 落点，不许白屏）', () => {
  assert.equal(pickRenderer(fx('a.zip')), undefined)
  registerRenderer(make({ id: 'text', weight: 10, match: (f) => (f.binary ? 0 : 1) }))
  assert.equal(pickRenderer(fx('a.zip', true)), undefined, '二进制文件不该落到文本兜底')
  assert.equal(pickRenderer(fx('a.txt'))?.id, 'text')
})

test('依赖不可用：missingDependencyReason 给出**写明原因**的落点；置为 ok 后消失', () => {
  registerRenderer(make({ id: 'docx', weight: 100, match: (f) => (f.ext === 'docx' ? 1 : 0), requires: ['conversion'] }))
  const picked = pickRenderer(fx('a.docx'))
  assert.ok(picked, '依赖不可用 ≠ 选不到：选得到，落点是 unavailable 占位')
  const reason = missingDependencyReason(picked!)
  assert.ok(reason && /转换服务未安装/.test(reason), `原因要写清楚，实际：${reason}`)
  // 默认状态：R08a 里两个依赖都没装
  assert.equal(dependencyStatus('conversion')?.ok, false)
  assert.equal(dependencyStatus('media')?.ok, false)
  setDependencyStatus('conversion', { ok: true })
  assert.equal(missingDependencyReason(picked!), undefined)
  setDependencyStatus('conversion', { ok: false, reason: '这次真的没装' })
  assert.equal(missingDependencyReason(picked!), '这次真的没装')
  assert.throws(() => setDependencyStatus('nope' as never, { ok: true }), /未知依赖/)
})

test('注销：删掉才返回 true，重复注销不抛错；注销后不再参与选择', () => {
  registerRenderer(make({ id: 'text' }))
  assert.equal(unregisterRenderer('text'), true)
  assert.equal(unregisterRenderer('text'), false)
  assert.equal(pickRenderer(fx('a.txt')), undefined)
})

test('fileRefOf：ext 与 lib/visualFile.extOf 同口径（唯一扩展名事实源）', () => {
  assert.deepEqual(fileRefOf('a/b/Sales.CSV'), { path: 'a/b/Sales.CSV', ext: 'csv', size: 0 })
  assert.equal(fileRefOf('.gitignore').ext, '')
  assert.equal(fileRefOf('Makefile').ext, '')
  assert.deepEqual(fileRefOf('x.png', 5, true), { path: 'x.png', ext: 'png', size: 5, binary: true })
})
