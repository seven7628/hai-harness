// 分类色板的取色规则：**"这是什么"由这张表回答**，颜色只在 CSS 里落。
//
// 为什么要有这个文件：颜色是分类的**表现**，判据必须是分类的**事实**——
// 工具族与图标（Narrative.tsx 的 toolIcon）同源、文件类型与产出卡 TypeGlyph 同源，
// 三处各写一套正则必然漂移（"图标说一套、颜色说一套"是最容易犯的错）。
//
// 用法：组件侧只写 data-* 属性（data-family / data-ft），配色全部在 index.css 里：
//   .tool-row[data-family='read'] .t { color: var(--cat-read) }
//   .tool-row .args.file[data-ft='code'] { color: var(--ft-code) }
// 这样换皮肤时**不用碰任何 .tsx**。

/** 工具族：看=read / 跑=cmd / 写=write / 托=agent / 网络=net / 技能=skill */
export type ToolFamily = 'read' | 'cmd' | 'write' | 'net' | 'agent' | 'skill'

// 判据顺序有讲究：先"托/外部"这类判词（task/web 前缀），再落到读/写/默认执行 ——
// 顺序换了会把 web_fetch（外部）判成 read，把 task（子 agent）判成默认的 cmd。
export function toolFamily(name: string): ToolFamily {
  const n = (name || '').toLowerCase()
  if (n === 'skill') return 'skill' // SKILL 块（/skill 加载）在对话里就是工具行的形态
  if (/^(task|agent|spawn|subagent|async|background|promote)/.test(n)) return 'agent'
  if (/^(web|fetch|curl|http|browser|ego|open_url|search_web)/.test(n)) return 'net'
  if (/^(edit|write|create|patch|apply|multi_edit)/.test(n)) return 'write'
  if (/^(read|view|list|glob|grep|search|find|ls)/.test(n)) return 'read'
  return 'cmd' // bash / 终端 / 沙箱 / 其它自定义工具：默认按"在跑东西"
}

/** 文件类型：与产出卡 TypeGlyph（ArtifactCard）的分类口径一致 */
export type FileType = 'code' | 'doc' | 'sheet' | 'slide' | 'pdf' | 'image' | 'data'

const EXT_FT: Record<string, FileType> = {
  // 代码：语言与样式/标记
  ts: 'code', tsx: 'code', js: 'code', jsx: 'code', mjs: 'code', cjs: 'code',
  go: 'code', py: 'code', rs: 'code', java: 'code', kt: 'code', swift: 'code',
  c: 'code', h: 'code', cc: 'code', cpp: 'code', hpp: 'code', cs: 'code',
  rb: 'code', php: 'code', scala: 'code', lua: 'code', dart: 'code',
  css: 'code', scss: 'code', less: 'code', html: 'code', htm: 'code', vue: 'code', svelte: 'code',
  sh: 'code', bash: 'code', zsh: 'code', ps1: 'code', bat: 'code', sql: 'code',
  // 文档
  md: 'doc', markdown: 'doc', mdx: 'doc', txt: 'doc', rst: 'doc', adoc: 'doc',
  doc: 'doc', docx: 'doc', docm: 'doc', rtf: 'doc', pages: 'doc',
  // 表格
  xlsx: 'sheet', xlsm: 'sheet', xls: 'sheet', csv: 'sheet', tsv: 'sheet', numbers: 'sheet',
  // 演示
  pptx: 'slide', pptm: 'slide', ppt: 'slide', key: 'slide',
  // PDF
  pdf: 'pdf',
  // 图片
  png: 'image', jpg: 'image', jpeg: 'image', gif: 'image', webp: 'image', bmp: 'image',
  ico: 'image', tif: 'image', tiff: 'image', svg: 'image', avif: 'image', heic: 'image',
  // 数据 / 配置 / 日志
  json: 'data', jsonc: 'data', yaml: 'data', yml: 'data', toml: 'data', ini: 'data',
  xml: 'data', plist: 'data', env: 'data', log: 'data', ndjson: 'data', sqlite: 'data', db: 'data',
}

/** 扩展名（可带点、可大小写）→ 文件类型；未知回退 data（"数据"是最中性的兜底） */
export function fileTypeOf(extOrPath: string): FileType {
  const s = (extOrPath || '').trim()
  if (!s) return 'data'
  // 传整路径也认：取最后一段的扩展名（`.env` 这类无 basename 的点文件按原名查表）
  const base = s.replaceAll('\\', '/').split('/').pop() ?? s
  const ext = base.includes('.') ? (base.split('.').pop() ?? '') : base
  return EXT_FT[ext.toLowerCase()] ?? 'data'
}
