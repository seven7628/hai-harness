// 第 2 步的前置门槛：**自己接管解析管线**必须与 react-markdown 的输出逐字节相同。
// 验证不过就不做（用户已同意这个顺序）。
//
// 比较对象（同一个语料，同样没有 components 覆盖 —— 覆盖只影响元素属性，不影响树）：
//   A react-markdown 的 <Markdown>{text}</Markdown>（renderToStaticMarkup）
//   B unified(remarkParse → remarkPlugins → remarkRehype → rehypePlugins) → runSync
//     → visit(raw→text / urlTransform) → hast-util-to-jsx-runtime → renderToStaticMarkup
//
// 用法：npx esbuild lab/md-pipeline-equal.tsx --bundle --platform=node --format=cjs \
//         --outfile=/tmp/md-pipe.cjs --loader:.css=empty && node /tmp/md-pipe.cjs
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import ReactMarkdown, { defaultUrlTransform } from 'react-markdown'
import { unified } from 'unified'
import remarkParse from 'remark-parse'
import remarkRehype from 'remark-rehype'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import remarkFrontmatter from 'remark-frontmatter'
import remarkGemoji from 'remark-gemoji'
import rehypeKatex from 'rehype-katex'
import { toJsxRuntime } from 'hast-util-to-jsx-runtime'
import { jsx, jsxs, Fragment } from 'react/jsx-runtime'
import { visit } from 'unist-util-visit'
import type { Root } from 'hast'

const remarkPlugins = [remarkGfm, remarkFrontmatter, remarkMath, remarkGemoji]
const rehypePlugins = [rehypeKatex]

const URL_ATTRS: Record<string, string[] | null> = {
  href: null, src: null, cite: null, poster: null, srcSet: null,
}

// 复刻 react-markdown 的 post()：raw → text，url 属性过一遍 urlTransform
function post(tree: Root): Root {
  visit(tree, (node: any, index: number | undefined, parent: any) => {
    if (node.type === 'raw' && parent && typeof index === 'number') {
      parent.children[index] = { type: 'text', value: node.value }
      return index
    }
    if (node.type === 'element') {
      for (const key in URL_ATTRS) {
        if (Object.hasOwn(node.properties ?? {}, key)) {
          node.properties[key] = defaultUrlTransform(String(node.properties[key] ?? ''))
        }
      }
    }
  })
  return tree
}

function renderPipeline(text: string): string {
  const processor = unified().use(remarkParse).use(remarkPlugins as never).use(remarkRehype, { allowDangerousHtml: true } as never).use(rehypePlugins as never)
  const tree = post(processor.runSync(processor.parse(text)) as unknown as Root)
  const el = toJsxRuntime(tree as never, {
    Fragment, jsx, jsxs, ignoreInvalidStyle: true, passKeys: true, passNode: true,
  } as never)
  return renderToStaticMarkup(el as never)
}
function renderReactMarkdown(text: string): string {
  return renderToStaticMarkup(
    createElement(ReactMarkdown as never, { remarkPlugins, rehypePlugins } as never, text),
  )
}

const corpus: Record<string, string> = {
  段落与强调: '普通段落，**粗体**、*斜体*、~~删除线~~、`inline code`。\n\n第二段。\n',
  列表与任务: '- 一\n- 二\n  - 二点一\n\n1. 有序\n2. 第二\n\n- [x] 做完\n- [ ] 没做\n',
  表格: '| 列一 | 列二 |\n|---|--:|\n| a | 1 |\n| b | 22 |\n',
  代码围栏: '```js\nconst a = 1\nconsole.log(a)\n```\n\n```\n纯文本\n```\n',
  标题: '# 一级\n\n## 二级\n\n### 三级 **粗**\n',
  链接与图片: '[外链](https://example.com/a?b=1&c=2) [锚点](#x) ![图](/tmp/a.png "t")\n',
  数学: '行内 $a^2+b^2=c^2$\n\n$$\n\\int_0^1 x\\,dx = \\frac12\n$$\n',
  引用与分隔: '> 引用第一行\n> 第二行\n\n---\n\n正文\n',
  HTML块: '<div class="x">原始 HTML</div>\n\n<span>行内 <b>html</b></span>\n',
  emoji与frontmatter: '---\ntitle: 测试\n---\n\n:rocket: 发射\n',
  脚注: '带脚注[^1]。\n\n[^1]: 脚注内容\n',
  alert: '> [!NOTE]\n> 提示内容\n',
  混合长文: Array.from({ length: 40 }, (_, i) => `第 ${i} 段 **粗** \`c\`\n\n- 项 ${i}\n\n| a | b |\n|---|---|\n| ${i} | ${i * 2} |\n`).join('\n'),
}

let fail = 0
for (const [name, text] of Object.entries(corpus)) {
  const a = renderReactMarkdown(text)
  const b = renderPipeline(text)
  if (a === b) {
    console.log(`OK   ${name}（${a.length}B）`)
  } else {
    fail++
    let i = 0
    while (i < Math.min(a.length, b.length) && a[i] === b[i]) i++
    console.log(`!!   ${name} 不一致（${a.length}B vs ${b.length}B，首个差异 @${i}）`)
    console.log(`     react-markdown: …${JSON.stringify(a.slice(Math.max(0, i - 50), i + 50))}`)
    console.log(`     自建管线      : …${JSON.stringify(b.slice(Math.max(0, i - 50), i + 50))}`)
  }
}
console.log(fail ? `\nFAIL ${fail}` : '\nPASS 自建管线与 react-markdown 逐字节相同')
