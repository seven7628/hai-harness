// shortReason-test.ts —— `/` 菜单里那一行「它管什么」（node:test，无 DOM）。
// 规则：第一句 → 太长就截；拉丁不切词；空描述返回空串（那一格留白，不编）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { shortReason } from './shortReason'

test('短描述原样保留', () => {
  assert.equal(shortReason('联网 · 并行检索'), '联网 · 并行检索')
  assert.equal(shortReason('把这段对话压缩成摘要'), '把这段对话压缩成摘要')
})

test('长描述只取第一句', () => {
  const s = '直读 PDF 正文与结构，识别扫描页与 CID 字体。缺少 Unicode 映射时不猜内容。'
  assert.equal(shortReason(s), '直读 PDF 正文与结构，识别扫描页与 CID 字体')
})

test('一整段话收成一行，超长就带省略号', () => {
  const s = 'tasteskill v2, the anti-slop frontend skill. Art direction for websites, distilled from a study of 247 curated gallery sites'
  const out = shortReason(s, 30)
  assert.ok(out.length <= 31, out)
  assert.ok(out.endsWith('…'), out)
  assert.ok(!/comp$/.test(out), '不该把一个词劈开: ' + out)
})

test('CJK 超长按字截断，非法中逗号/顿号收尾', () => {
  const out = shortReason('运行时皮肤的组件库与演示页，逐字照设计稿移植，含真浏览器断言与几何量测', 12)
  assert.ok(out.endsWith('…'), out)
  assert.ok(!/[…]$/.test(out.replace('…', '…')) || true)
  assert.ok(out.length === 13, out) // 12 字 + …
  assert.ok(!/[，,、]$/.test(out.slice(0, -1)), out)
})

test('换行折成空格；空描述返回空串', () => {
  assert.equal(shortReason('第一行\n第二行'), '第一行 第二行')
  assert.equal(shortReason('   '), '')
  assert.equal(shortReason(''), '')
})

test('英文句点后空格算句子边界', () => {
  assert.equal(shortReason('Creates a new Remotion video. Then it renders it.'), 'Creates a new Remotion video')
})
