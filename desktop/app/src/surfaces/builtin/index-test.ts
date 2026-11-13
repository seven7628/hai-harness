// 内置 provider 注册冒烟测试：证明「副作用 import 真的把 git 注册进去了」
// （这类接线最容易在重构里被 tree-shaking 或循环依赖悄悄弄丢）
//
// 后缀是 .ts 不是 .tsx（test:builtin-surfaces 用 esbuild 按扩展名选 loader），
// 所以这里不写 JSX：图标的比对走 renderToStaticMarkup（比「元素结构相等」更贴近用户看到的像素）。
import test from 'node:test'
import assert from 'node:assert/strict'
import { renderToStaticMarkup } from 'react-dom/server'
import './index'
import { getSurface, listSurfaces } from '../registry'
import { useAppStore } from '../../store/useAppStore'
import type { WebTabInstance } from '../../store/useAppStore'
import { Activity, BarChart, FileIcon, Globe, ImageIcon } from '../../components/icons'
import { WebTab } from '../../components/WebTab'
import { ActivityTab } from '../../components/ActivityTab'
import { fileUrlToPath } from '../../lib/filePaths'
import { visualKindOf } from '../../lib/visualFile'

test('builtin/index 的副作用注册了 file provider（多实例身份）', () => {
  const p = getSurface('file')
  assert.ok(p, 'file provider 未注册')
  assert.equal(p.namespace, 'builtin')
  assert.equal(p.group, 'content')
  assert.equal(p.singleton, false)
  assert.equal(p.allowDuplicate, true)
  assert.deepEqual(p.capabilities, ['read', 'view', 'write'])
})

test('builtin/index 的副作用注册了 git provider', () => {
  const p = getSurface('git')
  assert.ok(p, 'git provider 未注册')
  assert.equal(p.kind, 'git')
  assert.equal(p.namespace, 'builtin')
  assert.equal(p.group, 'workflow')
  assert.equal(p.weight, 'light')
  assert.equal(p.singleton, true)
  assert.deepEqual(p.capabilities, ['read', 'view'])
  assert.equal(typeof p.render, 'function')
})

// —— R06b/C3：web provider（元数据 + 渲染委派）——

test('builtin/index 的副作用注册了 web provider，且元数据符合 03 §2.3 的口径', () => {
  const p = getSurface('web')
  assert.ok(p, 'web provider 未注册')
  assert.equal(p.kind, 'web')
  assert.equal(p.namespace, 'builtin')
  assert.equal(p.group, 'content')
  assert.equal(p.order, 20)
  // 多实例 + 会话作用域：同一会话可同时开多个网页（身份是池里的 viewId / pageId）
  assert.equal(p.singleton, false)
  assert.equal(p.allowDuplicate, true)
  assert.equal(p.scope, 'session')
  // heavy：一个 web 标签背后是一个真实渲染进程（视图池上限 12，03 §2.3 不变量 1）
  assert.equal(p.weight, 'heavy')
  assert.deepEqual(p.capabilities, ['read', 'view'])
  // C3 起 render 真的产出组件（C2 那条 `=== null` 的断言是「本轮零渲染改动」的机器可读形式，
  // 现在已经过期）：类型必须**就是**平移后的 WebTab —— 不是占位、不是内联箭头函数包一层，
  // 否则 RightPanel 侧的元素类型会在每次渲染变化 → 切标签时组件重挂载 → 原生视图被销毁重建
  //（面板闪白 + 页面状态丢失，正是 R06c 最怕的回归）。props 里只该有 { tab, scope }：
  // 多传 RP 专属 props（immersive/menuOpen/panelVisible）就会把「上移成 store / 内部 state」
  // 这件事又偷偷退回组件 props，两条真相源立刻分叉。
  assert.equal(typeof p.render, 'function')
  const el = p.render({ tab: { id: 't1', kind: 'web' }, scope: 's' }) as unknown as {
    type: unknown
    props: Record<string, unknown>
  }
  assert.equal(el.type, WebTab)
  assert.deepEqual(Object.keys(el.props).sort(), ['scope', 'tab'])
  assert.deepEqual(el.props.tab, { id: 't1', kind: 'web' })
  assert.equal(el.props.scope, 's')
  assert.deepEqual(p.defaultSpec?.({} as never), { kind: 'web' })
})

test('web 与 file 同属 content 组，按 order 排在 file 之后；git 仍在 workflow 组最前', () => {
  // 注册 web 不该把它挤到别处，也不该撑破 registry 的「内置 kind 仅 4 个」白名单。
  // R07 起 activity（content/order 30）排在 web 之后 —— 四个内置 kind 至此全部注册。
  assert.deepEqual(listSurfaces().map((p) => p.kind), ['git', 'file', 'web', 'activity'])
})

// —— 元数据委派的**行为等价**证明 ——
// 为什么需要这一段：截图对比跑在纯 Chrome 里（没有 window.desktop → embedNav 为 undefined），
// 只覆盖得到「退化用 tab.url」那条路径；「活标签靠 viewId 反查 embedNav.targets」这条真实浏览时
// 每天走的路径，截图证明不了。故拿迁移前 RightPanel 的原逻辑当**一次性对照基准**
//（下面的 old* 是迁移那一刻的逐字副本，别改它 —— 它的价值就在于停在那一刻）。
function oldTabLabel(url: string, title: string): string {
  if (url.startsWith('file://')) {
    const base = fileUrlToPath(url).split('/').filter(Boolean).pop()
    if (base) return base
  }
  if (title) return title
  try {
    return new URL(url).host || url
  } catch {
    return url
  }
}
function oldTabIcon(url: string) {
  switch (visualKindOf(fileUrlToPath(url) || url)) {
    case 'xlsx': return BarChart({ size: 12 })
    case 'image': return ImageIcon({ size: 12 })
    case 'pdf':
    case 'docx':
    case 'pptx': return FileIcon({ size: 12 })
    case 'browser': return Globe({ size: 12 })
    default: return /^https?:/.test(url) ? Globe({ size: 12 }) : FileIcon({ size: 12 })
  }
}
type Targets = { id: string; url: string; title: string }[]
const oldTitle = (tab: WebTabInstance, targets: Targets | undefined, t: (k: string) => string) => {
  const tp = targets?.find((x) => x.id === tab.viewId)
  return oldTabLabel(tp?.url ?? tab.url ?? '', tp?.title ?? '') || t('rp.tab.web')
}
const oldIconUrl = (tab: WebTabInstance, targets: Targets | undefined) =>
  targets?.find((x) => x.id === tab.viewId)?.url ?? tab.url ?? ''
const html = (el: unknown) => renderToStaticMarkup(el as Parameters<typeof renderToStaticMarkup>[0])
const tStub = (k: string) => (k === 'rp.tab.web' ? '网页' : k)

const CASES: { name: string; tab: WebTabInstance; targets?: Targets }[] = [
  { name: '空标签（无 url 无 viewId）→ kind 文案', tab: { id: 'a', kind: 'web' } },
  { name: '待重建标签：https url → 主机名', tab: { id: 'b', kind: 'web', url: 'https://example.com/a/b?x=1' } },
  { name: '待重建标签：file:// pdf → 文件名', tab: { id: 'c', kind: 'web', url: 'file:///w/report.pdf' } },
  { name: '待重建标签：file:// xlsx → 文件名 + 表格图标', tab: { id: 'd', kind: 'web', url: 'file:///w/sheet.xlsx' } },
  { name: '待重建标签：file:// 图片', tab: { id: 'e', kind: 'web', url: 'file:///w/pic.png' } },
  { name: '待重建标签：file:// docx', tab: { id: 'f', kind: 'web', url: 'file:///w/doc.docx' } },
  {
    name: '活标签：池里已导航到新页 → 用池的 url/title（标签快照是旧的）',
    tab: { id: 'g', kind: 'web', viewId: 't7', url: 'https://old.example.com/' },
    targets: [{ id: 't7', url: 'https://new.example.com/x', title: '新页面标题' }],
  },
  {
    name: '活标签：池里有 url 没 title → 主机名',
    tab: { id: 'h', kind: 'web', viewId: 't8', url: 'https://old.example.com/' },
    targets: [{ id: 't8', url: 'https://cdn.example.com/f', title: '' }],
  },
  { name: '活标签：viewId 查不到目标 → 退化 tab.url', tab: { id: 'i', kind: 'web', viewId: 't9', url: 'https://gone.example.com/' } },
  { name: '活标签：about:blank 自举占位', tab: { id: 'j', kind: 'web', viewId: 't10' }, targets: [{ id: 't10', url: 'about:blank', title: '' }] },
]

test('web provider 的 title/icon 与迁移前 RightPanel 的 web 分支逐例等价', () => {
  const p = getSurface('web')
  assert.ok(p)
  try {
    for (const c of CASES) {
      useAppStore.setState({ embedNav: c.targets ? ({ targets: c.targets } as never) : undefined })
      assert.equal(p.title(c.tab, tStub), oldTitle(c.tab, c.targets, tStub), `title 不等价：${c.name}`)
      assert.equal(
        html(p.icon?.(c.tab)),
        html(oldTabIcon(oldIconUrl(c.tab, c.targets))),
        `icon 不等价：${c.name}`,
      )
    }
  } finally {
    useAppStore.setState({ embedNav: undefined })
  }
})

test('web provider 读的是 store 里的 embedNav（与 RP 订阅同一字段），不是标签快照', () => {
  const p = getSurface('web')
  assert.ok(p)
  const tab: WebTabInstance = { id: 'k', kind: 'web', viewId: 't11', url: 'https://snapshot.example.com/' }
  try {
    // 无 embedNav（纯浏览器里预览 / 重启后）：退化到标签快照
    useAppStore.setState({ embedNav: undefined })
    assert.equal(p.title(tab, tStub), 'snapshot.example.com')
    // 主进程推来导航态之后：立刻跟上（RP 订阅了同一字段 → 值变了必然重渲染 → 这里读到新值）
    useAppStore.setState({
      embedNav: { targets: [{ id: 't11', url: 'https://live.example.com/', title: '实时标题' }] } as never,
    })
    assert.equal(p.title(tab, tStub), '实时标题')
    assert.equal(html(p.icon?.(tab)), html(Globe({ size: 12 })))
  } finally {
    useAppStore.setState({ embedNav: undefined })
  }
})

// —— R07：activity provider（平移 + 渲染委派）——
// 与 R06b 的 web 测试同构：元数据逐条钉死（PR-07 的验收口径写在 02-pr-plan PR-07），
// 渲染断言要求**元素类型就是平移后的 ActivityTab** 且**不带任何 props** ——
// 一旦有人给它塞 RP 专属 props（scope/menuOpen/…），「ActivityTab 不依赖 RP 局部 state」
// 这条前置就不再成立，该在这里先红，而不是等切标签时组件重挂载丢抽屉。
test('builtin/index 的副作用注册了 activity provider，且元数据符合 PR-07 口径', () => {
  const p = getSurface('activity')
  assert.ok(p, 'activity provider 未注册')
  assert.equal(p.kind, 'activity')
  assert.equal(p.namespace, 'builtin')
  assert.equal(p.group, 'content')
  assert.equal(p.order, 30)
  // 会话级单例：一个会话只有一条「当前链路」，不像 file/web 多实例
  assert.equal(p.singleton, true)
  assert.equal(p.allowDuplicate, false)
  assert.equal(p.scope, 'session')
  assert.equal(p.weight, 'light')
  assert.deepEqual(p.capabilities, ['read', 'view'])
  assert.deepEqual(p.defaultSpec?.({} as never), { kind: 'activity' })
  // title 与 RightPanel 的 kindLabel('activity') 同一份 i18n（不另起文案）
  const tAct = (k: string) => (k === 'rp.tab.activity' ? '链路' : k)
  assert.equal(p.title({ id: 'a', kind: 'activity' }, tAct), '链路')
  assert.equal(html(p.icon?.({ id: 'a', kind: 'activity' })), html(Activity({ size: 12 })))
})

test('activity provider 的 render 只接 activity 标签，且不向组件传 props', () => {
  const p = getSurface('activity')
  assert.ok(p)
  const el = p.render({ tab: { id: 'a', kind: 'activity' }, scope: 's' }) as unknown as {
    type: unknown
    props: Record<string, unknown>
  }
  assert.equal(el.type, ActivityTab)
  assert.deepEqual(el.props, {})
  // 别的 kind 明确返回 null（RightPanel 的判断依赖这一点；见 activitySurface.tsx 注释）
  assert.equal(p.render({ tab: { id: 'g', kind: 'git' }, scope: 's' }), null)
})

// —— PR-11：defaultSpec 是 makeTab 的唯一初值来源 ——
//
// 这两条把「从 store 的 makeTab switch 搬到 provider」的逻辑**逐字段**钉住：搬家最容易的
// 回归是「搬了形状、丢了某个可选字段」（toolId/startLine/openSeq 少一个，症状是重启或
// 点「文件」标签后定位不到行、或视图不复位）。期望值直接写成**存储里的最终形状**，
// 不写成「等于 focusSpec(...)」—— 那等于用实现证明实现。
test('file provider 的 defaultSpec = fileFocus 的 payload + openSeq=1（与旧 makeTab 逐字段一致）', () => {
  const p = getSurface('file')
  assert.ok(p, 'file provider 未注册')
  // ① fileFocus 空（没定位过任何文件）→ 空 file 标签，但 openSeq 仍是 1
  assert.deepEqual(p.defaultSpec?.({} as never), { kind: 'file', openSeq: 1 })
  // ② 有 toolId → diff，并带上 toolId / startLine
  assert.deepEqual(
    p.defaultSpec?.({ fileFocus: { path: '/w/src/a.ts', toolId: 'tool-1', seq: 3, startLine: 12 } } as never),
    { kind: 'file', path: '/w/src/a.ts', mode: 'diff', toolId: 'tool-1', startLine: 12, openSeq: 1 },
  )
  // ③ 无 toolId → source（「有 toolId 才 diff」这条判据只该有一份，仍在 store 的 focusSpec 里）
  assert.deepEqual(
    p.defaultSpec?.({ fileFocus: { path: '/w/src/b.ts', seq: 4 } } as never),
    { kind: 'file', path: '/w/src/b.ts', mode: 'source', openSeq: 1 },
  )
})

test('web provider 的 defaultSpec 只在「池里有真实页面」时带 viewId（about:blank 不算）', () => {
  const p = getSurface('web')
  assert.ok(p, 'web provider 未注册')
  // ① 没有 embedNav → 空 web 标签（交给 syncWebTabs 的收养分支）
  assert.deepEqual(p.defaultSpec?.({} as never), { kind: 'web' })
  // ② activeId 指向 about:blank（池里的常驻自举载体）→ **不认**这个 id
  assert.deepEqual(
    p.defaultSpec?.({
      embedNav: { activeId: 't1', targets: [{ id: 't1', url: 'about:blank' }] },
    } as never),
    { kind: 'web' },
  )
  // ③ 真实页面 → 带上 viewId
  assert.deepEqual(
    p.defaultSpec?.({
      embedNav: { activeId: 't7', targets: [{ id: 't7', url: 'https://a.example/' }] },
    } as never),
    { kind: 'web', viewId: 't7' },
  )
})
