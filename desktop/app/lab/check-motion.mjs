// 动效表守卫：运行时皮肤的动效时长只能来自 src/runtime-tokens.css 的那张表。
// 组件里再出现裸的 `180ms` / `2.1s` 就是「两处真相」，改一处不生效 —— 这里让它红。
// 唯一例外：prefers-reduced-motion 的 `1ms` 急停开关（那是「关掉动效」的写法，不是时长）。
// 用法：node lab/check-motion.mjs
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, extname, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = fileURLToPath(new URL('../src/', import.meta.url))
const SKIP = new Set(['runtime-tokens.css', 'runtime-palette.css'])
const ALLOW = new Set(['1ms'])

const files = []
const walk = (dir) => {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) { walk(p); continue }
    if (extname(p) !== '.css' || SKIP.has(name)) continue
    const rel = relative(ROOT, p)
    // 只看运行时这一档的文件（另两档皮肤仍是旧样式，不在收口范围）
    if (!/runtime/.test(rel)) continue
    files.push(p)
  }
}
walk(ROOT)

const bad = []
for (const f of files) {
  const lines = readFileSync(f, 'utf8').split('\n')
  let inComment = false
  lines.forEach((line, i) => {
    const s = line.trim()
    // 块注释要按状态跟踪：续行不以 * 开头（中文说明常见），只查 `/*` 会漏
    const opens = line.includes('/*') && !line.includes('*/')
    const wasComment = inComment
    if (opens) inComment = true
    else if (inComment && line.includes('*/')) inComment = false
    if (wasComment || opens || line.includes('*/') || s.startsWith('/*') || s.startsWith('//')) return
    for (const h of line.match(/(?<![\w.-])\d+(?:\.\d+)?m?s\b/g) ?? []) {
      if (!ALLOW.has(h)) bad.push(`${relative(ROOT, f)}:${i + 1}  ${h}  ${s.slice(0, 70)}`)
    }
  })
}

if (bad.length) {
  console.log(`FAIL  ${bad.length} 处裸时长（应改用 runtime-tokens.css 里的 --t-* 名字）：`)
  console.log(bad.slice(0, 20).join('\n'))
  process.exit(1)
}
console.log(`PASS  动效时长全部来自 --t-* 表（${files.length} 个文件）`)
