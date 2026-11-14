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
//   3. 数据**只**通过 `host.command()` 取（白名单在 manifest.commands 里声明，
//      真正放行在 bridge；未声明会拿到 {ok:false, reason:'denied'}，不会抛）。
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

    // —— 总览：宿主能力的最小用法（host.command + host.events）——
    function Overview() {
      const theme = host.theme()
      const [workspaces, setWorkspaces] = useState([])
      const [error, setError] = useState('')
      const [tick, setTick] = useState(0)

      useEffect(() => {
        let alive = true
        host
          .command('workspace_list', {})
          .then((r) => {
            if (!alive) return
            // 拒绝是一个**值**（ok:false + reason），不是异常 —— 照实显示，别吞掉。
            if (!r.ok) setError(r.reason || 'denied')
            else setWorkspaces(r.data?.workspaces || [])
          })
        return () => {
          alive = false
        }
      }, [tick])

      // 已声明的 op 才能订阅（manifest.data）。这里用它做「别的地方变了 → 我也刷新」。
      useEffect(() => host.events.on('surfaces_changed', () => setTick((n) => n + 1)), [])

      const style = { padding: 12, color: theme.tokens.fg, font: '13px/1.6 system-ui, sans-serif' }
      return h(
        'div',
        { style, 'data-surface': 'cost-board', 'data-entry': 'overview' },
        h('div', { style: h2Style(theme) }, '成本看板'),
        error
          ? h('div', { style: { color: theme.tokens.err } }, '读不到工作区：' + error)
          : h('div', null, `工作区 ${workspaces.length} 个`),
        h(
          'ul',
          { style: { margin: '8px 0 0', paddingLeft: 18 } },
          workspaces.slice(0, 8).map((w, i) => h('li', { key: w.path || i }, w.title || w.name || w.path)),
        ),
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
              ['时间', ...SLOTS].map((d) => h('th', { key: d, style: { ...cell, color: theme.tokens.muted || theme.tokens.fg } }, d)),
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
                  h('td', { key: `${row[0]}-${i}`, style: { ...cell, color: i === 0 ? theme.tokens.muted || theme.tokens.fg : theme.tokens.fg } }, v),
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
            h('li', { key: r.name }, h('span', { style: { color: theme.tokens.muted || theme.tokens.fg } }, `${r.kind} · `), r.name),
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
