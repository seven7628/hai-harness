#!/usr/bin/env node
// _surface_dev_reload.mjs —— CreateMode **热加载**的真机冒烟（dev 安装 → 改 panel.js → 面板自己换内容）。
//
// 与 e2e/verify_agent_session_electron.mjs 同一套起法（真安装器 + 真 Electron + 真协议），
// 只把探针从「host.command」换成「文本版本号」：断言的是**重新 import 了新的模块字节**。
//
// 链路：surface_install({dev:true}) → <root>/<id> 是源目录的软链 → fsnotify 盯着受追踪文件
// → 保存 panel.js 触发 surfaceContentHash 变化 → surfaces_changed（真桥推送）→ 渲染层重载面板。
// 判据 V1 → V2：没有重装、没有重启、标签还在原位。
//
// 用法：cd desktop/app && node e2e/_surface_dev_reload.mjs
// 退出码：0 通过 / 1 断言失败 / 2 前置不满足。
import { _electron } from 'playwright'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const REPO = path.resolve(APP, '../..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const ID = 'dev-hot'
const KIND = `plugin:${ID}`
const fails = []
const check = (label, cond, detail = '') => {
  if (cond) console.log(`  ✅ ${label}`)
  else { fails.push(label); console.log(`  ❌ ${label}${detail ? ' —— ' + detail : ''}`) }
}
async function waitFor(fn, { timeout = 20000, label = '条件' } = {}) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v } catch { /* 继续等 */ }
    await sleep(150)
  }
  throw new Error(`等待超时：${label}`)
}

// 面板只做一件事：把自己被 import 时读到的那份字节里的版本号画出来。
const panelJs = (tag) => `// panel.js —— 热加载探针（tag=${tag}）
export const surface = {
  kind: ${JSON.stringify(KIND)},
  apply({ React, surfaces }) {
    const { createElement: h } = React
    function Hot() {
      return h('div', { 'data-hot': ${JSON.stringify(tag)}, style: { padding: 12, font: '13px ui-monospace, monospace' } },
        ${JSON.stringify(`HOT-${tag}`)})
    }
    surfaces.register({ kind: ${JSON.stringify(KIND)}, group: 'plugin', order: 20, title: () => '热加载探针', render: () => h(Hot) })
  },
}
`

const MANIFEST = {
  id: ID,
  name: { zh: '热加载探针', en: 'Hot reload probe' },
  version: '1.0.0',
  api: 1,
  kind: KIND,
  entry: 'panel.js',
  icon: 'icon.svg',
  placement: 'plugin',
  commands: [],
  data: ['surfaces_changed'],
}
const ICON = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" fill="none" stroke="#7aa2f7" stroke-width="2"/></svg>\n'

let HOME = ''
let SRC = ''
let app
try {
  console.log('\nT1 源目录 + dev 安装（真安装器，-dev）')
  SRC = fs.mkdtempSync(path.join(os.tmpdir(), 'dev-hot-src-'))
  fs.writeFileSync(path.join(SRC, 'surface.json'), JSON.stringify(MANIFEST, null, 2) + '\n')
  fs.writeFileSync(path.join(SRC, 'panel.js'), panelJs('V1'))
  fs.writeFileSync(path.join(SRC, 'icon.svg'), ICON)
  HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'dev-hot-home-'))
  fs.mkdirSync(path.join(HOME, '.go-code'), { recursive: true })
  fs.writeFileSync(path.join(HOME, '.go-code', 'settings.json'), JSON.stringify({ theme: 'dark', lang: 'zh' }, null, 2))
  const inst = spawnSync('go', ['test', './desktop/bridge', '-run', 'TestSurfaceInstallExternal', '-count=1', '-v',
    '-args', `-dir=${SRC}`, `-home=${HOME}`, '-dev=1'], { cwd: REPO, encoding: 'utf8' })
  const out = (inst.stdout || '') + (inst.stderr || '')
  check('安装回执 status=applied', /"status": "applied"/.test(out), out.split('\n').slice(-6).join(' | '))
  const linkPath = path.join(HOME, '.go-code', 'surfaces', ID)
  check('dev 安装落盘物是软链（指向源目录）',
    fs.lstatSync(linkPath).isSymbolicLink() && fs.realpathSync(linkPath) === fs.realpathSync(SRC), linkPath)
  const idx = JSON.parse(fs.readFileSync(path.join(HOME, '.go-code', 'surfaces', 'index.json'), 'utf8'))
  const beforeHash = idx.items[0].hash
  check('index 里 dev=true', idx.items[0].dev === true, JSON.stringify(idx.items[0]))

  console.log('\nT2 启动真 Electron')
  const USER_DATA = path.join(os.tmpdir(), `hai-desktop-${Math.abs([...HOME].reduce((h, c) => ((h << 5) - h + c.charCodeAt(0)) | 0, 0)).toString(36)}`)
  fs.mkdirSync(USER_DATA, { recursive: true })
  app = await _electron.launch({ executablePath: ELECTRON, args: ['.'], cwd: APP, env: { ...process.env, HOME, HAI_DATA_DIR: HOME } })
  const win = await app.firstWindow()
  const errs = []
  win.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  win.on('console', (m) => { const t = m.text(); if (/surface|bridge|plugin/i.test(t)) errs.push('console.' + m.type() + ': ' + t) })
  await win.waitForLoadState('domcontentloaded')
  const WS = fs.mkdtempSync(path.join(os.tmpdir(), 'dev-hot-ws-'))
  await win.evaluate(([ws]) => localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws })), [WS])
  await win.reload()
  await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: '主界面就绪' })

  console.log('\nT3 打开插件面板（V1）')
  const rail = win.locator('[data-kind="activity"]').first()
  await waitFor(() => rail.count().then((n) => n > 0), { label: '右栏图标条' })
  await rail.click({ timeout: 5000 }).catch(() => {})
  await waitFor(() => win.locator('.rtabs-add').count().then((n) => n > 0), { label: '＋ 按钮' })
  await win.locator('.rtabs-add').click()
  const entry = `[data-plugin-entry="${ID}/default"]`  // 零 entries 的插件由宿主合成一个 default 入口
  await waitFor(() => win.locator(entry).count().then((n) => n > 0), { label: '＋ 菜单里的插件入口' })
  await win.locator(entry).first().click()
  const hotText = () => win.locator('[data-hot]').first().getAttribute('data-hot', { timeout: 800 }).catch(() => '')
  const v1 = await waitFor(async () => (await hotText()) || null, { label: '面板 V1' })
  check('面板内容 = V1（真协议 import 到的第一版）', v1 === 'V1', String(v1))
  const tabsBefore = await win.locator('.rtabs .rtab').count()

  console.log('\nT4 改 panel.js（不重装、不重启）')
  const t0 = Date.now()
  fs.writeFileSync(path.join(SRC, 'panel.js'), panelJs('V2'))
  const v2 = await waitFor(async () => { const v = await hotText(); return v === 'V2' ? v : null }, { timeout: 15000, label: '面板换成 V2' })
    .catch(() => '')
  const ms = Date.now() - t0
  check(`保存后面板自己换成 V2（${ms}ms，无重装/无重启）`, v2 === 'V2', String(v2))
  check('标签没有被重启掉（同一个标签还开着）', (await win.locator('.rtabs .rtab').count()) === tabsBefore,
    `${tabsBefore} → ${await win.locator('.rtabs .rtab').count()}`)
  const idx2 = JSON.parse(fs.readFileSync(path.join(HOME, '.go-code', 'surfaces', 'index.json'), 'utf8'))
  check('index.hash 已换成内容修订（watcher 写过）', idx2.items[0].hash !== beforeHash,
    `${beforeHash} → ${idx2.items[0].hash}`)
  check('全程零 pageerror', errs.length === 0, errs.join(' | '))
} catch (e) {
  console.error('\n异常：', e)
  fails.push(String(e?.message ?? e))
} finally {
  await app?.close().catch(() => {})
}
console.log(fails.length === 0 ? '\n✅ 热加载冒烟全部通过' : `\n❌ ${fails.length} 项失败：\n  - ${fails.join('\n  - ')}`)
process.exit(fails.length === 0 ? 0 : 1)
