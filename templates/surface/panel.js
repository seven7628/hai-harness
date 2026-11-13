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
export const surface = {
  kind: 'plugin:cost-board',

  apply({ host, React, surfaces }) {
    const { createElement: h, useEffect, useState } = React

    function Board() {
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

      const style = {
        padding: 12,
        color: theme.tokens.fg,
        font: '13px/1.6 system-ui, sans-serif',
      }
      return h(
        'div',
        { style, 'data-surface': 'cost-board' },
        h('div', { style: { color: theme.tokens.accent, fontWeight: 600 } }, '成本看板'),
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

    surfaces.register({
      kind: 'plugin:cost-board',
      group: 'plugin', // 只装在 plugin 组：workflow 组是内置能力（链路/变更）
      order: 10,
      title: () => '成本看板',
      render: () => h(Board),
    })
  },
}
