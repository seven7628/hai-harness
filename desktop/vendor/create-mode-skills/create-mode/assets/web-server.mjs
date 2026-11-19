// server.mjs —— web.start 的最小服务（零依赖，Node >= 18）。
//
// 宿主按 surface.json 的 web.start 启动：node server.mjs，cwd = 插件目录；
// 轮询 /health 就绪后才 loadURL。普通浏览器也能直接打开 http://127.0.0.1:5235。
import http from 'node:http'

const PORT = Number(process.env.PORT || 5235)
const html = (title, body) => `<!doctype html><html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>${title}</title>
<style>
html,body{height:100%;margin:0}body{background:#0d1117;color:#e6edf3;font:14px/1.6 system-ui,sans-serif}
main{min-height:100%;padding:28px;box-sizing:border-box}h1{font-size:22px;margin:0 0 10px}
p{color:#8b949e}.card{border:1px solid #21262d;border-radius:10px;padding:18px;max-width:760px;background:#0f141a}
code,pre{font-family:ui-monospace,Menlo,monospace;font-size:12px}pre{white-space:pre-wrap;background:#010409;padding:10px;border-radius:8px}
</style></head><body><main><div class="card"><h1>${title}</h1>${body}
<pre id="host"></pre></div></main>
<script>
const h = window.haiHost
document.getElementById('host').textContent = h
  ? 'window.haiHost 已注入：' + (h.pluginId || '?')
  : '普通浏览器：window.haiHost 不存在（页面已降级）'
</script></body></html>`

const server = http.createServer((req, res) => {
  const u = new URL(req.url || '/', `http://127.0.0.1:${PORT}`)
  if (u.pathname === '/health') {
    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify({ ok: true, pid: process.pid }))
    return
  }
  const admin = u.pathname === '/admin'
  res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' })
  res.end(html(admin ? '后台' : '首页', `<p>${admin ? '第二个入口 / 第二个标签。' : '这个页面由插件自己的 server 提供。'}</p>
    <p>入口：<code>${u.pathname}</code> · 进程：<code>${process.pid}</code></p>
    <p>页面可以用 <code>window.haiHost.command('agent.session')</code> 调宿主 Agent。</p>`))
})
server.listen(PORT, '127.0.0.1', () => console.log(`[my-server-app] http://127.0.0.1:${PORT} pid=${process.pid}`))
process.on('SIGTERM', () => server.close(() => process.exit(0)))
