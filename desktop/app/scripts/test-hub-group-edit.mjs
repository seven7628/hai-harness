#!/usr/bin/env node
// 群组编辑（P7-01/I3）：store.saveGroupEdit 只发**一条** hub_group_edit + mock 的原子语义。
//
// 为什么值得单独一组：编辑群组曾经是「update(name/owner/mode) + member_add + member_remove」
// 多条命令的组合 —— 任一步失败会留下半改状态（owner 已换、成员删了一半）。P7-01 把它换成
// 一条原子命令，这个脚本钉住三件事：
//   1. wire 上只有一条 hub_group_edit（不再有 member_add/remove 序列，也不再有旧 update 形状）；
//   2. 前端预校验失败时一条命令都不发（错误文案直接给原因）；
//   3. mock 的 hub_group_edit 先校验后写：失败时 group 逐字段不变，成功时一次改全。
//
// 覆盖：
//   · openGroupEditor/closeGroupEditor（右栏 inspector 与左栏 ＋ 共用）
//   · 成功：改名 + 改模式 + 加人 + 移人 + 转让群主，全部在**一条**命令里
//   · 拒绝：移除 owner / 重名 / 重复添加 / 移除非成员 / 超过 12 人（且一条命令都不发）
//   · mock：hub_group_edit 的原子性（失败 = 群不变）、owner 转让、12 人上限
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
    sourcefile: 'hub-group-edit-entry.ts',
    loader: 'ts',
  },
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
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

/** 直接发一条命令并等响应。 */
const call = (type, payload = {}) => new Promise((resolve) => {
  const id = 920_000 + Math.floor(Math.random() * 1000)
  const offEv = t.onEvent((line) => {
    let ev
    try { ev = JSON.parse(line) } catch { return }
    if (ev.event_type !== 'command_response' || ev.id !== id) return
    offEv()
    resolve(ev)
  })
  void t.send({ id, type, payload })
})

/** 记录 wire 上的命令。 */
function spyWire() {
  const seen = []
  const orig = t.send.bind(t)
  t.send = (cmd) => {
    seen.push({ type: String(cmd?.type ?? ''), payload: cmd?.payload ?? {} })
    return orig(cmd)
  }
  return { seen, restore: () => { t.send = orig } }
}

// 会改群的命令（列表/读取类不在断言范围内：保存成功后本来就要刷新列表）。
const MUTATING = new Set([
  'hub_group_create', 'hub_group_update', 'hub_group_delete',
  'hub_group_member_add', 'hub_group_member_remove', 'hub_group_edit',
])
const mutations = (seen) => seen.filter((x) => MUTATING.has(x.type))

async function bootstrap() {
  H.setState({ open: true, error: null })
  await H.getState().refresh()
  await H.getState().loadGroups()
}

await bootstrap()
const groupOf = (id) => H.getState().groups.find((g) => g.id === id)
const rolesOf = (g) => Object.fromEntries((g?.members ?? []).map((m) => [m.agent_id, m.role]))
/** 从 mock 现读群（不经过 store 缓存）：原子性反证要看服务端的真实状态。 */
async function liveGroup(id) {
  const r = await call('hub_group_get', { group_id: id })
  if (!r.ok) throw new Error(`hub_group_get 失败: ${r.error}`)
  return r.data?.group
}

// —— 1) 弹层开关：store 持有（左栏 ＋ 与右栏「编辑群组」共用） ——
{
  eq(H.getState().groupEditor.open, false, '初始不打开')
  H.getState().openGroupEditor('grp_mock1')
  eq(H.getState().groupEditor.open, true, 'openGroupEditor 打开')
  eq(H.getState().groupEditor.groupId, 'grp_mock1', '编辑目标 = grp_mock1')
  H.getState().closeGroupEditor()
  eq(H.getState().groupEditor.open, false, 'closeGroupEditor 关闭')
  H.getState().openGroupEditor()
  eq(H.getState().groupEditor.groupId, null, '不带 id = 新建模式')
  H.getState().closeGroupEditor()
}

// —— 2) 成功：改名 + 模式 + 加人 + 移人 + 转让群主，一条命令 ——
{
  // 造 2 个新智能体（mock 的 hub_agent_create）。
  const made = []
  for (const name of ['编辑甲', '编辑乙']) {
    const r = await call('hub_agent_create', { profile: { name, provider: 'deepseek', model: 'deepseek-v4-flash', role: '测试' } })
    eq(r.ok, true, `创建智能体 ${name}`)
    made.push(String(r.data?.agent?.id ?? ''))
  }
  await H.getState().refresh()
  await H.getState().loadGroups()

  const spy = spyWire()
  const saved = await H.getState().saveGroupEdit({
    group_id: 'grp_mock1',
    name: '竞品研究（编辑后）',
    owner_agent_id: 'ag_mockwri2', // 原 member → 升 owner（转让与增删同一条命令）
    mode: 'readonly',
    add: [made[0], made[1]],
    remove: ['ag_mockres1'], // 原 owner：同一条命令里转让 + 移除
  })
  spy.restore()

  assert(!!saved, `保存成功返回刷新后的 group（error=${JSON.stringify(H.getState().error)}）`)
  eq(saved.name, '竞品研究（编辑后）', '群名已保存')
  eq(saved.owner_agent_id, 'ag_mockwri2', '群主已转让')
  eq(saved.policy?.mode, 'readonly', '协作模式已保存')
  const roles = rolesOf(saved)
  eq(roles['ag_mockres1'], undefined, '旧 owner 已被移除')
  eq(roles['ag_mockwri2'], 'owner', '新 owner 角色 = owner')
  eq(roles[made[0]], 'member', '新成员甲入群（member）')
  eq(roles[made[1]], 'member', '新成员乙入群（member）')
  eq(H.getState().error, null, '成功不写 error')

  // 核心断言：wire 上只有一条 hub_group_edit —— 没有 update/member_add/member_remove 序列。
  const sent = mutations(spy.seen)
  eq(sent.length, 1, `只发一条改群命令（got=${JSON.stringify(spy.seen.map((x) => x.type))}）`)
  eq(sent[0].type, 'hub_group_edit', '命令名 = hub_group_edit')
  const payload = sent[0].payload
  eq(payload.group_id, 'grp_mock1', 'payload.group_id')
  eq(payload.name, '竞品研究（编辑后）', 'payload.name')
  eq(payload.mode, 'readonly', 'payload.mode')
  eq(payload.owner, 'ag_mockwri2', 'payload.owner（不是旧命令的 patch.owner_agent_id）')
  eq(JSON.stringify(payload.add_members), JSON.stringify([made[0], made[1]]), 'payload.add_members')
  eq(JSON.stringify(payload.remove_members), JSON.stringify(['ag_mockres1']), 'payload.remove_members')
  assert(!('patch' in payload), '不再发旧 update 的 patch 形状')
  eq(spy.seen.filter((x) => x.type === 'hub_group_member_add' || x.type === 'hub_group_member_remove').length, 0,
    '不再有 member_add/remove 序列')
}

// —— 3) 拒绝路径：前端预校验，一条命令都不发 ——
{
  const before = groupOf('grp_mock1')
  const ownerID = String(before.owner_agent_id)
  const cases = [
    { name: '移除群主被拒', req: { remove: [ownerID] }, err: '先转让' },
    { name: '重复添加被拒', req: { add: [ownerID] }, err: '已在群里' },
    { name: '移除非成员被拒', req: { remove: ['ag_does_not_exist'] }, err: '不在群里' },
    { name: '重名被拒', req: { name: '场景演示 · 六项能力' }, err: '同名' },
    { name: '空名被拒', req: { name: '   ' }, err: '群名称不能为空' },
  ]
  for (const c of cases) {
    const spy = spyWire()
    const out = await H.getState().saveGroupEdit({
      group_id: 'grp_mock1', name: '竞品研究（编辑后）', mode: 'ask', add: [], remove: [], ...c.req,
    })
    spy.restore()
    eq(out, null, `${c.name}：返回 null`)
    assert(String(H.getState().error ?? '').includes(c.err), `${c.name}：错误含「${c.err}」（got=${JSON.stringify(H.getState().error)}）`)
    eq(mutations(spy.seen).length, 0, `${c.name}：不发任何改群命令`)
  }
}

// —— 4) 12 人上限：前端就挡住；12 人换人仍是一条命令 ——
{
  // 把群填到 12 人（当前 3 人：wri2 + 甲乙）。
  const ids = []
  for (let i = 0; i < 10; i++) {
    const r = await call('hub_agent_create', { profile: { name: `批量${i}`, provider: 'deepseek', model: 'deepseek-v4-flash' } })
    ids.push(String(r.data?.agent?.id ?? ''))
  }
  await H.getState().refresh()
  const spyAdd = spyWire()
  const first = await H.getState().saveGroupEdit({ group_id: 'grp_mock1', name: '竞品研究（编辑后）', add: ids.slice(0, 9), mode: 'ask' })
  spyAdd.restore()
  assert(!!first, '先加到 12 人')
  eq((first.members ?? []).length, 12, '群成员 = 12')
  eq(mutations(spyAdd.seen).length, 1, '加 9 人也是一条命令')

  const spy = spyWire()
  const out = await H.getState().saveGroupEdit({ group_id: 'grp_mock1', name: '竞品研究（编辑后）', add: [ids[9]], mode: 'ask' })
  spy.restore()
  eq(out, null, '超过 12 人被拒')
  assert(String(H.getState().error ?? '').includes('12'), `错误提到上限 12（got=${JSON.stringify(H.getState().error)}）`)
  eq(mutations(spy.seen).length, 0, '超限不发命令')

  // 换人（先移除一个再加一个）仍然可以：净数不变，一条命令完成。
  const spySwap = spyWire()
  const swapped = await H.getState().saveGroupEdit({
    group_id: 'grp_mock1', name: '竞品研究（编辑后）', mode: 'ask',
    remove: [ids[0]], add: [ids[9]],
  })
  spySwap.restore()
  assert(!!swapped, '12 人时换人成功（同一条命令里增删）')
  eq((swapped.members ?? []).length, 12, '换人后仍是 12 人')
  eq(mutations(spySwap.seen).length, 1, '换人也是一条命令')
  const swapPayload = mutations(spySwap.seen)[0].payload
  eq(JSON.stringify(swapPayload.remove_members), JSON.stringify([ids[0]]), '换人：remove_members')
  eq(JSON.stringify(swapPayload.add_members), JSON.stringify([ids[9]]), '换人：add_members')
}

// —— 5) mock 侧 hub_group_edit 的原子性（直接发命令，绕过 store 校验） ——
{
  const gid = 'grp_mock1'
  const snapshot = (g) => JSON.stringify({
    name: g?.name,
    owner: g?.owner_agent_id,
    members: (g?.members ?? []).map((m) => [m.agent_id, m.role]),
    mode: g?.policy?.mode,
  })
  const before = await liveGroup(gid)
  const ownerID = String(before.owner_agent_id)
  const memberID = String((before.members ?? []).find((m) => m.agent_id !== ownerID)?.agent_id ?? '')

  const failures = [
    { name: 'add 不存在的 agent', payload: { add_members: ['ag_nope'] }, err: '智能体不存在' },
    { name: '移除 owner', payload: { remove_members: [ownerID] }, err: '先转让' },
    { name: '重复添加已在群的人', payload: { add_members: [memberID] }, err: '已在群里' },
    { name: '移除不在群的人', payload: { remove_members: ['ag_nope'] }, err: '不在群里' },
    { name: '同时加入和移除', payload: { add_members: ['ag_nope'], remove_members: ['ag_nope'] }, err: '不能同时加入和移除' },
    { name: 'owner 不是成员', payload: { owner: 'ag_nope' }, err: '群主必须是群成员' },
    { name: '非法模式', payload: { mode: 'yolo' }, err: 'ask/auto/readonly' },
    { name: '改名冲突', payload: { name: '场景演示 · 六项能力' }, err: '同名' },
  ]
  for (const f of failures) {
    const r = await call('hub_group_edit', { group_id: gid, ...f.payload })
    eq(r.ok, false, `mock：${f.name} 被拒`)
    assert(String(r.error ?? '').includes(f.err), `mock：${f.name} 错误含「${f.err}」（got=${JSON.stringify(r.error)}）`)
    assert(snapshot(await liveGroup(gid)) === snapshot(before), `mock：${f.name} 失败后群逐字段不变`)
  }

  // 成功：一条命令同时改名/模式/群主/增删（新群主来自已有成员，新增一个新人）。
  const made = await call('hub_agent_create', { profile: { name: '原子新人', provider: 'deepseek', model: 'deepseek-v4-flash' } })
  eq(made.ok, true, 'mock：创建原子新人')
  const newID = String(made.data?.agent?.id ?? '')
  const r = await call('hub_group_edit', {
    group_id: gid,
    name: '竞品研究（原子改）',
    mode: 'auto',
    owner: memberID,
    add_members: [newID],
    remove_members: [ownerID],
  })
  eq(r.ok, true, `mock：一条命令改全字段（error=${JSON.stringify(r.error)}）`)
  const after = await liveGroup(gid)
  eq(after.name, '竞品研究（原子改）', 'mock：群名已改')
  eq(after.policy?.mode, 'auto', 'mock：模式已改')
  eq(after.owner_agent_id, memberID, 'mock：群主已转让')
  const roles = rolesOf(after)
  eq(roles[memberID], 'owner', 'mock：新群主角色 = owner')
  eq(roles[ownerID], undefined, 'mock：旧群主已移除')
  eq(roles[newID], 'member', 'mock：新成员入群（member）')
  const membersAfter = after.members ?? []
  eq(membersAfter.length, (before.members ?? []).length, 'mock：成员数不变（移除 1 人、加入 1 人）')
  eq(membersAfter.filter((m) => m.role === 'owner').length, 1, 'mock：恰好一个 owner')
  assert(membersAfter.filter((m) => m.agent_id !== memberID).every((m) => m.role === 'member'),
    'mock：其余成员角色不变（只有 owner 成对改写）')
}

off()
console.log(`\n✅ 群组编辑（hub_group_edit）：${pass} 项全部通过`)
