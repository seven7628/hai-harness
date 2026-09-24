---
name: pdf
version: 1.0.0
description: Read and inspect text from PDF files with pypdf — detecting scanned/image-only pages, CID-font and Unicode corruption in CJK text, encrypted files, and extracting pages selectively. Use this when the user asks to extract, summarise, search or convert the text of a PDF, or to inspect a PDF's pages, metadata or structure.
---

# PDF files (pypdf 6.19.0)

## How to run this

- Do all PDF work through `run_python`. The working directory **is the workspace root**: write deliverables as workspace-relative paths (`out/summary.txt`) so the user can find them. Under the default sandbox policy the workspace, `$HOME` and `/tmp` are writable and `~/.go-code` is denied; credential files under `$HOME` (`~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.netrc`, `~/.kube`) are **not** covered by any sandbox rule, so never read or copy them. Keep deliverables in the workspace and use `/tmp` for scratch files only.
- The first `run_python` call of a session can take minutes while the managed runtime bootstraps (tens of MB on disk — ≈70 MB measured — once). Pass `timeout` (default 120 s, max 3600) for large documents.
- Declare your deliverables: pass `outputs=["out/summary.txt"]` on the same `run_python` call. The desktop app uses these paths for the artifact card, the file list and the preview tab (it opens a new tab, or refreshes the one already showing that file) — a file you don't declare stays invisible to the user unless they go looking for it on disk.
- **Never `pip install`.** Installed here: `pypdf`, `Pillow`, `python-docx`, `openpyxl`, `xlsxwriter`, `lxml`, **`pypdfium2`** (page rendering: `PdfDocument` → `page.render(scale=…)` → `to_pil()`; it **renders, it does not OCR** — an image of a scan still has no text layer). **Not** installed: `pdfplumber`, `PyMuPDF`/`fitz`, `pytesseract`, `reportlab`, `fpdf`, `pandas`. There is **no PDF-generation library** in this runtime — plan accordingly.
- **OCR, precisely.** No Python OCR wrapper (`pytesseract` is not installed) and the traineddata set here is only `eng`, `osd`, `snum` — **`chi_sim` is missing**. The machine does have the **system** `tesseract 5.5.2` (`/opt/homebrew/bin/tesseract`), which gives three different boundaries:
  - **Latin/digit scans are recoverable.** Render the page with `pypdfium2` (Rule 1), then `tesseract page.png stdout -l eng`. Capture the output as bytes or pass `errors="replace"` — tesseract's stderr is not always valid UTF-8.
  - **Inside `run_python`** the same call works in principle: `subprocess.run(["tesseract", "page.png", "stdout", "-l", "eng"], capture_output=True)` (the sandbox allows `process-exec`). This was **not exercised end-to-end in the app this round** — if it fails, fall back to rendering the page and reading the PNG with `read_file`.
  - **Chinese scans still cannot be read here.** `-l chi_sim` fails ("Could not initialize tesseract") because the language data is absent. Say that plainly; never guess the content of a Chinese scan.
- **Do the cheap inspection first, on one page, before extracting everything.** `PdfReader(path)` is metadata only (page tree); `extract_text()` is the expensive part. Verified on a 4-page report: constructing the reader took 1.3 ms, extracting all four pages took 30 ms — that gap grows linearly with page count.

```python
from pypdf import PdfReader
r = PdfReader("doc.pdf")
print(len(r.pages), r.metadata)              # cheap
print(len(r.pages[0].extract_text() or ""))  # cheap-ish, and tells you a lot
```

## Rule 1 — pypdf extracts *text layers*, not pages

`extract_text()` returns the text stored in the PDF's content streams. It cannot see letters that are part of an image. A scanned document therefore yields **`''` — with no exception and no warning.** Verified on a PIL-generated image-only PDF:

```python
r.pages[0].extract_text()   # ''
```

This silent empty string is the most common way to get a confidently wrong answer: it looks exactly like "the page is blank". So **always distinguish "no text" from "no text layer"** before reporting anything:

```python
page = r.pages[0]
text = page.extract_text() or ""
if not text.strip():
    if len(page.images) > 0:
        print("image-only page -> likely a SCAN; no text layer to extract")
    else:
        print("genuinely blank page")
```

Verified on the image-only PDF: `page.images` → 1 image (`/XObject` present with key `/image`). But **only look at `images` when `text` came out empty**: a page with a text layer can still contain images (charts, logos). The 4-page Skia report in Rule 2 has text on every page *and* `len(page.images)` = 1, 2, 1, 1 — so images alone are not a scan signal.

**When it is a scan, say so — and say what is actually possible here.** Report "this PDF is a scanned/image-only document; it has no text layer." Then use the escalation below if the script is Latin/digit (`tesseract -l eng`), and state plainly that **a Chinese scan cannot be OCR'd on this machine** (no `chi_sim` data). Do **not** summarise, guess, or infer the contents from the filename, page count, or a screenshot-like mental model. A plausible-looking summary of a document you could not read is worse than an honest refusal — the user cannot tell it was invented. If the scan is mixed (some pages text, some images), report per page which is which.

> **One honest escalation is available** (added 2026-09): you can **render** a page to an image with `pypdfium2` (installed; see the runtime list above) — `pdfium.PdfDocument(path)[i].render(scale=…).to_pil().save("page.png")` — and then **look at that PNG with `read_file`**. If your model has vision, that lets you *read the scan directly*, which is a genuinely better answer than refusing. For a Latin/digit scan you can additionally run the system `tesseract -l eng` on that PNG (see "OCR, precisely" above).
>
> Note carefully what this is and is not: it is **not OCR** (no text layer is produced, and no OCR language data beyond `eng/osd/snum` is installed — you cannot grep the result). It is *you looking at a picture*. So: render at roughly `scale = 1568 / max(page_width_pt, page_height_pt)` (keeps the long edge at the downsampling threshold `read_file` uses, so you get the pixels you asked for), read a page or a few pages at a time — **at most 5 pages per batch** — rather than the whole document, call `page.close()` / `doc.close()` when you are done, and state plainly that the content was read from a rendered image (so the user can tell it apart from extracted text, and knows OCR-grade errors are possible). If your model has no vision, do not pretend — say the scan cannot be read here.

Related: a page that contains both a text layer and images is normal (e.g. a chart with a caption). Check whether *text* came out before treating the presence of images as fatal.

## Rule 2 — CJK PDFs may extract *readable but corrupted* text

This is subtler and more dangerous than a scan, because the output **looks correct to a human reader while being wrong to a program**. Verified on a real Chinese-language report produced by Headless Chrome / Skia (`Producer: Skia/PDF m128`), 4 pages:

- **Kangxi Radicals were substituted for ordinary ideographs.** 26 distinct codepoints in `U+2F00–U+2FDF` appeared where the document visually shows ordinary characters, because those codepoints are visually identical:

  | extracted | codepoint | visually identical to | normal codepoint |
  |---|---|---|---|
  | 用 | `U+2F64` KANGXI RADICAL USE | 用 | `U+7528` |
  | ⼀ | `U+2F00` KANGXI RADICAL ONE | 一 | `U+4E00` |
  | ⽂ | `U+2F42` KANGXI RADICAL SCRIPT | 文 | `U+6587` |

- The practical effect is that **plain string search silently fails**:

  ```python
  "调用量" in extracted   # False   <- the user can see this text on the page
  "收入" in extracted     # False
  "对比" in extracted     # False
  ```

  while the page renders perfectly. If you grep the extracted text for a CJK keyword and get no match, do not conclude the word is absent.

- **NUL bytes (`\x00`) appeared mid-word** in 3 places from different underlying causes — a missing space between digits (`年 1\x006 ⽉`), a missing space between words (`AICC\x00Core`), and inside a URL query string (`cid=3892707954\x004779743663`). NUL is **ambiguous**: stripping it joins `AICC` + `Core`, replacing it with a space breaks `1` + `6`. There is no universally correct fix; handle it per site, or report the raw string.

**Mitigation — normalize before you match, and be explicit that you did:**

```python
import unicodedata
text = unicodedata.normalize("NFKC", raw_text)   # Kangxi radicals -> normal ideographs
```

Verified effect on the real report: after NFKC, `调用量`, `收入`, `对比`, `支持` and `文本` all matched. NFKC fixed the character variants but **did not** fix the NUL bytes (still 3, length unchanged).

**When pypdf's output still looks wrong, swap extractors — PDFium reads the same file independently:**

```python
import pypdfium2 as pdfium
doc = pdfium.PdfDocument(path)
text = "".join(doc[i].get_textpage().get_text_range() for i in range(len(doc)))
for i in range(len(doc)):
    doc[i].close()
doc.close()
```

Verified on the same Skia report, this is *better than pypdf + NFKC*: `1141` characters (pypdf: 1157), **Kangxi-radical characters 98 → 0, NUL bytes 3 → 0**, and `调用量` / `收入` / `对比` / `支持` / `文本` all matched without any normalization. Discipline: **when the two libraries disagree, report both results side by side — do not silently pick the nicer one.** They use different character handlers (whitespace, ligatures, ordering) and the user needs to know which extractor produced which string.

So the working recipe for a CJK PDF is: extract → `unicodedata.normalize("NFKC", ...)` → handle `\x00` deliberately → then search/summarise. Two rules on top of that:

- **Check a sample rather than trusting the whole file.** Print the first few hundred characters and sanity-check them against what the user described. If it reads like garbage, stop and report; if it reads correctly, still normalize before matching.
- **Never silently "fix" text you cannot verify.** If a passage looks like mojibake or nonsense, quote it as-is and tell the user it appears corrupted. Do not paraphrase corrupted text into fluent prose — that turns an extraction artifact into a fabricated fact.
- Correct, well-made PDFs are unaffected: an ASCII PDF with a standard `WinAnsiEncoding`/Helvetica font needed no normalization, and NFKC is a no-op on plain ASCII. The corruption above is a property of how the *producing* tool embedded the font, not of pypdf.

## Rule 3 — page count is cheap, full text is not

Do not dump an entire long PDF into the context. Establish the size and shape first, then extract only what is needed:

```python
r = PdfReader("report.pdf")
print("pages:", len(r.pages))                      # cheap
print("meta:", dict(r.metadata or {}))             # cheap; /Title, /Producer, /CreationDate
print("outline:", len(r.outline or []))            # cheap; section structure if present
for i in range(min(3, len(r.pages))):              # first pages only
    t = r.pages[i].extract_text() or ""
    print(f"--- page {i+1} ({len(t)} chars) ---")
    print(t[:800])
```

Then decide: search pages individually (`for i, p in enumerate(r.pages)`), collect only matching pages, and keep a per-page character count so you can spot pages that are blank or image-only before you waste effort on them. `r.metadata` and `r.outline` are often enough to answer "what is this document?" without extracting any body text.

For a very large PDF, extracting every page's text into one string and printing it is both slow and context-destroying. Work page by page and report a bounded excerpt.

**Budgets:** if the document is **> 50 pages or > 10 MB**, process it in batches (e.g. ~20 pages per batch into `/tmp`, then read back what you need) instead of holding everything in one string; render **at most 5 pages** per pass; and close everything you open — `page.close()` / `doc.close()` for `pypdfium2`. Memory and context both blow up otherwise.

## Rule 4 — encryption is detected, not bypassed

```python
r = PdfReader("locked.pdf")
r.is_encrypted                    # True
r.decrypt("pw123")                # non-zero = success; the value varies with the matched
                                  # role (1 = user password, 2 = owner password)
r.decrypt("wrong")                # 0  (failure)
```

Verified: before a successful `decrypt()`, *any* page access raises `FileNotDecryptedError: File has not been decrypted` — including `len(r.pages)`. A wrong password makes `decrypt()` return `0`; it does not raise. Check the return value, not just the absence of an exception.

**AES-encrypted files (V4/V5) cannot be opened here.** `cryptography` is not installed, and pypdf's AES backend is a stub that raises `pypdf.errors.DependencyError: cryptography>=3.1 is required for AES algorithm` as soon as a string or stream really has to be decrypted. Measured on a V4/`AESV2` file: the password check returned a non-zero value (that part is RC4), then `r.metadata` raised the `DependencyError` — so **a successful-looking return from `decrypt()` proves nothing about being able to read the file**. Catch `DependencyError` and tell the user this machine cannot decrypt this file: **do not retry, do not `pip install`**, and do not report the password check as success. `decrypt()` handles RC4 (V1/V2) only.

Rules: if `is_encrypted` is true and you have no password, **ask the user for it** — do not attempt to crack it. If the user supplies a password, keep it in the script only, do not write it into output files or the report. The page count is unavailable until decryption succeeds, so "how many pages does this have?" genuinely cannot be answered for a locked file.

## Rule 5 — writing PDFs: you can rearrange, not author

pypdf manipulates existing pages; it cannot create content or render text:

```python
from pypdf import PdfReader, PdfWriter
w = PdfWriter()
w.add_page(PdfReader("in.pdf").pages[0])
w.add_blank_page(width=612, height=792)   # a truly blank page
w.pages[1].rotate(90)
w.write("out.pdf")
```

Verified: page order changes, blank-page insertion, rotation and merging all work, and text remains extractable afterwards. But **there is no way to put new text on a page** — no `reportlab`, no `fpdf`, no `matplotlib`. If the user asks you to *generate* a PDF report from data, or to *edit* the wording in a PDF, that is out of reach here: produce the content as `.docx` (python-docx) or `.xlsx` (openpyxl) instead and say why. Splitting, merging, reordering, rotating, and deleting pages are all fine.

## When to tell the user you cannot do it

- **The PDF has no text layer (a scan).** There is no Python OCR (`pytesseract` missing) and no `chi_sim` language data; the system `tesseract` covers **Latin/digit** scans only (`-l eng`). For a **Chinese** scan, say it cannot be read here. Report it plainly; offer that the user provide a text-based version, or a transcript, or that they run OCR themselves. Never invent the contents.
- **The extracted text is garbled** (mojibake, unreadable CJK, odd replacement characters) and normalization does not recover it: report the raw sample you got and say the encoding cannot be resolved here. Do not paraphrase it into confident prose.
- **The PDF is encrypted and no password is available.** Ask; do not attempt to break it.
- **The PDF is AES-encrypted (V4/V5).** `cryptography` is missing, so pypdf raises `DependencyError: cryptography>=3.1 is required for AES algorithm`. Say the file cannot be decrypted on this machine; do not retry or install anything.
- **The task is PDF *generation* or in-place text editing.** There is no PDF-writing library in this runtime. Propose `.docx`/`.xlsx` output instead.
- **The task needs layout-aware extraction** (tables with cell structure, multi-column reading order, precise coordinates). `pdfplumber` and `PyMuPDF` are not installed, and pypdf's `extract_text()` returns a plain string with no layout model — table structure and column order are **not** preserved, and text order follows the content stream, which is not necessarily visual order. Say that tables/columns may come out jumbled rather than presenting a jumbled table as authoritative.
- **The user wants an image of a page, or embedded images pulled out.** There is no OCR to read them, but there *is* a rasterizer: render the whole page with `pypdfium2` (`doc[i].render(scale=…).to_pil()`) into an image file, or take an embedded image with `page.images[i].data` (`Pillow` can open it from `io.BytesIO`). Both give you pictures, not text.
