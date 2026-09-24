import { memo, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import remarkFrontmatter from 'remark-frontmatter'
import remarkGemoji from 'remark-gemoji'
import rehypeKatex from 'rehype-katex'
import { fromHtml } from 'hast-util-from-html'
import { defaultSchema, sanitize } from 'hast-util-sanitize'
import type { Schema } from 'hast-util-sanitize'
import type { Element, ElementContent, Nodes, Root, RootContent } from 'hast'
import GithubSlugger from 'github-slugger'
import 'katex/dist/katex.min.css'
import { codeToHtml } from './highlight'
import { useAppStore } from '../store/useAppStore'
import { useResolvedTheme } from '../lib/useTheme'
import { useAppearance } from '../lib/appearance'
import { copyText } from '../lib/clipboard'
import { displayPath, isLocalAssetSrc, resolveLocalAssetPath, useLocalAsset } from '../lib/localAsset'
import { t as tStatic, useLang, useT } from '../i18n'

// mermaid 懒加载单例（与 shiki 同策略）：首次出现 mermaid 块才动态 import。
// 避免初始 bundle 拖入 mermaid 本体（体积大，走独立 chunk）。
let mermaidP: Promise<typeof import('mermaid')> | null = null
function getMermaid(): Promise<typeof import('mermaid')> {
  if (!mermaidP) mermaidP = import('mermaid')
  return mermaidP
}

// 行内数学 $...$ 的假阳性收严（remark 插件：需在 remarkMath 之后运行）。
// 背景：micromark 的行内数学允许定界符内侧带空白，于是散文里的美元符号会被吃成公式，
// 且**正文会被吞进公式**（实测，肉眼可见错乱）：
//   「成本 $5 到 $10 之间。」   → 渲染成 成本 [5 到]10 之间。
//   「echo $PATH 与 $HOME。」  → 渲染成 echo [PATH 与]HOME。
//   「脚本参数 $1 $2 和 $@。」  → 渲染成 [1] [2]…
// 规则：定界符**紧邻内侧为空白** → 判为散文中的 $，还原成纯文本（沿用语料里常见的
// 数学扩展约定：$ 后不紧跟空白、$ 前不紧接空白）。
// 影响：`$E=mc^2$` / `$$…$$` 块级公式不受影响；`$ x $`（人为留空格）这类写法会变纯文本
// ——这是有意取舍（宁可少认公式，不可吞正文）。
// 残余：`$PATH:$HOME`（内侧无空白）仍会被当公式（渲染为等宽数学体，但不吞正文）。
type MdNode = { type?: string; value?: string; children?: MdNode[]; data?: Record<string, unknown>; position?: { start?: { offset?: number }; end?: { offset?: number } } }
function remarkMathGuard(source: string) {
  return (tree: MdNode) => {
    const walk = (node: MdNode) => {
      if (!Array.isArray(node.children)) return
      for (let i = 0; i < node.children.length; i++) {
        const child = node.children[i]
        if (child.type === 'inlineMath') {
          const s = child.position?.start?.offset
          const e = child.position?.end?.offset
          if (typeof s !== 'number' || typeof e !== 'number') continue
          const raw = source.slice(s, e) // 含两侧定界符
          const inner = raw.slice(1, -1)
          if (inner !== inner.trim()) {
            node.children[i] = { type: 'text', value: raw } // 假阳性：还原为原文
          }
        } else {
          walk(child)
        }
      }
    }
    walk(tree)
  }
}

// ==================== 内联 HTML（README 的常态写法） ====================
// 背景（用户实测缺陷）：markdown 里直接写 HTML 是 README 的常态——居中标题、<picture>
// 双主题 logo、<br>、<details>、徽标 <img>。react-markdown 不接 rehype-raw 时会把这些
// **转义成文本**显示，于是「预览」出来的是一屏 `<p align="center">…` 源码（不渲染、也不报错）。
//
// 为什么不是直接上 rehype-raw：它把解析结果与 markdown 自己生成的节点混进同一棵 hast 树，
// 之后就再也分不清「哪段来自原始 HTML」。而两者必须区别对待：
//   · 原始 HTML 是不可信内容（模型产出、clone 下来的任意仓库都算）→ 必须过白名单 sanitize；
//   · markdown 生成的节点不能被 sanitize 波及：KaTeX 靠 `.math-inline` 认公式、围栏靠
//     `language-*` 认语言、GFM 脚注靠 id 互链——整树 sanitize 会把这些剥掉（功能静默失效），
//     而放宽 schema 到「什么都不剥」等于没 sanitize。
// 所以这里只做「raw 节点 → 解析 → sanitize → 就地替换」，其余节点一个字节不碰。

// 白名单：以 hast-util-sanitize 的 GitHub 默认 schema 为底（**未显式给出的键自动沿用默认值**，
// 所以这里只写默认不含、而 README 真用得上的部分）。默认已含且够用的：p/div/span/h1-h6/br/hr/
// a/img/picture/source/table 全家/ul/ol/li/dl/details/summary/kbd/ruby/sub/sup/ins/del…
const HTML_SCHEMA: Schema = {
  // 占位注释必须留下（整篇重解析的拼回靠它们，见 rehypeRawHtml）；作者自己写的注释会在拼回时
  // 被丢掉，不会进 DOM。
  allowComments: true,
  tagNames: [
    ...(defaultSchema.tagNames ?? []),
    'figure', 'figcaption', 'mark', 'small', 'u', 'abbr', 'time', 'caption', 'col', 'colgroup', 'wbr',
    // 内联 SVG（仓库自画的 logo / 示意图）。只放结构标签：不放 <foreignObject>（能把 HTML 塞回
    // SVG，等于给白名单开后门）、不放 <use>/<image>（可引用外部资源）、不放 <animate*>（可改属性）。
    'svg', 'g', 'path', 'circle', 'ellipse', 'rect', 'line', 'polyline', 'polygon',
    'text', 'tspan', 'defs', 'linearGradient', 'radialGradient', 'stop', 'clipPath', 'title', 'desc',
  ],
  attributes: {
    ...defaultSchema.attributes,
    // img：默认只放 src（宽度靠 `*` 里的 width/height 是够的，但 srcset/loading/decoding 要显式开）
    img: [...(defaultSchema.attributes?.img ?? []), 'srcSet', 'loading', 'decoding'],
    // source：默认只有 srcSet（<picture> 的 media 是挑图依据，必须留）
    source: ['srcSet', 'media', 'type', 'width', 'height'],
    svg: ['xmlns', 'viewBox', 'width', 'height', 'fill', 'stroke', 'role', 'ariaLabel', 'ariaHidden', 'focusable'],
    g: ['fill', 'stroke', 'strokeWidth', 'transform', 'opacity', 'clipPath'],
    path: ['d', 'fill', 'stroke', 'strokeWidth', 'strokeLinecap', 'strokeLinejoin', 'fillRule', 'clipRule', 'fillOpacity', 'strokeOpacity', 'opacity', 'transform'],
    circle: ['cx', 'cy', 'r', 'fill', 'stroke', 'strokeWidth', 'fillOpacity', 'opacity'],
    ellipse: ['cx', 'cy', 'rx', 'ry', 'fill', 'stroke', 'strokeWidth', 'opacity'],
    rect: ['x', 'y', 'width', 'height', 'rx', 'ry', 'fill', 'stroke', 'strokeWidth', 'opacity'],
    line: ['x1', 'y1', 'x2', 'y2', 'stroke', 'strokeWidth'],
    polyline: ['points', 'fill', 'stroke', 'strokeWidth'],
    polygon: ['points', 'fill', 'stroke', 'strokeWidth'],
    text: ['x', 'y', 'dx', 'dy', 'fontSize', 'fontFamily', 'fontWeight', 'fill', 'textAnchor', 'transform'],
    tspan: ['x', 'y', 'dx', 'dy', 'fill', 'fontSize'],
    stop: ['offset', 'stopColor', 'stopOpacity'],
    linearGradient: ['id', 'x1', 'y1', 'x2', 'y2', 'gradientUnits', 'gradientTransform'],
    radialGradient: ['id', 'cx', 'cy', 'r', 'fx', 'fy', 'gradientUnits', 'gradientTransform'],
    clipPath: ['id'],
  },
  protocols: {
    ...defaultSchema.protocols,
    // data: 是**条件**放行的：protocols 只按 scheme 判定，分辨不出 data:image/png 与
    // data:text/html（后者在 <img> 里虽不执行，但别留这个口子）→ 见 stripUnsafeDataUrls。
    src: [...(defaultSchema.protocols?.src ?? []), 'data'],
    href: [...(defaultSchema.protocols?.href ?? []), 'tel'],
  },
  // strip = 连内容一起删。默认只有 script；style 必须显式加（否则 CSS 会以**文本**形式留在
  // 正文里，白占篇幅还容易误导）。iframe/object/embed/form 等不在白名单里 → 会被降级成
  // 「留子节点、去标签」，这对它们是安全的（脚本/表单能力来自标签本身）。
  strip: ['script', 'style', 'iframe', 'object', 'embed', 'form', 'meta', 'link', 'base', 'template', 'noscript', 'textarea', 'button', 'select', 'option', 'audio', 'video', 'canvas'],
}

// src/srcSet 上的 data: 逐条过筛（protocols 只认单个 URL，srcSet 是逗号分隔的候选列表）。
function stripUnsafeDataUrls(node: Element) {
  for (const key of ['src', 'srcSet'] as const) {
    const raw = node.properties?.[key]
    if (typeof raw !== 'string') continue
    const parts = raw.split(',').map((s) => s.trim()).filter(Boolean)
    const kept = parts.filter((p) => !/^data:/i.test(p) || /^data:image\//i.test(p))
    if (kept.length === parts.length) continue
    if (!kept.length) delete node.properties?.[key]
    else node.properties[key] = kept.join(', ')
  }
}

// firstUrl：srcset 里第一个候选的 URL（`a.png 1x, b.png 2x` → a.png）。
function firstUrl(srcset: unknown): string | undefined {
  const first = String(srcset ?? '').split(',')[0]?.trim().split(/\s+/)[0]
  return first || undefined
}

// <picture> 的主题选择：README 用
//   <source media="(prefers-color-scheme: dark)" srcset="logo-dark.svg"><img src="logo-light.svg">
// 让 logo 跟随深色偏好。但应用的深/亮主题与系统的 prefers-color-scheme 是两回事（系统亮色下
// 照样可以选深色主题），照媒体查询走会挑到与周围底色打架的那一张。这里按**应用当前主题**挑定
// 一张，退化成普通 <img>：往后就没有「媒体查询」这回事了。
function pickPicture(pic: Element, dark: boolean): Element | null {
  const kids = pic.children.filter((c): c is Element => c.type === 'element')
  const img = kids.find((c) => c.tagName === 'img')
  let src: string | undefined
  for (const s of kids.filter((c) => c.tagName === 'source')) {
    const media = String(s.properties?.media ?? '')
    if (!/prefers-color-scheme/i.test(media)) continue // 尺寸/方向类媒体查询不表态：跳过
    // `(prefers-color-scheme: dark)` 命中深色；`not (... dark)` 命中浅色
    const matchesDark = /dark/i.test(media) !== /\bnot\b/i.test(media)
    if (matchesDark === dark) {
      src = firstUrl(s.properties?.srcSet)
      break
    }
  }
  if (!img && !src) return null // 既没有 <img> 兜底也没有 <source>：整块丢掉（渲染不出任何东西）
  return {
    type: 'element',
    tagName: 'img',
    properties: { ...(img?.properties ?? {}), ...(src ? { src } : {}) },
    children: [],
  }
}

// 原始 HTML 的 <pre>/<code> 打标记：组件层据此区分「原始 HTML 的预排版」与「markdown 围栏」
//（围栏要走高亮 + 复制按钮 + 解包 <pre>，原始 <pre> 恰恰要保留它自己那层）。标记必须在
// sanitize **之后**加：白名单里没有它，加在前面会被剥掉。
function markRawHtml(node: Element) {
  if (node.tagName === 'pre' || node.tagName === 'code') node.properties = { ...(node.properties ?? {}), dataHaiHtml: '1' }
}

// transformRaw：**原始 HTML 派生**节点的收尾（<picture> 定型 + data: 过筛 + 打标记）。
//   · skip：从 markdown 子树原样拼回来的节点 —— 一个字节都不碰（它们不该被这条通路的规则碰到，
//     例如围栏的 <pre> 一旦被当成「原始 HTML 的 pre」就会丢掉高亮与复制按钮）。
//   · descend：**容器**是 markdown 节点、但子节点里有原始 HTML（单行 `<p align=center>…</p>`
//     那种拆标签写法）→ 容器本身不碰，只继续往下处理子节点。
function transformRaw(
  node: RootContent,
  dark: boolean,
  skip: Set<Nodes>,
  descend: Set<Nodes>,
): RootContent | null {
  if (node.type !== 'element') return node
  if (skip.has(node)) return node
  if (descend.has(node)) {
    node.children = node.children
      .map((c) => transformRaw(c as RootContent, dark, skip, descend))
      .filter((c): c is ElementContent => c !== null)
    return node
  }
  if (node.tagName === 'picture') {
    const img = pickPicture(node, dark)
    return img ? transformRaw(img, dark, skip, descend) : null
  }
  markRawHtml(node)
  stripUnsafeDataUrls(node)
  node.children = node.children
    .map((c) => transformRaw(c as RootContent, dark, skip, descend))
    .filter((c): c is ElementContent => c !== null)
  return node
}

// 占位注释的取值：`md-3` 单个 markdown 节点；`md-o3` / `md-c3` 包住「内部还有行内 HTML」的
// markdown 节点。前缀带 md- 是为了与作者自己写的注释区分开。
const PH_SINGLE = /^md-(\d+)$/
const PH_OPEN = /^md-o(\d+)$/
const PH_CLOSE = /^md-c(\d+)$/
const commentValue = (n: RootContent) => (n.type === 'comment' ? String(n.value ?? '').trim() : '')

// rehypeRawHtml：把 markdown 里的内联/块级 HTML 变成真实元素（含白名单 sanitize）。
// 主题变化要重跑（<picture> 挑图），故做成 options 形式由组件传 dark。
//
// 为什么要**整篇重解析**、而不是「一个 raw 节点单独解析一次」：markdown 的**行内** HTML 在
// remark 里是**按标签拆开**的 —— `<p align="center"><b>标题</b></p>` 是 raw(`<p …>`) + raw(`<b>`)
// + text(标题) + raw(`</b>`) + raw(`</p>`) 五个兄弟节点。逐个解析只会得到三个各自闭合的残片：
// 居中丢了、粗体丢了（README 里单行写法的居中块正好踩这条，实测过）。
// 思路与 rehype-raw 一致（非 raw 节点换成占位符 → 整段重新解析 → 拼回），多出来的那一层信息是
// 「哪些节点是从 markdown 拼回来的」：sanitize 只该落在原始 HTML 上（理由见本节开头）。
function rehypeRawHtml(options: { dark: boolean }) {
  return (tree: Root) => {
    const stash: RootContent[] = []
    const parts: string[] = []
    let hasRaw = false
    const emit = (nodes: RootContent[]) => {
      for (const n of nodes) {
        if (n.type === 'raw') {
          hasRaw = true
          parts.push(String(n.value ?? '')) // 原文照抄：配对由解析器一次完成
          continue
        }
        const kids = Array.isArray((n as Element).children) ? (n as Element).children as RootContent[] : undefined
        const id = stash.push(n) - 1
        if (kids?.some((k) => k.type === 'raw')) {
          // 子树里还有 raw（拆标签那种）→ 用开/闭占位符包住，让里面的标签参与同一次解析
          parts.push(`<!--md-o${id}-->`)
          emit(kids)
          parts.push(`<!--md-c${id}-->`)
        } else {
          parts.push(`<!--md-${id}-->`) // 整块占位：markdown 的块级结构不卷进 HTML 解析
        }
      }
    }
    emit(tree.children as RootContent[])
    if (!hasRaw) return // 没有内联 HTML（绝大多数正文）：一个字节都不动
    const parsed = fromHtml(parts.join(''), { fragment: true })
    // 此刻树里只有原始 HTML + 占位注释 → 正好可以整树 sanitize（占位注释要留：allowComments）
    const clean = sanitize(parsed as Nodes, HTML_SCHEMA) as Root
    const skip = new Set<Nodes>() // 原样拼回的 markdown 子树
    const descend = new Set<Nodes>() // 容器是 markdown、子节点里有原始 HTML
    // 拼回：占位注释 → 原子树；其余（原始 HTML 派生）留在树上等 transformRaw 收尾。
    const rebuild = (nodes: RootContent[]): RootContent[] => {
      const out: RootContent[] = []
      for (let i = 0; i < nodes.length; i++) {
        const n = nodes[i]
        const v = commentValue(n)
        const open = PH_OPEN.exec(v)
        if (open) {
          // 收集到配对的 md-cN（含嵌套）
          let depth = 1
          const inner: RootContent[] = []
          for (i++; i < nodes.length; i++) {
            const d = nodes[i]
            const dv = commentValue(d)
            if (PH_OPEN.test(dv)) depth++
            else if (PH_CLOSE.test(dv) && --depth === 0) break
            inner.push(d)
          }
          const original = stash[Number(open[1])]
          const merged = { ...original, children: rebuild(inner) } as RootContent
          descend.add(merged)
          out.push(merged)
          continue
        }
        const single = PH_SINGLE.exec(v)
        if (single) {
          const original = stash[Number(single[1])]
          skip.add(original)
          out.push(original)
          continue
        }
        if (n.type === 'comment') continue // 作者自己写的注释：纯噪音，不留
        if (n.type === 'element') {
          out.push({
            ...n,
            children: rebuild(n.children as RootContent[]).filter((c) => c.type === 'element' || c.type === 'text'),
          } as ElementContent as RootContent)
          continue
        }
        out.push(n)
      }
      return out
    }
    tree.children = rebuild(clean.children as RootContent[]) as Root['children']
    tree.children = (tree.children as RootContent[])
      .map((c) => transformRaw(c, options.dark, skip, descend))
      .filter((c): c is RootContent => c !== null)
  }
}

// ==================== 标题锚点 / GitHub alerts / frontmatter ====================

// 标题 id：README 的目录（`[架构](#架构)`）要能点得动，脚注回链之外的锚点也得有落点。
// 前缀 `user-content-` 与 GitHub 一致，也正是 hast-util-sanitize 的 clobber 前缀——于是
// 「markdown 标题」「原始 HTML 里手写的 id」落在同一个命名空间里，也不会撞上应用自己的 DOM id
//（如 React 挂载点 #root）。链接侧的改写见组件里的 a 覆盖。
function rehypeHeadingIds() {
  return (tree: Root) => {
    const slugger = new GithubSlugger()
    const walk = (node: Root | Element) => {
      if (node.type === 'element' && /^h[1-6]$/.test(node.tagName)) {
        const id = node.properties?.id
        if (typeof id !== 'string' || !id) {
          node.properties = { ...(node.properties ?? {}), id: 'user-content-' + slugger.slug(headingText(node)) }
        }
      }
      for (const child of node.children) if (child.type === 'element') walk(child)
    }
    walk(tree)
  }
}

// headingText：标题的纯文本（含行内 code 与图片 alt —— GitHub 的 slug 也把 alt 算进去）。
function headingText(node: Nodes): string {
  let s = node.type === 'text' ? node.value : ''
  if (node.type === 'element' && typeof node.properties?.alt === 'string') s += ' ' + node.properties.alt
  for (const c of 'children' in node ? node.children : []) {
    if (c.type === 'text' || c.type === 'element') s += headingText(c)
  }
  return s
}

const ALERT_KINDS = ['note', 'tip', 'important', 'warning', 'caution']

// GitHub alerts（`> [!NOTE]` 起头的引用块）→ 带类型标题的提示块。
// 自写而不引插件包：标题文案要跟界面语言走（i18n），配色要落在既有主题 token 上——引一个包
// 这两件仍得自己做，不如把那 30 行留在本地。
// 结构走 mdast 的 data.hName/hProperties（mdast-util-to-hast 的官方逃生口），**不拼 HTML 字符串**：
// 拼字符串就得自己转义，那正是 XSS 的老入口。
function remarkGithubAlerts(labels: Record<string, string>) {
  return (tree: MdNode) => {
    const walk = (node: MdNode) => {
      node.children?.forEach(walk)
      if (node.type !== 'blockquote' || !node.children?.length) return
      const first = node.children[0]
      if (first.type !== 'paragraph' || !first.children?.length) return
      const head = first.children[0]
      if (head.type !== 'text' || typeof head.value !== 'string') return
      const m = /^\[!([a-zA-Z]+)\][ \t]*\r?\n?/.exec(head.value)
      const kind = m ? m[1].toLowerCase() : ''
      if (!m || !ALERT_KINDS.includes(kind)) return
      // 标记行整行吃掉；其后若是软换行也一并去掉（否则标题下会多一个空行）
      const rest = head.value.slice(m[0].length)
      if (rest) head.value = rest
      else {
        first.children.shift()
        if (first.children[0]?.type === 'break') first.children.shift()
      }
      if (!first.children.length) node.children.shift()
      node.data = { ...(node.data ?? {}), hName: 'div', hProperties: { className: ['md-alert', 'md-alert-' + kind] } }
      node.children.unshift({
        type: 'paragraph',
        data: { hName: 'div', hProperties: { className: ['md-alert-hd'] } },
        children: [{ type: 'text', value: labels[kind] || kind }],
      })
    }
    walk(tree)
  }
}

// frontmatter 解析（只认顶层 `键: 值`）。列表/多行标量/注释混排 → 返回空，调用方退回原样代码块：
// 元信息不是正文，宁可少加工，也不要把用户没写的 JSON 塞进页面（GitHub 的表格会序列化嵌套值，
// 这里选择原样显示）。
function parseFrontmatterRows(raw: string): [string, string][] {
  const rows: [string, string][] = []
  let cur: [string, string] | null = null
  const unquote = (s: string) => (/^(['"]).*\1$/.test(s.trim()) ? s.trim().slice(1, -1) : s.trim())
  for (const line of raw.replace(/\r\n?/g, '\n').split('\n')) {
    if (!line.trim() || /^\s*#/.test(line)) continue
    const m = /^([A-Za-z0-9_.$-]+)\s*:\s*(.*)$/.exec(line)
    if (m) {
      if (cur) rows.push(cur)
      cur = [m[1], unquote(m[2])]
      continue
    }
    if (cur && /^\s+\S/.test(line)) {
      cur[1] += (cur[1] ? '\n' : '') + line.trim() // 嵌套块：整块保留，不展开
      continue
    }
    return [] // 出现归不了类的行：整体放弃解析
  }
  if (cur) rows.push(cur)
  return rows
}

// frontmatter（YAML）→ 卡片。不接 remark-frontmatter 的后果是实打实的：`---\ntitle: x\n---`
// 会被当成「分隔线 + setext 标题」，README 开头的元信息直接变成正文里一行加粗大字。
function remarkFrontmatterCard(labels: Record<string, string>) {
  return (tree: MdNode) => {
    const walk = (children: MdNode[]) => {
      for (let i = 0; i < children.length; i++) {
        const node = children[i]
        if (node.type === 'yaml' || node.type === 'toml') {
          children[i] = frontmatterCard(String(node.value ?? ''), labels)
          continue
        }
        if (node.children) walk(node.children)
      }
    }
    walk(tree.children ?? [])
  }
}

function frontmatterCard(raw: string, labels: Record<string, string>): MdNode {
  const rows = parseFrontmatterRows(raw)
  const body: MdNode[] = rows.length
    ? rows.map(([k, v]) => ({
      type: 'paragraph',
      data: { hName: 'div', hProperties: { className: ['md-fm-row'] } },
      children: [
        { type: 'text', value: k, data: { hName: 'span', hProperties: { className: ['md-fm-k'] } } },
        { type: 'text', value: v, data: { hName: 'span', hProperties: { className: ['md-fm-v'] } } },
      ],
    }))
    : [{ type: 'code', value: raw.replace(/\n+$/, '') }] // 解析不出键值：原样代码块（带复制按钮）
  return {
    type: 'paragraph',
    data: { hName: 'div', hProperties: { className: ['md-fm'] } },
    children: [
      {
        type: 'paragraph',
        data: { hName: 'div', hProperties: { className: ['md-fm-hd'] } },
        children: [{ type: 'text', value: labels.frontmatter || 'frontmatter' }],
      },
      ...body,
    ],
  }
}


// Mermaid 图表（flowchart / 时序图 / 类图 / 甘特图 / 饼图 / ER / 思维导图…）。
// 识别 ```mermaid 围栏 → 渲染为 SVG；语法错误回退显示源码（+ 错误提示）。
// 主题随应用深/亮切换重渲；mermaid 要求 id 全局唯一 → 每次渲染用递增计数器。
let mermaidSeq = 0
function MermaidBlock({ code }: { code: string }) {
  const t = useT()
  const resolved = useResolvedTheme()
  const [svg, setSvg] = useState('')
  const [error, setError] = useState('')
  const idRef = useRef(`mermaid-${++mermaidSeq}`)

  useEffect(() => {
    let alive = true
    setSvg('')
    setError('')
    // mermaid 渲染是异步的（内部走 worker/布局），失败（语法错误/超时）回退源码
    getMermaid().then(async (m) => {
      if (!alive) return
      try {
        const mm = m.default || m
        // 首次使用时初始化（主题跟随；后续 render 复用配置）
        const isDark = resolved === 'ink'
        mm.initialize({
          startOnLoad: false,
          theme: isDark ? 'dark' : 'default',
          securityLevel: 'loose', // 允许 flowchart 中的 HTML 标签（常用）
          fontFamily: 'var(--font-mono)',
        })
        const { svg } = await mm.render(idRef.current, code)
        if (alive) setSvg(svg)
      } catch (e) {
        if (alive) setError(e instanceof Error ? e.message : String(e))
      }
    })
    return () => {
      alive = false
    }
  }, [code, resolved])

  if (error) {
    return (
      <div className="mermaid-block mermaid-error">
        <div className="mermaid-err-hd">{t('md.mermaidError')}</div>
        <pre className="cb-body cb-plain"><code>{code}</code></pre>
      </div>
    )
  }
  if (svg) {
    return <div className="mermaid-block" dangerouslySetInnerHTML={{ __html: svg }} />
  }
  // 渲染中：先显示源码，避免空白闪烁
  return (
    <div className="codeblock">
      <div className="cb-hd"><span className="cb-lang">mermaid</span></div>
      <pre className="cb-body cb-plain"><code>{code}</code></pre>
    </div>
  )
}

// 复制到剪贴板 + 结果反馈（三处代码块头部共用：mermaid / html / 普通代码块）。
// 为什么不能静默失败（2026-09-13 实测教训）：复制失败必须让用户看得见，否则无法区分
// 「没点上」与「坏了」——曾出现各处 catch{} 吞错、用户只看到「点了没反应」。
// 为什么走 copyText 而不是裸 navigator.clipboard（2026-09-15 实测更正）：Electron 下写剪贴板
// 的权限名随「是否用户手势」漂移（真实点击 → clipboard-sanitized-write），只放行
// clipboard-read 时点击必抛 NotAllowedError；copyText 内含主进程兜底，失败也是真失败。
type CopyState = 'idle' | 'ok' | 'fail'
function useCopy(text: string): { state: CopyState; copy: () => Promise<void> } {
  const [state, setState] = useState<CopyState>('idle')
  const timerRef = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(timerRef.current), [])
  const copy = async () => {
    const ok = await copyText(text)
    setState(ok ? 'ok' : 'fail')
    window.clearTimeout(timerRef.current)
    timerRef.current = window.setTimeout(() => setState('idle'), 1600)
  }
  return { state, copy }
}

/** 复制按钮文案：统一三态（idle / 已复制 / 复制失败），三处代码块头部共用。 */
function copyLabel(t: (k: string) => string, state: CopyState, idleKey = 'md.copy'): string {
  if (state === 'ok') return t('md.copied')
  if (state === 'fail') return t('md.copyFail')
  return t(idleKey)
}

// 注入骨架的「主题画布」：iframe 内拿不到应用的 CSS 变量，所以按当前解析出的主题
// 烘一份最小样式进去。做三件事，都是**低侵入**的：
//   ① 画布：body 底色/文字色/字体栈 = 与应用对话区同源（深色主题不再是一块刺眼白底，
//      正是用户反馈的「不贴合主题」）；未写样式的裸产出也因此读得下去。
//   ② 设计令牌：把应用主题映射成 --hai-* 变量暴露给内容（bg/surface/text/text-2/line/
//      accent）。模型只要用 var(--hai-*) 写样式，**深/亮两套主题都自动成立** —— 这是
//      「贴合系统主题」能真正成立的关键：模型无法在 iframe 里读到应用主题，只能靠这层。
//   ③ color-scheme：让原生控件/滚动条跟随主题（否则深色下内容里的滚动条仍是亮色）。
// 只落在 :root/body 与「完全没样式时的 table 兜底」上；模型自己的规则在同优先级下
// 后声明即覆盖（我们不使用 !important，也不碰任何后代元素）。
// readHaiTokens：把应用当前主题的 token 读成预览 iframe 用的那一组。
// 只在浏览器里读得到（getComputedStyle）；读不到就用兜底（测试/SSR）。
const HAI_TOKEN_FALLBACK: Record<'dark' | 'light', Record<'bg' | 'surface' | 'text' | 'text2' | 'line' | 'accent', string>> = {
  dark: { bg: '#1a1614', surface: '#231d1a', text: '#f0e9e4', text2: '#ada19a', line: '#42382f', accent: '#d97a66' },
  light: { bg: '#fdfcfb', surface: '#ffffff', text: '#1b1917', text2: '#5e5955', line: '#d9d2cc', accent: '#b23a2c' },
}
function readHaiTokens(dark: boolean) {
  const fb = HAI_TOKEN_FALLBACK[dark ? 'dark' : 'light']
  if (typeof document === 'undefined') return fb
  const cs = getComputedStyle(document.documentElement)
  const read = (name: string, fallback: string) => cs.getPropertyValue(name).trim() || fallback
  return {
    bg: read('--paper', fb.bg),
    surface: read('--paper-2', fb.surface),
    text: read('--ink', fb.text),
    text2: read('--ink-2', fb.text2),
    line: read('--ink-line-2', fb.line),
    accent: read('--cinnabar-2', fb.accent),
  }
}

function canvasCss(theme: 'ink' | 'paper'): string {
  const dark = theme !== 'paper'
  // 色值**读当前真实 token**（--paper/--paper-2/--ink/--ink-2/--ink-line-2/--cinnabar-2），
  // 不再抄一份 hex：抄的那份必然随主题改动整体过期 —— 2026-09-21 换朱砂主题时它就过期了一次
  //（抄的是旧的黄褐 + 砖红）。读计算值还顺带覆盖"用户自定义亮色底色"这条路径。
  // 兜底值只用于 SSR/测试这类没有 document 的场景。
  const t = readHaiTokens(dark)
  return `:root{color-scheme:${dark ? 'dark' : 'light'};` +
    `--hai-bg:${t.bg};--hai-surface:${t.surface};--hai-text:${t.text};` +
    `--hai-text-2:${t.text2};--hai-line:${t.line};--hai-accent:${t.accent};}` +
    `body{margin:8px;background:var(--hai-bg);color:var(--hai-text);` +
    `font-family:system-ui,-apple-system,"PingFang SC",sans-serif;line-height:1.6;}` +
    // 裸 <table>（模型完全不写样式时最常见）至少有线、有内边距；有样式时会被覆盖。
    `table{border-collapse:collapse;}` +
    `th,td{border:1px solid var(--hai-line);padding:6px 10px;text-align:left;}`
}

// HTML 代码块「高级渲染」：iframe 沙箱实时预览（小型 CodePen）。
// 安全设计：sandbox="allow-scripts"（无 allow-same-origin）→ 脚本在隔离 origin
// 运行，访问不到主应用 DOM/localStorage/cookie；无 allow-top-navigation → 不能
// 跳转父页面。与「直接 dangerouslySetInnerHTML 执行 HTML」相比，这是唯一安全的
// 实时预览方式（直接注入会让任意 <script> 摸到应用内部）。
// UI：卡片头「渲染 / 源码」切换；默认渲染；**默认无壳**（不描边框、不显工具条，
// 悬停才浮出，见 index.css .htmlblock 段）。高度自适应：srcdoc 注入探针脚本
// （ResizeObserver + postMessage 上报 body 高度）→ 矮内容自然收缩、高内容封顶
// 560px 内滚（完整可滚动查看；如需右侧浏览器独立加载，见 write_file 工具卡的
// 「打开」——只有真实落盘文件才提供，代码块本身不猜路径）。
//
// 悬停为何不能只用 CSS（实测结论，别改回纯 CSS）：隔离 origin 的 iframe 是独立
// 进程（OOPIF），**指针在其上方时父文档收不到任何指针事件、父容器 :hover 也不命中**
// （普通同源 iframe 会命中）。而这块区域 99% 的时间被预览 iframe 占满 → 纯 CSS
// 悬停等于「永远不浮出」。故复用已有的探针通道：子文档 mousemove → postMessage
// 上报 → 父组件置 frameHover；「移出」则由父文档侧反推（能收到父文档指针事件 =
// 指针已不在 iframe 上）。回归见 e2e/verify_html_preview_chrome.mjs。
function HtmlPreviewBlock({ code }: { code: string }) {
  const t = useT()
  const resolved = useResolvedTheme()
  const ap = useAppearance()
  const codeTheme = resolved === 'paper' ? ap.codeThemeLight : ap.codeThemeDark
  const [mode, setMode] = useState<'preview' | 'source'>('preview')
  const { state: copyState, copy } = useCopy(code)
  const [html, setHtml] = useState('')
  const [height, setHeight] = useState<number | undefined>(undefined) // iframe 实测高度（未测前 undefined → CSS 兜底 240px）
  const [frameHover, setFrameHover] = useState(false) // 指针在预览 iframe 内容上方（子探针上报）
  const [probeDead, setProbeDead] = useState(false) // 子探针没跑起来（如 HTML 自带 CSP script-src 'none'）→ 退回恒显
  const frameRef = useRef<HTMLIFrameElement>(null)
  const blockRef = useRef<HTMLDivElement>(null)
  const probeSeenRef = useRef(false) // 本次 srcDoc 是否收到过探针上报
  const MAX_H = 560 // 预览高度上限：超过则 iframe 内滚（完整可看）

  // 组装 iframe 文档：完整 <html> 直接用；片段（无 <html>）包基本骨架。
  // 末尾注入探针脚本（opaque origin 下跨域通信的唯一通道），上报两件事：
  //   ① 高度：DOMContentLoaded/load/ResizeObserver 三路触发，报 body 内容高度。
  //      注意：必须量 body.scrollHeight 而非 documentElement.scrollHeight —— 后者在
  //      body 内容矮于 iframe 视口时会被视口高度撑开（如视口 240 时恒报 246），
  //      导致矮内容无法收缩。body.scrollHeight 反映真实内容高度。
  //   ② 悬停：指针进入/在预览内移动时上报（见上「悬停为何不能只用 CSS」）。
  //      节流 150ms：移动中每 150ms 一次足够维持浮出态，又不至于刷屏。
  //
  // 主题底色（2026-09-13 增补）：**注入骨架的主题画布**。iframe 看不到应用的 CSS 变量，
  // 所以随主题把「最近一次解析出的主题值」烘进骨架 style —— 深色主题给深色画布、亮色给
  // 亮色，让「模型没写 body 背景」的片段也与对话区融为一体（此前恒白底：深色主题下
  // 是一块刺眼的白，正是用户反馈的「不像一体」）。**不覆盖模型自己的样式**：画布只落在
  // <body> 上且是普通声明，模型写了 body{background} 会自然覆盖（同优先级、后者胜）。
  // 另外给上默认字体栈/行高/文字色，让「完全没写样式」的产出也读得下去。
  const doc = useMemo(() => {
    const probe = '<script>(function(){' +
      'function h(){try{parent.postMessage({__htmlh:document.body.scrollHeight||0},\'*\')}catch(e){}}' +
      'h();' + // 立即报一次：让父组件尽早确认「探针活着」（探针失效兜底判定用，见下）
      'document.addEventListener(\'DOMContentLoaded\',h);' +
      'window.addEventListener(\'load\',function(){h();setTimeout(h,100);setTimeout(h,400)});' +
      'if(window.ResizeObserver){var ro=new ResizeObserver(h);window.addEventListener(\'load\',function(){ro.observe(document.body)})}' +
      'else{setInterval(h,400)}' +
      'var ht=0;function hv(){var n=Date.now();if(n-ht<150)return;ht=n;try{parent.postMessage({__htmlhover:1},\'*\')}catch(e){}}' +
      'window.addEventListener(\'mousemove\',hv,true);' +
      'window.addEventListener(\'mouseover\',hv,true);' +
      '})();</script>'
    const base = /<html[\s>]/i.test(code)
      ? code
      : `<!doctype html><html><head><meta charset="utf-8"><style>${canvasCss(resolved)}</style></head><body>${code}</body></html>`
    // 探针插到 </body> 前（无则追加末尾）；避免破坏用户文档结构
    return /<\/body>/i.test(base) ? base.replace(/<\/body>/i, probe + '</body>') : base + probe
  }, [code, resolved])

  // 代码变化 → 重置高度等待重新测量；同时清掉已高亮的源码（否则源码 tab 会一直
  // 显示「上一次 code 的高亮结果」——流式增长/内容变化后切到源码 tab 看到的是旧内容）。
  // 注意不动 frameHover：流式下 srcDoc 会反复重载，若跟着复位，指针停在预览上时
  // 工具条会跟着闪。
  useEffect(() => {
    setHeight(undefined)
    setHtml('')
    probeSeenRef.current = false
    setProbeDead(false)
    // 探针兜底：HTML 里若自带 CSP（如 script-src 'none'）或脚本被禁，注入的探针不会执行
    // → 收不到任何上报 → 悬停信号永远没有，工具条就再也点不到。这种文档退回「恒显」
    // （= 改动前的行为），保证功能不丢。总开关在 .probe-dead 上。
    const timer = setTimeout(() => { if (!probeSeenRef.current) setProbeDead(true) }, 2500)
    return () => clearTimeout(timer)
  }, [code])

  // 接收 iframe 内探针上报（校验来源：只认本组件 iframe）：高度 + 悬停。
  useEffect(() => {
    const onMsg = (e: MessageEvent) => {
      if (e.source !== frameRef.current?.contentWindow) return
      const d = e.data as { __htmlh?: number; __htmlhover?: number } | null
      if (!d || (d.__htmlh === undefined && d.__htmlhover === undefined)) return
      probeSeenRef.current = true
      const h = d.__htmlh
      if (typeof h === 'number' && h > 0) setHeight(Math.min(Math.max(h + 2, 40), MAX_H))
      if (d.__htmlhover === 1) setFrameHover(true)
    }
    window.addEventListener('message', onMsg)
    return () => window.removeEventListener('message', onMsg)
  }, [])

  // 悬停「退出」的判定：指针在 iframe 上方时父文档收不到任何指针事件，所以
  // **能收到**父文档指针事件 == 指针已不在 iframe 上；此时若已移出整块（工具条浮层
  // 之外）→ 收起。只在浮出期间挂监听（无悬停时零开销）。
  // 另外三处兜底：① 滚动会把整块从指针下挪走却不产生指针事件；② 指针移出应用窗口
  // （最后一个指针事件仍在块内，之后就没有事件了）；③ 窗口失焦。若指针其实还在预览
  // 上，下一次移动会由子探针重新点亮（宁可短暂消失，也不留一个悬空的工具条）。
  useEffect(() => {
    if (mode !== 'preview' || !frameHover) return
    const onMove = (e: PointerEvent) => {
      const r = blockRef.current?.getBoundingClientRect()
      if (!r) return
      const inside = e.clientX >= r.left && e.clientX <= r.right && e.clientY >= r.top && e.clientY <= r.bottom
      if (!inside) setFrameHover(false)
    }
    const off = () => setFrameHover(false)
    window.addEventListener('pointermove', onMove, true)
    window.addEventListener('scroll', off, true)
    document.addEventListener('mouseleave', off) // 指针离开文档（含移出窗口）
    window.addEventListener('blur', off)
    return () => {
      window.removeEventListener('pointermove', onMove, true)
      window.removeEventListener('scroll', off, true)
      document.removeEventListener('mouseleave', off)
      window.removeEventListener('blur', off)
    }
  }, [mode, frameHover])

  // 离开预览模式 → 清掉悬停态：否则从源码切回渲染时，浮层会「无悬停自显」。
  useEffect(() => {
    if (mode !== 'preview') setFrameHover(false)
  }, [mode])

  // 源码高亮（切到源码 tab 时才需要，懒算省一次高亮）；
  // 高亮失败（shiki chunk 加载失败等）→ 回退未高亮源码，不让 tab 空白。
  useEffect(() => {
    let alive = true
    if (mode !== 'source' || html) return
    codeToHtml(code, 'html', resolved)
      .then((h) => { if (alive) setHtml(h) })
      .catch(() => { /* 高亮不可用：源码 tab 走 <pre> 兜底 */ })
    return () => { alive = false }
  }, [mode, code, resolved, codeTheme, html])

  // 右侧侧边栏展示用的干净文档：与 doc 同构但**不含探针脚本**（探针只为对话内
  // 自适应高度服务，侧边栏全尺寸展示不需要，避免 postMessage 干扰）。
  const cleanDoc = useMemo(() => {
    return /<html[\s>]/i.test(code)
      ? code
      : `<!doctype html><html><head><meta charset="utf-8"><style>${canvasCss(resolved)}</style></head><body>${code}</body></html>`
  }, [code, resolved])
  const openInBrowser = () => {
    // 未落盘的纯展示代码块：内容本身即展示对象 → 右侧侧边栏（web tab）展示。
    // 有落盘文件的场景由 write_file 工具卡「打开」负责（file://），此处不猜路径。
    void useAppStore.getState().openHtmlContentInBrowser(cleanDoc)
  }

  const frame = (
    <iframe
      ref={frameRef}
      className="htmlblock-frame"
      sandbox="allow-scripts"
      srcDoc={doc}
      style={height != null ? { height } : undefined}
      title={t('md.htmlPreview')}
    />
  )

  return (
    <div ref={blockRef} className={`htmlblock ${mode === 'source' ? 'source-mode' : ''} ${frameHover ? 'hd-on' : ''} ${probeDead ? 'probe-dead' : ''}`}>
      <div className="htmlblock-hd">
        <span className="htmlblock-tabs" role="group" aria-label={t('md.htmlTabsAria')}>
          <button className={mode === 'preview' ? 'on' : ''} onClick={() => setMode('preview')} aria-pressed={mode === 'preview'}>{t('md.htmlPreview')}</button>
          <button className={mode === 'source' ? 'on' : ''} onClick={() => setMode('source')} aria-pressed={mode === 'source'}>{t('md.htmlSource')}</button>
        </span>
        <span className="grow" />
        {mode === 'preview' && (
          <button className="cb-copy htmlblock-open-btn" onClick={openInBrowser} title={t('md.htmlOpenBrowser')}>
            {t('md.htmlOpenBrowserBtn')}
          </button>
        )}
        <button className="cb-copy" onClick={copy} aria-label={t('md.copyAria')}>
          {copyLabel(t, copyState)}
        </button>
      </div>
      {mode === 'preview' ? (
        frame
      ) : html ? (
        <div className="cb-body" dangerouslySetInnerHTML={{ __html: html }} />
      ) : (
        <pre className="cb-body cb-plain"><code>{code}</code></pre>
      )}
    </div>
  )
}

// 代码块：语言标签 + 复制按钮 + shiki 高亮（异步加载，先出原文再替换为高亮）；
// 随主题切换重渲（深/亮用不同 shiki 主题，见 highlight.ts）。
function CodeBlock({ lang, code }: { lang: string; code: string }) {
  const t = useT()
  const [html, setHtml] = useState('')
  const { state: copyState, copy } = useCopy(code)
  const resolved = useResolvedTheme()
  const ap = useAppearance()
  const codeTheme = resolved === 'paper' ? ap.codeThemeLight : ap.codeThemeDark

  useEffect(() => {
    let alive = true
    // 行号：≥2 行的块才加 gutter（单行片段挂个「1」是噪音，参考稿是带行号的多行块）。
    codeToHtml(code, lang, resolved, code.split('\n').length >= 2)
      .then((h) => { if (alive) setHtml(h) })
      .catch(() => { /* shiki 不可用：保留未高亮 <pre> 原文（不白屏、不吞异常） */ })
    return () => {
      alive = false
    }
  }, [code, lang, resolved, codeTheme])

  return (
    <div className="codeblock">
      <div className="cb-hd">
        <span className="cb-lang">{lang || 'text'}</span>
        <button className="cb-copy" onClick={copy} aria-label={t('md.copyAria')}>
          {copyLabel(t, copyState)}
        </button>
      </div>
      {html ? (
        <div className="cb-body" dangerouslySetInnerHTML={{ __html: html }} />
      ) : (
        <pre className="cb-body cb-plain">
          <code>{code}</code>
        </pre>
      )}
    </div>
  )
}
// 失败态要能**自证原因**：只写一句「无法显示」，用户和排查者都只能猜。实测踩过——应用没重启时
// 主进程里没有 fs:read-image，界面上与「文件不存在」长得一模一样（都要靠 hover 看 title 才知道）。
// 这里把主进程/transport 的报错压成一句人话。
function shortReason(error: string | undefined, t: (k: string) => string): string {
  const e = String(error ?? '')
  // Electron 侧「没有这条通道」的两种典型报错：主进程没注册 handler / preload 里没这个方法
  if (/No handler registered|is not a function|Cannot read propert/i.test(e)) return t('md.imgNoChannel')
  if (/工作区外/.test(e)) return t('md.imgOutside')
  if (/不存在/.test(e)) return t('md.imgMissing')
  if (/不是图片文件/.test(e)) return t('md.imgNotImage')
  if (/过大/.test(e)) return t('md.imgTooBig')
  if (/受保护/.test(e)) return t('md.imgProtected')
  return t('md.imgUnavailable')
}

// MarkdownImage：图片渲染（本地相对路径的解析见 lib/localAsset.ts）。
// 为什么单独成组件而不是写在 components.img 里：本地图要发 IPC 取 data URL（有状态、有缓存），
// 而 components 里的覆盖是**无状态**的普通函数——hooks 不能写在那里。
function MarkdownImage({ src, alt, baseDir, workspace, ...rest }: {
  src?: string
  alt?: string
  baseDir?: string
  workspace?: string
} & Record<string, unknown>) {
  const t = useT()
  const raw = String(src ?? '')
  const local = useLocalAsset(isLocalAssetSrc(raw) ? raw : undefined, baseDir, workspace)
  if (!isLocalAssetSrc(raw)) return <img src={raw} alt={alt} {...rest} /> // 远程 / 内联：原样
  const shown = alt || displayPath(local.abs, workspace) || raw
  if (local.url) return <img src={local.url} alt={alt} data-local-path={local.abs ?? ''} {...rest} />
  if (!local.abs) return <span className="md-img-missing" title={raw}>{shown}<span className="md-img-missing-hint">{t('md.imgUnresolved')}</span></span>
  if (!local.error) return <span className="md-img-pending" title={displayPath(local.abs, workspace)}>{alt || ''}</span>
  // 读不到（不存在 / 越界 / 非图片 / 过大）：把**路径与原因**摆出来。只显示一张破图，用户
  // 既不知道它是本地图、也不知道去哪找它——那等于把「预览不全」换成了「预览静默失败」。
  return (
    <span className="md-img-missing" title={`${local.abs}\n${local.error}`}>
      {shown}<span className="md-img-missing-hint">{shortReason(local.error, t)}</span>
    </span>
  )
}

// Markdown：assistant 文本渲染（GFM 表格/删除线/任务列表 + 代码高亮/复制 +
// mermaid 图表 + KaTeX 数学公式 $...$ / $$...$$ + 内联 HTML + 标题锚点 + alerts/emoji/frontmatter）。
// 流式中也实时渲染（未闭合围栏暂时按行内处理，闭合后升级为代码块）。
// memo（问题3）：流的合并渲染下，只有「正在增长的那一条」assistant 块 text 变化才重 parse。
// baseDir：引用本文的**文件所在目录**（「最终文件」预览传；对话区不传 → 本地图锚到工作区根）。
export const Markdown = memo(function Markdown({ text, baseDir }: { text: string; baseDir?: string }) {
  const lang = useLang()
  const resolved = useResolvedTheme()
  // 工作区是本地相对图的兜底锚点（对话区没有文件可锚）；它也是主进程侧的合法性判定依据。
  const workspace = useAppStore((s) => s.workspace)
  const dark = resolved !== 'paper'
  // 相对图片/链接的锚点目录：优先「文件所在目录」（文件预览传 baseDir），对话区退回工作区根。
  // 注意别把它叫成 anchor —— a 覆盖里已有一个「页内锚点 href」的同名局部变量，遮蔽过一次
  //（表现为相对链接不被解析：传进去的 base 是 undefined）。
  const assetBase = baseDir ?? workspace
  // 插件文案随语言：用 useLang 的稳定值做依赖，而不是 useT() 每次新建的函数
  //（否则 remark 插件数组每次渲染都是新身份 → 每次渲染都全量重 parse）。
  const labels = useMemo(() => ({
    note: tStatic('md.alertNote'),
    tip: tStatic('md.alertTip'),
    important: tStatic('md.alertImportant'),
    warning: tStatic('md.alertWarning'),
    caution: tStatic('md.alertCaution'),
    frontmatter: tStatic('md.frontmatter'),
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }), [lang])
  // remarkMathGuard 需拿到原文切片（定界符内侧空白判定）→ 依赖 text
  const remarkPlugins = useMemo(
    () => [
      remarkGfm,
      remarkFrontmatter, // --- YAML --- → yaml 节点（否则会被当成分隔线 + setext 标题）
      [remarkFrontmatterCard, labels],
      remarkMath,
      [remarkMathGuard, text],
      [remarkGithubAlerts, labels],
      remarkGemoji, // :rocket: → 🚀
    ] as const,
    [text, labels],
  )
  // 顺序有意：raw HTML 先定型（<picture> 要按主题挑图 + 打标记），再补标题 id，最后 KaTeX
  //（KaTeX 会产出一棵自己的元素树，排在最后就不会被前面的遍历碰到）。
  const rehypePlugins = useMemo(
    () => [[rehypeRawHtml, { dark }], rehypeHeadingIds, rehypeKatex] as const,
    [dark],
  )
  return (
    <div className="md">
      <ReactMarkdown
        remarkPlugins={remarkPlugins as never}
        rehypePlugins={rehypePlugins as never}
        components={{
          code({ className, children, node }) {
            // 原始 HTML 的 <code>（sanitize 后打了标记）：不是 markdown 围栏，不套高亮/复制/行号
            // ——它通常嵌在原始 <pre> 里，那块 <pre> 才是版式主体（见下面的 pre 覆盖）。
            if (node?.properties?.dataHaiHtml) return <code className={className}>{children as ReactNode}</code>
            const match = /language-(\w+)/.exec(className || '')
            const raw = String(children ?? '')
            // 是否「围栏/缩进代码块」（块级）——必须在剥掉尾部换行前判断：
            // remark 给块级围栏的 value 恒以 \n 结尾（单行围栏 ``` \n ls -la \n ``` 也是），
            // 而行内 code 永不含换行（remark 把行内换行折叠成空格）。
            // 旧实现先 strip 再判 \n → 单行围栏被误判成行内代码（丢复制按钮/高亮/横滚容器）。
            const isBlock = raw.includes('\n')
            const code = raw.replace(/\n$/, '')
            // mermaid 图表：```mermaid 围栏 → 渲染为 SVG（而非高亮源码）
            if (match && match[1] === 'mermaid') return <MermaidBlock code={code} />
            // HTML：iframe 沙箱实时预览（渲染/源码可切换，见 HtmlPreviewBlock）
            if (match && (match[1] === 'html' || match[1] === 'html-preview')) return <HtmlPreviewBlock code={code} />
            if (match) return <CodeBlock lang={match[1]} code={code} />
            if (isBlock) return <CodeBlock lang="text" code={code} />
            return <code className={className}>{children as ReactNode}</code>
          },
          img: ({ node: _node, src, alt, ...props }) => (
            <MarkdownImage src={src} alt={alt} baseDir={assetBase} workspace={workspace} {...props} />
          ),
          // 链接：node 是 react-markdown 注入的 hast 节点（非 DOM 属性，透传会渲染
          // node="[object Object]"）→ 解构丢弃。
          // 外链（http/https）新窗口打开；站内锚点（#footnote 等）保持同页跳转——
          // target="_blank" 会把锚点也送到新窗口（Electron 下即"开裸窗口"）。
          a: ({ node: _node, href, children, ...props }) => {
            const h = String(href ?? '')
            const isExternal = /^https?:\/\//i.test(h)
            // 站内锚点：标题 id 带 user-content- 前缀（见 rehypeHeadingIds）→ href 同参才命中；
            // 已经带前缀的（GFM 脚注回链）原样保留。
            const anchor = h.startsWith('#') && h.length > 1
              ? '#' + (h.slice(1).startsWith('user-content-') ? h.slice(1) : 'user-content-' + h.slice(1))
              : undefined
            // 相对链接 = 工作区 / 同目录里的文件（README 的「文档」「示例」链接）：点开走既有的
            // 文件面板。此前这种链接会让渲染进程去导航一个不存在的 URL —— 被主进程的
            // will-navigate 拦掉，表现为「点了没反应」。
            const target = !isExternal && !anchor && !/^[a-z][a-z0-9+.-]*:/i.test(h)
              ? resolveLocalAssetPath(h, assetBase)
              : null
            const open = target
              ? {
                href: target,
                title: target,
                onClick: (e: React.MouseEvent) => {
                  e.preventDefault()
                  useAppStore.getState().openFile(target)
                },
              }
              : { href: anchor ?? href }
            return (
              <a
                {...props}
                {...open}
                {...(isExternal ? { target: '_blank', rel: 'noreferrer' } : {})}
              >
                {children}
              </a>
            )
          },
          // 围栏块外层 <pre>：CodeBlock 自带 <pre class="cb-body">，react-markdown 再套一层
          // 会得到 <pre><div class="codeblock">…（pre 的内容模型是 phrasing content，嵌 div
          // 非法；且整段选中/全选复制时会把头部「语言标签 + 复制」一起选进剪贴板）。
          // 这里解包：块级结构由 CodeBlock 自己负责。
          // 例外：**原始 HTML 的 <pre>**（ASCII 图、手工排版）要留住它自己那层预排版 ——
          // 解包会把它的换行与等宽字体一起丢掉（见 transformRaw 里的打标记）。
          pre: ({ children, node }) => (
            node?.properties?.dataHaiHtml ? <pre>{children}</pre> : <>{children}</>
          ),
          // 表格：包一层横向滚动容器。宽表（列多 / 单元格里是不可断的长 token，
          // min-content 宽度超过对话列宽）此前会撑破 .md 后被 .narrative
          // {overflow-x:hidden} 裁掉——既滑不动也看不全；现在溢出交给表格自身横滚
          // （与代码块 .cb-body / mermaid .mermaid-block 同策略），窄表仍占满列宽。
          // node 同上：解构丢弃。
          table: ({ children, node: _node, ...props }) => (
            <div className="md-table-wrap">
              <table {...props}>{children}</table>
            </div>
          ),
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  )
})
