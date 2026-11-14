// 手机入口（P8-01a）：只装基础设施 —— mobile transport + host seam + 最小壳。
//
// 与桌面 main.tsx 的三点差别（都为了不把桌面 wiring 拉进手机包）：
//   1. 不 import App / index.css / useAppStore / mock transport —— 手机只走
//      HTTP /api/cmd + WS /api/events（见 transport/mobile.ts 文件头）。
//   2. 样式只引 tokens（runtime-tokens/runtime-skin 的令牌层）+ hub.css + mobile.css，
//      不引整个 index.css（那是三栏桌面布局 + tailwind）。
//   3. 固定 data-skin='runtime'（MOBILE-UI-REUSE §0.5：手机皮肤收敛到 runtime）。
import React from 'react'
import ReactDOM from 'react-dom/client'
import { applyNarrativeSkin } from '../lib/narrativeSkin'
import { applyTheme } from '../lib/theme'
import MobileApp from './MobileApp'
import { readThemePref } from './theme'
import './mobile.css'

// getTransport() 的分流开关（transport/index.ts）。置位必须在首次渲染前：
// 静态 import 只会「求值」模块，getTransport() 是渲染时调用的，所以这里置位够早。
// 注意：手机壳自己直接 import mobile/transport/mobile（不经过 transport/index.ts），
// 免得把 mock/electron 两个 transport 打进手机包；两边通过 sharedMobileTransport()
// 拿到同一个实例。
window.__HAI_MOBILE__ = true

applyNarrativeSkin('runtime')
applyTheme(readThemePref())

// 中文字体首帧后异步注入（与桌面同源，避免首屏阻塞）。
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

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <MobileApp />
  </React.StrictMode>,
)

requestAnimationFrame(injectCJKFonts)
