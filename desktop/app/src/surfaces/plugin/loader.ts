// surfaces/plugin/loader.ts —— 渲染层的插件加载器（PR-14 C；03-code-plan §3.6）。
//
// 一句话：**index 是「装过的」，注册表是「活着的」，这个文件负责让后者追上前者。**
//
// 三条硬约束（每条都有对应的失败形态，注释里写的是踩过/预见过的样子）：
//
//  1. **单插件失败 = 只黑自己**：坏模块（语法错 / kind 不符 / apply 抛错）必须只让
//     这一个 kind 进不了注册表，内置面板与其它插件照旧。做法是「import + 校验 + apply
//     整段 try/catch，且 apply 之前登记的 provider 在失败时**逐个撤销**」——只 catch
//     不撤销的话，面板一半挂上了、一半没挂上，症状是「有时能开，有时空白」（最难查的一类）。
//  2. **失败要看得见**：`status.ts` 登记 + console.warn + `surface_status` 上报。
//     静默跳过的插件在 ui_inspect 里与「没装」长得一模一样。
//  3. **卸载必须连带注销**：`dispose()` 调 `unregisterSurface` 并清掉 host 订阅；
//     漏了这一步，标签还开着但 provider 没了 —— PR-10 的墓碑机制能兜住（unavailable
//     reason = missing），但「关掉再打开」会变成空面板。
//  4. **卸载连带清数据**（R17a）：插件的数据在 localStorage 里、以 `<id>:` 开头，
//     用户拍板「卸载 = 清掉它前缀下的所有键」。这里只负责触发（每轮 sync 拿 index 的
//     全部 id 去和「上次见过的 id 列表」对账，见 storage.ts）；禁用**不**清数据。
//
// 缓存：URL 里带 `?v=<hash>`（bridge 安装时算的 sha256）。hash 变 = 新 URL = 新模块实例
// （浏览器不会复用旧的 module record）；hash 不变 = 用同一份，不重复求值。
//
// 依赖方向：本文件**不 import store**（store 反过来要 import 它来启动，直接互相 import
// 会形成运行期环 —— 与 lib/uiSnapshot 同一取舍）。store 的能力（openTab / toast /
// 当前 workspace / 上报通道）由 startPluginLoader 的 deps 注入。
import * as React from 'react'
import type { ReactNode } from 'react'
import { emitUI, subscribeUI } from '../../lib/uiBus'
import { t as i18nT } from '../../i18n'
import { registerSurface, unregisterSurface } from '../registry'
import { noteSurfaceVersion } from '../health'
import { entryTitleText, setSurfaceEntryIndex } from './entries'
import { reconcileKnownIds } from './storage'
import { clearSurfaceLoadError, setSurfaceLoadError } from './status'
import type { SurfaceIndex, SurfaceIndexItem, SurfaceCommandReply, SurfaceAgentEventPayload, SurfaceEvent } from '../../transport/types'
import type { DistributiveOmit, HostApi, PluginTabInstance, SurfaceKind, SurfaceProvider, SurfaceTab } from '../types'
import { bridge } from '../../lib/bridge'
import { getHostLang } from '../../host/lang'
import { appUrlOf } from '../../lib/surfaceApp'
import type { PluginAppHostProps } from './webApp'

/** 生产 base URL：`hai-surface://<id>`（协议见 electron/surface-protocol.ts）。 */
export function surfaceBaseUrl(id: string): string {
  return `hai-surface://${id}`
}

/**
 * base URL 的**测试替身**（生产恒 undefined）。
 *
 * 为什么必须有它：e2e 跑在 Chromium 里、没有 Electron 主进程，`hai-surface://` 这个
 * 协议在那台「浏览器」上是未注册的（import 直接 Failed to fetch）。而探针要验的恰恰是
 * **store → loader 这条接线**（自己调 syncPlugins 会把最易漏的一段测成不存在）。
 * 于是保留接线、只换字节来源：同源静态目录 `/e2e/_pluginprobe_run/plugins/<id>`。
 *
 * 命名带 ForTest 是刻意的（本仓先例：resetUIBusForTest / unloadAllPluginsForTest）：
 * 生产代码里出现这个调用就是错的。
 */
let baseUrlOverride: SurfaceBaseUrl | undefined
export function setPluginBaseUrlForTest(base: SurfaceBaseUrl | undefined): void {
  baseUrlOverride = base
}

/** e2e / 探针可以把 base 换成同源的静态目录（`/e2e/_pluginprobe_run/plugins/<id>`）。 */
export type SurfaceBaseUrl = string | ((id: string) => string)

export interface PluginHostDeps {
  /** `ui` 通道上报（渲染层 → bridge 的 `surface_status`）。缺省 = 只登记本地错误。 */
  send?: (op: string, data: unknown) => void
  /** 插件要开标签（`host.command('ui.open_tab')`）。缺省 = 回 `denied`（宿主没这个能力）。 */
  openTab?: (spec: DistributiveOmit<SurfaceTab, 'id'>) => void
  /** 插件要弹一行提示（`host.command('ui.toast')`）。缺省 = 回 `denied`。 */
  toast?: (msg: string) => void
  /** 当前 workspace（旧的只读命令自动带上：看板类面板的命令大多按工作区取数）。 */
  getWorkspace?: () => string | undefined
  /**
   * 当前有没有活动会话（`composer.quote` 的 `no_session` 判据，§5.1）。
   * 缺省 = 不判定（草稿是工作区作用域的，没有会话时也能写；因此缺省按「有」处理，
   * 只有宿主明确说没有才拒绝 —— 不给自己造一个假的拒绝理由）。
   */
  hasActiveSession?: () => boolean
  /** i18n（缺省用 src/i18n 的 t）。 */
  t?: (key: string, vars?: Record<string, unknown>) => string
  /** 主题（缺省读 <html data-theme> + index.css 的 CSS 变量）。 */
  theme?: () => { mode: 'light' | 'dark'; tokens: Record<string, string> }
  /**
   * 当前环境能不能读到**权威** index（生产 = `!!bridge?.surfaceIndex`）。
   * 缺省（不给这个函数）= 不权威 = 不做数据清理。每次 sync 都重新问一遍：环境可能在运行中变
   * （比如 mobile/browser 模式下永远拿不到，那就不该有任何跨重启清理发生）。
   */
  indexAuthoritative?: () => boolean
  /**
   * 插件**新建**了自己的会话（`agent.session` / 不带 session_id 的 `agent.run`）→ 请宿主
   * 重新拉一次左栏的插件分组（`plugin_projects_list`）。
   *
   * 为什么必须由这边通知：左栏的虚拟 Project 只在启动那一刻水合一次，而插件建会话是
   * **运行中**发生的事 —— 不通知的话 Go 侧会话真的建出来了，左栏却是空的，用户只能重启
   * 才看见。缺省 = 不刷新（单测 / 浏览器档没有左栏插件分组）。
   *
   * 传进来的实现**自己负责合并/去抖**（生产 = store 的 schedulePluginProjectsRefresh，
   * 150ms 合并）：loader 一次命令调一次，不去猜调用频率。
   */
  refreshPluginProjects?: () => void
  /**
   * 这个插件 kind 的面板此刻**在不在标签栏里**（P2b 的 `requires_panel` 宿主门禁）。
   *
   * 判据必须是「标签集里有这个 kind」，不能只看当前激活标签：插件面板很可能开在**别的
   * 会话作用域**的标签栏里（用户切走了会话），那也算开着；反过来，把「当前面板不是它」
   * 当成关掉，会把一个明明在场的面板判成不在，模型收到的就是错的 `panel_not_open`。
   *
   * 缺省（不给这个函数）= 按「没开」处理，见 `panelBlocked`：门禁的失效方向必须是
   * 「拒绝执行」（模型看得懂、会换路），不能是「交给一个可能已经不在的面板」。
   */
  isSurfaceOpen?: (kind: string) => boolean
}

export interface PluginFailure {
  id: string
  kind: SurfaceKind
  error: string
}

/**
 * 一轮 sync 的来源声明。
 *
 * `authoritativeIndex` 缺省 **false**：传进来的 index 不权威时，数据清理（known-ids 对账）
 * 既不执行也不记账。为什么必须有这个开关：浏览器版（vite 直开、没有 preload）没有
 * `bridge.surfaceIndex`，store 那条路回的是硬编码的 `{items:[]}` —— 若把
 * 「拿不到 index」读成「插件全没了」，一次网页会话就会清空全部插件数据。
 * 也不能靠 `items` 非空去猜：卸载**最后一个**插件时 index 恰好是空的，那一次必须能清。
 */
export interface SyncOptions {
  /** true = 这份 index 是权威来源（生产 = Go 的 surfaceStore）。缺省 / false = 不权威。 */
  authoritativeIndex?: boolean
}

export interface SyncReport {
  /** 这一轮真的 import + apply 成功的 id */
  loaded: string[]
  /** 这一轮因为「禁用 / 消失 / hash 变了」被卸载的 id */
  unloaded: string[]
  /** 已是最新（hash 未变）而跳过的 id */
  skipped: string[]
  /** 失败（已登记 + console.warn）的插件 */
  failed: PluginFailure[]
}

interface LoadedPlugin {
  id: string
  kind: SurfaceKind
  hash: string
  entry: string
  /** 内容修订键：hash + 入口（ESM）或 web URL（网页型）。sync 用它判「要不要重载」。 */
  revision: string
  dispose: () => void
}

/** 已加载的插件（id → 句柄）。模块级单例：注册表也是单例，两者生命周期必须一致。 */
const loaded = new Map<string, LoadedPlugin>()

/**
 * URL 型插件的 host 对象（pluginId → createHost 的产物）。
 *
 * 为什么不是每来一条 command 新建一个：createHost 在创建时领一次 agent token，并持有
 * 事件订阅表；每次新建会让 token 越领越多、订阅越叠越厚。一个插件一份，dispose 时删。
 */
const webHosts = new Map<string, HostApi>()

/**
 * URL 型插件的 React 宿主组件（由 main.tsx 注入，见 registerPluginAppHost）。
 *
 * 为什么不直接 import webApp.tsx：webApp 要用 store（rightExpanded/panelMenuOpen/弹窗闸门），
 * 而 store 反过来 import 本文件 —— 直接 import 会形成 loader → webApp → store → loader
 * 的运行期环（本文件开头「不 import store」那条纪律）。类型是 type-only（擦除），
 * 组件在 main.tsx 求值时注入；探针没注入时渲染一行宿主错误，而不是白屏。
 */
let appHostComponent: React.ComponentType<PluginAppHostProps> | undefined

export function registerPluginAppHost(component: React.ComponentType<PluginAppHostProps>): void {
  appHostComponent = component
}

/** 内容修订键：hash 变 / 入口变 / web URL 变都要重载（三者在 index 里是不同字段）。 */
function revisionOf(item: SurfaceIndexItem): string {
  return `${item.hash}|${item.web ? `web:${item.web.url}` : `entry:${item.entry || 'panel.js'}`}`
}

/** 最近一次 sync 用的 base（渲染层拼入口图标 URL 时沿用同一条来源，不另猜协议）。 */
let currentBase: SurfaceBaseUrl | undefined

/**
 * pluginAssetUrl 插件目录内某个文件的 URL（入口图标 / 声明图标）。
 *
 * 与 applyManifest 拼入口时**同一份 base**：生产是 `hai-surface://<id>`，探针里由
 * setPluginBaseUrlForTest 换成同源静态目录。两处各拼一次迟早漂移（一处带 hash 一处不带、
 * 一处用了 `<id>` 一处忘了），而图标的失败形态是「空白 12px」—— 最难被注意到的那类。
 */
export function pluginAssetUrl(id: string, rel: string): string {
  if (!id || !rel) return ''
  // 优先级与 applyManifest 同一条来源：最近一次 sync 的 base → 测试替身 → 生产协议。
  // 少了中间那层，「只渲染左栏、不跑 loader」的探针/单测里图标永远是 hai-surface://（拉不到）。
  const base = baseOf(currentBase ?? baseUrlOverride ?? surfaceBaseUrl, id)
  return base ? `${base}/${rel.replace(/^\/+/, '')}` : ''
}

/** 免疫：正在进行的 sync（并发调用来一个就排队，绝不两个同时 import 同一个 id）。 */
let syncChain: Promise<SyncReport> = Promise.resolve({ loaded: [], unloaded: [], skipped: [], failed: [] })

const pluginKindOf = (id: string): SurfaceKind => `plugin:${id}`

function baseOf(baseUrl: SurfaceBaseUrl, id: string): string {
  const base = typeof baseUrl === 'function' ? baseUrl(id) : baseUrl
  return String(base ?? '').replace(/\/+$/, '')
}

const THEME_TOKENS: Record<string, string> = {
  bg: '--paper',
  fg: '--ink',
  border: '--ink-line',
  accent: '--cinnabar',
  ok: '--bamboo',
  warn: '--ochre',
  err: '--crimson',
  info: '--indigo',
}

function defaultTheme(): { mode: 'light' | 'dark'; tokens: Record<string, string> } {
  if (typeof document === 'undefined') return { mode: 'dark', tokens: {} }
  const root = document.documentElement
  const mode: 'light' | 'dark' = root.getAttribute('data-theme') === 'paper' ? 'light' : 'dark'
  const cs = getComputedStyle(root)
  const tokens: Record<string, string> = {}
  for (const [name, cssVar] of Object.entries(THEME_TOKENS)) {
    const v = cs.getPropertyValue(cssVar).trim()
    if (v) tokens[name] = v // 空值不给：插件读到 '' 与没有这个 token 是两件事
  }
  return { mode, tokens }
}

function iconNode(src: string): ReactNode {
  // 12×12 与内置 provider 的图标同尺寸（标签条只有这么大）。inline style 而不是新 CSS 类：
  // 插件资产不该要求主包同时发一版样式（那会让「装插件」变成「装插件 + 升级客户端」）。
  return React.createElement('img', { src, width: 12, height: 12, alt: '', draggable: false, style: { width: 12, height: 12 } })
}

/** `composer.quote` 的文本上限（字节，§6.2）：超限 = invalid_input，不截断（截断是静默改用户内容）。 */
const QUOTE_MAX_BYTES = 8 * 1024

/**
 * 本层就地处理的命令（不过桥）：`composer.quote` / `ui.open_tab` / `ui.toast`（§6.2）。
 * 它们是**渲染层的能力**（草稿、标签、toast 都在这一侧），发给 Go 只会变成一条没人实现的命令。
 */
const LOCAL_COMMANDS = new Set(['composer.quote', 'ui.open_tab', 'ui.toast'])

/**
 * 走 bridge 的插件 Agent 命令（§6.6）。与 main 的 `SURFACE_AGENT_COMMANDS` 同一张表 ——
 * 两边都在白名单里单独写一遍是刻意的：loader 这一份省一次 IPC，main 那一份才是边界
 *（渲染层与插件同进程，预检只能算礼貌）。
 */
const AGENT_COMMANDS = new Set(['agent.session', 'agent.run', 'agent.cancel', 'agent.snapshot', 'tool.result'])

/**
 * D11：四条旧只读命令**已不可用**（`workspace_list` / `session_list` / `git_snapshot` /
 * `ui_inspect`）。它们能读真实项目/会话元数据，与「插件不碰 Project」冲突。
 *
 * 为什么在 loader 里也拒一次（P1c 的 Go 侧宿主表已经清空、会回 denied）：这是**双保险**
 * 而不是第二份真相 —— 症状差别很大：Go 侧拒 = 一次 IPC + 一条审计；loader 拒 = 面板上
 * 立刻出现 denied 且不产生任何跨进程流量。安全的判据宁可重复，不要单点。
 */
const D11_DENIED = new Set(['workspace_list', 'session_list', 'git_snapshot', 'ui_inspect'])

/**
 * 本地事件源：`host.onEvent` 里不需要 token/桥就能工作的那几类（§5.2 的 `surfaces.changed`）。
 *
 * 两种拼写都认：uiBus 的 op 叫 `surfaces_changed`（PR-12 起就写进各家 manifest），而
 * 09 §5.2 的表格写作 `surfaces.changed`。同一个事件的两种拼法，映射到**同一个** op。
 */
const LOCAL_EVENT_OPS: Record<string, string> = {
  surfaces_changed: 'surfaces_changed',
  'surfaces.changed': 'surfaces_changed',
}

/** 插件 Agent 通道在渲染层的最小形状（= DesktopApi 的子集；单测可注入假实现）。 */
export interface SurfaceAgentApi {
  surfaceAgentToken(id: string): Promise<{ ok: boolean; token?: string; reason?: string }>
  surfaceAgent(p: { token: string; name: string; args?: unknown }): Promise<SurfaceCommandReply>
  onSurfaceAgentEvent(cb: (p: SurfaceAgentEventPayload) => void): () => void
}

/**
 * 这条 agent.* 命令是不是**刚建了一个插件会话**（见 `PluginHostDeps.refreshPluginProjects`）。
 *
 *   · `agent.session` 恒建新会话；
 *   · `agent.run` 只在 payload **没有** session_id 时才自己建一个（§6.6 D4：给了
 *     session_id 是往已有会话里发消息，不产生新的左栏条目）。
 *
 * 判据只看**原始 payload**，不看回执：回执顶层被归一摊平过 `session_id`（成功时总有一个），
 * 拿回执判会把每一次「复用已有会话的 run」都当成新建 —— 那是每次提问都重拉一遍左栏。
 * 「空白算没有」与 Go 对齐（`strings.TrimSpace(str(args,"session_id"))` 为空 = 新建），
 * 否则渲染层会认为「不刷新」，而 Go 那边真的建了一个新会话。
 */
function createsPluginSession(name: string, args: Record<string, unknown>): boolean {
  if (name === 'agent.session') return true
  if (name !== 'agent.run') return false
  const sid = args.session_id
  return typeof sid !== 'string' || sid.trim() === ''
}

/**
 * P2b 宿主门禁：这条 `tool.call` 是不是**必须由宿主挡下**（manifest 声明 requires_panel，
 * 而插件面板此刻不在标签栏里）。
 *
 * 为什么门禁在 loader 而不是「插件自己判」：`requires_panel` 是 manifest 的**声明**，
 * 插件忘判/判错都不该改变结论；而且「面板关掉后事件照样会到」是这条通道的固有形态 ——
 * 面板把 `onEvent('tool.call')` 注册在 apply 作用域（活到插件卸载），关标签并不会退订。
 * 挡在 `fn` 之前一步，插件连回调都进不去，模型收到的就是一个明确的 `panel_not_open`。
 *
 * 三个保守选择，都是「宁可拒绝」的方向：
 *   - deps 没给 `isSurfaceOpen`（单测 / 旧宿主）⇒ 视为没开（fail closed）；
 *   - 判据自己抛错 ⇒ 视为没开（不知道开没开 = 没开）；
 *   - `requires_panel` 只认**严格 true** ⇒ manifest 里写 `"true"`/1 不算数，
 *     与 Go 侧 json.Unmarshal 到 bool 的口径一致（那边这类值直接安装期报错）。
 */
function panelBlocked(p: SurfaceAgentEventPayload, id: string, deps: PluginHostDeps): boolean {
  const ev = p.event
  if (ev.type !== 'tool.call') return false
  const data = isPlainRecord(ev.data) ? ev.data : {}
  if (data.requires_panel !== true) return false
  try {
    return deps.isSurfaceOpen?.(pluginKindOf(id)) !== true
  } catch (e) {
    console.warn('[surface] isSurfaceOpen 判据抛错，按「面板未打开」处理：', e)
    return true
  }
}

/**
 * 门禁回执：把 `tool.result{ok:false,error:"panel_not_open"}` 直接发给宿主。
 *
 * 为什么**不**走 `host.command('tool.result')`：
 *  1. 这一跳发生在 `createHost` 构建 host 对象的中途（onEvent 是它的一个字段），
 *     此刻 host 还不存在，闭包里也拿不到自己；
 *  2. 回执不该经过插件的命令闸门 —— 说「我现在无法执行」不是一次工具动作，
 *     不声明 `tool.result` 的插件也必须能拒（否则门禁静默失灵，模型等满 deadline）。
 *
 * 失败**吞掉**：门禁本身是兜底，回执发不出去（浏览器档 / bridge 已退出 / token 没领到）
 * 时最多退化成「模型等到超时」，绝不能把异常抛进插件的订阅回调 —— 那会把一次工具调用
 * 变成一条插件崩溃（PR-10 的崩溃卡就是这么来的）。
 *
 * 没有 `call_id` 就不发：那是唯一能对上等待者的键，编一个反而会 resolve 掉别人的调用。
 */
function replyPanelBlocked(
  p: SurfaceAgentEventPayload,
  tokenPromise: Promise<string | undefined>,
  agentApi: SurfaceAgentApi | null,
): void {
  const data = isPlainRecord(p.event.data) ? p.event.data : {}
  const callId = typeof data.call_id === 'string' ? data.call_id.trim() : ''
  if (!callId || !agentApi?.surfaceAgent) return
  void (async () => {
    try {
      const token = await tokenPromise
      if (!token) return
      await agentApi.surfaceAgent({
        token,
        name: 'tool.result',
        args: { call_id: callId, ok: false, error: 'panel_not_open' },
      })
    } catch (e) {
      console.warn('[surface] panel_not_open 回执发送失败（模型将等待超时）：', e)
    }
  })()
}

/**
 * 读 `composer.quote` 的载荷（§5.1：`{text, source?}`）。
 *
 * 只做**形状与上限**校验，不猜业务：空白文本 = invalid_input，超 8 KiB = invalid_input。
 * source 允许字符串（人读的来源名）或 `{id?, title?}`（同一个来源的稳定指纹，便于去重），
 * 其它形状**丢弃而不是报错** —— 多一个未知字段不该让整条引用失败。
 */
function readQuotePayload(payload: unknown): { ok: true; text: string; source?: { id?: string; title: string } } | { ok: false } {
  const p = payload && typeof payload === 'object' && !Array.isArray(payload) ? (payload as Record<string, unknown>) : {}
  const text = typeof p.text === 'string' ? p.text.trim() : ''
  if (!text) return { ok: false }
  // 字节而不是字符：一个中文字 3 字节，用 length 会放过 3 倍大的东西（与 tabs 的持久化上限同一口径）。
  if (new TextEncoder().encode(text).length > QUOTE_MAX_BYTES) return { ok: false }
  const raw = p.source
  let source: { id?: string; title: string } | undefined
  if (typeof raw === 'string' && raw.trim()) source = { title: raw.trim().slice(0, 200) }
  else if (raw && typeof raw === 'object' && !Array.isArray(raw)) {
    const s = raw as Record<string, unknown>
    const title = typeof s.title === 'string' && s.title.trim() ? s.title.trim().slice(0, 200) : ''
    const sid = typeof s.id === 'string' && s.id.trim() ? s.id.trim().slice(0, 200) : undefined
    // title 缺省时用 id 兜底（chip 上总得有个字）；两者都没给就当没来源。
    const label = title || sid
    if (label) source = { ...(sid ? { id: sid } : {}), title: label }
  }
  return { ok: true, text, ...(source ? { source } : {}) }
}

/**
 * createHost 插件唯一的宿主能力面（§5：**只有 `command` 与 `onEvent`**）。
 *
 * 刻意没有 `events` / `openTab` / `toast` / `agent` / `composer` 这些具名对象：
 * `composer.quote` / `ui.*` 全部收进 command（§5 的第一条），这样「插件能做什么」只有
 * 一处白名单（manifest.commands + manifest.data），审计也只认这一个入口。
 *
 * `disposers` 由调用方持有：插件注册的事件订阅在这里登记撤销函数，卸载时逐个执行 ——
 * 否则订阅表会在每次「卸载→重装」后多留一份僵尸监听（回调里还引用着上一条闭包里的 host）。
 *
 * `agentApi` 缺省就是 claim 到的那个桥（DesktopApi 的结构子集）。单独留一个参数是为了
 * 单测能在没有 Electron 的 Node 里注入假实现，**不是**给 store 用的注入口。
 */
export function createHost(
  item: SurfaceIndexItem,
  deps: PluginHostDeps,
  disposers: Array<() => void>,
  agentApi: SurfaceAgentApi | null = bridge as SurfaceAgentApi | null,
): HostApi {
  const declared = new Set(item.commands)
  const ops = new Set(item.data)
  const id = item.id
  const t = deps.t ?? ((k: string, _v?: Record<string, unknown>) => i18nT(k))

  // token **在加载时领一次**（§8.2：main 签发的 per-plugin 身份，loader 闭包持有）。
  // 领取失败（浏览器档 / 旧主进程 / bridge 不在）不是致命错：`agent.*` 回 denied，
  // 本地事件与 composer.quote 照常可用 —— 插件不该因为「今天没有桥」整块白屏。
  const tokenPromise: Promise<string | undefined> = (async () => {
    try {
      const r = await agentApi?.surfaceAgentToken(id)
      return r?.ok === true && typeof r.token === 'string' && r.token ? r.token : undefined
    } catch {
      return undefined
    }
  })()

  /** 事件订阅的公共收尾：登记 disposer（卸载时统一回收），返回给插件的对象不变。 */
  const track = (off: () => void): (() => void) => {
    disposers.push(off)
    return off
  }

  return {
    React,
    t,
    theme: deps.theme ?? defaultTheme,
    command: async (name: string, payload?: unknown): Promise<SurfaceCommandReply> => {
      // 闸门 ①：manifest 声明（§5.1：未声明 = `not_declared`，不是 `denied`）。
      // 两个错误码分开是刻意的：`not_declared` 是插件自己的 bug（漏写 manifest），
      // `denied` 是宿主拒绝（越权/D11）—— 面板给用户的提示不一样。
      if (typeof name !== 'string' || !declared.has(name)) return { ok: false, reason: 'not_declared' }
      // 闸门 ②：D11 四条旧只读命令（声明了也拒，见 D11_DENIED 的注释）。
      if (D11_DENIED.has(name)) return { ok: false, reason: 'denied' }
      const args = payload && typeof payload === 'object' && !Array.isArray(payload)
        ? (payload as Record<string, unknown>)
        : {}

      // —— 本层处理（§6.2：不过桥）——
      if (LOCAL_COMMANDS.has(name)) {
        if (name === 'composer.quote') {
          // 「无活动会话」由宿主注入的探针判定：草稿是会话/工作区作用域的，没有会话时
          // 插进去的文本没有归处（§5.1 的 no_session）。
          if (deps.hasActiveSession && !deps.hasActiveSession()) return { ok: false, reason: 'no_session' }
          const q = readQuotePayload(payload)
          if (!q.ok) return { ok: false, reason: 'invalid_input' }
          // 只投递、不代发（D7）：Composer 收到后插进草稿并挂一个可删的来源 chip。
          emitUI('composer_quote', { text: q.text, ...(q.source ? { source: q.source } : {}) })
          return { ok: true }
        }
        if (name === 'ui.toast') {
          const msg = typeof args.message === 'string' ? args.message.trim() : ''
          if (!msg) return { ok: false, reason: 'invalid_input' }
          if (!deps.toast) return { ok: false, reason: 'denied' }
          deps.toast(msg.slice(0, 500))
          return { ok: true }
        }
        // ui.open_tab：**只能开自己**的 kind（§5.1）。别人的 kind 会让插件互相顶掉面板，
        // 而 kind 是 manifest 里写的公开信息 —— 这不是防御不明来源，是守 API 的边界。
        const kind = typeof args.kind === 'string' ? args.kind.trim() : ''
        if (!kind) return { ok: false, reason: 'invalid_input' }
        if (kind !== pluginKindOf(id)) return { ok: false, reason: 'denied' }
        if (!deps.openTab) return { ok: false, reason: 'denied' }
        const params = args.params && typeof args.params === 'object' && !Array.isArray(args.params)
          ? (args.params as Record<string, unknown>)
          : undefined
        deps.openTab({ kind, ...(params ? { params } : {}) } as DistributiveOmit<SurfaceTab, 'id'>)
        return { ok: true }
      }

      // —— 插件 Agent 通道（§6.6）：agent.session / agent.run / agent.cancel / agent.snapshot / tool.result ——
      if (AGENT_COMMANDS.has(name)) {
        const token = await tokenPromise
        // 领不到 token = 这个宿主没有 Agent 通道（浏览器档/旧主进程）⇒ denied，不报错。
        if (!token || !agentApi?.surfaceAgent) return { ok: false, reason: 'denied' }
        try {
          const reply = await agentApi.surfaceAgent({ token, name, args })
          const data = reply?.data
          if (reply?.ok === true) {
            // 新建了会话 → 请宿主重拉左栏插件分组（只在**成功**且确实新建时；失败没有新会话，
            // 带 session_id 的 run 也没有）。放在归一之前：判据用原始 payload，不用回执。
            // 通知失败（宿主回调自己抛）必须**吞掉**：会话已经在 Go 侧建好了，把它改成
            // `error` 回执会让插件以为没建成而重试 —— 结果是左栏多出两条空会话。
            if (createsPluginSession(name, args)) {
              try {
                deps.refreshPluginProjects?.()
              } catch (e) {
                console.warn('[surface] 左栏插件分组刷新失败（会话已创建）：', e)
              }
            }
            // §5.1 承诺插件看到 `{ok, session_id}`，§6.6 的 wire 把值放在 `data` 里 ——
            // 这里做**一次**归一：摊到回执顶层，同时保留 `data`（想按 wire 形状读也可以）。
            // 归一放在 loader 而不是让每个插件自己 `r.data ?? r`：那会在几十个面板里分叉。
            return isPlainRecord(data) ? { ok: true, data, ...data } : { ok: true, data }
          }
          return { ok: false, reason: reply?.reason || 'denied' }
        } catch {
          // 传输层异常收敛成固定文案（插件拿不到内部错误）。
          return { ok: false, reason: 'error' }
        }
      }

      // —— 其余名字：走 P1 之前的只读命令通道（P1c 起宿主表为空，Go 会回 denied）——
      // 为什么留着：`surface_command` 本身还在（管理面/审计仍需要它），插件侧小幅收紧
      // 放在 P1c 一起做；README/模板已经不再声明任何旧命令（D11）。
      const api = agentApi as SurfaceAgentApi & { surfaceCommand?: (p: { id: string; name: string; args?: Record<string, unknown>; workspace?: string }) => Promise<SurfaceCommandReply> }
      if (!api?.surfaceCommand) return { ok: false, reason: 'denied' }
      try {
        const reply = await api.surfaceCommand({
          id,
          name,
          args,
          ...(deps.getWorkspace?.() ? { workspace: deps.getWorkspace?.() } : {}),
        })
        return reply?.ok === true
          ? { ok: true, data: reply.data }
          : { ok: false, reason: reply?.reason || 'denied' }
      } catch {
        // 传输层异常也收敛成固定文案：插件拿不到内部错误（与 bridge 的 error 同一条口径）。
        return { ok: false, reason: 'error' }
      }
    },
    onEvent: (type: string, fn: (e: SurfaceEvent) => void): (() => void) => {
      // 闸门：manifest.data 声明（未声明 = 抛，与 events.on 的旧语义一致 ——
      // 写错的事件名必须在 apply 时炸出来，否则就是「订阅成功但永远收不到」）。
      if (typeof type !== 'string' || !ops.has(type)) throw new Error(`事件未在 manifest.data 中声明：${type}`)
      if (typeof fn !== 'function') throw new Error('host.onEvent 的第二个参数必须是函数')
      // 本地事件（surfaces.changed 一类）：不需要 token，走 uiBus。
      const localOp = LOCAL_EVENT_OPS[type]
      if (localOp) {
        return track(subscribeUI(localOp, (data) => {
          fn({ type, data })
        }))
      }
      // Agent 事件：订阅**一条**独立通道，把自己的 surface_id 过滤掉之后再按 type 分发。
      // 过滤在第一跳做（而不是让每个监听器自己判）：插件世界里不该出现别的插件的事件。
      //
      // `typeof off === 'function'` 这一道看着多余，其实不是：旧主进程 / 探针的假 api 上
      // 没有这个方法时，取到的可能是一个**返回 Promise 的兜底函数**。disposer 不是函数
      // 的后果是插件在 effect cleanup 里 `off()` 直接抛 → 整棵子树卸载（PR-10 的崩溃卡），
      // 而根因只是「宿主版本老」。
      if (!agentApi?.onSurfaceAgentEvent) return track(() => {})
      const raw = agentApi.onSurfaceAgentEvent((p) => {
        if (!p || p.surface_id !== id) return // 别的插件的事件：连监听器都不进
        if (!p.event || p.event.type !== type) return
        // P2b 宿主门禁（`requires_panel`）：面板没开时**不投递**，由宿主直接回
        // `tool.result{ok:false,error:"panel_not_open"}`。见 panelBlocked / replyPanelBlocked。
        if (panelBlocked(p, id, deps)) {
          replyPanelBlocked(p, tokenPromise, agentApi)
          return
        }
        fn(p.event)
      })
      return track(typeof raw === 'function' ? raw : () => {})
    },
  }
}

/** 「普通对象」判据（摊平 data 前的形状检查；数组/null/类实例都不摊）。 */
function isPlainRecord(v: unknown): v is Record<string, unknown> {
  if (!v || typeof v !== 'object' || Array.isArray(v)) return false
  const proto = Object.getPrototypeOf(v)
  return proto === Object.prototype || proto === null
}

/**
 * applyManifest 加载并应用一个插件（失败时**把已登记的 provider 全部撤销**再抛）。
 *
 * 校验顺序（先便宜后昂贵）：kind（manifest）→ import → 模块形状 → kind（模块）→ apply。
 * `kind` 判两次是刻意的：manifest 说 `plugin:demo`、panel.js 说 `plugin:other` 时，
 * 无论信哪一侧都会出现「注册表里有个 kind 与文件名对不上的 provider」——注册表按 kind
 * 索引，那种错位的症状是「另一个插件的标签渲染成了这个插件的面板」。
 */
async function applyManifest(item: SurfaceIndexItem, baseUrl: SurfaceBaseUrl, deps: PluginHostDeps): Promise<LoadedPlugin> {
  const id = item.id
  const expected = pluginKindOf(id)
  if (item.kind !== expected) {
    throw new Error(`surface.json 的 kind 必须是 ${expected}（实得 ${item.kind || '空'}）`)
  }
  const base = baseOf(baseUrl, id)
  if (!base) throw new Error('baseUrl 为空')
  const entry = item.entry || 'panel.js'
  // `?v=<hash>` 是缓存键：hash 变了 URL 就变了，浏览器不会把旧模块记录复用到新版本上。
  const url = `${base}/${entry}?v=${encodeURIComponent(item.hash || '0')}`

  let mod: { surface?: unknown; default?: unknown }
  try {
    mod = (await import(/* @vite-ignore */ url)) as { surface?: unknown; default?: unknown }
  } catch (e) {
    // import 失败最常见的是「panel.js 语法错 / 404」——原样上抛，由 sync 登记。
    // cause 带上原始错误：reason 只给一条人读的话，栈要留给 console.warn 与排查。
    throw new Error(`import ${url} 失败：${e instanceof Error ? e.message : String(e)}`, { cause: e })
  }
  const surface = (mod?.surface ?? mod?.default) as { kind?: unknown; apply?: unknown } | undefined
  if (!surface || typeof surface !== 'object') throw new Error('panel.js 没有导出 surface')
  if (surface.kind !== expected) {
    throw new Error(`panel.js 的 surface.kind 必须是 ${expected}（实得 ${String(surface.kind ?? '空')}）`)
  }
  if (typeof surface.apply !== 'function') throw new Error('surface.apply 不是函数')

  const disposers: Array<() => void> = []
  const host = createHost(item, deps, disposers)
  const iconUrl = item.icon ? `${base}/${item.icon}` : ''
  const surfacesApi = {
    register(p: Omit<SurfaceProvider, 'namespace'>) {
      if (!p || p.kind !== expected) {
        throw new Error(`插件只能注册自己的 kind：期望 ${expected}，实得 ${String(p?.kind)}`)
      }
      const provider = {
        ...p,
        namespace: 'plugin' as const,
        // 插件没给 title 时用 id 兜底：注册表不校验 title，而缺失的 title 会在
        // RightPanel 的 tabTitle 里抛 TypeError —— 那时表现是「整条标签条崩了」。
        title: typeof p.title === 'function' ? p.title : () => id,
        // icon.svg 存在就用协议 URL（插件自己声明了 icon 则尊重它的）。
        ...(item.icon && !p.icon ? { icon: () => iconNode(iconUrl) } : {}),
      } as SurfaceProvider
      registerSurface(provider)
      disposers.push(() => {
        unregisterSurface(provider.kind)
      })
    },
  }

  try {
    await (surface.apply as (ctx: unknown) => unknown)({ host, React: host.React, surfaces: surfacesApi })
  } catch (e) {
    // 半成功必须回滚：apply 里可能已经 register 过一个 provider 之后再抛。
    for (const d of [...disposers].reverse()) {
      try {
        d()
      } catch {
        /* 撤销自己的失败不该掩盖原始错误 */
      }
    }
    throw e
  }

  const dispose = (): void => {
    // 逆序：先撤销后注册的，事件订阅最后（与 React 的 effect cleanup 同一顺序直觉）。
    for (const d of [...disposers].reverse()) {
      try {
        d()
      } catch {
        /* 单个 disposer 失败不影响其余（否则一个坏插件能锁住整张注册表） */
      }
    }
    loaded.delete(id)
  }
  return { id, kind: expected, hash: item.hash, entry, revision: revisionOf(item), dispose }
}

/** 入口声明（按 params.entry 找）；没找到 = 零 entry 的默认入口 / 老标签。 */
function webEntryOf(item: SurfaceIndexItem, tab: PluginTabInstance): { path: string; title: string } {
  const entryId = typeof tab.params?.entry === 'string' ? tab.params.entry : ''
  const decl = (item.entries ?? []).find((e) => e.id === entryId)
  if (decl) return { path: decl.path || '', title: entryTitleText(decl.title, getHostLang()) }
  return { path: '', title: item.name || item.id }
}

/**
 * applyWebManifest 加载一个 URL 型插件（manifest.web）：不 import panel.js，注册一个
 * 宿主 provider，页面在原生视图里跑（见 electron/plugin-app.ts）。
 *
 * 与 applyManifest 的共同点：kind 校验、createHost、manifest 白名单、dispose 回滚、
 * 失败只黑自己。不同点只有「内容从哪来」——ESM 是 import 的 React 组件，web 是
 * `web.url + entries[].path` 的原生视图。
 */
function applyWebManifest(item: SurfaceIndexItem, deps: PluginHostDeps): LoadedPlugin {
  const id = item.id
  const expected = pluginKindOf(id)
  if (item.kind !== expected) {
    throw new Error(`surface.json 的 kind 必须是 ${expected}（实得 ${item.kind || '空'}）`)
  }
  const web = item.web
  if (!web?.url) throw new Error('manifest.web.url 缺失')

  const disposers: Array<() => void> = []
  const host = createHost(item, deps, disposers)
  webHosts.set(id, host)

  // 事件：宿主先按 manifest.data 订好，再统一转发给该插件的所有视图。为什么不给页面
  // 加「订阅才转发」的协议：tool.call 的 requires_panel 门禁需要在页面还没订阅时就
  // 成立；而 agent.* 的过滤本来就在 Go 侧按 surface_id 做完了。
  for (const type of item.data) {
    try {
      host.onEvent(type, (ev) => {
        void bridge?.pluginAppEvent?.({ pluginId: id, type, event: ev })
      })
    } catch (e) {
      console.warn(`[surface] web 插件 ${id} 订阅 ${type} 失败：`, e)
    }
  }

  const provider: SurfaceProvider<PluginTabInstance> = {
    kind: expected,
    namespace: 'plugin',
    group: 'plugin',
    title: (tab) => webEntryOf(item, tab).title,
    ...(item.icon ? { icon: () => iconNode(pluginAssetUrl(id, item.icon!)) } : {}),
    // 页面就是面板本身：去掉 .right-body 的 10px 内边距与滚动条（见 index.css full-mode）。
    fullBleed: true,
    // 视图由 PluginAppHost 自己 show/hide；RightPanel 的全局 embed 闸门不该把它当成
    // 「没人认领的视图」而误伤（那是浏览器 embed 的闸门，两者是两张池子）。
    nativeHost: () => true,
    render: ({ tab }) => {
      const props: PluginAppHostProps = {
        pluginId: id,
        tabId: tab.id,
        url: appUrlOf(web.url, webEntryOf(item, tab).path),
        webUrl: web.url,
        events: item.data,
        commands: item.commands,
        ...(web.start ? { start: web.start } : {}),
        ...(web.permissions?.length ? { permissions: web.permissions } : {}),
      }
      return appHostComponent
        ? React.createElement(appHostComponent, props)
        : React.createElement('div', { className: 'plugin-app-state', 'data-plugin-app-missing': id }, 'URL 插件宿主未注册')
    },
    onClose: (tab) => {
      void bridge?.pluginAppClose?.(`${id}:${tab.id}`)
    },
  }

  try {
    registerSurface(provider)
  } catch (e) {
    webHosts.delete(id)
    for (const d of [...disposers].reverse()) {
      try {
        d()
      } catch {
        /* 撤销自己的失败不掩盖原始错误 */
      }
    }
    throw e
  }
  disposers.push(() => unregisterSurface(expected))

  const dispose = (): void => {
    for (const d of [...disposers].reverse()) {
      try {
        d()
      } catch {
        /* 单个 disposer 失败不影响其余 */
      }
    }
    webHosts.delete(id)
    void bridge?.pluginAppClosePlugin?.(id)
    loaded.delete(id)
  }
  return { id, kind: expected, hash: item.hash, entry: item.entry, revision: revisionOf(item), dispose }
}

/** reportFailure 登记 + 上报（`ui_inspect` 的 surfaces[] 与 console 都看得见）。 */
function reportFailure(item: SurfaceIndexItem, deps: PluginHostDeps, err: unknown): PluginFailure {
  const kind = pluginKindOf(item.id)
  setSurfaceLoadError(kind, item.id, err)
  deps.send?.('surface_status', { kind, mounted: false, disabled: true, error: String((err as Error)?.message ?? err).slice(0, 300) })
  // console.warn 而不是 error：一个装坏的第三方插件不该把控制台刷红（它已经被 _inspect 看见了）。
  console.warn(`[surface] 插件 ${item.id} 加载失败（已跳过，其它插件不受影响）：`, err)
  return { id: item.id, kind, error: err instanceof Error ? err.message : String(err) }
}

/** unloadPlugin 卸载一个已加载的插件（幂等）。 */
export function unloadPlugin(id: string): boolean {
  const p = loaded.get(id)
  if (!p) return false
  p.dispose()
  clearSurfaceLoadError(p.kind)
  return true
}

/** loadedPluginIds 已加载的插件 id（码点排序；测试与诊断用）。 */
export function loadedPluginIds(): string[] {
  return [...loaded.keys()].sort()
}

/** isPluginLoaded 该 id 是否已经活在注册表里（标签条/诊断用）。 */
export function isPluginLoaded(id: string): boolean {
  return loaded.has(id)
}

async function syncOnce(
  index: SurfaceIndex,
  baseUrl: SurfaceBaseUrl,
  deps: PluginHostDeps,
  opts: SyncOptions = {},
): Promise<SyncReport> {
  const report: SyncReport = { loaded: [], unloaded: [], skipped: [], failed: [] }
  currentBase = baseUrl
  // 入口快照必须在**注册/卸载之前**更新：宿主的所有入口都读这一份（R16 / D-2），
  // 而禁用 → 入口消失、启用 → 入口回来这两件事都发生在同一轮 sync 里。放在这里
  // 还保证「插件加载失败」时入口照样在（D-3 最后一行：点它能开出墓碑标签）。
  setSurfaceEntryIndex(index)
  // ⓪ 跨重启的数据回收（R17a）：index 里**全部**的 id（含 enabled:false、含带 error
  //     的）才是「现在还有谁」的真相 —— 只看 alive / loaded 会漏掉「manifest 就坏掉、
  //     从来没加载成功过」的插件，用户卸掉它时数据会原地留下。没有 known-ids 记录
  //     （首次升级）时这里一个都不清，见 storage.ts 的 reconcileKnownIds 注释。
  reconcileKnownIds(
    (index?.items ?? []).map((it) => String(it?.id ?? '')).filter((x) => x.length > 0),
    // 「这份 index 算不算数」是**显式**入参，缺省 false：浏览器版那条路回的是硬编码的空
    // items，靠 items 非空去猜会在一次网页会话里清空全部插件数据（见 storage.ts 第 0 条）。
    opts.authoritativeIndex === true,
  )
  const alive = new Map<string, SurfaceIndexItem>()
  for (const item of index?.items ?? []) {
    if (!item || item.id === undefined) continue
    if (!item.enabled) continue
    if (item.error) {
      // 坏 manifest：跳过它自己（带 error 上报），**不影响其它插件**。
      report.failed.push(reportFailure(item, deps, new Error(item.error)))
      continue
    }
    alive.set(item.id, item)
  }

  // ① 先卸载：禁用 / 从 index 消失 / 声明坏了。顺序在加载之前是刻意的 ——
  //    否则同一轮里「A 被换成一个同名的新版本」会撞上 registerSurface 的重复注册断言。
  for (const id of [...loaded.keys()]) {
    const item = alive.get(id)
    if (!item) {
      const before = loaded.get(id)
      if (unloadPlugin(id)) report.unloaded.push(id)
      if (before) console.info(`[surface] 插件 ${id} 已卸载`)
      continue
    }
    const cur = loaded.get(id)
    if (cur && cur.revision !== revisionOf(item)) {
      if (unloadPlugin(id)) report.unloaded.push(id)
    }
  }

  // ② 再加载/复用。
  for (const [id, item] of alive) {
    const cur = loaded.get(id)
    if (cur && cur.revision === revisionOf(item)) {
      report.skipped.push(id)
      clearSurfaceLoadError(cur.kind) // 之前失败过、这一轮 index 又好了 → 清掉旧的错误行
      continue
    }
    try {
      // R-20：**换版本 = 干净重来**。放在 apply 之前是刻意的 —— 新版本连 import 都过不去
      // 时，旧版本留下的「3 次失败 → 自动禁用」会继续挂在墓碑上，用户看到的是「装了新版
      // 还是坏的」。真正的坏版本由 status.ts 的 load error 报告，不走这套计数。
      noteSurfaceVersion(pluginKindOf(id), item.hash)
      const p = item.web ? applyWebManifest(item, deps) : await applyManifest(item, baseUrl, deps)
      loaded.set(id, p)
      clearSurfaceLoadError(p.kind)
      deps.send?.('surface_status', { kind: p.kind, mounted: true, disabled: false, error: '' })
      report.loaded.push(id)
    } catch (e) {
      report.failed.push(reportFailure(item, deps, e))
    }
  }
  return report
}

/**
 * syncPlugins 启动与热变更的**唯一入口**。
 *
 * 并发语义：调用方（启动 + 每次 surfaces_changed）不需要自己去重 —— 这里串成一条链，
 * 后来的调用在前一次结束后再跑一次（合并语义而不是排队 N 次：链上永远只多一次）。
 * 传进来的 index 是**调用那一刻**的快照，所以「连续两次 surfaces_changed」第二次
 * 通常只需要处理增量（hash 未变的走 skipped）。
 */
export function syncPlugins(
  index: SurfaceIndex,
  baseUrl: SurfaceBaseUrl = baseUrlOverride ?? surfaceBaseUrl,
  deps: PluginHostDeps = {},
  opts: SyncOptions = {},
): Promise<SyncReport> {
  const run = syncChain.then(() => syncOnce(index, baseUrl, deps, opts))
  // 链上只保留「最后一次」失败不炸链：否则一次失败会让后续 sync 永远 reject。
  syncChain = run.catch(() => ({ loaded: [], unloaded: [], skipped: [], failed: [] }))
  return run
}

export interface PluginLoaderDeps extends PluginHostDeps {
  /** 读索引（生产 = bridge.surfaceIndex，走主进程；e2e = 探针内的假数据） */
  readIndex: () => Promise<SurfaceIndex>
  baseUrl?: SurfaceBaseUrl
}

/**
 * startPluginLoader 生产入口：立刻同步一次 + 每次 `surfaces_changed` 再同步一次。
 *
 * 返回 stop()：**只停止监听，不卸载已加载的插件**。理由：注册表住在渲染层，与 bridge
 * 进程无关 —— bridge 重启（start() 再来一次）时把面板全卸掉，用户会看到标签一个接一个
 * 变墓碑，而它们一秒后又会全回来（那是噪音，不是状态）。
 */
export function startPluginLoader(deps: PluginLoaderDeps): () => void {
  let stopped = false
  const baseUrl = baseUrlOverride ?? deps.baseUrl ?? surfaceBaseUrl
  const run = (): void => {
    if (stopped) return
    void deps
      .readIndex()
      .then((index) =>
        // 每轮都重新问：桥起来/断掉时「index 算不算数」也会跟着变。
        syncPlugins(index, baseUrl, deps, { authoritativeIndex: deps.indexAuthoritative?.() === true }),
      )
      .catch((e: unknown) => {
        // 读索引失败**不改动注册表**：一次 IPC 抖动不该等于「所有插件都被卸载」。
        console.warn('[surface] 读插件索引失败（保持现状）：', e)
      })
  }
  run()
  const off = subscribeUI('surfaces_changed', () => run())
  // URL 型插件的页面调 host.command：main 校验来源/声明后转发到这里，再用**同一个**
  // createHost 执行（agent token、D11、manifest 白名单都在那一份里）。
  const offHost = bridge?.onPluginAppHostCommand?.((req) => {
    const host = webHosts.get(req.pluginId)
    if (!host) {
      bridge?.pluginAppHostReply?.({ id: req.id, reply: { ok: false, reason: 'unknown_id' } })
      return
    }
    void host
      .command(req.name, req.payload)
      .then((reply) => bridge?.pluginAppHostReply?.({ id: req.id, reply }))
      .catch(() => bridge?.pluginAppHostReply?.({ id: req.id, reply: { ok: false, reason: 'error' } }))
  })
  return () => {
    stopped = true
    off()
    offHost?.()
  }
}

/** 仅供测试/探针：卸载全部（生产无调用点）。 */
export function unloadAllPluginsForTest(): void {
  for (const id of [...loaded.keys()]) unloadPlugin(id)
}
