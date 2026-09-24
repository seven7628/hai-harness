// embed_v2.ts — CDP SemanticAdapter（协议驱动重构，纯逻辑、无 Electron 依赖）。
//
// 背景（docs/BROWSER_USE_EMBED_V2.md）：
//   Electron `webContents.debugger` 是 CDP 的部分实现，与 chrome-devtools-mcp 1.8.0
//   （Puppeteer 严格状态机）存在系统性语义差异（§2），逐点 patch 上游是死路。
//   本模块在 Electron debugger 之上实现「标准 CDP 语义适配层」：所有页面级事件/命令
//   经此收敛为 Chromium 标准形式后再与 Go bridge 交互。
//
// 设计原则：
//   1. 纯逻辑：不 import electron/node 运行时，所有 IO 经回调注入（emit/command），
//      因此可在 Node 单测中直接验证（node:test）。
//   2. 协议驱动：以 Puppeteer 期望的精确事件序列为准（读上游 index.js 源码提炼）：
//        - goto: Page.navigate 响应带 loaderId → newDocument 分支 → 等
//          frame._lifecycleEvents 含 waitUntil（domcontentloaded）+ _loaderId 变化；
//        - #onLifecycleEvent 按 frameId 查 frame，查不到直接丢弃；
//        - init 事件 clear frame._lifecycleEvents，load 事件必须与 init 同 loaderId；
//        - #onFrameNavigated 主 frame 复用对象（removeFrame+updateId+addFrame），
//          watcher 引用不失效，但 frameNavigated 的 async 处理链会与 lifecycle 事件
//          竞争 —— 因此本层保证「frameNavigated 先于后续 lifecycle 事件产出」。
//   3. 单一出口：所有对外事件经 emit() 同步顺序广播（无优先级饥饿、无丢失）。
//
// 模块：LoaderIdManager / FrameTracker / ContextTracker / EventNormalizer /
//       CommandInterceptor / SemanticAdapter（组装）。

// ============ 类型定义（协议 v2 与 CDP 结构） ============

export interface CdpEvent {
  method: string
  params: Record<string, unknown>
}

export interface CdpCommand {
  id: number
  method: string
  params?: Record<string, unknown>
}

export interface CdpResponse {
  id: number
  result?: Record<string, unknown>
  error?: string
}

export interface AdapterCommandResult {
  /** true = 命令被拦截处理（已回复响应，不再下发 debugger） */
  intercepted: boolean
  /** 若 intercepted，回复给对端的响应 */
  response?: CdpResponse
  /** 响应写回对端后调用（用于「先响应后事件」的时序保证——goto 的
   *  LifecycleWatcher 在 navigate 响应到达时创建并记录 initialLoaderId，
   *  若 init 事件先于响应到达会把 _loaderId 改成新值 → watcher 记录的
   *  initialLoaderId 也变成新值 → newDocument 判定（_loaderId !== initial）
   *  永不满足 → 超时。因此补发 init 必须**晚于** navigate 响应）。 */
  afterResponse?: () => void
}

/** 视图导航生命周期（Electron 原生事件抽象，由宿主注入） */
export interface NativeNavEvents {
  /** did-start-loading */
  onStartLoading?: () => void
  /** did-finish-load（真实 load 完成） */
  onFinishLoad?: () => void
  /** did-navigate（主 frame 提交新文档，带 url） */
  onNavigate?: (url: string) => void
  /** did-fail-load */
  onFailLoad?: (code: number, desc: string) => void
}

/** 宿主注入的 IO 通道（生产：Electron webContents.debugger + TCP broadcast） */
export interface AdapterHost {
  targetId: string
  /** 向对端广播事件（协议 v2 event 行） */
  emit: (method: string, params: Record<string, unknown>) => void
  /** 向 Electron debugger 下发命令（返回 promise），失败抛错 */
  sendCommand: (method: string, params?: Record<string, unknown>) => Promise<unknown>
  /** 导航（loadURL/reload 等原生操作） */
  navigate: (url: string) => Promise<void>
  reload: () => void
  /** 取真实视口尺寸（用于 setDeviceMetricsOverride 改写） */
  viewport: () => { width: number; height: number } | null
  /** 日志 */
  log?: (msg: string) => void
}

// ============ LoaderIdManager ============

let loaderSeq = 0

/**
 * LoaderIdManager：分配稳定 loaderId。
 * - Page.navigate 响应 / lifecycleEvent init/load 必须同 loaderId（Puppeteer 判定）。
 * - 格式 nav-<ts36>-<targetId>（非空、可读、可追踪）。
 */
export class LoaderIdManager {
  next(targetId: string): string {
    loaderSeq++
    return `nav-${Date.now().toString(36)}-${loaderSeq.toString(36)}-${targetId}`
  }
}

// ============ FrameTracker ============

export interface FrameState {
  frameId: string
  loaderId: string
  lifecycleEvents: Set<string>
  url: string
  /** 是否已产出 frameNavigated（当前 loaderId 周期） */
}

/**
 * FrameTracker：追踪每个 target 的主 frame 状态机。
 *
 * Chromium 标准事件序列（Puppeteer 期望）：
 *   lifecycleEvent(init, loaderId) → frameNavigated → lifecycleEvent(DOMContentLoaded)
 *   → lifecycleEvent(load) → networkIdle
 *
 * 本层保证：
 *   1. init 与 load 同 loaderId（Electron 常不一致 → 补发统一）；
 *   2. frameNavigated 在 lifecycle 事件之前产出（async 竞争消除）；
 *   3. 首次导航（无 frameId 缓存）也能产出（宿主取 getResourceTree 后 setFrameId）。
 */
export class FrameTracker {
  private state: FrameState | null = null
  private navPending = false
  /** 挂起的导航补发（navigate 拦截时 frame 尚未初始化 → 等 getFrameTree/frameNavigated 后 flush） */
  private pendingLifecycle: { loaderId: string; url: string } | null = null

  constructor(
    private readonly targetId: string,
    private readonly loaderIds: LoaderIdManager,
    private readonly emit: AdapterHost['emit'],
    private readonly log?: AdapterHost['log'],
  ) {}

  /** 宿主在 attach 后初始化（getResourceTree 得到真实 frameId） */
  initFrame(frameId: string, url = ''): void {
    if (!frameId) return
    if (this.state && this.state.frameId === frameId) {
      // 已有同 id frame：仅补 url
      this.state.url = url
      return
    }
    this.state = {
      frameId,
      loaderId: '',
      lifecycleEvents: new Set(['init']),
      url,
    }
    this.log?.(`FrameTracker.initFrame(${this.targetId}) frameId=${frameId}`)
  }

  get frameId(): string {
    return this.state?.frameId ?? ''
  }

  get loaderId(): string {
    return this.state?.loaderId ?? ''
  }

  get lifecycleEvents(): ReadonlySet<string> {
    return this.state?.lifecycleEvents ?? new Set()
  }

  /** 新导航开始（Electron did-start-loading / Page.navigate 命令到达时） */
  beginNavigation(): void {
    this.navPending = true
  }

  /** 分配一个 loaderId（navigate 响应与补发事件共用） */
  nextLoaderId(): string {
    return this.loaderIds.next(this.targetId)
  }

  /**
   * 产出标准 lifecycle 序列（init + load，同 loaderId）。
   * 在 Page.navigate 提交（提交即返回）时调用：让 goto 立即满足。
   * - init: 更新 frame._loaderId + clear lifecycle（Puppeteer 判定 newDocument）
   * - load: 补全 waitUntil（patch 后 domcontentloaded；若原生 DOMContentLoaded 未到，
   *         补发 load 与 DOMContentLoaded 均可——但标准序列应先 DOMContentLoaded 再 load）
   * 顺序按 Chromium：init → DOMContentLoaded → load。
   */
  emitLifecyclePair(): string {
    return this.emitLifecyclePairWith(this.loaderIds.next(this.targetId))
  }

  /** 指定 loaderId 产出标准 lifecycle 序列（init/DOMContentLoaded/load 同 loaderId） */
  emitLifecyclePairWith(loaderId: string): string {
    if (!this.state) return ''
    if (!loaderId) loaderId = this.loaderIds.next(this.targetId)
    this.state.loaderId = loaderId
    this.state.lifecycleEvents = new Set()
    const frameId = this.state.frameId
    const ts = Date.now() / 1000
    this.emit('Page.lifecycleEvent', { frameId, loaderId, name: 'init', timestamp: ts })
    this.emit('Page.lifecycleEvent', { frameId, loaderId, name: 'DOMContentLoaded', timestamp: ts })
    this.emit('Page.lifecycleEvent', { frameId, loaderId, name: 'load', timestamp: ts })
    this.state.lifecycleEvents.add('init')
    this.state.lifecycleEvents.add('DOMContentLoaded')
    this.state.lifecycleEvents.add('load')
    this.log?.(`FrameTracker.emitLifecyclePair(${this.targetId}) loaderId=${loaderId}`)
    return loaderId
  }

  /** 挂起导航补发（frame 未初始化时调用；getFrameTree/frameNavigated 后 flush） */
  holdLifecyclePair(loaderId: string, url: string): void {
    this.pendingLifecycle = { loaderId, url }
    this.log?.(`FrameTracker.holdLifecyclePair(${this.targetId}) loaderId=${loaderId}（等 frame 初始化后补发）`)
  }

  /**
   * flush 挂起的导航补发。
   * 触发时机：Puppeteer 的 getFrameTree/getResourceTree 命令到达（frame 表已建，
   * 补发事件能被 `#onLifecycleEvent` 消费）、或首个 frameNavigated 事件。
   * 无法补发（无 frameId）时**保留 pending**（等下次触发）。
   * 返回是否已补发。
   */
  flushPendingLifecycle(frameId?: string, url?: string): boolean {
    const pending = this.pendingLifecycle
    if (!pending) return false
    if (!this.state) {
      // 仍无 state：用宿主给的 frameId 初始化（getFrameTree 响应场景）
      if (frameId) {
        this.initFrame(frameId, url ?? pending.url)
      } else {
        return false // 无 frameId，保留 pending（等 frameNavigated）
      }
    }
    this.pendingLifecycle = null
    this.emitLifecyclePairWith(pending.loaderId)
    this.log?.(`FrameTracker.flushPendingLifecycle(${this.targetId}) loaderId=${pending.loaderId}`)
    return true
  }

  /**
   * 标准化 frameNavigated（宿主转发 Electron 原始 frameNavigated 时调用）。
   * - 保证 frame.id 存在（Electron 结构不标准 → 补 id）；
   * - 总是透传（Puppeteer 需要它更新 frame.url + 触发 watcher check；
   *   #onFrameNavigated 主 frame 复用对象，watcher 引用不失效）；
   * - 更新 url。
   */
  normalizeFrameNavigated(params: Record<string, unknown>): CdpEvent | null {
    const raw = (params.frame ?? params) as Record<string, unknown>
    const frameId = (raw.id as string) || (params.frameId as string) || this.state?.frameId || ''
    const url = (raw.url as string) ?? this.state?.url ?? ''
    if (!frameId) return null
    // 首次 frameNavigated（此前无 state）：若存在挂起导航补发，先 flush
    // （frame 表已建立，补发能被 Puppeteer 消费）
    const hadPending = !this.state && !!this.pendingLifecycle
    this.initFrame(frameId, url)
    if (hadPending) this.flushPendingLifecycle()
    if (this.state) this.state.url = url
    // 标准结构：frame.id / loaderId / url 完整
    const framePayload: Record<string, unknown> = {
      ...raw,
      id: frameId,
      url,
      loaderId: this.state?.loaderId || undefined,
    }
    const out: CdpEvent = { method: 'Page.frameNavigated', params: { frame: framePayload, type: 'Navigation' } }
    this.log?.(`FrameTracker.normalizeFrameNavigated(${this.targetId}) frameId=${frameId}`)
    return out
  }

  /**
   * 标准化 lifecycle 事件（宿主转发 Electron 原始 lifecycleEvent 时调用）。
   * - init：更新 loaderId + 清 lifecycle（Puppeteer 语义），但**不补发**
   *   （emitLifecyclePair 已产出标准序列；重复 init 会清掉刚补的 DOMContentLoaded/load）；
   * - 其他（DOMContentLoaded/load/networkIdle）：透传，frameId 必须存在。
   */
  normalizeLifecycleEvent(params: Record<string, unknown>): CdpEvent | null {
    const name = params.name as string
    const frameId = (params.frameId as string) || this.state?.frameId || ''
    const loaderId = (params.loaderId as string) || this.state?.loaderId || ''
    if (!frameId) return null
    this.initFrame(frameId)
    if (!this.state) return null
    if (name === 'init') {
      // 原生 init（Electron 在导航开始时产出）：
      //   - 若本周期已由 emitLifecyclePair 补发过（lifecycle 完整），**不采纳**原生
      //     loaderId（避免状态漂移；Puppeteer 的 _loaderId 已由补发 init 设为 L1，
      //     后续事件都作用在同一 frame，语义一致）；
      //   - 否则（纯原生路径，无补发）采纳原生 loaderId（Puppeteer 语义：init 更新
      //     frame._loaderId + clear lifecycle——但这里不 clear，因为本层不负责
      //     Puppeteer 内部状态，只更新追踪值）。
      if (this.state.lifecycleEvents.has('load')) {
        // 补发周期：保持现有 loaderId（不漂移）
        return null
      }
      if (loaderId) {
        this.state.loaderId = loaderId
        this.log?.(`FrameTracker.normalizeLifecycle(init) ${this.targetId} loaderId=${loaderId}`)
      }
      // 原生 init 不补发（避免重复 init clear 掉 Puppeteer 已收的 lifecycle——
      // 但纯原生路径下 Puppeteer 需要 init 来设置 _loaderId！所以**透传**原生 init）
      if (loaderId) {
        return {
          method: 'Page.lifecycleEvent',
          params: { frameId, loaderId, name: 'init', timestamp: params.timestamp ?? Date.now() / 1000 },
        }
      }
      return null
    }
    // 透传标准 lifecycle（frameId 保证）
    this.state.lifecycleEvents.add(name)
    const out: CdpEvent = {
      method: 'Page.lifecycleEvent',
      params: { frameId, loaderId: loaderId || this.state.loaderId, name, timestamp: params.timestamp ?? Date.now() / 1000 },
    }
    return out
  }

  /** 原生 did-finish-load：补发完整 lifecycle（兜底：navigate 未走拦截路径时） */
  onFinishLoad(): CdpEvent[] {
    if (!this.state) return []
    if (this.state.lifecycleEvents.has('load')) return [] // 已补过
    const loaderId = this.state.loaderId || this.loaderIds.next(this.targetId)
    this.state.loaderId = loaderId
    const frameId = this.state.frameId
    const ts = Date.now() / 1000
    this.state.lifecycleEvents.add('init')
    this.state.lifecycleEvents.add('DOMContentLoaded')
    this.state.lifecycleEvents.add('load')
    this.log?.(`FrameTracker.onFinishLoad(${this.targetId}) 补发 init+DOMContentLoaded+load loaderId=${loaderId}`)
    // 完整序列：init（更新 _loaderId）→ DOMContentLoaded → load（同 loaderId）
    return [
      { method: 'Page.lifecycleEvent', params: { frameId, loaderId, name: 'init', timestamp: ts } },
      { method: 'Page.lifecycleEvent', params: { frameId, loaderId, name: 'DOMContentLoaded', timestamp: ts } },
      { method: 'Page.lifecycleEvent', params: { frameId, loaderId, name: 'load', timestamp: ts + 0.001 } },
    ]
  }

  /** 原生 did-navigate：主 frame 提交（可能 frameId 变化），重置本周期标记 */
  onNavigate(url: string): void {
    if (this.state) this.state.url = url
    this.navPending = false
  }

  onFailLoad(): void {
    this.navPending = false
  }

  get hasState(): boolean {
    return this.state !== null
  }
}

// ============ ContextTracker ============

export interface ContextInfo {
  id: number
  name: string
  frameId: string
  isDefault: boolean
  type: string
}

/**
 * ContextTracker：追踪 execution context（主 world + utility world）。
 *
 * 问题（§2.4）：Electron 的 Page.createIsolatedWorld 产出的 executionContextCreated
 * 时序/结构不标准（命令响应前到达 → Puppeteer 丢弃；缺 auxData.frameId → 不绑定 frame）。
 *
 * 本层：
 *   - 监听 debugger 原始 executionContextCreated，记录 context（含真实 id）；
 *   - createIsolatedWorld 命令**响应后**补发标准 executionContextCreated
 *     （name 精确匹配 `__puppeteer_utility_world__vXX`，auxData.frameId 正确）；
 *   - Runtime.evaluate 带 utility contextId → 剥离（Electron 不认，主 world 执行）。
 */
export class ContextTracker {
  /** name → contextId（utility world） */
  private utilityWorlds = new Map<string, number>()
  /** 主 world contextId（默认 world） */
  private mainWorldCtxId: number | null = null
  private seq = 0
  /** Runtime.enable 已过但主 world context 未缓存 → 等事件到达补发 */
  mainCtxPending = false

  constructor(
    private readonly targetId: string,
    private readonly emit: AdapterHost['emit'],
    private readonly log?: AdapterHost['log'],
    private readonly onMainCtxReady?: (ctxId: number) => void,
  ) {}

  /** 记录 debugger 原始 context 事件（透传前调用） */
  observeContextCreated(context: Record<string, unknown>): void {
    const id = context.id as number
    const name = context.name as string
    const aux = (context.auxData ?? {}) as Record<string, unknown>
    if (typeof id !== 'number') return
    if (name && name.startsWith('__puppeteer_utility_world__')) {
      this.utilityWorlds.set(name, id)
      this.log?.(`ContextTracker.observe(utility) ${this.targetId} name=${name} id=${id}`)
    } else if (aux.isDefault === true) {
      this.mainWorldCtxId = id
      this.log?.(`ContextTracker.observe(main) ${this.targetId} id=${id}`)
      // Runtime.enable 已过但当时主 world context 未缓存：现在补发（Puppeteer
      // MAIN_WORLD 需要它；延迟到事件到达保证用真实 id）
      if (this.mainCtxPending) {
        this.mainCtxPending = false
        this.onMainCtxReady?.(id)
      }
    }
  }

  /** createIsolatedWorld 响应后调用：补发标准 executionContextCreated */
  emitUtilityContext(worldName: string, frameId: string): number {
    const existing = this.utilityWorlds.get(worldName)
    const id = existing ?? 1000 + ++this.seq * 10 + (this.targetId.charCodeAt(1) || 0)
    this.utilityWorlds.set(worldName, id)
    this.emit('Runtime.executionContextCreated', {
      context: {
        id,
        origin: '',
        name: worldName,
        uniqueId: `go-code-util-${this.targetId}-${id}`,
        auxData: { isDefault: false, frameId, type: 'isolated' },
      },
    })
    this.log?.(`ContextTracker.emitUtilityContext ${this.targetId} world=${worldName} ctxId=${id} frameId=${frameId}`)
    return id
  }

  /**
   * Runtime.enable 响应后调用：补发主 world 的 executionContextCreated。
   * Electron debugger 的 Runtime.enable **不重放主 world context**（只重放
   * isolated/utility context）→ Puppeteer 的 MAIN_WORLD 永远无 context →
   * 所有走主 world 的操作（page.evaluate/frame.evaluate）等 #waitForExecutionContext
   * 30s 超时。真实 Chrome 的 Runtime.enable 会重放全部已存在 context。
   * 用缓存的真实主 world contextId 补发（isDefault=true）。
   */
  emitMainContext(frameId: string): number | null {
    if (this.mainWorldCtxId == null) return null
    const id = this.mainWorldCtxId
    this.emit('Runtime.executionContextCreated', {
      context: {
        id,
        origin: '',
        name: '',
        uniqueId: `go-code-main-${this.targetId}-${id}`,
        auxData: { isDefault: true, frameId, type: 'default' },
      },
    })
    this.log?.(`ContextTracker.emitMainContext ${this.targetId} ctxId=${id} frameId=${frameId}`)
    return id
  }

  /** Runtime.evaluate 参数预处理：剥离 utility contextId（返回是否改写） */
  stripUtilityContextId(params: Record<string, unknown>): boolean {
    const ctxId = params.contextId
    if (typeof ctxId !== 'number') return false
    // 只剥离 utility world 的 id（主 world 保留——Electron 认默认 world id）
    for (const id of this.utilityWorlds.values()) {
      if (id === ctxId) {
        delete params.contextId
        this.log?.(`ContextTracker.stripUtilityContextId ${this.targetId} ctxId=${ctxId}`)
        return true
      }
    }
    return false
  }

  get mainCtxId(): number | null {
    return this.mainWorldCtxId
  }
}

// ============ EventNormalizer ============

/**
 * EventNormalizer：debugger 原始事件 → 标准 CDP 事件。
 * - 只处理关键事件（Page 生命周期、Runtime.executionContextCreated），其余透传；
 * - 统一出口：返回 CdpEvent[]（可为空），由 SemanticAdapter 广播。
 */
export class EventNormalizer {
  constructor(
    private readonly targetId: string,
    private readonly frames: FrameTracker,
    private readonly contexts: ContextTracker,
    private readonly log?: AdapterHost['log'],
  ) {}

  normalize(method: string, params: Record<string, unknown>): CdpEvent[] {
    const out: CdpEvent[] = []
    switch (method) {
      case 'Page.frameNavigated':
      case 'Page.frameAttached': {
        const ev = this.frames.normalizeFrameNavigated(params)
        if (ev) out.push(ev)
        break
      }
      case 'Page.lifecycleEvent': {
        const ev = this.frames.normalizeLifecycleEvent(params)
        if (ev) out.push(ev)
        break
      }
      case 'Runtime.executionContextCreated': {
        const ctx = (params.context ?? {}) as Record<string, unknown>
        this.contexts.observeContextCreated(ctx)
        // 透传（Puppeteer 需要 context 事件；标准化 auxData.frameId）
        if (ctx.auxData && typeof ctx.auxData === 'object') {
          const aux = ctx.auxData as Record<string, unknown>
          if (!aux.frameId) aux.frameId = this.frames.frameId
        }
        out.push({ method, params })
        break
      }
      case 'Runtime.executionContextDestroyed':
      case 'Runtime.executionContextsCleared': {
        out.push({ method, params })
        break
      }
      case 'Network.requestWillBeSent': {
        // **关键补全**：Electron 的 requestWillBeSent 缺 `type` 字段，而 Puppeteer 的
        // isNavigationRequest() = `requestId === loaderId && type === 'Document'`
        // （上游 index.js L59234）——无 type → 导航请求永远不匹配 → LifecycleWatcher
        // 的 navigationResponse() 永不 resolve → goto 60s 超时（new_page 根因）。
        // 按 CDP 语义：requestId === loaderId 即主 frame 导航请求 → 补 type='Document'。
        const requestId = params.requestId as string
        const loaderId = params.loaderId as string
        if (typeof requestId === 'string' && typeof loaderId === 'string' && requestId === loaderId && !params.type) {
          params.type = 'Document'
          this.log?.(`EventNormalizer.requestWillBeSent(${this.targetId}) 补 type=Document requestId=${requestId}`)
        }
        out.push({ method, params })
        break
      }
      default:
        // 其余（Network/Debugger/Input/Emulation...）透传
        out.push({ method, params })
        break
    }
    return out
  }
}

// ============ CommandInterceptor ============

/**
 * CommandInterceptor：命令拦截/改写（协议 v2 cmd → Electron debugger）。
 *
 * 拦截表（Puppeteer 期望的 CDP 语义）：
 *   - Page.navigate        → loadURL 提交即返回 + 响应带 loaderId + 立即补发
 *                            init/DOMContentLoaded/load（goto newDocument 满足）
 *   - Page.reload          → webContents.reload()（同步返回 {}）
 *   - Page.createIsolatedWorld → 透传 debugger + 响应后补发标准 executionContextCreated
 *   - Runtime.evaluate     → 剥离 utility contextId（Electron 不认）
 *   - Emulation.setDeviceMetricsOverride → 改写为真实视口（所见即所得）
 *   - Page.setLifecycleEventsEnabled / Page.enable → 透传（debugger 原生支持）
 */
export class CommandInterceptor {
  constructor(
    private readonly targetId: string,
    private readonly frames: FrameTracker,
    private readonly contexts: ContextTracker,
    private readonly host: AdapterHost,
  ) {}

  /**
   * 处理一个命令。返回拦截结果：
   *   - intercepted=true：已回复（同步或异步经 host.sendCommand），
   *     调用方不再走默认路径；
   *   - intercepted=false：需调用方透传 debugger（默认路径）。
   */
  intercept(cmd: CdpCommand): AdapterCommandResult {
    const { method, params = {} } = cmd
    switch (method) {
      case 'Page.navigate': {
        const url = params.url as string
        this.frames.beginNavigation()
        void this.host.navigate(url ?? 'about:blank').catch(() => {})
        // 提交即返回：响应带 loaderId（goto 走 newDocument 分支）+ 补发标准 lifecycle
        // （init/DOMContentLoaded/load 同 loaderId，Puppeteer 判定 `_loaderId !== initial`）。
        // 若 frame 尚未初始化（无 frameId）：**挂起补发**——等 Puppeteer 的
        // getFrameTree/getResourceTree 命令到达（frame 表已建）或首个 frameNavigated
        // 事件时再 flush。**关键**：Puppeteer 的 FrameManager.initialize 是异步的，
        // navigate 命令可能先于 getFrameTree 发出（Electron 3ms 即回 vs Chrome 几十
        // ms）——若此时补发 lifecycle，`#onLifecycleEvent` 按 frameId 查 frame 表为空
        // → 事件丢弃 → goto 永不满足（实测 new_page 60s 超时根因）。
        let loaderId = ''
        if (this.frames.hasState) {
          loaderId = this.frames.nextLoaderId()
        } else {
          loaderId = `nav-${Date.now().toString(36)}`
          this.frames.holdLifecyclePair(loaderId, url)
        }
        return {
          intercepted: true,
          response: { id: cmd.id, result: { loaderId } },
          // **关键时序**：补发 init 必须**晚于** navigate 响应——LifecycleWatcher
          // 在响应到达时创建并记录 initialLoaderId = frame._loaderId；若 init 先到
          // （把 _loaderId 改成新值），watcher 记录的 initial 也是新值 → newDocument
          // 判定（_loaderId !== initial）永不满足 → goto 超时（实测 60s 根因）。
          afterResponse: () => {
            if (this.frames.hasState) {
              this.frames.emitLifecyclePairWith(loaderId) // 与响应同 loaderId
            } else {
              this.frames.flushPendingLifecycle()
            }
          },
        }
      }
      case 'Page.reload': {
        this.host.reload()
        return { intercepted: true, response: { id: cmd.id, result: {} } }
      }
      case 'Page.getFrameTree':
      case 'Page.getResourceTree': {
        // Puppeteer initialize 的 frame 表建立命令：若存在挂起导航补发，先 flush
        // （补发事件需在 getFrameTree 之后发出才能被 `#onLifecycleEvent` 消费）
        this.frames.flushPendingLifecycle()
        return { intercepted: false }
      }
      case 'Page.createIsolatedWorld': {
        // 透传（Electron 真实创建 world）→ 宿主在 sendCommand resolve 后调
        // onCommandDone 补发标准 executionContextCreated
        return { intercepted: false }
      }
      case 'Runtime.enable': {
        // Electron debugger 的 Runtime.enable 不重放主 world context（Puppeteer
        // 的 MAIN_WORLD 依赖它绑定 → page.evaluate 超时）。透传 + 响应后补发主
        // world context（onCommandDone 处理）。
        return { intercepted: false }
      }
      case 'Runtime.evaluate': {
        this.contexts.stripUtilityContextId(params)
        return { intercepted: false }
      }
      case 'Emulation.setDeviceMetricsOverride': {
        const vp = this.host.viewport()
        if (vp && typeof params.width === 'number' && params.width > 0) {
          params.width = Math.round(vp.width)
          params.height = Math.round(vp.height)
        }
        return { intercepted: false }
      }
      default:
        return { intercepted: false }
    }
  }

  /** createIsolatedWorld 透传完成后的回调（宿主调用） */
  afterCreateIsolatedWorld(worldName: string, frameId: string): void {
    this.contexts.emitUtilityContext(worldName, frameId)
  }
}

// ============ SemanticAdapter（组装） ============

export interface AdapterEventOut {
  /** 广播给对端的事件列表（顺序保证） */
  events: CdpEvent[]
}

/**
 * SemanticAdapter：单视图语义适配器。
 * 组装 LoaderIdManager / FrameTracker / ContextTracker / EventNormalizer /
 * CommandInterceptor，对外提供统一入口：
 *   - onCommand(cmd) → 命令处理（拦截/透传）
 *   - onEvent(method, params) → 事件标准化（原始 debugger → 标准 CDP）
 *   - onNativeNav(event) → Electron 原生导航事件
 *   - init(frameId) → attach 后初始化
 */
export class SemanticAdapter {
  readonly targetId: string
  readonly loaders: LoaderIdManager
  readonly frames: FrameTracker
  readonly contexts: ContextTracker
  readonly normalizer: EventNormalizer
  readonly interceptor: CommandInterceptor

  constructor(
    targetId: string,
    private readonly host: AdapterHost,
  ) {
    this.targetId = targetId
    this.loaders = new LoaderIdManager()
    this.frames = new FrameTracker(targetId, this.loaders, host.emit, host.log)
    this.contexts = new ContextTracker(targetId, host.emit, host.log, (ctxId) => {
      // 主 world context 就绪（可能晚于 Runtime.enable 响应）→ 补发
      this.contexts.emitMainContext(this.frames.frameId)
    })
    this.normalizer = new EventNormalizer(targetId, this.frames, this.contexts, host.log)
    this.interceptor = new CommandInterceptor(targetId, this.frames, this.contexts, host)
  }

  /** attach 后初始化（宿主取 getResourceTree 后调用） */
  init(frameId: string, url = ''): void {
    this.frames.initFrame(frameId, url)
  }

  /** 处理命令（协议 v2 cmd） */
  onCommand(cmd: CdpCommand): AdapterCommandResult {
    return this.interceptor.intercept(cmd)
  }

  /** 透传命令完成后的回调（宿主在 sendCommand resolve 后调用） */
  onCommandDone(cmd: CdpCommand): void {
    if (cmd.method === 'Page.createIsolatedWorld') {
      const params = cmd.params ?? {}
      const worldName = params.worldName as string
      const frameId = (params.frameId as string) || this.frames.frameId
      if (worldName) this.interceptor.afterCreateIsolatedWorld(worldName, frameId)
    }
    if (cmd.method === 'Runtime.enable') {
      // Electron 的 Runtime.enable 不重放主 world context → 补发（Puppeteer
      // MAIN_WORLD 绑定依据；不补则 page.evaluate 等 30s 超时）
      if (this.contexts.mainCtxId != null) {
        this.contexts.emitMainContext(this.frames.frameId)
      } else {
        this.contexts.mainCtxPending = true // 等主 world context 事件到达再补发
      }
    }
  }

  /** 处理 debugger 原始事件 → 标准事件列表（统一出口：已广播） */
  onEvent(method: string, params: Record<string, unknown>): CdpEvent[] {
    const evs = this.normalizer.normalize(method, params)
    for (const ev of evs) this.host.emit(ev.method, ev.params)
    return evs
  }

  /** Electron 原生导航事件（统一出口：事件已广播，返回数组供宿主检查） */
  onNativeNav(event: 'start' | 'finish' | 'navigate' | 'fail', payload?: { url?: string; code?: number; desc?: string }): CdpEvent[] {
    const out: CdpEvent[] = []
    const emit = (ev: CdpEvent): void => {
      out.push(ev)
      this.host.emit(ev.method, ev.params)
    }
    switch (event) {
      case 'start':
        this.frames.beginNavigation()
        break
      case 'navigate':
        this.frames.onNavigate(payload?.url ?? '')
        break
      case 'finish': {
        // 完整序列（init→DOMContentLoaded→load）由 onFinishLoad 产出，统一广播
        for (const ev of this.frames.onFinishLoad()) emit(ev)
        break
      }
      case 'fail':
        this.frames.onFailLoad()
        break
    }
    return out
  }
}

// ============ 视图池级适配器（多 target 路由） ============

/**
 * AdapterRegistry：targetId → SemanticAdapter 的注册表。
 * 事件/命令按 targetId 路由到对应 adapter；无 adapter 时透传（占位 target）。
 */
export class AdapterRegistry {
  private adapters = new Map<string, SemanticAdapter>()

  register(targetId: string, host: AdapterHost): SemanticAdapter {
    const a = new SemanticAdapter(targetId, host)
    this.adapters.set(targetId, a)
    return a
  }

  get(targetId: string): SemanticAdapter | undefined {
    return this.adapters.get(targetId)
  }

  remove(targetId: string): void {
    this.adapters.delete(targetId)
  }

  has(targetId: string): boolean {
    return this.adapters.has(targetId)
  }
}
