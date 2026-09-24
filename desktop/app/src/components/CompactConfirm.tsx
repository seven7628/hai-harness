import { useAppStore, modelWindowOf } from '../store/useAppStore'
import { fmtTokens } from './ui'
import { useT } from '../i18n'

// 压缩确认弹窗：压缩为不可逆动作（ui-ux 高优：先确认再执行）。
// 显示当前占用 + 阈值建议（90% 琥珀建议 / 95% 绛红强烈建议）；确认 → compact 命令。
export default function CompactConfirm({ onClose }: { onClose: () => void }) {
  const t = useT()
  const ctxTokens = useAppStore((s) => s.ctxTokens) // 当前上下文占用（事件推导）
  const compact = useAppStore((s) => s.compact)
  const model = useAppStore((s) => s.model)
  const settings = useAppStore((s) => s.settings)
  const contextWindow = useAppStore((s) => s.contextWindow)
  const window = modelWindowOf(settings, model, contextWindow) // 分母优先 harness 上报（问题六）
  const ctxUsed = ctxTokens ?? 0
  const pct = Math.round((ctxUsed / window) * 100)
  const level = pct >= 95 ? 'red' : pct >= 90 ? 'amber' : 'hint'

  const confirm = () => {
    compact()
    onClose()
  }

  return (
    <div className="modal-hint open" role="dialog" aria-modal="true" aria-label={t('compact.title')}>
      <div className="card compact-card">
        <h3>
          <span style={{ color: 'var(--warning)' }}>✦</span> {t('compact.title')}
        </h3>
        <p className="compact-desc">
          {t('compact.descPre')}
          <b>{fmtTokens(ctxUsed)}/{fmtTokens(window)}</b>（{pct}%）
          {t('compact.descMid')}
          <b>{t('compact.irreversible')}</b>。
          {pct >= 90 && (
            <span className={`compact-${level}`}>{pct >= 95 ? t('compact.near95') : t('compact.near90')}</span>
          )}
        </p>
        <div className="acts">
          <button className="btn" onClick={onClose}>{t('compact.cancel')}</button>
          <button className={`btn primary ${level}`} onClick={confirm}>{t('compact.confirm')}</button>
        </div>
      </div>
    </div>
  )
}
