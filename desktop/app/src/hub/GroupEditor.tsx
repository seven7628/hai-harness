import { useEffect, useMemo, useRef, useState } from 'react'
import { useT } from '../i18n'
import AgentSigil from './AgentSigil'
import { asGroupMode, GROUP_MODE_HINT, GROUP_MODES, type GroupMode } from './groupMode'
import { useHubStore } from './useHubStore'
import type { HubGroup } from './types'

// 群组弹层：新建 + 编辑共用一份（P6-17）。
//
// 骨架与 AgentEditor 同一套：.hub-modal-scrim（role=dialog、点遮罩关闭）+ .hub-modal。
// 与资料页不同的是单列、高度自适应 —— 群名/成员/群主/协作模式四个字段。
//
// 编辑模式（传 group）：成员表的勾选是**增删的唯一入口** ——
//   · 原本在群里、取消勾选 = 移除（群主不可取消，先转让）；
//   · 原本不在群里、勾上 = 添加；
//   · 保存时按 diff 走 hub_group_member_add/remove + hub_group_update（store.saveGroupEdit）。
// 这样「加人/移人/转让」三个动作在一张表里完成，不需要三套子界面。
//
// 键盘：Esc 关闭（捕获阶段监听，不会被 Hub 自己的快捷键先吃掉）；Tab 按浏览器默认
// 顺序走。打开期间只锁背景滚动（与 AttachmentStrip 灯箱同一套做法），不接管其它
// 全局状态。
//
// 校验分两层：空名 / 无成员 / 重名 / 超 12 人 / owner 被移除在前端直接给原因
//（bridge 也校验，但弹层不该为一次往返才显示错误）；其余以 store.error 为准。
const MAX_GROUP_MEMBERS = 12

export default function GroupEditor({ onClose, group }: { onClose: () => void; group?: HubGroup | null }) {
  const t = useT()
  const agents = useHubStore((s) => s.agents)
  const groups = useHubStore((s) => s.groups)
  const createGroup = useHubStore((s) => s.createGroup)
  const saveGroupEdit = useHubStore((s) => s.saveGroupEdit)
  const storeError = useHubStore((s) => s.error)

  const editing = !!group
  const originalMembers = useMemo(
    () => (group?.members ?? []).map((m) => String(m.agent_id)),
    [group],
  )
  const originalOwner = String(group?.owner_agent_id ?? '')

  const [name, setName] = useState(() => (group?.name ?? '').trim())
  const [picked, setPicked] = useState<string[]>(() => (editing ? originalMembers : []))
  const [owner, setOwner] = useState(() => (editing ? originalOwner : ''))
  // 三种协作模式都必须可选：bridge 侧 P6-06 把 mode 真接到了工具执行
  // （readonly = 只读闸门，ask = 非只读工具逐个审批，auto = 不加群级审批）。
  // 这里少给一个值，就等于把「readonly 群」藏起来 —— UI 与实际行为必须同一份事实。
  const [mode, setMode] = useState<GroupMode>(() => asGroupMode(group?.policy?.mode))
  const [busy, setBusy] = useState(false)
  const [localError, setLocalError] = useState<string | null>(null)
  // 只显示「这一次提交」写下的 store.error：Hub 里别的动作（发消息失败等）的文案
  // 不该在弹层刚打开时冒充群组保存错误。
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
      // 关闭后焦点回到触发元素（顶部 ＋ / 空态按钮 / inspector 的编辑按钮）；
      // 它已经不在文档里就不硬拽。
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
    // 群主不能从成员表里去掉：先转让群主（bridge 会拒绝「移除 owner」，这里提前挡住）。
    if (editing && id === owner && picked.includes(id)) {
      setLocalError('群主不能移除：先在「群主」里转让给别人')
      return
    }
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
    if (picked.length > MAX_GROUP_MEMBERS) {
      setLocalError(`群成员上限 ${MAX_GROUP_MEMBERS} 人（当前选了 ${picked.length} 人）`)
      return
    }
    if (groups.some((g) => g.id !== group?.id && (g.name ?? '').trim() === trimmed)) {
      setLocalError('已有同名群组，换个名字')
      return
    }
    if (owner && !picked.includes(owner)) {
      setLocalError('群主必须是群成员')
      return
    }
    setLocalError(null)
    setAttempted(true)
    setBusy(true)
    if (!group) {
      const created = await createGroup({
        name: trimmed,
        member_ids: picked,
        owner_agent_id: owner || picked[0],
        policy: { mode },
      })
      setBusy(false)
      // 成功：store 已经打开新群（openGroup），这里只负责关掉弹层。
      // 失败：留在弹层，store.error 由下面的错误行显示，输入全部保留。
      if (created) onClose()
      return
    }
    const add = picked.filter((id) => !originalMembers.includes(id))
    const remove = originalMembers.filter((id) => !picked.includes(id))
    const saved = await saveGroupEdit({
      group_id: group.id,
      name: trimmed,
      owner_agent_id: owner || picked[0],
      mode,
      add,
      remove,
    })
    setBusy(false)
    if (saved) onClose()
  }

  const error = localError ?? (attempted ? storeError : null)
  const title = editing ? '编辑群组' : '新建群组'

  return (
    <div className="hub-modal-scrim" role="dialog" aria-modal="true" aria-labelledby="hub-group-modal-title" onClick={onClose}>
      <div className="hub-modal hub-group-modal" onClick={(e) => e.stopPropagation()}>
        <header className="hub-modal-head">
          <span className="hub-modal-title" id="hub-group-modal-title">{title}</span>
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
              <span>
                成员
                <span className="hub-group-required">
                  （至少 1 人，最多 {MAX_GROUP_MEMBERS} 人；已选 {picked.length}）
                </span>
              </span>
              <div className="hub-group-members" role="group" aria-label="群成员">
                {roster.map((a) => {
                  const on = picked.includes(a.id)
                  const isOwner = on && a.id === owner
                  return (
                    <label key={a.id} className={`hub-group-member${on ? ' on' : ''}`}>
                      <input
                        type="checkbox"
                        checked={on}
                        disabled={isOwner && editing}
                        title={isOwner && editing ? '群主不能移除：先在「群主」里转让' : undefined}
                        onChange={() => toggle(a.id)}
                      />
                      {/* 22px：只是选人，不画运行状态点（点在这个尺寸上会和勾选框抢位置）。 */}
                      <AgentSigil agentId={a.id} name={a.name} responsibility={a.role} size={22} status={false} />
                      <span className="hub-group-member-name">{a.name || t('hub.unnamed')}</span>
                      <span className="hub-group-member-role">{a.role || '—'}</span>
                      {on && editing ? (
                        <span className={`hub-chip${isOwner ? ' owner' : ''}`}>{isOwner ? '群主' : '成员'}</span>
                      ) : null}
                      {on && editing && !isOwner ? (
                        <button
                          type="button"
                          className="hub-group-member-remove"
                          aria-label={`移除 ${a.name || a.id}`}
                          title="移除成员"
                          onClick={(e) => {
                            e.preventDefault()
                            e.stopPropagation()
                            toggle(a.id)
                          }}
                        >
                          ×
                        </button>
                      ) : null}
                    </label>
                  )
                })}
                {roster.length === 0 ? (
                  <p className="hub-group-empty">{t('hub.noAgents')}</p>
                ) : null}
              </div>
              {editing ? (
                <span className="hub-group-hint">取消勾选 = 从群里移除；群主需先转让（群主那一行的勾选框已锁定）。</span>
              ) : null}
            </div>

            <label className="hub-group-field">
              <span>群主</span>
              <select value={owner} onChange={(e) => setOwner(e.target.value)} disabled={picked.length === 0}>
                {picked.length === 0 ? <option value="">先选成员</option> : null}
                {picked.map((id) => <option key={id} value={id}>{nameOf(id)}</option>)}
              </select>
              {editing ? <span className="hub-group-hint">转让群主：选一个新群主，保存后旧群主变普通成员。</span> : null}
            </label>

            <label className="hub-group-field">
              <span>协作模式</span>
              <select value={mode} onChange={(e) => setMode(asGroupMode(e.target.value))}>
                {GROUP_MODES.map((m) => (
                  <option key={m} value={m}>{m === 'ask' ? 'ask（推荐）' : m}</option>
                ))}
              </select>
              <span className="hub-group-hint">{GROUP_MODE_HINT[mode]}</span>
            </label>

            {error ? <div className="hub-error" role="alert">{error}</div> : null}
          </div>
        </div>

        <footer className="hub-modal-foot">
          <button className="hub-link" onClick={onClose} disabled={busy}>{t('hub.cancel')}</button>
          <button className="hub-primary" onClick={() => void submit()} disabled={busy}>
            {busy ? (editing ? '保存中…' : '创建中…') : (editing ? '保存' : '创建')}
          </button>
        </footer>
      </div>
    </div>
  )
}
