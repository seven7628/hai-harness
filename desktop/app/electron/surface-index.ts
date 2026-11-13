// surface-index.ts —— `~/.go-code/surfaces/` 的**读**侧（PR-14 B）。
//
// 谁写这份目录：bridge（PR-13 的安装器，`desktop/bridge/surfaces.go`）。谁读：
//   · 主进程的 `hai-surface://` 协议（要 enabled 清单 + 扩展名判定）；
//   · `surface:index` IPC → 渲染层的 loader（要知道装了什么、hash 变没变）。
//
// 为什么不给渲染层直接读文件：渲染层没有 fs（预加载只开白名单 IPC），而且「哪些文件
// 可以给字节」这件事必须在主进程判 —— 那是安全边界，不是 UI 便利。
//
// 三处刻意的「不合并」：
//  1. **坏 manifest 不炸整份索引**：一条 error 只影响它自己（其余插件照常加载）。这与
//     bridge 侧「一条坏记录只丢它自己」是同一条纪律（PR-10 health）。
//  2. **不读 panel.js**：hash 已经在 index.json 里（安装时算的 sha256），主进程没有理由
//     为「知道版本变没变」再读一遍 256 KiB。
//  3. **不做校验兜底**：id 不在 `<id>` 目录名之外再拼一次；所有路径拼接只发生在
//     surfaceRoot() 之内（见 surface-protocol.ts 的 realpath 包含判断）。
import * as fs from 'node:fs'
import * as path from 'node:path'
import * as os from 'node:os'

/** 插件 id 的唯一判据（与 bridge 的 surfaceIDPattern 逐字相同）。 */
export const SURFACE_ID_RE = /^[a-z][a-z0-9-]{1,40}$/

/** manifest 缺省入口（与 bridge 的 surfaceDefaultEntry 逐字相同）。 */
export const DEFAULT_SURFACE_ENTRY = 'panel.js'

/** 缺省图标文件名（存在即用；模板 templates/surface/icon.svg 就是它）。 */
export const DEFAULT_SURFACE_ICON = 'icon.svg'

/** index.json 的磁盘协议版本（bridge 侧 surfaceIndexVersion）。不匹配 = 整份当损坏。 */
export const SURFACE_INDEX_VERSION = 1

export interface SurfaceIndexItem {
  id: string
  version: string
  /** sha256(入口文件) 的十六进制（bridge 安装时算的） */
  hash: string
  enabled: boolean
  /** manifest 声明的 kind（必须等于 `plugin:<id>`，loader 校验；坏 manifest 时缺省） */
  kind?: string
  /** manifest.commands：插件声明要调的宿主命令（白名单的一半） */
  commands: string[]
  /** manifest.data：插件声明要订阅的 ui op（host.events.on 的闸门） */
  data: string[]
  /** manifest.entry：`<id>` 目录内的相对路径（缺省 panel.js；PR-13 允许改名，如 dist/panel.js） */
  entry: string
  /** manifest.icon：`<id>` 目录内的相对路径（如 icon.svg）；缺省 = 没有图标 */
  icon?: string
  /** 这份 manifest 读不出来时的原因（loader 跳过它、ui_inspect 里能看到） */
  error?: string
}

export interface SurfaceIndex {
  version: number
  items: SurfaceIndexItem[]
}

let rootOverride = ''

/** configureSurfaceRoot 主进程在 ready 后调一次（`<cfgDir>/surfaces`，与 bridge 的 HOME 对齐）。 */
export function configureSurfaceRoot(dir: string): void {
  rootOverride = dir
}

/** surfaceRoot 生产根目录：`~/.go-code/surfaces`（多实例模式下由 configureSurfaceRoot 覆盖）。 */
export function surfaceRoot(): string {
  return rootOverride || path.join(os.homedir(), '.go-code', 'surfaces')
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : ''
}

function strList(v: unknown): string[] {
  if (!Array.isArray(v)) return []
  return v.filter((x): x is string => typeof x === 'string' && x.trim() !== '').map((x) => x.trim())
}

/**
 * readSurfaceIndex 读 index.json + 每个 `<id>/surface.json`。
 *
 * 空态是 `{version:1, items:[]}`（不是 null）：IPC 的消费者按 `.items` 直接迭代，
 * 「目录还不存在」是最常被走到的分支（新用户一次都没装过插件）。
 * 整份 index.json 坏/版本不符 → 同样回空列表：宁可「一个插件都没有」，也不要
 * 让主进程因为磁盘上的一份 JSON 抛异常（那会把协议与 IPC 一起带黑）。
 */
export function readSurfaceIndex(root: string = surfaceRoot()): SurfaceIndex {
  const empty: SurfaceIndex = { version: SURFACE_INDEX_VERSION, items: [] }
  let raw: string
  try {
    raw = fs.readFileSync(path.join(root, 'index.json'), 'utf8')
  } catch {
    return empty
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return empty
  }
  const idx = parsed as { version?: unknown; items?: unknown }
  if (typeof idx?.version !== 'number' || idx.version !== SURFACE_INDEX_VERSION) return empty
  if (!Array.isArray(idx.items)) return empty

  const items: SurfaceIndexItem[] = []
  for (const rawItem of idx.items) {
    if (!rawItem || typeof rawItem !== 'object') continue
    const it = rawItem as Record<string, unknown>
    const id = str(it.id)
    if (!SURFACE_ID_RE.test(id)) continue // 目录名不合法 → 整条跳过（绝不用它拼路径）
    const item: SurfaceIndexItem = {
      id,
      version: str(it.version),
      hash: str(it.hash),
      enabled: it.enabled === true,
      commands: [],
      data: [],
      entry: DEFAULT_SURFACE_ENTRY,
    }
    // manifest 是**声明**的来源（kind / commands / data / icon）。它坏掉不该让这条
    // 从清单里消失（用户还看得见「装了什么、为什么不能用」），所以带上 error 照发。
    try {
      const mfRaw = fs.readFileSync(path.join(root, id, 'surface.json'), 'utf8')
      const mf = JSON.parse(mfRaw) as Record<string, unknown>
      item.kind = str(mf.kind)
      item.commands = strList(mf.commands)
      item.data = strList(mf.data)
      // entry / icon 只接受**目录内相对路径**：绝对路径/上跳/反斜杠一律不收
      //（协议侧同样会拒，这里先挡一次是为了让 index 里的字段本身是可信的）。
      const relInDir = (v: unknown): string => {
        const rel = str(v).trim()
        if (!rel || path.isAbsolute(rel) || rel.includes('\\') || rel.split('/').includes('..')) return ''
        return rel
      }
      item.entry = relInDir(mf.entry) || DEFAULT_SURFACE_ENTRY
      // 图标：manifest 显式声明的优先；没声明但目录里**有** icon.svg 就用它
      //（01-design §5.6 的模板本来就带这个文件名 —— 让「拷模板」自动有图标）。
      const declaredIcon = relInDir(mf.icon)
      if (declaredIcon) item.icon = declaredIcon
      else if (fs.existsSync(path.join(root, id, DEFAULT_SURFACE_ICON))) item.icon = DEFAULT_SURFACE_ICON
      if (!SURFACE_ID_RE.test(str(mf.id)) || str(mf.id) !== id) {
        item.error = `surface.json 的 id 与目录名不一致（${str(mf.id) || '空'} ≠ ${id}）`
      } else if (!item.kind) {
        item.error = 'surface.json 缺少 kind'
      }
    } catch (e) {
      item.error = `surface.json 读不出来：${e instanceof Error ? e.message : String(e)}`
    }
    items.push(item)
  }
  return { version: SURFACE_INDEX_VERSION, items }
}
