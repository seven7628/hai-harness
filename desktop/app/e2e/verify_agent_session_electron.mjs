#!/usr/bin/env node
// verify_agent_session_electron.mjs —— 插件 `agent.session` 的**真 Electron 全链路**（P1d）。
//
// 已有的几条测试各自换掉了这条链上的一半，所以没有任何一条真的把「插件面板里的一个
// host.command」走完到「磁盘上的一个会话」：
//   · scripts/test-surface-host.mjs：loader 的闸门（真 loader，假 IPC）；
//   · go test -run TestSurfaceAgentSession*：Go 侧装配（真 manager，假 IPC）；
//   · verify_external_surface_electron.mjs：真 Electron + 真安装器，但插件只渲染静态内容。
// 这一条**一个替身都不留**：
//   1. 真安装器把一个临时插件装进临时 HOME（= <HOME>/.go-code/surfaces）；
//   2. 以 HOME=HAI_DATA_DIR=<同一个临时目录> 启动**真的**应用（真 preload、真 claim、
//      真 hai-surface:// 协议、真 RightPanel、真 Go 桥进程）；
//   3. 面板里 `host.command('agent.session')` → preload → main → 桥（surface_agent）
//      → `plugin-sessions/<id>/sessions/…`；断言插件拿到的 session_id 与磁盘/左栏同源；
//   4. 左栏插件分组里出现这条会话 —— A（会话即时刷新）的**唯一**真机证据：
//      在这条链路里除「插件建完之后宿主自己去重拉一次」以外没有任何东西会去拉列表。
//
// 为什么不断言 agent.run：那要真模型（Provider/密钥/网络），本机没有也不该有。agent.session
// 是**唯一**不花 token 就能造出插件会话的入口，也正是「插件先记 session_id、之后 run 复用它」
//（D4）的第一步 —— 要验的正是这一步。
//
// 用法：cd desktop/app && node e2e/verify_agent_session_electron.mjs
// 退出码：0 通过 / 1 断言失败 / 2 前置不满足（缺 dist 产物 / 装不上插件 / 起不来）。
//
// 多实例：HAI_DATA_DIR 会让 Electron 跳过单实例锁（本机可能正跑着用户自己的实例，
// 不加会被 requestSingleInstanceLock 直接踢掉，表现为 launch 后立刻退出）。
import { _electron } from 'playwright'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const REPO = path.resolve(APP, '../..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const PLUGIN_ID = 'agent-e2e'
const PLUGIN_KIND = `plugin:${PLUGIN_ID}`
// 会话标题带随机后缀：左栏那行的标题 = 这次调用传下去的 title，是「左栏那条就是本面板
// 刚建的会话」的**唯一**可见证据（同一个插件里没有第二条会话能叫这个名字）。
const SESSION_TITLE = `e2e-${Date.now().toString(36)}`

class Precondition extends Error {
  constructor(msg) { super(msg); this.name = 'Precondition'; this.exitCode = 2 }
}
const fail = (msg) => { throw new Precondition(msg) }
const fails = []
function check(label, cond, detail = '') {
  if (cond) console.log(`  ✅ ${label}`)
  else { fails.push(label); console.log(`  ❌ ${label}${detail ? ' —— ' + detail : ''}`) }
}
async function waitFor(fn, { timeout = 30000, label = '条件' } = {}) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v } catch { /* 继续等 */ }
    await sleep(200)
  }
  throw new Error(`等待超时：${label}`)
}

// —— 临时插件：surface.json + panel.js（无构建步骤，浏览器直接 import）——
//
// 面板只做三件事：记环境事实、问宿主建一个会话、问一条 D11 命令。判据全部写进
// `<pre data-probe-json>`：e2e 读 JSON 而不是在 DOM 里猜文本，形状错了会立刻炸而不是假绿。
const PANEL_JS = `// panel.js —— agent.session 全链路探针（由 verify_agent_session_electron.mjs 生成）
export const surface = {
  kind: ${JSON.stringify(PLUGIN_KIND)},

  apply({ host, React, surfaces }) {
    const { createElement: h, useEffect, useState } = React

    function Probe() {
      const theme = host.theme()
      const [facts, setFacts] = useState({ step: 'mount' })

      useEffect(() => {
        let alive = true
        const merge = (patch) => { if (alive) setFacts((f) => Object.assign({}, f, patch)) }
        ;(async () => {
          // ① 宿主边界：这两件事在**生产档真 preload**里必须是「不存在」，不是「藏起来」。
          merge({
            env: {
              desktop: typeof window.desktop,
              // 「隐藏」和「没有」是两件事：属性存在与否才是后者（D6 的边界）。
              hasDesktopKey: Object.prototype.hasOwnProperty.call(window, 'desktop'),
              claim: typeof window.__haiClaimHostApi,
              // 第二次 claim 必须是 null（D6 一次性交接）—— 直接在这里调用一次。
              secondClaim: typeof window.__haiClaimHostApi === 'function' ? window.__haiClaimHostApi() : 'missing',
            },
            hostKeys: Object.keys(host).sort(),
          })
          // ② agent.session：唯一不花 token 就能造出插件会话的入口（D4 的第一步）。
          try {
            const r = await host.command('agent.session', { title: ${JSON.stringify(SESSION_TITLE)} })
            merge({
              session: {
                ok: r.ok === true,
                session_id: typeof r.session_id === 'string' ? r.session_id : '',
                reason: typeof r.reason === 'string' ? r.reason : '',
                threw: false,
              },
            })
          } catch (e) {
            merge({ session: { ok: false, session_id: '', reason: '', threw: String(e && e.message || e) } })
          }
          // ③ D11：workspace_list 声明了也必须 denied（旧只读命令能读真实项目元数据）。
          try {
            const r = await host.command('workspace_list', {})
            merge({ workspaceList: { ok: r.ok === true, reason: typeof r.reason === 'string' ? r.reason : '' } })
          } catch (e) {
            merge({ workspaceList: { ok: true, reason: 'threw:' + String(e && e.message || e) } })
          }
          // ④ agent.run：**不调**。它要真模型（Provider/密钥/网络），本机不该依赖。
          merge({ step: 'done' })
        })()
        return () => { alive = false }
      }, [])

      return h(
        'div',
        {
          'data-surface': ${JSON.stringify(PLUGIN_ID)},
          style: { padding: 12, font: '12px/1.6 ui-monospace, monospace', color: theme.tokens.fg },
        },
        h('div', { style: { color: theme.tokens.accent, fontWeight: 600, marginBottom: 6 } }, 'agent.session 探针'),
        h('pre', { 'data-probe-json': ${JSON.stringify(PLUGIN_ID)}, style: { whiteSpace: 'pre-wrap', margin: 0 } },
          JSON.stringify(facts, null, 2)),
      )
    }

    surfaces.register({
      kind: ${JSON.stringify(PLUGIN_KIND)},
      group: 'plugin',
      order: 20,
      title: () => '会话探针',
      render: () => h(Probe),
    })
  },
}
`

const MANIFEST = {
  id: PLUGIN_ID,
  name: { zh: '会话探针', en: 'Session probe' },
  version: '1.0.0',
  api: 1,
  kind: PLUGIN_KIND,
  entry: 'panel.js',
  placement: 'plugin',
  // workspace_list 是**故意**声明的：D11 的判据是「声明了也 denied」。不声明它测到的
  // 只是 manifest 闸门（not_declared），那是另一条断言（见 test-surface-host T2）。
  commands: ['agent.session', 'workspace_list', 'composer.quote'],
  data: ['surfaces_changed'],
  entries: [{ id: 'probe', title: '会话探针', order: 10 }],
}

let code = 0
let app = null
let HOME = ''
let PLUGIN_DIR = ''
let WS = ''
const cleanup = () => {
  try { app?.close() } catch { /* 已退出 */ }
  for (const d of [HOME, PLUGIN_DIR, WS]) {
    if (!d) continue
    try { fs.rmSync(d, { recursive: true, force: true }) } catch { /* 尽力 */ }
  }
}
process.on('exit', cleanup)

try {
  console.log('\nT0 前置：dist 产物 + electron')
  const index = path.join(APP, 'dist', 'index.html')
  const mainJs = path.join(APP, 'dist-electron', 'main.js')
  const bridge = path.join(APP, 'dist', 'hai-bridge')
  if (!fs.existsSync(ELECTRON)) fail(`找不到 electron：${ELECTRON}`)
  // 缺产物就地构建（与 verify_external_surface_electron.mjs 同一条；不碰 5173/dev server）。
  if (!fs.existsSync(mainJs)) {
    console.log('  构建 dist-electron（npm run build:electron）…')
    const b = spawnSync('npm', ['run', 'build:electron'], { cwd: APP, encoding: 'utf8' })
    if (b.status !== 0) fail(`build:electron 失败：\n${b.stdout}\n${b.stderr}`)
  }
  if (!fs.existsSync(index)) {
    console.log('  构建渲染层（npx vite build）…')
    const b = spawnSync('npx', ['vite', 'build'], { cwd: APP, encoding: 'utf8' })
    if (b.status !== 0) fail(`vite build 失败：\n${b.stdout}\n${b.stderr}`)
  }
  if (!fs.existsSync(bridge)) {
    console.log('  构建桥（go build ./desktop/bridge）…')
    const b = spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], { cwd: REPO, encoding: 'utf8' })
    if (b.status !== 0) fail(`go build 失败：\n${b.stdout}\n${b.stderr}`)
  }
  check('三个前置产物齐（index.html / dist-electron/main.js / hai-bridge）',
    fs.existsSync(index) && fs.existsSync(mainJs) && fs.existsSync(bridge))

  // ---- T1：临时插件目录（真的 surface.json + panel.js，走真安装器的同一套校验）----
  console.log('\nT1 生成临时插件目录')
  PLUGIN_DIR = fs.mkdtempSync(path.join(os.tmpdir(), 'agent-e2e-plugin-'))
  fs.writeFileSync(path.join(PLUGIN_DIR, 'surface.json'), JSON.stringify(MANIFEST, null, 2) + '\n')
  fs.writeFileSync(path.join(PLUGIN_DIR, 'panel.js'), PANEL_JS)
  console.log(`  ${PLUGIN_DIR}`)
  check('surface.json + panel.js 就位',
    fs.existsSync(path.join(PLUGIN_DIR, 'surface.json')) && fs.existsSync(path.join(PLUGIN_DIR, 'panel.js')))

  // ---- T2：真安装器 → 临时 HOME（应用真会去读的那一份）----
  console.log('\nT2 真安装器 → 临时 HOME')
  HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'agent-e2e-home-'))
  fs.mkdirSync(path.join(HOME, '.go-code'), { recursive: true })
  fs.writeFileSync(path.join(HOME, '.go-code', 'settings.json'),
    JSON.stringify({ theme: 'dark', lang: 'zh' }, null, 2))
  const inst = spawnSync('go', ['test', './desktop/bridge', '-run', 'TestSurfaceInstallExternal',
    '-count=1', '-v', '-args', `-dir=${PLUGIN_DIR}`, `-home=${HOME}`], { cwd: REPO, encoding: 'utf8' })
  const instOut = (inst.stdout || '') + (inst.stderr || '')
  check('安装器回执 status=applied', /"status": "applied"/.test(instOut), instOut.split('\n').slice(-4).join(' | '))
  const indexPath = path.join(HOME, '.go-code', 'surfaces', 'index.json')
  const indexItems = fs.existsSync(indexPath) ? JSON.parse(fs.readFileSync(indexPath, 'utf8')).items : []
  check('index 里恰好这一条插件（enabled）',
    indexItems.length === 1 && indexItems[0].id === PLUGIN_ID && indexItems[0].enabled === true,
    JSON.stringify(indexItems))

  // ---- T3：启动真 Electron（真 preload / 真桥 / 真协议）----
  console.log('\nT3 启动真 Electron（HAI_DATA_DIR 多实例模式）')
  app = await _electron.launch({
    executablePath: ELECTRON, args: ['.'], cwd: APP,
    env: { ...process.env, HOME, HAI_DATA_DIR: HOME },
  })
  const win = await app.firstWindow()
  const errs = []
  win.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  await win.waitForLoadState('domcontentloaded')
  WS = fs.mkdtempSync(path.join(os.tmpdir(), 'agent-e2e-ws-'))
  await win.evaluate(([ws]) => {
    localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws }))
  }, [WS])
  await win.reload()
  await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: '主界面就绪' })

  // 左栏插件分组出现 = plugin_projects_list 已经有回执 = 桥真的在跑（agent.session 的前提）。
  // 这一步同时是「启动水合」的对照：此刻插件**还没有**会话，分组在、会话 0 条。
  console.log('\nT3a 左栏插件分组（启动水合，会话数应为 0）')
  await waitFor(() => win.locator('.plugin-group').count().then((n) => n > 0), { timeout: 60000, label: '左栏插件分组' })
  const before = await win.evaluate(() => {
    const g = document.querySelector('.plugin-group')
    const head = g?.querySelector('.ws-head')
    return {
      name: g?.querySelector('.wname')?.textContent?.trim() ?? '',
      count: g?.querySelector('.ws-count')?.textContent?.trim() ?? '',
      headTitle: head?.getAttribute('title') ?? '',
    }
  })
  console.log(`  分组：${JSON.stringify(before)}`)
  check('插件分组名 = manifest 的 name.zh', before.name === MANIFEST.name.zh, before.name)
  check('分组指向插件虚拟 home（plugin-sessions/<id>/home）',
    before.headTitle.includes(`plugin-sessions/${PLUGIN_ID}/home`), before.headTitle)
  check('启动那一刻还没有会话（会话数 0）', before.count === '0', before.count)

  // ---- T4：打开插件面板（＋ 菜单 → 真 hai-surface:// 协议 import panel.js）----
  console.log('\nT4 ＋ 菜单 → 面板（真协议）')
  const activityBtn = win.getByRole('button', { name: '链路', exact: true }).first()
  await waitFor(() => activityBtn.count().then((n) => n > 0), { label: '右栏图标条' })
  await activityBtn.click({ timeout: 5000 }).catch(() => {})
  await waitFor(() => win.locator('.rtabs-add').count().then((n) => n > 0), { label: '＋ 按钮' })
  await win.locator('.rtabs-add').click()
  const entrySel = `[data-plugin-entry="${PLUGIN_ID}/probe"]`
  await waitFor(() => win.locator(entrySel).count().then((n) => n > 0), { label: '＋ 菜单里的插件入口' })
  await win.locator(entrySel).first().click()

  // 面板的判据是异步写进 DOM 的：等到 step === 'done' 再读（读早了只会读到 mount 那一刻）。
  const probe = await waitFor(async () => {
    const raw = await win.locator('[data-probe-json]').first().textContent({ timeout: 1000 }).catch(() => '')
    if (!raw) return null
    try {
      const f = JSON.parse(raw)
      return f && f.step === 'done' ? f : null
    } catch { return null }
  }, { timeout: 60000, label: 'panel 探针结果' }).catch((e) => ({ error: String(e?.message ?? e) }))
  console.log(`  探针：${JSON.stringify(probe).slice(0, 400)}`)
  if (probe?.error) fail(`面板没写出探针结果：${probe.error}`)

  // ---- T5：宿主边界（生产档真 preload：不是隐藏，是没有）----
  console.log('\nT5 宿主边界（在生产档里由插件自己看到的那一份）')
  check('claim 函数是 preload 唯一暴露面', probe.env?.claim === 'function', String(probe.env?.claim))
  check('window.desktop 不存在（不是隐藏：连属性都没有）',
    probe.env?.desktop === 'undefined' && probe.env?.hasDesktopKey === false, JSON.stringify(probe.env))
  check('__haiClaimHostApi() 第二次调用返回 null（D6 一次性交接）',
    probe.env?.secondClaim === null, String(probe.env?.secondClaim))
  check('host keys 恰好 [React, command, onEvent, t, theme]',
    JSON.stringify(probe.hostKeys) === JSON.stringify(['React', 'command', 'onEvent', 't', 'theme']),
    JSON.stringify(probe.hostKeys))

  // ---- T6：agent.session 全链路（token → preload → main → Go → 磁盘）----
  console.log('\nT6 agent.session：插件拿到的 session_id')
  const sid = typeof probe.session?.session_id === 'string' ? probe.session.session_id : ''
  console.log(`  session_id = ${sid}`)
  check('回执 ok:true', probe.session?.ok === true, JSON.stringify(probe.session))
  check('session_id 以 s- 开头（Go 生成，不是渲染层编的）', /^s-[A-Za-z0-9]/.test(sid), sid)
  check('没有抛（host.command 永远回对象）', probe.session?.threw === false, String(probe.session?.threw))

  // ---- T7：落盘证据（plugin-sessions/<id>/sessions）----
  console.log('\nT7 磁盘：plugin-sessions/<id>/sessions')
  const pluginRoot = path.join(HOME, '.go-code', 'plugin-sessions', PLUGIN_ID)
  const sessionsDir = path.join(pluginRoot, 'sessions')
  const sessionJsonl = path.join(sessionsDir, `${sid}.jsonl`)
  check('plugin-sessions/<id>/ 建出来了', fs.existsSync(pluginRoot), pluginRoot)
  check('sessions/ 目录在（0700，Go 的 createIn 建的）',
    fs.existsSync(sessionsDir) && (fs.statSync(sessionsDir).mode & 0o777) === 0o700,
    fs.existsSync(sessionsDir) ? (fs.statSync(sessionsDir).mode & 0o777).toString(8) : '缺失')
  check('home/ 目录在（bash/file 的沙箱根）', fs.existsSync(path.join(pluginRoot, 'home')), PLUGIN_ID)
  const listing = (dir, depth = 2) => {
    if (!fs.existsSync(dir)) return []
    const walk = (d, prefix, left) => fs.readdirSync(d, { withFileTypes: true }).flatMap((e) => {
      const full = path.join(d, e.name)
      const rel = prefix + e.name + (e.isDirectory() ? '/' : ` (${fs.statSync(full).size}B)`)
      if (!e.isDirectory() || left <= 1) return [rel]
      return [rel, ...walk(full, rel, left - 1)]
    })
    return walk(dir, '', depth)
  }
  console.log(`  sessions/ 树：\n${listing(sessionsDir).map((l) => '    ' + l).join('\n')}`)
  // record 键锚在 pluginID 上（P1a §4.3 第 2 点）：插件会话的 record 根是
  // `<storeDir>/plugin-<id>/`，与项目会话的 `<appData>/checkpoints/<wsKey>/` 不是一套。
  check('record 目录 = sessions/plugin-<id>（record 与 jsonl 同目录，与项目会话分家）',
    fs.existsSync(path.join(sessionsDir, `plugin-${PLUGIN_ID}`)), `plugin-${PLUGIN_ID}`)
  check('会话在插件 store 里有自己的子树（sessions/<sid>/）',
    fs.existsSync(path.join(sessionsDir, sid)), sid)
  // 空会话**还没有** jsonl：Go 侧 Session 只在有消息后 persist（surface_agent_test.go 的
  // 同名断言写着「空会话此刻还没有 jsonl（Session 只在有消息后 persist）」）。本轮按约定
  // 不跑 agent.run（要真模型），所以这里钉的是**位置纪律**而不是「文件一定存在」：
  //   · 真落盘了就必须落在这个目录里（名字 = <sid>.jsonl）；
  //   · 别的会话的 jsonl 一条都不许出现；
  //   · 绝不许漏进**项目**会话目录（.go-code/sessions/<wsKey>/）。
  const strayFiles = fs.existsSync(sessionsDir)
    ? fs.readdirSync(sessionsDir).filter((n) => n.endsWith('.jsonl') && !n.startsWith(sid))
    : []
  check('sessions/ 里没有别的会话的 jsonl（位置纪律）', strayFiles.length === 0, JSON.stringify(strayFiles))
  check(`会话 jsonl 路径 = sessions/${sid}.jsonl（空会话允许尚未落盘）`,
    !fs.existsSync(sessionJsonl) || sessionJsonl === path.join(sessionsDir, `${sid}.jsonl`), sessionJsonl)
  const projSessionsDir = path.join(HOME, '.go-code', 'sessions')
  const leaked = fs.existsSync(projSessionsDir)
    ? fs.readdirSync(projSessionsDir, { recursive: true }).filter((n) => String(n).includes(sid))
    : []
  check('没有落进**项目**会话目录（.go-code/sessions/…）', leaked.length === 0, JSON.stringify(leaked))

  // ---- T8：D11（声明了也 denied）----
  console.log('\nT8 D11：workspace_list 声明了也必须 denied')
  check('workspace_list → ok:false', probe.workspaceList?.ok === false, JSON.stringify(probe.workspaceList))
  check('reason = denied', probe.workspaceList?.reason === 'denied', String(probe.workspaceList?.reason))

  // ---- T9：左栏即时刷新（A 的唯一真机证据）----
  //
  // 这里能成立**只可能**因为 `agent.session` 成功后宿主自己重拉了 plugin_projects_list：
  // 面板没有发任何会更新左栏的命令，启动水合早就跑完了（T3a 已断言当时是 0 条）。
  // 超时放宽到 15s：一次重拉要过 150ms 去抖 + 两条 IPC 往返。
  console.log('\nT9 左栏出现这条会话（A：会话即时刷新）')
  const row = await waitFor(async () => {
    const n = await win.locator('.plugin-group .ws-count').first().textContent().catch(() => '')
    if ((n || '').trim() !== '1') return null
    // 分组默认收起（isOpen 只看活跃工作区）→ 展开后才有会话行。
    const chev = win.locator('.plugin-group .ws-chev').first()
    if (await chev.getAttribute('aria-expanded') !== 'true') await chev.click().catch(() => {})
    const title = await win.locator('.plugin-group .session-title').first().textContent({ timeout: 1000 }).catch(() => '')
    return (title || '').trim() === SESSION_TITLE ? title.trim() : null
  }, { timeout: 15000, label: '左栏出现新会话' }).catch((e) => String(e?.message ?? e))
  check('左栏插件分组里出现这条会话（标题 = 面板传下去的 title）', row === SESSION_TITLE, String(row))
  check('全程零 pageerror', errs.length === 0, errs.join(' | '))

  await app.close()
  app = null
} catch (e) {
  if (e instanceof Precondition) {
    console.error(`\n前置不满足：${e.message}`)
    code = e.exitCode
  } else {
    console.error('\n全链路异常：', e)
    code = 1
  }
}

console.log('')
if (fails.length) {
  console.log(`❌ ${fails.length} 项失败：\n  - ${fails.join('\n  - ')}`)
  code = code || 1
} else if (code === 0) {
  console.log('✅ 全部通过（verify_agent_session_electron）')
}
try { await app?.close() } catch { /* 已退出 */ }
cleanup()
process.exit(code)
