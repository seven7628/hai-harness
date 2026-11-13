import type { AppSettings, ProviderSaveInput, MCPServerCfg, ExternalSkillsStatusResult, ExternalSkillsImportResult, FsOpReply, FsImportReply, InputHistoryItem, InputHistoryReply, OfficeKitStatus } from './transport/types'

export {}

declare global {
  interface Window {
    desktop?: {
      startupMark: (phase: string) => Promise<boolean>
      pickWorkspace: () => Promise<string | null>
      homeDir: () => Promise<string>
      createWorkspaceFolder: (parent: string, name: string) => Promise<{ ok: boolean; path?: string; error?: string }>
      startBridge: () => Promise<{ ok: boolean; error?: string }>
      command: (cmd: unknown) => Promise<{ ok: boolean; error?: string }>
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
  }
}
