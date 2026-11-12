// webTab-test.ts —— web 标签「身份判据」的单测（node:test，无 DOM、无 store 运行时）—— R06 C1。
//
// 为什么钉这些：这四条判据决定「重启恢复出来的标签点开能不能接回原来那个实例」，
// 而写错的三种表现**全是静默的**：
//
//   ① webTabKey 对**另一数据源**也返回键 → syncWebTabs 拿它去 live 集合里查，必然查不到 →
//      一次同步就把另一套标签全删掉（native/mcp 两套 id 空间不同，这是最易写错的一处）；
//   ② samePreviewTarget 只比 url → office 转换产物每次换临时 html_path（url 不同、源文件相同），
//      恢复的标签永远停在待重建态，用户点一次还多一个重复标签；
//   ③ isPendingWebTab 把「有池身份」的活标签也算成待重建 → 重启后点开它等于再建一个视图。
//
// 迁移类 PR 做「红先行」没有意义（旧实现本来就是对的），故用**变异检验**证明本文件有牙：
// 手改坏两处实现（webTabKey 对另一源也返回键、samePreviewTarget 只比 url），对应用例必须变红。
import test from 'node:test'
import assert from 'node:assert/strict'
import { isPendingWebTab, samePreviewTarget, webTabKey, webUrlSnapshot } from './webTab'
import type { AppState, BrowserPage, EmbedTargetInfo, TabInstance, WebTabInstance } from '../store/useAppStore'

// 待重建的恢复标签（重启后从持久化读回来的形态：只有 url 快照，没有池身份）
const pendingTab = (url = 'https://a.example/', srcPath?: string): WebTabInstance =>
  ({ id: 't1', kind: 'web', url, ...(srcPath ? { srcPath } : {}) })
// 活标签：native = 原生视图池的 'tN'，mcp = MCP 页列表的数字 id
const nativeTab = (viewId: string, extra: Partial<WebTabInstance> = {}): WebTabInstance =>
  ({ id: 't1', kind: 'web', viewId, ...extra })
const mcpTab = (pageId: number, extra: Partial<WebTabInstance> = {}): WebTabInstance =>
  ({ id: 't1', kind: 'web', pageId, ...extra })

// AppState 是整张 store 的面；这几条判据只读 embedNav.targets 与 browserPages，
// 故 fixture 只给这两个字段（类型断言仅限测试夹具，生产侧不这么做）。
const stateOf = (o: { embedNav?: { targets: EmbedTargetInfo[] }; browserPages?: BrowserPage[] }): AppState =>
  o as unknown as AppState
const target = (id: string, url: string, srcPath?: string): EmbedTargetInfo =>
  ({ id, url, title: '', loading: false, ...(srcPath ? { srcPath } : {}) })

// —— 一、isPendingWebTab：判据 = 「有 url 快照 + 没有任何池身份」——

test('isPendingWebTab：有 url、无 viewId/pageId → 待重建', () => {
  assert.equal(isPendingWebTab(pendingTab()), true)
  assert.equal(isPendingWebTab(pendingTab('https://a.example/', '/ws/a.pdf')), true)
})

test('isPendingWebTab：三态互斥逐条 —— 任一池身份在场就不是待重建（即便带着 url 快照）', () => {
  // 活标签（native）：只有 viewId
  assert.equal(isPendingWebTab(nativeTab('t7')), false)
  // 活标签（mcp）：只有 pageId
  assert.equal(isPendingWebTab(mcpTab(7)), false)
  // 既有池身份又残留 url 快照（rebuild 后的中间态）→ 仍算活标签
  assert.equal(isPendingWebTab(nativeTab('t7', { url: 'https://a.example/' })), false)
  assert.equal(isPendingWebTab(mcpTab(7, { url: 'https://a.example/' })), false)
})

test('isPendingWebTab：无 url 的空 web 标签（用户点「+ 浏览器」还没导航）→ 不是待重建', () => {
  assert.equal(isPendingWebTab({ id: 't1', kind: 'web' }), false)
  assert.equal(isPendingWebTab({ id: 't1', kind: 'web', url: '' }), false)
})

test('isPendingWebTab：非 web 标签一律 false', () => {
  const others: TabInstance[] = [
    { id: 't1', kind: 'activity' },
    { id: 't2', kind: 'git' },
    { id: 't3', kind: 'file', path: 'src/a.ts', mode: 'diff' },
  ]
  for (const t of others) assert.equal(isPendingWebTab(t), false, `${t.kind} 不可能是待重建 web 标签`)
})

test('isPendingWebTab：类型谓词确实把 TabInstance 收窄成 WebTabInstance（收窄失效则 tsc 报错）', () => {
  const readUrl = (t: TabInstance) => (isPendingWebTab(t) ? t.url : undefined)
  assert.equal(readUrl(pendingTab('https://a.example/')), 'https://a.example/')
  assert.equal(readUrl(nativeTab('t7')), undefined)
})

// —— 二、webTabKey：只认**当前数据源**的身份，另一源必须 undefined ——

test('webTabKey：native 源取 viewId', () => {
  assert.equal(webTabKey(nativeTab('t7'), 'native'), 't7')
  // 池里编号不是数字也无所谓：viewId 原样透传（主进程 't'+自增）
  assert.equal(webTabKey(nativeTab('t12'), 'native'), 't12')
})

test('webTabKey：mcp 源取 pageId 并加 p 前缀', () => {
  assert.equal(webTabKey(mcpTab(7), 'mcp'), 'p7')
  assert.equal(webTabKey(mcpTab(0), 'mcp'), 'p0')
})

test('webTabKey：另一数据源返回 undefined（风险 4 的核心 —— 两套 id 空间不得互认）', () => {
  // native 标签在 mcp 源下没有身份：否则 syncWebTabs 会拿 'p...' 去 native 的 live 集合里查
  assert.equal(webTabKey(nativeTab('t7'), 'mcp'), undefined)
  // mcp 标签在 native 源下同理
  assert.equal(webTabKey(mcpTab(7), 'native'), undefined)
})

test('webTabKey：待重建的恢复标签（无池身份）在两源下都没有键', () => {
  assert.equal(webTabKey(pendingTab(), 'native'), undefined)
  assert.equal(webTabKey(pendingTab(), 'mcp'), undefined)
})

test('webTabKey：非 web 标签一律 undefined', () => {
  for (const t of [{ id: 't1', kind: 'activity' }, { id: 't2', kind: 'git' }, { id: 't3', kind: 'file', path: 'a.ts' }] as TabInstance[]) {
    assert.equal(webTabKey(t, 'native'), undefined)
    assert.equal(webTabKey(t, 'mcp'), undefined)
  }
})

test('webTabKey：两个身份同时在场时按数据源各取各的（同步时不会串源）', () => {
  const both: WebTabInstance = { id: 't1', kind: 'web', viewId: 't7', pageId: 7 }
  assert.equal(webTabKey(both, 'native'), 't7')
  assert.equal(webTabKey(both, 'mcp'), 'p7')
})

// —— 三、samePreviewTarget：两侧都有 srcPath 时比 srcPath，否则比 url ——

test('samePreviewTarget：两侧都有 srcPath 时比 srcPath —— url 不同也算同一个页面', () => {
  const WS = '/ws'
  // office 转换产物：每次预览换一个临时 html_path，源文件才是身份
  const restored = pendingTab('file:///tmp/conv-aaa/a.pdf.html', '/ws/docs/a.pdf')
  const incoming = { url: 'file:///tmp/conv-bbb/a.pdf.html', srcPath: '/ws/docs/a.pdf' }
  assert.equal(samePreviewTarget(restored, incoming, WS), true)
})

test('samePreviewTarget：srcPath 不同 → 不算同一个（即便 url 碰巧相同）', () => {
  const restored = pendingTab('https://x.example/', '/ws/docs/a.pdf')
  assert.equal(samePreviewTarget(restored, { url: 'https://x.example/', srcPath: '/ws/docs/b.pdf' }, '/ws'), false)
})

test('samePreviewTarget：两侧都没有 srcPath 时比 url', () => {
  const restored = pendingTab('https://x.example/')
  assert.equal(samePreviewTarget(restored, { url: 'https://x.example/' }, '/ws'), true)
  assert.equal(samePreviewTarget(restored, { url: 'https://y.example/' }, '/ws'), false)
})

test('samePreviewTarget：只有一侧有 srcPath → 退回比 url（不做跨口径比较）', () => {
  const restored = pendingTab('https://x.example/') // 快照无 srcPath（旧版本存下来的）
  assert.equal(samePreviewTarget(restored, { url: 'https://x.example/', srcPath: '/ws/a.pdf' }, '/ws'), true)
  assert.equal(samePreviewTarget(restored, { url: 'https://y.example/', srcPath: '/ws/a.pdf' }, '/ws'), false)
})

test('samePreviewTarget：工作区内绝对路径与相对路径视为同一个源文件', () => {
  const restored = pendingTab('file:///tmp/conv-aaa/a.pdf.html', 'docs/a.pdf')
  const incoming = { url: 'file:///tmp/conv-bbb/a.pdf.html', srcPath: '/ws/docs/a.pdf' }
  assert.equal(samePreviewTarget(restored, incoming, '/ws'), true)
})

test('samePreviewTarget：工作区外的绝对路径不归一化 → 不是同一个', () => {
  const restored = pendingTab('https://x.example/', '/other/a.pdf')
  assert.equal(samePreviewTarget(restored, { url: 'https://x.example/', srcPath: '/ws/a.pdf' }, '/ws'), false)
})

test('samePreviewTarget：恢复标签没有 url（空标签）且无 srcPath → 绝不认领', () => {
  assert.equal(samePreviewTarget({ id: 't1', kind: 'web' }, { url: '' }, '/ws'), false)
  assert.equal(samePreviewTarget({ id: 't1', kind: 'web' }, { url: 'https://x.example/' }, '/ws'), false)
})

// —— 四、webUrlSnapshot：持久化时的三级反查（快照 → 视图池 viewId → MCP 页 pageId）——

test('webUrlSnapshot：标签自带 url 快照优先（原样带回 srcPath）', () => {
  const st = stateOf({ embedNav: { targets: [target('t7', 'https://pool.example/')] }, browserPages: [{ id: 7, url: 'https://mcp.example/', title: '' }] })
  // 带池身份 + 快照：快照仍是最可信的那一个（它是「当时用户打开的那一页」）
  assert.deepEqual(webUrlSnapshot({ id: 't1', kind: 'web', viewId: 't7', url: 'https://snap.example/', srcPath: '/ws/a.pdf' }, st), { url: 'https://snap.example/', srcPath: '/ws/a.pdf' })
  assert.deepEqual(webUrlSnapshot(pendingTab('https://snap.example/'), st), { url: 'https://snap.example/' })
})

test('webUrlSnapshot：按 viewId 反查视图池 targets；about:blank 是自举占位 → 不可恢复', () => {
  const st = stateOf({ embedNav: { targets: [target('t7', 'https://pool.example/', '/ws/a.pdf'), target('t8', 'about:blank')] }, browserPages: [] })
  assert.deepEqual(webUrlSnapshot(nativeTab('t7'), st), { url: 'https://pool.example/', srcPath: '/ws/a.pdf' })
  assert.equal(webUrlSnapshot(nativeTab('t8'), st), undefined)
  // 池里没有这个 id（已被 LRU 关掉 / 从未导航成功）→ 宁可不恢复
  assert.equal(webUrlSnapshot(nativeTab('t99'), st), undefined)
})

test('webUrlSnapshot：没有 targets（重启后主进程还没推第一帧）→ 不可恢复', () => {
  assert.equal(webUrlSnapshot(nativeTab('t7'), stateOf({ browserPages: [] })), undefined)
})

test('webUrlSnapshot：按 pageId 反查 MCP 页列表', () => {
  const st = stateOf({ browserPages: [{ id: 7, url: 'https://mcp.example/', title: '' }, { id: 8, url: '', title: '' }] })
  assert.deepEqual(webUrlSnapshot(mcpTab(7), st), { url: 'https://mcp.example/' })
  assert.equal(webUrlSnapshot(mcpTab(8), st), undefined)
  assert.equal(webUrlSnapshot(mcpTab(99), st), undefined)
})

test('webUrlSnapshot：什么身份都没有且无 url 快照 → 不可恢复', () => {
  assert.equal(webUrlSnapshot({ id: 't1', kind: 'web' }, stateOf({ browserPages: [] })), undefined)
})
