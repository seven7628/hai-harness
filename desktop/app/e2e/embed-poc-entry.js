// M0 POC 冒烟测试（独立入口，不走 React/UI/bridge）：
// 直接驱动 dist-electron/embed.js，自证「内嵌 WebContentsView 的 CDP 能被外部驱动」。
// 用法：cd desktop/app && ./node_modules/.bin/electron e2e/embed-poc-entry.js
const { app, BrowserWindow } = require('electron')
const embed = require('../dist-electron/embed.js')

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

app.disableHardwareAcceleration()

app.whenReady().then(async () => {
  const win = new BrowserWindow({ width: 1200, height: 800, show: false })
  const url =
    'data:text/html,<html><head><title>POC-EMBED-OK</title></head><body><div id="k">hi-from-embed</div></body></html>'
  try {
    embed.showEmbed(win, url, { x: 0, y: 0, width: 600, height: 800 })

    // 等 debugger attach 就绪
    let st = embed.embedStatus()
    for (let i = 0; i < 20 && !st.attached; i++) {
      await sleep(250)
      st = embed.embedStatus()
    }
    console.log('[poc] status =', JSON.stringify(st))

    // 外部 CDP 驱动内嵌视图：读 document.title + 取某个 DOM 节点的 innerText
    const res = await embed.embedCommand('Runtime.evaluate', {
      expression: 'document.title + "|" + document.getElementById("k").innerText',
      returnByValue: true,
    })
    console.log('[poc] evaluate =', JSON.stringify(res))
    const value = res?.result?.result?.value
    const pass = st.attached && value === 'POC-EMBED-OK|hi-from-embed'
    console.log('[poc] RESULT=' + (pass ? 'PASS' : 'FAIL') + (value ? ' value=' + value : ''))
  } catch (e) {
    console.log('[poc] ERROR ' + (e && e.message ? e.message : e))
  } finally {
    embed.destroyEmbed()
    app.exit(0)
  }
})
