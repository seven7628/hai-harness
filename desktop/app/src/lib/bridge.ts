/**
 * 宿主 ↔ preload 的**私有** bridge（L0 / D6 一次性交接）。
 *
 * 动这里之前先读 docs/creator-mode-v0.1/09-agent-bridge.md §6.1。三条硬规则：
 *
 *   1. preload 只暴露 `window.__haiClaimHostApi` **一个函数**，交接一次 —— 第二次调用返回
 *      null。页面上不再有 `window.desktop`（不是隐藏，是不存在）。
 *   2. `bridge` 是**模块私有常量**：不许挂回 window，不许作为参数/属性传给插件。
 *      插件世界里既没有承载 command/onEvent 的对象，也拿不到这个模块。
 *   3. 插件只能经 `apply({ host })` 拿到 host.command / host.onEvent
 *      （见 surfaces/plugin/loader.ts 的 createHost）。
 *
 * 求值时机 = **模块求值即 claim**，早于任何插件加载。推论：探针不能「先 import 再往
 * window.desktop 塞 fake」—— 注入必须发生在 bundle 求值之前（e2e 探针页都在
 * `await import(...)` 之前注入，正是这个原因）。
 */

import type { AppSettings, ProviderSaveInput, MCPServerCfg, ExternalSkillsStatusResult, ExternalSkillsImportResult, FsOpReply, FsImportReply, InputHistoryItem, InputHistoryReply, OfficeKitStatus, SurfaceIndex, SurfaceCommandReply, SurfaceActionResult, SurfaceAuditReply } from '../transport/types'

export type DesktopApi = {
  startupMark: (phase: string) => Promise<boolean>
  pickWorkspace: () => Promise<string | null>
  homeDir: () => Promise<string>
  createWorkspaceFolder: (parent: string, name: string) => Promise<{ ok: boolean; path?: string; error?: string }>
  startBridge: () => Promise<{ ok: boolean; error?: string }>
  command: (cmd: unknown) => Promise<{ ok: boolean; error?: string }>
  // Surface 插件（PR-14）：索引（只读元数据）+ 宿主只读命令的唯一入口。
  surfaceIndex: () => Promise<SurfaceIndex>
  surfaceCommand: (p: { id: string; name: string; args?: Record<string, unknown>; workspace?: string }) => Promise<SurfaceCommandReply>
  // 插件页（PR-14b）：启用开关 / 卸载（进 .trash，不直接删）/ 审计尾部。
  // 三条都转发到 bridge（写纪律只有 Go 一份）；失败给封闭 reason，不抛。
  surfaceSetEnabled: (id: string, enabled: boolean) => Promise<SurfaceActionResult>
  surfaceRemove: (id: string) => Promise<SurfaceActionResult>
  surfaceAudit: (limit?: number) => Promise<SurfaceAuditReply>
  stopBridge: () => Promise<{ ok: boolean }>
  settingsGet: () => Promise<AppSettings>
  settingsSet: (patch: Partial<AppSettings>) => Promise<{ ok: boolean; error?: string; settings?: AppSettings }>
  // 输入历史本地文件（~/.go-code/input-history.json；与 localStorage 镜像配合）
  inputHistoryGet: (p: { scope: string }) => Promise<InputHistoryReply>
  inputHistorySet: (p: { scope: string; items: InputHistoryItem[] }) => Promise<InputHistoryReply>
  inputHistoryForget: (p: { prefix: string; subtree?: boolean }) => Promise<{ ok: boolean; error?: string }>
  sttTranscribe: (p: { audio: ArrayBuffer; mimeType: string }) => Promise<{ ok: boolean; text?: string; error?: string }>
  externalSkillsStatus: () => Promise<ExternalSkillsStatusResult>
  officeKit: (req?: { refresh?: boolean }) => Promise<{ ok: boolean; kit?: OfficeKitStatus; error?: string }>
  importExternalSkills: () => Promise<{ ok: boolean; result?: ExternalSkillsImportResult; error?: string }>
  pickApp: () => Promise<{ name: string; bundleId?: string } | null>
  egoInstall: () => Promise<{ ok: boolean; error?: string }>
  egoLaunch: () => Promise<{ ok: boolean; error?: string }>
  providerSave: (p: ProviderSaveInput) => Promise<{ ok: boolean; hasKey?: boolean; error?: string }>
  providerRemove: (provider: string) => Promise<{ ok: boolean; error?: string }>
  providerKeyStatus: (provider: string) => Promise<boolean>
  // BR-08：明文 key 只在需要时按 provider 单取（本地测试/改名），不预填到设置表单
  providerKey?: (provider: string) => Promise<string>
  providerCopyKey?: (p: { from: string; to: string }) => Promise<{ ok: boolean; copied?: boolean; error?: string }>
  // OAuth 订阅登录（2026-08 P2）
  oauthLogin: (p: { provider: string }) => Promise<{ ok: boolean; error?: string; data?: Record<string, unknown> }>
  oauthLogout: (p: { provider: string }) => Promise<{ ok: boolean; error?: string; data?: Record<string, unknown> }>
  oauthStatus: (p: { provider: string }) => Promise<{ ok: boolean; error?: string; data?: Record<string, unknown> }>
  oauthPromptAnswer: (p: { provider: string; value: string; request_id?: string }) => Promise<{ ok: boolean; error?: string }>
  mcpSave: (servers: Record<string, MCPServerCfg>) => Promise<{ ok: boolean; error?: string }>
  fsList: (dir: string) => Promise<{ ok: boolean; path: string; entries: { name: string; path: string; isDir: boolean; size?: number }[]; error?: string }>
  fsReadRef: (req: { path: string; workspace: string }) => Promise<{ ok: boolean; content?: string; error?: string }>
  // 与 fsReadRef 同类（渲染进程给路径、主进程读盘），但只读图片、回 data URL 给 <img> 用。
  fsReadImage: (req: { path: string; workspace: string }) => Promise<{ ok: boolean; dataUrl?: string; mime?: string; error?: string }>
  fsStat: (p: string) => Promise<{ ok: boolean; size?: number; isFile?: boolean; error?: string }>
  // 文件树写操作（左栏「工作区文件」）：与 Transport 同名方法一一对应，见 transport/types.ts。
  fsRename: (p: { workspace: string; path: string; name: string }) => Promise<FsOpReply>
  fsMove: (p: { workspace: string; src: string; destDir: string }) => Promise<FsOpReply>
  fsMkdir: (p: { workspace: string; dir: string; name: string }) => Promise<FsOpReply>
  fsCreateFile: (p: { workspace: string; dir: string; name: string }) => Promise<FsOpReply>
  fsDuplicate: (p: { workspace: string; path: string }) => Promise<FsOpReply>
  fsCopy: (p: { workspace: string; src: string; destDir: string }) => Promise<FsOpReply>
  fsImport: (p: { workspace: string; destDir: string; sources: string[] }) => Promise<FsImportReply>
  fsTrash: (p: { workspace: string; path: string }) => Promise<FsOpReply>
  pickFsFile: (opts?: { multi?: boolean; filters?: { name: string; extensions: string[] }[] }) => Promise<string | null | string[]>
  pickFsDir: () => Promise<string | null>
  // 拖拽/选择得到的 File → 磁盘路径（JS 构造的 File 返回空串）
  getPathForFile: (file: File) => string
  applyTheme: (theme: string) => Promise<{ ok: boolean }>
  // 外观设置「字体大小」→ 原生缩放（返回钳制后的实际比例）
  setUiZoom: (scale: number) => Promise<{ ok: boolean; scale?: number }>
  // PR-05b：原生脏标记 + 退出拦截（before-quit 拦一次 → 渲染层复用脏确认框）
  setDirty?: (dirty: boolean) => Promise<{ ok: boolean }>
  quitAnyway?: () => Promise<{ ok: boolean }>
  onQuitRequest?: (cb: () => void) => () => void
  // M0/M2：右侧栏内嵌浏览器视图 + debugger 代理
  embedShow: (p: { url?: string; rect?: { x: number; y: number; width: number; height: number } }) => Promise<{ ok: boolean; error?: string; state?: unknown }>
  embedResize: (rect: { x: number; y: number; width: number; height: number }) => Promise<{ ok: boolean }>
  embedHide: () => Promise<{ ok: boolean }>
  embedCommand: (p: { method: string; params?: unknown }) => Promise<{ ok: boolean; result?: unknown; error?: string }>
  embedStatus: () => Promise<unknown>
  embedDebuggerProxy: () => Promise<{ ok: boolean; endpoint?: string; port?: number; error?: string }>
  embedDebuggerProxyStop: () => Promise<{ ok: boolean }>
  // M5：用户自由导航 + 内嵌视图状态/AI 动作订阅
  embedNavigate: (url: string) => Promise<{ ok: boolean; error?: string }>
  embedBack: () => Promise<{ ok: boolean }>
  embedForward: () => Promise<{ ok: boolean }>
  embedReload: () => Promise<{ ok: boolean }>
  // 剪贴板写入兜底（copyText 消费；见 src/lib/clipboard.ts）
  clipboardWrite: (text: string) => Promise<{ ok: boolean; error?: string }>
  // 本地文件动作（Artifacts 产出卡片）：用系统默认程序打开 / 在 Finder 中显示。
  // 安全：主进程侧只接受绝对路径且拒绝 ~/.go-code（含 settings.json/api_key）。
  openPath: (p: string) => Promise<{ ok: boolean; error?: string }>
  showItemInFolder: (p: string) => Promise<{ ok: boolean; error?: string }>
  // xlsx 工作簿预览（Artifacts/文件栏「表格预览」）：bridge 侧用受管 Python
  // （openpyxl）把工作簿转成自带样式的 HTML，返回产物路径 → 前端送内嵌浏览器 file://。
  // 主进程侧用负 id 直发 bridge 并等待响应（同 externalSkillsStatus 的通道形态；
  // 转换要跑 Python，可能秒级，首次还会先 bootstrap 运行时）。
  watchXlsx: (req: { path: string; workspace: string }) => Promise<{ ok: boolean; html_path?: string; error?: string; renderer?: 'libreoffice' | 'approximate' }>
  // docx 文档 / pptx 演示文稿预览：与 watchXlsx 同一条通路（主进程直发 bridge + 产物路径
  // 给内嵌浏览器），仅 bridge 命令不同。pptx 那条的产物是**近似版式重建**（Python 侧
  // python-pptx 不能渲染），故文档/UI 措辞都要带「近似」，别在前端许诺保真。
  watchDocx: (req: { path: string; workspace: string }) => Promise<{ ok: boolean; html_path?: string; error?: string }>
  watchPptx: (req: { path: string; workspace: string }) => Promise<{ ok: boolean; html_path?: string; error?: string }>
  // R08c 媒体：协议 URL / ffprobe 元信息 / 协议计数器（类型见 src/lib/mediaProbe.ts）
  mediaUrl: (p: { path: string; workspace: string }) => Promise<{ ok: boolean; url?: string; abs?: string; error?: string }>
  mediaProbe: (p: { path: string; workspace: string }) => Promise<{ ok: boolean; error?: string; data?: unknown }>
  mediaStats: () => Promise<{ requests: number; rangeRequests: number; firstByteHits: number; bytesServed: number }>
  /** 工作区移除时回收媒体根（主进程已知工作区清单）。旧主进程没有这个方法 → 可选调用。 */
  workspaceForget?: (p: string) => Promise<{ ok: boolean; error?: string }>
  onEmbedNav: (cb: (state: unknown) => void) => () => void
  onEmbedAiAction: (cb: (action: unknown) => void) => () => void
  // srcPath：预览的源文件路径 —— office 转换产物每次换临时 html_path，只有源文件能认得出
  // 「这是同一个文件」，去重与「刷新已存在的标签」都靠它（见 electron/embed.ts）。
  embedOpenTarget: (url: string, opts?: { sid?: string; activate?: boolean; srcPath?: string }) => Promise<{ ok: boolean; error?: string; state?: unknown }>
  embedSetSession: (sid: string) => Promise<{ ok: boolean; error?: string; state?: unknown }>
  embedActivate: (id: string) => Promise<{ ok: boolean; error?: string }>
  embedCloseTarget: (id: string) => Promise<{ ok: boolean; error?: string }>
  // 阶段 3：运行中会话集合 → 主进程 LRU 保护集（见 electron/embed.ts evictCandidateId）
  embedSetBusySids: (sids: string[]) => Promise<{ ok: boolean }>
  // 刷新/导航**指定**标签（不是激活视图）：预览命中已存在标签时把内容更新到最新
  //（file:// → reload；office 新产物路径 → navigate），见 store.openInBrowserTab。
  embedReloadTarget: (id: string) => Promise<{ ok: boolean; error?: string }>
  embedNavigateTarget: (id: string, url: string) => Promise<{ ok: boolean; error?: string }>
  // 登录同步：一键导入用户 Chrome 登录状态（复制文件、零解密；Chrome 需先退出）
  chromeImportLogin: () => Promise<{ ok: boolean; copied: number; chromeRunning: boolean; error?: string }>
  chromeClearLogin: () => Promise<{ ok: boolean; removed: number; error?: string }>
  onEvent: (cb: (line: string) => void) => () => void
  onExit: (cb: (info: { code: number | null; err: string }) => void) => () => void
  // hub:// 深链（open-url / second-instance argv → 主进程转发）；返回取消订阅。
  onHubDeepLink: (cb: (url: string) => void) => () => void
  // 冷启动/刷新期间排队的深链（先订阅再取，避免错过）。
  hubTakeDeepLinks: () => Promise<string[]>
  // 内存压力信号（2026-11-12，P1）：主进程轮询 process.getSystemMemoryInfo 推来，
  // 单位 KB。渲染进程据此主动降级视图（Chromium tab discarding 的模型）。
  onMemPressure: (cb: (p: { free: number; total: number; swapFree: number; swapTotal: number }) => void) => () => void
}

/** 向 preload 交接：拿不到（浏览器档 / 旧主进程 / 已被别人接管）就是 null。 */
function claimFromPreload(): DesktopApi | null {
  if (typeof window === 'undefined') return null
  const claim = window.__haiClaimHostApi
  if (typeof claim !== 'function') return null
  try {
    return claim() ?? null
  } catch {
    // preload 抛错（旧版主进程、被沙箱拦、二次交接）→ 当成「没有桥」：调用方走 mock
    // 或 localStorage 镜像，绝不因为桥缺失炸掉主流程。
    return null
  }
}

/**
 * 探针兼容开关：**只在非生产构建**成立。
 *
 * 为什么写 `!import.meta.env?.PROD` 而不是 `import.meta.env.DEV`（两者在 vite 里反相）：
 * scripts/ 下的 node 单测用 `esbuild --format=cjs` 打包 store，那里 `import.meta` 是空对象
 * （无 .env）⇒ `!undefined === true`，桩继续生效；vite 生产构建把它静态折成 `!true`，
 * 下面的回退分支与 `__haiTestBridge` 都被 dead-code 掉，**进不了产物**
 * （门禁 e2e/verify_host_boundary.mjs 就盯这一条）。
 */
const probeCompatEnabled = !import.meta.env?.PROD

/**
 * 探针注入的 fake api（e2e 用：探针在 bundle 之前把 `window.desktop` 摆好）。
 *
 * 读到就**立刻摘掉**：fake 只允许被消费一次，之后页面上不残留 `window.desktop` —— 探针
 * 断言「插件世界里 window.desktop 不存在」时看到的就是真应用看到的那件事。
 */
function takeInjectedProbeApi(): DesktopApi | null {
  if (typeof window === 'undefined') return null
  const w = window as Window & { desktop?: DesktopApi }
  const injected = w.desktop
  if (!injected) return null
  try {
    delete w.desktop
  } catch {
    w.desktop = undefined
  }
  return injected
}

/**
 * 宿主唯一入口。Electron 里 = preload 交出来的那个 api 对象；浏览器/手机档 = null
 * （`bridge?.xxx` 保持原来的可选链语义：没有 Electron 时走 mock / localStorage）。
 */
export const bridge: DesktopApi | null = claimFromPreload()
  ?? (probeCompatEnabled ? takeInjectedProbeApi() : null)

// 探针读取口（**仅非生产构建**）：探针以前读 `window.desktop.embedStatus()`，现在读这里。
// 生产构建没有这行（见 probeCompatEnabled 的注释）。
if (probeCompatEnabled && bridge) window.__haiTestBridge = bridge
