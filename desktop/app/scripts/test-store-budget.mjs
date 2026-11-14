// 视图内存预算的**接线层**回归（2026-11-12，P1）。
//
// 为什么要有这个测试：单元测试（viewBudget-test.ts）证明了 planBudget / degradeView
// 的**逻辑**对，但没证明**接线**对 —— 也就是：
//
//	事件 → setView → scheduleBudgetCheck（1s 定时器）→ enforceViewBudget
//	     → planBudget → setState({views})
//
// 这条链子上任一环断了（比如定时器没排上、节流把检查吃掉、setState 没生效），
// **单元测试全绿而机制完全不工作** —— 这正是线上看到的现象：
// 渲染进程 978 MB 的 V8 堆，而预算 256 MB 却没触发降级。
//
// 用法：npm run test:store:budget
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)

// 1) shim localStorage（store 模块加载时读取项目树/偏好）
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}

// 2) esbuild 打包 store（含真实 mock transport）到临时 CJS
const dir = mkdtempSync(join(tmpdir(), 'store-budget-'))
const outfile = join(dir, 'store-bundle.cjs')
const assetPlugin = {
  name: 'asset-suffix',
  setup(b) {
    b.onResolve({ filter: /\?(raw|url|no-inline)$/ }, () => ({
      path: 'asset',
      namespace: 'asset-stub',
    }))
    b.onLoad({ filter: /.*/, namespace: 'asset-stub' }, () => ({
      contents: 'module.exports = ""',
      loader: 'js',
    }))
  },
}

await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  plugins: [assetPlugin],
})
const { useAppStore, dispatchEvent } = require(outfile)

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// 造一个大视图：n 个块，每块 textLen 字符。
function bigView(n, textLen) {
  const blocks = []
  for (let i = 1; i <= n; i++) {
    blocks.push({
      kind: 'assistant',
      id: i,
      text: 'x'.repeat(textLen),
      streaming: false,
      thinking: '',
    })
  }
  return {
    blocks,
    agentDetached: [],
    approvals: [],
    runNodes: [],
    diffs: [],
    todos: [],
    usage: {},
    git: {},
    gitFiles: [],
    gitHistory: [],
    turns: [],
    traceEvents: [],
    streaming: false,
    runEnded: false,
    aborted: false,
    reqBlock: {},
    toolStartTs: {},
    runningAgentCount: 0,
    thinkStartTs: {},
    questions: [],
    pendingQueue: [],
    rightExpanded: false,
    rightLayout: { ratio: 0.32 },
    compressing: false,
    gitLoading: false,
    hydrated: true,
  }
}

// —— 场景 1：超预算 → **非活跃**视图被降级；活跃视图按设计被保护 ——
//
// 两个视图，每个 4,000 块 × 20KB 文本 ⇒ 估算 ≈ 2 × 160MB = 320MB > 256MB 预算。
// 切到 s2 后 s2 成为活跃 ⇒ s1 是候选 ⇒ s1 被降级；s2 受保护（活跃会话不许
// 在用户眼前把历史裁掉 —— 这是设计，不是 bug）。
useAppStore.setState({
  workspace: '/mock/w',
  activeSessionId: 's1',
  bridgeStatus: 'running',
  sessions: [{ id: 's1', title: 't' }],
  projects: [{ path: '/mock/w', sessions: [{ id: 's1', title: 't' }] }],
  views: { s1: bigView(4000, 20000), s2: bigView(4000, 20000) },
  blocks: [],
  runNodes: [],
  approvals: [],
  diffs: [],
  todos: [],
  usage: { input: 0, output: 0, cacheRead: 0, reasoning: 0 },
  turns: [],
  costUsd: 0,
  toolStartTs: {},
})

assert.ok(Object.values(useAppStore.getState().views).every((v) => v.blocks.length === 4000), '夹具没建好')

// 触发一次 setView（走 setView → scheduleBudgetCheck → 1s 定时器 → enforceViewBudget）。
useAppStore.getState().setActiveSession('s2')
await sleep(1500)

const after = useAppStore.getState().views
const s1n = after.s1?.blocks.length ?? 0
const s2n = after.s2?.blocks.length ?? 0
assert.ok(s1n < 4000, `非活跃视图 s1 应被降级，实际 ${s1n} 块`)
assert.ok(s2n === 4000, `活跃视图 s2 按设计受保护，实际 ${s2n} 块`)
console.log(`PASS 场景1：超预算 → 非活跃视图降级（s1 4000 → ${s1n}），活跃视图受保护（s2 ${s2n}）`)

// —— 场景 2：只剩活跃视图还超预算 → 它也降级，且**顶层镜像跟着降**——
//
// store 有两份 blocks：views[sid].blocks 与顶层 s.blocks（活跃会话镜像）。
// 降级只改 views 的话，顶层还指着旧数组 ⇒ 白降级（旧数组仍被引用 ⇒ 内存没释放）。
// 这是线上「978 MB 的 V8 堆却不降级」的一个直接原因。
useAppStore.setState({
  views: { s1: bigView(4000, 40000) },
  activeSessionId: 's1',
  blocks: [],
})
// 触发一次 setView（事件路径）—— setActiveSession 对**同一会话**会早退（不换会话
// 就不必重新恢复），那条路走不到 evictViews ⇒ 用事件来触发。
dispatchEvent({ event_type: 'agent_start', session_id: 's1', run_id: 'r-budget' })
await sleep(1500)

const v1 = useAppStore.getState().views.s1
assert.ok(v1.blocks.length <= 200, `活跃视图也该降级（最后手段），实际 ${v1.blocks.length} 块`)
const top = useAppStore.getState().blocks
assert.ok(
  top.length <= 200,
  `顶层镜像没跟着降级（${top.length} 块）⇒ 旧数组仍被引用，内存没释放`,
)
console.log(`PASS 场景2：活跃视图最后降级（${v1.blocks.length} 块）且顶层镜像同步（${top.length} 块）`)
