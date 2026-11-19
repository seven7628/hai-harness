// server.mjs —— 带服务网页插件的最小示例（零依赖，Node >= 18）。
//
// 宿主按 surface.json 的 web.start 启动它：node server.mjs，cwd = 插件目录。
// 页面通过 window.haiHost 调 Agent；普通浏览器打开时 window.haiHost 不存在，页面降级。
import http from 'node:http'

const PORT = Number(process.env.PORT || 5235)

const css = `
:root { color-scheme: dark; }
* { box-sizing: border-box; }
html, body { height: 100%; margin: 0; }
body {
  background: #0d1117; color: #e6edf3;
  font: 14px/1.55 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
}
.wrap { min-height: 100%; display: flex; flex-direction: column; }
header {
  display: flex; align-items: center; gap: 12px;
  padding: 14px 20px; border-bottom: 1px solid #21262d;
  background: #0d1117;
}
header h1 { margin: 0; font-size: 15px; letter-spacing: .01em; }
.pill {
  display: inline-flex; align-items: center; gap: 6px;
  padding: 3px 9px; border-radius: 999px; font-size: 11px;
  border: 1px solid #30363d; color: #8b949e;
}
.pill.ok { color: #3fb950; border-color: #238636; background: #0f1f13; }
.pill.bad { color: #f85149; border-color: #6e2a26; background: #1f1112; }
main { flex: 1; display: grid; grid-template-columns: 1.3fr .9fr; gap: 16px; padding: 20px; }
.card {
  border: 1px solid #21262d; border-radius: 10px; padding: 16px;
  background: #0f141a; min-width: 0;
}
.card h2 { margin: 0 0 8px; font-size: 13px; color: #8b949e; font-weight: 600; text-transform: uppercase; letter-spacing: .08em; }
.hero { font-size: 26px; font-weight: 650; margin: 2px 0 6px; }
.muted { color: #8b949e; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; }
input, button {
  font: inherit; border-radius: 7px; border: 1px solid #30363d;
  background: #161b22; color: #e6edf3; padding: 8px 10px;
}
input { flex: 1; min-width: 0; }
button { cursor: pointer; background: #238636; border-color: #2ea043; color: #fff; font-weight: 600; }
button:disabled { opacity: .5; cursor: not-allowed; }
.row { display: flex; gap: 8px; margin-top: 10px; }
pre {
  margin: 10px 0 0; padding: 10px; border-radius: 8px; background: #010409;
  border: 1px solid #21262d; white-space: pre-wrap; word-break: break-word;
  max-height: 260px; overflow: auto; color: #c9d1d9;
}
a { color: #58a6ff; }
@media (max-width: 760px) { main { grid-template-columns: 1fr; } }
`

function page({ title, path, body }) {
  return `<!doctype html><html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${title}</title><style>${css}</style></head>
<body><div class="wrap">
<header>
  <h1>${title}</h1>
  <span class="pill" id="host-pill">检测宿主…</span>
  <span class="pill" id="server-pill">server…</span>
  <span style="flex:1"></span>
  <span class="muted mono">${path}</span>
</header>
<main>${body}</main>
</div>
<script>
const host = window.haiHost
const $ = (id) => document.getElementById(id)
$('host-pill').textContent = host ? '宿主已连接 · ' + (host.pluginId || '?') : '普通浏览器（无 haiHost）'
$('host-pill').className = 'pill ' + (host ? 'ok' : 'bad')
fetch('/health').then((r) => r.json()).then((j) => {
  $('server-pill').textContent = 'server pid ' + (j.pid || '?')
  $('server-pill').className = 'pill ok'
}).catch(() => { $('server-pill').className = 'pill bad' })

let off = null
async function ask() {
  const input = $('ask-input')
  const out = $('answer')
  if (!host) { out.textContent = '这个页面不在宿主里：window.haiHost 不存在。'; return }
  const btn = $('ask-btn'); btn.disabled = true
  out.textContent = '创建插件会话…'
  try {
    const s = await host.command('agent.session', {})
    if (!s || s.ok !== true) { out.textContent = 'agent.session 失败：' + JSON.stringify(s); return }
    const sid = s.session_id || (s.data && s.data.session_id)
    if (!off) {
      off = host.onEvent('agent.delta', (ev) => { out.textContent += (ev && ev.data && ev.data.text) || '' })
      host.onEvent('agent.done', () => { btn.disabled = false })
      host.onEvent('agent.error', (ev) => { out.textContent += '\\n[error] ' + JSON.stringify(ev && ev.data); btn.disabled = false })
    }
    out.textContent = ''
    const r = await host.command('agent.run', { session_id: sid, input: input.value || '用一句话介绍这个页面' })
    if (!r || r.ok !== true) { out.textContent = 'agent.run 失败：' + JSON.stringify(r); btn.disabled = false }
  } catch (e) {
    out.textContent = '调用异常：' + String(e)
    btn.disabled = false
  }
}
const b = $('ask-btn'); if (b) b.addEventListener('click', ask)
</script></body></html>`
}

const server = http.createServer((req, res) => {
  const u = new URL(req.url || '/', `http://127.0.0.1:${PORT}`)
  if (u.pathname === '/health') {
    res.writeHead(200, { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store' })
    res.end(JSON.stringify({ ok: true, pid: process.pid, port: PORT }))
    return
  }
  if (u.pathname === '/favicon.ico') { res.writeHead(204); res.end(); return }
  const admin = u.pathname === '/admin' || u.pathname.startsWith('/admin/')
  const html = admin
    ? page({
        title: '后台 · 带服务的网页示例',
        path: '/admin',
        body: `<section class="card">
          <h2>后台页</h2>
          <div class="hero">第二个入口，第二个标签。</div>
          <p class="muted">它和首页是同一个服务进程，但右栏里是两个互相保活的标签。</p>
          <pre class="mono">entry = admin
path = /admin
server pid = ${process.pid}</pre>
        </section>
        <section class="card">
          <h2>宿主能力</h2>
          <p class="muted">页面仍然可以用 window.haiHost 调 Agent；切回首页时这里的 DOM 状态不会被丢。</p>
          <div class="row"><input id="ask-input" placeholder="问 Agent 一句话…"><button id="ask-btn">问 Agent</button></div>
          <pre id="answer" class="mono">尚未提问。</pre>
        </section>`,
      })
    : page({
        title: '首页 · 带服务的网页示例',
        path: '/',
        body: `<section class="card">
          <h2>这个页面由插件自己的 server 提供</h2>
          <div class="hero">打开即满幅，服务由宿主按需启动。</div>
          <p class="muted">宿主先探活，没起才 spawn <span class="mono">node server.mjs</span>，就绪后 loadURL。
          关标签不会杀服务；关插件/退出应用才会。</p>
          <pre class="mono">web.url = http://127.0.0.1:${PORT}
web.start = node server.mjs
health = /health
server pid = ${process.pid}</pre>
        </section>
        <section class="card">
          <h2>Agent 能力</h2>
          <p class="muted">页面通过 preload 注入的 <span class="mono">window.haiHost</span> 使用宿主 Agent；
          会话属于这个插件，不会混进用户的当前对话。</p>
          <div class="row"><input id="ask-input" placeholder="问 Agent 一句话…"><button id="ask-btn">问 Agent</button></div>
          <pre id="answer" class="mono">尚未提问。</pre>
        </section>`,
      })
  res.writeHead(200, { 'content-type': 'text/html; charset=utf-8', 'cache-control': 'no-store' })
  res.end(html)
})

server.listen(PORT, '127.0.0.1', () => {
  console.log(`[web-server-demo] listening on http://127.0.0.1:${PORT} pid=${process.pid}`)
})
const stop = () => server.close(() => process.exit(0))
process.on('SIGTERM', stop)
process.on('SIGINT', stop)
