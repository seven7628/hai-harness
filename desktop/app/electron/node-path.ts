// node-path.ts — 常见 node 安装位置探测（纯 Node，无 Electron 依赖，可单测）。
//
// 背景：桌面 App 由 Finder/Dock 启动时，Electron 进程继承的是 launchd 的最小 PATH
// （/usr/bin:/bin:/usr/sbin:/sbin），不含用户 shell 配置——Homebrew/官网 pkg/
// nvm/fnm/volta 装的 node 都不在 PATH 上，Go bridge（spawn 子进程）的
// exec.LookPath("node") 因此误报「未检测到 Node.js」，即使本机确实装了 node。
//
// 本模块在 PATH 缺 node 时探测常见安装位置，把 node 所在 bin 目录并入 spawn env
// 的 PATH。与 Go 侧（plugin/browser/install.go nodeCandidates）的候选目录兜底
// 互为双保险：Electron 注入保证整条 child 链一致；Go 侧候选保证 bridge 脱离
// Electron 自启（测试/裸跑）场景也不误报。
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'

// isExecFile 判定 path 为已存在文件（node 可执行）；目录/不存在 → false。
function isExecFile(p: string): boolean {
  try {
    return fs.statSync(p).isFile()
  } catch {
    return false
  }
}

// versionNums 解析版本目录名（"v20.11.1" → [20,11,1]、"18" → [18]）；
// 不可解析（非纯数字段）→ []。用于多版本根取最新。
export function versionNums(s: string): number[] {
  const n = s.replace(/^v/, '')
  if (!/^[\d.]+$/.test(n)) return []
  return n.split('.').map((x) => parseInt(x, 10) || 0)
}

// cmpVersion 逐段比较版本数字（21.7.3 > 8.17.0——字符串比较会误判）；缺段视为 0。
function cmpVersion(a: string, b: string): number {
  const av = versionNums(a)
  const bv = versionNums(b)
  const len = Math.max(av.length, bv.length)
  for (let i = 0; i < len; i++) {
    const x = i < av.length ? av[i] : 0
    const y = i < bv.length ? bv[i] : 0
    if (x !== y) return x - y
  }
  return 0
}

// nodeDirCandidates 直接含 node 可执行文件的 bin 目录（macOS GUI 启动场景）。
// GO_CODE_NODE_PATH_OVERRIDE（':' 分隔）整体覆盖直接 bin 候选——测试隔离用
// （屏蔽本机真实 node 的固定路径候选）；正常不设。
export function nodeDirCandidates(home: string, platform: NodeJS.Platform): string[] {
  const ov = process.env.GO_CODE_NODE_PATH_OVERRIDE
  if (ov) return ov.split(path.delimiter).filter(Boolean)
  const dirs = [
    '/opt/homebrew/bin', // Apple Silicon Homebrew
    '/usr/local/bin', // 官网 pkg / Intel Homebrew
    '/opt/local/bin', // MacPorts
    home + '/.volta/bin', // volta shim
    home + '/.local/bin', // 用户级 ~/.local/bin
  ]
  if (platform === 'win32') {
    // Windows 官方安装包默认目录（PATH 注入兜底；GUI 启动同样可能缺）
    return [
      ...dirs,
      home + '\\AppData\\Local\\Programs\\nodejs',
      process.env.ProgramFiles ? process.env.ProgramFiles + '\\nodejs' : '',
    ].filter(Boolean)
  }
  return dirs
}

// nodeVersionRoots 多版本根目录（nvm/fnm：bin 在 <root>/<version>/bin）。
// GO_CODE_NODE_VERSION_OVERRIDE（':' 分隔）整体覆盖版本根候选——测试隔离用；
// 正常不设。
export function nodeVersionRoots(home: string): string[] {
  const ov = process.env.GO_CODE_NODE_VERSION_OVERRIDE
  if (ov) return ov.split(path.delimiter).filter(Boolean)
  return [
    '/usr/local/nvm/versions/node', // nvm 系统级安装根
    home + '/.nvm/versions/node', // nvm 用户级安装根
    home + '/.local/share/fnm/node-versions', // fnm 安装根
    home + '/.fnm/node-versions', // fnm legacy 安装根
  ]
}

// pathForNode 探测一个可用 node：先直接 bin 目录候选，再多版本根取最新。
// 返回 node 可执行绝对路径 + 其所在 bin 目录；找不到 → null。
export function pathForNode(opts?: { home?: string; platform?: NodeJS.Platform }): { binDir: string; nodePath: string } | null {
  const home = opts?.home ?? os.homedir()
  const platform = opts?.platform ?? process.platform
  const exeName = platform === 'win32' ? 'node.exe' : 'node'
  // 1) 直接 bin 候选
  for (const dir of nodeDirCandidates(home, platform)) {
    if (isExecFile(path.join(dir, exeName))) return { binDir: dir, nodePath: path.join(dir, exeName) }
  }
  // 2) 版本根候选（取最新）
  for (const root of nodeVersionRoots(home)) {
    // 只保留含可执行 bin/node 的版本目录；readdirSync 失败 → 无候选
    let dirs: string[]
    try {
      dirs = fs
        .readdirSync(root)
        .filter((d) => {
          try {
            return fs.statSync(path.join(root, d)).isDirectory()
          } catch {
            return false
          }
        })
        .filter((d) => isExecFile(path.join(root, d, 'bin', exeName)))
    } catch {
      continue
    }
    if (dirs.length === 0) continue
    dirs.sort(cmpVersion)
    const p = path.join(root, dirs[dirs.length - 1], 'bin', exeName)
    if (isExecFile(p)) return { binDir: path.dirname(p), nodePath: p }
  }
  return null
}

// hasNodeOnPath PATH 各目录里是否已有 node 可执行（有 → 无需注入，用户 PATH 优先）。
export function hasNodeOnPath(p: string | undefined, platform: NodeJS.Platform): boolean {
  if (!p) return false
  const exeName = platform === 'win32' ? 'node.exe' : 'node'
  return p.split(path.delimiter).some((d) => {
    if (!d) return false
    return isExecFile(path.join(d, exeName))
  })
}

// augmentPathWithNode PATH 上无 node 时，把探测到的 node 所在 bin 目录前插。
// 返回新 PATH；无需注入（PATH 已有 node 或探测不到）→ 原 PATH。
export function augmentPathWithNode(currentPath: string | undefined, opts?: { home?: string; platform?: NodeJS.Platform }): string | undefined {
  const platform = opts?.platform ?? process.platform
  if (hasNodeOnPath(currentPath, platform)) return currentPath
  const found = pathForNode(opts)
  if (!found) return currentPath
  return found.binDir + path.delimiter + (currentPath || '')
}
