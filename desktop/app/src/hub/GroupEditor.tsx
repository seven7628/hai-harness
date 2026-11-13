import { useEffect, useMemo, useRef, useState } from 'react'
import { useT } from '../i18n'
import AgentSigil from './AgentSigil'
import { useHubStore } from './useHubStore'

// 新建群组弹层（左栏「群组」页签的顶部 ＋ / 空态按钮）。
//
// 骨架与 AgentEditor 同一套：.hub-modal-scrim（role=dialog、点遮罩关闭）+ .hub-modal。
// 与资料页不同的是单列、高度自适应 —— 这里只有三个字段和一张成员多选表。
//
// 键盘：Esc 关闭（捕获阶段监听，不会被 Hub 自己的快捷键先吃掉）；Tab 按浏览器默认
// 顺序走。打开期间只锁背景滚动（与 AttachmentStrip 灯箱同一套做法），不接管其它
// 全局状态。
//
// 校验分两层：空名 / 无成员 / 重名在前端直接给原因（bridge 也校验，但弹层不该为一次
// 往返才显示错误）；其余（成员不存在、保存失败）以 store.error 为准，失败留在弹层。
export default function GroupEditor({ onClose }: { onClose: () => void }) {
  const t = useT()
  const agents = useHubStore((s) => s.agents)
  const groups = useHubStore((s) => s.groups)
  const createGroup = useHubStore((s) => s.createGroup)
  const storeError = useHubStore((s) => s.error)

  const [name, setName] = useState('')
  const [picked, setPicked] = useState<string[]>([])
  const [owner, setOwner] = useState('')
  const [mode, setMode] = useState<'ask' | 'auto'>('ask')
  const [busy, setBusy] = useState(false)
  const [localError, setLocalError] = useState<string | null>(null)
  // 只显示「这一次提交」写下的 store.error：Hub 里别的动作（发消息失败等）的文案
  // 不该在弹层刚打开时冒充群组创建错误。
  const [attempted, setAttempted] = useState(false)

  // onClose 常是内联箭头（每次渲染新引用）：放进 ref，Esc 的监听才能只挂一次。
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose
  // 打开前的焦点（触发按钮）：在**首次渲染**时抓，不能等 effect —— autoFocus 在 commit
  // 阶段就把焦点移进弹层了，effect 里再读只会拿到弹层自己的输入框。
  const restoreTo = useRef<Element | null>(typeof document === 'undefined' ? null : document.activeElement)

  useEffect(() => {
    // 弹层打开时锁背景滚动：否则滚轮/触控板会把后面的左栏/时间线滚走。
    const prevBodyOverflow = document.body.style.overflow
    // 先抓成局部变量：cleanup 里再读 ref 会拿到「之后」的值（react-hooks 也会警告）。
    const back = restoreTo.current
    document.body.style.overflow = 'hidden'
    const onKey = (e: KeyboardEvent): void => {
      if (e.key !== 'Escape') return
      e.stopPropagation()
      onCloseRef.current()
    }
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('keydown', onKey, true)
      document.body.style.overflow = prevBodyOverflow
      // 关闭后焦点回到触发元素（顶部 ＋ / 空态按钮）；它已经不在文档里就不硬拽。
      if (back instanceof HTMLElement && document.contains(back)) back.focus()
    }
  }, [])

  // 成员只列没删除的智能体；顺序沿用左栏（agents 已是展示顺序）。
  const roster = useMemo(() => agents.filter((a) => !a.deleted), [agents])
  const nameOf = (id: string): string => {
    const a = roster.find((x) => x.id === id)
    return a?.name || id
  }

  function toggle(id: string): void {
    const next = picked.includes(id) ? picked.filter((x) => x !== id) : [...picked, id]
    setPicked(next)
    // 群主必须始终是成员（bridge 的「唯一 owner」）：取消勾选当前群主时自动切到第一个。
    if (!next.includes(owner)) setOwner(next[0] ?? '')
    if (localError) setLocalError(null)
  }

  async function submit(): Promise<void> {
    if (busy) return
    const trimmed = name.trim()
    if (!trimmed) {
      setLocalError('请填写群名称')
      return
    }
    if (picked.length === 0) {
      setLocalError('至少选择 1 个成员')
      return
    }
    if (groups.some((g) => (g.name ?? '').trim() === trimmed)) {
      setLocalError('已有同名群组，换个名字')
      return
    }
    setLocalError(null)
    setAttempted(true)
    setBusy(true)
    const group = await createGroup({
      name: trimmed,
      member_ids: picked,
      owner_agent_id: owner || picked[0],
      policy: { mode },
    })
    setBusy(false)
    // 成功：store 已经打开新群（openGroup），这里只负责关掉弹层。
    // 失败：留在弹层，store.error 由下面的错误行显示，输入全部保留。
    if (group) onClose()
  }

  const error = localError ?? (attempted ? storeError : null)

  return (
    <div className="hub-modal-scrim" role="dialog" aria-modal="true" aria-labelledby="hub-group-modal-title" onClick={onClose}>
      <div className="hub-modal hub-group-modal" onClick={(e) => e.stopPropagation()}>
        <header className="hub-modal-head">
          <span className="hub-modal-title" id="hub-group-modal-title">新建群组</span>
          <button className="hub-link" onClick={onClose}>{t('hub.close')}</button>
        </header>

        <div className="hub-modal-body">
          <div className="hub-group-form">
            <label className="hub-group-field">
              <span>群名称</span>
              <input
                autoFocus
                value={name}
                maxLength={32}
                placeholder="例如：竞品研究"
                onChange={(e) => {
                  setName(e.target.value)
                  if (localError) setLocalError(null)
                }}
              />
            </label>

            <div className="hub-group-field">
              <span>成员<span className="hub-group-required">（至少选 1 人）</span></span>
              <div className="hub-group-members" role="group" aria-label="群成员">
                {roster.map((a) => {
                  const on = picked.includes(a.id)
                  return (
                    <label key={a.id} className={`hub-group-member${on ? ' on' : ''}`}>
                      <input type="checkbox" checked={on} onChange={() => toggle(a.id)} />
                      {/* 22px：只是选人，不画运行状态点（点在这个尺寸上会和勾选框抢位置）。 */}
                      <AgentSigil agentId={a.id} name={a.name} responsibility={a.role} size={22} status={false} />
                      <span className="hub-group-member-name">{a.name || t('hub.unnamed')}</span>
                      <span className="hub-group-member-role">{a.role || '—'}</span>
                    </label>
                  )
                })}
                {roster.length === 0 ? (
                  <p className="hub-group-empty">{t('hub.noAgents')}</p>
                ) : null}
              </div>
            </div>

            <label className="hub-group-field">
              <span>群主</span>
              <select value={owner} onChange={(e) => setOwner(e.target.value)} disabled={picked.length === 0}>
                {picked.length === 0 ? <option value="">先选成员</option> : null}
                {picked.map((id) => <option key={id} value={id}>{nameOf(id)}</option>)}
              </select>
            </label>

            <label className="hub-group-field">
              <span>协作模式</span>
              <select value={mode} onChange={(e) => setMode(e.target.value === 'auto' ? 'auto' : 'ask')}>
                <option value="ask">ask（推荐）</option>
                <option value="auto">auto</option>
              </select>
              <span className="hub-group-hint">
                {mode === 'ask'
                  ? 'ask：有副作用的动作先征询你，其余按成员职责自动分工。'
                  : 'auto：成员之间自动接龙，不逐步征询；随时可以停止。'}
              </span>
            </label>

            {error ? <div className="hub-error" role="alert">{error}</div> : null}
          </div>
        </div>

        <footer className="hub-modal-foot">
          <button className="hub-link" onClick={onClose} disabled={busy}>{t('hub.cancel')}</button>
          <button className="hub-primary" onClick={() => void submit()} disabled={busy}>
            {busy ? '创建中…' : '创建'}
          </button>
        </footer>
      </div>
    </div>
  )
}
