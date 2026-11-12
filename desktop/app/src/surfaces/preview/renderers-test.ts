// 三个内置预览渲染器的**接线**冒烟测试（R08a）
//
// 为什么单测注册表之后还要这一份：注册表测试用的是假渲染器，证明不了「真渲染器被注册了」
// 与「load() 真的解出一个组件」。后者正是 04-review F45/F48 那一类静默失败 ——
// default 导出写错时 Host 会渲染空白，任何像素断言都不会红。故这里当场把组件解出来。
//
// 后缀是 .ts 不是 .tsx（同 builtin/index-test.ts）：esbuild 按扩展名选 loader，这里不写 JSX。
import test from 'node:test'
import assert from 'node:assert/strict'
import './renderers'
import { fileRefOf, listRenderers, pickRenderer } from './registry'

test('renderers/index 的副作用注册了 csv / image / text 三个渲染器（id 即 data-renderer）', () => {
  assert.deepEqual(listRenderers().map((r) => r.id), ['csv', 'image', 'text'])
})

test('weight 口径：image(80) > csv(60) > text(10)，与 types.PreviewWeight 注释一致', () => {
  const w = Object.fromEntries(listRenderers().map((r) => [r.id, r.weight]))
  assert.deepEqual(w, { csv: 60, image: 80, text: 10 })
  assert.ok(w.image > w.csv && w.csv > w.text, '顺序变了就等于换了赢家，必须显式钉住')
})

test('按扩展名命中正确的渲染器（大小写不敏感 / .tsv 也是表格）', () => {
  assert.equal(pickRenderer(fileRefOf('out/sales.csv'))?.id, 'csv')
  assert.equal(pickRenderer(fileRefOf('out/REPORT.TSV'))?.id, 'csv')
  assert.equal(pickRenderer(fileRefOf('brand/screenshot-main.png'))?.id, 'image')
  assert.equal(pickRenderer(fileRefOf('a/photo.JPEG'))?.id, 'image')
  assert.equal(pickRenderer(fileRefOf('auth.go'))?.id, 'text')
  assert.equal(pickRenderer(fileRefOf('Makefile'))?.id, 'text', '无扩展名文本仍要能看')
  assert.equal(pickRenderer(fileRefOf('README.md'))?.id, 'text', 'markdown 渲染器不在本轮：落 text')
})

test('本轮边界：pdf/docx/pptx/xlsx 与二进制无渲染器 → undefined（Host 给 unavailable 占位）', () => {
  // binary=true 是 bridge 对这几类格式的真实判定（file_preview 回 binary + 空 content）
  for (const p of ['a.pdf', 'a.docx', 'a.pptx', 'a.xlsx', 'a.mp4']) {
    assert.equal(pickRenderer(fileRefOf(p, 0, true)), undefined, `${p} 不该在 R08a 有渲染器（R08b/R08c）`)
  }
  assert.equal(pickRenderer(fileRefOf('a.zip', 0, true)), undefined, '二进制不落文本兜底（content 是空串）')
  assert.equal(pickRenderer(fileRefOf('a.zip'))?.id, 'text', '同样的扩展名但 bridge 判成文本 → text 接得住')
})

test('每个 load() 都解出组件（default 导出接错时这里当场红，而不是白屏）', async () => {
  for (const r of listRenderers()) {
    const C = await r.load()
    assert.equal(typeof C, 'function', `${r.id}.load() 没给出组件`)
  }
})
