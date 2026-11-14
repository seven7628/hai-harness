// 每面板一个 ErrorBoundary（00 §4.3「错误边界包住」；实现参考 03-code-plan §2.9）
//
// 三条不可退让的约束：
//  1. **只包内容区**：标签条 / 收起钮 / ＋ 菜单在边界外面 —— 崩一个面板不能让人没法切走。
//  2. **按标签隔离**：A 标签崩了切到 B，B 不能看到 A 的错误页（最容易漏的一条）。
//     手段是 **tabId prop 变化时复位错误态**，不是给边界挂 key —— 见 componentDidUpdate 的注释。
//  3. **崩溃不白屏**：渲染 CrashCard（面板名 + err.message + 可折叠 stack + 重试 / 禁用）。
//
// 墓碑（unavailable）与崩溃卡放在同一个文件：两者都是「内容区的占位」，视觉与文案同源，
// 分成两个文件只会让「禁用后到底渲染哪个」变成两处判断。
import { Component, useState } from 'react'
import type { ErrorInfo, ReactNode } from 'react'
import { useT } from '../i18n'
import { AUTO_DISABLE_AFTER, disableSurface, enableSurface, recordMountFailure, recordMountSuccess } from './health'
import { pluginIDOf, setSurfaceEnabled, surfaceReasonText } from './manage'
import type { SurfaceKind } from './types'
import { bridge } from '../lib/bridge'

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
 * 「移除标签」走调用方的 closeTabNow。
 *
 * 「重新启用」有两条路（PR-14b），按 kind 分：
 *   · 内置 kind：只清 health 的本地禁用集（enableSurface）—— 它没有磁盘上的启用位；
 *   · `plugin:<id>`：**先**让桥把 index 里的 enabled 翻回 true（surfaceSetEnabled），
 *     **再**清本地禁用集。顺序不能反：index 还写着 false 时 loader 不会注册 provider，
 *     本地先清掉只会得到一个「灰着但点了没反应」的墓碑 —— 而桥写成功会推
 *     surfaces_changed，loader 下一轮就把 provider 注册回来，标签自己复活。
 *     失败（unknown_id / bad_request / error）就在墓碑里说一句，不弹 alert。
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
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const why = reason === 'missing'
    ? t('rp.surface.missing')
    : (auto ? t('rp.surface.autoDisabled') : t('rp.surface.manualDisabled')).replace('{n}', String(AUTO_DISABLE_AFTER))
  const enable = async () => {
    const pluginId = pluginIDOf(kind)
    setErr('')
    // 没有 IPC 时（浏览器里 npm run dev / 老 preload）退回 PR-10 的行为：这时本地禁用集
    // 是**唯一**能解的那一份，拿它去要求磁盘那一份等于让按钮永久失效。
    if (!pluginId || !bridge?.surfaceSetEnabled) {
      enableSurface(kind) // 内置 kind：与 PR-10 一样，只有本地禁用集
      return
    }
    setBusy(true)
    try {
      const r = await setSurfaceEnabled(pluginId, true)
      if (!r.ok) {
        setErr(surfaceReasonText(t, r.reason))
        return
      }
      enableSurface(kind) // index 已同意 → 再清本地禁用集（顺序见文件头的注释）
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className={`surface-tombstone ${reason}`} data-kind={kind} data-reason={reason} role="status">
      <div className="surface-tombstone-title">{title}</div>
      <div className="surface-tombstone-why">{why}</div>
      <div className="surface-tombstone-actions">
        {reason === 'disabled' && (
          <button className="surface-tombstone-enable" disabled={busy} onClick={() => void enable()}>
            {t('rp.surface.enable')}
          </button>
        )}
        <button className="surface-tombstone-remove" onClick={onRemove}>{t('rp.surface.removeTab')}</button>
      </div>
      {err && <div className="surface-tombstone-why" data-tombstone-error>{err}</div>}
    </div>
  )
}

interface Props {
  kind: SurfaceKind
  /**
   * 这个边界当前服务的标签 id。**换了标签就把错误态复位**（componentDidUpdate）。
   *
   * 为什么不是 `key={tabId}`：key 一变 React 会把**整棵 children 重挂** —— 切 web 标签时
   * DOM 容器跟着重建，R06c 的「切标签原地复用」（原生视图与页面状态靠的就是容器不重挂）
   * 当场破功。tabId 变化只清错误态，children 是原地更新成新标签的内容，组件实例不换。
   */
  tabId: string
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

  componentDidUpdate(prevProps: Props, prevState: State) {
    // 换标签 ⇒ **上一条标签留下的**错误页必须让位（否则 B 看到的是 A 的崩溃原因）。
    // 只清 state.err：不换 key、不重挂 children —— 见 Props.tabId 的注释（R06c）。
    //
    // 判据必须是 `prevState.err`（这次更新**之前**就已经在错误态），不能只看 `this.state.err`：
    // 「切到 b4a」和「b4a 在这一帧崩了」是**同一个 commit** —— 用 this.state.err 判断会把刚崩出来
    // 的新错误当成旧标签的残留清掉，于是 children 再渲染一次、再崩一次，一次崩溃记两笔
    //（PR-10 实测踩过：连崩 3 次的用例在第 1 次就数到 2，直接误禁用）。
    if (prevProps.tabId !== this.props.tabId && prevState.err) {
      this.setState({ err: null })
      return
    }
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
