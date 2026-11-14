// 手机最小壳（P8-01a）：只验证「基础设施通了」——
//   · 凭据：localStorage 里的 token（旧页 key hai.token）+ 6 位码配对；
//   · 连接：GET /api/ping 自检 + Hub 事件流 WS 的实时状态；
//   · 事件：订阅 onEvent，显示帧数与最近一帧的 event_type（证明 WS 真的在投事件）。
//
// 刻意不做 Hub 列表/群/DM 屏幕（那是 P8-01b）：这里不 import useHubStore，
// 也不 import useAppStore —— 手机包一旦拉进桌面 store，复用边界就白划了。
import { useCallback, useEffect, useMemo, useState, type FormEvent } from 'react'
import { MobileHostPanelsProvider } from '../host/panels-mobile'
import { applyTheme, type ThemePref } from '../lib/theme'
import { sharedMobileTransport, type MobileLinkState, type MobileStatus, type MobileTransport } from './transport/mobile'
import { readThemePref, writeThemePref } from './theme'

type PingState = { state: 'idle' | 'checking' | 'ok' | 'fail'; paired?: boolean; error?: string }

const THEME_ORDER: ThemePref[] = ['system', 'dark', 'light']
const THEME_LABEL: Record<ThemePref, string> = { system: '跟随系统', dark: '深色', light: '浅色' }

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
  const [frames, setFrames] = useState(0)
  const [lastEvent, setLastEvent] = useState('')
  const [pairing, setPairing] = useState(() => !transport.token())
  const [theme, setTheme] = useState<ThemePref>(() => readThemePref())

  const check = useCallback(async () => {
    setPing({ state: 'checking' })
    const r = await transport.ping()
    if (!r.ok) setPing({ state: 'fail', error: r.error })
    else setPing({ state: 'ok', paired: r.data?.paired === true })
  }, [transport])

  useEffect(() => {
    const offStatus = transport.onStatus(setStatus)
    const offEvent = transport.onEvent((line) => {
      setFrames((n) => n + 1)
      try {
        const ev = JSON.parse(line) as { event_type?: string; type?: string }
        setLastEvent(ev.event_type || ev.type || '未知')
      } catch {
        setLastEvent('非 JSON 帧')
      }
    })
    void check()
    if (transport.token()) transport.connectHub()
    return () => {
      offStatus()
      offEvent()
      transport.disconnect()
    }
  }, [transport, check])

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

  const paired = token !== ''

  return (
    <MobileHostPanelsProvider>
      <div className="m-app" data-testid="mobile-shell">
        <header className="m-head">
          <div className="m-brand">
            <span className="m-brand-name">HAI</span>
            <span className="m-brand-tag">mobile</span>
          </div>
          <button
            type="button"
            className="m-icon-btn"
            onClick={cycleTheme}
            aria-label={`切换主题（当前：${THEME_LABEL[theme]}）`}
            data-testid="mobile-theme"
          >
            {theme === 'dark' ? '☾' : theme === 'light' ? '☀' : '◐'}
          </button>
        </header>

        <main className="m-main">
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
            <section className="m-card" aria-label="连接状态">
              <h1 className="m-title">已配对</h1>
              <p className="m-sub">手机端基础设施就绪。Hub 列表 / 群 / 私聊在 P8-01b 接入。</p>
              <dl className="m-rows">
                <Row label="服务地址" value={origin()} />
                <Row label="凭据" value="已保存（hai.token）" />
                <Row
                  label="Hub 事件流"
                  value={linkLabel(status.hub)}
                  tone={linkTone(status.hub)}
                  testId="mobile-hub-link"
                />
                <Row
                  label="已收事件"
                  value={frames === 0 ? '等待中' : `${frames} 帧 · 最近 ${lastEvent}`}
                  testId="mobile-frames"
                />
                <Row
                  label="凭据自检"
                  value={
                    ping.state === 'checking'
                      ? '检查中…'
                      : ping.state === 'fail'
                        ? `失败：${ping.error ?? '未知错误'}`
                        : ping.paired
                          ? '通过（/api/ping paired）'
                          : '凭据无效，请重新配对'
                  }
                  tone={ping.state === 'fail' ? 'bad' : ping.state === 'ok' && !ping.paired ? 'bad' : ping.state === 'ok' ? 'ok' : 'idle'}
                  testId="mobile-ping"
                />
              </dl>
              {status.lastError ? <p className="m-warn" data-testid="mobile-error">{status.lastError}</p> : null}
              <div className="m-actions">
                <button type="button" className="m-btn" onClick={reconnect} data-testid="mobile-reconnect">
                  重新连接
                </button>
                <button type="button" className="m-btn m-btn-ghost" onClick={() => setPairing(true)}>
                  重新配对
                </button>
              </div>
            </section>
          )}
        </main>

        <footer className="m-foot">
          <span>P8-01a · 基础设施</span>
          <span className="m-foot-dim">Hub 屏幕见 P8-01b</span>
        </footer>
      </div>
    </MobileHostPanelsProvider>
  )
}

function Row({ label, value, tone, testId }: { label: string; value: string; tone?: string; testId?: string }) {
  return (
    <div className="m-row" data-testid={testId}>
      <dt className="m-row-label">{label}</dt>
      <dd className={`m-row-value${tone ? ` m-tone-${tone}` : ''}`}>{value}</dd>
    </div>
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
