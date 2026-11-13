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
//   · 复审 B1：两条失败时按气泡的 cid 重试（wire 上正文/cid 都是点的那一条）
//   · 复审 B2：重试成功后草稿清空（用户改过的不清），账本不再同文 +1
//   · 复审 B3：成功一条只清它自己的 cid；旧失败原文重发仍复用首发 cid
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

/**
 * 记录真正上线（mock transport 的 send）的 hub_send / hub_group_send 载荷。
 *
 * 比 spySends 更贴复审现场：B1 的缺陷是「点了 A、wire 上却是 B」，只有看到
 * payload.text / client_msg_id 才能钉死它，而不是只看 store 的入参。
 */
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
        to: String(p.target?.agent_id ?? ''),
        group: String(p.group_id ?? ''),
      })
    }
    return orig(cmd)
  }
  return { seen, restore: () => { t.send = orig } }
}

const humanTexts = (list) => list.filter((m) => (m.sender?.kind ?? '') === 'human').map((m) => m.text)
const countText = (list, text) => humanTexts(list).filter((x) => x === text).length

// —— 1) DM：失败保留首发 cid，retry() 沿用同一 cid，账本不重复 ——
{
  H.setState({ open: true })
  await H.getState().select('ag_mockres1')
  eq(H.getState().activeId, 'ag_mockres1', 'DM 打开（hub_open）选中目标智能体')
  eq(countText(H.getState().messages, '先列一下三家竞品的价格。'), 1, 'DM 打开拿到历史（种子 1 条人工消息）')

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

// —— 5) 复审 B1：两条失败各自重试，wire 上是「点的那一条」 ——
// 复审现场：DM 发 __lostack__（A 失败）→ 发 __fail__（B 失败）→ 点 A 的重试，
// wire 里却是 {text:'__fail__', client_msg_id: B.cid}（retry 永远取最新 failed）。
{
  await H.getState().select('ag_mockwri2')
  const wire = spyWire()
  eq(await H.getState().send('__lostack__'), false, 'B1：A（__lostack__）对前端是失败')
  const cidA = H.getState().failed?.clientMsgId ?? ''
  eq(await H.getState().send('__fail__'), false, 'B1：B（__fail__）对前端是失败')
  const cidB = H.getState().failed?.clientMsgId ?? ''
  assert(cidA.startsWith('ui-') && cidB.startsWith('ui-') && cidA !== cidB, 'B1：两条失败各带首发 cid')
  eq(Object.keys(H.getState().failedByCid).length, 2, 'B1：失败表里两条记录都在（不再被单槽覆盖）')
  eq(H.getState().messages.filter((m) => m.failed).length, 2, 'B1：时间线上两条失败气泡')

  // 点 A 气泡的行内「重试」= retry(A 的 cid)
  eq(await H.getState().retry(cidA), true, 'B1：按 A 的 cid 重试成功（mock 按 cid 幂等）')
  const wireA = wire.seen[wire.seen.length - 1]
  eq(wireA.text, '__lostack__', 'B1：wire 正文是 A（不是最新失败的 B）')
  eq(wireA.cid, cidA, 'B1：wire cid 是 A 的首发 cid')
  eq(wireA.to, 'ag_mockwri2', 'B1：仍发回同一个 DM')
  eq(H.getState().failedByCid[cidA], undefined, 'B1：只清 A 的记录')
  eq(Object.keys(H.getState().failedByCid).join(','), cidB, 'B1：B 的记录还在（它自己还能重试）')
  eq(H.getState().failed?.clientMsgId, cidB, 'B1：横幅改指向还没重试的 B')

  // 点 B 的重试：mock 对 __fail__ 恒定注入失败，但 wire 上必须是 B 自己的正文 + cid
  eq(await H.getState().retry(cidB), false, 'B1：B 重试仍失败（mock 对 __fail__ 恒定注入）')
  const wireB = wire.seen[wire.seen.length - 1]
  eq(wireB.text, '__fail__', 'B1：wire 正文是 B')
  eq(wireB.cid, cidB, 'B1：wire cid 是 B 的首发 cid（没有换新）')
  wire.restore()
  H.getState().clearError()
}

// —— 6) 复审 B2：重试成功清草稿，再点发送不会同文 +1 ——
// 复审现场：失败草稿留在输入框 → 横幅重试成功 → 输入框不动 → 再点发送用新 cid，账本同文 2 条。
{
  await H.getState().select('ag_mockres1')
  const base = countText(H.getState().messages, '__lostack__')
  eq(await H.getState().send('__lostack__'), false, 'B2：DM 发送失败')
  const cid = H.getState().failed?.clientMsgId ?? ''
  // composer 失败后不丢草稿（与 HubSurface.submit 同语义）：这里把输入框还原成原文。
  H.getState().setDraft('ag_mockres1', `  ${'__lostack__'}  `)
  eq(await H.getState().retry(cid), true, 'B2：横幅重试成功（cid 幂等，账本不新增）')
  eq(H.getState().drafts['ag_mockres1'], '', 'B2：重试成功后草稿清空（比较忽略首尾空白）')
  await H.getState().select('ag_mockres1')
  const atReopen = H.getState().messages
  eq(countText(atReopen, '__lostack__'), base + 1, 'B2：账本只多了这一次发送的 1 条（重试没二次入账）')
  // 用户后来改过的草稿不能被重试抹掉
  eq(await H.getState().send('__lostack__'), false, 'B2：再失败一次')
  const cid2 = H.getState().failed?.clientMsgId ?? ''
  H.getState().setDraft('ag_mockres1', '我改成别的了')
  eq(await H.getState().retry(cid2), true, 'B2：再次重试成功')
  eq(H.getState().drafts['ag_mockres1'], '我改成别的了', 'B2：改过的草稿保留（不无条件清）')

  // 群同理：群 composer 草稿在 store 里，横幅重试（不经过 GroupView.submit）也能清
  await H.getState().selectGroup('grp_mock1')
  await waitFor(() => H.getState().activeGroupId === 'grp_mock1' && !H.getState().loading, '群打开（B2）')
  eq(await H.getState().sendGroup('__lostack__'), false, 'B2：群发送失败')
  const gcid = H.getState().failed?.clientMsgId ?? ''
  H.getState().setGroupDraft('grp_mock1', '__lostack__')
  eq(await H.getState().retry(gcid), true, 'B2：群横幅重试成功')
  eq(H.getState().groupDraftText['grp_mock1'], '', 'B2：群重试成功后草稿清空')
}

// —— 7) 复审 B3：成功一条不清别人的 cid；旧失败原文重发仍用首发 cid ——
// 复审现场：失败 A → 成功 B（无条件 failed:null）→ 重发 A 换新 cid → 账本 A 计数 2。
{
  await H.getState().select('ag_mockwri2')
  const base = countText(H.getState().messages, '__lostack__')
  const wire = spyWire()
  eq(await H.getState().send('__lostack__'), false, 'B3：A 失败（已落账、响应丢失）')
  const cidA = H.getState().failed?.clientMsgId ?? ''
  eq(await H.getState().send('B3 中间那条成功的'), true, 'B3：B 成功（成功不能把 A 的 cid 一起清掉）')
  eq(H.getState().failed?.clientMsgId, cidA, 'B3：成功后 A 的失败记录还在')
  eq(!!H.getState().failedByCid[cidA], true, 'B3：A 的 cid 仍在失败表里')

  // 用户直接把这句原文再发一次（不点重试、不传 cid）：必须复用 A 的首发 cid
  eq(await H.getState().send('__lostack__'), true, 'B3：重发 A 成功（命中 cid 幂等）')
  eq(wire.seen[wire.seen.length - 1].cid, cidA, 'B3：wire cid 仍是 A 的首发 cid')
  eq(!!H.getState().failedByCid[cidA], false, 'B3：成功后 A 的记录才被清掉')
  await H.getState().select('ag_mockwri2')
  eq(countText(H.getState().messages, '__lostack__'), base + 1, 'B3：账本 A 只有 1 次发送（没有换新 cid 多记一条）')
  wire.restore()
}

// —— 8) mock fixture：?hubFixture=needs|tasks（HUB-FIXTURE-01） ——
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

  // —— 8b) hub_need_resolve 与真实 bridge 对齐（P1 复审非阻塞）——
  // mock 原先任何 resolution 都成功，前端的 fail-closed 分支在演示里根本走不到。
  const pendingOf = async (id) => ((await call('hub_need_list', { status: 'pending' })).data?.needs ?? []).find((n) => n.id === id)
  const again = await call('hub_need_resolve', { need_id: 'need_mock_approval', resolution: 'deny' })
  eq(again.ok, true, '重复 resolve 幂等成功（不再报错）')
  eq(again.data?.already, true, '重复 resolve 返回 already:true')
  eq(again.data?.need?.status, 'resolved', '重复 resolve 不改状态（仍 resolved）')

  const badHandoff = await call('hub_need_resolve', { need_id: 'need_mock_handoff', resolution: 'acceptt' })
  eq(badHandoff.ok, false, '交接未知 resolution 失败（fail-closed）')
  assert(String(badHandoff.error ?? '').includes('未知的交接处理方式'),
    `交接错误信息与真实 bridge 同口径（got=${badHandoff.error}）`)
  eq((await pendingOf('need_mock_handoff'))?.status, 'pending', 'fail-closed 后交接仍是 pending（没被静默消费）')

  const noAnswers = await call('hub_need_resolve', { need_id: 'need_mock_question', resolution: 'answered' })
  eq(noAnswers.ok, false, '提问缺 answers 失败')
  assert(String(noAnswers.error ?? '').includes('缺少 answers'), `提问错误信息为「缺少 answers」（got=${noAnswers.error}）`)
  const emptyAnswers = await call('hub_need_resolve', {
    need_id: 'need_mock_question', resolution: 'answered', answers: {},
  })
  eq(emptyAnswers.ok, false, '提问 answers 为空对象失败')
  assert(String(emptyAnswers.error ?? '').includes('answers 不能为空'), `空 answers 错误信息（got=${emptyAnswers.error}）`)
  eq((await pendingOf('need_mock_question'))?.status, 'pending', '参数错误后提问仍是 pending')

  // 正常 payload（UI 提交的 {question_id: answer}）必须不受影响。
  const answered = await call('hub_need_resolve', {
    need_id: 'need_mock_question', resolution: 'answered', answers: { q1: '写「未公开」', q2: '中文' },
  })
  eq(answered.ok, true, '提问带 answers map 正常 resolve')
  eq(answered.data?.need?.status, 'resolved', '提问 resolve 后状态为 resolved')
  const accepted = await call('hub_need_resolve', { need_id: 'need_mock_handoff', resolution: 'accept' })
  eq(accepted.ok, true, '交接 accept 正常 resolve')
  eq((await pendingOf('need_mock_handoff')), undefined, '交接 accept 后从 pending 消失')
  delete globalThis.window
}

// —— 9) 乐观清空草稿：按下发送立刻清，失败才放回（HUB-CLEAR-01） ——
// 用户报障：消息已经乐观出现在时间线，输入框却要等 hub_send/hub_group_send 的响应回来
// 才清（真 bridge 慢时很明显）。mock 的响应只隔一个微任务，所以用 localStorage 把发送类
// 命令的响应人为推迟 300ms，才能断言「send() 返回前输入框已经空了」。
{
  // Node 25 自带一个内建的 localStorage，**读它会打印 --localstorage-file 警告**，而且它是
  // 访问器属性 —— 直接赋值只是把值塞进内建 setter，mock 侧无法把它和「真浏览器」区分开。
  // 所以用 defineProperty 换成数据属性桩（getItem 恒返回 '300'）：mock 只认这种被真正
  // 替换过的桩，浏览器里则读真 window.localStorage。测试结束按原描述符恢复。
  const lsDesc = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true, writable: true, value: { getItem: () => '300' },
  })
  await H.getState().select('ag_mockres1')
  const A = H.getState().activeId
  eq(A, 'ag_mockres1', '乐观清空：DM 打开')

  // 成功：不等 await 就空
  H.getState().setDraft(A, '待发内容')
  const t0 = Date.now()
  const p = H.getState().send('待发内容', undefined, undefined, '待发内容')
  eq(H.getState().drafts[A] ?? '', '', '乐观清空：send 同步清空草稿（不等响应）')
  eq(await p, true, '乐观清空：这次发送成功')
  // 证明 harness 真的生效：响应被推迟了 300ms，否则「同步清空」这条断言是空转的。
  assert(Date.now() - t0 >= 250, `乐观清空：mock 的发送响应确实被推迟（got=${Date.now() - t0}ms）`)
  eq(H.getState().drafts[A] ?? '', '', '乐观清空：成功后草稿仍为空')

  // 失败：同步清空 → 响应回来把原文放回输入框（用户改完能直接再发）
  H.getState().setDraft(A, '__fail__')
  const pf = H.getState().send('__fail__', undefined, undefined, '__fail__')
  eq(H.getState().drafts[A] ?? '', '', '乐观清空：失败场景同样同步清空')
  eq(await pf, false, '乐观清空：__fail__ 发送失败')
  eq(H.getState().drafts[A], '__fail__', '乐观清空：失败后原文回到输入框')

  // 等待期间用户输入新内容：失败也绝不覆盖
  H.getState().setDraft(A, '__fail__')
  const pw = H.getState().send('__fail__', undefined, undefined, '__fail__')
  H.getState().setDraft(A, '新内容')
  eq(await pw, false, '乐观清空：第二条 __fail__ 失败')
  eq(H.getState().drafts[A], '新内容', '乐观清空：等待期间的新输入不被失败恢复覆盖')

  // 成功也不能抹掉等待期间的新输入（这正是旧代码 setDraft('') 的缺陷）
  H.getState().setDraft(A, '第二条成功')
  const ps = H.getState().send('第二条成功', undefined, undefined, '第二条成功')
  H.getState().setDraft(A, '用户新打的字')
  eq(await ps, true, '乐观清空：第二条成功')
  eq(H.getState().drafts[A], '用户新打的字', '乐观清空：成功后不抹掉等待期间的新输入')

  // 群：同一套语义（sendGroup 的第 6 个参数是草稿）
  await H.getState().selectGroup('grp_mock1')
  const G = H.getState().activeGroupId
  eq(G, 'grp_mock1', '乐观清空：群打开')

  H.getState().setGroupDraft(G, '群里的待发内容')
  const pg = H.getState().sendGroup('群里的待发内容', [], false, undefined, undefined, '群里的待发内容')
  eq(H.getState().groupDraftText[G] ?? '', '', '乐观清空：群发送同步清空草稿')
  eq(await pg, true, '乐观清空：群发送成功')
  eq(H.getState().groupDraftText[G] ?? '', '', '乐观清空：群发送成功后草稿仍为空')

  H.getState().setGroupDraft(G, '__fail__')
  const pgf = H.getState().sendGroup('__fail__', [], false, undefined, undefined, '__fail__')
  eq(H.getState().groupDraftText[G] ?? '', '', '乐观清空：群失败场景同样同步清空')
  eq(await pgf, false, '乐观清空：群 __fail__ 失败')
  eq(H.getState().groupDraftText[G], '__fail__', '乐观清空：群失败后原文回到草稿')

  H.getState().setGroupDraft(G, '__fail__')
  const pgw = H.getState().sendGroup('__fail__', [], false, undefined, undefined, '__fail__')
  H.getState().setGroupDraft(G, '群里新打的字')
  eq(await pgw, false, '乐观清空：群第二条 __fail__ 失败')
  eq(H.getState().groupDraftText[G], '群里新打的字', '乐观清空：群里等待期间的新输入不被覆盖')

  if (lsDesc) Object.defineProperty(globalThis, 'localStorage', lsDesc)
  else delete globalThis.localStorage
}

off()
console.log(`\n✅ Hub 失败重试幂等 / fixture：${pass} 项全部通过`)
