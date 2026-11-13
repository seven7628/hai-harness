// e2e/_serve.mjs —— 探针 / 预览服务器的共用启动件（零依赖，只用 Node 内置模块）。
//
// 为什么不再写死端口：默认链一次跑 9 个带服务器的 e2e 脚本，前一个脚本 stopServer()
// 发的是 SIGTERM（异步），端口可能还没释放；后一个脚本用同一个固定端口（4191/4193/4199…）
// `--strictPort` 启动即失败，而它自己的 waitPort 却连上了**前一个还没退干净的进程** ——
// 于是 page.goto 拿到 ERR_CONNECTION_REFUSED（R05a3b 实测的 e2e:ctx-panel 链内竞态：
// 单独跑 73 项全过，链里跑第 68 步必红）。
//
// 两道判据都换了，不靠「多跑几次」：
//   1. 端口由内核分配（bind 0 → 读回 → 让给 vite）—— 从根上没有碰撞可能；
//   2. 就绪判据从「有东西在监听」升级为「**这张**探针页真的返回 200」，且子进程一退出
//      立刻判失败（连上别人的端口不会再被当成就绪）。

import net from 'node:net'

// freePort 向内核要一个当前空闲的端口（绑 0 取回后立刻释放，交给 vite 去 bind）。
export async function freePort(host = '127.0.0.1') {
  return new Promise((resolve, reject) => {
    const srv = net.createServer()
    srv.unref()
    srv.on('error', reject)
    srv.listen(0, host, () => {
      const port = srv.address().port
      srv.close(() => resolve(port))
    })
  })
}

// waitHttp 探针页真的能开（200）。child 一退出立刻抛 —— 端口被别人的进程占着时，
// vite 会带着 strictPort 退出，那种情况下「能连上」不等于「我们准备好了」。
export async function waitHttp(url, { timeoutMs = 120_000, child } = {}) {
  const deadline = Date.now() + timeoutMs
  let last = ''
  for (;;) {
    if (child && child.exitCode != null) {
      throw new Error(`服务器已退出（code ${child.exitCode}），探针页不可用：${url}`)
    }
    try {
      const res = await fetch(url, { redirect: 'follow' })
      if (res.ok) return
      last = `HTTP ${res.status}`
    } catch (e) {
      last = e instanceof Error ? e.message : String(e)
    }
    if (Date.now() > deadline) throw new Error(`探针页未就绪（${last}）：${url}`)
    await new Promise((r) => setTimeout(r, 250))
  }
}

// —— 端口归属自检 + 「前置不满足」的退出码契约（00 §4.4 / 04 F45、F46）——
//
// waitHttp 只证明「**这张 URL** 回了 200」，证明不了「回话的是我刚起的那个服务器」。
// F46 实测：一个残留在 5199 的 vite（**另一棵 worktree**）让探针连上了别人的模块图 →
// 探针页 404 / 永远等不到 __store，报出来的却是 waitForFunction 超时，看起来像代码坏了。
// 判据取探针页自己的 <title>（每个探针生成的 index.html 里都写了一个唯一标记）：
//   · 200 且带标记      → 就是我们的页面，放行；
//   · 200 但不带标记    → **别人的服务器**（自己的探针页在 spawn 之前就落盘了，不存在竞态）；
//   · 子进程先退出      → strictPort 撞了别人的端口，或探针编译失败（看 stderr）；
//   · 超时              → 前置没满足。
// 三种失败都抛 ProbePrecondition（exitCode 2）：链上看得见，但不会被读成「代码坏了」，
// 也不许静默 skip（00 §4.6 红归因）。
export class ProbePrecondition extends Error {
  constructor(message) {
    super(message)
    this.name = 'ProbePrecondition'
    this.exitCode = 2
  }
}

export const isPrecondition = (e) => e instanceof ProbePrecondition

// 各探针的 catch 收尾统一走这里：前置不满足 → 2，断言失败 → 1（绝不静默 0）。
export const exitCodeOf = (e) => (isPrecondition(e) ? 2 : 1)

export async function waitProbePage(url, { marker, probe = url, child, timeoutMs = 30_000 } = {}) {
  const deadline = Date.now() + timeoutMs
  let last = ''
  for (;;) {
    if (child && child.exitCode != null) {
      throw new ProbePrecondition(
        `${probe}: 探针服务器已退出（code ${child.exitCode}）—— ${url} 的端口被别的进程占着（--strictPort 会直接退出）`
        + '，或探针页编译失败（见上方 stderr）',
      )
    }
    try {
      const res = await fetch(url, { redirect: 'follow' })
      if (res.ok) {
        const body = await res.text()
        if (!marker || body.includes(marker)) return
        throw new ProbePrecondition(
          `${probe}: ${url} 回了 200，但页面里找不到 ${JSON.stringify(marker)}`
          + `（实得 ${JSON.stringify(body.slice(0, 120).replace(/\s+/g, ' '))}）—— 端口上跑的是**别人的**服务器`,
        )
      }
      last = `HTTP ${res.status}`
    } catch (e) {
      if (isPrecondition(e)) throw e
      last = e instanceof Error ? e.message : String(e)
    }
    if (Date.now() > deadline) {
      throw new ProbePrecondition(`${probe}: 探针页 ${timeoutMs}ms 内没就绪（最后：${last}）：${url}`)
    }
    await new Promise((r) => setTimeout(r, 250))
  }
}
