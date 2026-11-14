// SurfacesPage —— 设置 →「面板插件」（PR-14b）。
//
// 这是 `~/.go-code/surfaces/` 的**人眼视图**：装了什么、启用没启用、声明了哪些能力、
// 加载失败的原因，以及最近的调用记录（audit.jsonl 的尾部）。
//
// 四条刻意选择：
//  1. **页面不碰磁盘**：读走 surfaceIndex / surfaceAudit 两个 IPC，写走 surfaceSetEnabled /
//     surfaceRemove。index 的写入纪律（flock + .bak + 原子替换）只有 Go 的 surfaceStore
//     一份（PR-13）—— 渲染层再写一次就等于把「谁有权改 index」变成两处判定。
//  2. **卸载不是删文件**：surfaceRemove 走 PR-13 的 Remove，目录进 `.trash/<id>-<ts>`
//     （手滑一次还能捞回来）。所以按钮要先过 ConfirmModal，且文案说的是「移到回收站」。
//  3. **action 之后重拉 index + audit**：`surfaces_changed` 会让 loader 重算 provider
//     （禁用 → 卸下、启用 → 加载），而页面自己的这两份数据也必须跟着刷新，否则
//     开关显示的还是旧值。两件事都发生，用户才看到「点了就变」。
//  4. **失败给可读原因，不 alert**：reason 是封闭集合（unknown_id / bad_request / error），
//     映射成一句话贴在那一行上；alert 会挡住下一次操作，而且在无头探针里等于卡死。
//
// 样式复用既有 settings 类（settings-sec-title / set-desc / set-placeholder / mcp-server /
// mem-table / mcp-type）—— 不引入新的设计系统，也不加依赖。
import { useCallback, useEffect, useRef, useState } from 'react'
import { useT } from '../i18n'
import type { SurfaceAuditLine, SurfaceIndex, SurfaceIndexItem } from '../transport/types'
import { setSurfaceEnabled, surfaceReasonText } from '../surfaces/manage'
import ConfirmModal from './ConfirmModal'
import { Switch } from './SettingsGroups'

/**
 * 宿主允许插件调用的只读命令（**展示用副本**）。
 *
 * 权威表在 desktop/bridge/surfaces_command.go 的 surfaceHostCommands —— 这里只为了在页面上
 * 把「装插件 = 信任它，但能力是这四条」说清楚。加到那边的同时要顺手改这里；两边不一致
 * 的后果是多显示/少显示一行说明，不会放宽任何权限（判定始终在 Go）。
 */
const HOST_READONLY_COMMANDS = ['git_snapshot', 'workspace_list', 'session_list', 'ui_inspect']

/** audit 表一次拉多少行（与 ReadAudit 的缺省一致；上限 200 在 Go 侧）。 */
const AUDIT_LIMIT = 50

type LoadState = { index: SurfaceIndex | null; audit: SurfaceAuditLine[] | null }

export default function SurfacesPage() {
  const t = useT()
  const [state, setState] = useState<LoadState>({ index: null, audit: null })
  const [loading, setLoading] = useState(true)
  const [auditLoading, setAuditLoading] = useState(false)
  const [busyId, setBusyId] = useState<string | null>(null)
  // 每行一条错误（键 = 插件 id；页级错误用空串键）：贴在那一行下面，不弹窗。
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [confirmId, setConfirmId] = useState<string | null>(null)

  // reason → 一句人话：映射在 surfaces/manage.ts（与墓碑的「重新启用」共用）。
  // 调用点一律走 tRef.current（见下），这样 useCallback 的依赖里不必挂那个每帧都变的 t。
  // useT() 每次渲染都返回**新函数**，所以 t 不能进 useCallback/useEffect 的依赖数组：
  // 那会让 fetchIndex 的标识每帧都变 → 挂载 effect 每帧重跑 → setState → 无限渲染
  // （实测现象是「点开关，浏览器卡住不动」）。用 ref 拿当前 t，依赖只留稳定的 setter。
  const tRef = useRef(t)
  tRef.current = t

  const setRowError = useCallback((id: string, msg: string) => {
    setErrors((prev) => {
      const next = { ...prev }
      if (msg) next[id] = msg
      else delete next[id]
      return next
    })
  }, [])

  // fetchIndex 拉一次索引。读失败**不清空**已有列表（一次 IPC 抖动不该等于「插件都没了」），
  // 而是留一行页级错误 —— 与 loader 对「读索引失败保持现状」的口径一致。
  const fetchIndex = useCallback(async (): Promise<SurfaceIndex | null> => {
    const d = window.desktop
    if (!d?.surfaceIndex) return { version: 1, items: [] } // 浏览器/mock 档：空列表而不是崩溃
    try {
      return await d.surfaceIndex()
    } catch {
      setRowError('', tRef.current('surfaces.err.indexRead'))
      return null
    }
  }, [setRowError])

  const fetchAudit = useCallback(async (): Promise<SurfaceAuditLine[] | null> => {
    const d = window.desktop
    if (!d?.surfaceAudit) return []
    try {
      const r = await d.surfaceAudit(AUDIT_LIMIT)
      if (!r?.ok) {
        setRowError('', surfaceReasonText(tRef.current, r?.reason))
        return null
      }
      // 桥侧已经跳过坏行（ReadAudit），这里再挡一次非对象项：IPC 的另一头不是
      // 我们能证明的东西，而一个 null 会让整张表在渲染期抛错（白屏比少一行糟得多）。
      return Array.isArray(r.lines) ? r.lines.filter((l): l is SurfaceAuditLine => !!l && typeof l === 'object') : []
    } catch {
      setRowError('', tRef.current('surfaces.err.auditRead'))
      return null
    }
  }, [setRowError])

  /** refresh 同时重拉两份数据（任何动作之后都调用，见文件头第 3 条）。 */
  const refresh = useCallback(async () => {
    const [idx, audit] = await Promise.all([fetchIndex(), fetchAudit()])
    setState((prev) => ({
      index: idx ?? prev.index,
      audit: audit ?? prev.audit,
    }))
  }, [fetchAudit, fetchIndex])

  useEffect(() => {
    let alive = true
    void (async () => {
      const [idx, audit] = await Promise.all([fetchIndex(), fetchAudit()])
      if (!alive) return
      setState((prev) => ({ index: idx ?? prev.index, audit: audit ?? prev.audit }))
      setLoading(false)
    })()
    return () => {
      alive = false
    }
  }, [fetchAudit, fetchIndex])

  const onToggle = useCallback(
    async (item: SurfaceIndexItem, next: boolean) => {
      setBusyId(item.id)
      setRowError(item.id, '')
      try {
        // 传的是**目标值**（不是"当前值"）：传反了等于点「停用」变成再启用一次，
        // 而 UI 上看起来什么都没发生（e2e 的「开关传反值」变异钉住这一条）。
        const r = await setSurfaceEnabled(item.id, next)
        if (!r.ok) {
          setRowError(item.id, surfaceReasonText(tRef.current, r.reason))
          return
        }
        await refresh()
      } finally {
        setBusyId(null)
      }
    },
    [refresh, setRowError],
  )

  const onRemove = useCallback(
    async (id: string) => {
      setConfirmId(null)
      setBusyId(id)
      setRowError(id, '')
      try {
        const r = await window.desktop?.surfaceRemove?.(id)
        if (!r?.ok) {
          setRowError(id, surfaceReasonText(tRef.current, r?.reason))
          return
        }
        await refresh()
      } finally {
        setBusyId(null)
      }
    },
    [refresh, setRowError],
  )

  const onRefreshAudit = useCallback(async () => {
    setAuditLoading(true)
    try {
      const audit = await fetchAudit()
      if (audit) setState((prev) => ({ ...prev, audit }))
    } finally {
      setAuditLoading(false)
    }
  }, [fetchAudit])

  const items = state.index?.items ?? []
  // 审计表按「最新在上」渲染（bridge 回的是文件顺序：旧 → 新）。
  const auditRows = state.audit ? [...state.audit].reverse() : []

  const actionLabel = (action: string): string => {
    switch (action) {
      case 'install':
        return t('surfaces.action.install')
      case 'remove':
        return t('surfaces.action.remove')
      case 'enable':
        return t('surfaces.action.enable')
      case 'disable':
        return t('surfaces.action.disable')
      case 'command':
        return t('surfaces.action.command')
      default:
        return action // 未知 action 原样显示：协议新增了一种就多一行，不猜
    }
  }

  return (
    <div className="surfaces-page">
      <div className="settings-sec-title">{t('settings.surfaces')}</div>
      <p className="set-desc" style={{ marginBottom: 8 }}>{t('surfaces.hint')}</p>
      {/* 白名单四条：这是「装插件 = 信任它」这句话的具体边界 —— 面板能读到的宿主数据
          只有这四条只读命令，写类一条都没开（表在 bridge，页面上只是照抄）。 */}
      <div className="hook-note" style={{ marginBottom: 12 }}>
        <span>{t('surfaces.whitelist')}</span>
        {HOST_READONLY_COMMANDS.map((c) => (
          <span className="mcp-type mono" key={c}>{c}</span>
        ))}
      </div>

      {errors[''] && <p className="mcp-s-err">{errors['']}</p>}

      {loading && !state.index ? (
        <p className="set-placeholder">{t('surfaces.loading')}</p>
      ) : items.length === 0 ? (
        <p className="set-placeholder">{t('surfaces.empty')}</p>
      ) : (
        items.map((item) => (
          <div className="mcp-server" key={item.id} data-surface-id={item.id} data-enabled={item.enabled ? '1' : '0'}>
            <div className="mcp-s-h">
              <span className={`mcp-state ${item.enabled ? 'connected' : ''}`} data-surface-state>
                {item.enabled ? t('surfaces.enabled') : t('surfaces.disabled')}
              </span>
              <span className="mcp-name">{item.id}</span>
              {item.error && (
                <span className="mcp-state error" data-surface-error title={item.error}>
                  {t('surfaces.errorBadge')}
                </span>
              )}
              <span style={{ flex: 1 }} />
              {/* 版本 + 短 hash：hash 前 8 位够用来对「装的是不是同一版」（长 hash 在 index 里） */}
              <span className="mcp-type mono">{item.version || '—'}</span>
              <span className="mcp-type mono" title={item.hash}>{item.hash ? item.hash.slice(0, 8) : '—'}</span>
              <Switch
                on={item.enabled}
                disabled={busyId === item.id}
                label={item.enabled ? t('surfaces.disable') : t('surfaces.enable')}
                onChange={(next) => void onToggle(item, next)}
              />
              <button
                className="btn danger surface-uninstall"
                disabled={busyId === item.id}
                onClick={() => setConfirmId(item.id)}
              >
                {t('surfaces.uninstall')}
              </button>
            </div>

            {/* 声明（manifest.commands / data）：装之前看到的就是这份契约，装之后在这里对账。 */}
            <div className="mcp-s-cmd">
              <span className="set-desc">{t('surfaces.declared.commands')}</span>{' '}
              {item.commands.length === 0 ? (
                <span className="set-desc">{t('surfaces.declared.none')}</span>
              ) : (
                item.commands.map((c) => (
                  <span className="mcp-type mono" key={`c-${c}`} data-surface-command={c}>{c}</span>
                ))
              )}
            </div>
            <div className="mcp-s-cmd">
              <span className="set-desc">{t('surfaces.declared.data')}</span>{' '}
              {item.data.length === 0 ? (
                <span className="set-desc">{t('surfaces.declared.none')}</span>
              ) : (
                item.data.map((c) => (
                  <span className="mcp-type mono" key={`d-${c}`} data-surface-data={c}>{c}</span>
                ))
              )}
            </div>

            {/* manifest 坏掉的原因：这一条**照常显示**（用户还看得见装了什么、为什么不能用） */}
            {item.error && <div className="mcp-s-err mono" data-surface-error-msg>{item.error}</div>}
            {errors[item.id] && <div className="mcp-s-err" data-surface-row-error>{errors[item.id]}</div>}
          </div>
        ))
      )}

      <div className="settings-sec-title" style={{ marginTop: 18, display: 'flex', alignItems: 'center', gap: 10 }}>
        {t('surfaces.audit.title')}
        <button className="btn" disabled={auditLoading} onClick={() => void onRefreshAudit()}>
          {auditLoading ? t('surfaces.audit.refreshing') : t('surfaces.audit.refresh')}
        </button>
      </div>
      <p className="set-desc" style={{ marginBottom: 8 }}>{t('surfaces.audit.hint')}</p>
      {auditRows.length === 0 ? (
        <p className="set-placeholder">{t('surfaces.audit.empty')}</p>
      ) : (
        <table className="mem-table">
          <thead>
            <tr>
              <th>{t('surfaces.audit.col.id')}</th>
              <th>{t('surfaces.audit.col.action')}</th>
              <th>{t('surfaces.audit.col.cmd')}</th>
              <th>{t('surfaces.audit.col.ok')}</th>
              <th>{t('surfaces.audit.col.ts')}</th>
            </tr>
          </thead>
          <tbody>
            {auditRows.map((line, i) => (
              <tr key={`${line.ts}-${i}`} data-surface-audit data-surface-audit-ok={line.ok ? '1' : '0'}>
                <td className="mono">{line.id}</td>
                <td>{actionLabel(line.action)}</td>
                <td className="mono">{line.cmd || '—'}</td>
                <td className={line.ok ? '' : 'mono'} style={line.ok ? undefined : { color: 'var(--crimson)' }}>
                  {line.ok ? '✓' : '✕'}
                </td>
                <td className="mono" title={line.ts}>{formatTs(line.ts)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {confirmId && (
        <ConfirmModal
          title={t('surfaces.uninstall.title')}
          message={t('surfaces.uninstall.msg').replace('{id}', confirmId)}
          confirmLabel={t('surfaces.uninstall.confirm')}
          cancelLabel={t('surfaces.cancel')}
          danger
          onConfirm={() => void onRemove(confirmId)}
          onCancel={() => setConfirmId(null)}
        />
      )}
    </div>
  )
}

/**
 * formatTs 把 RFC3339Nano 截成「日期 + 时分秒」（R07 的表格口径：一列里容得下）。
 *
 * 刻意**不用** new Date().toLocaleString()：那是渲染环境时区/语言的函数，同一个 ts 在
 * 探针机与开发机上会显示成两串不同的字，截图对比与断言都会飘。截字符串则恒定。
 */
function formatTs(ts: string): string {
  if (!ts) return '—'
  const head = ts.slice(0, 10)
  const time = ts.slice(11, 19)
  return time ? `${head} ${time}` : head
}
