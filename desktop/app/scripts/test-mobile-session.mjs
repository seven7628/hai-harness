#!/usr/bin/env node
// 手机主会话页的纯逻辑验收（P8-02，Node 直跑，无需浏览器/构建）。
//
// 为什么值得一条脚本：手机会话页的**数据面**只有一层薄薄的东西 ——
// mobile/session-data.ts（workspace_list + list 的归一/排序/文案 + 按 id 对账的
// 请求助手）。它同时决定了三件容易静默坏掉的事：
//   1. 列表数据必须来自 bridge（手机首次配对没有桌面 localStorage 项目树）；
//   2. 点一行必须落到 useAppStore.setActiveSession(sid, ws)（不另建会话状态）；
//   3. 会话页必须复用桌面 CenterPanel，且**按需加载**（首屏 chunk 不能有 store/mock）。
// 前两条在这里用假 transport 断言；第三条是静态边界断言（源码形状 + 分包入口）。
//
// 另加：手机白名单的能力收窄（Hub 空态的新建入口、Composer 的桌面专属入口、store 的
// 手机命令闸门）—— 这些是「界面上有个按钮点了会 403」的那类回归。
import { build } from 'esbuild'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const APP = join(__dirname, '..')

let pass = 0
const ok = (m) => {
  pass++
  console.log(`  ✅ ${m}`)
}
const assert = (cond, m) => {
  if (!cond) throw new Error(`断言失败: ${m}`)
  ok(m)
}
const eq = (got, want, m) => {
  if (got !== want) throw new Error(`断言失败: ${m}（got=${JSON.stringify(got)} want=${JSON.stringify(want)}）`)
  ok(m)
}

// ─────────── 载入被测模块（esbuild 打包成 ESM，导入 data URL）───────────
const result = await build({
  entryPoints: [join(APP, 'src', 'mobile', 'session-data.ts')],
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
})
const mod = await import(`data:text/javascript;base64,${Buffer.from(result.outputFiles[0].text).toString('base64')}`)

console.log('1) workspace_list / list 的形状归一')

{
  const ws = mod.parseWorkspaces({
    workspaces: [
      { path: '/repo/a', title: '主力仓', pinned: true, last_opened: 1700000000000 },
      { path: '/repo/b' },
      { path: '   ' }, // 空路径：丢弃
      { title: '没有 path' },
      null,
    ],
  })
  eq(ws.length, 2, '空路径/无 path 的工作区被丢掉')
  eq(ws[0].title, '主力仓', 'title 透传')
  eq(ws[0].pinned, true, 'pinned 透传')
  eq(ws[0].lastOpened, 1700000000000, 'last_opened → lastOpened')
  eq(ws[1].title, '', '缺 title → 空串（由 workspaceLabel 兜底）')
  eq(ws[1].pinned, false, '缺 pinned → false')

  const none = mod.parseWorkspaces(undefined)
  eq(none.length, 0, '响应缺 workspaces → 空数组（不抛）')
  eq(mod.parseWorkspaces({ workspaces: 'nope' }).length, 0, 'workspaces 不是数组 → 空数组')
}

{
  const list = mod.parseSessions({
    sessions: [
      { id: 's1', title: '一', updated_at: 1000, pinned: true, running: true },
      { id: 's2', updatedAt: 2000 },
      { id: '' },
      {},
    ],
  })
  eq(list.length, 2, '缺 id 的会话被丢掉')
  eq(list[0].title, '一', 'title 透传')
  eq(list[0].running, true, 'running 透传')
  eq(list[1].title, 's2', '缺 title → 回落成 id（列表不能出现空白行）')
  eq(list[1].updatedAt, 2000, 'updatedAt（驼峰）也认')
}

console.log('\n2) 排序 / 默认工作区 / 文案')

{
  const rows = mod.sortSessionRows([
    { id: 'a', title: 'a', updatedAt: 100, pinned: false, running: false },
    { id: 'b', title: 'b', updatedAt: 300, pinned: false, running: false },
    { id: 'c', title: 'c', updatedAt: 50, pinned: true, running: false },
    { id: 'd', title: 'd', updatedAt: 50, pinned: false, running: false },
  ])
  eq(rows.map((r) => r.id).join(','), 'c,b,a,d', '置顶优先 → 最近活动降序 → 同刻按 id 降序')

  const noTs = mod.sortSessionRows([
    { id: 's10', title: '', pinned: false, running: false },
    { id: 's9', title: '', pinned: false, running: false },
  ])
  eq(noTs.map((r) => r.id).join(','), 's9,s10', '没有时间戳时按 id 降序（genID 单调 ≈ 创建逆序）')
}

{
  const list = mod.parseWorkspaces({ workspaces: [{ path: '/a' }, { path: '/b' }] })
  eq(mod.pickWorkspace(list, '/b'), '/b', '上次选中的工作区还在清单里 → 用它')
  eq(mod.pickWorkspace(list, '/gone'), '/a', '上次选中的不在了 → 回第一条')
  eq(mod.pickWorkspace([], '/a'), '', '空清单 → 空串')
  eq(mod.pathTail('/Users/x/repo-name/'), 'repo-name', 'pathTail 去掉尾斜杠取最后一段')
  eq(mod.pathTail('C:\\work\\repo'), 'repo', 'pathTail 认 Windows 分隔符')
  eq(mod.workspaceLabel({ path: '/a/b', title: '别名', pinned: false }), '别名', '工作区别名优先')
  eq(mod.workspaceLabel({ path: '/a/b', title: '', pinned: false }), 'b', '无别名 → 路径尾段')
  eq(mod.workspaceLabel({ path: '/', title: '', pinned: false }, 'en'), 'Untitled workspace', '尾段为空 → 本地化兜底')
  eq(mod.sessionLabel({ id: 's', title: '', pinned: false, running: false }, 'en'), 'Untitled session', '空标题会话兜底')
}

{
  const now = 1_700_000_000_000
  eq(mod.fmtSessionTime(undefined, now), '', '没有时间 → 空串（不显示假时间）')
  eq(mod.fmtSessionTime(now - 10_000, now), '刚刚', '10 秒前 = 刚刚')
  eq(mod.fmtSessionTime(now - 5 * 60_000, now), '5 分钟前', '分钟档')
  eq(mod.fmtSessionTime(now - 3 * 3600_000, now), '3 小时前', '小时档')
  eq(mod.fmtSessionTime(now - 2 * 86400_000, now), '2 天前', '天档')
  eq(mod.fmtSessionTime(now - 3 * 3600_000, now, 'en'), '3h ago', 'en 文案')
  assert(/^\d\d-\d\d$/.test(mod.fmtSessionTime(now - 30 * 86400_000, now)), '超过一周给 MM-DD')
}

console.log('\n3) request()：按命令 id 对账，不吃别人的帧')

function fakeTransport(handler) {
  const subs = new Set()
  const sent = []
  return {
    sent,
    onEvent(cb) {
      subs.add(cb)
      return () => subs.delete(cb)
    },
    async send(cmd) {
      sent.push(cmd)
      const r = await handler(cmd)
      if (r) for (const cb of [...subs]) cb(JSON.stringify(r))
      return { ok: true }
    },
    emit: (line) => {
      for (const cb of [...subs]) cb(line)
    },
    subs: () => subs.size,
  }
}

{
  // 正常：响应带同一个 id → 解出 data，且订阅被清掉（不泄漏）
  const t = fakeTransport(async (cmd) => ({
    event_type: 'command_response',
    id: cmd.id,
    ok: true,
    data: { workspaces: [{ path: '/w' }] },
  }))
  const r = await mod.request(t, 'workspace_list')
  assert(r.ok, 'request 成功')
  eq(r.data.workspaces[0].path, '/w', '拿到 data')
  eq(t.sent[0].type, 'workspace_list', '发出的命令类型正确')
  assert(t.sent[0].id >= 2_000_000_000, '直连 transport 的命令 id 从 20 亿起（不与 store 的 cmdSeq 撞）')
  eq(t.subs(), 0, '响应到达后解绑 onEvent（不泄漏订阅）')
}

{
  // 干扰帧：别的 id、非 JSON、非 command_response —— 都不能结算
  const t = fakeTransport(async () => null)
  const p = mod.request(t, 'list', { workspace: '/w' }, 60)
  t.emit('not json')
  t.emit(JSON.stringify({ event_type: 'content_chunk', id: 1, content: 'x' }))
  t.emit(JSON.stringify({ event_type: 'command_response', id: 12345, ok: true, data: {} }))
  const r = await p
  assert(!r.ok && /超时/.test(r.error || ''), '别的 id/别的帧不结算 → 超时收口')
  eq(t.subs(), 0, '超时后同样解绑')
}

{
  const t = fakeTransport(async (cmd) => ({ event_type: 'command_response', id: cmd.id, ok: false, error: 'command not allowed: list' }))
  const r = await mod.request(t, 'list')
  assert(!r.ok, '失败响应 → ok:false')
  eq(r.error, 'command not allowed: list', '错误原文透传（列表屏直接显示）')
}

{
  // transport 层直接失败（未配对）：不会再合成事件，request 必须自己收口
  const t = fakeTransport(async () => null)
  t.send = async () => ({ ok: false, error: '未配对' })
  const r = await mod.request(t, 'workspace_list')
  assert(!r.ok, 'send 失败 → ok:false')
  eq(r.error, '未配对', 'send 的错误原文透传')
}

{
  // fetchSessions 会把 workspace 放进 payload（bridge 靠它路由会话）
  const t = fakeTransport(async (cmd) => ({ event_type: 'command_response', id: cmd.id, ok: true, data: { sessions: [{ id: 's2', updated_at: 1 }, { id: 's1', updated_at: 9 }] } }))
  const r = await mod.fetchSessions(t, '/repo')
  eq(t.sent[0].payload.workspace, '/repo', 'list 带 workspace')
  eq(r.items.map((s) => s.id).join(','), 's1,s2', '会话按最近活动排序')
  const empty = await mod.fetchSessions(t, '')
  eq(empty.items.length, 0, '没有工作区 → 不发命令、空清单')
}

console.log('\n4) 静态边界：会话页复用桌面组件 + 按需加载')

{
  const read = (rel) => readFileSync(join(APP, rel), 'utf8')

  const app = read('src/mobile/MobileApp.tsx')
  assert(/lazy\(\(\) => import\('\.\/screens\/SessionListScreen'\)\)/.test(app), '会话列表按需加载（首屏 chunk 不带 store）')
  assert(/lazy\(\(\) => import\('\.\/screens\/SessionScreen'\)\)/.test(app), '会话页按需加载')
  assert(!/from '\.\.\/store\/useAppStore'/.test(app), 'MobileApp 仍不 import useAppStore（P8-01b 的边界不能破）')
  // 实测：静态引 i18n 会把中英两份完整字典（~200KB）整份拉进首屏 Hub chunk
  // （218KB → 433KB）。手机壳的既有文案本来就是就地写法，入口条两格也一样。
  assert(!/from '\.\.\/i18n'/.test(app), 'MobileApp 不静态 import i18n（字典不进首屏 chunk）')
  assert(/onOpenSession=\{openSession\}/.test(app), '壳把 openSession 交给 host provider（Hub 任务卡的「打开会话」同一落点）')
  assert(/data-testid="mobile-tab-sessions"/.test(app) && /data-testid="mobile-tab-hub"/.test(app), '入口条有 Hub / 会话两个并列入口')
  assert(/setSess\(null\)\s*\n\s*useHubStore\.getState\(\)\.closeHub\(\)/.test(app), '返回/后退同时清会话与 Hub（两条栈共用一条 history 记录）')
  // P8-03a：软键盘（visualViewport）与浏览器 UI 主题色都在入口装一次，不进组件树。
  const main = read('src/mobile/main.tsx')
  assert(/installVisualViewport\(\)/.test(main), '入口装 visualViewport 观察者（软键盘）')
  assert(/installThemeColor\(\)/.test(main), '入口装 theme-color 观察者（ink/paper 状态栏）')

  const vv = read('src/mobile/visual-viewport.ts')
  for (const [re, why] of [
    [/window\.visualViewport/, '读 visualViewport'],
    [/--m-vv-height/, '写可见高度变量'],
    [/--m-vv-offset/, '写可见区偏移变量'],
    [/offsetTop/, '用 offsetTop 跟随 iOS 上滚'],
    [/addEventListener\('resize'/, '监听 resize'],
    [/addEventListener\('scroll'/, '监听 scroll'],
    [/requestAnimationFrame/, 'resize/scroll 合并到下一帧'],
  ]) assert(re.test(vv), `visualViewport：${why}`)

  const chrome = read('src/mobile/chrome.ts')
  assert(/MutationObserver/.test(chrome) && /theme-color/.test(chrome), 'theme-color 跟随 data-theme（含 system 实时切换）')
  assert(/#08080a/.test(chrome) && /#f4f3f1/.test(chrome), 'theme-color 取 runtime ink/paper 的 --paper')

  const sess = read('src/mobile/screens/SessionScreen.tsx')
  assert(/<CenterPanel variant="mobile" \/>/.test(sess), '会话页直接挂桌面 CenterPanel（不复制转录/composer）')
  assert(!/className="msg|className=\{`msg/.test(sess), '会话页没有自己画消息')
  assert(/setActiveSession\(sid, ws\)/.test(sess), '激活走 useAppStore.setActiveSession(sid, ws)')
  assert(/connectChat\(ws, sid\)/.test(sess), '打开 chat WS（流式事件源）')
  assert(/disconnect\('chat'\)/.test(sess), '离开会话页断开 chat 链路（旧 sid 的订阅随之结束）')
  assert(/ensureStoreStarted\(\)/.test(sess), '接线走 ensureStoreStarted（只接一次，不重复 start）')

  const list = read('src/mobile/screens/SessionListScreen.tsx')
  assert(/fetchWorkspaces\(sharedMobileTransport\(\)\)/.test(list), '列表数据来自 workspace_list（不读桌面 localStorage 项目树）')
  assert(/fetchSessions\(sharedMobileTransport\(\), ws\)/.test(list), '会话数据来自 list')
  // 列表屏**完全不碰 store**：点开「会话」只加载它自己的小 chunk，桌面 store +
  // Narrative 那一大坨等真正打开某个会话时才拉（否则首点就要 450KB）。
  assert(!/from '[^']*(store\/useAppStore|session-boot)'/.test(list), '列表屏不 import store / session-boot（小 chunk 先出）')
  assert(/ensureStoreStarted\(\)/.test(sess), '会话页负责接线（ensureStoreStarted 在会话屏）')

  const boot = read('src/mobile/session-boot.ts')
  assert(/let booted = false/.test(boot) && /if \(booted\) return/.test(boot), 'ensureStoreStarted 幂等（模块级闸门）')

  const center = read('src/components/CenterPanel.tsx')
  assert(/variant\?: 'desktop' \| 'mobile'/.test(center), 'CenterPanel 有 variant（默认 desktop）')
  assert(/\{mobile \? null : <RecentBar \/>\}/.test(center), '手机上不渲染 RecentBar（标题由手机会话页头部给）')
  assert(/<Composer variant=\{variant\} \/>/.test(center), 'variant 透传给 Composer')

  const composer = read('src/components/Composer.tsx')
  assert(/variant\?: 'desktop' \| 'mobile'/.test(composer), 'Composer 有 variant（默认 desktop）')
  for (const [re, why] of [
    [/\{!mobile && !focused && \(\s*<ChipMenu\s*\n\s*className="mode-chip"/, '模式芯片'],
    [/\{!focused && \(<>\s*\n\s*<div\s*\n\s*ref=\{chipRef\}\s*\n\s*className="model-chip"/, '模型芯片'],
    [/\{!mobile && !focused && canImage && \(/, '图片附件按钮'],
    [/!mobile && !focused && \(\s*\n\s*<button\s*\n\s*className="attach-btn doc-btn"/, '添加文件按钮'],
    [/!mobile && sttEnabled && \(/, '语音按钮'],
    [/\{!mobile && atOpen && workspace && \(/, '@ 文件引用面板'],
    [/filtered\.length > 0 && !mobile && !focused/, '斜杠命令菜单'],
  ]) assert(re.test(composer), `手机上不渲染桌面专属入口：${why}`)
  assert(/composer\.placeholderMobile/.test(composer), '手机用不含 @ / 斜杠承诺的占位符')

  const css = read('src/mobile/mobile.css')
  for (const [re, why] of [
    [/\.m-session \.center \{/, 'CenterPanel 单列容器'],
    [/\.m-session \.narrative \{/, '叙述区自己滚'],
    // P8-03a：安全区先落到 --m-safe-* 变量（夹具可覆写成 34px 验证 home 条），
    // 由 .m-app 统一让位；composer-wrap 不再重复吃 env()（否则 iPhone 上双倍内边距）。
    [/:root \{[\s\S]{0,400}--m-safe-bottom: env\(safe-area-inset-bottom, 0px\)/, '安全区落到 --m-safe-* 变量'],
    [/\.m-session \.composer-wrap \{[\s\S]{0,200}padding: 6px 10px 8px/, 'composer 贴底（安全区不重复吃）'],
    [/\.m-session \.composer \.input,\s*\n\.m-session \.composer \.input-hl \{\s*\n\s*font-size: 16px/, 'iOS 输入 16px（不缩放）'],
    [/\.m-session \.composer \.row2 \.send-btn \{[\s\S]{0,80}width: 44px/, '发送/停止 44px'],
    [/\.m-session \.dock \.btn,[\s\S]{0,200}min-height: 44px/, '审批/提问按钮 44px'],
    // P8-03a：软键盘 / 单头 / composer 收紧 / 抽屉溢出 / 主题令牌补丁。
    [/\.m-app \{[\s\S]{0,400}height: var\(--m-vv-height, 100dvh\)/, '.m-app 用 --m-vv-height（软键盘）'],
    [/\.m-app \{[\s\S]{0,500}translateY\(var\(--m-vv-offset/, '.m-app 跟随可见区偏移'],
    [/\.m-conv \.hub-dm \.composer-meta \{\s*\n\s*display: none/, 'DM 空 meta 行不占位'],
    [/\.m-conv \.hub-demo \.composer > textarea \{[\s\S]{0,200}max-height: 88px/, 'composer 输入行 4 行封顶'],
    [/\.m-conv-head \{[\s\S]{0,300}position: absolute/, '会话返回键浮在共享 chat-header 上（单头）'],
    [/\.m-drawer \.hub-inspector-tabs\[data-overflow='end'\]/, '抽屉页签溢出边缘渐隐'],
    [/\.m-conv \.hub-demo\[data-theme\] \{[\s\S]{0,800}--text: var\(--ink\)/, 'Hub 会话面重指到 runtime 令牌'],
    [/--text-2: var\(--ink-2\)/, '补 index.css 派生令牌（muted 文字不再继承正文色）'],
    [/@media \(prefers-reduced-motion: reduce\) \{\s*\n\s*\.m-app,/, 'reduced-motion 覆盖手机壳与复用组件'],
    [/\.m-session \.json-box \{/, '审批卡 JSON 视图的壳样式（index.css 不在手机包）'],
  ]) assert(re.test(css), `手机样式：${why}`)
}

console.log('\n5) 能力收窄：手机白名单外的入口不渲染 / 不发')

{
  const read = (rel) => readFileSync(join(APP, rel), 'utf8')

  const sidebar = read('src/hub/HubSidebar.tsx')
  assert(/capabilities\.agentEditor \? \(\s*\n\s*<button className="hub-side-empty-add"/.test(sidebar), 'Hub 空态的「新建智能体」也受 agentEditor 控制（手机不放行 hub_agent_create）')

  const panels = read('src/host/panels-mobile.tsx')
  assert(/onOpenSession/.test(panels), '手机 host provider 接 onOpenSession')
  assert(!/会话页在手机上还没开放/.test(panels), '「请在桌面端打开」的指路文案已经撤掉（P8-02 真的能跳了）')
  assert(/if \(onOpenSession && ws && sid\) \{/.test(panels), 'openProjectSession 有壳就直接跳会话页')
  assert(/value=\{workspace\}/.test(panels), 'Markdown 相对资源锚点 = 当前会话的工作区')

  const store = read('src/store/useAppStore.ts')
  assert(/const MOBILE_UNSUPPORTED_COMMANDS = new Set\(\[/.test(store), 'store 有手机命令闸门（白名单外的命令不发）')
  assert(/if \(mobileHost\(\) && MOBILE_UNSUPPORTED_COMMANDS\.has\(cmd\.type\)\) return 0/.test(store), '闸门在 send() 里、发之前挡掉')
  for (const cmd of ['git_snapshot', 'git_history', 'switch_model', 'switch_effort', 'switch_persona', 'switch_mode', 'skills', 'compact', 'metrics']) {
    assert(new RegExp(`'${cmd}'`).test(store), `闸门覆盖 ${cmd}`)
  }
  assert(/window\.__HAI_MOBILE__ === true/.test(store), '闸门判据 = window.__HAI_MOBILE__（桌面恒 false）')
}

console.log(`\nMOBILE_SESSION_TEST_PASS (${pass})`)
