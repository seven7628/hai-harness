#!/usr/bin/env node
// 群组编辑（P6-17）：store.saveGroupEdit 的编排 + mock 的成员增删/群主转让语义。
//
// 为什么值得单独一组：编辑群组不是一个命令，而是「update(name/owner/mode) + add + remove」
// 三步的组合，顺序错了会被 bridge 的校验挡下（移除 owner、先加后删撞 12 人上限）。
// 这里把顺序、校验、mock 的同一套语义一次钉死（Node 直跑，无需浏览器）。
//
// 覆盖：
//   · openGroupEditor/closeGroupEditor（右栏 inspector 与左栏 ＋ 共用）
//   · 成功：改名 + 改模式 + 加人 + 移人 + 转让群主（wire 顺序：先转让 → 再移除 → 再加）
//   · 拒绝：移除 owner / 重名 / 重复添加 / 移除非成员 / 超过 12 人（且一条命令都不发）
//   · mock：hub_group_member_add/remove 的存在性、owner 不可移除、12 人上限、observer 拒绝
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

/** 记录 wire 上的命令（顺序很重要：先转让 → 再移除 → 再加）。 */
function spyWire() {
  const seen = []
  const orig = t.send.bind(t)
  t.send = (cmd) => {
    seen.push({ type: String(cmd?.type ?? ''), payload: cmd?.payload ?? {} })
    return orig(cmd)
  }
  return { seen, restore: () => { t.send = orig } }
}

async function bootstrap() {
  H.setState({ open: true, error: null })
  await H.getState().refresh()
  await H.getState().loadGroups()
}

await bootstrap()
const groupOf = (id) => H.getState().groups.find((g) => g.id === id)
const rolesOf = (g) => Object.fromEntries((g?.members ?? []).map((m) => [m.agent_id, m.role]))

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

// —— 2) 成功：改名 + 模式 + 加人 + 移人 + 转让群主 ——
{
  // 造 3 个新智能体（mock 的 hub_agent_create）。
  const made = []
  for (const name of ['编辑甲', '编辑乙', '编辑丙']) {
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
    owner_agent_id: 'ag_mockwri2', // 原 member → 升 owner（已在群里：必须先转让再移除）
    mode: 'readonly',
    add: [made[0], made[1]],
    remove: ['ag_mockres1'], // 原 owner：转让之后才允许移除
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

  const order = spy.seen.map((x) => x.type).filter((x) => x.startsWith('hub_group_'))
  eq(order[0], 'hub_group_update', '先发 update（转让群主，否则移除旧 owner 会被拒）')
  assert(order.includes('hub_group_member_remove') && order.includes('hub_group_member_add'), '包含增删命令')
  assert(order.indexOf('hub_group_member_remove') < order.indexOf('hub_group_member_add'), '先移除再加（先腾名额，避免 12 人上限）')
  const upd = spy.seen.find((x) => x.type === 'hub_group_update')
  eq(upd.payload?.patch?.owner_agent_id, 'ag_mockwri2', 'update 带 owner_agent_id')
  eq(upd.payload?.patch?.policy?.mode, 'readonly', 'update 带 policy.mode')
}

// —— 3) 拒绝路径：一条命令都不发 ——
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
    eq(spy.seen.filter((x) => x.type.startsWith('hub_group_')).length, 0, `${c.name}：不发任何 hub_group_* 命令`)
  }
}

// —— 4) 12 人上限：前端就挡住 ——
{
  // 把群填到 12 人（当前 3 人：wri2 + 甲乙）。
  const ids = []
  for (let i = 0; i < 10; i++) {
    const r = await call('hub_agent_create', { profile: { name: `批量${i}`, provider: 'deepseek', model: 'deepseek-v4-flash' } })
    ids.push(String(r.data?.agent?.id ?? ''))
  }
  await H.getState().refresh()
  const first = await H.getState().saveGroupEdit({ group_id: 'grp_mock1', name: '竞品研究（编辑后）', add: ids.slice(0, 9), mode: 'ask' })
  assert(!!first, '先加到 12 人')
  eq((first.members ?? []).length, 12, '群成员 = 12')

  const spy = spyWire()
  const out = await H.getState().saveGroupEdit({ group_id: 'grp_mock1', name: '竞品研究（编辑后）', add: [ids[9]], mode: 'ask' })
  spy.restore()
  eq(out, null, '超过 12 人被拒')
  assert(String(H.getState().error ?? '').includes('12'), `错误提到上限 12（got=${JSON.stringify(H.getState().error)}）`)
  eq(spy.seen.filter((x) => x.type.startsWith('hub_group_')).length, 0, '超限不发命令')

  // 换人（先移除一个再加一个）仍然可以：净数不变。
  const swapped = await H.getState().saveGroupEdit({
    group_id: 'grp_mock1', name: '竞品研究（编辑后）', mode: 'ask',
    remove: [ids[0]], add: [ids[9]],
  })
  assert(!!swapped, '12 人时换人成功（先移除再添加）')
  eq((swapped.members ?? []).length, 12, '换人后仍是 12 人')
}

// —— 5) mock 侧的成员命令语义（直接发命令，绕过 store 校验） ——
{
  const gid = 'grp_mock1'
  const g = groupOf(gid)
  const ownerID = String(g.owner_agent_id)
  const removeOwner = await call('hub_group_member_remove', { group_id: gid, agent_id: ownerID })
  eq(removeOwner.ok, false, 'mock：owner 不可移除')
  assert(String(removeOwner.error ?? '').includes('转让'), 'mock：错误提示先转让')

  const dup = await call('hub_group_member_add', { group_id: gid, agent_id: ownerID })
  eq(dup.ok, false, 'mock：重复添加被拒')

  const observer = await call('hub_group_member_add', { group_id: gid, agent_id: 'ag_mockres1', role: 'observer' })
  eq(observer.ok, false, 'mock：observer 角色已删除')

  const missing = await call('hub_group_member_add', { group_id: gid, agent_id: 'ag_nope' })
  eq(missing.ok, false, 'mock：未知智能体被拒')

  const transfer = await call('hub_group_update', { group_id: gid, patch: { owner_agent_id: 'ag_not_member' } })
  eq(transfer.ok, false, 'mock：owner 必须是成员')
}

off()
console.log(`\n✅ 群组编辑：${pass} 项全部通过`)
