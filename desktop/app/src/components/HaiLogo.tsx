// HAI 品牌视觉（brand/hai-logo.html 定稿，2026-08-14）：
// 直接用设计稿提取的 SVG 文件（src/assets/hai-logo-{light,dark}.svg），按主题选版本：
//   ink（夜墨/深色）= hai-logo-dark.svg；paper（宣纸/亮色）= hai-logo-light.svg。
// 字标 = 毛笔字 HAI（Nanum Brush Script）+ 朱砂「嗨」印章（Zhi Mang Xing），与 hai-logo.html 同款 CSS 排版。
import { useEffect, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import logoLight from '../assets/hai-logo-light.svg'
import logoDark from '../assets/hai-logo-dark.svg'

// viewBox 560×380，logo 始终按此比例渲染（显式写 width/height，压过 Tailwind preflight 的 img 规则）
const RATIO = 560 / 380

function resolve(): 'ink' | 'paper' {
  const pref = useAppStore.getState().settings.theme
  if (pref === 'system') {
    return window.matchMedia('(prefers-color-scheme: light)').matches ? 'paper' : 'ink'
  }
  return pref === 'light' ? 'paper' : 'ink'
}

export default function HaiLogo({ width, height, className }: { width?: number; height?: number; className?: string }) {
  const pref = useAppStore((s) => s.settings.theme)
  const [theme, setTheme] = useState<'ink' | 'paper'>(resolve)

  useEffect(() => {
    const update = () => setTheme(resolve())
    update()
    if (pref === 'system') {
      const mq = window.matchMedia('(prefers-color-scheme: light)')
      mq.addEventListener('change', update)
      return () => mq.removeEventListener('change', update)
    }
  }, [pref])

  const w = width ?? (height ? Math.round(height * RATIO) : undefined)
  const h = height ?? (width ? Math.round(width / RATIO) : undefined)

  return (
    <img
      src={theme === 'ink' ? logoDark : logoLight}
      width={w}
      height={h}
      style={w != null && h != null ? { width: w, height: h } : undefined}
      className={className}
      alt="HAI"
      draggable={false}
    />
  )
}

export function HaiWordmark({ size = 44 }: { size?: number }) {
  return (
    <span className="wordmark" style={{ fontSize: size }}>
      <span className="wH">H</span>
      <span className="wAI">AI</span>
      <span className="seal">嗨</span>
    </span>
  )
}
