#!/usr/bin/env node
// Hub 组件的浏览器夹具：把**单个 Hub 组件**挂进真浏览器页面。
//
// 为什么不用 mock 会话数据：mock.ts 是多 agent 并行改动的热点文件（协调结论），
// 而这里要验的只是「组件在真 CSS 下长什么样、点下去发生什么」。做法：
//   1. esbuild 打一个 ESM 包（组件 + 它要的 store，状态由调用方在这里 setState 注入）；
//   2. page.setContent 把它挂进 `.hub-demo`（hub.css/demo.css 的样式作用域根）；
//   3. 样式直接从 dev server 取 `?direct` —— 与应用跑的是同一份 CSS，不存副本。
//
// 前置：`VITE_PORT=5180 npm run dev:vite`（与 check-hub-* 系列同一个前置）。
// 纯工具模块，不自己跑断言；断言留在各 check 脚本里。
import { build } from 'esbuild'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const here = fileURLToPath(new URL('.', import.meta.url))
export const APP_DIR = join(here, '..')
export const HUB_DIR = join(APP_DIR, 'src', 'hub')

// 打包：入口是虚拟文件（调用方给的 contents），resolveDir 固定在 src/hub，
// 这样 import './TaskResultCard.tsx' 与源码里的相对路径一致。
export async function bundleHubComponent(contents) {
  const result = await build({
    stdin: { contents, resolveDir: HUB_DIR, sourcefile: 'hub-harness-entry.tsx', loader: 'tsx' },
    bundle: true,
    platform: 'browser',
    format: 'esm', // inline <script type=module>：IIFE 不允许 import.meta（mock.ts 用了它）
    write: false,
    logLevel: 'silent',
    define: { 'process.env.NODE_ENV': '"production"' },
    // 组件链路里的 CSS（katex/markdown）与字体：夹具只验组件结构与布局，
    // 真样式由页面 <link> 从 dev server 取，所以这里全部空掉。
    loader: { '.woff2': 'empty', '.woff': 'empty', '.ttf': 'empty', '.png': 'empty', '.svg': 'empty' },
    plugins: [{
      name: 'stub-css-and-vite-assets',
      setup(b) {
        b.onResolve({ filter: /\.css$/ }, (args) => ({ path: args.path, namespace: 'hub-css' }))
        // write:false 的构建不能产出 CSS 文件 → 让 CSS import 变成一个空 JS 模块。
        b.onLoad({ filter: /.*/, namespace: 'hub-css' }, () => ({ contents: 'export default {}', loader: 'js' }))
        // Vite 的 ?raw / ?url / ?no-inline 在 esbuild 里没有 loader：给空模块（本夹具不渲染品牌图）。
        b.onResolve({ filter: /\?(raw|url|no-inline)$/ }, (args) => ({ path: args.path, namespace: 'vite-asset' }))
        b.onLoad({ filter: /.*/, namespace: 'vite-asset' }, () => ({ contents: 'export default ""', loader: 'js' }))
      },
    }],
  })
  // stdin 入口的产物路径是 <stdout>（没有扩展名）：排除 .css 后取第一个。
  const js = result.outputFiles.find((f) => !f.path.endsWith('.css'))
  if (!js) throw new Error('esbuild 没有产出 JS')
  return js.text
}

/**
 * 挂载一个组件并返回 page。contents 里要自己 createRoot + 渲染，
 * 完成后设置 window.__harnessReady = true（本函数等它）。
 */
export async function openHubHarness(browser, {
  contents,
  baseURL = process.env.HUB_URL || 'http://localhost:5180/',
  width = 1280,
  height = 900,
  theme = 'ink',
} = {}) {
  const js = await bundleHubComponent(contents)
  const page = await browser.newPage({ viewport: { width, height } })
  const errors = []
  page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))
  // 夹具用 page.setContent（origin=null）加载 dev server 的 CSS：@font-face 必然被 CORS 拦。
  // 字体不参与任何断言，只过滤「字体请求失败」这一种噪声，其它 console error 照旧算失败。
  const fontNoise = (m) => /\/assets\/fonts\//.test(m.location?.()?.url ?? '') || /Access to font at/.test(m.text())
  page.on('console', (m) => { if (m.type() === 'error' && !fontNoise(m)) errors.push(m.text()) })
  page.__hubErrors = errors
  const css = (p) => new URL(p, baseURL).toString()
  await page.setContent(`<!doctype html>
<html lang="zh-CN" data-theme="${theme}">
<head>
<meta charset="utf-8">
<link rel="stylesheet" href="${css('src/index.css?direct')}">
<link rel="stylesheet" href="${css('src/hub/demo.css?direct')}">
<link rel="stylesheet" href="${css('src/hub/hub.css?direct')}">
</head>
<body>
<div id="hub-root" class="hub-demo"></div>
<script type="module">${js}</script>
</body>
</html>`, { waitUntil: 'load' })
  await page.waitForFunction(() => window.__harnessReady === true, undefined, { timeout: 15000 })
  return page
}
