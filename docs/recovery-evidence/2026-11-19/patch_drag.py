# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:80]
    s=s.replace(old,new,1)

rep('''| 元素（默认） | 直接点 | `element`；Shift 点 = `elements` |
| 区域 | 工具条「区域」或按住 `R` 拖 | `region` |''','''| 元素（默认） | 直接点；Shift 点追加；**拖框** | `element` / `elements`（拖框 = 框选，按 LCA 成组，≤20） |
| 区域 | 工具条「区域」或按住 `R` 拖 | `region`（不要求 DOM，canvas/跨域 iframe 也能用） |''')

rep('''// 区域拖拽：mousedown 记起点，mousemove 画框，mouseup 收口。
// 用 pointer 事件而不是 mouse：触控板/触屏一致。
addEventListener('pointerdown', (e) => {
  if (mode !== 'region' || e.button !== 0) return
  dragRect = { x: e.clientX, y: e.clientY, w: 0, h: 0 }
  e.preventDefault()
}, true)
addEventListener('pointermove', (e) => {
  if (!dragRect) return
  dragRect.w = e.clientX - dragRect.x
  dragRect.h = e.clientY - dragRect.y
  drawRect(dragRect)
}, true)
addEventListener('pointerup', (e) => {
  if (!dragRect) return
  const r = normalizeRect(dragRect)
  dragRect = null
  if (r.w < 8 || r.h < 8) return // 误触：忽略
  current = { __region: r, __elements: elementsIn(r) }
  showBar(null)
  void post('selection.changed', regionPayload(r))
}, true)''','''// 拖拽：元素模式 = 框选（→ elements），区域模式 = 区域（→ region）。
// 用 pointer 事件而不是 mouse：触控板/触屏一致；阈值 8px 用来区分「点击」与「拖拽」。
addEventListener('pointerdown', (e) => {
  if (e.button !== 0 || (mode !== 'region' && mode !== 'element')) return
  dragRect = { x: e.clientX, y: e.clientY, w: 0, h: 0 }
}, true)
addEventListener('pointermove', (e) => {
  if (!dragRect) return
  dragRect.w = e.clientX - dragRect.x
  dragRect.h = e.clientY - dragRect.y
  drawRect(dragRect)
}, true)
addEventListener('pointerup', () => {
  if (!dragRect) return
  const r = normalizeRect(dragRect)
  dragRect = null
  if (r.w < 8 && r.h < 8) return // 没动：交给 click 处理单点
  suppressClick = true // 拖完的这一次 click 要丢掉，否则会把框选覆盖成单选
  if (mode === 'region') {
    current = { __region: r, __elements: elementsIn(r) }
    showBar(null)
    void post('selection.changed', regionPayload(r))
    return
  }
  // 元素模式：框选 = 区域内的节点，按文档序取前 20（LCA 只用于展示成组提示）
  picked = elementsIn(r).slice(0, 20)
  if (picked.length) { drawPicked(picked); showBar(picked[0]) }
}, true)''')

rep('''document.addEventListener('click', (e) => {
  if (mode !== 'element') return''','''document.addEventListener('click', (e) => {
  if (mode !== 'element') return
  if (suppressClick) { suppressClick = false; e.preventDefault(); e.stopPropagation(); return }''')

# add suppressClick declaration
rep('''let dragRect = null      // 区域拖拽中的矩形（视口坐标）''','''let dragRect = null      // 区域拖拽中的矩形（视口坐标）
let suppressClick = false // 拖拽结束后的那次 click 要丢弃''')

# phasing note after the taxonomy table's 边界 table
rep('''**明确不引用**（避免把上下文撑爆，也避免把不可信内容当指令）：''','''> **分期**：Phase B1 支持 `element / elements / text / region / page / artifact / file` 七种；
> `comment`（批注 pin）放 B2 —— 它需要 pin 的放置/编辑 UI 与「多条意见一次提交」的交互，
> 与只读引用不是同一档工作量。

**明确不引用**（避免把上下文撑爆，也避免把不可信内容当指令）：''')
open(p,'w').write(s)
print('ok')
