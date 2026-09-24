import type { Transport, ExternalSkillsStatus, ExternalSkillsImportResult, FsImageResult, OAuthStatus } from './types'

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
    externalSkillsStatus: async (): Promise<ExternalSkillsStatus> => {
      const result = await d.externalSkillsStatus()
      if (!result.ok || !result.status) throw new Error(result.error ?? '检测外部 Skills 失败')
      return result.status
    },
    importExternalSkills: async (): Promise<{ ok: boolean; result?: ExternalSkillsImportResult; error?: string }> => d.importExternalSkills(),
    pickApp: () => d.pickApp(),
    providerSave: (p) => d.providerSave(p),
    providerRemove: (provider) => d.providerRemove(provider),
    providerKeyStatus: (provider) => d.providerKeyStatus(provider),
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
  }
}
