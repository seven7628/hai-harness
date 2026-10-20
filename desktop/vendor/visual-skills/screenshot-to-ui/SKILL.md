---
name: screenshot-to-ui
version: 1.0.0
description: 'Use when the target already exists as an image and the user wants it as code - a screenshot, mockup, wireframe, or a photo of another product. You read the image yourself with read_file and implement it in the project''s existing stack. Triggers: convert design, Figma, pixel-perfect, replicate page, clone this UI, build from picture, 截图转代码, 还原界面.'
requires:
  input: [image]
---

# Screenshot / mockup → front-end code

Everything here is done **by you**: you read the image, you write the code. There is no separate
vision service in this harness, no OCR step, and no external screenshot-to-code API — if a plan
needs one of those, the plan is wrong.

## How to run this

- Read the reference image with `read_file` (no line range — line ranges do not apply to images).
  `png`, `jpg/jpeg` and `gif` come back as an actual image block you can look at. If the file is a
  `.webp` or a PDF mockup, convert it first with `run_python` (Pillow / `pypdfium2` are preinstalled)
  and then read the PNG.
- Then write the implementation with `write_file` / `edit_file`. Declare deliverables on the same
  tool call when you use `run_python`.
- Work in the user's existing stack. Look for `package.json`, `tsconfig.json`, an existing component
  directory and a styling convention **before** choosing React vs plain HTML, CSS modules vs Tailwind.
  A pixel-perfect page in the wrong framework is a rewrite, not a deliverable.
- If the user only wants a static preview, one self-contained `.html` file is the cheapest correct
  answer — and opening it in the preview panel needs no build step.

## Phase 1 — read the image, write an inventory

Do this **before** writing code, and keep it short (a dozen lines, not an essay). For each screen:

1. **Layout skeleton** — how many columns, what is fixed vs fluid, header/sidebar/content/footer,
   and where the visual hierarchy sits (what the eye hits first).
2. **Components** — every button, input, tab, table, card, list, badge, avatar, and its state
   (default / hover / active / disabled) if the image shows more than one.
3. **Content** — actual strings, numbers, labels and their language. Reuse them verbatim; inventing
   longer copy changes the layout you are trying to match.
4. **Design tokens** — sample the colors from the image (approximate hex is fine), note the type
   scale (which is H1, which is body, what the smallest label is), spacing rhythm, radii, shadows,
   and border treatments.
5. **Assets** — icons, illustrations, logos, photos. Say which ones you can reproduce as inline SVG,
   which are text, and which genuinely need a file from the user.

If the image is low-resolution or partially obscured, say which parts you are guessing **in your
reply**. Do not silently invent a whole section.

## Phase 2 — implement

- **Structure first, styling second.** Get the DOM/section tree matching the skeleton, then apply
  tokens. This is what makes the result survive a resized window.
- **Reuse what the repo has.** Existing button/input/card components, the project's color tokens,
  its font stack. A new component that duplicates an existing one is a regression even if it looks right.
- **Responsive by construction** — flex/grid, `min-width: 0` on flex children, no fixed pixel widths
  for text containers. Check the narrow case: what wraps, what collapses, what scrolls.
- **Real states** — focus-visible rings, disabled styles, empty states, loading states. The screenshot
  only shows one state; ship the others or the component is half-built.
- **Accessibility is part of matching** — `<button>` not `<div onclick>`, `<label for>` on inputs,
  alt text on images, heading order, contrast. This is not optional polish.

## Phase 3 — compare and correct

- Render your result and look at it (open the file in the preview panel, or run the dev server and
  take a screenshot with the browser tooling if the project has it).
- Compare against the reference on: column widths, vertical rhythm, type scale, one accent color,
  corner radii, and the density of the densest row.
- Fix the top three discrepancies. Do not chase sub-pixel differences; do chase "the header is two
  lines instead of one" and "the cards are 40 px apart instead of 16".

## Reproducing a *live site* instead of an image

If the reference is a URL rather than a file, use the browser tooling available in this session
(ego-browser / the `browser_*` tools) to open it, screenshot it, and read the computed styles you
need. **Before you do that, confirm the user owns the site or has permission to replicate it** —
unauthorized replication can violate copyright and terms of service. If they cannot confirm, stop
and offer an original design inspired by general layout patterns instead.

## When to tell the user you cannot do it

- The image cannot be read (unsupported format, corrupt, or a PDF you cannot convert): say exactly
  which file and why, and ask for a PNG/JPG export. Do not guess the design from the filename.
- The mockup depends on assets you do not have (a licensed brand font, product photography, an icon
  set the project does not include): list them and ask. Do not substitute a random lookalike and
  claim it matches.
- The user wants a faithful copy of a site they have no rights to: refuse the replication and offer
  an original layout instead.
- The image is a photo of a screen at an unreadable angle/resolution: say what is legible and ask
  for a cleaner capture before building on guesses.
