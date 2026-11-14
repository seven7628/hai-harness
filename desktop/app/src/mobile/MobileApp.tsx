// 手机壳（P8-01b）：配对 → Hub 列表 → DM/群会话（详情走右侧抽屉）。
//
// 与桌面 main.tsx 的边界（P8-01a 立的，这里继续守）：
//   · 不 import App / index.css / useAppStore / mock transport / electron；
//   · Hub 组件与 store 全部复用桌面那一份（HubSidebar / HubSurface / HubInspector），
//     手机只加布局与导航（screens/ 三个文件都只做壳）；
//   · transport 走 transport/host.ts 的注册（main.tsx 里 setHubTransport），
//     i18n 走 host/lang.tsx（这里把手机语言下发）。
//
// 导航模型：**Hub store 的 open + 选中目标就是「会话页」**。
//   · 点列表行 → store.openAgent/openGroup 置 open=true → 本壳渲染 ConversationScreen；
//   · 返回（44px 按钮 / Android 返回键 / Esc）→ closeHub() → open=false → 回列表。
// 这样搜索命中、深链、告警跳转等所有「打开 Hub」的入口在手机上自动落到会话页，
// 不需要在每个入口上再挂一遍导航回调。
import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react'
import type { HostLang } from '../host/lang'
import { MobileHostPanelsProvider } from '../host/panels-mobile'
import { attachHubEvents, useHubStore } from '../hub/useHubStore'
import { applyTheme, type ThemePref } from '../lib/theme'
import { readLangPref, writeLangPref } from './lang'
import { sharedMobileTransport, type MobileLinkState, type MobileStatus, type MobileTransport } from './transport/mobile'
import { readThemePref, writeThemePref } from './theme'

// Hub 屏幕按需加载（React.lazy）：它们会带进消息渲染（markdown/katex/shiki 的按需 chunk）
// 与 Hub 组件树 —— 配对屏没必要等这几百 KB。配对完成后才拉，手机上首屏更快。
const HubListScreen = lazy(() => import('./screens/HubListScreen'))
const ConversationScreen = lazy(() => import('./screens/ConversationScreen'))
const InspectorDrawer = lazy(() => import('./screens/InspectorDrawer'))

type PingState = { state: 'idle' | 'checking' | 'ok' | 'fail'; paired?: boolean; error?: string }

const THEME_ORDER: ThemePref[] = ['system', 'dark', 'light']
const THEME_LABEL: Record<ThemePref, string> = { system: '跟随系统', dark: '深色', light: '浅色' }
const TOAST_MS = 2800

function linkLabel(state: MobileLinkState): string {
  switch (state) {
    case 'open':
      return '已连接'
    case 'connecting':
      return '连接中'
    case 'closed':
      return '重连中'
    case 'unauthorized':
      return '凭据失效'
    default:
      return '未连接'
  }
}

function linkTone(state: MobileLinkState): string {
  if (state === 'open') return 'ok'
  if (state === 'connecting' || state === 'closed') return 'warn'
  if (state === 'unauthorized') return 'bad'
  return 'idle'
}

export default function MobileApp() {
  const transport = useMemo(() => sharedMobileTransport(), [])
  const [token, setToken] = useState(() => transport.token())
  const [status, setStatus] = useState<MobileStatus>(() => transport.status())
  const [ping, setPing] = useState<PingState>({ state: 'idle' })
  const [pairing, setPairing] = useState(() => !transport.token())
  const [theme, setTheme] = useState<ThemePref>(() => readThemePref())
  const [lang, setLang] = useState<HostLang>(() => readLangPref())
  const [toast, setToast] = useState<{ id: number; msg: string; kind: 'ok' | 'error' } | null>(null)

  const paired = token !== ''
  // 会话页 = Hub 已打开且选中了 DM/群（见文件头的导航模型）。
  const hubOpen = useHubStore((s) => s.open)
  const activeId = useHubStore((s) => s.activeId)
  const activeGroupId = useHubStore((s) => s.activeGroupId)
  const convOpen = paired && hubOpen && Boolean(activeId || activeGroupId)

  const check = useCallback(async () => {
    setPing({ state: 'checking' })
    const r = await transport.ping()
    if (!r.ok) {
      setPing({ state: 'fail', error: r.error })
      return
    }
    setPing({ state: 'ok', paired: r.data?.paired === true })
    // 带着 token 却被告知「未配对」= 凭据失效：直接回到配对屏，别让用户对着空列表发呆。
    if (r.data?.paired === false && transport.token()) setPairing(true)
  }, [transport])

  // 连接状态 + 首次自检。Hub 事件流由 sharedMobileTransport().connectHub() 维持，
  // 与 Hub store 的订阅（下面的 attachHubEvents）共用同一条 WS。
  useEffect(() => {
    const offStatus = transport.onStatus(setStatus)
    void check()
    if (transport.token()) transport.connectHub()
    return () => {
      offStatus()
      transport.disconnect()
    }
  }, [transport, check])

  // Hub 事件订阅：与桌面 App.tsx 同一个入口（attachHubEvents）。配对后才挂，
  // 卸载/重新配对时解绑 —— 否则 StrictMode 下会叠出两条订阅、每帧处理两遍。
  useEffect(() => {
    if (!paired) return
    return attachHubEvents()
  }, [paired])

  // 列表数据：桌面左栏常驻时由 HubSidebar 自己拉；手机这里再拉一次是为了
  // 「配对后立刻有内容」，不必等列表挂载的 effect（幂等，代价一条命令）。
  useEffect(() => {
    if (!paired) return
    const st = useHubStore.getState()
    void st.refresh()
    void st.loadGroups()
  }, [paired])

  // Android 返回键 / 浏览器后退：会话页压一条 history，popstate 回列表。
  // 用 ref 记住「这条记录是我们压的」，应用内返回按钮走 history.back() 消费它，
  // 避免栈里留下一条永远退不掉的记录。
  const pushedRef = useRef(false)
  useEffect(() => {
    if (!convOpen) return
    const prev = window.history.state
    window.history.pushState({ ...(prev && typeof prev === 'object' ? prev : {}), haiHubConv: true }, '')
    pushedRef.current = true
    const onPop = (): void => {
      pushedRef.current = false
      useHubStore.getState().closeHub()
    }
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [convOpen])

  const back = useCallback(() => {
    if (pushedRef.current) {
      window.history.back() // 触发 popstate → closeHub
      return
    }
    useHubStore.getState().closeHub()
  }, [])

  // Esc 回列表（桌面 Hub 的 Esc 只关菜单/弹层，见 useHeaderMenu；这里是手机导航）。
  // 抽屉自己的 Esc 在捕获阶段 stopPropagation，先关抽屉。
  useEffect(() => {
    if (!convOpen) return
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') back()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [convOpen, back])

  useEffect(() => {
    if (!toast) return
    const timer = window.setTimeout(() => setToast(null), TOAST_MS)
    return () => window.clearTimeout(timer)
  }, [toast])

  // 语言：写入手机偏好 + <html lang>（读屏/断行用），i18n 通过 host/lang 读同一个值。
  useEffect(() => {
    writeLangPref(lang)
    document.documentElement.lang = lang === 'en' ? 'en' : 'zh-CN'
  }, [lang])

  const cycleTheme = (): void => {
    const next = THEME_ORDER[(THEME_ORDER.indexOf(theme) + 1) % THEME_ORDER.length]!
    setTheme(next)
    writeThemePref(next)
    applyTheme(next)
  }

  const reconnect = (): void => {
    transport.disconnect()
    transport.connectHub()
    void check()
  }

  const showToast = useCallback((msg: string, kind: 'ok' | 'error') => {
    setToast({ id: Date.now(), msg, kind })
  }, [])

  return (
    <MobileHostPanelsProvider lang={lang} onToast={showToast}>
      <div className={`m-app${convOpen ? ' m-app--conv' : ''}`} data-testid="mobile-shell">
        {convOpen ? null : (
          <header className="m-head">
            <div className="m-brand">
              <span className="m-brand-name">HAI</span>
              <span className="m-brand-tag">hub</span>
            </div>
            <div className="m-head-actions">
              <button
                type="button"
                className="m-icon-btn"
                onClick={() => setLang(lang === 'en' ? 'zh' : 'en')}
                aria-label={`切换语言（当前：${lang === 'en' ? 'English' : '中文'}）`}
                data-testid="mobile-lang"
              >
                {lang === 'en' ? 'EN' : '中'}
              </button>
              <button
                type="button"
                className="m-icon-btn"
                onClick={cycleTheme}
                aria-label={`切换主题（当前：${THEME_LABEL[theme]}）`}
                data-testid="mobile-theme"
              >
                {theme === 'dark' ? '☾' : theme === 'light' ? '☀' : '◐'}
              </button>
            </div>
          </header>
        )}

        <main className={convOpen ? 'm-main m-main--flush' : 'm-main'}>
          {pairing || !paired ? (
            <PairCard
              transport={transport}
              initialName={readDeviceName()}
              onPaired={async () => {
                setToken(transport.token())
                setPairing(false)
                await check()
                transport.connectHub()
              }}
            />
          ) : (
            <Suspense fallback={<p className="m-loading" role="status">加载 Hub…</p>}>
              {convOpen ? <ConversationScreen onBack={back} /> : <HubListScreen hubLink={status.hub} />}
            </Suspense>
          )}
        </main>

        {pairing || !paired || convOpen ? null : (
          <footer className="m-foot">
            <span className={`m-link m-tone-${linkTone(status.hub)}`} data-testid="mobile-hub-link">
              <span className="m-link-dot" aria-hidden="true" />
              {linkLabel(status.hub)}
            </span>
            <span className="m-foot-dim m-foot-origin" title={origin()}>{origin()}</span>
            <span className="m-foot-actions">
              <button type="button" className="m-foot-btn" onClick={reconnect} data-testid="mobile-reconnect">
                重连
              </button>
              <button type="button" className="m-foot-btn" onClick={() => setPairing(true)} data-testid="mobile-repair">
                重新配对
              </button>
            </span>
          </footer>
        )}

        {ping.state === 'fail' && !pairing ? (
          <p className="m-warn m-shell-warn" role="alert" data-testid="mobile-error">
            凭据自检失败：{ping.error ?? '未知错误'}
          </p>
        ) : null}
        {status.lastError && !pairing ? (
          <p className="m-warn m-shell-warn" role="alert" data-testid="mobile-link-error">{status.lastError}</p>
        ) : null}

        {convOpen ? (
          <Suspense fallback={null}>
            <InspectorDrawer />
          </Suspense>
        ) : null}

        {toast ? (
          <div className={`m-toast m-toast-${toast.kind}`} role="status" data-testid="mobile-toast">
            {toast.msg}
          </div>
        ) : null}
      </div>
    </MobileHostPanelsProvider>
  )
}

function PairCard({
  transport,
  initialName,
  onPaired,
}: {
  transport: MobileTransport
  initialName: string
  onPaired: () => Promise<void>
}) {
  const [code, setCode] = useState('')
  const [name, setName] = useState(initialName)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const submit = async (e: FormEvent): Promise<void> => {
    e.preventDefault()
    setError('')
    if (!/^\d{6}$/.test(code.trim())) {
      setError('请输入 6 位配对码')
      return
    }
    setBusy(true)
    const deviceName = name.trim() || '手机'
    const r = await transport.pair(code.trim(), deviceName)
    setBusy(false)
    if (!r.ok) {
      setError(r.error || '配对失败')
      return
    }
    try {
      localStorage.setItem('hai.name', deviceName) // 旧页同 key：下次预填设备名
    } catch {
      /* 隐私模式忽略 */
    }
    await onPaired()
  }

  return (
    <form className="m-card" onSubmit={(e) => void submit(e)} aria-label="设备配对">
      <h1 className="m-title">配对这台设备</h1>
      <p className="m-sub">在桌面端打开手机访问页，输入那里显示的 6 位配对码。</p>
      <label className="m-field">
        <span className="m-field-label">配对码</span>
        <input
          className="m-input"
          value={code}
          onChange={(e) => setCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
          inputMode="numeric"
          autoComplete="one-time-code"
          placeholder="123456"
          data-testid="mobile-pair-code"
        />
      </label>
      <label className="m-field">
        <span className="m-field-label">设备名</span>
        <input
          className="m-input"
          value={name}
          onChange={(e) => setName(e.target.value)}
          maxLength={32}
          placeholder="手机"
          data-testid="mobile-pair-name"
        />
      </label>
      {error ? <p className="m-warn" data-testid="mobile-pair-error">{error}</p> : null}
      <div className="m-actions">
        <button type="submit" className="m-btn" disabled={busy} data-testid="mobile-pair-submit">
          {busy ? '配对中…' : '配对'}
        </button>
      </div>
    </form>
  )
}

function origin(): string {
  try {
    return window.location.origin
  } catch {
    return '未知'
  }
}

// 设备名沿用旧页的 key（app.js LS.name = 'hai.name'）：配对过就不用每次重填。
function readDeviceName(): string {
  try {
    return localStorage.getItem('hai.name') || '手机'
  } catch {
    return '手机'
  }
}
