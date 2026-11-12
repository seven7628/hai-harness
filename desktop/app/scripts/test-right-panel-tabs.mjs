#!/usr/bin/env node
// L1 现状钉桩：右侧栏标签（RightTab）的**现有**行为。
//
// 为什么这组断言有意义：
//   右侧栏要从「5 值枚举单选组」改成「浏览器式标签管理」（docs/RIGHT_PANEL_TABS_PLAN.md）。
//   阶段 1 是**行为等价重构**——标签条上用户看得见的行为必须一模一样。等价重构最难防的
//   回归是「静默改变语义」：切会话不再恢复原标签、openFile 不再展开面板、focusBrowser 不再
//   收左栏……这些都测不出来（没有断言就等于允许漂移）。本脚本把今天的语义逐条钉住：
//
//     · 作用域 = 会话级（`views[sid].rightTab`），顶层只是活跃会话的镜像
//     · 未缓存会话回落默认（rightTab='activity' / rightExpanded=false）
//     · openFile 三件套：切 file tab + 展开 + 定位；filePreview/fileFocus 是**全局单例**
//     · focusBrowser 收左栏、focusBrowserForPreview 不收（语义分界，2026-09 实测缺陷的修复）
//     · rightSize 只在「首次进入/从折叠态打开」文件标签时给 60% 宽
//     · 视图缓存 LRU 上限 + running 会话保活（被淘汰 = 该会话右栏状态丢失）
//
// [阶段N 会变] 标记的断言描述的是**即将被有意改变**的行为：阶段 1/2 落地时它们应当**转红**，
// 那时按新语义改写，而不是当成回归。其余断言在重构后必须继续全绿。
//
// 用法：cd desktop/app && npm run test:right-tabs
// 退出码：全通过 0；任一断言失败非 0。
import { build } from 'esbuild'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const eq = (actual, expected, m) => {
  if (actual !== expected) throw new Error(`断言失败: ${m}\n     期望: ${JSON.stringify(expected)}\n     实际: ${JSON.stringify(actual)}`)
  ok(`${m}（= ${JSON.stringify(actual)}）`)
}

// 1) shim localStorage（store 模块加载时读取项目树/标题覆盖/偏好）
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}

// 2) mock window.desktop（**必须在 require bundle 之前**：store 模块加载时按
//    `typeof window !== 'undefined' && window.desktop` 选 transport）。这里只给无副作用桩：
//    被测对象是 store 内部的状态语义，不是 IPC 往返。
const sent = []
// 记录对底层视图池的调用（web 标签的切/关必须落到这里 —— 内层标签条退役后它是唯一入口）
const calls = { activate: [], close: [], open: [] }
global.window = {
  innerWidth: 1600, // setRightTab('file') 的 60% 宽规则读它
  desktop: {
    command: async (line) => { sent.push(line); return { ok: true } },
    embedStatus: async () => ({ ready: true, visible: true, attached: true, url: '', title: '', activeId: null, targets: [] }),
    embedCloseTarget: async (id) => { calls.close.push(id); return { ok: true } },
    embedSetSession: async () => ({ ok: true }),
    embedOpenTarget: async (url) => { calls.open.push(url); return { ok: true } },
    embedActivate: async (id) => { calls.activate.push(id); return { ok: true } },
    embedHide: async () => ({ ok: true }),
    embedShow: async () => ({ ok: true }),
    embedResize: async () => ({ ok: true }),
  },
  setTimeout: (...a) => setTimeout(...a),
  clearTimeout: (...a) => clearTimeout(...a),
}

// 3) esbuild 打包真实 store 到临时 CJS 后 require（与 test-store-scoping.mjs 同策略）
const dir = mkdtempSync(join(tmpdir(), 'right-tabs-'))
const outfile = join(dir, 'store-bundle.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  // store 间接依赖 src/transport/mock.ts，它 import 了 README/brand 资源（`?raw`/`?no-inline`/
  // `?url` 后缀）。node 侧没有 vite 的资源插件链 → 这里给 esbuild 补 loader，否则打包直接失败。
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})
const { useAppStore, putFilePreview } = require(outfile)

const WS = '/mock/ws'
const OTHER = '/mock/other'
const S1 = 's1'
const S2 = 's2'

// 一个「已水合」的会话 view：显式 hydrated=true（setActiveSession 只认这个标记；
// 旧判据 blocks.length>0 会把后台尾部事件/陈旧 preBuf 也误判成完整历史）。
// 形状从 store 自身取（先造一个真 view 当模板）——手工列字段会在 SessionView 增删字段时静默失效
//（evictViews → sessionBusy 会读 runNodes，缺了就直接 TypeError）。
const TEMPLATE = (() => {
  useAppStore.setState({ activeSessionId: '__tpl__', workspace: WS })
  useAppStore.getState().setRightTab('activity')
  const v = useAppStore.getState().views['__tpl__']
  const views = { ...useAppStore.getState().views }
  delete views['__tpl__']
  useAppStore.setState({ views, activeSessionId: undefined })
  return v
})()
const hydrated = (rightTab, rightExpanded) => ({
  ...TEMPLATE,
  hydrated: true,
  blocks: [{ id: 1, role: 'user', text: 'x' }],
  rightTab,
  rightExpanded,
  // TEMPLATE 是真实 view，带 {open:false}；只改 rightExpanded 会造出漂移态（PR-01 起布局是权威）
  rightLayout: rightExpanded ? { open: true, bias: 'even', ratio: 50 } : { open: false },
})

function reset(state = {}) {
  const views = state.views ?? {}
  const sid = state.activeSessionId
  // 顶层镜像是活跃会话 view 的镜像（真实应用里两者恒等）：给 views[active] 时必须同步，
  // 否则 setActiveSession 的「切走前回写顶层镜像」会把不一致的顶层值写回该 view。
  const mirror = sid && views[sid]
    ? {
        rightTab: views[sid].rightTab,
        rightExpanded: views[sid].rightExpanded,
        // rightLayout 是权威、rightExpanded 是镜像（PR-01）：seed 时两个一起写，否则会造出漂移态
        rightLayout: views[sid].rightLayout ?? { open: !!views[sid].rightExpanded, bias: 'even', ratio: 50 },
      }
    : {}
  useAppStore.setState({
    workspace: WS,
    activeSessionId: undefined,
    projects: [
      { path: WS, sessions: [{ id: S1, title: 'A' }, { id: S2, title: 'B' }] },
      { path: OTHER, sessions: [] },
    ],
    views: {},
    tabsByScope: {},
    recentSessions: [],
    titleOverrides: {},
    rightTab: 'activity',
    rightExpanded: false,
    rightLayout: { open: false },
    rightSize: null,
    leftExpanded: true,
    filePreviews: {},
    fileFocus: undefined,
    unreadViews: [],
    browserPages: [],
    browserShot: undefined,
    browserBusy: false,
    ...state,
    ...mirror,
    ...(state.rightTab !== undefined ? { rightTab: state.rightTab } : {}),
    ...(state.rightExpanded !== undefined
      ? {
          rightExpanded: state.rightExpanded,
          rightLayout: state.rightExpanded ? { open: true, bias: 'even', ratio: 50 } : { open: false },
        }
      : {}),
  })
}
const st = () => useAppStore.getState()

async function main() {
  console.log('\n—— A. 默认态与回落 ——')
  {
    reset()
    eq(st().rightTab, 'activity', 'A1 初始 rightTab')
    eq(st().rightExpanded, false, 'A2 初始 rightExpanded（面板默认收起）')

    // 未缓存会话：isHydrated=false → resetView() 回落默认
    reset({ activeSessionId: S1 })
    st().setRightTab('git')
    eq(st().rightTab, 'git', 'A3 有活跃会话时 setRightTab 生效')
    useAppStore.setState({ activeSessionId: S2 }) // 直接切，不经过 setActiveSession（未缓存分支的模拟）
    eq(st().rightTab, 'git', 'A4 [已知现状] 顶层镜像不随 activeSessionId 直接赋值变化（真实切换走 setActiveSession）')
  }

  console.log('\n—— B. 作用域：会话级（views[sid] 才是真相源）——')
  {
    reset({ activeSessionId: S1 })
    st().setRightTab('git')
    st().setRightExpanded(true)
    eq(st().views[S1].rightTab, 'git', 'B1 写入该会话的 view（不只是顶层镜像）')
    eq(st().views[S1].rightExpanded, true, 'B2 rightExpanded 同样是会话级')

    // 无活跃会话：只写顶层（视图无归属）
    reset()
    st().setRightTab('git')
    eq(st().rightTab, 'git', 'B3 无活跃会话时 setRightTab 仍写顶层')
    eq(Object.keys(st().views).length, 0, 'B4 无活跃会话时不创建 view')
  }

  console.log('\n—— C. 切会话：缓存命中恢复自己的标签，未缓存回落默认 ——')
  {
    reset({
      activeSessionId: S1,
      views: { [S1]: hydrated('git', true), [S2]: hydrated('web', true) },
    })
    // 真实切换（缓存命中 → loadViewToTop）
    st().setActiveSession(S2, WS)
    eq(st().rightTab, 'web', 'C1 切到缓存会话 → 恢复该会话自己的 rightTab')
    eq(st().rightExpanded, true, 'C2 同时恢复 rightExpanded')
    st().setActiveSession(S1, WS)
    eq(st().rightTab, 'git', 'C3 切回 → 仍是该会话自己的标签（互不串）')

    // C5：切换前把顶层镜像**回写**当前会话 view（防事件交错的中间态丢失）
    reset({ activeSessionId: S1, views: { [S1]: hydrated('git', true), [S2]: hydrated('web', true) } })
    useAppStore.setState({ rightTab: 'file' }) // 人为制造「顶层与 view 不一致」的中间态
    st().setActiveSession(S2, WS)
    eq(st().views[S1].rightTab, 'file', 'C5 切走前把顶层镜像回写当前会话 view')

    // 未缓存会话 → resetView 回落
    reset({ activeSessionId: S1, views: { [S1]: hydrated('git', true) } })
    st().setActiveSession(S2, WS)
    eq(st().rightTab, 'activity', 'C6 切到未缓存会话 → 回落 activity（[阶段5] 持久化后会变）')
    eq(st().rightExpanded, false, 'C7 未缓存会话右栏收起（[阶段5] 持久化后会变）')
  }

  console.log('\n—— D. 跨工作区切换：resetView + clearBrowser ——')
  {
    reset({
      activeSessionId: S1,
      views: { [S1]: hydrated('git', true) },
      browserPages: [{ id: 1, url: 'https://a' }],
      browserBusy: true,
    })
    useAppStore.setState({ projects: [...st().projects, { path: OTHER, sessions: [{ id: 'o1', title: 'O' }] }] })
    st().setActiveSession('o1', OTHER)
    eq(st().workspace, OTHER, 'D1 工作区已切换')
    eq(st().rightTab, 'activity', 'D2 跨工作区 → 右栏回落默认')
    eq(st().browserPages.length, 0, 'D3 跨工作区 → 浏览器页列表清空（clearBrowser）')
    eq(st().browserBusy, false, 'D4 browserBusy 复位（防新会话 web tab 永久「拉取中」）')
  }

  console.log('\n—— E. openFile / closeFile：三件套 + 内容不再被打开动作清掉（阶段 2 改写）——')
  {
    reset({ activeSessionId: S1, rightExpanded: false })
    st().openFile('src/a.ts')
    eq(st().rightTab, 'file', 'E1 openFile → 切文件标签')
    eq(st().rightExpanded, true, 'E2 openFile → 展开右栏')
    eq(st().fileFocus?.path, 'src/a.ts', 'E3 fileFocus 指向该文件')
    eq(st().fileFocus?.seq, 1, 'E4 fileFocus.seq 每次打开递增')
    // E5（阶段 2 改写，见 §4）：阶段 1 断言的是 `filePreview === undefined`（「打开即清空旧内容，
    // 等响应回填」）。那条在**全局单例**模型下必要，但会误伤「同一文件的另一个标签」——
    // 内容现在按归一化路径索引，清空只会把 diff/source 并存的另一个标签打成「正在读取文件…」。
    // 新断言更强：① 这次打开**真的发出**了 file_preview 请求（不能只是「清空了」）；
    //          ② 另一个文件的已缓存内容**不受影响**（本阶段要修的串台）。
    const filePreviewReqs = () => sent.filter((c) => c?.type === 'file_preview').map((c) => c.payload?.path)
    assert(filePreviewReqs().includes('src/a.ts'), `E5 openFile 发出 file_preview 请求（实际 ${JSON.stringify(filePreviewReqs())}）`)

    st().openFile('src/b.ts', 'tool-1') // 带 toolId = 操作/diff 模式
    eq(st().fileFocus?.path, 'src/b.ts', 'E6 再次 openFile 覆盖同一个 fileFocus（它是「最后定位」的指针，不是内容源）')
    eq(st().fileFocus?.seq, 2, 'E7 seq 递增到 2')
    eq(st().fileFocus?.toolId, 'tool-1', 'E8 带 toolId → diff/操作模式（缺省 = source 完整预览）')

    useAppStore.setState({ filePreviews: { 'src/a.ts': { path: 'src/a.ts', content: 'A', lines: 1 } } })
    st().openFile('src/b.ts')
    eq(st().filePreviews['src/a.ts']?.content, 'A', 'E5b 打开 B 不清掉 A 的内容（阶段 1 的「打开即清空」会把它抹掉）')

    st().closeFile()
    const curFileTab = () => st().tabsByScope[S1].tabs.find((t) => t.id === st().tabsByScope[S1].activeId)
    eq(curFileTab()?.mode, undefined, 'E9 closeFile 清激活 file 标签的视图 payload（面板退回文件活动列表）')
    eq(st().fileFocus, undefined, 'E10 closeFile 清 fileFocus')
  }

  console.log('\n—— F. focusBrowser 家族：收不收左栏是语义分界 ——')
  {
    reset({ activeSessionId: S1, leftExpanded: true, rightExpanded: false })
    st().focusBrowserForPreview()
    eq(st().rightExpanded, true, 'F1 预览 → 展开右栏')
    eq(st().rightTab, 'web', 'F2 预览 → 切网页标签')
    eq(st().leftExpanded, true, 'F3 预览 **不**收左栏（2026-09 实测缺陷的修复）')

    reset({ activeSessionId: S1, leftExpanded: true, rightExpanded: false })
    st().focusBrowser()
    eq(st().rightExpanded, true, 'F4 Browser Use → 展开右栏')
    eq(st().leftExpanded, false, 'F5 Browser Use → 收起左栏（「接管视界」语义）')

    // focusBrowserForUse：已打开（展开且在 web）时不重复
    reset({ activeSessionId: S1, rightExpanded: true, rightTab: 'web', leftExpanded: true })
    st().focusBrowserForUse()
    eq(st().leftExpanded, true, 'F6 已在 web 标签 → 不重复触发（不收左栏）')

    // 收起后重开
    reset({ activeSessionId: S1, rightExpanded: false, rightTab: 'activity', leftExpanded: true })
    st().focusBrowserForUse()
    eq(st().rightTab, 'web', 'F7 收起后再次使用 → 重新打开 web 标签')
    eq(st().leftExpanded, false, 'F8 该路径仍走 focusBrowser（收左栏）')

    // —— F9-F11 会话收口（2026-09-20，用户口径「别在别的会话里掀开我正看的面板」）——
    // browser_* 工具启动会走这条路径（tool_run_start → focusBrowserForUse(sid)）。旧实现不按
    // 会话过滤 → 后台会话里 Agent 一开浏览器，**用户当前正在看的会话**面板也被掀开（还收左栏）。
    // 现在：只有工具所属会话 == 当前会话才展开；别的会话只留未读点/呼吸徽标（见 onEmbedAiAction）。
    reset({ activeSessionId: S1, rightExpanded: false, rightTab: 'activity', leftExpanded: true })
    st().focusBrowserForUse(S1)
    eq(st().rightExpanded, true, 'F9 本会话的 Browser 工具 → 照旧展开（§6.9「看 AI 在干什么」保留）')

    reset({ activeSessionId: S1, rightExpanded: false, rightTab: 'activity', leftExpanded: true })
    st().focusBrowserForUse('S-OTHER')
    eq(st().rightExpanded, false, 'F10 别的会话的 Browser 工具 → **不展开**（不掀开用户正看的面板）')
    eq(st().leftExpanded, true, 'F11 也不收左栏（F5 的「接管视界」只在本会话成立）')

    // 无 sid（旧版事件/无归属）：回落当前会话 → 行为与 F9 一致，不制造回归
    reset({ activeSessionId: S1, rightExpanded: false, rightTab: 'activity', leftExpanded: true })
    st().focusBrowserForUse()
    eq(st().rightExpanded, true, 'F12 无 sid 的事件 → 回落当前会话，照旧展开（向后兼容）')
  }

  console.log('\n—— G. rightSize：60% 宽规则只在该触发时触发 ——')
  {
    reset({ activeSessionId: S1, rightExpanded: false })
    st().setRightTab('file')
    eq(st().rightSize, Math.round(1600 * 0.6), 'G1 首次进入文件标签 → 给 60% 视口宽')

    const before = st().rightSize
    st().setRightTab('git')
    st().setRightTab('file')
    eq(st().rightSize, before, 'G2 离开再回来 → 不重置宽度（只在 rightTab!==file 或未展开时给）')

    reset({ activeSessionId: S1, rightExpanded: true, rightTab: 'file', rightSize: 700 })
    st().setRightTab('file')
    eq(st().rightSize, 700, 'G3 已在文件标签且已展开 → 保持用户调过的宽度')

    reset({ activeSessionId: S1, rightExpanded: false, rightTab: 'file', rightSize: 700 })
    st().setRightTab('file')
    eq(st().rightSize, Math.round(1600 * 0.6), 'G4 从折叠态打开文件标签 → 重新给 60%')
  }

  console.log('\n—— H. 未读点（R4）：全局数组，按视图 id ——')
  {
    reset({ unreadViews: ['t1', 't2'] })
    st().markPreviewSeen('t1')
    eq(st().unreadViews.length, 1, 'H1 markPreviewSeen 移除该项')
    eq(st().unreadViews[0], 't2', 'H2 只移除指定项')
    st().markPreviewSeen('t9')
    eq(st().unreadViews.length, 1, 'H3 不存在的 id 是幂等空操作')
  }

  console.log('\n—— I. 视图缓存 LRU：上限 10 + running 会话保活 ——')
  {
    reset()
    const sids = Array.from({ length: 12 }, (_, i) => `k${i}`)
    for (const sid of sids) {
      useAppStore.setState({ activeSessionId: sid, workspace: WS })
      st().setRightTab('git') // 每次都会创建/更新 views[sid]
    }
    await sleep(10) // evictViews 走 queueMicrotask
    const n = Object.keys(st().views).length
    assert(n <= 10, `I1 视图缓存上限 10（实际 ${n}）`)

    // running 会话保活：给它一个 running 的 run 节点
    reset()
    const busySid = 'busy-1'
    useAppStore.setState({
      activeSessionId: busySid,
      workspace: WS,
      views: { [busySid]: { ...hydrated('git', true), runNodes: [{ id: 'r1', kind: 'run', status: 'running' }] } },
    })
    for (let i = 0; i < 12; i++) {
      const sid = `x${i}`
      useAppStore.setState({ activeSessionId: sid, workspace: WS })
      st().setRightTab('git')
    }
    await sleep(10)
    assert(st().views[busySid], 'I2 running 会话的 view 被保活（sessionBusy → 不参与淘汰）')
  }

  console.log('\n—— J. toggleRight：会话级展开/收起 ——')
  {
    reset({ activeSessionId: S1, rightExpanded: false })
    st().toggleRight()
    eq(st().rightExpanded, true, 'J1 toggleRight 展开')
    eq(st().views[S1].rightExpanded, true, 'J2 写进会话 view')
    st().toggleRight()
    eq(st().rightExpanded, false, 'J3 再 toggle 收起')

    reset()
    st().toggleRight()
    eq(st().rightExpanded, true, 'J4 无会话时只写顶层')
  }

  console.log('\n—— K. 标签模型（阶段 1 新增）：开/激活/关/重开/收敛 ——')
  {
    const scopeOf = () => st().activeSessionId || st().workspace
    const setOf = () => st().tabsByScope[scopeOf()]

    // K1：setRightTab 现在等价于「打开或激活该 kind 的标签」
    reset({ activeSessionId: S1 })
    st().setRightTab('git')
    // 首次进入该 scope：标签集按 rightTab 建默认集（= [链路]，与旧默认 rightTab='activity' 等价），
    // 随后 setRightTab('git') 再补一个 变更 标签 → 共 2 个。
    eq(setOf().tabs.length, 2, 'K1 默认集(链路) + 新开的 变更 = 2 个标签')
    eq(setOf().tabs[0].kind, 'activity', 'K2 默认集第一个是链路（与旧默认标签一致）')
    eq(setOf().tabs.find((t) => t.id === setOf().activeId).kind, 'git', 'K3 新开的标签即激活')

    // K4：幂等 —— 同类再开不新增实例（**非 file 类**的幂等键仍是 kind，阶段 1 口径不变）
    st().setRightTab('git')
    st().setRightTab('activity')
    st().setRightTab('git')
    eq(setOf().tabs.length, 2, 'K4 同类重复打开不新增实例（activity + git 共 2 个）')
    eq(setOf().tabs.find((t) => t.id === setOf().activeId).kind, 'git', 'K5 激活回到 git 标签')

    // K6-K9（阶段 2 改写，见 §4）：file 标签**多实例** —— 幂等键 = kind + path + mode。
    // 阶段 1 这里断言的是「第二个文件只更新同一个 file 标签的 payload」+「仍是单个 file 标签」，
    // 那正是本阶段要修的缺陷（打开 B 覆盖 A）。新断言反向钉住：**不同文件必须各占一个标签，
    // 且各自 payload 正确**（互不覆盖），同一 (path, mode) 才命中已有实例。
    const fileTabs = () => setOf().tabs.filter((t) => t.kind === 'file')
    st().openFile('src/a.ts')
    eq(fileTabs().length, 1, 'K6 第一个文件 → 1 个 file 标签')
    eq(fileTabs()[0].path, 'src/a.ts', 'K7 且 payload 指向它')
    eq(fileTabs()[0].mode, 'source', 'K7b 无 toolId → source 完整预览')
    st().openFile('src/b.ts', 'tool-9')
    eq(fileTabs().length, 2, 'K8 第二个文件 → **新增**一个 file 标签（阶段 1 是覆盖同一个）')
    eq(fileTabs()[0].path, 'src/a.ts', 'K8b 第一个标签的 payload 没被覆盖（本阶段的主缺陷）')
    eq(fileTabs()[1].path, 'src/b.ts', 'K8c 新标签指向第二个文件')
    eq(fileTabs()[1].mode, 'diff', 'K9 带 toolId → diff 模式')
    // 幂等：同一 (path, mode) 再开 → 命中已有实例（不新增），payload **整体替换** + openSeq 自增
    //（openSeq = 「每次打开都回默认视图」的触发器，原 fileFocus.seq 的语义迁到标签上）
    const bTab = fileTabs()[1]
    st().openFile('src/b.ts', 'tool-9')
    eq(fileTabs().length, 2, 'K9b 同一 (path, mode) 再开不新增实例（幂等）')
    eq(fileTabs()[1].id, bTab.id, 'K9c 命中的是同一个标签实例')
    eq(fileTabs()[1].openSeq, (bTab.openSeq ?? 0) + 1, 'K9d openSeq 自增（每次打开回默认视图的触发器）')
    // 同一文件的 diff 与 source **可并存**（D5）：mode 进幂等键，故两条互不覆盖
    st().openFile('src/b.ts', undefined, 'source')
    eq(fileTabs().length, 3, 'K9e 同一文件的 source 标签与 diff 标签并存（各自一个实例）')
    eq(fileTabs()[1].mode, 'diff', 'K9f diff 标签仍是 diff（没被 source 打开覆盖）')
    eq(fileTabs()[2].mode, 'source', 'K9g 新标签是 source 模式')

    // K10：关闭激活标签 → 顺延到邻居
    // 标签集此刻 = [activity, git, a.ts(source), b.ts(diff), b.ts(source)]（阶段 2 起 file 多实例）
    const ids = setOf().tabs.map((t) => t.id)
    eq(ids.length, 5, 'K10a 前置：5 个标签（2 个固定类 + 3 个 file）')
    st().activateTab(ids[0]) // 激活第一个（activity）
    st().closeTab(ids[0])
    eq(setOf().tabs.length, 4, 'K10 关闭后标签数减一')
    eq(setOf().activeId, ids[1], 'K11 关闭激活项 → 顺延到右邻居')
    eq(setOf().closed[0].id, ids[0], 'K12 关闭的标签进入 closed 栈（供重开）')

    // K13：重开
    st().reopenTab()
    eq(setOf().tabs.length, 5, 'K13 重开把标签加回')
    eq(setOf().activeId, ids[0], 'K14 重开的标签被激活')

    // K15：关光 → 空集且**不自动补默认标签**（否则刚关掉的会被立刻建回来）
    for (const t of [...setOf().tabs]) st().closeTab(t.id)
    eq(setOf().tabs.length, 0, 'K15 可以关光所有标签')
    eq(setOf().activeId, null, 'K16 空集时 activeId=null（内容区显示空态）')
    st().syncTabsToRightTab()
    eq(setOf().tabs.length, 0, 'K17 空集不因 sync 而复活标签')

    // K18：sync 已一致时不产生新状态对象（防 effect 自激）
    st().setRightTab('git')
    const before = st().tabsByScope
    st().syncTabsToRightTab()
    assert(st().tabsByScope === before, 'K18 sync 已一致 → 不 set（对象引用不变）')

    // K19：直写 rightTab（e2e 复位路径）后 sync 会自愈
    useAppStore.setState({ rightTab: 'git' })
    st().syncTabsToRightTab()
    eq(setOf().tabs.find((t) => t.id === setOf().activeId).kind, 'git', 'K19 直写 rightTab → sync 自愈到对应标签')

    // K20：标签集按 scope 隔离（切会话各自一套）
    reset({
      activeSessionId: S1,
      views: { [S1]: hydrated('git', true), [S2]: hydrated('activity', true) },
    })
    st().setRightTab('git')
    const s1Tabs = st().tabsByScope[S1].tabs.map((t) => t.kind)
    st().setActiveSession(S2, WS)
    st().syncTabsToRightTab()
    const s2Tabs = st().tabsByScope[S2].tabs.map((t) => t.kind)
    assert(s1Tabs.includes('git'), `K20 S1 的标签集含 git（实际 ${JSON.stringify(s1Tabs)}）`)
    assert(!s2Tabs.includes('git'), `K21 S2 的标签集与 S1 隔离（实际 ${JSON.stringify(s2Tabs)}）`)

    // K22：无会话时 scope = 工作区路径（D7）
    reset()
    st().setRightTab('git')
    assert(st().tabsByScope[WS], 'K22 无会话 → 标签集挂在工作区路径下')
    assert(st().tabsByScope[WS].tabs.some((t) => t.kind === 'git'), 'K23 该标签集含新开的 变更 标签')
    eq(st().tabsByScope[WS].tabs.find((t) => t.id === st().tabsByScope[WS].activeId).kind, 'git', 'K24 且它是激活项')
    eq(st().rightTab, 'git', 'K25 无会话时 rightTab 仍写顶层（与旧行为一致）')
  }

  console.log('\n—— K2. openFile 的可视类分支：一个文件一个标签（2026-09 用户口径）——')
  {
    const scopeOf = () => st().activeSessionId || st().workspace
    const setOf = () => st().tabsByScope[scopeOf()]
    const fileTabs = () => setOf().tabs.filter((t) => t.kind === 'file')

    // K26：可视类（pdf/图片/html/office）**不建 file 标签**。这类内容只有内嵌浏览器能渲染，
    // file 标签里只剩一行状态（「内容已在右侧预览中打开」），与自动开的预览标签同名 ——
    // 同一个文件占两个标签，用户看到的就是重复。
    reset({ activeSessionId: S1 })
    st().openFile('out/report.png')
    eq(fileTabs().length, 0, 'K26 可视类文件不建 file 标签')
    eq(st().fileFocus?.path, 'out/report.png', 'K27 fileFocus 仍指向该文件（之后手动开「文件」标签看得到它）')
    eq(st().rightTab, 'web', 'K28 可视类文件 → 落在自动预览标签（而不是文件标签）')
    eq(st().rightExpanded, true, 'K29 仍展开右栏')

    // K30：文本类**行为完全不变** —— 这条是上面那批断言的护栏：不能为了「不重复」把 file 标签整体关掉
    st().openFile('src/c.ts')
    eq(fileTabs().length, 1, 'K30 文本类仍建 file 标签')
    eq(fileTabs()[0].path, 'src/c.ts', 'K31 且 payload 是该文件')
    eq(fileTabs()[0].mode, 'source', 'K32 无 toolId → source 完整预览（口径不变）')
    eq(st().fileFocus?.path, 'src/c.ts', 'K33 文本类 fileFocus 照旧')

    // K34：带 toolId 的文本类（diff 模式）同样不变 —— 但阶段 2 起它**新开一个标签**，
    // 而不是把上面那个 source 标签改写成 diff（那就是「打开 B 覆盖 A」的同一个缺陷）。
    st().openFile('src/d.ts', 'tool-9')
    eq(fileTabs().length, 2, 'K34 另一个文件带 toolId → 新增一个 file 标签')
    eq(fileTabs()[1].mode, 'diff', 'K35 新标签是 diff 模式（口径不变）')
    eq(fileTabs()[0].path, 'src/c.ts', 'K35b 前一个标签仍是 src/c.ts（未被改写）')

    // K36（阶段 2 新增，见 §4）：同一文件的 diff 与 source **可并存**（D5）且互不串台。
    // 用 src/c.ts 走两条：一条 source（上面 K30 建的）、一条 diff（带 toolId 打开）。
    st().openFile('src/c.ts', 'tool-7')
    const cTabs = fileTabs().filter((t) => t.path === 'src/c.ts')
    eq(cTabs.length, 2, 'K36 同一文件并存 source + diff 两个标签')
    eq(cTabs.map((t) => t.mode).sort().join(','), 'diff,source', 'K37 两个标签的 mode 各自正确（互不覆盖）')
    eq(cTabs.find((t) => t.mode === 'diff')?.toolId, 'tool-7', 'K38 diff 标签带自己的 toolId')
  }

  console.log('\n—— K3. 文件内容缓存（阶段 2：按归一化路径索引，不再是全局单例）——')
  {
    const scopeOf = () => st().activeSessionId || st().workspace
    const setOf = () => st().tabsByScope[scopeOf()]
    const fileTabs = () => setOf().tabs.filter((t) => t.kind === 'file')
    const reqPaths = () => sent.filter((c) => c?.type === 'file_preview').map((c) => c.payload?.path)

    // K39：路径归一化 —— 工作区内的**绝对路径**与**相对路径**是同一个文件：同一个标签、同一份内容。
    // 没有这条，「从对话点开」（相对）与「从左栏文件树点开」（绝对）会各开一个标签（用户看到重复）。
    reset({ activeSessionId: S1 })
    st().openFile('src/a.ts', undefined, 'source')
    st().openFile(`${WS}/src/a.ts`, undefined, 'source')
    eq(fileTabs().length, 1, 'K39 绝对/相对写法指向同一文件 → 仍是 1 个标签（不重复开）')
    eq(fileTabs()[0].path, `${WS}/src/a.ts`, 'K40 payload 更新为最后一次打开的写法（标签标题取 basename，无差异）')

    // K41：内容按路径索引 —— 两个文件的缓存并存，互不覆盖（阶段 1 的单例是「后者覆盖前者」）
    const put = (p, content) => useAppStore.setState((s) => ({ filePreviews: { ...s.filePreviews, [p]: { path: p, content, lines: 1 } } }))
    put('src/a.ts', 'AAA')
    put('src/b.ts', 'BBB')
    eq(st().filePreviews['src/a.ts']?.content, 'AAA', 'K41 两个文件的内容并存（A 没被 B 覆盖）')
    eq(st().filePreviews['src/b.ts']?.content, 'BBB', 'K42 且 B 自己的内容正确')

    // K43：ensureFilePreview —— 已有缓存不发请求；缺缓存才补拉（组件在缓存缺失时的兜底）
    const before = reqPaths().length
    st().ensureFilePreview('src/a.ts')
    eq(reqPaths().length, before, 'K43 已有缓存 → ensure 不发请求')
    st().ensureFilePreview('src/missing.ts')
    eq(reqPaths().length, before + 1, 'K44 缺缓存 → ensure 补拉一次')
    st().ensureFilePreview('src/missing.ts')
    eq(reqPaths().length, before + 1, 'K45 同一路径在途 → 不重复发（去重）')

    // K46：缓存上限（内容最大 1 MiB，不能随「开过的文件数」无界增长）。
    // 走**真实写入函数**（putFilePreview）而不是 setState 直写 —— 上限与淘汰序就实现在它里面，
    // 直写等于绕过被测对象（第一版这么写，K46 直接红了：12 条全在）。
    let cache = {}
    for (let i = 0; i < 12; i++) cache = putFilePreview(cache, `src/f${i}.ts`, { path: `src/f${i}.ts`, content: `c${i}`, lines: 1 })
    eq(Object.keys(cache).length, 8, 'K46 缓存条数封顶（FILE_PREVIEWS_MAX = 8）')
    eq(cache['src/f11.ts']?.content, 'c11', 'K47 最新写入的那条一定还在（淘汰的是最旧的）')
    assert(cache['src/f0.ts'] === undefined, 'K48 最旧的被淘汰')
    // 刷新一条旧内容 → 它排到最后，下一轮淘汰的是别人（淘汰序 = 最近刷新，不是最近创建）
    cache = putFilePreview(cache, 'src/f4.ts', { path: 'src/f4.ts', content: 'refresh', lines: 1 })
    eq(cache['src/f4.ts']?.content, 'refresh', 'K48b 重新写入已存在的键 → 内容更新')
    eq(Object.keys(cache)[Object.keys(cache).length - 1], 'src/f4.ts', 'K48c 且它被移到末尾（最近刷新）')

    // K49：切工作区 → 清空内容缓存（键是工作区相对路径，不清会把 A 的内容显示在 B 的标签里）
    reset({ activeSessionId: S1, filePreviews: { 'src/a.ts': { path: 'src/a.ts', content: 'AAA', lines: 1 } } })
    useAppStore.setState({ projects: [...st().projects, { path: OTHER, sessions: [{ id: 'o1', title: 'O' }] }] })
    st().setActiveSession('o1', OTHER)
    eq(st().filePreviews['src/a.ts'], undefined, 'K49 跨工作区切换 → 文件内容缓存清空')
  }

  console.log('\n—— L. 内嵌页面 → 面板级 web 标签（阶段 1.5：一页一标签）——')
  {
    const scopeOf = () => st().activeSessionId || st().workspace
    const setOf = () => st().tabsByScope[scopeOf()]
    const kinds = () => setOf().tabs.map((x) => x.kind)
    const webIds = () => setOf().tabs.filter((x) => x.kind === 'web').map((x) => x.viewId ?? `p${x.pageId}`)

    // L1：视图池出现两个页面 → 标签条上出现两个 web 标签（顺序 = 池顺序）
    reset({ activeSessionId: S1 })
    st().syncTabsToRightTab()
    st().syncWebTabs('native', [
      { key: 't1', url: 'file:///a.pdf', title: '' },
      { key: 't2', url: 'https://example.com', title: 'Example' },
    ], 't1')
    eq(webIds().join(','), 't1,t2', 'L1 两个页面 → 两个 web 标签（池顺序）')
    eq(setOf().tabs.find((x) => x.id === setOf().activeId).kind, 'activity',
      'L2 用户停在 链路 标签 → 后台出现的页面**不抢激活**（阶段 4 的口径在此已成立）')
    // L2b：当前就在 web 标签上 → 跟随视图池的激活页（AI 切页时标签条跟着走）
    st().setRightTab('web')
    st().syncWebTabs('native', [
      { key: 't1', url: 'file:///a.pdf', title: '' },
      { key: 't2', url: 'https://example.com', title: 'Example' },
    ], 't2')
    eq(setOf().tabs.find((x) => x.id === setOf().activeId).viewId, 't2', 'L2b 在 web 标签 → 跟随池的激活页')

    // L3：页面消失（用户/AI 在别处关了它）→ 标签被收敛掉
    st().syncWebTabs('native', [{ key: 't1', url: 'file:///a.pdf', title: '' }], 't1')
    eq(webIds().join(','), 't1', 'L3 池里消失的页面 → 标签同步删除')

    // L4：点「+ 浏览器」开的空标签被第一个页面**收养**（不留空标签）
    reset({ activeSessionId: S1 })
    st().syncTabsToRightTab()
    st().setRightTab('web') // 空 web 标签（无 viewId）
    const genericId = setOf().tabs.find((x) => x.kind === 'web').id
    st().syncWebTabs('native', [{ key: 't9', url: 'file:///x.png', title: '' }], 't9')
    eq(webIds().join(','), 't9', 'L4 空标签被页面收养')
    eq(setOf().tabs.filter((x) => x.kind === 'web').length, 1, 'L5 不残留空标签')
    eq(setOf().tabs.find((x) => x.kind === 'web').id, genericId, 'L6 收养复用原标签 id（不闪）')

    // L7：无变化时不产生新状态对象（embed-nav 事件很频繁，否则会无谓重渲染）
    const before = st().tabsByScope
    st().syncWebTabs('native', [{ key: 't9', url: 'file:///x.png', title: '' }], 't9')
    assert(st().tabsByScope === before, 'L7 无变化 → 不 set（对象引用不变）')

    // L8：切 web 标签 → 真驱动底层视图（内层标签条退役后这是唯一入口）
    calls.activate.length = 0
    reset({ activeSessionId: S1 })
    st().syncTabsToRightTab()
    st().syncWebTabs('native', [{ key: 't1', url: 'https://a', title: 'A' }, { key: 't2', url: 'https://b', title: 'B' }], 't1')
    const t2tab = setOf().tabs.find((x) => x.viewId === 't2')
    st().activateTab(t2tab.id)
    eq(calls.activate.join(','), 't2', 'L8 切标签 → embedActivate(该页)')
    eq(st().unreadViews.includes('t2'), false, 'L9 切过去即视为已读（未读点清除）')

    // L10：关 web 标签 → 真关底层页面（否则僵尸渲染进程）
    calls.close.length = 0
    st().closeTab(t2tab.id)
    eq(calls.close.join(','), 't2', 'L10 关标签 → embedCloseTarget(该页)')
    eq(webIds().join(','), 't1', 'L11 池标签同步收敛')

    // L12：mcp 数据源有数据时 → native 标签让位（两套 id 空间不能各留一排）
    st().syncWebTabs('mcp', [{ key: 'p7', url: 'https://c', title: 'C' }], 'p7')
    eq(webIds().join(','), 'p7', 'L12 换数据源 → 另一套 id 的标签被清掉')

    // —— 阶段 3：休眠条目必须**当作存在**（否则标签会被当「页面已关闭」删掉）——
    // 视图池 LRU 驱逐 = 休眠：主进程销毁渲染进程但**保留条目与 id**，targets 里仍以原 id
    // 出现（sleeping: true）。标签身份 = viewId = 那个 id，故 syncWebTabs 收到同样的 key 时
    // 必须复用原标签实例（不新建、不删）。
    reset({ activeSessionId: S1 })
    st().syncTabsToRightTab()
    st().syncWebTabs('native', [{ key: 't1', url: 'file:///a.pdf', title: 'A' }, { key: 't2', url: 'https://b', title: 'B' }], 't1')
    const keepId = setOf().tabs.find((x) => x.viewId === 't2').id
    // 主进程侧：t2 被 LRU 休眠 → 仍以原 id 留在 targets 里（url 取休眠快照 + sleeping: true）。
    // 渲染层收到的 items 因此**仍然含 t2**（sleeping 标记只用于标签灰态，不参与收敛判据）。
    const beforeSleepSync = st().tabsByScope
    st().syncWebTabs('native', [{ key: 't1', url: 'file:///a.pdf', title: 'A' }, { key: 't2', url: 'https://b', title: 'B' }], 't1')
    eq(webIds().join(','), 't1,t2', 'L13 休眠条目（key 仍在 items 里）→ 标签**不删**')
    eq(setOf().tabs.find((x) => x.viewId === 't2').id, keepId, 'L14 且复用原标签实例（id 稳定 → 不闪、不影响 activeId）')
    assert(st().tabsByScope === beforeSleepSync, 'L14b 休眠条目的重复同步不产生状态 churn（对象引用不变）')
    // 对照：真关闭（条目从 targets 消失）→ 标签才删（休眠 ≠ 关闭）
    st().syncWebTabs('native', [{ key: 't1', url: 'file:///a.pdf', title: 'A' }], 't1')
    eq(webIds().join(','), 't1', 'L15 真关闭（条目从 targets 消失）→ 标签删除（与休眠区分）')
  }

  // —— Lb. ＋ → 网页 在「池里还没有真实页面」时必须真的开出一个标签（2026-09-21 用户报障）——
  //
  // 用户原话：「右侧侧边栏点击加号、选择网页无效」。真机 trace（Electron + 真 bridge）：
  //   t=1882ms tabs → [activity, web:t1]（点击确实建了标签，认了池里的 t1）
  //   t=1886ms embedNav { activeId: t1, targets: [t1|about:blank] }
  //   t=1886ms tabs → [activity]（标签被 syncWebTabs 判死删掉，rightTab 顶回 activity）
  // 即 4ms 内「菜单点了没反应」。根因：池里常驻一个 **about:blank 自举载体**且它就是 activeId，
  // 而 onEmbedNav 喂给 syncWebTabs 的 items 会**把它过滤掉**（`url && url !== 'about:blank'`）
  // —— 标签认下了 items 里永远不存在的 id，于是被当成「这个页面已经关了」。
  // 正确的形态是**空 web 标签**（无 viewId）：它被 syncWebTabs 的收养分支接住，池里出现第一个
  // 真实页面时复用它的实例（与 L4–L6 同一条路径）。
  console.log('\n—— Lb. ＋ → 网页（池里只有 about:blank 自举载体时也要真的开出标签）——')
  {
    const scopeOf = () => st().activeSessionId || st().workspace
    const bootstrap = [{ id: 't1', url: 'about:blank', title: '', sid: S1, visible: false, sleeping: false, restoring: false }]
    const navOf = (targets, activeId) => ({ id: activeId, url: '', title: '', loading: false, canGoBack: false, canGoForward: false, activeId, targets })
    const webTabs = () => (st().tabsByScope[scopeOf()]?.tabs ?? []).filter((x) => x.kind === 'web')

    reset({ activeSessionId: S1, embedNav: navOf(bootstrap, 't1') })
    st().syncTabsToRightTab()
    st().openTab({ kind: 'web' }) // ＝ ＋ 菜单里点「网页」（rail 的「网页」图标走同一条 openTab）
    eq(webTabs().length, 1, 'L16 ＋ → 网页 建出 web 标签')
    eq(webTabs()[0].viewId, undefined,
      'L17 不认 about:blank 自举载体当身份（认了就会被下一帧 nav 事件判死）')
    eq(st().rightTab, 'web', 'L18 面板切到网页')
    const madeId = webTabs()[0].id
    // 紧跟其后的 embed-nav 事件：onEmbedNav 把 about:blank 过滤掉 → items 为空
    st().syncWebTabs('native', [], null)
    eq(webTabs().length, 1, 'L19 空 items（池里只有自举载体）→ web 标签不被吃掉')
    eq(webTabs()[0].id, madeId, 'L20 还是同一个标签实例（不闪）')
    eq(st().tabsByScope[scopeOf()].activeId, madeId, 'L21 激活项仍指向它（旧实现里它被删 → 顺延回链路）')
    eq(st().rightTab, 'web', 'L22 rightTab 不被顶回上一个标签（旧实现：4ms 内退回 activity）')
    // 池里出现第一个真实页面 → 这个空标签认领它（不新建第二个标签）
    st().syncWebTabs('native', [{ key: 't9', url: 'file:///x.png', title: 'x.png' }], 't9')
    eq(webTabs().length, 1, 'L23 第一个真实页面出现 → 不新增标签')
    eq(webTabs()[0].id, madeId, 'L24 收养复用原标签实例（不闪）')
    eq(webTabs()[0].viewId, 't9', 'L25 视图接回这个标签')
    // 反向：池里**真实页面**是激活页时，新标签仍认它（不把它降级成空标签 —— 那会让面板显示别的页）
    reset({
      activeSessionId: S1,
      tabsByScope: {}, // 这个 scope 还没有标签集（首次进入 / 标签全关后重开）
      embedNav: navOf([...bootstrap, { id: 't9', url: 'https://a', title: 'A', sid: S1, visible: true, sleeping: false, restoring: false }], 't9'),
    })
    st().setRightTab('web')
    eq(webTabs().length, 1, 'L26 真实页面在池里 → 建出 web 标签')
    eq(webTabs()[0].viewId, 't9', 'L27 且认的是**真实页面**（about:blank 才降级成空标签）')
  }

  // —— M. 切 scope：只恢复标签集与激活标签，绝不新建/激活（阶段 4 的口径）——
  // 为什么单列：阶段 4 把「自动展开/自动切标签」的四条路径都改成**按 scope 过滤 + 只由用户
  // 显式动作触发**，最容易漏的回归就是「切会话/切工作区/新建会话」这三条路径上间接调了
  // focus*（用户原话：「切会话、切换工作区、新建会话……也会自动打开右侧浏览器 Tab 很别扭」）。
  // 这里用**间谍**直接钉住「这三条路径一次 focus* 都不调」—— 比只看结果更强：结果可能因为
  // 其它守卫恰好不变，而调用本身才是缺陷来源。
  console.log('\n—— M. 切 scope 不触发任何 focus*，只恢复该 scope 的标签集 ——')
  {
    const focusCalls = []
    const wrap = (name) => {
      const orig = st()[name]
      useAppStore.setState({ [name]: (...a) => { focusCalls.push(name); return orig(...a) } })
    }
    wrap('focusBrowser')
    wrap('focusBrowserForUse')
    wrap('focusBrowserForPreview')

    // M1/M2：缓存会话 → 恢复**它自己的**标签集与激活标签
    reset({ activeSessionId: S1, views: { [S1]: hydrated('git', true), [S2]: hydrated('web', true) } })
    useAppStore.setState({ tabsByScope: { [S1]: { tabs: [{ id: 'ta', kind: 'git' }, { id: 'tb', kind: 'web', viewId: 't9' }], activeId: 'tb', closed: [] } } })
    focusCalls.length = 0
    st().setActiveSession(S2, WS)
    eq(st().rightTab, 'web', 'M1 切到缓存会话 → 恢复该会话自己的 rightTab')
    eq(st().rightExpanded, true, 'M2 同时恢复 rightExpanded')
    eq(focusCalls.join(','), '', 'M3 切会话**不调**任何 focus*（阶段 4：绝不自动展开/切标签）')
    eq(st().tabsByScope[S1].activeId, 'tb', 'M4 走掉的那个 scope 的激活标签原样保留（不被改写）')

    st().setActiveSession(S1, WS)
    eq(st().tabsByScope[S1].activeId, 'tb', 'M5 切回来仍是该 scope 自己的激活标签')
    eq(st().rightTab, 'git', 'M6 rightTab 由该 scope 的标签集恢复（git）')
    eq(focusCalls.join(','), '', 'M7 来回切两次仍不调任何 focus*')

    // M8：跨工作区切换 → 回落默认（resetView + clearBrowser），同样不 focus*
    reset({ activeSessionId: S1, views: { [S1]: hydrated('git', true) } })
    useAppStore.setState({ tabsByScope: { [S1]: { tabs: [{ id: 'ta', kind: 'git' }], activeId: 'ta', closed: [] } } })
    focusCalls.length = 0
    st().setActiveSession('o1', OTHER)
    eq(st().workspace, OTHER, 'M8 工作区已切换')
    eq(st().rightTab, 'activity', 'M9 跨工作区 → 回落默认 activity')
    eq(st().rightExpanded, false, 'M10 跨工作区 → 右栏保持收起（不因上个工作区展开过就展开）')
    eq(focusCalls.join(','), '', 'M11 跨工作区**不调**任何 focus*')
    eq(st().tabsByScope[OTHER], undefined, 'M12 新工作区**不预先建**标签集（空态由组件 effect 兜）')

    // M13：新建会话（响应路径走同一个 setActiveSession）→ 空标签集 + 收起，不自动打开
    reset({ activeSessionId: S1, views: { [S1]: hydrated('web', true) } })
    useAppStore.setState({ tabsByScope: { [S1]: { tabs: [{ id: 'ta', kind: 'web', viewId: 't1' }], activeId: 'ta', closed: [] } } })
    focusCalls.length = 0
    st().setActiveSession('brand-new', WS) // 未缓存 → resetView 回落
    eq(st().rightTab, 'activity', 'M13 新建会话 → 回落默认 activity')
    eq(st().rightExpanded, false, 'M14 新建会话 → 右栏保持收起（绝不自动打开）')
    eq(st().tabsByScope['brand-new'], undefined, 'M15 新会话**不预先建**标签集（无标签 → 空态）')
    eq(focusCalls.join(','), '', 'M16 新建会话**不调**任何 focus*')

    // 还原被包装的三个 action（后续不再用；不留包装以免污染别的断言）
    delete st().focusBrowser
    delete st().focusBrowserForUse
    delete st().focusBrowserForPreview
    eq(typeof st().focusBrowserForPreview, 'undefined', 'M17 间谍已卸下（还原到原型上的实现）')
  }

  // —— N. 标签集持久化（阶段 5：localStorage go-code.sessionTabs）——
  //
  // 这一组验的是「重启后标签集与激活标签恢复 + 视图按需重建」在 store 层的全部语义：
  //   · 写入形态：剥掉运行时字段（viewId/pageId/openSeq）、web 存 url 快照、closed 不写；
  //   · 反查失败即丢弃（不恢复点不开的死标签）；
  //   · 恢复：标签集/顺序/类型/diff-source 模式/激活标签都对，且**不展开右栏、不收左栏**；
  //   · 视图按需重建的**反向**：恢复与「重启后第一帧空 targets」都不建/不删视图；
  //   · 认领：重建出来的视图接回**原来那个标签实例**（不多出重复标签）；
  //   · 坏数据不炸（截断 JSON / 非法 kind / 旧版本 / 结构不对）；
  //   · 上限淘汰序、跨 scope 不串。
  //
  // 「重启」怎么模拟：store 在**模块加载时**同步读 localStorage（loadPersistedTabs），所以
  // 一次重启 = 清掉 require 缓存重新加载 bundle。每次都重新 require 同一个 bundle（esbuild
  // 已产物化），拿到的是一份全新的模块状态 + 全新的 store 实例。
  console.log('\n—— N. 标签集持久化（阶段 5：重启恢复 + 视图按需重建）——')
  {
    const LS_TABS = 'go-code.sessionTabs'
    const TARGETS = [{ id: 't3', url: 'file:///mock/ws/out/report.pdf', title: 'report.pdf', sid: S1, visible: true, srcPath: `${WS}/out/report.pdf`, sleeping: false, restoring: false }]
    const nav = (targets, activeId) => ({ id: activeId, url: '', title: '', loading: false, canGoBack: false, canGoForward: false, activeId, targets })
    const readStored = () => JSON.parse(mem.get(LS_TABS))
    // 一次「重启」：清 require 缓存 → 重新加载 store 模块（它会同步读 localStorage）。
    // 先等一个防抖窗口：上一次加载的实例可能还有在途的写入，否则会把本次 seed 覆盖掉。
    // 注意这个 await 也是**前面各组的在途异步链**（openFile 对可视类文件的预览打开等）第一次
    // 拿到执行机会的地方 —— A~M 全是同步的，故它们的 IPC 调用会堆到这里才发生。所以视图池
    // 调用的清点放在这个 await **之后**（放在 boot 之前会被这些尾巴污染，实测踩过）。
    const boot = async (payload) => {
      await sleep(400)
      if (payload == null) mem.delete(LS_TABS)
      else mem.set(LS_TABS, typeof payload === 'string' ? payload : JSON.stringify(payload))
      calls.open.length = 0
      calls.activate.length = 0
      calls.close.length = 0
      delete require.cache[require.resolve(outfile)]
      return require(outfile)
    }
    const persisted = (scopes) => ({ v: 1, scopes })

    // N1：写入形态 —— 只存**地址**，不存运行时字段
    reset({ activeSessionId: S1 })
    useAppStore.setState({ embedNav: nav(TARGETS, 't3') })
    st().openFile('src/a.ts', 'tool-1') // diff 标签
    st().openFile('src/a.ts', undefined, 'source') // 同一文件的 source 标签（并存）
    st().syncWebTabs('native', [{ key: 't3', url: TARGETS[0].url, title: 'report.pdf', srcPath: TARGETS[0].srcPath }], 't3')
    st().setRightTab('git')
    const gitTab = st().tabsByScope[S1].tabs.find((t) => t.kind === 'git')
    st().closeTab(gitTab.id) // closed 栈里留一条（**不应**被持久化）
    st().flushSessionTabs()
    const raw = readStored()
    eq(raw.v, 1, 'N1 存储带版本号（结构变化时整体丢弃）')
    const stored = raw.scopes[S1]
    eq(stored.tabs.map((t) => t.kind).join(','), 'activity,file,file,web', 'N2 标签集按顺序落盘（类型正确）')
    const storedDiff = stored.tabs.find((t) => t.kind === 'file' && t.mode === 'diff')
    const storedSrc = stored.tabs.find((t) => t.kind === 'file' && t.mode === 'source')
    eq(storedDiff?.path, 'src/a.ts', 'N3 file 标签存 path')
    eq(storedDiff?.toolId, 'tool-1', 'N4 diff 标签存 toolId（恢复后仍是 diff 模式）')
    eq(storedSrc?.mode, 'source', 'N5 同一文件的 source 标签独立落盘（mode 是身份的一部分）')
    assert(!mem.get(LS_TABS).includes('openSeq'), 'N6 openSeq **不**入存储（它是「每次打开回默认视图」的触发器）')
    const storedWeb = stored.tabs.find((t) => t.kind === 'web')
    eq(storedWeb?.url, TARGETS[0].url, 'N7 web 标签的 url 从视图池**反查快照**进存储（TabInstance 本身没有 url）')
    eq(storedWeb?.srcPath, `${WS}/out/report.pdf`, 'N8 源文件路径一并快照（office 预览重建靠它）')
    assert(storedWeb && !('viewId' in storedWeb) && !('pageId' in storedWeb), 'N9 运行时 id（viewId/pageId）**不**入存储')
    assert(!('closed' in stored) && !mem.get(LS_TABS).includes('"closed"'), 'N10 closed 栈不持久化（跨重启没有意义）')
    eq(stored.activeId, st().tabsByScope[S1].activeId, 'N11 激活标签一并落盘')

    // N11b：反查不到 url 的 web 标签 → 丢弃（不恢复一个点不开的死标签）
    reset({ activeSessionId: S1 })
    useAppStore.setState({ embedNav: nav([], null) })
    st().setRightTab('web') // 「+ 浏览器」开的空 web 标签（没有 viewId，也没有 url）
    st().flushSessionTabs()
    eq(readStored().scopes[S1].tabs.filter((t) => t.kind === 'web').length, 0, 'N12 反查不到 url 的 web 标签**丢弃**（死标签不恢复）')

    // N13：重启恢复 —— 标签集/顺序/类型/diff 模式/激活标签
    const SEED = persisted({
      [S1]: {
        tabs: [
          { id: 'tab1', kind: 'activity' },
          { id: 'tab2', kind: 'file', path: 'src/a.ts', mode: 'diff', toolId: 'tool-1', startLine: 12 },
          { id: 'tab3', kind: 'web', url: TARGETS[0].url, srcPath: TARGETS[0].srcPath },
        ],
        activeId: 'tab2',
      },
    })
    const m = await boot(SEED)
    const st2 = () => m.useAppStore.getState()
    m.useAppStore.setState({ workspace: WS, activeSessionId: S1 }) // 真实链路：会话恢复响应 → activeSessionId
    eq(st2().rightExpanded, false, 'N13 恢复**不展开**右栏（恢复 ≠ 激活）')
    eq(st2().leftExpanded, true, 'N14 恢复**不收**左栏')
    eq(calls.open.length, 0, 'N15 恢复**不创建任何视图**（阶段 3 的休眠语义：视图留给用户点击那一刻）')
    st2().syncTabsToRightTab() // 面板 effect 的收口（真实链路由 RightPanel 的 effect 调用）
    const set2 = st2().tabsByScope[S1]
    eq(set2.tabs.map((t) => t.kind).join(','), 'activity,file,web', 'N16 标签集与顺序恢复')
    eq(set2.activeId, set2.tabs[1].id, 'N17 激活标签恢复（file，不是回落 activity）')
    eq(set2.tabs[1].mode, 'diff', 'N18 file 标签的 diff/source 模式恢复')
    eq(set2.tabs[1].toolId, 'tool-1', 'N19 diff 标签的 toolId 恢复')
    eq(set2.tabs[1].startLine, 12, 'N20 定位行恢复')
    eq(set2.tabs[1].openSeq, 1, 'N21 openSeq 不入存储 → 恢复时从 1 起')
    eq(set2.tabs[2].viewId, undefined, 'N22 web 标签恢复为**待重建**态（没有视图身份）')
    eq(set2.tabs[2].url, TARGETS[0].url, 'N23 url 快照恢复（重建的唯一依据）')
    eq(st2().rightTab, 'file', 'N24 rightTab 反向收敛到恢复的激活标签（而不是把激活标签改成 activity）')
    // 恢复标记的含义 = 「这个 scope 的标签集自本次启动起还没被用户碰过」→ 期间标签集权威
    //（rightTab 此刻只可能是 resetView/loadViewToTop 的默认值）。清标记的时机：已一致、或
    // 任何显式标签动作。故这里仍在。
    eq(set2.restored, true, 'N25 恢复标记仍在（该 scope 还没被用户碰过）')
    const refBefore = st2().tabsByScope
    st2().syncTabsToRightTab() // 第二次：激活标签已与 rightTab 一致 → 清标记
    assert(st2().tabsByScope !== refBefore, 'N26 已一致 → 清掉恢复标记（一次 set）')
    const refAfter = st2().tabsByScope
    st2().syncTabsToRightTab()
    assert(st2().tabsByScope === refAfter, 'N26b 第三次 sync 不 set（防 effect 自激）')
    eq(st2().tabsByScope[S1].restored, false, 'N26c 标记已清（此后 rightTab 恢复权威）')
    // N26d：恢复标记未清的 scope 在「切走再切回」后仍保留自己的激活标签（否则切回来会被
    // 默认的 activity 顶掉 —— 恢复出来的标签集往往没有 activity 标签，sync 会给它补一个）
    const m26 = await boot(persisted({ [S1]: { tabs: [{ id: 'a1', kind: 'git' }], activeId: 'a1' } }))
    m26.useAppStore.setState({ workspace: WS, activeSessionId: S1 })
    m26.useAppStore.getState().syncTabsToRightTab()
    eq(m26.useAppStore.getState().rightTab, 'git', 'N26d 恢复后 rightTab = 标签集的 kind（git）')
    m26.useAppStore.setState({ activeSessionId: S2 })
    m26.useAppStore.getState().syncTabsToRightTab()
    m26.useAppStore.setState({ activeSessionId: S1, rightTab: 'activity' }) // 切回（未水合会话 → resetView 默认值）
    m26.useAppStore.getState().syncTabsToRightTab()
    eq(m26.useAppStore.getState().tabsByScope[S1].tabs.map((t) => t.kind).join(','), 'git', 'N26e 切回来仍是该 scope 自己的标签集（没被默认 activity 顶掉）')
    eq(m26.useAppStore.getState().rightTab, 'git', 'N26f 且 rightTab 仍跟随它')

    // N27：重启后主进程推来的**空 targets** 不得把恢复的 web 标签当「页面已关闭」删掉
    st2().syncWebTabs('native', [], null)
    eq(st2().tabsByScope[S1].tabs.filter((t) => t.kind === 'web').length, 1, 'N27 空 targets 不删待重建的恢复标签（重启后的第一帧正是空的）')
    // N28：激活待重建标签**不**在 store 层建视图（重建点唯一收口在 RightPanel 的 effect，
    // 判据 = 面板展开 + 正在显示 web + 激活标签就是它）
    calls.open.length = 0
    const pendId = st2().tabsByScope[S1].tabs[2].id
    st2().activateTab(pendId)
    eq(calls.open.length, 0, 'N28 activateTab 本身不建视图（视图由渲染层「即将显示」时按需建）')
    eq(st2().tabsByScope[S1].tabs[2].viewId, undefined, 'N29 它仍是待重建态（视图还没建）')

    // N30：认领 —— 重建出来的视图接回**原来那个标签实例**（不多出重复标签）
    st2().syncWebTabs('native', [{ key: 't7', url: TARGETS[0].url, title: 'report.pdf', srcPath: TARGETS[0].srcPath }], 't7')
    const webTabs = st2().tabsByScope[S1].tabs.filter((t) => t.kind === 'web')
    eq(webTabs.length, 1, 'N30 认领后仍只有一个 web 标签（不新增重复标签）')
    eq(webTabs[0].id, pendId, 'N31 认领复用原标签 id（不闪、不影响 activeId）')
    eq(webTabs[0].viewId, 't7', 'N32 认领后变成活标签（viewId = 重建出来的视图）')
    eq(webTabs[0].url, undefined, 'N33 快照清掉（此后以视图池为真相源）')
    // N34：此后它就是一个正常活标签 —— 池里消失即删（阶段 1.5 的既有语义不受影响）
    st2().syncWebTabs('native', [], null)
    eq(st2().tabsByScope[S1].tabs.filter((t) => t.kind === 'web').length, 0, 'N34 认领之后的标签回到正常收敛（池里消失 → 删）')

    // N35：坏数据不炸（截断 JSON / 非法 kind / 旧版本 / 结构不对 / web 缺 url）
    const boot2 = await boot('{"v":1,"scopes":{"s1":{"tabs":[{"id":"a","ki')
    boot2.useAppStore.setState({ workspace: WS, activeSessionId: S1 })
    eq(Object.keys(boot2.useAppStore.getState().tabsByScope).length, 0, 'N35 截断的 JSON → 静默丢弃（不炸、正常启动）')
    const boot3 = await boot(persisted({ [S1]: { tabs: [{ id: 'a', kind: 'bogus' }, { id: 'b', kind: 'git' }, { id: 'c', kind: 'web' }, { id: 'd', kind: 'file', path: 'x.ts', mode: 'nope' }], activeId: 'a' } }))
    boot3.useAppStore.setState({ workspace: WS, activeSessionId: S1 })
    const set3 = boot3.useAppStore.getState().tabsByScope[S1]
    eq(set3.tabs.map((t) => t.kind).join(','), 'git,file', 'N36 非法 kind 丢弃、缺 url 的 web 丢弃、非法 mode 归一化成缺省')
    eq(set3.activeId, set3.tabs[0].id, 'N37 activeId 指向被丢弃的标签 → 回落到第一条（不炸）')
    const boot4 = await boot({ v: 0, scopes: { [S1]: { tabs: [{ id: 'a', kind: 'git' }], activeId: 'a' } } })
    eq(Object.keys(boot4.useAppStore.getState().tabsByScope).length, 0, 'N38 旧版本结构 → 整体丢弃（不做逐字段猜测）')
    const boot5 = await boot({ v: 1, scopes: { [S1]: { tabs: 'nope', activeId: 'a' }, [S2]: { tabs: [{ id: 'a', kind: 'git' }], activeId: 'a' } } })
    eq(Object.keys(boot5.useAppStore.getState().tabsByScope).join(','), S2, 'N39 单个 scope 结构不对 → 只丢那一个 scope（其余照常恢复）')
    const boot6 = await boot({ v: 1, scopes: { [S1]: { tabs: [{ id: 'a', kind: 'web', url: 'x'.repeat(4096) }], activeId: 'a' } } })
    eq(boot6.useAppStore.getState().tabsByScope[S1].tabs.length, 0, 'N40 超长 url（data: 整段 HTML）→ 丢弃（localStorage 配额护栏）')
    // N40b：**已退役的 kind**（2026-10 删掉的 'agent'/任务 tab）—— 老用户机器上存着它，
    // 重启时必须静默丢弃那一条，其余标签照常恢复。不丢弃的表现是：启动后标签条上出现一个
    // 点不动的「任务」标签（kind 不在枚举里 → 没有渲染分支），比少了这一条更难排查。
    const boot6b = await boot(persisted({
      [S1]: { tabs: [{ id: 'a', kind: 'activity' }, { id: 'b', kind: 'agent' }, { id: 'c', kind: 'git' }], activeId: 'b' },
    }))
    boot6b.useAppStore.setState({ workspace: WS, activeSessionId: S1 })
    const set6b = boot6b.useAppStore.getState().tabsByScope[S1]
    eq(set6b.tabs.map((t) => t.kind).join(','), 'activity,git', 'N40b 已退役的 kind（agent）被静默丢弃，其余标签照常恢复')
    eq(set6b.activeId, set6b.tabs[0].id, 'N40c 激活的是被丢弃那条 → 回落到第一条（不炸、不留悬空 activeId）')

    // N41：每 scope 上限淘汰序（丢最旧的，保留最新的；激活标签必须在集合里）
    reset({ activeSessionId: S1 })
    for (let i = 0; i < 25; i++) st().openFile(`src/f${i}.ts`, `tool-${i}`)
    st().flushSessionTabs()
    const capped = readStored().scopes[S1].tabs.filter((t) => t.kind === 'file')
    eq(capped.length, 20, 'N41 每 scope 持久化上限 20（超出丢弃最旧的）')
    eq(capped[capped.length - 1].path, 'src/f24.ts', 'N42 最新的标签一定在（淘汰的是最旧的）')
    assert(!capped.some((t) => t.path === 'src/f0.ts'), 'N43 最旧的被淘汰')
    // 激活的是最旧那个 → 也必须保留（否则重启后「激活标签」恢复不了）
    reset({ activeSessionId: S1 })
    for (let i = 0; i < 25; i++) st().openFile(`src/g${i}.ts`, `tool-${i}`)
    const oldest = st().tabsByScope[S1].tabs.find((t) => t.path === 'src/g0.ts')
    st().activateTab(oldest.id)
    st().flushSessionTabs()
    const capped2 = readStored().scopes[S1]
    eq(capped2.tabs.filter((t) => t.kind === 'file').length, 20, 'N44 上限仍然成立（挤掉最旧的一条给激活标签让位）')
    assert(capped2.tabs.some((t) => t.path === 'src/g0.ts'), 'N45 激活标签即使在淘汰端也保留')
    eq(capped2.activeId, capped2.tabs.find((t) => t.path === 'src/g0.ts').id, 'N46 且它仍是激活项')

    // N47：跨 scope 不串（与阶段 2 的 filePreviews 同构的坑）
    const boot7 = await boot(persisted({
      [S1]: { tabs: [{ id: 'a1', kind: 'git' }], activeId: 'a1' },
      [S2]: { tabs: [{ id: 'b1', kind: 'web', url: 'https://example.com' }], activeId: 'b1' },
    }))
    boot7.useAppStore.setState({ workspace: WS, activeSessionId: S1 })
    boot7.useAppStore.getState().syncTabsToRightTab()
    eq(boot7.useAppStore.getState().tabsByScope[S1].tabs.map((t) => t.kind).join(','), 'git', 'N47 S1 只拿到自己的标签集')
    boot7.useAppStore.setState({ activeSessionId: S2 })
    boot7.useAppStore.getState().syncTabsToRightTab()
    eq(boot7.useAppStore.getState().tabsByScope[S2].tabs.map((t) => t.kind).join(','), 'web', 'N48 S2 也只拿到自己的（两套互不串）')
    eq(boot7.useAppStore.getState().tabsByScope[S1].tabs.map((t) => t.kind).join(','), 'git', 'N49 切走不改动别的 scope')

    // N50：scope 数上限（最近会话优先保留）—— 防「几百个会话各存一排标签」无界增长
    const many = {}
    for (let i = 0; i < 45; i++) many[`sess${i}`] = { tabs: [{ id: `t${i}`, kind: 'git' }], activeId: `t${i}` }
    const boot8 = await boot(persisted(many))
    boot8.useAppStore.setState({ workspace: WS, recentSessions: [{ path: WS, id: 'sess44' }] })
    boot8.useAppStore.setState({ tabsByScope: { ...boot8.useAppStore.getState().tabsByScope } }) // 触发一次写入
    boot8.useAppStore.getState().flushSessionTabs()
    const scopes8 = JSON.parse(mem.get(LS_TABS)).scopes
    eq(Object.keys(scopes8).length, 40, 'N50 持久化的 scope 数封顶 40')
    assert('sess44' in scopes8, 'N51 最近用过的会话优先保留')
  }

  console.log(`\n全部通过：${pass} 项断言\n`)
}

main().catch((e) => {
  console.error(`\n❌ ${e.message}\n`)
  process.exit(1)
})
