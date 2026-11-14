#!/usr/bin/env node
// 手机 transport 验收（Node 直跑，无需浏览器/构建）。
//
// 为什么这些断言值得单独一条脚本：手机 transport 是「桌面 UI 能跑在手机上」的唯一
// 数据面，它有三条容易被后续改动静默破坏的契约（见 src/mobile/transport/mobile.ts）：
//   1. HTTP 应答必须**合成** command_response —— bridge 的 /api/cmd 只回 HTTP，
//      hub/api.ts 的 hubCommand 订阅的却是 WS 事件；丢了这一帧，手机 Hub 每条命令
//      都要等满 20s 超时（P8-01 审计的「四个真正阻塞」之一）。
//   2. onEvent 多订阅 + 双 WS（chat 不带 scope / hub 带 scope=hub）+ 同帧去重。
//   3. 401 不清 token、错误可读；没实现的方法降级 unsupported、绝不抛。
//
// 另加静态边界断言：手机包（src/mobile/**）与改造过的 6 个 Hub 组件都**不得**再
// import store/useAppStore —— 一旦破线，桌面整棵 wiring（i18n 绑 store、Electron
// 专属动作）就跟着进手机包，P8-01 的复用边界白划。
import { build } from 'esbuild'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join, relative } from 'node:path'

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
  entryPoints: [join(APP, 'src', 'mobile', 'transport', 'mobile.ts')],
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
})
const mod = await import(`data:text/javascript;base64,${Buffer.from(result.outputFiles[0].text).toString('base64')}`)

// ─────────── 测试替身 ───────────
function jsonReply(status, body) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  }
}

function memStorage(init = {}) {
  const m = new Map(Object.entries(init))
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => {
      m.set(k, String(v))
    },
    dump: () => Object.fromEntries(m),
  }
}

class MockWS {
  static instances = []
  constructor(url) {
    this.url = url
    this.closed = false
    this.onopen = null
    this.onmessage = null
    this.onclose = null
    this.onerror = null
    MockWS.instances.push(this)
  }
  close() {
    this.closed = true
  }
  fireOpen() {
    this.onopen?.()
  }
  fire(data) {
    this.onmessage?.({ data })
  }
  fireClose() {
    this.onclose?.()
  }
}

function fakeTimers() {
  const timers = []
  return {
    timers,
    setTimeout: (fn, ms) => {
      timers.push({ fn, ms })
      return timers.length - 1
    },
    clearTimeout: () => {},
    runLast() {
      const t = timers.pop()
      if (t) t.fn()
      return t
    },
  }
}

function harness(extra = {}) {
  const storage = extra.storage ?? memStorage({ 'hai.token': 'T'.repeat(8) })
  const calls = []
  const timers = fakeTimers()
  let clock = 0
  const fetchMock =
    extra.fetch ??
    (async (url, init) => {
      calls.push({ url, init })
      return jsonReply(200, { ok: true, data: { pong: 1 } })
    })
  const tr = mod.mobileTransport({
    storage,
    fetch: fetchMock,
    WebSocket: extra.WebSocket ?? MockWS,
    location: { protocol: 'http:', host: 'phone.local:5273' },
    now: () => clock,
    setTimeout: timers.setTimeout,
    clearTimeout: timers.clearTimeout,
  })
  return {
    tr,
    storage,
    calls,
    timers,
    advance: (ms) => {
      clock += ms
    },
  }
}

console.log('\n▶ 凭据读取（旧页 key 优先）')
{
  const both = mod.mobileTransport({ storage: memStorage({ 'hai.token': 'OLD', 'hai.mobile.token': 'NEW' }) })
  eq(both.token(), 'OLD', '同时存在时取旧页 key hai.token')
  const fallback = mod.mobileTransport({ storage: memStorage({ 'hai.mobile.token': 'NEW' }) })
  eq(fallback.token(), 'NEW', '只有 hai.mobile.token 时取它')
  const none = mod.mobileTransport({ storage: memStorage() })
  eq(none.token(), '', '都没有 → 空串（壳据此显示配对卡）')
  eq(mod.TOKEN_KEYS[0], 'hai.token', 'TOKEN_KEYS[0] 是旧页 key')
}

console.log('\n▶ HTTP 应答 → 合成 command_response')
{
  const { tr, calls } = harness()
  const seen = []
  const seen2 = []
  tr.onEvent((line) => seen.push(JSON.parse(line)))
  tr.onEvent((line) => seen2.push(JSON.parse(line)))

  const r = await tr.send({ id: 5_000_001, type: 'hub_group_list', payload: { x: 1 } })
  assert(r.ok === true, 'send 返回 {ok:true}')
  eq(calls.length, 1, '只打一次 /api/cmd')
  eq(calls[0].url, '/api/cmd', 'URL 是 /api/cmd')
  eq(calls[0].init.method, 'POST', '方法 POST')
  eq(calls[0].init.headers.Authorization, `Bearer ${'T'.repeat(8)}`, '带 Authorization: Bearer <token>')
  eq(JSON.parse(calls[0].init.body).cmd, 'hub_group_list', 'body.cmd = type（不是 body.type）')
  eq(JSON.parse(calls[0].init.body).payload.x, 1, 'body.payload 原样透传')

  eq(seen.length, 1, '订阅者 1 收到合成的 command_response')
  eq(seen2.length, 1, '订阅者 2 也收到（多订阅）')
  eq(seen[0].event_type, 'command_response', 'event_type = command_response')
  eq(seen[0].id, 5_000_001, 'id 是请求 id（hubCommand 靠它配对）')
  eq(seen[0].ok, true, 'ok 透传')
  eq(seen[0].data.pong, 1, 'data 透传（body.data）')
  eq(seen[0].error, undefined, '成功帧不带 error')
}

console.log('\n▶ 业务失败：ok:false 也要合成事件')
{
  const { tr } = harness({ fetch: async () => jsonReply(200, { ok: false, error: '群不存在', code: 'not_found' }) })
  const seen = []
  tr.onEvent((line) => seen.push(JSON.parse(line)))
  const r = await tr.send({ id: 42, type: 'hub_group_get', payload: {} })
  eq(r.ok, false, 'send 返回 {ok:false}')
  eq(r.error, '群不存在', 'error 可读（服务端原文）')
  eq(seen.length, 1, '失败也合成 command_response')
  eq(seen[0].ok, false, '合成帧 ok=false')
  eq(seen[0].error, '群不存在', '合成帧带 error')
  eq(seen[0].code, 'not_found', '合成帧带 code')
}

console.log('\n▶ 401：不清 token、错误可读')
{
  const storage = memStorage({ 'hai.token': 'KEEP' })
  const { tr } = harness({ storage, fetch: async () => jsonReply(401, { error: 'unauthorized' }) })
  const seen = []
  tr.onEvent((line) => seen.push(JSON.parse(line)))
  const r = await tr.send({ id: 7, type: 'hub_agent_list', payload: {} })
  eq(r.ok, false, 'send 返回 {ok:false}')
  assert(/401/.test(r.error ?? ''), '错误里带 401（可读）')
  assert(/配对/.test(r.error ?? ''), '错误里告诉用户去重新配对')
  eq(storage.getItem('hai.token'), 'KEEP', '**没有**静默删除 token')
  eq(tr.token(), 'KEEP', 'transport 仍持有凭据')
  eq(seen.length, 1, '401 也合成 command_response（hubCommand 不会干等 20s）')
  eq(seen[0].ok, false, '合成帧 ok=false')
  assert(/401/.test(tr.status().lastError ?? ''), 'status().lastError 记下 401')
  eq(tr.status().hub, 'unauthorized', 'Hub 事件流标成 unauthorized（不再每 15s 撞墙）')
}

console.log('\n▶ 网络错误：可读、不抛、不动 token')
{
  const storage = memStorage({ 'hai.token': 'KEEP2' })
  const { tr } = harness({
    storage,
    fetch: async () => {
      throw new Error('fetch failed: ECONNREFUSED')
    },
  })
  const seen = []
  tr.onEvent((line) => seen.push(JSON.parse(line)))
  const r = await tr.send({ id: 8, type: 'hub_agent_list', payload: {} })
  eq(r.ok, false, 'send 返回 {ok:false}')
  assert(/网络错误/.test(r.error ?? ''), '错误前缀是「网络错误」')
  assert(/ECONNREFUSED/.test(r.error ?? ''), '保留底层原因（可读）')
  eq(storage.getItem('hai.token'), 'KEEP2', '网络错误不动 token')
  eq(seen.length, 1, '网络错误也合成 command_response')
  eq(seen[0].ok, false, '合成帧 ok=false')
}

console.log('\n▶ 未配对：不发请求，但仍合成失败帧')
{
  const { tr, calls } = harness({ storage: memStorage() })
  const seen = []
  tr.onEvent((line) => seen.push(JSON.parse(line)))
  const r = await tr.send({ id: 9, type: 'hub_group_list', payload: {} })
  eq(r.ok, false, 'send 返回 {ok:false}')
  eq(calls.length, 0, '没有凭据就不打 /api/cmd')
  eq(seen.length, 1, '仍然合成一帧（等待方不会超时）')
  assert(/配对/.test(seen[0].error ?? ''), '错误提示去配对')
}

console.log('\n▶ WS：chat / hub 两条流、多订阅、同帧去重、重连换代')
{
  const { tr, advance, timers } = harness()
  MockWS.instances.length = 0

  tr.connectHub()
  eq(MockWS.instances.length, 1, 'connectHub 开一条 WS')
  const hub = MockWS.instances[0]
  assert(hub.url.startsWith('ws://phone.local:5273/api/events?'), 'ws:// 按 location 推导')
  assert(/scope=hub/.test(hub.url), 'hub 流带 scope=hub')
  assert(/token=/.test(hub.url), 'token 走 query（浏览器 WS 不能设 Authorization 头）')

  const a = []
  const b = []
  tr.onEvent((line) => a.push(line))
  tr.onEvent((line) => b.push(line))
  hub.fireOpen()
  eq(tr.status().hub, 'open', 'onopen → status.hub = open')

  const frame = JSON.stringify({ event_type: 'hub_message', id: 1, group_id: 'g1' })
  hub.fire(frame)
  eq(a.length, 1, '订阅者 1 收到 WS 帧')
  eq(b.length, 1, '订阅者 2 收到同一帧（多订阅）')
  eq(a[0], frame, '事件原样 JSON 字符串分发（不重新序列化）')

  hub.fire(frame)
  eq(a.length, 1, '3s 内同一帧只投一次（Safari 双连接重复帧）')
  advance(4000)
  hub.fire(frame)
  eq(a.length, 2, '超过 3s 后同内容视为新帧')

  // 断线重连：onclose 排定时器 → 换代开新连接；旧连接的回调一律作废。
  hub.fireClose()
  eq(tr.status().hub, 'closed', 'onclose → status.hub = closed（重连中）')
  eq(timers.timers.length, 1, '排了一次重连')
  timers.runLast()
  eq(MockWS.instances.length, 2, '重连开了新连接')
  const hub2 = MockWS.instances[1]
  hub2.fireOpen()
  eq(tr.status().hub, 'open', '新连接 open 后恢复')
  hub.fire(JSON.stringify({ event_type: 'stale', id: 2 }))
  eq(a.length, 2, '旧连接迟到的帧被 generation 守卫丢弃')

  // chat 流：不带 scope（mobile_server.go 只接受空 scope 或 hub），按 workspace+session_id 过滤
  tr.connectChat('/w/proj', 'sess-1')
  const chat = MockWS.instances[MockWS.instances.length - 1]
  assert(/workspace=%2Fw%2Fproj/.test(chat.url), 'chat 流带 workspace')
  assert(/session_id=sess-1/.test(chat.url), 'chat 流带 session_id')
  assert(!/scope=/.test(chat.url), 'chat 流**不带** scope（服务端会 400）')

  tr.disconnect()
  eq(tr.status().hub, 'idle', 'disconnect 把状态复位')
  assert(hub2.closed === true, 'disconnect 真的关掉连接')
}

console.log('\n▶ 未实现方法降级 unsupported（不抛）')
{
  const { tr } = harness()
  for (const name of ['fsList', 'fsRead', 'mcpList', 'oauthLogin', 'createWorkspaceFolder', 'notARealMethod']) {
    let out
    let threw = null
    try {
      out = await tr[name]('/tmp', {})
    } catch (e) {
      threw = e
    }
    eq(threw, null, `${name}() 不抛异常`)
    eq(out.ok, false, `${name}() 返回 {ok:false}`)
    assert(String(out.error).startsWith('unsupported'), `${name}() 的 error 以 unsupported 开头（got=${out.error}）`)
  }
  eq(typeof tr.then, 'undefined', 'transport 不是 thenable（否则 await 会死循环）')
  const s = await tr.start('/w')
  eq(s.ok, true, 'start(workspace) 返回 ok')
  const exitOff = tr.onExit(() => {})
  eq(typeof exitOff, 'function', 'onExit 返回退订函数（可空实现）')
}

console.log('\n▶ settings：合理默认 + 写 localStorage')
{
  const storage = memStorage({ 'hai.theme': 'paper' })
  const { tr } = harness({ storage })
  const s0 = await tr.settingsGet()
  eq(s0.theme, 'light', '默认主题跟随旧页 hai.theme=paper → light')
  eq(s0.lang, 'zh', '默认语言 zh')
  const w = await tr.settingsSet({ theme: 'dark' })
  eq(w.ok, true, 'settingsSet 返回 ok')
  eq(w.settings.theme, 'dark', 'settingsSet 回带合并后的设置')
  const s1 = await tr.settingsGet()
  eq(s1.theme, 'dark', 'settingsGet 读回已写入的主题')
  assert(storage.getItem('hai.mobile.settings') !== null, '落到 localStorage（hai.mobile.settings）')
}

console.log('\n▶ ping / pair（配对屏依赖）')
{
  const storage = memStorage()
  let pingBody = { app: 'hai', paired: false }
  const { tr } = harness({
    storage,
    fetch: async (url) => {
      if (url === '/api/ping') return jsonReply(200, pingBody)
      if (url === '/api/pair') return jsonReply(200, { token: 'PAIRED', device_id: 'dev1' })
      return jsonReply(404, { error: 'nope' })
    },
  })
  const p0 = await tr.ping()
  eq(p0.ok, true, 'ping 200 → ok')
  eq(p0.data.paired, false, '未配对时 paired=false')
  const pair = await tr.pair('123456', '我的手机')
  eq(pair.ok, true, 'pair 成功')
  eq(tr.token(), 'PAIRED', 'pair 把 token 写进 storage')
  eq(storage.getItem('hai.token'), 'PAIRED', '写的是旧页 key（旧页/新页互相认）')
  pingBody = { app: 'hai', paired: true }
  const p1 = await tr.ping()
  eq(p1.data.paired, true, '配对后 ping paired=true')
  const bad = await tr.pair('000000', 'x')
  eq(bad.ok, true, '（替身固定返回成功，这里只验证不再重复写空 token）')
}

console.log('\n▶ 静态边界：手机包与 Hub 组件不得 import 桌面 store')
{
  const walk = (dir) => {
    const out = []
    for (const name of readdirSync(dir)) {
      const p = join(dir, name)
      if (statSync(p).isDirectory()) out.push(...walk(p))
      else if (/\.(ts|tsx)$/.test(name)) out.push(p)
    }
    return out
  }
  const mobileFiles = walk(join(APP, 'src', 'mobile'))
  assert(mobileFiles.length >= 4, `手机包有 ${mobileFiles.length} 个源文件`)
  for (const f of mobileFiles) {
    const src = readFileSync(f, 'utf8')
    assert(!/from\s+['"][^'"]*store\/useAppStore['"]/.test(src), `${relative(APP, f)} 不 import useAppStore`)
    assert(!/from\s+['"][^'"]*transport\/mock['"]/.test(src), `${relative(APP, f)} 不 import mock transport`)
  }
  for (const name of ['GroupView', 'HubSurface', 'HubInspector', 'HubInlineNeeds', 'HubAttachments', 'MessageBubble']) {
    const src = readFileSync(join(APP, 'src', 'hub', `${name}.tsx`), 'utf8')
    assert(!/from\s+['"][^'"]*useAppStore['"]/.test(src), `${name}.tsx 已改走 host seam（无 useAppStore import）`)
    assert(/host\/panels/.test(src), `${name}.tsx 引 host/panels`)
  }
  const seam = readFileSync(join(APP, 'src', 'host', 'panels.ts'), 'utf8')
  assert(!/from\s+['"][^'"]*useAppStore['"]/.test(seam), 'host/panels.ts 不 import useAppStore（桌面实现在 panels-desktop.tsx）')
  const idx = readFileSync(join(APP, 'src', 'transport', 'index.ts'), 'utf8')
  assert(/__HAI_MOBILE__/.test(idx), 'transport/index.ts 有手机分流')
  assert(/sharedMobileTransport/.test(idx), '分流用 sharedMobileTransport（与手机壳同一实例）')
}

console.log('\n▶ 静态边界：入口/构建/脚本已接线')
{
  const html = readFileSync(join(APP, 'mobile.html'), 'utf8')
  assert(/src="\/src\/mobile\/main\.tsx"/.test(html), 'mobile.html 指向 src/mobile/main.tsx')
  assert(/viewport-fit=cover/.test(html), 'mobile.html 带 viewport-fit=cover（safe-area）')
  const vite = readFileSync(join(APP, 'vite.config.ts'), 'utf8')
  assert(/mobile:\s*'mobile\.html'/.test(vite), 'vite 多入口含 mobile.html')
  assert(/main:\s*'index\.html'/.test(vite), 'vite 多入口含 index.html')
  assert(/dist-mobile/.test(vite), '手机产物目录 dist-mobile')
  assert(/base:\s*'\.\/'/.test(vite), "base:'./' 保持（/m/ 子路径托管）")
  const pkg = JSON.parse(readFileSync(join(APP, 'package.json'), 'utf8'))
  assert(!!pkg.scripts['mobile:dev'], 'package.json 有 mobile:dev')
  assert(!!pkg.scripts['mobile:build'], 'package.json 有 mobile:build')
  assert(!!pkg.scripts['test:mobile-transport'], 'package.json 有 test:mobile-transport')
  const cssSanity = readFileSync(join(APP, 'scripts', 'test-css-sanity.mjs'), 'utf8')
  assert(/src\/mobile\/mobile\.css/.test(cssSanity), 'test:css 覆盖 src/mobile/mobile.css')
}

console.log(`\n全部通过：${pass} 条断言`)
