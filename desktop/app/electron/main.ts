import { app, BrowserWindow, ipcMain, dialog, safeStorage, session, shell, net, clipboard, protocol } from 'electron'
import { spawn, spawnSync, execFile, ChildProcessWithoutNullStreams } from 'child_process'
import * as readline from 'readline'
import * as path from 'path'
import { pathToFileURL } from 'url'
import * as fs from 'fs'
import * as os from 'os'
import { randomUUID } from 'crypto'
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
// 文件树写操作（重命名/移动/新建/复制副本）：守卫与实现同源，见 fs-ops.ts
import { copyEntry, createDir, createFile, duplicateEntry, guardTrash, importEntry, moveEntry, renameEntry } from './fs-ops'
// 输入框历史本地文件（~/.go-code/input-history.json；与 renderer 的 localStorage 镜像配合）
import { ipcInputHistoryForget, ipcInputHistoryGet, ipcInputHistorySet, inputHistoryFilePath } from './input-history'

// 音视频字节通道（R08c）：`hai-media://` 支持 Range 的自定义协议 + 路径校验，见 media-protocol.ts
import { MEDIA_SCHEME_PRIVILEGES, mediaStats, mediaUrlForFile, registerMediaProtocol } from './media-protocol'
// Surface 插件资产通道（PR-14）：`hai-surface://<id>/panel.js`，路径校验是纯函数
// （surface-protocol.ts / surface-index.ts），单测不启动 Electron。
import { SURFACE_SCHEME_PRIVILEGES, registerSurfaceProtocol } from './surface-protocol'
import { SURFACE_ID_RE, configureSurfaceRoot, readSurfaceIndex } from './surface-index'
// 媒体协议的信任锚：只有用户在系统选择器里亲手打开过的工作区才能当媒体根（见文件头注释）
import { configureKnownWorkspaces, forgetWorkspace, isKnownWorkspace, rememberWorkspace } from './known-workspaces'

// hub:// 深链（P4-02）：argv 提取是纯逻辑，单独文件才能被 Node 测试
import { HUB_PROTOCOL, extractHubDeepLinkArg } from './deeplink'
// 主进程 → 渲染进程的唯一发送通道（BR-04）：窗口 null／webContents 已销毁时丢弃而不是抛错
import { createRendererSender } from './renderer-send'
// settings.json 单写者语义（BR-01 / BR-29 / BR-02 写侧）：v1→v2 迁移、白名单 patch、
// provider/mcp 段级写、原子文件 IO。写侧只认**原始对象**，不认「默认值 ∪ 磁盘值」的并集。
import {
  applyMcpServers,
  applyProviderRemove,
  applyProviderSave,
  applySettingsPatch,
  commitSettingsWrite,
  isValidProviderName,
  runSettingsMigration,
  type JsonObject,
  type SettingsApplyResult,
  type SettingsFsOps,
} from './settings-store'
// provider key 的唯一持久化容器（BR-08）：keys.json + safeStorage（无钥匙串时 base64 降级）、
// 存量明文迁移、spawn env / 桥命令的按需注入。主进程不再自己解析 keys.json。
import {
  activeProviderKeyEnv,
  copyKey as keyStoreCopyKey,
  getKey as keyStoreGetKey,
  hasKey as keyStoreHasKey,
  injectProviderKey,
  migratePlaintextProviderKeys,
  removeKey as keyStoreRemoveKey,
  setKey as keyStoreSetKey,
  type KeyCryptoOps,
  type KeyStoreArgs,
  type KeyStoreFsOps,
} from './keys-store'

// `hai-media://` 的特权必须**在 app ready 之前**登记（Electron 的硬性时序要求，晚了就再也
// 注册不上）：standard + secure 让 <video> 认它是可播放的安全来源，stream 让响应体是流
// （1 小时 mp4 不整个读进内存），supportFetchAPI 让探针能 fetch 同一 URL 断言 206。
// `hai-surface://` 同理：渲染层的 loader 要用 `import()` 拉 panel.js，所以它必须是
// standard + secure + supportFetchAPI（见 surface-protocol.ts 的常量注释）。
protocol.registerSchemesAsPrivileged([MEDIA_SCHEME_PRIVILEGES, SURFACE_SCHEME_PRIVILEGES])

let mainWindow: BrowserWindow | null = null

// sendToRenderer：所有 webContents.send 的唯一出口（见 renderer-send.ts 的取舍说明）。
// 传 getter 而不是实例：主窗口会在关闭/重开时重建。
const rendererSender = createRendererSender(() => mainWindow)
const sendToRenderer = rendererSender.send
let bridge: ChildProcessWithoutNullStreams | null = null

// —— bridge 启动闸门（2026-11-14）——
//
// `ensureBridgeBin()` 在产物比源码旧时会**跑 go build**（数秒）。这段时间里
// `bridge` 还是 null，而渲染层**已经在发命令**了（wire 的快照、hub attach、
// office-kit 探针、new_session…）⇒ 老实现直接回「bridge 未运行」⇒
// 前端 `looksBridgeDown()` 命中 ⇒ `bridgeStatus` 被**永久** latch 成 error
// ⇒ 红色横幅「bridge 已退出」常驻，要用户手点「重启 bridge」才消失。
// （实测：刚重建过 bridge 就会命中——产物与源码的 mtime 比较正好触发 go build。）
//
// ⇒ 改成：启动期间把命令**排队**，bridge 起来后按序补发（命令 id 原样带上，
// 回执仍能对上号）。只有「不在启动中」才是真的没跑起来。
let bridgeBooting = false
const bridgeBootQueue: string[] = []
// —— 原生脏标记 + 退出拦截（PR-05b）——
// docDirty 由渲染层镜像（任一编辑标签有未保存改动）；allowQuit 是**防死循环**的放行标志：
// 拦下 → 问用户 → 用户选丢弃/保存完 → quitAnyway() 置 true → app.quit() 第二次进
// before-quit 时直接放行。没有这个标志，preventDefault 会把进程永久锁在退出前。
let docDirty = false
let allowQuit = false
// lastPickedDir 最近一次由**系统选择器**返回的目录（create-workspace-folder 的父目录白名单）。
// 为什么需要：create-workspace-folder 的 `parent` 也是渲染进程传的任意绝对路径 —— 那是
// 信任锚上的第四条可控写入口（复评「顺手改」第 1 条）。它只能登记「刚建出来的空目录」，
// 危害有限，但没有理由留一条「渲染进程说在哪建就在哪建」的路。
let lastPickedDir = ''

// —— hub:// 深链（P4-02）——
// 三个入口：macOS open-url、Win/Linux second-instance argv、首实例启动 argv（冷启动点链接）。
// 冷启动/刷新时 renderer 还没订阅 IPC：先排队，等它挂载后主动 take（丢一条链接比抢跑更糟）。
// 协议注册只在打包态做（见 registerHubProtocol）：dev 下注册会把系统里的 hub:// 指向开发目录。
let rendererReady = false
let pendingHubDeepLinks: string[] = []

function forwardHubDeepLink(raw: string): void {
  const link = String(raw ?? '').trim()
  if (!link) return
  // 冷启动/刷新期间 renderer 还没订阅：先排队（上限 20，点很多次也只留最近几条）。
  // 判空与发送之间的 race 交给 sendToRenderer（不抛）；真没送出去也回队，等下次挂载 take。
  if (!rendererReady || !mainWindow || mainWindow.webContents.isDestroyed() || !sendToRenderer('hub:deep-link', link)) {
    pendingHubDeepLinks.push(link)
    if (pendingHubDeepLinks.length > 20) pendingHubDeepLinks.shift()
  }
}

// renderer 挂载后**主动来取**队列：订阅在前、take 在后，之间到达的链接不会漏。
// take 之后 rendererReady=true，后续深链直接 send。页面刷新（did-start-loading）会复位。
ipcMain.handle('hub:take-deep-links', () => {
  const queued = pendingHubDeepLinks
  pendingHubDeepLinks = []
  rendererReady = true
  return queued
})

/**
 * 注册 hub:// 协议处理器（仅打包态）。
 * macOS 的 scheme 来自 Info.plist 的 CFBundleURLTypes（electron-builder 生成），运行时不需要
 * setAsDefaultProtocolClient；Win/Linux 才需要写注册表/desktop 文件。
 * 注册失败不影响启动：链接仍可经 second-instance argv 进入。
 */
function registerHubProtocol(): void {
  if (!app.isPackaged || process.defaultApp) return
  try {
    if (process.platform === 'darwin') return
    app.setAsDefaultProtocolClient(HUB_PROTOCOL)
  } catch {
    /* 注册失败：留给 argv 通路，不阻塞启动 */
  }
}

// macOS：URL scheme 在 ready 之前就可能到达，必须尽早注册事件监听。
app.on('open-url', (event, url) => {
  event.preventDefault()
  forwardHubDeepLink(url)
})

// Win/Linux 冷启动：深链就在自己的 argv 里（没有 second-instance 可等）。
{
  const initial = extractHubDeepLinkArg(process.argv)
  if (initial) pendingHubDeepLinks.push(initial)
}

// —— 启动打点（TEMP-STARTUP：定位黑屏/启动慢；**已停用**，不再写 electron-startup.log）——
function startupMark(_phase: string): void {
  /* 已停用：启动诊断已完成，不写文件 */
}

type BridgeCommandResponse = {
  event_type?: string
  id?: number
  ok: boolean
  error?: string
  // code 稳定错误码（bridge 的 respCode；surface_command 用它回 denied/unknown_id/timeout/error）
  code?: string
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

// —— 插件 ↔ Agent 通道的身份（P1b2，§6.6）——
//
// surface_id → token：main 在插件加载前签发（loader 经 `surface:agent-token` 领）。
// 只存在内存里：token 是**进程内**凭证（重启后没有 run 可续），也不该出现在任何磁盘状态里。
// 反查表按需扫（插件数量是个位数），不维护第二份真相 —— 两张表总有一天会不一致。
const surfaceAgentTokens = new Map<string, string>()
const SURFACE_AGENT_EVENT_CHANNEL = 'surface:agent-event'
// agent.* 的五条（§6.6 命令表；P3a 新增只读 snapshot）。manifest 声明的其它名字走 surface_command 那条老路，
// 不走这里；不在表里的名字一律 denied（插件 Agent 通道不承载宿主的其它命令）。
const SURFACE_AGENT_COMMANDS = new Set(['agent.session', 'agent.run', 'agent.cancel', 'agent.snapshot', 'tool.result'])
// `agent.run` 的**立即回执**（不是整轮耗时）：Go 建 run 就回，流式输出走事件。
// 30s 是创建会话 + 取 provider 的宽限；超时只说明 ack 丢了，不代表 run 没起来。
const SURFACE_AGENT_TIMEOUT_MS = 30_000

function surfaceAgentTokenOf(id: string): string {
  let token = surfaceAgentTokens.get(id)
  if (!token) {
    token = randomUUID()
    surfaceAgentTokens.set(id, token)
  }
  return token
}

function surfaceIdForToken(token: string): string | undefined {
  for (const [id, t] of surfaceAgentTokens) if (t === token) return id
  return undefined
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
// 手机端静态页目录（P8 方案 A：一个目录，不 embed/不 zip）：
// 打包态 → Contents/Resources/mobile-web（electron-builder extraResources 从 dist-mobile 复制）；
// dev → 仓库 desktop/app/dist-mobile（先跑 npm run mobile:build，mobile.html 会改名 index.html）。
function bundledMobileWebDir(): string {
  if (app.isPackaged) {
    return path.join(process.resourcesPath, 'mobile-web')
  }
  return path.join(APP_ROOT, 'dist-mobile')
}

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

// 内置视觉/UI 技能集合目录（visual-ui / screenshot-to-ui / visual-review，2026-10；随包分发）：
// 打包态 → Contents/Resources/visual-skills（electron-builder extraResources）；
// dev → 仓库 desktop/vendor/visual-skills。与办公技能**完全同一套**同步机制
// （Go 侧 syncBundledSkills，逐子目录 → ~/.agents/skills，按 frontmatter version 决定覆盖）。
//
// 与智谱 GLM-skills 的关键差别：这三个技能全程由**会话选中的模型**执行
// （模型自己 read_file 看图、自己写 HTML/代码），不调用任何外部视觉 API、不需要额外
// key 或 Python 依赖。哪些技能对当前会话可见由 bridge 的 skill_capability.go 按
// 选中模型的能力（requires.input: [image]）决定。
function bundledVisualSkillsDir(): string {
  if (app.isPackaged) {
    return path.join(process.resourcesPath, 'visual-skills')
  }
  return path.resolve(APP_ROOT, '..', 'vendor', 'visual-skills')
}

// 内置 CreateMode 技能集合目录（2026-11；随包分发）：
// 打包态 → Contents/Resources/create-mode-skills（electron-builder extraResources）；
// dev → 仓库 desktop/vendor/create-mode-skills。Go 侧 syncBundledSkills 逐子目录同步到
// ~/.agents/skills/<name>，按 frontmatter version 决定是否覆盖用户副本。
// 集合里目前只有 create-mode；保留集合形状，未来加 CreateMode 相关技能不用改接线。
function bundledCreateModeSkillsDir(): string {
  if (app.isPackaged) {
    return path.join(process.resourcesPath, 'create-mode-skills')
  }
  return path.resolve(APP_ROOT, '..', 'vendor', 'create-mode-skills')
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
    // execFile 是异步回调：此刻窗口可能已被关掉（用户在这 5s 内退出）—— 走统一通道丢弃，
    // 否则 exception 会从子进程回调冒到主进程（BR-04 的同类现场）。
    sendToRenderer('bridge:event', JSON.stringify({
      event_type: 'ego_activate_status',
      ok,
      ...(ok ? { message } : { error: err?.message ?? 'ego lite 激活失败' }),
    }))
  })
}

// handleWorkspacePick 手机端「在电脑上选目录」：弹目录选择器并把结果回填给 bridge。
// 失败/取消都回填 canceled，避免手机一直转圈（bridge 侧还有 5 分钟 TTL 兜底）。
async function handleWorkspacePick(requestId: string): Promise<void> {
  const payload: Record<string, unknown> = { request_id: requestId }
  try {
    if (!mainWindow) throw new Error('窗口不可用')
    const r = await dialog.showOpenDialog(mainWindow, {
      title: '为手机端添加工作区',
      properties: ['openDirectory', 'createDirectory'],
    })
    if (r.canceled || !r.filePaths[0]) payload.canceled = true
    else {
      payload.path = r.filePaths[0]
      // 用户亲手选的目录 → 进媒体根清单（与桌面端「添加工作区」同一条口径）
      rememberWorkspace(r.filePaths[0])
      lastPickedDir = path.resolve(r.filePaths[0])
    }
  } catch (e) {
    payload.canceled = true
    payload.error = (e as Error).message
  }
  await sendBridgeRequest('workspace_pick_result', payload)
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
  // 内置视觉/UI 技能集合目录（随包分发）：dev = 仓库 desktop/vendor/visual-skills；
  // 打包 = Resources/visual-skills。Go bridge 逐子目录同步到 ~/.agents/skills/<name>。
  // 与上面办公技能同样的「集合目录」设计：新增一个视觉技能只加子目录，两侧接线都不动。
  env.GO_CODE_VISUAL_SKILLS_DIR = bundledVisualSkillsDir()
  // 内置 CreateMode 技能集合目录（随包分发）：dev = 仓库 desktop/vendor/create-mode-skills；
  // 打包 = Resources/create-mode-skills。Go bridge 同步到 ~/.agents/skills/create-mode，
  // 让打包版用户打开 surface 工具后能直接 load_skill(create-mode)。
  env.GO_CODE_CREATE_MODE_SKILLS_DIR = bundledCreateModeSkillsDir()
  // GUI（Finder/Dock）启动的 PATH 通常不含用户 shell 配置（Homebrew/nvm 等）：
  // bridge 的 exec.LookPath("node") 找不到 → 误报「未检测到 Node.js」。PATH 上
  // 没有 node 时，把探测到的 node 所在目录前插（已有则不动——用户 PATH 优先）。
  // 探测逻辑见 node-path.ts（pathForNode/augmentPathWithNode，纯函数可单测）。
  const augmented = augmentPathWithNode(env.PATH)
  if (augmented !== env.PATH) env.PATH = augmented
  // 手机端页面（/m/）：和桌面渲染页一样走**一个磁盘目录**，不 embed、不 zip、不缓存。
  // dev = 仓库 desktop/app/dist-mobile；打包态 = Resources/mobile-web（extraResources）。
  env.HAI_MOBILE_WEB_DIR = bundledMobileWebDir()
  return env
}

// —— 全局设置持久化（~/.go-code/）——
// settings.json：**非密钥**配置（theme/lang/provider 配置 + 模型窗口 map）。
// provider API key 不在这里（BR-08）：key 只住 keys.json（safeStorage 加密，0600），
// 启动时 spawn env 只带当前活跃 provider 的一把，切换/保存时由主进程按需注入 bridge 命令。
// 详见 keys-store.ts 文件头（含「无钥匙串 → base64 降级，不是加密」的口径）。
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
  // 显示名（2026-11）：纯 UI 字段，不改 settings 的键（id 不可变）——历史 metrics /
  // 用户价表 / billing 请求全按 id 索引，改 id 会把历史断成两个 provider。
  label?: string
  openai_compat?: OpenAICompatCapabilities
  /** legacy：存量明文 key 的历史字段。迁移后 settings 不再写它（BR-08）；读侧也不再外泄给 renderer。 */
  api_key?: string
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
// MCPServerCfg 单台 MCP 服务器配置（与 bridge mcp 包 ServerConfig 同构；env/headers
// 明文存 settings.json——v1 决策，见 mcp/config.go 的 Headers 注释）。
interface MCPServerCfg {
  type?: 'stdio' | 'http' | 'streamable_http' | 'streamable-http' | 'sse'
  command?: string
  args?: string[]
  env?: Record<string, string>
  url?: string
  // headers 远端传输（http/sse）每次请求附带的 HTTP 头（如 Authorization）。
  // 与 bridge 的校验口径一致：仅 http/sse 有意义，stdio 忽略。
  headers?: Record<string, string>
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
// agent 段除提醒阈值外的可调项：压缩阈值（占窗口比例）+ 子 agent 并发槽上限。
// 三者的缺省与校验口径以 bridge（desktop/bridge/providers.go 的 agentSettings）为准，
// 这里只负责「读进来的值合法化 + 写出去时不丢字段」。
interface AgentRuntimeSettings {
  reminder_rounds: number
  compress_threshold?: number
  max_subagents?: number
  max_explore_subagents?: number
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
  agent?: AgentRuntimeSettings // Agent 运行时设置（提醒阈值 + 压缩阈值 + 子 agent 并发槽上限）
  stt?: STTSettings // 语音输入（话筒）配置；配置齐全才显示话筒
  editor?: boolean // 内嵌编辑器回滚开关（PR-05b）；缺省 = 开启
}

// 内置五卡（不可删除；其余为自定义 provider）。命名校验见 settings-store.ts 的
// isValidProviderName（与 bridge validProviderName 同规则）。
const BUILTIN_PROVIDERS = ['deepseek', 'openai', 'opencode', 'kimi', 'zhipu'] as const

const DEFAULT_SETTINGS: AppSettings = {
  theme: 'dark',
  lang: 'zh',
  provider: {
    active: 'deepseek',
    deepseek: {
      base_url: 'https://api.deepseek.com/v1',
      models: { 'deepseek-flash': 1000000, 'deepseek-v4.1-flash': 1000000, 'deepseek-v4-flash': 1000000, 'deepseek-v4-pro': 1000000, 'deepseek-v4-flash-vision-exp': 1000000, 'deepseek-chat': 64000 },
      max_tokens: { 'deepseek-flash': 384000, 'deepseek-v4.1-flash': 384000, 'deepseek-v4-flash': 384000, 'deepseek-v4-pro': 384000, 'deepseek-v4-flash-vision-exp': 384000, 'deepseek-chat': 8192 },
      protocol: 'chat_completions',
      input_types: { 'deepseek-v4-flash-vision-exp': ['text', 'image'] }, // DeepSeek 视觉模型
    },
    openai: {
      base_url: 'https://api.openai.com/v1',
      models: { 'gpt-6-astra': 1050000, 'gpt-6.1-sol': 1050000, 'gpt-6-sol': 1050000, 'gpt-6-luna': 1050000, 'gpt-4o': 128000 },
      max_tokens: { 'gpt-6-astra': 128000, 'gpt-6.1-sol': 128000, 'gpt-6-sol': 128000, 'gpt-6-luna': 128000, 'gpt-4o': 16384 },
      protocol: 'chat_completions',
      // GPT-4o 系列多模态（text+image）：Provider 设置可逐模型切换
      input_types: { 'gpt-4o': ['text', 'image'], 'gpt-4o-mini': ['text', 'image'], 'gpt-4.1': ['text', 'image'] },
    },
    opencode: {
      base_url: 'https://opencode.ai/zen/go/v1',
      models: { 'deepseek-flash': 1000000, 'deepseek-v4.1-flash': 1000000, 'deepseek-v4-flash': 1000000, 'deepseek-v4-pro': 1000000, 'deepseek-v4-flash-vision-exp': 1000000, 'glm-5.1': 202752, 'glm-5.2': 1000000, 'glm-5.3': 1000000, 'glm-5.3-flash': 1000000, 'kimi-k3': 1048576, 'kimi-k2.7-code': 262144, 'kimi-k2.6': 262144, 'kimi-k2.5': 262144, 'mimo-v2.6-pro': 1048576, 'mimo-v2.6-flash': 1048576, 'mimo-v2.5': 1000000, 'mimo-v2.5-pro': 1048576, 'hy3': 256000, 'hy4-preview': 1024000, 'qwen3.6-plus': 1000000, 'qwen3.7-max': 1000000, 'qwen3.7-plus': 1000000, 'qwen3.8-max': 1000000, 'minimax-m2.7': 204800, 'minimax-m3': 1000000, 'gpt-5.6-luna': 1050000, 'gpt-6-luna': 1050000, 'claude-haiku-5-5': 1000000, 'grok-4.6': 500000, 'muse-spark-1.2-contributor': 1048576, 'longcat-2.0': 1000000 },
      max_tokens: { 'deepseek-flash': 384000, 'deepseek-v4.1-flash': 384000, 'deepseek-v4-flash': 384000, 'deepseek-v4-pro': 384000, 'deepseek-v4-flash-vision-exp': 384000, 'glm-5.1': 32768, 'glm-5.2': 131072, 'glm-5.3': 131072, 'glm-5.3-flash': 131072, 'kimi-k3': 131072, 'kimi-k2.7-code': 262144, 'kimi-k2.6': 65536, 'kimi-k2.5': 262144, 'mimo-v2.6-pro': 131072, 'mimo-v2.6-flash': 131072, 'mimo-v2.5': 128000, 'mimo-v2.5-pro': 128000, 'hy3': 64000, 'hy4-preview': 64000, 'qwen3.6-plus': 65536, 'qwen3.7-max': 65536, 'qwen3.7-plus': 65536, 'qwen3.8-max': 131072, 'minimax-m2.7': 131072, 'minimax-m3': 131072, 'gpt-5.6-luna': 128000, 'gpt-6-luna': 128000, 'claude-haiku-5-5': 128000, 'grok-4.6': 500000, 'muse-spark-1.2-contributor': 131072, 'longcat-2.0': 131072 },
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
  // compress_threshold 刻意**不写默认值**：缺字段 = bridge 用自身默认 0.8，
  // 用户没动过这项时配置里不留下任何痕迹（改了才出现）。
  agent: { reminder_rounds: 30 },
  sandbox: { mode: 'seatbelt', sensitive_paths: [] }, // 命令沙箱默认开启（2026-08-16 用户决策）
  permissions: { computer_approved_apps: [] }, // Computer Use 已授权 App 空 = 全部拒绝（安全默认）
  stt: { enabled: false }, // 语音输入默认关闭（配置 base_url/model/api_key 后开启）
  editor: true, // 内嵌编辑器（PR-05b）默认开启；设 false = 隐藏所有「编辑」入口（回滚用）
}

// SettingsRead 读盘结果：settings 永远可用（显示兜底），corrupt 标记「文件在但读不出」。
// 区分两种失败是数据安全的前提：
//   - 文件不存在（首次运行/用户删过）→ 默认值起步是安全的，可以写盘；
//   - 文件存在但读不出（JSON 坏 / 权限 / 半截写入）→ 默认值只能用于显示，**绝不能写回**：
//     每条写路径都是「读整份 → 改自己那部分 → 整文件重写」，读失败静默变默认值 = 下一次
//     保存把用户的全部 provider / api_key / MCP 配置整份清成默认（真机事故，不可逆）。
type SettingsRead = { settings: AppSettings; corrupt: boolean; error?: string }

function defaultSettingsClone(): AppSettings {
  return JSON.parse(JSON.stringify(DEFAULT_SETTINGS)) as AppSettings // 深拷贝兜底默认
}

// RawSettingsRead 原始对象读盘（**不合并默认值**）：写侧唯一合法基线。
// 缺文件（首次运行/用户删过）→ {}（可以按「只有用户显式写的键」起步）；
// 文件在但读不出 / 不是 JSON 对象 → corrupt（默认值只能用于显示，绝不能写回）。
type RawSettingsRead = { raw: JsonObject; corrupt: boolean; error?: string }

function readRawSettings(): RawSettingsRead {
  try {
    const parsed = JSON.parse(fs.readFileSync(SETTINGS_FILE(), 'utf8')) as unknown
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      console.error(`[settings] 读取失败（不是 JSON 对象）：${SETTINGS_FILE()} 暂时只按默认值显示；写盘侧会拒绝覆盖，不会清空配置`)
      return { raw: {}, corrupt: true, error: '不是 JSON 对象' }
    }
    return { raw: parsed as JsonObject, corrupt: false }
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e)
    const missing = !fs.existsSync(SETTINGS_FILE())
    if (!missing) {
      // 每条写路径都是「读 → 改 → 写」：读失败静默变默认值 = 下一次保存把用户的全部
      // provider / api_key / MCP 配置整份清成默认（真机事故，不可逆）——所以这里只允许显示。
      console.error(`[settings] 读取失败（${msg}）：${SETTINGS_FILE()} 暂时只按默认值显示；写盘侧会拒绝覆盖，不会清空配置`)
    }
    return { raw: {}, corrupt: !missing, error: missing ? undefined : `读取失败（${msg}）` }
  }
}

// readSettingsResult **显示用**：DEFAULT_SETTINGS 深拷贝后逐段合并磁盘值（BR-01 的读侧一半）。
// 写侧禁止使用它——它返回的是「默认值 ∪ 磁盘值」，拿它当写基线会把默认值固化进用户文件。
function readSettingsResult(): SettingsRead {
  const r = readRawSettings()
  if (r.corrupt) return { settings: defaultSettingsClone(), corrupt: true, error: r.error }
  const raw = r.raw as Partial<AppSettings> & JsonObject
  const s = defaultSettingsClone()
  const rawProvider = raw.provider && typeof raw.provider === 'object' && !Array.isArray(raw.provider) ? raw.provider : {}
  const provider: AppSettings['provider'] = { ...s.provider, ...rawProvider }
  // Agent 运行时段（提醒阈值 / 压缩阈值 / 子 agent 并发槽）：字段级合并 + 逐项合法化，
  // 缺字段回退默认（口径与 bridge 的 loadAgentSettings 一致）。
  // 旧 settings 没有 agent 段时 reminder_rounds 用默认 30；0 明确表示禁用。
  const agent: AgentRuntimeSettings = {
    reminder_rounds: s.agent?.reminder_rounds ?? 30,
    ...(raw.agent && typeof raw.agent === 'object' ? raw.agent : {}),
  }
  if (!Number.isFinite(agent.reminder_rounds) || agent.reminder_rounds < 0) agent.reminder_rounds = 30
  if (agent.compress_threshold !== undefined) {
    const t = Number(agent.compress_threshold)
    // 非法值（含 NaN / <=0 / >1）删掉字段 —— 让 bridge 走自己的默认，
    // 而不是把 0 或 3 写死进配置（0 = 永不压缩，是误配，不是选项）。
    if (!Number.isFinite(t) || t <= 0 || t > 1) delete agent.compress_threshold
  }
  for (const k of ['max_subagents', 'max_explore_subagents'] as const) {
    const v = Number(agent[k])
    if (!Number.isFinite(v) || v < 1) delete agent[k]
  }
  // 内置三卡：默认合并（老配置缺字段/缺协议时回退默认）
  for (const k of BUILTIN_PROVIDERS) provider[k] = { ...s.provider[k], ...(raw.provider?.[k] ?? {}) }
  // 自定义 provider：补齐 ProviderCfg 缺省（手改 settings.json 缺 models/max_tokens/protocol 时 UI 不崩）。
  // 原值展开在前：认不出的字段（auth_type / usage_input_includes_cache / cache_ttl_1h /
  // deleted / 将来新增）必须原样带过 —— 按固定键集重建等于每次读盘都把它们删一遍，
  // 随后任何一次写盘就把这个「删除结果」固化（真机事故：自定义 provider 的 auth_type 静默消失）。
  for (const [k, v] of Object.entries(provider)) {
    if (k === 'active' || BUILTIN_PROVIDERS.includes(k as (typeof BUILTIN_PROVIDERS)[number])) continue
    const cfg = (v ?? {}) as ProviderCfg
    provider[k] = {
      ...cfg,
      base_url: cfg.base_url ?? '',
      models: cfg.models ?? {},
      max_tokens: cfg.max_tokens ?? {},
      protocol: cfg.protocol ?? 'chat_completions',
      input_types: cfg.input_types ?? {},
    }
  }
  // BR-08：明文 key 只住 keys.json —— 即使迁移失败（settings 里还留着明文），也绝不下发 renderer。
  // key 的「已设置」指示走 provider:key-status，明文只在本地测试/改名时按 provider 单独取。
  for (const [k, v] of Object.entries(provider)) {
    if (k === 'active' || typeof v !== 'object' || v === null) continue
    delete (v as ProviderCfg).api_key
  }
  // permissions 字段级合并：旧 settings 缺 computer_approved_apps → 补空数组
  //（避免整段 raw 覆盖丢字段；allowed_apps 残留字段不再读入——已废弃）。
  const permissions: PermissionsSettings = {
    computer_approved_apps: raw.permissions?.computer_approved_apps ?? [],
  }
  // 内部键（schema_version / explicit）只给写侧自己用，不进 renderer 看到的 settings 形状。
  const body: JsonObject = { ...raw }
  delete body.explicit
  delete body.schema_version
  return { settings: { ...s, ...body, provider, agent, permissions } as AppSettings, corrupt: false }
}

function readSettings(): AppSettings {
  return readSettingsResult().settings
}

// quarantineCorruptSettings 把读不出的 settings.json 挪到带时间戳的备份名（**rename，不删**）。
// 为什么挪而不是就地修：用户配置（含明文 key）只有这一个副本，留着才可能人工恢复；
// 挪走后「缺文件」语义成立，客户端可以按默认值继续工作，不必永久卡在只读。
// 返回备份路径；失败返回空串（此时原文件仍在，写侧继续拒绝）。
function quarantineCorruptSettings(): string {
  const stamp = new Date().toISOString().replace(/[:.]/g, '-')
  const backup = `${SETTINGS_FILE()}.corrupt-${stamp}`
  try {
    fs.renameSync(SETTINGS_FILE(), backup)
    return backup
  } catch {
    return ''
  }
}

// settingsFs：迁移与写盘共用的原子文件适配器（目录 0700、文件 0600、同目录 tmp + rename）。
// settings.json 仍有 MCP env/headers（明文，历史决策）与迁移期的 legacy api_key，权限与
// bridge 的 patchJSONFile 保持一致；provider key 另有 keys.json（见 keys-store.ts）。
const settingsFs: SettingsFsOps = {
  mkdir: (dir, mode) => fs.mkdirSync(dir, { recursive: true, mode }),
  readFile: (file) => fs.readFileSync(file, 'utf8'),
  writeFile: (file, text, mode) => fs.writeFileSync(file, text, { mode }),
  rename: (from, to) => fs.renameSync(from, to),
  unlink: (file) => fs.unlinkSync(file),
}

// commitSettings：四条写路径的**唯一出口**。settings-store.commitSettingsWrite 负责
// 「读原始对象（未迁移先迁移落盘）→ mutate → changed 才原子写」，这里只做 main.ts 侧的适配：
//   · 坏文件（JSON 坏 / 读不出）→ 先 quarantine 原文件再拒绝这次写（不删，留作人工恢复）；
//   · 迁移失败 → 拒绝这次写。**不写 schema_version:2**：给未迁移的 v1 文件盖章会让下次启动
//     的 already-v2 直接跳过迁移，固化默认值就再也删不掉了（BR-01 的安全属性，复审 B2）。
// 写基线永远是**原始对象**（不合并 DEFAULT_SETTINGS），否则一次主题修改就把默认值固化进用户文件。
function commitSettings(mutate: (raw: JsonObject) => SettingsApplyResult): { ok: true } | { ok: false; error: string } {
  const r = commitSettingsWrite({
    file: SETTINGS_FILE(),
    backupFile: `${SETTINGS_FILE()}.bak`,
    defaults: DEFAULT_SETTINGS as unknown as JsonObject,
    io: settingsFs,
    log: (m) => console.log(m),
    mutate,
  })
  if (r.ok) return { ok: true }
  if (!r.corrupt) {
    console.error(`[settings] 写盘被拒绝：${r.error}`)
    return { ok: false, error: r.error }
  }
  const backup = quarantineCorruptSettings()
  const where = backup ? `旧文件已备份到 ${backup}（未删除），可从中恢复 Provider 配置` : '原文件保持不变，请修复后重试'
  console.error(`[settings] ${r.error}`)
  return { ok: false, error: `${r.error}；${where}。` }
}

// migrateSettingsOnStartup：存量 v1 → v2（§9.3）。必须在 **bridge spawn 之前**跑：
// bridge 逐次读盘判默认值，迁移没跑完就 spawn 会按 v1 里固化的旧默认启动。
// 迁移失败只记日志、继续用旧文件 —— 迁移失败不能阻止应用启动。
function migrateSettingsOnStartup(): void {
  try {
    const r = runSettingsMigration({
      file: SETTINGS_FILE(),
      backupFile: `${SETTINGS_FILE()}.bak`,
      defaults: DEFAULT_SETTINGS as unknown as JsonObject,
      io: settingsFs,
      log: (m) => console.log(m),
    })
    if (!r.ok) console.error(`[settings] v1→v2 迁移失败（继续用原文件）：${r.error}`)
    else if (r.skipped) console.log(`[settings] v1→v2 迁移跳过：${r.skipped}`)
  } catch (e) {
    console.error('[settings] v1→v2 迁移异常（继续用原文件）', e)
  }
}

// migrateProviderKeysOnStartup：settings.json 的存量明文 `provider.<id>.api_key` → keys.json（BR-08）。
// 必须排在 **migrateSettingsOnStartup 之后、bridge spawn 之前**：
//   · v2 迁移按 §9.3 规则保留 api_key，这里才把它搬进 keys.json；
//   · bridge 启动时 loadSettings 按 settings 重算内存 key，先搬走再 spawn 才不会读到明文。
// 失败只记日志、不阻止启动、不改任何文件（keys-store 保证「宁可重复不可丢 key」）；
// 下次启动 / 下一次 provider:save 会重试。
function migrateProviderKeysOnStartup(): void {
  try {
    const file = SETTINGS_FILE()
    let raw: JsonObject
    try {
      const parsed: unknown = JSON.parse(fs.readFileSync(file, 'utf8'))
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return // 非对象：不动
      raw = parsed as JsonObject
    } catch {
      return // 缺文件（新装）或坏 JSON：没有可安全迁移的明文，原样不动
    }
    const r = migratePlaintextProviderKeys({
      ...keyStoreArgs(),
      settingsRaw: raw,
      settingsFile: file,
      settingsBackupFile: `${file}.bak`,
    })
    if (!r.ok) console.error(`[keys] 明文 key 迁移失败（不阻止启动，下次重试）：${r.error}`)
  } catch (e) {
    console.error('[keys] 明文 key 迁移异常（不阻止启动）', e)
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

// —— keys.json 适配层（BR-08）——
// 逻辑全在 keys-store.ts（纯函数，可在 node:test 里构造坏文件/写失败）；这里只做 Electron 适配：
//   fs → KeyStoreFsOps（原子写 + readdir 找备份），safeStorage → KeyCryptoOps。
function keysFsOps(): KeyStoreFsOps {
  return {
    ...settingsFs,
    readdir: (dir) => fs.readdirSync(dir),
  }
}

/** safeStorage 适配：加密可用性每次现查（钥匙串在会话中可能变得可用/不可用）。 */
function keysCryptoOps(): KeyCryptoOps {
  return {
    isEncryptionAvailable: () => safeStorage.isEncryptionAvailable(),
    encryptString: (plain) => safeStorage.encryptString(plain).toString('base64'),
    decryptString: (data) => safeStorage.decryptString(Buffer.from(data, 'base64')),
  }
}

function keyStoreArgs(): KeyStoreArgs {
  return {
    file: KEYS_FILE(),
    io: keysFsOps(),
    crypto: keysCryptoOps(),
    log: (msg) => console.error(msg),
  }
}

/**
 * legacy 明文 key：迁移失败/未跑时 settings.json 里可能还留着 `provider.<id>.api_key`。
 * 读的是**原始文件**（readSettingsResult 已把明文摘掉，不能读显示用对象）。
 */
function legacyPlainKey(provider: string): string {
  try {
    const raw = JSON.parse(fs.readFileSync(SETTINGS_FILE(), 'utf8')) as JsonObject
    const providers = raw && typeof raw.provider === 'object' && raw.provider !== null ? (raw.provider as JsonObject) : {}
    const cfg = providers[provider]
    if (!cfg || typeof cfg !== 'object') return ''
    const k = (cfg as JsonObject).api_key
    return typeof k === 'string' ? k : ''
  } catch {
    return ''
  }
}

/** 某 provider 的明文 key：keys.json 优先，迁移未完成时回退 settings 遗留明文（保证可用）。 */
function providerKeyOf(provider: string): string {
  if (!isValidProviderName(provider)) return ''
  return keyStoreGetKey(keyStoreArgs(), provider) || legacyPlainKey(provider)
}

// spawn env 的 key 部分：**只注入当前活跃 provider 的一把**（BR-08）。
// 旧实现把 keys.json 的全部条目塞进 env —— bridge 的所有 MCP/插件子进程都会继承这些明文 key，
// 且旧条目会在 bridge 内存为空时顶替新 key（401 根因）。非活跃 provider 的 key 只在切换时
// 随命令 payload 注入（sendCommand → injectProviderKey）。
function loadKeysEnv(): Record<string, string> {
  try {
    const active = String(readSettings().provider.active ?? '')
    return activeProviderKeyEnv(active, providerKeyOf)
  } catch {
    return {}
  }
}

// 删除某 provider 的 key（provider:remove 用；settings 删成功后才调，失败保留 key 不丢）
function removeProviderKey(provider: string): void {
  const r = keyStoreRemoveKey(keyStoreArgs(), provider)
  if (!r.ok) console.error(`[keys] 删除 ${provider} 的 key 失败：${r.error}`)
}

// 该 provider 是否有已存 key（仅布尔指示，回 renderer 展示「已设置 ✓」）。
// 主源 = keys.json；迁移失败时回退 settings 遗留明文（保证「已设置」不误报未设置）。
function providerHasKey(provider: string): boolean {
  if (!isValidProviderName(provider)) return false
  if (keyStoreHasKey(keyStoreArgs(), provider)) return true
  return Boolean(legacyPlainKey(provider).trim())
}

function devUrl(): string | undefined {
  return process.env.VITE_DEV_SERVER_URL
}

function createWindow(): void {
  startupMark('createWindow')
  rendererReady = false
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
    // 刷新后新的 renderer 还没订阅/还没 take：深链必须回到队列，否则 send 给一个
    // 没有监听器的页面 = 静默丢链接（复审：注释说了复位，代码此前漏了这一步）。
    rendererReady = false
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
  // 打开 precise memory info：让渲染进程能读到 performance.memory.usedJSHeapSize
  // （Chromium 默认不给，隐私考虑）。预算体检用它区分「我们的数据」与
  // 「渲染进程的底噪」（Chromium + React + 代码 + DOM）。
  app.commandLine.appendSwitch('enable-precise-memory-info')
  // V8 堆**硬上限**（2026-11-13 卡死事故后加）。
  //
  // ## 为什么必须设
  //
  // V8 的默认上限是按**机器内存**推算的（16 GB 机器 ⇒ 约 2–4 GB）。不设的话，
  // V8 没有任何理由做 major GC ⇒ **垃圾能堆好几小时**。实测：渲染进程
  // `usedJSHeapSize` 从 51 MB 一路涨到 625 MB（≈11 MB/min 的分配速率），
  // 而**活对象只有 ~30 MB**（app store 17 MB + React 树/DOM 约 6 MB + 其余）
  // ⇒ **595 MB 是没收的垃圾**。
  //
  // 机器一满（swap 颠簸），这些页被换出 ⇒ 等 V8 终于回收时，major GC 得遍历
  // 3.7 GB **已换出**的页 ⇒ 每轮 ~48 秒纯磁盘读（实测磁盘 73 MB/s）⇒
  // 渲染进程主线程卡死 ⇒ **整个应用点不动**。
  //
  // ⇒ 硬上限把「GC 要遍历的量」钉在 768 MB 以内：换页再慢，单轮也有界，
  // 而且这个量级的页多半还在内存里 ⇒ **死亡螺旋不会形成**。
  //
  // ## 为什么是 768 MB 而不是更小
  //
  // 活对象实测 ~30 MB，最坏（大会话 + 大 DOM）也就百 MB 级 ⇒ 768 有 7 倍余量，
  // 不会把应用顶到 V8 的 OOM 崩溃线（v8.dev/blog/heap-size-limit：到顶是
  // **崩溃**，不是降级 ⇒ 这是兜底，不是常规路径；常规路径是前端的字节预算 +
  // 堆轴降级）。
  app.commandLine.appendSwitch('js-flags', '--max-old-space-size=768')
  // 渲染进程的 console 透传到终端（2026-11-12）。
  //
  // 为什么需要：视图内存预算的降级/淘汰会打一行 `[budget] …`，而渲染进程的
  // console 只在 devtools 里 —— **终端看不到** ⇒ 机制有没有在工作无法观测。
  // （2026-11-12 就因为看不见这行，误判成「机制没生效」，绕了一大圈。）
  // 只透传带标记前缀的行，避免刷屏。
  mainWindow.webContents.on('console-message', (_e: unknown, ...rest: unknown[]) => {
    for (const a of rest) {
      if (typeof a === 'string' && a.startsWith('[budget]')) {
        console.log(`[renderer] ${a}`)
        return
      }
    }
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

  // 放行闸门：把启动期间排队的命令按序补发（id 原样 ⇒ 回执对得上号）。
  if (bridgeBootQueue.length) {
    const queued = bridgeBootQueue.splice(0)
    for (const line of queued) {
      try {
        bridge!.stdin.write(line)
      } catch {
        /* 忽略：bridge 刚起来，写失败让它自己报错 */
      }
    }
  }
  bridgeBooting = false

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
    if (pending.length === 0) return
    // 送不到（窗口不在/已销毁）就丢弃这一批：与原逐行行为一致，防缓冲泄漏。
    // 这里在 16ms 定时器里执行，抛异常会变成 uncaughtException —— 统一走 sendToRenderer。
    if (sendToRenderer('bridge:event', { lines: pending.splice(0) })) return
    pending.length = 0
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
    // 手机端请求「在电脑上选目录」：主进程弹选择器 → 回填 workspace_pick_result。
    // 不回给渲染进程：结果由 bridge 广播 workspace_picked，桌面左栏走常规刷新。
    if (t.includes('"event_type":"workspace_pick_request"')) {
      let rid = ''
      try {
        rid = String((JSON.parse(t) as { request_id?: string }).request_id ?? '')
      } catch {
        /* 解析失败忽略 */
      }
      if (rid) void handleWorkspacePick(rid)
      return
    }
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
      sendToRenderer('bridge:event', t)
      return
    }
    // kit 状态变化（R08d）：bridge 在「kit 的 bin 已经不存在」时主动推一份新状态。
    // 主进程据此更新预览超时的依据（卸载后不该再为它付 90s），并照常转发给渲染层
    //（store 用它更新横幅，见 useAppStore 的 office_kit_status 分支）。
    if (t.includes('"event_type":"office_kit_status"')) {
      try {
        const ev = JSON.parse(t) as { data?: { kit?: { available?: boolean } } }
        if (typeof ev.data?.kit?.available === 'boolean') officeKitAvailable = ev.data.kit.available
      } catch { /* 解析失败不影响转发 */ }
    }
    if (t.includes('"event_type":"surface_agent_event"')) {
      // 插件 Agent 事件（P1b2，§6.6）：**独立通道**，不进 bridge:event、也不进 16ms 批量缓冲。
      // 归属判定不在这里做（main 不认识 manifest）：data 原样交给渲染层，loader 按
      // `surface_id === item.id` 过滤后再分发给 host.onEvent 的监听器。
      // 但「这个 id 在本进程里从没领过 token」= 没有装载它的 loader 实例（或已卸载且再没加载）
      // ⇒ 一条都没人会认，丢弃即可；留着只会让 Go 端的错误 id 变成静默广播。
      let forwarded = false
      try {
        const ev = JSON.parse(t) as { data?: { surface_id?: string; event?: unknown } }
        const data = ev.data
        if (data && typeof data.surface_id === 'string' && data.event && surfaceAgentTokens.has(data.surface_id)) {
          forwarded = sendToRenderer(SURFACE_AGENT_EVENT_CHANNEL, data)
        }
      } catch {
        /* 解析失败：没有 surface_id 就没有归属，丢弃比乱投递好 */
      }
      // sent=false（窗口不在/已销毁）不用额外处理：sendToRenderer 的语义就是「送不到就丢掉」，
      // 与上面 bridge:event 的批量缓冲同一口径 —— 窗口生命周期问题不该升级成命令失败。
      void forwarded
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
        sendToRenderer('bridge:event', t)
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
    // 退出时窗口常在拆除中（webContents 已销毁）：此前 if (mainWindow) 挡不住 destroyed，
    // send 直接抛在 exit 回调里。统一走判空通道。
    sendToRenderer('bridge:exit', { code, err: err.slice(-4000) })
  })
  void pushProviderKeys()
  return true
}

// pushProviderKeys：spawn 后把 keys.json 的 key 逐个注入 bridge 内存（BR-08，**只走命令不进 env**）。
//
// 为什么需要：spawn env 只带当前活跃 provider 的一把 key（避免 key 泄漏给所有 MCP/插件子进程），
// 但 bridge 会把会话绑定到各自的 provider（`bs.provider` 重启后恢复）——会话的 provider 不是
// 活跃那家时，keyLocked 会返回空 → 「未配置 API key」/401。启动补推一次让每条会话都能拿到 key；
// 之后 key 轮换由 provider:save → set_provider 与 switch_model 按需注入。
//
// 走 sendBridgeRequest（带 id）而不是 sendCommand：响应在登记表里被吃掉，不会当成
// command_response 事件打到 renderer（否则每次启动都会多出一串 set_provider 响应）。
// 日志只记数量，**不记 key 明文**。
async function pushProviderKeys(): Promise<void> {
  try {
    const settings = readSettings()
    const names = Object.keys(settings.provider).filter((k) => k !== 'active')
    let pushed = 0
    for (const provider of names) {
      const cfg = settings.provider[provider]
      if (!cfg || typeof cfg !== 'object' || cfg.deleted) continue
      const key = providerKeyOf(provider)
      if (!key) continue
      const r = await sendBridgeRequest('set_provider', { provider, api_key: key })
      if (r.ok) pushed++
    }
    if (pushed > 0) console.log(`[keys] 已向 bridge 注入 ${pushed} 个 provider 的 key`)
  } catch (e) {
    console.error('[keys] provider key 预热失败（不影响启动，切 provider 时会再注入）', e)
  }
}

// sendCommand：renderer → bridge 的**唯一普通命令出口**（sendBridgeRequest 只跑 oauth_*/启动预热）。
// BR-08：转发前按需注入 provider key（白名单见 keys-store.PROVIDER_KEY_COMMANDS）——
//   · set_provider（保存/切换 provider）、switch_model（切模型）缺 api_key → 按 payload.provider 取；
//   · list_models / provider_billing（设置页按 provider 操作）同理：env 只有活跃那家的一把 key；
//   · payload 已带 api_key（用户输入 / 显式空串清 key）一律不覆盖。
// key 只进 stdin，**不进日志/错误消息**（这里也不打印 cmd）。
function sendCommand(cmd: unknown): { ok: boolean; error?: string } {
  if (!bridge || bridge.killed) {
    // 启动中（可能在 go build）⇒ **排队**，别把前端推进永久错误态。
    if (bridgeBooting) {
      bridgeBootQueue.push(JSON.stringify(injectProviderKey(cmd, providerKeyOf)) + '\n')
      return { ok: true }
    }
    return { ok: false, error: 'bridge 未运行' }
  }
  bridge.stdin.write(JSON.stringify(injectProviderKey(cmd, providerKeyOf)) + '\n')
  return { ok: true }
}

// —— 预览请求的超时口径（R08d 复评 P2）——
//
// 两条路的耗时差一个数量级，所以不能给一个数：
//   - 近似渲染（受管 Python 转 HTML）：正常 <1s，卡死时 30s 就该报错让人重试；
//   - kit 路径（soffice → PDF）：单次上限 60s（officekit.DefaultTimeout）+ 冷启 3–6s，
//     90s 才是它的界。
// 之前三条预览 IPC 无条件写 90s —— 等于把「卡死的近似转换」也拖到 90s（相对 R08a 的
// 30s 是回归）。这里按**预计会走哪条路**选：kit 可用才付 90s，其余保持 30s。
//
// 判定依据来自本进程已知的 kit 状态：渲染层启动时会查一次（App.tsx 的 fetchOfficeKit），
// 设置页「重新探测」与 bridge 主动推的 office_kit_status 事件也会更新它。
const PREVIEW_TIMEOUT_MS = 30_000
const KIT_PREVIEW_TIMEOUT_MS = 90_000
// undefined = 还没问过（第一次预览前补问一次；问不到就按 30s，不把预览卡在这上面）。
let officeKitAvailable: boolean | undefined

function previewTimeoutMs(): number {
  return officeKitAvailable === true ? KIT_PREVIEW_TIMEOUT_MS : PREVIEW_TIMEOUT_MS
}

// ensureOfficeKitKnown 首次需要选超时时问一次（之后走缓存/事件）。
async function ensureOfficeKitKnown(): Promise<void> {
  if (officeKitAvailable !== undefined) return
  const r = await sendBridgeRequest('office_kit_status', {})
  const kit = r.data?.kit as { available?: boolean } | undefined
  officeKitAvailable = r.ok ? kit?.available === true : false
}

// timeoutMs 默认 30s（历史口径）。需要更长时间的调用（R08d：soffice→PDF 单次上限 60s，
// 冷启 3–6s）必须显式放宽 —— 否则主进程先放弃、bridge 那边还在烧 CPU 转一份没人要的文件。
// 预览类请求一律走 previewTimeoutMs()（见上），不要在这里写死数字。
function sendBridgeRequest(type: string, payload: Record<string, unknown> = {}, timeoutMs = 30_000): Promise<BridgeCommandResponse> {
  if (!bridge || bridge.killed) return Promise.resolve({ ok: false, error: 'bridge 未运行' })
  const id = bridgeRequestSeq--
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      pendingBridgeRequests.delete(id)
      resolve({ ok: false, error: 'bridge 请求超时' })
    }, timeoutMs)
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

// isTrustedIpcSender IPC 来源判定：只认主窗口的**主 frame**。
// 内嵌网页（WebContentsView）与子 frame 拿不到 `window.desktop`，但一旦将来有人把 preload
// 暴露给它们，这里就是最后一道门 —— 媒体的两条 IPC 会把本地文件变成可读字节，不能只靠
// 「渲染进程是我们自己的代码」这条假设。
function isTrustedIpcSender(e: Electron.IpcMainInvokeEvent): boolean {
  const wc = mainWindow?.webContents
  if (!wc) return false
  return e.sender === wc && e.senderFrame === wc.mainFrame
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
  // —— 文件树写操作（左栏「工作区文件」的文件夹管理，实现与守卫见 fs-ops.ts）——
  // 全部 async（fs-ops 对外统一返回 Promise）：O(1) 的 rename/mkdir 立刻返回，
  // O(bytes) 的拷贝（副本/拷贝到别处/跨卷移动/访达导入）走 fs.promises.cp 分块流式 ——
  // 大目录不会再占住主进程（此前用 cpSync 时窗口输入与其它 IPC 会一起迟滞）。
  // workspace 由渲染层随 payload 注入（与 fs:read-ref 同口径），主进程据此做包含判断 ——
  // 文件树的作用域就是用户眼前这棵树，写操作不允许越出它。
  ipcMain.handle('fs:rename', (_e, p: { workspace: string; path: string; name: string }) =>
    renameEntry(p?.workspace ?? '', p?.path ?? '', p?.name ?? '', cfgDir()))
  ipcMain.handle('fs:move', (_e, p: { workspace: string; src: string; destDir: string }) =>
    moveEntry(p?.workspace ?? '', p?.src ?? '', p?.destDir ?? '', cfgDir()))
  ipcMain.handle('fs:mkdir', (_e, p: { workspace: string; dir: string; name: string }) =>
    createDir(p?.workspace ?? '', p?.dir ?? '', p?.name ?? '', cfgDir()))
  ipcMain.handle('fs:create-file', (_e, p: { workspace: string; dir: string; name: string }) =>
    createFile(p?.workspace ?? '', p?.dir ?? '', p?.name ?? '', cfgDir()))
  // 从访达拖入：源在工作区外，逐个拷进 destDir（一个 IPC 处理整批，避免 N 次往返）。
  // 逐个报告失败而不是整批失败：拖 5 个进来、其中 1 个同名，另外 4 个应该照样进去。
  ipcMain.handle('fs:import', async (_e, p: { workspace: string; destDir: string; sources: string[] }) => {
    const ws = p?.workspace ?? ''
    const destDir = p?.destDir ?? ''
    const sources = Array.isArray(p?.sources) ? p.sources : []
    const imported: string[] = []
    const failed: { name: string; error: string }[] = []
    for (const src of sources) {
      // 逐个 await：源可能很大，并发拷反而拖慢磁盘；也让失败报告的顺序与拖入顺序一致。
      const r = await importEntry(ws, destDir, src, cfgDir())
      if (r.ok && r.path) imported.push(r.path)
      else failed.push({ name: String(src).split('/').pop() || String(src), error: r.error ?? '导入失败' })
    }
    return { ok: true, imported, failed }
  })
  ipcMain.handle('fs:copy', (_e, p: { workspace: string; src: string; destDir: string }) =>
    copyEntry(p?.workspace ?? '', p?.src ?? '', p?.destDir ?? '', cfgDir()))
  ipcMain.handle('fs:duplicate', (_e, p: { workspace: string; path: string }) =>
    duplicateEntry(p?.workspace ?? '', p?.path ?? '', cfgDir()))
  // 删除 = 移进系统废纸篓（可恢复，与 Cmd+Delete 的 Mac 口径一致），**不做 rm**。
  // 二次确认由渲染层弹（ConfirmModal），主进程只负责守卫 + 动作：IPC 也可能被将来的
  // 快捷键/菜单复用，把「确认」放主进程会让 e2e 与非交互调用都无法绕开。
  ipcMain.handle('fs:trash', async (_e, p: { workspace: string; path: string }) => {
    const g = guardTrash(p?.workspace ?? '', p?.path ?? '', cfgDir())
    if (!g.ok || !g.path) return g
    try {
      await shell.trashItem(g.path)
    } catch (e) {
      return { ok: false, error: (e as Error).message, code: 'IO' }
    }
    return { ok: true, path: g.path }
  })
  ipcMain.handle('fs:pick-dir', async (e) => {
    const win = BrowserWindow.fromWebContents(e.sender) ?? undefined
    const r = win ? await dialog.showOpenDialog(win, { properties: ['openDirectory', 'createDirectory'] }) : await dialog.showOpenDialog({ properties: ['openDirectory', 'createDirectory'] })
    return r.canceled || r.filePaths.length === 0 ? null : r.filePaths[0]
  })
  ipcMain.handle('bridge:start', async () => {
    // 闸门**在 await 之前**就上：ensureBridgeBin 可能要 go build（数秒），
    // 这段时间渲染层的命令要排队而不是被判成「bridge 未运行」。
    bridgeBooting = true
    const res = await ensureBridgeBin()
    if (!res.ok || !res.bin) {
      bridgeBooting = false
      bridgeBootQueue.length = 0 // 起不来 ⇒ 排队的命令也送不了，丢掉
      return res
    }
    const ok = spawnBridge(res.bin)
    if (!ok) {
      bridgeBooting = false
      bridgeBootQueue.length = 0
      return { ok: false, error: 'bridge 启动失败' }
    }
    return { ok: true }
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
    if (r.canceled || !r.filePaths[0]) return null
    // **媒体协议的信任锚就在这里**：只有经系统选择器确认过的目录才进 known-workspaces。
    // 渲染进程传什么 workspace 都不能自己登记（评审裁决 1：workspace=/Users 曾能登记成功）。
    rememberWorkspace(r.filePaths[0])
    lastPickedDir = path.resolve(r.filePaths[0])
    return r.filePaths[0]
  })
  ipcMain.handle('home-dir', () => app.getPath('home'))
  ipcMain.handle('create-workspace-folder', (e, parent: string, name: string) => {
    if (!isTrustedIpcSender(e)) return { ok: false, error: '不受信任的调用来源' }
    const clean = String(name ?? '').trim()
    const base = String(parent ?? '')
    if (!base || !path.isAbsolute(base)) return { ok: false, error: '父目录无效' }
    // 父目录必须是系统选择器**刚刚**给的那个（同一个洞的边角，复评顺手改 1）
    let samePicked = false
    if (lastPickedDir) {
      try {
        samePicked = fs.realpathSync(base) === fs.realpathSync(lastPickedDir)
      } catch {
        samePicked = path.resolve(base) === lastPickedDir
      }
    }
    if (!samePicked) return { ok: false, error: '父目录必须来自系统选择器（请先「选择工作区」）' }
    if (!clean || clean.includes('/') || clean === '.' || clean === '..') return { ok: false, error: '无效的文件夹名称' }
    const target = path.join(base, clean)
    try {
      fs.mkdirSync(target) // recursive:false → 已存在 EEXIST，抛错
      rememberWorkspace(target) // 新建即用户亲手指定的目录（父目录来自同一个选择器）
      return { ok: true, path: target }
    } catch (e) {
      const m = (e as NodeJS.ErrnoException).code === 'EEXIST' ? '目录已存在' : (e as Error).message
      return { ok: false, error: m }
    }
  })
  // —— 全局设置 ——
  ipcMain.handle('settings:get', () => readSettings())
  // 输入框历史：本地文件持久化（scope = workspace + session + 可选 agent task）。
  // 渲染层同步读写 localStorage 镜像；这里只负责把镜像对齐到可备份的文件。
  ipcMain.handle('input-history:get', (_e, p: { scope?: unknown } = {}) => {
    const scope = typeof p?.scope === 'string' ? p.scope : ''
    if (!scope || scope.length > 4096) return { ok: false, error: 'invalid scope' }
    return ipcInputHistoryGet(inputHistoryFilePath(cfgDir()), scope)
  })
  ipcMain.handle('input-history:set', (_e, p: { scope?: unknown; items?: unknown } = {}) => {
    const scope = typeof p?.scope === 'string' ? p.scope : ''
    if (!scope || scope.length > 4096) return { ok: false, error: 'invalid scope' }
    if (!Array.isArray(p?.items)) return { ok: false, error: 'invalid items' }
    return ipcInputHistorySet(inputHistoryFilePath(cfgDir()), scope, p.items)
  })
  ipcMain.handle('input-history:forget', (_e, p: { prefix?: unknown; subtree?: unknown } = {}) => {
    const prefix = typeof p?.prefix === 'string' ? p.prefix : ''
    if (!prefix || prefix.length > 4096) return { ok: false, error: 'invalid prefix' }
    return ipcInputHistoryForget(inputHistoryFilePath(cfgDir()), prefix, p?.subtree === true)
  })
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
  // 办公组件状态（R08d）：设置页展示 + 「重新探测」。kit 缺失是正常结果（ok:true +
  // available:false），由前端决定横幅/引导文案；只有 bridge 真的不可用才是 error。
  ipcMain.handle('office-kit:status', async (_e, req: { refresh?: boolean } = {}) => {
    const started = await ensureBridgeBin()
    if (!started.ok || !started.bin) return { ok: false, error: started.error ?? 'bridge 不可用' }
    if (!spawnBridge(started.bin)) return { ok: false, error: 'bridge 启动失败' }
    const response = await sendBridgeRequest('office_kit_status', { refresh: req?.refresh === true })
    if (!response.ok) return { ok: false, error: response.error ?? '读取办公组件状态失败' }
    // 顺手更新预览超时的依据（见 previewTimeoutMs）：设置页「重新探测」后热生效。
    officeKitAvailable = (response.data?.kit as { available?: boolean } | undefined)?.available === true
    return { ok: true, kit: response.data?.kit }
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
  //      「一问一答、可能很久」的请求 —— 主进程这条通道自带超时（previewTimeoutMs：
  //      kit 可用 90s / 否则 30s）与错误归并；
  //   ② 产物路径只给内嵌浏览器用（不进 store 状态树/事件流），不需要广播给所有窗口。
  // 超时口径：30s 对已 bootstrap 的机器足够（实测小表 <1s）；未 bootstrap 的机器会
  // 报「bridge 请求超时」，用户重试一次即可（第二次运行时已就绪）—— 与「首次 run_python
  // 调用慢」的既有体验一致。R08d 起这条 30s 只给近似路径：kit 可用时放宽到 90s
  //（soffice 单次上限 60s + 冷启），见 previewTimeoutMs。
  ipcMain.handle('xlsx:preview', async (_e, req: { path?: string; workspace?: string; approximate?: boolean } = {}) => {
    const started = await ensureBridgeBin()
    if (!started.ok || !started.bin) return { ok: false, error: started.error ?? 'bridge 不可用' }
    if (!spawnBridge(started.bin)) return { ok: false, error: 'bridge 启动失败' }
    const path = String(req?.path ?? '')
    if (!path) return { ok: false, error: 'path 为空' }
    // 超时按**预计走的路径**选（kit 可用 90s，否则 30s）：见 previewTimeoutMs 的注释。
    await ensureOfficeKitKnown()
    const response = await sendBridgeRequest('xlsx_preview', {
      path,
      ...(req?.workspace ? { workspace: String(req.workspace) } : {}),
      // 面板渲染器只认 HTML（它把产物读回前端塞 iframe）：approximate=true 跳过 kit，
      // 否则 kit 可用时拿到的是 PDF。原生视图不传，照旧拿 PDF（见 preview_convert 注释）。
      ...(req?.approximate === true ? { approximate: true } : {}),
    }, previewTimeoutMs())
    if (!response.ok) return { ok: false, error: response.error ?? '工作簿转换失败' }
    const htmlPath = response.data?.html_path
    if (typeof htmlPath !== 'string' || !htmlPath) return { ok: false, error: 'bridge 未返回 HTML 路径' }
    // renderer：libreoffice（kit → PDF）| approximate（既有近似 HTML）。前端据此出横幅
    //（kit 缺失时如实说明「当前为近似渲染」）—— 缺省按 approximate，不谎称保真。
    const renderer = response.data?.renderer === 'libreoffice' ? 'libreoffice' : 'approximate'
    return { ok: true, html_path: htmlPath, renderer }
  })
  // docx 文档预览：renderer 点「文档预览」→ bridge 用受管 Python（mammoth）把文档
  // 转成 HTML → 返回产物路径 → renderer 送内嵌浏览器 file://（与 PDF 预览同一条通道）。
  //
  // 为什么走主进程直发（sendBridgeRequest，负 id）而不是 renderer 的 command 通道：
  //   ① 转换要跑受管 Python，**首次还会先 bootstrap 运行时**（建 venv + 装约 42MB），
  //      可能几分钟；renderer 的 pending 表是「事件驱动 + 无超时」的，不适合这种
  //      「一问一答、可能很久」的请求 —— 主进程这条通道自带超时（previewTimeoutMs：
  //      kit 可用 90s / 否则 30s）与错误归并；
  //   ② 产物路径只给内嵌浏览器用（不进 store 状态树/事件流），不需要广播给所有窗口。
  // 超时口径：30s 对已 bootstrap 的机器足够（实测小文档 <1s）；未 bootstrap 的机器会
  // 报「bridge 请求超时」，用户重试一次即可（第二次运行时已就绪）—— 与「首次 run_python
  // 调用慢」的既有体验一致。R08d 起这条 30s 只给近似路径：kit 可用时放宽到 90s
  //（soffice 单次上限 60s + 冷启），见 previewTimeoutMs。
  // 上面这套理由三条预览通路（xlsx/docx/pptx）**逐字共用**，改一处要三处一起想。
  ipcMain.handle('docx:preview', async (_e, req: { path?: string; workspace?: string; approximate?: boolean } = {}) => {
    const started = await ensureBridgeBin()
    if (!started.ok || !started.bin) return { ok: false, error: started.error ?? 'bridge 不可用' }
    if (!spawnBridge(started.bin)) return { ok: false, error: 'bridge 启动失败' }
    const path = String(req?.path ?? '')
    if (!path) return { ok: false, error: 'path 为空' }
    // 超时按**预计走的路径**选（kit 可用 90s，否则 30s）：见 previewTimeoutMs 的注释。
    await ensureOfficeKitKnown()
    const response = await sendBridgeRequest('docx_preview', {
      path,
      ...(req?.workspace ? { workspace: String(req.workspace) } : {}),
      // 面板渲染器只认 HTML（它把产物读回前端塞 iframe）：approximate=true 跳过 kit，
      // 否则 kit 可用时拿到的是 PDF。原生视图不传，照旧拿 PDF（见 preview_convert 注释）。
      ...(req?.approximate === true ? { approximate: true } : {}),
    }, previewTimeoutMs())
    if (!response.ok) return { ok: false, error: response.error ?? '文档转换失败' }
    const htmlPath = response.data?.html_path
    if (typeof htmlPath !== 'string' || !htmlPath) return { ok: false, error: 'bridge 未返回 HTML 路径' }
    // renderer：libreoffice（kit → PDF）| approximate（既有近似 HTML）。前端据此出横幅
    //（kit 缺失时如实说明「当前为近似渲染」）—— 缺省按 approximate，不谎称保真。
    const renderer = response.data?.renderer === 'libreoffice' ? 'libreoffice' : 'approximate'
    return { ok: true, html_path: htmlPath, renderer }
  })
  // pptx 演示文稿预览：与上面 docx 那条逐字同构（同样的直发理由与 30s 口径），
  // 唯一差别是转换器：Python 侧 python-pptx **不能渲染**，产物是按真实 OOXML 几何做的
  // **近似版式重建**（做不到的几件事印在页面顶部，见 builtin.PptxPreviewer）。前端按钮
  // 文案也据此写「近似」—— 通路不保真，入口就不该许下保真的承诺。
  ipcMain.handle('pptx:preview', async (_e, req: { path?: string; workspace?: string; approximate?: boolean } = {}) => {
    const started = await ensureBridgeBin()
    if (!started.ok || !started.bin) return { ok: false, error: started.error ?? 'bridge 不可用' }
    if (!spawnBridge(started.bin)) return { ok: false, error: 'bridge 启动失败' }
    const path = String(req?.path ?? '')
    if (!path) return { ok: false, error: 'path 为空' }
    // 超时按**预计走的路径**选（kit 可用 90s，否则 30s）：见 previewTimeoutMs 的注释。
    await ensureOfficeKitKnown()
    const response = await sendBridgeRequest('pptx_preview', {
      path,
      ...(req?.workspace ? { workspace: String(req.workspace) } : {}),
      // 面板渲染器只认 HTML（它把产物读回前端塞 iframe）：approximate=true 跳过 kit，
      // 否则 kit 可用时拿到的是 PDF。原生视图不传，照旧拿 PDF（见 preview_convert 注释）。
      ...(req?.approximate === true ? { approximate: true } : {}),
    }, previewTimeoutMs())
    if (!response.ok) return { ok: false, error: response.error ?? '演示文稿转换失败' }
    const htmlPath = response.data?.html_path
    if (typeof htmlPath !== 'string' || !htmlPath) return { ok: false, error: 'bridge 未返回 HTML 路径' }
    // renderer：libreoffice（kit → PDF）| approximate（既有近似 HTML）。前端据此出横幅
    //（kit 缺失时如实说明「当前为近似渲染」）—— 缺省按 approximate，不谎称保真。
    const renderer = response.data?.renderer === 'libreoffice' ? 'libreoffice' : 'approximate'
    return { ok: true, html_path: htmlPath, renderer }
  })
  // —— 媒体预览（R08c）：URL 通道 + 探测 ——
  //
  // 两条门禁（评审裁决 1），缺一条都能让协议退化成任意目录的读取通道：
  //   ① 调用来源：必须是主窗口的**主 frame**（内嵌网页/子 frame 不得借道）；
  //   ② 工作区：必须出现在 known-workspaces（用户亲手用系统选择器打开过）——
  //      渲染进程自己传的字符串不再是授权凭据。
  //
  // media:url：渲染进程要 `<video src>` 的唯一入口。路径校验在生产实现里（mediaUrlForFile
  // 也顺带登记协议的工作区根）：只给工作区内、扩展名在白名单里的文件。
  ipcMain.handle('media:url', (e, p: { path?: string; workspace?: string } = {}) => {
    if (!isTrustedIpcSender(e)) return { ok: false, error: '不受信任的调用来源' }
    return mediaUrlForFile(String(p?.path ?? ''), String(p?.workspace ?? ''), isKnownWorkspace)
  })
  // media:probe：ffprobe 元信息 + 编解码判断。走主进程直发（sendBridgeRequest，负 id），
  // 与 xlsx/docx/pptx 三条预览通路同一个理由：一问一答、自带超时、不广播进事件流。
  // **降级不是错误**：ffprobe 不存在时 bridge 回 ok + available:false，这里照样把 data 交回
  // 前端 —— 面板要能一边说「读不到媒体信息」一边继续放。
  ipcMain.handle('media:probe', async (e, p: { path?: string; workspace?: string } = {}) => {
    if (!isTrustedIpcSender(e)) return { ok: false, error: '不受信任的调用来源' }
    const workspace = String(p?.workspace ?? '')
    if (!isKnownWorkspace(workspace)) return { ok: false, error: '工作区未登记（只能用系统选择器打开过的工作区预览媒体）' }
    const started = await ensureBridgeBin()
    if (!started.ok || !started.bin) return { ok: false, error: started.error ?? 'bridge 不可用' }
    if (!spawnBridge(started.bin)) return { ok: false, error: 'bridge 启动失败' }
    const path = String(p?.path ?? '')
    if (!path) return { ok: false, error: 'path 为空' }
    const response = await sendBridgeRequest('media_probe', { path, workspace })
    return { ok: response.ok, error: response.error, data: response.data }
  })
  // media:stats：协议计数器（请求数 / Range 数 / 首字节数 / 字节数）。e2e 用它证明
  // 「切走再切回不重新请求首字节」——数的是生产里真正处理请求的那段代码，不是另建的替身。
  ipcMain.handle('media:stats', (e) => (isTrustedIpcSender(e) ? mediaStats() : { requests: 0, rangeRequests: 0, firstByteHits: 0, bytesServed: 0 }))
  // surface:index：渲染层 loader 的唯一数据源（装了哪些插件 / hash 变没变 / 声明了哪些
  // 命令与 op）。只读、只回元数据 —— panel.js 的字节走 `hai-surface://`（协议侧另判一次
  // enabled + 扩展名 + realpath 包含）。坏 manifest 的项带 error 照发（跳过它、不拖累别人）。
  ipcMain.handle('surface:index', (e) => {
    if (!isTrustedIpcSender(e)) return { version: 1, items: [] }
    return readSurfaceIndex()
  })
  // surface:command：插件调宿主只读命令的**渲染层入口**。
  //
  // 这里是**转发**不是边界：白名单与审计在 bridge（surfaces_command.go）——渲染层跑的
  // 就是插件自己的进程，那边的预检只能算「省一次 IPC」。主进程这边只做三件事：
  //   ① 来源可信（主窗口主 frame，复用 media 的同一条门禁）；
  //   ② 形状合法（id 过 pattern，name 非空）—— 非法形状不值得发一条 bridge 命令；
  //   ③ 用**负 id** 的一问一答通道（sendBridgeRequest），不让它进渲染层的事件流：
  //      插件命令没有 run/session 归属，进了就会以「会话事实」的身份参与视图构建。
  ipcMain.handle('surface:command', async (e, p: { id?: unknown; name?: unknown; args?: unknown; workspace?: unknown } = {}) => {
    if (!isTrustedIpcSender(e)) return { ok: false, reason: 'denied' }
    const id = typeof p?.id === 'string' ? p.id : ''
    const name = typeof p?.name === 'string' ? p.name.trim() : ''
    if (!SURFACE_ID_RE.test(id) || !name) return { ok: false, reason: 'denied' }
    const args = p?.args && typeof p.args === 'object' && !Array.isArray(p.args) ? (p.args as Record<string, unknown>) : {}
    const workspace = typeof p?.workspace === 'string' ? p.workspace : ''
    const response = await sendBridgeRequest('surface_command', { id, name, args, ...(workspace ? { workspace } : {}) }, 10_000)
    if (!response.ok) {
      // reason 是 bridge 给的固定集合（denied / unknown_id / timeout / error）。
      // 桥**没回**（超时 / 进程不在）时没有 code：按 error 文案分成 timeout / error，
      // 而不是一律报 denied —— 「git 太慢」与「越权」在面板上必须是两句话，
      // 否则用户会去翻权限而问题在性能。ok 恒 false，安全上不放宽任何东西。
      if (response.code) return { ok: false, reason: response.code }
      const detail = response.error ?? ''
      return { ok: false, reason: /超时|timeout|timed out/i.test(detail) ? 'timeout' : 'error' }
    }
    return { ok: true, data: response.data?.result }
  })
  // surface:agent-token / surface:agent —— 插件 ↔ Agent 的通道（P1b2，§6.6）。
  //
  // 身份只认 **token**：main 在插件加载前签发 per-plugin token（`crypto.randomUUID()`），
  // 渲染层的 loader 闭包持有、payload 不带 plugin_id；这里用 token 换回 surface_id 再注入。
  // 于是「渲染进程能伪造 id」不成立 —— 伪造一个别人的 id 需要别人的 token（main 只发给
  // 领过它的那个 loader 实例，而插件拿不到 claim 出去的 api 对象，见 src/lib/bridge.ts）。
  //
  // 同一个 id 重复领取回**同一个** token：loader 每次加载（含 hash 变化的热更新、卸载后
  // 重装）都会领一次，作废旧 token 会让在飞的 run 失去凭证 —— 而 main 不做 run 记账，
  // 无法知道哪些还在飞。进程内有效、不落盘（重启后已无 run 可续）。
  ipcMain.handle('surface:agent-token', (e, id: unknown) => {
    if (!isTrustedIpcSender(e)) return { ok: false, reason: 'denied' }
    const sid = typeof id === 'string' ? id.trim() : ''
    if (!SURFACE_ID_RE.test(sid)) return { ok: false, reason: 'denied' }
    return { ok: true, token: surfaceAgentTokenOf(sid) }
  })
  ipcMain.handle('surface:agent', async (e, p: { token?: unknown; name?: unknown; args?: unknown } = {}) => {
    if (!isTrustedIpcSender(e)) return { ok: false, reason: 'denied' }
    const token = typeof p?.token === 'string' ? p.token : ''
    const name = typeof p?.name === 'string' ? p.name.trim() : ''
    const sid = token ? surfaceIdForToken(token) : undefined
    // ② 形状 + 白名单：name 只认 agent.* 的五条（§6.6）。不在这张表里就不是插件 Agent
    // 通道该承载的东西（旧只读命令 D11 已禁用，不该从这里绕回去）。
    if (!sid || !SURFACE_AGENT_COMMANDS.has(name)) return { ok: false, reason: 'denied' }
    const args = p?.args && typeof p.args === 'object' && !Array.isArray(p.args) ? (p.args as Record<string, unknown>) : {}
    // ③ 与 surface:command 同一条一问一答通道（负 id）：插件 Agent 的回执不进渲染层
    // 事件流 —— run 的事件走 surface:agent-event 那条独立通道（D9）。
    const response = await sendBridgeRequest('surface_agent', { id: sid, name, args }, SURFACE_AGENT_TIMEOUT_MS)
    if (!response.ok) {
      // §6.5 封闭集合：bridge 给了 code 就用（denied / no_session / quota / …），
      // 没给（超时 / 进程不在）按 error 文案分成 timeout / error —— 与 surface:command 同口径。
      if (response.code) return { ok: false, reason: response.code }
      // §6.6 的拒绝信封把 reason 也放在 data 里（`{ok:false, code, data:{ok:false,reason}}`）：
      // code 是首选，data.reason 是同一个封闭集合的第二种携带方式（P1c 只写了一处时也能对上）。
      const nested = response.data?.reason
      if (typeof nested === 'string' && nested) return { ok: false, reason: nested }
      const detail = response.error ?? ''
      return { ok: false, reason: /超时|timeout|timed out/i.test(detail) ? 'timeout' : 'error' }
    }
    // 回执**原样**带回：data 里的 session_id / run_id 由 Go 决定，main 不解释也不改写
    //（P1c 之前这里就没有 Go 端，收到 ok 也只能是探针/桩）。
    return { ok: true, data: response.data }
  })
  // surface:set-enabled / surface:remove / surface:audit —— 插件页（PR-14b）的三条。
  //
  // 三条都是**转发**：index 的写入纪律（flock + .bak + 原子替换）只住在 Go 的
  // surfaceStore（PR-13），主进程在这里再写一份就等于把「谁有权改 index」变成两处判定，
  // 而松的那一处就是「禁用只是 UI 上灰了」。这里的形状校验只为省一条注定被拒的
  // bridge 命令；真正的封闭拒绝集合在 surfaces_manage.go（unknown_id / bad_request / error）。
  //
  // 走 sendBridgeRequest（负 id 一问一答）而不是渲染层的事件流：这三条描述的是
  // `~/.go-code/surfaces/` 这份应用级状态，没有 run/session 归属，进了事件流就会以
  // 「会话事实」的身份参与视图构建（与 surface:command 同一条理由）。
  ipcMain.handle('surface:set-enabled', async (e, p: { id?: unknown; enabled?: unknown } = {}) => {
    if (!isTrustedIpcSender(e)) return { ok: false, reason: 'denied' }
    const id = typeof p?.id === 'string' ? p.id : ''
    // enabled 必须是**布尔**：缺省当 false 会让一次少传字段的调用变成静默禁用。
    const enabled = typeof p?.enabled === 'boolean' ? p.enabled : null
    if (!SURFACE_ID_RE.test(id) || enabled === null) return { ok: false, reason: 'bad_request' }
    const response = await sendBridgeRequest('surface_set_enabled', { id, enabled }, 10_000)
    if (!response.ok) return { ok: false, reason: response.code ?? 'error' }
    return { ok: true, id, enabled }
  })
  // surface:remove 走 PR-13 的 Remove：目录进 `.trash/<id>-<ts>`，**不直接删**。
  // 插件页的卸载按钮与 `surface_remove` 工具是同一条磁盘路径，只有入口不同。
  ipcMain.handle('surface:remove', async (e, p: { id?: unknown } = {}) => {
    if (!isTrustedIpcSender(e)) return { ok: false, reason: 'denied' }
    const id = typeof p?.id === 'string' ? p.id : ''
    if (!SURFACE_ID_RE.test(id)) return { ok: false, reason: 'bad_request' }
    const response = await sendBridgeRequest('surface_remove', { id }, 10_000)
    if (!response.ok) return { ok: false, reason: response.code ?? 'error' }
    return { ok: true, id, status: response.data?.status }
  })
  // surface:audit 读 audit.jsonl 的尾部（limit 缺省 50、上限 200 —— 界在 Go 的 ReadAudit）。
  // 坏行在桥侧就跳过了，这里拿到的是一份**已经干净**的数组：空数组 = 还没有调用记录。
  ipcMain.handle('surface:audit', async (e, p: { limit?: unknown } = {}) => {
    if (!isTrustedIpcSender(e)) return { ok: false, reason: 'denied', lines: [] }
    const limit = typeof p?.limit === 'number' && Number.isFinite(p.limit) ? Math.trunc(p.limit) : 0
    const response = await sendBridgeRequest('surface_audit', limit > 0 ? { limit } : {}, 10_000)
    if (!response.ok) return { ok: false, reason: response.code ?? 'error', lines: [] }
    const lines = Array.isArray(response.data?.lines) ? response.data.lines : []
    return { ok: true, lines }
  })
  // workspace:forget：工作区被移除时回收媒体根（评审裁决 1 的「关闭时删除」）。
  // 只有主窗口主 frame 能调；删的是清单里的条目，不做任何路径推断。
  ipcMain.handle('workspace:forget', (e, p: unknown) => {
    if (!isTrustedIpcSender(e)) return { ok: false, error: '不受信任的调用来源' }
    forgetWorkspace(p)
    return { ok: true }
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
  // settings:set：写侧白名单 patch（BR-29）+ 只写**原始对象**（BR-01）。
  // 不再 `{ ...cur, ...patch }` 把「默认值 ∪ 磁盘值」的并集整份写回 —— 那是「改个主题
  // settings.json 从几行变 200+ 行、新版本默认值对老用户永不生效」的根因。
  // 返回值仍走 readSettingsResult()：renderer 拿到完整设置形状（与 settings:get 一致）。
  ipcMain.handle('settings:set', (_e, patch: unknown) => {
    const res = commitSettings((raw) => applySettingsPatch(raw, patch))
    if (!res.ok) return { ok: false, error: res.error }
    return { ok: true, settings: readSettingsResult().settings }
  })
  // provider:save：持久化配置（settings.json）+ key（keys.json，safeStorage 加密）。不转发 bridge——
  // 热切换由 renderer 另发 set_provider 桥命令（key 只在命令 payload 瞬态，不入持久态）。
  // 写侧语义在 settings-store.applyProviderSave：只改 provider.<name> 里输入出现的字段，
  // 兄弟 provider / provider 段未知字段 / 顶层未知键一律不动（BR-02 的 Electron 一半），
  // 也**不补** DEFAULT_SETTINGS 的 base_url/models/max_tokens/protocol/input_types。
  // BR-08：api_key **永远不进 settings**（api_key: undefined 先剔除），只落 keys.json：
  //   · 非空 key → setKey（覆盖旧条目，旧格式自动升级为 enc:true）；
  //   · null → 明确删 key（显式「清除」语义；前端目前不传 null，undefined/空串 = 不修改）；
  //   · 先写 settings 再写 key：配置没落盘就不该留下孤儿 key（配置写失败 → 直接返回，用户重试）。
  ipcMain.handle('provider:save', (_e, p: { provider: string; active?: string; base_url?: string; label?: string; protocol?: string; models?: Record<string, number>; max_tokens?: Record<string, number>; prices?: Record<string, { input: number; cache_read: number; cache_write: number; output: number }>; input_types?: Record<string, string[]>; openai_compat?: OpenAICompatCapabilities; auth_type?: 'api_key' | 'oauth'; usage_input_includes_cache?: boolean | null; cache_ttl_1h?: boolean | null; api_key?: string | null }) => {
    const res = commitSettings((raw) => applyProviderSave(raw, { ...p, api_key: undefined }))
    if (!res.ok) return res
    const key = typeof p?.api_key === 'string' ? p.api_key : ''
    if (key.trim()) {
      const w = keyStoreSetKey(keyStoreArgs(), p.provider, key)
      if (!w.ok) return { ok: false, error: `保存配置成功，但 API key 写入失败：${w.error}` }
    } else if (p?.api_key === null) {
      // 显式清除（当前 UI 不发 null；保留这个语义，避免将来「删 key」只能靠删 provider）
      removeProviderKey(p.provider)
    }
    return { ok: true, hasKey: providerHasKey(p.provider) }
  })
  // provider:remove：删除 provider（settings.json + keys.json；内置用 deleted 标记）。
  // 所有 provider（含内置 5 家）都可删除；删除活跃则回退到首个未删除的内置（deepseek 优先）。
  // deleted 标记让 bridge 重启后不复活（bridge 默认预置 5 家，靠标记识别用户删除）。
  // 写侧语义在 settings-store.applyProviderRemove：只动 provider.<name> 与 provider.active；
  // 内置 provider **不重建对象、不补默认值**（旧实现在删除时把整份默认写回，正是 BR-01）。
  // 顺序：settings 删成功后才删 keys.json 条目（失败保留 key，绝不丢）。
  ipcMain.handle('provider:remove', (_e, provider: string) => {
    const res = commitSettings((raw) => applyProviderRemove(raw, provider, { builtins: BUILTIN_PROVIDERS }))
    if (!res.ok) return res
    removeProviderKey(provider)
    return { ok: true }
  })
  // provider:key-status：只回布尔（迁移失败时 keys-store 查不到再回退 settings 遗留明文）。
  ipcMain.handle('provider:key-status', (_e, provider: string) => providerHasKey(provider))
  // provider:key：取某 provider 的明文 key（仅 renderer 本地「测试连接」/改名用）。
  // 不做日志、不进错误消息；无 key / 解密失败 → 空串。
  ipcMain.handle('provider:key', (_e, provider: string) => providerKeyOf(String(provider ?? '')))
  // provider:copy-key：provider 改名时把 keys.json 条目原样搬到新 id（不解密再加密）。
  ipcMain.handle('provider:copy-key', (_e, p: { from: string; to: string }) => {
    const r = keyStoreCopyKey(keyStoreArgs(), String(p?.from ?? ''), String(p?.to ?? ''))
    if (!r.ok) return { ok: false, error: r.error }
    return { ok: true, copied: r.copied }
  })
  // —— OAuth 订阅登录（2026-08 P2）：登录/登出/状态经 bridge 命令；auth_url/device_code 事件
  //    由 bridge 推送（oauth_event），主进程负责唤起系统浏览器（shell.openExternal）。
  ipcMain.handle('oauth:login', (_e, p: { provider: string }) => sendBridgeRequest('oauth_login', p))
  ipcMain.handle('oauth:logout', (_e, p: { provider: string }) => sendBridgeRequest('oauth_logout', p))
  ipcMain.handle('oauth:status', (_e, p: { provider: string }) => sendBridgeRequest('oauth_status', p))
  ipcMain.handle('oauth:prompt-answer', (_e, p: { provider: string; value: string }) => sendBridgeRequest('oauth_prompt_answer', p))
  // mcp:save：持久化 MCP 服务器配置（settings.json mcpServers 段，全量替换）。
  // 不转发 bridge——热应用由 renderer 另发 mcp_set 桥命令（重建会话工具集）。
  // 写侧只替换自己的段：provider/theme/hooks 等顶层键与其它段原样保留
  //（校验与类型归一在 settings-store.normalizeMcpServers，与 bridge 的 Validate 同口径）。
  ipcMain.handle('mcp:save', (_e, servers: Record<string, MCPServerCfg>) => {
    const res = commitSettings((raw) => applyMcpServers(raw, servers))
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
  // 原生脏标记（PR-05b）：macOS 用 setDocumentEdited（标题栏红点 + 关窗提示），
  // 其它平台空实现 —— 「零依赖」也包括不为一个视觉提示引入平台分支。
  ipcMain.handle('app:set-dirty', (_e, dirty: unknown) => {
    docDirty = dirty === true
    if (process.platform === 'darwin') mainWindow?.setDocumentEdited(docDirty)
    return { ok: true }
  })
  // 确认框选了「丢弃」/所有脏文档保存完毕 → 真正退出（置放行标志，见 before-quit）
  ipcMain.handle('app:quit-anyway', () => {
    allowQuit = true
    app.quit()
    return { ok: true }
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
  app.on('second-instance', (_event, argv) => {
    // Win/Linux：第二次启动是「点链接」的常态路径，深链在 argv 里。
    const link = extractHubDeepLinkArg(argv)
    if (link) forwardHubDeepLink(link)
    if (mainWindow) {
      if (mainWindow.isMinimized()) mainWindow.restore()
      mainWindow.focus()
    }
  })
}

// —— 内存压力信号（2026-11-12，P1）——
//
// Chromium 的 tab discarding 用的是 OS 的内存压力通知
// （chromiumos-design-docs/tab-discarding-and-reloading.md）：内存压力下丢弃
// 标签页的**可重取状态**，保留用户正在交互的东西。
//
// Electron 没有直接暴露那个通知，但主进程能读系统内存：
// `process.getSystemMemoryInfo()` → {total, free, swapTotal, swapFree}（KB）。
// 渲染进程据此决定要不要**主动降级**视图。
//
// 为什么必须自己做：V8 的堆上限是**崩溃线**不是降级线
// （v8.dev/blog/heap-size-limit：到顶 → "a series of last resort garbage
// collections" → 再不行就 "stops execution and reports an out-of-memory
// failure" ⇒ Electron 里就是 Aw, Snap）。而且我们没设 --max-old-space-size。
//
// 5 秒一次足够：内存压力是慢变量（换页颠簸以秒计，不是毫秒计）。
function startMemPressureWatch(): void {
  const timer = setInterval(() => {
    if (!mainWindow || mainWindow.isDestroyed()) return
    try {
      const m = process.getSystemMemoryInfo()
      sendToRenderer('mem:pressure', {
        free: m.free,
        total: m.total,
        swapFree: m.swapFree,
        swapTotal: m.swapTotal,
      })
    } catch {
      /* 主进程 API 不可用（理论上不会）：静默忽略，不影响主流程 */
    }
  }, 5000)
  // 不因为这个定时器而吊住进程退出。
  timer.unref?.()
}

app.whenReady().then(() => {
  startupMark('whenReady')
  // 媒体根清单（settings.json 同目录、0600）：pick-workspace 等三处写入，见 known-workspaces.ts
  configureKnownWorkspaces(path.join(cfgDir(), 'workspaces.json'))
  // 存量 settings v1→v2 迁移：必须在 bridge spawn（renderer 发 bridge:start）之前跑完，
  // 否则 bridge 会按 v1 里固化的旧默认启动。失败只记日志、继续用旧文件（不阻止启动）。
  migrateSettingsOnStartup()
  // 存量明文 provider key → keys.json（BR-08）：排在 v2 迁移之后（v2 保留 api_key）、
  // spawn 之前（bridge 启动时要通过 env / 命令拿 key，且不能再从 settings 读到明文）。
  migrateProviderKeysOnStartup()
  registerHubProtocol()

  registerIpc()
  // 媒体协议：只读、只服务工作区内、只服务媒体扩展名（拒绝分支见 media-protocol.ts）
  registerMediaProtocol()
  // Surface 插件资产（PR-14）：只读、只服务 `<cfgDir>/surfaces/<id>/`、只服务资产扩展名，
  // 且**只服务 enabled 的插件**（禁用/卸载后立刻 404）。路径判定见 surface-protocol.ts。
  configureSurfaceRoot(path.join(cfgDir(), 'surfaces'))
  registerSurfaceProtocol()
  createWindow()
  startMemPressureWatch()
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

// before-quit：脏文档时**拦一次**并问渲染层（复用脏确认框）。三个放行条件：
//   · allowQuit（用户已经答过：丢弃/保存完）
//   · 没有脏文档
//   · 窗口已经没了（没人能回答；关窗后 mac 上的 cmd+Q 就是这条路径，再拦只会挂住进程）
// stopBridge 每次都调：它幂等（bridge 为 null 时什么都不做）。
app.on('before-quit', (e) => {
  if (!allowQuit && docDirty && mainWindow && !mainWindow.isDestroyed()) {
    e.preventDefault()
    sendToRenderer('app:quit-request')
    return
  }
  stopBridge()
})
