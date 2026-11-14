// surfaces/plugin/storage.ts —— 插件数据的生命周期（R17a）。
//
// 契约（skills/hai-surface-dev/SKILL.md 的 Storage 一节是同一句话的用户侧版本）：
// 宿主**不给**插件 storage API，插件把状态写在 `localStorage` 里，键以 `<id>:` 开头。
// 用户已拍板：**卸载 = 清掉该 id 前缀下的所有键**（插件存储是可弃的，需要活过卸载的
// 东西本来就不该放在这里）。**禁用不删**（插件还在、数据还该在，重新启用就接着用）。
//
// 两个入口，职责不同、都必须幂等：
//  1. `clearPluginData(id)` —— 显式清理。设置页的「清除数据」按钮，以及**卸载成功后
//     立刻**调一次（不依赖 `surfaces_changed`：页面可能已经切走、事件可能丢）。
//  2. `reconcileKnownIds(currentIds, authoritative)` —— 跨重启兜底。`syncPlugins` 每轮调一次
//     （`authoritative=false` 时什么都不做：浏览器版拿到的空 index 不是「插件都没了」）：
//     上一次见过的 id 这次不在 index 里 = 卸载发生在宿主没运行的时候，清掉它的数据。
//
// 为什么单独一个文件、而不是塞进 loader.ts：设置页要 import 清理函数，而 loader.ts
// 会连带拉进 React 组件树、uiBus、插件宿主能力面（那一坨对设置页是纯负担），
// 更要紧的是 loader 依赖 store 的注入面 —— 让「删几个 localStorage 键」这点事
// 挂在那条依赖链上，迟早有人为了一个按钮把 store 拉进设置页形成环。
//
// 单一真相：`surface.disabled` 的写入只走 health.ts（本文件调它的 clearSurfaceHealth，
// 不自己 parse 那份 JSON）—— 两个读写点就是两份真相，PR-10 已经为此付过一次代价。
import { clearSurfaceHealth, forgetSurfaceVersion } from '../health'
import type { SurfaceKind } from '../types'

/** 插件键的前缀分隔符：`<id>:<whatever>`（与 SKILL.md 的例子 `course-mvp:progress:v1` 一致）。 */
export const PLUGIN_DATA_SEP = ':'

/**
 * 「上次见过的插件 id 列表」的存储键。
 *
 * 为什么需要它：清理必须发生在**卸载那一刻**，但卸载完全可能发生在宿主没跑的时候
 * （用户直接删目录 / 上一次会话里已经移除）。运行期的增量只能看到「这一轮 index 里
 * 少了谁」，看不到「上一轮是谁」—— 那份记忆只能落盘。
 */
export const KNOWN_IDS_KEY = 'hai-surface.known-ids'

/** 记录的格式版本。写在**值**里而不是键名里：键名带版本的话，升级当天就读不到旧记录了。 */
export const KNOWN_IDS_VERSION = 1

/** 插件 id → 它的数据键前缀。空 id 返回空串（调用方必须拒掉，见 clearPluginData）。 */
export function pluginDataPrefixOf(id: string): string {
  const clean = String(id ?? '').trim()
  return clean ? `${clean}${PLUGIN_DATA_SEP}` : ''
}

/** localStorage 的安全句柄：隐私模式 / 被策略禁掉时返回 null（本文件的每个入口都不许抛）。 */
function storage(): Storage | null {
  try {
    return typeof localStorage === 'undefined' ? null : localStorage
  } catch {
    return null
  }
}

/**
 * pluginDataKeys 该插件前缀下的所有键（码点排序，稳定输出）。
 *
 * 先收集再删除：`localStorage` 的索引在 removeItem 之后会前移，一边删一边按下标遍历
 * 会漏掉一半的键（症状是「清了一半，剩下的一半要再点一次」）。
 */
export function pluginDataKeys(id: string): string[] {
  const s = storage()
  const prefix = pluginDataPrefixOf(id)
  if (!s || !prefix) return []
  const out: string[] = []
  try {
    for (let i = 0; i < s.length; i += 1) {
      const k = s.key(i)
      if (k && k.startsWith(prefix)) out.push(k)
    }
  } catch {
    return []
  }
  return out.sort()
}

/**
 * clearPluginData 清掉一个插件的全部数据（幂等：没数据时删 0 个键，仍然清健康记录）。
 *
 * 同时删掉 `surface.disabled` 里 `plugin:<id>` 那一条：kind 已经不存在了，留着的唯一
 * 效果是「同名插件重装后一上来就是禁用态」—— 那正是 R-20 抱怨的那类幽灵状态。
 *
 * 返回删掉的键数（设置页的 toast 要报个数；0 也照样算成功）。
 */
export function clearPluginData(id: string): number {
  const prefix = pluginDataPrefixOf(id)
  // 空 id 的前缀是 ':'，那会把**所有**以冒号开头的键（也就是别的插件的数据）一起扫掉。
  // 这个函数只有一个调用来源（id 来自 index），但没有这条守卫的话，一次 undefined
  // 透传就是「用户点了一个按钮，别人的数据没了」。
  if (!prefix) return 0
  const s = storage()
  const keys = pluginDataKeys(id)
  let removed = 0
  if (s) {
    for (const k of keys) {
      try {
        s.removeItem(k)
        removed += 1
      } catch {
        // 单个键删不掉（配额/策略）不该中断其余：清掉能清的是更好的结果。
      }
    }
  }
  const kind = `plugin:${String(id).trim()}` as SurfaceKind
  clearSurfaceHealth(kind)
  forgetSurfaceVersion(kind)
  return removed
}

/** 读「上次见过的 id 列表」。**null = 没有记录**（首次升级），调用方必须当作「什么都不清」。 */
export function readKnownIds(): string[] | null {
  const s = storage()
  if (!s) return null
  // getItem 也可能抛（隐私模式下的某些实现）：读取失败 = 没有记录 = 不清任何东西。
  const raw = ((): string | null => {
    try {
      return s.getItem(KNOWN_IDS_KEY)
    } catch {
      return null
    }
  })()
  if (!raw) return null
  try {
    const parsed: unknown = JSON.parse(raw)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null
    const o = parsed as Record<string, unknown>
    if (typeof o.version !== 'number' || o.version < 1) return null
    if (!Array.isArray(o.ids)) return null
    // 单个坏元素只丢它自己：一份被截断的 JSON 不该升级成「把所有人的数据都当孤儿」。
    const ids = o.ids.filter((x): x is string => typeof x === 'string' && x.trim().length > 0)
    return [...new Set(ids.map((x) => x.trim()))].sort()
  } catch {
    return null
  }
}

/** 写下「这一轮 index 里的全部 id」。写不进去也不抛（隐私模式下每次启动都重新判断一次而已）。 */
export function writeKnownIds(ids: string[]): void {
  const s = storage()
  if (!s) return
  const clean = [...new Set(ids.map((x) => String(x ?? '').trim()).filter((x) => x.length > 0))].sort()
  try {
    s.setItem(KNOWN_IDS_KEY, JSON.stringify({ version: KNOWN_IDS_VERSION, ids: clean }))
  } catch {
    // 配额满 → 这次不记。下次启动仍然按「没有记录」处理（不清任何东西），安全的那一侧。
  }
}

/**
 * reconcileKnownIds 跨重启的「消失的 id」回收；返回被清掉数据的 id（码点排序）。
 *
 * 四条必须守住的语义（任意一条破了都会误删用户数据）：
 *  0. **authoritative=false → 既不清理也不写记录**（缺省就是这个）。`syncOnce` 的 index 来自
 *     `bridge.surfaceIndex`，而浏览器版（vite 直开、没有 preload）那条路回的是
 *     硬编码的 `{items:[]}` —— 把「拿不到 index」读成「一个插件都没装」，一次网页会话就能
 *     把**全部**插件数据清空。所以「这份 index 算不算数」必须由调用方显式声明，绝不靠
 *     `items` 非空去猜：卸载最后一个插件时 index 恰好是空的，那一次**必须**能清。
 *     非权威时连 known-ids 都不许写 —— 否则一次网页会话会把「上次见过谁」覆盖成空，
 *     下一次真机启动就没有对照物，跨重启卸载永远回收不了。
 *  1. **currentIds 是 index.items 的全部 id**（含 `enabled:false`、含带着 manifest error 的），
 *     不是 `loaded`、也不是 `alive` —— 一个声明就坏掉、从来没加载成功的插件被卸载时，
 *     它的数据同样要清；而**禁用的插件不能算消失**（禁用不删数据）。
 *  2. **没有记录 = 首次升级 = 一个都不清**：老用户的机器上已经装着一堆插件，第一次跑
 *     到这个函数时 known 是 null，若把它当成「已知集合为空 / 或当成全部已知」，前者无害，
 *     后者会把所有已装插件的数据当场删光。宁可漏清一次，也不能误删一次。
 *  3. **先清再写**：清理中途抛（比如某个 removeItem 被策略拦）时，known 列表还没更新，
 *     下一次启动会重试；反过来先写后清，一次失败就等于永久漏清。
 */
export function reconcileKnownIds(currentIds: string[], authoritative = false): string[] {
  // 缺省**不权威**：未知来源 = 不动数据。这是唯一安全的缺省值（见上面第 0 条）。
  if (authoritative !== true) return []
  const current = [...new Set(currentIds.map((x) => String(x ?? '').trim()).filter((x) => x.length > 0))].sort()
  const known = readKnownIds()
  const gone = known === null ? [] : known.filter((id) => !current.includes(id))
  for (const id of gone) {
    try {
      clearPluginData(id)
    } catch {
      // 清理失败不该让整个 sync 挂掉（插件本身还等着加载）。
    }
  }
  writeKnownIds(current)
  return gone
}
