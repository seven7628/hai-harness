import React from 'react'
import ReactDOM from 'react-dom/client'
import './surfaces/builtin' // 注册内置 SurfaceProvider（副作用 import，必须早于首次渲染）
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
    const w = window as unknown as { __store?: unknown; __dispatch?: unknown; __hydrate?: unknown }
    w.__store = m.useAppStore
    // 恢复路径（checkpoint hydrate）也是验证入口：lab/ 要能构造「刷新时 run 还在跑」
    // 这种形态（live 位 + checkpoint 里 running 的顶层 run 节点），而不是只注入最终块。
    w.__hydrate = m.hydrateCheckpoint
    // 事件分发也是验证入口：lab/ 的断言要能构造「事件顺序」这类场景
    //（例如运行中插话的 injected 位、子 agent 摘离的任务），而不是只注入最终块。
    w.__dispatch = m.dispatchEvent
    // markdown 解析缓存（第 2 步）的观测口：切会话/刷新复用的就是它，量内存要用
    void import('./lib/mdPipeline').then((p) => {
      ;(w as unknown as Record<string, unknown>).__mdCache = { stats: p.mdCacheStats, clear: p.clearMdCache }
    })
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
