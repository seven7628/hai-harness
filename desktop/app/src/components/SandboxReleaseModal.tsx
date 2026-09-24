import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'

// 沙箱拦截放行 modal（macOS Seatbelt / sandbox-exec 拦截 EPERM/deny 时弹出）：
// 与 ApprovalModal 同构的 scrim/modal。
// 「允许一次」= 该命令绕过沙箱在本机直接重跑一次；「拒绝」= 命令保留拦截文本不重跑。
// 不可取消：bash 工具调用在等待决策（阻塞 agent 循环），超时（60s）自动拒绝。
export default function SandboxReleaseModal() {
  const t = useT()
  const release = useAppStore((s) => s.release)
  const releaseSandbox = useAppStore((s) => s.releaseSandbox)
  if (!release) return null
  return (
    <div className="modal-hint open" role="dialog" aria-modal="true" aria-label={t('sandbox.dialogAria')}>
      <div className="card">
        <h3>
          <span style={{ color: 'var(--warning)' }}>🛡</span> {t('sandbox.title')}
        </h3>
        <p className="set-desc">{t('sandbox.desc')}</p>
        <div className="mono sbx-hint" title={release.hint}>{release.hint}</div>
        <p className="set-desc">
          {t('sandbox.hint')}
        </p>
        <div className="acts">
          <button className="btn" onClick={() => releaseSandbox(false)}>{t('sandbox.reject')}</button>
          <button className="btn primary" onClick={() => releaseSandbox(true)}>{t('sandbox.allowOnce')}</button>
        </div>
      </div>
    </div>
  )
}