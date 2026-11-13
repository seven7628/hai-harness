// fs-ops.test.ts — 文件树写操作（fs-ops.ts）单测：node:test，纯 Node 无 Electron 依赖。
//
// 为什么值得单测：这些操作**改用户的真实文件**，而拒绝分支在 UI 里极难逐条复现（要造
// 工作区外的路径、~/.ssh 下的目录、把目录拖进自己的子孙……）。每条拒绝理由都在这里钉死，
// 顺带把三个很容易被后来者「顺手放宽」的正确行为也钉住：
//   · 同目录拖动是**无操作成功**，不是报错（用户拖回原处不该看到红字）；
//   · 只改大小写的重命名必须允许（macOS 大小写不敏感卷上 existsSync 会命中自己）；
//   · 复制副本的存在性冲突是**自动递增**，不弹拒绝（副本名是我们生成的）。
//
// 运行：esbuild bundle 后 node --test（见 package.json test:fs-ops）。
import { test, after, before } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import {
  copyEntry,
  createDir,
  importEntry,
  createFile,
  duplicateEntry,
  guardTrash,
  moveEntry,
  renameEntry,
  validateName,
} from './fs-ops.ts'

// 工作区用临时目录；「工作区外」用**另一个**临时目录 —— 只有真的另一个目录才证明包含判断生效。
const root = fs.mkdtempSync(path.join(os.tmpdir(), 'fs-ops-ws-'))
const outside = fs.mkdtempSync(path.join(os.tmpdir(), 'fs-ops-out-'))
// 配置目录：特意放在工作区**里面**，验证「即使在工作区内，配置目录也不许动」。
const cfg = path.join(root, '.go-code')
// 敏感目录用假 HOME 模拟（isSensitiveFsPath 每次调用都读 os.homedir()）。
const fakeHome = fs.mkdtempSync(path.join(os.tmpdir(), 'fs-ops-home-'))
const sshDir = path.join(fakeHome, '.ssh')
const oldHome = process.env.HOME

before(() => {
  process.env.HOME = fakeHome
  fs.mkdirSync(cfg, { recursive: true })
  fs.writeFileSync(path.join(cfg, 'settings.json'), '{}')
  fs.mkdirSync(sshDir, { recursive: true })
  fs.writeFileSync(path.join(sshDir, 'id_rsa'), 'key')
  fs.mkdirSync(path.join(root, 'src'), { recursive: true })
  fs.mkdirSync(path.join(root, 'docs'), { recursive: true })
  fs.mkdirSync(path.join(root, 'src', 'deep'), { recursive: true })
  fs.writeFileSync(path.join(root, 'a.txt'), 'a')
  fs.writeFileSync(path.join(root, 'src', 'main.go'), 'package main')
})

after(() => {
  process.env.HOME = oldHome
  for (const p of [root, outside, fakeHome]) fs.rmSync(p, { recursive: true, force: true })
})

const ws = () => root
const exists = (p: string) => fs.existsSync(path.join(root, p))

// —— 名字校验 ——

test('validateName 拒绝空名/点/斜杠/超长，放行点文件', async () => {
  assert.equal(validateName(''), '名称不能为空')
  assert.equal(validateName('   '), '名称不能为空') // 全空白 = 空
  assert.equal(validateName('.'), '名称无效')
  assert.equal(validateName('..'), '名称无效')
  assert.equal(validateName('a/b'), '名称不能包含斜杠')
  assert.equal(validateName('a\u0000b'), '名称不能包含斜杠')
  assert.equal(validateName('x'.repeat(256)), '名称过长')
  assert.equal(validateName('.env'), null) // 用户要建点文件就让他建
  assert.equal(validateName('笔记 2026.md'), null)
})

// —— 重命名 ——

test('renameEntry 成功并可读回', async () => {
  const r = await renameEntry(ws(), path.join(root, 'a.txt'), 'b.txt', cfg)
  assert.equal(r.ok, true)
  assert.equal(r.path, path.join(root, 'b.txt'))
  assert.equal(exists('b.txt'), true)
  assert.equal(exists('a.txt'), false)
  await renameEntry(ws(), path.join(root, 'b.txt'), 'a.txt', cfg) // 复位给后面的用例
})

test('renameEntry 同名冲突直接拒绝（EXISTS），不动磁盘', async () => {
  const r = await renameEntry(ws(), path.join(root, 'a.txt'), 'src', cfg)
  assert.equal(r.ok, false)
  assert.equal(r.code, 'EXISTS')
  assert.equal(exists('a.txt'), true)
})

test('renameEntry 改成自己 = 无操作成功（不是冲突）', async () => {
  const r = await renameEntry(ws(), path.join(root, 'a.txt'), 'a.txt', cfg)
  assert.equal(r.ok, true)
  assert.equal(exists('a.txt'), true)
})

test('renameEntry 拒工作区根 / 工作区外 / 配置目录 / 敏感目录', async () => {
  assert.equal((await renameEntry(ws(), root, 'x', cfg)).code, 'IS_ROOT')
  assert.equal((await renameEntry(ws(), path.join(outside, 'x'), 'y', cfg)).code, 'DENIED')
  assert.equal((await renameEntry(ws(), path.join(cfg, 'settings.json'), 'z.json', cfg)).code, 'DENIED')
  const ssh = await renameEntry(ws(), path.join(sshDir, 'id_rsa'), 'k', cfg)
  assert.equal(ssh.ok, false)
  assert.equal(ssh.code, 'DENIED')
  assert.equal(fs.existsSync(path.join(sshDir, 'id_rsa')), true) // 真的没动
})

test('renameEntry 名字非法 → INVALID_NAME（先于路径守卫）', async () => {
  assert.equal((await renameEntry(ws(), path.join(root, 'a.txt'), 'a/b', cfg)).code, 'INVALID_NAME')
})

// —— 移动 ——

test('moveEntry 把文件搬进目标目录', async () => {
  const r = await moveEntry(ws(), path.join(root, 'a.txt'), path.join(root, 'docs'), cfg)
  assert.equal(r.ok, true)
  assert.equal(r.path, path.join(root, 'docs', 'a.txt'))
  assert.equal(exists('docs/a.txt'), true)
  assert.equal(exists('a.txt'), false)
  await moveEntry(ws(), path.join(root, 'docs', 'a.txt'), root, cfg) // 搬回根（目标 = 工作区根，允许）
})

test('moveEntry 同目录 = 无操作成功', async () => {
  const r = await moveEntry(ws(), path.join(root, 'a.txt'), root, cfg)
  assert.equal(r.ok, true)
  assert.equal(r.path, path.join(root, 'a.txt'))
})

test('moveEntry 目录不能搬进自己或自己的子孙（CYCLE）', async () => {
  assert.equal((await moveEntry(ws(), path.join(root, 'src'), path.join(root, 'src'), cfg)).code, 'CYCLE')
  assert.equal((await moveEntry(ws(), path.join(root, 'src'), path.join(root, 'src', 'deep'), cfg)).code, 'CYCLE')
  assert.equal(exists('src/main.go'), true) // 两步都没动
})

test('moveEntry 目标已存在同名项 → EXISTS', async () => {
  fs.writeFileSync(path.join(root, 'docs', 'a.txt'), 'x')
  const r = await moveEntry(ws(), path.join(root, 'a.txt'), path.join(root, 'docs'), cfg)
  assert.equal(r.ok, false)
  assert.equal(r.code, 'EXISTS')
  assert.equal(exists('a.txt'), true)
  fs.rmSync(path.join(root, 'docs', 'a.txt'))
})

test('moveEntry 目标是文件/不存在 → 拒绝', async () => {
  assert.equal((await moveEntry(ws(), path.join(root, 'a.txt'), path.join(root, 'a.txt'), cfg)).code, 'IO')
  assert.equal(
    (await moveEntry(ws(), path.join(root, 'a.txt'), path.join(root, 'nope'), cfg)).code,
    'NOT_FOUND',
  )
})

test('moveEntry 源/目标越界 → DENIED', async () => {
  assert.equal((await moveEntry(ws(), path.join(outside, 'x'), root, cfg)).code, 'DENIED')
  assert.equal((await moveEntry(ws(), path.join(root, 'a.txt'), outside, cfg)).code, 'DENIED')
})

// —— 新建 ——

test('createDir 新建目录；重名 → EXISTS', async () => {
  const r = await createDir(ws(), root, 'newdir', cfg)
  assert.equal(r.ok, true)
  assert.equal(exists('newdir'), true)
  assert.equal((await createDir(ws(), root, 'newdir', cfg)).code, 'EXISTS')
})

test('createFile 新建空文件；重名 → EXISTS（不覆盖已有内容）', async () => {
  const r = await createFile(ws(), path.join(root, 'src'), 'util.go', cfg)
  assert.equal(r.ok, true)
  assert.equal(fs.readFileSync(path.join(root, 'src', 'util.go'), 'utf8'), '')
  fs.writeFileSync(path.join(root, 'src', 'util.go'), 'keep me')
  const again = await createFile(ws(), path.join(root, 'src'), 'util.go', cfg)
  assert.equal(again.code, 'EXISTS')
  assert.equal(fs.readFileSync(path.join(root, 'src', 'util.go'), 'utf8'), 'keep me')
})

// —— 拷贝到别的目录（⌘C/⌘V）——

test('copyEntry 拷进目标目录并保留原名；源仍在', async () => {
  const r = await copyEntry(ws(), path.join(root, 'a.txt'), path.join(root, 'src'), cfg)
  assert.equal(r.ok, true)
  assert.equal(r.path, path.join(root, 'src', 'a.txt'))
  assert.equal(exists('a.txt'), true) // 拷贝 ≠ 移动
  assert.equal(exists('src/a.txt'), true)
})

test('copyEntry 同名 → EXISTS；拷进自己子孙 → CYCLE；越界 → DENIED', async () => {
  assert.equal((await copyEntry(ws(), path.join(root, 'a.txt'), path.join(root, 'src'), cfg)).code, 'EXISTS')
  assert.equal((await copyEntry(ws(), path.join(root, 'src'), path.join(root, 'src', 'deep'), cfg)).code, 'CYCLE')
  assert.equal((await copyEntry(ws(), path.join(root, 'a.txt'), outside, cfg)).code, 'DENIED')
})

// —— 从工作区外导入（访达拖入）——

test('importEntry 把工作区外的文件拷进来', async () => {
  fs.writeFileSync(path.join(outside, 'dropped.txt'), 'from finder')
  const r = await importEntry(ws(), path.join(root, 'src'), path.join(outside, 'dropped.txt'), cfg)
  assert.equal(r.ok, true)
  assert.equal(r.path, path.join(root, 'src', 'dropped.txt'))
  assert.equal(fs.readFileSync(path.join(root, 'src', 'dropped.txt'), 'utf8'), 'from finder')
  assert.equal(fs.existsSync(path.join(outside, 'dropped.txt')), true) // 导入 = 拷贝，源保留
})

test('importEntry 同名 → EXISTS；目标是文件 → IO；源不存在 → NOT_FOUND', async () => {
  assert.equal((await importEntry(ws(), path.join(root, 'src'), path.join(outside, 'dropped.txt'), cfg)).code, 'EXISTS')
  assert.equal((await importEntry(ws(), path.join(root, 'src', 'main.go'), path.join(outside, 'dropped.txt'), cfg)).code, 'IO')
  assert.equal((await importEntry(ws(), root, path.join(outside, 'ghost.txt'), cfg)).code, 'NOT_FOUND')
})

test('importEntry 拒敏感目录 / 配置目录 / 工作区外的目标', async () => {
  assert.equal((await importEntry(ws(), root, path.join(sshDir, 'id_rsa'), cfg)).code, 'DENIED')
  assert.equal((await importEntry(ws(), root, path.join(cfg, 'settings.json'), cfg)).code, 'DENIED')
  assert.equal((await importEntry(ws(), outside, path.join(outside, 'dropped.txt'), cfg)).code, 'DENIED')
  assert.equal(fs.existsSync(path.join(root, 'id_rsa')), false) // 真的没拷进来
})

// —— 复制副本 ——

test('duplicateEntry 文件名插在扩展名前，冲突自动递增', async () => {
  const r1 = await duplicateEntry(ws(), path.join(root, 'a.txt'), cfg)
  assert.equal(r1.path, path.join(root, 'a copy.txt'))
  const r2 = await duplicateEntry(ws(), path.join(root, 'a.txt'), cfg)
  assert.equal(r2.path, path.join(root, 'a copy 2.txt'))
  assert.equal(fs.readFileSync(path.join(root, 'a copy.txt'), 'utf8'), 'a')
})

test('duplicateEntry 目录整棵复制', async () => {
  const r = await duplicateEntry(ws(), path.join(root, 'src'), cfg)
  assert.equal(r.ok, true)
  assert.equal(r.path, path.join(root, 'src copy'))
  assert.equal(exists('src copy/deep'), true)
  assert.equal(fs.readFileSync(path.join(root, 'src copy', 'main.go'), 'utf8'), 'package main')
})

test('duplicateEntry 拒工作区根（复制整棵树没意义，且会瞬间撑爆磁盘）', async () => {
  assert.equal((await duplicateEntry(ws(), root, cfg)).code, 'IS_ROOT')
})

// —— 异步不阻塞：这次改造的唯一目的 ——

test('大目录拷贝期间事件循环不被占住（fs.promises.cp 流式）', async () => {
  // 为什么值得单测：改成 fs.promises.cp 的**全部意义**就是「拷大目录时主进程还能干活」。
  // 判据不是「多快」，而是「拷贝期间定时器能不能跑」：cpSync 会把整段拷贝塞进一个同步块，
  // 定时器一次都不会触发（这条用例会红）；流式 cp 会分块让出事件循环。
  // 48 MiB 是刻意的：小到不拖慢测试，大到同步拷贝至少几十毫秒。
  const big = path.join(root, 'big')
  fs.mkdirSync(big, { recursive: true })
  fs.writeFileSync(path.join(big, 'blob.bin'), Buffer.alloc(48 * 1024 * 1024, 7))
  let ticks = 0
  const iv = setInterval(() => { ticks++ }, 1)
  const r = await copyEntry(ws(), big, path.join(root, 'src'), cfg)
  clearInterval(iv)
  assert.equal(r.ok, true)
  assert.ok(ticks > 0, `拷贝期间定时器至少触发过一次（实得 ${ticks} 次）—— 为 0 说明拷贝又变回同步的了`)
  fs.rmSync(big, { recursive: true, force: true })
  fs.rmSync(path.join(root, 'src', 'big'), { recursive: true, force: true })
})

// —— 删除前守卫（trashItem 本身是 Electron 能力，这里只测守卫）——

test('guardTrash 放行工作区内普通项，拒根/越界/配置/敏感/不存在', async () => {
  assert.equal(guardTrash(ws(), path.join(root, 'a.txt'), cfg).ok, true)
  assert.equal(guardTrash(ws(), root, cfg).code, 'IS_ROOT')
  assert.equal(guardTrash(ws(), path.join(outside, 'x'), cfg).code, 'DENIED')
  assert.equal(guardTrash(ws(), path.join(cfg, 'settings.json'), cfg).code, 'DENIED')
  assert.equal(guardTrash(ws(), path.join(sshDir, 'id_rsa'), cfg).code, 'DENIED')
  assert.equal(guardTrash(ws(), path.join(root, 'ghost.txt'), cfg).code, 'NOT_FOUND')
})
