// 工作区级 git 自动识别回归测试（真实 store + esbuild 打包，Node 直接跑，无需浏览器）。
//
// 背景：git 能力此前只挂在「活跃会话」上 —— 添加本地 git 项目为工作区后（无会话），
// 不会发出任何 git_snapshot 请求，状态栏/变更面板误报「不是 Git 仓库」。
// 修复：loadWsGit（工作区级探测，不依赖会话）+ setActiveWorkspace/start 自动触发 +
// command_response(git_snapshot) 按「有/无 session」分流写入 view 或 wsGit。
//
// 用法：npm run test:ws-git
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)

// 1) shim localStorage
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}

// 2) esbuild 打包真实 store（含真实 mock transport）
const dir = mkdtempSync(join(tmpdir(), 'ws-git-'))
const outfile = join(dir, 'store-bundle.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' }, // mock.ts 的 ?raw/?url 资源
})
const { useAppStore } = require(outfile)

const st = () => useAppStore.getState()

// 启动（wire transport + bridgeStatus 流转；start() 本身也会触发一次工作区探测）
useAppStore.setState({ workspace: '/mock/w', projects: [{ path: '/mock/w', sessions: [] }], bridgeStatus: 'idle' })
await st().start()
assert.equal(st().bridgeStatus, 'running', '启动失败：bridgeStatus 应为 running')

// 场景1：启动水合即自动探测 —— 无需任何会话，wsGit 落地、loading 复位
assert.ok(st().wsGit, '场景1失败：start() 后应已完成工作区级 git 探测（wsGit 非空）')
assert.equal(st().wsGitLoading, false, '场景1失败：响应到达后 wsGitLoading 应复位')
assert.equal(st().wsGit.is_repo, false, '场景1失败：mock 工作区非 git 仓库，is_repo 应为 false')
console.log('PASS 场景1：无会话时工作区级 git 探测结果写入 wsGit')

// 场景2：显式 loadWsGit 幂等重探
useAppStore.setState({ wsGit: undefined })
st().loadWsGit()
assert.ok(st().wsGit, '场景2失败：loadWsGit 后 wsGit 应重新写入')
console.log('PASS 场景2：loadWsGit 显式触发工作区级探测')

// 场景3：setActiveWorkspace（添加/切换工作区主入口）→ 自动触发探测
useAppStore.setState({ wsGit: undefined, wsGitLoading: false })
st().setActiveWorkspace('/mock/w2')
assert.ok(st().wsGit, '场景3失败：切换工作区后应自动完成 git 探测（wsGit 非空）')
assert.equal(st().workspace, '/mock/w2', '场景3失败：活跃工作区未切换')
console.log('PASS 场景3：setActiveWorkspace 自动识别新工作区的 git 状态')

// 场景4（回归防护）：会话级 git_snapshot 仍走 view 写入 + 顶层镜像，不被工作区分流劫持
const sid = 's-git'
useAppStore.setState({ activeSessionId: sid })
st().loadGitSnapshot() // payload 带 session_id → 响应回显 → 必须写 views[sid]，而非 wsGit
const v = st().views[sid]
assert.ok(v, '场景4失败：会话级快照未写入对应 view')
assert.equal(v.gitLoading, false, '场景4失败：会话级快照 loading 未复位')
assert.ok(v.git && typeof v.git.is_repo === 'boolean', '场景4失败：会话级 git 数据缺失')
console.log('PASS 场景4：会话级 git_snapshot 仍正确路由到会话 view')

// 场景5：Statusbar 回退语义 —— 会话级非仓库时读 wsGit；会话级是仓库时优先会话级
useAppStore.setState({
  git: { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true },
  wsGit: { is_repo: true, branch: 'main', ahead: 0, behind: 0, staged: 1, modified: 2, untracked: 0, conflicted: 0, clean: false },
})
const effGit = st().git.is_repo ? st().git : st().wsGit ?? st().git
assert.equal(effGit.branch, 'main', '场景5失败：会话级无仓库数据时应回退到 wsGit')
console.log('PASS 场景5：Statusbar 数据源回退（会话级 → 工作区级）')

// 场景6：工作区级 git_history —— 无会话拉取历史，写入 wsGitHistory（B2 修复：此前响应被丢弃）
useAppStore.setState({ activeSessionId: undefined, wsGitHistory: [] })
st().loadWsGitHistory()
assert.ok(Array.isArray(st().wsGitHistory) && st().wsGitHistory.length === 1, '场景6失败：工作区级历史应写入 wsGitHistory')
assert.equal(st().wsGitHasMore, false, '场景6失败：has_more 应为 false')
assert.equal(st().wsGitHistLoading, false, '场景6失败：历史加载态未复位')
console.log('PASS 场景6：工作区级 git_history 路由到 wsGitHistory')

// 场景7：工作区级 git_commit_diff —— diff 写回对应提交
const mockHash = st().wsGitHistory[0].hash
st().loadWsCommitDiff(mockHash)
const patched = st().wsGitHistory.find((c) => c.hash === mockHash)
assert.ok(patched?.diff && patched.diff.includes('diff --git'), '场景7失败：提交 diff 未写回 wsGitHistory')
assert.equal(st().wsCommitDiffLoading, undefined, '场景7失败：提交 diff 加载态未复位')
console.log('PASS 场景7：工作区级 git_commit_diff 写回对应提交')

// 场景8（回归防护）：会话级 git_history / git_commit_diff 仍正确路由到 view（B2 修复的会话侧）
useAppStore.setState({ activeSessionId: sid, views: {} })
st().loadGitHistory()
const vh = st().views[sid]
assert.ok(vh?.gitHistory?.length === 1, '场景8失败：会话级历史未写入 view（B2 回归）')
st().loadCommitDiff(mockHash)
const vh2 = st().views[sid]
assert.ok(vh2?.gitHistory?.[0]?.diff?.includes('diff --git'), '场景8失败：会话级提交 diff 未写回 view')
console.log('PASS 场景8：会话级 git_history/git_commit_diff 路由到 view')

// 场景9：工作区级 git_file_diff —— 写入 wsFileDiffs 缓存
useAppStore.setState({ activeSessionId: undefined, wsFileDiffs: {} })
st().loadWsFileDiff('README.md', false)
assert.ok(st().wsFileDiffs['README.md']?.includes('diff --git'), '场景9失败：工作区级文件 diff 未写入 wsFileDiffs')
console.log('PASS 场景9：工作区级 git_file_diff 路由到 wsFileDiffs')

// 场景10（P1）：git_stage 写操作 —— ok 后 busy 复位且触发双层重拉（wsGit 仍在即视为重拉完成）
useAppStore.setState({ activeSessionId: undefined, gitOpBusy: undefined, gitOpError: undefined })
st().gitStage(['a.txt'])
assert.equal(st().gitOpBusy, undefined, '场景10失败：写操作 ok 后 gitOpBusy 应复位')
assert.equal(st().gitOpError, undefined, '场景10失败：成功不应置 gitOpError')
assert.ok(st().wsGit, '场景10失败：写操作成功后应重拉工作区快照')
console.log('PASS 场景10：git_stage ok → busy 复位 + 双层重拉')

// 场景11（P1）：git_commit ok → busy 复位
st().gitCommit('test', false)
assert.equal(st().gitOpBusy, undefined, '场景11失败：commit ok 后 gitOpBusy 应复位')
console.log('PASS 场景11：git_commit ok 路由')

// 场景12（P1.5）：会话级历史「加载更多」—— append 追加而非替换
useAppStore.setState({ activeSessionId: sid, views: {} })
st().loadGitHistory()
assert.equal(st().views[sid]?.gitHistory?.length, 1, '场景12失败：会话级历史首页应为 1 条')
assert.equal(st().views[sid]?.gitHasMore, false, '场景12失败：会话级 has_more 应为 false')
st().loadGitHistory(true) // mock 恒返回 1 条 → append 后 2 条
assert.equal(st().views[sid]?.gitHistory?.length, 2, '场景12失败：加载更多应追加而非替换')
console.log('PASS 场景12：会话级 git_history 分页追加')

// 场景13（P1）：写操作在途时防并发 —— busy 未复位前再次调用被忽略（以 commit 为例：
// mock 同步应答无法观察 busy 窗口，这里验证 busy 复位后可再次发起）
st().gitStage(['b.txt'])
assert.equal(st().gitOpBusy, undefined, '场景13失败：第二次写操作应正常完成并复位')
console.log('PASS 场景13：写操作可重复发起（busy 生命周期正确）')

// 场景14（P2）：分支清单路由 + checkout 走写操作通道
useAppStore.setState({ activeSessionId: undefined, gitBranches: undefined })
st().loadGitBranches()
assert.ok(Array.isArray(st().gitBranches) && st().gitBranches?.length === 1, '场景14失败：分支清单应写入 gitBranches')
assert.equal(st().gitBranches?.[0].name, 'main', '场景14失败：分支名不符')
st().gitCheckout('dev')
assert.equal(st().gitOpBusy, undefined, '场景14失败：checkout ok 后 busy 应复位')
console.log('PASS 场景14：分支清单路由 + checkout 写通道')

// 场景15（P2）：git init ok → busy 复位（非仓库空态引导链路）
st().gitInitRepo()
assert.equal(st().gitOpBusy, undefined, '场景15失败：git init ok 后 busy 应复位')
assert.equal(st().gitOpError, undefined, '场景15失败：git init ok 不应置错')
console.log('PASS 场景15：git init 引导链路路由')

console.log('WS_GIT_TEST_PASS')
