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

// P2b：模型侧工具全名 = plugin__<id>__<name>（Go 侧 §5.3 的拼法，本探针逐字使用）。
const TOOL = {
  echo: `plugin__${PLUGIN_ID}__echo`,
  needs_panel: `plugin__${PLUGIN_ID}__needs_panel`,
  write_note: `plugin__${PLUGIN_ID}__write_note`,
}
// 三个工具场景的 input（= 上游看到的最后一条 user 文本 = 假服务分流键）。
const SCENE_USE_TOOL = 'use-tool'
const SCENE_CLOSED_PANEL = 'closed-panel'
const SCENE_WRITE_TOOL = 'write-tool'
const SCENE_CANCEL = 'cancel'
// P3a T15：用户从宿主 UI 驱动插件会话（不是 agent.run）。
const SCENE_USER_STATE = 'user-state'
// closed-panel 的响应延迟：面板必须**先**关掉，宿主门禁才是一次真实的门禁。
const CLOSED_PANEL_DELAY_MS = 2500
// cancel 的响应延迟：留足一个「请求已到上游、run 仍在跑」的窗口给取消按钮。
const CANCEL_DELAY_MS = 4000

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

// SCENES：一个场景 = 一轮工具调用（可选）+ 收尾文本帧。
//
// `echo` 回的结果是 {echo:"hello"}（T11 在第二次请求里断言 hello）；
// `needs_panel` 声明 requires_panel:true（T12 的门禁对象）；`write_note` 是 read_only:false
//（T13 的审批对象）。
const SCENES = {
  [INPUT]: { text: [REPLY_A, REPLY_B] },
  [SCENE_USE_TOOL]: {
    toolCall: { id: 'call_1', name: TOOL.echo, args: '{"text":"hello"}' },
    text: ['tool ok'],
  },
  [SCENE_CLOSED_PANEL]: {
    toolCall: { id: 'call_2', name: TOOL.needs_panel, args: '{}' },
    text: ['panel blocked'],
    delayMs: CLOSED_PANEL_DELAY_MS,
  },
  [SCENE_WRITE_TOOL]: {
    toolCall: { id: 'call_3', name: TOOL.write_note, args: '{"text":"note"}' },
    text: ['write ok'],
  },
  // T14：请求已到上游但迟迟不回，专门给 agent.cancel 制造「在跑」窗口。
  // 没有被取消时它会在 CANCEL_DELAY_MS 后回 done；取消成功则应在此之前收到
  // agent.error{cancelled}，且**没有** done —— 两者是互斥终态。
  [SCENE_CANCEL]: { text: ['cancel should not complete'], delayMs: CANCEL_DELAY_MS },
  [SCENE_USER_STATE]: { text: ['host state ok'] },
}

// contentTexts：一条消息的文本块（字符串 content 与 [{type:'text',text}] 两种线形都认）。
function contentTexts(m) {
  if (typeof m?.content === 'string') return [m.content]
  if (Array.isArray(m?.content)) {
    return m.content.filter((c) => c && typeof c.text === 'string').map((c) => String(c.text))
  }
  return []
}

// sceneOf：最后一条 user 文本 = 本次 run 的 input（插件 agent.run 的原文）。
function sceneOf(parsed) {
  const msgs = Array.isArray(parsed?.messages) ? parsed.messages : []
  for (let i = msgs.length - 1; i >= 0; i--) {
    if (msgs[i]?.role !== 'user') continue
    const t = contentTexts(msgs[i])[0]
    if (t && t.trim()) return t.trim()
  }
  return ''
}

// hasToolMessage：这次请求里，**最后一条 user 之后**是否已有 role:"tool"。
//
// 不能判「全历史里有没有 tool」：P2b 的三个场景复用同一个会话（D4），T11 的
// echo 结果会一直留在 messages 里。若按全历史判，T12/T13 的第一轮就会被误判成
// 工具轮第二次请求，假上游直接回文本 —— 工具调用根本不会发生（本探针真踩过）。
function hasToolMessage(parsed) {
  const msgs = Array.isArray(parsed?.messages) ? parsed.messages : []
  let lastUser = -1
  for (let i = msgs.length - 1; i >= 0; i--) {
    if (msgs[i]?.role === 'user') { lastUser = i; break }
  }
  if (lastUser < 0) return false
  return msgs.slice(lastUser + 1).some((m) => m?.role === 'tool')
}

// toolTexts：role:"tool" 消息的文本（T11 断言 hello、T12 断言 panel_not_open 都读它）。
function toolTexts(body) {
  let v
  try { v = JSON.parse(body) } catch { return [] }
  const out = []
  for (const m of Array.isArray(v?.messages) ? v.messages : []) {
    if (m?.role !== 'tool') continue
    out.push(...contentTexts(m))
  }
  return out
}
function startFakeProvider() {
  const server = http.createServer((req, res) => {
    const chunks = []
    // 客户端中途断开（桥被 close / run 被取消）不该把探针炸掉：这只是一次记录失败。
    req.on('error', () => {})
    res.on('error', () => {})
    // 取消会让客户端先断开；清掉延迟写，别让 4s 定时器在探针收尾后还挂着。
    let timer = null
    res.on('close', () => { if (timer) clearTimeout(timer) })
    req.on('data', (c) => chunks.push(c))
    req.on('end', () => {
      const body = Buffer.concat(chunks).toString('utf8')
      const url = (req.url || '').split('?')[0]
      if (req.method === 'POST' && url === CHAT_PATH) {
        let parsed = null
        try { parsed = JSON.parse(body) } catch { /* 记原文即可 */ }
        // 场景按**最后一条 user 文本**分流：它就是插件 host.command('agent.run', {input}) 的原文。
        // round2 = 请求里已经有 role:"tool" 消息 ⇒ 这是同一个 run 的第二次请求（工具结果回模型）。
        const scene = sceneOf(parsed)
        const round2 = hasToolMessage(parsed)
        const rec = { url, body, parsed, scene, round2, at: Date.now(), respondedAt: 0 }
        chatRequests.push(rec)
        res.writeHead(200, {
          'Content-Type': 'text/event-stream',
          'Cache-Control': 'no-cache',
          Connection: 'keep-alive',
        })
        const frame = (obj) => {
          try { res.write('data: ' + JSON.stringify(obj) + '\n\n') } catch { /* 客户端已断 */ }
        }
        const head = { id: 'chatcmpl-agent-run-e2e', object: 'chat.completion.chunk', model: MODEL }
        // 未声明的场景按 ping 回：探针跑歪时也要有终态，不能吊死在一个永远不结束的流上。
        const spec = SCENES[scene] || SCENES.ping
        const frames = []
        if (spec.toolCall && !round2) {
          // 工具轮：id/name/arguments **一帧给齐**（真实网关的常见形态；拆帧由
          // provider/toolcall_merge.go 负责，那是它自己的单测面）。
          frames.push({
            ...head,
            choices: [{ index: 0, delta: { role: 'assistant', tool_calls: [{
              index: 0, id: spec.toolCall.id, type: 'function',
              function: { name: spec.toolCall.name, arguments: spec.toolCall.args },
            }] } }],
          })
          frames.push({
            ...head,
            choices: [{ index: 0, delta: {}, finish_reason: 'tool_calls' }],
            usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 },
          })
        } else {
          for (const text of spec.text) {
            frames.push({ ...head, choices: [{ index: 0, delta: { role: 'assistant', content: text } }] })
          }
          frames.push({
            ...head,
            choices: [{ index: 0, delta: {}, finish_reason: 'stop' }],
            usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 },
          })
        }
        const write = () => {
          if (res.writableEnded || res.destroyed) return
          rec.respondedAt = Date.now()
          for (const f of frames) frame(f)
          try { res.write('data: [DONE]\n\n'); res.end() } catch { /* 已断 */ }
        }
        // closed-panel 要等面板关掉、cancel 要等用户按下取消，两者都需要延迟首轮。
        if (spec.delayMs && !round2) timer = setTimeout(write, spec.delayMs)
        else write()
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
const PANEL_JS = `// panel.js —— agent.run + 插件工具（tool.call/tool.result）探针（由 verify_agent_run_electron.mjs 生成）
//
// 两个作用域，各管一件事，缺一不可：
//   · **apply 作用域**：tool.call / agent.done 的订阅活到插件卸载（关标签不退订）。
//     T12 要证明的正是「面板关了，宿主仍然拦得住」—— 所以证据必须挂在标签之外的地方
//     （globalThis），而不是某个组件的 useState 里（那些会随标签关闭一起消失）。
//   · Probe 组件：P1e 的 ping 探针（订阅 → agent.run → 拼 delta → 终态）+ 三个按钮。
export const surface = {
  kind: ${JSON.stringify(PLUGIN_KIND)},

  apply({ host, React, surfaces }) {
    const { createElement: h, useEffect, useState } = React
    const str = (v) => (typeof v === 'string' ? v : '')
    const num = (v) => (typeof v === 'number' ? v : -1)

    // 证据容器挂 globalThis：面板标签关掉之后它们还在，Node 侧 win.evaluate 直接读。
    const calls = (globalThis.__agentToolCalls = globalThis.__agentToolCalls || [])
    const runs = (globalThis.__agentToolRuns = globalThis.__agentToolRuns || [])
    const terminals = (globalThis.__agentRunTerminals = globalThis.__agentRunTerminals || [])
    const states = (globalThis.__agentStates = globalThis.__agentStates || [])
    // 复用 ping 建出来的那个会话（D4）：后续场景不各建一条会话，左栏也不要长出多行。
    // lastRunId 给 T14 的 cancel 按钮用：它只取消最近一次 agent.run 的回执 id。
    const shared = (globalThis.__agentSharedSession = globalThis.__agentSharedSession || { sessionId: '', lastRunId: '' })

    // P3a：用户从宿主 UI 直接驱动插件会话时，插件也必须看到会话级状态。
    // 放在 apply 作用域（不随面板标签关闭而消失），T15 用它做判据。
    host.onEvent('agent.state', (e) => {
      const d = (e && e.data) || {}
      states.push({
        status: str(d.status),
        session_id: str(d.session_id),
        run_id: str(d.run_id),
        seq: num(d.seq),
        reason: str(d.reason),
        message: str(d.message),
        at: Date.now(),
      })
    })

    // ① tool.call（apply 作用域）：原样记下 data，再回 tool.result。
    //    记录里**没有** needs_panel 这一条 = T12 的门禁证据（宿主挡在投递之前，插件没进过回调）。
    host.onEvent('tool.call', (e) => {
      const d = (e && e.data) || {}
      const args = d.args && typeof d.args === 'object' ? d.args : {}
      const rec = {
        tool: str(d.tool),
        call_id: str(d.call_id),
        read_only: d.read_only === true,
        requires_panel: d.requires_panel === true,
        deadline_ms: num(d.deadline_ms),
        args,
        run_id: str(e.run_id),
        session_id: str(e.session_id),
        seq: num(e.seq),
        at: Date.now(),
        replied: false,
      }
      calls.push(rec)
      // echo 回 {echo:text}（T11 在第二次请求里读 hello）；其余回 {ok:true} 带最小结果。
      let result = { ok: true }
      if (rec.tool === 'echo') result = { echo: str(args.text) }
      if (rec.tool === 'write_note') result = { note: str(args.text) }
      Promise.resolve(host.command('tool.result', { call_id: rec.call_id, ok: true, result }))
        .then((r) => { rec.replied = !!r && r.ok === true })
        .catch((err) => { rec.replyError = String((err && err.message) || err) })
    })

    // ② agent.done（apply 作用域）：终态文本是 T11/T12/T13 的判据 —— 面板关着也必须留下。
    host.onEvent('agent.done', (e) => {
      const d = (e && e.data) || {}
      terminals.push({
        run_id: str(e.run_id),
        session_id: str(e.session_id),
        text: str(d.text),
        seq: num(e.seq),
        at: Date.now(),
      })
    })
    // T14：取消的终态是 agent.error{cancelled}，不是 done。apply 作用域记录，
    // 面板标签关掉/重挂都不影响证据。
    host.onEvent('agent.error', (e) => {
      const d = (e && e.data) || {}
      terminals.push({
        run_id: str(e.run_id),
        session_id: str(e.session_id),
        text: str(d.text),
        reason: str(d.reason),
        message: str(d.message),
        error: true,
        seq: num(e.seq),
        at: Date.now(),
      })
    })

    function Probe() {
      const theme = host.theme()
      const [facts, setFacts] = useState({ step: 'mount' })

      useEffect(() => {
        let alive = true
        const merge = (patch) => { if (alive) setFacts((f) => Object.assign({}, f, patch)) }

        // ping 只跑一次：T13 之前会重新打开标签，Probe 重挂不该再发一轮 agent.run ——
        // 那会让「第一次上游请求 body」这条 P1e 断言与 chatRequests 的计数漂移。
        if (globalThis.__agentPingStarted) { merge({ step: 'done', reused: true }); return }
        globalThis.__agentPingStarted = true

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
            if (ack.session_id) shared.sessionId = ack.session_id
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

      // 三个工具场景的统一入口：发 agent.run（复用 ping 的会话），回执写进 globalThis。
      const startRun = async (scene) => {
        const rec = { scene, at: Date.now(), threw: '', ack: { ok: false, run_id: '', session_id: '', reason: '' } }
        runs.push(rec)
        const payload = { input: scene }
        if (shared.sessionId) payload.session_id = shared.sessionId
        try {
          const r = await host.command('agent.run', payload)
          rec.ack.ok = !!r && r.ok === true
          rec.ack.run_id = str(r && r.run_id)
          rec.ack.session_id = str(r && r.session_id)
          rec.ack.reason = str(r && r.reason)
          if (rec.ack.session_id) shared.sessionId = rec.ack.session_id
          if (rec.ack.run_id) shared.lastRunId = rec.ack.run_id
        } catch (e) {
          rec.threw = String((e && e.message) || e)
        }
      }

      return h(
        'div',
        {
          'data-surface': ${JSON.stringify(PLUGIN_ID)},
          style: { padding: 12, font: '12px/1.6 ui-monospace, monospace', color: theme.tokens.fg },
        },
        h('div', { style: { color: theme.tokens.accent, fontWeight: 600, marginBottom: 6 } }, 'agent.run 探针'),
        h('div', { style: { display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 6 } },
          ['use-tool', 'closed-panel', 'write-tool', 'cancel'].map((s) =>
            h('button', { key: s, 'data-act': s, onClick: () => { void startRun(s) } }, s)),
          h('button', {
            key: 'cancel-run',
            'data-act': 'cancel-run',
            onClick: async () => {
              // T14：取消最近一次 agent.run；回执写 globalThis 供 Node 侧读。
              const ack = { ok: false, run_id: str(shared.lastRunId), reason: '', threw: '' }
              try {
                const r = await host.command('agent.cancel', { run_id: shared.lastRunId })
                ack.ok = !!r && r.ok === true
                ack.reason = str(r && r.reason)
              } catch (e) {
                ack.threw = String((e && e.message) || e)
              }
              globalThis.__agentCancelAck = ack
            },
          }, 'cancel-run')),
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
  commands: ['agent.session', 'agent.run', 'agent.cancel', 'agent.snapshot', 'tool.result'],
  // usage 也声明：它是成功 run 的必经终态之一（finish 里紧跟 delta 投出），
  // 订阅到 = 收尾帧的 usage 真的从 provider 走到了插件手里。
  //
  // tool.call（P2b）必须在这里声明 —— 安装期会**硬拒**「声明了 tools[] 却没声明
  // tool.call」的 manifest（否则每次工具调用只能等到超时，见 surfaces.go validateSurfaceTools）。
  data: ['agent.delta', 'agent.done', 'agent.error', 'agent.usage', 'agent.state', 'tool.call'],
  // P2b：三个工具各自的声明位就是被验证的对象 ——
  //   echo       read_only:true                                  → 免审批，独立于面板
  //   needs_panel read_only:true + requires_panel:true            → T12 的宿主门禁对象
  //   write_note read_only:false                                 → T13 的审批对象
  // input 一律 object schema（安装期只判这个，语义校验归插件）。
  tools: [
    {
      name: 'echo',
      description: '回显一段文本（只读，免审批）',
      input: {
        type: 'object',
        properties: { text: { type: 'string', description: '要回显的文本' } },
        required: ['text'],
      },
      read_only: true,
      requires_panel: false,
    },
    {
      name: 'needs_panel',
      description: '需要插件面板在场才能执行（requires_panel）',
      input: { type: 'object', properties: {} },
      read_only: true,
      requires_panel: true,
    },
    {
      name: 'write_note',
      description: '写一条便签（非只读 ⇒ 走宿主审批）',
      input: {
        type: 'object',
        properties: { text: { type: 'string', description: '便签内容' } },
        required: ['text'],
      },
      read_only: false,
      requires_panel: false,
    },
  ],
  entries: [{ id: 'probe', title: '运行探针', order: 10 }],
}

let code = 0
let app = null
let provider = null
let HOME = ''
let PLUGIN_DIR = ''
let WS = ''
let USER_DATA = ''
const cleanup = () => {
  try { app?.close() } catch { /* 已退出 */ }
  try {
    provider?.server.closeAllConnections?.()
    provider?.server.close()
  } catch { /* 已关闭 */ }
  for (const d of [HOME, PLUGIN_DIR, WS, USER_DATA]) {
    if (!d) continue
    try { fs.rmSync(d, { recursive: true, force: true }) } catch { /* 尽力 */ }
  }
}
process.on('exit', cleanup)

// Electron 的 userData（electron/main.ts unifyUserData）：`<appData>/hai-desktop-<hash>`，
// hash 必须与那边**同一算法**，否则预建的目录不会被 Chromium 使用。
//
// 为什么要预建：Chromium 在 Electron ready 之前就写 `<userData>/DevToolsActivePort`。
// macOS 首次运行、目录还不存在时，mkdir 偶尔慢半拍，DevTools handler 直接报
// `No such file or directory`，Playwright 只能等满 180s 启动超时（2026-11-14 全链真踩过，
// 目录创建时间与超时同一秒）。这是启动抖动，不是链路红；把父目录先建好即可。
function electronUserDataDir(dataDir) {
  const base = process.platform === 'darwin'
    ? path.join(os.homedir(), 'Library', 'Application Support')
    : process.platform === 'win32'
      ? (process.env.APPDATA || path.join(os.homedir(), 'AppData', 'Roaming'))
      : (process.env.XDG_CONFIG_HOME || path.join(os.homedir(), '.config'))
  let h = 0
  for (let i = 0; i < dataDir.length; i++) h = ((h << 5) - h + dataDir.charCodeAt(i)) | 0
  return path.join(base, `hai-desktop-${Math.abs(h).toString(36)}`)
}

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
        // 不放 api_key：启动时 migrateProviderKeysOnStartup 会把明文 key 搬进 keys.json，
        // 那一步走 safeStorage（macOS 钥匙串）；钥匙串同步等待+窗口尚未创建时，Playwright
        // 只看到 180s 启动超时（本探针在全链里真踩过）。假上游不校验 Authorization，
        // 这条链路不需要 key，也就别在本探针里顺带测钥匙串迁移。
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
  USER_DATA = electronUserDataDir(HOME)
  fs.mkdirSync(USER_DATA, { recursive: true })
  check('Electron userData 目录已预建（DevToolsActivePort 的父目录）',
    fs.existsSync(USER_DATA), USER_DATA)
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

  // ---- P2b：工具场景（同一会话复用；左栏计数保持 1）----
  //
  // 三条场景共用 ping 建出来的 session_id（globalThis.__agentSharedSession）。这样
  // T10 的「插件分组 1 条」不会被后续 run 改写成 4 条，工具的 jsonl 也落在同一个
  // transcript 上 —— 工具轮与最终回答是**一次会话的连续历史**，不是三次孤立调用。
  const readToolCalls = () => win.evaluate(() => (globalThis.__agentToolCalls || []).slice())
  const readToolRuns = () => win.evaluate(() => (globalThis.__agentToolRuns || []).slice())
  const readTerminals = () => win.evaluate(() => (globalThis.__agentRunTerminals || []).slice())
  const openPluginPanel = async () => {
    await activityBtn.click({ timeout: 5000 }).catch(() => {})
    await waitFor(() => win.locator('.rtabs-add').count().then((n) => n > 0), { label: '＋ 按钮（重开）' })
    await win.locator('.rtabs-add').click()
    await waitFor(() => win.locator(entrySel).count().then((n) => n > 0), { label: '＋ 菜单里的插件入口（重开）' })
    await win.locator(entrySel).first().click()
    await waitFor(() => win.locator('button[data-act="use-tool"]').count().then((n) => n > 0), { label: '工具按钮（重开）' })
  }
  const waitRunAck = (scene) => waitFor(async () => {
    const rs = await readToolRuns()
    return rs.find((r) => r.scene === scene && r.ack && r.ack.ok === true) || null
  }, { timeout: 30000, label: `${scene} 的 agent.run 回执` })
  const waitTerminal = (runID, text) => waitFor(async () => {
    const ts = await readTerminals()
    return ts.find((t) => t.run_id === runID && String(t.text).includes(text)) || null
  }, { timeout: 60000, label: `${runID} 的 done ${text}` })
  const toolTextOf = (scene) => {
    const r = chatRequests.find((x) => x.scene === scene && x.round2 === true)
    return r ? toolTexts(r.body) : []
  }

  // ---- T11：use-tool → tool.call → 面板 tool.result → 第二轮 → done ----
  console.log('\nT11 工具轮：tool.call → tool.result → 第二次上游请求')
  check('T11 前置：插件标签与工具按钮在场',
    await win.locator('button[data-act="use-tool"]').count() > 0)
  await win.locator('button[data-act="use-tool"]').first().click()
  const useRun = await waitRunAck(SCENE_USE_TOOL)
  await waitTerminal(useRun.ack.run_id, 'tool ok')
  const echoCall = (await readToolCalls()).find((c) =>
    c.tool === 'echo' && c.run_id === useRun.ack.run_id) || null
  console.log(`  echo tool.call：${JSON.stringify(echoCall)}`)
  check('T11 tool.call 到达面板', !!echoCall)
  check('T11 call_id 以 tc- 开头', !!echoCall && /^tc-/.test(echoCall.call_id), String(echoCall?.call_id))
  check('T11 args.text = hello（模型参数原样到面板）',
    echoCall?.args?.text === 'hello', JSON.stringify(echoCall?.args ?? null))
  check('T11 deadline_ms > 0', (echoCall?.deadline_ms ?? 0) > 0, String(echoCall?.deadline_ms))
  check('T11 read_only=true / requires_panel=false',
    echoCall?.read_only === true && echoCall?.requires_panel === false,
    JSON.stringify({ read_only: echoCall?.read_only, requires_panel: echoCall?.requires_panel }))
  await waitFor(async () => (await readToolCalls()).some((c) =>
    c.tool === 'echo' && c.run_id === useRun.ack.run_id && c.replied === true),
  { timeout: 10000, label: '面板回 tool.result' })
  const useToolMsgs = toolTextOf(SCENE_USE_TOOL)
  console.log(`  第二次请求 role:tool 文本：${JSON.stringify(useToolMsgs)}`)
  check('T11 第二轮上游请求带工具结果 hello', useToolMsgs.some((t) => t.includes('hello')), JSON.stringify(useToolMsgs))
  check('T11 该场景恰好两轮上游请求',
    chatRequests.filter((r) => r.scene === SCENE_USE_TOOL).length === 2,
    String(chatRequests.filter((r) => r.scene === SCENE_USE_TOOL).length))
  check('T11 终态 done 文本 = tool ok',
    (await readTerminals()).some((t) => t.run_id === useRun.ack.run_id && t.text.includes('tool ok')))

  // ---- T12：requires_panel + 关标签 → 宿主门禁，插件回调不得进入 ----
  console.log('\nT12 requires_panel：关标签后由宿主回 panel_not_open')
  check('T12 前置：needs_panel 声明 requires_panel=true',
    MANIFEST.tools?.some((t) => t.name === 'needs_panel' && t.requires_panel === true))
  await win.locator('button[data-act="closed-panel"]').first().click()
  const closedRun = await waitRunAck(SCENE_CLOSED_PANEL)
  // 假服务第一轮延迟 1s：必须在这 1s 内关掉最后一个该 kind 的标签。
  const pluginTab = win.locator('.rtabs .rtab', { hasText: '运行探针' }).first()
  if (await pluginTab.count()) {
    await pluginTab.locator('.rtab-close').click()
  } else {
    check('T12 找到插件标签并关闭', false, '未找到「运行探针」标签')
    throw new VerifyFailure('T12 无法关闭插件标签')
  }
  await waitTerminal(closedRun.ack.run_id, 'panel blocked')
  const closedMsgs = toolTextOf(SCENE_CLOSED_PANEL)
  console.log(`  第二次请求 role:tool 文本：${JSON.stringify(closedMsgs)}`)
  check('T12 模型看到 panel_not_open', closedMsgs.some((t) => t.includes('panel_not_open')), JSON.stringify(closedMsgs))
  const needsCalls = (await readToolCalls()).filter((c) => c.tool === 'needs_panel')
  check('T12 宿主挡在插件回调之前（needs_panel 从未进入面板）', needsCalls.length === 0, JSON.stringify(needsCalls))
  check('T12 标签确实关掉了',
    await win.locator('.rtabs .rtab', { hasText: '运行探针' }).count() === 0)

  // ---- T13：write_note（read_only=false）→ 审批 ----
  console.log('\nT13 非只读工具：审批后才 tool.call')
  // 审批卡属于**插件会话**的对话流。不先切到那条会话，run 虽然会跑，卡片却渲染在
  // 用户当前看的另一个会话里（甚至根本不可见）—— 这不是审批没发生，是探针看错了地方。
  const pluginGroup = win.locator('.plugin-group').first()
  const pluginChev = pluginGroup.locator('.ws-chev').first()
  if (await pluginChev.getAttribute('aria-expanded') !== 'true') await pluginChev.click().catch(() => {})
  await pluginGroup.locator('.session-title').first().click()
  await openPluginPanel()
  await win.locator('button[data-act="write-tool"]').first().click()
  const writeRun = await waitRunAck(SCENE_WRITE_TOOL)
  // P2c：read_only:false 的插件工具实现 MandatoryApproval —— 即使会话是「自动执行」，
  // 也必须出现审批卡；批准前面板不得收到 write_note、上游不得有第二轮。
  const approve = win.getByRole('button', { name: '批准', exact: true }).first()
  let approveVisible = true
  try {
    await approve.waitFor({ state: 'visible', timeout: 15000 })
  } catch {
    approveVisible = false
  }
  check('T13 审批卡出现（Auto/Full Access 也不能静默放行）', approveVisible,
    approveVisible ? '' : '15s 内没有「批准」按钮')
  if (!approveVisible) throw new VerifyFailure('T13 强制审批卡未出现')
  const approvalText = await win.locator('.appr-dock').first().innerText().catch(() => '')
  check('T13 审批卡显示「插件名 · 工具名」',
    approvalText.includes('运行探针 · write_note'), approvalText.slice(0, 200))
  check('T13 批准前没有第二轮上游请求',
    chatRequests.filter((r) => r.scene === SCENE_WRITE_TOOL && r.round2 === true).length === 0)
  check('T13 批准前面板没有收到 write_note',
    (await readToolCalls()).filter((c) => c.tool === 'write_note').length === 0)
  await approve.click()
  await waitTerminal(writeRun.ack.run_id, 'write ok')
  const writeCalls = (await readToolCalls()).filter((c) => c.tool === 'write_note')
  check('T13 批准后 tool.call 到达面板', writeCalls.length === 1, JSON.stringify(writeCalls))
  const writeMsgs = toolTextOf(SCENE_WRITE_TOOL)
  check('T13 工具结果 note 回到模型', writeMsgs.some((t) => t.includes('note')), JSON.stringify(writeMsgs))
  check('T13 终态 done 文本 = write ok',
    (await readTerminals()).some((t) => t.run_id === writeRun.ack.run_id && t.text.includes('write ok')))

  // ---- T14：agent.cancel 在请求进行中中断 ----
  console.log('\nT14 agent.cancel：上游迟迟不回时取消，终态必须是 error{cancelled}')
  await win.locator('button[data-act="cancel"]').first().click()
  const cancelRun = await waitRunAck(SCENE_CANCEL)
  // 等假上游真的收到请求：此时 run 在跑、响应被 CANCEL_DELAY_MS 按住。
  await waitFor(() => chatRequests.find((r) => r.scene === SCENE_CANCEL), { label: 'cancel 场景的上游请求' })
  const cancelAt = Date.now()
  await win.locator('button[data-act="cancel-run"]').first().click()
  const cancelAck = await waitFor(() => win.evaluate(() => globalThis.__agentCancelAck || null),
    { timeout: 10000, label: 'agent.cancel 回执' })
  check('T14 agent.cancel 回执 ok', cancelAck.ok === true, JSON.stringify(cancelAck))
  const cancelErr = await waitFor(async () => {
    const ts = await readTerminals()
    return ts.find((t) => t.run_id === cancelRun.ack.run_id && t.error && t.reason === 'cancelled') || null
  }, { timeout: 10000, label: '取消的 agent.error{cancelled}' })
  check('T14 终态是 agent.error{cancelled}', !!cancelErr, JSON.stringify(cancelErr))
  const cancelDone = (await readTerminals()).find((t) => t.run_id === cancelRun.ack.run_id && !t.error)
  check('T14 取消后没有 agent.done（终态互斥）', !cancelDone, JSON.stringify(cancelDone))
  check('T14 取消在延迟响应到点前生效',
    Date.now() - cancelAt < CANCEL_DELAY_MS - 500, `elapsed=${Date.now() - cancelAt}ms`)
  check('P2b 全程零 pageerror', errs.length === 0, errs.join(' | '))

  // ---- T15（P3a）：用户从宿主 UI 驱动插件会话 → 插件收到 agent.state ----
  // 关键：这条 run 不是 agent.run 发起的，旧的 surfaceAgentRun 投影会完全看不到；
  // 插件只靠 apply 作用域里的 agent.state 订阅知道会话在跑。
  console.log('\nT15 用户驱动插件会话 → agent.state')
  const statesBefore = await win.evaluate(() => (globalThis.__agentStates || []).length)
  const sessionTitle = win.locator('.plugin-group .session-title').first()
  await sessionTitle.click({ timeout: 5000 }).catch(() => {})
  const userComposer = win.locator('.composer textarea.input, .composer textarea').first()
  await waitFor(() => userComposer.isVisible(), { timeout: 15000, label: '插件会话输入框' })
  await userComposer.fill(SCENE_USER_STATE)
  await userComposer.press('Enter')
  const statePair = await waitFor(async () => {
    const evs = await win.evaluate(() => (globalThis.__agentStates || []).slice())
    const fresh = evs.slice(statesBefore)
    const running = fresh.find((s) => s.status === 'running' && s.session_id === sid)
    const idle = fresh.find((s) => s.status === 'idle' && s.session_id === sid)
    return running && idle ? { running, idle, fresh } : null
  }, { timeout: 60000, label: 'agent.state running → idle' }).catch(() => null)
  check('T15 用户驱动 run 出现 agent.state running', !!(statePair && statePair.running), JSON.stringify(statePair))
  check('T15 用户驱动 run 出现 agent.state idle', !!(statePair && statePair.idle), JSON.stringify(statePair))
  check('T15 state 的 session_id = 插件会话', !!(statePair && statePair.running.session_id === sid), JSON.stringify(statePair))
  check('T15 state 不含正文键', !!(statePair && !('text' in statePair.idle) && !('content' in statePair.idle)), JSON.stringify(statePair))
  check('T15 state seq 递增', !!(statePair && statePair.running.seq < statePair.idle.seq), JSON.stringify(statePair))
  check('T15 全程零 pageerror', errs.length === 0, errs.join(' | '))

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
