import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'

// bridge 异常退出的常驻提示条。
//
// 为什么不能只靠 Statusbar / toast：
//   · Statusbar 只显示 bridgeStatus 字符串（"error"），error 文案只有 BootScreen 读；
//   · toast 会自动消失；
//   · 命令在 bridge 死后的失败旧实现是静默吞掉的 —— 用户看到的是「没有报错，但点了没反应」。
// 这条横幅把「bridge 死了」变成可操作状态：重启 bridge / 刷新界面。
export default function BridgeBanner() {
  const status = useAppStore((s) => s.bridgeStatus)
  const error = useAppStore((s) => s.bridgeError)
  const restartBridge = useAppStore((s) => s.restartBridge)
  const t = useT()
  if (status !== 'error') return null
  return (
    <div
      role="alert"
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 8,
        flex: '0 0 auto',
        padding: '6px 12px',
        fontSize: 12,
        background: 'var(--surface-2, #2b2b2b)',
        borderBottom: '1px solid var(--line, #444)',
        color: 'var(--text, #ddd)',
      }}
    >
      <span
        aria-hidden
        style={{ width: 8, height: 8, borderRadius: '50%', background: 'var(--danger, #e5484d)', flex: '0 0 auto' }}
      />
      <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
        {t('bridge.exited')}
        {error ? `: ${error}` : ''}
      </span>
      <button type="button" className="btn" onClick={restartBridge}>{t('bridge.restart')}</button>
      <button type="button" className="btn" onClick={() => window.location.reload()}>{t('bridge.reload')}</button>
    </div>
  )
}
