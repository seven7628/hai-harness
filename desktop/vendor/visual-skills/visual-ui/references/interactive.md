<!--
visual-ui reference · Interactive widgets
Read this file only when the answer needs one of these shapes.
Every snippet is self-contained: copy it, then replace the content.
Keep the <style> and the validation logic; they encode the frame's constraints.
-->

# Interactive widgets

Both snippets put `<script>` **after** the markup they query, wrap everything in an IIFE,
and never touch `localStorage` (it throws in this frame's opaque origin — keep state in a
closure variable).

For tabs, the two failures to avoid are: a panel container without `min-height` (the frame
re-measures on every switch and the block jumps) and missing arrow-key navigation.

### Tabbed panels

Tabs are the right answer when several views belong to one answer. They matter extra here: the frame
caps at 560 px, so tabs keep content **above the fold** instead of forcing an inner scroll.

```html
<div class="card">
  <div class="tabs" role="tablist" aria-label="视图">
    <button role="tab" id="t1" aria-controls="p1" aria-selected="true">概览</button>
    <button role="tab" id="t2" aria-controls="p2" aria-selected="false" tabindex="-1">明细</button>
  </div>
  <div class="panels">
    <section role="tabpanel" id="p1" aria-labelledby="t1" tabindex="0">概览内容</section>
    <section role="tabpanel" id="p2" aria-labelledby="t2" tabindex="0" hidden>明细内容</section>
  </div>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .tabs { display: flex; gap: 4px; padding: 3px; margin-bottom: 14px;
          background: var(--hai-bg); border: 1px solid var(--hai-line); border-radius: 8px;
          overflow-x: auto; }
  .tabs [role="tab"] { flex: 0 0 auto; padding: 5px 13px; font: inherit; font-size: 12.5px;
          color: var(--hai-text-2); background: transparent; border: 1px solid transparent;
          border-radius: 6px; cursor: pointer; }
  .tabs [role="tab"]:hover { color: var(--hai-text); }
  .tabs [role="tab"][aria-selected="true"] { color: var(--hai-text); font-weight: 600;
          background: var(--hai-surface); border-color: var(--hai-line); }
  .tabs [role="tab"]:focus-visible { outline: 2px solid var(--hai-accent); outline-offset: 2px; }
  /* pins the frame height so switching tabs does not make the card jump */
  .panels { min-height: 240px; }
  .panels section:focus-visible { outline: 2px solid var(--hai-accent);
          outline-offset: 4px; border-radius: 6px; }
</style>
<script>
(function () {
  var tabs = Array.prototype.slice.call(document.querySelectorAll('[role="tab"]'));
  var panels = tabs.map(function (t) { return document.getElementById(t.getAttribute('aria-controls')); });
  function select(i, focus) {
    tabs.forEach(function (t, j) {
      var on = i === j;
      t.setAttribute('aria-selected', on ? 'true' : 'false');
      t.tabIndex = on ? 0 : -1;
      if (panels[j]) panels[j].hidden = !on;
    });
    if (focus) tabs[i].focus();
  }
  tabs.forEach(function (t, i) {
    t.addEventListener('click', function () { select(i, false); });
    t.addEventListener('keydown', function (e) {
      var n = null;
      if (e.key === 'ArrowRight') n = (i + 1) % tabs.length;
      else if (e.key === 'ArrowLeft') n = (i - 1 + tabs.length) % tabs.length;
      else if (e.key === 'Home') n = 0;
      else if (e.key === 'End') n = tabs.length - 1;
      if (n !== null) { e.preventDefault(); select(n, true); }
    });
  });
  select(0, false);
})();
</script>
```

Tab rules that prevent the two classic failures:

- **Set `min-height` on the panel container.** Without it the iframe re-measures on every switch and
  the whole block jumps.
- **Implement arrow keys.** Tab without keyboard navigation is a mouse-only widget; `←/→` plus
  `Home/End` is five lines (above).
- Hidden panels use the `hidden` attribute, not `display:none` in CSS — `hidden` is what assistive
  tech reads as "not shown", and it keeps the panel in the DOM so ids stay valid.
- Do not hide the whole panel content behind a tab when it is the *only* content — that is a step for
  nothing.

### Calculator / simulator (live output)

```html
<div class="card">
  <h3>缓存能省下多少数据库压力</h3>
  <div class="field">
    <label for="qps">总请求量 <output id="qv" for="qps">6,000</output> QPS</label>
    <input id="qps" type="range" min="100" max="10000" step="100" value="6000">
  </div>
  <div class="field">
    <label for="hit">缓存命中率 <output id="hv" for="hit">85</output>%</label>
    <input id="hit" type="range" min="0" max="99" step="1" value="85">
  </div>
  <div class="out">
    <div class="big"><span id="db">900</span><span class="u">QPS</span></div>
    <div class="sub">落到数据库的查询量</div>
    <div class="sub2" id="inst">按单实例 1,500 QPS：需要 1 台</div>
  </div>
  <p class="note" id="note">命中率 85% ⇒ 数据库压力降到原来的 15%</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .card h3 { margin: 0 0 14px; font-size: 15px; font-weight: 600; }
  .field { margin-bottom: 12px; }
  .field label { display: block; margin-bottom: 6px; font-size: 12.5px; color: var(--hai-text-2); }
  .field output { color: var(--hai-text); font-weight: 600; font-variant-numeric: tabular-nums; }
  .field input[type="range"] { width: 100%; max-width: 420px;
          accent-color: var(--hai-accent); cursor: pointer; }
  .field input[type="range"]:focus-visible { outline: 2px solid var(--hai-accent);
          outline-offset: 3px; }
  .out { margin: 14px 0 10px; padding: 12px 14px; background: var(--hai-bg);
         border: 1px solid var(--hai-line); border-radius: 8px; }
  .big { font-size: 26px; font-weight: 650; font-variant-numeric: tabular-nums; }
  .big .u { margin-left: 4px; font-size: 12px; font-weight: 400; color: var(--hai-text-2); }
  .sub { margin-top: 2px; font-size: 11.5px; color: var(--hai-text-2); }
  .sub2 { margin-top: 7px; font-size: 12px; font-variant-numeric: tabular-nums; }
  .note { margin: 8px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
<script>
(function () {
  var CAP = 1500;
  var qps = document.getElementById('qps'), hit = document.getElementById('hit');
  var qv = document.getElementById('qv'), hv = document.getElementById('hv');
  var db = document.getElementById('db'), inst = document.getElementById('inst');
  var note = document.getElementById('note');
  function fmt(n) { return n.toLocaleString('zh-CN'); }
  function update() {
    var total = qps.valueAsNumber, rate = hit.valueAsNumber;
    if (!Number.isFinite(total) || !Number.isFinite(rate)) return;  // never print NaN
    var remain = 1 - rate / 100;
    var dbQps = Math.round(total * remain);
    qv.textContent = fmt(total);
    hv.textContent = String(rate);
    db.textContent = fmt(dbQps);
    inst.textContent = dbQps === 0
      ? '按单实例 ' + fmt(CAP) + ' QPS：无需实例（全部命中）'
      : '按单实例 ' + fmt(CAP) + ' QPS：需要 ' + Math.ceil(dbQps / CAP) + ' 台';
    note.textContent = '命中率 ' + rate + '% ⇒ 数据库压力降到原来的 ' +
      (dbQps === 0 ? '0%' : Math.round(remain * 1000) / 10 + '%');
  }
  qps.addEventListener('input', update);
  hit.addEventListener('input', update);
  update();
})();
</script>
```
