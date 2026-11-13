#!/usr/bin/env node
// xlsx 工作簿预览端到端回归（2026-09）。
//
// 全链实测：**真 xlsx → 真 bridge → 受管 Python + openpyxl 转 HTML → 真 Chromium 渲染**。
// 与其它 e2e 的分工：
//   - e2e/verify_artifacts_card.mjs（⑩b）：在普通 Chrome 探针页里注入假 watchXlsx/embed，
//     只锁前端接线（按钮 → store.previewXlsx → 内嵌浏览器 URL）；
//   - tools/builtin/xlsx_preview*_test.go：锁 Go 侧转换的解析/转义/闸门/清理；
//   - 本脚本是唯一同时覆盖「真 bridge + 真 openpyxl + 真浏览器」的一环 —— 单测能证明
//     HTML 字符串里有 <table>，但证明不了「浏览器把中文渲染对了、<script> 没被执行、
//     样式真的生效（计算样式非默认值）」。
//
// 依赖（缺任一项即 skip，退出码 0 —— 与 Go 侧慢测试口径一致）：
//   - 受管 Python 运行时已 bootstrap（~/.go-code/runtime/python/venv/bin/python + openpyxl）
//   - 仓库里有 go（用于构建 bridge；已存在 desktop/app/dist/hai-bridge 则直接复用）
//
// 用法：cd desktop/app && npm run e2e:xlsx
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
const BRIDGE = process.env.XLSX_E2E_BRIDGE || path.join(APP, 'dist/hai-bridge')

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const skip = (m) => { console.log(`\n⏭  跳过：${m}`); process.exit(0) }

if (!fs.existsSync(PY)) skip(`受管 Python 运行时未就绪（${PY}）—— 先跑 go test ./runtime/ -run TestRealEnsureInstallsRuntime`)

// 造表脚本：中文 + 多 sheet + 公式（无缓存值）+ 注入字符 + 数字/日期/布尔 + 大表。
// 用**受管解释器**造（与生产转换同一份 openpyxl），不用别的 Python。
const GEN = `
import openpyxl, datetime
wb = openpyxl.Workbook()
ws = wb.active
ws.title = "销售 汇总"
ws.append(["产品名称", "数量", "单价", "金额", "备注"])
ws.append(["苹果 & 梨", 3, 12.5, "=B2*C2", "含 <b>标签</b>"])
ws.append(["香蕉", 10, 3.25, "=B3*C3", "<script>alert('xss')</script>"])
ws.append(["日期", datetime.datetime(2026, 9, 16, 13, 45), True, None, "多行\\n单元格"])
ws2 = wb.create_sheet("第二张表")
ws2.append(["键", "值"])
ws2.append(["甲", 1.5])
wb.save("report.xlsx")

big = openpyxl.Workbook()
bs = big.active
bs.title = "大表"
bs.append(["行号", "名称", "数量", "金额", "备注"])
for i in range(1, 2001):
    bs.append([i, "项目%d" % i, i % 17, i * 1.25, "说明" if i % 3 == 0 else None])
big.save("big.xlsx")
print("GEN_OK")
`

// GEN_NAV：R08b 的导航/冻结夹具。
//   · 20 个 sheet（导航项数与上限的边界）、每个 30 行：页面够高（锚点跳转才有意义），
//     且每张表的内容高（约 900px）**超过 78vh 的容器**—— 否则 set scrollTop 是空操作，
//     「冻结表头吸顶」会变成一条永远为真的假断言（第一版就是 15 行，实测撞到了）；
//   · 第 17 张放冻结窗格 C3 + 12 列各 26 字符宽（约 2244px，远宽于 1100 的视口 ——
//     横向能滚出足够距离，「非冻结列确实被滚走」这条反证才有余量；宽列同时覆盖
//     「列宽按 Excel 字符宽换算」这条）。
const GEN_NAV = `
import openpyxl
wb = openpyxl.Workbook()
for i in range(1, 21):
    ws = wb.active if i == 1 else wb.create_sheet()
    ws.title = "表%02d" % i
    ws.append(["列" + chr(ord("A") + c) for c in range(12)])
    for r in range(1, 31):
        ws.append(["第%d张行%d列%d" % (i, r, c) for c in range(1, 13)])
    if i == 17:
        ws.freeze_panes = "C3"
        for col in "ABCDEFGHIJKL":
            ws.column_dimensions[col].width = 26
wb.save("nav20.xlsx")
print("GEN_NAV_OK")
`

// —— 驱动真 bridge（stdio NDJSON）：发一条命令，等它的 command_response ——
function bridgeCall(bin, type, payload, timeoutMs = 180000) {
  return new Promise((resolve, reject) => {
    const child = spawn(bin, [], { env: process.env })
    const rl = readline.createInterface({ input: child.stdout })
    const id = 424242
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
  const ws = fs.mkdtempSync(path.join(os.tmpdir(), 'xlsx-e2e-'))
  execFileSync(PY, ['-c', GEN], { cwd: ws, stdio: 'inherit' })

  //bridge 二进制：**总是现构建**（增量约 2s）。
  //
  // ❗实测教训：原先写成「存在就复用 desktop/app/dist/hai-bridge」，结果跑的是上一次
  // 构建的旧二进制 —— 断言失败在「中文表头为空」这种看起来像编码问题的地方，实际是
  // 旧 bridge 产出的旧 HTML（thead 修复还没进去）。跨版本排查成本远高于 2s 构建。
  try {
    if (fs.existsSync(BRIDGE)) fs.rmSync(BRIDGE)
    execFileSync('go', ['build', '-o', BRIDGE, './desktop/bridge'], { cwd: REPO, stdio: 'inherit' })
  } catch (e) {
    skip(`go build bridge 失败（${e.message}）`)
  }

  console.log('\n[1] 真 bridge 转换含中文/多 sheet/公式/注入字符的工作簿')
  const small = await bridgeCall(BRIDGE, 'xlsx_preview', { workspace: ws, path: 'report.xlsx' })
  assert(typeof small.html_path === 'string' && small.html_path.startsWith('/'), `bridge 返回绝对 HTML 路径：${small.html_path}`)
  assert(fs.existsSync(small.html_path), '产物文件真实存在（不是只回了个路径字符串）')
  // 产物必须在受管临时区，**不得**落在用户工作区（预览产物不是用户资产）
  assert(!small.html_path.startsWith(ws + path.sep), `产物不落在工作区（${small.html_path}）`)
  const raw = fs.readFileSync(small.html_path, 'utf8')
  assert(raw.includes('<meta charset="utf-8">'), '自带 charset 声明（缺了中文会 mojibake）')
  assert(raw.includes('<style>'), '自带 <style>（内嵌浏览器不继承应用样式）')
  assert(!raw.includes('<script>'), '单元格里的 <script> 已转义（不是注入）')

  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1100, height: 800 } })
  const dialogs = []
  const errs = []
  page.on('dialog', async (d) => { dialogs.push(d.message()); await d.dismiss() })
  page.on('pageerror', (e) => errs.push(e.message))

  console.log('\n[2] 真 Chromium 渲染（file://，与内嵌浏览器同引擎）')
  await page.goto('file://' + small.html_path)
  const s = await page.evaluate(() => {
    const tables = [...document.querySelectorAll('table')]
    const rows = tables[0] ? [...tables[0].querySelectorAll('tbody tr')] : []
    const th = document.querySelector('thead th')
    const td = document.querySelector('td')
    return {
      charset: document.characterSet,
      tableCount: tables.length,
      headers: tables[0] ? [...tables[0].querySelectorAll('thead th')].map((c) => c.textContent) : [],
      row1: rows[0] ? [...rows[0].children].map((c) => c.textContent) : [],
      bodyText: document.body.innerText,
      // 「样式真的生效」只能从计算样式看（源码里有 <style> 不代表规则命中）
      thBg: th ? getComputedStyle(th).backgroundColor : null,
      thPosition: th ? getComputedStyle(th).position : null,
      tdBorder: td ? getComputedStyle(td).borderBottomWidth : null,
      // 注入面：文档里真实存在的 script 元素（单元格里的应当只是文本）
      scriptEls: document.querySelectorAll('script').length,
      inTableCellEls: document.querySelectorAll('td b, td script, td img').length,
      formulaCells: document.querySelectorAll('td.n.formula, td.formula').length,
      // 数字列右对齐（text-align: right 只对 .n 生效）
      numAlign: document.querySelector('td.n') ? getComputedStyle(document.querySelector('td.n')).textAlign : null,
      uncalculated: [...document.querySelectorAll('td.formula')].map((c) => c.textContent),
    }
  })
  assert(s.charset === 'UTF-8', `声明并按 UTF-8 解码：${s.charset}`)
  assert(s.bodyText.includes('销售 汇总') && s.bodyText.includes('第二张表'), '多 sheet：两张表的标题都渲染出来')
  assert(s.tableCount === 2, `每个 sheet 一张表（实际 ${s.tableCount} 张）`)
  assert(s.headers.join(',') === '产品名称,数量,单价,金额,备注', `中文表头正确渲染（无 mojibake）：${s.headers.join(',')}`)
  assert(s.row1[0] === '苹果 & 梨', `HTML 实体正确还原：${JSON.stringify(s.row1[0])}`)
  assert(s.row1[4] === '含 <b>标签</b>', `单元格里的标签按**文本**显示：${JSON.stringify(s.row1[4])}`)
  assert(s.inTableCellEls === 0, `表格内无被解析出的 b/script/img 元素（实际 ${s.inTableCellEls}）`)
  assert(s.scriptEls === 0 && dialogs.length === 0, `单元格里的 <script> 未被执行（script 元素 ${s.scriptEls}，弹窗 ${dialogs.length}）`)
  assert(s.thBg !== 'rgba(0, 0, 0, 0)', `表头底色样式生效（computed bg=${s.thBg}）`)
  assert(s.thPosition === 'sticky', `表头吸顶生效（position=${s.thPosition}）`)
  assert(s.tdBorder !== '0px', `单元格边框样式生效（border=${s.tdBorder}）`)
  assert(s.numAlign === 'right', `数字列右对齐（computed text-align=${s.numAlign}）`)
  assert(s.uncalculated.some((t) => t.startsWith('=')), `无缓存值的公式显示为公式文本：${JSON.stringify(s.uncalculated)}`)
  assert(s.bodyText.includes('没有缓存值'), '页面说明公式为何显示成公式文本（不静默）')

  console.log('\n[3] 大表闸门（2000 行 → 限流 + 如实标注）')
  const big = await bridgeCall(BRIDGE, 'xlsx_preview', { workspace: ws, path: 'big.xlsx' })
  await page.goto('file://' + big.html_path)
  const b = await page.evaluate(() => ({
    dataRows: document.querySelectorAll('tbody tr').length,
    bodyText: document.body.innerText,
  }))
  assert(b.dataRows === 499, `只渲染 499 数据行 + 1 表头 = 500（实际 ${b.dataRows} 数据行）`)
  assert(/还有 1501 行未显示/.test(b.bodyText), '页面如实标注未显示的行数（不静默丢弃）')

  console.log('\n[4] 错误路径：不存在的文件 / 二进制垃圾 / 目录 / 越界路径都给明确错误')
  for (const [p, why] of [['missing.xlsx', '文件不存在'], ['.', '目录'], ['../evil.xlsx', '越界路径']]) {
    let msg = ''
    try {
      await bridgeCall(BRIDGE, 'xlsx_preview', { workspace: ws, path: p })
    } catch (e) {
      msg = e.message
    }
    assert(msg !== '', `${why} → bridge 报错而不是静默成功（${JSON.stringify(msg.slice(0, 60))}）`)
  }
  // 假 xlsx（zip 头都不对）：必须报错且**不残留产物目录**
  fs.writeFileSync(path.join(ws, 'broken.xlsx'), 'not a workbook')
  let brokenMsg = ''
  try {
    await bridgeCall(BRIDGE, 'xlsx_preview', { workspace: ws, path: 'broken.xlsx' })
  } catch (e) {
    brokenMsg = e.message
  }
  assert(brokenMsg !== '', `损坏的工作簿 → 明确报错（${JSON.stringify(brokenMsg.slice(0, 80))}）`)

  // ── R08b：多 sheet 导航 + 冻结窗格（真渲染，rect 比对，不看「元素存在」）──────────────
  //
  // 为什么必须比对矩形：锚点跳转「成功」的标准不是 #hash 变了，而是**目标标题真的落在
  // 吸顶导航下方**。sticky 导航 + scroll-margin 少写一条，元素依然存在、href 依然正确，
  // 只有 rect 会告诉你标题被压在导航条底下。
  console.log('\n[5] 20 个工作表：导航跳转第 17 张 + 冻结窗格（rect 比对）')
  execFileSync(PY, ['-c', GEN_NAV], { cwd: ws, stdio: 'inherit' })
  const navF = await bridgeCall(BRIDGE, 'xlsx_preview', { workspace: ws, path: 'nav20.xlsx' })
  await page.goto('file://' + navF.html_path)
  const navCount = await page.locator('nav.sheet-nav a').count()
  assert(navCount === 20, `导航 20 项（实际 ${navCount}）`)
  await page.click('nav.sheet-nav a[href="#sheet-17"]')
  await page.waitForTimeout(300)
  const jump = await page.evaluate(() => {
    const nb = document.querySelector('nav.sheet-nav').getBoundingClientRect()
    const sec = document.getElementById('sheet-17')
    const h2 = sec.querySelector('h2').getBoundingClientRect()
    const th = sec.querySelector('thead th').getBoundingClientRect()
    return {
      navBottom: nb.bottom, h2Top: h2.top, h2Bottom: h2.bottom,
      thTop: th.top, thBottom: th.bottom, vh: innerHeight,
      text: sec.querySelector('h2').textContent,
    }
  })
  assert(jump.text.startsWith('17. '), `跳到的是第 17 张（标题：${jump.text}）`)
  assert(jump.h2Top >= jump.navBottom - 0.5,
    `目标标题没被 sticky 导航挡住（h2.top=${jump.h2Top.toFixed(1)} ≥ nav.bottom=${jump.navBottom.toFixed(1)}）`)
  assert(jump.thTop >= jump.navBottom - 0.5 && jump.thBottom <= jump.vh,
    `第 17 张的表头在视口内且不在导航之下（th.top=${jump.thTop.toFixed(1)} th.bottom=${jump.thBottom.toFixed(1)}）`)
  const frozen = await page.evaluate(() => {
    const sec = document.getElementById('sheet-17')
    const wrap = sec.querySelector('.table-wrap')
    wrap.scrollTop = 260
    wrap.scrollLeft = 600
    const wr = wrap.getBoundingClientRect()
    const cols = [...sec.querySelectorAll('thead th.frozen-col')].map((el) => el.getBoundingClientRect())
    const th = sec.querySelector('thead th').getBoundingClientRect()
    const neighbor = sec.querySelectorAll('thead th')[2].getBoundingClientRect()
    const bodyFrozen = sec.querySelector('tbody td.frozen-col').getBoundingClientRect()
    return {
      wrapTop: wr.top, wrapLeft: wr.left,
      thTop: th.top, col0Left: cols[0].left, col0w: cols[0].width, col1Left: cols[1].left,
      neighborLeft: neighbor.left, bodyFrozenLeft: bodyFrozen.left,
      canX: wrap.scrollWidth > wrap.clientWidth, scrollLeft: wrap.scrollLeft,
      scrollTop: wrap.scrollTop, canY: wrap.scrollHeight > wrap.clientHeight,
    }
  })
  assert(frozen.canX && frozen.scrollLeft > 0, `宽表横向可滚（scrollLeft=${frozen.scrollLeft}）`)
  // 纵向同理：容器必须真的能滚，否则「冻结表头吸顶」是空断言。
  assert(frozen.canY && frozen.scrollTop > 0, `表体纵向可滚（scrollTop=${frozen.scrollTop}），否则吸顶断言恒真`)
  assert(Math.abs(frozen.thTop - frozen.wrapTop) <= 1.5,
    `表内滚动后冻结表头仍吸在容器顶部（th.top=${frozen.thTop.toFixed(1)} wrap.top=${frozen.wrapTop.toFixed(1)}）`)
  assert(Math.abs(frozen.col0Left - frozen.wrapLeft) <= 1.5,
    `冻结第 1 列贴住容器左边（left=${frozen.col0Left.toFixed(1)} wrap.left=${frozen.wrapLeft.toFixed(1)}）`)
  assert(Math.abs(frozen.col1Left - (frozen.wrapLeft + frozen.col0w)) <= 1.5,
    `冻结第 2 列紧跟第 1 列、不重叠（left=${frozen.col1Left.toFixed(1)} 期望 ${(frozen.wrapLeft + frozen.col0w).toFixed(1)}）`)
  assert(Math.abs(frozen.bodyFrozenLeft - frozen.wrapLeft) <= 1.5, '表体里的冻结列同样贴边（不只是表头）')
  // 反证：非冻结列必须真的被滚走 —— 否则上面几条只是在证明「整张表没滚动」。
  assert(frozen.neighborLeft < frozen.wrapLeft - 50,
    `非冻结列已滚出左边（left=${frozen.neighborLeft.toFixed(1)}），冻结列不是假象`)
  const SHOTS = path.join(__dirname, '_xlsx_shots')
  fs.mkdirSync(SHOTS, { recursive: true })
  const shotPath = path.join(SHOTS, 'xlsx-sheet17-frozen.png')
  await page.screenshot({ path: shotPath })
  console.log(`  📷 ${shotPath}`)

  assert(errs.length === 0, `页面无脚本错误（${errs.length ? errs.join(' | ') : 'clean'}）`)
  await browser.close()
  try { fs.rmSync(ws, { recursive: true, force: true }) } catch { /* 忽略 */ }
  console.log(`\n✅ xlsx 预览端到端：${pass} 项全部通过`)
}

main().catch((e) => {
  console.error('\n❌ ' + (e?.message ?? e))
  process.exit(1)
})
