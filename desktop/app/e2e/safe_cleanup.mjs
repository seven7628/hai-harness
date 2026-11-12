// safe_cleanup.mjs — E2E 进程清理的**安全护栏**（所有 e2e 脚本共用）。
//
// 背景（严重事故）：早期 E2E 用 `pkill -9 -f <泛字符串>` 清孤儿进程，字符串取的是
// 通用名（`chrome-devtools-mcp` / `browser-profile` / `hai-desktop`）。这些名字
// 同样出现在**用户真实会话**的命令行里：
//   - 用户日常 Chrome：`--user-data-dir=~/.go-code/browser-profile` → 命中 `browser-profile`
//   - 用户真实 MCP ：`node .../chrome-devtools-mcp/build/src/index.js` → 命中 `chrome-devtools-mcp`
// 结果：跑一次 E2E 就把用户正开着的 Chrome / 浏览器会话 SIGKILL 掉，登录态随之中断。
//
// 整改原则：**E2E 只允许回收「自己启动的」进程**。判定方式不靠进程名，而靠
// 「命令行里是否含本次 E2E 的隔离路径标记」——用户真实进程不含这些标记，天然免疫。
//
// 用法：
//   const { killByMarkers, assertNoForeignKill } = createCleaner({ home: HOME, ws: WS })

import { execFileSync } from 'node:child_process'
import path from 'node:path'

/** 读进程表：返回 [{ pid, command }]。用 ps 而非 /proc（macOS 无 /proc）。 */
export function listProcesses() {
  const out = execFileSync('/bin/ps', ['-eo', 'pid=,command='], { encoding: 'utf8', maxBuffer: 32 * 1024 * 1024 })
  return out
    .split('\n')
    .map((line) => {
      const m = /^\s*(\d+)\s+(.*)$/.exec(line)
      return m ? { pid: Number(m[1]), command: m[2] } : null
    })
    .filter(Boolean)
}

/** 本进程及其祖先链的 pid（避免自杀 / 杀父进程）。 */
function selfAndAncestors() {
  const pids = new Set([process.pid])
  let pid = process.pid
  for (let i = 0; i < 16; i++) {
    let ppid = 0
    try {
      ppid = Number(execFileSync('/bin/ps', ['-o', 'ppid=', '-p', String(pid)], { encoding: 'utf8' }).trim())
    } catch {
      break
    }
    if (!ppid || ppid <= 1) break
    pids.add(ppid)
    pid = ppid
  }
  return pids
}

/**
 * 创建只针对本 E2E 隔离环境的清理器。
 *
 * @param {{ markers: string[] }} opts markers = 本次 E2E 特有的路径标记（如隔离 HOME 路径）。
 *   命令行必须**包含其中至少一个**标记才会被回收；标记越长越具体越安全。
 */
export function createCleaner({ markers }) {
  if (!Array.isArray(markers) || markers.length === 0) {
    throw new Error('createCleaner: markers 不能为空（空标记会退化成泛匹配 pkill，禁止）')
  }
  // 防呆：标记必须够具体（拒绝 'chrome' / 'browser' 这类会误伤真实进程的短通用词）。
  for (const m of markers) {
    if (typeof m !== 'string' || m.length < 8 || !m.includes('/')) {
      throw new Error(`createCleaner: 标记 ${JSON.stringify(m)} 过于宽泛（需为 >=8 字符且含 "/" 的路径片段）`)
    }
  }

  const protectedPids = selfAndAncestors()

  return {
    /**
     * 回收命令行含任一标记的进程。返回被杀的 pid 列表。
     * 跳过自身/祖先；先 SIGTERM 优雅退，再对残留 SIGKILL。
     */
    killByMarkers({ graceMs = 200 } = {}) {
      const victims = listProcesses().filter(
        (p) => !protectedPids.has(p.pid) && markers.some((m) => p.command.includes(m)),
      )
      if (victims.length === 0) return []

      for (const v of victims) {
        try {
          process.kill(v.pid, 'SIGTERM')
        } catch { /* 已退出 */ }
      }
      if (graceMs > 0) {
        const end = Date.now() + graceMs
        while (Date.now() < end) { /* busy-wait 让出优雅退出窗口 */ }
      }
      for (const v of victims) {
        try {
          process.kill(v.pid, 0) // 仍存在 → 升级强杀
          process.kill(v.pid, 'SIGKILL')
        } catch { /* 已退出 */ }
      }
      return victims.map((v) => v.pid)
    },

    /** 供测试断言：列出会被本清理器命中的进程（不执行任何 kill）。 */
    previewVictims() {
      return listProcesses()
        .filter((p) => !protectedPids.has(p.pid) && markers.some((m) => p.command.includes(m)))
        .map((p) => ({ pid: p.pid, command: p.command }))
    },
  }
}

/**
 * 断言不会误伤用户真实进程（销毁前自检）。
 *
 * 判定用「真实用户目录」的精确路径，而非通用子串——否则 E2E 自己的隔离 profile
 * （`<隔离HOME>/.go-code/browser-profile`）会被 `.go-code/browser-profile` 子串误判命中，
 * 导致清理反被中止。realHome 必须是**未被子进程隔离覆盖**的真实用户主目录。
 */
export function assertNoForeignKill(victims, { realHome } = {}) {
  if (!realHome) return
  const FOREIGN = [
    path.join(realHome, 'Library', 'Application Support', 'Google', 'Chrome'), // 用户日常 Chrome
    path.join(realHome, '.go-code', 'browser-profile'), // 用户真实受管 profile
    path.join(realHome, 'Library', 'Application Support', 'hai-desktop'), // 用户真实桌面应用（现名）
    path.join(realHome, 'Library', 'Application Support', 'go-code-desktop'), // 同上（2026-09-23 之前的名字，机器上可能还在）
  ]
  for (const v of victims) {
    for (const f of FOREIGN) {
      if (v.command.includes(f)) {
        throw new Error(`安全中止：清理目标 ${v.pid} 属于用户真实环境（命中 "${f}"）：${v.command.slice(0, 200)}`)
      }
    }
  }
}
