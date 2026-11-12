#!/usr/bin/env node
// 定时任务 UI 验证：驱动真实 Electron 应用，检查三个 cron 页面的布局规范。
// 数据来源：HAI_DATA_DIR 隔离数据根（多实例模式）——不再使用真实 HOME/userData。
//   历史事故：旧版注释说“仅验证渲染，不修改数据”，但它用真实 HOME + 真实 userData 启动，
//   并直接覆写 localStorage['go-code.projects'] 指向仓库路径 → 用户的真实工作区配置被改掉；
//   同时与用户正在使用的应用抢单实例锁。现改为隔离数据根 + 隔离工作区。
// 用法：cd desktop/app && node e2e/cron_ui_check.mjs

import { _electron } from 'playwright'
import { createRequire } from 'module'
import { execSync, spawnSync } from 'child_process'
import fs from 'fs'
import os from 'os'
import path from 'path'
import { fileURLToPath } from 'url'

const require = createRequire(import.meta.url)
const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const REPO = path.resolve(APP, '../..')
// 隔离数据根（HAI_DATA_DIR → 多实例模式：独立 cfgDir + userData + bridge HOME，跳过单实例锁）
const DATA_DIR = process.env.GO_CODE_E2E_DATA_DIR || path.join(os.tmpdir(), 'gocode-e2e-cron-data')
const WS = process.env.GO_CODE_E2E_WS || path.join(os.tmpdir(), 'gocode-e2e-cron-ws')
const ELECTRON = require('electron')
const GOMODCACHE = execSync('go env GOMODCACHE').toString().trim()

function log(...a) { console.log('[cron-ui]', ...a) }
function sleep(ms) { return new Promise((r) => setTimeout(r, ms)) }
async function waitFor(fn, { timeout = 30000, interval = 500, label = '条件' } = {}) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v } catch { /* 继续 */ }
    await sleep(interval)
  }
  throw new Error(`等待超时: ${label}`)
}

// 前置：确保 bridge 二进制最新
function ensureBridge() {
  const r = spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], { cwd: REPO, encoding: 'utf8' })
  if (r.status !== 0) throw new Error(`go build bridge 失败:\n${r.stderr}`)
  log('bridge 已构建')
}

// 前置：把真实 cron 数据**复制**进隔离数据根（本脚本原本就依赖现有 c1 任务+历史来点行）。
// 复制而非共用：E2E 只在副本上跑，绝不改动用户真实 ~/.go-code/cron*。
function seedDataDir() {
  const realDir = path.join(os.homedir(), '.go-code')
  const dstDir = path.join(DATA_DIR, '.go-code')
  fs.mkdirSync(dstDir, { recursive: true })
  fs.mkdirSync(WS, { recursive: true })
  for (const name of ['cron.json', 'cron_runs.jsonl', 'settings.json']) {
    const src = path.join(realDir, name)
    if (fs.existsSync(src)) {
      fs.copyFileSync(src, path.join(dstDir, name))
      log(`已复制 ${name} → 隔离数据根`)
    }
  }
  const srcSessions = path.join(realDir, 'cron_sessions')
  if (fs.existsSync(srcSessions)) {
    fs.cpSync(srcSessions, path.join(dstDir, 'cron_sessions'), { recursive: true })
    log('已复制 cron_sessions → 隔离数据根')
  }
}

async function main() {
  ensureBridge()
  seedDataDir()
  let app, win
  try {
    log(`启动 Electron（隔离数据根 ${DATA_DIR}）…`)
    app = await _electron.launch({
      executablePath: ELECTRON,
      args: ['.'],
      cwd: APP,
      env: { ...process.env, HOME: DATA_DIR, HAI_DATA_DIR: DATA_DIR, GOMODCACHE },
    })
    win = await app.firstWindow()
    win.on('console', (msg) => { if (msg.type() === 'error') console.log('[renderer-err]', msg.text()) })
    await win.waitForLoadState('domcontentloaded')
    // 注入隔离工作区（不碰用户真实 localStorage —— userData 已随 HAI_DATA_DIR 隔离）
    await win.evaluate(([ws]) => {
      localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws }))
    }, [WS])
    await win.reload()
    log('等待主界面…')
    await waitFor(() => win.locator('.app-root').count().then((n) => n > 0), { label: '主界面' })
    await sleep(1500)

    // 1) 打开定时任务页（左栏按钮）
    log('打开定时任务页…')
    const cronBtn = win.locator('button:has-text("定时任务")').first()
    await cronBtn.click()
    await waitFor(() => win.locator('.cron-page').count().then((n) => n > 0), { label: 'cron 页' })
    await sleep(800)

    // 检查列表页
    const listChecks = await win.evaluate(() => {
      const card = document.querySelector('.cron-list-page .cron-card')
      const table = document.querySelector('.cron-list-page .cron-table')
      const pager = document.querySelector('.cron-list-page .cron-pager')
      const spContent = document.querySelector('.cron-list-page .sp-content')
      const cs = card ? getComputedStyle(card) : null
      const scs = spContent ? getComputedStyle(spContent) : null
      return {
        hasCard: !!card,
        cardMaxWidth: cs?.maxWidth,
        cardRadius: cs?.borderRadius,
        cardAlign: scs?.alignItems,
        hasTable: !!table,
        tableRows: table ? table.querySelectorAll('tbody tr').length : 0,
        hasPager: !!pager,
        pagerDisplay: pager ? getComputedStyle(pager).display : null,
        jobNames: [...(table ? table.querySelectorAll('tbody tr td:first-child .cron-prompt') : [])].map((e) => e.textContent),
      }
    })
    log('列表页检查:', JSON.stringify(listChecks, null, 2))

    // 2) 点击第一行任务 → 执行历史页
    log('点击任务进入执行历史…')
    await win.locator('.cron-job-row').first().click()
    await waitFor(() => win.locator('.cron-runs-page').count().then((n) => n > 0), { label: 'runs 页' })
    await sleep(800)
    const runsChecks = await win.evaluate(() => {
      const card = document.querySelector('.cron-runs-page .cron-card')
      const table = document.querySelector('.cron-runs-page .cron-table')
      const pager = document.querySelector('.cron-runs-page .cron-pager')
      const jobInfo = document.querySelector('.cron-runs-jobinfo')
      return {
        hasCard: !!card,
        hasTable: !!table,
        tableRows: table ? table.querySelectorAll('tbody tr').length : 0,
        hasPager: !!pager,
        jobInfoText: jobInfo?.textContent?.slice(0, 80),
        runIds: [...(table ? table.querySelectorAll('tbody tr .mono') : [])].map((e) => e.textContent).slice(0, 3),
      }
    })
    log('执行历史页检查:', JSON.stringify(runsChecks, null, 2))

    // 3) 点击第一行执行 → 详情页
    log('点击执行进入详情…')
    await win.locator('.cron-run-row').first().click()
    await waitFor(() => win.locator('.cron-detail-page').count().then((n) => n > 0), { label: 'detail 页' })
    await sleep(800)
    const detailChecks = await win.evaluate(() => {
      const card = document.querySelector('.cron-detail-page .cron-card')
      const meta = document.querySelector('.cron-detail-meta')
      const msgs = [...document.querySelectorAll('.cron-detail-messages .msg')]
      return {
        hasCard: !!card,
        hasMeta: !!meta,
        msgCount: msgs.length,
        msgRoles: msgs.map((m) => m.className),
        msgWho: msgs.map((m) => m.querySelector('.who b')?.textContent),
        msgBodyHasMd: msgs.map((m) => !!m.querySelector('.body .md-text') || !!m.querySelector('.body p')),
        cardRadius: card ? getComputedStyle(card).borderRadius : null,
      }
    })
    log('详情页检查:', JSON.stringify(detailChecks, null, 2))

    // 汇总
    const allOk =
      listChecks.hasCard && listChecks.hasTable && listChecks.cardMaxWidth && listChecks.cardAlign === 'center' &&
      runsChecks.hasCard && runsChecks.hasTable &&
      detailChecks.hasCard && detailChecks.msgCount >= 1 && detailChecks.msgRoles.length > 0
    log(allOk ? '✅ 全部布局检查通过' : '❌ 存在布局问题')
    console.log(JSON.stringify({ listChecks, runsChecks, detailChecks, allOk }, null, 2))
  } finally {
    if (app) await app.close().catch(() => {})
  }
}

main().catch((e) => { console.error('❌', e); process.exit(1) })
