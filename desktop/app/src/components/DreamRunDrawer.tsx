/* Dream 运行记录详情抽屉（设置 → 记忆 → 运行记录，点某一行打开）。
 *
 * 为什么是抽屉而不是新页面：用户点的是表格里的一行，"这一轮到底干了什么"是对那一行
 * 的追问 —— 详情盖在当前面板上、关掉还在原来那一行，不打断"上下翻着看几轮"的动作。
 *
 * 数据来源分两段（调用方负责拼）：
 *   · 表格那一行本身（dream_get 的 runs）先给头部：起止/耗时/provider/model/状态/错误；
 *   · dream_detail 回来后再补 result 全文 + 逐库 + 时间线。
 * 所以 props 里有可选的 loading / error：它们描述的是"详情还没到"，不是"这一轮失败了"，
 * 两者的视觉必须能分开（运行失败 = 红，详情取不到 = 黄）。
 *
 * 老记录（result / steps 字段之前写的）没有这两项：steps 缺失 ≠ 那轮没跑工具，所以
 * 空时间线说的是"没有保存步骤"，不是在断言当时什么都没做。
 *
 * 样式全部内联：index.css 正被另一处改动占着，这个抽屉不新增任何全局 CSS。
 */
import { useEffect, useRef } from 'react'
import type { CSSProperties, ReactElement } from 'react'
import { useT } from '../i18n'
import { fmtDuration } from '../lib/duration'
import type { DreamRun } from '../store/useAppStore'

interface Props {
  /** 头部至少要有表格那一行；详情到达后调用方换成 dream_detail 的整条记录。 */
  run: DreamRun
  onClose: () => void
  /** 详情请求在飞（头部已可读，正文还没有）。 */
  loading?: boolean
  /** 详情请求失败 —— 最常见的是记录已被 200 行滚动窗口挤掉，属正常态，不报红。 */
  error?: string
}

const pad2 = (n: number) => String(n).padStart(2, '0')

function toMs(v?: string): number | undefined {
  if (!v) return undefined
  const t = new Date(v).getTime()
  return Number.isFinite(t) ? t : undefined
}

/** YYYY-MM-DD HH:mm:ss（用户可见时间统一 24h、不跟系统区域跑，同 SettingsModal）。 */
function fmtStamp(v?: string): string {
  const t = toMs(v)
  if (t == null) return '—'
  const d = new Date(t)
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`
}

/** 时间线只要时刻：同一天里看到日期只是噪声。 */
function fmtClock(v?: string): string {
  const t = toMs(v)
  if (t == null) return '—'
  const d = new Date(t)
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`
}

const SEC_TITLE: CSSProperties = {
  fontSize: 11,
  fontWeight: 600,
  letterSpacing: '.04em',
  color: 'var(--text-2)',
  margin: '16px 0 6px',
}
const META_K: CSSProperties = { color: 'var(--text-3)', fontSize: 11, whiteSpace: 'nowrap' }
const META_V: CSSProperties = { color: 'var(--text)', fontSize: 11.5 }
const PANEL: CSSProperties = {
  position: 'fixed',
  top: 0,
  right: 0,
  bottom: 0,
  width: 'min(620px, 96vw)',
  zIndex: 111, // 压过设置卡（.settings-pop = 101）与它的遮罩（100）
  display: 'flex',
  flexDirection: 'column',
  background: 'var(--elevated)',
  borderLeft: '1px solid var(--border-strong)',
  boxShadow: '-20px 0 60px rgba(0, 0, 0, 0.45)',
}
const SCRIM: CSSProperties = {
  position: 'fixed',
  inset: 0,
  zIndex: 110,
  background: 'rgba(8, 6, 4, 0.32)',
}
/** 长正文（收尾正文 / 错误）：pre-wrap + 限高可滚，绝不撑破抽屉。 */
const TEXT_BOX: CSSProperties = {
  whiteSpace: 'pre-wrap',
  wordBreak: 'break-word',
  overflow: 'auto',
  margin: 0,
  padding: '8px 10px',
  borderRadius: 6,
  border: '1px solid var(--border)',
  background: 'var(--bg)',
  color: 'var(--text)',
  fontSize: 11.5,
  lineHeight: 1.65,
  fontFamily: 'var(--font-mono)',
}

export default function DreamRunDrawer({ run, onClose, loading, error }: Props): ReactElement {
  const t = useT()
  const closeRef = useRef<HTMLButtonElement>(null)

  // 焦点进出：打开时进关闭按钮（键盘用户不用 Tab 找出口），关闭时还给触发它的那一行。
  // 触发元素在挂载时抓 —— 卸载时 document.activeElement 已经是被移除的抽屉里的按钮了。
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null
    closeRef.current?.focus()
    return () => {
      if (prev && prev.isConnected && typeof prev.focus === 'function') prev.focus()
    }
  }, [])

  // Esc 关闭。注意 SettingsModal 有一个 window 级 Esc（关整个设置），它注册得更早、
  // 会先跑 —— 那边必须用"抽屉开着就 return"让位（见 SettingsModal 的 detailOpenRef）。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const startMs = toMs(run.started_at)
  const endMs = toMs(run.finished_at)
  // 耗时只在**跑完**时给：运行中的差值每看一次都不一样，那不是"耗时"。
  const durMs = endMs != null && startMs != null ? endMs - startMs : undefined
  const status: 'failed' | 'running' | 'done' = run.error ? 'failed' : run.finished_at ? 'done' : 'running'
  const statusColor = status === 'failed' ? 'var(--error)' : status === 'running' ? 'var(--running)' : 'var(--success)'
  const statusGlyph = status === 'failed' ? '✗' : status === 'running' ? '▶' : '✓'
  const statusLabel = t(`settings.dream.detail.status.${status}`)

  const repos = run.repos ?? []
  const steps = run.steps ?? []
  // 有 result 看全文；没有 result 但有 summary = 老记录，只有压成一行的那句话。
  const hasResult = !!run.result
  const showResult = !!run.result || !!run.summary

  return (
    <>
      <div style={SCRIM} onClick={onClose} aria-hidden="true" />
      <div
        id="dream-detail-drawer"
        role="dialog"
        aria-modal="true"
        aria-label={t('settings.dream.detail.title')}
        style={PANEL}
      >
        {/* —— 头部：标题 + 状态 + 关闭 —— */}
        <div
          style={{
            flex: '0 0 auto',
            padding: '12px 14px 10px',
            borderBottom: '1px solid var(--border)',
            background: 'var(--surface)',
          }}
        >
          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <span style={{ flex: 1, fontSize: 12.5, fontWeight: 600, color: 'var(--text)' }}>
              {t('settings.dream.detail.title')}
            </span>
            <span
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: 4,
                fontSize: 11,
                color: statusColor,
                border: `1px solid ${statusColor}`,
                borderRadius: 999,
                padding: '1px 8px',
              }}
            >
              <span aria-hidden>{statusGlyph}</span>
              {statusLabel}
            </span>
            <button
              ref={closeRef}
              onClick={onClose}
              aria-label={t('settings.dream.detail.close')}
              title={t('settings.dream.detail.close')}
              style={{
                flex: '0 0 auto',
                width: 22,
                height: 22,
                display: 'inline-flex',
                alignItems: 'center',
                justifyContent: 'center',
                background: 'transparent',
                border: '1px solid var(--border-strong)',
                borderRadius: 5,
                color: 'var(--text-2)',
                cursor: 'pointer',
                fontSize: 12,
                lineHeight: 1,
              }}
            >
              ✕
            </button>
          </div>

          {/* —— 起止 / 耗时 / 模型 —— */}
          <div
            style={{
              display: 'grid',
              gridTemplateColumns: 'auto 1fr',
              gap: '3px 10px',
              marginTop: 10,
              alignItems: 'baseline',
            }}
          >
            <span style={META_K}>{t('settings.dream.detail.startedAt')}</span>
            <span style={META_V} className="mono">{fmtStamp(run.started_at)}</span>
            <span style={META_K}>{t('settings.dream.detail.finishedAt')}</span>
            <span style={META_V} className="mono">
              {run.finished_at ? fmtStamp(run.finished_at) : '—'}
            </span>
            <span style={META_K}>{t('settings.dream.detail.duration')}</span>
            {/* NaN / 负值 / 没跑完 → '—'（fmtDuration 自己兜底，不在这里另写一套）。 */}
            <span style={META_V} className="mono">{fmtDuration(durMs)}</span>
            <span style={META_K}>{t('settings.dream.detail.model')}</span>
            <span style={{ ...META_V, wordBreak: 'break-all' }}>
              <code>{run.provider || '—'}</code>
              {run.model ? <span style={{ color: 'var(--text-3)' }}> · {run.model}</span> : null}
            </span>
          </div>

          {/* —— 本轮的错误（模型/工具链失败）：头部下方红字，原样保留换行 —— */}
          {run.error && (
            <div
              style={{
                ...TEXT_BOX,
                maxHeight: 140,
                marginTop: 10,
                color: 'var(--error)',
                border: '1px solid var(--error)',
                background: 'rgba(228, 116, 106, 0.10)',
              }}
            >
              {run.error}
            </div>
          )}
        </div>

        {/* —— 正文（滚动区）—— */}
        <div style={{ flex: 1, minHeight: 0, overflowY: 'auto', padding: '0 14px 18px' }}>
          {loading ? (
            <p style={{ ...SEC_TITLE, color: 'var(--text-3)', fontWeight: 400 }}>
              {t('settings.dream.detail.loading')}
            </p>
          ) : error ? (
            // 取不到详情（多半是记录已被 200 行滚动窗口挤掉）：黄色说明，不是红错。
            <div
              style={{
                ...TEXT_BOX,
                maxHeight: 120,
                marginTop: 14,
                color: 'var(--warning)',
                border: '1px solid var(--border)',
                fontFamily: 'inherit',
                fontSize: 11.5,
              }}
            >
              {error}
            </div>
          ) : (
            <>
              {showResult && (
                <>
                  <div style={SEC_TITLE}>{t('settings.dream.detail.result')}</div>
                  <div style={{ ...TEXT_BOX, maxHeight: 320 }}>{run.result || run.summary}</div>
                  {!hasResult && run.finished_at && (
                    <p style={{ margin: '6px 0 0', fontSize: 11, color: 'var(--text-3)' }}>
                      {t('settings.dream.detail.resultLegacy')}
                    </p>
                  )}
                </>
              )}

              {repos.length > 0 && (
                <>
                  <div style={SEC_TITLE}>{t('settings.dream.detail.repos')}</div>
                  <table className="mem-table">
                    <thead>
                      <tr>
                        <th>{t('settings.dream.detail.repos.col.id')}</th>
                        <th style={{ textAlign: 'right', width: 64 }}>
                          {t('settings.dream.detail.repos.col.topics')}
                        </th>
                        <th style={{ textAlign: 'right', width: 64 }}>
                          {t('settings.dream.detail.repos.col.archived')}
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {repos.map((r, i) => (
                        <tr key={`${r.id}-${i}`}>
                          <td>
                            <code>{r.id || '—'}</code>
                            {r.note ? <span className="tag-dim"> {r.note}</span> : null}
                          </td>
                          <td style={{ textAlign: 'right' }} className="mono">{r.topics || 0}</td>
                          <td style={{ textAlign: 'right' }} className="mono">{r.archived || 0}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </>
              )}

              <>
                <div style={SEC_TITLE}>{t('settings.dream.detail.steps')}</div>
                {steps.length === 0 ? (
                  <p style={{ margin: 0, fontSize: 11, color: 'var(--text-3)' }}>
                    {t('settings.dream.detail.steps.empty')}
                  </p>
                ) : (
                  <div>
                    {steps.map((st, i) => {
                      // 省略占位（capDreamSteps 插的那一条）：tool 为空是因为它**不是一次调用**，
                      // 必须先判 omitted 再看 ok，否则会被画成一个失败步骤。
                      if ((st.omitted ?? 0) > 0) {
                        return (
                          <div
                            key={`omitted-${i}`}
                            style={{
                              display: 'flex',
                              alignItems: 'center',
                              gap: 8,
                              padding: '5px 0',
                              fontSize: 10.5,
                              color: 'var(--text-3)',
                            }}
                          >
                            <span style={{ flex: 1, height: 1, background: 'var(--border)' }} />
                            <span>
                              {t('settings.dream.detail.steps.omitted').replace('{n}', String(st.omitted))}
                            </span>
                            <span style={{ flex: 1, height: 1, background: 'var(--border)' }} />
                          </div>
                        )
                      }
                      return (
                        <div
                          key={`step-${i}`}
                          style={{
                            display: 'grid',
                            gridTemplateColumns: '62px 12px 1fr',
                            gap: '0 6px',
                            padding: '3px 0',
                            alignItems: 'start',
                            borderBottom: '1px solid var(--border)',
                          }}
                        >
                          <span className="mono" style={{ fontSize: 10.5, color: 'var(--text-3)' }}>
                            {fmtClock(st.t)}
                          </span>
                          <span
                            aria-hidden
                            style={{ fontSize: 10.5, color: st.ok ? 'var(--success)' : 'var(--error)' }}
                          >
                            {st.ok ? '✓' : '✗'}
                          </span>
                          <div style={{ minWidth: 0 }}>
                            <div style={{ display: 'flex', gap: 6, alignItems: 'baseline' }}>
                              <code style={{ fontSize: 11, color: 'var(--text)' }}>{st.tool || '—'}</code>
                              {st.arg ? (
                                // 参数（bash 命令可能很长）截两行，悬停看全文。
                                <span
                                  title={st.arg}
                                  style={{
                                    flex: 1,
                                    minWidth: 0,
                                    fontSize: 10.5,
                                    color: 'var(--text-2)',
                                    display: '-webkit-box',
                                    WebkitLineClamp: 2,
                                    WebkitBoxOrient: 'vertical',
                                    overflow: 'hidden',
                                    wordBreak: 'break-all',
                                  }}
                                >
                                  {st.arg}
                                </span>
                              ) : null}
                            </div>
                            {st.note ? (
                              <div style={{ fontSize: 10.5, color: 'var(--text-3)' }}>{st.note}</div>
                            ) : null}
                          </div>
                        </div>
                      )
                    })}
                  </div>
                )}
              </>
            </>
          )}
        </div>
      </div>
    </>
  )
}
