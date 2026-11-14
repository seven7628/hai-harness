#!/usr/bin/env node
// 手机新页静态验收（P8-03b）：不需要浏览器、不需要构建，Node 直跑。
//
// 为什么需要它：旧页（bridge/mobile_web 的手写 PWA）退役后，原来挂在
// test:mobile-hub / test:mobile-390 上的**静态**约束不能一起消失 —— 那些断言
// 守住的是「页面能开、不发散、不拼 HTML、触控够大」，与用哪套框架无关。
// 浏览器层的真机行为仍由 check:mobile-hub / check:mobile-session 覆盖。
//
// 覆盖：
//   A. 页面外壳：viewport（刘海 + Android 键盘）、theme-color、#root、模块入口；
//   B. 不拼 HTML：src/mobile/** 没有 innerHTML / dangerouslySetInnerHTML；
//   C. 令牌同源：mobile.css 只 @import 桌面令牌/hub/skin，不另起色板（替代已删除的
//      tokens_sync_test.go）；
//   D. 传输契约：配对/命令/事件端点与凭据 key（旧页配对过的手机不掉档）；
//   E. 旧页退役：mobile_web/ 目录不存在、代码里没有旧文件引用、内置 zip 结构正确；
//   F. 触控与动效：44px 触控下限、prefers-reduced-motion 有兜底。
import { readFileSync, readdirSync, existsSync, statSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const REPO = resolve(APP, '..', '..')
const SRC = join(APP, 'src', 'mobile')
const BRIDGE = join(REPO, 'desktop', 'bridge')
const ZIP = join(BRIDGE, 'mobile_web.zip')

let pass = 0
const failures = []
function check(name, fn) {
  try {
    fn()
    pass++
    console.log(`  ✅ ${name}`)
  } catch (e) {
    failures.push(`${name}: ${e.message}`)
    console.error(`  ❌ ${name}: ${e.message}`)
  }
}
function assert(cond, msg) {
  if (!cond) throw new Error(msg)
}

function walk(dir) {
  const out = []
  for (const name of readdirSync(dir)) {
    const full = join(dir, name)
    if (statSync(full).isDirectory()) out.push(...walk(full))
    else out.push(full)
  }
  return out
}
const srcFiles = walk(SRC).map((full) => ({ rel: relative(APP, full), text: readFileSync(full, 'utf8') }))
const mobileHtml = readFileSync(join(APP, 'mobile.html'), 'utf8')
const mobileCss = readFileSync(join(SRC, 'mobile.css'), 'utf8')

// ── A. 页面外壳 ────────────────────────────────────────────────
check('mobile.html: viewport 带 viewport-fit=cover + interactive-widget', () => {
  const viewport = /<meta name="viewport" content="([^"]+)"/.exec(mobileHtml)
  assert(viewport, '缺 <meta name="viewport">')
  assert(/viewport-fit=cover/.test(viewport[1]), '缺 viewport-fit=cover（刘海/安全区）')
  assert(/interactive-widget=resizes-content/.test(viewport[1]), '缺 interactive-widget=resizes-content（Android 键盘）')
  assert(!/user-scalable\s*=\s*no/.test(viewport[1]), '不许禁双指放大（无障碍底线）')
})
check('mobile.html: theme-color / #root / 模块入口 / zh-CN', () => {
  assert(/name="theme-color"/.test(mobileHtml), '缺 meta theme-color')
  assert(/<div id="root"><\/div>/.test(mobileHtml), '缺 #root 挂载点')
  assert(/type="module" src="\/src\/mobile\/main\.tsx"/.test(mobileHtml), '缺模块入口 src/mobile/main.tsx')
  assert(/lang="zh-CN"/.test(mobileHtml), 'lang 应为 zh-CN')
  assert(/data-skin="runtime"/.test(mobileHtml), '手机固定 data-skin=runtime')
})
check('mobile.html: 不静态加载字体（首帧后由 main.tsx 注入）', () => {
  assert(!/fonts\.googleapis\.com\/css/.test(mobileHtml), '字体 CSS 不能静态 link（阻塞首屏）')
})

// ── B. 不拼 HTML ──────────────────────────────────────────────
check('src/mobile/**: 没有 innerHTML / insertAdjacentHTML / dangerouslySetInnerHTML', () => {
  const hits = []
  for (const f of srcFiles) {
    for (const re of [/\.innerHTML\b/, /insertAdjacentHTML/, /dangerouslySetInnerHTML/, /document\.write\(/]) {
      if (re.test(f.text)) hits.push(`${f.rel} → ${re}`)
    }
  }
  assert(hits.length === 0, `发现拼 HTML：${hits.join('; ')}`)
})

// ── C. 令牌同源（替代 tokens_sync_test.go）────────────────────
check('mobile.css: 只 @import 桌面令牌层，不复制色板', () => {
  for (const dep of ['../runtime-palette.css', '../runtime-tokens.css', '../hub/hub.css', '../runtime-skin.css']) {
    assert(mobileCss.includes(`@import '${dep}'`), `缺 @import '${dep}'`)
  }
  assert(!mobileCss.includes('@import \'../index.css\''), '手机壳不该引 index.css（13k 行桌面三栏布局）')
})
check('mobile.css: 不另起字体族（沿用桌面 --font-*）', () => {
  const families = [...mobileCss.matchAll(/font-family:\s*([^;]+);/g)].map((m) => m[1].trim())
  const stray = families.filter((f) => !f.startsWith('var(') && !/^inherit$/.test(f))
  assert(stray.length === 0, `font-family 直接写死了字体：${stray.join(' / ')}`)
})

// ── D. 传输契约 ───────────────────────────────────────────────
check('transport: 配对/命令/事件端点与凭据 key 不变', () => {
  const ts = readFileSync(join(SRC, 'transport', 'mobile.ts'), 'utf8')
  for (const s of ["'/api/pair'", "'/api/cmd'", '/api/events', "'hai.token'", "'hai.mobile.token'"]) {
    assert(ts.includes(s), `缺 ${s}`)
  }
  assert(ts.includes('scope=hub') || ts.includes("scope: 'hub'"), 'Hub 事件流应带 scope=hub')
})

// ── E. 旧页退役 ───────────────────────────────────────────────
check('旧页目录 desktop/bridge/mobile_web/ 已删除', () => {
  assert(!existsSync(join(BRIDGE, 'mobile_web')), '旧页目录还在')
})
check('代码里没有旧页文件引用（注释/脚本一起清）', () => {
  const oldNames = ['mobile_web/app.js', 'mobile_web/hub_model.js', 'mobile_web/avatar_model.js',
    'mobile_web/tokens.css', 'mobile_web/style.css', 'mobile_web/index.html']
  const dirs = [join(APP, 'src'), join(APP, 'scripts'), join(APP, 'electron'), BRIDGE]
  const hits = []
  for (const dir of dirs) {
    for (const full of walk(dir)) {
      if (!/\.(ts|tsx|mjs|js|go|css|html)$/.test(full)) continue
      if (full === fileURLToPath(import.meta.url)) continue
      const text = readFileSync(full, 'utf8')
      for (const name of oldNames) if (text.includes(name)) hits.push(`${relative(REPO, full)} → ${name}`)
    }
  }
  assert(hits.length === 0, `还有旧页引用：${hits.join('; ')}`)
})

// zip 中央目录（不引第三方依赖，只读名字）
function zipEntryNames(buf) {
  let eocd = -1
  const floor = Math.max(0, buf.length - 22 - 0xffff)
  for (let i = buf.length - 22; i >= floor; i--) {
    if (buf.readUInt32LE(i) === 0x06054b50) { eocd = i; break }
  }
  if (eocd < 0) throw new Error('不是 zip（找不到 EOCD）')
  const count = buf.readUInt16LE(eocd + 10)
  let off = buf.readUInt32LE(eocd + 16)
  const names = []
  for (let i = 0; i < count; i++) {
    if (buf.readUInt32LE(off) !== 0x02014b50) throw new Error(`中央目录第 ${i} 项损坏`)
    const nameLen = buf.readUInt16LE(off + 28)
    const extraLen = buf.readUInt16LE(off + 30)
    const commentLen = buf.readUInt16LE(off + 32)
    names.push(buf.toString('utf8', off + 46, off + 46 + nameLen))
    off += 46 + nameLen + extraLen + commentLen
  }
  return names
}

check('mobile_web.zip: 有 index.html + assets，且没有旧页文件', () => {
  assert(existsSync(ZIP), '缺 desktop/bridge/mobile_web.zip（先跑 npm run mobile:embed）')
  const buf = readFileSync(ZIP)
  assert(buf.length < 16 * 1024 * 1024, `zip ${(buf.length / 1024 / 1024).toFixed(1)}MB 过大`)
  const names = zipEntryNames(buf)
  assert(names.includes('index.html'), 'zip 根缺 index.html（/m/ 只能靠它兜底）')
  assert(names.some((n) => n.startsWith('assets/') && n.endsWith('.js')), 'zip 里没有 assets/*.js')
  assert(names.some((n) => n.startsWith('assets/') && n.endsWith('.css')), 'zip 里没有 assets/*.css')
  for (const gone of ['mobile.html', 'app.js', 'hub_model.js', 'tokens.css', 'style.css']) {
    assert(!names.includes(gone), `zip 里还有旧页文件 ${gone}`)
  }
  for (const n of names) {
    assert(!n.startsWith('/') && !n.split('/').includes('..') && !n.includes('\\'), `zip 条目路径非法：${n}`)
  }
})
check('bridge 只 embed zip，不再 embed 目录', () => {
  const go = readFileSync(join(BRIDGE, 'mobile_web.go'), 'utf8')
  assert(/\/\/go:embed mobile_web\.zip/.test(go), 'go:embed 应指向 mobile_web.zip')
  assert(!/\/\/go:embed mobile_web\n/.test(go), '仍在 embed 旧目录')
})

// ── F. 触控与动效 ─────────────────────────────────────────────
check('mobile.css: 44px 触控下限与 reduced-motion 兜底', () => {
  assert(/44px/.test(mobileCss), '缺 44px 触控规则')
  assert(/prefers-reduced-motion/.test(mobileCss), '缺 prefers-reduced-motion 兜底')
})

if (failures.length) {
  console.error(`\n手机静态验收失败 ${failures.length} 项`)
  process.exit(1)
}
console.log(`\n手机静态验收通过（${pass} 项）`)
