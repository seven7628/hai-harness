// [recovered] from /private/tmp/builtin-surfaces.test.cjs —— 回译（back-translation）：
// 原文件是 esbuild 产出的 `jsxDEV(...)` 编译形态；本文件按同一逻辑回译成更接近原始 TSX 的写法，
// **不改变任何行为**。仅此一个文件回译，其余同级模块仍保留编译形态（顶部标「未回译」）。
//
// 注意：同级 ./frame ./writer ./paneMachine ./theme ./formatSelection 目前仍是编译产物，
// 其导出名是 bundle 内联的（next/terminalWriter/encodeCtrl/decodeFrame/...）。下面 import 的名字
// 是据其逻辑推出的**期望导出名**，属「推断」；若后续把这些模块也回译，以此为准对齐即可。
/* eslint-disable @typescript-eslint/no-explicit-any */
import { useCallback, useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import type { Terminal as XTerm } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'

import { useT } from '../../i18n'
import { useAppStore } from '../../store/useAppStore'
import { getTransport } from '../../transport'
import { emitUI } from '../../lib/uiBus'
import { copyText } from '../../lib/clipboard'
import { next, type PaneState, type PaneEvent } from './paneMachine'
import { terminalWriter } from './writer'
import { encodeCtrl, decodeFrame } from './frame'
import { formatSelection } from './formatSelection'
import { readTermTheme, watchTheme } from './theme'

const RESIZE_DEBOUNCE_MS = 100

type MenuPos = { x: number; y: number }

export function TerminalPane({ tabId, ptyId }: { tabId: string; ptyId: string }) {
  const t = useT()
  const tRef = useRef(t)
  tRef.current = t

  const hostRef = useRef<HTMLDivElement | null>(null)
  const termRef = useRef<XTerm | null>(null)
  const sockRef = useRef<WebSocket | null>(null)
  const sendRef = useRef<(data: string) => void>(() => {})

  const stateRef = useRef<PaneState>({ s: 'connecting', ptyId })
  const [state, setState] = useState<PaneState>(stateRef.current)
  const [selection, setSelection] = useState('')
  const [inserted, setInserted] = useState(false)
  const [menu, setMenu] = useState<MenuPos | null>(null)

  const apply = useCallback((ev: PaneEvent) => {
    const { state: ns, fx } = next(stateRef.current, ev)
    stateRef.current = ns
    for (const f of fx) {
      if (f.k === 'reset-term') termRef.current?.reset()
      if (f.k === 'close-socket') {
        sockRef.current?.close()
        sockRef.current = null
      }
    }
    setState(ns)
  }, [])

  const quote = useCallback(
    (raw: string) => {
      const text = formatSelection(raw).slice(0, 8 << 10)
      if (!text) return false
      emitUI('composer_quote', {
        text,
        source: { id: `term:${tabId}`, title: tRef.current('term.quoteSource') },
      })
      setInserted(true)
      window.setTimeout(() => setInserted(false), 800)
      return true
    },
    [tabId],
  )

  const quoteSelection = useCallback(
    () => quote(termRef.current?.getSelection() ?? ''),
    [quote],
  )

  const quoteScreen = useCallback(() => {
    const term = termRef.current
    if (!term) return false
    const buf = term.buffer.active
    const lines: string[] = []
    for (let i = 0; i < term.rows; i++) {
      lines.push(buf.getLine(buf.viewportY + i)?.translateToString(true) ?? '')
    }
    while (lines.length && lines[lines.length - 1].trim() === '') lines.pop()
    return quote(lines.join('\n'))
  }, [quote])

  const quoteSmart = useCallback(() => {
    if (!quoteSelection()) quoteScreen()
  }, [quoteSelection, quoteScreen])

  const openMenu = useCallback((e: React.MouseEvent) => {
    e.preventDefault()
    setMenu({ x: e.clientX, y: e.clientY })
  }, [])

  const paste = useCallback(async () => {
    try {
      const text = await navigator.clipboard.readText()
      if (text) termRef.current?.paste(text)
    } catch {
      /* 剪贴板不可用：静默 */
    }
  }, [])

  useEffect(() => {
    if (!menu) return
    const close = () => setMenu(null)
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setMenu(null)
    }
    window.addEventListener('mousedown', close)
    window.addEventListener('wheel', close, { passive: true })
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('mousedown', close)
      window.removeEventListener('wheel', close)
      window.removeEventListener('keydown', onKey)
    }
  }, [menu])

  useEffect(() => {
    const host = hostRef.current
    if (!host) return
    let disposed = false

    const term = new Terminal({
      // Nerd Font 优先（p10k 的 powerline/图标都在私用区）；没装则退等宽字体。
      fontFamily:
        "'MesloLGL Nerd Font Mono','Meslo LG L','JetBrains Mono','IBM Plex Mono',monospace",
      fontSize: 12,
      lineHeight: 1, // powerline 分隔符要上下无缝
      letterSpacing: 0,
      scrollback: 5e3,
      cursorBlink: true,
      allowProposedApi: true,
      theme: readTermTheme(),
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(host)
    termRef.current = term

    const writer = terminalWriter((data: Uint8Array, done: () => void) => term.write(data, done))

    const doFit = () => {
      if (!host.clientWidth || !host.clientHeight) return
      try {
        fit.fit()
      } catch {
        /* 容器尺寸为 0 时 fit 会抛，忽略 */
      }
    }
    doFit()
    void document.fonts?.ready?.then(() => {
      if (!disposed) doFit()
    })

    let timer: ReturnType<typeof setTimeout> | null = null
    const scheduleFit = () => {
      if (timer) clearTimeout(timer)
      timer = setTimeout(() => {
        timer = null
        if (disposed) return
        doFit()
        const sock = sockRef.current
        if (sock && sock.readyState === WebSocket.OPEN) {
          sock.send(encodeCtrl({ t: 'resize', cols: term.cols, rows: term.rows }))
        }
      }, RESIZE_DEBOUNCE_MS)
    }
    const ro = new ResizeObserver(scheduleFit)
    ro.observe(host)

    const offTheme = watchTheme(() => {
      term.options.theme = readTermTheme()
    })

    term.onData((d) => sendRef.current(d))
    term.onSelectionChange(() => setSelection(term.getSelection()))

    term.attachCustomKeyEventHandler((e) => {
      if (e.type !== 'keydown') return true
      const mod = e.metaKey || e.ctrlKey
      if (mod && e.shiftKey && e.key === 'Enter') {
        quoteSmart()
        return false
      }
      if (mod && !e.shiftKey && e.key.toLowerCase() === 'c') {
        if (term.getSelection()) return false // 有选区交给浏览器复制
        sendRef.current('\x03') // 无选区 = SIGINT
        return false
      }
      if (mod && !e.shiftKey && e.key.toLowerCase() === 'v') return false // 交给 paste 事件
      return true
    })

    const onKeyDown = (e: KeyboardEvent) => e.stopPropagation()
    host.addEventListener('keydown', onKeyDown)

    void (async () => {
      const r = await getTransport().ptyTicket(ptyId)
      if (disposed) return
      if (!r.ok || !r.url || !r.ticket) {
        apply({
          t: 'error',
          error: r.error ?? tRef.current('term.failed').replace('{error}', 'ticket'),
        })
        return
      }
      apply({ t: 'ticket', ticket: r.ticket })
      const sock = new WebSocket(`${r.url}?ticket=${encodeURIComponent(r.ticket)}`)
      sock.binaryType = 'arraybuffer'
      sockRef.current = sock
      sendRef.current = (d) => {
        if (sock.readyState === WebSocket.OPEN) sock.send(d)
      }
      sock.onopen = () => apply({ t: 'ws-open' })
      sock.onmessage = (ev) => {
        const { ctrl, data } = decodeFrame(ev.data)
        if (ctrl) {
          switch (ctrl.t) {
            case 'hello':
              apply({ t: 'hello' })
              break
            case 'reset':
              apply({ t: 'reset' })
              break
            case 'gap':
              term.writeln(`\x1b[2m${tRef.current('term.truncated')}\x1b[0m`)
              break
            case 'exit':
              apply({ t: 'exit', code: ctrl.code })
              break
            default:
              break
          }
          return
        }
        if (data && data.length) {
          writer.push(data)
          apply({ t: 'output' })
        }
      }
      sock.onerror = () => {
        if (!disposed) {
          apply({ t: 'error', error: tRef.current('term.failed').replace('{error}', 'socket') })
        }
      }
      sock.onclose = () => {
        if (disposed) return
        if (stateRef.current.s === 'exited') return
        apply({ t: 'error', error: tRef.current('term.failed').replace('{error}', 'closed') })
      }
    })()

    return () => {
      disposed = true
      if (timer) clearTimeout(timer)
      ro.disconnect()
      offTheme()
      host.removeEventListener('keydown', onKeyDown)
      writer.clear()
      sockRef.current?.close()
      sockRef.current = null
      term.dispose()
      termRef.current = null
    }
  }, [ptyId, apply, quoteSmart])

  const reopen = useCallback(() => {
    useAppStore.getState().termClose(tabId)
    useAppStore.getState().termEnsure(tabId)
  }, [tabId])

  const overlay =
    state.s === 'failed'
      ? { kind: 'failed' as const, text: state.error }
      : state.s === 'exited'
        ? { kind: 'exited' as const, text: t('term.exit').replace('{code}', String(state.code ?? '?')) }
        : state.s === 'live'
          ? null
          : {
              kind: 'loading' as const,
              text: state.s === 'replaying' ? t('term.replaying') : t('term.starting'),
            }

  return (
    <div
      className="term-pane"
      data-tab={tabId}
      data-state={state.s}
      onContextMenu={openMenu}
    >
      <div className="term-host" ref={hostRef} />

      {selection.trim() !== '' && state.s === 'live' && (
        <button type="button" className="term-insert" onClick={quoteSelection}>
          {inserted ? t('term.inserted') : t('term.insert')}
          <kbd>⌘⇧↵</kbd>
        </button>
      )}

      {menu && (
        <div
          className="term-menu"
          role="menu"
          style={{ left: menu.x, top: menu.y }}
          onMouseDown={(e) => e.stopPropagation()}
        >
          <button
            role="menuitem"
            disabled={!selection.trim()}
            onClick={() => {
              setMenu(null)
              quoteSelection()
            }}
          >
            {t('term.menu.selection')}
            <kbd>⌘⇧↵</kbd>
          </button>
          <button
            role="menuitem"
            onClick={() => {
              setMenu(null)
              quoteScreen()
            }}
          >
            {t('term.menu.screen')}
          </button>
          <div className="term-menu-sep" role="separator" />
          <button
            role="menuitem"
            disabled={!selection.trim()}
            onClick={() => {
              setMenu(null)
              void copyText(selection)
            }}
          >
            {t('term.menu.copy')}
          </button>
          <button
            role="menuitem"
            onClick={() => {
              setMenu(null)
              void paste()
            }}
          >
            {t('term.menu.paste')}
          </button>
          <button
            role="menuitem"
            onClick={() => {
              setMenu(null)
              termRef.current?.clear()
            }}
          >
            {t('term.menu.clear')}
          </button>
          <button
            role="menuitem"
            onClick={() => {
              setMenu(null)
              reopen()
            }}
          >
            {t('term.menu.reopen')}
          </button>
        </div>
      )}

      {overlay && (
        <div className={`term-overlay ${overlay.kind}`} role="status">
          <span className="term-overlay-text">{overlay.text}</span>
          {(overlay.kind === 'exited' || overlay.kind === 'failed') && (
            <button type="button" className="btn btn-xs" onClick={reopen}>
              {t('term.reopen')}
            </button>
          )}
        </div>
      )}
    </div>
  )
}
