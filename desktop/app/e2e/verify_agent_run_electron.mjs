#!/usr/bin/env node
// verify_agent_run_electron.mjs —— 插件 `agent.run` 的**真 Electron + 真桥 + 真 Provider 协议**全链路（P1e）。
//
// 姊妹探针 verify_agent_session_electron.mjs 停在「agent.session 建出一个空会话」：它刻意
// **不**调 agent.run，因为那要真模型（Provider / 密钥 / 外网）。这一条把最后一段补上，
// 用一个**本机**的 OpenAI 兼容 SSE 假服务替掉外网 —— 除了「上游模型是谁」以外不留替身：
//
//   1. 本地 http.createServer（127.0.0.1:0）：只处理 POST /v1/chat/completions，
//      按 OpenAI SSE 形状回两段文本 + finish_reason + usage + [DONE]，并记录请求体；
//   2. 临时 HOME 写 settings.json（provider.active=local，base_url 指向这个端口）；
//   3. 真安装器把临时插件装进那份 HOME；以 HOME=HAI_DATA_DIR 启动**真的**应用
//      （真 preload、真 claim、真 RightPanel、真 Go 桥进程）；
//   4. 面板先订阅 agent.delta/done/error/usage，再 `host.command('agent.run', {input:'ping'})`
//      —— **不带 session_id**：D4 的「新建会话」分支（也是 P1e 空会话落盘的入口）；
//   5. 断言：ack 先行（run_id/session_id 形状）→ delta/done 文本 → seq 单调 → 无 error
//      → 本地服务收到了 model=fake-model 且 body 含 ping 的请求 → transcript jsonl
//      里真有 user ping / assistant "local ok" → 左栏插件分组出现这条会话；
//   6. 收尾：close app、close server、删临时目录 —— 不留进程与端口。
//
// 为什么这条链路不能靠 Go 测试：`surface_agent_test.go` 用假 provider 钉住事件协议，
// provider 的 SSE 解析由 provider 包的单测钉住 —— 但「插件的 host.command 一路走到
// 真实 HTTP 请求、再回到插件手里的事件」没有任何一条测试真的跑过。这里跑的就是它。
//
// 用法：cd desktop/app && node e2e/verify_agent_run_electron.mjs
// 退出码：0 通过 / 1 断言失败 / 2 前置不满足（缺 dist 产物 / 装不上插件 / 起不来）。
//
// 多实例：HAI_DATA_DIR 让 Electron 跳过单实例锁（本机可能正跑着用户自己的实例）。
import { _electron } from 'playwright'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import http from 'node:http'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const REPO = path.resolve(APP, '../..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const PLUGIN_ID = 'agent-run-e2e'
const PLUGIN_KIND = `plugin:${PLUGIN_ID}`
// 上游模型名：settings.json 里**只有**这一个模型 ⇒ preferredModel()/defaultModelFor()
// 必须自动选它。写死一个不可能撞上真模型的名字，请求里出现它=确实来自本探针的配置。
const MODEL = 'fake-model'
const INPUT = 'ping'
const REPLY_A = 'local '
const REPLY_B = 'ok'
const REPLY = REPLY_A + REPLY_B

class Precondition extends Error {
  constructor(msg) { super(msg); this.name = 'Precondition'; this.exitCode = 2 }
}
// VerifyFailure：断言已经记进 fails，只是要停止后续步骤（不打栈，收尾统一报）。
class VerifyFailure extends Error {
  constructor(msg) { super(msg); this.name = 'VerifyFailure' }
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

// —— 本地 OpenAI 兼容 SSE 假服务 ——
//
// 只实现一个端点：真 Provider 只会在 POST /v1/chat/completions 上花 token（`/v1/models`
// 是设置页「拉取模型」用的，启动期可能被问一次，回个单模型清单即可 —— 那不算证据，
// 也不进 chatRequests）。
// 帧形状对齐 provider/upstream_cost_test.go 的参考流：文本 delta → finish_reason=stop
// 与 usage 同一帧 → [DONE]。agent.done 的文本来自 LLM 收尾内容，所以两段拼起来必须
// 恰好是 "local ok"。
const CHAT_PATH = '/v1/chat/completions'
const chatRequests = []
function startFakeProvider() {
  const server = http.createServer((req, res) => {
    const chunks = []
    // 客户端中途断开（桥被 close / run 被取消）不该把探针炸掉：这只是一次记录失败。
    req.on('error', () => {})
    res.on('error', () => {})
    req.on('data', (c) => chunks.push(c))
    req.on('end', () => {
      const body = Buffer.concat(chunks).toString('utf8')
      const url = (req.url || '').split('?')[0]
      if (req.method === 'POST' && url === CHAT_PATH) {
        let parsed = null
        try { parsed = JSON.parse(body) } catch { /* 记原文即可 */ }
        chatRequests.push({ url, body, parsed, at: Date.now() })
        res.writeHead(200, {
          'Content-Type': 'text/event-stream',
          'Cache-Control': 'no-cache',
          Connection: 'keep-alive',
        })
        const frame = (obj) => res.write('data: ' + JSON.stringify(obj) + '\n\n')
        const head = { id: 'chatcmpl-agent-run-e2e', object: 'chat.completion.chunk', model: MODEL }
        // ① 两段文本：分两帧，让「delta 合并窗口」有多段可合（合成一段也合法，断言只看拼接结果）。
        frame({ ...head, choices: [{ index: 0, delta: { role: 'assistant', content: REPLY_A } }] })
        frame({ ...head, choices: [{ index: 0, delta: { content: REPLY_B } }] })
        // ② finish + usage 同一帧，然后 [DONE]。
        frame({
          ...head,
          choices: [{ index: 0, delta: {}, finish_reason: 'stop' }],
          usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 },
        })
        res.write('data: [DONE]\n\n')
        res.end()
        return
      }
      if (req.method === 'GET' && url.endsWith('/models')) {
        res.writeHead(200, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ object: 'list', data: [{ id: MODEL, object: 'model' }] }))
        return
      }
      res.writeHead(404, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ error: { message: `no route: ${req.method} ${url}` } }))
    })
  })
  return new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address()
      resolve({ server, port })
    })
  })
}

// —— 临时插件：surface.json + panel.js（无构建步骤，浏览器直接 import）——
//
// 面板做四件事：① 记宿主边界事实；② **先**订阅四个 agent.* 事件；③ **后**发 agent.run
// （不带 session_id）；④ 把 ack / 事件序列 / 拼出的文本 / 终态演进写进
// `<pre data-run-json>`。判据读 JSON，不在 DOM 里猜文本。
const PANEL_JS = `// panel.js —— agent.run 全链路探针（由 verify_agent_run_electron.mjs 生成）
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
        // 事件是本探针的**主证据**：先订阅再发命令，否则第一条 delta 会落在订阅之前
        // （Go 侧回执先行、事件后到 —— 订阅必须在回执之前就位）。
        const seen = []
        const on = (type) => host.onEvent(type, (e) => {
          const d = (e && e.data) || {}
          seen.push({
            type: String(type),
            seq: typeof e.seq === 'number' ? e.seq : -1,
            run_id: typeof e.run_id === 'string' ? e.run_id : '',
            session_id: typeof e.session_id === 'string' ? e.session_id : '',
            text: typeof d.text === 'string' ? d.text : '',
            reason: typeof d.reason === 'string' ? d.reason : '',
            message: typeof d.message === 'string' ? d.message : '',
            input_tokens: typeof d.input_tokens === 'number' ? d.input_tokens : -1,
            output_tokens: typeof d.output_tokens === 'number' ? d.output_tokens : -1,
          })
          merge({ events: seen.slice() })
        })
        const offs = ['agent.delta', 'agent.done', 'agent.error', 'agent.usage'].map(on)

        ;(async () => {
          // ① 宿主边界：生产档真 preload 里这些必须是「不存在」，不是「藏起来」。
          merge({
            env: {
              desktop: typeof window.desktop,
              hasDesktopKey: Object.prototype.hasOwnProperty.call(window, 'desktop'),
              claim: typeof window.__haiClaimHostApi,
              secondClaim: typeof window.__haiClaimHostApi === 'function' ? window.__haiClaimHostApi() : 'missing',
            },
            hostKeys: Object.keys(host).sort(),
          })
          // ② agent.run：**不带 session_id** = D4 的新建会话分支（P1e 空会话落盘）。
          const ack = { ok: false, run_id: '', session_id: '', reason: '', threw: '' }
          try {
            const r = await host.command('agent.run', { input: ${JSON.stringify(INPUT)} })
            ack.ok = r.ok === true
            ack.run_id = typeof r.run_id === 'string' ? r.run_id : ''
            ack.session_id = typeof r.session_id === 'string' ? r.session_id : ''
            ack.reason = typeof r.reason === 'string' ? r.reason : ''
          } catch (e) {
            ack.threw = String(e && e.message || e)
          }
          merge({ ack })
          // ③ 等终态（done/error）：delta 是过程，done/error 才是这一轮结束的判据。
          // 上限 60s：本机假上游毫秒级返回，到这里还等不到就说明链路卡住了，宁可红。
          const terminal = () => seen.find((e) => e.type === 'agent.done' || e.type === 'agent.error')
          const deadline = Date.now() + 60000
          while (alive && !terminal() && Date.now() < deadline) {
            await new Promise((r) => setTimeout(r, 100))
          }
          const evs = seen.slice()
          const deltas = evs.filter((e) => e.type === 'agent.delta')
          const done = evs.find((e) => e.type === 'agent.done')
          const errs = evs.filter((e) => e.type === 'agent.error')
          merge({
            step: 'done',
            terminal: done ? 'done' : errs.length ? 'error' : 'timeout',
            deltaText: deltas.map((e) => e.text).join(''),
            doneText: done ? done.text : '',
            errorReason: errs.length ? errs[0].reason : '',
            events: evs,
          })
        })()

        return () => { alive = false; for (const off of offs) { try { off() } catch (e) { /* 已卸载 */ } } }
      }, [])

      return h(
        'div',
        {
          'data-surface': ${JSON.stringify(PLUGIN_ID)},
          style: { padding: 12, font: '12px/1.6 ui-monospace, monospace', color: theme.tokens.fg },
        },
        h('div', { style: { color: theme.tokens.accent, fontWeight: 600, marginBottom: 6 } }, 'agent.run 探针'),
        h('pre', { 'data-run-json': ${JSON.stringify(PLUGIN_ID)}, style: { whiteSpace: 'pre-wrap', margin: 0 } },
          JSON.stringify(facts, null, 2)),
      )
    }

    surfaces.register({
      kind: ${JSON.stringify(PLUGIN_KIND)},
      group: 'plugin',
      order: 20,
      title: () => '运行探针',
      render: () => h(Probe),
    })
  },
}
`

const MANIFEST = {
  id: PLUGIN_ID,
  name: { zh: '运行探针', en: 'Run probe' },
  version: '1.0.0',
  api: 1,
  kind: PLUGIN_KIND,
  entry: 'panel.js',
  placement: 'plugin',
  // 四个 Agent 命令全声明：本探针只调 agent.run，但「声明 ∩ 宿主表」的闸门要看齐全，
  // 少一个都会让日后加 cancel/tool.result 的探针重新踩 manifest 的坑。
  commands: ['agent.session', 'agent.run', 'agent.cancel', 'tool.result'],
  // usage 也声明：它是成功 run 的必经终态之一（finish 里紧跟 delta 投出），
  // 订阅到 = 收尾帧的 usage 真的从 provider 走到了插件手里。
  data: ['agent.delta', 'agent.done', 'agent.error', 'agent.usage'],
  entries: [{ id: 'probe', title: '运行探针', order: 10 }],
}

let code = 0
let app = null
let provider = null
let HOME = ''
let PLUGIN_DIR = ''
let WS = ''
const cleanup = () => {
  try { app?.close() } catch { /* 已退出 */ }
  try {
    provider?.server.closeAllConnections?.()
    provider?.server.close()
  } catch { /* 已关闭 */ }
  for (const d of [HOME, PLUGIN_DIR, WS]) {
    if (!d) continue
    try { fs.rmSync(d, { recursive: true, force: true }) } catch { /* 尽力 */ }
  }
}
process.on('exit', cleanup)

// userTexts 请求体里 role=user 的文本块（字符串 content 或 [{type:'text',text}] 两种线形）。
// 只判「原文里出现过 ping」不够：系统提示词里随手一个 typing 就能让断言假绿 ——
// 这一条要的是「插件的 input 真的作为**用户消息**发给了上游」。
function userTexts(body) {
  let v
  try { v = JSON.parse(body) } catch { return [] }
  const out = []
  for (const m of Array.isArray(v?.messages) ? v.messages : []) {
    if (m?.role !== 'user') continue
    if (typeof m.content === 'string') out.push(m.content)
    else if (Array.isArray(m.content)) {
      for (const c of m.content) if (c && c.type === 'text' && typeof c.text === 'string') out.push(c.text)
    }
  }
  return out
}

const t0 = Date.now()
try {
  console.log('\nT0 前置：本地假上游 + dist 产物 + electron')
  provider = await startFakeProvider()
  console.log(`  假上游：http://127.0.0.1:${provider.port}/v1（只认 POST ${CHAT_PATH}）`)
  check('本地假上游已监听（127.0.0.1 随机端口）', provider.port > 0, String(provider.port))

  const index = path.join(APP, 'dist', 'index.html')
  const mainJs = path.join(APP, 'dist-electron', 'main.js')
  const bridge = path.join(APP, 'dist', 'hai-bridge')
  if (!fs.existsSync(ELECTRON)) fail(`找不到 electron：${ELECTRON}`)
  // electron 与 Go 桥**始终重建**：这两条正是本探针要验的边界（preload token / surface_agent
  // / SSE 解析）。只在缺失时构建会让「源码已改、dist-electron 还是旧 preload」的全链跑法
  // 红成 agent.run=denied 或事件不来（姊妹探针在完整 npm test 里真踩过）。
  console.log('  构建 dist-electron（npm run build:electron）…')
  {
    const b = spawnSync('npm', ['run', 'build:electron'], { cwd: APP, encoding: 'utf8' })
    if (b.status !== 0) fail(`build:electron 失败：\n${b.stdout}\n${b.stderr}`)
  }
  if (!fs.existsSync(index)) {
    console.log('  构建渲染层（npx vite build）…')
    const b = spawnSync('npx', ['vite', 'build'], { cwd: APP, encoding: 'utf8' })
    if (b.status !== 0) fail(`vite build 失败：\n${b.stdout}\n${b.stderr}`)
  }
  console.log('  构建桥（go build ./desktop/bridge）…')
  {
    const b = spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], { cwd: REPO, encoding: 'utf8' })
    if (b.status !== 0) fail(`go build 失败：\n${b.stdout}\n${b.stderr}`)
  }
  check('三个前置产物齐（index.html / dist-electron/main.js / hai-bridge）',
    fs.existsSync(index) && fs.existsSync(mainJs) && fs.existsSync(bridge))

  // ---- T1：临时插件目录（真的 surface.json + panel.js，走真安装器的同一套校验）----
  console.log('\nT1 生成临时插件目录')
  PLUGIN_DIR = fs.mkdtempSync(path.join(os.tmpdir(), 'agent-run-e2e-plugin-'))
  fs.writeFileSync(path.join(PLUGIN_DIR, 'surface.json'), JSON.stringify(MANIFEST, null, 2) + '\n')
  fs.writeFileSync(path.join(PLUGIN_DIR, 'panel.js'), PANEL_JS)
  console.log(`  ${PLUGIN_DIR}`)
  check('surface.json + panel.js 就位',
    fs.existsSync(path.join(PLUGIN_DIR, 'surface.json')) && fs.existsSync(path.join(PLUGIN_DIR, 'panel.js')))

  // ---- T2：临时 HOME（settings.json 指向本地假上游）+ 真安装器 ----
  console.log('\nT2 临时 HOME：settings.json 的 provider 段 + 真安装器')
  HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'agent-run-e2e-home-'))
  fs.mkdirSync(path.join(HOME, '.go-code'), { recursive: true })
  // 只配一个模型：defaultModel()/defaultModelFor() 的「排序取首个」就会自动选它 —— 这是
  // 「不靠 switch_model 也能跑」的前提（真机上用户选过的模型走 lastModel，本探针不涉及）。
  const settings = {
    theme: 'dark',
    lang: 'zh',
    provider: {
      active: 'local',
      local: {
        base_url: `http://127.0.0.1:${provider.port}/v1`,
        protocol: 'chat_completions',
        api_key: 'sk-local',
        models: { [MODEL]: 128000 },
        max_tokens: { [MODEL]: 4096 },
      },
    },
  }
  fs.writeFileSync(path.join(HOME, '.go-code', 'settings.json'), JSON.stringify(settings, null, 2) + '\n')
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
  try {
    app = await _electron.launch({
      executablePath: ELECTRON, args: ['.'], cwd: APP,
      env: { ...process.env, HOME, HAI_DATA_DIR: HOME },
    })
  } catch (e) {
    fail(`Electron 起不来：${e?.message ?? e}`)
  }
  const win = await app.firstWindow()
  const errs = []
  win.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  await win.waitForLoadState('domcontentloaded')
  WS = fs.mkdtempSync(path.join(os.tmpdir(), 'agent-run-e2e-ws-'))
  await win.evaluate(([ws]) => {
    localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws }))
  }, [WS])
  await win.reload()
  await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: '主界面就绪' })

  // 左栏插件分组出现 = plugin_projects_list 已经有回执 = 桥真的在跑（agent.run 的前提）。
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

  // 面板的判据是异步写进 DOM 的：等到 step === 'done' 再读（read 早了只有 mount 那一刻）。
  // 90s：假上游毫秒级返回，余量留给桥启动 + 标题生成 + 左栏水合。
  // 等不到 = 断言失败（不是前置不满足）：那是链路真的卡住/事件没到，必须红在断言里。
  let probe = null
  try {
    probe = await waitFor(async () => {
      const raw = await win.locator('[data-run-json]').first().textContent({ timeout: 1000 }).catch(() => '')
      if (!raw) return null
      try {
        const f = JSON.parse(raw)
        return f && f.step === 'done' ? f : null
      } catch { return null }
    }, { timeout: 90000, label: 'panel 探针结果' })
  } catch (e) {
    check('面板在 90s 内写出 step=done 的探针结果', false, String(e?.message ?? e))
    throw new VerifyFailure('panel 探针超时')
  }
  console.log(`  ack：${JSON.stringify(probe.ack)}`)
  console.log(`  terminal=${probe.terminal} deltaText=${JSON.stringify(probe.deltaText)} doneText=${JSON.stringify(probe.doneText)}`)
  console.log(`  events：${JSON.stringify((probe.events || []).map((e) => [e.type, e.seq]))}`)

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

  // ---- T6：agent.run 回执（回执先行，事件后到）----
  console.log('\nT6 agent.run：回执形状')
  const ack = probe.ack || {}
  const runId = typeof ack.run_id === 'string' ? ack.run_id : ''
  const sid = typeof ack.session_id === 'string' ? ack.session_id : ''
  console.log(`  run_id = ${runId}\n  session_id = ${sid}`)
  check('回执 ok:true', ack.ok === true, JSON.stringify(ack))
  check('没有抛（host.command 永远回对象）', ack.threw === '', String(ack.threw))
  check('run_id 以 sr- 开头（Go 生成，不是渲染层编的）', /^sr-[A-Za-z0-9]/.test(runId), runId)
  check('session_id 以 s- 开头（不带 session_id ⇒ D4 新建）', /^s-[A-Za-z0-9]/.test(sid), sid)

  // ---- T7：provider 流式 → 事件（delta → usage → done）----
  console.log('\nT7 事件：delta 拼接 / done 一致 / seq 单调 / 无 error')
  const events = Array.isArray(probe.events) ? probe.events : []
  const deltas = events.filter((e) => e.type === 'agent.delta')
  const dones = events.filter((e) => e.type === 'agent.done')
  const errEvents = events.filter((e) => e.type === 'agent.error')
  const usages = events.filter((e) => e.type === 'agent.usage')
  const seqs = events.map((e) => e.seq)
  check('终态是 done（不是 error/timeout）', probe.terminal === 'done', String(probe.terminal))
  check('收到 ≥1 条 agent.delta', deltas.length >= 1, String(deltas.length))
  check(`delta 拼接包含 ${JSON.stringify(REPLY)}`, String(probe.deltaText).includes(REPLY), JSON.stringify(probe.deltaText))
  check('done 文本与 delta 拼接完全一致', probe.doneText === probe.deltaText,
    `done=${JSON.stringify(probe.doneText)} delta=${JSON.stringify(probe.deltaText)}`)
  check('恰好 1 条 agent.done', dones.length === 1, String(dones.length))
  check('零 agent.error', errEvents.length === 0, JSON.stringify(errEvents.map((e) => [e.reason, e.message])))
  check('errorReason 为空', !probe.errorReason, String(probe.errorReason))
  check('seq 严格单调递增（每条 ≥1）',
    seqs.length > 0 && seqs[0] >= 1 && seqs.every((s, i) => i === 0 || s > seqs[i - 1]), JSON.stringify(seqs))
  check('每条事件的 run_id / session_id 都是本次回执那一对',
    events.length > 0 && events.every((e) => e.run_id === runId && e.session_id === sid),
    JSON.stringify(events.map((e) => [e.run_id, e.session_id])))
  // finish 里 usage 紧跟 delta、先于 done（协议顺序），且 token 数非负 —— 收尾帧真的被解析了。
  check('收到 agent.usage（收尾帧的 usage 走到了插件）', usages.length === 1, String(usages.length))
  const usage = usages[0] || {}
  check('usage 的 input/output tokens 是非负数字',
    usage.input_tokens >= 0 && usage.output_tokens >= 0,
    JSON.stringify([usage.input_tokens, usage.output_tokens]))
  if (usages.length && dones.length) {
    check('usage 排在 done 之前（delta … usage … done 的顺序是协议的一部分）', usage.seq < dones[0].seq,
      `${usage.seq} < ${dones[0].seq}`)
  }

  // ---- T8：本地上游真的被请求过（model / 正文 / 流式）----
  console.log('\nT8 本地假上游收到的请求')
  const sample = chatRequests[0]
  check(`至少收到 1 次 POST ${CHAT_PATH}`, chatRequests.length >= 1, String(chatRequests.length))
  if (sample?.parsed) {
    console.log(`  样例：method=POST url=${sample.url} model=${JSON.stringify(sample.parsed.model)} stream=${JSON.stringify(sample.parsed.stream)} stream_options=${JSON.stringify(sample.parsed.stream_options)}`)
    const msgs = Array.isArray(sample.parsed.messages) ? sample.parsed.messages : []
    console.log(`  messages=${msgs.length} 条（roles=${JSON.stringify(msgs.map((m) => m.role))}）`)
    console.log(`  user 文本：${JSON.stringify(userTexts(sample.body).map((t) => t.slice(0, 80)))}`)
    console.log(`  body 片段：${sample.body.slice(0, 240).replace(/\s+/g, ' ')}…`)
  }
  check(`每次请求的 model 都是 ${JSON.stringify(MODEL)}（defaultModel 自动选了唯一模型）`,
    chatRequests.length > 0 && chatRequests.every((r) => r.parsed?.model === MODEL),
    JSON.stringify(chatRequests.map((r) => r.parsed?.model)))
  check('请求路径 = /v1/chat/completions（base_url 没有重复拼 /v1）',
    chatRequests.every((r) => r.url === CHAT_PATH), JSON.stringify(chatRequests.map((r) => r.url)))
  check(`插件的 input 作为 user 消息进了请求体（${JSON.stringify(INPUT)}）`,
    chatRequests.some((r) => userTexts(r.body).some((t) => t.trim() === INPUT)),
    JSON.stringify(chatRequests.map((r) => userTexts(r.body).map((t) => t.slice(0, 40)))))
  check('请求是流式（stream:true）', chatRequests.length > 0 && chatRequests.every((r) => r.parsed?.stream === true),
    JSON.stringify(chatRequests.map((r) => r.parsed?.stream)))

  // ---- T9：transcript jsonl（真落盘，按真实行格式断言）----
  //
  // 格式（session/store_file.go）：每行一个 fileRecord ——
  //   {"type":"history","messages":[…]}   ← append-only 历史（桌面档只留第一条：标题来源）
  //   {"type":"state","state":{"messages":[…],"usage":…,"cost":…}}  ← 每个轮次检查点
  // 所以断言不是「文件在」，而是「最后一条 state 的 messages 里有 user ping + assistant local ok」。
  console.log('\nT9 transcript jsonl：<HOME>/.go-code/plugin-sessions/<id>/sessions/<sid>.jsonl')
  const pluginRoot = path.join(HOME, '.go-code', 'plugin-sessions', PLUGIN_ID)
  const sessionsDir = path.join(pluginRoot, 'sessions')
  const sessionJsonl = path.join(sessionsDir, `${sid}.jsonl`)
  const readRows = () => {
    if (!fs.existsSync(sessionJsonl)) return []
    return fs.readFileSync(sessionJsonl, 'utf8').split('\n').filter(Boolean).flatMap((line) => {
      try { return [JSON.parse(line)] } catch { return [] }
    })
  }
  // 落盘是轮次检查点写的，done 之后可能还差一次 flush —— 等它出现再断言（≤15s）。
  const transcript = await waitFor(() => {
    const rows = readRows()
    const state = [...rows].reverse().find((r) => r.type === 'state')
    const msgs = state?.state?.messages ?? []
    const texts = (role) => msgs.filter((m) => m.role === role)
      .flatMap((m) => (Array.isArray(m.content) ? m.content : []))
      .filter((c) => c.type === 'text')
      .map((c) => String(c.content ?? ''))
    return texts('user').some((t) => t.includes(INPUT)) && texts('assistant').some((t) => t.includes(REPLY))
      ? { rows, state, user: texts('user'), assistant: texts('assistant') }
      : null
  }, { timeout: 15000, label: 'jsonl 里的 user ping + assistant "local ok"' }).catch(() => null)
  check('jsonl 存在（P1e：run 之后空会话变成有内容的会话）', fs.existsSync(sessionJsonl), sessionJsonl)
  if (transcript) {
    console.log(`  ${sessionJsonl}（${fs.statSync(sessionJsonl).size}B / ${transcript.rows.length} 行）`)
    console.log(`  行类型：${JSON.stringify(transcript.rows.map((r) => r.type))}`)
    console.log(`  state.messages.user=${JSON.stringify(transcript.user)}`)
    console.log(`  state.messages.assistant=${JSON.stringify(transcript.assistant)}`)
    console.log(`  state.usage=${JSON.stringify(transcript.state?.state?.usage ?? null)}`)
  } else if (fs.existsSync(sessionJsonl)) {
    const rows = readRows()
    console.log(`  ⚠️  ${sessionJsonl} 有 ${rows.length} 行但没匹配到：${JSON.stringify(rows).slice(0, 600)}`)
  }
  check('最后一条 state 的 messages 含 user 的 ping',
    !!transcript && transcript.user.some((t) => t.includes(INPUT)), JSON.stringify(transcript?.user ?? null))
  check(`最后一条 state 的 messages 含 assistant 的 ${JSON.stringify(REPLY)}`,
    !!transcript && transcript.assistant.some((t) => t.includes(REPLY)), JSON.stringify(transcript?.assistant ?? null))
  // 标题来源：桌面档（keep_history=false）只写**第一条** history 行，冷磁盘会话标题读它。
  const firstHistory = (transcript?.rows ?? []).find((r) => r.type === 'history')
  check('第一条 history 行仍在（冷磁盘标题的来源，P1e 没有被裁掉）',
    !!firstHistory && JSON.stringify(firstHistory).includes(INPUT), JSON.stringify(firstHistory ?? null).slice(0, 200))
  // 位置纪律：别漏进项目会话目录，也别在插件 sessions/ 下混进别的会话。
  const stray = fs.existsSync(sessionsDir)
    ? fs.readdirSync(sessionsDir).filter((n) => n.endsWith('.jsonl') && `${n}` !== `${sid}.jsonl`)
    : []
  check('sessions/ 里没有别的会话的 jsonl', stray.length === 0, JSON.stringify(stray))
  const projSessionsDir = path.join(HOME, '.go-code', 'sessions')
  const leaked = fs.existsSync(projSessionsDir)
    ? fs.readdirSync(projSessionsDir, { recursive: true }).filter((n) => String(n).includes(sid))
    : []
  check('没有落进**项目**会话目录（.go-code/sessions/…）', leaked.length === 0, JSON.stringify(leaked))

  // ---- T10：左栏插件分组出现这条会话 ----
  //
  // 这一行能出现只可能因为两件事都成立：① Go 真的建了会话（jsonl 已落盘）；②
  // agent.run 成功后 loader 请宿主重拉了 plugin_projects_list（T3a 那一刻还是 0 条）。
  // 标题：无手动标题 ⇒ 「第一条 user 文本（ping）」或「AI 标题（假上游回的是 local ok，
  // ≤15 字符，原样采用）」—— 两者都只可能来自这一次 run。
  console.log('\nT10 左栏插件分组出现这条会话')
  const row = await waitFor(async () => {
    const n = await win.locator('.plugin-group .ws-count').first().textContent().catch(() => '')
    if ((n || '').trim() !== '1') return null
    // 分组默认收起（isOpen 只看活跃工作区）→ 展开后才有会话行。
    const chev = win.locator('.plugin-group .ws-chev').first()
    if (await chev.getAttribute('aria-expanded') !== 'true') await chev.click().catch(() => {})
    const title = await win.locator('.plugin-group .session-title').first().textContent({ timeout: 1000 }).catch(() => '')
    return [INPUT, REPLY].includes((title || '').trim()) ? title.trim() : null
  }, { timeout: 30000, label: '左栏出现这条会话（标题 = ping 或 AI 标题 local ok）' }).catch((e) => String(e?.message ?? e))
  check('左栏插件分组里出现这条会话（标题 = ping 或 local ok）', [INPUT, REPLY].includes(String(row)), String(row))
  check('全程零 pageerror', errs.length === 0, errs.join(' | '))

  await app.close()
  app = null
} catch (e) {
  if (e instanceof Precondition) {
    console.error(`\n前置不满足：${e.message}`)
    code = e.exitCode
  } else if (e instanceof VerifyFailure) {
    console.error(`\n断言失败，后续步骤跳过：${e.message}`)
    code = 1
  } else {
    console.error('\n全链路异常：', e)
    code = 1
  }
}

console.log('')
const elapsed = ((Date.now() - t0) / 1000).toFixed(1)
if (fails.length) {
  console.log(`❌ ${fails.length} 项失败（${elapsed}s）：\n  - ${fails.join('\n  - ')}`)
  code = code || 1
} else if (code === 0) {
  console.log(`✅ 全部通过（verify_agent_run_electron，${elapsed}s）`)
}
try { await app?.close() } catch { /* 已退出 */ }
cleanup()
process.exit(code)
