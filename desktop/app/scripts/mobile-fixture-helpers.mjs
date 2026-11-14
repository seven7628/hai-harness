// 手机浏览器夹具的公共工具（P8-03a）。
//
// check-mobile-hub.mjs 与 check-mobile-session.mjs 共用同一套判据，避免「两个夹具
// 各写一份对比度/键盘模拟」慢慢漂移。这里只放与页面无关的注入/采样，不放 mock 数据
// （两个夹具的 bridge 数据形状不同，各自维护）。
//
// 依赖：调用方已 import { chromium } from 'playwright'；本文件不自己起浏览器。

/** 主题：在页面脚本之前写 hai.theme（旧页/新页同一个 key），boot 时 applyTheme 读取。 */
export async function installTheme(context, theme) {
  await context.addInitScript((t) => {
    try {
      localStorage.setItem('hai.theme', t)
    } catch {
      /* 隐私模式：跟随系统，断言会提前失败并说明原因 */
    }
  }, theme)
}

/**
 * visualViewport 假体：height 默认取 innerHeight（= 无键盘），测试里改成 400 再
 * dispatch resize，模拟 iOS「layout viewport 不变、可见区变矮」。真机不需要这个假体；
 * 夹具这样做是为了在 Chromium 里证明 composer 跟着可见区走，而不是只靠
 * setViewportSize（那会把 layout viewport 一起缩，100dvh 本来就会动）。
 */
export async function installFakeVisualViewport(context) {
  await context.addInitScript(() => {
    const fake = new EventTarget()
    let h = 0
    Object.defineProperty(fake, 'height', {
      configurable: true,
      get: () => h || window.innerHeight,
      set: (v) => {
        h = v
      },
    })
    fake.width = window.innerWidth
    fake.offsetTop = 0
    fake.offsetLeft = 0
    fake.scale = 1
    Object.defineProperty(window, 'visualViewport', { configurable: true, get: () => fake })
  })
}

/**
 * 对比度采样（WCAG 相对亮度）：只量「文字色 vs 最近的不透明祖先底色」，断言两套主题
 * 下都不是白字白底/黑字黑底。color-mix 的计算值在 Chromium 里可能是 rgb() 也可能是
 * color(srgb …)，两种都解析。阈值 4.5:1 = WCAG AA 正文口径。
 */
export async function auditContrast(page, pairs, label) {
  const rows = await page.evaluate((pairs) => {
    const parse = (s) => {
      const m = /rgba?\(([^)]+)\)/.exec(s || '')
      if (m) {
        const p = m[1].split(/[,\s/]+/).filter(Boolean).map(Number)
        return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 }
      }
      const cm = /color\(srgb\s+([\d.]+)\s+([\d.]+)\s+([\d.]+)(?:\s*\/\s*([\d.]+))?\)/.exec(s || '')
      if (cm) return { r: Number(cm[1]) * 255, g: Number(cm[2]) * 255, b: Number(cm[3]) * 255, a: cm[4] ? Number(cm[4]) : 1 }
      return null
    }
    const lum = ({ r, g, b }) => {
      const f = (v) => {
        v /= 255
        return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4)
      }
      return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b)
    }
    const ratio = (a, b) => {
      const l1 = lum(a)
      const l2 = lum(b)
      const hi = Math.max(l1, l2)
      const lo = Math.min(l1, l2)
      return (hi + 0.05) / (lo + 0.05)
    }
    const bgOf = (el) => {
      let n = el
      while (n && n !== document.documentElement) {
        const c = parse(getComputedStyle(n).backgroundColor)
        if (c && c.a > 0.5) return c
        n = n.parentElement
      }
      const body = parse(getComputedStyle(document.body).backgroundColor)
      return body && body.a > 0.5 ? body : { r: 255, g: 255, b: 255, a: 1 }
    }
    return pairs.map(([sel, name]) => {
      const el = document.querySelector(sel)
      if (!el) return { name, missing: true }
      const cs = getComputedStyle(el)
      const fg = parse(cs.color)
      const bg = bgOf(el)
      const r = fg && bg ? ratio(fg, bg) : 0
      return {
        name,
        fg: cs.color,
        bg: `rgb(${Math.round(bg.r)}, ${Math.round(bg.g)}, ${Math.round(bg.b)})`,
        ratio: Math.round(r * 100) / 100,
      }
    })
  }, pairs)
  const missing = rows.filter((r) => r.missing)
  if (missing.length) throw new Error(`${label}: 对比度采样选择器不存在 ${JSON.stringify(missing)}`)
  const low = rows.filter((r) => r.ratio < 4.5)
  if (low.length) throw new Error(`${label}: 对比度 <4.5:1 ${JSON.stringify(low)}`)
  console.log(`  ✅ ${label}: 对比度 ${rows.map((r) => `${r.name}=${r.ratio}:1`).join(' ')}`)
  return rows
}
