import { useEffect, useMemo, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import type { MCPLayerInfo, MCPProjectInfo, MCPProjectLayer, MCPProjectServer } from '../store/useAppStore'
import type { MCPServerCfg } from '../transport/types'
import { useT } from '../i18n'
import { FolderIcon, Gear } from './icons'
import { DetailHead, SourceCard, Switch } from './SettingsGroups'

// ─── MCP 设置页（2026-09-21 改造：全局 × 项目两级 + 二级详情面板）───
//
// 一级 = 来源分组卡片：
//   · 全局（用户配置，跨工作区）→ ~/.go-code/settings.json；
//   · 项目 → 每个**已绑定且仍存在**的项目一张卡（已从项目列表移除 / 目录被删的项目不展示）。
// 二级 = 详情面板：
//   · 全局：可增删改 + 启停 + 刷新连接（Electron 写 settings.json → mcp_set 热应用）；
//   · 项目：{ws}/.go-code/settings.json 里的服务器可增删改 + 启停（mcp_project_set 写盘 + 热应用），
//     {ws}/.mcp.json 里的**只读**（Claude Code 项目级约定、随仓库分发）。
//
// 数据源：settings.mcpServers（全局配置）+ mcp_list（当前工作区连接状态/工具数/层状态）
// + mcp_projects（多项目项目层汇总，只读文件、不创建运行态）。
// 契约详见 docs/MCP_HOOKS_SETTINGS_PANEL_2026-09-21.md。
export function MCPTab() {
  const t = useT()
  const projects = useAppStore((s) => s.projects)
  const mcpProjects = useAppStore((s) => s.mcpProjects)
  const mcpProjectsLoading = useAppStore((s) => s.mcpProjectsLoading)
  const mcpLayers = useAppStore((s) => s.mcpLayers)
  const fetchMCPServers = useAppStore((s) => s.fetchMCPServers)
  const fetchMCPProjects = useAppStore((s) => s.fetchMCPProjects)
  // null = 列表；'user' = 全局详情；项目路径 = 该项目详情
  const [sel, setSel] = useState<string | null>(null)
  // 全局配置文件路径：来自 mcp_list.layers（user 层），拿不到时只显示「所有项目共用」
  const userPath = mcpLayers.find((l) => l.source === 'user')?.path

  const paths = useMemo(() => projects.map((p) => p.path), [projects])
  const pathsKey = paths.join('\u0000')
  useEffect(() => {
    fetchMCPServers()
  }, [fetchMCPServers])
  useEffect(() => {
    fetchMCPProjects(paths)
    // paths 用拼接后的 key 作依赖：数组引用每轮渲染都变，直接用会死循环
  }, [fetchMCPProjects, pathsKey]) // eslint-disable-line react-hooks/exhaustive-deps

  // 项目列表 = 已绑定（paths）+ 仍存在（exists）；后端也返 exists=false 的项，前端直接过滤
  const list = paths
    .map((p) => mcpProjects.find((x) => x.path === p))
    .filter((x): x is MCPProjectInfo => Boolean(x && x.exists))

  if (sel === 'user') return <MCPUserDetail onBack={() => setSel(null)} />
  if (sel) {
    const proj = list.find((p) => p.path === sel)
    if (proj) return <MCPProjectDetail proj={proj} onBack={() => setSel(null)} />
  }

  return (
    <>
      <div className="settings-sec-title">{t('settings.mcp')}</div>
      <p className="set-desc" style={{ marginBottom: 6 }}>{t('mcp.hint')}</p>

      <div className="src-sec">{t('mcp.sec.global')}</div>
      <SourceCard
        icon={<Gear size={14} />}
        title={t('mcp.user.title')}
        meta={`${t('mcp.card.userMeta')}${userPath ? ' · ' + userPath : ''}`}
        onClick={() => setSel('user')}
      />

      <div className="src-sec">{t('mcp.sec.project')}</div>
      {mcpProjectsLoading && list.length === 0 && <p className="set-placeholder">{t('mcp.loading')}</p>}
      {!mcpProjectsLoading && list.length === 0 && <p className="set-placeholder">{t('mcp.noProject')}</p>}
      {list.map((p) => {
        const n = p.servers.length
        return (
          <SourceCard
            key={p.path}
            icon={<FolderIcon size={14} />}
            title={p.name}
            meta={`${n > 0 ? t('mcp.card.projectMeta').replace('{n}', String(n)) : t('mcp.card.none')} · ${p.path}`}
            onClick={() => setSel(p.path)}
          />
        )
      })}
      <p className="set-desc" style={{ marginTop: 8 }}>{t('mcp.project.note')}</p>
    </>
  )
}

// MCPUserDetail 全局（用户配置）详情：可增删改 + 启停 + 刷新连接。
function MCPUserDetail({ onBack }: { onBack: () => void }) {
  const t = useT()
  const servers = useAppStore((s) => s.mcpServers)
  const layers = useAppStore((s) => s.mcpLayers)
  const mcpSaving = useAppStore((s) => s.mcpSaving)
  const fetchMCPServers = useAppStore((s) => s.fetchMCPServers)
  const saveMCP = useAppStore((s) => s.saveMCP)
  const refreshMCP = useAppStore((s) => s.refreshMCP)
  const settings = useAppStore((s) => s.settings)
  const cfg = settings.mcpServers ?? {}
  const userLayer = layers.find((l) => l.source === 'user')

  const [adding, setAdding] = useState(false)
  const [editingName, setEditingName] = useState<string | null>(null) // 内联展开编辑的服务器（null = 无）
  const [draft, setDraft] = useState<MCPServerDraft>(emptyDraft())
  const [err, setErr] = useState<string | null>(null)

  useEffect(() => {
    fetchMCPServers()
  }, [fetchMCPServers])

  const fullMap = (): Record<string, MCPServerCfg> => {
    const map: Record<string, MCPServerCfg> = {}
    for (const [n, c] of Object.entries(cfg)) map[n] = { ...c }
    return map
  }
  const closeForm = () => {
    setAdding(false)
    setEditingName(null)
    setErr(null)
  }
  const openAdd = () => {
    setDraft(emptyDraft())
    setEditingName(null)
    setErr(null)
    setAdding(true)
  }
  const openEdit = (name: string) => {
    setDraft(draftOf(name, cfg[name] ?? {}))
    setAdding(false)
    setErr(null)
    setEditingName(name)
  }
  const save = async () => {
    const built = buildCfg(draft)
    if (!built.ok) {
      setErr(t(built.err))
      return
    }
    const map = fullMap()
    if (editingName && editingName !== built.name) delete map[editingName] // 改名：移除旧键
    map[built.name] = built.cfg
    const r = await saveMCP(map)
    if (!r.ok) setErr(r.error ?? t('mcp.errSave'))
    else closeForm()
  }
  const remove = async (name: string) => {
    const map = fullMap()
    delete map[name]
    await saveMCP(map)
  }
  const toggle = async (name: string, on: boolean) => {
    const map = fullMap()
    map[name] = { ...map[name], enabled: on }
    await saveMCP(map)
  }

  return (
    <>
      <DetailHead title={t('mcp.user.title')} sub={userLayer?.path ?? ''} onBack={onBack}>
        <button className="btn" onClick={() => fetchMCPServers()} disabled={mcpSaving}>
          {mcpSaving ? '…' : t('mcp.refresh')}
        </button>
      </DetailHead>
      <p className="set-desc">{t('mcp.user.desc')}</p>
      {userLayer && <LayerRow layer={userLayer} />}
      {err && <p className="nwf-err" role="alert">{err}</p>}
      <div className="mcp-head">
        <button className="btn primary" onClick={openAdd}>{t('mcp.add')}</button>
      </div>
      {adding && (
        <ServerForm
          draft={draft}
          setDraft={setDraft}
          isAdd
          onSave={save}
          onCancel={closeForm}
        />
      )}
      {Object.keys(cfg).length === 0 && !adding ? (
        <p className="set-placeholder">{t('mcp.empty')}</p>
      ) : (
        Object.entries(cfg).map(([name, c]) => {
          const st = servers.find((s) => s.name === name)
          const enabled = c.enabled ?? true
          return (
            <div className="mcp-server" key={name}>
              <div className="mcp-s-h">
                <span className={`mcp-state ${st?.state ?? 'idle'}`} title={st?.state ?? 'idle'}>
                  {statusLabel(t, st?.state ?? 'idle')}
                  {st?.state === 'connected' && st?.tool_count != null ? ` · ${t('mcp.toolCount').replace('{n}', String(st.tool_count))}` : ''}
                </span>
                <span className="mcp-name">{name}</span>
                <span className="mcp-type">{c.type ?? 'stdio'}</span>
                <span className="grow" />
                {st?.state === 'error' && <button className="btn" onClick={() => refreshMCP(name)}>{t('mcp.refresh')}</button>}
                <button className="btn" onClick={() => (editingName === name ? closeForm() : openEdit(name))}>
                  {editingName === name ? t('mcp.cancel') : t('mcp.edit')}
                </button>
                <button className="btn" onClick={() => remove(name)}>{t('mcp.remove')}</button>
                <span className={`mcp-enabled ${enabled ? 'on' : ''}`}>{enabled ? t('mcp.enable') : t('mcp.disabled')}</span>
                <Switch on={enabled} onChange={(v) => toggle(name, v)} label={name} />
              </div>
              {editingName !== name && (
                <>
                  <div className="mcp-s-cmd mono">{displayCmd(c) || '—'}</div>
                  {st?.state === 'error' && st?.error && <div className="mcp-s-err mono">{st.error}</div>}
                </>
              )}
              {editingName === name && (
                <ServerForm draft={draft} setDraft={setDraft} onSave={save} onCancel={closeForm} />
              )}
            </div>
          )
        })
      )}
    </>
  )
}

// MCPProjectDetail 某项目的项目层详情：
//   · {ws}/.go-code/settings.json 的服务器可增删改 + 启停（写盘 + 热应用）；
//   · {ws}/.mcp.json 的服务器只读（随仓库分发）；
//   · 连接状态只在项目当前打开（running）时才有（不创建运行态就不会有状态）。
function MCPProjectDetail({ proj, onBack }: { proj: MCPProjectInfo; onBack: () => void }) {
  const t = useT()
  const mcpProjectSaving = useAppStore((s) => s.mcpProjectSaving)
  const saveMCPProject = useAppStore((s) => s.saveMCPProject)
  const fetchMCPProjects = useAppStore((s) => s.fetchMCPProjects)
  const projects = useAppStore((s) => s.projects)
  const paths = useMemo(() => projects.map((p) => p.path), [projects])

  const [adding, setAdding] = useState(false)
  const [editingName, setEditingName] = useState<string | null>(null)
  const [draft, setDraft] = useState<MCPServerDraft>(emptyDraft())
  const [err, setErr] = useState<string | null>(null)

  // 可编辑 map = 该项目 .go-code/settings.json 里的服务器（read-only 的 .mcp.json 不并入）
  // 项目文件里校验失败被跳过的条目（这些条目不在 servers 里 → 保存时会被整段覆盖掉，必须提示）
  const skippedNames = proj.layers.filter((l) => l.path.endsWith('.go-code/settings.json')).flatMap((l) => l.skipped ?? [])

  const editableMap = (): Record<string, MCPServerCfg> => {
    const map: Record<string, MCPServerCfg> = {}
    for (const s of proj.servers) {
      if (!s.editable) continue
      map[s.name] = {
        type: (s.type || 'stdio') as MCPServerCfg['type'],
        ...(s.command ? { command: s.command } : {}),
        ...(s.args?.length ? { args: s.args } : {}),
        ...(s.url ? { url: s.url } : {}),
        ...(s.env && Object.keys(s.env).length ? { env: s.env } : {}),
        enabled: s.enabled,
      }
    }
    return map
  }
  const closeForm = () => {
    setAdding(false)
    setEditingName(null)
    setErr(null)
  }
  const openAdd = () => {
    setDraft(emptyDraft())
    setEditingName(null)
    setErr(null)
    setAdding(true)
  }
  const openEdit = (s: MCPProjectServer) => {
    setDraft(draftOf(s.name, editableMap()[s.name] ?? {}))
    setAdding(false)
    setErr(null)
    setEditingName(s.name)
  }
  const save = async () => {
    const built = buildCfg(draft)
    if (!built.ok) {
      setErr(t(built.err))
      return
    }
    const map = editableMap()
    if (editingName && editingName !== built.name) delete map[editingName]
    map[built.name] = built.cfg
    const r = await saveMCPProject(proj.path, map)
    if (!r.ok) setErr(r.error ?? t('mcp.errSave'))
    else closeForm()
  }
  const remove = async (name: string) => {
    const map = editableMap()
    delete map[name]
    await saveMCPProject(proj.path, map)
  }
  const toggle = async (name: string, on: boolean) => {
    const map = editableMap()
    map[name] = { ...map[name], enabled: on }
    await saveMCPProject(proj.path, map)
  }

  return (
    <>
      <DetailHead title={proj.name} sub={proj.path} onBack={onBack}>
        <button className="btn" onClick={() => fetchMCPProjects(paths)} disabled={mcpProjectSaving}>
          {mcpProjectSaving ? '…' : t('mcp.refreshState')}
        </button>
      </DetailHead>
      <p className="set-desc">{t('mcp.project.desc')}</p>
      <div className="mcp-layers">
        <div className="mcp-layers-h">
          <span className="set-label">{t('mcp.layers.title2')}</span>
          {!proj.running && <span className="mcp-layers-ws">{t('mcp.project.closed')}</span>}
        </div>
        {proj.layers.map((l) => (
          <LayerRow key={l.path} layer={l} />
        ))}
      </div>
      {err && <p className="nwf-err" role="alert">{err}</p>}
      {/* 坏条目可见性：面板只渲染能解析的服务器，保存是「整段替换」——不提示就是静默丢配置 */}
      {skippedNames.length > 0 && (
        <p className="hook-note warn">
          {t('mcp.project.skippedWarn').replace('{n}', String(skippedNames.length)).replace('{k}', skippedNames.join('、'))}
        </p>
      )}
      <div className="mcp-head">
        <button className="btn primary" onClick={openAdd}>{t('mcp.add')}</button>
      </div>
      {adding && <ServerForm draft={draft} setDraft={setDraft} isAdd onSave={save} onCancel={closeForm} />}
      {proj.servers.length === 0 && !adding ? (
        <p className="set-placeholder">{t('mcp.project.empty')}</p>
      ) : (
        proj.servers.map((s) => {
          const enabled = s.enabled
          return (
            <div className={`mcp-server ${s.editable ? '' : 'readonly'}`} key={s.name}>
              <div className="mcp-s-h">
                {proj.running && (
                  <span className={`mcp-state ${s.state ?? 'idle'}`} title={s.state ?? 'idle'}>
                    {statusLabel(t, s.state ?? 'idle')}
                    {s.state === 'connected' && s.tool_count != null ? ` · ${t('mcp.toolCount').replace('{n}', String(s.tool_count))}` : ''}
                  </span>
                )}
                <span className="mcp-name">{s.name}</span>
                <span className="mcp-type">{s.type}</span>
                <span className="mcp-badge" title={s.file}>{s.editable ? t('mcp.badge.editable') : t('mcp.badge.repo')}</span>
                <span className="grow" />
                {s.editable ? (
                  <>
                    <button className="btn" onClick={() => (editingName === s.name ? closeForm() : openEdit(s))}>
                      {editingName === s.name ? t('mcp.cancel') : t('mcp.edit')}
                    </button>
                    <button className="btn" onClick={() => remove(s.name)}>{t('mcp.remove')}</button>
                    <span className={`mcp-enabled ${enabled ? 'on' : ''}`}>{enabled ? t('mcp.enable') : t('mcp.disabled')}</span>
                    <Switch on={enabled} onChange={(v) => toggle(s.name, v)} label={s.name} />
                  </>
                ) : (
                  <span className="mcp-enabled">{t('mcp.project.readonly')}</span>
                )}
              </div>
              {editingName !== s.name && (
                <>
                  <div className="mcp-s-cmd mono">
                    {s.type === 'stdio'
                      ? `${s.command ?? ''}${s.args?.length ? ' ' + s.args.join(' ') : ''}`
                      : (s.url ?? '')}
                  </div>
                  {s.state === 'error' && s.error && <div className="mcp-s-err mono">{s.error}</div>}
                </>
              )}
              {editingName === s.name && <ServerForm draft={draft} setDraft={setDraft} onSave={save} onCancel={closeForm} />}
            </div>
          )
        })
      )}
    </>
  )
}

// LayerRow 一行配置文件状态（存在性/有效性/贡献台数/跳过条数；stale = 沿用上次有效内容）。
function LayerRow({ layer }: { layer: MCPProjectLayer | MCPLayerInfo }) {
  const t = useT()
  const stale = 'stale' in layer ? Boolean(layer.stale) : false
  return (
    <div className={`mcp-layer ${stale ? 'stale' : layer.present ? 'on' : 'off'}`}>
      <span className="mcp-layer-dot" aria-hidden />
      <span className="mcp-layer-path mono" title={layer.path}>{layer.path}</span>
      <span className="grow" />
      {stale ? (
        <span className="mcp-layer-bad" title={layer.error ?? ''}>{t('mcp.layers.stale')}</span>
      ) : !layer.present ? (
        <span className="mcp-layer-none">{t('mcp.layers.absent')}</span>
      ) : !layer.ok ? (
        <span className="mcp-layer-bad" title={layer.error ?? ''}>{t('mcp.layers.bad')}</span>
      ) : (
        <span className="mcp-layer-n">{t('mcp.layers.count').replace('{n}', String(layer.servers))}</span>
      )}
      {!!layer.skipped?.length && (
        <span className="mcp-layer-skip" title={layer.skipped.join(', ')}>
          {t('mcp.layers.skipped').replace('{n}', String(layer.skipped.length))}
        </span>
      )}
    </div>
  )
}

// —— MCP 服务器表单（全局/项目两级共用；新增卡与内联编辑同形）——

type MCPServerDraft = {
  name: string
  type: 'stdio' | 'http' | 'sse'
  cmdType: 'npx' | 'uvx' | 'python3' | 'custom'
  command: string
  args: string
  url: string
  envRows: { key: string; value: string }[]
}

// 命令预设：npx / uvx / python3 一键填 command（args 留占位提示）；custom = 手填
const CMD_PRESETS: Record<'npx' | 'uvx' | 'python3' | 'custom', { command: string; argsPh: string }> = {
  npx: { command: 'npx', argsPh: '-y @scope/mcp-server' },
  uvx: { command: 'uvx', argsPh: 'mcp-server-pkg' },
  python3: { command: 'python3', argsPh: '-m module' },
  custom: { command: '', argsPh: '' },
}

const emptyDraft = (): MCPServerDraft => ({ name: '', type: 'stdio', cmdType: 'npx', command: 'npx', args: '', url: '', envRows: [] })

// cmdTypeOf 按 command 反推预设（匹配预设默认 command → 该预设；否则 custom）。
function cmdTypeOf(command: string): MCPServerDraft['cmdType'] {
  for (const [ct, p] of Object.entries(CMD_PRESETS) as [MCPServerDraft['cmdType'], (typeof CMD_PRESETS)['npx']][]) {
    if (ct !== 'custom' && p.command && p.command === command) return ct
  }
  return 'custom'
}

function envToRows(env?: Record<string, string>): { key: string; value: string }[] {
  return Object.entries(env ?? {}).map(([key, value]) => ({ key, value }))
}
function rowsToEnv(rows: { key: string; value: string }[]): Record<string, string> | undefined {
  const out: Record<string, string> = {}
  for (const r of rows) {
    const k = r.key.trim()
    if (k) out[k] = r.value
  }
  return Object.keys(out).length > 0 ? out : undefined
}

// draftOf 用现有配置回填草稿（编辑入口）。
function draftOf(name: string, c: MCPServerCfg): MCPServerDraft {
  const command = c.command ?? CMD_PRESETS.npx.command
  return {
    name,
    type: c.type ?? 'stdio',
    cmdType: cmdTypeOf(command),
    command,
    args: (c.args ?? []).join(' '),
    url: c.url ?? '',
    envRows: envToRows(c.env),
  }
}

// buildCfg 草稿 → 配置（含必填校验）：err 为 i18n key；name 为 trim 后的服务器名。
function buildCfg(draft: MCPServerDraft): { ok: true; name: string; cfg: MCPServerCfg } | { ok: false; err: string } {
  const name = draft.name.trim()
  if (!name) return { ok: false, err: 'mcp.errName' }
  const isStdio = draft.type === 'stdio'
  const command = draft.command.trim()
  if (isStdio && !command) return { ok: false, err: 'mcp.errCommand' }
  if (!isStdio && !draft.url.trim()) return { ok: false, err: 'mcp.errUrl' }
  const env = rowsToEnv(draft.envRows)
  return {
    ok: true,
    name,
    cfg: {
      type: draft.type,
      ...(isStdio
        ? { command, args: draft.args.trim() ? draft.args.split(/\s+/).filter(Boolean) : undefined }
        : { url: draft.url.trim() }),
      ...(env ? { env } : {}),
    },
  }
}

function displayCmd(c: MCPServerCfg): string {
  const args = c.args ?? []
  return c.type === 'stdio' || !c.type ? `${c.command ?? ''}${args.length ? ' ' + args.join(' ') : ''}` : (c.url ?? '')
}

function statusLabel(t: (k: string) => string, s: string): string {
  return s === 'connected' ? t('mcp.state.connected') : s === 'error' ? t('mcp.state.error') : t('mcp.state.idle')
}

// ServerForm 服务器表单：类型/命令预设/command+args|url/env 行列表/保存取消。
function ServerForm({
  draft,
  setDraft,
  isAdd,
  onSave,
  onCancel,
}: {
  draft: MCPServerDraft
  setDraft: (fn: (d: MCPServerDraft) => MCPServerDraft) => void
  isAdd?: boolean
  onSave: () => void
  onCancel: () => void
}) {
  const t = useT()
  return (
    <div className={`mcp-form ${isAdd ? 'mcp-form-add' : ''}`}>
      <div className="set-row">
        <div className="set-label">{t('mcp.name')}</div>
        <input className="prov-input" value={draft.name} onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))} aria-label="mcp name" spellCheck={false} />
      </div>
      <div className="set-row">
        <div className="set-label">{t('mcp.type')}</div>
        <div className="seg" role="radiogroup" aria-label="mcp type">
          {(['stdio', 'http', 'sse'] as const).map((tp) => (
            <button key={tp} className={draft.type === tp ? 'cur' : ''} aria-pressed={draft.type === tp} onClick={() => setDraft((d) => ({ ...d, type: tp }))}>{tp}</button>
          ))}
        </div>
      </div>
      {draft.type === 'stdio' ? (
        <>
          <div className="set-row">
            <div className="set-label">{t('mcp.preset')}</div>
            <div className="seg" role="radiogroup" aria-label="mcp preset">
              {(['npx', 'uvx', 'python3', 'custom'] as const).map((ct) => (
                <button
                  key={ct}
                  className={draft.cmdType === ct ? 'cur' : ''}
                  aria-pressed={draft.cmdType === ct}
                  onClick={() => setDraft((d) => {
                    const next = { ...d, cmdType: ct }
                    if (ct !== 'custom') next.command = CMD_PRESETS[ct].command
                    return next
                  })}
                >
                  {ct}
                </button>
              ))}
            </div>
          </div>
          <div className="set-row">
            <div className="set-label">{t('mcp.command')}</div>
            <input className="prov-input" value={draft.command} onChange={(e) => setDraft((d) => ({ ...d, command: e.target.value, cmdType: cmdTypeOf(e.target.value) }))} aria-label="mcp command" spellCheck={false} placeholder="npx" />
          </div>
          <div className="set-row">
            <div className="set-label">{t('mcp.args')}</div>
            <input className="prov-input" value={draft.args} onChange={(e) => setDraft((d) => ({ ...d, args: e.target.value }))} aria-label="mcp args" spellCheck={false} placeholder={CMD_PRESETS[draft.cmdType].argsPh} />
          </div>
        </>
      ) : (
        <div className="set-row">
          <div className="set-label">{t('mcp.url')}</div>
          <input className="prov-input" value={draft.url} onChange={(e) => setDraft((d) => ({ ...d, url: e.target.value }))} aria-label="mcp url" spellCheck={false} placeholder="http://localhost:8080/mcp" />
        </div>
      )}
      <div className="mcp-env-block">
        <div className="set-label">{t('mcp.env')}</div>
        {draft.envRows.map((row, i) => (
          <div className="mcp-env-row" key={i}>
            <input
              className="prov-input mcp-env-key" value={row.key} aria-label={`env key ${i + 1}`} spellCheck={false}
              placeholder={t('mcp.env.key')}
              onChange={(e) => setDraft((d) => { const rows = [...d.envRows]; rows[i] = { ...rows[i], key: e.target.value }; return { ...d, envRows: rows } })}
            />
            <span className="mcp-env-eq">=</span>
            <input
              className="prov-input mcp-env-val" value={row.value} aria-label={`env value ${i + 1}`} spellCheck={false}
              placeholder={t('mcp.env.value')}
              onChange={(e) => setDraft((d) => { const rows = [...d.envRows]; rows[i] = { ...rows[i], value: e.target.value }; return { ...d, envRows: rows } })}
            />
            <button className="btn mcp-env-del" aria-label={`delete env ${i + 1}`} onClick={() => setDraft((d) => ({ ...d, envRows: d.envRows.filter((_, j) => j !== i) }))}>✕</button>
          </div>
        ))}
        <button className="btn mcp-env-add" onClick={() => setDraft((d) => ({ ...d, envRows: [...d.envRows, { key: '', value: '' }] }))}>
          + {t('mcp.env.add')}
        </button>
      </div>
      <div className="mcp-form-btns">
        <button className="btn primary" onClick={onSave}>{t('mcp.save')}</button>
        <button className="btn" onClick={onCancel}>{t('mcp.cancel')}</button>
      </div>
    </div>
  )
}
