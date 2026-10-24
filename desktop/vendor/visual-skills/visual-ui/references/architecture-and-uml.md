<!--
visual-ui reference · Architecture, sequence, ER and state diagrams
Read this file only when the answer needs one of these shapes.
Every snippet is self-contained: copy it, then replace the content.
Keep the <style> and the markup structure; they encode the frame's constraints.
-->

# Architecture, sequence, ER and state diagrams

These four answer "explain the shape of this thing". They share two constraints that decide the
whole layout:

- **The frame is 300–560 px wide and ~560 px tall.** There is no room for a left-to-right sprawl or
  for a graph that needs a pan-and-zoom tool. If the real diagram has 20 nodes, the honest answer
  shows the 6 that carry the explanation and says what was left out.
- **A diagram is a claim about topology.** Only draw an edge you can support from the code, the
  config or the user's own description. When you infer one, mark it in the caption ("虚线为推测").
  The arrow is as falsifiable as a number.

That is also why none of these four are SVG: an `<svg viewBox="0 0 360 …">` at a 300 px frame
scales 12 px type down to ~8 px, and a 640-wide one to 5 px. DOM markup keeps type at its real size
at every width. The only SVG left in these skeletons is a 16×22 arrow glyph — no text to shrink.

## Layered architecture

Use when the question is **composition** — what the system is made of and which layer may call
which. HTML boxes, not SVG: labels wrap, chips reflow, and there is no coordinate arithmetic to get
wrong. Top = closest to the user, bottom = storage.

```html
<div class="card">
  <h3>系统分层</h3>
  <div class="arch" role="img"
       aria-label="分层架构，自上而下：接入层为桌面端与 CLI；网关层为鉴权、路由、限流；领域层为会话、计费、子智能体；存储层为 SQLite 与文件。箭头表示同步调用。日志、指标、追踪为横切能力。">
    <div class="layer">
      <span class="tag">接入层</span>
      <span class="node">桌面端</span><span class="node">CLI</span>
    </div>
    <svg class="down" viewBox="0 0 16 22" aria-hidden="true">
      <path d="M8 1v14M3.5 10.5 8 15l4.5-4.5"/>
    </svg>
    <div class="layer">
      <span class="tag">网关层</span>
      <span class="node">鉴权</span><span class="node">路由</span><span class="node">限流</span>
    </div>
    <svg class="down" viewBox="0 0 16 22" aria-hidden="true">
      <path d="M8 1v14M3.5 10.5 8 15l4.5-4.5"/>
    </svg>
    <div class="layer core">
      <span class="tag">领域层 · 核心</span>
      <span class="node">会话</span><span class="node">计费</span><span class="node">子智能体</span>
    </div>
    <svg class="down" viewBox="0 0 16 22" aria-hidden="true">
      <path d="M8 1v14M3.5 10.5 8 15l4.5-4.5"/>
    </svg>
    <div class="layer">
      <span class="tag">存储层</span>
      <span class="node">SQLite</span><span class="node">文件</span>
    </div>
    <div class="layer xcut">
      <span class="tag">横切</span>
      <span class="node">日志</span><span class="node">指标</span><span class="node">追踪</span>
    </div>
  </div>
  <p class="note">箭头 = 同步调用；消息队列与外部依赖未画出 · 示例</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; line-height: 1.6; }
  .card h3 { margin: 0 0 12px; font-size: 15px; font-weight: 600; }
  .arch { display: flex; flex-direction: column; }
  .layer { display: flex; flex-wrap: wrap; align-items: center; gap: 6px;
           border: 1px solid var(--hai-line); border-radius: 8px;
           background: var(--hai-bg); padding: 8px 10px; }
  .tag { flex: 0 0 100%; margin-bottom: 2px; font-size: 11px; color: var(--hai-text-2); }
  .node { font-size: 12px; padding: 2px 8px; border-radius: 999px;
          background: var(--hai-surface); border: 1px solid var(--hai-line); }
  .layer.core { border-color: var(--hai-accent); }
  .layer.core .node { border-color: var(--hai-accent); }
  .layer.xcut { border-style: dashed; margin-top: 12px; }
  .down { align-self: center; width: 14px; height: 20px; margin: 2px 0;
          fill: none; stroke: var(--hai-text-2); stroke-width: 1.4; }
  .note { margin: 10px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
```

Rules: **4–6 layers is the ceiling** and each layer holds at most 4 chips — this is a summary, not an
inventory. If one layer is the point of the answer, accent it (`core`) and leave the rest quiet.
Keep the layer names to what the codebase actually calls them; inventing "BFF 层" because it reads
well is the same failure as inventing data.

## Sequence

Use when the question is **order** — who calls whom, in what sequence, and what comes back. Three
actors maximum; four is already unreadable at 300 px. Built from DOM rows rather than SVG for the
reason above: the labels must survive at 11 px.

```html
<div class="card">
  <h3>下单请求的调用顺序</h3>
  <div class="seq" role="img"
       aria-label="时序图，自上而下四条消息：一，客户端向网关提交订单；二，网关校验后转发给订单服务；三，订单服务向网关返回订单号；四，网关向客户端返回 201。">
    <div class="heads"><span>客户端</span><span>网关</span><span>订单服务</span></div>
    <div class="track">
      <i class="life" style="--x:16.7%"></i>
      <i class="life" style="--x:50%"></i>
      <i class="life" style="--x:83.3%"></i>

      <!-- one row per message, in time order; --a / --b are the lane centres above -->
      <div class="m" style="--a:16.7%;--b:50%"><span class="lab">1 提交订单</span><i class="ar"></i></div>
      <div class="m" style="--a:50%;--b:83.3%"><span class="lab">2 校验后转发</span><i class="ar"></i></div>
      <div class="m back" style="--a:83.3%;--b:50%"><span class="lab">3 订单号</span><i class="ar"></i></div>
      <div class="m back" style="--a:50%;--b:16.7%"><span class="lab">4 201 Created</span><i class="ar"></i></div>
    </div>
  </div>
  <p class="note">实线 = 请求，虚线 = 返回；省略了重试与异步回调 · 示例</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; line-height: 1.6; }
  .card h3 { margin: 0 0 12px; font-size: 15px; font-weight: 600; }
  .heads { display: grid; grid-template-columns: repeat(3, 1fr); }
  .heads span { margin: 0 2px; padding: 4px 2px; text-align: center; font-size: 11.5px;
                border: 1px solid var(--hai-line); border-radius: 6px; background: var(--hai-bg); }
  .track { position: relative; padding-top: 12px; }
  .life { position: absolute; top: 0; bottom: 0; left: var(--x); width: 0;
          border-left: 1px dashed var(--hai-line); }
  .m { position: relative; height: 34px; }
  .ar { position: absolute; top: 20px;
        left: min(var(--a), var(--b));
        width: calc(max(var(--a), var(--b)) - min(var(--a), var(--b)));
        border-top: 1.3px solid var(--hai-text-2); }
  .ar::after { content: ""; position: absolute; top: -5px; right: -1px;
               border: 4px solid transparent; border-left-color: var(--hai-text-2); }
  .m.back .ar { border-top-style: dashed; }
  .m.back .ar::after { right: auto; left: -1px;
                       border-left-color: transparent; border-right-color: var(--hai-text-2); }
  .lab { position: absolute; top: 0; white-space: nowrap;
         left: calc(min(var(--a), var(--b)) + (max(var(--a), var(--b)) - min(var(--a), var(--b))) / 2);
         transform: translateX(-50%); padding: 0 4px; font-size: 11px;
         background: var(--hai-surface); color: var(--hai-text); }
  .note { margin: 10px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
```

Rules: for two actors, use `repeat(2, 1fr)` and `--x` / `--a` / `--b` at `25%` and `75%`. Keep labels
≤ 8 characters — a longer one collides with the neighbouring lifeline; move the detail into the
caption. Messages are spaced evenly by *time order*, not by duration: if the reader needs timing, say
it in the caption instead of faking a scale. No self-calls (they need a loop that eats 40 px of a
narrow frame) — write "订单服务内部写库" in the caption instead.

## ER (entity relationships)

Two entities fit side by side with the relation in the middle. The card is **not** a table: the
middle column is the edge, and the cardinality sits on it.

```html
<div class="card">
  <h3>订单与用户</h3>
  <div class="er" role="img"
       aria-label="实体关系图：用户与订单为一对多；订单通过 user_id 外键指向用户主键 id。">
    <div class="ent">
      <div class="ename">用户</div>
      <ul><li><b>id</b><span>PK</span></li><li>email</li><li>created_at</li></ul>
    </div>
    <div class="rel" aria-hidden="true"><span>1</span><i></i><span>N</span></div>
    <div class="ent">
      <div class="ename">订单</div>
      <ul><li><b>id</b><span>PK</span></li><li><b>user_id</b><span>FK</span></li><li>amount</li></ul>
    </div>
  </div>
  <p class="note">一个用户可有多张订单；金额单位为分 · 示例</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; line-height: 1.6; }
  .card h3 { margin: 0 0 12px; font-size: 15px; font-weight: 600; }
  .er { display: grid; grid-template-columns: 1fr 32px 1fr; align-items: center; }
  .ent { min-width: 0; border: 1px solid var(--hai-line); border-radius: 8px;
         background: var(--hai-bg); overflow: hidden; }
  .ename { padding: 6px 9px; font-size: 12px; font-weight: 600;
           background: var(--hai-surface); border-bottom: 1px solid var(--hai-line); }
  .ent ul { list-style: none; margin: 0; padding: 5px 9px 7px; font-size: 11.5px; }
  .ent li { display: flex; justify-content: space-between; gap: 6px; line-height: 1.7;
            overflow-wrap: anywhere; }
  .ent b { font-weight: 500; }
  .ent li span { color: var(--hai-text-2); font-size: 10.5px; }
  .rel { display: flex; flex-direction: column; align-items: center; gap: 3px;
         color: var(--hai-text-2); font-size: 10.5px; }
  .rel i { width: 100%; height: 1px; background: var(--hai-text-2); }
  .note { margin: 10px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
```

`PK` / `FK` marks are what make this an ER diagram rather than two tables; the middle `1 — N` is the
relation. Keep each field row to **name + key badge**: adding a type column (`uuid`, `timestamptz`)
halves the width and long types then wrap mid-word at 300 px. If types are the point, give them their
own line or use the edge-list variant below.

Three or more entities do not fit: keep the two that carry the answer and list the rest as edges,
which is readable at any width and still states the cardinality:

```html
<div class="card">
  <h3>关系</h3>
  <ul class="edges">
    <li><b>订单</b> <span>N:1</span> <b>用户</b> <em>user_id → id</em></li>
    <li><b>订单项</b> <span>N:1</span> <b>订单</b> <em>order_id → id</em></li>
    <li><b>订单项</b> <span>N:1</span> <b>商品</b> <em>sku_id → id</em></li>
  </ul>
  <p class="note">三张关键表的关系；支付流水等从表略 · 示例</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; line-height: 1.6; }
  .card h3 { margin: 0 0 10px; font-size: 15px; font-weight: 600; }
  .edges { list-style: none; margin: 0; padding: 0; font-size: 12px; }
  .edges li { display: flex; flex-wrap: wrap; align-items: baseline; gap: 6px;
              padding: 7px 0; border-bottom: 1px solid var(--hai-line); }
  .edges li:last-child { border-bottom: none; }
  .edges span { color: var(--hai-text-2); font-size: 10.5px;
                border: 1px solid var(--hai-line); border-radius: 4px; padding: 0 4px; }
  .edges em { flex: 0 0 100%; font-style: normal; font-size: 11px; color: var(--hai-text-2); }
  .note { margin: 10px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
```

## State machine

Use when **one thing** has branches in its lifecycle. The spine is the path that actually happens
most of the time; every branch is written as an explicit labelled edge underneath, because a
branching spine in SVG needs diagonal edges that are unreadable at this width.

```html
<div class="card">
  <h3>订单状态</h3>
  <div class="fsm" role="img"
       aria-label="状态机：待支付在支付成功后进入已支付，已支付发货后进入已发货，已发货签收后进入已完成。分支：待支付超时 30 分钟进入已取消，已支付可退款进入退款中。">
    <div class="st"><b>待支付</b><span>起点</span></div>
    <div class="step"><i></i><span>支付成功</span></div>
    <div class="st"><b>已支付</b></div>
    <div class="step"><i></i><span>发货</span></div>
    <div class="st"><b>已发货</b></div>
    <div class="step"><i></i><span>签收</span></div>
    <div class="st done"><b>已完成</b><span>终态</span></div>
    <ul class="alt">
      <li><span>待支付</span><em>超时 30 分钟</em><span>已取消</span></li>
      <li><span>已支付</span><em>退款</em><span>退款中</span></li>
    </ul>
  </div>
  <p class="note">实线为常规路径，下方为分支；关闭订单与对账任务未画出 · 示例</p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; line-height: 1.6; }
  .card h3 { margin: 0 0 12px; font-size: 15px; font-weight: 600; }
  .fsm { display: flex; flex-direction: column; align-items: center; }
  .st { display: flex; align-items: baseline; gap: 6px; width: 100%; max-width: 220px;
        justify-content: center; padding: 5px 10px; font-size: 12px; box-sizing: border-box;
        border: 1px solid var(--hai-line); border-radius: 7px; background: var(--hai-bg); }
  .st span { font-size: 10.5px; color: var(--hai-text-2); }
  .st.done { border-color: var(--hai-accent); }
  /* transition: a 1px rail with the event label beside it, never text on the line */
  .step { position: relative; display: flex; align-items: center; box-sizing: border-box;
          width: 100%; height: 30px; padding-left: calc(50% + 10px); }
  .step i { position: absolute; left: 50%; top: 4px; bottom: 4px;
            width: 1px; background: var(--hai-text-2); }
  .step i::after { content: ""; position: absolute; left: -3px; bottom: 0;
                   border: 3.5px solid transparent; border-top-color: var(--hai-text-2); }
  .step span { position: relative; font-size: 11px; color: var(--hai-text-2); }
  .alt { list-style: none; width: 100%; margin: 14px 0 0; padding: 10px 0 0;
         border-top: 1px dashed var(--hai-line); font-size: 11.5px; }
  .alt li { display: flex; flex-wrap: wrap; align-items: baseline; gap: 6px; padding: 3px 0; }
  .alt em { font-style: normal; font-size: 10.5px; color: var(--hai-text-2);
            padding: 0 5px; border-left: 1px solid var(--hai-line);
            border-right: 1px solid var(--hai-line); }
  .note { margin: 10px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
```

The `.step` rail is drawn from `50%` and the label sits to its right — this is the one place where a
line runs *beside* text rather than under it. A `class` diagram is the ER entity card with a third
section for methods and `extends` / `implements` written as edges in the relation list; do not build
a separate shape for it.

## What these are not

- **Not a graph.** No pan, zoom, minimap or node dragging. If the topology does not fit, that is a
  signal to cut it down, not to add controls.
- **Not a dump.** The caption states what was omitted; the prose above the block does the explaining.
  A diagram that needs a paragraph per box is a list wearing a diagram's clothes.
- **Not decoration for a one-line answer.** "这个系统很复杂" does not need a diagram; a specific
  question about composition, order, relations or states does.
