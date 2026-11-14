#!/usr/bin/env node
// 手机新页 → bridge 内置资产（P8-03b）。
//
// 做三件事：
//   1. `MOBILE_ONLY=1 vite build`（产物 dist-mobile/，gitignore）；
//   2. dist-mobile/mobile.html → index.html：bridge 用 http.FileServer 托管 /m/，
//      根路径只能靠 index.html 兜底（旧页就是这么放的）；
//   3. 打包 desktop/bridge/mobile_web.zip：go:embed 进 bridge 二进制，`go build`
//      在干净检出也能出带新页的 /m/。
//
// 为什么是 zip 而不是直接 embed 18MB 原始产物：embed 目录里 452 个文件全进 git，
// 一次构建改哈希名就是几百个删除+新增；zip 是单文件、约 5MB，diff 也只一行。
//
// 确定性：文件顺序（字典序）、mtime（固定 2020-01-01 UTC）、-X 去扩展属性、-D 不写目录项。
// 同一份 dist-mobile 重跑两次得到同一个 SHA-256 —— 否则每次构建都会让 zip 变脏。
//
// 只支持 macOS/Linux（用系统 zip）。仓库当前只做 macOS 开发，Windows 需另找 zip 实现。
//
// 用法：
//   node scripts/build-mobile-embed.mjs              # 构建 + 打包（npm run mobile:embed）
//   node scripts/build-mobile-embed.mjs --skip-build # 复用现有 dist-mobile，只重打包
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import {
  existsSync, mkdirSync, readdirSync, readFileSync, renameSync, rmSync, statSync, utimesSync,
} from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const DIST = join(APP, 'dist-mobile')
const OUT = join(APP, '..', 'bridge', 'mobile_web.zip')
// 固定时间戳：zip 只存到 2 秒精度，选一个 DOS 时间能表示的值。
const FIXED_MTIME = new Date('2020-01-01T00:00:00Z')
const MAX_ZIP_BYTES = 16 * 1024 * 1024 // 超过说明把原始产物塞进去了，别提交
const skipBuild = process.argv.includes('--skip-build')
const tmpZip = `${OUT}.tmp-${process.pid}`

function die(msg) {
  rmSync(tmpZip, { force: true })
  console.error(`mobile:embed ❌ ${msg}`)
  process.exit(1)
}

function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { stdio: 'inherit', ...opts })
  if (r.error) die(`${cmd} 启动失败：${r.error.message}`)
  if (r.status !== 0) die(`${cmd} ${args.join(' ')} 退出码 ${r.status}`)
}

// 1) Vite 构建手机入口
if (!skipBuild) {
  const vite = join(APP, 'node_modules', 'vite', 'bin', 'vite.js')
  if (!existsSync(vite)) die(`找不到 ${vite} —— 先在 desktop/app 跑 npm install`)
  console.log('mobile:embed · MOBILE_ONLY=1 vite build')
  run(process.execPath, [vite, 'build'], { cwd: APP, env: { ...process.env, MOBILE_ONLY: '1' } })
}

// 2) mobile.html → index.html（/m/ 根路径靠它）
const mobileHtml = join(DIST, 'mobile.html')
const indexHtml = join(DIST, 'index.html')
if (existsSync(mobileHtml)) renameSync(mobileHtml, indexHtml)
if (!existsSync(indexHtml)) die(`没有 ${indexHtml} —— 先跑 npm run mobile:build，或去掉 --skip-build`)

// 3) 确定性打包
function walk(dir) {
  const out = []
  for (const name of readdirSync(dir)) {
    const full = join(dir, name)
    const st = statSync(full)
    if (st.isDirectory()) out.push(...walk(full))
    else if (st.isFile()) out.push(full)
    else die(`dist-mobile 里有非普通文件：${full}`)
  }
  return out
}

const files = walk(DIST)
  .map((full) => relative(DIST, full).split('\\').join('/'))
  .filter((rel) => rel !== 'mobile.html') // 已改名，兜底排除
  .sort((a, b) => (a < b ? -1 : a > b ? 1 : 0))

for (const rel of files) utimesSync(join(DIST, rel), FIXED_MTIME, FIXED_MTIME)

mkdirSync(dirname(OUT), { recursive: true })
const zip = spawnSync('zip', ['-X', '-D', '-q', '-@', tmpZip], {
  cwd: DIST,
  input: `${files.join('\n')}\n`,
})
if (zip.error) die(`zip 不可用（需要系统 zip；本仓库只支持 macOS/Linux）：${zip.error.message}`)
if (zip.status !== 0) die(`zip 退出码 ${zip.status}`)

const bytes = statSync(tmpZip).size
if (bytes > MAX_ZIP_BYTES) {
  die(`zip ${(bytes / 1024 / 1024).toFixed(1)}MB 超过上限 ${MAX_ZIP_BYTES / 1024 / 1024}MB`)
}
renameSync(tmpZip, OUT)

const sha = createHash('sha256').update(readFileSync(OUT)).digest('hex')
console.log(
  `mobile:embed ✅ ${files.length} 个文件 → ${relative(APP, OUT)} ` +
    `${(bytes / 1024 / 1024).toFixed(2)}MB sha256=${sha.slice(0, 16)}`,
)
