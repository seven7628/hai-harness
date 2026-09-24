import type { Highlighter, ShikiTransformer } from 'shiki'
import type { ElementContent } from 'hast'
import type { ResolvedTheme } from '../lib/useTheme'
import { codeThemeOf } from '../lib/appearance'

// shiki 按需加载单例：首次出现代码块时才动态 import 并初始化语法集，
// 避免初始 bundle 拖入全部语言（shiki 本体 + 语法解析器较大，走独立 chunk）。
// 预载两套默认主题（亮 one-light / 暗 github-dark）让首屏不为高亮等第二次往返；
// 其余主题在**外观设置里被选中时**用 loadTheme() 现载（见 codeToHtml 的 ensureTheme）。
const DEFAULT_THEMES = ['one-light', 'github-dark'] as const
const LANGS = [
  'go',
  'typescript',
  'tsx',
  'javascript',
  'jsx',
  'json',
  'yaml',
  'sql',
  'python',
  'rust',
  'html',
  'css',
  'vue',
  'vue-html',
  'markdown',
  'diff',
  'shellscript', // shiki 归一化 bash → shellscript
]

let p: Promise<Highlighter> | null = null

function get(): Promise<Highlighter> {
  if (!p) p = import('shiki').then((m) => m.createHighlighter({ langs: LANGS, themes: [...DEFAULT_THEMES] }))
  return p
}

// 主题名可能来自 localStorage（形状校验已做，但名字不一定被当前 shiki 版本认识）：
// 未知名字退回该主题的默认，避免 codeToHtml 直接抛错把代码块打回无高亮。
async function ensureTheme(h: Highlighter, name: string, fallback: string): Promise<string> {
  const loaded = h.getLoadedThemes() as string[]
  if (loaded.includes(name)) return name
  try {
    await h.loadTheme(name as never)
    return name
  } catch {
    if (loaded.includes(fallback)) return fallback
    await h.loadTheme(fallback as never).catch(() => { /* 默认主题也载不动：交给 shiki 抛原始错误 */ })
    return fallback
  }
}

// codeToHtml 高亮单段代码；未知语言回退 text（无高亮，纯样式）。
// resolved 主题由调用方经 useResolvedTheme() 传入（代码块需随主题切换重渲）。
// lineNumbers 为 true 时每行加行号 gutter（对话代码块与右侧文件栏共用同一实现，
// 与 `file_path:line_number` 引用契约对齐——行号即文件中真实行号）。
// themeOverride 供外观设置里的主题预览卡指定具体主题（不吃全局偏好）。
export async function codeToHtml(
  code: string,
  lang: string,
  resolved: ResolvedTheme = 'ink',
  lineNumbers = false,
  themeOverride?: string,
): Promise<string> {
  const h = await get()
  const safe = h.getLoadedLanguages().includes(lang) ? lang : 'text'
  const fallback = resolved === 'paper' ? 'one-light' : 'github-dark'
  const theme = await ensureTheme(h, themeOverride ?? codeThemeOf(resolved), fallback)
  return h.codeToHtml(code, {
    lang: safe,
    theme,
    transformers: lineNumbers ? [lineNumbersTransformer] : [],
  })
}

// 行号 transformer：给每个 <span class="line"> 前插一个行号 gutter span。
// 用 CSS counter 渲染数字（每个 .shiki 容器独立计数，多文件互不影响），
// 数字右侧 1ch 空格与代码首列对齐；复制选区时 gutter 通过
// user-select:none 自动排除（浏览器复制仅含可选文本）。行号列不占
// code 内容宽度 → 无需负 margin，横向滚动条按内容宽度计算。
// data-line：真实行号（1-based），供「最终文件」按 read_file start_line 定位滚动。
const lineNumbersTransformer = {
  name: 'go-code:line-numbers',
  line(hast, line) {
    const gutter = {
      type: 'element' as const,
      tagName: 'span',
      properties: { className: ['fp-ln'], 'data-line': line },
      children: [] as ElementContent[],
    }
    if (hast.children.length === 0) {
      // 空行：占位符保等高（pre 内行高由字体决定，空 span 无内容会塌陷）
      hast.children.push({ type: 'text', value: '\u00A0' })
    }
    hast.children.unshift(gutter)
  },
} satisfies ShikiTransformer
