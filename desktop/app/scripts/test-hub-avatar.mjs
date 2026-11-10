#!/usr/bin/env node
// Hub 头像 sigil v2 的纯函数验收（Node 直测，不启浏览器）：
//   身份确定性 / 500 seeds 覆盖 8 形状 8 色相 4 trait / role 只改 trait /
//   无 id 兜底 / 四态优先级 / Group 2–6 叠放规则 / 24·32·48 尺寸常量。
// 视觉与算法源：docs/demo/avatar-v2.html + 03-frontend-design.md §3.3。
import { build } from 'esbuild'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const entry = join(__dirname, '..', 'src', 'hub', 'avatar.ts')
const result = await build({
  entryPoints: [entry],
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
})
const source = result.outputFiles[0].text
const mod = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

// 1) 确定性：同一 (id, resp) 连算三次完全一致；换 location/写法不变。
{
  const a = mod.deriveSigil('agt_01H9ZQ4K7M2X3Y6P8R5T0V1W2C', '企业版定价、折扣政策、SLA 对比')
  const b = mod.deriveSigil('agt_01H9ZQ4K7M2X3Y6P8R5T0V1W2C', '  企业版定价、折扣政策、SLA 对比  ')
  const c = mod.deriveSigil('agt_01H9ZQ4K7M2X3Y6P8R5T0V1W2C', '企业版定价、折扣政策、SLA 对比')
  assert(JSON.stringify(a) === JSON.stringify(b) && JSON.stringify(b) === JSON.stringify(c),
    '同一 (id, resp) 三次推导字节相同（含首尾空白）')
  assert(a.shapeId === mod.SHAPES[a.shape].id && a.hueId === mod.HUES[a.hue].id && a.traitId === mod.TRAITS[a.trait].id,
    'shape/hue/trait 索引与定义表对齐')
}

// 2) 覆盖：500 个合成 id 覆盖全部 8 形状 / 8 色相 / 4 trait。
{
  const shapes = new Set()
  const hues = new Set()
  const traits = new Set()
  for (let i = 0; i < 500; i++) {
    const d = mod.deriveSigil(`agt_seed_${i}`, `职责 ${i % 7}`)
    shapes.add(d.shape)
    hues.add(d.hue)
    traits.add(d.trait)
  }
  assert(shapes.size === 8, `500 seeds 覆盖 8 形状（实际 ${shapes.size}）`)
  assert(hues.size === 8, `500 seeds 覆盖 8 色相（实际 ${hues.size}）`)
  assert(traits.size === 4, `500 seeds 覆盖 4 内侧标记（实际 ${traits.size}）`)
  assert(mod.SHAPES.length === 8 && mod.HUES.length === 8 && mod.TRAITS.length === 4,
    '定义表规模 = 8 / 8 / 4')
}

// 3) role 只改 trait：shape/hue 不变，trait 至少有一个 seed 真的变了。
{
  let traitChanged = 0
  let identityStable = true
  for (let i = 0; i < 50; i++) {
    const id = `agt_role_${i}`
    const a = mod.deriveSigil(id, '研究员')
    const b = mod.deriveSigil(id, '把结论写成可读的稿子')
    if (a.shape !== b.shape || a.hue !== b.hue) identityStable = false
    if (a.trait !== b.trait) traitChanged++
  }
  assert(identityStable, 'role 不影响 shape/hue（50 个 id 全一致）')
  assert(traitChanged > 0, `role 会改内侧 trait（50 个 id 中 ${traitChanged} 个变化）`)
}

// 4) 无 id 兜底：anon|norm(resp)，稳定且不是空头像。
{
  const a = mod.deriveSigil('', '临时智能体')
  const b = mod.deriveSigil(null, '临时智能体')
  const c = mod.deriveSigil(undefined, '  临时智能体  ')
  assert(a.seedId === 'anon|临时智能体' && a.shape === b.shape && b.shape === c.shape && a.hue === c.hue,
    '无 id 时退化为 anon|norm(resp)，同一职责稳定同图')
  assert(Number.isInteger(a.shape) && a.shape >= 0 && a.shape < 8, '无 id 也渲染出合法形状')
  assert(mod.normalizeResponsibility('  ＡＢＣ\tＤＥＦ  ') === 'abc def',
    'norm：NFKC 全角转半角 + 小写 + 折叠空白')
  assert(mod.normalizeResponsibility('x'.repeat(400)).length === 256, 'norm 截断 256 字符')
}

// 5) 四态优先级：needs_attention > offline > working > idle。
{
  const r = mod.resolveAgentState
  assert(r({ pending: true }) === 'needs_attention', 'pending → needs_attention')
  assert(r({ deleted: true }) === 'offline', 'deleted → offline')
  assert(r({ running: true }) === 'working', 'running → working')
  assert(r({}) === 'idle', '都没有 → idle')
  assert(r({ pending: true, deleted: true, running: true }) === 'needs_attention', '审批压过离线与运行')
  assert(r({ deleted: true, running: true }) === 'offline', '离线压过运行')
  assert(r({ running: true, pending: false }) === 'working', '显式 false 不误判')
  assert(mod.stateRank('needs_attention') > mod.stateRank('offline') &&
    mod.stateRank('offline') > mod.stateRank('working') &&
    mod.stateRank('working') > mod.stateRank('idle'), '优先级表 needs > offline > working > idle')
  assert(mod.highestState(['idle', 'working', 'offline']) === 'offline', 'highestState 取最高优先级')
}

// 6) Group 叠放 2–6：2–4 全显示；≥5 前 3 + N；只有 lead 画状态层。
{
  const members = (n) => Array.from({ length: n }, (_, i) => ({
    id: `agt_m${i}`,
    responsibility: `职责 ${i}`,
    state: ['idle', 'working', 'needs_attention', 'offline'][i % 4],
  }))
  for (const n of [2, 3, 4]) {
    const p = mod.planStack(members(n))
    assert(p.members.length === n && p.more === 0, `${n} 个成员全部显示，无 +N`)
  }
  for (const n of [5, 6]) {
    const p = mod.planStack(members(n))
    assert(p.members.length === 3 && p.more === n - 3, `${n} 个成员：前 3 + ${n - 3}`)
  }
  {
    const p = mod.planStack(members(6))
    assert(p.members.filter((m) => m.withStatus).length === 1 && p.members[0].withStatus,
      '状态层只在最左 lead 画一次')
    assert(p.members.slice(1).every((m) => m.state === 'idle' || !m.withStatus),
      '其余成员不画状态层')
    assert(p.state === 'needs_attention', '组状态取成员最高优先级（含 needs_attention）')
    assert(p.members[0].state === p.state, 'lead 承担组状态')
    assert(p.counts.total === 6 && p.counts.needs_attention >= 1, '状态摘要按全部成员统计')
  }
  {
    const p = mod.planStack(members(4), { groupRunning: true })
    assert(p.state === 'needs_attention', '群在跑但成员有待审批 → 仍是 needs_attention')
    const idleOnly = mod.planStack([{ id: 'a', state: 'idle' }, { id: 'b', state: 'idle' }], { groupRunning: true })
    assert(idleOnly.state === 'working' && idleOnly.members[0].state === 'working',
      '群会话在跑且无人更高优先级 → lead=working')
  }
}

// 7) 尺寸常量：只允许 24/32/48（26/34/44/56 这类旧值不再出现）。
{
  assert(JSON.stringify(mod.SIGIL_SIZES) === '[24,32,48]', 'SIGIL_SIZES = 24/32/48')
  assert(mod.SIGIL_VIEWBOX === 48, '视图框 48（环半径 22，外接 22.75 < 24）')
}


// 8) store 数据 → 状态：running / needs.pending / deleted 三张表合成四态。
{
  const stateOf = mod.stateResolver({
    running: { ag_run: true, ag_both: true },
    pending: new Set(['ag_attn', 'ag_both']),
  })
  assert(stateOf('ag_run') === 'working', 'stateResolver：running → working')
  assert(stateOf('ag_attn') === 'needs_attention', 'stateResolver：needs.pending → needs_attention')
  assert(stateOf('ag_both', true) === 'needs_attention', 'stateResolver：pending 压过 deleted/running')
  assert(stateOf('ag_gone', true) === 'offline', 'stateResolver：deleted → offline')
  assert(stateOf('ag_none') === 'idle', 'stateResolver：都没有 → idle')
}

// 9) 叠放 aria 文案：成员数 + 状态摘要；组会话在跑时摘要与 lead 同源。
{
  const labelsEntry = join(__dirname, '..', 'src', 'hub', 'avatarLabels.ts')
  const labelsBuild = await build({
    entryPoints: [labelsEntry], bundle: true, platform: 'node', format: 'esm', write: false, logLevel: 'silent',
  })
  const labelsSource = labelsBuild.outputFiles[0].text
  const labels = await import(`data:text/javascript;base64,${Buffer.from(labelsSource).toString('base64')}`)
  const dict = {
    'hub.avatar.label': '{name}，{state}',
    'hub.avatar.state.idle': '待命',
    'hub.avatar.state.working': '工作中',
    'hub.avatar.state.needs_attention': '需要你',
    'hub.avatar.state.offline': '离线',
    'hub.avatar.stack.label': '{n} 个智能体，{summary}',
    'hub.avatar.stack.sep': '，',
    'hub.avatar.stack.summary.working': '{n} 个工作中',
    'hub.avatar.stack.summary.needs_attention': '{n} 个需要你',
    'hub.avatar.stack.summary.offline': '{n} 个离线',
    'hub.avatar.stack.summary.idle': '全部待命',
    'hub.avatar.stack.summary.groupRunning': '群会话进行中',
  }
  const t = (k) => dict[k] ?? k
  assert(labels.agentSigilLabel(t, 'Researcher', 'working') === 'Researcher，工作中',
    '独立头像 label：名字 + 状态词')
  const counts = mod.countStates(['working', 'working', 'offline', 'idle'])
  assert(labels.agentStackLabel(t, 5, counts) === '5 个智能体，2 个工作中，1 个离线',
    '叠放 label：成员数 + 非零状态摘要')
  assert(labels.agentStackLabel(t, 2, mod.countStates(['idle', 'idle'])) === '2 个智能体，全部待命',
    '叠放 label：全待命不输出 0 个工作中')
  assert(labels.agentStackLabel(t, 2, mod.countStates(['idle', 'idle']), 'working') === '2 个智能体，群会话进行中',
    '叠放 label：群会话在跑时摘要与 lead 的 working 一致')
}

console.log(`\n✅ hub avatar 纯逻辑：${pass} 项全部通过`)
