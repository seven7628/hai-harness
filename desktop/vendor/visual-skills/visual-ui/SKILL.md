---
name: visual-ui
version: 1.5.0
description: Build charts, dashboards, comparison cards, timelines, tabbed panels, pricing tables, heatmaps, trees and small interactive widgets as a single self-contained HTML block that the app renders in a live sandboxed iframe. Use this when the user asks for a chart, a visual comparison, a dashboard, a calculator, a tabbed view, a timeline, a diagram or any answer where layout and styling carry meaning that plain Markdown cannot express.
---

# Visual answers as sandboxed HTML

## How to run this

- Emit **one** fenced ```` ```html ```` block in your reply. The app renders it in a sandboxed iframe
  immediately and also offers "open in browser" (right panel) for a full-size view. Do not write the
  HTML into a file for the user to open unless they asked for a file — the preview is the deliverable.
- The block must be **self-contained**. Ship its own `<style>`: the iframe inherits none of the app's
  CSS and sits on a light canvas. Unstyled output renders as bare browser defaults.
- Prefer the theme variables below over hardcoded colors. They are injected for you and follow the
  app's current light/dark theme, so a widget written with them looks native in both.
- Keep the block focused on the visual. Put the explanation in normal Markdown around it.
- Verify before you ship: no `NaN`, no empty denominators, no control that does nothing, and every
  number carries its unit.
- **Pick a shape from the catalogue below before writing markup.** Defaulting to "card + a few
  paragraphs" for a comparison, a sequence or a set of alternatives is the most common way to
  produce something that is technically valid HTML and still worse than plain Markdown.

## Where the HTML actually runs

The app injects the block into `<iframe sandbox="allow-scripts" srcdoc="…">`. Consequences that
change what you may write:

| Constraint | What it means for you |
|---|---|
| `allow-scripts` **without** `allow-same-origin` | The frame has an opaque origin. `localStorage`, `sessionStorage`, `document.cookie`, `IndexedDB` and same-origin `fetch` all throw. |
| No access to the parent page | `window.parent.document` throws. You cannot read app state or call app functions. |
| Theme variables are injected | `--hai-bg`, `--hai-surface`, `--hai-text`, `--hai-text-2`, `--hai-line`, `--hai-accent` are defined on `:root`. Use them. |
| Your `<style>` runs | Styles you write win over the injected canvas (it only styles `body` and bare tables). |
| Responsive height | The frame measures your content and grows up to **560 px**, then scrolls inside. A tall page is readable but not fully visible at once — put the most important part first. |
| No bundler, no import map | There is no `three` / `d3` / `chart.js` to `import`. Write charts as inline `<svg>` or with plain DOM. |
| Local files are not reachable | Relative URLs (`./chart.png`) do **not** resolve in an opaque-origin frame. Inline small images as `data:` URLs, or use absolute `https:` URLs. |
| Network is not guaranteed | Load nothing from a CDN. Assume offline. Inline everything. |
| `prefers-reduced-motion` | The canvas honors it; respect it in your own animations too. |

## Theme tokens

```css
:root {
  --hai-bg:      /* page background   */
  --hai-surface: /* card background   */
  --hai-text:    /* primary text      */
  --hai-text-2:  /* secondary text    */
  --hai-line:    /* borders / rules   */
  --hai-accent:  /* one accent color  */
}
```

There is deliberately **no** success/warning/danger palette token. If a colormap needs more than the
accent, define your own hues in your `<style>` — but keep the body on the tokens so it still follows
the theme. Never hardcode a background or text color: it will be unreadable in one of the two themes.

**How to get more than one color without breaking the theme.** Derive everything from the accent and
the text tokens instead of inventing hexes:

```css
/* a scale in one accent: opacity on a token color */
.s1 { fill: var(--hai-accent); fill-opacity: 1; }
.s2 { fill: var(--hai-accent); fill-opacity: .65; }
.s3 { fill: var(--hai-accent); fill-opacity: .35; }

/* a tinted background that works in both themes */
.tint { background: color-mix(in srgb, var(--hai-accent) 12%, var(--hai-bg)); }

/* status colors: pair the hue with a text label or icon, never hue alone */
.ok   { color: var(--hai-text);   border-color: var(--hai-accent); }
.warn { color: var(--hai-text-2); border-color: var(--hai-text-2); }
```

If you need a genuine multi-hue categorical palette (say 6 series), declare six fixed hues and keep
the *chrome* — background, text, borders — on tokens. Check them against both a light and a dark
surface: a palette tuned on white usually fails on near-black.

## Layout system

The frame is narrow-to-medium typically (300–560 px visible width). Build for that first.

### The container

```html
<div class="card"><!-- one visual, one card --></div>
```

```css
.card {
  background: var(--hai-surface);
  color: var(--hai-text);
  border: 1px solid var(--hai-line);
  border-radius: 10px;
  padding: 16px 18px;
  font-family: system-ui, -apple-system, "PingFang SC", sans-serif;
  line-height: 1.6;
  /* keep the card from collapsing when it sits in a flex/grid row */
  min-width: 0;
}
```

- **One card per block.** If the answer genuinely has two unrelated visuals, use two stacked cards, not
  one card with two unrelated things inside.
- Do not set a fixed `width` on the card. It fills the frame.
- `padding: 16px 18px` and `border-radius: 10px` are the house defaults; stay near them so blocks
  generated at different times look like they belong to the same app.

### Grids that actually collapse

```css
/* auto-fit + minmax: 4 across when wide, 1 per row when narrow, no media query needed */
.grid { display: grid; gap: 10px;
        grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); }

/* a fixed column count that must collapse: override at the narrow breakpoint */
.kpis { display: grid; gap: 10px; grid-template-columns: repeat(4, 1fr); }
@media (max-width: 520px) { .kpis { grid-template-columns: repeat(2, 1fr); } }
```

Rules that prevent the usual breakage:

- `min-width: 0` on any flex/grid child that contains long text or a table, or it will refuse to
  shrink and push the card into horizontal scroll.
- Prefer `auto-fit`/`minmax` over a pile of media queries.
- Never set a fixed pixel width on a text container. `max-width` is fine and encouraged for prose.
- Long strings (IDs, URLs, stack traces) need `overflow-wrap: anywhere` or they overflow the card.

### Vertical rhythm

Use one spacing scale and stick to it: `4 / 8 / 12 / 16 / 24`. Section gap `16`, block gap `12`,
label-to-value `3–4`. If everything is `10px` the visual has no hierarchy.

### The header pattern

```html
<div class="head">
  <h3>标题</h3>
  <span class="stamp">示例数据 · 2026-10-19</span>
</div>
```

```css
.head { display: flex; align-items: baseline; justify-content: space-between;
        gap: 10px; margin-bottom: 12px; }
.card h3 { margin: 0; font-size: 15px; font-weight: 600; }
.stamp { font-size: 11px; color: var(--hai-text-2); font-variant-numeric: tabular-nums; }
```

The stamp is where the "示例数据" honesty label lives — visible, in the widget, not only in prose.

### Typography scale

| Role | Size | Weight | Notes |
|---|---|---|---|
| Card title (`h3`) | 15 px | 600 | one per card |
| Section label | 11–12 px | 500 | `--hai-text-2`, often uppercase-ish |
| Body / table cell | 12.5–13 px | 400 | |
| The one big number | 21–26 px | 650 | `font-variant-numeric: tabular-nums` |
| Caption / note | 11–11.5 px | 400 | `--hai-text-2` |

- **Every number gets `font-variant-numeric: tabular-nums`.** Proportional digits make columns of
  figures look ragged.
- Minimum readable size is **11 px**; do not go smaller to fit more in. Cut content instead.
- Body text on `--hai-surface` must hit 4.5:1 contrast. `--hai-text-2` is for secondary labels, not
  for the answer itself.

## Shape catalogue

Pick one. Each entry is the *shape*, not a fixed design — adapt content.

| The answer is… | Use | Avoid |
|---|---|---|
| one number with context | **KPI card** (one big number, unit, period, delta) | paragraph of numbers |
| a few numbers to scan | **KPI strip** (2–4 cards, same unit family) | a table of two rows |
| a trend over time | **line / area chart** `<svg>` | bar chart for continuous time |
| magnitudes across categories | **horizontal bar** (sorted, from zero) | pie chart with 8 slices |
| part-to-whole, ≤ 4 parts | **stacked bar** or **donut** with a legend and values | pie with no labels |
| distribution across 2 dimensions | **heatmap** (grid of cells + scale) | 3D anything |
| options against criteria | **comparison table**, or **weighted score** when deciding | prose "on one hand" |
| alternatives that need detail each | **tabbed panels** | one enormous table |
| a sequence of steps / events | **timeline** (vertical, status per node) | bulleted list |
| hierarchy / dependencies | **SVG diagram** with arrow markers | ASCII art |
| a plan vs reality gap | **progress bars** with an explicit target | a single "% done" |
| inputs that change a result | **calculator / simulator** with live output | static numbers |
| one item chosen from N | **pricing / plan cards** | table with 12 columns |
| status of many items | **status list** with a badge per row | traffic-light emoji |
| before / after | **two-column diff** or **split bar** | two separate charts |
| ranking | **sorted bar list** with rank numbers | unsorted table |

## Interaction policy

Not every shape should be interactive. A widget that *can* be poked at but does not change its
answer is worse than a static one: it invites clicks, returns nothing, and hides the real content
behind affordances. The test is one question:

> **Does the answer itself change when the reader supplies something different?**

If yes → the control is part of the answer, build it. If no → ship it static.

### Tiers

| Tier | Meaning | Shapes |
|---|---|---|
| **Required** | Without the control the shape cannot answer its question. | calculator / simulator, tabbed panels |
| **Conditional** | Add the control **only** when the reader is genuinely deciding something and the weighting/ordering is not yet fixed. | comparison table (weights), bar chart (sort), ranking list |
| **Hover only** | The data is static, but reading a dense cell needs help. Hover/focus reveals a value that is also present in the DOM. | heatmap, line chart (crosshair) |
| **Static** | The answer is fixed. Adding controls would be decoration. | KPI strip, line chart, timeline, SVG diagram, pricing cards, status list, progress |
| **Outbound** | The widget sends a message back to the conversation. Rare — see below. | an explicit "reply with this" button |

### Rules

1. **A control must change a rendered number, ordering, or selection.** If you cannot say in one
   sentence what changes when the slider moves, delete the control.
2. **Conditional means ask first.** If the answer already implies fixed weights ("we care most about
   cost"), ship the table static with the weights as text. Only add weight sliders when the message
   is *"it depends on what you value"*.
3. **Hover is never the only channel.** Anything revealed on hover must also be present for keyboard
   focus and screen readers — put the value in the DOM (`<title>`, visible text, or `aria-label`),
   and make the element focusable with `tabindex="0"`. Touch devices have no hover.
4. **Do not animate to look alive.** No pulsing, no counting-up numbers, no skeleton shimmer on static
   data. Motion is reserved for a state change the reader caused.
5. **State does not survive.** No `localStorage` — it throws in the opaque origin. Tab selection, slider
   positions and filter state reset whenever the block re-renders. If the reader must keep a choice,
   have them put it in their next message.
6. **Sending back is opt-in, not decoration.** A widget may compose a message for the reader to send
   (protocol: `references/reply-protocol.md`) only when the reader's answer genuinely needs structure
   the chat box cannot express — picking one of five options, a small form, a rating. Do not attach a
   "send this" button to a visual that the reader just needs to look at, and never send on the
   reader's behalf without showing them the text first.
7. **Respect `prefers-reduced-motion`** on any transition you add.

### Interactive patterns that are already here

- **Tabs** (`references/interactive.md`) — required: switching views is the point.
- **Calculator** (`references/interactive.md`) — required: output is a function of input.
- **Heatmap hover** (`references/charts.md`) — hover/focus reveals the cell value.
- **Bar sort toggle** (`references/charts.md`) — conditional: reorders the same data.
- **Weighted comparison** (`references/compare-and-sequence.md`) — conditional: weights drive the score.
- **Flowchart highlight** (`references/diagram-and-status.md`) — hover/focus traces a path.

Everything else in those files is deliberately static. Do not "improve" a static snippet by bolting
on a control — copy it as-is.

## Component reference files

The skeletons live in `references/`, one file per family. **Read only the file you need** —
that is the point of keeping them out of this file: the shapes are verbose, and loading all of
them for a single KPI card would waste most of the context.

| The shape you picked | Read this file |
|---|---|
| bar chart · line chart · heatmap | `references/charts.md` |
| tabbed panels · calculator / simulator | `references/interactive.md` |
| comparison table · timeline · progress · pricing cards | `references/compare-and-sequence.md` |
| SVG diagram · KPI strip · status list | `references/diagram-and-status.md` |
| **the widget must send something back to the conversation** | `references/reply-protocol.md` |

All paths are relative to this skill's directory. The absolute directory is listed in
the "Skill resources" section of the block that loaded this skill — prefix it to the relative
path and pass the result to `read_file`. Read at most two files for one answer; if you think
you need three, the answer probably wants one shape, not three.

Each reference snippet is complete: markup, `<style>` and any `<script>`. Copy it and replace
the content. Keep the `<style>` block and the input-validation logic — they encode this frame's
constraints (theme tokens, the 560 px height cap, the opaque origin).

The last row is different from the others: it is a **protocol**, not a shape. Read it only when the
reader is expected to answer something inside the widget (pick an option, rate something, confirm a
step). Most widgets do not need it — a visual answer is usually something to read, not a form.

If nothing in the table fits, build from the primitives in **Layout system** above and follow the
**Stability checklist** below. Do not invent a reference file path — the five above are the only
ones.


## Rules that keep the widget honest

1. **Never invent data.** If you use sample numbers, say so in the widget ("示例数据"). A pretty
   chart of made-up figures is worse than no chart.
2. **Validate every input** — `valueAsNumber`, `Number.isFinite`, domain bounds, non-zero divisors.
   Disable the result (or show an error) instead of printing `NaN` or `Infinity`.
3. **Every control must do real work.** A slider that changes nothing is a lie. If the user only
   needs to read numbers, drop the control.
4. **Accessibility** — semantic headings, `<label for>` on every input, keyboard-reachable buttons,
   visible focus, contrast ≥ 4.5:1 against the surface, and `aria-live="polite"` on results that
   change without navigation.
5. **No `top-level await`.** The frame runs a classic script context in `srcdoc`; keep logic
   synchronous or use `.then()`.

## Stability checklist (run before shipping)

These are the failure modes that actually show up in this frame. Check each one.

**Scripts**

- [ ] Every `<script>` block is **complete** — no half-written IIFE, no dangling `catch`.
- [ ] No `top-level await`, no ESM syntax (`import` / `export`), no `type="module"`.
- [ ] The script runs **after** the DOM it queries. Put `<script>` at the end, or wrap in
      `DOMContentLoaded`. Query the element first and bail with `if (!el) return;` rather than
      throwing.
- [ ] No `localStorage` / `sessionStorage` / `document.cookie` / `indexedDB` — they throw here.
      Keep state in a closure variable.
- [ ] No `window.parent.*`, no `window.top.*`, no host bridge.
- [ ] Wrap anything that touches `JSON.parse` of user text in `try/catch`.

**Numbers**

- [ ] No `NaN`, `Infinity`, `-0`, or `undefined` ever reaches the DOM.
- [ ] Every division is guarded against a zero denominator.
- [ ] `Math.round` / `toFixed` applied once, at the display step, not mid-calculation.
- [ ] **Watch float accumulation before rounding.** `toFixed(2)` on a running sum of weighted terms
      is order-dependent: `4.5*0.3 + 3*0.25 + 4*0.2 + 4.5*0.15 + 3.5*0.1` evaluates to
      `3.9249999999999994` → `"3.92"`, while the true value is `3.925`. Either show more precision,
      or round the *scaled* integer: `Math.round(sum * 1000) / 1000`. Do not let two places in the
      same widget print different values for the same quantity.
- [ ] Percentages have a defined base and the base is visible.
- [ ] Every number carries its unit; units are consistent across a chart series.

**Layout**

- [ ] No fixed pixel `width` on a text container; `max-width` only.
- [ ] `min-width: 0` on flex/grid children holding long text.
- [ ] `overflow-wrap: anywhere` on cells that may hold IDs, URLs, or stack traces.
- [ ] Grids collapse: check a ~320 px width and a ~560 px width.
- [ ] Nothing important relies on hover — there is no persistent pointer on touch.
- [ ] The first screen (≈560 px) carries the answer, not the setup.
- [ ] Tab containers have `min-height` so switching does not jump the frame.

**Charts**

- [ ] Axes labelled, gridlines present, bar charts start at zero.
- [ ] Categories sorted when order matters.
- [ ] `<svg>` has `role="img"` and a `aria-label` that states what the chart shows **and its unit**.
- [ ] Text ≥ 12 px in SVG viewBox units, and does not overlap lines or other text.
- [ ] Colour carries meaning only alongside a label or shape (colour-blind safe).

**Theme**

- [ ] No hardcoded `#fff`, `#000`, or grey hexes — tokens only.
- [ ] Nothing depends on the page being light or dark (check both mentally).
- [ ] `color-scheme` is left to the canvas; do not fight it.

**Sandbox**

- [ ] No CDN `<script src>`, no `@import url(...)`, no web fonts.
- [ ] Images are `data:` URLs or absolute `https:` URLs — never relative paths.
- [ ] Animations respect `prefers-reduced-motion`.

## When to tell the user you cannot do it

- They ask for a chart of **live** data (current price, weather, their database) and no tool has
  returned that data: say the session has no live data source and offer a labeled sample or ask for
  the numbers. Do not fabricate a figure and do not imply the chart is real.
- They ask for a **3D / WebGL** scene or a heavy charting library: the sandbox has no import map and
  no guaranteed network. Offer an SVG approximation instead of shipping a blank frame.
- They ask for the widget to **save state across messages** or to call back into the app: the frame
  has an opaque origin with no persistence and no host bridge. Say so, and put the values they need
  to keep into their next message instead. **This includes tab selection** — it resets to the first tab
  on every render.
- They ask for something that needs **many megabytes of data** (a full table of thousands of rows):
  the frame is for a visual answer, not a data browser. Summarise, chart it, or offer a file instead.
