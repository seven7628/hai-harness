#!/usr/bin/env node
// 对话滚动跟随回归：验证「上滑看历史时不被流式输出拽回底部」及其反例。
//
// 覆盖 5 个场景（每条都对应一次真实报障或一次修复引入的回归）：
//   A 不碰滚动 → 流式期间必须持续贴底（gap≈0，不显示回底按钮）
//   B 上滑 360px → 流式继续时必须原地不动（曾有 bug：每帧被钉回底部）
//   C 上滑后自己滚回底部 → 必须恢复跟随
//   D 上滑后点「回到最新」→ 回底并恢复跟随
//   E 上滑后切会话 → 新会话必须自动贴底
//      （曾有 bug：切会话清空 blocks 使容器塌缩、scrollTop 被钳到 0，
//        这个「变小」被误判成用户上滑，跟随被永久关闭）
//
// 前置：需要一个 vite dev server（本脚本默认 localhost:5173）。
//   cd desktop/app && npm run dev:vite
// 然后：node e2e/scroll_follow_check.mjs
//
// 退出码 0=全通过，1=有场景失败。

import { chromium } from 'playwright'

const URL = process.env.SCROLL_CHECK_URL || 'http://localhost:5173/'
const SEND_TEXT = '输出一段较长的内容用于测试滚动跟随'
let failed = 0
const ok = (c, msg) => { console.log((c ? '  ✓ ' : '  ✗ ') + msg); if (!c) failed++ }

const state = (p) => p.evaluate(() => {
  const n = document.querySelector('.narrative')
  if (!n) return null
  return {
    top: Math.round(n.scrollTop),
    max: Math.round(n.scrollHeight - n.clientHeight),
    jump: !!document.querySelector('.narrative-jump')
  }
})

async function newSession(b) {
  const p = await b.newPage({ viewport: { width: 1280, height: 800 } })
  await p.goto(URL, { waitUntil: 'domcontentloaded', timeout: 60000 })
  await p.waitForSelector('.narrative', { timeout: 30000 })
  await p.waitForTimeout(1000)
  try { await p.getByText('新建会话', { exact: false }).first().click({ timeout: 5000 }) } catch {}
  await p.waitForTimeout(700)
  const ta = await p.$('textarea')
  if (!ta) throw new Error('找不到输入框')
  await ta.click()
  await ta.type(SEND_TEXT)
  await p.keyboard.press('Enter')
  // 等流式内容超过一屏（否则没有滚动可测）
  for (let i = 0; i < 25; i++) {
    const s = await state(p)
    if (s && s.max > 500) break
    await p.waitForTimeout(300)
  }
  return p
}

// 轮询到条件成立（回底可能跨一两帧）；仍不成立则返回最后一次观测值
async function pollUntil(p, fn, ms = 2000) {
  const end = Date.now() + ms
  let s = await state(p)
  while (Date.now() < end) {
    if (fn(s)) return s
    await p.waitForTimeout(60)
    s = await state(p)
  }
  return s
}

async function scrollUp(p, n = 4) {
  await p.hover('.narrative')
  for (let i = 0; i < n; i++) { await p.mouse.wheel(0, -120); await p.waitForTimeout(60) }
}

const b = await chromium.launch()
try {
  {
    console.log('A 不碰滚动 → 应持续贴底')
    const p = await newSession(b)
    let worstGap = 0, sawJump = false
    for (let i = 0; i < 8; i++) {
      await p.waitForTimeout(400)
      const s = await state(p)
      worstGap = Math.max(worstGap, s.max - s.top)
      if (s.jump) sawJump = true
    }
    ok(worstGap < 60, `流式期间最大 gap=${worstGap}px（应 <60）`)
    ok(!sawJump, '不应显示「回到最新」按钮')
    await p.close()
  }
  {
    console.log('B 上滑 → 流式继续时不应被拽回')
    const p = await newSession(b)
    await scrollUp(p)
    const before = await state(p)
    await p.waitForTimeout(2500)
    const after = await state(p)
    const drift = after.top - before.top
    ok(before.jump, '上滑后应显示「回到最新」按钮')
    ok(after.max >= before.max, `等待期间内容仍在增长（${before.max}→${after.max}px）`)
    ok(drift < 150, `位置漂移 ${drift}px（应 <150）`)
    await p.close()
  }
  {
    console.log('C 上滑后自己滚回底部 → 应恢复跟随')
    const p = await newSession(b)
    await scrollUp(p)
    for (let i = 0; i < 12; i++) { await p.mouse.wheel(0, 200); await p.waitForTimeout(60) }
    const s = await pollUntil(p, (x) => x.max - x.top < 60)
    ok(s.max - s.top < 60, `恢复后 gap=${s.max - s.top}px（应 <60）`)
    await p.close()
  }
  {
    console.log('D 点「回到最新」→ 回底并跟随')
    const p = await newSession(b)
    await scrollUp(p)
    await p.click('.narrative-jump')
    const s = await pollUntil(p, (x) => x.max - x.top < 60)
    ok(s.max - s.top < 60, `回底后 gap=${s.max - s.top}px（应 <60）`)
    await p.close()
  }
  {
    console.log('E 上滑后切会话 → 新会话应自动贴底')
    const p = await newSession(b)
    await scrollUp(p)
    try { await p.getByText('新建会话', { exact: false }).first().click({ timeout: 5000 }) } catch {}
    await p.waitForTimeout(900)
    const ta = await p.$('textarea')
    await ta.click()
    await ta.type('第二个会话内容')
    await p.keyboard.press('Enter')
    await p.waitForTimeout(2500)
    const s = await state(p)
    ok(s.max - s.top < 60, `新会话 gap=${s.max - s.top}px（应 <60）`)
    await p.close()
  }
} finally {
  await b.close()
}
console.log(failed ? `\n✗ ${failed} 个场景失败` : '\n✓ 全部通过')
process.exit(failed ? 1 : 0)
