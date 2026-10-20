---
name: visual-review
version: 1.0.0
description: Review a UI you or the user already has — screenshot, rendered page or component code — against accessibility, layout, typography, color and interaction-state criteria, and return prioritized findings with concrete fixes. Use this when the user asks to review, audit, critique or improve the look, feel, accessibility or polish of an interface.
requires:
  input: [image]
---

# Visual review of an interface

You look at the thing and you report what is wrong. No external audit service is involved; the
findings must come from what you can actually see and read in this session.

## How to run this

- Get the target in front of you first: read the screenshot with `read_file` (no line ranges for
  images), or read the component/CSS files with `read_file` / `grep`.
- If the user points at a running dev server, use the session's browser tooling to screenshot it at
  **two widths** (a desktop width and ~390 px). Half of all layout bugs only exist at one of them.
- Report findings, not impressions. Each finding needs: what is wrong, where (file:line, or which
  region of the screenshot), why it matters, and the concrete fix.
- You are reviewing **what exists**. Do not rewrite the whole component unless asked — a review that
  returns a full replacement is a rewrite, and it discards decisions you did not evaluate.

## What to check, in priority order

Order matters: a beautiful page with no focus ring is worse than a plain page that works.

### 1. Blocking (fix first)

- **Contrast** — body text below 4.5:1, large text below 3:1, placeholder text that is the only
  label, text over an image without a scrim. Compute it, don't eyeball it: `#767676` on white is
  exactly 4.54:1; anything lighter fails.
- **Focus visibility** — an interactive element with `outline: none` and no replacement. Tab through
  the page in the browser tooling if available.
- **Keyboard reachability** — clickable `<div>`/`<span>` without role+tabindex+key handlers; custom
  dropdowns that trap or lose focus.
- **Form labelling** — `<input>` without a `<label for>` or `aria-label`; errors not associated with
  the field (`aria-describedby`).
- **Layout breakage at 390 px** — horizontal scroll, overlapping text, a table that cannot be read,
  a fixed-width sidebar eating the viewport.
- **Unreadable state in the other theme** — hardcoded `#fff` background or `#333` text that only works
  in one of light/dark.

### 2. Correctness

- Numbers without units, sorting that does not match the label, a chart whose axis does not start at
  zero for bars (or a truncated axis without a break marker).
- Truncation that hides meaning (`text-overflow: ellipsis` on the primary identifier).
- Date/number formatting inconsistent within one screen.
- Empty and loading states missing, so the screen looks broken while data arrives.

### 3. Craft

- **Type scale** — more than 3–4 sizes on one screen usually means no scale. Check line-height
  (body ≈ 1.5–1.7) and measure length (45–75 characters).
- **Spacing rhythm** — one-off margins (13 px, 27 px) instead of a 4/8 px scale; inconsistent gaps
  between sibling cards.
- **Alignment** — optical alignment of icons vs text baselines; labels not aligned in a column.
- **Color use** — accent color doing five unrelated jobs; semantic colors (red/green) used for
  decoration.
- **Density** — the densest row of a table is the moment the design is judged; check that one.
- **Motion** — animation that ignores `prefers-reduced-motion`, or easing that makes a control feel
  laggy.
- **Icons** — mixed stroke widths, mixed families, or an icon standing alone with no label where the
  meaning is not universal.

### 4. Consistency with the codebase

- New colors/shadows/radii that duplicate existing tokens (grep the token file first).
- A component that re-implements an existing one.
- Class naming or file placement that breaks the surrounding convention.

## Report format

Group by priority, cap at ~10 findings (the rest are noise), and end with the two or three changes
that would move the result the most:

```markdown
## Blocking
- `settings.tsx:88` — the Save button removes the focus ring (`outline: none`) with no replacement.
  Keyboard users cannot see where they are. Fix: use `:focus-visible` with the app's ring token.

## Correctness
- Revenue chart: the y-axis starts at 40, so a 5% difference looks like 3×. Fix: start at zero or
  add an explicit axis break.

## Craft
- Four type sizes within 12 px of each other. Fix: collapse to a 3-step scale (12 / 15 / 20).

## Highest impact
1. Fix focus rings (blocking, affects every control).
2. ...
```

If you found nothing worth reporting in a category, say so — an empty section is information.
Never pad a review with generic advice ("consider improving accessibility") that is not tied to what
you actually saw.

## When to tell the user you cannot do it

- No screenshot and no runnable page was provided: ask for one of them. Reviewing a description of
  an interface produces fiction.
- The screenshot is too small or cropped to judge contrast or spacing: say which judgement you had
  to skip rather than guessing numbers.
- The user wants a measured WCAG conformance report (every criterion, signed off): say this is a
  design review, not a conformance audit, and list what you did verify.
- The target is a third-party site the user does not own and they want a full replication review:
  reviewing is fine, but do not produce a replica — see the authorization rule in
  `screenshot-to-ui`.
