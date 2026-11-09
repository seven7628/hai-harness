#!/usr/bin/env node
// P4-01：200+ 条群消息 + 3 条并行流式草稿的长会话基准。
//
//   npm run bench:hub-long                 # 构建 production 单页 → Chromium 实测（3 轮取中位数）
//   npm run bench:hub-long -- --fallback   # 强制无浏览器回退（只量纯数据层）
//   BENCH_RUNS=5 BENCH_OUT=/tmp/x.json npm run bench:hub-long
//
// 量什么（同一条命令前后各跑一次即可对比）：
//   ① 首屏：mount() → 双 rAF（≈ 首帧绘制），外加 DOM 节点数；
//   ② 每次 delta：事件入队 → DOM 提交（MutationObserver），以及 CDP 的
//      Script/Layout 时间摊到每条 delta；
//   ③ 键入：keydown → input 后下一帧，真实 page.keyboard.type；
//   ④ 滚动：贴底跟随时 rAF 间隔 + 上滑时 rAF 间隔 + longtask；
//   ⑤ DOM/内存：元素数、消息节点数、usedJSHeapSize（--expose-gc 后采样）；
//   ⑥ 弱网不变量 + reduced-motion（跑在真实 DOM 上，失败即非零退出）。
//
// 为什么先 vite build 再跑：dev 版 React 会把渲染放大约 2~3 倍，绝对数字没有参考意义；
// production 单页构建到临时目录，静态服务器只托管这一次产物，跑完即删。
import { build as viteBuild } from 'vite'
import { build as esbuildBuild } from 'esbuild'
import http from 'node:http'
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, extname, join, normalize, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = dirname(fileURLToPath(import.meta.url))
const APP_ROOT = resolve(__dirname, '..')
const RUNS = Math.max(1, Number(process.env.BENCH_RUNS || 3))
const FORCE_FALLBACK = process.argv.includes('--fallback')
const OUT = process.env.BENCH_OUT || ''
const VIEWPORT = { width: 1280, height: 800 }

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.webp': 'image/webp',
  '.woff2': 'font/woff2',
  '.woff': 'font/woff',
  '.wasm': 'application/wasm',
}

const median = (xs) => {
  const a = xs.filter((x) => typeof x === 'number' && Number.isFinite(x)).sort((x, y) => x - y)
  if (a.length === 0) return null
  return a[Math.floor((a.length - 1) / 2)]
}

const round = (x, d = 2) => (typeof x === 'number' ? Math.round(x * 10 ** d) / 10 ** d : x)

function serveStatic(root) {
  const server = http.createServer(async (req, res) => {
    try {
      const url = new URL(req.url || '/', 'http://bench.invalid')
      let pathname = decodeURIComponent(url.pathname)
      if (pathname === '/' || pathname === '') pathname = '/lab/hub-long.html'
      const file = resolve(root, '.' + normalize(pathname))
      if (file !== root && !file.startsWith(root + sep)) {
        res.writeHead(403).end('forbidden')
        return
      }
      const data = await readFile(file)
      res.writeHead(200, { 'content-type': MIME[extname(file)] || 'application/octet-stream', 'cache-control': 'no-store' })
      res.end(data)
    } catch {
      res.writeHead(404).end('not found')
    }
  })
  return new Promise((resolvePromise) => {
    server.listen(0, '127.0.0.1', () => {
      const addr = server.address()
      resolvePromise({
        url: `http://127.0.0.1:${addr.port}`,
        close: () => new Promise((r) => server.close(r)),
      })
    })
  })
}

async function runBrowserSuite(page) {
  const info = await page.evaluate(() => window.__hubBench.fixtureInfo())
  const mount = await page.evaluate(() => window.__hubBench.mount())
  const burst = await page.evaluate(() => window.__hubBench.deltaBurst({ count: 120, gapMs: 8 }))
  const domAfterBurst = burst.dom

  const composer = page.locator('.hub-composer textarea')
  await composer.click()
  // ① 静态键入（无并发 delta）：单独看「长列表 + 每次输入重渲染」的成本。
  await page.evaluate(() => window.__hubBench.resetInput())
  await page.keyboard.type('abcdefghijkl', { delay: 40 })
  await page.waitForTimeout(120)
  const inputStatic = await page.evaluate(() => window.__hubBench.inputStats())
  // ② 并发流式键入：后台每 30ms 一条 delta，键入与流式竞争主线程。
  await composer.fill('')
  await page.evaluate(() => window.__hubBench.startBackgroundStream({ intervalMs: 30 }))
  await page.waitForTimeout(200)
  await page.evaluate(() => window.__hubBench.resetInput())
  await page.keyboard.type('mnopqrstuvwxyz0123', { delay: 40 })
  await page.waitForTimeout(120)
  const inputStreaming = await page.evaluate(() => window.__hubBench.inputStats())
  const backgroundDeltas = await page.evaluate(() => window.__hubBench.stopBackgroundStream())

  const scroll = await page.evaluate(() => window.__hubBench.scrollStream({ durationMs: 2400 }))
  const snap = await page.evaluate(() => window.__hubBench.snapshot({ gc: true }))

  const motionNoPref = await page.evaluate(() => window.__hubBench.motionProbe())
  await page.emulateMedia({ reducedMotion: 'reduce' })
  const motionReduce = await page.evaluate(() => window.__hubBench.motionProbe())
  await page.emulateMedia({ reducedMotion: 'no-preference' })

  const ui = await page.evaluate(() => window.__hubBench.uiProbe())
  const weak = await page.evaluate(() => window.__hubBench.weakNetwork())

  const motionNoPrefOk =
    motionNoPref.caret === 'hub-caret' &&
    motionNoPref.typingDot === 'hub-typing-dot' &&
    motionNoPref.highlight === 'hub-msg-highlight-in' &&
    motionNoPref.convDot === 'hub-conv-pulse' &&
    motionNoPref.reduceMatches === false
  const motionReduceOk =
    motionReduce.reduceMatches === true &&
    [motionReduce.caret, motionReduce.typingDot, motionReduce.highlight, motionReduce.convDot].every((x) => x === 'none')

  return {
    info,
    mountMs: mount.mountMs,
    mountThreadHtmlHash: mount.dom.threadHtmlHash,
    mountThreadElements: mount.dom.threadElements,
    mountElements: mount.dom.totalElements,
    mountMessages: mount.dom.messages,
    mountRoundGroups: mount.dom.roundGroups,
    mountRoutingRows: mount.dom.routingRows,
    mountScrollHeight: mount.scrollHeight,
    deltaCount: burst.count,
    deltaCommitted: burst.committed,
    deltaWallMs: burst.wallMs,
    deltaCommitP50: burst.commit.p50,
    deltaCommitP95: burst.commit.p95,
    deltaCommitMax: burst.commit.max,
    deltaLayoutP50: burst.layout.p50,
    deltaLayoutP95: burst.layout.p95,
    deltaLayoutMax: burst.layout.max,
    deltaStreaming: domAfterBurst.streaming,
    deltaMessages: domAfterBurst.messages,
    inputStaticN: inputStatic.samples.n,
    inputStaticP50: inputStatic.samples.p50,
    inputStaticP95: inputStatic.samples.p95,
    inputStaticMax: inputStatic.samples.max,
    inputStaticRaw: inputStatic.raw,
    inputStaticFrameRaw: inputStatic.frameRaw,
    inputStaticFrameP50: inputStatic.frameSamples.p50,
    inputStaticFrameP95: inputStatic.frameSamples.p95,
    inputStaticFrameMax: inputStatic.frameSamples.max,
    inputN: inputStreaming.samples.n,
    inputP50: inputStreaming.samples.p50,
    inputP95: inputStreaming.samples.p95,
    inputMax: inputStreaming.samples.max,
    inputRaw: inputStreaming.raw,
    inputFrameRaw: inputStreaming.frameRaw,
    inputFrameP50: inputStreaming.frameSamples.p50,
    inputFrameP95: inputStreaming.frameSamples.p95,
    inputFrameMax: inputStreaming.frameSamples.max,
    inputBackgroundDeltas: backgroundDeltas,
    scrollFrames: scroll.frames,
    scrollRafP50: scroll.raf.p50,
    scrollRafP95: scroll.raf.p95,
    scrollRafMax: scroll.raf.max,
    scrollOver50: scroll.over50,
    scrollLongTaskN: scroll.longTasks.n,
    scrollLongTaskMax: scroll.longTasks.max,
    scrollLongTaskTotal: scroll.longTasks.total,
    scrollBottomGapMax: scroll.bottomGapMaxPx,
    scrollDrift: scroll.driftPx,
    finalThreadElements: snap.dom.threadElements,
    finalElements: snap.dom.totalElements,
    finalMessages: snap.dom.messages,
    finalStreaming: snap.dom.streaming,
    finalRoundGroups: snap.dom.roundGroups,
    finalRoutingRows: snap.dom.routingRows,
    finalTypingText: snap.dom.typingText,
    heapUsedMB: snap.memory.usedJSHeapSize,
    heapTotalMB: snap.memory.totalJSHeapSize,
    motionNoPref,
    motionReduce,
    motionNoPrefOk,
    motionReduceOk,
    ui,
    weak,
  }
}

function summarize(runs) {
  const keys = [
    'mountMs', 'mountThreadElements', 'mountElements', 'mountMessages', 'mountRoundGroups', 'mountRoutingRows',
    'deltaCommitted', 'deltaWallMs', 'deltaCommitP50', 'deltaCommitP95', 'deltaCommitMax',
    'deltaLayoutP50', 'deltaLayoutP95', 'deltaLayoutMax',
    'inputStaticN', 'inputStaticP50', 'inputStaticP95', 'inputStaticMax',
    'inputStaticFrameP50', 'inputStaticFrameP95', 'inputStaticFrameMax',
    'inputN', 'inputP50', 'inputP95', 'inputMax', 'inputBackgroundDeltas',
    'inputFrameP50', 'inputFrameP95', 'inputFrameMax',
    'scrollFrames', 'scrollRafP50', 'scrollRafP95', 'scrollRafMax', 'scrollOver50',
    'scrollLongTaskN', 'scrollLongTaskMax', 'scrollLongTaskTotal', 'scrollBottomGapMax', 'scrollDrift',
    'finalThreadElements', 'finalElements', 'finalMessages', 'finalStreaming', 'finalRoundGroups', 'finalRoutingRows',
    'heapUsedMB', 'heapTotalMB',
  ]
  const out = {}
  for (const k of keys) out[k] = median(runs.map((r) => r[k]))
  out.motionNoPrefOk = runs.every((r) => r.motionNoPrefOk)
  out.motionReduceOk = runs.every((r) => r.motionReduceOk)
  out.uiPass = runs.every((r) => r.ui.pass)
  out.uiFailures = runs.flatMap((r, i) => (r.ui.pass ? [] : [{ run: i + 1, ...r.ui }]))
  out.weakPass = runs.every((r) => r.weak.pass)
  out.weakFailures = runs.flatMap((r, i) => r.weak.checks.filter((c) => !c.pass).map((c) => ({ run: i + 1, name: c.name })))
  out.uiProbe = runs[0].ui
  out.motionNoPref = runs[0].motionNoPref
  out.motionReduce = runs[0].motionReduce
  out.typingBeforeWeak = runs[0].finalTypingText
  out.fixtureMessages = runs[0].info.messages
  out.fixtureRounds = runs[0].info.rounds
  out.fixtureTextChars = runs[0].info.textChars
  out.fixtureDrafts = runs[0].info.drafts
  out.mountThreadHtmlHash = runs[0].mountThreadHtmlHash
  out.inputStaticRaw = runs[0].inputStaticRaw
  out.inputRaw = runs[0].inputRaw
  out.inputStaticFrameRaw = runs[0].inputStaticFrameRaw
  out.inputFrameRaw = runs[0].inputFrameRaw
  return out
}

function printBrowserReport(summary, chromiumVersion, runs) {
  const s = summary
  console.log(`MODE=browser chromium=${chromiumVersion} runs=${runs} viewport=${VIEWPORT.width}x${VIEWPORT.height} production=1`)
  console.log(`fixture messages=${s.fixtureMessages} routingRounds=${s.fixtureRounds} roundGroups=${s.mountRoundGroups} textChars=${s.fixtureTextChars}`)
  console.log('')
  console.log('metric                              value')
  console.log('----------------------------------  ------------------')
  console.log(`initial mount → first frame (ms)    ${s.mountMs}`)
  console.log(`mount thread DOM elements            ${s.mountThreadElements}`)
  console.log(`mount total DOM elements             ${s.mountElements}`)
  console.log(`mount thread HTML hash              ${s.mountThreadHtmlHash}`)
  console.log(`per-delta commit p50 / p95 / max ms ${s.deltaCommitP50} / ${s.deltaCommitP95} / ${s.deltaCommitMax}`)
  console.log(`per-delta forced layout p50/p95/max ms ${s.deltaLayoutP50} / ${s.deltaLayoutP95} / ${s.deltaLayoutMax}`)
  console.log(`delta burst wall ms (${s.deltaCommitted} committed) ${s.deltaWallMs}`)
  console.log(`keystroke handler static ms p50/p95  ${s.inputStaticP50} / ${s.inputStaticP95} (max ${s.inputStaticMax})`)
  console.log(`keystroke → next frame static ms     ${s.inputStaticFrameP50} / ${s.inputStaticFrameP95} (max ${s.inputStaticFrameMax})`)
  console.log(`keystroke handler + streaming p50/95 ${s.inputP50} / ${s.inputP95} (max ${s.inputMax}; ${s.inputBackgroundDeltas} bg deltas)`)
  console.log(`keystroke → next frame + streaming   ${s.inputFrameP50} / ${s.inputFrameP95} (max ${s.inputFrameMax})`)
  console.log(`scroll RAF p50 / p95 / max ms       ${s.scrollRafP50} / ${s.scrollRafP95} / ${s.scrollRafMax}`)
  console.log(`scroll frames >50ms                 ${s.scrollOver50} / ${s.scrollFrames}`)
  console.log(`scroll long tasks n / max / total ms ${s.scrollLongTaskN} / ${s.scrollLongTaskMax} / ${s.scrollLongTaskTotal}`)
  console.log(`stick-bottom gap max px             ${s.scrollBottomGapMax}`)
  console.log(`scroll drift while reading up px    ${s.scrollDrift}`)
  console.log(`final thread / total DOM elements    ${s.finalThreadElements} / ${s.finalElements}`)
  console.log(`final messages / round groups        ${s.finalMessages} / ${s.finalRoundGroups}`)
  console.log(`JS heap used / total MB              ${s.heapUsedMB} / ${s.heapTotalMB}`)
  console.log(`reduced-motion no-preference pass    ${s.motionNoPrefOk}  ${JSON.stringify(s.motionNoPref)}`)
  console.log(`reduced-motion reduce pass           ${s.motionReduceOk}  ${JSON.stringify(s.motionReduce)}`)
  console.log(`context chip → highlight (memo dep)  ${s.uiPass ? 'PASS' : 'FAIL ' + JSON.stringify(s.uiFailures)} ${JSON.stringify(s.uiProbe)}`)
  console.log(`weak-network invariants              ${s.weakPass ? 'PASS' : 'FAIL ' + JSON.stringify(s.weakFailures)}`)
}

async function runFallback(reason) {
  console.log(`MODE=fallback reason=${reason}`)
  const result = await esbuildBuild({
    entryPoints: [join(APP_ROOT, 'src/lab/hubLongPure.ts')],
    bundle: true,
    platform: 'node',
    format: 'esm',
    write: false,
    logLevel: 'silent',
  })
  const source = result.outputFiles[0].text
  const mod = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)
  const pure = mod.runHubLongPureBench()
  console.log(`fixture messages=${pure.messages} rounds=${pure.rounds} items=${pure.items} drafts=${pure.drafts} textChars=${pure.textChars}`)
  console.log(`applyHubDelta (µs/delta) mean/p95/max  ${pure.applyDeltaUs.mean} / ${pure.applyDeltaUs.p95} / ${pure.applyDeltaUs.max}`)
  console.log(`buildThreadItems (ms/rebuild) mean/p95/max ${pure.buildItemsMs.mean} / ${pure.buildItemsMs.p95} / ${pure.buildItemsMs.max}`)
  console.log('render/keystroke/scroll/DOM metrics: unavailable (no browser)')
  const summary = { mode: 'fallback', reason, pure }
  console.log('BENCH_JSON=' + JSON.stringify(summary))
  if (OUT) {
    await writeFile(OUT, JSON.stringify(summary, null, 2))
    console.log(`wrote ${OUT}`)
  }
  process.exitCode = 0
}

async function main() {
  if (!FORCE_FALLBACK) {
    let chromium
    try {
      ;({ chromium } = await import('playwright'))
    } catch (e) {
      await runFallback(`playwright-unavailable: ${String(e.message).split('\n')[0]}`)
      return
    }

    const outDir = await mkdtemp(join(tmpdir(), 'hub-long-bench-'))
    let server
    let browser
    try {
      await viteBuild({
        root: APP_ROOT,
        logLevel: 'error',
        build: {
          outDir,
          emptyOutDir: true,
          minify: 'esbuild',
          reportCompressedSize: false,
          rollupOptions: { input: { hubLong: join(APP_ROOT, 'lab/hub-long.html') } },
        },
      })
      server = await serveStatic(outDir)
      try {
        browser = await chromium.launch({
          headless: true,
          args: [
            '--enable-precise-memory-info',
            '--js-flags=--expose-gc',
            '--disable-background-timer-throttling',
            '--disable-renderer-backgrounding',
            '--disable-backgrounding-occluded-windows',
            '--disable-dev-shm-usage',
          ],
        })
      } catch (e) {
        await runFallback(`chromium-launch-failed: ${String(e.message).split('\n')[0]}`)
        return
      }

      const results = []
      const pageErrors = []
      const total = RUNS + 1 // 1 轮预热（编译/字体/shiki 缓存不进中位数）
      for (let i = 0; i < total; i++) {
        const page = await browser.newPage({ viewport: VIEWPORT, deviceScaleFactor: 1 })
        page.on('pageerror', (e) => pageErrors.push(e.message))
        await page.goto(`${server.url}/lab/hub-long.html`, { waitUntil: 'load' })
        await page.waitForFunction(() => !!window.__hubBench, null, { timeout: 30_000 })
        await page.evaluate(() => document.fonts.ready.then(() => true))
        const run = await runBrowserSuite(page)
        await page.close()
        if (i === 0) console.log(`warmup done: mount=${run.mountMs}ms deltaCommitP50=${run.deltaCommitP50}ms`)
        else results.push(run)
      }

      if (pageErrors.length > 0) {
        throw new Error(`page errors: ${JSON.stringify([...new Set(pageErrors)].slice(0, 5))}`)
      }

      const summary = summarize(results)
      console.log(`[bench] chromium=${browser.version()}`)
      printBrowserReport(summary, browser.version(), results.length)

      const info = results[0].info
      const payload = { mode: 'browser', chromium: browser.version(), runs: results.length, viewport: VIEWPORT, fixture: info, metrics: summary }
      console.log('BENCH_JSON=' + JSON.stringify(payload))
      if (OUT) {
        await writeFile(OUT, JSON.stringify(payload, null, 2))
        console.log(`wrote ${OUT}`)
      }
      const failed = !summary.weakPass || !summary.uiPass || !summary.motionNoPrefOk || !summary.motionReduceOk
      if (failed) {
        console.error('[bench] FAIL: weak-network or reduced-motion invariant broken')
        process.exitCode = 1
      }
    } finally {
      if (browser) await browser.close().catch(() => {})
      if (server) await server.close().catch(() => {})
      await rm(outDir, { recursive: true, force: true }).catch(() => {})
    }
  } else {
    await runFallback('forced-by-flag')
  }
}

main().catch((e) => {
  console.error('[bench] FAIL:', e.stack || e.message || e)
  process.exitCode = 1
})
