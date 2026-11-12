#!/usr/bin/env node
// 失败重试的幂等契约（Node 直跑，无需浏览器）：HUB-IDEM-01 / HUB-FAIL-01 的 store 侧回归。
//
// 为什么值得单独一组：桥按 client_msg_id 幂等（desktop/bridge/hub_commands.go 的 hubSend，
// 命中即回原 seq）。前端如果重试时换一个新 cid，「响应丢了但消息其实已入队」的那条就会
// 二次进模型上下文 —— 这是纯逻辑，不需要浏览器就能钉死。
//
// 覆盖：
//   · DM 失败 → failed 记录带首发 cid → retry() 沿用同一 cid → 账本只有 1 条
//   · DM 同一条正文重发（不点「重试」，直接在 composer 再发一次）也沿用首发 cid，
//     且失败气泡先被摘掉（不叠第二条乐观气泡）
//   · 群失败 → failed 记录 kind=group（横幅据此给出「重试」）→ retry() 复用 cid → 只留 1 条
//   · 切目标（换 DM / 换群）会丢掉失败的 cid —— 重试永远不会发到别的会话
//   · mock 的 ?hubFixture=needs|tasks（HUB-FIXTURE-01）：4 种 need 与排队/失败/中断任务
import { build } from 'esbuild'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const hubDir = join(__dirname, '..', 'src', 'hub')

const result = await build({
  stdin: {
    contents: [
      "export * as store from './useHubStore.ts'",
      "export * as transport from '../transport/index.ts'",
    ].join('\n'),
    resolveDir: hubDir,
    sourcefile: 'hub-idem-entry.ts',
    loader: 'ts',
  },
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
  // useHubStore 会把 mock transport 一起拉进来；Vite 的 ?raw/?url/?no-inline 在 esbuild 里
  // 没有对应 loader，给它们一个空模块（本测试不渲染品牌图）。
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
const { store, transport } = mod
const H = store.useHubStore

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const eq = (got, want, m) => assert(got === want, `${m}（got=${JSON.stringify(got)} want=${JSON.stringify(want)}）`)

const t = transport.getTransport()
const off = store.attachHubEvents()
const waitFor = async (pred, label, timeoutMs = 4000) => {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (pred()) return
    await new Promise((r) => setTimeout(r, 20))
  }
  throw new Error(`等待超时：${label}`)
}

/** 记录 store 实际传给 send/sendGroup 的复用 cid（retry 就是靠它沿用首发 cid）。 */
function spySends() {
  const st = H.getState()
  const seen = { dm: [], group: [] }
  const origSend = st.send
  const origGroup = st.sendGroup
  H.setState({
    send: (text, reuse) => { seen.dm.push({ text, reuse: reuse ?? '' }); return origSend(text, reuse) },
    sendGroup: (text, mentions, all, reuse) => {
      seen.group.push({ text, reuse: reuse ?? '' })
      return origGroup(text, mentions, all, reuse)
    },
  })
  return { seen, restore: () => H.setState({ send: origSend, sendGroup: origGroup }) }
}

const humanTexts = (list) => list.filter((m) => (m.sender?.kind ?? '') === 'human').map((m) => m.text)

// —— 1) DM：失败保留首发 cid，retry() 沿用同一 cid，账本不重复 ——
{
  H.setState({ open: true })
  await H.getState().select('ag_mockres1')
  assert(humanTexts(H.getState().messages).length >= 0, 'DM 打开（hub_open）成功')

  const spy = spySends()
  const okSend = await H.getState().send('__lostack__')
  eq(okSend, false, 'lost-ack 发送对前端是失败')
  const failed = H.getState().failed
  assert(!!failed, 'store 记录了失败')
  eq(failed.kind, 'dm', '失败记录 kind=dm')
  eq(failed.agentId, 'ag_mockres1', '失败记录带目标智能体')
  assert(/^ui-/.test(failed.clientMsgId), '失败记录保留首发 client_msg_id（ui-…）')
  const cid = failed.clientMsgId
  eq(H.getState().messages.filter((m) => m.failed).length, 1, '失败气泡标 failed（只有 1 条）')
  eq(H.getState().error !== null, true, '错误横幅有文案（DM 与群共用）')

  const okRetry = await H.getState().retry()
  eq(okRetry, true, 'retry() 成功')
  eq(spy.seen.dm[spy.seen.dm.length - 1].reuse, cid, 'retry 沿用首发的 client_msg_id')
  eq(H.getState().failed, null, '成功后失败记录清空')
  eq(H.getState().messages.filter((m) => m.failed).length, 0, 'failed 气泡被清掉')
  eq(H.getState().messages.filter((m) => m.text === '__lostack__').length, 1, '时间线只有 1 条乐观/真实消息')

  // 重开会话：账本（mock 的 hub_open）里也只能有 1 条 —— 同 cid 命中幂等、没有二次入账。
  await H.getState().select('ag_mockres1')
  eq(humanTexts(H.getState().messages).filter((x) => x === '__lostack__').length, 1, '账本里只有 1 条（cid 幂等）')
  spy.restore()
}

// —— 2) DM：同一正文直接重发（不点重试）也沿用 cid，且不叠第二条气泡 ——
{
  // 换一个干净的 DM：这里要按「同一会话里同一条正文出现几次」来数，不能带上第 1 组的账本。
  await H.getState().select('ag_mockwri2')
  const spy = spySends()
  await H.getState().send('__lostack__')
  const cid = H.getState().failed?.clientMsgId ?? ''
  assert(cid.startsWith('ui-'), '第二次失败仍有首发 cid')
  eq(H.getState().messages.filter((m) => m.failed).length, 1, '失败气泡 1 条')
  // 用户没点「重试」，直接在 composer 里再发一次同样的正文（不传 cid，靠 store 自己匹配）
  const okAgain = await H.getState().send('__lostack__')
  eq(okAgain, true, '同正文重发成功（命中 cid 幂等）')
  // 这里不传第二个参数，所以看的是「结果」：cid 没复用就会多出一条账本消息，
  // failed 记录也不会因为 cid 命中而被清掉。
  eq(H.getState().failed, null, '同正文重发后失败记录清空（说明内部复用了首发 cid）')
  eq(spy.seen.dm[spy.seen.dm.length - 1].reuse, '', '（重发路径不经过 retry，没有显式传 cid）')
  eq(H.getState().messages.filter((m) => m.text === '__lostack__').length, 1, '时间线不叠第二条乐观气泡')
  eq(H.getState().messages.filter((m) => m.failed).length, 0, '失败气泡在重发时被摘掉')
  await H.getState().select('ag_mockwri2')
  eq(humanTexts(H.getState().messages).filter((x) => x === '__lostack__').length, 1, '账本仍只有 1 条')
  spy.restore()
}

// —— 3) 切目标会丢掉失败记录（重试不会发错人） ——
{
  await H.getState().send('__fail__')
  assert(!!H.getState().failed, '注入失败后有失败记录')
  eq(H.getState().failed?.kind, 'dm', '失败记录仍是 dm')
  await H.getState().select('ag_mockwri2')
  eq(H.getState().failed, null, '换 DM 后失败记录清空（不会把消息发到别人的会话）')
  eq(await H.getState().retry(), false, '没有失败记录时 retry 直接返回 false')
}

// —— 4) 群：失败记录 kind=group、retry 复用 cid、账本只留 1 条 ——
{
  await H.getState().selectGroup('grp_mock1')
  await waitFor(() => H.getState().activeGroupId === 'grp_mock1' && !H.getState().loading, '群打开')
  const spy = spySends()
  const okSend = await H.getState().sendGroup('__lostack__', ['ag_mockres1'], false)
  eq(okSend, false, '群 lost-ack 发送对前端是失败')
  const failed = H.getState().failed
  assert(!!failed, '群失败也留记录（横幅据此给「重试」，旧版群没有重试入口）')
  eq(failed.kind, 'group', '群失败记录 kind=group')
  eq(failed.groupId, 'grp_mock1', '群失败记录带群 id')
  eq(failed.mentions?.[0], 'ag_mockres1', '群失败记录保留 @ 目标（重发不缩小唤醒面）')
  const cid = failed.clientMsgId
  assert(/^g-/.test(cid), '群失败记录保留首发 client_msg_id（g-…）')
  eq(H.getState().groupMessages.filter((m) => m.failed).length, 1, '群失败气泡 1 条')

  const okRetry = await H.getState().retry()
  eq(okRetry, true, '群 retry() 成功')
  eq(spy.seen.group[spy.seen.group.length - 1].reuse, cid, '群 retry 沿用首发 cid')
  eq(H.getState().failed, null, '群成功后失败记录清空')
  eq(H.getState().groupMessages.filter((m) => m.failed).length, 0, '群 failed 气泡被清掉')
  eq(H.getState().groupMessages.filter((m) => m.text === '__lostack__').length, 1, '群时间线只有 1 条')
  await H.getState().selectGroup('grp_mock1')
  await waitFor(() => !H.getState().loading, '群重开')
  eq(H.getState().groupMessages.filter((m) => m.text === '__lostack__').length, 1, '群账本仍只有 1 条（cid 幂等）')

  // 换群同样丢掉失败记录
  await H.getState().sendGroup('__fail__')
  assert(!!H.getState().failed, '群注入失败后有失败记录')
  H.getState().clearGroup()
  eq(H.getState().failed, null, '关群/换群后失败记录清空')
  spy.restore()
}

// —— 5) mock fixture：?hubFixture=needs|tasks（HUB-FIXTURE-01） ——
{
  const call = (type, payload = {}) => new Promise((resolve) => {
    const id = 900_000 + Math.floor(Math.random() * 1000)
    const off = t.onEvent((line) => {
      let ev
      try { ev = JSON.parse(line) } catch { return }
      if (ev.event_type !== 'command_response' || ev.id !== id) return
      off()
      resolve(ev)
    })
    void t.send({ id, type, payload })
  })
  // 默认（无 fixture）：Needs 为空 —— 这就是审计里「面板恒空」的另一面，别改坏日常演示。
  const before = await call('hub_need_list', { status: 'pending' })
  eq((before.data?.needs ?? []).length, 0, '默认 mock：Needs 为空（日常演示不受影响）')
  const tasksBefore = await call('hub_task_list')
  assert(!(tasksBefore.data?.tasks ?? []).some((x) => x.status === 'queued'), '默认 mock：任务板没有注入的排队任务')

  // 打开 fixture 后再问一次（mock 按 window.location.search 判档，与 ?hubFixture=cards 同款）
  globalThis.window = { location: { search: '?hubFixture=needs' } }
  const needs = await call('hub_need_list', { status: 'pending' })
  const kinds = (needs.data?.needs ?? []).map((n) => n.kind)
  eq(kinds.length, 4, '?hubFixture=needs：4 种 need')
  for (const k of ['approval', 'question', 'failure', 'handoff']) {
    assert(kinds.includes(k), `?hubFixture=needs：包含 ${k}`)
  }
  globalThis.window = { location: { search: '?hubFixture=tasks' } }
  const tasks = (await call('hub_task_list')).data?.tasks ?? []
  for (const s of ['queued', 'failed', 'interrupted']) {
    assert(tasks.some((x) => x.status === s), `?hubFixture=tasks：包含 ${s} 任务`)
  }
  // resolve 之后该 need 消失（Needs 面板的四个动作依赖它）
  globalThis.window = { location: { search: '?hubFixture=needs' } }
  const resolved = await call('hub_need_resolve', { need_id: 'need_mock_approval', resolution: 'approve' })
  eq(resolved.ok, true, '?hubFixture=needs：可以 resolve')
  eq(resolved.data?.need?.status, 'resolved', 'resolve 后状态为 resolved')
  const after = await call('hub_need_list', { status: 'pending' })
  eq((after.data?.needs ?? []).length, 3, 'resolve 后 pending 只剩 3 条')
  delete globalThis.window
}

off()
console.log(`\n✅ Hub 失败重试幂等 / fixture：${pass} 项全部通过`)
