# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
anchor = "### 23.2 服务端选区引用表（`desktop/bridge/selection_refs.go`，新增全文）"
addition = r'''### 23.1c Inspector：四种取选模式（对 §8.5 的增量）

§8.5 的 inspector 只做了「点选单个元素」。要支撑 §23.1b 的 8 种 kind，浮动条上需要
**四个模式**（其中两个是自动的）：

| 模式 | 进入方式 | 产出 kind |
|---|---|---|
| 元素（默认） | 直接点 | `element`；Shift 点 = `elements` |
| 区域 | 工具条「区域」或按住 `R` 拖 | `region` |
| 文字 | 直接拖选文本（自动，无需切模式） | `text` |
| 整页 | 工具条「整页」 | `page` |

**增量代码**（接在 §8.5 的 `infoOf` / 事件绑定之后；保持同样的 `post()` 与 Shadow DOM 高亮层）：

```js
// —— 模式状态：'element' | 'region'；text 由 selectionchange 自动识别 ——
let mode = 'element'
let picked = []          // Shift 多选：按点选顺序（语义提升后的节点）
let dragRect = null      // 区域拖拽中的矩形（视口坐标）

// 工具条追加两个按钮与一个提示位（沿用 .bar 的样式）
const tools = document.createElement('div')
tools.className = 'row'
tools.innerHTML = '<button class="ghost" data-mode="element">元素</button>' +
  '<button class="ghost" data-mode="region">区域</button>' +
  '<button class="ghost" data-mode="page">整页</button>' +
  '<span class="hint"></span>'
bar.prepend(tools)
tools.addEventListener('click', (e) => {
  const m = e.target && e.target.getAttribute('data-mode')
  if (m === 'page') { void post('selection.commit', commitPayload('page', pagePayload())) ; return }
  if (m) { mode = m; tools.querySelectorAll('button').forEach((b) => b.classList.toggle('on', b.getAttribute('data-mode') === m)) }
})

// 元素点选：Shift 追加；普通点选清空重来
document.addEventListener('click', (e) => {
  if (mode !== 'element') return
  const el = lift(e.target)
  if (!el) return
  e.preventDefault(); e.stopPropagation()
  if (e.shiftKey) {
    if (!picked.includes(el) && picked.length < 20) picked.push(el)
  } else {
    picked = [el]
  }
  drawPicked(picked)
  showBar(picked[picked.length - 1])
})

// 区域拖拽：mousedown 记起点，mousemove 画框，mouseup 收口。
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
}, true)

// 文字选区：selectionchange 非折叠时提示「引用这段文字」。
document.addEventListener('selectionchange', () => {
  const sel = getSelection()
  if (!sel || sel.isCollapsed || !sel.toString().trim()) return
  const range = sel.getRangeAt(0)
  const anchor = lift(range.startContainer.parentElement)
  textPick = { text: sel.toString().slice(0, 300), anchor, range: rangeOf(range), context: contextOf(range) }
  tools.querySelector('.hint').textContent = '引用这段文字？点「引用」'
})

// —— payload 构造：每种 kind 只带 §23.1b 列出的字段，绝不多带 ——
function payloadFor(kind) {
  if (kind === 'region') return regionPayload(current.__region)
  if (kind === 'page') return pagePayload()
  if (kind === 'text') return { ...textPick, anchor: infoOf(textPick.anchor) }
  if (kind === 'elements') return { selector: selectorOf(picked[0]), items: picked.map(infoOf), truncated: picked.length >= 20 }
  return infoOf(picked[0])
}

// 提交：两个动作都带 kind；「引用」= reference，「发送」= edit
function commitPayload(kind, payload) {
  return { kind, artifact_id: ARTIFACT_ID, version: VERSION, selection: payload,
           mode: mode === 'region' ? 'reference' : 'reference' }
}
```

> 高亮层与 `selectorOf` / `infoOf` 全部复用 §8.5，不新写一套。`drawPicked` 只是把
> 多个高亮框一起画；`drawRect` 画区域框。
>
> **`document.addEventListener('click')` 要保留 capture 阶段的 `preventDefault`**
>（设计稿要求「点选不触发页面自己的跳转/提交」）：上面的示例省略了 `true` 参数是为了可读，
> 实现时必须与原 §8.5 的那一版合并成**一个** listener，不要绑两次。

'''
assert anchor in s
s=s.replace(anchor, addition+anchor,1)
open(p,'w').write(s)
print('ok')
