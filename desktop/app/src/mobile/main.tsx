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
import { setHubTransport } from '../transport/host'
import { installThemeColor } from './chrome'
import MobileApp from './MobileApp'
import { sharedMobileTransport } from './transport/mobile'
import { readThemePref } from './theme'
import { installVisualViewport } from './visual-viewport'
import './mobile.css'

// Hub transport seam（P8-01b）：Hub 的 store/命令/附件只认 transport/host.ts。
// 手机壳注册自己的 transport —— **不 import transport/index.ts**，否则 electron/mock
// 会被静态边拉进手机包。sharedMobileTransport() 是进程内单例，壳与 Hub 拿到同一个实例。
setHubTransport(() => sharedMobileTransport())

// getTransport() 的分流开关（transport/index.ts）：手机包不 import 它，但浏览器里若
// 有模块误引（未来的共享组件），这个开关能保证分流仍落到手机 transport 而不是 mock。
// 置位必须在首次渲染前：getTransport() 是渲染/动作时调用的，静态 import 只求值模块。
window.__HAI_MOBILE__ = true

applyNarrativeSkin('runtime')
applyTheme(readThemePref())

// 软键盘：把 visualViewport 的可见高度/偏移写到 --m-vv-*（见 visual-viewport.ts）。
// 必须在首帧前装好，否则 composer 会先按 100dvh 画一帧再被键盘顶掉。
installVisualViewport()

// 浏览器 UI（状态栏/地址栏）颜色跟随 ink/paper，含 system 跟随的实时切换。
installThemeColor()

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
