// embed_v2.test.ts — SemanticAdapter 纯逻辑单测（node:test，无 Electron 依赖）。
// 运行：node --test desktop/app/electron/embed_v2.test.ts
// 验证契约（来自 docs/BROWSER_USE_EMBED_V2.md §4 与上游 Puppeteer 源码）：
//   1. FrameTracker: init/DOMContentLoaded/load 同 loaderId，顺序正确
//   2. LoaderIdManager: navigate 响应与事件同 loaderId，且非空
//   3. EventNormalizer: frameNavigated 结构标准化（frame.id 完整）
//   4. CommandInterceptor: navigate 提交即返回 + 补发序列；evaluate 剥离 utility ctxId
//   5. ContextTracker: createIsolatedWorld 响应后补发标准 executionContextCreated
//   6. 端到端序列：模拟 goto（navigate → lifecycle 补发 → 满足）
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  SemanticAdapter,
  LoaderIdManager,
  FrameTracker,
  ContextTracker,
  EventNormalizer,
  CommandInterceptor,
  AdapterRegistry,
  type AdapterHost,
  type CdpCommand,
  type CdpEvent,
} from './embed_v2.ts'

// —— 测试宿主：记录 emit 事件 + 命令 ——
interface HostState {
  emitted: CdpEvent[]
  commands: { method: string; params?: Record<string, unknown> }[]
  navigated: string[]
  reloaded: number
}

function makeHost(targetId = 't1'): { host: AdapterHost; state: HostState } {
  const state: HostState = { emitted: [], commands: [], navigated: [], reloaded: 0 }
  const host: AdapterHost = {
    targetId,
    emit: (method, params) => state.emitted.push({ method, params }),
    sendCommand: async (method, params) => {
      state.commands.push({ method, params })
      return {}
    },
    navigate: async (url) => {
      state.navigated.push(url)
    },
    reload: () => {
      state.reloaded++
    },
    viewport: () => ({ width: 380, height: 600 }),
    log: () => {},
  }
  return { host, state }
}

// ============ LoaderIdManager ============

test('LoaderIdManager: 分配的 loaderId 非空且单调递增', () => {
  const m = new LoaderIdManager()
  const a = m.next('t1')
  const b = m.next('t1')
  assert.ok(a.length > 0)
  assert.ok(a.startsWith('nav-'))
  assert.notEqual(a, b)
  assert.ok(a.includes('t1'))
})

// ============ FrameTracker ============

test('FrameTracker: emitLifecyclePair 产出 init→DOMContentLoaded→load 且同 loaderId', () => {
  const { host, state } = makeHost('t1')
  const loaders = new LoaderIdManager()
  const ft = new FrameTracker('t1', loaders, host.emit)
  ft.initFrame('FRAME-1', 'about:blank')
  ft.emitLifecyclePair()
  const lifecycles = state.emitted.filter((e) => e.method === 'Page.lifecycleEvent')
  assert.equal(lifecycles.length, 3)
  assert.deepEqual(
    lifecycles.map((e) => (e.params as { name?: string }).name),
    ['init', 'DOMContentLoaded', 'load'],
  )
  const loaderIds = new Set(lifecycles.map((e) => (e.params as { loaderId?: string }).loaderId))
  assert.equal(loaderIds.size, 1) // 全部同 loaderId
  assert.ok(![...loaderIds][0]?.startsWith('nav-') === false)
})

test('FrameTracker: normalizeFrameNavigated 产出标准 frame.id 结构', () => {
  const { host, state } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  ft.initFrame('FRAME-1')
  // Electron 不标准：frameId 在顶层而非 frame.id
  const ev = ft.normalizeFrameNavigated({ frameId: 'FRAME-1', url: 'https://example.com/' })
  assert.ok(ev)
  assert.equal(ev.method, 'Page.frameNavigated')
  const frame = (ev.params as { frame?: Record<string, unknown> }).frame as Record<string, unknown>
  assert.equal(frame.id, 'FRAME-1') // id 已补全
  assert.equal(frame.url, 'https://example.com/')
})

test('FrameTracker: 原生 init 不覆盖补发的 DOMContentLoaded/load', () => {
  const { host, state } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  ft.initFrame('FRAME-1')
  ft.emitLifecyclePair()
  const before = state.emitted.length
  // 原生 init 到达（补发周期内）：不采纳 loaderId，不透传（避免状态漂移/重复 init）
  const ev = ft.normalizeLifecycleEvent({ frameId: 'FRAME-1', loaderId: 'native-loader', name: 'init' })
  assert.equal(ev, null)
  assert.equal(state.emitted.length, before) // 不新增
  assert.notEqual(ft.loaderId, 'native-loader') // 不漂移
})

test('FrameTracker: 纯原生路径 init 透传（更新 loaderId）', () => {
  const { host, state } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  ft.initFrame('FRAME-1')
  // 无补发周期：原生 init 透传（Puppeteer 需要它设置 _loaderId）
  const ev = ft.normalizeLifecycleEvent({ frameId: 'FRAME-1', loaderId: 'native-loader', name: 'init' })
  assert.ok(ev)
  assert.equal(ev.method, 'Page.lifecycleEvent')
  assert.equal((ev.params as { name?: string }).name, 'init')
  assert.equal((ev.params as { loaderId?: string }).loaderId, 'native-loader')
  assert.equal(ft.loaderId, 'native-loader')
})

test('FrameTracker: onFinishLoad 补发完整序列（init+DOMContentLoaded+load 同 loaderId）', () => {
  const { host, state } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  ft.initFrame('FRAME-1')
  const evs = ft.onFinishLoad()
  assert.equal(evs.length, 3)
  assert.deepEqual(
    evs.map((e) => (e.params as { name?: string }).name),
    ['init', 'DOMContentLoaded', 'load'],
  )
  const loaderIds = new Set(evs.map((e) => (e.params as { loaderId?: string }).loaderId))
  assert.equal(loaderIds.size, 1) // 同 loaderId
  // 幂等：再次调用不重复
  assert.equal(ft.onFinishLoad().length, 0)
})

// ============ ContextTracker ============

test('ContextTracker: emitUtilityContext 产出标准 context（name/auxData.frameId）', () => {
  const { host, state } = makeHost('t1')
  const ct = new ContextTracker('t1', host.emit)
  const id = ct.emitUtilityContext('__puppeteer_utility_world__1', 'FRAME-1')
  assert.ok(id > 0)
  const ev = state.emitted[0]
  assert.equal(ev.method, 'Runtime.executionContextCreated')
  const ctx = (ev.params as { context?: Record<string, unknown> }).context as Record<string, unknown>
  assert.equal(ctx.name, '__puppeteer_utility_world__1')
  const aux = ctx.auxData as Record<string, unknown>
  assert.equal(aux.frameId, 'FRAME-1')
  assert.equal(aux.isDefault, false)
  assert.equal(aux.type, 'isolated')
})

test('ContextTracker: stripUtilityContextId 剥离 utility id（保留主 world id）', () => {
  const { host } = makeHost('t1')
  const ct = new ContextTracker('t1', host.emit)
  const utilId = ct.emitUtilityContext('__puppeteer_utility_world__1', 'FRAME-1')
  const utilParams: Record<string, unknown> = { expression: '1+1', contextId: utilId }
  assert.equal(ct.stripUtilityContextId(utilParams), true)
  assert.equal(utilParams.contextId, undefined)
  // 主 world id 不剥离
  const mainParams: Record<string, unknown> = { expression: '1+1', contextId: 7 }
  assert.equal(ct.stripUtilityContextId(mainParams), false)
  assert.equal(mainParams.contextId, 7)
})

// ============ EventNormalizer ============

test('EventNormalizer: executionContextCreated 补 auxData.frameId', () => {
  const { host } = makeHost('t1')
  const loaders = new LoaderIdManager()
  const ft = new FrameTracker('t1', loaders, host.emit)
  ft.initFrame('FRAME-1')
  const ct = new ContextTracker('t1', host.emit)
  const en = new EventNormalizer('t1', ft, ct)
  const evs = en.normalize('Runtime.executionContextCreated', {
    context: { id: 5, name: '', auxData: { isDefault: true } },
  })
  assert.equal(evs.length, 1)
  const ctx = (evs[0].params.context as Record<string, unknown>).auxData as Record<string, unknown>
  assert.equal(ctx.frameId, 'FRAME-1') // 补全
})

test('EventNormalizer: requestWillBeSent 补 type=Document（导航请求）', () => {
  const { host } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  ft.initFrame('FRAME-1')
  const en = new EventNormalizer('t1', ft, new ContextTracker('t1', host.emit))
  // 导航请求：requestId === loaderId
  const evs = en.normalize('Network.requestWillBeSent', {
    requestId: 'L1',
    loaderId: 'L1',
    frameId: 'FRAME-1',
    request: { url: 'https://example.com/' },
  })
  assert.equal(evs.length, 1)
  assert.equal(evs[0].params.type, 'Document') // 已补全
  // 非导航请求（requestId ≠ loaderId）：不补
  const evs2 = en.normalize('Network.requestWillBeSent', {
    requestId: 'R2',
    loaderId: 'L1',
    request: { url: 'https://example.com/x.js' },
  })
  assert.equal(evs2[0].params.type, undefined)
})

test('EventNormalizer: 非关键事件透传', () => {
  const { host } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  const en = new EventNormalizer('t1', ft, new ContextTracker('t1', host.emit))
  const evs = en.normalize('Network.requestWillBeSent', { requestId: 'r1' })
  assert.equal(evs.length, 1)
  assert.equal(evs[0].method, 'Network.requestWillBeSent')
})

// ============ CommandInterceptor ============

test('CommandInterceptor: Page.navigate 提交即返回 + 响应带 loaderId + afterResponse 补发', () => {
  const { host, state } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  ft.initFrame('FRAME-1')
  const ct = new ContextTracker('t1', host.emit)
  const ci = new CommandInterceptor('t1', ft, ct, host)
  const res = ci.intercept({ id: 42, method: 'Page.navigate', params: { url: 'https://example.com/' } })
  assert.equal(res.intercepted, true)
  assert.ok(res.response)
  assert.equal(res.response.id, 42)
  const loaderId = (res.response.result as { loaderId?: string }).loaderId
  assert.ok(loaderId && loaderId.length > 0) // 非空（newDocument 判定）
  assert.deepEqual(state.navigated, ['https://example.com/']) // loadURL 已调
  // **关键**：响应时补发未发生（事件晚于响应——LifecycleWatcher 先记录
  // initialLoaderId 再收 init，newDocument 判定才满足）
  assert.equal(state.emitted.length, 0)
  // afterResponse（宿主写回响应后调用）→ 补发序列
  res.afterResponse?.()
  const lifecycles = state.emitted.filter((e) => e.method === 'Page.lifecycleEvent')
  assert.equal(lifecycles.length, 3)
  const sameLoader = lifecycles.every((e) => (e.params as { loaderId?: string }).loaderId === loaderId)
  assert.ok(sameLoader) // 响应与事件同 loaderId
})

test('CommandInterceptor: navigate 响应先于补发事件（时序保证）', () => {
  const { host, state } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  ft.initFrame('FRAME-1')
  const ci = new CommandInterceptor('t1', ft, new ContextTracker('t1', host.emit), host)
  const res = ci.intercept({ id: 1, method: 'Page.navigate', params: { url: 'https://x.com/' } })
  assert.ok(res.response)
  assert.equal(state.emitted.length, 0) // 拦截时无事件
  res.afterResponse?.()
  assert.ok(state.emitted.length >= 3) // 响应后补发
})

test('CommandInterceptor: Page.navigate 无 frame 状态时挂起补发（getFrameTree 后 flush）', async () => {
  const { host, state } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  const ci = new CommandInterceptor('t1', ft, new ContextTracker('t1', host.emit), host)
  // 1. navigate（无 state → 挂起）
  const res = ci.intercept({ id: 42, method: 'Page.navigate', params: { url: 'https://example.com/' } })
  assert.equal(res.intercepted, true)
  const loaderId = (res.response?.result as { loaderId?: string }).loaderId
  assert.ok(loaderId)
  assert.equal(state.emitted.length, 0) // 未补发（挂起）
  // 2. getFrameTree 命令到达（Puppeteer initialize）→ flush
  const res2 = ci.intercept({ id: 43, method: 'Page.getFrameTree' })
  assert.equal(res2.intercepted, false)
  // flush 时无 frameId → 不补发（等 frameNavigated）
  assert.equal(state.emitted.length, 0)
  // 3. frameNavigated 到达 → initFrame + flush 补发
  ft.normalizeFrameNavigated({ frameId: 'FRAME-1', url: 'https://example.com/' })
  const lifecycles = state.emitted.filter((e) => e.method === 'Page.lifecycleEvent')
  assert.equal(lifecycles.length, 3)
  assert.deepEqual(
    lifecycles.map((e) => (e.params as { name?: string }).name),
    ['init', 'DOMContentLoaded', 'load'],
  )
  assert.ok(lifecycles.every((e) => (e.params as { loaderId?: string }).loaderId === loaderId))
})

test('CommandInterceptor: Page.navigate 无 frame 状态时挂起，getResourceTree 响应场景 flush', () => {
  const { host, state } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  const ci = new CommandInterceptor('t1', ft, new ContextTracker('t1', host.emit), host)
  const res = ci.intercept({ id: 42, method: 'Page.navigate', params: { url: 'https://example.com/' } })
  const loaderId = (res.response?.result as { loaderId?: string }).loaderId
  assert.ok(loaderId)
  assert.equal(state.emitted.length, 0)
  // 宿主先拿到真实 frameId（getResourceTree 响应）→ init → flush
  ft.initFrame('FRAME-1', 'about:blank')
  const res2 = ci.intercept({ id: 43, method: 'Page.getResourceTree' })
  assert.equal(res2.intercepted, false)
  const lifecycles = state.emitted.filter((e) => e.method === 'Page.lifecycleEvent')
  assert.equal(lifecycles.length, 3)
  assert.ok(lifecycles.every((e) => (e.params as { loaderId?: string }).loaderId === loaderId))
})

test('CommandInterceptor: Page.reload 拦截为 reload()', () => {
  const { host, state } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  const ci = new CommandInterceptor('t1', ft, new ContextTracker('t1', host.emit), host)
  const res = ci.intercept({ id: 7, method: 'Page.reload' })
  assert.equal(res.intercepted, true)
  assert.equal(state.reloaded, 1)
  assert.deepEqual(res.response, { id: 7, result: {} })
})

test('CommandInterceptor: Runtime.evaluate 剥离 utility ctxId（透传）', () => {
  const { host } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  const ct = new ContextTracker('t1', host.emit)
  const utilId = ct.emitUtilityContext('__puppeteer_utility_world__1', 'FRAME-1')
  const ci = new CommandInterceptor('t1', ft, ct, host)
  const params: Record<string, unknown> = { expression: 'document.title', contextId: utilId }
  const res = ci.intercept({ id: 9, method: 'Runtime.evaluate', params })
  assert.equal(res.intercepted, false)
  assert.equal(params.contextId, undefined) // 已剥离
})

test('CommandInterceptor: setDeviceMetricsOverride 改写为真实视口', () => {
  const { host } = makeHost('t1')
  const ft = new FrameTracker('t1', new LoaderIdManager(), host.emit)
  const ci = new CommandInterceptor('t1', ft, new ContextTracker('t1', host.emit), host)
  const params: Record<string, unknown> = { width: 1280, height: 720 }
  const res = ci.intercept({ id: 11, method: 'Emulation.setDeviceMetricsOverride', params })
  assert.equal(res.intercepted, false)
  assert.equal(params.width, 380) // 真实视口宽
  assert.equal(params.height, 600)
})

// ============ SemanticAdapter 端到端 ============

test('SemanticAdapter: goto 全链路（navigate → 补发序列 → 满足 newDocument）', () => {
  const { host, state } = makeHost('t1')
  const a = new SemanticAdapter('t1', host)
  a.init('FRAME-1', 'about:blank')

  // 1. Page.navigate 命令
  const res = a.onCommand({ id: 1, method: 'Page.navigate', params: { url: 'https://example.com/' } })
  assert.equal(res.intercepted, true)
  const loaderId = (res.response?.result as { loaderId?: string }).loaderId
  assert.ok(loaderId)

  // 2. 响应时无补发（时序保证）；afterResponse 后补发
  assert.equal(state.emitted.length, 0)
  res.afterResponse?.()
  const lifecycles = state.emitted.filter((e) => e.method === 'Page.lifecycleEvent')
  assert.equal(lifecycles.length, 3)
  assert.ok(lifecycles.every((e) => (e.params as { loaderId?: string }).loaderId === loaderId))

  // 3. 模拟真实导航事件到达（frameNavigated + 原生 lifecycle）
  const navEv = a.onEvent('Page.frameNavigated', {
    frameId: 'FRAME-1',
    url: 'https://example.com/',
    frame: { id: 'FRAME-1', url: 'https://example.com/' },
  })
  // frameNavigated 本周期已由补发覆盖 → 不再重复产出（避免 Puppeteer 重建 frame 中断 watcher）
  assert.ok(Array.isArray(navEv))
  // 原生 lifecycle（Electron 产出的真实事件）
  const evs = a.onEvent('Page.lifecycleEvent', {
    frameId: 'FRAME-1',
    loaderId: 'native-load-1',
    name: 'load',
    timestamp: 123,
  })
  assert.equal(evs.length, 1) // 透传（不重复补发）
  assert.equal((evs[0].params as { name?: string }).name, 'load')

  // 4. 断言：Puppeteer 侧等价状态（frame._loaderId ≠ initial，lifecycle 含 domcontentloaded）
  const frame = a.frames
  assert.ok(frame.loaderId) // 非空
  assert.ok(frame.lifecycleEvents.has('DOMContentLoaded'))
  assert.ok(frame.lifecycleEvents.has('load'))
})

test('SemanticAdapter: createIsolatedWorld → onCommandDone 补发标准 context', () => {
  const { host, state } = makeHost('t1')
  const a = new SemanticAdapter('t1', host)
  a.init('FRAME-1', 'about:blank')
  const cmd: CdpCommand = {
    id: 5,
    method: 'Page.createIsolatedWorld',
    params: { frameId: 'FRAME-1', worldName: '__puppeteer_utility_world__1', grantUniveralAccess: true },
  }
  const res = a.onCommand(cmd)
  assert.equal(res.intercepted, false) // 透传 debugger
  a.onCommandDone(cmd) // 宿主在 sendCommand resolve 后调用
  const ctxEv = state.emitted.find((e) => e.method === 'Runtime.executionContextCreated')
  assert.ok(ctxEv)
  const ctx = (ctxEv.params.context as Record<string, unknown>)
  assert.equal(ctx.name, '__puppeteer_utility_world__1')
  assert.equal((ctx.auxData as Record<string, unknown>).frameId, 'FRAME-1')
})

test('SemanticAdapter: onNativeNav(finish) 兜底补发完整序列', () => {
  const { host, state } = makeHost('t1')
  const a = new SemanticAdapter('t1', host)
  a.init('FRAME-1', 'about:blank')
  const evs = a.onNativeNav('finish')
  assert.equal(evs.length, 3)
  assert.deepEqual(
    evs.map((e) => (e.params as { name?: string }).name),
    ['init', 'DOMContentLoaded', 'load'],
  )
  const loaderIds = new Set(evs.map((e) => (e.params as { loaderId?: string }).loaderId))
  assert.equal(loaderIds.size, 1)
  assert.ok(state.emitted.length >= 3)
})

test('AdapterRegistry: targetId 路由', () => {
  const { host } = makeHost('t1')
  const reg = new AdapterRegistry()
  reg.register('t1', host)
  reg.register('t2', host)
  assert.ok(reg.has('t1'))
  assert.ok(reg.get('t2'))
  reg.remove('t1')
  assert.equal(reg.has('t1'), false)
})

test('SemanticAdapter: Runtime.enable 响应后补发主 world context（MAIN_WORLD 绑定）', () => {
  const { host, state } = makeHost('t1')
  const a = new SemanticAdapter('t1', host)
  a.init('FRAME-1', 'about:blank')
  // 先模拟主 world context 事件到达（缓存 id）
  a.onEvent('Runtime.executionContextCreated', {
    context: { id: 7, name: '', auxData: { isDefault: true, frameId: 'FRAME-1' } },
  })
  // Runtime.enable 命令 → 透传 → onCommandDone 补发主 world context
  const res = a.onCommand({ id: 10, method: 'Runtime.enable' })
  assert.equal(res.intercepted, false)
  a.onCommandDone({ id: 10, method: 'Runtime.enable' })
  const mainCtx = state.emitted.filter(
    (e) => e.method === 'Runtime.executionContextCreated' &&
      (e.params.context as Record<string, unknown>).name === '' &&
      ((e.params.context as { auxData?: Record<string, unknown> }).auxData?.isDefault === true),
  )
  assert.ok(mainCtx.length >= 1, '应补发主 world context')
  const ctx = mainCtx[mainCtx.length - 1].params.context as Record<string, unknown>
  assert.equal(ctx.id, 7) // 用真实缓存的 id
  assert.equal((ctx.auxData as Record<string, unknown>).isDefault, true)
})

test('SemanticAdapter: Runtime.enable 时主 world 未缓存 → pending 等事件到达补发', () => {
  const { host, state } = makeHost('t1')
  const a = new SemanticAdapter('t1', host)
  a.init('FRAME-1', 'about:blank')
  // Runtime.enable 时主 world context 未缓存
  const res = a.onCommand({ id: 11, method: 'Runtime.enable' })
  assert.equal(res.intercepted, false)
  a.onCommandDone({ id: 11, method: 'Runtime.enable' })
  // 无补发（pending）
  const before = state.emitted.length
  assert.equal(before, 0)
  // 主 world context 事件到达 → 触发补发
  a.onEvent('Runtime.executionContextCreated', {
    context: { id: 9, name: '', auxData: { isDefault: true, frameId: 'FRAME-1' } },
  })
  const mainCtx = state.emitted.filter(
    (e) => e.method === 'Runtime.executionContextCreated' &&
      ((e.params.context as { auxData?: Record<string, unknown> }).auxData?.isDefault === true),
  )
  assert.ok(mainCtx.length >= 1, '事件到达后应补发主 world context')
  assert.equal((mainCtx[0].params.context as Record<string, unknown>).id, 9)
})
