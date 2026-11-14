// 软键盘适配（P8-03a）：只靠 100dvh 不够。
//
// 100dvh 是「动态视口」——随浏览器 UI 伸缩，但**不随软键盘缩小**（iOS Safari 尤其：
// 键盘弹出时 layout viewport 不变，composer 被压在键盘下面）。visualViewport 是唯一
// 能拿到「当前真正可见区域」的接口：
//   · height    → 可见高度（键盘弹出后变小）
//   · offsetTop → 浏览器为露出焦点把页面向上滚时，可见区相对 layout viewport 的偏移
//
// 把两者写到 <html> 的 --m-vv-height / --m-vv-offset，mobile.css 的 .m-app 用它们
// 定高 + translateY，composer 因此始终贴在键盘上沿。没有 visualViewport 的浏览器
// （老 WebView / 桌面 Chromium 的某些环境）不写变量，CSS 回落 100dvh —— 行为与
// P8-02 完全一致。
//
// 为什么放在 main.tsx 而不是 React 组件里：它是纯 DOM 观察者，与渲染无关；装在入口
// 可以保证首帧前就有值，不会先按 100dvh 画一帧再跳。桌面入口不 import 本文件。
export function installVisualViewport(): () => void {
  const vv = window.visualViewport
  if (!vv) return () => {}

  const root = document.documentElement
  let raf = 0

  const apply = (): void => {
    raf = 0
    // vv.height 偶发为 0（切后台/过渡帧）：回落到 layout viewport 高度，
    // 不要把上一帧的 --m-vv-height 留在那里（否则壳会一直停在旧高度）。
    const height = Math.round(vv.height) || Math.round(window.innerHeight)
    const offset = Math.round(vv.offsetTop)
    root.style.setProperty('--m-vv-height', `${height}px`)
    root.style.setProperty('--m-vv-offset', `${offset}px`)
  }

  // resize/scroll 在 iOS 上会连着来很多帧：合并到下一帧，只写一次变量。
  const schedule = (): void => {
    if (raf) return
    raf = requestAnimationFrame(apply)
  }

  apply()
  vv.addEventListener('resize', schedule)
  vv.addEventListener('scroll', schedule)
  // 横竖屏切换时部分浏览器只发 orientationchange，不发 visualViewport resize。
  window.addEventListener('orientationchange', schedule)

  return () => {
    if (raf) cancelAnimationFrame(raf)
    vv.removeEventListener('resize', schedule)
    vv.removeEventListener('scroll', schedule)
    window.removeEventListener('orientationchange', schedule)
    root.style.removeProperty('--m-vv-height')
    root.style.removeProperty('--m-vv-offset')
  }
}
