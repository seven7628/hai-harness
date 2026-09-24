import { useEffect, useState } from 'react'
import { useAppStore, type BrowserCfg } from '../store/useAppStore'
import { useT } from '../i18n'
import Sel from './Sel'

// 浏览器插件设置编辑器（插件卡内联展开）。分组：登录/会话、浏览器基础、隐私/安全、高级（折叠）。
// 数据源 = browser_config_get（有效配置，含默认）；保存 = browser_config_set（bridge 校验 +
// 落盘 config.json + 热应用）。防乱调：选项优先（seg/select/switch），前端轻校验 + bridge 兜底。

const VIEWPORT_PRESETS = ['1280x720', '1920x1080', '1440x900', '1024x768', '375x812']
const CHANNELS = ['', 'stable', 'canary', 'dev', 'beta'] as const
const SESSIONS = ['persistent', 'isolated', 'existing'] as const
const PRESENTATIONS = ['embedded', 'window'] as const
const URL_MODES = ['none', 'block', 'allow'] as const

const BOOLS = ['headless', 'redact_network_headers', 'usage_statistics', 'accept_insecure_certs'] as const
type BoolKey = (typeof BOOLS)[number]

export function BrowserSettingsEditor({ onClose }: { onClose: () => void }) {
  const t = useT()
  const cfg = useAppStore((s) => s.browserConfig)
  const fetchBrowserConfig = useAppStore((s) => s.fetchBrowserConfig)
  const saveBrowserConfig = useAppStore((s) => s.saveBrowserConfig)
  const [draft, setDraft] = useState<BrowserCfg | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [advanced, setAdvanced] = useState(false)
  const [loadFailed, setLoadFailed] = useState(false)
  const [attempt, setAttempt] = useState(0) // 重试计数（触发重新拉取）

  // —— 登录同步（一键导入 Chrome 登录状态；用户只需点一个按钮，无其它交互）——
  const [cookieBusy, setCookieBusy] = useState(false)
  const [cookieMsg, setCookieMsg] = useState<{ ok: boolean; text: string } | null>(null)

  // 打开即取有效配置（尚未取到则触发一次）；取到后填本地草稿
  useEffect(() => {
    if (!cfg) {
      fetchBrowserConfig()
      return
    }
    setDraft((d) => d ?? { ...cfg })
  }, [cfg, fetchBrowserConfig, attempt])

  // 防卡死：拉取超时（8s）仍未取到 → 显示重试，而非无限「加载中」
  useEffect(() => {
    if (draft || cfg) return
    const timer = window.setTimeout(() => setLoadFailed(true), 8000)
    return () => window.clearTimeout(timer)
  }, [draft, cfg])

  if (!draft) {
    return (
      <div className="mcp-form">
        {loadFailed ? (
          <>
            <p className="set-placeholder">{t('plugin.browser.loadFailed')}</p>
            <button
              className="btn"
              onClick={() => {
                setLoadFailed(false)
                setAttempt((n) => n + 1) // 重拉：effect 依赖 attempt 触发 fetchBrowserConfig
              }}
            >
              {t('common.retry')}
            </button>
          </>
        ) : (
          <p className="set-placeholder">{t('plugin.browser.loading')}</p>
        )}
      </div>
    )
  }

  const set = <K extends keyof BrowserCfg>(k: K, v: BrowserCfg[K]) => setDraft((d) => (d ? { ...d, [k]: v } : d))
  const boolToggle = (k: BoolKey) => (
    <button
      className={`switch ${draft[k] ? 'on' : ''}`}
      role="switch"
      aria-checked={draft[k]}
      onClick={() => set(k, !draft[k])}
    />
  )

  const save = async () => {
    setErr(null)
    if (!/^\d{2,5}x\d{2,5}$/.test(draft.viewport)) {
      setErr(t('plugin.browser.err.viewport'))
      return
    }
    if (draft.url_mode !== 'none' && draft.url_patterns.length === 0) {
      setErr(t('plugin.browser.err.urlEmpty'))
      return
    }
    setBusy(true)
    const r = await saveBrowserConfig(draft)
    setBusy(false)
    if (!r.ok) {
      setErr(r.error ?? t('plugin.browser.err.save'))
      return
    }
    onClose()
  }

  // —— 登录同步：一键导入用户 Chrome 登录状态（值不透明：只提示成功/失败，不展示任何细节）——
  const runChromeImport = async () => {
    setCookieMsg(null)
    if (!window.desktop) {
      setCookieMsg({ ok: false, text: t('plugin.browser.cookie.noDesktop') })
      return
    }
    // 会自动退出并重开 Chrome，先让用户知情确认
    if (!window.confirm(t('plugin.browser.cookie.confirmQuit'))) return
    setCookieBusy(true)
    try {
      const r = await window.desktop.chromeImportLogin()
      if (!r.ok) {
        setCookieMsg({
          ok: false,
          text: r.chromeRunning ? t('plugin.browser.cookie.chromeRunning') : (r.error ?? t('plugin.browser.cookie.importFail')),
        })
        return
      }
      setCookieMsg({ ok: true, text: t('plugin.browser.cookie.chromeImported') })
    } finally {
      setCookieBusy(false)
    }
  }

  return (
    <div className="mcp-form">
      {err && <p className="nwf-err" role="alert">{err}</p>}

      {/* 登录同步：一键导入 Chrome 登录状态（用户主动授权；只提示成功/失败） */}
      <div className="set-group">
        <div className="set-label">{t('plugin.browser.cookie.title')}</div>
        <p className="set-desc">{t('plugin.browser.cookie.hint')}</p>
        <p className="set-desc set-warn">{t('plugin.browser.cookie.secure')}</p>

        <div className="set-row">
          <button className="btn primary" onClick={runChromeImport} disabled={cookieBusy}>
            {cookieBusy ? '…' : t('plugin.browser.cookie.oneClick')}
          </button>
        </div>

        {cookieMsg && (
          <p className={`set-desc ${cookieMsg.ok ? '' : 'nwf-err'}`} role="status">
            {cookieMsg.text}
          </p>
        )}
      </div>

      {/* 登录/会话 */}
      <div className="set-row">
        <div className="set-label">{t('plugin.browser.session')}</div>
        <div className="seg" role="radiogroup" aria-label={t('plugin.browser.session')}>
          {SESSIONS.map((m) => (
            <button
              key={m}
              className={draft.session_mode === m ? 'cur' : ''}
              aria-pressed={draft.session_mode === m}
              onClick={() => set('session_mode', m)}
            >
              {t(`plugin.browser.session.${m}`)}
            </button>
          ))}
        </div>
      </div>
      <p className="set-desc">{t(`plugin.browser.session.${draft.session_mode}.hint`)}</p>

      {/* 渲染位置（presentation） */}
      <div className="set-row">
        <div className="set-label">{t('plugin.browser.presentation')}</div>
        <div className="seg" role="radiogroup" aria-label={t('plugin.browser.presentation')}>
          {PRESENTATIONS.map((m) => (
            <button
              key={m}
              className={draft.presentation === m ? 'cur' : ''}
              aria-pressed={draft.presentation === m}
              onClick={() => set('presentation', m)}
            >
              {t(`plugin.browser.presentation.${m}`)}
            </button>
          ))}
        </div>
      </div>
      <p className="set-desc">{t(`plugin.browser.presentation.${draft.presentation}.hint`)}</p>

      {/* 浏览器基础 */}
      <div className="set-row">
        <div className="set-label">{t('plugin.browser.headless')}</div>
        {boolToggle('headless')}
      </div>
      <div className="set-row">
        <div className="set-label">{t('plugin.browser.viewport')}</div>
        <div className="seg" role="radiogroup" aria-label={t('plugin.browser.viewport')}>
          {VIEWPORT_PRESETS.map((v) => (
            <button
              key={v}
              className={draft.viewport === v ? 'cur' : ''}
              aria-pressed={draft.viewport === v}
              onClick={() => set('viewport', v)}
            >
              {v}
            </button>
          ))}
        </div>
      </div>
      <div className="set-row">
        <div className="set-label">{t('plugin.browser.channel')}</div>
        <Sel
          value={draft.channel}
          options={CHANNELS.map((c) => ({
            value: c,
            label: t(c ? `plugin.browser.channel.${c}` : 'plugin.browser.channel.default'),
          }))}
          onChange={(v) => set('channel', v as BrowserCfg['channel'])}
          style={{ flex: 1 }}
        />
      </div>

      {/* 隐私/安全 */}
      <div className="set-row">
        <div className="set-label">{t('plugin.browser.redact')}</div>
        {boolToggle('redact_network_headers')}
      </div>
      <div className="set-row">
        <div className="set-label">{t('plugin.browser.usage')}</div>
        {boolToggle('usage_statistics')}
      </div>
      <div className="set-row">
        <div className="set-label">{t('plugin.browser.urlMode')}</div>
        <div className="seg" role="radiogroup" aria-label={t('plugin.browser.urlMode')}>
          {URL_MODES.map((m) => (
            <button
              key={m}
              className={draft.url_mode === m ? 'cur' : ''}
              aria-pressed={draft.url_mode === m}
              onClick={() => set('url_mode', m)}
            >
              {t(`plugin.browser.urlMode.${m}`)}
            </button>
          ))}
        </div>
      </div>
      {draft.url_mode !== 'none' && (
        <div className="set-row">
          <div className="set-label">{t('plugin.browser.urlPatterns')}</div>
          <textarea
            className="prov-input"
            rows={4}
            value={draft.url_patterns.join('\n')}
            onChange={(e) => set('url_patterns', e.target.value.split('\n').map((s) => s.trim()).filter(Boolean))}
            aria-label={t('plugin.browser.urlPatterns')}
            placeholder="https://example.com/*"
            spellCheck={false}
          />
        </div>
      )}

      {/* 高级（折叠） */}
      <button className="btn" onClick={() => setAdvanced((v) => !v)} aria-expanded={advanced}>
        {advanced ? '▾ ' : '▸ '}
        {t('plugin.browser.advanced')}
      </button>
      {advanced && (
        <>
          <div className="set-row">
            <div className="set-label">{t('plugin.browser.acceptInsecure')}</div>
            {boolToggle('accept_insecure_certs')}
          </div>
          {draft.accept_insecure_certs && <p className="mcp-s-err">{t('plugin.browser.acceptInsecure.warn')}</p>}
          <div className="set-row">
            <div className="set-label">{t('plugin.browser.userDataDir')}</div>
            <input
              className="prov-input"
              value={draft.user_data_dir ?? ''}
              onChange={(e) => set('user_data_dir', e.target.value)}
              aria-label={t('plugin.browser.userDataDir')}
              placeholder={t('plugin.browser.userDataDir.placeholder')}
              spellCheck={false}
            />
          </div>
        </>
      )}

      <div className="mcp-form-btns">
        <button className="btn primary" onClick={save} disabled={busy}>
          {busy ? '…' : t('plugin.browser.save')}
        </button>
        <button className="btn" onClick={onClose} disabled={busy}>
          {t('plugin.browser.cancel')}
        </button>
      </div>
    </div>
  )
}
