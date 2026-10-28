import { useState } from 'react'
import './ledger.css'

/* The last line of a turn: what it cost. Four facts at rest — calls, tokens,
   cache, wall. The trace's three timings (TTFT, the model's own time, and the
   rate those two imply) are the first thing a narrow column cannot hold, so
   they wait behind the pointer instead of fighting for the same 400px.
   A tap opens them too: a phone has no hover. */
export function Ledger({
  calls,
  tokens,
  cacheRead,
  input,
  wallMs,
  ttftMs,
  llmMs,
  outputTokens,
}: {
  calls: number
  tokens: number
  cacheRead: number
  input: number
  wallMs: number
  ttftMs?: number
  llmMs?: number
  outputTokens?: number
}) {
  const [open, setOpen] = useState(false)
  const fact = (v: string | number, k: string) => (<><b>{v}</b> {k}</>)
  const hit = input ? ((cacheRead / input) * 100).toFixed(1) + '%' : '\u2014'
  // a short turn is not "0.01M tokens": scale the unit, never print noise
  const tok = tokens >= 1e6 ? (tokens / 1e6).toFixed(2) + 'M'
    : tokens >= 1e3 ? Math.round(tokens / 1e3) + 'k'
    : String(Math.round(tokens))
  const tps = llmMs && outputTokens ? Math.round(outputTokens / (llmMs / 1000)) : 0
  const stamp = (k: string, v: string) => (<><span className="rt-lg-k">{k}</span> <b>{v}</b></>)

  const more: React.ReactNode[] = []
  // under 50ms the number is measurement noise, not a first token
  if (ttftMs && ttftMs >= 50) more.push(stamp('TTFT', (ttftMs / 1000).toFixed(ttftMs < 1000 ? 2 : 1) + ' s'))
  if (llmMs) more.push(stamp('LLM', (llmMs / 1000).toFixed(1) + ' s'))
  if (tps) more.push(fact(tps, 'tok/s'))

  return (
    <div
      className={'rt-ledger' + (open ? ' is-open' : '')}
      tabIndex={0}
      role="group"
      aria-label="turn cost"
      onClick={() => more.length && setOpen((v) => !v)}
    >
      {fact(calls, calls === 1 ? 'call' : 'calls')}
      {' \u00b7 '}{fact(tok, 'tokens')}
      {' \u00b7 '}{fact(hit, 'cache')}
      {' \u00b7 '}{fact(Math.round(wallMs / 1000), 's turn')}
      {more.length ? (
        <span className="rt-lg-more">
          <span className="rt-lg-in">
            {'\u00a0\u00b7 '}
            {more.map((n, i) => (<span key={i}>{i ? ' \u00b7 ' : null}{n}</span>))}
          </span>
        </span>
      ) : null}
    </div>
  )
}
