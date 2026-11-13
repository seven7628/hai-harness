import type { Transport, ExternalSkillsStatus, ExternalSkillsImportResult, FsImageResult, OAuthStatus, OfficeKitStatus } from './types'

// Electron 真实 transport：走 preload 暴露的 IPC → 主进程 → Go bridge stdio
export function electronTransport(): Transport {
  const d = window.desktop! // 仅当 window.desktop 存在时被调用（getTransport 分流）
  return {
    start: () => d.startBridge(), // 幂等启动；workspace 随命令 payload 注入
    pickWorkspace: () => d.pickWorkspace(),
    homeDir: () => d.homeDir(),
    createWorkspaceFolder: (parent, name) => d.createWorkspaceFolder(parent, name),
    settingsGet: () => d.settingsGet(),
    settingsSet: (patch) => d.settingsSet(patch),
    // 兼容「renderer 已更新、主进程还是旧版」：preload 没这些方法时返回 unsupported，
    // 渲染层继续用 localStorage 镜像，绝不因为历史能力缺失炸掉主流程。
    inputHistoryGet: (scope) =>
      typeof d.inputHistoryGet === 'function'
        ? d.inputHistoryGet({ scope })
        : Promise.resolve({ ok: false, error: 'unsupported' }),
    inputHistorySet: (scope, items) =>
      typeof d.inputHistorySet === 'function'
        ? d.inputHistorySet({ scope, items })
        : Promise.resolve({ ok: false, error: 'unsupported' }),
    inputHistoryForget: (prefix, subtree) =>
      typeof d.inputHistoryForget === 'function'
        ? d.inputHistoryForget({ prefix, subtree })
        : Promise.resolve({ ok: false, error: 'unsupported' }),
    externalSkillsStatus: async (): Promise<ExternalSkillsStatus> => {
      const result = await d.externalSkillsStatus()
      if (!result.ok || !result.status) throw new Error(result.error ?? '检测外部 Skills 失败')
      return result.status
    },
    importExternalSkills: async (): Promise<{ ok: boolean; result?: ExternalSkillsImportResult; error?: string }> => d.importExternalSkills(),
    officeKitStatus: async (refresh?: boolean): Promise<OfficeKitStatus> => {
      const result = await d.officeKit({ refresh: refresh === true })
      if (!result.ok || !result.kit) throw new Error(result.error ?? '读取办公组件状态失败')
      return result.kit
    },
    pickApp: () => d.pickApp(),
    providerSave: (p) => d.providerSave(p),
    providerRemove: (provider) => d.providerRemove(provider),
    providerKeyStatus: (provider) => d.providerKeyStatus(provider),
    // 兼容「renderer 已更新、主进程还是旧版」：没有该方法时降级为空串（不预填、不炸主流程）。
    providerKey: (provider) => (typeof d.providerKey === 'function' ? d.providerKey(provider) : Promise.resolve('')),
    providerCopyKey: (from, to) =>
      typeof d.providerCopyKey === 'function'
        ? d.providerCopyKey({ from, to })
        : Promise.resolve({ ok: false, error: 'unsupported' }),
    // OAuth 订阅登录（2026-08 P2）：经主进程 IPC 转发 bridge 命令
    oauthLogin: async (provider) => {
      const r = await d.oauthLogin({ provider })
      return { ok: r.ok, error: r.error }
    },
    oauthLogout: async (provider) => {
      const r = await d.oauthLogout({ provider })
      return { ok: r.ok, error: r.error }
    },
    oauthStatus: async (provider) => {
      const r = await d.oauthStatus({ provider })
      if (!r.ok || !r.data) throw new Error(r.error ?? 'oauth_status 失败')
      return r.data as unknown as OAuthStatus
    },
    oauthPromptAnswer: (provider, value, requestId) => d.oauthPromptAnswer({ provider, value, ...(requestId ? { request_id: requestId } : {}) }),
    mcpSave: (servers) => d.mcpSave(servers),
    fsList: (dir) => d.fsList(dir) as Promise<{ ok: boolean; path: string; entries: { name: string; path: string; isDir: boolean; size?: number }[]; error?: string }>,
    fsReadRef: (req) => d.fsReadRef(req) as Promise<{ ok: boolean; content?: string; error?: string }>,
    fsReadImage: (req) => d.fsReadImage(req) as Promise<FsImageResult>,
    fsStat: (p) => d.fsStat(p),
    // 文件树写操作：workspace 随 payload 注入（主进程据此限定作用域）。
    fsRename: (req) => d.fsRename(req),
    fsMove: (req) => d.fsMove(req),
    fsMkdir: (req) => d.fsMkdir(req),
    fsCreateFile: (req) => d.fsCreateFile(req),
    fsDuplicate: (req) => d.fsDuplicate(req),
    fsCopy: (req) => d.fsCopy(req),
    fsImport: (req) => d.fsImport(req),
    fsTrash: (req) => d.fsTrash(req),
    // 无参 → 主进程按 opts 缺省分支返回 string | null（既有单选语义）；显式 multi → string[]。
    pickFsFile: async () => {
      const r = await d.pickFsFile()
      return typeof r === 'string' ? r : null
    },
    pickFsFiles: async (opts) => {
      const r = await d.pickFsFile({ multi: true, ...(opts?.filters?.length ? { filters: opts.filters } : {}) })
      return Array.isArray(r) ? r : []
    },
    pickFsDir: () => d.pickFsDir(),
    getPathForFile: (file) => d.getPathForFile(file),
    // xlsx 工作簿预览：主进程转发 bridge xlsx_preview（受管 Python 转 HTML）。
    watchXlsx: (req) => d.watchXlsx(req),
    // docx/pptx 预览：与 watchXlsx 同一条通路（主进程转发 bridge docx_preview / pptx_preview）。
    watchDocx: (req) => d.watchDocx(req),
    watchPptx: (req) => d.watchPptx(req),
    sttTranscribe: (audio, mimeType) => d.sttTranscribe({ audio, mimeType }),
    send: (cmd) => d.command(cmd),
    onEvent: (cb) =>
      d.onEvent((payload: string | { lines: string[] }) => {
        // 批量 IPC 解包（FIX_OPTIMIZATION 问题2②）：{ lines } 逐行回调（保序）；单行字符串直接回调
        if (typeof payload === 'object' && payload !== null && Array.isArray(payload.lines)) {
          for (const l of payload.lines) cb(l)
        } else if (typeof payload === 'string') {
          cb(payload)
        }
      }),
    onExit: (cb) => d.onExit(cb),
    // 协议深链：旧主进程/preload 没有这个方法时退化成空订阅（不因能力缺失炸主流程）。
    onHubDeepLink: (cb) =>
      typeof d.onHubDeepLink === 'function' ? d.onHubDeepLink(cb) : () => undefined,
    takeHubDeepLinks: () =>
      typeof d.hubTakeDeepLinks === 'function' ? d.hubTakeDeepLinks() : Promise.resolve([]),
  }
}
