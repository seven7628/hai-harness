// hub_model.js —— 手机 Agent Hub 的纯逻辑层（无 DOM、无网络）。
//
// 为什么单独一层：手机端没有构建步骤，app.js 又要同时处理聊天流与 Hub 流。
// 把「seq 合并/去重、乐观消息替换、delta 追加、@ 分词与插入、任务卡映射、
// 事件分流、resync 决策」这些容易错的地方做成纯函数，Node 直测（test-mobile-hub.mjs），
// 页面只负责把结果画出来。
//
// 约定：
//   - 本文件不产生 HTML 字符串，调用方只用 textContent/createElement；
//   - 所有函数不改入参（返回新数组/新对象），页面可以放心用旧快照回滚；
//   - 字段名与 bridge 的 hub 命令/事件一致（seq/id/client_msg_id/refs…）。
;(function (root, factory) {
  if (typeof module === 'object' && module.exports) module.exports = factory()
  else root.HaiMobileHubModel = factory()
})(typeof globalThis !== 'undefined' ? globalThis : this, function () {
  'use strict'

  const HUB_PREFIX = 'hub_'
  // @all 不是真实 agent id：正文里显示 @all/@所有人，发送时走 mention_all 字段。
  const ALL_MENTION_ID = '__all__'
  // 服务端帧（没有 event_type）。Hub 连接只认这三种；其它一律忽略。
  const HUB_FRAMES = { hello: true, ping: true, resync: true }

  function str(v) {
    if (typeof v === 'string') return v.trim()
    if (typeof v === 'number' && Number.isFinite(v)) return String(v)
    return ''
  }

  function num(v) {
    const n = typeof v === 'number' ? v : Number(v)
    return Number.isFinite(n) ? n : 0
  }

  // ---------- 主题 ----------
  // 「跟随系统 / 墨 / 宣纸」三档。值只存模式，实际皮肤由 resolveTheme 算出来；
  // 颜色全部来自 tokens.css（ink 的 --paper / paper 的 --paper），不新增色板。
  const THEME_MODES = ['system', 'ink', 'paper']
  const THEME_COLORS = { ink: '#1a1614', paper: '#fdfcfb' }

  function normalizeThemeMode(v) {
    const s = str(v).toLowerCase()
    return THEME_MODES.indexOf(s) >= 0 ? s : 'system'
  }

  // prefersLight = matchMedia('(prefers-color-scheme: light)').matches
  function resolveTheme(mode, prefersLight) {
    const m = normalizeThemeMode(mode)
    if (m === 'system') return prefersLight ? 'paper' : 'ink'
    return m
  }

  function themeColor(theme) {
    return THEME_COLORS[theme === 'paper' ? 'paper' : 'ink']
  }

  // ---------- 事件分流 ----------
  // 会话流与 Hub 流在 bridge 侧已隔离；客户端再挡一层，防止两条连接的帧
  // 因为回调写错而互相污染（例如 hub_message 被画进聊天屏）。

  function isHubEvent(ev) {
    return !!ev && typeof ev.event_type === 'string' && ev.event_type.indexOf(HUB_PREFIX) === 0
  }

  // source = 'chat' | 'hub'。返回 'chat' / 'hub' / 'ignore'。
  // 注意：resync 只在 hub 连接上被接受；聊天连接仍走自己的 live/resync 分支。
  function eventRoute(ev, source) {
    if (!ev || typeof ev !== 'object') return 'ignore'
    if (source === 'hub') {
      if (isHubEvent(ev)) return 'hub'
      if (typeof ev.type === 'string' && HUB_FRAMES[ev.type]) return 'hub'
      return 'ignore'
    }
    if (source === 'chat') {
      if (isHubEvent(ev)) return 'ignore'
      return 'chat'
    }
    return 'ignore'
  }

  // resync/重连后要重拉哪些快照。**只返回 Hub 目标**：绝不返回会话 sid，
  // 否则客户端会误走 openSession(state.sid)（D-mobile-hub-resync）。
  function hubResyncTargets(screen, ids) {
    const out = { home: true, groupId: '', agentId: '' }
    if (screen === 'hub-group') out.groupId = str(ids && ids.groupId)
    else if (screen === 'hub-dm') out.agentId = str(ids && ids.agentId)
    return out
  }

  // ---------- 消息合并 ----------

  function clientMsgID(msg) {
    if (!msg) return ''
    return str(msg.client_msg_id) || str(msg.refs && msg.refs.client_msg_id)
  }

  function messageKeys(msg) {
    const keys = []
    if (msg && str(msg.id)) keys.push('id:' + str(msg.id))
    if (msg && num(msg.seq) > 0) keys.push('seq:' + num(msg.seq))
    return keys
  }

  // 未落盘的乐观消息 seq=0，永远排在已落盘消息之后。
  function sortSeq(msg) {
    const s = num(msg && msg.seq)
    return s > 0 ? s : Number.MAX_SAFE_INTEGER
  }

  // 合并一页/一条消息：按 id 与 seq 去重，按 seq 升序；用 client_msg_id
  // 把乐观气泡替换成服务端回执（服务端消息才是权威形状）。
  function mergeMessages(existing, incoming) {
    const out = Array.isArray(existing) ? existing.filter(Boolean).slice() : []
    const add = Array.isArray(incoming) ? incoming : incoming ? [incoming] : []

    function indexes() {
      const byKey = new Map()
      const byClient = new Map()
      for (let i = 0; i < out.length; i++) {
        const m = out[i]
        for (const k of messageKeys(m)) if (!byKey.has(k)) byKey.set(k, i)
        const cid = clientMsgID(m)
        if (cid && !byClient.has(cid)) byClient.set(cid, i)
      }
      return { byKey, byClient }
    }

    for (const raw of add) {
      if (!raw || typeof raw !== 'object') continue
      const inc = Object.assign({}, raw)
      const cid = clientMsgID(inc)
      const idx = indexes()
      let at = -1
      if (cid && idx.byClient.has(cid)) at = idx.byClient.get(cid)
      if (at < 0) {
        for (const k of messageKeys(inc)) {
          if (idx.byKey.has(k)) {
            at = idx.byKey.get(k)
            break
          }
        }
      }
      if (at >= 0) {
        const prev = out[at]
        const merged = Object.assign({}, prev, inc)
        // 服务端回执到了：本地的 pending/local 标记不再成立。
        if (num(inc.seq) > 0) {
          delete merged.pending
          delete merged.local
          delete merged.draft
        }
        if (prev.refs || inc.refs) merged.refs = Object.assign({}, prev.refs || {}, inc.refs || {})
        out[at] = merged
      } else {
        out.push(inc)
      }
    }

    // 跨页合并可能带重复：以 id/seq/client_msg_id 做最后一道去重。
    const seen = new Set()
    const seenClient = new Set()
    const unique = []
    for (const m of out) {
      const cid = clientMsgID(m)
      const keys = messageKeys(m)
      if ((cid && seenClient.has(cid)) || keys.some((k) => seen.has(k))) continue
      if (cid) seenClient.add(cid)
      for (const k of keys) seen.add(k)
      unique.push(m)
    }
    unique.sort((a, b) => sortSeq(a) - sortSeq(b))
    return unique
  }

  // 发送前先画一条本地气泡；服务端 hub_message 到达后按 client_msg_id 替换。
  function optimisticMessage(opts) {
    const o = opts || {}
    const cid = str(o.clientMsgId) || 'local-' + Date.now() + '-' + Math.random().toString(36).slice(2, 8)
    const msg = {
      id: 'local:' + cid,
      seq: 0,
      client_msg_id: cid,
      kind: 'text',
      sender: { kind: o.senderKind || 'human' },
      text: String(o.text || ''),
      created_at: o.createdAt || new Date().toISOString(),
      pending: true,
      local: true,
    }
    if (o.groupId) msg.group_id = str(o.groupId)
    if (o.agentId) msg.agent_id = str(o.agentId)
    return msg
  }

  // hub_message 事件 → 消息对象。补齐事件外层字段，避免后端版本差异导致丢正文。
  function messageFromEvent(ev) {
    if (!ev || !ev.message || typeof ev.message !== 'object') return null
    const msg = Object.assign({}, ev.message)
    if (num(msg.seq) <= 0 && num(ev.seq) > 0) msg.seq = num(ev.seq)
    if (!str(msg.group_id) && ev.group_id) msg.group_id = str(ev.group_id)
    if (!str(msg.id) && ev.message_id) msg.id = str(ev.message_id)
    if (ev.client_msg_id && !clientMsgID(msg)) {
      msg.refs = Object.assign({}, msg.refs || {}, { client_msg_id: str(ev.client_msg_id) })
    }
    if (!msg.kind) msg.kind = 'text'
    return msg
  }

  // hub_message_delta 是临时草稿：按 message_id + index 追加。
  // 只应作用于「当前打开的时间线」；final message 到达后同 id 的 delta 全部丢弃。
  // indexByID 由调用方持有（message_id → 已应用的最大 index），本函数返回新对象。
  function applyDelta(messages, ev, indexByID) {
    const list = Array.isArray(messages) ? messages.slice() : []
    const seen = Object.assign({}, indexByID || {})
    if (!ev || !str(ev.message_id)) return { messages: list, indexByID: seen, applied: false }
    const id = str(ev.message_id)
    const idx = Number(ev.index)
    const hasIdx = Number.isFinite(idx)
    const last = seen[id]
    if (hasIdx && last !== undefined && idx <= last) {
      return { messages: list, indexByID: seen, applied: false, stale: true }
    }
    // final 已经落入时间线：delta 是迟到的临时态，不能覆盖权威正文。
    const final = list.find((m) => m && str(m.id) === id && num(m.seq) > 0)
    if (final) {
      if (hasIdx) seen[id] = Math.max(last === undefined ? idx : last, idx)
      return { messages: list, indexByID: seen, applied: false, stale: true }
    }
    const delta = typeof ev.delta === 'string' ? ev.delta : ''
    if (!delta) return { messages: list, indexByID: seen, applied: false }

    const at = list.findIndex((m) => m && str(m.id) === id && num(m.seq) <= 0)
    if (at >= 0) {
      const prev = list[at]
      list[at] = Object.assign({}, prev, { text: String(prev.text || '') + delta, draft: true })
    } else {
      list.push({
        id,
        group_id: str(ev.group_id),
        kind: 'text',
        seq: 0,
        draft: true,
        sender: { kind: 'agent', agent_id: str(ev.agent_id) },
        text: delta,
        created_at: new Date().toISOString(),
      })
    }
    if (hasIdx) seen[id] = idx
    return { messages: list, indexByID: seen, applied: true }
  }

  // 任务结果卡：只认 bridge 反规范化进 refs 的快照；task 参数可选（任务板加载后可用）。
  const TASK_STATUS_VIEW = {
    queued: { label: '排队中（目标忙）', tone: 'muted', terminal: false },
    dispatched: { label: '已派发，等待开始', tone: 'info', terminal: false },
    running: { label: '进行中', tone: 'info', terminal: false },
    completed: { label: '已完成', tone: 'success', terminal: true },
    delivered: { label: '已回推', tone: 'success', terminal: true },
    failed: { label: '失败', tone: 'error', terminal: true },
    interrupted: { label: '被中断（人/重启/超时）', tone: 'warn', terminal: true },
    canceled: { label: '已取消', tone: 'muted', terminal: true },
    declined: { label: '已被拒绝', tone: 'warn', terminal: true },
    blocked_budget: { label: '预算/护栏阻断', tone: 'warn', terminal: true },
  }

  function taskStatusView(status) {
    const key = str(status)
    if (TASK_STATUS_VIEW[key]) return Object.assign({}, TASK_STATUS_VIEW[key])
    if (!key) return { label: '未知状态', tone: 'muted', terminal: false }
    return { label: key, tone: 'muted', terminal: false }
  }

  function usageFrom(v) {
    if (!v || typeof v !== 'object') return null
    const usage = {
      input: num(v.input) || undefined,
      output: num(v.output) || undefined,
      cost_usd: Number.isFinite(Number(v.cost_usd)) ? Number(v.cost_usd) : undefined,
      cost_priced: v.cost_priced === true,
    }
    if (!usage.input && !usage.output && usage.cost_usd === undefined) return null
    return usage
  }

  function artifactsFrom(v) {
    if (!Array.isArray(v)) return []
    const out = []
    for (const item of v) {
      if (!item || typeof item !== 'object') continue
      const name = str(item.name) || str(item.path).split('/').pop()
      if (!name) continue
      out.push({
        name,
        path: str(item.path),
        kind: str(item.kind) || 'artifact',
        size: num(item.size) || 0,
      })
    }
    return out
  }

  function durationText(ms) {
    const v = Math.max(0, Math.round(num(ms)))
    if (!v) return ''
    if (v < 1000) return v + 'ms'
    if (v < 60000) return (v / 1000).toFixed(1) + 's'
    const min = Math.floor(v / 60000)
    const sec = Math.round((v % 60000) / 1000)
    return min + ' 分 ' + String(sec).padStart(2, '0') + ' 秒'
  }

  function shortTokens(n) {
    const v = Math.max(0, Math.round(num(n)))
    if (v >= 1000000) return (v / 1000000).toFixed(1) + 'M'
    if (v >= 1000) return (v / 1000).toFixed(v >= 10000 ? 0 : 1) + 'k'
    return String(v)
  }

  function usageText(usage) {
    if (!usage) return ''
    const parts = []
    if (usage.input || usage.output) parts.push(shortTokens(usage.input) + ' → ' + shortTokens(usage.output) + ' tok')
    if (usage.cost_usd !== undefined && usage.cost_priced) {
      parts.push('$' + (usage.cost_usd < 0.01 ? usage.cost_usd.toFixed(4) : usage.cost_usd.toFixed(2)))
    } else if (usage.cost_usd !== undefined) {
      parts.push('未计价')
    }
    return parts.join(' · ')
  }

  function artifactText(art) {
    const size = art.size ? (art.size >= 1024 ? (art.size / 1024).toFixed(1) + 'KB' : art.size + 'B') : ''
    return [art.kind || 'artifact', art.path || art.name, size].filter(Boolean).join(' · ')
  }

  function taskCardView(msg, task) {
    if (!msg || msg.kind !== 'task_result') return null
    const refs = msg.refs && typeof msg.refs === 'object' ? msg.refs : {}
    const status = str(refs.status) || 'unknown'
    const sv = taskStatusView(status)
    const body = str(msg.text)
    const error = str(refs.error) || str(task && task.error) || str(task && task.result && task.result.error)
    const summary = str(refs.summary) || str(task && task.result && task.result.summary) || body
    const title = str(refs.title) || str(task && task.title) || '任务结果'
    const agentId = str(refs.agent_id) || str(task && task.target && task.target.agent_id)
    const originKind = str(refs.origin_kind) || str(task && task.origin && task.origin.kind)
    const originAgentId = str(refs.origin_agent_id) || str(task && task.origin && task.origin.agent_id)
    const originAgentName = str(refs.origin_agent_name) || str(task && task.origin && task.origin.agent_name)
    const usage = usageFrom(refs.usage)
    return {
      taskId: str(refs.task_id) || str(task && task.id),
      status,
      statusLabel: sv.label,
      tone: sv.tone,
      terminal: sv.terminal,
      title,
      summary,
      error,
      agentId,
      initiatorAgentId: originKind && originKind !== 'human' ? originAgentId : '',
      initiatorName: originKind && originKind !== 'human' ? originAgentName : '',
      durationMs: Math.max(0, num(refs.duration_ms)),
      durationLabel: durationText(refs.duration_ms),
      usage,
      usageLabel: usageText(usage),
      artifacts: artifactsFrom(refs.artifacts).map((a) => Object.assign({}, a, { label: artifactText(a) })),
    }
  }

  // hub_task_update 到达时把当前时间线上的同任务卡状态刷新（历史正文不动）。
  function applyTaskUpdate(messages, task) {
    if (!task || !str(task.id) || !Array.isArray(messages)) return messages
    let changed = false
    const out = messages.map((m) => {
      if (!m || m.kind !== 'task_result' || !m.refs) return m
      if (str(m.refs.task_id) !== str(task.id)) return m
      const refs = Object.assign({}, m.refs, {
        status: str(task.status) || str(m.refs.status),
        error: str(task.error) || str(m.refs.error),
      })
      if (task.result && task.result.summary) refs.summary = str(task.result.summary)
      changed = true
      return Object.assign({}, m, { refs })
    })
    return changed ? out : messages
  }

  // ---------- @ 分词/插入 ----------
  // 与桌面 mentions.ts 同一口径的简化版：跳过 fenced/inline code、URL、email；
  // 最长成员名优先；边界只看 ASCII 字母数字/下划线（中文「请@Alice」照常生效）。
  // 不做 Markdown 链接结构扫描（手机端正文按纯文本渲染，没有嵌套链接问题）。

  function maskMentionSkipped(text) {
    const s = String(text || '')
    const masked = new Array(s.length).fill(false)

    // 围栏代码块（``` / ~~~）：未闭合就遮到结尾，避免把半截代码当点名。
    for (const fence of ['```', '~~~']) {
      let from = 0
      for (;;) {
        const start = s.indexOf(fence, from)
        if (start < 0) break
        const end = s.indexOf(fence, start + fence.length)
        const stop = end < 0 ? s.length : end + fence.length
        for (let i = start; i < stop; i++) masked[i] = true
        from = stop
      }
    }

    // 行内代码（未被围栏遮住的部分）。
    for (let i = 0; i < s.length; i++) {
      if (masked[i] || s[i] !== '`') continue
      let end = i + 1
      while (end < s.length && (masked[end] || s[end] !== '`')) end++
      if (end >= s.length) break
      for (let k = i; k <= end; k++) if (!masked[k]) masked[k] = true
      i = end
    }

    // URL：`https://example.com/@Alice` 不是点名。
    const urlRe = /https?:\/\/[^\s<>"'`]+/gi
    for (const m of s.matchAll(urlRe)) {
      if (masked[m.index]) continue
      for (let i = m.index; i < m.index + m[0].length; i++) masked[i] = true
    }

    // email：`alice@example.com` 里的 @ 不是点名。
    const mailRe = /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g
    for (const m of s.matchAll(mailRe)) {
      if (masked[m.index]) continue
      for (let i = m.index; i < m.index + m[0].length; i++) masked[i] = true
    }
    return masked
  }

  function boundaryBefore(text, i) {
    if (i <= 0) return true
    const c = text[i - 1]
    if (c === '\\' || c === '/' || c === '@') return false
    return !/[A-Za-z0-9_-]/.test(c)
  }

  function boundaryAfter(text, at) {
    if (at >= text.length) return true
    return !/[A-Za-z0-9_-]/.test(text[at])
  }

  function sortedMembers(members) {
    return (members || [])
      .filter((m) => m && str(m.id) && str(m.name))
      .slice()
      .sort((a, b) => str(b.name).length - str(a.name).length)
  }

  function matchMentionAt(text, i, members) {
    if (text[i] !== '@' && text[i] !== '＠') return null
    if (!boundaryBefore(text, i)) return null
    const rest = text.slice(i + 1)
    // @all / @所有人：只在边界成立时算点名。
    if (/^all(?![A-Za-z0-9_-])/i.test(rest)) {
      return { id: ALL_MENTION_ID, name: 'all', length: 3 }
    }
    if (rest.slice(0, 3) === '所有人' && boundaryAfter(rest, 3)) {
      return { id: ALL_MENTION_ID, name: '所有人', length: 3 }
    }
    for (const m of members) {
      const name = str(m.name)
      if (!rest.toLowerCase().startsWith(name.toLowerCase())) continue
      if (!boundaryAfter(rest, name.length)) continue
      return { id: str(m.id), name, length: name.length }
    }
    return null
  }

  function tokenizeMentions(text, members) {
    const s = String(text || '')
    const masked = maskMentionSkipped(s)
    const list = sortedMembers(members)
    const out = []
    let buf = ''
    for (let i = 0; i < s.length; ) {
      const hit = !masked[i] ? matchMentionAt(s, i, list) : null
      if (hit) {
        if (buf) {
          out.push({ kind: 'text', text: buf })
          buf = ''
        }
        out.push({
          kind: 'mention',
          text: s.slice(i, i + 1 + hit.length),
          agentId: hit.id,
          name: hit.name,
        })
        i += 1 + hit.length
        continue
      }
      buf += s[i]
      i++
    }
    if (buf) out.push({ kind: 'text', text: buf })
    return out
  }

  // 正文里命中的成员 id（去重、保持出现顺序；@all 不进结构化 mentions）。
  function mentionIds(text, members) {
    const seen = new Set()
    const deleted = new Set((members || []).filter((m) => m && m.deleted).map((m) => str(m.id)))
    const out = []
    for (const t of tokenizeMentions(text, members)) {
      if (t.kind !== 'mention' || t.agentId === ALL_MENTION_ID) continue
      if (deleted.has(t.agentId)) continue // 已删除成员不能再被点名（历史消息仍可高亮）
      if (seen.has(t.agentId)) continue
      seen.add(t.agentId)
      out.push(t.agentId)
    }
    return out
  }

  function hasMentionAll(text) {
    for (const t of tokenizeMentions(text, [])) {
      if (t.kind === 'mention' && t.agentId === ALL_MENTION_ID) return true
    }
    return false
  }

  // ---------- 定稿正文：安全 markdown 计划 ----------
  // 只产出「节点描述」，不产出 HTML 字符串：页面用 createElement/textContent 渲染，
  // 因此模型正文里的 <img onerror> / <script> 永远只是文本。
  // 规则与聊天 mdLite 保持一致：``` 围栏（首行语言标注丢掉）、`行内代码`、**粗体**。
  function markdownPlan(text) {
    const out = []
    String(text == null ? '' : text)
      .split('```')
      .forEach((seg, i) => {
        if (i % 2 === 1) {
          const body = seg.replace(/^[^\n]*\n/, '').replace(/\n+$/, '')
          out.push({ kind: 'codeblock', text: body })
          return
        }
        for (const part of seg.split(/(`[^`\n]+`|\*\*[^*\n]+\*\*)/)) {
          if (!part) continue
          if (part.length > 2 && part.startsWith('`') && part.endsWith('`')) {
            out.push({ kind: 'code', text: part.slice(1, -1) })
          } else if (part.length > 4 && part.startsWith('**') && part.endsWith('**')) {
            out.push({ kind: 'bold', text: part.slice(2, -2) })
          } else {
            out.push({ kind: 'text', text: part })
          }
        }
      })
    return out
  }

  // 定稿 Hub 正文 = markdown 计划 + @ 高亮：只在普通文本段里再切 mention，
  // 围栏/行内代码里的 @ 保持代码原样（不误标成员）。
  function bodyParts(text, members) {
    const out = []
    for (const part of markdownPlan(text)) {
      if (part.kind !== 'text') {
        out.push(part)
        continue
      }
      for (const t of tokenizeMentions(part.text, members)) {
        out.push(t.kind === 'mention' ? { kind: 'mention', text: t.text, agentId: t.agentId, name: t.name } : { kind: 'text', text: t.text })
      }
    }
    return out
  }

  const MENTION_QUERY_STOP = /[\s，。！？、；：,.!?;:]/

  function activeMentionQuery(text, caret) {
    const s = String(text || '')
    const at = Math.max(0, Math.min(num(caret), s.length))
    for (let i = at - 1; i >= 0; i--) {
      const ch = s[i]
      if (ch === '@' || ch === '＠') {
        if (i > 0 && (boundaryBefore(s, i) === false)) return null
        const query = s.slice(i + 1, at)
        if (MENTION_QUERY_STOP.test(query)) return null
        return { query, start: i }
      }
      if (/\s/.test(ch)) return null
    }
    return null
  }

  // 键入 @ 的触发判断。IME 组词中（isComposing/compositionstart）不弹选择器：
  // 中文输入法把「＠」当候选上屏前，query 是半截拼音，弹了只会打断组词。
  function mentionTypingQuery(text, caret, opts) {
    if (opts && opts.composing) return null
    return activeMentionQuery(text, caret)
  }

  // 选中成员后插入 `@Name `；光标前若是「@Ali」半截 token 就整体替换。
  function applyMention(text, caret, name) {
    const s = String(text || '')
    const at = Math.max(0, Math.min(num(caret), s.length))
    const q = activeMentionQuery(s, at)
    const start = q ? q.start : at
    const next = s[at]
    const suffix = next && /\s/.test(next) ? '' : ' '
    const insert = '@' + String(name || '') + suffix
    return {
      text: s.slice(0, start) + insert + s.slice(at),
      caret: start + 1 + String(name || '').length + 1,
    }
  }

  function filterMentionCandidates(query, members, opts) {
    const q = str(query).toLowerCase()
    const includeAll = !!(opts && opts.includeAll)
    const weight = { owner: 0, member: 1, observer: 2 }
    const list = sortedMembers(members)
      .filter((m) => m.deleted !== true)
      .sort((a, b) => {
        const wa = weight[a.groupRole] === undefined ? 1 : weight[a.groupRole]
        const wb = weight[b.groupRole] === undefined ? 1 : weight[b.groupRole]
        if (wa !== wb) return wa - wb
        return str(a.name).localeCompare(str(b.name), 'zh-Hans-CN')
      })
    const matched = !q
      ? list
      : list.filter((m) => [m.name, m.id, m.role, m.groupRole].some((v) => str(v).toLowerCase().indexOf(q) >= 0))
    if (!includeAll) return matched
    const all = { id: ALL_MENTION_ID, name: '所有人', role: '', groupRole: 'all' }
    const showAll = !q || 'all'.indexOf(q) >= 0 || '所有人'.indexOf(q) >= 0
    return showAll ? [all].concat(matched) : matched
  }

  function groupRoster(group, agents) {
    const byID = new Map((agents || []).map((a) => [str(a.id), a]))
    return (group && group.members ? group.members : []).map((m) => {
      const a = byID.get(str(m.agent_id)) || {}
      return {
        id: str(m.agent_id),
        name: str(a.name) || str(m.agent_id),
        role: str(a.role),
        groupRole: str(m.role),
        avatar: a.avatar || null,
        accent: str(a.accent),
        deleted: a.deleted === true,
      }
    })
  }

  // observer 只旁听，不能接派任务；已删除成员不列。
  function assignableMembers(roster) {
    return (roster || []).filter((m) => !m.deleted && m.groupRole !== 'observer')
  }

  // 发送 payload：正文里的 @Name 仍由后端解析一次，这里显式带上 agent_id，
  // 让「点选过谁」不依赖名字二次解析（与桌面同口径）。
  function groupSendPayload(groupId, text, members, clientMsgId) {
    const body = String(text || '')
    const payload = { group_id: str(groupId), text: body }
    const ids = mentionIds(body, members)
    if (ids.length) payload.mentions = ids
    if (hasMentionAll(body)) payload.mention_all = true
    if (str(clientMsgId)) payload.client_msg_id = str(clientMsgId)
    return payload
  }

  function dmSendPayload(agentId, text, clientMsgId) {
    const payload = {
      target: { kind: 'dm', agent_id: str(agentId) },
      text: String(text || ''),
    }
    if (str(clientMsgId)) payload.client_msg_id = str(clientMsgId)
    return payload
  }

  function assignPayload(groupId, agentId, instruction, title, clientMsgId) {
    const payload = {
      group_id: str(groupId),
      agent_id: str(agentId),
      instruction: String(instruction || ''),
    }
    if (str(title)) payload.title = str(title)
    if (str(clientMsgId)) payload.client_msg_id = str(clientMsgId)
    return payload
  }

  // ---------- 发送失败：草稿不丢 ----------
  // 请求成功才清空；发送期间用户新敲的内容不能被回执覆盖。
  // 失败时若输入框已被别处清空（例如系统回收），把原文恢复回来。
  function draftAfterSend(current, sent) {
    const cur = String(current == null ? '' : current)
    const body = String(sent || '')
    return cur.trim() === body.trim() ? '' : cur
  }

  function draftAfterFailure(current, sent) {
    const cur = String(current == null ? '' : current)
    return cur.trim() ? cur : String(sent || '')
  }

  // ---------- 发送幂等：失败草稿的重试必须复用 client_msg_id ----------
  // 手动「重试」是同一个失败草稿的第二次尝试，不是一条新消息：首次请求可能已经
  // 落盘、只是响应/网络断了，换新 cid 会让服务端写成两条。所以：
  //   - 同一 (草稿正文, 目标) 的重试沿用首次 cid；
  //   - 成功之后清空（下一次发送是新消息）；
  //   - 用户改了草稿或换了目标 → 内容/去向已经不同，换新 cid（见 sendSlotAfterEdit）。
  // slot = { text, target, cid } | null。
  function sendSlotID(slot, text, target, makeID) {
    const key = String(text == null ? '' : text).trim()
    const scope = str(target)
    const cid = str(slot && slot.cid)
    if (cid && slot.text === key && str(slot.target) === scope) {
      return { text: key, target: scope, cid }
    }
    const made = typeof makeID === 'function' ? makeID() : makeID
    return { text: key, target: scope, cid: str(made) }
  }

  // 草稿被用户改动（键入/插入 @）→ 已经不是那条失败消息：丢掉复用的 cid。
  function sendSlotAfterEdit(slot, text) {
    if (!slot) return null
    return String(text == null ? '' : text).trim() === slot.text ? slot : null
  }

  // 同 cid 的消息已经落盘（首次请求其实成功了）：不要再插乐观气泡，
  // 否则 seq=0 的本地对象会把权威回执盖成「发送中」。
  function hasLandedClientMsg(messages, clientMsgId) {
    const cid = str(clientMsgId)
    if (!cid) return false
    return (Array.isArray(messages) ? messages : []).some((m) => m && clientMsgID(m) === cid && num(m.seq) > 0)
  }

  // ---------- 已读游标 ----------
  // 只有「贴底 + 页面可见」才算看到最新：往上读历史时不能把未读清掉。
  const READ_BOTTOM_PX = 48

  function shouldMarkRead(metrics, visible) {
    if (!visible || !metrics) return false
    const top = num(metrics.scrollTop)
    const height = num(metrics.scrollHeight)
    const client = num(metrics.clientHeight)
    if (height <= 0 || client <= 0) return false
    return height - top - client <= READ_BOTTOM_PX
  }

  // ---------- 告警 ----------
  // 存储文本必须和去重键同源：code/message 都为空时统一成「Hub 告警」，
  // 否则每来一条空告警都会重复堆在首页。
  function hubAlertText(ev) {
    return str(ev && ev.message) || str(ev && ev.code) || 'Hub 告警'
  }

  function pushHubAlert(alerts, ev) {
    const text = hubAlertText(ev)
    const severity = str(ev && ev.severity) || 'warning'
    const rest = (Array.isArray(alerts) ? alerts : []).filter((a) => a && a.text !== text)
    return [{ text, severity }].concat(rest).slice(0, 5)
  }

  // ---------- 列表/系统行 ----------

  // 群左栏/私聊预览：任务结论优先，其次最后一条正文。
  function hubPreview(progress) {
    if (!progress) return ''
    return str(progress.last_outcome) || str(progress.last_text) || str(progress.last_task_summary) || ''
  }

  // routing_receipt 的落盘文本是 targets=ag_x；人类 UI 应该画成人话。
  function routingSummary(text, nameById) {
    const raw = String(text || '')
    const m = /^(@all\s+)?targets=([^ ]*)(?:\s+dropped=(\d+))?$/.exec(raw.trim())
    if (!m) return raw
    const names = (m[2] ? m[2].split(',') : []).filter(Boolean).map((id) => {
      return (nameById && nameById[id]) || id
    })
    const dropped = num(m[3])
    return (m[1] ? '@all ' : '') +
      (names.length ? '唤醒 ' + names.join('、') : '无人被唤醒') +
      (dropped ? '（丢弃 ' + dropped + ' 个）' : '')
  }

  function parseNeedQuestions(need) {
    if (!need || need.kind !== 'question' || !str(need.body)) return []
    try {
      const raw = JSON.parse(need.body)
      if (!Array.isArray(raw)) return []
      return raw
        .filter((q) => q && typeof q === 'object')
        .map((q, i) => ({
          id: str(q.id) || String(i),
          question: str(q.question) || str(q.text) || '问题 ' + (i + 1),
        }))
    } catch {
      return []
    }
  }

  return {
    ALL_MENTION_ID,
    THEME_MODES,
    normalizeThemeMode,
    resolveTheme,
    themeColor,
    isHubEvent,
    eventRoute,
    hubResyncTargets,
    clientMsgID,
    mergeMessages,
    optimisticMessage,
    messageFromEvent,
    applyDelta,
    taskStatusView,
    taskCardView,
    applyTaskUpdate,
    maskMentionSkipped,
    tokenizeMentions,
    mentionIds,
    hasMentionAll,
    markdownPlan,
    bodyParts,
    activeMentionQuery,
    mentionTypingQuery,
    applyMention,
    filterMentionCandidates,
    groupRoster,
    assignableMembers,
    groupSendPayload,
    dmSendPayload,
    assignPayload,
    draftAfterSend,
    draftAfterFailure,
    sendSlotID,
    sendSlotAfterEdit,
    hasLandedClientMsg,
    shouldMarkRead,
    hubAlertText,
    pushHubAlert,
    hubPreview,
    routingSummary,
    parseNeedQuestions,
    durationText,
    usageText,
    artifactText,
  }
})
