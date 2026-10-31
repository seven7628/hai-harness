#!/usr/bin/env node
// 释放「本项目」遗留的 dev 进程：占端口的 vite + 抢着单实例锁的 Electron。
//
// 为什么需要它（2026-10-29 用户报障「怎么又崩溃了」）：
//   dev 脚本原本是 `concurrently -k` —— 任一子进程退出就把整组带塌。
//   于是一次外部 SIGTERM（误杀进程组）= 桌面端 + vite 一起消失，日志里
//   只剩「Network service crashed」这种看不出所以然的痕迹。
//   去掉 -k 之后各子进程互不牵连（死一个不会带走另一个，日志也能看出谁先死），
//   但代价是 vite 会残留占住端口、Electron 会残留占住单实例锁：
//   下一次 `npm run dev` 要么撞端口，要么新 Electron 秒退（抢不到锁）。
//   本脚本在 dev 启动前把**本项目**的遗留进程精确清掉，重启就是确定的。
//
// 匹配必须精确到本项目的绝对路径：本机还有 hai-studio 也在跑 vite
// （端口 5273/5199）。按名字 pkill vite/npm 会把两边的栈一起打死 ——
// 那才是真正制造「崩溃」的那只手。
import { execFileSync } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'

const APP = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const VITE_PORT = Number(process.env.VITE_PORT || 5173)

// vite 判定：本项目 node_modules 下的 vite，且正在监听 VITE_PORT
const VITE_MARK = path.join(APP, 'node_modules', 'vite')
// Electron 判定：本项目 node_modules 下的 Electron 二进制
const ELECTRON_MARK = path.join(APP, 'node_modules', 'electron', 'dist')

function run(cmd, args) {
  try {
    return execFileSync(cmd, args, { encoding: 'utf8' }).trim()
  } catch {
    return ''
  }
}

function alive(pid) {
  try {
    process.kill(pid, 0)
    return true
  } catch {
    return false
  }
}

// SIGTERM → 等 → SIGKILL。只对本项目自己的进程下手。
function stop(pid, label) {
  if (!alive(pid)) return
  console.log(`[free-stale-stack] ${label} pid=${pid} → SIGTERM`)
  try {
    process.kill(pid, 'SIGTERM')
  } catch {
    return
  }
  for (let i = 0; i < 25; i++) {
    if (!alive(pid)) {
      console.log(`[free-stale-stack] ${label} pid=${pid} 已退出`)
      return
    }
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 200)
  }
  console.log(`[free-stale-stack] ${label} pid=${pid} 未响应 SIGTERM → SIGKILL`)
  try {
    process.kill(pid, 'SIGKILL')
  } catch {
    /* 已经没了 */
  }
}

// 1) 占着 vite 端口的、且是本项目的 vite
const listeners = run('lsof', ['-nP', `-iTCP:${VITE_PORT}`, '-sTCP:LISTEN', '-t'])
const vitePids = new Set()
for (const line of listeners.split('\n')) {
  const pid = Number(line.trim())
  if (!Number.isInteger(pid) || pid <= 0) continue
  const cmd = run('ps', ['-o', 'command=', '-p', String(pid)])
  if (!cmd.includes(VITE_MARK)) {
    console.log(`[free-stale-stack] 端口 ${VITE_PORT} 被其它程序占用（pid=${pid}，非本项目 vite），跳过`)
    continue
  }
  vitePids.add(pid)
}
for (const pid of vitePids) stop(pid, '遗留 vite')

// 2) 本项目遗留的 Electron（会占住单实例锁，让新实例秒退）
const psAll = run('ps', ['-ax', '-o', 'pid=,command='])
for (const line of psAll.split('\n')) {
  const m = line.trim().match(/^(\d+)\s+(.*)$/)
  if (!m) continue
  const pid = Number(m[1])
  const cmd = m[2]
  if (pid === process.pid) continue
  if (!cmd.includes(ELECTRON_MARK)) continue
  if (cmd.includes('node_modules/.bin/concurrently')) continue
  stop(pid, '遗留 Electron')
}

if (vitePids.size === 0) {
  console.log(`[free-stale-stack] 端口 ${VITE_PORT} 空闲，无遗留进程`)
}
