// HAI 手机遥控器：配对 → 工作区管理 → 会话（含子智能体）→ 流式 + HITL。
// 无构建、无依赖；渲染只碰 textContent / createElement（不拼 HTML）。
(() => {
  'use strict'

  const $ = (id) => document.getElementById(id)
  const LS = { token: 'hai.token', ws: 'hai.ws', sid: 'hai.sid', name: 'hai.name' }
  const SCREENS = ['pair', 'list', 'workspaces', 'chat', 'agents', 'agent']

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
    lastMsgAt: 0, // 最近一次收到任何帧的时刻（链路看门狗用）
    chunkKey: '', // 当前流式请求标识（request_id/run_id）+ 已处理的块序号
    chunkIdx: -1,
    screen: 'pair',
    nav: [],
    agents: new Map(), // task_id → { task_id, label, status, result }
    agentTask: '',
    agentEntries: [],
    agentPoll: null,
  }

  // 主题：与桌面同一套 data-theme（ink=墨 / paper=宣纸），跟随系统
  const themeMQ = window.matchMedia('(prefers-color-scheme: light)')
  function applyTheme() {
    document.documentElement.dataset.theme = themeMQ.matches ? 'paper' : 'ink'
  }
  applyTheme()
  themeMQ.addEventListener?.('change', applyTheme)

  // ---------- 基础 ----------

  function show(screen) {
    state.screen = screen
    for (const s of SCREENS) $('screen-' + s).hidden = s !== screen
    $('back').hidden = screen === 'pair' || screen === 'list'
    $('menu').hidden = screen !== 'chat'
    $('title').textContent = titleFor(screen)
  }

  function titleFor(screen) {
    switch (screen) {
      case 'workspaces':
        return '工作区'
      case 'agents':
        return '子智能体'
      case 'agent':
        return state.agentTask ? labelOf(state.agentTask) : '子智能体'
      case 'chat':
        return sessionTitle() || workspaceTitle() || '会话'
      default:
        return 'HAI'
    }
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
    if (!r.ok) throw new Error(data.error || 'HTTP ' + r.status)
    if (data.ok === false) throw new Error(data.error || '命令失败')
    return data.data || {}
  }

  function resetAuth() {
    state.token = ''
    localStorage.removeItem(LS.token)
    disconnect()
    show('pair')
  }

  function el(tag, cls, text) {
    const n = document.createElement(tag)
    if (cls) n.className = cls
    if (text != null) n.textContent = text
    return n
  }

  // 轻量 markdown：``` 代码块、`行内代码`、**粗体**。
  // 只用于**定稿**消息（记录/快照）；流式过程每帧重排太贵，直接 textContent 出纯文本。
  function mdLite(text) {
    const frag = document.createDocumentFragment()
    String(text)
      .split('```')
      .forEach((seg, i) => {
        if (i % 2 === 1) {
          // 代码块：首行若是语言标注（```js）丢掉
          const body = seg.replace(/^[^\n]*\n/, '').replace(/\n+$/, '')
          frag.appendChild(el('pre', '', body))
          return
        }
        for (const part of seg.split(/(`[^`\n]+`|\*\*[^*\n]+\*\*)/)) {
          if (!part) continue
          if (part.length > 2 && part.startsWith('`') && part.endsWith('`')) {
            frag.appendChild(el('code', '', part.slice(1, -1)))
          } else if (part.length > 4 && part.startsWith('**') && part.endsWith('**')) {
            frag.appendChild(el('b', '', part.slice(2, -2)))
          } else {
            frag.appendChild(document.createTextNode(part))
          }
        }
      })
    return frag
  }

  function fmtTime(ms) {
    if (!ms) return ''
    const d = new Date(ms)
    if (Date.now() - ms < 86400000) return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
    return d.toLocaleDateString('zh-CN', { month: 'numeric', day: 'numeric' })
  }

  function workspaceTitle() {
    const w = state.workspaces.find((x) => x.path === state.ws)
    return w ? w.title || w.path : ''
  }

  function sessionTitle() {
    const s = state.sessions.find((x) => x.id === state.sid)
    return s ? s.title : ''
  }

  // ---------- 配对 ----------

  function pairError(msg) {
    const e = $('pair-error')
    e.textContent = msg
    e.hidden = !msg
  }

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

  // ---------- 工作区 ----------

  async function loadWorkspaces() {
    const data = await api('workspace_list')
    state.workspaces = data.workspaces || []
    renderWorkspaceSelect()
    if (!state.workspaces.length) {
      state.ws = ''
      state.sessions = []
      renderSessions()
      show('list')
      $('list-empty').hidden = false
      $('list-empty').textContent = '还没有工作区，点右上角 ⚙ 添加。'
      return
    }
    const saved = localStorage.getItem(LS.ws)
    state.ws = state.workspaces.some((w) => w.path === saved) ? saved : state.workspaces[0].path
    await loadSessions()
  }

  function renderWorkspaceSelect() {
    const sel = $('ws-select')
    sel.textContent = ''
    for (const w of state.workspaces) {
      const o = document.createElement('option')
      o.value = w.path
      o.textContent = (w.pinned ? '★ ' : '') + (w.title || w.path)
      sel.appendChild(o)
    }
    if (state.ws) sel.value = state.ws
  }

  async function loadSessions() {
    state.ws = $('ws-select').value || state.ws
    localStorage.setItem(LS.ws, state.ws)
    const data = await api('list', { workspace: state.ws })
    state.sessions = data.sessions || []
    renderSessions()
    show('list')
  }

  function renderSessions() {
    const ul = $('sessions')
    ul.textContent = ''
    $('list-empty').hidden = state.sessions.length > 0
    for (const s of state.sessions) {
      const li = document.createElement('li')
      const t = el('span', 't', (s.pinned ? '★ ' : '') + (s.title || s.id))
      const m = el('span', 'm', fmtTime(s.updated_at))
      li.append(t, m)
      if (s.running) li.appendChild(el('span', 'dot'))
      const more = el('button', 'icon more', '⋯')
      more.addEventListener('click', (e) => {
        e.stopPropagation()
        openSessionActions(s)
      })
      li.appendChild(more)
      li.addEventListener('click', () => openSession(s.id))
      ul.appendChild(li)
    }
  }

  function openSessionActions(s) {
    openActions(s.title || s.id, [
      { label: '重命名', fn: () => renameSession(s) },
      { label: s.pinned ? '取消置顶' : '置顶', fn: () => pinSession(s, !s.pinned) },
      { label: '删除会话', danger: true, fn: () => deleteSession(s) },
    ])
  }

  async function renameSession(s) {
    const title = prompt('会话标题（留空恢复自动标题）', s.title || '')
    if (title === null) return
    try {
      await api('session_rename', { workspace: state.ws, session_id: s.id, title: title.trim() })
      await loadSessions()
    } catch (e) {
      alert(e.message)
    }
  }

  async function pinSession(s, pinned) {
    try {
      await api('session_pin', { workspace: state.ws, session_id: s.id, pinned })
      await loadSessions()
    } catch (e) {
      alert(e.message)
    }
  }

  async function deleteSession(s) {
    if (!confirm(`删除「${s.title || s.id}」？会话记录与事件日志都会删掉。`)) return
    try {
      await api('delete_session', { workspace: state.ws, session_id: s.id })
      if (state.sid === s.id) {
        state.sid = ''
        disconnect()
      }
      await loadSessions()
    } catch (e) {
      alert(e.message)
    }
  }

  // ---------- 工作区管理页 ----------

  function renderWorkspaces() {
    const ul = $('ws-list')
    ul.textContent = ''
    for (const w of state.workspaces) {
      const li = document.createElement('li')
      const left = el('div', 'ws-left')
      left.append(el('div', 'ws-title', (w.pinned ? '★ ' : '') + (w.title || w.path)))
      left.append(el('div', 'ws-path', w.path))
      li.appendChild(left)
      const more = el('button', 'icon more', '⋯')
      more.addEventListener('click', (e) => {
        e.stopPropagation()
        openActions(w.title || w.path, [
          { label: w.pinned ? '取消置顶' : '置顶', fn: () => updateWorkspace(w, { pinned: !w.pinned }) },
          {
            label: '重命名',
            fn: async () => {
              const title = prompt('工作区名称（留空恢复目录名）', w.title || '')
              if (title === null) return
              await updateWorkspace(w, { title: title.trim() })
            },
          },
          { label: '移除（不删文件）', danger: true, fn: () => removeWorkspace(w) },
        ])
      })
      li.appendChild(more)
      li.addEventListener('click', async () => {
        state.ws = w.path
        $('ws-select').value = w.path
        await loadSessions()
      })
      ul.appendChild(li)
    }
    if (!state.workspaces.length) {
      ul.appendChild(el('li', 'hint', '还没有工作区。'))
    }
  }

  async function updateWorkspace(w, patch) {
    try {
      const data = await api('workspace_update', { path: w.path, ...patch })
      state.workspaces = data.workspaces || state.workspaces
      renderWorkspaces()
      renderWorkspaceSelect()
    } catch (e) {
      alert(e.message)
    }
  }

  async function removeWorkspace(w) {
    if (!confirm(`从列表移除「${w.title || w.path}」？只移除登记，不删磁盘文件。`)) return
    try {
      const data = await api('workspace_remove', { path: w.path })
      state.workspaces = data.workspaces || []
      if (state.ws === w.path && state.workspaces[0]) state.ws = state.workspaces[0].path
      renderWorkspaces()
      renderWorkspaceSelect()
    } catch (e) {
      alert(e.message)
    }
  }

  async function addWorkspaceByPath() {
    const path = prompt('工作区绝对路径（如 /Users/you/project）')
    if (!path) return
    try {
      await api('workspace_add', { path: path.trim() })
      await loadWorkspaces()
      renderWorkspaces()
    } catch (e) {
      alert(e.message)
    }
  }

  // 让电脑弹目录选择器：手机只发请求，然后轮询工作区清单。
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
          state.workspaces = list
          renderWorkspaces()
          renderWorkspaceSelect()
          return
        }
      } catch {
        /* 网络抖动继续等 */
      }
      if (Date.now() - started > 90000) done()
    }, 1500)
  }

  // ---------- 会话 ----------

  async function newSession() {
    const data = await api('new_session', { workspace: state.ws, blocks_limit: 30 })
    state.sid = data.session_id
    localStorage.setItem(LS.sid, state.sid)
    await openSession(state.sid)
  }

  async function openSession(sid) {
    // bridge 的恢复字段是 id（历史协议），session_id 只是兼容写法 —— 两个都传
    const data = await api('new_session', { workspace: state.ws, id: sid, session_id: sid, blocks_limit: 30 })
    state.sid = data.session_id || sid
    localStorage.setItem(LS.sid, state.sid)
    state.blocks = parseBlocks(data.view_checkpoint)
    state.cursor = data.blocks_cursor || 0
    state.hasMore = !!data.blocks_has_more
    state.live = { text: '', thinking: '' }
    state.chunkKey = ''
    state.chunkIdx = -1
    state.running = false
    state.agents = new Map()
    for (const a of data.agents || []) {
      if (a && a.task_id) {
        state.agents.set(a.task_id, {
          task_id: a.task_id,
          label: a.label || a.task_id,
          status: a.status || '',
          result: a.task_result || '',
        })
      }
    }
    renderMessages()
    renderAgentsBar()
    show('chat')
    connect()
    scrollToEnd()
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
    if (state.live.thinking) {
      const t = el('div', 'msg thinking', state.live.thinking)
      t.dataset.live = '1'
      box.appendChild(t)
    }
    if (state.live.text) {
      const n = el('div', 'msg assistant' + (state.running ? ' cursor' : ''), state.live.text)
      n.dataset.live = '1'
      box.appendChild(n)
    }
    updateRunning()
  }

  function userMsg(text) {
    const wrap = el('div', 'msg user')
    wrap.appendChild(el('div', 'body', text || ''))
    return wrap
  }

  function renderBlock(b) {
    switch (b.kind) {
      case 'user':
        return userMsg(b.text || '')
      case 'assistant': {
        const wrap = document.createDocumentFragment()
        if (b.thinking) wrap.appendChild(el('div', 'msg thinking', b.thinking))
        if (b.text) {
          const node = el('div', 'msg assistant')
          node.appendChild(mdLite(b.text))
          wrap.appendChild(node)
        }
        return wrap
      }
      case 'tool': {
        const d = document.createElement('details')
        d.className = 'tool-group'
        const sum = document.createElement('summary')
        sum.append(el('span', 'name', b.name || 'tool'))
        sum.appendChild(el('span', 'st', (b.status || '') + (b.is_error ? ' · 失败' : '')))
        d.appendChild(sum)
        if (b.args) d.appendChild(el('pre', '', b.args))
        if (b.result) d.appendChild(el('pre', '', b.result))
        return d
      }
      case 'agent': {
        // 子智能体卡片：点开看它自己的转录
        const d = document.createElement('button')
        d.className = 'agent-card'
        d.textContent = `子智能体 · ${b.label || b.task_id || ''} · ${b.status || ''}`
        d.addEventListener('click', () => openAgent(b.task_id))
        return d
      }
      case 'error':
        return el('div', 'msg error', b.error || b.text || '出错了')
      case 'divider':
        return document.createElement('hr')
      case 'compression':
        return el('div', 'msg system', '上下文已压缩')
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
    requestAnimationFrame(() => {
      box.scrollTop = box.scrollHeight
    })
  }

  async function loadEarlier() {
    if (!state.hasMore || !state.cursor) return
    const box = $('messages')
    const before = box.scrollHeight
    const btn = $('load-earlier')
    if (btn) btn.disabled = true
    try {
      const data = await api('blocks_page', {
        workspace: state.ws,
        session_id: state.sid,
        before_id: state.cursor,
        limit: 50,
      })
      state.blocks = (data.blocks || []).concat(state.blocks)
      state.cursor = data.cursor || 0
      state.hasMore = !!data.has_more
      renderMessages()
      box.scrollTop = box.scrollHeight - before
    } catch (e) {
      appendError(e.message)
    }
  }

  // ---------- 子智能体 ----------

  function renderAgentsBar() {
    const bar = $('agents-bar')
    const list = [...state.agents.values()]
    if (!list.length) {
      bar.hidden = true
      return
    }
    const running = list.filter((a) => a.status === 'running').length
    bar.hidden = false
    bar.textContent = `子智能体 ${list.length}${running ? ` · 运行中 ${running}` : ''} ›`
  }

  function labelOf(taskId) {
    const a = state.agents.get(taskId)
    return (a && a.label) || taskId
  }

  function renderAgentsList() {
    const ul = $('agents-list')
    ul.textContent = ''
    const list = [...state.agents.values()]
    $('agents-empty').hidden = list.length > 0
    for (const a of list) {
      const li = document.createElement('li')
      const t = el('span', 't', a.label || a.task_id)
      const m = el('span', 'm', a.status || '')
      li.append(t, m)
      if (a.status === 'running') li.appendChild(el('span', 'dot'))
      li.addEventListener('click', () => openAgent(a.task_id))
      ul.appendChild(li)
    }
  }

  async function openAgents() {
    renderAgentsList()
    state.nav.push('chat')
    show('agents')
  }

  async function openAgent(taskId) {
    if (!taskId) return
    state.agentTask = taskId
    state.agentEntries = []
    $('agent-messages').textContent = ''
    $('agent-meta').textContent = ''
    state.nav.push(state.screen === 'agents' ? 'agents' : 'chat')
    show('agent')
    await loadAgentHistory()
  }

  async function loadAgentHistory() {
    const taskId = state.agentTask
    if (!taskId) return
    try {
      const first = await api('agent_history', { workspace: state.ws, session_id: state.sid, task_id: taskId, offset: 0, limit: 200 })
      let entries = first.entries || []
      const total = first.total || entries.length
      if (first.has_more && total > 200) {
        const tail = await api('agent_history', {
          workspace: state.ws,
          session_id: state.sid,
          task_id: taskId,
          offset: Math.max(0, total - 200),
          limit: 200,
        })
        entries = tail.entries || entries
      }
      state.agentEntries = entries
      renderAgentEntries()
      const a = state.agents.get(taskId)
      $('agent-meta').textContent = `${a ? a.status : ''}${total ? ` · ${total} 条记录` : ''}`
      const running = a && a.status === 'running'
      $('agent-stop').hidden = !running
      $('agent-send').hidden = !!running && false
      if (running) startAgentPoll()
      else stopAgentPoll()
    } catch (e) {
      $('agent-messages').appendChild(el('div', 'msg error', e.message))
    }
  }

  function renderAgentEntries() {
    const box = $('agent-messages')
    box.textContent = ''
    for (const e of state.agentEntries) {
      if (e.kind === 'user') box.appendChild(el('div', 'msg user', e.text || ''))
      else if (e.kind === 'assistant') box.appendChild(el('div', 'msg assistant', e.text || ''))
      else if (e.kind === 'tool') {
        const d = document.createElement('details')
        d.className = 'tool'
        const s = document.createElement('summary')
        s.textContent = e.name || 'tool'
        d.appendChild(s)
        if (e.args) d.appendChild(el('pre', '', e.args))
        if (e.result) d.appendChild(el('pre', '', e.result))
        box.appendChild(d)
      }
    }
    if (!state.agentEntries.length) box.appendChild(el('div', 'hint', '还没有记录。'))
    box.scrollTop = box.scrollHeight
  }

  function startAgentPoll() {
    if (state.agentPoll) return
    state.agentPoll = setInterval(() => {
      if (state.screen !== 'agent') return stopAgentPoll()
      loadAgentHistory()
    }, 2500)
  }

  function stopAgentPoll() {
    if (state.agentPoll) {
      clearInterval(state.agentPoll)
      state.agentPoll = null
    }
  }

  async function sendToAgent() {
    const input = $('agent-input')
    const text = input.value.trim()
    if (!text || !state.agentTask) return
    input.value = ''
    input.style.height = 'auto'
    try {
      await api('agent_send', { workspace: state.ws, session_id: state.sid, task_id: state.agentTask, text })
      await loadAgentHistory()
    } catch (e) {
      alert(e.message)
    }
  }

  async function stopAgent() {
    if (!state.agentTask) return
    try {
      await api('interrupt_task', { workspace: state.ws, session_id: state.sid, task_id: state.agentTask })
      await loadAgentHistory()
    } catch (e) {
      alert(e.message)
    }
  }

  // ---------- 事件流 ----------

  // 每建一条连接 generation +1，旧连接的回调一律作废。
  // 原因：Safari 息屏/切后台后 WebSocket 半死不活，旧连接仍可能继续推消息；
  // 两条连接同时活着时，同一帧被 onEvent 处理两遍 —— 表现就是每个 token 都重复
  // （"add add a a"），而落盘记录是干净的。
  let connGen = 0
  let retryTimer = null

  function disconnect() {
    if (pickTimer) {
      clearInterval(pickTimer)
      pickTimer = null
      $('ws-wait').hidden = true
    }
    stopAgentPoll()
    state.disposed = true
    if (retryTimer) {
      clearTimeout(retryTimer)
      retryTimer = null
    }
    connGen++
    if (state.conn) {
      try {
        state.conn.close()
      } catch {
        /* ignore */
      }
      state.conn = null
    }
  }

  // 同一条事件被投递两遍的兜底：帧内容逐字节相同才丢弃（时间戳不同即视为新帧，
  // 所以正常的重复词 "the the" 不会被误删）。
  const seenFrames = new Map()
  function isDuplicateFrame(raw) {
    const now = Date.now()
    const prev = seenFrames.get(raw)
    if (prev !== undefined && now - prev < 3000) return true
    seenFrames.set(raw, now)
    if (seenFrames.size > 600) {
      for (const [k, t] of seenFrames) if (now - t > 3000) seenFrames.delete(k)
    }
    return false
  }

  function connect() {
    if (!state.token || !state.sid) return
    disconnect()
    state.disposed = false
    const gen = connGen
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const q = `token=${encodeURIComponent(state.token)}&workspace=${encodeURIComponent(state.ws)}&session_id=${encodeURIComponent(state.sid)}`
    const conn = new WebSocket(`${proto}://${location.host}/api/events?${q}`)
    state.conn = conn
    conn.onopen = () => {
      if (gen !== connGen) return
      state.retry = 0
      state.lastMsgAt = Date.now() // 刚连上：别让看门狗把新连接判成「没消息」
    }
    conn.onmessage = (m) => {
      if (gen !== connGen) return
      state.lastMsgAt = Date.now()
      if (isDuplicateFrame(m.data)) return
      let ev
      try {
        ev = JSON.parse(m.data)
      } catch {
        return
      }
      onEvent(ev)
    }
    conn.onclose = () => {
      if (gen !== connGen) return
      state.conn = null
      if (state.disposed) return
      const delay = Math.min(1000 * 2 ** state.retry, 15000)
      state.retry++
      if (retryTimer) clearTimeout(retryTimer)
      retryTimer = setTimeout(() => {
        retryTimer = null
        connect()
      }, delay)
    }
  }

  function onEvent(ev) {
    switch (ev.event_type) {
      case 'content_chunk':
        if (isStaleChunk(ev)) break
        state.running = true
        state.live.text += ev.content || ''
        scheduleLive()
        break
      case 'reasoning_chunk':
        if (isStaleChunk(ev)) break
        state.running = true
        state.live.thinking += ev.content || ''
        scheduleLive()
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
        appendTool({ kind: 'tool', tool_id: ev.id, name: ev.name, args: ev.arguments, status: 'running' })
        break
      case 'tool_run_end':
        updateTool(ev.id, ev.result, ev.is_error || !!ev.error)
        break
      case 'tool_approval_requested':
        state.approval = ev
        renderApproval()
        break
      case 'ask_user_question':
        state.questions = ev
        renderQuestions()
        break
      case 'task_started':
        if (ev.task_id) {
          state.agents.set(ev.task_id, {
            task_id: ev.task_id,
            label: ev.name || ev.task_id,
            status: ev.status || 'running',
          })
          renderAgentsBar()
          if (state.screen === 'agents') renderAgentsList()
        }
        break
      case 'task_end':
        if (ev.task_id) {
          const a = state.agents.get(ev.task_id) || { task_id: ev.task_id, label: ev.task_id }
          a.status = ev.status || 'done'
          a.result = ev.result || ev.error || ''
          state.agents.set(ev.task_id, a)
          renderAgentsBar()
          if (state.screen === 'agents') renderAgentsList()
          if (state.screen === 'agent' && state.agentTask === ev.task_id) loadAgentHistory()
        }
        break
      case 'task_message':
        if (state.screen === 'agent' && state.agentTask === ev.task_id) loadAgentHistory()
        break
      case 'agent_end':
        state.running = false
        state.live = { text: '', thinking: '' }
        state.chunkKey = ''
        state.chunkIdx = -1
        $('jump').hidden = true
        updateRunning()
        setTimeout(() => {
          if (!state.disposed) openSession(state.sid)
        }, 400)
        break
      case 'session_persist_error':
        appendError('保存失败：' + (ev.error || ''))
        break
      default:
        // 服务端帧（没有 event_type）
        if (ev.type === 'live') {
          applyLiveFrame(ev)
        } else if (ev.type === 'resync') {
          // 老服务端才发 resync：整页重拉快照
          openSession(state.sid)
        }
        // type === 'ping'：只用来刷新 lastMsgAt（已在 onmessage 里更新）
        break
    }
  }

  function patchLive() {
    const box = $('messages')
    // 只有已经贴在底部时才自动跟随；否则用户正在往回读历史，不该被拽走
    const stick = nearBottom(box)
    if (state.live.thinking) {
      let t = box.querySelector('.msg.thinking[data-live="1"]')
      if (!t) {
        t = el('div', 'msg thinking')
        t.dataset.live = '1'
        box.appendChild(t)
      }
      t.textContent = state.live.thinking
    }
    let node = box.querySelector('.msg.assistant[data-live="1"]')
    if (!node) {
      node = el('div', 'msg assistant')
      node.dataset.live = '1'
      box.appendChild(node)
    }
    node.textContent = state.live.text
    node.classList.toggle('cursor', state.running)
    updateRunning()
    if (stick) scrollToEnd()
    // 没贴底说明用户在往回读：给一个「最新」入口，别让流式内容"消失"在下面
    $('jump').hidden = stick || !state.running
  }

  function nearBottom(box) {
    return box.scrollHeight - box.scrollTop - box.clientHeight < 80
  }

  // 流式块去重：同一 (request_id, index) 只处理一次。
  // 两条连接各投一遍、或重连补发时 index 不会前进 —— 直接丢，比字符串比较精确。
  function isStaleChunk(ev) {
    if (typeof ev.index !== 'number') return false
    const key = ev.request_id || ev.run_id || ''
    if (key !== state.chunkKey) {
      state.chunkKey = key
      state.chunkIdx = ev.index
      return false
    }
    if (ev.index > state.chunkIdx) {
      state.chunkIdx = ev.index
      return false
    }
    // index 回退到 0 = 新一轮请求（provider 没给 request_id 时靠这条兜底）
    if (ev.index === 0 && state.chunkIdx > 0) {
      state.chunkIdx = 0
      return false
    }
    return true
  }

  // 每帧最多重排一次：chunk 可能每秒几十条，逐条改 textContent 会让手机掉帧。
  let liveRaf = 0
  function scheduleLive() {
    if (liveRaf) return
    liveRaf = requestAnimationFrame(() => {
      liveRaf = 0
      patchLive()
    })
  }

  // 服务端 live 快照：直接替换「正在流的那一段」（重连/丢帧自愈）。
  function applyLiveFrame(ev) {
    const box = $('messages')
    for (const n of box.querySelectorAll('[data-live="1"]')) n.remove()
    state.live = { text: ev.text || '', thinking: ev.thinking || '' }
    state.running = !!ev.running
    state.chunkKey = ''
    state.chunkIdx = -1
    updateRunning()
    if (state.live.text || state.live.thinking) patchLive()
    else $('jump').hidden = true
  }

  function appendTool(t) {
    state.blocks.push(t)
    $('messages').appendChild(renderBlock(t))
    scrollToEnd()
  }

  function updateTool(id, result, isError) {
    const b = state.blocks.find((x) => x.kind === 'tool' && x.tool_id === id)
    if (b) {
      b.result = result
      b.is_error = isError
      b.status = 'done'
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
      opts.forEach((o) => {
        const label = document.createElement('label')
        const input = document.createElement('input')
        input.type = q.multi ? 'checkbox' : 'radio'
        input.name = 'q_' + q.id
        input.value = typeof o === 'string' ? o : o.label
        label.append(input, el('span', '', input.value))
        wrap.appendChild(label)
      })
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
      await api('answer_question', { workspace: state.ws, session_id: state.sid, batch_id: ev.id, answers })
    } catch (e) {
      appendError('提交失败：' + e.message)
    }
  }

  // ---------- 通用动作面板 ----------

  function openActions(title, items) {
    $('actions-title').textContent = title
    const body = $('actions-body')
    body.textContent = ''
    for (const it of items) {
      const b = el('button', it.danger ? 'danger' : 'ghost', it.label)
      b.style.width = '100%'
      b.style.marginBottom = '8px'
      b.addEventListener('click', async () => {
        $('actions').hidden = true
        await it.fn()
      })
      body.appendChild(b)
    }
    $('actions').hidden = false
  }

  // ---------- 输入 ----------

  async function send() {
    const input = $('input')
    const text = input.value.trim()
    if (!text || state.running) return
    input.value = ''
    input.style.height = 'auto'
    state.blocks.push({ kind: 'user', text })
    $('messages').appendChild(userMsg(text))
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

  // ---------- 导航 ----------

  function navBack() {
    stopAgentPoll()
    const prev = state.nav.pop()
    if (prev === 'agents') {
      renderAgentsList()
      show('agents')
      return
    }
    if (prev === 'chat' && state.sid) {
      show('chat')
      return
    }
    if (state.screen === 'chat') {
      disconnect()
      loadSessions()
      return
    }
    if (state.screen === 'agent') {
      renderAgentsList()
      show('agents')
      return
    }
    loadWorkspaces()
  }

  // ---------- 启动 ----------

  async function boot() {
    if (!state.token) {
      show('pair')
      return
    }
    try {
      await loadWorkspaces()
    } catch (e) {
      if (state.token) pairError(e.message)
      show('pair')
    }
  }

  // ---------- 链路自愈 ----------
  // iOS 息屏/切后台后 socket 常常「假活」：onclose 不触发，事件却不再到达。
  // 两个手段：回前台立刻检查并重建；运行中长时间收不到任何帧（含服务端 15s 心跳）也重建。
  // 重建后服务端会在 subscribe 时推一帧 live 快照，屏幕不会空。
  const WATCHDOG_MS = 45000
  function reviveConn() {
    if (state.screen !== 'chat' && state.screen !== 'agent') return
    state.retry = 0
    connect()
  }
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) return
    if (!state.conn || state.conn.readyState !== WebSocket.OPEN) reviveConn()
  })
  setInterval(() => {
    if (state.disposed) return
    if (state.screen !== 'chat' && state.screen !== 'agent') return
    if (Date.now() - state.lastMsgAt < WATCHDOG_MS) return
    reviveConn()
  }, 10000)

  function bind() {
    $('pair-btn').addEventListener('click', doPair)
    $('pair-code').addEventListener('keydown', (e) => {
      if (e.key === 'Enter') doPair()
    })
    $('back').addEventListener('click', navBack)
    $('menu').addEventListener('click', () => {
      const s = state.sessions.find((x) => x.id === state.sid)
      if (s) openSessionActions(s)
    })
    $('ws-select').addEventListener('change', () => {
      disconnect()
      loadSessions()
    })
    $('ws-manage').addEventListener('click', () => {
      renderWorkspaces()
      state.nav.push('list')
      show('workspaces')
    })
    $('ws-pick').addEventListener('click', pickOnDesktop)
    $('ws-manual').addEventListener('click', addWorkspaceByPath)
    $('new-session').addEventListener('click', newSession)
    $('agents-bar').addEventListener('click', openAgents)
    $('send').addEventListener('click', send)
    $('jump').addEventListener('click', () => {
      $('jump').hidden = true
      scrollToEnd()
    })
    $('stop').addEventListener('click', stop)
    $('agent-send').addEventListener('click', sendToAgent)
    $('agent-stop').addEventListener('click', stopAgent)
    $('approval-approve').addEventListener('click', () => decide(true))
    $('approval-reject').addEventListener('click', () => decide(false))
    $('questions-submit').addEventListener('click', () => submitQuestions(false))
    $('questions-skip').addEventListener('click', () => submitQuestions(true))
    $('actions-cancel').addEventListener('click', () => {
      $('actions').hidden = true
    })
    for (const [id, fn] of [
      ['input', () => {}],
      ['agent-input', () => {}],
    ]) {
      const t = $(id)
      t.addEventListener('input', () => {
        t.style.height = 'auto'
        t.style.height = Math.min(t.scrollHeight, 120) + 'px'
      })
      t.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
          e.preventDefault()
          id === 'input' ? send() : sendToAgent()
        }
      })
      void fn
    }
    window.addEventListener('beforeunload', disconnect)
  }

  // 扫码进入：桌面端二维码把配对码放在 hash 里（#pair=123456）。
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
