#!/usr/bin/env node
// P8-01b 的两条 seam：Hub transport（transport/host.ts）与语言（host/lang.tsx）。
//
// 为什么值得一条 Node 脚本：这两条缝的唯一目的就是**切断静态依赖**（手机包不能因为
// 复用 Hub 组件而拉进 electron/mock/useAppStore）。这类回归在类型层面完全合法 ——
// 只有「包体里出现了什么」和「未注册时会不会静默退回 mock」能抓住，所以两头都测：
//   1. 运行时：未注册必须抛明确错误（不许静默 mock）；注册后取到的是同一个实例；
//   2. 渲染：HostLangProvider 的 lang 决定 useT()/t()（组件内与非组件代码一致）；
//   3. 静态：i18n/useHubStore/api/useHubAttachments 不再走静态 import 的实现模块。
import { build } from 'esbuild'
import { readFileSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createRequire } from 'node:module'

const APP = join(dirname(fileURLToPath(import.meta.url)), '..')
const SRC = join(APP, 'src')

let ok = 0
const eq = (got, want, m) => {
  if (got !== want) throw new Error(`断言失败: ${m}（got=${JSON.stringify(got)} want=${JSON.stringify(want)}）`)
  ok++
  console.log(`  ✅ ${m}（got=${JSON.stringify(got)}）`)
}
const assert = (cond, m) => {
  if (!cond) throw new Error(`断言失败: ${m}`)
  ok++
  console.log(`  ✅ ${m}`)
}

// —— 打包一个 Node 侧模块：入口是虚拟文件，resolveDir=src（与源码相对路径一致）——
const ENTRY = `
import * as React from 'react'
import { renderToString } from 'react-dom/server'
export * as host from './transport/host.ts'
export * as transport from './transport/index.ts'
export * as i18n from './i18n.ts'
export * as lang from './host/lang.tsx'
import { HostLangProvider } from './host/lang.tsx'
import { useT } from './i18n.ts'

function Probe() {
  const t = useT()
  return React.createElement('span', null, t('hub.title'))
}

export function renderProbe(lang) {
  return renderToString(React.createElement(HostLangProvider, { lang }, React.createElement(Probe)))
}
`

// CJS 产物：react-dom/server 是 CJS（内部 require('stream')），打成 ESM 会炸在
// 「Dynamic require of stream is not supported」。平台 node + CJS 与其它 Node 夹具一致。
const out = join('/tmp', `p8-host-seams-${process.pid}.cjs`)
await build({
  stdin: { contents: ENTRY, resolveDir: SRC, sourcefile: 'p8-host-seams-entry.tsx', loader: 'tsx' },
  bundle: true,
  platform: 'node',
  format: 'cjs',
  outfile: out,
  logLevel: 'silent',
  // mock transport 会引 Vite 的 ?raw/?url/?no-inline 与字体：与其它 Hub 夹具同一套空桩。
  loader: { '.woff2': 'empty', '.woff': 'empty', '.ttf': 'empty', '.png': 'empty', '.svg': 'empty' },
  plugins: [{
    name: 'stub-vite-assets',
    setup(b) {
      b.onResolve({ filter: /\?(raw|url|no-inline)$/ }, (a) => ({ path: a.path, namespace: 'vite-asset' }))
      b.onLoad({ filter: /.*/, namespace: 'vite-asset' }, () => ({ contents: 'export default ""', loader: 'js' }))
    },
  }],
})

const mod = createRequire(import.meta.url)(out)
const { host, transport, i18n, lang } = mod

console.log('\n1) Hub transport seam')
{
  // transport/index.ts 在模块求值时注册 getTransport：两处必须拿到同一个单例。
  const a = host.getHubTransport()
  const b = transport.getTransport()
  assert(a === b, 'transport/index.ts 求值时已注册（getHubTransport 与 getTransport 同实例）')

  // 未注册：必须抛明确错误，而不是静默退回 mock（P8-01 的失败模式就是「手机上看到假数据」）。
  host.resetHubTransport()
  eq(host.hasHubTransport(), false, 'resetHubTransport 之后未注册')
  let err = null
  try {
    host.getHubTransport()
  } catch (e) {
    err = e
  }
  assert(err instanceof Error, '未注册时抛 Error（不返回 mock）')
  assert(/未注册/.test(err.message), '错误信息说明「未注册」并指路（桌面 import transport/index.ts / 手机 setHubTransport）')
  assert(/setHubTransport/.test(err.message), '错误信息给出注册方法')

  // 注册后按工厂取值（不缓存）：换一个工厂立即生效。
  const fake = { send: async () => ({ ok: true }) }
  host.setHubTransport(() => fake)
  assert(host.getHubTransport() === fake, 'setHubTransport 注册的工厂立即生效')

  // 复原：后面的断言与同进程内其它 import 仍走真注册。
  host.setHubTransport(transport.getTransport)
  assert(host.getHubTransport() === a, '恢复注册后回到桌面 transport')
}

console.log('\n2) 语言 seam')
{
  // 无 provider：默认 zh（与桌面 settings 默认值一致，组件测试/夹具不会崩）。
  eq(lang.getHostLang(), 'zh', '默认语言 zh')
  eq(i18n.t('hub.title'), '智能体', '非组件 t() 默认读 zh')

  // provider 渲染期同步模块级值：同一次渲染里 useT() 与 t() 看到同一种语言。
  const en = mod.renderProbe('en')
  assert(en.includes('Agents'), 'HostLangProvider lang=en → useT() 返回英文')
  eq(lang.getHostLang(), 'en', 'provider 渲染后非组件 t() 也切到 en')
  eq(i18n.t('hub.title'), 'Agents', '非组件 t() 读到 en')

  const zh = mod.renderProbe('zh')
  assert(zh.includes('智能体'), 'HostLangProvider lang=zh → useT() 返回中文')
  eq(lang.getHostLang(), 'zh', '切回 zh 后模块级值同步')
}

console.log('\n3) 静态边界（手机包不拉桌面 store / electron / mock）')
{
  const read = (rel) => readFileSync(join(APP, rel), 'utf8')

  const i18n = read('src/i18n.ts')
  assert(!/from '\.\/store\/useAppStore'/.test(i18n), 'i18n.ts 不再 import useAppStore')
  assert(/from '\.\/host\/lang'/.test(i18n), 'i18n.ts 从 host/lang 取语言')

  for (const rel of ['src/hub/useHubStore.ts', 'src/hub/api.ts', 'src/hub/useHubAttachments.ts']) {
    const src = read(rel)
    assert(/from '\.\.\/transport\/host'/.test(src), `${rel} 走 transport/host`)
    assert(!/from '\.\.\/transport'/.test(src), `${rel} 不再静态 import transport/index（会把 electron/mock 拉进手机包）`)
  }

  const main = read('src/mobile/main.tsx')
  assert(/setHubTransport\(\(\) => sharedMobileTransport\(\)\)/.test(main), '手机 main.tsx 在首次渲染前注册 sharedMobileTransport()')
  assert(!/from '\.\.\/transport'/.test(main), '手机 main.tsx 不 import transport/index')

  const mobile = read('src/host/panels-mobile.tsx')
  assert(!/from '\.\.\/store\/useAppStore'/.test(mobile), '手机 host provider 不 import useAppStore')
  assert(/HostLangProvider/.test(mobile), '手机 provider 提供 host 语言')
  assert(/MOBILE_CAPABILITIES/.test(mobile), '手机 provider 按 allowlist 收窄能力')

  const desktop = read('src/host/panels-desktop.tsx')
  assert(/useAppStore\(\(s\) => s\.settings\.lang\)/.test(desktop), '桌面 provider 用 settings.lang 提供语言')
  assert(/HostLangProvider/.test(desktop), '桌面 provider 挂 HostLangProvider')
  assert(/DESKTOP_CAPABILITIES/.test(desktop), '桌面能力全开（行为零变化）')

  // 能力收窄必须真的落到组件上：手机 allowlist 未放行的入口不能留成 403 按钮。
  const sidebar = read('src/hub/HubSidebar.tsx')
  assert(/variant = 'desktop'/.test(sidebar), 'HubSidebar 有 mobile variant（默认 desktop）')
  assert(/capabilities\.groupAdmin/.test(sidebar), 'HubSidebar 的群编辑入口受 groupAdmin 控制')
  const inspector = read('src/hub/HubInspector.tsx')
  assert(/capabilities\.groupAdmin/.test(inspector) && /capabilities\.agentEditor/.test(inspector), 'HubInspector 的编辑入口受能力控制')
  const group = read('src/hub/GroupView.tsx')
  assert(/capabilities\.groupAdmin/.test(group), 'GroupView 的协作模式/预算/重置受 groupAdmin 控制')
  const artifacts = read('src/hub/TaskArtifacts.tsx')
  assert(/artifactPreview/.test(artifacts), 'TaskArtifacts 的预览入口受 artifactPreview 控制')
  const ctx = read('src/hub/ContextPanel.tsx')
  assert(/promptPreview/.test(ctx), 'ContextPanel 的提示词分层受 promptPreview 控制')
}

console.log('\n4) 手机 Hub 屏幕：只做布局/导航，列表与会话都复用桌面组件')
{
  const read = (rel) => readFileSync(join(APP, rel), 'utf8')
  const app = read('src/mobile/MobileApp.tsx')
  assert(/MobileHostPanelsProvider/.test(app), 'MobileApp 挂手机 host provider')
  assert(/attachHubEvents\(\)/.test(app), 'MobileApp 订阅 Hub 事件（attachHubEvents）')
  assert(/return attachHubEvents\(\)/.test(app), '订阅带 cleanup（StrictMode/重新配对不叠订阅）')
  assert(/st\.refresh\(\)/.test(app) && /st\.loadGroups\(\)/.test(app), '配对后主动 refresh + loadGroups')
  assert(/pushState/.test(app) && /popstate/.test(app), '会话页压 history，Android 返回键可回列表')
  assert(/'Escape'/.test(app), 'Esc 回列表')
  assert(/lazy\(\(\) => import\('\.\/screens\/HubListScreen'\)\)/.test(app), 'Hub 列表按需加载（配对屏不等消息渲染）')
  assert(!/from '\.\.\/store\/useAppStore'/.test(app), 'MobileApp 不 import useAppStore')

  const list = read('src/mobile/screens/HubListScreen.tsx')
  assert(/<HubSidebar variant="mobile" \/>/.test(list), '列表屏复用 HubSidebar（variant=mobile），不复制列表')
  assert(!/import .*(AgentSigil|AgentStack|HubRow)/.test(list), '列表屏没有自己画行')

  const conv = read('src/mobile/screens/ConversationScreen.tsx')
  assert(/<HubSurface \/>/.test(conv), '会话屏复用 HubSurface（DM 与群同一份）')
  assert(/data-testid="mobile-back"/.test(conv), '会话屏有返回按钮（e2e 依赖这个 testid）')
  assert(!/import .*(GroupView|MessageBubble|TaskResultCard)/.test(conv), '会话屏没有自己画消息/任务卡')
  // P8-03a 单头：外层不再有第二个「⋯」抽屉入口；共享 chat-header 的「上下文」是抽屉入口，
  // 「更多操作」是唯一菜单。外层只留返回 + 视觉隐藏标题（e2e/读屏兜底）。
  assert(!/mobile-inspector-open/.test(conv), '会话屏外层不再有重复的「⋯」抽屉入口（单头）')
  assert(/data-testid="mobile-conv-title"/.test(conv), '会话屏保留 mobile-conv-title（e2e/读屏兜底）')
  assert(/className="m-conv-head"/.test(conv) && !/m-conv-inspector/.test(conv), '会话屏外层头只做返回（不占一行）')

  const drawer = read('src/mobile/screens/InspectorDrawer.tsx')
  assert(/<HubInspector \/>/.test(drawer), '抽屉复用 HubInspector（Context/Tasks/Needs 不隐藏）')
  assert(/role="dialog"/.test(drawer), '抽屉是 dialog 语义')
  assert(/'Escape'/.test(drawer), '抽屉 Esc 关闭')
  assert(/data-overflow/.test(drawer), '抽屉页签量溢出写 data-overflow（横向滚动 + 边缘渐隐）')
  assert(/ResizeObserver/.test(drawer), '抽屉页签跟随宽度/字号变化重量溢出')
}

console.log(`\nHOST_SEAMS_TEST_PASS (${ok})`)
