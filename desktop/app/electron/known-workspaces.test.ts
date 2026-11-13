// known-workspaces.test.ts — 「用户真的打开过的工作区」清单（媒体协议的信任锚）单测。
//
// 为什么值得单测：这是 R08c 评审裁决 1 的修复本体。评审实测过：修复前渲染进程传
// `workspace=/Users`（或 ~/Documents）就能把它登记成媒体根，协议随即能把家目录下任何
// 媒体文件流式读出来。这条清单是唯一说「谁能当根」的地方，所以它的每条拒绝都要钉住：
//   · 不是绝对路径 / 不存在 / 不是目录 → 不登记；
//   · 没登记过的目录 → isKnownWorkspace 为 false（fail-closed）；
//   · 落盘后**重启仍然记得**（否则每次重启都要重新选目录，用户会以为媒体坏了）；
//   · forget 之后立刻失效（工作区移除 = 根立刻回收，不再「驻留到进程退出」）。
//
// 运行：esbuild bundle 后 node --test（见 package.json test:known-workspaces）。
import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import {
  configureKnownWorkspaces,
  forgetWorkspace,
  isKnownWorkspace,
  knownWorkspaces,
  rememberWorkspace,
} from './known-workspaces.ts'

const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'known-ws-'))
const other = fs.mkdtempSync(path.join(os.tmpdir(), 'known-ws-other-'))
const file = path.join(dir, 'workspaces.json')
let wsA = ''
let wsB = ''

before(() => {
  wsA = fs.mkdtempSync(path.join(dir, 'project-a-'))
  wsB = fs.mkdtempSync(path.join(dir, 'project-b-'))
  configureKnownWorkspaces(file)
})

after(() => {
  fs.rmSync(dir, { recursive: true, force: true })
  fs.rmSync(other, { recursive: true, force: true })
})

test('remember 只接受「绝对路径 + 存在 + 是目录」', () => {
  assert.equal(rememberWorkspace('relative/path'), undefined, '相对路径不能登记')
  assert.equal(rememberWorkspace(''), undefined)
  assert.equal(rememberWorkspace(123 as unknown as string), undefined)
  assert.equal(rememberWorkspace(path.join(dir, 'nope')), undefined, '不存在的目录不能登记')
  const f = path.join(dir, 'a.txt')
  fs.writeFileSync(f, 'x')
  assert.equal(rememberWorkspace(f), undefined, '文件不能当工作区')
})

test('登记过的目录 → isKnownWorkspace 为 true（realpath 口径，软链也认）', () => {
  const real = rememberWorkspace(wsA)
  assert.equal(real, fs.realpathSync(wsA))
  assert.equal(isKnownWorkspace(wsA), true)
  // 软链到同一个目录：realpath 后仍是同一个根
  const link = path.join(dir, 'link-a')
  fs.symlinkSync(wsA, link)
  assert.equal(isKnownWorkspace(link), true)
  assert.equal(isKnownWorkspace(wsB), false, '没登记过的目录必须 false')
  assert.equal(isKnownWorkspace('/Users'), false, '评审的原始攻击路径：不能被当成已知工作区')
  assert.equal(isKnownWorkspace(''), false)
})

test('落盘：换一个进程（重新 configure）仍然记得 —— 否则每次重启都要重新选目录', () => {
  rememberWorkspace(wsA)
  configureKnownWorkspaces(file) // 模拟重启：内存清空，重新从磁盘读
  assert.equal(isKnownWorkspace(wsA), true)
  assert.deepEqual(knownWorkspaces(), [fs.realpathSync(wsA)])
})

test('forget：工作区移除后根立刻失效（不再驻留到进程退出）', () => {
  rememberWorkspace(wsA)
  rememberWorkspace(wsB)
  assert.equal(isKnownWorkspace(wsB), true)
  forgetWorkspace(wsB)
  assert.equal(isKnownWorkspace(wsB), false)
  configureKnownWorkspaces(file) // 磁盘上也删掉了
  assert.equal(isKnownWorkspace(wsB), false)
  assert.equal(isKnownWorkspace(wsA), true, '删一个不影响另一个')
})

test('目录已经不存在时 forget 仍然能清掉记录（用户删了文件夹再移除工作区）', () => {
  const gone = fs.mkdtempSync(path.join(dir, 'gone-'))
  rememberWorkspace(gone)
  assert.equal(isKnownWorkspace(gone), true)
  fs.rmSync(gone, { recursive: true, force: true })
  forgetWorkspace(gone)
  configureKnownWorkspaces(file)
  assert.equal(knownWorkspaces().includes(fs.realpathSync(dir)), false)
  assert.equal(isKnownWorkspace(gone), false)
})

test('没有 512 条的 FIFO：登记第 520 个目录后，第 1 个仍然记得', () => {
  // 复评裁决 1：这里曾经是 slice(-512)，与 media-protocol 的 MAX_ROOTS 是同一个 bug 的
  // 第二次实现。这两个地方都只靠一条用例守着，别再让它们靠「读代码」保证。
  const base = fs.mkdtempSync(path.join(dir, 'many-'))
  const before = knownWorkspaces().length
  const first = path.join(base, 'w-000')
  fs.mkdirSync(first)
  rememberWorkspace(first)
  for (let i = 1; i < 520; i++) {
    const d = path.join(base, `w-${String(i).padStart(3, '0')}`)
    fs.mkdirSync(d)
    rememberWorkspace(d)
  }
  assert.equal(knownWorkspaces().length, before + 520, '不该被截断')
  assert.equal(isKnownWorkspace(first), true, '最早那个目录必须还在（否则第 521 个一登记它就没了）')
  configureKnownWorkspaces(file) // 落盘也要完整
  assert.equal(isKnownWorkspace(first), true)
  fs.rmSync(base, { recursive: true, force: true })
})

test('落盘文件损坏 → 空清单（fail-closed，宁可多选一次目录）', () => {
  fs.writeFileSync(file, '{ this is not json')
  configureKnownWorkspaces(file)
  assert.deepEqual(knownWorkspaces(), [])
  assert.equal(isKnownWorkspace(wsA), false)
})
