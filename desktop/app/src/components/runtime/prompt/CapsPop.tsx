import { useEffect, useMemo, useRef, useState } from 'react'
import { useAppStore } from '../../../store/useAppStore'
import { useT } from '../../../i18n'
import './caps.css'

/* 「能力」弹层 —— 设计 index.html:916-946 的 .pop.caps-pop：
   本轮可用 <N 项>，下面按类折叠：技能 / MCP / 记忆 / 钩子 / 子智能体，
   点一类展开；**只有技能行会插入文本**（点它把 `/名字 ` 写进输入框），其余是只读事实。

   数字都从真实数据来，不补零：
   - 技能 = 已启用的技能（不能用的那些是模型/工具集不满足 requires，不冒充「可用」）
   - MCP   = 已启用的服务器
   - 记忆  = memoryListing.files（没加载过就不显示这一类，而不是显示 0）
   - 钩子  = hooksInfo 里各源的 handler 数
   - 子智能体 = 当前视图里还在跑的 agent 块
   某一类没有数据就不画那一行 —— 空的「0 项」比不显示更会误导。 */


type HooksInfoLike = {
  user?: { body?: { events?: Record<string, { hooks: { name?: string; command?: string; type?: string }[] }[]> } }
  projects?: { body?: { events?: Record<string, { hooks: { name?: string; command?: string; type?: string }[] }[]> } }[]
}

/** 钩子 handler 的扁平列表（各源 × 各事件 × 各组） */
export const hookHandlersOf = (info?: HooksInfoLike) =>
  [info?.user, ...(info?.projects ?? [])]
    .flatMap((src) => Object.values(src?.body?.events ?? {}).flatMap((groups) => groups.flatMap((g) => g.hooks)))

/** 按钮上的那个数字 —— 与弹层里「N 项」必须同源，否则同一个按钮报两个数。
    口径：已启用技能 + 已启用 MCP + 记忆文件 + 钩子 handler + 正在跑的子智能体。
    没有数据的类不计入（弹层里也不显示那一行）。 */
export const capsCountOf = (s: {
  skills: { enabled: boolean }[]
  mcpServers: { enabled: boolean }[]
  memoryListing?: { files: unknown[] }
  hooksInfo?: HooksInfoLike
  activeSessionId?: string
  views: Record<string, { blocks?: { kind: string; status?: string }[] } | undefined>
}): number => {
  const v = s.activeSessionId ? s.views[s.activeSessionId] : undefined
  const agents = (v?.blocks ?? []).filter((b) => b.kind === 'agent' && (b.status === 'running' || b.status === 'interrupting')).length
  return s.skills.filter((x) => x.enabled).length
    + s.mcpServers.filter((m) => m.enabled).length
    + (s.memoryListing?.files?.length ?? 0)
    + hookHandlersOf(s.hooksInfo).length
    + agents
}

type Group = {
  key: string
  label: string
  n: number
  rows: React.ReactNode
}

export function CapsPop({ onInsertSkill, onClose }: {
  onInsertSkill: (cmd: string) => void
  onClose: () => void
}) {
  const t = useT()
  const skills = useAppStore((s) => s.skills)
  const mcp = useAppStore((s) => s.mcpServers)
  const hooksInfo = useAppStore((s) => s.hooksInfo)
  const memory = useAppStore((s) => s.memoryListing)
  const agents = useAppStore((s) => {
    const v = s.activeSessionId ? s.views[s.activeSessionId] : undefined
    return (v?.blocks ?? []).filter((b) => b.kind === 'agent' && (b.status === 'running' || b.status === 'interrupting'))
      .length
  })
  const fetchSkills = useAppStore((s) => s.fetchSkills)
  const fetchMCPServers = useAppStore((s) => s.fetchMCPServers)

  // 打开时刷新一次：这一层说的是「**本轮**可用」，拿旧清单等于报过期数
  useEffect(() => {
    fetchSkills()
    fetchMCPServers()
  }, [fetchSkills, fetchMCPServers])

  // 点外面关（与其它弹层同一套）
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onClose()
    }
    document.addEventListener('mousedown', onDoc)
    return () => document.removeEventListener('mousedown', onDoc)
  }, [onClose])

  const [open, setOpen] = useState<Record<string, boolean>>({ skills: true })
  const toggle = (k: string) => setOpen((m) => ({ ...m, [k]: !m[k] }))

  const groups: Group[] = useMemo(() => {
    const out: Group[] = []
    const usable = skills.filter((s) => s.enabled)
    if (usable.length) {
      out.push({
        key: 'skills',
        label: t('caps.skills'),
        n: usable.length,
        rows: (
          <>
            {usable.map((s) => (
              <button
                className="pop-row two"
                type="button"
                key={s.name}
                // 技能行是这一层里唯一会写东西进输入框的：写成 `/名字 `（带尾空格），
                // 于是斜杠补全不会抢着弹出来 —— 用户点的是「用它」，不是「再选一次」
                onClick={() => onInsertSkill(`/${s.name} `)}
              >
                <span className="k">{s.name}</span>
                <span className="m" />
                <small>{s.description || ''}</small>
              </button>
            ))}
          </>
        ),
      })
    }
    const mcps = mcp.filter((m) => m.enabled)
    if (mcps.length) {
      out.push({
        key: 'mcp',
        label: 'MCP',
        n: mcps.length,
        rows: (
          <>
            {mcps.map((m) => (
              <div className="pop-row" key={m.name}>
                <i className={`dot ${m.state === 'connected' ? 'ok' : m.state === 'error' ? 'warn' : ''}`} />
                <span className="k">{m.name}</span>
                <span className="m">{m.tool_count} {t('caps.tools')} · {m.type}</span>
              </div>
            ))}
          </>
        ),
      })
    }
    if (memory?.files?.length) {
      out.push({
        key: 'mem',
        label: t('caps.memory'),
        n: memory.files.length,
        rows: (
          <>
            {memory.files.slice(0, 12).map((f) => (
              <div className="pop-row" key={f.name}>
                <span className="k">{f.name}</span>
                <span className="m">{f.type ?? (f.indexed ? 'indexed' : '')}</span>
              </div>
            ))}
          </>
        ),
      })
    }
    // 钩子：各源的 handler 数之和；没有配置就没有这一类
    const hookList = hookHandlersOf(hooksInfo)
    if (hookList.length) {
      out.push({
        key: 'hooks',
        label: t('caps.hooks'),
        n: hookList.length,
        rows: (
          <>
            {hookList.slice(0, 12).map((h, i) => (
              <div className="pop-row" key={i}>
                <span className="k">{h.name || h.command || h.type || '—'}</span>
                <span className="m">{h.type ?? ''}</span>
              </div>
            ))}
          </>
        ),
      })
    }
    if (agents) {
      out.push({
        key: 'agents',
        label: t('caps.subagents'),
        n: agents,
        rows: (
          <p className="acc-note">{t('caps.subagentsNote').replace('{n}', String(agents))}</p>
        ),
      })
    }
    return out
  }, [skills, mcp, memory, hooksInfo, agents, t, onInsertSkill])

  const total = groups.reduce((n, g) => n + g.n, 0)

  return (
    <div className="pop caps-pop" data-pop="caps" ref={ref} role="dialog" aria-label={t('caps.aria')}>
      <div className="caps-head">
        <span>{t('caps.head')}</span>
        <b>{t('caps.count').replace('{n}', String(total))}</b>
      </div>
      {groups.map((g) => (
        <div key={g.key}>
          <button
            className="acc-row"
            type="button"
            aria-expanded={Boolean(open[g.key])}
            onClick={() => toggle(g.key)}
          >
            <i className={`acc-dot acc-dot-${g.key}`} />
            {g.label}
            <b>{g.n}</b>
            <svg className="chev" viewBox="0 0 12 12" aria-hidden="true">
              <path d="M2.6 4.2 6.8 8.6l4.2-4.4" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </button>
          {open[g.key] ? <div className="acc-body">{g.rows}</div> : null}
        </div>
      ))}
      <p className="acc-note">{t('caps.note')}</p>
    </div>
  )
}
