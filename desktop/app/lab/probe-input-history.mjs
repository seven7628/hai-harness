// 输入框历史（↑/↓）的浏览器探针：正文 / Command / 文档引用 / 图片 / 纯图片 / 同文不同图。
//
// 用法：
//   cd desktop/app && npx vite --port 5288
//   node lab/probe-input-history.mjs          # 或 LAB_URL=http://localhost:5173/ node ...
//
// 判据：历史回填后 textarea 的值、附件 chip 数量与图片 src 必须与发送前一致；
// localStorage 里只允许出现图片指纹，不允许出现图片 data URL。
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const b = await Browser.launch({ width: 1280, height: 900 })
const page = await b.newPage({ width: 1280, height: 900, reducedMotion: true })
await page.goto(URL, { settleMs: 3500 })

const waitFor = async (fn, label, timeout = 8000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval(fn)) return
    await wait(80)
  }
  throw new Error('timeout: ' + label)
}
const val = () => page.eval(() => document.querySelector('.composer textarea')?.value ?? null)
const type = (text) => page.eval((t) => {
  const el = document.querySelector('.composer textarea')
  el.focus()
  Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(el, t)
  el.dispatchEvent(new Event('input', { bubbles: true }))
}, text)
const key = async (k, code, vk) => {
  await page.send('Input.dispatchKeyEvent', { type: 'keyDown', key: k, code, windowsVirtualKeyCode: vk, nativeVirtualKeyCode: vk })
  await page.send('Input.dispatchKeyEvent', { type: 'keyUp', key: k, code, windowsVirtualKeyCode: vk, nativeVirtualKeyCode: vk })
}
const up = () => key('ArrowUp', 'ArrowUp', 38)
const enter = () => key('Enter', 'Enter', 13)
const imageCount = () => page.eval(() => document.querySelectorAll('.composer .attach-chip img, .composer .cy-im').length)
const docCount = () => page.eval(() => document.querySelectorAll('.composer .doc-chip').length)
const imageSrcs = () => page.eval(() => [...document.querySelectorAll('.composer .attach-chip img, .composer .cy-im')].map((x) => x.src))
const check = (label, got, want) => {
  const ok = JSON.stringify(got) === JSON.stringify(want)
  console.log((ok ? 'PASS ' : 'FAIL ') + label + ': got=' + JSON.stringify(got) + ' want=' + JSON.stringify(want))
  if (!ok) process.exitCode = 1
}
const pasteImage = (dataUrl, name) => page.eval(async ({ u, n }) => {
  const blob = await (await fetch(u)).blob()
  const file = new File([blob], n, { type: 'image/png' })
  const dt = new DataTransfer()
  dt.items.add(file)
  document.querySelector('.composer textarea').dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }))
}, { u: dataUrl, n: name })
const dropDoc = (name) => page.eval((n) => {
  const dt = new DataTransfer()
  dt.items.add(new File(['hello doc'], n, { type: 'text/plain' }))
  const target = document.querySelector('.composer')
  for (const ev of ['dragenter', 'dragover', 'drop']) {
    target.dispatchEvent(new DragEvent(ev, { dataTransfer: dt, bubbles: true, cancelable: true }))
  }
}, name)
const clearChips = async () => {
  for (let i = 0; i < 20; i++) {
    const n = await page.eval(() => {
      const xs = [...document.querySelectorAll('.composer .attach-chip .attach-x')]
      if (!xs.length) return 0
      xs[0].click()
      return xs.length
    })
    if (!n) break
    await wait(60)
  }
}
const send = async () => {
  await enter()
  await waitFor(() => document.querySelector('.composer textarea').value === '', 'send clears input')
  await wait(450)
}

const RED = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAFgwJ/lU4W8QAAAABJRU5ErkJggg=='
const BLUE = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII='

await waitFor(() => !!document.querySelector('.composer textarea'), 'composer')
await clearChips()

// 1. 正文 + 图片
await type('看图')
await pasteImage(RED, 'red.png')
await waitFor(() => document.querySelectorAll('.composer .attach-chip img, .composer .cy-im').length === 1, 'red chip')
await send()
await up()
check('正文回填', await val(), '看图')
check('图片回填', await imageSrcs(), [RED])
const stored = await page.eval(() => localStorage.getItem('go-code.inputHistory'))
check('图片字节不落 localStorage', stored.includes('iVBORw0KGgo'), false)

// 2. 纯图片
await clearChips()
await type('')
await pasteImage(BLUE, 'blue.png')
await waitFor(() => document.querySelectorAll('.composer .attach-chip img, .composer .cy-im').length === 1, 'blue chip')
await send()
await up()
check('纯图片：正文为空', await val(), '')
check('纯图片：附件回填', await imageSrcs(), [BLUE])

// 3. 文档引用 chips
await clearChips()
await type('读文件')
await dropDoc('note.txt')
await waitFor(() => document.querySelectorAll('.composer .doc-chip').length === 1, 'doc chip')
await send()
await up()
check('文档引用：正文', await val(), '读文件')
check('文档引用：chip 回填', await docCount(), 1)
const stored3 = await page.eval(() => localStorage.getItem('go-code.inputHistory'))
check('文档引用：只存路径不存内容', stored3.includes('hello doc'), false)
check('文档引用：路径已持久化', stored3.includes('/mock/note.txt'), true)

// 4. Command
await clearChips()
await type('/summary')
await send()
await up()
check('Command 回填', await val(), '/summary')

// 5. 同文不同图：两条；完全重复：去重
await clearChips()
await type('同文')
await pasteImage(RED, 'red.png')
await waitFor(() => document.querySelectorAll('.composer .attach-chip img, .composer .cy-im').length === 1, 'red again')
await send()
await clearChips()
await type('同文')
await pasteImage(BLUE, 'blue.png')
await waitFor(() => document.querySelectorAll('.composer .attach-chip img, .composer .cy-im').length === 1, 'blue again')
await send()
await up()
check('同文不同图：最新是 blue', await imageSrcs(), [BLUE])
await up()
check('同文不同图：上一条是 red', await imageSrcs(), [RED])
await up()
check('同文不同图：再上一条是 /summary', await val(), '/summary')

// 6. 本地镜像被清（清站点数据/换安装）→ 从「本地文件」（mock = 独立 localStorage 键）恢复
await type('')
await page.eval(() => document.querySelector('.composer textarea').setSelectionRange(0, 0))
const sid = await page.eval(() => window.__store.getState().activeSessionId)
const ws = await page.eval(() => window.__store.getState().workspace)
await page.eval(() => localStorage.removeItem('go-code.inputHistory'))
check('本地镜像已清', await page.eval(() => localStorage.getItem('go-code.inputHistory')), null)
await page.eval(() => window.__store.getState().newSession())
await wait(1200)
await page.eval(({ sid, ws }) => window.__store.getState().setActiveSession(sid, ws), { sid, ws })
await wait(1500)
await up()
check('从文件镜像恢复最新一条', await val(), '同文')
check('从文件镜像恢复图片', await imageSrcs(), [BLUE])

console.log('errors', page.consoleErrors)
await b.close()
