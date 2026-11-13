// 预览渲染器契约（R08a / 01-design §4.7、03-code-plan §2.11）
//
// 为什么需要这层：右栏要承接的**不是面板、是各种文件怎么被看见**。文档与媒体不新增 kind
//（决策 D-I），而是 `file{mode:'preview'}` 内部按扩展名选渲染器。选择规则全部收在
// registry.ts（可单测、不依赖浏览器），组件只认 `data-renderer` 这一个可观测点。
import type { ComponentType } from 'react'

/**
 * 渲染器需要的**文件身份**（不是内容）。
 *
 * 与 03 §2.11 的草案同形（path/ext/size），只多一个 `binary`：bridge 判定二进制时 content
 * 为空串，文本渲染器据此**不该**接（否则拿空串渲染出一块空代码面，用户以为文件是空的）。
 */
export interface FileRef {
  path: string
  /** 小写扩展名，不含点；无扩展名 / `.gitignore` 这类点开头 → ''。 */
  ext: string
  /** 近似字节数：文本 = content.length（字符数，见 registry.fileRefOf），二进制/未知 = 0。 */
  size: number
  /** bridge 判定为二进制（file_preview 的 binary 字段，content 为空）。 */
  binary?: boolean
}

/**
 * 渲染器拿到的内容：**由调用方读好**再传进来。
 *
 * 渲染器自己不开读文件通道 —— 越界校验与 1 MiB 上限都在 bridge 侧（file_preview），
 * 绕过它等于开洞；这条边界与 CsvPreview 的既有口径一致。
 */
export interface PreviewProps {
  file: FileRef
  /**
   * 标签 id（`file` 标签的身份）。R08c 起用于媒体元素池的 owner 归属：
   * 关标签时只有「这个标签」的 owner 被摘掉，别的标签还在播同一个文件就不受影响。
   * 其余渲染器用不到（可选，不传也能工作）。
   */
  tabId?: string
  /** 文本内容；二进制渲染器（image/pdf/office）不使用，为 ''。 */
  content: string
  /**
   * 预高亮的 shiki HTML。**可选**，两条路各取所需：
   *   · 文件面板（FilePreview）本来就算了一份给「定位到读过的行」用 → 传下来，省一次高亮；
   *   · 独立预览面板（mode='preview'）没有定位锚 → 不传，渲染器自己用 useCodeHtml 算。
   * 传了就**必须**用传进来的那份（主题/字号都是调用方那一份，两边各算会闪一次色）。
   */
  html?: string
  /**
   * bridge 的 1 MiB 闸是否截断了 content（file_preview 的 `truncated`）。
   *
   * 为什么放进契约：截断的后果取决于**渲染器**怎么用这份内容 —— text 少几行无所谓，
   * html 少掉的往往是页面尾部（图表、结论），office 少掉的是后半本书。这类「少了一截」
   * 必须由渲染器自己如实说，而不是等用户发现内容对不上。缺省（不传）= 没截断。
   */
  truncated?: boolean
  /**
   * 「我接不了这个文件」的上报口（R08b）。
   *
   * 为什么必须是上报、不是自己画一块占位：`data-renderer` 只有一个写者（PreviewHost），
   * 若渲染器自己渲染 `data-renderer="unavailable"`，宿主就会把它**包在** `data-renderer="docx"`
   * 里面 —— 同屏两个 renderer 标识，探针与看板都得猜哪个算数。上报让宿主把整块换成
   * unavailable（带 data-renderer-id），与「依赖缺失」那条路落到同一个组件上。
   *
   * 触发场景（都要求**写明缺什么**，不许笼统「加载失败」）：转换失败/超时、产物读不回、
   * 内嵌视图建不起来。调用时机：effect / catch 里（渲染期调用会 setState 报警）。
   */
  onUnavailable?: (reason: string) => void
}

/**
 * 渲染器依赖（如「转换服务」「媒体协议」「内嵌浏览器视图」）。R08a 立名册；
 * R08b 起有真值：docx/pptx/xlsx → conversion、pdf → embed（探测见 deps.ts），R08c 的 media 待接。
 *
 * 为什么注册表要认识它：未安装时的落点必须是**写明原因的占位**（`data-renderer="unavailable"`），
 * 而不是白屏。把「依赖名」当自由字符串会出现拼错的 requires 永远静默不可用 —— 故注册即校验。
 */
export type PreviewDependency = 'conversion' | 'media' | 'embed'

/** 依赖可用性。不可用时 reason 是给用户看的原因（诊断文案，与 bridge 的 error 同口径，不走 i18n）。 */
export type DependencyStatus = { ok: true } | { ok: false; reason: string }

export interface PreviewRenderer {
  /** 稳定 id，也是 DOM 上的 `data-renderer`（'text' | 'csv' | 'image' | 'pdf' | …）。 */
  id: string
  /**
   * 0 = 不接，非 0 = 接。**只判「接不接」**，不参与排序 —— 排序看 weight
   * （03 §2.11 原文「0=不接，取最大」在 R08a 落成：match 只做门禁，weight 做优先级，
   *  这样「文本兜底」不必用一个巨大的 match 分数去压别的渲染器）。
   */
  match: (f: FileRef) => number
  /**
   * 优先级，**越大越先**。数值口径（并发取只写在这里，避免各处各猜）：
   *   100 专用格式（pdf/office，R08b）· 90 媒体 · 80 图片 · 70 页面（html/svg）
   *   · 60 表格(csv) · 10 文本兜底
   * 同 weight 时**按注册顺序**取先注册的那个（见 registry.pickRenderer 的严格大于）。
   */
  weight: number
  /** 依赖名册（见 PreviewDependency）。缺省 = 无依赖。 */
  requires?: PreviewDependency[]
  /**
   * 懒加载：返回组件本身（不是元素）。不进启动 bundle —— 03 §2.11 的硬要求。
   * 同一 id 只会被调用一次（loader.ts 缓存 Promise），失败不缓存（允许重试）。
   */
  load: () => Promise<ComponentType<PreviewProps>>
}
