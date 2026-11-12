#!/usr/bin/env node
// PR-02 不变量：activeId 权威、rightTab 单点派生。
//
// 这三条是后续所有 Surface 迁移的地基（PR-03/04/06/07 都假设它们成立）：
//   1) 任何标签动作之后，rightTab === 激活标签的 kind（没有标签时保留旧值）
//   2) 外部直接 setState({ rightTab }) **不再**换掉用户正在看的标签（旧的反向收敛已删）
//   3) 标签动作不碰布局（rightLayout 不变；自动展开只属于 openTab，留给后续 PR 接线）
//
// 用法：cd desktop/app && npm run test:activeid
// 退出码：全通过 0；任一断言失败非 0。
import { build } from 'esbuild'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const eq = (actual, expected, m) => {
  if (actual !== expected) throw new Error(`断言失败: ${m}\n     期望: ${JSON.stringify(expected)}\n     实际: ${JSON.stringify(actual)}`)
  ok(`${m}（= ${JSON.stringify(actual)}）`)
}

// 1) localStorage / window.desktop 桩（必须在 require bundle 之前）
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}
global.window = {
  innerWidth: 1600,
  desktop: {
    command: async () => ({ ok: true }),
    embedStatus: async () => ({ ready: true, visible: true, attached: true, url: '', title: '', activeId: null, targets: [] }),
    embedCloseTarget: async () => ({ ok: true }),
    embedSetSession: async () => ({ ok: true }),
    embedOpenTarget: async () => ({ ok: true }),
    embedActivate: async () => ({ ok: true }),
    embedHide: async () => ({ ok: true }),
    embedShow: async () => ({ ok: true }),
    embedResize: async () => ({ ok: true }),
  },
  setTimeout: (...a) => setTimeout(...a),
  clearTimeout: (...a) => clearTimeout(...a),
}

const dir = mkdtempSync(join(tmpdir(), 'activeid-'))
const outfile = join(dir, 'store-bundle.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})
const { useAppStore } = require(outfile)

const WS = '/mock/ws'
const S1 = 's1'
const S2 = 's2'
const st = () => useAppStore.getState()
const active = () => {
  const s = st()
  const set = s.tabsByScope[s.activeSessionId ?? s.workspace]
  return set?.tabs.find((t) => t.id === set.activeId)
}
const reset = (patch = {}) => {
  useAppStore.setState({
    workspace: WS,
    activeSessionId: S1,
    views: {},
    tabsByScope: {},
    projects: [{ path: WS, sessions: [{ id: S1, title: 'A' }, { id: S2, title: 'B' }] }],
    recentSessions: [],
    rightTab: 'activity',
    rightExpanded: false,
    rightLayout: { open: false },
    leftExpanded: true,
    settings: st().settings,
    ...patch,
  })
}

const layoutBefore = () => JSON.stringify(st().rightLayout)

async function main() {
  console.log('\n—— A. 标签动作后 rightTab 恒等于激活标签的 kind ——')
  {
    reset()
    st().openTab({ kind: 'git' })
    eq(active()?.kind, 'git', 'A1 openTab(git) → 激活 git')
    eq(st().rightTab, 'git', 'A2 rightTab 派生自激活标签')

    const gitId = active().id
    st().openTab({ kind: 'web' })
    eq(st().rightTab, 'web', 'A3 再开 web → rightTab 跟着走')
    st().activateTab(gitId)
    eq(st().rightTab, 'git', 'A4 切回 git → rightTab 跟着走')

    st().closeTab(gitId)
    const next = active()
    assert(next != null, 'A5a 关掉激活标签后仍有新激活项（顺延到邻居）')
    eq(st().rightTab, next.kind, 'A5b rightTab 跟着新激活项')
  }

  console.log('\n—— B. 外部改 rightTab 不再换掉用户正在看的标签（旧反向收敛已删）——')
  {
    reset()
    st().openTab({ kind: 'web' })   // 集合里**确实存在** web 标签：旧实现一定会切过去
    const webId = active().id
    st().openTab({ kind: 'git' })
    const gitId = active().id
    assert(gitId !== webId, 'B0 两个标签实例不同')
    useAppStore.setState({ rightTab: 'web' }) // 模拟旧 e2e / 旧持久化数据直接改派生字段
    // 关键：真实链路里 RightPanel 的 effect 会调 sync —— 旧分支②正在这里反选 activeId
    st().syncTabsToRightTab()
    eq(active().id, gitId, 'B1 sync 后 activeId 不变（旧实现会切到 web）')
    eq(active().kind, 'git', 'B2 激活标签仍是 git')
    eq(st().rightTab, 'git', 'B3 sync 把派生字段纠正回激活标签的 kind')
  }

  console.log('\n—— C. 标签动作不碰布局（自动展开留给后续 PR 接线）——')
  {
    reset()
    const b = layoutBefore()
    st().openTab({ kind: 'git' })
    st().openTab({ kind: 'activity' })
    st().closeTab(active().id)
    eq(layoutBefore(), b, 'C1 rightLayout 在整个标签动作序列里不变')
    eq(st().rightExpanded, false, 'C2 rightExpanded 镜像也不变')
  }

  console.log('\n—— D. 关光标签：activeId=null，rightTab 保留（不抛错）——')
  {
    reset()
    st().openTab({ kind: 'git' })
    for (const t of [...(st().tabsByScope[S1]?.tabs ?? [])]) st().closeTab(t.id)
    const set = st().tabsByScope[S1]
    eq(set.tabs.length, 0, 'D1 标签全部关闭')
    eq(set.activeId, null, 'D2 activeId = null（空态）')
    eq(st().rightTab, 'git', 'D3 rightTab 保留最后的值（兼容字段语义）')
  }

  console.log('\n—— E. 切会话：activeId 恢复，rightTab 与之一致 ——')
  {
    reset()
    st().openTab({ kind: 'git' })
    const gitId = active().id
    // 切到 S2：没有标签集 → 按当前 rightTab 建默认集
    useAppStore.setState({ activeSessionId: S2 })
    st().syncTabsToRightTab()
    const s2active = active()
    assert(s2active != null, 'E1a 新会话的 activeId 可解析')
    eq(st().tabsByScope[S2]?.tabs.length, 1, 'E1b 新会话建了默认标签集（1 个）')
    eq(st().rightTab, s2active.kind, 'E2 新会话 rightTab = 其激活标签')
    // 切回 S1
    useAppStore.setState({ activeSessionId: S1 })
    st().syncTabsToRightTab()
    eq(st().tabsByScope[S1]?.activeId, gitId, 'E3 切回后 activeId 是原来的 git 标签')
    eq(st().rightTab, 'git', 'E4 切回后 rightTab 与 activeId 一致')
  }

  console.log('\n—— F. 同 kind 两个实例：rightTab 相同、activeId 不同（PR-04 地基）——')
  {
    reset()
    st().openTab({ kind: 'file', path: '/ws/a.md', mode: 'source' })
    const a = active().id
    st().openTab({ kind: 'file', path: '/ws/b.md', mode: 'source' })
    const b = active().id
    assert(a !== b, 'F1 两个 file 标签各自成实例（path 不同）')
    eq(st().rightTab, 'file', 'F2 两个实例的 kind 相同 → rightTab 不变')
    st().activateTab(a)
    eq(st().rightTab, 'file', 'F3 切到另一个 file 实例，rightTab 仍是 file')
    eq(active().id, a, 'F4 activeId 跟着切')
  }

  console.log('\n—— G. 恢复态的空标签集：标记能清掉，不抛错 ——')
  {
    reset()
    useAppStore.setState({ tabsByScope: { [S1]: { tabs: [], activeId: null, closed: [], restored: true } } })
    st().syncTabsToRightTab()
    eq(st().tabsByScope[S1]?.restored, false, 'G1 空集恢复态首次 sync 即清标记')
    eq(st().tabsByScope[S1]?.tabs.length, 0, 'G2 不因为清标记而补默认标签（空态保持）')
  }

  console.log(`\n全部通过：${pass} 项断言\n`)
}

main().catch((e) => {
  console.error(`\n❌ ${e.message}\n`)
  process.exit(1)
})
