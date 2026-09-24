// 生成 macOS 应用图标资源（2026-09-24）
//   build/icon-source.svg  →  build/icon.iconset/*.png  →  build/HAI.icns
//
// 为什么用 Chromium 而不是 qlmanage/rsvg：本机没有 SVG 光栅化 CLI，而 playwright
// 已是 devDependency。Chromium 对每个尺寸**直接按目标分辨率矢量光栅化**（不做先大后小的
// 重采样），边缘抗锯齿质量最好。
//
// 用法：node build/make-icons.mjs [--preview]
//   --preview  额外写一张 icon-preview.png（各尺寸放大对照，肉眼检查用）
import { chromium } from 'playwright'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const withPreview = process.argv.includes('--preview')

const base = path.dirname(fileURLToPath(import.meta.url))
const svgSource = fs.readFileSync(path.join(base, 'icon-source.svg'), 'utf8')
const iconset = path.join(base, 'icon.iconset')

// 与 icon-source.svg 中 #markLayer 的几何保持同一套换算：
// 品牌 viewBox 560×380，墨迹包围盒 x 89..473（宽 384）、y 59..311（高 252），中心 (281,185)
const INK_W = 384
const INK_CX = 281
const INK_CY = 185
const TILE = 824
const CANVAS = 1024

/** logo 墨迹宽度占圆角方边长的比例 → [scale, translateX, translateY] */
function markTransform (inkFraction) {
  const scale = (inkFraction * TILE) / INK_W
  return {
    scale,
    x: CANVAS / 2 - INK_CX * scale,
    y: CANVAS / 2 - INK_CY * scale
  }
}

// 同一枚图标，按尺寸分三档画法（与 macOS 惯例一致：Apple 自己也给每个尺寸单独出图）。
// 不这么做的话，logo 笔画在 32px 下只有 0.86px 宽，整枚图标会糊成一团灰。
//   inkFraction  墨迹宽度 / 圆角方边长（824）
//   strokeWidth  品牌 viewBox(560×380) 单位
//   simplify     去掉投影与顶部柔光（缩小后只剩脏），并把内描边加粗到看得见
const TIERS = [
  { max: 32, inkFraction: 0.78, strokeWidth: 48, simplify: true }, // 16·32：可读性优先
  { max: 64, inkFraction: 0.66, strokeWidth: 30, simplify: false }, // 64：略加粗，其余照原稿
  { max: Infinity, inkFraction: 0.62, strokeWidth: 22, simplify: false } // 128+：品牌原稿笔重
]

const tierFor = size => TIERS.find(t => size <= t.max)

// iconset 文件名 → ICNS 条目类型（PNG 载荷：小尺寸 icp4-6/ic11-12，大尺寸 ic07-14）
const ENTRIES = [
  ['icon_16x16.png', 'icp4', 16],
  ['icon_16x16@2x.png', 'ic11', 32],
  ['icon_32x32.png', 'icp5', 32],
  ['icon_32x32@2x.png', 'ic12', 64],
  ['icon_128x128.png', 'ic07', 128],
  ['icon_128x128@2x.png', 'ic13', 256],
  ['icon_256x256.png', 'ic08', 256],
  ['icon_256x256@2x.png', 'ic14', 512],
  ['icon_512x512.png', 'ic09', 512],
  ['icon_512x512@2x.png', 'ic10', 1024]
]

/** 按目标尺寸渲染 icon-source.svg，返回 PNG buffer */
async function render (page, size) {
  const tier = tierFor(size)
  await page.setViewportSize({ width: size, height: size })
  await page.setContent(
    `<!doctype html><html><body style="margin:0;background:transparent">` +
      svgSource.replace(/width="1024" height="1024"/, `width="${size}" height="${size}"`) +
      `</body></html>`,
    { waitUntil: 'load' }
  )
  const t = markTransform(tier.inkFraction)
  await page.evaluate(
    ({ simplify, transform, strokeWidth }) => {
      const set = (id, fn) => { const el = document.getElementById(id); if (el) fn(el) }
      if (simplify) {
        set('shadowLayer', el => el.remove())
        set('glowLayer', el => el.remove())
        set('rimLayer', el => el.firstElementChild.setAttribute('stroke-width', '12'))
      }
      set('markLayer', el => el.setAttribute('transform', transform))
      set('mark', el => el.setAttribute('stroke-width', String(strokeWidth)))
    },
    {
      simplify: tier.simplify,
      transform: `translate(${t.x.toFixed(2)},${t.y.toFixed(2)}) scale(${t.scale.toFixed(5)})`,
      strokeWidth: tier.strokeWidth
    }
  )
  return page.screenshot({ omitBackground: true })
}

/** 手工拼 ICNS 容器（iconutil 在本机沙箱里生成不了临时文件）：'icns' 头 + 总长 + [type+len+PNG] */
function packIcns (pngs) {
  const chunks = pngs.map(([type, png]) => {
    const head = Buffer.alloc(8)
    head.write(type, 0, 4, 'ascii')
    head.writeUInt32BE(8 + png.length, 4)
    return Buffer.concat([head, png])
  })
  const total = 8 + chunks.reduce((a, b) => a + b.length, 0)
  const head = Buffer.alloc(8)
  head.write('icns', 0, 4, 'ascii')
  head.writeUInt32BE(total, 4)
  return { buf: Buffer.concat([head, ...chunks]), total }
}

const browser = await chromium.launch()
const page = await browser.newPage({ deviceScaleFactor: 1 })

fs.rmSync(iconset, { recursive: true, force: true })
fs.mkdirSync(iconset, { recursive: true })

const cache = new Map()
const pngs = []
for (const [name, type, size] of ENTRIES) {
  if (!cache.has(size)) cache.set(size, await render(page, size))
  const png = cache.get(size)
  fs.writeFileSync(path.join(iconset, name), png)
  pngs.push([type, png])
  console.log(`  ${name.padEnd(20)} ${size}×${size}  ${String(png.length).padStart(7)} B  ${tierFor(size).simplify ? '简化画法' : ''}`)
}

const { buf, total } = packIcns(pngs)
fs.writeFileSync(path.join(base, 'HAI.icns'), buf)
fs.copyFileSync(path.join(iconset, 'icon_512x512@2x.png'), path.join(base, 'icon-1024.png'))
console.log(`\nHAI.icns  ${total} B, ${pngs.length} 条目\nicon-1024.png  1024×1024（预览用）`)

if (withPreview) {
  // 各尺寸并排对照：<128 的按整数倍像素放大（nearest）看实际像素，≥128 的原尺寸看整体
  const SHOWN = 128
  const cells = ENTRIES
    .filter(([name]) => !name.endsWith('@2x.png') || name.startsWith('icon_512'))
    .map(([name, , size]) => {
      const b64 = fs.readFileSync(path.join(iconset, name)).toString('base64')
      const up = size < SHOWN
      return `<figure><img src="data:image/png;base64,${b64}" style="width:${SHOWN}px;height:${SHOWN}px;image-rendering:${up ? 'pixelated' : 'auto'}">` +
        `<figcaption>${size}px${up ? ` · ${SHOWN / size}× 放大` : ''}</figcaption></figure>`
    }).join('')
  await page.setViewportSize({ width: 60 + (SHOWN + 22) * 7, height: 200 })
  await page.setContent(`<!doctype html><meta charset="utf-8"><style>
    body{margin:0;padding:24px;background:#8d8d93;font:12px -apple-system,sans-serif;color:#fff}
    .row{display:flex;gap:22px}figure{margin:0;text-align:center}img{display:block}
    figcaption{margin-top:8px;opacity:.9}</style><div class="row">${cells}</div>`, { waitUntil: 'load' })
  await page.screenshot({ path: path.join(base, 'icon-preview.png'), fullPage: true })
  console.log('icon-preview.png  各尺寸对照')
}

await browser.close()
