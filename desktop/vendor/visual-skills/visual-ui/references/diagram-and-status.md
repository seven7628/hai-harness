<!--
visual-ui reference · Diagrams and status lists
Read this file only when the answer needs one of these shapes.
Every snippet is self-contained: copy it, then replace the content.
Keep the <style> and the validation logic; they encode the frame's constraints.
-->

# Diagrams and status lists

- **Diagram** — hierarchy or flow. One `<svg>`, arrow markers via `<defs>`, label font
  ≥ 12 px, text centred **inside** boxes. More than ~6 nodes: switch to a vertical layout.
- **KPI strip** — 2–4 scannable numbers, one big number per card, unit and period attached.
- **Status list** — many items with a per-row state. Pair the icon with text; colour alone
  fails for colour-blind users and in the wrong theme.

### Diagram (SVG + marker)

```html
<div class="card">
  <h3>请求链路</h3>
  <svg viewBox="0 0 640 210" width="100%" role="img"
       aria-label="流程图：客户端到网关到应用再到上游；应用与缓存之间双向读写">
    <defs>
      <marker id="ar" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7"
              orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="currentColor"/></marker>
    </defs>
    <g class="edges">
      <line x1="130" y1="52" x2="176" y2="52" marker-end="url(#ar)"/>
      <line x1="290" y1="52" x2="336" y2="52" marker-end="url(#ar)"/>
      <line x1="450" y1="52" x2="496" y2="52" marker-end="url(#ar)"/>
      <line x1="395" y1="74" x2="395" y2="126" marker-end="url(#ar)"/>
    </g>
    <g class="boxes">
      <rect x="20"  y="30" width="110" height="44" rx="7"/>
      <rect x="180" y="30" width="110" height="44" rx="7"/>
      <rect x="340" y="30" width="110" height="44" rx="7"/>
      <rect x="500" y="30" width="110" height="44" rx="7"/>
      <rect x="340" y="130" width="110" height="44" rx="7" class="store"/>
    </g>
    <g class="labs">
      <text x="75" y="57">客户端</text><text x="235" y="57">网关</text>
      <text x="395" y="57">应用</text><text x="555" y="57">上游服务</text>
      <text x="395" y="157">缓存</text>
    </g>
  </svg>
  <p class="note">箭头表示请求方向；未标命中率——没有实际数据时不做定量结论</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; color: var(--hai-text-2); }
  .card h3 { margin: 0 0 10px; font-size: 15px; font-weight: 600; color: var(--hai-text); }
  .boxes rect { fill: var(--hai-bg); stroke: var(--hai-line); stroke-width: 1.2; }
  .boxes rect.store { stroke: var(--hai-accent); stroke-width: 1.6; }
  .edges line { stroke: var(--hai-text-2); stroke-width: 1.4; }
  .labs text { fill: var(--hai-text); font-size: 13px; text-anchor: middle; }
  .note { margin: 10px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
```

Diagram rules: boxes ≥ 42 px tall, label font ≥ 12 px, arrow markers via `<defs>` (never a hand-drawn
triangle per edge), and text positioned **inside** boxes centred on the box, not floating near lines.
For more than ~6 nodes, switch to a vertical layered diagram — horizontal flows run out of width fast.

**Optional path highlight** — "hover only" tier: the diagram's data is fixed, so the interaction
must not invent new information, only make an existing path easier to follow. Nodes get
`tabindex="0"` so keyboard users get the same reveal, and the connected edges are declared with
`data-from` / `data-to` rather than guessed from geometry:

```html
<g class="boxes" id="nodes">
  <rect data-node="client" tabindex="0" x="20" y="30" width="110" height="44" rx="7"/>
  <rect data-node="gw"     tabindex="0" x="180" y="30" width="110" height="44" rx="7"/>
  <rect data-node="app"    tabindex="0" x="340" y="30" width="110" height="44" rx="7"/>
</g>
<g class="edges" id="edges">
  <line data-from="client" data-to="gw"/>
  <line data-from="gw"     data-to="app"/>
</g>
<script>
(function () {
  var nodes = Array.prototype.slice.call(document.querySelectorAll('#nodes [data-node]'));
  var edges = Array.prototype.slice.call(document.querySelectorAll('#edges [data-from]'));
  if (nodes.length === 0) return;
  function clear() {
    edges.forEach(function (e) { e.classList.remove('lit'); });
    nodes.forEach(function (n) { n.classList.remove('lit'); });
  }
  function light(name) {
    clear();
    edges.forEach(function (e) {
      if (e.getAttribute('data-from') === name || e.getAttribute('data-to') === name) {
        e.classList.add('lit');
        var other = e.getAttribute('data-from') === name
          ? e.getAttribute('data-to') : e.getAttribute('data-from');
        var n = document.querySelector('#nodes [data-node="' + other + '"]');
        if (n) n.classList.add('lit');
      }
    });
  }
  nodes.forEach(function (n) {
    var name = n.getAttribute('data-node');
    n.addEventListener('mouseenter', function () { light(name); });
    n.addEventListener('focus', function () { light(name); });
    n.addEventListener('mouseleave', clear);
    n.addEventListener('blur', clear);
  });
})();
</script>
```

```css
/* 高亮只改描边/线宽，不改填充色：填充一变就可能压垮文字对比度 */
#edges .lit { stroke: var(--hai-accent); stroke-width: 2.2; }
#nodes .lit { stroke: var(--hai-accent); stroke-width: 1.8; }
#nodes [data-node]:focus-visible { outline: 2px solid var(--hai-accent); outline-offset: 2px; }
```

Do **not** add this to a diagram the reader only needs to read once. It earns its keep when the
question is "what talks to what", not "what is the shape of the system".

### KPI strip

```html
<div class="card">
  <h3>本周关键指标</h3>
  <div class="kpis">
    <div class="kpi"><div class="k">总调用</div><div class="v">91.6<span>千次</span></div>
      <div class="d up">较上周 +6.2%</div></div>
    <div class="kpi"><div class="k">P95 延迟</div><div class="v">214<span>ms</span></div>
      <div class="d down">较上周 +12ms</div></div>
    <div class="kpi"><div class="k">错误率</div><div class="v">0.42<span>%</span></div>
      <div class="d up">较上周 −0.08pt</div></div>
  </div>
  <p class="note">统计周期：2026-10-13 至 10-19 · 示例数据</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .card h3 { margin: 0 0 14px; font-size: 15px; font-weight: 600; }
  .kpis { display: grid; gap: 10px; grid-template-columns: repeat(3, 1fr); }
  .kpi { background: var(--hai-bg); border: 1px solid var(--hai-line);
         border-radius: 8px; padding: 10px 12px; min-width: 0; }
  .kpi .k { font-size: 11px; color: var(--hai-text-2); }
  .kpi .v { margin-top: 3px; font-size: 21px; font-weight: 650;
            font-variant-numeric: tabular-nums; }
  .kpi .v span { margin-left: 2px; font-size: 11px; font-weight: 400; color: var(--hai-text-2); }
  .kpi .d { margin-top: 5px; font-size: 11px; font-variant-numeric: tabular-nums; }
  .kpi .d.up { color: var(--hai-text); }
  .kpi .d.down { color: var(--hai-text-2); }
  .note { margin: 12px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
  @media (max-width: 520px) { .kpis { grid-template-columns: 1fr; } }
</style>
```

### Status list / checklist

```html
<div class="card">
  <h3>发布前检查</h3>
  <ul class="st">
    <li class="ok"><span class="ic" aria-hidden="true">✓</span>
      <div><div class="t">单元测试全绿</div><div class="d">1,284 passed</div></div></li>
    <li class="warn"><span class="ic" aria-hidden="true">!</span>
      <div><div class="t">集成测试有 2 个跳过</div><div class="d">缺少 sandbox 凭证</div></div></li>
    <li class="todo"><span class="ic" aria-hidden="true">·</span>
      <div><div class="t">压测未执行</div><div class="d">计划在预发环境做</div></div></li>
  </ul>
  <p class="note">状态用图标 + 文字表达，不只靠颜色</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .card h3 { margin: 0 0 12px; font-size: 15px; font-weight: 600; }
  .st { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 10px; }
  .st li { display: grid; grid-template-columns: 18px 1fr; gap: 9px; align-items: start; }
  .ic { display: grid; place-items: center; width: 18px; height: 18px; border-radius: 50%;
        font-size: 11px; font-weight: 700; border: 1px solid var(--hai-line);
        color: var(--hai-text-2); }
  .st li.ok .ic { background: var(--hai-accent); border-color: var(--hai-accent);
                  color: var(--hai-surface); }
  .st li.warn .ic { border-color: var(--hai-text); color: var(--hai-text); }
  .t { font-size: 12.5px; }
  .d { margin-top: 2px; font-size: 11px; color: var(--hai-text-2); }
  .note { margin: 12px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
```
