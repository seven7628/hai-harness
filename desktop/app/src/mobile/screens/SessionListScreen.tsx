// 手机会话列表屏（P8-02）：工作区 + 会话。
//
// 数据来源是 bridge（workspace_list + list），不是桌面 localStorage 的项目树 ——
// 手机首次配对时那份树是空的（见 mobile/session-data.ts 的文件头）。
//
// 这一层刻意**薄**：只画行、只做导航。点一行 → 交给壳的 onOpenSession(ws, sid)，
// 真正的激活在 SessionScreen 里走 useAppStore.setActiveSession(sid, ws)（桌面同一套
// 恢复/事件路径）。列表屏**完全不碰 store**（连 wire 都不在这里做）—— 于是点开
// 「会话」只需加载这个 ~3KB 的小 chunk，桌面 store + Narrative 那一大坨要等真正打开
// 某个会话时才拉（见 session-boot.ts 与 SessionScreen）。
//
// 与 Hub 列表并列（壳的 tab 切换），返回栈也是分开的：会话页返回 → 这里，Hub 返回 →
// Hub 列表（见 MobileApp 的 nav）。
import { useCallback, useEffect, useMemo, useState } from 'react'
import { t as i18nT, useLang, useT } from '../../i18n'
import { sharedMobileTransport } from '../transport/mobile'
import {
  fetchSessions,
  fetchWorkspaces,
  fmtSessionTime,
  pickWorkspace,
  readSavedWorkspace,
  sessionLabel,
  workspaceLabel,
  writeSavedWorkspace,
  type MobileSessionRow,
  type MobileWorkspace,
} from '../session-data'

export default function SessionListScreen({
  onOpenSession,
}: {
  /** 打开某个工作区的某个会话（壳负责切屏；本屏不持有会话状态）。 */
  onOpenSession: (ws: string, sid: string, title?: string) => void
}) {
  const t = useT()
  const lang = useLang()
  const [workspaces, setWorkspaces] = useState<MobileWorkspace[]>([])
  const [ws, setWs] = useState('')
  const [sessions, setSessions] = useState<MobileSessionRow[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [tick, setTick] = useState(0) // 「刷新」按钮：变一下就重跑加载 effect

  // 工作区清单：只在挂载/刷新时拉。失败保留上一次的结果（弱网下不要清空屏幕）。
  // 注意 deps 里**不能放 t**：useT() 每次渲染都返回新函数，放进去就是死循环。
  useEffect(() => {
    let alive = true
    setLoading(true)
    setError('')
    void fetchWorkspaces(sharedMobileTransport()).then((r) => {
      if (!alive) return
      if (!r.ok) {
        setError(r.error || i18nT('mobile.sessions.loadFailed'))
        setLoading(false)
        return
      }
      setWorkspaces(r.items)
      setWs((cur) => pickWorkspace(r.items, cur || readSavedWorkspace()))
    })
    return () => {
      alive = false
    }
  }, [tick])

  // 会话清单：跟着选中的工作区走。切工作区先清空，避免短暂显示上一个工作区的行。
  useEffect(() => {
    if (!ws) {
      setSessions([])
      setLoading(false)
      return
    }
    let alive = true
    setLoading(true)
    setError('')
    writeSavedWorkspace(ws)
    void fetchSessions(sharedMobileTransport(), ws).then((r) => {
      if (!alive) return
      if (!r.ok) {
        setError(r.error || i18nT('mobile.sessions.loadFailed'))
        setSessions([])
      } else {
        setSessions(r.items)
      }
      setLoading(false)
    })
    return () => {
      alive = false
    }
  }, [ws, tick])

  const refresh = useCallback(() => setTick((n) => n + 1), [])
  // 相对时间的基准：会话清单或工作区变了才重算（不在渲染里读 Date.now()，
  // 那会让每次重渲染都得到不同的字符串）。deps 里刻意只放「变了就该重算」的信号 ——
  // ESLint 看不出这层意图，故显式豁免。
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const now = useMemo(() => Date.now(), [sessions, ws])

  return (
    <div className="m-sess-list" data-testid="mobile-session-list">
      {workspaces.length > 1 ? (
        <label className="m-sess-ws">
          <span className="m-sess-ws-label">{t('mobile.sessions.workspace')}</span>
          <select
            className="m-sess-ws-select"
            value={ws}
            onChange={(e) => setWs(e.target.value)}
            data-testid="mobile-session-workspace"
          >
            {workspaces.map((w) => (
              <option key={w.path} value={w.path}>
                {(w.pinned ? '★ ' : '') + workspaceLabel(w, lang)}
              </option>
            ))}
          </select>
        </label>
      ) : null}

      {error ? (
        <p className="m-warn m-sess-error" role="alert" data-testid="mobile-session-error">
          <span className="m-sess-error-text">{error}</span>
          <button type="button" className="m-sess-retry" onClick={refresh}>
            {t('mobile.sessions.retry')}
          </button>
        </p>
      ) : null}

      {!loading && workspaces.length === 0 ? (
        <p className="m-sess-empty" data-testid="mobile-session-empty">
          {t('mobile.sessions.noWorkspace')}
        </p>
      ) : null}

      {!loading && workspaces.length > 0 && sessions.length === 0 ? (
        <p className="m-sess-empty" data-testid="mobile-session-empty-ws">
          {t('mobile.sessions.noSession')}
        </p>
      ) : null}

      <ul className="m-sess-rows" role="list">
        {sessions.map((s) => (
          <li key={s.id}>
            <button
              type="button"
              className="m-sess-row"
              onClick={() => onOpenSession(ws, s.id, sessionLabel(s, lang))}
              data-testid="mobile-session-row"
              data-sid={s.id}
            >
              <span className="m-sess-row-main">
                <span className="m-sess-row-title">
                  {s.pinned ? <span className="m-sess-pin" aria-hidden="true">★</span> : null}
                  {sessionLabel(s, lang)}
                </span>
                <span className="m-sess-row-meta">
                  {s.running ? <span className="m-sess-run" aria-hidden="true" /> : null}
                  {s.running ? t('mobile.sessions.running') : ''}
                  {s.running && fmtSessionTime(s.updatedAt, now, lang) ? ' · ' : ''}
                  {fmtSessionTime(s.updatedAt, now, lang)}
                </span>
              </span>
              <span className="m-sess-row-chev" aria-hidden="true">›</span>
            </button>
          </li>
        ))}
      </ul>

      {loading ? (
        <p className="m-loading" role="status" data-testid="mobile-session-loading">
          {t('mobile.sessions.loading')}
        </p>
      ) : null}

      {workspaces.length > 0 ? (
        <div className="m-sess-foot">
          <button type="button" className="m-sess-retry" onClick={refresh} data-testid="mobile-session-refresh">
            {t('mobile.sessions.refresh')}
          </button>
        </div>
      ) : null}
    </div>
  )
}
