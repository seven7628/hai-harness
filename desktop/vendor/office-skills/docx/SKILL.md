---
name: docx
version: 1.0.0
description: Create, read and edit Word documents (.docx) with python-docx — heading/outline structure, body-order traversal, tables vs paragraphs, run splitting and CJK font selection. Use this when the user asks for a Word document (report, letter, contract, memo), or asks to extract headings, text, tables or structure out of an existing .docx.
---

# Word documents (python-docx 1.2.0)

## How to run this

- Do all document work through `run_python`. The working directory **is the workspace root**: write deliverables as workspace-relative paths (`reports/draft.docx`) so the user can find them. Under the default sandbox policy the workspace, `$HOME` and `/tmp` are writable and `~/.go-code` is denied; credential files under `$HOME` (`~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.netrc`, `~/.kube`) are **not** covered by any sandbox rule, so never read or copy them. Keep deliverables in the workspace and use `/tmp` for scratch files only.
- Create parent directories yourself: `os.makedirs("reports", exist_ok=True)`.
- Declare your deliverables: pass `outputs=["reports/draft.docx"]` on the same `run_python` call. The desktop app uses these paths for the artifact card, the file list and the preview tab (it opens a new tab, or refreshes the one already showing that file) — a file you don't declare stays invisible to the user unless they go looking for it on disk.
- Prefer writing a **new** file over overwriting the user's source document; an in-place transform that goes wrong is unrecoverable.
- The first `run_python` call of a session can take minutes while the managed runtime bootstraps (tens of MB on disk — ≈70 MB measured — once). Pass `timeout` (default 120 s, max 3600) for large documents.
- **Never `pip install`.** Installed here: `python-docx`, `lxml`, `Pillow`, `openpyxl`, `xlsxwriter`, `pypdf`, `pypdfium2`, `mammoth`. **Not** installed: `pandas`, `numpy`, `docx2pdf`, `reportlab`. Do not import them.
- A `.docx` is a ZIP — `read_file` shows you binary noise, not text. Verify your output by re-opening it with python-docx and printing the paragraphs/tables you produced.
- Start from a real document (`Document("input.docx")`) when the user wants to modify an existing one. Only use a blank `Document()` when creating something from scratch; a blank document has no house style, header or footer, and the user will get something that looks nothing like their template. Headers and footers are still reachable on a blank document — and they are **not** part of `doc.paragraphs`:

  ```python
  sec = doc.sections[0]
  sec.header.paragraphs[0].text = "页眉文字"   # a blank doc still has header/footer paragraphs
  sec.footer.paragraphs[0].text = "第 1 页"
  print(sec.header.is_linked_to_previous)      # True = inherit from previous section, False = own
  ```

  Verified: the header/footer text survives save + reload, and it never appears in `doc.paragraphs` (so Rule 1/Rule 2 "read the document" excludes it).

## Rule 1 — read structure from `paragraph.style.name`

> **Alternative worth knowing when the goal is *reading* rather than structure** (added 2026-09): `mammoth` is installed (`mammoth.convert_to_html(open(path,"rb")).value`) and maps semantics to HTML — `Heading` → `<h1>/<h2>`, `List Bullet` → `<ul><li>`, tables → `<table>`, and embedded images → `data:` URLs. It is a **one-call** way to get the document's shape, and it is what the app's own docx preview uses. Two caveats: it is a *lossy render*, not an editing API (never write back through it), and Word's `Title` style needs care. A **bare** `mammoth.convert_to_html(...)` call renders `Title` as a plain `<p>` plus a warning (verified: `<p>季度经营回顾</p>` + `Unrecognised paragraph style: Title (Style ID: Title)`), whereas **the app's own docx preview pipeline promotes `Title` to `<h1 class="doc-title">`** through an explicit style map (`desktop/app/e2e/verify_docx_pptx_preview.mjs:167`; map at `tools/builtin/docx_preview.go:252`). So do not tell the user "Title is lost" — say which pipeline you mean: in the app's preview it renders as the document title, in a bare mammoth call it does not. For precise paragraph/run/table control, use python-docx as below.

The style name is the reliable signal for document outline, and it is what the user sees in Word's navigation pane:

```python
for p in doc.paragraphs:
    print(p.style.name, "|", p.text)
# Title | 季度销售报告
# Heading 1 | 一、业务进展
# Normal | 正文段落……
# Heading 2 | 1.1 收入构成
```

`Heading 1` … `Heading 9` are the outline levels; `Normal` is body text; `Title` is the document title. Use this to build a table of contents, split a document into sections, or verify you generated the structure you intended.

**`add_heading(text, level)` uses level `0` for `Title`, not `Heading 1`.** Verified mapping:

| call | `style.name` |
|---|---|
| `add_heading("x", 0)` | `Title` |
| `add_heading("x", 1)` | `Heading 1` |
| `add_heading("x", 2)` | `Heading 2` |

So `add_heading("报告", 0)` produces a title, not a first-level heading. Off-by-one here makes the whole outline shift one level.

Also note that a paragraph's outline position can come from direct formatting rather than a style, in documents produced by other tools. If `style.name` is uninformative across the whole file (everything `Normal`), check the raw XML (`p._p.xml`) for an `outlineLvl` before assuming the document has no headings.

## Rule 2 — paragraphs and tables are two separate lists, and neither is document order

`document.paragraphs` contains only top-level paragraphs. **Tables are invisible to it**, and `document.tables` is a separate list with no positional relationship to the prose around it.

```python
len(doc.paragraphs)   # 5  -> tables are NOT in here
len(doc.tables)       # 1  -> separately
```

So "the paragraph after the table" is not obtainable from either list. To preserve reading order (which is what a human means by "the document"), walk the body XML:

```python
from docx.oxml.ns import qn
from docx.table import Table
from docx.text.paragraph import Paragraph

def walk_body(container, doc):
    for child in container.iterchildren():
        tag = child.tag.split("}")[-1]
        if tag == "p":
            p = Paragraph(child, doc)
            print("PARA", repr(p.style.name), repr(p.text))
        elif tag == "tbl":
            t = Table(child, doc)
            print("TABLE", [[c.text for c in row.cells] for row in t.rows])
        elif tag == "sdt":
            # content control: its paragraphs/tables sit inside w:sdtContent and are
            # invisible to doc.paragraphs / doc.tables -- descend or lose the block
            content = child.find(qn("w:sdtContent"))
            if content is not None:
                walk_body(content, doc)
        # other tags exist and must be tolerated — see below

walk_body(doc.element.body, doc)
```

Verified on a document built as *title, H1, body, H2, table, body*: this traversal returns exactly that order, while `paragraphs` silently omits the table.

Important: **the body contains elements other than `p` and `tbl`.** A trailing `sectPr` (section properties) is always present, and `sdt`/`bookmarkStart` wrappers also occur. Always branch on the tag and ignore what you don't handle — raising on the unexpected tag is how this traversal breaks on real documents. (`Paragraph(child, doc)` works for `p`; table cells contain their own nested paragraphs, which this loop does not descend into — see Rule 4 for nested tables.)

- **`w:sdt` (content control) content is silently invisible to `doc.paragraphs` and `doc.tables`**, which is why the `sdt` branch above is not optional. Verified: after injecting a `w:sdt` holding a paragraph `BBB` and another holding a one-cell table `CCC`, then saving and reopening, `doc.paragraphs == ['AAA']` and `len(doc.tables) == 0` — while `'BBB'` and `'CCC'` are still in `doc.element.body.xml`. The same file through the traversal above returns `AAA`, `BBB`, `TABLE [['CCC']]`.
  **Self-check before you report an extraction:** compare the number of blocks the traversal returned with `len(doc.paragraphs) + len(doc.tables)`. If they differ (verified example: `3` vs `1`), the XML holds blocks your walk never reached — say the extraction is incomplete instead of reporting a short document.
- **`hyperlink` is *not* a body-level element** — it always sits *inside* a `w:p`. Verified: `doc.element.body` children are `['p', 'sectPr']`, while that paragraph's children are `['r', 'hyperlink', 'r']`. So it can never be the top-level tag you branch on, and the split matters when you edit: **`p.text` contains the hyperlink text, `p.runs` does not.** Verified on one paragraph: `p.text == 'para with link: LINKTEXT tail'` but `[r.text for r in p.runs] == ['para with link: ', ' tail']` — a run-only rewrite drops `LINKTEXT` without any error. python-docx 1.2.0 gives you `paragraph.hyperlinks` for this (verified: `[(h.text, h.address) for h in p.hyperlinks] == [('LINKTEXT', 'https://example.com/link')]`).

## Rule 3 — Word splits text across runs, and a naive edit fails *silently*

A paragraph is a list of runs, and the split has **no relationship to the visible text**. Spell-check state, revision marks, pasted text and formatting changes all break one visible sentence into several runs. Verified:

```python
p.text        # 'The quarterly revenue increased by 12% year-over-year.'
[r.text for r in p.runs]
# ['The quarterly revenue ', 'increased', ' by 12% year-over-year.']
"increased by" in p.text                        # True   <- the human-visible phrase
any("increased by" in r.text for r in p.runs)   # False  <- no single run contains it
```

Two consequences that matter more than the split itself:

1. **`p.text` is a concatenation, not stored text.** It is great for reading/searching and useless for editing — there is no setter that works the way you'd hope.
2. **Cross-run search-and-replace silently does nothing.** A loop like `for r in p.runs: if old in r.text: r.text = r.text.replace(old, new)` does not raise and does not change the document when the phrase spans runs. Verified: the text came back byte-identical. Careful though — a hit that falls **inside a single run** *is* replaced (verified), so the same loop succeeds on some paragraphs and fails silently on others. That is worse than always failing: you cannot conclude "this loop doesn't work", and you cannot trust it either. Never report success based on "no exception raised" — always re-read the paragraph and compare.

To edit reliably, either work at paragraph granularity (rebuild the paragraph's runs from scratch), or collapse the runs first:

```python
def merge_runs(par):
    """Collapse all runs of a paragraph into the first one; returns it (or None)."""
    if not par.runs:
        return None
    first = par.runs[0]
    for r in par.runs[1:]:
        first.text += r.text
        r._element.getparent().remove(r._element)
    return first
```

After merging, `par.runs` is a single run and replacement works — but **merging destroys per-run formatting.** Verified: a bold run followed by a plain run collapses into one entirely-bold run. So merge only where formatting is uniform (body text you are replacing wholesale), and never over a paragraph that mixes bold/italic/links/superscripts. When formatting must be preserved, edit only the runs that are entirely inside the target text, or rebuild the run structure explicitly and copy `run.font` / `run.bold` from the original.

**Contract of `merge_runs()`: it only ever touches `par.runs`.** If the paragraph holds a `hyperlink`, the merged run's text still does **not** equal `p.text` — link text is in `p.text` but in no run. Verified: merged `run.text == 'para with link:  tail'` while `p.text == 'para with link:  tailLINKTEXT'`; handle `paragraph.hyperlinks` separately. Fields fail the other way round (the gap is in the text itself, not between runs): a `w:fldSimple`'s cached result is missing from **both** `p.text` and `runs` (verified `p.text == 'page: '`), and a complex field's code (`w:instrText`, e.g. `PAGE`) is in neither — merging neither fixes nor reveals those.

A useful pattern for "find a placeholder and fill it in": locate the paragraph by `p.text` (reliable), then rebuild that one paragraph's content rather than patching runs.

## Rule 4 — tables

- Access cells as `table.rows[i].cells[j]`; `len(table.rows)` / `len(table.columns)` give the grid size.
- **Merged cells repeat.** After merging `row0.cells[0] .. cells[2]`, iterating `row.cells` yields the same text three times — there is no `None` and no marker telling you it is a merge. Deduplicate by identity when you care:

  ```python
  seen, unique = set(), []
  for c in table.rows[0].cells:
      if id(c._tc) not in seen:
          seen.add(id(c._tc)); unique.append(c.text)
  ```

  Verified: a 2×3 table with `A1:C1` merged reports `['merged','merged','merged']` both before and after a save/reload.
- `table.style` may be `None` on tables from other producers. `"Table Grid"` is the style that draws visible borders — a table without it looks like unformatted text in Word, which users read as "the table didn't come out". Set it explicitly when creating tables.
- `cell.text = "x"` replaces the cell's content with a single run. For per-cell formatting, use `cell.paragraphs[0].add_run(...)`.
- **A nested table is reachable only through the cell.** A table inside a cell is not listed in `document.tables` and its text is not in `cell.text` — both look at the outer level only. Verified on a 1×1 table holding a 1×2 table: `len(document.tables) == 1`, `cell.text == 'outer text\n'`, while `len(cell.tables) == 1` gives `[['N1', 'N2']]`. Use `cell.tables` (and recurse) when you walk into cells.

## Rule 5 — CJK text: content is safe, fonts are not automatic

CJK content round-trips perfectly through python-docx — verified with headings, full-width punctuation and `¥` symbols. No encoding setup is needed, and you should not add any.

The real CJK problem is **font selection**. A run with no explicit font, and a document with no theme font, stores *no* font at all:

```python
pp = Document().add_paragraph(); r = pp.add_run("x")
r.font.name                           # None
pp.style.font.name                    # None
Document().styles["Normal"].font.name # None
```

Whatever renders the file then picks a default, and a font with no CJK glyphs produces boxes/tofu or an unexpected fallback. To pin a CJK font you must set **both** the ASCII font and the East-Asian font attribute — setting `r.font.name` alone only covers the Latin run properties:

```python
from docx.oxml.ns import qn

run = paragraph.add_run("中文内容")
run.font.name = "SimSun"
run._element.rPr.rFonts.set(qn("w:eastAsia"), "SimSun")   # <- the part that matters for CJK
```

Do this for every run that contains CJK if the document must render predictably, or set it once on the `Normal` style if the whole document is CJK. Only use fonts you can reasonably expect the user to have (`SimSun`, `SimHei`, `Microsoft YaHei` on Windows; `PingFang SC`, `Songti SC` on macOS). If you specify a font that is absent, Word substitutes something — which is exactly the tofu problem you were trying to avoid, so tell the user which font you pinned and why.

Note that `run._element.rPr` is `None` until the run has run properties. The line above works because `run.font.name = ...` has already created them; set the font name first.

## Rule 6 — do not shell out to `textutil` or other converters

Tempting for "convert this HTML to docx", but measured behaviour on this machine:

- `textutil -convert docx` output is readable by python-docx, but with a UTF-8 source and **no** `<meta charset>`, Chinese came out as mojibake (`报告标题` → `鎶ュ憡鏍囬`). With `<meta charset="utf-8">` in the HTML the text was correct.
- Even when the text survived, **all structure was flattened**: an `<h1>` and a `<table>` became six plain `Normal` paragraphs and **zero** tables — the heading hierarchy and the table were both gone.

So a `textutil` conversion produces a document that looks plausible in a paragraph dump while having lost the structure the user asked for. Verified against `pandoc 3.9` on the same input: `textutil` produced 8 paragraphs, **all** `Normal` (table cells flattened into plain paragraphs) and **0 tables**, while `pandoc in.html -o out.docx` kept `Title` / `Heading 1` / `Heading 2` and produced **1 real table** with the correct cell text. So: `textutil` is the untrustworthy one; if you must go HTML→docx, `pandoc` is the path that survives verification. Still prefer building the document with python-docx — you control styles, tables and fonts, and the result does not depend on the mapping a converter happens to pick.

## When to tell the user you cannot do it

- **`.doc` (legacy binary) input.** python-docx cannot open it. Verified: both an OLE2 `.doc` and a plain `.txt` raise `PackageNotFoundError: Package not found at '...'` — the error means "this is not a ZIP/OOXML package", so do not read it as "file is missing". There is no converter here; say so instead of guessing at the content.
- **`.docx` → PDF.** No LibreOffice, no Word, no `docx2pdf`/`reportlab`, and no LaTeX/Typst/WeasyPrint engine — verified: `pandoc in.html -o out.pdf` fails with `pdflatex not found` (exit 47). `wpscli` 1.1.0 does exist here (`/Applications/wpsoffice.app/Contents/MacOS/wpscli`, `word2pdf`) but it runs inside the WPS app's own sandbox (in testing it could not read `/tmp`), and this skill does not drive GUI apps — so treat this as genuinely impossible on this machine. Offer the `.docx` and let the user export it.
- **Faithful layout reproduction.** python-docx has no layout engine: it cannot paginate, position floating images, compute where a page break falls, or tell you the page count. If the request is "make this fit on two pages" or "match this PDF's layout", that needs Word and you should say so.
- **Content that only exists in the rendered output.** python-docx reads stored text, not rendering. Verified: a field wrapped in `w:fldSimple` (as Word writes a table of contents) yields `p.text == ''` even when a cached result is present inside it — python-docx does not traverse into the field wrapper. Complex fields (`fldChar`/`instrText`) behave similarly, and page numbers are caches that may be absent entirely. So a generated TOC/page-number/cross-reference field reads as empty or absent. Report that the value is not stored rather than inventing it.
- **Tracked changes / comments.** python-docx does not model revisions, and this bites in the direction you would not expect: `paragraph.text` skips **both** `w:del` (deleted) **and** `w:ins` (inserted) content. Verified by injecting one of each — a word inside `w:ins` was absent from `p.text` *and* from `p.runs`, while present in `p._p.xml`. A human opening that document **sees** the inserted word. So if a document carries revision marks, your extracted text can silently omit text that is visibly part of the document; say that the extraction may not match what is displayed rather than treating it as the final wording.
- **Unexpected `KeyError`/`BadZipFile` on open.** The file is encrypted or not a real `.docx`. Report the error; do not retry with random flags.
