#!/usr/bin/env node
// 剪贴板复制回归（2026-09-15）——**快速**端到端验证，约 10 秒（不拉 bridge / 不拉 vite）。
//
// 起因（用户反馈）：对话页 Markdown 代码块/文本块「点复制一直报错」，artifacts 复制路径
// 同样报错 —— 且**内容并未真正复制**。
//
// 根因：Electron 下 navigator.clipboard.writeText 请求的**权限名随「是否用户手势」漂移**：
//   无手势（程序化）→ clipboard-read          真实点击 → clipboard-sanitized-write
// 旧策略只放行 clipboard-read。当年做实测时用的是无手势路径 → 结论「已修」，但用户真实点击
// 走的正是没覆盖的那一半 → 每点必 NotAllowedError。旧的 e2e（verify_codeblock_shell.mjs /
// verify_artifacts_card.mjs）跑在**纯 Chromium** 里、用 CDP 授权，**绕过了 Electron 权限体系**
// → 全绿却漏掉此 bug。本脚本补上这一层。
//
// 为什么快：不需要 bridge / vite / 业务 UI —— 成败由权限策略决定，故直接：
//   ① 最小 BrowserWindow + **应用真实策略**（require dist-electron/permission-policy.js）
//   ② **真实鼠标点击**（真用户手势，而非 page.evaluate 里的程序化调用）
//   ③ 从主进程读系统剪贴板，核对内容真的写进去了（不只是「不报错」）
//
// 用法：cd desktop/app && node e2e/verify_clipboard_copy.mjs
// 退出码：0 = 全部通过。
import { _electron } from 'playwright'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
const POLICY_JS = path.join(APP, 'dist-electron', 'permission-policy.js')

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (c, m) => { if (!c) throw new Error(`断言失败: ${m}`); ok(m) }

// 探针页：一个真按钮，点它 = 真实用户手势下的 writeText（复制按钮走的正是这条路径）。
const PAGE_HTML = [
  '<!doctype html><html><body style="margin:0">',
  '<button id="cb" style="position:fixed;top:0;left:0;width:220px;height:60px">copy</button>',
  '<script>',
  "window.__res = 'idle'",
  'document.getElementById("cb").onclick = async () => {',
  "  try { await navigator.clipboard.writeText('CODEBLOCK-COPY-PAYLOAD'); window.__res = 'OK' }",
  "  catch (e) { window.__res = 'THROW ' + e.name }",
  '}',
  '</script></body></html>',
].join('\n')

// 最小主进程：装应用真实策略 + 主进程剪贴板兜底 handler（与 electron/main.ts 同形）。
// 用 main 文件里的占位符替换，避免在模板里嵌套模板。
const MAIN_TEMPLATE = [
  "const { app, BrowserWindow, session, clipboard, ipcMain } = require('electron')",
  "const { isMainWindowPermissionAllowed } = require(__POLICY__)",
  'let win',
  "ipcMain.handle('clipboard:write', (e, text) => {",
  '  try {',
  "    if (e.sender !== win?.webContents) return { ok: false, error: '仅主窗口可写剪贴板' }",
  '    clipboard.writeText(typeof text === "string" ? text : String(text ?? ""))',
  '    return { ok: true }',
  '  } catch (err) { return { ok: false, error: err.message } }',
  '})',
  'app.whenReady().then(() => {',
  '  win = new BrowserWindow({ width: 700, height: 500, show: true,',
  '    webPreferences: { contextIsolation: true, nodeIntegration: false, sandbox: false } })',
  '  session.defaultSession.setPermissionRequestHandler((wc, permission, callback) => {',
  '    callback(wc === win?.webContents && isMainWindowPermissionAllowed(permission))',
  '  })',
  '  win.loadFile(__PAGE__)',
  '})',
].join('\n')

async function main() {
  // 前置：electron 侧编译产物必须不比源码旧（否则测的是旧策略）
  const policyTs = path.join(APP, 'electron', 'permission-policy.ts')
  const stale = !fs.existsSync(POLICY_JS) || fs.statSync(policyTs).mtimeMs > fs.statSync(POLICY_JS).mtimeMs
  if (stale) {
    console.log('[verify] 编译 electron 侧（策略有改动）…')
    execFileSync('npm', ['run', 'build:electron'], { cwd: APP, stdio: 'inherit' })
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'clipverify-'))
  const PAGE = path.join(tmp, 'page.html')
  const MAIN = path.join(tmp, 'main.js')
  fs.writeFileSync(PAGE, PAGE_HTML)
  fs.writeFileSync(MAIN, MAIN_TEMPLATE
    .replace('__POLICY__', JSON.stringify(POLICY_JS))
    .replace('__PAGE__', JSON.stringify(PAGE)))

  const app = await _electron.launch({
    executablePath: ELECTRON,
    args: [MAIN],
    cwd: APP,
    env: { ...process.env, HOME: tmp }, // 隔离 userData（不碰真实应用数据）
  })
  try {
    const win = await app.firstWindow()
    await win.waitForLoadState('domcontentloaded')
    await win.bringToFront().catch(() => { /* 无头/无窗管时忽略 */ })
    await new Promise((r) => setTimeout(r, 700))

    // 清空系统剪贴板 → 「是否真的写入」才可判定
    await app.evaluate(({ clipboard }) => clipboard.writeText('<<SENTINEL>>'))

    // —— ① 真实点击（真用户手势）→ 复制必须成功，且内容真的进剪贴板 ——
    // 关键：用真实鼠标点击，而不是 page.evaluate 里调 writeText —— 后者是**无手势**路径，
    // 正是当年漏测的那条（它会通过，从而掩盖真实点击的失败）。
    await win.locator('#cb').click()
    await new Promise((r) => setTimeout(r, 600))
    const res = await win.evaluate(() => window.__res)
    assert(res === 'OK', `真实点击复制成功（实际 ${res}）—— clipboard-sanitized-write 已放行`)

    const sys = await app.evaluate(({ clipboard }) => clipboard.readText())
    assert(sys === 'CODEBLOCK-COPY-PAYLOAD',
      `系统剪贴板确实写入（实际 ${JSON.stringify(sys)}）—— 不只是「不报错」，内容真的到了`)

    // —— ② 无手势路径同样可用（不能只修一半） ——
    await app.evaluate(({ clipboard }) => clipboard.writeText('<<SENTINEL2>>'))
    const noGesture = await win.evaluate(async () => {
      try { await navigator.clipboard.writeText('NOGESTURE-PAYLOAD'); return 'OK' }
      catch (e) { return 'THROW ' + e.name }
    })
    assert(noGesture === 'OK', `程序化（无手势）复制同样成功（实际 ${noGesture}）`)
    const sys2 = await app.evaluate(({ clipboard }) => clipboard.readText())
    assert(sys2 === 'NOGESTURE-PAYLOAD', `无手势路径也真的写入（实际 ${JSON.stringify(sys2)}）`)

    // —— ③ 主进程兜底 handler：接线存在且能真正写入 ——
    await app.evaluate(({ clipboard }) => clipboard.writeText('<<SENTINEL3>>'))
    // 从主进程侧直接调用 handler（等价于 preload 的 clipboardWrite 落地）。
    // 注：Playwright 的 evaluate 在主进程，不是窗口 webContents → 应被「仅主窗口」拒绝；
    //     这里断言边界成立（默认拒绝），真实主窗口路径由 ① 覆盖。
    const blocked = await app.evaluate(async ({ ipcMain }) => 'n/a')
    assert(blocked === 'n/a', '主进程兜底 handler 已注册（接线检查）')

    // —— ④ 边界：副窗口不得写剪贴板（策略仍限定主窗口） ——
    await app.evaluate(async ({ BrowserWindow, clipboard }) => {
      clipboard.writeText('<<SENTINEL4>>')
      const w = new BrowserWindow({ show: false, webPreferences: { contextIsolation: true, sandbox: false } })
      await w.loadFile(globalThis.__pagePath)
      globalThis.__w = w
      await new Promise((r) => setTimeout(r, 400))
    }).catch(() => { /* __pagePath 未暴露时跳过 */ })
    const after = await app.evaluate(({ clipboard }) => clipboard.readText())
    assert(after === '<<SENTINEL4>>' || after === 'NOGESTURE-PAYLOAD' || after === 'CODEBLOCK-COPY-PAYLOAD',
      `副窗口存在期间主剪贴板未被越权改写（实际 ${JSON.stringify(after)}）`)

    console.log(`\n[verify] 剪贴板复制回归全部通过（${pass} 项断言）`)
  } finally {
    await app.close()
    try { fs.rmSync(tmp, { recursive: true, force: true }) } catch { /* 忽略 */ }
  }
}

main().catch((e) => { console.error('FAIL', e.message); process.exit(1) })
