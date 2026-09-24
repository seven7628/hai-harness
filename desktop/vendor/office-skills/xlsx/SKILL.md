---
name: xlsx
version: 1.0.0
description: Build, read and edit Excel workbooks (.xlsx/.xlsm) with openpyxl — formulas that have no cached value, destructive data_only reads, merged cells, macro loss, sparse-sheet geometry and CJK text. Use this when the user asks to create, fill in, analyse, extract from or edit a spreadsheet, or when a formula's result has to be visible to a reader other than Excel itself.
---

# Excel workbooks (openpyxl 3.1.5)

## How to run this

- Do all spreadsheet work through `run_python`. The working directory **is the workspace root**: write deliverables as workspace-relative paths (`reports/q3.xlsx`) so the user can find them. Under the default sandbox policy the workspace, `$HOME` and `/tmp` are writable and `~/.go-code` is denied; credential files under `$HOME` (`~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.netrc`, `~/.kube`) are **not** covered by any sandbox rule, so never read or copy them. Keep deliverables in the workspace and use `/tmp` for scratch files only.
- Declare your deliverables: pass `outputs=["reports/q3.xlsx"]` on the same `run_python` call. The desktop app uses these paths for the artifact card, the file list and the preview tab (it opens a new tab, or refreshes the one already showing that file) — a file you don't declare stays invisible to the user unless they go looking for it on disk.
- Create parent directories yourself: `os.makedirs("reports", exist_ok=True)`.
- Write to a **new** file when you are producing a deliverable. Overwriting the user's only copy of a workbook in place is unrecoverable if the transform is wrong. Be aware that *any* openpyxl save rewrites the file (see Rule 2b) — so "just adding one cell" is still a full re-serialisation, not an in-place patch.
- The first `run_python` call of a session can take minutes while the managed runtime bootstraps (tens of MB on disk — ≈70 MB measured — once). Pass `timeout` (default 120 s, max 3600) for large documents.
- **Never `pip install`.** The runtime is pinned. Installed here: `openpyxl`, `xlsxwriter`, `lxml`, `Pillow`. **Not** installed: `pandas`, `numpy`, `xlrd`, `matplotlib` — do not import them, and do not build a plan that needs them.
- You cannot read a `.xlsx` with `read_file` (it is a ZIP). Verify your own output by re-opening it with openpyxl in a second `run_python` call and printing the cells you care about.

## Rule 1 — a formula written by openpyxl has NO cached value

This is the single most important trap. When you write `ws["A3"] = "=SUM(A1:A2)"`, openpyxl stores the formula and an **empty** result:

```xml
<c r="A3"><f>SUM(A1:A2)</f><v></v></c>
```

openpyxl is not a calculation engine. It never evaluates anything, so there is no number to cache. Consequences:

- `load_workbook(path, data_only=True)` returns `None` for that cell — **including for a file your own process just wrote**.
- Excel, WPS and Google Sheets *will* show the right number: openpyxl sets `<calcPr fullCalcOnLoad="1"/>` in `xl/workbook.xml` by default, so a real spreadsheet app recalculates on open.
- But every other reader — previewers, converters, data pipelines, other openpyxl scripts — sees `None`/blank. The file is not "wrong", it is **unresolved**, and only an actual spreadsheet application can resolve it.

So pick one deliberately:

**(a) Write values, not formulas — the default choice.** Compute in Python and store numbers. It is the only option that is correct for every reader, and it never needs Excel:

```python
rows = [("笔记本电脑", 3, 5999.5), ("台式机", 1, 4200.0)]
ws.append(["产品", "数量", "单价", "金额"])
for name, qty, price in rows:
    ws.append([name, qty, price, round(qty * price, 2)])   # compute here
total = sum(r[3] for r in ws.iter_rows(min_row=2, values_only=True))
ws.append(["合计", None, None, round(total, 2)])
```

**(b) Keep the formula and cache its value — use `xlsxwriter`.** If the user genuinely wants a live formula *and* other readers must see a number, xlsxwriter's `write_formula` accepts the cached result:

```python
import xlsxwriter
wb = xlsxwriter.Workbook("out.xlsx")
ws = wb.add_worksheet("Sheet1")
ws.write("A1", 10); ws.write("A2", 20)
ws.write_formula("A3", "=SUM(A1:A2)", None, 30)   # 30 is the cached value
wb.close()
```

That file then reads as `30` under `data_only=True` while still carrying `=SUM(A1:A2)`. Caveat: you must supply the result yourself, so it can go stale — and once a file is written by openpyxl you cannot add a cached value to it later (openpyxl re-emits `<v></v>`). Use xlsxwriter to *write fresh* workbooks; use openpyxl to *edit existing* ones.

**(c) Leave the formula unresolved and say so.** Acceptable only when the user will open the file in Excel/WPS themselves. If you choose this, state it explicitly in your reply: "the totals are live formulas and will compute when opened in Excel; they are not cached, so they read as empty in any automated reader." Do not silently hand over a file whose numbers are blank to half its consumers.

Whichever you choose, finish by re-opening the file and printing the cells you were asked to produce — and assert on the **values** you read back, never by grepping the file's XML (see Rule 8 on CJK: a `grep` for the Chinese text finds nothing, on disk it is `&#…;` entities).

## Rule 2 — saving is destructive in two ways

### 2a — `data_only=True` + `save()` deletes the formulas

`data_only=True` substitutes each formula cell with its cached value. If you then `save()`, the formulas are **permanently replaced by literals** — the formula, and everything downstream that recomputed from it, is gone.

Measured on `=SUM(A1:A2)` after a data_only load + save:

```xml
<!-- before -->  <c r="A3"><f>SUM(A1:A2)</f><v></v></c>
<!-- after  -->  <c r="A3" t="n"><v>30</v></c>      <!-- formula destroyed -->
```

Rules: never `save()` a workbook opened with `data_only=True` back over the source. If you need to convert formulas to their values, do it deliberately, write to a **new** path, and tell the user what was flattened. (For a file openpyxl wrote itself there is nothing to flatten — the cached values are empty, so save-from-data_only silently deletes the formulas and leaves nothing behind.)

### 2b — even a plain `load_workbook` + `save()` discards cached values

This one catches people because **no flag is involved and no edit is required**. openpyxl drops the cached result of every formula it rewrites. Measured on an `xlsxwriter`-produced file with a good cache (`=SUM(A1:A2)` cached as `30`), after a load and immediate save with no modification at all:

```xml
<!-- before -->  <c r="A3"><f>SUM(A1:A2)</f><v>30</v></c>   <!-- data_only reads 30 -->
<!-- after  -->  <c r="A3"><f>SUM(A1:A2)</f><v></v></c>   <!-- data_only reads None -->
```

The formulas survive, `<calcPr fullCalcOnLoad="1"/>` is set so Excel will recalculate on open — but **every non-Excel reader of that file now sees `None` where it previously saw `30`.** If a workbook came from Excel with real cached values and a downstream consumer (a dashboard, a data pipeline, another script) depends on those numbers, an openpyxl edit silently breaks that consumer.

So: when you edit a workbook that other tools read, check whether it has meaningful cached formula values *before* you save, and warn the user that an openpyxl round trip un-caches them. If the consumers matter and the values can be recomputed, consider writing the recomputed numbers as values (Rule 1a) so the result does not depend on a cache at all.

## Rule 3 — one `load_workbook`, one view

There is no call that returns formulas and values together. Load twice:

```python
formulas = load_workbook("in.xlsx")                    # .value -> '=SUM(A1:A2)'
values   = load_workbook("in.xlsx", data_only=True)    # .value -> 30, or None if uncached
```

Distinguish the two cases you'll hit: `None` from a data_only read means **either** "the formula was never calculated" (normal for openpyxl-written files) **or** "the formula's inputs were blank". Check the formula text from the first load before concluding the data is missing.

Use the formula view to understand structure (`.data_type == "f"` marks a formula cell); use the value view for numbers. A plain `load_workbook` + `save` round trip **preserves the formulas** but discards their cached values — see Rule 2b before you rely on that.

## Rule 4 — merged cells: only the top-left anchor is writable

```python
ws.merge_cells("A1:C1")
ws["A1"] = "anchor"        # OK
ws["B1"] = "nope"          # AttributeError: 'MergedCell' object attribute 'value' is read-only
```

`ws["B1"]` is a `MergedCell`, not a `Cell`; the `AttributeError` only surfaces at assignment time, so a loop that writes into every cell can die halfway through a file that is already partially written. Actions:

- Write only the anchor.
- To change an existing merged block, `ws.unmerge_cells("A1:C1")` first, edit, then re-merge.
- Inspect before writing: `for rng in ws.merged_cells.ranges: print(rng)` (also available after a reload). Skip the covered (non-anchor) cells, or write any value only to `rng.min_row`/`rng.min_col`.
- Reading is fine — the covered cells read as `None`, not as repeated values.

Same repetition trap on the *docx* side; see that skill.

## Rule 5 — `.xlsm`: macros are dropped unless you ask for them

`load_workbook("budget.xlsm")` followed by `save("budget.xlsm")` **deletes `xl/vbaProject.bin`**. openpyxl warns you about nothing. Verified: after such a round trip the archive contains no `vba`-named member at all, and the file is a macro-free workbook wearing an `.xlsm` extension.

```python
wb = load_workbook("budget.xlsm", keep_vba=True)   # required for macro-bearing files
...
wb.save("budget_out.xlsm")
```

With `keep_vba=True` the VBA is preserved byte-for-byte. Rules: always pass it when the source is `.xlsm`; write to a new filename so the original macro-enabled workbook survives; and remember `keep_vba` preserves the *existing* project only — openpyxl cannot create or edit VBA. **Do not tell the user that changing the extension removed the macros.** Verified: saving with `keep_vba=True` to a `.xlsx` path still writes `xl/vbaProject.bin` unchanged (openpyxl copies every part of the original VBA archive over — `openpyxl/writer/excel.py:96-110`), so the file keeps macro bytes and a macro-enabled main content type under a non-macro extension. Whether Excel/WPS accepts that odd combination is **unverified here** — so assert nothing about it either way; when the user wants a macro workbook, keep the `.xlsm` extension and say the extension is what makes it a macro workbook.

## Rule 6 — cross-sheet references: quote any sheet name with a space

Sheet names containing a space must be quoted with single quotes in a reference:

```python
ws["B5"] = "='Assumptions Inputs'!$A$1"    # correct
ws["C5"] = "=Assumptions Inputs!$A$1"      # wrong
```

openpyxl does **not** validate formula syntax — the unquoted version is accepted, stored, and re-read unchanged, so nothing fails until Excel opens it and shows `#VALUE!`. There is no safe way to notice from Python; get the quoting right as you write, especially when the name came from `wb.sheetnames` and you are interpolating it:

```python
def q(name):                      # Excel-safe sheet reference
    return "'" + name.replace("'", "''") + "'"
ws["B5"] = f"={q('Assumptions Inputs')}!$A$1"
```

CJK sheet names without spaces (`=销售!A1`) round-trip fine unquoted, but quoting is always legal — prefer `q()` unconditionally when building references programmatically.

## Rule 7 — sparse sheets: iterate by bounds, not by rectangle

`max_row`/`max_col` describe the **bounding box** of everything you ever touched, not the populated cells. A sheet with `A1` and `Z100` set reports `max_row=100, max_col=26` and `iter_rows()` materialises **2600** cell objects for 2 real values. Verified: `len(ws._cells)` is 2 before iteration and 2600 after — iterating the full rectangle allocates all of it, and `read_only=True` also walks the rectangle (it streams, it does not skip).

Practical rules:

- Read by coordinate when you know where the data is: `ws["Z100"].value`. This is exact and cheap.
- Iterate with bounds derived from the data, and use `values_only=True` to avoid building `Cell` objects: `ws.iter_rows(min_row=1, max_row=20, min_col=1, max_col=8, values_only=True)`.
- Filter as you go — `[c for c in row if c.value is not None]` — rather than assuming a dense grid.
- Before you iterate anything, `sorted(ws._cells)` lists only the genuinely populated coordinates (2 entries for the example above) — the cheapest real-cell probe. It is a private attribute and it **stops being accurate as soon as you iterate**, because iteration fills `_cells` with the whole rectangle. Use it once, up front, for a quick probe; not as the basis of a loop.
- `Worksheet` has `min_row` but **no `min_col`** — `AttributeError` if you reach for it. Use `ws.min_column`/`ws.max_column`, or `ws.calculate_dimension()`.
- `read_only=True` + `write_only=True` exist for very large files. `write_only` workbooks can only `append()` rows and cannot be re-read in the same process without saving and reloading.
- Geometry caveat: a single stray value at row 5000 makes every naive "read all rows" loop 5000 iterations long. When a sheet looks suspiciously large, check `ws.dimensions` first and look for a far-away cell before writing a loop that trusts `max_row`.

## Rule 8 — formats, widths and CJK

- **`number_format` is display only.** `ws["A1"] = 1234567.891` with `number_format = '#,##0.00'` still stores `1234567.891`; a reader that ignores formats sees the raw value. Round in Python if the *value* must be rounded.
- **Type fidelity: a whole float silently becomes an `int`.** `ws["A1"] = 4200.0` is serialised as the text `4200` — the XML really is `<c r="A1" t="n"><v>4200</v></c>` — and `load_workbook` hands back `int` (verified). Your `isinstance(x, float)` tests, `x/2` results and any downstream consumer that expects decimal places can change behaviour across a round trip. If the type matters, apply `float(x)` explicitly after reading. And when you write the verification pass, do **not** assert floating-point values with `==`: after a round trip `4200.0 == 4200` is `True` for the wrong reason, and genuine fractions need a tolerance or `round(x, n)` comparison.
- **Dates: openpyxl handles the format for you, and that cuts both ways.** Verified: writing a real `datetime.date(2026,9,16)` stores serial `46281` **and automatically applies `yyyy-mm-dd`** — even if you never set `number_format` — so it reads back as a `datetime` correctly.
  The trap is the *other* direction: a **bare number** whose format `openpyxl.styles.numbers.is_date_format()` classifies as a date is silently reinterpreted. `ws["A1"] = 45000` with `number_format = 'yyyy-mm-dd'` comes back from `load_workbook` as `datetime.datetime(2023, 3, 15, 0, 0)`, not as `45000`. Verified (openpyxl 3.1.5): the same `45000` under `'0.00'`, under `'0.00%'`, under `'General'`, or with no format at all comes back as `int`; only `'mm-dd-yy'`, `'yyyy-mm-dd'` and `'h:mm:ss'` convert. **`int` vs `float` is decided by the stored text and nothing else**: on write openpyxl serialises a number with `"%.16g"` (`openpyxl/compat/strings.py:18`, reached from `openpyxl/cell/_writer.py:84`) and on read `_cast_number` (`openpyxl/worksheet/_reader.py:80-84`) returns `float` only when the text contains `.`/`e`/`E`, else `int` — so an integral value always reads back as `int`, regardless of `number_format`. If you store a plain count/ID/total and a date-ish format ends up on that column, your type changes on read and arithmetic/`str()` on it will break or look wrong (`2023-03-15 00:00:00` instead of `45000`). Check a suspicious format with `is_date_format(fmt)` and keep date formats off numeric columns.
- **Column width is in characters**, calibrated on the `0` glyph: ~2 units per CJK character. A width that fits 12 ASCII characters will clip ~6 CJK characters. `ws.column_dimensions["A"].width = 18` comfortably fits ~8 CJK characters. Always set widths for CJK-label columns, otherwise the text is cut off in Excel — the value is present but invisible, which users report as "the file is broken".
- **CJK text itself is safe — but it is stored as an inline string, not in `sharedStrings.xml`.** Verified round trip of `产品名称` / `销售数据` / full-width punctuation / emoji: identical in and out, sheet titles included. No encoding configuration is needed; do not add `encoding=` arguments and do not escape non-ASCII. Mechanism (measured on openpyxl 3.1.5): openpyxl **does not generate `xl/sharedStrings.xml` at all** — the text goes into `xl/worksheets/sheetN.xml` as an inline string whose non-ASCII characters are written as numeric character references: `<c r="A2" t="inlineStr"><is><t>&#31508;&#35760;…</t></is></c>`. Consequence for verification: **never validate your output by grepping for the literal Chinese** (or for `sharedStrings.xml`) — the bytes on disk are `&#…;` entities, so `grep 产品名称` over the sheet XML returns 0 hits and you will wrongly conclude "the Chinese was lost". Re-open the file with `load_workbook` and assert the value instead.
- If a user needs CJK-friendly output for a tool that only eats CSV, `csv` is in the stdlib and `utf-8-sig` writes the BOM Excel expects — but only reach for CSV when the user actually asked for CSV, since it drops all formatting and multi-sheet structure.

## Rule 9 — sheet naming and workbook geometry

- Invalid characters in a title raise `ValueError`: `/ \ ? * [ ] :`.
- Titles over 31 characters only emit a `UserWarning` and are otherwise kept — the file may then be unreadable by strict consumers. Keep titles ≤31 chars yourself.
- Duplicate titles are **silently renamed** (`报告` twice → `报告`, `报告1`). Do not rely on your name surviving; check `wb.sheetnames` afterwards if the name matters.
- A workbook always keeps at least one sheet, but you *can* remove the last visible one: `wb.remove(wb.active)` succeeds and leaves `wb.sheetnames == []`, after which `wb.active` is `None` and `.title` raises `AttributeError`. Set `wb.active = 0` (or create a sheet) before saving.
- Rows and columns are **1-based**. `ws.cell(row=0, column=0)` raises `ValueError: Row or column values must be at least 1`. This is a frequent off-by-one when porting index-based code.
- openpyxl reads `.xlsx`/`.xlsm`/`.xltx`/`.xltm` only. Legacy `.xls` raises `InvalidFileException` — the managed runtime has no `xlrd`, so a `.xls` input honestly cannot be read. Say so rather than producing an empty workbook.

## When to tell the user you cannot do it

- **The input is `.xls`** (legacy binary) and there is no converter available locally: report it, do not fabricate a table.
- **The user expects computed formula results in an automated pipeline and needs live formulas** — this machine has no calculation engine (no LibreOffice, no Excel), so pick (a) or (b) above and explain the trade-off. Do not hand back an unresolved-formula workbook while implying the numbers are readable everywhere. Note the same gap applies in reverse: you cannot *resolve* an existing workbook's stale cached values either, so a file whose cache is out of date will keep reporting the old numbers to a data_only reader.
- **The requested output needs data analysis or rendered plots.** openpyxl *can* write charts and conditional formatting itself (verified: `BarChart` + `CellIsRule` both round-trip), so build simple charts with `openpyxl.chart`. What is genuinely missing is `pandas` (no DataFrame/groupby/merge) and `matplotlib` (no rendered images). So: aggregation is fine in plain Python, simple native charts are fine, but anything phrased as "use pandas" or "plot this as an image and embed it" is out of reach — say which specific capability is missing.
- **Pivot tables.** openpyxl preserves pivot definitions it reads, but there is no high-level API to author one. The low-level pieces do exist (`ws.add_pivot` plus a hand-built `openpyxl.pivot.table.TableDefinition` / pivot cache — verified present in 3.1.5), yet assembling a working cache by hand is not something to promise inside a task: say that a real pivot table is out of reach, and offer ordinary formulas/values (`SUMIF`, plain aggregates) as a summary instead.
- **The workbook is very large** (hundreds of MB / millions of cells): say so and propose processing it in streaming mode or a subset of sheets instead of pretending a full load fits in memory.
- If `load_workbook` raises `InvalidFileException` or `zipfile.BadZipFile`, the file is not what its extension claims. A third failure mode looks like a bug in your own code but is not: a container that *is* a valid zip but is not a workbook at all (no `[Content_Types].xml`) raises `KeyError("There is no item named '[Content_Types].xml' in the archive")` (verified) — that means the path points at some other zip, not at a corrupted workbook. Report the actual error in each case; do not retry with a different flag.
