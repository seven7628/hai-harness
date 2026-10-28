import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import './index.css'

// TEMP-STARTUP：黑屏定位打点（renderer 侧时间轴，经 IPC 回传 main 写 electron-startup.log）
if (window.desktop) {
  document.body.classList.add('is-electron')
  window.desktop.startupMark('renderer-js-exec')
}

// 中文字体 JS 动态注入（首帧后异步加载，零阻塞——head 静态 link 会阻塞 module script）：
// Noto Serif SC / Nanum Brush Script / Zhi Mang Xing 分片按需，IBM Plex 已本地托管。
function injectCJKFonts(): void {
  const urls = [
    'https://fonts.googleapis.com/css2?family=Noto+Serif+SC:wght@500;600;700&display=swap',
    'https://fonts.googleapis.com/css2?family=Nanum+Brush+Script&family=Zhi+Mang+Xing&display=swap',
  ]
  for (const href of urls) {
    const link = document.createElement('link')
    link.rel = 'stylesheet'
    link.href = href
    document.head.appendChild(link)
  }
}

// 开发期验证钩子：把 store 暴露到 window（**仅 dev 构建**）。lab/ 下的浏览器断言
// 需要在真应用里切档（密度/皮肤/会话）——这些是 store 上的动作，不是 DOM 属性；
// 靠点设置页去切会把「验证一条规则」变成「走一遍 UI」。生产构建不含这段。
if (import.meta.env.DEV) {
  void import('./store/useAppStore').then((m) => {
    ;(window as unknown as { __store?: unknown }).__store = m.useAppStore
  })
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)

// 首帧绘制后打点 + 注入中文字体（不阻塞首帧）
const desktop = window.desktop // 收缩缓存：回调内 TS 不保持外层 window.desktop 收缩
if (desktop) {
  requestAnimationFrame(() => {
    desktop.startupMark('renderer-first-frame')
    injectCJKFonts()
  })
} else {
  injectCJKFonts()
}
