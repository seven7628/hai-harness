#!/usr/bin/env node
// mobile:build 后把 Vite 的 mobile.html 改成 index.html。
//
// bridge 的 /m/ 用 http.FileServer 托管一个目录；目录根必须有 index.html 才能
// 在 /m/ 直接出页面（Vite 的多入口产物名是 mobile.html）。改完后：
//   · 开发态 electron/main.ts 的 HAI_MOBILE_WEB_DIR=dist-mobile 可直接用；
//   · 打包态 extraResources 把 dist-mobile 复制成 Resources/mobile-web，同样直接可用。
// 不生成 zip、不做运行时解压缓存。
import { existsSync, renameSync, rmSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = dirname(dirname(fileURLToPath(import.meta.url)))
const DIST = join(APP, 'dist-mobile')
const src = join(DIST, 'mobile.html')
const dst = join(DIST, 'index.html')

if (!existsSync(src)) {
  console.error(`mobile:build 产物缺 ${src}（先跑 MOBILE_ONLY=1 vite build）`)
  process.exit(1)
}
rmSync(dst, { force: true })
renameSync(src, dst)
console.log(`mobile:build → ${dst}`)
