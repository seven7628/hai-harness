// 内置预览渲染器的**接线**冒烟测试（R08a 三个 + R08b 四个 + R08c 一个 + 2026-11 补 html）
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

test('renderers/index 的副作用注册了 9 个渲染器（id 即 data-renderer）', () => {
  assert.deepEqual(listRenderers().map((r) => r.id), ['csv', 'image', 'text', 'pdf', 'docx', 'pptx', 'xlsx', 'media', 'html'])
})

test('weight 口径：专用格式(100) > media(90) > image(80) > html(70) > csv(60) > text(10)，与 types.PreviewRenderer 注释一致', () => {
  const w: Record<string, number> = Object.fromEntries(listRenderers().map((r) => [r.id, r.weight]))
  assert.ok(w.pdf > w.media && w.media > w.image && w.image > w.html && w.html > w.csv && w.csv > w.text, '顺序变了就等于换了赢家，必须显式钉住')
  for (const id of ['pdf', 'docx', 'pptx', 'xlsx', 'html']) {
    assert.ok(w[id] > w.text, `${id} 必须压过文本兜底（否则会被渲染成空代码面）`)
  }
  // 字面量断言放最后：@types/node 的 deepStrictEqual 是 `asserts actual is T`，写在前面会把
  // w 的类型收窄成那个字面量对象，后面的 w[id] 就索引不动了（TS7053）。
  assert.deepEqual(w, { csv: 60, image: 80, text: 10, pdf: 100, docx: 100, pptx: 100, xlsx: 100, media: 90, html: 70 })
})

test('requires：pdf→embed，docx/pptx/xlsx→conversion，media→media（挂载前拦截，写明缺什么）', () => {
  const req = Object.fromEntries(listRenderers().map((r) => [r.id, r.requires]))
  assert.deepEqual(req.pdf, ['embed'])
  assert.deepEqual(req.docx, ['conversion'])
  assert.deepEqual(req.pptx, ['conversion'])
  assert.deepEqual(req.xlsx, ['conversion'])
  assert.deepEqual(req.media, ['media'], '音视频要 hai-media:// 通道（Range），缺了不该渲染黑着的 <video>')
  assert.equal(req.text, undefined, '文本兜底无依赖')
})

test('按扩展名命中正确的渲染器（大小写不敏感 / .tsv 也是表格）', () => {
  assert.equal(pickRenderer(fileRefOf('out/sales.csv'))?.id, 'csv')
  assert.equal(pickRenderer(fileRefOf('report.pdf', 0, true))?.id, 'pdf')
  assert.equal(pickRenderer(fileRefOf('REPORT.PDF', 0, true))?.id, 'pdf')
  assert.equal(pickRenderer(fileRefOf('a/spec.docx', 0, true))?.id, 'docx')
  assert.equal(pickRenderer(fileRefOf('a/spec.docm', 0, true))?.id, 'docx')
  assert.equal(pickRenderer(fileRefOf('a/spec.doc', 0, true))?.id, 'docx', '老格式也交给转换通道（由 bridge 给出失败原因）')
  assert.equal(pickRenderer(fileRefOf('a/deck.pptx', 0, true))?.id, 'pptx')
  assert.equal(pickRenderer(fileRefOf('a/book.xlsx', 0, true))?.id, 'xlsx')
  assert.equal(pickRenderer(fileRefOf('a/book.xls', 0, true))?.id, 'xlsx')
  assert.equal(pickRenderer(fileRefOf('out/REPORT.TSV'))?.id, 'csv')
  assert.equal(pickRenderer(fileRefOf('brand/screenshot-main.png'))?.id, 'image')
  assert.equal(pickRenderer(fileRefOf('a/photo.JPEG'))?.id, 'image')
  assert.equal(pickRenderer(fileRefOf('auth.go'))?.id, 'text')
  assert.equal(pickRenderer(fileRefOf('Makefile'))?.id, 'text', '无扩展名文本仍要能看')
  assert.equal(pickRenderer(fileRefOf('README.md'))?.id, 'text', 'markdown 渲染器不在本轮：落 text')
})

test('html/htm/svg 命中 html 渲染器（与默认通道同一份 BROWSER_EXTS）', () => {
  // 用户报的回归：这三个扩展名此前落到 text → 面板里是源码。默认通道（内嵌浏览器）与这里的
  // match 必须同源，否则会出现「打开是浏览器、预览还是源码」的半修状态。
  for (const p of ['out/demo.html', 'INDEX.HTM', 'brand/logo.svg']) {
    assert.equal(pickRenderer(fileRefOf(p, 512))?.id, 'html', `${p} 必须落到 html 渲染器`)
  }
  // 二进制判定也不该把它推走：html 的 match 只看扩展名（内容读不回时由渲染器自己说明）
  assert.equal(pickRenderer(fileRefOf('demo.html', 0, true))?.id, 'html')
  // 反向：非浏览器扩展名不许被它抢走
  assert.equal(pickRenderer(fileRefOf('main.go'))?.id, 'text')
  assert.equal(pickRenderer(fileRefOf('page.html.bak'))?.id, 'text', '扩展名以最后一段为准，.bak 不是 html')
})

test('R08c：音视频命中 media（mp4/mp3/mkv 都是），未知二进制仍无渲染器', () => {
  // binary=true 是 bridge 对这几类格式的真实判定（file_preview 回 binary + 空 content）
  for (const p of ['a.mp4', 'a.mp3', 'a.mkv', 'a.wav', 'CAMERA.MOV']) {
    assert.equal(pickRenderer(fileRefOf(p, 0, true))?.id, 'media', `${p} 必须落到 media 渲染器`)
  }
  assert.equal(pickRenderer(fileRefOf('a.zip', 0, true)), undefined, '未知二进制仍给 unavailable 占位')
  assert.equal(pickRenderer(fileRefOf('a.zip'))?.id, 'text', '同样的扩展名但 bridge 判成文本 → text 接得住')
})

test('.pdf 不会被 text 抢走（哪怕 bridge 把它判成文本、size>0）', () => {
  // 变异口径：把 pdf 的 weight 调到 5 就会红在这里 —— 那是「容器还在、内容没了」的经典退化。
  assert.equal(pickRenderer(fileRefOf('a.pdf', 999))?.id, 'pdf')
  assert.equal(pickRenderer(fileRefOf('a.pdf', 999, true))?.id, 'pdf')
})

test('markdown 仍是 R08a 的临时态：.md 走 text（R08b 不改，报告里确认它仍可读）', () => {
  assert.equal(pickRenderer(fileRefOf('docs/README.md'))?.id, 'text')
  assert.equal(pickRenderer(fileRefOf('docs/README.MD'))?.id, 'text')
})

test('每个 load() 都解出组件（default 导出接错时这里当场红，而不是白屏）', async () => {
  for (const r of listRenderers()) {
    const C = await r.load()
    assert.equal(typeof C, 'function', `${r.id}.load() 没给出组件`)
  }
})
