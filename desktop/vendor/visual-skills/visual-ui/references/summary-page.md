<!--
visual-ui reference · Full-answer page (summary / review / retrospective)
Read this file only when the whole answer should be a page rather than a chart inside prose.
Every snippet is self-contained: copy it, then replace the content.
Keep the <style> and the section order; they encode the frame's constraints.
-->

# Full-answer page

The page **is** the answer: masthead → verdict → 2–5 sections → footnote. The reader should get the
point from the first screen and be able to scan the rest, not read it linearly.

## System summary page

```html
<div class="card page">
  <div class="head">
    <h1>orderd 架构总结</h1>
    <span class="stamp">8 个文件 · 161 行 Go</span>
  </div>

  <p class="verdict">单进程订单服务：网关 → 订单领域 → SQLite，支付同步外调。
    没有消息队列，状态机在内存里而不是数据库约束。</p>

  <ul class="kpis">
    <li><b>4</b><span>Go 包</span></li>
    <li><b>3</b><span>张表</span></li>
    <li><b>6</b><span>订单状态</span></li>
    <li><b>0</b><span>异步链路</span></li>
  </ul>

  <h2>组成</h2>
  <div class="layers">
    <div class="band"><span class="bl">接入层</span>
      <span class="chip">gateway</span><span class="chip">auth</span><span class="chip">rateLimit</span></div>
    <div class="band"><span class="bl">领域层</span>
      <span class="chip">order.Service</span><span class="chip">Transition</span></div>
    <div class="band"><span class="bl">数据层</span>
      <span class="chip">order.Store</span><span class="chip">user.Store</span><span class="chip">SQLite</span></div>
  </div>

  <h2>一次支付的顺序</h2>
  <ol class="steps">
    <li><b>网关</b> 校验 API key 与限流后转发 <code>server.go:20</code></li>
    <li><b>订单服务</b> 调 <code>billing.Capture</code>（同步，10s 超时）</li>
    <li><b>订单服务</b> 写 <code>orders.status = paid</code></li>
  </ol>

  <h2>数据模型</h2>
  <p><code>users 1—N orders 1—N order_items</code>；<code>orders.user_id</code> 是外键。
    但进程没有开 <code>PRAGMA foreign_keys</code>，这条约束实际不生效。</p>

  <h2>风险</h2>
  <table>
    <thead><tr><th scope="col">风险</th><th scope="col">证据与影响</th></tr></thead>
    <tbody>
      <tr><td>外键未启用</td><td>写库前无 pragma，脏 <code>user_id</code> 可落库</td></tr>
      <tr><td>状态机在内存</td><td><code>allowed</code> map 只拦 API，绕过 API 的写不受约束</td></tr>
      <tr><td>支付是空实现</td><td><code>Capture</code> 返回 nil，本地永远成功</td></tr>
    </tbody>
  </table>

  <p class="foot">范围：仅这 8 个文件（2026-10 快照）；未看支付商内部实现；
    KPI 为例示口径，非生产数据。</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif;
          line-height: 1.6; min-width: 0; }
  .head { display: flex; flex-wrap: wrap; align-items: baseline;
          justify-content: space-between; gap: 4px 10px; margin-bottom: 10px; }
  .head h1 { margin: 0; font-size: 18px; font-weight: 650; line-height: 1.35; }
  .stamp { font-size: 11px; color: var(--hai-text-2); font-variant-numeric: tabular-nums; }
  .verdict { margin: 0 0 14px; padding: 2px 0 2px 12px; font-size: 13px;
             border-left: 3px solid var(--hai-accent); }
  .kpis { display: grid; grid-template-columns: repeat(4, 1fr);
          gap: 8px; list-style: none; margin: 0; padding: 0; }
  @media (max-width: 420px) { .kpis { grid-template-columns: repeat(2, 1fr); } }
  .kpis li { padding: 8px 10px; border: 1px solid var(--hai-line);
             border-radius: 8px; background: var(--hai-bg); min-width: 0; }
  .kpis b { display: block; font-size: 20px; font-weight: 650;
            font-variant-numeric: tabular-nums; line-height: 1.3; }
  .kpis span { font-size: 10.5px; color: var(--hai-text-2); }
  h2 { margin: 20px 0 8px; font-size: 13px; font-weight: 600; }
  .verdict + .kpis + h2 { margin-top: 18px; }
  .layers { display: flex; flex-direction: column; gap: 6px; }
  .band { display: flex; flex-wrap: wrap; align-items: center; gap: 5px;
          padding: 7px 9px; border: 1px solid var(--hai-line);
          border-radius: 7px; background: var(--hai-bg); }
  .bl { flex: 0 0 100%; font-size: 10.5px; color: var(--hai-text-2); }
  .chip { font-size: 11.5px; padding: 1px 7px; border-radius: 999px;
          border: 1px solid var(--hai-line); background: var(--hai-surface); }
  .steps { margin: 0; padding-left: 18px; font-size: 12.5px; }
  .steps li { margin: 3px 0; }
  code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
         font-size: 11px; background: var(--hai-bg); border-radius: 3px;
         padding: 0 3px; overflow-wrap: anywhere; }
  table { width: 100%; border-collapse: collapse; table-layout: fixed; font-size: 11.5px; }
  td:first-child, th:first-child { width: 38%; }
  th { padding: 5px 4px; text-align: left; font-size: 10.5px; font-weight: 500;
       color: var(--hai-text-2); border-bottom: 1px solid var(--hai-line); }
  td { padding: 5px 4px; border-bottom: 1px solid var(--hai-line);
       vertical-align: top; overflow-wrap: anywhere; }
  tr:last-child td { border-bottom: none; }
  .foot { margin: 20px 0 0; padding-top: 10px; font-size: 11px;
          color: var(--hai-text-2); border-top: 1px solid var(--hai-line); }
</style>
```

## The page blocks

Keep to this vocabulary; each block already has styles in the skeleton above.

| Block | Use for | Budget |
|---|---|---|
| Masthead (`h1` + stamp) | what this page is, and the snapshot it describes | one line each |
| Verdict (`.verdict`) | the conclusion a reader could stop after | 1–3 sentences, accent rule |
| KPI row (`.kpis`) | 3–4 numbers that frame the answer | one unit per number |
| Layer bands (`.layers`) | composition, top-down | 3–5 bands, ≤ 4 chips each |
| Numbered steps (`.steps`) | order of calls / events | 3–6 steps, one line each |
| Short table | risks, options, before/after | ≤ 4 rows, 2–3 columns |
| Footnote (`.foot`) | scope, omissions, sample-data label | 1–3 lines |

For a decision page, the verdict states the recommendation and the table becomes "选项 / 代价"; for a
retrospective, the steps become the timeline and the table becomes "发生了什么 / 学到什么". The
sequence does not change.

## Rules

- **The first screen must stand alone.** Title, verdict, and the strongest evidence above ~560 px.
  Everything after it is support, not the point.
- **One idea per section.** A heading followed by three paragraphs of prose means the section should
  have been Markdown; each section should own one artifact from the table above.
- **Do not restate the page outside the block.** One lead-in sentence, then the page; after it at
  most a next-step line.
- **The footnote is not optional.** Which files/versions were examined, what was left out, whether
  the numbers are 示例. This is the cheapest honesty in the whole design.
- **No navigation.** No table of contents, jump links or "back to top" — the frame scrolls and those
  controls only pretend to do something.
- **A short answer stays prose.** If the whole thing fits in three sentences, a page is a costume.
