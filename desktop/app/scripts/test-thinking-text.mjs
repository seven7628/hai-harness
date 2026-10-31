// 思考正文段落切分回归（esbuild 打包 src/lib/thinkingText.ts，Node 直接跑）。
//
// 起因（2026-09-19 用户报障）：V3 皮肤下思考过程的段落间隔太大 —— 思考正文是 pre-wrap 纯文本，
// 段落之间的空行渲染成一整个行盒（≈ 2 倍行高），比正文段落的 4px 松一个数量级。
// 修法 = 按空行切成 <p class="tk-p">，间距交给 CSS 按皮肤控制。
//
// 本脚本锁住切分口径（错一条就会静默改坏渲染）：
//   T1 空行分段：单/多空行等价，不产生空段，末尾空行不占位
//   T2 段内单换行保留（列表式短行仍按原样折行）
//   T3 围栏代码块内的空行不分段、``` 配对不被拆开（inlineCode 按反引号奇偶配对）
//   T4 边界：纯空白文本、无空行单段、CRLF 文本
//
// 用法：npm run test:thinking-text
import { build } from 'esbuild'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)
const dir = mkdtempSync(join(tmpdir(), 'thinking-text-'))
const outfile = join(dir, 'thinking-text.cjs')
await build({
  entryPoints: [join(__dirname, '../src/lib/thinkingText.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile, logLevel: 'silent',
})
const { splitThinkingParagraphs: split } = require(outfile)

let pass = 0
let fail = 0
const ok = (cond, label) => {
  if (cond) { pass++; console.log(`  ✓ ${label}`) } else { fail++; console.log(`  ✗ ${label}`) }
}
const eq = (got, want, label) => ok(JSON.stringify(got) === JSON.stringify(want), `${label}${JSON.stringify(got) === JSON.stringify(want) ? '' : `（得到 ${JSON.stringify(got)}）`}`)
const section = (name) => console.log(`\n${name}`)

section('T1 空行分段')
eq(split('段一\n\n段二'), ['段一', '段二'], '单空行分成两段')
eq(split('段一\n\n\n\n段二'), ['段一', '段二'], '多空行不产生空段')
eq(split('段一\n\n段二\n\n'), ['段一', '段二'], '末尾空行不占位')
eq(split('\n\n段一\n\n'), ['段一'], '首尾空行不占位')
eq(split('a\n b\n\nc'), ['a\n b', 'c'], '段首缩进保留')

section('T2 段内单换行')
eq(split('1. foo\n2. bar\n3. baz'), ['1. foo\n2. bar\n3. baz'], '列表式单换行 = 一段（原样折行）')
eq(split('单段无空行'), ['单段无空行'], '无空行 = 单段')

section('T3 围栏代码块')
{
  const text = '先看这段代码：\n\n```go\nfunc main() {\n\n\tprintln("hi")\n}\n```\n\n然后结论。'
  const got = split(text)
  eq(got.length, 3, '代码块内的空行不分段（3 段）')
  ok(got[1].startsWith('```go') && got[1].endsWith('```'), '围栏开合留在同一段（配对不被打断）')
  ok(got[1].includes('println'), '代码内容完整')
  const fences = got[1].split('\n').filter((l) => /^\s*```/.test(l)).length
  ok(fences === 2, `同段内围栏数为偶数（实际 ${fences}）`)
  ok(got.every((p) => (p.split('`').length - 1) % 2 === 0), '各段反引号数为偶数（inlineCode 奇偶配对正确）')
}

section('T4 边界')
eq(split(''), [], '空串 → 无段')
eq(split('   \n\n  \n'), [], '纯空白 → 无段')
eq(split('a\r\n\r\nb'), ['a', 'b'], 'CRLF 空行同样分段（尾随 \\r 剥掉）')
eq(split('只有一段\r\n带 CRLF 换行'), ['只有一段\n带 CRLF 换行'], 'CRLF 段内换行不误分段且归一为 \\n')

console.log(`\n结果：${pass} 通过 / ${fail} 失败`)
process.exit(fail === 0 ? 0 : 1)
