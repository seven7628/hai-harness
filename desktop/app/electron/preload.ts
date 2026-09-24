import { contextBridge, ipcRenderer, webUtils } from 'electron'

export interface BridgeExitInfo {
  code: number | null
  err: string
}

// 与 main.ts 的 AppSettings 对齐（renderer 侧只读消费；写经 settings:set/provider:save/mcp:save）
export interface ProviderCfg {
  base_url: string
  api_key?: string // API key 明文（settings.json provider 段；桌面端 Provider 设置直接写盘）
  models: Record<string, number>
  max_tokens: Record<string, number>
  protocol?: 'chat_completions' | 'responses' | 'anthropic'
  input_types?: Record<string, string[]> // model → 输入类型（默认 ["text"]；多模态 = ["text","image"]）
  auth_type?: 'api_key' | 'oauth' // 鉴权方式（OAuth 订阅登录 2026-08；缺省 api_key）
  usage_input_includes_cache?: boolean // usage 口径标注（input_tokens 是否含缓存；缺省=自动判定）
  cache_ttl_1h?: boolean // 缓存 TTL 标注：true=1h / false=5m（缺省=按端点默认）
}
export interface MCPServerCfg {
  type?: 'stdio' | 'http' | 'sse'
  command?: string
  args?: string[]
  env?: Record<string, string>
  url?: string
  enabled?: boolean
}
export interface GoalAlignmentSettings {
  reminder_rounds: number
}
export interface AppSettings {
  theme: 'dark' | 'light' | 'system'
  lang: 'zh' | 'en'
  // 浏览器引擎（2026-09）：mcp（默认，Browser Use）| ego（ego lite + ego-browser skill）。
  // 引擎级互斥：ego 时 browser 插件工具不注册进 Agent（bridge 读此字段）。
  browser_engine?: 'mcp' | 'ego'
  provider: {
    active: string
    deepseek: ProviderCfg
    openai: ProviderCfg
    opencode: ProviderCfg
    [id: string]: ProviderCfg | string
  }
  mcpServers: Record<string, MCPServerCfg>
  agent?: GoalAlignmentSettings
}

export interface ExternalSkillSource {
  name: string
  root: string
  skills_dir: string
  root_exists: boolean
  skills_dir_exists: boolean
  skill_count: number
}
export interface ExternalSkillsStatus {
  enabled: boolean
  sources: ExternalSkillSource[]
}
export interface ExternalSkillsStatusResult {
  ok: boolean
  status?: ExternalSkillsStatus
  error?: string
}
export interface ExternalSkillImport {
  source: string
  name: string
  target: string
}
export interface ExternalSkillsImportResult {
  status: ExternalSkillsStatus
  imported: ExternalSkillImport[]
}

contextBridge.exposeInMainWorld('desktop', {
  startupMark: (phase: string) => ipcRenderer.invoke('startup-mark', phase),
  pickWorkspace: () => ipcRenderer.invoke('pick-workspace'),
  homeDir: () => ipcRenderer.invoke('home-dir'),
  createWorkspaceFolder: (parent: string, name: string) => ipcRenderer.invoke('create-workspace-folder', parent, name),
  startBridge: () => ipcRenderer.invoke('bridge:start'), // 幂等：已运行则复用，workspace 随命令 payload 传入
  command: (cmd: unknown) => ipcRenderer.invoke('bridge:command', cmd),
  stopBridge: () => ipcRenderer.invoke('bridge:stop'),
  settingsGet: () => ipcRenderer.invoke('settings:get') as Promise<AppSettings>,
  settingsSet: (patch: Partial<AppSettings>) => ipcRenderer.invoke('settings:set', patch) as Promise<{ ok: boolean; error?: string; settings?: AppSettings }>,
  sttTranscribe: (p: { audio: ArrayBuffer; mimeType: string }) => ipcRenderer.invoke('stt:transcribe', p) as Promise<{ ok: boolean; text?: string; error?: string }>,
  externalSkillsStatus: () => ipcRenderer.invoke('external-skills:status') as Promise<ExternalSkillsStatusResult>,
  importExternalSkills: () => ipcRenderer.invoke('external-skills:import') as Promise<{ ok: boolean; result?: ExternalSkillsImportResult; error?: string }>,
  pickApp: () => ipcRenderer.invoke('app:pick') as Promise<{ name: string; bundleId?: string } | null>,
  egoInstall: () => ipcRenderer.invoke('ego:install') as Promise<{ ok: boolean; error?: string }>,
  // 启动已安装的 ego lite（已装未运行时用；open -a，无需 TCC 授权）
  egoLaunch: () => ipcRenderer.invoke('ego:launch') as Promise<{ ok: boolean; error?: string }>,
  providerSave: (p: { provider: string; active?: string; base_url?: string; protocol?: string; models?: Record<string, number>; max_tokens?: Record<string, number>; input_types?: Record<string, string[]>; openai_compat?: { minimal?: boolean; base_path?: string; reasoning_effort?: boolean; max_completion_tokens?: boolean; stream_usage?: boolean; tools?: boolean }; auth_type?: 'api_key' | 'oauth'; usage_input_includes_cache?: boolean | null; cache_ttl_1h?: boolean | null; api_key?: string }) =>
    ipcRenderer.invoke('provider:save', p) as Promise<{ ok: boolean; hasKey?: boolean; error?: string }>,
  providerRemove: (provider: string) => ipcRenderer.invoke('provider:remove', provider) as Promise<{ ok: boolean; error?: string }>,
  providerKeyStatus: (provider: string) => ipcRenderer.invoke('provider:key-status', provider) as Promise<boolean>,
  // OAuth 订阅登录（2026-08 P2）：登录/登出/状态经主进程转发 bridge 命令
  oauthLogin: (p: { provider: string }) => ipcRenderer.invoke('oauth:login', p) as Promise<{ ok: boolean; error?: string; data?: Record<string, unknown> }>,
  oauthLogout: (p: { provider: string }) => ipcRenderer.invoke('oauth:logout', p) as Promise<{ ok: boolean; error?: string; data?: Record<string, unknown> }>,
  oauthStatus: (p: { provider: string }) => ipcRenderer.invoke('oauth:status', p) as Promise<{ ok: boolean; error?: string; data?: Record<string, unknown> }>,
  oauthPromptAnswer: (p: { provider: string; value: string }) => ipcRenderer.invoke('oauth:prompt-answer', p) as Promise<{ ok: boolean; error?: string }>,
  mcpSave: (servers: Record<string, MCPServerCfg>) =>
    ipcRenderer.invoke('mcp:save', servers) as Promise<{ ok: boolean; error?: string }>,
  fsList: (dir: string) => ipcRenderer.invoke('fs:list', dir),
  fsReadRef: (req: { path: string; workspace: string }) => ipcRenderer.invoke('fs:read-ref', req),
  // 与 fsReadRef 同类（渲染进程给路径、主进程读盘），但只读图片、回 data URL 给 <img> 用。
  fsReadImage: (req: { path: string; workspace: string }) => ipcRenderer.invoke('fs:read-image', req) as Promise<{ ok: boolean; dataUrl?: string; mime?: string; error?: string }>,
  fsStat: (p: string) => ipcRenderer.invoke('fs:stat', p) as Promise<{ ok: boolean; size?: number; isFile?: boolean; error?: string }>,
  // 无 opts 时保持既有返回（string | null）；opts.multi 时返回 string[]（取消 → []）。
  pickFsFile: (opts?: { multi?: boolean; filters?: { name: string; extensions: string[] }[] }) =>
    ipcRenderer.invoke('fs:pick-file', opts) as Promise<string | null | string[]>,
  pickFsDir: () => ipcRenderer.invoke('fs:pick-dir'),
  // getPathForFile 把拖拽/选择得到的 File 还原为磁盘路径（Electron 32+ 官方方式，
  // 取代已废弃的 File.path 属性）。仅磁盘文件有路径；JS 构造的 File 返回空串。
  // 必须在 preload 调用（renderer 侧无此 API），故经 contextBridge 暴露。
  getPathForFile: (file: File) => webUtils.getPathForFile(file),
  applyTheme: (theme: string) => ipcRenderer.invoke('settings:apply-theme', theme),
  // 外观设置「字体大小」：原生缩放（1 = 100%）。缩小/放大都要过范围钳制，主进程再兜一次。
  setUiZoom: (scale: number) => ipcRenderer.invoke('ui:set-zoom', scale),
  // M0 POC：右侧栏内嵌浏览器视图（WebContentsView + CDP 驱动通路）
  embedShow: (p: { url?: string; rect?: { x: number; y: number; width: number; height: number } }) => ipcRenderer.invoke('embed:show', p),
  embedResize: (rect: { x: number; y: number; width: number; height: number }) => ipcRenderer.invoke('embed:resize', rect),
  embedHide: () => ipcRenderer.invoke('embed:hide'),
  embedCommand: (p: { method: string; params?: unknown }) => ipcRenderer.invoke('embed:command', p),
  embedStatus: () => ipcRenderer.invoke('embed:status'),
  embedDebuggerProxy: () => ipcRenderer.invoke('embed:debugger-proxy'),
  embedDebuggerProxyStop: () => ipcRenderer.invoke('embed:debugger-proxy-stop'),
  // M5：用户自由导航 + 内嵌视图状态/AI 动作订阅
  embedNavigate: (url: string) => ipcRenderer.invoke('embed:navigate', url),
  embedBack: () => ipcRenderer.invoke('embed:back'),
  embedForward: () => ipcRenderer.invoke('embed:forward'),
  embedReload: () => ipcRenderer.invoke('embed:reload'),
  // 本地文件动作（Artifacts 产出卡片）：系统默认程序打开 / 在 Finder 中显示。
  // 剪贴板写入兜底：复制动作统一走这里（主进程 clipboard 模块，不吃 renderer 权限判定）。
  // 见 main.ts 的 'clipboard:write' 与 src/lib/clipboard.ts 的 copyText()。
  clipboardWrite: (text: string) => ipcRenderer.invoke('clipboard:write', text) as Promise<{ ok: boolean; error?: string }>,
  openPath: (p: string) => ipcRenderer.invoke('shell:open-path', p) as Promise<{ ok: boolean; error?: string }>,
  showItemInFolder: (p: string) => ipcRenderer.invoke('shell:show-item', p) as Promise<{ ok: boolean; error?: string }>,
  // xlsx 工作簿预览：bridge 侧转 HTML（受管 Python + openpyxl），返回产物路径。
  watchXlsx: (req: { path: string; workspace: string }) => ipcRenderer.invoke('xlsx:preview', req) as Promise<{ ok: boolean; html_path?: string; error?: string }>,
  // docx 文档 / pptx 演示文稿预览：与 watchXlsx 是同一条通路（同样的 invoke 形态与返回），
  // 只是主进程侧换 bridge 命令（mammoth / python-pptx）。调用方见 store 的 previewOfficeFile。
  watchDocx: (req: { path: string; workspace: string }) => ipcRenderer.invoke('docx:preview', req) as Promise<{ ok: boolean; html_path?: string; error?: string }>,
  watchPptx: (req: { path: string; workspace: string }) => ipcRenderer.invoke('pptx:preview', req) as Promise<{ ok: boolean; html_path?: string; error?: string }>,
  onEmbedNav: (cb: (state: unknown) => void) => {
    const listener = (_e: unknown, state: unknown) => cb(state)
    ipcRenderer.on('embed-nav', listener)
    return () => ipcRenderer.removeListener('embed-nav', listener)
  },
  onEmbedAiAction: (cb: (action: unknown) => void) => {
    const listener = (_e: unknown, action: unknown) => cb(action)
    ipcRenderer.on('embed-ai-action', listener)
    return () => ipcRenderer.removeListener('embed-ai-action', listener)
  },
  // M6：多页面视图池（真开/切换/关闭真实 target）
  // srcPath = 预览的**源文件路径**（office 转换产物每次换临时 html_path，只有源文件认得出
  // 「同一个文件」）→ 命中已有标签时主进程刷新它而不是新建（见 embed.ts openEmbedTarget）。
  embedOpenTarget: (url: string, opts?: { sid?: string; activate?: boolean; srcPath?: string }) => ipcRenderer.invoke('embed:open-target', url, opts),
  embedSetSession: (sid: string) => ipcRenderer.invoke('embed:set-session', sid),
  embedActivate: (id: string) => ipcRenderer.invoke('embed:activate', id),
  embedCloseTarget: (id: string) => ipcRenderer.invoke('embed:close-target', id),
  // 阶段 3：运行中会话集合（store 的 busySids）→ 主进程视图池 LRU 保护集：这些会话的标签
  // 不得被休眠（会话在跑任务时 Agent 随时可能再命令它的页面）。
  embedSetBusySids: (sids: string[]) => ipcRenderer.invoke('embed:set-busy-sids', sids) as Promise<{ ok: boolean }>,
  // 刷新/导航**指定**标签（不是激活视图）：预览命中已存在标签时用 —— file:// 内容变了要 reload，
  // office 换产物要 navigate；后台标签（Agent 产出）也必须能更新，故不能走 embedReload/embedNavigate。
  embedReloadTarget: (id: string) => ipcRenderer.invoke('embed:reload-target', id) as Promise<{ ok: boolean; error?: string }>,
  embedNavigateTarget: (id: string, url: string) => ipcRenderer.invoke('embed:navigate-target', id, url) as Promise<{ ok: boolean; error?: string }>,
  // 登录同步：一键导入用户 Chrome 登录状态（复制文件、零解密；只报成功/失败）
  chromeImportLogin: () => ipcRenderer.invoke('chrome:import-login') as Promise<{ ok: boolean; copied: number; chromeRunning: boolean; error?: string }>,
  chromeClearLogin: () => ipcRenderer.invoke('chrome:clear-login') as Promise<{ ok: boolean; removed: number; error?: string }>,
  onEvent: (cb: (payload: string | { lines: string[] }) => void) => {
    const listener = (_e: unknown, payload: string | { lines: string[] }) => cb(payload)
    ipcRenderer.on('bridge:event', listener)
    return () => ipcRenderer.removeListener('bridge:event', listener)
  },
  onExit: (cb: (info: BridgeExitInfo) => void) => {
    const listener = (_e: unknown, info: BridgeExitInfo) => cb(info)
    ipcRenderer.on('bridge:exit', listener)
    return () => ipcRenderer.removeListener('bridge:exit', listener)
  },
})

declare global {
  interface Window {
    desktop: {
      pickWorkspace: () => Promise<string | null>
      homeDir: () => Promise<string>
      createWorkspaceFolder: (parent: string, name: string) => Promise<{ ok: boolean; path?: string; error?: string }>
      startBridge: () => Promise<{ ok: boolean; error?: string }>
      command: (cmd: unknown) => Promise<{ ok: boolean; error?: string }>
      stopBridge: () => Promise<{ ok: boolean }>
      settingsGet: () => Promise<AppSettings>
      settingsSet: (patch: Partial<AppSettings>) => Promise<AppSettings>
      sttTranscribe: (p: { audio: ArrayBuffer; mimeType: string }) => Promise<{ ok: boolean; text?: string; error?: string }>
      externalSkillsStatus: () => Promise<ExternalSkillsStatusResult>
      importExternalSkills: () => Promise<{ ok: boolean; result?: ExternalSkillsImportResult; error?: string }>
      pickApp: () => Promise<{ name: string; bundleId?: string } | null>
      providerSave: (p: { provider: string; active?: string; base_url?: string; protocol?: string; models?: Record<string, number>; max_tokens?: Record<string, number>; input_types?: Record<string, string[]>; openai_compat?: { minimal?: boolean; base_path?: string; reasoning_effort?: boolean; max_completion_tokens?: boolean; stream_usage?: boolean; tools?: boolean }; auth_type?: 'api_key' | 'oauth'; usage_input_includes_cache?: boolean | null; cache_ttl_1h?: boolean | null; api_key?: string }) => Promise<{ ok: boolean; hasKey?: boolean; error?: string }>
      providerKeyStatus: (provider: string) => Promise<boolean>
      mcpSave: (servers: Record<string, MCPServerCfg>) => Promise<{ ok: boolean; error?: string }>
      fsList: (dir: string) => Promise<{ ok: boolean; path: string; entries: { name: string; path: string; isDir: boolean; size?: number }[]; error?: string }>
      fsReadRef: (req: { path: string; workspace: string }) => Promise<{ ok: boolean; content?: string; error?: string }>
      fsReadImage: (req: { path: string; workspace: string }) => Promise<{ ok: boolean; dataUrl?: string; mime?: string; error?: string }>
      fsStat: (p: string) => Promise<{ ok: boolean; size?: number; isFile?: boolean; error?: string }>
      pickFsFile: (opts?: { multi?: boolean; filters?: { name: string; extensions: string[] }[] }) => Promise<string | null | string[]>
      pickFsDir: () => Promise<string | null>
      // 拖拽/选择得到的 File → 磁盘路径（JS 构造的 File 返回空串）
      getPathForFile: (file: File) => string
      watchXlsx: (req: { path: string; workspace: string }) => Promise<{ ok: boolean; html_path?: string; error?: string }>
      applyTheme: (theme: string) => Promise<{ ok: boolean }>
      embedShow: (p: { url?: string; rect?: { x: number; y: number; width: number; height: number } }) => Promise<{ ok: boolean; error?: string; state?: unknown }>
      embedResize: (rect: { x: number; y: number; width: number; height: number }) => Promise<{ ok: boolean }>
      embedHide: () => Promise<{ ok: boolean }>
      embedCommand: (p: { method: string; params?: unknown }) => Promise<{ ok: boolean; result?: unknown; error?: string }>
      embedStatus: () => Promise<unknown>
      embedDebuggerProxy: () => Promise<{ ok: boolean; endpoint?: string; port?: number; error?: string }>
      embedDebuggerProxyStop: () => Promise<{ ok: boolean }>
      embedNavigate: (url: string) => Promise<{ ok: boolean; error?: string }>
      embedBack: () => Promise<{ ok: boolean }>
      embedForward: () => Promise<{ ok: boolean }>
      embedReload: () => Promise<{ ok: boolean }>
      // 本地文件动作（Artifacts 产出卡片）
      openPath: (p: string) => Promise<{ ok: boolean; error?: string }>
      showItemInFolder: (p: string) => Promise<{ ok: boolean; error?: string }>
      onEmbedNav: (cb: (state: unknown) => void) => () => void
      onEmbedAiAction: (cb: (action: unknown) => void) => () => void
      embedOpenTarget: (url: string, opts?: { sid?: string; activate?: boolean; srcPath?: string }) => Promise<{ ok: boolean; error?: string; state?: unknown }>
      embedSetSession: (sid: string) => Promise<{ ok: boolean; error?: string; state?: unknown }>
      embedActivate: (id: string) => Promise<{ ok: boolean; error?: string }>
      embedCloseTarget: (id: string) => Promise<{ ok: boolean; error?: string }>
      embedSetBusySids: (sids: string[]) => Promise<{ ok: boolean }>
      // 刷新/导航**指定**标签（不是激活视图）；命中已有预览标签时由 store 调用
      embedReloadTarget: (id: string) => Promise<{ ok: boolean; error?: string }>
      embedNavigateTarget: (id: string, url: string) => Promise<{ ok: boolean; error?: string }>
      onEvent: (cb: (payload: string | { lines: string[] }) => void) => () => void
      onExit: (cb: (info: BridgeExitInfo) => void) => () => void
    }
  }
}
