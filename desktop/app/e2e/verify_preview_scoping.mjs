#!/usr/bin/env node
// R0 验收（真 Electron 端到端）：内嵌预览视图池**按会话隔离**。
//
// 为什么这组断言有意义（每条都防一个「实现错了就静默退化」的点）：
//   内嵌视图池是**进程级单例**（electron/embed.ts 的 views Map），UI 却是**会话**维度：
//   ① 视图不带会话归属时，AI 在后台会话开页会顶掉前台会话正看的视图 → 面板突然空白；
//   ② 切会话后用户看到的是**上一个会话**的页面 → 串会话（视觉与语义都错）；
//   ③ 省事的实现会「切会话就销毁非本会话视图」——这会打断在跑的 AI 命令：Go 侧
//      Enumerate() 靠真实视图池路由 pageId，视图没了命令就落到空处。
//      R0 因此选**可见性隔离 + 全量保活**：一个视图都不销毁，只把本会话的激活视图显示在面板。
//   ④ 「后台产出不抢视线」：Agent 在后台会话产出的页面若抢走激活态，用户正看的页面会被换掉。
//   ⑤ 跨会话激活若放行，面板会被别的会话的视图顶成空白（前端已按会话过滤，这里是兜底）。
//   ⑥ 「切回会话恢复用户那页」：pickSessionView 必须**优先「用户显式看过」的视图**（ADR-0 规格修正②）。
//      否则 Agent 在后台会话产出的页面会把用户原先在看的那页顶掉 —— 这正是「后台产出不抢视线」
//      在**切会话**路径上的实现载体（activate:false 不参与 LRU 挑选，lastUsed 保持 0）。
//      两级优先：① 显式看过的里最近看的 → ② 没有则最近创建的（用户切回时总该看到点东西，而非空白）。
//
// 真隔离必须在 **Electron 侧**成立（不是前端过滤）→ 所以断言读的是 embedStatus() 的
// targets[].sid / targets[].visible（主进程真实视图池状态），而不是 DOM。
//
// 用法：cd desktop/app && npm run e2e:scoping
//   GO_CODE_E2E_FORCE_BUILD=1 → 强制 npm run build（并行改 electron/ 或 src/ 后需要新产物）
// 退出码：全通过 0；任一断言失败非 0（TDD「先红」阶段靠这个非 0 码证明脚本有效）。
import { _electron } from 'playwright'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const REPO = path.resolve(APP, '../..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
// 隔离 HOME（否则会污染用户真实 ~/.go-code）+ 隔离工作区。
// HAI_DATA_DIR 一并置上 → 进「多实例模式」：配置根与 userData（localStorage）都跟着隔离，
// 且跳过单实例锁（用户正开着应用时不隔离会直接抢锁退出，表现成莫名其妙的红）。
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'scoping-e2e-home-'))
const WS = fs.mkdtempSync(path.join(os.tmpdir(), 'scoping-e2e-ws-'))
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

async function waitFor(fn, { timeout = 30000, interval = 250, label = '条件' } = {}) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v } catch { /* 继续 */ }
    await sleep(interval)
  }
  throw new Error(`等待超时: ${label}`)
}

// —— 构建产物（策略同 perm_check.mjs：缺产物才构建）——
const index = path.join(APP, 'dist', 'index.html')
const mainJs = path.join(APP, 'dist-electron', 'main.js')
const needsBuild = process.env.GO_CODE_E2E_FORCE_BUILD === '1' || !fs.existsSync(index) || !fs.existsSync(mainJs)
if (needsBuild) {
  let b = null
  for (let i = 0; i < 3; i++) {
    b = spawnSync('npm', ['run', 'build'], { cwd: APP, encoding: 'utf8' })
    if (b.status === 0) break
    // 并行任务可能正在改 electron/** 或 src/**（半成品 → 编译失败）：等几秒重试。
    // 绝不去改产品代码来「修」构建。
    console.log(`[build] 第 ${i + 1} 次失败，5s 后重试…\n${(b.stderr || b.stdout || '').slice(-600)}`)
    await sleep(5000)
  }
  if (!b || b.status !== 0) throw new Error(`npm run build 失败（重试 3 次）:\n${b?.stdout ?? ''}\n${b?.stderr ?? ''}`)
} else {
  // e2e 跑的是 dist-electron/main.js（不是 electron/main.ts）：产物比源码旧 → R0 的效果看不见。
  // 只提醒、不自动重建（每次 e2e 都构建太慢）。
  const newer = ['electron/embed.ts', 'electron/main.ts', 'electron/preload.ts']
    .map((f) => path.join(APP, f))
    .filter((f) => fs.existsSync(f) && fs.statSync(f).mtimeMs > fs.statSync(mainJs).mtimeMs)
  if (newer.length) {
    console.log(`[warn] dist-electron 比 ${newer.map((f) => path.relative(APP, f)).join(', ')} 旧 → 当前跑的是旧产物；`
      + '验证 R0 请先 npm run build（或 GO_CODE_E2E_FORCE_BUILD=1 npm run e2e:scoping）')
  }
}
const gb = spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], { cwd: REPO, encoding: 'utf8' })
if (gb.status !== 0) throw new Error(`go build 失败:\n${gb.stderr}`)

// 三个待预览的本地文件（内容可区分；file:// 是预览链路真实形态）
const fileA = path.join(WS, 'preview-a.html')
const fileB = path.join(WS, 'preview-b.html')
const fileC = path.join(WS, 'preview-c.html')
// A10/A11 另需两个：同一会话里「用户那页」与「Agent 后台那页」必须能区分开
const fileD = path.join(WS, 'preview-d.html')
const fileE = path.join(WS, 'preview-e.html')
fs.writeFileSync(fileA, '<html><body><h1 id="marker">PREVIEW-A-MARKER</h1></body></html>')
fs.writeFileSync(fileB, '<html><body><h1 id="marker">PREVIEW-B-MARKER</h1></body></html>')
fs.writeFileSync(fileC, '<html><body><h1 id="marker">PREVIEW-C-MARKER</h1></body></html>')
fs.writeFileSync(fileD, '<html><body><h1 id="marker">PREVIEW-D-MARKER</h1></body></html>')
fs.writeFileSync(fileE, '<html><body><h1 id="marker">PREVIEW-E-MARKER</h1></body></html>')
// A9b 专用（阶段 3）：触发 LRU 驱逐用的**全新** URL（A9/A10/A11 用 fileA..fileE，不能混用 ——
// 去重命中会变成「刷新已有标签」，拿不到新视图，驱逐就无从触发）。
const sleepFiles = []
for (let i = 0; i < 14; i++) {
  const p = path.join(WS, `sleep-${i}.html`)
  fs.writeFileSync(p, `<html><body><h1 id="marker">PREVIEW-SLEEP-${i}</h1></body></html>`)
  sleepFiles.push(p)
}
const toFileUrl = (p) => 'file://' + p.split('/').map(encodeURIComponent).join('/')

async function main() {
  let app = null
  try {
    console.log(`[scoping] HOME=${HOME}\n[scoping] WS=${WS}`)
    app = await _electron.launch({
      executablePath: ELECTRON,
      args: ['.'],
      cwd: APP,
      env: { ...process.env, HOME, HAI_DATA_DIR: HOME },
    })
    const win = await app.firstWindow()
    win.on('console', (m) => { if (m.type() === 'error' && !/favicon/.test(m.text())) console.log('[renderer-err]', m.text()) })
    await win.waitForLoadState('domcontentloaded')
    // 注入隔离工作区（userData 已随 HAI_DATA_DIR 隔离，不碰用户真实 localStorage）→ reload 跳过原生选目录
    await win.evaluate(([ws]) => { localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws })) }, [WS])
    await win.reload()
    await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: '主界面就绪', timeout: 60000 })

    // —— A1 前置硬检查：渲染层可见面必须有 embedSetSession ——
    // 不用 skip：它不存在正是本脚本要暴露的事实（TDD 先红）。skip 会把「没实现」伪装成「通过」。
    const setSessionType = await win.evaluate(() => typeof window.desktop?.embedSetSession)
    if (setSessionType !== 'function') {
      throw new Error(`embedSetSession 不存在 —— R0 未实现（window.desktop.embedSetSession 的 typeof = ${JSON.stringify(setSessionType)}）`)
    }
    ok('A1 window.desktop.embedSetSession 已接线（typeof function）')

    // 等内嵌链路就绪：store 自举在启动期会 embedShow + embedHide 各一次（视图零常驻），
    // 等它跑完再断言，避免「自举把面板全局可见位翻 false」与断言竞态。
    // 非致命：即便等不到（bridge 异常），下面的显式 embedShow 也会建出视图，断言依然成立。
    try {
      await waitFor(async () => !!(await win.evaluate(() => window.desktop.embedStatus()))?.ready, { label: '内嵌视图池就绪', timeout: 60000 })
      await sleep(1500)
    } catch (e) {
      console.log(`  ⚠ 视图池就绪等待超时（继续，由显式 embedShow 兜底）：${e.message}`)
    }

    // 页面内测试辅助（复用同一份逻辑，避免每个 evaluate 重写）：
    //  - showPanel：真实流程里由 RightPanel 打开网页面板时调 embedShow —— 它是 targets[].visible 的前提
    //  - openAndId：用「打开前后 targets 差集」定位新视图 id，不依赖实现把 id 放在响应的哪个字段
    await win.evaluate(() => {
      window.__sc = {
        showPanel: () => window.desktop.embedShow({}),
        snap: async () => {
          const st = await window.desktop.embedStatus()
          return {
            activeId: st?.activeId ?? null,
            panelVisible: !!st?.visible,
            // sleeping：阶段 3 的休眠态（视图池 LRU 驱逐后条目**仍以原 id 在 targets 里**，
            // 见 docs/RIGHT_PANEL_TABS_PLAN.md §11.4）—— A9/A9b 靠它区分「关闭」与「休眠」。
            targets: (st?.targets ?? []).map((t) => ({ id: t.id, url: t.url, sid: t.sid, visible: t.visible, sleeping: !!t.sleeping })),
          }
        },
        target: async (id) => (await window.__sc.snap()).targets.find((t) => t.id === id) ?? null,
        openAndId: async (url, opts) => {
          const before = new Set(((await window.desktop.embedStatus())?.targets ?? []).map((t) => t.id))
          const r = await window.desktop.embedOpenTarget(url, opts)
          const fresh = ((await window.desktop.embedStatus())?.targets ?? []).filter((t) => !before.has(t.id))
          return { ok: !!r?.ok, error: r?.error ?? '', id: fresh[0]?.id ?? '', state: r?.state ?? null }
        },
      }
    })

    // readTarget(id, expectVisible)：先 embedShow（面板可见是 visible 的前提）再读该 target。
    // 重试 6 次的原因：启动期 store 自举会 embedShow+embedHide 各一次，若正好插在读之间会把
    // 面板全局可见位翻成 false —— 那是与「会话隔离」无关的抖动。断言仍是最终判据：
    // 6 次都不匹配就返回最后一次观测，由调用方按实际值失败。
    const readTarget = async (id, expectVisible) => {
      let last = null
      for (let i = 0; i < 6; i++) {
        await win.evaluate(() => window.__sc.showPanel())
        await sleep(120)
        last = await win.evaluate((tid) => window.__sc.target(tid), id)
        if (!!last?.visible === expectVisible) break
      }
      return last
    }

    // —— A2 开 A（sid=S-A）：视图带会话归属 + 当前会话内显示 ——
    const openA = await win.evaluate(async ([url]) => {
      await window.desktop.embedSetSession('S-A') // 真实流程：用户当前就在 S-A 会话
      await window.__sc.showPanel()              // 真实流程：右栏网页面板已打开
      const r = await window.__sc.openAndId(url, { sid: 'S-A', activate: true })
      await new Promise((res) => setTimeout(res, 700)) // 等 file:// 加载出 url/title
      return r
    }, [toFileUrl(fileA)])
    assert(openA.ok === true, `A2 openEmbedTarget(url,{sid:'S-A',activate:true}) 应返回 ok（实际 ok=${openA.ok}，error=${JSON.stringify(openA.error)}）`)
    assert(!!openA.id, `A2 新视图应出现在 targets 里（打开前后差集为空 → 没真建视图；state=${JSON.stringify(openA.state)}）`)
    const tA1 = await readTarget(openA.id, true)
    assert(tA1?.sid === 'S-A', `A2 target 应带会话归属 sid='S-A'（实际 sid=${JSON.stringify(tA1?.sid)}，target=${JSON.stringify(tA1)}）`)
    assert(tA1?.visible === true, `A2 当前会话 S-A 的激活视图应显示（visible 期望 true，实际 ${JSON.stringify(tA1?.visible)}，target=${JSON.stringify(tA1)}）`)

    // —— A3/A4 切到 S-B：真隔离 + 保活 ——
    const snapB = await win.evaluate(async () => {
      await window.desktop.embedSetSession('S-B') // 切到另一个会话（此刻本会话还没有任何视图）
      await window.__sc.showPanel()
      return await window.__sc.snap()
    })
    const kept = snapB.targets.find((t) => t.id === openA.id) ?? null
    assert(!!kept, `A4 切会话不得销毁视图（保活）：S-A 的 target 必须仍在 targets 里——Go 侧 Enumerate/pageId 路由依赖真实视图池；实际 targets=${JSON.stringify(snapB.targets)}`)
    const tA2 = await readTarget(openA.id, false)
    assert(tA2?.visible === false, `A3 切到 S-B 后 S-A 的视图必须真的不显示（Electron 侧隔离，不是前端过滤）：visible 期望 false，实际 ${JSON.stringify(tA2?.visible)}（activeId=${JSON.stringify(snapB.activeId)}）`)

    // —— A5 切回 S-A：恢复显示 ——
    await win.evaluate(() => window.desktop.embedSetSession('S-A'))
    const tA3 = await readTarget(openA.id, true)
    assert(tA3?.visible === true, `A5 切回 S-A 应恢复显示（visible 期望 true，实际 ${JSON.stringify(tA3?.visible)}，target=${JSON.stringify(tA3)}）`)

    // —— A6 两会话并存：各会话只看自己的 ——
    const openB = await win.evaluate(async ([url]) => {
      await window.desktop.embedSetSession('S-B')
      await window.__sc.showPanel()
      const r = await window.__sc.openAndId(url, { sid: 'S-B', activate: true })
      await new Promise((res) => setTimeout(res, 700))
      return r
    }, [toFileUrl(fileB)])
    assert(openB.ok === true, `A6 openEmbedTarget(url,{sid:'S-B',activate:true}) 应返回 ok（实际 ok=${openB.ok}，error=${JSON.stringify(openB.error)}）`)
    assert(!!openB.id, `A6 新视图应出现在 targets 里（state=${JSON.stringify(openB.state)}）`)
    const tB1 = await readTarget(openB.id, true)
    assert(tB1?.sid === 'S-B', `A6 target 应带会话归属 sid='S-B'（实际 sid=${JSON.stringify(tB1?.sid)}，target=${JSON.stringify(tB1)}）`)
    assert(tB1?.visible === true, `A6 当前会话 S-B 的激活视图应显示（visible 期望 true，实际 ${JSON.stringify(tB1?.visible)}）`)
    const tA4 = await readTarget(openA.id, false)
    assert(tA4?.visible === false, `A6 各会话只看自己的：S-A 的视图在 S-B 下必须隐藏（visible 期望 false，实际 ${JSON.stringify(tA4?.visible)}）`)
    const both = await win.evaluate(() => window.__sc.snap())
    assert([openA.id, openB.id].every((id) => both.targets.some((t) => t.id === id)),
      `A6 两个会话的视图必须并存于池中（A=${openA.id} B=${openB.id}，实际 targets=${JSON.stringify(both.targets)}）`)

    // —— A7 activate:false 不抢激活态（后台产出不抢视线）——
    const openC = await win.evaluate(async ([url]) => {
      const before = await window.__sc.snap()
      const r = await window.__sc.openAndId(url, { sid: 'S-B', activate: false })
      await new Promise((res) => setTimeout(res, 500))
      await window.__sc.showPanel()
      const after = await window.__sc.snap()
      return { r, beforeActive: before.activeId, afterActive: after.activeId, target: after.targets.find((t) => t.id === r.id) ?? null }
    }, [toFileUrl(fileC)])
    assert(openC.r.ok === true, `A7 activate:false 也应正常建视图（实际 ok=${openC.r.ok}，error=${JSON.stringify(openC.r.error)}）`)
    assert(openC.afterActive === openC.beforeActive, `A7 activate:false 不得改激活态（后台产出不抢视线）：期望 activeId 仍为 ${JSON.stringify(openC.beforeActive)}，实际 ${JSON.stringify(openC.afterActive)}`)
    assert(openC.target?.sid === 'S-B', `A7 新视图仍应归属当前会话 sid='S-B'（实际 sid=${JSON.stringify(openC.target?.sid)}）`)
    assert(openC.target?.visible === false, `A7 非激活视图不得显示（visible 期望 false，实际 ${JSON.stringify(openC.target?.visible)}）`)

    // —— A8 跨会话激活拒绝 ——
    const cross = await win.evaluate(async ([idA]) => {
      await window.desktop.embedSetSession('S-B')
      await window.__sc.showPanel()
      const before = (await window.__sc.snap()).activeId
      const r = await window.desktop.embedActivate(idA)
      await window.__sc.showPanel()
      const after = (await window.__sc.snap()).activeId
      return { before, after, ok: !!r?.ok }
    }, [openA.id])
    // 前置只锁「属于 S-B」：setEmbedSession 重挑哪一个本会话视图是**实现细节**（LRU / 首个都合规），
    // 断言锁死具体 id 会把实现选择当成规格，制造假红。
    assert([openB.id, openC.r.id].includes(cross.before),
      `A8 前置：切到 S-B 后激活视图应是 S-B 自己的视图之一（B=${openB.id} / C=${openC.r.id}），实际 ${JSON.stringify(cross.before)}`)
    assert(cross.after === cross.before, `A8 跨会话激活必须被拒绝：在 S-B 下调 embedActivate(S-A 的视图 ${openA.id}) 不得改激活态（期望仍为 ${JSON.stringify(cross.before)}，实际 ${JSON.stringify(cross.after)}）`)

    // —— A9 逐个关闭：target 真消失（= **真删除**，与「休眠」区分开）——
    // 阶段 3 起「从 targets 消失」只属于真关闭：LRU 驱逐是**休眠**（条目仍以原 id 在 targets
    // 里，带 sleeping:true），故这里额外钉住「关闭后不得以休眠形态残留」。
    for (const [label, id] of [['A', openA.id], ['B', openB.id], ['C', openC.r.id]]) {
      const r = await win.evaluate(async ([tid]) => {
        const before = await window.__sc.snap()
        const res = await window.desktop.embedCloseTarget(tid)
        const after = await window.__sc.snap()
        return { ok: !!res?.ok, error: res?.error ?? '', n0: before.targets.length, n1: after.targets.length, ids: after.targets.map((t) => t.id), sleeping: after.targets.filter((t) => t.sleeping).map((t) => t.id) }
      }, [id])
      assert(r.ok === true, `A9 embedCloseTarget(${id}) 应返回 ok（实际 ok=${r.ok}，error=${JSON.stringify(r.error)}）`)
      assert(!r.ids.includes(id), `A9 关闭后 target ${id}（${label}）必须从 targets 消失（实际 targets=${JSON.stringify(r.ids)}）`)
      assert(!r.sleeping.includes(id), `A9 关闭是**真删除**，不得以「休眠」形态残留（休眠条目仍在 targets 里带 sleeping:true；实际 sleeping=${JSON.stringify(r.sleeping)}）`)
      assert(r.n1 < r.n0, `A9 关闭后 targets 数量应减少（${label}：${r.n0} → ${r.n1}）`)
    }

    // —— A9b 休眠 ≠ 关闭（阶段 3 的核心区分）——
    // 触发一次 LRU 驱逐（连开超过活条目上限 12 个「用户看过」的视图），然后钉住：
    //   ① 被驱逐的条目**仍在 targets 里**（原 id + sleeping:true）—— 标签身份不丢，
    //      渲染层据此保住标签（syncWebTabs 把「key 仍在」当页面存在，见 §11.4）；
    //   ② 它此时**不是**「已关闭」：还在 targets、id 不变（对比 A9 的真删除）；
    //   ③ 此时关掉它 = 真删除（从 targets 消失）—— 「关闭」的语义没有被休眠改写。
    // ③ 同时是清理（不留 12 个视图给 A10/A11）。
    const a9b = await win.evaluate(async ([urls]) => {
      const before = new Set((await window.__sc.snap()).targets.map((t) => t.id))
      const created = []
      let allOk = true
      for (const u of urls) {
        const r = await window.__sc.openAndId(u, { sid: 'S-SLEEP', activate: true }) // 用户显式打开 → 参与 LRU
        if (!r.ok) { allOk = false; break }
        if (r.id) created.push(r.id)
      }
      const st = await window.__sc.snap()
      const sleeping = st.targets.filter((t) => t.sleeping).map((t) => t.id)
      const victim = sleeping[0] ?? ''
      const victimTarget = st.targets.find((t) => t.id === victim) ?? null
      // 关掉被休眠的那条 → 真删除
      const closed = await window.desktop.embedCloseTarget(victim)
      const after = await window.__sc.snap()
      // 清理：关掉本次新建的其余视图（不留满池给 A10/A11）
      for (const id of created) if (id !== victim) await window.desktop.embedCloseTarget(id)
      const cleaned = await window.__sc.snap()
      return {
        allOk, created, sleeping, victim,
        victimInTargets: !!victimTarget, victimSleeping: victimTarget?.sleeping === true,
        closedOk: !!closed?.ok,
        goneAfterClose: !after.targets.some((t) => t.id === victim),
        beforeCount: before.size,
        live: st.targets.filter((t) => !t.sleeping).length,
        total: st.targets.length,
        cleanedLive: cleaned.targets.filter((t) => !t.sleeping).length,
      }
    }, [sleepFiles.map(toFileUrl)])
    assert(a9b.allOk, `A9b 超限**不再抛错**（连开 14 个「用户看过」的视图全部 ok —— 阶段 3：LRU 驱逐取代报错）`)
    assert(a9b.sleeping.length >= 1 && !!a9b.victim, `A9b 确实发生了休眠驱逐（否则下面的区分断言是空转；sleeping=${JSON.stringify(a9b.sleeping)}）`)
    assert(a9b.victimInTargets === true && a9b.victimSleeping === true,
      `A9b 被驱逐的条目必须**仍在 targets 里**（原 id + sleeping:true）—— 标签身份靠它，掉了标签就会被当「已关闭」删掉；实际 victim=${a9b.victim} target=${JSON.stringify(a9b.victimInTargets)} sleeping=${JSON.stringify(a9b.victimSleeping)}`)
    assert(a9b.live <= 12, `A9b 活条目数不突破上限 12（上限口径 = 活渲染进程数；实际 live=${a9b.live} total=${a9b.total}）`)
    assert(a9b.closedOk === true && a9b.goneAfterClose === true,
      `A9b 休眠条目被**关闭**时才是真删除（从 targets 消失）：closedOk=${JSON.stringify(a9b.closedOk)} gone=${JSON.stringify(a9b.goneAfterClose)}`)
    assert(a9b.cleanedLive <= 2, `A9b 清理：本次新建的视图已关闭（留给 A10/A11 的池是干净的；实际 live=${a9b.cleanedLive}）`)
    // —— A10/A11 切回会话恢复「用户那页」（ADR-0 规格修正②：pickSessionView 两级优先）——
    // A8 的注释曾把「重挑哪一个本会话视图」当实现细节、只锁「属于本会话」；但修正②已把它定为
    // 规格，且它是「Agent 后台产出不把用户原先在看的那页顶掉」的**唯一实现载体** → 这里锁死具体 id。
    const pick = await win.evaluate(async ([u1, u2, u3, u4, u5]) => {
      const open = (url, opts) => window.__sc.openAndId(url, opts)
      const panel = () => window.__sc.showPanel()
      // S-A：用户显式看过 P1（有显式视图的会话，供「切走」这一半）
      await window.desktop.embedSetSession('S-A')
      await panel()
      const p1 = await open(u1, { sid: 'S-A', activate: true })
      // S-B：用户显式看过 P2 → Agent 在后台开 P3（activate:false，不得参与挑选）
      await window.desktop.embedSetSession('S-B')
      await panel()
      const p2 = await open(u2, { sid: 'S-B', activate: true })
      const p3 = await open(u3, { sid: 'S-B', activate: false })
      const afterBg = (await window.__sc.snap()).activeId
      // 切走 → 切回 S-B：必须回到用户看的 P2，而不是 Agent 刚开的 P3
      await window.desktop.embedSetSession('S-A')
      await window.desktop.embedSetSession('S-B')
      await panel()
      const backB = (await window.__sc.snap()).activeId
      // S-C：只有后台产出（两个）、没有任何「显式看过」→ 走第二级（最近创建的）
      await window.desktop.embedSetSession('S-C')
      await panel()
      const q1 = await open(u4, { sid: 'S-C', activate: false })
      const q2 = await open(u5, { sid: 'S-C', activate: false })
      await window.desktop.embedSetSession('S-A')
      await window.desktop.embedSetSession('S-C')
      await panel()
      const backC = (await window.__sc.snap()).activeId
      return { p1: p1.id, p2: p2.id, p3: p3.id, q1: q1.id, q2: q2.id, afterBg, backB, backC }
    }, [toFileUrl(fileA), toFileUrl(fileB), toFileUrl(fileC), toFileUrl(fileD), toFileUrl(fileE)])
    assert(pick.afterBg === pick.p2, `A10 前置：Agent 后台开页后激活态应仍是用户那页 ${pick.p2}（实际 ${JSON.stringify(pick.afterBg)}）`)
    assert(pick.backB === pick.p2, `A10 切回会话应恢复「用户显式看过的那页」${pick.p2}，而不是 Agent 后台刚开的 ${pick.p3}（实际 activeId=${JSON.stringify(pick.backB)}；ADR-0 修正②）`)
    assert(pick.backC === pick.q2, `A11 本会话只有后台产出时应挑最近创建的 ${pick.q2}（不能空白、也不能挑更早的 ${pick.q1}；实际 activeId=${JSON.stringify(pick.backC)}）`)
  } finally {
    if (app) await app.close().catch(() => { /* 已退出忽略 */ })
    fs.rmSync(HOME, { recursive: true, force: true })
    fs.rmSync(WS, { recursive: true, force: true })
  }
}

main().then(() => {
  console.log(`\n✅ 内嵌预览视图池会话隔离：${pass} 项全部通过`)
  process.exit(0)
}).catch((e) => {
  console.error(`\n❌ ${e?.message ?? e}`)
  process.exit(1)
})
