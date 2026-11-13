#!/usr/bin/env node
// content_chunk 的「不可见更新」契约（Node 直跑，无需浏览器）。
//
// 背景：mock 与真 bridge 在发 hub_message_delta（新草稿路径）的同时**仍然**发
// content_chunk（旧 live 路径）。旧实现在每个 token 上无条件写两处 state：
//   ① `live: s.live + piece` —— 有草稿时 HubSurface 的 showLive=false，这段正文没人画，
//      但换引用会让整棵 DM 视图（消息列表 + Markdown）reconcile 一遍；
//   ② `running: { ...s.running, [activeId]: true }` —— 已经是 true 也换新对象，订阅整张
//      running map 的左栏/群视图/右栏全部白重渲染。
// 这里把「什么时候必须写、什么时候必须一个字都不动」钉死：state 引用比内容更容易回退
// （有人把 `!== true` 写成 `!!` 或把 `return s` 改成 `return {}`，渲染就悄悄退化回去）。
//
// 覆盖：
//   · 无草稿（delta 还没来）：content_chunk 仍写 live（兜底路径不能死）；
//   · running 已 true：content_chunk 复用原 running 对象（引用不变）；
//   · 有 DM 草稿：content_chunk 不写 live、不碰 running，且 store 零通知；
//   · running false→true 且有草稿：只点亮运行点，不写 live；
//   · 群模式（activeGroupId 非空）：content_chunk 不写 live；
//   · final 落地清掉草稿后：content_chunk 又能写 live（闸门不是写死的）；
//   · 空 piece / 非根 run / 别的会话：仍然被原有闸门挡掉。
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
    sourcefile: 'hub-stream-identity-entry.ts',
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
const H = mod.store.useHubStore

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const eq = (got, want, m) => assert(got === want, `${m}（got=${JSON.stringify(got)} want=${JSON.stringify(want)}）`)

// —— 事件注入：与 bridge/mock 同一条入口（store.handleEvent 收的是 JSON 行）——
const emit = (ev) => H.getState().handleEvent(JSON.stringify(ev))
const rootStart = (sessionId, runId) => emit({
  event_type: 'agent_start', session_id: sessionId, run_id: runId,
  parent_run_id: '', depth: 0, name: 'Researcher',
})
const chunk = (sessionId, runId, content, extra = {}) => emit({
  event_type: 'content_chunk', session_id: sessionId, run_id: runId,
  parent_run_id: '', depth: 0, content, ...extra,
})
const delta = (agentId, messageId, index, text, groupId = '') => emit({
  event_type: 'hub_message_delta', group_id: groupId, agent_id: agentId,
  message_id: messageId, index, delta: text,
})
/** 记录 store 在一次操作里通知了几次订阅者（0 = 零渲染）。 */
function countNotifications(fn) {
  let n = 0
  const off = H.subscribe(() => { n++ })
  try { fn() } finally { off() }
  return n
}

const DM_SID = 'hub.ag_mockres1.dm'
const ROOT_RUN = 'run_root_1'

// —— 1) 打开 DM，模拟根 run 开始 ——
{
  H.setState({ open: true })
  await H.getState().select('ag_mockres1')
  eq(H.getState().activeId, 'ag_mockres1', 'DM 打开（hub_open）选中目标智能体')
  eq(H.getState().sid, DM_SID, 'DM 的 session_id 是 hub.<agent>.dm')
  eq(H.getState().live, '', '打开后 live 为空')
  rootStart(DM_SID, ROOT_RUN)
  eq(H.getState().running.ag_mockres1, true, 'agent_start 点亮运行点')
  eq(H.getState().rootRunId, ROOT_RUN, 'agent_start 记下根 run_id')
  eq(H.getState().groupDrafts.length, 0, '此时没有任何草稿（delta 还没来）')
}

// —— 2) 无草稿：content_chunk 仍走 live 兜底；running 已 true → 引用不变 ——
{
  const runningBefore = H.getState().running
  const liveBefore = H.getState().live
  chunk(DM_SID, ROOT_RUN, 'x')
  eq(H.getState().live, `${liveBefore}x`, '无草稿时 content_chunk 仍累加 live（兜底路径没死）')
  eq(H.getState().running, runningBefore, 'running 已 true → 复用原对象（引用不变）')
}

// —— 3) hub_message_delta 建草稿 ——
{
  delta('ag_mockres1', 'm1', 0, 'a')
  const drafts = H.getState().groupDrafts
  eq(drafts.length, 1, 'DM delta 建出一条草稿')
  eq(drafts[0].groupId, '', 'DM 草稿的 groupId 是空串')
  eq(drafts[0].agentId, 'ag_mockres1', '草稿归属当前智能体')
  eq(drafts[0].text, 'a', '草稿正文 = 第一块 delta')
}

// —— 4) 有草稿：content_chunk 一个字都不写（live 不变、running 引用不变、零通知）——
{
  const liveBefore = H.getState().live
  const runningBefore = H.getState().running
  const stateBefore = H.getState()
  const notified = countNotifications(() => chunk(DM_SID, ROOT_RUN, 'y'))
  eq(H.getState().live, liveBefore, '有草稿时 content_chunk 不写 live（同一段正文的第二份没人画）')
  eq(H.getState().running, runningBefore, '有草稿时 running 引用不变')
  assert(H.getState() === stateBefore, '整个 state 对象不变（Zustand 不通知）')
  eq(notified, 0, '有草稿时 content_chunk 零通知 → 零 reconcile')
  // 连着 20 个 token：仍然零通知（不是只挡第一个）。
  const notified20 = countNotifications(() => {
    for (let i = 0; i < 20; i++) chunk(DM_SID, ROOT_RUN, `t${i}`)
  })
  eq(notified20, 0, '连续 20 个 token 全部零通知')
  eq(H.getState().live, liveBefore, '20 个 token 之后 live 仍然没动')
}

// —— 5) 有草稿 + running false→true：只点亮运行点，不写 live ——
{
  H.setState({ running: { ...H.getState().running, ag_mockres1: false } })
  const runningBefore = H.getState().running
  const liveBefore = H.getState().live
  chunk(DM_SID, ROOT_RUN, 'z')
  eq(H.getState().running.ag_mockres1, true, '有草稿时 content_chunk 仍然点亮运行点')
  assert(H.getState().running !== runningBefore, 'running 从 false→true 换了引用（必须重渲染左栏）')
  eq(H.getState().live, liveBefore, 'false→true 这一次也不写 live')
  // 再发一个 token：现在 running 已 true，又回到零变化。
  const runningNow = H.getState().running
  const notified = countNotifications(() => chunk(DM_SID, ROOT_RUN, 'z2'))
  eq(H.getState().running, runningNow, '点亮之后的下一个 token 复用 running 引用')
  eq(notified, 0, '点亮之后的下一个 token 零通知')
}

// —— 6) 群模式：activeGroupId 非空时 content_chunk 不写 live ——
{
  await H.getState().selectGroup('grp_mock1')
  eq(H.getState().activeGroupId, 'grp_mock1', '群打开（selectGroup）')
  eq(H.getState().activeId, null, '群模式没有 activeId')
  // 群模式下 store 的 sid 是 null（content_chunk 因此本就被 `!sid` 挡掉）；这里显式给一个
  // 群会话 id，验证的是**第二道**闸门：即使 session/run 都对得上，live 也不该被写。
  const GROUP_SID = 'hub.ag_mockres1.g.grp_mock1'
  H.setState({ sid: GROUP_SID, live: 'g0' })
  const runningBefore = H.getState().running
  const notified = countNotifications(() => chunk(GROUP_SID, 'run_group_1', '群正文'))
  eq(H.getState().live, 'g0', '群模式 content_chunk 不写 live（群正文走 groupDrafts）')
  eq(H.getState().running, runningBefore, '群模式 running 引用不变')
  eq(notified, 0, '群模式 content_chunk 零通知')
}

// —— 7) final 落地清掉草稿后，content_chunk 又能写 live（闸门没写死）——
{
  await H.getState().select('ag_mockres1')
  eq(H.getState().activeId, 'ag_mockres1', '切回 DM')
  eq(H.getState().groupDrafts.length, 0, '切回 DM 后草稿清空')
  rootStart(DM_SID, ROOT_RUN)
  delta('ag_mockres1', 'm2', 0, '半截')
  eq(H.getState().groupDrafts.length, 1, '重新建一条草稿')
  const liveBefore = H.getState().live
  chunk(DM_SID, ROOT_RUN, '被丢弃')
  eq(H.getState().live, liveBefore, '有草稿期间 live 不动（复现第 4 步）')
  // 轮末 final：message_id 复用草稿的 m2。
  emit({
    event_type: 'hub_message', agent_id: 'ag_mockres1', seq: 9001,
    message: {
      seq: 9001, id: 'm2', kind: 'text', sender: { kind: 'agent', agent_id: 'ag_mockres1' },
      text: '半截', created_at: 'T',
    },
  })
  eq(H.getState().groupDrafts.length, 0, 'final 删掉草稿')
  chunk(DM_SID, ROOT_RUN, '新一轮')
  eq(H.getState().live, '新一轮', 'final 之后 content_chunk 又能写 live（闸门没有写死）')
}

// —— 8) 原有闸门仍然生效：空 piece / 非根 run / 别的会话 ——
{
  const liveBefore = H.getState().live
  const notifiedEmpty = countNotifications(() => chunk(DM_SID, ROOT_RUN, ''))
  eq(H.getState().live, liveBefore, '空 piece 不写 live')
  eq(notifiedEmpty, 0, '空 piece 零通知')
  // 子 agent 的 chunk 带同一 session_id、不同 run_id（depth>0）→ 丢掉。
  const notifiedSub = countNotifications(() => chunk(DM_SID, 'run_child_1', '子 agent 正文', { depth: 1 }))
  eq(H.getState().live, liveBefore, '子 agent（非根 run）的 chunk 不写 live')
  eq(notifiedSub, 0, '子 agent 的 chunk 零通知')
  const notifiedOther = countNotifications(() => chunk('hub.ag_mockwri2.dm', ROOT_RUN, '别人的会话'))
  eq(H.getState().live, liveBefore, '别的会话的 chunk 不写 live')
  eq(notifiedOther, 0, '别的会话的 chunk 零通知')
  // 根 run 已设：run_id 对不上的根级 chunk 也要丢（同一会话里上一轮迟到的正文）。
  const notifiedRun = countNotifications(() => chunk(DM_SID, 'run_root_old', '上一轮'))
  eq(H.getState().live, liveBefore, 'run_id 对不上根 run 的 chunk 不写 live')
  eq(notifiedRun, 0, 'run_id 对不上的 chunk 零通知')
}

console.log(`\n✅ hub content_chunk 不可见更新契约：${pass} 项全部通过`)
