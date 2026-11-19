// webApp.tsx —— URL 型插件的渲染层宿主（2026-11）。
//
// 这一页**不是** React 组件树里的页面：真正的内容在主进程的 WebContentsView 里（见
// electron/plugin-app.ts）。本组件只做四件事：
//   1. 建/复用视图（按 `${pluginId}:${tabId}`，一个 tab 一条）；
//   2. 把 DOM 容器量成矩形，show/hide 原生视图（切标签/收右栏/弹窗打开都要 hide ——
//      原生层级恒在 DOM 之上，不 hide 会盖住设置、提问等弹窗）；
//   3. 宿主自己的 loading / error 态（视图加载完成前不露 Chromium 的空白页/错误页）；
//   4. 卸载只 hide 不 close：切走再回来还是那一页（浏览器标签的心智）。关 tab 由
//      provider.onClose 负责 close。
//
// 页面能力不走这里：preload 注入的 `window.haiHost` 在视图里，command 经主进程回到
// 渲染层的 createHost（与 ESM 面板同一份白名单/Agent token/审计）。
import { useEffect, useRef, useState } from 'react'
import { useT } from '../../i18n'
import { bridge } from '../../lib/bridge'
import { embedRectOf } from '../../lib/embedRect'
import { useDomOverlayOpen, useUiModalOpen } from '../../lib/uiOverlay'
import { useAppStore } from '../../store/useAppStore'
import type { SurfaceAppStatus, SurfaceWebStartDecl } from '../../transport/types'

export interface PluginAppHostProps {
  pluginId: string
  tabId: string
  url: string
  /** manifest.web.url 的根：服务探活/启动用（url 可能带 entry path） */
  webUrl?: string
  /** manifest.web.start：有它则主进程先拉起服务再加载页面 */
  start?: SurfaceWebStartDecl
  /** manifest.data：交给 preload 做事件闸门（main 再校验一次） */
  events: string[]
  /** manifest.commands：交给 preload/main 做命令闸门 */
  commands: string[]
  /** manifest.web.permissions：目前只支持 media */
  permissions?: string[]
}

export function PluginAppHost({ pluginId, tabId, url, webUrl, start, events, commands, permissions }: PluginAppHostProps) {
  const t = useT()
  const hostRef = useRef<HTMLDivElement>(null)
  const key = `${pluginId}:${tabId}`
  const [status, setStatus] = useState<SurfaceAppStatus['status']>('loading')
  const [error, setError] = useState('')
  const [logs, setLogs] = useState('')
  const [nonce, setNonce] = useState(0)
  const rightExpanded = useAppStore((s) => s.rightExpanded)
  const menuOpen = useAppStore((s) => s.panelMenuOpen)
  const uiModalOpen = useUiModalOpen()
  const domOverlayOpen = useDomOverlayOpen(hostRef, true)
  const hidden = !rightExpanded || menuOpen || uiModalOpen || domOverlayOpen

  // 建/复用视图。依赖 key/url：换 entry（同一 tab 换 URL）时重新 load。
  useEffect(() => {
    let alive = true
    setStatus('loading')
    setError('')
    setLogs('')
    const off = bridge?.onPluginAppStatus?.((s) => {
      if (!alive || s.key !== key) return
      setStatus(s.status)
      setError(s.error ?? '')
      setLogs(s.logs ?? '')
    })
    void bridge?.pluginAppOpen?.({ key, pluginId, url, ...(webUrl ? { webUrl } : {}), events, commands, ...(start ? { start } : {}), ...(permissions?.length ? { permissions } : {}) })
      .then((r) => {
        if (!alive) return
        if (!r.ok) {
          setStatus('error')
          setError(r.error || '打开失败')
        }
      })
      .catch((e: unknown) => {
        if (!alive) return
        setStatus('error')
        setError(e instanceof Error ? e.message : String(e))
      })
    return () => {
      alive = false
      off?.()
      // 切标签/收右栏：只 hide。视图在 main 里保活，回来时页面状态还在。
      void bridge?.pluginAppHide?.(key)
    }
  }, [key, pluginId, url, webUrl, start, events, commands, permissions, nonce])

  // 量矩形 + show/hide。hidden 时一律 hide（原生层级压不住 DOM 弹窗）。
  useEffect(() => {
    if (status !== 'ready' || hidden) {
      void bridge?.pluginAppHide?.(key)
      return
    }
    const el = hostRef.current
    if (!el) return
    let raf = 0
    const sync = () => {
      const rect = embedRectOf(el)
      if (rect) void bridge?.pluginAppShow?.({ key, rect })
    }
    const schedule = () => {
      cancelAnimationFrame(raf)
      raf = requestAnimationFrame(sync)
    }
    sync()
    const ro = new ResizeObserver(schedule)
    ro.observe(el)
    window.addEventListener('resize', schedule)
    // 右栏展开有 180ms 动画：动画期间 DOM 矩形一直在变，ResizeObserver 不一定每帧都发。
    const t1 = window.setTimeout(sync, 60)
    const t2 = window.setTimeout(sync, 240)
    return () => {
      cancelAnimationFrame(raf)
      ro.disconnect()
      window.removeEventListener('resize', schedule)
      clearTimeout(t1)
      clearTimeout(t2)
      void bridge?.pluginAppHide?.(key)
    }
  }, [status, hidden, key, nonce])

  // 卸载 = 切标签/收右栏，只 hide；真正的 close 在 provider.onClose。
  useEffect(() => () => { void bridge?.pluginAppHide?.(key) }, [key])

  const retry = () => {
    // 关掉再建：did-fail-load 留下的 Chromium 错误页不能靠 show 变好。
    const closing = bridge?.pluginAppClose?.(key)
    if (closing) void closing.then(() => setNonce((n) => n + 1))
    else setNonce((n) => n + 1)
  }

  return (
    <div className="plugin-app" ref={hostRef} data-plugin-app={key}>
      {status !== 'ready' && (
        <div className="plugin-app-state" data-state={status}>
          {status === 'error' ? (
            <>
              <div className="plugin-app-state-title">{t('rp.surface.app.failed')}</div>
              <div className="plugin-app-state-detail mono">{error || url}</div>
              {logs && <pre className="plugin-app-state-logs mono">{logs}</pre>}
              <div className="plugin-app-state-actions">
                <button className="btn sm" onClick={retry}>{t('rp.surface.app.retry')}</button>
                <span className="plugin-app-state-url mono">{url}</span>
              </div>
            </>
          ) : (
            <>
              <div className="plugin-app-state-title">
                {status === 'starting' ? t('rp.surface.app.starting') : t('rp.surface.app.loading')}
              </div>
              <div className="plugin-app-state-url mono">{url}</div>
              {status === 'starting' && <div className="plugin-app-state-hint">{t('rp.surface.app.startingHint')}</div>}
            </>
          )}
        </div>
      )}
    </div>
  )
}
