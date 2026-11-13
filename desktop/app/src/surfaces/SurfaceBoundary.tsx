// 每面板一个 ErrorBoundary（00 §4.3「错误边界包住」；实现参考 03-code-plan §2.9）
//
// 三条不可退让的约束：
//  1. **只包内容区**：标签条 / 收起钮 / ＋ 菜单在边界外面 —— 崩一个面板不能让人没法切走。
//  2. **按标签隔离**：key 由调用方挂（RightPanel 里 `key={tab.id}`），A 标签崩了切到 B
//     必须是一张新边界；否则 B 看到的是 A 的错误页（最容易漏的一条）。
//  3. **崩溃不白屏**：渲染 CrashCard（面板名 + err.message + 可折叠 stack + 重试 / 禁用）。
//
// 墓碑（unavailable）与崩溃卡放在同一个文件：两者都是「内容区的占位」，视觉与文案同源，
// 分成两个文件只会让「禁用后到底渲染哪个」变成两处判断。
import { Component } from 'react'
import type { ErrorInfo, ReactNode } from 'react'
import { useT } from '../i18n'
import { AUTO_DISABLE_AFTER, disableSurface, enableSurface, recordMountFailure, recordMountSuccess } from './health'
import type { SurfaceKind } from './types'

/** 墓碑原因：disabled = 被禁用（自动或手动）；missing = 插件 provider 没注册（插件缺失）。 */
export type UnavailableReason = 'disabled' | 'missing'

function CrashCard({ kind, title, err, onRetry, onDisable }: {
  kind: SurfaceKind
  title: string
  err: Error
  onRetry: () => void
  onDisable: () => void
}) {
  const t = useT()
  return (
    <div className="surface-crash" data-kind={kind} role="alert">
      <div className="surface-crash-head">
        <span className="surface-crash-badge">{t('rp.surface.crashBadge')}</span>
        <span className="surface-crash-title">{title}</span>
      </div>
      <div className="surface-crash-msg">{err.message}</div>
      <details className="surface-crash-details">
        <summary>{t('rp.surface.details')}</summary>
        <pre className="surface-crash-stack">{err.stack || String(err)}</pre>
      </details>
      <div className="surface-crash-actions">
        <button className="surface-crash-retry" onClick={onRetry}>{t('rp.surface.retry')}</button>
        <button className="surface-crash-disable" onClick={onDisable}>{t('rp.surface.disable')}</button>
      </div>
    </div>
  )
}

/**
 * 面板占位（墓碑）：provider 缺失或被禁用时**不渲染 provider**，只说明原因并给两个动作。
 * 「重新启用」走 enableSurface（同一份禁用集），「移除标签」走调用方的 closeTabNow。
 */
export function SurfaceTombstone({ kind, title, reason, auto, onRemove }: {
  kind: SurfaceKind
  title: string
  reason: UnavailableReason
  /** reason='disabled' 时：是否自动禁用（决定文案；手动禁用不写「连续 N 次」） */
  auto?: boolean
  onRemove: () => void
}) {
  const t = useT()
  const why = reason === 'missing'
    ? t('rp.surface.missing')
    : (auto ? t('rp.surface.autoDisabled') : t('rp.surface.manualDisabled')).replace('{n}', String(AUTO_DISABLE_AFTER))
  return (
    <div className={`surface-tombstone ${reason}`} data-kind={kind} data-reason={reason} role="status">
      <div className="surface-tombstone-title">{title}</div>
      <div className="surface-tombstone-why">{why}</div>
      <div className="surface-tombstone-actions">
        {reason === 'disabled' && (
          <button className="surface-tombstone-enable" onClick={() => enableSurface(kind)}>
            {t('rp.surface.enable')}
          </button>
        )}
        <button className="surface-tombstone-remove" onClick={onRemove}>{t('rp.surface.removeTab')}</button>
      </div>
    </div>
  )
}

interface Props {
  kind: SurfaceKind
  /** 面板名（与标签条同源，由 RightPanel 用 tabTitle 传入） */
  title: string
  children?: ReactNode
}

interface State {
  err: Error | null
}

export class SurfaceBoundary extends Component<Props, State> {
  state: State = { err: null }

  static getDerivedStateFromError(err: unknown): State {
    return { err: err instanceof Error ? err : new Error(String(err)) }
  }

  componentDidCatch(err: Error, _info: ErrorInfo) {
    // 计数与「3 次自动禁用」全在 health.ts —— 边界只上报事实，不做策略。
    recordMountFailure(this.props.kind, err)
  }

  componentDidMount() {
    // 走到这里 = children 完整渲染了一轮（render 期抛错时本组件的 didMount 不会执行）。
    if (!this.state.err) recordMountSuccess(this.props.kind)
  }

  componentDidUpdate() {
    // retry 后重新渲染成功也要归零：光靠 didMount 覆盖不到「同一个标签原地重试成功」。
    if (!this.state.err) recordMountSuccess(this.props.kind)
  }

  private retry = () => this.setState({ err: null })

  render() {
    if (this.state.err) {
      return (
        <CrashCard
          kind={this.props.kind}
          title={this.props.title}
          err={this.state.err}
          onRetry={this.retry}
          // 禁用后由 registry 的订阅推动 RightPanel 重渲染 → 这里整体换成墓碑。
          onDisable={() => disableSurface(this.props.kind)}
        />
      )
    }
    return this.props.children
  }
}
