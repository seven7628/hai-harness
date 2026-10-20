<!--
visual-ui reference · Comparison, timeline and progress
Read this file only when the answer needs one of these shapes.
Every snippet is self-contained: copy it, then replace the content.
Keep the <style> and the validation logic; they encode the frame's constraints.
-->

# Comparison, timeline and progress

Use these when the answer is *which one* or *in what order*:

- **Table** — up to ~4 columns. More than that, switch to tabbed per-option panels; a
  horizontally scrolling table inside a 400 px frame is unreadable.
- **Timeline** — a sequence with per-node status. Not for unordered lists.
- **Progress** — always show actual / target and the remaining gap, never a bare percentage.
- **Plan cards** — one item chosen from N, each needing its own detail.

### Comparison table

Two variants. **Static** when the weights are already decided; **weighted** only when the reader
is genuinely choosing and the trade-off depends on what they value (see Interaction policy).

**Static** — ship this one by default:

```html
<div class="card">
  <h3>方案对比</h3>
  <div class="scroll">
    <table>
      <thead><tr><th scope="col">评估项</th><th scope="col" class="num">权重</th>
        <th scope="col" class="num">方案 A</th><th scope="col" class="num">方案 B</th></tr></thead>
      <tbody>
        <tr><th scope="row">性能</th><td class="num w">30%</td><td class="num">4.5</td><td class="num">3.5</td></tr>
        <tr><th scope="row">成本</th><td class="num w">25%</td><td class="num">3.0</td><td class="num">4.5</td></tr>
        <tr><th scope="row">可维护性</th><td class="num w">20%</td><td class="num">4.0</td><td class="num">3.5</td></tr>
        <tr><th scope="row">生态</th><td class="num w">15%</td><td class="num">4.5</td><td class="num">3.0</td></tr>
        <tr><th scope="row">上手成本</th><td class="num w">10%</td><td class="num">3.5</td><td class="num">4.5</td></tr>
      </tbody>
    </table>
  </div>
  <p class="note">加权得分 = Σ(单项分 × 权重)：A = <b>3.925</b>，B = <b>3.775</b>（满分 5.0；
    权重合计 100%）。示例数据</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .card h3 { margin: 0 0 12px; font-size: 15px; font-weight: 600; }
  .scroll { overflow-x: auto; }  /* the narrow-width escape hatch, not a layout */
  table { width: 100%; border-collapse: collapse; font-size: 12.5px; }
  thead th { padding: 6px 4px; font-size: 11px; font-weight: 500; color: var(--hai-text-2);
             border-bottom: 1px solid var(--hai-line); text-align: left; }
  tbody th { padding: 6px 4px; font-weight: 400; text-align: left; }
  tbody td { padding: 6px 4px; border-bottom: 1px solid var(--hai-line); }
  tbody tr:last-child th, tbody tr:last-child td { border-bottom: none; }
  .num { text-align: right; font-variant-numeric: tabular-nums; }
  .w { color: var(--hai-text-2); }
  .note { margin: 12px 0 0; font-size: 11.5px; line-height: 1.65; color: var(--hai-text-2); }
</style>
```

**Weighted** — only when the answer is "it depends on your priorities". The sliders **renormalise**
and re-rank live, so the control genuinely changes the verdict:

```html
<div class="card">
  <h3>按你的优先级打分</h3>
  <ul class="ws">
    <li><label for="w1">性能 <output id="o1" for="w1">30</output></label>
      <input id="w1" type="range" min="0" max="100" step="5" value="30" data-w="0"></li>
    <li><label for="w2">成本 <output id="o2" for="w2">25</output></label>
      <input id="w2" type="range" min="0" max="100" step="5" value="25" data-w="1"></li>
    <li><label for="w3">可维护性 <output id="o3" for="w3">20</output></label>
      <input id="w3" type="range" min="0" max="100" step="5" value="20" data-w="2"></li>
  </ul>
  <ul class="rank" id="rank" aria-live="polite"></ul>
  <p class="note">权重自动归一化（合计不足 100% 时按比例放大）；滑到 0 表示该项不计入。
    单项得分：A = 4.5 / 3.0 / 4.0，B = 3.5 / 4.5 / 3.5（性能 / 成本 / 可维护性）。示例数据</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .card h3 { margin: 0 0 14px; font-size: 15px; font-weight: 600; }
  .ws { list-style: none; margin: 0 0 14px; padding: 0; display: flex;
        flex-direction: column; gap: 9px; }
  .ws label { display: flex; justify-content: space-between; font-size: 12.5px;
              color: var(--hai-text-2); margin-bottom: 4px; }
  .ws output { color: var(--hai-text); font-weight: 600; font-variant-numeric: tabular-nums; }
  .ws input { width: 100%; accent-color: var(--hai-accent); cursor: pointer; }
  .ws input:focus-visible { outline: 2px solid var(--hai-accent); outline-offset: 3px; }
  .rank { list-style: none; margin: 0; padding: 0; display: flex;
          flex-direction: column; gap: 7px; }
  .rank li { display: grid; grid-template-columns: 22px 54px 1fr 44px; gap: 9px;
             align-items: center; font-size: 12px; }
  .rank .pos { color: var(--hai-text-2); font-variant-numeric: tabular-nums; }
  .rank .bar { height: 8px; background: var(--hai-bg); border: 1px solid var(--hai-line);
               border-radius: 999px; overflow: hidden; }
  .rank .bar i { display: block; height: 100%; background: var(--hai-accent);
                 border-radius: 999px; }
  .rank li:not(:first-child) .bar i { opacity: .45; }
  .rank .sc { text-align: right; font-variant-numeric: tabular-nums; font-weight: 600; }
  .rank li:not(:first-child) .sc { font-weight: 400; color: var(--hai-text-2); }
  .note { margin: 14px 0 0; font-size: 11.5px; line-height: 1.65; color: var(--hai-text-2); }
</style>
<script>
(function () {
  var NAMES = ['方案 A', '方案 B'];
  var SCORES = [[4.5, 3.0, 4.0], [3.5, 4.5, 3.5]];
  var sliders = Array.prototype.slice.call(document.querySelectorAll('.ws input'));
  var rank = document.getElementById('rank');
  if (!rank || sliders.length === 0) return;

  function render() {
    var w = sliders.map(function (s) { return s.valueAsNumber || 0; });
    var sum = w.reduce(function (a, b) { return a + b; }, 0);
    sliders.forEach(function (s, i) {
      var out = document.getElementById('o' + (i + 1));
      if (out) out.textContent = String(w[i]);
    });

    var rows = SCORES.map(function (sc, k) {
      // 权重合计为 0：全部并列 0 分（不留 NaN）
      var score = sum === 0 ? 0 : sc.reduce(function (a, v, i) { return a + v * w[i]; }, 0) / sum;
      return { name: NAMES[k], score: score };
    }).sort(function (a, b) { return b.score - a.score; });

    var max = 5;  // 单项满分 5 → 加权后满分 5
    rank.textContent = '';
    rows.forEach(function (r, i) {
      var li = document.createElement('li');
      var pct = max > 0 ? (r.score / max) * 100 : 0;
      li.innerHTML = '<span class="pos">' + (i + 1) + '</span>' +
        '<span>' + r.name + '</span>' +
        '<span class="bar"><i style="width:' + pct.toFixed(1) + '%"></i></span>' +
        '<span class="sc">' + r.score.toFixed(2) + '</span>';
      rank.appendChild(li);
    });
    if (sum === 0) {
      var li = document.createElement('li');
      li.innerHTML = '<span class="pos">—</span><span>请至少给一项权重</span>' +
        '<span class="bar"></span><span class="sc">—</span>';
      rank.textContent = '';
      rank.appendChild(li);
    }
  }

  sliders.forEach(function (s) { s.addEventListener('input', render); });
  render();
})();
</script>
```

With **more than four columns**, switch to tabbed per-option panels — a horizontally scrolling table
inside a 400 px frame is unreadable.

### Timeline

```html
<div class="card">
  <h3>里程碑</h3>
  <ol class="tl">
    <li class="done"><div class="when">2026-08</div><div class="what">规格提取完成</div>
      <div class="desc">接口清单与字段类型冻结</div></li>
    <li class="now"><div class="when">2026-10 · 进行中</div><div class="what">领域层实现</div>
      <div class="desc">8 / 12 个端点转绿</div></li>
    <li class="todo"><div class="when">2026-12</div><div class="what">下线旧服务</div>
      <div class="desc">保留回滚开关两周</div></li>
  </ol>
  <p class="note">示例排期</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .card h3 { margin: 0 0 16px; font-size: 15px; font-weight: 600; }
  .tl { list-style: none; margin: 0; padding: 0 0 0 22px; position: relative; }
  .tl::before { content: ""; position: absolute; left: 5px; top: 6px; bottom: 8px;
                width: 2px; background: var(--hai-line); border-radius: 2px; }
  .tl li { position: relative; padding-bottom: 16px; }
  .tl li:last-child { padding-bottom: 0; }
  .tl li::before { content: ""; position: absolute; left: -22px; top: 5px;
                   width: 12px; height: 12px; border-radius: 50%; box-sizing: border-box;
                   background: var(--hai-surface); border: 2px solid var(--hai-line); }
  .tl li.done::before { background: var(--hai-accent); border-color: var(--hai-accent); }
  .tl li.now::before  { border-color: var(--hai-accent);
                        box-shadow: 0 0 0 4px color-mix(in srgb, var(--hai-accent) 22%, transparent); }
  .when { font-size: 11px; color: var(--hai-text-2); font-variant-numeric: tabular-nums; }
  .what { margin-top: 2px; font-size: 13px; font-weight: 600; }
  .tl li.todo .what { color: var(--hai-text-2); font-weight: 500; }
  .desc { margin-top: 3px; font-size: 12px; color: var(--hai-text-2); }
  .note { margin: 14px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
```

### Progress against a target

```html
<div class="card">
  <h3>季度目标进度</h3>
  <ul class="prog">
    <li><div class="ph"><span>新签合同额</span><span class="pv">¥ 820 万 / 1,200 万</span></div>
      <div class="track"><i style="width:68%"></i><b class="target" style="left:100%"></b></div>
      <div class="pm">达成 68% · 距目标 ¥380 万</div></li>
    <li><div class="ph"><span>交付项目数</span><span class="pv">14 / 18</span></div>
      <div class="track"><i style="width:78%"></i><b class="target" style="left:100%"></b></div>
      <div class="pm">达成 78% · 差 4 个</div></li>
  </ul>
  <p class="note">示例数据；百分比 = 实际 / 目标，目标线在 100% 处</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .card h3 { margin: 0 0 14px; font-size: 15px; font-weight: 600; }
  .prog { list-style: none; margin: 0; padding: 0; display: flex;
          flex-direction: column; gap: 14px; }
  .ph { display: flex; justify-content: space-between; gap: 10px; font-size: 12.5px;
        margin-bottom: 5px; }
  .pv { color: var(--hai-text-2); font-variant-numeric: tabular-nums; }
  .track { position: relative; height: 9px; background: var(--hai-bg);
           border: 1px solid var(--hai-line); border-radius: 999px; overflow: visible; }
  .track i { display: block; height: 100%; background: var(--hai-accent); opacity: .75;
             border-radius: 999px; }
  .target { position: absolute; top: -3px; width: 2px; height: 15px;
            background: var(--hai-text-2); border-radius: 1px; }
  .pm { margin-top: 4px; font-size: 11px; color: var(--hai-text-2);
        font-variant-numeric: tabular-nums; }
  .note { margin: 12px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
```

A bare percentage hides the target. Always show **actual / target** and the remaining gap.

### Plan / pricing cards

```html
<div class="card">
  <h3>套餐选择</h3>
  <div class="plans">
    <div class="plan"><div class="pn">基础</div>
      <div class="pp">¥49<span>/月</span></div>
      <ul><li>10 万次调用</li><li>7 天日志</li><li>邮件支持</li></ul></div>
    <div class="plan rec"><div class="pn">标准 <em>推荐</em></div>
      <div class="pp">¥199<span>/月</span></div>
      <ul><li>100 万次调用</li><li>30 天日志</li><li>工单 + 群支持</li></ul></div>
  </div>
  <p class="note">示例价格；超出配额按 ¥0.8 / 万次计费</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .card h3 { margin: 0 0 14px; font-size: 15px; font-weight: 600; }
  .plans { display: grid; gap: 10px; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); }
  .plan { background: var(--hai-bg); border: 1px solid var(--hai-line);
          border-radius: 8px; padding: 12px 14px; min-width: 0; }
  .plan.rec { border-color: var(--hai-accent); }
  .pn { font-size: 12px; color: var(--hai-text-2); }
  .pn em { margin-left: 6px; padding: 1px 6px; font-style: normal; font-size: 10px;
           border-radius: 999px; background: var(--hai-accent); color: var(--hai-surface); }
  .pp { margin: 4px 0 10px; font-size: 23px; font-weight: 650;
        font-variant-numeric: tabular-nums; }
  .pp span { font-size: 11px; font-weight: 400; color: var(--hai-text-2); }
  .plan ul { margin: 0; padding-left: 16px; font-size: 12px; line-height: 1.8;
             color: var(--hai-text-2); }
  .note { margin: 12px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
```
