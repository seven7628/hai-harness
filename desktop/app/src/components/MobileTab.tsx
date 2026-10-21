import { useEffect, useState } from 'react'
import { CopyIcon, RefreshIcon, Smartphone, TrashIcon } from './icons'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'

// 「手机连接」设置页：拿配对码 + 看已配对设备 + 撤销。
//
// 为什么配对码要放在桌面端：手机端只能证明「我知道码」，不能自己发码。
// 码 5 分钟过期、单次使用；被用掉后这里重新拉一次 mobile_status 就会出新码。
export default function MobileTab() {
  const t = useT()
  const status = useAppStore((s) => s.mobileStatus)
  const fetchMobileStatus = useAppStore((s) => s.fetchMobileStatus)
  const revoke = useAppStore((s) => s.revokeMobileDevice)
  const setMobileLan = useAppStore((s) => s.setMobileLan)
  const [copied, setCopied] = useState<'url' | 'code' | null>(null)

  useEffect(() => {
    fetchMobileStatus()
  }, [fetchMobileStatus])

  const copy = async (kind: 'url' | 'code', text: string) => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(kind)
      setTimeout(() => setCopied(null), 1500)
    } catch {
      /* 剪贴板不可用（无权限/非安全上下文）：按钮不反馈，信息本身仍可手抄 */
    }
  }

  if (!status) return <div className="set-desc">{t('mobile.loading')}</div>
  if (!status.enabled) return <div className="set-desc">{t('mobile.disabled')}</div>

  const urls = status.urls && status.urls.length ? status.urls : status.addr ? [`http://${status.addr}/m/`] : []
  const devices = status.devices ?? []

  return (
    <div>
      <div className="settings-sec-title">
        <Smartphone size={14} /> {t('mobile.title')}
      </div>
      <p className="set-desc">{t('mobile.hint')}</p>

      <label className="set-row" style={{ gap: 8, cursor: 'pointer' }}>
        <input type="checkbox" checked={!!status.lan} onChange={(e) => setMobileLan(e.target.checked)} />
        <span className="set-label">{t('mobile.lan')}</span>
      </label>
      <p className="set-desc">{t('mobile.lanHint')}</p>

      <div className="settings-sec-title" style={{ marginTop: 12 }}>
        {t('mobile.addr')}
      </div>
      {urls.length === 0 ? (
        <p className="set-desc">—</p>
      ) : (
        urls.map((u) => (
          <div className="set-row" key={u}>
            <code className="text-input" style={{ flex: 1, userSelect: 'all' }}>
              {u}
            </code>
            <button className="btn" onClick={() => copy('url', u)}>
              <CopyIcon size={13} /> {copied === 'url' ? t('mobile.copied') : t('mobile.copy')}
            </button>
          </div>
        ))
      )}

      <div className="set-row">
        <div className="set-label">{t('mobile.code')}</div>
        <span className="mesh-code" style={{ fontSize: 22, letterSpacing: 6 }}>
          {status.code || '------'}
        </span>
        <button className="btn" onClick={() => copy('code', status.code ?? '')} disabled={!status.code}>
          <CopyIcon size={13} /> {copied === 'code' ? t('mobile.copied') : t('mobile.copy')}
        </button>
        <button className="btn" title={t('mobile.refresh')} onClick={() => fetchMobileStatus()}>
          <RefreshIcon size={13} />
        </button>
      </div>
      <p className="set-desc">{t('mobile.codeHint')}</p>

      <div className="settings-sec-title" style={{ marginTop: 18 }}>
        {t('mobile.devices')}
      </div>
      {devices.length === 0 ? (
        <p className="set-desc">{t('mobile.noDevices')}</p>
      ) : (
        devices.map((d) => (
          <div className="set-row" key={d.id}>
            <div className="set-label">{d.name || d.id}</div>
            <span className="set-desc" style={{ flex: 1 }}>
              {t('mobile.pairedAt')} {new Date(d.created_at * 1000).toLocaleDateString()}
              {d.last_seen ? ` · ${t('mobile.lastSeen')} ${new Date(d.last_seen * 1000).toLocaleTimeString()}` : ''}
            </span>
            <button className="btn" onClick={() => revoke(d.id)}>
              <TrashIcon size={13} /> {t('mobile.revoke')}
            </button>
          </div>
        ))
      )}

      <p className="set-desc" style={{ marginTop: 14 }}>
        {t('mobile.security')}
      </p>
    </div>
  )
}
