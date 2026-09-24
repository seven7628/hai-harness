// M0 POC：WebContentsView 内嵌视图 + CDP 驱动通路自证。
// 目的：验证「内嵌 webContents 的 CDP 能被外部客户端驱动」，为路径 B 的
// chrome-devtools-mcp 连接外部 CDP 端点打底。
// 生产将拆为独立模块 + go bridge 双向转发 + 插件 session_mode:embedded。
//
// M5：AI 动作观测（Input/Page 命令 → 透明 overlay 可视化）+ 导航控制与状态推送。
// M6：视图池化——多个真实 WebContents 各持稳定 targetId，调试代理协议 v2
// （cmd/event 带 targetId；open/close/enumerate 控制通道），支持真正的多页面：
// chrome-devtools-mcp 的 new_page/close_page 经 bridge Opener 真开真关，
// 页面切换（activate）把对应视图换入右侧栏面板。
//
// 预览标签语义（2026-09）：**打开过的文件 → 刷新已存在的标签；没开过 → 新开标签**。
// 去重身份 = 源文件路径（srcPath，office 转换产物每次换临时目录，只有源文件认得出是同一个
// 文件）优先、URL 兜底；命中即刷新（同 URL → reload / 换 URL → navigate，见 reloadEmbedTarget /
// navigateEmbedTarget —— 两者都作用**指定**视图，不是激活视图：后台标签也要能刷新内容）。
//
// 阶段 3（2026-09-20，见 docs/RIGHT_PANEL_TABS_PLAN.md §11）：视图池上限从「超限抛错」改为
// **LRU 休眠**——最久没用过的「用户看过」的标签被休眠（销毁渲染进程、保留条目与 id），
// 点标签即恢复；AI 的命令打到休眠条目会先唤醒再执行。用户口径：「上限超出就报错肯定不好，
// 会阻塞 Agent 运行；我建议改为 LRU，只是影响展示不影响 Agent 运行时」。
import { session, WebContentsView } from 'electron'
import type { BrowserWindow } from 'electron'
import * as net from 'node:net'
import { createInterface } from 'node:readline'
import { SemanticAdapter } from './embed_v2'

// browserTrace 已停用（Browser Use 已稳定，不再写 ~/.go-code/browser_trace.log，
// 避免占用磁盘）。保留 API 兼容调用点；函数体 no-op。
export function browserTrace(_msg: string): void {
  /* 已停用：不写文件、不打印 */
}

export interface EmbedRect {
  x: number
  y: number
  width: number
  height: number
}
export interface EmbedTargetInfo {
  id: string
  url: string
  title: string
  loading: boolean
  sid: string      // 新增：归属会话（'' = 自举载体，不属于任何会话）
  visible: boolean // 新增：当前是否真的显示在面板（诊断 + 验收用）
  srcPath: string  // 预览标签的**源文件路径**（'' = 不是文件预览）：office 转换产物每次换
                   // 临时 html_path，URL 认不出「同一个文件」——去重与「刷新已存在标签」都靠它
  // —— 阶段 3：视图池 LRU 休眠（见 docs/RIGHT_PANEL_TABS_PLAN.md §11.4）——
  sleeping: boolean  // 已休眠：渲染进程已销毁，但**条目仍在 targets 里**（标签身份 = 稳定 id）
  restoring: boolean // 正在恢复：休眠条目已重建、首个文档还没加载完（标签显示「正在恢复…」）
}
export interface EmbedState {
  ready: boolean
  visible: boolean
  attached: boolean
  url: string
  title: string
  activeId: string | null
  targets: EmbedTargetInfo[]
}

// 独立 session（partition），隔离 go-code 自身上下文；持久登录 profile 后续映射到此 partition。
const PARTITION = 'persist:gocode-browser'
// 右侧栏区域默认宽；渲染进程可在布局确定后经 embed:resize 覆盖。
const DEFAULT_SIDEBAR_WIDTH = 380
const DEFAULT_TOP = 40
// MAX_EMBED_VIEWS 内嵌视图池上限。每个视图是一个独立渲染进程（与 Chrome 标签同量级），
// 不设上限时用户连续点预览会让内存单调增长（CMU CHI 2021 实测约 25% 用户因标签过多
// 导致浏览器崩溃）。
//
// 阶段 3（用户口径 2026-09-20）：「上限超出就报错肯定不好，会阻塞 Agent 运行；我建议改为
// LRU，只是影响展示不影响 Agent 运行时」。故超限不再抛错，而是把**最久没用过的、用户看过
// 的**视图**休眠**（销毁渲染进程、保留条目与 id，点标签即恢复）。有休眠 + 保护集兜底后
// 「同时活着的渲染进程」显著少于标签数，上限可从 8（JetBrains "no more than 8 tabs" 的
// 经验值）提到 12：超出的部分只是多几个标签（休眠态不占渲染进程）。
const MAX_EMBED_VIEWS = 12
// DORMANT_CMD_GRACE_MS 「AI 最近还在用」的保护窗：窗口内被命令过的视图不参与 LRU 挑选
//（命令打到已休眠的 target 是硬禁忌 —— 见 docs/RIGHT_PANEL_TABS_PLAN.md §11.3/§11.5）。
const DORMANT_CMD_GRACE_MS = 30_000
// DORMANT_RESTORE_TIMEOUT_MS 恢复（重建 + 首个文档加载）的等待上限：等不到也放行，
// 不把用户点击 / AI 命令挂死（真正的失败由后续状态与错误页暴露）。
const DORMANT_RESTORE_TIMEOUT_MS = 5000

interface EmbedEntry {
  id: string
  view: WebContentsView
  navWired: boolean
  attached: boolean
  // §8.1：视图就绪（did-finish-load + debugger attach 完成）。open 控制请求等待它，
  // 消灭「Go 侧拿到 targetId 即发命令、撞上未 attach 窗口」的竞态。
  readyPromise: Promise<void>
  readyResolve: () => void
  destroyed: boolean
  // §1.8.0：主 frame id 缓存（frameNavigated/getResourceTree 时更新；navigate 补发
  // lifecycle 用）。Puppeteer frameNavigated 重建 frame 后旧引用失效，补发必须用
  // **当前**主 frame id 且尽量同步——缓存避免每次 navigate 异步等 getResourceTree。
  mainFrameId: string
  // §1.8.0：Electron 原生 utility world 的 executionContext id（createIsolatedWorld
  // 后 Puppeteer 需要它；Electron 原生事件在命令响应前到 → 被 Puppeteer 丢弃，
  // 补发时用**真实 id** 而非伪造——Puppeteer evaluate 用它发命令，我们剥离后主 world 执行）。
  utilityCtxId: number
  // V2：CDP 语义适配层（协议驱动重构，见 embed_v2.ts）。所有页面级命令/事件
  // 经它收敛为标准 CDP 语义（frame/loaderId/context 追踪 + 拦截补发）。
  adapter: SemanticAdapter
  // V2：adapter 是否已用真实 frameId 初始化（getResourceTree 后置位）
  adapterInited: boolean
  sid: string       // 归属会话
  lastUsed: number  // 最近**显式**使用的序号（0 = 用户从未显式看过：后台产出的视图不参与 LRU 挑选）
  // 阶段 3：最近一次收到 AI 命令的时间（0 = 从未）。LRU 保护集用它判「AI 还在用这一页」
  //（埋点在 cmd case —— Electron 侧能看到 100% 的 AI 命令，不需要 Go 反向推送，见 §11.3）。
  lastCmdAt: number
  // 阶段 3：createEntry 传入的 URL。首个文档提交前 getURL() 是空/about:blank，而休眠条目靠
  // 「休眠时的 URL」重建 —— 没有这个兜底，在加载完成前被驱逐的条目会变成**恢复不回来、也关不掉**
  // 的幽灵（渲染层按 url 过滤标签，用户看不到它）。实测：连开十几个视图时正好落在这个窗口。
  requestedUrl: string
  shown: boolean    // applyPanelVisibility 的判定结果（我们自己记账，Electron 无 getVisible）
  srcPath: string   // 预览的源文件路径（见 EmbedTargetInfo.srcPath）；'' = 非文件预览
}

// DormantInfo 休眠条目的重建信息（阶段 3）：渲染进程已销毁，但**条目与 id 保留**。
// 为什么不给 EmbedEntry.view 置空：见 §11.4 —— 那会让 targetInfos/applyPanelVisibility/
// applyViewBounds/embedStatus/navigateEmbed 等约 10 处都要加分支。
interface DormantInfo {
  id: string
  url: string      // 休眠时的 URL（恢复时重新加载它）
  title: string
  sid: string
  srcPath: string
  lastUsed: number // 保留 LRU 次序：休眠不该让「用户上次看它的时间」归零
}

let winRef: BrowserWindow | null = null
let visible = false
let manualBounds = false
let lastBounds: EmbedRect | null = null

// —— M6：视图池 ——
const views = new Map<string, EmbedEntry>() // 插入顺序 = 创建顺序
// —— 阶段 3：休眠池（LRU 驱逐的落地形态）——
// 键与 views 同域（都是稳定 id），同一时刻一个 id 只可能在其中之一。
const dormant = new Map<string, DormantInfo>()
// 正在恢复的 id（重建后首个文档未加载完）：只用于标签上的「正在恢复…」提示。
const restoringIds = new Set<string>()
// 运行中会话集合（渲染层推送，见 setBusySids）：其视图不得被驱逐 —— 会话在跑任务时
// Agent 随时可能再命令它的页面（§11.3 的保护集）。
let busySids: ReadonlySet<string> = new Set<string>()
let activeId: string | null = null
let targetSeq = 0
let currentSid = ''   // 当前会话（渲染层经 embed:set-session 同步）
let lruSeq = 0        // 单调递增，供 lastUsed

function activeEntry(): EmbedEntry | null {
  return activeId ? views.get(activeId) ?? null : null
}

// createEntry 新建一个内嵌视图（独立 partition、拦截新窗口、CDP attach、导航事件接线）。
// idOverride 只给「休眠恢复」用：恢复必须沿用**原 id**（标签身份 = id，见 §11.4），
// 而默认路径每次递增 targetSeq（新标签）。
function createEntry(url: string, sid = '', srcPath = '', idOverride = ''): EmbedEntry {
  if (!idOverride) targetSeq++
  const id = idOverride || 't' + targetSeq
  const s = session.fromPartition(PARTITION)
  const view = new WebContentsView({
    webPreferences: {
      session: s,
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: true,
      // 关键：关闭后台节流。AI 常在用户看别的会话时开页（视图隐藏），Chromium 默认对
      // 隐藏页面压速 → 渲染线程迟滞 → CDP 初始化命令（Network.enable 等）排队超时，
      // 页面对象建不起来（MCP 页表空、list_pages 返回空）。视图必须随时全速运行。
      backgroundThrottling: false,
    },
  })
  // 深色底色：新建视图首帧（about:blank）不再白屏闪烁，与主题一致
  view.setBackgroundColor('#16171b')
  // 内嵌视图内开新窗口/外链一律不放行（不弹系统浏览器；生产按 url_mode/URLPattern 拦截）。
  view.webContents.setWindowOpenHandler(() => ({ action: 'deny' }))
  let readyResolve!: () => void
  const readyPromise = new Promise<void>((res) => { readyResolve = res })
  const entry: EmbedEntry = { id, view, navWired: false, attached: false, readyPromise, readyResolve, destroyed: false, mainFrameId: '', utilityCtxId: 0, adapterInited: false, adapter: undefined as unknown as SemanticAdapter, sid, lastUsed: 0, lastCmdAt: 0, requestedUrl: url, shown: false, srcPath }
  // V2：创建语义适配层（host 绑定本视图的 debugger/导航/广播）
  entry.adapter = new SemanticAdapter(id, {
    targetId: id,
    emit: (method, params) => {
      broadcastLine({ type: 'event', method, params, targetId: id })
    },
    sendCommand: (method, params) => {
      attachEntryDebugger(entry)
      // 经对象方法调用（勿解构）：原生绑定方法裸调用会抛 Illegal invocation。
      return entry.view.webContents.debugger.sendCommand(method, params) as Promise<unknown>
    },
    navigate: async (url) => {
      void entry.view.webContents.loadURL(url).catch(() => { /* 提交失败由后续状态暴露 */ })
    },
    reload: () => {
      entry.view.webContents.reload()
    },
    viewport: () => {
      if (!lastBounds) return null
      return { width: Math.round(lastBounds.width), height: Math.round(lastBounds.height) }
    },
    log: (msg) => browserTrace(`adapter(${id}) ${msg}`),
  })
  view.webContents.on('did-finish-load', () => {
    attachEntryDebugger(entry)
    injectFxLayer(entry) // 文档就绪后才注入动效层（提前下发会悬挂并阻塞该视图全部 CDP 命令）
    // §8.1：文档已提交即视为「视图就绪」（open 等待释放）；attach 若失败由后续
    // sendCommand 前的 attachEntryDebugger 重试兜底（不阻塞 open 的 5s 兜底）。
    entry.readyResolve()
    // V2：adapter 兜底补发 load（navigate 未走拦截路径时，如用户地址栏导航/reload）。
    // 若 adapter 尚未初始化 frameId，先异步取资源树初始化（统一出口，不走旧补丁）。
    if (!entry.adapterInited) {
      void (async () => {
        try {
          if (entry.destroyed || !entry.attached) return
          const tree = (await entry.view.webContents.debugger.sendCommand('Page.getResourceTree')) as {
            frameTree?: { frame?: { id?: string; url?: string } }
          }
          const fid = tree?.frameTree?.frame?.id ?? ''
          if (fid) {
            entry.mainFrameId = fid
            entry.adapter.init(fid, tree?.frameTree?.frame?.url ?? '')
            entry.adapterInited = true
            browserTrace(`adapter.init(finish) ${entry.id} frameId=${fid}`)
          }
        } catch {
          /* 竞态忽略 */
        }
        entry.adapter.onNativeNav('finish')
      })()
      return
    }
    entry.adapter.onNativeNav('finish')
  })
  view.webContents.on('did-start-loading', () => {
    // V2：导航开始 → adapter 状态机感知（用户地址栏/reload 等非 CDP 路径）
    entry.adapter.onNativeNav('start')
    // §8.4-6：每次导航开始都尝试注入（幂等：已注入则 fxArmed 短路；
    // 新导航的新文档由 addScriptToEvaluateOnNewDocument 自动装，
    // 当前文档若因 attach 时序漏装则在此补）。
    injectFxLayer(entry)
  })
  view.webContents.on('did-fail-load', () => {
    // 加载失败也视为已「提交文档」（错误页可继续操作）；释放 open 等待者
    entry.readyResolve()
    entry.adapter.onNativeNav('fail')
  })
  view.webContents.on('destroyed', () => {
    entry.destroyed = true
    entry.readyResolve() // 释放等待者（open 侧会因 destroyed 报错）
  })
  wireEntryNav(entry)
  wireDebuggerEvents(entry)
  if (lastBounds) view.setBounds(lastBounds)
  if (winRef && !winRef.isDestroyed()) winRef.contentView.addChildView(view)
  view.setVisible(false)
  if (!view.webContents.getURL() || view.webContents.getURL() === 'about:blank') {
    if (url && url !== 'about:blank') void view.webContents.loadURL(url)
    else void view.webContents.loadURL('about:blank')
    // ↑ 必须确保提交一个文档：全新视图不 load 时渲染进程/inspector 不完整，
    //   debugger.sendCommand 会永不落定（实测 Network.enable 30s 无响应），
    //   页面级 CDP 链路从根上瘫痪。
  } else if (url && url !== view.webContents.getURL()) {
    void view.webContents.loadURL(url)
  }
  views.set(id, entry)
  // §8.4-6：每个视图创建即武装 FX 脚本（新文档自动注入），不依赖 did-finish-load 时序
  ensureFxScript(entry)
  return entry
}

function attachEntryDebugger(e: EmbedEntry): void {
  try {
    if (!e.view.webContents.debugger.isAttached()) e.view.webContents.debugger.attach('1.3')
  } catch {
    /* attach 竞态忽略；以 isAttached 为准 */
  }
  e.attached = e.view.webContents.debugger.isAttached()
}

// ensureFxScript 幂等注册「新文档自动注入」FX 脚本（Page.addScriptToEvaluateOnNewDocument）。
// **任何时刻调用都安全**：不依赖当前文档是否已提交（注册只对未来文档生效），
// 也不会悬挂/阻塞 CDP 队列。new_page/激活/导航开始时调用，保证**每个视图从创建起
// 就武装**——新文档（含必应等导航目标）自动带 FX，不再依赖 did-finish-load 的时序。
// 当前已加载文档的安装（Runtime.evaluate）由 did-finish-load → injectFxLayer 负责。
function ensureFxScript(e: EmbedEntry): void {
  if (fxArmed.has(e) || e.destroyed) return
  attachEntryDebugger(e)
  if (!e.attached) return
  e.view.webContents.debugger
    .sendCommand('Page.addScriptToEvaluateOnNewDocument', { source: FX_SOURCE })
    .then(() => {
      fxArmed.add(e)
      browserTrace(`fx.armed ${e.id}`)
    })
    .catch(() => {
      // 注册失败：留待 did-finish-load / 下次导航的 injectFxLayer 重试（含退避）
    })
}

// injectFxLayer 在文档就绪后注入 AI 动效层。**必须**在 did-finish-load 之后调用——
// 文档未就绪（建视图瞬间）经 debugger 下发的命令会永久悬挂，并阻塞该视图后续
// 全部 CDP 命令排队（实测：snapshot/click/evaluate 全部 30s 超时，仅浏览器级幸存）。
// §8.4 修复：fxArmed **成功后才置位**（此前提前置位导致 addScript 失败后永不重试，
// 导航后新文档动效层永远缺失）；失败指数退避重试（1s/2s/4s 共 3 次）。
function injectFxLayer(e: EmbedEntry, attempt = 0): void {
  if (!e.attached) {
    // §8.4-6：open 新建视图的 did-finish-load 可能先于 debugger attach 触发，
    // 旧代码 `!e.attached` 直接 return 且无重试 → 新视图 FX 永远不注入（实测 t2
    // 必应页无 fly）。先主动 attach，仍失败则指数退避重试。
    attachEntryDebugger(e)
    if (!e.attached) {
      if (attempt < 3) {
        setTimeout(() => injectFxLayer(e, attempt + 1), 1000 * Math.pow(2, attempt))
      }
      return
    }
  }
  try {
    if (!fxArmed.has(e)) {
      void e.view.webContents.debugger
        .sendCommand('Page.addScriptToEvaluateOnNewDocument', { source: FX_SOURCE })
        .then(() => {
          fxArmed.add(e) // 成功后才置位：后续导航的 did-finish-load 不再重复注入
          void e.view.webContents.debugger.sendCommand('Runtime.evaluate', { expression: FX_SOURCE }).catch(() => {})
        })
        .catch(() => {
          // 失败：指数退避重试（1s/2s/4s），仍失败放弃（不阻塞主链路）
          if (attempt < 3) {
            const delay = 1000 * Math.pow(2, attempt)
            setTimeout(() => injectFxLayer(e, attempt + 1), delay)
          }
        })
      return
    }
    void e.view.webContents.debugger.sendCommand('Runtime.evaluate', { expression: FX_SOURCE }).catch(() => {})
  } catch {
    /* 视图销毁竞态忽略 */
  }
}

// ensureEmbed 确保池非空（首个视图即默认 target，占位 about:blank）。
function ensureEmbed(win: BrowserWindow): void {
  winRef = win
  if (views.size > 0) return
  const e = createEntry('about:blank')
  activeId = e.id
}

// applyPanelVisibility 把「可见性 + 激活态」应用到全部视图（激活者显示在面板，其余隐藏但保活渲染）。
function applyPanelVisibility(): void {
  for (const [id, e] of views) {
    // e.sid === '' 放行自举视图：它是 attach/枚举载体，需保活（但不显示内容）
    const on = visible && id === activeId && (e.sid === '' || e.sid === currentSid)
    e.shown = on
    e.view.setVisible(on)
  }
}

// pickSessionView 挑本会话该激活的视图（切会话与关闭顺延共用同一套规则，避免漂移）。
function pickSessionView(): string | null {
  const mine = [...views.values()].filter((e) => e.sid === currentSid && e.sid !== '')
  // 优先「用户显式看过」的（lastUsed > 0，取最近看的）；没有则退到本会话最近**创建**的
  //（用户切回会话时总该看到点东西，而不是空白 —— 但绝不能因为后台产出就把
  // 他原先在看的那一页顶掉）。两者都没有 → 自举视图 → null。
  const viewed = mine.filter((e) => e.lastUsed > 0)
  const pool = viewed.length ? viewed : mine
  const pick = pool.sort((a, b) => b.lastUsed - a.lastUsed || Number(b.id.slice(1)) - Number(a.id.slice(1)))[0]
  return pick?.id ?? [...views.values()].find((e) => e.sid === '')?.id ?? null
}

// —— 阶段 3：LRU 驱逐 + 休眠（见 docs/RIGHT_PANEL_TABS_PLAN.md §11）——

// setBusySids 由渲染层推送「正在跑任务的会话」集合（store 的 busySids，口径 = sessionBusy）。
// 为什么要推：Electron 侧看不到会话运行状态，而「会话在跑任务」= Agent 随时可能再命令它的
// 页面 → 那页绝不能被驱逐（命令打到死 target 是硬禁忌）。
export function setBusySids(sids: string[]): void {
  busySids = new Set((sids ?? []).map((s) => String(s ?? '')).filter(Boolean))
}

// restorableUrlOf 该视图「休眠后能重建回来」的 URL：优先当前 URL（已提交的文档），
// 退到 createEntry 时请求的 URL（首个文档还没提交时 getURL() 是空/about:blank —— 见 requestedUrl）。
function restorableUrlOf(e: EmbedEntry): string {
  const cur = e.view.webContents.getURL()
  if (cur && cur !== 'about:blank') return cur
  return e.requestedUrl && e.requestedUrl !== 'about:blank' ? e.requestedUrl : ''
}

// evictCandidateId 挑一个可休眠的视图（= LRU 驱逐的落点）：在**用户显式看过**的
//（lastUsed > 0）视图里取 lastUsed 最小者，跳过全部保护集：
//   · lastUsed === 0 —— AI 刚开 / Agent 后台产出，用户从没看过。驱逐它会让 new_page 刚
//     拿到的 targetId 立刻失效（§11.5 风险表），也是「产出即展示」不被打断的前提。
//   · id === activeId —— 面板正显示它。
//   · Date.now() - lastCmdAt < DORMANT_CMD_GRACE_MS —— AI 最近还在给它下命令。
//   · sid ∈ busySids —— 该会话正在跑任务。
//   · 当前会话仅剩这一个 web 视图 —— 驱逐它会让用户正看的会话面板空掉。
//   · 没有可重建的 URL（about:blank 占位：自举视图 / AI 的空页）—— 休眠了也恢复不回来，
//     而渲染层按 url 过滤标签 → 用户看不到、点不到、关不掉 = 一条谁也碰不到的幽灵。
// 无可驱逐对象时返回 null（调用方决定失败口径：用户点击响亮、背景静默）。
function evictCandidateId(): string | null {
  const now = Date.now()
  const mine = currentSid === '' ? [] : [...views.values()].filter((e) => e.sid === currentSid)
  const lastOfScope = mine.length === 1 ? mine[0].id : ''
  let pick: EmbedEntry | null = null
  for (const e of views.values()) {
    if (e.destroyed) continue
    if (e.lastUsed === 0) continue
    if (e.id === activeId || e.id === lastOfScope) continue
    if (now - e.lastCmdAt < DORMANT_CMD_GRACE_MS) continue
    if (e.sid !== '' && busySids.has(e.sid)) continue
    if (!restorableUrlOf(e)) continue
    if (!pick || e.lastUsed < pick.lastUsed) pick = e
  }
  return pick?.id ?? null
}

// sleepEntry 把视图**休眠**：销毁渲染进程，但把重建所需信息记进 dormant、**保留原 id**。
// 为什么不直接 destroy：标签身份（面板级 web 标签的 viewId）就是这个 id，丢掉它标签会被
// syncWebTabs 当「页面已关闭」删掉（§11.4）；休眠条目仍以原 id 出现在 embed-nav 的 targets
// 里（sleeping: true），标签点一下就重建恢复。
// **不广播 destroyed**：那是「target 真没了」的通知（Go 侧会 RemoveTarget）；休眠只是释放
// 渲染进程，页面在 AI 眼里仍在（list_pages 仍列它，命令会触发唤醒）。
function sleepEntry(e: EmbedEntry): void {
  dormant.set(e.id, {
    id: e.id,
    url: restorableUrlOf(e), // 首个文档未提交时退到「请求的 URL」（否则恢复不回来，见 requestedUrl）
    title: e.view.webContents.getTitle(),
    sid: e.sid,
    srcPath: e.srcPath,
    lastUsed: e.lastUsed,
  })
  views.delete(e.id)
  e.destroyed = true // 阻止并发 CDP 命令继续进入即将关闭的 WebContents
  e.readyResolve()
  try {
    if (winRef && !winRef.isDestroyed()) winRef.contentView.removeChildView(e.view)
  } catch {
    /* 忽略 */
  }
  try {
    if (e.view.webContents.debugger.isAttached()) e.view.webContents.debugger.detach()
  } catch {
    /* 忽略 */
  }
  try {
    e.view.webContents.close()
  } catch {
    /* 已销毁忽略 */
  }
  browserTrace(`embed.sleep ${e.id} url=${dormant.get(e.id)?.url ?? ''}`)
}

// reviveEntry 恢复（重建）一个休眠视图：**同步**建出视图（沿用原 id）+ **异步**加载
//（createEntry 内已 loadURL）；返回「首个文档加载落定」的 Promise，失败上抛 —— 调用方
//（用户点击 / AI 命令）据此报错，而不是拿到「ok 但白面板」。
// 不复用 openEmbedTarget：它默认去重 + 分配新 id，标签身份会断（§11.4）。
function reviveEntry(id: string, urlOverride?: string): Promise<void> {
  const d = dormant.get(id)
  if (!d) return Promise.resolve()
  const url = urlOverride ?? d.url
  dormant.delete(id)
  const e = createEntry(url || 'about:blank', d.sid, d.srcPath, id)
  e.lastUsed = d.lastUsed // 休眠不该改变「用户上次看它的时间」（LRU 次序保持）
  restoringIds.add(id)
  pushEmbedNav() // 标签立刻从「已休眠」变「正在恢复…」
  return new Promise<void>((resolve, reject) => {
    let settled = false
    const settle = (err?: Error): void => {
      if (settled) return
      settled = true
      restoringIds.delete(id)
      pushEmbedNav()
      if (err) reject(err)
      else resolve()
    }
    // 等首个文档落定。did-finish-load/did-fail-load 是**主 frame** 级事件；
    // 超时兜底：等不到也放行（不把用户点击 / AI 命令挂死）。
    e.view.webContents.once('did-finish-load', () => settle())
    e.view.webContents.once('did-fail-load', (_ev, code, desc) => settle(new Error(`恢复页面失败（${code} ${desc}）`)))
    setTimeout(() => settle(), DORMANT_RESTORE_TIMEOUT_MS)
  })
}

function rightRegionRect(win: BrowserWindow): EmbedRect {
  const b = win.getContentBounds()
  return {
    x: Math.max(0, b.width - DEFAULT_SIDEBAR_WIDTH),
    y: DEFAULT_TOP,
    width: DEFAULT_SIDEBAR_WIDTH,
    height: Math.max(0, b.height - DEFAULT_TOP),
  }
}

// isValidEmbedRect rect 合法性：宽高必须达到面板最小可用尺寸。
// 刷新后首次 show 时 renderer 的 getBoundingClientRect 可能算出 0/极小值
//（DOM 未布局完成），直接使用会把视图放到错误位置浮在最上层。
function isValidEmbedRect(rect: Partial<EmbedRect>): boolean {
  return typeof rect.width === 'number' && rect.width >= 80 &&
    typeof rect.height === 'number' && rect.height >= 80 &&
    typeof rect.x === 'number' && typeof rect.y === 'number'
}

// toDip 渲染进程给的 CSS px 矩形 → native 视图用的 DIP。
// 外观设置「字体大小」走 webContents.setZoomFactor（原生缩放），此时 1 CSS px = z DIP：
// 不乘这个系数，放大后内嵌页面会比容器小一圈（偏差 = (z-1) × 位置），缩小则溢出压住主界面。
// 只作用于**渲染进程测出来的**矩形；rightRegionRect 那类由窗口 bounds 直接算的已是 DIP，不乘。
function toDip(win: BrowserWindow | null, rect: EmbedRect): EmbedRect {
  const z = win && !win.isDestroyed() ? win.webContents.getZoomFactor() : 1
  if (!z || z === 1) return rect
  return {
    x: Math.round(rect.x * z),
    y: Math.round(rect.y * z),
    width: Math.round(rect.width * z),
    height: Math.round(rect.height * z),
  }
}

export function showEmbed(win: BrowserWindow, url: string, rect?: Partial<EmbedRect>): EmbedState {
  ensureEmbed(win)
  if (!rect || !isValidEmbedRect(rect)) {
    // rect 缺失或异常（刷新后 DOM 未布局完成时 shotRef 可能算出 0/极小值）：
    // 回退到右侧栏默认区域，避免视图被放到错误位置浮在最上层。
    lastBounds = rightRegionRect(win)
  } else {
    lastBounds = toDip(win, { x: rect.x ?? 0, y: rect.y ?? 0, width: rect.width ?? 0, height: rect.height ?? 0 })
  }
  applyViewBounds(lastBounds)
  // 兼容旧语义：指定 url 且激活视图还在初始页 → 直接导航过去（M6 后多页由 createTarget 走）
  const act = activeEntry()
  if (url && act && (act.view.webContents.getURL() === '' || act.view.webContents.getURL() === 'about:blank')) {
    void act.view.webContents.loadURL(url)
  }
  for (const e of views.values()) attachEntryDebugger(e)
  visible = true
  // 显示路径同样对齐排版视口：内容区尺寸没变过时（收起再展开）ResizeObserver 不会触发，
  // 而「正在显示的那一页」的视口必须与容器一致（见 alignEntryViewport）。
  // 用**调用方给的量测值**（rect）而不是 lastBounds：后者可能是上面的兜底区域，拿它对会排歪。
  alignEntryViewport(
    activeEntry(),
    rect && isValidEmbedRect(rect)
      ? { width: rect.width ?? 0, height: rect.height ?? 0 }
      : undefined,
  )
  applyPanelVisibility()
  pushEmbedNav()
  return embedStatus()
}

// —— M6：多 target 管理（UI / 控制通道共用）——

// openEmbedTarget 新开一个真实视图并立即激活（AI new_page / 未来 UI「+」按钮共用）。
//
// 预览标签语义（2026-09）：带 srcPath 或同 URL 命中**本会话**已有标签时**不新建**，而是把
// 那个标签更新到最新内容（URL 不同 → navigate；相同 → reload）——「打开过的文件：刷新已存在
// 的标签；没开过：新开标签」。去重只在同会话内：跨会话命中会把这个视图直接赋给 activeId /
// currentSid（这里绕过了 activateEmbedTarget 的跨会话拒绝）——那等于把别人的标签搬进本会话，
// 是隔离漏洞（R0 的收口点）。
// 去重身份先比 srcPath（**两侧都有值**时），再比 URL。
export function openEmbedTarget(
  win: BrowserWindow | null,
  rawUrl: string,
  opts?: { sid?: string; activate?: boolean; srcPath?: string; dedupe?: boolean },
): EmbedState {
  const sid = String(opts?.sid ?? '')
  const activate = opts?.activate !== false
  const srcPath = String(opts?.srcPath ?? '')
  const url = rawUrl || 'about:blank'
  if (!win && winRef) win = winRef
  if (!win) throw new Error('窗口未就绪')
  ensureEmbed(win)
  // dedupe:false 只给 MCP 控制通道的 open 用：协议 v2 的 new_page/close_page 是「真开真关」
  // （见文件头），new_page 命中已有标签会让 Puppeteer 的页表把同一个 targetId 记两次。
  // about:blank 不参与去重：它是「占位载体」（自举视图），不是「打开过的文件」——
  // 拿它命中自举视图会把新请求静默变成「复用那个不可见的占位页」。
  if (opts?.dedupe !== false && url && url !== 'about:blank') {
    const hit = [...views.values()].find((e) => !e.destroyed && e.sid === sid &&
      (srcPath && e.srcPath ? e.srcPath === srcPath : e.view.webContents.getURL() === url))
    if (hit) {
      // 刷新已存在的标签：换路径（office 产物）→ 导航；同 URL → 重读磁盘内容。
      const hitUrl = hit.view.webContents.getURL()
      if (hitUrl !== url) void navigateEntry(hit, url).catch(() => { /* 失败由后续状态暴露 */ })
      else reloadEntry(hit)
      // activate 的语义与新建时**逐字相同**（自愈 currentSid + 置 activeId + 递增 lastUsed）；
      // activate:false（Agent 后台产出）一律不碰这三样（不抢视线）。
      if (activate) {
        if (sid !== '') currentSid = sid
        activeId = hit.id
        hit.lastUsed = ++lruSeq
      }
      ensureFxScript(hit)
      injectFxLayer(hit)
      applyPanelVisibility()
      pushEmbedNav()
      return embedStatus()
    }
    // 阶段 3：命中的标签可能**正在休眠**（渲染进程已销毁，但条目与 id 还在）——
    // 恢复它，而不是新开一个重复标签（去重身份对休眠条目必须同样成立）。
    // 恢复是同步建视图 + 异步加载，故这里可以立即返回状态（与上面的活条目命中同序）。
    const dhit = [...dormant.values()].find((d) => d.sid === sid &&
      (srcPath && d.srcPath ? d.srcPath === srcPath : d.url === url))
    if (dhit) {
      void reviveEntry(dhit.id).catch(() => { /* 失败由后续状态（错误页/空白）暴露 */ })
      if (activate) {
        if (sid !== '') currentSid = sid
        activeId = dhit.id // lastUsed 由 reviveEntry 保留，不再递增（同 reloadEmbedTarget 口径）
      }
      applyPanelVisibility()
      pushEmbedNav()
      return embedStatus()
    }
  }
  // 阶段 3（用户口径）：超限**不再抛错** —— 按 LRU 休眠最久没用过的「用户看过」的标签，
  // 让出渲染进程。只有保护集里一个可驱逐对象都没有时才失败（用户显式点击由 store 响亮报错，
  // 背景路径 quiet 静默）——那种形态说明 12 个视图全在使用中，明确失败比偷偷丢更诚实。
  // 注意 ensureEmbed 在前：池为空时它刚创建的默认视图也计入上限，判断口径统一。
  while (views.size >= MAX_EMBED_VIEWS) {
    const victim = evictCandidateId()
    if (!victim) {
      throw new Error(`内嵌视图已达上限 ${MAX_EMBED_VIEWS} 个，且都在使用中（无法休眠），请先关闭一些标签`)
    }
    const ve = views.get(victim)
    if (ve) sleepEntry(ve)
  }
  const e = createEntry(url, sid, srcPath)
  // activate 的语义 =「调用方明确要求现在就显示它」→ 它必然属于当前会话。
  // 故 activate 时以传入的 sid 为准并自愈 currentSid（App 的 effect 可能还没跑；
  // 不自愈就会出现「点了预览但面板不显示」的静默失败）。
  // 反过来 activate:false（Agent 后台产出）**绝不**碰 activeId / currentSid —— 那才是「不抢视线」。
  // ⚠️ 这里**不能**再加「仅当属当前会话才设」之类的条件：AI 经 CDP 开页的调用方靠
  // st.activeId 取新视图 id（见控制通道 'open'），加了条件会让 new_page 拿到空 targetId。
  // lastUsed 只在显式激活时递增：后台产出的视图不参与 LRU，否则用户切走再切回时
  // 会被 Agent 刚开的标签顶掉位置（他原先在看的那一页消失）。
  if (activate) {
    if (sid !== '') currentSid = sid
    activeId = e.id
    e.lastUsed = ++lruSeq
  }
  // §8.4-6：new_page 即武装（createEntry 内已 ensureFxScript），再补一次当前文档安装
  ensureFxScript(e)
  injectFxLayer(e) // 文档可能已就绪（loadURL 同步提交时），幂等补装当前文档
  // 不强制 visible=true：AI 可能在后台会话开页，用户正看别的界面——强制显示会让
  // 白屏/未完成页面浮在无关 UI 上方。可见性由渲染层 embedShow/embedHide（面板真实
  // 状态）驱动；用户切回浏览器会话时 RightPanel effect 会自动 embedShow。
  applyPanelVisibility()
  pushEmbedNav()
  return embedStatus()
}

// —— 按**具体标签**作用的刷新/导航（不是激活视图）——
// 预览命中已有标签时要更新的正是**那个**标签，而它完全可能在后台（Agent 产出 activate:false
// 开出来的），甚至不是当前激活的那个 —— 所以不能复用 navigateEmbed/embedReload（那两个只作用
// activateEntry()）。两者都**不**改 activeId、**不**碰 visible（后台标签的刷新不得把面板点亮、
// 不得抢视线）。

// reloadEntry 重读当前 URL。用 reloadIgnoringCache 而不是 reload：file:// 也会进 Chromium
// 缓存，普通 reload 会出现「刷新了但显示的还是旧内容」——而「看到最新一次产出」正是这个
// 动作存在的全部理由。
function reloadEntry(e: EmbedEntry): void {
  e.view.webContents.reloadIgnoringCache()
}

// navigateEntry 让该视图导航到 target（已归一化的 URL）；不碰激活态与可见性。
async function navigateEntry(e: EmbedEntry, target: string): Promise<void> {
  // loadURL 在 ERR_ABORTED（重定向/被后续导航顶掉）时 reject 属正常路径，其余上抛
  //（同 navigateEmbed：文件不存在时 Chromium 给的是**空白页**，吞掉就成了「ok:true + 白面板」）。
  await e.view.webContents.loadURL(target).catch((err: unknown) => {
    if ((err as { code?: string })?.code === 'ERR_ABORTED') return
    throw err
  })
}

// reloadEmbedTarget 刷新**指定**视图（渲染层「打开过的文件 → 刷新已存在的标签」的落地）。
// 未知 id 明确抛错（调用方要能区分「刷新了」与「那个标签已经不在了」，而不是静默成功）。
// 命中休眠条目 → 重建（重建本身就是一次完整加载 = 刷新）；**不**改激活态与可见性
//（与活条目路径逐字同口径：后台标签的刷新不得把面板点亮、不得抢视线）。
export async function reloadEmbedTarget(id: string): Promise<void> {
  const e = views.get(String(id ?? ''))
  if (e && !e.destroyed) {
    reloadEntry(e)
    return
  }
  const d = dormant.get(String(id ?? ''))
  if (!d) throw new Error(`视图 ${id} 不存在`)
  await reviveEntry(d.id)
}

// navigateEmbedTarget 让**指定**视图导航到新 URL：office 每次转换产出不同的临时 html_path，
// 靠它把新产物落回**同一个**标签（否则每个源文件每预览一次就多一个重复标签）。
export async function navigateEmbedTarget(id: string, rawUrl: string): Promise<void> {
  const key = String(id ?? '')
  const target = normalizeInputUrl(rawUrl)
  const e = views.get(key)
  if (e && !e.destroyed) {
    if (!target) return
    await navigateEntry(e, target)
    pushEmbedNav()
    return
  }
  const d = dormant.get(key)
  if (!d) throw new Error(`视图 ${id} 不存在`)
  if (!target) return
  // 恢复时直接加载**新 URL**（跳过休眠时的旧 URL：多加载一次旧产物既慢又会让标签闪一下旧内容）
  await reviveEntry(key, target)
}

// closeEmbedTarget 关闭并销毁指定视图；关闭的是激活视图时顺延激活最早的剩余视图。
// 休眠条目同样可关（标签上的 ✕ 走这里）：把条目从 dormant 摘掉即可 —— 它与活条目共用
// 同一个 id 空间，对 Go 侧而言都是「target 真没了」，故两条路径都广播 destroyed。
export function closeEmbedTarget(id: string): EmbedState {
  const e = views.get(id)
  const wasDormant = dormant.delete(id)
  if (!e && !wasDormant) return embedStatus()
  if (e) {
    views.delete(id)
    e.destroyed = true // §8.1-7：先标记销毁，阻止并发 CDP 命令继续进入已关闭 WebContents
    e.readyResolve() // 释放 open 等待者（其侧会因 destroyed 报错）
    try {
      if (winRef && !winRef.isDestroyed()) winRef.contentView.removeChildView(e.view)
    } catch {
      /* 忽略 */
    }
    try {
      if (e.view.webContents.debugger.isAttached()) e.view.webContents.debugger.detach()
    } catch {
      /* 忽略 */
    }
    try {
      e.view.webContents.close()
    } catch {
      /* 已销毁忽略 */
    }
  }
  // 阶段 3（P1）：通知 Go 侧「这个 target 真的没了」→ forwarder.RemoveTarget → 广播
  // Target.targetDestroyed + Target.detachedFromTarget（否则 Puppeteer 的页表留着幽灵页，
  // 而 Page.close() 会挂满 300s，见 §11.1/§11.2）。**幂等**：Go 侧 RemoveTarget 对未知 id
  // 早退，重复关闭同一条目不会产生副作用。
  // 为什么不在 destroyEmbed() 里广播：那里先 stopDebuggerProxy 杀掉所有 socket，广播必然
  // 落空（§11.1）。全量清理由下次 attach 的 ResyncTargets 兜住。
  broadcastLine({ type: 'destroyed', targetId: id })
  if (activeId === id) {
    // 优先顺延到**本会话**最近用过的视图；没有则退回自举视图；再没有则 null（面板显示空态）。
    // 原实现取 views.keys().next()（创建最早的那个）——隔离后会挑到别的会话的视图 → 面板空白。
    activeId = pickSessionView()
  }
  applyPanelVisibility()
  pushEmbedNav()
  return embedStatus()
}

// activateEmbedTarget 切换面板显示的视图（用户点页列表 tab）。命中休眠条目 → 先重建
//（**沿用原 id**，见 §11.4）再激活；恢复失败上抛，让渲染层能 toast（不静默白面板）。
export async function activateEmbedTarget(id: string): Promise<EmbedState> {
  const e = views.get(id)
  if (!e) {
    const d = dormant.get(id)
    if (!d) return embedStatus()
    // 跨会话激活拒绝（口径与活条目逐字相同，见下）
    if (d.sid !== '' && d.sid !== currentSid) return embedStatus()
    await reviveEntry(id)
    const revived = views.get(id)
    if (!revived) return embedStatus()
    activeId = id
    revived.lastUsed = ++lruSeq // 用户点开 = 显式使用（LRU 次序随之刷新）
    ensureFxScript(revived)
    injectFxLayer(revived)
    alignEntryViewport(revived, cssBoundsOfPanel())
    if (visible) applyPanelVisibility()
    pushEmbedNav()
    return embedStatus()
  }
  // 跨会话激活拒绝（前端已按会话过滤列表，这里兜底：否则面板会被别的会话的视图顶成空白）
  if (e.sid !== '' && e.sid !== currentSid) return embedStatus()
  activeId = id
  e.lastUsed = ++lruSeq
  ensureFxScript(e)
  injectFxLayer(e)
  alignEntryViewport(e, cssBoundsOfPanel())
  if (visible) applyPanelVisibility()
  pushEmbedNav()
  return embedStatus()
}

// alignEntryViewport 把某个视图的**排版视口**对齐到容器矩形（CSS px —— 调用方给的就是渲染层的
// 量测值 / lastBounds ÷ zoom，两者同口径）。
//
// 为什么必须有这一步（2026-09-21 用户报障「图片/网页无法正常加载，只剩左上角一小块」）：
// 页面排版视口是视图**自己的**状态，只有显式下发 `Emulation.setDeviceMetricsOverride` 才会变，
// 而渲染层那条命令：① 只作用于主进程当前的 activeId；② 只在**尺寸变化**时下发
//（ResizeObserver 驱动）。于是这些路径都会让「正在显示的那一页」留着错误的视口：
//   · 同尺寸下切换标签 → BrowserTab 不重挂载、容器尺寸没变 → 谁也不会补一次对齐；
//   · 面板展开（收起期间渲染层不再下发收缩的矩形，尺寸自然也没变）→ ResizeObserver 同样不触发；
//   · 切标签瞬间：渲染层的对齐命令可能先于 embedActivate 的 IPC 到达 → 打给了**上一个**视图。
// 收口在「谁成为面板当前显示的页」这两条主进程路径（激活 / 显示）上最稳：主进程自己知道是哪个
// 视图，不依赖渲染层时序。
function alignEntryViewport(e: EmbedEntry | null, css: { width: number; height: number } | undefined): void {
  if (!e || e.destroyed || !css) return
  const width = Math.max(1, Math.round(css.width))
  const height = Math.max(1, Math.round(css.height))
  try {
    attachEntryDebugger(e)
    if (!e.attached) return // 未 attach：渲染层下一次同步会补（此处静默，不打扰用户）
    void e.view.webContents.debugger
      .sendCommand('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: false })
      .catch(() => { /* CDP 未就绪/视图正在销毁：忽略 */ })
  } catch {
    /* 忽略：对齐是兜底，不该把显示路径带崩 */
  }
}

// cssBoundsOfPanel：把渲染层最后量测的面板矩形（lastBounds，DIP）换算成 CSS px。
// 只在渲染层**真的量过**（manualBounds）时才返回：否则 lastBounds 是右侧栏默认区域的估算值
//（showEmbed 的兜底），拿它对齐会把页面排歪 —— 真正的量测随后就到，那时按量测对齐。
function cssBoundsOfPanel(): { width: number; height: number } | undefined {
  if (!manualBounds || !lastBounds) return undefined
  const z = winRef && !winRef.isDestroyed() ? winRef.webContents.getZoomFactor() : 1
  return { width: lastBounds.width / (z || 1), height: lastBounds.height / (z || 1) }
}

// applyViewBounds 同步全部视图 bounds（隐藏视图也保持，切回即时呈现）。
function applyViewBounds(rect: EmbedRect): void {
  for (const e of views.values()) e.view.setBounds(rect)
}

// setEmbedBounds 由渲染进程在侧栏布局确定后覆盖（手动定位，窗口 resize 不再自动覆盖）。
export function setEmbedBounds(rect: EmbedRect): void {
  if (views.size === 0) return
  manualBounds = true
  lastBounds = toDip(winRef, { ...rect })
  applyViewBounds(lastBounds)
}

// reflowEmbed 窗口尺寸变化时按右侧区域自动重排（仅当渲染进程未手动定位）。
export function reflowEmbed(): void {
  if (views.size === 0 || !winRef || manualBounds) return
  lastBounds = rightRegionRect(winRef)
  applyViewBounds(lastBounds)
}

export function hideEmbed(): void {
  visible = false
  applyPanelVisibility()
}

// —— M5/M6：导航状态推送（地址栏/前进后退可用性/页签清单）——
// 任一视图的 URL/标题/加载态变化 → 推给渲染进程（React 工具条消费激活视图 + 页签列表）。

export interface EmbedNavState extends EmbedTargetInfo {
  canGoBack: boolean
  canGoForward: boolean
  targets: EmbedTargetInfo[]
  activeId: string | null
}

function targetInfos(): EmbedTargetInfo[] {
  const out: EmbedTargetInfo[] = []
  for (const e of views.values()) {
    out.push({ id: e.id, url: e.view.webContents.getURL(), title: e.view.webContents.getTitle(), loading: e.view.webContents.isLoading(), sid: e.sid, visible: e.shown, srcPath: e.srcPath, sleeping: false, restoring: restoringIds.has(e.id) })
  }
  // 阶段 3：休眠条目**必须**出现在这里（原 id + sleeping: true）—— 渲染层 web 标签的身份
  // 就是这个 id，条目从 targets 消失 = 标签被 syncWebTabs 当「页面已关闭」删掉（§11.4）。
  // url/title 取休眠时的快照（渲染进程已销毁，没有活的 webContents 可读）。
  for (const d of dormant.values()) {
    out.push({ id: d.id, url: d.url, title: d.title, loading: false, sid: d.sid, visible: false, srcPath: d.srcPath, sleeping: true, restoring: false })
  }
  return out
}

function embedNavState(): EmbedNavState {
  const act = activeEntry()
  const wc = act?.view.webContents
  return {
    id: act?.id ?? '',
    url: wc?.getURL() ?? '',
    title: wc?.getTitle() ?? '',
    loading: !!wc?.isLoading(),
    // R0：顶层字段描述**激活视图**（EmbedNavState extends EmbedTargetInfo，新增字段必须补齐）
    sid: act?.sid ?? '',
    visible: act?.shown ?? false,
    srcPath: act?.srcPath ?? '',
    // 激活视图恒为活条目（activeId 在保护集里，休眠永不选它），故这两项恒为 false/当前值
    sleeping: false,
    restoring: !!act && restoringIds.has(act.id),
    canGoBack: !!wc?.navigationHistory.canGoBack(),
    canGoForward: !!wc?.navigationHistory.canGoForward(),
    targets: targetInfos(),
    activeId,
  }
}

// pushEmbedNav 把导航状态推给主窗口渲染层（工具条消费）。兼容旧字段名 url/title。
function pushEmbedNav(): void {
  if (!winRef || winRef.isDestroyed()) return
  winRef.webContents.send('embed-nav', embedNavState())
}

// wireEntryNav 给单个视图接导航事件（幂等）。
function wireEntryNav(e: EmbedEntry): void {
  if (e.navWired) return
  e.navWired = true
  const wc = e.view.webContents
  for (const ev of ['did-start-loading', 'did-stop-loading', 'did-navigate', 'did-navigate-in-page', 'page-title-updated']) {
    wc.on(ev as never, () => {
      pushEmbedNav()
      // 协议 v2：向 Go 侧同步该 target 元数据（转发器刷新 info）
      broadcastLine({
        type: 'state', targetId: e.id,
        url: wc.getURL(), title: wc.getTitle(), loading: wc.isLoading(),
      })
    })
  }
}

// wireDebuggerEvents 把某视图 debugger 的页面级事件打上 targetId 转发给所有 TCP 客户端
// （attach 前注册即可：未 attach 时 debugger 不产事件）。
// V2：事件经 SemanticAdapter.onEvent 标准化（frame/loaderId/context 语义补全 +
// 主动排序）后统一广播；移除手写补丁（emitAfterInit/标准化 frameNavigated 等
// 全部收敛到 adapter）。
function wireDebuggerEvents(e: EmbedEntry): void {
  e.view.webContents.debugger.on('message' as never, (_ev: unknown, method: string, params: unknown) => {
    // 全量事件日志（诊断链路用；所有事件都记，重点看 lifecycle/executionContextCreated）
    const p = (params ?? {}) as { name?: string; frameId?: string; frame?: { id?: string; loaderId?: string; url?: string }; context?: { id?: number; name?: string; auxData?: { frameId?: string } } }
    const ctxName = p.context?.name ?? '-'
    const frameObj = p.frame ? `frame={id:${p.frame.id ?? '-'},loader:${p.frame.loaderId ?? '-'}}` : ''
    browserTrace(`electron.event(${String(method)}) target=${e.id} name=${p.name ?? '-'} frameId=${p.frameId ?? p.context?.auxData?.frameId ?? '-'} ctx=${ctxName} ${frameObj}`.trimEnd())
    // 缓存主 frame id（frameNavigated / executionContextCreated 的主 frame）
    if (method === 'Page.frameNavigated' && typeof (p.frame?.id ?? p.frameId) === 'string' && (p.frame?.id ?? p.frameId)) {
      e.mainFrameId = p.frame?.id ?? p.frameId ?? ''
    }
    // 缓存 utility world 的真实 context id（Electron 原生 createIsolatedWorld 产出；
    // 命令响应前到达会被 Puppeteer 丢弃 → createIsolatedWorld 后我们用真实 id 重放）
    if (method === 'Runtime.executionContextCreated' && p.context?.name?.startsWith('__puppeteer_utility_world__') && typeof p.context.id === 'number') {
      e.utilityCtxId = p.context.id
    }
    // V2：统一出口——adapter 标准化（frameNavigated 内部 initFrame 兜底；
    // lifecycle/context 语义补全；可能补发/改写），统一广播。
    e.adapter.onEvent(String(method), (params ?? {}) as Record<string, unknown>)
  })
}

// embedCommand 向激活的内嵌视图发送 CDP 命令（M0 自证通路保留；渲染层调试用）。
export async function embedCommand(method: string, params?: unknown): Promise<unknown> {
  const act = activeEntry()
  if (!act || !act.attached) throw new Error('内嵌视图未就绪（debugger 未 attach）')
  // 必须经对象方法调用：Electron 的 Debugger.sendCommand 是原生绑定方法，
  // 解构/裸调用会抛 "Illegal invocation: Function must be called on an object of type Debugger"。
  return act.view.webContents.debugger.sendCommand(method, params) as Promise<unknown>
}

// setEmbedSession 切换「当前会话」：重挑本会话的激活视图 + 重算可见性。
// **不销毁任何视图**（保活：切回会话时标签还在，符合多标签心智）。
// 为什么不做「销毁非本会话视图」：Go 侧 Enumerate() 靠真实视图池路由 AI 命令，销毁会打断在跑的会话。
export function setEmbedSession(sid: string): EmbedState {
  currentSid = String(sid ?? '')
  activeId = pickSessionView()
  applyPanelVisibility()
  pushEmbedNav()
  return embedStatus()
}

export function embedStatus(): EmbedState {
  const act = activeEntry()
  return {
    ready: views.size > 0,
    visible,
    attached: !!act?.attached,
    url: act?.view.webContents.getURL() ?? '',
    title: act?.view.webContents.getTitle() ?? '',
    activeId,
    targets: targetInfos(),
  }
}

export function destroyEmbed(): void {
  for (const e of views.values()) {
    try {
      if (winRef && !winRef.isDestroyed()) winRef.contentView.removeChildView(e.view)
    } catch {
      /* 忽略 */
    }
    try {
      if (e.view.webContents.debugger.isAttached()) e.view.webContents.debugger.detach()
    } catch {
      /* 忽略 */
    }
    try {
      e.view.webContents.close()
    } catch {
      /* 忽略 */
    }
  }
  views.clear()
  dormant.clear()
  restoringIds.clear()
  busySids = new Set<string>()
  activeId = null
  targetSeq = 0
  stopDebuggerProxy()
  // 无条件复位模块级状态（即使上方 close 抛错，也保证不再指向已销毁对象）
  winRef = null
  visible = false
  manualBounds = false
  lastBounds = null
}

// —— M5：用户导航控制（迷你工具条：地址栏 / 前进 / 后退 / 刷新）——
// 用户自由操作入口：不经 AI、不经 MCP，直接驱动激活的内嵌视图。

// normalizeInputUrl 补全协议（裸域名 → https://）。
function normalizeInputUrl(raw: string): string {
  const s = raw.trim()
  if (!s) return ''
  if (/^[a-z][a-z0-9+.-]*:/i.test(s)) return s // 已带 scheme（http/https/file…）
  return 'https://' + s
}

export async function navigateEmbed(rawUrl: string): Promise<EmbedState> {
  const act = activeEntry()
  if (!act) throw new Error('内嵌视图未创建')
  const target = normalizeInputUrl(rawUrl)
  if (!target) return embedStatus()
  // loadURL 在 ERR_ABORTED（重定向/取消/被后续导航顶掉）时会 reject，属正常路径不外抛。
  // 但**其余错误必须上抛**：文件不存在时 Chromium 给的是**空白页**（实测 body 为空、
  // 无任何错误文案），这里吞掉就等于「ok:true + 一片白面板 + 无提示」——PDF 预览正好
  // 走这条路径（ArtifactCard 的 PDF 分支 → openHtmlInBrowser），路径失效时用户无从判断
  // 是没点上还是文件没了。上抛后由 embed:navigate 的 catch 转成 {ok:false,error}，
  // 渲染层即可 toast（见 store.openHtmlInBrowser）。
  await act.view.webContents.loadURL(target).catch((e: unknown) => {
    if ((e as { code?: string })?.code === 'ERR_ABORTED') return
    throw e
  })
  visible = true
  applyPanelVisibility()
  pushEmbedNav()
  return embedStatus()
}

export function embedGoBack(): void {
  const act = activeEntry()
  if (act?.view.webContents.navigationHistory.canGoBack()) act.view.webContents.navigationHistory.goBack()
  pushEmbedNav()
}

export function embedGoForward(): void {
  const act = activeEntry()
  if (act?.view.webContents.navigationHistory.canGoForward()) act.view.webContents.navigationHistory.goForward()
  pushEmbedNav()
}

export function embedReload(): void {
  activeEntry()?.view.webContents.reload()
}

// —— M2/M6：debugger TCP 代理（协议 v2 多路复用）——
// 把视图池的各 webContents.debugger 暴露成一条 JSON 行协议 TCP 端点，供 go bridge 的
// MultiClient 连接。协议：
//   命令   → {"type":"cmd","id":N,"method":"...","params":{...},"targetId":"t2"}
//   响应   → {"type":"resp","id":N,"result":{...}} | {"type":"resp","id":N,"error":"..."}
//   事件   → {"type":"event","method":"...","params":{...},"targetId":"t2"}
//   控制   → {"type":"open","rid":N,"url"} ⇒ {"type":"opened","rid":N,"targetId"}
//          → {"type":"close","rid":N,"targetId"} ⇒ {"type":"closed","rid":N}
//          → {"type":"enumerate","rid":N} ⇒ {"type":"targets","rid":N,"targets":[...]}
//   推送   → {"type":"state","targetId","url","title","loading"}
//          → {"type":"destroyed","targetId"}   // 阶段 3：target 真的没了（关闭/驱逐后关闭），
//                                              // Go 侧 RemoveTarget（广播 targetDestroyed +
//                                              // detachedFromTarget）—— 休眠**不**发它
// targetId 缺省路由到当前激活视图（单视图用法完全兼容 v1）。
let dbgServer: net.Server | null = null
const dbgClients = new Set<net.Socket>()

function writeJson(sock: net.Socket, obj: unknown): void {
  sock.write(JSON.stringify(obj) + '\n')
}

// broadcastLine 广播一行 JSON 给所有 TCP 客户端（事件 / state 推送共用）。
function broadcastLine(obj: unknown): void {
  const line = JSON.stringify(obj) + '\n'
  for (const s of dbgClients) s.write(line)
}

// observeAiCommand 在 AI 的 CDP 命令进入内嵌视图前做旁路观测（可视化），不改变命令本身。
// 仅激活视图的动作可视化（overlay 只盖在激活视图上）。
// §8.4-3：click（mousePressed）前先派发 move，让「光标飞过去 + 点击涟漪」可见
//（上游 Locator click 无 mouseMoved 轨迹，§8.4.2）。
function observeAiCommand(entryId: string, method: unknown, params: unknown): void {
  const m = method as string | undefined
  const p = (params ?? {}) as { x?: number; y?: number; type?: string }
  if (m === 'Input.dispatchMouseEvent' && p.type === 'mousePressed' && typeof p.x === 'number' && typeof p.y === 'number') {
    if (entryId === activeId) emitAiAction({ kind: 'move', x: p.x, y: p.y })
  }
  const a = classifyAiAction(m, params)
  if (a && entryId === activeId) emitAiAction(a)
}

function startDbgServer(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer((sock) => {
      dbgClients.add(sock)
      sock.on('close', () => dbgClients.delete(sock))
      sock.on('error', () => dbgClients.delete(sock))
      const rl = createInterface({ input: sock })
      // 处理一行协议消息。**命名函数**（而不是内联箭头）：阶段 3 的「命令打到休眠条目 →
      // 先唤醒再执行」靠**重放**同一条命令行实现（唤醒后重放 = 走正常的活条目路径，
      // 零逻辑分叉；首次进入时休眠分支没有任何副作用，重放是安全的）。
      const onLine = (line: string): void => {
        let req: { type?: string; id?: number; rid?: number; method?: string; params?: unknown; targetId?: string; url?: string }
        try {
          req = JSON.parse(line)
        } catch {
          return
        }
        switch (req?.type) {
          case 'cmd': {
            if (req.id == null) return
            const id = req.targetId ?? activeId ?? ''
            const e = id ? views.get(id) : undefined
            if (!e) {
              // 阶段 3：命令打到**休眠**条目 → 先唤醒（重建 + 加载，沿用原 id）再重放命令。
              // 绝不让 AI 的命令落到死 target：休眠条目仍在 Go 的页表里（list_pages 列它），
              // 直接报「target 不存在」等于让 Agent 的既有页表突然失效 —— 而驱逐只该影响
              // 展示，不该影响 Agent 运行时（用户口径）。恢复失败才回错误（Go 侧能诊断）。
              const d = id ? dormant.get(id) : undefined
              if (!d) {
                writeJson(sock, { type: 'resp', id: req.id, error: `target ${id || '(无)'} 不存在` })
                return
              }
              const rid = req.id
              browserTrace(`electron.cmd(${String(req.method)}) target=${id} 命中休眠条目 → 唤醒后重放`)
              void (async () => {
                try {
                  await reviveEntry(id)
                  if (!views.get(id)) throw new Error(`target ${id} 恢复失败`)
                  onLine(line)
                } catch (er) {
                  writeJson(sock, { type: 'resp', id: rid, error: er instanceof Error ? er.message : String(er) })
                }
              })()
              return
            }
            // 阶段 3（P3）：记「AI 最近用过这一页」的时间 → LRU 保护集（见 evictCandidateId）。
            // Electron 侧能看到 100% 的 AI 命令，不需要 Go 反向推送（§11.3）。
            e.lastCmdAt = Date.now()
            attachEntryDebugger(e)
            observeAiCommand(e.id, req.method, req.params)
            // 全量命令日志（诊断链路用；定位后移除）
            browserTrace(`electron.cmd(${String(req.method)}) target=${id}`)
            // V2：全部命令先经 SemanticAdapter.onCommand 拦截/改写：
            //   - Page.navigate → loadURL 提交即返回 + 响应带 loaderId + 补发 lifecycle
            //   - Page.reload → reload()
            //   - Page.createIsolatedWorld → 透传 + 响应后补发标准 executionContextCreated
            //   - Runtime.evaluate → 剥离 utility contextId
            //   - Emulation.setDeviceMetricsOverride → 改写为真实视口
            //   - 其余 → 透传 debugger（默认路径）
            const cmdParams = (req.params ?? {}) as Record<string, unknown>
            const res = e.adapter.onCommand({ id: req.id, method: String(req.method), params: cmdParams })
            if (res.intercepted && res.response) {
              // 已拦截：adapter 已同步回复（navigate/reload 等）
              browserTrace(`electron.adapter.intercept(${String(req.method)}) ${id}`)
              writeJson(sock, { type: 'resp', id: req.id, result: res.response.result ?? {} })
              // 响应写回后再补发（时序保证：goto 的 watcher 先记录 initialLoaderId，
              // 再收 init 更新 _loaderId → newDocument 判定满足）。
              // **延迟 30ms**：Go bridge 内部 resp 入队（handleWS goroutine）与事件入队
              // （Relay goroutine）是并发的——即使 Electron 侧 TCP 顺序 resp 先，Go 侧
              // 也可能让事件先入 ws 队列（Puppeteer 先收 init 污染 initialLoaderId →
              // newDocument 永不满足 → 60s 超时）。延迟让 resp 充分到达 Puppeteer
              // （watcher 已创建、initialLoaderId 已记录）后再补发，彻底消除竞态。
              if (res.afterResponse) {
                const fn = res.afterResponse
                setTimeout(() => {
                  try {
                    fn()
                  } catch (er) {
                    browserTrace(`electron.adapter.afterResponse FAIL: ${er instanceof Error ? er.message : String(er)}`)
                  }
                }, 30)
              }
              return
            }
            // 透传路径：adapter 可能已改写 params（evaluate 剥离 contextId / 视口改写）
            // 防御：sendCommand 同步抛错（如 Debugger is not attached）或 promise 悬挂
            // （视图无文档时实测永不落定）都不能吞掉 resp——否则 Go 侧 30s 超时、链路假死。
            try {
              const send = e.view.webContents.debugger.sendCommand(String(req.method), cmdParams) as Promise<unknown>
              const t0 = Date.now()
              const settle = new Promise<unknown>((_, rej) =>
                setTimeout(() => rej(new Error('CDP 命令 45s 无响应（视图可能未就绪）')), 45000),
              )
              Promise.race([send, settle])
                .then((result) => {
                  const dt = Date.now() - t0
                  browserTrace(`electron.sendCommand(${String(req.method)}) ${dt}ms`)
                  // V2：透传命令完成回调（createIsolatedWorld → 补发标准 context）
                  e.adapter.onCommandDone({ id: req.id as number, method: String(req.method), params: cmdParams })
                  writeJson(sock, { type: 'resp', id: req.id, result })
                })
                .catch((er: unknown) => {
                  const dt = Date.now() - t0
                  browserTrace(`electron.sendCommand(${String(req.method)}) FAIL ${dt}ms: ${er instanceof Error ? er.message : String(er)}`)
                  writeJson(sock, { type: 'resp', id: req.id, error: er instanceof Error ? er.message : String(er) })
                })
            } catch (er) {
              writeJson(sock, { type: 'resp', id: req.id, error: er instanceof Error ? er.message : String(er) })
            }
            return
          }
          case 'open': {
            const rid = req.rid ?? 0
            browserTrace(`electron.open begin url=${String(req.url ?? 'about:blank')}`)
            const tOpen = Date.now()
            // §8.1-8：等视图就绪（did-finish-load + attach）再应答 opened（async 化）
            void (async () => {
              try {
                // dedupe:false —— 控制通道的 new_page 必须**真**新建（协议 v2「真开真关」）：
                // 命中已有标签会让 Puppeteer 的页表把同一个 targetId 记两次。
                const st = openEmbedTarget(winRef, String(req.url ?? 'about:blank'), { dedupe: false })
                const id = st.activeId ?? ''
                const entry = id ? views.get(id) : undefined
                if (entry) {
                  await Promise.race([
                    entry.readyPromise,
                    new Promise((res) => setTimeout(res, 5000)), // 兜底：5s 未就绪也放行（避免 Go 侧挂死）
                  ])
                  if (entry.destroyed) throw new Error('视图已销毁')
                }
                browserTrace(`electron.open done ${Date.now() - tOpen}ms id=${id}`)
                writeJson(sock, { type: 'opened', rid, targetId: id })
              } catch (er) {
                browserTrace(`electron.open FAIL ${Date.now() - tOpen}ms: ${er instanceof Error ? er.message : String(er)}`)
                writeJson(sock, { type: 'ctl-error', rid, error: er instanceof Error ? er.message : String(er) })
              }
            })()
            return
          }
          case 'close': {
            const rid = req.rid ?? 0
            closeEmbedTarget(String(req.targetId ?? ''))
            writeJson(sock, { type: 'closed', rid })
            return
          }
          case 'enumerate': {
            writeJson(sock, { type: 'targets', rid: req.rid ?? 0, targets: targetInfos() })
            return
          }
        }
      }
      rl.on('line', onLine)
    })
    srv.on('error', (e) => reject(e))
    srv.listen(0, '127.0.0.1', () => {
      dbgServer = srv
      const a = srv.address()
      resolve(typeof a === 'object' && a ? a.port : 0)
    })
  })
}

// startDebuggerProxy 启动 TCP 代理（127.0.0.1:0 随机端口），返回端口号。幂等。
export function startDebuggerProxy(): Promise<number> {
  return new Promise((resolve, reject) => {
    if (dbgServer) {
      const a = dbgServer.address()
      resolve(typeof a === 'object' && a ? a.port : 0)
      return
    }
    startDbgServer().then(resolve, reject)
  })
}

// stopDebuggerProxy 关闭 TCP 代理。
export function stopDebuggerProxy(): void {
  for (const s of dbgClients) s.destroy()
  dbgClients.clear()
  if (dbgServer) {
    dbgServer.close()
    dbgServer = null
  }
}

// —— M5：AI 动作观测 + 透明 overlay 视图 ——
//
// HTML overlay 盖不住原生视图（WebContentsView 永远合成在窗口 DOM 之上），所以
// 「看到 AI 操作鼠标」用第二个 WebContentsView：全透明背景 + 输入穿透
// （setIgnoreInputEvents，鼠标/滚轮/键盘直达下层浏览器视图），只负责画：
//   - AI 光标（跟随 Input.dispatchMouseEvent 坐标平滑移动）
//   - 点击涟漪（mousePressed 按压点 / mouseReleased 扩散环）
//   - 输入气泡（Input.insertText 文本）/ 导航横幅（Page.navigate）

export interface AiAction {
  kind: 'move' | 'down' | 'up' | 'wheel' | 'type' | 'key' | 'nav'
  x?: number
  y?: number
  text?: string
}

// classifyAiAction 把 AI 经 CDP 下发的命令归类为可视化动作（未识别 → null 忽略）。
function classifyAiAction(method: string | undefined, params: unknown): AiAction | null {
  if (!method) return null
  const p = (params ?? {}) as Record<string, unknown>
  if (method === 'Input.dispatchMouseEvent') {
    const x = typeof p.x === 'number' ? p.x : undefined
    const y = typeof p.y === 'number' ? p.y : undefined
    switch (p.type) {
      case 'mouseMoved': return { kind: 'move', x, y }
      case 'mousePressed': return { kind: 'down', x, y }
      case 'mouseReleased': return { kind: 'up', x, y }
      case 'mouseWheel': return { kind: 'wheel', x, y }
      default: return null
    }
  }
  if (method === 'Input.insertText') return { kind: 'type', text: String(p.text ?? '').slice(0, 40) }
  if (method === 'Input.dispatchKeyEvent') {
    // 只呈现带字符/键名的按键（rawDown/up 成对事件无 text，过滤避免双计）
    const t = p.text ?? p.key
    return t ? { kind: 'key', text: String(t).slice(0, 16) } : null
  }
  if (method === 'Page.navigate') return { kind: 'nav', text: String(p.url ?? '') }
  if (method === 'Page.reload') return { kind: 'nav', text: '刷新当前页' }
  return null
}

// emitAiAction 广播一条 AI 动作：主窗口渲染层（徽标/最近动作）+ 页面内动效层
// （光标/涟漪/气泡/横幅，经 CDP Runtime.evaluate 派发到当前激活视图）。
function emitAiAction(a: AiAction): void {
  if (winRef && !winRef.isDestroyed()) winRef.webContents.send('embed-ai-action', a)
  const e = activeEntry()
  if (!e) return
  try {
    attachEntryDebugger(e)
    if (!e.attached) return
    void e.view.webContents.debugger
      .sendCommand('Runtime.evaluate', { expression: `window.__gocodeAiFx&&__gocodeAiFx.dispatch(${JSON.stringify(a)})` })
      .catch(() => {})
  } catch {
    /* 视图销毁竞态忽略 */
  }
}

// FX_SOURCE AI 动效层（注入到目标页面内部执行）：光标跟随 / 点击涟漪 / 输入气泡 / 导航横幅。
// 经 CDP 注入（Page.addScriptToEvaluateOnNewDocument 新文档自动重装 + Runtime.evaluate 当前
// 文档安装）。取代旧 WebContentsView overlay：原生视图无法输入穿透（webContents 无
// setIgnoreInputEvents，CSS pointer-events 对原生命中测试无效），曾导致面板「看得见点不动」。
const fxArmed = new WeakSet<EmbedEntry>()

const FX_SOURCE =
  '(function(){' +
  'if(window.__gocodeAiFx)return;' +
  "var st=document.createElement('style');st.textContent=" +
  JSON.stringify(
    '@keyframes gocodeRip{from{transform:scale(.4);opacity:1}to{transform:scale(3);opacity:0}}' +
      '.gocode-rip{position:fixed;width:14px;height:14px;margin:-7px;border-radius:50%;border:2.5px solid rgba(80,160,255,.95);z-index:2147483646;pointer-events:none;animation:gocodeRip .55s ease-out forwards}' +
      '.gocode-dot{position:fixed;width:10px;height:10px;margin:-5px;border-radius:50%;background:rgba(255,120,60,.85);z-index:2147483646;pointer-events:none}' +
      '.gocode-tail{position:fixed;width:8px;height:8px;margin:-4px;border-radius:50%;background:rgba(120,200,255,.5);z-index:2147483645;pointer-events:none;transition:opacity .12s ease-out}',
  ) +
  ';document.documentElement.appendChild(st);' +
  "var cur=document.createElement('div');" +
  'cur.innerHTML=' +
  JSON.stringify(
    '<svg viewBox="0 0 24 24" style="width:22px;height:22px;filter:drop-shadow(0 1px 2px rgba(0,0,0,.45))"><path d="M5 2l14 11-6.5 1L16 21l-3 1.4-3.4-7.3L5 19z" fill="#fff" stroke="#333" stroke-width="1.2"/></svg>',
  ) +
  ';' +
  "cur.style.cssText='position:fixed;left:-2px;top:-2px;width:22px;height:22px;opacity:0;z-index:2147483646;pointer-events:none;will-change:transform;transition:transform .09s ease-out,opacity .15s';" +
  'document.documentElement.appendChild(cur);' +
  "function mk(cls,css){var d=document.createElement('div');if(cls)d.className=cls;d.style.cssText=(css||'')+';position:fixed;pointer-events:none';document.documentElement.appendChild(d);return d}" +
  "var tip=mk(null,'left:50%;bottom:18px;transform:translateX(-50%);max-width:86%;padding:5px 12px;border-radius:14px;background:rgba(24,26,32,.88);color:#eaf0ff;font:500 12px/1.4 -apple-system,PingFang SC,sans-serif;box-shadow:0 4px 14px rgba(0,0,0,.35);opacity:0;transition:opacity .25s;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;z-index:2147483646');" +
  "var nv=mk(null,'left:50%;top:12px;transform:translateX(-50%);max-width:90%;padding:6px 14px;border-radius:16px;background:rgba(36,110,255,.92);color:#fff;font:600 12px/1.4 -apple-system,PingFang SC,sans-serif;box-shadow:0 4px 16px rgba(20,80,220,.4);opacity:0;transition:opacity .25s;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;z-index:2147483646');" +
  'var tailEls=[];for(var _i=0;_i<6;_i++){var _t=mk("gocode-tail","width:8px;height:8px;margin:-4px;border-radius:50%;background:rgba(120,200,255,.55);transition:opacity .12s ease-out;z-index:2147483645");_t.style.opacity="0";tailEls.push(_t)}' +
  'var idleT=0,tipT=0,navT=0,lx=null,ly=null,flyI=null,flySteps=8,flyN=0,flySX=0,flySY=0,flyDX=0,flyDY=0,hist=[];' +
  'function clamp(v,m){return Math.max(0,Math.min(m,v))}' +
  'function alive(){cur.style.opacity=1;clearTimeout(idleT);idleT=setTimeout(function(){cur.style.opacity=0},4000)}' +
  'function setPos(x,y){cur.style.transform="translate("+x+"px,"+y+"px)"}' +
  'function ease(k){k=1-k;return 1-k*k*k}' +
  'function cancelFly(){if(flyI){clearInterval(flyI);flyI=null}}' +
  'function pushHist(x,y){hist.push([x,y]);if(hist.length>7)hist.shift()}' +
  'function paintTail(){' +
  'if(hist.length<2)return;' +
  'var n=Math.min(6,hist.length-1);' +
  'for(var i=1;i<=n;i++){' +
  'var p=hist[hist.length-1-i];' +
  'var t=tailEls[i-1];' +
  't.style.left=clamp(p[0],innerWidth)+"px";' +
  't.style.top=clamp(p[1],innerHeight)+"px";' +
  't.style.opacity=(0.5*(1-i/n)).toFixed(2);' +
  '}' +
  'for(var j=n;j<6;j++)tailEls[j].style.opacity="0"' +
  '}' +
  'function fly(x,y){' +
  'if(lx==null){setPos(x,y);lx=x;ly=y;pushHist(x,y);return}' +
  'var dx=x-lx,dy=y-ly;' +
  'var d=Math.sqrt(dx*dx+dy*dy);' +
  'if(d<1){lx=x;ly=y;setPos(x,y);pushHist(x,y);return}' +
  'cancelFly();' +
  'flySteps=Math.max(8,Math.min(20,Math.round(d/16)));' +
  'flyN=0;flySX=lx;flySY=ly;flyDX=dx;flyDY=dy;' +
  'flyI=setInterval(function(){' +
  'flyN++;var k=ease(flyN/flySteps);' +
  'var cx=flySX+flyDX*k,cy=flySY+flyDY*k;' +
  'setPos(cx,cy);pushHist(cx,cy);paintTail();' +
  'if(flyN>=flySteps){lx=flySX+flyDX;ly=flySY+flyDY;cancelFly()}' +
  '},16)' +
  '}' +
  'function ripple(x,y){var r=mk("gocode-rip","z-index:2147483646");r.style.left=clamp(x,innerWidth)+"px";r.style.top=clamp(y,innerHeight)+"px";setTimeout(function(){r.remove()},600)}' +
  'function press(x,y){var d=mk("gocode-dot","z-index:2147483646");d.style.left=clamp(x,innerWidth)+"px";d.style.top=clamp(y,innerHeight)+"px";setTimeout(function(){d.remove()},450)}' +
  'function showTip(t){tip.textContent=t;tip.style.opacity=1;clearTimeout(tipT);tipT=setTimeout(function(){tip.style.opacity=0},2000)}' +
  'function toast(t){nv.textContent=t;nv.style.opacity=1;clearTimeout(navT);navT=setTimeout(function(){nv.style.opacity=0},2600)}' +
  'window.__gocodeAiFx={dispatch:function(a){' +
  'var x=a.x||0,y=a.y||0;' +
  'switch(a.kind){' +
  "case 'move':alive();fly(x,y);paintTail();break;" +
  "case 'down':alive();press(x,y);if(!flyI){setPos(x,y);lx=x;ly=y;pushHist(x,y);paintTail()}break;" +
  "case 'up':alive();ripple(x,y);if(!flyI){setPos(x,y);lx=x;ly=y;pushHist(x,y);paintTail()}break;" +
  "case 'wheel':alive();fly(x,y);paintTail();break;" +
  "case 'type':showTip('AI 输入「'+(a.text||'')+'」');break;" +
  "case 'key':showTip('按键 '+(a.text||''));break;" +
  "case 'nav':toast('AI 正在打开 '+(a.text||'新页面'));break;" +
  '}}}' +
  '})()'
