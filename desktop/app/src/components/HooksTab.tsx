import { useEffect, useMemo, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import type {
  HookClaudeFile,
  HookGroupView,
  HookHandlerView,
  HookProjectView,
  HookSectionView,
  HookSourceView,
  HooksInfo,
} from '../store/useAppStore'
import { useT } from '../i18n'
import { FolderIcon, Gear, HookIcon } from './icons'
import { DetailHead, FlagRow, SourceCard, Switch } from './SettingsGroups'
import Sel from './Sel'

// ─── 钩子设置页（2026-09-21 新增：全局 × 项目两级 + 二级详情面板）───
//
// hooks 能力在 `go-code/hooks`（配置 + 外部命令执行器 + 适配层），本轮补管理面：
//   · 一级分组卡片：「来自配置」（用户配置 / Claude 兼容层只读）+「来自项目配置文件」（每个已绑定项目）；
//   · 二级详情：按事件分组的钩子列表（展开看 命令/超时/matcher/异步/失败模式），
//     单条启停 + 增删改；用户配置另有全局开关（总闸 / 允许项目层 / 兼容层 / 强制信任）。
// 写盘：用户级 → ~/.go-code/settings.json 的 hooks 段；项目级 → {ws}/.go-code/hooks.json（bridge hooks_set）。
// 生效：下一次 Run（Session.SetHooks 热替换）；信任与总闸状态在每条钩子上显式标注，不静默隐藏。
// 契约详见 docs/MCP_HOOKS_SETTINGS_PANEL_2026-09-21.md。

// 事件固定顺序（会话开始 → 用户输入 → 工具 → 压缩 → 回合结束）：与 hooks.Anchors 一致。
const HOOK_EVENTS = ['SessionStart', 'UserPromptSubmit', 'PreToolUse', 'PostToolUse', 'PostToolBatch', 'PreCompact', 'Stop'] as const

// hookCount 一个 hooks 段里的钩子总数（分组卡片副标题「N 个钩子」）。
function hookCount(body?: HookSectionView): number {
  let n = 0
  for (const groups of Object.values(body?.events ?? {})) {
    for (const g of groups ?? []) n += (g.hooks ?? []).length
  }
  return n
}

// commandsOf 一个 hooks 段里出现的全部命令（信任状态查询/批量信任用）。
function commandsOf(body?: HookSectionView): string[] {
  const out: string[] = []
  for (const groups of Object.values(body?.events ?? {})) {
    for (const g of groups ?? []) {
      for (const h of g.hooks ?? []) if (h.command) out.push(h.command)
    }
  }
  return out
}

// handlerState 单条钩子「是否真的会执行」+ 未执行原因（契约 §4；规则写死在此，UI 与文档一致）。
// scopeKind=user：用户自己写的配置默认可信（requireTrust 例外）；project：还要 allowProjectHooks + 已信任。
function handlerState(
  h: HookHandlerView,
  scopeKind: 'user' | 'project',
  eff: HooksInfo['effective'],
  trust: HooksInfo['trust'],
): { on: boolean; reason?: string } {
  if (h.enabled === false) return { on: false, reason: 'hook.why.disabled' }
  if (eff.disableAllHooks) return { on: false, reason: 'hook.why.disableAll' }
  const st = trust[h.command ?? ''] ?? 'untrusted'
  const trusted = st === 'trusted' || st === 'managed'
  if (scopeKind === 'project') {
    if (!eff.allowProjectHooks) return { on: false, reason: 'hook.why.notAllowed' }
    if (!trusted) return { on: false, reason: st === 'modified' ? 'hook.why.modified' : 'hook.why.untrusted' }
    return { on: true }
  }
  if (eff.requireTrust && !trusted) return { on: false, reason: 'hook.why.untrusted' }
  return { on: true }
}

export function HooksTab() {
  const t = useT()
  const projects = useAppStore((s) => s.projects)
  const info = useAppStore((s) => s.hooksInfo)
  const loading = useAppStore((s) => s.hooksLoading)
  const fetchHooks = useAppStore((s) => s.fetchHooks)
  const [sel, setSel] = useState<string | null>(null) // null=列表；'user'；'claude-user'；项目路径

  const paths = useMemo(() => projects.map((p) => p.path), [projects])
  const pathsKey = paths.join('\u0000')
  useEffect(() => {
    fetchHooks(paths)
    // paths 用拼接后的 key 作依赖：数组引用每轮渲染都变，直接用会死循环
  }, [fetchHooks, pathsKey]) // eslint-disable-line react-hooks/exhaustive-deps

  // 项目：已绑定（paths 即绑定列表）+ 仍存在；已删除项目的配置不展示
  const projList = (info?.projects ?? []).filter((p) => p.exists !== false && paths.includes(p.workspace ?? ''))

  if (info && sel === 'user') {
    return <HooksDetail src={info.user} info={info} scopeKind="user" editable rev={info.at ?? 0} onBack={() => setSel(null)} />
  }
  if (info && sel === 'claude-user' && info.claudeUser) {
    return <HooksDetail src={info.claudeUser} info={info} scopeKind="user" editable={false} rev={info.at ?? 0} onBack={() => setSel(null)} />
  }
  if (info && sel) {
    const p = projList.find((x) => x.workspace === sel)
    if (p) return <HooksDetail src={p} info={info} scopeKind="project" editable rev={info.at ?? 0} onBack={() => setSel(null)} />
  }

  return (
    <>
      <div className="settings-sec-title">{t('settings.hooks')}</div>
      <p className="set-desc" style={{ marginBottom: 6 }}>{t('hook.hint')}</p>

      {!info ? (
        <p className="set-placeholder">{loading ? t('hook.loading') : t('hook.empty')}</p>
      ) : (
        <>
          <div className="src-sec">{t('hook.sec.config')}</div>
          <SourceCard
            icon={<Gear size={14} />}
            title={t('hook.user.title')}
            meta={`${t('hook.count').replace('{n}', String(hookCount(info.user.body)))} · ${info.user.path}`}
            onClick={() => setSel('user')}
          />
          {info.claudeUser?.present && (
            <SourceCard
              icon={<Gear size={14} />}
              title={t('hook.claude.title')}
              meta={`${t('hook.count').replace('{n}', String(hookCount(info.claudeUser.body)))} · ${info.claudeUser.path}`}
              badge={t('hook.readonly')}
              onClick={() => setSel('claude-user')}
            />
          )}

          <div className="src-sec">{t('hook.sec.project')}</div>
          {projList.length === 0 && <p className="set-placeholder">{t('mcp.noProject')}</p>}
          {projList.map((p) => {
            const n = hookCount(p.body) + (p.claude ?? []).reduce((acc, c) => acc + hookCount(c.body), 0)
            const untrusted = commandsOf(p.body).filter((c) => (info.trust[c] ?? 'untrusted') === 'untrusted').length
            return (
              <SourceCard
                key={p.workspace}
                icon={<FolderIcon size={14} />}
                title={p.title}
                meta={`${t('hook.count').replace('{n}', String(n))} · ${p.workspace ?? ''}`}
                badge={
                  !(info.effective.allowProjectHooks || Boolean(p.body?.allowProjectHooks))
                    ? t('hook.badge.off')
                    : untrusted > 0
                      ? t('hook.badge.untrusted')
                      : undefined
                }
                onClick={() => setSel(p.workspace ?? '')}
              />
            )
          })}
          <p className="set-desc" style={{ marginTop: 8 }}>{t('hook.project.note')}</p>
        </>
      )}
    </>
  )
}

// HooksDetail 二级详情面板：一个来源（用户配置 / Claude 兼容层 / 某项目）的全部钩子。
function HooksDetail({
  src,
  info,
  scopeKind,
  editable,
  rev,
  onBack,
}: {
  src: HookSourceView
  info: HooksInfo
  scopeKind: 'user' | 'project'
  editable: boolean
  rev: number
  onBack: () => void
}) {
  const t = useT()
  const saveHooks = useAppStore((s) => s.saveHooks)
  const trustHooks = useAppStore((s) => s.trustHooks)
  const saving = useAppStore((s) => s.hooksSaving)
  const workspace = scopeKind === 'project' ? src.workspace : undefined
  // 本地可编辑副本：保存成功后 store 回拉（hooksInfo 变化）→ 以磁盘为准重同步
  const [body, setBody] = useState<HookSectionView>(src.body ?? {})
  const [err, setErr] = useState<string | null>(null)
  const [open, setOpen] = useState<string | null>(null) // 展开的钩子（`ev#gi#hi`）
  const [form, setForm] = useState<HookForm | null>(null)
  const claudeFiles: HookClaudeFile[] = (src as HookProjectView).claude ?? []

  // 回拉（rev 变化）时把本地副本重同步为磁盘态：只重置数据，不动展开/表单这些临时 UI 态
  // （否则「切换开关」这类即时保存会把正在展开的条目收起来）。
  useEffect(() => {
    setBody(src.body ?? {})
  }, [src.path, src.workspace, rev]) // eslint-disable-line react-hooks/exhaustive-deps

  // commit 改一处 → 立即写盘（写盘即热应用：下一次 Run 生效）；失败保留本地态并提示
  const commit = async (next: HookSectionView) => {
    setBody(next)
    if (!editable) return
    const r = await saveHooks(scopeKind === 'project' ? 'project' : 'user', workspace, next)
    setErr(r.ok ? null : (r.error ?? t('hook.errSave')))
  }

  const groupsOf = (ev: string): HookGroupView[] => body.events?.[ev] ?? []
  const setGroups = (ev: string, groups: HookGroupView[]): HookSectionView => {
    const events = { ...(body.events ?? {}) }
    const pruned = groups.filter((g) => (g.hooks ?? []).length > 0)
    if (pruned.length) events[ev] = pruned
    else delete events[ev]
    return { ...body, events }
  }
  const patchHandler = (ev: string, gi: number, hi: number, patch: Partial<HookHandlerView>): HookSectionView => {
    const groups = groupsOf(ev).map((g, i) =>
      i === gi ? { ...g, hooks: (g.hooks ?? []).map((h, j) => (j === hi ? { ...h, ...patch } : h)) } : g,
    )
    return setGroups(ev, groups)
  }
  const removeHandler = (ev: string, gi: number, hi: number): HookSectionView => {
    const groups = groupsOf(ev).map((g, i) => (i === gi ? { ...g, hooks: (g.hooks ?? []).filter((_, j) => j !== hi) } : g))
    return setGroups(ev, groups)
  }
  const submitForm = () => {
    if (!form) return
    const command = form.command.trim()
    if (!command) {
      setErr(t('hook.errCommand'))
      return
    }
    const timeout = form.timeout.trim() ? Number(form.timeout) : 0
    if (!Number.isFinite(timeout) || timeout < 0) {
      setErr(t('hook.errTimeout'))
      return
    }
    const next: HookHandlerView = {
      type: 'command',
      ...(form.name.trim() ? { name: form.name.trim() } : {}),
      command,
      ...(timeout ? { timeout } : {}),
      ...(form.async ? { async: true } : {}),
      ...(form.failureMode !== 'fail_open' ? { failureMode: form.failureMode } : {}),
    }
    const matcher = form.matcher.trim()
    if (form.gi == null || form.hi == null) {
      // 新增：同名 matcher 的组直接追加，避免每加一条就多一个空壳组
      const groups = groupsOf(form.ev).map((g) => ({ ...g }))
      const hit = groups.findIndex((g) => (g.matcher ?? '').trim() === matcher)
      if (hit >= 0) groups[hit].hooks = [...(groups[hit].hooks ?? []), next]
      else groups.push({ ...(matcher ? { matcher } : {}), hooks: [next] })
      void commit(setGroups(form.ev, groups))
    } else {
      const gi = form.gi
      const hi = form.hi
      const prev = groupsOf(form.ev)[gi]?.hooks?.[hi] ?? {}
      // 编辑：保留面板不认识的字段（statusMessage/additionalContextLimit/enabled 等）
      const merged: HookHandlerView = { ...prev, ...next }
      if (prev.enabled !== undefined) merged.enabled = prev.enabled
      const groups = groupsOf(form.ev).map((g, i) => (i === gi ? { ...g, matcher: matcher || undefined, hooks: (g.hooks ?? []).map((h, j) => (j === hi ? merged : h)) } : g))
      void commit(setGroups(form.ev, groups))
    }
    setForm(null)
  }

  const trust = info.trust
  const untrustedCmds = commandsOf(body).filter((c) => {
    const st = trust[c] ?? 'untrusted'
    return st === 'untrusted' || st === 'modified'
  })
  // 项目层是否被允许加载 = 用户层开关 ∨ 该项目文件自带的 allowProjectHooks。
  // effective 是响应级单值（工作区无关的一次 Load），项目文件里的自举开关只有本页知道；
  // 少报会让人以为"配了却不跑"，多报更糟（显示会跑其实不跑）—— 这里取两者的并集，与运行期
  // hooks.Load 的 peekAllowProject 自举判定一致。
  const projectAllowed = info.effective.allowProjectHooks || Boolean(body.allowProjectHooks)
  const evOptions = HOOK_EVENTS.map((ev) => ({ value: ev, label: ev, hint: t(`hook.evdesc.${ev}`) }))
  const failOptions = [
    { value: 'fail_open', label: 'fail_open', hint: t('hook.fail.open.hint') },
    { value: 'fail_closed', label: 'fail_closed', hint: t('hook.fail.closed.hint') },
  ]

  return (
    <>
      <DetailHead title={src.title} sub={src.path} onBack={onBack}>
        {saving ? <span className="set-desc">…</span> : null}
      </DetailHead>
      <p className="set-desc">
        {scopeKind === 'project' ? t('hook.project.desc') : t('hook.user.desc')}
      </p>

      {!src.ok && <p className="nwf-err mono" role="alert">{src.error ?? t('hook.parseErr')}</p>}
      {!!src.unknownKeys?.length && (
        <p className="hook-note warn">{t('hook.unknownKeys').replace('{k}', src.unknownKeys.join('、'))}</p>
      )}
      {scopeKind === 'project' && !projectAllowed && (
        <p className="hook-note warn">{t('hook.allow.hint')}</p>
      )}
      {scopeKind === 'project' && untrustedCmds.length > 0 && (
        <div className="hook-note">
          <span>{t('hook.trust.hint').replace('{n}', String(untrustedCmds.length))}</span>
          {editable && (
            <button className="btn primary" onClick={() => void trustHooks(untrustedCmds, 'project')}>
              {t('hook.trust.action')}
            </button>
          )}
        </div>
      )}
      {err && <p className="nwf-err" role="alert">{err}</p>}

      {/* 全局开关（只出现在用户配置：它们是用户层的段级字段，项目层同名开关另有 allowProjectHooks 语义） */}
      {scopeKind === 'user' && (
        <>
          <div className="src-sec">{t('hook.flags.title')}</div>
          <div className="hook-flags">
            <FlagRow
              label={t('hook.flag.disableAll')}
              desc={t('hook.flag.disableAll.desc')}
              on={Boolean(body.disableAllHooks)}
              disabled={!editable}
              onChange={(v) => void commit({ ...body, disableAllHooks: v })}
            />
            <FlagRow
              label={t('hook.flag.allowProject')}
              desc={t('hook.flag.allowProject.desc')}
              on={Boolean(body.allowProjectHooks)}
              disabled={!editable}
              onChange={(v) => void commit({ ...body, allowProjectHooks: v })}
            />
            <FlagRow
              label={t('hook.flag.readClaude')}
              desc={t('hook.flag.readClaude.desc')}
              on={body.readClaudeSettings !== false}
              disabled={!editable}
              onChange={(v) => void commit({ ...body, readClaudeSettings: v })}
            />
            <FlagRow
              label={t('hook.flag.requireTrust')}
              desc={t('hook.flag.requireTrust.desc')}
              on={Boolean(body.requireTrust)}
              disabled={!editable}
              onChange={(v) => void commit({ ...body, requireTrust: v })}
            />
          </div>
        </>
      )}

      <div className="src-sec">{t('hook.list.title')}</div>
      {editable && (
        <div className="mcp-head">
          <button className="btn primary" onClick={() => setForm({ ev: 'PreToolUse', gi: null, hi: null, matcher: '', name: '', command: '', timeout: '', async: false, failureMode: 'fail_open' })}>
            {t('hook.add')}
          </button>
        </div>
      )}
      {form && (
        <div className="mcp-form mcp-form-add">
          <div className="set-row">
            <div className="set-label">{t('hook.event')}</div>
            <Sel value={form.ev} options={evOptions} onChange={(v) => setForm((f) => (f ? { ...f, ev: v } : f))} />
          </div>
          <div className="set-row">
            <div className="set-label">{t('hook.matcher')}</div>
            <input
              className="prov-input" value={form.matcher} spellCheck={false}
              placeholder={t('hook.matcher.ph.' + form.ev)}
              aria-label="hook matcher"
              onChange={(e) => setForm((f) => (f ? { ...f, matcher: e.target.value } : f))}
            />
          </div>
          <div className="set-row">
            <div className="set-label">{t('hook.command')}</div>
            <input
              className="prov-input" value={form.command} spellCheck={false}
              placeholder="/path/to/hook.sh"
              aria-label="hook command"
              onChange={(e) => setForm((f) => (f ? { ...f, command: e.target.value } : f))}
            />
          </div>
          <div className="set-row">
            <div className="set-label">{t('hook.name')}</div>
            <input
              className="prov-input" value={form.name} spellCheck={false}
              placeholder={t('hook.name.ph')}
              aria-label="hook name"
              onChange={(e) => setForm((f) => (f ? { ...f, name: e.target.value } : f))}
            />
          </div>
          <div className="set-row">
            <div className="set-label">{t('hook.timeout')}</div>
            <input
              className="prov-input" value={form.timeout} spellCheck={false} inputMode="numeric"
              placeholder={t('hook.timeout.ph')}
              aria-label="hook timeout"
              onChange={(e) => setForm((f) => (f ? { ...f, timeout: e.target.value.replace(/[^0-9.]/g, '') } : f))}
            />
          </div>
          <div className="set-row">
            <div className="set-label">{t('hook.failureMode')}</div>
            <Sel value={form.failureMode} options={failOptions} onChange={(v) => setForm((f) => (f ? { ...f, failureMode: v } : f))} />
          </div>
          <FlagRow
            label={t('hook.async')}
            desc={t('hook.async.desc')}
            on={form.async}
            onChange={(v) => setForm((f) => (f ? { ...f, async: v } : f))}
          />
          <div className="mcp-form-btns">
            <button className="btn primary" onClick={submitForm}>{t('mcp.save')}</button>
            <button className="btn" onClick={() => setForm(null)}>{t('mcp.cancel')}</button>
          </div>
        </div>
      )}

      {HOOK_EVENTS.filter((ev) => groupsOf(ev).length > 0).length === 0 && !form && (
        <p className="set-placeholder">{t('hook.empty2')}</p>
      )}
      {HOOK_EVENTS.map((ev) => {
        const groups = groupsOf(ev)
        if (groups.length === 0) return null
        return (
          <div className="hook-group" key={ev}>
            <div className="hook-group-h">
              <span className="hook-group-ic" aria-hidden><HookIcon size={13} /></span>
              <div className="hook-group-title-box">
                <div className="hook-group-title">{ev}</div>
                <div className="hook-group-sub">{t(`hook.evdesc.${ev}`)}</div>
              </div>
            </div>
            {groups.map((g, gi) => (
              <div className="hook-matcher-box" key={gi}>
                {(g.matcher ?? '').trim() !== '' && (
                  <div className="hook-matcher">
                    {t('hook.matcher')}: <span className="mono">{g.matcher}</span>
                  </div>
                )}
                {(g.hooks ?? []).map((h, hi) => {
                  const key = `${ev}#${gi}#${hi}`
                  const st = handlerState(h, scopeKind, info.effective, trust)
                  const label = h.name?.trim() || t('hook.item').replace('{n}', String(hi + 1))
                  const timeout = h.timeout ? (h.timeout >= 1000 ? `${h.timeout} ms` : `${h.timeout} ${t('hook.seconds')}`) : t('hook.timeout.default')
                  return (
                    <div className={`hook-item ${st.on ? '' : 'off'}`} key={key}>
                      <div className="hook-item-h">
                        <button type="button" className="hook-item-name" onClick={() => setOpen(open === key ? null : key)} title={h.command}>
                          {label}
                        </button>
                        {!st.on && st.reason && <span className="hook-badge" title={t(st.reason)}>{t(st.reason)}</span>}
                        <span className="grow" />
                        {scopeKind === 'project' && !st.on && (trust[h.command ?? ''] ?? 'untrusted') !== 'trusted' && editable && h.command && (
                          <button className="btn" onClick={() => void trustHooks([h.command as string], 'project')}>{t('hook.trust.one')}</button>
                        )}
                        {editable && (
                          <>
                            <button
                              className="btn"
                              onClick={() => setForm({ ev, gi, hi, matcher: g.matcher ?? '', name: h.name ?? '', command: h.command ?? '', timeout: h.timeout ? String(h.timeout) : '', async: Boolean(h.async), failureMode: h.failureMode ?? 'fail_open' })}
                            >
                              {t('mcp.edit')}
                            </button>
                            <button className="btn" onClick={() => void commit(removeHandler(ev, gi, hi))}>{t('mcp.remove')}</button>
                            <Switch on={h.enabled !== false} onChange={(v) => void commit(patchHandler(ev, gi, hi, { enabled: v }))} label={label} />
                          </>
                        )}
                        {!editable && <span className="mcp-enabled">{t('hook.readonly')}</span>}
                      </div>
                      {open === key && (
                        <div className="hook-item-body">
                          <div className="hook-kv">
                            <span className="hook-k">{t('hook.handler')}</span>
                            <span className="hook-v mono">{h.type ?? 'command'}</span>
                          </div>
                          <div className="hook-kv">
                            <span className="hook-k">{t('hook.command')}</span>
                            <span className="hook-v mono">{h.command ?? '—'}</span>
                          </div>
                          <div className="hook-kv">
                            <span className="hook-k">{t('hook.timeout')}</span>
                            <span className="hook-v">{timeout}</span>
                          </div>
                          {g.matcher && (
                            <div className="hook-kv">
                              <span className="hook-k">{t('hook.matcher')}</span>
                              <span className="hook-v mono">{g.matcher}</span>
                            </div>
                          )}
                          {h.async && (
                            <div className="hook-kv">
                              <span className="hook-k">{t('hook.async')}</span>
                              <span className="hook-v">{t('settings.on')}</span>
                            </div>
                          )}
                          {h.failureMode && h.failureMode !== 'fail_open' && (
                            <div className="hook-kv">
                              <span className="hook-k">{t('hook.failureMode')}</span>
                              <span className="hook-v mono">{h.failureMode}</span>
                            </div>
                          )}
                          {h.command && (
                            <div className="hook-kv">
                              <span className="hook-k">{t('hook.trust')}</span>
                              <span className="hook-v">{t(`hook.trust.${trust[h.command] ?? 'untrusted'}`)}</span>
                            </div>
                          )}
                        </div>
                      )}
                    </div>
                  )
                })}
              </div>
            ))}
          </div>
        )
      })}

      {/* 项目的 Claude 兼容层（只读；hooks.Load 会读 {ws}/.claude/settings.json） */}
      {claudeFiles.filter((c) => c.present).map((c) => {
        const groups = Object.entries(c.body?.events ?? {})
        return (
          <div className="hook-group readonly" key={c.path}>
            <div className="hook-group-h">
              <span className="hook-group-ic" aria-hidden><HookIcon size={13} /></span>
              <div className="hook-group-title-box">
                <div className="hook-group-title">{t('hook.claude.title')}</div>
                <div className="hook-group-sub mono" title={c.path}>{c.path}</div>
              </div>
            </div>
            {groups.length === 0 && <p className="set-placeholder">{t('hook.empty2')}</p>}
            {groups.map(([ev, gs]) => (
              <div className="hook-matcher-box" key={ev}>
                <div className="hook-matcher">{ev}</div>
                {gs.map((g, gi) => (
                  <div key={gi}>
                    {(g.hooks ?? []).map((h, hi) => (
                      <div className="hook-item off" key={hi}>
                        <div className="hook-item-h">
                          <span className="hook-item-name">{h.name?.trim() || t('hook.item').replace('{n}', String(hi + 1))}</span>
                          <span className="grow" />
                          <span className="hook-badge">{t('hook.readonly')}</span>
                        </div>
                        <div className="hook-item-body">
                          <div className="hook-kv">
                            <span className="hook-k">{t('hook.command')}</span>
                            <span className="hook-v mono">{h.command ?? '—'}</span>
                          </div>
                        </div>
                      </div>
                    ))}
                  </div>
                ))}
              </div>
            ))}
          </div>
        )
      })}

      {info.warnings.length > 0 && (
        <div className="hook-note warn">
          <div>{t('hook.warnings')}</div>
          <ul className="hook-warn-list mono">
            {info.warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </div>
      )}
    </>
  )
}

// HookForm 新增/编辑钩子的表单态（gi/hi 为 null = 新增）。
type HookForm = {
  ev: string
  gi: number | null
  hi: number | null
  matcher: string
  name: string
  command: string
  timeout: string
  async: boolean
  failureMode: string
}
