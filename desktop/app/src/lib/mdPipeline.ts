/* 自建 markdown 管线（替代 react-markdown 内部的「每次渲染都 parse」）。
 *
 * 为什么：react-markdown 的解析发生在它自己的 render 里（`processor.runSync`），组件一
 * 卸载就没；切会话/刷新回来必然把整条转录重新解析一遍。实测（lab/probe-reload-md.mjs）
 * 10 条 × 6k 字结构化回答：首次挂载 0.379s、切走再回来 0.234s，其中约一半是解析
 *（lab/probe-md-split.mjs：一次渲染 25.8ms 里解析占 24ms）。
 *
 * 做法：把 react-markdown 内部那两步搬出来 ——
 *   ① unified(remarkParse → remark 插件 → remarkRehype → rehype 插件).runSync → hast
 *   ② post 变换（raw 节点 → text、url 属性过 defaultUrlTransform）
 *   ③ hast-util-to-jsx-runtime → React 元素
 * 然后**缓存 ②的 hast 树**（只在非流式时缓存）：再次挂载只做 ③，跳过解析。
 *
 * 等价性由 lab/md-pipeline-equal.tsx 保证：13 类语料（表格/代码/数学/HTML/图片/脚注/
 * alert/frontmatter/emoji/长混排…）与 react-markdown 的产物**逐字节相同**。
 */
import { unified } from 'unified'
import remarkParse from 'remark-parse'
import remarkRehype from 'remark-rehype'
import { toJsxRuntime } from 'hast-util-to-jsx-runtime'
import { Fragment, jsx, jsxs } from 'react/jsx-runtime'
import { defaultUrlTransform } from 'react-markdown'
import type { Components } from 'react-markdown'
import { visit } from 'unist-util-visit'
import type { Root } from 'hast'
import type { ReactNode } from 'react'

/** 缓存预算：按**源文本字符数**计（比按条目计更贴近内存）。
    实测（lab/probe-md-cache-mem.mjs）：每 10k 字正文的 hast 树约占 0.85MB（剥掉 position 后）。
    120k 字 ≈ 10MB 封顶 —— 足够装 20 条 6k 字的长回答，又不至于把长会话的内存吃穿。 */
const MAX_CHARS = 120_000
const MAX_ENTRIES = 120

const cache = new Map<string, Root>()
let cachedChars = 0

/** URL 属性白名单（与 react-markdown 的 urlAttributes 一致）。 */
const URL_ATTRS = ['href', 'src', 'cite', 'poster', 'srcset'] as const

/** react-markdown 的 post 变换：raw → text（转义），url 属性过一遍 urlTransform。 */
function postTransform(tree: Root): Root {
  const visitor = (node: any, index?: number, parent?: any) => {
    if (node.type === 'raw' && parent && typeof index === 'number') {
      parent.children[index] = { type: 'text', value: node.value }
      return index
    }
    if (node.type === 'element' && node.properties) {
      for (const key of URL_ATTRS) {
        if (Object.hasOwn(node.properties, key)) node.properties[key] = defaultUrlTransform(String(node.properties[key] ?? ''))
      }
    }
    return undefined
  }
  visit(tree as never, visitor as never)
  return tree
}

/** 位置信息只在插件里有用；缓存时剥掉（remark-parse 的 position 占的内存比节点本身还大）。 */
function stripPositions(node: unknown): void {
  const n = node as { position?: unknown; children?: unknown[] }
  if (n && typeof n === 'object') {
    if ('position' in n) delete n.position
    if (Array.isArray(n.children)) for (const c of n.children) stripPositions(c)
  }
}

export type MdPipelineOptions = {
  /** 缓存键（null = 不缓存，例如流式） */
  key: string | null
  remarkPlugins: readonly unknown[]
  rehypePlugins: readonly unknown[]
}

/** 源文本 → hast（带跨挂载缓存）。 */
export function parseMarkdown(text: string, opts: MdPipelineOptions): Root {
  if (opts.key) {
    const hit = cache.get(opts.key)
    if (hit) {
      // LRU：命中就挪到队尾（Map 保持插入序）
      cache.delete(opts.key)
      cache.set(opts.key, hit)
      return hit
    }
  }
  const processor = unified()
    .use(remarkParse)
    .use(opts.remarkPlugins as never)
    // allowDangerousHtml 是 react-markdown 的默认（raw 节点留给 post 变换决定去留）
    .use(remarkRehype, { allowDangerousHtml: true } as never)
    .use(opts.rehypePlugins as never)
  const tree = postTransform(processor.runSync(processor.parse(text)) as unknown as Root)
  if (opts.key) {
    stripPositions(tree)
    cache.set(opts.key, tree)
    cachedChars += text.length
    // 驱逐：按插入序（最旧的最先走）
    for (const [k, v] of cache) {
      if (cachedChars <= MAX_CHARS && cache.size <= MAX_ENTRIES) break
      cache.delete(k)
      cachedChars -= keyChars(k) ?? 0
      void v
    }
  }
  return tree
}

/* 驱逐时要从预算里扣掉那一条的字符数 —— 键里编了长度，省一份并行记录 */
const LEN_TAG = '|len:'
function keyChars(k: string): number | null {
  const at = k.lastIndexOf(LEN_TAG)
  if (at < 0) return null
  const n = Number(k.slice(at + LEN_TAG.length))
  return Number.isFinite(n) ? n : null
}
export function makeKey(parts: (string | number | boolean | undefined)[], text: string): string {
  return parts.map((p) => String(p ?? '')).join('|') + LEN_TAG + text.length + '|' + hash32(text)
}
function hash32(s: string): string {
  let h = 0x811c9dc5
  for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 0x01000193) }
  return (h >>> 0).toString(16)
}

/** hast → React 元素（每次渲染都要跑，但只做元素构造，不做解析）。 */
export function hastToElements(tree: Root, components: Components | undefined): ReactNode {
  return toJsxRuntime(tree as never, {
    Fragment, jsx, jsxs,
    components,
    ignoreInvalidStyle: true,
    passKeys: true,
    passNode: true,
  } as never) as ReactNode
}

/** 测试/探针用：清空缓存并读当前占用 */
export function mdCacheStats(): { entries: number; chars: number } {
  return { entries: cache.size, chars: cachedChars }
}
export function clearMdCache(): void {
  cache.clear()
  cachedChars = 0
}
