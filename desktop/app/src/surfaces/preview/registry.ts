// 预览渲染器注册表：模块级单例，唯一写者（同 surfaces/registry.ts 的纪律）
//
// 三条约束（与 Surface 注册表同源）：
//   · **注册即校验**，重复 id / 未知依赖 / 字段缺失一律抛错（不静默覆盖、不静默半可用）；
//   · 选择必须**确定**：weight 降序 + 同 weight 按注册顺序（不靠 Map 迭代顺序碰巧）；
//   · 本模块不 import 任何 React / 浏览器 API —— 注册表本身要能单测（R08a 的验收点之一）。
//
// 与 Surface 注册表的差别：那边按 kind 唯一（一个 kind 一个 provider），这边是**有序候选表**
// （同一份文件可以有多个渲染器接，按 weight 决出赢家；pdf 与 image 都可能接同一个 .pdf）。
import { extOf } from '../../lib/visualFile'
import type {
  DependencyStatus,
  FileRef,
  PreviewDependency,
  PreviewRenderer,
} from './types'

/**
 * 已知依赖名册 → 默认状态。requires 写错名字必须在注册那一刻就炸，而不是上线后静默不可用。
 *
 * 默认值一律「不可用」：渲染器先别抢（抢了就得在面板里现编一句「不知道」），由 deps.ts 在
 * 启动时用真环境覆盖（`detectPreviewDeps`）。这个方向与「依赖没探测到就当成有」相比只错一次：
 * 漏探测 = 显示写明原因的占位；反过来的错是**白屏**。
 */
const KNOWN_DEPS: PreviewDependency[] = ['conversion', 'media', 'embed']
const DEFAULT_DEPS: Array<[PreviewDependency, DependencyStatus]> = [
  ['conversion', { ok: false, reason: '转换服务状态未知（docx/pptx/xlsx 预览不可用）' }],
  ['media', { ok: false, reason: '媒体协议未接（音视频预览不可用）' }],
  ['embed', { ok: false, reason: '内嵌浏览器视图状态未知（pdf 预览不可用）' }],
]

/** 注册顺序 = 候选顺序：数组而不是 Map，同 weight 的先后**显式**可读（不依赖实现细节）。 */
let entries: PreviewRenderer[] = []
let byId = new Map<string, PreviewRenderer>()
let deps = new Map<PreviewDependency, DependencyStatus>(DEFAULT_DEPS)

function assertRenderer(r: PreviewRenderer) {
  if (!r || typeof r !== 'object') throw new Error('渲染器必须是对象')
  if (typeof r.id !== 'string' || !r.id.trim()) throw new Error('渲染器缺少 id')
  if (typeof r.match !== 'function') throw new Error(`渲染器缺少 match：${r.id}`)
  if (typeof r.load !== 'function') throw new Error(`渲染器缺少 load：${r.id}`)
  if (typeof r.weight !== 'number' || !Number.isFinite(r.weight)) {
    throw new Error(`渲染器 weight 必须是有限数字：${r.id}`)
  }
  if (r.requires != null) {
    if (!Array.isArray(r.requires)) throw new Error(`渲染器 requires 必须是数组：${r.id}`)
    for (const d of r.requires) {
      if (!KNOWN_DEPS.includes(d)) throw new Error(`未知依赖：${String(d)}（渲染器 ${r.id}）`)
    }
  }
}

/** 注册。重复 id 直接抛错（不静默覆盖 —— 覆盖会让「谁生效」变成加载顺序的偶然）。 */
export function registerRenderer(r: PreviewRenderer): void {
  assertRenderer(r)
  if (byId.has(r.id)) throw new Error(`预览渲染器 id 重复注册：${r.id}`)
  entries = [...entries, r]
  byId = new Map(byId).set(r.id, r)
}

/** 注销。返回是否真的删除了（重复注销不抛错）。 */
export function unregisterRenderer(id: string): boolean {
  const hit = byId.get(id)
  if (!hit) return false
  entries = entries.filter((r) => r.id !== id)
  byId = new Map(byId)
  byId.delete(id)
  return true
}

export function getRenderer(id: string | undefined): PreviewRenderer | undefined {
  return id ? byId.get(id) : undefined
}

/** 注册顺序（不是 weight 序）。测试与诊断用；选择走 pickRenderer。 */
export function listRenderers(): PreviewRenderer[] {
  return [...entries]
}

/**
 * 选出接这份文件的渲染器（0 = 没人接 → undefined）。
 *
 * `only` 是给**视图开关**用的收窄：文件面板的「表格 / 纯文本」是 UI 决策（同一份 .csv 在
 * 纯文本模式下不能被 csv 渲染器抢走），故由调用方指名候选，而不是把模式塞进 FileRef。
 */
export function pickRenderer(f: FileRef, opts?: { only?: readonly string[] }): PreviewRenderer | undefined {
  const only = opts?.only
  let best: PreviewRenderer | undefined
  for (const r of entries) {
    if (only && !only.includes(r.id)) continue
    if (r.match(f) <= 0) continue
    // 严格大于：同 weight 保留**先注册**的那个（确定性来源，不靠 Map 迭代顺序）
    if (!best || r.weight > best.weight) best = r
  }
  return best
}

/** 依赖可用性上报（R08b/R08c 接真值；本轮只有测试与探针会调）。 */
export function setDependencyStatus(dep: PreviewDependency, status: DependencyStatus): void {
  if (!KNOWN_DEPS.includes(dep)) throw new Error(`未知依赖：${String(dep)}`)
  deps = new Map(deps).set(dep, status)
}

export function dependencyStatus(dep: PreviewDependency): DependencyStatus | undefined {
  return deps.get(dep)
}

/**
 * 该渲染器缺的依赖原因（不缺 → undefined）。多个缺失只报第一个：占位文案说清一个
 * 真实原因即可，堆一串只会让人跳过。
 */
export function missingDependencyReason(r: PreviewRenderer): string | undefined {
  for (const d of r.requires ?? []) {
    const st = deps.get(d) ?? { ok: false as const, reason: `未知依赖：${d}` }
    if (!st.ok) return st.reason
  }
  return undefined
}

/**
 * 文件身份构造（唯一的 ext 口径：lib/visualFile.extOf，与自动打开/产出卡同一份）。
 * size 语义见 types.FileRef：文本给 content.length，二进制给 0（bridge 没回字节数）。
 */
export function fileRefOf(path: string, size = 0, binary?: boolean): FileRef {
  return { path, ext: extOf(String(path ?? '')), size: Math.max(0, size | 0), ...(binary ? { binary: true } : {}) }
}

/** 仅供测试：清空注册表与依赖状态（探针的「清空变异」也走它，见 e2e/verify_preview_renderers.mjs）。 */
export function resetRenderersForTest(): void {
  entries = []
  byId = new Map()
  deps = new Map(DEFAULT_DEPS)
}
