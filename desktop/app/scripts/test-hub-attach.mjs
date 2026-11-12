#!/usr/bin/env node
// Hub 附件（图片 / 文件引用）的纯逻辑 + store 契约（Node 直跑，不需要浏览器）。
//
// 为什么单独一组：附件横跨四个入口（粘贴 / 拖拽 / 选图 / 纸夹）和三条出口
// （乐观气泡 / 失败重试 / mock 回显），任何一处口径不同都会悄悄坏掉：
//   · 换一张图再发仍然命中旧 cid → bridge 幂等挡成旧消息（用户以为没发出去）；
//   · 文件引用重复拼进正文 → 模型看到两份 @path；
//   · 旧消息没有 attachments → 渲染层崩；
//   · 纯图片（正文为空）被 store/composer 的 `!text` 门禁挡住。
// 这些都不需要浏览器就能钉死。
//
// 覆盖：
//   · 常量：MAX_HUB_ATTACHMENTS=10，MAX_HUB_IMAGE_BYTES=3MiB（与主会话同口径）
//   · attachmentFingerprint：稳定排序（换顺序同指纹）、换图/换文件/换大小必变、
//     undefined = 空串
//   · withFileRefs：一行一个 @path、去重、保留原换行、正文里已有就不重复、
//     空正文只留引用行（不留前导空行）
//   · validateAttachments / imagesOf / filesOf：undefined 兼容、张数/体积/必需字段
//   · store.send：纯图片（text 为空）可发，wire 上带 attachments；mock 回显后
//     时间线里的用户消息带 data_url
//   · store：失败记录带 attachments + 指纹；retry 沿用 cid 且带同一张图；
//     换图重发不复用旧 cid（两条失败各自保留）；群同理
import { build } from 'esbuild'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const hubDir = join(__dirname, '..', 'src', 'hub')

const result = await build({
  stdin: {
    contents: [
      "export * as attachments from './attachments.ts'",
      "export * as store from './useHubStore.ts'",
      "export * as transport from '../transport/index.ts'",
    ].join('\n'),
    resolveDir: hubDir,
    sourcefile: 'hub-attach-entry.ts',
    loader: 'ts',
  },
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
  // useHubStore 会把 mock transport 一起拉进来；Vite 的 ?raw/?url/?no-inline 在
  // esbuild 里没有对应 loader，给它们一个空模块（本测试不渲染品牌图）。
  plugins: [{
    name: 'stub-vite-assets',
    setup(build) {
      build.onResolve({ filter: /\?(raw|url|no-inline)$/ }, (args) => ({ path: args.path, namespace: 'vite-asset' }))
      build.onLoad({ filter: /.*/, namespace: 'vite-asset' }, () => ({ contents: 'export default ""', loader: 'js' }))
    },
  }],
})
const source = result.outputFiles[0].text
const mod = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)
const { attachments: A, store, transport } = mod
const H = store.useHubStore

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
// data URL 很长：成功行只留摘要，失败时才需要看全文（这里仍截断，够定位）。
const brief = (v) => {
  const s = JSON.stringify(v)
  return s.length > 160 ? `${s.slice(0, 157)}…` : s
}
const eq = (got, want, m) => assert(got === want, `${m}（got=${brief(got)} want=${brief(want)}）`)

const PNG_A = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAYAAADED76LAAAALklEQVR42mPQ0DH5f7MqDSsGyTHAGNgkQTQDugA6mwFdF7ppxCvAawVeRxLyJgDMMZrx26+tKgAAAABJRU5ErkJggg=='
const PNG_B = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=='
const img = (dataUrl, name = 'shot.png') => ({ kind: 'image', name, mime_type: 'image/png', bytes: dataUrl.length, data_url: dataUrl })
const file = (path, name = 'pricing.md', bytes = 2140) => ({ kind: 'file', name, path, bytes })

// —— 1) 纯函数：常量 / 指纹 / 正文拼接 / 校验 ——
{
  eq(A.MAX_HUB_ATTACHMENTS, 10, '附件张数上限 = 10（与主会话一致）')
  eq(A.MAX_HUB_IMAGE_BYTES, 3 * 1024 * 1024, '图片总量上限 = 3MiB（复用 lib/imageAttach）')

  const a = [img(PNG_A, 'a.png'), file('/ws/pricing.md')]
  const b = [file('/ws/pricing.md'), img(PNG_A, 'a.png')]
  eq(A.attachmentFingerprint(a), A.attachmentFingerprint(b), '同一批附件换顺序 → 指纹不变（不换 cid）')
  eq(A.attachmentFingerprint(undefined), '', '旧调用无附件 → 空指纹')
  eq(A.attachmentFingerprint([]), '', '空附件 → 空指纹')
  assert(A.attachmentFingerprint(a) !== A.attachmentFingerprint([img(PNG_B, 'a.png'), file('/ws/pricing.md')]),
    '换一张图 → 指纹改变（换图重发不复用旧 cid）')
  assert(A.attachmentFingerprint(a) !== A.attachmentFingerprint([img(PNG_A, 'a.png'), file('/ws/other.md')]),
    '换文件路径 → 指纹改变')
  assert(A.attachmentFingerprint(a) !== A.attachmentFingerprint([img(PNG_A, 'b.png'), file('/ws/pricing.md')]),
    '换文件名 → 指纹改变')
  assert(A.attachmentFingerprint(a) !== A.attachmentFingerprint([{ ...img(PNG_A, 'a.png'), bytes: 1 }, file('/ws/pricing.md')]),
    '换大小 → 指纹改变')
  assert(A.attachmentFingerprint([img(PNG_A)]) !== A.attachmentFingerprint([file('/ws/x.png')]),
    '同 name 不同类型 → 指纹改变')

  eq(A.withFileRefs('看看这个', [file('/ws/a.md')]), '看看这个\n@/ws/a.md', '文件引用追加到正文（一行一个）')
  eq(A.withFileRefs('', [file('/ws/a.md')]), '@/ws/a.md', '空正文只留引用行（无前导空行）')
  eq(A.withFileRefs('第一行\n第二行', [file('/ws/a.md'), file('/ws/b.md')]), '第一行\n第二行\n@/ws/a.md\n@/ws/b.md',
    '保留原有换行 + 每个文件一行')
  eq(A.withFileRefs('看看这个\n@/ws/a.md', [file('/ws/a.md')]), '看看这个\n@/ws/a.md', '正文里已有同路径 → 不重复拼')
  eq(A.withFileRefs('看看', [file('/ws/a b.md')]), '看看\n@/ws/a%20b.md', '路径含空格 → %20（@token 以空白为界）')
  eq(A.withFileRefs('看看\n@/ws/a%20b.md', [file('/ws/a b.md')]), '看看\n@/ws/a%20b.md', '已拼过的 %20 引用不重复')
  eq(A.withFileRefs('看看', [file('/ws/a.md'), file('/ws/a.md')]), '看看\n@/ws/a.md', '同一路径传入两次 → 只拼一次')
  eq(A.withFileRefs('@/ws/a.md.bak', [file('/ws/a.md')]), '@/ws/a.md.bak\n@/ws/a.md', '前缀相同但 token 边界不同 → 仍然追加')
  eq(A.withFileRefs('看看', [img(PNG_A)]), '看看', '图片不进正文（走 attachments）')
  eq(A.withFileRefs('看看', undefined), '看看', '无文件 → 正文原样')

  assert(A.validateAttachments(undefined).ok, '旧调用（undefined）合法')
  eq(A.validateAttachments(undefined).attachments.length, 0, 'undefined → 空附件表')
  eq(A.imagesOf(undefined).length, 0, 'imagesOf(undefined) 为空（旧消息不崩）')
  eq(A.filesOf(undefined).length, 0, 'filesOf(undefined) 为空')
  eq(A.imagesOf([img(PNG_A), file('/ws/a')]).length, 1, 'imagesOf 只取图片')
  eq(A.filesOf([img(PNG_A), file('/ws/a')]).length, 1, 'filesOf 只取文件')
  eq(A.imagesBytes([img(PNG_A)]), PNG_A.length, 'imagesBytes 累计 data_url 体积')

  const tooMany = Array.from({ length: 11 }, (_, i) => file(`/ws/${i}.md`))
  assert(!A.validateAttachments(tooMany).ok, '11 个附件 → 不合法')
  assert(!A.validateAttachments([{ kind: 'image', name: 'x.png' }]).ok, '图片缺 data_url → 不合法')
  assert(!A.validateAttachments([{ kind: 'file', name: 'x.md' }]).ok, '文件缺 path → 不合法')
  const huge = 'data:image/png;base64,' + 'A'.repeat(A.MAX_HUB_IMAGE_BYTES)
  assert(!A.validateAttachments([img(huge)]).ok, '图片总量超 3MiB → 不合法')
  assert(A.validateAttachments([{ kind: 'file', path: '/ws/a.md' }]).ok, '最小合法文件引用（无 name/size）')
  eq(A.normalizeAttachments([null, 1, {}, { kind: 'image' }]).length, 0, '非法条目被丢掉（不崩）')
}

// —— 2) store：DM 纯图片发送 + mock 回显 ——
const t = transport.getTransport()
const off = store.attachHubEvents?.()
const waitFor = async (pred, label, timeoutMs = 5000) => {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (pred()) return
    await new Promise((r) => setTimeout(r, 20))
  }
  throw new Error(`等待超时：${label}`)
}

function spyWire() {
  const seen = []
  const orig = t.send.bind(t)
  t.send = (cmd) => {
    if (cmd?.type === 'hub_send' || cmd?.type === 'hub_group_send') {
      const p = cmd.payload ?? {}
      seen.push({
        type: cmd.type,
        text: String(p.text ?? ''),
        cid: String(p.client_msg_id ?? ''),
        attachments: Array.isArray(p.attachments) ? p.attachments : [],
      })
    }
    return orig(cmd)
  }
  return { seen, restore: () => { t.send = orig } }
}

{
  H.setState({ open: true })
  await H.getState().select('ag_mockres1')
  eq(H.getState().activeId, 'ag_mockres1', 'DM 选中目标智能体')

  const wire = spyWire()
  const okImg = await H.getState().send('', undefined, [img(PNG_A)])
  eq(okImg, true, '纯图片（正文为空）允许发送')
  const last = wire.seen[wire.seen.length - 1]
  eq(last.text, '', 'wire 上正文为空')
  eq(last.attachments.length, 1, 'wire 上带 attachments')
  eq(last.attachments[0].data_url, PNG_A, 'wire 上带压缩后的 data_url')
  await waitFor(() => H.getState().messages.some((m) => m.sender?.kind === 'human' && m.attachments?.length),
    'mock 回显带附件的用户消息')
  const echoed = H.getState().messages.filter((m) => m.sender?.kind === 'human' && m.attachments?.length).pop()
  eq(echoed.attachments[0].data_url, PNG_A, 'mock 回显把 attachments 原样带回（时间线能渲染图片）')
  eq(H.getState().messages.filter((m) => m.pending).length, 0, '发送成功后没有残留的 pending 气泡')
  wire.restore()
}

// —— 3) store：失败保留附件与指纹；retry 沿用 cid 且带同一张图 ——
{
  const wire = spyWire()
  const okSend = await H.getState().send('__lostack__', undefined, [img(PNG_A), file('/ws/pricing.md')])
  eq(okSend, false, 'lost-ack 对前端是失败')
  const failed = H.getState().failed
  assert(!!failed, '失败记录存在')
  eq(failed.attachments?.length, 2, '失败记录带上附件（重试才有图）')
  eq(failed.attachments[0].data_url, PNG_A, '失败记录里的图片 data_url 完整')
  eq(failed.attachmentsFingerprint, A.attachmentFingerprint([img(PNG_A), file('/ws/pricing.md')]), '失败记录带附件指纹')
  const cid = failed.clientMsgId
  const bubble = H.getState().messages.find((m) => m.clientMsgId === cid)
  assert(!!bubble && bubble.failed && bubble.attachments?.length === 2, '失败气泡仍显示附件（不静默丢图）')

  const okRetry = await H.getState().retry(cid)
  eq(okRetry, true, 'retry 成功')
  const wireRetry = wire.seen[wire.seen.length - 1]
  eq(wireRetry.cid, cid, 'retry 沿用首发 cid（不重复入账）')
  eq(wireRetry.attachments.length, 2, 'retry 的 payload 带同一批附件')
  eq(H.getState().failed, null, '成功后失败记录清空')
  await H.getState().select('ag_mockres1')
  const human = H.getState().messages.filter((m) => m.sender?.kind === 'human' && m.text === '__lostack__')
  eq(human.length, 1, '账本里只有 1 条（cid 幂等，附件没让它重复入账）')
  eq(human[0].attachments?.[0]?.data_url, PNG_A, '账本里的那条也带图')
  wire.restore()
}

// —— 4) store：换图重发 ≠ 同一条消息（不复用旧 cid） ——
{
  const wire = spyWire()
  await H.getState().send('__lostack__', undefined, [img(PNG_A)])
  const first = H.getState().failed
  assert(!!first, '第一张图发送失败')
  const cidA = first.clientMsgId
  // 用户删掉旧图、换成另一张，再发一次同样的正文：正文相同但附件不同。
  const okSend = await H.getState().send('__lostack__', undefined, [img(PNG_B)])
  eq(okSend, false, '第二张图也失败（mock 注入）')
  const cidB = H.getState().failed?.clientMsgId ?? ''
  assert(cidB !== '', '第二次失败有 cid')
  assert(cidB !== cidA, '换图后不复用旧 cid（否则 bridge 幂等会把它挡成旧消息）')
  eq(Object.keys(H.getState().failedByCid).length, 2, '两条失败各自保留记录（各自的附件）')
  eq(H.getState().failedByCid[cidA].attachments?.[0].data_url, PNG_A, '旧记录的图没被覆盖')
  eq(H.getState().failedByCid[cidB].attachments?.[0].data_url, PNG_B, '新记录带新图')

  // 同一个 cid 重试要带上它自己的那张图（B2 的附件版）。
  const wireCount = wire.seen.length
  eq(await H.getState().retry(cidA), true, '重试旧的那条成功')
  const retried = wire.seen.slice(wireCount).find((w) => w.cid === cidA)
  assert(!!retried, 'wire 上出现的是旧 cid')
  eq(retried.attachments[0].data_url, PNG_A, '重试旧 cid 带的是它自己的图（不是最新那张）')
  wire.restore()
}

// —— 5) 群：图片 + 文件引用并存，正文为空也允许 ——
{
  await H.getState().selectGroup('grp_mock1')
  await waitFor(() => H.getState().activeGroupId === 'grp_mock1' && !H.getState().loading, '群打开')
  const wire = spyWire()
  const okGroup = await H.getState().sendGroup('', undefined, false, undefined, [img(PNG_A)])
  eq(okGroup, true, '群：纯图片（无正文）允许发送')
  const sent = wire.seen[wire.seen.length - 1]
  eq(sent.type, 'hub_group_send', '走的是 hub_group_send')
  eq(sent.attachments.length, 1, '群 wire 上带 attachments')
  await waitFor(() => H.getState().groupMessages.some((m) => m.sender?.kind === 'human' && m.attachments?.length),
    '群 mock 回显带附件的消息')

  const okFiles = await H.getState().sendGroup('看这个\n@/ws/a.md', ['ag_mockres1'], false, undefined, [file('/ws/a.md')])
  eq(okFiles, true, '群：文件引用 + @ 一起发')
  const sent2 = wire.seen[wire.seen.length - 1]
  eq(sent2.attachments[0].path, '/ws/a.md', '群 wire 上带文件引用 path')
  eq(sent2.attachments[0].kind, 'file', '文件附件的 kind=file')
  eq(sent2.text, '看这个\n@/ws/a.md', '正文里的 @path 原样上线（后端不读文件）')
  wire.restore()
}

if (typeof off === 'function') off()
console.log(`\n通过 ${pass} 项断言`)
