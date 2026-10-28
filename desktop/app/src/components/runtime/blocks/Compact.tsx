import { memo, useMemo, useState } from 'react'
import type { CompressionInfo } from '../../../store/useAppStore'
import { Markdown } from '../../markdown'
import { fmtDuration, fmtRun, fmtTokens, fmtUsd } from '../../ui'
import { Orbit } from '../Orbit'
import { useClock } from '../../../lib/useNow'
import { useT } from '../../../i18n'
import './compact.css'

/* A compression is one line in the transcript — the same 30px a tool call
   gets — and everything that would make it a card lives in the fold.

   What may be drawn is decided by what is known:
   · while it runs, only countable facts (messages digested so far, seconds).
     How long a summary takes is not knowable, so nothing on screen pretends it
     is — there is no progress bar in the running state.
   · when it lands, the measurement exists, so the finished state may draw it.

   The bar measures messages, not tokens: `before`/`after` are message counts,
   and the only token figure the event carries is the context size *after* the
   cut — a single number cannot make a ratio. So the bar keeps the unit it can
   be honest in, and the tokens appear as the fact they are ("ctx 6.8k"). */

/* 设计里的记法：42.1k / 6.8k / 1.09M —— 一位小数，单位随量级走 */
const tokShort = (n: number): string =>
  n >= 1e6 ? (n / 1e6).toFixed(1) + 'M' : n >= 1e3 ? (n / 1e3).toFixed(1) + 'k' : String(Math.round(n))

const CHECK =
  'M2.5 6.3l2.3 2.3L9.5 3.7'
const CHEV = 'M4.2 2.6L8.6 6.8l-4.4 4.2'

export const Compact = memo(function Compact({ b }: { b: CompressionInfo }) {
  const t = useT()
  const [open, setOpen] = useState(false)
  const failed = Boolean(b.error)
  const aborted = Boolean(b.aborted)
  const active = Boolean(b.active) && !failed && !aborted
  const state = failed ? 'fail' : aborted ? 'abort' : active ? 'run' : 'done'

  const before = Math.max(0, b.before)
  const after = Math.max(0, b.after)
  /* 配比的第一口径是 token（设计里 42.1k → 6.8k），因为"18 条 → 1 条"读者没有体感。
     这一对来自 CompressEnd.CtxTokens/ctx_tokens_before（同一请求组装口径）；拿不到
     才退回消息数 —— 两个数都缺时干脆不画配比（见下方 cut）。 */
  const tokIn = b.ctxTokensBefore && b.ctxTokensBefore > 0 ? b.ctxTokensBefore : 0
  const tokOut = b.ctxTokens && b.ctxTokens > 0 ? b.ctxTokens : 0
  const byToken = tokIn > 0 && tokOut > 0 && tokOut < tokIn
  const keepPct = byToken
    ? Math.min(100, (tokOut / tokIn) * 100)
    : before
      ? Math.min(100, (after / before) * 100)
      : 0
  const saved = keepPct || byToken ? Math.max(0, 100 - Math.round(keepPct)) : 0
  const dur = b.durMs != null ? fmtDuration(b.durMs) : null
  // 失败/中止都没有真的切下去（after == before）：显示「−0%」是一句假话，
  // 所以配比只在成功落地时画
  const cut = state === 'done' && before > 0
  // the meta reads left to right as: what the context is now · how long it took
  const meta: string[] = []
  if (!active) {
    if (byToken) {
      // the shape the canvas uses: 42.1k → 6.8k
      meta.push(tokShort(tokIn) + ' \u2192 ' + tokShort(tokOut))
    } else if (b.ctxTokens != null && b.ctxTokens > 0) {
      meta.push(t('compact.ctx').replace('{n}', fmtTokens(b.ctxTokens)))
    }
  }
  if (dur) meta.push(dur)

  const label = active
    ? t('compression.inProgress')
    : failed
      ? t('compression.failed')
      : aborted
        ? t('compression.aborted')
        : t('compression.summary').replace('{before}', String(before)).replace('{after}', String(after))

  // the summary is prose the model wrote; the analysis is a numbered trace of
  // what it decided to keep — two different things, two different shapes
  const think = useMemo(
    () =>
      (b.analysis || '')
        .split('\n')
        .map((l) => l.replace(/^\s*(?:[-*•]|\d+[.)])\s*/, '').trim())
        .filter(Boolean),
    [b.analysis],
  )
  const total = b.usage ? b.usage.input + b.usage.output : 0
  // 运行中的秒数走全局 1s 时钟（只有这一处订阅，正文不跟着重渲）
  const liveMs = useClock(b.startedAt, active && b.startedAt != null)

  return (
    <div className="rt-cp" data-s={state}>
      <button
        className="rt-cp-row"
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <span className="rt-cp-g" aria-hidden="true">
          {active ? (
            <Orbit size={16} />
          ) : failed ? (
            <svg viewBox="0 0 12 12" aria-hidden="true">
              <path d="M3.2 3.2l5.6 5.6M8.8 3.2l-5.6 5.6" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" />
            </svg>
          ) : aborted ? (
            <svg viewBox="0 0 12 12" aria-hidden="true">
              <rect x="3" y="3" width="6" height="6" rx="1" fill="currentColor" />
            </svg>
          ) : (
            <svg viewBox="0 0 12 12" aria-hidden="true">
              <path d={CHECK} fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          )}
        </span>
        <span className="rt-cp-lab">{label}</span>
        {cut ? (
          <span
            className="rt-cp-ratio"
            role="img"
            aria-label={t('compact.keptOf')
              .replace('{kept}', byToken ? tokShort(tokOut) : String(after))
              .replace('{all}', byToken ? tokShort(tokIn) : String(before))}
          >
            <i style={{ width: keepPct.toFixed(1) + '%' }} />
          </span>
        ) : null}
        <span className="rt-cp-meta">
          {active && b.startedAt ? <span className="rt-cp-el">{fmtRun(liveMs) + '\u2026'}</span> : null}
          {cut ? (
            <>
              <span className="cut">{'\u2212' + saved + '%'}</span>
              {meta.length ? <span className="sep">{'\u00b7'}</span> : null}
            </>
          ) : null}
          {meta.map((m, i) => (
            <span key={i}>
              {i ? <span className="sep">{'\u00b7'}</span> : null}
              {m}
            </span>
          ))}
        </span>
        <svg className="rt-cp-chev" viewBox="0 0 12 12" aria-hidden="true">
          <path d={CHEV} fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      </button>

      <div className={'rt-cp-fold' + (open ? ' open' : '')}>
        <div>
          <div className="rt-cp-in">
            {failed ? (
              <div className="rt-cp-err" role="alert">{b.error}</div>
            ) : aborted ? (
              <div className="rt-cp-err">{t('compression.abortedDesc')}</div>
            ) : (
              <>
                {b.summary ? (
                  <div className="rt-cp-sum">
                    <span className="rt-cp-k">
                      {t('compact.summaryK').replace('{n}', String(before))}
                    </span>
                    <Markdown text={b.summary} />
                  </div>
                ) : null}

                {think.length ? (
                  <details className="rt-cp-think">
                    <summary>{t('compact.think')}</summary>
                    <ol>
                      {think.map((l, i) => (
                        <li key={i}>{l}</li>
                      ))}
                    </ol>
                  </details>
                ) : null}

                {cut ? (
                  <>
                    <div
                      className="rt-cp-bar"
                      role="img"
                      aria-label={t('compact.keptOf')
              .replace('{kept}', byToken ? tokShort(tokOut) : String(after))
              .replace('{all}', byToken ? tokShort(tokIn) : String(before))}
                    >
                      <i className="keep" style={{ width: keepPct.toFixed(1) + '%' }} />
                    </div>
                    <div className="rt-cp-legend">
                      <span className="l keep">
                        {t('compact.kept')} <b>{byToken ? tokShort(tokOut) : after}</b>
                      </span>
                      <span className="l drop">
                        {t('compact.dropped')} <b>{byToken ? tokShort(tokIn - tokOut) : before - after}</b>
                      </span>
                      <span>{t('compact.saved').replace('{n}', String(saved))}</span>
                    </div>
                  </>
                ) : null}

                <div className="rt-cp-facts">
                  {b.reason ? (
                    <div className="rt-cp-li" data-k="plain">
                      <span className="k">{t('compression.reason')}</span>
                      <span>{b.reason === 'automatic' ? t('compression.auto') : t('compression.manual')}</span>
                    </div>
                  ) : null}
                  {b.ctxTokens ? (
                    <div className="rt-cp-li" data-k="kept">
                      <span className="k">{t('compression.afterCtx')}</span>
                      <span>{fmtTokens(b.ctxTokens)} tok</span>
                    </div>
                  ) : null}
                  {b.model ? (
                    <div className="rt-cp-li" data-k="plain">
                      <span className="k">{t('compression.model')}</span>
                      <span className="mono">{b.model}</span>
                    </div>
                  ) : null}
                  {b.usage ? (
                    <div className="rt-cp-li" data-k="plain">
                      <span className="k">{t('compression.inputOutput')}</span>
                      <span>
                        {fmtTokens(b.usage.input)} / {fmtTokens(b.usage.output)} tok
                        {b.usage.cacheRead > 0 ? ' \u00b7 ' + t('compression.cacheRead') + ' ' + fmtTokens(b.usage.cacheRead) : ''}
                        {b.usage.costUsd ? ' \u00b7 ' + fmtUsd(b.usage.costUsd) : ''}
                      </span>
                    </div>
                  ) : null}
                </div>

                <div className="rt-cp-foot">
                  {dur ? <span>{dur}</span> : null}
                  {total ? <span>{t('compact.callCost').replace('{n}', fmtTokens(total))}</span> : null}
                  {b.reason ? <span>{b.reason === 'automatic' ? t('compression.auto') : t('compression.manual')}</span> : null}
                </div>
              </>
            )}
          </div>
        </div>
      </div>
    </div>
  )
})
