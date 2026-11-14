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
// B 段（评审 #1/#2/#4/#5）测**主干管道**：真 useAppStore + 真 transport 事件通道，不注入
// 假 send。它咬住三处「注入式用例永远看不到」的地方：
//   · dispatchEvent 对 file_* 响应的**就地 return**（删掉 → 冲突会把全局 bridge 标红）
//   · file_* 失败仍参与 bridge 掉线判定（进程死了不能只出一行文档红条）
//   · 批量关闭的确认队列（两个脏标签不能只问一个，更不许静默丢）
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
  entryPoints: ['src/lib/editorState.ts', 'src/lib/editorDocs.ts', 'src/store/useAppStore.ts'],
  bundle: true,
  platform: 'node',
  format: 'cjs',
  outdir,
  logLevel: 'error',
  // store 间接依赖 src/transport/mock.ts，它 import 了 README/brand 资源（`?raw`/`?no-inline`/
  // `?url`）。node 侧没有 vite 的资源插件链 → 补 loader（与 test-right-panel-tabs 同款）。
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})
// 多入口时 esbuild 按共同父目录（src/）铺开输出：lib/editorState.js、store/useAppStore.js…
const out = (...p) => join(outdir, ...p)
const S = require(out('lib', 'editorState.js'))
const { createEditorDocs } = require(out('lib', 'editorDocs.js'))

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

// ───────────────────────── 5b. 行尾（CRLF）─────────────────────────
console.log('\n[5b] CRLF：打开不显脏，保存按原行尾还原（评审 #3）')
{
  const crlf = 'package auth\r\n\r\nfunc Login() {}\r\n'
  // 纯函数三件套
  eq(S.toEditorText(crlf), 'package auth\n\nfunc Login() {}\n', '磁盘原文 → 编辑器空间：CRLF 归一成 LF')
  eq(S.eolOf(crlf), '\r\n', 'eol 判定：含 CRLF → \\r\\n')
  eq(S.eolOf('a\nb\n'), '\n', 'eol 判定：纯 LF → \\n')
  eq(S.toWireText('a\nb\n', '\r\n'), 'a\r\nb\r\n', '写盘形态：LF → CRLF')
  eq(S.toWireText('a\nb\n', '\n'), 'a\nb\n', 'LF 文件原样写回（不无中生有加 \\r）')
  eq(S.toEditorText('a\rb'), 'a\nb', '老式单 \\r 也归一（不然 dirty 恒真）')

  const s = await newSession({
    file_open: () => ({ ok: true, data: { ok: true, path: '/ws/crlf.go', content: crlf, hash: 'h1' } }),
    file_save: () => ({ ok: true, data: { ok: true, hash: 'h2' } }),
  })
  await s.docs.open('t1', '/ws/crlf.go')
  eq(s.docs.get('t1').eol, '\r\n', '打开时记住文件行尾')
  eq(s.docs.get('t1').dirty, false, '**CRLF 文件打开后不显脏**（textarea 归一成 LF 造成的假脏）')
  // 编辑器空间必须是 LF（textarea 的 value 只会是 LF）：这条同时挡两个方向 ——
  // ① 存 CRLF 原样 → 用户敲一个字就和 baseContent 全不等（假脏）；② 保存时再转一次 → \r\r\n
  eq(s.docs.get('t1').content, 'package auth\n\nfunc Login() {}\n', '打开后编辑器里存的是 LF 文本（textarea 的空间）')
  // 「打开 → 不改 → 保存」必须**逐字节等于磁盘原文**（否则一次空保存就是全文件 diff）
  await s.docs.save('t1')
  eq(s.b.of('file_save')[0].payload.content, crlf, '不改就保存：载荷与磁盘原文逐字节相同（没有行尾漂移）')
  eq(s.docs.get('t1').dirty, false, '空保存后仍然干净')
  s.docs.change('t1', 'package auth\n\nfunc Login() { return }\n')
  const r = await s.docs.save('t1')
  eq(r.ok, true, 'CRLF 文件保存成功')
  const p1 = s.b.of('file_save')[1].payload
  eq(p1.content.indexOf('\r\n') > 0, true, '保存载荷是 CRLF（不把整份文件改成 LF）')
  eq(p1.content.indexOf('\n\n'), -1, '载荷里没有裸 LF（除 CRLF 的第二字节）')
  eq(s.docs.get('t1').dirty, false, '保存后仍然干净（baseContent 存的是编辑器空间）')

  // hash 缺失的兜底：不能把 baseHash 写成空串（那等于下一次「无条件覆盖」）
  const h = await newSession({
    file_open: openReply('x', 'h-open'),
    file_save: () => ({ ok: true, data: { ok: true } }), // 老 bridge / 异常响应：没有 hash
  })
  await h.docs.open('t1', '/ws/a.txt')
  h.docs.change('t1', 'y')
  await h.docs.save('t1')
  eq(h.docs.get('t1').baseHash, 'h-open', '响应缺 hash → 保留旧 baseHash（不降级成无条件覆盖）')
  eq(h.docs.get('t1').dirty, false, '缺 hash 也算保存成功（内容已落盘）')
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

// ───────────────────────── B. 主干管道（真 store，不注入假 send）─────────────────────────
// 这一段是评审 #1 要求的「干净检出下也有覆盖」：起**真 useAppStore**，用 preload 的
// window.desktop 桩把命令输出录下来、把响应从**transport.onEvent**（真事件通道）喂回去，
// 走完 send → pending → wire() → dispatchEvent → onCommandResponseData → Promise 的全程。
// 注入式用例（A 段）永远看不到 dispatchEvent 里那两个分支，这里才咬得住。
console.log('\n[B] 主干管道：真 store（send/dispatchEvent/closeTabs/回滚开关）')

const emitted = []            // 发出去的命令（含 id，用来喂响应）
const listeners = new Set()   // transport.onEvent 的订阅者（wire + onCommandResponseData）
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}
global.window = {
  innerWidth: 1600, // openTab 的 60% 宽规则要读它
  addEventListener() {}, removeEventListener() {},
  desktop: new Proxy({
    command: async (cmd) => { emitted.push(cmd); return { ok: true } },
    onEvent: (cb) => { listeners.add(cb); return () => listeners.delete(cb) },
    onExit: () => () => {},
    startBridge: async () => ({ ok: true }),
    settingsGet: async () => ({ theme: 'dark', lang: 'zh', provider: { active: 'deepseek' }, mcpServers: {} }),
  }, {
    get(t, k) {
      if (k in t) return t[k]
      const name = String(k)
      if (name.startsWith('on')) return () => () => {}
      return async () => ({ ok: true })
    },
  }),
}
// **必须在 stub 之后** require：store 模块加载时按 window.desktop 选 electron transport
const store = require(out('store', 'useAppStore.js'))
const { useAppStore, dispatchEvent } = store
// 一行命令响应 → 真事件通道（wire() 的 listener 先跑，onCommandResponseData 后跑）
const emit = (line) => { for (const cb of [...listeners]) cb(JSON.stringify(line)) }
const lastOf = (type, from = 0) => [...emitted.slice(from)].reverse().find((c) => c.type === type)
const reset = () => {
  // tabsByScope/fileFocus 也要复位：标签集留着会让下一段的标签数多一个
  //（R11b 之前还要复位面板字段 —— 「按面板补默认标签」那条路已随字段一起删掉）。
  useAppStore.setState({
    bridgeStatus: 'running', bridgeError: undefined, toast: undefined,
    workspace: '/ws', activeSessionId: '', tabsByScope: {}, dirtyTabs: {}, closeConfirm: undefined,
    fileFocus: undefined, filePreviews: {},
  })
}

await useAppStore.getState().start('/ws') // 唯一入口：wire() 在这里订阅 transport.onEvent
assert(listeners.size > 0, 'wire() 已订阅事件通道（否则下面的 dispatchEvent 根本不会跑）')

// —— B1. file_save 冲突：Promise 拿到 code，且**不能**污染全局 bridgeError ——
{
  reset()
  const docs = createEditorDocs({ send: (t, p) => useAppStore.getState().editCommand(t, p) })
  const base = emitted.length
  const pOpen = docs.open('t1', '/ws/a.txt')
  const openCmd = lastOf('file_open', base)
  emit({ event_type: 'command_response', id: openCmd.id, ok: true, data: { ok: true, path: '/ws/a.txt', content: 'body', hash: 'h1' } })
  await pOpen
  eq(docs.get('t1').baseHash, 'h1', 'B1 打开：baseHash 来自 file_open 响应')

  docs.change('t1', 'mine')
  const pSave = docs.save('t1')
  const saveCmd = lastOf('file_save', base)
  emit({ event_type: 'command_response', id: saveCmd.id, ok: false, code: 'conflict', error: '文件已被外部修改，本次保存未写入（baseHash 不匹配）', data: { path: '/ws/a.txt', currentHash: 'h-disk' } })
  const r = await pSave
  eq(r.ok, false, 'B1 冲突：Promise 以失败 settle')
  eq(r.code, 'conflict', 'B1 冲突：**code 穿过整条管道**到调用方（不是靠文案匹配）')
  eq(docs.get('t1').banner.kind, 'conflict', 'B1 文档状态：黄条（文档自己的局部状态）')
  // ★ 这两条是 dispatchEvent 里那个「就地 return」的锚点：把 return 删掉，file_save 的
  //   ok:false 会掉进通用失败分支 → bridgeError 被写成冲突文案，这里立刻红。
  eq(useAppStore.getState().bridgeError, undefined, 'B1 冲突**不写**全局 bridgeError（删掉 return 这条必红）')
  eq(useAppStore.getState().bridgeStatus, 'running', 'B1 冲突**不改** bridgeStatus')
}

// —— B2. 成功路径：ok:true 带 hash → 文档干净 + baseHash 换新 ——
{
  reset()
  const docs = createEditorDocs({ send: (t, p) => useAppStore.getState().editCommand(t, p) })
  const base = emitted.length
  const pOpen = docs.open('t2', '/ws/b.txt')
  emit({ event_type: 'command_response', id: lastOf('file_open', base).id, ok: true, data: { ok: true, path: '/ws/b.txt', content: 'v1', hash: 'h1' } })
  await pOpen
  docs.change('t2', 'v2')
  const pSave = docs.save('t2')
  const saveCmd = lastOf('file_save', base)
  eq(saveCmd.payload.baseHash, 'h1', 'B2 保存载荷带打开时的 hash')
  emit({ event_type: 'command_response', id: saveCmd.id, ok: true, data: { ok: true, path: '/ws/b.txt', hash: 'h2', size: 2, mtime: 1 } })
  eq((await pSave).ok, true, 'B2 成功：Promise resolve')
  eq(docs.get('t2').baseHash, 'h2', 'B2 成功：baseHash 换成响应里的新 hash')
  eq(docs.get('t2').dirty, false, 'B2 成功：脏清除')
  eq(docs.get('t2').banner, undefined, 'B2 成功：无条')
}

// —— B3. bridge 掉线：全局健康信号要红，文档状态按 code 走（评审 #5）——
{
  reset()
  const docs = createEditorDocs({ send: (t, p) => useAppStore.getState().editCommand(t, p) })
  const base = emitted.length
  const pOpen = docs.open('t3', '/ws/c.txt')
  emit({ event_type: 'command_response', id: lastOf('file_open', base).id, ok: true, data: { ok: true, path: '/ws/c.txt', content: 'x', hash: 'h1' } })
  await pOpen
  docs.change('t3', 'y')
  const pSave = docs.save('t3')
  const saveCmd = lastOf('file_save', base)
  emit({ event_type: 'command_response', id: saveCmd.id, ok: false, code: 'write_failed', error: 'EPIPE: broken pipe' })
  eq((await pSave).ok, false, 'B3 保存失败')
  eq(useAppStore.getState().bridgeStatus, 'error', 'B3 bridge 掉线 → **全局** bridgeStatus 变红（不能只出文档红条）')
  assert(String(useAppStore.getState().bridgeError || '').includes('EPIPE'), 'B3 全局 bridgeError 带原始原因')
  // 「只动全局信号，不碰文档状态」：文档按 code=write_failed 走 error 条，仍是 ready（不是 invalid）
  eq(docs.get('t3').status, 'ready', 'B3 文档状态不被全局信号改写（仍可继续编辑）')
  eq(docs.get('t3').banner.kind, 'error', 'B3 文档只出通用错误条（由 code 决定，不由 bridge 状态决定）')
}

// —— B4. 回滚开关的唯一判据在 store（评审 #2）——
{
  reset()
  const st = () => useAppStore.getState()
  const fileTabs = () => {
    const s = st()
    const set = s.tabsByScope[s.activeSessionId || s.workspace] || {}
    return (set.tabs || []).filter((t) => t.kind === 'file')
  }
  const settings = st().settings
  useAppStore.setState({ settings: { ...settings, editor: false } })
  st().openFileForEdit('/ws/x.go')
  eq(fileTabs().length, 0, 'B4 settings.editor=false → openFileForEdit **不产生** edit 标签（入口判据在 store）')
  useAppStore.setState({ settings: { ...settings, editor: true } })
  st().openFileForEdit('/ws/x.go')
  const tabs = fileTabs()
  eq(tabs.length, 1, 'B4 开关打开 → 产生一个 file 标签')
  eq(tabs[0].mode, 'edit', 'B4 且 mode=edit（身份 = (path, mode)）')
  st().openFileForEdit('/mock/shot.PNG')
  eq(fileTabs().length, 1, 'B4 非文本（.PNG）即使开关开着也不建 edit 标签')
}

// —— B5. 批量关闭：脏标签进队列逐个问，干净的直接关，一个都不静默丢（评审 #4）——
{
  reset()
  const st = () => useAppStore.getState()
  const ids = {}
  for (const name of ['a', 'b', 'c']) {
    ids[name] = st().openTab({ kind: 'file', path: `/ws/${name}.go`, mode: 'source' })
  }
  // 只数 file 标签：作用域里总有一个默认标签（syncTabsToRightTab 的既有语义），不要它来干扰计数
  const tabIds = () => {
    const s = st()
    const set = s.tabsByScope[s.activeSessionId || s.workspace] || {}
    return (set.tabs || []).filter((t) => t.kind === 'file').map((t) => t.id)
  }
  eq(tabIds().length, 3, 'B5 三个 file 标签就位')
  st().setTabDirty(ids.b, true)
  st().setTabDirty(ids.c, true)

  // 关其它（保留 a）：干净的一个不留（本来就是"其它"），脏的两个必须都留下
  st().closeTabs([ids.b, ids.c])
  eq(tabIds().length, 3, 'B5 关其它：两个脏标签**一个都没关**（不静默丢缓冲区）')
  const q = st().closeConfirm
  eq(q && q.reason === 'close' && q.tabIds.length, 2, 'B5 确认队列里有 2 个待确认的脏标签')

  // 逐个确认：处理完一个，队列推进到下一个，标签才真的消失
  st().closeConfirmNext(ids.b)
  eq(tabIds().includes(ids.b), false, 'B5 确认第 1 个 → 它被关掉')
  eq(tabIds().includes(ids.c), true, 'B5 **只剩未确认的那个**（c 还在）')
  eq(st().closeConfirm.tabIds.length, 1, 'B5 队列推进到第 2 个')
  st().closeConfirmNext(ids.c)
  eq(tabIds().length, 1, 'B5 确认第 2 个 → 只剩原本保留的 a')
  eq(st().closeConfirm, undefined, 'B5 队列清空后确认框收起')

  // 取消：队列整体作废，标签全部保留（不许因为取消而丢内容）
  st().setTabDirty(ids.a, true)
  st().closeTab(ids.a)
  eq(st().closeConfirm.tabIds.length, 1, 'B5 单个脏标签关闭 → 进队列')
  st().cancelCloseConfirm()
  eq(st().closeConfirm, undefined, 'B5 取消 → 框收起')
  eq(tabIds().includes(ids.a), true, 'B5 取消 → 标签还在（内容不动）')
}

console.log(`\n✅ test-editor: ${pass} 项全通过`)
