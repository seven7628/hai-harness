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
//   E. 旧页退役 + 方案 A：mobile_web/ 与 zip 都不存在；手机页是 dist-mobile 目录，
//      package.json 的 extraResources 把它复制成 Resources/mobile-web；
//   F. 触控与动效：44px 触控下限、prefers-reduced-motion 有兜底。
import { readFileSync, readdirSync, existsSync, statSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const REPO = resolve(APP, '..', '..')
const SRC = join(APP, 'src', 'mobile')
const BRIDGE = join(REPO, 'desktop', 'bridge')

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

check('方案 A：没有 mobile_web 目录、没有 mobile_web.zip', () => {
  assert(!existsSync(join(BRIDGE, 'mobile_web')), '旧页目录还在')
  assert(!existsSync(join(BRIDGE, 'mobile_web.zip')), 'zip 还在（方案 A 不应有运行时解压缓存）')
})

check('package.json：dist-mobile 作为 extraResources → Resources/mobile-web', () => {
  const pkg = JSON.parse(readFileSync(join(APP, 'package.json'), 'utf8'))
  const extra = pkg.build?.extraResources ?? []
  assert(extra.some((x) => x.from === 'dist-mobile' && x.to === 'mobile-web'), 'extraResources 缺 dist-mobile → mobile-web')
  assert(String(pkg.scripts?.build || '').includes('mobile:build'), 'build 应先跑 mobile:build')
  assert(!String(pkg.scripts?.build || '').includes('mobile:embed'), 'build 不应再跑 mobile:embed')
  assert(String(pkg.scripts?.['mobile:build'] || '').includes('mobile-dist-index.mjs'), 'mobile:build 应把 mobile.html 改成 index.html')
})

check('electron/main.ts：dev 与打包态都设置 HAI_MOBILE_WEB_DIR', () => {
  const ts = readFileSync(join(APP, 'electron', 'main.ts'), 'utf8')
  assert(ts.includes('bundledMobileWebDir'), '缺 bundledMobileWebDir')
  assert(ts.includes("path.join(process.resourcesPath, 'mobile-web')"), '打包态应指向 Resources/mobile-web')
  assert(ts.includes("path.join(APP_ROOT, 'dist-mobile')"), 'dev 应指向 dist-mobile')
  assert(ts.includes('env.HAI_MOBILE_WEB_DIR = bundledMobileWebDir()'), 'bridgeEnv 应设置 HAI_MOBILE_WEB_DIR')
})

check('bridge：/m/ 只服务磁盘目录，不再 embed zip', () => {
  const go = readFileSync(join(BRIDGE, 'mobile_server.go'), 'utf8')
  assert(go.includes('func mobileWebSub(diskDir string)'), '缺 mobileWebSub')
  assert(go.includes('HAI_MOBILE_WEB_DIR 未设置'), '缺缺失目录的明确错误')
  assert(!existsSync(join(BRIDGE, 'mobile_web.go')), 'mobile_web.go 应已删除')
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
