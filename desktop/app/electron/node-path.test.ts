// node-path.test.ts — node-path.ts 单测（node:test，纯 Node 无 Electron 依赖）。
// 覆盖：常见安装位置探测（brew/官网 pkg/nvm/fnm/volta）、多版本取最新、
// PATH 已有 node 时不注入、PATH 缺失时前插探测目录。
// 运行：esbuild bundle 后 node --test（见 package.json test:node-path）。
//
// 隔离说明：本机（CI/开发者 mac）可能真实装有 node（如 /opt/homebrew/bin/node），
// 固定路径候选会抢先命中导致断言不稳定。测试用 GO_CODE_NODE_PATH_OVERRIDE /
// GO_CODE_NODE_VERSION_OVERRIDE 把候选列表指到临时目录，完全掌控探测范围。
import { test, beforeEach, afterEach } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import { pathForNode, augmentPathWithNode, hasNodeOnPath, versionNums, nodeDirCandidates, nodeVersionRoots } from './node-path.ts'

const PLAT = process.platform === 'win32' ? 'win32' : 'darwin'
const EXE = PLAT === 'win32' ? 'node.exe' : 'node'
const savedPath = process.env.GO_CODE_NODE_PATH_OVERRIDE
const savedVers = process.env.GO_CODE_NODE_VERSION_OVERRIDE

beforeEach(() => {
  // 每个用例默认屏蔽本机真实固定路径候选（用例需要时自行 set）
  process.env.GO_CODE_NODE_PATH_OVERRIDE = '/nonexistent-override'
  process.env.GO_CODE_NODE_VERSION_OVERRIDE = '/nonexistent-override'
})
afterEach(() => {
  if (savedPath === undefined) delete process.env.GO_CODE_NODE_PATH_OVERRIDE
  else process.env.GO_CODE_NODE_PATH_OVERRIDE = savedPath
  if (savedVers === undefined) delete process.env.GO_CODE_NODE_VERSION_OVERRIDE
  else process.env.GO_CODE_NODE_VERSION_OVERRIDE = savedVers
})

// fakeHome 构造临时 home
function fakeHome(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'gocode-nodepath-'))
}

// mkNode 在 dir 下造一个可执行 node（真实文件）
function mkNode(dir: string): string {
  fs.mkdirSync(dir, { recursive: true })
  const p = path.join(dir, EXE)
  fs.writeFileSync(p, '#!/usr/bin/env sh\nexit 0\n', { mode: 0o755 })
  return p
}

test('nodeDirCandidates 覆盖 macOS 常见安装位置', () => {
  delete process.env.GO_CODE_NODE_PATH_OVERRIDE
  const dirs = nodeDirCandidates('/Users/x', 'darwin')
  for (const want of ['/opt/homebrew/bin', '/usr/local/bin', '/opt/local/bin', '/Users/x/.volta/bin', '/Users/x/.local/bin']) {
    assert.ok(dirs.includes(want), `缺 ${want}: ${dirs.join(',')}`)
  }
})

test('nodeVersionRoots 覆盖 nvm/fnm 版本根', () => {
  delete process.env.GO_CODE_NODE_VERSION_OVERRIDE
  const roots = nodeVersionRoots('/Users/x')
  for (const want of ['/usr/local/nvm/versions/node', '/Users/x/.nvm/versions/node', '/Users/x/.local/share/fnm/node-versions', '/Users/x/.fnm/node-versions']) {
    assert.ok(roots.includes(want), `缺 ${want}: ${roots.join(',')}`)
  }
})

test('versionNums 解析（含 v 前缀；不可解析为空）', () => {
  assert.deepEqual(versionNums('v20.11.1'), [20, 11, 1])
  assert.deepEqual(versionNums('18'), [18])
  assert.deepEqual(versionNums('node'), [])
  assert.deepEqual(versionNums(''), [])
})

test('pathForNode：nvm 多版本根取最新', () => {
  if (PLAT !== 'darwin') return // nvm 布局为 POSIX 路径
  const home = fakeHome()
  const root = path.join(home, '.nvm', 'versions', 'node')
  mkNode(path.join(root, 'v18.20.4', 'bin'))
  const v20 = mkNode(path.join(root, 'v20.11.1', 'bin'))
  mkNode(path.join(root, 'v8.17.0', 'bin'))
  process.env.GO_CODE_NODE_VERSION_OVERRIDE = root // 只测这个版本根
  const got = pathForNode({ home, platform: 'darwin' })
  assert.ok(got, '应探测到 nvm node')
  assert.equal(got.nodePath, v20, '应取版本最大 v20.11.1')
  assert.equal(got.binDir, path.dirname(v20))
})

test('pathForNode：直接 bin 候选命中（volta 布局）', () => {
  const home = fakeHome()
  const v = mkNode(path.join(home, '.volta', 'bin'))
  process.env.GO_CODE_NODE_PATH_OVERRIDE = path.join(home, '.volta', 'bin')
  const got = pathForNode({ home, platform: PLAT })
  assert.ok(got, '应探测到 volta node')
  assert.equal(got.nodePath, v)
})

test('pathForNode：找不到返回 null', () => {
  const got = pathForNode({ home: fakeHome(), platform: PLAT })
  assert.equal(got, null)
})

test('hasNodeOnPath：PATH 目录含 node 判定', () => {
  const home = fakeHome()
  const dir = path.join(home, 'bin')
  mkNode(dir)
  assert.ok(hasNodeOnPath(dir, PLAT))
  assert.ok(!hasNodeOnPath(path.join(home, 'nope'), PLAT))
  assert.ok(!hasNodeOnPath(undefined, PLAT))
})

test('augmentPathWithNode：PATH 已有 node 不注入（用户 PATH 优先）', () => {
  const home = fakeHome()
  const dir = path.join(home, 'existing')
  mkNode(dir)
  const out = augmentPathWithNode(dir, { home: fakeHome(), platform: PLAT })
  assert.equal(out, dir, '已有 node 不应改动 PATH')
})

test('augmentPathWithNode：PATH 无 node → 前插探测到的 bin 目录', () => {
  const home = fakeHome()
  const v = mkNode(path.join(home, '.volta', 'bin'))
  process.env.GO_CODE_NODE_PATH_OVERRIDE = path.dirname(v)
  const cur = path.join(home, 'no-node-dir')
  const out = augmentPathWithNode(cur, { home, platform: PLAT })
  assert.ok(out, '应注入')
  assert.ok(out!.startsWith(path.dirname(v) + path.delimiter), `应前插 ${path.dirname(v)}: ${out}`)
})

test('augmentPathWithNode：PATH 无 node 且探测不到 → 原 PATH', () => {
  const cur = '/usr/bin:/bin'
  const out = augmentPathWithNode(cur, { home: fakeHome(), platform: PLAT })
  assert.equal(out, cur)
})
