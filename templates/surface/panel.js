// templates/surface/panel.js —— 一个 Surface 面板的最小样本（**无构建步骤**）。
//
// 这段代码就是浏览器直接 `import()` 的那一份，所以：
//   · **不写 JSX**（面板不经过 vite，没有编译器）→ 一切用 React.createElement；
//   · **不 import 任何包**（`import x from 'some-lib'` 在浏览器里没有解析器，会直接失败）
//     → React 由宿主传入（`apply({ React })`）；
//   · 单文件 ≤ 256 KiB、整目录 ≤ 1 MiB（安装器会拒，见 01-design §5.5）。
//
// 三件事决定了面板「长得像这个应用」：
//   1. 颜色/主题**只**从 `host.theme()` 取（不要写死 #fff —— 暗色下会瞎）；
//   2. 标题/图标走 provider（标签条只认 provider.title / provider.icon）；
//   3. 一切宿主能力**只**经 `host.command(name, payload)`（白名单在 manifest.commands 里
//      声明；未声明拿到 {ok:false, reason:'not_declared'}，**永不抛** —— 拒绝是一个值）；
//      事件**只**经 `host.onEvent(type, fn)`（未在 manifest.data 声明会抛，所以写错名字
//      在 apply 阶段就能发现）。这两个就是插件的全部口子（09-agent-bridge §5）。
//
// D11（旧只读命令已不可用）：`workspace_list` / `session_list` / `git_snapshot` /
// `ui_inspect` 既不是 command、也不再放行 —— 它们能读真实项目/会话元数据，与「插件不碰
// Project」冲突。声明了它们也不会生效（一律 denied），所以别照着旧例子抄。
//
// R16 起这份模板演示**多入口**：surface.json 的 entries 声明了三个入口（总览/课表/资源），
// 宿主把它们画在右栏 ＋ → 面板插件、空态能力面板、以及设置页的「打开」上；点哪一个都用
// `openTab({ kind, params: { entry: '<id>' } })` 打开一个**独立标签**，本文件按
// `tab.params.entry` 决定画哪一页。插件**不做**菜单、不做路由条 —— 入口是 manifest 的事（D-2）。
export const surface = {
  kind: 'plugin:cost-board',

  apply({ host, React, surfaces }) {
    const { createElement: h, useEffect, useState } = React

    // entry id → 标题。**必须与 surface.json 的 entries[].id 一一对齐**：
    // 宿主用 `params.entry` 打开标签，这里用它选页面；对不上时退回总览（宁可画错一页，
    // 也不要空白 —— 空白会被当成「插件坏了」）。
    const TITLES = { overview: '总览', schedule: '课表', resources: '资源' }

    // P2：manifest.tools[] 声明的工具由模型发起，插件在 tool.call 里执行并回 tool.result。
    // 这个样本是只读回显（read_only:true，不弹审批）；写工具必须 read_only:false，宿主会强制
    // 审批（Auto/Full Access 也一样），插件不能代点。requires_panel:true 时宿主在面板没开时
    // 直接回 panel_not_open，本回调不会被调用。
    host.onEvent('tool.call', (ev) => {
      const d = ev.data || {}
      const text = d.args && typeof d.args.text === 'string' ? d.args.text : ''
      host.command('tool.result', { call_id: d.call_id, ok: true, result: { echo: text } })
    })

    // 没有 params（老标签 / 宿主直接按 kind 打开）也落回总览：默认入口的语义由插件自己兜。
    const entryOf = (tab) => {
      const id = tab && tab.params ? tab.params.entry : undefined
      return typeof id === 'string' && TITLES[id] ? id : 'overview'
    }

    const pageStyle = (theme) => ({
      padding: 12,
      color: theme.tokens.fg,
      font: '13px/1.6 system-ui, sans-serif',
    })
    const h2Style = (theme) => ({ color: theme.tokens.accent, fontWeight: 600, margin: '0 0 6px' })

    // —— 总览：新 API 的最小用法（host.command + host.onEvent + composer.quote）——
    //
    // 这一页演示三件事，每一件都对应一个「用错了就会出事」的点：
    //   ① agent.run 的**回执只含 run_id/session_id**，内容靠事件流（agent.delta …）；
    //      所以必须先在 manifest.data 里声明要收的事件。
    //   ② onEvent 订阅由宿主在插件卸载/重载时统一回收；off() 只用于提前退订。组件里的
    //      useEffect cleanup 适合「只跟这个面板同长」的订阅，工具监听通常放在 apply 作用域。
    //   ③ composer.quote **只插草稿、绝不自动发送**（D7）：发不发由用户按回车决定。
    function Overview() {
      const theme = host.theme()
      const [question, setQuestion] = useState('把这周的课程安排整理成一张表')
      const [answer, setAnswer] = useState('')
      const [status, setStatus] = useState('')
      const [busy, setBusy] = useState(false)
      const sessionRef = React.useRef('') // 插件自己记会话 id（D4：宿主不替你记）
      const runRef = React.useRef('')

      // 每个 onEvent 都返回 disposer；effect 的 cleanup 里逐个调用。
      // 写成 `const off = host.onEvent(...)` 而不是 `.on(...)`：没有 host.events 这个对象了。
      useEffect(() => {
        const offDelta = host.onEvent('agent.delta', (ev) => setAnswer((prev) => prev + ((ev.data && ev.data.text) || '')))
        const offDone = host.onEvent('agent.done', () => {
          setBusy(false)
          setStatus('完成')
        })
        const offError = host.onEvent('agent.error', (ev) => {
          setBusy(false)
          setStatus('出错：' + ((ev.data && ev.data.reason) || 'error'))
        })
        // 本地事件（不需要 token）：插件装卸/启停时宿主会推它。这里只演示「收到就算数」。
        const offChanged = host.onEvent('surfaces_changed', () => setStatus('插件清单有变化'))
        return () => {
          offDelta()
          offDone()
          offError()
          offChanged()
        }
      }, [])

      const ask = async () => {
        setBusy(true)
        setAnswer('')
        setStatus('')
        // ① 先要一个会话（D4：不传 session_id 给 agent.run 也是新建，这里演示显式建）
        if (!sessionRef.current) {
          const s = await host.command('agent.session', { title: '成本看板' })
          if (!s.ok) {
            setBusy(false)
            setStatus('建会话失败：' + (s.reason || 'denied'))
            return
          }
          // 回执把 data 里的字段摊在顶层（loader 归一），两种读法都对，这里用扁平的那个。
          sessionRef.current = s.session_id || (s.data && s.data.session_id) || ''
        }
        // ② 发起 run：**立即**回 run_id，内容随后从事件流里来
        const r = await host.command('agent.run', { session_id: sessionRef.current, input: question })
        if (!r.ok) {
          setBusy(false)
          setStatus('发起失败：' + (r.reason || 'denied'))
          return
        }
        runRef.current = r.run_id || (r.data && r.data.run_id) || ''
      }

      const cancel = async () => {
        const r = await host.command('agent.cancel', { run_id: runRef.current })
        if (r.ok) await host.command('ui.toast', { message: '已请求停止' })
        setStatus(r.ok ? '已请求停止' : '停止失败：' + (r.reason || 'denied'))
      }

      const quote = async () => {
        // 只插草稿（D7）：回执 ok 之后输入框里就有这段字，光标在末尾，发不发由用户决定。
        const r = await host.command('composer.quote', {
          text: question,
          source: { id: 'cost-board:question', title: '成本看板 · 问题' },
        })
        setStatus(r.ok ? '已插入草稿（不会自动发送）' : '引用失败：' + (r.reason || 'denied'))
      }

      const btn = {
        padding: '4px 10px',
        marginRight: 8,
        borderRadius: 6,
        border: `1px solid ${theme.tokens.border}`,
        background: 'transparent',
        color: theme.tokens.fg,
        cursor: 'pointer',
        font: 'inherit',
      }
      return h(
        'div',
        { style: pageStyle(theme), 'data-surface': 'cost-board', 'data-entry': 'overview' },
        h('div', { style: h2Style(theme) }, '成本看板'),
        h(
          'div',
          { style: { color: theme.tokens.fg, marginBottom: 8 } },
          '问一句，结果流式回到这里；引用只进草稿，不会自动发送。',
        ),
        h('textarea', {
          value: question,
          onChange: (e) => setQuestion(e.target.value),
          rows: 3,
          'data-template-question': '',
          style: { width: '100%', boxSizing: 'border-box', padding: 6, color: theme.tokens.fg, background: 'transparent', border: `1px solid ${theme.tokens.border}`, borderRadius: 6, font: 'inherit' },
        }),
        h(
          'div',
          { style: { margin: '8px 0' } },
          h('button', { type: 'button', style: btn, onClick: ask, disabled: busy, 'data-template-ask': '' }, busy ? '运行中…' : '问 Agent'),
          h('button', { type: 'button', style: btn, onClick: quote, 'data-template-quote': '' }, '引用到草稿'),
          busy ? h('button', { type: 'button', style: btn, onClick: cancel }, '停止') : null,
        ),
        // 拒绝是一个**值**（ok:false + reason），不是异常 —— 照实显示，别吞掉。
        status ? h('div', { style: { color: theme.tokens.fg }, 'data-template-status': '' }, status) : null,
        answer
          ? h('pre', { 'data-template-answer': '', style: { whiteSpace: 'pre-wrap', margin: '8px 0 0', font: 'inherit' } }, answer)
          : null,
      )
    }

    // —— 课表：演示「同一个 provider 的第二页」。数据写死（D-1：存储归插件，
    //     真插件该用自己的 IndexedDB / 后端；模板不替它决定存储形态）。——
    const SLOTS = ['周一', '周二', '周三', '周四', '周五']
    const COURSES = [
      ['09:00', '高等数学', '线性代数', '高等数学', '概率论', '—'],
      ['11:00', '数据结构', '数据结构', '—', '数据结构', '算法实验'],
      ['14:00', '—', '编译原理', '编译原理', '—', '编译原理'],
    ]

    function Schedule() {
      const theme = host.theme()
      const cell = { padding: '4px 8px', borderBottom: `1px solid ${theme.tokens.border}`, textAlign: 'left' }
      return h(
        'div',
        { style: pageStyle(theme), 'data-surface': 'cost-board', 'data-entry': 'schedule' },
        h('div', { style: h2Style(theme) }, '本学期课表'),
        h(
          'table',
          { style: { borderCollapse: 'collapse', width: '100%', fontSize: 12 } },
          h(
            'thead',
            null,
            h(
              'tr',
              { key: 'head' },
              ['时间', ...SLOTS].map((d) => h('th', { key: d, style: { ...cell, color: theme.tokens.fg } }, d)),
            ),
          ),
          h(
            'tbody',
            null,
            COURSES.map((row) =>
              h(
                'tr',
                { key: row[0] },
                row.map((v, i) =>
                  h('td', { key: `${row[0]}-${i}`, style: { ...cell, color: theme.tokens.fg, fontWeight: i === 0 ? 600 : 400 } }, v),
                ),
              ),
            ),
          ),
        ),
      )
    }

    // —— 资源：第三页。同一份面板里换的是内容，不是 kind —— 一个插件只有一个 kind（D-2）。——
    const RESOURCES = [
      { name: '课程大纲.pdf', kind: 'pdf' },
      { name: '第 3 讲 幻灯片.pptx', kind: 'pptx' },
      { name: '习题集.docx', kind: 'docx' },
      { name: '实验数据.xlsx', kind: 'xlsx' },
    ]

    function Resources() {
      const theme = host.theme()
      return h(
        'div',
        { style: pageStyle(theme), 'data-surface': 'cost-board', 'data-entry': 'resources' },
        h('div', { style: h2Style(theme) }, '课程资源'),
        h(
          'ul',
          { style: { margin: '8px 0 0', paddingLeft: 18 } },
          RESOURCES.map((r) =>
            h('li', { key: r.name }, h('span', { style: { color: theme.tokens.fg } }, `${r.kind} · `), r.name),
          ),
        ),
      )
    }

    // 分发：**一个** render 入口按 tab.params.entry 选页 —— 不注册第二个 provider，
    // 也不让宿主知道页面内部结构（D-2：页面内部路由归插件）。
    function Panel({ tab }) {
      const entry = entryOf(tab)
      if (entry === 'schedule') return h(Schedule)
      if (entry === 'resources') return h(Resources)
      return h(Overview)
    }

    surfaces.register({
      kind: 'plugin:cost-board',
      group: 'plugin', // 只装在 plugin 组：workflow 组是内置能力（链路/变更）
      order: 10,
      // 标题跟着入口走：宿主对**已声明**的入口会用 manifest 里的 title（菜单与标签同源），
      // 这里给的是「宿主没声明时」的兜底（老标签 / 直接按 kind 打开）。
      title: (tab) => TITLES[entryOf(tab)],
      render: ({ tab }) => h(Panel, { tab }),
    })
  },
}
