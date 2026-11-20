// [recovered] from /private/tmp/builtin-surfaces.test.cjs（模块标记 `// src/components/terminal/TerminalTab.tsx`）
// 回译（back-translation）：原文是 esbuild 的 `jsxDEV(...)` 编译形态，本文件按同一逻辑回译成
// TSX，**不改变行为**；补上真实 import（bundle 里这些符号是包内变量，源码里必须自己取）。
// 移除 @ts-nocheck（已被 tsc 覆盖）。
//
// 职责只有三件（把「进程生命周期」与「渲染」分开）：
//   ① 挂载时 termEnsure(tabId) —— 让 store 去 fork/复用 PTY；
//   ② 有 ptyId → 交给 TerminalPane（它只管连接与像素）；
//   ③ 没有 ptyId → starting 覆盖层；failed → 错误文案 + 重开按钮（用户能自己修）。
// 为什么不用 sessionStorage 之类记账：PTY 是进程级身份，页面刷新后必然重建，
// 唯一真相只能是 AppState.term（见 store 的 TermRuntime 注释）。
import { useEffect } from 'react'

import { useT } from '../../i18n'
import { useAppStore } from '../../store/useAppStore'
import { TerminalPane } from './TerminalPane'

export function TerminalTab({ tabId }: { tabId: string }) {
  const t = useT()
  const rt = useAppStore((s) => s.term[tabId])
  const termEnsure = useAppStore((s) => s.termEnsure)

  useEffect(() => {
    termEnsure(tabId)
  }, [tabId, termEnsure])

  if (rt?.ptyId) return <TerminalPane tabId={tabId} ptyId={rt.ptyId} />

  if (rt?.status === 'failed') {
    return (
      <div className="term-pane" data-state="failed">
        <div className="term-overlay failed" role="status">
          <span className="term-overlay-text">{t('term.failed').replace('{error}', rt.error ?? '')}</span>
          <button type="button" className="btn btn-xs" onClick={() => termEnsure(tabId)}>
            {t('term.reopen')}
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="term-pane" data-state="loading">
      <div className="term-overlay loading" role="status">
        <span className="term-overlay-text">{t('term.starting')}</span>
      </div>
    </div>
  )
}
