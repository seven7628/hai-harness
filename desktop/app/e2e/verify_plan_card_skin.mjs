#!/usr/bin/env node
// 计划卡（todo_add / todo_update）在两套皮肤下的几何回归（2026-09-20）
//
// 起因（用户报障）：V3 皮肤下计划卡「太贴边」—— 头部（列表图标 / 详情 / 耗时）顶在卡边框上。
// 根因：V3 皮肤把 .plan-hd 的 padding 归零（行语言），但 `.plan-card` 的卡壳（边框 + 底色 +
// 圆角）没销 —— 于是头部被卡边框夹住；而 V3 的设计语言里计划本来就「不是卡」
// （docs/design-research/07-component-spec.md §9.3：AI Elements 的 Plan 是带边框的 Card，
//  与「一屏只有一个边框」冲突，只借它的信息结构）。移植漏项见同文档 §16.5 的同类复盘。
//
// 本脚本用真实 Narrative 组件 + 真实 index.css / narrative-skin.css 渲染一张
// 「展开的 live 计划卡 + 失败态计划卡」，在 current / v3 两种皮肤下逐条断言：
//   ① current：卡壳仍在（默认皮肤零影响）—— 边框 1px / 底色非透明 / 圆角 8px
//   ② v3：卡壳销掉 —— 边框 0 / 底色透明 / 圆角 0（「展开是清单，不是卡」）
//   ③ v3：头部首/末子元素与消息列内容盒齐平（不再被卡边框夹住 = 用户报的「贴边」）
//   ④ v3：清单挂在左竖线上（线 = 内容左 + 8px），文字不贴右边、不被裁切
//   ⑤ v3：「详情」原始参数区 与 失败文本走展开体语言（左竖线 + 12px，来自卡内盒子的 26/31px 缩进退役）
//   ⑥ 皮肤只改视觉：两种皮肤下计划卡数量与 .plan-item 数量一致
//
// 用法：cd desktop/app && node e2e/verify_plan_card_skin.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4191)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

async function waitPort(port, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { clearTimeout(timer); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 临时探针页：挂真实 Narrative + 两份真 CSS（跑完删除）
const PROBE_DIR = path.join(__dirname, '_planskinprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh"><head><meta charset="utf-8"><title>plan skin probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import Narrative from '../../src/components/Narrative'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'
import '../../src/narrative-skin.css'

const q = new URLSearchParams(location.search)
document.documentElement.setAttribute('data-skin', q.get('skin') === 'v3' ? 'v3' : 'current')
document.documentElement.setAttribute('data-theme', q.get('theme') || 'ink')

const TODOS = [
  { id: 't1', title: '调研：上下文统计（分区面板 + ctx 环 + 缓存命中率）的前后端数据链路与口径', done: true },
  { id: 't2', title: '定位 Anthropic usage/缓存 token 口径问题：后端 Harness 层 vs 前端计算', done: true },
  { id: 't3', title: 'B. Anthropic tokens/s：映射 thinking_tokens→Reasoning + 「思考摘要化」信号，前端分母改整轮', done: true },
  { id: 't4', title: 'C. Anthropic Input 归一化：形状判定，避免第三方端点缓存重复计数', done: false },
  { id: 't5', title: 'G. 验证与交付：go 测试/前端 typecheck+lint、文档口径说明、清理', done: false },
]

useAppStore.setState({
  persona: 'code',
  activeSessionId: 'probe',
  todos: TODOS,
  blocks: [
    { kind: 'user', id: 1, text: '排查一下上下文统计的口径问题' },
    { kind: 'tool', id: 2, toolId: 'call_todo_1', name: 'todo_update',
      args: JSON.stringify({ id: 't3', done: true }), result: '{"ok":true}',
      status: 'done', durMs: 1, startedAt: Date.now() - 1000, todos: TODOS },
    // 失败态计划卡（最后一张待办卡 = live，默认展开 → .plan-err 可见）
    { kind: 'tool', id: 3, toolId: 'call_todo_2', name: 'todo_update',
      args: JSON.stringify({ id: 't9', done: true }), result: 'todo_update: unknown id "t9"',
      isError: true, status: 'done', durMs: 2, startedAt: Date.now() - 500, todos: TODOS },
  ],
})

createRoot(document.getElementById('root')!).render(
  <div style={{ height: '100vh', display: 'flex', flexDirection: 'column' }}><Narrative /></div>,
)
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_planskinprobe_run/index.html`
// stdio: 'ignore'（vite 的输出没人读，管道背压会把脚本挂住）；清理时先 TERM 再 KILL
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'ignore' })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)
// 看门狗：浏览器/网络异常时也要带非零码退出（否则 CI 里表现为挂死）
const watchdog = setTimeout(() => {
  console.error('\n❌ 超时（120s）：vite / chromium 未按预期完成')
  cleanup()
  process.exit(1)
}, 120_000)
watchdog.unref()

// 量测：卡壳 / 头部与消息列的对齐 / 清单竖线 / 详情与失败文本的展开体语言
const probe = () => {
  const R = (el) => el.getBoundingClientRect()
  const nar = document.querySelector('.narrative')
  const ncs = getComputedStyle(nar)
  const contentLeft = R(nar).left + parseFloat(ncs.paddingLeft)
  const contentRight = R(nar).right - parseFloat(ncs.paddingRight)
  const cards = [...document.querySelectorAll('.plan-card')]
  const card0 = cards.find((c) => !c.classList.contains('error')) || cards[0]
  const ccs = getComputedStyle(card0)
  const hd = card0.querySelector('.plan-hd')
  const vis = [...hd.children].filter((el) => !el.classList.contains('grow'))
  const list = card0.querySelector('.plan-list')
  const items = [...card0.querySelectorAll('.plan-item')].map((li) => {
    const txt = li.querySelector('.plan-txt')
    return {
      clipped: txt.scrollWidth > txt.clientWidth + 1,
      roomRight: Math.round(contentRight - R(txt).right),
      txtFromLine: Math.round(R(txt).left - R(list).left),
    }
  })
  const box = (el) => {
    if (!el) return null
    const cs = getComputedStyle(el)
    return {
      fromContentLeft: Math.round(R(el).left - contentLeft),
      borderLeft: parseFloat(cs.borderLeftWidth),
      borderTop: parseFloat(cs.borderTopWidth),
      paddingLeft: parseFloat(cs.paddingLeft),
    }
  }
  return {
    skin: document.documentElement.getAttribute('data-skin'),
    cards: cards.length,
    items: card0.querySelectorAll('.plan-item').length,
    shell: {
      border: parseFloat(ccs.borderTopWidth),
      radius: parseFloat(ccs.borderTopLeftRadius),
      bg: ccs.backgroundColor,
    },
    hdFirstFromContentLeft: Math.round(R(vis[0]).left - contentLeft),
    hdLastFromContentRight: Math.round(contentRight - R(vis[vis.length - 1]).right),
    hdFirstFromCardLeft: Math.round(R(vis[0]).left - R(card0).left),
    listLineFromContentLeft: list ? Math.round(R(list).left - contentLeft) : null,
    items0: items,
    raw: box(card0.querySelector('.plan-raw')),
    err: box(document.querySelector('.plan-card.error .plan-err')),
  }
}

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true })
  const page = await browser.newPage({ viewport: { width: 860, height: 900 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => { if (m.type() === 'error') errs.push(m.text().slice(0, 200)) })

  // —— current（默认皮肤）：卡壳在、头部被卡内边距托住 ——
  await page.goto(`${PROBE_URL}?skin=current&theme=ink`)
  await page.waitForSelector('.plan-card')
  await page.locator('.plan-card').first().locator('.plan-hd').click() // 非 live 卡默认折叠 → 点开
  await page.locator('.plan-card').first().locator('.plan-raw-toggle').click() // 「详情」露出 .plan-raw
  await page.waitForTimeout(150)
  const cur = await page.evaluate(probe)
  console.log('\n[current]', JSON.stringify(cur))
  assert(cur.shell.border === 1 && cur.shell.radius === 8 && cur.shell.bg !== 'rgba(0, 0, 0, 0)',
    'current 皮肤：计划卡仍是带边框/底色的卡（默认皮肤零影响）')
  assert(cur.hdFirstFromCardLeft > 5, `current 皮肤：头部被卡内边距托住（离卡左边 ${cur.hdFirstFromCardLeft}px > 5）`)
  assert(cur.raw && cur.raw.borderTop === 1, 'current 皮肤：「详情」区仍是卡内盒子（虚线分隔线在）')

  // —— v3（组件样式）：不是卡；头部与消息列齐平；清单挂左竖线；展开体走竖线语言 ——
  await page.goto(`${PROBE_URL}?skin=v3&theme=ink`)
  await page.waitForSelector('.plan-card')
  await page.locator('.plan-card').first().locator('.plan-hd').click()
  await page.locator('.plan-card').first().locator('.plan-raw-toggle').click()
  await page.waitForTimeout(150)
  const v3 = await page.evaluate(probe)
  console.log('\n[v3]', JSON.stringify(v3))
  assert(v3.skin === 'v3', 'v3 皮肤：data-skin 真的生效（不是兜底 current）')
  assert(v3.shell.border === 0 && v3.shell.radius === 0 && v3.shell.bg === 'rgba(0, 0, 0, 0)',
    'v3 皮肤：计划卡壳销掉（无边框 / 无底色 / 无圆角 —— 「展开是清单，不是卡」）')
  assert(Math.abs(v3.hdFirstFromContentLeft) <= 1,
    `v3 皮肤：头部首元素与消息列内容盒齐平（偏 ${v3.hdFirstFromContentLeft}px）`)
  assert(Math.abs(v3.hdLastFromContentRight) <= 1,
    `v3 皮肤：头部末元素（详情/耗时）与消息列右边界齐平（偏 ${v3.hdLastFromContentRight}px）—— 即用户报的「贴边」不再出现`)
  assert(v3.listLineFromContentLeft === 8, `v3 皮肤：清单左竖线在内容左 + 8px（实测 ${v3.listLineFromContentLeft}px）`)
  assert(v3.items0.every((i) => i.txtFromLine === 51), 'v3 皮肤：清单文字挂在竖线 + 51px（圆点 + 8px 间距不变）')
  assert(v3.items0.every((i) => !i.clipped && i.roomRight > 0),
    `v3 皮肤：清单文字不贴右边、不被裁切（最紧一行余 ${Math.min(...v3.items0.map((i) => i.roomRight))}px）`)
  assert(v3.raw && v3.raw.fromContentLeft === 8 && v3.raw.borderLeft === 1 && v3.raw.borderTop === 0 && v3.raw.paddingLeft === 12,
    'v3 皮肤：「详情」原始参数区走展开体语言（左竖线 + 12px，卡内盒子的 26px 缩进退役）')
  assert(v3.err && v3.err.fromContentLeft === 8 && v3.err.borderLeft === 1 && v3.err.paddingLeft === 12,
    'v3 皮肤：失败文本同款展开体语言（红竖线 + 12px）')

  // —— 皮肤只改视觉：结构计数一致 ——
  assert(cur.cards === v3.cards && cur.items === v3.items,
    `皮肤只改视觉：计划卡数 ${cur.cards}/${v3.cards}、清单项数 ${cur.items}/${v3.items} 一致`)
  assert(errs.length === 0, `两种皮肤控制台 0 报错（实得 ${errs.length}：${errs.slice(0, 3).join(' | ')}）`)

  await browser.close()
  console.log(`\n全部通过（${pass} 项断言）`)
}

main().then(() => process.exit(0)).catch((e) => { console.error(`\n❌ ${e.message}`); process.exit(1) })
