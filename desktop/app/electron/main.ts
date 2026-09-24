import { app, BrowserWindow, ipcMain, dialog, safeStorage, session, shell, net, clipboard } from 'electron'
import { spawn, spawnSync, execFile, ChildProcessWithoutNullStreams } from 'child_process'
import * as readline from 'readline'
import * as path from 'path'
import { pathToFileURL } from 'url'
import * as fs from 'fs'
import * as os from 'os'
// M0 POC：右侧栏内嵌浏览器视图（WebContentsView + CDP 驱动通路）
import { showEmbed, setEmbedBounds, reflowEmbed, hideEmbed, embedCommand, embedStatus, destroyEmbed, startDebuggerProxy, stopDebuggerProxy, navigateEmbed, embedGoBack, embedGoForward, embedReload, reloadEmbedTarget, navigateEmbedTarget, openEmbedTarget, activateEmbedTarget, closeEmbedTarget, setEmbedSession, setBusySids } from './embed'
// 登录同步：一键导入用户 Chrome 登录状态（读+解密 → 写入 Electron partition；见 chrome-import.ts）
import { importCookiesToPartition } from './chrome-import'
import type { ChromeCookie as ImportableChromeCookie } from './chrome-cookies-read'
import { augmentPathWithNode } from './node-path'
import { isMainWindowPermissionAllowed } from './permission-policy'
// 敏感目录判定（.ssh/.aws 等）：@ 引用读取与本地图片通道共用同一份，见 fs-guard.ts
import { isSensitiveFsPath } from './fs-guard'
// Markdown 预览的本地图片：绝对路径 → data URL（README 里的 <img src="brand/x.svg">）
import { readImageAsDataUrl } from './read-image'

let mainWindow: BrowserWindow | null = null
let bridge: ChildProcessWithoutNullStreams | null = null

// —— 启动打点（TEMP-STARTUP：定位黑屏/启动慢；**已停用**，不再写 electron-startup.log）——
function startupMark(_phase: string): void {
  /* 已停用：启动诊断已完成，不写文件 */
}

type BridgeCommandResponse = {
  event_type?: string
  id?: number
  ok: boolean
  error?: string
  data?: Record<string, unknown>
}

type PendingBridgeRequest = {
  resolve: (response: BridgeCommandResponse) => void
  timer: NodeJS.Timeout
}

// Electron 主进程自己发给 bridge 的请求使用负 id，避免与 renderer store 的正数命令 id 混淆。
const pendingBridgeRequests = new Map<number, PendingBridgeRequest>()
let bridgeRequestSeq = -1

function settleBridgeRequests(error: string): void {
  for (const [id, request] of pendingBridgeRequests) {
    clearTimeout(request.timer)
    pendingBridgeRequests.delete(id)
    request.resolve({ ok: false, error })
  }
}

// —— 多实例/数据目录覆盖（2026-09：本地双开验证 mesh 联调用）——
// HAI_DATA_DIR 非空 = 「便携数据根」：该目录作为本实例的配置根（内含 .go-code/settings.json
// 与本地会话数据），userData（localStorage）也按它隔离 → 同一台机器可同时跑多个互不干扰
// 的客户端（各自 HOME/配置/节点 id 独立）。用于本地起两个客户端验证 Session Mesh 通信。
// 桥进程（Go）读 $HOME/.go-code —— 因此 spawn bridge 时把 HOME 指向 HAI_DATA_DIR，
// Electron 侧 cfgDir 同步指向 <HAI_DATA_DIR>/.go-code，两侧读写同一份配置。
const MULTI_DATA_DIR = process.env.HAI_DATA_DIR || (process.env.HAI_MULTI === '1' ? path.join(os.tmpdir(), 'hai-multi-' + process.pid) : '')

// —— 统一 userData 目录（2026-08-18 固定；2026-09-23 改名 hai-desktop）：dev（`electron .`，
// 取 package.json name）与打包态（electron-builder productName = HAI）默认落在不同的 Application
// Support 子目录，导致 localStorage（工作区项目树 / 活跃工作区 / 会话标题覆盖 / 界面偏好）互不
// 相通——打包版首次启动表现为「之前的配置、工作区记录、对话历史全都没了」。此处显式固定为
// hai-desktop：dev 与打包共享同一份数据；构建期已在 dev 产生的工作区/会话数据无需迁移即可被
// 打包版读取。多实例模式（HAI_DATA_DIR）：userData 追加数据目录后缀，localStorage 按实例隔离。
function unifyUserData(): void {
  const base = app.getPath('appData')
  const suffix = MULTI_DATA_DIR ? `-${hashDataDir(MULTI_DATA_DIR)}` : ''
  const target = path.join(base, `hai-desktop${suffix}`)
  // 改名迁移：目标目录还没有 localStorage 时，按新旧顺序整体迁入一次旧目录 ——
  //   go-code-desktop*：2026-09-23 之前的名字，改名必须继承，否则用户的工作区/标题/偏好全丢
  //   HAI：更早的打包版 productName 目录
  if (!fs.existsSync(path.join(target, 'Local Storage'))) {
    const legacies = [`go-code-desktop${suffix}`, 'HAI'].map((n) => path.join(base, n))
    for (const legacy of legacies) {
      if (!fs.existsSync(legacy)) continue
      try {
        fs.mkdirSync(path.dirname(target), { recursive: true })
        fs.cpSync(legacy, target, { recursive: true })
        break
      } catch {
        /* 迁移失败：忽略（统一目录从空开始，不影响启动） */
      }
      // 说明：旧目录**不删除**。若迁移时有旧实例在运行，复制内容可能不完整 ——
      // 旧目录原样保留，下次目标仍无 localStorage 时会再试一次。
    }
  }
  app.setPath('userData', target)
}
unifyUserData()

// hashDataDir 数据目录 → userData 后缀（短稳定 hash；多实例隔离用）。
function hashDataDir(dir: string): string {
  let h = 0
  for (let i = 0; i < dir.length; i++) h = ((h << 5) - h + dir.charCodeAt(i)) | 0
  return Math.abs(h).toString(36)
}

// 运行目录：dev 时 __dirname = <app>/dist-electron
const APP_ROOT = path.resolve(__dirname, '..')
const BRIDGE_BIN = path.join(APP_ROOT, 'dist', 'hai-bridge')

// 内置 Browser Use 组件目录（vendored chrome-devtools-mcp，随包分发）：
// 打包态 → Contents/Resources/chrome-devtools-mcp（electron-builder extraResources）；
// dev → 仓库 desktop/vendor/chrome-devtools-mcp。spawnBridge 以 env 传给 Go bridge。
function bundledBrowserVendorDir(): string {
  if (app.isPackaged) {
    return path.join(process.resourcesPath, 'chrome-devtools-mcp')
  }
  const repoVendor = path.resolve(APP_ROOT, '..', 'vendor', 'chrome-devtools-mcp')
  return repoVendor
}

// 内置 Computer Use helper 源目录（随包分发，dev = 仓库编译产物）：
// 打包态 → Contents/Resources/computer-helper（electron-builder extraResources）；
// dev → 仓库 desktop/computer-helper（含已编译 computer-helper 二进制）。
// spawnBridge 以 env GO_CODE_COMPUTER_HELPER_DIR 传给 Go bridge，后者启动时
// 同步到 ~/.go-code/plugins/computer/（受管组件目录）。
function bundledComputerHelperDir(): string {
  if (app.isPackaged) {
    return path.join(process.resourcesPath, 'computer-helper')
  }
  return path.resolve(APP_ROOT, '..', 'computer-helper')
}

// 内置 ego-browser skill 目录（ego lite 集成，2026-09；随包分发）：
// 打包态 → Contents/Resources/ego-browser-skill（electron-builder extraResources）；
// dev → 仓库 desktop/vendor/ego-browser-skill。含 SKILL.md + scripts/install.sh
// （安装引导用）+ learnings/references。
function bundledEgoSkillDir(): string {
  if (app.isPackaged) {
    return path.join(process.resourcesPath, 'ego-browser-skill')
  }
  return path.resolve(APP_ROOT, '..', 'vendor', 'ego-browser-skill')
}

// 内置办公技能集合目录（docx/xlsx/pdf/pptx，2026-09；随包分发）：
// 打包态 → Contents/Resources/office-skills（electron-builder extraResources）；
// dev → 仓库 desktop/vendor/office-skills。目录下每个子目录 = 一个技能（各含 SKILL.md）。
// 传集合目录而非每技能一个 env：新增技能只加子目录，Go/TS 接线都不动。
function bundledOfficeSkillsDir(): string {
  if (app.isPackaged) {
    return path.join(process.resourcesPath, 'office-skills')
  }
  return path.resolve(APP_ROOT, '..', 'vendor', 'office-skills')
}

// activateEgoLite 把 ego lite 窗口带到前台（open -a，LaunchServices 路径）。
// 不用 osascript `tell application to activate`：那需要宿主对目标 app 的 AppleEvent
// 自动化授权（TCC：系统设置 → 隐私 → 自动化），首启未授权会失败；open -a 拉起/
// 聚焦已注册 app 无需 TCC 授权（实测 shell 与子进程均可用，且 app 未运行时能拉起）。
// 激活后把结果事件送回 renderer（toast 提示切 Space）。激活失败也回传（前端可见）。
function activateEgoLite(message: string): void {
  const appName = 'ego lite'
  execFile('/usr/bin/open', ['-a', appName], { timeout: 5000 }, (err) => {
    const ok = !err
    mainWindow?.webContents.send('bridge:event', JSON.stringify({
      event_type: 'ego_activate_status',
      ok,
      ...(ok ? { message } : { error: err?.message ?? 'ego lite 激活失败' }),
    }))
  })
}

// bridgeEnv 构造 spawn env：继承 process.env + provider key 注入 + PATH 补 node
// 目录（GUI 启动 PATH 缺 node 时）。多实例模式：HOME 指向数据根（见 MULTI_DATA_DIR）。
function bridgeEnv(): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...process.env, ...loadKeysEnv() }
  if (MULTI_DATA_DIR) env.HOME = MULTI_DATA_DIR
  // 内置 Browser Use 组件目录（vendored chrome-devtools-mcp）：打包态 = Resources/
  // chrome-devtools-mcp；dev = 仓库 desktop/vendor/chrome-devtools-mcp。Go bridge 据此
  // 同步组件到 ~/.go-code/plugins/browser/（安装=复制，不再 npm 下载）。
  env.GO_CODE_BROWSER_VENDOR_DIR = bundledBrowserVendorDir()
  // 内置 Computer Use helper 源目录（随包分发）：dev = 仓库 desktop/computer-helper；
  // 打包 = Resources/computer-helper。Go bridge 据此同步到 ~/.go-code/plugins/computer/。
  env.GO_CODE_COMPUTER_HELPER_DIR = bundledComputerHelperDir()
  // 内置 ego-browser skill 源目录（随包分发）：dev = 仓库 desktop/vendor/ego-browser-skill；
  // 打包 = Resources/ego-browser-skill。Go bridge 据此同步到 ~/.agents/skills/ego-browser
  // （不存在才复制——未装 ego lite 的机器也能发现技能；已存在尊重 ego onboarding symlink）。
  env.GO_CODE_EGO_SKILL_DIR = bundledEgoSkillDir()
  // 内置办公技能集合目录（随包分发）：dev = 仓库 desktop/vendor/office-skills；
  // 打包 = Resources/office-skills。Go bridge 据此把每个技能子目录同步到
  // ~/.agents/skills/<name>（不存在才复制）——这些技能此前只活在工作区
  // .agents/skills，换 workspace 或打包分发就丢了；同步到全局层后跨工作区可用。
  // （「已存在即跳过」是逐技能判断的：新增一个技能目录能到达已装用户，改既有技能内容则到不了。）
  env.GO_CODE_OFFICE_SKILLS_DIR = bundledOfficeSkillsDir()
  // GUI（Finder/Dock）启动的 PATH 通常不含用户 shell 配置（Homebrew/nvm 等）：
  // bridge 的 exec.LookPath("node") 找不到 → 误报「未检测到 Node.js」。PATH 上
  // 没有 node 时，把探测到的 node 所在目录前插（已有则不动——用户 PATH 优先）。
  // 探测逻辑见 node-path.ts（pathForNode/augmentPathWithNode，纯函数可单测）。
  const augmented = augmentPathWithNode(env.PATH)
  if (augmented !== env.PATH) env.PATH = augmented
  return env
}

// —— 全局设置持久化（~/.go-code/）——
// settings.json：非密钥配置（theme/lang/provider 配置 + 模型窗口 map）+ provider api_key 明文
// （用户决策 2026-08-16：桌面端有 Provider 设置面板，API key 直接明文持久化在 settings.json
// provider 段，bridge 启动时 loadSettings 读取；不再依赖 keys.json 加密注入）。
// keys.json（safeStorage 加密）仅保留旧版本遗留读取（loadKeysEnv 兜底），新写入一律落 settings.json。
// 多实例模式（HAI_DATA_DIR）：配置根随数据目录（<dir>/.go-code，与桥 HOME 对齐）。
const cfgDir = () => MULTI_DATA_DIR
  ? path.join(MULTI_DATA_DIR, '.go-code')
  : path.join(os.homedir(), '.go-code')
const SETTINGS_FILE = () => path.join(cfgDir(), 'settings.json')
const KEYS_FILE = () => path.join(cfgDir(), 'keys.json')

interface OpenAICompatCapabilities {
  minimal?: boolean
  base_path?: string
  reasoning_effort?: boolean
  max_completion_tokens?: boolean
  stream_usage?: boolean
  tools?: boolean
}

interface ProviderCfg {
  base_url: string
  openai_compat?: OpenAICompatCapabilities
  api_key?: string // API key 明文（2026-08-16 起持久化于此，替代 keys.json 加密注入）
  models: Record<string, number> // model → context window（tokens；1M=1000000）
  max_tokens: Record<string, number> // model → 单次输出上限（tokens；DeepSeek v4-flash 官方 384k）
  // 每模型用户价表覆盖（USD/1M tokens；缺省/全 0 = 用内置注册表价表）——与 types.ts ModelPrices 同构
  prices?: Record<string, { input: number; cache_read: number; cache_write: number; output: number }>
  protocol?: 'chat_completions' | 'responses' | 'anthropic' // OpenAI 兼容接入协议；responses/anthropic 暂未支持（默认 chat_completions）
  input_types?: Record<string, string[]> // model → 输入类型（默认 ["text"]；多模态 = ["text","image"] → 允许图片上传）
  auth_type?: 'api_key' | 'oauth' // 鉴权方式（2026-08 OAuth 订阅登录；缺省 api_key）
  // usage 口径标注：input_tokens 是否已含 cache_read/cache_write（bridge loadSettings 读；缺省=自动判定）
  usage_input_includes_cache?: boolean
  cache_ttl_1h?: boolean // 缓存 TTL 标注：true=1h / false=5m（缺省=按端点默认）
  deleted?: boolean // 用户已删除（内置 5 家删除标记；bridge 重启不复活）
}
// MCPServerCfg 单台 MCP 服务器配置（与 bridge mcp 包 ServerConfig 同构；env 明文存 settings.json——v1 决策）
interface MCPServerCfg {
  type?: 'stdio' | 'http' | 'streamable_http' | 'sse'
  command?: string
  args?: string[]
  env?: Record<string, string>
  url?: string
  enabled?: boolean
}
// 命令沙箱（macOS Seatbelt / sandbox-exec）：mode none=进程隔离直通 / seatbelt=沙箱。
interface SandboxSettings {
  mode: 'none' | 'seatbelt'
  sensitive_paths?: string[]
}
// permissions 设置段：仅存 Computer Use 已授权 App（统一授权门禁；设置 → 插件 →
// Computer Use 维护）。旧 allowed_apps（open 白名单）已随「权限」设置页移除（2026-09）。
interface PermissionsSettings {
  computer_approved_apps: string[]
}
interface GoalAlignmentSettings {
  reminder_rounds: number
}
interface RuntimeSettings {
  stop_background_on_interrupt?: boolean // 主停止是否连后台任务一起停（缺省 true；S3-B）
}
// 语音输入（STT）配置：OpenAI 兼容 /audio/transcriptions 端点（与 types.ts STTSettings 同构）
interface STTSettings {
  enabled?: boolean
  base_url?: string
  model?: string
  api_key?: string
  vendor?: string // 当前服务商预设 id（仅 UI 回显；转写只用 base_url/model/api_key）
  base_urls?: Record<string, string> // 各服务商手改过的端点（按厂商分组，仅 UI 回显）
  custom_models?: Record<string, string[]> // 用户自定义模型名（按厂商分组，仅 UI 回显）
}
interface AppSettings {
  theme: 'dark' | 'light' | 'system'
  lang: 'zh' | 'en'
  provider: {
    active: string // 当前活跃 provider id
    deepseek: ProviderCfg
    openai: ProviderCfg
    opencode: ProviderCfg
    kimi: ProviderCfg
    zhipu: ProviderCfg
    [id: string]: ProviderCfg | string // 用户自定义 OpenAI provider（key = id）
  }
  mcpServers: Record<string, MCPServerCfg> // MCP 服务器配置（bridge 启动/热应用读取）
  sandbox?: SandboxSettings // 命令沙箱（缺省 seatbelt；桌面端默认开启 2026-08-16 决策）
  permissions?: PermissionsSettings // Computer Use 已授权 App（空 = 全部拒绝）
  agent?: GoalAlignmentSettings // Agent 运行时目标一致性提醒；0 = 禁用
  runtime?: RuntimeSettings // 运行期行为开关（主停止后台任务语义）
  stt?: STTSettings // 语音输入（话筒）配置；配置齐全才显示话筒
}

// 内置五卡（不可删除；其余为自定义 provider）
const BUILTIN_PROVIDERS = ['deepseek', 'openai', 'opencode', 'kimi', 'zhipu'] as const
// isValidProviderName 自定义 provider 命名校验（与 bridge validProviderName 同规则）：
// 非空、非保留键 "active"，仅字母/数字/短横/下划线且首字符为字母。
function isValidProviderName(name: string): boolean {
  if (!name || name === 'active') return false
  return /^[A-Za-z][A-Za-z0-9_-]*$/.test(name)
}

const DEFAULT_SETTINGS: AppSettings = {
  theme: 'dark',
  lang: 'zh',
  provider: {
    active: 'deepseek',
    deepseek: {
      base_url: 'https://api.deepseek.com/v1',
      models: { 'deepseek-v4-flash': 1000000, 'deepseek-v4-pro': 1000000, 'deepseek-v4-flash-vision-exp': 1000000, 'deepseek-chat': 64000 },
      max_tokens: { 'deepseek-v4-flash': 384000, 'deepseek-v4-pro': 384000, 'deepseek-v4-flash-vision-exp': 384000, 'deepseek-chat': 8192 },
      protocol: 'chat_completions',
      input_types: { 'deepseek-v4-flash-vision-exp': ['text', 'image'] }, // DeepSeek 视觉模型
    },
    openai: {
      base_url: 'https://api.openai.com/v1',
      models: { 'gpt-4o': 128000 },
      max_tokens: { 'gpt-4o': 16384 },
      protocol: 'chat_completions',
      // GPT-4o 系列多模态（text+image）：Provider 设置可逐模型切换
      input_types: { 'gpt-4o': ['text', 'image'], 'gpt-4o-mini': ['text', 'image'], 'gpt-4.1': ['text', 'image'] },
    },
    opencode: {
      base_url: 'https://opencode.ai/zen/go/v1',
      models: { 'deepseek-v4-flash': 1000000, 'deepseek-v4-pro': 1000000, 'deepseek-v4-flash-vision-exp': 1000000, 'glm-5.1': 202752, 'glm-5.2': 1000000, 'glm-5.3': 1000000, 'glm-5.3-flash': 1000000, 'kimi-k3': 1048576, 'kimi-k2.7-code': 262144, 'kimi-k2.6': 262144, 'kimi-k2.5': 262144, 'mimo-v2.6-pro': 1048576, 'mimo-v2.6-flash': 1048576, 'mimo-v2.5': 1000000, 'mimo-v2.5-pro': 1048576, 'hy3': 256000, 'hy4-preview': 1024000, 'qwen3.6-plus': 1000000, 'qwen3.7-max': 1000000, 'qwen3.7-plus': 1000000, 'qwen3.8-max': 1000000, 'minimax-m2.7': 204800, 'minimax-m3': 1000000, 'gpt-5.6-luna': 1050000, 'grok-4.6': 500000, 'muse-spark-1.2-contributor': 1048576, 'longcat-2.0': 1000000 },
      max_tokens: { 'deepseek-v4-flash': 384000, 'deepseek-v4-pro': 384000, 'deepseek-v4-flash-vision-exp': 384000, 'glm-5.1': 32768, 'glm-5.2': 131072, 'glm-5.3': 131072, 'glm-5.3-flash': 131072, 'kimi-k3': 131072, 'kimi-k2.7-code': 262144, 'kimi-k2.6': 65536, 'kimi-k2.5': 262144, 'mimo-v2.6-pro': 131072, 'mimo-v2.6-flash': 131072, 'mimo-v2.5': 128000, 'mimo-v2.5-pro': 128000, 'hy3': 64000, 'hy4-preview': 64000, 'qwen3.6-plus': 65536, 'qwen3.7-max': 65536, 'qwen3.7-plus': 65536, 'qwen3.8-max': 131072, 'minimax-m2.7': 131072, 'minimax-m3': 131072, 'gpt-5.6-luna': 128000, 'grok-4.6': 500000, 'muse-spark-1.2-contributor': 131072, 'longcat-2.0': 131072 },
      protocol: 'chat_completions',
      input_types: {}, // 默认纯文本；多模态模型可在 Provider 设置里勾选「图片」
    },
    kimi: {
      base_url: 'https://api.moonshot.cn/v1', // Kimi 月之暗面（境内）；境外可改 https://api.moonshot.ai/v1
      models: { 'kimi-k2.6': 262144, 'kimi-k2.7-code': 262144, 'kimi-k2.7-code-highspeed': 262144, 'kimi-k2.5': 262144, 'kimi-k2-thinking': 262144, 'kimi-k3': 1048576 },
      max_tokens: { 'kimi-k2.6': 262144, 'kimi-k2.7-code': 262144, 'kimi-k2.7-code-highspeed': 262144, 'kimi-k2.5': 262144, 'kimi-k2-thinking': 262144, 'kimi-k3': 131072 },
      protocol: 'chat_completions',
      input_types: {}, // 默认纯文本；多模态模型可在 Provider 设置里勾选「图片」
    },
    zhipu: {
      base_url: 'https://open.bigmodel.cn/api/paas/v4', // 智谱 Zhipu（ZAI/BigModel），版本根 /api/paas/v4
      models: { 'glm-4.6': 204800, 'glm-4.7': 204800, 'glm-5-turbo': 200000, 'glm-5.1': 202752, 'glm-5.2': 1000000, 'glm-5.3': 1000000, 'glm-5.3-flash': 1000000, 'glm-5v-turbo': 200000, 'glm-4.5-flash': 131072 },
      max_tokens: { 'glm-4.6': 131072, 'glm-4.7': 131072, 'glm-5-turbo': 131072, 'glm-5.1': 32768, 'glm-5.2': 131072, 'glm-5.3': 131072, 'glm-5.3-flash': 131072, 'glm-5v-turbo': 131072, 'glm-4.5-flash': 98304 },
      protocol: 'chat_completions',
      input_types: {}, // 默认纯文本；多模态模型可在 Provider 设置里勾选「图片」
    },
  },
  mcpServers: {},
  agent: { reminder_rounds: 30 },
  sandbox: { mode: 'seatbelt', sensitive_paths: [] }, // 命令沙箱默认开启（2026-08-16 用户决策）
  permissions: { computer_approved_apps: [] }, // Computer Use 已授权 App 空 = 全部拒绝（安全默认）
  runtime: { stop_background_on_interrupt: true }, // 主停止连后台任务一起停（S3-B 缺省 true）
  stt: { enabled: false }, // 语音输入默认关闭（配置 base_url/model/api_key 后开启）
}

function readSettings(): AppSettings {
  try {
    const raw = JSON.parse(fs.readFileSync(SETTINGS_FILE(), 'utf8'))
    const s = JSON.parse(JSON.stringify(DEFAULT_SETTINGS)) as AppSettings // 深拷贝兜底默认
    const provider: AppSettings['provider'] = { ...s.provider, ...(raw.provider ?? {}) }
    // Agent 提醒配置：旧 settings 没有 agent 段时使用默认 30；0 明确表示禁用。
    const agent: GoalAlignmentSettings = {
      ...s.agent,
      ...(raw.agent && typeof raw.agent === 'object' ? raw.agent : {}),
    }
    if (!Number.isFinite(agent.reminder_rounds) || agent.reminder_rounds < 0) agent.reminder_rounds = 30
    // 内置三卡：默认合并（老配置缺字段/缺协议时回退默认）
    for (const k of BUILTIN_PROVIDERS) provider[k] = { ...s.provider[k], ...(raw.provider?.[k] ?? {}) }
    // 自定义 provider：补齐 ProviderCfg 缺省（手改 settings.json 缺 models/max_tokens/protocol 时 UI 不崩）
    for (const [k, v] of Object.entries(provider)) {
      if (k === 'active' || BUILTIN_PROVIDERS.includes(k as (typeof BUILTIN_PROVIDERS)[number])) continue
      const cfg = v as ProviderCfg | undefined
      provider[k] = { base_url: cfg?.base_url ?? '', api_key: cfg?.api_key ?? '', models: cfg?.models ?? {}, max_tokens: cfg?.max_tokens ?? {}, protocol: cfg?.protocol ?? 'chat_completions', openai_compat: cfg?.openai_compat, input_types: cfg?.input_types ?? {}, prices: cfg?.prices }
    }
    // permissions 字段级合并：旧 settings 缺 computer_approved_apps → 补空数组
    //（避免整段 raw 覆盖丢字段；allowed_apps 残留字段不再读入——已废弃）。
    const permissions: PermissionsSettings = {
      computer_approved_apps: raw.permissions?.computer_approved_apps ?? [],
    }
    return { ...s, ...raw, provider, agent, permissions }
  } catch {
    return JSON.parse(JSON.stringify(DEFAULT_SETTINGS)) as AppSettings
  }
}

function writeSettings(s: AppSettings): { ok: true } | { ok: false; error: string } {
  const tmp = `${SETTINGS_FILE()}.tmp`
  try {
    // settings.json 含明文 provider api_key / MCP env：目录 0700、文件 0600（与 bridge
    // persistAuthType 的 tmp+rename 0600 一致，避免两边权限策略互相覆盖）。
    fs.mkdirSync(cfgDir(), { recursive: true, mode: 0o700 })
    // 原子写（同目录临时文件 + rename）：writeFileSync 直接写目标是「先截断再写」，
    // 崩溃/被 kill 会留下半截文件，并发读（bridge 的文件监听热重载）也会读到半截 →
    // 配置损坏（provider key / MCP 服务器一起消失）。rename 原子替换：读方要么旧要么新。
    fs.writeFileSync(tmp, JSON.stringify(s, null, 2), { mode: 0o600 })
    fs.renameSync(tmp, SETTINGS_FILE())
    return { ok: true }
  } catch (e) {
    // 磁盘只读/权限不足/磁盘满/JSON 序列化失败：必须向调用方暴露失败，
    // 不能假装保存成功（否则 UI 显示已保存、实际未落盘，后续还可能用旧配置覆盖）。
    try {
      fs.unlinkSync(tmp) // 失败残留的临时文件（含明文 key）清掉，避免堆积
    } catch {
      /* 无残留 / 删不掉：忽略 */
    }
    const msg = e instanceof Error ? e.message : String(e)
    return { ok: false, error: `配置写盘失败: ${msg}` }
  }
}

// —— 语音输入（STT）：主进程把录音转发到 OpenAI 兼容 /audio/transcriptions ——
// 走 Electron net.fetch（主进程发起，无 CORS）；API key 从 settings 读取，不进网页层。
async function transcribeAudio(audio: ArrayBuffer, mimeType: string): Promise<{ ok: boolean; text?: string; error?: string }> {
  const stt = readSettings().stt
  const baseUrl = (stt?.base_url ?? '').trim().replace(/\/+$/, '')
  const model = (stt?.model ?? '').trim()
  const apiKey = (stt?.api_key ?? '').trim()
  if (!baseUrl || !model || !apiKey) return { ok: false, error: '语音模型未配置（设置 → 通用设置 → 语音输入）' }
  const url = `${baseUrl}/audio/transcriptions`
  try {
    // multipart/form-data：OpenAI 兼容端点要求 file + model 字段（Go bridge 同构）
    const boundary = `----HAI-STT-${Date.now().toString(16)}`
    const ext = mimeType.includes('webm') ? 'webm' : mimeType.includes('ogg') ? 'ogg' : 'wav'
    const enc = new TextEncoder()
    const head = enc.encode(
      `--${boundary}\r\n` +
      `Content-Disposition: form-data; name="file"; filename="recording.${ext}"\r\n` +
      `Content-Type: ${mimeType}\r\n\r\n`,
    )
    const field = enc.encode(
      `\r\n--${boundary}\r\n` +
      `Content-Disposition: form-data; name="model"\r\n\r\n` +
      `${model}\r\n--${boundary}--\r\n`,
    )
    const body = new Uint8Array(head.byteLength + audio.byteLength + field.byteLength)
    body.set(head, 0)
    body.set(new Uint8Array(audio), head.byteLength)
    body.set(field, head.byteLength + audio.byteLength)
    const res = await net.fetch(url, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${apiKey}`,
        'Content-Type': `multipart/form-data; boundary=${boundary}`,
      },
      body: Buffer.from(body.buffer),
    })
    if (!res.ok) {
      const detail = (await res.text().catch(() => '')).slice(0, 300)
      return { ok: false, error: `转写失败（HTTP ${res.status}）：${detail}` }
    }
    const data = (await res.json().catch(() => null)) as { text?: string } | null
    const text = (data?.text ?? '').trim()
    if (!text) return { ok: false, error: '转写结果为空' }
    return { ok: true, text }
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e)
    return { ok: false, error: `转写请求失败：${msg}` }
  }
}

// 内置五卡的固定 env 名；自定义 provider 用通用 GOCODE_KEY_<UPPER(id)>（bridge keyLocked 兜底）。
function envNameForProvider(provider: string): string | null {
  const fixed: Record<string, string> = { deepseek: 'DEEPSEEK_API_TOKEN', openai: 'OPENAI_API_KEY', opencode: 'OPENCODE_API_KEY', kimi: 'KIMI_API_KEY', zhipu: 'ZHIPUAI_API_KEY' }
  if (fixed[provider]) return fixed[provider]
  if (isValidProviderName(provider)) return `GOCODE_KEY_${provider.toUpperCase()}`
  return null
}

// 读取全部已存 key（解密），返回 env 变量名 → 明文 key
// 2026-09：settings.json 已有明文 key 的 provider 跳过（settings 是新主源，spawn 注入
// 的 env 只作 bridge keyLocked 的兜底；若同时注入旧 keys.json 条目，bridge 内存空时
// 会回退到旧 key → 覆盖 settings 新 key → 401）。仅注入 settings 里没有的遗留 key。
function loadKeysEnv(): Record<string, string> {
  const out: Record<string, string> = {}
  let settings: AppSettings | null = null
  try {
    settings = readSettings()
  } catch {
    /* settings 不可读：回落纯 keys.json */
  }
  try {
    const keys = JSON.parse(fs.readFileSync(KEYS_FILE(), 'utf8')) as Record<string, { enc: boolean; data: string }>
    for (const [provider, v] of Object.entries(keys)) {
      if (settings) {
        const pc = settings.provider[provider]
        if (pc && typeof pc !== 'string' && typeof pc.api_key === 'string' && pc.api_key.trim()) {
          continue // settings 已有明文 key → 不注入旧加密条目（防覆盖）
        }
      }
      const name = envNameForProvider(provider)
      if (!name) continue
      try {
        out[name] = v.enc ? safeStorage.decryptString(Buffer.from(v.data, 'base64')) : Buffer.from(v.data, 'base64').toString()
      } catch {
        /* 解密失败（换机器/钥匙串丢失）：跳过该 key */
      }
    }
  } catch {
    /* 无 keys.json */
  }
  return out
}

// 删除某 provider 的 key（provider:remove 调用；keys.json 移除条目）
function removeProviderKey(provider: string): void {
  try {
    const keys = JSON.parse(fs.readFileSync(KEYS_FILE(), 'utf8')) as Record<string, unknown>
    delete keys[provider]
    fs.writeFileSync(KEYS_FILE(), JSON.stringify(keys, null, 2), { mode: 0o600 })
  } catch {
    /* 无 keys.json / 写失败：忽略 */
  }
}

// 该 provider 是否有已存 key（仅布尔指示，回 renderer 展示「已设置 ✓」）。
// 主源 = settings.json provider.api_key（明文，2026-08-16 起）；旧 keys.json 加密条目兜底。
function providerHasKey(provider: string): boolean {
  try {
    const s = readSettings()
    const pc = s.provider[provider]
    if (pc && typeof pc !== 'string' && typeof pc.api_key === 'string' && pc.api_key.trim()) return true
  } catch {
    /* 读 settings 失败：落入 keys.json 检查 */
  }
  try {
    const keys = JSON.parse(fs.readFileSync(KEYS_FILE(), 'utf8')) as Record<string, unknown>
    return Boolean(keys && keys[provider])
  } catch {
    return false
  }
}

function devUrl(): string | undefined {
  return process.env.VITE_DEV_SERVER_URL
}

function createWindow(): void {
  startupMark('createWindow')
  mainWindow = new BrowserWindow({
    width: 1440,
    height: 900,
    minWidth: 980,
    minHeight: 620,
    show: false, // 就绪再显示（ready-to-show）：消除 renderer 冷启动期的黑屏（实测 ~2.3s）
    backgroundColor: '#0d1117',
    titleBarStyle: 'hiddenInset',
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: false,
    },
  })
  const url = devUrl()
  // 外链一律交给系统浏览器，且绝不在应用内开新窗口（2026-09-11 实测修复）：
  // 主窗口此前没有 setWindowOpenHandler —— Markdown 外链带 target="_blank"，
  // 点击后 Electron 默认在应用内新建一个「裸 BrowserWindow」（无 preload/无 UI，
  // 只有一片空白 + 目标网页，关掉还要手动找窗口），实测 windowsAfterExternalLink 1→2。
  // 现在：deny 新窗口 + shell.openExternal 交给系统浏览器（与 OAuth 链接同一约定）。
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    if (/^https?:\/\//i.test(url)) void shell.openExternal(url)
    return { action: 'deny' }
  })
  // 兜底：renderer 内主动导航（window.location 赋值等）也不许把应用带到外部站点——
  // 外部地址交给系统浏览器。只放行本应用自己的入口（dev server 同源 / 本应用 index.html）。
  // 比较时归一化尾部斜杠与 hash（dev 入口可能带/不带 "/"，刷新时 URL 会规范化）。
  const selfUrl = url || pathToFileURL(path.join(APP_ROOT, 'dist', 'index.html')).href
  const norm = (u: string) => u.split('#')[0].replace(/\/+$/, '')
  mainWindow.webContents.on('will-navigate', (e, target) => {
    if (norm(target) === norm(selfUrl)) return
    e.preventDefault()
    if (/^https?:\/\//i.test(target)) void shell.openExternal(target)
  })
  // 权限处理器：只放行主窗口的两类请求，其余一律拒绝。
  //
  // ① media（麦克风，语音输入）：Electron 默认无 handler 时 getUserMedia 一律拒绝
  //    （NotAllowedError），必须显式授权；macOS 系统层首次使用仍会弹系统授权（TCC）。
  // ② 剪贴板写入：**权限名随「是否处于用户手势」而变**，两个都必须放行（2026-09-15 实测更正）：
  //      · 无手势调用 writeText（程序化赋值，如 useEffect 里写入）→ 请求 `clipboard-read`
  //      · 有手势调用 writeText（**点击「复制」按钮正是这条路径**）→ 请求 `clipboard-sanitized-write`
  //    此前只放行 `clipboard-read`，且当时的实测是在**无手势**场景做的 → 结论「已修」但只覆盖
  //    了无手势分支：无头/程序化调用通过，真实点击反而每次 NotAllowedError。这正是用户看到的
  //    「代码块/文本块点复制一直报错、artifacts 复制路径也报错」——报错是真实失败，系统剪贴板
  //    并未写入（已用真实 Electron + 真实鼠标点击核对 `clipboard.readText()`，确认未写入）。
  //    实测矩阵（Electron 33.4.11，最小 app，逐个变量隔离）：
  //      | 触发方式            | 请求的权限名                  | 只放行 clipboard-read |
  //      | 程序化（无手势）    | clipboard-read                | OK                    |
  //      | 真实点击（有手势）  | clipboard-sanitized-write     | THROW NotAllowedError |
  //      与 origin（file:// 或 http://localhost）、是否装 preload、是否装 check handler 均无关。
  //    （另外 renderer 侧有主进程剪贴板兜底 `clipboard:write`，见 electron/preload.ts —— 即使
  //      未来权限名再变，复制也不会退化成「报错且没复制」。）
  // 仍然限定 webContents === 主窗口：副窗口 / 内嵌视图不得读写剪贴板（实测副窗口依旧被拒）。
  // 策略抽到 permission-policy.ts（纯函数 → 毫秒级单测；含权限名漂移的实测依据）。
  session.defaultSession.setPermissionRequestHandler((webContents, permission, callback) => {
    const isMain = webContents === mainWindow?.webContents
    if (isMain && isMainWindowPermissionAllowed(permission)) {
      callback(true)
    } else {
      callback(false)
    }
  })
  // 首帧可绘制时再显示：用户看到窗口即有内容（动画/界面），无黑屏
  mainWindow.once('ready-to-show', () => {
    startupMark('ready-to-show')
    mainWindow?.show()
  })
  if (url) void mainWindow.loadURL(url)
  else void mainWindow.loadFile(path.join(APP_ROOT, 'dist', 'index.html'))
  mainWindow.webContents.on('did-finish-load', () => {
    startupMark('did-finish-load')
  })
  mainWindow.webContents.on('did-start-loading', () => {
    startupMark('did-start-loading')
    // 页面刷新（reload/导航）瞬间强制隐藏内嵌浏览器视图——WebContentsView 层级
    // 恒在 DOM 之上，刷新时 renderer 重新挂载（React 状态重置）期间若视图保持
    // visible，会浮在最上层挡住主页面（2026-08-30 实测：展开右栏后刷新必现）。
    // 刷新完成后由 renderer 的可见性 effect 按需重新 show。
    hideEmbed()
  })
  // renderer 首帧/动画开始回传（preload 经 console-message 或 IPC）
  mainWindow.webContents.on('did-stop-loading', () => {
    startupMark('did-stop-loading')
  })
  mainWindow.on('closed', () => {
    destroyEmbed()
    mainWindow = null
  })
  // M0：窗口尺寸变化时按右侧区域自动重排内嵌视图（渲染进程手动定位后不再覆盖）
  mainWindow.on('resize', () => {
    reflowEmbed()
  })
}

// 若 bridge 二进制缺失，dev 阶段自动 go build
// 打包态：bridge 随应用放入 Contents/Resources/hai-bridge（electron-builder extraResources，
// 避免二进制在 asar 内无法 spawn），优先级最高；dev 用 <app>/dist/hai-bridge（或环境变量覆盖）。
function newestMtime(paths: string[]): number {
  let newest = 0
  for (const p of paths) {
    try {
      const st = fs.statSync(p)
      newest = Math.max(newest, st.mtimeMs)
      if (st.isDirectory()) {
        for (const name of fs.readdirSync(p)) newest = Math.max(newest, newestMtime([path.join(p, name)]))
      }
    } catch { /* missing */ }
  }
  return newest
}

// 开发模式下源码比 bridge 新时自动重建，避免 Electron 复用旧二进制导致修复后的
// llm_error/session_run_error 或 provider 协议能力看似「没有生效」。生产包永远使用随包产物。
async function ensureBridgeBin(): Promise<{ ok: boolean; bin?: string; error?: string }> {
  if (app.isPackaged) {
    const packaged = path.join(process.resourcesPath, 'hai-bridge')
    return fs.existsSync(packaged)
      ? { ok: true, bin: packaged }
      : { ok: false, error: `bridge 二进制缺失（预期在 ${packaged}）` }
  }
  const repoRoot = path.resolve(APP_ROOT, '../..')
  const override = process.env.GO_CODE_DESKTOP_BIN || ''
  if (override && fs.existsSync(override)) return { ok: true, bin: override }
  const devSources = [
    path.join(repoRoot, 'desktop', 'bridge'),
    path.join(repoRoot, 'provider'),
    path.join(repoRoot, 'agents'),
    path.join(repoRoot, 'session'),
    path.join(repoRoot, 'events'),
    path.join(repoRoot, 'core'),
  ]
  const sourceMtime = newestMtime(devSources)
  let binMtime = 0
  try { binMtime = fs.statSync(BRIDGE_BIN).mtimeMs } catch { /* missing */ }
  const needsBuild = !binMtime || sourceMtime > binMtime
  if (!needsBuild) return { ok: true, bin: BRIDGE_BIN }
  try {
    await new Promise<void>((resolve, reject) => {
      const p = spawn('go', ['build', '-o', BRIDGE_BIN, './desktop/bridge'], { cwd: repoRoot })
      let out = ''
      p.stdout.on('data', (d: Buffer) => (out += d.toString()))
      p.stderr.on('data', (d: Buffer) => (out += d.toString()))
      p.on('error', reject)
      p.on('close', (code) => (code === 0 ? resolve() : reject(new Error(out || `go build exit ${code}`))))
    })
    return { ok: true, bin: BRIDGE_BIN }
  } catch (e) {
    return { ok: false, error: `go build 失败: ${(e as Error).message}` }
  }
}

// 单进程多 workspace：bridge 只起一次，所有工作区 × 会话复用一个进程；
// workspace 随命令 payload 传入（protocol 维度），切换工作区不重启 bridge。
// 批量 IPC（FIX_OPTIMIZATION 问题2②）：stdout 行先入 pending 缓冲，每 ~16ms 或 ≥200 行 flush 一次，
// 发送 { lines: string[] }；command_response 行先 flush 缓冲再单行直发（保序，renderer 依它作同步点）。
function spawnBridge(bin: string): boolean {
  if (bridge) return true // 已在运行：幂等复用，绝不 kill
  try {
    bridge = spawn(bin, [], { env: bridgeEnv() })
  } catch {
    return false
  }

  const pending: string[] = []
  const FLUSH_INTERVAL_MS = 16
  const FLUSH_THRESHOLD = 200
  let flushTimer: NodeJS.Timeout | null = null
  const clearFlushTimer = () => {
    if (flushTimer) {
      clearTimeout(flushTimer)
      flushTimer = null
    }
  }
  const flush = () => {
    if (mainWindow && pending.length > 0) {
      mainWindow.webContents.send('bridge:event', { lines: pending.splice(0) })
    } else {
      pending.length = 0 // 无窗口时丢弃剩余行（与原逐行行为一致），防缓冲泄漏
    }
  }
  const scheduleFlush = () => {
    if (flushTimer) return // 同一批已有一枚定时器，不重置（避免无限推迟）
    flushTimer = setTimeout(() => {
      flushTimer = null
      flush()
    }, FLUSH_INTERVAL_MS)
  }

  const rl = readline.createInterface({ input: bridge.stdout })
  rl.on('line', (line) => {
    const t = line.trim()
    if (!t || !mainWindow) return
    // ego lite 激活请求（2026-09 方案 1）：bridge 发意图，主进程执行激活——
    // bridge 子进程无 TCC「自动化控制 ego lite」授权（osascript 会失败），
    // Electron 主进程有（chrome-import osascript 先例）。执行后回传结果给前端。
    if (t.includes('"event_type":"ego_activate_request"')) {
      let msg = 'ego lite 已带到前台——请在其窗口切到 agent 正在操作的 Space 查看实时进展。'
      try {
        const ev = JSON.parse(t) as { message?: string }
        if (ev.message) msg = ev.message
      } catch { /* 用默认文案 */ }
      void activateEgoLite(msg)
      return // 不转发原始请求行；激活结果经 ego_activate_status 单独回传
    }
    // OAuth 登录事件（2026-08 P2）：auth_url → 唤起系统浏览器；其余转发前端展示
    if (t.includes('"event_type":"oauth_event"')) {
      try {
        const ev = JSON.parse(t) as { event?: { type?: string; url?: string; verification_uri?: string } }
        const event = ev.event
        const type = event?.type
        if (type === 'auth_url' && event?.url) void shell.openExternal(event.url)
        else if (type === 'device_code' && event?.verification_uri) void shell.openExternal(event.verification_uri)
      } catch { /* 解析失败照常转发 */ }
      mainWindow.webContents.send('bridge:event', t)
      return
    }
    if (t.includes('"event_type":"command_response"')) {
      // 先送完缓冲中的普通事件，再立即直发该同步点行（保序）
      clearFlushTimer()
      flush()
      let response: BridgeCommandResponse | undefined
      try {
        response = JSON.parse(t) as BridgeCommandResponse
      } catch {
        response = undefined
      }
      if (response?.id != null && pendingBridgeRequests.has(response.id)) {
        const request = pendingBridgeRequests.get(response.id)!
        pendingBridgeRequests.delete(response.id)
        clearTimeout(request.timer)
        request.resolve(response)
      } else {
        mainWindow.webContents.send('bridge:event', t)
      }
      return
    }
    pending.push(t)
    if (pending.length >= FLUSH_THRESHOLD) {
      clearFlushTimer()
      flush()
      return
    }
    scheduleFlush()
  })
  let err = ''
  bridge.stderr.setEncoding('utf8')
  // TEMP-TRACE：bridge stderr 实时回显（诊断 embed attach 卡点，定位后移除）
  bridge.stderr.on('data', (d: string) => {
    err += d
    process.stdout.write('[bridge] ' + d)
  })
  bridge.on('error', () => {
    settleBridgeRequests('bridge 进程错误')
    clearFlushTimer()
    flush()
    bridge = null
  })
  bridge.on('exit', (code) => {
    settleBridgeRequests(`bridge 退出 (${code})`)
    clearFlushTimer()
    flush() // 退出前送完剩余缓冲行，保证事件先于 bridge:exit 到达
    bridge = null
    if (mainWindow) mainWindow.webContents.send('bridge:exit', { code, err: err.slice(-4000) })
  })
  return true
}

function sendCommand(cmd: unknown): { ok: boolean; error?: string } {
  if (!bridge || bridge.killed) return { ok: false, error: 'bridge 未运行' }
  bridge.stdin.write(JSON.stringify(cmd) + '\n')
  return { ok: true }
}

function sendBridgeRequest(type: string, payload: Record<string, unknown> = {}): Promise<BridgeCommandResponse> {
  if (!bridge || bridge.killed) return Promise.resolve({ ok: false, error: 'bridge 未运行' })
  const id = bridgeRequestSeq--
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      pendingBridgeRequests.delete(id)
      resolve({ ok: false, error: 'bridge 请求超时' })
    }, 30_000)
    pendingBridgeRequests.set(id, { resolve, timer })
    try {
      bridge!.stdin.write(JSON.stringify({ id, type, payload }) + '\n')
    } catch (e) {
      clearTimeout(timer)
      pendingBridgeRequests.delete(id)
      resolve({ ok: false, error: (e as Error).message })
    }
  })
}

function stopBridge(): void {
  if (bridge) bridge.kill('SIGTERM')
}

// —— @ 引用文件系统（renderer ↔ 主进程 fs；工作区内/外都能读，不受 bridge 沙箱约束）——
interface FsEntry {
  name: string
  path: string
  isDir: boolean
  size?: number
}

// listFsDir 浏览目录：读目录项（目录优先 + 名称升序），供 @ 引用弹出文件浏览器
function listFsDir(dir: string): { ok: boolean; path: string; entries: FsEntry[]; error?: string } {
  const base = String(dir ?? '')
  if (!base || !path.isAbsolute(base)) return { ok: false, path: base, entries: [], error: '无效目录路径' }
  // 敏感目录 deny（V2 SECURITY-06）：文件树浏览不得进入含密钥/凭证的目录
  if (isSensitiveFsPath(base)) return { ok: false, path: base, entries: [], error: '该目录受保护（含敏感凭据）' }
  let items
  try {
    items = fs.readdirSync(base, { withFileTypes: true })
  } catch (e) {
    return { ok: false, path: base, entries: [], error: (e as NodeJS.ErrnoException).message }
  }
  const entries = items
    .map((d) => {
      const p = path.join(base, d.name)
      let size: number | undefined
      if (d.isFile()) {
        try {
          size = fs.statSync(p).size
        } catch {
          /* 统计失败（悬空链接等）：忽略尺寸 */
        }
      }
      return { name: d.name, path: p, isDir: d.isDirectory(), size }
    })
    .sort((a, b) => (a.isDir === b.isDir ? a.name.localeCompare(b.name) : a.isDir ? -1 : 1))
  return { ok: true, path: base, entries }
}

const MAX_REF_FILE_CHARS = 64 * 1024 // 单文件引用读取上限（字符；超限截断并标注）
const MAX_REF_DIR_ENTRIES = 300 // 单目录引用文件清单上限（递归；超限截断并标注）

// relTooWorkspace 绝对路径 → 工作区相对展示（./xxx）；工作区外保持绝对路径
function relToWorkspace(ws: string, abs: string): string {
  if (ws && path.isAbsolute(ws)) {
    const r = path.relative(ws, abs)
    if (r && r !== '..' && !r.startsWith('..' + path.sep) && !path.isAbsolute(r)) return './' + r.split(path.sep).join('/')
  }
  return abs
}

// readFsRef 展开 @ 引用：文件 → FilePath 块（含内容）；目录 → Dir 块（含递归文件清单）。
// 返回文本直接拼进用户消息（对齐产品格式：FilePath ./text.txt FileContent … / Dir ./textDir …）。
// 路径安全（V2 SECURITY-06）：提供 workspace 时必须在其内（realpath containment）；
// 敏感目录（.ssh/.aws/.go-code 等）一律拒绝；工作区外引用走 pickFsFile（用户明确选择）。
function readFsRef(p: { path: string; workspace: string }): { ok: boolean; content?: string; error?: string } {
  const target = String(p?.path ?? '')
  if (!target || !path.isAbsolute(target)) return { ok: false, error: '无效引用路径' }
  const ws = String(p?.workspace ?? '')
  if (ws && path.isAbsolute(ws)) {
    const rel = path.relative(path.resolve(ws), path.resolve(target))
    if (rel === '..' || rel.startsWith('..' + path.sep) || path.isAbsolute(rel)) {
      return { ok: false, error: `引用路径在工作区外: ${target}（工作区外文件请用「选择文件」）` }
    }
  }
  if (isSensitiveFsPath(target)) return { ok: false, error: '该路径受保护（含敏感凭据）' }
  let st
  try {
    st = fs.statSync(target)
  } catch {
    return { ok: false, error: `文件或目录不存在: ${target}` }
  }
  if (st.isDirectory()) {
    // 递归文件清单（跳常见重目录 .git / node_modules；cap 300 防爆炸）
    const lines: string[] = []
    let count = 0
    const walk = (dir: string) => {
      if (count >= MAX_REF_DIR_ENTRIES) return
      let names: string[]
      try {
        names = fs.readdirSync(dir)
      } catch {
        return
      }
      names.sort()
      for (const n of names) {
        if (count >= MAX_REF_DIR_ENTRIES) return
        const full = path.join(dir, n)
        let ist
        try {
          ist = fs.statSync(full)
        } catch {
          continue
        }
        if (ist.isDirectory()) {
          if (n === '.git' || n === 'node_modules') continue
          walk(full)
        } else if (ist.isFile()) {
          lines.push('  ' + relToWorkspace(ws, full))
          count++
        }
      }
    }
    walk(target)
    if (count >= MAX_REF_DIR_ENTRIES) lines.push(`  [目录清单已截断: >${MAX_REF_DIR_ENTRIES} 个文件]`)
    return { ok: true, content: `Dir: ${relToWorkspace(ws, target)}\nFiles:\n` + (lines.length ? lines.join('\n') : '  (空目录)') }
  }
  if (st.isFile()) {
    let buf: Buffer
    try {
      buf = fs.readFileSync(target)
    } catch (e) {
      return { ok: false, error: (e as NodeJS.ErrnoException).message }
    }
    if (buf.includes(0)) return { ok: true, content: `FilePath: ${relToWorkspace(ws, target)}\nFileContent: [二进制文件，不读内容]` }
    let s = buf.toString('utf8')
    if (s.length > MAX_REF_FILE_CHARS) s = s.slice(0, MAX_REF_FILE_CHARS) + `\n[文件内容已截断: >${MAX_REF_FILE_CHARS} 字符]`
    return { ok: true, content: `FilePath: ${relToWorkspace(ws, target)}\nFileContent:\n${s}` }
  }
  return { ok: false, error: '既不是普通文件也不是目录' }
}

// fsStat 单路径元信息（引用附件条展示「文件名 + 大小」用；不读内容，不复制）。
// 非绝对路径 / 不存在 → ok:false（前端据此显示「—」并提示，而不是静默省略大小）。
function fsStat(p: string): { ok: boolean; size?: number; isFile?: boolean; error?: string } {
  const target = String(p ?? '')
  if (!target || !path.isAbsolute(target)) return { ok: false, error: '无效路径' }
  try {
    const st = fs.statSync(target)
    return { ok: true, size: st.size, isFile: st.isFile() }
  } catch (e) {
    return { ok: false, error: (e as NodeJS.ErrnoException).message }
  }
}

// guardLocalPath 本地文件动作的统一入口校验（Artifacts 产出卡片用）。
// 返回 { ok:false, error } 或 { ok:true, path }（绝对路径）。
// 拒绝：非字符串 / 空 / 非绝对路径 / harness 配置目录（~/.go-code：api_key 明文）。
function guardLocalPath(p: unknown): { ok: boolean; path?: string; error?: string } {
  if (typeof p !== 'string' || !p) return { ok: false, error: 'absolute path required' }
  if (!path.isAbsolute(p)) return { ok: false, error: 'absolute path required' }
  const cfg = path.resolve(cfgDir())
  const abs = path.resolve(p)
  if (abs === cfg || abs.startsWith(cfg + path.sep)) {
    return { ok: false, error: 'protected path' }
  }
  return { ok: true, path: abs }
}

function registerIpc(): void {  // renderer 启动打点回传（TEMP-STARTUP：黑屏定位）
  ipcMain.handle('startup-mark', (_e, phase: string) => {
    startupMark(`renderer:${phase}`)
    return true
  })
  // @ 引用文件系统
  ipcMain.handle('fs:list', (_e, dir: string) => listFsDir(dir))
  ipcMain.handle('fs:read-ref', (_e, p: { path: string; workspace: string }) => readFsRef(p))
  // Markdown 本地图片：只读图片、回 data URL 给 <img>（路径口径与 read-ref 不同，见 read-image.ts 顶部注释）
  ipcMain.handle('fs:read-image', (_e, p: { path: string; workspace: string }) => readImageAsDataUrl(p))
  // 原生「选择文件」：无 opts → 单选返回 string | null（既有调用方 FileRefPanel 不变）；
  // opts.multi → 多选返回 string[]（取消 → []），供 Composer「添加文件」一次引多个。
  // opts.filters → 透传 showOpenDialog（如只列文档类型）。
  ipcMain.handle('fs:pick-file', async (e, opts?: { multi?: boolean; filters?: { name: string; extensions: string[] }[] }) => {
    const win = BrowserWindow.fromWebContents(e.sender) ?? undefined
    const properties: ('openFile' | 'multiSelections')[] = opts?.multi ? ['openFile', 'multiSelections'] : ['openFile']
    const o: { properties: ('openFile' | 'multiSelections')[]; filters?: { name: string; extensions: string[] }[] } = { properties }
    if (opts?.filters?.length) o.filters = opts.filters
    const r = win ? await dialog.showOpenDialog(win, o) : await dialog.showOpenDialog(o)
    if (opts?.multi) return r.canceled ? [] : r.filePaths
    return r.canceled || r.filePaths.length === 0 ? null : r.filePaths[0]
  })
  // 引用附件的体积展示（Composer 文档附件条用「文件名 + 大小」）。只读 stat，不读内容。
  ipcMain.handle('fs:stat', (_e, p: string) => fsStat(p))
  ipcMain.handle('fs:pick-dir', async (e) => {
    const win = BrowserWindow.fromWebContents(e.sender) ?? undefined
    const r = win ? await dialog.showOpenDialog(win, { properties: ['openDirectory', 'createDirectory'] }) : await dialog.showOpenDialog({ properties: ['openDirectory', 'createDirectory'] })
    return r.canceled || r.filePaths.length === 0 ? null : r.filePaths[0]
  })
  ipcMain.handle('bridge:start', async () => {
    const res = await ensureBridgeBin()
    if (!res.ok || !res.bin) return res
    const ok = spawnBridge(res.bin)
    return ok ? { ok: true } : { ok: false, error: 'bridge 启动失败' }
  })
  ipcMain.handle('bridge:command', (_e, cmd: unknown) => sendCommand(cmd))
  ipcMain.handle('bridge:stop', () => {
    stopBridge()
    return { ok: true }
  })
  ipcMain.handle('pick-workspace', async (e) => {
    // 与 fs:pick-* / app:pick 一致：挂到发起窗口（macOS 上不挂父窗口的对话框可能不置前，
    // 用户看不到「选择文件夹」弹窗 → 表现为点了没反应）。
    const win = BrowserWindow.fromWebContents(e.sender) ?? undefined
    const opts = { properties: ['openDirectory', 'createDirectory'] as ('openDirectory' | 'createDirectory')[] }
    const r = win ? await dialog.showOpenDialog(win, opts) : await dialog.showOpenDialog(opts)
    return r.canceled ? null : r.filePaths[0]
  })
  ipcMain.handle('home-dir', () => app.getPath('home'))
  ipcMain.handle('create-workspace-folder', (_e, parent: string, name: string) => {
    const clean = String(name ?? '').trim()
    const base = String(parent ?? '')
    if (!base || !path.isAbsolute(base)) return { ok: false, error: '父目录无效' }
    if (!clean || clean.includes('/') || clean === '.' || clean === '..') return { ok: false, error: '无效的文件夹名称' }
    const target = path.join(base, clean)
    try {
      fs.mkdirSync(target) // recursive:false → 已存在 EEXIST，抛错
      return { ok: true, path: target }
    } catch (e) {
      const m = (e as NodeJS.ErrnoException).code === 'EEXIST' ? '目录已存在' : (e as Error).message
      return { ok: false, error: m }
    }
  })
  // —— 全局设置 ——
  ipcMain.handle('settings:get', () => readSettings())
  // 语音转写：renderer 录音（ArrayBuffer）→ 主进程转发 OpenAI 兼容端点（net.fetch 无 CORS）
  ipcMain.handle('stt:transcribe', (_e, p: { audio: ArrayBuffer; mimeType: string }) => {
    if (!p || !p.audio) return { ok: false, error: '无音频数据' }
    return transcribeAudio(p.audio, String(p.mimeType ?? 'audio/webm'))
  })
  ipcMain.handle('external-skills:status', async () => {
    const started = await ensureBridgeBin()
    if (!started.ok || !started.bin) return { ok: false, error: started.error ?? 'bridge 不可用' }
    if (!spawnBridge(started.bin)) return { ok: false, error: 'bridge 启动失败' }
    const response = await sendBridgeRequest('external_skills_status')
    if (!response.ok) return { ok: false, error: response.error ?? '检测外部 Skills 失败' }
    return { ok: true, status: response.data?.status }
  })
  ipcMain.handle('external-skills:import', async () => {
    const started = await ensureBridgeBin()
    if (!started.ok || !started.bin) return { ok: false, error: started.error ?? 'bridge 不可用' }
    if (!spawnBridge(started.bin)) return { ok: false, error: 'bridge 启动失败' }
    const response = await sendBridgeRequest('external_skills_import')
    if (!response.ok) return { ok: false, error: response.error ?? '导入外部 Skills 失败', result: response.data }
    return { ok: true, result: response.data }
  })
  // xlsx 工作簿预览：renderer 点「表格预览」→ bridge 用受管 Python（openpyxl）把工作簿
  // 转成 HTML → 返回产物路径 → renderer 送内嵌浏览器 file://（与 PDF 预览同一条通道）。
  //
  // 为什么走主进程直发（sendBridgeRequest，负 id）而不是 renderer 的 command 通道：
  //   ① 转换要跑受管 Python，**首次还会先 bootstrap 运行时**（建 venv + 装约 42MB），
  //      可能几分钟；renderer 的 pending 表是「事件驱动 + 无超时」的，不适合这种
  //      「一问一答、可能很久」的请求 —— 主进程这条通道自带静默 30s 超时与错误归并；
  //   ② 产物路径只给内嵌浏览器用（不进 store 状态树/事件流），不需要广播给所有窗口。
  // 超时口径：30s 对已 bootstrap 的机器足够（实测小表 <1s）；未 bootstrap 的机器会
  // 报「bridge 请求超时」，用户重试一次即可（第二次运行时已就绪）—— 与「首次 run_python
  // 调用慢」的既有体验一致，故不为它单独加长超时（避免长期挂着看不见的请求）。
  ipcMain.handle('xlsx:preview', async (_e, req: { path?: string; workspace?: string } = {}) => {
    const started = await ensureBridgeBin()
    if (!started.ok || !started.bin) return { ok: false, error: started.error ?? 'bridge 不可用' }
    if (!spawnBridge(started.bin)) return { ok: false, error: 'bridge 启动失败' }
    const path = String(req?.path ?? '')
    if (!path) return { ok: false, error: 'path 为空' }
    const response = await sendBridgeRequest('xlsx_preview', {
      path,
      ...(req?.workspace ? { workspace: String(req.workspace) } : {}),
    })
    if (!response.ok) return { ok: false, error: response.error ?? '工作簿转换失败' }
    const htmlPath = response.data?.html_path
    if (typeof htmlPath !== 'string' || !htmlPath) return { ok: false, error: 'bridge 未返回 HTML 路径' }
    return { ok: true, html_path: htmlPath }
  })
  // docx 文档预览：renderer 点「文档预览」→ bridge 用受管 Python（mammoth）把文档
  // 转成 HTML → 返回产物路径 → renderer 送内嵌浏览器 file://（与 PDF 预览同一条通道）。
  //
  // 为什么走主进程直发（sendBridgeRequest，负 id）而不是 renderer 的 command 通道：
  //   ① 转换要跑受管 Python，**首次还会先 bootstrap 运行时**（建 venv + 装约 42MB），
  //      可能几分钟；renderer 的 pending 表是「事件驱动 + 无超时」的，不适合这种
  //      「一问一答、可能很久」的请求 —— 主进程这条通道自带静默 30s 超时与错误归并；
  //   ② 产物路径只给内嵌浏览器用（不进 store 状态树/事件流），不需要广播给所有窗口。
  // 超时口径：30s 对已 bootstrap 的机器足够（实测小文档 <1s）；未 bootstrap 的机器会
  // 报「bridge 请求超时」，用户重试一次即可（第二次运行时已就绪）—— 与「首次 run_python
  // 调用慢」的既有体验一致，故不为它单独加长超时（避免长期挂着看不见的请求）。
  // 上面这套理由三条预览通路（xlsx/docx/pptx）**逐字共用**，改一处要三处一起想。
  ipcMain.handle('docx:preview', async (_e, req: { path?: string; workspace?: string } = {}) => {
    const started = await ensureBridgeBin()
    if (!started.ok || !started.bin) return { ok: false, error: started.error ?? 'bridge 不可用' }
    if (!spawnBridge(started.bin)) return { ok: false, error: 'bridge 启动失败' }
    const path = String(req?.path ?? '')
    if (!path) return { ok: false, error: 'path 为空' }
    const response = await sendBridgeRequest('docx_preview', {
      path,
      ...(req?.workspace ? { workspace: String(req.workspace) } : {}),
    })
    if (!response.ok) return { ok: false, error: response.error ?? '文档转换失败' }
    const htmlPath = response.data?.html_path
    if (typeof htmlPath !== 'string' || !htmlPath) return { ok: false, error: 'bridge 未返回 HTML 路径' }
    return { ok: true, html_path: htmlPath }
  })
  // pptx 演示文稿预览：与上面 docx 那条逐字同构（同样的直发理由与 30s 口径），
  // 唯一差别是转换器：Python 侧 python-pptx **不能渲染**，产物是按真实 OOXML 几何做的
  // **近似版式重建**（做不到的几件事印在页面顶部，见 builtin.PptxPreviewer）。前端按钮
  // 文案也据此写「近似」—— 通路不保真，入口就不该许下保真的承诺。
  ipcMain.handle('pptx:preview', async (_e, req: { path?: string; workspace?: string } = {}) => {
    const started = await ensureBridgeBin()
    if (!started.ok || !started.bin) return { ok: false, error: started.error ?? 'bridge 不可用' }
    if (!spawnBridge(started.bin)) return { ok: false, error: 'bridge 启动失败' }
    const path = String(req?.path ?? '')
    if (!path) return { ok: false, error: 'path 为空' }
    const response = await sendBridgeRequest('pptx_preview', {
      path,
      ...(req?.workspace ? { workspace: String(req.workspace) } : {}),
    })
    if (!response.ok) return { ok: false, error: response.error ?? '演示文稿转换失败' }
    const htmlPath = response.data?.html_path
    if (typeof htmlPath !== 'string' || !htmlPath) return { ok: false, error: 'bridge 未返回 HTML 路径' }
    return { ok: true, html_path: htmlPath }
  })
  // app:pick：原生「选择应用程序」对话框（NSOpenPanel 选 .app，主窗口置顶 + 自带搜索）
  // → 返回所选 App 的显示名 + bundle id（权限白名单：避免手输拼错）。用户取消 → null。
  // （曾用 osascript choose application，但对话框不自动置前、真实环境选择不可靠——改原生。）
  ipcMain.handle('app:pick', async () => {
    const win = BrowserWindow.getAllWindows().find((w) => w.isVisible())
    const opts = {
      title: '选择允许打开的 App',
      buttonLabel: '选择',
      defaultPath: '/Applications', // 直达系统 App 目录（.app 作为文件包可选中，面板自带搜索）
      properties: ['openFile'] as ('openFile' | 'openDirectory' | 'multiSelections')[],
    }
    try {
      console.log('[app:pick] 打开对话框')
      const r = win ? await dialog.showOpenDialog(win, opts) : await dialog.showOpenDialog(opts)
      if (r.canceled || r.filePaths.length === 0) {
        console.log('[app:pick] 用户取消')
        return null
      }
      const p = r.filePaths[0]
      // 显示名优先：mdls kMDItemDisplayName（Spotlight 元数据，返回本地化显示名，
      // 如 wpsoffice.app → "WPS Office"）；失败回退 .app 文件名。
      // 授权存显示名，open_app 才能与模型看到的显示名匹配（2026-09 真机教训：
      // 存文件名 wpsoffice 导致 open_app "WPS Office" 被门禁拦）。
      let name = ''
      try {
        const out = spawnSync('/usr/bin/mdls', ['-name', 'kMDItemDisplayName', p], { encoding: 'utf8', timeout: 5000 })
        if (out.status === 0) {
          const m = String(out.stdout ?? '').match(/=\s*"?(.+?)"?\s*$/)
          if (m && m[1].trim() !== '(null)') name = m[1].trim()
        }
      } catch {
        /* mdls 失败忽略 */
      }
      if (!name) name = path.basename(p).replace(/\.app$/i, '')
      let bundleId = ''
      try {
        const out = spawnSync('/usr/bin/defaults', ['read', path.join(p, 'Contents', 'Info'), 'CFBundleIdentifier'], { encoding: 'utf8', timeout: 5000 })
        if (out.status === 0) bundleId = String(out.stdout ?? '').trim()
      } catch {
        /* bundle id 读取失败（非 .app 包）忽略 */
      }
      console.log(`[app:pick] 选择 ${p} → name=${name} bundleId=${bundleId || '(无)'}`)
      return { name, bundleId }
    } catch (e) {
      console.error('[app:pick] 异常', e)
      return null
    }
  })
  ipcMain.handle('settings:set', (_e, patch: Partial<AppSettings>) => {
    const cur = readSettings()
    const next: AppSettings = { ...cur, ...patch }
    if (patch.provider) {
      next.provider = { ...cur.provider }
      for (const [k, v] of Object.entries(patch.provider)) {
        if (k === 'active') {
          next.provider.active = v as string
        } else if (isValidProviderName(k)) {
          next.provider[k] = { ...((cur.provider[k] as ProviderCfg | undefined) ?? {}), ...(v as ProviderCfg) }
        }
      }
    }
    if (patch.agent) {
      const rounds = Number(patch.agent.reminder_rounds)
      next.agent = {
        ...(cur.agent ?? { reminder_rounds: 30 }),
        reminder_rounds: Number.isFinite(rounds) && rounds >= 0 ? Math.round(rounds) : (cur.agent?.reminder_rounds ?? 30),
      }
    }
    const res = writeSettings(next)
    if (!res.ok) return { ok: false, error: res.error }
    return { ok: true, settings: next }
  })
  // provider:save：持久化配置（settings.json）+ key（safeStorage 加密 keys.json）。不转发 bridge——
  // 热切换由 renderer 另发 set_provider 桥命令（key 只在命令 payload 瞬态，不入持久态）。
  ipcMain.handle('provider:save', (_e, p: { provider: string; active?: string; base_url?: string; protocol?: string; models?: Record<string, number>; max_tokens?: Record<string, number>; prices?: Record<string, { input: number; cache_read: number; cache_write: number; output: number }>; input_types?: Record<string, string[]>; openai_compat?: OpenAICompatCapabilities; auth_type?: 'api_key' | 'oauth'; usage_input_includes_cache?: boolean | null; cache_ttl_1h?: boolean | null; api_key?: string }) => {
    if (!isValidProviderName(p.provider)) return { ok: false, error: '无效 provider 名称' }
    if (p.active && !isValidProviderName(p.active)) return { ok: false, error: '无效 provider 名称' }
    const cur = readSettings()
    if (p.active) cur.provider.active = p.active
    const pc = (cur.provider[p.provider] as ProviderCfg | undefined) ?? { base_url: '', models: {}, max_tokens: {}, protocol: 'chat_completions', input_types: {} }
    // 用户重新保存配置 = 重新启用（清除删除标记；内置 provider 删除是 deleted 标记，
    // 重新添加同名时若不清除，bridge 重启仍视为已删除 → 配置丢失）。
    delete pc.deleted
    if (typeof p.base_url === 'string' && p.base_url) pc.base_url = p.base_url
    if (p.openai_compat && typeof p.openai_compat === 'object') pc.openai_compat = p.openai_compat
    if (p.protocol === 'chat_completions' || p.protocol === 'responses' || p.protocol === 'anthropic') pc.protocol = p.protocol
    if (p.models && typeof p.models === 'object') pc.models = p.models
    if (p.max_tokens && typeof p.max_tokens === 'object') pc.max_tokens = p.max_tokens
    if (p.prices && typeof p.prices === 'object') pc.prices = p.prices // 每模型价表覆盖（USD/1M tokens）
    if (p.input_types && typeof p.input_types === 'object') pc.input_types = p.input_types // 模型输入类型（图片门控）
    if (p.auth_type === 'api_key' || p.auth_type === 'oauth') pc.auth_type = p.auth_type // OAuth 鉴权方式（2026-08）
    // usage 口径标注（2026-09-21）：bool = 写字段（bridge loadSettings 读它）；
    // 显式 null = 删字段（回「自动」判定）——「不存在」与「false」语义不同，不能只写不删。
    if (p.usage_input_includes_cache === true || p.usage_input_includes_cache === false) pc.usage_input_includes_cache = p.usage_input_includes_cache
    else if (p.usage_input_includes_cache === null) delete pc.usage_input_includes_cache
    // 缓存 TTL 标注（同三态：bool 写字段 / null 删字段回自动）
    if (p.cache_ttl_1h === true || p.cache_ttl_1h === false) pc.cache_ttl_1h = p.cache_ttl_1h
    else if (p.cache_ttl_1h === null) delete pc.cache_ttl_1h
    if (p.api_key) {
      pc.api_key = p.api_key // 明文 key 持久化到 settings.json provider 段（用户决策 2026-08-16）
      // 同步删除 keys.json 同名旧条目（2026-09）：新 key 已明文落 settings.json，
      // 若 keys.json 仍留旧加密 key，spawnBridge 会把它当 env（如 ZHIPUAI_API_KEY）
      // 注入 bridge，且 bridge keyLocked 在内存空时回退 env → 旧 key 覆盖新 key →
      // 401「令牌已过期或验证不正确」。settings 是新主源，旧加密条目必须清除。
      removeProviderKey(p.provider)
    }
    cur.provider[p.provider] = pc
    const res = writeSettings(cur)
    if (!res.ok) return res
    return { ok: true, hasKey: providerHasKey(p.provider) }
  })
  // provider:remove：删除 provider（settings.json 移除 + keys.json 移除 + deleted 标记）。
  // 所有 provider（含内置 5 家）都可删除；删除活跃则回退到首个未删除的内置（deepseek 优先）。
  // deleted 标记让 bridge 重启后不复活（bridge 默认预置 5 家，靠标记识别用户删除）。
  ipcMain.handle('provider:remove', (_e, provider: string) => {
    if (!isValidProviderName(provider)) {
      return { ok: false, error: '无效 provider 名称' }
    }
    const cur = readSettings()
    // 内置 5 家：物理删除会因 bridge 预置复活 → 用 deleted 标记替代（保留默认值供恢复）
    if ((BUILTIN_PROVIDERS as readonly string[]).includes(provider)) {
      const pc = cur.provider[provider]
      const cfg = pc && typeof pc !== 'string' ? pc : ({} as ProviderCfg)
      cur.provider[provider] = {
        base_url: cfg.base_url ?? '', models: cfg.models ?? {}, max_tokens: cfg.max_tokens ?? {},
        protocol: cfg.protocol ?? 'chat_completions', openai_compat: cfg.openai_compat,
        input_types: cfg.input_types ?? {}, prices: cfg.prices,
        auth_type: cfg.auth_type ?? 'api_key',
        ...(cfg.api_key ? { api_key: cfg.api_key } : {}),
        deleted: true,
      } as ProviderCfg
    } else {
      // 自定义 provider：物理删除
      delete cur.provider[provider]
    }
    // 删除活跃 → 回退到首个未删除的内置（deepseek 优先；全删则 active 置 deepseek 占位）
    if (cur.provider.active === provider) {
      cur.provider.active = (BUILTIN_PROVIDERS as readonly string[]).find((b) => b !== provider && !(cur.provider[b] as ProviderCfg | undefined)?.deleted) ?? 'deepseek'
    }
    const res = writeSettings(cur)
    if (!res.ok) return res
    removeProviderKey(provider)
    return { ok: true }
  })
  ipcMain.handle('provider:key-status', (_e, provider: string) => providerHasKey(provider))
  // —— OAuth 订阅登录（2026-08 P2）：登录/登出/状态经 bridge 命令；auth_url/device_code 事件
  //    由 bridge 推送（oauth_event），主进程负责唤起系统浏览器（shell.openExternal）。
  ipcMain.handle('oauth:login', (_e, p: { provider: string }) => sendBridgeRequest('oauth_login', p))
  ipcMain.handle('oauth:logout', (_e, p: { provider: string }) => sendBridgeRequest('oauth_logout', p))
  ipcMain.handle('oauth:status', (_e, p: { provider: string }) => sendBridgeRequest('oauth_status', p))
  ipcMain.handle('oauth:prompt-answer', (_e, p: { provider: string; value: string }) => sendBridgeRequest('oauth_prompt_answer', p))
  // mcp:save：持久化 MCP 服务器配置（settings.json mcpServers 段，全量替换）。
  // 不转发 bridge——热应用由 renderer 另发 mcp_set 桥命令（重建会话工具集）。
  ipcMain.handle('mcp:save', (_e, servers: Record<string, MCPServerCfg>) => {
    const cur = readSettings()
    if (!servers || typeof servers !== 'object') return { ok: false, error: '无效配置' }
    for (const [name, c] of Object.entries(servers)) {
      if (!name.trim()) return { ok: false, error: '服务器名不能为空' }
      const t = c?.type ?? 'stdio'
      if (!['stdio', 'http', 'streamable_http', 'sse'].includes(t)) return { ok: false, error: `未知 type ${t}` }
      if (t === 'stdio' && !c?.command?.trim()) return { ok: false, error: `${name}: stdio 需 command` }
      if (t !== 'stdio' && !c?.url?.trim()) return { ok: false, error: `${name}: 需 url` }
    }
    cur.mcpServers = servers
    const res = writeSettings(cur)
    if (!res.ok) return res
    return { ok: true }
  })
  // 主题变化 → 窗口底色跟随（防切亮色时 pre-paint 白/黑闪）；主渲染由 renderer 设 data-theme
  ipcMain.handle('settings:apply-theme', (_e, theme: string) => {
    if (mainWindow) mainWindow.setBackgroundColor(theme === 'light' ? '#ffffff' : '#0d1117')
    return { ok: true }
  })
  // 外观设置「字体大小」→ 原生缩放（等同 Ctrl +/-）。为什么不用 CSS zoom：实测 zoom 1.5 时
  // 100vh 仍按视口算 → 应用根（height:100vh）溢出被裁，position:fixed 的浮层也会错位；
  // 原生缩放由 Chromium 统一处理 vh/fixed/媒体查询。内嵌视图的 bounds 换算见 embed.ts toDip()。
  ipcMain.handle('ui:set-zoom', (_e, scale: number) => {
    if (!mainWindow) return { ok: false, scale: 1 }
    const z = Math.min(2, Math.max(0.5, Number(scale) || 1))
    mainWindow.webContents.setZoomFactor(z)
    return { ok: true, scale: z }
  })
  // 下载安装 ego lite（Browser 引擎=ego 的引导，2026-09）：
  // 跑随包 skill 的 scripts/install.sh（下载 DMG → 装 /Applications → 去 quarantine → 启动）。
  // 输出实时回传（install 日志可能很长；ret 聚合尾部几行）。不阻塞主进程。
  ipcMain.handle('ego:install', async (_e) => {
    const skillDir = bundledEgoSkillDir()
    const script = path.join(skillDir, 'scripts', 'install.sh')
    if (!fs.existsSync(script)) {
      // 回退：打开 ego lite 官网（用户手动下载）
      void shell.openExternal('https://lite.ego.app/')
      return { ok: false, error: 'install.sh 不存在，已打开官网供手动下载' }
    }
    return new Promise<{ ok: boolean; error?: string }>((resolve) => {
      const child = spawn('sh', [script], { env: { ...process.env, PATH: `/usr/bin:/bin:/usr/sbin:/sbin:${process.env.PATH ?? ''}` } })
      let tail = ''
      const collect = (buf: Buffer) => {
        const s = buf.toString()
        tail = (tail + s).split('\n').slice(-6).join('\n')
      }
      child.stdout?.on('data', collect)
      child.stderr?.on('data', collect)
      child.on('error', (err) => resolve({ ok: false, error: `启动安装失败: ${err.message}` }))
      child.on('close', (code) => {
        if (code === 0) resolve({ ok: true })
        else resolve({ ok: false, error: `安装脚本退出码 ${code}${tail ? `：${tail.trim()}` : ''}` })
      })
    })
  })
  // 启动已安装的 ego lite（2026-09）：open -a（LaunchServices；无需 TCC 自动化授权，
  // app 未运行时也能拉起）。已安装未运行的场景由插件页「启动 ego lite」按钮触发，
  // 替代此前的「重新下载安装」——app 装过一次就不该再让用户下载。
  ipcMain.handle('ego:launch', async (_e) => {
    return new Promise<{ ok: boolean; error?: string }>((resolve) => {
      execFile('/usr/bin/open', ['-a', 'ego lite'], { timeout: 8000 }, (err) => {
        if (err) resolve({ ok: false, error: err.message })
        else resolve({ ok: true })
      })
    })
  })
  // —— M0 POC：右侧栏内嵌浏览器视图（WebContentsView + CDP 驱动通路）——
  ipcMain.handle('embed:show', (_e, p: { url?: string; rect?: { x: number; y: number; width: number; height: number } }) => {
    if (!mainWindow) return { ok: false, error: '窗口未就绪' }
    showEmbed(mainWindow, p?.url ?? '', p?.rect)
    return { ok: true, state: embedStatus() }
  })
  ipcMain.handle('embed:resize', (_e, rect: { x: number; y: number; width: number; height: number }) => {
    setEmbedBounds(rect)
    return { ok: true }
  })
  ipcMain.handle('embed:hide', () => {
    hideEmbed()
    return { ok: true }
  })
  ipcMain.handle('embed:command', async (_e, p: { method: string; params?: unknown }) => {
    try {
      return { ok: true, result: await embedCommand(p.method, p.params) }
    } catch (e) {
      return { ok: false, error: (e as Error).message }
    }
  })
  ipcMain.handle('embed:status', () => embedStatus())
  // M2：debugger TCP 代理（把内嵌视图 webContents.debugger 暴露成 JSON 行协议端点，供 go bridge 连接）
  ipcMain.handle('embed:debugger-proxy', async () => {
    try {
      const port = await startDebuggerProxy()
      return { ok: true, endpoint: `127.0.0.1:${port}`, port }
    } catch (e) {
      return { ok: false, error: (e as Error).message }
    }
  })
  ipcMain.handle('embed:debugger-proxy-stop', () => {
    stopDebuggerProxy()
    return { ok: true }
  })
  // —— M5：用户自由导航（迷你工具条：地址栏 / 前进 / 后退 / 刷新）——
  ipcMain.handle('embed:navigate', async (_e, url: string) => {
    try {
      return { ok: true, state: await navigateEmbed(String(url ?? '')) }
    } catch (e) {
      return { ok: false, error: (e as Error).message }
    }
  })
  ipcMain.handle('embed:back', () => {
    embedGoBack()
    return { ok: true }
  })
  ipcMain.handle('embed:forward', () => {
    embedGoForward()
    return { ok: true }
  })
  ipcMain.handle('embed:reload', () => {
    embedReload()
    return { ok: true }
  })
  // 剪贴板写入兜底（renderer 的复制动作统一走这里；见 src/lib/clipboard.ts）。
  //
  // 为什么主进程也要有一条通路：renderer 侧 `navigator.clipboard.writeText` 依赖 Electron 的
  // 权限判定，而权限名会随版本/触发方式变化（实测：无手势=clipboard-read，真实点击=
  // clipboard-sanitized-write）。权限一旦没放行，复制就是**失败且无写入** —— 而复制是高频
  // 基础动作，不该依赖一个会漂移的权限名。主进程 clipboard 模块不走 renderer 权限体系，
  // 是同一端能力的稳定入口。
  //
  // 安全：与 shell:open-path 同一条边界 —— 只限定主窗口调用（副窗口/内嵌浏览器视图不得
  // 写用户剪贴板），且内容是纯文本（无文件路径/命令执行能力）。
  // 返回 ok=是否写入（writeText 无返回值，失败表现为抛错）。
  ipcMain.handle('clipboard:write', (e, text: unknown) => {
    try {
      if (e.sender !== mainWindow?.webContents) return { ok: false, error: '仅主窗口可写剪贴板' }
      clipboard.writeText(typeof text === 'string' ? text : String(text ?? ''))
      return { ok: true }
    } catch (err) {
      return { ok: false, error: (err as Error).message }
    }
  })
  // 本地文件动作（Artifacts 产出卡片）：用系统默认程序打开本地文件（.xlsx → Excel、
  // .png → 预览）与在 Finder 中显示。文件已在本机，无需「下载/另存为」。
  //
  // 安全（硬性约束）：shell.openPath 是强能力（可执行任意本地文件），故双重校验——
  //   ① renderer 只把 agent_end.artifacts 携带的路径直传（不经 LLM 文本）
  //   ② 主进程侧只接受**绝对路径**，且拒绝 harness 自身配置目录（含 settings.json 的
  //      api_key 明文），与 bridge 的 denyHarnessConfigPath 同为一道保护。
  ipcMain.handle('shell:open-path', async (_e, p: unknown) => {
    const abs = guardLocalPath(p)
    if (!abs.ok) return abs
    const err = await shell.openPath(abs.path!) // 返回空串 = 成功
    return err ? { ok: false, error: err } : { ok: true }
  })
  ipcMain.handle('shell:show-item', (_e, p: unknown) => {
    const abs = guardLocalPath(p)
    if (!abs.ok) return abs
    shell.showItemInFolder(abs.path!) // 无返回值；文件不存在时系统静默（不假报成功）
    return { ok: true }
  })
  // —— M6：多页面（真实多 target 视图池）——
  ipcMain.handle('embed:open-target', (_e, url: string, opts?: { sid?: string; activate?: boolean; srcPath?: string }) => {
    try {
      if (!mainWindow) return { ok: false, error: '窗口未就绪' }
      return { ok: true, state: openEmbedTarget(mainWindow, String(url ?? ''), { sid: String(opts?.sid ?? ''), activate: opts?.activate !== false, srcPath: String(opts?.srcPath ?? '') }) }
    } catch (e) {
      return { ok: false, error: (e as Error).message }
    }
  })
  // 预览标签语义：刷新/导航**指定**标签（不是激活视图）。命中已存在的标签时，渲染层靠这两个
  // 把「打开过的文件」更新到最新内容 —— 后台标签（Agent 产出）也要能刷新，所以不能复用
  // embed:reload / embed:navigate 那两个「只作用激活视图」的老通道。
  ipcMain.handle('embed:reload-target', async (_e, id: string) => {
    try {
      // 阶段 3：休眠条目命中 → 主进程先重建（重建即加载 = 刷新），故这里要 await
      //（不 await 会让「刷新失败」变成静默成功：渲染层拿 ok 就以为内容已更新）。
      await reloadEmbedTarget(String(id ?? ''))
      return { ok: true }
    } catch (e) {
      return { ok: false, error: (e as Error).message }
    }
  })
  ipcMain.handle('embed:navigate-target', async (_e, id: string, url: string) => {
    try {
      await navigateEmbedTarget(String(id ?? ''), String(url ?? ''))
      return { ok: true }
    } catch (e) {
      return { ok: false, error: (e as Error).message }
    }
  })
  ipcMain.handle('embed:set-session', (_e, sid: string) => {
    try {
      return { ok: true, state: setEmbedSession(String(sid ?? '')) }
    } catch (e) {
      return { ok: false, error: (e as Error).message }
    }
  })
  ipcMain.handle('embed:activate', async (_e, id: string) => {
    try {
      // 阶段 3：命中休眠条目时会先重建再激活，失败（如文件已删）必须让渲染层拿到 ok:false
      // 去 toast —— 否则就是「点了没反应」的静默失败。
      return { ok: true, state: await activateEmbedTarget(String(id ?? '')) }
    } catch (e) {
      return { ok: false, error: (e as Error).message }
    }
  })
  // 阶段 3：运行中会话集合（渲染层 busySids 推送）→ 视图池 LRU 保护集：这些会话的标签
  // 不得被休眠（会话在跑任务时 Agent 随时可能再命令它的页面）。
  ipcMain.handle('embed:set-busy-sids', (_e, sids: string[]) => {
    setBusySids(Array.isArray(sids) ? sids.map(String) : [])
    return { ok: true }
  })
  ipcMain.handle('embed:close-target', (_e, id: string) => {
    try {
      return { ok: true, state: closeEmbedTarget(String(id ?? '')) }
    } catch (e) {
      return { ok: false, error: (e as Error).message }
    }
  })
  // —— 登录同步：一键导入用户 Chrome 登录状态（读+解密 → 写入 Electron partition）——
  // embedded 模式 Browser Use 实际使用 persist:gocode-browser partition，导入必须写这里。
  const cookieStore = () => session.fromPartition('persist:gocode-browser').cookies
  // autoQuit=true：Chrome 运行中自动优雅退出 → 导入 → 重新打开，用户只点一次。
  ipcMain.handle('chrome:import-login', async () => {
    const writeFn = async (cookies: ImportableChromeCookie[]): Promise<number> => {
      let ok = 0
      const s = cookieStore()
      for (const c of cookies) {
        try {
          await s.set({
            url: `https://${c.domain}${c.path || '/'}`,
            name: c.name,
            value: c.value,
            domain: c.domain,
            path: c.path || '/',
            secure: c.secure,
            httpOnly: c.httpOnly,
            expirationDate: c.expirationDate,
            sameSite: c.sameSite,
          })
          ok++
        } catch {
          /* 单条失败跳过 */
        }
      }
      await s.flushStore().catch(() => {})
      return ok
    }
    const r = await importCookiesToPartition(writeFn, { autoQuit: true })
    return r
  })
  // 清除已导入登录态（逐域移除 partition cookie）
  ipcMain.handle('chrome:clear-login', async () => {
    try {
      const s = cookieStore()
      // 无记录文件，清整个 partition 的 cookies（可撤销：只影响 AI 浏览器会话）
      const all = await s.get({}).catch(() => [])
      let removed = 0
      for (const c of all) {
        try {
          await s.remove(`https://${c.domain}${c.path || '/'}`, c.name)
          removed++
        } catch {
          /* 单条失败继续 */
        }
      }
      return { ok: true, removed }
    } catch (e) {
      return { ok: false, removed: 0, error: (e as Error).message }
    }
  })
}

// 单实例锁：防止多个实例同时跑（dev 下 vite 端口/bridge stdio/settings.json 会被多实例
// 抢占 → 其中一个被杀，表现为"应用崩溃"。锁定后第二个实例直接退出并聚焦已有窗口）。
// 多实例模式（HAI_DATA_DIR / HAI_MULTI）：跳过锁——每个实例有独立配置根与 userData，
// 可同机并行（本地双开验证 Session Mesh 等跨客户端联调）。
const MULTI_INSTANCE = Boolean(MULTI_DATA_DIR)
if (!MULTI_INSTANCE && !app.requestSingleInstanceLock()) {
  app.quit()
} else if (!MULTI_INSTANCE) {
  app.on('second-instance', () => {
    if (mainWindow) {
      if (mainWindow.isMinimized()) mainWindow.restore()
      mainWindow.focus()
    }
  })
}

app.whenReady().then(() => {
  startupMark('whenReady')
  registerIpc()
  createWindow()
  // 屏幕录制 SCK 预热（2026-09 Computer Use 截图排障）：macOS 14+ 截屏走
  // ScreenCaptureKit 独立 TCC（kTCCServiceScreenCaptureKit）——screencapture 命令
  // 与 desktopCapturer 都依赖它，系统设置「屏幕录制」只写旧服务。启动时调一次
  // desktopCapturer.getSources 触发系统授权弹窗（用户允许后截图通道可用）。
  // 幂等：已授权则无弹窗静默成功。
  try {
    const { desktopCapturer } = require('electron') as typeof import('electron')
    desktopCapturer.getSources({ types: ['screen'], thumbnailSize: { width: 64, height: 64 } })
      .then((sources) => {
        if (sources.length > 0) console.log('[screen] SCK 预热成功（截图通道可用）')
      })
      .catch(() => { /* 未授权/拒绝：Computer Use 截图会返回权限引导 */ })
  } catch { /* desktopCapturer 不可用（非 darwin）忽略 */ }
  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow()
  })
})
app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') app.quit()
})

app.on('before-quit', () => {
  stopBridge()
})
