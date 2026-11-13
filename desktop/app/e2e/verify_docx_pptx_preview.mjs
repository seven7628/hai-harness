#!/usr/bin/env node
// docx / pptx 应用内预览端到端回归（2026-09，D4/D5）。
//
// 全链实测：**真 docx/pptx → 真 bridge → 受管 Python（mammoth / python-pptx）转 HTML
//          → 真 Chromium 渲染**。与 e2e:xlsx 同构，共用同一份驱动方式。
//
// 与其它测试的分工：
//   - tools/builtin/docx_preview*_test.go / pptx_preview*_test.go：锁 Go 侧脚本组装、
//     闸门、清理与错误文案；
//   - 本脚本是唯一同时覆盖「真 bridge + 真转换器 + 真浏览器」的一环 —— 单测能证明
//     HTML 里有 <h1>，但证明不了「浏览器把中文渲染对了、<script> 没被执行、样式真的命中」。
//
// 依赖（缺任一项即 skip，退出码 0 —— 与 Go 侧慢测试口径一致）：
//   - 受管 Python 运行时已 bootstrap（venv + mammoth + python-docx + python-pptx）
//   - 仓库里有 go（用于构建 bridge）
//
// 用法：cd desktop/app && npm run e2e:office
// 退出码：全通过 0；断言失败非 0；前置缺失 skip 0。
import { chromium } from 'playwright'
import { spawn, execFileSync } from 'child_process'
import readline from 'readline'
import path from 'path'
import os from 'os'
import fs from 'fs'
import { fileURLToPath } from 'url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const REPO = path.resolve(APP, '../..')
const PY = path.join(os.homedir(), '.go-code/runtime/python/venv/bin/python')
const BRIDGE = process.env.OFFICE_E2E_BRIDGE || path.join(APP, 'dist/hai-bridge')

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const skip = (m) => { console.log(`\n⏭  跳过：${m}`); process.exit(0) }

if (!fs.existsSync(PY)) skip(`受管 Python 运行时未就绪（${PY}）—— 先跑 go test ./runtime/ -run TestRealEnsureInstallsRuntime`)

// ─────────────────────────────────────────────────────────────────────────────
// 造样本：中文标题/粗体/项目符号/表格/内嵌图片（docx）；
//        多页、标题、项目符号、嵌套形状、内嵌图片（pptx）。
// 用**受管解释器**造（与生产转换同一份库）。
// ─────────────────────────────────────────────────────────────────────────────
const GEN_DOCX = `
from docx import Document
from docx.shared import Inches, Pt
from PIL import Image
Image.new("RGB", (160, 100), "#c65a48").save("pic.png")

d = Document()
d.add_heading("季度经营回顾", 0)          # Title 样式（mammoth 会转成 <p> + warning）
d.add_heading("业绩", level=1)            # Heading 1 → <h1>
d.add_heading("分区域", level=2)          # Heading 2 → <h2>（验层级不是全塌成 h1）
p = d.add_paragraph("营收同比增长 ")
p.add_run("12%").bold = True              # 粗体 → <strong>
d.add_paragraph("要点一", style="List Bullet")
d.add_paragraph("要点二", style="List Bullet")
t = d.add_table(rows=2, cols=2); t.style = "Table Grid"
t.cell(0, 0).text = "指标"; t.cell(0, 1).text = "数值"
t.cell(1, 0).text = "营收"; t.cell(1, 1).text = "12800"
d.add_paragraph("带图段落：")
d.add_picture("pic.png", width=Inches(1.2))
d.save("report.docx")

# 注入面：单元格/段落里的 HTML 与脚本必须按文本显示、不得被执行
d2 = Document()
d2.add_paragraph("普通段落")
d2.add_paragraph("<script>alert('xss')</script>")
d2.add_paragraph("含 <b>标签</b> 与 & 符号")
tb = d2.add_table(rows=1, cols=1); tb.style = "Table Grid"
tb.cell(0, 0).text = "<img src=x onerror=alert(1)>"
d2.save("inject.docx")
print("GEN_OK")
`

const GEN_PPTX = `
from pptx import Presentation
from pptx.util import Inches, Pt, Emu
from PIL import Image
Image.new("RGB", (200, 120), "#7a9eb8").save("pic.png")

prs = Presentation()
# 第 1 页：标题 + 副标题（用 layout 的 placeholder）
s1 = prs.slides.add_slide(prs.slide_layouts[0])
s1.shapes.title.text = "季度经营回顾"
s1.placeholders[1].text = "2026 Q3 · 业绩与展望"

# 第 2 页：标题 + 文本框 + 项目符号 + 图片（自由形状，测绝对定位）
s2 = prs.slides.add_slide(prs.slide_layouts[5])
s2.shapes.title.text = "关键指标"
tb = s2.shapes.add_textbox(Inches(0.8), Inches(2.0), Inches(5.0), Inches(1.6))
tf = tb.text_frame; tf.text = "营收 12800"
p = tf.add_paragraph(); p.text = "同比增长 12%"
for r in p.runs: r.font.bold = True
s2.shapes.add_picture("pic.png", Inches(6.2), Inches(2.2), width=Inches(2.0))

# 第 3 页：中文 + 字号（验字体大小换算）
s3 = prs.slides.add_slide(prs.slide_layouts[5])
s3.shapes.title.text = "第三页：中文标题"
b = s3.shapes.add_textbox(Inches(1.0), Inches(2.5), Inches(6.0), Inches(1.0))
b.text_frame.text = "正文内容 「引号」 ¥100"
b.text_frame.paragraphs[0].runs[0].font.size = Pt(28)
prs.save("deck.pptx")
print("GEN_OK")
`

// —— 驱动真 bridge（stdio NDJSON）：发一条命令，等它的 command_response ——
function bridgeCall(bin, type, payload, timeoutMs = 180000) {
  return new Promise((resolve, reject) => {
    const child = spawn(bin, [], { env: process.env })
    const rl = readline.createInterface({ input: child.stdout })
    const id = 424243
    let settled = false
    const timer = setTimeout(() => {
      if (settled) return
      settled = true
      child.kill('SIGKILL')
      reject(new Error(`${type} 超时（${timeoutMs}ms）`))
    }, timeoutMs)
    rl.on('line', (line) => {
      let ev
      try { ev = JSON.parse(line) } catch { return }
      if (ev.event_type !== 'command_response' || ev.id !== id) return
      settled = true
      clearTimeout(timer)
      child.kill('SIGKILL')
      ev.ok ? resolve(ev.data ?? {}) : reject(new Error(ev.error || `${type} 失败`))
    })
    child.on('error', (e) => { if (!settled) { settled = true; clearTimeout(timer); reject(e) } })
    child.stdin.write(JSON.stringify({ id, type, payload }) + '\n')
  })
}

async function main() {
  const ws = fs.mkdtempSync(path.join(os.tmpdir(), 'office-e2e-'))
  execFileSync(PY, ['-c', GEN_DOCX], { cwd: ws, stdio: 'inherit' })
  execFileSync(PY, ['-c', GEN_PPTX], { cwd: ws, stdio: 'inherit' })

  try {
    if (fs.existsSync(BRIDGE)) fs.rmSync(BRIDGE)
    execFileSync('go', ['build', '-o', BRIDGE, './desktop/bridge'], { cwd: REPO, stdio: 'inherit' })
  } catch (e) {
    skip(`go build bridge 失败（${e.message}）`)
  }

  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1100, height: 800 } })
  const dialogs = []
  const errs = []
  page.on('dialog', async (d) => { dialogs.push(d.message()); await d.dismiss() })
  page.on('pageerror', (e) => errs.push(e.message))

  // ═════════════════════════════════════════════════════════════════
  console.log('\n[1] docx：真 bridge（mammoth）转换含标题/粗体/列表/表格/图片的文档')
  // ═════════════════════════════════════════════════════════════════
  const docx = await bridgeCall(BRIDGE, 'docx_preview', { workspace: ws, path: 'report.docx' })
  assert(typeof docx.html_path === 'string' && docx.html_path.startsWith('/'), `bridge 返回绝对 HTML 路径：${docx.html_path}`)
  assert(fs.existsSync(docx.html_path), '产物文件真实存在（不是只回了个路径字符串）')
  assert(!docx.html_path.startsWith(ws + path.sep), `产物不落在工作区（${docx.html_path}）`)
  const dRaw = fs.readFileSync(docx.html_path, 'utf8')
  assert(dRaw.includes('<meta charset="utf-8">'), '自带 charset 声明（缺了中文会 mojibake）')
  assert(dRaw.includes('<style>'), '自带 <style>（内嵌浏览器不继承应用样式）')
  assert(/<h1[^>]*>业绩<\/h1>/.test(dRaw), 'Heading 1 → <h1>（语义标题，不是裸段落）')
  assert(/<h2[^>]*>分区域<\/h2>/.test(dRaw), 'Heading 2 → <h2>（层级保留，没全塌成 h1）')
  assert(/<h1[^>]*class="doc-title"[^>]*>季度经营回顾<\/h1>/.test(dRaw),
    'Word 的 Title 样式 → h1.doc-title（转换器有意提升；mammoth 裸输出会落成 <p>）')
  assert(dRaw.includes('<strong'), '粗体映射为 strong')
  assert(dRaw.includes('<ul') && dRaw.includes('<li'), '项目符号映射为 ul/li')
  assert(dRaw.includes('<table'), '表格保留为 table')
  assert(/<img[^>]+src="data:image\//.test(dRaw), '内嵌图片转为 data URL（不依赖外部文件）')

  // ═════════════════════════════════════════════════════════════════
  console.log('\n[2] docx：真 Chromium 渲染（file://，与内嵌浏览器同引擎）')
  // ═════════════════════════════════════════════════════════════════
  await page.goto('file://' + docx.html_path)
  const d = await page.evaluate(() => ({
    charset: document.characterSet,
    bodyText: document.body.innerText,
    h1: [...document.querySelectorAll('h1, h2')].map((e) => e.textContent),
    cells: [...document.querySelectorAll('table tr')].map((tr) => [...tr.children].map((c) => c.textContent).join('|')),
    imgs: [...document.querySelectorAll('img')].map((i) => ({ src: i.src.slice(0, 24), w: i.naturalWidth, h: i.naturalHeight })),
    scriptEls: document.querySelectorAll('script').length,
    bodyBg: getComputedStyle(document.body).backgroundColor,
    titleFont: (() => { const e = document.querySelector('h1.doc-title'); return e ? parseFloat(getComputedStyle(e).fontSize) : 0 })(),
    h1Font: (() => { const e = document.querySelector('h1:not(.doc-title)'); return e ? parseFloat(getComputedStyle(e).fontSize) : 0 })(),
  }))
  assert(d.charset === 'UTF-8', `声明并按 UTF-8 解码：${d.charset}`)
  assert(d.bodyText.includes('业绩') && d.bodyText.includes('季度经营回顾'), '中文标题与正文正确渲染（无 mojibake）')
  assert(d.h1.includes('业绩') && d.h1.includes('分区域'), `h1/h2 层级在渲染里可见：h1=${JSON.stringify(d.h1)}`)
  assert(d.titleFont > d.h1Font, `文档标题字号大于正文标题（外观可区分：doc-title=${d.titleFont}px > h1=${d.h1Font}px）`)
  assert(d.bodyText.includes('12%'), '粗体文字内容存在')
  assert(d.bodyText.includes('要点一') && d.bodyText.includes('要点二'), '列表项都渲染出来')
  assert(d.cells.some((r) => r === '指标|数值') && d.cells.some((r) => r === '营收|12800'), `表格内容正确：${JSON.stringify(d.cells)}`)
  assert(d.imgs.length === 1 && d.imgs[0].w > 0 && d.imgs[0].h > 0, `内嵌图片真的解码出像素（naturalSize=${d.imgs[0]?.w}x${d.imgs[0]?.h}）`)
  assert(d.imgs[0].src.startsWith('data:image/'), '图片走 data URL（产物自包含，无外部依赖）')
  assert(d.scriptEls === 0, `文档里没有被解析出的 script 元素（实际 ${d.scriptEls}）`)

  // ═════════════════════════════════════════════════════════════════
  console.log('\n[3] docx：注入面（HTML/脚本按文本显示，不执行）')
  // ═════════════════════════════════════════════════════════════════
  const inj = await bridgeCall(BRIDGE, 'docx_preview', { workspace: ws, path: 'inject.docx' })
  await page.goto('file://' + inj.html_path)
  const i = await page.evaluate(() => ({
    bodyText: document.body.innerText,
    scriptEls: document.querySelectorAll('script').length,
    boldEls: document.querySelectorAll('b, strong').length,
    imgInCell: document.querySelectorAll('table img').length,
    bodyHtml: document.body.innerHTML,
  }))
  assert(i.bodyText.includes("<script>alert('xss')</script>"), '段落里的 <script> 按**文本**显示（未被执行）')
  assert(i.scriptEls === 0, `页面无 script 元素（实际 ${i.scriptEls}）`)
  assert(i.bodyText.includes('含 <b>标签</b> 与 & 符号'), `HTML 按文本显示且实体还原：${JSON.stringify(i.bodyText.slice(0, 80))}`)
  assert(i.imgInCell === 0, `表格里的 <img onerror> 未被解析成图片元素（实际 ${i.imgInCell}）`)
  assert(dialogs.length === 0, `全程无弹窗（${dialogs.length}）`)

  // ═════════════════════════════════════════════════════════════════
  console.log('\n[4] pptx：真 bridge（python-pptx 近似版式）转换 3 页演示文稿')
  // ═════════════════════════════════════════════════════════════════
  const pptx = await bridgeCall(BRIDGE, 'pptx_preview', { workspace: ws, path: 'deck.pptx' })
  assert(typeof pptx.html_path === 'string' && pptx.html_path.startsWith('/'), `bridge 返回绝对 HTML 路径：${pptx.html_path}`)
  assert(fs.existsSync(pptx.html_path), '产物文件真实存在')
  assert(!pptx.html_path.startsWith(ws + path.sep), '产物不落在工作区')
  const pRaw = fs.readFileSync(pptx.html_path, 'utf8')
  assert(pRaw.includes('<meta charset="utf-8">'), '自带 charset 声明')
  assert(/近似|approximate/i.test(pRaw), '页面**显式标注**这是近似重建（不假装是渲染器输出）')

  // ═════════════════════════════════════════════════════════════════
  console.log('\n[5] pptx：真 Chromium 渲染（页数/文字/绝对定位/图片/data URL）')
  // ═════════════════════════════════════════════════════════════════
  await page.goto('file://' + pptx.html_path)
  const p = await page.evaluate(() => {
    const slides = document.querySelectorAll('[data-slide], section.slide, .slide')
    const first = slides[0]
    // 绝对定位的 shape：拿一个已知有 left/top 的元素看计算样式
    const positioned = [...document.querySelectorAll('[style*="left"], .shape')]
    return {
      charset: document.characterSet,
      slideCount: slides.length,
      bodyText: document.body.innerText,
      firstAspect: first ? (first.getBoundingClientRect().width / first.getBoundingClientRect().height) : null,
      positioned: positioned.length,
      posStyle: positioned[0] ? getComputedStyle(positioned[0]).position : null,
      imgs: [...document.querySelectorAll('img')].map((i) => ({ w: i.naturalWidth, src: i.src.slice(0, 24) })),
      scriptEls: document.querySelectorAll('script').length,
      boldCount: document.querySelectorAll('b, strong, [style*="font-weight: bold"], [style*="font-weight:700"]').length,
    }
  })
  assert(p.charset === 'UTF-8', `声明并按 UTF-8 解码：${p.charset}`)
  assert(p.slideCount === 3, `3 页都渲染出来（实际 ${p.slideCount}）`)
  assert(p.bodyText.includes('季度经营回顾') && p.bodyText.includes('关键指标'), '第 1/2 页标题文字正确')
  assert(p.bodyText.includes('第三页：中文标题') && p.bodyText.includes('正文内容'), '第 3 页中文正确（无 mojibake）')
  assert(p.bodyText.includes('营收 12800') || p.bodyText.includes('营收'), '文本框内容渲染')
  assert(p.bodyText.includes('¥100') || p.bodyText.includes('100'), '特殊字符（¥/引号）正确')
  assert(p.slideCount === 3 && p.firstAspect && p.firstAspect > 1.2 && p.firstAspect < 1.5,
    `第 1 页宽高比符合 10:7.5（实测 ${p.firstAspect?.toFixed(3)}）`)
  assert(p.positioned > 0 && p.posStyle === 'absolute', `形状用绝对定位版式（${p.positioned} 个，position=${p.posStyle}）`)
  assert(p.imgs.length >= 1 && p.imgs.some((x) => x.w > 0), `内嵌图片解码出像素（${JSON.stringify(p.imgs.map((x) => x.w))}）`)
  assert(p.imgs.every((x) => x.src.startsWith('data:image/')), '图片走 data URL（自包含）')
  assert(p.boldCount >= 1, `粗体生效（${p.boldCount} 处）`)
  assert(p.scriptEls === 0, `页面无 script 元素（实际 ${p.scriptEls}）`)

  // ═════════════════════════════════════════════════════════════════
  console.log('\n[6] 错误路径：不存在 / 目录 / 越界 / 伪造 / 错格式，都明确报错且不静默成功')
  // ═════════════════════════════════════════════════════════════════
  fs.writeFileSync(path.join(ws, 'fake.docx'), 'not a zip at all')
  // 把 pptx 塞给 docx 转换器（真 zip，但不是 Word 文档）—— 应报错而不是产出空页面
  fs.copyFileSync(path.join(ws, 'deck.pptx'), path.join(ws, 'wrong.docx'))
  const cases = [
    ['docx_preview', 'missing.docx', 'docx 文件不存在'],
    ['docx_preview', '.', 'docx 目录'],
    ['docx_preview', '../evil.docx', 'docx 越界路径'],
    ['docx_preview', 'fake.docx', 'docx 伪造文件'],
    ['docx_preview', 'wrong.docx', 'docx 收到 pptx（错格式）'],
    ['pptx_preview', 'missing.pptx', 'pptx 文件不存在'],
    ['pptx_preview', '.', 'pptx 目录'],
    ['pptx_preview', '../evil.pptx', 'pptx 越界路径'],
  ]
  for (const [cmd, p, why] of cases) {
    let msg = ''
    try {
      await bridgeCall(BRIDGE, cmd, { workspace: ws, path: p })
    } catch (e) {
      msg = e.message
    }
    assert(msg !== '', `${why} → 报错而不是静默成功（${JSON.stringify(msg.slice(0, 70))}）`)
  }

  assert(errs.length === 0, `页面无脚本错误（${errs.length ? errs.join(' | ') : 'clean'}）`)
  await browser.close()
  try { fs.rmSync(ws, { recursive: true, force: true }) } catch { /* 忽略 */ }

  console.log(`\n✅ docx/pptx 预览端到端：${pass} 项全部通过`)
}

main().catch(async (e) => {
  console.error(`\n❌ ${e.message}`)
  process.exit(1)
})
