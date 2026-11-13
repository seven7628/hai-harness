import { useT } from '../i18n'

// Hub 的错误横幅（DM 与群共用）：重试 + 关闭。
//
// 之前群里发送失败只有一条「网络中断 ×」—— 没有重试入口，失败气泡又永久留在时间线，
// 用户只能重新敲一遍（还会叠出第二条乐观气泡，P4 复审 HUB-FAIL-01）。重试走 store 的
// retry()：它沿用首发 client_msg_id，先把失败气泡摘掉再重发（HUB-IDEM-01）。
export default function HubErrorBanner({
  message,
  canRetry,
  busy,
  onRetry,
  onDismiss,
}: {
  message: string
  /** 只有「当前目标的失败记录」才给重试，避免把消息发到别的会话。 */
  canRetry: boolean
  busy?: boolean
  onRetry: () => void
  onDismiss: () => void
}) {
  const t = useT()
  return (
    <div className="hub-error hub-error-row hub-center-banner" role="alert">
      <span>{message}</span>
      {canRetry ? (
        <button className="hub-link" onClick={onRetry} disabled={busy}>
          {t('hub.retry')}
        </button>
      ) : null}
      <button className="hub-link" onClick={onDismiss} aria-label={t('hub.dismiss')}>×</button>
    </div>
  )
}
