<!--
visual-ui reference · Charts and data marks
Read this file only when the answer needs one of these shapes.
Every snippet is self-contained: copy it, then replace the content.
Keep the <style> and the validation logic; they encode the frame's constraints.
-->

# Charts and data marks

Choosing between them:

- **Bar** — magnitude across categories. Always starts at zero.
- **Line** — change over continuous time. May start above zero *if the note says so*.
- **Heatmap** — density across two dimensions (rows × columns). Print the value in the
  cell when there are fewer than ~30 cells; colour alone is not readable.

Shared geometry rule (applies to every SVG here): pick a pixel box
`viewBox="0 0 640 H"`, reserve margins — left 60 for y labels, bottom 26–40 for x labels,
top 20 — then `y = baseline - value / max * plotH`. Compute the numbers, do not place by eye.

### Bar chart (SVG, honest axes)

```html
<div class="card">
  <h3>各渠道访问量</h3>
  <svg viewBox="0 0 640 250" width="100%" role="img"
       aria-label="五个渠道访问量柱状图，单位千次，自然搜索最高 42.3 千次">
    <g class="grid">
      <line x1="60" y1="210" x2="620" y2="210"/><line x1="60" y1="172" x2="620" y2="172"/>
      <line x1="60" y1="134" x2="620" y2="134"/><line x1="60" y1="96"  x2="620" y2="96"/>
      <line x1="60" y1="58"  x2="620" y2="58"/><line x1="60" y1="20"  x2="620" y2="20"/>
    </g>
    <g class="ylab">
      <text x="52" y="214" text-anchor="end">0</text><text x="52" y="176" text-anchor="end">10</text>
      <text x="52" y="138" text-anchor="end">20</text><text x="52" y="100" text-anchor="end">30</text>
      <text x="52" y="62"  text-anchor="end">40</text><text x="52" y="24"  text-anchor="end">50</text>
    </g>
    <text x="34" y="14" class="unit">千次</text>
    <g class="bars">
      <rect x="88"  y="49.26"  width="56" height="160.74" rx="3" class="top"/>
      <rect x="200" y="100.94" width="56" height="109.06" rx="3"/>
      <rect x="312" y="136.28" width="56" height="73.72"  rx="3"/>
      <rect x="424" y="167.44" width="56" height="42.56"  rx="3"/>
      <rect x="536" y="176.18" width="56" height="33.82"  rx="3"/>
    </g>
    <g class="val">
      <text x="116" y="43">42.3</text><text x="228" y="95">28.7</text>
      <text x="340" y="130">19.4</text><text x="452" y="161">11.2</text>
      <text x="564" y="170">8.9</text>
    </g>
    <g class="xlab">
      <text x="116" y="232">自然搜索</text><text x="228" y="232">应用商店</text>
      <text x="340" y="232">社交推荐</text><text x="452" y="232">邮件</text>
      <text x="564" y="232">付费投放</text>
    </g>
  </svg>
  <p class="note">单位：千次 · 纵轴从 0 起算 · 示例数据</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .card h3 { margin: 0 0 12px; font-size: 15px; font-weight: 600; }
  svg .grid line { stroke: var(--hai-line); }
  svg .grid line:first-child { stroke: var(--hai-text-2); stroke-opacity: .5; }
  svg .bars rect { fill: var(--hai-accent); fill-opacity: .4; }
  svg .bars rect.top { fill-opacity: .95; }
  svg text { fill: var(--hai-text-2); font-size: 12px; text-anchor: middle; }
  svg .ylab text { text-anchor: end; }
  svg .val text { fill: var(--hai-text); font-size: 11.5px; font-variant-numeric: tabular-nums; }
  svg .unit { font-size: 11px; text-anchor: start; }
  .note { margin: 10px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
```

Geometry rule for every SVG chart: pick a pixel box (`viewBox="0 0 640 H"`), reserve margins
(left 60 for y labels, bottom 26–40 for x labels, top 20), then `y = baseline − value / max * plotH`.
Write the computed numbers out; do not hand-place bars by eye.

### Line chart

```html
<div class="card">
  <h3>近 6 个月活跃用户</h3>
  <svg viewBox="0 0 640 240" width="100%" role="img"
       aria-label="折线图：近六个月月活，单位千人，从 18.2 增至 28.2">
    <g class="grid">
      <line x1="56" y1="200" x2="620" y2="200"/><line x1="56" y1="155" x2="620" y2="155"/>
      <line x1="56" y1="110" x2="620" y2="110"/><line x1="56" y1="65"  x2="620" y2="65"/>
      <line x1="56" y1="20"  x2="620" y2="20"/>
    </g>
    <g class="ylab">
      <text x="48" y="204" text-anchor="end">10</text><text x="48" y="159" text-anchor="end">15</text>
      <text x="48" y="114" text-anchor="end">20</text><text x="48" y="69"  text-anchor="end">25</text>
      <text x="48" y="24"  text-anchor="end">30</text>
    </g>
    <text x="30" y="14" class="unit">千人</text>
    <polyline class="line" points="90,126 196,108 302,92 408,72 514,56 600,36"/>
    <g class="dots">
      <circle cx="90"  cy="126" r="3.5"/><circle cx="196" cy="108" r="3.5"/>
      <circle cx="302" cy="92"  r="3.5"/><circle cx="408" cy="72"  r="3.5"/>
      <circle cx="514" cy="56"  r="3.5"/><circle cx="600" cy="36"  r="3.5"/>
    </g>
    <g class="xlab">
      <text x="90" y="222">5 月</text><text x="196" y="222">6 月</text>
      <text x="302" y="222">7 月</text><text x="408" y="222">8 月</text>
      <text x="514" y="222">9 月</text><text x="600" y="222">10 月</text>
    </g>
  </svg>
  <p class="note">单位：千人 · 示例数据；纵轴不从 0 起算（趋势图，非量值对比）</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .card h3 { margin: 0 0 12px; font-size: 15px; font-weight: 600; }
  svg .grid line { stroke: var(--hai-line); }
  svg .grid line:first-child { stroke: var(--hai-text-2); stroke-opacity: .5; }
  svg .line { fill: none; stroke: var(--hai-accent); stroke-width: 2;
              stroke-linejoin: round; stroke-linecap: round; }
  svg .dots circle { fill: var(--hai-surface); stroke: var(--hai-accent); stroke-width: 2; }
  svg text { fill: var(--hai-text-2); font-size: 12px; text-anchor: middle; }
  svg .ylab text { text-anchor: end; }
  svg .unit { font-size: 11px; text-anchor: start; }
  .note { margin: 10px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
```

A line chart may start above zero **only if you say so** in the note. Bars must start at zero.

**Optional sort toggle** — conditional per the Interaction policy: only add it when the original
order carries meaning that sorting would destroy (e.g. a fixed weekday sequence). Otherwise sort the
categories once, statically, and drop the control. If you do add it, re-render the bars' `x`
positions and keep every label attached:

```html
<button id="sortBtn" aria-pressed="false">按数值排序</button>
<script>
(function () {
  var btn = document.getElementById('sortBtn');
  var svg = document.querySelector('svg[role="img"]');
  if (!btn || !svg) return;
  var bars = Array.prototype.slice.call(svg.querySelectorAll('.bars rect'));
  var labels = Array.prototype.slice.call(svg.querySelectorAll('.xlab text'));
  var values = Array.prototype.slice.call(svg.querySelectorAll('.val text'));
  if (bars.length !== labels.length || bars.length !== values.length) return; // 结构自检
  var original = bars.map(function (b) { return b.getAttribute('x'); });
  var sorted = false;

  btn.addEventListener('click', function () {
    sorted = !sorted;
    btn.setAttribute('aria-pressed', sorted ? 'true' : 'false');
    btn.textContent = sorted ? '恢复原顺序' : '按数值排序';

    var order = bars.map(function (b, i) {
      return { i: i, v: parseFloat(values[i].textContent) || 0 };
    }).sort(function (a, b) { return sorted ? b.v - a.v : a.i - b.i; });

    order.forEach(function (o, slot) {
      var x = original[slot];
      var b = bars[o.i], l = labels[o.i], v = values[o.i];
      if (b) b.setAttribute('x', x);
      // 标签与数值跟着柱子走：x 同源，避免错位
      [l, v].forEach(function (t) { if (t) t.setAttribute('x',
        String(parseFloat(x) + parseFloat(b ? b.getAttribute('width') : '0') / 2)); });
    });
  });
})();
</script>
```

Sorting must keep **bars, category labels and value labels in the same trio** — the usual bug is
moving the rects and leaving the labels behind. If that is hard to guarantee, sort the data in the
markup instead and skip the control.

### Heatmap (one hue, opacity scale)

The value lives **in the DOM** on every cell, so it is readable without a pointer (keyboard tab
walks the cells, screen readers announce the label). Hover/focus only promotes it visually.

```html
<div class="card">
  <h3>按小时 × 星期的请求密度</h3>
  <div class="hm" role="table" aria-label="请求密度热力图：行是星期，列是时段，单位千次">
    <div class="hm-row" role="row"><span class="hm-lab" role="rowheader">周一</span>
      <i role="cell" tabindex="0" data-v="1.2" style="--v:.2">1.2</i>
      <i role="cell" tabindex="0" data-v="2.1" style="--v:.35">2.1</i>
      <i role="cell" tabindex="0" data-v="3.6" style="--v:.6">3.6</i>
      <i role="cell" tabindex="0" data-v="5.4" style="--v:.9">5.4</i>
      <i role="cell" tabindex="0" data-v="4.2" style="--v:.7">4.2</i>
      <i role="cell" tabindex="0" data-v="1.8" style="--v:.3">1.8</i></div>
    <div class="hm-row" role="row"><span class="hm-lab" role="rowheader">周二</span>
      <i role="cell" tabindex="0" data-v="1.5" style="--v:.25">1.5</i>
      <i role="cell" tabindex="0" data-v="2.4" style="--v:.4">2.4</i>
      <i role="cell" tabindex="0" data-v="3.9" style="--v:.65">3.9</i>
      <i role="cell" tabindex="0" data-v="6.0" style="--v:1">6.0</i>
      <i role="cell" tabindex="0" data-v="4.5" style="--v:.75">4.5</i>
      <i role="cell" tabindex="0" data-v="2.1" style="--v:.35">2.1</i></div>
  </div>
  <div class="legend"><span>低</span>
    <i style="--v:.15"></i><i style="--v:.4"></i><i style="--v:.7"></i><i style="--v:1"></i>
    <span>高</span><span class="unit">单位：千次</span></div>
  <p class="note" id="hmnote" aria-live="polite">行 = 星期，列 = 时段（4 小时一格）· 示例数据</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .card h3 { margin: 0 0 12px; font-size: 15px; font-weight: 600; }
  .hm { display: flex; flex-direction: column; gap: 4px; }
  .hm-row { display: grid; grid-template-columns: 44px repeat(6, 1fr); gap: 4px;
            align-items: center; }
  .hm-lab { font-size: 11px; color: var(--hai-text-2); }
  /* 数值直接压在格子上。**不用 opacity**：opacity 会作用到整棵子树，格内数字
     会跟着一起变淡，浅色格里基本看不见。改用 color-mix 只压底色，文字恒为
     --hai-text，两种主题下都保对比度。 */
  .hm i { display: grid; place-items: center; height: 26px; border-radius: 4px;
          font-style: normal; font-size: 10.5px; font-variant-numeric: tabular-nums;
          color: var(--hai-text); cursor: default;
          background: color-mix(in srgb,
            var(--hai-accent) calc(var(--v, .3) * 100%), var(--hai-bg)); }
  .hm i:hover { background: color-mix(in srgb,
            var(--hai-accent) calc(var(--v, .3) * 70% + 30%), var(--hai-bg)); }
  .hm i:focus-visible { outline: 2px solid var(--hai-accent); outline-offset: 1px; }
  .legend { display: flex; align-items: center; gap: 6px; margin-top: 12px;
            font-size: 11px; color: var(--hai-text-2); }
  .legend i { width: 18px; height: 10px; border-radius: 2px;
              background: color-mix(in srgb,
                var(--hai-accent) calc(var(--v, .3) * 100%), var(--hai-bg)); }
  .legend .unit { margin-left: auto; }
  .note { margin: 10px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
<script>
(function () {
  var cells = Array.prototype.slice.call(document.querySelectorAll('.hm [role="cell"]'));
  var note = document.getElementById('hmnote');
  if (!note || cells.length === 0) return;
  var base = note.textContent;

  function describe(cell) {
    var rowLabel = cell.parentNode.querySelector('.hm-lab');
    var peers = Array.prototype.slice.call(
      cell.parentNode.querySelectorAll('[role="cell"]'));
    var col = peers.indexOf(cell) + 1;   // 只数格子，不受行首标签影响
    return (rowLabel ? rowLabel.textContent : '?') +
      ' · 第 ' + col + ' 时段 · ' + cell.getAttribute('data-v') + ' 千次';
  }

  cells.forEach(function (c) {
    c.addEventListener('mouseenter', function () { note.textContent = describe(c); });
    c.addEventListener('focus', function () { note.textContent = describe(c); });
    c.addEventListener('blur', function () { note.textContent = base; });
  });
  document.querySelector('.hm').addEventListener('mouseleave', function () {
    note.textContent = base;
  });
})();
</script>
```

Two rules that make a heatmap readable:

- **Print the value in the cell** when there are fewer than ~30 cells. Colour alone fails for
  colour-blind readers and changes meaning between light and dark themes.
- **Keep the text on a fixed token colour** (`--hai-text`) and push contrast through cell opacity,
  never the other way round — light text on a light accent cell is the usual failure.
