---
name: pptx
version: 1.0.0
description: Create, read and edit PowerPoint presentations (.pptx/.pptm) with python-pptx — no rendering (python-pptx cannot draw a slide and this machine has no LibreOffice), table text that a naive shape walk silently drops, group shapes that need recursion, EMU geometry, speaker notes and CJK fonts that need an explicit <a:ea>. Use this when the user asks to create or edit a slide deck, or to extract the text, tables, speaker notes or structure out of an existing .pptx.
---

# PowerPoint presentations (python-pptx 1.0.2)

## How to run this

- Do all presentation work through `run_python`. The working directory **is the workspace root**: write deliverables as workspace-relative paths (`decks/q3.pptx`) so the user can find them. Under the default sandbox policy the workspace, `$HOME` and `/tmp` are writable and `~/.go-code` is denied; credential files under `$HOME` (`~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.netrc`, `~/.kube`) are **not** covered by any sandbox rule, so never read or copy them. Keep deliverables in the workspace and use `/tmp` for scratch files only.
- Declare your deliverables: pass `outputs=["decks/q3.pptx"]` on the same `run_python` call. The desktop app uses these paths for the artifact card, the file list and the preview tab (it opens a new tab, or refreshes the one already showing that file) — a file you don't declare stays invisible to the user unless they go looking for it on disk.
- Create parent directories yourself: `os.makedirs("decks", exist_ok=True)`.
- Write to a **new** file when you produce a deliverable. `Presentation.save()` re-serialises the whole package, so an in-place transform that goes wrong over the user's only copy of a deck is unrecoverable.
- The first `run_python` call of a session can take minutes while the managed runtime bootstraps (tens of MB on disk — ≈70 MB measured — once). Pass `timeout` (default 120 s, max 3600) for large documents.
- **Never `pip install`.** Installed here: `python-pptx`, `lxml`, `Pillow`, `python-docx`, `openpyxl`, `xlsxwriter`, `pypdf`, `pypdfium2`, `mammoth`. **Not** installed: `pandas`, `numpy`, `matplotlib`, `docx2pdf` — and there is **no scriptable renderer**: no LibreOffice/`soffice` (verified: absent from `/Applications` and from `PATH`), and although WPS Office is installed (`/Applications/wpsoffice.app`) this skill does not drive its GUI. So there is no renderer and no pptx→PDF path (Rule 1) — **if the user needs a PDF or an image of a slide, ask them to export it from WPS or PowerPoint.**
- A `.pptx` is a ZIP — `read_file` shows you binary noise, not text. Verify your own output by re-opening it with python-pptx in a second `run_python` call and printing the slides/shapes you produced.
- Start from the user's file (`Presentation("input.pptx")`) when editing an existing deck; only use a blank `Presentation()` when creating something from scratch. A blank deck has none of their theme, master or layouts, and the result will look nothing like their template.
- python-pptx reads and writes `.pptx` and `.pptm` (the same OOXML container). It only ever sees *stored* shapes — never layout, animation or rendering.

## Rule 1 — python-pptx cannot render: you cannot see a slide, and the in-app preview is an approximation

python-pptx is a reader/writer of the OOXML package. It has no layout engine and no rasteriser: it cannot draw a slide, cannot compute where text wraps, and cannot tell you what a slide *looks* like. This machine has no *scriptable* renderer either (verified: no LibreOffice/`soffice` in `/Applications` or on `PATH`; WPS Office is installed, but this skill does not drive its GUI), so the usual escape hatch — pptx → PDF → look at the picture — does not exist here. If the user needs a PDF or an image, they have to export it from WPS or PowerPoint themselves.

What you have instead:

- **To see a deck: the app's own preview.** It rebuilds each slide approximately — every shape absolutely positioned from the file's real EMU geometry, images inlined as data URLs — and the page header says plainly that it is an *approximate layout reconstruction, not a render*: no font fidelity (whatever the browser has is substituted), no animations or transitions, SmartArt/charts/embedded objects not drawn. Unsupported shapes get a **visible placeholder in place** instead of being dropped silently, and a footer counts what was left out. Limits: 60 slides, 200 shapes per slide, 24 MB of images — beyond that the preview says so rather than showing a broken page. So the preview answers "which slide has what, roughly where"; it is never evidence of pixel fidelity.
- **To read content: python-pptx, on the real geometry and text.** Rules 2–4 are the traps that make a naive read silently incomplete; Rules 5–10 are the ones that bite when you write.
- **To check your own work: structurally.** Re-open the file and print the slide count, per-slide shape/text/table dumps, and the geometry you set (Rule 7). You cannot look at the deck, so do not describe how it *looks* — describe what is *stored*.

If the user wants to see the result faithfully, they open it in PowerPoint/Keynote; say that rather than implying you looked at it.

## Rule 2 — table text is NOT in `shape.text_frame.text`: a naive walk drops every table

A table is a graphic frame, not a text shape. Measured on a slide holding a real table: the table shape reports `has_text_frame=False` and `has_table=True`, so the obvious walk returns **nothing at all** for that slide:

```python
[sh.text_frame.text for sh in slide.shapes if sh.has_text_frame]   # -> []   the table is gone
```

No exception, no warning — an "export the text of this deck" built on that line silently loses every table, and a deck's numbers usually live in tables, so the summary reads as if the data were never there. Go through the table object instead:

```python
for shape in slide.shapes:
    if shape.has_table:                       # check this BEFORE has_text_frame
        for row in shape.table.rows:
            print([cell.text for cell in row.cells])
```

Measured: the naive walk gave `[]` for the table slide; the cell walk gave `['指标', '', '']`. Note the empty elements: a cell with no text is `''`, not missing — print row by row so a blank cell does not shift the columns in your output.

`has_text_frame` is `False` for pictures, lines and charts too, so treat it as a **per-shape branch**, not a filter. Cells own a full text frame (`cell.text_frame`) if you need per-paragraph detail; `cell.text` joins paragraphs with `\n`.

## Rule 3 — group shapes hide their children from `slide.shapes`: recurse or lose content

Measured: a slide whose two text boxes sit inside one group reports at the top level only

```
[('PLACEHOLDER', ''), ('GROUP', False)]      # the group's own text is empty
```

and the two texts are **unreachable** from `slide.shapes`. Recursing into `group_shape.shapes` returned `['组内文字A', '组内文字B']`. So walk recursively:

```python
from pptx.enum.shapes import MSO_SHAPE_TYPE

def walk(shapes, tf=None):
    """Yield (page_left, page_top, shape) for every shape, descending into groups.

    Group children are mapped into slide coordinates on the way out, so every position
    it yields is comparable. tf = (sx, sy, tx, ty) with page = (tx + x*sx, ty + y*sy);
    None means slide space.
    """
    for sh in shapes:
        sx, sy, tx, ty = tf or (1, 1, 0, 0)
        if sh.shape_type == MSO_SHAPE_TYPE.GROUP:
            chOff, chExt = sh._element.chOff, sh._element.chExt
            box_w, box_h = sh.width * sx, sh.height * sy     # this group's box, on the page
            csx = box_w / chExt.cx if chExt.cx else sx       # child space → page
            csy = box_h / chExt.cy if chExt.cy else sy
            yield from walk(sh.shapes, (csx, csy,
                                        tx + sh.left * sx - chOff.x * csx,
                                        ty + sh.top * sy - chOff.y * csy))
        else:
            yield tx + sh.left * sx, ty + sh.top * sy, sh
```

Two things to know about group children:

- **Their coordinates are in the group's own child space**, not the slide's: a child of a group whose `off` is (2 in, 3 in) with `chOff` (0, 0) still reports `left = 0.5 in` — measured — while it actually sits at 3 in on the page. One more thing about *who wrote the file*: a group **python-pptx built** is normalised to its children's bounding box (`pptx/shapes/shapetree.py:511`, `_recalculate_extents()`, re-run on every `add_*`), so its `chOff` is the children's min x/y — **not** `(0,0)` (measured: children at 1 in → `chOff = (914400, 914400)`) — while its `off` is set to that same box. The 3.2 in / 0.5 in figures above come from PowerPoint-saved decks, where `off` is the placed box and `chOff = (0,0)`. The mapping is `page = box + (child − chOff) × box/chExt`, and for a **nested** group `box` is the parent's box *already mapped to the page*, not its raw `ext`: composing with the raw `ext` misplaces every grandchild (measured on a two-level deck: 3.52 in instead of the correct 3.49 in). So never compare a child's raw `left/top` against slide EMU, and never sort children alongside top-level shapes — `walk` above does the composition for you and hands back page coordinates. (`chOff`/`chExt` are required in a conformant file, so a real deck always has them; if a malformed one lacks them, `walk` fails loudly with `InvalidXmlError` (`from pptx.exc import InvalidXmlError`) rather than misplacing the shape — and the two halves of that belong to two different objects (both measured): the *access* builds the missing `chOff`/`chExt` as **empty elements** (`get_or_add_*`), and it is the *next* read — `chExt.cx` on an element that has no `cx` — that raises `required 'cx' attribute not present`. That access is a side effect: save after it and the file gains empty `<a:chOff/>`/`<a:chExt/>` elements, so do not save a file you only meant to read.) (If you only need text, ignore geometry entirely.)
- **`slide.shapes` yields spTree order, which is z-order (back to front), not reading order.** If the user asks what the deck *says*, that order will not match what the eye reads; sort by the page `(top, left)` that `walk` yields — sorting the raw attributes instead inverts the order of any grouped slide (measured: a group at page y=3.2 in sorted *above* a title at y=1.0 in, because its child reported y=0.2 in) — and say it is your reconstruction rather than the file's order.

This is the base of every shape loop in this skill — the "read everything" recipe is just `walk`, plus the table branch from Rule 2, plus the notes branch from Rule 9:

```python
def dump_deck(path):
    prs = Presentation(path)
    for i, slide in enumerate(prs.slides, 1):
        print(f"--- slide {i} ---")
        for left, top, sh in sorted(walk(slide.shapes), key=lambda r: (r[1] or 0, r[0] or 0)):
            if sh.has_table:                                  # Rule 2: tables first
                for row in sh.table.rows:
                    print("[table]", [c.text for c in row.cells])
            elif sh.has_text_frame and sh.text_frame.text.strip():
                print("[text]", sh.text_frame.text)
        if slide.has_notes_slide:                             # Rule 9: has_* first (no side effect)
            notes = slide.notes_slide.notes_text_frame.text.strip()
            if notes:
                print("[notes]", notes)
```

To **build** a group (writing, not reading), `add_group_shape()` hands you an empty one; populate it through `g.shapes`, not `slide.shapes`:

```python
g = slide.shapes.add_group_shape()
tb = g.shapes.add_textbox(Inches(1), Inches(1), Inches(3), Inches(0.6))
tb.text_frame.text = "组内文字"
```

Measured on that three-liner: `off = (1 in, 1 in)`, `ext = (3 in, 0.6 in)` and `chOff = (1 in, 1 in)` — the group's box is *derived* from its children and re-derived on every `add_*`, so a group you build yourself rarely has `chOff = (0,0)` (see the `chOff`/`chExt` note above, and note that `off == chOff` there). The practical upshot: pass **slide** coordinates to `g.shapes.add_textbox(...)` and that is where the shape lands, which `walk` confirms.

## Rule 4 — `text_frame.text = ...` replaces, and unfilled placeholders still exist

Assignment on a text frame **clears it first**; it is not an append:

```python
tf = slide.shapes.title.text_frame
tf.text = "第一行"
tf.text = "第二行"        # 第一行 is REPLACED, not kept
tf.text                   # '第二行'
p = tf.add_paragraph()    # appends a paragraph — takes NO argument here
p.text = "第三行"
tf.text                   # '第二行\n第三行'
```

Measured exactly as above. If you are used to python-docx, note the difference: `add_paragraph()` in python-pptx takes **no** text argument (`p = tf.add_paragraph(); p.text = "..."`). `_Paragraph.text = ...` also replaces that one paragraph's content, so "set `tf.text` once, then append paragraphs" is the reliable pattern.

The second half of this trap is **placeholders**. A placeholder that was never filled is still a shape in the file, with `text == ''`:

- When **reading**: an empty placeholder is not content. Do not print it as a bullet, and if a slide looks empty, check `shape.is_placeholder` / `shape.placeholder_format.type` and the slide's layout before concluding the slide is blank — "blank" usually means "the placeholders were never filled", not "there is nothing here".
- When **writing from a layout**: fill the placeholders the layout already gives you (`slide.shapes.title`, `slide.placeholders[...]`) instead of adding a text box on top of them. The placeholder carries the theme's position, size and font; a text box dropped over an unfilled placeholder leaves **two** overlapping shapes in the file, and the user sees the leftover empty placeholder box when they edit the deck.

## Rule 5 — CJK text: content is safe, fonts are not automatic (same shape of problem as docx Rule 5)

CJK content round-trips through python-pptx unchanged — no encoding setup, do not add any. The problem is font selection, and it fails **silently**: setting `run.font.name` writes only the Latin typeface, so Chinese/Japanese/Korean glyphs do not use it at all.

Measured: after `run.font.name = "Source Han Sans"` the run XML contains `<a:latin typeface="Source Han Sans"/>` and **no `<a:ea>`** — so the renderer falls back to the theme's East-Asian font. You must write the East-Asian and complex-script attributes yourself:

```python
from pptx.oxml.ns import qn

def set_font(run, name):
    """Pin a font for every script class of a run (latin + East-Asian + complex)."""
    run.font.name = name                       # creates <a:rPr> with <a:latin>
    rPr = run._r.get_or_add_rPr()
    for tag in ("a:ea", "a:cs"):
        el = rPr.find(qn(tag))
        if el is None:
            el = rPr.makeelement(qn(tag), {})
            rPr.append(el)
        el.set("typeface", name)
```

Measured after the fix: `<a:latin>`, `<a:ea>` and `<a:cs>` all carry the font. Run this for every run that contains CJK (the loop is idempotent — re-running just re-sets the typeface). Keep the schema order `latin → ea → cs`: appending is fine right after you set `run.font.name` on a fresh run, but if the run already carries other `a:rPr` children (a hyperlink, `a:sym`), insert the two elements right after `<a:latin>` instead of appending.

Same conclusion as the docx skill: only pin fonts you can reasonably expect the user to have (`PingFang SC`/`Songti SC` on macOS, `Microsoft YaHei`/`SimSun` on Windows), and tell the user which font you pinned — a font that is absent gets substituted, which is the tofu/fallback problem you were trying to avoid.

## Rule 6 — three different failures raise the same exception: check `exists` and ZIP yourself

Measured: `Presentation()` raises `PackageNotFoundError: Package not found at 'X'` for **all** of these:

| what you passed | actual cause |
|---|---|
| a legacy `.ppt` (OLE2, the pre-2007 format) | wrong format — not an OOXML package |
| a corrupt/truncated `.pptx` | broken container |
| a path that does not exist | a typo or a wrong working directory |

The message is identical, so **you cannot tell the three apart from the exception**. Report the raw text and you will tell the user "the file is missing" when the real problem is the format, or "unsupported format" when the path is simply wrong. Discriminate before you load:

```python
import os, zipfile
from pptx import Presentation

def load_deck(path):
    if not os.path.exists(path):
        raise FileNotFoundError(f"no such file: {path}")               # cause 1
    if not zipfile.is_zipfile(path):
        raise ValueError(f"not an OOXML package (legacy .ppt or corrupt): {path}")   # causes 2/3
    return Presentation(path)
```

Both `.docx` and `.pptx` are ZIP containers, so `zipfile.is_zipfile` is the cheap format probe; a `.ppt` is an OLE2 binary and fails it. The docx skill carries the same note — do not read `PackageNotFoundError` as "file is missing".

## Rule 7 — every length is EMU: use `Inches()/Pt()/Emu()`, never bare numbers

914400 EMU = 1 inch, 12700 EMU = 1 point. Measured default template: `prs.slide_width = 9144000` (10 in) and `prs.slide_height = 6858000` (7.5 in) — i.e. **4:3**, which surprises people who expect 16:9:

```python
from pptx.util import Inches, Pt, Emu

prs.slide_width  = Inches(13.333)     # 16:9 — set BEFORE adding slides
prs.slide_height = Inches(7.5)        # python-pptx does not rescale what is already placed

slide.shapes.add_textbox(Inches(1), Inches(1), Inches(4), Inches(0.6))
run.font.size = Pt(18)                # 228600 EMU
```

`Inches()/Pt()/Emu()` return integer-like `Length` values, so arithmetic and comparisons work. Bare numbers are accepted **silently** and mean EMU: `width=3` means three EMU — a shape 0.0000033 in wide, invisible on the slide, with no error. Every `left/top/width/height` you read back is EMU too. (The app preview converts at 96 dpi: `px = EMU / 9525`, so a 10-inch slide shows as 960 px wide — useful when comparing a shape's size against the preview.)

## Rule 8 — pictures: give one dimension and the aspect ratio is preserved

```python
slide.shapes.add_picture("chart.png", Inches(1), Inches(1), width=Inches(3))
```

Measured: with only `width` given, the height came back `1371600` EMU (1.5 in) for a 2:1 source image — the original ratio, computed by python-pptx. Give **both** `width` and `height` and it stretches the image to exactly that box (silent distortion, no warning), so set both only when you deliberately want a stretch to fill a frame.

Also: the image file must exist on disk and be a format Pillow/python-pptx can read (PNG/JPEG/GIF/BMP/TIFF). Images are **embedded** in the `.pptx`, so a picture-heavy deck grows by the images' size — and the app preview inlines them as data URLs under a 24 MB budget, labelling any it had to skip.

## Rule 9 — speaker notes live on `slide.notes_slide`, and *reading* them has a side effect

```python
if slide.has_notes_slide:                       # check first — no side effect
    notes = slide.notes_slide.notes_text_frame.text
    print(notes)

slide.notes_slide.notes_text_frame.text = "讲稿要点"    # writing creates the notes slide if needed
```

`notes_text_frame` is an ordinary text frame, so Rule 4 applies to it (assignment replaces; `add_paragraph()` appends).

The trap is the asymmetry: **`slide.notes_slide` creates a notes slide when the slide has none** (that is how the property is implemented), while `slide.has_notes_slide` only tests. Verified on a fresh deck: `has_notes_slide` was `False`, and after one read through `notes_slide` it was `True`. So a "read-only" pass that touches `notes_slide` for every slide **mutates the deck in memory** — if you then save it, the file gains empty notes slides the user never had. Always gate the read on `has_notes_slide`.

## Rule 10 — charts: you can author them, but the app preview does not draw them

```python
from pptx.chart.data import CategoryChartData
from pptx.enum.chart import XL_CHART_TYPE

data = CategoryChartData()
data.categories = ["Q1", "Q2", "Q3"]
data.add_series("销量", (19.2, 21.4, 16.7))
slide.shapes.add_chart(XL_CHART_TYPE.COLUMN_CLUSTERED,
                       Inches(1), Inches(2), Inches(6), Inches(4), data)
```

`add_chart(chart_type, x, y, cx, cy, chart_data)` writes a real chart part with its own embedded data, and PowerPoint/Keynote draw it normally. The **app preview does not**: it puts a visible placeholder in the chart's place, labelled with the chart type and series names (`图表（COLUMN_CLUSTERED，1 个系列：销量）未渲染`). Tell the user that when you add a chart — otherwise a chart that is perfectly fine in the file reads as "the preview is broken".

For reading, a chart is a graphic frame like a table: `has_text_frame` is `False`, and `shape.chart` is the only way in (`chart.chart_type`, `chart.series[i].name`). If the user needs the chart's *numbers*, do not treat what you can extract as authoritative — read them from the source data you were given instead, and say where the numbers came from.

## When to tell the user you cannot do it

- **The user wants pixel-level / faithful slides** ("make it look exactly like this", "does it fit on one slide?"). There is no **scriptable** renderer on this machine — no LibreOffice/`soffice`, no PowerPoint, and WPS Office (`/Applications/wpsoffice.app`) is installed but not driven by this skill — and python-pptx has no layout engine, so it cannot paginate, wrap text or measure anything. The in-app preview is an *approximate* reconstruction (no font fidelity, no animation, no SmartArt/charts). Say that the deck has to be opened in PowerPoint/Keynote/WPS to judge how it looks — and that if the user wants a PDF or an image of a slide, they have to export it from one of those, because nothing on this machine can.
- **The input is a legacy `.ppt`** (OLE2, pre-2007). python-pptx cannot open it — and the exception text is misleading (Rule 6). Ask the user to re-save it as `.pptx` in PowerPoint/WPS; there is no converter here. Do not guess at the content.
- **Animations, transitions or SmartArt content.** python-pptx does not model them: you can neither read what a deck has nor create it. SmartArt is worse than invisible — its text is not in the shape at all (it lives in a separate diagram part python-pptx never loads), so a shape walk returns an empty graphic frame for it. Say the content is not reachable, and do not invent what the diagram said.
- **Font embedding or theme-level fidelity.** python-pptx writes font *names* into run properties; it cannot embed a font, cannot inspect the fonts a deck depends on, and cannot tell you what any renderer will substitute. If the requirement is "it must use our corporate font, exactly", only PowerPoint can confirm it.
- **The user needs a rendered image of a slide** (a PNG/PDF of page 3). There is no rasteriser here; offer the `.pptx` itself, or a screenshot of the app's *preview*, clearly labelled as approximate.
- **`Presentation()` fails with `PackageNotFoundError`.** Report which of the three causes you actually verified (missing / not a ZIP / corrupt) instead of repeating the exception text, which conflates them.
