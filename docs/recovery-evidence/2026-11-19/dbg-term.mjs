import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { chromium } from 'playwright'
const APP = '/Users/admin/workspace/go-code/.go-code/worktrees/terminal-panel/desktop/app'
const E2E = path.join(APP, 'e2e')
// 复用探针页（上次运行已删，这里重新生成一次：直接跑 verify 的生成段）
const src = fs.readFileSync(path.join(E2E, 'verify_terminal.mjs'), 'utf8')
const gen = src.slice(src.indexOf("fs.mkdirSync(PROBE_DIR"), src.indexOf("const server = spawn"))
// 只执行生成段：把常量准备好
const PORT = 5199
const PROBE_DIR = path.join(E2E, '_dbgterm_run')
const MARKER = '<title>terminal probe</title>'
eval(gen)
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
const url = `http://localhost:${PORT}/e2e/_dbgterm_run/index.html`
for (let i = 0; i < 80; i++) { try { const r = await fetch(url); if (r.ok) break } catch {} await new Promise(r => setTimeout(r, 250)) }
const b = await chromium.launch()
const p = await b.newPage({ viewport: { width: 1280, height: 800 } })
p.on('console', m => console.log('[console]', m.type(), m.text().slice(0, 200)))
p.on('pageerror', e => console.log('[pageerror]', String(e).slice(0, 300)))
await p.goto(url)
await p.waitForTimeout(2000)
await p.evaluate(() => window.__store.getState().openTab({ kind: 'term' }))
await p.waitForTimeout(1500)
const info = await p.evaluate(() => {
  const host = document.querySelector('.term-host')
  const xterm = document.querySelector('.xterm')
  const rows = document.querySelector('.xterm-rows')
  return {
    hostHTML: host ? host.innerHTML.slice(0, 400) : null,
    hasXterm: !!xterm, hasRows: !!rows,
    rowsText: rows ? rows.textContent : null,
    screenText: document.querySelector('.xterm-screen')?.textContent ?? null,
    bodyText: document.body.textContent?.slice(0, 200),
    fakeWsSent: window.__fakeWs ? Object.keys(window.__fakeWs) : null,
    paneState: document.querySelector('.term-pane')?.getAttribute('data-state'),
  }
})
console.log(JSON.stringify(info, null, 2).slice(0, 2000))
server.kill('SIGTERM')
await b.close()
fs.rmSync(PROBE_DIR, { recursive: true, force: true })
