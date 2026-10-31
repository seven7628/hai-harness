// ask_user 选项解析回归测试（纯函数 + esbuild 打包，Node 直接跑，无需浏览器）。
//
// 背景（2026-09）：提问面板的选项从「纯字符串数组」升级为「可带预览的对象」
// （模型给选项配 preview：ASCII 版面 / 代码或 diff 片段 / 配置或 schema 变体 —— 见
// tools/question 的工具描述与 events/ask.go 的 QuestionOption）。前端 lib/questionOptions
// 是**唯一**的归一化入口，本脚本锁住它的关键行为：
//   T1  对象形态 {"label","preview"} 原样保留（预览透传到面板的唯一数据源）
//   T2  纯字符串 "A" → {label:"A"}（历史会话落盘事件 / 前端 mock / 模型简写都要能回放）
//   T3  两种写法在同一题里混用（真实回放：老事件 + 新事件同屏）
//   T4  脏数据（null / 数字 / 嵌套对象 / 非数组）→ 丢弃且不抛错
//      —— 面板是「模型阻塞等回答」的界面，一条脏数据把渲染搞崩 = 用户无法作答 = agent 卡死
//   T5  空 label / 纯空白 label 丢弃；空 preview 不落 preview 键（对象形状稳定）
//   T6  hasPreview 只看非空 preview（决定是否走并排预览版面）
//
// 用法：npm run test:question-options
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createRequire } from 'node:module'

const __dirname = dirname(fileURLToPath(import.meta.url))
const require = createRequire(import.meta.url)

// esbuild 打包纯函数模块（无 React / 无 store 依赖 → 直接 require 真实源码）
const dir = mkdtempSync(join(tmpdir(), 'ask-options-test-'))
const outfile = join(dir, 'questionOptions.cjs')
await build({
  entryPoints: [join(__dirname, '../src/lib/questionOptions.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' }, // mock.ts 的 ?raw/?url 资源
})
const { parseQuestionOptions, hasPreview } = require(outfile)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assertEq = (actual, expected, m) => {
  assert.deepStrictEqual(actual, expected, `${m}\n  实际: ${JSON.stringify(actual)}\n  期望: ${JSON.stringify(expected)}`)
  ok(m)
}

console.log('\n[T1] 对象形态（label + preview）')
{
  const opts = parseQuestionOptions([
    { label: '停靠面板', preview: '+--+\n|  |\n+--+' },
    { label: '全屏弹窗', preview: '+==+\n|  |\n+==+' },
  ])
  assertEq(opts, [
    { label: '停靠面板', preview: '+--+\n|  |\n+--+' },
    { label: '全屏弹窗', preview: '+==+\n|  |\n+==+' },
  ], 'label 与 preview 原样保留（markdown 换行不被改写）')
  assert(hasPreview(opts), '带 preview → 走并排预览版面')

  // 对象但没 preview（模型只给 label）→ 与字符串写法结果一致
  assertEq(parseQuestionOptions([{ label: 'A' }, { label: 'B' }]), [{ label: 'A' }, { label: 'B' }], '对象无 preview 时等同纯 label')
}

console.log('\n[T2] 纯字符串形态（历史事件 / mock / 模型简写）')
{
  assertEq(parseQuestionOptions(['A', 'B']), [{ label: 'A' }, { label: 'B' }], '字符串 → {label}（preview 键不出现）')
  assertEq(parseQuestionOptions(['  留白  ']), [{ label: '留白' }], 'label 两端留白裁掉（按钮文字与回答文本都要干净）')
  assert(!hasPreview(parseQuestionOptions(['A', 'B'])), '全是字符串 → 不走预览版面（沿用原横排按钮）')
}

console.log('\n[T3] 两种写法混用（老事件 + 新事件同屏回放）')
{
  const opts = parseQuestionOptions(['仅 auth 包', { label: '连同 500 兜底一起收敛', preview: 'diff' }])
  assertEq(opts, [{ label: '仅 auth 包' }, { label: '连同 500 兜底一起收敛', preview: 'diff' }], '同一题内混用两种写法都能解析')
  assert(hasPreview(opts), '只要有一个带 preview → 整题走预览版面')
}

console.log('\n[T4] 脏数据不崩（提问面板是「模型阻塞等回答」的界面，崩了 = agent 卡死）')
{
  assertEq(parseQuestionOptions(null), [], 'null → []')
  assertEq(parseQuestionOptions(undefined), [], 'undefined → []')
  assertEq(parseQuestionOptions('A'), [], '非数组（字符串）→ []（不把字符串当单元素数组）')
  assertEq(parseQuestionOptions([null, 42, true, [], ['A'], { label: null }, { label: 42 }, { preview: 'x' }, { label: '' }]), [], '各种脏元素全部丢弃')
  assertEq(
    parseQuestionOptions([{ label: 'A', preview: 42 }, { label: 'B', preview: { nested: 'obj' } }]),
    [{ label: 'A' }, { label: 'B' }],
    'preview 非字符串 → 当没写预览（不把对象塞进 Markdown 渲染器）',
  )
  assertEq(
    parseQuestionOptions([{ label: 'ok', preview: null }, { label: 'keep', preview: 'x' }]),
    [{ label: 'ok' }, { label: 'keep', preview: 'x' }],
    '脏元素不影响同批的干净元素（逐元素兜底，不整体放弃）',
  )
}

console.log('\n[T5] 空 label / 空 preview 归一化')
{
  assertEq(parseQuestionOptions(['', '  ', '\n', 'A']), [{ label: 'A' }], '空/纯空白 label 丢弃（空按钮点不下去，也没有回答文本）')
  assertEq(parseQuestionOptions([{ label: 'A', preview: '' }]), [{ label: 'A' }], '空 preview 不落 preview 键（对象形状稳定，便于断言）')
  assertEq(parseQuestionOptions([]), [], '空数组 → []（面板据此显示纯输入框：无快捷选项）')
}

console.log('\n[T6] hasPreview')
{
  assert(!hasPreview([]), '无选项 → false')
  assert(!hasPreview([{ label: 'A' }, { label: 'B' }]), '都没预览 → false')
  assert(hasPreview([{ label: 'A' }, { label: 'B', preview: 'x' }]), '有一个带非空预览 → true')
}

console.log(`\n✅ ask_user 选项解析：${pass} 项全部通过`)
