// 生成 DMG 安装窗口的底图（2026-09-24）
//   brand/hai-logo-light.svg  →  build/dmg-background.png（700×460，1x）
//                                build/dmg-background@2x.png（1400×920，Retina）
//
// electron-builder 的行为（node_modules/dmg-builder/out/dmgUtil.js）：
//   - dmg.background 指到 dmg-background.png，会自动找同目录的 @2x 并用
//     `tiffutil -cathidpicheck` 合成 Retina TIFF；
//   - **底图的像素尺寸就是窗口尺寸**（所以 window 不必另配）；
//   - dmg.contents 的 x/y 是「内容区左上角 → 图标中心」的设备无关像素。
//
// ⚠️ 底图从**内容区**左上角 1:1 铺开，而内容区 ≠ 窗口：Finder 的标题栏（28px）
//    与标签页栏（28px）会盖住底图最上面的 CHROME 像素，底图最下面的 CHROME 像素
//    则被窗口下沿裁掉（实测：窗口 600×400 时内容区 600×344，提示文字正压在裁切线上）。
//    底图仍按窗口尺寸出图（这样 chrome 高度变化时也能铺满），但可见元素一律排在
//    y < H - CHROME 之内，见下方自检。
//
// ⚠️ 改 APP_ICON / LINK_ICON 必须同步改 desktop/app/package.json 的 dmg.contents，
//    否则箭头会指偏。脚本结束会打印当前应填的坐标。
//
// 用浅色底的原因：应用图标是深色的（夜墨版），深底上会糊在一起。
// 配色取品牌「宣纸」亮色版，字标直接用 brand/hai-logo-light.svg，不另抄一份几何。
//
// 用法：node build/make-dmg-background.mjs
import { chromium } from 'playwright'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const base = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(base, '../../..')

// ── 版面（单位：1x 像素；底图坐标 = 内容区坐标 = dmg.contents 坐标）────────
const W = 700
const H = 460
const CHROME = 56 // 标题栏 28 + 标签页栏 28
const VISIBLE_H = H - CHROME // 实际可见的内容区高度：404
const APP_ICON = { x: 200, y: 236 } // 应用图标中心
const LINK_ICON = { x: 500, y: 236 } // Applications 替身图标中心
const ICON_SIZE = 128 // 与 package.json 的 dmg.iconSize 一致
const MARK = { cx: W / 2, cy: 96, inkWidth: 120 } // 顶部品牌字标
const RULE = { cx: W / 2, cy: 44, halfWidth: 52 } // 字标上方的朱砂细线
const HINT = { cx: W / 2, cy: VISIBLE_H - 30, text: '拖入 Applications 文件夹即可安装' } // 距内容区下沿 30px

// 自检：可见元素不能越过内容区下沿（提示文字基线 + 下伸部约 4px）
if (HINT.cy + 4 > VISIBLE_H - 20) {
  throw new Error(`提示文字 y=${HINT.cy} 离内容区下沿（${VISIBLE_H}）不足 20px，会被裁掉`)
}
if (APP_ICON.y + ICON_SIZE / 2 + 30 > VISIBLE_H || LINK_ICON.y + ICON_SIZE / 2 + 30 > VISIBLE_H) {
  throw new Error(`图标 + 名称标签超出内容区下沿（${VISIBLE_H}）`)
}

const INK_W = 384 // brand 字标墨迹包围盒：x 89..473、y 59..311，中心 (281,185)
const INK_CX = 281
const INK_CY = 185
const VIEW_W = 560
const VIEW_H = 380

// 品牌亮色字标：只取 <svg> 内部内容，外层的尺寸/视图框由这里重新指定
const logoSvg = fs.readFileSync(path.join(repoRoot, 'brand/hai-logo-light.svg'), 'utf8')
const logoInner = logoSvg.replace(/^[\s\S]*?<svg[^>]*>/, '').replace(/<\/svg>[\s\S]*$/, '').trim()

const markScale = MARK.inkWidth / INK_W
const markX = MARK.cx - INK_CX * markScale
const markY = MARK.cy - INK_CY * markScale

// 两枚图标之间的箭头
const arrowLeft = APP_ICON.x + ICON_SIZE / 2 + 16
const arrowRight = LINK_ICON.x - ICON_SIZE / 2 - 16
const arrowY = (APP_ICON.y + LINK_ICON.y) / 2

const n = v => String(Math.round(v * 100) / 100)

const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}" viewBox="0 0 ${W} ${H}">
  <!-- HAI DMG 安装窗口底图 · 宣纸亮色底 + 品牌字标 + 拖拽箭头
       ⚠️ 图标中心坐标与 desktop/app/package.json 的 dmg.contents 一一对应，改一处必须改另一处。 -->
  <defs>
    <linearGradient id="paper" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="#FCFAF7"/>
      <stop offset="1" stop-color="#EFE9DF"/>
    </linearGradient>
    <radialGradient id="sheen" cx="0.5" cy="0.02" r="0.85">
      <stop offset="0" stop-color="#ffffff" stop-opacity="0.85"/>
      <stop offset="1" stop-color="#ffffff" stop-opacity="0"/>
    </radialGradient>
  </defs>

  <rect width="${W}" height="${H}" fill="url(#paper)"/>
  <rect width="${W}" height="${H}" fill="url(#sheen)"/>

  <!-- 顶部一条朱砂细线，收住标题栏下方的留白 -->
  <rect x="${n(RULE.cx - RULE.halfWidth)}" y="${n(RULE.cy)}" width="${n(RULE.halfWidth * 2)}" height="2.5" rx="1.25" fill="#C23A2E" opacity="0.75"/>

  <!-- 品牌字标（亮色版：浓墨 / 朱砂 / 淡墨） -->
  <g transform="translate(${n(markX)},${n(markY)}) scale(${n(markScale)})">${logoInner}</g>

  <!-- 拖拽箭头 -->
  <g fill="none" stroke="#C7BCB0" stroke-width="4" stroke-linecap="round" stroke-linejoin="round">
    <path d="M ${n(arrowLeft)} ${n(arrowY)} H ${n(arrowRight)}"/>
    <path d="M ${n(arrowRight - 17)} ${n(arrowY - 17)} L ${n(arrowRight)} ${n(arrowY)} L ${n(arrowRight - 17)} ${n(arrowY + 17)}"/>
  </g>

  <text x="${n(HINT.cx)}" y="${n(HINT.cy)}" text-anchor="middle"
        font-family="-apple-system, BlinkMacSystemFont, 'PingFang SC', 'Helvetica Neue', sans-serif"
        font-size="13" fill="#A79E95" letter-spacing="0.2">${HINT.text}</text>
</svg>
`

const browser = await chromium.launch()
const page = await browser.newPage({ deviceScaleFactor: 1 })
for (const [suffix, scale] of [['', 1], ['@2x', 2]]) {
  await page.setViewportSize({ width: W * scale, height: H * scale })
  await page.setContent(
    `<!doctype html><html><body style="margin:0">` +
      svg.replace(`width="${W}" height="${H}"`, `width="${W * scale}" height="${H * scale}"`) +
      `</body></html>`,
    { waitUntil: 'load' }
  )
  const out = path.join(base, `dmg-background${suffix}.png`)
  await page.screenshot({ path: out })
  console.log(`  dmg-background${suffix}.png  ${W * scale}×${H * scale}`)
}
await browser.close()

console.log(`\npackage.json 的 dmg 配置应对应：`)
console.log(`  "window":   { "width": ${W}, "height": ${H} }   // 内容区 ${W}×${VISIBLE_H}（标题栏+标签页栏占 ${CHROME}px）`)
console.log(`  "iconSize": ${ICON_SIZE}`)
console.log(`  "contents": [ { "x": ${APP_ICON.x}, "y": ${APP_ICON.y} }, { "x": ${LINK_ICON.x}, "y": ${LINK_ICON.y}, "type": "link", "path": "/Applications" } ]`)
