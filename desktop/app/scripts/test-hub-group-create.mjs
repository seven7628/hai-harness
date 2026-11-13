#!/usr/bin/env node
// 新建群组（hub_group_create）：store 动作 + mock 的端到端契约（Node 直跑，无需浏览器）。
//
// 为什么值得单独一组：左栏「群组」页签原先只有空态文案，没有任何创建入口；bridge 早就有
// hub_group_create，但前端 store 从没调用、mock 也没实现 —— 于是「能建群」这件事在
// 浏览器里点不出来，回归脚本也钉不住。这里把三件事一次钉死：
//   1. store.createGroup 的编排（校验 → 命令 → 刷新列表 → 打开新群）
//   2. wire 上的 payload（name / member_ids / owner_agent_id / policy 一个不差）
//   3. mock 的新群语义（空历史、不灌别人的种子、不影响既有 demo 群）
//
// 覆盖：
//   · 成功：返回 group、hub_group_list 里出现、activeGroupId 指向新群、Hub 已打开、payload 正确
//   · 失败：空 name / 空 members 不发命令且 error 非空；重名 / 未知成员由 mock 拒绝并透出原因
//   · 新群打开：selectGroup 不报错，history 为空，hub_group_get 正常返回
//   · 既有 grp_mock1 / grp_demo 的顺序与内容不变（其它回归脚本靠「第一行」选群）
import { build } from 'esbuild'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const APP = join(__dirname, '..')
const hubDir = join(APP, 'src', 'hub')

const result = await build({
  stdin: {
    contents: [
      "export * as store from './useHubStore.ts'",
      "export * as transport from '../transport/index.ts'",
    ].join('\n'),
    resolveDir: hubDir,
    sourcefile: 'hub-group-create-entry.ts',
    loader: 'ts',
  },
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
  // 与 test-hub-idem.mjs 同一套：Vite 的 ?raw/?url/?no-inline 在 esbuild 里没有 loader。
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

/** 直接发一条命令并等响应（绕过 store，用来验 mock 侧的语义）。 */
const call = (type, payload = {}) => new Promise((resolve) => {
  const id = 910_000 + Math.floor(Math.random() * 1000)
  const offEv = t.onEvent((line) => {
    let ev
    try { ev = JSON.parse(line) } catch { return }
    if (ev.event_type !== 'command_response' || ev.id !== id) return
    offEv()
    resolve(ev)
  })
  void t.send({ id, type, payload })
})

/** 记录 wire 上的命令（payload 与类型）；createGroup 的入参契约只看这里。 */
function spyWire() {
  const seen = []
  const orig = t.send.bind(t)
  t.send = (cmd) => {
    seen.push({ type: String(cmd?.type ?? ''), payload: cmd?.payload ?? {} })
    return orig(cmd)
  }
  return { seen, restore: () => { t.send = orig } }
}

// —— 1) 成功：命令 → 列表 → 打开新群 ——
{
  H.setState({ open: false, error: null, groups: [], activeGroupId: null })
  const spy = spyWire()
  const group = await H.getState().createGroup({
    name: '  测试群  ',
    member_ids: ['ag_mockres1', 'ag_mockwri2'],
  })
  spy.restore()

  assert(!!group, 'createGroup 返回新群（不是 null）')
  eq(group.name, '测试群', 'name 去掉首尾空格')
  eq(group.id.startsWith('grp_mock_'), true, `新群 id 走 grp_mock_ 前缀（got=${group.id}）`)
  eq(group.owner_agent_id, 'ag_mockres1', 'owner 缺省取第一个成员')
  eq((group.members ?? []).map((m) => m.agent_id).join(','), 'ag_mockres1,ag_mockwri2', '成员顺序与入参一致')
  eq((group.members ?? [])[0]?.role, 'owner', '第一个成员角色是 owner')
  eq((group.members ?? [])[1]?.role, 'member', '其余成员角色是 member')
  eq(group.policy?.mode, 'ask', '未传 policy 时默认 ask')

  const creates = spy.seen.filter((x) => x.type === 'hub_group_create')
  eq(creates.length, 1, '只发了一次 hub_group_create')
  const p = creates[0].payload
  eq(p.name, '测试群', 'wire name 是 trim 后的值')
  eq((p.member_ids ?? []).join(','), 'ag_mockres1,ag_mockwri2', 'wire member_ids 保持勾选顺序')
  eq(p.owner_agent_id, 'ag_mockres1', 'wire owner_agent_id 缺省为第一个成员')
  eq(p.policy?.mode, 'ask', 'wire policy.mode = ask')

  const st = H.getState()
  assert(st.groups.some((g) => g.id === group.id), 'hub_group_list 里出现新群（左栏能渲染）')
  eq(st.activeGroupId, group.id, 'activeGroupId 指向新群')
  eq(st.open, true, 'Hub 已打开（从别处新建也落到群视图）')
  eq(st.error, null, '成功不写 error')

  // 群主显式指定：不在第一位时角色也要跟着走。
  const explicit = await H.getState().createGroup({
    name: '显式群主', member_ids: ['ag_mockres1', 'ag_mockwri2'], owner_agent_id: 'ag_mockwri2',
    policy: { mode: 'auto' },
  })
  assert(!!explicit, '显式群主：创建成功')
  eq(explicit.owner_agent_id, 'ag_mockwri2', '显式 owner 被保留')
  eq((explicit.members ?? []).find((m) => m.agent_id === 'ag_mockwri2')?.role, 'owner', 'owner 成员角色升为 owner')
  eq((explicit.members ?? []).find((m) => m.agent_id === 'ag_mockres1')?.role, 'member', '其余成员仍是 member')
  eq(explicit.policy?.mode, 'auto', '显式 policy.mode 生效')
}

// —— 2) 前端校验：空 name / 空 members 不发命令 ——
{
  const spy = spyWire()
  const before = H.getState().groups.length

  eq(await H.getState().createGroup({ name: '   ', member_ids: ['ag_mockres1'] }), null, '空 name 返回 null')
  assert(!!H.getState().error, `空 name 写了 error（got=${JSON.stringify(H.getState().error)}）`)

  eq(await H.getState().createGroup({ name: '没有成员', member_ids: [] }), null, '空 members 返回 null')
  assert(!!H.getState().error, `空 members 写了 error（got=${JSON.stringify(H.getState().error)}）`)

  // 全空白的 member_ids 等同空（trim 之后没有有效 id）。
  eq(await H.getState().createGroup({ name: '空白成员', member_ids: ['  '] }), null, '空白 member_ids 返回 null')

  spy.restore()
  eq(spy.seen.filter((x) => x.type === 'hub_group_create').length, 0, '前端校验拦下：一条 hub_group_create 都没发')
  eq(H.getState().groups.length, before, '校验失败不新增群')
}

// —— 3) 后端（mock）拒绝：重名 / 未知成员，错误透出且列表不变 ——
{
  const before = H.getState().groups.length
  eq(await H.getState().createGroup({ name: '测试群', member_ids: ['ag_mockres1'] }), null, '重名被拒绝')
  assert(String(H.getState().error ?? '').includes('同名'), `重名错误透出（got=${JSON.stringify(H.getState().error)}）`)

  eq(await H.getState().createGroup({ name: '幽灵群', member_ids: ['ag_nope'] }), null, '未知成员被拒绝')
  assert(String(H.getState().error ?? '').includes('不存在'), `未知成员错误透出（got=${JSON.stringify(H.getState().error)}）`)

  eq(await H.getState().createGroup({
    name: '群主不在成员里', member_ids: ['ag_mockres1'], owner_agent_id: 'ag_mockwri2',
  }), null, 'owner 不是成员时被拒绝')
  eq(H.getState().groups.length, before, '失败不新增群')
}

// —— 3b) 群模式：readonly 必须能选、能落库、能改（P6-06 的真实开关） ——
{
  // 新建时选 readonly：wire 上必须是 readonly（不是被前端悄悄降级成 ask/auto）。
  const spy = spyWire()
  const ro = await H.getState().createGroup({
    name: '只读群', member_ids: ['ag_mockres1'], policy: { mode: 'readonly' },
  })
  spy.restore()
  assert(!!ro, 'readonly 群创建成功')
  eq(ro.policy?.mode, 'readonly', 'readonly 群 policy.mode 落库')
  const createPayload = spy.seen.find((x) => x.type === 'hub_group_create')?.payload ?? {}
  eq(createPayload.policy?.mode, 'readonly', 'wire policy.mode = readonly')

  // 改成 auto（GroupView 头部菜单/GroupEditor 走同一条 hub_group_update）。
  const upd = await call('hub_group_update', { group_id: ro.id, patch: { policy: { mode: 'auto' } } })
  eq(upd.ok, true, 'hub_group_update 改模式成功')
  eq(upd.data?.group?.policy?.mode, 'auto', '改模式后响应里的 mode = auto')
  const after = await call('hub_group_get', { group_id: ro.id })
  eq(after.data?.group?.policy?.mode, 'auto', '重新读取仍是 auto（不是只改响应）')

  // 非法值必须被拒绝：mock 接受一个不存在的模式 = UI 可以显示一个不存在的开关。
  const bad = await call('hub_group_update', { group_id: ro.id, patch: { policy: { mode: 'turbo' } } })
  eq(bad.ok, false, '非法群模式被拒绝')
  assert(String(bad.error ?? '').includes('ask/auto/readonly'), `非法模式错误文案可读（got=${JSON.stringify(bad.error)}）`)
  const still = await call('hub_group_get', { group_id: ro.id })
  eq(still.data?.group?.policy?.mode, 'auto', '非法更新不改动已有模式')
}

// —— 4) 打开新群：空历史不报错（不能灌进「竞品研究」的种子） ——
{
  const group = H.getState().groups.find((g) => g.name === '测试群')
  assert(!!group, '测试群还在列表里')

  await H.getState().selectGroup(group.id)
  eq(H.getState().activeGroupId, group.id, 'selectGroup 选中新群')
  eq(H.getState().groupMessages.length, 0, '新群历史为空')
  eq(H.getState().error, null, '打开空历史群不报错')

  const got = await call('hub_group_get', { group_id: group.id })
  eq(got.ok, true, 'hub_group_get 新群不报错')
  eq((got.data?.group?.members ?? []).length, 2, 'hub_group_get 带回成员')
  eq(got.data?.group?.name, '测试群', 'hub_group_get 带回群名')

  const hist = await call('hub_group_history', { group_id: group.id, limit: 50 })
  eq(hist.ok, true, 'hub_group_history 新群不报错')
  eq((hist.data?.messages ?? []).length, 0, '新群历史为空（没有别人的种子消息）')

  const act = await call('hub_group_activity', { group_id: group.id })
  eq(act.ok, true, 'hub_group_activity 新群不报错')
}

// —— 5) 既有 demo 群不受影响：顺序/内容一个字节都不动 ——
{
  const list = await call('hub_group_list', {})
  const ids = (list.data?.groups ?? []).map((g) => g.id)
  eq(ids[0], 'grp_mock1', 'grp_mock1 仍在第一位（其它回归脚本靠第一行选群）')
  assert(ids.includes('grp_demo'), '场景演示群 grp_demo 仍在')
  assert(ids.length >= 4, `新群也出现在 hub_group_list（got=${ids.join(',')}）`)

  const mock1 = await call('hub_group_get', { group_id: 'grp_mock1' })
  eq(mock1.ok, true, 'grp_mock1 打开正常')
  const seed = await call('hub_group_history', { group_id: 'grp_mock1', limit: 50 })
  eq((seed.data?.messages ?? []).length > 0, true, 'grp_mock1 仍有自己的种子历史')

  // progress 里也要有新群（左栏预览/未读投影按这个键取）。
  const progress = list.data?.progress ?? {}
  const created = H.getState().groups.filter((g) => g.name === '测试群' || g.name === '显式群主')
  for (const g of created) {
    assert(Object.prototype.hasOwnProperty.call(progress, g.id), `progress 覆盖新群 ${g.name}`)
  }
}

// —— 4) 已删的群策略旋钮（P6-13）：旧客户端传了必须被忽略，不是报错、也不是存住 ——
//
// max_parallel_runs（P6-02：并发 = 本轮目标数）与 max_turns_per_human（P6-13：群策略
// 限制取消）已经从 GroupPolicy 整个删掉。判据三件事：
//   · 前端类型里没有这两个字段（留一个没人读的字段 = 给 UI 一个假开关）；
//   · 旧 payload 带着它们 → 命令照常成功（不报错）；
//   · 它们不会出现在返回的 policy 里（不会「存住了但不生效」），同批合法字段照常生效。
{
  const types = readFileSync(join(APP, 'src', 'hub', 'types.ts'), 'utf8')
  const policyBlock = (types.match(/export interface HubGroupPolicy \{[\s\S]*?\n\}/) ?? [''])[0]
  assert(policyBlock.includes('max_handoff_depth') && policyBlock.includes('max_mentions_per_message'), 'HubGroupPolicy 仍有生效字段')
  assert(!/^\s*max_turns_per_human\?:/m.test(policyBlock), 'HubGroupPolicy 里没有已删的 max_turns_per_human 字段')
  assert(!/^\s*max_parallel_runs\?:/m.test(policyBlock), 'HubGroupPolicy 里没有已删的 max_parallel_runs 字段')

  const legacyPolicy = { mode: 'auto', max_turns_per_human: 8, max_parallel_runs: 3, max_mentions_per_message: 2 }
  const created = await call('hub_group_create', {
    name: '旧旋钮群', member_ids: ['ag_mockres1', 'ag_mockwri2'], policy: legacyPolicy,
  })
  eq(created.ok, true, '旧客户端带已删字段建群：不报错')
  const policy = created.data?.group?.policy ?? {}
  eq(policy.mode, 'auto', '同批合法字段（mode）照常生效')
  eq(policy.max_mentions_per_message, 2, '同批合法字段（max_mentions）照常生效')
  assert(!('max_turns_per_human' in policy), '已删的 max_turns_per_human 没有被存进群策略')
  assert(!('max_parallel_runs' in policy), '已删的 max_parallel_runs 没有被存进群策略')

  const patched = await call('hub_group_update', {
    group_id: String(created.data?.group?.id ?? ''),
    patch: { policy: { max_turns_per_human: 1, max_parallel_runs: 9, daily_budget_usd: 7 } },
  })
  eq(patched.ok, true, '旧客户端 patch 已删字段：不报错')
  const after = patched.data?.group?.policy ?? {}
  eq(after.daily_budget_usd, 7, '同批合法字段（日预算）照常生效')
  assert(!('max_turns_per_human' in after) && !('max_parallel_runs' in after), 'patch 也没把已删字段写回群策略')
}

off()
console.log(`\n✅ Hub 新建群组：${pass} 项全部通过`)
