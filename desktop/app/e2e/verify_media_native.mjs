#!/usr/bin/env node
// 音视频预览的**真 Electron** 验收（R08c）：协议 + 播放器 + 切标签 + 降级。
//
// 为什么必须另有一层（链内的 media-protocol.test.ts 只能验一半）：
//   · Range / Content-Range 这件事只有**真 Chromium 的媒体栈**才是最终裁判：
//     206 头对不对、拖动进度条时浏览器到底发什么 Range、切标签回来会不会重新拉首字节；
//   · 「ffprobe 藏掉仍然能播」只有真的把 PATH 摘掉才证明得了（mock 掉一次调用等于
//     没验 LookPath 那条路）；
//   · 播放器的元信息（duration/readyState）来自解码器，DOM 断言证明不了能解码。
//
// 用法：cd desktop/app && npm run e2e:media-native
//   MEDIA_E2E_SRC=<file.mp4>   现成的 1 小时 mp4 夹具（缺省用 ffmpeg 现场生成，约 20MB / 4s）
//   GO_CODE_E2E_FORCE_BUILD=1  强制 npm run build（改了 electron/ 或 src/ 后必须）
// 退出码：全通过 0；断言失败 1；**前置不满足 2**（没有 ffmpeg 且没给夹具 / 端口被占）。
import { _electron } from 'playwright'
import { spawn, spawnSync, execFileSync } from 'child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { freePort, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const REPO = path.resolve(APP, '../..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
const SHOTS = path.join(__dirname, '_media_shots')

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const fail = []
const check = (cond, m, detail = '') => {
  if (cond) ok(m)
  else { fail.push(m); console.log(`  ❌ ${m} ${detail}`) }
}
const precondition = (m) => {
  console.error(`\n⛔ 前置不满足（退出码 2，不是测试失败）：${m}`)
  process.exit(2)
}
async function waitFor(fn, { timeout = 60000, interval = 250, label = '条件' } = {}) {
  const end = Date.now() + timeout
  let last
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v; last = v } catch (e) { last = e?.message }
    await sleep(interval)
  }
  throw new Error(`等待超时: ${label}（最后：${JSON.stringify(last)}）`)
}

// —— 前置：ffmpeg（生成夹具）——
const hasFfmpeg = spawnSync('ffmpeg', ['-version'], { encoding: 'utf8' }).status === 0
const hasFfprobe = spawnSync('ffprobe', ['-version'], { encoding: 'utf8' }).status === 0
const MEDIA_SRC = process.env.MEDIA_E2E_SRC || ''
if (!MEDIA_SRC && !hasFfmpeg) {
  precondition('没有 ffmpeg，也没有 MEDIA_E2E_SRC 指向一份 1 小时 mp4 —— 媒体夹具造不出来')
}
if (!hasFfprobe) {
  precondition('本机没有 ffprobe：探测通道无法验证（降级分支另有专门用例，但主用例需要真 ffprobe）')
}

// —— 构建（与 e2e:preview-native 同一套：改 electron/ 或 src/ 后必须重跑）——
const indexHtml = path.join(APP, 'dist', 'index.html')
const mainJs = path.join(APP, 'dist-electron', 'main.js')
if (process.env.GO_CODE_E2E_FORCE_BUILD === '1' || !fs.existsSync(indexHtml) || !fs.existsSync(mainJs)) {
  const b = spawnSync('npm', ['run', 'build'], { cwd: APP, encoding: 'utf8' })
  if (b.status !== 0) precondition(`npm run build 失败（不是断言失败）：\n${b.stdout}\n${b.stderr}`)
}
const gb = spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], { cwd: REPO, encoding: 'utf8' })
if (gb.status !== 0) precondition(`go build 失败（不是断言失败）：\n${gb.stderr}`)

// —— 夹具工作区 ——
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'media-native-home-'))
const WS = fs.mkdtempSync(path.join(os.tmpdir(), 'media-native-ws-'))
fs.mkdirSync(SHOTS, { recursive: true })
const LONG = path.join(WS, 'long.mp4')
const MKV = path.join(WS, 'clip.mkv')
const MP3 = path.join(WS, 'song.mp3')
const TXT = 'notes.txt'
fs.writeFileSync(path.join(WS, TXT), '切换到这一页再切回去\n')

console.log('\n[0] 夹具')
if (MEDIA_SRC) {
  fs.copyFileSync(MEDIA_SRC, LONG)
  ok(`复用现成 mp4：${MEDIA_SRC}`)
} else {
  // 1 小时、160×120、1fps、h264+aac：编码 ~4s / 21MB，但时长/关键帧分布是真的一份长视频
  // （g=60 → 每 60s 一个关键帧，拖动 3000s 真要解容器找索引）。
  execFileSync('ffmpeg', [
    '-v', 'error', '-y',
    '-f', 'lavfi', '-i', 'testsrc2=size=160x120:rate=1',
    '-f', 'lavfi', '-i', 'sine=frequency=440:sample_rate=8000',
    '-t', '3600', '-c:v', 'libx264', '-preset', 'ultrafast', '-g', '60', '-pix_fmt', 'yuv420p',
    '-c:a', 'aac', '-b:a', '16k', '-ac', '1', '-movflags', '+faststart', LONG,
  ], { stdio: 'inherit' })
  ok('生成 1 小时 mp4（h264+aac, faststart）')
}
execFileSync('ffmpeg', ['-v', 'error', '-y', '-f', 'lavfi', '-i', 'testsrc2=size=160x120:rate=10', '-t', '3',
  '-c:v', 'libx264', '-preset', 'ultrafast', '-pix_fmt', 'yuv420p', MKV], { stdio: 'inherit' })
execFileSync('ffmpeg', ['-v', 'error', '-y', '-f', 'lavfi', '-i', 'sine=frequency=660:sample_rate=44100', '-t', '3',
  '-c:a', 'libmp3lame', '-b:a', '64k', MP3], { stdio: 'inherit' })
ok('生成 mkv（需转码）与 mp3（音频）夹具')
check(!fs.existsSync(path.join(WS, 'notes.mp4')), '工作区里只有真媒体与一个文本（无干扰文件）')

// —— vite dev server（renderer 走源码：e2e 才能 import 到与真应用同一个 store 实例）——
const DEV_PORT = await freePort()
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(DEV_PORT), '--strictPort'], { cwd: APP, stdio: 'pipe' })
const cleanup = () => { try { server.kill('SIGTERM') } catch { /* 已退出 */ } }
process.on('exit', cleanup)

// store 入口（与真应用同一个模块实例）
const STORE = '/src/store/useAppStore.ts'

async function launch({ home, env = {} }) {
  const app = await _electron.launch({
    executablePath: ELECTRON,
    args: [APP],
    cwd: APP,
    env: { ...process.env, HOME: home, HAI_DATA_DIR: home, VITE_DEV_SERVER_URL: `http://localhost:${DEV_PORT}/`, ...env },
  })
  const win = await app.firstWindow()
  await win.waitForLoadState('domcontentloaded')

  // 工作区必须走**真入口**：主进程的 known-workspaces 只认系统选择器结果（评审裁决 1）。
  // e2e 不能绕过它（绕过就等于没验那道门），所以只把主进程的 dialog 换成返回夹具目录，
  // 其余照用户的路走：欢迎页的「选择工作区」→ pick-workspace IPC → rememberWorkspace → store。
  await app.evaluate(({ dialog }, [ws]) => {
    dialog.showOpenDialog = async () => ({ canceled: false, filePaths: [ws] })
  }, [WS])
  // 没有工作区时应用停在 BootScreen（只有一个「选择工作区」按钮）
  const bootBtn = win.locator('.boot-cta button').first()
  await waitFor(() => bootBtn.isVisible(), { label: '欢迎页的「选择工作区」', timeout: 180000 })
  await bootBtn.click()

  // 180s 而不是 90s：这条链里它是第 5 个 Electron 启动（前四个脚本的实例刚退），
  // 本机同时跑其它会话的 Electron 时首屏会被拖到分钟级（实测一次 90s 超时、单跑全绿）。
  // 这是**启动等待**，不是断言 —— 放宽只影响「环境慢时多久放弃」，不影响判据。
  await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: '主界面就绪', timeout: 180000 })
  await waitFor(() => win.evaluate(async ([m, ws]) => (await import(m)).useAppStore.getState().workspace === ws, [STORE, WS]),
    { label: '工作区已切换（经真实选择器入口）', timeout: 60000 })
  // 空工作区按规格**不**自动建会话 → 用真应用的 newSession 建一个（与 e2e:preview-native 同口径）
  const sid0 = await win.evaluate(async (m) => (await import(m)).useAppStore.getState().activeSessionId ?? '', STORE)
  if (!sid0) {
    await win.evaluate(async (m) => { await (await import(m)).useAppStore.getState().newSession() }, STORE)
  }
  await waitFor(() => win.evaluate(async (m) => !!(await import(m)).useAppStore.getState().activeSessionId, STORE),
    { label: 'activeSessionId', timeout: 60000 })
  return { app, win }
}

const openPreview = (win, rel) => win.evaluate(async ([m, p]) => {
  const store = (await import(m)).useAppStore
  const id = store.getState().openTab({ kind: 'file', path: p, mode: 'preview' })
  store.getState().setRightExpanded(true)
  return id
}, [STORE, rel])
const activateTab = (win, id) => win.evaluate(async ([m, tid]) => {
  (await import(m)).useAppStore.getState().activateTab(tid)
}, [STORE, id])
const mediaStats = (win) => win.evaluate(() => window.desktop.mediaStats())

let app
try {
  await waitProbePage(`http://localhost:${DEV_PORT}/`, { marker: '<div id="root">', probe: 'e2e:media-native', child: server, timeoutMs: 120000 })

  // ======================= 第一次启动：正常环境（有 ffprobe） =======================
  const first = await launch({ home: HOME })
  app = first.app
  const win = first.win
  const errs = []
  win.on('console', (m) => {
    const t = m.text()
    if (m.type() === 'error' && !/favicon/.test(t)) errs.push(t.slice(0, 200))
  })

  // ---------------- 1. 1 小时 mp4：直放 + 元信息 ----------------
  console.log('\n[1] 1 小时 mp4：协议通道 + 元信息')
  const mp4Tab = await openPreview(win, 'long.mp4')
  await win.waitForSelector('[data-renderer="media"] video', { timeout: 60000 })
  await waitFor(() => win.evaluate(() => {
    const v = document.querySelector('[data-renderer="media"] video')
    return !!v && v.readyState >= 1 && Number.isFinite(v.duration) && v.duration > 0
  }), { label: 'video 元数据就绪', timeout: 60000 })
  {
    const s = await win.evaluate(() => {
      const host = document.querySelector('[data-renderer="media"] .pv-media')
      const v = host?.querySelector('video')
      return {
        mode: host?.getAttribute('data-playback-mode'),
        kind: host?.getAttribute('data-media-kind'),
        probe: host?.getAttribute('data-probe-state'),
        duration: v?.duration ?? 0,
        meta: host?.querySelector('.pv-media-meta')?.textContent ?? '',
        path: host?.querySelector('.pv-media-path')?.textContent ?? '',
        hasOpen: !!host?.querySelector('.pv-media-open'),
      }
    })
    check(s.mode === 'direct', `矩阵判定：1 小时 mp4 = direct（实得 ${s.mode}）`)
    check(s.kind === 'video' && s.probe === 'ok', `播放器类型 video、探测状态 ok（${s.kind}/${s.probe}）`)
    check(Math.abs(s.duration - 3600) < 2, `解码器给出的时长 ≈ 3600s（实得 ${s.duration.toFixed(2)}）`)
    check(/1:00:0\d/.test(s.meta), `元信息里有真时长 1:00:0x（${s.meta.match(/1:00:0\d/)?.[0] ?? s.meta.slice(0, 80)}）`)
    check(/160×120/.test(s.meta), `元信息里有分辨率 160×120（${s.meta.slice(0, 120)}）`)
    check(/h264/i.test(s.meta) && /aac/i.test(s.meta), `元信息里有编码 h264 / aac（${s.meta.slice(0, 120)}）`)
    check(/Mbps|kbps/.test(s.meta), '元信息里有码率')
    check(s.hasOpen, '有「用系统播放器打开」按钮')
    check(/long\.mp4$/.test(s.path), `面板显示文件路径（${s.path}）`)
  }
  await win.screenshot({ path: path.join(SHOTS, 'm1-long-mp4.png') })

  // ---------------- 2. Range：真 206 + Content-Range ----------------
  console.log('\n[2] Range：主进程 fetch 同一 URL（绕开 CORS，仍走同一条 protocol.handle）')
  const mediaUrl = await win.evaluate(() => {
    const v = document.querySelector('[data-renderer="media"] video')
    return v?.getAttribute('src') ?? v?.src ?? ''
  })
  check(/^hai-media:\/\/local\//.test(mediaUrl), `src 是 hai-media:// URL（不是 data:/file:）：${mediaUrl.slice(0, 60)}…`)
  check(!/^(data|blob|file):/.test(mediaUrl), 'src 不是 data:/blob:/file:（没把文件读进内存）')
  const r206 = await app.evaluate(async ({ session }, [url]) => {
    const res = await session.defaultSession.fetch(url, { headers: { Range: 'bytes=0-1023' } })
    const body = await res.arrayBuffer()
    return {
      status: res.status,
      contentRange: res.headers.get('content-range'),
      acceptRanges: res.headers.get('accept-ranges'),
      contentType: res.headers.get('content-type'),
      length: body.byteLength,
    }
  }, [mediaUrl])
  check(r206.status === 206, `Range: bytes=0-1023 → 206（实得 ${r206.status}）`)
  check(/^bytes 0-1023\/\d+$/.test(r206.contentRange ?? ''), `Content-Range 正确：${r206.contentRange}`)
  check(r206.acceptRanges === 'bytes', `Accept-Ranges: bytes（实得 ${r206.acceptRanges}）`)
  check(r206.contentType === 'video/mp4', `Content-Type: video/mp4（实得 ${r206.contentType}）`)
  check(r206.length === 1024, `真的只回了 1024 字节（实得 ${r206.length}）`)
  const r416 = await app.evaluate(async ({ session }, [url]) => {
    const res = await session.defaultSession.fetch(url, { headers: { Range: 'bytes=999999999-' } })
    return { status: res.status, contentRange: res.headers.get('content-range') }
  }, [mediaUrl])
  check(r416.status === 416, `越界 Range → 416（实得 ${r416.status}）`)
  check(/^bytes \*\/\d+$/.test(r416.contentRange ?? ''), `416 带 Content-Range: bytes */size（实得 ${r416.contentRange}）`)

  // ---------------- 3. 拖到 3000s：seeked 在 1s 内 ----------------
  console.log('\n[3] 拖动到 3000s：seeked 事件在 1s 内')
  const seek = await win.evaluate(() => new Promise((resolve) => {
    const v = document.querySelector('[data-renderer="media"] video')
    const t0 = performance.now()
    const timer = setTimeout(() => resolve({ ms: -1, t: v.currentTime }), 3000)
    v.addEventListener('seeked', () => {
      clearTimeout(timer)
      resolve({ ms: performance.now() - t0, t: v.currentTime })
    }, { once: true })
    v.currentTime = 3000
  }))
  check(seek.ms >= 0, '收到 seeked 事件', JSON.stringify(seek))
  check(seek.ms >= 0 && seek.ms < 1000, `seeked 在 1s 内（实得 ${Math.round(seek.ms)}ms）`)
  check(Math.abs(seek.t - 3000) < 1, `currentTime 落在 3000s（实得 ${seek.t.toFixed(2)}）`)

  // ---------------- 4. 切走 5 秒再切回：不重新请求首字节 ----------------
  console.log('\n[4] 切到别的标签 5s 再切回：元素复用、位置保留、首字节没有重下')
  const before = await mediaStats(win)
  await openPreview(win, TXT)
  await win.waitForSelector('[data-renderer="text"] .fp-code .fp-ln', { timeout: 60000 })
  await sleep(5000)
  await activateTab(win, mp4Tab)
  await win.waitForSelector('[data-renderer="media"] video', { timeout: 60000 })
  await sleep(500)
  const afterBack = await win.evaluate(() => {
    const host = document.querySelector('[data-renderer="media"] .pv-media')
    const v = host?.querySelector('video')
    return { t: v?.currentTime ?? -1, ready: v?.readyState ?? 0, paused: v?.paused }
  })
  const after = await mediaStats(win)
  check(Math.abs(afterBack.t - 3000) < 1, `切回来播放位置仍是 3000s（实得 ${afterBack.t?.toFixed?.(2)}）`)
  check(afterBack.ready >= 1, `切回来元素已就绪（readyState=${afterBack.ready}）—— 没有重新解容器`)
  check(after.firstByteHits === before.firstByteHits, `首字节请求次数没变（${before.firstByteHits} → ${after.firstByteHits}）`)
  check(after.rangeRequests === before.rangeRequests, `Range 请求次数没变（${before.rangeRequests} → ${after.rangeRequests}）`)
  console.log(`     （协议计数：requests ${before.requests}→${after.requests}，bytes ${before.bytesServed}→${after.bytesServed}）`)
  await win.screenshot({ path: path.join(SHOTS, 'm2-after-switch-back.png') })

  // ---------------- 5. mkv：明确提示需要转码，不渲染黑框播放器 ----------------
  console.log('\n[5] mkv：提示需要转码 + 系统播放器入口')
  await openPreview(win, 'clip.mkv')
  await win.waitForSelector('[data-renderer="media"] .pv-media[data-playback-mode="transcode"]', { timeout: 60000 })
  // 「需转码」是**立刻**给出的（按容器），探测结果稍后才到 —— 等它到齐再断言
  await waitFor(() => win.evaluate(() => document.querySelector('[data-renderer="media"] .pv-media')?.getAttribute('data-probe-state') === 'ok'),
    { label: 'mkv 的 ffprobe 探测完成', timeout: 60000 })
  {
    const s = await win.evaluate(() => {
      const host = document.querySelector('[data-renderer="media"] .pv-media')
      return {
        mode: host?.getAttribute('data-playback-mode'),
        text: host?.textContent ?? '',
        videos: host?.querySelectorAll('video').length ?? -1,
        open: !!host?.querySelector('.pv-media-open'),
        probe: host?.getAttribute('data-probe-state'),
        meta: host?.querySelector('.pv-media-meta')?.textContent ?? '',
      }
    })
    check(s.mode === 'transcode', `矩阵判定：mkv = transcode（实得 ${s.mode}）`)
    check(/需要转码/.test(s.text), '面板写明「需要转码」')
    check(/Matroska|mkv/i.test(s.text), `面板给出原因（${s.text.slice(0, 120)}）`)
    check(s.videos === 0, `没有渲染播放器（视频元素 ${s.videos} 个 —— 有就是黑框）`)
    check(s.open, '仍然给「用系统播放器打开」')
    check(s.probe === 'ok', `mkv 的 ffprobe 探测照常成功（实得 ${s.probe}）`)
    check(/0:03/.test(s.meta ?? '') && /h264/i.test(s.meta ?? ''), `mkv 的元信息是真的（${(s.meta ?? '').slice(0, 80)}）`)
  }
  await win.screenshot({ path: path.join(SHOTS, 'm3-mkv-transcode.png') })

  // ---------------- 6. mp3：<audio> + 音频元信息 ----------------
  console.log('\n[6] mp3：<audio> 而不是 <video>')
  await openPreview(win, 'song.mp3')
  await win.waitForSelector('[data-renderer="media"] audio', { timeout: 60000 })
  // <audio> 出现 ≠ 元数据已解出来：duration 在 loadedmetadata 之前是 NaN（实测偶发，
  // 上游那条 waitForSelector 只证明元素在 DOM 里）。断言时长之前必须等它真的有值。
  await waitFor(() => win.evaluate(() => {
    const a = document.querySelector('[data-renderer="media"] audio')
    return !!a && a.readyState >= 1 && Number.isFinite(a.duration) && a.duration > 0
  }), { label: 'audio 元数据就绪', timeout: 60000 })
  {
    const s = await win.evaluate(() => {
      const host = document.querySelector('[data-renderer="media"] .pv-media')
      const a = host?.querySelector('audio')
      return {
        kind: host?.getAttribute('data-media-kind'),
        mode: host?.getAttribute('data-playback-mode'),
        dur: a?.duration ?? 0,
        videos: host?.querySelectorAll('video').length ?? -1,
        meta: host?.querySelector('.pv-media-meta')?.textContent ?? '',
      }
    })
    check(s.kind === 'audio' && s.videos === 0, `mp3 用 <audio>（kind=${s.kind}, video=${s.videos}）`)
    check(s.mode === 'direct', `mp3 = direct（实得 ${s.mode}）`)
    check(Math.abs(s.dur - 3) < 1, `时长 ≈ 3s（实得 ${s.dur.toFixed(2)}）`)
    check(/mp3/i.test(s.meta), `编码显示 mp3（${s.meta.slice(0, 80)}）`)
  }
  await win.screenshot({ path: path.join(SHOTS, 'm4-mp3-audio.png') })

  // ---------------- 7. 安全门：渲染进程编 workspace 换不到媒体根 ----------------
  console.log('\n[7] 安全门：workspace 由渲染进程编 → 登记不上（评审裁决 1）')
  {
    // 工作区**外**的一份真媒体文件（.mp4，扩展名完全合法）—— 评审实测的原始攻击载荷
    const leakDir = fs.mkdtempSync(path.join(os.tmpdir(), 'media-leak-'))
    const leak = path.join(leakDir, 'leak.mp4')
    fs.writeFileSync(leak, Buffer.alloc(4096, 7))
    const before = await mediaStats(win)
    const attempt = await win.evaluate(async ([p, ws]) => {
      const url = await window.desktop.mediaUrl({ path: p, workspace: ws })
      const probe = await window.desktop.mediaProbe({ path: p, workspace: ws })
      // 再试一次评审原文的 `workspace=/Users`：走 probe（它先查清单，理由最直白）
      const root = await window.desktop.mediaProbe({ path: '/Users/whatever.mp4', workspace: '/Users' })
      return { url, probe, root }
    }, [leak, leakDir])
    // 计数要在「直连协议」之前取：IPC 被拒的三次一次都不该走到 protocol.handle
    const afterIpc = await mediaStats(win)
    check(attempt.url.ok === false, `工作区外的真 mp4：mediaUrl 必须拒绝（${JSON.stringify(attempt.url)}）`)
    check(/未登记/.test(attempt.url.error ?? ''), `拒绝理由写明「未登记」：${attempt.url.error}`)
    check(attempt.probe.ok === false, `mediaProbe 同样拒绝（第二扇门）：${JSON.stringify(attempt.probe)}`)
    check(attempt.root.ok === false && /未登记/.test(attempt.root.error ?? ''),
      `workspace=/Users 必须被清单拒绝（评审原文）：${JSON.stringify(attempt.root)}`)
    // 根没登记 → 直接构造协议 URL 也拿不到字节（主进程 fetch 绕开 CORS，仍走同一条 handler）
    const leakUrl = 'hai-media://local' + leak.split('/').map(encodeURIComponent).join('/')
    const direct = await app.evaluate(async ({ session }, [u]) => {
      const res = await session.defaultSession.fetch(u)
      return { status: res.status, bytes: (await res.arrayBuffer()).byteLength }
    }, [leakUrl])
    check(direct.status === 403, `直接请求 hai-media:// 也 403（实得 ${direct.status}，${direct.bytes} 字节）`)
    check(direct.bytes < 100, `拒绝响应里没有文件字节（${direct.bytes}）`)
    check(afterIpc.requests === before.requests, `被拒的三次都没有走到协议层（requests ${before.requests}→${afterIpc.requests}）`)
    // 直连那一次**会**走到协议层（403 也是协议给的答案），所以只要求它没送出字节
    const afterDirect = await mediaStats(win)
    check(afterDirect.requests === before.requests + 1, `直连那一次确实打到了协议（${before.requests}→${afterDirect.requests}）`)
    fs.rmSync(leakDir, { recursive: true, force: true })
  }

  check(errs.length === 0, `渲染进程无 console error（${errs.length ? errs.join(' | ') : 'clean'}）`)
  await app.close()
  app = null

  // ======================= 第二次启动：把 ffprobe 从 PATH 里藏掉 =======================
  console.log('\n[8] 降级：藏掉 ffprobe（PATH 不含它的目录）→ 说「无法读取媒体信息」但仍可播放')
  const HOME2 = fs.mkdtempSync(path.join(os.tmpdir(), 'media-native-home2-'))
  // 只留系统目录：homebrew 的 /opt/homebrew/bin（ffprobe 在那里）不在其中。
  // 这一层是**真的换环境**，不是 mock 一次调用 —— LookPath 走的就是这条 PATH。
  // 但 node_modules/.bin/electron 是个 `#!/usr/bin/env node` 的壳脚本：PATH 里没有 node
  // 就连应用都起不来。所以给一个**只含 node** 的垫片目录（ffprobe 仍然不可见）。
  const shim = fs.mkdtempSync(path.join(os.tmpdir(), 'media-native-shim-'))
  fs.symlinkSync(process.execPath, path.join(shim, 'node'))
  const second = await launch({ home: HOME2, env: { PATH: `${shim}:/usr/bin:/bin:/usr/sbin:/sbin` } })
  const win2 = second.win
  let app2 = second.app
  try {
    await openPreview(win2, 'long.mp4')
    await win2.waitForSelector('[data-renderer="media"] video', { timeout: 60000 })
    await waitFor(() => win2.evaluate(() => {
      const v = document.querySelector('[data-renderer="media"] video')
      return !!v && v.readyState >= 1 && v.duration > 0
    }), { label: '降级下 video 仍能解出元数据', timeout: 60000 })
    const s = await win2.evaluate(() => {
      const host = document.querySelector('[data-renderer="media"] .pv-media')
      const v = host?.querySelector('video')
      return {
        probe: host?.getAttribute('data-probe-state'),
        mode: host?.getAttribute('data-playback-mode'),
        warn: host?.querySelector('[data-state="degraded"]')?.textContent ?? '',
        degraded: !!host?.querySelector('[data-state="degraded"]'),
        duration: v?.duration ?? 0,
        ready: v?.readyState ?? 0,
        meta: host?.querySelector('.pv-media-meta')?.textContent ?? '',
      }
    })
    check(s.probe === 'unavailable', `探测状态 unavailable（实得 ${s.probe}）`)
    check(s.degraded && /无法读取媒体信息/.test(s.warn), `面板写明「无法读取媒体信息」（${s.warn.slice(0, 100)}）`)
    check(/ffprobe/.test(s.warn), '原因里点名 ffprobe（用户能据此装/修）')
    check(s.mode === 'direct', `没有 ffprobe 时仍按容器判 direct（实得 ${s.mode}）`)
    check(s.ready >= 1 && Math.abs(s.duration - 3600) < 2, `仍然可播放：元素解出 ${s.duration.toFixed(1)}s（readyState=${s.ready}）`)
    check(/1:00:0\d/.test(s.meta), `时长从解码器补上（元信息：${s.meta.slice(0, 80)}）`)
    await win2.screenshot({ path: path.join(SHOTS, 'm5-degrade-no-ffprobe.png') })
  } finally {
    try { await app2.close() } catch { /* ignore */ }
    fs.rmSync(HOME2, { recursive: true, force: true })
    fs.rmSync(shim, { recursive: true, force: true })
  }

  console.log('\n截图：')
  for (const f of fs.readdirSync(SHOTS)) console.log('  ' + path.join(SHOTS, f))
} catch (e) {
  if (isPrecondition(e)) {
    try { await app?.close() } catch { /* 忽略 */ }
    precondition(e.message)
  }
  console.error('\n❌ ' + (e?.message ?? e))
  fail.push(e?.message ?? String(e))
} finally {
  try { await app?.close() } catch { /* 已关闭 */ }
  cleanup()
  fs.rmSync(HOME, { recursive: true, force: true })
  fs.rmSync(WS, { recursive: true, force: true })
}

if (fail.length) {
  console.error(`\n❌ ${fail.length} 项失败：\n - ` + fail.join('\n - '))
  process.exit(1)
}
console.log(`\n✅ 媒体验收：${pass} 项全部通过（截图在 ${SHOTS}）`)
process.exit(0)
