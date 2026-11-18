// mdPipeline-test.ts —— markdown 解析缓存的单测（node:test，无 DOM）—— P0-1（2026-11-17）。
//
// ## 为什么钉这些
//
// markdown 缓存是「切会话/刷新回来不重新解析」的全部依据（mdPipeline.ts 文件头：
// 一次渲染 25.8ms 里解析占 24ms）。而这个缓存**失效时没有任何报错** —— 只是
// 「md 缓存: 0 条」，然后每次渲染都全量重解析。
//
// 2026-11-17 实测（渲染进程 PID 42010，dev 模式 27 小时）：
//   [budget][mem] md 缓存: 0 条 / 1859130 字符
// 0 条 + 186 万字符 = 缓存**永久失效**（不是"没命中"，是"根本存不住"）。
//
// 机制（修复前）：
//   · makeKey 产出 `parts|len:5000|616035e5`
//   · keyChars 用 Number("5000|616035e5") 解析 → NaN → 返回 null
//   · 驱逐循环 `cachedChars -= keyChars(k) ?? 0` → 减 0 → 字符数**永不回落**
//   · 循环的 break 条件 `cachedChars <= MAX_CHARS` 再也不成立 → **每次插入都清空整个缓存**
//
// 所以断言要钉两件事：① 超预算后缓存**仍然存得住东西**；② cachedChars **会回落**。
import test from 'node:test'
import assert from 'node:assert/strict'
import { clearMdCache, makeKey, mdCacheStats, parseMarkdown } from './mdPipeline'

/** 与 mdPipeline.ts 里的常量一致（那边没导出，这里写死并在断言里引用）。 */
const MAX_CHARS = 120_000
const MAX_ENTRIES = 120

/** 造一段真实形状的 markdown（表格 + 代码块 + 列表），让解析走完整管线。
 *  `pad` 控制体积 —— 驱逐测试要**真的撑爆** MAX_CHARS(12 万字)，否则测不到 bug。 */
function md(i: number, pad = 0): string {
  const body = [
    `## 第 ${i} 段`,
    '',
    '| 列 A | 列 B |',
    '|---|---|',
    `| ${i} | ${i * 2} |`,
    '',
    '```js',
    `const v${i} = ${i}`,
    '```',
    '',
    `- 项 ${i}A`,
    `- 项 ${i}B`,
  ].join('\n')
  return pad > 0 ? body + '\n\n' + ('填充段落。'.repeat(Math.ceil(pad / 5))) : body
}

const parse = (text: string) =>
  parseMarkdown(text, { key: makeKey(['lang', 'dark'], text), remarkPlugins: [], rehypePlugins: [] })

test('mdPipeline：缓存键能解出源文本长度（keyChars 不得返回 null）', () => {
  clearMdCache()
  const text = 'x'.repeat(5000)
  const key = makeKey(['lang', 'dark'], text)
  // keyChars 是模块私有的，但驱逐要用它扣字符数 ⇒ 用「驱逐后 chars 是否回落」间接验证。
  // 这里先钉住键的形状：长度必须紧跟 |len: 且以 | 分隔（否则 Number() 解析不出）。
  const at = key.lastIndexOf('|len:')
  assert.ok(at >= 0, '键里必须有 |len: 段')
  const rest = key.slice(at + '|len:'.length)
  assert.match(rest, /^\d+\|/, '长度段必须以 | 结尾（当前是 len:N|hash，Number() 会得到 NaN）')
  assert.equal(Number(rest.split('|')[0]), 5000)
})

test('mdPipeline：超过字符预算后缓存仍能存住东西（修复前会塌成 0 条）', () => {
  clearMdCache()
  // 30 条 × 6k 字 = 18 万字 > MAX_CHARS(12 万) ⇒ 修复前这一步之后 entries 变成 0。
  for (let i = 0; i < 30; i++) parse(md(i, 6000))
  const s = mdCacheStats()
  assert.ok(s.entries > 0, `缓存塌了：entries=${s.entries} chars=${s.chars}（修复前必然是 0）`)
  assert.ok(s.entries <= MAX_ENTRIES, `条目数应受上限约束：${s.entries}`)
  assert.ok(s.chars <= MAX_CHARS, `cachedChars 应回落到预算内：${s.chars} > ${MAX_CHARS}`)
})

test('mdPipeline：重复渲染同一份正文命中缓存（不重新解析）', () => {
  clearMdCache()
  const text = md(1)
  const first = parse(text)
  const second = parse(text)
  assert.equal(first, second, '同一份正文应返回同一棵 hast 树（说明命中缓存而非重解析）')
  const s = mdCacheStats()
  assert.equal(s.entries, 1)
  assert.equal(s.chars, text.length)
})

test('mdPipeline：驱逐真的会把字符数减下去', () => {
  clearMdCache()
  // 一条超长正文（160k > MAX_CHARS 120k）⇒ 插入后必须被驱逐 ⇒ chars 归零。
  const big = 'y'.repeat(160_000)
  parse(big)
  const s = mdCacheStats()
  assert.ok(s.chars <= MAX_CHARS, `超预算的一条应被驱逐：chars=${s.chars}`)
  assert.equal(s.entries, 0, '超预算的单条应被驱逐干净')
})
