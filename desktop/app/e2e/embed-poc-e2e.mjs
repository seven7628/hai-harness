// M0 POC 冒烟测试（Playwright 驱动 Electron，捕获主进程 [poc] 日志）。
// 用法：cd desktop/app && node e2e/embed-poc-e2e.mjs
import { _electron } from 'playwright'
import { createRequire } from 'module'
import path from 'path'
import { fileURLToPath } from 'url'

const require = createRequire(import.meta.url)
const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const ELECTRON = require('electron')
const ENTRY = path.join(__dirname, 'embed-poc-entry.js')

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const app = await _electron.launch({ args: [ENTRY], executablePath: ELECTRON, env: { ...process.env, ELECTRON_DISABLE_GPU: '1' } })
const out = app.process().stdout
let buffer = ''
const seen = []
out.on('data', (d) => {
  buffer += d.toString()
  const lines = buffer.split('\n')
  buffer = lines.pop() ?? ''
  for (const l of lines) if (l.includes('[poc]')) seen.push(l)
})
try {
  await app.waitForEvent('close', { timeout: 30000 })
} catch {
  await app.close().catch(() => {})
}
console.log(seen.join('\n'))
const pass = seen.some((l) => l.includes('RESULT=PASS'))
console.log('[runner] overall=' + (pass ? 'PASS' : 'FAIL'))
process.exit(pass ? 0 : 1)
