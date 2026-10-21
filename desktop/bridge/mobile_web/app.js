// HAI 手机遥控器（P0）：配对 → 工作区/会话 → 会话内流式 + HITL。
// 无构建、无依赖：全部走 /api/cmd 与 /api/events，渲染只碰 textContent（不拼 HTML）。
(() => {
  'use strict'

  const $ = (id) => document.getElementById(id)
  const LS = {
    token: 'hai.token', addr: 'hai.addr', ws: 'hai.ws', sid: 'hai.sid', name: 'hai.name',
  }

  const state = {
    token: localStorage.getItem(LS.token) || '',
    workspaces: [],
    sessions: [],
    ws: '',
    sid: '',
    blocks: [],
    cursor: 0,
    hasMore: false,
    live: { text: '', thinking: '' },
    running: false,
    approval: null,
    questions: null,
    conn: null,
    retry: 0,
    disposed: false,
  }

  // ---------- 基础 ----------

  function show(screen) {
    for (const id of ['screen-pair', 'screen-list', 'screen-chat']) {
      $(id).hidden = id !== screen
    }
    $('back').hidden = screen !== 'screen-chat'
    $('menu').hidden = screen !== 'screen-chat'
    $('title').textContent = screen === 'screen-chat' ? ($('ws-select').selectedOptions[0]?.textContent || 'HAI') : 'HAI'
  }

  async function api(cmd, payload = {}) {
    const r = await fetch('/api/cmd', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...(state.token ? { Authorization: 'Bearer ' + state.token } : {}),
      },
      body: JSON.stringify({ cmd, payload }),
    })
    if (r.status === 401) {
      resetAuth()
      throw new Error('设备凭据失效，请重新配对')
    }
    const data = await r.json().catch(() => ({}))
    if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status))
    if (data.ok === false) throw new Error(data.error || '命令失败')
    return data.data || {}
  }

  function resetAuth() {
    state.token = ''
    localStorage.removeItem(LS.token)
    disconnect()
    show('screen-pair')
  }

  function pairError(msg) {
    const el = $('pair-error')
    el.textContent = msg
    el.hidden = !msg
  }

  // ---------- 配对 ----------

  async function doPair() {
    pairError('')
    const code = $('pair-code').value.trim()
    const name = $('pair-name').value.trim() || '手机'
    if (!/^\d{6}$/.test(code)) return pairError('请输入 6 位配对码')
    try {
      const r = await fetch('/api/pair', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code, name }),
      })
      const data = await r.json().catch(() => ({}))
      if (!r.ok) return pairError(data.error || '配对失败')
      state.token = data.token
      localStorage.setItem(LS.token, data.token)
      localStorage.setItem(LS.name, name)
      await boot()
    } catch (e) {
      pairError(e.message || '网络错误')
    }
  }

  // ---------- 工作区 / 会话 ----------

  async function loadWorkspaces() {
    const data = await api('workspace_list')
    state.workspaces = data.workspaces || []
    const sel = $('ws-select')
    sel.textContent = ''
    for (const w of state.workspaces) {
      const o = document.createElement('option')
      o.value = w.path
      o.textContent = w.title || w.path
      sel.appendChild(o)
    }
    if (!state.workspaces.length) {
      show('screen-list')
      $('sessions').textContent = ''
      $('list-empty').hidden = false
      $('list-empty').textContent = '还没有工作区，点右上角 ＋ 添加。'
      return
    }
    const saved = localStorage.getItem(LS.ws)
    if (saved && state.workspaces.some((w) => w.path === saved)) {
      sel.value = saved
    } else {
      sel.value = state.workspaces[0].path
    }
    state.ws = sel.value
    await loadSessions()
  }

  async function loadSessions() {
    state.ws = $('ws-select').value
    localStorage.setItem(LS.ws, state.ws)
    const data = await api('list', { workspace: state.ws })
    state.sessions = data.sessions || []
    renderSessions()
    show('screen-list')
  }

  function renderSessions() {
    const ul = $('sessions')
    ul.textContent = ''
    $('list-empty').hidden = state.sessions.length > 0
    for (const s of state.sessions) {
      const li = document.createElement('li')
      const t = document.createElement('span')
      t.className = 't'
      t.textContent = s.title || s.id
      const m = document.createElement('span')
      m.className = 'm'
      m.textContent = fmtTime(s.updated_at)
      li.append(t, m)
      if (s.running) {
        const d = document.createElement('span')
        d.className = 'dot'
        d.title = '运行中'
        li.appendChild(d)
      }
      li.addEventListener('click', () => openSession(s.id))
      ul.appendChild(li)
    }
  }

  // 让电脑弹目录选择器：手机只发请求，然后轮询工作区清单（比再开一条 WS 简单）。
  let pickTimer = null
  async function pickOnDesktop() {
    if (pickTimer) return
    $('ws-wait').hidden = false
    const before = state.workspaces.length
    try {
      await api('workspace_pick_request')
    } catch (e) {
      $('ws-wait').hidden = true
      alert(e.message)
      return
    }
    const started = Date.now()
    pickTimer = setInterval(async () => {
      const done = () => {
        clearInterval(pickTimer)
        pickTimer = null
        $('ws-wait').hidden = true
      }
      try {
        const data = await api('workspace_list')
        const list = data.workspaces || []
        if (list.length !== before) {
          done()
          await loadWorkspaces()
          return
        }
      } catch {
        /* 网络抖动继续等 */
      }
      if (Date.now() - started > 90000) done()
    }, 1500)
  }

  async function addWorkspace() {
    const path = prompt('工作区绝对路径（如 /Users/you/project）')
    if (!path) return
    try {
      await api('workspace_add', { path })
      await loadWorkspaces()
    } catch (e) {
      alert(e.message)
    }
  }

  // ---------- 会话 ----------

  async function newSession() {
    const data = await api('new_session', { workspace: state.ws, blocks_limit: 30 })
    state.sid = data.session_id
    localStorage.setItem(LS.sid, state.sid)
    await openSession(data.session_id)
  }

  async function openSession(sid) {
    const data = await api('new_session', { workspace: state.ws, session_id: sid, blocks_limit: 30 })
    state.sid = data.session_id || sid
    localStorage.setItem(LS.sid, state.sid)
    state.blocks = parseBlocks(data.view_checkpoint)
    state.cursor = data.blocks_cursor || 0
    state.hasMore = !!data.blocks_has_more
    state.live = { text: '', thinking: '' }
    state.running = false
    renderMessages()
    show('screen-chat')
    connect()
    scrollToEnd()
  }

  async function loadEarlier() {
    if (!state.hasMore || !state.cursor) return
    const box = $('messages')
    const before = box.scrollHeight
    const btn = $('load-earlier')
    if (btn) btn.disabled = true
    try {
      const data = await api('blocks_page', {
        workspace: state.ws, session_id: state.sid, before_id: state.cursor, limit: 50,
      })
      const older = Array.isArray(data.blocks) ? data.blocks : []
      state.blocks = older.concat(state.blocks)
      state.cursor = data.cursor || 0
      state.hasMore = !!data.has_more
      renderMessages()
      // 前插后把视口钉在原来那条消息上（否则会跳到顶部）
      box.scrollTop = box.scrollHeight - before
    } catch (e) {
      appendError(e.message)
    }
  }

  function parseBlocks(vc) {
    if (!vc) return []
    try {
      const v = typeof vc === 'string' ? JSON.parse(vc) : vc
      return Array.isArray(v.blocks) ? v.blocks : []
    } catch {
      return []
    }
  }

  function renderMessages() {
    const box = $('messages')
    box.textContent = ''
    if (state.hasMore) {
      const more = el('button', 'ghost', '加载更早')
      more.id = 'load-earlier'
      more.addEventListener('click', loadEarlier)
      box.appendChild(more)
    }
    for (const b of state.blocks) box.appendChild(renderBlock(b))
    if (state.live.thinking) box.appendChild(el('div', 'msg thinking', state.live.thinking))
    if (state.live.text) {
      const d = el('div', 'msg assistant' + (state.running ? ' cursor' : ''), state.live.text)
      box.appendChild(d)
    }
    updateRunning()
  }

  function renderBlock(b) {
    switch (b.kind) {
      case 'user':
        return el('div', 'msg user', b.text || '')
      case 'assistant': {
        const wrap = document.createDocumentFragment()
        if (b.thinking) wrap.appendChild(el('div', 'msg thinking', b.thinking))
        if (b.text) wrap.appendChild(el('div', 'msg assistant', b.text))
        return wrap
      }
      case 'tool': {
        const d = document.createElement('details')
        d.className = 'tool'
        const s = document.createElement('summary')
        s.textContent = `${b.name || 'tool'} · ${b.status || ''}${b.is_error ? ' · 失败' : ''}`
        d.appendChild(s)
        if (b.args) d.appendChild(el('pre', '', b.args))
        if (b.result) d.appendChild(el('pre', '', b.result))
        return d
      }
      case 'error':
        return el('div', 'msg error', b.error || b.text || '出错了')
      case 'divider': {
        const hr = document.createElement('hr')
        return hr
      }
      case 'compression':
        return el('div', 'hint', '上下文已压缩')
      default:
        return b.text ? el('div', 'msg assistant', b.text) : document.createDocumentFragment()
    }
  }

  function updateRunning() {
    $('stop').hidden = !state.running
    $('send').hidden = state.running
  }

  function scrollToEnd() {
    const box = $('messages')
    requestAnimationFrame(() => { box.scrollTop = box.scrollHeight })
  }

  // ---------- 事件流 ----------

  function disconnect() {
    if (pickTimer) {
      clearInterval(pickTimer)
      pickTimer = null
      $('ws-wait').hidden = true
    }
    state.disposed = true
    if (state.conn) {
      try { state.conn.close() } catch {}
      state.conn = null
    }
  }

  function connect() {
    if (!state.token || !state.sid) return
    disconnect()
    state.disposed = false
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const q = `token=${encodeURIComponent(state.token)}&workspace=${encodeURIComponent(state.ws)}&session_id=${encodeURIComponent(state.sid)}`
    const conn = new WebSocket(`${proto}://${location.host}/api/events?${q}`)
    state.conn = conn

    conn.onopen = () => { state.retry = 0 }
    conn.onmessage = (m) => {
      let ev
      try { ev = JSON.parse(m.data) } catch { return }
      onEvent(ev)
    }
    conn.onclose = () => {
      state.conn = null
      if (state.disposed) return
      const delay = Math.min(1000 * 2 ** state.retry, 15000)
      state.retry++
      setTimeout(connect, delay)
    }
  }

  function onEvent(ev) {
    switch (ev.event_type) {
      case 'content_chunk':
        state.running = true
        state.live.text += ev.content || ''
        patchLive()
        break
      case 'reasoning_chunk':
        state.running = true
        state.live.thinking += ev.content || ''
        patchLive()
        break
      case 'llm_start':
        state.running = true
        updateRunning()
        break
      case 'llm_end':
        if (typeof ev.content === 'string' && ev.content) state.live.text = ev.content
        patchLive()
        break
      case 'tool_run_start':
        state.running = true
        appendTool({
          kind: 'tool', tool_id: ev.id, name: ev.name, args: ev.arguments, status: 'running',
        })
        break
      case 'tool_run_end':
        updateTool(ev.id, ev.name, ev.result, ev.is_error || !!ev.error, ev.duration_ms)
        break
      case 'tool_approval_requested':
        state.approval = ev
        renderApproval()
        break
      case 'ask_user_question':
        state.questions = ev
        renderQuestions()
        break
      case 'agent_end':
        state.running = false
        state.live = { text: '', thinking: '' }
        updateRunning()
        setTimeout(() => { if (!state.disposed) openSession(state.sid) }, 400)
        break
      case 'session_persist_error':
        appendError('保存失败：' + (ev.error || ''))
        break
      default:
        if (ev.type === 'resync') openSession(state.sid)
        break
    }
  }

  function patchLive() {
    const box = $('messages')
    const last = box.lastElementChild
    // 只更新「尾部那一块」：live 文本永远是最后追加的助手气泡
    if (state.live.thinking && (!last || !last.classList.contains('thinking'))) {
      box.appendChild(el('div', 'msg thinking', state.live.thinking))
    }
    const nodes = box.querySelectorAll('.msg.assistant')
    let node = nodes[nodes.length - 1]
    if (!node || node.dataset.live !== '1') {
      node = el('div', 'msg assistant')
      node.dataset.live = '1'
      box.appendChild(node)
    }
    node.textContent = state.live.text
    node.classList.toggle('cursor', state.running)
    updateRunning()
    scrollToEnd()
  }

  function appendTool(t) {
    state.blocks.push(t)
    const box = $('messages')
    box.appendChild(renderBlock(t))
    scrollToEnd()
  }

  function updateTool(id, name, result, isError, dur) {
    const b = state.blocks.find((x) => x.kind === 'tool' && x.tool_id === id)
    if (b) {
      b.result = result
      b.is_error = isError
      b.status = 'done'
      b.dur_ms = dur
    }
    renderMessages()
    scrollToEnd()
  }

  function appendError(msg) {
    $('messages').appendChild(el('div', 'msg error', msg))
    scrollToEnd()
  }

  // ---------- HITL ----------

  function renderApproval() {
    const ev = state.approval
    if (!ev) return
    const body = $('approval-body')
    body.textContent = ''
    body.appendChild(el('div', 'hint', ev.name || ''))
    body.appendChild(el('pre', '', ev.arguments || ''))
    $('approval').hidden = false
  }

  async function decide(approve) {
    const ev = state.approval
    if (!ev) return
    $('approval').hidden = true
    state.approval = null
    try {
      await api('approve', { workspace: state.ws, session_id: state.sid, id: ev.id, approve })
      appendNote(approve ? '已同意' : '已拒绝')
    } catch (e) {
      appendError('审批失败：' + e.message)
    }
  }

  function renderQuestions() {
    const ev = state.questions
    if (!ev) return
    const body = $('questions-body')
    body.textContent = ''
    for (const q of ev.questions || []) {
      const wrap = el('div', 'q')
      wrap.appendChild(el('div', 'qt', q.question))
      const opts = q.options || []
      if (opts.length) {
        opts.forEach((o, i) => {
          const label = document.createElement('label')
          const input = document.createElement('input')
          input.type = q.multi ? 'checkbox' : 'radio'
          input.name = 'q_' + q.id
          input.value = typeof o === 'string' ? o : o.label
          label.append(input, el('span', '', input.value))
          wrap.appendChild(label)
        })
      }
      const free = document.createElement('input')
      free.type = 'text'
      free.placeholder = opts.length ? '其他答案（可空）' : '输入答案'
      free.dataset.qid = q.id
      free.className = 'free'
      wrap.appendChild(free)
      body.appendChild(wrap)
    }
    $('questions').hidden = false
  }

  async function submitQuestions(skip) {
    const ev = state.questions
    if (!ev) return
    const answers = []
    if (!skip) {
      for (const q of ev.questions || []) {
        const picked = [...document.querySelectorAll(`input[name="q_${CSS.escape(q.id)}"]:checked`)].map((x) => x.value)
        const free = document.querySelector(`input.free[data-qid="${CSS.escape(q.id)}"]`)
        const extra = free ? free.value.trim() : ''
        const parts = picked.concat(extra ? [extra] : [])
        if (parts.length) answers.push({ question_id: q.id, answer: parts.join(', ') })
      }
    }
    $('questions').hidden = true
    state.questions = null
    try {
      await api('answer_question', {
        workspace: state.ws, session_id: state.sid, batch_id: ev.id, answers,
      })
    } catch (e) {
      appendError('提交失败：' + e.message)
    }
  }

  // ---------- 输入 ----------

  async function send() {
    const input = $('input')
    const text = input.value.trim()
    if (!text || state.running) return
    input.value = ''
    input.style.height = 'auto'
    state.blocks.push({ kind: 'user', text })
    $('messages').appendChild(el('div', 'msg user', text))
    scrollToEnd()
    state.running = true
    state.live = { text: '', thinking: '' }
    updateRunning()
    try {
      await api('ask', { workspace: state.ws, session_id: state.sid, text })
    } catch (e) {
      state.running = false
      updateRunning()
      appendError(e.message)
    }
  }

  async function stop() {
    try {
      await api('interrupt', { workspace: state.ws, session_id: state.sid })
    } catch (e) {
      appendError(e.message)
    }
  }

  // ---------- 工具 ----------

  function el(tag, cls, text) {
    const n = document.createElement(tag)
    if (cls) n.className = cls
    if (text != null) n.textContent = text
    return n
  }

  function fmtTime(ms) {
    if (!ms) return ''
    const d = new Date(ms)
    const now = Date.now()
    if (now - ms < 86400000) return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
    return d.toLocaleDateString('zh-CN', { month: 'numeric', day: 'numeric' })
  }

  // ---------- 启动 ----------

  async function boot() {
    if (!state.token) { show('screen-pair'); return }
    try {
      await loadWorkspaces()
    } catch (e) {
      if (state.token) pairError(e.message)
      show('screen-pair')
    }
  }

  function bind() {
    $('pair-btn').addEventListener('click', doPair)
    $('pair-code').addEventListener('keydown', (e) => { if (e.key === 'Enter') doPair() })
    $('back').addEventListener('click', () => { disconnect(); loadSessions() })
    $('menu').addEventListener('click', async () => {
      if (confirm('切换工作区？')) { disconnect(); await loadWorkspaces() }
    })
    $('ws-select').addEventListener('change', () => { disconnect(); loadSessions() })
    $('ws-add').addEventListener('click', addWorkspace)
    $('ws-pick').addEventListener('click', pickOnDesktop)
    $('new-session').addEventListener('click', newSession)
    $('send').addEventListener('click', send)
    $('stop').addEventListener('click', stop)
    $('approval-approve').addEventListener('click', () => decide(true))
    $('approval-reject').addEventListener('click', () => decide(false))
    $('questions-submit').addEventListener('click', () => submitQuestions(false))
    $('questions-skip').addEventListener('click', () => submitQuestions(true))
    const input = $('input')
    input.addEventListener('input', () => {
      input.style.height = 'auto'
      input.style.height = Math.min(input.scrollHeight, 120) + 'px'
    })
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
        e.preventDefault()
        send()
      }
    })
    window.addEventListener('beforeunload', disconnect)
  }

  // 扫码进入：桌面端二维码把配对码放在 hash 里（#pair=123456）。
  // 用掉即从地址栏抹掉，刷新页面不会重复配对（码本来就是单次的）。
  function pairFromHash() {
    const m = location.hash.match(/pair=(\d{6})/)
    if (!m) return false
    history.replaceState(null, '', location.pathname + location.search)
    if (state.token) return false
    $('pair-code').value = m[1]
    return true
  }

  bind()
  if (pairFromHash()) doPair()
  else boot()
})()
