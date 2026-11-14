// surfaces/plugin/entries.ts —— 插件入口的**渲染层快照**（R16 / D-2 的宿主侧）。
//
// 一句话：index 里每个 enabled 插件的 `entries[]` 都在这里变成「一行菜单项」，
// 三处入口（＋ 菜单 / 空态能力面板 / 设置页的「打开」）读的都是这一份。
//
// 四条刻意的选择：
//
//  1. **唯一的写者是加载器**（`setSurfaceEntryIndex` 由 loader 的 syncOnce 调）：
//     加载器已经是「谁读 `surface:index`」的唯一答案，菜单没有理由再发一次 IPC。
//     两次读 = 两份快照 = 「菜单里有、注册表里没有」这种最难查的半态。
//  2. **零 entries = 一个默认入口**（D-2 的硬约束）：不是体验优化 —— 任何已安装且
//     enabled 的插件至少有一个可见入口，否则它就是一个「加载成功但界面上看不见」的
//     东西，用户只能靠卸载去发现它。
//  3. **菜单只要 enabled，标题要全部**：禁用/卸载后入口必须立刻消失（三处），但**已经
//     开着的标签**还要显示它原来的名字（墓碑不叫 `plugin:course-manager`）。所以快照分
//     两份：`menu`（enabled）与 `declaredTitles`（含禁用）。
//  4. **不做二次校验**：id/title/图标的合法性归 Go 的安装期校验（D-3）；这里只做
//     「这份 IPC 数据能不能安全地渲染」的防御（坏项丢掉、绝不抛）。
//  5. **标题留着声明、不在这里选语言**（R16b）：`title` 可能是 `{ zh, en }`。选语言的
//     唯一入口是本文件的 entryTitleText，调用点在渲染层（useLang）。
import type { SurfaceEntryDecl, SurfaceEntryTitle, SurfaceIndex, SurfaceIndexItem } from '../../transport/types'
import { notifySurfacesChanged } from '../registry'

/**
 * 零 entries 时合成入口的 id（`params.entry === 'default'`）。
 *
 * 为什么不是插件 id：`params.entry` 的语义是「这个插件的第几个页面」，而插件 id 说的是
 * 「哪个插件」。两者同名会让以后加「按 entry 名做路由」时出现一个天然歧义（插件 `foo` 的
 * 默认入口到底叫 `foo` 还是叫 `default`）。
 */
export const DEFAULT_ENTRY_ID = 'default'

/** 渲染层要的语言（与 host/lang.tsx 的 HostLang 同一集合）。 */
export type EntryLang = 'zh' | 'en'

/**
 * 把声明里的标题按当前语言取成字符串（R16b）——**唯一的**语言选择点。
 *
 * 规则（与 manifest.name 的 zh → en 兜底同向，只是反过来：先本语言，再另一边）：
 *   · 单字符串：两种语言都用它（老 manifest 行为一字不变）；
 *   · `{ zh, en }`：当前语言有值用它，没有就用另一边；两边都没有 = 空串（安装期已拒）。
 * 为什么不在快照里选好：菜单/标签是活的语言订阅者，而快照只在 sync 时更新 ——
 * 切一次语言要等下一次 surfaces_changed 才改文案，是用户看得见的 bug。
 */
export function entryTitleText(title: SurfaceEntryTitle | undefined | null, lang: EntryLang): string {
  if (typeof title === 'string') return title
  if (!title || typeof title !== 'object') return ''
  const zh = typeof title.zh === 'string' ? title.zh.trim() : ''
  const en = typeof title.en === 'string' ? title.en.trim() : ''
  return lang === 'en' ? en || zh : zh || en
}

/** 快照比较用的稳定签名：字符串写法与对象写法即使同值也必须区分（en 下表现不同）。 */
const titleSig = (t: SurfaceEntryTitle | undefined): string =>
  (typeof t === 'string' ? `s:${t}` : `o:${JSON.stringify(t ?? null)}`)

/** 一条可渲染的入口（菜单项 / 设置页的「打开」）。 */
export interface PluginEntryItem {
  /** 插件 id（= `~/.go-code/surfaces/<id>` 的目录名） */
  pluginId: string
  /** 插件显示名（manifest.name 的 zh → en → id）：多个插件时菜单用它分组 */
  pluginName: string
  /** provider kind（`plugin:<id>`） */
  kind: `plugin:${string}`
  /** `params.entry` 的取值 */
  entryId: string
  /**
   * 菜单/设置页看到的标题（声明项取 manifest.title；默认入口取插件显示名）。
   *
   * R16b 起是**声明本身**（`string | { zh, en }`），不是某一语言的字符串：
   * 渲染点用 `entryTitleText(title, useLang())` 现选 —— 提前选好就等于「切语言要重读索引」。
   */
  title: SurfaceEntryTitle
  /** `<id>` 目录内的相对路径（缺省 = 用 kind 图标） */
  icon?: string
  /** true = 宿主合成的默认入口（零 entries），不是 manifest 声明的 */
  fallback: boolean
}

/**
 * 声明的入口，按 order 升序（缺省 100）。
 *
 * 相等时**保持 manifest 里的顺序**（Array.prototype.sort 自 ES2019 起稳定）：让「不写
 * order」的作者拥有「按我写的顺序排」这条最直觉的语义，而不是被迫给每一项编号。
 */
export function entriesOf(item: SurfaceIndexItem): SurfaceEntryDecl[] {
  const list = Array.isArray(item.entries) ? item.entries : []
  return [...list].sort((a, b) => (a.order ?? 100) - (b.order ?? 100))
}

/**
 * 一个插件的入口项：声明的 entries；一条都没有 → 一个默认入口（标题取 name/id）。
 *
 * 标题的兜底顺序 = `manifest.name`（electron 侧已按 zh → en → 任意非空解析）→ id。
 * 为什么不到 provider 的 title() 里取：菜单可能在 provider 注册**之前**就要画出来
 * （插件加载失败时也得有入口，D-3 的最后一行），而 provider 只有活的插件才有。
 */
export function entryItemsOf(item: SurfaceIndexItem): PluginEntryItem[] {
  const kind = `plugin:${item.id}` as const
  const pluginName = String(item.name ?? '').trim() || item.id
  const declared = entriesOf(item)
  if (declared.length === 0) {
    return [{ pluginId: item.id, pluginName, kind, entryId: DEFAULT_ENTRY_ID, title: pluginName, fallback: true }]
  }
  return declared.map((e) => ({
    pluginId: item.id,
    pluginName,
    kind,
    entryId: e.id,
    title: e.title,
    ...(e.icon ? { icon: e.icon } : {}),
    fallback: false,
  }))
}

/** 一组同插件的入口（菜单/空态面板/设置页共用的**一份**分组口径）。 */
export interface PluginEntryGroup {
  pluginId: string
  pluginName: string
  items: PluginEntryItem[]
}

/**
 * 按插件分组，保留「第一次出现」的顺序（= index 里 plugins 的顺序，与设置页一致）。
 *
 * 为什么宿主自己分组而不是让每项自带分隔线：三处入口都要「同一个插件的几项在一起」，
 * 三处各写一遍分组就会出现「菜单按插件分、设置页按标题排」的错位 —— 而用户对入口的
 * 心智只有一个（这个插件有这几页）。
 */
export function groupEntryItems(items: PluginEntryItem[]): PluginEntryGroup[] {
  const byId = new Map<string, PluginEntryGroup>()
  for (const it of items) {
    const g = byId.get(it.pluginId)
    if (g) g.items.push(it)
    else byId.set(it.pluginId, { pluginId: it.pluginId, pluginName: it.pluginName, items: [it] })
  }
  return [...byId.values()]
}

// —— 快照 ——
//
// 模块级而不是 store 字段：入口列表是「插件的声明」，与会话/工作区无关（不随 scope 分域），
// 放进 per-scope 的标签集里只会多出一份要同步的副本。订阅复用 registry 的
// subscribeSurfaces（与 health 的禁用集同一条通道，见 registry.ts notifySurfacesChanged）。

let menu: PluginEntryItem[] = []
let declaredTitles = new Map<string, SurfaceEntryTitle>()
let signature = ''

const titleKey = (pluginId: string, entryId: string): string => `${pluginId}\u0000${entryId}`

/**
 * 用一份刚读到的索引更新快照（loader 的 syncOnce 在**注册/卸载之前**调用它）。
 *
 * 变化时才广播：RightPanel 的订阅会把每次广播变成一次重渲染，而 syncOnce 的每一次
 * （包括「什么都没变、全是 skipped」的那次）都会走到这里 —— 无脑广播就是一条
 * 「点开设置页 → 右栏重渲染三次」的隐性开销。
 */
export function setSurfaceEntryIndex(index: SurfaceIndex): void {
  const items = Array.isArray(index?.items) ? index.items : []
  const nextMenu: PluginEntryItem[] = []
  const nextTitles = new Map<string, SurfaceEntryTitle>()
  for (const item of items) {
    if (!item || typeof item.id !== 'string' || !item.id) continue
    // 标题表**不看 enabled**：禁用后入口从菜单消失，但已开的标签还得叫「课表」。
    for (const decl of entriesOf(item)) nextTitles.set(titleKey(item.id, decl.id), decl.title)
    if (item.enabled) nextMenu.push(...entryItemsOf(item))
  }
  const sig = JSON.stringify(nextMenu)
  const titlesChanged = nextTitles.size !== declaredTitles.size
    || [...nextTitles].some(([k, v]) => titleSig(declaredTitles.get(k)) !== titleSig(v))
  declaredTitles = nextTitles
  if (sig === signature && !titlesChanged) return
  signature = sig
  menu = nextMenu
  notifySurfacesChanged()
}

/** 当前可渲染的入口（只含 enabled 插件；空数组 = 没有任何插件入口）。 */
export function listPluginEntries(): PluginEntryItem[] {
  return menu
}

/**
 * 标签标题：**已声明**的 entry 用 manifest 的 title。
 *
 * 为什么由宿主定，而不是永远问 provider：菜单是宿主画的，菜单项叫「课表」而标签叫别的，
 * 用户会以为点错了。默认入口例外（零 entries 时主人是 provider 自己 —— 它没声明标题，
 * 宿主就不该用 name 覆盖掉 provider 的 title()）。
 */
export function declaredEntryTitle(kind: string, params: unknown, lang: EntryLang = 'zh'): string {
  if (!kind.startsWith('plugin:') || !params || typeof params !== 'object' || Array.isArray(params)) return ''
  const entryId = (params as Record<string, unknown>).entry
  if (typeof entryId !== 'string' || !entryId) return ''
  return entryTitleText(declaredTitles.get(titleKey(kind.slice('plugin:'.length), entryId)), lang)
}

/** 仅供测试/探针：清空快照（生产里模块只求值一次，没有调用点）。 */
export function resetPluginEntriesForTest(): void {
  menu = []
  declaredTitles = new Map()
  signature = ''
}
