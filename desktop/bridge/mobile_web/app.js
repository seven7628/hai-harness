// HAI 手机遥控器：配对 → 工作区管理 → 会话（含子智能体）→ 流式 + HITL。
// 无构建、无依赖；渲染只碰 textContent / createElement（不拼 HTML）。
(() => {
  'use strict'

  const $ = (id) => document.getElementById(id)
  // 纯逻辑层（hub_model.js）：seq 合并/@ 分词/任务卡/事件分流都在这里。
  const H = window.HaiMobileHubModel || null
  // 头像生成器（avatar_model.js）：与桌面 avatar.ts 同算法的纯 JS 副本。
  const A = window.HaiMobileAvatar || null
  const LS = { token: 'hai.token', ws: 'hai.ws', sid: 'hai.sid', name: 'hai.name', theme: 'hai.theme' }
  const SCREENS = ['pair', 'list', 'workspaces', 'chat', 'agents', 'agent', 'hub', 'hub-group', 'hub-dm']
  const HUB_SCREENS = new Set(['hub', 'hub-group', 'hub-dm'])

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
    chunkSeen: new Map(), // (类型|request_id) → 已处理的最大块序号
    screen: 'pair',
    nav: [],
    agents: new Map(), // task_id → { task_id, label, status, result }
    agentTask: '',
    agentEntries: [],
    agentPoll: null,
    // Agent Hub：与聊天流完全独立的连接/快照/时间线状态。
    hub: {
      conn: null,
      gen: 0,
      retry: 0,
      retryTimer: null,
      disposed: true,
      hasConnected: false,
      lastMsgAt: 0,
      seenFrames: new Map(),
      agents: [],
      agentsById: new Map(),
      groups: [],
      progress: {},
      dm: {},
      running: {},
      convStatus: {},
      readSeq: {},
      alerts: [],
      needs: [],
      homeSeq: 0,
      homeLoading: false,
      homePending: false,
      homeTimer: null,
      activeGroupId: '',
      activeAgentId: '',
      group: null,
      conversation: null,
      groupMessages: [],
      groupHasMore: false,
      groupLoading: false,
      groupSeq: 0,
      drafts: [],
      deltaIndex: {},
      dmAgent: null,
      dmMessages: [],
      dmHasMore: false,
      dmLoading: false,
      dmSeq: 0,
      dmAwaiting: false,
      sheetKind: '',
      tasks: [],
      sheetNeeds: [],
      needsGroupId: '',
      mention: { open: false, query: '', index: 0 },
      // 失败草稿的发送幂等槽（{text,target,cid}）：重试复用首次 client_msg_id，
      // 成功/改动草稿才清空（见 hub_model.sendSlotID）。群与 DM 各一条。
      pendingGroupSend: null,
      pendingDMSend: null,
      // 全历史搜索（hub_search）：输入 debounce 250ms，结果按 (created_at,id) 游标翻页。
      search: {
        open: false,
        query: '',
        scope: 'all',
        hits: [],
        hasMore: false,
        next: null,
        loading: false,
        error: '',
        gen: 0,
        timer: null,
      },
      busy: false,
    },
  }

  // 无头/隐私模式的浏览器（以及部分 App 内嵌 WebView）不持久化 localStorage：
  // 每次启动都会丢凭据、被迫重新配对（服务端 token 其实永不过期）。
  // 兜底两条：cookie 作第二存储；凭据也可以直接写在 URL 里 —— ?t=<token> 或 #t=<token>，
  // 固定导航到这条 URL（或存书签）就永远不用再输配对码。它等价于密码，别外传。
  function tokenFromURL() {
    const h = location.hash.match(/[#&]t=([0-9a-f]{64})/i)
    if (h) return h[1]
    const q = new URLSearchParams(location.search).get('t') || ''
    return /^[0-9a-f]{64}$/i.test(q) ? q : ''
  }

  function cookieToken() {
    const m = document.cookie.match(/(?:^|;\s*)hai_token=([0-9a-f]{64})/i)
    return m ? m[1] : ''
  }

  function rememberToken(tok) {
    state.token = tok
    try {
      localStorage.setItem(LS.token, tok)
    } catch {
      /* 隐私模式：忽略 */
    }
    try {
      document.cookie = 'hai_token=' + tok + '; path=/; max-age=31536000; SameSite=Lax'
    } catch {
      /* ignore */
    }
  }

  // 免配对链接：把当前凭据写进 URL，给无持久化存储的浏览器用
  function bookmarkURL() {
    return `${location.origin}/m/?t=${encodeURIComponent(state.token)}`
  }

  // 主题：与桌面同一套 data-theme（ink=墨 / paper=宣纸）。
  // 三档模式（system/ink/paper）存 localStorage；system 只在系统外观变化时跟随，
  // 手动选了 ink/paper 后系统切换不再抢方向盘。
  const themeMQ = window.matchMedia('(prefers-color-scheme: light)')
  let themeMode = H ? H.normalizeThemeMode(localStorage.getItem(LS.theme)) : 'system'

  function themeLabel() {
    if (themeMode === 'system') return '跟随系统'
    return themeMode === 'paper' ? '宣纸' : '墨'
  }

  function applyTheme() {
    const theme = H ? H.resolveTheme(themeMode, themeMQ.matches) : themeMode === 'system' ? (themeMQ.matches ? 'paper' : 'ink') : themeMode
    document.documentElement.dataset.theme = theme
    const meta = document.querySelector('meta[name="theme-color"]')
    if (meta) meta.content = H ? H.themeColor(theme) : theme === 'paper' ? '#fdfcfb' : '#1a1614'
    renderThemeOptions()
  }

  function renderThemeOptions() {
    const root = $('theme-options')
    if (!root) return
    for (const b of root.querySelectorAll('[data-theme-mode]')) {
      const on = b.dataset.themeMode === themeMode
      b.setAttribute('aria-checked', on ? 'true' : 'false')
      b.classList.toggle('on', on)
    }
    const btn = $('theme')
    if (btn) {
      btn.title = '外观：' + themeLabel()
      btn.setAttribute('aria-label', '外观：' + themeLabel())
    }
  }

  function setThemeMode(mode) {
    themeMode = H ? H.normalizeThemeMode(mode) : mode
    try {
      localStorage.setItem(LS.theme, themeMode)
    } catch {
      /* 隐私模式：本次会话内仍生效 */
    }
    applyTheme()
  }

  applyTheme()
  themeMQ.addEventListener?.('change', () => {
    // 只有 system 模式跟随系统；手动选择是显式意图，不能被系统外观覆盖。
    if (themeMode === 'system') applyTheme()
  })

  // ---------- 基础 ----------

  function show(screen) {
    state.screen = screen
    for (const s of SCREENS) $('screen-' + s).hidden = s !== screen
    $('back').hidden = screen === 'pair' || screen === 'list'
    $('menu').hidden = screen !== 'chat'
    // 外观切换在列表/Hub 都可达；它是覆盖层，不占导航栈。
    $('theme').hidden = !(screen === 'list' || HUB_SCREENS.has(screen))
    // 搜索入口只在 Hub 三屏的头部出现；离开 Hub 时顺手收起搜索面板。
    $('hub-search-btn').hidden = !HUB_SCREENS.has(screen)
    if (!HUB_SCREENS.has(screen)) closeHubSearch()
    $('theme-sheet').hidden = true
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
      case 'hub':
        return 'Agent Hub'
      case 'hub-group':
        return (state.hub.group && state.hub.group.name) || '群组'
      case 'hub-dm':
        return (state.hub.dmAgent && state.hub.dmAgent.name) || '私聊'
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
      // 不再静默删 token：一次抖动/服务端重启就把凭据清掉，等于每次都要重新扫码。
      // 留着，由用户显式重新配对覆盖（见 pair 屏提示）。
      disconnect()
      disconnectHub()
      show('pair')
      pairError('设备凭据已失效（可能被撤销），请重新配对')
      throw new Error('设备凭据失效，请重新配对')
    }
    const data = await r.json().catch(() => ({}))
    if (!r.ok) throw new Error(data.error || 'HTTP ' + r.status)
    if (data.ok === false) throw new Error(data.error || '命令失败')
    return data.data || {}
  }

  function el(tag, cls, text) {
    const n = document.createElement(tag)
    if (cls) n.className = cls
    if (text != null) n.textContent = text
    return n
  }

  const SVG_NS = 'http://www.w3.org/2000/svg'
  function svgEl(tag, attrs) {
    const n = document.createElementNS(SVG_NS, tag)
    if (attrs) {
      for (const key of Object.keys(attrs)) n.setAttribute(key, String(attrs[key]))
    }
    return n
  }

  // 轻量 markdown：``` 代码块、`行内代码`、**粗体**。解析在 Hub 纯逻辑层，
  // 这里只把「节点描述」落成 DOM —— 全程 createElement/textContent，不拼 HTML。
  // 只用于**定稿**消息（记录/快照）；流式过程每帧重排太贵，直接 textContent 出纯文本。
  function mdLite(text) {
    const frag = document.createDocumentFragment()
    const plan = H && H.markdownPlan ? H.markdownPlan(text) : [{ kind: 'text', text: String(text == null ? '' : text) }]
    for (const part of plan) {
      if (part.kind === 'codeblock') frag.appendChild(el('pre', '', part.text))
      else if (part.kind === 'code') frag.appendChild(el('code', '', part.text))
      else if (part.kind === 'bold') frag.appendChild(el('b', '', part.text))
      else frag.appendChild(document.createTextNode(part.text))
    }
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
      rememberToken(data.token)
      try {
        localStorage.setItem(LS.name, name)
      } catch {
        /* ignore */
      }
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

  // 每次打开会话 +1；迟到的响应（用户已经切走）按序号丢弃。
  let openSeq = 0

  async function openSession(sid) {
    const seq = ++openSeq
    // 先断流：切换期间旧会话的流式事件绝不能落到新会话的屏幕上
    disconnect()
    // bridge 的恢复字段是 id（历史协议），session_id 只是兼容写法 —— 两个都传
    const data = await api('new_session', { workspace: state.ws, id: sid, session_id: sid, blocks_limit: 30 })
    if (seq !== openSeq) return // 迟到的响应：用户已切到别的会话
    state.sid = data.session_id || sid
    localStorage.setItem(LS.sid, state.sid)
    state.blocks = parseBlocks(data.view_checkpoint)
    state.cursor = data.blocks_cursor || 0
    state.hasMore = !!data.blocks_has_more
    state.live = { text: '', thinking: '' }
    state.chunkSeen.clear()
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
    // 服务端把「当前轮已产出的文本」一并带回（blocks 只到上一个轮次边界），
    // 否则中途打开/重连时这一段是空的，要等下一个 token 才出现。
    if (data.live_text || data.live_thinking) {
      state.live = { text: data.live_text || '', thinking: data.live_thinking || '' }
      state.running = !!data.live_running
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
        // 只有 "." / 空白 的 reasoning 块没有信息量（模型只调工具时常见），
        // 直接渲染会在时间线上留一串孤零零的点。
        if ((b.thinking || '').trim().length > 1) {
          const d = makeThinking(false) // 历史轮次：默认收起
          d.querySelector('.think-body').textContent = b.thinking
          setThinkSummary(d, b.thinking)
          wrap.appendChild(d)
        }
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
        if (b.tool_id) d.dataset.toolId = b.tool_id
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
      else if (e.kind === 'assistant') {
        const n = el('div', 'msg assistant')
        n.appendChild(mdLite(e.text || ''))
        box.appendChild(n)
      }
      else if (e.kind === 'tool') {
        const d = document.createElement('details')
        d.className = 'tool-group' // 与主会话同一个样式类（原来写 'tool'，没有对应 CSS）
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

  // ---------- Agent Hub（P4-03b） ----------
  // 与聊天流完全独立：独立 WebSocket（scope=hub）、独立快照、独立渲染。
  // 页面只消费 hub_model.js 的纯函数结果；这里负责 DOM 与命令编排。

  function isHubScreen() {
    return HUB_SCREENS.has(state.screen)
  }

  function hubAgent(id) {
    return state.hub.agentsById.get(String(id || '')) || null
  }

  function hubAgentName(id) {
    const a = hubAgent(id)
    return (a && a.name) || String(id || '智能体')
  }

  function hubTime(iso) {
    if (!iso) return ''
    const ms = typeof iso === 'number' ? iso : Date.parse(String(iso))
    return ms ? fmtTime(ms) : ''
  }

  // ---- 头像 sigil v2（与桌面同图：id + 职责 → shape/hue/trait；状态来自 running/needs/deleted） ----
  const AVATAR_STATE_ZH = { idle: '待命', working: '工作中', needs_attention: '需要你', offline: '离线' }
  let pendingCache = { needs: null, ids: new Set() }

  function hubPendingAgentIds() {
    const needs = state.hub.needs || []
    if (pendingCache.needs === needs) return pendingCache.ids
    const ids = new Set()
    for (const n of needs) {
      if (n && n.status === 'pending' && n.agent_id) ids.add(String(n.agent_id))
    }
    pendingCache = { needs, ids }
    return ids
  }

  function hubStateOf(agentId, deleted) {
    const id = String(agentId || '')
    if (!A) return 'idle'
    return A.resolveAgentState({
      running: !!id && state.hub.running[id] === true,
      pending: !!id && hubPendingAgentIds().has(id),
      deleted: deleted === true,
    })
  }

  function hubSigilLabel(name, state) {
    return String(name || '智能体') + '，' + (AVATAR_STATE_ZH[state] || AVATAR_STATE_ZH.idle)
  }

  function hubStackSummary(plan) {
    const c = plan.counts
    const parts = []
    if (c.working) parts.push(c.working + ' 个工作中')
    if (c.needs_attention) parts.push(c.needs_attention + ' 个需要你')
    if (c.offline) parts.push(c.offline + ' 个离线')
    if (!parts.length) parts.push(plan.state === 'working' ? '群会话进行中' : '全部待命')
    return parts.join('，')
  }

  function hubStackLabel(name, plan) {
    const prefix = name ? String(name) + '，' : ''
    return prefix + plan.total + ' 个智能体，' + hubStackSummary(plan)
  }

  function hubSigil(opts) {
    const o = opts || {}
    const agent = o.agent || null
    const id = o.id != null ? o.id : (agent && agent.id)
    const responsibility = o.responsibility != null ? o.responsibility : (agent && agent.role)
    const state = o.state || hubStateOf(id, agent && agent.deleted)
    const size = o.size === 24 || o.size === 48 ? o.size : 32
    const showStatus = o.status !== false
    const identity = A.deriveSigil(id, responsibility)
    const node = el('span', 'sigil s' + size)
    node.dataset.agentId = String(id || '')
    node.dataset.shape = identity.shapeId
    node.dataset.hue = identity.hueId
    node.dataset.trait = identity.traitId
    node.dataset.state = showStatus ? state : 'none'
    node.dataset.size = String(size)
    if (o.label) {
      node.setAttribute('role', 'img')
      node.setAttribute('aria-label', o.label)
    } else {
      node.setAttribute('aria-hidden', 'true')
    }
    const root = svgEl('svg', { viewBox: '0 0 48 48', focusable: 'false', 'aria-hidden': 'true' })
    root.appendChild(svgEl('path', { class: 'shape', d: A.SHAPES[identity.shape].d }))
    const traits = svgEl('g', { class: 'traits' })
    for (const mark of A.TRAITS[identity.trait].marks) {
      if (mark.type === 'circle') traits.appendChild(svgEl('circle', { class: 'trait', cx: mark.cx, cy: mark.cy, r: mark.r }))
      else if (mark.type === 'rect') {
        traits.appendChild(svgEl('rect', {
          class: 'trait', x: mark.x, y: mark.y, width: mark.width, height: mark.height, rx: mark.rx,
        }))
      } else {
        traits.appendChild(svgEl('path', { class: 'trait line', d: mark.d, style: 'stroke-width:' + mark.width }))
      }
    }
    root.appendChild(traits)
    if (showStatus) {
      const status = svgEl('g', { class: 'status' })
      if (state === 'idle') status.appendChild(svgEl('circle', { class: 'ring', cx: 24, cy: 24, r: 22, opacity: '.55' }))
      else if (state === 'working') status.appendChild(svgEl('circle', { class: 'ring arc', cx: 24, cy: 24, r: 22, 'stroke-width': 2 }))
      else if (state === 'needs_attention') {
        status.appendChild(svgEl('circle', { class: 'ring thick', cx: 24, cy: 24, r: 22 }))
        status.appendChild(svgEl('circle', { class: 'dot', cx: 38, cy: 10, r: 5 }))
      }
      root.appendChild(status)
    }
    node.appendChild(root)
    return node
  }

  function hubStack(group, opts) {
    const o = opts || {}
    const size = o.size === 24 || o.size === 48 ? o.size : 32
    const members = ((group && group.members) || []).map((m) => {
      const a = hubAgent(m.agent_id)
      return { id: m.agent_id, responsibility: a && a.role, state: hubStateOf(m.agent_id, a && a.deleted) }
    })
    const plan = A.planStack(members, { groupRunning: o.groupRunning === true })
    const wrap = el('span', 'agent-stack s' + size)
    wrap.dataset.state = plan.state
    if (o.label === false) wrap.setAttribute('aria-hidden', 'true')
    else {
      wrap.setAttribute('role', 'img')
      wrap.setAttribute('aria-label', hubStackLabel(o.name || (group && group.name), plan))
    }
    plan.members.forEach((m, i) => {
      const node = hubSigil({
        id: m.id,
        responsibility: m.responsibility,
        state: m.state,
        size,
        status: m.withStatus,
      })
      node.style.zIndex = String(20 - i)
      wrap.appendChild(node)
    })
    if (plan.more > 0) {
      const more = el('span', 'agent-stack-more s' + size, '+' + plan.more)
      more.setAttribute('aria-hidden', 'true')
      wrap.appendChild(more)
    }
    return wrap
  }

  function hubNameMap() {
    const names = {}
    for (const a of state.hub.agents) names[a.id] = a.name || a.id
    return names
  }

  function hubRoster() {
    return H ? H.groupRoster(state.hub.group, state.hub.agents) : []
  }

  function hubError(msg) {
    const box = $('hub-error')
    if (!box) return
    box.hidden = !msg
    box.textContent = msg || ''
  }

  function hydrateHubAgents(agents) {
    state.hub.agents = Array.isArray(agents) ? agents.filter(Boolean) : []
    state.hub.agentsById = new Map(state.hub.agents.map((a) => [String(a.id), a]))
    for (const a of state.hub.agents) {
      if (state.hub.running[a.id] === undefined) state.hub.running[a.id] = false
    }
  }

  function groupRunning(g) {
    if (!g) return false
    if (state.hub.convStatus[g.id] === 'running') return true
    const p = state.hub.progress[g.id]
    if (p && ['queued', 'dispatched', 'running'].includes(String(p.last_task_status || ''))) return true
    return (g.members || []).some((m) => state.hub.running[m.agent_id] === true)
  }

  function agentRunning(id) {
    return state.hub.running[id] === true
  }

  function unreadBadge(n) {
    const v = Math.max(0, Number(n) || 0)
    if (!v) return null
    return el('span', 'hub-unread', v > 99 ? '99+' : String(v))
  }

  function hubStatusDot(on) {
    const d = el('span', 'hub-dot' + (on ? '' : ' off'))
    d.title = on ? '运行中' : '空闲'
    return d
  }

  // ---- Hub 首页 ----

  async function loadHubHome(opts = {}) {
    if (!H) return
    if (!state.token) return
    if (state.hub.homeLoading) {
      state.hub.homePending = true
      return
    }
    state.hub.homeSeq++
    const seq = state.hub.homeSeq
    state.hub.homeLoading = true
    try {
      const [al, gl, nl] = await Promise.all([
        api('hub_agent_list'),
        api('hub_group_list'),
        api('hub_need_list', { status: 'pending' }).catch(() => null),
      ])
      if (seq !== state.hub.homeSeq) return
      hydrateHubAgents(al.agents || [])
      state.hub.dm = al.dm || {}
      state.hub.groups = Array.isArray(gl.groups) ? gl.groups : []
      state.hub.progress = gl.progress || {}
      if (nl && Array.isArray(nl.needs)) state.hub.needs = nl.needs
      hubError('')
      if (state.screen === 'hub') renderHubHome()
    } catch (e) {
      if (seq === state.hub.homeSeq && state.screen === 'hub') hubError(e.message || '加载 Hub 失败')
    } finally {
      state.hub.homeLoading = false
      if (state.hub.homePending) {
        state.hub.homePending = false
        loadHubHome({ silent: true })
      }
    }
  }

  function scheduleHubHomeRefresh(delay = 500) {
    if (state.hub.homeTimer) return
    state.hub.homeTimer = setTimeout(() => {
      state.hub.homeTimer = null
      if (isHubScreen()) loadHubHome({ silent: true })
    }, delay)
  }

  function hubRow(avatar, title, sub, metaRight, onOpen) {
    const row = el('button', 'hub-row')
    row.type = 'button'
    row.append(avatar)
    const main = el('div', 'hub-row-main')
    const titleRow = el('div', 'hub-row-title')
    titleRow.append(...title)
    main.append(titleRow)
    if (sub) main.append(el('div', 'hub-row-sub', sub))
    row.append(main)
    if (metaRight && metaRight.length) {
      const meta = el('div', 'hub-row-meta')
      meta.append(...metaRight)
      row.append(meta)
    }
    row.addEventListener('click', onOpen)
    return row
  }

  function renderHubAlerts() {
    const box = $('hub-alerts')
    box.textContent = ''
    const list = state.hub.alerts.slice(0, 3)
    box.hidden = list.length === 0
    for (const a of list) {
      const cls = a.severity === 'error' ? 'hub-alert error' : a.severity === 'info' ? 'hub-alert info' : 'hub-alert'
      box.appendChild(el('div', cls, a.text))
    }
  }

  function renderHubHome() {
    renderHubAlerts()
    const needsCount = state.hub.needs.length
    const badge = $('hub-needs-count')
    badge.hidden = needsCount === 0
    badge.textContent = needsCount > 99 ? '99+' : String(needsCount)

    const groupUl = $('hub-groups')
    groupUl.textContent = ''
    const groups = state.hub.groups.slice().sort((a, b) => {
      const pa = state.hub.progress[a.id] || {}
      const pb = state.hub.progress[b.id] || {}
      if (!!a.pinned !== !!b.pinned) return a.pinned ? -1 : 1
      const ua = pa.unread ? 1 : 0
      const ub = pb.unread ? 1 : 0
      if (ua !== ub) return ub - ua
      return (Date.parse(pb.last_at || '') || 0) - (Date.parse(pa.last_at || '') || 0)
    })
    $('hub-groups-empty').hidden = groups.length > 0
    for (const g of groups) {
      const p = state.hub.progress[g.id] || {}
      const title = [el('span', 'hub-row-name', g.name || g.id)]
      const unread = unreadBadge(p.unread)
      if (unread) title.push(unread)
      title.push(hubStatusDot(groupRunning(g)))
      const sub = H.hubPreview(p) || ((g.members || []).length + ' 位成员')
      const meta = [el('span', '', hubTime(p.last_at))]
      const stack = hubStack(g, { size: 32, label: false, groupRunning: groupRunning(g) })
      groupUl.appendChild(hubRow(stack, title, sub, meta, () => openHubGroup(g.id)))
    }

    const dmUl = $('hub-dms')
    dmUl.textContent = ''
    const dms = state.hub.agents.slice().sort((a, b) => {
      const pa = state.hub.dm[a.id] || {}
      const pb = state.hub.dm[b.id] || {}
      const ta = Date.parse(pa.last_at || '') || 0
      const tb = Date.parse(pb.last_at || '') || 0
      if (ta !== tb) return tb - ta
      return String(a.name || '').localeCompare(String(b.name || ''), 'zh-Hans-CN')
    })
    $('hub-dms-empty').hidden = dms.length > 0
    for (const a of dms) {
      const p = state.hub.dm[a.id] || {}
      const title = [el('span', 'hub-row-name', a.name || a.id)]
      const unread = unreadBadge(p.unread)
      if (unread) title.push(unread)
      title.push(hubStatusDot(agentRunning(a.id)))
      const sub = H.hubPreview(p) || a.role || '私聊'
      const meta = [el('span', '', hubTime(p.last_at))]
      dmUl.appendChild(hubRow(hubSigil({ agent: a, size: 32 }), title, sub, meta, () => openHubDM(a.id)))
    }
    refreshHubTitle()
  }

  function refreshHubTitle() {
    if (state.screen === 'hub' || state.screen === 'hub-group' || state.screen === 'hub-dm') {
      $('title').textContent = titleFor(state.screen)
    }
  }

  function openHub() {
    if (!H) return
    state.nav.push('list')
    state.hub.activeGroupId = ''
    state.hub.activeAgentId = ''
    show('hub')
    renderHubHome()
    ensureHubSocket()
    loadHubHome()
  }

  function leaveHub() {
    closeMentionPicker()
    closeHubSheet()
    closeHubSearch()
    disconnectHub()
    show('list')
  }

  // ---- 全历史搜索（hub_search）----

  function openHubSearch() {
    if (!H) return
    const s = state.hub.search
    s.open = true
    $('hub-search').hidden = false
    $('hub-search-input').value = s.query
    renderHubSearchScopes()
    renderHubSearchResults()
    // 等覆盖层可见再 focus：iOS 上 display:none 的输入框 focus 会被吞掉。
    setTimeout(() => {
      try {
        $('hub-search-input').focus()
      } catch {
        /* ignore */
      }
    }, 30)
  }

  function closeHubSearch() {
    const s = state.hub.search
    if (!s) return
    if (s.timer) {
      clearTimeout(s.timer)
      s.timer = null
    }
    s.open = false
    s.gen++ // 作废在飞响应：关掉面板后迟到的结果不该再画
    const box = $('hub-search')
    if (box) box.hidden = true
  }

  // 清空查询/结果并关闭面板（「清空」按钮与 Escape 共用）。
  function clearHubSearch() {
    const s = state.hub.search
    if (s.timer) {
      clearTimeout(s.timer)
      s.timer = null
    }
    s.query = ''
    s.hits = []
    s.hasMore = false
    s.next = null
    s.error = ''
    s.gen++
    const input = $('hub-search-input')
    if (input) input.value = ''
    closeHubSearch()
    renderHubSearchResults()
  }

  function renderHubSearchScopes() {
    const s = state.hub.search
    for (const b of $('hub-search-scopes').querySelectorAll('[data-search-scope]')) {
      const on = b.dataset.searchScope === s.scope
      b.classList.toggle('on', on)
      b.setAttribute('aria-selected', on ? 'true' : 'false')
    }
  }

  // 输入 debounce 250ms（与桌面同一口径）：手机上每敲一个字就 fan-out 一次太贵。
  function scheduleHubSearch() {
    const s = state.hub.search
    if (s.timer) clearTimeout(s.timer)
    s.timer = setTimeout(() => {
      s.timer = null
      runHubSearch(false)
    }, 250)
  }

  async function runHubSearch(more) {
    const s = state.hub.search
    const q = String(s.query || '').trim()
    const gen = ++s.gen
    if (!q) {
      s.hits = []
      s.hasMore = false
      s.next = null
      s.error = ''
      s.loading = false
      renderHubSearchResults()
      return
    }
    s.loading = true
    if (!more) s.error = ''
    renderHubSearchResults()
    try {
      const payload = H.searchPayload(q, s.scope, more ? s.next : null)
      const data = await api('hub_search', payload)
      if (gen !== s.gen) return
      const page = H.searchPageView(data)
      s.hits = more ? H.mergeSearchResults(s.hits, page.hits) : page.hits
      s.hasMore = page.hasMore
      s.next = page.next
      s.error = ''
    } catch (e) {
      if (gen !== s.gen) return
      s.error = e.message || '搜索失败'
      if (!more) {
        s.hits = []
        s.hasMore = false
        s.next = null
      }
    } finally {
      if (gen === s.gen) {
        s.loading = false
        renderHubSearchResults()
      }
    }
  }

  function renderHubSearchResults() {
    const s = state.hub.search
    const box = $('hub-search-results')
    if (!box) return
    box.textContent = ''
    const err = $('hub-search-error')
    err.hidden = !s.error
    err.textContent = s.error || ''
    $('hub-search-more').hidden = !s.hasMore
    $('hub-search-more').textContent = s.loading ? '加载中…' : '更早的结果'
    const q = String(s.query || '').trim()
    if (!q) {
      box.appendChild(el('p', 'hint', '搜索所有群聊与私聊的历史消息。'))
      return
    }
    if (s.loading && s.hits.length === 0) {
      box.appendChild(el('p', 'hint', '搜索中…'))
      return
    }
    if (!s.hits.length && !s.error) {
      box.appendChild(el('p', 'hint', '没有匹配的消息。'))
      return
    }
    for (const hit of s.hits) {
      const row = el('button', 'hub-search-hit' + (hit.ownerOnly ? ' owner-only' : ''))
      row.type = 'button'
      row.dataset.searchKey = hit.key
      const top = el('div', 'hub-search-hit-top')
      top.appendChild(el('span', 'hub-chip hub-search-scope', H.searchScopeLabel(hit.scope)))
      top.appendChild(el('span', 'hub-search-hit-name', hit.name))
      if (hit.ownerOnly) top.appendChild(el('span', 'hub-chip owner-only', '仅群主可见'))
      top.appendChild(el('span', 'hub-search-hit-time', hubTime(hit.createdAt)))
      row.appendChild(top)
      row.appendChild(el('div', 'hub-search-hit-snippet', hit.snippet || ''))
      const meta = []
      if (hit.sender) meta.push(hit.sender)
      if (hit.seq) meta.push('#' + hit.seq)
      if (meta.length) row.appendChild(el('div', 'hub-search-hit-meta', meta.join(' · ')))
      row.addEventListener('click', () => {
        void openHubSearchHit(hit)
      })
      box.appendChild(row)
    }
  }

  // 点结果：先打开会话（复用 openHubGroup/openHubDM 的加载与已读语义），
  // 命中不在已加载的一页里再拉「以命中 seq 结尾」的一页并合并，最后滚动+高亮。
  async function openHubSearchHit(hit) {
    if (!H || !hit || !hit.targetId) return
    closeHubSearch()
    if (hit.scope === 'group') await openHubGroup(hit.targetId)
    else await openHubDM(hit.targetId)
    const still =
      hit.scope === 'group'
        ? state.hub.activeGroupId === hit.targetId
        : state.hub.activeAgentId === hit.targetId
    if (!still) return
    const messages = hit.scope === 'group' ? state.hub.groupMessages : state.hub.dmMessages
    const loaded = messages.some((m) => Number(m && m.seq) === hit.seq)
    if (!loaded) {
      const req = H.searchAroundRequest(hit.scope, hit.targetId, hit.seq)
      if (!req) return
      try {
        const page = await api(req.cmd, req.payload)
        if (hit.scope === 'group') {
          if (state.hub.activeGroupId !== hit.targetId) return
          state.hub.groupMessages = H.mergeMessages(state.hub.groupMessages, page.messages || [])
          state.hub.groupHasMore = firstPersistedSeq(state.hub.groupMessages) > 1
          renderHubGroupMessages()
        } else {
          if (state.hub.activeAgentId !== hit.targetId) return
          state.hub.dmMessages = H.mergeMessages(state.hub.dmMessages, page.messages || [])
          state.hub.dmHasMore = firstPersistedSeq(state.hub.dmMessages) > 1
          renderHubDM()
        }
      } catch (e) {
        hubError('加载命中附近的历史失败：' + (e.message || e))
        return
      }
    }
    jumpToHubSeq(hit.seq)
  }

  // 跳到 seq 并高亮：reduced-motion 时用 auto（滚动是功能，不该只剩动画）。
  function jumpToHubSeq(seq) {
    const n = Number(seq) || 0
    if (!(n > 0)) return
    const box = state.screen === 'hub-group' ? $('hub-group-messages') : state.screen === 'hub-dm' ? $('hub-dm-messages') : null
    if (!box) return
    const node = box.querySelector('[data-seq="' + n + '"]')
    if (!node) return
    const reduced =
      typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches
    node.scrollIntoView({ block: 'center', behavior: reduced ? 'auto' : 'smooth' })
    node.classList.add('hub-msg-highlight')
    setTimeout(() => node.classList.remove('hub-msg-highlight'), 2000)
  }

  // ---- 群时间线 ----

  async function openHubGroup(gid) {
    if (!H || !gid) return
    state.nav.push('hub')
    state.hub.activeGroupId = String(gid)
    state.hub.activeAgentId = ''
    state.hub.group = null
    state.hub.conversation = null
    state.hub.groupMessages = []
    state.hub.drafts = []
    state.hub.deltaIndex = {}
    state.hub.groupHasMore = false
    state.hub.groupLoading = true
    show('hub-group')
    renderHubGroupChrome()
    renderHubGroupMessages({ stick: true })
    ensureHubSocket()
    await loadHubGroupSnapshot(gid, { reset: true })
  }

  async function loadHubGroupSnapshot(gid, opts = {}) {
    if (!H || !gid) return
    const seq = ++state.hub.groupSeq
    state.hub.groupLoading = true
    if (opts.reset) {
      state.hub.groupHasMore = false
      renderHubGroupMessages({ stick: true })
    }
    try {
      const [meta, page] = await Promise.all([
        api('hub_group_get', { group_id: gid }),
        api('hub_group_history', { group_id: gid, limit: 50 }),
      ])
      if (seq !== state.hub.groupSeq || state.hub.activeGroupId !== String(gid)) return
      state.hub.group = meta.group || null
      state.hub.conversation = meta.conversation || null
      state.hub.convStatus[gid] = state.hub.conversation ? state.hub.conversation.status : ''
      const incoming = page.messages || []
      state.hub.groupMessages = opts.reset
        ? H.mergeMessages([], incoming)
        : H.mergeMessages(state.hub.groupMessages, incoming)
      // 以合并后的第一条 seq 判断是否还有更早（与 bridge groupHistoryHasMore 同口径）；
      // 只信 page.has_more 会在「已翻到 seq=1」后仍留着按钮。
      state.hub.groupHasMore = firstPersistedSeq(state.hub.groupMessages) > 1
      // resync 时草稿一律作废：final 才是权威，丢失的半截正文由后续 delta 重新长出来。
      state.hub.drafts = []
      state.hub.deltaIndex = {}
      state.hub.groupLoading = false
      hubError('')
      renderHubGroupChrome()
      renderHubGroupMessages({ stick: !!opts.reset })
      refreshHubTitle()
      void markHubGroupRead()
    } catch (e) {
      if (seq === state.hub.groupSeq) {
        state.hub.groupLoading = false
        $('hub-group-messages').appendChild(el('div', 'hub-empty', '加载群失败：' + (e.message || e)))
        renderHubGroupStatus()
      }
    } finally {
      if (seq === state.hub.groupSeq) state.hub.groupLoading = false
    }
  }

  function renderHubGroupChrome() {
    const head = $('hub-group-head')
    head.textContent = ''
    const g = state.hub.group
    const title = g ? g.name || g.id : '群组'
    const bits = []
    if (g) bits.push((g.members || []).length + ' 位成员')
    if (g && g.policy && g.policy.mode) bits.push('策略 ' + g.policy.mode)
    const cv = state.hub.conversation
    if (cv) bits.push(conversationLabel(cv))
    const runningNow = groupRunning(g) || (cv && cv.status === 'running') || state.hub.drafts.length > 0
    if (g) head.appendChild(hubStack(g, { size: 32, name: g.name || g.id, groupRunning: runningNow }))
    const main = el('div', 'hub-head-main')
    main.appendChild(el('div', 'hub-head-title', title))
    main.appendChild(el('div', 'hub-head-sub', bits.join(' · ') || '加载中…'))
    head.appendChild(main)

    const tools = $('hub-group-tools')
    tools.textContent = ''
    const tasks = el('button', 'hub-chip', '任务板')
    tasks.type = 'button'
    tasks.addEventListener('click', () => openHubTasks())
    const needs = el('button', 'hub-chip', 'Needs you')
    needs.type = 'button'
    needs.addEventListener('click', () => openHubNeeds(state.hub.activeGroupId))
    const assign = el('button', 'hub-chip', '派任务')
    assign.type = 'button'
    assign.addEventListener('click', openHubAssign)
    tools.append(tasks, needs, assign)
    if (runningNow) {
      const stop = el('button', 'hub-chip danger', '停止自动对话')
      stop.type = 'button'
      stop.addEventListener('click', stopHubConversation)
      tools.appendChild(stop)
    }
  }

  function conversationLabel(cv) {
    if (!cv) return ''
    switch (cv.status) {
      case 'running':
        return '自动对话进行中（' + (cv.turns_used || 0) + '/' + (cv.turn_budget || 8) + ' 轮）'
      case 'waiting_human':
        return '等你继续'
      case 'stopped':
        return '已停止，发消息会重新开始'
      case 'exhausted':
        return '已到护栏上限' + (cv.stop_reason ? '：' + cv.stop_reason : '')
      default:
        return '空闲'
    }
  }

  function firstPersistedSeq(messages) {
    for (const m of messages || []) {
      const s = Number(m && m.seq) || 0
      if (s > 0) return s
    }
    return 0
  }

  function lastPersistedSeq(messages) {
    let last = 0
    for (const m of messages || []) last = Math.max(last, Number(m && m.seq) || 0)
    return last
  }

  function renderHubGroupMessages(opts = {}) {
    const box = $('hub-group-messages')
    const stick = opts.stick || nearBottom(box)
    box.textContent = ''
    state.hub.draftNodes = new Map()

    if (state.hub.groupHasMore) {
      const more = el('button', 'hub-load-more', state.hub.groupLoading ? '加载中…' : '加载更早')
      more.type = 'button'
      more.disabled = state.hub.groupLoading
      more.addEventListener('click', () => {
        void loadOlderGroup()
      })
      box.appendChild(more)
    }

    const names = hubNameMap()
    const roster = hubRoster()
    for (const m of state.hub.groupMessages) {
      const node = renderHubMessage(m, { group: true, names, roster })
      if (node) box.appendChild(node)
    }
    for (const d of state.hub.drafts) {
      const node = renderHubDraft(d)
      box.appendChild(node)
    }
    if (!state.hub.groupMessages.length && !state.hub.drafts.length) {
      box.appendChild(el('div', 'hub-empty', state.hub.groupLoading ? '加载中…' : '还没有消息。发一条 @成员 开始，或从任务板派发任务。'))
    }
    renderHubGroupStatus()
    if (stick) box.scrollTop = box.scrollHeight
  }

  // 定稿正文：markdown（粗体/行内代码/围栏）+ @ 高亮。
  // bodyParts 只给节点描述，这里统一 createElement/textContent；模型里的 HTML 只是文本。
  function appendHubBody(body, text, roster) {
    const parts = H && H.bodyParts ? H.bodyParts(text, roster || []) : [{ kind: 'text', text: String(text || '') }]
    for (const part of parts) {
      if (part.kind === 'mention') body.appendChild(el('span', 'hub-mention', part.text))
      else if (part.kind === 'codeblock') body.appendChild(el('pre', '', part.text))
      else if (part.kind === 'code') body.appendChild(el('code', '', part.text))
      else if (part.kind === 'bold') body.appendChild(el('b', '', part.text))
      else body.appendChild(document.createTextNode(part.text))
    }
  }

  // 每条已落盘消息都带 data-seq：搜索结果点进来要能 scrollIntoView 到命中。
  // owner-only 必须有可见徽标（人类能看到正文，但要知道它只有群主能看到）。
  function decorateHubMessage(node, m) {
    if (!node || !m) return node
    const seq = Number(m.seq) || 0
    if (seq > 0) node.dataset.seq = String(seq)
    if (m.visibility === 'owner' && !node.querySelector('.hub-chip.owner-only')) {
      node.appendChild(el('span', 'hub-chip owner-only', '仅群主可见'))
    }
    return node
  }

  function renderHubMessage(m, ctx) {
    return decorateHubMessage(renderHubMessageInner(m, ctx), m)
  }

  function renderHubMessageInner(m, ctx) {
    if (!m) return null
    const names = ctx.names || {}
    if (m.kind === 'task_result') return renderHubTaskCard(m, names)
    if (m.kind === 'routing_receipt') {
      return el('div', 'hub-msg system', H.routingSummary(m.text || '', names))
    }
    if (m.kind === 'handoff') {
      const status = String((m.refs && m.refs.status) || '')
      const label = status === 'accepted' ? '已接受' : status === 'declined' ? '已拒绝' : '等待确认'
      return el('div', 'hub-msg system', '交接 · ' + (m.text || '') + '（' + label + '）')
    }
    if (m.kind === 'approval_stub') {
      return el('div', 'hub-msg system', '审批占位：' + (m.text || '详情在 Needs you'))
    }
    const sender = m.sender || {}
    if (sender.kind === 'system' || m.kind === 'system' || m.kind === 'round_summary') {
      return el('div', 'hub-msg system', '系统 · ' + (m.text || ''))
    }
    const human = sender.kind === 'human'
    const wrap = el('div', 'hub-msg ' + (human ? 'human' : 'agent'))
    if (!human) {
      const who = el('div', 'who')
      who.appendChild(el('span', 'name', sender.agent_id ? hubAgentName(sender.agent_id) : '智能体'))
      if (m.created_at) who.appendChild(el('span', 'time', hubTime(m.created_at)))
      wrap.appendChild(who)
    }
    const body = el('div', 'body')
    appendHubBody(body, m.text || '', ctx.group ? ctx.roster : [])
    if (m.pending) body.appendChild(el('div', 'hint', '发送中…'))
    wrap.appendChild(body)
    return wrap
  }

  function renderHubTaskCard(m, names) {
    const view = H.taskCardView(m)
    if (!view) return null
    const card = el('div', 'hub-card ' + (view.tone || 'muted'))
    card.dataset.taskId = view.taskId
    const head = el('div', 'hub-card-head')
    head.appendChild(el('span', 'hub-status-chip hub-tone-' + (view.tone || 'muted'), view.statusLabel))
    head.appendChild(el('span', 'hub-card-title', view.title))
    card.appendChild(head)

    const meta = el('div', 'hub-card-meta')
    if (view.agentId) meta.appendChild(el('span', '', '执行者 ' + (names[view.agentId] || hubAgentName(view.agentId))))
    if (view.initiatorName) meta.appendChild(el('span', '', '派发 ' + view.initiatorName))
    else if (view.initiatorAgentId) meta.appendChild(el('span', '', '派发 ' + (names[view.initiatorAgentId] || view.initiatorAgentId)))
    if (view.durationLabel) meta.appendChild(el('span', '', '耗时 ' + view.durationLabel))
    if (view.usageLabel) meta.appendChild(el('span', '', view.usageLabel))
    if (meta.childNodes.length) card.appendChild(meta)

    if (view.summary) card.appendChild(el('div', 'hub-card-text', view.summary))
    if (view.artifacts.length) {
      const ul = el('ul', 'hub-card-list')
      for (const a of view.artifacts) ul.appendChild(el('li', '', a.label))
      card.appendChild(ul)
    }
    if (view.error) card.appendChild(el('div', 'hub-card-error', '错误：' + view.error))
    if (view.taskId) card.appendChild(el('div', 'hub-card-meta', '任务 ' + view.taskId))
    return card
  }

  function renderHubDraft(d) {
    const wrap = el('div', 'hub-msg agent draft')
    wrap.dataset.draftId = d.id
    const who = el('div', 'who')
    who.appendChild(el('span', 'name', hubAgentName(d.sender && d.sender.agent_id)))
    who.appendChild(el('span', 'time', '正在输入'))
    wrap.appendChild(who)
    const body = el('div', 'body')
    body.textContent = d.text || ''
    wrap.appendChild(body)
    state.hub.draftNodes.set(d.id, body)
    return wrap
  }

  function renderHubGroupStatus() {
    const box = $('hub-group-status')
    const cv = state.hub.conversation
    const drafts = state.hub.drafts || []
    let text = ''
    if (drafts.length) {
      const ids = Array.from(new Set(drafts.map((d) => String((d.sender && d.sender.agent_id) || '')))).filter(Boolean)
      text = (ids.map(hubAgentName).join('、') || '智能体') + ' 正在输入'
      box.classList.add('hub-typing')
    } else if (cv && cv.status === 'running') {
      text = '自动对话进行中 ' + (cv.turns_used || 0) + '/' + (cv.turn_budget || 8)
      box.classList.remove('hub-typing')
    } else if (cv && cv.status === 'waiting_human') {
      text = '等你继续'
      box.classList.remove('hub-typing')
    } else if (groupRunning(state.hub.group)) {
      text = '成员运行中'
      box.classList.remove('hub-typing')
    } else {
      box.classList.remove('hub-typing')
    }
    box.hidden = !text
    box.textContent = ''
    if (text) {
      const span = el('span', '', text)
      if (drafts.length) span.className = 'dots'
      box.appendChild(span)
    }
  }

  async function loadOlderGroup() {
    const gid = state.hub.activeGroupId
    if (!gid || state.hub.groupLoading || !state.hub.groupHasMore) return
    const before = firstPersistedSeq(state.hub.groupMessages)
    if (!before) return
    state.hub.groupLoading = true
    renderHubGroupMessages({ stick: false })
    const box = $('hub-group-messages')
    const prevHeight = box.scrollHeight
    const prevTop = box.scrollTop
    try {
      const page = await api('hub_group_history', { group_id: gid, before_seq: before, limit: 50 })
      if (state.hub.activeGroupId !== String(gid) || state.screen !== 'hub-group') return
      const pageMessages = page.messages || []
      state.hub.groupMessages = H.mergeMessages(state.hub.groupMessages, pageMessages)
      // 空页 = 已经没有更早的（不要因为首条 seq 仍是 11 就无限转圈）。
      state.hub.groupHasMore = pageMessages.length > 0 && firstPersistedSeq(state.hub.groupMessages) > 1
      state.hub.groupLoading = false
      renderHubGroupMessages({ stick: false })
      box.scrollTop = box.scrollHeight - prevHeight + prevTop
    } catch (e) {
      state.hub.groupLoading = false
      renderHubGroupMessages({ stick: false })
      box.appendChild(el('div', 'hub-empty', '加载更早失败：' + (e.message || e)))
    } finally {
      state.hub.groupLoading = false
    }
  }

  // 已读游标：只有时间线贴底且页面可见才推进（读历史不能清未读）。
  function atReadBottom(boxId) {
    if (document.hidden || !H) return false
    const box = $(boxId)
    if (!box) return false
    return H.shouldMarkRead(
      { scrollTop: box.scrollTop, scrollHeight: box.scrollHeight, clientHeight: box.clientHeight },
      true,
    )
  }

  let readRaf = 0
  function scheduleHubReadCheck() {
    if (readRaf) return
    readRaf = requestAnimationFrame(() => {
      readRaf = 0
      if (state.screen === 'hub-group') void markHubGroupRead()
      else if (state.screen === 'hub-dm') void markHubDMRead(lastPersistedSeq(state.hub.dmMessages))
    })
  }

  async function markHubGroupRead() {
    const gid = state.hub.activeGroupId
    const seq = lastPersistedSeq(state.hub.groupMessages)
    if (!gid || !seq) return
    if (!atReadBottom('hub-group-messages')) return
    if ((state.hub.readSeq[gid] || 0) >= seq) return
    state.hub.readSeq[gid] = seq
    try {
      await api('hub_mark_read', { group_id: gid, seq })
      if (state.hub.progress[gid]) state.hub.progress[gid] = Object.assign({}, state.hub.progress[gid], { unread: 0 })
    } catch {
      // 已读游标是 best-effort：失败不打断阅读，下次打开再推。
      state.hub.readSeq[gid] = 0
    }
  }

  function nextClientMsgID(prefix) {
    return prefix + '-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 8)
  }

  function appendHubSendError(boxId, label, retry) {
    const box = $(boxId)
    const wrap = el('div', 'hub-msg system hub-send-error')
    wrap.appendChild(el('span', '', label))
    if (retry) {
      const again = el('button', 'ghost hub-retry', '重试')
      again.type = 'button'
      again.addEventListener('click', () => {
        wrap.remove()
        retry()
      })
      wrap.appendChild(again)
    }
    box.appendChild(wrap)
    box.scrollTop = box.scrollHeight
  }

  function restoreComposer(input) {
    input.style.height = 'auto'
    input.style.height = Math.min(input.scrollHeight, 120) + 'px'
  }

  async function sendHubGroup() {
    const input = $('hub-group-input')
    const gid = state.hub.activeGroupId
    const text = input.value.trim()
    if (!gid || !text || state.hub.busy) return
    state.hub.busy = true
    // 请求成功前不清空：失败时正文（含选中的 @ 提及）原样留在输入框里。
    closeMentionPicker()
    const roster = hubRoster()
    // 同一个失败草稿「重试」沿用首次 client_msg_id：首次请求可能已经落盘，只是
    // 响应丢了；换新 id 会写成第二条。成功或用户改了草稿才换（见 sendSlotID）。
    const slot = H.sendSlotID(state.hub.pendingGroupSend, text, gid, () => nextClientMsgID('g'))
    const cid = slot.cid
    state.hub.pendingGroupSend = slot
    const optimistic = H.optimisticMessage({ text, clientMsgId: cid, senderKind: 'human', groupId: gid })
    // 首次请求其实成功、事件已经先到：不再插 seq=0 的乐观气泡（会把回执盖回「发送中」）。
    if (!H.hasLandedClientMsg(state.hub.groupMessages, cid)) {
      state.hub.groupMessages = H.mergeMessages(state.hub.groupMessages, optimistic)
      renderHubGroupMessages({ stick: true })
    }
    try {
      const data = await api('hub_group_send', H.groupSendPayload(gid, text, roster, cid))
      // 成功才清空；发送期间用户新敲的内容保留。幂等槽同时失效：下一次是新消息。
      state.hub.pendingGroupSend = null
      input.value = H.draftAfterSend(input.value, text)
      restoreComposer(input)
      if (state.hub.activeGroupId !== String(gid)) return
      const final = {
        id: data.message_id || optimistic.id,
        seq: Number(data.seq) || 0,
        client_msg_id: cid,
        group_id: gid,
        kind: 'text',
        sender: { kind: 'human' },
        text,
        created_at: new Date().toISOString(),
      }
      // 事件通常比 HTTP 响应先到（bridge 先 emit 再 resp）：已经落盘的消息不再
      // 用合成对象覆盖，避免把服务端 created_at/id 改回本地值。
      const landed = H.hasLandedClientMsg(state.hub.groupMessages, cid)
      if (!landed) state.hub.groupMessages = H.mergeMessages(state.hub.groupMessages, final)
      renderHubGroupMessages({ stick: true })
      if (data.seq) void markHubGroupRead()
      scheduleHubHomeRefresh()
    } catch (e) {
      // 失败：草稿与幂等槽都留着 —— 「重试」必须还是同一个 client_msg_id。
      state.hub.groupMessages = state.hub.groupMessages.filter((m) => !(m && m.pending && H.clientMsgID(m) === cid))
      input.value = H.draftAfterFailure(input.value, text)
      restoreComposer(input)
      input.focus()
      renderHubGroupMessages({ stick: true })
      appendHubSendError('hub-group-messages', '发送失败：' + (e.message || e), sendHubGroup)
    } finally {
      state.hub.busy = false
    }
  }

  function appendHubGroupError(text) {
    const box = $('hub-group-messages')
    box.appendChild(el('div', 'hub-msg system', text))
    box.scrollTop = box.scrollHeight
  }

  async function stopHubConversation() {
    const gid = state.hub.activeGroupId
    if (!gid || state.hub.busy) return
    state.hub.busy = true
    try {
      const data = await api('hub_conversation_stop', { group_id: gid })
      if (state.hub.activeGroupId !== String(gid)) return
      state.hub.conversation = data.conversation || state.hub.conversation
      if (state.hub.conversation) state.hub.convStatus[gid] = state.hub.conversation.status
      // 停止后服务端会抑制在跑 run 的 final：草稿不会再有终稿，必须立即清掉，
      // 否则「正在输入」会永远停在屏幕上。
      state.hub.drafts = []
      state.hub.deltaIndex = {}
      renderHubGroupMessages({ stick: nearBottom($('hub-group-messages')) })
      renderHubGroupChrome()
      renderHubGroupStatus()
    } catch (e) {
      appendHubGroupError('停止失败：' + (e.message || e))
    } finally {
      state.hub.busy = false
    }
  }

  // ---- DM 时间线 ----

  async function openHubDM(agentId) {
    if (!H || !agentId) return
    state.nav.push('hub')
    state.hub.activeAgentId = String(agentId)
    state.hub.activeGroupId = ''
    state.hub.dmAgent = hubAgent(agentId)
    state.hub.dmMessages = []
    state.hub.dmHasMore = false
    state.hub.dmAwaiting = false
    state.hub.dmLoading = true
    show('hub-dm')
    renderHubDM()
    ensureHubSocket()
    await loadHubDMSnapshot(agentId, { reset: true })
  }

  async function loadHubDMSnapshot(agentId, opts = {}) {
    if (!H || !agentId) return
    const seq = ++state.hub.dmSeq
    state.hub.dmLoading = true
    try {
      const data = await api('hub_open', {
        target: { kind: 'dm', agent_id: String(agentId) },
        limit: 50,
        // 已读由「贴底 + 可见」决定：打开只读历史时不推进服务端游标。
        mark_read: false,
      })
      if (seq !== state.hub.dmSeq || state.hub.activeAgentId !== String(agentId)) return
      state.hub.dmAgent = data.agent || state.hub.dmAgent || hubAgent(agentId)
      state.hub.dmMessages = opts.reset
        ? H.mergeMessages([], data.messages || [])
        : H.mergeMessages(state.hub.dmMessages, data.messages || [])
      state.hub.dmHasMore = firstPersistedSeq(state.hub.dmMessages) > 1
      state.hub.dmAwaiting = false
      state.hub.dmLoading = false
      hubError('')
      renderHubDM({ stick: !!opts.reset })
      refreshHubTitle()
      // 只有渲染后确实在底部（且页面可见）才推进已读；否则保留未读角标。
      if (opts.reset) void markHubDMRead(lastPersistedSeq(state.hub.dmMessages))
      else scheduleHubReadCheck()
    } catch (e) {
      if (seq === state.hub.dmSeq) {
        $('hub-dm-messages').appendChild(el('div', 'hub-empty', '打开私聊失败：' + (e.message || e)))
        renderHubDMStatus()
      }
    } finally {
      if (seq === state.hub.dmSeq) state.hub.dmLoading = false
    }
  }

  function renderHubDM(opts = {}) {
    const head = $('hub-dm-head')
    head.textContent = ''
    const a = state.hub.dmAgent
    const title = (a && a.name) || state.hub.activeAgentId || '私聊'
    const sub = []
    if (a && a.role) sub.push(a.role)
    if (a && a.provider && a.model) sub.push(a.provider + '/' + a.model)
    const dmState = hubStateOf(state.hub.activeAgentId, a && a.deleted)
    if (dmState === 'working') sub.push('运行中')
    if (a) head.appendChild(hubSigil({ agent: a, size: 32, state: dmState, label: hubSigilLabel(a.name || a.id, dmState) }))
    const main = el('div', 'hub-head-main')
    main.appendChild(el('div', 'hub-head-title', title))
    main.appendChild(el('div', 'hub-head-sub', sub.join(' · ') || '私聊'))
    head.appendChild(main)

    const box = $('hub-dm-messages')
    const stick = opts.stick || nearBottom(box)
    box.textContent = ''
    if (state.hub.dmHasMore) {
      const more = el('button', 'hub-load-more', state.hub.dmLoading ? '加载中…' : '加载更早')
      more.type = 'button'
      more.disabled = state.hub.dmLoading
      more.addEventListener('click', () => {
        void loadOlderDM()
      })
      box.appendChild(more)
    }
    const names = hubNameMap()
    for (const m of state.hub.dmMessages) {
      const node = renderHubMessage(m, { group: false, names, roster: [] })
      if (node) box.appendChild(node)
    }
    if (!state.hub.dmMessages.length) {
      box.appendChild(el('div', 'hub-empty', state.hub.dmLoading ? '加载中…' : '还没有聊过。发一条消息开始。'))
    }
    renderHubDMStatus()
    if (stick) box.scrollTop = box.scrollHeight
  }

  function renderHubDMStatus() {
    const box = $('hub-dm-status')
    const text = state.hub.dmAwaiting ? '已发送，等待回复' : ''
    box.hidden = !text
    box.textContent = text
    box.className = text ? 'hub-status hub-typing' : 'hub-status'
  }

  async function loadOlderDM() {
    const agentId = state.hub.activeAgentId
    if (!agentId || state.hub.dmLoading || !state.hub.dmHasMore) return
    const before = firstPersistedSeq(state.hub.dmMessages)
    if (!before) return
    state.hub.dmLoading = true
    renderHubDM()
    const box = $('hub-dm-messages')
    const prevHeight = box.scrollHeight
    const prevTop = box.scrollTop
    try {
      const page = await api('hub_history', { agent_id: agentId, before_seq: before, limit: 50 })
      if (state.hub.activeAgentId !== String(agentId) || state.screen !== 'hub-dm') return
      const pageMessages = page.messages || []
      state.hub.dmMessages = H.mergeMessages(state.hub.dmMessages, pageMessages)
      state.hub.dmHasMore = pageMessages.length > 0 && firstPersistedSeq(state.hub.dmMessages) > 1
      state.hub.dmLoading = false
      renderHubDM()
      box.scrollTop = box.scrollHeight - prevHeight + prevTop
    } catch (e) {
      state.hub.dmLoading = false
      renderHubDM()
      box.appendChild(el('div', 'hub-empty', '加载更早失败：' + (e.message || e)))
    } finally {
      state.hub.dmLoading = false
    }
  }

  async function markHubDMRead(seq) {
    const agentId = state.hub.activeAgentId
    const v = Number(seq) || 0
    if (!agentId || !v) return
    if (!atReadBottom('hub-dm-messages')) return
    if ((state.hub.readSeq['dm:' + agentId] || 0) >= v) return
    state.hub.readSeq['dm:' + agentId] = v
    try {
      await api('hub_mark_read', { agent_id: agentId, seq: v })
      if (state.hub.dm[agentId]) state.hub.dm[agentId] = Object.assign({}, state.hub.dm[agentId], { unread: 0 })
    } catch {
      state.hub.readSeq['dm:' + agentId] = 0
    }
  }

  async function sendHubDM() {
    const input = $('hub-dm-input')
    const agentId = state.hub.activeAgentId
    const text = input.value.trim()
    if (!agentId || !text || state.hub.busy) return
    state.hub.busy = true
    // 与群聊同口径：成功前不清空，失败保留草稿并给重试入口；重试复用同一个
    // client_msg_id（首次请求可能已落盘，只是响应丢了）。
    const slot = H.sendSlotID(state.hub.pendingDMSend, text, agentId, () => nextClientMsgID('d'))
    const cid = slot.cid
    state.hub.pendingDMSend = slot
    const optimistic = H.optimisticMessage({ text, clientMsgId: cid, senderKind: 'human', agentId })
    if (!H.hasLandedClientMsg(state.hub.dmMessages, cid)) {
      state.hub.dmMessages = H.mergeMessages(state.hub.dmMessages, optimistic)
    }
    state.hub.dmAwaiting = true
    renderHubDM({ stick: true })
    try {
      const data = await api('hub_send', H.dmSendPayload(agentId, text, cid))
      state.hub.pendingDMSend = null
      input.value = H.draftAfterSend(input.value, text)
      restoreComposer(input)
      if (state.hub.activeAgentId !== String(agentId)) return
      const final = {
        id: optimistic.id,
        seq: Number(data.seq) || 0,
        client_msg_id: cid,
        agent_id: agentId,
        kind: 'text',
        sender: { kind: 'human' },
        text,
        created_at: new Date().toISOString(),
      }
      const landed = H.hasLandedClientMsg(state.hub.dmMessages, cid)
      if (!landed) state.hub.dmMessages = H.mergeMessages(state.hub.dmMessages, final)
      renderHubDM({ stick: true })
      if (data.seq) void markHubDMRead(data.seq)
      scheduleHubHomeRefresh()
    } catch (e) {
      state.hub.dmMessages = state.hub.dmMessages.filter((m) => !(m && m.pending && H.clientMsgID(m) === cid))
      state.hub.dmAwaiting = false
      input.value = H.draftAfterFailure(input.value, text)
      restoreComposer(input)
      input.focus()
      renderHubDM({ stick: true })
      appendHubSendError('hub-dm-messages', '发送失败：' + (e.message || e), sendHubDM)
    } finally {
      state.hub.busy = false
    }
  }

  // ---- Hub 事件流（独立 WebSocket） ----

  function hubFrameSeen(raw) {
    const now = Date.now()
    const prev = state.hub.seenFrames.get(raw)
    if (prev !== undefined && now - prev < 3000) return true
    state.hub.seenFrames.set(raw, now)
    if (state.hub.seenFrames.size > 600) {
      for (const [k, t] of state.hub.seenFrames) if (now - t > 3000) state.hub.seenFrames.delete(k)
    }
    return false
  }

  function ensureHubSocket() {
    if (!H || !state.token || !isHubScreen()) return
    if (state.hub.conn && state.hub.conn.readyState <= WebSocket.OPEN) return
    // 旧连接处于 CLOSING（onclose 还没回调）：先换代，避免它的回调把新连接置空。
    if (state.hub.conn) {
      state.hub.gen++
      try {
        state.hub.conn.close()
      } catch {
        /* ignore */
      }
      state.hub.conn = null
    }
    state.hub.disposed = false
    connectHub()
  }

  function disconnectHub() {
    state.hub.disposed = true
    state.hub.gen++
    state.hub.hasConnected = false
    if (state.hub.retryTimer) {
      clearTimeout(state.hub.retryTimer)
      state.hub.retryTimer = null
    }
    if (state.hub.conn) {
      try {
        state.hub.conn.close()
      } catch {
        /* ignore */
      }
      state.hub.conn = null
    }
  }

  function connectHub() {
    if (!H || !state.token) return
    const gen = state.hub.gen
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    // 浏览器 WebSocket 不能设 Authorization 头；bridge 的 auth() 同时认
    // query 里的 token（见 mobile_server.go），所以沿用聊天连接的 query 方式。
    const url = `${proto}://${location.host}/api/events?scope=hub&token=${encodeURIComponent(state.token)}`
    const conn = new WebSocket(url)
    state.hub.conn = conn
    conn.onopen = () => {
      if (gen !== state.hub.gen) return
      state.hub.retry = 0
      state.hub.lastMsgAt = Date.now()
    }
    conn.onmessage = (m) => {
      if (gen !== state.hub.gen) return
      state.hub.lastMsgAt = Date.now()
      if (hubFrameSeen(m.data)) return
      let ev
      try {
        ev = JSON.parse(m.data)
      } catch {
        return
      }
      onHubFrame(ev)
    }
    conn.onclose = () => {
      if (gen !== state.hub.gen) return
      state.hub.conn = null
      if (state.hub.disposed || !isHubScreen()) return
      const delay = Math.min(1000 * 2 ** state.hub.retry, 15000)
      state.hub.retry++
      if (state.hub.retryTimer) clearTimeout(state.hub.retryTimer)
      state.hub.retryTimer = setTimeout(() => {
        state.hub.retryTimer = null
        if (!state.hub.disposed && isHubScreen()) connectHub()
      }, delay)
    }
  }

  function onHubFrame(ev) {
    if (!H || H.eventRoute(ev, 'hub') === 'ignore') return
    if (ev.type === 'hello') {
      const reconnected = state.hub.hasConnected
      state.hub.hasConnected = true
      state.hub.retry = 0
      // 首次连接的数据由 openHubGroup/openHubDM/loadHubHome 拉；重连可能漏事件，
      // 先重拉快照再继续增量（绝不走会话的 openSession 分支）。
      if (reconnected) void hubResync('hello')
      return
    }
    if (ev.type === 'ping') return
    if (ev.type === 'resync') {
      void hubResync('resync')
      return
    }
    if (!H.isHubEvent(ev)) return
    onHubEvent(ev)
  }

  async function hubResync() {
    if (!H) return
    const plan = H.hubResyncTargets(state.screen, {
      groupId: state.hub.activeGroupId,
      agentId: state.hub.activeAgentId,
    })
    if (!plan) return
    if (plan.home) void loadHubHome({ silent: true })
    if (plan.groupId && plan.groupId === state.hub.activeGroupId) {
      await loadHubGroupSnapshot(plan.groupId, { reset: false })
    }
    if (plan.agentId && plan.agentId === state.hub.activeAgentId) {
      await loadHubDMSnapshot(plan.agentId, { reset: false })
    }
    if (state.hub.sheetKind === 'tasks') void loadHubTasks()
    if (state.hub.sheetKind === 'needs') void loadHubNeeds(state.hub.needsGroupId)
  }

  function onHubEvent(ev) {
    switch (ev.event_type) {
      case 'hub_message': {
        const msg = H.messageFromEvent(ev)
        if (!msg) break
        if (msg.group_id && msg.group_id === state.hub.activeGroupId) {
          const stick = nearBottom($('hub-group-messages'))
          state.hub.drafts = state.hub.drafts.filter((d) => String(d.id) !== String(msg.id))
          state.hub.groupMessages = H.mergeMessages(state.hub.groupMessages, msg)
          renderHubGroupMessages({ stick })
          renderHubGroupChrome()
          void markHubGroupRead()
        } else if (!msg.group_id && ev.agent_id && ev.agent_id === state.hub.activeAgentId) {
          const stick = nearBottom($('hub-dm-messages'))
          state.hub.dmMessages = H.mergeMessages(state.hub.dmMessages, msg)
          const sender = msg.sender || {}
          if (sender.kind === 'agent') state.hub.dmAwaiting = false
          renderHubDM({ stick })
          void markHubDMRead(msg.seq)
        }
        scheduleHubHomeRefresh()
        break
      }
      case 'hub_message_delta': {
        // 只作用于当前打开的群时间线；其它群的流式草稿不缓存（final 会补齐）。
        if (state.screen !== 'hub-group' || ev.group_id !== state.hub.activeGroupId) break
        const res = H.applyDelta(state.hub.drafts, ev, state.hub.deltaIndex)
        if (!res.applied) break
        state.hub.drafts = res.messages
        state.hub.deltaIndex = res.indexByID
        const box = $('hub-group-messages')
        const stick = nearBottom(box)
        if (!state.hub.draftNodes) state.hub.draftNodes = new Map()
        let body = state.hub.draftNodes.get(String(ev.message_id))
        if (!body) {
          // 新草稿：整列重排一次（只在首块发生），之后每块只追加文本。
          renderHubGroupMessages({ stick: true })
          renderHubGroupChrome() // 出现「正在输入」时把停止按钮带出来
        } else {
          body.appendChild(document.createTextNode(ev.delta || ''))
          if (stick) box.scrollTop = box.scrollHeight
        }
        renderHubGroupStatus()
        break
      }
      case 'hub_task_update': {
        const task = ev.task
        const taskGid = task && ((task.target && task.target.group_id) || ev.group_id)
        if (task && taskGid === state.hub.activeGroupId) {
          const stick = nearBottom($('hub-group-messages'))
          const next = H.applyTaskUpdate(state.hub.groupMessages, task)
          if (next !== state.hub.groupMessages) {
            state.hub.groupMessages = next
            renderHubGroupMessages({ stick })
          }
        }
        if (state.hub.sheetKind === 'tasks') void loadHubTasks()
        scheduleHubHomeRefresh()
        break
      }
      case 'hub_agent_status': {
        if (!ev.agent_id) break
        const on = ev.status !== 'idle' && ev.status !== 'deleted'
        state.hub.running[ev.agent_id] = on
        if (state.screen === 'hub') renderHubHome()
        if (ev.agent_id === state.hub.activeAgentId) renderHubDM()
        if (state.hub.group && (state.hub.group.members || []).some((m) => m.agent_id === ev.agent_id)) {
          renderHubGroupChrome()
          renderHubGroupStatus()
        }
        break
      }
      case 'hub_conversation': {
        if (!ev.group_id) break
        const cv = ev.conversation || null
        state.hub.convStatus[ev.group_id] = cv ? cv.status : ''
        if (ev.group_id === state.hub.activeGroupId) {
          state.hub.conversation = cv
          if (cv && (cv.status === 'stopped' || cv.status === 'exhausted')) {
            state.hub.drafts = []
            state.hub.deltaIndex = {}
            renderHubGroupMessages({ stick: nearBottom($('hub-group-messages')) })
          }
          renderHubGroupChrome()
          renderHubGroupStatus()
        } else if (state.screen === 'hub') {
          renderHubHome()
        }
        break
      }
      case 'hub_need': {
        const need = ev.need
        if (need && need.id) {
          const rest = state.hub.needs.filter((n) => n.id !== need.id)
          state.hub.needs = need.status && need.status !== 'pending' ? rest : [need, ...rest]
          if (state.screen === 'hub') renderHubHome()
        }
        if (state.hub.sheetKind === 'needs') void loadHubNeeds(state.hub.needsGroupId)
        break
      }
      case 'hub_alert': {
        // 文本与去重键同源（见 H.pushHubAlert）：code/message 都为空也只占一行。
        state.hub.alerts = H.pushHubAlert(state.hub.alerts, ev)
        if (state.screen === 'hub') renderHubAlerts()
        if (state.hub.sheetKind === 'needs') void loadHubNeeds(state.hub.needsGroupId)
        break
      }
      case 'hub_handoff': {
        scheduleHubHomeRefresh()
        if (state.hub.sheetKind === 'needs') void loadHubNeeds(state.hub.needsGroupId)
        break
      }
      default:
        break
    }
  }

  // ---- 全屏 @ 选择器 ----

  function mentionCandidates() {
    if (!H) return []
    const q = state.hub.mention.query
    return H.filterMentionCandidates(q, hubRoster(), { includeAll: true })
  }

  function openMentionPicker(initialQuery) {
    if (!H || state.screen !== 'hub-group') return
    // 按钮调用时第一个参数是 MouseEvent；只有字符串才是「键入的初始 query」。
    const query = typeof initialQuery === 'string' ? initialQuery : ''
    state.hub.mention.open = true
    state.hub.mention.query = query
    state.hub.mention.index = 0
    const filter = $('mention-filter')
    filter.value = query
    $('mention-picker').hidden = false
    renderMentionPicker()
    filter.focus()
    filter.setSelectionRange(query.length, query.length)
  }

  // 键入 @ 的即时触发：input/caret → H.mentionTypingQuery（IME 组词中不弹）。
  // 选择器已开时同步筛选词；token 被删掉就收起。
  function mentionFromComposer(input, composing) {
    if (!H || state.screen !== 'hub-group') return
    const caret = typeof input.selectionStart === 'number' ? input.selectionStart : input.value.length
    const q = H.mentionTypingQuery(input.value, caret, { composing })
    if (!q) {
      if (state.hub.mention.open && document.activeElement === input) closeMentionPicker()
      return
    }
    if (!state.hub.mention.open) {
      openMentionPicker(q.query)
      return
    }
    state.hub.mention.query = q.query
    state.hub.mention.index = 0
    $('mention-filter').value = q.query
    renderMentionPicker()
  }

  function closeMentionPicker() {
    state.hub.mention.open = false
    $('mention-picker').hidden = true
    const input = $('hub-group-input')
    if (document.activeElement === $('mention-filter') && input) input.focus()
  }

  function renderMentionPicker() {
    const list = $('mention-list')
    list.textContent = ''
    const items = mentionCandidates()
    $('mention-empty').hidden = items.length > 0
    if (state.hub.mention.index >= items.length) state.hub.mention.index = Math.max(0, items.length - 1)
    items.forEach((m, i) => {
      const row = el('button', 'mention-item')
      row.type = 'button'
      row.setAttribute('role', 'option')
      row.setAttribute('aria-selected', i === state.hub.mention.index ? 'true' : 'false')
      row.append(hubSigil({ agent: m, size: 24, status: false }))
      const main = el('div', 'mention-item-main')
      main.appendChild(el('div', 'mention-item-name', '@' + m.name))
      const sub = [m.role, m.groupRole === 'observer' ? '旁听' : m.groupRole === 'owner' ? '群主' : '']
        .filter(Boolean)
        .join(' · ')
      if (sub) main.appendChild(el('div', 'mention-item-sub', sub))
      row.appendChild(main)
      row.addEventListener('click', () => pickMention(m))
      list.appendChild(row)
    })
  }

  function pickMention(member) {
    const input = $('hub-group-input')
    const caret = typeof input.selectionStart === 'number' ? input.selectionStart : input.value.length
    const applied = H.applyMention(input.value, caret, member.name)
    input.value = applied.text
    // 插入 @ 改动了草稿：失败草稿的幂等槽随之失效（下一次发送是新消息）。
    state.hub.pendingGroupSend = H.sendSlotAfterEdit(state.hub.pendingGroupSend, input.value)
    input.focus()
    input.setSelectionRange(applied.caret, applied.caret)
    input.style.height = 'auto'
    input.style.height = Math.min(input.scrollHeight, 120) + 'px'
    closeMentionPicker()
  }

  // ---- Hub 抽屉：任务板 / Needs / 派任务 ----

  function openHubSheet(title, kind) {
    $('hub-sheet-title').textContent = title
    $('hub-sheet-body').textContent = ''
    $('hub-sheet').hidden = false
    state.hub.sheetKind = kind
  }

  function closeHubSheet() {
    $('hub-sheet').hidden = true
    state.hub.sheetKind = ''
  }

  let hubTasksSeq = 0
  let hubNeedsSeq = 0

  function openHubTasks() {
    if (!state.hub.activeGroupId) return
    openHubSheet('任务板', 'tasks')
    $('hub-sheet-body').appendChild(el('div', 'hint', '加载中…'))
    void loadHubTasks()
  }

  async function loadHubTasks() {
    const gid = state.hub.activeGroupId
    if (!gid) return
    const seq = ++hubTasksSeq
    try {
      const data = await api('hub_task_list', { group_id: gid, limit: 100 })
      if (seq !== hubTasksSeq) return
      state.hub.tasks = Array.isArray(data.tasks) ? data.tasks : []
      if (state.hub.sheetKind === 'tasks') renderTasksSheet()
    } catch (e) {
      if (seq === hubTasksSeq && state.hub.sheetKind === 'tasks') {
        $('hub-sheet-body').textContent = ''
        $('hub-sheet-body').appendChild(el('div', 'hub-empty', '加载任务失败：' + (e.message || e)))
      }
    }
  }

  function renderTasksSheet() {
    if (state.hub.sheetKind !== 'tasks') return
    const body = $('hub-sheet-body')
    body.textContent = ''
    if (!state.hub.tasks.length) {
      body.appendChild(el('div', 'hub-empty', '这个群还没有任务。'))
      return
    }
    for (const t of state.hub.tasks) {
      const sv = H.taskStatusView(t.status)
      const item = el('div', 'hub-sheet-item')
      const head = el('div', 'hub-card-head')
      head.appendChild(el('span', 'hub-status-chip hub-tone-' + sv.tone, sv.label))
      head.appendChild(el('span', 'hub-sheet-title', t.title || t.instruction || t.id))
      item.appendChild(head)
      const target = (t.target && t.target.agent_id) || ''
      const meta = el('div', 'hub-sheet-sub')
      meta.textContent = [target ? '执行者 ' + hubAgentName(target) : '', hubTime(t.updated_at || t.created_at), t.id]
        .filter(Boolean)
        .join(' · ')
      item.appendChild(meta)
      if (t.error) item.appendChild(el('div', 'hub-card-error', t.error))
      const actions = el('div', 'hub-sheet-actions')
      if (!sv.terminal) {
        const cancel = el('button', 'danger', '取消任务')
        cancel.type = 'button'
        cancel.addEventListener('click', () => runTaskAction('hub_task_cancel', t.id, cancel))
        actions.appendChild(cancel)
      } else if (!t.target || t.target.kind !== 'agent') {
        const retry = el('button', 'ghost', '重试')
        retry.type = 'button'
        retry.addEventListener('click', () => runTaskAction('hub_task_retry', t.id, retry))
        actions.appendChild(retry)
      }
      if (actions.childNodes.length) item.appendChild(actions)
      body.appendChild(item)
    }
  }

  async function runTaskAction(cmd, taskId, btn) {
    btn.disabled = true
    try {
      await api(cmd, { task_id: taskId })
      await loadHubTasks()
      scheduleHubHomeRefresh()
    } catch (e) {
      btn.disabled = false
      $('hub-sheet-body').appendChild(el('div', 'hub-empty', '操作失败：' + (e.message || e)))
    }
  }

  function openHubNeeds(groupId) {
    state.hub.needsGroupId = groupId || ''
    state.hub.sheetNeeds = []
    openHubSheet('Needs you', 'needs')
    $('hub-sheet-body').appendChild(el('div', 'hint', '加载中…'))
    void loadHubNeeds(groupId)
  }

  async function loadHubNeeds(groupId) {
    const seq = ++hubNeedsSeq
    const payload = { status: 'pending' }
    if (groupId) payload.group_id = groupId
    try {
      const data = await api('hub_need_list', payload)
      if (seq !== hubNeedsSeq) return
      const list = Array.isArray(data.needs) ? data.needs : []
      // 群抽屉的列表是过滤后的，不能覆盖首页的全局计数。
      if (groupId) state.hub.sheetNeeds = list
      else {
        state.hub.needs = list
        if (state.hub.sheetKind === 'needs' && !state.hub.needsGroupId) state.hub.sheetNeeds = list
        if (state.screen === 'hub') renderHubHome()
      }
      if (state.hub.sheetKind === 'needs') renderNeedsSheet()
    } catch (e) {
      if (seq === hubNeedsSeq && state.hub.sheetKind === 'needs') {
        $('hub-sheet-body').textContent = ''
        $('hub-sheet-body').appendChild(el('div', 'hub-empty', '加载 Needs 失败：' + (e.message || e)))
      }
    }
  }

  function renderNeedsSheet() {
    if (state.hub.sheetKind !== 'needs') return
    const body = $('hub-sheet-body')
    body.textContent = ''
    if (!state.hub.sheetNeeds.length) {
      body.appendChild(el('div', 'hub-empty', '没有待处理项。'))
      return
    }
    for (const need of state.hub.sheetNeeds) {
      const item = el('div', 'hub-sheet-item')
      const head = el('div', 'hub-card-head')
      head.appendChild(el('span', 'hub-status-chip', needKindLabel(need.kind)))
      head.appendChild(el('span', 'hub-sheet-title', need.title || need.kind || '待处理'))
      item.appendChild(head)
      if (need.body) {
        let pretty = need.body
        try {
          pretty = JSON.stringify(JSON.parse(need.body), null, 2)
        } catch {
          // 正文不是 JSON：原样展示
        }
        item.appendChild(el('pre', 'hub-need-body', pretty.length > 1200 ? pretty.slice(0, 1200) + '…' : pretty))
      }
      const meta = el('div', 'hub-sheet-sub')
      meta.textContent = [need.agent_id ? hubAgentName(need.agent_id) : '', hubTime(need.created_at), need.task_id || '']
        .filter(Boolean)
        .join(' · ')
      if (meta.textContent) item.appendChild(meta)
      const actions = el('div', 'hub-sheet-actions')
      addNeedActions(item, actions, need)
      if (actions.childNodes.length) item.appendChild(actions)
      body.appendChild(item)
    }
  }

  function needKindLabel(kind) {
    return { approval: '审批', question: '提问', handoff: '交接', failure: '失败', budget: '预算' }[kind] || kind || '事项'
  }

  function addNeedActions(item, actions, need) {
    const resolve = (resolution, answers) => {
      const btn = actions.querySelector('button')
      if (btn) btn.disabled = true
      api('hub_need_resolve', Object.assign({ need_id: need.id, resolution }, answers ? { answers } : {}))
        .then(() => loadHubNeeds(state.hub.needsGroupId))
        .then(() => scheduleHubHomeRefresh())
        .catch((e) => {
          if (btn) btn.disabled = false
          $('hub-sheet-body').appendChild(el('div', 'hub-empty', '处理失败：' + (e.message || e)))
        })
    }
    if (need.kind === 'approval') {
      actions.appendChild(namedButton('批准', 'primary', () => resolve('approve')))
      actions.appendChild(namedButton('拒绝', 'ghost', () => resolve('deny')))
      return
    }
    if (need.kind === 'handoff') {
      actions.appendChild(namedButton('接受', 'primary', () => resolve('accept')))
      actions.appendChild(namedButton('拒绝', 'ghost', () => resolve('decline')))
      return
    }
    if (need.kind === 'question') {
      const questions = H.parseNeedQuestions(need)
      if (!questions.length) {
        item.appendChild(el('div', 'hub-empty', '这个问题需要在电脑端会话里回答。'))
        return
      }
      const answers = {}
      for (const q of questions) {
        const label = el('label', '', q.question)
        const input = document.createElement('input')
        input.placeholder = '输入回答'
        input.addEventListener('input', () => {
          answers[q.id] = input.value
        })
        label.appendChild(input)
        item.appendChild(label)
      }
      actions.appendChild(namedButton('提交回答', 'primary', () => resolve('answered', answers)))
      return
    }
    actions.appendChild(namedButton('标记已读', 'ghost', () => resolve('acknowledged')))
  }

  function namedButton(label, cls, fn) {
    const b = el('button', cls, label)
    b.type = 'button'
    b.addEventListener('click', fn)
    return b
  }

  function openHubAssign() {
    const gid = state.hub.activeGroupId
    if (!gid) return
    const members = H.assignableMembers(hubRoster())
    openHubSheet('派任务', 'assign')
    const body = $('hub-sheet-body')
    if (!members.length) {
      body.appendChild(el('div', 'hub-empty', '没有可接任务的成员（观察者不能接任务）。'))
      return
    }
    const form = el('div', 'hub-form')
    const toLabel = el('label', '', '执行成员')
    const select = document.createElement('select')
    for (const m of members) {
      const o = document.createElement('option')
      o.value = m.id
      o.textContent = m.name + (m.role ? ' · ' + m.role : '')
      select.appendChild(o)
    }
    toLabel.appendChild(select)
    const titleLabel = el('label', '', '标题（可空）')
    const titleInput = document.createElement('input')
    titleInput.placeholder = '例如：整理竞品价格'
    titleLabel.appendChild(titleInput)
    const insLabel = el('label', '', '任务说明')
    const instruction = document.createElement('textarea')
    instruction.rows = 4
    instruction.placeholder = '说清楚目标、输入和验收标准…'
    insLabel.appendChild(instruction)
    const err = el('div', 'error')
    err.hidden = true
    const submit = el('button', 'primary', '派发')
    submit.type = 'button'
    submit.addEventListener('click', async () => {
      const agentId = select.value
      const text = instruction.value.trim()
      if (!agentId || !text) {
        err.hidden = false
        err.textContent = '请选择成员并填写任务说明。'
        return
      }
      submit.disabled = true
      try {
        await api('hub_group_assign', H.assignPayload(gid, agentId, text, titleInput.value.trim(), nextClientMsgID('a')))
        closeHubSheet()
        scheduleHubHomeRefresh()
      } catch (e) {
        submit.disabled = false
        err.hidden = false
        err.textContent = '派发失败：' + (e.message || e)
      }
    })
    form.append(toLabel, titleLabel, insLabel, err, submit)
    body.appendChild(form)
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

  // 会画到对话流里的事件类型；只有这些必须严格属于当前会话。
  const SESSION_SCOPED = new Set([
    'content_chunk',
    'reasoning_chunk',
    'llm_start',
    'llm_end',
    'tool_run_start',
    'tool_run_end',
    'agent_end',
    'session_persist_error',
  ])

  function onEvent(ev) {
    // 聊天连接只处理会话事件：Hub 事件即使因双连接/回调写错而到这里也直接丢弃，
    // 绝不把 hub_message 画进聊天屏（反之由 onHubFrame 的 eventRoute 兜住）。
    if (H && H.eventRoute(ev, 'chat') === 'ignore') return
    // emit 给每行注入了 session_id：不是当前会话的一条都不画。
    // 这是「串台」的最后一道闸 —— 双连接、hub 过滤失效、切会话竞态都挡得住。
    if (ev.session_id && state.sid && ev.session_id !== state.sid && SESSION_SCOPED.has(ev.event_type)) return
    switch (ev.event_type) {
      case 'content_chunk':
        if (isStaleChunk(ev, 'c')) break
        if (!state.live.text) collapseLiveThinking() // 思考结束 → 正文开始
        state.running = true
        state.live.text += ev.content || ''
        scheduleLive()
        break
      case 'reasoning_chunk':
        if (isStaleChunk(ev, 'r')) break
        state.running = true
        state.live.thinking += ev.content || ''
        scheduleLive()
        break
      case 'agent_start':
        commitLive() // 新 run：上一轮残留不串到这一轮
        state.running = true
        updateRunning()
        break
      case 'llm_start':
        commitLive() // 新一轮 LLM：上一轮先定稿
        state.running = true
        updateRunning()
        break
      case 'llm_end':
        // 本轮最终文本（权威）；然后立刻冻结成块，后续工具卡才能排在它后面
        if (typeof ev.content === 'string' && ev.content) state.live.text = ev.content
        commitLive()
        break
      case 'tool_run_start':
        commitLive() // 工具卡之前的内容先定稿，避免文字被"推"到卡片下方
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
        commitLive()
        state.running = false
        state.live = { text: '', thinking: '' }
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
      let d = box.querySelector('details.thinking[data-live="1"]')
      if (!d) {
        d = makeThinking(true) // 流式中展开
        d.dataset.live = '1'
        box.appendChild(d)
      }
      patchText(d.querySelector('.think-body'), state.live.thinking)
      setThinkSummary(d, state.live.thinking)
    }
    if (state.live.text || state.running) {
      let node = box.querySelector('.msg.assistant[data-live="1"]')
      if (!node) {
        node = el('div', 'msg assistant')
        node.dataset.live = '1'
        box.appendChild(node)
      }
      patchText(node, state.live.text)
      node.classList.toggle('cursor', state.running)
    }
    updateRunning()
    if (stick) scrollToEnd()
    // 没贴底说明用户在往回读：给一个「最新」入口，别让流式内容"消失"在下面
    $('jump').hidden = stick || !state.running
  }

  // 思考块：流式时展开（看得见进度），本轮定稿后收起成一行「思考 · N 字」，
  // 点开仍可看全文 —— 否则長 reasoning 会把对话顶出好几屏。
  function makeThinking(open) {
    const d = el('details', 'msg thinking')
    if (open) d.open = true
    d.appendChild(el('summary', 'think-summary', '思考'))
    d.appendChild(el('div', 'think-body'))
    return d
  }

  function setThinkSummary(node, text) {
    const sm = node.querySelector('summary')
    if (sm) sm.textContent = '思考 · ' + [...(text || '')].length + ' 字'
  }

  // 思考阶段一结束（正文第一个 chunk 到达）就折叠：不必等到本轮的 llm_end，
  // 更不必等整轮 run 结束 —— 那期间展开的 reasoning 一直占着屏幕。
  function collapseLiveThinking() {
    const d = $('messages').querySelector('details.thinking[data-live="1"]')
    if (!d || !d.open) return
    if (d.dataset.autoclosed === '1') return // 用户手动展开过就不再强行收起
    d.open = false
    d.dataset.autoclosed = '1'
  }

  // 一次 run（agent_start → agent_end）里有**多轮** LLM：每轮 reasoning/content
  // 之后可能跟工具调用，然后是下一轮。轮次边界必须把 live 冻结成永久块，
  // 否则：(1) 后一轮的 llm_end 会整段覆盖前一轮；(2) 文本永远堆在最后，
  // 工具卡插在它前面，顺序与桌面 blocks 不一致。
  function commitLive() {
    // 先冲刷待渲染的帧：chunk 与 llm_end 常在同一帧内到达（rAF 还没跑），
    // 此时 DOM 里根本没有 live 节点，直接清 state.live 会把本轮文本丢掉。
    if (liveRaf) {
      cancelAnimationFrame(liveRaf)
      liveRaf = 0
    }
    if (state.live.text || state.live.thinking) patchLive()
    // 记进 blocks：updateTool / 分页 / 重连等任何一次 renderMessages() 都要能
    // 重新画出这些已定稿的轮次（否则一次工具结束就把前面冲掉）
    const text = (state.live.text || '').trim()
    const think = (state.live.thinking || '').trim()
    if (text || think) state.blocks.push({ kind: 'assistant', text: state.live.text, thinking: state.live.thinking })
    const box = $('messages')
    for (const n of box.querySelectorAll('[data-live="1"]')) {
      if (!(n.textContent || '').trim()) {
        n.remove() // 空块不留占位
        continue
      }
      n.removeAttribute('data-live')
      n.classList.remove('cursor')
      if (n.tagName === 'DETAILS' && n.classList.contains('thinking')) {
        n.open = false // 一轮结束：思考收起，只留一行摘要
        setThinkSummary(n, (n.querySelector('.think-body') || {}).textContent || '')
      }
    }
    state.live = { text: '', thinking: '' }
    state.chunkSeen.clear()
  }

  // 只追加新增部分：每帧整段重写 textContent 会让长文本反复重排，
  // 在 iPhone 上表现为内容抖动/闪烁（滚动位置被反复钳制）。
  function patchText(node, text) {
    const cur = node.textContent || ''
    if (text === cur) return
    if (cur && text.startsWith(cur)) {
      node.appendChild(document.createTextNode(text.slice(cur.length)))
      return
    }
    node.textContent = text // 首次渲染 / live 快照整段替换
  }

  function nearBottom(box) {
    return box.scrollHeight - box.scrollTop - box.clientHeight < 80
  }

  // 流式块去重：同一 (类型, request_id, index) 只处理一次。
  // 两条连接各投一遍、或重连补发时 index 不会前进 —— 直接丢，比字符串比较精确。
  // 类型必须分开计数：reasoning 与 content 的 index 是各自独立的序列
  //（共用计数器会把交替到达的第一个 content 块当成重复而丢掉）。
  function isStaleChunk(ev, kind) {
    if (typeof ev.index !== 'number') return false
    const key = kind + '|' + (ev.request_id || ev.run_id || '')
    const last = state.chunkSeen.get(key)
    if (last === undefined || ev.index > last) {
      state.chunkSeen.set(key, ev.index)
      return false
    }
    // index 回退到 0 = 新一轮请求（provider 没给 request_id 时靠这条兜底）
    if (ev.index === 0 && last > 0) {
      state.chunkSeen.set(key, 0)
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
    state.chunkSeen.clear()
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
    // 原地替换这一个卡片即可。**不能** renderMessages()：那是按 state.blocks 整页重绘，
    // 而本轮正在流的节点还不在 blocks 里，一重绘就把前面的内容冲掉（顺序全乱）。
    const sel = id ? `#messages .tool-group[data-tool-id="${CSS.escape(String(id))}"]` : ''
    const old = sel ? document.querySelector(sel) : null
    if (old && b) {
      old.replaceWith(renderBlock(b))
      scrollToEnd()
      return
    }
    if (b) renderMessages() // 兜底：节点不在当前窗口（例如刚分页进来）
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
    // 覆盖层优先：外观/搜索/ @ 选择器/抽屉开着时，返回键只关覆盖层。
    if (!$('theme-sheet').hidden) {
      $('theme-sheet').hidden = true
      return
    }
    if (state.hub.search.open) {
      closeHubSearch()
      return
    }
    if (state.hub.mention.open) {
      closeMentionPicker()
      return
    }
    if (!$('hub-sheet').hidden) {
      closeHubSheet()
      return
    }
    stopAgentPoll()
    const prev = state.nav.pop()
    if (state.screen === 'hub-group' || state.screen === 'hub-dm') {
      state.hub.activeGroupId = ''
      state.hub.activeAgentId = ''
      state.hub.group = null
      state.hub.dmAgent = null
      state.hub.drafts = []
      state.hub.deltaIndex = {}
      if (prev === 'hub') {
        show('hub')
        renderHubHome()
        ensureHubSocket()
        loadHubHome({ silent: true })
        return
      }
      leaveHub()
      return
    }
    if (state.screen === 'hub') {
      leaveHub()
      return
    }
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
      if (!(await verifyToken())) {
        show('pair')
        pairError('凭据失效或已在桌面端撤销，请重新配对（也可用桌面端给的免配对链接进入）')
        return
      }
      await loadWorkspaces()
    } catch (e) {
      if (state.token) pairError(e.message)
      show('pair')
    }
  }

  // verifyToken 用最轻的 /api/ping 自检：凭据无效时把 token 留着不删，
  // 让「重新配对」覆盖它——静默清空会让每次抖动都变成一次扫码。
  async function verifyToken() {
    if (!state.token) return false
    try {
      const r = await fetch('/api/ping', { headers: { Authorization: 'Bearer ' + state.token } })
      if (!r.ok) return true // 5xx/网关问题不算凭据失效
      const d = await r.json().catch(() => ({}))
      return d.paired !== false
    } catch {
      return true // 离线：先放行，进列表会给出网络错误
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
    if (isHubScreen() && (!state.hub.conn || state.hub.conn.readyState !== WebSocket.OPEN)) {
      state.hub.retry = 0
      ensureHubSocket()
    }
  })
  setInterval(() => {
    if (state.disposed) return
    if (isHubScreen()) {
      if (!state.hub.conn || state.hub.conn.readyState !== WebSocket.OPEN) {
        ensureHubSocket()
      } else if (Date.now() - state.hub.lastMsgAt > WATCHDOG_MS) {
        // 半死连接（iOS 息屏常见）：主动重连，并按「重连」处理重拉 Hub 快照。
        state.hub.retry = 0
        state.hub.hasConnected = true
        state.hub.gen++
        try {
          state.hub.conn.close()
        } catch {
          /* ignore */
        }
        state.hub.conn = null
        state.hub.disposed = false
        connectHub() // 新连接的 hello 会触发 hubResync
      }
      return
    }
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
    $('theme').addEventListener('click', () => {
      $('theme-sheet').hidden = false
      renderThemeOptions()
    })
    $('theme-close').addEventListener('click', () => {
      $('theme-sheet').hidden = true
    })
    for (const b of $('theme-options').querySelectorAll('[data-theme-mode]')) {
      b.addEventListener('click', () => {
        setThemeMode(b.dataset.themeMode)
        $('theme-sheet').hidden = true
      })
    }
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
    $('open-hub').addEventListener('click', openHub)
    $('hub-search-btn').addEventListener('click', openHubSearch)
    $('hub-search-close').addEventListener('click', closeHubSearch)
    $('hub-search-clear').addEventListener('click', () => {
      clearHubSearch()
    })
    $('hub-search-more').addEventListener('click', () => {
      const s = state.hub.search
      if (s.loading || !s.hasMore) return
      void runHubSearch(true)
    })
    $('hub-search-input').addEventListener('input', () => {
      state.hub.search.query = $('hub-search-input').value
      renderHubSearchResults()
      scheduleHubSearch()
    })
    $('hub-search-input').addEventListener('keydown', (e) => {
      if (e.key !== 'Enter') return
      e.preventDefault()
      const s = state.hub.search
      if (s.timer) {
        clearTimeout(s.timer)
        s.timer = null
      }
      void runHubSearch(false)
    })
    // Escape 在 document 级处理：焦点可能不在输入框（例如刚点过「更早的结果」），
    // 但面板开着时 Escape 一律清空并关闭。stopPropagation 无所谓，面板只有一个。
    document.addEventListener('keydown', (e) => {
      if (e.key !== 'Escape' || !state.hub.search.open) return
      e.preventDefault()
      clearHubSearch()
    })
    for (const b of $('hub-search-scopes').querySelectorAll('[data-search-scope]')) {
      b.addEventListener('click', () => {
        const s = state.hub.search
        s.scope = b.dataset.searchScope || 'all'
        renderHubSearchScopes()
        if (String(s.query || '').trim()) void runHubSearch(false)
      })
    }
    $('hub-needs').addEventListener('click', () => openHubNeeds(''))
    $('hub-group-at').addEventListener('click', () => openMentionPicker(''))
    $('hub-group-send').addEventListener('click', sendHubGroup)
    $('hub-dm-send').addEventListener('click', sendHubDM)
    $('mention-back').addEventListener('click', closeMentionPicker)
    $('hub-sheet-close').addEventListener('click', closeHubSheet)
    $('mention-filter').addEventListener('input', () => {
      state.hub.mention.query = $('mention-filter').value
      state.hub.mention.index = 0
      renderMentionPicker()
    })
    $('mention-filter').addEventListener('keydown', (e) => {
      const items = mentionCandidates()
      if (e.key === 'Escape') {
        e.preventDefault()
        closeMentionPicker()
        return
      }
      if (e.key === 'ArrowDown') {
        e.preventDefault()
        if (items.length) state.hub.mention.index = (state.hub.mention.index + 1) % items.length
        renderMentionPicker()
        return
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault()
        if (items.length) state.hub.mention.index = (state.hub.mention.index - 1 + items.length) % items.length
        renderMentionPicker()
        return
      }
      if (e.key === 'Enter') {
        e.preventDefault()
        const pick = items[state.hub.mention.index]
        if (pick) pickMention(pick)
      }
    })
    for (const [id, fn] of [
      ['hub-group-input', sendHubGroup],
      ['hub-dm-input', sendHubDM],
    ]) {
      const t = $(id)
      let composing = false
      t.addEventListener('compositionstart', () => {
        composing = true
      })
      t.addEventListener('compositionend', () => {
        composing = false
        if (id === 'hub-group-input') mentionFromComposer(t, false)
      })
      t.addEventListener('input', (e) => {
        t.style.height = 'auto'
        t.style.height = Math.min(t.scrollHeight, 120) + 'px'
        // 用户改了草稿：正文已不是那条失败消息，重试必须换新的 client_msg_id。
        if (id === 'hub-group-input') {
          state.hub.pendingGroupSend = H.sendSlotAfterEdit(state.hub.pendingGroupSend, t.value)
        } else {
          state.hub.pendingDMSend = H.sendSlotAfterEdit(state.hub.pendingDMSend, t.value)
        }
        // 键入 @ 即时唤起全屏选择器；IME 组词期间不打断（＠ 上屏后再判断）。
        if (id === 'hub-group-input') mentionFromComposer(t, composing || e.isComposing === true)
      })
      t.addEventListener('keydown', (e) => {
        if (e.key === 'Escape' && state.hub.mention.open) {
          e.preventDefault()
          closeMentionPicker()
          return
        }
        if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
          e.preventDefault()
          fn()
        }
      })
    }
    // 贴底 + 可见才推进已读；往上读历史或切后台时不动游标。
    for (const id of ['hub-group-messages', 'hub-dm-messages']) {
      $(id).addEventListener('scroll', scheduleHubReadCheck, { passive: true })
    }
    document.addEventListener('visibilitychange', () => {
      if (!document.hidden && isHubScreen()) scheduleHubReadCheck()
    })
    $('agents-bar').addEventListener('click', openAgents)
    $('send').addEventListener('click', send)
    $('copy-link').addEventListener('click', async () => {
      const url = bookmarkURL()
      try {
        await navigator.clipboard.writeText(url)
        $('copy-link').textContent = '已复制免配对链接'
        setTimeout(() => {
          $('copy-link').textContent = '复制免配对链接'
        }, 1800)
      } catch {
        // 无剪贴板权限（无头/非安全上下文）：直接把链接摊开，手抄或脚本取用
        prompt('免配对链接（等价于密码，勿外传）', url)
      }
    })
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
    window.addEventListener('beforeunload', () => {
      disconnect()
      disconnectHub()
    })
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

  // 凭据优先级：URL（免配对链接）> localStorage > cookie
  const urlTok = tokenFromURL()
  if (urlTok) rememberToken(urlTok)
  else if (!state.token) {
    const ck = cookieToken()
    if (ck) state.token = ck
  }

  // 配对屏常驻显示当前访问地址：换了主机/端口（= 换了 localStorage 作用域）时一眼可见
  $('pair-addr').textContent = '当前地址：' + location.origin + '/m/'

  if (pairFromHash()) doPair()
  else boot()
})()
