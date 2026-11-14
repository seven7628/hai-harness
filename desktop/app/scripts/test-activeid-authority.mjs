#!/usr/bin/env node
// PR-02 不变量：activeId 权威。（R11b 起「面板 = 激活标签的 kind」由选择器现算，
// 这里原来钉的是派生字段 rightTab === 激活标签的 kind —— 字段已删，同一事实改由
// `selectRightTab` 表达；选择器本身另有单测 src/lib/rightPanel-test.ts。）
//
// 这几条是后续所有 Surface 迁移的地基（PR-03/04/06/07 都假设它们成立）：
//   1) 任何标签动作之后，选择器读到的 kind == 激活标签的 kind（关光标签时无值，**不**保留旧值）
//   2) 外部改非标签状态、以及 syncTabsToRightTab，都**不**换掉用户正在看的标签
//   3) 标签动作不碰布局（rightLayout 不变；自动展开只属于 openTab）
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
const { useAppStore, selectRightTab } = require(outfile)

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
    rightExpanded: false,
    rightLayout: { open: false },
    leftExpanded: true,
    settings: st().settings,
    ...patch,
  })
}

const layoutBefore = () => JSON.stringify(st().rightLayout)

async function main() {
  console.log('\n—— A. 标签动作后选择器恒等于激活标签的 kind ——')
  {
    reset()
    st().openTab({ kind: 'git' })
    eq(active()?.kind, 'git', 'A1 openTab(git) → 激活 git')
    eq(selectRightTab(st()), 'git', 'A2 选择器派生自激活标签')

    const gitId = active().id
    st().openTab({ kind: 'web' })
    eq(selectRightTab(st()), 'web', 'A3 再开 web → 选择器跟着走')
    st().activateTab(gitId)
    eq(selectRightTab(st()), 'git', 'A4 切回 git → 选择器跟着走')

    st().closeTab(gitId)
    const next = active()
    assert(next != null, 'A5a 关掉激活标签后仍有新激活项（顺延到邻居）')
    eq(selectRightTab(st()), next.kind, 'A5b 选择器跟着新激活项')
  }

  // R11b：B 组原来验的是「外部直写面板字段不会换掉激活标签」。字段已删，那条路不可表达；
  // 保留下来的同义事实是：syncTabsToRightTab 与任何**非标签**状态写入都不碰 activeId。
  console.log('\n—— B. 外部改非标签状态 / sync 都不换掉用户正在看的标签 ——')
  {
    reset()
    st().openTab({ kind: 'web' })   // 集合里**确实存在** web 标签：旧实现一定会切过去
    const webId = active().id
    st().openTab({ kind: 'git' })
    const gitId = active().id
    assert(gitId !== webId, 'B0 两个标签实例不同')
    useAppStore.setState({ rightExpanded: true, leftExpanded: false }) // 非标签状态
    // 关键：真实链路里 RightPanel 的 effect 会调 sync
    st().syncTabsToRightTab()
    eq(active().id, gitId, 'B1 sync 后 activeId 不变（sync 只建默认集/清标记）')
    eq(active().kind, 'git', 'B2 激活标签仍是 git')
    eq(selectRightTab(st()), 'git', 'B3 选择器读的就是激活标签（没有可漂移的副本）')
  }

  console.log('\n—— C. D-A：只有 openTab 会展开右栏，其它标签动作不碰布局 ——')
  {
    reset()
    eq(st().rightLayout.open, false, 'C0 初始收起')
    st().openTab({ kind: 'git' })
    eq(st().rightLayout.open, true, 'C1 openTab → 自动展开（D-A 的唯一判定点）')
    eq(st().rightExpanded, true, 'C2 rightExpanded 镜像同步')
    const afterOpen = JSON.stringify(st().rightLayout)
    st().activateTab(active().id)
    eq(JSON.stringify(st().rightLayout), afterOpen, 'C3 activateTab 不改布局')
    st().openTab({ kind: 'activity' })
    eq(JSON.stringify(st().rightLayout), afterOpen, 'C4 已展开时再 openTab 不重置 bias')

    // 用户手动收起 → 下一次内容触发仍要展开（不加锁）
    st().setRightExpanded(false)
    eq(st().rightLayout.open, false, 'C5 手动收起')
    st().closeTab(active().id)
    eq(st().rightLayout.open, false, 'C6 closeTab 不会把面板弹开')
    st().openTab({ kind: 'git' })
    eq(st().rightLayout.open, true, 'C7 收起后下一次 openTab 仍展开（D-A 无锁）')
  }

  console.log('\n—— D. 关光标签：activeId=null，选择器无值（不抛错、不保留旧值）——')
  {
    reset()
    st().openTab({ kind: 'git' })
    for (const t of [...(st().tabsByScope[S1]?.tabs ?? [])]) st().closeTab(t.id)
    const set = st().tabsByScope[S1]
    eq(set.tabs.length, 0, 'D1 标签全部关闭')
    eq(set.activeId, null, 'D2 activeId = null（空态）')
    eq(selectRightTab(st()), undefined, 'D3 关光标签后无值（旧字段会保留最后的值 —— 被删掉的正是那个语义）')
  }

  console.log('\n—— E. 切会话：activeId 恢复，选择器与之一致 ——')
  {
    reset()
    st().openTab({ kind: 'git' })
    const gitId = active().id
    // 切到 S2：没有标签集 → sync 建默认集（activity）
    useAppStore.setState({ activeSessionId: S2 })
    st().syncTabsToRightTab()
    const s2active = active()
    assert(s2active != null, 'E1a 新会话的 activeId 可解析')
    eq(st().tabsByScope[S2]?.tabs.length, 1, 'E1b 新会话建了默认标签集（1 个）')
    eq(selectRightTab(st()), s2active.kind, 'E2 新会话选择器 = 其激活标签 kind')
    // 切回 S1
    useAppStore.setState({ activeSessionId: S1 })
    st().syncTabsToRightTab()
    eq(st().tabsByScope[S1]?.activeId, gitId, 'E3 切回后 activeId 是原来的 git 标签')
    eq(selectRightTab(st()), 'git', 'E4 切回后选择器与 activeId 一致')
  }

  console.log('\n—— F. 同 kind 两个实例：选择器相同、activeId 不同（PR-04 地基）——')
  {
    reset()
    st().openTab({ kind: 'file', path: '/ws/a.md', mode: 'source' })
    const a = active().id
    st().openTab({ kind: 'file', path: '/ws/b.md', mode: 'source' })
    const b = active().id
    assert(a !== b, 'F1 两个 file 标签各自成实例（path 不同）')
    eq(selectRightTab(st()), 'file', 'F2 两个实例的 kind 相同 → 选择器不变')
    st().activateTab(a)
    eq(selectRightTab(st()), 'file', 'F3 切到另一个 file 实例，选择器仍是 file')
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
