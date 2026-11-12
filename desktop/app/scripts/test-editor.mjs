#!/usr/bin/env node
// PR-05b 不变量：内嵌编辑器的**纯逻辑**（不启 Electron、不开浏览器）。
//
// 测的是**真实现**：esbuild 把 src/lib/editorState.ts 与 src/lib/editorDocs.ts 打成
// cjs，脚本 require 它们，直接喂事件、数命令调用。故意不去复述实现的写法（比如不自己
// 算 dirty），否则改坏了实现断言也跟着改坏 —— 那就成了自证。
//
// 覆盖（PR-05b 验收表里「不点界面也能验」的那几条）：
//   · dirty 判定：内容相同 = 干净；差一个字符 / 只差尾随换行 = 脏
//   · 冲突决策：干净 + 外部变更 → 重载；脏 + 外部变更 → 黄条不重载；删除 + 脏 → 禁止保存
//   · 保存载荷：首次 baseHash = 打开时的 hash；覆盖保存 baseHash=""; 成功后 baseHash 换新
//   · file_save 返回 denied → 状态 invalid，且**不重发**（数命令条数）
//
// 用法：cd desktop/app && npm run test:editor
import { build } from 'esbuild'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'

const require = createRequire(import.meta.url)
let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const eq = (actual, expected, m) => {
  if (actual !== expected) throw new Error(`断言失败: ${m}\n     期望: ${JSON.stringify(expected)}\n     实际: ${JSON.stringify(actual)}`)
  ok(`${m}（= ${JSON.stringify(actual)}）`)
}

const outdir = mkdtempSync(join(tmpdir(), 'editor-test-'))
await build({
  entryPoints: ['src/lib/editorState.ts', 'src/lib/editorDocs.ts'],
  bundle: true,
  platform: 'node',
  format: 'cjs',
  outdir,
  logLevel: 'error',
})
const S = require(join(outdir, 'editorState.js'))
const { createEditorDocs } = require(join(outdir, 'editorDocs.js'))

// —— 假 bridge：按脚本给应答，并记账 ——
function fakeBridge(script = {}) {
  const calls = []
  const send = async (type, payload) => {
    calls.push({ type, payload })
    const h = script[type] ?? (() => ({ ok: true, data: {} }))
    return h(payload, calls.filter((c) => c.type === type).length)
  }
  return { calls, send, of: (t) => calls.filter((c) => c.type === t) }
}
// 打开一份文件的默认应答
const openReply = (content, hash) => () => ({ ok: true, data: { ok: true, path: '/ws/a.txt', content, hash } })
const savedReply = (hash) => () => ({ ok: true, data: { ok: true, path: '/ws/a.txt', hash, size: 1, mtime: 0 } })

async function newSession(script) {
  const b = fakeBridge(script)
  const dirty = new Map()
  const docs = createEditorDocs({ send: b.send, onDirty: (id, d) => dirty.set(id, d) })
  return { b, docs, dirty }
}

// ───────────────────────── 1. dirty 判定 ─────────────────────────
console.log('\n[1] dirty 判定（派生值，不另存一份真相）')
{
  eq(S.isDirty('a\nb', 'a\nb'), false, '内容完全相同 → 干净')
  eq(S.isDirty('a\nb', 'a\nB'), true, '差一个字符 → 脏')
  // 只差尾随换行也算脏：保存会把那个换行真的写进磁盘（编辑器不能替用户猜）
  eq(S.isDirty('a\nb\n', 'a\nb'), true, '只差尾随换行 → 脏')
  eq(S.isDirty('a\nb', 'a\nb\n'), true, '反向：少一个尾随换行 → 脏')

  const { docs, dirty } = await newSession({ file_open: openReply('hello\n', 'h1') })
  await docs.open('t1', '/ws/a.txt')
  eq(docs.get('t1').dirty, false, '刚打开 → 不脏')
  docs.change('t1', 'hello\n')
  eq(docs.get('t1').dirty, false, '改成同样的内容 → 仍不脏（按内容比，不按按键次数）')
  docs.change('t1', 'hello\n\n')
  eq(docs.get('t1').dirty, true, '加了一个换行 → 脏')
  eq(dirty.get('t1'), true, '脏状态已上报给宿主（标签圆点/原生红点）')
  docs.change('t1', 'hello\n')
  eq(docs.get('t1').dirty, false, '改回去 → 回到干净（不是"按过键就洗不掉"）')
  eq(dirty.get('t1'), false, '干净状态同步回宿主')
}

// ───────────────────────── 2. 冲突决策 ─────────────────────────
console.log('\n[2] 冲突决策：外部变更的三种处境')
{
  const clean = { ...S.emptyDoc('/ws/a.txt'), content: 'x', baseContent: 'x', baseHash: 'h', status: 'ready', dirty: false }
  const dirt = { ...clean, content: 'x2', dirty: true }
  const effClean = S.externalEffect(clean, {})
  assert('reload' in effClean, '干净 + 外部变更 → 重新加载（静默，不打扰）')
  const effDirty = S.externalEffect(dirt, {})
  assert('banner' in effDirty && effDirty.banner.kind === 'external', '脏 + 外部变更 → 只挂黄条')
  const effDel = S.externalEffect(dirt, { deleted: true })
  assert('banner' in effDel && effDel.banner.kind === 'deleted', '删除 + 脏 → 黄条（删除专用文案）')
  eq(S.saveBlock({ ...dirt, deleted: true }), 'deleted', '删除 + 脏 → 禁止保存（保存=新建，必须显式确认）')
  eq(S.saveBlock({ ...dirt, deleted: true }, true), '', '黄条上显式「覆盖保存」才放行')
  eq(S.saveBlock({ ...dirt, readonly: 'too_large' }), 'readonly', '截断只读 → 禁止保存')
  eq(S.saveBlock({ ...dirt, status: 'invalid' }), 'invalid', 'denied 之后 → 禁止保存')

  // 走一遍会话：干净时外部改 → 真的重发 file_open；脏时 → 一条命令都不发
  const s1 = await newSession({ file_open: openReply('one', 'h1') })
  await s1.docs.open('t1', '/ws/a.txt')
  s1.docs.external('/ws/a.txt', { hash: 'h2' })
  await new Promise((r) => setTimeout(r, 0))
  eq(s1.b.of('file_open').length, 2, '干净 + 外部变更 → 自动发第二次 file_open（静默重载）')

  const s2 = await newSession({ file_open: openReply('one', 'h1') })
  await s2.docs.open('t1', '/ws/a.txt')
  s2.docs.change('t1', 'mine')
  s2.docs.external('/ws/a.txt', { hash: 'h2' })
  await new Promise((r) => setTimeout(r, 0))
  eq(s2.b.of('file_open').length, 1, '脏 + 外部变更 → **不**自动重载（不发 file_open）')
  eq(s2.docs.get('t1').content, 'mine', '本地改动原样保留')
  eq(s2.docs.get('t1').banner.kind, 'external', '黄条已挂上')
}

// ───────────────────────── 3. 保存载荷 ─────────────────────────
console.log('\n[3] 保存载荷：baseHash 的来源与更新')
{
  const s = await newSession({
    file_open: openReply('body', 'h-open'),
    file_save: (() => { let n = 0; return () => ({ ok: true, data: { ok: true, path: '/ws/a.txt', hash: `h-save${++n}` } }) })(),
  })
  await s.docs.open('t1', '/ws/a.txt')
  eq(s.docs.get('t1').baseHash, 'h-open', '打开后 baseHash = file_open 的 hash')

  s.docs.change('t1', 'body!')
  const r1 = await s.docs.save('t1')
  eq(r1.ok, true, '第一次保存成功')
  const p1 = s.b.of('file_save')[0].payload
  eq(p1.baseHash, 'h-open', '首次保存带打开时的 hash（不是空串 → 服务端能判冲突）')
  eq(p1.content, 'body!', '保存的是 textarea 当前内容')
  eq(p1.allowNew, false, '保存永远不带 allowNew（新建必须显式）')
  eq(s.docs.get('t1').baseHash, 'h-save1', '成功后 baseHash 更新为新 hash')
  eq(s.docs.get('t1').dirty, false, '成功后 dirty 清除（baseContent 也换了）')

  s.docs.change('t1', 'body!!')
  await s.docs.save('t1')
  eq(s.b.of('file_save')[1].payload.baseHash, 'h-save1', '第二次保存带**上一次成功**的 hash')

  // 覆盖保存：冲突条上的显式动作 → baseHash 清空
  const s2 = await newSession({
    file_open: openReply('body', 'h-open'),
    file_save: () => ({ ok: false, code: 'conflict', data: { path: '/ws/a.txt', currentHash: 'h-disk' } }),
  })
  await s2.docs.open('t1', '/ws/a.txt')
  s2.docs.change('t1', 'mine')
  const rc = await s2.docs.save('t1')
  eq(rc.code, 'conflict', 'baseHash 不匹配 → conflict')
  eq(s2.docs.get('t1').banner.kind, 'conflict', '冲突 → 黄条')
  eq(s2.docs.get('t1').banner.currentHash, 'h-disk', '条上带磁盘当前 hash（不用再发一次 file_open 才知道）')
  eq(s2.docs.get('t1').content, 'mine', '冲突**不覆盖**本地内容')
  // 「覆盖保存」用 baseHash:"" 重发
  await s2.docs.save('t1', { overwrite: true })
  eq(s2.b.of('file_save')[1].payload.baseHash, '', '覆盖保存 → baseHash 空串（服务端=无条件写）')
}

// ───────────────────────── 4. denied 之后不重发 ─────────────────────────
console.log('\n[4] denied：状态变 invalid，且不再发第二次')
{
  const s = await newSession({
    file_open: openReply('body', 'h-open'),
    file_save: () => ({ ok: false, code: 'denied', error: '文件已失效' }),
  })
  await s.docs.open('t1', '/ws/a.txt')
  s.docs.change('t1', 'edited')
  const r = await s.docs.save('t1')
  eq(r.ok, false, '保存失败')
  eq(s.docs.get('t1').status, 'invalid', 'denied → status=invalid')
  eq(s.docs.get('t1').banner.kind, 'invalid', 'denied → 红条（重新打开）')
  const r2 = await s.docs.save('t1')
  eq(r2.blocked, 'invalid', '再按 ⌘S → 直接拦下（blocked=invalid）')
  eq(s.b.of('file_save').length, 1, '**只发过一次** file_save（denied 后不再重试）')

  // 重新打开 = 重新拿授权：成功后又能保存
  const s2 = await newSession({
    file_open: (() => { let n = 0; return () => ({ ok: true, data: { ok: true, path: '/ws/a.txt', content: 'body', hash: `h${++n}` } }) })(),
    file_save: () => ({ ok: true, data: { ok: true, hash: 'h-new' } }),
  })
  await s2.docs.open('t1', '/ws/a.txt')
  await s2.docs.reload('t1')
  eq(s2.docs.get('t1').status, 'ready', '重新加载后回到 ready')
  s2.docs.change('t1', 'again')
  eq((await s2.docs.save('t1')).ok, true, 'reload 拿回授权后可以再保存')
}

// ───────────────────────── 5. 只读 / 关闭 / 重载语义 ─────────────────────────
console.log('\n[5] 只读、关闭与 baseContent')
{
  const big = await newSession({ file_open: () => ({ ok: true, data: { ok: true, path: '/ws/big.txt', content: 'x', hash: 'h', truncated: true } }) })
  await big.docs.open('t1', '/ws/big.txt')
  eq(big.docs.get('t1').readonly, 'too_large', 'truncated=true → 只读（截断内容写回会毁文件）')
  eq(S.saveBlock(big.docs.get('t1')), 'readonly', '只读文件按 ⌘S 直接被拦（不发命令）')

  const bin = await newSession({ file_open: () => ({ ok: true, data: { ok: true, path: '/ws/x.bin', content: '', hash: 'h', binary: true } }) })
  await bin.docs.open('t1', '/ws/x.bin')
  eq(bin.docs.get('t1').readonly, 'binary', 'binary=true → 只读')

  const s = await newSession({ file_open: openReply('v1', 'h1') })
  await s.docs.open('t1', '/ws/a.txt')
  s.docs.change('t1', 'v2')
  eq(s.docs.get('t1').dirty, true, '改动后脏')
  eq(s.dirty.get('t1'), true, '脏状态已镜像给宿主')
  await s.docs.reload('t1')
  eq(s.docs.get('t1').content, 'v1', '重新加载 → 内容回到磁盘版本')
  eq(s.docs.get('t1').dirty, false, '重新加载 → 脏状态清除')
  // 这条单独立着：宿主镜像（标签圆点）也必须是 false。曾经只在 reducer 里清 dirty、
  // 忘了通知宿主 → 圆点留在标签上，而 store 说「没改动」。
  eq(s.dirty.get('t1'), false, '重新加载 → 宿主的镜像也清掉（否则圆点撒谎）')

  // 关闭：退订 + 丢状态
  const c = await newSession({ file_open: openReply('v1', 'h1') })
  await c.docs.open('t1', '/ws/a.txt')
  c.docs.close('t1')
  eq(c.b.of('file_close').length, 1, '关标签 → 发一次 file_close')
  eq(c.docs.get('t1'), undefined, '关标签 → 本地状态丢弃')
  eq(c.docs.anyDirty(), false, '没有文档 → anyDirty=false')
}

// ───────────────────────── 6. 入口开关 ─────────────────────────
console.log('\n[6] 回滚开关与入口判据')
{
  eq(S.isEditorEnabled({ editor: true }), true, 'settings.editor=true → 入口出现')
  eq(S.isEditorEnabled({}), true, '缺省（老 settings.json）→ 入口出现')
  eq(S.isEditorEnabled({ editor: false }), false, 'settings.editor=false → 入口不出现（回滚开关）')
  eq(S.isEditorEnabled(undefined), true, 'settings 还没加载 → 视为开启（撤开关不该炸）')
  eq(S.isTextLikePath('a.go'), true, '源码 → 可编辑')
  eq(S.isTextLikePath('shot.PNG'), false, '图片（大写扩展名）→ 不给编辑入口')
  eq(S.isTextLikePath('book.xlsx'), false, '工作簿 → 不给编辑入口')
}

console.log(`\n✅ test-editor: ${pass} 项全通过`)
