#!/usr/bin/env node
// Hub 头像 sigil v2 的纯函数验收（Node 直测，不启浏览器）：
//   身份确定性 / 500 seeds 覆盖 8 形状 8 色相 4 trait / role 只改 trait /
//   无 id 兜底 / 四态优先级 / Group 2–6 叠放规则 / 24·32·48 尺寸常量。
// 视觉与算法源：docs/demo/avatar-v2.html + 03-frontend-design.md §3.3。
import { build } from 'esbuild'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import { unlinkSync, writeFileSync } from 'node:fs'
import { createRequire } from 'node:module'

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

// 6) Group 叠放 2–6：2–4 全显示；≥5 前 4 + N（demo 口径）。
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
    assert(p.members.length === 4 && p.more === n - 4, `${n} 个成员：前 4 + ${n - 4}`)
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


// 10) 真实组件输出（SSR）：demo Luna DOM；独立头像带 label，叠放按 4 + N。
{
  const { createElement } = await import('react')
  const { renderToStaticMarkup } = await import('react-dom/server')
  const compBuild = await build({
    entryPoints: [join(__dirname, '..', 'src', 'hub', 'AgentStack.tsx')],
    bundle: true,
    platform: 'node',
    format: 'esm',
    jsx: 'automatic',
    external: ['react', 'react/jsx-runtime'],
    write: false,
    logLevel: 'silent',
  })
  // 写到 scripts/ 下再 import：data: URL 无法解析 react/jsx-runtime 这类裸模块名。
  const compPath = join(__dirname, '.tmp-agentstack.mjs')
  writeFileSync(compPath, compBuild.outputFiles[0].text)
  const { default: AgentStack } = await import(`${compPath}?v=${Date.now()}`)
  unlinkSync(compPath)
  const members = [
    { id: 'agt_a', responsibility: '研究', state: 'working' },
    { id: 'agt_b', responsibility: '写作', state: 'idle' },
    { id: 'agt_c', responsibility: '复核', state: 'idle' },
    { id: 'agt_d', responsibility: '归档', state: 'idle' },
    { id: 'agt_e', responsibility: '监控', state: 'idle' },
    { id: 'agt_f', responsibility: '统计', state: 'idle' },
  ]
  const html = renderToStaticMarkup(createElement(AgentStack, {
    members, size: 32, label: '竞品研究，6 个智能体，群会话进行中',
  }))
  assert(html.includes('role="img"') && html.includes('aria-label="竞品研究，6 个智能体，群会话进行中"'),
    '叠放容器输出 role=img + 含群名/成员数/摘要的 aria-label')
  assert((html.match(/data-luna-face/g) || []).length === 4, '≥5 时只显示前 4 个头像（demo 口径）')
  assert(html.includes('avatar-more') && html.includes('>+2</span>'), '≥5 时输出 +N 徽标')
  assert(!html.includes('class="ring') && !html.includes('class="trait'), '不再输出 sigil 的环/内侧标记')

  const sigilBuild = await build({
    entryPoints: [join(__dirname, '..', 'src', 'hub', 'AgentSigil.tsx')],
    bundle: true, platform: 'node', format: 'esm', jsx: 'automatic',
    external: ['react', 'react/jsx-runtime'], write: false, logLevel: 'silent',
  })
  const sigilPath = join(__dirname, '.tmp-agentsigil.mjs')
  writeFileSync(sigilPath, sigilBuild.outputFiles[0].text)
  const { default: AgentSigil } = await import(`${sigilPath}?v=${Date.now()}`)
  unlinkSync(sigilPath)
  const labelled = renderToStaticMarkup(createElement(AgentSigil, {
    agentId: 'agt_a', responsibility: '研究', state: 'working', size: 52, label: 'Researcher，工作中',
  }))
  const labelledTag = labelled.slice(0, labelled.indexOf('>') + 1)
  assert(labelledTag.includes('role="img"') && labelledTag.includes('aria-label="Researcher，工作中"') &&
    !labelledTag.includes('aria-hidden'), '独立头像 label → role=img + 状态词（不再 aria-hidden）')
  assert(labelled.includes('data-luna-face') && labelled.includes('data-fill="#3C82F6"'),
    'Luna SVG 由 demo 引擎填充（data-fill 来自职责色）')
  const decorative = renderToStaticMarkup(createElement(AgentSigil, {
    agentId: 'agt_a', responsibility: '研究', state: 'working', size: 52,
  }))
  const decorativeTag = decorative.slice(0, decorative.indexOf('>') + 1)
  assert(decorativeTag.includes('aria-hidden="true"') && !decorativeTag.includes('role="img"'),
    '与名字同行的头像仍 aria-hidden')

  // 严格 demo：accent 不参与头像。即使调用方还传 accent，data-fill 也必须仍是职责色。
  const accented = renderToStaticMarkup(createElement(AgentSigil, {
    agentId: 'agt_a', accent: '#7bc199', responsibility: '研究', state: 'working', size: 52,
  }))
  assert(accented.includes('data-fill="#3C82F6"'), 'SSR：传 accent=#7bc199 也不改头像（研究→#3C82F6）')
  assert(!accented.includes('data-fill="#7bc199"'), 'SSR：头像不得回读 profile.accent')
  assert(labelled.includes('data-fill="#3C82F6"'), 'SSR：不传 accent 仍按职责规则（研究→#3C82F6）')
}

// 10b) 身份色纯函数：严格 demo 只认职责规则/散列，函数签名里没有 accent。
{
  const lunaBuild = await build({
    entryPoints: [join(__dirname, '..', 'src', 'hub', 'lunaAvatar.ts')],
    bundle: true, platform: 'node', format: 'esm', write: false, logLevel: 'silent',
  })
  const lunaPath = join(__dirname, '.tmp-lunaavatar.mjs')
  writeFileSync(lunaPath, lunaBuild.outputFiles[0].text)
  const luna = await import(`${lunaPath}?v=${Date.now()}`)
  unlinkSync(lunaPath)
  const role = '竞品研究员'
  assert(luna.lunaColor('agt_mockres1', 'Researcher', role) === '#3C82F6',
    'lunaColor：研究→蓝（12 色 role palette）')
  assert(luna.lunaColor('agt_mockwri2', 'Writer', '写作') === '#885CF4',
    'lunaColor：写作→紫（12 色 role palette）')
  assert(luna.lunaColor('agt_other', 'Other', role) === '#3C82F6',
    'lunaColor：同一职责不同智能体同色（demo 的职责规则先于散列）')
  assert(luna.lunaColor('agt_x', '', '') === luna.lunaColor('agt_x', '', ''),
    'lunaColor：无职责时按 id 稳定散列')
  assert(luna.lunaColor('agt_x', '', '') !== luna.lunaColor('agt_y', '', ''),
    'lunaColor：不同 id 散列不同色')
}

// 11) 手机端 avatar_model.js 与桌面 avatar.ts 同 seed 对照（防两套算法漂移）。
{
  const require = createRequire(import.meta.url)
  const mobile = require(join(__dirname, '..', '..', 'bridge', 'mobile_web', 'avatar_model.js'))
  assert(JSON.stringify(mobile.SHAPES) === JSON.stringify(mod.SHAPES), '手机/桌面 8 形状路径逐字一致')
  assert(JSON.stringify(mobile.HUES) === JSON.stringify(mod.HUES), '手机/桌面 8 色相（dark/light）逐字一致')
  assert(JSON.stringify(mobile.TRAITS) === JSON.stringify(mod.TRAITS), '手机/桌面 4 内侧标记逐字一致')
  assert(JSON.stringify(mobile.SIGIL_SIZES) === JSON.stringify(mod.SIGIL_SIZES), '手机/桌面尺寸档位一致（24/32/48）')

  let mismatch = 0
  for (let i = 0; i < 300; i++) {
    const id = i % 11 === 0 ? '' : `agt_drift_${i}`
    const resp = `职责 ${i % 13}`
    const a = mod.deriveSigil(id, resp)
    const b = mobile.deriveSigil(id, resp)
    if (a.shape !== b.shape || a.hue !== b.hue || a.trait !== b.trait ||
      a.shapeId !== b.shapeId || a.hueId !== b.hueId || a.traitId !== b.traitId) mismatch++
  }
  assert(mismatch === 0, `手机/桌面 300 个 seed 推导完全一致（mismatch=${mismatch}）`)

  let stackMismatch = 0
  const pool = ['idle', 'working', 'needs_attention', 'offline']
  for (let n = 0; n <= 6; n++) {
    const members = Array.from({ length: n }, (_, i) => ({
      id: `agt_stack_${i}`, responsibility: `r${i}`, state: pool[i % 4],
    }))
    for (const groupRunning of [false, true]) {
      const a = mod.planStack(members, { groupRunning })
      const b = mobile.planStack(members, { groupRunning })
      if (a.state !== b.state || a.more !== b.more || a.total !== b.total ||
        a.members.length !== b.members.length ||
        a.members.some((m, i) => m.id !== b.members[i].id || m.state !== b.members[i].state ||
          m.withStatus !== b.members[i].withStatus)) stackMismatch++
    }
  }
  assert(stackMismatch === 0, `手机/桌面 planStack（0–6 成员 × 群运行）完全一致（mismatch=${stackMismatch}）`)

  for (const input of [
    {}, { running: true }, { deleted: true }, { pending: true },
    { pending: true, deleted: true, running: true }, { deleted: true, running: true },
  ]) {
    assert(mobile.resolveAgentState(input) === mod.resolveAgentState(input),
      `手机/桌面状态映射一致：${JSON.stringify(input)}`)
  }
}

console.log(`\n✅ hub avatar 纯逻辑：${pass} 项全部通过`)
